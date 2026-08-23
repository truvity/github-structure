// Package engine is the Pulumi half of the GitHub structure engine: one
// stack per organization, every resource driven by a row in the
// registry (pkg/registry).
//
// Scope discipline — this program owns STRUCTURE:
//
//   - org settings, teams (existence, nesting, privacy);
//   - repository settings, branch protection, Actions permissions;
//   - team→repository permissions.
//
// It does NOT own team MEMBERSHIP: that belongs to the roster service
// (INF-484/INF-487). Adding membership resources here would put two
// systems in a reconciliation fight over the same objects.
//
// It also does not own App creation, installation, or App permissions —
// GitHub has no API for those (execution plan §4.1). Those live in
// cmd/githubctl and docs/operations/github-apps-day1.md.
//
// # Import, never recreate
//
// Every resource here already exists on the Truvity org. Each is adopted
// via check-then-import: [readLiveState] asks GitHub what is there, and
// each resource is imported when live and created when not. So the first
// preview is a ZERO DIFF against reality, and a destroy/recreate of a
// live repository — the failure mode this program exists to prevent —
// cannot happen by accident. The same code stamps a fresh org (INF-473)
// with no flags to flip.
//
// # Branch protection is the exception, and why
//
// [deployProtection] does NOT import. Check-then-import is only sound for
// a resource this stack has never managed: once Pulumi CREATES one, the
// next run finds it live, takes the import branch, and asks to import a
// resource already in state with no importID — which Pulumi satisfies by
// REPLACING it. For branch protection the delete half of that replace
// leaves the branch unprotected, and the next diff reads clean, so
// nothing surfaces it.
//
// Every rule the engine creates therefore armed a bomb on its own next
// run. On 2026-07-29 both `gitops` and `github-roster` were found with no
// protection at all while the stack reported no drift; the same mechanism
// left a permanently stuck pending-delete on `vuln-alerts-github-roster`,
// whose DELETE the org's enforced security configuration refuses forever.
//
// Adopting an organization whose rules pre-date the engine is now a
// one-off `pulumi import` — which is the right home for a one-off.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"

	"github.com/pulumi/pulumi-github/sdk/v6/go/github"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	app "github.com/truvity/github-structure/pkg/app"
	registry "github.com/truvity/github-structure/pkg/registry"
)

const (
	// fieldAutoInit is only meaningful at creation, so it is always ignored.
	// A constant because repoIgnoredFields and its tests both name it, and
	// a typo in either would silently stop ignoring it.
	fieldAutoInit = "autoInit"
)

// Deploy provisions one organization's structure. The stack name IS the
// org login, so a second org (INF-473) is a new stack file plus a
// cfg/github.yaml key — never new code.
// Deploy declares one organization's whole structure — settings, Actions
// policy, owners, teams, repositories with their protection and rulesets
// — from the registry, against live state read through the App client.
//
// The caller supplies the FACTS: which org, the loaded registry, and the
// engine App's credentials (wherever the estate keeps them — SSM, SOPS,
// a secret manager). The engine is the MECHANISM and holds no opinion on
// any of the three.
func Deploy(c *pulumi.Context, logger *slog.Logger, org string, reg *registry.Config, creds Credentials) error {
	logger = logger.With(slog.String("org", org))

	orgCfg, ok := reg.Orgs[org]
	if !ok {
		return fmt.Errorf("org %q not in the registry (known: %v)", org, reg.SortedOrgs())
	}

	provider, err := github.NewProvider(c, "github-"+org, &github.ProviderArgs{
		Owner: pulumi.String(org),
		AppAuth: &github.ProviderAppAuthArgs{
			Id:             pulumi.String(creds.AppID),
			InstallationId: pulumi.String(creds.InstallationID),
			PemFile:        pulumi.String(creds.PrivateKey),
		},
		// GitHub's own guidance for github.com: serialize writes rather
		// than risk the abuse rate limiter.
		ParallelRequests: pulumi.Bool(false),
	})
	if err != nil {
		return fmt.Errorf("create github provider: %w", err)
	}

	client, err := creds.Client(c.Context())
	if err != nil {
		return err
	}

	live, err := readLiveState(c.Context(), client, org, orgCfg)
	if err != nil {
		return fmt.Errorf("read live state: %w", err)
	}

	if err := deployOrgSettings(c, org, orgCfg, live, provider); err != nil {
		return fmt.Errorf("org settings: %w", err)
	}

	if err := deployOrgActions(c, org, orgCfg, provider); err != nil {
		return fmt.Errorf("org actions: %w", err)
	}

	if err := deployOwners(c, org, orgCfg, provider); err != nil {
		return fmt.Errorf("org owners: %w", err)
	}

	teams, err := deployTeams(c, client, org, orgCfg, live, provider)
	if err != nil {
		return err
	}

	if err := deployRepos(c, org, reg, orgCfg, teams, live, provider); err != nil {
		return err
	}

	logger.InfoContext(c.Context(), "github structure declared",
		slog.Int("teams", len(orgCfg.Teams)),
		slog.Int("repos", len(orgCfg.Repos)),
	)

	return nil
}

type (
	// Credentials are the structure engine App's. The App is the only
	// identity the engine uses — no human PAT — so an apply is
	// attributable to the engine, not to whoever last ran it. WHERE they
	// live (SSM, SOPS, a secret manager) is the caller's estate.
	Credentials struct {
		AppID          string
		InstallationID string
		PrivateKey     string
	}
)

// Client builds a REST client from the same credentials the provider
// uses, for the check-then-import preflight.
func (c *Credentials) Client(ctx context.Context) (*app.Client, error) {
	appID, err := strconv.ParseInt(c.AppID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("app id %q is not a number: %w", c.AppID, err)
	}

	installationID, err := strconv.ParseInt(c.InstallationID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("installation id %q is not a number: %w", c.InstallationID, err)
	}

	return app.NewClient(ctx, appID, []byte(c.PrivateKey), installationID)
}

func deployOrgSettings(c *pulumi.Context, org string, orgCfg *registry.Org, live *liveState, provider *github.Provider) error {
	s := orgCfg.Settings

	// Billing belongs to the IT team, managed in the UI — the engine
	// passes the org's live value through because the provider requires
	// the field, and ignores it thereafter (decided 2026-07-31).
	if live.billingEmail == "" {
		return fmt.Errorf("org %q has no billing email set; billing is IT-managed — set it in the GitHub UI first", org)
	}

	args := &github.OrganizationSettingsArgs{
		BillingEmail:                pulumi.String(live.billingEmail),
		DefaultRepositoryPermission: pulumi.String(s.DefaultRepositoryPermission),

		MembersCanCreateRepositories:         pulumi.Bool(s.MembersCanCreateRepositories),
		MembersCanCreatePublicRepositories:   pulumi.Bool(s.MembersCanCreatePublicRepositories),
		MembersCanCreatePrivateRepositories:  pulumi.Bool(s.MembersCanCreatePrivateRepositories),
		MembersCanCreateInternalRepositories: pulumi.Bool(s.MembersCanCreateInternalRepositories),
		MembersCanCreatePages:                pulumi.Bool(s.MembersCanCreatePages),
		MembersCanCreatePublicPages:          pulumi.Bool(s.MembersCanCreatePublicPages),
		MembersCanCreatePrivatePages:         pulumi.Bool(s.MembersCanCreatePrivatePages),
		MembersCanForkPrivateRepositories:    pulumi.Bool(s.MembersCanForkPrivateRepositories),

		WebCommitSignoffRequired: pulumi.Bool(s.WebCommitSignoffRequired),
		HasOrganizationProjects:  pulumi.Bool(s.HasOrganizationProjects),
		HasRepositoryProjects:    pulumi.Bool(s.HasRepositoryProjects),

		AdvancedSecurityEnabledForNewRepositories:             pulumi.Bool(s.AdvancedSecurityEnabledForNewRepos),
		DependabotAlertsEnabledForNewRepositories:             pulumi.Bool(s.DependabotAlertsEnabledForNewRepos),
		DependabotSecurityUpdatesEnabledForNewRepositories:    pulumi.Bool(s.DependabotSecurityUpdatesForNewRepos),
		DependencyGraphEnabledForNewRepositories:              pulumi.Bool(s.DependencyGraphEnabledForNewRepos),
		SecretScanningEnabledForNewRepositories:               pulumi.Bool(s.SecretScanningForNewRepos),
		SecretScanningPushProtectionEnabledForNewRepositories: pulumi.Bool(s.SecretScanningPushProtForNewRepos),
	}

	opts := []pulumi.ResourceOption{
		pulumi.Provider(provider),
		// The org always exists — a stack for a nonexistent org is a
		// typo, not a provisioning request.
		pulumi.Import(pulumi.ID(org)),
		// Never diff billing: the value sent is the live one by
		// construction, and the IT team may change it at any time.
		pulumi.IgnoreChanges([]string{"billingEmail"}),
	}

	// `name` is the org's DISPLAY name, not its URL slug. Left unset,
	// Pulumi AUTO-NAMES it and would push a generated string
	// ("org-truvity-1fd213c") onto the organization. So it is either
	// declared deliberately or ignored deliberately — never left alone.
	if s.DisplayName != "" {
		args.Name = pulumi.String(s.DisplayName)
	} else {
		opts = append(opts, pulumi.IgnoreChanges([]string{"name"}))
	}

	_, err := github.NewOrganizationSettings(c, "org-"+org, args, opts...)

	return err
}

// deployOrgActions declares the organization's Actions policy — the
// ceiling every repository policy sits beneath. Skipped when the
// registry omits it, since an org that does not declare a policy should
// not have one imposed.
func deployOrgActions(c *pulumi.Context, org string, orgCfg *registry.Org, provider *github.Provider) error {
	a := orgCfg.Settings.Actions
	if a == nil {
		return nil
	}

	if _, err := github.NewActionsOrganizationPermissions(c, "org-actions-"+org, &github.ActionsOrganizationPermissionsArgs{
		AllowedActions:      pulumi.String(a.AllowedActions),
		EnabledRepositories: pulumi.String(a.EnabledRepositories),
		ShaPinningRequired:  pulumi.Bool(a.ShaPinningRequired),
	}, pulumi.Provider(provider), pulumi.Import(pulumi.ID(org))); err != nil {
		return err
	}

	_, err := github.NewActionsOrganizationWorkflowPermissions(c, "org-workflow-perms-"+org,
		&github.ActionsOrganizationWorkflowPermissionsArgs{
			OrganizationSlug:             pulumi.String(org),
			DefaultWorkflowPermissions:   pulumi.String(a.DefaultWorkflowPermissions),
			CanApprovePullRequestReviews: pulumi.Bool(a.CanApprovePullRequests),
		}, pulumi.Provider(provider), pulumi.Import(pulumi.ID(org)))

	// Actions VARIABLES are declared in the registry but not managed here.
	// The provider (6.15.0) cannot import an ActionsOrganizationVariable —
	// preview fails outright with "provider does not support importing
	// resources" — and creating one that already exists is a 409. Handing
	// them to Pulumi would mean deleting live variables first, which is a
	// window with no CI. So they follow the SecurityConfigurations
	// precedent: declared for humans, verified by `just github-drift`,
	// enforced by nothing. See CheckOrgVariables.
	return err
}

// deployOwners asserts that each declared login holds the organization's
// `admin` role — GitHub's owner.
//
// One resource per login, deliberately: the engine may PROMOTE a login
// it is told about and may never DEMOTE one it was not. A full-set
// reconciliation would make an accidentally-empty block mean "remove
// every owner", which is the accident github-roster names explicitly in
// its own config doc and refuses to risk; nothing is gained by us taking
// the risk it declined. `githubctl drift` reports live owners missing
// from the registry, and a human decides.
//
// No Import is declared. The provider's create is a PUT of the
// membership, which is idempotent for a login that already holds the
// role, so an existing owner adopts cleanly on first apply. Declaring
// Import for already-managed resources is what replaced every team on
// 2026-08-07 — see the team option comments below.
func deployOwners(
	c *pulumi.Context,
	org string,
	orgCfg *registry.Org,
	provider *github.Provider,
) error {
	for _, owner := range orgCfg.Owners {
		_, err := github.NewMembership(c, fmt.Sprintf("owner-%s-%s", org, owner), &github.MembershipArgs{
			Username: pulumi.String(owner),
			Role:     pulumi.String("admin"),
			// Dropping a login from owners: is a DEMOTION, never an
			// eviction. Without this the provider removes the person
			// from the organization outright on destroy — deleting one
			// registry line would take a human's whole org membership,
			// their team seats with it, and read as a tidy-up in review.
			DowngradeOnDestroy: pulumi.Bool(true),
		}, pulumi.Provider(provider))
		if err != nil {
			return fmt.Errorf("owner %q: %w", owner, err)
		}
	}

	return nil
}

// deployTeams declares the team graph parents-first and returns each
// team's resource, which repository grants and child teams both need.
//
// Teams enter this state by IMPORT-ADOPTION ONLY (the 2026-07-31 rule,
// made structural): a team missing from the org is created live through
// the API at apply time and then declared with an import — never left
// to Pulumi's create path, whose lifecycle re-plans phantom REPLACEs
// forever (createDefaultMaintainer/ldapDn — see the option comments).
// Previews stay read-only: a missing team simply previews as a create,
// and the apply that follows imports instead.
func deployTeams(
	c *pulumi.Context,
	client *app.Client,
	org string,
	orgCfg *registry.Org,
	live *liveState,
	provider *github.Provider,
) (map[string]*github.Team, error) {
	teams := make(map[string]*github.Team, len(orgCfg.Teams))

	for _, slug := range topoSortTeams(orgCfg) {
		row := orgCfg.Teams[slug]

		if _, exists := live.teamID(slug); !exists && !c.DryRun() {
			if err := createTeamLive(c.Context(), client, org, slug, row, live); err != nil {
				return nil, err
			}
		}

		args := &github.TeamArgs{
			Name:    pulumi.String(teamName(slug, row)),
			Privacy: pulumi.String(row.Privacy),
			// createDefaultMaintainer is deliberately NOT set. It would
			// add the creating identity as a maintainer — a MEMBERSHIP
			// change, this program's one forbidden move — but the
			// provider defaults it to false, so omitting it gives the
			// same behavior without the deprecation warning it emits on
			// all 23 teams.
		}

		if row.Description != "" {
			args.Description = pulumi.String(row.Description)
		}

		if row.Notifications != "" {
			args.NotificationSetting = pulumi.String(notificationSetting(row.Notifications))
		}

		if row.Parent != "" {
			parent, ok := teams[row.Parent]
			if !ok {
				return nil, fmt.Errorf("team %q: parent %q not declared yet — topological sort is wrong", slug, row.Parent)
			}

			args.ParentTeamId = parent.ID().ToStringOutput()
		}

		opts := []pulumi.ResourceOption{
			pulumi.Provider(provider),
			// createDefaultMaintainer is create-only and ldapDn is
			// Enterprise-only: the GitHub API never returns either, so
			// the provider re-plans phantom REPLACEs for teams in the
			// CREATED lifecycle (imported ones are stable), and
			// ignoreChanges cannot fully suppress it (provider Check
			// re-injects defaults after ignores apply) — the 2026-07-31
			// incident. Hence createTeamLive above: every team is
			// import-adopted, and a replace plan on any team remains a
			// stop-the-line bug — applied to a populated team it
			// silently drops every member.
			pulumi.IgnoreChanges([]string{"createDefaultMaintainer", "ldapDn"}),
			// Blocks a plain DELETE, and only that. It is worth having —
			// removing a team row should take a deliberate unprotect —
			// but it is NOT the backstop this comment used to claim.
			//
			// Proven on 2026-08-07: every one of these twelve teams
			// carried protect:true in state, and an `up` applied
			// `replace: 13` anyway. Their old numeric IDs are 404 today.
			// A replace deletes the team and makes a new one with the
			// same slug; membership is the roster service's (INF-484) and
			// lives nowhere in this state, so Pulumi dropped every member,
			// could not restore them, and reported nothing lost. The
			// roster refilled the human teams within a sync — `ci-cd`,
			// which no roster refills, stayed empty and took org-wide CI
			// down for four days.
			//
			// What actually guards this is `just github-preflight`
			// (INF-530), which refuses a plan containing such a replace.
			// Run `just github-deploy`, never a bare `pulumi up`.
			pulumi.Protect(true),
		}

		// DO NOT change this import ID to the numeric one. The provider
		// accepts both, and `live.teamID(slug)` right there has the
		// number, so switching looks like an obvious tidy-up. It is not:
		// measured on 2026-08-11, it plans `+-12 to replace` — every team
		// in the org — with an empty replaceKeys.
		//
		// The reason is that the DECLARED import ID is part of the
		// resource's identity: state records `importID`, and a run that
		// asks for a different one reads as a different resource. State
		// currently holds the slug, so the slug is what keeps the plan
		// empty. This is an equilibrium, not a preference.
		//
		// It is also how the 2026-08-07 outage happened, one step
		// earlier in the same mechanism: the teams sat in state with
		// `importID: None` (pre-87dcec0e, Pulumi had created them), this
		// unconditional Import asked to adopt them anyway, and Pulumi
		// satisfied "import something already in state without an
		// importID" by REPLACING all twelve — exactly what the package
		// doc above warns about for branch protection. `protect: true`
		// was set on every one of them and did not stop it; their old
		// numeric IDs are 404 today. Membership is not in this state, so
		// every member was dropped silently.
		//
		// Before touching this line, run `just github-preflight`, which
		// refuses any plan that would replace a team (INF-530).
		if _, exists := live.teamID(slug); exists {
			opts = append(opts, pulumi.Import(pulumi.ID(slug)))
		}

		team, err := github.NewTeam(c, "team-"+slug, args, opts...)
		if err != nil {
			return nil, fmt.Errorf("team %s: %w", slug, err)
		}

		teams[slug] = team
	}

	return teams, nil
}

// createTeamLive creates a missing team through the API so the resource
// declaration right after it can import-adopt. The parent, when set, was
// processed earlier (topological order), so its ID is always in live.
func createTeamLive(
	ctx context.Context,
	client *app.Client,
	org, slug string,
	row *registry.Team,
	live *liveState,
) error {
	payload := map[string]any{
		"name":    teamName(slug, row),
		"privacy": row.Privacy,
	}

	if row.Description != "" {
		payload["description"] = row.Description
	}

	if row.Notifications != "" {
		payload["notification_setting"] = notificationSetting(row.Notifications)
	}

	if row.Parent != "" {
		parentID, ok := live.teamID(row.Parent)
		if !ok {
			return fmt.Errorf("team %q: parent %q not in live state — topological sort is wrong", slug, row.Parent)
		}

		id, err := strconv.ParseInt(parentID, 10, 64)
		if err != nil {
			return fmt.Errorf("team %q: parent id %q is not a number: %w", slug, parentID, err)
		}

		payload["parent_team_id"] = id
	}

	var created struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	}

	if err := client.Post(ctx, "/orgs/"+org+"/teams", payload, &created); err != nil {
		return fmt.Errorf("create team %q: %w", slug, err)
	}

	// The registry keys teams by slug and GitHub derives the slug from
	// the name; a mismatch here would import the wrong thing forever.
	if created.Slug != slug {
		return fmt.Errorf("created team %q got slug %q — rename the registry key or the team name", slug, created.Slug)
	}

	live.teamIDs[slug] = strconv.FormatInt(created.ID, 10)

	return nil
}

func deployRepos(
	c *pulumi.Context,
	org string,
	reg *registry.Config,
	orgCfg *registry.Org,
	teams map[string]*github.Team,
	live *liveState,
	provider *github.Provider,
) error {
	for _, name := range orgCfg.SortedRepos() {
		resolved := reg.Resolve(orgCfg.Repos[name])

		repo, err := deployRepo(c, name, resolved, live, provider)
		if err != nil {
			return fmt.Errorf("repo %s: %w", name, err)
		}

		// An archived repository is read-only: GitHub answers 403 to
		// writes on its Actions permissions, workflow permissions and
		// branch protection. Declaring those resources would put a
		// failing API call into every future run — the wedge described
		// in docs/operations/github-repo-archival.md — so an archived
		// row gets the repository and nothing else.
		//
		// Team grants stay: collaborator access is managed through the
		// org's team endpoints, which archiving does not close, and who
		// can read a retired repo is still a decision worth owning.
		if !resolved.Archived {
			if err := deployRepoActions(c, name, resolved, repo, live, provider); err != nil {
				return fmt.Errorf("repo %s actions: %w", name, err)
			}

			if err := deployProtection(c, name, resolved, repo, provider); err != nil {
				return fmt.Errorf("repo %s protection: %w", name, err)
			}

			if err := deployTagRulesets(c, name, orgCfg.Repos[name].TagRulesets, teams, provider); err != nil {
				return fmt.Errorf("repo %s tag rulesets: %w", name, err)
			}

			if err := deployBranchRulesets(c, name, orgCfg.Repos[name].BranchRulesets, provider); err != nil {
				return fmt.Errorf("repo %s branch rulesets: %w", name, err)
			}
		}

		if err := deployTeamGrants(c, name, resolved, teams, live, provider); err != nil {
			return fmt.Errorf("repo %s team grants: %w", name, err)
		}
	}

	_ = org

	return nil
}

// repoIgnoredFields returns the inputs this engine DECLARES but must not
// WRITE for a given repo — values GitHub refuses, where a planned write
// wedges the stack instead of converging it.
//
// Extracted from deployRepo to be testable. Both rules were learned from
// production: the archived set from a 403 that blocked every repo behind
// it in the run, and allowForking from truvity/workstation, whose creation
// half-failed (repo created, resource errored) and left the stack unable
// to converge. Neither was catchable by reading.
func repoIgnoredFields(r registry.Resolved) []string {
	// The default branch follows the repository's history, not our
	// config; declaring it as an input would propose branch renames.
	// autoInit only ever matters at creation.
	ignore := []string{fieldAutoInit}

	// On a PRIVATE repository, forking is the ORGANIZATION's setting
	// (`members_can_fork_private_repositories`), not the repository's.
	// GitHub answers a PATCH carrying `allow_forking` with
	//
	//   422 This organization does not allow private repository forking
	//
	// even when the value we send agrees with the org — the field is not
	// ours to write. Same reasoning as the archived block below: a value
	// GitHub will not accept must never become a planned write.
	//
	// The imported estate hid this completely. Import records live values,
	// so an imported private repo plans no write to the field and the
	// engine looks correct. Creating truvity/workstation on 2026-08-16 was
	// the first time this engine MADE a private repo rather than adopting
	// one: the POST succeeded, the follow-up PATCH 422'd, and the stack
	// then could not converge — every later run re-planned the same doomed
	// update, leaving the repo with no branch protection.
	//
	// Public repos are deliberately left alone: they are always forkable
	// and GitHub does not allow turning that off, so the live value is the
	// only possible one and ignoring it would hide drift we can see.
	if r.Visibility == registry.VisibilityPrivate {
		ignore = append(ignore, "allowForking")
	}

	// On an archived repository `archived` is the only attribute the
	// engine may own. GitHub rejects a PATCH to any other field, so a
	// profile edit — or simply a drift the profile would otherwise
	// correct — must not become a planned write here: it would 403 and
	// wedge the stack for every repo behind it in the run.
	//
	// Ignoring rather than omitting is deliberate. The args above still
	// carry the profile's values, so the row keeps reading as a normal
	// repository and clearing `archived` restores full management with
	// no other edit.
	if r.Archived {
		ignore = append(ignore,
			"visibility",
			"description",
			"hasIssues",
			"hasWiki",
			"hasProjects",
			"allowAutoMerge",
			"allowSquashMerge",
			"allowMergeCommit",
			"allowRebaseMerge",
			"allowUpdateBranch",
			"deleteBranchOnMerge",
			"allowForking",
		)
	}

	// Dedup: a private AND archived repo hits both branches, and both name
	// allowForking. Pulumi tolerates the repeat, but it is the tell that two
	// rules were written without knowing about each other — collapse it here
	// so a third rule cannot quietly multiply.
	seen := make(map[string]struct{}, len(ignore))
	out := ignore[:0]

	for _, f := range ignore {
		if _, dup := seen[f]; dup {
			continue
		}

		seen[f] = struct{}{}

		out = append(out, f)
	}

	return out
}

func deployRepo(
	c *pulumi.Context,
	name string,
	r registry.Resolved,
	live *liveState,
	provider *github.Provider,
) (*github.Repository, error) {
	args := &github.RepositoryArgs{
		Name:       pulumi.String(name),
		Visibility: pulumi.String(r.Visibility),

		HasIssues:   pulumi.Bool(r.HasIssues),
		HasWiki:     pulumi.Bool(r.HasWiki),
		HasProjects: pulumi.Bool(r.HasProjects),

		AllowAutoMerge:      pulumi.Bool(r.AllowAutoMerge),
		AllowSquashMerge:    pulumi.Bool(r.AllowSquashMerge),
		AllowMergeCommit:    pulumi.Bool(r.AllowMergeCommit),
		AllowRebaseMerge:    pulumi.Bool(r.AllowRebaseMerge),
		AllowUpdateBranch:   pulumi.Bool(r.AllowUpdateBranch),
		DeleteBranchOnMerge: pulumi.Bool(r.DeleteBranchOnMerge),
		AllowForking:        pulumi.Bool(r.AllowForking),

		Archived: pulumi.Bool(r.Archived),
	}

	if r.Description != "" {
		args.Description = pulumi.String(r.Description)
	}

	ignore := repoIgnoredFields(r)

	opts := []pulumi.ResourceOption{
		pulumi.Provider(provider),
		pulumi.IgnoreChanges(ignore),
		// Losing a repository to a mistyped stack name is not a
		// recoverable accident. Protect is a resource OPTION rather than
		// an input, so unlike archiveOnDestroy — which is client-side
		// only and therefore unreadable, leaving a permanent diff that
		// blocks import entirely — it costs nothing at import time.
		//
		// It stops a DELETE. It does not stop a REPLACE, which on a
		// repository is a delete-and-recreate that takes the history with
		// it — see the team block above for the 2026-08-07 proof that
		// protect:true does not hold against one. `just github-preflight`
		// is what refuses that plan (INF-530).
		pulumi.Protect(true),
	}

	if live.repoExists(name) {
		opts = append(opts, pulumi.Import(pulumi.ID(name)))
	}

	return github.NewRepository(c, "repo-"+name, args, opts...)
}

func deployRepoActions(
	c *pulumi.Context,
	name string,
	r registry.Resolved,
	repo *github.Repository,
	live *liveState,
	provider *github.Provider,
) error {
	opts := []pulumi.ResourceOption{
		pulumi.Provider(provider),
		// Dropping one of these from the program means "stop managing
		// this setting", never "put the setting back". Without this the
		// provider's delete PUTs Actions defaults onto a live
		// repository, which is both a surprising mutation and, once the
		// repo is archived, a 403 that fails the whole run — and Pulumi
		// orders deletes AFTER updates, so the archive lands first and
		// the reset cannot win. Retaining makes an archival pure config.
		pulumi.RetainOnDelete(true),
	}

	// Both Actions resources are settings ON a repository rather than
	// objects of their own: they exist exactly when the repo does, and
	// import by repository name.
	if live.repoExists(name) {
		opts = append(opts, pulumi.Import(pulumi.ID(name)))
	}

	// Repository comes from the repo RESOURCE, not the bare name, so the
	// engine has a dependency edge. With a plain string both Actions
	// settings raced their repository's creation, and on 2026-08-23 one
	// lost: PUT .../actions/permissions answered 404 seconds after the
	// repo create returned (create-path bug #3 —
	// docs/operations/github-engine-create-path.md). The value is
	// identical, so the imported estate sees no diff.
	if _, err := github.NewActionsRepositoryPermissions(c, "actions-"+name, &github.ActionsRepositoryPermissionsArgs{
		Repository:     repo.Name,
		Enabled:        pulumi.Bool(true),
		AllowedActions: pulumi.String(r.Actions.AllowedActions),
	}, opts...); err != nil {
		return err
	}

	// Workflow permissions cannot be written on a repository with no
	// commits — GitHub answers 409 — so migration targets skip the
	// resource until their history arrives; the diff that appears then
	// applies cleanly.
	if live.repoEmpty(name) {
		return nil
	}

	_, err := github.NewWorkflowRepositoryPermissions(c, "workflow-perms-"+name, &github.WorkflowRepositoryPermissionsArgs{
		Repository:                   repo.Name,
		DefaultWorkflowPermissions:   pulumi.String(r.Actions.DefaultWorkflowPermissions),
		CanApprovePullRequestReviews: pulumi.Bool(r.Actions.CanApprovePullRequests),
	}, opts...)

	return err
}

// deployProtection declares the default branch's protection rule.
//
// A row with protection.enabled:false gets NO resource at all — that is
// a different state from an empty rule, and it is the state gitops is in
// today (snapshot D1/D2).
func deployProtection(
	c *pulumi.Context,
	name string,
	r registry.Resolved,
	repo *github.Repository,
	provider *github.Provider,
) error {
	if !r.Protection.Enabled {
		return nil
	}

	p := r.Protection

	args := &github.BranchProtectionArgs{
		RepositoryId:                  repo.NodeId,
		Pattern:                       pulumi.String(r.DefaultBranch),
		EnforceAdmins:                 pulumi.Bool(p.EnforceAdmins),
		AllowsDeletions:               pulumi.Bool(p.AllowDeletions),
		AllowsForcePushes:             pulumi.Bool(p.AllowForcePushes),
		RequireConversationResolution: pulumi.Bool(p.RequireConvResolution),
		RequireSignedCommits:          pulumi.Bool(p.RequireSignatures),
		RequiredLinearHistory:         pulumi.Bool(p.RequireLinearHistory),
	}

	if len(p.RequiredChecks) > 0 {
		contexts := make(pulumi.StringArray, 0, len(p.RequiredChecks))
		for _, check := range p.RequiredChecks {
			contexts = append(contexts, pulumi.String(check))
		}

		args.RequiredStatusChecks = github.BranchProtectionRequiredStatusCheckArray{
			github.BranchProtectionRequiredStatusCheckArgs{
				Strict:   pulumi.Bool(p.Strict),
				Contexts: contexts,
			},
		}
	}

	// 0 approvals means NO review block at all — GitHub distinguishes
	// "no reviews required" from "zero approvals required", and sending
	// the latter turns reviews on.
	if p.RequiredApprovals > 0 {
		review := github.BranchProtectionRequiredPullRequestReviewArgs{
			RequiredApprovingReviewCount: pulumi.Int(p.RequiredApprovals),
			DismissStaleReviews:          pulumi.Bool(p.DismissStaleReviews),
			RequireCodeOwnerReviews:      pulumi.Bool(p.RequireCodeOwnerReviews),
		}

		// Actors whose PRs skip the review requirement while checks
		// stay required — how renovate automerges green non-major PRs
		// on a human-reviewed repo (policy-as-code, not a bot
		// rubber-stamping approvals).
		if len(p.PullRequestBypassers) > 0 {
			bypassers := make(pulumi.StringArray, 0, len(p.PullRequestBypassers))
			for _, b := range p.PullRequestBypassers {
				bypassers = append(bypassers, pulumi.String(b))
			}

			review.PullRequestBypassers = bypassers
		}

		args.RequiredPullRequestReviews = github.BranchProtectionRequiredPullRequestReviewArray{review}
	}

	opts := []pulumi.ResourceOption{pulumi.Provider(provider)}

	// NO import here, deliberately — see the note on check-then-import in
	// the package doc.
	//
	// Import is safe only for a resource this stack has never managed. Once
	// Pulumi CREATES a rule, the next run finds it live, flips to the import
	// branch, and asks to import a resource that is already in state without
	// an importID — which Pulumi satisfies by REPLACING it. The delete half
	// of that replace removes the branch's protection, and the following
	// diff reads clean, so nothing surfaces it.
	//
	// That is not hypothetical: on 2026-07-29 both `gitops` and
	// `github-roster` were found with no protection at all and stale rule
	// IDs in state. Every rule this engine creates arms the bomb on its own
	// next run.
	//
	// Adopting an organization whose rules pre-date the engine is now a
	// one-off `pulumi import`, which is the right place for a one-off.

	_, err := github.NewBranchProtection(c, "protection-"+name, args, opts...)

	return err
}

// deployTagRulesets declares one repository ruleset per tag_rulesets
// row: creation, update and deletion of refs matching the pattern are
// restricted to the bypass teams. This is how a release act (a
// {project}/v* tag) gets an owner — push access to the repository no
// longer implies the right to cut a release.
//
// NO import here, deliberately — the same reasoning as branch
// protection above: check-then-import is only sound for resources this
// stack has never managed, and every ruleset that exists was created by
// this stack (the engine predates rulesets on these orgs). Adopting a
// pre-existing ruleset is a one-off `pulumi import`.
func deployTagRulesets(
	c *pulumi.Context,
	repoName string,
	rulesets []*registry.TagRuleset,
	teams map[string]*github.Team,
	provider *github.Provider,
) error {
	for _, rs := range rulesets {
		actors := make(github.RepositoryRulesetBypassActorArray, 0, len(rs.BypassTeams))

		for _, slug := range rs.BypassTeams {
			team, ok := teams[slug]
			if !ok {
				// Validation guarantees the team row exists; missing here
				// means it failed to deploy, which fails the run earlier.
				return fmt.Errorf("bypass team %q not deployed", slug)
			}

			actorID := team.ID().ToStringOutput().ApplyT(strconv.Atoi).(pulumi.IntOutput)

			actors = append(actors, github.RepositoryRulesetBypassActorArgs{
				ActorId:    actorID,
				ActorType:  pulumi.String("Team"),
				BypassMode: pulumi.String("always"),
			})
		}

		_, err := github.NewRepositoryRuleset(c, "ruleset-"+repoName+"-"+rs.Name, &github.RepositoryRulesetArgs{
			Name:        pulumi.String(rs.Name),
			Repository:  pulumi.String(repoName),
			Target:      pulumi.String("tag"),
			Enforcement: pulumi.String("active"),
			Conditions: &github.RepositoryRulesetConditionsArgs{
				RefName: &github.RepositoryRulesetConditionsRefNameArgs{
					Includes: pulumi.StringArray{pulumi.String(rs.Pattern)},
					Excludes: pulumi.StringArray{},
				},
			},
			Rules: &github.RepositoryRulesetRulesArgs{
				Creation: pulumi.Bool(true),
				Update:   pulumi.Bool(true),
				Deletion: pulumi.Bool(true),
			},
			BypassActors: actors,
		}, pulumi.Provider(provider))
		if err != nil {
			return fmt.Errorf("ruleset %s: %w", rs.Name, err)
		}
	}

	return nil
}

// deployBranchRulesets declares branch-target rulesets: a PR approval
// requirement that named Apps bypass (Integration actors by DATABASE
// id). This shape exists because classic protection cannot carry an
// App bypass under App auth (D11) — the write fails with "Resource
// not accessible by integration" and a REST-set allowance wedges the
// provider's refresh of the whole protection object. Create-only, no
// import — the branch-protection lesson applies unchanged.
func deployBranchRulesets(
	c *pulumi.Context,
	repoName string,
	rulesets []*registry.BranchRuleset,
	provider *github.Provider,
) error {
	for _, rs := range rulesets {
		actors := make(github.RepositoryRulesetBypassActorArray, 0, len(rs.BypassApps)+1)

		// OrganizationAdmin: GitHub's REST API IGNORES actor_id on write
		// for this actor type and returns 0 on read — so 0 is the only
		// drift-free spelling. Declaring the documented "1" produced a
		// perpetual update-in-plan (write 1, read 0, diff forever),
		// found via refresh validation on 2026-08-24.
		if rs.BypassOrgAdmins {
			actors = append(actors, github.RepositoryRulesetBypassActorArgs{
				ActorId:    pulumi.Int(0),
				ActorType:  pulumi.String("OrganizationAdmin"),
				BypassMode: pulumi.String("always"),
			})
		}

		for _, id := range rs.BypassApps {
			actors = append(actors, github.RepositoryRulesetBypassActorArgs{
				ActorId:    pulumi.Int(id),
				ActorType:  pulumi.String("Integration"),
				BypassMode: pulumi.String("always"),
			})
		}

		_, err := github.NewRepositoryRuleset(c, "ruleset-"+repoName+"-"+rs.Name, &github.RepositoryRulesetArgs{
			Name:        pulumi.String(rs.Name),
			Repository:  pulumi.String(repoName),
			Target:      pulumi.String("branch"),
			Enforcement: pulumi.String("active"),
			Conditions: &github.RepositoryRulesetConditionsArgs{
				RefName: &github.RepositoryRulesetConditionsRefNameArgs{
					Includes: pulumi.StringArray{pulumi.String(rs.Pattern)},
					Excludes: pulumi.StringArray{},
				},
			},
			Rules: &github.RepositoryRulesetRulesArgs{
				PullRequest: &github.RepositoryRulesetRulesPullRequestArgs{
					RequiredApprovingReviewCount: pulumi.Int(rs.RequiredApprovals),
				},
			},
			BypassActors: actors,
		}, pulumi.Provider(provider))
		if err != nil {
			return fmt.Errorf("branch ruleset %s: %w", rs.Name, err)
		}
	}

	return nil
}

func deployTeamGrants(
	c *pulumi.Context,
	name string,
	r registry.Resolved,
	teams map[string]*github.Team,
	live *liveState,
	provider *github.Provider,
) error {
	slugs := make([]string, 0, len(r.Teams))
	for slug := range r.Teams {
		slugs = append(slugs, slug)
	}

	// Deterministic order: creation order feeds URNs.
	sort.Strings(slugs)

	for _, slug := range slugs {
		team, ok := teams[slug]
		if !ok {
			return fmt.Errorf("grant to unknown team %q (validation should have caught this)", slug)
		}

		opts := []pulumi.ResourceOption{pulumi.Provider(provider)}

		// TeamRepository imports by "teamID:repo" with the NUMERIC team
		// ID — the slug is rejected. Importable only when the GRANT
		// itself already exists; team-and-repo existing is not enough.
		if teamID, exists := live.teamID(slug); exists && live.repoExists(name) && live.grantExists(slug, name) {
			opts = append(opts, pulumi.Import(pulumi.ID(teamID+":"+name)))
		}

		if _, err := github.NewTeamRepository(c, "grant-"+slug+"-"+name, &github.TeamRepositoryArgs{
			TeamId:     team.ID().ToStringOutput(),
			Repository: pulumi.String(name),
			Permission: pulumi.String(r.Teams[slug]),
		}, opts...); err != nil {
			return fmt.Errorf("team %s: %w", slug, err)
		}
	}

	return nil
}

// topoSortTeams orders teams parents-first, so a child always has its
// parent's ID available. Ties break alphabetically to keep resource
// order stable between runs.
func topoSortTeams(orgCfg *registry.Org) []string {
	ordered := make([]string, 0, len(orgCfg.Teams))
	placed := make(map[string]bool, len(orgCfg.Teams))

	for len(ordered) < len(orgCfg.Teams) {
		progress := false

		for _, slug := range orgCfg.SortedTeams() {
			if placed[slug] {
				continue
			}

			if parent := orgCfg.Teams[slug].Parent; parent != "" && !placed[parent] {
				continue
			}

			ordered = append(ordered, slug)
			placed[slug] = true
			progress = true
		}

		// Validation rejects cycles; this guards against a future change
		// breaking that guarantee rather than an expected path.
		if !progress {
			break
		}
	}

	return ordered
}

func teamName(slug string, row *registry.Team) string {
	if row.Name != "" {
		return row.Name
	}

	return slug
}

// notificationSetting maps our readable value onto GitHub's enum.
func notificationSetting(v string) string {
	if v == registry.NotificationsDisabled {
		return "notifications_disabled"
	}

	return "notifications_enabled"
}
