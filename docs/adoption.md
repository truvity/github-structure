# Adopting an organization

The engine is designed to take over estates that already exist. The
order below is the one that worked twice in the source estate — once for
a 60-repo organization, once for its sister org.

## 0. Create the engine's App — one browser click

The engine authenticates as a GitHub App, never a PAT: an apply is then
attributable to the engine, not to whoever last ran it. GitHub has no
API to create an App; `pkg/app`'s manifest flow automates everything
around the one thing it cannot:

1. The manifest (name, permissions, webhook) renders from the App's
   registry row.
2. A local HTTP listener serves the one-click creation redirect; you
   click once in the browser.
3. GitHub returns a code; the flow exchanges it for the App's
   credentials and hands them back to the CALLER. Where they live —
   1Password, SSM, SOPS — is your estate's decision; the library never
   stores them.

Install the App on the organization (one more click; GitHub has no API
for that either), record the installation ID, and put all three values
where your deploy can read them (`credentials_ssm_prefix` in the
registry names that place).

App permissions the engine needs: Administration (org+repo, RW),
Members (RW), Actions/Workflows metadata (R), plus whatever your drift
checks probe. Keep one App per organization: display names are globally
unique, so prefix them (`app_prefix`, default `{org}-`).

## 1. Snapshot, then declare exactly what is live

Write the registry to reproduce live GitHub EXACTLY — every setting
captured as it is, deviations recorded as per-repo `overrides` with a
`reason:` that names the keep-or-fix decision. Resist fixing anything in
this pass. The exit test: the first `pulumi preview` after import is a
**zero diff**. Only then do you know the registry describes reality, and
only then are later diffs meaningful.

## 2. Let the engine import

Run the deploy. Live resources are imported (adopted), not created;
branch protection deliberately skips check-then-import (docs/safety.md).
Validate with `pulumi preview --refresh` — zero drift.

## 3. Normalise deliberately, one decision at a time

Now the interesting work: each override either becomes a profile change
(everyone gets it), gets a better reason, or is deleted (the repo joins
its profile). Every normalisation is a small reviewed diff. The source
estate's registry file reads as a decision log because of this phase —
that is a feature, keep the comments.

## 4. A second organization is a key, not a codebase

`orgs:` is a map. The second org references the same profiles; its own
App; its own credential prefix. Everything estate-specific stays in the
file.

## Day-1 for a NEW repository (created by the engine)

1. Add the row: profile + `checks_waived` with the standard exit text
   (a new repo has no CI; without the waiver its required `check` blocks
   every PR forever — and with no waiver the repo cannot even be
   created, because preflight would refuse the plan).
2. Deploy. The engine creates repo + Actions settings + protection.
3. Push the initial content (no protection obstacle: `required_approvals: 0`
   profiles allow direct pushes until required checks exist).
4. Once CI reports `check` on master, DELETE the waiver — the stale-waiver
   guard will force you to anyway — and deploy again.
