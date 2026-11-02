package factoryd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// onReviews answers ModelReview with concerns on the heads in concerns and
// an approval on any other.
func (h *workflowHarness) onReviews(concerns ...string) {
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			if slices.Contains(concerns, commit) {
				return Review{Verdict: "concerns", Summary: "one fault", Findings: []string{"rename the flag"}}, nil
			}
			return Review{}, nil
		})
}

// beforeCheckinAnswer runs the workflow as a run from before the
// checkin-answer and verified-head changes, which carries neither marker.
func (h *workflowHarness) beforeCheckinAnswer() {
	h.env.OnGetVersion("checkin-answer", workflow.DefaultVersion, versionCheckinAnswer).Return(workflow.DefaultVersion)
	h.env.OnGetVersion("verified-head", workflow.DefaultVersion, versionVerifiedHead).Return(workflow.DefaultVersion)
}

// A share stop on a card whose pull request is open: Go on grants the
// share its question promises beside the verification of the head, and
// records it before the next session, whose start check-in reads the share
// from the row and runs. The sessions check the share as the runner does
// (checkinBudget): €1.06 spent of a €10 card's €1.00 share. A run from
// before the change verifies alone, and its next session stops at the same
// share before any model call.
func TestCheckinShareStopWithOpenPRGrantsTheShare(t *testing.T) {
	for _, before := range []bool{false, true} {
		h := newWorkflowHarness(t)
		if before {
			h.beforeCheckinAnswer()
		}
		h.taskConfig = askTaskConfig()
		var steps []string
		h.onVerify(&steps, nil)
		var rows []TaskUsage // the row as each session started
		h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
			func(ctx context.Context, in SessionInput) (SessionResult, error) {
				h.sessions = append(h.sessions, in)
				rows = append(rows, h.rowUsage)
				id := fmt.Sprintf("s%d", len(h.sessions))
				steps = append(steps, "session "+id)
				left := policy.Limits{}.CheckinShareOf(10)*float64(1+h.rowUsage.ExtraCheckinShare) - 1.06
				switch {
				case len(h.sessions) == 1:
					return pushed(id, "c1"), nil
				case in.CheckInAsk != nil && left <= 0:
					return checkinStopped(id, OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
						Reason: CheckinShareSpent, Record: &CheckInRecord{Moment: CheckInGate, Session: id}}), nil
				}
				return pushed(id, "c2"), nil
			})
		h.onReviews("c1")
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
		h.signalAfter(24*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

		if final := h.run(t); final != "done" {
			t.Fatalf("before %v: final = %q, escalations = %q", before, final, h.escalated)
		}
		if before {
			want := []string{"session s1", "pr c1 after 0 cards", "session s2", "pr c1 after 1 cards", "session s3"}
			if !slices.Equal(steps, want) || len(h.escalated) != 2 || rows[2].ExtraCheckinShare != 0 {
				t.Errorf("before the change: steps %q, escalations %q, row at s3 %+v; want %q, the stop asked twice, no share granted",
					steps, h.escalated, rows[2], want)
			}
			continue
		}
		want := []string{"session s1", "pr c1 after 0 cards", "session s2", "pr c1 after 1 cards", "session s3", "pr c2 after 1 cards", "approval c2"}
		if !slices.Equal(steps, want) {
			t.Fatalf("steps = %q, want %q: the answer verifies c1 again and the next session runs on the grant", steps, want)
		}
		if len(h.escalated) != 1 || !strings.HasPrefix(h.escalated[0], checkinShareStop) {
			t.Fatalf("escalations = %q, want the one share stop", h.escalated)
		}
		if rows[2].ExtraCheckinShare != 1 || h.sessions[2].CheckInAsk == nil {
			t.Fatalf("s3 started on the row %+v with ask %+v; want the granted share recorded before its start check-in", rows[2], h.sessions[2].CheckInAsk)
		}
	}
}

// A restarted run whose card waits at a share stop with the pull request
// open: the answer grants the share and records it, beside the
// verification of the head, which buys no session. A run from before the
// change verifies alone.
func TestCheckinShareResumeWithOpenPRGrantsTheShare(t *testing.T) {
	for _, before := range []bool{false, true} {
		h := newWorkflowHarness(t)
		if before {
			h.beforeCheckinAnswer()
		}
		h.state = RunState{Status: "waiting-human", Question: checkinShareStop, PR: PullRequest{Number: 9}, Head: "c2",
			Usage: TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}}
		var steps []string
		h.onVerify(&steps, []SessionResult{pushed("s2", "c3")})
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

		if final := h.run(t); final != "done" {
			t.Fatalf("before %v: final = %q, finals = %v", before, final, h.finals)
		}
		if want := []string{"pr c2 after 1 cards", "approval c2"}; !slices.Equal(steps, want) || len(h.sessions) != 0 {
			t.Fatalf("before %v: steps %q, sessions %d; want %q and no session", before, steps, len(h.sessions), want)
		}
		want := []TaskUsage{{AgentTime: 5 * time.Minute, Attempts: 1, ExtraCheckinShare: 1}}
		if before {
			want = nil
		}
		if !slices.Equal(h.usage, want) {
			t.Fatalf("before %v: usage = %+v, want %+v", before, h.usage, want)
		}
	}
}

// A session that pushed c2 and then stopped at its mid-session check-in's
// share: the answer verifies c2, the session's own push, not c1, the head
// it started from, so the policy gate meets no moved head and no head hold
// is spent, and grants the share, so the next stop is asked as a stop of
// its own. A run from before the change verifies c1, holds the moved head
// with a branch read, and its next stop states that no reply can change
// the head c1.
func TestCheckinAnswerVerifiesTheSessionsOwnPush(t *testing.T) {
	for _, before := range []bool{false, true} {
		h := newWorkflowHarness(t)
		if before {
			h.beforeCheckinAnswer()
		}
		h.taskConfig = askTaskConfig()
		h.branchHead = "c2"
		var gated []string
		h.env.OnActivity(ActivityGatePolicy, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(
			func(ctx context.Context, taskID string, pr int64, commit string, pin int64) (PolicyGate, error) {
				gated = append(gated, commit)
				if commit == "c1" && len(gated) > 1 {
					return PolicyGate{}, headMovedError("pull request #9's head moved to c2 after its checks passed on c1")
				}
				return PolicyGate{}, nil
			})
		var steps []string
		h.onVerify(&steps, []SessionResult{
			pushed("s1", "c1"),
			{SessionID: "s2", Outcome: OutcomeCheckinHuman, Commit: "c2", Trip: string(TripNoProgress), Reason: CheckinShareSpent,
				CheckIn: &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend, Reason: CheckinShareSpent,
					Record: &CheckInRecord{Moment: CheckInSession, Session: "s2"}}},
			checkinStopped("s3", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
				Reason: checkinCallCapStop, Record: &CheckInRecord{Moment: CheckInGate, Session: "s3"}})})
		h.onReviews("c1", "c2")
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
		h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

		if final := h.run(t); final != "closed" {
			t.Fatalf("before %v: final = %q, escalations = %q", before, final, h.escalated)
		}
		if len(h.escalated) != 2 {
			t.Fatalf("before %v: escalations = %q, want the share stop and the next stop", before, h.escalated)
		}
		if before {
			want := []string{"session 1 after 0 cards", "pr c1 after 0 cards", "session 2 after 0 cards", "pr c1 after 1 cards", "session 3 after 1 cards"}
			if !slices.Equal(steps, want) || !slices.Equal(gated, []string{"c1", "c1", "c2"}) || h.branchReads != 1 ||
				!strings.Contains(h.escalated[1], deadlockAskMark+": the head c1") {
				t.Errorf("before the change: steps %q, gated %q, branch reads %d, repeat %q; want %q, c1 gated again, one hold, the stale head named",
					steps, gated, h.branchReads, h.escalated[1], want)
			}
			continue
		}
		want := []string{"session 1 after 0 cards", "pr c1 after 0 cards", "session 2 after 0 cards", "pr c2 after 1 cards", "session 3 after 1 cards"}
		if !slices.Equal(steps, want) {
			t.Fatalf("steps = %q, want %q: the answer verifies the session's push", steps, want)
		}
		if !slices.Equal(gated, []string{"c1", "c2"}) || h.branchReads != 0 {
			t.Fatalf("gated %q, branch reads %d; want c1 then c2 and no head hold", gated, h.branchReads)
		}
		if !strings.HasPrefix(h.escalated[1], checkinSpendAsk+checkinCallCapStop) {
			t.Fatalf("the next stop %q, want the call cap's own ask", h.escalated[1])
		}
		if u := h.usage[len(h.usage)-1]; u.ExtraCheckinShare != 1 {
			t.Fatalf("usage = %+v, want the share granted", u)
		}
	}
}

// A session that pushed c2 and then stopped at a check-in that judged a
// goal conflict: the card goes on from c2, the session's own push, so the
// next session starts from it and the same stop repeated there names c2.
// A run from before the change goes on from c1, the head the session
// started from, and names c1 though the branch holds c2.
func TestCheckinRepeatNamesTheBranchHead(t *testing.T) {
	for _, before := range []bool{false, true} {
		h := newWorkflowHarness(t)
		if before {
			h.beforeCheckinAnswer()
		}
		h.taskConfig = askTaskConfig()
		conflict := &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckGoalConflict, Reason: "the objective contradicts the review"}
		var steps []string
		h.onVerify(&steps, []SessionResult{
			pushed("s1", "c1"),
			{SessionID: "s2", Outcome: OutcomeCheckinHuman, Commit: "c2", Trip: string(TripNoProgress), Reason: conflict.Reason, CheckIn: conflict},
			checkinStopped("s3", OutcomeCheckinHuman, conflict)})
		h.onReviews("c1")
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
		h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

		if final := h.run(t); final != "closed" {
			t.Fatalf("before %v: final = %q, escalations = %q", before, final, h.escalated)
		}
		head := "c2"
		if before {
			head = "c1"
		}
		if len(h.sessions) != 3 || h.sessions[2].StartRef != head || len(h.escalated) != 2 ||
			!strings.Contains(h.escalated[1], deadlockAskMark+": the head "+head) {
			t.Fatalf("before %v: s3 starts from %q, escalations %q; want %s named", before, h.sessions[2].StartRef, h.escalated, head)
		}
	}
}

// A check-in stop repeated on the same head says no reply can change it
// only while the card's limits are unchanged: a raise of the budget, which
// widens the share in euros, or of the sessions, is a change, and the
// repeat is asked as a stop of its own. A run from before the change
// compares the granted shares and the line raises alone.
func TestCheckinRepeatGuardSeesARaise(t *testing.T) {
	for name, tc := range map[string]struct {
		share    float64 // the second stop's share in euros
		sessions int     // the sessions an Extend raised while the first stop waited
		before   bool
		deadlock bool
	}{
		"unchanged":                          {1, 0, false, true},
		"budget raised":                      {3, 0, false, false},
		"sessions raised":                    {1, 2, false, false},
		"budget raised, before the change":   {3, 0, true, true},
		"sessions raised, before the change": {1, 2, true, true},
	} {
		h := newWorkflowHarness(t)
		if tc.before {
			h.beforeCheckinAnswer()
		}
		h.taskConfig = askTaskConfig()
		stop := func(id string, share float64) SessionResult {
			return checkinStopped(id, OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckGoalConflict,
				Reason: "the objective contradicts the review", Share: share, Record: &CheckInRecord{Moment: CheckInGate, Session: id}})
		}
		var steps []string
		h.onVerify(&steps, []SessionResult{pushed("s1", "c1"), stop("s2", 1), stop("s3", tc.share)})
		h.onReviews("c1")
		h.env.OnActivity(ActivityReadUsage, mock.Anything, "t1").Return(
			func(ctx context.Context, taskID string) (TaskUsage, error) {
				u := h.rowUsage
				if len(h.escalated) > 0 {
					u.RaisedSessions = tc.sessions
				}
				return u, nil
			})
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
		h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

		if final := h.run(t); final != "closed" {
			t.Fatalf("%s: final = %q, escalations = %q", name, final, h.escalated)
		}
		if len(h.escalated) != 2 {
			t.Fatalf("%s: escalations = %q, want the stop and its repeat", name, h.escalated)
		}
		if got := strings.Contains(h.escalated[1], deadlockAskMark); got != tc.deadlock {
			t.Errorf("%s: the repeat %q says no reply can change it: %v, want %v", name, h.escalated[1], got, tc.deadlock)
		}
	}
}

// verify returns the head its stages ended on, and each caller keeps it:
// c2, a head the agent moved past the gates and the policy stage held, is
// the next session's start head, not c1, the head the verification began
// on. The loop's own verify, a restarted run's verify of its open pull
// request and a restarted gate stop's answer each take it. A run from
// before the change keeps c1.
func TestVerifyReturnsTheAdoptedHead(t *testing.T) {
	gateStop := "The gate supervisor stopped ModelReview (attempt 1) past its bound of 10m0s: hangs"
	for _, caller := range []string{"loop", "reconcile", "resumed"} {
		for _, before := range []bool{false, true} {
			name := fmt.Sprintf("%s, before %v", caller, before)
			h := newWorkflowHarness(t)
			if before {
				h.beforeCheckinAnswer()
			}
			results := []SessionResult{pushed("s2", "c3")}
			switch caller {
			case "loop":
				results = append([]SessionResult{pushed("s1", "c1")}, results...)
			case "reconcile":
				h.state = RunState{Status: "in-review", PR: PullRequest{Number: 9}, Head: "c1", Usage: TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}}
			case "resumed":
				h.state = RunState{Status: "waiting-human", Question: gateStop, PR: PullRequest{Number: 9}, Head: "c1",
					Usage: TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}}
				h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the gateway is back", Comment: 602})
			}
			h.branchHead = "c2"
			h.env.OnActivity(ActivityGatePolicy, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(
				func(ctx context.Context, taskID string, pr int64, commit string, pin int64) (PolicyGate, error) {
					if commit == "c1" {
						return PolicyGate{}, headMovedError("pull request #9's head moved to c2 after its checks passed on c1")
					}
					return PolicyGate{}, nil
				})
			var steps []string
			h.onVerify(&steps, results)
			h.onReviews("c2")
			h.signalAfter(24*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

			if final := h.run(t); final != "done" {
				t.Fatalf("%s: final = %q, finals = %v", name, final, h.finals)
			}
			want := "c2"
			if before {
				want = "c1"
			}
			if last := h.sessions[len(h.sessions)-1]; last.StartRef != want {
				t.Errorf("%s: the session after the hold starts from %q, want %q", name, last.StartRef, want)
			}
		}
	}
}
