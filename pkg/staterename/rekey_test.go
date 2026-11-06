package staterename_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-structure/pkg/registry"
	"github.com/truvity/github-structure/pkg/staterename"
)

const (
	org     = "acme"
	oldName = "widget"
	newName = "gadget"
)

// registryDoc is the fixture registry with the renamed row called repo.
// Two more rows carry the old name inside theirs, which is what makes a
// prefix or suffix match on a resource name unsafe.
func registryDoc(repo string) string {
	return `
presets:
  public:
    visibility: public
    has_issues: true
    has_wiki: true
    has_projects: false
    allow_auto_merge: true
    allow_squash_merge: false
    allow_merge_commit: false
    allow_rebase_merge: true
    delete_branch_on_merge: true
    allow_update_branch: false
    allow_forking: true
    has_downloads: false
    default_branch: master
    actions:
      allowed_actions: all
      default_workflow_permissions: write
      can_approve_pull_request_reviews: true
    protection:
      enabled: true
      required_checks: [check]
      strict: false
      enforce_admins: false
      required_approvals: 0
      dismiss_stale_reviews: false
      require_code_owner_reviews: false
      require_conversation_resolution: false
      require_linear_history: false
      require_signatures: false
      allow_force_pushes: false
      allow_deletions: false
access:
  managers:
    management: pull
orgs:
  acme:
    engine_credentials:
      ssm_prefix: /creds/structure-engine/acme
    settings:
      default_repository_permission: none
      members_can_create_repositories: true
      members_can_create_public_repositories: true
      members_can_create_private_repositories: true
      members_can_create_internal_repositories: false
      members_can_create_pages: false
      members_can_create_public_pages: false
      members_can_create_private_pages: false
      members_can_fork_private_repositories: false
      web_commit_signoff_required: false
      has_organization_projects: false
      has_repository_projects: true
      dependabot_alerts_enabled_for_new_repositories: true
      dependabot_security_updates_enabled_for_new_repositories: true
      dependency_graph_enabled_for_new_repositories: true
      advanced_security_enabled_for_new_repositories: false
      secret_scanning_enabled_for_new_repositories: false
      secret_scanning_push_protection_enabled_for_new_repositories: false
      actions:
        allowed_actions: all
        enabled_repositories: all
        sha_pinning_required: false
        default_workflow_permissions: write
        can_approve_pull_request_reviews: true
    teams:
      management:
        privacy: closed
      engineers:
        privacy: closed
        parent: management
    repos:
      ` + repo + `:
        preset: public
        access: [managers]
        teams:
          engineers: push
        tag_rulesets:
          - name: release-tags
            pattern: refs/tags/v*
            bypass_teams: [management]
        branch_rulesets:
          - name: master-check
            pattern: ~DEFAULT_BRANCH
            required_approvals: 1
            required_checks: [check]
            bypass_apps: []
            bypass_org_admins: true
            bypass_teams: [management]
      widget-two:
        preset: public
        access: [managers]
      big-widget:
        preset: public
        access: [managers]
`
}

func loadRegistry(t *testing.T, repo string) *registry.Config {
	t.Helper()

	cfg, err := registry.Load(fstest.MapFS{"github.yaml": {Data: []byte(registryDoc(repo))}})
	require.NoError(t, err)

	return cfg
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()

	raw, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)

	return raw
}

// decode returns the state's resources by URN name.
func decode(t *testing.T, raw []byte) (root map[string]any, byName map[string]map[string]any) {
	t.Helper()

	require.NoError(t, json.Unmarshal(raw, &root))

	dep, ok := root["deployment"].(map[string]any)
	require.True(t, ok, "an export wraps the deployment")

	byName = map[string]map[string]any{}

	for _, r := range dep["resources"].([]any) {
		res := r.(map[string]any)
		urn := res["urn"].(string)
		byName[urn[strings.LastIndex(urn, "::")+2:]] = res
	}

	return root, byName
}

func mustRekey(t *testing.T, state []byte, cfg *registry.Config, from, to string) ([]byte, staterename.Report) {
	t.Helper()

	out, rep, err := staterename.Rekey(state, org, cfg, from, to)
	require.NoError(t, err)

	return out, rep
}

// The golden. The expected state is the same stack as it would be written
// had the repository always been called gadget, with the old name left
// where it is free text: a description, a topic, a homepage, a ref pattern.
func TestRekeyGolden(t *testing.T) {
	cfg := loadRegistry(t, newName)

	out, rep := mustRekey(t, fixture(t, "state-widget.json"), cfg, oldName, newName)

	assert.JSONEq(t, string(fixture(t, "state-gadget.json")), string(out))

	assert.False(t, rep.Noop)
	assert.Equal(t, oldName, rep.From)
	assert.Equal(t, newName, rep.To)
	assert.Len(t, rep.Moved, 8, "repo, two Actions settings, protection, two rulesets, two grants")
	assert.Positive(t, rep.References, "other resources referred to the moved ones")
	assert.Equal(t, 16, rep.Resources)

	for _, m := range rep.Moved {
		assert.Contains(t, m.From, oldName)
		assert.Contains(t, m.To, newName)
	}
}

func TestRekeyLeavesFreeTextAndOtherRepositoriesAlone(t *testing.T) {
	cfg := loadRegistry(t, newName)
	out, _ := mustRekey(t, fixture(t, "state-widget.json"), cfg, oldName, newName)

	_, before := decode(t, fixture(t, "state-widget.json"))
	_, after := decode(t, out)

	repo := after["repo-"+newName]
	require.NotNil(t, repo)

	for _, section := range []string{"inputs", "outputs"} {
		in := repo[section].(map[string]any)
		assert.Equal(t, newName, in["name"], section)
		assert.Equal(t, oldName, in["description"], "%s: free text is not a reference", section)
		assert.Equal(t, []any{oldName, "acme-tool"}, in["topics"], section)
		assert.Equal(t, "https://example.com/"+oldName, in["homepageUrl"], section)
	}

	out2 := repo["outputs"].(map[string]any)
	assert.Equal(t, "acme/"+newName, out2["fullName"])
	assert.Equal(t, "git@github.com:acme/"+newName+".git", out2["sshCloneUrl"])

	tags := after["ruleset-"+newName+"-release-tags"]["inputs"].(map[string]any)
	assert.Equal(t, []any{"refs/tags/v*", "refs/tags/" + oldName},
		tags["conditions"].(map[string]any)["refName"].(map[string]any)["includes"], "a ref pattern is not a repository name")

	for _, other := range []string{"repo-widget-two", "repo-big-widget", "grant-management-widget-two", "grant-management-big-widget"} {
		assert.Equal(t, before[other], after[other], "%s belongs to another repository", other)
	}

	assert.Equal(t, before["team-management"], after["team-management"])
}

func TestRekeyKeepsEverythingElseInTheExport(t *testing.T) {
	cfg := loadRegistry(t, newName)
	out, _ := mustRekey(t, fixture(t, "state-widget.json"), cfg, oldName, newName)

	before, _ := decode(t, fixture(t, "state-widget.json"))
	after, _ := decode(t, out)

	assert.Equal(t, before["version"], after["version"])
	assert.Equal(t, before["deployment"].(map[string]any)["manifest"], after["deployment"].(map[string]any)["manifest"])
	assert.Equal(t, before["deployment"].(map[string]any)["secrets_providers"], after["deployment"].(map[string]any)["secrets_providers"])
}

func TestRekeyRoundTripAndIdempotence(t *testing.T) {
	forward := loadRegistry(t, newName)
	back := loadRegistry(t, oldName)

	original := fixture(t, "state-widget.json")

	moved, _ := mustRekey(t, original, forward, oldName, newName)

	// Swapping from and to is the rollback; the registry is the one the
	// rollback leaves behind.
	restored, rep := mustRekey(t, moved, back, newName, oldName)
	assert.JSONEq(t, string(original), string(restored))
	assert.False(t, rep.Noop)

	// A second run of the same re-key finds nothing to do and changes nothing.
	again, rep := mustRekey(t, moved, forward, oldName, newName)
	assert.True(t, rep.Noop)
	assert.Empty(t, rep.Moved)
	assert.JSONEq(t, string(moved), string(again))
}

func TestRekeyAcceptsABareDeployment(t *testing.T) {
	cfg := loadRegistry(t, newName)

	var root map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(fixture(t, "state-widget.json"), &root))

	out, _ := mustRekey(t, root["deployment"], cfg, oldName, newName)

	var want map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(fixture(t, "state-gadget.json"), &want))

	assert.JSONEq(t, string(want["deployment"]), string(out))
}

// The engine declares the workflow permissions only once the repository has
// commits, so a state without them is a state this row can legitimately have.
func TestRekeyToleratesAbsentWorkflowPermissions(t *testing.T) {
	cfg := loadRegistry(t, newName)
	state := dropResource(t, fixture(t, "state-widget.json"), "workflow-perms-"+oldName)

	out, rep := mustRekey(t, state, cfg, oldName, newName)
	assert.Len(t, rep.Moved, 7)

	_, byName := decode(t, out)
	assert.NotContains(t, byName, "workflow-perms-"+newName)
	assert.Contains(t, byName, "repo-"+newName)
}

func TestRekeyRefuses(t *testing.T) {
	cfg := loadRegistry(t, newName)
	good := fixture(t, "state-widget.json")

	cases := []struct {
		name    string
		state   []byte
		cfg     *registry.Config
		org     string
		from    string
		to      string
		message string
	}{
		{"target already in the state", cloneResource(t, good, "repo-"+oldName, "repo-"+newName), cfg, org, oldName, newName, "already holds"},
		{"target partly in the state", cloneResource(t, good, "grant-management-"+oldName, "grant-management-"+newName), cfg, org, oldName, newName, "already holds"},
		{"protection missing", dropResource(t, good, "protection-"+oldName), cfg, org, oldName, newName, "missing"},
		{"a grant missing", dropResource(t, good, "grant-engineers-"+oldName), cfg, org, oldName, newName, "missing"},
		{
			"a ruleset the row does not declare",
			cloneResource(t, good, "ruleset-"+oldName+"-master-check", "ruleset-"+oldName+"-stray"),
			cfg, org, oldName, newName, "unexpected",
		},
		{"the same resource twice", cloneResource(t, good, "repo-"+oldName, "repo-"+oldName), cfg, org, oldName, newName, "twice"},
		{"nothing carries the old name", dropAll(t, good, oldName), cfg, org, oldName, newName, "no resource"},
		{"from equals to", good, cfg, org, newName, newName, "same"},
		{"empty from", good, cfg, org, "", newName, "empty"},
		{"empty to", good, cfg, org, oldName, "", "empty"},
		{"to is not a row", good, cfg, org, oldName, "nowhere", "no row"},
		{"from is still a row", good, loadRegistry(t, oldName), org, oldName, "widget-two", "still"},
		{"unknown org", good, cfg, "globex", oldName, newName, "org"},
		{"nil registry", good, nil, org, oldName, newName, "registry"},
		{"not json", []byte("not json"), cfg, org, oldName, newName, "state"},
		{"no resources", []byte(`{"version":3,"deployment":{}}`), cfg, org, oldName, newName, "resources"},
		{"a pending operation on a moved resource", pendingOn(t, good, "repo-"+oldName), cfg, org, oldName, newName, "pending"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := staterename.Rekey(tc.state, tc.org, tc.cfg, tc.from, tc.to)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.message)
			assert.Nil(t, out, "a refusal returns no state")
		})
	}
}

// A refusal names everything that is wrong at once, so an operator fixes it
// in one pass.
func TestRekeyRefusalAggregates(t *testing.T) {
	cfg := loadRegistry(t, newName)
	state := dropResource(t, dropResource(t, fixture(t, "state-widget.json"), "protection-"+oldName), "grant-engineers-"+oldName)

	_, _, err := staterename.Rekey(state, org, cfg, oldName, newName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "protection-"+oldName)
	assert.Contains(t, err.Error(), "grant-engineers-"+oldName)
}

func TestResourceNames(t *testing.T) {
	required, optional := staterename.ResourceNames(loadRegistry(t, newName), org, newName)

	assert.Equal(t, []string{
		"actions-gadget", "grant-engineers-gadget", "grant-management-gadget", "protection-gadget",
		"repo-gadget", "ruleset-gadget-master-check", "ruleset-gadget-release-tags",
	}, required)
	assert.Equal(t, []string{"workflow-perms-gadget"}, optional)

	required, optional = staterename.ResourceNames(loadRegistry(t, newName), org, "nowhere")
	assert.Empty(t, required)
	assert.Empty(t, optional)
}

// ---- state surgery for the refusal cases ----

func resources(t *testing.T, raw []byte) (map[string]any, []any) {
	t.Helper()

	var root map[string]any
	require.NoError(t, json.Unmarshal(raw, &root))

	dep := root["deployment"].(map[string]any)

	return root, dep["resources"].([]any)
}

func withResources(t *testing.T, root map[string]any, rs []any) []byte {
	t.Helper()

	root["deployment"].(map[string]any)["resources"] = rs

	out, err := json.Marshal(root)
	require.NoError(t, err)

	return out
}

func nameOf(r any) string {
	urn := r.(map[string]any)["urn"].(string)

	return urn[strings.LastIndex(urn, "::")+2:]
}

func dropResource(t *testing.T, raw []byte, name string) []byte {
	t.Helper()

	root, rs := resources(t, raw)

	out := make([]any, 0, len(rs))

	for _, r := range rs {
		if nameOf(r) != name {
			out = append(out, r)
		}
	}

	require.Len(t, out, len(rs)-1, "fixture has %s", name)

	return withResources(t, root, out)
}

func dropAll(t *testing.T, raw []byte, carrying string) []byte {
	t.Helper()

	root, rs := resources(t, raw)

	out := make([]any, 0, len(rs))

	for _, r := range rs {
		n := nameOf(r)
		if n == "repo-"+carrying || strings.HasSuffix(n, "-"+carrying) && !strings.Contains(n, "-two") && !strings.Contains(n, "big-") ||
			strings.HasPrefix(n, "ruleset-"+carrying+"-") {
			continue
		}

		out = append(out, r)
	}

	return withResources(t, root, out)
}

// cloneResource appends a copy of resource src under the name dst.
func cloneResource(t *testing.T, raw []byte, src, dst string) []byte {
	t.Helper()

	root, rs := resources(t, raw)

	for _, r := range rs {
		if nameOf(r) != src {
			continue
		}

		cp := map[string]any{}
		for k, v := range r.(map[string]any) {
			cp[k] = v
		}

		urn := cp["urn"].(string)
		cp["urn"] = urn[:strings.LastIndex(urn, "::")+2] + dst

		return withResources(t, root, append(rs, cp))
	}

	require.Fail(t, "fixture has no "+src)

	return nil
}

func pendingOn(t *testing.T, raw []byte, name string) []byte {
	t.Helper()

	root, rs := resources(t, raw)

	for _, r := range rs {
		if nameOf(r) == name {
			root["deployment"].(map[string]any)["pending_operations"] = []any{
				map[string]any{"type": "updating", "resource": r},
			}

			out, err := json.Marshal(root)
			require.NoError(t, err)

			return out
		}
	}

	require.Fail(t, "fixture has no "+name)

	return nil
}
