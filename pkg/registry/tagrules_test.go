package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-structure/pkg/registry"
)

// tagRuleRow is a one-ruleset registry whose body is `extra` (lines
// indented to sit inside the ruleset), beside a pattern and a name.
func tagRuleRow(extra string) string {
	return withRepos(`
    repos:
      widget:
        preset: public
        access: [managers]
        tag_rulesets:
          - name: v2-tags
            pattern: refs/tags/v2.*
` + extra)
}

// A ruleset that names no `rules:` restricts all three acts, as it always
// did: Rules stays nil and EffectiveRules is the full set in render order.
func TestTagRulesetWithoutRulesIsAllThree(t *testing.T) {
	cfg, err := load(t, tagRuleRow("            bypass_teams: [management]\n"))
	require.NoError(t, err)

	rs := cfg.Orgs["acme"].Repos["widget"].TagRulesets[0]
	assert.Nil(t, rs.Rules)
	assert.False(t, rs.NoBypass)
	assert.Equal(t, []string{"creation", "update", "deletion"}, rs.EffectiveRules())
}

// A subset loads as written, and EffectiveRules reports it in the fixed
// render order whatever order the row used.
func TestTagRulesetRulesSubset(t *testing.T) {
	cfg, err := load(t, tagRuleRow("            rules: [deletion, creation]\n            bypass_teams: [management]\n"))
	require.NoError(t, err)

	rs := cfg.Orgs["acme"].Repos["widget"].TagRulesets[0]
	assert.Equal(t, []string{"deletion", "creation"}, rs.Rules)
	assert.Equal(t, []string{"creation", "deletion"}, rs.EffectiveRules())
}

// The two shapes the issue names: creation restricted to one team, and
// update plus deletion with no bypass at all.
func TestTagRulesetIssueShapesLoad(t *testing.T) {
	cfg, err := load(t, withRepos(`
    repos:
      widget:
        preset: public
        access: [managers]
        tag_rulesets:
          - name: v2-tags-create
            pattern: refs/tags/v2.*
            rules: [creation]
            bypass_teams: [management]
          - name: v2-tags-immutable
            pattern: refs/tags/v2.*
            rules: [update, deletion]
            no_bypass: true
`))
	require.NoError(t, err)

	rows := cfg.Orgs["acme"].Repos["widget"].TagRulesets
	require.Len(t, rows, 2)

	assert.Equal(t, []string{"creation"}, rows[0].Rules)
	assert.Equal(t, []string{"management"}, rows[0].BypassTeams)
	assert.Equal(t, []string{"update", "deletion"}, rows[1].Rules)
	assert.True(t, rows[1].NoBypass)
	assert.Empty(t, rows[1].BypassTeams)
}

// `no_bypass` with no `rules:` is the full set nobody can bypass: allowed,
// because it is a deliberate statement.
func TestTagRulesetNoBypassAloneLoads(t *testing.T) {
	_, err := load(t, tagRuleRow("            no_bypass: true\n"))
	require.NoError(t, err)
}

// An org-level named ruleset carries the new keys to every row that
// references it, and the same validation applies to its body.
func TestNamedTagRulesetCarriesRulesAndNoBypass(t *testing.T) {
	cfg, err := load(t, withRepos(`
    tag_rulesets:
      frozen:
        pattern: refs/tags/v2.*
        rules: [update, deletion]
        no_bypass: true
    repos:
      widget:
        preset: public
        access: [managers]
        tag_rulesets: [frozen]
`))
	require.NoError(t, err)

	rs := cfg.Orgs["acme"].Repos["widget"].TagRulesets[0]
	assert.Equal(t, "frozen", rs.Name)
	assert.Equal(t, []string{"update", "deletion"}, rs.Rules)
	assert.True(t, rs.NoBypass)

	_, err = load(t, withRepos(`
    tag_rulesets:
      frozen:
        pattern: refs/tags/v2.*
        rules: [update, push]
        no_bypass: true
    repos:
      widget:
        preset: public
        access: [managers]
        tag_rulesets: [frozen]
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"push" is not one of creation, update, deletion`)
}

func TestTagRulesetRulesAndNoBypassRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"empty rules": {
			"            rules: []\n            bypass_teams: [management]\n",
			"rules is empty",
		},
		"unknown rule": {
			"            rules: [creation, push]\n            bypass_teams: [management]\n",
			`rules entry "push" is not one of creation, update, deletion`,
		},
		"repeated rule": {
			"            rules: [update, update]\n            bypass_teams: [management]\n",
			`rules entry "update" is listed twice`,
		},
		"no_bypass beside a team": {
			"            no_bypass: true\n            bypass_teams: [management]\n",
			"no_bypass contradicts",
		},
		"no_bypass beside an app": {
			"            no_bypass: true\n            bypass_apps: [automation]\n",
			"no_bypass contradicts",
		},
		"no_bypass beside org admins": {
			"            no_bypass: true\n            bypass_org_admins: true\n",
			"no_bypass contradicts",
		},
		"no bypass and no no_bypass": {
			"            rules: [creation]\n",
			"at least one bypass team is required",
		},
		"rules without a bypass": {
			"            rules: [update, deletion]\n            bypass_apps: [automation]\n",
			"at least one bypass team is required",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, tagRuleRow(tc.body))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// The refusal tells the author the deliberate spelling.
func TestTagRulesetMissingBypassMentionsNoBypass(t *testing.T) {
	_, err := load(t, tagRuleRow(""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no_bypass: true")
}

// A typo in the new keys is refused like any other unknown key, though the
// ruleset decodes through a custom UnmarshalYAML.
func TestTagRulesetUnknownKeyStillRefused(t *testing.T) {
	_, err := load(t, tagRuleRow("            rule: [creation]\n            bypass_teams: [management]\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "field rule not found")
}

// A row may not copy the org's body, and the new fields are part of the
// body: a row that differs only in `rules` or `no_bypass` is a legitimate
// one-off, not a copy.
func TestTagRulesetCopyDetectionSeesRulesAndNoBypass(t *testing.T) {
	const org = `
    tag_rulesets:
      v2-tags:
        pattern: refs/tags/v2.*
        bypass_teams: [management]
    repos:
      widget:
        preset: public
        access: [managers]
        tag_rulesets:
`

	_, err := load(t, withRepos(org+`          - name: v2-tags
            pattern: refs/tags/v2.*
            bypass_teams: [management]
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is a copy of the org-level one")

	// `rules` spelled out as the default is still the same body.
	_, err = load(t, withRepos(org+`          - name: v2-tags
            pattern: refs/tags/v2.*
            rules: [creation, update, deletion]
            bypass_teams: [management]
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is a copy of the org-level one")

	_, err = load(t, withRepos(org+`          - name: v2-tags
            pattern: refs/tags/v2.*
            rules: [creation]
            bypass_teams: [management]
`))
	require.NoError(t, err)

	_, err = load(t, withRepos(org+`          - name: v2-tags
            pattern: refs/tags/v2.*
            no_bypass: true
`))
	require.NoError(t, err)
}

// The exported set is the three acts and nothing else.
func TestTagRuleRulesAreTheThreeActs(t *testing.T) {
	assert.Equal(t, []string{"creation", "update", "deletion"}, registry.TagRuleRules)
}
