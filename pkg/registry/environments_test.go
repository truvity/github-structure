package registry_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	registry "github.com/truvity/github-structure/pkg/registry"
)

// withEnvs puts an environments block on the minimal registry's repository row.
func withEnvs(t *testing.T, block string) (*registry.Config, error) {
	t.Helper()

	return load(t, strings.Replace(minimal, "        access: [managers]\n", "        access: [managers]\n"+block, 1))
}

func TestEnvironmentsLoad(t *testing.T) {
	cfg, err := withEnvs(t, `        environments:
          dev: {}
          stage:
            branches: [main]
            tags: ["v*"]
          production:
            branches: [main]
            tags: ["v*"]
            reviewer_teams: [engineers]
            prevent_self_review: true
          common:
            branches: [main]
`)
	require.NoError(t, err)

	repo := cfg.Orgs["acme"].Repos["widget"]
	assert.Equal(t, []string{"common", "dev", "production", "stage"}, repo.SortedEnvironments())

	assert.False(t, repo.Environments["dev"].HasBranchPolicy(), "an empty environment lets any ref deploy")
	assert.True(t, repo.Environments["stage"].HasBranchPolicy())
	assert.Equal(t, []string{"v*"}, repo.Environments["production"].Tags)
	assert.Equal(t, []string{"engineers"}, repo.Environments["production"].ReviewerTeams)
	assert.True(t, repo.Environments["production"].PreventSelfReview)
}

func TestEnvironmentsRefusals(t *testing.T) {
	cases := map[string]struct {
		block string
		want  string
	}{
		"name with a space": {
			"        environments:\n          \"my env\": {}\n",
			"the name must match",
		},
		"name with a wildcard": {
			"        environments:\n          \"prod*\": {}\n",
			"the name must match",
		},
		"name with a slash": {
			"        environments:\n          \"a/b\": {}\n",
			"the name must match",
		},
		"a pattern that allows every ref": {
			"        environments:\n          prod:\n            branches: [\"*\"]\n",
			"allows every ref",
		},
		"a ref-qualified pattern": {
			"        environments:\n          prod:\n            branches: [refs/heads/main]\n",
			"without the refs/ prefix",
		},
		"a tag ref-qualified pattern": {
			"        environments:\n          prod:\n            tags: [refs/tags/v1]\n",
			"without the refs/ prefix",
		},
		"an empty pattern": {
			"        environments:\n          prod:\n            branches: [\"\"]\n",
			"is empty or has whitespace",
		},
		"a pattern with a space": {
			"        environments:\n          prod:\n            tags: [\"v 1\"]\n",
			"is empty or has whitespace",
		},
		"a duplicate pattern": {
			"        environments:\n          prod:\n            branches: [main, main]\n",
			"listed twice",
		},
		"a reviewer team the org does not have": {
			"        environments:\n          prod:\n            reviewer_teams: [ghosts]\n",
			"which this org does not have",
		},
		"a reviewer team listed twice": {
			"        environments:\n          prod:\n            reviewer_teams: [engineers, engineers]\n",
			"listed twice",
		},
		"prevent_self_review without a reviewer": {
			"        environments:\n          prod:\n            prevent_self_review: true\n",
			"there is no reviewer",
		},
		"an unknown field": {
			"        environments:\n          prod:\n            branch: main\n",
			"field branch not found",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := withEnvs(t, tc.block)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestEnvironmentsRefuseMoreThanSixReviewers(t *testing.T) {
	// Seven distinct teams cannot be named from the minimal registry's two,
	// so the check is reached by the count alone.
	_, err := withEnvs(t, "        environments:\n          prod:\n            reviewer_teams: [a, b, c, d, e, f, g]\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GitHub allows 6 reviewers")
}

func TestEnvironmentsOnAnArchivedRepository(t *testing.T) {
	_, err := withEnvs(t, "        archived: true\n        environments:\n          dev: {}\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "archived, so environments do nothing")
}
