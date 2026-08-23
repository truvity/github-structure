package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"

	registry "github.com/truvity/github-structure/pkg/registry"
)

// Both rules under test were learned from production, and neither was
// catchable by reading the code.
//
//   - allowForking: creating truvity/workstation on 2026-08-16 half-failed —
//     GitHub made the repo, then answered the follow-up PATCH with
//     "422 This organization does not allow private repository forking".
//     The resource errored while the repo existed, so the stack could not
//     converge: every later run re-planned the same doomed update and the
//     repo sat with no branch protection.
//   - the archived set: a PATCH to any field but `archived` 403s, which
//     wedges every repo behind it in the run.
//
// The whole imported estate hides the first one. Import records live values,
// so an imported private repo plans no write to the field and the engine
// looks correct — it only breaks the first time the engine CREATES a private
// repo rather than adopting one.
func TestRepoIgnoredFields(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		repo        registry.Resolved
		wantContain []string
		wantAbsent  []string
	}{
		"private repo: allowForking is the org's, never ours to write": {
			repo:        registry.Resolved{Visibility: registry.VisibilityPrivate},
			wantContain: []string{fieldAutoInit, "allowForking"},
			// Not archived, so the profile's other fields stay managed.
			wantAbsent: []string{"visibility", "description", "allowAutoMerge"},
		},
		"public repo: allowForking stays managed": {
			repo:        registry.Resolved{Visibility: "public"},
			wantContain: []string{fieldAutoInit},
			// Public repos are always forkable and GitHub does not allow
			// turning that off, so the live value is the only possible one —
			// ignoring it would hide drift we can actually see.
			wantAbsent: []string{"allowForking"},
		},
		"archived: everything but archived itself": {
			repo: registry.Resolved{Visibility: "public", Archived: true},
			wantContain: []string{
				fieldAutoInit, "visibility", "description", "hasIssues", "hasWiki",
				"hasProjects", "allowAutoMerge", "allowSquashMerge",
				"allowMergeCommit", "allowRebaseMerge", "allowUpdateBranch",
				"deleteBranchOnMerge", "allowForking",
			},
			// `archived` must stay writable — it is the one field the engine
			// still owns, and ignoring it would make archival inexpressible.
			wantAbsent: []string{"archived"},
		},
		"archived AND private: both rules, no duplicate": {
			repo:        registry.Resolved{Visibility: registry.VisibilityPrivate, Archived: true},
			wantContain: []string{"allowForking", "visibility"},
			wantAbsent:  []string{"archived"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := repoIgnoredFields(tc.repo)

			for _, want := range tc.wantContain {
				assert.Contains(t, got, want)
			}

			for _, absent := range tc.wantAbsent {
				assert.NotContains(t, got, absent)
			}
		})
	}
}

// A private+archived repo hits both branches, which append allowForking
// twice. Pulumi tolerates it, but a duplicate is a smell that the two rules
// were written without knowing about each other — pin the dedup so a future
// third rule does not quietly multiply.
func TestRepoIgnoredFieldsHasNoDuplicates(t *testing.T) {
	t.Parallel()

	got := repoIgnoredFields(registry.Resolved{
		Visibility: registry.VisibilityPrivate,
		Archived:   true,
	})

	seen := make(map[string]int, len(got))
	for _, f := range got {
		seen[f]++
	}

	for field, n := range seen {
		assert.Equal(t, 1, n, "field %q appears %d times", field, n)
	}
}
