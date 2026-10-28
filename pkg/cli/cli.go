// Package cli is the operator command line over this library: the GitHub
// plane operations that have no Pulumi resource, packaged so a consuming
// estate's own main is only wiring.
//
//	<tool> [--org <org>] drift                # registry vs live GitHub
//	<tool> [--org <org>] entitlements apply   # write declared variables and scopes
//	<tool> [--org <org>] preflight            # what a structure apply would touch
//	<tool> [--org <org>] <extra>              # commands the estate adds (Config.Extra)
//
// The package knows nothing about any estate. Everything particular is a
// Config field: where the registry is read from, the default
// organisation, which teams a roster refills, how the structure stack is
// selected, and any extra commands. It never creates GitHub Apps and
// never holds a key; GitHub is reached through the ambient `gh` CLI, like
// pkg/app.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"

	"github.com/truvity/github-structure/pkg/app"
	"github.com/truvity/github-structure/pkg/preflight"
	"github.com/truvity/github-structure/pkg/registry"
)

type (
	// Config is everything an estate supplies.
	Config struct {
		// Name is the tool's name in usage text. Default "githubctl".
		Name string
		// Registry holds github.yaml (and the files it references), in the
		// shape registry.Load reads. Required.
		Registry fs.FS
		// DefaultOrg is the --org default. Empty means --org is required.
		DefaultOrg string
		// TeamFillable reports whether some system refills the named
		// team's membership; passed to preflight.Opts. nil means no team
		// is refillable, the safe default.
		TeamFillable func(slug string) bool
		// Stack selects the org's already-initialised structure stack for
		// preflight. nil disables the preflight command (it then fails
		// with a clear error instead of being absent from usage).
		Stack func(ctx context.Context, org string) (auto.Stack, error)
		// Extra are estate-specific commands run beside the built-in ones.
		Extra []Command
		// Stdout and Stderr default to io.Discard when nil; main passes
		// os.Stdout and os.Stderr.
		Stdout, Stderr io.Writer
	}

	// Command is an estate-specific subcommand.
	Command struct {
		Name  string
		Usage string
		Run   func(ctx context.Context, env Env) error
	}

	// Env is what an Extra command is handed.
	Env struct {
		// Org is the resolved --org value.
		Org string
		// Registry is the loaded, validated registry.
		Registry *registry.Config
		Stdout   io.Writer
		Stderr   io.Writer
	}

	// checks are the live reads and writes, a field so tests can replace
	// GitHub with a script.
	checks struct {
		orgVariables func(ctx context.Context, org string, cfg *registry.Org) ([]app.Drift, error)
		orgOwners    func(ctx context.Context, org string, cfg *registry.Org) ([]app.Drift, error)
		undeclared   func(ctx context.Context, org string, cfg *registry.Org) ([]app.Drift, error)
		repoSettings func(ctx context.Context, org string, cfg *registry.Config) ([]app.Drift, error)
		bypass       func(ctx context.Context, org string, cfg *registry.Config) ([]app.Drift, error)
		scopes       func(ctx context.Context, org string, cfg *registry.Config) ([]app.Drift, error)
		reconcileVar func(ctx context.Context, org string, cfg *registry.Config) ([]string, error)
		reconcileScp func(ctx context.Context, org string, cfg *registry.Config) ([]string, error)
		preflight    func(ctx context.Context, o preflight.Opts) error
	}

	runner struct {
		cfg Config
		chk checks
	}
)

func liveChecks() checks {
	return checks{
		orgVariables: app.CheckOrgVariables,
		orgOwners:    app.CheckOrgOwners,
		undeclared:   app.CheckUndeclaredRepos,
		repoSettings: app.CheckRepoSettings,
		bypass:       app.CheckBypassSurfaces,
		scopes:       app.CheckEntitlementScopes,
		reconcileVar: app.ReconcileOrgVariables,
		reconcileScp: app.ReconcileEntitlementScopes,
		preflight:    preflight.Run,
	}
}

// Run executes one invocation (args excludes the program name) and returns
// the process exit code: 0 success, 1 failure or drift, 2 usage.
func Run(ctx context.Context, cfg Config, args []string) int {
	return (&runner{cfg: cfg, chk: liveChecks()}).run(ctx, args)
}

func (r *runner) name() string {
	if r.cfg.Name == "" {
		return "githubctl"
	}

	return r.cfg.Name
}

func (r *runner) stdout() io.Writer {
	if r.cfg.Stdout == nil {
		return io.Discard
	}

	return r.cfg.Stdout
}

func (r *runner) stderr() io.Writer {
	if r.cfg.Stderr == nil {
		return io.Discard
	}

	return r.cfg.Stderr
}

func (r *runner) usage() string {
	var b strings.Builder

	fmt.Fprintf(&b, "usage: %s [--org <org>] <command>\n\n", r.name())
	b.WriteString("commands:\n")
	b.WriteString("  drift                compare the registry against live GitHub; non-zero on any drift\n")
	b.WriteString("  entitlements apply   write every declared variable value/visibility, then PUT every declared scope\n")
	b.WriteString("  preflight            preview the structure stack and refuse a plan that would replace\n")
	b.WriteString("                       something whose state lives outside it\n")

	for _, c := range r.cfg.Extra {
		fmt.Fprintf(&b, "  %-20s %s\n", c.Name, c.Usage)
	}

	b.WriteString("\nflags:\n")
	b.WriteString("  --org <org>          organization login (a key under orgs: in the registry)")

	if r.cfg.DefaultOrg != "" {
		fmt.Fprintf(&b, " (default %q)", r.cfg.DefaultOrg)
	}

	b.WriteString("\n")

	return b.String()
}

func (r *runner) run(ctx context.Context, args []string) int {
	fset := flag.NewFlagSet(r.name(), flag.ContinueOnError)
	fset.SetOutput(io.Discard)

	org := fset.String("org", r.cfg.DefaultOrg, "organization login")

	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(r.stdout(), r.usage())

			return 0
		}

		_, _ = fmt.Fprintln(r.stderr(), "error:", err)
		_, _ = fmt.Fprint(r.stderr(), r.usage())

		return 2
	}

	rest := fset.Args()
	if len(rest) == 0 || rest[0] == "help" {
		_, _ = fmt.Fprint(r.stdout(), r.usage())

		if len(rest) == 0 {
			return 2
		}

		return 0
	}

	if err := r.dispatch(ctx, *org, rest); err != nil {
		var u usageError
		if errors.As(err, &u) {
			_, _ = fmt.Fprintln(r.stderr(), "error:", err)
			_, _ = fmt.Fprint(r.stderr(), r.usage())

			return 2
		}

		_, _ = fmt.Fprintln(r.stderr(), "error:", err)

		return 1
	}

	return 0
}

type usageError string

func (u usageError) Error() string { return string(u) }

func (r *runner) dispatch(ctx context.Context, org string, rest []string) error {
	switch rest[0] {
	case "drift":
		if len(rest) != 1 {
			return usageError("drift takes no arguments")
		}

		return r.drift(ctx, org)
	case "entitlements":
		if len(rest) != 2 || rest[1] != "apply" {
			return usageError("usage: entitlements apply")
		}

		return r.entitlementsApply(ctx, org)
	case "preflight":
		if len(rest) != 1 {
			return usageError("preflight takes no arguments")
		}

		return r.preflight(ctx, org)
	}

	for _, c := range r.cfg.Extra {
		if c.Name == rest[0] {
			reg, err := r.load()
			if err != nil {
				return err
			}

			return c.Run(ctx, Env{Org: org, Registry: reg, Stdout: r.stdout(), Stderr: r.stderr()})
		}
	}

	return usageError(fmt.Sprintf("unknown command %q", rest[0]))
}

func (r *runner) load() (*registry.Config, error) {
	if r.cfg.Registry == nil {
		return nil, errors.New("no registry configured")
	}

	return registry.Load(r.cfg.Registry)
}

// lookup loads the registry and resolves the organisation this invocation
// is about, failing with a usable message rather than a nil dereference.
func (r *runner) lookup(org string) (*registry.Config, *registry.Org, error) {
	if org == "" {
		return nil, nil, errors.New("--org is required")
	}

	reg, err := r.load()
	if err != nil {
		return nil, nil, err
	}

	orgCfg, ok := reg.Orgs[org]
	if !ok {
		return nil, nil, fmt.Errorf("org %q is not in the registry (known: %s)", org, strings.Join(reg.SortedOrgs(), ", "))
	}

	return reg, orgCfg, nil
}

// drift runs every read-only comparison of the registry against live
// GitHub and prints each finding; any finding is a non-zero exit.
//
// Apps are not compared: the registry does not describe them, and an App's
// state is the credential custodian's to report. What is compared is every
// surface an App TOUCHES: the bypass actors and the entitlement scopes.
func (r *runner) drift(ctx context.Context, org string) error {
	reg, orgCfg, err := r.lookup(org)
	if err != nil {
		return err
	}

	var drifts []app.Drift

	// Repo settings, bypass surfaces and scopes need the whole registry,
	// not just the org row: a repository's settings are a PROFILE with the
	// row's overrides on top, and only Config knows the profiles.
	steps := []func() ([]app.Drift, error){
		func() ([]app.Drift, error) { return r.chk.orgVariables(ctx, org, orgCfg) },
		func() ([]app.Drift, error) { return r.chk.orgOwners(ctx, org, orgCfg) },
		func() ([]app.Drift, error) { return r.chk.undeclared(ctx, org, orgCfg) },
		func() ([]app.Drift, error) { return r.chk.repoSettings(ctx, org, reg) },
		// Review-bypass allowances and ruleset actors: the hand-mutation
		// class that turns a refresh into a surprise App-bypass removal.
		func() ([]app.Drift, error) { return r.chk.bypass(ctx, org, reg) },
		// The derived selected-repo membership of the update-bot pair: the
		// dead zone where a missing entitlement looks exactly like a
		// quiet day.
		func() ([]app.Drift, error) { return r.chk.scopes(ctx, org, reg) },
	}

	for _, step := range steps {
		found, err := step()
		if err != nil {
			return err
		}

		drifts = append(drifts, found...)
	}

	if len(drifts) == 0 {
		_, _ = fmt.Fprintf(r.stdout(), "%s: %d variable rows, %d owner(s) and %d repo(s) match live GitHub\n",
			org, len(orgVariables(orgCfg)), len(orgCfg.Owners), len(orgCfg.Repos))

		return nil
	}

	for _, d := range drifts {
		_, _ = fmt.Fprintln(r.stdout(), d)
	}

	return fmt.Errorf("%d drift(s)", len(drifts))
}

// entitlementsApply enforces the registry's Actions block.
//
// VALUES FIRST, then scopes, and the order is load-bearing: GitHub answers
// 404 to the selected-repositories endpoint of a variable that does not
// exist, so a variable this run creates has to exist before its membership
// can be PUT. The create carries the membership with it, and the scope pass
// that follows then finds it already correct.
//
// Secret VALUES are not here and never will be: the reconciler has nowhere
// honest to read one from. Their membership is reconciled, their existence
// is a human's act, and a declared secret that is not live is reported
// rather than invented.
func (r *runner) entitlementsApply(ctx context.Context, org string) error {
	reg, _, err := r.lookup(org)
	if err != nil {
		return err
	}

	values, err := r.chk.reconcileVar(ctx, org, reg)
	if err != nil {
		return err
	}

	scopes, err := r.chk.reconcileScp(ctx, org, reg)
	if err != nil {
		return err
	}

	applied := make([]string, 0, len(values)+len(scopes))
	applied = append(applied, values...)
	applied = append(applied, scopes...)

	if len(applied) == 0 {
		_, _ = fmt.Fprintf(r.stdout(), "%s: every declared variable and scope already matches live GitHub\n", org)

		return nil
	}

	for _, a := range applied {
		_, _ = fmt.Fprintln(r.stdout(), a)
	}

	return nil
}

// preflight selects the org's structure stack through Config.Stack and
// hands the registry, the stack and the roster policy to pkg/preflight.
func (r *runner) preflight(ctx context.Context, org string) error {
	if r.cfg.Stack == nil {
		return errors.New("preflight is not configured: Config.Stack is nil")
	}

	reg, _, err := r.lookup(org)
	if err != nil {
		return err
	}

	stack, err := r.cfg.Stack(ctx, org)
	if err != nil {
		return fmt.Errorf("select stack %s: %w", org, err)
	}

	return r.chk.preflight(ctx, preflight.Opts{
		Org:          org,
		Registry:     reg,
		Stack:        stack,
		TeamFillable: r.cfg.TeamFillable,
	})
}

// orgVariables is nil-safe: an org may declare no Actions block at all.
func orgVariables(orgCfg *registry.Org) map[string]*registry.OrgVariable {
	if orgCfg.Settings == nil || orgCfg.Settings.Actions == nil {
		return nil
	}

	return orgCfg.Settings.Actions.Variables
}
