package registry_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const inventory = `    security_configurations:
      Baseline:
        enforcement: enforced
        dependabot_alerts: enabled
        dependabot_security_updates: not_set
        applies_to: everything
      Strict:
        enforcement: enforced
        dependabot_alerts: enabled
        dependabot_security_updates: enabled
        applies_to: some
`

// withSecurity adds the inventory, an optional org default, and a
// second (archived) repo, then a row-level setting on widget.
func withSecurity(def, widget string) string {
	body := strings.Replace(minimal, "    repos:\n", inventory+def+"    repos:\n      old:\n        preset: public\n        archived: true\n", 1)

	return strings.Replace(body, "      widget:\n        preset: public\n", "      widget:\n        preset: public\n"+widget, 1)
}

func TestSecurityConfigurationAssignments(t *testing.T) {
	c, err := load(t, withSecurity("    default_security_configuration: Baseline\n", "        security_configuration: Strict\n"))
	require.NoError(t, err)

	// widget's own value wins; the archived repo does not take the default.
	assert.Equal(t, map[string]string{"widget": "Strict"}, c.Orgs["acme"].SecurityConfigurationAssignments())

	c, err = load(t, withSecurity("    default_security_configuration: Baseline\n", ""))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"widget": "Baseline"}, c.Orgs["acme"].SecurityConfigurationAssignments())
}

func TestSecurityConfigurationUnsetIsUnmanaged(t *testing.T) {
	c, err := load(t, minimal)
	require.NoError(t, err)
	assert.Empty(t, c.Orgs["acme"].SecurityConfigurationAssignments())
}

func TestSecurityConfigurationUnknownName(t *testing.T) {
	_, err := load(t, withSecurity("", "        security_configuration: Nope\n"))
	require.ErrorContains(t, err, `security_configuration: "Nope" is not in the org's security_configurations`)

	_, err = load(t, withSecurity("    default_security_configuration: Nope\n", ""))
	require.ErrorContains(t, err, "default_security_configuration")
}
