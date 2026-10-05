package registry

import "testing"

func TestGlobsIntersect(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v*", "v*", true},
		{"*", "v*", true},
		{"**", "v*", true},
		{"deploy/pulumi/v*", "v*", false},
		{"deploy/pulumi/v*", "deploy/*/v*", true},
		{"deploy/pulumi/v*", "deploy/v*", false},
		{"deploy/pulumi/v*", "sdk/v*", false},
		{"*/v*", "deploy/pulumi/v*", false}, // * does not cross /
		{"**/v*", "deploy/pulumi/v*", true},
		{"deploy/v1", "deploy/v?", true},
		{"deploy/v1", "deploy/v22", false},
		{"vault/*", "v*", false},
		{"v?", "v*", true},
	}

	for _, tc := range cases {
		if got := globsIntersect(tc.a, tc.b); got != tc.want {
			t.Errorf("globsIntersect(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}

		if got := globsIntersect(tc.b, tc.a); got != tc.want {
			t.Errorf("globsIntersect(%q, %q) = %v, want %v (symmetry)", tc.b, tc.a, got, tc.want)
		}
	}
}
