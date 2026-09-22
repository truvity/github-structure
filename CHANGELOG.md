# Changelog

Notable changes to this library. The release itself is the git tag (Go
module versioning); this file is the prose a consumer needs before
taking a new one.

## Unreleased

### Added

- **Said once: named tag rulesets, waivers by reason, team defaults.**
  Three more places where an estate wrote one decision many times.

  - `tag_rulesets:` at the **org** level is a map of ruleset bodies
    keyed by display name; a row's `tag_rulesets:` list may name one
    instead of carrying a body (`tag_rulesets: [release-tags]`). A row
    may not write a body under a name the org defines. The key is the
    name, on purpose: the engine keys the live ruleset on repo + name,
    so this is a reference, never a rename.
  - `checks_waived:` at the **org** level maps a reason to the
    repositories it covers. Same fact as the row field, landed on each
    row at load; a repository named twice (row and list, or two
    reasons) is refused.
  - `team_defaults:` on an org fills `privacy` and `notifications` where
    a team row leaves them unsaid.

  All three resolve at load into the rows they replace: `Org.Repos[…]`,
  `Org.Teams[…]` and `Org.ChecksWaived()` read exactly as before, and an
  estate that adopts them produces a byte-identical resolved state.
  Nothing to do on upgrade; nothing changes for a file that uses none of
  them.

  One behavioural note for Go callers: a `TagRuleset` decoded from YAML
  now goes through `UnmarshalYAML` (a bare string is a reference), and
  `Validate()` on a hand-built `Config` refuses an unresolved reference —
  `Load` is what resolves them.

### Changed

- Documentation only: `docs/doctrine.md` described the model two
  releases ago — "a profile must set every field" and an approval gate
  spelled as a `required_approvals: 0` override, both retired by 0.9.0
  and 0.10.0. It now describes presets as diffs and `review:`, and says
  each thing in one place: the *why* in doctrine, the schema in
  registry, the guards and their incidents in safety.

## 0.10.0 — 2026-09-21

### Added

- **A preset is a diff against GitHub's defaults.** A preset used to have
  to state all 33 settings fields; it now states only what differs from
  what GitHub creates a repository with. `private` goes from 33 lines to
  about ten, and reads as what is true of the estate rather than as a
  transcription.

  The property the old rule protected is kept, and by the same means:
  resolution is base → preset → overrides, so every field is still set
  and still applied, and nothing is left unmanaged. The base is a
  CONSTANT in the library, not a live lookup — if GitHub changes a
  default, nothing an estate applies moves.

  Existing presets are unaffected: a complete preset resolves exactly as
  before. Nothing to do on upgrade.

- **`default_access` on an organization.** The access bundles every
  repository in that org gets unless its row decides for itself. For an
  estate that grants the same teams nearly everywhere, this is one
  decision instead of one copy per row.

  Silence inherits; an explicit `access: []` on a row opts out. Applied
  at load, so resolution, entitlements and drift see one effective row,
  and the resolved state still shows every grant per repository. A
  `default_access` naming an undeclared bundle fails the load even if no
  row inherits it today.

## 0.9.0 — 2026-09-21

### Changed

- **BREAKING (registry schema).** The `apps:` block is gone, and
  `app_prefix:` with it. A ruleset's `bypass_apps` still takes App
  names — now the App's **slug**, resolved against the Apps INSTALLED
  on the organization (`GET /orgs/{org}/installations`) when the engine
  deploys, instead of against a row in this file.

  Why: the block's one load-bearing job was turning a name into a
  database id, and the id is GitHub's fact about an App, not the
  registry's. Held in a file it was hand-copied, unreadable at the
  point of use, and stale the moment an App was recreated. The rest of
  the block was inventory: an App's existence, its key, its
  installation and its permissions belong to whoever holds that key,
  and a second description of them here could only disagree.

  Migrating: delete `apps:` and `app_prefix:` from every org. Leave
  every `bypass_apps:` list exactly as it is — the names already match
  the App slugs, and nothing a ruleset renders changes. A file that
  still carries either key **fails to load**, naming the org: a retired
  block that loaded silently would read as "still managed here".

  The engine App needs `organization_administration: read` on any
  organization whose rulesets name a bypass App. It is asked for
  nowhere else — an estate whose rules name none never needs it.

  Go callers: `Org.BypassAppIDs` becomes
  `registry.InstalledApps.BypassAppIDs` (`InstalledApps` is
  `map[slug]id`). `app.InstalledAppIDs(ctx, client, org)` reads that
  map as the App; `CheckBypassSurfaces` reads it through `gh` for
  itself. Removed: `registry.App`, `Org.Apps`, `Org.AppPrefix`,
  `Org.SortedApps`, `Org.OwnedApps`, `Config.AppPrefixFor`,
  `registry.InstallAll`/`InstallSelected`, and `app.CheckApps` (App
  rows to compare against no longer exist — the other drift checks are
  unchanged).

- **BREAKING (registry schema).** `bypass_apps`, on both
  `tag_rulesets` and `branch_rulesets`, now takes GitHub App **names**
  — keys of the same organization's `apps` map — instead of App
  database ids. The loader reads each id back out of the App's own row,
  with no API call, so what the engine renders is unchanged for every
  ruleset whose named App is the one the id pointed at.

  Why: an App's database id is a fact the registry already states once,
  on the App's row. Repeated inside a ruleset it was hand-looked-up,
  unreadable at the point of use, and stale the moment an App was
  recreated — and a stale bypass actor announces itself only by
  refusing a release act.

  Migrating: replace each id with the name of the `apps` row that
  carries it, and add a row for any App that has none (an App held
  outside this registry belongs in an `external: true` row — it is
  inventory either way). Ids are not accepted during a transition: a
  field that takes both keeps the habit alive. A numeric entry is
  rejected with a message saying so.

  Go callers: see the entry above — the resolver moved from
  `Org.BypassAppIDs` to `registry.InstalledApps.BypassAppIDs` in the
  same release, and `apps` rows are no longer where a name resolves.

### Added

- A bypass App name that does not resolve is refused, never dropped.
  The spelling half runs at **load**: an all-digits name is rejected as
  the field's old id spelling. The existence half runs at **deploy**,
  where live GitHub can answer it — a name with no installation on the
  organization stops the deploy and names the App. Neither was
  expressible before, and their absence is what let a bypass list drift
  out of date in silence: the gate stays, the actor goes, and nothing
  says so until the release act it permits is refused.
