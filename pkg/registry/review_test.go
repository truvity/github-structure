package registry_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	registry "github.com/truvity/github-structure/pkg/registry"
)

// The review gate: one field per repository answering "how does a pull
// request become a merge here?". These tests hold that answer to ONE
// spelling — the older ways of saying it must not survive next to it,
// because two spellings drift and the reader believes the wrong one.

func TestReviewEmptyByDefault(t *testing.T) {
	c, err := load(t, minimal)
	require.NoError(t, err)

	got, ok := c.ResolveRepo("acme", "widget")
	require.True(t, ok)
	assert.Empty(t, got.Review,
		"no review field anywhere: the pre-review shape, where protection's own approval knobs are the gate")
}

func TestReviewComesFromThePreset(t *testing.T) {
	body := strings.Replace(minimal, "  public:\n", "  public:\n    review: required\n", 1)

	c, err := load(t, body)
	require.NoError(t, err)

	got, ok := c.ResolveRepo("acme", "widget")
	require.True(t, ok)
	assert.Equal(t, registry.ReviewRequired, got.Review)
}

func TestReviewRowWinsOverPreset(t *testing.T) {
	body := strings.Replace(minimal, "  public:\n", "  public:\n    review: required\n", 1)
	body = strings.Replace(body, "        preset: public\n", "        preset: public\n        review: none\n", 1)

	c, err := load(t, body)
	require.NoError(t, err)

	got, ok := c.ResolveRepo("acme", "widget")
	require.True(t, ok)
	assert.Equal(t, registry.ReviewNone, got.Review,
		"the row is where a deviation belongs, and it carries no reason: which repos need a reviewed merge is a fact about them, not a settings deviation")
}

func TestReviewRejectsUnknownValue(t *testing.T) {
	body := strings.Replace(minimal, "        preset: public\n", "        preset: public\n        review: maybe\n", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be none or required")
}

func TestReviewRejectsUnknownValueOnPreset(t *testing.T) {
	body := strings.Replace(minimal, "  public:\n", "  public:\n    review: two\n", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be none or required")
}

// A preset stating both spellings would have the engine obey one of them
// and the file say the other.
func TestReviewRejectsPresetWithClassicApprovals(t *testing.T) {
	body := strings.Replace(minimal, "  public:\n", "  public:\n    review: none\n", 1)
	body = strings.Replace(body, "      required_approvals: 0", "      required_approvals: 1", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "review owns the approval gate")
}

func TestReviewRejectsOverrideWithClassicApprovals(t *testing.T) {
	body := strings.Replace(minimal, "  public:\n", "  public:\n    review: none\n", 1)
	body = strings.Replace(body, "        preset: public\n", `        preset: public
        reason: historical
        overrides:
          protection:
            required_approvals: 2
`, 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "review owns the approval gate")
}

// `review` is a row field. Inside overrides it would need a reason and
// would merge like a setting, giving the one decisive field two homes.
func TestReviewRejectedInsideOverrides(t *testing.T) {
	body := strings.Replace(minimal, "        preset: public\n", `        preset: public
        reason: historical
        overrides:
          review: required
`, 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "belongs on the repository row")
}

func TestReviewRejectedOnArchivedRepo(t *testing.T) {
	body := strings.Replace(minimal, "        preset: public\n",
		"        preset: public\n        archived: true\n        review: required\n", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "archived")
}

// The hand-written approval ruleset is exactly what the field renders,
// so one next to it is a second, divergent copy of the same gate.
func TestReviewRejectsHandWrittenApprovalRuleset(t *testing.T) {
	body := strings.Replace(minimal, "        preset: public\n", `        preset: public
        review: required
        branch_rulesets:
          - name: pr-approval
            pattern: ~DEFAULT_BRANCH
            required_approvals: 1
            bypass_org_admins: true
`, 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "both require approvals")
}

// Under `review: none` an approval ruleset is accepted when it names a
// bypass App — the direct-pushing writer `required` cannot exempt — and
// refused without one, because then `review: required` says the same.
func TestReviewNoneAcceptsApprovalRulesetOnlyWithBypassApp(t *testing.T) {
	body := strings.Replace(minimal, "        preset: public\n", `        preset: public
        review: none
        branch_rulesets:
          - name: master-check
            pattern: ~DEFAULT_BRANCH
            required_approvals: 1
            required_checks: [check]
            bypass_apps: [acme-releases]
            bypass_org_admins: true
`, 1)

	_, err := load(t, body)
	require.NoError(t, err)

	noApp := strings.Replace(body, "            bypass_apps: [acme-releases]\n", "            bypass_apps: []\n", 1)

	_, err = load(t, noApp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no bypass_apps")
}

// A checks-only ruleset is legitimate next to `review` — it is how an
// App direct-pushes past CI — unless it takes the name the field
// renders, which would be two declarations of one ruleset.
func TestReviewAcceptsChecksOnlyRulesetButNotItsName(t *testing.T) {
	body := strings.Replace(minimal, "        preset: public\n", `        preset: public
        review: none
        branch_rulesets:
          - name: master-check
            pattern: ~DEFAULT_BRANCH
            required_approvals: 0
            required_checks: [check]
            bypass_apps: [acme-releases]
`, 1)

	_, err := load(t, body)
	require.NoError(t, err)

	clash := strings.Replace(body, "name: master-check", "name: pr-approval", 1)

	_, err = load(t, clash)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides")
}

// Resolve is the engine's only input, so it must never hand the engine a
// classic approval block that a review value has already answered for —
// including from a Config built in code, which no loader validated.
func TestResolveZeroesClassicApprovalsUnderReview(t *testing.T) {
	approvals := 2
	enabled := true
	bypassers := []string{"/someone"}
	review := registry.ReviewRequired

	c := &registry.Config{
		Presets: map[string]*registry.RepoSettings{
			"p": {
				Review: &review,
				Protection: &registry.ProtectionSettings{
					Enabled:              &enabled,
					RequiredApprovals:    &approvals,
					PullRequestBypassers: &bypassers,
				},
			},
		},
	}

	got := c.Resolve(&registry.Repo{Preset: "p"})

	assert.Equal(t, registry.ReviewRequired, got.Review)
	assert.Equal(t, 0, got.Protection.RequiredApprovals)
	assert.Empty(t, got.Protection.PullRequestBypassers)
}

// review_dismiss_stale: whether the rendered gate dismisses an approval
// when commits are pushed. It follows `review`'s precedence (row over
// preset), is false wherever nobody says otherwise, and cannot be set
// without a gate to carry it.

func TestReviewDismissStaleDefaultsToFalse(t *testing.T) {
	body := strings.Replace(minimal, "  public:\n", "  public:\n    review: required\n", 1)

	c, err := load(t, body)
	require.NoError(t, err)

	got, ok := c.ResolveRepo("acme", "widget")
	require.True(t, ok)
	assert.False(t, got.ReviewDismissStale,
		"unset is the shape every gate had before the field: no dismissal rendered")
}

func TestReviewDismissStaleOnTheRow(t *testing.T) {
	body := strings.Replace(minimal, "  public:\n", "  public:\n    review: required\n", 1)
	body = strings.Replace(body, "        preset: public\n", "        preset: public\n        review_dismiss_stale: true\n", 1)

	c, err := load(t, body)
	require.NoError(t, err)

	got, ok := c.ResolveRepo("acme", "widget")
	require.True(t, ok)
	assert.True(t, got.ReviewDismissStale)
}

func TestReviewDismissStaleFromThePresetAndRowWins(t *testing.T) {
	body := strings.Replace(minimal, "  public:\n", "  public:\n    review: required\n    review_dismiss_stale: true\n", 1)

	c, err := load(t, body)
	require.NoError(t, err)

	got, ok := c.ResolveRepo("acme", "widget")
	require.True(t, ok)
	assert.True(t, got.ReviewDismissStale)

	optOut := strings.Replace(body, "        preset: public\n", "        preset: public\n        review_dismiss_stale: false\n", 1)

	c, err = load(t, optOut)
	require.NoError(t, err)

	got, ok = c.ResolveRepo("acme", "widget")
	require.True(t, ok)
	assert.False(t, got.ReviewDismissStale, "an explicit false on the row opts out of the preset's true")
}

// There is no gate to dismiss reviews on without `review: required`.
func TestReviewDismissStaleNeedsAGate(t *testing.T) {
	for _, review := range []string{"", "        review: none\n"} {
		body := strings.Replace(minimal, "        preset: public\n",
			"        preset: public\n"+review+"        review_dismiss_stale: true\n", 1)

		_, err := load(t, body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "review_dismiss_stale")
	}
}

func TestReviewDismissStaleRejectedOnPresetWithoutGate(t *testing.T) {
	body := strings.Replace(minimal, "  public:\n", "  public:\n    review_dismiss_stale: true\n", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs `review: required`")
}

// A row that turns the gate off over a preset that dismisses must say
// so for the flag too, instead of leaving a setting that does nothing.
func TestReviewDismissStaleRowNoneOverPresetMustOptOut(t *testing.T) {
	body := strings.Replace(minimal, "  public:\n", "  public:\n    review: required\n    review_dismiss_stale: true\n", 1)
	body = strings.Replace(body, "        preset: public\n", "        preset: public\n        review: none\n", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "review_dismiss_stale")

	body = strings.Replace(body, "        review: none\n", "        review: none\n        review_dismiss_stale: false\n", 1)

	_, err = load(t, body)
	require.NoError(t, err)
}

func TestReviewDismissStaleRejectedInOverridesAndOnArchived(t *testing.T) {
	body := strings.Replace(minimal, "  public:\n", "  public:\n    review: required\n", 1)

	archived := strings.Replace(body, "        preset: public\n",
		"        preset: public\n        archived: true\n        review_dismiss_stale: true\n", 1)

	_, err := load(t, archived)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "archived")
}

// The same flag on a hand-written branch ruleset (not the synthesised
// gate) loads and survives into the declared ruleset.
func TestBranchRulesetDismissStaleReviewsOnPushLoads(t *testing.T) {
	body := strings.Replace(minimal, "        preset: public\n", `        preset: public
        branch_rulesets:
          - name: approvals
            pattern: ~DEFAULT_BRANCH
            required_approvals: 1
            bypass_org_admins: true
            dismiss_stale_reviews_on_push: true
`, 1)

	c, err := load(t, body)
	require.NoError(t, err)

	rs := c.Orgs["acme"].Repos["widget"].BranchRulesets
	require.Len(t, rs, 1)
	assert.True(t, rs[0].DismissStaleReviewsOnPush)
}
