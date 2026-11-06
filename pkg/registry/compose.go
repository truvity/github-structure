package registry

// compose.go — layering a company's rows over the provider's registry.
//
// The provider's github.yaml stays the one file Load reads. Compose reads
// the same file and lets each org take ONE fragment from a company: rows
// the company owns (teams, repositories, Actions variables, and the
// org-level settings the provider has left unset), validated against a
// ceiling the provider declares per org. Every row remembers who wrote it,
// the merged result goes through the same validation as a Load, and a
// fragment that breaks the ceiling gives no config at all, so a caller
// keeps its last good pin instead of applying half of one.
//
// The ceiling is exported as JSON (Config.Boundary) and a company runs
// ValidateFragment against it before it ever reaches the provider: the same
// function Compose runs, so the refusal is the same words in both places.

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

type (
	// FragmentCeiling is what the provider lets a fragment contain, per
	// org. An org without one accepts no fragment.
	FragmentCeiling struct {
		// Presets are the settings profiles a fragment's repositories may
		// name.
		Presets []string `yaml:"presets,omitempty" json:"presets"`
		// Access are the access bundles a fragment's repositories (and its
		// default_access) may name.
		Access []string `yaml:"access,omitempty" json:"access"`
		// BypassApps are the Apps a fragment's rulesets may name as
		// bypass actors.
		BypassApps []string `yaml:"bypass_apps,omitempty" json:"bypass_apps"`
		// MaxTeamPermission is the strongest permission a fragment may
		// grant a team on a repository. Required.
		MaxTeamPermission string `yaml:"max_team_permission" json:"max_team_permission"`
		// PublicRepos says whether a fragment's repositories may be
		// public. Default false.
		PublicRepos bool `yaml:"public_repos,omitempty" json:"public_repos"`
		// Reserved are names the provider keeps for itself even where it
		// declares nothing under them yet.
		Reserved *Reserved `yaml:"reserved,omitempty" json:"reserved,omitempty"`
	}

	// Reserved are names a fragment may not use.
	Reserved struct {
		Repos     []string `yaml:"repos,omitempty" json:"repos,omitempty"`
		Teams     []string `yaml:"teams,omitempty" json:"teams,omitempty"`
		Variables []string `yaml:"variables,omitempty" json:"variables,omitempty"`
		// TagRulesets are org-level named tag rulesets.
		TagRulesets []string `yaml:"tag_rulesets,omitempty" json:"tag_rulesets,omitempty"`
	}

	// OrgFragment is the part of an org a fragment may carry, and the keys
	// it may not. Those are fields all the same, so a fragment that
	// carries one is refused by name instead of by the decoder's
	// "field not found".
	OrgFragment struct {
		DefaultAccess []string                `yaml:"default_access,omitempty"`
		Settings      *OrgSettings            `yaml:"settings,omitempty"`
		TeamDefaults  *TeamDefaults           `yaml:"team_defaults,omitempty"`
		TagRulesets   map[string]*TagRuleset  `yaml:"tag_rulesets,omitempty"`
		Waivers       map[string][]string     `yaml:"checks_waived,omitempty"`
		Teams         map[string]*Team        `yaml:"teams,omitempty"`
		Repos         map[string]*Repo        `yaml:"repos,omitempty"`
		Variables     map[string]*OrgVariable `yaml:"variables,omitempty"`

		// Not allowed in a fragment: the engine's identity, the org's
		// owners and its security configurations are the provider's.
		Company                      string                            `yaml:"company,omitempty"`
		EngineCredentials            *EngineCredentials                `yaml:"engine_credentials,omitempty"`
		Owners                       []string                          `yaml:"owners,omitempty"`
		RunnerGroups                 map[string]*RunnerGroup           `yaml:"runner_groups,omitempty"`
		SecurityConfigurations       map[string]*SecurityConfiguration `yaml:"security_configurations,omitempty"`
		DefaultSecurityConfiguration string                            `yaml:"default_security_configuration,omitempty"`
	}

	// Fragment is one owner's rows for one org.
	Fragment struct {
		// Org is the org the rows are for.
		Org string `yaml:"org"`
		// Owner labels who wrote the rows. It is not a YAML key: the one
		// who loads the file says whose it is.
		Owner       string `yaml:"-"`
		OrgFragment `yaml:",inline"`
	}

	// Boundary is the ceiling and the reserved names of one org, as JSON a
	// company can validate against. It names things and carries no value:
	// no credentials path, no owner, no variable value.
	Boundary struct {
		Org string `json:"org"`
		// Presets are the allowed presets, each with the visibility it
		// resolves to, so a company can judge a row without the
		// provider's presets.
		Presets           map[string]string `json:"presets"`
		Access            []string          `json:"access"`
		BypassApps        []string          `json:"bypass_apps"`
		MaxTeamPermission string            `json:"max_team_permission"`
		PublicRepos       bool              `json:"public_repos"`
		// Reserved are the explicit reservations together with every name
		// the provider declares.
		Reserved Reserved `json:"reserved"`
		// ProviderOwns are the org-level singletons the provider already
		// sets, and a fragment therefore may not: settings,
		// default_access, team_defaults.
		ProviderOwns []string `json:"provider_owns"`
	}
)

const (
	ownsSettings     = "settings"
	ownsDefaultAcc   = "default_access"
	ownsTeamDefaults = "team_defaults"
)

var permissionRank = map[string]int{ //nolint:gochecknoglobals // constant table
	PermPull: 1, PermTriage: 2, PermPush: 3, PermMaintain: 4, PermAdmin: 5,
}

// ParseFragment reads a fragment file: the same strict YAML as the registry
// (an unknown key is an error), with an `org:` key naming the org. owner
// labels the rows. Keys a fragment may not carry parse, so that
// ValidateFragment refuses them by name.
func ParseFragment(owner string, data []byte) (Fragment, error) {
	var f Fragment

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(&f); err != nil {
		return Fragment{}, fmt.Errorf("fragment: parse: %w", err)
	}

	if f.Org == "" {
		return Fragment{}, errors.New("fragment: `org:` is required: a fragment is for one org")
	}

	f.Owner = owner

	return f, nil
}

// Compose reads the provider's github.yaml like Load and lays each
// fragment's rows over the org it names. With no fragments the result is
// exactly what Load returns.
//
// An org takes at most one fragment, and only if the provider gave it a
// fragment_ceiling. Every violation of every fragment is reported at once,
// and any violation returns no config.
func Compose(provider fs.FS, fragments ...Fragment) (*Config, error) {
	c, err := parseConfig(provider)
	if err != nil {
		return nil, err
	}

	if err := c.compose(fragments); err != nil {
		return nil, err
	}

	if err := c.finish(); err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Config) compose(fragments []Fragment) error {
	byOrg := map[string][]int{}
	for i := range fragments {
		byOrg[fragments[i].Org] = append(byOrg[fragments[i].Org], i)
	}

	var errs []error

	for _, org := range sortedKeys(byOrg) {
		idx := byOrg[org]

		cfg, ok := c.Orgs[org]
		if !ok || cfg == nil {
			errs = append(errs, fmt.Errorf("org %q: fragment owner %q: org %q is not in the registry (known: %s)",
				org, fragments[idx[0]].Owner, org, strings.Join(c.SortedOrgs(), ", ")))

			continue
		}

		if len(idx) > 1 {
			owners := make([]string, len(idx))
			for i, n := range idx {
				owners[i] = fmt.Sprintf("%q", fragments[n].Owner)
			}

			errs = append(errs, fmt.Errorf("org %q: more than one fragment (owners %s): an org takes one", org, strings.Join(owners, ", ")))

			continue
		}

		f := &fragments[idx[0]]

		if cfg.FragmentCeiling == nil {
			errs = append(errs, fmt.Errorf("org %q: fragment owner %q: the org has no fragment_ceiling, so it accepts no fragments", org, f.Owner))

			continue
		}

		if err := c.validateCeiling(org, cfg); err != nil {
			errs = append(errs, fmt.Errorf("github.yaml: %w", err))

			continue
		}

		b, err := c.boundary(org)
		if err != nil {
			errs = append(errs, err)

			continue
		}

		if err := ValidateFragment(b, *f); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	for _, org := range sortedKeys(byOrg) {
		f := &fragments[byOrg[org][0]]

		if err := c.merge(c.Orgs[org], f); err != nil {
			return fmt.Errorf("org %q: fragment owner %q: %w", org, f.Owner, err)
		}

		// ValidateFragment holds each name against the provider's, folded.
		// This is the same rule on the merged result, so two rows of one
		// kind never end up differing only by case.
		if problems := mergedCaseProblems(c.Orgs[org], f); len(problems) > 0 {
			errs = append(errs, fmt.Errorf("org %q: fragment owner %q: %s", org, f.Owner, strings.Join(problems, "; ")))
		}
	}

	return errors.Join(errs...)
}

// mergedCaseProblems reports the case-only collisions in the merged org that
// involve a row of the fragment.
func mergedCaseProblems(org *Org, f *Fragment) []string {
	var p problemList

	var vars []string
	if org.Settings != nil && org.Settings.Actions != nil {
		vars = sortedKeys(org.Settings.Actions.Variables)
	}

	for _, k := range []struct {
		kind   string
		merged []string
		mine   []string
	}{
		{"teams", sortedKeys(org.Teams), sortedKeys(f.Teams)},
		{"repos", sortedKeys(org.Repos), sortedKeys(f.Repos)},
		{"variables", vars, sortedKeys(f.Variables)},
		{"tag_rulesets", sortedKeys(org.TagRulesets), sortedKeys(f.TagRulesets)},
	} {
		for _, g := range caseCollisions(k.merged) {
			if containsAny(k.mine, g) {
				p.add("%s.%s: differs only by case from %s.%s: GitHub treats the names as one", k.kind, g[0], k.kind, g[1])
			}
		}
	}

	sort.Strings(p.list)

	return p.list
}

func containsAny(list, names []string) bool {
	for _, s := range names {
		if contains(list, s) {
			return true
		}
	}

	return false
}

// merge moves the fragment's rows into the org, each marked with its owner.
// The fragment has already passed ValidateFragment.
func (c *Config) merge(org *Org, f *Fragment) error {
	if f.Settings != nil {
		org.Settings = f.Settings
	}

	if len(f.DefaultAccess) > 0 {
		org.DefaultAccess = f.DefaultAccess
	}

	if f.TeamDefaults != nil {
		org.TeamDefaults = f.TeamDefaults
	}

	if len(f.Teams) > 0 && org.Teams == nil {
		org.Teams = map[string]*Team{}
	}

	for slug, t := range f.Teams {
		if t == nil {
			t = &Team{}
		}

		t.Owner = f.Owner
		org.Teams[slug] = t
	}

	if len(f.Repos) > 0 && org.Repos == nil {
		org.Repos = map[string]*Repo{}
	}

	for name, r := range f.Repos {
		r.Owner = f.Owner
		org.Repos[name] = r
	}

	if len(f.TagRulesets) > 0 && org.TagRulesets == nil {
		org.TagRulesets = map[string]*TagRuleset{}
	}

	for name, t := range f.TagRulesets {
		org.TagRulesets[name] = t
	}

	for reason, names := range f.Waivers {
		if org.Waivers == nil {
			org.Waivers = map[string][]string{}
		}

		org.Waivers[reason] = append(org.Waivers[reason], names...)
	}

	if len(f.Variables) > 0 {
		if org.Settings == nil || org.Settings.Actions == nil {
			return errors.New("variables need the org's settings.actions block, and the org has none")
		}

		if org.Settings.Actions.Variables == nil {
			org.Settings.Actions.Variables = map[string]*OrgVariable{}
		}

		for name, v := range f.Variables {
			v.Owner = f.Owner
			org.Settings.Actions.Variables[name] = v
		}
	}

	return nil
}

// Boundary exports the ceiling and the reserved names of an org, for a
// company to validate its fragment against (ValidateFragment). An org with
// no fragment_ceiling has no boundary: it accepts no fragments.
func (c *Config) Boundary(org string) (Boundary, error) {
	return c.boundary(org)
}

func (c *Config) boundary(org string) (Boundary, error) {
	cfg, ok := c.Orgs[org]
	if !ok || cfg == nil {
		return Boundary{}, fmt.Errorf("org %q is not in the registry (known: %s)", org, strings.Join(c.SortedOrgs(), ", "))
	}

	fc := cfg.FragmentCeiling
	if fc == nil {
		return Boundary{}, fmt.Errorf("org %q has no fragment_ceiling, so it accepts no fragments", org)
	}

	b := Boundary{
		Org:               org,
		Presets:           map[string]string{},
		Access:            sortedCopy(fc.Access),
		BypassApps:        sortedCopy(fc.BypassApps),
		MaxTeamPermission: fc.MaxTeamPermission,
		PublicRepos:       fc.PublicRepos,
		ProviderOwns:      []string{},
	}

	for _, name := range fc.Presets {
		if p, ok := c.Presets[name]; ok {
			eff := *gitHubDefaults()
			eff.overlay(p)

			b.Presets[name] = derefString(eff.Visibility)
		}
	}

	var res Reserved
	if fc.Reserved != nil {
		res = *fc.Reserved
	}

	var vars []string
	if cfg.Settings != nil && cfg.Settings.Actions != nil {
		vars = sortedKeys(cfg.Settings.Actions.Variables)
	}

	b.Reserved = Reserved{
		Repos:       union(res.Repos, sortedKeys(cfg.Repos)),
		Teams:       union(res.Teams, sortedKeys(cfg.Teams)),
		Variables:   union(res.Variables, vars),
		TagRulesets: union(res.TagRulesets, sortedKeys(cfg.TagRulesets)),
	}

	if cfg.Settings != nil {
		b.ProviderOwns = append(b.ProviderOwns, ownsSettings)
	}

	if len(cfg.DefaultAccess) > 0 {
		b.ProviderOwns = append(b.ProviderOwns, ownsDefaultAcc)
	}

	if cfg.TeamDefaults != nil {
		b.ProviderOwns = append(b.ProviderOwns, ownsTeamDefaults)
	}

	return b, nil
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)

	return out
}

// union is the sorted set of both lists; nil when both are empty.
func union(a, b []string) []string {
	seen := map[string]bool{}

	var out []string

	for _, l := range [][]string{a, b} {
		for _, s := range l {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}

	sort.Strings(out)

	return out
}

// ValidateFragment holds a fragment to an org's boundary. It is the check
// Compose runs, so a company that runs it first sees the refusal the
// provider would give, in the same words. Every violation is named, with
// the key path of the row, and the error is nil only when there are none.
//
//	org "acme": fragment owner "globex": repos.gadget: provider-owned (reserved); ...
func ValidateFragment(b Boundary, f Fragment) error {
	problems := fragmentProblems(b, f)
	if len(problems) == 0 {
		return nil
	}

	return fmt.Errorf("org %q: fragment owner %q: %s", b.Org, f.Owner, strings.Join(problems, "; "))
}

type problemList struct{ list []string }

func (p *problemList) add(format string, args ...any) {
	p.list = append(p.list, fmt.Sprintf(format, args...))
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}

	return false
}

// containsFold is contains for names GitHub treats as case-insensitive
// (repositories, teams, variables, rulesets): "Infra" is the provider's
// "infra".
func containsFold(list []string, s string) bool {
	for _, e := range list {
		if strings.EqualFold(e, s) {
			return true
		}
	}

	return false
}

// caseCollisions groups names that differ only by case. Each group has two
// or more names, sorted, and the groups are sorted by their folded name.
func caseCollisions(names []string) [][]string {
	byFold := map[string][]string{}

	for _, n := range names {
		k := strings.ToLower(n)
		byFold[k] = append(byFold[k], n)
	}

	var out [][]string

	for _, k := range sortedKeys(byFold) {
		if g := byFold[k]; len(g) > 1 {
			sort.Strings(g)

			out = append(out, g)
		}
	}

	return out
}

// caseProblems names the case-only collisions among a fragment's own rows
// of one kind.
func caseProblems(p *problemList, kind string, names []string) {
	for _, g := range caseCollisions(names) {
		p.add("%s.%s: differs only by case from %s.%s: GitHub treats the names as one", kind, g[0], kind, g[1])
	}
}

func fragmentProblems(b Boundary, f Fragment) []string {
	var p problemList

	if f.Org != b.Org {
		p.add("org: the fragment is for %q", f.Org)
	}

	if !slugPattern.MatchString(f.Owner) {
		p.add("owner: %q is not a valid owner label", f.Owner)
	}

	if f.Company != "" {
		p.add("company: not allowed in a fragment")
	}

	if f.EngineCredentials != nil {
		p.add("engine_credentials: not allowed in a fragment")
	}

	if len(f.Owners) > 0 {
		p.add("owners: not allowed in a fragment")
	}

	if len(f.RunnerGroups) > 0 {
		p.add("runner_groups: not allowed in a fragment")
	}

	if len(f.SecurityConfigurations) > 0 {
		p.add("security_configurations: not allowed in a fragment")
	}

	if f.DefaultSecurityConfiguration != "" {
		p.add("default_security_configuration: not allowed in a fragment")
	}

	caseProblems(&p, "teams", sortedKeys(f.Teams))
	caseProblems(&p, "repos", sortedKeys(f.Repos))
	caseProblems(&p, "variables", sortedKeys(f.Variables))
	caseProblems(&p, "tag_rulesets", sortedKeys(f.TagRulesets))

	singletonProblems(&p, b, f)
	teamProblems(&p, b, f)
	repoProblems(&p, b, f)
	variableProblems(&p, b, f)
	rulesetNameProblems(&p, b, f)

	for _, reason := range sortedKeys(f.Waivers) {
		for _, name := range f.Waivers[reason] {
			if f.Repos[name] == nil {
				p.add("checks_waived %q: repo %q is not a repository of this fragment", reason, name)
			}
		}
	}

	sort.Strings(p.list)

	return p.list
}

func singletonProblems(p *problemList, b Boundary, f Fragment) {
	if f.Settings != nil && contains(b.ProviderOwns, ownsSettings) {
		p.add("settings: provider-owned (reserved)")
	}

	if f.TeamDefaults != nil && contains(b.ProviderOwns, ownsTeamDefaults) {
		p.add("team_defaults: provider-owned (reserved)")
	}

	if len(f.DefaultAccess) > 0 && contains(b.ProviderOwns, ownsDefaultAcc) {
		p.add("default_access: provider-owned (reserved)")

		return
	}

	for i, bundle := range f.DefaultAccess {
		if !contains(b.Access, bundle) {
			p.add("default_access[%d] %q outside ceiling", i, bundle)
		}
	}
}

func teamProblems(p *problemList, b Boundary, f Fragment) {
	for _, slug := range sortedKeys(f.Teams) {
		if containsFold(b.Reserved.Teams, slug) {
			p.add("teams.%s: provider-owned (reserved)", slug)
		}

		// A parent passes access down to its children. A team nested under
		// a team the fragment does not own would inherit, or lend, access
		// across owners, so a parent is the fragment's own.
		if t := f.Teams[slug]; t != nil && t.Parent != "" {
			if _, ok := f.Teams[t.Parent]; !ok {
				p.add("teams.%s.parent %q is not a team of this fragment", slug, t.Parent)
			}
		}
	}
}

func repoProblems(p *problemList, b Boundary, f Fragment) {
	for _, name := range sortedKeys(f.Repos) {
		path := "repos." + name

		if containsFold(b.Reserved.Repos, name) {
			p.add("%s: provider-owned (reserved)", path)
		}

		row := f.Repos[name]
		if row == nil {
			p.add("%s: empty row", path)

			continue
		}

		visibility, known := b.Presets[row.Preset]
		if !known {
			p.add("%s.preset %q outside ceiling", path, row.Preset)
		}

		if row.Overrides != nil && row.Overrides.Visibility != nil {
			visibility, known = *row.Overrides.Visibility, true
		}

		if known && visibility == VisibilityPublic && !b.PublicRepos {
			p.add("%s: public visibility outside ceiling", path)
		}

		for i, bundle := range row.Access {
			if !contains(b.Access, bundle) {
				p.add("%s.access[%d] %q outside ceiling", path, i, bundle)
			}
		}

		for _, slug := range sortedKeys(row.Teams) {
			perm := row.Teams[slug]

			switch rank, ok := permissionRank[perm]; {
			case !ok:
				p.add("%s.teams.%s %q is not a team permission", path, slug, perm)
			case rank > permissionRank[b.MaxTeamPermission]:
				p.add("%s.teams.%s %q outside ceiling", path, slug, perm)
			}
		}

		for i, rs := range row.TagRulesets {
			if rs != nil {
				appProblems(p, b, fmt.Sprintf("%s.tag_rulesets[%d]", path, i), rs.BypassApps)
			}
		}

		for i, rs := range row.BranchRulesets {
			if rs != nil {
				appProblems(p, b, fmt.Sprintf("%s.branch_rulesets[%d]", path, i), rs.BypassApps)
			}
		}
	}
}

func appProblems(p *problemList, b Boundary, path string, apps []string) {
	for i, app := range apps {
		if !contains(b.BypassApps, app) {
			p.add("%s.bypass_apps[%d] %q outside ceiling", path, i, app)
		}
	}
}

func variableProblems(p *problemList, b Boundary, f Fragment) {
	for _, name := range sortedKeys(f.Variables) {
		if containsFold(b.Reserved.Variables, name) {
			p.add("variables.%s: provider-owned (reserved)", name)
		}
	}
}

func rulesetNameProblems(p *problemList, b Boundary, f Fragment) {
	for _, name := range sortedKeys(f.TagRulesets) {
		if containsFold(b.Reserved.TagRulesets, name) {
			p.add("tag_rulesets.%s: provider-owned (reserved)", name)
		}

		if rs := f.TagRulesets[name]; rs != nil {
			appProblems(p, b, "tag_rulesets."+name, rs.BypassApps)
		}
	}
}

// validateCeiling checks an org's fragment_ceiling against the registry it
// sits in: every name it allows exists, and the maximum permission is one.
func (c *Config) validateCeiling(login string, org *Org) error {
	fc := org.FragmentCeiling
	if fc == nil {
		return nil
	}

	for _, name := range fc.Presets {
		if _, ok := c.Presets[name]; !ok {
			return fmt.Errorf("org %q: fragment_ceiling.presets: %q names no declared preset", login, name)
		}
	}

	for _, name := range fc.Access {
		if _, ok := c.Access[name]; !ok {
			return fmt.Errorf("org %q: fragment_ceiling.access: %q names no declared access bundle", login, name)
		}
	}

	for _, app := range fc.BypassApps {
		if !slugPattern.MatchString(app) {
			return fmt.Errorf("org %q: fragment_ceiling.bypass_apps: %q is not an App slug", login, app)
		}
	}

	if !validPermissions[fc.MaxTeamPermission] {
		return fmt.Errorf("org %q: fragment_ceiling.max_team_permission %q must be one of pull, triage, push, maintain, admin",
			login, fc.MaxTeamPermission)
	}

	if fc.Reserved != nil {
		for kind, names := range map[string][]string{
			"repos": fc.Reserved.Repos, "teams": fc.Reserved.Teams, "variables": fc.Reserved.Variables,
		} {
			for _, name := range names {
				if !slugPattern.MatchString(name) {
					return fmt.Errorf("org %q: fragment_ceiling.reserved.%s: %q is not a valid name", login, kind, name)
				}
			}
		}
	}

	return nil
}
