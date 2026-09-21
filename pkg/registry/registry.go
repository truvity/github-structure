// Package registry loads and validates the GitHub plane registry — one
// YAML file (github.yaml at the root of the fs.FS handed to Load)
// describing every org the structure engine owns: org settings, teams,
// repos (a settings profile plus per-repo overrides with reasons), and
// the Actions runner groups ARC registers scale sets against.
//
// The registry is DESIRED STATE for everything Pulumi can apply and
// INVENTORY for the few things it cannot. GitHub Apps are neither: an
// App's existence, its key and its installation belong to whoever holds
// that key, so the registry names an App where a rule references one
// (ruleset bypass actors) and resolves the name against the
// organization's LIVE installations at deploy — see InstalledApps.
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

	"go.yaml.in/yaml/v3"
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

// Review values — the default branch's REVIEW gate, the one field that
// decides how a pull request becomes a merge. Everything else about the
// gate (which contexts must pass) stays in `protection`.
const (
	// ReviewNone asks for no approving review: the default branch is
	// gated by its required checks alone, and an automated pull request
	// completes on green through GitHub's own auto-merge.
	ReviewNone = "none"
	// ReviewRequired asks for one approving review — from a human or
	// from an App with pull_requests: write, both of which GitHub counts
	// towards reviewDecision. It is rendered as a RULESET rather than
	// classic protection because only a ruleset can carry the
	// organization-admin bypass (see BranchRuleset.BypassOrgAdmins).
	ReviewRequired = "required"
	// ReviewRulesetName is the display name of the ruleset `required`
	// renders. It is the name the estate's hand-written approval
	// rulesets already carry, on purpose: the engine keys a ruleset
	// resource on repo + name, so adopting the field UPDATES the live
	// ruleset instead of replacing it — and a ruleset replacement has a
	// window with no gate at all.
	ReviewRulesetName = "pr-approval"
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

	validReview = map[string]bool{
		ReviewNone: true, ReviewRequired: true,
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

	validRunnerVisibility = map[string]bool{
		RunnerVisibilityAll: true, RunnerVisibilitySelected: true, RunnerVisibilityPrivate: true,
	}
)

type (
	// Config is the whole registry.
	Config struct {
		// Presets are the shared repo SETTINGS vocabulary — how a repo
		// behaves and how its branches are protected. Every repo row names
		// exactly one; per-repo overrides layer on top. Presets carry no
		// team grants: see Access.
		Presets map[string]*RepoSettings `yaml:"presets"`
		// Access is the shared GRANTS vocabulary, deliberately a separate
		// axis from Presets. Bundling the two meant a repo inherited its
		// grants from whichever settings profile it named, so changing a
		// repo's protection policy silently changed who could read it —
		// collapsing two profiles that differed only in protection would
		// have handed marketing and a product team access to the board's
		// repository. Split, that class of mistake is unavailable.
		Access map[string]map[string]string `yaml:"access"`
		// Orgs maps a GitHub org login to its desired structure.
		Orgs map[string]*Org `yaml:"orgs"`
	}

	// Org is one GitHub organization.
	Org struct {
		// Company links this org to a cfg/companies.yaml row. Truvity is
		// the incumbent and has no code there; other orgs must name one.
		Company string `yaml:"company,omitempty"`
		// EngineCredentials says WHERE the credentials of the App this
		// engine acts as, for this org, are kept — the caller reads
		// them from there and hands engine.Credentials to Deploy. It is
		// per-org because each org has its own engine App, and those
		// Apps need not live in the same store: an estate moving from
		// one secret store to another moves one org at a time, and the
		// half-migrated state has to be declarable.
		EngineCredentials *EngineCredentials `yaml:"engine_credentials"`
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
		// DefaultAccess are the access bundles every repository in this
		// org gets unless its row says otherwise. It exists because an
		// estate-wide grant written per row is fifty copies of one
		// decision: trust-form grants the same three teams push on all
		// but one of its repositories, and fifty copies can drift while
		// one cannot.
		//
		// A row OPTS OUT with an explicit empty list (`access: []`), and
		// overrides by naming its own bundles. Absent means inherit —
		// which is why this is applied at load: everything downstream
		// (resolution, entitlements, drift) then sees one effective row,
		// and the resolved state still shows every grant per repository,
		// so the inheritance is readable where it matters.
		DefaultAccess []string `yaml:"default_access,omitempty"`

		Teams map[string]*Team `yaml:"teams"`
		// Repos are the in-scope repositories, keyed by name.
		Repos map[string]*Repo `yaml:"repos"`
		// RunnerGroups are the Actions runner groups ARC targets.
		RunnerGroups map[string]*RunnerGroup `yaml:"runner_groups,omitempty"`
		// SecurityConfigurations is INVENTORY, never desired state: the
		// provider has no resource for code security configurations, and
		// an enforced one makes GitHub reject per-repository writes to
		// the settings it covers. Recorded so the registry still answers
		// "who owns Dependabot alerts". See
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
		// Secrets are org-level Actions secrets, declared by NAME and
		// scope only — values live in 1Password and reach GitHub by
		// hand or by the secrets-mirror, never through this registry.
		// Declaring them exists for the scope: a selected-visibility
		// secret whose repository list is hand-kept is the entitlement
		// dead zone (INF-580 — renovate silently never ran on seven
		// repos because nothing owned the list).
		Secrets map[string]*OrgSecret `yaml:"secrets,omitempty"`
	}

	// OrgSecret is one org-level Actions secret: name and scope, no value.
	OrgSecret struct {
		// Visibility is private | selected | all, defaulting to PRIVATE.
		Visibility string `yaml:"visibility,omitempty"`
		// Scope declares the selected-repo membership (visibility
		// `selected` only).
		Scope *EntitlementScope `yaml:"scope,omitempty"`
	}

	// EntitlementScope is a DERIVED selected-repo set: a profile-wide
	// rule plus explicit additions, never a hand-kept full list. New
	// repos matching the rule are entitled at birth by the next
	// reconcile; hand mutations show up as drift.
	EntitlementScope struct {
		// DerivePreset entitles every non-archived repo carrying this
		// profile (e.g. "public").
		DerivePreset string `yaml:"derive_preset,omitempty"`
		// Repos are explicit additions beyond the rule.
		Repos []string `yaml:"repos,omitempty"`
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
		// Scope declares the selected-repo membership (visibility
		// `selected` only) — derived + explicit, reconciled by
		// idempotent PUTs and drift-checked (INF-580).
		Scope *EntitlementScope `yaml:"scope,omitempty"`
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
		// Preset names a key in Config.Presets.
		Preset string `yaml:"preset"`
		// Access names keys in Config.Access. Effective grants are the
		// union of these bundles, with the repo's own Teams winning on
		// conflict. A repo that should not have a grant simply does not
		// list the bundle — removal is expressible, which it was not when
		// grants rode on the settings profile.
		Access []string `yaml:"access,omitempty"`
		// Description is the repo blurb; empty means unmanaged.
		Description string `yaml:"description,omitempty"`
		// Teams are grants ADDED to this repo's access bundles, or upgrades
		// of one. Removal is expressed by not listing the bundle, which is
		// the whole point of splitting access from presets: it used to be
		// inexpressible, and that forced sensitive repositories onto
		// bespoke profiles whose real purpose was the grant list.
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
		// Review is this repository's review gate — `none` or
		// `required` — overriding the preset's. Empty inherits the
		// preset, and a preset that states neither leaves the approval
		// knobs in `protection` where they were hand-spelled: the
		// pre-review shape, still supported for the one gate the
		// two-value vocabulary cannot say (named human bypassers, which
		// only classic protection can express).
		//
		// It sits on the ROW, not in `overrides:`, because it is not a
		// settings deviation needing a keep-or-fix justification. Which
		// repositories require a reviewed merge is a first-class fact
		// about the repository; demanding a `reason:` for each would
		// bury the one field that decides merge mechanics in prose.
		Review string `yaml:"review,omitempty"`
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
		// RequiredChecks are status-check context names the ruleset
		// requires. Unlike classic protection's required_checks, a
		// ruleset's bypass_actors DO skip these — the one way to let a
		// specific App direct-push without CI (the Kargo promotion
		// writer, gitops). Leave empty to require no checks.
		RequiredChecks []string `yaml:"required_checks,omitempty"`
		// BypassApps are the slugs of Apps allowed to bypass — how an
		// automation merges its own green PRs. Each name is resolved
		// against the Apps INSTALLED on the same org, by
		// InstalledApps.BypassAppIDs, into the GitHub App database id
		// the REST API wants.
		//
		// Names, not ids, because an id is GitHub's fact about an App,
		// not the registry's. Written out here it is hand-copied,
		// unreadable, and stale the moment an App is recreated — and a
		// stale bypass does not announce itself, it refuses one release
		// act.
		BypassApps []string `yaml:"bypass_apps"`
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
		// BypassApps are the slugs of Apps allowed the same tag acts as
		// Integration actors — how a scheduled auto-release cuts its tag
		// without a human in the release team. Additive to BypassTeams,
		// which stays required: an App key can rotate away, a team
		// cannot.
		//
		// Same vocabulary as the branch ruleset's field: an App slug,
		// resolved against the org's live installations.
		BypassApps []string `yaml:"bypass_apps,omitempty"`
		// BypassOrgAdmins additionally lets org admins act on matching
		// tags — the audit-visible break-glass, same semantics as the
		// branch-ruleset field (OrganizationAdmin actor, id 0: GitHub
		// ignores the documented 1 on write and returns 0 on read, so
		// 0 is the only drift-free spelling).
		BypassOrgAdmins bool `yaml:"bypass_org_admins,omitempty"`
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

		// Review is the preset's review gate (see Repo.Review), and the
		// only optional field a preset may leave out: a class of
		// repositories whose gate is still hand-spelled in `protection`
		// has nothing truthful to say here, and a preset forced to
		// guess would state a gate the engine then renders. Repos
		// deviate on the ROW (`review:`), never in `overrides:`.
		Review *string `yaml:"review,omitempty"`
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

	// EngineCredentials is one org's engine-App credential source:
	// EXACTLY ONE of the fields below. Only the LOCATION is declared —
	// what the three credential values are called inside it is the
	// estate's convention (for SSM, the field names under FieldAppID and
	// friends), because the thing that writes them is the estate's too.
	EngineCredentials struct {
		// SSMPrefix is a prefix in an AWS-SSM-shaped store holding one
		// parameter per credential field.
		SSMPrefix string `yaml:"ssm_prefix,omitempty"`
		// OpenBAO is one KV v2 secret holding all three credential
		// values as properties.
		OpenBAO *OpenBAOSecret `yaml:"openbao,omitempty"`
	}

	// OpenBAOSecret locates one secret in an OpenBAO (or Vault) KV v2
	// mount. Nothing here is a credential: it is a path.
	OpenBAOSecret struct {
		// Namespace is the OpenBAO namespace the mount lives in. Empty
		// means root.
		Namespace string `yaml:"namespace,omitempty"`
		// Mount is the KV v2 mount path.
		Mount string `yaml:"mount"`
		// Path is the secret's path inside the mount, with no `data/`
		// segment: that is the KV v2 API's, not the secret's.
		Path string `yaml:"path"`
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

// InstalledApps is one organization's App installations, as slug → App
// database id: the answer GET /orgs/{org}/installations gives.
//
// It is the registry's only App vocabulary. The library used to carry an
// `apps:` inventory whose load-bearing job was turning a ruleset's App
// NAME into the database id the REST API wants — an id hand-copied into
// a file, stale the moment an App was recreated, and stale silently.
// Live GitHub knows the same fact and cannot be out of date about it, so
// the caller reads it there (pkg/app does, both as the engine App and
// through the operator's CLI) and hands it to BypassAppIDs.
type InstalledApps map[string]int

// The field-name convention an SSM-shaped engine credential follows:
// one parameter per name under EngineCredentials.SSMPrefix. Nothing here
// creates or writes those parameters — whoever holds the App key does
// that — but the engine has to know what to read.
const (
	FieldAppID          = "github-app-id"
	FieldInstallationID = "github-installation-id"
	FieldPrivateKey     = "github-private-key"
)

// Load reads github.yaml from the given filesystem and validates it.
func Load(fsys fs.FS) (*Config, error) {
	var c Config
	if err := load(fsys, "github.yaml", &c); err != nil {
		// The loader is strict, so a retired key fails here as an
		// anonymous "field not found in type". A reader who wrote
		// `apps:` needs the forwarding address, not the decoder's
		// vocabulary — so say where Apps went before handing the raw
		// error back.
		if orgs := orgsDeclaringApps(fsys); len(orgs) > 0 {
			return nil, fmt.Errorf("github.yaml: org %s: the App vocabulary (`apps:`, `app_prefix:`)"+
				" is no longer part of the registry. An App's existence, its key and its"+
				" installation belong to the credential custodian's catalogue; a ruleset's"+
				" bypass_apps names the App, and the id is resolved from the organization's"+
				" LIVE installations at deploy. Delete the block: %w",
				strings.Join(orgs, ", "), err)
		}

		return nil, err
	}

	c.applyDefaultAccess()

	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("github.yaml: %w", err)
	}

	return &c, nil
}

// applyDefaultAccess gives every repository its org's default bundles
// unless its row decided for itself. Before validation, so an inherited
// bundle name is checked exactly like a written one.
//
// Absent (nil) inherits; an explicit empty list opts out. That asymmetry
// is the whole feature: a row that says nothing takes the estate's
// decision, and a row that means "none" has to say so — which is a
// sentence somebody wrote, not a key somebody forgot.
func (c *Config) applyDefaultAccess() {
	for _, login := range c.SortedOrgs() {
		org := c.Orgs[login]
		if len(org.DefaultAccess) == 0 {
			continue
		}

		for _, name := range org.SortedRepos() {
			if repo := org.Repos[name]; repo.Access == nil {
				repo.Access = org.DefaultAccess
			}
		}
	}
}

// orgsDeclaringApps re-reads the file leniently to name the orgs that
// still carry the retired App vocabulary. Only ever called once a strict
// decode has already failed, so the second parse costs nothing in the
// normal case — and an unreadable or unparseable file simply yields no
// names, leaving the original error to speak.
func orgsDeclaringApps(fsys fs.FS) []string {
	data, err := fs.ReadFile(fsys, "github.yaml")
	if err != nil {
		return nil
	}

	var probe struct {
		Orgs map[string]struct {
			Apps      map[string]any `yaml:"apps"`
			AppPrefix string         `yaml:"app_prefix"`
		} `yaml:"orgs"`
	}

	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil
	}

	var orgs []string

	for _, login := range sortedKeys(probe.Orgs) {
		if org := probe.Orgs[login]; org.Apps != nil || org.AppPrefix != "" {
			orgs = append(orgs, login)
		}
	}

	return orgs
}

// Validate enforces every registry invariant.
func (c *Config) Validate() error {
	if len(c.Presets) == 0 {
		return fmt.Errorf("at least one settings profile is required")
	}

	for _, bundle := range sortedKeys(c.Access) {
		for team, perm := range c.Access[bundle] {
			if !validPermissions[perm] {
				return fmt.Errorf("access bundle %q: team %q permission %q invalid", bundle, team, perm)
			}
		}
	}

	for _, name := range sortedKeys(c.Presets) {
		if err := validatePreset(name, c.Presets[name]); err != nil {
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

// validatePreset checks a preset's VALUES. It no longer demands that a
// preset set every field: a preset is a diff against gitHubDefaults, and
// completeness is a property of the resolved state, which base.go
// guarantees and TestGitHubDefaultsAreComplete pins.
//
// What is still refused is a value that cannot mean what it says, and
// the combinations the engine would obey silently.
func validatePreset(name string, p *RepoSettings) error {
	if !slugPattern.MatchString(name) {
		return fmt.Errorf("preset %q: invalid name", name)
	}

	if p == nil {
		return fmt.Errorf("preset %q: empty", name)
	}

	// Effective values, so a check below reads what the preset MEANS
	// whether it stated a field or inherited it. `p` stays the preset
	// itself: overlay carries settings and not `review`, which is this
	// registry's own vocabulary rather than a GitHub setting, so reading
	// review off the merged copy would skip its validation entirely.
	eff := *gitHubDefaults()
	eff.overlay(p)

	if !validVisibility[*eff.Visibility] {
		return fmt.Errorf("preset %q: visibility %q must be public or private", name, *eff.Visibility)
	}

	if !validAllowedActions[*eff.Actions.AllowedActions] {
		return fmt.Errorf("preset %q: actions.allowed_actions %q invalid", name, *eff.Actions.AllowedActions)
	}

	if !validWorkflowPermissions[*eff.Actions.DefaultWorkflowPermissions] {
		return fmt.Errorf("preset %q: actions.default_workflow_permissions %q must be read or write", name, *eff.Actions.DefaultWorkflowPermissions)
	}

	if n := *eff.Protection.RequiredApprovals; n < 0 || n > 6 {
		return fmt.Errorf("preset %q: protection.required_approvals %d out of range 0..6", name, n)
	}

	if p.Review != nil {
		if !validReview[*p.Review] {
			return fmt.Errorf("preset %q: review %q must be %s or %s", name, *p.Review, ReviewNone, ReviewRequired)
		}

		// One spelling per gate. `review` owns the approval half of the
		// default-branch rule; leaving a preset's classic approval
		// block set too would make the file say two things and the
		// engine obey one of them silently.
		if *eff.Protection.RequiredApprovals != 0 {
			return fmt.Errorf("preset %q: review %q with protection.required_approvals %d —"+
				" review owns the approval gate; set required_approvals to 0",
				name, *p.Review, *eff.Protection.RequiredApprovals)
		}
	}

	return nil
}

// validateEntitlementScope keeps a scope honest: it may only ride
// selected visibility, its rule must name a declared profile, and its
// explicit additions must be repos this org declares (an entitlement
// for a repo that does not exist is a typo hiding a dead grant).
func (c *Config) validateEntitlementScope(login string, org *Org, subject, visibility string, s *EntitlementScope) error {
	if s == nil {
		return nil
	}

	if visibility != "selected" {
		return fmt.Errorf("org %q: %s: scope requires visibility selected (got %q)", login, subject, visibility)
	}

	if s.DerivePreset != "" {
		if _, ok := c.Presets[s.DerivePreset]; !ok {
			return fmt.Errorf("org %q: %s: scope derive_preset %q names no declared preset", login, subject, s.DerivePreset)
		}
	}

	for _, r := range s.Repos {
		if _, ok := org.Repos[r]; !ok {
			return fmt.Errorf("org %q: %s: scope repo %q is not declared", login, subject, r)
		}
	}

	if s.DerivePreset == "" && len(s.Repos) == 0 {
		return fmt.Errorf("org %q: %s: scope declares neither a rule nor repos", login, subject)
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

// validateEngineCredentials refuses an org whose engine App credentials
// are nowhere, or in two places at once. Which of the two an org uses is
// a fact about that org and nothing else reads it, so a caller can
// switch one org's source without touching the other's.
func validateEngineCredentials(login string, creds *EngineCredentials) error {
	switch {
	case creds == nil:
		return fmt.Errorf("org %q: engine_credentials is required: the engine acts as an App, and the caller has to know where its credentials are", login)
	case creds.SSMPrefix == "" && creds.OpenBAO == nil:
		return fmt.Errorf("org %q: engine_credentials names no source: set ssm_prefix or openbao", login)
	case creds.SSMPrefix != "" && creds.OpenBAO != nil:
		return fmt.Errorf("org %q: engine_credentials names both ssm_prefix and openbao: exactly one, so a read cannot pick the stale one", login)
	case creds.OpenBAO != nil && creds.OpenBAO.Mount == "":
		return fmt.Errorf("org %q: engine_credentials.openbao.mount is required", login)
	case creds.OpenBAO != nil && creds.OpenBAO.Path == "":
		return fmt.Errorf("org %q: engine_credentials.openbao.path is required", login)
	case creds.OpenBAO != nil && strings.Contains(creds.OpenBAO.Path, "/data/"):
		return fmt.Errorf("org %q: engine_credentials.openbao.path %q carries the KV v2 API's `data/` segment: name the secret, not the endpoint",
			login, creds.OpenBAO.Path)
	}

	return nil
}

// Describe names an engine credential source for an error message or a
// log line: the kind and the location, never a value.
func (e *EngineCredentials) Describe() string {
	switch {
	case e == nil:
		return "no declared source"
	case e.OpenBAO != nil:
		return "openbao " + e.OpenBAO.String()
	default:
		return "ssm " + e.SSMPrefix
	}
}

// String is the secret's location, written the way an operator would
// look it up: namespace, mount, path.
func (s *OpenBAOSecret) String() string {
	namespace := s.Namespace
	if namespace == "" {
		namespace = "root"
	}

	return namespace + "/" + s.Mount + "/" + s.Path
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

	// Checked here and not only where it is inherited: a default every
	// row happens to override today is still a typo, and one nobody
	// notices until the row that would have used it is added.
	for _, bundle := range org.DefaultAccess {
		if _, ok := c.Access[bundle]; !ok {
			return fmt.Errorf("org %q: default_access %q names no declared access bundle", login, bundle)
		}
	}

	if p := org.Settings.DefaultRepositoryPermission; p != "none" && p != "read" && p != "write" && p != "admin" {
		return fmt.Errorf("org %q: default_repository_permission %q must be none, read, write or admin", login, p)
	}

	if org.Settings.Actions != nil {
		for _, name := range sortedKeys(org.Settings.Actions.Variables) {
			if err := c.validateEntitlementScope(login, org, "variable "+name,
				org.Settings.Actions.Variables[name].Visibility,
				org.Settings.Actions.Variables[name].Scope); err != nil {
				return err
			}
		}

		for _, name := range sortedKeys(org.Settings.Actions.Secrets) {
			s := org.Settings.Actions.Secrets[name]
			if v := s.Visibility; v != "" && v != "private" && v != "selected" && v != "all" {
				return fmt.Errorf("org %q: secret %s: visibility %q must be private, selected or all", login, name, v)
			}

			if err := c.validateEntitlementScope(login, org, "secret "+name, s.Visibility, s.Scope); err != nil {
				return err
			}
		}
	}

	// The location's SHAPE (e.g. a "/secrets/" mirror convention, or
	// which namespace an estate keeps App keys in) is the consuming
	// estate's rule, asserted in its own registry tests; the library
	// requires only that exactly one engine credential source exists,
	// so a read can never silently pick the wrong one.
	if err := validateEngineCredentials(login, org.EngineCredentials); err != nil {
		return err
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

		if _, ok := c.Presets[repo.Preset]; !ok {
			return fmt.Errorf("org %q repo %q: unknown preset %q", login, name, repo.Preset)
		}

		if repo.Overrides != nil && repo.Reason == "" {
			return fmt.Errorf("org %q repo %q: overrides require a reason (which keep-or-fix decision made this deviation deliberate)", login, name)
		}

		if repo.Review != "" && !validReview[repo.Review] {
			return fmt.Errorf("org %q repo %q: review %q must be %s or %s", login, name, repo.Review, ReviewNone, ReviewRequired)
		}

		// `review` is a row field, never an override: an override
		// carries a reason and merges into the settings, and a second
		// place to say the same thing is how the two drift apart.
		if repo.Overrides != nil && repo.Overrides.Review != nil {
			return fmt.Errorf("org %q repo %q: review belongs on the repository row, not in overrides —"+
				" write `review: %s` next to `preset:`", login, name, *repo.Overrides.Review)
		}

		for team, perm := range repo.Teams {
			if _, ok := org.Teams[team]; !ok {
				return fmt.Errorf("org %q repo %q: grant to unknown team %q", login, name, team)
			}

			if !validPermissions[perm] {
				return fmt.Errorf("org %q repo %q: team %q permission %q invalid", login, name, team, perm)
			}
		}

		// Access bundles must exist, and must grant only to teams the org
		// actually has.
		for _, bundle := range repo.Access {
			grants, ok := c.Access[bundle]
			if !ok {
				return fmt.Errorf("org %q repo %q: unknown access bundle %q", login, name, bundle)
			}

			for team, perm := range grants {
				if _, ok := org.Teams[team]; !ok {
					return fmt.Errorf("org %q repo %q: access bundle %q grants to team %q, which this org does not have",
						login, name, bundle, team)
				}

				if !validPermissions[perm] {
					return fmt.Errorf("org %q repo %q: access bundle %q gives team %q invalid permission %q",
						login, name, bundle, team, perm)
				}
			}
		}

		if repo.ChecksWaived != "" {
			profileChecks := c.Presets[repo.Preset].Protection
			if profileChecks == nil || profileChecks.RequiredChecks == nil || len(*profileChecks.RequiredChecks) == 0 {
				return fmt.Errorf("org %q repo %q: checks_waived, but preset %q requires no checks — the waiver has outlived its cause and should be deleted",
					login, name, repo.Preset)
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

			if repo.Review != "" {
				return fmt.Errorf("org %q repo %q: archived, so review does nothing — an archived repository"+
					" is read-only and takes no pull requests. Delete the field, or unarchive", login, name)
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
		resolved := c.Resolve(repo)

		if err := c.validateReview(login, name, repo, resolved); err != nil {
			return err
		}

		prot := resolved.Protection
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

// validateReview keeps the review gate to ONE spelling per repository.
//
// The field exists so a reader can answer "how does a pull request
// become a merge here?" from one line. That only holds while the older
// spellings — a classic approval block, a hand-written approval ruleset
// — cannot sit next to it saying something else.
func (c *Config) validateReview(login, name string, repo *Repo, resolved Resolved) error {
	if resolved.Review == "" {
		return nil
	}

	// Resolve has already zeroed the classic approval block for a
	// reviewed repo, so the DECLARED value is what has to be judged:
	// otherwise a row that says `review: none` on a preset requiring an
	// approval would quietly drop that approval and read as intended.
	if approvals, bypassers := c.declaredApprovals(repo); resolved.Protection.Enabled {
		if approvals != 0 {
			return fmt.Errorf("org %q repo %q: review %q with protection.required_approvals %d —"+
				" review owns the approval gate; set required_approvals to 0", login, name,
				resolved.Review, approvals)
		}

		if len(bypassers) > 0 {
			return fmt.Errorf("org %q repo %q: review %q with protection.pull_request_bypassers %v —"+
				" review owns the approval gate, and it has no bypass list; delete them", login, name,
				resolved.Review, bypassers)
		}
	}

	for _, rs := range repo.BranchRulesets {
		if rs == nil {
			continue
		}

		if rs.RequiredApprovals > 0 {
			return fmt.Errorf("org %q repo %q: review %q and branch ruleset %q both require approvals —"+
				" delete the ruleset row; `review: %s` renders it", login, name,
				resolved.Review, rs.Name, ReviewRequired)
		}

		if rs.Name == ReviewRulesetName {
			return fmt.Errorf("org %q repo %q: branch ruleset %q collides with the one `review` renders —"+
				" rename it, or drop it and let review own the gate", login, name, rs.Name)
		}
	}

	return nil
}

// declaredApprovals is the classic approval block a repo row DECLARES —
// its preset's, with its own override on top — before Resolve answers
// for it on behalf of `review`.
func (c *Config) declaredApprovals(repo *Repo) (int, []string) {
	var (
		approvals int
		bypassers []string
	)

	if preset := c.Presets[repo.Preset]; preset != nil && preset.Protection != nil {
		approvals = derefInt(preset.Protection.RequiredApprovals)
		bypassers = derefStrings(preset.Protection.PullRequestBypassers)
	}

	if repo.Overrides != nil && repo.Overrides.Protection != nil {
		if p := repo.Overrides.Protection.RequiredApprovals; p != nil {
			approvals = *p
		}

		if p := repo.Overrides.Protection.PullRequestBypassers; p != nil {
			bypassers = *p
		}
	}

	return approvals, bypassers
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

		// Only the SPELLING is checkable here. Whether the App is
		// installed is a question for live GitHub, and the load path has
		// no credentials — so the existence half runs at deploy, where
		// the answer is authoritative rather than a copy of it.
		if err := refuseAppIDSpellings(rs.BypassApps); err != nil {
			return fmt.Errorf("org %q repo %q: tag ruleset %q: %w", login, name, rs.Name, err)
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
		case rs.RequiredApprovals <= 0 && len(rs.RequiredChecks) == 0:
			return fmt.Errorf("org %q repo %q: branch ruleset %q: required_approvals must be positive"+
				" or required_checks non-empty — a ruleset enforcing nothing is noise", login, name, rs.Name)
		case len(rs.BypassApps) == 0 && !rs.BypassOrgAdmins:
			return fmt.Errorf("org %q repo %q: branch ruleset %q: a bypass is required (bypass_apps"+
				" or bypass_org_admins) — without one this belongs in classic protection", login, name, rs.Name)
		}

		// See validateTagRulesets: spelling at load, existence at deploy.
		if err := refuseAppIDSpellings(rs.BypassApps); err != nil {
			return fmt.Errorf("org %q repo %q: branch ruleset %q: %w", login, name, rs.Name, err)
		}
	}

	return nil
}

// BypassAppIDs resolves ruleset bypass App names to the GitHub App
// DATABASE ids the REST API takes as Integration bypass actors.
//
// The receiver IS the scope: one organization's live installations.
// A ruleset on a repository in that organization can only name an App
// installed on it, which is the same question GitHub asks when the
// actor is written — so a name that resolves here is an actor that
// works, and a name that does not is a deploy that stops.
//
// An unknown name is an ERROR and never an empty slice. Silently
// dropping an unresolvable bypass actor is precisely the failure this
// vocabulary exists to prevent: the ruleset stays, the actor does not,
// and nothing says so until a release act is refused.
func (installed InstalledApps) BypassAppIDs(names []string) ([]int, error) {
	if len(names) == 0 {
		return nil, nil
	}

	ids := make([]int, 0, len(names))

	for _, appName := range names {
		if err := refuseAppIDSpelling(appName); err != nil {
			return nil, err
		}

		id, ok := installed[appName]
		if !ok {
			return nil, fmt.Errorf("bypass app %q is not installed on this organization —"+
				" a bypass actor that does not resolve would be dropped, not defaulted."+
				" Install the App on the org (this field takes the App's slug), or drop the name", appName)
		}

		ids = append(ids, id)
	}

	return ids, nil
}

// refuseAppIDSpelling rejects the database-id spelling this field used
// to take. YAML happily reads a bare 4597170 as the string "4597170",
// so without this the old spelling arrives as a name that merely fails
// to resolve — and the reader is told the App is not installed when
// what actually changed is the field's vocabulary.
//
// This is the half of the check that needs no network, so it runs at
// LOAD as well as at deploy: a file that still spells ids is wrong
// before anyone authenticates to anything.
func refuseAppIDSpelling(appName string) error {
	if isAllDigits(appName) {
		return fmt.Errorf("bypass app %q looks like a GitHub App database id:"+
			" this field takes App NAMES (the App's slug on GitHub), and the id is"+
			" resolved from the organization's live installations", appName)
	}

	return nil
}

// refuseAppIDSpellings applies refuseAppIDSpelling to a whole list.
func refuseAppIDSpellings(names []string) error {
	for _, appName := range names {
		if err := refuseAppIDSpelling(appName); err != nil {
			return err
		}
	}

	return nil
}

// isAllDigits reports whether s is a non-empty run of ASCII digits —
// the shape of the database id this field used to take.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}

	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
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
