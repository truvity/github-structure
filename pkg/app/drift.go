package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	registry "github.com/truvity/github-structure/pkg/registry"
)

// defaultVariableVisibility is what a row means when it says nothing:
// the safe end. See CheckOrgVariables.
const (
	defaultVariableVisibility = "private"
)

const (
	// fieldRegistry / wantDeclared are the vocabulary for "the registry
	// does not claim this thing", shared by the owner, repo and ruleset
	// checks so they read identically in output.
	fieldRegistry = "registry"
	wantDeclared  = "declared in cfg/github.yaml"

	// fieldExistence / fieldVisibility / gotAbsent are shared for the
	// same reason: the variable, owner, repo and ruleset checks all
	// speak them, and one of them drifting to a synonym would split
	// output a reader expects to be able to grep as a single vocabulary.
	fieldExistence  = "existence"
	fieldVisibility = "visibility"
	gotAbsent       = "absent"
)

type (
	// Drift is one difference between the registry and live GitHub.
	Drift struct {
		Subject string
		Field   string
		Want    string
		Got     string
	}

	// liveVariable is what GitHub reports for one org variable.
	liveVariable struct {
		value      string
		visibility string
	}

	// liveRepoSettings is what GitHub reports for one repository.
	//
	// Deliberately fetched one repo at a time: the org LIST endpoint omits
	// every merge-behavior field (allow_auto_merge and friends come back
	// absent, not false), so a check built on the cheap call would compare
	// the registry against nulls and report the whole fleet as clean. That
	// is the exact failure this check exists to end.
	liveRepoSettings struct {
		Visibility          string `json:"visibility"`
		Description         string `json:"description"`
		DefaultBranch       string `json:"default_branch"`
		Archived            bool   `json:"archived"`
		HasIssues           bool   `json:"has_issues"`
		HasWiki             bool   `json:"has_wiki"`
		HasProjects         bool   `json:"has_projects"`
		AllowAutoMerge      bool   `json:"allow_auto_merge"`
		AllowSquashMerge    bool   `json:"allow_squash_merge"`
		AllowMergeCommit    bool   `json:"allow_merge_commit"`
		AllowRebaseMerge    bool   `json:"allow_rebase_merge"`
		DeleteBranchOnMerge bool   `json:"delete_branch_on_merge"`
		AllowUpdateBranch   bool   `json:"allow_update_branch"`
		AllowForking        bool   `json:"allow_forking"`
	}
)

func (d Drift) String() string {
	return fmt.Sprintf("%s: %s: registry=%q live=%q", d.Subject, d.Field, d.Want, d.Got)
}

// API performs a read-only GitHub API call with the operator's own gh
// credentials, decoding the response into dst.
//
// Exported for the preflight checks in cmd/githubctl, which ask GitHub
// questions the Pulumi state cannot answer (team membership above all —
// it is the roster's, so it appears in no stack).
// apiPaginated follows GitHub's Link headers and hands back ONE page per
// element, because `gh api --paginate` streams several JSON documents and
// only `--slurp` makes them a single parseable value. Callers flatten.
//
// Kept separate from API rather than changing it: the other callers read
// single objects, where --paginate/--slurp would wrap the answer in an
// array and break them.
func apiPaginated(ctx context.Context, path string, dst any) error {
	cmd := exec.CommandContext(ctx, "gh", "api", "--paginate", "--slurp", path)

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if ghSaysNotFound(msg) {
			return fmt.Errorf("gh api --paginate %s: %w: %s", path, ErrNotFound, msg)
		}

		return fmt.Errorf("gh api --paginate %s: %w: %s", path, err, msg)
	}

	return json.Unmarshal(stdout.Bytes(), dst)
}

// API performs a single authenticated GET against the GitHub REST API
// (via the ambient `gh` CLI) and unmarshals the response into dst —
// the escape hatch for callers probing endpoints the typed checks do
// not cover.
func API(ctx context.Context, path string, dst any) error {
	return ghAPI(ctx, path, dst)
}

// ghSaysNotFound reports whether gh's stderr means HTTP 404.
//
// The gh CLI signals status in prose, not in its exit code, so matching
// the text is the only signal available short of teaching this helper its
// own auth and using net/http. Split out from ghAPI to be testable: the
// alternative is a test that constructs the wrapped error itself and
// proves only that errors.Is works.
func ghSaysNotFound(stderr string) bool {
	return strings.Contains(stderr, "HTTP 404")
}

// ghSaysNoCommit reports whether gh's stderr means "that ref has no
// commit" — an empty repository, which answers 422 rather than 404.
func ghSaysNoCommit(stderr string) bool {
	return strings.Contains(stderr, "No commit found for SHA")
}

// ghAPI shells out to the gh CLI so the checker runs with whatever
// credentials the operator (or CI) already has, rather than growing its
// own auth path.
func ghAPI(ctx context.Context, path string, dst any) error {
	cmd := exec.CommandContext(ctx, "gh", "api", path)

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		// gh reports HTTP status in prose on stderr, not via exit code, so
		// matching the text is the only signal available short of teaching
		// this helper its own auth and using net/http.
		// Surface 404 as the package's existing sentinel, the same way
		// Client.Get does. Without this, the two API paths in this package
		// disagree about whether "does not exist yet" is failure — and the
		// gh-CLI path is the one preflight uses.
		if ghSaysNotFound(msg) {
			return fmt.Errorf("gh api %s: %w: %s", path, ErrNotFound, msg)
		}

		if ghSaysNoCommit(msg) {
			return fmt.Errorf("gh api %s: %w: %s", path, ErrNoCommit, msg)
		}

		return fmt.Errorf("gh api %s: %w: %s", path, err, msg)
	}

	return json.Unmarshal(stdout.Bytes(), dst)
}

// CheckOrgVariables compares the org's declared Actions variables against
// live GitHub.
//
// The DECLARED side of this is now enforced — ReconcileOrgVariables
// writes any value or visibility that differs — so the findings this
// check still has to itself are the ones no reconciler may act on:
//
//   - a variable that is LIVE and undeclared. Deleting it is not a thing
//     a reconciler should do as a side effect of an editing slip, so it
//     is reported and a human decides;
//   - a declared variable the reconciler has not been run for yet, which
//     is the drift check doing its actual job.
//
// Visibility is checked as strictly as the values, and defaults to
// `private` when a row omits it. These name internal infrastructure —
// bucket names, in-cluster URLs — and one widened to `all` becomes
// readable by any public repository's workflow, which is precisely the
// leak the public shared workflow exists to avoid.
func CheckOrgVariables(ctx context.Context, org string, cfg *registry.Org) ([]Drift, error) {
	if cfg.Settings == nil || cfg.Settings.Actions == nil || len(cfg.Settings.Actions.Variables) == 0 {
		return nil, nil
	}

	live, err := liveOrgVariables(ctx, org)
	if err != nil {
		return nil, err
	}

	return compareOrgVariables(cfg.Settings.Actions.Variables, live), nil
}

// liveOrgVariables reads the org's Actions variables — the one fetch the
// check and the reconciler share, so they can never disagree about what
// "live" means.
//
// One page of 100 is deliberate rather than lazy: org variables are a
// hand-curated set of infrastructure particulars, and an estate that
// pushes past a hundred of them wants a rethink before it wants
// pagination.
func liveOrgVariables(ctx context.Context, org string) (map[string]liveVariable, error) {
	var page struct {
		Variables []struct {
			Name       string `json:"name"`
			Value      string `json:"value"`
			Visibility string `json:"visibility"`
		} `json:"variables"`
	}

	if err := ghAPI(ctx, fmt.Sprintf("/orgs/%s/actions/variables?per_page=100", org), &page); err != nil {
		return nil, err
	}

	live := make(map[string]liveVariable, len(page.Variables))
	for _, v := range page.Variables {
		live[v.Name] = liveVariable{value: v.Value, visibility: v.Visibility}
	}

	return live, nil
}

// compareOrgVariables is the comparison itself, separated from the fetch
// so it can be tested without a network.
func compareOrgVariables(want map[string]*registry.OrgVariable, live map[string]liveVariable) []Drift {
	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}

	sort.Strings(names)

	var drifts []Drift

	for _, name := range names {
		got, present := live[name]
		if !present {
			drifts = append(drifts, Drift{Subject: "variable " + name, Field: fieldExistence, Want: want[name].Value, Got: gotAbsent})

			continue
		}

		if got.value != want[name].Value {
			drifts = append(drifts, Drift{Subject: "variable " + name, Field: "value", Want: want[name].Value, Got: got.value})
		}

		visibility := want[name].Visibility
		if visibility == "" {
			visibility = defaultVariableVisibility
		}

		if got.visibility != visibility {
			drifts = append(drifts, Drift{
				Subject: "variable " + name,
				Field:   fieldVisibility,
				Want:    visibility,
				Got:     got.visibility,
			})
		}
	}

	// An undeclared variable is reported, never removed: it may be
	// someone's in-flight work, and this command does not delete.
	undeclared := make([]string, 0)

	for name := range live {
		if _, known := want[name]; !known {
			undeclared = append(undeclared, name)
		}
	}

	sort.Strings(undeclared)

	for _, name := range undeclared {
		drifts = append(drifts, Drift{
			Subject: "variable " + name,
			Field:   fieldRegistry,
			Want:    wantDeclared,
			Got:     "live but unknown",
		})
	}

	return drifts
}

// CheckUndeclaredRepos reports every ACTIVE repository in the org that no
// registry row claims.
//
// This is the check the repo-count assertion cannot be. A count notices
// that the registry changed; it cannot notice a repository that was
// created in GitHub and never added — the count simply stays right,
// because the missing row was never in it. That is exactly how `gemaal`
// went unmanaged (INF-552): active, public, running shared CI, and
// sitting on `allow_auto_merge: false` while every profiled repo had it
// true, so Renovate's auto-merge request was refused and nothing went
// red for days.
//
// Archived repositories are excluded deliberately: an estate's archived
// tail is out of scope by decision, and a check that shouted about it
// would be ignored within a week — the failure mode this whole file
// exists to avoid.
func CheckUndeclaredRepos(ctx context.Context, org string, cfg *registry.Org) ([]Drift, error) {
	type liveRepo struct {
		Name     string `json:"name"`
		Archived bool   `json:"archived"`
	}

	// PAGINATED, and that is load-bearing: the org holds 158
	// repositories against a 100-per-page maximum, so an unpaginated
	// call sees the first page and reports "clean" for everything after
	// it. A guard that cannot see the whole org is worse than none — it
	// answers the question it was asked with a number it did not measure.
	var pages [][]liveRepo
	if err := apiPaginated(ctx, "orgs/"+org+"/repos?per_page=100&type=all", &pages); err != nil {
		return nil, fmt.Errorf("list %s repos: %w", org, err)
	}

	var live []liveRepo
	for _, page := range pages {
		live = append(live, page...)
	}

	var drifts []Drift

	for _, r := range live {
		if r.Archived {
			continue
		}

		if _, declared := cfg.Repos[r.Name]; !declared {
			drifts = append(drifts, Drift{
				Subject: "repo " + r.Name,
				Field:   fieldRegistry,
				Want:    wantDeclared,
				Got:     "active but undeclared — nothing manages its settings",
			})
		}
	}

	sort.Slice(drifts, func(i, j int) bool { return drifts[i].Subject < drifts[j].Subject })

	return drifts, nil
}

// CheckOrgOwners compares the org's declared owners against live GitHub.
//
// This is the enforcement half of a deliberate split. The engine ASSERTS
// each declared login holds the `admin` role, one resource per login, so
// it can promote but never demote — a full-set model would make an
// accidentally-empty block mean "remove every owner". That safety leaves
// one blind spot: an owner added out-of-band is managed by nobody and
// visible in no plan. This check closes it by reporting, and a human
// decides whether the answer is "add them to the registry" or "take the
// role away".
//
// Owner is the widest grant GitHub has — org settings, billing, repo
// deletion, every team — so an unexplained one is worth a failed check.
// It is also the shape of carve-out that has hurt estates before: a
// membership managed by hand, outside every system, is refilled by
// nothing when it is emptied.
func CheckOrgOwners(ctx context.Context, org string, cfg *registry.Org) ([]Drift, error) {
	if len(cfg.Owners) == 0 {
		return nil, nil
	}

	var members []struct {
		Login string `json:"login"`
	}

	if err := ghAPI(ctx, fmt.Sprintf("/orgs/%s/members?role=admin&per_page=100", org), &members); err != nil {
		return nil, err
	}

	live := make([]string, 0, len(members))
	for _, m := range members {
		live = append(live, m.Login)
	}

	return compareOrgOwners(cfg.Owners, live), nil
}

// compareOrgOwners is the comparison itself, separated from the fetch so
// it can be tested without a network.
//
// Comparison is case-insensitive because GitHub logins are, but the
// reported strings keep their original spelling so the operator can
// match them against what the UI shows.
func compareOrgOwners(want, live []string) []Drift {
	// An org that declares no owners has not adopted the field and opts
	// out. CheckOrgOwners returns early on the same condition; the guard
	// is repeated HERE because that one is an optimization and this one
	// is the rule — with it only there, calling the comparison directly
	// reports every owner of every un-adopted org as undeclared.
	if len(want) == 0 {
		return nil
	}

	liveSet := make(map[string]string, len(live))
	for _, login := range live {
		liveSet[strings.ToLower(login)] = login
	}

	wantSet := make(map[string]string, len(want))
	for _, login := range want {
		wantSet[strings.ToLower(login)] = login
	}

	var drifts []Drift

	declared := append([]string(nil), want...)
	sort.Strings(declared)

	for _, login := range declared {
		if _, ok := liveSet[strings.ToLower(login)]; !ok {
			drifts = append(drifts, Drift{
				Subject: "owner " + login,
				Field:   "role",
				Want:    "admin",
				Got:     "not an owner",
			})
		}
	}

	undeclared := make([]string, 0, len(live))
	for _, login := range live {
		if _, ok := wantSet[strings.ToLower(login)]; !ok {
			undeclared = append(undeclared, login)
		}
	}

	sort.Strings(undeclared)

	for _, login := range undeclared {
		drifts = append(drifts, Drift{
			Subject: "owner " + login,
			Field:   fieldExistence,
			Want:    "not declared in cfg/github.yaml",
			Got:     "admin",
		})
	}

	return drifts
}

// CheckRepoSettings compares every declared repository's RESOLVED
// settings — its profile with the row's overrides layered on — against
// live GitHub.
//
// This closes the gap CheckUndeclaredRepos cannot: that one notices a
// repository with no row at all, and says nothing once a row exists.
// But a row is a statement of intent, not a fact about GitHub — it only
// becomes true when `just github-deploy` runs. Between the merge and the
// apply the registry and the estate disagree, and until now nothing
// looked.
//
// That window is not hypothetical. gemaal's row landed 2026-08-17 with
// `profile: public`, which sets allow_auto_merge true; the apply did not
// follow. So the repository kept the false it was created with, Renovate
// kept asking GitHub to arm auto-merge, GitHub kept refusing, and
// gemaal#39 sat green and unmerged while `just github-drift` reported
// everything matching — because drift covered variables and owners
// and simply did not look at repository settings.
//
// Branch protection is NOT compared here. It is a separate resource with
// its own shape, and a partial check that looked like a total one would
// be the same trap in a new place; the settings above are the ones a
// profile promises and the ones that go wrong silently.
func CheckRepoSettings(ctx context.Context, org string, registry *registry.Config) ([]Drift, error) {
	orgCfg, ok := registry.Orgs[org]
	if !ok {
		return nil, nil
	}

	var drifts []Drift

	for _, name := range orgCfg.SortedRepos() {
		// An archived row is inert by the same rule the engine uses:
		// every setting is ignored rather than written to a repository
		// GitHub has frozen.
		if orgCfg.Repos[name].Archived {
			continue
		}

		want, resolved := registry.ResolveRepo(org, name)
		if !resolved {
			continue
		}

		var live liveRepoSettings

		if err := ghAPI(ctx, "/repos/"+org+"/"+name, &live); err != nil {
			// A declared repository that does not exist is a to-do, not
			// a failed check: the row is ahead of the estate, which is
			// how a repository gets created in the first place.
			if errors.Is(err, ErrNotFound) {
				drifts = append(drifts, Drift{
					Subject: "repo " + name,
					Field:   fieldExistence,
					Want:    wantDeclared,
					Got:     gotAbsent,
				})

				continue
			}

			return nil, err
		}

		// A repository archived out-of-band is reported as that one
		// fact. Comparing its settings underneath would bury the only
		// line that matters in a dozen it explains.
		if live.Archived {
			drifts = append(drifts, Drift{Subject: "repo " + name, Field: "archived", Want: "false", Got: "true"})

			continue
		}

		drifts = append(drifts, compareRepoSettings(name, want, live)...)
	}

	return drifts, nil
}

// compareRepoSettings is the comparison itself, separated from the fetch
// so it can be tested without a network.
//
// `description` is compared like everything else, and that is not
// pedantry: the engine writes the field only when the row declares one
// (internal/components/github), so a repository carrying a description
// the registry does not know about LOSES it on the next apply. Reporting
// it is the only warning before the wipe.
func compareRepoSettings(name string, want registry.Resolved, got liveRepoSettings) []Drift {
	subject := "repo " + name

	var drifts []Drift

	for _, c := range []struct {
		field string
		want  string
		got   string
	}{
		{fieldVisibility, want.Visibility, got.Visibility},
		{"description", want.Description, got.Description},
		{"default_branch", want.DefaultBranch, got.DefaultBranch},
	} {
		if c.want != c.got {
			drifts = append(drifts, Drift{Subject: subject, Field: c.field, Want: c.want, Got: c.got})
		}
	}

	for _, c := range []struct {
		field string
		want  bool
		got   bool
	}{
		{"allow_auto_merge", want.AllowAutoMerge, got.AllowAutoMerge},
		{"allow_squash_merge", want.AllowSquashMerge, got.AllowSquashMerge},
		{"allow_merge_commit", want.AllowMergeCommit, got.AllowMergeCommit},
		{"allow_rebase_merge", want.AllowRebaseMerge, got.AllowRebaseMerge},
		{"delete_branch_on_merge", want.DeleteBranchOnMerge, got.DeleteBranchOnMerge},
		{"allow_update_branch", want.AllowUpdateBranch, got.AllowUpdateBranch},
		{"allow_forking", want.AllowForking, got.AllowForking},
		{"has_issues", want.HasIssues, got.HasIssues},
		{"has_wiki", want.HasWiki, got.HasWiki},
		{"has_projects", want.HasProjects, got.HasProjects},
	} {
		if c.want != c.got {
			drifts = append(drifts, Drift{
				Subject: subject,
				Field:   c.field,
				Want:    strconv.FormatBool(c.want),
				Got:     strconv.FormatBool(c.got),
			})
		}
	}

	return drifts
}
