package app

// Organization Actions entitlements: the org-level variables and secrets
// a shared workflow reads out of the caller's context, and the
// selected-repo membership that decides who may read them.
//
// This file is where they are ENFORCED. Two halves, and the split is a
// custody decision, not an omission:
//
//   - VARIABLES are reconciled whole — name, value, visibility and
//     membership. A variable's value is not a secret (runner labels,
//     bucket names, in-cluster URLs), so the registry can hold it and
//     this reconciler can write it.
//   - SECRETS are reconciled by MEMBERSHIP ONLY. Their values are never
//     read from, written by, or known to this code. A registry that
//     could write a secret value would need somewhere to read it from,
//     which is the key-custody problem the estate just retired; the
//     rows exist so the scope has an owner.
//
// The motivating class (INF-580): renovate silently never ran on seven
// repos because nothing owned the RENOVATE_* selected lists — workflows
// "skip cleanly" when unentitled, which is the failure mode that looks
// like success. Undeclared-but-live variables are the mirror of it and
// are REPORTED, never deleted: see CheckOrgVariables.
//
// Why here and not in the Pulumi engine, which owns every other piece of
// org structure — two independent blockers, either one sufficient:
//
//  1. CUSTODY. The engine authenticates as the structure App, whose
//     permission set is administration / organization_administration /
//     members / contents / metadata. GitHub gates the org variables API
//     behind the separate `organization_actions_variables` permission,
//     and editing an App's permissions has NO API at all — it is a
//     console edit plus an installation approval. So the engine cannot
//     so much as LIST these objects today, while this reconciler runs
//     on the operator's own credentials and already writes the
//     membership half.
//  2. ONE WRITER. Value and membership are one object. Handing the value
//     to Pulumi and leaving the membership here would put two systems
//     and two identities on the same variable, each blind to the other's
//     writes.
//
// Every write is idempotent and diff-gated: the reconciler reads live
// first and touches nothing that already matches, so it is safe to run
// on every deploy. Set-semantics PUTs replace a membership list whole,
// so there is no delete-then-recreate window — and nothing here ever
// deletes a variable or a secret.

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

// scopeTarget is one declared selected-scope: a variable's or secret's
// resolved repo membership.
type scopeTarget struct {
	kind    string // API segment: variables | secrets
	subject string // drift subject: "variable X" / "secret X"
	name    string
	want    []string
}

// entitlementTargets walks the org's declared Actions variables and
// secrets and renders every scope into its resolved repo list.
func entitlementTargets(org string, cfg *registry.Config) []scopeTarget {
	orgCfg, ok := cfg.Orgs[org]
	if !ok || orgCfg.Settings == nil || orgCfg.Settings.Actions == nil {
		return nil
	}

	actions := orgCfg.Settings.Actions

	var targets []scopeTarget

	for _, name := range sortedStrings(mapKeysOf(actions.Variables)) {
		if v := actions.Variables[name]; v.Scope != nil {
			targets = append(targets, scopeTarget{
				kind:    "variables",
				subject: "variable " + name,
				name:    name,
				want:    cfg.ResolveEntitlementScope(org, v.Scope),
			})
		}
	}

	for _, name := range sortedStrings(mapKeysOf(actions.Secrets)) {
		if s := actions.Secrets[name]; s.Scope != nil {
			targets = append(targets, scopeTarget{
				kind:    "secrets",
				subject: "secret " + name,
				name:    name,
				want:    cfg.ResolveEntitlementScope(org, s.Scope),
			})
		}
	}

	return targets
}

// liveScopeRepos lists the current selected-repository membership.
// One page of 100 covers the estate; a scope pushing past that deserves
// a redesign before it deserves pagination.
func liveScopeRepos(ctx context.Context, org, kind, name string) ([]string, error) {
	var page struct {
		Repositories []struct {
			Name string `json:"name"`
		} `json:"repositories"`
	}

	path := fmt.Sprintf("/orgs/%s/actions/%s/%s/repositories?per_page=100", org, kind, name)
	if err := ghAPI(ctx, path, &page); err != nil {
		return nil, err
	}

	repos := make([]string, 0, len(page.Repositories))
	for _, r := range page.Repositories {
		repos = append(repos, r.Name)
	}

	return repos, nil
}

// CheckEntitlementScopes reports the symmetric difference between every
// declared scope and its live membership — hand additions and silent
// exclusions alike.
//
// A subject that is declared but does not exist live is a FINDING, not
// an error. GitHub answers 404 to the membership endpoint of a variable
// or secret that is not there, and returning that as a failure ended the
// whole drift run at the first such row — so one stale declaration hid
// every other check behind it, including the undeclared-but-live ones.
// A report that stops at its first bad row is a report nobody can trust
// to be complete.
func CheckEntitlementScopes(ctx context.Context, org string, cfg *registry.Config) ([]Drift, error) {
	var drifts []Drift

	for _, t := range entitlementTargets(org, cfg) {
		got, err := liveScopeRepos(ctx, org, t.kind, t.name)

		if errors.Is(err, ErrNotFound) {
			drifts = append(drifts, Drift{
				Subject: t.subject,
				Field:   fieldExistence,
				Want:    "declared with " + strconv.Itoa(len(t.want)) + " selected repo(s)",
				Got:     gotAbsent,
			})

			continue
		}

		if err != nil {
			return nil, err
		}

		drifts = append(drifts, compareBypassSets(t.subject, "selected repos", t.want, got)...)
	}

	return drifts, nil
}

// ReconcileEntitlementScopes PUTs every out-of-sync scope to its declared
// membership and reports what changed. Equal sets are skipped — the
// reconciler is safe to run on every deploy.
func ReconcileEntitlementScopes(ctx context.Context, org string, cfg *registry.Config) ([]string, error) {
	ids := newRepoIDs(org)

	var applied []string

	for _, t := range entitlementTargets(org, cfg) {
		got, err := liveScopeRepos(ctx, org, t.kind, t.name)

		// Absent live: reported and skipped, for the reason
		// CheckEntitlementScopes gives. A missing VARIABLE is created by
		// ReconcileOrgVariables, which runs first; a missing SECRET has
		// no value this code may know, so only a human can create it.
		if errors.Is(err, ErrNotFound) {
			applied = append(applied, fmt.Sprintf("%s: NOT LIVE — scope not applied", t.subject))

			continue
		}

		if err != nil {
			return nil, err
		}

		if sameStringSet(t.want, got) {
			continue
		}

		selected, err := ids.resolve(ctx, t.want)
		if err != nil {
			return nil, err
		}

		path := fmt.Sprintf("/orgs/%s/actions/%s/%s/repositories", org, t.kind, t.name)
		if err := ghAPIPut(ctx, path, map[string]any{"selected_repository_ids": selected}); err != nil {
			return nil, fmt.Errorf("set scope for %s: %w", t.subject, err)
		}

		applied = append(applied, fmt.Sprintf("%s: %d repos (was %d)", t.subject, len(t.want), len(got)))
	}

	return applied, nil
}

// ghAPIPut mirrors ghAPI for set-semantics writes: gh's own credentials,
// JSON body on stdin (inline -f fields cannot express integer arrays).
func ghAPIPut(ctx context.Context, path string, payload any) error {
	return ghAPIWrite(ctx, "PUT", path, payload)
}

// ghAPIWrite is ghAPIPut for any verb — POST creates a variable, PATCH
// edits one.
func ghAPIWrite(ctx context.Context, method, path string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode payload for %s: %w", path, err)
	}

	cmd := exec.CommandContext(ctx, "gh", "api", "-X", method, path, "--input", "-")
	cmd.Stdin = bytes.NewReader(encoded)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh api %s %s: %w: %s", method, path, err, strings.TrimSpace(stderr.String()))
	}

	return nil
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	set := make(map[string]bool, len(a))
	for _, s := range a {
		set[s] = true
	}

	for _, s := range b {
		if !set[s] {
			return false
		}
	}

	return true
}

func mapKeysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	return keys
}

func sortedStrings(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)

	return out
}

// repoIDs resolves repository names to the numeric IDs GitHub's
// entitlement endpoints insist on, caching each lookup: one scope names
// the same repository as the next, and the org's rate limit is shared
// with everything else the run does.
type repoIDs struct {
	org   string
	cache map[string]int64
}

func newRepoIDs(org string) *repoIDs {
	return &repoIDs{org: org, cache: map[string]int64{}}
}

func (r *repoIDs) resolve(ctx context.Context, names []string) ([]int64, error) {
	out := make([]int64, 0, len(names))

	for _, name := range names {
		id, ok := r.cache[name]
		if !ok {
			var repo struct {
				ID int64 `json:"id"`
			}

			if err := ghAPI(ctx, "/repos/"+r.org+"/"+name, &repo); err != nil {
				return nil, fmt.Errorf("resolve repo id for %s/%s: %w", r.org, name, err)
			}

			id = repo.ID
			r.cache[name] = id
		}

		out = append(out, id)
	}

	return out, nil
}

// visibilitySelected is GitHub's name for "these repositories and no
// others" — the only visibility that carries a membership list.
const visibilitySelected = "selected"

// variableWrite is one write the variable reconciler intends: creating a
// declared variable that is not live, or correcting the value or
// visibility of one that is.
type variableWrite struct {
	name       string
	create     bool
	value      string
	visibility string
	// reason is what goes in the operator's report — the field that
	// differed and what it was, so an unexpected write is legible
	// before it is investigated.
	reason string
}

// planOrgVariables decides the writes, separated from the API so the
// decision can be tested without a network.
//
// Two properties are structural, not incidental:
//
//   - a variable that already matches produces NO write, so the
//     reconciler is a no-op on a clean estate and its output is
//     exactly the set of changes it made;
//   - a LIVE variable that no row declares produces no write either.
//     This code never deletes. Removing a variable every CI job reads
//     is not a thing a reconciler should be able to do as a side effect
//     of an editing slip; CheckOrgVariables reports the extra one and a
//     human decides.
func planOrgVariables(want map[string]*registry.OrgVariable, live map[string]liveVariable) []variableWrite {
	var writes []variableWrite

	for _, name := range sortedStrings(mapKeysOf(want)) {
		declared := want[name]

		visibility := declared.Visibility
		if visibility == "" {
			visibility = defaultVariableVisibility
		}

		got, present := live[name]
		if !present {
			writes = append(writes, variableWrite{
				name:       name,
				create:     true,
				value:      declared.Value,
				visibility: visibility,
				reason:     "not live",
			})

			continue
		}

		var reasons []string

		if got.value != declared.Value {
			reasons = append(reasons, fmt.Sprintf("value was %q", got.value))
		}

		if got.visibility != visibility {
			reasons = append(reasons, fmt.Sprintf("visibility was %q", got.visibility))
		}

		if len(reasons) == 0 {
			continue
		}

		writes = append(writes, variableWrite{
			name:       name,
			value:      declared.Value,
			visibility: visibility,
			reason:     strings.Join(reasons, ", "),
		})
	}

	return writes
}

// ReconcileOrgVariables makes every declared org Actions variable exist
// with the declared value and visibility, and reports what it changed.
//
// This is the enforcement half of CheckOrgVariables. Before it, the
// registry's variable rows were a description maintained by hand next to
// the console that actually held the values, and the two disagreed in
// both directions — a value edited live drifted silently, and a row
// added here did nothing at all.
//
// Selected-repository membership is written HERE too, for a variable
// whose row declares a scope: GitHub's PATCH replaces the whole object,
// so editing a `selected` variable's value without sending the list
// would be a write whose effect on the membership is the provider's
// guess rather than ours. Sending the resolved list makes the write
// total, and it is the same list ReconcileEntitlementScopes would PUT.
func ReconcileOrgVariables(ctx context.Context, org string, cfg *registry.Config) ([]string, error) {
	orgCfg, ok := cfg.Orgs[org]
	if !ok || orgCfg.Settings == nil || orgCfg.Settings.Actions == nil {
		return nil, nil
	}

	want := orgCfg.Settings.Actions.Variables
	if len(want) == 0 {
		return nil, nil
	}

	live, err := liveOrgVariables(ctx, org)
	if err != nil {
		return nil, err
	}

	ids := newRepoIDs(org)

	var applied []string

	for _, write := range planOrgVariables(want, live) {
		payload := map[string]any{
			"name":       write.name,
			"value":      write.value,
			"visibility": write.visibility,
		}

		scope := want[write.name].Scope

		switch {
		case write.visibility != visibilitySelected:
		case scope != nil:
			selected, resolveErr := ids.resolve(ctx, cfg.ResolveEntitlementScope(org, scope))
			if resolveErr != nil {
				return nil, resolveErr
			}

			payload["selected_repository_ids"] = selected
		case write.create:
			// A `selected` variable created with no list is readable by
			// nobody, and a workflow that cannot read it SKIPS rather
			// than fails (INF-580). Refusing is the only outcome that
			// cannot be mistaken for success.
			return nil, fmt.Errorf(
				"variable %s is declared `selected` with no scope and does not exist live: "+
					"add a scope, or create it by hand if its membership is deliberately unmanaged", write.name)
		}

		method, path := "PATCH", fmt.Sprintf("/orgs/%s/actions/variables/%s", org, write.name)
		if write.create {
			method, path = "POST", fmt.Sprintf("/orgs/%s/actions/variables", org)
		}

		if err := ghAPIWrite(ctx, method, path, payload); err != nil {
			return nil, fmt.Errorf("set variable %s: %w", write.name, err)
		}

		verb := "updated"
		if write.create {
			verb = "created"
		}

		applied = append(applied, fmt.Sprintf("variable %s: %s (%s)", write.name, verb, write.reason))
	}

	return applied, nil
}
