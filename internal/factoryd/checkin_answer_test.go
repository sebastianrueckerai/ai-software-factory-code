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
// only while the card's limits are unchanged: a raise of the sessions is a
// change, and the repeat is asked as a stop of its own. A run from before
// the change compares the line raises alone.
func TestCheckinRepeatGuardSeesARaise(t *testing.T) {
	for name, tc := range map[string]struct {
		sessions int // the sessions an Extend raised while the first stop waited
		before   bool
		deadlock bool
	}{
		"unchanged":                          {0, false, true},
		"sessions raised":                    {2, false, false},
		"sessions raised, before the change": {2, true, true},
	} {
		h := newWorkflowHarness(t)
		if tc.before {
			h.beforeCheckinAnswer()
		}
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

// The repeat guard reads the checkin-answer change only where the ask from
// before the change and the ask at it differ, as the answer does: a run
// that waits at a repeat whose asks agree reads nothing, so its answer
// still takes the change. A sessions raise parts the asks, and the version
// is read at that stop, before its question.
func TestCheckinRepeatReadsTheChangeOnlyWhereTheAsksDiffer(t *testing.T) {
	for name, tc := range map[string]struct {
		sessions int   // the sessions an Extend raised while the first stop waited
		reads    []int // the questions asked before each read
	}{
		"the asks agree":   {0, nil},
		"a sessions raise": {2, []int{1}},
	} {
		h := newWorkflowHarness(t)
		var reads []int
		h.env.OnGetVersion("checkin-answer", workflow.DefaultVersion, versionCheckinAnswer).
			Run(func(mock.Arguments) { reads = append(reads, len(h.escalated)) }).Return(versionCheckinAnswer)
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

		if final := h.run(t); final != "closed" || len(h.escalated) != 2 {
			t.Fatalf("%s: final = %q, escalations = %q", name, final, h.escalated)
		}
		if !slices.Equal(reads, tc.reads) {
			t.Errorf("%s: checkin-answer read after %v questions, want %v", name, reads, tc.reads)
		}
	}
}

// A cheap queue turn moves the head from c1 to m1, main's merge into the
// branch, and the approval stage's change request sends the head back to
// the agent: the next session starts from m1, and at the session cap
// that head is not verified again. A run from before the change starts
// the session from c1.
func TestQueueTurnHeadIsTheNextSessionsStart(t *testing.T) {
	for _, before := range []bool{false, true} {
		q := newQueueHarness(t, policy.QueueProject, green)
		if before {
			q.beforeCheckinAnswer()
		}
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
			t.Fatalf("before %v: final = %q, finals %v", before, final, q.finals)
		}
		want := "m1"
		if before {
			want = "c1"
		}
		if len(q.sessions) != 2 || q.sessions[1].StartRef != want {
			t.Fatalf("before %v: sessions %+v, want the second from %s", before, q.sessions, want)
		}
		policies := 0
		for _, s := range q.steps {
			if s == "policy" {
				policies++
			}
		}
		if policies != 1 || len(q.escalated) != 1 || !AtSessionCap(q.escalated[0]) {
			t.Fatalf("before %v: steps %q, escalations %q; want one pass of the gates and the session cap's card", before, q.steps, q.escalated)
		}
	}
}

// A head the policy stage held, c2, counts as judged: when the card
// reaches its session cap on it, the cap check does not verify c2 again.
// A run from before the change verifies it a second time.
func TestCapCheckSkipsAnAdoptedHead(t *testing.T) {
	for _, before := range []bool{false, true} {
		h := newWorkflowHarness(t)
		if before {
			h.beforeCheckinAnswer()
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
		want := []string{"session 1 after 0 cards", "pr c1 after 0 cards", "session 2 after 0 cards"}
		if before {
			want = append(want, "pr c2 after 0 cards")
		}
		if !slices.Equal(steps, want) || len(h.escalated) != 1 || !AtSessionCap(h.escalated[0]) {
			t.Errorf("before %v: steps %q, escalations %q; want %q and the session cap's card", before, steps, h.escalated, want)
		}
	}
}

// A roll whose policy stage held a moved head, r2, with red checks ends at
// the next round when RollCommit leaves the branch at r2: the findings
// stand on the head it leaves as it is. A run from before the change
// compares RollCommit's head with r1, the head it verified, and verifies
// r2 again before it ends.
func TestRollEndsOnTheHeldHeadsFindings(t *testing.T) {
	for _, before := range []bool{false, true} {
		g, commits := newRollHarnessWith(t, func(g *gateHarness) {
			g.policyErrs = []error{headMovedError("pull request #9's head moved to r2 after its checks passed on r1")}
		}, "r1", "r2")
		if before {
			g.beforeCheckinAnswer()
		}
		g.branchHead = "r2"
		var checked []string
		g.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9, URL: "https://git.example/sf/r/pulls/9"}, nil)
		g.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).Return(
			func(ctx context.Context, taskID, sha string) (ChecksResult, error) {
				checked = append(checked, sha)
				return ChecksResult{State: ChecksFailure, Findings: []string{"test red on " + sha}}, nil
			})
		if final := g.run(t); final != "failed" {
			t.Fatalf("before %v: final = %q, finals %v", before, final, g.finals)
		}
		if before {
			if len(*commits) != 3 || len(checked) < 2 || checked[1] != "r2" {
				t.Errorf("before the change: RollCommit calls %d, checks on %q; want r2 verified again in a third round", len(*commits), checked)
			}
			continue
		}
		if len(*commits) != 2 || !slices.Equal(checked, []string{"r2"}) || g.finals[0][1] != "roll: checks failed: test red on r2" {
			t.Errorf("RollCommit calls %d, checks on %q, finals %v; want the second round ending on r2's findings", len(*commits), checked, g.finals)
		}
	}
}

// Forgejo's refusal to merge main into the pull request is a fact, not a
// finding a model raised: the session it sends back starts with the
// conflict's instruction and no check-in judges it first. A run from
// before the change asks the check-in about it.
func TestMergeConflictGoesToTheSessionWithoutACheckin(t *testing.T) {
	for _, before := range []bool{false, true} {
		q := newQueueHarness(t, policy.QueueProject, green)
		if before {
			q.env.OnGetVersion("conflict-fact", workflow.DefaultVersion, versionConflictFact).Return(workflow.DefaultVersion)
		}
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
			t.Fatalf("before %v: final = %q, finals %v", before, final, q.finals)
		}
		if len(q.sessions) != 2 || len(q.sessions[1].Findings) != 1 || !strings.HasPrefix(q.sessions[1].Findings[0], "the pull request no longer merges into main") {
			t.Fatalf("before %v: sessions %+v, want the second on the conflict's instruction", before, q.sessions)
		}
		if asked := q.sessions[1].CheckInAsk != nil; asked != before {
			t.Errorf("before %v: the conflict's session asks the check-in: %v, want %v", before, asked, before)
		}
	}
}

// A session whose check-in stopped it mid-session ran its conversation:
// it is the next session's parent, whose working state that one continues,
// as any session that ran is. A session stopped at its start check-in ran
// nothing and stays no parent. A run from before the change keeps the
// last session that ended on its own as the parent.
func TestMidSessionCheckinStopIsTheNextParent(t *testing.T) {
	for _, before := range []bool{false, true} {
		h := newWorkflowHarness(t)
		if before {
			h.env.OnGetVersion("stopped-parent", workflow.DefaultVersion, versionStoppedParent).Return(workflow.DefaultVersion)
		}
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
			t.Fatalf("before %v: final = %q, %d sessions, escalations %q", before, final, len(h.sessions), h.escalated)
		}
		want := []string{"s1", "s2", "s2"}
		if before {
			want = []string{"s1", "s1", "s1"}
		}
		got := []string{h.sessions[1].ParentSessionID, h.sessions[2].ParentSessionID, h.sessions[3].ParentSessionID}
		if !slices.Equal(got, want) {
			t.Errorf("before %v: the parents of s2, s3, s4 = %q, want %q", before, got, want)
		}
		if !before && (h.sessions[2].ParentOutcome != OutcomeCheckinHuman || h.sessions[2].ParentTrip != string(TripNoProgress)) {
			t.Errorf("s3's parent ended %q after %q, want s2's check-in stop after no_progress", h.sessions[2].ParentOutcome, h.sessions[2].ParentTrip)
		}
	}
}

// spentStop is a check-in's spend stop that names neither the call cap nor the
// time cap: its card asks, and a human's answer verifies an open head.
const spentStop = "the card's spend is above the policy's bound"
