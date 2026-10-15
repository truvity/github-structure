// Command github-structure runs the operations that are not Pulumi
// resources. Today: reconciling code security configuration
// attachments (`security-configurations plan|apply`).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/truvity/github-structure/pkg/registry"
	"github.com/truvity/github-structure/pkg/seccfg"
)

const usage = `usage: github-structure security-configurations <plan|apply> --org <org> (--desired <file> | --registry <github.yaml>) [flags]

  plan    read GitHub, print the attachments needed; writes nothing
  apply   perform them; refuses without --confirm

flags:
  --org <org>              organization login (required)
  --desired <file>         YAML: "repos: {<repo>: <configuration name>}"
  --registry <file>        a registry file named github.yaml; its rows'
                           security_configuration / the org default
  --show-unmanaged         list the unmanaged repositories, not just count them
  --confirm                (apply) actually write

The token is read from GH_TOKEN, else GITHUB_TOKEN. Nothing is ever detached.`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, seccfg.DefaultBaseURL)

	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, baseURL string) int {
	if len(args) < 2 || args[0] != "security-configurations" || (args[1] != "plan" && args[1] != "apply") {
		_, _ = fmt.Fprintln(stderr, usage)

		return 2
	}

	if err := securityConfigurations(ctx, args[1], args[2:], stdout, stderr, baseURL); err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)

		return 1
	}

	return 0
}

func securityConfigurations(ctx context.Context, verb string, args []string, stdout, stderr io.Writer, baseURL string) error {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(stderr)

	org := fs.String("org", "", "organization login")
	desiredPath := fs.String("desired", "", "desired-state file")
	registryPath := fs.String("registry", "", "registry file (github.yaml)")
	showUnmanaged := fs.Bool("show-unmanaged", false, "list unmanaged repositories")
	confirm := fs.Bool("confirm", false, "apply: actually write")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *org == "" {
		return errors.New("--org is required")
	}

	if (*desiredPath == "") == (*registryPath == "") {
		return errors.New("exactly one of --desired and --registry is required")
	}

	if verb == "plan" && *confirm {
		return errors.New("--confirm is for apply; plan never writes")
	}

	desired, err := loadDesired(*org, *desiredPath, *registryPath)
	if err != nil {
		return err
	}

	token, err := seccfg.TokenFromEnv()
	if err != nil {
		return err
	}

	client := seccfg.NewClient(token)
	client.BaseURL = baseURL

	plan, err := seccfg.Plan(ctx, client, *org, desired)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprint(stdout, plan.String())

	if *showUnmanaged {
		for _, r := range plan.Unmanaged {
			_, _ = fmt.Fprintln(stdout, "unmanaged", r)
		}
	}

	if verb == "plan" {
		return nil
	}

	if !*confirm {
		return errors.New("refusing to apply without --confirm (the plan above was not applied)")
	}

	if plan.Empty() {
		return nil
	}

	if err := seccfg.Apply(ctx, client, plan); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(stdout, "applied: %d attachment(s) requested\n", len(plan.Changes))

	return nil
}

func loadDesired(org, desiredPath, registryPath string) (map[string]string, error) {
	if desiredPath != "" {
		data, err := os.ReadFile(desiredPath)
		if err != nil {
			return nil, err
		}

		return seccfg.ParseDesired(data)
	}

	if filepath.Base(registryPath) != "github.yaml" {
		return nil, errors.New("--registry must point at a file named github.yaml")
	}

	cfg, err := registry.Load(os.DirFS(filepath.Dir(registryPath)))
	if err != nil {
		return nil, err
	}

	o, ok := cfg.Orgs[org]
	if !ok {
		return nil, fmt.Errorf("organization %q is not in the registry", org)
	}

	return o.SecurityConfigurationAssignments(), nil
}
