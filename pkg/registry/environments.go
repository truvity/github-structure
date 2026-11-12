package registry

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	// maxEnvironmentReviewers is GitHub's ceiling on required reviewers
	// (teams and users together) of one environment.
	maxEnvironmentReviewers = 6
)

// environmentNameRE is an environment name the engine will declare: letters,
// digits, dot, dash and underscore, starting with a letter or a digit. It is
// the name a workflow writes after `environment:` and a cloud trust may pin
// exactly, so no space, slash or wildcard is allowed to make it ambiguous.
var environmentNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`) //nolint:gochecknoglobals // a constant pattern

// Environment is one deployment environment of a repository: the object a
// workflow job names with `environment:`, which gates the job on a branch or
// tag policy and on a reviewer.
//
// The zero value declares an environment with no branch policy (any ref may
// deploy to it) and no reviewer.
type Environment struct {
	// Branches are the branch name patterns (`main`, `release/*`) that may
	// deploy to the environment. With Tags, they are the environment's
	// deployment branch policy: a ref that matches none is refused. Both
	// empty: any ref may deploy.
	Branches []string `yaml:"branches,omitempty"`
	// Tags are the tag name patterns (`v*`) that may deploy to it.
	Tags []string `yaml:"tags,omitempty"`
	// ReviewerTeams are org team slugs, one of whom must approve a job
	// before it runs. At most six. Reviewers must have at least read access
	// to the repository.
	ReviewerTeams []string `yaml:"reviewer_teams,omitempty"`
	// PreventSelfReview stops the person who triggered a job from
	// approving it. Only meaningful with ReviewerTeams.
	PreventSelfReview bool `yaml:"prevent_self_review,omitempty"`
}

// HasBranchPolicy reports whether the environment restricts which refs may
// deploy to it.
func (e Environment) HasBranchPolicy() bool { return len(e.Branches)+len(e.Tags) > 0 }

// SortedEnvironments returns the repository's environment names in a stable
// order, which feeds resource names and so must not depend on map order.
func (r *Repo) SortedEnvironments() []string {
	names := make([]string, 0, len(r.Environments))
	for name := range r.Environments {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// validateEnvironments holds a repo's environments to the rules that keep an
// environment from meaning something other than what it says.
func validateEnvironments(login, name string, org *Org, repo *Repo) error {
	for _, env := range repo.SortedEnvironments() {
		where := fmt.Sprintf("org %q repo %q: environments[%s]", login, name, env)

		if !environmentNameRE.MatchString(env) {
			return fmt.Errorf("%s: the name must match %s (an exact name a workflow and a cloud trust can pin)",
				where, environmentNameRE)
		}

		e := repo.Environments[env]

		if err := validateEnvironmentPatterns(where, "branches", e.Branches); err != nil {
			return err
		}

		if err := validateEnvironmentPatterns(where, "tags", e.Tags); err != nil {
			return err
		}

		if err := validateEnvironmentReviewers(where, org, e); err != nil {
			return err
		}
	}

	return nil
}

func validateEnvironmentPatterns(where, field string, patterns []string) error {
	seen := make(map[string]bool, len(patterns))

	for _, p := range patterns {
		switch {
		case strings.TrimSpace(p) == "" || strings.ContainsAny(p, " \t\n"):
			return fmt.Errorf("%s.%s: %q is empty or has whitespace", where, field, p)
		case strings.HasPrefix(p, "refs/"):
			return fmt.Errorf("%s.%s: %q is a pattern relative to %s, without the refs/ prefix", where, field, p, field)
		case p == "*" || p == "**":
			return fmt.Errorf("%s.%s: %q allows every ref, which is the same as no policy — omit the field instead",
				where, field, p)
		case seen[p]:
			return fmt.Errorf("%s.%s: %q listed twice", where, field, p)
		}

		seen[p] = true
	}

	return nil
}

func validateEnvironmentReviewers(where string, org *Org, e Environment) error {
	if len(e.ReviewerTeams) > maxEnvironmentReviewers {
		return fmt.Errorf("%s.reviewer_teams: %d teams, GitHub allows %d reviewers", where, len(e.ReviewerTeams), maxEnvironmentReviewers)
	}

	if e.PreventSelfReview && len(e.ReviewerTeams) == 0 {
		return fmt.Errorf("%s.prevent_self_review: there is no reviewer to keep from approving their own job —"+
			" name reviewer_teams, or drop the field", where)
	}

	seen := make(map[string]bool, len(e.ReviewerTeams))

	for _, slug := range e.ReviewerTeams {
		if _, ok := org.Teams[slug]; !ok {
			return fmt.Errorf("%s.reviewer_teams: team %q, which this org does not have", where, slug)
		}

		if seen[slug] {
			return fmt.Errorf("%s.reviewer_teams: %q listed twice", where, slug)
		}

		seen[slug] = true
	}

	return nil
}
