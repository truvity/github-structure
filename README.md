# github-structure

**A GitHub organization's structure as code — with the discipline that
makes it survivable.**

One YAML registry describes everything an organization *is*: settings,
teams, repositories, branch protection, rulesets, Actions policy. A
Pulumi engine applies it. A preflight
refuses the plans that history shows destroy things. Drift checks find
what nobody declared.

| Package | What |
| --- | --- |
| `pkg/registry` | The registry schema: settings **profiles**, per-repo **overrides with reasons**, **waivers with exit conditions**; strict loading (unknown keys are errors) and validation |
| `pkg/engine` | The Pulumi engine: org settings, owners, teams, repos, protection, rulesets, Actions permissions — import-first adoption of what already exists |
| `pkg/preflight` | The guards that run BEFORE the engine: refuse replaces of irreplaceable resources, catch waivers that outlived their cause, refuse plans that empty teams |
| `pkg/app` | The GitHub App side: App JWT, the REST client, drift checks (variables, owners, repo settings, bypass surfaces), and resolving a ruleset's bypass App slugs against the org's live installations. It authenticates AS an App; it never creates, inventories or audits one |

Extracted from a production estate that manages two organizations with
it; the docs keep the incidents that shaped each rule, because the rule
without its incident reads as pedantry.

Part of a documentation triangle: this repo covers what a repository
*is*; [ci-workflows](https://github.com/truvity/ci-workflows) covers
what it *does* (see its `docs/estate-lifecycle.md` for the end-to-end
path); [ci-plane](https://github.com/truvity/ci-plane) covers where CI
*runs* and the artifact doctrine (`docs/normalization.md`). All three
components are held to the [component contract](https://github.com/truvity/policy/blob/master/docs/contracts/component.md).

## Who it is for

Teams that run one or more GitHub organizations as Pulumi-managed
infrastructure and want the org's teams, repositories, branch protection,
rulesets and Actions policy declared in one registry file instead of
clicked through settings pages, one repository at a time. It is a Go
library, not a service: you write the Pulumi program for your estate and
import these packages; there is no bundled registry file to run against —
bring your own `orgs:` YAML.

## The model

**1. Profiles, and overrides that must explain themselves.** Every
repository points at exactly one settings profile (`public`, `private`,
yours). A repo may deviate — but only through an `overrides:` block whose
`reason:` names the decision that made the deviation deliberate. There is
no third state: a setting is either the profile's or a documented
exception. Estates rot through silent exceptions; this schema makes them
unrepresentable.

**2. Waivers carry their exit condition.** A new repository has no CI, so
its required `check` context would block every PR forever. The registry
answer is `checks_waived:` — free text that must state when the waiver
dies ("delete when ci.yaml reports `check` on master"). The preflight
probes live CI and **fails the deploy once the waiver's exit condition
has come true**: an exception can exist, but it cannot be forgotten.

**3. The engine must not be able to do the worst thing.** Some resources
hold state that lives outside any Pulumi stack — a team's membership, a
branch protection's standing. Replacing them "cleanly" destroys that
state invisibly: the plan after the replace reads clean while the members
are simply gone. The preflight refuses such plans outright; a human who
really means it can still act, but never by accident. See
[docs/safety.md](docs/safety.md) for each guard and the incident that
earned it.

## Install and a worked example

```sh
go get github.com/truvity/github-structure@v0.12.2
```

```go
//go:embed github.yaml
var content embed.FS

reg, err := registry.Load(content)                    // strict: unknown keys are errors
// ... read your engine App's credentials from wherever your estate keeps
// secrets (SSM, SOPS, a manager) ...
err = engine.Deploy(ctx, logger, "my-org", reg, engine.Credentials{
    AppID: id, InstallationID: inst, PrivateKey: pem,
})
```

One stack per organization. The registry is shared: a second org is a new
`orgs:` key referencing the same profiles — never new code.

Run `preflight` against the stack's `pulumi preview` before every apply
(wire it as a hard dependency of your deploy command, so a refused plan
never reaches Pulumi), and validate applies with `pulumi preview
--refresh` afterwards.

## Consumers

Imported as a Go library by:

- **truvity/gitops**: uses the registry, engine, preflight and app packages
  to manage the production organization's structure
- **A second, non-AWS estate**: uses the registry, engine and app packages
  to manage the per-team organization's structure (it does not import
  `preflight`)

Each estate maintains its own registry and stack but uses the shared
library.

## Neighbours

- **ci-workflows**: defines the estate's component contract and CI discipline
- **access-roster**: maintains the roster of people and groups; the registry
  must refer to teams that access-roster can reconcile (teams are listed but
  not created here)

## Documentation

- [docs/doctrine.md](docs/doctrine.md) — the registry model: presets as
  diffs, overrides with reasons, the review gate, waivers, what is
  written once, the structure/membership split
- [docs/safety.md](docs/safety.md) — every guard, with the incident that
  produced it
- [docs/adoption.md](docs/adoption.md) — adopting an existing organization
  (import-first), creating the engine's App with one click, day-1 order
- [docs/registry.md](docs/registry.md) — the registry file, field by field

## The rule that makes this repository public

**Mechanism only.** The registry FILE — your orgs, teams, repos, waiver
texts, credential paths — stays in your (private) estate; this library
carries the schema, the engine and the guards. `hack/leak-canary.sh`
enforces it in CI, because public history cannot be unpublished.

## Status

Used in production by two organizations. The packages are stable: the schema
converges on completeness, and the engine and guards are battle-tested
against the actual incidents that shaped them.

## Development

The toolchain (Go, `golangci-lint`, `goreleaser`, `govulncheck`, `just`) is
pinned in `devbox.json`; `devbox shell` (or direnv, on `cd` into the repo)
puts it on `PATH`. `just --list` shows every recipe; `just check` runs what
CI runs on a pull request — `build`, `test`, `lint`, `leak-canary` — plus
`vuln`, which CI otherwise runs on its own daily schedule so a new
advisory never turns a PR red.

## Releasing

A release is a `v*` git tag. `.github/workflows/release.yaml` triggers on
that tag and calls the shared `release-public.yaml` workflow
(truvity/ci-workflows), which runs `goreleaser`; `.goreleaser.yaml` sets
`skip: true` on builds, since this is a library, so the only output is a
GitHub Release whose changelog is generated from the commits since the
last tag (conventional-commit prefixes `chore`, `ci`, `docs` and `test`
are excluded). The Go module itself is versioned by the tag; nothing else
is published.

Most tags are cut automatically: `.github/workflows/auto-release.yaml`
runs on every push to `master`, gated on the org variables `AUTO_RELEASE`
and `ACCESS_ROSTER_ISSUER` both being set (they are, for this repository).
A push whose merged PR carries the `security` label releases immediately;
every other push is picked up by the Monday cron that follows.

## Licence

MIT — see [LICENSE](LICENSE).
