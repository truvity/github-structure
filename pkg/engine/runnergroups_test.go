package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pulumi/pulumi-github/sdk/v6/go/github"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	app "github.com/truvity/github-structure/pkg/app"
	registry "github.com/truvity/github-structure/pkg/registry"
)

const rgTypeToken = "github:index/actionsRunnerGroup:ActionsRunnerGroup"

// runRunnerGroups declares the org's repositories (ids 1000, 1001, … in name
// order) and then its runner groups.
func runRunnerGroups(t *testing.T, orgCfg *registry.Org, live *liveState) *repoIDMocks {
	t.Helper()

	m := &repoIDMocks{envMocks: *newEnvMocks(), ids: map[string]int{}}
	for i, name := range orgCfg.SortedRepos() {
		m.ids[name] = 1000 + i
	}

	require.NoError(t, pulumi.RunErr(func(c *pulumi.Context) error {
		provider, err := github.NewProvider(c, "gh", &github.ProviderArgs{})
		if err != nil {
			return err
		}

		ids := map[string]pulumi.IntOutput{}

		for _, name := range orgCfg.SortedRepos() {
			repo, err := github.NewRepository(c, "repo-"+name, &github.RepositoryArgs{Name: pulumi.String(name)})
			if err != nil {
				return err
			}

			ids[name] = repo.RepoId
		}

		return deployRunnerGroups(c, orgCfg, ids, live, provider)
	}, pulumi.WithMocks("proj", "stack", m)))

	return m
}

func emptyRGLive() *liveState { return &liveState{runnerGroups: map[string]int64{}} }

func intList(v resource.PropertyValue) []int {
	var out []int
	for _, e := range v.ArrayValue() {
		out = append(out, int(e.NumberValue()))
	}

	return out
}

// The three shapes the estate's stable tiers have today (2026-11-24,
// kernel hack/grun-runner-groups.sh --check): an org-wide group on every
// private repository, and a group on named repositories. Neither restricts
// workflows. These inputs are what the live groups hold, so importing them
// plans no change.
func TestDeployRunnerGroupsReproducesTheStableTiers(t *testing.T) {
	t.Run("org-wide (trust-form, finttech)", func(t *testing.T) {
		org := &registry.Org{
			Repos:        map[string]*registry.Repo{"wallet": {}},
			RunnerGroups: map[string]*registry.RunnerGroup{"stable": {Visibility: "all"}},
		}
		live := emptyRGLive()
		live.runnerGroups["stable"] = 3
		live.runnerGroups["Default"] = 1

		m := runRunnerGroups(t, org, live)

		in := m.inputs["runner-group-stable"]
		require.NotNil(t, in)
		assert.Equal(t, "stable", in["name"].StringValue())
		assert.Equal(t, "all", in["visibility"].StringValue())
		assert.False(t, in["allowsPublicRepositories"].BoolValue())
		assert.False(t, in["restrictedToWorkflows"].BoolValue())
		assert.NotContains(t, in, resource.PropertyKey("selectedRepositoryIds"), "visibility all names no repository")
		assert.NotContains(t, in, resource.PropertyKey("selectedWorkflows"), "no restriction, no list")
		assert.Equal(t, map[string]string{"runner-group-stable": "3"}, onlyRunnerGroups(m))
	})

	t.Run("named repositories (truvity)", func(t *testing.T) {
		repos := []string{"dms", "eudi", "keycloak", "docs", "arch", "incidents"}
		org := &registry.Org{
			Repos:        map[string]*registry.Repo{"unrelated": {}},
			RunnerGroups: map[string]*registry.RunnerGroup{"stable-pilot": {Visibility: "selected", Repos: repos}},
		}
		for _, r := range repos {
			org.Repos[r] = &registry.Repo{}
		}

		live := emptyRGLive()
		live.runnerGroups["stable-pilot"] = 3

		m := runRunnerGroups(t, org, live)

		in := m.inputs["runner-group-stable-pilot"]
		require.NotNil(t, in)
		assert.Equal(t, "selected", in["visibility"].StringValue())

		var want []int
		for _, r := range []string{"arch", "dms", "docs", "eudi", "incidents", "keycloak"} {
			want = append(want, m.ids[r])
		}

		assert.Equal(t, want, intList(in["selectedRepositoryIds"]), "the declared repositories' ids, and only theirs")
		assert.Equal(t, map[string]string{"runner-group-stable-pilot": "3"}, onlyRunnerGroups(m))
	})
}

func onlyRunnerGroups(m *repoIDMocks) map[string]string {
	out := map[string]string{}

	for name, id := range m.imports {
		if m.types[name] == rgTypeToken {
			out[name] = id
		}
	}

	return out
}

// A group that is not live is created; a workflow restriction renders the
// list in the row's order.
func TestDeployRunnerGroupsCreatesWhatIsNotLive(t *testing.T) {
	org := &registry.Org{
		Repos: map[string]*registry.Repo{"widget": {}},
		RunnerGroups: map[string]*registry.RunnerGroup{"release": {
			Visibility:            "selected",
			Repos:                 []string{"widget"},
			RestrictedToWorkflows: true,
			SelectedWorkflows:     []string{"acme/widget/.github/workflows/z.yaml@refs/heads/master", "acme/widget/.github/workflows/a.yaml@refs/heads/master"},
		}},
	}

	m := runRunnerGroups(t, org, emptyRGLive())

	in := m.inputs["runner-group-release"]
	require.NotNil(t, in)
	assert.Equal(t, rgTypeToken, m.types["runner-group-release"])
	assert.True(t, in["restrictedToWorkflows"].BoolValue())

	var workflows []string
	for _, w := range in["selectedWorkflows"].ArrayValue() {
		workflows = append(workflows, w.StringValue())
	}

	assert.Equal(t, org.RunnerGroups["release"].SelectedWorkflows, workflows)
	assert.Empty(t, onlyRunnerGroups(m), "nothing is live: created")
}

// An org that declares no runner group declares no runner-group resource.
func TestDeployRunnerGroupsDeclaresNothingByDefault(t *testing.T) {
	m := runRunnerGroups(t, &registry.Org{Repos: map[string]*registry.Repo{"widget": {}}}, emptyRGLive())

	for name, typ := range m.types {
		assert.NotEqual(t, rgTypeToken, typ, name)
	}
}

const rgPath = "/orgs/acme/actions/runner-groups?per_page=100&page=1"

func rgOrg() *registry.Org {
	return &registry.Org{RunnerGroups: map[string]*registry.RunnerGroup{"stable": {Visibility: "all"}}}
}

// The live groups are read by name to id.
func TestReadRunnerGroups(t *testing.T) {
	t.Parallel()

	get := &fakeGetter{answers: map[string]string{
		rgPath: `{"total_count":2,"runner_groups":[{"id":1,"name":"Default"},{"id":3,"name":"stable"}]}`,
	}}
	s := emptyRGLive()

	require.NoError(t, s.readRunnerGroups(t.Context(), get, "acme", rgOrg()))
	assert.Equal(t, map[string]int64{"Default": 1, "stable": 3}, s.runnerGroups)
	assert.Equal(t, []string{rgPath}, get.asked)
}

// More groups than a page holds are read page by page.
func TestReadRunnerGroupsPages(t *testing.T) {
	t.Parallel()

	groups := make([]string, 0, runnerGroupsPageSize)
	for i := range runnerGroupsPageSize {
		groups = append(groups, fmt.Sprintf(`{"id":%d,"name":"g%d"}`, i+10, i))
	}

	get := &fakeGetter{answers: map[string]string{
		rgPath: `{"total_count":101,"runner_groups":[` + strings.Join(groups, ",") + `]}`,
		"/orgs/acme/actions/runner-groups?per_page=100&page=2": `{"total_count":101,"runner_groups":[{"id":7,"name":"stable"}]}`,
	}}
	s := emptyRGLive()

	require.NoError(t, s.readRunnerGroups(t.Context(), get, "acme", rgOrg()))
	assert.Equal(t, int64(7), s.runnerGroups["stable"], "the second page is read")
	assert.Len(t, get.asked, 2)
}

// An org with no runner group declared costs no API call, so an estate that
// never declares one never needs the App's "Self-hosted runners" permission.
func TestReadRunnerGroupsAsksNothingWhenNoneAreDeclared(t *testing.T) {
	t.Parallel()

	get := &fakeGetter{}
	s := emptyRGLive()

	require.NoError(t, s.readRunnerGroups(t.Context(), get, "acme", &registry.Org{}))
	assert.Empty(t, get.asked)
}

// A missing permission answers 403 or 404, which must stop the run and name
// the permission, not read as "no groups" and plan a conflicting create.
func TestReadRunnerGroupsRefusesAnUnreadableAnswer(t *testing.T) {
	t.Parallel()

	for name, err := range map[string]error{
		"forbidden": assert.AnError,
		"not found": app.ErrNotFound,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			get := &fakeGetter{errs: map[string]error{rgPath: err}}
			got := emptyRGLive().readRunnerGroups(t.Context(), get, "acme", rgOrg())
			require.Error(t, got)
			assert.Contains(t, got.Error(), "organization_self_hosted_runners")
		})
	}
}
