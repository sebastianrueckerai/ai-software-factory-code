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

	if final := g.run(t); final != "closed" {
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

// A human's answer to a check wait's escalation runs the check wait again
// on the same commit, not a new session: the agent has nothing to change.
func TestWorkflowGateEscalationAnswerRunsTheGateAgain(t *testing.T) {
	g := newGateHarness(t)
	g.onSessions(pushed("s1", "c1"))
	g.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9}, nil)
	var waits []string
	g.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).After(30 * time.Minute).Return(
		func(ctx context.Context, taskID, sha string) (ChecksResult, error) {
			waits = append(waits, sha)
			return ChecksResult{State: ChecksSuccess}, nil
		}).Once()
	g.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, sha string) (ChecksResult, error) {
			waits = append(waits, sha)
			return ChecksResult{State: ChecksSuccess}, nil
		})
	g.onGateSupervisor(GateDecision{Action: ActionEscalate, Diagnosis: "no runner serves the label", Recommendation: "register a runner"})
	g.tripAfter(11*time.Minute, ActivityWaitChecks, 1)
	g.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "runner registered", Comment: 700})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(g.sessions) != 1 || len(g.escalated) != 1 || !slices.Equal(g.states, []string{"running", "in-review", "in-review"}) {
		t.Fatalf("sessions %d, escalations %q, states %v; want one session, one escalation, in-review again after the answer", len(g.sessions), g.escalated, g.states)
	}
	if last := g.usage[len(g.usage)-1]; last.ExtraAttempts != 0 {
		t.Fatalf("usage = %+v, want no granted attempt: the gate used no agent time", last)
	}
	if !slices.Equal(g.steps, []string{"policy", "repro c1", "review c1", "approval c1", "merge"}) {
		t.Fatalf("steps = %v", g.steps)
	}
}

// A merge's escalation answered runs the merge again with its approval.
func TestWorkflowMergeEscalationKeepsTheApproval(t *testing.T) {
	g := newGateHarness(t, func(g *gateHarness) { g.mergeDelay = 20 * time.Minute })
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.onGateSupervisor(GateDecision{Action: ActionEscalate, Diagnosis: "Forgejo hangs on the merge", Recommendation: "restart Forgejo"})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.tripAfter(time.Hour+5*time.Minute, ActivityMerge, 4)
	g.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "restarted", Comment: 700})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if n := len(g.approvals); n != 1 || len(g.sessions) != 1 || !strings.Contains(g.finals[0][1], "after sebastian's approval") {
		t.Fatalf("approval requests %d, sessions %d, reason %q; want the one approval merged", n, len(g.sessions), g.finals[0][1])
	}
	if want := []string{"9 c1 sebastian", "9 c1 sebastian"}; !slices.Equal(g.merges, want) {
		t.Fatalf("merges = %v, want %v", g.merges, want)
	}
}

// No retry on the last allowed attempt (GateReproduction has two), no
// wait for a session.
func TestWorkflowGateAllowancesFollowTheActivity(t *testing.T) {
	g := newGateHarness(t, func(g *gateHarness) { g.reproDelay = 20 * time.Minute })
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	calls := g.onGateSupervisor(GateDecision{Action: ActionWait, Diagnosis: "running"})
	g.env.RegisterDelayedCallback(func() {
		g.env.SignalWorkflow(SignalGateBound, GateTrip{Activity: ActivityGateReproduction, ActivityID: "11", Attempt: 2, AlertID: 1})
	}, 10*time.Minute)
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || len(*calls) != 1 || !(*calls)[0].CanWait || (*calls)[0].CanRetry {
		t.Fatalf("final %q, calls %+v; want wait but no retry on attempt 2 of 2", final, *calls)
	}

	h := newWorkflowHarness(t)
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(15*time.Minute).Return(pushed("s1", "c1"), nil)
	h.onChecks(ChecksResult{State: ChecksSuccess})
	sessionCalls := h.onGateSupervisor(GateDecision{Action: ActionRetry, Diagnosis: "hung"})
	h.tripAfter(5*time.Minute, ActivityRunSession, 1)
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" || len(*sessionCalls) != 1 || (*sessionCalls)[0].CanWait || !(*sessionCalls)[0].CanRetry {
		t.Fatalf("final %q, calls %+v; want retry but no wait for a session", final, *sessionCalls)
	}

	m := newGateHarness(t, func(g *gateHarness) { g.mergeDelay = 20 * time.Minute })
	m.onSessions(pushed("s1", "c1"))
	m.onChecks(ChecksResult{State: ChecksSuccess})
	mergeCalls := m.onGateSupervisor(GateDecision{Action: ActionRetry, Diagnosis: "hung"})
	m.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	m.env.RegisterDelayedCallback(func() {
		m.env.SignalWorkflow(SignalGateBound, GateTrip{Activity: ActivityMerge, ActivityID: "11", Attempt: 1, AlertID: 1, Bound: 90 * time.Second})
	}, time.Hour+5*time.Minute)
	if final := m.run(t); final != "done" || len(*mergeCalls) != 1 || (*mergeCalls)[0].CanWait || !(*mergeCalls)[0].CanRetry {
		t.Fatalf("final %q, calls %+v; want retry but no wait for a merge (2 x 90 s outlasts its 2 min attempt)", final, *mergeCalls)
	}
}

// A gate that finishes while the supervisor deliberates wins (ruling
// 171): the trip at 5 min starts a 10-min supervisor call, the checks end
// at 10 min, and the gates go on at once; the late cancel changes
// nothing. Before the change the workflow waited for the answer (15 min)
// and then dropped it.
func TestWorkflowGateThatEndsDuringTheSupervisorCallWins(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version workflow.Version
		request time.Duration
	}{{"race", versionGateSupervisorRace, 10 * time.Minute}, {"before", workflow.DefaultVersion, 15 * time.Minute}} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateHarness(t)
			g.env.OnGetVersion("gate-supervisor-race", workflow.DefaultVersion, versionGateSupervisorRace).Return(tc.version)
			g.onSessions(pushed("s1", "c1"))
			g.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9}, nil)
			g.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).After(10*time.Minute).Return(ChecksResult{State: ChecksSuccess}, nil)
			g.env.OnActivity(ActivitySuperviseGate, mock.Anything, "t1", mock.Anything).After(10*time.Minute).
				Return(GateDecision{Action: ActionCancel, Diagnosis: "stuck"}, nil)
			start := g.env.Now()
			g.tripAfter(5*time.Minute, ActivityWaitChecks, 1)
			g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
			if final := g.run(t); final != "done" {
				t.Fatalf("final = %q, finals %v", final, g.finals)
			}
			if len(g.requested) != 1 || g.requested[0].Sub(start) != tc.request {
				t.Fatalf("approval requested %v after the start, want %s", g.requested[0].Sub(start), tc.request)
			}
		})
	}
}
