package factoryd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// newRollHarness is a roll task's run: Reconcile reports a roll, RunSession
// fails the test if called, RollCommit answers heads in order (the last
// repeating), and the policy classes the k8s/ change high.
func newRollHarness(t *testing.T, heads ...string) (*gateHarness, *[]string) {
	return newRollHarnessWith(t, func(*gateHarness) {}, heads...)
}

// newRollHarnessWith is newRollHarness with setup run before the mocks.
func newRollHarnessWith(t *testing.T, setup func(*gateHarness), heads ...string) (*gateHarness, *[]string) {
	g := newGateHarness(t, func(g *gateHarness) {
		g.gate = PolicyGate{Class: "high", Outcome: policy.OutcomeRequiresApproval, PolicyVersion: 7, Changed: 2, Paths: []string{"k8s/factoryd.yaml"}}
		g.review = Review{Note: "no model review: a roll task makes no model call"}
		setup(g)
	})
	g.state = RunState{Roll: true}
	g.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			t.Errorf("a roll task ran a session: %+v", in)
			return SessionResult{}, nil
		})
	var commits []string
	g.env.OnActivity(ActivityRollCommit, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) (string, error) {
			commits = append(commits, g.env.Now().Format(time.TimeOnly))
			return heads[min(len(commits), len(heads))-1], nil
		})
	return g, &commits
}

// A roll task runs no session: RollCommit writes the branch and its head
// goes straight to the pull request, the checks, the gates and the card;
// the approval merges it as for any task.
func TestWorkflowRollGoesStraightToChecks(t *testing.T) {
	g, commits := newRollHarness(t, "r1")
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(*commits) != 1 || len(g.sessions) != 0 {
		t.Fatalf("RollCommit calls %v, sessions %v", *commits, g.sessions)
	}
	if want := []string{"policy", "repro r1", "review r1", "approval r1", "gap", "merge", "settle"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
	if !slices.Equal(g.merges, []string{"9 r1 sebastian"}) || g.approvals[0].Gates.Policy.Class != "high" {
		t.Fatalf("merges %v, approval %+v", g.merges, g.approvals[0])
	}
	// a roll makes no model call (ruling 178): its review's note is no
	// finding; the card says the merge waits for a session gap
	for _, w := range g.approvals[0].WhyHuman {
		if strings.Contains(w, noVerdict) {
			t.Fatalf("why human = %q", g.approvals[0].WhyHuman)
		}
	}
	if !slices.Contains(g.approvals[0].WhyHuman, RollGapNote) {
		t.Fatalf("why human = %q, want %q", g.approvals[0].WhyHuman, RollGapNote)
	}
	if !slices.Equal(g.states, []string{"running", "in-review"}) {
		t.Fatalf("states = %v", g.states)
	}
}

// Newer images folded into the open roll arrive as a change request: the
// run has RollCommit write them and takes the new head through the stages
// and a new card; the approval merges the new head.
func TestWorkflowRollFoldsANewHeadBeforeItsMerge(t *testing.T) {
	g, commits := newRollHarness(t, "r1", "r2")
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalRollFolded, TaskSignal{Author: "factory-agent", PR: 9, Body: "roll c2c2c2c: worker folded in"})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	want := []string{"policy", "repro r1", "review r1", "approval r1", "policy", "repro r2", "review r2", "approval r2", "gap", "merge", "settle"}
	if !slices.Equal(g.steps, want) || len(*commits) != 2 || len(g.sessions) != 0 {
		t.Fatalf("steps = %v, want %v; RollCommit calls %v", g.steps, want, *commits)
	}
	if !slices.Equal(g.merges, []string{"9 r2 sebastian"}) {
		t.Fatalf("merges = %v", g.merges)
	}
	if !slices.Equal(g.states, []string{"running", "in-review", "running", "in-review"}) {
		t.Fatalf("states = %v", g.states)
	}
}

// Red checks on a head RollCommit leaves as it is fail the roll: no agent
// answers findings on a roll, and the next green commit opens the next.
func TestWorkflowRollFailsOnFindingsItCannotAnswer(t *testing.T) {
	g, commits := newRollHarness(t, "r1")
	red := "check \"pr / test (pull_request)\": failure"
	g.onChecks(ChecksResult{State: ChecksFailure, Findings: []string{red}})
	if final := g.run(t); final != "failed" {
		t.Fatalf("final = %q", final)
	}
	if len(*commits) != 2 || len(g.sessions) != 0 || len(g.steps) != 0 {
		t.Fatalf("RollCommit calls %v, sessions %v, steps %v", *commits, g.sessions, g.steps)
	}
	if reason := g.finals[0][1]; reason != "roll: checks failed: "+red {
		t.Fatalf("reason = %q", reason)
	}
}

// A healed roll run adopts its open pull request: RollCommit returns the
// head as it is and the run verifies it, no session.
func TestWorkflowRollHealAdoptsItsPullRequest(t *testing.T) {
	g, commits := newRollHarness(t, "r1")
	g.state = RunState{Roll: true, PR: PullRequest{Number: 9}, Head: "r1"}
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalPRClosed, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "failed" || !strings.Contains(g.finals[0][1], "closed without merge") {
		t.Fatalf("final = %q %v", final, g.finals)
	}
	if len(*commits) != 1 || len(g.sessions) != 0 || !slices.Equal(g.steps, []string{"policy", "repro r1", "review r1", "approval r1"}) {
		t.Fatalf("RollCommit calls %v, steps %v", *commits, g.steps)
	}
}

// A fold the previous round's RollCommit already wrote (inserted between
// its drain and its read) wakes the approval wait on an unmoved head: the
// run takes the head through the stages again and never counts the fold
// as a finding.
func TestWorkflowRollFoldOnAnUnmovedHeadIsNoFinding(t *testing.T) {
	g, commits := newRollHarness(t, "r1")
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalRollFolded, TaskSignal{Author: "factory-agent", PR: 9, Body: "roll c2c2c2c: worker folded in"})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	want := []string{"policy", "repro r1", "review r1", "approval r1", "policy", "repro r1", "review r1", "approval r1", "gap", "merge", "settle"}
	if !slices.Equal(g.steps, want) || len(*commits) != 2 {
		t.Fatalf("steps = %v, want %v; RollCommit calls %v", g.steps, want, *commits)
	}
}

// A human's change request on a roll's unmoved head fails it: no agent
// answers it.
func TestWorkflowRollFailsOnAHumansChangeRequest(t *testing.T) {
	g, _ := newRollHarness(t, "r1")
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", PR: 9, Body: "not now"})
	if final := g.run(t); final != "failed" || g.finals[0][1] != "roll: changes requested: reviewer sebastian requested changes: not now" {
		t.Fatalf("final = %q %v", final, g.finals)
	}
}

// Every merge of a roll waits for a session gap first, and its hold ends
// with the merge, a merge that failed or was refused too: the retry and
// the next approval wait for a gap again; a merged one's hold settles
// until the restart its merge brings.
func TestWorkflowRollMergesInASessionGap(t *testing.T) {
	g, _ := newRollHarness(t, "r1")
	g.merge = []MergeResult{{Failed: "forgejo answered 502"}, {Refusal: "the approval is stale"}, {Merged: true}}
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	want := []string{"policy", "repro r1", "review r1", "approval r1", "gap", "merge", "clear", "gap", "merge", "clear", "gap", "merge", "settle"}
	if !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
}

// A roll whose pull request closes while it waits for the gap ends there:
// no merge; its Finish ends the hold.
func TestWorkflowRollClosedWhileWaitingForTheGap(t *testing.T) {
	g, _ := newRollHarnessWith(t, func(g *gateHarness) { g.gapDelay = 2 * time.Hour }, "r1")
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(2*time.Hour, SignalPRClosed, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "failed" || !strings.Contains(g.finals[0][1], "closed without merge") {
		t.Fatalf("final = %q %v", final, g.finals)
	}
	if len(g.merges) != 0 || slices.Contains(g.steps, "merge") {
		t.Fatalf("merges %v, steps %v; want none", g.merges, g.steps)
	}
}

// A card's session waits while a roll holds sessions, before the session's
// agent time starts: the wait is no agent time and needs no session time.
func TestWorkflowSessionWaitsForARollsHold(t *testing.T) {
	h := newWorkflowHarness(t)
	var calls []string
	h.env.OnActivity(ActivityAwaitRollHold, mock.Anything, "t1").After(2 * time.Hour).Return(
		func(ctx context.Context, taskID string) error {
			calls = append(calls, "hold")
			return nil
		})
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(10 * time.Minute).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			calls = append(calls, "session")
			h.sessions = append(h.sessions, in)
			return pushed("s1", "c1"), nil
		})
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	if !slices.Equal(calls, []string{"hold", "session"}) {
		t.Fatalf("calls = %v, want the hold's wait, then the session", calls)
	}
	if len(h.usage) == 0 || h.usage[0].AgentTime != 10*time.Minute {
		t.Fatalf("usage = %+v, want 10m of agent time, the session's only", h.usage)
	}
	if !slices.Equal(h.states, []string{"running", "in-review"}) {
		t.Fatalf("states = %v, want running while held", h.states)
	}
}

// A planner session waits for a roll's hold the same way: no agent time.
func TestPlanSessionWaitsForARollsHold(t *testing.T) {
	h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"})
	plansBefore := -1
	h.env.OnActivity(ActivityAwaitRollHold, mock.Anything, "t1").After(time.Hour).Return(
		func(ctx context.Context, taskID string) error {
			plansBefore = len(h.plans)
			return nil
		})
	h.signalAfter(3*time.Hour, SignalPlanAccepted, TaskSignal{Author: "sebastian", Revision: 1, SHA256: "sha-1"})
	if final := h.run(t); final != "done" {
		t.Fatalf("finals %v", h.finals)
	}
	if plansBefore != 0 || len(h.plans) != 1 {
		t.Fatalf("planner sessions before the hold's wait %d, after %d; want the wait first, then one", plansBefore, len(h.plans))
	}
	if len(h.usage) != 1 || h.usage[0].AgentTime != planSession {
		t.Fatalf("usage %+v, want %s of agent time, the session's only", h.usage, planSession)
	}
}

// A roll under a project's merge queue takes no turn in it: its stages go
// straight to the approval with no EnterQueue and no SyncWithMain, so a
// card merges past a roll that waits (roadmap 222, item 1).
func TestWorkflowRollTakesNoQueueTurn(t *testing.T) {
	g, commits := newRollHarnessWith(t, func(g *gateHarness) {
		g.taskConfig = &TaskConfig{Process: "spec", Lane: policy.LaneReady, Class: policy.ClassCoder, Session: policy.SessionCode,
			Stages: testStages(), Queue: policy.QueueProject}
	}, "r1")
	g.env.OnActivity(ActivityEnterQueue, mock.Anything, "t1").Return(func(ctx context.Context, taskID string) (int, error) {
		t.Errorf("a roll entered the merge queue")
		return 0, nil
	})
	g.env.OnActivity(ActivitySyncWithMain, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr int64, from string) (SyncResult, error) {
			t.Errorf("a roll synced with main")
			return SyncResult{From: from, To: from}, nil
		})
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(*commits) != 1 {
		t.Fatalf("RollCommit calls %v", *commits)
	}
	if want := []string{"policy", "repro r1", "review r1", "approval r1", "gap", "merge", "settle"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
}
