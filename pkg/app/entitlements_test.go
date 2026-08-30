package app

import (
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
