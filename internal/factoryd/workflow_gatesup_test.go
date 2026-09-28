package factoryd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/workflow"
)

// onGateSupervisor answers SuperviseGate calls in order with decisions (the
// last repeating) and records each input with its time.
func (h *workflowHarness) onGateSupervisor(decisions ...GateDecision) *[]GateSupervision {
	var got []GateSupervision
	h.env.OnActivity(ActivitySuperviseGate, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID string, in GateSupervision) (GateDecision, error) {
			got = append(got, in)
			return decisions[min(len(got), len(decisions))-1], nil
		})
	return &got
}

func (h *workflowHarness) tripAfter(d time.Duration, activity string, alert int64) {
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow(SignalGateBound, GateTrip{Activity: activity, ActivityID: "11", Attempt: 1, AlertID: alert, Bound: 10 * time.Minute})
	}, d)
}

// One check wait: its first trip may wait or retry; after the wait the
// second may only retry; after the retry the third may do neither. The
// checks then finish and the task goes on to its merge.
func TestWorkflowGateTripsOneWaitAndOneRetryPerRun(t *testing.T) {
	g := newGateHarness(t)
	g.onSessions(pushed("s1", "c1"))
	g.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9}, nil)
	g.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).After(30*time.Minute).Return(ChecksResult{State: ChecksSuccess}, nil)
	calls := g.onGateSupervisor(GateDecision{Action: ActionWait, Diagnosis: "running"}, GateDecision{Action: ActionRetry, Diagnosis: "hung poll"},
		GateDecision{Action: ActionWait, Diagnosis: "running"})
	g.tripAfter(11*time.Minute, ActivityWaitChecks, 1)
	g.tripAfter(21*time.Minute, ActivityWaitChecks, 2)
	g.tripAfter(25*time.Minute, ActivityWaitChecks, 3)
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	var got []string
	for _, in := range *calls {
		got = append(got, strings.Join([]string{in.Trip.Activity, jsonOf(in.Input), jsonOf([]bool{in.CanWait, in.CanRetry})}, " "))
	}
	want := []string{`WaitChecks ["t1","c1"] [true,true]`, `WaitChecks ["t1","c1"] [false,true]`, `WaitChecks ["t1","c1"] [false,false]`}
	if !slices.Equal(got, want) {
		t.Fatalf("supervisor calls = %q, want %q", got, want)
	}
}

// cancel fails the task with the diagnosis at once; the check wait is
// cancelled, not waited for.
func TestWorkflowGateCancelFailsTheTask(t *testing.T) {
	g := newGateHarness(t)
	g.onSessions(pushed("s1", "c1"))
	g.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9}, nil)
	g.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).After(30*time.Minute).Return(ChecksResult{State: ChecksSuccess}, nil)
	g.onGateSupervisor(GateDecision{Action: ActionCancel, Diagnosis: "the workflow file names a runner label no runner serves"})
	start := g.env.Now()
	g.tripAfter(11*time.Minute, ActivityWaitChecks, 1)

	if final := g.run(t); final != "failed" || g.finishAt.Sub(start) >= 30*time.Minute {
		t.Fatalf("final = %q after %s", final, g.finishAt.Sub(start))
	}
	if want := "the gate supervisor cancelled WaitChecks: the workflow file names a runner label no runner serves"; g.finals[0][1] != want {
		t.Fatalf("reason = %q, want %q", g.finals[0][1], want)
	}
}

// escalate stops the session and hands the task to a human with the
// diagnosis and the recommendation; the wait is no agent time, and the
// answer buys an attempt that carries it.
func TestWorkflowGateEscalateWaitsForAHuman(t *testing.T) {
	h := newWorkflowHarness(t)
	record := func(res SessionResult) func(context.Context, SessionInput) (SessionResult, error) {
		return func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			return res, nil
		}
	}
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(15 * time.Minute).Return(record(pushed("s1", "c1"))).Once()
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(record(pushed("s2", "c2")))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.onGateSupervisor(GateDecision{Action: ActionEscalate, Diagnosis: "the worker pod waits for an image it cannot pull",
		Recommendation: "check the registry pull secret"})
	h.tripAfter(5*time.Minute, ActivityRunSession, 1)
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "fixed the secret", Comment: 700})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	want := "The gate supervisor stopped RunSession (attempt 1) past its bound of 10m0s: the worker pod waits for an image it cannot pull\n" +
		"Recommendation: check the registry pull secret"
	if len(h.escalated) != 1 || h.escalated[0] != want {
		t.Fatalf("escalations = %q, want %q", h.escalated, want)
	}
	if len(h.sessions) != 2 || !slices.Equal(h.sessions[1].Answers, []string{`sebastian, to "The gate supervisor stopped RunSession (attempt 1) past its bound of 10m0s: the worker pod waits for an image it cannot pull": fixed the secret`}) {
		t.Fatalf("sessions = %+v", h.sessions)
	}
	if first := h.usage[0]; first.AgentTime != 5*time.Minute || first.Attempts != 1 {
		t.Fatalf("usage after the stopped session = %+v, want 5 min of agent time, the human's hour not counted", first)
	}
	if last := h.usage[len(h.usage)-1]; last.ExtraAttempts != 1 {
		t.Fatalf("usage = %+v, want the answer's granted attempt", last)
	}
}

// A merge past its bound is supervised like the other gates; a failed
// supervisor call escalates.
func TestWorkflowGateSupervisorFailureEscalatesTheMerge(t *testing.T) {
	g := newGateHarness(t, func(g *gateHarness) { g.mergeDelay = 20 * time.Minute })
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.env.OnActivity(ActivitySuperviseGate, mock.Anything, "t1", mock.Anything).Return(GateDecision{}, errors.New("store down"))
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.tripAfter(time.Hour+5*time.Minute, ActivityMerge, 4)
	g.signalAfter(3*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := g.run(t); final != "failed" {
		t.Fatalf("final = %q", final)
	}
	if len(g.escalated) != 1 || !strings.Contains(g.escalated[0], "stopped Merge") || !strings.Contains(g.escalated[0], "the gate supervisor failed: store down") {
		t.Fatalf("escalations = %q", g.escalated)
	}
}

// A trip of another activity, or one left from an activity that ended, is
// not the running one's: no supervisor call.
func TestWorkflowIgnoresTripsOfOtherActivities(t *testing.T) {
	g := newGateHarness(t)
	g.onSessions(pushed("s1", "c1"))
	g.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9}, nil)
	g.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).After(30*time.Minute).Return(ChecksResult{State: ChecksSuccess}, nil)
	calls := g.onGateSupervisor(GateDecision{Action: ActionCancel, Diagnosis: "x"})
	g.tripAfter(10*time.Minute, ActivityRunSession, 1)
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" || len(*calls) != 0 {
		t.Fatalf("final = %q after %d supervisor calls, want done after none", final, len(*calls))
	}
	if strings.Contains(g.logs.String(), "unhandled signals") {
		t.Fatalf("log:\n%s", g.logs.String())
	}
}

// A workflow started before the gate supervisor leaves its trips alone.
func TestWorkflowBeforeGateSupervisorIgnoresTrips(t *testing.T) {
	g := newGateHarness(t)
	g.env.OnGetVersion("gate-supervisor", workflow.DefaultVersion, versionGateSupervisor).Return(workflow.DefaultVersion)
	g.onSessions(pushed("s1", "c1"))
	g.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9}, nil)
	g.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).After(30*time.Minute).Return(ChecksResult{State: ChecksSuccess}, nil)
	calls := g.onGateSupervisor(GateDecision{Action: ActionCancel, Diagnosis: "x"})
	g.tripAfter(11*time.Minute, ActivityWaitChecks, 1)
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" || len(*calls) != 0 {
		t.Fatalf("final = %q after %d supervisor calls, want done after none", final, len(*calls))
	}
}
