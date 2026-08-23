package preflight

import (
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	teamURN  = "urn:pulumi:truvity::nexus-github::github:index/team:Team::team-ci-cd"
	teamURN2 = "urn:pulumi:truvity::nexus-github::github:index/team:Team::team-dms"
	repoURN  = "urn:pulumi:truvity::nexus-github::github:index/repository:Repository::repo-bar"
)

// preEvent builds the one event shape the collector reads.
func preEvent(urn, typ, op string) events.EngineEvent {
	return events.EngineEvent{
		EngineEvent: apitype.EngineEvent{
			ResourcePreEvent: &apitype.ResourcePreEvent{
				Metadata: apitype.StepEventMetadata{
					URN:  urn,
					Type: typ,
					Op:   apitype.OpType(op),
				},
			},
		},
	}
}

// drain feeds events through the collector the way Preview does.
func drain(t *testing.T, evs ...events.EngineEvent) []plannedReplace {
	t.Helper()

	ch := make(chan events.EngineEvent)
	out := make(chan []plannedReplace, 1)

	go collectReplaces(ch, out)

	for i := range evs {
		ch <- evs[i]
	}

	close(ch)

	return <-out
}

// The plan that matters: a team being replaced must be caught. This is the
// 2026-08-07 shape — Pulumi emits three ops for one replacement.
func TestCollectReplacesCatchesTeamReplace(t *testing.T) {
	got := drain(t,
		preEvent(teamURN, "github:index/team:Team", "replace"),
		preEvent(teamURN, "github:index/team:Team", "create-replacement"),
		preEvent(teamURN, "github:index/team:Team", "delete-replaced"),
	)

	require.Len(t, got, 1, "three ops on one URN are one doomed resource, not three")
	assert.Equal(t, teamURN, got[0].urn)
	assert.Equal(t, "create-replacement, delete-replaced, replace", got[0].opList())
	assert.Contains(t, got[0].reason, "membership")
}

// A clean plan must not trip the guard — otherwise it would be ignored
// within a week, which is the real failure mode for a preflight.
func TestCollectReplacesIgnoresNonReplaceOps(t *testing.T) {
	got := drain(t,
		preEvent(teamURN, "github:index/team:Team", "same"),
		preEvent(teamURN, "github:index/team:Team", "update"),
		preEvent(teamURN2, "github:index/team:Team", "import"),
		preEvent(teamURN2, "github:index/team:Team", "create"),
	)

	assert.Empty(t, got, "update/import/create on a team are the normal path")
}

// A repository replace deletes the repository and all its history. There is
// no routine case for it here, and protect:true is not a backstop — 62 repos
// carry it, and the 08-07 team replace proved it does not hold.
func TestCollectReplacesCatchesRepositoryReplace(t *testing.T) {
	got := drain(t, preEvent(repoURN, "github:index/repository:Repository", "replace"))

	require.Len(t, got, 1)
	assert.Contains(t, got[0].reason, "entire history")
}

// The guard stays narrow enough to be believed: resources the stack fully
// owns are none of its business.
func TestCollectReplacesIgnoresUnguardedTypes(t *testing.T) {
	const urn = "urn:pulumi:truvity::nexus-github::github:index/actionsRepositoryPermissions:ActionsRepositoryPermissions::actions-bar"

	got := drain(t, preEvent(urn, "github:index/actionsRepositoryPermissions:ActionsRepositoryPermissions", "replace"))

	assert.Empty(t, got, "a settings resource can be replaced without losing anything")
}

func TestCollectReplacesSortsAndSeparatesResources(t *testing.T) {
	got := drain(t,
		preEvent(teamURN2, "github:index/team:Team", "replace"),
		preEvent(teamURN, "github:index/team:Team", "replace"),
	)

	require.Len(t, got, 2)
	assert.Equal(t, teamURN, got[0].urn, "sorted by URN so the report is stable across runs")
	assert.Equal(t, teamURN2, got[1].urn)
}

// Branch protection is guarded for the mirror-image reason: the delete half
// leaves a branch unprotected and the next diff reads clean.
func TestCollectReplacesCatchesBranchProtection(t *testing.T) {
	const urn = "urn:pulumi:truvity::nexus-github::github:index/branchProtection:BranchProtection::prot-bar"

	got := drain(t, preEvent(urn, "github:index/branchProtection:BranchProtection", "delete-replaced"))

	require.Len(t, got, 1)
	assert.Contains(t, got[0].reason, "unprotected")
}

// Events without a ResourcePreEvent (summaries, diagnostics, stdout) make up
// most of the stream and must not panic the collector.
func TestCollectReplacesSkipsNonResourceEvents(t *testing.T) {
	got := drain(t,
		events.EngineEvent{},
		preEvent(teamURN, "github:index/team:Team", "replace"),
		events.EngineEvent{},
	)

	require.Len(t, got, 1)
}
