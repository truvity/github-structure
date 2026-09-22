package registry_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The three ways a registry says a thing once — a named tag ruleset, a
// waiver grouped by reason, a team default — each resolve to exactly the
// rows they replace. The proof is the same for all three: the resolved
// Config is indistinguishable from the one where every row spelled it
// out, so an estate adopting them changes nothing it applies.

const (
	inlineRuleset = `
    repos:
      widget:
        preset: public
        access: [managers]
        tag_rulesets:
          - name: release-tags
            pattern: refs/tags/v*
            bypass_teams: [management]
            bypass_apps: [automation]
`
	namedRuleset = `
    tag_rulesets:
      release-tags:
        pattern: refs/tags/v*
        bypass_teams: [management]
        bypass_apps: [automation]
    repos:
      widget:
        preset: public
        access: [managers]
        tag_rulesets: [release-tags]
`
)

func withRepos(body string) string {
	return strings.Replace(minimal, `
    repos:
      widget:
        preset: public
        access: [managers]
`, body, 1)
}

func TestNamedTagRulesetResolvesLikeAnInlineOne(t *testing.T) {
	inline, err := load(t, withRepos(inlineRuleset))
	require.NoError(t, err)

	named, err := load(t, withRepos(namedRuleset))
	require.NoError(t, err)

	want := inline.Orgs["acme"].Repos["widget"].TagRulesets
	got := named.Orgs["acme"].Repos["widget"].TagRulesets

	require.Len(t, got, 1)
	assert.Empty(t, got[0].Ref, "a loaded Config never carries a reference")
	assert.Equal(t, *want[0], *got[0], "the engine must see the row it always saw")
	assert.Equal(t, "release-tags", named.Orgs["acme"].TagRulesets["release-tags"].Name, "the key is the name")
}

func TestNamedTagRulesetIsACopyPerRow(t *testing.T) {
	two := strings.Replace(namedRuleset, `
        tag_rulesets: [release-tags]
`, `
        tag_rulesets: [release-tags]
      gadget:
        preset: public
        access: [managers]
        tag_rulesets: [release-tags]
`, 1)

	cfg, err := load(t, withRepos(two))
	require.NoError(t, err)

	org := cfg.Orgs["acme"]
	a, b := org.Repos["widget"].TagRulesets[0], org.Repos["gadget"].TagRulesets[0]
	assert.NotSame(t, a, b, "resolving into rows must not alias one struct across repositories")
	assert.NotSame(t, org.TagRulesets["release-tags"], a, "nor alias the definition itself")
}

func TestNamedTagRulesetUnknownReferenceIsRefused(t *testing.T) {
	_, err := load(t, withRepos(strings.Replace(namedRuleset, "[release-tags]", "[release-tag]", 1)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `references "release-tag", which the org does not define`)
}

func TestNamedTagRulesetCopiedInlineIsRefused(t *testing.T) {
	copyOf := strings.Replace(namedRuleset, `
        tag_rulesets: [release-tags]
`, `
        tag_rulesets:
          - name: release-tags
            pattern: refs/tags/v*
            bypass_teams: [management]
            bypass_apps: [automation]
`, 1)

	_, err := load(t, withRepos(copyOf))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is a copy of the org-level one")
}

func TestNamedTagRulesetDeviationUnderTheSameNameIsAOneOff(t *testing.T) {
	// The name is the key of a LIVE per-repository resource: a row whose
	// rule deliberately differs (here: no bypass App) keeps its name
	// rather than being forced into a rename that replaces the ruleset.
	deviation := strings.Replace(namedRuleset, `
        tag_rulesets: [release-tags]
`, `
        tag_rulesets:
          - name: release-tags
            pattern: refs/tags/v*
            bypass_teams: [management]
`, 1)

	cfg, err := load(t, withRepos(deviation))
	require.NoError(t, err)
	assert.Empty(t, cfg.Orgs["acme"].Repos["widget"].TagRulesets[0].BypassApps, "the row's own body wins")
}

func TestNamedTagRulesetKeyIsTheName(t *testing.T) {
	_, err := load(t, withRepos(strings.Replace(namedRuleset, `
      release-tags:
        pattern:`, `
      release-tags:
        name: other
        pattern:`, 1)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disagrees with the key")
}

func TestNamedTagRulesetIsValidatedEvenWhenUnused(t *testing.T) {
	unused := strings.Replace(namedRuleset, "bypass_teams: [management]", "bypass_teams: [nobody]", 1)
	unused = strings.Replace(unused, "        tag_rulesets: [release-tags]\n", "", 1)

	_, err := load(t, withRepos(unused))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `bypass team "nobody" is not in this org's teams`)
}

func TestTagRulesetBodyStaysStrict(t *testing.T) {
	// A custom decoder must not become the one place a typo loads.
	_, err := load(t, withRepos(strings.Replace(inlineRuleset, "bypass_apps:", "bypass_app:", 1)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bypass_app")
}

func TestOrgWaiverLandsOnTheRow(t *testing.T) {
	cfg, err := load(t, withRepos(`
    checks_waived:
      "no workflow here reports check yet": [widget, gadget]
    repos:
      widget:
        preset: public
        access: [managers]
      gadget:
        preset: public
        access: [managers]
`))
	require.NoError(t, err)

	org := cfg.Orgs["acme"]
	assert.Equal(t, map[string]string{
		"widget": "no workflow here reports check yet",
		"gadget": "no workflow here reports check yet",
	}, org.ChecksWaived(), "the row and the org list are one fact")

	for _, name := range []string{"widget", "gadget"} {
		assert.Empty(t, cfg.Resolve(org.Repos[name]).Protection.RequiredChecks, "%s: the profile's checks are dropped, as a row waiver drops them", name)
	}
}

func TestOrgWaiverRefusesARepoWaivedTwice(t *testing.T) {
	for name, body := range map[string]string{
		"on its row too": `
    checks_waived:
      "no CI yet": [widget]
    repos:
      widget:
        preset: public
        access: [managers]
        checks_waived: no CI yet
`,
		"under two reasons": `
    checks_waived:
      "no CI yet": [widget]
      "deploy-only workflows": [widget]
    repos:
      widget:
        preset: public
        access: [managers]
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, withRepos(body))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "already waived")
		})
	}
}

func TestOrgWaiverRefusesAnUnknownRepoAndAnEmptyReason(t *testing.T) {
	_, err := load(t, withRepos(`
    checks_waived:
      "no CI yet": [gadget]
    repos:
      widget:
        preset: public
        access: [managers]
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `repo "gadget" is not declared`)

	_, err = load(t, withRepos(`
    checks_waived:
      "": [widget]
    repos:
      widget:
        preset: public
        access: [managers]
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs a reason")
}

func TestOrgWaiverOnAnArchivedRepoIsRefusedLikeARowOne(t *testing.T) {
	_, err := load(t, withRepos(`
    checks_waived:
      "no CI yet": [widget]
    repos:
      widget:
        preset: public
        access: [managers]
        archived: true
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "archived, so checks_waived does nothing")
}

func TestTeamDefaultsFillWhatARowLeavesUnsaid(t *testing.T) {
	cfg, err := load(t, strings.Replace(minimal, `
    teams:
      management:
        privacy: closed
      engineers:
        privacy: closed
        parent: management
`, `
    team_defaults:
      privacy: closed
      notifications: enabled
    teams:
      management: {}
      engineers:
        parent: management
        notifications: disabled
`, 1))
	require.NoError(t, err)

	teams := cfg.Orgs["acme"].Teams
	assert.Equal(t, "closed", teams["management"].Privacy)
	assert.Equal(t, "enabled", teams["management"].Notifications)
	assert.Equal(t, "closed", teams["engineers"].Privacy, "a nested team inherits too")
	assert.Equal(t, "disabled", teams["engineers"].Notifications, "the row keeps what it wrote")
}

func TestTeamDefaultsAreValidated(t *testing.T) {
	_, err := load(t, strings.Replace(minimal, "    teams:\n", "    team_defaults:\n      privacy: open\n    teams:\n", 1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "team_defaults.privacy")
}

func TestTeamDefaultsDoNotExemptANestedTeamFromTheSecretRule(t *testing.T) {
	// The inherited value is checked exactly like a written one.
	_, err := load(t, strings.Replace(minimal, `
    teams:
      management:
        privacy: closed
      engineers:
        privacy: closed
        parent: management
`, `
    team_defaults:
      privacy: secret
    teams:
      management: {}
      engineers:
        parent: management
`, 1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a nested team cannot be secret")
}
