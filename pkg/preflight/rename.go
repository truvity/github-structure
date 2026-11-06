package preflight

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"

	registry "github.com/truvity/github-structure/pkg/registry"
)

// Provider tokens of the repository-scoped resources the engine declares
// beyond those in state.go.
const (
	actionsType  = "github:index/actionsRepositoryPermissions:ActionsRepositoryPermissions"
	workflowType = "github:index/workflowRepositoryPermissions:WorkflowRepositoryPermissions"
	grantType    = "github:index/teamRepository:TeamRepository"
)

// singleSuffix are the kinds whose resource name is a prefix and the
// repository name, and nothing else.
var singleSuffix = map[string]string{ //nolint:gochecknoglobals // constant table
	repoType:       "repo-",
	actionsType:    "actions-",
	workflowType:   "workflow-perms-",
	protectionType: "protection-",
}

const (
	rulesetPrefix = "ruleset-"
	grantPrefix   = "grant-"
)

// plannedStep is one create or delete the plan holds on a resource.
type plannedStep struct {
	urn  string
	typ  string
	name string
}

// plan is what the preview events say, as far as the guards read it.
type plan struct {
	replaces []plannedReplace
	creates  []plannedStep
	deletes  []plannedStep
}

// renameFinding is a repository the plan deletes under one name and
// creates under another.
type renameFinding struct {
	Old, New string
	// Resources pairs each deleted resource with the one created in its
	// place, as "old-name -> new-name".
	Resources []string
}

// collectStep keeps the creates and deletes of repository-scoped resources.
func collectStep(p *plan, meta apitype.StepEventMetadata) {
	if !repositoryScoped(meta.Type) {
		return
	}

	s := plannedStep{urn: meta.URN, typ: meta.Type, name: shortURN(meta.URN)}

	switch string(meta.Op) {
	case "create":
		p.creates = append(p.creates, s)
	case "delete":
		p.deletes = append(p.deletes, s)
	}
}

func repositoryScoped(typ string) bool {
	if _, ok := singleSuffix[typ]; ok {
		return true
	}

	return typ == rulesetType || typ == grantType
}

// split is a resource name read as a resource of repository repo: kind
// says how the name is built, and rest is what the name holds besides the
// repository (a ruleset's name, a grant's team).
type split struct {
	repo string
	rest string
}

// readAsCreated reads a created resource's name against the registry: the
// repository part must be a declared row. A name can read more than one way
// (repository "a" with a ruleset "b-x", repository "a-b" with a ruleset
// "x"), so every reading is returned.
func readAsCreated(org *registry.Org, typ, name string) []split {
	var out []split

	if prefix, ok := singleSuffix[typ]; ok {
		if repo, ok := strings.CutPrefix(name, prefix); ok && org.Repos[repo] != nil {
			out = append(out, split{repo: repo})
		}

		return out
	}

	switch typ {
	case rulesetType:
		for repo := range org.Repos {
			if rest, ok := strings.CutPrefix(name, rulesetPrefix+repo+"-"); ok && rest != "" {
				out = append(out, split{repo: repo, rest: rest})
			}
		}
	case grantType:
		for team := range org.Teams {
			if repo, ok := strings.CutPrefix(name, grantPrefix+team+"-"); ok && org.Repos[repo] != nil {
				out = append(out, split{repo: repo, rest: team})
			}
		}
	}

	return out
}

// formerRepo reads a deleted resource's name as the same kind of resource
// as a created one (same rest), and returns the repository it belonged to.
func formerRepo(typ, name, rest string) (string, bool) {
	if prefix, ok := singleSuffix[typ]; ok {
		repo, ok := strings.CutPrefix(name, prefix)

		return repo, ok && repo != ""
	}

	switch typ {
	case rulesetType:
		middle, ok := strings.CutPrefix(name, rulesetPrefix)
		if !ok {
			return "", false
		}

		repo, ok := strings.CutSuffix(middle, "-"+rest)

		return repo, ok && repo != ""
	case grantType:
		repo, ok := strings.CutPrefix(name, grantPrefix+rest+"-")

		return repo, ok && repo != ""
	}

	return "", false
}

// renameShaped finds the repositories the plan deletes under one name and
// creates under another: for a created resource of a declared row, a
// deleted resource of the same kind (same type, same ruleset name or team)
// whose repository is not declared any more.
//
// That plan is a renamed row whose state was not re-keyed. It is not
// caught by the other guards when it is made of resources they do not
// cover: a team grant is neither irreplaceable nor guarded, and the delete
// half of one can strip a team's access when GitHub redirects the old name
// to the renamed repository.
//
// allow names repositories (either name of a pair) whose delete and create
// are meant.
func renameShaped(org *registry.Org, p plan, allow []string) []renameFinding {
	if org == nil {
		return nil
	}

	allowed := map[string]bool{}
	for _, a := range allow {
		allowed[a] = true
	}

	byPair := map[[2]string]*renameFinding{}

	for _, c := range p.creates {
		for _, rd := range readAsCreated(org, c.typ, c.name) {
			for _, d := range p.deletes {
				if d.typ != c.typ {
					continue
				}

				old, ok := formerRepo(d.typ, d.name, rd.rest)
				if !ok || old == rd.repo || org.Repos[old] != nil {
					continue
				}

				if allowed[old] || allowed[rd.repo] {
					continue
				}

				key := [2]string{old, rd.repo}
				if byPair[key] == nil {
					byPair[key] = &renameFinding{Old: old, New: rd.repo}
				}

				byPair[key].Resources = append(byPair[key].Resources, d.name+" -> "+c.name)
			}
		}
	}

	out := make([]renameFinding, 0, len(byPair))

	for _, f := range byPair {
		sort.Strings(f.Resources)
		out = append(out, *f)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].New != out[j].New {
			return out[i].New < out[j].New
		}

		return out[i].Old < out[j].Old
	})

	return out
}

func writeRenames(b *strings.Builder, org string, renames []renameFinding) {
	_, _ = fmt.Fprintf(b, "\nREFUSING: %s plans to delete the resources of %d repository(ies) under one name and create them under another"+
		" (a rename-shaped plan).\n\n", org, len(renames))

	for i := range renames {
		r := &renames[i]
		_, _ = fmt.Fprintf(b, "  %s -> %s\n", r.Old, r.New)

		for _, res := range r.Resources {
			_, _ = fmt.Fprintf(b, "      %s\n", res)
		}

		b.WriteString("\n")
	}

	b.WriteString("If a repository was renamed, do not apply this plan. Rename it on GitHub, rename its\n")
	b.WriteString("row, and re-key the state instead, one repository at a time:\n\n")

	for i := range renames {
		_, _ = fmt.Fprintf(b, "    state rename-repo %s %s\n", renames[i].Old, renames[i].New)
	}

	b.WriteString("\n(docs/safety.md, \"Renaming a repository row is a state operation\".) Applying the plan\n")
	b.WriteString("deletes the old resources: branch protection and rulesets go, and a team grant's delete can\n")
	b.WriteString("strip a team's access.\n\n")
	b.WriteString("If the delete and the create are both meant (one repository removed, another added in the\n")
	b.WriteString("same run), name the repository to allow it:\n\n")

	for i := range renames {
		_, _ = fmt.Fprintf(b, "    preflight --allow-recreate %s\n", renames[i].New)
	}

	b.WriteString("\n")
}
