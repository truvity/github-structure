# github-structure

**A GitHub organization's structure as code — with the discipline that
makes it survivable.**

One YAML registry describes everything an organization *is*: settings,
teams, repositories, branch protection, rulesets, Actions policy, the
GitHub Apps it depends on. A Pulumi engine applies it. A preflight
refuses the plans that history shows destroy things. Drift checks find
what nobody declared.

| Package | What |
| --- | --- |
| `pkg/registry` | The registry schema: settings **profiles**, per-repo **overrides with reasons**, **waivers with exit conditions**; strict loading (unknown keys are errors) and validation |
| `pkg/engine` | The Pulumi engine: org settings, owners, teams, repos, protection, rulesets, Actions permissions, App inventory — import-first adoption of what already exists |
| `pkg/preflight` | The guards that run BEFORE the engine: refuse replaces of irreplaceable resources, catch waivers that outlived their cause, refuse plans that empty teams |
| `pkg/app` | The GitHub App side: manifest-flow creation (one browser click, no PAT), App JWT, the REST client, drift checks |

Extracted from a production estate that manages two organizations with
it; the docs keep the incidents that shaped each rule, because the rule
without its incident reads as pedantry.

Part of a documentation triangle: this repo covers what a repository
*is*; [ci-workflows](https://github.com/truvity/ci-workflows) covers
what it *does* (see its `docs/estate-lifecycle.md` for the end-to-end
path); [ci-plane](https://github.com/truvity/ci-plane) covers where CI
*runs* and the artifact doctrine (`docs/normalization.md`).

## The three ideas

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

## Quickstart

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

## Documentation

- [docs/doctrine.md](docs/doctrine.md) — the registry model: profiles,
  overrides, waivers, merge-gate patterns, the structure/membership split
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

## Licence

MIT — see [LICENSE](LICENSE).
