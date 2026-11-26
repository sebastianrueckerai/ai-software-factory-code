package factoryd

import (
	"context"
	"testing"
	"time"
)

const (
	onPullRequests = "on:\n  push:\n  pull_request:\n    types: [opened]\n"
	onPushOnly     = "on:\n  push:\n    branches: [main]\n"
)

// LandPullRequest waits for pending checks: the pull request merges when
// the last read is green, is refused for a failure that appears while it
// waits, and merges at the end of the grace when no check ever reports.
func TestLandWaitsForPendingChecks(t *testing.T) {
	pending := []CommitStatus{{State: "success", Context: "lint"}, {State: "pending", Context: "config-check"}}
	green := []CommitStatus{{State: "success", Context: "lint"}, {State: "success", Context: "config-check"}}
	red := []CommitStatus{{State: "failure", Context: "config-check"}}
	for _, tc := range []struct {
		name    string
		seq     [][]CommitStatus
		grace   time.Duration
		merged  bool
		reads   int
		refusal string
		// workflow is the repository's only workflow file at main
		workflow string
	}{
		{"pending, then green", [][]CommitStatus{pending, pending, green}, 0, true, 3, "", onPullRequests},
		{"none yet, pending, then green", [][]CommitStatus{nil, nil, pending, green}, time.Second, true, 4, "", onPullRequests},
		{"none yet, then a failure", [][]CommitStatus{nil, nil, red}, time.Second, false, 3, "the check “config-check” of its head h1 has the state failure", onPullRequests},
		{"pending with a failure beside it", [][]CommitStatus{{{State: "pending", Context: "a"}, {State: "error", Context: "b"}}}, 0, false, 1, "the check “b” of its head h1 has the state error", onPullRequests},
		{"none ever, the grace passes", [][]CommitStatus{nil}, 20 * time.Millisecond, true, -1, "", onPullRequests},
		// no pull request trigger: no status will come, so no grace is waited and the head is read once
		{"none ever, no pull request trigger", [][]CommitStatus{nil}, time.Hour, true, 1, "", onPushOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fj, c, _, _ := cardRig(t)
			// the rows end by their sequence, never by the bound: a loaded runner spends the rig's 5ms on one read
			landChecksBound = time.Minute
			if tc.grace > 0 {
				landNoStatusGrace = tc.grace
			}
			fj.mergeable, fj.statusSeq = true, tc.seq
			fj.workflows = map[string]string{"ci.yml": tc.workflow}
			got := c.Forgejo.LandPullRequest(context.Background(), c.Config.Repo, 1, Attribution{Role: "schema-copy"}, "t", nil)
			if got != tc.refusal {
				t.Fatalf("refusal %q, want %q", got, tc.refusal)
			}
			fj.mu.Lock()
			defer fj.mu.Unlock()
			if got := len(fj.merges) == 1; got != tc.merged {
				t.Fatalf("merges %q, want merged %v", fj.merges, tc.merged)
			}
			if tc.reads >= 0 && fj.reads != tc.reads {
				t.Fatalf("%d status reads, want %d", fj.reads, tc.reads)
			}
		})
	}
}
