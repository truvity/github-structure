package preflight

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	registry "github.com/truvity/github-structure/pkg/registry"
)

// The regression test that matters: the real stack state from 2026-08-06,
// the day before an `up` replaced all twelve teams and took org-wide CI
// down for four days. Every team in it carries `importID: None`, which is
// what made the next run's unconditional Import(slug) a replace.
//
// If this check had existed, it would have refused that apply.
func TestInspectTeamImportIDsCatchesTheRealPreIncidentState(t *testing.T) {
	raw, err := os.ReadFile("testdata/state-2026-08-06-preincident.json")
	require.NoError(t, err)

	problems, err := inspectTeamImportIDs(raw)
	require.NoError(t, err)

	assert.Len(t, problems, 12, "every team in the 08-06 state was a bomb, not just ci-cd")

	joined := strings.Join(problems, "\n")
	assert.Contains(t, joined, `team "ci-cd"`)
	assert.Contains(t, joined, `state's id is "7315349"`)
	assert.Contains(t, joined, "REPLACING")
}

// The state after the incident is the healthy shape: importID == slug.
func TestInspectTeamImportIDsAcceptsMatchingImportIDs(t *testing.T) {
	raw := stateWith(t,
		teamRes("team-ci-cd", "18889468", "ci-cd"),
		teamRes("team-team-dms", "18889461", "team-dms"),
	)

	problems, err := inspectTeamImportIDs(raw)
	require.NoError(t, err)

	assert.Empty(t, problems)
}

// Changing the declared import ID to the numeric one — the "obvious" fix —
// leaves state disagreeing with the program. Measured against the live
// stack, that plans +-12 to replace.
func TestInspectTeamImportIDsCatchesNumericImportID(t *testing.T) {
	raw := stateWith(t, teamRes("team-ci-cd", "18889468", "18889468"))

	problems, err := inspectTeamImportIDs(raw)
	require.NoError(t, err)

	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], `Import("ci-cd")`)
	assert.Contains(t, problems[0], `importID is "18889468"`)
}

// A repository imports by NAME, which is also its id — so a missing
// importID is harmless there. Flagging it would be a false alarm, and the
// two repos adopted on 2026-08-11 are in exactly this shape.
func TestInspectTeamImportIDsAcceptsRepoWithoutImportIDMatchingItsID(t *testing.T) {
	raw := stateWith(t, map[string]any{
		"urn":    "urn:pulumi:truvity::nexus-github::github:index/repository:Repository::repo-json",
		"type":   "github:index/repository:Repository",
		"id":     "json",
		"custom": true,
	})

	problems, err := inspectTeamImportIDs(raw)
	require.NoError(t, err)

	assert.Empty(t, problems, "id == declared import ID, so Pulumi has nothing to reconcile")
}

// A repository whose id has drifted from its declared name IS the bomb.
func TestInspectTeamImportIDsCatchesRepoIDMismatch(t *testing.T) {
	raw := stateWith(t, map[string]any{
		"urn":    "urn:pulumi:truvity::nexus-github::github:index/repository:Repository::repo-json",
		"type":   "github:index/repository:Repository",
		"id":     "some-other-repo",
		"custom": true,
	})

	problems, err := inspectTeamImportIDs(raw)
	require.NoError(t, err)

	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "REPLACING")
}

// Types the engine never imports (branch protection) are not judged.
func TestInspectTeamImportIDsIgnoresUnimportedTypes(t *testing.T) {
	raw := stateWith(t, map[string]any{
		"urn":    "urn:pulumi:truvity::nexus-github::github:index/branchProtection:BranchProtection::protection-bar",
		"type":   "github:index/branchProtection:BranchProtection",
		"id":     "whatever",
		"custom": true,
	})

	problems, err := inspectTeamImportIDs(raw)
	require.NoError(t, err)

	assert.Empty(t, problems, "deployProtection does not import, so it cannot hit this")
}

func TestInspectTeamImportIDsRejectsGarbage(t *testing.T) {
	_, err := inspectTeamImportIDs([]byte("{not json"))
	require.Error(t, err)
}

// rosterManaged decides which teams nothing will refill. Getting it wrong
// in the permissive direction is what let ci-cd sit empty for four days.
func teamRes(name, id, importID string) map[string]any {
	r := map[string]any{
		"urn":    "urn:pulumi:truvity::nexus-github::github:index/team:Team::" + name,
		"type":   "github:index/team:Team",
		"id":     id,
		"custom": true,
	}

	if importID != "" {
		r["importID"] = importID
	}

	return r
}

func stateWith(t *testing.T, resources ...map[string]any) []byte {
	t.Helper()

	raw, err := json.Marshal(map[string]any{
		"manifest":  map[string]any{},
		"resources": resources,
	})
	require.NoError(t, err)

	return raw
}

// unfillableGrantedTeams with an injected fillable policy: only granted
// teams that NOTHING refills are selected — a selector that quietly
// returns everything (or nothing) would make checkRobotTeams noise (or
// make it pass forever while checking nothing).
func TestUnfillableGrantedTeamsHonoursTheFillablePolicy(t *testing.T) {
	reg := &registry.Config{
		// Resolution goes through the profile — a repo whose profile is
		// unknown resolves to nothing, so the fixture declares one.
		Profiles: map[string]*registry.RepoSettings{"standard": {}},
		Orgs: map[string]*registry.Org{
			"example": {
				Teams: map[string]*registry.Team{
					"team-people": {},
					"robots":      {},
					"idle":        {},
				},
				Repos: map[string]*registry.Repo{
					"svc": {
						Profile: "standard",
						Teams:   map[string]string{"team-people": "push", "robots": "push"},
					},
				},
			},
		},
	}

	got := unfillableGrantedTeams(reg, "example", func(slug string) bool {
		return slug == "team-people" // an IdP-backed roster fills team-*
	})

	// robots holds a grant and nothing fills it; idle holds no grant.
	assert.Equal(t, []string{"robots"}, got)

	assert.Nil(t, unfillableGrantedTeams(reg, "no-such-org", func(string) bool { return false }))
}
