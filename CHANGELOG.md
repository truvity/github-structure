# Changelog

Notable changes to this library. The release itself is the git tag (Go
module versioning); this file is the prose a consumer needs before
taking a new one.

## Unreleased

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
