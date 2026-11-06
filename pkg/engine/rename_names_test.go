package engine

import (
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-structure/pkg/staterename"
)

// nameMocks records the name of every resource the program declares, by
// type token.
type nameMocks struct {
	mu    sync.Mutex
	names map[string]string
}

func (m *nameMocks) NewResource(a pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.names[a.Name] = a.TypeToken

	// A team's id is read back as a number (a ruleset's bypass actor).
	if id, ok := renameTeamIDs[strings.TrimPrefix(a.Name, "team-")]; ok && strings.HasSuffix(a.TypeToken, ":Team") {
		return id, a.Inputs, nil
	}

	return a.Name + "_id", a.Inputs, nil
}

func (m *nameMocks) Call(a pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return a.Args, nil
}

// The state re-key derives a row's resource names from the registry
// (staterename.ResourceNames). The engine spells them where it declares the
// resources. If the two ever disagree a re-key would refuse a valid state,
// or worse accept a wrong one, so this runs the engine's repository half
// under mocks and compares. It needs no pulumi CLI, so it runs in CI.
func TestStateRenameNamesMatchTheEngine(t *testing.T) {
	reg, orgCfg := renameRegistry(t, newRepo)

	// The repository exists live with commits: every resource is declared.
	h := &harness{apiURL: "http://127.0.0.1:1/"}
	m := &nameMocks{names: map[string]string{}}

	require.NoError(t, pulumi.RunErr(
		h.program(reg, orgCfg, liveWith(newRepo, rowSlugs(reg, orgCfg, newRepo)), nil),
		pulumi.WithMocks(renameProject, renameOrg, m)))

	var declared []string

	for name, typ := range m.names {
		if strings.HasPrefix(typ, "github:") && !strings.HasSuffix(typ, ":Team") {
			declared = append(declared, name)
		}
	}

	sort.Strings(declared)

	required, optional := staterename.ResourceNames(reg, renameOrg, newRepo)
	want := append(append([]string{}, required...), optional...)
	sort.Strings(want)

	assert.Equal(t, want, declared)
	assert.Contains(t, optional, "workflow-perms-"+newRepo, "declared only once the repository has commits")
}

// A repository with no commits declares no workflow permissions: that is
// the optional name.
func TestStateRenameOptionalNameIsTheOneAnEmptyRepositorySkips(t *testing.T) {
	reg, orgCfg := renameRegistry(t, newRepo)

	live := liveWith(newRepo, rowSlugs(reg, orgCfg, newRepo))
	live.emptyRepos[newRepo] = true

	h := &harness{apiURL: "http://127.0.0.1:1/"}
	m := &nameMocks{names: map[string]string{}}

	require.NoError(t, pulumi.RunErr(h.program(reg, orgCfg, live, nil), pulumi.WithMocks(renameProject, renameOrg, m)))

	required, optional := staterename.ResourceNames(reg, renameOrg, newRepo)

	for _, n := range required {
		assert.Contains(t, m.names, n)
	}

	for _, n := range optional {
		assert.NotContains(t, m.names, n)
	}
}
