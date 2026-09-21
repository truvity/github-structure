package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"

	registry "github.com/truvity/github-structure/pkg/registry"
)

// The installations read costs the engine App an
// organization_administration permission, so it is asked for only where
// a ruleset actually names a bypass App. A permission demanded for a
// fact nobody wanted is a permission that gets granted rather than
// questioned.
func TestNamesBypassApps(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		org  *registry.Org
		want bool
	}{
		"no rulesets at all": {
			org:  &registry.Org{Repos: map[string]*registry.Repo{"widget": {}}},
			want: false,
		},
		"rulesets bypassed by admins and teams only": {
			org: &registry.Org{Repos: map[string]*registry.Repo{"widget": {
				BranchRulesets: []*registry.BranchRuleset{{Name: "pr-approval", BypassOrgAdmins: true}},
				TagRulesets:    []*registry.TagRuleset{{Name: "tags", BypassTeams: []string{"engineers"}}},
			}}},
			want: false,
		},
		"a branch ruleset names one": {
			org: &registry.Org{Repos: map[string]*registry.Repo{"widget": {
				BranchRulesets: []*registry.BranchRuleset{{Name: "master-check", BypassApps: []string{"acme-releases"}}},
			}}},
			want: true,
		},
		"a tag ruleset names one": {
			org: &registry.Org{Repos: map[string]*registry.Repo{"widget": {
				TagRulesets: []*registry.TagRuleset{{Name: "tags", BypassApps: []string{"acme-releases"}}},
			}}},
			want: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, namesBypassApps(tc.org))
		})
	}
}
