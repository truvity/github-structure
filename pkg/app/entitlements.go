package app

// Entitlement scopes: the selected-repo membership of org Actions
// variables and secrets, DERIVED from the registry (a profile-wide rule
// plus explicit additions) instead of hand-kept. The motivating class
// (INF-580): renovate silently never ran on seven repos because nothing
// owned the RENOVATE_* selected lists — workflows "skip cleanly" when
// unentitled, which is the failure mode that looks like success.
//
// Reconciliation is idempotent set-semantics PUTs — GitHub's
// selected-repositories endpoint replaces the whole list, so there is no
// delete-then-recreate window and no Pulumi import problem (the provider
// cannot import org variables at all; engine.go documents why they are
// not managed there).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
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
func CheckEntitlementScopes(ctx context.Context, org string, cfg *registry.Config) ([]Drift, error) {
	var drifts []Drift

	for _, t := range entitlementTargets(org, cfg) {
		got, err := liveScopeRepos(ctx, org, t.kind, t.name)
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
	ids := map[string]int64{}

	var applied []string

	for _, t := range entitlementTargets(org, cfg) {
		got, err := liveScopeRepos(ctx, org, t.kind, t.name)
		if err != nil {
			return nil, err
		}

		if sameStringSet(t.want, got) {
			continue
		}

		selected := make([]int64, 0, len(t.want))

		for _, repo := range t.want {
			id, ok := ids[repo]
			if !ok {
				var r struct {
					ID int64 `json:"id"`
				}

				if err := ghAPI(ctx, "/repos/"+org+"/"+repo, &r); err != nil {
					return nil, fmt.Errorf("resolve repo id for %s/%s: %w", org, repo, err)
				}

				id = r.ID
				ids[repo] = id
			}

			selected = append(selected, id)
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
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode payload for %s: %w", path, err)
	}

	cmd := exec.CommandContext(ctx, "gh", "api", "-X", "PUT", path, "--input", "-")
	cmd.Stdin = bytes.NewReader(encoded)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh api PUT %s: %w: %s", path, err, strings.TrimSpace(stderr.String()))
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
