package registry

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const settingsBlock = `
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
        variables:
          RUNNER_LABELS:
            value: fleet
`

// providerYAML is the provider's file for the fixture org. With inline set
// it also holds the rows a company would otherwise bring as a fragment, so
// the two shapes can be compared.
func providerYAML(inline bool) string {
	extraTeam, extraRepo, extraVar, waiver := "", "", "", ""

	if inline {
		extraTeam = "      squad:\n        privacy: closed\n"
		extraRepo = `      gadget:
        preset: private
        access: [devs]
        teams:
          squad: push
        tag_rulesets:
          - name: release
            pattern: refs/tags/v*
            bypass_teams: [squad]
            bypass_apps: [release-bot]
`
		extraVar = "          GLOBEX_REGION:\n            value: eu\n"
		waiver = "    checks_waived:\n      no CI yet: [gadget]\n"
	}

	settings := settingsBlock + extraVar

	return `
presets:
  private:
    visibility: private
    protection:
      enabled: true
      required_checks: [check]
  public:
    visibility: public
    protection:
      enabled: true
      required_checks: [check]
  restricted:
    visibility: private
    protection:
      enabled: true
      required_checks: [check]
access:
  managers:
    management: pull
  devs:
    squad: push
orgs:
  acme:
    engine_credentials:
      ssm_prefix: /creds/structure-engine/acme
    owners: [root-admin]
    fragment_ceiling:
      presets: [private, public]
      access: [devs]
      bypass_apps: [release-bot]
      max_team_permission: push
      public_repos: false
      reserved:
        repos: [reserved-repo]
        teams: [reserved-team]
        variables: [RESERVED_VAR]
` + settings + waiver + `    teams:
      management:
        privacy: closed
` + extraTeam + `    repos:
      infra:
        preset: private
        access: [managers]
` + extraRepo + `  initech:
    engine_credentials:
      ssm_prefix: /creds/structure-engine/initech
` + settingsBlock + `    teams:
      staff:
        privacy: closed
    repos:
      tools:
        preset: private
`
}

func providerFS(inline bool) fstest.MapFS {
	return fstest.MapFS{"github.yaml": {Data: []byte(providerYAML(inline))}}
}

// goodFragment is a fragment inside the ceiling. A fresh one per call:
// Compose moves its rows into the config.
func goodFragment() Fragment {
	return Fragment{
		Org:   "acme",
		Owner: "globex",
		OrgFragment: OrgFragment{
			Teams: map[string]*Team{"squad": {Privacy: "closed"}},
			Repos: map[string]*Repo{"gadget": {
				Preset: "private",
				Access: []string{"devs"},
				Teams:  map[string]string{"squad": "push"},
				TagRulesets: []*TagRuleset{{
					Name: "release", Pattern: "refs/tags/v*",
					BypassTeams: []string{"squad"}, BypassApps: []string{"release-bot"},
				}},
			}},
			Variables: map[string]*OrgVariable{"GLOBEX_REGION": {Value: "eu"}},
			Waivers:   map[string][]string{"no CI yet": {"gadget"}},
		},
	}
}

// Without fragments the provider's file is the config, field for field.
func TestComposeWithoutFragmentsIsLoad(t *testing.T) {
	for _, inline := range []bool{false, true} {
		want, err := Load(providerFS(inline))
		require.NoError(t, err)

		got, err := Compose(providerFS(inline))
		require.NoError(t, err)

		assert.Equal(t, want, got)

		got, err = Compose(providerFS(inline), []Fragment{}...)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
}

// Moving rows from the provider's file into a fragment changes who wrote
// them and nothing about what they resolve to: the engine, the drift
// check and the preflight read the same effective rows.
func TestComposeFragmentResolvesLikeTheRowsInline(t *testing.T) {
	inline, err := Load(providerFS(true))
	require.NoError(t, err)

	composed, err := Compose(providerFS(false), goodFragment())
	require.NoError(t, err)

	for _, org := range []string{"acme", "initech"} {
		a, b := inline.Orgs[org], composed.Orgs[org]

		assert.Equal(t, a.SortedRepos(), b.SortedRepos(), org)
		assert.Equal(t, a.SortedTeams(), b.SortedTeams(), org)
		assert.Equal(t, a.ChecksWaived(), b.ChecksWaived(), org)

		for _, name := range a.SortedRepos() {
			want := inline.Resolve(a.Repos[name])
			got := composed.Resolve(b.Repos[name])
			assert.Equal(t, want, got, "%s/%s", org, name)

			wantJSON, err := json.Marshal(want)
			require.NoError(t, err)

			gotJSON, err := json.Marshal(got)
			require.NoError(t, err)
			assert.Equal(t, string(wantJSON), string(gotJSON), "%s/%s resolved output is byte-identical", org, name)
		}
	}

	assert.Equal(t, inline.Orgs["acme"].Settings.Actions.Variables["GLOBEX_REGION"].Value,
		composed.Orgs["acme"].Settings.Actions.Variables["GLOBEX_REGION"].Value)
	assert.Equal(t, inline.Orgs["acme"].Teams["squad"].Privacy, composed.Orgs["acme"].Teams["squad"].Privacy)
}

// Every row says who wrote it; the provider's own are empty.
func TestComposeMarksTheOwnerOfEachRow(t *testing.T) {
	cfg, err := Compose(providerFS(false), goodFragment())
	require.NoError(t, err)

	acme := cfg.Orgs["acme"]

	assert.Equal(t, "globex", acme.Repos["gadget"].Owner)
	assert.Equal(t, "globex", acme.Teams["squad"].Owner)
	assert.Equal(t, "globex", acme.Settings.Actions.Variables["GLOBEX_REGION"].Owner)

	assert.Empty(t, acme.Repos["infra"].Owner, "the provider's rows stay as written")
	assert.Empty(t, acme.Teams["management"].Owner)
	assert.Empty(t, acme.Settings.Actions.Variables["RUNNER_LABELS"].Owner)

	assert.Equal(t, "no CI yet", acme.Repos["gadget"].ChecksWaived, "a fragment's waiver lands on its row")
}

type refusal struct {
	name string
	edit func(f *Fragment)
	want []string
}

func refusals() []refusal {
	return []refusal{
		{"repo the provider declares", func(f *Fragment) { f.Repos["infra"] = &Repo{Preset: "private"} }, []string{"repos.infra", "provider-owned"}},
		{
			"repo the provider reserves",
			func(f *Fragment) { f.Repos["reserved-repo"] = &Repo{Preset: "private"} },
			[]string{"repos.reserved-repo", "provider-owned (reserved)"},
		},
		{"team the provider declares", func(f *Fragment) { f.Teams["management"] = &Team{} }, []string{"teams.management", "provider-owned"}},
		{"team the provider reserves", func(f *Fragment) { f.Teams["reserved-team"] = &Team{} }, []string{"teams.reserved-team", "provider-owned (reserved)"}},
		{
			"variable the provider declares",
			func(f *Fragment) { f.Variables["RUNNER_LABELS"] = &OrgVariable{Value: "x"} },
			[]string{"variables.RUNNER_LABELS", "provider-owned"},
		},
		{
			"variable the provider reserves",
			func(f *Fragment) { f.Variables["RESERVED_VAR"] = &OrgVariable{Value: "x"} },
			[]string{"variables.RESERVED_VAR", "provider-owned (reserved)"},
		},
		{
			"repo the provider declares, in another case",
			func(f *Fragment) { f.Repos["Infra"] = &Repo{Preset: "private"} },
			[]string{"repos.Infra", "provider-owned"},
		},
		{
			"repo the provider reserves, in another case",
			func(f *Fragment) { f.Repos["Reserved-Repo"] = &Repo{Preset: "private"} },
			[]string{"repos.Reserved-Repo", "provider-owned (reserved)"},
		},
		{
			"team the provider declares, in another case",
			func(f *Fragment) { f.Teams["Management"] = &Team{} },
			[]string{"teams.Management", "provider-owned"},
		},
		{
			"team the provider reserves, in another case",
			func(f *Fragment) { f.Teams["Reserved-Team"] = &Team{} },
			[]string{"teams.Reserved-Team", "provider-owned (reserved)"},
		},
		{
			"variable the provider declares, in another case",
			func(f *Fragment) { f.Variables["runner_labels"] = &OrgVariable{Value: "x"} },
			[]string{"variables.runner_labels", "provider-owned"},
		},
		{
			"variable the provider reserves, in another case",
			func(f *Fragment) { f.Variables["reserved_var"] = &OrgVariable{Value: "x"} },
			[]string{"variables.reserved_var", "provider-owned (reserved)"},
		},
		{
			"two repos of the fragment that differ only by case",
			func(f *Fragment) { f.Repos["Gadget"] = &Repo{Preset: "private"} },
			[]string{"repos.Gadget", "differs only by case", "repos.gadget"},
		},
		{
			"two teams of the fragment that differ only by case",
			func(f *Fragment) { f.Teams["Squad"] = &Team{} },
			[]string{"teams.Squad", "differs only by case", "teams.squad"},
		},
		{
			"two variables of the fragment that differ only by case",
			func(f *Fragment) { f.Variables["globex_region"] = &OrgVariable{Value: "us"} },
			[]string{"variables.GLOBEX_REGION", "differs only by case", "variables.globex_region"},
		},
		{
			"engine credentials",
			func(f *Fragment) { f.EngineCredentials = &EngineCredentials{SSMPrefix: "/x"} },
			[]string{"engine_credentials", "not allowed in a fragment"},
		},
		{"owners", func(f *Fragment) { f.Owners = []string{"someone"} }, []string{"owners", "not allowed in a fragment"}},
		{"security configurations", func(f *Fragment) {
			f.SecurityConfigurations = map[string]*SecurityConfiguration{"Baseline": {}}
		}, []string{"security_configurations", "not allowed in a fragment"}},
		{
			"default security configuration",
			func(f *Fragment) { f.DefaultSecurityConfiguration = "Baseline" },
			[]string{"default_security_configuration", "not allowed in a fragment"},
		},
		{"runner groups", func(f *Fragment) { f.RunnerGroups = map[string]*RunnerGroup{"fleet": {}} }, []string{"runner_groups", "not allowed in a fragment"}},
		{"company link", func(f *Fragment) { f.Company = "globex" }, []string{"company", "not allowed in a fragment"}},
		{"tag ruleset bypass app outside the ceiling", func(f *Fragment) {
			f.Repos["gadget"].TagRulesets[0].BypassApps = []string{"other-bot"}
		}, []string{"repos.gadget.tag_rulesets[0].bypass_apps[0]", `"other-bot"`, "outside ceiling"}},
		{"branch ruleset bypass app outside the ceiling", func(f *Fragment) {
			f.Repos["gadget"].BranchRulesets = []*BranchRuleset{{
				Name: "gate", Pattern: "~DEFAULT_BRANCH", RequiredApprovals: 1, BypassApps: []string{"other-bot"},
			}}
		}, []string{"repos.gadget.branch_rulesets[0].bypass_apps[0]", "outside ceiling"}},
		{
			"preset outside the ceiling",
			func(f *Fragment) { f.Repos["gadget"].Preset = "restricted" },
			[]string{"repos.gadget.preset", `"restricted"`, "outside ceiling"},
		},
		{
			"access bundle outside the ceiling",
			func(f *Fragment) { f.Repos["gadget"].Access = []string{"managers"} },
			[]string{"repos.gadget.access[0]", `"managers"`, "outside ceiling"},
		},
		{
			"team permission above the maximum",
			func(f *Fragment) { f.Repos["gadget"].Teams["squad"] = "admin" },
			[]string{"repos.gadget.teams.squad", `"admin"`, "outside ceiling"},
		},
		{"visibility public through the preset", func(f *Fragment) { f.Repos["gadget"].Preset = "public" }, []string{"repos.gadget", "public", "outside ceiling"}},
		{"visibility public through an override", func(f *Fragment) {
			v := VisibilityPublic
			f.Repos["gadget"].Overrides = &RepoSettings{Visibility: &v}
			f.Repos["gadget"].Reason = "demo"
		}, []string{"repos.gadget", "public", "outside ceiling"}},
		{"team parent not in the fragment", func(f *Fragment) { f.Teams["squad"].Parent = "management" }, []string{"teams.squad.parent", `"management"`}},
		{"default access bundle outside the ceiling", func(f *Fragment) { f.DefaultAccess = []string{"managers"} }, []string{"default_access[0]", "outside ceiling"}},
		{"settings the provider sets", func(f *Fragment) { f.Settings = &OrgSettings{} }, []string{"settings", "provider-owned"}},
		{"waiver for a repository the fragment does not own", func(f *Fragment) { f.Waivers["no CI yet"] = []string{"infra"} }, []string{"checks_waived", `"infra"`}},
		{"no owner", func(f *Fragment) { f.Owner = "" }, []string{"owner"}},
	}
}

func TestComposeRefusesEachViolation(t *testing.T) {
	for _, tc := range refusals() {
		t.Run(tc.name, func(t *testing.T) {
			f := goodFragment()
			tc.edit(&f)

			cfg, err := Compose(providerFS(false), f)
			require.Error(t, err)
			assert.Nil(t, cfg, "a refusal returns no config, so a caller keeps its last good pin")

			assert.Contains(t, err.Error(), `org "acme"`)

			for _, w := range tc.want {
				assert.Contains(t, err.Error(), w)
			}
		})
	}
}

// The same code runs for a company validating its own fragment against the
// exported boundary, so what Compose would refuse is what the company
// sees first, in the same words.
func TestValidateFragmentIsWhatComposeRuns(t *testing.T) {
	provider, err := Load(providerFS(false))
	require.NoError(t, err)

	b, err := provider.Boundary("acme")
	require.NoError(t, err)

	require.NoError(t, ValidateFragment(b, goodFragment()))

	for _, tc := range refusals() {
		t.Run(tc.name, func(t *testing.T) {
			f := goodFragment()
			tc.edit(&f)

			viaValidate := ValidateFragment(b, f)
			require.Error(t, viaValidate)

			f = goodFragment()
			tc.edit(&f)

			_, viaCompose := Compose(providerFS(false), f)
			require.Error(t, viaCompose)
			assert.Equal(t, viaCompose.Error(), viaValidate.Error())
		})
	}
}

func TestComposeNamesEveryViolationAtOnce(t *testing.T) {
	f := goodFragment()
	f.Repos["infra"] = &Repo{Preset: "private"}
	f.Repos["gadget"].TagRulesets[0].BypassApps = []string{"x"}
	f.Owners = []string{"someone"}

	_, err := Compose(providerFS(false), f)
	require.Error(t, err)

	for _, w := range []string{"repos.infra", `bypass_apps[0] "x" outside ceiling`, "owners"} {
		assert.Contains(t, err.Error(), w)
	}

	assert.Contains(t, err.Error(), `org "acme": fragment owner "globex": `, "the shape operators will grep for")
}

func TestComposeRefusesADuplicateFragment(t *testing.T) {
	second := goodFragment()
	second.Owner = "initech-co"

	_, err := Compose(providerFS(false), goodFragment(), second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than one fragment")
	assert.Contains(t, err.Error(), `"acme"`)
}

func TestComposeRefusesAFragmentForAnUnknownOrg(t *testing.T) {
	f := goodFragment()
	f.Org = "umbrella"

	_, err := Compose(providerFS(false), f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"umbrella"`)
	assert.Contains(t, err.Error(), "not in the registry")
}

// Fragments are default off: an org with no ceiling accepts none.
func TestComposeRefusesAFragmentForAnOrgWithoutACeiling(t *testing.T) {
	f := goodFragment()
	f.Org = "initech"

	_, err := Compose(providerFS(false), f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"initech"`)
	assert.Contains(t, err.Error(), "fragment_ceiling")
}

// What the merged result still has to satisfy is the registry's own
// validation: a fragment cannot smuggle in a row Load would refuse.
func TestComposeHoldsFragmentRowsToTheRegistryRules(t *testing.T) {
	f := goodFragment()
	f.Repos["gadget"].Teams["nobody"] = "push"

	_, err := Compose(providerFS(false), f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nobody")
}

func TestLoadRefusesABrokenCeiling(t *testing.T) {
	for name, edit := range map[string]func(string) string{
		"unknown preset": func(s string) string {
			return strings.Replace(s, "presets: [private, public]", "presets: [private, nowhere]", 1)
		},
		"unknown access bundle": func(s string) string {
			return strings.Replace(s, "access: [devs]\n      bypass_apps", "access: [nowhere]\n      bypass_apps", 1)
		},
		"no maximum permission": func(s string) string { return strings.Replace(s, "      max_team_permission: push\n", "", 1) },
		"invalid permission": func(s string) string {
			return strings.Replace(s, "max_team_permission: push", "max_team_permission: root", 1)
		},
		"invalid reserved name": func(s string) string { return strings.Replace(s, "repos: [reserved-repo]", "repos: ['bad name']", 1) },
	} {
		t.Run(name, func(t *testing.T) {
			doc := edit(providerYAML(false))
			require.NotEqual(t, providerYAML(false), doc)

			_, err := Load(fstest.MapFS{"github.yaml": {Data: []byte(doc)}})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "fragment_ceiling")
		})
	}
}

func TestBoundaryIsJSONWithNoSecrets(t *testing.T) {
	cfg, err := Load(providerFS(false))
	require.NoError(t, err)

	b, err := cfg.Boundary("acme")
	require.NoError(t, err)

	assert.Equal(t, "acme", b.Org)
	assert.Equal(t, "push", b.MaxTeamPermission)
	assert.False(t, b.PublicRepos)
	assert.Equal(t, []string{"release-bot"}, b.BypassApps)
	assert.Equal(t, []string{"devs"}, b.Access)
	assert.Equal(t, map[string]string{"private": "private", "public": "public"}, b.Presets,
		"each allowed preset with the visibility it resolves to")
	assert.Equal(t, []string{"infra", "reserved-repo"}, b.Reserved.Repos, "declared and reserved names together")
	assert.Equal(t, []string{"management", "reserved-team"}, b.Reserved.Teams)
	assert.Equal(t, []string{"RESERVED_VAR", "RUNNER_LABELS"}, b.Reserved.Variables)
	assert.Equal(t, []string{"settings"}, b.ProviderOwns, "org-level singletons the provider already sets")

	raw, err := json.Marshal(b)
	require.NoError(t, err)

	for _, secret := range []string{"ssm_prefix", "creds", "root-admin", "fleet"} {
		assert.NotContains(t, string(raw), secret, "the boundary names, it does not carry values")
	}

	var back Boundary
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, b, back)

	// A company validates with the decoded JSON.
	require.NoError(t, ValidateFragment(back, goodFragment()))
}

func TestBoundaryRefusesWhatHasNoCeiling(t *testing.T) {
	cfg, err := Load(providerFS(false))
	require.NoError(t, err)

	_, err = cfg.Boundary("initech")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fragment_ceiling")

	_, err = cfg.Boundary("umbrella")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in the registry")
}

func TestParseFragmentIsStrictAndNamesReservedKeys(t *testing.T) {
	const doc = `
org: acme
teams:
  squad:
    privacy: closed
repos:
  gadget:
    preset: private
    access: [devs]
    teams:
      squad: push
variables:
  GLOBEX_REGION:
    value: eu
checks_waived:
  no CI yet: [gadget]
`

	f, err := ParseFragment("globex", []byte(doc))
	require.NoError(t, err)
	assert.Equal(t, "acme", f.Org)
	assert.Equal(t, "globex", f.Owner)
	assert.Contains(t, f.Repos, "gadget")

	_, err = Compose(providerFS(false), f)
	require.NoError(t, err)

	_, err = ParseFragment("globex", []byte("org: acme\nrepoz: {}\n"))
	require.Error(t, err, "an unknown key is an error, like everywhere in the registry")

	f, err = ParseFragment("globex", []byte("org: acme\nengine_credentials:\n  ssm_prefix: /x\n"))
	require.NoError(t, err, "a reserved key parses so that it is refused by name")

	b := mustBoundary(t)
	err = ValidateFragment(b, f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "engine_credentials")

	_, err = ParseFragment("globex", []byte("teams: {}\n"))
	require.Error(t, err, "a fragment names its org")
}

func mustBoundary(t *testing.T) Boundary {
	t.Helper()

	cfg, err := Load(providerFS(false))
	require.NoError(t, err)

	b, err := cfg.Boundary("acme")
	require.NoError(t, err)

	return b
}

// The ceiling's collision check folds case: GitHub names a repository,
// team or variable the same whatever its case, so a case variant of a
// provider row would address the provider's own resource.
func TestValidateFragmentFoldsCaseForReservedRulesets(t *testing.T) {
	b := mustBoundary(t)
	b.Reserved.TagRulesets = []string{"Release-Gate"}

	f := goodFragment()
	f.TagRulesets = map[string]*TagRuleset{"release-gate": {Name: "x", Pattern: "refs/tags/v*"}}

	err := ValidateFragment(b, f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tag_rulesets.release-gate: provider-owned (reserved)")
}

func TestMergedResultRefusesCaseOnlyCollisions(t *testing.T) {
	org := &Org{
		Teams: map[string]*Team{"Squad": {}, "squad": {}, "other": {}},
		Repos: map[string]*Repo{"gadget": {}, "Gadget": {}, "Alone": {}, "ALONE": {}},
		Settings: &OrgSettings{Actions: &OrgActions{
			Variables: map[string]*OrgVariable{"REGION": {}, "region": {}},
		}},
	}
	f := &Fragment{OrgFragment: OrgFragment{
		Teams:     map[string]*Team{"squad": {}},
		Repos:     map[string]*Repo{"Gadget": {}},
		Variables: map[string]*OrgVariable{"region": {}},
	}}

	got := mergedCaseProblems(org, f)
	assert.Equal(t, []string{
		"repos.Gadget: differs only by case from repos.gadget: GitHub treats the names as one",
		"teams.Squad: differs only by case from teams.squad: GitHub treats the names as one",
		"variables.REGION: differs only by case from variables.region: GitHub treats the names as one",
	}, got, "only the collisions that involve a row of the fragment")
}
