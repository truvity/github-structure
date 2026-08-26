package app

// Drift over the BYPASS surfaces: branch-protection review-bypass
// allowances and ruleset bypass actors. These are the two places a
// hand mutation is most tempting (one API PUT unblocks a stuck merge
// or a rejected tag) and most dangerous when undeclared: on 2026-08-26
// a routine `pulumi up --refresh` planned to REMOVE a hand-added App
// bypass from a tag ruleset — which would have silently broken the
// weekly auto-release. The engine can only converge on what the
// registry claims; this check reports everything else.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	registry "github.com/truvity/github-structure/pkg/registry"
)

type (
	liveBypassAllowances struct {
		BypassPullRequestAllowances struct {
			Users []struct {
				Login string `json:"login"`
			} `json:"users"`
			Teams []struct {
				Slug string `json:"slug"`
			} `json:"teams"`
			Apps []struct {
				Slug string `json:"slug"`
			} `json:"apps"`
		} `json:"bypass_pull_request_allowances"`
	}

	liveRuleset struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Target string `json:"target"`
	}

	liveRulesetDetail struct {
		BypassActors []struct {
			ActorID   int64  `json:"actor_id"`
			ActorType string `json:"actor_type"`
		} `json:"bypass_actors"`
	}
)

// CheckBypassSurfaces compares live review-bypass allowances and
// ruleset bypass actors against the registry, repo by repo.
func CheckBypassSurfaces(ctx context.Context, org string, cfg *registry.Config) ([]Drift, error) {
	orgCfg, ok := cfg.Orgs[org]
	if !ok {
		return nil, nil
	}

	teamIDs := map[string]int64{}

	var drifts []Drift

	for _, name := range orgCfg.SortedRepos() {
		repo := orgCfg.Repos[name]
		if repo.Archived {
			continue
		}

		want, resolved := cfg.ResolveRepo(org, name)
		if !resolved {
			continue
		}

		d, err := checkReviewBypasses(ctx, org, name, want)
		if err != nil {
			return nil, err
		}

		drifts = append(drifts, d...)

		d, err = checkRulesetActors(ctx, org, name, repo, teamIDs)
		if err != nil {
			return nil, err
		}

		drifts = append(drifts, d...)
	}

	return drifts, nil
}

func checkReviewBypasses(ctx context.Context, org, name string, want registry.Resolved) ([]Drift, error) {
	branch := want.DefaultBranch
	if branch == "" {
		branch = "master"
	}

	var live liveBypassAllowances

	err := ghAPI(ctx, "/repos/"+org+"/"+name+"/branches/"+branch+"/protection/required_pull_request_reviews", &live)
	if err != nil {
		// No protection, or no review requirement: nothing to bypass.
		// A missing protection object is CheckRepoSettings' concern.
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}

		return nil, err
	}

	got := make([]string, 0, 4)
	for _, u := range live.BypassPullRequestAllowances.Users {
		got = append(got, "/"+u.Login)
	}

	for _, t := range live.BypassPullRequestAllowances.Teams {
		got = append(got, org+"/"+t.Slug)
	}

	for _, a := range live.BypassPullRequestAllowances.Apps {
		got = append(got, "app:"+a.Slug)
	}

	return compareBypassSets(
		"repo "+name, "review bypass allowances",
		normalizeBypassers(org, want.Protection.PullRequestBypassers), got), nil
}

func checkRulesetActors(ctx context.Context, org, name string, repo *registry.Repo, teamIDs map[string]int64) ([]Drift, error) {
	// Declared expectation per ruleset name.
	wantActors := map[string][]string{}

	for _, rs := range repo.TagRulesets {
		wantActors[rs.Name] = declaredTagActors(ctx, org, rs, teamIDs)
	}

	for _, rs := range repo.BranchRulesets {
		wantActors[rs.Name] = declaredBranchActors(rs)
	}

	var live []liveRuleset
	if err := ghAPI(ctx, "/repos/"+org+"/"+name+"/rulesets", &live); err != nil {
		if errors.Is(err, ErrNotFound) {
			live = nil
		} else {
			return nil, err
		}
	}

	var drifts []Drift

	seen := map[string]bool{}

	for _, rs := range live {
		seen[rs.Name] = true

		want, declared := wantActors[rs.Name]
		if !declared {
			// The whole ruleset is undeclared — one line, not an actor
			// diff against an expectation that does not exist.
			drifts = append(drifts, Drift{
				Subject: "repo " + name + " ruleset " + rs.Name,
				Field:   fieldRegistry,
				Want:    wantDeclared,
				Got:     "live but undeclared",
			})

			continue
		}

		var detail liveRulesetDetail
		if err := ghAPI(ctx, "/repos/"+org+"/"+name+"/rulesets/"+strconv.FormatInt(rs.ID, 10), &detail); err != nil {
			return nil, err
		}

		got := make([]string, 0, len(detail.BypassActors))
		for _, a := range detail.BypassActors {
			got = append(got, a.ActorType+":"+strconv.FormatInt(a.ActorID, 10))
		}

		drifts = append(drifts,
			compareBypassSets("repo "+name+" ruleset "+rs.Name, "bypass actors", want, got)...)
	}

	for rsName := range wantActors {
		if !seen[rsName] {
			drifts = append(drifts, Drift{
				Subject: "repo " + name + " ruleset " + rsName,
				Field:   fieldExistence,
				Want:    wantDeclared,
				Got:     gotAbsent,
			})
		}
	}

	return drifts, nil
}

// normalizeBypassers mirrors the engine's qualification rule: a bare
// entry is a user ("/login"); an entry with "/" passes verbatim; a
// team ref is spelled "org/slug" in both places.
func normalizeBypassers(org string, declared []string) []string {
	_ = org

	out := make([]string, 0, len(declared))
	for _, b := range declared {
		if !strings.Contains(b, "/") {
			b = "/" + b
		}

		out = append(out, b)
	}

	return out
}

// declaredTagActors renders a tag ruleset's expected actor set in the
// live vocabulary (Type:id). Team ids are resolved (and cached) via the
// API — the registry speaks slugs, GitHub speaks database ids.
func declaredTagActors(ctx context.Context, org string, rs *registry.TagRuleset, teamIDs map[string]int64) []string {
	actors := make([]string, 0, len(rs.BypassTeams)+len(rs.BypassApps)+1)

	if rs.BypassOrgAdmins {
		actors = append(actors, "OrganizationAdmin:0")
	}

	for _, id := range rs.BypassApps {
		actors = append(actors, "Integration:"+strconv.Itoa(id))
	}

	for _, slug := range rs.BypassTeams {
		id, ok := teamIDs[slug]
		if !ok {
			var team struct {
				ID int64 `json:"id"`
			}

			if err := ghAPI(ctx, "/orgs/"+org+"/teams/"+slug, &team); err == nil {
				id = team.ID
			}

			teamIDs[slug] = id
		}

		actors = append(actors, "Team:"+strconv.FormatInt(id, 10))
	}

	return actors
}

func declaredBranchActors(rs *registry.BranchRuleset) []string {
	actors := make([]string, 0, len(rs.BypassApps)+1)

	if rs.BypassOrgAdmins {
		actors = append(actors, "OrganizationAdmin:0")
	}

	for _, id := range rs.BypassApps {
		actors = append(actors, "Integration:"+strconv.Itoa(id))
	}

	return actors
}

// compareBypassSets reports the symmetric difference of two actor sets
// as drift lines — extras are the hand-add class, absences the
// hand-remove class.
func compareBypassSets(subject, field string, want, got []string) []Drift {
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}

	gotSet := map[string]bool{}
	for _, g := range got {
		gotSet[g] = true
	}

	var missing, extra []string

	for _, w := range want {
		if !gotSet[w] {
			missing = append(missing, w)
		}
	}

	for _, g := range got {
		if !wantSet[g] {
			extra = append(extra, g)
		}
	}

	sort.Strings(missing)
	sort.Strings(extra)

	var drifts []Drift

	if len(missing) > 0 {
		drifts = append(drifts, Drift{
			Subject: subject,
			Field:   field,
			Want:    strings.Join(missing, ", "),
			Got:     gotAbsent,
		})
	}

	if len(extra) > 0 {
		drifts = append(drifts, Drift{
			Subject: subject,
			Field:   field,
			Want:    "not declared",
			Got:     fmt.Sprintf("extra: %s", strings.Join(extra, ", ")),
		})
	}

	return drifts
}
