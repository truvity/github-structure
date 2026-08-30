package registry

import (
	"sort"
)

type (
	// Resolved is a repo's fully-determined settings: the profile with
	// the repo's overrides layered on top. Every field is concrete — the
	// Pulumi program never sees a pointer and never has to guess a
	// default.
	Resolved struct {
		Visibility          string
		HasIssues           bool
		HasWiki             bool
		HasProjects         bool
		AllowAutoMerge      bool
		AllowSquashMerge    bool
		AllowMergeCommit    bool
		AllowRebaseMerge    bool
		DeleteBranchOnMerge bool
		AllowUpdateBranch   bool
		AllowForking        bool
		HasDownloads        bool
		DefaultBranch       string
		Description         string

		// Archived is the repo row's own flag, never a profile's: a
		// retirement is a fact about one repository, not a class of
		// them. When set, every field above is inert — the engine
		// ignores them rather than planning writes GitHub will reject.
		Archived bool

		Actions    ResolvedActions
		Protection ResolvedProtection

		// Teams is the union of the repo's access bundles merged with its
		// own grants. A
		// repo grant wins over the profile's for the same team.
		Teams map[string]string
	}

	// ResolvedActions is a repo's concrete Actions policy.
	ResolvedActions struct {
		AllowedActions             string
		DefaultWorkflowPermissions string
		CanApprovePullRequests     bool
	}

	// ResolvedProtection is a repo's concrete branch-protection rule.
	// Enabled:false means no rule exists at all.
	ResolvedProtection struct {
		Enabled                 bool
		RequiredChecks          []string
		Strict                  bool
		EnforceAdmins           bool
		RequiredApprovals       int
		PullRequestBypassers    []string
		DismissStaleReviews     bool
		RequireCodeOwnerReviews bool
		RequireConvResolution   bool
		RequireLinearHistory    bool
		RequireSignatures       bool
		AllowForcePushes        bool
		AllowDeletions          bool
	}
)

// Resolve layers a repo's overrides over its preset. Callers must have
// validated the config first (Load does); an unknown preset resolves to
// the zero value rather than panicking.
func (c *Config) Resolve(repo *Repo) Resolved {
	preset, ok := c.Presets[repo.Preset]
	if !ok {
		return Resolved{}
	}

	merged := *preset
	merged.overlay(repo.Overrides)

	// A waiver drops the preset's required checks for this repo. The
	// profile keeps declaring them, so the intent stays visible and
	// lifting the waiver is a one-line deletion.
	if repo.ChecksWaived != "" && merged.Protection != nil {
		clone := cloneProtection(merged.Protection)
		clone.RequiredChecks = &[]string{}
		merged.Protection = clone
	}

	out := Resolved{
		Visibility:          derefString(merged.Visibility),
		HasIssues:           derefBool(merged.HasIssues),
		HasWiki:             derefBool(merged.HasWiki),
		HasProjects:         derefBool(merged.HasProjects),
		AllowAutoMerge:      derefBool(merged.AllowAutoMerge),
		AllowSquashMerge:    derefBool(merged.AllowSquashMerge),
		AllowMergeCommit:    derefBool(merged.AllowMergeCommit),
		AllowRebaseMerge:    derefBool(merged.AllowRebaseMerge),
		DeleteBranchOnMerge: derefBool(merged.DeleteBranchOnMerge),
		AllowUpdateBranch:   derefBool(merged.AllowUpdateBranch),
		AllowForking:        derefBool(merged.AllowForking),
		HasDownloads:        derefBool(merged.HasDownloads),
		DefaultBranch:       derefString(merged.DefaultBranch),
		Description:         repo.Description,
		Archived:            repo.Archived,
		Teams:               c.effectiveTeams(repo),
	}

	if merged.Actions != nil {
		out.Actions = ResolvedActions{
			AllowedActions:             derefString(merged.Actions.AllowedActions),
			DefaultWorkflowPermissions: derefString(merged.Actions.DefaultWorkflowPermissions),
			CanApprovePullRequests:     derefBool(merged.Actions.CanApprovePullRequests),
		}
	}

	if merged.Protection != nil {
		out.Protection = ResolvedProtection{
			Enabled:                 derefBool(merged.Protection.Enabled),
			RequiredChecks:          derefStrings(merged.Protection.RequiredChecks),
			Strict:                  derefBool(merged.Protection.Strict),
			EnforceAdmins:           derefBool(merged.Protection.EnforceAdmins),
			RequiredApprovals:       derefInt(merged.Protection.RequiredApprovals),
			PullRequestBypassers:    derefStrings(merged.Protection.PullRequestBypassers),
			DismissStaleReviews:     derefBool(merged.Protection.DismissStaleReviews),
			RequireCodeOwnerReviews: derefBool(merged.Protection.RequireCodeOwnerReviews),
			RequireConvResolution:   derefBool(merged.Protection.RequireConvResolution),
			RequireLinearHistory:    derefBool(merged.Protection.RequireLinearHistory),
			RequireSignatures:       derefBool(merged.Protection.RequireSignatures),
			AllowForcePushes:        derefBool(merged.Protection.AllowForcePushes),
			AllowDeletions:          derefBool(merged.Protection.AllowDeletions),
		}
	}

	return out
}

// ResolveRepo is the by-name form of Resolve.
func (c *Config) ResolveRepo(login, name string) (Resolved, bool) {
	org, ok := c.Orgs[login]
	if !ok {
		return Resolved{}, false
	}

	repo, ok := org.Repos[name]
	if !ok {
		return Resolved{}, false
	}

	return c.Resolve(repo), true
}

// overlay copies every non-nil field of src over the receiver. The
// receiver is a shallow copy of a profile, so nested structs are cloned
// before mutation — a profile must never be modified by resolving a repo.
func (s *RepoSettings) overlay(src *RepoSettings) {
	if src == nil {
		return
	}

	overwrite(&s.Visibility, src.Visibility)
	overwrite(&s.HasIssues, src.HasIssues)
	overwrite(&s.HasWiki, src.HasWiki)
	overwrite(&s.HasProjects, src.HasProjects)
	overwrite(&s.AllowAutoMerge, src.AllowAutoMerge)
	overwrite(&s.AllowSquashMerge, src.AllowSquashMerge)
	overwrite(&s.AllowMergeCommit, src.AllowMergeCommit)
	overwrite(&s.AllowRebaseMerge, src.AllowRebaseMerge)
	overwrite(&s.DeleteBranchOnMerge, src.DeleteBranchOnMerge)
	overwrite(&s.AllowUpdateBranch, src.AllowUpdateBranch)
	overwrite(&s.AllowForking, src.AllowForking)
	overwrite(&s.HasDownloads, src.HasDownloads)
	overwrite(&s.DefaultBranch, src.DefaultBranch)

	if src.Actions != nil {
		clone := cloneActions(s.Actions)
		overwrite(&clone.AllowedActions, src.Actions.AllowedActions)
		overwrite(&clone.DefaultWorkflowPermissions, src.Actions.DefaultWorkflowPermissions)
		overwrite(&clone.CanApprovePullRequests, src.Actions.CanApprovePullRequests)
		s.Actions = clone
	}

	if src.Protection != nil {
		clone := cloneProtection(s.Protection)
		overwrite(&clone.Enabled, src.Protection.Enabled)
		overwrite(&clone.RequiredChecks, src.Protection.RequiredChecks)
		overwrite(&clone.Strict, src.Protection.Strict)
		overwrite(&clone.EnforceAdmins, src.Protection.EnforceAdmins)
		overwrite(&clone.RequiredApprovals, src.Protection.RequiredApprovals)
		overwrite(&clone.PullRequestBypassers, src.Protection.PullRequestBypassers)
		overwrite(&clone.DismissStaleReviews, src.Protection.DismissStaleReviews)
		overwrite(&clone.RequireCodeOwnerReviews, src.Protection.RequireCodeOwnerReviews)
		overwrite(&clone.RequireConvResolution, src.Protection.RequireConvResolution)
		overwrite(&clone.RequireLinearHistory, src.Protection.RequireLinearHistory)
		overwrite(&clone.RequireSignatures, src.Protection.RequireSignatures)
		overwrite(&clone.AllowForcePushes, src.Protection.AllowForcePushes)
		overwrite(&clone.AllowDeletions, src.Protection.AllowDeletions)
		s.Protection = clone
	}
}

// missingFields lists the fields a profile failed to set. Profiles must
// be complete; this is what makes that check readable.
func (s *RepoSettings) missingFields() []string {
	var missing []string

	check := func(name string, set bool) {
		if !set {
			missing = append(missing, name)
		}
	}

	check("visibility", s.Visibility != nil)
	check("has_issues", s.HasIssues != nil)
	check("has_wiki", s.HasWiki != nil)
	check("has_projects", s.HasProjects != nil)
	check("allow_auto_merge", s.AllowAutoMerge != nil)
	check("allow_squash_merge", s.AllowSquashMerge != nil)
	check("allow_merge_commit", s.AllowMergeCommit != nil)
	check("allow_rebase_merge", s.AllowRebaseMerge != nil)
	check("delete_branch_on_merge", s.DeleteBranchOnMerge != nil)
	check("allow_update_branch", s.AllowUpdateBranch != nil)
	check("allow_forking", s.AllowForking != nil)
	check("has_downloads", s.HasDownloads != nil)
	check("default_branch", s.DefaultBranch != nil)

	if s.Actions == nil {
		missing = append(missing, "actions")
	} else {
		check("actions.allowed_actions", s.Actions.AllowedActions != nil)
		check("actions.default_workflow_permissions", s.Actions.DefaultWorkflowPermissions != nil)
		check("actions.can_approve_pull_request_reviews", s.Actions.CanApprovePullRequests != nil)
	}

	if s.Protection == nil {
		missing = append(missing, "protection")

		return missing
	}

	check("protection.enabled", s.Protection.Enabled != nil)
	check("protection.required_checks", s.Protection.RequiredChecks != nil)
	check("protection.strict", s.Protection.Strict != nil)
	check("protection.enforce_admins", s.Protection.EnforceAdmins != nil)
	check("protection.required_approvals", s.Protection.RequiredApprovals != nil)
	check("protection.dismiss_stale_reviews", s.Protection.DismissStaleReviews != nil)
	check("protection.require_code_owner_reviews", s.Protection.RequireCodeOwnerReviews != nil)
	check("protection.require_conversation_resolution", s.Protection.RequireConvResolution != nil)
	check("protection.require_linear_history", s.Protection.RequireLinearHistory != nil)
	check("protection.require_signatures", s.Protection.RequireSignatures != nil)
	check("protection.allow_force_pushes", s.Protection.AllowForcePushes != nil)
	check("protection.allow_deletions", s.Protection.AllowDeletions != nil)

	return missing
}

// ── iteration order helpers ────────────────────────────────────────────
// Every map that feeds Pulumi resource creation is iterated in sorted
// order; unstable order means churning URNs.

// SortedOrgs returns org logins in deterministic order.
func (c *Config) SortedOrgs() []string { return sortedKeys(c.Orgs) }

// SortedTeams returns team slugs in deterministic order.
func (o *Org) SortedTeams() []string { return sortedKeys(o.Teams) }

// SortedRepos returns repo names in deterministic order.
func (o *Org) SortedRepos() []string { return sortedKeys(o.Repos) }

// SortedApps returns App names in deterministic order.
func (o *Org) SortedApps() []string { return sortedKeys(o.Apps) }

// SortedRunnerGroups returns runner-group names in deterministic order.
func (o *Org) SortedRunnerGroups() []string { return sortedKeys(o.RunnerGroups) }

// ChecksWaived lists the repos currently running without their
// profile's required checks, with the reason for each. Enumerable on
// purpose: an exception nobody can list is an exception nobody revisits.
func (o *Org) ChecksWaived() map[string]string {
	waived := make(map[string]string)

	for _, name := range o.SortedRepos() {
		if reason := o.Repos[name].ChecksWaived; reason != "" {
			waived[name] = reason
		}
	}

	return waived
}

// OwnedApps returns the Apps we author (external rows excluded), sorted.
func (o *Org) OwnedApps() []string {
	var names []string

	for _, name := range o.SortedApps() {
		if !o.Apps[name].External {
			names = append(names, name)
		}
	}

	return names
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

// effectiveTeams is the union of the repo's access bundles, with the
// repo's own grants layered last so a row can always tighten or override
// what a bundle gives. Bundle order is irrelevant: bundles are disjoint by
// construction, and any overlap would be identical grants anyway.
func (c *Config) effectiveTeams(repo *Repo) map[string]string {
	out := map[string]string{}

	for _, bundle := range repo.Access {
		for team, perm := range c.Access[bundle] {
			out[team] = perm
		}
	}

	return mergeTeams(out, repo.Teams)
}

func mergeTeams(profile, repo map[string]string) map[string]string {
	out := make(map[string]string, len(profile)+len(repo))

	for team, perm := range profile {
		out[team] = perm
	}

	for team, perm := range repo {
		out[team] = perm
	}

	return out
}

func cloneActions(a *ActionsSettings) *ActionsSettings {
	if a == nil {
		return &ActionsSettings{}
	}

	clone := *a

	return &clone
}

func cloneProtection(p *ProtectionSettings) *ProtectionSettings {
	if p == nil {
		return &ProtectionSettings{}
	}

	clone := *p

	return &clone
}

func overwrite[T any](dst **T, src *T) {
	if src != nil {
		*dst = src
	}
}

func derefBool(p *bool) bool {
	if p == nil {
		return false
	}

	return *p
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}

	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}

	return *p
}

func derefStrings(p *[]string) []string {
	if p == nil {
		return nil
	}

	return *p
}

// ResolveEntitlementScope renders a derived scope into the sorted repo
// list it entitles within one org: every non-archived repo carrying
// DerivePreset, plus the explicit additions, deduplicated. A nil scope
// resolves to nil — "declared selected with no scope" is CheckOrgVariables'
// pre-INF-580 behavior (visibility checked, membership not).
func (c *Config) ResolveEntitlementScope(login string, s *EntitlementScope) []string {
	if s == nil {
		return nil
	}

	org, ok := c.Orgs[login]
	if !ok {
		return nil
	}

	set := map[string]bool{}

	if s.DerivePreset != "" {
		for name, repo := range org.Repos {
			if repo.Archived {
				continue
			}

			if repo.Preset == s.DerivePreset {
				set[name] = true
			}
		}
	}

	for _, name := range s.Repos {
		set[name] = true
	}

	return sortedKeys(set)
}
