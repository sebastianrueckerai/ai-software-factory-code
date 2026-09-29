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
	g := newGateHarness(t, func(g *gateHarness) {
		g.gate = PolicyGate{Class: "high", Outcome: policy.OutcomeRequiresApproval, PolicyVersion: 7, Changed: 2, Paths: []string{"k8s/factoryd.yaml"}}
		g.review = Review{Note: "no model review: a roll task makes no model call"}
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
	if want := []string{"policy", "repro r1", "review r1", "approval r1", "merge"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
	if !slices.Equal(g.merges, []string{"9 r1 sebastian"}) || g.approvals[0].Gates.Policy.Class != "high" {
		t.Fatalf("merges %v, approval %+v", g.merges, g.approvals[0])
	}
	// a roll makes no model call (ruling 178): its review's note is no finding
	for _, w := range g.approvals[0].WhyHuman {
		if strings.Contains(w, noVerdict) {
			t.Fatalf("why human = %q", g.approvals[0].WhyHuman)
		}
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
	want := []string{"policy", "repro r1", "review r1", "approval r1", "policy", "repro r2", "review r2", "approval r2", "merge"}
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
	want := []string{"policy", "repro r1", "review r1", "approval r1", "policy", "repro r1", "review r1", "approval r1", "merge"}
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
