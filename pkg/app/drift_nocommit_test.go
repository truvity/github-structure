package app

import (
	"testing"
)

// An EMPTY repository — declared, created, never pushed to — answers
// 422 "No commit found for SHA: main" to anything asked about its own
// default branch. Not 404: the repository exists, its branch does not.
//
// Found by running the required-context guard against a real estate:
// one empty repository failed the whole organization's preflight, which
// is the same catch-22 the 404 case was fixed for. This pins the string
// gh actually emits — if gh rewords it, this fails loudly rather than
// silently restoring the failure for the next empty repository.
func TestGhSaysNoCommit(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		stderr string
		want   bool
	}{
		"gh 422 verbatim": {
			stderr: "gh: No commit found for SHA: main (HTTP 422)",
			want:   true,
		},
		"inside a longer message": {
			stderr: "gh api /repos/acme/empty/commits/main/check-runs?per_page=100: " +
				"exit status 1: gh: No commit found for SHA: main (HTTP 422)",
			want: true,
		},
		"another 422 is not this one — a malformed request must still fail loudly": {
			stderr: "gh: Validation Failed (HTTP 422)",
			want:   false,
		},
		"404 is the other sentinel": {
			stderr: "gh: Not Found (HTTP 404)",
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

			if got := ghSaysNoCommit(tc.stderr); got != tc.want {
				t.Fatalf("ghSaysNoCommit(%q) = %v, want %v", tc.stderr, got, tc.want)
			}
		})
	}
}
