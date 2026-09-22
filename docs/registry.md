# The registry file, field by field

`registry.Load(fsys)` reads `github.yaml` from the root of the given
`fs.FS` (an `embed.FS` in the consuming estate) and rejects unknown keys
— a typo is an error, never a silently-ignored setting.

```yaml
profiles:            # shared, top-level — the whole point
  <profile-name>:    # e.g. public, private
    review: none|required   # the default branch's REVIEW gate, for every
                            # repo on this preset (see below). The one
                            # optional field: omit it where the class's
                            # gate is still hand-spelled in `protection`.
    visibility: public|private
    has_issues: …    # ONLY WHAT DIFFERS from GitHub's own new-repository
                     # defaults — a preset is a diff (see below)
    actions: {allowed_actions: …, default_workflow_permissions: …, …}
    protection:
      enabled: true|false          # false = NO rule at all (a different
                                   # state from an empty rule)
      required_checks: [check]     # non-strict recommended: strict
                                   # serialises merges for no safety gain
      enforce_admins: …
      required_approvals: 0|N      # 0 = no review block AT ALL (GitHub
                                   # distinguishes it from "zero approvals")
      …
    teams:           # DIRECT team→repo grants only; effective/inherited
      <team>: pull|triage|push|maintain|admin   # access is NOT declared

orgs:
  <login>:
    default_access: [bundle, …]   # bundles every repo here gets unless
                                  # its row decides for itself; `access: []`
                                  # on a row opts out
    engine_credentials:           # where YOUR estate keeps the engine
      ssm_prefix: …               # App's credentials for THIS org —
                                  # exactly one source. Either an
                                  # SSM-shaped prefix (one parameter per
                                  # field)…
      openbao:                    # …or one OpenBAO/Vault KV v2 secret
        namespace: …              # (optional; empty = root)
        mount: kv                 # the KV v2 mount
        path: …                   # the secret, with no `data/` segment
                                  # The property/parameter NAMES inside
                                  # are your estate's convention; the
                                  # library declares the place only.
    settings: {…}                 # org-level toggles
    owners: [login, …]            # asserted (promote-only) + drift-checked
    team_defaults:                # what a team row inherits for the
      privacy: closed             # fields it leaves unsaid; a row that
      notifications: enabled      # writes the field keeps what it wrote
    teams:
      <slug>: {privacy: …, parent: …}   # existence/nesting — NEVER members
    tag_rulesets:                 # NAMED bodies, written once, that a
      <name>:                     # row references by name below
        {pattern: refs/tags/…, bypass_teams: […], bypass_apps: […]}
    checks_waived:                # waivers grouped by REASON: the text
      <reason>: [<name>, …]       # once, the repositories as a list
    repos:
      <name>:
        profile: <profile-name>
        review: none|required     # this repo's gate, overriding its
                                  # preset's. No reason: required — which
                                  # repos need a reviewed merge is a fact
                                  # about them, not a settings deviation.
        description: …            # optional; written to GitHub
        archived: true            # read-only rows: repo + nothing else
        checks_waived: >-         # free text, MUST carry its exit
          New repo, no CI yet. Exit: delete when ci.yaml reports
          `check` on PRs and master.
        overrides:                # sparse; each block carries reason:
          reason: …               # the decision that made it deliberate
          protection: {…}
        tag_rulesets:             # bypass_teams reference declared teams
          - <name>                # …the org's named ruleset of that name
          - {name: …, pattern: refs/tags/…, bypass_teams: […],
             bypass_apps: […]}     # …or a one-off body. App SLUGS, live
        branch_rulesets: [...]
    runner_groups: {…}
```

Sharp edges the schema enforces (each learned, not designed):

- A waiver on a no-checks profile is rejected — it has no cause.
- A waiver on an archived repository is rejected — no branch, no effect.
- `required_approvals: 0` renders NO review block: sending a zero-count
  block would turn reviews ON.
- Protection `enabled: false` creates no resource — deliberately
  distinct from an empty rule.
- Team grants list only DIRECT grants. GitHub's per-repo team listing
  reports EFFECTIVE access including inheritance; declaring those
  would CREATE direct grants that did not exist.
- Fields GitHub owns in a given org state (e.g. `allow_forking` where
  the org forbids private forking) are excluded from writes —
  `repoIgnoredFields` in pkg/engine records each, with tests naming why.

## A preset is a diff against GitHub's defaults

A preset states what is true of YOUR estate. Anything it does not
mention is GitHub's own answer for a new repository — private, `main`,
all three merge buttons, no auto-merge, read-only workflow token, no
branch protection rule.

```yaml
presets:
  private:                       # ~10 lines, not 33
    default_branch: master
    allow_merge_commit: false
    allow_auto_merge: true
    delete_branch_on_merge: true
    has_wiki: false
    actions: {default_workflow_permissions: write}
    protection: {enabled: true, required_checks: [check], enforce_admins: true}
```

**Every field is still applied.** Resolution is base → preset →
overrides, and the resolved state sets all 33 — so nothing is left
unmanaged, which is the property the old "state every field" rule
existed to protect. What changed is only which of them a human writes
down.

**The base is a constant, not a lookup.** It is what GitHub creates a
repository with, asserted in the library rather than fetched. If GitHub
changes a default tomorrow, nothing your estate applies moves: the
engine still sets every field from the resolved state, and the resolved
state still comes from the table. Being wrong about an entry therefore
costs a preset that reads as if it inherits one thing while inheriting
another — visible in the resolved state — not a live setting that
silently drifts.

**One level only.** Base → preset. A preset cannot extend another
preset: a chain is a thing you have to unwind in your head to know what
a repository gets, and eleven presets taught that lesson once already.

## `default_access` — an estate-wide grant, written once

Where an organization grants the same teams on nearly every repository,
`default_access` says it at the org and each row inherits it:

```yaml
orgs:
  example:
    default_access: [platform]
    repos:
      ordinary: {preset: private}                  # inherits [platform]
      special:  {preset: private, access: [other]} # its own bundles
      alone:    {preset: private, access: []}      # opts out, deliberately
```

Silence inherits; an **explicit empty list** opts out. The asymmetry is
the feature — a row that means "no grants" has to say so, so it reads as
a decision rather than a forgotten key. Inheritance is applied at load,
so resolution, entitlements and drift all see one effective row, and the
resolved state still shows every grant per repository.

## Said once — named tag rulesets, waivers by reason, team defaults

Three more places where an estate used to write one decision many
times. Each resolves **at load** into exactly the rows it replaces, so
the engine, drift and preflight see the structs they always saw, and an
estate that adopts them changes nothing it applies — the resolved state
before and after is the proof.

**A named tag ruleset** is a body under the org's `tag_rulesets:`, keyed
by the ruleset's display name; a row's `tag_rulesets:` list then names
it. A row may still write a one-off body, but not under a name the org
defines — two bodies for one name is how they disagree. The key *is* the
name: a row referencing `release-tags` renders a ruleset called
`release-tags`, which matters because the engine keys the live resource
on repo + name, so renaming is a replacement with an unprotected window.

```yaml
orgs:
  example:
    tag_rulesets:
      release-tags:
        pattern: refs/tags/v*
        bypass_teams: [role-release]
        bypass_apps: [ci-automation]
    repos:
      library:  {preset: public, tag_rulesets: [release-tags]}
      another:  {preset: public, tag_rulesets: [release-tags]}
      oddity:                                     # a one-off stays inline
        preset: public
        tag_rulesets:
          - {name: project-tags, pattern: "refs/tags/*/v*", bypass_teams: [role-release]}
```

**Waivers by reason** are the org's `checks_waived:` — a map from the
reason to the repositories it covers. It is the same fact as the row's
`checks_waived:`, landed on each row at load, so `Org.ChecksWaived()`,
the preflight's stale-waiver guard and the archived-repository refusal
keep working unchanged. A repository is waived once: named on its row
*and* here, or under two reasons, is refused. A row keeps its own
`checks_waived:` for a reason that is its alone.

```yaml
orgs:
  example:
    checks_waived:
      "no workflow here reports `check` yet": [alpha, beta, gamma]
      "the workflows here deploy; none reports `check`": [delta]
```

**Team defaults** fill `privacy` and `notifications` where a team's row
leaves them unsaid; a row that writes the field keeps what it wrote.
Applied before validation, so an inherited `secret` on a nested team is
refused exactly as a written one would be.

## `review` — one field decides how a pull request becomes a merge

| `review` | what the engine renders | how automation merges |
| -- | -- | -- |
| `none` | classic protection: the resolved `required_checks`, **no** approval block | GitHub's own auto-merge, on green |
| `required` | a `pr-approval` ruleset on `~DEFAULT_BRANCH`: one approving review, organization admins bypass | an App with `pull_requests: write` approves, then auto-merge on green |

One approving review counts whether it comes from a person or from a
GitHub App: GitHub folds an App's review into `reviewDecision` the same
way. That is why `required` needs no `bypass_apps` — automation that
used to step OVER the gate now satisfies it, and the approval is a
review anyone can read instead of an exemption nobody sees.

`required` is a ruleset rather than classic protection for one reason:
only a ruleset carries the organization-admin bypass. Classic protection
either exempts admins from everything (`enforce_admins: false`) or from
nothing.

Where the checks live: classic protection, unless the repo has no
classic rule at all (`protection.enabled: false`), in which case the
rendered ruleset carries them. Exactly one resource enforces a context —
two would be two rules to keep in step, and a ruleset check is
bypassable where a classic one is not.

Sharp edges, all enforced by the loader:

- The field has ONE spelling per repo. `review` next to a non-zero
  `required_approvals`, next to `pull_request_bypassers`, or next to a
  hand-written approval ruleset is rejected — including the case where
  the value comes from the preset and the number from an override, which
  would otherwise drop an approval requirement silently.
- It lives on the repository ROW, never in `overrides:`.
- It is rejected on an archived repository: no branch, no pull requests.
- The rendered ruleset is named `pr-approval`, which is what hand-written
  approval rulesets were already called — so adopting the field UPDATES
  the live ruleset instead of replacing it. A ruleset replacement has a
  window with no gate at all.
- A repo whose rendered rule requires a context nothing has ever
  produced is refused by `preflight` (`checkUnreportedChecks`), with
  `checks_waived:` as the written escape. The opposite direction —
  a waiver whose context now reports — was already refused.

## Bypass semantics — pick the mechanism by what must stay automatic

Three ways to let an actor through a gate, and they are NOT
interchangeable (each learned on 2026-08-26):

- **Classic `pull_request_bypassers`** exempts the actor's PRs from the
  review requirement entirely — the ONLY shape **auto-merge** can ride:
  an armed PR completes on green checks alone. A ruleset bypass never
  does this.
- **Ruleset bypass actors** (`bypass_apps`, `bypass_org_admins`) permit
  a *deliberate, audit-visible act* — the "bypass rules" button, or a
  bot pushing a tag. Right for release acts; wrong for anything that
  must complete unattended on a PR.

  `bypass_apps` takes App **slugs**, resolved against the Apps INSTALLED
  on the organization (`GET /orgs/{org}/installations`) at deploy. The
  database id is GitHub's fact about an App: written into a file it is
  hand-copied, unreadable, and stale the moment the App is recreated —
  stale silently, which is the failure mode that matters. Two refusals
  keep it honest: an all-digits name is a LOAD error (the field's old
  spelling), and a name with no live installation stops the DEPLOY,
  naming the App. Never an actor that quietly goes missing — a dropped
  bypass says nothing until the act it permits is refused. Resolution is
  org-scoped because the installation list is: an App installed on a
  sibling organization does not resolve here.

  The reading costs the engine App `organization_administration: read`,
  and it is asked for only where some ruleset names a bypass App.
- **`enforce_admins: false`** is the blunt escape hatch: admins ignore
  the whole classic rule set. Prefer the two above.

Provider spellings that cost a failed apply each:

- `pull_request_bypassers` entries are **actor refs**: `/login` for a
  user, `org/team-slug` for a team. A bare login fails GraphQL node
  resolution at apply time.
- Ruleset App actors use the App's **database id** (the number in the
  App settings URL), not the node id.
- `OrganizationAdmin` ruleset actors must be declared with
  `actor_id: 0` — GitHub ignores the documented `1` on write and
  returns 0 on read, so any other spelling is a perpetual diff.

## Organization Actions variables and secrets

Both are declared under `settings.actions`, and the split between them
is one of custody:

- a **variable**'s value is not a secret — runner labels, bucket names,
  in-cluster URLs — so the registry holds it and
  `ReconcileOrgVariables` writes it. Name, value, visibility and
  membership are all owned here;
- a **secret** is declared by NAME, visibility and scope only. Its value
  is never read, written or known by this code: a registry that could
  write one would first need somewhere to read it from, which is a key
  custody problem it should not create. Put the source in a comment
  beside the row, so the inventory says who to ask.

Neither is a Pulumi resource. The structure engine authenticates as the
structure App, and GitHub gates the org variables API behind the
separate `organization_actions_variables` permission — which an App
cannot be granted through any API, only a console edit plus an
installation approval. Giving the engine the value while this
reconciler keeps the membership would also put two writers with two
identities on one object. `pkg/app/entitlements.go` carries the whole
argument.

### Entitlement scopes — derived, never hand-kept

A `selected`-visibility org variable or secret carries a `scope`: a
profile-wide rule plus explicit additions, resolved to the repo list at
apply time.

```yaml
settings:
  actions:
    variables:
      RENOVATE_CLIENT_ID:
        value: "123456"
        visibility: selected
        scope:
          derive_preset: public    # every non-archived repo on the `public` preset
          repos: [workstation]     # explicit additions beyond the rule
    secrets:
      RENOVATE_APP_PRIVATE_KEY:    # names + scope only — values NEVER live here
        visibility: selected
        scope:
          derive_preset: public
```

Why derived: a hand-kept list is the entitlement dead zone. Workflows on
unentitled repos "skip cleanly" — renovate silently never ran on seven
repos (INF-580, found twice: two repos 2026-08-25, seven more
2026-08-27) and nothing noticed, because a missing entitlement looks
exactly like a quiet day. Under a derived rule, new repos matching the
profile are entitled at birth by the next reconcile, and every hand
mutation surfaces as drift.

The mechanics live in `pkg/app`: `ReconcileOrgVariables` (create or
correct a variable's value and visibility, sending the resolved
membership with it so a PATCH is never a partial write),
`ReconcileEntitlementScopes` (idempotent set-semantics PUTs — GitHub's
selected-repositories endpoint replaces the whole list, so there is no
delete window) and, for drift, `CheckOrgVariables` plus
`CheckEntitlementScopes`.

Three refusals are deliberate:

- nothing here DELETES a variable or a secret. A live one that no row
  declares is reported by `CheckOrgVariables` and a human decides —
  removing something every CI job reads must not be reachable from an
  editing slip;
- creating a `selected` variable with no declared scope is an error, not
  an empty list. A variable nobody may read makes its workflows *skip*,
  which reads as a quiet day;
- a declared subject that is not live is a FINDING, not a failed run.
  Returning it as an error ended the drift run at its first stale row
  and hid every finding behind it.

Known third surface NOT yet covered: GitHub **App installation**
repository lists. The API to read or edit another App's installation
membership requires credentials this tooling deliberately does not hold
(org-owner UI, or user-to-server grants beyond the CLI token) — the
renovate App's own selected list was the third layer of the same dead
zone. Until that grows an API story, App installs stay a checklist item
in the estate's safety runbook.
