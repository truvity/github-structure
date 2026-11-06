package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-structure/pkg/registry"
)

// ceilingRegistry is the fixture registry with a fragment_ceiling on its
// org, a second preset and one access bundle for a fragment to name.
func ceilingRegistry(t *testing.T) fstest.MapFS {
	t.Helper()

	raw, err := os.ReadFile("testdata/github.yaml")
	require.NoError(t, err)

	doc := string(raw)

	for _, edit := range [][2]string{
		{"access:\n  managers:\n    management: pull\n",
			"  private:\n    visibility: private\naccess:\n  managers:\n    management: pull\n  devs:\n    engineers: push\n"},
		{"      ssm_prefix: /creds/structure-engine/acme\n",
			"      ssm_prefix: /creds/structure-engine/acme\n" +
				"    fragment_ceiling:\n" +
				"      presets: [private]\n" +
				"      access: [devs]\n" +
				"      bypass_apps: [release-bot]\n" +
				"      max_team_permission: push\n" +
				"      reserved:\n" +
				"        repos: [reserved-repo]\n"},
	} {
		next := strings.Replace(doc, edit[0], edit[1], 1)
		require.NotEqual(t, doc, next, "fixture moved; update this test")

		doc = next
	}

	return fstest.MapFS{"github.yaml": {Data: []byte(doc)}}
}

const goodFragmentYAML = `org: acme
teams:
  squad:
    privacy: closed
repos:
  gadget:
    preset: private
    access: [devs]
    teams:
      squad: push
`

const badFragmentYAML = `org: acme
repos:
  widget:
    preset: private
  gizmo:
    preset: private
    tag_rulesets:
      - name: release
        pattern: refs/tags/v*
        bypass_apps: [x]
owners: [someone]
`

func writeFile(t *testing.T, name, content string) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))

	return p
}

// fragmentRunner has the ceiling registry; bare has none, as a company's
// own CI does.
func fragmentRunner(t *testing.T, bare bool) (*runner, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()

	r, out, errb := newRunner(t, script(new([]string)))
	r.cfg.Registry = ceilingRegistry(t)

	if bare {
		r.cfg.Registry = nil
		r.cfg.DefaultOrg = ""
	}

	return r, out, errb
}

func TestBoundaryPrintsTheCeilingAsJSONWithoutValues(t *testing.T) {
	r, out, errb := fragmentRunner(t, false)

	require.Equal(t, 0, r.run(context.Background(), []string{"boundary"}), errb.String())

	var b registry.Boundary
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&b), out.String())

	assert.Equal(t, "acme", b.Org)
	assert.Equal(t, "push", b.MaxTeamPermission)
	assert.Equal(t, []string{"devs"}, b.Access)
	assert.Equal(t, []string{"release-bot"}, b.BypassApps)
	assert.Contains(t, b.Reserved.Repos, "widget", "every name the provider declares is reserved")
	assert.Contains(t, b.Reserved.Repos, "reserved-repo")
	assert.NotContains(t, out.String(), "ssm_prefix", "the boundary names things and carries no credentials path")
	assert.NotContains(t, out.String(), "/creds/")
}

func TestBoundaryUsesTheOrgFlag(t *testing.T) {
	r, out, errb := fragmentRunner(t, false)

	require.Equal(t, 0, r.run(context.Background(), []string{"--org", "acme", "boundary"}), errb.String())
	assert.Contains(t, out.String(), `"org": "acme"`)

	errb.Reset()
	assert.Equal(t, 1, r.run(context.Background(), []string{"--org", "other", "boundary"}))
	assert.Contains(t, errb.String(), `"other"`)
}

func TestBoundaryRefusesAnOrgWithoutACeiling(t *testing.T) {
	var out, errb bytes.Buffer

	r, _, _ := newRunner(t, script(new([]string)))
	r.cfg.Stdout, r.cfg.Stderr = &out, &errb

	assert.Equal(t, 1, r.run(context.Background(), []string{"boundary"}))
	assert.Contains(t, errb.String(), "fragment_ceiling")
	assert.Empty(t, out.String(), "a refusal prints no boundary")
}

func TestBoundaryTakesNoArguments(t *testing.T) {
	r, _, errb := fragmentRunner(t, false)

	assert.Equal(t, 2, r.run(context.Background(), []string{"boundary", "extra"}))
	assert.Contains(t, errb.String(), "boundary")
}

func TestValidateFragmentAcceptsAFragmentInsideTheRegistryBoundary(t *testing.T) {
	r, out, errb := fragmentRunner(t, false)
	path := writeFile(t, "fragment.yaml", goodFragmentYAML)

	require.Equal(t, 0, r.run(context.Background(), []string{"validate-fragment", "--owner", "globex", path}), errb.String())
	assert.Contains(t, out.String(), `org "acme": fragment owner "globex": ok`)
}

func TestValidateFragmentRefusesEveryViolationWithItsKeyPath(t *testing.T) {
	r, out, errb := fragmentRunner(t, false)
	path := writeFile(t, "fragment.yaml", badFragmentYAML)

	require.Equal(t, 1, r.run(context.Background(), []string{"validate-fragment", "--owner", "globex", path}))

	assert.Empty(t, out.String())
	assert.Contains(t, errb.String(), `org "acme": fragment owner "globex"`)
	assert.Contains(t, errb.String(), "repos.widget: provider-owned (reserved)")
	assert.Contains(t, errb.String(), `repos.gizmo.tag_rulesets[0].bypass_apps[0] "x" outside ceiling`)
	assert.Contains(t, errb.String(), "owners: not allowed in a fragment")
}

// A company's CI has the exported boundary and nothing else, and sees the
// words the provider's Compose would say.
func TestValidateFragmentAgainstAnExportedBoundaryNeedsNoRegistry(t *testing.T) {
	prov, pout, perr := fragmentRunner(t, false)
	require.Equal(t, 0, prov.run(context.Background(), []string{"boundary"}), perr.String())

	boundary := writeFile(t, "boundary.json", pout.String())

	company, out, errb := fragmentRunner(t, true)

	good := writeFile(t, "good.yaml", goodFragmentYAML)
	require.Equal(t, 0, company.run(context.Background(), []string{"validate-fragment", "--boundary", boundary, "--owner", "globex", good}), errb.String())
	assert.Contains(t, out.String(), "ok")

	bad := writeFile(t, "bad.yaml", badFragmentYAML)
	errb.Reset()
	require.Equal(t, 1, company.run(context.Background(), []string{"validate-fragment", "--owner", "globex", "--boundary=" + boundary, bad}))

	// The same refusal the provider's own check gives.
	prov2, _, perr2 := fragmentRunner(t, false)
	require.Equal(t, 1, prov2.run(context.Background(), []string{"validate-fragment", "--owner", "globex", bad}))
	assert.Equal(t, perr2.String(), errb.String())
}

func TestValidateFragmentWithoutABoundaryOrRegistryFailsLoudly(t *testing.T) {
	company, _, errb := fragmentRunner(t, true)
	path := writeFile(t, "fragment.yaml", goodFragmentYAML)

	assert.Equal(t, 1, company.run(context.Background(), []string{"validate-fragment", "--owner", "globex", path}))
	assert.Contains(t, errb.String(), "--boundary")
}

func TestValidateFragmentHoldsTheFragmentToTheBoundarysOrg(t *testing.T) {
	prov, pout, perr := fragmentRunner(t, false)
	require.Equal(t, 0, prov.run(context.Background(), []string{"boundary"}), perr.String())

	boundary := writeFile(t, "boundary.json", pout.String())
	other := writeFile(t, "other.yaml", strings.Replace(goodFragmentYAML, "org: acme", "org: initech", 1))

	company, _, errb := fragmentRunner(t, true)

	assert.Equal(t, 1, company.run(context.Background(), []string{"validate-fragment", "--boundary", boundary, "--owner", "globex", other}))
	assert.Contains(t, errb.String(), `the fragment is for "initech"`)
}

func TestValidateFragmentRefusesWhatItCannotRead(t *testing.T) {
	prov, pout, perr := fragmentRunner(t, false)
	require.Equal(t, 0, prov.run(context.Background(), []string{"boundary"}), perr.String())

	good := writeFile(t, "good.yaml", goodFragmentYAML)
	boundary := writeFile(t, "boundary.json", pout.String())

	for name, tc := range map[string]struct {
		boundaryArg string
		fragment    string
		want        string
	}{
		"missing fragment":        {boundary, filepath.Join(t.TempDir(), "absent.yaml"), "absent.yaml"},
		"unknown key in fragment": {boundary, writeFile(t, "f.yaml", goodFragmentYAML+"colour: red\n"), "colour"},
		"fragment without org":    {boundary, writeFile(t, "f.yaml", "repos: {}\n"), "org"},
		"missing boundary":        {filepath.Join(t.TempDir(), "absent.json"), good, "absent.json"},
		"unknown key in boundary": {writeFile(t, "b.json", `{"org":"acme","colour":"red"}`), good, "colour"},
		"boundary is not json":    {writeFile(t, "b.json", "org: acme\n"), good, "boundary"},
	} {
		t.Run(name, func(t *testing.T) {
			company, out, errb := fragmentRunner(t, true)

			assert.Equal(t, 1, company.run(context.Background(),
				[]string{"validate-fragment", "--boundary", tc.boundaryArg, "--owner", "globex", tc.fragment}))
			assert.Contains(t, errb.String(), tc.want)
			assert.Empty(t, out.String(), "nothing is declared valid")
		})
	}
}

func TestValidateFragmentUsageErrors(t *testing.T) {
	good := writeFile(t, "good.yaml", goodFragmentYAML)

	for name, args := range map[string][]string{
		"no file":      {"validate-fragment", "--owner", "globex"},
		"two files":    {"validate-fragment", "--owner", "globex", good, good},
		"no owner":     {"validate-fragment", good},
		"unknown flag": {"validate-fragment", "--owner", "globex", "--strict", good},
	} {
		t.Run(name, func(t *testing.T) {
			r, _, errb := fragmentRunner(t, false)

			assert.Equal(t, 2, r.run(context.Background(), args))
			assert.Contains(t, errb.String(), "usage: validate-fragment")
		})
	}
}

func TestUsageListsTheFragmentCommands(t *testing.T) {
	r, out, _ := fragmentRunner(t, false)

	assert.Equal(t, 0, r.run(context.Background(), []string{"help"}))
	assert.Contains(t, out.String(), "boundary")
	assert.Contains(t, out.String(), "validate-fragment")
}
