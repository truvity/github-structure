package app

import (
	"testing"

	registry "github.com/truvity/github-structure/pkg/registry"
)

func TestNormalizeBypassers(t *testing.T) {
	got := normalizeBypassers("truvity", []string{"excavador", "/a-ignatov-parc", "truvity/team-dms"})
	want := []string{"/excavador", "/a-ignatov-parc", "truvity/team-dms"}

	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCompareBypassSets(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		d := compareBypassSets("repo x", "f", []string{"a", "b"}, []string{"b", "a"})
		if len(d) != 0 {
			t.Fatalf("expected no drift, got %v", d)
		}
	})

	t.Run("hand-added extra is reported", func(t *testing.T) {
		d := compareBypassSets("repo x", "f", []string{"Team:1"}, []string{"Team:1", "Integration:4597170"})
		if len(d) != 1 {
			t.Fatalf("expected 1 drift, got %v", d)
		}

		if d[0].Got != "extra: Integration:4597170" {
			t.Errorf("got %q", d[0].Got)
		}
	})

	t.Run("hand-removed declared is reported", func(t *testing.T) {
		d := compareBypassSets("repo x", "f", []string{"/excavador", "Team:1"}, []string{"Team:1"})
		if len(d) != 1 {
			t.Fatalf("expected 1 drift, got %v", d)
		}

		if d[0].Want != "/excavador" || d[0].Got != gotAbsent {
			t.Errorf("drift = %+v", d[0])
		}
	})

	t.Run("both directions at once", func(t *testing.T) {
		d := compareBypassSets("repo x", "f", []string{"a"}, []string{"b"})
		if len(d) != 2 {
			t.Fatalf("expected 2 drifts, got %v", d)
		}
	})
}

// workflow-only-tags is declared by a row field; its expected bypass set
// is the GitHub Actions integration alone, so a hand-added team or admin
// shows up as drift.
func TestWorkflowOnlyTagsExpectedActors(t *testing.T) {
	want := declaredRenderedActors(registry.WorkflowOnlyTagsRulesetFor([]string{"deploy/v*"}))
	if len(want) != 1 || want[0] != "Integration:15368" {
		t.Fatalf("want = %v", want)
	}

	d := compareBypassSets("repo x ruleset workflow-only-tags", "bypass actors", want,
		[]string{"Integration:15368", "Team:7", "OrganizationAdmin:0"})
	if len(d) != 1 || d[0].Got != "extra: OrganizationAdmin:0, Team:7" {
		t.Fatalf("drift = %+v", d)
	}
}
