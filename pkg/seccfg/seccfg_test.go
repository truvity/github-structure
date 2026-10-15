package seccfg_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-structure/pkg/seccfg"
)

const secret = "s3cret-token"

// fake is a GitHub stand-in: two-per-page lists with Link headers, the
// per-repository attachment endpoint, and a recorder for writes.
type fake struct {
	t       *testing.T
	configs []map[string]any
	repos   []map[string]any
	// attached: repo name -> {status, configuration name}
	attached map[string][2]string
	srv      *httptest.Server

	mu      sync.Mutex
	posts   []post
	methods []string
}

type post struct {
	Path string
	Body struct {
		Scope string  `json:"scope"`
		IDs   []int64 `json:"selected_repository_ids"`
	}
}

func newFake(t *testing.T) *fake {
	t.Helper()

	f := &fake{
		t: t,
		configs: []map[string]any{
			{"id": 10, "name": "Baseline"},
			{"id": 20, "name": "Strict"},
			{"id": 30, "name": "Spare"},
		},
		repos: []map[string]any{
			{"id": 1, "name": "example-repo", "archived": false},
			{"id": 2, "name": "example-api", "archived": false},
			{"id": 3, "name": "example-web", "archived": false},
			{"id": 4, "name": "example-old", "archived": true},
			{"id": 5, "name": "example-other", "archived": false},
		},
		attached: map[string][2]string{
			"example-repo": {"enforced", "Baseline"},
			"example-api":  {"attached", "Strict"},
			"example-web":  {"detached", "Strict"},
		},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)

	return f
}

func (f *fake) client() *seccfg.Client {
	c := seccfg.NewClient(secret)
	c.BaseURL = f.srv.URL

	return c
}

func page(w http.ResponseWriter, r *http.Request, items []map[string]any, srvURL string) {
	const size = 2

	n := 1
	if p := r.URL.Query().Get("page"); p != "" {
		_, _ = fmt.Sscan(p, &n)
	}

	lo, hi := (n-1)*size, min(n*size, len(items))
	if lo > len(items) {
		lo = len(items)
	}

	if hi < len(items) {
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?per_page=100&page=%d>; rel="next"`, srvURL, r.URL.Path, n+1))
	}

	_ = json.NewEncoder(w).Encode(items[lo:hi])
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.methods = append(f.methods, r.Method+" "+r.URL.Path)

	if r.Header.Get("Authorization") != "Bearer "+secret {
		http.Error(w, "bad credentials", http.StatusUnauthorized)

		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/orgs/example-org/code-security/configurations":
		page(w, r, f.configs, f.srv.URL)
	case r.Method == http.MethodGet && r.URL.Path == "/orgs/example-org/repos":
		page(w, r, f.repos, f.srv.URL)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/code-security-configuration"):
		name := strings.Split(r.URL.Path, "/")[3]

		a, ok := f.attached[name]
		if !ok {
			w.WriteHeader(http.StatusNoContent)

			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"status": a[0], "configuration": map[string]any{"id": f.idOf(a[1]), "name": a[1]}})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/attach"):
		p := post{Path: r.URL.Path}
		assert.NoError(f.t, json.NewDecoder(r.Body).Decode(&p.Body))
		f.posts = append(f.posts, p)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("{}"))
	default:
		http.NotFound(w, r)
	}
}

func (f *fake) idOf(name string) any {
	for _, c := range f.configs {
		if c["name"] == name {
			return c["id"]
		}
	}

	return nil
}

func (f *fake) writes() []string {
	var out []string

	for _, m := range f.methods {
		if !strings.HasPrefix(m, "GET ") {
			out = append(out, m)
		}
	}

	return out
}

func TestPlanDiffAcrossPages(t *testing.T) {
	f := newFake(t)
	f.attached = map[string][2]string{}

	plan, err := seccfg.Plan(context.Background(), f.client(), "example-org", map[string]string{
		"example-repo": "Baseline",
		"example-api":  "Strict",
	})
	require.NoError(t, err)

	assert.Equal(t, []string{
		"attach example-api: <none> -> Strict",
		"attach example-repo: <none> -> Baseline",
	}, plan.Lines())
	// Pagination: 5 repos over 3 pages and 3 configs over 2, with the
	// archived repository left out of "unmanaged".
	assert.Equal(t, []string{"example-other", "example-web"}, plan.Unmanaged)
	assert.Empty(t, f.writes())
}

func TestPlanComparesCurrentAttachment(t *testing.T) {
	f := newFake(t)
	plan, err := seccfg.Plan(context.Background(), f.client(), "example-org", map[string]string{
		"example-repo":  "Baseline", // already on it (enforced)
		"example-api":   "Baseline", // moves off Strict
		"example-web":   "Strict",   // detached: counts as none
		"example-other": "Strict",   // 204: none
	})
	require.NoError(t, err)

	assert.Equal(t, []string{
		"attach example-api: Strict -> Baseline",
		"attach example-other: <none> -> Strict",
		"attach example-web: <none> -> Strict",
	}, plan.Lines())
	assert.Equal(t, []string{"example-repo"}, plan.InSync)
	assert.Empty(t, plan.Unmanaged)
}

func TestApplyGroupsPerConfigurationAndNeverDetaches(t *testing.T) {
	f := newFake(t)
	f.attached = map[string][2]string{"example-api": {"attached", "Strict"}}

	desired := map[string]string{
		"example-repo":  "Baseline",
		"example-web":   "Baseline",
		"example-other": "Strict",
	}

	plan, err := seccfg.Plan(context.Background(), f.client(), "example-org", desired)
	require.NoError(t, err)
	require.NoError(t, seccfg.Apply(context.Background(), f.client(), plan))

	// One call per target configuration, all its repositories in it.
	require.Len(t, f.posts, 2)
	assert.Equal(t, "/orgs/example-org/code-security/configurations/10/attach", f.posts[0].Path)
	assert.Equal(t, "selected", f.posts[0].Body.Scope)
	assert.Equal(t, []int64{1, 3}, f.posts[0].Body.IDs)
	assert.Equal(t, "/orgs/example-org/code-security/configurations/20/attach", f.posts[1].Path)
	assert.Equal(t, []int64{5}, f.posts[1].Body.IDs)

	// example-api is unmanaged: reported, never touched. Nothing but
	// attach POSTs was sent: no DELETE, no PUT, no detach.
	assert.Contains(t, plan.Unmanaged, "example-api")

	for _, w := range f.writes() {
		assert.True(t, strings.HasPrefix(w, "POST ") && strings.HasSuffix(w, "/attach"), w)
	}
}

func TestApplyEmptyPlanWritesNothing(t *testing.T) {
	f := newFake(t)
	require.NoError(t, seccfg.Apply(context.Background(), f.client(), seccfg.ChangeSet{Org: "example-org"}))
	assert.Empty(t, f.methods)
}

func TestPlanUnknownNamesAreErrorsBeforeAnyWrite(t *testing.T) {
	f := newFake(t)

	_, err := seccfg.Plan(context.Background(), f.client(), "example-org", map[string]string{
		"example-repo": "Nope",
		"missing-repo": "Baseline",
		"example-old":  "Baseline",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown security configuration "Nope"`)
	assert.Contains(t, err.Error(), `unknown repository "missing-repo"`)
	assert.Contains(t, err.Error(), `"example-old" is archived`)
	assert.Empty(t, f.writes())
	assert.Empty(t, f.posts)
}

func TestErrorsNeverCarryTheToken(t *testing.T) {
	f := newFake(t)
	c := seccfg.NewClient("wrong-" + secret)
	c.BaseURL = f.srv.URL

	_, err := seccfg.Plan(context.Background(), c, "example-org", map[string]string{"example-repo": "Baseline"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.NotContains(t, c.String(), secret)
}

func TestPaginationRefusesForeignHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<https://evil.example/next>; rel="next"`)
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	c := seccfg.NewClient(secret)
	c.BaseURL = srv.URL

	_, err := seccfg.Plan(context.Background(), c, "example-org", nil)
	require.ErrorContains(t, err, "different host")
}

func TestTokenFromEnv(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	_, err := seccfg.TokenFromEnv()
	require.ErrorIs(t, err, seccfg.ErrNoToken)

	t.Setenv("GITHUB_TOKEN", "b")
	tok, err := seccfg.TokenFromEnv()
	require.NoError(t, err)
	assert.Equal(t, "b", tok)

	t.Setenv("GH_TOKEN", "a")
	tok, _ = seccfg.TokenFromEnv()
	assert.Equal(t, "a", tok)
}
