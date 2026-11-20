package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	registry "github.com/truvity/github-structure/pkg/registry"
)

// fakeGitHub puts a `gh` on PATH that behaves like the immutable-releases
// endpoint of a GitHub: GET answers the repository's state file (404 when
// there is none), PUT and DELETE rewrite it, and every call is recorded.
// It is the repository's stand-in for a fake server, because this package
// reaches GitHub through the ambient `gh` CLI and not over HTTP.
type fakeGitHub struct {
	dir string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()

	f := &fakeGitHub{dir: t.TempDir()}

	script := `#!/bin/sh
echo "$*" >> "$FAKE_GH_DIR/calls"
method=GET
path=$2
if [ "$2" = "-X" ]; then method=$3; path=$4; fi
repo=${path#/repos/*/}
repo=${repo%%/*}
state="$FAKE_GH_DIR/state-$repo"
if [ -f "$FAKE_GH_DIR/refuse-$repo" ]; then
  echo "gh: Resource not accessible by integration (HTTP 403)" >&2
  exit 1
fi
case "$method" in
GET)
  if [ ! -f "$state" ]; then
    echo "gh: Not Found (HTTP 404)" >&2
    exit 1
  fi
  read -r body < "$state"
  printf '%s' "$body"
  ;;
PUT)
  echo '{"enabled":true,"enforced_by_owner":false}' > "$state"
  ;;
DELETE)
  echo '{"enabled":false,"enforced_by_owner":false}' > "$state"
  ;;
esac
`

	require.NoError(t, os.WriteFile(filepath.Join(f.dir, "gh"), []byte(script), 0o755)) //nolint:gosec // a test stub must be executable

	t.Setenv("PATH", f.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_GH_DIR", f.dir)

	return f
}

// live sets a repository's immutable-releases state.
func (f *fakeGitHub) live(t *testing.T, repo string, enabled, enforced bool) {
	t.Helper()

	body := `{"enabled":` + boolJSON(enabled) + `,"enforced_by_owner":` + boolJSON(enforced) + "}\n"
	require.NoError(t, os.WriteFile(filepath.Join(f.dir, "state-"+repo), []byte(body), 0o600))
}

// refuse makes every call about a repository fail with a 403.
func (f *fakeGitHub) refuse(t *testing.T, repo string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(f.dir, "refuse-"+repo), nil, 0o600))
}

// calls are the recorded gh invocations, in order.
func (f *fakeGitHub) calls(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if os.IsNotExist(err) {
		return nil
	}

	require.NoError(t, err)

	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// writes are the recorded calls that changed something.
func (f *fakeGitHub) writes(t *testing.T) []string {
	t.Helper()

	var out []string

	for _, c := range f.calls(t) {
		if strings.HasPrefix(c, "api -X ") {
			out = append(out, c)
		}
	}

	return out
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}

	return "false"
}

func immutableConfig() *registry.Config {
	return &registry.Config{
		Orgs: map[string]*registry.Org{
			"acme": {
				Repos: map[string]*registry.Repo{
					"turn-on":  {ImmutableReleases: ptr(true)},
					"turn-off": {ImmutableReleases: ptr(false)},
					"settled":  {ImmutableReleases: ptr(true)},
					"unset":    {},
					"attic":    {ImmutableReleases: ptr(true), Archived: true},
				},
			},
		},
	}
}

func liveAll(t *testing.T, f *fakeGitHub) {
	t.Helper()

	f.live(t, "turn-on", false, false)
	f.live(t, "turn-off", true, false)
	f.live(t, "settled", true, false)
	// "unset" and "attic" have no state file: a read of either would 404,
	// which would show as an unreadable repository. They must not be read.
}

// The plan lists the repositories that differ, leaves the matching one
// alone and never reads a repository whose row is silent or archived.
func TestPlanImmutableReleases(t *testing.T) {
	f := newFakeGitHub(t)
	liveAll(t, f)

	plan, err := PlanImmutableReleases(t.Context(), "acme", immutableConfig())
	require.NoError(t, err)

	assert.Equal(t, []ImmutableChange{
		{Repo: "turn-off", Want: false, Got: true},
		{Repo: "turn-on", Want: true, Got: false},
	}, plan.Changes)
	assert.Equal(t, []string{"settled"}, plan.InSync)
	assert.Empty(t, plan.Unreadable, "an unset or archived row is not read, so it cannot 404")
	assert.Equal(t, []string{
		"repo turn-off: immutable_releases: on -> off",
		"repo turn-on: immutable_releases: off -> on",
	}, plan.Lines())
	assert.Equal(t, "2 to change, 1 in sync, 0 not readable", plan.Summary())

	assert.Empty(t, f.writes(t), "a plan never writes")

	for _, c := range f.calls(t) {
		assert.NotContains(t, c, "/unset/")
		assert.NotContains(t, c, "/attic/")
	}
}

// Apply writes PUT for on and DELETE for off, only where they differ, and
// a second run finds nothing to do.
func TestApplyImmutableReleasesWritesOnlyTheDifference(t *testing.T) {
	f := newFakeGitHub(t)
	liveAll(t, f)

	plan, err := PlanImmutableReleases(t.Context(), "acme", immutableConfig())
	require.NoError(t, err)

	applied, err := ApplyImmutableReleases(t.Context(), "acme", plan)
	require.NoError(t, err)

	assert.Equal(t, []string{
		"repo turn-off: immutable_releases: on -> off",
		"repo turn-on: immutable_releases: off -> on",
	}, applied)
	assert.Equal(t, []string{
		"api -X DELETE /repos/acme/turn-off/immutable-releases",
		"api -X PUT /repos/acme/turn-on/immutable-releases",
	}, f.writes(t))

	again, err := PlanImmutableReleases(t.Context(), "acme", immutableConfig())
	require.NoError(t, err)
	assert.True(t, again.Empty(), "the second plan is empty: %v", again.Lines())
	assert.Equal(t, []string{"settled", "turn-off", "turn-on"}, sortedStrings(again.InSync))
}

// Unset is "not managed": a registry with no row stating the field reads
// and writes nothing, whatever the live state is.
func TestImmutableReleasesUnsetIsNeverTouched(t *testing.T) {
	f := newFakeGitHub(t)
	f.live(t, "widget", true, false)

	cfg := &registry.Config{Orgs: map[string]*registry.Org{
		"acme": {Repos: map[string]*registry.Repo{"widget": {}}},
	}}

	plan, err := PlanImmutableReleases(t.Context(), "acme", cfg)
	require.NoError(t, err)
	assert.True(t, plan.Empty())

	drifts, err := CheckImmutableReleases(t.Context(), "acme", cfg)
	require.NoError(t, err)
	assert.Empty(t, drifts)

	assert.Empty(t, f.calls(t), "no GitHub call at all for a row that does not state the field")
}

// Drift reports the same differences as the plan, in the drift's own
// vocabulary: subject, field, registry value and live value.
func TestCheckImmutableReleasesReportsDrift(t *testing.T) {
	f := newFakeGitHub(t)
	liveAll(t, f)

	drifts, err := CheckImmutableReleases(t.Context(), "acme", immutableConfig())
	require.NoError(t, err)

	require.Len(t, drifts, 2)
	assert.Equal(t, Drift{Subject: "repo turn-off", Field: "immutable_releases", Want: "false", Got: "true"}, drifts[0])
	assert.Equal(t, Drift{Subject: "repo turn-on", Field: "immutable_releases", Want: "true", Got: "false"}, drifts[1])
	assert.Equal(t, `repo turn-on: immutable_releases: registry="true" live="false"`, drifts[1].String())
	assert.Empty(t, f.writes(t), "drift never writes")
}

// A repository GitHub answers 404 for (missing, or the token has no admin
// read) is not a failure and not a drift: it is printed as not visible.
func TestCheckImmutableReleasesUnreadableIsNotVisible(t *testing.T) {
	f := newFakeGitHub(t)
	f.live(t, "settled", true, false)

	cfg := &registry.Config{Orgs: map[string]*registry.Org{
		"acme": {Repos: map[string]*registry.Repo{
			"settled": {ImmutableReleases: ptr(true)},
			"missing": {ImmutableReleases: ptr(true)},
		}},
	}}

	drifts, err := CheckImmutableReleases(t.Context(), "acme", cfg)
	require.NoError(t, err)

	require.Len(t, drifts, 1)
	assert.True(t, drifts[0].NotVisible)
	assert.Equal(t, "repo missing: immutable_releases: not visible to this token, not compared", drifts[0].String())

	plan, err := PlanImmutableReleases(t.Context(), "acme", cfg)
	require.NoError(t, err)
	assert.Equal(t, []string{"missing"}, plan.Unreadable)
	assert.True(t, plan.Empty())
}

// An organization that enforces the setting on makes a declared off
// impossible to apply: the plan says so, drift names it, and apply refuses
// before it writes anything, including the changes that would have worked.
func TestImmutableReleasesDeclaredOffButEnforcedIsRefusedBeforeAnyWrite(t *testing.T) {
	f := newFakeGitHub(t)
	f.live(t, "turn-on", false, false)
	f.live(t, "forced", true, true)

	cfg := &registry.Config{Orgs: map[string]*registry.Org{
		"acme": {Repos: map[string]*registry.Repo{
			"turn-on": {ImmutableReleases: ptr(true)},
			"forced":  {ImmutableReleases: ptr(false)},
		}},
	}}

	plan, err := PlanImmutableReleases(t.Context(), "acme", cfg)
	require.NoError(t, err)

	require.Len(t, plan.Blocked(), 1)
	assert.Contains(t, plan.Lines()[0], "repo forced: immutable_releases: on -> off (blocked: enforced by the organization)")

	drifts, err := CheckImmutableReleases(t.Context(), "acme", cfg)
	require.NoError(t, err)
	require.NotEmpty(t, drifts)
	assert.Equal(t, "true (enforced by the organization)", drifts[0].Got)

	_, err = ApplyImmutableReleases(t.Context(), "acme", plan)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced")
	assert.Empty(t, f.writes(t), "nothing is written when any change is blocked")
}

// A declared on that is already enforced on is simply in sync.
func TestImmutableReleasesEnforcedOnMatchesDeclaredOn(t *testing.T) {
	f := newFakeGitHub(t)
	f.live(t, "forced", true, true)

	cfg := &registry.Config{Orgs: map[string]*registry.Org{
		"acme": {Repos: map[string]*registry.Repo{"forced": {ImmutableReleases: ptr(true)}}},
	}}

	plan, err := PlanImmutableReleases(t.Context(), "acme", cfg)
	require.NoError(t, err)
	assert.True(t, plan.Empty())
}

// A refused read (403) is an error naming the repository, not a silent
// skip; a refused write stops apply and names the repository too.
func TestImmutableReleasesErrorsNameTheRepository(t *testing.T) {
	f := newFakeGitHub(t)
	f.live(t, "turn-on", false, false)
	f.refuse(t, "turn-on")

	cfg := &registry.Config{Orgs: map[string]*registry.Org{
		"acme": {Repos: map[string]*registry.Repo{"turn-on": {ImmutableReleases: ptr(true)}}},
	}}

	_, err := PlanImmutableReleases(t.Context(), "acme", cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repo turn-on")
	assert.Contains(t, err.Error(), "HTTP 403")

	_, err = ApplyImmutableReleases(t.Context(), "acme", ImmutablePlan{
		Org:     "acme",
		Changes: []ImmutableChange{{Repo: "turn-on", Want: true}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "turn-on")
}

// An org the registry does not have plans nothing.
func TestPlanImmutableReleasesUnknownOrg(t *testing.T) {
	plan, err := PlanImmutableReleases(t.Context(), "nobody", immutableConfig())
	require.NoError(t, err)
	assert.True(t, plan.Empty())
}

// ── tag ruleset rules, in the drift comparison ─────────────────────────

func detailWithRules(types ...string) liveRulesetDetail {
	var d liveRulesetDetail

	d.Rules = make([]struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredApprovingReviewCount *int  `json:"required_approving_review_count"`
			DismissStaleReviewsOnPush    *bool `json:"dismiss_stale_reviews_on_push"`
			RequireCodeOwnerReview       *bool `json:"require_code_owner_review"`
		} `json:"parameters"`
	}, len(types))

	for i, ty := range types {
		d.Rules[i].Type = ty
	}

	return d
}

func TestCompareLiveTagRules(t *testing.T) {
	const subject = "repo widget ruleset v2-tags-create"

	t.Run("a subset that matches is clean", func(t *testing.T) {
		assert.Empty(t, compareLiveTagRules(subject, []string{"creation"}, detailWithRules("creation")))
	})

	t.Run("the default is all three", func(t *testing.T) {
		all := (&registry.TagRuleset{}).EffectiveRules()
		assert.Empty(t, compareLiveTagRules(subject, all, detailWithRules("deletion", "update", "creation")))
	})

	t.Run("a rule narrowed by hand is reported", func(t *testing.T) {
		d := compareLiveTagRules(subject, []string{"creation", "deletion"}, detailWithRules("creation"))
		require.Len(t, d, 1)
		assert.Equal(t, "rules", d[0].Field)
		assert.Equal(t, "deletion", d[0].Want)
		assert.Equal(t, gotAbsent, d[0].Got)
	})

	t.Run("a rule added by hand is reported", func(t *testing.T) {
		d := compareLiveTagRules(subject, []string{"creation"}, detailWithRules("creation", "update"))
		require.Len(t, d, 1)
		assert.Equal(t, "extra: update", d[0].Got)
	})

	t.Run("a rule the registry has no word for is not compared", func(t *testing.T) {
		assert.Empty(t, compareLiveTagRules(subject, []string{"creation"}, detailWithRules("creation", "non_fast_forward")))
	})

	t.Run("rules the token cannot read are not visible, not drift", func(t *testing.T) {
		d := compareLiveTagRules(subject, []string{"creation"}, liveRulesetDetail{})
		require.Len(t, d, 1)
		assert.True(t, d[0].NotVisible)
	})
}

// A ruleset with no bypass at all expects an empty actor set: any live
// actor is an extra.
func TestDeclaredTagActorsForNoBypass(t *testing.T) {
	rs := &registry.TagRuleset{Name: "immutable", NoBypass: true}

	assert.Empty(t, declaredTagActors(t.Context(), "acme", rs, nil, map[string]int64{}))

	d := compareBypassSets("repo widget ruleset immutable", "bypass actors", nil, []string{"OrganizationAdmin:0"})
	require.Len(t, d, 1)
	assert.Equal(t, "extra: OrganizationAdmin:0", d[0].Got)
}
