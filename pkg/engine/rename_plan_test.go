package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/pulumi/pulumi-github/sdk/v6/go/github"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-structure/pkg/registry"
)

// The question this file answers, with the real Pulumi engine and the real
// GitHub provider rather than mocks: when a repository row is renamed (same
// repository, new name), what does the engine plan?
//
// Mocks cannot answer it. A mock sees what the program declares; whether a
// declaration is a create, an update, a replace or nothing is decided by the
// Pulumi engine (URNs, aliases, import ids) and by the provider's schema
// (which inputs force a replacement). So these tests run `pulumi preview`
// against a local file backend, with the provider pointed at a fake GitHub
// on 127.0.0.1 that answers the configure-time organization read and
// nothing else. Nothing leaves the machine and no organization is touched.
//
// They need the pulumi CLI on PATH and the github provider plugin (the CLI
// downloads it on first use). Without the CLI they skip, and say so. CI
// carries no pulumi CLI, so there they always skip: they are a proof to run
// locally when the engine, the provider or this file changes, not a gate.

const (
	renameOrg     = "acme"
	renameProject = "rename-proof"
	oldRepo       = "widget"
	newRepo       = "gadget"
	// providerID is the id the synthetic state gives the provider.
	providerID = "4f7c0b2e-1a52-4d1e-9a3b-6c1d2e3f4a5b"
	// unknown is how a preview prints a value not known until apply.
	unknown = "04da6b54-80e4-46f7-96ec-b56ff0331ba9"
	// repoNodeID is the GraphQL node id GitHub gave the repository. It
	// does not change when the repository is renamed.
	repoNodeID = "R_kgDOfixture"
)

// tfSchemaVersions are the upstream Terraform schema versions of the
// provider's resources that have one (terraform-provider-github v6, as
// bundled by pulumi-github v6.15.0).
var tfSchemaVersions = map[string]int{ //nolint:gochecknoglobals // fixture
	"github:index/repository:Repository":               1,
	"github:index/branchProtection:BranchProtection":   2,
	"github:index/repositoryRuleset:RepositoryRuleset": 1,
}

// renameTeamIDs are the numeric ids GitHub gave the two teams.
var renameTeamIDs = map[string]string{"management": "101", "engineers": "102"} //nolint:gochecknoglobals // fixture

// rulesetIDs are the numeric ids GitHub gave the row's two rulesets.
var rulesetIDs = map[string]string{"master-check": "9001", "release-tags": "9002"} //nolint:gochecknoglobals // fixture

// renameRegistry is the test registry with its one row named repo and
// shaped like the platform repository's: branch protection, a branch
// ruleset with a team and org-admin bypass, a tag ruleset, Actions settings,
// and team grants from an access bundle plus a per-repo team.
func renameRegistry(t *testing.T, repo string) (*registry.Config, *registry.Org) {
	t.Helper()

	base, err := os.ReadFile("../cli/testdata/github.yaml")
	require.NoError(t, err)

	const row = "      widget:\n        preset: public\n        access: [managers]\n"
	require.Contains(t, string(base), row, "fixture row moved; update this test")

	doc := strings.Replace(string(base), row,
		"      "+repo+":\n"+
			"        preset: public\n"+
			"        access: [managers]\n"+
			"        teams:\n"+
			"          engineers: push\n"+
			"        tag_rulesets:\n"+
			"          - name: release-tags\n"+
			"            pattern: refs/tags/v*\n"+
			"            bypass_teams: [management]\n"+
			"        branch_rulesets:\n"+
			"          - name: master-check\n"+
			"            pattern: ~DEFAULT_BRANCH\n"+
			"            required_approvals: 1\n"+
			"            required_checks: [check]\n"+
			"            bypass_apps: []\n"+
			"            bypass_org_admins: true\n"+
			"            bypass_teams: [management]\n", 1)

	reg, err := registry.Load(fstest.MapFS{"github.yaml": {Data: []byte(doc)}})
	require.NoError(t, err)

	return reg, reg.Orgs[renameOrg]
}

// liveWith is what GitHub holds: both teams, the repository under the name
// it has live, and every grant on it. With no name, nothing exists yet.
func liveWith(name string, grants []string) *liveState {
	live := &liveState{
		teamIDs:    map[string]string{},
		repos:      map[string]bool{},
		emptyRepos: map[string]bool{},
		grants:     map[string]map[string]bool{},
	}

	if name == "" {
		return live
	}

	for slug, id := range renameTeamIDs {
		live.teamIDs[slug] = id
	}

	live.repos[name] = true

	for _, slug := range grants {
		live.grants[slug] = map[string]bool{name: true}
	}

	return live
}

// program is the engine's team and repository half, as Deploy
// declares it. transform, when set, is registered on the stack first.
func (h *harness) program(reg *registry.Config, orgCfg *registry.Org, live *liveState, transform pulumi.ResourceTransformation) pulumi.RunFunc {
	return func(c *pulumi.Context) error {
		if transform != nil {
			if err := c.RegisterStackTransformation(transform); err != nil {
				return err
			}
		}

		provider, err := github.NewProvider(c, "github-"+renameOrg, &github.ProviderArgs{
			Owner:   pulumi.String(renameOrg),
			BaseUrl: pulumi.String(h.apiURL),
		})
		if err != nil {
			return err
		}

		teams, err := deployTeams(c, nil, renameOrg, orgCfg, live, provider)
		if err != nil {
			return err
		}

		return deployRepos(c, renameOrg, reg, orgCfg, teams, live, provider)
	}
}

// fakeGitHub answers the provider's configure-time organization read and
// 404s everything else. Every request is recorded, so a test can prove the
// preview attempted no write.
type fakeGitHub struct {
	mu       sync.Mutex
	requests []string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/orgs/"+renameOrg) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"login":%q,"id":1,"type":"Organization"}`, renameOrg)

		return
	}

	http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
}

func (f *fakeGitHub) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []string

	for _, r := range f.requests {
		if !strings.HasPrefix(r, http.MethodGet+" ") {
			out = append(out, r)
		}
	}

	return out
}

// harness is one stack on a local file backend, and its fake GitHub.
type harness struct {
	t      *testing.T
	stack  auto.Stack
	gh     *fakeGitHub
	apiURL string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	if _, err := exec.LookPath("pulumi"); err != nil {
		t.Skip("SKIPPED, NOT PASSED: no pulumi CLI on PATH; this proof needs the real engine and provider")
	}

	gh := &fakeGitHub{}
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)

	stack, err := auto.UpsertStackInlineSource(t.Context(), renameOrg, renameProject,
		func(*pulumi.Context) error { return nil },
		auto.Project(workspace.Project{Name: tokens.PackageName(renameProject), Runtime: workspace.NewProjectRuntimeInfo("go", nil)}),
		auto.WorkDir(t.TempDir()),
		auto.PulumiHome(pulumiHome(t)),
		auto.EnvVars(map[string]string{
			"PULUMI_BACKEND_URL":       "file://" + t.TempDir(),
			"PULUMI_CONFIG_PASSPHRASE": "rename-proof",
			"PULUMI_SKIP_UPDATE_CHECK": "true",
			// A token the fake API does not check. The API itself is the
			// provider's baseUrl, set by the program.
			"GITHUB_TOKEN": "not-a-real-token",
		}),
	)
	require.NoError(t, err)

	return &harness{t: t, stack: stack, gh: gh, apiURL: srv.URL + "/"}
}

// pulumiHome is a PULUMI_HOME of the test's own, so the CLI's workspace
// files and logs go away with the test. Its plugins are the user's, so the
// provider is not downloaded again for every test. It is set on the test
// process too: the SDK runs `pulumi version` with the process environment.
func pulumiHome(t *testing.T) string {
	t.Helper()

	home := t.TempDir()

	userHome := os.Getenv("PULUMI_HOME")
	if userHome == "" {
		if dir, err := os.UserHomeDir(); err == nil {
			userHome = filepath.Join(dir, ".pulumi")
		}
	}

	if plugins := filepath.Join(userHome, "plugins"); userHome != "" {
		if fi, err := os.Stat(plugins); err == nil && fi.IsDir() {
			require.NoError(t, os.Symlink(plugins, filepath.Join(home, "plugins")))
		}
	}

	t.Setenv("PULUMI_HOME", home)

	return home
}

// step is one planned engine operation on a GitHub resource.
type step struct {
	Op   string
	Name string
	Keys []string
}

func (s step) String() string {
	if len(s.Keys) > 0 {
		return fmt.Sprintf("%s %s (replace keys %v)", s.Op, s.Name, s.Keys)
	}

	return s.Op + " " + s.Name
}

// preview runs `pulumi preview` of prog. It returns every planned step on
// a GitHub resource, the planned new state of every resource by URN, and
// the preview's error, if any.
func (h *harness) preview(prog pulumi.RunFunc) ([]step, map[string]*apitype.StepEventStateMetadata, error) {
	h.t.Helper()

	h.stack.Workspace().SetProgram(prog)

	ch := make(chan events.EngineEvent)
	done := make(chan struct{})

	var (
		steps  []step
		states = map[string]*apitype.StepEventStateMetadata{}
	)

	go func() {
		defer close(done)

		for ev := range ch {
			if ev.ResourcePreEvent == nil {
				continue
			}

			m := ev.ResourcePreEvent.Metadata
			if m.New != nil {
				states[m.URN] = m.New
			}

			if strings.HasPrefix(m.Type, "github:") {
				steps = append(steps, step{Op: string(m.Op), Name: urnName(m.URN), Keys: m.Keys})
			}
		}
	}()

	_, err := h.stack.Preview(context.WithoutCancel(h.t.Context()), optpreview.EventStreams(ch))
	<-done

	sort.Slice(steps, func(i, j int) bool {
		if steps[i].Name != steps[j].Name {
			return steps[i].Name < steps[j].Name
		}

		return steps[i].Op < steps[j].Op
	})

	return steps, states, err
}

func urnName(urn string) string { return urn[strings.LastIndex(urn, "::")+2:] }

func (h *harness) urn(typ, name string) string {
	return fmt.Sprintf("urn:pulumi:%s::%s::%s::%s", renameOrg, renameProject, typ, name)
}

// edges are the references to other resources a resource is registered
// with. The engine records them in state as they arrive, and a state
// re-key has to move every one of them.
type edges struct {
	parent      string
	deps        []string
	propDeps    map[string][]string
	deletedWith string
}

// edgeMocks records the RegisterResource request of every resource the
// program declares, in the order the SDK sends them.
type edgeMocks struct {
	h     *harness
	mu    sync.Mutex
	order []string
	edges map[string]edges
}

func (m *edgeMocks) NewResource(a pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	req := a.RegisterRPC
	e := edges{
		parent:      req.GetParent(),
		deps:        req.GetDependencies(),
		propDeps:    map[string][]string{},
		deletedWith: req.GetDeletedWith(),
	}

	for k, v := range req.GetPropertyDependencies() {
		e.propDeps[k] = v.GetUrns()
	}

	urn := m.h.urn(a.TypeToken, a.Name)
	m.order = append(m.order, urn)
	m.edges[urn] = e

	// A team's id is read back as a number (a ruleset's bypass actor).
	if id, ok := renameTeamIDs[strings.TrimPrefix(a.Name, "team-")]; ok && strings.HasSuffix(a.TypeToken, ":Team") {
		return id, a.Inputs, nil
	}

	return a.Name + "_id", a.Inputs, nil
}

func (m *edgeMocks) Call(a pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return a.Args, nil
}

// edgesOf runs prog under mocks and returns the edges the SDK registers
// each resource with, by URN, and the registration order. The SDK sends
// the same requests to the real engine; only the answers are mocked.
func (h *harness) edgesOf(prog pulumi.RunFunc) (map[string]edges, []string) {
	h.t.Helper()

	m := &edgeMocks{h: h, edges: map[string]edges{}}
	require.NoError(h.t, pulumi.RunErr(prog, pulumi.WithMocks(renameProject, renameOrg, m)))

	return m.edges, m.order
}

// refs are every URN a resource refers to.
func (e edges) refs() []string {
	out := append([]string{}, e.deps...)
	for _, d := range e.propDeps {
		out = append(out, d...)
	}

	if e.parent != "" {
		out = append(out, e.parent)
	}

	if e.deletedWith != "" {
		out = append(out, e.deletedWith)
	}

	return out
}

// topoOrder orders urns so that each comes after every resource it refers
// to, as the engine writes a state; ties keep registration order. The CLI
// refuses to import a state that refers to a resource before it appears.
func topoOrder(t *testing.T, urns []string, refs func(string) []string) []string {
	t.Helper()

	placed := map[string]bool{}
	out := make([]string, 0, len(urns))

	for len(out) < len(urns) {
		progress := false

		for _, u := range urns {
			if placed[u] {
				continue
			}

			ready := true

			for _, r := range refs(u) {
				if !placed[r] && slices.Contains(urns, r) {
					ready = false

					break
				}
			}

			if ready {
				placed[u] = true
				out = append(out, u)
				progress = true
			}
		}

		require.True(t, progress, "a cycle among %v", urns)
	}

	return out
}

// seedState writes the state the stack would hold after the engine had
// created and then adopted the row under name: one resource per declared
// GitHub object, with the provider's planned inputs and outputs, the ids
// GitHub would have assigned, the import ids the engine declares, and the
// dependency edges and parent the SDK registers it with, in dependency
// order.
//
// Built from a create preview because nothing can be applied here. The
// control test proves the seed is faithful: previewing the same program
// against it must plan nothing at all, and an update of it writes back
// the same edges.
func (h *harness) seedState(reg *registry.Config, orgCfg *registry.Org, name string) {
	h.t.Helper()

	_, planned, err := h.preview(h.program(reg, orgCfg, liveWith("", nil), nil))
	require.NoError(h.t, err)

	// The edges of the program as it runs once the row exists live: the
	// run that wrote the state being seeded.
	edgesByURN, order := h.edgesOf(h.program(reg, orgCfg, liveWith(name, rowSlugs(reg, orgCfg, name)), nil))

	providerURN := h.urn("pulumi:providers:github", "github-"+renameOrg)
	providerRef := providerURN + "::" + providerID
	stackURN := h.urn("pulumi:pulumi:Stack", renameProject+"-"+renameOrg)

	resources := []apitype.ResourceV3{{
		URN: resource.URN(stackURN), Type: "pulumi:pulumi:Stack",
	}}

	prov := planned[providerURN]
	require.NotNil(h.t, prov, "the provider is declared")

	resources = append(resources, apitype.ResourceV3{
		URN: resource.URN(providerURN), Custom: true, ID: providerID,
		Type: "pulumi:providers:github", Inputs: prov.Inputs, Outputs: prov.Inputs,
		Parent: resource.URN(stackURN),
	})

	// The engine skips workflow permissions while the repository does not
	// exist live, so the create preview cannot plan them: declare them as
	// the engine does once it does.
	r := reg.Resolve(orgCfg.Repos[name])
	wf := map[string]any{
		"repository":                   name,
		"defaultWorkflowPermissions":   r.Actions.DefaultWorkflowPermissions,
		"canApprovePullRequestReviews": r.Actions.CanApprovePullRequests,
	}
	planned[h.urn("github:index/workflowRepositoryPermissions:WorkflowRepositoryPermissions", "workflow-perms-"+name)] =
		&apitype.StepEventStateMetadata{
			Type:   "github:index/workflowRepositoryPermissions:WorkflowRepositoryPermissions",
			Custom: true, RetainOnDelete: true, Inputs: wf, Outputs: wf,
		}

	var urns []string

	for _, u := range order {
		if s := planned[u]; s != nil && strings.HasPrefix(s.Type, "github:") {
			urns = append(urns, u)
		}
	}

	var declared []string

	for u, s := range planned {
		if strings.HasPrefix(s.Type, "github:") {
			declared = append(declared, u)
		}
	}

	require.ElementsMatch(h.t, declared, urns, "the mocked run declares what the preview planned")

	urns = topoOrder(h.t, urns, func(u string) []string { return edgesByURN[u].refs() })

	seeded := 0

	for _, u := range urns {
		s := planned[u]

		id, importID := seedIDs(s.Type, urnName(u), name)
		inputs := resolveUnknowns(s.Inputs)

		// A create preview plans no outputs, so the state's outputs are
		// what the provider would read back: the inputs, plus the
		// computed fields a later input refers to.
		outputs := map[string]any{}
		for k, v := range inputs {
			if k != "__defaults" {
				outputs[k] = v
			}
		}

		for k, v := range seedComputed(s.Type, urnName(u), name) {
			outputs[k] = v
		}

		// A team id read back as an input: the grant's team, the child
		// team's parent.
		switch {
		case strings.HasSuffix(s.Type, ":TeamRepository"):
			team := renameTeamIDs[strings.TrimSuffix(strings.TrimPrefix(urnName(u), "grant-"), "-"+name)]
			inputs["teamId"], outputs["teamId"] = team, team
		case strings.HasSuffix(s.Type, ":Team") && orgCfg.Teams[strings.TrimPrefix(urnName(u), "team-")].Parent != "":
			parent := renameTeamIDs[orgCfg.Teams[strings.TrimPrefix(urnName(u), "team-")].Parent]
			inputs["parentTeamId"], outputs["parentTeamId"] = parent, parent
		}

		// The bridge records the Terraform schema version in every state;
		// without it the provider runs its upgraders on current-shape
		// state and panics.
		if v, ok := tfSchemaVersions[s.Type]; ok {
			outputs["__meta"] = fmt.Sprintf(`{"schema_version":"%d"}`, v)
		}

		e := edgesByURN[u]
		require.Equal(h.t, stackURN, e.parent, "%s: the engine declares no parent but the stack", u)

		r := apitype.ResourceV3{
			URN: resource.URN(u), Custom: true, Type: tokens.Type(s.Type),
			ID: resource.ID(id), ImportID: resource.ID(importID),
			Inputs: inputs, Outputs: outputs,
			Parent: resource.URN(e.parent), Provider: providerRef,
			Protect: s.Protect, RetainOnDelete: s.RetainOnDelete,
			Dependencies: urnList(e.deps), DeletedWith: resource.URN(e.deletedWith),
		}

		if len(e.propDeps) > 0 {
			r.PropertyDependencies = map[resource.PropertyKey][]resource.URN{}
			for k, d := range e.propDeps {
				r.PropertyDependencies[resource.PropertyKey(k)] = urnList(d)
			}
		}

		seeded += len(e.deps)

		resources = append(resources, r)
	}

	// Actions, workflow permissions and branch protection take the
	// repository's outputs, grants and bypass actors the teams' ids: a
	// seed with no edge would prove a re-key on a state that cannot exist.
	require.Positive(h.t, seeded, "the seed carries the dependency edges")

	require.NoError(h.t, h.importState(resources))
}

func urnList(xs []string) []resource.URN {
	if len(xs) == 0 {
		return nil
	}

	out := make([]resource.URN, len(xs))
	for i, x := range xs {
		out[i] = resource.URN(x)
	}

	return out
}

// importState runs `pulumi stack import`, which checks that every
// reference in the state names a resource listed before it.
func (h *harness) importState(resources []apitype.ResourceV3) error {
	h.t.Helper()

	// The magic cookie is the CLI's tamper check: sha256 of the version.
	const version = "3.0.0"

	raw, err := json.Marshal(apitype.DeploymentV3{
		Manifest:  apitype.ManifestV1{Version: version, Magic: fmt.Sprintf("%x", sha256.Sum256([]byte(version)))},
		Resources: resources,
	})
	require.NoError(h.t, err)

	return h.stack.Import(h.t.Context(), apitype.UntypedDeployment{Version: 3, Deployment: raw})
}

// seedIDs are the id GitHub's provider records for each kind and the
// import id the engine declares for it (empty where it declares none).
func seedIDs(typ, resName, repo string) (id, importID string) {
	switch {
	case strings.HasSuffix(typ, ":Team"):
		slug := strings.TrimPrefix(resName, "team-")

		return renameTeamIDs[slug], slug
	case strings.HasSuffix(typ, ":Repository"),
		strings.HasSuffix(typ, ":ActionsRepositoryPermissions"),
		strings.HasSuffix(typ, ":WorkflowRepositoryPermissions"):
		return repo, repo
	case strings.HasSuffix(typ, ":TeamRepository"):
		slug := strings.TrimSuffix(strings.TrimPrefix(resName, "grant-"), "-"+repo)

		return renameTeamIDs[slug] + ":" + repo, renameTeamIDs[slug] + ":" + repo
	case strings.HasSuffix(typ, ":BranchProtection"):
		return "BPR_fixture", ""
	case strings.HasSuffix(typ, ":RepositoryRuleset"):
		return rulesetIDs[strings.TrimPrefix(resName, "ruleset-"+repo+"-")], ""
	default:
		return resName, ""
	}
}

// seedComputed are the computed outputs GitHub would have returned that
// the engine or the provider's diff reads.
func seedComputed(typ, resName, repo string) map[string]any {
	switch {
	case strings.HasSuffix(typ, ":Repository"):
		return map[string]any{
			"nodeId": repoNodeID, "fullName": renameOrg + "/" + repo, "repoId": 4242,
			"topics": []any{}, "ignoreVulnerabilityAlertsDuringRead": false,
		}
	case strings.HasSuffix(typ, ":Team"):
		return map[string]any{"slug": strings.TrimPrefix(resName, "team-"), "createDefaultMaintainer": false}
	default:
		return nil
	}
}

// resolveUnknowns replaces what a create preview cannot know: the team ids
// read back as a grant's team or a ruleset's bypass actor, and the
// repository node id read back by branch protection. Any other unknown is
// a computed output, dropped as a fresh state would not have it either.
func resolveUnknowns(m map[string]any) map[string]any {
	out := map[string]any{}

	for k, v := range m {
		switch {
		case v == unknown && k == "repositoryId":
			out[k] = repoNodeID
		case v == unknown:
			continue
		default:
			out[k] = resolveValue(k, v)
		}
	}

	return out
}

func resolveValue(key string, v any) any {
	switch x := v.(type) {
	case map[string]any:
		if x["actorType"] == "Team" && x["actorId"] == unknown {
			// One bypass team (management) in the fixture.
			y := resolveUnknowns(x)
			y["actorId"] = 101

			return y
		}

		return resolveUnknowns(x)
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			out = append(out, resolveValue(key, e))
		}

		return out
	default:
		return v
	}
}

// aliasOldName gives every resource whose name carries the new repository
// name an alias to the same name with the old one: what a renamed_from
// field on the row would have the engine declare.
func aliasOldName(args *pulumi.ResourceTransformationArgs) *pulumi.ResourceTransformationResult {
	old, ok := renamedResource(args.Name, newRepo, oldRepo)
	if !ok {
		return nil
	}

	return &pulumi.ResourceTransformationResult{
		Props: args.Props,
		Opts:  append(args.Opts, pulumi.Aliases([]pulumi.Alias{{Name: pulumi.String(old)}})),
	}
}

// renamedResource maps an engine resource name that carries repository
// from to the name carrying to, in the engine's six shapes.
func renamedResource(name, from, to string) (string, bool) {
	for _, p := range []string{"repo-", "actions-", "workflow-perms-", "protection-"} {
		if name == p+from {
			return p + to, true
		}
	}

	if rest, ok := strings.CutPrefix(name, "ruleset-"+from+"-"); ok {
		return "ruleset-" + to + "-" + rest, true
	}

	if strings.HasPrefix(name, "grant-") && strings.HasSuffix(name, "-"+from) {
		return strings.TrimSuffix(name, from) + to, true
	}

	return "", false
}

// renameToken is the whole-token rule of the state re-key: the name as a
// value, or as a path, URL or id segment ("acme/OLD", ".../OLD.git",
// "101:OLD"). A substring inside free text never moves.
func renameToken(s, from, to string) string {
	re := regexp.MustCompile(`(^|[/:])` + regexp.QuoteMeta(from) + `($|\.git$|[/:])`)

	return re.ReplaceAllString(s, "${1}"+to+"${2}")
}

func renameValue(v any, from, to string) any {
	switch x := v.(type) {
	case string:
		return renameToken(x, from, to)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = renameValue(e, from, to)
		}

		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			if k == "description" {
				out[k] = e

				continue
			}

			out[k] = renameValue(e, from, to)
		}

		return out
	default:
		return v
	}
}

// stale names the parts of the re-key a test leaves out, to prove that
// each one is needed. The zero value is the whole re-key.
type stale struct {
	references bool // other resources' dependencies, parent, deletedWith
	importIDs  bool
	outputs    bool
}

// rekey does to the local state what the consumer's state re-key does to
// a real one after `gh repo rename`: every resource whose name carries the
// old repository name gets the new name in its URN, and every reference to
// that URN in another resource (dependencies, property dependencies,
// parent, deletedWith, provider) moves with it. The name moves as a whole
// token in the moved resources' ids, import ids, inputs and outputs.
// Nothing else is touched. It returns what `pulumi stack import` answers.
func (h *harness) rekey(from, to string, leave stale) error {
	h.t.Helper()

	exported, err := h.stack.Export(h.t.Context())
	require.NoError(h.t, err)

	var d apitype.DeploymentV3
	require.NoError(h.t, json.Unmarshal(exported.Deployment, &d))

	moved := map[resource.URN]resource.URN{}

	for i := range d.Resources {
		r := &d.Resources[i]
		u := string(r.URN)

		n, ok := renamedResource(urnName(u), from, to)
		if !ok {
			continue
		}

		r.URN = resource.URN(u[:strings.LastIndex(u, "::")+2] + n)
		moved[resource.URN(u)] = r.URN
		r.ID = resource.ID(renameToken(string(r.ID), from, to))
		r.Inputs, _ = renameValue(r.Inputs, from, to).(map[string]any)

		if !leave.importIDs {
			r.ImportID = resource.ID(renameToken(string(r.ImportID), from, to))
		}

		if !leave.outputs {
			r.Outputs, _ = renameValue(r.Outputs, from, to).(map[string]any)
		}
	}

	require.NotEmpty(h.t, moved, "nothing carried %q", from)

	refs := 0

	if !leave.references {
		follow := func(u resource.URN) resource.URN {
			if n, ok := moved[u]; ok {
				refs++

				return n
			}

			return u
		}

		for i := range d.Resources {
			r := &d.Resources[i]
			r.Parent = follow(r.Parent)
			r.DeletedWith = follow(r.DeletedWith)

			for j := range r.Dependencies {
				r.Dependencies[j] = follow(r.Dependencies[j])
			}

			for k := range r.PropertyDependencies {
				for j := range r.PropertyDependencies[k] {
					r.PropertyDependencies[k][j] = follow(r.PropertyDependencies[k][j])
				}
			}

			// A provider reference is "<urn>::<id>".
			if p, id, ok := cutLast(r.Provider, "::"); ok {
				r.Provider = string(follow(resource.URN(p))) + "::" + id
			}
		}

		require.Positive(h.t, refs, "the state carries references to the moved resources")
	}

	return h.importState(d.Resources)
}

func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}

	return s[:i], s[i+len(sep):], true
}

// rowSlugs are the teams the row grants access to.
func rowSlugs(reg *registry.Config, orgCfg *registry.Org, row string) []string {
	out := make([]string, 0, len(reg.Resolve(orgCfg.Repos[row]).Teams))
	for slug := range reg.Resolve(orgCfg.Repos[row]).Teams {
		out = append(out, slug)
	}

	sort.Strings(out)

	return out
}

// planOf indexes the steps by resource name, ops joined.
func planOf(steps []step) map[string]string {
	ops := map[string][]string{}
	for _, s := range steps {
		ops[s.Name] = append(ops[s.Name], s.Op)
	}

	out := make(map[string]string, len(ops))
	for n, o := range ops {
		out[n] = strings.Join(o, "+")
	}

	return out
}

func logPlan(t *testing.T, title string, steps []step, err error) {
	t.Helper()

	t.Logf("%s (%d steps, preview error: %v)", title, len(steps), err != nil)

	for _, s := range steps {
		t.Logf("  %s", s)
	}
}

// renameCase seeds the state of the row under the old name and returns the
// harness with the registry renamed to the new one.
func renameCase(t *testing.T) (*harness, *registry.Config, *registry.Org, []string) {
	t.Helper()

	h := newHarness(t)

	oldReg, oldOrg := renameRegistry(t, oldRepo)
	h.seedState(oldReg, oldOrg, oldRepo)

	reg, orgCfg := renameRegistry(t, newRepo)

	return h, reg, orgCfg, rowSlugs(reg, orgCfg, newRepo)
}

// The control. Previewing the program that produced the seeded state, with
// GitHub holding exactly that, plans nothing: every resource is `same`,
// including the ones adopted by import. Without this, a "nothing planned"
// below could be a broken seed rather than a property of the engine.
func TestRenamePlanControlSeedIsFaithful(t *testing.T) {
	h := newHarness(t)
	reg, orgCfg := renameRegistry(t, oldRepo)
	h.seedState(reg, orgCfg, oldRepo)

	steps, _, err := h.preview(h.program(reg, orgCfg, liveWith(oldRepo, rowSlugs(reg, orgCfg, oldRepo)), nil))
	logPlan(t, "control: same row, same state", steps, err)
	require.NoError(t, err)

	// Ten objects: two teams, the repository, its two Actions settings,
	// protection, two rulesets and two grants.
	require.Len(t, steps, 10, "every engine resource is planned")

	for _, s := range steps {
		assert.Equal(t, "same", s.Op, "%s", s)
	}

	assert.Empty(t, h.gh.writes(), "a preview writes nothing")

	// The seeded edges are the ones the engine records. An update of the
	// same program is all `same` steps, so it writes nothing to GitHub;
	// the engine still writes the state back, with every resource's
	// dependencies as the program registered them. (In the order its
	// steps finished, which varies, so only the set is compared.)
	seeded := h.stateEdges()

	_, err = h.stack.Up(context.WithoutCancel(t.Context()))
	require.NoError(t, err)
	assert.ElementsMatch(t, seeded, h.stateEdges(), "the engine writes back the seeded edges")
	assert.Empty(t, h.gh.writes(), "an all-same update writes nothing")
}

// stateEdges are the references every resource in the stack's state holds,
// one line per resource.
func (h *harness) stateEdges() []string {
	h.t.Helper()

	exported, err := h.stack.Export(h.t.Context())
	require.NoError(h.t, err)

	var d apitype.DeploymentV3
	require.NoError(h.t, json.Unmarshal(exported.Deployment, &d))

	out := make([]string, 0, len(d.Resources))

	for i := range d.Resources {
		r := &d.Resources[i]
		deps := make([]string, 0, len(r.Dependencies))
		for _, u := range r.Dependencies {
			deps = append(deps, urnName(string(u)))
		}

		sort.Strings(deps)

		props := make([]string, 0, len(r.PropertyDependencies))

		for k, us := range r.PropertyDependencies {
			if len(us) == 0 {
				continue
			}

			names := make([]string, 0, len(us))
			for _, u := range us {
				names = append(names, urnName(string(u)))
			}

			sort.Strings(names)
			props = append(props, string(k)+"="+strings.Join(names, ","))
		}

		sort.Strings(props)

		out = append(out, fmt.Sprintf("%s parent=%s deps=%v props=%v deletedWith=%s",
			urnName(string(r.URN)), r.Parent, deps, props, r.DeletedWith))
	}

	return out
}

// The answer to the question. Renaming the row and nothing else plans a
// create of every resource under the new name and a delete of every one
// under the old: the name is part of every resource's URN and the engine
// declares no alias. The repository's delete is refused only because it is
// protected; the preview fails on it. Branch protection, both rulesets and
// both grants carry no protection, so a plan without the repository row
// would delete them.
func TestRenamePlanRenamedRowIsDeleteAndCreate(t *testing.T) {
	h, reg, orgCfg, grants := renameCase(t)

	// GitHub still has the old name, as when the registry change is
	// previewed before the rename.
	steps, _, err := h.preview(h.program(reg, orgCfg, liveWith(oldRepo, grants), nil))
	logPlan(t, "row renamed, state untouched", steps, err)

	require.Error(t, err, "the protected repository's delete fails the preview")
	assert.Contains(t, err.Error(), "repo-"+oldRepo+`" cannot be deleted`)

	plan := planOf(steps)
	for _, n := range []string{
		"repo-%s", "actions-%s", "protection-%s",
		"ruleset-%s-master-check", "ruleset-%s-release-tags",
		"grant-engineers-%s", "grant-management-%s",
	} {
		assert.Equal(t, "create", plan[fmt.Sprintf(n, newRepo)], n)
		assert.Equal(t, "delete", plan[fmt.Sprintf(n, oldRepo)], n)
	}

	assert.Equal(t, "delete", plan["workflow-perms-"+oldRepo])
	assert.Equal(t, "same", plan["team-management"])
	assert.Empty(t, h.gh.writes(), "a preview writes nothing")
}

// The proof of the safe route. After the repository is renamed on GitHub,
// a state re-key -- every URN carrying the old name moved to the new name,
// every other resource's reference to it (dependencies, property
// dependencies, parent, deletedWith) moved with it, and the name moved as
// a whole token in its id, import id, inputs and outputs -- leaves the
// renamed row planning nothing at all: no create, no delete, no replace,
// no update. That is the contract a consumer's state re-key tool must
// meet.
func TestRenamePlanRekeyedStatePlansNothing(t *testing.T) {
	h, reg, orgCfg, grants := renameCase(t)
	require.NoError(t, h.rekey(oldRepo, newRepo, stale{}))

	steps, _, err := h.preview(h.program(reg, orgCfg, liveWith(newRepo, grants), nil))
	logPlan(t, "row renamed, state re-keyed, GitHub renamed", steps, err)
	require.NoError(t, err)
	require.Len(t, steps, 10)

	for _, s := range steps {
		assert.Equal(t, "same", s.Op, "%s", s)
	}

	assert.Empty(t, h.gh.writes(), "a preview writes nothing")
}

// Each part of the re-key is needed. Leave one out and the result is
// refused or plans a replacement.
func TestRenamePlanRekeyNeedsEveryPart(t *testing.T) {
	t.Run("references", func(t *testing.T) {
		// Other resources still name the old URNs in their dependencies:
		// the CLI refuses the state, and its suggested --force would
		// import dangling edges.
		h, _, _, _ := renameCase(t)

		err := h.rekey(oldRepo, newRepo, stale{references: true})
		require.Error(t, err)
		t.Logf("stack import: %v", err)
		assert.Contains(t, err.Error(), "refers to missing resource")
		assert.Contains(t, err.Error(), "repo-"+oldRepo)
	})

	t.Run("import ids", func(t *testing.T) {
		h, reg, orgCfg, grants := renameCase(t)
		require.NoError(t, h.rekey(oldRepo, newRepo, stale{importIDs: true}))

		steps, _, err := h.preview(h.program(reg, orgCfg, liveWith(newRepo, grants), nil))
		logPlan(t, "re-keyed, import ids left stale", steps, err)

		// The fake GitHub has no repository to read, so the preview stops
		// at the import; the op it planned is the finding.
		require.Error(t, err)
		assert.Contains(t, err.Error(), "resource '"+newRepo+"' does not exist", "the import's read is what stops it")
		assert.Equal(t, "import-replacement", planOf(steps)["repo-"+newRepo],
			"an import id mismatch replaces the repository")
		assert.Empty(t, h.gh.writes(), "a preview writes nothing")
	})

	t.Run("outputs", func(t *testing.T) {
		h, reg, orgCfg, grants := renameCase(t)
		require.NoError(t, h.rekey(oldRepo, newRepo, stale{outputs: true}))

		steps, _, err := h.preview(h.program(reg, orgCfg, liveWith(newRepo, grants), nil))
		logPlan(t, "re-keyed, outputs left stale", steps, err)
		require.NoError(t, err)

		plan := planOf(steps)
		for _, slug := range grants {
			assert.Contains(t, plan["grant-"+slug+"-"+newRepo], "replace", "a stale repository output forces a new grant")
		}

		for _, n := range []string{"repo-%s", "actions-%s", "workflow-perms-%s", "ruleset-%s-master-check", "ruleset-%s-release-tags"} {
			assert.Equal(t, "update", plan[fmt.Sprintf(n, newRepo)], n)
		}

		assert.Empty(t, h.gh.writes(), "a preview writes nothing")
	})
}

// Why the engine has no `renamed_from` alias field. Aliasing every renamed
// resource to its old name is not enough with this provider, in either
// order:
//
//   - before GitHub is renamed (the engine would do the rename): the
//     repository, its Actions settings and both rulesets update in place,
//     but a team grant's `repository` forces a replacement, and the workflow
//     permissions are not declared at all (the engine skips them while the
//     new name does not exist live), so they are deleted from state. The
//     grant's delete addresses the old name; if GitHub redirects it to the
//     renamed repository, the team is left with no access.
//   - after GitHub is renamed: check-then-import declares the new name as
//     the import id, the aliased state still holds the old one, and Pulumi
//     answers an import id mismatch with an import-REPLACEMENT of the
//     repository.
//
// If the provider ever stops forcing a new grant on a repository change,
// the first half of this test fails when run with the pulumi CLI, and an
// alias field is worth another look. CI skips it, so a provider bump is
// the moment to run it by hand.
func TestRenamePlanAliasesAloneAreNotARename(t *testing.T) {
	t.Run("before the GitHub rename", func(t *testing.T) {
		h, reg, orgCfg, grants := renameCase(t)

		steps, _, err := h.preview(h.program(reg, orgCfg, liveWith(oldRepo, grants), aliasOldName))
		logPlan(t, "row renamed, aliases to the old names, GitHub not renamed", steps, err)
		require.NoError(t, err)

		plan := planOf(steps)
		assert.Equal(t, "update", plan["repo-"+newRepo], "the repository is renamed in place")
		assert.Equal(t, "update", plan["actions-"+newRepo])
		assert.Equal(t, "update", plan["ruleset-"+newRepo+"-master-check"])
		assert.Equal(t, "same", plan["protection-"+newRepo], "protection follows the node id")

		for _, slug := range grants {
			assert.Contains(t, plan["grant-"+slug+"-"+newRepo], "replace", "a grant's repository forces a new grant")
		}

		assert.Equal(t, "delete", plan["workflow-perms-"+oldRepo])
		assert.Empty(t, h.gh.writes(), "a preview writes nothing")
	})

	t.Run("after the GitHub rename", func(t *testing.T) {
		h, reg, orgCfg, grants := renameCase(t)

		steps, _, err := h.preview(h.program(reg, orgCfg, liveWith(newRepo, grants), aliasOldName))
		logPlan(t, "row renamed, aliases to the old names, GitHub renamed", steps, err)

		// The fake GitHub has no repository to read, so the preview stops
		// at the import; the op it planned is the finding.
		require.Error(t, err)
		assert.Contains(t, err.Error(), "resource '"+newRepo+"' does not exist", "the import's read is what stops it")
		assert.Equal(t, "import-replacement", planOf(steps)["repo-"+newRepo],
			"an import id mismatch replaces the repository")
		assert.Empty(t, h.gh.writes(), "a preview writes nothing")
	})
}
