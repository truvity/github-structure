package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	app "github.com/truvity/github-structure/pkg/app"
	registry "github.com/truvity/github-structure/pkg/registry"
)

// The installations read costs the engine App an
// organization_administration permission, so it is asked for only where
// a ruleset actually names a bypass App. A permission demanded for a
// fact nobody wanted is a permission that gets granted rather than
// questioned.
func TestNamesBypassApps(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		org  *registry.Org
		want bool
	}{
		"no rulesets at all": {
			org:  &registry.Org{Repos: map[string]*registry.Repo{"widget": {}}},
			want: false,
		},
		"rulesets bypassed by admins and teams only": {
			org: &registry.Org{Repos: map[string]*registry.Repo{"widget": {
				BranchRulesets: []*registry.BranchRuleset{{Name: "pr-approval", BypassOrgAdmins: true}},
				TagRulesets:    []*registry.TagRuleset{{Name: "tags", BypassTeams: []string{"engineers"}}},
			}}},
			want: false,
		},
		"a branch ruleset names one": {
			org: &registry.Org{Repos: map[string]*registry.Repo{"widget": {
				BranchRulesets: []*registry.BranchRuleset{{Name: "master-check", BypassApps: []string{"acme-releases"}}},
			}}},
			want: true,
		},
		"a tag ruleset names one": {
			org: &registry.Org{Repos: map[string]*registry.Repo{"widget": {
				TagRulesets: []*registry.TagRuleset{{Name: "tags", BypassApps: []string{"acme-releases"}}},
			}}},
			want: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, namesBypassApps(tc.org))
		})
	}
}

// fakeGetter answers readEnvironments' reads from canned JSON by path.
type fakeGetter struct {
	answers map[string]string
	errs    map[string]error
	asked   []string
}

func (f *fakeGetter) Get(_ context.Context, path string, dst any) error {
	f.asked = append(f.asked, path)

	if err, ok := f.errs[path]; ok {
		return err
	}

	body, ok := f.answers[path]
	if !ok {
		return app.ErrNotFound
	}

	return json.Unmarshal([]byte(body), dst)
}

func envOrg() *registry.Org {
	return &registry.Org{Repos: map[string]*registry.Repo{
		"widget": {Environments: map[string]registry.Environment{
			"dev":   {},
			"stage": {Branches: []string{"main"}, Tags: []string{"v*"}},
			"prod":  {Branches: []string{"main"}},
		}},
		"plain":    {},
		"archived": {Archived: true, Environments: map[string]registry.Environment{"dev": {}}},
		"missing":  {Environments: map[string]registry.Environment{"dev": {}}},
	}}
}

// An environment is imported only when it is live, and a deployment policy
// only when one of the same kind and pattern is live: the numeric id is part
// of its import id.
func TestReadEnvironments(t *testing.T) {
	t.Parallel()

	get := &fakeGetter{answers: map[string]string{
		"/repos/acme/widget/environments?per_page=100": `{"total_count":2,"environments":[{"name":"stage"},{"name":"dev"}]}`,
		"/repos/acme/widget/environments/stage/deployment-branch-policies?per_page=100": `{"total_count":2,"branch_policies":[` +
			`{"id":11,"name":"main","type":"branch"},{"id":12,"name":"v*","type":"tag"}]}`,
	}}

	s := &liveState{
		repos:        map[string]bool{"widget": true, "plain": true, "archived": true},
		environments: map[string]map[string]bool{},
		envPolicies:  map[string]map[string]string{},
	}

	require.NoError(t, s.readEnvironments(t.Context(), get, "acme", envOrg()))

	assert.True(t, s.environmentExists("widget", "dev"))
	assert.True(t, s.environmentExists("widget", "stage"))
	assert.False(t, s.environmentExists("widget", "prod"), "declared and not live: created")
	assert.False(t, s.environmentExists("missing", "dev"), "a repository that does not exist has no environments")

	id, ok := s.policyID("widget", "stage", "branch", "main")
	assert.True(t, ok)
	assert.Equal(t, "11", id)

	id, ok = s.policyID("widget", "stage", "tag", "v*")
	assert.True(t, ok)
	assert.Equal(t, "12", id)

	_, ok = s.policyID("widget", "stage", "tag", "main")
	assert.False(t, ok, "the kind is part of the key")

	// Asked of GitHub: the one existing repository that declares some. Not a
	// repository without environments, an archived one, one that is not live
	// yet, and no policies for an environment that is not live or has no
	// branch policy.
	assert.Equal(t, []string{
		"/repos/acme/widget/environments?per_page=100",
		"/repos/acme/widget/environments/stage/deployment-branch-policies?per_page=100",
	}, get.asked)
}

// A repository with no environments declared costs no API call, so an estate
// that never uses them never needs the App's Environments permission.
func TestReadEnvironmentsAsksNothingWhenNoneAreDeclared(t *testing.T) {
	t.Parallel()

	get := &fakeGetter{}
	s := &liveState{repos: map[string]bool{"plain": true}, environments: map[string]map[string]bool{}, envPolicies: map[string]map[string]string{}}

	require.NoError(t, s.readEnvironments(t.Context(), get, "acme",
		&registry.Org{Repos: map[string]*registry.Repo{"plain": {}}}))
	assert.Empty(t, get.asked)
}

// A missing permission answers 403 or 404, which must stop the run and say
// what to grant, not read as "no environments" and plan creates that then
// conflict with the live ones.
func TestReadEnvironmentsRefusesAnUnreadableAnswer(t *testing.T) {
	t.Parallel()

	for name, err := range map[string]error{
		"forbidden": errors.New("GET: 403 Forbidden: Resource not accessible by integration"),
		"not found": app.ErrNotFound,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			get := &fakeGetter{errs: map[string]error{"/repos/acme/widget/environments?per_page=100": err}}
			s := &liveState{repos: map[string]bool{"widget": true}, environments: map[string]map[string]bool{}, envPolicies: map[string]map[string]string{}}

			got := s.readEnvironments(t.Context(), get, "acme",
				&registry.Org{Repos: map[string]*registry.Repo{"widget": {Environments: map[string]registry.Environment{"dev": {}}}}})
			require.Error(t, got)
			assert.Contains(t, got.Error(), `"Environments": read and write`)
		})
	}
}
