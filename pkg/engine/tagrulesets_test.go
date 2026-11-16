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
