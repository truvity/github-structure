package staterename

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/truvity/github-structure/pkg/registry"
)

// Move is one resource whose URN changed.
type Move struct {
	From, To string
}

// Report says what a Rekey did.
type Report struct {
	From, To string
	// Noop is true when the state already holds the target's resources and
	// none of the source's: the re-key was done before.
	Noop bool
	// Moved are the resources that took the new name, in state order.
	Moved []Move
	// References counts the references to a moved resource that were
	// rewritten: dependencies, property dependencies, parents,
	// deletedWith, aliases, replaceWith, viewOf and provider references.
	References int
	// Resources is the number of resources in the state, before and after.
	Resources int
}

// freeTextKeys are fields whose value is text or a ref pattern, never a
// reference to the repository: a topic or a description that happens to be
// the old name is still the old name.
var freeTextKeys = map[string]bool{ //nolint:gochecknoglobals // constant set
	"description": true,
	"topics":      true,
	"homepageUrl": true,
	"pattern":     true,
	"includes":    true,
	"excludes":    true,
}

const repositoryTypeSuffix = ":Repository"

// Rekey moves the state of the repository row `from` to the name `to`, after
// the repository was renamed on GitHub and the registry row was renamed.
// state is `pulumi stack export` output, or its "deployment" object alone.
//
// It changes, in the resources of the row and nowhere else: the URN, the id
// and import id, and the name as a whole token in inputs and outputs (the
// value, or a path, URL or id segment such as "acme/old", ".../old.git" or
// "101:old"). A substring inside text never moves, and neither does a
// description, topic, homepage or ref pattern. It rewrites every reference
// to a moved URN held by any resource. Secret values stay as they are: a
// ciphertext cannot be read, and the first refresh after the rename reads
// the live ones.
//
// The row's resources are derived from the registry row `to`, with the
// engine's naming, so the transform refuses what it cannot be sure of:
//
//   - `to` is not a row, or `from` still is;
//   - the state already holds a resource under the target name;
//   - a resource the row always declares is missing from the state;
//   - the state holds a resource carrying the old name that the row does not
//     declare and no other row owns;
//   - the same URN appears twice, or a pending operation touches a resource
//     that would move.
//
// A refusal names every problem at once and returns no state. When the state
// already holds all of the target's resources and none of the source's,
// Rekey returns it unchanged with Report.Noop set, so running it twice is
// safe. Swapping `from` and `to` (with the registry reverted) is the
// rollback. Output is indented JSON with sorted keys; the resource order is
// kept.
func Rekey(state []byte, org string, cfg *registry.Config, from, to string) ([]byte, Report, error) {
	rep := Report{From: from, To: to}

	if err := checkArgs(org, cfg, from, to); err != nil {
		return nil, rep, err
	}

	root, dep, resources, err := parse(state)
	if err != nil {
		return nil, rep, err
	}

	rep.Resources = len(resources)

	byName, err := indexByName(resources)
	if err != nil {
		return nil, rep, err
	}

	rn := newRenamer(cfg, org, from, to)

	if clash := rn.ambiguous(); len(clash) > 0 {
		return nil, rep, fmt.Errorf("repository names %q and %q produce the same resource names (%s); rename through an intermediate name",
			from, to, strings.Join(clash, ", "))
	}

	var present, targets []string

	for name := range byName {
		switch {
		case rn.fromSet[name]:
			present = append(present, name)
		case rn.toSet[name]:
			targets = append(targets, name)
		}
	}

	sort.Strings(present)
	sort.Strings(targets)

	if len(present) == 0 {
		if missing := missingFrom(rn.toRequired, byName); len(missing) == 0 {
			rep.Noop = true

			return state, rep, nil
		}

		return nil, rep, fmt.Errorf("state holds no resource of repository %q, and the target %q is incomplete (missing %s)",
			from, to, strings.Join(missingFrom(rn.toRequired, byName), ", "))
	}

	var problems []string

	if len(targets) > 0 {
		problems = append(problems, fmt.Sprintf("state already holds resources under the target name %q: %s", to, strings.Join(targets, ", ")))
	}

	if missing := missingFrom(rn.fromRequired, byName); len(missing) > 0 {
		problems = append(problems, fmt.Sprintf("resources missing from the state: %s", strings.Join(missing, ", ")))
	}

	var unexpected []string

	for name := range byName {
		if rn.carriesFrom(name) && !rn.fromSet[name] && !rn.owned[name] {
			unexpected = append(unexpected, name)
		}
	}

	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		problems = append(problems, fmt.Sprintf("unexpected resources carry the old name, and the row %q does not declare them: %s",
			to, strings.Join(unexpected, ", ")))
	}

	for _, name := range pendingOn(dep, rn.fromSet) {
		problems = append(problems, fmt.Sprintf("a pending operation touches %s; finish or cancel it first", name))
	}

	if len(problems) > 0 {
		return nil, rep, fmt.Errorf("refusing to re-key %q to %q in org %q:\n  - %s", from, to, org, strings.Join(problems, "\n  - "))
	}

	moved := map[string]string{}

	for _, r := range resources {
		res := r.(map[string]any)
		urn := res["urn"].(string)

		name := urnName(urn)
		if !rn.fromSet[name] {
			continue
		}

		next := urn[:len(urn)-len(name)] + rn.moveName(name)
		moved[urn] = next
		rep.Moved = append(rep.Moved, Move{From: urn, To: next})
	}

	isMoved := map[string]bool{}
	for u := range moved {
		isMoved[u] = true
	}

	for _, r := range resources {
		res := r.(map[string]any)

		if isMoved[res["urn"].(string)] {
			rekeyOwn(res, from, to)
			res["urn"] = moved[res["urn"].(string)]
		}

		rep.References += followRefs(res, moved)
	}

	if len(resources) != rep.Resources {
		return nil, rep, errors.New("internal error: the resource count changed")
	}

	if _, err := indexByName(resources); err != nil {
		return nil, rep, fmt.Errorf("internal error: %w", err)
	}

	out, err := encode(root)
	if err != nil {
		return nil, rep, err
	}

	return out, rep, nil
}

func checkArgs(org string, cfg *registry.Config, from, to string) error {
	switch {
	case from == "" || to == "":
		return errors.New("empty repository name")
	case from == to:
		return fmt.Errorf("from and to are the same name %q", from)
	case cfg == nil:
		return errors.New("no registry given")
	case cfg.Orgs[org] == nil:
		return fmt.Errorf("registry has no org %q", org)
	case cfg.Orgs[org].Repos[to] == nil:
		return fmt.Errorf("registry has no row %q in org %q: rename the row before the state", to, org)
	case cfg.Orgs[org].Repos[from] != nil:
		return fmt.Errorf("registry still declares a row %q in org %q: a rename removes the old row", from, org)
	}

	return nil
}

func parse(state []byte) (root, dep map[string]any, resources []any, err error) {
	dec := json.NewDecoder(bytes.NewReader(state))
	dec.UseNumber()

	if err := dec.Decode(&root); err != nil {
		return nil, nil, nil, fmt.Errorf("state is not a JSON object: %w", err)
	}

	dep = root
	if d, ok := root["deployment"].(map[string]any); ok {
		dep = d
	}

	resources, _ = dep["resources"].([]any)
	if len(resources) == 0 {
		return nil, nil, nil, errors.New("state has no resources")
	}

	for i, r := range resources {
		res, ok := r.(map[string]any)
		if !ok {
			return nil, nil, nil, fmt.Errorf("state resource %d is not an object", i)
		}

		if _, ok := res["urn"].(string); !ok {
			return nil, nil, nil, fmt.Errorf("state resource %d has no urn", i)
		}
	}

	return root, dep, resources, nil
}

func urnName(urn string) string {
	return urn[strings.LastIndex(urn, "::")+2:]
}

// indexByName maps resource names to resources and refuses a URN held twice.
func indexByName(resources []any) (map[string]map[string]any, error) {
	byName := map[string]map[string]any{}
	seen := map[string]bool{}

	var twice []string

	for _, r := range resources {
		res := r.(map[string]any)
		urn := res["urn"].(string)

		if seen[urn] {
			twice = append(twice, urnName(urn))
		}

		seen[urn] = true
		byName[urnName(urn)] = res
	}

	if len(twice) > 0 {
		sort.Strings(twice)

		return nil, fmt.Errorf("state holds the same URN twice: %s", strings.Join(twice, ", "))
	}

	return byName, nil
}

func missingFrom(want []string, byName map[string]map[string]any) []string {
	var out []string

	for _, n := range want {
		if byName[n] == nil {
			out = append(out, n)
		}
	}

	return out
}

// pendingOn names the resources in the state's pending operations that
// would move.
func pendingOn(dep map[string]any, fromSet map[string]bool) []string {
	var out []string

	ops, _ := dep["pending_operations"].([]any)
	for _, o := range ops {
		op, _ := o.(map[string]any)
		res, _ := op["resource"].(map[string]any)

		urn, _ := res["urn"].(string)
		if urn != "" && fromSet[urnName(urn)] {
			out = append(out, urnName(urn))
		}
	}

	sort.Strings(out)

	return out
}

// renamer knows which resource names belong to the row, before and after,
// and which belong to other rows.
type renamer struct {
	from, to     string
	teams        []string
	fromRequired []string
	toRequired   []string
	fromSet      map[string]bool
	toSet        map[string]bool
	owned        map[string]bool
}

func newRenamer(cfg *registry.Config, org, from, to string) *renamer {
	orgCfg := cfg.Orgs[org]
	rn := &renamer{
		from: from, to: to,
		fromSet: map[string]bool{}, toSet: map[string]bool{}, owned: map[string]bool{},
	}

	for slug := range orgCfg.Teams {
		rn.teams = append(rn.teams, slug)
	}

	sort.Strings(rn.teams)

	var opt []string

	rn.fromRequired, opt = namesFor(cfg, org, to, from)
	for _, n := range append(append([]string{}, rn.fromRequired...), opt...) {
		rn.fromSet[n] = true
	}

	rn.toRequired, opt = namesFor(cfg, org, to, to)
	for _, n := range append(append([]string{}, rn.toRequired...), opt...) {
		rn.toSet[n] = true
	}

	for row := range orgCfg.Repos {
		if row == to {
			continue
		}

		req, opt := namesFor(cfg, org, row, row)
		for _, n := range append(req, opt...) {
			rn.owned[n] = true
		}
	}

	return rn
}

// ambiguous lists names that both the old and the new spelling of the row
// would claim (a repository "a" renamed to "a-b" with a ruleset called
// "b-x" and one called "x").
func (rn *renamer) ambiguous() []string {
	var out []string

	for n := range rn.fromSet {
		if rn.toSet[n] {
			out = append(out, n)
		}
	}

	sort.Strings(out)

	return out
}

// carriesFrom reports whether the name is spelled the way a resource of a
// repository called `from` is spelled.
func (rn *renamer) carriesFrom(name string) bool {
	_, ok := rn.rename(name)

	return ok
}

func (rn *renamer) moveName(name string) string {
	n, _ := rn.rename(name)

	return n
}

// rename maps a resource name carrying the old repository name to the name
// carrying the new one, in the engine's six shapes.
func (rn *renamer) rename(name string) (string, bool) {
	for _, p := range []string{prefixRepo, prefixActions, prefixWorkflowPerm, prefixProtection} {
		if name == p+rn.from {
			return p + rn.to, true
		}
	}

	if rest, ok := strings.CutPrefix(name, prefixRuleset+rn.from+"-"); ok {
		return prefixRuleset + rn.to + "-" + rest, true
	}

	for _, slug := range rn.teams {
		if name == prefixGrant+slug+"-"+rn.from {
			return prefixGrant + slug + "-" + rn.to, true
		}
	}

	return "", false
}

// rekeyOwn rewrites the fields of a moved resource that spell the name.
func rekeyOwn(res map[string]any, from, to string) {
	isRepo := strings.HasSuffix(fmt.Sprint(res["type"]), repositoryTypeSuffix)

	for _, k := range []string{"id", "importID"} {
		if s, ok := res[k].(string); ok {
			res[k] = renameToken(s, from, to)
		}
	}

	for _, k := range []string{"inputs", "outputs"} {
		if v, ok := res[k]; ok {
			res[k] = rewrite(v, from, to, isRepo)
		}
	}
}

func rewrite(v any, from, to string, isRepo bool) any {
	switch x := v.(type) {
	case string:
		return renameToken(x, from, to)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = rewrite(e, from, to, isRepo)
		}

		return out
	case map[string]any:
		out := make(map[string]any, len(x))

		for k, e := range x {
			if freeTextKeys[k] || k == "name" && !isRepo {
				out[k] = e

				continue
			}

			out[k] = rewrite(e, from, to, isRepo)
		}

		return out
	default:
		return v
	}
}

// renameToken replaces `from` with `to` where it is the whole value or a
// whole path, URL or id segment: bounded on the left by the start or "/" or
// ":", and on the right by the end, "/", ":" or a closing ".git". A match
// inside a longer word never moves.
func renameToken(s, from, to string) string {
	var b strings.Builder

	last := 0

	for i := 0; i+len(from) <= len(s); {
		j := strings.Index(s[i:], from)
		if j < 0 {
			break
		}

		start := i + j
		end := start + len(from)

		leftOK := start == 0 || s[start-1] == '/' || s[start-1] == ':'
		rightOK := end == len(s) || s[end] == '/' || s[end] == ':' || s[end:] == ".git"

		if leftOK && rightOK {
			b.WriteString(s[last:start])
			b.WriteString(to)

			last = end
		}

		i = start + 1
	}

	b.WriteString(s[last:])

	return b.String()
}

// followRefs points every reference in res at the moved URNs and returns
// how many it changed.
func followRefs(res map[string]any, moved map[string]string) int {
	n := 0

	follow := func(u string) string {
		if next, ok := moved[u]; ok {
			n++

			return next
		}

		return u
	}

	for _, k := range []string{"parent", "deletedWith", "viewOf"} {
		if s, ok := res[k].(string); ok {
			res[k] = follow(s)
		}
	}

	// A provider reference is "<urn>::<id>".
	if s, ok := res["provider"].(string); ok {
		if i := strings.LastIndex(s, "::"); i >= 0 {
			res["provider"] = follow(s[:i]) + s[i:]
		}
	}

	for _, k := range []string{"dependencies", "aliases", "replaceWith"} {
		followList(res[k], follow)
	}

	if pd, ok := res["propertyDependencies"].(map[string]any); ok {
		for _, l := range pd {
			followList(l, follow)
		}
	}

	return n
}

func followList(v any, follow func(string) string) {
	list, _ := v.([]any)
	for i, e := range list {
		if s, ok := e.(string); ok {
			list[i] = follow(s)
		}
	}
}

func encode(root map[string]any) ([]byte, error) {
	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "    ")

	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("encode state: %w", err)
	}

	return buf.Bytes(), nil
}
