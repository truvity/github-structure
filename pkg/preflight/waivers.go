package preflight

import (
	"context"
	"errors"
	"fmt"

	app "github.com/truvity/github-structure/pkg/app"
	registry "github.com/truvity/github-structure/pkg/registry"
)

type (
	// waiverProbe is one waived repo together with what the waiver
	// suppresses — everything the API half needs to decide whether the
	// waiver has outlived its cause.
	waiverProbe struct {
		Repo string
		// DefaultBranch is where evidence is sought: a suppressed
		// context reporting on its HEAD means the CI the waiver was
		// waiting for has landed.
		DefaultBranch string
		// SuppressedChecks are the contexts the profile requires and
		// the waiver switches off.
		SuppressedChecks []string
		Reason           string
	}
)

// waiverProbes returns an org's waived repos with the checks their
// waivers suppress.
//
// Split out from checkStaleWaivers so the selection is testable: a rule
// that silently selects nothing passes every run and protects nothing.
func waiverProbes(reg *registry.Config, org string) []waiverProbe {
	orgCfg, ok := reg.Orgs[org]
	if !ok {
		return nil
	}

	var probes []waiverProbe

	for _, name := range orgCfg.SortedRepos() {
		repo := orgCfg.Repos[name]
		if repo.ChecksWaived == "" {
			continue
		}

		// Resolve as if the waiver were lifted. The profile keeps
		// declaring the checks — that is the point of the waiver
		// design — so clearing the field on a copy recovers exactly
		// what deleting the waiver line would require.
		lifted := *repo
		lifted.ChecksWaived = ""
		resolved := reg.Resolve(&lifted)

		// A waiver on a profile that requires nothing is rejected by
		// the loader, so this cannot happen on a validated registry —
		// but a probe with nothing to probe would only produce noise.
		if len(resolved.Protection.RequiredChecks) == 0 {
			continue
		}

		probes = append(probes, waiverProbe{
			Repo:             name,
			DefaultBranch:    resolved.DefaultBranch,
			SuppressedChecks: resolved.Protection.RequiredChecks,
			Reason:           repo.ChecksWaived,
		})
	}

	return probes
}

// checkStaleWaivers fails when a waived repo's suppressed checks are
// already reporting on its default branch. A waiver exists because
// requiring a context no workflow produces blocks every PR forever; a
// context that IS produced means the cause is gone, and the waiver is
// now switching off a gate everyone believes to be on.
//
// The loader catches the opposite direction (waiver on a profile that
// requires nothing). This direction needs GitHub: the gitops waiver sat
// stale in exactly this way on 2026-08-13 — ci.yaml landed with the ARC
// un-pause, and nobody noticed until a human went looking for the
// auto-merge button.
//
// Evidence is the default branch's HEAD only. A CI contract that runs
// exclusively on pull requests would go undetected — acceptable for
// now: every waiver contract to date (bar, gitops) runs on push too,
// and scanning PR heads can come when a repo actually has that shape.
func checkStaleWaivers(ctx context.Context, reg *registry.Config, org string) ([]string, error) {
	var problems []string

	for _, probe := range waiverProbes(reg, org) {
		reported, err := reportedChecks(ctx, org, probe.Repo, probe.DefaultBranch)
		if err != nil {
			// A repo declared in the registry but not yet created answers 404,
			// and that is the normal state of the very run that creates it.
			// Failing here made a new repo undeployable: it has no CI, so it
			// must carry `checks_waived`, which made this guard probe a repo
			// that does not exist — the waiver the repo needs is what stopped
			// the repo being made. A repo with no commits reports no checks,
			// so the honest answer to "has the waived check landed?" is no.
			if noEvidence(err) {
				continue
			}

			return nil, err
		}

		if landed := reportedSuppressed(probe.SuppressedChecks, reported); len(landed) > 0 {
			problems = append(problems, fmt.Sprintf(
				"repo %q: checks_waived (%q), but %v already reports on %s — "+
					"the waiver has outlived its cause and should be deleted",
				probe.Repo, probe.Reason, landed, probe.DefaultBranch))
		}
	}

	return problems, nil
}

// noEvidence reports whether a probe failed because there is nothing to
// look at yet, rather than because the probe itself is broken.
//
// Two shapes, both of them information: a repository the registry
// declares but GitHub does not have yet answers 404, and an EMPTY
// repository — declared, created, never pushed to — answers 422 "No
// commit found for SHA" for its own default branch. Both mean "no checks
// have reported here", which is the honest input to either guard. The
// second was found by running this guard against a real estate: one
// empty repository failed the whole organization's preflight.
func noEvidence(err error) bool {
	return errors.Is(err, app.ErrNotFound) || errors.Is(err, app.ErrNoCommit)
}

// requiredChecksOf returns the contexts a repo's default-branch rule
// would require — from classic protection, or from the ruleset a
// `review: required` row renders where there is no classic rule. It is
// the engine's own choice, read back: a guard that probes a different
// set than the engine writes protects nothing.
func requiredChecksOf(resolved registry.Resolved) []string {
	if resolved.Archived {
		return nil
	}

	if resolved.Protection.Enabled {
		return resolved.Protection.RequiredChecks
	}

	if resolved.Review == registry.ReviewRequired {
		return resolved.Protection.RequiredChecks
	}

	return nil
}

// checkUnreportedChecks fails when a repo REQUIRES a status context that
// nothing has ever produced on its default branch.
//
// This is the failure the waiver mechanism exists for, caught before it
// happens instead of after: a required context no workflow emits blocks
// every pull request on that repository forever, and it reads as "CI has
// not run yet" rather than as a failure — three estate repos sat
// unmergeable that way on 2026-08-15, which is what bought the
// `checks_waived` escape hatch. A repository whose CI has not landed
// must say so in writing (`checks_waived:` with its exit condition);
// this guard is what makes "must" true.
//
// The exit is deliberately generous: ANY of the required contexts having
// reported clears the repo, in either of two windows: the default
// branch's HEAD, and the head of the latest pull request (see below). A
// repo that does not exist yet, or that has no commits at all, is
// skipped — the run that creates a repository is not the run that can
// prove its CI.
func checkUnreportedChecks(ctx context.Context, reg *registry.Config, org string) ([]string, error) {
	orgCfg, ok := reg.Orgs[org]
	if !ok {
		return nil, nil
	}

	var problems []string

	for _, name := range orgCfg.SortedRepos() {
		repo := orgCfg.Repos[name]
		if repo.ChecksWaived != "" {
			continue // the waiver is the written admission; checkStaleWaivers judges it
		}

		resolved := reg.Resolve(repo)

		required := requiredChecksOf(resolved)
		if len(required) == 0 {
			continue
		}

		reported, err := reportedChecks(ctx, org, name, resolved.DefaultBranch)
		if err != nil {
			if noEvidence(err) {
				continue
			}

			return nil, err
		}

		// Second window: the most recent pull request's head. A required
		// context is satisfied on a PR, and a repository whose CI fans
		// in ONLY on pull_request reports nothing under that name on
		// master — four public repositories are in exactly that shape,
		// and probing the branch alone called every one of them broken.
		if len(reportedSuppressed(required, reported)) == 0 {
			pullReported, err := reportedOnLatestPull(ctx, org, name)
			if err != nil {
				return nil, err
			}

			reported = append(reported, pullReported...)
		}

		if len(reportedSuppressed(required, reported)) == 0 {
			problems = append(problems, fmt.Sprintf(
				"repo %q: requires %v, but nothing has reported it on %s or on its latest pull request — "+
					"a required context no workflow produces blocks every pull request forever. Land the "+
					"workflow, or say so in writing with checks_waived (delete it when the context reports)",
				name, required, resolved.DefaultBranch))
		}
	}

	return problems, nil
}

// reportedOnLatestPull lists the check-run names on the head of the
// repository's most recently updated pull request — the window where a
// PR-only CI contract is visible at all. A repository that has never
// had a pull request, or whose head commit is gone, reports nothing,
// which is the honest answer to "does this context exist?".
func reportedOnLatestPull(ctx context.Context, org, repo string) ([]string, error) {
	var pulls []struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}

	path := fmt.Sprintf("/repos/%s/%s/pulls?state=all&sort=updated&direction=desc&per_page=1", org, repo)
	if err := app.API(ctx, path, &pulls); err != nil {
		if noEvidence(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("read pull requests of %s/%s: %w", org, repo, err)
	}

	if len(pulls) == 0 || pulls[0].Head.SHA == "" {
		return nil, nil
	}

	reported, err := reportedChecks(ctx, org, repo, pulls[0].Head.SHA)
	if err != nil {
		if noEvidence(err) {
			return nil, nil
		}

		return nil, err
	}

	return reported, nil
}

// reportedChecks lists the check-run names on a branch's HEAD.
func reportedChecks(ctx context.Context, org, repo, branch string) ([]string, error) {
	var payload struct {
		CheckRuns []struct {
			Name string `json:"name"`
		} `json:"check_runs"`
	}

	path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?per_page=100", org, repo, branch)
	if err := app.API(ctx, path, &payload); err != nil {
		return nil, fmt.Errorf("read check runs of %s/%s@%s: %w", org, repo, branch, err)
	}

	names := make([]string, 0, len(payload.CheckRuns))
	for _, run := range payload.CheckRuns {
		names = append(names, run.Name)
	}

	return names, nil
}

// reportedSuppressed returns the suppressed contexts that are actually
// reporting, in the suppressed list's order. Success or failure of the
// run is irrelevant: a required check is satisfied by the context
// existing and passing, but its EXISTENCE alone proves the workflow
// has landed.
func reportedSuppressed(suppressed, reported []string) []string {
	have := make(map[string]bool, len(reported))
	for _, name := range reported {
		have[name] = true
	}

	var landed []string

	for _, name := range suppressed {
		if have[name] {
			landed = append(landed, name)
		}
	}

	return landed
}
