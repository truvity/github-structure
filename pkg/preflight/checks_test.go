package preflight

import (
	"testing"

	"github.com/stretchr/testify/assert"

	registry "github.com/truvity/github-structure/pkg/registry"
)

// requiredChecksOf must name the contexts the ENGINE writes, not the
// ones the registry happens to mention: a guard probing a different set
// than the engine enforces reports on a rule nobody has.
func TestRequiredChecksOf(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		repo registry.Resolved
		want []string
	}{
		"classic rule: its contexts are the gate": {
			repo: registry.Resolved{
				Protection: registry.ResolvedProtection{Enabled: true, RequiredChecks: []string{"check"}},
			},
			want: []string{"check"},
		},
		"no rule and no review: nothing is required, so nothing can block": {
			repo: registry.Resolved{
				Protection: registry.ResolvedProtection{Enabled: false, RequiredChecks: []string{"check"}},
			},
			want: nil,
		},
		"review: required with no classic rule: the rendered ruleset carries the contexts": {
			repo: registry.Resolved{
				Review:     registry.ReviewRequired,
				Protection: registry.ResolvedProtection{Enabled: false, RequiredChecks: []string{"check", "integration"}},
			},
			want: []string{"check", "integration"},
		},
		"waived: the contexts are already gone from the resolved rule": {
			repo: registry.Resolved{
				Protection: registry.ResolvedProtection{Enabled: true},
			},
			want: nil,
		},
		"archived: no branch, no rule, nothing to probe": {
			repo: registry.Resolved{
				Archived:   true,
				Protection: registry.ResolvedProtection{Enabled: true, RequiredChecks: []string{"check"}},
			},
			want: nil,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, requiredChecksOf(tc.repo))
		})
	}
}

// The guard clears a repo as soon as ONE required context has reported:
// the question is whether the workflow exists, not whether it passed.
func TestReportedSuppressedIsEvidenceOfExistence(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{"check"},
		reportedSuppressed([]string{"check", "integration"}, []string{"check", "build"}))
	assert.Empty(t, reportedSuppressed([]string{"check"}, []string{"build"}))
}
