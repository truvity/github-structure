package app

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// errNotInstalled means the App exists but no installation on the target
// org has appeared yet — the state we poll through, not a failure.
var errNotInstalled = errors.New("app not installed on the org")

func isNotInstalled(err error) bool { return errors.Is(err, errNotInstalled) }

// JWT mints the short-lived RS256 assertion an App authenticates with
// (GitHub caps the lifetime at 10 minutes and rejects future-dated iat,
// so we backdate a minute to absorb clock skew).
//
// Hand-rolled rather than pulling in a JWT library: this is the entire
// surface we need, and a signing dependency in the credential path is
// exactly the kind of thing worth not having.
func JWT(appID int64, privateKey []byte) (string, error) {
	key, err := parseRSAKey(privateKey)
	if err != nil {
		return "", err
	}

	now := time.Now()

	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": fmt.Sprintf("%d", appID),
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}

	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(headerJSON) + "." + enc.EncodeToString(claimsJSON)

	sum := sha256.Sum256([]byte(signing))

	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("signing app JWT: %w", err)
	}

	return signing + "." + enc.EncodeToString(sig), nil
}

// parseRSAKey accepts both PKCS#1 ("RSA PRIVATE KEY") and PKCS#8
// ("PRIVATE KEY") PEM blocks. GitHub issues PKCS#1; 1Password round-trips
// have been known to re-encode, so accept either rather than fail late.
func parseRSAKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("private key is not PEM")
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing private key: %w", err)
	}

	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is %T, want RSA", parsed)
	}

	return key, nil
}

// FindInstallation returns the App's installation ID on the given org.
func FindInstallation(ctx context.Context, appID int64, privateKey []byte, org string) (int64, error) {
	var installations []struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}

	if err := appGet(ctx, appID, privateKey, "/app/installations?per_page=100", &installations); err != nil {
		return 0, err
	}

	for _, inst := range installations {
		if strings.EqualFold(inst.Account.Login, org) {
			return inst.ID, nil
		}
	}

	return 0, errNotInstalled
}

// InstallationRepos lists the repositories a `selected`-scope
// installation covers. This is the ONLY way to read that scope — there
// is no endpoint a user token can call (snapshot §4.3) — which is why
// our own Apps can be reconciled and vendor ones cannot.
func InstallationRepos(ctx context.Context, appID int64, privateKey []byte, installationID int64) ([]string, error) {
	token, err := installationToken(ctx, appID, privateKey, installationID)
	if err != nil {
		return nil, err
	}

	var page struct {
		Repositories []struct {
			Name string `json:"name"`
		} `json:"repositories"`
	}

	req, err := newAPIRequest(ctx, http.MethodGet, "/installation/repositories?per_page=100", "token "+token)
	if err != nil {
		return nil, err
	}

	if err := doJSON(req, &page); err != nil {
		return nil, err
	}

	names := make([]string, 0, len(page.Repositories))
	for _, r := range page.Repositories {
		names = append(names, r.Name)
	}

	return names, nil
}

func installationToken(ctx context.Context, appID int64, privateKey []byte, installationID int64) (string, error) {
	assertion, err := JWT(appID, privateKey)
	if err != nil {
		return "", err
	}

	req, err := newAPIRequest(ctx, http.MethodPost,
		fmt.Sprintf("/app/installations/%d/access_tokens", installationID), "Bearer "+assertion)
	if err != nil {
		return "", err
	}

	var out struct {
		Token string `json:"token"`
	}

	if err := doJSON(req, &out); err != nil {
		return "", err
	}

	return out.Token, nil
}

func appGet(ctx context.Context, appID int64, privateKey []byte, path string, dst any) error {
	assertion, err := JWT(appID, privateKey)
	if err != nil {
		return err
	}

	req, err := newAPIRequest(ctx, http.MethodGet, path, "Bearer "+assertion)
	if err != nil {
		return err
	}

	return doJSON(req, dst)
}

func newAPIRequest(ctx context.Context, method, path, authorization string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, http.NoBody)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", authorization)
	req.Header.Set(apiVersionHeader, apiVersion)

	return req, nil
}

func doJSON(req *http.Request, dst any) error {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", req.Method, req.URL.Path, resp.Status, strings.TrimSpace(string(body)))
	}

	if dst == nil {
		return nil
	}

	return json.Unmarshal(body, dst)
}
