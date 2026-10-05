package registry

import (
	"fmt"
	"strings"
)

const (
	// GitHubActionsAppID is the GitHub App database id of the built-in
	// GitHub Actions integration, the identity behind a workflow's
	// GITHUB_TOKEN. Read from `GET /apps/github-actions` (slug
	// `github-actions`) on 2026-10-05; it is GitHub's own constant, the
	// same on every organization.
	//
	// It is pinned here instead of resolved because the usual route
	// cannot reach it: bypass_apps resolves slugs from the org's
	// installations (`GET /orgs/{org}/installations`), and the built-in
	// integration is not an installation of any organization.
	GitHubActionsAppID = 15368

	// WorkflowOnlyTagsRuleset is the display name of the one ruleset a
	// repository's `workflow_only_tags` renders. Fixed, so the engine
	// keys the live ruleset on repo + this name and a tag_rulesets row
	// cannot claim it.
	WorkflowOnlyTagsRuleset = "workflow-only-tags"

	tagRefPrefix = "refs/tags/"

	// rootReleaseTags is the shape of a root release tag (v1.2.3). A
	// workflow-only pattern that can match any such name is refused:
	// the ruleset's only bypass is a workflow, so catching root tags
	// would take the manual signed-tag path away.
	rootReleaseTags = "v*"
)

// RenderedRuleset is the declarative shape of a ruleset the engine
// renders and the drift check compares: everything a reader needs to see
// without running Pulumi.
type RenderedRuleset struct {
	Name         string          `json:"name"`
	Target       string          `json:"target"`
	Enforcement  string          `json:"enforcement"`
	Include      []string        `json:"include"`
	Exclude      []string        `json:"exclude"`
	Rules        []string        `json:"rules"`
	BypassActors []RenderedActor `json:"bypass_actors"`
}

// RenderedActor is one bypass actor in GitHub's vocabulary.
type RenderedActor struct {
	ActorID    int    `json:"actor_id"`
	ActorType  string `json:"actor_type"`
	BypassMode string `json:"bypass_mode"`
}

// WorkflowOnlyTagsRulesetFor renders a repository's workflow_only_tags
// patterns as the single ruleset they stand for, or nil when there are
// none. Patterns are relative tag names and gain the refs/tags/ prefix.
//
// The only bypass actor is the GitHub Actions integration. No team and
// no OrganizationAdmin: the point is that a tag matching these patterns
// can come from the repository's own release workflow (GITHUB_TOKEN)
// and from nothing else, an App token or a PAT included.
func WorkflowOnlyTagsRulesetFor(patterns []string) *RenderedRuleset {
	if len(patterns) == 0 {
		return nil
	}

	include := make([]string, 0, len(patterns))
	for _, p := range patterns {
		include = append(include, tagRefPrefix+p)
	}

	return &RenderedRuleset{
		Name:        WorkflowOnlyTagsRuleset,
		Target:      "tag",
		Enforcement: "active",
		Include:     include,
		Exclude:     []string{},
		Rules:       []string{"creation", "update", "deletion"},
		BypassActors: []RenderedActor{{
			ActorID:    GitHubActionsAppID,
			ActorType:  "Integration",
			BypassMode: "always",
		}},
	}
}

// validateWorkflowOnlyTags holds a repo's workflow_only_tags to the
// rules that keep the ruleset from bricking a release path.
//
// This ruleset is exempt from the at-least-one-bypass-team rule that
// every tag_rulesets row answers to, BECAUSE its bypass is the workflow:
// the rule exists so a ruleset is never one nobody can bypass, and the
// repository's release workflow can always bypass this one. A team here
// would also be a hole, since the field's purpose is that a human or an
// App credential cannot write these tags.
func validateWorkflowOnlyTags(login, name string, repo *Repo) error {
	if repo.WorkflowOnlyTags == nil {
		return nil
	}

	where := fmt.Sprintf("org %q repo %q: workflow_only_tags", login, name)

	if len(repo.WorkflowOnlyTags) == 0 {
		return fmt.Errorf("%s: empty list — omit the field to declare none", where)
	}

	seen := make(map[string]bool, len(repo.WorkflowOnlyTags))

	for _, p := range repo.WorkflowOnlyTags {
		if err := checkWorkflowOnlyPattern(p); err != nil {
			return fmt.Errorf("%s: %q: %w", where, p, err)
		}

		if seen[p] {
			return fmt.Errorf("%s: %q listed twice", where, p)
		}

		seen[p] = true

		if globsIntersect(p, rootReleaseTags) {
			return fmt.Errorf("%s: %q could match a root release tag (%s) — that would lock the manual"+
				" signed-tag path out; scope the pattern under a prefix such as deploy/",
				where, p, rootReleaseTags)
		}
	}

	for _, rs := range repo.TagRulesets {
		if rs == nil {
			continue
		}

		if rs.Name == WorkflowOnlyTagsRuleset {
			return fmt.Errorf("%s: tag_rulesets may not use the name %q — it is the ruleset this field renders",
				where, WorkflowOnlyTagsRuleset)
		}

		other := strings.TrimPrefix(rs.Pattern, tagRefPrefix)

		for _, p := range repo.WorkflowOnlyTags {
			if globsIntersect(p, other) {
				return fmt.Errorf("%s: %q overlaps tag ruleset %q (%s): two rulesets would govern the same tags,"+
					" and the one with a team bypass would defeat this one's purpose",
					where, p, rs.Name, rs.Pattern)
			}
		}
	}

	return nil
}

// checkWorkflowOnlyPattern checks the spelling of one pattern.
func checkWorkflowOnlyPattern(p string) error {
	switch {
	case strings.TrimSpace(p) == "":
		return fmt.Errorf("empty pattern")
	case strings.TrimSpace(p) != p:
		return fmt.Errorf("surrounding whitespace")
	case strings.HasPrefix(p, "refs/"):
		return fmt.Errorf("write the tag name relative to refs/tags/ (the prefix is added)")
	case strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/"):
		return fmt.Errorf("a pattern may not start or end with /")
	case strings.ContainsAny(p, "[]{}!"):
		return fmt.Errorf("only literal text, ?, * and ** are supported — spell character classes out")
	}

	return nil
}

// globsIntersect reports whether some tag name could match both
// patterns. `*` and `?` stand for anything but `/`, `**` for anything:
// the fnmatch rules GitHub applies to ref names. Conservative where
// unsure, since a false "overlap" is a refused row and a false "none"
// is a ruleset that governs more than its author meant.
func globsIntersect(a, b string) bool {
	ta, tb := globTokens(a), globTokens(b)
	seen := map[[2]int]bool{}

	var walk func(i, j int) bool

	walk = func(i, j int) bool {
		key := [2]int{i, j}
		if seen[key] {
			return false
		}

		seen[key] = true

		if i == len(ta) && j == len(tb) {
			return true
		}

		// A star may match nothing.
		if i < len(ta) && ta[i].multi && walk(i+1, j) {
			return true
		}

		if j < len(tb) && tb[j].multi && walk(i, j+1) {
			return true
		}

		if i == len(ta) || j == len(tb) || !tokensShareChar(ta[i], tb[j]) {
			return false
		}

		ni, nj := i, j
		if !ta[i].multi {
			ni++
		}

		if !tb[j].multi {
			nj++
		}

		return walk(ni, nj)
	}

	return walk(0, 0)
}

// globToken is one element: a literal byte, ? (one non-slash), * (any
// run of non-slash) or ** (any run).
type globToken struct {
	lit   byte // 0 unless a literal
	multi bool // * or **
	slash bool // may match "/" (only **)
}

func globTokens(p string) []globToken {
	var out []globToken

	for i := 0; i < len(p); i++ {
		switch {
		case p[i] == '*' && i+1 < len(p) && p[i+1] == '*':
			out = append(out, globToken{multi: true, slash: true})
			i++
		case p[i] == '*':
			out = append(out, globToken{multi: true})
		case p[i] == '?':
			out = append(out, globToken{})
		default:
			out = append(out, globToken{lit: p[i]})
		}
	}

	return out
}

func (t globToken) allows(c byte) bool {
	if t.lit != 0 {
		return t.lit == c
	}

	return c != '/' || t.slash
}

func tokensShareChar(x, y globToken) bool {
	switch {
	case x.lit != 0:
		return y.allows(x.lit)
	case y.lit != 0:
		return x.allows(y.lit)
	default:
		// Both are classes; any letter is in both.
		return true
	}
}
