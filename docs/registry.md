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
    has_issues: …    # every settings field REQUIRED — no partial profiles
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
    credentials_ssm_prefix: …     # where YOUR estate keeps the engine
                                  # App's credentials; shape is yours
    app_prefix: "{org}-"          # display names are globally unique
    settings: {…}                 # org-level toggles
    owners: [login, …]            # asserted (promote-only) + drift-checked
    teams:
      <slug>: {privacy: …, parent: …}   # existence/nesting — NEVER members
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
          - {name: …, pattern: refs/tags/…, bypass_teams: […],
             bypass_apps: […]}     # App NAMES from this org's `apps`
        branch_rulesets: [...]
    apps:                         # inventory + drift for GitHub Apps
      <name>:
        external: true            # third-party: drift detection only
        permissions: {…}          # compared against live
        credentials:
          op_item: …              # source-of-truth item in your store
          ssm_prefix: …           # optional machine mirror
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

  `bypass_apps` takes App **names** — keys of the same organization's
  `apps` map — and the loader reads each id back out of the App's own
  row. The id is a fact the registry already states once; stating it a
  second time inside a ruleset makes it hand-copied, unreadable and
  stale the moment an App is recreated. A name that does not resolve is
  a LOAD ERROR naming the repository and the ruleset, never an actor
  that quietly goes missing — a dropped bypass says nothing until the
  act it permits is refused. Resolution is org-scoped: a ruleset can
  only name an App its own organization declares.
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

## Entitlement scopes — derived, never hand-kept

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
          derive_profile: public   # every non-archived public-profile repo
          repos: [workstation]     # explicit additions beyond the rule
    secrets:
      RENOVATE_APP_PRIVATE_KEY:    # names + scope only — values NEVER live here
        visibility: selected
        scope:
          derive_profile: public
```

Why derived: a hand-kept list is the entitlement dead zone. Workflows on
unentitled repos "skip cleanly" — renovate silently never ran on seven
repos (INF-580, found twice: two repos 2026-08-25, seven more
2026-08-27) and nothing noticed, because a missing entitlement looks
exactly like a quiet day. Under a derived rule, new repos matching the
profile are entitled at birth by the next reconcile, and every hand
mutation surfaces as drift.

The mechanics live in `pkg/app`: `ReconcileEntitlementScopes` (idempotent
set-semantics PUTs — GitHub's selected-repositories endpoint replaces the
whole list, so there is no delete window and no Pulumi import problem)
and `CheckEntitlementScopes` (symmetric-difference drift). Secrets are
declared by NAME and scope only; their values reach GitHub outside this
registry.

Known third surface NOT yet covered: GitHub **App installation**
repository lists. The API to read or edit another App's installation
membership requires credentials this tooling deliberately does not hold
(org-owner UI, or user-to-server grants beyond the CLI token) — the
renovate App's own selected list was the third layer of the same dead
zone. Until that grows an API story, App installs stay a checklist item
in the estate's safety runbook.
