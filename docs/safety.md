# The safety model — every guard, and the incident that earned it

This library's posture: **the engine must not be able to do the worst
thing by accident.** Pulumi will cheerfully plan a replace that destroys
state it does not know exists; GitHub will cheerfully accept it. The
guards below run as a preflight over `pulumi preview` and refuse the
plan before it reaches an apply. Each exists because something happened.

## Guard: no replaces of irreplaceable resources

Some resource types hold real state OUTSIDE the stack:

- **Teams.** Membership lives in whatever system fills teams — not in
  this stack. A replace deletes the team and creates a new one with the
  same slug; every diff afterwards reads clean while the members are
  simply gone.
- **Branch protections / rulesets.** A replace leaves a window with no
  protection at all, and the follow-up diff is clean.

The preflight scans the preview's operation list for replace-shaped ops
(`replace`, `create-replacement`, `delete-replaced`, …) on these types
and refuses the whole deploy.

*The incident:* on 2026-08-07 the source estate's engine emptied all 12
teams of one organization and broke CI org-wide for four days.
`protect: true` was set on every one of them and did not stop it —
Pulumi's protect stops DELETES, not REPLACES, and a replace is a delete
wearing a create's clothes. The preflight is the fix; protect stays as
defence-in-depth.

## Guard: stale waivers fail the deploy

`checks_waived:` says "this repo's CI hasn't landed yet". The preflight
probes the repository's default branch for check runs; once the waived
context actually reports, the waiver has outlived its cause and the
deploy refuses to run until the waiver is deleted. Exceptions may
exist; they may not be forgotten.

*Wrinkle that made this subtle:* a repository that does not exist yet
answers 404 to the probe. The first engine-created repository was
blocked by its own waiver — the guard read 404 as failure instead of
as "no commits, no checks". 404 is information.

## Guard: no unfillable team left required

A team whose membership source cannot fill it (no roster mapping, no
sync) but which holds grants is a standing outage waiting for its first
removal. The preflight refuses to leave such a team empty-but-granted.

## Guard: imported IDs must match

Every imported resource's ID is compared against state before an apply.
An ID mismatch means the state and the live object have diverged in a
way an apply would "fix" destructively.

## Import-first adoption — and the check-then-import trap

The engine ADOPTS what exists: when live state shows a resource, it is
imported rather than created, so the first preview over an existing
estate is a zero diff and nothing is ever recreated for being unknown.

One exception, learned the hard way: **branch protection is never
check-then-imported.** Import is safe only for a resource the stack has
never managed. Once the stack CREATES a rule, the next run finds it
live, flips to the import branch, and asks to import a resource already
in state without an import ID — which Pulumi satisfies by REPLACING it.
The delete half removes the branch's protection, and the following diff
reads clean. On 2026-07-29 two repositories in the source estate were
found with no protection at all while the stack reported no drift.

## The create path is the untested path

Every resource in an adopted estate was imported, so the create branches
barely run. The source estate's log so far:

1. A new repo's own waiver blocked its creation (the 404-probe bug
   above).
2. The create half-failed on a field GitHub refuses for that org state
   (`allow_forking` on a private-forking-disabled org) and wedged the
   stack: the repo existed while its resource erred, so every later run
   re-planned the same doomed update. Fields GitHub owns in a given
   state are excluded from writes (`repoIgnoredFields`), with tests
   recording why each entry is there.
3. The Actions-settings resources took the repository as a plain string
   — no dependency edge — and raced their repository's creation;
   creating three repositories at once, one race was lost (PUT answered
   404 seconds after the create returned). Both now take the
   repository resource's own output, so the edge exists.

Expect a fourth. When an apply half-fails: check whether the resource
exists live while erroring in state — that combination is a wedge, and
it needs the declaration fixed, not a retry.

## Drift checks: the questions a plan cannot answer

A Pulumi plan compares registry to state. Two failure modes live outside
that comparison, so `pkg/app` probes GitHub directly:

- **Undeclared active repositories.** A repo created in GitHub and never
  added to the registry is managed by nobody — and no count assertion
  can notice it, because the missing row was never counted. (How the
  source estate shipped a public repo that sat on the wrong auto-merge
  setting for days, with nothing red anywhere.)
- **Undeclared owners.** Owner is the widest grant GitHub has; the
  engine can only promote, so an owner added out-of-band is visible in
  no plan. The drift check reports them and a human decides.

## Operational rules that pair with the guards

- **Deploy only through a preflight-gated command** (make the preflight
  a hard dependency of your deploy recipe, so a refused plan never
  reaches Pulumi).
- **After any apply, validate with `pulumi preview --refresh`**: state
  is re-read from GitHub, and anything accidentally destroyed shows up
  as drift. Zero is the only acceptable answer.
- **If an apply half-fails, do not re-run and hope** (see above).
- **Never apply a refresh plan you have not read.** `up --refresh`
  reconciles state to live — including hand-added things the registry
  does not know. On 2026-08-26 an unread refresh plan would have
  removed a hand-added App bypass from a tag ruleset and silently
  broken a weekly auto-release. Read the diff; if it wants to remove
  something live, the fix is usually to DECLARE it, not to apply.
- **Interrupted runs leave a stale state lock.** Ctrl+C kills Pulumi
  before it releases the backend lock; every later command then fails
  with "the stack is currently locked". Recover with
  `pulumi cancel --stack <s> --yes` — run it from the Pulumi project
  directory, or it cannot even find its backend.
- **Out-of-band protection edits invalidate state node IDs.** Deleting
  and recreating a branch protection rule via the REST API gives it a
  new GraphQL node id; the next apply fails with "Could not resolve to
  a node…". Recover with a targeted
  `pulumi refresh --target <protection-urn>`; if the follow-up create
  then hits "Name already protected: <branch>", delete the live rule
  once and let the engine recreate it as declared.
- **Refresh passes take minutes, not seconds** — one GitHub API read
  per resource. A silent multi-minute `--refresh` is working, not hung;
  a frozen single connection beyond ~5 minutes is wedged and safe to
  SIGINT (which releases the lock cleanly).
- **A run stuck "queued" with zero jobs has two known causes — check
  githubstatus before debugging your own YAML.** (1) An invalid
  reusable-workflow interface, e.g. a `workflow_call` secret name with
  hyphens (secret names are `[A-Z_][A-Z0-9_]*`): GitHub accepts the
  dispatch and then silently never expands the callee. (2) A GitHub
  Actions outage: on 2026-08-26 a database-primary failure left every
  run estate-wide planned-but-unassigned for an hour, indistinguishable
  from cause 1 except that untouched workflows queue too.
