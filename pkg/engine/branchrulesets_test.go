package engine

import (
	"sync"
	"testing"

	"github.com/pulumi/pulumi-github/sdk/v6/go/github"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-structure/pkg/registry"
)

// teamIDMocks captures inputs like captureMocks, and gives a team a
// numeric id, as GitHub does: the ruleset reads it back as its actor id.
type teamIDMocks struct {
	mu   sync.Mutex
	seen map[string]resource.PropertyMap
}

func (m *teamIDMocks) NewResource(a pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.seen[a.Name] = a.Inputs

	if a.TypeToken == "github:index/team:Team" {
		return "4242", a.Inputs, nil
	}

	return a.Name + "_id", a.Inputs, nil
}

func (m *teamIDMocks) Call(a pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return a.Args, nil
}

// A bypass team renders as a Team actor in bypass mode pull_request, beside
// the org-admin and App actors that keep mode always: the team merges past
// the gate through a pull request, and a direct push stays refused for it.
func TestDeployBranchRulesetsBypassTeamIsPullRequestOnly(t *testing.T) {
	mocks := &teamIDMocks{seen: map[string]resource.PropertyMap{}}

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		provider, err := github.NewProvider(ctx, "gh", &github.ProviderArgs{})
		if err != nil {
			return err
		}

		sre, err := github.NewTeam(ctx, "team-sre", &github.TeamArgs{Name: pulumi.String("sre")}, pulumi.Provider(provider))
		if err != nil {
			return err
		}

		return deployBranchRulesets(ctx, "widget",
			registry.InstalledApps{"acme-releases": 77},
			[]*registry.BranchRuleset{{
				Name:              "master-check",
				Pattern:           "~DEFAULT_BRANCH",
				RequiredApprovals: 1,
				BypassApps:        []string{"acme-releases"},
				BypassOrgAdmins:   true,
				BypassTeams:       []string{"sre"},
			}},
			map[string]*github.Team{"sre": sre},
			provider)
	}, pulumi.WithMocks("proj", "stack", mocks))
	require.NoError(t, err)

	in, ok := mocks.seen["ruleset-widget-master-check"]
	require.True(t, ok, "the ruleset is declared, keyed on repo + name; got %v", mocks.seen)

	type actor struct {
		Type string
		ID   int
		Mode string
	}

	var got []actor
	for _, v := range in["bypassActors"].ArrayValue() {
		a := v.ObjectValue()
		got = append(got, actor{a["actorType"].StringValue(), int(a["actorId"].NumberValue()), a["bypassMode"].StringValue()})
	}

	assert.Equal(t, []actor{
		{"OrganizationAdmin", 0, "always"},
		{"Integration", 77, "always"},
		{"Team", 4242, "pull_request"},
	}, got)
}

// A team the ruleset names but the run never deployed fails the run, by
// name, instead of rendering a ruleset with that actor missing.
func TestDeployBranchRulesetsUndeployedBypassTeamFails(t *testing.T) {
	mocks := &teamIDMocks{seen: map[string]resource.PropertyMap{}}

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		return deployBranchRulesets(ctx, "widget", nil,
			[]*registry.BranchRuleset{{
				Name:              "master-check",
				Pattern:           "~DEFAULT_BRANCH",
				RequiredApprovals: 1,
				BypassTeams:       []string{"sre"},
			}},
			nil, nil)
	}, pulumi.WithMocks("proj", "stack", mocks))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `bypass team "sre" not deployed`)
}

// A ruleset with no bypass and the plain rules renders exactly the rules
// it names: deletion, non_fast_forward, a pull_request rule with no
// approvals that allows only the listed merge methods, and strict status
// checks. A non-default branch is just a pattern.
func TestDeployBranchRulesetsPlainRulesNoBypass(t *testing.T) {
	mocks := &captureMocks{seen: map[string]resource.PropertyMap{}}

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		provider, err := github.NewProvider(ctx, "gh", &github.ProviderArgs{})
		if err != nil {
			return err
		}

		return deployBranchRulesets(ctx, "widget", nil,
			[]*registry.BranchRuleset{{
				Name:                "release-checks",
				Pattern:             "refs/heads/release",
				NoBypass:            true,
				BlockDeletion:       true,
				BlockForcePush:      true,
				RequirePullRequest:  true,
				AllowedMergeMethods: []string{"rebase"},
				RequiredChecks:      []string{"checks"},
				StrictChecks:        true,
			}},
			nil, provider)
	}, pulumi.WithMocks("proj", "stack", mocks))
	require.NoError(t, err)

	in, ok := mocks.seen["ruleset-widget-release-checks"]
	require.True(t, ok, "the ruleset is declared, keyed on repo + name; got %v", mocks.seen)

	assert.Empty(t, in["bypassActors"].ArrayValue(), "no_bypass renders no actor at all")

	refName := in["conditions"].ObjectValue()["refName"].ObjectValue()
	assert.Equal(t, "refs/heads/release", refName["includes"].ArrayValue()[0].StringValue())

	rules := in["rules"].ObjectValue()
	assert.True(t, rules["deletion"].BoolValue())
	assert.True(t, rules["nonFastForward"].BoolValue())

	pr := rules["pullRequest"].ObjectValue()
	assert.Equal(t, 0.0, pr["requiredApprovingReviewCount"].NumberValue())
	require.Len(t, pr["allowedMergeMethods"].ArrayValue(), 1)
	assert.Equal(t, "rebase", pr["allowedMergeMethods"].ArrayValue()[0].StringValue())

	checks := rules["requiredStatusChecks"].ObjectValue()
	assert.True(t, checks["strictRequiredStatusChecksPolicy"].BoolValue())
	assert.Equal(t, "checks", checks["requiredChecks"].ArrayValue()[0].ObjectValue()["context"].StringValue())
}

// A row that sets none of the plain rules renders none of them: no
// deletion, no non_fast_forward, no strict flag, no pull_request rule at
// zero approvals. The rows already deployed plan no diff.
func TestDeployBranchRulesetsPlainRulesAreOptIn(t *testing.T) {
	mocks := &captureMocks{seen: map[string]resource.PropertyMap{}}

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		provider, err := github.NewProvider(ctx, "gh", &github.ProviderArgs{})
		if err != nil {
			return err
		}

		return deployBranchRulesets(ctx, "widget", registry.InstalledApps{"acme-releases": 77},
			[]*registry.BranchRuleset{{
				Name:           "master-check",
				Pattern:        "~DEFAULT_BRANCH",
				RequiredChecks: []string{"check"},
				BypassApps:     []string{"acme-releases"},
			}},
			nil, provider)
	}, pulumi.WithMocks("proj", "stack", mocks))
	require.NoError(t, err)

	rules := mocks.seen["ruleset-widget-master-check"]["rules"].ObjectValue()
	for _, key := range []resource.PropertyKey{"deletion", "nonFastForward", "pullRequest"} {
		_, present := rules[key]
		assert.False(t, present, "%s must not be rendered unasked", key)
	}

	checks := rules["requiredStatusChecks"].ObjectValue()
	_, strict := checks["strictRequiredStatusChecksPolicy"]
	assert.False(t, strict, "strict stays unset unless asked for")
}
