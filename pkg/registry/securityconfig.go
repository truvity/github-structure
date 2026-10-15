package registry

import "fmt"

// validateSecurityConfigurationName accepts empty, or a name the org
// records in security_configurations. The inventory is the registry's
// statement of which configurations exist; naming one it does not list
// is a typo or a missing record, and either is cheaper to fail at load
// than at the reconciler's plan.
func validateSecurityConfigurationName(org *Org, name string) error {
	if name == "" {
		return nil
	}

	if _, ok := org.SecurityConfigurations[name]; !ok {
		return fmt.Errorf("%q is not in the org's security_configurations", name)
	}

	return nil
}

// SecurityConfigurationAssignments returns repository name -> desired
// code security configuration name, for the attachment reconciler: a
// row's own security_configuration, else the org default. Archived
// repositories take only an explicit value, never the default (GitHub
// does not attach configurations to them). Repositories with neither
// are absent, which the reconciler reports as unmanaged and never
// touches.
func (o *Org) SecurityConfigurationAssignments() map[string]string {
	out := map[string]string{}

	for name, repo := range o.Repos {
		switch {
		case repo == nil:
		case repo.SecurityConfiguration != "":
			out[name] = repo.SecurityConfiguration
		case o.DefaultSecurityConfiguration != "" && !repo.Archived:
			out[name] = o.DefaultSecurityConfiguration
		}
	}

	return out
}
