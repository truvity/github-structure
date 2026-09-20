// Package app is the GitHub App side of the structure engine: the App
// JWT, the REST client the engine and preflight probe live state with,
// and the drift checks that compare a live organization against its
// registry.
//
// It does NOT create Apps. An App's existence, its key and its
// installation are the credential custodian's business (for truvity,
// access-roster's App catalogue); this package only authenticates AS an
// App whose credentials it is handed, and reports where live GitHub has
// wandered away from what the registry declares.
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// apiBase is the REST host every call here goes to.
	apiBase = "https://api.github.com"

	apiVersionHeader = "X-GitHub-Api-Version"
	apiVersion       = "2022-11-28"
)

// ErrNotFound is returned when GitHub answers 404 — for the callers here
// that is information ("this resource does not exist yet"), not failure.
var ErrNotFound = errors.New("not found")

// ErrNoCommit is returned when a ref exists in name only: an EMPTY
// repository answers "No commit found for SHA" with 422 to anything
// asked about its default branch. Same class of answer as ErrNotFound —
// "there is nothing there yet" — but GitHub spells it differently, and a
// guard that treats it as failure refuses a deploy because a repository
// has no commits.
var ErrNoCommit = errors.New("no commit")

type (
	// Client makes authenticated calls as a GitHub App installation. It
	// exists so the Pulumi program can ask GitHub what already exists
	// before deciding to import or create (check-then-import), using the
	// same credentials it deploys with rather than a second auth path.
	Client struct {
		token string
	}

	// Info is an App's own metadata, read as the App itself.
	Info struct {
		Slug        string            `json:"slug"`
		Name        string            `json:"name"`
		ClientID    string            `json:"client_id"`
		Permissions map[string]string `json:"permissions"`
	}
)

// NewClient mints an installation token for the App.
func NewClient(ctx context.Context, appID int64, privateKey []byte, installationID int64) (*Client, error) {
	token, err := installationToken(ctx, appID, privateKey, installationID)
	if err != nil {
		return nil, fmt.Errorf("mint installation token: %w", err)
	}

	return &Client{token: token}, nil
}

// Get fetches path into dst. A 404 returns ErrNotFound so callers can
// branch on existence without string-matching an error.
func (c *Client) Get(ctx context.Context, path string, dst any) error {
	req, err := newAPIRequest(ctx, http.MethodGet, path, "token "+c.token)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}

	if dst == nil {
		return nil
	}

	return json.Unmarshal(body, dst)
}

// Post sends payload to path and decodes the response into dst (which may
// be nil). Non-2xx responses are errors carrying the response body.
func (c *Client) Post(ctx context.Context, path string, payload, dst any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode payload for %s: %w", path, err)
	}

	req, err := newAPIRequest(ctx, http.MethodPost, path, "token "+c.token)
	if err != nil {
		return err
	}

	req.Body = io.NopCloser(bytes.NewReader(encoded))
	req.ContentLength = int64(len(encoded))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}

	if dst == nil {
		return nil
	}

	return json.Unmarshal(body, dst)
}

// Exists reports whether a resource is there, treating 404 as a plain no.
func (c *Client) Exists(ctx context.Context, path string) (bool, error) {
	err := c.Get(ctx, path, nil)

	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}
