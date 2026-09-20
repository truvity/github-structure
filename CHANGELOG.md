# Changelog

Notable changes to this library. The release itself is the git tag (Go
module versioning); this file is the prose a consumer needs before
taking a new one.

## Unreleased

### Changed

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

  Go callers: `Org.BypassAppIDs(names []string) ([]int, error)` is the
  resolver. `deployTagRulesets`, `deployBranchRulesets` and the drift
  check now take the `*registry.Org` they resolve against.

### Added

- An unresolvable bypass App name is a **load error** naming the
  organization, repository and ruleset. It was previously impossible to
  express, and its absence is what let a bypass list drift out of date
  in silence.
