package registry_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	registry "github.com/truvity/github-structure/pkg/registry"
)

// withWOT puts a workflow_only_tags list (and optionally a tag ruleset)
// on the minimal registry's repository row.
func withWOT(t *testing.T, extra string) (*registry.Config, error) {
	t.Helper()

	return load(t, strings.Replace(minimal, "        preset: public\n", extra, 1))
}

func wotList(items ...string) string {
	return wotApp("release-app", items...)
}

func wotApp(app string, items ...string) string {
	var b strings.Builder

	b.WriteString("        preset: public\n        workflow_only_tags:\n          app: " + app + "\n          patterns:\n")

	for _, i := range items {
		b.WriteString("            - " + i + "\n")
	}

	return b.String()
}

func TestWorkflowOnlyTagsLoads(t *testing.T) {
	cfg, err := withWOT(t, wotList(`"deploy/pulumi/v*"`, `"sdk/v*"`))
	require.NoError(t, err)

	repo := cfg.Orgs["acme"].Repos["widget"]
	assert.Equal(t, "release-app", repo.WorkflowOnlyTags.App)
	assert.Equal(t, []string{"deploy/pulumi/v*", "sdk/v*"}, repo.WorkflowOnlyTags.Patterns)
}

// The release-tags shape (a team bypass on root v* tags) sits beside the
// field without touching it.
func TestWorkflowOnlyTagsBesideRootReleaseTags(t *testing.T) {
	_, err := withWOT(t, wotList(`"deploy/pulumi/v*"`)+`        tag_rulesets:
          - name: release-tags
            pattern: refs/tags/v*
            bypass_teams: [engineers]
`)
	require.NoError(t, err)
}

func TestWorkflowOnlyTagsRefusals(t *testing.T) {
	releaseTags := `        tag_rulesets:
          - name: release-tags
            pattern: refs/tags/v*
            bypass_teams: [engineers]
`
	projectTags := `        tag_rulesets:
          - name: project-tags
            pattern: refs/tags/deploy/*/v*
            bypass_teams: [engineers]
`

	cases := map[string]struct {
		body string
		want string
	}{
		"v*":                  {wotList(`"v*"`), "could match a root release tag"},
		"*":                   {wotList(`"*"`), "could match a root release tag"},
		"**":                  {wotList(`"**"`), "could match a root release tag"},
		"v1.*":                {wotList(`"v1.*"`), "could match a root release tag"},
		"?1.0.0":              {wotList(`"?1.0.0"`), "could match a root release tag"},
		"empty patterns":      {"        preset: public\n        workflow_only_tags: {app: release-app, patterns: []}\n", "patterns is empty"},
		"no app":              {"        preset: public\n        workflow_only_tags: {patterns: [\"deploy/v*\"]}\n", "app is required"},
		"github-actions":      {wotApp("github-actions", `"deploy/v*"`), "422"},
		"app id":              {wotApp(`"15368"`, `"deploy/v*"`), "database id"},
		"v0.14.0 list shape":  {"        preset: public\n        workflow_only_tags: [\"deploy/v*\"]\n", "no longer a list"},
		"empty pattern":       {wotList(`""`), "empty pattern"},
		"duplicate":           {wotList(`"deploy/v*"`, `"deploy/v*"`), "listed twice"},
		"full ref":            {wotList(`"refs/tags/deploy/v*"`), "relative to refs/tags/"},
		"character class":     {wotList(`"deploy/[ab]*"`), "character classes"},
		"overlaps root rules": {wotList(`"v*"`) + releaseTags, "could match a root release tag"},
		"overlaps other":      {wotList(`"deploy/pulumi/v*"`) + projectTags, `overlaps tag ruleset "project-tags"`},
		"name is reserved": {wotList(`"deploy/v*"`) + `        tag_rulesets:
          - name: workflow-only-tags
            pattern: refs/tags/other/*
            bypass_teams: [engineers]
`, "may not use the name"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := withWOT(t, tc.body)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestWorkflowOnlyTagsRefusedOnArchivedRepo(t *testing.T) {
	_, err := withWOT(t, wotList(`"deploy/v*"`)+"        archived: true\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workflow_only_tags")
}

// The ruleset has no team bypass and still loads: the exemption from the
// at-least-one-team rule is the point of the field.
func TestWorkflowOnlyTagsNeedsNoBypassTeam(t *testing.T) {
	_, err := withWOT(t, wotList(`"deploy/v*"`))
	require.NoError(t, err)
}

func TestWorkflowOnlyTagsNone(t *testing.T) {
	assert.Nil(t, registry.WorkflowOnlyTagsRulesetFor(nil, 7))
}

func TestWorkflowOnlyTagsRenderGolden(t *testing.T) {
	got, err := json.MarshalIndent(
		registry.WorkflowOnlyTagsRulesetFor([]string{"deploy/pulumi/v*", "sdk/v*"}, 424242), "", "  ")
	require.NoError(t, err)

	want, err := os.ReadFile("testdata/workflow-only-tags.golden.json")
	require.NoError(t, err)

	assert.JSONEq(t, string(want), string(got))

	// Nothing but the workflow: no team, no organization admin.
	rs := registry.WorkflowOnlyTagsRulesetFor([]string{"x/v*"}, 424242)
	require.Len(t, rs.BypassActors, 1)
	assert.Equal(t, "Integration", rs.BypassActors[0].ActorType)
	assert.Equal(t, 424242, rs.BypassActors[0].ActorID)
}
