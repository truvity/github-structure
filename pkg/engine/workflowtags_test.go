package engine

import (
	"sync"
	"testing"

	"github.com/pulumi/pulumi-github/sdk/v6/go/github"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-structure/pkg/registry"
)

type captureMocks struct {
	mu   sync.Mutex
	seen map[string]resource.PropertyMap
}

func (m *captureMocks) NewResource(a pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.seen[a.Name] = a.Inputs

	return a.Name + "_id", a.Inputs, nil
}

func (m *captureMocks) Call(a pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return a.Args, nil
}

// The plan for workflow_only_tags: one tag ruleset named
// workflow-only-tags, with the row's App (resolved from installations) as
// its only bypass actor.
func TestDeployWorkflowOnlyTagsPlan(t *testing.T) {
	mocks := &captureMocks{seen: map[string]resource.PropertyMap{}}

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		provider, err := github.NewProvider(ctx, "gh", &github.ProviderArgs{})
		if err != nil {
			return err
		}

		return deployWorkflowOnlyTags(ctx, "widget", registry.InstalledApps{"release-app": 424242},
			&registry.WorkflowOnlyTags{App: "release-app", Patterns: []string{"deploy/pulumi/v*"}}, provider)
	}, pulumi.WithMocks("proj", "stack", mocks))
	require.NoError(t, err)

	in, ok := mocks.seen["ruleset-widget-workflow-only-tags"]
	require.True(t, ok, "the ruleset is declared, keyed on repo + name; got %v", mocks.seen)

	assert.Equal(t, "workflow-only-tags", in["name"].StringValue())
	assert.Equal(t, "tag", in["target"].StringValue())
	assert.Equal(t, "active", in["enforcement"].StringValue())

	includes := in["conditions"].ObjectValue()["refName"].ObjectValue()["includes"].ArrayValue()
	require.Len(t, includes, 1)
	assert.Equal(t, "refs/tags/deploy/pulumi/v*", includes[0].StringValue())

	rules := in["rules"].ObjectValue()
	assert.True(t, rules["creation"].BoolValue())
	assert.True(t, rules["update"].BoolValue())
	assert.True(t, rules["deletion"].BoolValue())

	actors := in["bypassActors"].ArrayValue()
	require.Len(t, actors, 1)

	a := actors[0].ObjectValue()
	assert.EqualValues(t, 424242, a["actorId"].NumberValue())
	assert.Equal(t, "Integration", a["actorType"].StringValue())
	assert.Equal(t, "always", a["bypassMode"].StringValue())
}

func TestDeployWorkflowOnlyTagsDeclaresNothingWhenUnset(t *testing.T) {
	mocks := &captureMocks{seen: map[string]resource.PropertyMap{}}

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		return deployWorkflowOnlyTags(ctx, "widget", nil, nil, nil)
	}, pulumi.WithMocks("proj", "stack", mocks))
	require.NoError(t, err)
	assert.Empty(t, mocks.seen)
}

// An App slug that is not installed on the org stops the deploy: a ruleset
// whose bypass was silently dropped would lock the release workflow out.
func TestDeployWorkflowOnlyTagsRefusesUninstalledApp(t *testing.T) {
	mocks := &captureMocks{seen: map[string]resource.PropertyMap{}}

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		provider, err := github.NewProvider(ctx, "gh", &github.ProviderArgs{})
		if err != nil {
			return err
		}

		return deployWorkflowOnlyTags(ctx, "widget", registry.InstalledApps{},
			&registry.WorkflowOnlyTags{App: "release-app", Patterns: []string{"deploy/v*"}}, provider)
	}, pulumi.WithMocks("proj", "stack", mocks))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not installed")
	assert.NotContains(t, mocks.seen, "ruleset-widget-workflow-only-tags")
}
