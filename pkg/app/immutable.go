package app

// Immutable releases: the per-repository setting that makes a published
// release's tag and assets unchangeable.
//
// This is a reconciler, not a Pulumi resource, for the same reasons the
// org variables are (see entitlements.go), and one more: the Pulumi GitHub
// provider has no resource for it at all, and the Go SDK has no dynamic
// provider to write one with (that is a Node and Python feature). The
// setting is two REST verbs and a read:
//
//	GET    /repos/{owner}/{repo}/immutable-releases  → {"enabled", "enforced_by_owner"}
//	PUT    /repos/{owner}/{repo}/immutable-releases  → on
//	DELETE /repos/{owner}/{repo}/immutable-releases  → off
//
// so it follows the engine's existing pattern for a setting outside the
// provider: a read-only plan, an idempotent apply that touches only what
// differs, and a drift check that reads the same plan.
//
// The row field is `immutable_releases: true | false`. Unset means NOT
// MANAGED: the repository is not read, not compared and never written. An
// explicit false is a decision, and is applied.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	registry "github.com/truvity/github-structure/pkg/registry"
)

const fieldImmutableReleases = "immutable_releases"

type (
	// ImmutableChange is one repository whose live setting differs from
	// its row.
	ImmutableChange struct {
		Repo string
		// Want is the declared value, Got the live one.
		Want, Got bool
		// EnforcedByOwner is GitHub's flag that the organization (or
		// enterprise) enforces the setting on, so a per-repository DELETE
		// is refused. It only blocks a Want of false.
		EnforcedByOwner bool
	}

	// ImmutablePlan is what ApplyImmutableReleases will do. It is data:
	// reading it writes nothing.
	ImmutablePlan struct {
		Org string
		// Changes are the repositories to switch, in registry order.
		Changes []ImmutableChange
		// InSync are declared repositories already at their value.
		InSync []string
		// Unreadable are declared repositories GitHub answered 404 for: the
		// repository is missing, or the token lacks admin read on it. They
		// are never written.
		Unreadable []string
	}

	liveImmutableReleases struct {
		Enabled         bool `json:"enabled"`
		EnforcedByOwner bool `json:"enforced_by_owner"`
	}
)

// Blocked are the changes GitHub will refuse: the declared value is off,
// and the owner enforces it on.
func (p ImmutablePlan) Blocked() []ImmutableChange {
	var out []ImmutableChange

	for _, ch := range p.Changes {
		if !ch.Want && ch.EnforcedByOwner {
			out = append(out, ch)
		}
	}

	return out
}

// Empty reports whether applying the plan would write nothing.
func (p ImmutablePlan) Empty() bool { return len(p.Changes) == 0 }

// Lines renders the plan, one line per repository that would change, a
// blocked one marked as such, and one per repository that could not be
// read.
func (p ImmutablePlan) Lines() []string {
	lines := make([]string, 0, len(p.Changes)+len(p.Unreadable))

	for _, ch := range p.Changes {
		line := fmt.Sprintf("repo %s: %s: %s -> %s", ch.Repo, fieldImmutableReleases, onOff(ch.Got), onOff(ch.Want))
		if !ch.Want && ch.EnforcedByOwner {
			line += " (blocked: enforced by the organization)"
		}

		lines = append(lines, line)
	}

	for _, repo := range p.Unreadable {
		lines = append(lines, fmt.Sprintf("repo %s: %s: not readable (missing, or the token has no admin read), not changed",
			repo, fieldImmutableReleases))
	}

	return lines
}

// Summary is a one-line count for humans.
func (p ImmutablePlan) Summary() string {
	return fmt.Sprintf("%d to change, %d in sync, %d not readable", len(p.Changes), len(p.InSync), len(p.Unreadable))
}

func onOff(b bool) string {
	if b {
		return "on"
	}

	return "off"
}

// PlanImmutableReleases reads the live setting of every declared,
// non-archived repository whose row states `immutable_releases`, and
// returns the switches needed to make GitHub match. It never writes. A
// repository whose row leaves the field unset is not read at all.
func PlanImmutableReleases(ctx context.Context, org string, cfg *registry.Config) (ImmutablePlan, error) {
	plan := ImmutablePlan{Org: org}

	orgCfg, ok := cfg.Orgs[org]
	if !ok {
		return plan, nil
	}

	for _, name := range orgCfg.SortedRepos() {
		repo := orgCfg.Repos[name]
		if repo.ImmutableReleases == nil || repo.Archived {
			continue
		}

		var live liveImmutableReleases

		err := ghAPI(ctx, immutableReleasesPath(org, name), &live)

		switch {
		case errors.Is(err, ErrNotFound):
			plan.Unreadable = append(plan.Unreadable, name)

			continue
		case err != nil:
			return ImmutablePlan{}, fmt.Errorf("repo %s: %w", name, err)
		}

		if live.Enabled == *repo.ImmutableReleases {
			plan.InSync = append(plan.InSync, name)

			continue
		}

		plan.Changes = append(plan.Changes, ImmutableChange{
			Repo:            name,
			Want:            *repo.ImmutableReleases,
			Got:             live.Enabled,
			EnforcedByOwner: live.EnforcedByOwner,
		})
	}

	return plan, nil
}

// CheckImmutableReleases is the drift view of the plan: one finding per
// repository whose live setting differs from its row, and one not-visible
// line per repository GitHub would not let the token read. A repository
// that does not exist is CheckRepoSettings' finding, so the 404 here is
// not counted a second time.
func CheckImmutableReleases(ctx context.Context, org string, cfg *registry.Config) ([]Drift, error) {
	plan, err := PlanImmutableReleases(ctx, org, cfg)
	if err != nil {
		return nil, err
	}

	var drifts []Drift

	for _, ch := range plan.Changes {
		got := strconv.FormatBool(ch.Got)
		if ch.EnforcedByOwner {
			got += " (enforced by the organization)"
		}

		drifts = append(drifts, Drift{
			Subject: "repo " + ch.Repo,
			Field:   fieldImmutableReleases,
			Want:    strconv.FormatBool(ch.Want),
			Got:     got,
		})
	}

	for _, repo := range plan.Unreadable {
		drifts = append(drifts, Drift{Subject: "repo " + repo, Field: fieldImmutableReleases, NotVisible: true})
	}

	return drifts, nil
}

// ApplyImmutableReleases performs a plan: PUT where it says on, DELETE
// where it says off, one repository at a time, and returns a line per
// write. It refuses, before writing anything, a plan that holds a change
// GitHub would refuse (an off the organization enforces on): a half-applied
// plan with a known failure in it is worse than none. Equal repositories
// are not in the plan, so running it again writes nothing.
func ApplyImmutableReleases(ctx context.Context, org string, plan ImmutablePlan) ([]string, error) {
	if blocked := plan.Blocked(); len(blocked) > 0 {
		names := make([]string, 0, len(blocked))
		for _, ch := range blocked {
			names = append(names, ch.Repo)
		}

		return nil, fmt.Errorf("%s: declared off, but the organization enforces immutable releases on: %s;"+
			" nothing was written", fieldImmutableReleases, strings.Join(names, ", "))
	}

	applied := make([]string, 0, len(plan.Changes))

	for _, ch := range plan.Changes {
		method := "DELETE"
		if ch.Want {
			method = "PUT"
		}

		if err := ghAPINoBody(ctx, method, immutableReleasesPath(org, ch.Repo)); err != nil {
			return applied, fmt.Errorf("set %s for %s: %w", fieldImmutableReleases, ch.Repo, err)
		}

		applied = append(applied, fmt.Sprintf("repo %s: %s: %s -> %s", ch.Repo, fieldImmutableReleases, onOff(ch.Got), onOff(ch.Want)))
	}

	return applied, nil
}

func immutableReleasesPath(org, repo string) string {
	return "/repos/" + org + "/" + repo + "/immutable-releases"
}

// ghAPINoBody is ghAPIWrite for a verb that takes no body: the enable and
// disable calls answer 204 and reject nothing they were not sent.
func ghAPINoBody(ctx context.Context, method, path string) error {
	cmd := exec.CommandContext(ctx, "gh", "api", "-X", method, path)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh api %s %s: %w: %s", method, path, err, strings.TrimSpace(stderr.String()))
	}

	return nil
}
