package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"

	app "github.com/truvity/github-structure/pkg/app"
	registry "github.com/truvity/github-structure/pkg/registry"
)

const (
	// teamType is the provider token for a GitHub team resource.
	teamType = "github:index/team:Team"
	// repoType is the provider token for a GitHub repository resource.
	repoType = "github:index/repository:Repository"
	// protectionType is the provider token for a branch protection rule.
	protectionType = "github:index/branchProtection:BranchProtection"
)

// declaredImportID returns the import ID the engine declares for a
// resource, derived the same way deployTeams/deployRepos derive it: the
// URN's resource name minus its prefix.
//
// Only types the engine imports unconditionally are listed. Branch
// protection is absent on purpose — deployProtection does not import at
// all, which is precisely why it is not exposed to this failure.
func declaredImportID(typ, resourceName string) (string, bool) {
	switch typ {
	case teamType:
		return strings.TrimPrefix(resourceName, "team-"), true
	case repoType:
		return strings.TrimPrefix(resourceName, "repo-"), true
	default:
		return "", false
	}
}

// checkImportIDs catches the 2026-08-07 outage one step BEFORE the plan
// shows it.
//
// deployTeams declares pulumi.Import(slug) for every live team on every
// run. Pulumi answers "import a resource already in state whose importID
// is missing or different" by REPLACING it — deleting the team, creating
// a new one with the same slug, and dropping every member on the way,
// because membership is the roster's and lives in no stack.
//
// The replace itself is caught by the plan-level guard. This check is
// cheaper and earlier: a team sitting in state with the wrong importID is
// a bomb whether or not today's plan happens to trip it, and it is exactly
// the state the 08-06 checkpoint was in the day before the org lost every
// team.
func checkImportIDs(ctx context.Context, stack auto.Stack, org string) ([]string, error) {
	exported, err := stack.Export(ctx)
	if err != nil {
		return nil, fmt.Errorf("export stack %s: %w", org, err)
	}

	problems, err := inspectTeamImportIDs(exported.Deployment)
	if err != nil {
		return nil, fmt.Errorf("stack %s: %w", org, err)
	}

	return problems, nil
}

// inspectTeamImportIDs is the pure half of checkImportIDs, so the rule can
// be tested against a real checkpoint instead of a live stack —
// testdata/state-2026-08-06-preincident.json is the actual state the org
// was in the day before it lost every team.
func inspectTeamImportIDs(raw []byte) ([]string, error) {
	var deployment apitype.DeploymentV3
	if err := json.Unmarshal(raw, &deployment); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}

	var problems []string

	for i := range deployment.Resources {
		r := &deployment.Resources[i]

		name := shortURN(string(r.URN))

		declared, guarded := declaredImportID(string(r.Type), name)
		if !guarded {
			continue
		}

		// Pulumi compares the declared import ID against the recorded
		// importID when there is one, and against the resource's own id
		// when there is not. A mismatch is read as a different resource,
		// and satisfied by REPLACING.
		//
		// This is why repositories are fine without an importID (they
		// import by name, which IS their id) and teams are not (they
		// import by slug while their id is numeric). Checking only for a
		// missing importID would clear every team on 2026-08-06 as
		// dangerous — correct — and every freshly adopted repository as
		// dangerous too, which is a false alarm, and a guard that cries
		// wolf is a guard that gets bypassed.
		effective := string(r.ImportID)
		source := "importID"

		if effective == "" {
			effective = string(r.ID)
			source = "id"
		}

		if effective == declared {
			continue
		}

		problems = append(problems, fmt.Sprintf(
			"%s %q: the engine declares Import(%q) but state's %s is %q — Pulumi resolves that mismatch by REPLACING "+
				"(this is exactly the 2026-08-06 state that cost the org every team the next morning)",
			shortType(string(r.Type)), declared, declared, source, effective))
	}

	sort.Strings(problems)

	return problems, nil
}

// shortType renders a provider token as the word a human would use.
func shortType(typ string) string {
	if i := strings.LastIndex(typ, ":"); i >= 0 {
		return strings.ToLower(typ[i+1:])
	}

	return typ
}

// checkRobotTeams fails when a team that no roster can refill holds
// repository grants but has no members.
//
// The roster syncs `team-<x>` from `team-<x>@truvity.com` and the
// partner-* families from their own directories. Everything else — the
// robots, `ci-cd` above all — is populated by hand exactly once, so a team
// that loses its members stays lost. That asymmetry is why an org-wide
// event on 2026-08-07 looked like one bot's problem for four days: the
// roster quietly repaired every human team, and nothing repaired this one.
//
// Grants are the trigger: a team with repository access and nobody in it
// is either a silent membership loss or a grant that should have been
// deleted. Both are worth a non-zero exit.
func checkRobotTeams(ctx context.Context, reg *registry.Config, org string, fillable func(string) bool) ([]string, error) {
	slugs := unfillableGrantedTeams(reg, org, fillable)

	var problems []string

	for _, slug := range slugs {
		var members []struct {
			Login string `json:"login"`
		}

		path := fmt.Sprintf("/orgs/%s/teams/%s/members?per_page=100", org, slug)
		if err := app.API(ctx, path, &members); err != nil {
			return nil, fmt.Errorf("read members of team %s: %w", slug, err)
		}

		if len(members) == 0 {
			problems = append(problems, fmt.Sprintf(
				"team %q holds repository grants but has NO members, and no roster will refill it — "+
					"add its machine account by hand (docs/operations/github-ci-bot.md)", slug))
		}
	}

	return problems, nil
}

// unfillableGrantedTeams returns the teams that hold repository grants and
// that no roster will refill — the set worth asking GitHub about.
//
// Split out from checkRobotTeams so the selection is testable: a rule that
// silently selects nothing passes every run and protects nothing, which is
// the failure mode this whole issue is about.
func unfillableGrantedTeams(reg *registry.Config, org string, fillable func(string) bool) []string {
	orgCfg, ok := reg.Orgs[org]
	if !ok {
		return nil
	}

	granted := map[string]bool{}

	// Resolved, not raw: a team can reach every repo of a profile without
	// appearing on a single repo row, and those grants are just as real.
	for name := range orgCfg.Repos {
		resolved, ok := reg.ResolveRepo(org, name)
		if !ok {
			continue
		}

		for slug := range resolved.Teams {
			granted[slug] = true
		}
	}

	slugs := make([]string, 0, len(granted))

	for slug := range granted {
		if fillable(slug) {
			continue
		}

		slugs = append(slugs, slug)
	}

	sort.Strings(slugs)

	return slugs
}
