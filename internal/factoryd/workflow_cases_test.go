package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/activity"
	client "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/proto"

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
	if len(g.trips) != 2 || !strings.HasPrefix(g.trips[0].Reason, capReached) {
		t.Fatalf("trips = %+v, want the session cap's and the review stop's", g.trips)
	}
	want := []string{`sebastian, to "` + capReached + `": one more, then stop`}
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
	if len(g.escalated) != 1 || !strings.HasPrefix(g.escalated[0], capReached) || !strings.Contains(g.escalated[0], finding) {
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

// A run recorded before the case-verdicts change, whose trip's case is
// answered with a resolution that carries verdicts (spliced in for the
// issue's close), replays at today's code: the recording holds no marker,
// so the verdicts are not taken and the run goes on to the wait for the
// close a resolution of close makes. The history ends with the completed
// task that holds the signal, which the replayer runs.
func TestWorkflowReplaysACaseResolvedRecordedBeforeTheCaseVerdicts(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "devserver-small-card-agent-time-cap.json"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := client.HistoryFromJSON(bytes.NewReader(data), client.HistoryJSONOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var caseID int64
	at := -1
	for i, e := range base.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && (string(id[0].GetData()) == `"case-verdicts"` || string(id[0].GetData()) == `"case-faced"`) {
			t.Fatalf("the recording holds the marker %s: it is no run from before", id[0].GetData())
		}
		if c := e.GetActivityTaskCompletedEventAttributes(); c != nil && caseID == 0 {
			var out TripOutcome
			if converter.GetDefaultDataConverter().FromPayloads(c.GetResult(), &out) == nil && out.CaseID != 0 {
				caseID = out.CaseID
			}
		}
		if e.GetWorkflowExecutionSignaledEventAttributes().GetSignalName() == SignalIssueClosed {
			at = i
		}
	}
	if caseID == 0 || at < 0 {
		t.Fatalf("the recording holds case %d and the close at %d, want both", caseID, at)
	}
	payloads, err := converter.GetDefaultDataConverter().ToPayloads(TaskSignal{Author: CheckinDecider, Case: &CaseSignal{CaseID: caseID, Rung: 1, By: CheckinDecider,
		Res: Resolution{Action: ActClose, Verdicts: map[string]CheckVerdict{"TasksByPR is undefined": {State: CheckDropped, Evidence: "tasks.go:12"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	// the close's signal becomes the resolution, and the history ends with
	// the workflow task that takes it, completed: the replayer runs that
	// task, which takes the resolution and waits for the close
	events := append([]*historypb.HistoryEvent{}, base.Events[:at+4]...)
	sig := events[at].GetWorkflowExecutionSignaledEventAttributes()
	sig.SignalName = SignalCaseResolved
	sig.Input = payloads
	// the run reads the row's raises before it takes a resolution: the
	// recorded ReadUsage and its workflow task, copied to follow the signal's
	read := -1
	for i, e := range base.Events {
		if e.GetActivityTaskScheduledEventAttributes().GetActivityType().GetName() == ActivityReadUsage {
			read = i
			break
		}
	}
	if read < 0 {
		t.Fatal("the recording holds no ReadUsage")
	}
	next := int64(at + 5)
	for k, e := range base.Events[read : read+6] {
		e = proto.Clone(e).(*historypb.HistoryEvent)
		id := next + int64(k)
		e.EventId = id
		switch {
		case e.GetActivityTaskScheduledEventAttributes() != nil:
			a := e.GetActivityTaskScheduledEventAttributes()
			a.ActivityId, a.WorkflowTaskCompletedEventId = strconv.FormatInt(id, 10), int64(at+4)
		case e.GetActivityTaskStartedEventAttributes() != nil:
			e.GetActivityTaskStartedEventAttributes().ScheduledEventId = next
		case e.GetActivityTaskCompletedEventAttributes() != nil:
			a := e.GetActivityTaskCompletedEventAttributes()
			a.ScheduledEventId, a.StartedEventId = next, next+1
		case e.GetWorkflowTaskStartedEventAttributes() != nil:
			e.GetWorkflowTaskStartedEventAttributes().ScheduledEventId = next + 3
		case e.GetWorkflowTaskCompletedEventAttributes() != nil:
			a := e.GetWorkflowTaskCompletedEventAttributes()
			a.ScheduledEventId, a.StartedEventId = next+3, next+4
		}
		events = append(events, e)
	}
	replaySplice(t, events)
}

// A run recorded before the case-faced change waits on its case: a
// case_faced signal for that case, with a comment above the trip's, is
// spliced into the recorded wait, in a workflow task of its own that holds
// the change's marker and its search-attribute upsert, as a live receipt
// writes them, and the recorded close follows. The replay takes the signal
// where the wait reads it and calls GetVersion there: a wait that read the
// signal without it, or called it at the start of the wait, leaves the
// recorded marker with no command to match and fails the replay.
func TestWorkflowReplaysACaseFacedSplicedIntoAPreChangeWait(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "devserver-small-card-agent-time-cap.json"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := client.HistoryFromJSON(bytes.NewReader(data), client.HistoryJSONOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var caseID, tripComment int64
	at, versions := -1, -1
	for i, e := range base.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"case-faced"` {
			t.Fatal("the recording holds the case-faced marker: it is no run from before")
		}
		if c := e.GetActivityTaskCompletedEventAttributes(); c != nil && caseID == 0 {
			var out TripOutcome
			if converter.GetDefaultDataConverter().FromPayloads(c.GetResult(), &out) == nil && out.CaseID != 0 {
				caseID, tripComment = out.CaseID, out.Comment
			}
		}
		if e.GetUpsertWorkflowSearchAttributesEventAttributes() != nil {
			versions = i
		}
		if e.GetWorkflowExecutionSignaledEventAttributes().GetSignalName() == SignalIssueClosed {
			at = i
		}
	}
	if caseID == 0 || tripComment == 0 || at < 0 || versions < 0 {
		t.Fatalf("the recording holds case %d (comment %d), the close at %d and its versions at %d, want all", caseID, tripComment, at, versions)
	}
	var seen []string
	attr := base.Events[versions].GetUpsertWorkflowSearchAttributesEventAttributes().GetSearchAttributes().GetIndexedFields()["TemporalChangeVersion"]
	if err := json.Unmarshal(attr.GetData(), &seen); err != nil {
		t.Fatal(err)
	}
	seen = append(seen, "case-faced-1")
	slices.Sort(seen)
	seenData, _ := json.Marshal(seen)

	payloads, err := converter.GetDefaultDataConverter().ToPayloads(TaskSignal{Author: FactoryDecider, Comment: tripComment + 99,
		Case: &CaseSignal{CaseID: caseID, Rung: 3, By: FactoryDecider, Comment: tripComment + 99}})
	if err != nil {
		t.Fatal(err)
	}
	faced := proto.Clone(base.Events[at]).(*historypb.HistoryEvent)
	faced.GetWorkflowExecutionSignaledEventAttributes().SignalName = SignalCaseFaced
	faced.GetWorkflowExecutionSignaledEventAttributes().Input = payloads
	first := int64(at + 1) // the id the signal takes
	events := append([]*historypb.HistoryEvent{}, base.Events[:at]...)
	events = append(events, faced)
	for _, e := range base.Events[at+1 : at+4] {
		events = append(events, proto.Clone(e).(*historypb.HistoryEvent))
	}
	completed := first + 3
	version := proto.Clone(base.Events[5]).(*historypb.HistoryEvent)
	ma := version.GetMarkerRecordedEventAttributes()
	ma.Details["change-id"].Payloads[0].Data = []byte(`"case-faced"`)
	ma.WorkflowTaskCompletedEventId = completed
	upsert := proto.Clone(base.Events[6]).(*historypb.HistoryEvent)
	ua := upsert.GetUpsertWorkflowSearchAttributesEventAttributes()
	ua.WorkflowTaskCompletedEventId = completed
	ua.SearchAttributes.IndexedFields["TemporalChangeVersion"].Data = seenData
	events = append(events, version, upsert)
	const inserted = 6
	for _, e := range base.Events[at:] {
		e = proto.Clone(e).(*historypb.HistoryEvent)
		old := strconv.FormatInt(e.EventId, 10)
		shiftEventIDs(e, first, inserted)
		if a := e.GetActivityTaskScheduledEventAttributes(); a != nil && a.ActivityId == old {
			a.ActivityId = strconv.FormatInt(e.EventId+inserted, 10)
		}
		events = append(events, e)
	}
	for i, e := range events {
		e.EventId = int64(i + 1)
	}
	var got []workflow.Version
	replaySplice(t, events, &versionsSeen{change: "case-faced", got: &got})
	if len(got) == 0 || got[len(got)-1] != versionCaseFaced {
		t.Fatalf("the replay's GetVersion of case-faced answered %v, want the recorded marker's version %d where the signal is taken", got, versionCaseFaced)
	}
}

// A run recorded before the case-held change waits on its case at the cap
// and its issue closes: the replay reaches the wait's end, where the run now
// ends the case of a wait it leaves, reads the change there and answers
// DefaultVersion, so it schedules no EndCase the recording lacks.
func TestWorkflowReplaysACaseWaitThatEndedBeforeTheCaseHeldChange(t *testing.T) {
	hist := loadHistory(t, filepath.Join("testdata", "devserver-small-card-agent-time-cap.json"))
	closed := false
	for _, e := range hist.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"case-held"` {
			t.Fatal("the recording holds the case-held marker: it is no run from before")
		}
		if a := e.GetActivityTaskScheduledEventAttributes(); a != nil && a.ActivityType.GetName() == ActivityEndCase {
			t.Fatal("the recording ends a case: the run's own end of its case would replay unseen")
		}
		closed = closed || e.GetWorkflowExecutionSignaledEventAttributes().GetSignalName() == SignalIssueClosed
	}
	if !closed {
		t.Fatal("the recording's wait ends with no issue close")
	}
	var got []workflow.Version
	replaySplice(t, hist.Events, &versionsSeen{change: "case-held", got: &got})
	if !slices.Equal(got, []workflow.Version{workflow.DefaultVersion}) {
		t.Fatalf("the replay's GetVersion of case-held answered %v, want DefaultVersion once, at the wait's end", got)
	}
}

// A plan run recorded before the plan-case-held change asks at the session
// cap, its trip faced at once, and a writer's comment ends the wait: the
// replay reaches the wait's end, where the loop now ends the case of a wait
// that returned with no answer of it, reads the change there and answers
// DefaultVersion, so it schedules no EndCase the recording lacks.
func TestWorkflowReplaysAPlanWaitThatEndedBeforeThePlanCaseHeldChange(t *testing.T) {
	hist := loadHistory(t, filepath.Join("testdata", "devserver-plan-faced-trip-writer-comment.json"))
	var activities, signals []string
	for _, e := range hist.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"plan-case-held"` {
			t.Fatal("the recording holds the plan-case-held marker: it is no run from before")
		}
		if a := e.GetActivityTaskScheduledEventAttributes(); a != nil {
			activities = append(activities, a.ActivityType.GetName())
		}
		if s := e.GetWorkflowExecutionSignaledEventAttributes(); s != nil {
			signals = append(signals, s.GetSignalName())
		}
	}
	if !slices.Contains(activities, ActivityOpenTrip) || !slices.Contains(activities, ActivityRunPlan) || slices.Contains(activities, ActivityEndCase) ||
		!slices.Equal(signals, []string{SignalHumanComment}) {
		t.Fatalf("activities %q, signals %q; want a plan run's trip, a writer's comment and no EndCase", activities, signals)
	}
	var got []workflow.Version
	replaySplice(t, hist.Events, &versionsSeen{change: "plan-case-held", got: &got})
	if !slices.Equal(got, []workflow.Version{workflow.DefaultVersion}) {
		t.Fatalf("the replay's GetVersion of plan-case-held answered %v, want DefaultVersion once, at the wait's end", got)
	}
}

// The plan loop's wait at the session cap ends its case when it returns with
// no answer of it: a writer's comment while rung 1 holds the case or the
// human has its face, and the issue's close. Rung 1's own answer ends
// nothing.
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
}

// versionsSeen records what GetVersion answers for one change id during a
// replay.
type versionsSeen struct {
	interceptor.WorkerInterceptorBase
	change string
	got    *[]workflow.Version
}

func (s versionsSeen) InterceptWorkflow(_ workflow.Context, next interceptor.WorkflowInboundInterceptor) interceptor.WorkflowInboundInterceptor {
	i := &versionsInbound{seen: s}
	i.Next = next
	return i
}

type versionsInbound struct {
	interceptor.WorkflowInboundInterceptorBase
	seen versionsSeen
}

func (i *versionsInbound) Init(out interceptor.WorkflowOutboundInterceptor) error {
	o := &versionsOutbound{seen: i.seen}
	o.Next = out
	return i.Next.Init(o)
}

type versionsOutbound struct {
	interceptor.WorkflowOutboundInterceptorBase
	seen versionsSeen
}

func (o *versionsOutbound) GetVersion(ctx workflow.Context, changeID string, minSupported, maxSupported workflow.Version) workflow.Version {
	v := o.Next.GetVersion(ctx, changeID, minSupported, maxSupported)
	if changeID == o.seen.change {
		*o.seen.got = append(*o.seen.got, v)
	}
	return v
}

// shiftEventIDs moves the references of e's attributes to events from id
// from on up by by, after events were spliced in before them.
func shiftEventIDs(e *historypb.HistoryEvent, from, by int64) {
	attrs := reflect.ValueOf(e).Elem().FieldByName("Attributes")
	if !attrs.IsValid() || attrs.IsNil() {
		return
	}
	body := attrs.Elem().Elem().Field(0).Elem()
	for i := range body.NumField() {
		f := body.Field(i)
		if f.Kind() == reflect.Int64 && strings.HasSuffix(body.Type().Field(i).Name, "EventId") && f.Int() >= from {
			f.SetInt(f.Int() + by)
		}
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
			{SessionID: "s2", Outcome: OutcomeCheckinHuman, Commit: "c2", Trip: string(TripNoProgress), Reason: CheckinShareSpent,
				CheckIn: &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend, Reason: CheckinShareSpent,
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
			{SessionID: "s2", Outcome: OutcomeCheckinHuman, Commit: "c2", Trip: string(TripNoProgress), Reason: CheckinShareSpent,
				CheckIn: &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend, Reason: CheckinShareSpent,
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
			{SessionID: "s2", Outcome: OutcomeCheckinHuman, Commit: "c2", Trip: string(TripNoProgress), Reason: CheckinShareSpent,
				CheckIn: &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend, Reason: CheckinShareSpent,
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
			{SessionID: "s2", Outcome: OutcomeCheckinHuman, Commit: "c2", Trip: string(TripNoProgress), Reason: CheckinShareSpent,
				CheckIn: &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend, Reason: CheckinShareSpent,
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
		g.env.OnActivity(ActivityWithdrawQuestion, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(
			func(ctx context.Context, taskID, reason string, comment int64, why string) (int64, error) {
				withdrawn = append(withdrawn, fmt.Sprintf("%d at %s", comment, g.env.Now().Sub(g.started)))
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
		if !slices.Equal(w, []string{"640 at 20m0s"}) || len(a) != 1 || len(e) != 0 {
			t.Errorf("%s: withdrawn %q, answers %q, ended %q; want comment 640 withdrawn at 20m, the reply answering, no case ended", name, w, a, e)
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
		env.RegisterActivityWithOptions(func(ctx context.Context, taskID, reason string, comment int64, why string) (int64, error) {
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
