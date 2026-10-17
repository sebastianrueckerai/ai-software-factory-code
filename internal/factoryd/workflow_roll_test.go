package factoryd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/workflow"

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
	ro := 0
	g.env.OnActivity(ActivityAwaitRollout, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr int64, bound time.Duration) (RolloutResult, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			ro++
			g.steps = append(g.steps, "rollout")
			if len(g.rollout) == 0 {
				return RolloutResult{Confirmed: true}, nil
			}
			return g.rollout[min(ro, len(g.rollout))-1], nil
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
	if want := []string{"policy", "repro r1", "review r1", "criteria r1", "approval r1", "merge", "rollout"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
	if !slices.Equal(g.merges, []string{"9 r1 sebastian"}) || g.approvals[0].Gates.Policy.Class != "high" {
		t.Fatalf("merges %v, approval %+v", g.merges, g.approvals[0])
	}
	// a roll makes no model call (ruling 178): its review's note is no
	// finding; the card names no session gap (roadmap 222, item 2)
	for _, w := range g.approvals[0].WhyHuman {
		if strings.Contains(w, noVerdict) || w == RollGapNote {
			t.Fatalf("why human = %q", g.approvals[0].WhyHuman)
		}
	}
	if !slices.Equal(g.states, []string{"running", "in-review"}) {
		t.Fatalf("states = %v", g.states)
	}
}

// A roll ends done when its rollout is confirmed (ruling 178): the
// approval's merge is not its end — the run waits for the images it
// merged to run on their Deployments (AwaitRollout) and finishes done
// naming what went live, never the wall-clock cap.
func TestWorkflowRollEndsDoneWhenRolloutConfirms(t *testing.T) {
	g, _ := newRollHarness(t, "r1")
	g.rollout = []RolloutResult{{Confirmed: true, Live: []string{"factoryd"}}}
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	reason := g.finals[0][1]
	if !strings.Contains(reason, "rollout live: factoryd") || strings.Contains(reason, "wall-clock") {
		t.Fatalf("reason = %q, want the rollout named, no wall-clock cap", reason)
	}
	if !slices.Contains(g.steps, "rollout") {
		t.Fatalf("steps = %v, want the rollout wait", g.steps)
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
	want := []string{"policy", "repro r1", "review r1", "criteria r1", "approval r1", "policy", "repro r2", "review r2", "criteria r2", "approval r2", "merge", "rollout"}
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

// A roll schedules no review beside its checks wait (ruling 178): its
// review is the stage's own, after green checks.
func TestWorkflowRollReviewsAfterChecks(t *testing.T) {
	g, commits := newRollHarness(t, "r1")
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if len(*commits) != 1 || len(g.reviewGates) != 1 || !g.reviewGates[0].ChecksPassed {
		t.Fatalf("RollCommit calls %v, review gates %+v: want one review after green checks", *commits, g.reviewGates)
	}
	if want := []string{"policy", "repro r1", "review r1", "criteria r1", "approval r1", "merge", "rollout"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
}

// Red checks on a head RollCommit leaves as it is fail the roll: the
// local gates ran first (the pinned order), no agent answers findings on
// a roll, and the next green commit opens the next.
func TestWorkflowRollFailsOnFindingsItCannotAnswer(t *testing.T) {
	g, commits := newRollHarness(t, "r1")
	red := "check \"pr / test (pull_request)\": failure"
	g.onChecks(ChecksResult{State: ChecksFailure, Findings: []string{red}})
	if final := g.run(t); final != "failed" {
		t.Fatalf("final = %q", final)
	}
	if len(*commits) != 2 || len(g.sessions) != 0 || !slices.Equal(g.steps, []string{"policy", "repro r1"}) {
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
	if len(*commits) != 1 || len(g.sessions) != 0 || !slices.Equal(g.steps, []string{"policy", "repro r1", "review r1", "criteria r1", "approval r1"}) {
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
	want := []string{"policy", "repro r1", "review r1", "criteria r1", "approval r1", "policy", "repro r1", "review r1", "criteria r1", "approval r1", "merge", "rollout"}
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

// A roll merges at once after its approval: no session gap, no hold
// (roadmap 222, item 2: sessions survive the restart its merge brings); a
// merge that failed or was refused waits for the retry or the next
// approval only.
func TestWorkflowRollMergesWithoutAGap(t *testing.T) {
	g, _ := newRollHarness(t, "r1")
	g.merge = []MergeResult{{Failed: "forgejo answered 502"}, {Refusal: "the approval is stale"}, {Merged: true}}
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	want := []string{"policy", "repro r1", "review r1", "criteria r1", "approval r1", "merge", "merge", "merge", "rollout"}
	if !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
	if slices.ContainsFunc(g.steps, func(s string) bool { return s == "gap" || s == "clear" || s == "settle" }) {
		t.Fatalf("steps %v; want no gap and no hold", g.steps)
	}
}

// A fold delivered during a roll's session-gap wait stops the merge: the
// gap wait's race takes no roll_folded, so once the gap is over the wait
// drains folds again and returns the folded verdict like its selector
// loop does; the next round commits the folded images and asks for the
// merge again, which the new approval carries through.
func TestWorkflowRollFoldDuringTheGapWaitStopsTheMerge(t *testing.T) {
	g, commits := newRollHarnessWith(t, func(g *gateHarness) { g.gapDelay = 10 * time.Minute }, "r1", "r2")
	g.env.OnGetVersion("roll-gap", workflow.DefaultVersion, versionRollNoGap).Return(versionRollGap)
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(time.Hour+time.Minute, SignalRollFolded, TaskSignal{Author: "factory-agent", PR: 9, Body: "roll c2c2c2c: worker folded in"})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	want := []string{"policy", "repro r1", "review r1", "criteria r1", "approval r1", "gap", "policy", "repro r2", "review r2", "criteria r2", "approval r2", "gap", "merge", "settle", "rollout"}
	if !slices.Equal(g.steps, want) || len(*commits) != 2 {
		t.Fatalf("steps = %v, want %v; RollCommit calls %v", g.steps, want, *commits)
	}
	if !slices.Equal(g.merges, []string{"9 r2 sebastian"}) {
		t.Fatalf("merges = %v", g.merges)
	}
}

// A roll whose gap found no fold merges its approved head.
func TestWorkflowRollMergesAfterItsGapWithoutAFold(t *testing.T) {
	g, commits := newRollHarness(t, "r1")
	g.env.OnGetVersion("roll-gap", workflow.DefaultVersion, versionRollNoGap).Return(versionRollGap)
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	want := []string{"policy", "repro r1", "review r1", "criteria r1", "approval r1", "gap", "merge", "settle", "rollout"}
	if !slices.Equal(g.steps, want) || len(*commits) != 1 {
		t.Fatalf("steps = %v, want %v; RollCommit calls %v", g.steps, want, *commits)
	}
	if !slices.Equal(g.merges, []string{"9 r1 sebastian"}) {
		t.Fatalf("merges = %v", g.merges)
	}
}

// A stray roll_folded on a task that is no roll changes nothing: it
// sleeps in its channel until the run ends and is drained.
func TestWorkflowNoRollIgnoresAStrayRollFolded(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnActivity(ActivityRollCommit, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) (string, error) {
			t.Errorf("a stray roll_folded ran a roll commit: %s", taskID)
			return "", nil
		})
	h.onSessions(pushed("s1", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalRollFolded, TaskSignal{Author: "factory-agent", PR: 9, Body: "roll c2c2c2c: worker folded in"})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	if len(h.sessions) != 1 {
		t.Fatalf("sessions = %+v, want the one", h.sessions)
	}
	if strings.Contains(h.logs.String(), "unhandled signals") {
		t.Fatalf("log:\n%s", h.logs.String())
	}
}

// A card's session waits for no roll's hold (roadmap 222, item 2): the
// session starts at once and its agent time is the session's only.
func TestWorkflowSessionWaitsForNoRollHold(t *testing.T) {
	h := newWorkflowHarness(t)
	var calls []string
	h.env.OnActivity(ActivityAwaitRollHold, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) error {
			t.Errorf("a session waited for a roll's hold")
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
	if !slices.Equal(calls, []string{"session"}) {
		t.Fatalf("calls = %v, want the session alone", calls)
	}
	if len(h.usage) == 0 || h.usage[0].AgentTime != 10*time.Minute {
		t.Fatalf("usage = %+v, want 10m of agent time, the session's only", h.usage)
	}
}

// A planner session waits for no roll's hold either.
func TestPlanSessionWaitsForNoRollHold(t *testing.T) {
	h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"})
	h.env.OnActivity(ActivityAwaitRollHold, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) error {
			t.Errorf("a planner session waited for a roll's hold")
			return nil
		})
	h.signalAfter(3*time.Hour, SignalPlanAccepted, TaskSignal{Author: "sebastian", Revision: 1, SHA256: "sha-1"})
	if final := h.run(t); final != "done" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(h.plans) != 1 || len(h.usage) != 1 || h.usage[0].AgentTime != planSession {
		t.Fatalf("plans %d, usage %+v; want one planner session with %s of agent time", len(h.plans), h.usage, planSession)
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
	if want := []string{"policy", "repro r1", "review r1", "criteria r1", "approval r1", "merge", "rollout"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
}

// A roll's review that answers concerns and names no finding is a remark on
// the card: the roll reaches its approval with the remark among the reasons
// for a human and is not failed.
func TestWorkflowRollConcernsWithoutFindingsIsARemark(t *testing.T) {
	g, _ := newRollHarnessWith(t, func(g *gateHarness) {
		g.review = Review{Verdict: "concerns", Summary: "the pins look odd"}
	}, "r1")
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(g.approvals) != 1 || !slices.Equal(g.approvals[0].WhyHuman, []string{"review: remark, no finding: the pins look odd"}) {
		t.Fatalf("approvals %+v; want the remark the one reason for a human", g.approvals)
	}
}

// A roll from before the remark took the same review as a finding no agent
// answers: RollCommit leaves the head at r1, and the roll fails.
func TestWorkflowRollBeforeReviewRemarkFailsOnConcernsWithoutFindings(t *testing.T) {
	g, commits := newRollHarnessWith(t, func(g *gateHarness) {
		g.review = Review{Verdict: "concerns", Summary: "the pins look odd"}
	}, "r1")
	g.env.OnGetVersion("review-remark", workflow.DefaultVersion, versionReviewRemark).Return(workflow.DefaultVersion)
	g.onChecks(ChecksResult{State: ChecksSuccess})
	if final := g.run(t); final != "failed" || g.finals[0][1] != "roll: review failed: model review: the pins look odd" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(*commits) != 2 || len(g.approvals) != 0 || !slices.Equal(g.steps, []string{"policy", "repro r1", "review r1"}) {
		t.Fatalf("RollCommit calls %v, approvals %+v, steps %v", *commits, g.approvals, g.steps)
	}
}
