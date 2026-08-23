# The registry doctrine

The registry is one YAML file. Its design constraint is
**company-agnosticism**: profiles are top-level and shared, so a second
organization is a new `orgs:` key referencing the same profiles — never
new code. Everything below follows from four rules.

## 1. One row per thing; adding is a row, never code

The registry is DESIRED STATE for everything the engine can apply, and
INVENTORY for everything it cannot (GitHub has no API for App creation,
installation, or permission approval — those rows exist so drift checks
and humans can see them). If adding a repository to your estate requires
touching Go, the abstraction has failed.

## 2. Profiles set every field; overrides carry reasons

A profile must set EVERY field — the loader rejects a partial one. That
is deliberate: a partial profile is an invisible dependency on defaults,
and defaults move.

A repository points at exactly one profile. Deviations go through
`overrides:` with a `reason:` naming the keep-or-fix decision. The test
for a good reason is that a stranger can tell whether the exception is
still needed. Reasons rot too — the estate this library comes from
normalises them away: an override whose reason has expired is DELETED,
and the deletion commit records why.

## 3. The merge-gate doctrine: three patterns, one spelling each

Every repository sits in exactly one of three merge-gate patterns:

1. **No gate** — the profile's `required_checks: []`. For estates whose
   CI contract has not landed. When an estate gains a working `check`
   on PRs, flip the PROFILE to require it and mark stragglers with
   per-repo `checks_waived:` — the exception stays visible AS an
   exception rather than hidden in a profile that asks for nothing.
2. **Checks, no approval** — a check-requiring profile plus a
   `required_approvals: 0` override with a `reason:`. The approval knob
   is a statement about the maintainer pool, not the code: where the
   pool is too small to survive you-cannot-approve-your-own-PR,
   requiring one deadlocks the repository.
3. **Checks and approval** — a check-requiring profile as-is.

Note the coupling patterns 2 and 3 accept: with `enforce_admins` on
there is no merge-button bypass, so a dead CI estate blocks every PR.
The escape hatch is re-adding the waiver and applying from a working
tree, which needs no merge.

### Waivers, precisely

`checks_waived:` is free text with a mandatory shape: it names what must
land and when to delete the waiver ("Exit: delete when ci.yaml reports
`check` on PRs and master"). Two enforcement points:

- The loader rejects a waiver on a repository whose profile requires no
  checks (the waiver has no cause) and on an archived repository (it can
  do nothing).
- The preflight probes the default branch for check runs and FAILS once
  the exit condition has come true — a stale waiver blocks the next
  deploy until it is deleted. In the source estate the first stale
  waiver was caught within hours of going stale.

Why so much ceremony for a boolean? Because a required context that no
workflow produces blocks every PR forever, and the failure reads as "no
CI configured" rather than as a failure. Three repositories in the
source estate sat unmergeable that way on one afternoon.

## 4. Structure and membership are different planes

This engine owns STRUCTURE: orgs, teams (existence, nesting, privacy),
repositories and their settings, protection, Apps, runner groups. Team
MEMBERSHIP deliberately never appears in the registry — membership is
liveness-shaped data owned by whatever system tracks people (an IdP
join, an HR-driven roster). The same split shows in owners: the engine
ASSERTS each declared owner holds the role — one resource per login, so
it can promote but never demote — and the drift check reports owners
nobody declared. An accidentally-empty block must never mean "remove
everyone"; see docs/safety.md for the incident behind that sentence.
