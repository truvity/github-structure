package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	registry "github.com/truvity/github-structure/pkg/registry"
)

// reviewGate IS the review mechanics: what it returns is the whole of
// what a `review:` row changes on GitHub. Each case below is a shape the
// estate is in today, so a regression here is a live gate moving.
func TestReviewGate(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		repo registry.Resolved
		want *registry.BranchRuleset
	}{
		"no review field: the pre-review shape renders no gate of its own": {
			repo: registry.Resolved{
				Protection: registry.ResolvedProtection{Enabled: true, RequiredChecks: []string{"check"}},
			},
			want: nil,
		},
		"none: checks alone gate the branch, so auto-merge completes on green": {
			repo: registry.Resolved{
				Review:     registry.ReviewNone,
				Protection: registry.ResolvedProtection{Enabled: true, RequiredChecks: []string{"check"}},
			},
			want: nil,
		},
		"required with a classic rule: the ruleset carries the approval, protection keeps the checks": {
			repo: registry.Resolved{
				Review:     registry.ReviewRequired,
				Protection: registry.ResolvedProtection{Enabled: true, RequiredChecks: []string{"check"}},
			},
			want: &registry.BranchRuleset{
				Name:              registry.ReviewRulesetName,
				Pattern:           defaultBranchRef,
				RequiredApprovals: 1,
				BypassOrgAdmins:   true,
			},
		},
		"required with no classic rule: the ruleset is the only place the checks can live": {
			repo: registry.Resolved{
				Review:     registry.ReviewRequired,
				Protection: registry.ResolvedProtection{Enabled: false, RequiredChecks: []string{"check", "integration"}},
			},
			want: &registry.BranchRuleset{
				Name:              registry.ReviewRulesetName,
				Pattern:           defaultBranchRef,
				RequiredApprovals: 1,
				RequiredChecks:    []string{"check", "integration"},
				BypassOrgAdmins:   true,
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := reviewGate(tc.repo)

			if tc.want == nil {
				assert.Nil(t, got)

				return
			}

			require.NotNil(t, got)
			assert.Equal(t, *tc.want, *got)
			assert.Empty(t, got.BypassApps,
				"no App bypasses a review gate any more: an App review counts towards reviewDecision, "+
					"so the fleet's automation approves instead of stepping over the rule")
		})
	}
}
