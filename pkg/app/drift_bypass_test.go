package app

import (
	"encoding/json"
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

// The live pull_request rule is read for what it enforces: approvals,
// dismissal and code-owner review. A ruleset without the rule (checks
// only) reads as zero and false, which is what a declaration without it
// expects.
func TestLivePullRequestRule(t *testing.T) {
	for name, tc := range map[string]struct {
		body                string
		approvals           int
		dismiss, codeOwners bool
	}{
		"code-owner gate": {
			body: `{"rules":[{"type":"pull_request","parameters":{"required_approving_review_count":1,` +
				`"dismiss_stale_reviews_on_push":false,"require_code_owner_review":true}}]}`,
			approvals: 1, codeOwners: true,
		},
		"plain approval gate": {
			body:      `{"rules":[{"type":"pull_request","parameters":{"required_approving_review_count":2,"dismiss_stale_reviews_on_push":true}}]}`,
			approvals: 2, dismiss: true,
		},
		"checks only": {
			body: `{"rules":[{"type":"required_status_checks","parameters":{}}]}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var d liveRulesetDetail
			if err := json.Unmarshal([]byte(tc.body), &d); err != nil {
				t.Fatal(err)
			}

			approvals, dismiss, codeOwners := d.pullRequestRule()
			if approvals != tc.approvals || dismiss != tc.dismiss || codeOwners != tc.codeOwners {
				t.Fatalf("got (%d, %v, %v), want (%d, %v, %v)",
					approvals, dismiss, codeOwners, tc.approvals, tc.dismiss, tc.codeOwners)
			}
		})
	}
}
