package app

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	registry "github.com/truvity/github-structure/pkg/registry"
)

// ── manifest ───────────────────────────────────────────────────────────

func ourApp() *registry.App {
	return &registry.App{
		Description: "Line one\n  line two\n",
		URL:         "https://github.com/truvity/gitops",
		Install:     registry.InstallAll,
		Permissions: map[string]string{
			"organization_administration": "write",
			"administration":              "write",
			"contents":                    "read",
			"metadata":                    "read",
		},
		Credentials: &registry.AppCredentials{
			OpItem:    "github-app-truvity-iac",
			SSMPrefix: "/creds/structure-engine/example",
		},
	}
}

func TestBuildManifest(t *testing.T) {
	got, err := BuildManifest("truvity-iac", ourApp(), "http://127.0.0.1:9797/callback")
	require.NoError(t, err)

	assert.Equal(t, "truvity-iac", got.Name)
	assert.Equal(t, "http://127.0.0.1:9797/callback", got.RedirectURL)
	assert.False(t, got.Public, "our Apps must never be public")
	assert.Nil(t, got.HookAttrs, "API-only Apps carry no webhook block at all")

	// The folded YAML description becomes one line.
	assert.Equal(t, "Line one line two", got.Description)

	// Permissions come from the registry row verbatim, except metadata —
	// GitHub grants it implicitly and rejects it in a manifest.
	assert.Equal(t, map[string]string{
		"organization_administration": "write",
		"administration":              "write",
		"contents":                    "read",
	}, got.DefaultPerm)
}

func TestBuildManifestRefusesExternal(t *testing.T) {
	app := ourApp()
	app.External = true
	app.Credentials = nil

	_, err := BuildManifest("slack", app, "http://127.0.0.1:9797/callback")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "external")
}

// Re-creating an adopted App would mint a SECOND App with a colliding
// display name — GitHub allows it, and the result is a confusing mess.
func TestBuildManifestRefusesAdopted(t *testing.T) {
	app := ourApp()
	app.Adopted = true
	app.AppID = 4260816

	_, err := BuildManifest("truvity-arc-preview", app, "http://127.0.0.1:9797/callback")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestManifestSerialisesAsGitHubExpects(t *testing.T) {
	manifest, err := BuildManifest("truvity-iac", ourApp(), "http://127.0.0.1:9797/callback")
	require.NoError(t, err)

	body, err := json.Marshal(manifest)
	require.NoError(t, err)

	var round map[string]any
	require.NoError(t, json.Unmarshal(body, &round))

	// Field names are GitHub's, not ours — a rename here breaks the flow
	// silently (GitHub ignores unknown keys and creates a bare App).
	assert.Contains(t, round, "name")
	assert.Contains(t, round, "url")
	assert.Contains(t, round, "redirect_url")
	assert.Contains(t, round, "default_permissions")

	// GitHub REQUIRES `url` inside hook_attributes whenever the object is
	// present, and rejects the manifest with "url wasn't supplied" — an
	// error that reads as though the TOP-LEVEL url were missing. Sending
	// {"active": false} cost a real click sitting on 2026-07-28.
	assert.NotContains(t, round, "hook_attributes",
		"an API-only App must omit hook_attributes entirely, not send it empty")
}

func TestBuildManifestIncludesWebhookWhenConfigured(t *testing.T) {
	app := ourApp()
	app.WebhookURL = "https://example.com/hook"

	got, err := BuildManifest("truvity-hooked", app, "http://127.0.0.1:9797/callback")
	require.NoError(t, err)

	require.NotNil(t, got.HookAttrs)
	assert.Equal(t, "https://example.com/hook", got.HookAttrs.URL)
	assert.True(t, got.HookAttrs.Active)

	// And when present it must carry the url GitHub demands.
	body, err := json.Marshal(got)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"hook_attributes":{"url":"https://example.com/hook"`)
}

func TestCreateAndInstallURLs(t *testing.T) {
	assert.Equal(t,
		"https://github.com/organizations/truvity/settings/apps/new?state=abc",
		CreateURL("truvity", "abc"))

	assert.Equal(t,
		"https://github.com/apps/truvity-iac/installations/new",
		InstallURL("truvity-iac"))
}

// ── JWT ────────────────────────────────────────────────────────────────

func TestJWTVerifies(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	pkcs1 := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	token, err := JWT(12345, pkcs1)
	require.NoError(t, err)

	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)

	// The signature must actually verify — a JWT GitHub rejects would
	// only surface as a 401 at the worst possible moment.
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)

	require.NoError(t, rsa.VerifyPKCS1v15(&key.PublicKey, 5, sum[:], sig))

	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(payload, &claims))

	assert.Equal(t, "12345", claims.Iss)

	now := time.Now().Unix()
	assert.LessOrEqual(t, claims.Exp-now, int64(600), "GitHub rejects an exp more than 10 minutes out")
	assert.Positive(t, claims.Exp-now, "token must not be born expired")
	assert.LessOrEqual(t, claims.Iat, now, "GitHub rejects a future-dated iat; we backdate to absorb clock skew")
}

// GitHub issues PKCS#1; a 1Password round-trip has been known to
// re-encode as PKCS#8, and failing on that would look like a corrupt key.
func TestJWTAcceptsPKCS8(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	_, err = JWT(1, pkcs8)
	require.NoError(t, err)
}

func TestJWTRejectsGarbage(t *testing.T) {
	_, err := JWT(1, []byte("not a pem"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PEM")
}

// ── drift comparison ───────────────────────────────────────────────────

func TestCompareAppClean(t *testing.T) {
	app := &registry.App{
		AppID:          10,
		InstallationID: 20,
		Install:        registry.InstallAll,
		Permissions:    map[string]string{"contents": "read", "metadata": "read"},
	}

	inst := installation{
		ID: 20, AppID: 10, AppSlug: "x", RepositorySelection: "all",
		Permissions: map[string]string{"contents": "read", "metadata": "read"},
	}

	assert.Empty(t, compareApp("x", app, inst))
}

// The case this exists for: an App silently gains a permission in the
// browser. IaC cannot prevent it (no API), so it must at least shout.
func TestCompareAppDetectsWidenedPermission(t *testing.T) {
	app := &registry.App{
		AppID: 10, InstallationID: 20, Install: registry.InstallAll,
		Permissions: map[string]string{"contents": "read"},
	}

	inst := installation{
		ID: 20, AppID: 10, RepositorySelection: "all",
		Permissions: map[string]string{"contents": "write", "administration": "write"},
	}

	drifts := compareApp("x", app, inst)
	require.Len(t, drifts, 2)

	// Declared-but-changed first, then present-but-undeclared.
	assert.Equal(t, "permission contents", drifts[0].Field)
	assert.Equal(t, "read", drifts[0].Want)
	assert.Equal(t, "write", drifts[0].Got)

	assert.Equal(t, "permission administration", drifts[1].Field)
	assert.Equal(t, "", drifts[1].Want, "an undeclared permission has no registry value")
	assert.Equal(t, "write", drifts[1].Got)
}

func TestCompareAppDetectsScopeChange(t *testing.T) {
	app := &registry.App{AppID: 10, InstallationID: 20, Install: registry.InstallSelected}
	inst := installation{ID: 20, AppID: 10, RepositorySelection: "all"}

	drifts := compareApp("x", app, inst)
	require.Len(t, drifts, 1)
	assert.Equal(t, "install", drifts[0].Field)
	assert.Equal(t, "selected", drifts[0].Want)
	assert.Equal(t, "all", drifts[0].Got)
}

// ── 1Password field handling ───────────────────────────────────────────

// The bug this guards: recording an installation ID after the install
// click must not drop the private key written moments earlier.
func TestCollapse(t *testing.T) {
	assert.Equal(t, "one two three", collapse("one\n  two\n  three\n"))
	assert.Equal(t, "", collapse("   \n  "))
}

// TestCompareOrgVariables exercises each way a variable can drift, and
// mutates the live side for every case — a test that only checked "no
// drift when equal" would pass against a function that always returns
// nil.
func TestCompareOrgVariables(t *testing.T) {
	t.Parallel()

	want := map[string]*registry.OrgVariable{
		"CI_RUNNER_LABEL":    {Value: "preview"},
		"RENOVATE_CLIENT_ID": {Value: "Iv23", Visibility: "selected"},
	}

	matching := map[string]liveVariable{
		"CI_RUNNER_LABEL":    {value: "preview", visibility: "private"},
		"RENOVATE_CLIENT_ID": {value: "Iv23", visibility: "selected"},
	}

	if drifts := compareOrgVariables(want, matching); len(drifts) != 0 {
		t.Fatalf("expected no drift, got %v", drifts)
	}

	for name, mutate := range map[string]func(map[string]liveVariable){
		"value changed": func(l map[string]liveVariable) {
			l["CI_RUNNER_LABEL"] = liveVariable{value: "stable", visibility: "private"}
		},
		// The one that matters most: widening to `all` exposes internal
		// names to any public repository's workflow.
		"visibility widened": func(l map[string]liveVariable) {
			l["CI_RUNNER_LABEL"] = liveVariable{value: "preview", visibility: "all"}
		},
		"declared but deleted live": func(l map[string]liveVariable) {
			delete(l, "CI_RUNNER_LABEL")
		},
		"live but undeclared": func(l map[string]liveVariable) {
			l["SNEAKY"] = liveVariable{value: "x", visibility: "all"}
		},
	} {
		live := make(map[string]liveVariable, len(matching))
		for k, v := range matching {
			live[k] = v
		}

		mutate(live)

		if drifts := compareOrgVariables(want, live); len(drifts) != 1 {
			t.Errorf("%s: expected exactly 1 drift, got %d: %v", name, len(drifts), drifts)
		}
	}

	// An omitted visibility means private, not "anything goes".
	defaulted := map[string]liveVariable{
		"CI_RUNNER_LABEL":    {value: "preview", visibility: "selected"},
		"RENOVATE_CLIENT_ID": {value: "Iv23", visibility: "selected"},
	}

	drifts := compareOrgVariables(want, defaulted)
	if len(drifts) != 1 || drifts[0].Field != "visibility" || drifts[0].Want != "private" {
		t.Errorf("omitted visibility should default to private, got %v", drifts)
	}
}

func TestCompareOrgOwnersMatches(t *testing.T) {
	t.Parallel()

	// Case differs between registry and API on purpose: GitHub logins are
	// case-insensitive, and a comparison that missed that would report one
	// owner as both undeclared and absent, forever.
	got := compareOrgOwners(
		[]string{"excavador", "TrustForm-Adm"},
		[]string{"trustform-adm", "Excavador"},
	)

	assert.Empty(t, got)
}

func TestCompareOrgOwnersReportsUndeclaredOwner(t *testing.T) {
	t.Parallel()

	// The case this check exists for: someone was made an owner
	// out-of-band. The engine cannot demote them — it asserts per login
	// and never reconciles the set — so reporting is the only way an
	// unexplained owner surfaces at all.
	got := compareOrgOwners([]string{"excavador"}, []string{"excavador", "rogue"})

	require.Len(t, got, 1)
	assert.Equal(t, "owner rogue", got[0].Subject)
	assert.Equal(t, "existence", got[0].Field)
	assert.Equal(t, "admin", got[0].Got)
}

func TestCompareOrgOwnersReportsDeclaredNonOwner(t *testing.T) {
	t.Parallel()

	got := compareOrgOwners([]string{"excavador", "demoted"}, []string{"excavador"})

	require.Len(t, got, 1)
	assert.Equal(t, "owner demoted", got[0].Subject)
	assert.Equal(t, "role", got[0].Field)
	assert.Equal(t, "not an owner", got[0].Got)
}

func TestCompareOrgOwnersEmptyDeclarationReportsNothing(t *testing.T) {
	t.Parallel()

	// An org that declares no owners opts out of the check entirely.
	// CheckOrgOwners returns early on this, and the comparison must agree:
	// treating "no declaration" as "no owner may exist" would fail every
	// org that has not adopted the field yet.
	assert.Empty(t, compareOrgOwners(nil, []string{"excavador"}))
}
