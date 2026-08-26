// Package registry loads and validates the GitHub plane registry — one
// YAML file (github.yaml at the root of the fs.FS handed to Load)
// describing every org the structure engine owns: org settings, teams,
// repos (a settings profile plus per-repo overrides with reasons), the
// GitHub Apps the org depends on, and the Actions runner groups ARC
// registers scale sets against.
//
// The registry is DESIRED STATE for everything Pulumi can apply and
// INVENTORY for everything it cannot (App creation, App installation and
// App permission edits have no API — see docs/operations/github-apps-day1.md).
// Rows marked external are third-party Apps: drift detection only.
//
// Company-agnosticism is the design constraint. Profiles are top-level
// and shared, so a second org (INF-473, the TP migration) is a new
// `orgs:` key referencing the same `public`/`private` profiles — never
// new code.
package registry

import (
	"fmt"
	"io/fs"
	"regexp"
	"strings"
)

// Team permission levels, in ascending order of power. These are the
// values GitHub accepts for a team's permission on a repository.
const (
	PermPull     = "pull"
	PermTriage   = "triage"
	PermPush     = "push"
	PermMaintain = "maintain"
	PermAdmin    = "admin"
)

// Repository visibility values.
const (
	VisibilityPublic  = "public"
	VisibilityPrivate = "private"
)

// Team privacy values. GitHub calls a visible team "closed" and a hidden
// one "secret"; nested teams must be closed.
const (
	PrivacyClosed = "closed"
	PrivacySecret = "secret"
)

// Team notification settings.
const (
	NotificationsEnabled  = "enabled"
	NotificationsDisabled = "disabled"
)

// Actions allowed-actions policy values.
const (
	ActionsAll             = "all"
	ActionsLocalOnly       = "local_only"
	ActionsSelected        = "selected"
	WorkflowPermissionRead = "read"
	WorkflowPermissionWrit = "write"
)

// App installation scope values.
const (
	InstallAll      = "all"
	InstallSelected = "selected"
)

// Runner-group visibility values.
const (
	RunnerVisibilityAll      = "all"
	RunnerVisibilitySelected = "selected"
	RunnerVisibilityPrivate  = "private"
)

var (
	// slugPattern covers org logins, team slugs, repo names and App names.
	slugPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

	validPermissions = map[string]bool{
		PermPull: true, PermTriage: true, PermPush: true,
		PermMaintain: true, PermAdmin: true,
	}

	validVisibility = map[string]bool{
		VisibilityPublic: true, VisibilityPrivate: true,
	}

	validPrivacy = map[string]bool{
		PrivacyClosed: true, PrivacySecret: true,
	}

	validNotifications = map[string]bool{
		NotificationsEnabled: true, NotificationsDisabled: true,
	}

	validAllowedActions = map[string]bool{
		ActionsAll: true, ActionsLocalOnly: true, ActionsSelected: true,
	}

	validWorkflowPermissions = map[string]bool{
		WorkflowPermissionRead: true, WorkflowPermissionWrit: true,
	}

	validInstallScope = map[string]bool{
		InstallAll: true, InstallSelected: true,
	}

	validRunnerVisibility = map[string]bool{
		RunnerVisibilityAll: true, RunnerVisibilitySelected: true, RunnerVisibilityPrivate: true,
	}

	// validAppPermissions is the GitHub App permission vocabulary we use.
	// Org-level keys carry the organization_ prefix (plus `members`),
	// exactly as GET /orgs/{org}/installations reports them — so a drift
	// check is a literal map comparison, no translation layer.
	validAppPermissions = map[string]bool{
		// repository-level
		"actions": true, "administration": true, "checks": true,
		"contents": true, "deployments": true, "discussions": true,
		"environments": true, "issues": true, "merge_queues": true,
		"metadata": true, "packages": true, "pages": true,
		"pull_requests": true, "repository_hooks": true,
		"repository_projects": true, "secret_scanning_alerts": true,
		"secrets": true, "security_events": true, "statuses": true,
		"vulnerability_alerts": true, "workflows": true,
		// organization-level
		"members": true, "organization_administration": true,
		"organization_custom_properties": true, "organization_custom_roles": true,
		"organization_events": true, "organization_hooks": true,
		"organization_packages": true, "organization_plan": true,
		"organization_projects": true, "organization_secrets": true,
		"organization_self_hosted_runners": true, "organization_user_blocking": true,
		"team_discussions": true,
	}

	validAppPermissionLevels = map[string]bool{
		"read": true, "write": true, "admin": true,
	}
)

type (
	// Config is the whole registry.
	Config struct {
		// Profiles are the shared repo settings vocabulary. Every repo row
		// names exactly one; per-repo overrides layer on top.
		Profiles map[string]*RepoSettings `yaml:"profiles"`
		// Orgs maps a GitHub org login to its desired structure.
		Orgs map[string]*Org `yaml:"orgs"`
	}

	// Org is one GitHub organization.
	Org struct {
		// Company links this org to a cfg/companies.yaml row. Truvity is
		// the incumbent and has no code there; other orgs must name one.
		Company string `yaml:"company,omitempty"`
		// AppPrefix is the mandatory name prefix for Apps we author in
		// this org — App display names are globally unique on GitHub.
		// Defaults to "{org}-".
		AppPrefix string `yaml:"app_prefix,omitempty"`
		// CredentialsSSMPrefix is where the structure engine's own App
		// credentials live in the estate's secret store — the caller
		// reads them from there and hands engine.Credentials to Deploy.
		CredentialsSSMPrefix string `yaml:"credentials_ssm_prefix"`
		// Settings are the org-level toggles.
		Settings *OrgSettings `yaml:"settings"`
		// Owners are the organization's owners (GitHub's `admin` org
		// role), by login. This is the half of a contract the roster
		// service already keeps its side of: it renders owners from
		// CURRENT state and never computes them, because "owners are
		// registry-pinned and change by reviewed infrastructure
		// commit". This registry is that pin.
		//
		// ASSERTED, NOT RECONCILED. Each login gets its own resource,
		// so the engine can promote a listed login but can never
		// demote one you forgot to list — a full-set model here would
		// make "omitted the block" mean "remove every owner", which is
		// the accident neither service may have. Divergence between
		// live and declared is reported by `githubctl drift`, where a
		// human decides, exactly as org variables work.
		//
		// Removing a login from this list DEMOTES them to member; it
		// does not evict them from the org (DowngradeOnDestroy).
		Owners []string `yaml:"owners,omitempty"`
		// Teams are the org's teams, keyed by slug. Membership is NOT
		// modeled here — that is the roster service's territory
		// (INF-484/INF-487); this engine owns structure only.
		Teams map[string]*Team `yaml:"teams"`
		// Repos are the in-scope repositories, keyed by name.
		Repos map[string]*Repo `yaml:"repos"`
		// Apps are the GitHub Apps the org depends on.
		Apps map[string]*App `yaml:"apps"`
		// RunnerGroups are the Actions runner groups ARC targets.
		RunnerGroups map[string]*RunnerGroup `yaml:"runner_groups,omitempty"`
		// SecurityConfigurations is INVENTORY, never desired state: the
		// provider has no resource for code security configurations, and
		// an enforced one makes GitHub reject per-repository writes to
		// the settings it covers. Recorded so the registry still answers
		// "who owns Dependabot alerts" — the same role the `external`
		// App rows play. See
		// docs/operations/github-security-configurations.md.
		SecurityConfigurations map[string]*SecurityConfiguration `yaml:"security_configurations,omitempty"`
	}

	// SecurityConfiguration is one org-level code security configuration,
	// recorded for humans. Nothing reads these fields.
	SecurityConfiguration struct {
		Enforcement               string `yaml:"enforcement"`
		DependabotAlerts          string `yaml:"dependabot_alerts"`
		DependabotSecurityUpdates string `yaml:"dependabot_security_updates"`
		AppliesTo                 string `yaml:"applies_to"`
	}

	// OrgSettings are the org-level toggles the engine owns.
	//
	// The remaining cosmetic fields (description, blog, location) stay
	// absent: they are null today and managing null invites diff noise
	// (D9). DisplayName is the exception — see its comment.
	OrgSettings struct {
		// DisplayName is the organization's profile name — the "Name"
		// field in the UI, NOT the URL slug (that is the immutable
		// `login`). Empty means unmanaged, which is what D9 asked for
		// while it was null.
		//
		// Setting it is also what stops Pulumi auto-naming the resource
		// and pushing a generated string like "org-truvity-1fd213c" onto
		// the organization.
		DisplayName string `yaml:"display_name,omitempty"`
		// Billing is deliberately NOT here (decided 2026-07-31): it is
		// the IT team's, managed in the GitHub UI. The provider's
		// settings resource requires the field, so the engine passes the
		// org's live value through and never diffs it.
		DefaultRepositoryPermission          string `yaml:"default_repository_permission"`
		MembersCanCreateRepositories         bool   `yaml:"members_can_create_repositories"`
		MembersCanCreatePublicRepositories   bool   `yaml:"members_can_create_public_repositories"`
		MembersCanCreatePrivateRepositories  bool   `yaml:"members_can_create_private_repositories"`
		MembersCanCreateInternalRepositories bool   `yaml:"members_can_create_internal_repositories"`
		MembersCanCreatePages                bool   `yaml:"members_can_create_pages"`
		MembersCanCreatePublicPages          bool   `yaml:"members_can_create_public_pages"`
		MembersCanCreatePrivatePages         bool   `yaml:"members_can_create_private_pages"`
		MembersCanForkPrivateRepositories    bool   `yaml:"members_can_fork_private_repositories"`
		WebCommitSignoffRequired             bool   `yaml:"web_commit_signoff_required"`
		HasOrganizationProjects              bool   `yaml:"has_organization_projects"`
		HasRepositoryProjects                bool   `yaml:"has_repository_projects"`
		DependabotAlertsEnabledForNewRepos   bool   `yaml:"dependabot_alerts_enabled_for_new_repositories"`
		DependabotSecurityUpdatesForNewRepos bool   `yaml:"dependabot_security_updates_enabled_for_new_repositories"`
		DependencyGraphEnabledForNewRepos    bool   `yaml:"dependency_graph_enabled_for_new_repositories"`
		AdvancedSecurityEnabledForNewRepos   bool   `yaml:"advanced_security_enabled_for_new_repositories"`
		SecretScanningForNewRepos            bool   `yaml:"secret_scanning_enabled_for_new_repositories"`
		SecretScanningPushProtForNewRepos    bool   `yaml:"secret_scanning_push_protection_enabled_for_new_repositories"`
		// Actions is the ORG-level Actions policy — the ceiling every
		// repository's own policy sits under (INF-410's "Actions org
		// permissions"). Readable only through the App: a human token
		// with read:org gets 403.
		Actions *OrgActions `yaml:"actions"`
	}

	// OrgActions is the organization's GitHub Actions policy.
	OrgActions struct {
		AllowedActions             string `yaml:"allowed_actions"`
		EnabledRepositories        string `yaml:"enabled_repositories"`
		ShaPinningRequired         bool   `yaml:"sha_pinning_required"`
		DefaultWorkflowPermissions string `yaml:"default_workflow_permissions"`
		CanApprovePullRequests     bool   `yaml:"can_approve_pull_request_reviews"`
		// Variables are org-level Actions variables: the particulars a
		// PUBLIC shared workflow must never contain (runner labels, bucket
		// names, in-cluster URLs). The workflow reads them from the
		// caller's context, so the same public code serves both orgs
		// without either one's estate appearing in it.
		Variables map[string]*OrgVariable `yaml:"variables,omitempty"`
	}

	// OrgVariable is one org-level Actions variable.
	OrgVariable struct {
		Value string `yaml:"value"`
		// Visibility is private | selected | all, defaulting to PRIVATE —
		// the safe end. A variable naming internal infrastructure that
		// drifts to `all` becomes readable by any public repository's
		// workflow, which is exactly the leak the public shared workflow
		// exists to avoid. `selected` additionally carries a repository
		// list that is NOT modeled here; the drift check reports the
		// visibility, not the membership.
		Visibility string `yaml:"visibility,omitempty"`
	}

	// Team is one org team. Nesting is expressed by Parent (a team slug).
	Team struct {
		Name          string `yaml:"name,omitempty"`
		Description   string `yaml:"description,omitempty"`
		Privacy       string `yaml:"privacy,omitempty"`
		Parent        string `yaml:"parent,omitempty"`
		Notifications string `yaml:"notifications,omitempty"`
	}

	// Repo is one repository row: a profile reference plus the deviations
	// that survived the keep-or-fix review.
	Repo struct {
		// Profile names a key in Config.Profiles.
		Profile string `yaml:"profile"`
		// Description is the repo blurb; empty means unmanaged.
		Description string `yaml:"description,omitempty"`
		// Teams are grants ADDED to the profile's team map (or upgrades
		// of a profile grant). Removing a profile grant is not expressible
		// on purpose — that is a profile change, not a repo exception.
		Teams map[string]string `yaml:"teams,omitempty"`
		// TagRulesets are repository rulesets targeting tags: each
		// restricts creation, update and deletion of refs matching its
		// pattern to the listed bypass teams. This is how a release act
		// (a {project}/v* tag) gets an owner: push access to the repo no
		// longer implies the right to cut a release.
		//
		// Per-repo and raw (not part of the profile/override merge):
		// which tag namespaces exist is a property of the repo's release
		// contract, not of its settings tier.
		TagRulesets []*TagRuleset `yaml:"tag_rulesets,omitempty"`
		// BranchRulesets are repository rulesets targeting branches —
		// today expressing exactly one shape: a pull-request approval
		// requirement that named GitHub Apps may bypass. This exists
		// because classic protection cannot carry an App bypass under
		// the engine's App auth at all: referencing an App actor fails
		// with "Resource not accessible by integration" on write AND
		// wedges refresh once set via REST (observed 2026-08-15, D11).
		// Rulesets take Integration bypass actors by database ID over
		// REST — the same machinery the tag rulesets already use.
		BranchRulesets []*BranchRuleset `yaml:"branch_rulesets,omitempty"`
		// ChecksWaived suspends the profile's required status checks for
		// this repo, and its value is the reason.
		//
		// This exists because the failure mode is silent and total:
		// requiring a check context that no workflow produces blocks
		// EVERY pull request, forever. A repo whose CI has not landed
		// yet therefore needs an explicit, per-repo escape — while the
		// profile keeps stating the intent, so the exception is visible
		// as an exception rather than as a profile that asks for
		// nothing (INF-410).
		//
		// Lifting it is deleting one line. Waiving checks a profile does
		// not require is rejected, so a waiver cannot outlive its cause.
		ChecksWaived string `yaml:"checks_waived,omitempty"`
		// Overrides are per-repo settings deviations. Every override is a
		// documented decision (see the snapshot's D-table); Reason says
		// which, so a future reader knows whether it is permanent.
		Overrides *RepoSettings `yaml:"overrides,omitempty"`
		// Reason explains the overrides. Required when Overrides is set.
		Reason string `yaml:"reason,omitempty"`
		// Archived retires the repository: read-only on GitHub, and here
		// reduced to a single owned attribute.
		//
		// GitHub rejects settings writes on an archived repository, so
		// the engine declares NOTHING else for one — no Actions
		// permissions, no workflow permissions, no branch protection —
		// and ignores every settings input on the repository itself.
		// Only `archived` is owned; the rest is frozen at whatever it
		// held on the way in.
		//
		// This is what keeps a retirement expressible. Before it existed
		// the only options were to leave a live row that 403s on every
		// run — the trap that wedged the engine during the INF-512
		// sdk-python adoption — or to drop the row and hand-archive,
		// which puts the repo outside the registry and leaves a
		// protected orphan in state. See
		// docs/operations/github-repo-archival.md.
		//
		// Reversible: clear the flag and the row resumes full
		// management, because unarchiving is itself a settings write the
		// provider makes before any other.
		Archived bool `yaml:"archived,omitempty"`
	}

	// BranchRuleset is one branch-target ruleset row: a PR approval
	// gate with App bypass (see the field comment on Repo).
	BranchRuleset struct {
		// Name is the ruleset's display name, unique within the repo.
		Name string `yaml:"name"`
		// Pattern is the ref pattern (e.g. ~DEFAULT_BRANCH).
		Pattern string `yaml:"pattern"`
		// RequiredApprovals is the PR review count the ruleset enforces.
		RequiredApprovals int `yaml:"required_approvals"`
		// BypassApps are GitHub App DATABASE ids (not node ids) allowed
		// to bypass — how renovate automerges its green PRs.
		BypassApps []int `yaml:"bypass_apps"`
		// BypassOrgAdmins lets organization admins merge without the
		// review. Rulesets, unlike classic protection, do NOT exempt
		// admins implicitly: moving bar's approval gate into a ruleset
		// (D11) silently removed the admin bypass the repo had relied
		// on, blocking its own maintainers' PRs. Set this to keep the
		// pre-ruleset behavior explicit rather than accidental.
		BypassOrgAdmins bool `yaml:"bypass_org_admins,omitempty"`
	}

	// TagRuleset is one tag-protection ruleset row on a repository.
	TagRuleset struct {
		// Name is the ruleset's display name, unique within the repo.
		Name string `yaml:"name"`
		// Pattern is the fnmatch ref pattern the ruleset covers; it must
		// start with refs/tags/ (this type expresses tag rulesets only).
		Pattern string `yaml:"pattern"`
		// BypassTeams are the org team slugs allowed to create, update
		// and delete matching tags. At least one is required: a ruleset
		// nobody can bypass makes the tag namespace permanently
		// unwritable, which is a bricked release path, not protection.
		BypassTeams []string `yaml:"bypass_teams"`
	}

	// RepoSettings is both a profile (all fields set) and an override
	// (any subset). Pointers distinguish "not specified" from "false" —
	// the whole reason overrides can be partial.
	RepoSettings struct {
		Visibility          *string `yaml:"visibility,omitempty"`
		HasIssues           *bool   `yaml:"has_issues,omitempty"`
		HasWiki             *bool   `yaml:"has_wiki,omitempty"`
		HasProjects         *bool   `yaml:"has_projects,omitempty"`
		AllowAutoMerge      *bool   `yaml:"allow_auto_merge,omitempty"`
		AllowSquashMerge    *bool   `yaml:"allow_squash_merge,omitempty"`
		AllowMergeCommit    *bool   `yaml:"allow_merge_commit,omitempty"`
		AllowRebaseMerge    *bool   `yaml:"allow_rebase_merge,omitempty"`
		DeleteBranchOnMerge *bool   `yaml:"delete_branch_on_merge,omitempty"`
		AllowUpdateBranch   *bool   `yaml:"allow_update_branch,omitempty"`
		AllowForking        *bool   `yaml:"allow_forking,omitempty"`
		HasDownloads        *bool   `yaml:"has_downloads,omitempty"`
		DefaultBranch       *string `yaml:"default_branch,omitempty"`

		Actions    *ActionsSettings    `yaml:"actions,omitempty"`
		Protection *ProtectionSettings `yaml:"protection,omitempty"`

		// Teams is the profile's baseline team grant map. On an override
		// it is ignored — repo-level grants live in Repo.Teams.
		Teams map[string]string `yaml:"teams,omitempty"`
	}

	// ActionsSettings is the repo's GitHub Actions policy.
	ActionsSettings struct {
		AllowedActions             *string `yaml:"allowed_actions,omitempty"`
		DefaultWorkflowPermissions *string `yaml:"default_workflow_permissions,omitempty"`
		CanApprovePullRequests     *bool   `yaml:"can_approve_pull_request_reviews,omitempty"`
	}

	// ProtectionSettings is the default branch's protection rule.
	// Enabled:false means "no protection rule at all" (GitHub 404s), not
	// "an empty rule" — the two are different states.
	ProtectionSettings struct {
		Enabled *bool `yaml:"enabled,omitempty"`
		// RequiredChecks are status-check contexts. A context declared
		// here that no workflow produces blocks every PR forever — see
		// the snapshot's D1.
		RequiredChecks *[]string `yaml:"required_checks,omitempty"`
		// Strict is GitHub's "branch must be up to date before merging".
		// False fleet-wide: the public-repo CI lesson.
		Strict *bool `yaml:"strict,omitempty"`
		// EnforceAdmins applies the rule to admins too.
		EnforceAdmins *bool `yaml:"enforce_admins,omitempty"`
		// RequiredApprovals of 0 means NO review requirement block at all.
		RequiredApprovals *int `yaml:"required_approvals,omitempty"`
		// PullRequestBypassers are actors whose PRs skip the REVIEW
		// requirement (checks stay required). Format: usernames,
		// org/team slugs, or /<app-slug> (the provider's leading-slash
		// convention for Apps — app/<slug> fails to resolve). This is how automation
		// (renovate) automerges its green non-major PRs on a repo
		// whose humans still need review — policy-as-code instead of
		// a bot rubber-stamping approvals. Requires
		// required_approvals > 0 (bypassing a review nobody requires
		// is a config smell the validator rejects).
		PullRequestBypassers    *[]string `yaml:"pull_request_bypassers,omitempty"`
		DismissStaleReviews     *bool     `yaml:"dismiss_stale_reviews,omitempty"`
		RequireCodeOwnerReviews *bool     `yaml:"require_code_owner_reviews,omitempty"`
		RequireConvResolution   *bool     `yaml:"require_conversation_resolution,omitempty"`
		RequireLinearHistory    *bool     `yaml:"require_linear_history,omitempty"`
		RequireSignatures       *bool     `yaml:"require_signatures,omitempty"`
		AllowForcePushes        *bool     `yaml:"allow_force_pushes,omitempty"`
		AllowDeletions          *bool     `yaml:"allow_deletions,omitempty"`
	}

	// App is one GitHub App the org depends on.
	App struct {
		// External marks a vendor App: inventory + drift detection only.
		// We never create, install or edit those.
		External bool `yaml:"external,omitempty"`
		// Adopted marks an App we author that predates this registry and
		// whose 1Password item has not been normalized to the field
		// convention yet. Such a row is inventory + drift only until the
		// item is normalized and credentials are filled in — at which
		// point the flag goes away. It is deliberately narrow: a row can
		// be credential-less only by admitting it in writing.
		Adopted bool `yaml:"adopted,omitempty"`
		// Description is the App blurb (ours: rendered into the manifest).
		Description string `yaml:"description,omitempty"`
		// URL is the App's homepage (ours: required by the manifest flow).
		URL string `yaml:"url,omitempty"`
		// AppID and InstallationID are recorded once known. Optional —
		// drift detection keys on the App slug, so a freshly registered
		// row works before its first creation.
		AppID          int64 `yaml:"app_id,omitempty"`
		InstallationID int64 `yaml:"installation_id,omitempty"`
		// Install is the installation's repository scope.
		Install string `yaml:"install"`
		// Repos scopes a `selected` installation. Empty means "not read
		// yet" — there is no REST endpoint to read another App's scope
		// with a user token (snapshot §4.3).
		Repos []string `yaml:"repos,omitempty"`
		// Permissions is the App's permission set, keyed exactly as
		// GET /orgs/{org}/installations reports it.
		Permissions map[string]string `yaml:"permissions"`
		// Events are the webhook events the App subscribes to.
		Events []string `yaml:"events,omitempty"`
		// WebhookURL is where GitHub delivers events. Empty means the App
		// is API-only and the manifest omits the webhook block entirely —
		// GitHub rejects a hook_attributes object that has no url.
		WebhookURL string `yaml:"webhook_url,omitempty"`
		// Credentials locate the App's secrets. Required for ours,
		// forbidden for external rows (we never hold vendor keys).
		Credentials *AppCredentials `yaml:"credentials,omitempty"`
		// Note carries context a future reader needs (why it exists, what
		// replaced it, which ticket retires it).
		Note string `yaml:"note,omitempty"`
	}

	// AppCredentials is the one credential doctrine: 1Password is the
	// source of truth, SSM is the mirror. The field names inside the
	// 1Password item are fixed by convention (see FieldAppID and friends).
	AppCredentials struct {
		// OpItem names the item in the estate's credential store (for
		// truvity, a 1Password item in the breakglass vault). Always
		// required: every App the engine authors has a source of truth.
		OpItem string `yaml:"op_item"`
		// SSMPrefix is where the estate mirrors the item for machine
		// consumers (for truvity, cfg/secrets.yaml → SSM).
		//
		// Optional, because mirroring is a CONSUMER's need, not a
		// property of the App. An App consumed only from GitHub itself
		// (Renovate on public repos, whose key is copied by hand into a
		// scoped org secret — INF-491) has a 1Password item and no SSM
		// path at all.
		SSMPrefix string `yaml:"ssm_prefix,omitempty"`
	}

	// RunnerGroup is an Actions runner group — the GitHub-side half of
	// the ARC scale-set model (which repos may target which runners).
	RunnerGroup struct {
		Visibility               string   `yaml:"visibility"`
		Repos                    []string `yaml:"repos,omitempty"`
		AllowsPublicRepositories bool     `yaml:"allows_public_repositories,omitempty"`
		RestrictedToWorkflows    bool     `yaml:"restricted_to_workflows,omitempty"`
		SelectedWorkflows        []string `yaml:"selected_workflows,omitempty"`
	}
)

// InstallationIDField returns the field holding an App's installation ID
// for the given organization. The App's PRIMARY org uses the bare
// `github-installation-id`; any additional installation of the same App
// — the roster's sandbox org being the one sanctioned case — is suffixed
// with the org, so a test installation can never be mistaken for, or
// overwrite, the production one.
func InstallationIDField(org, primaryOrg string) string {
	if org == "" || org == primaryOrg {
		return FieldInstallationID
	}

	return FieldInstallationID + "-" + org
}

// The credential field-name convention for App credentials. Every App's
// secret-store item uses exactly these names, so a consumer (or an AI
// agent session) can find credentials knowing only the App name.
const (
	FieldAppID          = "github-app-id"
	FieldInstallationID = "github-installation-id"
	FieldPrivateKey     = "github-private-key"
	FieldClientID       = "github-client-id"
	FieldClientSecret   = "github-client-secret"
	FieldWebhookSecret  = "github-webhook-secret"
)

// Load reads github.yaml from the given filesystem and validates it.
func Load(fsys fs.FS) (*Config, error) {
	var c Config
	if err := load(fsys, "github.yaml", &c); err != nil {
		return nil, err
	}

	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("github.yaml: %w", err)
	}

	return &c, nil
}

// Validate enforces every registry invariant.
func (c *Config) Validate() error {
	if len(c.Profiles) == 0 {
		return fmt.Errorf("at least one settings profile is required")
	}

	for _, name := range sortedKeys(c.Profiles) {
		if err := validateProfile(name, c.Profiles[name]); err != nil {
			return err
		}
	}

	if len(c.Orgs) == 0 {
		return fmt.Errorf("at least one org is required")
	}

	for _, login := range c.SortedOrgs() {
		if err := c.validateOrg(login, c.Orgs[login]); err != nil {
			return err
		}
	}

	return nil
}

// validateProfile requires a profile to be COMPLETE: every field set, so
// a repo row can never resolve to a half-specified resource.
func validateProfile(name string, p *RepoSettings) error {
	if !slugPattern.MatchString(name) {
		return fmt.Errorf("profile %q: invalid name", name)
	}

	if p == nil {
		return fmt.Errorf("profile %q: empty", name)
	}

	missing := p.missingFields()
	if len(missing) > 0 {
		return fmt.Errorf("profile %q: incomplete, missing %s (profiles must set every field; partial specs belong in a repo's overrides)",
			name, strings.Join(missing, ", "))
	}

	if !validVisibility[*p.Visibility] {
		return fmt.Errorf("profile %q: visibility %q must be public or private", name, *p.Visibility)
	}

	if !validAllowedActions[*p.Actions.AllowedActions] {
		return fmt.Errorf("profile %q: actions.allowed_actions %q invalid", name, *p.Actions.AllowedActions)
	}

	if !validWorkflowPermissions[*p.Actions.DefaultWorkflowPermissions] {
		return fmt.Errorf("profile %q: actions.default_workflow_permissions %q must be read or write", name, *p.Actions.DefaultWorkflowPermissions)
	}

	if n := *p.Protection.RequiredApprovals; n < 0 || n > 6 {
		return fmt.Errorf("profile %q: protection.required_approvals %d out of range 0..6", name, n)
	}

	for team, perm := range p.Teams {
		if !validPermissions[perm] {
			return fmt.Errorf("profile %q: team %q permission %q invalid", name, team, perm)
		}
	}

	return nil
}

// validateOwners checks the owner whitelist is well-formed.
//
// Case matters more here than it looks: GitHub logins are
// case-insensitive but the drift check compares against what the API
// returns, so two spellings of one owner would read as one declared and
// one undeclared forever. Duplicates are rejected for the same reason —
// a login listed twice would declare two resources for one membership.
func validateOwners(login string, owners []string) error {
	seen := make(map[string]bool, len(owners))

	for _, owner := range owners {
		switch {
		case owner == "":
			return fmt.Errorf("org %q: owners contains an empty login", login)
		case !slugPattern.MatchString(owner):
			return fmt.Errorf("org %q: owner %q is not a valid GitHub login", login, owner)
		case seen[strings.ToLower(owner)]:
			return fmt.Errorf("org %q: owner %q listed twice", login, owner)
		}

		seen[strings.ToLower(owner)] = true
	}

	return nil
}

func (c *Config) validateOrg(login string, org *Org) error {
	if !slugPattern.MatchString(login) {
		return fmt.Errorf("org %q: invalid login", login)
	}

	if org == nil {
		return fmt.Errorf("org %q: empty row", login)
	}

	if org.Settings == nil {
		return fmt.Errorf("org %q: settings are required", login)
	}

	if p := org.Settings.DefaultRepositoryPermission; p != "none" && p != "read" && p != "write" && p != "admin" {
		return fmt.Errorf("org %q: default_repository_permission %q must be none, read, write or admin", login, p)
	}

	// The prefix's SHAPE (e.g. a "/secrets/" mirror convention) is the
	// consuming estate's rule, asserted in its own registry tests; the
	// library requires only that an engine credential source exists.
	if org.CredentialsSSMPrefix == "" {
		return fmt.Errorf("org %q: credentials_ssm_prefix is required", login)
	}

	if err := validateOwners(login, org.Owners); err != nil {
		return err
	}

	if a := org.Settings.Actions; a != nil {
		if !validAllowedActions[a.AllowedActions] {
			return fmt.Errorf("org %q: settings.actions.allowed_actions %q invalid", login, a.AllowedActions)
		}

		if e := a.EnabledRepositories; e != "all" && e != "none" && e != "selected" {
			return fmt.Errorf("org %q: settings.actions.enabled_repositories %q must be all, none or selected", login, e)
		}

		if !validWorkflowPermissions[a.DefaultWorkflowPermissions] {
			return fmt.Errorf("org %q: settings.actions.default_workflow_permissions %q must be read or write", login, a.DefaultWorkflowPermissions)
		}
	}

	if err := org.validateTeams(login); err != nil {
		return err
	}

	if err := c.validateRepos(login, org); err != nil {
		return err
	}

	if err := org.validateApps(login); err != nil {
		return err
	}

	return org.validateRunnerGroups(login)
}

// validateTeams checks the team graph: known parents, no cycles, and the
// GitHub rule that a nested team cannot be secret.
func (o *Org) validateTeams(login string) error {
	for _, slug := range o.SortedTeams() {
		team := o.Teams[slug]

		if !slugPattern.MatchString(slug) {
			return fmt.Errorf("org %q team %q: invalid slug", login, slug)
		}

		if team == nil {
			return fmt.Errorf("org %q team %q: empty row", login, slug)
		}

		if team.Privacy != "" && !validPrivacy[team.Privacy] {
			return fmt.Errorf("org %q team %q: privacy %q must be closed or secret", login, slug, team.Privacy)
		}

		if team.Notifications != "" && !validNotifications[team.Notifications] {
			return fmt.Errorf("org %q team %q: notifications %q must be enabled or disabled", login, slug, team.Notifications)
		}

		if team.Parent == "" {
			continue
		}

		if _, ok := o.Teams[team.Parent]; !ok {
			return fmt.Errorf("org %q team %q: unknown parent %q", login, slug, team.Parent)
		}

		if team.Privacy == PrivacySecret {
			return fmt.Errorf("org %q team %q: a nested team cannot be secret (GitHub rule)", login, slug)
		}
	}

	return o.detectTeamCycles(login)
}

func (o *Org) detectTeamCycles(login string) error {
	for _, slug := range o.SortedTeams() {
		seen := map[string]bool{slug: true}

		for cur := o.Teams[slug].Parent; cur != ""; cur = o.Teams[cur].Parent {
			if seen[cur] {
				return fmt.Errorf("org %q team %q: parent cycle through %q", login, slug, cur)
			}

			seen[cur] = true
		}
	}

	return nil
}

func (c *Config) validateRepos(login string, org *Org) error {
	for _, name := range org.SortedRepos() {
		repo := org.Repos[name]

		if !slugPattern.MatchString(name) {
			return fmt.Errorf("org %q repo %q: invalid name", login, name)
		}

		if repo == nil {
			return fmt.Errorf("org %q repo %q: empty row", login, name)
		}

		if _, ok := c.Profiles[repo.Profile]; !ok {
			return fmt.Errorf("org %q repo %q: unknown profile %q", login, name, repo.Profile)
		}

		if repo.Overrides != nil && repo.Reason == "" {
			return fmt.Errorf("org %q repo %q: overrides require a reason (which keep-or-fix decision made this deviation deliberate)", login, name)
		}

		if repo.Overrides != nil && len(repo.Overrides.Teams) > 0 {
			return fmt.Errorf("org %q repo %q: overrides.teams is not a thing — repo-level grants go in the repo's own teams map", login, name)
		}

		for team, perm := range repo.Teams {
			if _, ok := org.Teams[team]; !ok {
				return fmt.Errorf("org %q repo %q: grant to unknown team %q", login, name, team)
			}

			if !validPermissions[perm] {
				return fmt.Errorf("org %q repo %q: team %q permission %q invalid", login, name, team, perm)
			}
		}

		// Profile team grants must also reference teams that exist.
		for team := range c.Profiles[repo.Profile].Teams {
			if _, ok := org.Teams[team]; !ok {
				return fmt.Errorf("org %q repo %q: profile %q grants to team %q, which this org does not have",
					login, name, repo.Profile, team)
			}
		}

		if repo.ChecksWaived != "" {
			profileChecks := c.Profiles[repo.Profile].Protection
			if profileChecks == nil || profileChecks.RequiredChecks == nil || len(*profileChecks.RequiredChecks) == 0 {
				return fmt.Errorf("org %q repo %q: checks_waived, but profile %q requires no checks — the waiver has outlived its cause and should be deleted",
					login, name, repo.Profile)
			}
		}

		if err := validateTagRulesets(login, name, org, repo); err != nil {
			return err
		}

		if err := validateBranchRulesets(login, name, repo); err != nil {
			return err
		}

		// An archived repo declares nothing but the repository itself,
		// so anything that only shapes the resources it no longer has is
		// inert. Silently-inert config is how a row comes to mean
		// something it does not, so say so instead of ignoring it.
		if repo.Archived {
			if repo.Overrides != nil {
				return fmt.Errorf("org %q repo %q: archived, so overrides do nothing — GitHub rejects settings"+
					" writes on an archived repository and the engine ignores every settings input."+
					" Delete the overrides, or unarchive", login, name)
			}

			if repo.ChecksWaived != "" {
				return fmt.Errorf("org %q repo %q: archived, so checks_waived does nothing — no branch"+
					" protection is declared for an archived repository. Delete the waiver, or unarchive", login, name)
			}

			if len(repo.TagRulesets) > 0 {
				return fmt.Errorf("org %q repo %q: archived, so tag_rulesets do nothing — an archived"+
					" repository is read-only and takes no tag pushes to restrict. Delete them, or unarchive", login, name)
			}

			// Protection below is resolved from the profile and never
			// declared for this repo, so the enforces-anything check
			// would judge a rule that does not exist.
			continue
		}

		// Protection that enforces nothing at all is indistinguishable
		// from no protection, but much harder to notice. Blocking force
		// pushes or deletions counts as enforcement — a rule can be
		// meaningful without gating merges, which is exactly the shape a
		// repo lands in while its checks are waived.
		prot := c.Resolve(repo).Protection
		if len(prot.PullRequestBypassers) > 0 && prot.RequiredApprovals == 0 {
			return fmt.Errorf("org %q repo %q: pull_request_bypassers with required_approvals 0 —"+
				" there is no review requirement to bypass; delete the bypassers or require approvals", login, name)
		}

		if prot.Enabled && !protectionEnforcesAnything(prot) {
			return fmt.Errorf("org %q repo %q: protection is enabled but enforces nothing — add a requirement, or set protection.enabled: false", login, name)
		}
	}

	return nil
}

// validateTagRulesets checks one repo's tag_rulesets rows: named,
// tag-scoped, and bypassable by at least one existing team — a ruleset
// nobody can bypass does not protect the namespace, it bricks it.
func validateTagRulesets(login, name string, org *Org, repo *Repo) error {
	seen := make(map[string]bool, len(repo.TagRulesets))

	for i, rs := range repo.TagRulesets {
		if rs == nil || rs.Name == "" {
			return fmt.Errorf("org %q repo %q: tag_rulesets[%d]: name is required", login, name, i)
		}

		if seen[rs.Name] {
			return fmt.Errorf("org %q repo %q: tag ruleset %q declared twice", login, name, rs.Name)
		}

		seen[rs.Name] = true

		if !strings.HasPrefix(rs.Pattern, "refs/tags/") {
			return fmt.Errorf("org %q repo %q: tag ruleset %q: pattern %q must start with refs/tags/",
				login, name, rs.Name, rs.Pattern)
		}

		if len(rs.BypassTeams) == 0 {
			return fmt.Errorf("org %q repo %q: tag ruleset %q: at least one bypass team is required —"+
				" with none, nobody can ever create a matching tag", login, name, rs.Name)
		}

		for _, team := range rs.BypassTeams {
			if _, ok := org.Teams[team]; !ok {
				return fmt.Errorf("org %q repo %q: tag ruleset %q: bypass team %q is not in this org's teams",
					login, name, rs.Name, team)
			}
		}
	}

	return nil
}

// validateBranchRulesets checks one repo's branch_rulesets rows: named,
// enforcing something, and bypassable — without a bypass the gate
// belongs in classic protection (see the BranchRuleset field comment).
func validateBranchRulesets(login, name string, repo *Repo) error {
	for i, rs := range repo.BranchRulesets {
		switch {
		case rs == nil || rs.Name == "":
			return fmt.Errorf("org %q repo %q: branch_rulesets[%d]: name is required", login, name, i)
		case rs.Pattern == "":
			return fmt.Errorf("org %q repo %q: branch ruleset %q: pattern is required", login, name, rs.Name)
		case rs.RequiredApprovals <= 0:
			return fmt.Errorf("org %q repo %q: branch ruleset %q: required_approvals must be positive —"+
				" a ruleset enforcing nothing is noise", login, name, rs.Name)
		case len(rs.BypassApps) == 0 && !rs.BypassOrgAdmins:
			return fmt.Errorf("org %q repo %q: branch ruleset %q: a bypass is required (bypass_apps"+
				" or bypass_org_admins) — without one this belongs in classic protection", login, name, rs.Name)
		}
	}

	return nil
}

// protectionEnforcesAnything reports whether a rule actually restricts
// anything at all. Blocking force pushes or deletions counts: a rule can
// be meaningful without gating merges, which is exactly the shape a repo
// lands in while its required checks are waived (INF-410).
func protectionEnforcesAnything(p ResolvedProtection) bool {
	return len(p.RequiredChecks) > 0 ||
		p.RequiredApprovals > 0 ||
		p.EnforceAdmins ||
		!p.AllowForcePushes ||
		!p.AllowDeletions ||
		p.RequireConvResolution ||
		p.RequireLinearHistory ||
		p.RequireSignatures
}

func (o *Org) validateApps(login string) error {
	prefix := o.appPrefix(login)

	for _, name := range o.SortedApps() {
		app := o.Apps[name]

		if !slugPattern.MatchString(name) {
			return fmt.Errorf("org %q app %q: invalid name", login, name)
		}

		if app == nil {
			return fmt.Errorf("org %q app %q: empty row", login, name)
		}

		if !validInstallScope[app.Install] {
			return fmt.Errorf("org %q app %q: install %q must be all or selected", login, name, app.Install)
		}

		if app.Install == InstallAll && len(app.Repos) > 0 {
			return fmt.Errorf("org %q app %q: install: all cannot list repos", login, name)
		}

		for _, repo := range app.Repos {
			if _, ok := o.Repos[repo]; !ok {
				return fmt.Errorf("org %q app %q: scoped to repo %q, which is not an in-scope row", login, name, repo)
			}
		}

		for perm, level := range app.Permissions {
			if !validAppPermissions[perm] {
				return fmt.Errorf("org %q app %q: unknown permission %q", login, name, perm)
			}

			if !validAppPermissionLevels[level] {
				return fmt.Errorf("org %q app %q: permission %q level %q must be read, write or admin", login, name, perm, level)
			}
		}

		if err := validateAppOwnership(login, name, prefix, app); err != nil {
			return err
		}
	}

	return nil
}

// validateAppOwnership enforces the split between Apps we author (must
// be prefixed, must have credentials) and vendor Apps (must not).
func validateAppOwnership(login, name, prefix string, app *App) error {
	if app.External {
		if app.Adopted {
			return fmt.Errorf("org %q app %q: external and adopted are mutually exclusive", login, name)
		}

		if app.Credentials != nil {
			return fmt.Errorf("org %q app %q: external Apps have no credentials of ours", login, name)
		}

		if app.AppID == 0 || app.InstallationID == 0 {
			return fmt.Errorf("org %q app %q: external rows must record app_id and installation_id (they exist; we only inventory them)", login, name)
		}

		return nil
	}

	if !strings.HasPrefix(name, prefix) {
		return fmt.Errorf("org %q app %q: our Apps must be prefixed %q — App display names are globally unique on GitHub", login, name, prefix)
	}

	if app.Adopted {
		if app.AppID == 0 || app.InstallationID == 0 {
			return fmt.Errorf("org %q app %q: adopted rows must record app_id and installation_id (the App already exists)", login, name)
		}

		if app.Note == "" {
			return fmt.Errorf("org %q app %q: adopted rows need a note saying what still has to happen (credential normalization)", login, name)
		}

		return nil
	}

	if app.Credentials == nil {
		return fmt.Errorf("org %q app %q: credentials (op_item + ssm_prefix) are required for Apps we author", login, name)
	}

	if app.Credentials.OpItem == "" {
		return fmt.Errorf("org %q app %q: credentials.op_item is required", login, name)
	}

	// The mirror path's SHAPE (e.g. a "/secrets/" convention) is the
	// consuming estate's rule, asserted in its own registry tests.

	if app.URL == "" {
		return fmt.Errorf("org %q app %q: url is required — the manifest flow needs a homepage URL", login, name)
	}

	return nil
}

func (o *Org) validateRunnerGroups(login string) error {
	for _, name := range sortedKeys(o.RunnerGroups) {
		group := o.RunnerGroups[name]

		if group == nil {
			return fmt.Errorf("org %q runner group %q: empty row", login, name)
		}

		if !validRunnerVisibility[group.Visibility] {
			return fmt.Errorf("org %q runner group %q: visibility %q must be all, selected or private", login, name, group.Visibility)
		}

		if group.Visibility != RunnerVisibilitySelected && len(group.Repos) > 0 {
			return fmt.Errorf("org %q runner group %q: only visibility: selected may list repos", login, name)
		}

		for _, repo := range group.Repos {
			if _, ok := o.Repos[repo]; !ok {
				return fmt.Errorf("org %q runner group %q: targets repo %q, which is not an in-scope row", login, name, repo)
			}
		}

		if group.RestrictedToWorkflows && len(group.SelectedWorkflows) == 0 {
			return fmt.Errorf("org %q runner group %q: restricted_to_workflows needs selected_workflows", login, name)
		}
	}

	return nil
}

// appPrefix returns the mandatory prefix for Apps we author in this org.
func (o *Org) appPrefix(login string) string {
	if o.AppPrefix != "" {
		return o.AppPrefix
	}

	return login + "-"
}

// AppPrefixFor returns the App name prefix for the named org.
func (c *Config) AppPrefixFor(login string) string {
	org, ok := c.Orgs[login]
	if !ok {
		return login + "-"
	}

	return org.appPrefix(login)
}
