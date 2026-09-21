package registry

// GitHub's own new-repository defaults, as the layer every preset is a
// diff against.
//
// WHY A BASE AT ALL. A preset used to be required to set all 33 fields,
// so that a repository could never resolve to a half-specified resource
// — an unset field in the engine means UNMANAGED, and an unmanaged field
// is one somebody changes in the UI while nothing changes it back. That
// requirement is still met, and in the same way: a preset resolves
// through this base, so every field is set, every field is applied, and
// nothing is unmanaged. What changed is only which of the 33 a human has
// to write down.
//
// So a preset now says what is TRUE OF THIS ESTATE and nothing else. A
// reader who wants to know whether `has_downloads` is on does not learn
// it from a preset that repeats GitHub's answer; they learn it from the
// resolved state, which is where it was always worth reading.
//
// THIS TABLE IS A CONSTANT, NOT A LOOKUP. It is what GitHub creates a
// repository with as of 2026-09-21, asserted here rather than fetched —
// so if GitHub changes a default tomorrow, nothing an estate applies
// moves: the engine still sets every field from the resolved state, and
// the resolved state still comes from here. The cost of being wrong is
// therefore a preset that reads as if it inherits one thing while
// inheriting another, which a reader can catch, rather than a live
// setting that silently drifts, which nobody can.
//
// Protection is the one entry that is not a value but an absence:
// GitHub creates a repository with NO branch protection rule at all, so
// the base disables it and every field under it is inert until a preset
// turns it on. That is why `enabled: false` sits beside a full set of
// rule fields — the fields have to exist for a preset to be able to
// diff against them.
func gitHubDefaults() *RepoSettings {
	return &RepoSettings{
		// A repository is created private unless asked otherwise; the
		// estate's public repos say so explicitly.
		Visibility: ptr("private"),
		// Issues, wiki and projects are on; downloads and forking follow
		// visibility (forking is off for private repos by default).
		HasIssues:    ptr(true),
		HasWiki:      ptr(true),
		HasProjects:  ptr(true),
		HasDownloads: ptr(true),
		AllowForking: ptr(false),
		// Merge, squash and rebase are all offered; auto-merge is not,
		// the branch is not deleted after a merge, and the "update
		// branch" button is off.
		AllowMergeCommit:    ptr(true),
		AllowSquashMerge:    ptr(true),
		AllowRebaseMerge:    ptr(true),
		AllowAutoMerge:      ptr(false),
		DeleteBranchOnMerge: ptr(false),
		AllowUpdateBranch:   ptr(false),
		// GitHub's default initial branch since 2020.
		DefaultBranch: ptr("main"),
		Actions: &ActionsSettings{
			AllowedActions: ptr("all"),
			// Read-only since 2023: the safe half of the change GitHub
			// made after the write-by-default era.
			DefaultWorkflowPermissions: ptr("read"),
			CanApprovePullRequests:     ptr(false),
		},
		// No rule exists on a new repository. Everything below it is the
		// shape a preset fills in once it sets enabled: true.
		Protection: &ProtectionSettings{
			Enabled:                 ptr(false),
			RequiredChecks:          ptr([]string{}),
			Strict:                  ptr(false),
			EnforceAdmins:           ptr(false),
			RequiredApprovals:       ptr(0),
			DismissStaleReviews:     ptr(false),
			RequireCodeOwnerReviews: ptr(false),
			RequireConvResolution:   ptr(false),
			RequireLinearHistory:    ptr(false),
			RequireSignatures:       ptr(false),
			AllowForcePushes:        ptr(false),
			AllowDeletions:          ptr(false),
		},
		// Review is deliberately absent: it is not a GitHub setting but
		// this registry's vocabulary for the approval half of the gate,
		// and a base that guessed would state a gate the engine renders.
	}
}

func ptr[T any](v T) *T { return &v }
