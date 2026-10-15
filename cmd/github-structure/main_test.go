package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fakeGitHub(t *testing.T, posts *atomic.Int32) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/orgs/example-org/code-security/configurations":
			_, _ = w.Write([]byte(`[{"id":10,"name":"Baseline"}]`))
		case r.URL.Path == "/orgs/example-org/repos":
			_, _ = w.Write([]byte(`[{"id":1,"name":"example-repo"},{"id":2,"name":"example-api"}]`))
		case strings.HasSuffix(r.URL.Path, "/code-security-configuration"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost:
			posts.Add(1)
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	return srv.URL
}

func desiredFile(t *testing.T) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), "desired.yaml")
	require.NoError(t, os.WriteFile(p, []byte("repos:\n  example-repo: Baseline\n"), 0o600))

	return p
}

func TestPlanIsReadOnlyAndPrintsNoToken(t *testing.T) {
	var posts atomic.Int32

	t.Setenv("GH_TOKEN", "s3cret-token")

	var out, errb bytes.Buffer

	code := run(context.Background(), []string{"security-configurations", "plan", "--org", "example-org", "--desired", desiredFile(t)},
		&out, &errb, fakeGitHub(t, &posts))

	assert.Equal(t, 0, code, errb.String())
	assert.Contains(t, out.String(), "attach example-repo: <none> -> Baseline")
	assert.Contains(t, out.String(), "1 unmanaged")
	assert.NotContains(t, out.String()+errb.String(), "s3cret-token")
	assert.Zero(t, posts.Load())
}

func TestApplyRefusesWithoutConfirm(t *testing.T) {
	var posts atomic.Int32

	t.Setenv("GH_TOKEN", "t")

	var out, errb bytes.Buffer

	args := []string{"security-configurations", "apply", "--org", "example-org", "--desired", desiredFile(t)}
	url := fakeGitHub(t, &posts)

	assert.Equal(t, 1, run(context.Background(), args, &out, &errb, url))
	assert.Contains(t, errb.String(), "--confirm")
	assert.Zero(t, posts.Load())

	assert.Equal(t, 0, run(context.Background(), append(args, "--confirm"), &out, &errb, url))
	assert.Equal(t, int32(1), posts.Load())
}

func TestUsageAndUnknownNamesExitNonZero(t *testing.T) {
	var out, errb bytes.Buffer

	assert.Equal(t, 2, run(context.Background(), nil, &out, &errb, ""))

	t.Setenv("GH_TOKEN", "t")

	var posts atomic.Int32

	p := filepath.Join(t.TempDir(), "d.yaml")
	require.NoError(t, os.WriteFile(p, []byte("repos:\n  example-repo: Missing\n"), 0o600))
	assert.Equal(t, 1, run(context.Background(), []string{"security-configurations", "plan", "--org", "example-org", "--desired", p},
		&out, &errb, fakeGitHub(t, &posts)))
	assert.Contains(t, errb.String(), "unknown security configuration")
}
