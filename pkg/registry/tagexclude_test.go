package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tagRulesetRow(exclude string) string {
	return withRepos(`
    repos:
      widget:
        preset: public
        access: [managers]
        tag_rulesets:
          - name: release-tags
            pattern: refs/tags/v*
` + exclude + `
            bypass_teams: [management]
`)
}

// The exclude list loads as written; a named ruleset carries it to every
// row that references it.
func TestTagRulesetExcludeLoads(t *testing.T) {
	cfg, err := load(t, tagRulesetRow(`            exclude: ["refs/tags/v*-*"]`))
	require.NoError(t, err)

	rs := cfg.Orgs["acme"].Repos["widget"].TagRulesets[0]
	assert.Equal(t, []string{"refs/tags/v*-*"}, rs.Exclude)

	named, err := load(t, withRepos(`
    tag_rulesets:
      release-tags:
        pattern: refs/tags/v*
        exclude: ["refs/tags/v*-*"]
        bypass_teams: [management]
    repos:
      widget:
        preset: public
        access: [managers]
        tag_rulesets: [release-tags]
`))
	require.NoError(t, err)
	assert.Equal(t, []string{"refs/tags/v*-*"}, named.Orgs["acme"].Repos["widget"].TagRulesets[0].Exclude)
}

// No exclude stays nil: the engine renders the empty list it always did.
func TestTagRulesetWithoutExcludeIsNil(t *testing.T) {
	cfg, err := load(t, tagRulesetRow(""))
	require.NoError(t, err)
	assert.Nil(t, cfg.Orgs["acme"].Repos["widget"].TagRulesets[0].Exclude)
}

func TestTagRulesetExcludeRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ exclude, want string }{
		"not a tag ref":      {`            exclude: ["v*-*"]`, "must start with refs/tags/"},
		"a branch ref":       {`            exclude: ["refs/heads/main"]`, "must start with refs/tags/"},
		"equals the pattern": {`            exclude: ["refs/tags/v*"]`, "equals the pattern"},
		"listed twice":       {`            exclude: ["refs/tags/v*-*", "refs/tags/v*-*"]`, "listed twice"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, tagRulesetRow(tc.exclude))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
