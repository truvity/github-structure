package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	registry "github.com/truvity/github-structure/pkg/registry"
)

// publicish is the shape a `profile: public` row resolves to, written
// out so each test below can change exactly one field and nothing else.
func publicish() (registry.Resolved, liveRepoSettings) {
	want := registry.Resolved{
		Visibility:          "public",
		Description:         "the install/lease service",
		DefaultBranch:       "master",
		AllowAutoMerge:      true,
		AllowRebaseMerge:    true,
		DeleteBranchOnMerge: true,
		HasIssues:           true,
	}

	live := liveRepoSettings{
		Visibility:          "public",
		Description:         "the install/lease service",
		DefaultBranch:       "master",
		AllowAutoMerge:      true,
		AllowRebaseMerge:    true,
		DeleteBranchOnMerge: true,
		HasIssues:           true,
	}

	return want, live
}

func TestCompareRepoSettingsClean(t *testing.T) {
	want, live := publicish()

	assert.Empty(t, compareRepoSettings("gemaal", want, live))
}

// The case this whole check exists for: the row is merged, the apply
// never ran, so the repository keeps the false it was created with.
// Renovate's platformAutomerge then asks GitHub to arm auto-merge,
// GitHub refuses, and the PR sits green and unmerged (gemaal#39).
func TestCompareRepoSettingsDetectsUndeployedAutoMerge(t *testing.T) {
	want, live := publicish()
	live.AllowAutoMerge = false

	drifts := compareRepoSettings("gemaal", want, live)
	require.Len(t, drifts, 1)

	assert.Equal(t, "repo gemaal", drifts[0].Subject)
	assert.Equal(t, "allow_auto_merge", drifts[0].Field)
	assert.Equal(t, "true", drifts[0].Want)
	assert.Equal(t, "false", drifts[0].Got)
}

// A repository carrying a description the registry does not declare
// LOSES it on the next apply, because the engine writes the field only
// when the row has one. The check is the only warning before the wipe.
func TestCompareRepoSettingsReportsDescriptionAboutToBeWiped(t *testing.T) {
	want, live := publicish()
	want.Description = ""

	drifts := compareRepoSettings("gemaal", want, live)
	require.Len(t, drifts, 1)

	assert.Equal(t, "description", drifts[0].Field)
	assert.Empty(t, drifts[0].Want, "the registry declares none")
	assert.Equal(t, "the install/lease service", drifts[0].Got)
}

// Every compared field must actually be compared. A table this long is
// easy to extend and just as easy to extend WRONG — a copy-pasted row
// that reads the same struct field twice compares nothing and still
// passes a clean-case test.
func TestCompareRepoSettingsComparesEveryDeclaredField(t *testing.T) {
	flip := map[string]func(*liveRepoSettings){
		"visibility":             func(l *liveRepoSettings) { l.Visibility = "private" },
		"description":            func(l *liveRepoSettings) { l.Description = "something else" },
		"default_branch":         func(l *liveRepoSettings) { l.DefaultBranch = "main" },
		"allow_auto_merge":       func(l *liveRepoSettings) { l.AllowAutoMerge = false },
		"allow_squash_merge":     func(l *liveRepoSettings) { l.AllowSquashMerge = true },
		"allow_merge_commit":     func(l *liveRepoSettings) { l.AllowMergeCommit = true },
		"allow_rebase_merge":     func(l *liveRepoSettings) { l.AllowRebaseMerge = false },
		"delete_branch_on_merge": func(l *liveRepoSettings) { l.DeleteBranchOnMerge = false },
		"allow_update_branch":    func(l *liveRepoSettings) { l.AllowUpdateBranch = true },
		"allow_forking":          func(l *liveRepoSettings) { l.AllowForking = true },
		"has_issues":             func(l *liveRepoSettings) { l.HasIssues = false },
		"has_wiki":               func(l *liveRepoSettings) { l.HasWiki = true },
		"has_projects":           func(l *liveRepoSettings) { l.HasProjects = true },
	}

	for field, mutate := range flip {
		t.Run(field, func(t *testing.T) {
			want, live := publicish()
			mutate(&live)

			drifts := compareRepoSettings("gemaal", want, live)
			require.Len(t, drifts, 1, "changing %s must be reported exactly once", field)
			assert.Equal(t, field, drifts[0].Field)
		})
	}
}
