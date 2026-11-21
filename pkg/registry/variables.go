package registry

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	// maxVariableValue is GitHub's ceiling on the size of an Actions
	// variable's value, in bytes.
	maxVariableValue = 48 * 1024
	// reservedVariablePrefix is the prefix GitHub keeps for itself: it
	// refuses a variable whose name starts with it.
	reservedVariablePrefix = "GITHUB_"
)

// variableNameRE is a variable name GitHub accepts: letters, digits and
// underscore, not starting with a digit.
var variableNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`) //nolint:gochecknoglobals // a constant pattern

// secretLikeNameRE matches a name that reads as a secret's. A variable is
// stored and shown in clear (the registry is a file in git, the value is in
// the API's answer to anyone with read access), so a name that promises a
// secret is refused before a secret is written under it.
var secretLikeNameRE = regexp.MustCompile(`(?i)SECRET|TOKEN|PASSWORD|KEY`) //nolint:gochecknoglobals // a constant pattern

// arnRE is an ARN: `arn:<partition>:<service>:<region>:<account>:<resource>`,
// the region and the account possibly empty.
var arnRE = regexp.MustCompile(`^arn:[a-z-]+:[a-z0-9-]+:[a-z0-9-]*:[0-9]*:\S+$`) //nolint:gochecknoglobals // a constant pattern

// SortedVariables returns the repository's variable names in a stable order,
// which feeds resource names and so must not depend on map order.
func (r *Repo) SortedVariables() []string {
	names := make([]string, 0, len(r.Variables))
	for name := range r.Variables {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// validateVariables holds a repo's Actions variables to the rules that keep a
// variable from being a secret in disguise or a name GitHub refuses.
//
// The rule on secrets is deliberately blunt. A name that contains SECRET,
// TOKEN, PASSWORD or KEY is refused, because a variable is plain text that
// every reader of the repository sees. The one thing such a name may carry is
// an ARN (`AWS_KMS_KEY_ARN`, `RELEASE_ROLE_ARN`): an ARN names a thing, it
// does not open it. Anything else under such a name belongs in a secret, which
// this registry does not manage.
func validateVariables(login, name string, repo *Repo) error {
	for _, v := range repo.SortedVariables() {
		where := fmt.Sprintf("org %q repo %q: variables[%s]", login, name, v)
		value := repo.Variables[v]

		switch {
		case !variableNameRE.MatchString(v):
			return fmt.Errorf("%s: the name may hold letters, digits and underscore, and must not start with a digit", where)
		case strings.HasPrefix(strings.ToUpper(v), reservedVariablePrefix):
			return fmt.Errorf("%s: names starting with %s are GitHub's own", where, reservedVariablePrefix)
		case value == "":
			return fmt.Errorf("%s: the value is empty — give it a value, or drop the variable", where)
		case len(value) > maxVariableValue:
			return fmt.Errorf("%s: the value is %d bytes, GitHub allows %d", where, len(value), maxVariableValue)
		case secretLikeNameRE.MatchString(v) && !arnRE.MatchString(value):
			return fmt.Errorf("%s: the name reads as a secret (it contains SECRET, TOKEN, PASSWORD or KEY) and a variable is plain"+
				" text for everyone who can read the repository. Only an ARN may be kept under such a name;"+
				" put anything else in a secret", where)
		}
	}

	if groups := caseCollisions(repo.SortedVariables()); len(groups) > 0 {
		return fmt.Errorf("org %q repo %q: variables %v differ only by case, which GitHub treats as one name",
			login, name, groups[0])
	}

	return nil
}
