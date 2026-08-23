// Package app is the GitHub App side of the structure engine: the
// manifest creation flow, the App JWT, the REST client the engine and
// preflight probe live state with, and the drift checks.
//
// The manifest flow is the achievable maximum of App automation. GitHub
// has no API to create an App: the flow trades that for exactly ONE
// browser click per App. Everything on either side of the click is
// automated here — the manifest renders from the registry row, the
// one-click redirect is served locally, the returned code is exchanged
// for credentials which are handed BACK to the caller (where they are
// stored is the estate's decision, not this package's). No human ever
// copies a private key, which is the point: hand-copying a PEM is how
// credentials end up in shell history and chat logs.
//
// After creation the App still has to be INSTALLED on the org — also a
// click, also unavoidable on the Team plan (the install API is
// Enterprise-only). The helper waits for it and records the resulting
// installation ID, so the operator's whole job is "click twice".
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	registry "github.com/truvity/github-structure/pkg/registry"
)

const (
	// githubBase is the browser-facing host (the manifest form posts here).
	githubBase = "https://github.com"
	// apiBase is the REST host (the code→credentials exchange).
	apiBase = "https://api.github.com"

	apiVersionHeader = "X-GitHub-Api-Version"
	apiVersion       = "2022-11-28"

	// permMetadata is granted implicitly by GitHub. The registry keeps it
	// so drift comparison stays a literal map compare; the manifest drops
	// it because GitHub rejects it as an explicit request.
	permMetadata = "metadata"
)

type (
	// Manifest is GitHub's App-manifest document. Field names are fixed
	// by GitHub; see
	// https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest
	Manifest struct {
		Name        string            `json:"name"`
		URL         string            `json:"url"`
		Description string            `json:"description,omitempty"`
		RedirectURL string            `json:"redirect_url"`
		HookAttrs   *HookAttributes   `json:"hook_attributes,omitempty"`
		Public      bool              `json:"public"`
		DefaultPerm map[string]string `json:"default_permissions"`
		DefaultEvts []string          `json:"default_events,omitempty"`
	}

	// HookAttributes configures the App's webhook.
	//
	// Omitted entirely for API-only Apps. GitHub REQUIRES `url` inside
	// this object whenever the object is present, and rejects the whole
	// manifest with "url wasn't supplied" if it is missing — an error
	// that reads as though the App's TOP-LEVEL url were the problem.
	HookAttributes struct {
		URL    string `json:"url"`
		Active bool   `json:"active"`
	}

	// Credentials is what POST /app-manifests/{code}/conversions returns.
	// This is the only moment GitHub ever discloses the private key.
	Credentials struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		Name          string `json:"name"`
		HTMLURL       string `json:"html_url"`
		PEM           string `json:"pem"`
		WebhookSecret string `json:"webhook_secret"`
		ClientID      string `json:"client_id"`
		ClientSecret  string `json:"client_secret"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
)

// BuildManifest renders the manifest for one registry row. The row is the
// single source of truth for the App's permissions — which is what makes
// "the registry describes everything" true rather than aspirational.
func BuildManifest(name string, app *registry.App, redirectURL string) (*Manifest, error) {
	if app.External {
		return nil, fmt.Errorf("app %q is external (vendor-owned) — we do not create it", name)
	}

	if app.Adopted {
		return nil, fmt.Errorf("app %q already exists (adopted row, app_id %d) — creating it again would mint a second App with a colliding name", name, app.AppID)
	}

	if len(app.Permissions) == 0 {
		return nil, fmt.Errorf("app %q: no permissions in the registry row", name)
	}

	perms := make(map[string]string, len(app.Permissions))
	for k, v := range app.Permissions {
		if k == permMetadata {
			continue
		}

		perms[k] = v
	}

	manifest := &Manifest{
		Name:        name,
		URL:         app.URL,
		Description: collapse(app.Description),
		RedirectURL: redirectURL,
		Public:      false,
		DefaultPerm: perms,
		DefaultEvts: app.Events,
	}

	// Only Apps that actually receive webhooks carry the block.
	if app.WebhookURL != "" {
		manifest.HookAttrs = &HookAttributes{URL: app.WebhookURL, Active: true}
	}

	return manifest, nil
}

// CreateURL is where the manifest form posts. The App is created in the
// ORG's namespace, not a personal one — a personal App would be a
// single-owner dependency.
func CreateURL(org, state string) string {
	return fmt.Sprintf("%s/organizations/%s/settings/apps/new?state=%s", githubBase, org, state)
}

// InstallURL is the org-install consent page for a created App.
func InstallURL(slug string) string {
	return fmt.Sprintf("%s/apps/%s/installations/new", githubBase, slug)
}

// Convert exchanges the temporary code GitHub hands back for the App's
// permanent credentials. The code is single-use and expires in an hour;
// the exchange needs no authentication, which is why the state parameter
// checked by the caller matters.
func Convert(ctx context.Context, code string) (*Credentials, error) {
	url := fmt.Sprintf("%s/app-manifests/%s/conversions", apiBase, code)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set(apiVersionHeader, apiVersion)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchanging manifest code: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("manifest conversion failed: %s: %s", resp.Status, bytes.TrimSpace(body))
	}

	var creds Credentials
	if err := json.Unmarshal(body, &creds); err != nil {
		return nil, fmt.Errorf("decoding credentials: %w", err)
	}

	if creds.PEM == "" {
		return nil, fmt.Errorf("manifest conversion returned no private key")
	}

	return &creds, nil
}

// WaitForInstallation polls until the App reports an installation on the
// org, returning its ID. The poll exists because installing is a human
// click we cannot trigger; the deadline keeps a forgotten terminal from
// spinning forever.
func WaitForInstallation(ctx context.Context, appID int64, pem []byte, org string, timeout time.Duration) (int64, error) {
	deadline := time.Now().Add(timeout)

	for {
		id, err := FindInstallation(ctx, appID, pem, org)
		if err == nil {
			return id, nil
		}

		if !isNotInstalled(err) {
			return 0, err
		}

		if time.Now().After(deadline) {
			return 0, fmt.Errorf("timed out waiting for the App to be installed on %s", org)
		}

		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// collapse turns a YAML folded description into a single line — GitHub
// shows it verbatim and trailing newlines look like a bug.
func collapse(s string) string {
	out := make([]rune, 0, len(s))

	space := false

	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			space = true

			continue
		}

		if space && len(out) > 0 {
			out = append(out, ' ')
		}

		space = false

		out = append(out, r)
	}

	return string(out)
}
