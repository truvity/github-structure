# The registry file, field by field

`registry.Load(fsys)` reads `github.yaml` from the root of the given
`fs.FS` (an `embed.FS` in the consuming estate) and rejects unknown keys
— a typo is an error, never a silently-ignored setting.

```yaml
profiles:            # shared, top-level — the whole point
  <profile-name>:    # e.g. public, private
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
             bypass_apps: […]}     # App DATABASE ids (auto-release bots)
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
