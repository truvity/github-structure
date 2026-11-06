package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// renamedRegistry is the fixture registry with its one row already renamed.
func renamedRegistry(t *testing.T) fstest.MapFS {
	t.Helper()

	raw, err := os.ReadFile("testdata/github.yaml")
	require.NoError(t, err)

	doc := strings.Replace(string(raw), "      widget:\n", "      gadget:\n", 1)
	require.NotEqual(t, string(raw), doc, "fixture row moved; update this test")

	return fstest.MapFS{"github.yaml": {Data: []byte(doc)}}
}

// stateRunner is a runner whose stack holds the fixture state of the row
// under its old name. The returned pointer receives what is imported.
func stateRunner(t *testing.T) (r *runner, imported *[]apitype.UntypedDeployment, out, errb *strings.Builder, dir string) {
	t.Helper()

	raw, err := os.ReadFile("testdata/state-widget.json")
	require.NoError(t, err)

	var export struct {
		Version    int             `json:"version"`
		Deployment json.RawMessage `json:"deployment"`
	}

	require.NoError(t, json.Unmarshal(raw, &export))

	var calls []string

	chk := script(&calls)
	chk.exportState = func(context.Context, auto.Stack) (apitype.UntypedDeployment, error) {
		return apitype.UntypedDeployment{Version: export.Version, Deployment: export.Deployment}, nil
	}

	imported = new([]apitype.UntypedDeployment)
	chk.importState = func(_ context.Context, _ auto.Stack, s apitype.UntypedDeployment) error {
		*imported = append(*imported, s)

		return nil
	}
	chk.now = func() time.Time { return time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC) }

	rr, _, _ := newRunner(t, chk)
	rr.cfg.Registry = renamedRegistry(t)
	rr.cfg.Stack = func(context.Context, string) (auto.Stack, error) { return auto.Stack{}, nil }

	var ob, eb strings.Builder

	rr.cfg.Stdout, rr.cfg.Stderr = &ob, &eb

	return rr, imported, &ob, &eb, t.TempDir()
}

func TestStateRenameRepoIsADryRunByDefault(t *testing.T) {
	r, imported, out, errb, dir := stateRunner(t)

	assert.Equal(t, 0, r.run(context.Background(), []string{"state", "rename-repo", "widget", "gadget", "--backup-dir", dir}), errb.String())

	assert.Empty(t, *imported, "a dry run writes no state")
	assert.Contains(t, out.String(), "would re-key")
	assert.Contains(t, out.String(), "repo-widget -> repo-gadget")
	assert.Contains(t, out.String(), "grant-management-widget -> grant-management-gadget")
	assert.Contains(t, out.String(), "dry run")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "a dry run saves no backup")
}

func TestStateRenameRepoApplySavesTheBackupThenImports(t *testing.T) {
	r, imported, out, errb, dir := stateRunner(t)

	// Flags may come before or after the names.
	assert.Equal(t, 0, r.run(context.Background(), []string{"state", "rename-repo", "--apply", "widget", "gadget", "--backup-dir=" + dir}), errb.String())

	require.Len(t, *imported, 1)
	assert.Equal(t, 3, (*imported)[0].Version)
	assert.Contains(t, string((*imported)[0].Deployment), "repo-gadget")
	assert.NotContains(t, string((*imported)[0].Deployment), `"repo-widget"`)

	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Contains(t, filepath.Base(matches[0]), "rename-widget-to-gadget-20261106T093000Z")

	info, err := os.Stat(matches[0])
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "a state holds encrypted secrets")

	raw, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	assert.Contains(t, string(raw), "repo-widget", "the backup is the state as it was")
	assert.Contains(t, out.String(), matches[0])
	assert.Contains(t, out.String(), "refresh preview")
}

func TestStateRenameRepoSecondRunChangesNothing(t *testing.T) {
	r, imported, _, errb, dir := stateRunner(t)

	args := []string{"state", "rename-repo", "widget", "gadget", "--apply", "--backup-dir", dir}
	assert.Equal(t, 0, r.run(context.Background(), args), errb.String())
	require.Len(t, *imported, 1)

	// The stack now holds what was imported.
	re := (*imported)[0]
	r.chk.exportState = func(context.Context, auto.Stack) (apitype.UntypedDeployment, error) { return re, nil }

	var out strings.Builder

	r.cfg.Stdout = &out

	assert.Equal(t, 0, r.run(context.Background(), args), errb.String())
	assert.Len(t, *imported, 1, "nothing was imported the second time")
	assert.Contains(t, out.String(), "nothing to do")
}

func TestStateRenameRepoRefusesAndWritesNothing(t *testing.T) {
	r, imported, _, errb, dir := stateRunner(t)

	// The registry row is still called gadget, so there is no row "other".
	assert.Equal(t, 1, r.run(context.Background(), []string{"state", "rename-repo", "widget", "other", "--apply", "--backup-dir", dir}))
	assert.Contains(t, errb.String(), `registry has no row "other"`)
	assert.Empty(t, *imported)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "a refusal saves no backup")
}

func TestStateRenameRepoImportFailureNamesTheBackup(t *testing.T) {
	r, _, _, errb, dir := stateRunner(t)
	r.chk.importState = func(context.Context, auto.Stack, apitype.UntypedDeployment) error { return errors.New("backend down") }

	assert.Equal(t, 1, r.run(context.Background(), []string{"state", "rename-repo", "widget", "gadget", "--apply", "--backup-dir", dir}))
	assert.Contains(t, errb.String(), "backend down")
	assert.Contains(t, errb.String(), dir, "the operator is told where the unchanged state is")
}

func TestStateRenameRepoUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"state"},
		{"state", "other"},
		{"state", "rename-repo"},
		{"state", "rename-repo", "widget"},
		{"state", "rename-repo", "widget", "gadget", "extra"},
		{"state", "rename-repo", "widget", "gadget", "--force"},
	} {
		r, imported, _, errb, _ := stateRunner(t)

		assert.Equal(t, 2, r.run(context.Background(), args), "%v: %s", args, errb.String())
		assert.Empty(t, *imported)
	}
}

func TestStateRenameRepoNeedsAStackSelector(t *testing.T) {
	r, _, _, errb, _ := stateRunner(t)
	r.cfg.Stack = nil

	assert.Equal(t, 1, r.run(context.Background(), []string{"state", "rename-repo", "widget", "gadget"}))
	assert.Contains(t, errb.String(), "Config.Stack is nil")
}

// The backup holds the whole state, so by default it goes to the user's
// cache directory, not to the working directory, which may be a checkout
// that gets committed.
func TestStateRenameRepoBackupDefaultsOutsideTheWorkingDirectory(t *testing.T) {
	r, imported, out, errb, _ := stateRunner(t)

	cache, cwd := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Chdir(cwd)

	assert.Equal(t, 0, r.run(context.Background(), []string{"state", "rename-repo", "widget", "gadget", "--apply"}), errb.String())
	require.Len(t, *imported, 1)

	inCache, err := filepath.Glob(filepath.Join(cache, "*", "*.json"))
	require.NoError(t, err)
	require.Len(t, inCache, 1, "the backup is in the cache directory, under the tool's name")
	assert.Contains(t, out.String(), inCache[0])

	info, err := os.Stat(filepath.Dir(inCache[0]))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "the directory is private too")

	inCwd, err := os.ReadDir(cwd)
	require.NoError(t, err)
	assert.Empty(t, inCwd, "nothing lands in the working directory")
}

func TestStateRenameRepoWarnsOfABackupInsideAGitWorkTree(t *testing.T) {
	r, imported, _, errb, _ := stateRunner(t)

	tree := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(tree, ".git"), 0o700))

	dir := filepath.Join(tree, "sub")
	require.NoError(t, os.Mkdir(dir, 0o700))

	assert.Equal(t, 0, r.run(context.Background(), []string{"state", "rename-repo", "widget", "gadget", "--apply", "--backup-dir", dir}), errb.String())
	require.Len(t, *imported, 1, "an explicit directory is the operator's choice")

	assert.Contains(t, errb.String(), "inside a git work tree")
	assert.Contains(t, errb.String(), dir)
}

func TestStateRenameRepoNoWarningOutsideAGitWorkTree(t *testing.T) {
	r, _, _, errb, dir := stateRunner(t)

	assert.Equal(t, 0, r.run(context.Background(), []string{"state", "rename-repo", "widget", "gadget", "--apply", "--backup-dir", dir}), errb.String())
	assert.NotContains(t, errb.String(), "git work tree")
}
