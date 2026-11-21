package engine

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/pulumi/pulumi-github/sdk/v6/go/github"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	app "github.com/truvity/github-structure/pkg/app"
	registry "github.com/truvity/github-structure/pkg/registry"
	"github.com/truvity/github-structure/pkg/staterename"
)

const releaseRoleARN = "arn:test:iam::1234:role/release-widget"

func varRow() *registry.Repo {
	return &registry.Repo{Variables: map[string]string{
		"RELEASE_ROLE_ARN": releaseRoleARN,
		"deploy_region":    "eu-central-1",
	}}
}

func emptyVarLive() *liveState {
	return &liveState{variables: map[string]map[string]bool{}}
}

func runVariables(t *testing.T, row *registry.Repo, live *liveState) *envMocks {
	t.Helper()

	m := newEnvMocks()

	require.NoError(t, pulumi.RunErr(func(c *pulumi.Context) error {
		provider, err := github.NewProvider(c, "gh", &github.ProviderArgs{})
		if err != nil {
			return err
		}

		repo, err := github.NewRepository(c, "repo-"+envRepo, &github.RepositoryArgs{Name: pulumi.String(envRepo)})
		if err != nil {
			return err
		}

		return deployVariables(c, envRepo, repo, row, live, provider)
	}, pulumi.WithMocks("proj", "stack", m)))

	return m
}

// A variable is declared from the row, one resource each, with GitHub's
// upper-case spelling of the name.
func TestDeployVariablesPlan(t *testing.T) {
	m := runVariables(t, varRow(), emptyVarLive())

	role := m.inputs["variable-widget-RELEASE_ROLE_ARN"]
	require.NotNil(t, role)
	assert.Equal(t, "RELEASE_ROLE_ARN", role["variableName"].StringValue())
	assert.Equal(t, releaseRoleARN, role["value"].StringValue())
	assert.Equal(t, envRepo, role["repository"].StringValue())

	region := m.inputs["variable-widget-DEPLOY_REGION"]
	require.NotNil(t, region, "the name is declared in upper case, as GitHub keeps it")
	assert.Equal(t, "DEPLOY_REGION", region["variableName"].StringValue())

	assert.Empty(t, m.imports, "nothing is live: everything is created")
}

// A variable set by hand is imported by "repo:NAME", never created over (that
// is the conflict the import exists to avoid); what is not live is created.
func TestDeployVariablesImportsWhatIsLive(t *testing.T) {
	live := emptyVarLive()
	live.variables[envRepo] = map[string]bool{"RELEASE_ROLE_ARN": true}

	m := runVariables(t, varRow(), live)

	assert.Equal(t, map[string]string{
		"variable-widget-RELEASE_ROLE_ARN": "widget:RELEASE_ROLE_ARN",
	}, m.imports)
}

// A row with no variables declares none.
func TestDeployVariablesDeclaresNothingForAPlainRow(t *testing.T) {
	m := runVariables(t, &registry.Repo{}, emptyVarLive())

	for name, typ := range m.types {
		assert.NotContains(t, typ, "ActionsVariable", name)
	}
}

// The state re-key spells these names too; the engine and the re-key must
// agree or a rename would refuse a valid state.
func TestVariableNamesMatchTheStateRename(t *testing.T) {
	m := runVariables(t, varRow(), emptyVarLive())

	var declared []string

	for name, typ := range m.types {
		if strings.Contains(typ, "ActionsVariable") {
			declared = append(declared, name)
		}
	}

	sort.Strings(declared)

	reg := &registry.Config{
		Presets: map[string]*registry.RepoSettings{"p": {}},
		Orgs:    map[string]*registry.Org{"acme": {Repos: map[string]*registry.Repo{envRepo: varRow()}}},
	}

	required, _ := staterename.ResourceNames(reg, "acme", envRepo)

	var want []string

	for _, n := range required {
		if strings.HasPrefix(n, "variable-") {
			want = append(want, n)
		}
	}

	sort.Strings(want)

	assert.Equal(t, want, declared)
}

func varOrg() *registry.Org {
	return &registry.Org{Repos: map[string]*registry.Repo{
		"widget":   {Variables: map[string]string{"RELEASE_ROLE_ARN": releaseRoleARN, "OTHER": "x"}},
		"plain":    {},
		"archived": {Archived: true, Variables: map[string]string{"A": "b"}},
		"missing":  {Variables: map[string]string{"A": "b"}},
	}}
}

const varsPath = "/repos/acme/widget/actions/variables?per_page=30&page=1"

// A variable is imported only when it is live; only an existing repository
// that declares some is asked about.
func TestReadVariables(t *testing.T) {
	t.Parallel()

	get := &fakeGetter{answers: map[string]string{
		varsPath: `{"total_count":1,"variables":[{"name":"RELEASE_ROLE_ARN","value":"x"}]}`,
	}}
	s := &liveState{
		repos:     map[string]bool{"widget": true, "plain": true, "archived": true},
		variables: map[string]map[string]bool{},
	}

	require.NoError(t, s.readVariables(t.Context(), get, "acme", varOrg()))

	assert.True(t, s.variableExists("widget", "RELEASE_ROLE_ARN"))
	assert.True(t, s.variableExists("widget", "release_role_arn"), "GitHub keeps the name in upper case")
	assert.False(t, s.variableExists("widget", "OTHER"), "declared and not live: created")
	assert.False(t, s.variableExists("missing", "A"))
	assert.Equal(t, []string{varsPath}, get.asked)
}

// More variables than a page holds are read page by page.
func TestReadVariablesPages(t *testing.T) {
	t.Parallel()

	names := make([]string, 0, 30)
	for i := range 30 {
		names = append(names, fmt.Sprintf(`{"name":"V%d"}`, i))
	}

	first := `{"total_count":31,"variables":[` + strings.Join(names, ",") + `]}`

	get := &fakeGetter{answers: map[string]string{
		varsPath: first,
		"/repos/acme/widget/actions/variables?per_page=30&page=2": `{"total_count":31,"variables":[{"name":"LAST"}]}`,
	}}
	s := &liveState{repos: map[string]bool{"widget": true}, variables: map[string]map[string]bool{}}

	require.NoError(t, s.readVariables(t.Context(), get, "acme",
		&registry.Org{Repos: map[string]*registry.Repo{"widget": {Variables: map[string]string{"LAST": "x"}}}}))

	assert.True(t, s.variableExists("widget", "LAST"), "the second page is read")
	assert.Len(t, get.asked, 2)
}

// A repository with no variables declared costs no API call, so an estate
// that never uses them never needs the App's Variables permission.
func TestReadVariablesAsksNothingWhenNoneAreDeclared(t *testing.T) {
	t.Parallel()

	get := &fakeGetter{}
	s := &liveState{repos: map[string]bool{"plain": true}, variables: map[string]map[string]bool{}}

	require.NoError(t, s.readVariables(t.Context(), get, "acme",
		&registry.Org{Repos: map[string]*registry.Repo{"plain": {}}}))
	assert.Empty(t, get.asked)
}

// A missing permission answers 403 or 404, which must stop the run and say
// what to grant, not read as "no variables".
func TestReadVariablesRefusesAnUnreadableAnswer(t *testing.T) {
	t.Parallel()

	for name, err := range map[string]error{
		"forbidden": assert.AnError,
		"not found": app.ErrNotFound,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			get := &fakeGetter{errs: map[string]error{varsPath: err}}
			s := &liveState{repos: map[string]bool{"widget": true}, variables: map[string]map[string]bool{}}

			got := s.readVariables(t.Context(), get, "acme",
				&registry.Org{Repos: map[string]*registry.Repo{"widget": {Variables: map[string]string{"A": "b"}}}})
			require.Error(t, got)
			assert.Contains(t, got.Error(), `"Variables": read and write`)
		})
	}
}
