package engine

import (
	"testing"

	"github.com/pulumi/pulumi-github/sdk/v6/go/github"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-structure/pkg/registry"
)

// renderTagRuleset deploys one tag ruleset under mocks and returns the
// ref_name condition it rendered: the include and exclude lists.
func renderTagRuleset(t *testing.T, rs *registry.TagRuleset) (includes, excludes []string) {
	t.Helper()

	mocks := &teamIDMocks{seen: map[string]resource.PropertyMap{}}

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		provider, err := github.NewProvider(ctx, "gh", &github.ProviderArgs{})
		if err != nil {
			return err
		}

		release, err := github.NewTeam(ctx, "team-release", &github.TeamArgs{Name: pulumi.String("release")}, pulumi.Provider(provider))
		if err != nil {
			return err
		}

		return deployTagRulesets(ctx, "widget", registry.InstalledApps{},
			[]*registry.TagRuleset{rs}, map[string]*github.Team{"release": release}, provider)
	}, pulumi.WithMocks("proj", "stack", mocks))
	require.NoError(t, err)

	// Keyed on repository + name: a changed exclude updates this same
	// resource in place, it never replaces it.
	in, ok := mocks.seen["ruleset-widget-"+rs.Name]
	require.True(t, ok, "the ruleset is declared, keyed on repo + name; got %v", mocks.seen)

	ref := in["conditions"].ObjectValue()["refName"].ObjectValue()

	for _, v := range ref["includes"].ArrayValue() {
		includes = append(includes, v.StringValue())
	}

	require.True(t, ref["excludes"].IsArray(), "excludes is always rendered as a list")

	for _, v := range ref["excludes"].ArrayValue() {
		excludes = append(excludes, v.StringValue())
	}

	return includes, excludes
}

// A row with no exclude renders an EMPTY exclude list, exactly as before
// the field existed, so no existing ruleset sees a diff.
func TestDeployTagRulesetsWithoutExcludeRendersEmptyList(t *testing.T) {
	includes, excludes := renderTagRuleset(t, &registry.TagRuleset{
		Name: "release-tags", Pattern: "refs/tags/v*", BypassTeams: []string{"release"},
	})

	assert.Equal(t, []string{"refs/tags/v*"}, includes)
	assert.Empty(t, excludes)
}

// A row's exclude patterns render into the ruleset's ref_name excludes,
// in the order written.
func TestDeployTagRulesetsRendersExclude(t *testing.T) {
	includes, excludes := renderTagRuleset(t, &registry.TagRuleset{
		Name:        "release-tags",
		Pattern:     "refs/tags/v*",
		Exclude:     []string{"refs/tags/v*-*", "refs/tags/v0.*"},
		BypassTeams: []string{"release"},
	})

	assert.Equal(t, []string{"refs/tags/v*"}, includes)
	assert.Equal(t, []string{"refs/tags/v*-*", "refs/tags/v0.*"}, excludes)
}

// renderTagRulesetInputs deploys one tag ruleset under mocks and returns
// every input the provider would be given.
func renderTagRulesetInputs(t *testing.T, rs *registry.TagRuleset) resource.PropertyMap {
	t.Helper()

	mocks := &teamIDMocks{seen: map[string]resource.PropertyMap{}}

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		provider, err := github.NewProvider(ctx, "gh", &github.ProviderArgs{})
		if err != nil {
			return err
		}

		release, err := github.NewTeam(ctx, "team-release", &github.TeamArgs{Name: pulumi.String("release")}, pulumi.Provider(provider))
		if err != nil {
			return err
		}

		return deployTagRulesets(ctx, "widget", registry.InstalledApps{},
			[]*registry.TagRuleset{rs}, map[string]*github.Team{"release": release}, provider)
	}, pulumi.WithMocks("proj", "stack", mocks))
	require.NoError(t, err)

	in, ok := mocks.seen["ruleset-widget-"+rs.Name]
	require.True(t, ok, "the ruleset is declared, keyed on repo + name; got %v", mocks.seen)

	return in
}

// renderedRules are the rule flags that rendered true, in render order.
func renderedRules(in resource.PropertyMap) []string {
	rules := in["rules"].ObjectValue()

	var out []string

	for _, name := range []string{"creation", "update", "deletion"} {
		if v, set := rules[resource.PropertyKey(name)]; set && v.IsBool() && v.BoolValue() {
			out = append(out, name)
		}
	}

	return out
}

// A row with no `rules:` renders all three acts true, exactly the inputs
// every tag ruleset had before the field existed, so none plans a diff.
func TestDeployTagRulesetsDefaultRendersAllThreeRules(t *testing.T) {
	in := renderTagRulesetInputs(t, &registry.TagRuleset{
		Name: "release-tags", Pattern: "refs/tags/v*", BypassTeams: []string{"release"},
	})

	assert.Equal(t, []string{"creation", "update", "deletion"}, renderedRules(in))

	// Nothing else appears in the rules block.
	assert.Len(t, in["rules"].ObjectValue(), 3)
}

// A subset renders only the acts it names; an unnamed act is not set at
// all, so the provider reads it as not restricted.
func TestDeployTagRulesetsRendersTheChosenRules(t *testing.T) {
	for name, tc := range map[string]struct {
		rules []string
		want  []string
	}{
		"creation only":         {[]string{"creation"}, []string{"creation"}},
		"update and deletion":   {[]string{"update", "deletion"}, []string{"update", "deletion"}},
		"order does not matter": {[]string{"deletion", "creation"}, []string{"creation", "deletion"}},
		"all three spelled":     {[]string{"creation", "update", "deletion"}, []string{"creation", "update", "deletion"}},
	} {
		t.Run(name, func(t *testing.T) {
			in := renderTagRulesetInputs(t, &registry.TagRuleset{
				Name: "v2-tags", Pattern: "refs/tags/v2.*", Rules: tc.rules, BypassTeams: []string{"release"},
			})

			assert.Equal(t, tc.want, renderedRules(in))
			assert.Len(t, in["rules"].ObjectValue(), len(tc.want), "only the named acts are rendered")
		})
	}
}

// `no_bypass` renders a ruleset with an empty bypass list (not an absent
// one, so a live actor added by hand is a diff the plan removes), and the
// chosen rules still render.
func TestDeployTagRulesetsNoBypassRendersNoActors(t *testing.T) {
	in := renderTagRulesetInputs(t, &registry.TagRuleset{
		Name: "v2-immutable", Pattern: "refs/tags/v2.*", Rules: []string{"update", "deletion"}, NoBypass: true,
	})

	assert.Equal(t, []string{"update", "deletion"}, renderedRules(in))

	actors := in["bypassActors"]
	require.True(t, actors.IsArray(), "bypass_actors is rendered as a list, empty here")
	assert.Empty(t, actors.ArrayValue())
}

// The creation-only shape from the issue: restricted to one team, which is
// rendered as a Team actor beside the one rule.
func TestDeployTagRulesetsCreationOnlyKeepsItsTeam(t *testing.T) {
	in := renderTagRulesetInputs(t, &registry.TagRuleset{
		Name: "v2-create", Pattern: "refs/tags/v2.*", Rules: []string{"creation"}, BypassTeams: []string{"release"},
	})

	assert.Equal(t, []string{"creation"}, renderedRules(in))

	actors := in["bypassActors"].ArrayValue()
	require.Len(t, actors, 1)
	assert.Equal(t, "Team", actors[0].ObjectValue()["actorType"].StringValue())
}
