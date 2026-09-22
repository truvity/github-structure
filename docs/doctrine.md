# The registry doctrine

The registry is one YAML file. Its design constraint is
**company-agnosticism**: profiles are top-level and shared, so a second
organization is a new `orgs:` key referencing the same profiles — never
new code. Everything below follows from four rules.

## 1. One row per thing; adding is a row, never code

The registry is DESIRED STATE for everything the engine can apply, and
INVENTORY for the few things it cannot (an org's code security
configurations, say — the provider has no resource for them, but "who
owns Dependabot alerts" is still worth answering). It is NOT a copy of
facts GitHub already holds: a ruleset names a bypass App, and the id is
read from the live installation list, because a copied id is stale the
moment the App is recreated and says nothing when it is. If adding a
repository to your estate requires touching Go, the abstraction has
failed.

## 2. A preset is a diff, the resolved state is complete, overrides carry reasons

A preset states what is true of your estate; whatever it leaves unsaid
is GitHub's own answer for a new repository, pinned as a constant in the
library. The property that matters is on the OTHER side: resolution is
base → preset → overrides, so the resolved state sets every field and
nothing is left unmanaged. A human writes down the decisions; the engine
still applies all of them. (Until v0.10.0 the same property was bought
by making a preset state all 33 fields — that rule is gone, the property
is not.)

A repository points at exactly one preset. Deviations go through
`overrides:` with a `reason:` naming the keep-or-fix decision. The test
for a good reason is that a stranger can tell whether the exception is
still needed. Reasons rot too — the estate this library comes from
normalises them away: an override whose reason has expired is DELETED,
and the deletion commit records why.

The same rule scales up: anything an estate would otherwise write per
row — a grant every repository gets, a tag ruleset eight repositories
share, a waiver reason sixty rows repeat, a team's privacy — is written
ONCE at the org and lands on the rows at load. The rows stay the unit
the engine reads; the file stops carrying copies. `docs/registry.md`
has each spelling.

## 3. The merge gate: checks from the preset, the review from one field

Every repository's default branch is gated by the contexts its preset
requires, and its REVIEW is one row field with two values:

- `review: none` — the checks alone; an automated pull request completes
  on green through GitHub's own auto-merge.
- `review: required` — one approving review, from a person or from an
  App with `pull_requests: write` (GitHub counts both), rendered as a
  ruleset because only a ruleset can exempt organization admins.

Which repositories require a review is a fact about the maintainer pool,
not the code: where the pool is too small to survive
you-cannot-approve-your-own-PR, requiring one deadlocks the repository.
That is why the field sits on the row, needs no `reason:`, and has ONE
spelling — the loader refuses a `review` beside the older approval knobs
in `protection`, so the file cannot say two things about the same gate.

A repository whose CI has not landed yet cannot pass a check nobody
produces, and with `enforce_admins` on there is no merge-button bypass.
The written escape is the waiver.

### Waivers, precisely

`checks_waived:` is free text with a mandatory shape: it names what must
land and when to delete the waiver ("Exit: delete when ci.yaml reports
`check` on PRs and master"). A repository whose exception shares its
reason with others is listed under that reason at the org instead — the
same waiver, written once. Either way the preset keeps stating the
intent, so the exception is visible AS an exception rather than hidden
in a preset that asks for nothing.

Why so much ceremony for a boolean? Because a required context that no
workflow produces blocks every PR forever, and the failure reads as "no
CI configured" rather than as a failure. Three repositories in the
source estate sat unmergeable that way on one afternoon. What enforces
the shape — the loader's refusals and the preflight that fails the
deploy once the exit condition has come true — is in `docs/safety.md`,
with the incidents.

## 4. Structure and membership are different planes

This engine owns STRUCTURE: orgs, teams (existence, nesting, privacy),
repositories and their settings, protection, runner groups. Team
MEMBERSHIP deliberately never appears in the registry — membership is
liveness-shaped data owned by whatever system tracks people (an IdP
join, an HR-driven roster). The same split shows in owners: the engine
ASSERTS each declared owner holds the role — one resource per login, so
it can promote but never demote — and the drift check reports owners
nobody declared. An accidentally-empty block must never mean "remove
everyone"; see docs/safety.md for the incident behind that sentence.
