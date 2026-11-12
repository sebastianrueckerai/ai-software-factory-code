package factoryd

import (
	"context"
	"encoding/json"
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

// A session that pushed c2 and then stopped at a check-in that judged a
// goal conflict: the card goes on from c2, the session's own push, so the
// next session starts from it and the same stop repeated there names c2.
func TestCheckinRepeatNamesTheBranchHead(t *testing.T) {
	h := newWorkflowHarness(t)
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
		t.Fatalf("final = %q, escalations = %q", final, h.escalated)
	}
	head := "c2"
	if len(h.sessions) != 3 || h.sessions[2].StartRef != head || len(h.escalated) != 2 ||
		!strings.Contains(h.escalated[1], deadlockAskMark+": the head "+head) {
		t.Fatalf("s3 starts from %q, escalations %q; want %s named", h.sessions[2].StartRef, h.escalated, head)
	}
}

// A run restarted between two equal check-in stops still forms the deadlock
// ask at the second: the first stop's mark rides its trip into its case's
// facts, and the new run starts from the mark the card's cases hold, so
// the repeat on the unchanged head c1 and limits names the exits. A first
// run from before the change records no mark, and its successor asks the
// plain stop again.
func TestARestartedRunFormsTheDeadlockAskAtTheSecondEqualStop(t *testing.T) {
	conflict := &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckGoalConflict, Reason: "the objective contradicts the review"}
	for name, before := range map[string]bool{"after the change": false, "before the change": true} {
		t.Run(name, func(t *testing.T) {
			first := newWorkflowHarness(t)
			if before {
				first.env.OnGetVersion("checkin-mark", workflow.DefaultVersion, versionCheckinMark).Return(workflow.DefaultVersion)
			}
			first.taskConfig = askTaskConfig()
			var steps []string
			first.onVerify(&steps, []SessionResult{pushed("s1", "c1"), checkinStopped("s2", OutcomeCheckinHuman, conflict)})
			first.onReviews("c1")
			first.signalAfter(time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
			if final := first.run(t); final != "closed" || len(first.trips) != 1 {
				t.Fatalf("the first run = %q with trips %+v, want closed at its one stop", final, first.trips)
			}
			// the case keeps the trip's facts as JSON: the store's copy
			b, err := json.Marshal(first.trips[0].Extra)
			if err != nil {
				t.Fatal(err)
			}
			var facts map[string]any
			if err := json.Unmarshal(b, &facts); err != nil {
				t.Fatal(err)
			}
			mark := checkinMarkOf([]Case{{ID: 3, Facts: map[string]any{"head": "c0"}}, {ID: 4, Facts: facts}, {ID: 5, Facts: map[string]any{"head": "c1"}}})
			if before != (mark == nil) {
				t.Fatalf("the stored mark = %+v, want one only after the change", mark)
			}

			second := newWorkflowHarness(t)
			second.taskConfig = askTaskConfig()
			second.state = RunState{Status: "waiting-human", Question: first.escalated[0], PR: PullRequest{Number: 9}, Head: "c1",
				Usage: first.rowUsage, CheckinMark: mark}
			second.onVerify(&steps, []SessionResult{checkinStopped("s3", OutcomeCheckinHuman, conflict)})
			second.onReviews("c1")
			second.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
			second.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
			if final := second.run(t); final != "closed" || len(second.sessions) != 1 || len(second.escalated) == 0 {
				t.Fatalf("the restarted run = %q, sessions %d, escalations %q", final, len(second.sessions), second.escalated)
			}
			last := second.escalated[len(second.escalated)-1]
			if deadlock := strings.Contains(last, deadlockAskMark+": the head c1"); deadlock == before {
				t.Fatalf("the second stop asks %q; want the deadlock ask only after the change", last)
			}
		})
	}
}

// The kept mark of a call cap's stop holds Granted once an answer the run
// went on from resolved it: a grant, not a re-verify, nor an answer still
// to come. A run restarted from running after such a grant asks the
// repeat on c1 as the run that granted it would: the restart produced no
// new commit.
func TestARestartAfterACallCapGrantNamesTheUnchangedCommit(t *testing.T) {
	mark := map[string]any{"Kind": CheckSpend, "Tip": "c1", "ExtraLines": 0, "RaisedSessions": 0}
	kept := func(status, action string) *checkinStopMark {
		c := Case{ID: 4, Status: status, Facts: map[string]any{factCheckinMark: mark, "checkin_stop": CheckinStopCallCap}}
		if action != "" {
			c.Resolution = &CaseResolution{By: "sebastian", Rung: 3, Action: action}
		}
		return checkinMarkOf([]Case{c})
	}
	for name, tc := range map[string]struct {
		status  string
		action  string
		granted bool
	}{
		"a grant":       {CaseResolved, ActExtend, true},
		"a continue":    {CaseResolved, ActContinue, true},
		"a re-verify":   {CaseResolved, ActReverify, false},
		"no answer yet": {CaseAtHuman, "", false},
	} {
		if got := kept(tc.status, tc.action); got == nil || got.Granted != tc.granted {
			t.Errorf("%s: the kept mark = %+v, want Granted %v", name, got, tc.granted)
		}
	}
	if got := checkinMarkOf([]Case{{ID: 4, Status: CaseResolved, Resolution: &CaseResolution{Action: ActExtend}, Facts: map[string]any{factCheckinMark: mark}}}); got == nil || got.Granted {
		t.Errorf("a stop of no cap: the kept mark = %+v, want no Granted", got)
	}

	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	h.state = RunState{Status: "running", PR: PullRequest{Number: 9}, Head: "c1", Usage: TaskUsage{AgentTime: 5 * time.Minute, Attempts: 2},
		CheckinMark: kept(CaseResolved, ActExtend)}
	var steps []string
	h.onVerify(&steps, []SessionResult{checkinStopped("s3", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
		Reason: checkinCallCapStop, Record: &CheckInRecord{Moment: CheckInGate, Session: "s3", ParentSession: "s2"}})})
	h.onReviews("c1")
	h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	if final := h.run(t); final != "closed" || len(h.escalated) != 1 {
		t.Fatalf("final = %q, escalations %q; want closed after the repeat's ask", final, h.escalated)
	}
	for _, want := range []string{"no new commit", "the head is still c1"} {
		if !strings.Contains(h.escalated[0], want) {
			t.Errorf("the repeat ask %q misses %q", h.escalated[0], want)
		}
	}
}

// A check-in stop repeated on the same head says no reply can change it
// only while the card's limits are unchanged: a raise of the sessions is a
// change, and the repeat is asked as a stop of its own.
func TestCheckinRepeatGuardSeesARaise(t *testing.T) {
	for name, tc := range map[string]struct {
		sessions int // the sessions an Extend raised while the first stop waited
		deadlock bool
	}{
		"unchanged":       {0, true},
		"sessions raised": {2, false},
	} {
		h := newWorkflowHarness(t)
		h.taskConfig = askTaskConfig()
		stop := func(id string) SessionResult {
			return checkinStopped(id, OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckGoalConflict,
				Reason: "the objective contradicts the review", Record: &CheckInRecord{Moment: CheckInGate, Session: id}})
		}
		var steps []string
		h.onVerify(&steps, []SessionResult{pushed("s1", "c1"), stop("s2"), stop("s3")})
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
// request and a restarted gate stop's answer each take it.
func TestVerifyReturnsTheAdoptedHead(t *testing.T) {
	gateStop := "The gate supervisor stopped ModelReview (attempt 1) past its bound of 10m0s: hangs"
	for _, caller := range []string{"loop", "reconcile", "resumed"} {
		h := newWorkflowHarness(t)
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
			t.Fatalf("%s: final = %q, finals = %v", caller, final, h.finals)
		}
		if last := h.sessions[len(h.sessions)-1]; last.StartRef != "c2" {
			t.Errorf("%s: the session after the hold starts from %q, want c2", caller, last.StartRef)
		}
	}
}

// A cap stop whose reason names the cap the check-in ran with is the call
// cap's or the time cap's stop to its answer, as one that named none: the
// answer buys the session, with the pull request open too, and verifies
// no head.
func TestCapStopsNamingTheirCapBuyTheSession(t *testing.T) {
	for _, reason := range []string{"check-in call cap reached after 20 calls", "check-in time cap reached after 4m0s"} {
		h := newWorkflowHarness(t)
		h.taskConfig = askTaskConfig()
		var steps []string
		h.onVerify(&steps, []SessionResult{
			pushed("s1", "c1"),
			checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend, Reason: reason,
				Record: &CheckInRecord{Moment: CheckInGate, Session: "s2"}}),
			pushed("s3", "c2")})
		h.onReviews("c1")
		h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "one more check-in round", Comment: 602})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

		if final := h.run(t); final != "done" {
			t.Fatalf("%s: final = %q, finals = %v", reason, final, h.finals)
		}
		want := []string{"session 1 after 0 cards", "pr c1 after 0 cards", "session 2 after 0 cards",
			"session 3 after 1 cards", "pr c2 after 1 cards", "approval c2"}
		if !slices.Equal(steps, want) {
			t.Errorf("%s: steps = %q, want %q: the answer buys the session, no verify of c1", reason, steps, want)
		}
	}
}

// A cheap queue turn moves the head from c1 to m1, main's merge into the
// branch, and the approval stage's change request sends the head back to
// the agent: the next session starts from m1, and at the session cap
// that head is not verified again.
func TestQueueTurnHeadIsTheNextSessionsStart(t *testing.T) {
	q := newQueueHarness(t, policy.QueueProject, green)
	q.taskConfig.Sessions = 2
	q.sync = func(from string) SyncResult {
		if from == "c1" {
			return SyncResult{From: from, To: "m1", Main: "main1", Base: "b0"}
		}
		return SyncResult{From: from, To: from}
	}
	q.branchHead = "m1"
	q.onSessions(pushed("s1", "c1"), SessionResult{SessionID: "s2", Outcome: OutcomeNoPush, Summary: "stopped"})
	q.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", PR: 9, Body: "rename it"})
	q.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := q.run(t); final != "closed" {
		t.Fatalf("final = %q, finals %v", final, q.finals)
	}
	want := "m1"
	if len(q.sessions) != 2 || q.sessions[1].StartRef != want {
		t.Fatalf("sessions %+v, want the second from %s", q.sessions, want)
	}
	policies := 0
	for _, s := range q.steps {
		if s == "policy" {
			policies++
		}
	}
	if policies != 1 || len(q.escalated) != 1 || !AtSessionCap(q.escalated[0]) {
		t.Fatalf("steps %q, escalations %q; want one pass of the gates and the session cap's card", q.steps, q.escalated)
	}
}

// A head the policy stage held, c2, counts as judged: when the card
// reaches its session cap on it, the cap check does not verify c2 again.
// The hold adopts c2 within the pass only in a run from before
// auto-moved-head; from it on the policy stage verifies c2 from the first
// stage before the second session, and the cap check does not verify it
// again.
func TestCapCheckSkipsAnAdoptedHead(t *testing.T) {
	for _, before := range []bool{false, true} {
		h := newWorkflowHarness(t)
		if before {
			h.env.OnGetVersion("auto-moved-head", workflow.DefaultVersion, versionAutoMovedHead).Return(workflow.DefaultVersion)
		}
		h.taskConfig = askTaskConfig()
		h.taskConfig.Sessions = 2
		h.branchHead = "c2"
		h.env.OnActivity(ActivityGatePolicy, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(
			func(ctx context.Context, taskID string, pr int64, commit string, pin int64) (PolicyGate, error) {
				if commit == "c1" {
					return PolicyGate{}, headMovedError("pull request #9's head moved to c2 after its checks passed on c1")
				}
				return PolicyGate{}, nil
			})
		var steps []string
		h.onVerify(&steps, []SessionResult{pushed("s1", "c1"), {SessionID: "s2", Outcome: OutcomeNoPush, Summary: "stopped"}})
		h.onReviews("c2")
		h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
		if final := h.run(t); final != "closed" {
			t.Fatalf("before %v: final = %q, finals %v", before, final, h.finals)
		}
		want := []string{"session 1 after 0 cards", "pr c1 after 0 cards", "pr c2 after 0 cards", "session 2 after 0 cards"}
		if before {
			want = []string{"session 1 after 0 cards", "pr c1 after 0 cards", "session 2 after 0 cards"}
		}
		if !slices.Equal(steps, want) || len(h.escalated) != 1 || !AtSessionCap(h.escalated[0]) {
			t.Errorf("before %v: steps %q, escalations %q; want %q and the session cap's card", before, steps, h.escalated, want)
		}
	}
}

// A roll whose policy stage held a moved head, r2, with red checks ends at
// the next round when RollCommit leaves the branch at r2: the findings
// stand on the head it leaves as it is.
func TestRollEndsOnTheHeldHeadsFindings(t *testing.T) {
	g, commits := newRollHarnessWith(t, func(g *gateHarness) {
		g.policyErrs = []error{headMovedError("pull request #9's head moved to r2 after its checks passed on r1")}
	}, "r1", "r2")
	g.branchHead = "r2"
	var checked []string
	g.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9, URL: "https://git.example/sf/r/pulls/9"}, nil)
	g.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, sha string) (ChecksResult, error) {
			checked = append(checked, sha)
			return ChecksResult{State: ChecksFailure, Findings: []string{"test red on " + sha}}, nil
		})
	if final := g.run(t); final != "failed" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(*commits) != 2 || !slices.Equal(checked, []string{"r2"}) || g.finals[0][1] != "roll: checks failed: test red on r2" {
		t.Errorf("RollCommit calls %d, checks on %q, finals %v; want the second round ending on r2's findings", len(*commits), checked, g.finals)
	}
}

// Forgejo's refusal to merge main into the pull request is a fact, not a
// finding a model raised: the session it sends back starts with the
// conflict's instruction and no check-in judges it first.
func TestMergeConflictGoesToTheSessionWithoutACheckin(t *testing.T) {
	q := newQueueHarness(t, policy.QueueProject, green)
	q.taskConfig.CheckIn = time.Minute
	q.sync = func(from string) SyncResult {
		if from == "c1" {
			return SyncResult{From: from, Conflict: "pulls/9/update?style=merge: status 409"}
		}
		return SyncResult{From: from, To: from}
	}
	q.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	q.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := q.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, q.finals)
	}
	if len(q.sessions) != 2 || len(q.sessions[1].Findings) != 1 || !strings.HasPrefix(q.sessions[1].Findings[0], "the pull request no longer merges into main") {
		t.Fatalf("sessions %+v, want the second on the conflict's instruction", q.sessions)
	}
	if q.sessions[1].CheckInAsk != nil {
		t.Errorf("the conflict's session asks the check-in")
	}
}

// A session whose check-in stopped it mid-session ran its conversation:
// it is the next session's parent, whose working state that one continues,
// as any session that ran is. A session stopped at its start check-in ran
// nothing and stays no parent.
func TestMidSessionCheckinStopIsTheNextParent(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	h.taskConfig.Sessions = 6
	conflict := func(moment string) *CheckInOutcome {
		return &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckGoalConflict, Reason: "the objective contradicts the review",
			Record: &CheckInRecord{Moment: moment}}
	}
	var steps []string
	h.onVerify(&steps, []SessionResult{
		pushed("s1", "c1"),
		{SessionID: "s2", Outcome: OutcomeCheckinHuman, Commit: "c2", Trip: string(TripNoProgress), Summary: "check-in: human after no_progress",
			Reason: "the objective contradicts the review", CheckIn: conflict(CheckInSession)},
		checkinStopped("s3", OutcomeCheckinHuman, conflict(CheckInGate)),
		pushed("s4", "c3")})
	h.onReviews("c1")
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 603})
	h.signalAfter(24*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 4 {
		t.Fatalf("final = %q, %d sessions, escalations %q", final, len(h.sessions), h.escalated)
	}
	want := []string{"s1", "s2", "s2"}
	got := []string{h.sessions[1].ParentSessionID, h.sessions[2].ParentSessionID, h.sessions[3].ParentSessionID}
	if !slices.Equal(got, want) {
		t.Errorf("the parents of s2, s3, s4 = %q, want %q", got, want)
	}
	if h.sessions[2].ParentOutcome != OutcomeCheckinHuman || h.sessions[2].ParentTrip != string(TripNoProgress) {
		t.Errorf("s3's parent ended %q after %q, want s2's check-in stop after no_progress", h.sessions[2].ParentOutcome, h.sessions[2].ParentTrip)
	}
}

// spentStop is a check-in's spend stop that names neither the call cap nor the
// time cap: its card asks, and a human's answer verifies an open head.
const spentStop = "the card's spend is above the policy's bound"
