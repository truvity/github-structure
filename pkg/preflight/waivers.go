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
		var payload struct {
			CheckRuns []struct {
				Name string `json:"name"`
			} `json:"check_runs"`
		}

		path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?per_page=100",
			org, probe.Repo, probe.DefaultBranch)
		if err := app.API(ctx, path, &payload); err != nil {
			// A repo declared in the registry but not yet created answers 404,
			// and that is the normal state of the very run that creates it.
			// Failing here made a new repo undeployable: it has no CI, so it
			// must carry `checks_waived`, which made this guard probe a repo
			// that does not exist — the waiver the repo needs is what stopped
			// the repo being made. A repo with no commits reports no checks,
			// so the honest answer to "has the waived check landed?" is no.
			if errors.Is(err, app.ErrNotFound) {
				continue
			}

			return nil, fmt.Errorf("read check runs of %s/%s@%s: %w",
				org, probe.Repo, probe.DefaultBranch, err)
		}

		reported := make([]string, 0, len(payload.CheckRuns))
		for _, run := range payload.CheckRuns {
			reported = append(reported, run.Name)
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
