package staterename

import (
	"sort"

	"github.com/truvity/github-structure/pkg/registry"
)

// Resource name prefixes, as pkg/engine registers them. The engine's test
// suite declares the row's resources under mocks and compares the names
// with ResourceNames, so a prefix that changes there fails here in CI.
const (
	prefixRepo         = "repo-"
	prefixActions      = "actions-"
	prefixWorkflowPerm = "workflow-perms-"
	prefixProtection   = "protection-"
	prefixRuleset      = "ruleset-"
	prefixGrant        = "grant-"
)

// ResourceNames are the engine resource names of the row repo in org: the
// ones the engine always declares (required), and the ones it declares
// only while GitHub's state allows (optional).
//
// The only optional resource is the repository's workflow permissions: the
// engine skips them while the repository has no commits, because GitHub
// answers 409 to the write.
//
// An unknown org or row has no names.
func ResourceNames(cfg *registry.Config, org, repo string) (required, optional []string) {
	return namesFor(cfg, org, repo, repo)
}

// namesFor derives the names of the row `row`, as they would be spelled if
// the repository were called `as`. The shape of the set comes from the row;
// only the spelling comes from `as`.
func namesFor(cfg *registry.Config, org, row, as string) (required, optional []string) {
	if cfg == nil {
		return nil, nil
	}

	orgCfg := cfg.Orgs[org]
	if orgCfg == nil || orgCfg.Repos[row] == nil {
		return nil, nil
	}

	r := orgCfg.Repos[row]
	resolved := cfg.Resolve(r)
	seen := map[string]bool{}

	add := func(dst *[]string, name string) {
		if !seen[name] {
			seen[name] = true
			*dst = append(*dst, name)
		}
	}

	add(&required, prefixRepo+as)

	// An archived repository is read-only: the engine declares the
	// repository and its grants and nothing else.
	if !resolved.Archived {
		add(&required, prefixActions+as)
		add(&optional, prefixWorkflowPerm+as)

		if resolved.Protection.Enabled {
			add(&required, prefixProtection+as)
		}

		for _, rs := range r.TagRulesets {
			add(&required, prefixRuleset+as+"-"+rs.Name)
		}

		if rs := registry.WorkflowOnlyTagsRulesetFor(r.WorkflowOnlyTags); rs != nil {
			add(&required, prefixRuleset+as+"-"+rs.Name)
		}

		for _, rs := range r.BranchRulesets {
			add(&required, prefixRuleset+as+"-"+rs.Name)
		}

		if gate := registry.ReviewGate(resolved); gate != nil {
			add(&required, prefixRuleset+as+"-"+gate.Name)
		}
	}

	for slug := range resolved.Teams {
		add(&required, prefixGrant+slug+"-"+as)
	}

	sort.Strings(required)
	sort.Strings(optional)

	return required, optional
}
