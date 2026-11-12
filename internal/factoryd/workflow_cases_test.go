package factoryd

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

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
	if len(h.trips) != 1 || !strings.HasPrefix(h.trips[0].Reason, capReachedLadder) || h.trips[0].Head != "c3" {
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
	if len(h.escalated) != 1 || !strings.HasPrefix(h.escalated[0], capReachedLadder) {
		t.Fatalf("asked %q, want the session cap once", h.escalated)
	}
	want := []string{`sebastian, to "` + capReachedLadder + `": one more, then stop`}
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
				Reason:   spentStop,
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
	if len(h.escalated) != 1 || !strings.HasPrefix(h.escalated[0], timeCapReachedLadder) {
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

// A plain comment at the check-in's spend stop buys nothing: the
// checkin_share case is answered by extend or fund through Act. The answer
// still verifies the session's push.
func TestWorkflowPlainAnswerAtTheShareStopBuysNoShare(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	var steps []string
	h.onVerify(&steps, []SessionResult{
		pushed("s1", "c1"),
		{SessionID: "s2", Outcome: OutcomeCheckinHuman, Commit: "c2", Trip: string(TripNoProgress), Reason: spentStop,
			CheckIn: &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend, Reason: spentStop,
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
	want := []string{`sebastian, to "` + capReachedLadder + `": extend: 1 session from the pool`}
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

// The typed answer of a case the run does not wait on, as the question
// the review asked beside the checks answered while the run waits on the
// session cap, is kept for the wait that comes to the case: the extend
// buys the fourth session, its pass trips the review's stop, whose case
// the repeat guard holds, and the wait takes the answer at once — no one
// is asked, the review runs again sequentially after the green checks,
// and the answer buys no session of its own.
func TestWorkflowKeepsAnotherCasesResolutionForItsOwnWait(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil), func(g *gateHarness) { g.reviewDelay = 3 * time.Hour })
	g.state = RunState{Status: "running", Usage: TaskUsage{AgentTime: 12 * time.Minute, Attempts: 3}}
	g.escalationIDs = []int64{601, 602}
	g.onSessions(pushed("s4", "c4"))
	g.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9}, nil)
	g.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).After(20*time.Minute).Return(ChecksResult{State: ChecksSuccess}, nil)
	g.onGateSupervisor(GateDecision{Action: ActionEscalate, Diagnosis: "the review model hangs", Recommendation: "look at the gateway"})
	g.signalAfter(30*time.Minute, SignalCaseResolved, caseResolved(702, Resolution{Action: ActReverify}, 610))
	extend := Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}, Note: "one more, then stop"}
	g.env.RegisterDelayedCallback(func() {
		g.rowUsage.RaisedSessions = 1
		g.env.SignalWorkflow(SignalCaseResolved, caseResolved(701, extend, 605))
	}, time.Hour)
	g.runTripAfter(time.Hour+15*time.Minute, ActivityModelReview, 1, 2)
	g.signalAfter(10*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(g.trips) != 2 || !strings.HasPrefix(g.trips[0].Reason, capReachedLadder) {
		t.Fatalf("trips = %+v, want the session cap's and the review stop's", g.trips)
	}
	want := []string{`sebastian, to "` + capReachedLadder + `": one more, then stop`}
	if len(g.sessions) != 1 || g.sessions[0].Attempt != 4 || g.sessions[0].Sessions != 4 || !slices.Equal(g.sessions[0].Answers, want) {
		t.Fatalf("sessions = %+v, want the extend's fourth of four with %q", g.sessions, want)
	}
	if last := g.reviewGates[len(g.reviewGates)-1]; !last.ChecksPassed || last.Beside {
		t.Fatalf("review gates %+v: want the last review after the green checks, not beside them", g.reviewGates)
	}
	if len(g.requested) != 1 || g.requested[0].Sub(g.started) != 4*time.Hour+20*time.Minute {
		t.Fatalf("approval requested %v, want once at 4h20m: the extend's hour, the checks' 20 min and the sequential review's 3h", g.requested)
	}
}

// In a run from before the case queue the same answer is dropped, and the
// question must be answered again once it is asked: the run waits the
// quarter hour between the stop's ask and the second answer, so its
// approval comes at 4h30m, not 4h20m.
func TestWorkflowBeforeCaseQueueWaitsForTheAnswerAgain(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil), func(g *gateHarness) { g.reviewDelay = 3 * time.Hour })
	g.beforeCaseQueue()
	g.state = RunState{Status: "running", Usage: TaskUsage{AgentTime: 12 * time.Minute, Attempts: 3}}
	g.escalationIDs = []int64{601, 602}
	g.onSessions(pushed("s4", "c4"))
	g.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9}, nil)
	g.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).After(20*time.Minute).Return(ChecksResult{State: ChecksSuccess}, nil)
	g.onGateSupervisor(GateDecision{Action: ActionEscalate, Diagnosis: "the review model hangs", Recommendation: "look at the gateway"})
	g.signalAfter(30*time.Minute, SignalCaseResolved, caseResolved(702, Resolution{Action: ActReverify}, 610))
	extend := Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}, Note: "one more, then stop"}
	g.env.RegisterDelayedCallback(func() {
		g.rowUsage.RaisedSessions = 1
		g.env.SignalWorkflow(SignalCaseResolved, caseResolved(701, extend, 605))
	}, time.Hour)
	g.runTripAfter(time.Hour+15*time.Minute, ActivityModelReview, 1, 2)
	g.signalAfter(time.Hour+30*time.Minute, SignalCaseResolved, caseResolved(702, Resolution{Action: ActReverify}, 611))
	g.signalAfter(10*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(g.trips) != 2 {
		t.Fatalf("trips = %+v, want the session cap's and the review stop's", g.trips)
	}
	if len(g.requested) != 1 || g.requested[0].Sub(g.started) != 4*time.Hour+30*time.Minute {
		t.Fatalf("approval requested %v, want once at 4h30m: the answer had to come again after the ask", g.requested)
	}
}

// A healed run's waits take the case's resolution: the human's wait
// reverifies the pull request's head with no session, the cap's wait
// hands the next session to the stronger model, and the screening's
// closes the card into a successor.
func TestWorkflowHealedWaitsTakeTheResolution(t *testing.T) {
	t.Run("the human's wait", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.state = RunState{Status: "waiting-human", PR: PullRequest{Number: 9}, Head: "c3", Question: capReachedLadder + "\nlast: the review asked for changes"}
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
// opens. The session it buys is sebastian's draw against the pool.
func TestWorkflowChangeRequestAtTheCapBuysTheSession(t *testing.T) {
	h := newWorkflowHarness(t)
	h.branchHead = "c2"
	var grants []string
	h.env.OnActivity(ActivityRecordGrant, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, by string, d policy.Draw, key string) error {
			grants = append(grants, fmt.Sprintf("%s %+v %s", by, d, key))
			return nil
		})
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
	if want := []string{fmt.Sprintf("sebastian %+v %s", policy.Draw{Sessions: 1}, "card_extended default-test-run-id 1 0s")}; !slices.Equal(grants, want) {
		t.Fatalf("grants drawn = %q, want %q", grants, want)
	}
}

// The audit's gap verdict at the session cap — the hold-out settlement's
// findings, which name no writer — buys its session as the factory's own
// draw (versionAuditDraw): rung 0 against the pool, keyed to the run
// (grantKey), and no cap card opens. The session it buys carries the
// audit's finding.
func TestWorkflowAuditGapAtTheCapIsTheFactoryDraw(t *testing.T) {
	finding := "Every key turn lands in the log (hold-out key_log): the audit found a real gap the spec names and the plan's proof does not cover; work the criterion."
	g := newGateHarness(t, func(g *gateHarness) {
		g.criteria = []CriteriaResult{
			{Holdouts: []HoldoutGateResult{{Idx: 1, ID: "key_log", Verdict: "red", Evidence: "--- FAIL: TestKeyLog (0.01s)"}}},
			{},
		}
		g.settlements = []HoldoutSettlement{{Findings: []string{finding}}}
	})
	g.branchHead = "c2"
	var draws []string
	g.env.OnActivity(ActivityAuditDraw, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, d policy.Draw, key string) error {
			draws = append(draws, fmt.Sprintf("%s %+v %s", taskID, d, key))
			return nil
		})
	var steps []string
	g.onVerify(&steps, []SessionResult{
		{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "no changes to push"},
		{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "no changes to push"},
		{Outcome: OutcomeRedirect, SessionID: "s3", Commit: "c2", Trip: "repetition", Summary: "supervisor: redirect after repetition", Findings: []string{"f3"}},
		pushed("s4", "c4")})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if want := []string{"session 1 after 0 cards", "session 2 after 0 cards", "session 3 after 0 cards", "pr c2 after 0 cards",
		"session 4 after 0 cards", "pr c4 after 0 cards"}; !slices.Equal(steps, want) || len(g.escalated) != 0 {
		t.Fatalf("steps %q, asked %q; want %q and no one asked", steps, g.escalated, want)
	}
	if want := []string{"policy", "repro c2", "review c2", "criteria c2", "settle c2", "policy", "repro c4", "review c4", "criteria c4", "approval c4", "merge"}; !slices.Equal(g.steps, want) {
		t.Fatalf("gates %q, want %q", g.steps, want)
	}
	if s := g.sessions[3]; s.Attempt != 4 || !slices.Equal(s.Findings, []string{finding}) || g.usage[len(g.usage)-1].ExtraAttempts != 1 {
		t.Fatalf("fourth session %+v, usage %+v; want attempt 4 with the audit's finding, one attempt granted", s, g.usage[len(g.usage)-1])
	}
	if want := []string{fmt.Sprintf("t1 %+v %s", policy.Draw{Sessions: 1}, "card_extended default-test-run-id 1 0s")}; !slices.Equal(draws, want) {
		t.Fatalf("audit draws = %q, want %q", draws, want)
	}
}

// The pool empty, the same audit gap at the cap buys no session: the
// factory's draw refuses, the bought session and its time go back, and
// the card asks a human, whose extend on the console (a raised session
// on the row) starts the next session with the audit's finding.
func TestWorkflowAuditGapAtAnEmptyPoolAsksAHuman(t *testing.T) {
	finding := "Every key turn lands in the log (hold-out key_log): the audit found a real gap the spec names and the plan's proof does not cover; work the criterion."
	g := newGateHarness(t, func(g *gateHarness) {
		g.criteria = []CriteriaResult{
			{Holdouts: []HoldoutGateResult{{Idx: 1, ID: "key_log", Verdict: "red", Evidence: "--- FAIL: TestKeyLog (0.01s)"}}},
			{},
		}
		g.settlements = []HoldoutSettlement{{Findings: []string{finding}}}
	})
	g.branchHead = "c2"
	g.env.OnActivity(ActivityAuditDraw, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, d policy.Draw, key string) error {
			return temporal.NewNonRetryableApplicationError("the escalation pool is empty: no session left of its bound", errDrawRefused, nil)
		})
	var steps []string
	g.onVerify(&steps, []SessionResult{
		{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "no changes to push"},
		{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "no changes to push"},
		{Outcome: OutcomeRedirect, SessionID: "s3", Commit: "c2", Trip: "repetition", Summary: "supervisor: redirect after repetition", Findings: []string{"f3"}},
		pushed("s4", "c4")})
	g.env.RegisterDelayedCallback(func() {
		g.rowUsage = TaskUsage{RaisedSessions: 1}
		g.env.SignalWorkflow(SignalCaseResolved, caseResolved(701, Resolution{Action: ActHandover, Args: map[string]any{"model": "stronger"}}, 605))
	}, 2*time.Hour)
	g.signalAfter(5*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if want := []string{"session 1 after 0 cards", "session 2 after 0 cards", "session 3 after 0 cards", "pr c2 after 0 cards",
		"session 4 after 1 cards", "pr c4 after 1 cards"}; !slices.Equal(steps, want) {
		t.Fatalf("steps %q, want %q", steps, want)
	}
	if want := []string{"policy", "repro c2", "review c2", "criteria c2", "settle c2", "policy", "repro c4", "review c4", "criteria c4", "approval c4", "merge"}; !slices.Equal(g.steps, want) {
		t.Fatalf("gates %q, want %q", g.steps, want)
	}
	if len(g.escalated) != 1 || !strings.HasPrefix(g.escalated[0], capReachedLadder) || !strings.Contains(g.escalated[0], finding) {
		t.Fatalf("asked %q, want one session-cap card holding the audit's finding", g.escalated)
	}
	if s := g.sessions[3]; s.Attempt != 4 || s.Model != "factory/big" || !slices.Equal(s.Findings, []string{finding}) {
		t.Fatalf("fourth session %+v, want attempt 4 on factory/big with the audit's finding, after the ask", s)
	}
	if u := g.usage[len(g.usage)-1]; u.ExtraAttempts != 0 {
		t.Fatalf("usage %+v, want no attempt granted by the refused draw", u)
	}
}

// A run from before versionAuditDraw buys the audit's gap session at the
// cap ungated, as a writer's ran before versionCases: no draw, no card.
func TestWorkflowAuditGapAtTheCapFromBeforeAuditDrawBuysUngated(t *testing.T) {
	finding := "Every key turn lands in the log (hold-out key_log): the audit found a real gap the spec names and the plan's proof does not cover; work the criterion."
	g := newGateHarness(t, func(g *gateHarness) {
		g.criteria = []CriteriaResult{
			{Holdouts: []HoldoutGateResult{{Idx: 1, ID: "key_log", Verdict: "red", Evidence: "--- FAIL: TestKeyLog (0.01s)"}}},
			{},
		}
		g.settlements = []HoldoutSettlement{{Findings: []string{finding}}}
	})
	g.beforeAuditDraw()
	g.branchHead = "c2"
	var draws int
	g.env.OnActivity(ActivityAuditDraw, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, d policy.Draw, key string) error {
			draws++
			return nil
		})
	var steps []string
	g.onVerify(&steps, []SessionResult{
		{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "no changes to push"},
		{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "no changes to push"},
		{Outcome: OutcomeRedirect, SessionID: "s3", Commit: "c2", Trip: "repetition", Summary: "supervisor: redirect after repetition", Findings: []string{"f3"}},
		pushed("s4", "c4")})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if s := g.sessions[3]; s.Attempt != 4 || !slices.Equal(s.Findings, []string{finding}) || len(g.escalated) != 0 || draws != 0 {
		t.Fatalf("fourth session %+v, asked %q, draws %d; want attempt 4 ungated, no one asked, no draw", s, g.escalated, draws)
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

// An agent's escalation asks a human with the agent's own choices on the ask;
// the answer reaches the next session tagged with the stop's words, with the
// findings the agent had and the stopped session as its parent.
func TestWorkflowEscalationWaitsForAHumanAnswer(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(
		SessionResult{Outcome: OutcomeEscalate, SessionID: "s1", Reason: "the check cannot pass\n\nProgress: 9 commands", Findings: []string{"ran x"},
			Choices: []string{"edit the script", "skip the check"}},
		pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "you may edit the script", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if len(h.escalated) != 1 || !strings.Contains(h.escalated[0], "the check cannot pass") || len(h.choices) != 1 ||
		!slices.Equal(h.choices[0], []string{"edit the script", "skip the check"}) {
		t.Fatalf("escalations = %q with choices %q", h.escalated, h.choices)
	}
	if len(h.sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(h.sessions))
	}
	second := h.sessions[1]
	if !slices.Equal(second.Findings, []string{"ran x"}) || !slices.Equal(second.Answers, []string{`sebastian, to "the check cannot pass": you may edit the script`}) || second.ParentSessionID != "s1" {
		t.Fatalf("second attempt = %+v", second)
	}
}

// The slice a human answer buys is a quarter of the task's resolved agent
// time, floored at min(minSession, WallClockCap) so the attempt it buys is
// never born under the 5 minutes a session needs to start. Each case's first
// session ends with no model answer, all but one minute of the cap spent, so
// the grant tops the one minute up to the slice: ExtraTime is the slice less
// that minute. The session after it (1 minute, inside even the smallest
// slice) takes the number the first left.
func TestAnswerSliceIsAQuarterOfTheAgentTime(t *testing.T) {
	for name, tc := range map[string]struct {
		cap       time.Duration
		wantExtra time.Duration
	}{
		"2h, used 119m, a quarter": {cap: 2 * time.Hour, wantExtra: 29 * time.Minute},
		"10m, used 9m, the floor":  {cap: 10 * time.Minute, wantExtra: 4 * time.Minute},
		"30m, used 29m, a quarter": {cap: 30 * time.Minute, wantExtra: 6*time.Minute + 30*time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testWorkflowConfig
			cfg.WallClockCap = tc.cap
			h := newWorkflowHarness(t, TaskWorkflow{Config: func() WorkflowConfig { return cfg }})
			h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(tc.cap - time.Minute).Return(
				func(ctx context.Context, in SessionInput) (SessionResult, error) {
					h.sessions = append(h.sessions, in)
					return SessionResult{Outcome: OutcomeEscalate, SessionID: "s1", Reason: "stuck"}, nil
				}).Once()
			h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(time.Minute).Return(
				func(ctx context.Context, in SessionInput) (SessionResult, error) {
					h.sessions = append(h.sessions, in)
					return pushed("s2", "c2"), nil
				})
			h.env.OnActivity(ActivitySessionAnswered, mock.Anything, mock.Anything).Return(
				func(ctx context.Context, in SessionAnswerInput) (bool, error) { return in.SessionID != "s1", nil })
			h.onChecks(ChecksResult{State: ChecksSuccess})
			h.signalAfter(tc.cap+time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "try again", Comment: 602})
			h.signalAfter(tc.cap+2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

			if final := h.run(t); final != "done" || len(h.sessions) != 2 {
				t.Fatalf("final = %q after %d sessions, finals = %v; want done after the answer's session", final, len(h.sessions), h.finals)
			}
			want := TaskUsage{AgentTime: tc.cap, Attempts: 1, ExtraTime: tc.wantExtra}
			if last := h.usage[len(h.usage)-1]; last != want {
				t.Fatalf("usage = %+v, want %+v", last, want)
			}
		})
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
	if want := []string{timeCapReachedLadder + "\nlast: checks findings went back to the agent\n- checks, round 1: gosec G706"}; !slices.Equal(h.escalated, want) {
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
// names the exits instead, both trip as the call cap's.
func TestCheckinSpendStopTellsItsTripTheStop(t *testing.T) {
	const callCapStop = checkinCallCapStop + " after 20 calls"
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	var steps []string
	h.onVerify(&steps, []SessionResult{
		pushed("s1", "c1"),
		checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
			Reason: callCapStop, Record: &CheckInRecord{Moment: CheckInGate, Session: "s2", ParentSession: "s1"}}),
		checkinStopped("s3", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
			Reason: callCapStop, Record: &CheckInRecord{Moment: CheckInGate, Session: "s3", ParentSession: "s2"}})})
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			return Review{Verdict: "concerns", Summary: "one fault", Findings: []string{"rename the flag"}}, nil
		})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" {
		t.Fatalf("final = %q, escalations = %q", final, h.escalated)
	}
	if len(h.trips) != 2 || strings.Contains(h.trips[1].Reason, callCapStop) {
		t.Fatalf("trips = %+v, want the call cap stop and a repeat whose ask names no cap", h.trips)
	}
	for i, trip := range h.trips {
		if trip.CheckinStop != CheckinStopCallCap {
			t.Errorf("trip %d's stop = %q, want %q", i, trip.CheckinStop, CheckinStopCallCap)
		}
	}
}

// A close resolution reaches the run while Act's close of the issue is on
// its way: the run waits for the issue's close and ends on it, with no ask
// again, no gate run again and no session, at the session cap, at a
// gate's wait and on a failed dependency alike.
func TestWorkflowCloseWaitsForTheIssuesClose(t *testing.T) {
	closing := Resolution{Action: ActClose}
	closed := [2]string{"closed", ReasonIssueClosed + " without merge"}
	t.Run("the session cap", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.state = RunState{Status: "running", Usage: TaskUsage{AgentTime: 12 * time.Minute, Attempts: 3}}
		h.onSessions(pushed("s4", "c4"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(time.Hour, SignalCaseResolved, caseResolved(701, closing, 0))
		h.signalAfter(2*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

		h.run(t)
		if len(h.finals) != 1 || h.finals[0] != closed || len(h.escalated) != 1 || len(h.sessions) != 0 {
			t.Fatalf("finals %v, asked %q, sessions %d; want %v after the one ask and no session", h.finals, h.escalated, len(h.sessions), closed)
		}
	})
	t.Run("a gate's wait", func(t *testing.T) {
		g := newGateHarnessWith(t, stagesWorkflow(3, nil))
		g.onSessions(pushed("s1", "c1"))
		g.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9}, nil)
		var waits []string
		g.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).After(30 * time.Minute).Return(
			func(ctx context.Context, taskID, sha string) (ChecksResult, error) {
				waits = append(waits, sha)
				return ChecksResult{State: ChecksSuccess}, nil
			})
		g.onGateSupervisor(GateDecision{Action: ActionEscalate, Diagnosis: "no runner serves the label", Recommendation: "register a runner"})
		g.tripAfter(11*time.Minute, ActivityWaitChecks, 1)
		g.signalAfter(time.Hour, SignalCaseResolved, caseResolved(701, closing, 0))
		g.signalAfter(5*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

		g.run(t)
		if len(g.finals) != 1 || g.finals[0] != closed || !slices.Equal(waits, []string{"c1"}) || slices.Contains(g.steps, "approval c1") {
			t.Fatalf("finals %v, checks waited on %q, steps %q; want %v with the tripped checks wait alone", g.finals, waits, g.steps, closed)
		}
	})
	t.Run("a failed dependency", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.taskConfig = depsConfig()
		failed := Dependency{TaskID: "t-a", Issue: 5, Key: "links", Status: "failed"}
		h.env.OnActivity(ActivityDependencies, mock.Anything, "t1").Return(DepState{Waiting: []int64{5}, Failed: &failed}, nil)
		h.onSessions(pushed("s1", "c1"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(time.Hour, SignalCaseResolved, caseResolved(701, closing, 0))
		h.signalAfter(2*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

		h.run(t)
		if len(h.finals) != 1 || h.finals[0] != closed || len(h.escalated) != 1 || len(h.sessions) != 0 {
			t.Fatalf("finals %v, asked %q, sessions %d; want %v before the card starts", h.finals, h.escalated, len(h.sessions), closed)
		}
	})
}

// A checks wait's stop answered with wait_checks runs the wait again on
// the same commit, and green checks go on to the next gate: no session, no
// agent time, no new head.
func TestWorkflowWaitChecksResolutionRunsTheChecksWaitAgain(t *testing.T) {
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
	g.onGateSupervisor(GateDecision{Action: ActionEscalate, Diagnosis: "the checks outlasted their bound", Recommendation: "keep waiting"})
	g.tripAfter(11*time.Minute, ActivityWaitChecks, 1)
	g.signalAfter(time.Hour, SignalCaseResolved, caseResolved(701, Resolution{Action: ActWaitChecks, Note: "Keep waiting"}, 605))
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if !slices.Equal(waits, []string{"c1", "c1"}) || len(g.sessions) != 1 || !slices.Contains(g.steps, "approval c1") {
		t.Fatalf("checks waited on %q, sessions %d, steps %q; want the wait run again on c1, one session and c1 at its approval", waits, len(g.sessions), g.steps)
	}
	if last := g.usage[len(g.usage)-1]; last.ExtraAttempts != 0 {
		t.Fatalf("usage = %+v, want no granted attempt", last)
	}
}

// A rung's verdicts ride its resolution to the run: once the resolution is
// taken they stand with the check-ins' verdicts, so the next check-in's ask
// counts the finding a rung dropped as dropped before, and the last review
// carries the drop for the next review to read: both as a gate check-in's
// drop does. A run from before the change takes none.
func TestTakingAResolutionFeedsTheRunsVerdicts(t *testing.T) {
	const finding = "TasksByPR is undefined"
	drop := map[string]CheckVerdict{finding: {State: CheckDropped, Evidence: "tasks.go:12 at head: TasksByPR is declared"}}
	type seen struct {
		Priors []map[string]CheckVerdict
		Review map[string]CheckVerdict
		Prior  int
	}
	run := func(before bool) seen {
		var suite testsuite.WorkflowTestSuite
		env := suite.NewTestWorkflowEnvironment()
		env.RegisterWorkflowWithOptions(func(ctx workflow.Context) (seen, error) {
			tr := &taskRun{ctx: ctx, resolution: &Resolution{Action: ActExtend, Verdicts: drop}, reviews: []Review{{Verdict: "concerns", Findings: []string{finding}}}}
			tr.takeResolution()
			ask := tr.checkinAsk("s1", []string{finding}, []string{"review"}, GateResults{})
			return seen{Priors: tr.checkinVerdicts, Review: tr.reviews[0].Verdicts, Prior: ask.Findings[0].PriorDropped}, nil
		}, workflow.RegisterOptions{Name: "verdicts"})
		if before {
			env.OnGetVersion("case-verdicts", workflow.DefaultVersion, versionCaseVerdicts).Return(workflow.DefaultVersion)
		}
		env.ExecuteWorkflow("verdicts")
		var got seen
		if err := env.GetWorkflowResult(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := run(false); !reflect.DeepEqual(got.Priors, []map[string]CheckVerdict{drop}) || !maps.Equal(got.Review, drop) || got.Prior != 1 {
		t.Fatalf("after the change: %+v, want the drop among the verdicts, on the last review and counted once as a prior", got)
	}
	if got := run(true); got.Priors != nil || got.Review != nil || got.Prior != 0 {
		t.Fatalf("a run from before: %+v, want no verdict taken", got)
	}
}

// A finding the next review words with other case or spacing is the same
// finding: the ask counts each earlier check-in's verdict on it once. A run
// from before counts by exact text.
func TestCheckinAskCountsARepeatByFindingID(t *testing.T) {
	const finding = "The file  lacks the check."
	confirmed := func(text string) map[string]CheckVerdict {
		return map[string]CheckVerdict{text: {State: CheckConfirmed}}
	}
	priors := []map[string]CheckVerdict{
		confirmed("the file lacks the check."), confirmed(finding), confirmed("THE FILE LACKS THE CHECK."),
		confirmed("the file lacks the test."),
	}
	run := func(before bool) int {
		var suite testsuite.WorkflowTestSuite
		env := suite.NewTestWorkflowEnvironment()
		env.RegisterWorkflowWithOptions(func(ctx workflow.Context) (int, error) {
			tr := &taskRun{ctx: ctx, checkinVerdicts: priors}
			return tr.checkinAsk("s1", []string{finding}, []string{"review"}, GateResults{}).Findings[0].PriorConfirmed, nil
		}, workflow.RegisterOptions{Name: "priors"})
		if before {
			env.OnGetVersion("checkin-priors", workflow.DefaultVersion, versionCheckinPriors).Return(workflow.DefaultVersion)
		}
		env.ExecuteWorkflow("priors")
		var got int
		if err := env.GetWorkflowResult(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := run(false); got != 3 {
		t.Fatalf("after the change: %d prior confirmations, want the 3 check-ins that worded it the same", got)
	}
	if got := run(true); got != 1 {
		t.Fatalf("a run from before: %d, want the 1 of the exact text", got)
	}
}

// A verdict that names the earlier finding it repeats joins that finding's
// group: the ask lists one earlier finding with both confirmations, and the
// finding in either wording counts them; a wording the check-ins never
// judged counts none.
func TestCheckinAskGroupsNamedRepeats(t *testing.T) {
	const first, second, fresh = "notRunRow skips the row", "the guard drops the audit row", "an unrelated finding"
	priors := []map[string]CheckVerdict{
		{first: {State: CheckConfirmed}},
		{second: {State: CheckConfirmed, Repeats: findingID(first)}},
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(func(ctx workflow.Context) (*CheckInAsk, error) {
		tr := &taskRun{ctx: ctx, checkinVerdicts: priors}
		return tr.checkinAsk("s1", []string{second, fresh}, []string{"review", "review"}, GateResults{}), nil
	}, workflow.RegisterOptions{Name: "groups"})
	env.ExecuteWorkflow("groups")
	var got CheckInAsk
	if err := env.GetWorkflowResult(&got); err != nil {
		t.Fatal(err)
	}
	if want := []PriorFinding{{ID: findingID(first), Text: first, Confirmed: 2}}; !reflect.DeepEqual(got.Priors, want) {
		t.Fatalf("priors = %+v, want %+v", got.Priors, want)
	}
	if got.Findings[0].PriorConfirmed != 2 || got.Findings[0].Group != findingID(first) || got.Findings[1].PriorConfirmed != 0 || got.Findings[1].Group != "" {
		t.Fatalf("findings = %+v, want the second wording counting both confirmations and the fresh one none", got.Findings)
	}
}

// The plan loop's wait at the session cap ends its case when it returns with
// no answer of it: a writer's comment while rung 1 holds the case or the
// human has its face, the issue's close, and the plan's questions answered.
// Rung 1's own answer ends nothing.
func TestPlanWaitEndsTheCaseOfAWaitItLeaves(t *testing.T) {
	invalid := []string{"plan: card web: size huge: split it; a card is one package, one page or one set of endpoints"}
	run := func(held bool, sigs func(h *planHarness)) []string {
		h := newPlanHarness(t, PlanResult{Findings: invalid}, PlanResult{Findings: invalid}, PlanResult{Findings: invalid}, PlanResult{Revision: 1, SHA256: "sha-1"})
		h.file = map[int]bool{1: true}
		h.rungOne = held
		start := h.env.Now()
		h.env.OnActivity(ActivityReadUsage, mock.Anything, "t1").Return(func(ctx context.Context, taskID string) (TaskUsage, error) {
			row := h.rowUsage
			if h.env.Now().Sub(start) >= time.Hour {
				row.RaisedSessions = 2
			}
			return row, nil
		})
		sigs(h)
		h.signalAfter(3*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
		h.run(t)
		return h.ended
	}
	comment := func(h *planHarness) {
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "alice", Body: "keep the web card small", Comment: 610})
	}
	for _, held := range []bool{true, false} {
		if got := run(held, comment); !slices.Equal(got, []string{"701 a writer's comment moved the plan on"}) {
			t.Errorf("held %v, a writer's comment: ended %q, want case 701 for the comment", held, got)
		}
	}
	if got := run(true, func(*planHarness) {}); !slices.Equal(got, []string{"701 the run ended: " + ReasonIssueClosed + " before the plan filed its cards"}) {
		t.Errorf("the issue's close: ended %q, want case 701 for the run's end", got)
	}
	resolved := func(h *planHarness) {
		h.signalAfter(time.Hour, SignalCaseResolved, TaskSignal{Author: CheckinDecider, Case: &CaseSignal{CaseID: 701, Rung: 1, By: CheckinDecider,
			Res: Resolution{Action: ActExtend, Args: map[string]any{"sessions": 2}}}})
	}
	if got := run(true, resolved); len(got) != 0 {
		t.Errorf("rung 1's answer: ended %q, want none", got)
	}
	answered := func(h *planHarness) {
		h.open = []int{0}
		h.signalAfter(time.Hour, SignalPlanAnswered, TaskSignal{Author: "alice", Revision: 0, Index: 1})
	}
	if got := run(true, answered); !slices.Equal(got, []string{"701 the plan's questions were answered"}) {
		t.Errorf("the plan's answers: ended %q, want case 701 for the answers", got)
	}
}

// A group revert's agent card has two sessions, not the policy's three: its
// TaskConfig says so, and the workflow opens the session cap's case after the
// second, with the findings of both, before any third session runs.
func TestGroupRevertCardHasTwoSessions(t *testing.T) {
	pinned := fixtureVersion(t)
	for source, want := range map[string]int{SourceGroupRevert: groupRevertSessions, SourceRevert: 0, "webhook": 0} {
		r, _, _ := newActivityRunner(t, Task{ID: "t1", Kind: "agent", Status: "queued", Repository: "demo-app/demo-app", Source: source, Track: "direct"}, &prForgejo{})
		r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
		val, err := execActivity(t, r, ActivityTaskConfig, "t1", int64(7))
		if got := getInto[TaskConfig](t, val, err); got.Sessions != want {
			t.Errorf("a %s card's TaskConfig has %d sessions, want %d", source, got.Sessions, want)
		}
	}

	h := newWorkflowHarness(t)
	h.taskConfig = &TaskConfig{Process: "maintenance", Track: policy.TrackDirect, Class: policy.ClassCoder, Session: policy.SessionCode,
		Stages: testStages(), Queue: policy.QueueNone, Sessions: groupRevertSessions}
	h.onSessions(
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "no changes to push", Findings: []string{"f1"}},
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "no changes to push", Findings: []string{"f2"}},
		pushed("s3", "c3"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	extend := Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}, Note: "keep the later fix"}
	h.env.RegisterDelayedCallback(func() {
		h.rowUsage.RaisedSessions = 1
		h.env.SignalWorkflow(SignalCaseResolved, caseResolved(701, extend, 605))
	}, time.Hour)
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	wantCard := capReachedLadder + "\nlast: no changes to push\n- session, attempt 1: f1\n- session, attempt 2: f2"
	if !slices.Equal(h.escalated, []string{wantCard}) {
		t.Fatalf("escalations = %q, want the card after the second session %q", h.escalated, wantCard)
	}
	if len(h.sessions) != 3 || h.sessions[0].Sessions != 2 || h.sessions[2].Attempt != 3 {
		t.Fatalf("sessions = %+v, want two of two, then the one the extend bought", h.sessions)
	}
}

// A card's agent time follows its size: the sizing
// step's large card gets one and a half of the class's 20 minutes, so a
// 25-minute session that pushes is within its time, where the class's time
// alone would have reached the cap; a small card gets half, so a 10-minute
// session without a push reaches it and the card asks for time.
func TestSizedCardAgentTimeFollowsItsSize(t *testing.T) {
	for name, tc := range map[string]struct {
		size   string
		took   time.Duration
		result SessionResult
		asks   int
	}{
		"large": {"large", 25 * time.Minute, pushed("s1", "c1"), 0},
		"small": {"small", 10 * time.Minute, SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "no changes to push"}, 1},
	} {
		h := newWorkflowHarness(t)
		h.taskConfig = askTaskConfig()
		h.taskConfig.PlanShape = &policy.PlanShape{}
		h.state = RunState{Status: "running", Source: "webhook"}
		h.env.OnActivity(ActivitySizeCard, mock.Anything, "t1").Return(
			func(ctx context.Context, taskID string) (SizeResult, error) { return SizeResult{Size: tc.size}, nil })
		h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(tc.took).Return(
			func(ctx context.Context, in SessionInput) (SessionResult, error) {
				h.sessions = append(h.sessions, in)
				return tc.result, nil
			})
		h.onChecks(ChecksResult{State: ChecksSuccess})
		final := "done"
		if tc.asks > 0 {
			final = "closed"
			h.signalAfter(2*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
		} else {
			h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
		}

		if got := h.run(t); got != final || len(h.sessions) != 1 || len(h.escalated) != tc.asks {
			t.Fatalf("%s: final = %q after %d sessions, asked %q, finals = %v; want %s after one session and %d ask", name, got, len(h.sessions), h.escalated, h.finals, final, tc.asks)
		}
		if tc.asks > 0 && !strings.HasPrefix(h.escalated[0], timeCapReachedLadder) {
			t.Fatalf("%s: asked %q, want the agent time's card", name, h.escalated)
		}
	}
}

// The last session stops with work in progress (a redirect with c2): at the
// session cap c2 goes through verification; its red checks go to the one
// cap case, not to an approval card, and the session the extend buys gets
// them.
func TestSessionCapVerifiesUnverifiedHead(t *testing.T) {
	h := newWorkflowHarness(t)
	h.branchHead = "c2"
	var steps []string
	h.onVerify(&steps, []SessionResult{
		{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "no changes to push"},
		{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "no changes to push"},
		{Outcome: OutcomeRedirect, SessionID: "s3", Commit: "c2", Trip: "repetition", Summary: "supervisor: redirect after repetition", Findings: []string{"f3"}},
		pushed("s4", "c4")}, gosecRed, ChecksResult{State: ChecksSuccess})
	extend := Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}}
	h.env.RegisterDelayedCallback(func() {
		h.rowUsage.RaisedSessions = 1
		h.env.SignalWorkflow(SignalCaseResolved, caseResolved(701, extend, 605))
	}, time.Hour)
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if want := []string{"session 1 after 0 cards", "session 2 after 0 cards", "session 3 after 0 cards", "pr c2 after 0 cards",
		"session 4 after 1 cards", "pr c4 after 1 cards", "approval c4"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want %v", steps, want)
	}
	if want := []string{capReachedLadder + "\nlast: checks findings went back to the agent\n- supervisor:repetition, attempt 3: f3\n- checks, round 1: gosec G706"}; !slices.Equal(h.escalated, want) {
		t.Fatalf("asked %q, want %q", h.escalated, want)
	}
	if h.sessions[3].StartRef != "c2" || !slices.Equal(h.sessions[3].Findings, []string{"gosec G706"}) {
		t.Fatalf("fourth session = %+v, want it from c2 with the checks finding", h.sessions[3])
	}
}

// A run restarted with a change request pending on its PR's head c1 and the
// whole 20m used: c1 counts as judged, so the time cap's case opens with no
// second verification of c1.
func TestTimeCapAdoptedHeadIsJudged(t *testing.T) {
	h := newWorkflowHarness(t)
	h.branchHead = "c1"
	change := "reviewer sebastian requested changes: rename it"
	h.state = RunState{Status: "in-review", PR: PullRequest{Number: 9}, Head: "c1", Findings: []string{change},
		Usage: TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1}}
	var steps []string
	h.onVerify(&steps, []SessionResult{pushed("s2", "c2")})
	extend := Resolution{Action: ActExtend, Args: map[string]any{"agent_time_s": 600}}
	h.env.RegisterDelayedCallback(func() {
		h.rowUsage.RaisedTime = 10 * time.Minute
		h.env.SignalWorkflow(SignalCaseResolved, caseResolved(701, extend, 605))
	}, time.Hour)
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if want := []string{"session 2 after 1 cards", "pr c2 after 1 cards", "approval c2"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want %v", steps, want)
	}
	if want := []string{timeCapReachedLadder + "\nlast: no session produced a result\n- " + change}; !slices.Equal(h.escalated, want) {
		t.Fatalf("asked %q, want %q", h.escalated, want)
	}
}

// With 20m of agent time and 16m used, 4m are left: no session starts and the
// time cap opens its case. 15m used leaves 5m: a session starts. An agent
// time of 3m, unused, starts a session too; with 1m used, 2m are left: none.
func TestNoSessionUnderFiveMinutes(t *testing.T) {
	for name, tc := range map[string]struct {
		cap, used time.Duration
		sessions  int
	}{
		"20m, 16m used": {20 * time.Minute, 16 * time.Minute, 0},
		"20m, 15m used": {20 * time.Minute, 15 * time.Minute, 1},
		"3m, none used": {3 * time.Minute, 0, 1},
		"3m, 1m used":   {3 * time.Minute, time.Minute, 0},
	} {
		cfg := testWorkflowConfig
		cfg.WallClockCap = tc.cap
		h := newWorkflowHarness(t, TaskWorkflow{Config: func() WorkflowConfig { return cfg }})
		h.state = RunState{Status: "running", Usage: TaskUsage{AgentTime: tc.used, Attempts: 1}}
		var steps []string
		h.onVerify(&steps, []SessionResult{pushed("s2", "c2")})
		final, asks := "done", 0
		if tc.sessions == 0 {
			final, asks = "closed", 1
			h.signalAfter(time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
		} else {
			h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
		}

		if got := h.run(t); got != final || len(h.sessions) != tc.sessions || len(h.trips) != asks {
			t.Fatalf("%s: final = %q, sessions = %d, trips = %d, finals = %v; want %s, %d sessions and %d trips", name, got, len(h.sessions), len(h.trips), h.finals, final, tc.sessions, asks)
		}
	}
}

// A session that cannot start goes to a human like any stop, but it ran no
// agent: its minute is no agent time, so the card keeps its whole time and the
// session the extend buys runs on the same model.
func TestWorkflowSessionThatCannotStartTakesNoAgentTime(t *testing.T) {
	h := newWorkflowHarness(t)
	reason := startEscalation("image pull", "push the worker image or correct its reference",
		"start failed: image pull: container clone waits with ErrImagePull: not found")
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(time.Minute).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			return SessionResult{Outcome: OutcomeEscalate, SessionID: "s1", Model: in.Model, Reason: reason, StartFailed: true}, nil
		}).Once()
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(5 * time.Minute).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			return pushed("s2", "c2"), nil
		})
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalCaseResolved, caseResolved(701, Resolution{Action: ActRetry, Note: "the image is pushed"}, 605))
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 2 {
		t.Fatalf("final = %q after %d sessions, finals = %v; want done after the second session", final, len(h.sessions), h.finals)
	}
	if !slices.Equal(h.escalated, []string{reason}) {
		t.Fatalf("asked %q, want the start failure's", h.escalated)
	}
	if second := h.sessions[1]; second.Model != h.sessions[0].Model {
		t.Fatalf("second session = %+v, want the first's model", second)
	}
	if last := h.usage[len(h.usage)-1]; last.AgentTime != 5*time.Minute || last.ExtraTime != 0 {
		t.Fatalf("usage = %+v, want only the second session's 5 minutes as agent time and no slice bought", last)
	}
}

// A trip that rung 1 holds returns no comment: its face comes later, from
// the hand-up. The face's case_faced signal tells the run the comment, and a
// plain reply to the face then runs the next step as it does after a trip's
// own comment. A reply that came before the signal is counted when it lands.
// A comment written before the face is no answer to it. With no signal the
// reply reads the face from the store: one the store holds counts the reply
// as the signal would, and with none the reply counts for nothing and the
// card waits. That a run that waited on a case before the change takes the
// signal is pinned by the spliced replay.
func TestWorkflowCountsTheReplyToAFacePostedAfterTheTrip(t *testing.T) {
	run := func(t *testing.T, faceAt, replyAt time.Duration, reply int64, stored CaseState) ([]string, int) {
		h := newWorkflowHarness(t)
		h.taskConfig = askTaskConfig()
		h.rungOne = true
		h.caseState = func(int64) CaseState { return stored }
		var steps []string
		h.onVerify(&steps, []SessionResult{
			pushed("s1", "c1"),
			{SessionID: "s2", Outcome: OutcomeCheckinHuman, Commit: "c2", Trip: string(TripNoProgress), Reason: spentStop,
				CheckIn: &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend, Reason: spentStop,
					Record: &CheckInRecord{Moment: CheckInSession, Session: "s2"}}}})
		h.onReviews("c1")
		if faceAt > 0 {
			h.signalAfter(faceAt, SignalCaseFaced, TaskSignal{Author: FactoryDecider, Comment: 601, Case: &CaseSignal{CaseID: 701, Rung: 3, By: FactoryDecider, Comment: 601}})
		}
		h.signalAfter(replyAt, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: reply})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
		h.run(t)
		shown := 0
		for _, r := range h.recorded {
			if r.step == "signal" && r.detail["signal"] == SignalHumanComment && fmt.Sprint(r.detail["comment"]) == fmt.Sprint(reply) {
				shown++
			}
		}
		return steps, shown
	}
	want := []string{"session 1 after 0 cards", "pr c1 after 0 cards", "session 2 after 0 cards", "pr c2 after 1 cards", "approval c2"}
	if got, shown := run(t, 30*time.Minute, time.Hour, 602, CaseState{}); !slices.Equal(got, want) || shown != 1 {
		t.Fatalf("with the face's signal first: steps = %q, shown %d, want %q once", got, shown, want)
	}
	if got, shown := run(t, 2*time.Hour, time.Hour, 602, CaseState{}); !slices.Equal(got, want) || shown != 1 {
		t.Fatalf("with the reply before the signal: steps = %q, reply on the timeline %d times, want it counted when the signal lands, shown once: %q", got, shown, want)
	}
	if got, _ := run(t, 2*time.Hour, time.Hour, 600, CaseState{}); !slices.Equal(got, want[:3]) {
		t.Fatalf("with a comment written before the face: steps = %q, want it no answer to the face: %q", got, want[:3])
	}
	if got, shown := run(t, 0, time.Hour, 602, CaseState{Comment: 601, Ended: true}); !slices.Equal(got, want) || shown != 1 {
		t.Fatalf("with no signal and the face in the store, which the reply resolved: steps = %q, shown %d, want the reply counted once: %q", got, shown, want)
	}
	if got, _ := run(t, 0, time.Hour, 602, CaseState{}); !slices.Equal(got, want[:3]) {
		t.Fatalf("with no signal and no face in the store: steps = %q, want the reply no answer: %q", got, want[:3])
	}
}

// A run that asked before holds a second trip with rung 1: a reply that
// comes before the face's signal is newer than every question, so it is kept
// for the face, not filed under the older question, and it runs the next
// step when the signal lands.
func TestWorkflowKeepsARepliesBeforeTheFaceOfASecondQuestion(t *testing.T) {
	run := func(faceAt, replyAt time.Duration) int {
		h := newWorkflowHarness(t)
		h.escalationIDs = []int64{5, 9}
		year := SessionResult{Outcome: OutcomeEscalate, SessionID: "s2", Reason: "The agent asks: Which year?\nWhy: none\nRecommendation: 2026"}
		h.onSessions(licenseQuestion, year, pushed("s3", "c3"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "use MIT", Comment: 7})
		h.env.RegisterDelayedCallback(func() { h.rungOne = true }, 30*time.Minute)
		h.signalAfter(faceAt, SignalCaseFaced, TaskSignal{Author: FactoryDecider, Comment: 11, Case: &CaseSignal{CaseID: 702, Rung: 3, By: FactoryDecider, Comment: 11}})
		h.signalAfter(replyAt, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "2026", Comment: 12})
		h.signalAfter(5*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
		h.run(t)
		return len(h.sessions)
	}
	first, reply := run(2*time.Hour, 3*time.Hour), run(3*time.Hour, 2*time.Hour)
	if first != 3 || reply != 3 {
		t.Fatalf("sessions %d with the signal first and %d with the reply first, want 3 each: the reply answers the face in either order", first, reply)
	}
}

// A second reply to the license question (5, answered by 7) comes while rung
// 1 holds the year question and the face is unknown: it is kept, and when the
// wait learns the face (11), from its signal or from the store on the next
// reply (12), or when rung 1's own answer ends the wait, it is filed under
// the license question it answers, so the next session reads it beside the
// year's answer.
func TestWorkflowFilesAKeptReplyUnderTheOlderQuestionItAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(h *workflowHarness, start time.Time)
		year string
	}{
		{"the face's signal", func(h *workflowHarness, start time.Time) {
			h.signalAfter(2*time.Hour, SignalCaseFaced, TaskSignal{Author: FactoryDecider, Comment: 11, Case: &CaseSignal{CaseID: 702, Rung: 3, By: FactoryDecider, Comment: 11}})
			h.signalAfter(3*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "2026", Comment: 12})
		}, `to "Which year?": 2026`},
		{"the face in the store", func(h *workflowHarness, start time.Time) {
			h.caseState = func(int64) CaseState {
				if h.env.Now().Sub(start) < 2*time.Hour {
					return CaseState{}
				}
				return CaseState{Comment: 11, Ended: true}
			}
			h.signalAfter(3*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "2026", Comment: 12})
		}, `to "Which year?": 2026`},
		{"rung 1's answer", func(h *workflowHarness, start time.Time) {
			h.signalAfter(2*time.Hour, SignalCaseResolved, TaskSignal{Author: CheckinDecider, Case: &CaseSignal{CaseID: 702, Rung: 1, By: CheckinDecider,
				Res: Resolution{Action: ActContinue, Note: "take 2026"}}})
		}, `to "Which year?": take 2026`},
	} {
		h := newWorkflowHarness(t)
		h.escalationIDs = []int64{5, 9}
		year := SessionResult{Outcome: OutcomeEscalate, SessionID: "s2", Reason: "The agent asks: Which year?\nWhy: none\nRecommendation: 2026"}
		h.onSessions(licenseQuestion, year, pushed("s3", "c3"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "use MIT", Comment: 7})
		h.env.RegisterDelayedCallback(func() { h.rungOne = true }, 30*time.Minute)
		h.signalAfter(90*time.Minute, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "and MIT for the docs too", Comment: 8})
		tc.end(h, h.env.Now())
		h.signalAfter(5*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
		h.run(t)
		if len(h.sessions) != 3 {
			t.Fatalf("%s: sessions %d, want 3", tc.name, len(h.sessions))
		}
		answers := strings.Join(h.sessions[2].Answers, "\n")
		if !strings.Contains(answers, `to "Which license?": and MIT for the docs too`) || !strings.Contains(answers, tc.year) {
			t.Errorf("%s: session 3's answers %q, want the second license reply under the license question and %q", tc.name, answers, tc.year)
		}
		filed := slices.ContainsFunc(h.recorded, func(r recordedStep) bool {
			return r.step == "signal" && fmt.Sprint(r.detail["comment"]) == "8" && fmt.Sprint(r.detail["answers"]) == "5"
		})
		if !filed {
			t.Errorf("%s: timeline %+v, want comment 8 recorded as answering 5", tc.name, h.recorded)
		}
	}
}

// twoHeldStops runs a card whose two questions rung 1 holds (cases 701 and
// 702) with sigs, the issue closed at 5h, and answers its sessions' answers,
// its trips, the cases it ended and its timeline's signal rows.
func twoHeldStops(t *testing.T, sigs func(h *workflowHarness)) (answers []string, summary string) {
	h := newWorkflowHarness(t)
	h.rungOne = true
	year := SessionResult{Outcome: OutcomeEscalate, SessionID: "s2", Reason: "The agent asks: Which year?\nWhy: none\nRecommendation: 2026"}
	h.onSessions(licenseQuestion, year, pushed("s3", "c3"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	sigs(h)
	h.signalAfter(5*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	h.run(t)
	for _, s := range h.sessions {
		answers = append(answers, strings.Join(s.Answers, " | "))
	}
	var rows []string
	for _, r := range h.recorded {
		if r.step == "signal" {
			rows = append(rows, fmt.Sprintf("%v/%v/%v", r.detail["signal"], r.detail["comment"], r.detail["ignored"]))
		}
	}
	return answers, fmt.Sprintf("trips %d ended %q finals %v rows %v", len(h.trips), h.ended, h.finals, rows)
}

// rungOneAnswer is rung 1's continue with note on case id, which carries no
// comment.
func rungOneAnswer(id int64, note string) TaskSignal {
	return TaskSignal{Author: CheckinDecider, Case: &CaseSignal{CaseID: id, Rung: 1, By: CheckinDecider, Res: Resolution{Action: ActContinue, Note: note}}}
}

// Rung 1 answers both of a run's stops, each answer with no comment: the
// next session reads both notes, the second under its own question, not
// dropped as a copy of the first.
func TestWorkflowReadsRungOnesAnswerToEachStop(t *testing.T) {
	answers, _ := twoHeldStops(t, func(h *workflowHarness) {
		h.signalAfter(30*time.Minute, SignalCaseResolved, rungOneAnswer(701, "use MIT"))
		h.signalAfter(2*time.Hour, SignalCaseResolved, rungOneAnswer(702, "take 2026"))
	})
	if len(answers) != 3 || !strings.Contains(answers[2], `to "Which license?": use MIT`) || !strings.Contains(answers[2], `to "Which year?": take 2026`) {
		t.Fatalf("sessions' answers %q, want session 3 to read both of rung 1's answers", answers)
	}
}

// A wait takes a copy of a case_resolved as a no-op: two copies of 701's at
// once inside its wait, a stale copy of 701's while the run waits on 702, and
// a copy of 702's after its wait give the run of one copy.
func TestWorkflowTakesCopiesOfACaseResolvedInsideItsWaits(t *testing.T) {
	base := func(h *workflowHarness) {
		h.signalAfter(30*time.Minute, SignalCaseResolved, rungOneAnswer(701, "use MIT"))
		h.signalAfter(2*time.Hour, SignalCaseResolved, rungOneAnswer(702, "take 2026"))
	}
	oneAnswers, one := twoHeldStops(t, base)
	for name, at := range map[string]struct {
		d  time.Duration
		id int64
	}{"two at once": {30 * time.Minute, 701}, "a stale copy of 701": {90 * time.Minute, 701}, "a copy of 702 after its wait": {150 * time.Minute, 702}} {
		note := map[int64]string{701: "use MIT", 702: "take 2026"}[at.id]
		answers, got := twoHeldStops(t, func(h *workflowHarness) {
			base(h)
			h.signalAfter(at.d, SignalCaseResolved, rungOneAnswer(at.id, note))
		})
		if got != one || !slices.Equal(answers, oneAnswers) {
			t.Errorf("%s: %s, answers %q; want the run of one copy: %s, answers %q", name, got, answers, one, oneAnswers)
		}
	}
}

// A stale copy of an older face of the case the run waits on, which comes
// after its newer face (650), changes nothing: a late comment written
// between the two faces is no answer to the year question rung 1 held, and
// the reply to the newer face is.
func TestWorkflowTakesNoFaceOlderThanItsEscalation(t *testing.T) {
	h := newWorkflowHarness(t)
	h.escalationIDs = []int64{5, 9}
	year := SessionResult{Outcome: OutcomeEscalate, SessionID: "s2", Reason: "The agent asks: Which year?\nWhy: none\nRecommendation: 2026"}
	h.onSessions(licenseQuestion, year, pushed("s3", "c3"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "use MIT", Comment: 7})
	h.env.RegisterDelayedCallback(func() { h.rungOne = true }, 30*time.Minute)
	h.signalAfter(2*time.Hour, SignalCaseFaced, TaskSignal{Author: FactoryDecider, Comment: 650, Case: &CaseSignal{CaseID: 702, Rung: 3, By: FactoryDecider, Comment: 650}})
	h.signalAfter(130*time.Minute, SignalCaseFaced, TaskSignal{Author: FactoryDecider, Comment: 601, Case: &CaseSignal{CaseID: 702, Rung: 3, By: FactoryDecider, Comment: 601}})
	h.signalAfter(150*time.Minute, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "what about the old question", Comment: 620})
	h.signalAfter(3*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "2026", Comment: 660})
	h.signalAfter(5*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	h.run(t)
	if len(h.sessions) != 3 {
		t.Fatalf("sessions %d, want 3", len(h.sessions))
	}
	answers := strings.Join(h.sessions[2].Answers, " | ")
	if !strings.Contains(answers, `to "Which year?": 2026`) || strings.Contains(answers, `to "Which year?": what about the old question`) {
		t.Fatalf("session 3's answers %q, want the reply to the newer face as the year's answer and not the comment between the faces", answers)
	}
}

// A second copy of either case signal, as the heal pass sends one a send
// delivered after all, changes nothing in the run: the steps, the answers the
// next session reads and the reply's place on the timeline are those of one.
func TestWorkflowTakesASecondCopyOfACaseSignalAsNoOp(t *testing.T) {
	run := func(copies int, sig func(h *workflowHarness, at time.Duration)) ([]string, []string, int) {
		h := newWorkflowHarness(t)
		h.taskConfig = askTaskConfig()
		h.rungOne = true
		var steps []string
		h.onVerify(&steps, []SessionResult{
			pushed("s1", "c1"),
			{SessionID: "s2", Outcome: OutcomeCheckinHuman, Commit: "c2", Trip: string(TripNoProgress), Reason: spentStop,
				CheckIn: &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend, Reason: spentStop,
					Record: &CheckInRecord{Moment: CheckInSession, Session: "s2"}}},
			pushed("s3", "c3")})
		h.onReviews("c1")
		for i := range copies {
			sig(h, 30*time.Minute+time.Duration(i)*20*time.Minute)
		}
		h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
		h.run(t)
		shown := 0
		for _, r := range h.recorded {
			if r.step == "signal" && r.detail["signal"] == SignalHumanComment && fmt.Sprint(r.detail["comment"]) == "602" {
				shown++
			}
		}
		var answers []string
		if len(h.sessions) > 2 {
			answers = h.sessions[2].Answers
		}
		return steps, answers, shown
	}
	resolved := func(h *workflowHarness, at time.Duration) {
		h.signalAfter(at, SignalCaseResolved, TaskSignal{Author: CheckinDecider, Case: &CaseSignal{CaseID: 701, Rung: 1, By: CheckinDecider,
			Res: Resolution{Action: ActContinue, Note: "go on"}}})
	}
	faced := func(h *workflowHarness, at time.Duration) {
		h.signalAfter(at, SignalCaseFaced, TaskSignal{Author: FactoryDecider, Comment: 601, Case: &CaseSignal{CaseID: 701, Rung: 3, By: FactoryDecider, Comment: 601}})
		if at == 30*time.Minute {
			h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
		}
	}
	for name, sig := range map[string]func(*workflowHarness, time.Duration){"case_resolved": resolved, "case_faced": faced} {
		oneSteps, oneAnswers, oneShown := run(1, sig)
		twoSteps, twoAnswers, twoShown := run(3, sig)
		if len(oneSteps) < 4 || !slices.Equal(oneSteps, twoSteps) || !slices.Equal(oneAnswers, twoAnswers) || oneShown != twoShown {
			t.Errorf("%s: one copy gives steps %q, answers %q, the reply shown %d; three give %q, %q, %d; want the same, past the stop",
				name, oneSteps, oneAnswers, oneShown, twoSteps, twoAnswers, twoShown)
		}
	}
}

// Rung 1 holds the trip's case and resolves it itself while a comment the
// face never came for waits: the comment is recorded once, as no answer,
// when the wait ends; when the store says the case ended with no face, no
// face comes, so it is recorded when it comes.
func TestWorkflowRecordsACommentKeptForAFaceThatNeverCame(t *testing.T) {
	run := func(stored CaseState) []recordedStep {
		h := newWorkflowHarness(t)
		h.taskConfig = askTaskConfig()
		h.rungOne = true
		h.caseState = func(int64) CaseState { return stored }
		var steps []string
		h.onVerify(&steps, []SessionResult{
			pushed("s1", "c1"),
			{SessionID: "s2", Outcome: OutcomeCheckinHuman, Commit: "c2", Trip: string(TripNoProgress), Reason: spentStop,
				CheckIn: &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend, Reason: spentStop,
					Record: &CheckInRecord{Moment: CheckInSession, Session: "s2"}}},
			pushed("s3", "c3")})
		h.onReviews("c1")
		h.signalAfter(30*time.Minute, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "what is it doing?", Comment: 602})
		h.signalAfter(time.Hour, SignalCaseResolved, TaskSignal{Author: CheckinDecider, Case: &CaseSignal{CaseID: 701, Rung: 1, By: CheckinDecider, Res: Resolution{Action: ActContinue, Note: "go on"}}})
		h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
		start := h.env.Now()
		h.run(t)
		var shown []recordedStep
		for _, r := range h.recorded {
			if r.step == "signal" && fmt.Sprint(r.detail["comment"]) == "602" {
				r.at = time.Time{}.Add(r.at.Sub(start))
				shown = append(shown, r)
			}
		}
		return shown
	}
	for _, tc := range []struct {
		stored CaseState
		at     time.Duration
	}{{CaseState{}, time.Hour}, {CaseState{Ended: true}, 30 * time.Minute}} {
		shown := run(tc.stored)
		if len(shown) != 1 || shown[0].detail["ignored"] != "older than the escalation" || shown[0].at.Sub(time.Time{}) != tc.at {
			t.Errorf("store %+v: comment 602 on the timeline %+v, want it once as no answer at %s", tc.stored, shown, tc.at)
		}
	}
}

// A reply older than the newest question, delivered while rung 1 holds a
// later stop, cannot answer the face: it is filed under the question it
// answers when it comes, with no read of the face from the store.
func TestWorkflowFilesAReplyOlderThanTheNewestQuestionAtOnce(t *testing.T) {
	h := newWorkflowHarness(t)
	h.escalationIDs = []int64{5, 9}
	year := SessionResult{Outcome: OutcomeEscalate, SessionID: "s2", Reason: "The agent asks: Which year?\nWhy: none\nRecommendation: 2026"}
	month := SessionResult{Outcome: OutcomeEscalate, SessionID: "s3", Reason: "The agent asks: Which month?\nWhy: none\nRecommendation: May"}
	h.onSessions(licenseQuestion, year, month)
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "use MIT", Comment: 7})
	h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "2026", Comment: 10})
	h.env.RegisterDelayedCallback(func() { h.rungOne = true }, 90*time.Minute)
	h.signalAfter(3*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "and MIT for the docs too", Comment: 8})
	h.signalAfter(4*time.Hour, SignalCaseFaced, TaskSignal{Author: FactoryDecider, Comment: 11, Case: &CaseSignal{CaseID: 703, Rung: 3, By: FactoryDecider, Comment: 11}})
	h.signalAfter(5*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "May", Comment: 12})
	h.signalAfter(7*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	start := h.env.Now()
	h.run(t)
	i := slices.IndexFunc(h.recorded, func(r recordedStep) bool {
		return r.step == "signal" && fmt.Sprint(r.detail["comment"]) == "8" && fmt.Sprint(r.detail["answers"]) == "5"
	})
	if i < 0 || h.recorded[i].at.Sub(start) != 3*time.Hour || len(h.stateReads) != 0 {
		t.Fatalf("timeline %+v, state reads %v; want comment 8 filed under 5 at 3h, with no read of the face", h.recorded, h.stateReads)
	}
}

// A wait on a case that returns without the case's answer ends the case,
// with what moved the run on: a change request, the pull request's close,
// the switch to the planner. Rung 1 holds the case (701), or the human has
// its face: no case outlives its wait.
func TestWorkflowEndsTheCaseOfAWaitItLeaves(t *testing.T) {
	run := func(held bool, at time.Duration, name string, sig TaskSignal) []string {
		h := newWorkflowHarness(t)
		h.taskConfig = askTaskConfig()
		h.rungOne = held
		var steps []string
		h.onVerify(&steps, []SessionResult{
			pushed("s1", "c1"),
			{SessionID: "s2", Outcome: OutcomeCheckinHuman, Commit: "c2", Trip: string(TripNoProgress), Reason: spentStop,
				CheckIn: &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend, Reason: spentStop,
					Record: &CheckInRecord{Moment: CheckInSession, Session: "s2"}}},
			pushed("s3", "c3")})
		h.onReviews("c1")
		h.signalAfter(at, name, sig)
		h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
		h.run(t)
		return h.ended
	}
	changes := TaskSignal{Author: "sebastian", Body: "rename it", PR: 9}
	for _, held := range []bool{true, false} {
		if got := run(held, time.Hour, SignalChangesRequested, changes); !slices.Equal(got, []string{"701 reviewer sebastian requested changes"}) {
			t.Errorf("held %v, a change request: ended %q, want case 701 for the change request", held, got)
		}
	}
	if got := run(true, time.Hour, SignalPRClosed, TaskSignal{Author: "sebastian", PR: 9}); !slices.Equal(got, []string{"701 the run ended: pull request closed without merge"}) {
		t.Errorf("the pull request's close: ended %q, want case 701 for the run's end", got)
	}

	noPush := SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "no changes to push"}
	p, _ := toPlanHarness(t, becomeSwitched, timedSession{res: noPush})
	p.rungOne = true
	p.state = RunState{Status: "running", Usage: TaskUsage{Attempts: 2}}
	p.toPlanAfter(time.Hour, toPlanTooBig)
	if final := p.run(t); final != "done" {
		t.Fatalf("the switch: finals %v", p.finals)
	}
	if !slices.Equal(p.ended, []string{"701 " + toPlanner}) {
		t.Errorf("the switch: ended %q, want case 701 for the planner", p.ended)
	}
}

// The real gate-beside flow with the ask held by rung 1 (the review's trip
// at 15 minutes, red checks at 20): its face's signal gives the ask its
// comment, so the red checks withdraw that question, and the human's reply
// to the face, in either order with the signal, reaches the next session;
// stopped before any face, the ask has nothing to withdraw and its case
// ends, so rung 1 faces no question that no longer stands.
func TestBesideAskHeldByRungOneThroughTheRealFlow(t *testing.T) {
	run := func(faceAt, replyAt time.Duration) (withdrawn []string, answers []string, ended []string) {
		g := newGateHarnessWith(t, stagesWorkflow(3, withAudit), func(g *gateHarness) { g.reviewDelay, g.auditDelay = 3*time.Hour, 3*time.Hour })
		g.rungOne = true
		g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
		g.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9}, nil)
		g.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", "c1").After(20*time.Minute).Return(ChecksResult{State: ChecksFailure, Findings: []string{"red 1"}}, nil)
		g.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", "c2").After(20*time.Minute).Return(ChecksResult{State: ChecksSuccess}, nil)
		g.onGateSupervisor(GateDecision{Action: ActionEscalate, Diagnosis: "the model hangs", Recommendation: "look at the gateway"})
		g.env.OnActivity(ActivityWithdrawQuestion, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
			func(ctx context.Context, taskID, reason string, comment int64, why string, caseID int64) (int64, error) {
				withdrawn = append(withdrawn, fmt.Sprintf("%d case %d at %s", comment, caseID, g.env.Now().Sub(g.started)))
				return 900, nil
			})
		g.tripAfter(15*time.Minute, ActivityModelReview, 1)
		if faceAt > 0 {
			g.signalAfter(faceAt, SignalCaseFaced, TaskSignal{Author: FactoryDecider, Comment: 640, Case: &CaseSignal{CaseID: 701, Rung: 3, By: FactoryDecider, Comment: 640}})
		}
		if replyAt > 0 {
			g.signalAfter(replyAt, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "gateway restarted", Comment: 700})
		}
		g.signalAfter(10*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		if final := g.run(t); final != "done" {
			t.Fatalf("final = %q, finals %v", final, g.finals)
		}
		if len(g.sessions) > 1 {
			answers = g.sessions[1].Answers
		}
		return withdrawn, answers, g.ended
	}
	for name, tc := range map[string]struct{ face, reply time.Duration }{
		"the face first": {16 * time.Minute, 17 * time.Minute}, "the reply first": {18 * time.Minute, 17 * time.Minute}} {
		w, a, e := run(tc.face, tc.reply)
		if !slices.Equal(w, []string{"640 case 701 at 20m0s"}) || len(a) != 1 || len(e) != 0 {
			t.Errorf("%s: withdrawn %q, answers %q, ended %q; want comment 640 of case 701 withdrawn at 20m, the reply answering, no case ended", name, w, a, e)
		}
	}
	w, a, e := run(0, 0)
	if len(w) != 0 || len(a) != 0 || len(e) != 1 || !strings.HasPrefix(e[0], "701 ") {
		t.Errorf("no face: withdrawn %q, answers %q, ended %q; want nothing withdrawn and case 701 ended", w, a, e)
	}
}

// The run ends while a gate beside the checks has an ask rung 1 holds (the
// review's trip at 15 minutes, the pull request closed at 30, the checks
// still waiting): the run's end closes the questions it asked and ends the
// held ask's case, so rung 1 faces no question on a card no run holds.
func TestTheRunsEndEndsTheCaseOfABesideAskRungOneHolds(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, withAudit), func(g *gateHarness) { g.reviewDelay, g.auditDelay = 3*time.Hour, 3*time.Hour })
	g.rungOne = true
	g.onSessions(pushed("s1", "c1"))
	g.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9}, nil)
	g.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", "c1").After(time.Hour).Return(ChecksResult{State: ChecksSuccess}, nil)
	g.onGateSupervisor(GateDecision{Action: ActionEscalate, Diagnosis: "the model hangs", Recommendation: "look at the gateway"})
	g.tripAfter(15*time.Minute, ActivityModelReview, 1)
	g.signalAfter(30*time.Minute, SignalPRClosed, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "failed" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if !slices.Equal(g.ended, []string{"701 the run ended"}) {
		t.Fatalf("ended %q, want case 701 for the run's end", g.ended)
	}
}

// The console's answer to a face posted after the trip is filed once: the
// answer's case_resolved and the webhook's copy of its comment, in either
// order, reach the next session as one answer.
func TestWorkflowFilesTheConsolesAnswerToALateFaceOnce(t *testing.T) {
	for name, copyAt := range map[string]time.Duration{"the copy after": time.Hour + time.Second, "the copy before": time.Hour - time.Second} {
		t.Run(name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			h.rungOne = true
			h.state = RunState{Status: "running", Usage: TaskUsage{AgentTime: 12 * time.Minute, Attempts: 3}}
			h.onSessions(pushed("s4", "c4"))
			h.onChecks(ChecksResult{State: ChecksSuccess})
			extend := Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}, Note: "one more, then stop"}
			h.signalAfter(30*time.Minute, SignalCaseFaced, TaskSignal{Author: FactoryDecider, Comment: 601, Case: &CaseSignal{CaseID: 701, Rung: 3, By: FactoryDecider, Comment: 601}})
			h.env.RegisterDelayedCallback(func() {
				h.rowUsage.RaisedSessions = 1
				h.env.SignalWorkflow(SignalCaseResolved, caseResolved(701, extend, 650))
			}, time.Hour)
			h.signalAfter(copyAt, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "one more, then stop", Comment: 650})
			h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
			if final := h.run(t); final != "done" {
				t.Fatalf("final = %q, finals %v", final, h.finals)
			}
			if len(h.sessions) != 1 || len(h.sessions[0].Answers) != 1 {
				t.Fatalf("sessions %+v, want one session with the answer once", h.sessions)
			}
		})
	}
}

// A gate beside the checks wait whose ask rung 1 holds has no comment yet:
// stopBeside withdraws nothing for it, and the face's signal gives it the
// comment, after which the withdrawal names it.
func TestStopBesideWithdrawsOnlyAnAskWithItsFaceComment(t *testing.T) {
	run := func(faced bool) []int64 {
		var suite testsuite.WorkflowTestSuite
		env := suite.NewTestWorkflowEnvironment()
		var withdrawn []int64
		env.RegisterActivityWithOptions(func(ctx context.Context, taskID, reason string, comment int64, why string, caseID int64) (int64, error) {
			if caseID != 7 {
				return 0, fmt.Errorf("withdrawal named case %d, want 7", caseID)
			}
			withdrawn = append(withdrawn, comment)
			return 900, nil
		}, activity.RegisterOptions{Name: ActivityWithdrawQuestion})
		env.RegisterWorkflowWithOptions(func(ctx workflow.Context) error {
			short := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
			t := &taskRun{ctx: ctx, short: short, taskID: "t1", caseID: 7}
			f, settle := workflow.NewFuture(ctx)
			settle.SetValue(nil)
			t.beside = []*besideRun{{asked: "the stop", caseID: 7, gateRun: &gateRun{future: f, cancel: func() {}}}}
			_ = workflow.Sleep(ctx, time.Minute)
			runs := t.beside
			t.beside = nil
			t.stopBeside("the head went back", runs...)
			return nil
		}, workflow.RegisterOptions{Name: "beside"})
		if faced {
			env.RegisterDelayedCallback(func() {
				env.SignalWorkflow(SignalCaseFaced, TaskSignal{Case: &CaseSignal{CaseID: 7, Comment: 640}})
			}, 30*time.Second)
		}
		env.ExecuteWorkflow("beside")
		if err := env.GetWorkflowError(); err != nil {
			t.Fatal(err)
		}
		return withdrawn
	}
	if got := run(false); len(got) != 0 {
		t.Fatalf("withdrawn %v for an ask with no comment, want none", got)
	}
	if got := run(true); !slices.Equal(got, []int64{640}) {
		t.Fatalf("withdrawn %v after the face's signal, want comment 640", got)
	}
}

// The earlier findings a statement lists are the 21 groups' most recently
// judged 20, newest first: a finding judged in the first check-in and again
// in the last stays on the list, and the one judged only once, earliest,
// leaves it.
func TestCheckinAskListsTheMostRecentlyJudgedGroups(t *testing.T) {
	var priors []map[string]CheckVerdict
	text := func(i int) string { return fmt.Sprintf("finding number %d", i) }
	for i := 1; i <= 21; i++ {
		priors = append(priors, map[string]CheckVerdict{text(i): {State: CheckConfirmed}})
	}
	priors = append(priors, map[string]CheckVerdict{text(1): {State: CheckConfirmed}})
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(func(ctx workflow.Context) ([]PriorFinding, error) {
		tr := &taskRun{ctx: ctx, checkinVerdicts: priors}
		return tr.checkinAsk("s1", []string{"another"}, []string{"review"}, GateResults{}).Priors, nil
	}, workflow.RegisterOptions{Name: "recent"})
	env.ExecuteWorkflow("recent")
	var got []PriorFinding
	if err := env.GetWorkflowResult(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 20 || got[0].Text != text(1) || got[0].Confirmed != 2 || got[1].Text != text(21) || got[19].Text != text(3) {
		t.Fatalf("priors = %+v, want 20 groups: finding 1 first (judged last), then 21 down to 3; finding 2 left", got)
	}
}
