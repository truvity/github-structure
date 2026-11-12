package engine

import (
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi-github/sdk/v6/go/github"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	registry "github.com/truvity/github-structure/pkg/registry"
	"github.com/truvity/github-structure/pkg/staterename"
)

// envMocks records every resource's inputs and import id by name.
type envMocks struct {
	mu      sync.Mutex
	inputs  map[string]resource.PropertyMap
	imports map[string]string
	types   map[string]string
}

func newEnvMocks() *envMocks {
	return &envMocks{
		inputs:  map[string]resource.PropertyMap{},
		imports: map[string]string{},
		types:   map[string]string{},
	}
}

func (m *envMocks) NewResource(a pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.inputs[a.Name] = a.Inputs
	m.types[a.Name] = a.TypeToken

	if a.ID != "" {
		m.imports[a.Name] = a.ID
	}

	if a.TypeToken == "github:index/team:Team" {
		return renameTeamIDs[strings.TrimPrefix(a.Name, "team-")], a.Inputs, nil
	}

	return a.Name + "_id", a.Inputs, nil
}

func (m *envMocks) Call(a pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return a.Args, nil
}

const envRepo = "widget"

func envRow() *registry.Repo {
	return &registry.Repo{Environments: map[string]registry.Environment{
		"dev":   {},
		"stage": {Branches: []string{"main"}, Tags: []string{"v*"}},
		"prod": {
			Branches: []string{"main"}, Tags: []string{"v*"},
			ReviewerTeams: []string{"engineers"}, PreventSelfReview: true,
		},
	}}
}

func runEnvironments(t *testing.T, row *registry.Repo, live *liveState) *envMocks {
	t.Helper()

	m := newEnvMocks()

	require.NoError(t, pulumi.RunErr(func(c *pulumi.Context) error {
		provider, err := github.NewProvider(c, "gh", &github.ProviderArgs{})
		if err != nil {
			return err
		}

		team, err := github.NewTeam(c, "team-engineers", &github.TeamArgs{Name: pulumi.String("engineers")})
		if err != nil {
			return err
		}

		repo, err := github.NewRepository(c, "repo-"+envRepo, &github.RepositoryArgs{Name: pulumi.String(envRepo)})
		if err != nil {
			return err
		}

		return deployEnvironments(c, envRepo, repo, row, map[string]*github.Team{"engineers": team}, live, provider)
	}, pulumi.WithMocks("proj", "stack", m)))

	return m
}

func emptyEnvLive() *liveState {
	return &liveState{
		environments: map[string]map[string]bool{},
		envPolicies:  map[string]map[string]string{},
	}
}

// Environments are declared from the row: an empty one has no branch policy,
// a restricted one has a custom policy and a policy resource per pattern, and
// a reviewed one carries its team's numeric id and the self-review setting.
func TestDeployEnvironmentsPlan(t *testing.T) {
	m := runEnvironments(t, envRow(), emptyEnvLive())

	dev := m.inputs["environment-widget-dev"]
	require.NotNil(t, dev)
	assert.Equal(t, "dev", dev["environment"].StringValue())
	assert.False(t, dev["preventSelfReview"].BoolValue())
	assert.NotContains(t, dev, resource.PropertyKey("deploymentBranchPolicy"), "no policy: any ref may deploy")
	assert.NotContains(t, dev, resource.PropertyKey("reviewers"))

	stage := m.inputs["environment-widget-stage"]
	require.NotNil(t, stage)

	policy := stage["deploymentBranchPolicy"].ObjectValue()
	assert.True(t, policy["customBranchPolicies"].BoolValue())
	assert.False(t, policy["protectedBranches"].BoolValue())
	assert.NotContains(t, stage, resource.PropertyKey("reviewers"))

	prod := m.inputs["environment-widget-prod"]
	require.NotNil(t, prod)
	assert.True(t, prod["preventSelfReview"].BoolValue())

	reviewers := prod["reviewers"].ArrayValue()
	require.Len(t, reviewers, 1)

	teams := reviewers[0].ObjectValue()["teams"].ArrayValue()
	require.Len(t, teams, 1)
	assert.EqualValues(t, 102, teams[0].NumberValue(), "the team's numeric id, not its slug")

	branch := m.inputs["environment-policy-widget-stage-branch-main"]
	require.NotNil(t, branch)
	assert.Equal(t, "main", branch["branchPattern"].StringValue())
	assert.Equal(t, "stage", branch["environment"].StringValue())
	assert.NotContains(t, branch, resource.PropertyKey("tagPattern"))

	tag := m.inputs["environment-policy-widget-stage-tag-v*"]
	require.NotNil(t, tag)
	assert.Equal(t, "v*", tag["tagPattern"].StringValue())
	assert.NotContains(t, tag, resource.PropertyKey("branchPattern"))

	assert.Contains(t, m.inputs, "environment-policy-widget-prod-branch-main")
	assert.Contains(t, m.inputs, "environment-policy-widget-prod-tag-v*")

	for name := range m.inputs {
		assert.NotContains(t, name, "environment-policy-widget-dev", "an empty environment has no policy")
	}

	assert.Empty(t, m.imports, "nothing is live: everything is created")
}

// A live environment is imported by "repo:environment", a live policy by
// "repo:environment:id"; what is not live is created.
func TestDeployEnvironmentsImportsWhatIsLive(t *testing.T) {
	live := emptyEnvLive()
	live.environments[envRepo] = map[string]bool{"dev": true, "stage": true}
	live.envPolicies[envRepo+"/stage"] = map[string]string{"branch:main": "11"}

	m := runEnvironments(t, envRow(), live)

	assert.Equal(t, map[string]string{
		"environment-widget-dev":                      "widget:dev",
		"environment-widget-stage":                    "widget:stage",
		"environment-policy-widget-stage-branch-main": "widget:stage:11",
	}, m.imports)
}

// A row with no environments declares none, and an environment the row does
// not name is not something the engine knows about.
func TestDeployEnvironmentsDeclaresNothingForAPlainRow(t *testing.T) {
	m := runEnvironments(t, &registry.Repo{}, emptyEnvLive())

	for name, typ := range m.types {
		assert.NotContains(t, typ, "Environment", name)
	}
}

// The state re-key spells these names too; the engine and the re-key must
// agree or a rename would refuse a valid state.
func TestEnvironmentNamesMatchTheStateRename(t *testing.T) {
	m := runEnvironments(t, envRow(), emptyEnvLive())

	var declared []string

	for name, typ := range m.types {
		if strings.Contains(typ, "RepositoryEnvironment") {
			declared = append(declared, name)
		}
	}

	sort.Strings(declared)

	reg := &registry.Config{
		Presets: map[string]*registry.RepoSettings{"p": {}},
		Orgs:    map[string]*registry.Org{"acme": {Repos: map[string]*registry.Repo{envRepo: envRow()}}},
	}

	required, _ := staterename.ResourceNames(reg, "acme", envRepo)

	var want []string

	for _, n := range required {
		if strings.HasPrefix(n, "environment-") {
			want = append(want, n)
		}
	}

	sort.Strings(want)

	assert.Equal(t, want, declared)
}
