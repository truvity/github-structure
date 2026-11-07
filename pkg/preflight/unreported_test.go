package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type (
	// fakeRun is one check run as the API reports it: a name and a
	// status (queued, in_progress, completed).
	fakeRun struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}

	// fakeCommit is one commit of a made-up history.
	fakeCommit struct {
		SHA    string
		Parent string
		Runs   []fakeRun
	}

	// fakeGitHub answers the three reads the unreported-check guard
	// makes, from a made-up repository history. A path it does not know is
	// an error, so a guard that starts reading something new fails loudly.
	fakeGitHub struct {
		// commits is keyed by ref: the branch name for the tip, and the
		// sha for everything else.
		commits map[string]fakeCommit
		// pulls maps the `base` query to the head sha of the latest pull
		// request into that branch; the empty key is the latest pull
		// request overall, which is what an unfiltered query returns.
		pulls map[string]string
	}
)

func (f *fakeGitHub) get(_ context.Context, path string, dst any) error {
	u, err := url.Parse(path)
	if err != nil {
		return err
	}

	var body any

	switch parts := strings.Split(strings.Trim(u.Path, "/"), "/"); {
	case len(parts) == 5 && parts[0] == "repos" && parts[3] == "commits":
		commit, ok := f.commits[parts[4]]
		if !ok {
			return fmt.Errorf("fake: no commit %q", parts[4])
		}

		type sha struct {
			SHA string `json:"sha"`
		}

		parents := []sha{}
		if commit.Parent != "" {
			parents = append(parents, sha{commit.Parent})
		}

		body = map[string]any{"sha": commit.SHA, "parents": parents}
	case len(parts) == 6 && parts[0] == "repos" && parts[3] == "commits" && parts[5] == "check-runs":
		commit, ok := f.commits[parts[4]]
		if !ok {
			return fmt.Errorf("fake: no commit %q", parts[4])
		}

		runs := commit.Runs
		if runs == nil {
			runs = []fakeRun{}
		}

		body = map[string]any{"check_runs": runs}
	case len(parts) == 4 && parts[0] == "repos" && parts[3] == "pulls":
		type head struct {
			SHA string `json:"sha"`
		}

		type pull struct {
			Head head `json:"head"`
		}

		pulls := []pull{}
		if sha, ok := f.pulls[u.Query().Get("base")]; ok {
			pulls = append(pulls, pull{head{sha}})
		}

		body = pulls
	default:
		return fmt.Errorf("fake: unexpected path %q", path)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}

	return json.Unmarshal(raw, dst)
}

func done(names ...string) []fakeRun {
	runs := make([]fakeRun, 0, len(names))
	for _, name := range names {
		runs = append(runs, fakeRun{name, "completed"})
	}

	return runs
}

// history builds a linear history, newest first: the first element is
// the branch tip, reachable as "master"; each commit's parent is the next.
func history(commits ...fakeCommit) map[string]fakeCommit {
	out := map[string]fakeCommit{}

	for i := range commits {
		if i+1 < len(commits) {
			commits[i].Parent = commits[i+1].SHA
		}

		out[commits[i].SHA] = commits[i]
	}

	out["master"] = commits[0]

	return out
}

func problemsOf(t *testing.T, gh *fakeGitHub) []string {
	t.Helper()

	problems, err := unreportedChecks(t.Context(), gh.get, loadWaiverFixture(t), "acme")
	require.NoError(t, err)

	return problems
}

// A tip whose CI is still queued or running has not reported yet: that
// is not yet known, and it must not read as "never reported". The
// answer is on the commit before it, whose CI has finished.
func TestUnreportedChecksPendingTipIsNotYetKnown(t *testing.T) {
	for name, tc := range map[string]struct {
		status string
	}{
		"queued":      {status: "queued"},
		"in progress": {status: "in_progress"},
	} {
		t.Run(name, func(t *testing.T) {
			gh := &fakeGitHub{commits: history(
				fakeCommit{SHA: "tip1", Runs: []fakeRun{{"lint", tc.status}}},
				fakeCommit{SHA: "prev", Runs: done("check", "lint")},
			)}

			assert.Empty(t, problemsOf(t, gh))
		})
	}

	t.Run("two pending tips in a row", func(t *testing.T) {
		gh := &fakeGitHub{commits: history(
			fakeCommit{SHA: "tip1", Runs: []fakeRun{{"lint", "queued"}}},
			fakeCommit{SHA: "mid1", Runs: []fakeRun{{"lint", "in_progress"}}},
			fakeCommit{SHA: "prev", Runs: done("check")},
		)}

		assert.Empty(t, problemsOf(t, gh))
	})
}

// A context that has really never been reported still refuses, and
// finished runs are never a reason to look further back: only a pending
// tip is "not yet known".
func TestUnreportedChecksNeverReportedStillRefuses(t *testing.T) {
	gh := &fakeGitHub{commits: history(
		fakeCommit{SHA: "tip1", Runs: done("lint", "renovate")},
		// Reporting on an older commit is not evidence for a tip whose
		// CI has finished without it.
		fakeCommit{SHA: "prev", Runs: done("check")},
	)}

	problems := problemsOf(t, gh)

	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], `repo "gadget"`)
	assert.Contains(t, problems[0], "checks_waived")
}

// A tip with no runs at all, all of them finished elsewhere, is the
// same: nothing is pending, so nothing is "yet".
func TestUnreportedChecksNoRunsStillRefuses(t *testing.T) {
	gh := &fakeGitHub{commits: history(fakeCommit{SHA: "tip1"})}

	problems := problemsOf(t, gh)

	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "checks_waived")
}

// When CI is pending and there is nothing earlier to learn from, the
// run cannot pass, but the message must say what is true: wait for the
// run, not add a waiver.
func TestUnreportedChecksPendingWithoutEvidenceSaysWait(t *testing.T) {
	for name, commits := range map[string]map[string]fakeCommit{
		"previous commit finished without it": history(
			fakeCommit{SHA: "tip1", Runs: []fakeRun{{"lint", "in_progress"}}},
			fakeCommit{SHA: "prev", Runs: done("lint")},
		),
		"CI pending on every commit walked": history(
			fakeCommit{SHA: "c1", Runs: []fakeRun{{"lint", "queued"}}},
			fakeCommit{SHA: "c2", Runs: []fakeRun{{"lint", "queued"}}},
			fakeCommit{SHA: "c3", Runs: []fakeRun{{"lint", "queued"}}},
			fakeCommit{SHA: "c4", Runs: []fakeRun{{"lint", "queued"}}},
			fakeCommit{SHA: "c5", Runs: []fakeRun{{"lint", "queued"}}},
			fakeCommit{SHA: "c6", Runs: []fakeRun{{"lint", "queued"}}},
			fakeCommit{SHA: "c7", Runs: []fakeRun{{"lint", "queued"}}},
			fakeCommit{SHA: "c8", Runs: []fakeRun{{"lint", "queued"}}},
			fakeCommit{SHA: "c9", Runs: []fakeRun{{"lint", "queued"}}},
			fakeCommit{SHA: "c10", Runs: []fakeRun{{"lint", "queued"}}},
			fakeCommit{SHA: "c11", Runs: []fakeRun{{"lint", "queued"}}},
			fakeCommit{SHA: "c12", Runs: []fakeRun{{"lint", "queued"}}},
		),
	} {
		t.Run(name, func(t *testing.T) {
			problems := problemsOf(t, &fakeGitHub{commits: commits})

			require.Len(t, problems, 1)
			assert.NotContains(t, problems[0], "checks_waived")
			assert.Contains(t, problems[0], "still queued or running")
		})
	}
}

// The second window is the latest pull request INTO the default
// branch. A newer pull request into another branch says nothing about
// the context the default branch's rule requires.
func TestUnreportedChecksPullWindowIsTheDefaultBranch(t *testing.T) {
	commits := history(
		fakeCommit{SHA: "tip1", Runs: done("lint")},
		fakeCommit{SHA: "other", Runs: done("check")},
		fakeCommit{SHA: "intoMaster", Runs: done("check")},
	)

	t.Run("a newer pull request into another branch is not evidence", func(t *testing.T) {
		gh := &fakeGitHub{commits: commits, pulls: map[string]string{"": "other"}}

		assert.Len(t, problemsOf(t, gh), 1)
	})

	t.Run("a pull request into the default branch is", func(t *testing.T) {
		gh := &fakeGitHub{commits: commits, pulls: map[string]string{"": "other", "master": "intoMaster"}}

		assert.Empty(t, problemsOf(t, gh))
	})
}
