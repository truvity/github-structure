package app

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	registry "github.com/truvity/github-structure/pkg/registry"
)

func scopedConfig() *registry.Config {
	return &registry.Config{
		Presets: map[string]*registry.RepoSettings{"public": {}, "private": {}},
		Orgs: map[string]*registry.Org{
			"acme": {
				Repos: map[string]*registry.Repo{
					"tool-a":   {Preset: "public"},
					"tool-b":   {Preset: "public"},
					"attic":    {Preset: "public", Archived: true},
					"internal": {Preset: "private"},
				},
				Settings: &registry.OrgSettings{
					Actions: &registry.OrgActions{
						Variables: map[string]*registry.OrgVariable{
							"RENOVATE_CLIENT_ID": {
								Value:      "iv1",
								Visibility: "selected",
								Scope: &registry.EntitlementScope{
									DerivePreset: "public",
									Repos:        []string{"internal"},
								},
							},
							"UNSCOPED": {Value: "x", Visibility: "private"},
						},
						Secrets: map[string]*registry.OrgSecret{
							"RENOVATE_APP_PRIVATE_KEY": {
								Visibility: "selected",
								Scope:      &registry.EntitlementScope{DerivePreset: "public"},
							},
						},
					},
				},
			},
		},
	}
}

// TestEntitlementTargets: the derived rule covers non-archived
// profile-matching repos, explicit additions join it, archived repos and
// unscoped rows stay out.
func TestEntitlementTargets(t *testing.T) {
	targets := entitlementTargets("acme", scopedConfig())

	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2 (the unscoped variable must not appear)", len(targets))
	}

	v := targets[0]
	if v.subject != "variable RENOVATE_CLIENT_ID" || v.kind != "variables" {
		t.Fatalf("first target = %+v", v)
	}

	want := []string{"internal", "tool-a", "tool-b"}
	if len(v.want) != len(want) {
		t.Fatalf("variable scope = %v, want %v", v.want, want)
	}

	for i := range want {
		if v.want[i] != want[i] {
			t.Errorf("variable scope[%d] = %q, want %q (sorted, archived excluded)", i, v.want[i], want[i])
		}
	}

	s := targets[1]
	if s.subject != "secret RENOVATE_APP_PRIVATE_KEY" || s.kind != "secrets" {
		t.Fatalf("second target = %+v", s)
	}

	if len(s.want) != 2 {
		t.Fatalf("secret scope = %v, want the two live public repos", s.want)
	}
}

func TestSameStringSet(t *testing.T) {
	if !sameStringSet([]string{"a", "b"}, []string{"b", "a"}) {
		t.Error("order must not matter")
	}

	if sameStringSet([]string{"a"}, []string{"a", "b"}) {
		t.Error("length mismatch must differ")
	}

	if sameStringSet([]string{"a", "b"}, []string{"a", "c"}) {
		t.Error("member mismatch must differ")
	}
}

// TestPlanOrgVariables pins the three properties the reconciler's
// safety rests on: a matching row writes nothing, a row whose value or
// visibility moved is corrected, and a LIVE variable no row declares is
// left alone — the reconciler must never be able to delete one.
func TestPlanOrgVariables(t *testing.T) {
	t.Parallel()

	want := map[string]*registry.OrgVariable{
		"MATCHES":             {Value: "same", Visibility: "private"},
		"DEFAULTS_TO_PRIVATE": {Value: "same"},
		"VALUE_MOVED":         {Value: "declared", Visibility: "private"},
		"WIDENED":             {Value: "same", Visibility: "private"},
		"ABSENT":              {Value: "new", Visibility: "all"},
	}

	live := map[string]liveVariable{
		"MATCHES":             {value: "same", visibility: "private"},
		"DEFAULTS_TO_PRIVATE": {value: "same", visibility: "private"},
		"VALUE_MOVED":         {value: "live", visibility: "private"},
		"WIDENED":             {value: "same", visibility: "all"},
		"UNDECLARED":          {value: "someone else's", visibility: "private"},
	}

	writes := planOrgVariables(want, live)

	got := map[string]variableWrite{}
	for _, w := range writes {
		got[w.name] = w
	}

	if len(writes) != 3 {
		t.Fatalf("writes = %d (%v), want 3: the value, the visibility and the absent one", len(writes), got)
	}

	if _, planned := got["MATCHES"]; planned {
		t.Error("a variable that already matches must produce no write — the reconciler has to be a no-op on a clean estate")
	}

	if _, planned := got["DEFAULTS_TO_PRIVATE"]; planned {
		t.Error("an omitted visibility means `private`; a live private variable must not be rewritten")
	}

	if _, planned := got["UNDECLARED"]; planned {
		t.Fatal("a live variable no row declares must never be written or deleted — it is CheckOrgVariables' finding, not the reconciler's")
	}

	if w := got["VALUE_MOVED"]; w.create || w.value != "declared" {
		t.Errorf("VALUE_MOVED = %+v, want an update to the declared value", w)
	}

	if w := got["WIDENED"]; w.create || w.visibility != "private" {
		t.Errorf("WIDENED = %+v, want the visibility narrowed back to the declared one", w)
	}

	if w := got["ABSENT"]; !w.create || w.value != "new" || w.visibility != "all" {
		t.Errorf("ABSENT = %+v, want a create carrying the declared value and visibility", w)
	}
}

// TestCheckEntitlementScopesAbsentSubject: a declared variable or secret
// that does not exist live is a FINDING, not an error.
//
// Regression for the failure this replaced: GitHub answers 404 to the
// membership endpoint of a subject that is not there, the check returned
// it as an error, and `drift` aborted on the FIRST such row — so one
// stale declaration hid every other finding in the run, including the
// live-but-undeclared variables the same command exists to surface.
func TestCheckEntitlementScopesAbsentSubject(t *testing.T) {
	stubGh(t, "gh: Not Found (HTTP 404)")

	cfg := scopedConfig()

	drifts, err := CheckEntitlementScopes(t.Context(), "acme", cfg)
	if err != nil {
		t.Fatalf("a missing subject must be reported, not returned as an error: %v", err)
	}

	if len(drifts) != 2 {
		t.Fatalf("drifts = %v, want one per declared scope", drifts)
	}

	for _, d := range drifts {
		if d.Field != fieldExistence || d.Got != gotAbsent {
			t.Errorf("drift = %+v, want an existence finding", d)
		}
	}
}

// stubGh puts a `gh` on PATH that fails with the given stderr, so the
// exec-based helpers can be exercised without a network or a token.
func stubGh(t *testing.T, stderr string) {
	t.Helper()

	dir := t.TempDir()

	script := "#!/bin/sh\necho " + strconv.Quote(stderr) + " >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil { //nolint:gosec // a test stub must be executable
		t.Fatalf("write gh stub: %v", err)
	}

	t.Setenv("PATH", dir)
}
