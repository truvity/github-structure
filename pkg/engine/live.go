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
	// billingEmail is the org's CURRENT billing address. Billing is the
	// IT team's, managed in the UI — but the provider's settings
	// resource requires the field, so the engine passes the live value
	// through and never diffs it.
	billingEmail string
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
// Both are one extra apply, not a correctness problem — but at INF-473's
// ~70 repositories they are the difference between "it worked" and "it
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
	}

	if err := state.readTeams(ctx, client, org); err != nil {
		return nil, err
	}

	if err := state.readGrants(ctx, client, org, orgCfg); err != nil {
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

	return state, nil
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
