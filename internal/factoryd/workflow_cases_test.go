package factoryd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
)

// caseResolved is Act's case_resolved of case id by sebastian with res,
// carried by comment.
func caseResolved(id int64, res Resolution, comment int64) TaskSignal {
	return TaskSignal{Author: "sebastian", Comment: comment, Case: &CaseSignal{CaseID: id, Rung: 3, By: "sebastian", Res: res, Comment: comment}}
}

// onWaitChecks answers OpenPullRequest with #9 and WaitChecks green, and
// records the heads WaitChecks ran on.
func (h *workflowHarness) onWaitChecks(heads *[]string) {
	h.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9, URL: "https://git.example/sf/r/pulls/9"}, nil)
	h.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, sha string) (ChecksResult, error) {
			*heads = append(*heads, sha)
			return ChecksResult{State: ChecksSuccess}, nil
		})
}

// A run at its session cap with pull request #9 open on c3: rung 0 takes
// the stop with route_approval, so no one is asked and no session opens.
// The head goes through the gates to its approval, and the merge ends the
// card.
func TestWorkflowTakesRungZerosResolutionWithoutWaiting(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "running", PR: PullRequest{Number: 9}, Head: "c3", Findings: []string{"reviewer sebastian requested changes: keep the flag"},
		Usage: TaskUsage{AgentTime: 12 * time.Minute, Attempts: 3}}
	h.rungZero = func(int, TripInput) *Resolution { return &Resolution{Action: ActRouteApproval} }
	h.onSessions(pushed("s4", "c4"))
	var checked []string
	h.onWaitChecks(&checked)
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	if len(h.trips) != 1 || !strings.HasPrefix(h.trips[0].Reason, capReached) || h.trips[0].PR != 9 {
		t.Fatalf("trips = %+v, want the session cap's on #9", h.trips)
	}
	if len(h.escalated) != 0 || len(h.sessions) != 0 {
		t.Fatalf("asked %q, sessions %d; want no one asked and no session", h.escalated, len(h.sessions))
	}
	if !slices.Equal(checked, []string{"c3"}) {
		t.Fatalf("checks ran on %q, want the open head c3 once", checked)
	}
}

// At the session cap the human extends the card by one session: the run
// reads raised_sessions 1 on the row, and the fourth session runs with the
// cap at four and the note among the answers. The answer buys nothing of
// its own.
func TestWorkflowResumesOnCaseResolvedExtend(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "running", Usage: TaskUsage{AgentTime: 12 * time.Minute, Attempts: 3}}
	h.onSessions(pushed("s4", "c4"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	extend := Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}, Note: "one more, then stop"}
	h.env.RegisterDelayedCallback(func() {
		h.rowUsage.RaisedSessions = 1
		h.env.SignalWorkflow(SignalCaseResolved, caseResolved(701, extend, 605))
	}, time.Hour)
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	if len(h.escalated) != 1 || !strings.HasPrefix(h.escalated[0], capReached) {
		t.Fatalf("asked %q, want the session cap once", h.escalated)
	}
	want := []string{`sebastian, to "` + capReached + `": one more, then stop`}
	if len(h.sessions) != 1 || h.sessions[0].Attempt != 4 || h.sessions[0].Sessions != 4 || !slices.Equal(h.sessions[0].Answers, want) {
		t.Fatalf("sessions = %+v, want the fourth of four with the note %q", h.sessions, want)
	}
	if u := h.usage[0]; u.ExtraAttempts != 0 || u.ExtraTime != 0 {
		t.Fatalf("usage = %+v, want no attempt or time the answer bought", u)
	}
}

// A successor chosen at any wait closes the card into one: at the session
// cap with no work state, at a check-in's stop with the stop's work state.
func TestWorkflowFinishesIntoTheSuccessorFromAnyWait(t *testing.T) {
	successor := Resolution{Action: ActSuccessor}
	t.Run("the session cap", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.state = RunState{Status: "running", PR: PullRequest{Number: 9}, Head: "c3", Findings: []string{"reviewer sebastian requested changes: keep the flag"},
			Usage: TaskUsage{AgentTime: 12 * time.Minute, Attempts: 3}}
		h.onSessions(pushed("s4", "c4"))
		h.signalAfter(time.Hour, SignalCaseResolved, caseResolved(701, successor, 605))

		if final := h.run(t); final != "failed" || h.finals[0][1] != "closed into successor t-succ" {
			t.Fatalf("finals = %v, want failed closed into successor t-succ", h.finals)
		}
		if len(h.successions) != 1 || h.successions[0].PR != 9 || h.successions[0].Findings != "" || len(h.sessions) != 0 {
			t.Fatalf("successions %+v, sessions %d; want one carrying #9 with no work state, no session", h.successions, len(h.sessions))
		}
	})
	t.Run("a check-in's stop", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.taskConfig = askTaskConfig()
		var steps []string
		h.onVerify(&steps, []SessionResult{
			pushed("s1", "c1"),
			checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
				Reason:   CheckinShareSpent,
				Findings: []CheckFinding{{Text: "the migration runs outside its transaction"}},
				Review:   &Review{Verdict: "concerns", Summary: "two faults block the merge"},
				Record:   &CheckInRecord{Moment: CheckInGate, Session: "s2", ParentSession: "s1"}})})
		h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
			func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
				return Review{Verdict: "concerns", Summary: "one fault", Findings: []string{"rename the flag"}}, nil
			})
		h.signalAfter(time.Hour, SignalCaseResolved, caseResolved(701, successor, 605))

		if final := h.run(t); final != "failed" || h.finals[0][1] != "closed into successor t-succ" {
			t.Fatalf("finals = %v, want failed closed into successor t-succ", h.finals)
		}
		if len(h.successions) != 1 || h.successions[0].PR != 9 || len(h.sessions) != 2 {
			t.Fatalf("successions %+v, sessions %d; want one carrying #9 after the two sessions", h.successions, len(h.sessions))
		}
		for _, want := range []string{"the migration runs outside its transaction", "review: concerns"} {
			if !strings.Contains(h.successions[0].Findings, want) {
				t.Errorf("the successor's findings = %q, want %q", h.successions[0].Findings, want)
			}
		}
	})
}

// The console's answer posts comment 7 as the human and resolves the case,
// and the webhook's copy of comment 7 comes too. Either order, the answer
// is one: the typed resolution counts, the wait ends once, and a later
// wait files the copy under nothing new.
func TestWorkflowDedupesTheWebhookCopyOfTheAnswer(t *testing.T) {
	license := Resolution{Action: ActHandover, Args: map[string]any{"model": "stronger"}, Note: "use MIT"}
	answered := `sebastian, to "Which license?": use MIT`
	for _, tc := range []struct {
		name  string
		first func(h *workflowHarness)
	}{
		{"the resolution first", func(h *workflowHarness) {
			h.signalAfter(time.Hour, SignalCaseResolved, caseResolved(701, license, 7))
			h.signalAfter(time.Hour+time.Second, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "use MIT", Comment: 7})
		}},
		{"the webhook's copy first", func(h *workflowHarness) {
			h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "use MIT", Comment: 7})
			h.signalAfter(time.Hour+time.Second, SignalCaseResolved, caseResolved(701, license, 7))
			h.signalAfter(time.Hour+2*time.Second, SignalAnswerWhole, TaskSignal{Author: "sebastian", Comment: 7})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			h.escalationIDs = []int64{5, 9}
			year := SessionResult{Outcome: OutcomeEscalate, SessionID: "s2", Reason: "The agent asks: Which year?\nWhy: none\nRecommendation: 2026"}
			h.onSessions(licenseQuestion, year, pushed("s3", "c3"))
			h.onChecks(ChecksResult{State: ChecksSuccess})
			tc.first(h)
			h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "2026", Comment: 10})
			h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

			if final := h.run(t); final != "done" {
				t.Fatalf("final = %q, finals %v", final, h.finals)
			}
			if len(h.sessions) != 3 {
				t.Fatalf("sessions = %d, want three: one after each answer", len(h.sessions))
			}
			if got := h.sessions[1]; got.Model != "factory/big" || !slices.Equal(got.Answers, []string{answered}) {
				t.Fatalf("the session after the first answer = %+v, want the stronger model and the one answer %q", got, answered)
			}
			if got := h.sessions[2].Answers; len(got) != 2 || got[1] != answered || !strings.HasSuffix(got[0], ": 2026") {
				t.Fatalf("the last session's answers = %q, want the year's and the license's once", got)
			}
		})
	}
}

// A day cap's wait that ends on its own at 00:00 UTC withdraws its case
// with the reason, and the next session runs.
func TestWorkflowWithdrawsTheCaseWhenTheCapTurns(t *testing.T) {
	h, run := capHarness(t, 0, pushed("s1", "c1"))
	h.env.OnActivity(ActivitySessionFunded, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, model string) (string, error) {
			if h.env.Now().Before(oct(15, 0, 0)) {
				return "the day cap cannot fund a call: Answer, and the attempt waits on its own for the UTC day's headroom.\nspent", nil
			}
			return "", nil
		})
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if want := []string{"701 the wait ended on its own: the cap no longer binds a session"}; !slices.Equal(h.ended, want) {
		t.Fatalf("EndCase calls %q, want %q", h.ended, want)
	}
	if len(run.began) != 1 || !run.began[0].Equal(oct(15, 0, 0)) {
		t.Fatalf("sessions began at %v, want one at 00:00", run.began)
	}
}

// A dependency that failed with no replacement is a trip. Rung 0's retry
// puts the card back in the wait on the replacement, whose end starts it,
// and a human's plain answer starts it at once with the reason as a finding.
func TestWorkflowTripsOnAFailedDependency(t *testing.T) {
	failed := Dependency{TaskID: "t-a", Issue: 5, Key: "links", Status: "failed"}
	reason := depFailedReason(failed)
	if want := "A dependency failed: #5 (links), card t-a, ended failed with no replacement. Retry it, close this card, or answer to start it anyway."; reason != want || escalationSource(reason) != SourceDependencyFailed {
		t.Fatalf("reason %q of source %q, want %q of %q", reason, escalationSource(reason), want, SourceDependencyFailed)
	}
	if kind, _ := caseKindOf(escalationSource(reason), reason); kind != CaseDependencyFailed {
		t.Fatalf("kind = %s, want %s", kind, CaseDependencyFailed)
	}
	deps := func(h *workflowHarness, states ...DepState) *[]time.Duration {
		start, asked := h.env.Now(), &[]time.Duration{}
		h.env.OnActivity(ActivityDependencies, mock.Anything, "t1").Return(func(ctx context.Context, taskID string) (DepState, error) {
			*asked = append(*asked, h.env.Now().Sub(start))
			return states[min(len(*asked), len(states))-1], nil
		})
		return asked
	}
	t.Run("rung 0 retries it", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.taskConfig = depsConfig()
		asked := deps(h, DepState{Waiting: []int64{5}, Failed: &failed}, DepState{Waiting: []int64{9}}, DepState{})
		h.rungZero = func(int, TripInput) *Resolution {
			return &Resolution{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}}
		}
		h.onSessions(pushed("s1", "c1"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(5*time.Minute, SignalDepEnded, TaskSignal{})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

		if final := h.run(t); final != "done" {
			t.Fatalf("final = %q, finals %v", final, h.finals)
		}
		if len(h.trips) != 1 || h.trips[0].Reason != reason || len(h.escalated) != 0 {
			t.Fatalf("trips %+v, asked %q; want the failed dependency's trip, no one asked", h.trips, h.escalated)
		}
		if !slices.Equal(*asked, []time.Duration{0, 0, 5 * time.Minute}) || len(h.sessions) != 1 || len(h.sessions[0].Findings) != 0 {
			t.Fatalf("dependencies asked at %v, sessions %+v; want again at once, then at the replacement's end, one session with no finding", *asked, h.sessions)
		}
	})
	t.Run("a human answers", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.taskConfig = depsConfig()
		deps(h, DepState{Waiting: []int64{5}, Failed: &failed})
		h.onSessions(pushed("s1", "c1"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the links are in already", Comment: 602})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

		if final := h.run(t); final != "done" {
			t.Fatalf("final = %q, finals %v", final, h.finals)
		}
		if !slices.Equal(h.escalated, []string{reason}) || len(h.sessions) != 1 || !slices.Equal(h.sessions[0].Findings, []string{reason}) {
			t.Fatalf("asked %q, sessions %+v; want the trip asked once and the session with the reason as its finding", h.escalated, h.sessions)
		}
	})
}

// A session that used the agent time with nothing pushed opens the agent
// time's case instead of failing the card, and an extend of ten minutes on
// the row runs the next session: an answer buys no time of its own.
func TestWorkflowTimeCapWithoutAHeadAsks(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(19 * time.Minute).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			return SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "no changes to push"}, nil
		}).Once()
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(5 * time.Minute).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			return pushed("s2", "c2"), nil
		})
	h.onChecks(ChecksResult{State: ChecksSuccess})
	extend := Resolution{Action: ActExtend, Args: map[string]any{"agent_time_s": 600}}
	h.env.RegisterDelayedCallback(func() {
		h.rowUsage.RaisedTime = 10 * time.Minute
		h.env.SignalWorkflow(SignalCaseResolved, caseResolved(701, extend, 605))
	}, time.Hour)
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	if len(h.escalated) != 1 || !strings.HasPrefix(h.escalated[0], timeCapReached) {
		t.Fatalf("asked %q, want the agent time's card once", h.escalated)
	}
	if len(h.sessions) != 2 || h.sessions[1].Attempt != 2 {
		t.Fatalf("sessions = %+v, want the second after the extend", h.sessions)
	}
	if u := h.usage[len(h.usage)-1]; u.ExtraTime != 0 || u.ExtraAttempts != 0 {
		t.Fatalf("usage = %+v, want no time or attempt the answer bought", u)
	}
}
