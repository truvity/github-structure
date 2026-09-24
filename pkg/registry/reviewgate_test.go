package registry_test

import (
	"testing"

	"github.com/truvity/github-structure/pkg/registry"
)

// The review gate is the whole of the review mechanics, and since
// 2026-09-24 TWO things read it: the engine, which creates the ruleset,
// and the drift check, which has to recognise it as declared. They
// cannot be allowed to disagree, which is why the decision lives here
// and is tested here.
//
// The disagreement is what this is really guarding. While only the
// engine knew, `github-drift` called every synthesised ruleset "live but
// undeclared" -- one false line per `review: required` repository, 35 on
// truvity -- so the check always exited non-zero, gated nothing, and a
// real drift went unread in the noise for a day.
func TestReviewGate(t *testing.T) {
	t.Parallel()

	t.Run("none asks for no ruleset", func(t *testing.T) {
		t.Parallel()

		if got := registry.ReviewGate(registry.Resolved{Review: registry.ReviewNone}); got != nil {
			t.Fatalf("review: none synthesised %+v, want nil", got)
		}
	})

	// The pre-review shape, where the approval knobs in `protection` are
	// the gate and no ruleset is involved at all.
	t.Run("unset asks for no ruleset", func(t *testing.T) {
		t.Parallel()

		if got := registry.ReviewGate(registry.Resolved{}); got != nil {
			t.Fatalf("empty review synthesised %+v, want nil", got)
		}
	})

	t.Run("required names the ruleset the engine creates", func(t *testing.T) {
		t.Parallel()

		got := registry.ReviewGate(registry.Resolved{Review: registry.ReviewRequired})
		if got == nil {
			t.Fatal("review: required synthesised nothing")
		}

		// The NAME is load-bearing twice over: the engine keys the
		// resource on repo + name, so a change here replaces the live
		// ruleset rather than updating it -- and a replacement has a
		// window with no gate at all.
		if got.Name != registry.ReviewRulesetName {
			t.Errorf("name %q, want %q", got.Name, registry.ReviewRulesetName)
		}

		if got.RequiredApprovals != 1 {
			t.Errorf("approvals %d, want 1", got.RequiredApprovals)
		}

		// Rulesets do NOT exempt org admins the way classic protection
		// does. Dropping this locks the maintainers out of their own
		// repository, which is how it was learned the first time.
		if !got.BypassOrgAdmins {
			t.Error("org admins cannot bypass; a ruleset does not exempt them implicitly")
		}

		if got.Pattern != registry.DefaultBranchRef {
			t.Errorf("pattern %q, want %q", got.Pattern, registry.DefaultBranchRef)
		}
	})

	// Where classic protection is off, the ruleset is the only thing
	// left to carry the required checks -- so it carries them, and a
	// repository does not silently lose its check gate by turning
	// classic protection off.
	t.Run("it carries the checks when classic protection does not", func(t *testing.T) {
		t.Parallel()

		r := registry.Resolved{Review: registry.ReviewRequired}
		r.Protection.Enabled = false
		r.Protection.RequiredChecks = []string{"check"}

		got := registry.ReviewGate(r)
		if len(got.RequiredChecks) != 1 || got.RequiredChecks[0] != "check" {
			t.Errorf("required checks %v, want [check]", got.RequiredChecks)
		}
	})

	t.Run("it leaves the checks to classic protection when that is on", func(t *testing.T) {
		t.Parallel()

		r := registry.Resolved{Review: registry.ReviewRequired}
		r.Protection.Enabled = true
		r.Protection.RequiredChecks = []string{"check"}

		// Duplicating them would require the same context twice, and a
		// ruleset's bypass actors skip ruleset checks while classic
		// checks apply to everyone -- so which one holds the checks is
		// a real difference, not a tidiness question.
		if got := registry.ReviewGate(r); len(got.RequiredChecks) != 0 {
			t.Errorf("required checks %v, want none: classic protection has them", got.RequiredChecks)
		}
	})
}
