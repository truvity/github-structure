package preflight

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	registry "github.com/truvity/github-structure/pkg/registry"
)

const (
	// waiverFixture is a valid registry with one waived repo (widget)
	// and one unwaived (gadget) on the same check-requiring profile —
	// the minimal pair that proves the selection keys on the waiver
	// and not on the profile.
	waiverFixture = `
profiles:
  public:
    visibility: public
    has_issues: true
    has_wiki: true
    has_projects: false
    allow_auto_merge: true
    allow_squash_merge: false
    allow_merge_commit: false
    allow_rebase_merge: true
    delete_branch_on_merge: true
    allow_update_branch: false
    allow_forking: true
    has_downloads: false
    default_branch: master
    actions:
      allowed_actions: all
      default_workflow_permissions: write
      can_approve_pull_request_reviews: true
    protection:
      enabled: true
      required_checks: [check]
      strict: false
      enforce_admins: false
      required_approvals: 0
      dismiss_stale_reviews: false
      require_code_owner_reviews: false
      require_conversation_resolution: false
      require_linear_history: false
      require_signatures: false
      allow_force_pushes: false
      allow_deletions: false
    teams:
      management: pull
orgs:
  acme:
    app_prefix: acme-
    credentials_ssm_prefix: /creds/structure-engine/acme
    settings:
      default_repository_permission: none
      members_can_create_repositories: true
      members_can_create_public_repositories: true
      members_can_create_private_repositories: true
      members_can_create_internal_repositories: false
      members_can_create_pages: false
      members_can_create_public_pages: false
      members_can_create_private_pages: false
      members_can_fork_private_repositories: false
      web_commit_signoff_required: false
      has_organization_projects: false
      has_repository_projects: true
      dependabot_alerts_enabled_for_new_repositories: true
      dependabot_security_updates_enabled_for_new_repositories: true
      dependency_graph_enabled_for_new_repositories: true
      advanced_security_enabled_for_new_repositories: false
      secret_scanning_enabled_for_new_repositories: false
      secret_scanning_push_protection_enabled_for_new_repositories: false
      actions:
        allowed_actions: all
        enabled_repositories: all
        sha_pinning_required: false
        default_workflow_permissions: write
        can_approve_pull_request_reviews: true
    teams:
      management:
        privacy: closed
    apps: {}
    repos:
      widget:
        profile: public
        checks_waived: CI has not landed yet
      gadget:
        profile: public
`
)

func loadWaiverFixture(t *testing.T) *registry.Config {
	t.Helper()

	registry, err := registry.Load(fstest.MapFS{
		"github.yaml": &fstest.MapFile{Data: []byte(waiverFixture)},
	})
	require.NoError(t, err)

	return registry
}

// The selection must key on the waiver: widget (waived) is probed with
// the profile's checks recovered, gadget (same profile, no waiver) is
// not probed at all.
func TestWaiverProbesSelectsOnlyWaivedRepos(t *testing.T) {
	registry := loadWaiverFixture(t)

	probes := waiverProbes(registry, "acme")

	require.Len(t, probes, 1)
	assert.Equal(t, waiverProbe{
		Repo:             "widget",
		DefaultBranch:    "master",
		SuppressedChecks: []string{"check"},
		Reason:           "CI has not landed yet",
	}, probes[0])
}

func TestWaiverProbesUnknownOrg(t *testing.T) {
	registry := loadWaiverFixture(t)

	assert.Nil(t, waiverProbes(registry, "no-such-org"))
}

// Against the REAL registry: every waiver the config declares must
// yield exactly one probe, and every probe must carry something to
// probe for. Tied to ChecksWaived() rather than to repo names, so the
// test outlives individual waivers coming and going — while still
// failing if the selection ever silently drops one, which would make
// checkStaleWaivers pass forever while checking nothing.
func TestReportedSuppressed(t *testing.T) {
	for name, tc := range map[string]struct {
		suppressed []string
		reported   []string
		want       []string
	}{
		"landed": {
			suppressed: []string{"check"},
			reported:   []string{"lint", "check"},
			want:       []string{"check"},
		},
		"not landed": {
			suppressed: []string{"check"},
			reported:   []string{"lint", "renovate"},
			want:       nil,
		},
		"nothing reports": {
			suppressed: []string{"check"},
			reported:   nil,
			want:       nil,
		},
		"partial landing reports only what landed": {
			suppressed: []string{"check", "govulncheck", "trivy"},
			reported:   []string{"trivy", "check"},
			want:       []string{"check", "trivy"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, reportedSuppressed(tc.suppressed, tc.reported))
		})
	}
}
