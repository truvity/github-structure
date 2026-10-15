package seccfg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
)

// None is how a plan spells "no configuration attached".
const None = "<none>"

type (
	// Change is one repository whose attachment differs from desired.
	Change struct {
		Repo string
		// Current is the attached configuration's name, "" for none.
		Current string
		Desired string

		repoID, desiredID int64
	}

	// ChangeSet is what Apply will do. It is data: reading it writes nothing.
	ChangeSet struct {
		Org string
		// Changes are the repositories to attach, sorted by name.
		Changes []Change
		// InSync are desired repositories already on their configuration.
		InSync []string
		// Unmanaged are the organization's non-archived repositories the
		// desired set does not mention. They are never touched.
		Unmanaged []string
	}

	configuration struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}

	repository struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Archived bool   `json:"archived"`
	}

	attachment struct {
		Status        string         `json:"status"`
		Configuration *configuration `json:"configuration"`
	}
)

// Empty reports whether Apply would write nothing.
func (p ChangeSet) Empty() bool { return len(p.Changes) == 0 }

// Lines renders the changes, one `attach <repo>: <current> -> <desired>`
// per line.
func (p ChangeSet) Lines() []string {
	lines := make([]string, 0, len(p.Changes))

	for _, ch := range p.Changes {
		cur := ch.Current
		if cur == "" {
			cur = None
		}

		lines = append(lines, fmt.Sprintf("attach %s: %s -> %s", ch.Repo, cur, ch.Desired))
	}

	return lines
}

// detached statuses mean the repository has no live attachment even
// though GitHub still names the last configuration.
var detached = map[string]bool{"detached": true, "removed": true, "removed_by_enterprise": true}

// Plan reads the organization's configurations, repositories and the
// current attachment of every desired repository, and returns the
// attachments needed to make GitHub match desired (repo name ->
// configuration name). It never writes. An unknown configuration or
// repository, or an archived repository, is an error naming every
// offender; no partial plan is returned.
func Plan(ctx context.Context, c *Client, org string, desired map[string]string) (ChangeSet, error) {
	if org == "" {
		return ChangeSet{}, errors.New("organization is required")
	}

	o := url.PathEscape(org)

	configs, err := getAll[configuration](ctx, c, "/orgs/"+o+"/code-security/configurations?per_page=100")
	if err != nil {
		return ChangeSet{}, fmt.Errorf("list configurations: %w", err)
	}

	repos, err := getAll[repository](ctx, c, "/orgs/"+o+"/repos?per_page=100&type=all")
	if err != nil {
		return ChangeSet{}, fmt.Errorf("list repositories: %w", err)
	}

	cfgByName := map[string]configuration{}
	for _, cf := range configs {
		cfgByName[cf.Name] = cf
	}

	repoByName := map[string]repository{}
	for _, r := range repos {
		repoByName[r.Name] = r
	}

	names := make([]string, 0, len(desired))
	for r := range desired {
		names = append(names, r)
	}

	sort.Strings(names)

	var problems []error

	for _, r := range names {
		cfgName := desired[r]

		if _, ok := cfgByName[cfgName]; !ok {
			problems = append(problems, fmt.Errorf("repository %q: unknown security configuration %q", r, cfgName))
		}

		switch repo, ok := repoByName[r]; {
		case !ok:
			problems = append(problems, fmt.Errorf("unknown repository %q in organization %q", r, org))
		case repo.Archived:
			problems = append(problems, fmt.Errorf("repository %q is archived; GitHub does not attach configurations to it", r))
		}
	}

	if len(problems) > 0 {
		return ChangeSet{}, errors.Join(problems...)
	}

	plan := ChangeSet{Org: org}

	for _, r := range names {
		repo, want := repoByName[r], cfgByName[desired[r]]

		cur, err := currentAttachment(ctx, c, org, r)
		if err != nil {
			return ChangeSet{}, err
		}

		if cur != nil && cur.ID == want.ID {
			plan.InSync = append(plan.InSync, r)

			continue
		}

		ch := Change{Repo: r, Desired: want.Name, repoID: repo.ID, desiredID: want.ID}
		if cur != nil {
			ch.Current = cur.Name
		}

		plan.Changes = append(plan.Changes, ch)
	}

	for _, r := range repos {
		if _, managed := desired[r.Name]; !managed && !r.Archived {
			plan.Unmanaged = append(plan.Unmanaged, r.Name)
		}
	}

	slices.Sort(plan.Unmanaged)

	return plan, nil
}

// currentAttachment returns the configuration a repository is attached
// to, or nil for none (including 204 and a detached/removed status).
func currentAttachment(ctx context.Context, c *Client, org, repo string) (*configuration, error) {
	u := c.base() + "/repos/" + url.PathEscape(org) + "/" + url.PathEscape(repo) + "/code-security-configuration"

	resp, err := c.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}

	switch {
	case resp.status == http.StatusNoContent:
		return nil, nil
	case resp.status < 200 || resp.status >= 300:
		return nil, resp.failure(http.MethodGet, u)
	}

	var a attachment
	if err := json.Unmarshal(resp.body, &a); err != nil {
		return nil, fmt.Errorf("GET %s: decode: %w", u, err)
	}

	if a.Configuration == nil || detached[a.Status] {
		return nil, nil
	}

	return a.Configuration, nil
}

// Apply performs the plan: one attach call per target configuration,
// covering every repository planned onto it. It never detaches. The
// calls are accepted asynchronously by GitHub (HTTP 202); a failure
// stops Apply and names the configuration, leaving earlier calls done.
func Apply(ctx context.Context, c *Client, plan ChangeSet) error {
	groups := map[int64][]int64{}
	names := map[int64]string{}

	var order []int64

	for _, ch := range plan.Changes {
		if _, ok := groups[ch.desiredID]; !ok {
			order = append(order, ch.desiredID)
			names[ch.desiredID] = ch.Desired
		}

		groups[ch.desiredID] = append(groups[ch.desiredID], ch.repoID)
	}

	slices.Sort(order)

	for _, id := range order {
		u := fmt.Sprintf("%s/orgs/%s/code-security/configurations/%d/attach", c.base(), url.PathEscape(plan.Org), id)
		payload := map[string]any{"scope": "selected", "selected_repository_ids": groups[id]}

		resp, err := c.do(ctx, http.MethodPost, u, payload)
		if err != nil {
			return fmt.Errorf("attach to %q: %w", names[id], err)
		}

		if resp.status < 200 || resp.status >= 300 {
			return fmt.Errorf("attach to %q: %w", names[id], resp.failure(http.MethodPost, u))
		}
	}

	return nil
}

// Summary is a one-line count for humans.
func (p ChangeSet) Summary() string {
	return fmt.Sprintf("%d to attach, %d in sync, %d unmanaged", len(p.Changes), len(p.InSync), len(p.Unmanaged))
}

// String renders the plan for a terminal.
func (p ChangeSet) String() string {
	var b strings.Builder

	for _, l := range p.Lines() {
		b.WriteString(l + "\n")
	}

	b.WriteString(p.Summary() + "\n")

	return b.String()
}
