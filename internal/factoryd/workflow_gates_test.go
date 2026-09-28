package factoryd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// gateHarness scripts the gates: steps records every gate, review,
// approval and merge call in order.
type gateHarness struct {
	*workflowHarness
	steps     []string
	approvals []ApprovalInput
	merges    []string // "pr commit approver"
	gate      PolicyGate
	repro     []ReproResult // in order, the last repeating
	review    Review
	merge     []MergeResult // in order, the last repeating
	// policyDelay is how long GatePolicy takes; reviewErr fails ModelReview
	policyDelay   time.Duration
	reviewErr     error
	reviewDelay   time.Duration
	policyCommits []string
	started       time.Time
}

// newGateHarness registers the gate mocks after setup adjusted the
// defaults: a normal class, no reproduction, an approving review, a
// merge that goes through.
func newGateHarness(t *testing.T, setup ...func(*gateHarness)) *gateHarness {
	g := &gateHarness{workflowHarness: newWorkflowHarness(t),
		gate:   PolicyGate{Class: "normal", Outcome: policy.OutcomeRequiresApproval, PolicyVersion: 7, Changed: 2},
		repro:  []ReproResult{{}},
		review: Review{Verdict: "approve", Summary: "does what the brief asks"},
		merge:  []MergeResult{{Merged: true}},
	}
	for _, f := range setup {
		f(g)
	}
	g.started = g.env.Now()
	g.env.OnActivity(ActivityGatePolicy, mock.Anything, "t1", mock.Anything, mock.Anything).After(g.policyDelay).Return(
		func(ctx context.Context, taskID string, pr int64, commit string) (PolicyGate, error) {
			g.policyCommits = append(g.policyCommits, commit)
			g.steps = append(g.steps, "policy")
			return g.gate, nil
		})
	g.env.OnActivity(ActivityGateMigration, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr int64, migrations []string) error {
			g.steps = append(g.steps, "migration "+strings.Join(migrations, ","))
			return nil
		})
	n := 0
	g.env.OnActivity(ActivityGateReproduction, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr int64, commit string) (ReproResult, error) {
			n++
			g.steps = append(g.steps, "repro "+commit)
			return g.repro[min(n, len(g.repro))-1], nil
		})
	g.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).After(g.reviewDelay).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, gates GateResults) (Review, error) {
			g.steps = append(g.steps, "review "+commit)
			return g.review, g.reviewErr
		})
	g.env.OnActivity(ActivityRequestApproval, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID string, in ApprovalInput) error {
			g.steps = append(g.steps, "approval "+in.Commit)
			g.approvals = append(g.approvals, in)
			return nil
		})
	m := 0
	g.env.OnActivity(ActivityMerge, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr int64, commit, approver string) (MergeResult, error) {
			m++
			g.steps = append(g.steps, "merge")
			g.merges = append(g.merges, fmt.Sprintf("%d %s %s", pr, commit, approver))
			return g.merge[min(m, len(g.merge))-1], nil
		})
	return g
}

// Green checks go through the policy gate, the reproduction and the model
// review to an approval request; the console's approved signal merges as
// factory-merge at the gated head and finishes the task done.
func TestWorkflowGatesThenApprovalMerges(t *testing.T) {
	g := newGateHarness(t)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, g.finals)
	}
	if want := []string{"policy", "repro c1", "review c1", "approval c1", "merge"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
	if !slices.Equal(g.merges, []string{"9 c1 sebastian"}) {
		t.Fatalf("merges = %v", g.merges)
	}
	in := g.approvals[0]
	if in.PR.Number != 9 || in.Gates.Policy.Class != "normal" || in.Review.Verdict != "approve" {
		t.Fatalf("approval input = %+v", in)
	}
	if !strings.Contains(g.finals[0][1], "factory-merge after sebastian's approval") {
		t.Fatalf("reason = %q", g.finals[0][1])
	}
}

// A forbidden risk class fails the task with the paths; no review, no
// approval, no merge.
func TestWorkflowForbiddenClassFailsTheTask(t *testing.T) {
	g := newGateHarness(t)
	g.gate = PolicyGate{Class: "forbidden", Outcome: policy.OutcomeForbidden, Paths: []string{".forgejo/workflows/pr.yml"}, PolicyVersion: 7}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})

	if final := g.run(t); final != "failed" {
		t.Fatalf("final = %q", final)
	}
	if !slices.Equal(g.steps, []string{"policy"}) || !strings.Contains(g.finals[0][1], ".forgejo/workflows/pr.yml") || !strings.Contains(g.finals[0][1], "policy version 7") {
		t.Fatalf("steps = %v, reason = %q", g.steps, g.finals[0][1])
	}
}

// Changed migrations are flagged before the reproduction; a PR without
// any gets no migration gate.
func TestWorkflowMigrationsAreFlagged(t *testing.T) {
	g := newGateHarness(t)
	g.gate.Class, g.gate.Migrations = "migration", []string{"internal/factoryd/migrations/0016_x.sql"}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if g.steps[1] != "migration internal/factoryd/migrations/0016_x.sql" || g.approvals[0].Gates.Policy.Migrations[0] != "internal/factoryd/migrations/0016_x.sql" {
		t.Fatalf("steps = %v", g.steps)
	}
}

// A failed reproduction sends the next attempt back to the agent on the
// stronger rung with the output as findings, like red checks.
func TestWorkflowFailedReproductionIsFindings(t *testing.T) {
	g := newGateHarness(t)
	g.repro = []ReproResult{{Ran: true, Command: "go test ./x", Exit: 1, Output: "--- FAIL: TestX"}, {Ran: true, Command: "go test ./x"}}
	g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if want := []string{"policy", "repro c1", "policy", "repro c2", "review c2", "approval c2", "merge"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
	second := g.sessions[1]
	if second.Model != "factory/coding-frontier" || len(second.Findings) != 1 ||
		!strings.Contains(second.Findings[0], "exit 1") || !strings.Contains(second.Findings[0], "--- FAIL: TestX") || !strings.Contains(second.Findings[0], "go test ./x") {
		t.Fatalf("second attempt = %+v", second)
	}
}

// A merge Forgejo refuses keeps the wait; the next approval merges.
func TestWorkflowRefusedMergeKeepsWaiting(t *testing.T) {
	g := newGateHarness(t)
	g.merge = []MergeResult{{Refusal: "405 not approved"}, {Merged: true}}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" || len(g.merges) != 2 {
		t.Fatalf("final = %q after merges %v", final, g.merges)
	}
}

// A refused merge followed by a human's merge in Forgejo stays valid.
func TestWorkflowHumanMergeAfterARefusalIsDone(t *testing.T) {
	g := newGateHarness(t)
	g.merge = []MergeResult{{Refusal: "405"}}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" || g.finals[0][1] != "pull request merged" || len(g.merges) != 1 {
		t.Fatalf("final = %q (%v) after merges %v", final, g.finals, g.merges)
	}
}

// An approval for another pull request, or one sent before the approval
// request was opened, merges nothing.
func TestWorkflowIgnoresStrayApprovals(t *testing.T) {
	g := newGateHarness(t, func(g *gateHarness) { g.policyDelay = time.Hour })
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(30*time.Minute, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 8})
	g.signalAfter(3*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := g.run(t); final != "failed" || len(g.merges) != 0 {
		t.Fatalf("final = %q after merges %v", final, g.merges)
	}
}

// A change request during the approval wait starts a new attempt; its
// head goes through the gates again.
func TestWorkflowChangesDuringApprovalStartANewAttempt(t *testing.T) {
	g := newGateHarness(t)
	g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", Body: "rename it", PR: 9})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if !slices.Equal(g.merges, []string{"9 c2 sebastian"}) || len(g.approvals) != 2 {
		t.Fatalf("merges = %v, approvals = %d", g.merges, len(g.approvals))
	}
	if f := g.sessions[1].Findings; len(f) != 1 || f[0] != "reviewer sebastian requested changes: rename it" {
		t.Fatalf("findings = %v", f)
	}
}

// A model review that errors is a note on the card, not the task's end.
func TestWorkflowFailedModelReviewIsANote(t *testing.T) {
	g := newGateHarness(t, func(g *gateHarness) { g.reviewErr = terminalError("litellm down") })
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if note := g.approvals[0].Review.Note; !strings.Contains(note, "litellm down") {
		t.Fatalf("review note = %q", note)
	}
}

// Workflows started before versionGates keep the plain review wait: no
// gate runs, an approval signal does nothing, a human's merge finishes.
func TestWorkflowBeforeGatesRunsNoGate(t *testing.T) {
	g := newGateHarness(t)
	g.env.OnGetVersion("gates", workflow.DefaultVersion, versionGates).Return(workflow.DefaultVersion)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" || len(g.steps) != 0 {
		t.Fatalf("final = %q, steps = %v", final, g.steps)
	}
}

// GatePolicy gets the gated commit (versionGatesPinned).
func TestWorkflowGatePolicyGetsTheGatedCommit(t *testing.T) {
	g := newGateHarness(t)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || !slices.Equal(g.policyCommits, []string{"c1"}) {
		t.Fatalf("final = %q, GatePolicy commits = %q", final, g.policyCommits)
	}
}

// A terminal signal during the model review decides at once: no approval
// request, no waiting for the review.
func TestWorkflowTerminalSignalDuringTheReviewDecides(t *testing.T) {
	g := newGateHarness(t, func(g *gateHarness) { g.reviewDelay = 3 * time.Hour })
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(10*time.Minute, SignalPRClosed, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "failed" || len(g.approvals) != 0 {
		t.Fatalf("final = %q, approvals %d", final, len(g.approvals))
	}
	if g.finishAt.Sub(g.started) > time.Hour {
		t.Fatalf("finished %s after the start, want right after the close", g.finishAt.Sub(g.started))
	}
}

// A merge that failed (not refused) keeps the approval: it is tried again
// after mergeRetryAfter with the same approver, without a new signal.
func TestWorkflowFailedMergeIsRetriedWithTheSameApproval(t *testing.T) {
	g := newGateHarness(t)
	g.merge = []MergeResult{{Failed: "forgejo 502"}, {Merged: true}}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || !slices.Equal(g.merges, []string{"9 c1 sebastian", "9 c1 sebastian"}) {
		t.Fatalf("final = %q, merges = %v", final, g.merges)
	}
}

// A human's merge found by Merge finishes the task credited to them.
func TestWorkflowMergeFoundDoneByAHuman(t *testing.T) {
	g := newGateHarness(t)
	g.merge = []MergeResult{{Merged: true, By: "sebastian"}}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "alice", PR: 9})
	if final := g.run(t); final != "done" || g.finals[0][1] != "pull request merged by sebastian" {
		t.Fatalf("final = %q (%v)", final, g.finals)
	}
}
