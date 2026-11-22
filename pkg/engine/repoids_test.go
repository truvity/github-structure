package engine

import (
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi-github/sdk/v6/go/github"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repoIDMocks answers a repository's creation with a repoId output, the
// number GitHub gives a repository when it is created.
type repoIDMocks struct {
	envMocks

	ids map[string]int
}

func (m *repoIDMocks) NewResource(a pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	id, state, err := m.envMocks.NewResource(a)
	if err != nil || a.TypeToken != "github:index/repository:Repository" {
		return id, state, err
	}

	name := strings.TrimPrefix(a.Name, "repo-")
	out := state.Copy()
	out["repoId"] = resource.NewNumberProperty(float64(m.ids[name]))

	return id, out, nil
}

// The repository ids a run returns are the RepoId outputs of the
// repositories it declares, one per row, so a repository created in this
// run has its id in this run, without a read of the API.
func TestDeployReposReturnsRepoIDs(t *testing.T) {
	reg, orgCfg := renameRegistry(t, "widget")
	// The teams are live; no repository is, so every one is created by
	// this run.
	live := liveWith("", nil)
	for slug, id := range renameTeamIDs {
		live.teamIDs[slug] = id
	}

	m := &repoIDMocks{envMocks: *newEnvMocks(), ids: map[string]int{}}
	for i, name := range orgCfg.SortedRepos() {
		m.ids[name] = 1000 + i
	}

	var (
		mu  sync.Mutex
		got = map[string]int{}
	)

	require.NoError(t, pulumi.RunErr(func(c *pulumi.Context) error {
		provider, err := github.NewProvider(c, "gh", &github.ProviderArgs{})
		if err != nil {
			return err
		}

		teams, err := deployTeams(c, nil, renameOrg, orgCfg, live, provider)
		if err != nil {
			return err
		}

		ids, err := deployRepos(c, renameOrg, reg, orgCfg, teams, live, provider)
		if err != nil {
			return err
		}

		for name, id := range ids {
			id.ApplyT(func(v int) int {
				mu.Lock()
				defer mu.Unlock()

				got[name] = v

				return v
			})
		}

		return nil
	}, pulumi.WithMocks("proj", "stack", m)))

	require.Len(t, got, len(orgCfg.SortedRepos()), "one id per declared repository")
	assert.Equal(t, m.ids, got)
}
