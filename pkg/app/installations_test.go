package app

import (
	"testing"

	"github.com/stretchr/testify/assert"

	registry "github.com/truvity/github-structure/pkg/registry"
)

// The one thing the deploy path needs from an installation list: the
// slug a ruleset names, mapped to the database id the REST API takes as
// an Integration bypass actor.
func TestAppIDsBySlug(t *testing.T) {
	got := appIDsBySlug([]installation{
		{AppID: 10, AppSlug: "acme-releases"},
		{AppID: 11, AppSlug: "vendorbot"},
	})

	assert.Equal(t, registry.InstalledApps{"acme-releases": 10, "vendorbot": 11}, got)
}

// Both auth paths must ask GitHub the same question, or a deploy and a
// drift report could disagree about which actor a ruleset names.
func TestInstallationsPathIsOrgScopedAndPaged(t *testing.T) {
	assert.Equal(t, "/orgs/acme/installations?per_page=100", installationsPath("acme"))
}
