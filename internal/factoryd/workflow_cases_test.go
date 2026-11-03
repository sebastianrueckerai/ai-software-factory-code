package factoryd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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
	if len(h.trips) != 1 || !strings.HasPrefix(h.trips[0].Reason, capReached) || h.trips[0].Head != "c3" {
		t.Fatalf("trips = %+v, want the session cap's on c3", h.trips)
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

// A check-in stop after the session pushed c2, answered with reverify by
// the human or with route_approval by rung 0: the head through the gates
// is c2, the session's push, not c1, the head the session started from.
func TestWorkflowCheckinStopReverifiesThePushedHead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *workflowHarness)
		cards int
	}{
		{"reverify", func(h *workflowHarness) {
			h.signalAfter(time.Hour, SignalCaseResolved, caseResolved(701, Resolution{Action: ActReverify}, 605))
		}, 1},
		{"route_approval", func(h *workflowHarness) {
			h.rungZero = func(int, TripInput) *Resolution { return &Resolution{Action: ActRouteApproval} }
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			h.taskConfig = askTaskConfig()
			var steps []string
			h.onVerify(&steps, []SessionResult{
				pushed("s1", "c1"),
				{SessionID: "s2", Outcome: OutcomeCheckinHuman, Commit: "c2", Reason: "the objective asks for two stores",
					CheckIn: &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckGoalConflict, Reason: "the objective asks for two stores",
						Record: &CheckInRecord{Moment: CheckInSession, Session: "s2"}}}})
			h.onReviews("c1")
			tc.setup(h)
			h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

			if final := h.run(t); final != "done" {
				t.Fatalf("final = %q, finals %v", final, h.finals)
			}
			want := []string{"session 1 after 0 cards", "pr c1 after 0 cards", "session 2 after 0 cards", fmt.Sprintf("pr c2 after %d cards", tc.cards), "approval c2"}
			if !slices.Equal(steps, want) {
				t.Fatalf("steps = %q, want %q: the answer verifies the session's push", steps, want)
			}
		})
	}
}

// Under the cases gate a plain comment at the check-in share's stop buys
// no share: the checkin_share case is answered by extend or fund through
// Act. The answer still verifies the session's push.
func TestWorkflowPlainAnswerAtTheShareStopBuysNoShare(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	var steps []string
	h.onVerify(&steps, []SessionResult{
		pushed("s1", "c1"),
		{SessionID: "s2", Outcome: OutcomeCheckinHuman, Commit: "c2", Trip: string(TripNoProgress), Reason: CheckinShareSpent,
			CheckIn: &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend, Reason: CheckinShareSpent,
				Record: &CheckInRecord{Moment: CheckInSession, Session: "s2"}}}})
	h.onReviews("c1")
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	want := []string{"session 1 after 0 cards", "pr c1 after 0 cards", "session 2 after 0 cards", "pr c2 after 1 cards", "approval c2"}
	if !slices.Equal(steps, want) {
		t.Fatalf("steps = %q, want %q", steps, want)
	}
	for _, u := range h.usage {
		if u.ExtraCheckinShare != 0 {
			t.Fatalf("usage = %+v, want no share the comment bought", h.usage)
		}
	}
}

// A gate's wait takes the case's resolution: rerun_checks runs the head's
// checks again and the gates run once on the head the rerun made, the
// review and the audit beside its checks, the audit given the stages'
// second pass as its round; handover to the stronger model
// makes the next session's model that one; successor closes the card into
// one.
func TestWorkflowGateWaitTakesRerunAndHandover(t *testing.T) {
	gate := func(t *testing.T, res Resolution, reviews ...Review) (*gateHarness, *[]string, *[]string) {
		g := newGateHarnessWith(t, stagesWorkflow(3, withAudit), func(g *gateHarness) { g.reviews = reviews })
		g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
		g.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9}, nil)
		waits, reran := &[]string{}, &[]string{}
		g.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).After(30 * time.Minute).Return(
			func(ctx context.Context, taskID, sha string) (ChecksResult, error) {
				*waits = append(*waits, sha)
				return ChecksResult{State: ChecksSuccess}, nil
			}).Once()
		g.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).Return(
			func(ctx context.Context, taskID, sha string) (ChecksResult, error) {
				*waits = append(*waits, sha)
				return ChecksResult{State: ChecksSuccess}, nil
			})
		g.env.OnActivity(ActivityRerunChecks, mock.Anything, "t1", mock.Anything).Return(
			func(ctx context.Context, taskID, sha string) (string, error) {
				*reran = append(*reran, sha)
				return sha + "r", nil
			})
		g.onGateSupervisor(GateDecision{Action: ActionEscalate, Diagnosis: "no runner serves the label", Recommendation: "register a runner"})
		g.tripAfter(11*time.Minute, ActivityWaitChecks, 1)
		g.signalAfter(time.Hour, SignalCaseResolved, caseResolved(701, res, 605))
		g.signalAfter(5*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		return g, waits, reran
	}
	t.Run("rerun_checks", func(t *testing.T) {
		g, waits, reran := gate(t, Resolution{Action: ActRerunChecks})
		if final := g.run(t); final != "done" {
			t.Fatalf("final = %q, finals %v", final, g.finals)
		}
		if !slices.Equal(*reran, []string{"c1"}) || !slices.Equal(*waits, []string{"c1", "c1r"}) || !slices.Contains(g.steps, "approval c1r") {
			t.Fatalf("reran %q, checks waited on %q, steps %q; want c1 run again and c1r through the gates to its approval", *reran, *waits, g.steps)
		}
		for _, gate := range []string{"review c1r", "audit c1r"} {
			if n := len(slices.DeleteFunc(slices.Clone(g.steps), func(s string) bool { return s != gate })); n != 1 {
				t.Errorf("steps %q: %s %d times, want once", g.steps, gate, n)
			}
		}
		if !slices.Equal(g.auditRounds, []int{1, 2}) {
			t.Errorf("audit rounds %v, want c1's in the stages' first pass and c1r's in the second", g.auditRounds)
		}
	})
	t.Run("successor", func(t *testing.T) {
		g, _, _ := gate(t, Resolution{Action: ActSuccessor})
		if final := g.run(t); final != "failed" || g.finals[0][1] != "closed into successor t-succ" {
			t.Fatalf("finals = %v, want failed closed into successor t-succ", g.finals)
		}
		if len(g.successions) != 1 || g.successions[0].PR != 9 || len(g.sessions) != 1 {
			t.Fatalf("successions %+v, sessions %d; want one carrying #9 after the one session", g.successions, len(g.sessions))
		}
	})
	t.Run("handover", func(t *testing.T) {
		concerns := Review{Verdict: "concerns", Summary: "one fault", Findings: []string{"rename the flag"}}
		g, _, _ := gate(t, Resolution{Action: ActHandover, Args: map[string]any{"model": "stronger"}}, concerns, Review{Verdict: "approve", Summary: "fine"})
		if final := g.run(t); final != "done" {
			t.Fatalf("final = %q, finals %v", final, g.finals)
		}
		if len(g.sessions) != 2 || g.sessions[1].Model != "factory/big" {
			t.Fatalf("sessions = %+v, want the second on factory/big", g.sessions)
		}
	})
}

// At the session cap, an extend with no note, carried by comment 7, and
// the webhook's copy of comment 7 in the same pass: one answer, the
// extend's own words.
func TestWorkflowFilesAResolutionWithNoNoteUnderItsComment(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "running", Usage: TaskUsage{AgentTime: 12 * time.Minute, Attempts: 3}}
	h.onSessions(pushed("s4", "c4"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	extend := Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}}
	h.env.RegisterDelayedCallback(func() {
		h.rowUsage.RaisedSessions = 1
		h.env.SignalWorkflow(SignalCaseResolved, caseResolved(701, extend, 7))
		h.env.SignalWorkflow(SignalHumanComment, TaskSignal{Author: "sebastian", Body: "factoryd: sebastian answered with extend: 1 session from the pool.", Comment: 7})
	}, time.Hour)
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	want := []string{`sebastian, to "` + capReached + `": extend: 1 session from the pool`}
	if len(h.sessions) != 1 || !slices.Equal(h.sessions[0].Answers, want) {
		t.Fatalf("sessions = %+v, want the fourth with the one answer %q", h.sessions, want)
	}
}

// Rung 0's rerun_checks at the session cap runs c3's checks again at the
// loop's top, with no session, and the head the rerun made goes through
// the gates.
func TestWorkflowRerunsTheChecksAtTheLoopsTop(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "running", PR: PullRequest{Number: 9}, Head: "c3", Findings: []string{"reviewer sebastian requested changes: keep the flag"},
		Usage: TaskUsage{AgentTime: 12 * time.Minute, Attempts: 3}}
	h.rungZero = func(int, TripInput) *Resolution { return &Resolution{Action: ActRerunChecks} }
	h.onSessions(pushed("s4", "c4"))
	var checked, reran []string
	h.onWaitChecks(&checked)
	h.env.OnActivity(ActivityRerunChecks, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, sha string) (string, error) {
			reran = append(reran, sha)
			return sha + "r", nil
		})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	if !slices.Equal(reran, []string{"c3"}) || !slices.Equal(checked, []string{"c3r"}) || len(h.sessions) != 0 || len(h.escalated) != 0 {
		t.Fatalf("reran %q, checked %q, sessions %d, asked %q; want c3 run again, c3r checked, no session, no one asked", reran, checked, len(h.sessions), h.escalated)
	}
}

// Rung 0's resolution of the question the review beside the checks asked
// is the review stage's answer, and no one is asked: reverify reviews the
// head again after the green checks; rerun_checks runs c1's checks again,
// and the review and the audit run once on c1r, beside its checks;
// successor closes the card into one.
func TestStagesRungZeroAnswersTheReviewBesideTheChecks(t *testing.T) {
	beside := func(t *testing.T, res Resolution) *gateHarness {
		g := besideTheChecks(t, 3*time.Hour)
		g.rungZero = func(int, TripInput) *Resolution { return &res }
		g.tripAfter(15*time.Minute, ActivityModelReview, 2)
		g.onGateSupervisor(GateDecision{Action: ActionEscalate, Diagnosis: "the review model hangs", Recommendation: "look at the gateway"})
		g.signalAfter(10*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		return g
	}
	t.Run("reverify", func(t *testing.T) {
		g := beside(t, Resolution{Action: ActReverify})
		if final := g.run(t); final != "done" {
			t.Fatalf("final = %q, finals %v", final, g.finals)
		}
		if len(g.trips) != 1 || len(g.escalated) != 0 {
			t.Fatalf("trips %d, asked %q; want the one trip and no one asked", len(g.trips), g.escalated)
		}
		if last := g.reviewGates[len(g.reviewGates)-1]; !last.ChecksPassed || last.Beside {
			t.Fatalf("review gates %+v: want the last review after the green checks, not beside them", g.reviewGates)
		}
		if len(g.requested) != 1 || g.requested[0].Sub(g.started) != 3*time.Hour+20*time.Minute {
			t.Fatalf("approval requested %v, want once, at 3h20m: the checks' 20 min and the sequential review's 3h", g.requested)
		}
	})
	t.Run("rerun_checks", func(t *testing.T) {
		g := newGateHarnessWith(t, stagesWorkflow(3, withAudit), func(g *gateHarness) { g.reviewDelay = 3 * time.Hour })
		g.onSessions(pushed("s1", "c1"))
		g.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9}, nil)
		g.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).After(20*time.Minute).Return(ChecksResult{State: ChecksSuccess}, nil)
		var reran []string
		g.env.OnActivity(ActivityRerunChecks, mock.Anything, "t1", mock.Anything).Return(
			func(ctx context.Context, taskID, sha string) (string, error) {
				reran = append(reran, sha)
				return sha + "r", nil
			})
		g.rungZero = func(int, TripInput) *Resolution { return &Resolution{Action: ActRerunChecks} }
		g.tripAfter(15*time.Minute, ActivityModelReview, 2)
		g.onGateSupervisor(GateDecision{Action: ActionEscalate, Diagnosis: "the review model hangs", Recommendation: "look at the gateway"})
		g.signalAfter(10*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

		if final := g.run(t); final != "done" {
			t.Fatalf("final = %q, finals %v", final, g.finals)
		}
		if !slices.Equal(reran, []string{"c1"}) || len(g.escalated) != 0 {
			t.Fatalf("reran %q, asked %q; want c1 run again and no one asked", reran, g.escalated)
		}
		for _, gate := range []string{"review c1r", "audit c1r"} {
			if n := len(slices.DeleteFunc(slices.Clone(g.steps), func(s string) bool { return s != gate })); n != 1 {
				t.Errorf("steps %q: %s %d times, want once", g.steps, gate, n)
			}
		}
	})
	t.Run("successor", func(t *testing.T) {
		g := beside(t, Resolution{Action: ActSuccessor})
		if final := g.run(t); final != "failed" || g.finals[0][1] != "closed into successor t-succ" {
			t.Fatalf("finals = %v, want failed closed into successor t-succ", g.finals)
		}
		if len(g.successions) != 1 || len(g.requested) != 0 {
			t.Fatalf("successions %+v, approvals %v; want one successor and no approval", g.successions, g.requested)
		}
	})
}

// A healed run's waits take the case's resolution: the human's wait
// reverifies the pull request's head with no session, the cap's wait
// hands the next session to the stronger model, and the screening's
// closes the card into a successor.
func TestWorkflowHealedWaitsTakeTheResolution(t *testing.T) {
	t.Run("the human's wait", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.state = RunState{Status: "waiting-human", PR: PullRequest{Number: 9}, Head: "c3", Question: capReached + "\nlast: the review asked for changes"}
		h.onSessions(pushed("s1", "c4"))
		var checked []string
		h.onWaitChecks(&checked)
		h.signalAfter(time.Hour, SignalCaseResolved, caseResolved(701, Resolution{Action: ActReverify}, 605))
		h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

		if final := h.run(t); final != "done" {
			t.Fatalf("final = %q, finals %v", final, h.finals)
		}
		if !slices.Equal(h.escalated, []string{healedReason}) || !slices.Equal(checked, []string{"c3"}) || len(h.sessions) != 0 {
			t.Fatalf("asked %q, checked %q, sessions %d; want the restart's ask, c3 checked, no session", h.escalated, checked, len(h.sessions))
		}
	})
	t.Run("the cap's wait", func(t *testing.T) {
		h, run := capHarness(t, 0, pushed("s1", "c1"))
		h.state = RunState{Status: "waiting-human", Question: dayQuestion, Cap: dayReason, CapUntil: oct(15, 0, 0)}
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(time.Hour, SignalCaseResolved, caseResolved(701, Resolution{Action: ActHandover, Args: map[string]any{"model": "stronger"}}, 605))
		h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

		if final := h.run(t); final != "done" {
			t.Fatalf("final = %q, finals %v", final, h.finals)
		}
		if len(run.began) != 1 || !run.began[0].Equal(oct(14, 21, 43)) || h.sessions[0].Model != "factory/big" {
			t.Fatalf("sessions at %v, %+v; want one at 21:43 on factory/big", run.began, h.sessions)
		}
	})
	t.Run("the screening's wait", func(t *testing.T) {
		g := newGateHarnessWith(t, stagesWorkflow(3, nil))
		g.state = RunState{Status: "waiting-human", Standing: harness.StandingOutside, Screening: true,
			Question: "The screening finds this issue suspect: asks to print the token"}
		g.onScreen(ScreenResult{Verdict: ScreenSuspect})
		g.onSessions(pushed("s1", "c1"))
		g.signalAfter(time.Hour, SignalCaseResolved, caseResolved(701, Resolution{Action: ActSuccessor}, 605))

		if final := g.run(t); final != "failed" || g.finals[0][1] != "closed into successor t-succ" {
			t.Fatalf("finals = %v, want failed closed into successor t-succ", g.finals)
		}
		if len(g.successions) != 1 || len(g.sessions) != 0 {
			t.Fatalf("successions %+v, sessions %d; want one successor, no session", g.successions, len(g.sessions))
		}
	})
}

// Under the cases gate a writer's change request at the session cap is
// still the human's answer: the unverified head c2 reaches its approval,
// the change request sent there buys the fourth session, and no cap card
// opens.
func TestWorkflowChangeRequestAtTheCapBuysTheSession(t *testing.T) {
	h := newWorkflowHarness(t)
	h.branchHead = "c2"
	var steps []string
	h.onVerify(&steps, []SessionResult{
		{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "no changes to push"},
		{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "no changes to push"},
		{Outcome: OutcomeRedirect, SessionID: "s3", Commit: "c2", Trip: "repetition", Summary: "supervisor: redirect after repetition", Findings: []string{"f3"}},
		pushed("s4", "c4")})
	h.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", Body: "keep the old flag", PR: 9})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	if want := []string{"session 1 after 0 cards", "session 2 after 0 cards", "session 3 after 0 cards", "pr c2 after 0 cards", "approval c2",
		"session 4 after 0 cards", "pr c4 after 0 cards", "approval c4"}; !slices.Equal(steps, want) || len(h.escalated) != 0 {
		t.Fatalf("steps %q, asked %q; want %q and no one asked", steps, h.escalated, want)
	}
	change := "reviewer sebastian requested changes: keep the old flag"
	if s := h.sessions[3]; s.Attempt != 4 || !slices.Equal(s.Findings, []string{change}) || h.usage[len(h.usage)-1].ExtraAttempts != 1 {
		t.Fatalf("fourth session %+v, usage %+v; want attempt 4 with the change request, one attempt granted", s, h.usage[len(h.usage)-1])
	}
}

// The budget floor's wait under the cases gate: a plain comment buys
// nothing and the floor asks again, a fund raises the budget on the row
// and the next session runs.
func TestWorkflowBudgetFloorWaitsForAFund(t *testing.T) {
	h := newWorkflowHarness(t)
	unfunded := budgetRefused + ": 0.080 EUR of the 2.000 EUR budget remains, and the cheapest call needs more. Raise the budget (Fund on the board) and answer, or close the card."
	funded := false
	floor := func(ctx context.Context, taskID, model string) (string, error) {
		if funded {
			return "", nil
		}
		return unfunded, nil
	}
	h.env.OnActivity(ActivitySessionFunded, mock.Anything, "t1", mock.Anything).Return(floor)
	h.env.OnActivity(ActivityAttemptFunded, mock.Anything, "t1", mock.Anything).Return(floor)
	h.onSessions(pushed("s1", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 650})
	h.env.RegisterDelayedCallback(func() {
		funded = true
		h.env.SignalWorkflow(SignalCaseResolved, caseResolved(702, Resolution{Action: ActFund, Args: map[string]any{"eur": 1}}, 660))
	}, 2*time.Hour)
	h.signalAfter(4*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	if len(h.escalated) != 2 || !strings.HasPrefix(h.escalated[0], budgetRefused) || !strings.HasPrefix(h.escalated[1], budgetRefused) {
		t.Fatalf("asked %q, want the floor asked again after the plain comment", h.escalated)
	}
	if len(h.sessions) != 1 || h.sessions[0].Attempt != 1 {
		t.Fatalf("sessions = %+v, want the first after the fund", h.sessions)
	}
}

// Under the cases gate the answer to a session that took no number still
// buys its slice of agent time: 19 of the 20 minutes went on a session
// with no model answer, so the answer grants 4 minutes and the session
// after it runs on the same number.
func TestWorkflowAnswerToAnUncountedSessionKeepsItsSlice(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(19 * time.Minute).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			return SessionResult{Outcome: OutcomeEscalate, SessionID: "s1", Reason: "stuck"}, nil
		}).Once()
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(5 * time.Minute).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			return pushed("s2", "c2"), nil
		})
	h.env.OnActivity(ActivitySessionAnswered, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionAnswerInput) (bool, error) { return in.SessionID != "s1", nil })
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "try the other lock", Comment: 650})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	want := TaskUsage{AgentTime: 24 * time.Minute, Attempts: 1, ExtraTime: 4 * time.Minute}
	if len(h.sessions) != 2 || h.sessions[1].Attempt != 1 || h.usage[len(h.usage)-1] != want {
		t.Fatalf("sessions %+v, usage %+v; want the second on number 1 and %+v", h.sessions, h.usage[len(h.usage)-1], want)
	}
}

// The agent-time cap with a pushed head under the cases gate: c1's red
// checks go to the time cap's card, an extend of ten minutes on the row
// runs the next session with them, and the answer buys no time of its
// own.
func TestWorkflowTimeCapWithAHeadAsks(t *testing.T) {
	h := newWorkflowHarness(t)
	h.branchHead = "c1"
	h.state = RunState{Status: "running", Usage: TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1}}
	var steps []string
	h.onVerify(&steps, []SessionResult{pushed("s2", "c2")}, gosecRed, ChecksResult{State: ChecksSuccess})
	extend := Resolution{Action: ActExtend, Args: map[string]any{"agent_time_s": 600}}
	h.env.RegisterDelayedCallback(func() {
		h.rowUsage.RaisedTime = 10 * time.Minute
		h.env.SignalWorkflow(SignalCaseResolved, caseResolved(701, extend, 605))
	}, time.Hour)
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	if want := []string{"pr c1 after 0 cards", "session 2 after 1 cards", "pr c2 after 1 cards", "approval c2"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %q, want %q", steps, want)
	}
	if want := []string{timeCapReached + "\nlast: checks findings went back to the agent\n- checks, round 1: gosec G706"}; !slices.Equal(h.escalated, want) {
		t.Fatalf("asked %q, want %q", h.escalated, want)
	}
	if !slices.Equal(h.sessions[0].Findings, []string{"gosec G706"}) || h.usage[len(h.usage)-1].ExtraTime != 0 {
		t.Fatalf("session %+v, usage %+v; want the checks finding and no time the answer bought", h.sessions[0], h.usage[len(h.usage)-1])
	}
}

// A case_resolved of a case the run does not wait on, as one rung 0
// resolved in place, is dropped: the wait goes on to its own case's.
func TestWorkflowDropsAnotherCasesResolution(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(licenseQuestion, pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalCaseResolved, caseResolved(799, Resolution{Action: ActSuccessor}, 0))
	h.signalAfter(2*time.Hour, SignalCaseResolved, caseResolved(701, Resolution{Action: ActContinue, Note: "use MIT"}, 605))
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	want := []string{`sebastian, to "Which license?": use MIT`}
	if len(h.successions) != 0 || len(h.sessions) != 2 || !slices.Equal(h.sessions[1].Answers, want) {
		t.Fatalf("successions %+v, sessions %+v; want none and the second with %q", h.successions, h.sessions, want)
	}
}

// A handover to the cheaper model takes a card that started on the
// stronger rung down to the start rung for its next session.
func TestWorkflowHandoverToTheCheaperModel(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = &TaskConfig{Process: "maintenance", Track: policy.TrackDirect, Class: policy.ClassCoder, Session: policy.SessionCode,
		Stages: testStages(), Queue: policy.QueueNone, Stronger: true}
	h.onSessions(licenseQuestion, pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalCaseResolved, caseResolved(701, Resolution{Action: ActHandover, Args: map[string]any{"model": "cheaper"}}, 605))
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	if len(h.sessions) != 2 || h.sessions[0].Model != "factory/big" || h.sessions[1].Model != "factory/small" {
		t.Fatalf("sessions = %+v, want factory/big then factory/small", h.sessions)
	}
}

// A check-in's spend stop tells its trip which stop it is, whatever its
// ask says: the share stop and its repeat on the same head, whose ask
// names the exits instead, both trip as the share's.
func TestCheckinSpendStopTellsItsTripTheStop(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	var steps []string
	h.onVerify(&steps, []SessionResult{
		pushed("s1", "c1"),
		checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
			Reason: CheckinShareSpent, Record: &CheckInRecord{Moment: CheckInGate, Session: "s2", ParentSession: "s1"}}),
		checkinStopped("s3", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
			Reason: CheckinShareSpent, Record: &CheckInRecord{Moment: CheckInGate, Session: "s3", ParentSession: "s2"}})})
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			return Review{Verdict: "concerns", Summary: "one fault", Findings: []string{"rename the flag"}}, nil
		})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" {
		t.Fatalf("final = %q, escalations = %q", final, h.escalated)
	}
	if len(h.trips) != 2 || strings.Contains(h.trips[1].Reason, CheckinShareSpent) {
		t.Fatalf("trips = %+v, want the share stop and a repeat whose ask names no share", h.trips)
	}
	for i, trip := range h.trips {
		if trip.CheckinStop != CheckinStopShare {
			t.Errorf("trip %d's stop = %q, want %q", i, trip.CheckinStop, CheckinStopShare)
		}
	}
}
