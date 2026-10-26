package registry

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	// githubActionsSlug is the slug of the built-in GitHub Actions
	// integration, the identity behind a workflow's GITHUB_TOKEN. It is
	// refused as a workflow_only_tags app, see validateWorkflowOnlyTags.
	githubActionsSlug = "github-actions"

	// WorkflowOnlyTagsRuleset is the display name of the one ruleset a
	// repository's `workflow_only_tags` renders. Fixed, so the engine
	// keys the live ruleset on repo + this name and a tag_rulesets row
	// cannot claim it.
	WorkflowOnlyTagsRuleset = "workflow-only-tags"

	tagRefPrefix = "refs/tags/"

	// rootReleaseTags is the shape of a root release tag (v1.2.3). A
	// workflow-only pattern that can match any such name is refused:
	// the ruleset's only bypass is a workflow's App, so catching root
	// tags would take the manual signed-tag path away.
	rootReleaseTags = "v*"
)

// WorkflowOnlyTags is a row's `workflow_only_tags:` value: tag name
// patterns that only one named App may write, and that App's slug.
//
//	workflow_only_tags:
//	  app: truvity-ci-automation-roster
//	  patterns: [deploy/pulumi/v*]
type WorkflowOnlyTags struct {
	// App is the slug of the one App that may create, update and delete
	// the tags, resolved from the organization's installations exactly
	// like bypass_apps. The repository's release workflow mints that
	// App's installation token and writes the tags with it.
	App string `yaml:"app"`
	// Patterns are tag names relative to refs/tags/.
	Patterns []string `yaml:"patterns"`
}

// UnmarshalYAML refuses the v0.14.0 spelling (a bare list of patterns)
// with the reason it is gone, instead of yaml's "cannot unmarshal !!seq
// into WorkflowOnlyTags".
func (w *WorkflowOnlyTags) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.SequenceNode {
		return fmt.Errorf("workflow_only_tags is no longer a list of patterns (the v0.14.0 shape):" +
			" it bypassed the built-in GitHub Actions integration, which GitHub refuses on apply" +
			" (422 \"Actor GitHub Actions integration must be part of the ruleset source or owner organization\")." +
			" Write `workflow_only_tags: {app: <slug of an org-installed App>, patterns: [...]}`" +
			" and have the release workflow tag with that App's installation token")
	}

	type plain WorkflowOnlyTags

	var p plain

	if err := n.Decode(&p); err != nil {
		return err
	}

	*w = WorkflowOnlyTags(p)

	return nil
}

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
// appID is the database id the row's App slug resolved to
// (InstalledApps.BypassAppIDs).
//
// The only bypass actor is that App. No team and no OrganizationAdmin:
// the point is that a tag matching these patterns can come from the
// repository's release workflow (holding the App's installation token)
// and from nothing else, a person's or another App's credential included.
func WorkflowOnlyTagsRulesetFor(patterns []string, appID int) *RenderedRuleset {
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
			ActorID:    appID,
			ActorType:  "Integration",
			BypassMode: "always",
		}},
	}
}

// validateWorkflowOnlyTags holds a repo's workflow_only_tags to the
// rules that keep the ruleset from bricking a release path.
//
// This ruleset is exempt from the at-least-one-bypass-team rule that
// every tag_rulesets row answers to, BECAUSE its bypass is the release
// workflow's App: the rule exists so a ruleset is never one nobody can
// bypass, and the release workflow can always bypass this one. A team
// here would also be a hole, since the field's purpose is that a human
// cannot write these tags.
//
// The built-in GitHub Actions integration is refused as the app. GitHub
// answers a ruleset naming it with 422 "Actor GitHub Actions integration
// must be part of the ruleset source or owner organization" (found on
// apply, 2026-10-05): it is not an installation of the organization, so
// a GITHUB_TOKEN can never be a ruleset bypass.
func validateWorkflowOnlyTags(login, name string, repo *Repo) error {
	if repo.WorkflowOnlyTags == nil {
		return nil
	}

	where := fmt.Sprintf("org %q repo %q: workflow_only_tags", login, name)

	switch app := repo.WorkflowOnlyTags.App; {
	case strings.TrimSpace(app) == "":
		return fmt.Errorf("%s: app is required — the slug of the org-installed App whose installation"+
			" token the release workflow tags with", where)
	case app == githubActionsSlug:
		return fmt.Errorf("%s: app %q is refused: GitHub rejects the built-in GitHub Actions integration"+
			" as a ruleset bypass (422 \"Actor GitHub Actions integration must be part of the ruleset"+
			" source or owner organization\"), so a GITHUB_TOKEN can never be reserved this way."+
			" Name an App installed on the organization and tag with its installation token", where, app)
	}

	if err := refuseAppIDSpelling(repo.WorkflowOnlyTags.App); err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}

	if len(repo.WorkflowOnlyTags.Patterns) == 0 {
		return fmt.Errorf("%s: patterns is empty — omit the field to declare none", where)
	}

	seen := make(map[string]bool, len(repo.WorkflowOnlyTags.Patterns))

	for _, p := range repo.WorkflowOnlyTags.Patterns {
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

		for _, p := range repo.WorkflowOnlyTags.Patterns {
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
