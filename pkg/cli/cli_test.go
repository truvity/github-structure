package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/github-structure/pkg/app"
	"github.com/truvity/github-structure/pkg/preflight"
	"github.com/truvity/github-structure/pkg/registry"
)

// script is a checks set whose every read is clean and records call order.
func script(calls *[]string) checks {
	rec := func(name string, d ...app.Drift) func() ([]app.Drift, error) {
		return func() ([]app.Drift, error) {
			*calls = append(*calls, name)

			return d, nil
		}
	}

	return checks{
		orgVariables: func(context.Context, string, *registry.Org) ([]app.Drift, error) { return rec("variables")() },
		orgOwners:    func(context.Context, string, *registry.Org) ([]app.Drift, error) { return rec("owners")() },
		undeclared:   func(context.Context, string, *registry.Org) ([]app.Drift, error) { return rec("undeclared")() },
		repoSettings: func(context.Context, string, *registry.Config) ([]app.Drift, error) { return rec("settings")() },
		bypass:       func(context.Context, string, *registry.Config) ([]app.Drift, error) { return rec("bypass")() },
		scopes:       func(context.Context, string, *registry.Config) ([]app.Drift, error) { return rec("scopes")() },
		reconcileVar: func(context.Context, string, *registry.Config) ([]string, error) {
			*calls = append(*calls, "apply-variables")

			return []string{"set variable EXAMPLE"}, nil
		},
		reconcileScp: func(context.Context, string, *registry.Config) ([]string, error) {
			*calls = append(*calls, "apply-scopes")

			return nil, nil
		},
		preflight: func(context.Context, preflight.Opts) error {
			*calls = append(*calls, "preflight")

			return nil
		},
	}
}

func newRunner(t *testing.T, chk checks) (*runner, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()

	var out, errb bytes.Buffer

	return &runner{
		cfg: Config{
			Registry:   os.DirFS("testdata"),
			DefaultOrg: "acme",
			Stdout:     &out,
			Stderr:     &errb,
		},
		chk: chk,
	}, &out, &errb
}

func TestDriftRunsEveryCheckInOrderAndSummarises(t *testing.T) {
	var calls []string

	r, out, errb := newRunner(t, script(&calls))

	assert.Equal(t, 0, r.run(context.Background(), []string{"drift"}), errb.String())
	assert.Equal(t, []string{"variables", "owners", "undeclared", "settings", "bypass", "scopes"}, calls)
	assert.Equal(t, "acme: 0 variable rows, 0 owner(s) and 1 repo(s) match live GitHub\n", out.String())
}

func TestDriftReportsEachFindingAndCountsThem(t *testing.T) {
	var calls []string

	chk := script(&calls)
	chk.bypass = func(context.Context, string, *registry.Config) ([]app.Drift, error) {
		return []app.Drift{{Subject: "widget", Field: "bypass", Want: "none", Got: "an app"}}, nil
	}

	r, out, errb := newRunner(t, chk)

	assert.Equal(t, 1, r.run(context.Background(), []string{"drift"}))
	assert.Contains(t, out.String(), "widget")
	assert.Contains(t, errb.String(), "1 drift(s)")
}

func TestDriftStopsAtTheFirstReadError(t *testing.T) {
	var calls []string

	chk := script(&calls)
	chk.orgOwners = func(context.Context, string, *registry.Org) ([]app.Drift, error) {
		return nil, errors.New("boom")
	}

	r, _, errb := newRunner(t, chk)

	assert.Equal(t, 1, r.run(context.Background(), []string{"drift"}))
	assert.Equal(t, []string{"variables"}, calls)
	assert.Contains(t, errb.String(), "boom")
}

func TestEntitlementsApplyWritesValuesBeforeScopes(t *testing.T) {
	var calls []string

	r, out, errb := newRunner(t, script(&calls))

	assert.Equal(t, 0, r.run(context.Background(), []string{"entitlements", "apply"}), errb.String())
	assert.Equal(t, []string{"apply-variables", "apply-scopes"}, calls)
	assert.Equal(t, "set variable EXAMPLE\n", out.String())
}

func TestEntitlementsApplyCleanEstatePrintsOneLine(t *testing.T) {
	var calls []string

	chk := script(&calls)
	chk.reconcileVar = func(context.Context, string, *registry.Config) ([]string, error) { return nil, nil }

	r, out, _ := newRunner(t, chk)

	assert.Equal(t, 0, r.run(context.Background(), []string{"entitlements", "apply"}))
	assert.Equal(t, "acme: every declared variable and scope already matches live GitHub\n", out.String())
}

func TestEntitlementsNeedsTheApplyVerb(t *testing.T) {
	var calls []string

	r, _, _ := newRunner(t, script(&calls))

	assert.Equal(t, 2, r.run(context.Background(), []string{"entitlements"}))
	assert.Empty(t, calls)
}

func TestOrgFlagSelectsTheOrgAndUnknownOrgNamesTheKnownOnes(t *testing.T) {
	var calls []string

	r, _, errb := newRunner(t, script(&calls))

	assert.Equal(t, 1, r.run(context.Background(), []string{"--org", "other-org", "drift"}))
	assert.Contains(t, errb.String(), `org "other-org" is not in the registry (known: acme)`)
	assert.Empty(t, calls, "no GitHub read for an org the registry does not declare")
}

func TestNoDefaultOrgMeansOrgIsRequired(t *testing.T) {
	var calls []string

	r, _, errb := newRunner(t, script(&calls))
	r.cfg.DefaultOrg = ""

	assert.Equal(t, 1, r.run(context.Background(), []string{"drift"}))
	assert.Contains(t, errb.String(), "--org is required")
}

func TestPreflightPassesTheEstatePolicyThrough(t *testing.T) {
	var calls []string

	chk := script(&calls)

	var got preflight.Opts

	chk.preflight = func(_ context.Context, o preflight.Opts) error {
		got = o

		return nil
	}

	r, _, errb := newRunner(t, chk)
	r.cfg.TeamFillable = func(slug string) bool { return strings.HasPrefix(slug, "roster-") }
	r.cfg.Stack = func(_ context.Context, org string) (auto.Stack, error) {
		assert.Equal(t, "acme", org)

		return auto.Stack{}, nil
	}

	assert.Equal(t, 0, r.run(context.Background(), []string{"preflight"}), errb.String())
	assert.Equal(t, "acme", got.Org)
	assert.NotNil(t, got.Registry)
	assert.True(t, got.TeamFillable("roster-example"))
	assert.False(t, got.TeamFillable("robots"))
}

func TestPreflightWithoutAStackSelectorFailsLoudly(t *testing.T) {
	var calls []string

	r, _, errb := newRunner(t, script(&calls))

	assert.Equal(t, 1, r.run(context.Background(), []string{"preflight"}))
	assert.Contains(t, errb.String(), "Config.Stack is nil")
	assert.Empty(t, calls)
}

func TestPreflightStackSelectionErrorIsWrapped(t *testing.T) {
	var calls []string

	r, _, errb := newRunner(t, script(&calls))
	r.cfg.Stack = func(context.Context, string) (auto.Stack, error) { return auto.Stack{}, errors.New("no such stack") }

	assert.Equal(t, 1, r.run(context.Background(), []string{"preflight"}))
	assert.Contains(t, errb.String(), "select stack acme: no such stack")
	assert.Empty(t, calls)
}

func TestExtraCommandsReceiveTheOrgAndRegistry(t *testing.T) {
	var calls []string

	r, out, errb := newRunner(t, script(&calls))
	r.cfg.Extra = []Command{{
		Name:  "subjects",
		Usage: "an estate command",
		Run: func(_ context.Context, env Env) error {
			_, _ = env.Stdout.Write([]byte(env.Org + " " + strings.Join(env.Registry.SortedOrgs(), ",")))

			return nil
		},
	}}

	assert.Equal(t, 0, r.run(context.Background(), []string{"subjects"}), errb.String())
	assert.Equal(t, "acme acme", out.String())
}

func TestUsageListsExtraCommandsAndUnknownCommandIsAUsageError(t *testing.T) {
	var calls []string

	r, _, errb := newRunner(t, script(&calls))
	r.cfg.Extra = []Command{{Name: "subjects", Usage: "an estate command"}}

	assert.Equal(t, 2, r.run(context.Background(), []string{"nope"}))
	assert.Contains(t, errb.String(), `unknown command "nope"`)
	assert.Contains(t, errb.String(), "subjects")
	assert.Contains(t, errb.String(), `(default "acme")`)
}

func TestHelpExitsZeroAndNoArgsIsUsageError(t *testing.T) {
	var calls []string

	r, out, _ := newRunner(t, script(&calls))

	assert.Equal(t, 0, r.run(context.Background(), []string{"--help"}))
	assert.Contains(t, out.String(), "usage: githubctl")
	assert.Equal(t, 2, r.run(context.Background(), nil))
}

func TestRunWithoutARegistryFails(t *testing.T) {
	var out, errb bytes.Buffer

	code := Run(context.Background(), Config{DefaultOrg: "acme", Stdout: &out, Stderr: &errb}, []string{"drift"})

	require.Equal(t, 1, code)
	assert.Contains(t, errb.String(), "no registry configured")
}
