package registry_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	registry "github.com/truvity/github-structure/pkg/registry"
)

const (
	// minimal is a valid registry used as the base for negative cases:
	// each test mutates one thing and asserts the loader rejects it.
	minimal = `
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
      widget:
        preset: public
        access: [managers]
`
)

func load(t *testing.T, body string) (*registry.Config, error) {
	t.Helper()

	return registry.Load(fstest.MapFS{
		"github.yaml": &fstest.MapFile{Data: []byte(body)},
	})
}

func TestLoadMinimal(t *testing.T) {
	c, err := load(t, minimal)
	require.NoError(t, err)

	assert.Equal(t, []string{"acme"}, c.SortedOrgs())
	assert.Equal(t, []string{"engineers", "management"}, c.Orgs["acme"].SortedTeams())
}

// ── the engine's own credentials ───────────────────────────────────────

const (
	// ssmSource is the minimal registry's engine credential source; the
	// cases below swap it for another shape.
	ssmSource = `    engine_credentials:
      ssm_prefix: /creds/structure-engine/acme
`
)

func TestEngineCredentialsAcceptOpenBAO(t *testing.T) {
	body := strings.Replace(minimal, ssmSource, `    engine_credentials:
      openbao:
        namespace: management
        mount: kv
        path: github-apps/acme-iac
`, 1)

	c, err := load(t, body)
	require.NoError(t, err)

	creds := c.Orgs["acme"].EngineCredentials
	require.NotNil(t, creds.OpenBAO)
	assert.Equal(t, "management/kv/github-apps/acme-iac", creds.OpenBAO.String())
	assert.Equal(t, "openbao management/kv/github-apps/acme-iac", creds.Describe())
}

func TestEngineCredentialsDescribeSSM(t *testing.T) {
	c, err := load(t, minimal)
	require.NoError(t, err)

	assert.Equal(t, "ssm /creds/structure-engine/acme", c.Orgs["acme"].EngineCredentials.Describe())
}

func TestEngineCredentialsAreRequired(t *testing.T) {
	body := strings.Replace(minimal, ssmSource, "", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "engine_credentials is required")
}

func TestEngineCredentialsRejectAnEmptySource(t *testing.T) {
	body := strings.Replace(minimal, ssmSource, "    engine_credentials: {}\n", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "names no source")
}

func TestEngineCredentialsRejectTwoSources(t *testing.T) {
	body := strings.Replace(minimal, ssmSource, `    engine_credentials:
      ssm_prefix: /creds/structure-engine/acme
      openbao:
        mount: kv
        path: github-apps/acme-iac
`, 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one")
}

func TestEngineCredentialsOpenBAONeedsMountAndPath(t *testing.T) {
	for _, tc := range []struct{ name, block, want string }{
		{"no mount", "        path: github-apps/acme-iac\n", "mount is required"},
		{"no path", "        mount: kv\n", "path is required"},
		{"the API path", "        mount: kv\n        path: kv/data/github-apps/acme-iac\n", "`data/` segment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(minimal, ssmSource, "    engine_credentials:\n      openbao:\n"+tc.block, 1)

			_, err := load(t, body)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	_, err := load(t, minimal+"\nnonsense: true\n")
	require.Error(t, err)
}

// ── profile completeness ───────────────────────────────────────────────

func TestProfileMustBeComplete(t *testing.T) {
	body := strings.Replace(minimal, "    has_wiki: true\n", "", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has_wiki")
	assert.Contains(t, err.Error(), "incomplete")
}

func TestProfileRejectsBadVisibility(t *testing.T) {
	body := strings.Replace(minimal, "visibility: public", "visibility: secret", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "visibility")
}

func TestProfileRejectsBadWorkflowPermissions(t *testing.T) {
	body := strings.Replace(minimal, "default_workflow_permissions: write", "default_workflow_permissions: rw", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "default_workflow_permissions")
}

// ── repo rows ──────────────────────────────────────────────────────────

func TestRepoRejectsUnknownProfile(t *testing.T) {
	body := strings.Replace(minimal, "        preset: public", "        preset: internal", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown preset")
}

func TestRepoRejectsGrantToUnknownTeam(t *testing.T) {
	body := minimal + `        teams:
          ghosts: push
`
	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown team")
}

func TestRepoRejectsBadTeamPermission(t *testing.T) {
	body := minimal + `        teams:
          engineers: superuser
`
	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission")
}

func TestOverridesRequireAReason(t *testing.T) {
	body := minimal + `        overrides:
          has_wiki: false
`
	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reason")
}

// Grants cannot ride on a settings override. This used to need its own
// validation rule; now the schema simply has nowhere to put them, which
// is the stronger form — a preset cannot carry a grant at all.
func TestOverridesRejectTeams(t *testing.T) {
	body := minimal + `        reason: testing
        overrides:
          teams:
            engineers: push
`
	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "field teams not found")
}

// A rule that enforces nothing at all is indistinguishable from no
// protection, but much harder to notice.
func TestProtectionMustEnforceSomething(t *testing.T) {
	body := strings.Replace(minimal, "      required_checks: [check]", "      required_checks: []", 1)
	body = strings.Replace(body, "      allow_force_pushes: false", "      allow_force_pushes: true", 1)
	body = strings.Replace(body, "      allow_deletions: false", "      allow_deletions: true", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enforces nothing")
}

// But blocking force pushes IS enforcement, even with nothing gating
// merges — the shape a repo takes while its checks are waived.
func TestProtectionWithOnlyForcePushBlockIsValid(t *testing.T) {
	body := strings.Replace(minimal, "      required_checks: [check]", "      required_checks: []", 1)

	_, err := load(t, body)
	require.NoError(t, err)
}

func TestProtectionDisabledIsFine(t *testing.T) {
	body := strings.Replace(minimal, "      required_checks: [check]", "      required_checks: []", 1)
	body = strings.Replace(body, "      enabled: true", "      enabled: false", 1)

	_, err := load(t, body)
	require.NoError(t, err)
}

// ── branch rulesets ────────────────────────────────────────────────────

// A ruleset must carry SOME bypass — apps or org admins — or the gate
// belongs in classic protection. bypass_org_admins alone is a valid
// bypass (D12: the audit-visible admin escape hatch on ci-workflows).
func TestBranchRulesetAcceptsOrgAdminOnlyBypass(t *testing.T) {
	body := strings.Replace(minimal, "        preset: public\n", `        preset: public
        branch_rulesets:
          - name: pr-approval
            pattern: ~DEFAULT_BRANCH
            required_approvals: 1
            bypass_org_admins: true
`, 1)

	_, err := load(t, body)
	require.NoError(t, err)
}

// A checks-only ruleset (required_approvals 0) is valid when it requires
// status checks and has a bypass — the shape that lets an App direct-push
// past CI while everyone else stays gated.
func TestBranchRulesetAcceptsChecksOnlyWithBypass(t *testing.T) {
	body := strings.Replace(minimal, "        preset: public\n", `        preset: public
        branch_rulesets:
          - name: master-check
            pattern: ~DEFAULT_BRANCH
            required_approvals: 0
            required_checks: [check]
            bypass_apps: [acme-releases]
`, 1)

	_, err := load(t, body)
	require.NoError(t, err)
}

// ── bypass App names ───────────────────────────────────────────────────

// A ruleset names an App; the id comes from the organization's LIVE
// installations. The registry never states the id, because an id copied
// into a file is stale the moment the App is recreated and says nothing
// when it is.
func TestBypassAppNameResolvesAgainstLiveInstallations(t *testing.T) {
	body := strings.Replace(minimal, "        preset: public\n", `        preset: public
        branch_rulesets:
          - name: master-check
            pattern: ~DEFAULT_BRANCH
            required_approvals: 0
            required_checks: [check]
            bypass_apps: [acme-releases]
        tag_rulesets:
          - name: release-tags
            pattern: refs/tags/v*
            bypass_teams: [engineers]
            bypass_apps: [acme-releases]
`, 1)

	c, err := load(t, body)
	require.NoError(t, err)

	repo := c.Orgs["acme"].Repos["widget"]
	installed := registry.InstalledApps{"acme-releases": 12345, "unrelated": 999}

	branchIDs, err := installed.BypassAppIDs(repo.BranchRulesets[0].BypassApps)
	require.NoError(t, err)
	assert.Equal(t, []int{12345}, branchIDs)

	tagIDs, err := installed.BypassAppIDs(repo.TagRulesets[0].BypassApps)
	require.NoError(t, err)
	assert.Equal(t, []int{12345}, tagIDs)
}

// Whether an App is installed is live GitHub's answer, and the load path
// has no credentials to ask with — so a name loads, and the DEPLOY that
// cannot resolve it stops, naming the App. A ruleset rendered with one
// actor missing would be worse than a failed deploy: the gate stays, the
// bypass does not, and nothing says so until the act it permits is
// refused.
func TestBypassAppNotInstalledFailsAtDeploy(t *testing.T) {
	body := strings.Replace(minimal, "        preset: public\n", `        preset: public
        branch_rulesets:
          - name: master-check
            pattern: ~DEFAULT_BRANCH
            required_approvals: 0
            required_checks: [check]
            bypass_apps: [acme-typo]
`, 1)

	c, err := load(t, body)
	require.NoError(t, err, "the loader cannot know what is installed")

	names := c.Orgs["acme"].Repos["widget"].BranchRulesets[0].BypassApps

	_, err = registry.InstalledApps{"acme-releases": 12345}.BypassAppIDs(names)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `bypass app "acme-typo"`)
	assert.Contains(t, err.Error(), "not installed on this organization")
	assert.Contains(t, err.Error(), "would be dropped, not defaulted")
}

// The same refusal with nothing installed at all — the shape a fresh
// organization has, where "resolve to nothing" is the tempting default.
func TestBypassAppRefusesEmptyInstallations(t *testing.T) {
	_, err := registry.InstalledApps{}.BypassAppIDs([]string{"acme-releases"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `bypass app "acme-releases"`)
}

// Nothing to resolve resolves to nothing, without asking anyone.
func TestBypassAppIDsEmptyNames(t *testing.T) {
	ids, err := registry.InstalledApps(nil).BypassAppIDs(nil)
	require.NoError(t, err)
	assert.Empty(t, ids)
}

// Database ids are no longer a spelling this field accepts. Keeping both
// forms would keep the habit that made the ids stale in the first place,
// so the transition is one-way and the loader says so in type terms —
// this is the half of the check that needs no network, so it stays at
// LOAD where a typo is cheapest to find.
func TestBypassAppRejectsDatabaseID(t *testing.T) {
	body := strings.Replace(minimal, "        preset: public\n", `        preset: public
        branch_rulesets:
          - name: master-check
            pattern: ~DEFAULT_BRANCH
            required_approvals: 0
            required_checks: [check]
            bypass_apps: [12345]
`, 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `branch ruleset "master-check"`)
	assert.Contains(t, err.Error(), "looks like a GitHub App database id")
}

// Tag rulesets speak the same vocabulary and refuse the same way: the
// tag namespace is where a bad actor reference bites (a refused release
// tag), so its error must name the ruleset too.
func TestBypassAppRejectsDatabaseIDOnTagRuleset(t *testing.T) {
	body := strings.Replace(minimal, "        preset: public\n", `        preset: public
        tag_rulesets:
          - name: release-tags
            pattern: refs/tags/v*
            bypass_teams: [engineers]
            bypass_apps: [12345]
`, 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `tag ruleset "release-tags"`)
	assert.Contains(t, err.Error(), "looks like a GitHub App database id")
}

// An `apps:` block does not load. It is refused rather than ignored, and
// the refusal says where Apps went: a silently dropped inventory would
// read as "still managed here" to the next person to open the file.
func TestLoadRejectsRetiredAppsBlock(t *testing.T) {
	body := strings.Replace(minimal, "    teams:\n", `    apps:
      acme-releases:
        external: true
        app_id: 12345
        install: all
    teams:
`, 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `org acme`)
	assert.Contains(t, err.Error(), "`apps:`")
	assert.Contains(t, err.Error(), "credential custodian")
	assert.Contains(t, err.Error(), "LIVE installations")
}

// app_prefix went with it: its only job was checking the names of App
// rows this file no longer carries.
func TestLoadRejectsRetiredAppPrefix(t *testing.T) {
	body := strings.Replace(minimal, "    teams:\n", "    app_prefix: acme-\n    teams:\n", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "app_prefix")
}

// A ruleset that enforces neither approvals nor checks is noise.
func TestBranchRulesetRejectsEnforcingNothing(t *testing.T) {
	body := strings.Replace(minimal, "        preset: public\n", `        preset: public
        branch_rulesets:
          - name: empty
            pattern: ~DEFAULT_BRANCH
            required_approvals: 0
            bypass_org_admins: true
`, 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enforcing nothing is noise")
}

func TestBranchRulesetRejectsNoBypassAtAll(t *testing.T) {
	body := strings.Replace(minimal, "        preset: public\n", `        preset: public
        branch_rulesets:
          - name: pr-approval
            pattern: ~DEFAULT_BRANCH
            required_approvals: 1
`, 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a bypass is required")
}

// ── check waivers (INF-410) ────────────────────────────────────────────

// The profile keeps stating the intent; the waiver drops it for one
// repo. That is the whole point — the exception stays visible.
func TestChecksWaivedDropsProfileChecks(t *testing.T) {
	body := minimal + `        checks_waived: CI has not landed yet
`
	c, err := load(t, body)
	require.NoError(t, err)

	got, ok := c.ResolveRepo("acme", "widget")
	require.True(t, ok)

	assert.Empty(t, got.Protection.RequiredChecks, "the waiver must clear the preset's checks")
	assert.True(t, got.Protection.Enabled, "protection itself stays on")

	assert.Equal(t, map[string]string{"widget": "CI has not landed yet"}, c.Orgs["acme"].ChecksWaived())
}

// A waiver whose profile requires nothing has outlived its cause, and
// would otherwise sit there forever implying an exception that is not
// one.
func TestChecksWaivedRejectedWhenProfileRequiresNothing(t *testing.T) {
	body := strings.Replace(minimal, "      required_checks: [check]", "      required_checks: []", 1)
	body = strings.Replace(body, "      enforce_admins: false", "      enforce_admins: true", 1)
	body += `        checks_waived: stale
`
	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outlived its cause")
}

func TestChecksWaivedEmptyByDefault(t *testing.T) {
	c, err := load(t, minimal)
	require.NoError(t, err)
	assert.Empty(t, c.Orgs["acme"].ChecksWaived())
}

// ── archival ───────────────────────────────────────────────────────────

// A retired repo keeps its row and its profile; only `archived` changes.
// Everything else must still resolve, because clearing the flag is the
// whole of an unarchive — no other edit should be needed to get the repo
// back under full management.
func TestResolveCarriesArchived(t *testing.T) {
	body := minimal + `        archived: true
`
	c, err := load(t, body)
	require.NoError(t, err)

	got, ok := c.ResolveRepo("acme", "widget")
	require.True(t, ok)

	assert.True(t, got.Archived)

	// The profile still resolves underneath the flag.
	assert.Equal(t, "public", got.Visibility)
	assert.True(t, got.HasIssues)
	assert.Equal(t, "all", got.Actions.AllowedActions)
}

func TestRepoDefaultsToNotArchived(t *testing.T) {
	c, err := load(t, minimal)
	require.NoError(t, err)

	got, ok := c.ResolveRepo("acme", "widget")
	require.True(t, ok)

	assert.False(t, got.Archived, "a row without the flag must not be treated as retired")
}

// The `public` profile in `minimal` enables protection with a required
// check. On a live repo that is fine; on an archived one the engine
// declares no protection at all, so the enforces-anything check must not
// judge a rule that will never exist. Guards a loader that would reject
// every archived repo on a protection-enabled profile.
func TestArchivedRepoOnProtectedProfileLoads(t *testing.T) {
	body := minimal + `        archived: true
`
	_, err := load(t, body)
	require.NoError(t, err)
}

// Inert config is worse than rejected config: the row reads as if it
// still shapes something. Both of these only shape resources an archived
// repo no longer has.
func TestArchivedRepoRejectsOverrides(t *testing.T) {
	body := minimal + `        archived: true
        reason: maintained fork
        overrides:
          has_issues: false
`
	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "archived, so overrides do nothing")
}

func TestArchivedRepoRejectsChecksWaived(t *testing.T) {
	body := minimal + `        archived: true
        checks_waived: CI has not landed
`
	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "archived, so checks_waived does nothing")
}

// ── override resolution ────────────────────────────────────────────────

func TestResolveAppliesOverrides(t *testing.T) {
	body := minimal + `        reason: maintained fork
        overrides:
          has_issues: false
          allow_squash_merge: true
          protection:
            required_checks: [check, trivy]
`
	c, err := load(t, body)
	require.NoError(t, err)

	got, ok := c.ResolveRepo("acme", "widget")
	require.True(t, ok)

	// Overridden.
	assert.False(t, got.HasIssues)
	assert.True(t, got.AllowSquashMerge)
	assert.Equal(t, []string{"check", "trivy"}, got.Protection.RequiredChecks)

	// Inherited from the profile.
	assert.True(t, got.HasWiki)
	assert.True(t, got.AllowRebaseMerge)
	assert.True(t, got.Protection.Enabled)
	assert.False(t, got.Protection.Strict)
	assert.Equal(t, "master", got.DefaultBranch)
	assert.Equal(t, "all", got.Actions.AllowedActions)
}

// The bug this guards: overlay() mutating the shared profile, so the
// first repo with an override silently changes every later repo.
func TestResolveDoesNotMutateTheProfile(t *testing.T) {
	body := minimal + `        reason: testing
        overrides:
          has_issues: false
          protection:
            enabled: false
      other:
        preset: public
`
	c, err := load(t, body)
	require.NoError(t, err)

	first, ok := c.ResolveRepo("acme", "widget")
	require.True(t, ok)
	require.False(t, first.HasIssues)
	require.False(t, first.Protection.Enabled)

	second, ok := c.ResolveRepo("acme", "other")
	require.True(t, ok)
	assert.True(t, second.HasIssues, "preset leaked an override from another repo")
	assert.True(t, second.Protection.Enabled, "profile's nested protection leaked an override")
}

func TestResolveMergesTeamGrants(t *testing.T) {
	body := minimal + `        teams:
          engineers: push
          management: admin
`
	c, err := load(t, body)
	require.NoError(t, err)

	got, ok := c.ResolveRepo("acme", "widget")
	require.True(t, ok)

	// Profile grant kept, repo grant added, repo grant wins on conflict.
	assert.Equal(t, map[string]string{"engineers": "push", "management": "admin"}, got.Teams)
}

// ── teams ──────────────────────────────────────────────────────────────

func TestTeamRejectsUnknownParent(t *testing.T) {
	body := strings.Replace(minimal, "        parent: management", "        parent: nowhere", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown parent")
}

func TestTeamRejectsCycle(t *testing.T) {
	body := strings.Replace(minimal,
		"      management:\n        privacy: closed\n",
		"      management:\n        privacy: closed\n        parent: engineers\n", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cycle")
}

func TestNestedTeamCannotBeSecret(t *testing.T) {
	body := strings.Replace(minimal,
		"      engineers:\n        privacy: closed\n",
		"      engineers:\n        privacy: secret\n", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "secret")
}

// ── runner groups ──────────────────────────────────────────────────────

func TestRunnerGroupRejectsReposWhenNotSelected(t *testing.T) {
	body := minimal + `
    runner_groups:
      arc:
        visibility: all
        repos: [widget]
`
	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "visibility: selected")
}

func TestRunnerGroupRejectsUnknownRepo(t *testing.T) {
	body := minimal + `
    runner_groups:
      arc:
        visibility: selected
        repos: [ghost]
`
	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an in-scope row")
}

// ── the real registry ──────────────────────────────────────────────────

// The committed cfg/github.yaml must always load. This is the test that
// fails when someone adds a row by hand and gets it subtly wrong.
func TestLoadAcceptsOwners(t *testing.T) {
	c, err := load(t, minimal+"    owners:\n      - excavador\n      - a-ignatov-parc\n")
	require.NoError(t, err)

	assert.Equal(t, []string{"excavador", "a-ignatov-parc"}, c.Orgs["acme"].Owners)
}

func TestOwnersAreOptional(t *testing.T) {
	// Most orgs have not adopted the field; absence must stay valid.
	c, err := load(t, minimal)
	require.NoError(t, err)

	assert.Empty(t, c.Orgs["acme"].Owners)
}

func TestOwnersRejectDuplicate(t *testing.T) {
	// Case-insensitively duplicate: GitHub treats these as one login, so
	// the engine would declare two resources for a single membership.
	_, err := load(t, minimal+"    owners:\n      - excavador\n      - Excavador\n")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "listed twice")
}

func TestOwnersRejectMalformedLogin(t *testing.T) {
	_, err := load(t, minimal+"    owners:\n      - \"not a login\"\n")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid GitHub login")
}

// ── access bundles ─────────────────────────────────────────────────────

func TestAccessBundleResolvesIntoTeams(t *testing.T) {
	c, err := load(t, minimal)
	require.NoError(t, err)

	got := c.Resolve(c.Orgs["acme"].Repos["widget"])
	assert.Equal(t, map[string]string{"management": "pull"}, got.Teams,
		"a listed bundle must contribute its grants")
}

// The repo row is the last word, so a row can upgrade what a bundle gives
// without needing a bespoke bundle for one repository.
func TestRepoTeamsOverrideAccessBundle(t *testing.T) {
	body := minimal + `        teams:
            management: push
`
	c, err := load(t, body)
	require.NoError(t, err)

	got := c.Resolve(c.Orgs["acme"].Repos["widget"])
	assert.Equal(t, "push", got.Teams["management"])
}

func TestRejectsUnknownAccessBundle(t *testing.T) {
	body := strings.Replace(minimal, "access: [managers]", "access: [nobody]", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown access bundle")
}

func TestAccessBundleCannotGrantToUnknownTeam(t *testing.T) {
	body := strings.Replace(minimal, "    management: pull", "    ghosts: pull", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "which this org does not have")
}

// The regression test for why this split exists. Two repositories on the
// SAME preset must be able to have different grants — before, a repo's
// access rode on the settings profile it named, so making two repos share
// a protection policy silently made them share an audience. That is how a
// board repository ends up readable by marketing.
func TestSamePresetCanCarryDifferentAccess(t *testing.T) {
	body := strings.Replace(minimal, `      widget:
        preset: public
        access: [managers]`, `      widget:
        preset: public
        access: [managers]
      secret:
        preset: public`, 1)

	c, err := load(t, body)
	require.NoError(t, err)

	open := c.Resolve(c.Orgs["acme"].Repos["widget"])
	shut := c.Resolve(c.Orgs["acme"].Repos["secret"])

	assert.Equal(t, map[string]string{"management": "pull"}, open.Teams)
	assert.Empty(t, shut.Teams,
		"same preset, no bundle listed: the grant must NOT be inherited")
	assert.Equal(t, open.Visibility, shut.Visibility,
		"and the settings must still be shared — that is the point of the preset")
}
