package preflight

import (
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	registry "github.com/truvity/github-structure/pkg/registry"
)

const (
	repoT    = "github:index/repository:Repository"
	actionsT = "github:index/actionsRepositoryPermissions:ActionsRepositoryPermissions"
	wfT      = "github:index/workflowRepositoryPermissions:WorkflowRepositoryPermissions"
	protT    = "github:index/branchProtection:BranchProtection"
	rulesetT = "github:index/repositoryRuleset:RepositoryRuleset"
	grantT   = "github:index/teamRepository:TeamRepository"
)

func step(typ, name string) plannedStep {
	return plannedStep{urn: "urn:pulumi:acme::example-github::" + typ + "::" + name, typ: typ, name: name}
}

// orgWith is an org whose registry declares these repositories and the two
// teams every fixture grants.
func orgWith(repos ...string) *registry.Org {
	o := &registry.Org{
		Repos: map[string]*registry.Repo{},
		Teams: map[string]*registry.Team{"engineers": {}, "management": {}, "team-a": {}},
	}
	for _, r := range repos {
		o.Repos[r] = &registry.Repo{}
	}

	return o
}

// A renamed row, whole: every resource of widget deleted, every one of
// gadget created. This is what the preview says when the row is renamed and
// the state is not re-keyed.
func wholeRename() plan {
	return plan{
		creates: []plannedStep{
			step(repoT, "repo-gadget"), step(actionsT, "actions-gadget"), step(wfT, "workflow-perms-gadget"),
			step(protT, "protection-gadget"), step(rulesetT, "ruleset-gadget-master-check"),
			step(grantT, "grant-engineers-gadget"), step(grantT, "grant-management-gadget"),
		},
		deletes: []plannedStep{
			step(repoT, "repo-widget"), step(actionsT, "actions-widget"), step(wfT, "workflow-perms-widget"),
			step(protT, "protection-widget"), step(rulesetT, "ruleset-widget-master-check"),
			step(grantT, "grant-engineers-widget"), step(grantT, "grant-management-widget"),
		},
	}
}

func TestRenameShapedRefusesAWholeRename(t *testing.T) {
	got := renameShaped(orgWith("gadget"), wholeRename(), nil)

	require.Len(t, got, 1)
	assert.Equal(t, "widget", got[0].Old)
	assert.Equal(t, "gadget", got[0].New)
	assert.Len(t, got[0].Resources, 7, "every resource of the row is named")
}

// The hole this closes. The repository and its protection are guarded by
// their own checks (a protected repository's delete fails, a protection
// delete is refused), but a plan that deletes only team grants passed: a
// grant is neither irreplaceable nor guarded. Renaming a repository
// through an alias, or after the repository's resources were re-keyed by
// hand, leaves exactly that plan, and its delete can strip a team's access
// when GitHub redirects the old name.
func TestRenameShapedRefusesAGrantOnlyRename(t *testing.T) {
	p := plan{
		creates: []plannedStep{step(grantT, "grant-engineers-gadget"), step(grantT, "grant-management-gadget")},
		deletes: []plannedStep{step(grantT, "grant-engineers-widget"), step(grantT, "grant-management-widget")},
	}

	got := renameShaped(orgWith("gadget"), p, nil)

	require.Len(t, got, 1)
	assert.Equal(t, "widget", got[0].Old)
	assert.Equal(t, "gadget", got[0].New)
}

func TestRenameShapedKnowsRulesetAndHyphenatedNames(t *testing.T) {
	p := plan{
		creates: []plannedStep{step(rulesetT, "ruleset-big-gadget-pr-approval"), step(grantT, "grant-team-a-big-gadget")},
		deletes: []plannedStep{step(rulesetT, "ruleset-big-widget-pr-approval"), step(grantT, "grant-team-a-big-widget")},
	}

	got := renameShaped(orgWith("big-gadget"), p, nil)

	require.Len(t, got, 1)
	assert.Equal(t, "big-widget", got[0].Old)
	assert.Equal(t, "big-gadget", got[0].New)
	assert.Len(t, got[0].Resources, 2)
}

func TestRenameShapedHonoursAnOverrideForThatNameOnly(t *testing.T) {
	p := wholeRename()
	p.creates = append(p.creates, step(repoT, "repo-other"))
	p.deletes = append(p.deletes, step(repoT, "repo-former"))

	org := orgWith("gadget", "other")

	// A plain delete of one repository and create of another cannot be
	// told from a rename by their names, so each create pairs with each
	// delete of its kind: the override is what separates them.
	got := renameShaped(org, p, []string{"gadget"})
	require.Len(t, got, 2, "the override covers gadget and nothing else")

	for _, f := range got {
		assert.Equal(t, "other", f.New)
	}

	assert.Empty(t, renameShaped(org, p, []string{"gadget", "other"}))
	assert.Empty(t, renameShaped(org, p, []string{"widget", "former"}), "the old name names the pair too")
}

// The guard stays narrow enough to be believed.
func TestRenameShapedIgnoresWhatIsNotARename(t *testing.T) {
	cases := map[string]plan{
		"a plain create": {creates: []plannedStep{step(repoT, "repo-gadget"), step(grantT, "grant-engineers-gadget")}},
		"a plain delete": {deletes: []plannedStep{step(grantT, "grant-engineers-widget")}},
		"a different team": {
			creates: []plannedStep{step(grantT, "grant-engineers-gadget")},
			deletes: []plannedStep{step(grantT, "grant-management-widget")},
		},
		"a different kind": {
			creates: []plannedStep{step(protT, "protection-gadget")},
			deletes: []plannedStep{step(actionsT, "actions-widget")},
		},
		"a different ruleset": {
			creates: []plannedStep{step(rulesetT, "ruleset-gadget-master-check")},
			deletes: []plannedStep{step(rulesetT, "ruleset-widget-release-tags")},
		},
		"the old repository is still a row": {
			creates: []plannedStep{step(grantT, "grant-engineers-gadget")},
			deletes: []plannedStep{step(grantT, "grant-engineers-widget")},
		},
	}

	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Empty(t, renameShaped(orgWith("gadget", "widget"), p, nil))
		})
	}
}

func TestRenameShapedNeedsTheCreateToBeARegistryRow(t *testing.T) {
	// gadget is not declared: the create is not a row's resource, so the
	// pair is not a rename of one.
	assert.Empty(t, renameShaped(orgWith(), wholeRename(), nil))
}

// The plan the guards read comes from the preview events: a create and a
// delete of a repository-scoped resource are collected, and the existing
// replacement guard keeps seeing what it saw.
func TestCollectPlanCollectsCreatesAndDeletes(t *testing.T) {
	ch, out := startPlan()

	ch <- preEvent("urn:pulumi:acme::example-github::"+grantT+"::grant-engineers-gadget", grantT, "create")
	ch <- preEvent("urn:pulumi:acme::example-github::"+grantT+"::grant-engineers-widget", grantT, "delete")
	ch <- preEvent(teamURN, "github:index/team:Team", "replace")
	ch <- preEvent("urn:pulumi:acme::example-github::"+grantT+"::grant-engineers-big", grantT, "same")

	close(ch)

	p := <-out

	require.Len(t, p.creates, 1)
	assert.Equal(t, "grant-engineers-gadget", p.creates[0].name)
	require.Len(t, p.deletes, 1)
	assert.Equal(t, "grant-engineers-widget", p.deletes[0].name)
	require.Len(t, p.replaces, 1)
}

func startPlan() (chan events.EngineEvent, chan plan) {
	ch := make(chan events.EngineEvent)
	out := make(chan plan, 1)

	go collectPlan(ch, out)

	return ch, out
}

// The refusal says what it saw and what to do: re-key the state, or, when
// the delete and the create are both meant, name the repository.
func TestWriteRenamesPointsToStateRenameRepo(t *testing.T) {
	var b strings.Builder

	writeRenames(&b, "acme", []renameFinding{{
		Old: "widget", New: "gadget",
		Resources: []string{"grant-engineers-widget -> grant-engineers-gadget"},
	}})

	out := b.String()
	assert.Contains(t, out, "REFUSING")
	assert.Contains(t, out, "rename")
	assert.Contains(t, out, "state rename-repo widget gadget")
	assert.Contains(t, out, "--allow-recreate gadget")
	assert.Contains(t, out, "grant-engineers-widget -> grant-engineers-gadget")
}

// The refusal reaches the caller as an error and names the count, so a
// deploy wired to Run stops.
func TestReportCountsRenameShapedPlans(t *testing.T) {
	err := report("acme", nil, nil, []renameFinding{{Old: "widget", New: "gadget", Resources: []string{"a -> b"}}})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "1 rename-shaped repository plan(s)")
}
