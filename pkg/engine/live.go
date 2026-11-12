package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	app "github.com/truvity/github-structure/pkg/app"
	registry "github.com/truvity/github-structure/pkg/registry"
)

// liveState is what already exists on GitHub, read once before any
// resource is declared.
//
// This is the check half of **check-then-import**
// (docs/guides/add-pulumi-stack.md §2). Without it the program would
// have to be told, out of band, which resources to adopt — and the day
// someone adds a row for a repository that does not exist yet, or
// enables protection on a branch that has none, a hardcoded "import
// everything" would fail at apply with an unhelpful error. Asking
// GitHub costs one round trip per resource kind and removes the whole
// class of problem.
type liveState struct { //nolint:grouper // one type in this file; a group of one reads worse
	// teamIDs maps a team slug to its numeric ID. Several import IDs
	// need the number; the slug is rejected.
	teamIDs map[string]string
	// repos is the set of repositories that already exist.
	repos map[string]bool
	// emptyRepos marks existing repositories with no commits yet.
	emptyRepos map[string]bool
	// grants maps a team slug to the set of repositories it already has
	// access to. A TeamRepository is imported ONLY when the grant itself
	// exists — team-and-repo existing is not enough (2026-08-01: 29 new
	// grants for a freshly created team all failed as bogus imports).
	grants map[string]map[string]bool
	// environments maps a repository to the set of its deployment
	// environments that already exist, read only for a repository whose row
	// declares some. An environment is imported ONLY when it is live.
	environments map[string]map[string]bool
	// envPolicies maps "repo/environment" to the environment's existing
	// deployment policies, by "branch:<pattern>" or "tag:<pattern>", to the
	// policy's numeric id, which is part of its import id.
	envPolicies map[string]map[string]string
	// billingEmail is the org's CURRENT billing address. Billing is the
	// IT team's, managed in the UI — but the provider's settings
	// resource requires the field, so the engine passes the live value
	// through and never diffs it.
	billingEmail string
	// installedApps maps an App slug to its database id, for the Apps
	// installed on this organization — what a ruleset's bypass_apps
	// names resolve against. Read from GitHub rather than declared: the
	// id is GitHub's fact about an App, and a copy of it in a file is
	// stale the moment the App is recreated, silently.
	//
	// Empty when no ruleset names an App: see readInstalledApps.
	installedApps registry.InstalledApps
}

// readLiveState queries GitHub ONCE, before any resource is declared.
//
// Consequence worth knowing when CREATING a repository rather than
// adopting one: the repo's sub-resources (Actions permissions, workflow
// permissions, vulnerability alerts) are settings that exist implicitly
// the moment the repository does — but this snapshot was taken before
// that, so they are planned as creates and the first apply half-fails.
// A second apply sees the repo and imports them correctly.
//
// Likewise branch protection cannot attach to a default branch that has
// no commits yet: push at least an empty root commit before the rule can
// be created.
//
// Both are one extra apply, not a correctness problem — but at a second
// org's ~70 repositories they are the difference between "it worked" and "it
// half-worked twice", so budget for repeat applies there.
func readLiveState(
	ctx context.Context,
	client *app.Client,
	org string,
	orgCfg *registry.Org,
) (*liveState, error) {
	state := &liveState{
		teamIDs:    make(map[string]string, len(orgCfg.Teams)),
		repos:      make(map[string]bool, len(orgCfg.Repos)),
		emptyRepos: make(map[string]bool, len(orgCfg.Repos)),
		grants:     make(map[string]map[string]bool),

		environments: make(map[string]map[string]bool),
		envPolicies:  make(map[string]map[string]string),
	}

	if err := state.readTeams(ctx, client, org); err != nil {
		return nil, err
	}

	if err := state.readGrants(ctx, client, org, orgCfg); err != nil {
		return nil, err
	}

	if err := state.readInstalledApps(ctx, client, org, orgCfg); err != nil {
		return nil, err
	}

	var orgInfo struct {
		BillingEmail string `json:"billing_email"`
	}

	if err := client.Get(ctx, "/orgs/"+org, &orgInfo); err != nil {
		return nil, fmt.Errorf("read org billing email: %w", err)
	}

	state.billingEmail = orgInfo.BillingEmail

	for _, name := range orgCfg.SortedRepos() {
		var repo struct {
			Size int64 `json:"size"`
		}

		err := client.Get(ctx, "/repos/"+org+"/"+name, &repo)

		switch {
		case errors.Is(err, app.ErrNotFound):
			state.repos[name] = false
		case err != nil:
			return nil, fmt.Errorf("check repo %s: %w", name, err)
		default:
			state.repos[name] = true
			// GitHub answers 409 to several settings writes on a repo
			// with no commits; the engine skips those until history
			// arrives (migration targets are created empty).
			state.emptyRepos[name] = repo.Size == 0
		}
	}

	if err := state.readEnvironments(ctx, client, org, orgCfg); err != nil {
		return nil, err
	}

	return state, nil
}

// getter is the one call readEnvironments makes of the API client, so a test
// can answer it.
type getter interface {
	Get(ctx context.Context, path string, dst any) error
}

// readEnvironments reads, for each EXISTING repository that declares
// environments, which of them already exist and which deployment policies
// they hold, so an environment or a policy is imported when it is live and
// created when it is not.
//
// An unreadable answer stops the run: the engine App needs the repository
// permission "Environments" to read and write them, and a missing one answers
// 403 or 404. Reading that as "no environments" would plan creates for
// environments that exist and fail at apply with a conflict.
func (s *liveState) readEnvironments(ctx context.Context, client getter, org string, orgCfg *registry.Org) error {
	for _, name := range orgCfg.SortedRepos() {
		repo := orgCfg.Repos[name]
		if repo == nil || repo.Archived || len(repo.Environments) == 0 || !s.repos[name] {
			continue
		}

		var list struct {
			Environments []struct {
				Name string `json:"name"`
			} `json:"environments"`
		}

		if err := client.Get(ctx, "/repos/"+org+"/"+name+"/environments?per_page=100", &list); err != nil {
			return fmt.Errorf("list environments of repo %s: %w (the engine App needs the repository permission"+
				" \"Environments\": read and write)", name, err)
		}

		live := make(map[string]bool, len(list.Environments))
		for _, e := range list.Environments {
			live[e.Name] = true
		}

		s.environments[name] = live

		for _, env := range repo.SortedEnvironments() {
			if !live[env] || !repo.Environments[env].HasBranchPolicy() {
				continue
			}

			var policies struct {
				BranchPolicies []struct {
					ID   int64  `json:"id"`
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"branch_policies"`
			}

			path := "/repos/" + org + "/" + name + "/environments/" + env + "/deployment-branch-policies?per_page=100"
			if err := client.Get(ctx, path, &policies); err != nil && !errors.Is(err, app.ErrNotFound) {
				return fmt.Errorf("list deployment policies of environment %s of repo %s: %w", env, name, err)
			}

			byPattern := make(map[string]string, len(policies.BranchPolicies))

			for _, p := range policies.BranchPolicies {
				kind := p.Type
				if kind == "" {
					kind = "branch"
				}

				byPattern[kind+":"+p.Name] = strconv.FormatInt(p.ID, 10)
			}

			s.envPolicies[name+"/"+env] = byPattern
		}
	}

	return nil
}

// environmentExists reports whether a repository's environment is live.
func (s *liveState) environmentExists(repo, env string) bool { return s.environments[repo][env] }

// policyID returns the numeric id of a live deployment policy, by its kind
// ("branch" or "tag") and pattern.
func (s *liveState) policyID(repo, env, kind, pattern string) (string, bool) {
	id, ok := s.envPolicies[repo+"/"+env][kind+":"+pattern]

	return id, ok
}

func (s *liveState) readTeams(ctx context.Context, client *app.Client, org string) error {
	var teams []struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	}

	if err := client.Get(ctx, "/orgs/"+org+"/teams?per_page=100", &teams); err != nil {
		if errors.Is(err, app.ErrNotFound) {
			return nil
		}

		return fmt.Errorf("list teams: %w", err)
	}

	for _, t := range teams {
		s.teamIDs[t.Slug] = strconv.FormatInt(t.ID, 10)
	}

	return nil
}

// readGrants reads each granted-to team's current repositories, so a
// TeamRepository import happens only for grants that actually exist.
func (s *liveState) readGrants(ctx context.Context, client *app.Client, org string, orgCfg *registry.Org) error {
	referenced := map[string]bool{}

	for _, repo := range orgCfg.Repos {
		for slug := range repo.Teams {
			referenced[slug] = true
		}
	}

	for slug := range referenced {
		if _, exists := s.teamIDs[slug]; !exists {
			continue
		}

		var repos []struct {
			Name string `json:"name"`
		}

		if err := client.Get(ctx, "/orgs/"+org+"/teams/"+slug+"/repos?per_page=100", &repos); err != nil {
			if errors.Is(err, app.ErrNotFound) {
				continue
			}

			return fmt.Errorf("list team %s repos: %w", slug, err)
		}

		set := make(map[string]bool, len(repos))
		for _, r := range repos {
			set[r.Name] = true
		}

		s.grants[slug] = set
	}

	return nil
}

// readInstalledApps resolves the org's App installations, but only where
// a ruleset actually names one.
//
// The endpoint needs organization_administration: read, which an estate
// whose rulesets name no bypass App has no other reason to grant — and a
// permission demanded for a fact nobody asked for is a permission that
// gets granted rather than questioned.
func (s *liveState) readInstalledApps(ctx context.Context, client *app.Client, org string, orgCfg *registry.Org) error {
	if !namesBypassApps(orgCfg) {
		return nil
	}

	installed, err := app.InstalledAppIDs(ctx, client, org)
	if err != nil {
		return fmt.Errorf("resolve bypass apps: %w", err)
	}

	s.installedApps = installed

	return nil
}

// namesBypassApps reports whether any ruleset in the org names a bypass
// App at all.
func namesBypassApps(orgCfg *registry.Org) bool {
	for _, repo := range orgCfg.Repos {
		for _, rs := range repo.TagRulesets {
			if len(rs.BypassApps) > 0 {
				return true
			}
		}

		for _, rs := range repo.BranchRulesets {
			if len(rs.BypassApps) > 0 {
				return true
			}
		}
	}

	return false
}

// grantExists reports whether a team already has access to a repository.
func (s *liveState) grantExists(slug, repo string) bool {
	return s.grants[slug][repo]
}

// teamID returns a team's numeric ID and whether it exists live.
func (s *liveState) teamID(slug string) (string, bool) {
	id, ok := s.teamIDs[slug]

	return id, ok
}

// repoExists reports whether a repository is already there.
func (s *liveState) repoExists(name string) bool { return s.repos[name] }

// repoEmpty reports whether an existing repository has no commits yet. A
// repository this run is about to create is empty by definition.
func (s *liveState) repoEmpty(name string) bool { return !s.repos[name] || s.emptyRepos[name] }
