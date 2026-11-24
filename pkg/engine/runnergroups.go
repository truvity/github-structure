package engine

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/pulumi/pulumi-github/sdk/v6/go/github"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	registry "github.com/truvity/github-structure/pkg/registry"
)

// runnerGroupsPageSize is the page size GitHub allows at most for the list of
// an organization's Actions runner groups.
const runnerGroupsPageSize = 100

// runnerGroupPermission is the engine App permission the runner-group API
// needs, named in every error that a missing grant causes.
const runnerGroupPermission = `the engine App needs the organization permission "Self-hosted runners"` +
	` (organization_self_hosted_runners): read and write`

// readRunnerGroups reads the organization's Actions runner groups, name to id,
// so a declared group is imported when it is live and created when it is not.
// The settings are not kept: the resource's own refresh reads them, and a
// setting that differs is an in-place update.
//
// An org that declares no runner group costs no API call, so an estate that
// never declares one never needs the App's "Self-hosted runners" permission.
// An unreadable answer stops the run: reading a 403 as "no groups" would plan
// a create for a group that exists, and the create fails with a conflict.
func (s *liveState) readRunnerGroups(ctx context.Context, client getter, org string, orgCfg *registry.Org) error {
	if len(orgCfg.RunnerGroups) == 0 {
		return nil
	}

	for page := 1; ; page++ {
		var list struct {
			TotalCount   int `json:"total_count"`
			RunnerGroups []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"runner_groups"`
		}

		path := fmt.Sprintf("/orgs/%s/actions/runner-groups?per_page=%d&page=%d", org, runnerGroupsPageSize, page)
		if err := client.Get(ctx, path, &list); err != nil {
			return fmt.Errorf("list Actions runner groups of org %s: %w (%s)", org, err, runnerGroupPermission)
		}

		for _, g := range list.RunnerGroups {
			s.runnerGroups[g.Name] = g.ID
		}

		if len(list.RunnerGroups) < runnerGroupsPageSize || len(s.runnerGroups) >= list.TotalCount {
			break
		}
	}

	return nil
}

// deployRunnerGroups declares the organization's Actions runner groups, one
// ActionsRunnerGroup each: the GitHub-side half of the self-hosted runner
// model, which repositories and workflows a group's runners take jobs from.
//
// Check-then-import, like the variables: a group that is live is imported by
// its numeric id, which is also the id the resource gets when it is created.
// A group the registry does not name (GitHub's own "Default" among them) is
// left alone: the engine adds and adopts, it does not enumerate.
//
// RETAINED on delete. A group holds runners that register into it by name
// (an ARC scale set's runnerGroup); deleting it moves them to the Default
// group, where they take jobs from every repository. Dropping a row only
// stops managing the group; an organization owner deletes it on purpose.
//
// The selected repositories are the ids of the repositories this run
// declares, so a repository created in the same run is in its group in the
// same run.
func deployRunnerGroups(
	c *pulumi.Context,
	orgCfg *registry.Org,
	repoIDs map[string]pulumi.IntOutput,
	live *liveState,
	provider *github.Provider,
) error {
	for _, name := range orgCfg.SortedRunnerGroups() {
		group := orgCfg.RunnerGroups[name]

		args := &github.ActionsRunnerGroupArgs{
			Name:                     pulumi.String(name),
			Visibility:               pulumi.String(group.Visibility),
			AllowsPublicRepositories: pulumi.Bool(group.AllowsPublicRepositories),
			RestrictedToWorkflows:    pulumi.Bool(group.RestrictedToWorkflows),
		}

		if group.Visibility == registry.RunnerVisibilitySelected {
			repos := slices.Sorted(slices.Values(group.Repos))
			ids := make(pulumi.IntArray, 0, len(repos))

			for _, repo := range repos {
				id, ok := repoIDs[repo]
				if !ok {
					return fmt.Errorf("runner group %s: repo %s is not declared by this run", name, repo)
				}

				ids = append(ids, id)
			}

			args.SelectedRepositoryIds = ids
		}

		// Left out when empty: GitHub answers [] for a group without a
		// workflow restriction, and the row says nothing. In the row's
		// order: the provider keeps the list as a list, in GitHub's order.
		if len(group.SelectedWorkflows) > 0 {
			args.SelectedWorkflows = pulumi.ToStringArray(group.SelectedWorkflows)
		}

		opts := []pulumi.ResourceOption{pulumi.Provider(provider), pulumi.RetainOnDelete(true)}

		if id, ok := live.runnerGroups[name]; ok {
			opts = append(opts, pulumi.Import(pulumi.ID(strconv.FormatInt(id, 10))))
		}

		if _, err := github.NewActionsRunnerGroup(c, runnerGroupResourceName(name), args, opts...); err != nil {
			return fmt.Errorf("runner group %s: %w", name, err)
		}
	}

	return nil
}

// runnerGroupResourceName is the engine's resource name of a runner group.
// One stack is one organization, so the group's name is unique in it.
func runnerGroupResourceName(name string) string { return "runner-group-" + name }
