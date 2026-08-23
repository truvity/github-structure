// Package preflight is the guard that runs BEFORE the structure engine:
// a read-only judgement over the org's registry, its stack state and a
// `pulumi preview`, refusing the plans history shows destroy things — a
// replacement of an irreplaceable resource (a team's membership, a
// branch protection's standing), a guarded delete, an import-ID
// mismatch, an unfillable granted team, a waiver that outlived its
// cause. Wire Run as a hard dependency of the deploy command; see
// docs/safety.md for each guard and the incident that earned it.
package preflight

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"

	registry "github.com/truvity/github-structure/pkg/registry"
)

// irreplaceable lists resource types whose real state lives OUTSIDE this
// stack, so a replace destroys something Pulumi cannot put back and does
// not know it lost.
//
// Teams are the whole reason this command exists. Their membership is the
// roster's (INF-484) and appears nowhere in this state; a replace deletes
// the team and creates a new one with the same slug, so every diff after
// it reads clean while the members are simply gone. That is not
// hypothetical — see the incident note on replaceOps below.
//
// Branch protection belongs here for the same reason in reverse: the
// delete half leaves a branch unprotected and the next diff reads clean
// (the 2026-07-29 finding recorded in the github component's package doc).
// Repositories are here because in THIS stack a replace means GitHub
// deletes the repository and makes a new empty one — the history, the
// issues, the stars, gone. There is no routine reason to replace a
// repository row, and `archiveOnDestroy` is client-side only (unreadable,
// per the component's own note), so nothing softens it. 62 of them carry
// protect:true, which the 08-07 team replace proved is not a backstop.
var irreplaceable = map[string]string{
	teamType:       "team membership is the roster's and is not in this state — a replace drops every member silently",
	repoType:       "a repository replace DELETES the repository and its entire history",
	protectionType: "the delete half leaves the branch unprotected and the next diff reads clean",
}

// replaceOps are the engine ops that destroy and recreate. Pulumi reports
// a replacement as `replace` in a summary preview and splits it into
// create-replacement/delete-replaced when the steps are shown separately,
// so all of them have to be caught.
//
// Why a preflight rather than pulumi's own guard: on 2026-08-07 an `up`
// applied `replace: 13` — every team in the org — against a state where
// all twelve teams carried `protect: true`. The old teams are 404 today,
// so protect did NOT stop the delete. Pulumi's protection cannot be the
// only thing standing between a plan and a silent org-wide access loss.
var replaceOps = map[string]bool{
	"replace":                true,
	"create-replacement":     true,
	"delete-replaced":        true,
	"import-replacement":     true,
	"discard-replaced":       true,
	"remove-pending-replace": true,
}

// deleteGuarded lists resource types whose plain DELETE is dangerous
// enough to stop a run, separately from replacement.
//
// Branch protection only. Its delete is the archival wedge: an archived
// row stops declaring protection, Pulumi orders deletes AFTER updates, so
// the repository archives first and the delete then answers
// "Repository is archived". The run fails half-applied and every
// subsequent run retries the same doomed delete — the stack is stuck
// until someone removes the entries from state by hand.
//
// That is not hypothetical: it happened on 2026-08-17 archiving the six
// TrustForm copies. Six protections errored, twelve Actions resources
// deleted cleanly because they carry RetainOnDelete, and the six had to
// be dropped from state to recover.
//
// RetainOnDelete would have prevented it too, and is deliberately NOT
// used here. For Actions permissions, dropping the resource means "stop
// managing this setting" and retaining is exactly right. For protection,
// dropping it can equally mean "this branch should no longer be
// protected" — retaining would silently leave a live protection rule
// nobody manages, which is the opposite of a safe default for a security
// control. So the delete stays real, and this guard makes it impossible
// to run one by accident.
var deleteGuarded = map[string]string{
	protectionType: "deleting branch protection 403s once the repo is archived and wedges the stack; " +
		"if this is an archival, remove it from state first (docs/operations/github-repo-archival.md)",
}

// deleteOps are the engine ops that destroy without recreating.
var deleteOps = map[string]bool{
	"delete": true,
}

// plannedReplace is one resource the plan would destroy and recreate.
// Pulumi emits a replacement as several events (replace, then
// create-replacement/delete-replaced), so ops is a set: reporting one line
// per event turned 12 doomed teams into 36 lines of noise.
type (
	plannedReplace struct {
		urn    string
		typ    string
		ops    map[string]bool
		reason string
	}
)

// opList renders the collected ops in a stable order.
func (p *plannedReplace) opList() string {
	ops := make([]string, 0, len(p.ops))
	for op := range p.ops {
		ops = append(ops, op)
	}

	sort.Strings(ops)

	return strings.Join(ops, ", ")
}

type (
	// Opts wires one preflight run. The caller selects the stack (via
	// Pulumi's Automation API) and loads the registry; the preflight
	// only judges.
	Opts struct {
		// Org being deployed — must have a registry row.
		Org string
		// Registry is the loaded, validated registry.
		Registry *registry.Config
		// Stack is the org's structure stack, already selected.
		Stack auto.Stack
		// TeamFillable reports whether SOME system refills the named
		// team's membership (an IdP-driven roster, a sync job). Teams
		// holding repository grants that nothing can refill are refused
		// — an emptied one stays empty (docs/safety.md). nil means "no
		// team is refillable", the safe default.
		TeamFillable func(slug string) bool
	}
)

// Run previews the org's structure stack and fails when the plan — or
// the state it would apply to — contains a catastrophe: a replacement of
// an irreplaceable resource, a guarded delete, an import-ID mismatch, an
// unfillable granted team, or a waiver that outlived its cause.
//
// Run this before EVERY `pulumi up` on a structure stack, as a hard
// dependency of the deploy command. It is a read-only preview: it
// changes nothing, and its only job is to turn a silent catastrophe
// into a non-zero exit code.
func Run(ctx context.Context, o Opts) error {
	org := o.Org
	reg := o.Registry

	if _, ok := reg.Orgs[org]; !ok {
		return fmt.Errorf("org %q not in the registry (known: %v)", org, reg.SortedOrgs())
	}

	stack := o.Stack
	fillable := o.TeamFillable
	if fillable == nil {
		fillable = func(string) bool { return false }
	}

	// Two state-level checks first: they are cheap, they need no plan, and
	// each catches the outage a step earlier than the plan does.
	var problems []string

	importProblems, err := checkImportIDs(ctx, stack, org)
	if err != nil {
		return err
	}

	problems = append(problems, importProblems...)

	memberProblems, err := checkRobotTeams(ctx, reg, org, fillable)
	if err != nil {
		return err
	}

	problems = append(problems, memberProblems...)

	waiverProblems, err := checkStaleWaivers(ctx, reg, org)
	if err != nil {
		return err
	}

	problems = append(problems, waiverProblems...)

	// Engine events carry the per-resource op; the summary alone would say
	// "13 to replace" without naming what, which is not enough to decide
	// whether a replacement is the legitimate kind.
	eventCh := make(chan events.EngineEvent)
	found := make(chan []plannedReplace, 1)

	go collectReplaces(eventCh, found)

	_, err = stack.Preview(ctx,
		optpreview.ErrorProgressStreams(os.Stderr),
		optpreview.EventStreams(eventCh),
	)
	if err != nil {
		return fmt.Errorf("preview stack %s: %w", org, err)
	}

	replaces := <-found

	if len(problems) == 0 && len(replaces) == 0 {
		_, _ = fmt.Fprintf(os.Stdout,
			"preflight OK — %s: every imported resource's ID matches state, "+
				"no unfillable team left empty, no waiver outlived its cause, "+
				"nothing irreplaceable planned for replacement\n", org)

		return nil
	}

	return report(org, problems, replaces)
}

// report prints everything wrong at once. Fixing one problem only to be
// told about the next is how a guard earns a habit of being bypassed.
func report(org string, problems []string, replaces []plannedReplace) error {
	var b strings.Builder

	if len(problems) > 0 {
		_, _ = fmt.Fprintf(&b, "\nREFUSING: %s has %d state-level problem(s).\n\n", org, len(problems))

		for _, p := range problems {
			_, _ = fmt.Fprintf(&b, "  • %s\n\n", p)
		}
	}

	if len(replaces) > 0 {
		writeReplaces(&b, org, replaces)
	}

	_, _ = fmt.Fprint(os.Stdout, b.String())

	return fmt.Errorf("preflight failed: %d state problem(s), %d planned replacement(s)",
		len(problems), len(replaces))
}

// collectReplaces drains the engine event stream, which must be consumed
// to completion or Preview blocks.
func collectReplaces(eventCh <-chan events.EngineEvent, out chan<- []plannedReplace) {
	byURN := map[string]*plannedReplace{}

	for e := range eventCh {
		if e.ResourcePreEvent == nil {
			continue
		}

		meta := e.ResourcePreEvent.Metadata

		reason, guarded := irreplaceable[meta.Type]
		guarded = guarded && replaceOps[string(meta.Op)]

		// A plain delete of a guarded type is its own hazard, with its
		// own explanation — see deleteGuarded.
		if !guarded {
			if r, ok := deleteGuarded[meta.Type]; ok && deleteOps[string(meta.Op)] {
				reason, guarded = r, true
			}
		}

		if !guarded {
			continue
		}

		p, seen := byURN[meta.URN]
		if !seen {
			p = &plannedReplace{
				urn:    meta.URN,
				typ:    meta.Type,
				ops:    map[string]bool{},
				reason: reason,
			}
			byURN[meta.URN] = p
		}

		p.ops[string(meta.Op)] = true
	}

	found := make([]plannedReplace, 0, len(byURN))
	for _, p := range byURN {
		found = append(found, *p)
	}

	sort.Slice(found, func(i, j int) bool { return found[i].urn < found[j].urn })

	out <- found
}

func writeReplaces(b *strings.Builder, org string, replaces []plannedReplace) {
	_, _ = fmt.Fprintf(b, "\nREFUSING: %s plans to replace %d resource(s) whose state lives outside this stack.\n\n",
		org, len(replaces))

	for i := range replaces {
		r := &replaces[i]
		_, _ = fmt.Fprintf(b, "  %s\n", shortURN(r.urn))
		_, _ = fmt.Fprintf(b, "      ops:    %s\n", r.opList())
		_, _ = fmt.Fprintf(b, "      why it matters: %s\n\n", r.reason)
	}

	b.WriteString("A replacement here is almost never what you meant. Before overriding:\n")
	b.WriteString("  1. Read the diff. An empty replaceKeys means the provider found no\n")
	b.WriteString("     property that requires replacement — that is a phantom, not a change.\n")
	b.WriteString("  2. Check whether the declared pulumi.Import ID changed, or whether the\n")
	b.WriteString("     resource sits in state with no importID. Either makes Pulumi replace\n")
	b.WriteString("     rather than adopt (INF-530).\n")
	b.WriteString("  3. protect:true will NOT save you — it did not on 2026-08-07.\n")
}

// shortURN trims the stack/project prefix that is the same on every line.
func shortURN(urn string) string {
	if i := strings.LastIndex(urn, "::"); i >= 0 {
		return urn[i+2:]
	}

	return urn
}
