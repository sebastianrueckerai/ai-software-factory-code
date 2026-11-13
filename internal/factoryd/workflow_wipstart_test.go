package factoryd

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/mock"
	"slices"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/workflow"
)

// deadAfterPushing is a session that pushed work in progress to commit w2
// and ended on outcome.
func deadAfterPushing(outcome SessionOutcome) SessionResult {
	return SessionResult{Outcome: outcome, SessionID: "s1", Commit: "w2", Summary: "the pod was killed",
		Findings: []string{"previous session ended unfinished: the pod was killed"}}
}

// A session that pushed and then ended without a verdict hands the pushed
// head to the next session as its start, with itself as the parent. A run
// from before wip-start keeps the base (the empty start) and, as before, the
// refused session's parent. No pull request opens on the interim head: at
// the wall-clock cap the branch reads as holding no finished work, and the
// cap card's answer buys the session that goes on from it.
func TestWorkflowNextSessionStartsFromThePushedHead(t *testing.T) {
	for _, tc := range []struct {
		outcome SessionOutcome
		gate    bool
	}{
		{OutcomeNoPush, true}, {OutcomeRedirect, true}, {OutcomeRetryStronger, true},
		{OutcomeProviderRefused, true}, {OutcomeDayCap, true}, {OutcomeWallClock, true},
		{OutcomeNoPush, false}, {OutcomeRedirect, false}, {OutcomeDayCap, false}, {OutcomeWallClock, false},
	} {
		name := string(tc.outcome) + " from before the gate keeps the base"
		if tc.gate {
			name = string(tc.outcome) + " hands its pushed head on"
		}
		t.Run(name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			if !tc.gate {
				h.env.OnGetVersion("wip-start", workflow.DefaultVersion, versionWipStart).Return(workflow.DefaultVersion)
			}
			dead := deadAfterPushing(tc.outcome)
			if tc.outcome == OutcomeProviderRefused {
				dead.Status = 429
			}
			var steps []string
			h.onVerify(&steps, []SessionResult{dead, pushed("s2", "c3")})
			if tc.outcome == OutcomeWallClock {
				h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
			}
			h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

			if final := h.run(t); final != "done" {
				t.Fatalf("final = %q (%v), want done", final, h.finals)
			}
			var prs []string
			for _, s := range steps {
				if head, ok := strings.CutPrefix(s, "pr "); ok {
					prs = append(prs, strings.Fields(head)[0])
				}
			}
			if !slices.Equal(prs, []string{"c3"}) {
				t.Fatalf("pull requests opened on %v, want only the finished head c3 (steps %v)", prs, steps)
			}
			if len(h.sessions) < 2 {
				t.Fatalf("sessions = %d, want 2", len(h.sessions))
			}
			second := h.sessions[1]
			want := ""
			if tc.gate {
				want = "w2"
			}
			if second.StartRef != want {
				t.Fatalf("second start = %q, want %q", second.StartRef, want)
			}
			if tc.gate && second.ParentSessionID != "s1" {
				t.Fatalf("second parent = %q, want s1, the session whose head it starts from", second.ParentSessionID)
			}
		})
	}
}

// A healed run whose card has no pull request starts its first session from
// the branch head Reconcile read, whether the card was running or waiting on
// a human; a run from before wip-start starts from the base however the head
// reads.
func TestWorkflowHealedRunStartsFromTheBranchHead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  RunState
		answer bool
	}{
		{"a running card", RunState{Status: "running", Head: "w2", Usage: TaskUsage{Attempts: 1}}, false},
		{"a card waiting on a human", RunState{Status: "waiting-human", Question: "which alias?", Head: "w2", Usage: TaskUsage{Attempts: 1}}, true},
	} {
		for _, gate := range []bool{true, false} {
			name, start := tc.name+" adopts the head at the gate", "w2"
			if !gate {
				name, start = tc.name+" keeps the base before the gate", ""
			}
			t.Run(name, func(t *testing.T) {
				h := newWorkflowHarness(t)
				if !gate {
					h.env.OnGetVersion("wip-start", workflow.DefaultVersion, versionWipStart).Return(workflow.DefaultVersion)
				}
				h.state = tc.state
				h.onSessions(pushed("s2", "c3"))
				h.onChecks(ChecksResult{State: ChecksSuccess})
				if tc.answer {
					h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
				}
				h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

				if final := h.run(t); final != "done" {
					t.Fatalf("final = %q, want done", final)
				}
				if got := h.sessions[0].StartRef; got != start {
					t.Fatalf("first start = %q, want %q", got, start)
				}
			})
		}
	}
}

// A healed run whose open pull request holds an interim push as its head
// does not take that head through the gates as finished work: a session
// continues from it.
func TestWorkflowHealedRunDoesNotVerifyAnInterimHead(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "running", PR: PullRequest{Number: 9}, Head: "w2", Unfinished: true, Usage: TaskUsage{Attempts: 1}}
	var steps []string
	h.onVerify(&steps, []SessionResult{pushed("s2", "c3")})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if want := []string{"session 2 after 0 cards", "pr c3 after 0 cards", "approval c3"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want %v: no gate on w2", steps, want)
	}
	if h.sessions[0].StartRef != "w2" {
		t.Fatalf("first start = %q, want the interim head w2", h.sessions[0].StartRef)
	}
}

// A run from before the gate counts a healed pull request's head as judged,
// as before, however Reconcile reads it.
func TestWorkflowBeforeWipStartJudgesTheHealedHead(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnGetVersion("wip-start", workflow.DefaultVersion, versionWipStart).Return(workflow.DefaultVersion)
	h.state = RunState{Status: "running", PR: PullRequest{Number: 9}, Head: "w2", Unfinished: true,
		Findings: []string{"reviewer sebastian requested changes: rename it"}, Usage: TaskUsage{Attempts: 1}}
	var steps []string
	h.onVerify(&steps, []SessionResult{deadAfterPushing(OutcomeNoPush), pushed("s3", "c3")})
	h.signalAfter(48*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q (%v), want done", final, h.finals)
	}
	if len(h.sessions) != 2 || h.sessions[0].StartRef != "w2" || !slices.Contains(h.sessions[1].Findings, "the branch holds w2; the gates have judged it") {
		t.Fatalf("sessions %d, the first from %q, the second told %q; want the handover saying the gates judged w2",
			len(h.sessions), h.sessions[0].StartRef, h.sessions[len(h.sessions)-1].Findings)
	}
}

// An interim head is no finished work: where a path would take it through the
// gates, the gate's refusal sends the card on in a working session that
// starts from that head, told so beside what the stopped session was told,
// and the card never fails on it. No check-in judges the refusal: one that
// would drop it, as a check-in finds nothing to finish in complete work,
// would send the same head to the same refusal.
func TestWorkflowContinuesInASessionWhereAGateRefusesAnInterimHead(t *testing.T) {
	const refused = "the branch's head w2 is a session's work in progress, which no gate judges: continue from it and finish the work"
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	h.wipHeads = []string{"w2"}
	sessions := []SessionResult{deadAfterPushing(OutcomeNoPush), checkinStopped("s2", OutcomeCheckinDropped, &CheckInOutcome{Decision: ActReverify}), pushed("s3", "c3")}
	var steps []string
	h.onVerify(&steps, nil)
	n := 0
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			if in.CheckInAsk != nil && slices.ContainsFunc(in.CheckInAsk.Findings, func(f CheckFinding) bool { return f.Text == refused }) {
				return checkinStopped(fmt.Sprintf("s%d", in.Attempt), OutcomeCheckinDropped, &CheckInOutcome{Decision: ActReverify}), nil
			}
			n++
			return sessions[min(n, len(sessions))-1], nil
		})
	h.signalAfter(48*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q (%v), want done: no failure on an interim head", final, h.finals)
	}
	var prs []string
	for _, s := range steps {
		if head, ok := strings.CutPrefix(s, "pr "); ok {
			prs = append(prs, strings.Fields(head)[0])
		}
	}
	if want := []string{"w2", "c3"}; !slices.Equal(prs, want) {
		t.Fatalf("pull requests opened on %v, want %v (steps %v)", prs, want, steps)
	}
	last := h.sessions[len(h.sessions)-1]
	if last.StartRef != "w2" || last.CheckInAsk != nil || !slices.Equal(last.Findings, []string{refused}) {
		t.Fatalf("last session starts from %q, check-in ask %+v, findings %q; want a working session from w2 told %q", last.StartRef, last.CheckInAsk, last.Findings, refused)
	}
}

// The session after a refusal is told of the newest refused head only: a
// head refused again is told once, and a later head's refusal replaces the
// earlier one's, whose commit the branch moved past.
func TestWorkflowTellsTheNewestRefusedInterimHead(t *testing.T) {
	refused := func(head string) string {
		return "the branch's head " + head + " is a session's work in progress, which no gate judges: continue from it and finish the work"
	}
	for _, later := range []string{"w2", "w3"} {
		t.Run("then "+later, func(t *testing.T) {
			cfg := testWorkflowConfig
			cfg.MaxSessions = 5
			h := newWorkflowHarness(t, TaskWorkflow{Config: func() WorkflowConfig { return cfg }})
			h.taskConfig = askTaskConfig()
			h.wipHeads = []string{"w2", "w3"}
			var steps []string
			dropped := checkinStopped("s2", OutcomeCheckinDropped, &CheckInOutcome{Decision: ActReverify})
			again := deadAfterPushing(OutcomeNoPush)
			again.Commit = later
			h.onVerify(&steps, []SessionResult{deadAfterPushing(OutcomeNoPush), dropped, again, dropped, pushed("s5", "c3")})
			h.signalAfter(48*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

			if final := h.run(t); final != "done" {
				t.Fatalf("final = %q (%v), want done", final, h.finals)
			}
			want := []string{"session 1 after 0 cards", "session 2 after 0 cards", "pr w2 after 0 cards", "session 3 after 0 cards",
				"session 4 after 0 cards", "pr " + later + " after 0 cards", "session 5 after 0 cards", "pr c3 after 0 cards", "approval c3"}
			if !slices.Equal(steps, want) {
				t.Fatalf("steps = %v, want %v", steps, want)
			}
			if fifth := h.sessions[4]; fifth.StartRef != later || !slices.Equal(fifth.Findings, []string{refused(later)}) {
				t.Fatalf("the fifth session starts from %s, told %q; want from %s, told of it alone", fifth.StartRef, fifth.Findings, later)
			}
		})
	}
}

// An interim head is no head the gates judged, whether their refusal or the
// heal met it as the pull request's head: a session that dies on it hands
// on that the gates have not judged it.
func TestWorkflowARefusedInterimHeadIsNotJudged(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	h.state = RunState{Status: "waiting-human", Question: "which alias?", PR: PullRequest{Number: 9}, Head: "w2", Unfinished: true,
		Usage: TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}}
	h.wipHeads = []string{"w2"}
	var steps []string
	h.onVerify(&steps, []SessionResult{deadAfterPushing(OutcomeNoPush), pushed("s3", "c3")})
	h.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", Body: "rename it", PR: 9})
	h.signalAfter(48*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q (%v), want done", final, h.finals)
	}
	if len(h.sessions) != 2 || !slices.Contains(h.sessions[1].Findings, "the branch holds w2; the gates have not judged it") {
		t.Fatalf("sessions %d, the second told %q; want the handover saying no gate judged w2", len(h.sessions), h.sessions[len(h.sessions)-1].Findings)
	}
}
