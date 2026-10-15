// Package seccfg reconciles which GitHub code security configuration
// each repository of an organization is attached to.
//
// It is deliberately not a Pulumi resource: the provider has none, and
// under an enforced configuration GitHub refuses per-repository writes,
// so a resource that can be created but never deleted wedges a stack.
// Attachment is a plain REST reconcile run from a pipeline instead:
// Plan reads and returns a ChangeSet, Apply attaches, and nothing here ever detaches.
package seccfg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const (
	// DefaultBaseURL is the REST host calls go to.
	DefaultBaseURL = "https://api.github.com"

	apiVersion = "2022-11-28"
	maxBody    = 16 << 20
	// maxPages bounds a paginated read so a server that always
	// advertises a next page cannot loop forever.
	maxPages = 1000
)

// ErrNoToken is returned by TokenFromEnv when neither variable is set.
var ErrNoToken = errors.New("no GitHub token: set GH_TOKEN or GITHUB_TOKEN")

// Client is a minimal authenticated GitHub REST client. The token is
// never included in an error or a String; callers must not log it.
type Client struct {
	// BaseURL defaults to DefaultBaseURL; tests point it at a fake.
	BaseURL string
	// HTTP defaults to http.DefaultClient.
	HTTP *http.Client

	token string
}

// NewClient returns a client authenticating with token. Any bearer
// token with organization administration rights works: a personal
// access token, or a GitHub App installation token.
func NewClient(token string) *Client {
	return &Client{BaseURL: DefaultBaseURL, HTTP: http.DefaultClient, token: token}
}

// TokenFromEnv returns GH_TOKEN, else GITHUB_TOKEN.
func TokenFromEnv() (string, error) {
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v, nil
		}
	}

	return "", ErrNoToken
}

// String keeps the token out of any %v of the client.
func (c *Client) String() string { return "seccfg.Client{" + c.BaseURL + "}" }

type apiResponse struct {
	status int
	body   []byte
	next   string
}

func (c *Client) do(ctx context.Context, method, rawURL string, payload any) (*apiResponse, error) {
	var body io.Reader

	if payload != nil {
		enc, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode payload for %s %s: %w", method, rawURL, err)
		}

		body = bytes.NewReader(enc)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)

	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, rawURL, redact(err, c.token))
	}

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("%s %s: read body: %w", method, rawURL, err)
	}

	return &apiResponse{status: resp.StatusCode, body: raw, next: nextLink(resp.Header.Get("Link"))}, nil
}

func (r *apiResponse) failure(method, rawURL string) error {
	return fmt.Errorf("%s %s: HTTP %d: %s", method, rawURL, r.status, strings.TrimSpace(string(r.body)))
}

func redact(err error, token string) error {
	if token == "" {
		return err
	}

	return errors.New(strings.ReplaceAll(err.Error(), token, "***"))
}

func (c *Client) base() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}

	return strings.TrimRight(c.BaseURL, "/")
}

// getAll follows Link rel="next" until exhausted, decoding each page
// (a JSON array) into the accumulated result. A next link that leaves
// the client's own host is refused rather than followed, so the token
// cannot be sent elsewhere.
func getAll[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var out []T

	next := c.base() + path

	baseURL, err := url.Parse(c.base())
	if err != nil {
		return nil, err
	}

	for page := 0; next != ""; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("GET %s: more than %d pages", path, maxPages)
		}

		u, err := url.Parse(next)
		if err != nil || u.Host != baseURL.Host {
			return nil, fmt.Errorf("GET %s: refusing pagination link to a different host", path)
		}

		resp, err := c.do(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}

		if resp.status < 200 || resp.status >= 300 {
			return nil, resp.failure(http.MethodGet, next)
		}

		var items []T
		if err := json.Unmarshal(resp.body, &items); err != nil {
			return nil, fmt.Errorf("GET %s: decode: %w", next, err)
		}

		out = append(out, items...)
		next = resp.next
	}

	return out, nil
}

// nextLink extracts rel="next" from a Link header, or "".
func nextLink(h string) string {
	for part := range strings.SplitSeq(h, ",") {
		segs := strings.Split(strings.TrimSpace(part), ";")
		if len(segs) < 2 {
			continue
		}

		target := strings.Trim(strings.TrimSpace(segs[0]), "<>")

		for _, s := range segs[1:] {
			if strings.ReplaceAll(strings.TrimSpace(s), " ", "") == `rel="next"` {
				return target
			}
		}
	}

	return ""
}
