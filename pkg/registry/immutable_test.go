package registry_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func immutableRow(field string) string {
	return withRepos(`
    repos:
      widget:
        preset: public
        access: [managers]
` + field)
}

// `immutable_releases` is a tri-state: unset is not managed, and false is
// a decision that is not the same as unset.
func TestImmutableReleasesTriState(t *testing.T) {
	for name, tc := range map[string]struct {
		field string
		want  *bool
	}{
		"unset": {"", nil},
		"true":  {"        immutable_releases: true\n", ptrBool(true)},
		"false": {"        immutable_releases: false\n", ptrBool(false)},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := load(t, immutableRow(tc.field))
			require.NoError(t, err)

			got := cfg.Orgs["acme"].Repos["widget"].ImmutableReleases

			if tc.want == nil {
				assert.Nil(t, got, "unset must stay nil: it means not managed")

				return
			}

			require.NotNil(t, got)
			assert.Equal(t, *tc.want, *got)
		})
	}
}

func TestImmutableReleasesMustBeABoolean(t *testing.T) {
	_, err := load(t, immutableRow("        immutable_releases: yes please\n"))
	require.Error(t, err)
}

// On an archived repository GitHub writes nothing, so a row that states the
// field would mean something it does not: refused, like its neighbours.
func TestImmutableReleasesRefusedOnArchived(t *testing.T) {
	body := strings.Replace(immutableRow("        immutable_releases: true\n"),
		"        preset: public\n", "        preset: public\n        archived: true\n", 1)

	_, err := load(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "archived, so immutable_releases does nothing")
}

func ptrBool(b bool) *bool { return &b }
