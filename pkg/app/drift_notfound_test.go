package app

import (
	"testing"
)

// Regression for 2026-08-16: declaring the `workstation` repo made
// `just github-deploy` unrunnable. A new repo has no CI, so it must carry
// `checks_waived`; that waiver made preflight's stale-waiver guard probe
// a repo that did not exist yet; the probe 404'd and preflight aborted
// before pulumi — so the waiver the repo needed was the very thing
// preventing the repo from being created.
//
// The fix is that a 404 from the gh-CLI path becomes ErrNotFound, as it
// already did on the Client.Get path, so callers can treat "not created
// yet" as information. This pins the string gh actually emits: if gh
// rewords it, this fails loudly rather than silently restoring the
// catch-22 for the next new repo.
func TestGhSaysNotFound(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		stderr string
		want   bool
	}{
		"gh 404 verbatim": {
			stderr: "gh: Not Found (HTTP 404)",
			want:   true,
		},
		"404 inside a longer message": {
			stderr: "gh api /repos/truvity/workstation/commits/master/check-runs?per_page=100: " +
				"exit status 1: gh: Not Found (HTTP 404)",
			want: true,
		},
		"403 is not 404 — a permissions problem must still fail loudly": {
			stderr: "gh: Resource not accessible by integration (HTTP 403)",
			want:   false,
		},
		"prose 'not found' without a status is not proof": {
			stderr: "gh: could not resolve to a Repository with the name 'x'",
			want:   false,
		},
		"empty": {
			stderr: "",
			want:   false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := ghSaysNotFound(tc.stderr); got != tc.want {
				t.Fatalf("ghSaysNotFound(%q) = %v, want %v", tc.stderr, got, tc.want)
			}
		})
	}
}
