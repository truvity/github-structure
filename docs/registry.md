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
        review_dismiss_stale: true  # optional; the review: required gate
                                  # dismisses an approval when commits are
                                  # pushed. Also allowed on a profile; the
                                  # row wins, false opts out. Refused
                                  # without review: required.
        description: …            # optional; written to GitHub
        homepage: https://…       # optional; absolute https URL, written
                                  # to GitHub. Unset = empty, as before
        security_configuration: <name>  # optional; a key of the org's
                                  # security_configurations. Not an engine
                                  # input: reconciled by `github-structure
                                  # security-configurations` (see README).
                                  # The org's
                                  # default_security_configuration fills it
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
        workflow_only_tags:       # tags only the repo's own release
          - deploy/pulumi/v*      # workflow may write (see below)
        branch_rulesets: [...]
        environments:             # deployment environments (see below)
          production:
            branches: [main]
            tags: ["v*"]
            reviewer_teams: [team-release]
            prevent_self_review: true
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

## Written once at the org, landed on the rows at load

Four things an estate would otherwise write per row are written once at
the org. Each resolves **at load** into exactly the rows it replaces, so
resolution, entitlements, drift and preflight see one effective row —
the structs they always saw — and an estate that adopts any of them
changes nothing it applies. The resolved state before and after is the
proof, and it still shows every grant, waiver and ruleset per repository.

**`default_access`** is the grant nearly every repository gets:

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
a decision rather than a forgotten key.

**A named tag ruleset** is a body under the org's `tag_rulesets:`, keyed
by the ruleset's display name; a row's `tag_rulesets:` list then names
it. The key *is* the name: a row referencing `release-tags` renders a
ruleset called `release-tags`, which matters because the engine keys the
live resource on repo + name, so renaming is a replacement with an
unprotected window. A row may still write a one-off body — even under a
name the org defines, when its rule deliberately differs, since a forced
rename would replace the live ruleset. What it may not write is a COPY:
a body identical to the org's under the same name is refused, with the
reference as the fix.

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

**Workflow-only tags** are a row's `workflow_only_tags:` — relative tag
names (`deploy/pulumi/v*`, rendered as `refs/tags/deploy/pulumi/v*`) that
only the repository's own release workflow may create, update or delete.
It renders ONE tag ruleset named `workflow-only-tags` (creation, update
and deletion, active) whose only bypass actor is the built-in GitHub
Actions integration (`Integration`, App id 15368, `bypass_mode: always`).
No team, no organization admin.

```yaml
      service:
        preset: public
        workflow_only_tags: [deploy/pulumi/v*, sdk/v*]
```

- **The workflow must tag with `GITHUB_TOKEN`.** That token is the
  integration's identity. A token minted for another App, or a personal
  access token, is refused by this ruleset — by design.
- The id is a pinned constant (`registry.GitHubActionsAppID`), not a
  `bypass_apps` slug: that field resolves against the org's
  installations, and the built-in integration is not one. Numeric ids
  stay refused in `bypass_apps`.
- The ruleset is exempt from the at-least-one-bypass-team rule that
  `tag_rulesets` rows answer to. That rule exists so a ruleset is never
  one nobody can bypass; the release workflow can always bypass this
  one, and a team here would be a hole in the very thing it is for.
- Refused at load: an empty list; duplicates; a pattern that could match
  a root release tag (`v*`, `*`, `**`, `v1.*`, …), so a manual signed
  `v1.2.3` tag stays possible; a pattern that overlaps the include
  pattern of any of the repository's `tag_rulesets` (root `v*` rulesets
  keep their own team-bypass shape); a `tag_rulesets` row named
  `workflow-only-tags`; any of it on an archived repository. Patterns
  take literal text, `?`, `*` (not across `/`) and `**`.
- The drift check expects the ruleset to exist and its bypass set to be
  exactly the integration.

**Environments** are a row's `environments:`, a map from the name a
workflow writes after `environment:` to what gates a job that names it:

```yaml
      service:
        preset: private
        environments:
          dev: {}                       # any ref may deploy, nobody reviews
          stage: {branches: [main], tags: ["v*"]}
          production:
            branches: [main]
            tags: ["v*"]
            reviewer_teams: [team-release]   # org team slugs, at most six
            prevent_self_review: true
```

- `branches` and `tags` are the deployment policy: patterns relative to
  `refs/heads/` and `refs/tags/` (`main`, `release/*`, `v*`; no `refs/`
  prefix). Both empty means any ref may deploy. Each pattern renders one
  `RepositoryEnvironmentDeploymentPolicy`; the environment then allows
  custom policies only.
- `reviewer_teams` are team slugs of the org, each resolved to its numeric
  id, who must approve a job before it runs (one of them is enough).
  Reviewers need at least read access to the repository.
  `prevent_self_review` stops the person who triggered a job from approving
  it and needs a reviewer to mean anything.
- The name is exact: letters, digits, `.`, `-`, `_`. A cloud trust can pin
  it, so no space, slash or wildcard is allowed. Patterns of `*` or `**`
  are refused: they allow every ref, which is the same as no policy.
- **Import, never recreate.** An environment that is live is imported
  (`repo:environment`), a live policy of the same kind and pattern is
  imported (`repo:environment:id`); anything else is created. Reading them
  needs the engine App's repository permission **Environments: read and
  write**; without it the run stops and says so, and a repository that
  declares no environments never asks.
- **Retained on delete.** Dropping an environment or a pattern from the row
  stops the engine managing it and deletes nothing: deleting an
  environment deletes its secrets and variables with it, which this stack
  never held. An environment the row does not name is left alone (the
  engine adds and adopts, it does not enumerate), so retiring one is a
  manual deletion after its last workflow has moved.
- Not compared by the drift check: a policy added or an environment edited
  by hand shows up in the next preview, not in `drift`.
- Refused at load: a name that is not exact; an empty, duplicate,
  whitespace or `refs/`-qualified pattern; a reviewer team the org does not
  have, listed twice, or more than six; `prevent_self_review` without a
  reviewer; any of it on an archived repository.

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
- The one exception: under `review: none`, a hand-written
  `branch_rulesets` row may require approvals when it names
  `bypass_apps`. That is the shape for a default branch that an App
  writes by direct push (gitops' Kargo promotion App): there is no pull
  request to approve, and the `required` gate carries no App bypass.
  Without `bypass_apps` the row must say `review: required` instead.
- It lives on the repository ROW, never in `overrides:`.
- `review_dismiss_stale: true` (row, or profile with the row winning)
  adds `dismiss_stale_reviews_on_push` to the rendered ruleset: pushing
  new commits to a pull request dismisses its approvals. Unset renders
  nothing extra. It is its own key rather than
  `protection.dismiss_stale_reviews`, which is classic protection's
  approval block — the very block `review` zeroes — so reusing it would
  turn dismissal on for any registry that carries it there today. The
  loader refuses it without `review: required`, in `overrides:` and on
  an archived repository, and `github-drift` compares the live flag both
  ways. A hand-written `branch_rulesets` row takes the same setting as
  `dismiss_stale_reviews_on_push: true`.
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
- **Ruleset bypass actors** (`bypass_apps`, `bypass_org_admins`,
  `bypass_teams`) permit
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

  A branch ruleset also takes `bypass_teams`: org team slugs whose
  members merge a pull request past the ruleset (its approvals and its
  checks) without being organization admins, for a standing function
  that owns the repository. Teams render in bypass mode `pull_request`,
  not `always`: the bypass is the merge of a pull request anyone can
  read, and a direct push to the branch stays refused for the team. A
  slug the org does not declare is a LOAD error.
  A branch ruleset can also be declared with NO bypass, deliberately:
  `no_bypass: true`. That is the shape for a branch classic protection
  cannot carry (it covers the default branch only), such as a release
  line, and it is refused beside any `bypass_*` field. A row that names
  neither is still an error — a forgotten bypass is more often a mistake
  than a decision. Beside it, a branch ruleset takes the rules that need
  no actor, each rendered only when set: `block_deletion` (the `deletion`
  rule), `block_force_push` (`non_fast_forward`), `require_pull_request`
  (the `pull_request` rule with no approvals — `required_approvals: 0`
  alone never emits it, so a checks-only ruleset can still be
  direct-pushed), `allowed_merge_methods` (`merge`, `squash`, `rebase`;
  needs the pull-request rule) and `strict_checks` (the branch must be up
  to date before a pull request merges; needs `required_checks`).
  `require_code_owner_review: true` requires an approval from a code
  owner (the repository's CODEOWNERS file) for the paths a pull request
  changes. It is a setting of the pull-request rule too, so it needs
  `required_approvals` or `require_pull_request`, and like every rule of
  a ruleset its bypass actors skip it: a code-owner gate meant to bind
  organization admins names an App bypass and no `bypass_org_admins`.
  Classic protection's `protection.require_code_owner_reviews` is a
  different key on the other mechanism, and `review` zeroes it.
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
repos (found twice: two repos 2026-08-25, seven more
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

## Layering a company's rows over the provider's registry

The provider's `github.yaml` stays the file `registry.Load` reads, and
nothing below changes how it loads. When a company has to own rows inside
an org the provider manages, each side keeps one writer per row:
`registry.Compose(provider fs.FS, fragments ...Fragment)` lays one
fragment per org over the provider's file, and the merged result goes
through the same validation as a `Load`. Without fragments the result is
exactly what `Load` returns.

**The ceiling.** An org opts in with a `fragment_ceiling:` beside its other
keys. An org without one accepts no fragment.

```yaml
orgs:
  acme:
    fragment_ceiling:
      presets: [private]          # the profiles a fragment's rows may name
      access: [devs]              # the access bundles they may name
      bypass_apps: [release-bot]  # the Apps their rulesets may bypass
      max_team_permission: push   # the strongest grant to a team (required)
      public_repos: false         # whether a row may be public (default false)
      reserved:                   # names kept even where nothing is declared yet
        repos: [infra]
        teams: [platform]
        variables: [RUNNER_LABELS]
```

**What a fragment may carry.** `default_access`, `settings`,
`team_defaults`, `tag_rulesets`, `checks_waived`, `teams`, `repos` and
`variables` (merged into the org's `settings.actions.variables`). The org
singletons (`settings`, `default_access`, `team_defaults`) only where the
provider has not set them. It may not carry `engine_credentials`,
`owners`, `runner_groups`, `security_configurations`,
`default_security_configuration` or `company`; each is refused by name. A
fragment file is the same strict YAML with an `org:` key
(`registry.ParseFragment(owner, data)`).

**What is refused.** A repository, team, variable or named tag ruleset
the provider declares or reserves, compared without regard to case as
GitHub does (`Infra` is the provider's `infra`), and two rows of one kind
in the merged result that differ only by case; a preset, access bundle or bypass App
outside the ceiling; a team permission above the maximum; a public
repository where `public_repos` is false (through the preset or through an
override); a team parent that is not a team of the fragment (a parent
passes access down, and across owners that would lend it); a waiver for a
repository the fragment does not own; a second fragment for the same org; a
fragment for an org the registry lacks or one with no ceiling. Every
violation is named with its key path, all at once:

```text
org "acme": fragment owner "globex": repos.gadget: provider-owned (reserved); repos.gadget.tag_rulesets[0].bypass_apps[0] "x" outside ceiling
```

A refusal returns no config, so a caller keeps its last good pin.

**Outside the ceiling: bypass teams and org-admin bypass.** The ceiling
bounds bypass Apps. A ruleset's `bypass_teams`, `bypass_org_admins` and
`protection.pull_request_bypassers` are not held to the fragment's own
teams (a `bypass_teams` entry only has to be a team of the merged org, as
in `Load`). They widen who may merge on the fragment's own repositories,
not on the provider's. A provider that wants
them limited to the fragment's own teams adds that to the ceiling.

**Who wrote a row.** `Repo`, `Team` and org-variable rows carry a
read-only `Owner`: empty for the provider's own rows, the fragment's owner
label for the rest. It changes nothing downstream: the resolved row, the
resource names and the URNs are the ones the same rows give written inline.

**Validating before the provider does.** `Config.Boundary(org)` exports
the ceiling and the reserved names (the explicit ones together with every
name the provider declares) as JSON. It carries names only: no credentials
path, no owner, no variable value. A company decodes it and runs
`registry.ValidateFragment(boundary, fragment)`, which is the check
`Compose` runs, so the refusal is the same words in both places.

**Doing it from a command line.** Neither side has to write Go for this.
The provider's tool (`cli.Run`) gains `boundary`, which prints an org's
boundary as JSON, to be published wherever the company's CI can read it:

```sh
githubctl --org acme boundary > boundary.json
```

A company's CI then holds its fragment file to that file with the
`github-structure` command, which needs no registry, no token and no
network:

```sh
github-structure validate-fragment --owner globex --boundary boundary.json github-fragment.yaml
```

It prints `org "acme": fragment owner "globex": ok` and exits 0, or names
every violation with its key path and exits 1, in the words `Compose` uses.
`--owner` is the label of whoever wrote the file (it is not a YAML key). The
boundary file is read strictly: a file that is not the output of `boundary`
is refused rather than half-read. Where the tool is configured with the
registry itself, `validate-fragment` without `--boundary` uses the
fragment's own org from it. The boundary is a snapshot: validate against the
current one, since a later `Compose` holds the fragment to the registry as
it is then.
