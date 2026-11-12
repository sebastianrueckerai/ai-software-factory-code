package factoryd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// newRollHarness is a roll task's run: Reconcile reports a roll, RunSession
// fails the test if called, RollCommit answers heads in order (the last
// repeating), and the policy classes the k8s/ change high.
func newRollHarness(t *testing.T, heads ...string) (*gateHarness, *[]string) {
	return newRollHarnessWith(t, func(*gateHarness) {}, heads...)
}

// newRollHarnessWith is newRollHarness with setup run before the mocks.
func newRollHarnessWith(t *testing.T, setup func(*gateHarness), heads ...string) (*gateHarness, *[]string) {
	g := newGateHarness(t, func(g *gateHarness) {
		g.gate = PolicyGate{Class: "high", Outcome: policy.OutcomeRequiresApproval, PolicyVersion: 7, Changed: 2, Paths: []string{"k8s/factoryd.yaml"}}
		g.review = Review{Note: "no model review: a roll task makes no model call"}
		setup(g)
	})
	g.state = RunState{Roll: true}
	g.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			t.Errorf("a roll task ran a session: %+v", in)
			return SessionResult{}, nil
		})
	var commits []string
	g.env.OnActivity(ActivityRollCommit, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) (string, error) {
			commits = append(commits, g.env.Now().Format(time.TimeOnly))
			return heads[min(len(commits), len(heads))-1], nil
		})
	ro := 0
	g.env.OnActivity(ActivityAwaitRollout, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr int64, bound time.Duration) (RolloutResult, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			ro++
			g.steps = append(g.steps, "rollout")
			if len(g.rollout) == 0 {
				return RolloutResult{Confirmed: true}, nil
			}
			return g.rollout[min(ro, len(g.rollout))-1], nil
		})
	return g, &commits
}

// A roll task runs no session: RollCommit writes the branch and its head
// goes straight to the pull request, the checks, the gates and the card;
// the approval merges it as for any task.
func TestWorkflowRollGoesStraightToChecks(t *testing.T) {
	g, commits := newRollHarness(t, "r1")
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(*commits) != 1 || len(g.sessions) != 0 {
		t.Fatalf("RollCommit calls %v, sessions %v", *commits, g.sessions)
	}
	if want := []string{"policy", "repro r1", "review r1", "criteria r1", "approval r1", "merge", "rollout"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
	if !slices.Equal(g.merges, []string{"9 r1 sebastian"}) || g.approvals[0].Gates.Policy.Class != "high" {
		t.Fatalf("merges %v, approval %+v", g.merges, g.approvals[0])
	}
	// a roll makes no model call: its review's note is no finding
	for _, w := range g.approvals[0].WhyHuman {
		if strings.Contains(w, noVerdict) {
			t.Fatalf("why human = %q", g.approvals[0].WhyHuman)
		}
	}
	if !slices.Equal(g.states, []string{"running", "in-review"}) {
		t.Fatalf("states = %v", g.states)
	}
}

// A roll ends done when its rollout is confirmed: the
// approval's merge is not its end — the run waits for the images it
// merged to run on their Deployments (AwaitRollout) and finishes done
// naming what went live, never the wall-clock cap.
func TestWorkflowRollEndsDoneWhenRolloutConfirms(t *testing.T) {
	g, _ := newRollHarness(t, "r1")
	g.rollout = []RolloutResult{{Confirmed: true, Live: []string{"factoryd"}}}
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	reason := g.finals[0][1]
	if !strings.Contains(reason, "rollout live: factoryd") || strings.Contains(reason, "wall-clock") {
		t.Fatalf("reason = %q, want the rollout named, no wall-clock cap", reason)
	}
	if !slices.Contains(g.steps, "rollout") {
		t.Fatalf("steps = %v, want the rollout wait", g.steps)
	}
}

// Newer images folded into the open roll arrive as a change request: the
// run has RollCommit write them and takes the new head through the stages
// and a new card; the approval merges the new head.
func TestWorkflowRollFoldsANewHeadBeforeItsMerge(t *testing.T) {
	g, commits := newRollHarness(t, "r1", "r2")
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalRollFolded, TaskSignal{Author: "factory-agent", PR: 9, Body: "roll c2c2c2c: worker folded in"})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	want := []string{"policy", "repro r1", "review r1", "criteria r1", "approval r1", "policy", "repro r2", "review r2", "criteria r2", "approval r2", "merge", "rollout"}
	if !slices.Equal(g.steps, want) || len(*commits) != 2 || len(g.sessions) != 0 {
		t.Fatalf("steps = %v, want %v; RollCommit calls %v", g.steps, want, *commits)
	}
	if !slices.Equal(g.merges, []string{"9 r2 sebastian"}) {
		t.Fatalf("merges = %v", g.merges)
	}
	if !slices.Equal(g.states, []string{"running", "in-review", "running", "in-review"}) {
		t.Fatalf("states = %v", g.states)
	}
}

// A roll schedules no review beside its checks wait: its
// review is the stage's own, after green checks.
func TestWorkflowRollReviewsAfterChecks(t *testing.T) {
	g, commits := newRollHarness(t, "r1")
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if len(*commits) != 1 || len(g.reviewGates) != 1 || !g.reviewGates[0].ChecksPassed {
		t.Fatalf("RollCommit calls %v, review gates %+v: want one review after green checks", *commits, g.reviewGates)
	}
	if want := []string{"policy", "repro r1", "review r1", "criteria r1", "approval r1", "merge", "rollout"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
}

// Red checks on a head RollCommit leaves as it is fail the roll: the
// local gates ran first (the pinned order), no agent answers findings on
// a roll, and the next green commit opens the next.
func TestWorkflowRollFailsOnFindingsItCannotAnswer(t *testing.T) {
	g, commits := newRollHarness(t, "r1")
	red := "check \"pr / test (pull_request)\": failure"
	g.onChecks(ChecksResult{State: ChecksFailure, Findings: []string{red}})
	if final := g.run(t); final != "failed" {
		t.Fatalf("final = %q", final)
	}
	if len(*commits) != 2 || len(g.sessions) != 0 || !slices.Equal(g.steps, []string{"policy", "repro r1"}) {
		t.Fatalf("RollCommit calls %v, sessions %v, steps %v", *commits, g.sessions, g.steps)
	}
	if reason := g.finals[0][1]; reason != "roll: checks failed: "+red {
		t.Fatalf("reason = %q", reason)
	}
}

// A healed roll run adopts its open pull request: RollCommit returns the
// head as it is and the run verifies it, no session.
func TestWorkflowRollHealAdoptsItsPullRequest(t *testing.T) {
	g, commits := newRollHarness(t, "r1")
	g.state = RunState{Roll: true, PR: PullRequest{Number: 9}, Head: "r1"}
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalPRClosed, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "failed" || !strings.Contains(g.finals[0][1], "closed without merge") {
		t.Fatalf("final = %q %v", final, g.finals)
	}
	if len(*commits) != 1 || len(g.sessions) != 0 || !slices.Equal(g.steps, []string{"policy", "repro r1", "review r1", "criteria r1", "approval r1"}) {
		t.Fatalf("RollCommit calls %v, steps %v", *commits, g.steps)
	}
}

// A fold the previous round's RollCommit already wrote (inserted between
// its drain and its read) wakes the approval wait on an unmoved head: the
// run takes the head through the stages again and never counts the fold
// as a finding.
func TestWorkflowRollFoldOnAnUnmovedHeadIsNoFinding(t *testing.T) {
	g, commits := newRollHarness(t, "r1")
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalRollFolded, TaskSignal{Author: "factory-agent", PR: 9, Body: "roll c2c2c2c: worker folded in"})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	want := []string{"policy", "repro r1", "review r1", "criteria r1", "approval r1", "policy", "repro r1", "review r1", "criteria r1", "approval r1", "merge", "rollout"}
	if !slices.Equal(g.steps, want) || len(*commits) != 2 {
		t.Fatalf("steps = %v, want %v; RollCommit calls %v", g.steps, want, *commits)
	}
}

// A human's change request on a roll's unmoved head fails it: no agent
// answers it.
func TestWorkflowRollFailsOnAHumansChangeRequest(t *testing.T) {
	g, _ := newRollHarness(t, "r1")
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", PR: 9, Body: "not now"})
	if final := g.run(t); final != "failed" || g.finals[0][1] != "roll: changes requested: reviewer sebastian requested changes: not now" {
		t.Fatalf("final = %q %v", final, g.finals)
	}
}

// A roll merges at once after its approval (roadmap 222, item 2: sessions
// survive the restart its merge brings); a merge that failed or was
// refused waits for the retry or the next approval only.
func TestWorkflowRollMergesWithoutAGap(t *testing.T) {
	g, _ := newRollHarness(t, "r1")
	g.merge = []MergeResult{{Failed: "forgejo answered 502"}, {Refusal: "the approval is stale"}, {Merged: true}}
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	want := []string{"policy", "repro r1", "review r1", "criteria r1", "approval r1", "merge", "merge", "merge", "rollout"}
	if !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
}

// A stray roll_folded on a task that is no roll changes nothing: it
// sleeps in its channel until the run ends and is drained.
func TestWorkflowNoRollIgnoresAStrayRollFolded(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnActivity(ActivityRollCommit, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) (string, error) {
			t.Errorf("a stray roll_folded ran a roll commit: %s", taskID)
			return "", nil
		})
	h.onSessions(pushed("s1", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalRollFolded, TaskSignal{Author: "factory-agent", PR: 9, Body: "roll c2c2c2c: worker folded in"})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, h.finals)
	}
	if len(h.sessions) != 1 {
		t.Fatalf("sessions = %+v, want the one", h.sessions)
	}
	if strings.Contains(h.logs.String(), "unhandled signals") {
		t.Fatalf("log:\n%s", h.logs.String())
	}
}

// A roll under a project's merge queue takes no turn in it: its stages go
// straight to the approval with no EnterQueue and no SyncWithMain, so a
// card merges past a roll that waits (roadmap 222, item 1).
func TestWorkflowRollTakesNoQueueTurn(t *testing.T) {
	g, commits := newRollHarnessWith(t, func(g *gateHarness) {
		g.taskConfig = &TaskConfig{Process: "spec", Track: policy.TrackDirect, Class: policy.ClassCoder, Session: policy.SessionCode,
			Stages: testStages(), Queue: policy.QueueProject}
	}, "r1")
	g.env.OnActivity(ActivityEnterQueue, mock.Anything, "t1").Return(func(ctx context.Context, taskID string) (int, error) {
		t.Errorf("a roll entered the merge queue")
		return 0, nil
	})
	g.env.OnActivity(ActivitySyncWithMain, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr int64, from string) (SyncResult, error) {
			t.Errorf("a roll synced with main")
			return SyncResult{From: from, To: from}, nil
		})
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(*commits) != 1 {
		t.Fatalf("RollCommit calls %v", *commits)
	}
	if want := []string{"policy", "repro r1", "review r1", "criteria r1", "approval r1", "merge", "rollout"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
}

// A roll's review that answers concerns and names no finding is a remark on
// the card: the roll reaches its approval with the remark among the reasons
// for a human and is not failed.
func TestWorkflowRollConcernsWithoutFindingsIsARemark(t *testing.T) {
	g, _ := newRollHarnessWith(t, func(g *gateHarness) {
		g.review = Review{Verdict: "concerns", Summary: "the pins look odd"}
	}, "r1")
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(g.approvals) != 1 || !slices.Equal(g.approvals[0].WhyHuman, []string{"review: remark, no finding: the pins look odd"}) {
		t.Fatalf("approvals %+v; want the remark the one reason for a human", g.approvals)
	}
}

// overBound is a session the worker ended on the named push refusal: the
// branch's diff against its merge base is over the card's line bound even
// after the merge of main, so the card cannot fit its patch.
func overBound(session string) SessionResult {
	refusal := "push failed: push refused: over the card's line bound: the branch's diff against its merge base is 340 lines over the card's 300-line bound; merge main, run the checks, and push again — if the merge cannot fit, the card closes into a successor"
	return SessionResult{Outcome: OutcomeNoPush, SessionID: session, Summary: refusal,
		Findings: []string{"previous session ended without a pushed commit: " + refusal}}
}

// A session that ends on the named push refusal closes the card into a
// successor instead of looping: the successor activity runs once with the
// refusal's findings and the open pull request, the card finishes failed
// with a reason naming the successor, and nobody is asked.
func TestPushOverBoundClosesIntoASuccessor(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	var steps []string
	h.onVerify(&steps, []SessionResult{pushed("s1", "c1"), overBound("s2")})
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			return Review{Verdict: "concerns", Summary: "one fault", Findings: []string{"rename the flag"}}, nil
		})
	h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "failed" {
		t.Fatalf("final = %q, escalations %q", final, h.escalated)
	}
	if len(h.successions) != 1 || h.successions[0].TaskID != "t1" || h.successions[0].PR != 9 {
		t.Fatalf("the successor filings = %+v, want one for t1 carrying PR 9", h.successions)
	}
	if !strings.Contains(h.successions[0].Findings, "push refused: over the card's line bound") {
		t.Fatalf("the successor's findings = %q, want the refusal", h.successions[0].Findings)
	}
	if reason := h.finals[0][1]; !strings.Contains(reason, "successor") || !strings.Contains(reason, "t-succ") {
		t.Fatalf("the finish reason %q must name the successor", reason)
	}
	if len(h.escalated) != 0 {
		t.Fatalf("escalations = %q, want none", h.escalated)
	}
}

// The second over-bound end of a line comes back from RunSession as an
// escalation (TestSecondOverBoundEndOpensACase): the workflow asks the
// human with its reason, which opens the card's case, and files no
// successor.
func TestSecondOverBoundEndFilesNoSuccessor(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	again := overBound("s1")
	again.Outcome = OutcomeEscalate
	again.Reason = "This card continues work an earlier card left over its line bound, and its session ended over the bound again: " +
		"640 changed lines against the card's 300-line bound, on factory/t1 at c7. Cut the work into cards that each fit the bound; no further successor is filed."
	var steps []string
	h.onVerify(&steps, []SessionResult{again})
	h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	h.run(t)
	if len(h.successions) != 0 {
		t.Fatalf("the successor filings = %+v, want none", h.successions)
	}
	if len(h.escalated) != 1 || h.escalated[0] != again.Reason {
		t.Fatalf("escalations = %q, want the cut asked once", h.escalated)
	}
}

// The deadlock ask's successor exit runs the same close: the answer files
// the successor with the work state instead of buying another session, and
// the card finishes failed naming it.
func TestDeadlockAnswerCloseIntoASuccessorRunsTheSamePath(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	var steps []string
	h.onVerify(&steps, []SessionResult{
		pushed("s1", "c1"),
		checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
			Reason:   spentStop,
			Findings: []CheckFinding{{Text: "the migration runs outside its transaction"}},
			Review:   &Review{Verdict: "concerns", Summary: "two faults block the merge"},
			Record:   &CheckInRecord{Moment: CheckInGate, Session: "s2", ParentSession: "s1"}}),
		checkinStopped("s3", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
			Reason:   spentStop,
			Findings: []CheckFinding{{Text: "the migration runs outside its transaction"}},
			Review:   &Review{Verdict: "concerns", Summary: "two faults block the merge"},
			Record:   &CheckInRecord{Moment: CheckInGate, Session: "s3", ParentSession: "s2"}})})
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			return Review{Verdict: "concerns", Summary: "one fault", Findings: []string{"rename the flag"}}, nil
		})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: SuccessorChoice, Comment: 603})
	h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "failed" {
		t.Fatalf("final = %q, escalations %q", final, h.escalated)
	}
	if len(h.escalated) != 2 {
		t.Fatalf("escalations = %q, want the stop and its deadlock ask", h.escalated)
	}
	if len(h.successions) != 1 || h.successions[0].PR != 9 {
		t.Fatalf("the successor filings = %+v, want one carrying PR 9", h.successions)
	}
	for _, want := range []string{"the migration runs outside its transaction", "review: concerns"} {
		if !strings.Contains(h.successions[0].Findings, want) {
			t.Errorf("the successor's findings = %q, want %q", h.successions[0].Findings, want)
		}
	}
	if reason := h.finals[0][1]; !strings.Contains(reason, "successor") {
		t.Fatalf("the finish reason %q must name the successor", reason)
	}
	if n := len(h.sessions); n != 3 {
		t.Fatalf("sessions = %d, want three: the answer closes the card instead of buying a fourth", n)
	}
}

// The successor activity files one card: the stuck card's objective with
// the findings so far, the carried pull request and its size and budget,
// queued on the direct track with no base of its own — it starts from
// main's tip — on an issue labelled like a filed card's, started under
// the work bound.
func TestSuccessorFilesOneCard(t *testing.T) {
	ctx := context.Background()
	r, store, objects, fj, wf := newRetryRig(t)
	a := store.tasks["t-a"]
	a.Size, a.PRNumber = "small", 7
	store.tasks["t-a"] = a
	fj.calls = nil
	id, err := r.Successor(ctx, SuccessorInput{TaskID: "t-a", PR: 9,
		Findings: "\n- the migration runs outside its transaction\n- the flag is read before it is set"})
	if err != nil || id != SuccessorID("t-a") {
		t.Fatalf("Successor = %q, %v", id, err)
	}
	s := store.tasks[id]
	if s.Status != "queued" || s.Track != policy.TrackDirect || s.Source != SourceSuccessor || s.Kind != "agent" ||
		s.CarryPR != 9 || s.SHA != "" || s.Repository != "linkshort/linkshort" || s.Parent != "t-plan" ||
		s.Process != "spec" || s.BudgetEUR != 2 || s.Size != "small" {
		t.Fatalf("the successor row = %+v", s)
	}
	is := fj.issues[9]
	for _, want := range []string{"Findings so far:", "\n- the migration runs outside its transaction", "\n- the flag is read before it is set",
		"Carries: #9", "Size: small", "Budget: 2.00 EUR", successorMarker("t-a")} {
		if !strings.Contains(is.body, want) {
			t.Errorf("the successor issue %q misses %q", is.body, want)
		}
	}
	if is.poster != "factory-agent" || !slices.Equal(is.labels, []string{"agent", "state/queued"}) || s.IssueNumber != 9 {
		t.Fatalf("issue #9 = %+v, the row's issue %d", is, s.IssueNumber)
	}
	if !slices.Equal(wf.started, []string{id}) {
		t.Fatalf("started %q, want the successor alone", wf.started)
	}
	if got := events(t, objects, id); len(got) != 1 || !strings.HasPrefix(got[0], "card_created") {
		t.Fatalf("the successor's events %v, want its card_created", got)
	}
}

// A successor filing that stopped between its issue and its row runs again
// to the same one card: the marker holds the issue, the row's identity
// holds the card, and no second issue is filed.
func TestSuccessorRetryFilesNoSecondIssue(t *testing.T) {
	ctx := context.Background()
	r, store, _, fj, _ := newRetryRig(t)
	fj.failLabel = 1
	if _, err := r.Successor(ctx, SuccessorInput{TaskID: "t-a", PR: 9, Findings: "\n- one finding"}); err == nil {
		t.Fatal("the first filing must stop at its labels")
	}
	id, err := r.Successor(ctx, SuccessorInput{TaskID: "t-a", PR: 9, Findings: "\n- one finding"})
	if err != nil {
		t.Fatalf("the second filing = %v", err)
	}
	var filed []*planIssue
	for n := int64(1); n <= fj.next; n++ {
		if is := fj.issues[n]; is != nil && strings.Contains(is.body, successorMarker("t-a")) {
			filed = append(filed, is)
		}
	}
	if len(filed) != 1 {
		t.Fatalf("issues with the marker = %d, want one", len(filed))
	}
	if store.tasks[id].IssueNumber != 9 {
		t.Fatalf("the successor row's issue = %d, want 9", store.tasks[id].IssueNumber)
	}
}

// A successor's issue that closed inside the retry window — filed, then
// closed by a human or a rule before the retry lands — is still found:
// the marker search reads closed issues too, so the retry files no
// second issue.
func TestSuccessorFindsItsIssueClosed(t *testing.T) {
	ctx := context.Background()
	r, store, _, fj, _ := newRetryRig(t)
	fj.failLabel = 1
	if _, err := r.Successor(ctx, SuccessorInput{TaskID: "t-a", PR: 9, Findings: "\n- one finding"}); err == nil {
		t.Fatal("the first filing must stop at its labels")
	}
	fj.mu.Lock()
	fj.issues[9].state = "closed"
	fj.mu.Unlock()
	id, err := r.Successor(ctx, SuccessorInput{TaskID: "t-a", PR: 9, Findings: "\n- one finding"})
	if err != nil {
		t.Fatalf("the refiling = %v", err)
	}
	var filed []*planIssue
	for n := int64(1); n <= fj.next; n++ {
		if is := fj.issues[n]; is != nil && strings.Contains(is.body, successorMarker("t-a")) {
			filed = append(filed, is)
		}
	}
	if len(filed) != 1 {
		t.Fatalf("issues with the marker = %d, want one", len(filed))
	}
	if store.tasks[id].IssueNumber != 9 {
		t.Fatalf("the successor row's issue = %d, want 9", store.tasks[id].IssueNumber)
	}
}

// Only factoryd's note, the first line of the findings, continues the old
// card's branch: an agent's summary that quotes the note further down
// starts the successor from main, as any close does.
func TestSuccessorContinuesTheBranchOnFactorysNoteAlone(t *testing.T) {
	ctx := context.Background()
	r, store, _, fj, _ := newRetryRig(t)
	id, err := r.Successor(ctx, SuccessorInput{TaskID: "t-a",
		Findings: "\n- previous session ended without a pushed commit: I read " + overBoundNote + " factory/t-a at c7"})
	if err != nil {
		t.Fatal(err)
	}
	if head, ok := fj.branches["factory/"+id]; ok || store.tasks[id].SHA != "" {
		t.Fatalf("the successor's branch = %q, base %q; want no branch and main's tip", head, store.tasks[id].SHA)
	}
}

// An over-bound result closes into a successor that continues the old
// card's branch: the branch is copied to the successor's own, so its first
// session starts at the interim head (startRef reads the branch first) and
// its clone, which fetches the start's branch when the start is missing,
// holds the work. A close without the note starts from main, as before.
func TestSuccessorOfOverBoundWorkStartsAtTheInterimHead(t *testing.T) {
	ctx := context.Background()
	r, store, _, fj, _ := newRetryRig(t)
	id, err := r.Successor(ctx, SuccessorInput{TaskID: "t-a",
		Findings: "\n- " + overBoundNote + " factory/t-a at c7"})
	if err != nil {
		t.Fatal(err)
	}
	if fj.branches["factory/"+id] != "c7" {
		t.Fatalf("the successor's branch = %q, want the old card's interim head c7", fj.branches["factory/"+id])
	}
	s := store.tasks[id]
	_, repo, err := r.task(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if ref, err := r.startRef(ctx, &s, repo, ""); err != nil || ref != "c7" || s.SHA != store.tasks["t-a"].SHA {
		t.Fatalf("startRef = %q, %v with base %q; want the interim head with the old card's base", ref, err, s.SHA)
	}
	// the objective Successor writes is the one the line's rule reads
	if !continuesOverBoundWork(s) {
		t.Fatalf("the successor's objective %q does not read as over-bound work continued", s.Objective)
	}
}

// newRollConflictHarness is a roll task's run whose RollCommit answers by
// call number, the first merge conflicting.
func newRollConflictHarness(t *testing.T, commit func(n int) (string, error)) (*gateHarness, *int) {
	g := newGateHarness(t, func(g *gateHarness) {
		g.gate = PolicyGate{Class: "high", Outcome: policy.OutcomeRequiresApproval, PolicyVersion: 7, Changed: 2, Paths: []string{"k8s/factoryd.yaml"}}
		g.review = Review{Note: "no model review: a roll task makes no model call"}
	})
	g.state = RunState{Roll: true}
	n := 0
	g.env.OnActivity(ActivityRollCommit, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) (string, error) {
			n++
			return commit(n)
		})
	g.env.OnActivity(ActivityAwaitRollout, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr int64, bound time.Duration) (RolloutResult, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.steps = append(g.steps, "rollout")
			return RolloutResult{Confirmed: true}, nil
		})
	g.merge = []MergeResult{{Refusal: "forgejo POST .../merge: status 405: Please try again later", Conflict: true}, {Merged: true}}
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	return g, &n
}

// A roll's merge conflicts because main moved the roll's files: the roll's
// loop runs RollCommit again, which cuts the branch again, and the new head
// goes through the stages and merges.
func TestWorkflowRollConflictCutsAgainAndMerges(t *testing.T) {
	g, n := newRollConflictHarness(t, func(n int) (string, error) { return []string{"r1", "r2"}[min(n, 2)-1], nil })
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	want := []string{"policy", "repro r1", "review r1", "criteria r1", "approval r1", "merge", "policy", "repro r2", "review r2", "criteria r2", "approval r2", "merge", "rollout"}
	if *n != 2 || !slices.Equal(g.steps, want) || !slices.Equal(g.merges[len(g.merges)-1:], []string{"9 r2 sebastian"}) {
		t.Fatalf("steps %v, RollCommit calls %d, merges %v, want the cut-again head through the stages and merged", g.steps, *n, g.merges)
	}
}

// The next RollCommit ends the roll by the state of main's files: the task
// fails with that reason, and none of the conflict's text for an agent
// reaches it.
func TestWorkflowRollConflictEndsByTheStateNotByAnAgentText(t *testing.T) {
	why := "main changed k8s/factoryd.yaml (4040404 hand roll of the gateway) since c1c1c1c: the next run that holds it rolls"
	g, n := newRollConflictHarness(t, func(n int) (string, error) {
		if n == 1 {
			return "r1", nil
		}
		return "", terminalError("%s", why)
	})
	if final := g.run(t); final != "failed" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if *n != 2 || len(g.finals) == 0 || g.finals[0][1] != "roll commit: "+why || strings.Contains(g.finals[0][1], "git merge") {
		t.Fatalf("RollCommit calls %d, finals %v, want the state as the reason", *n, g.finals)
	}
}

// RollCommit answers that the roll has nothing to roll (its cut builds main's
// images, every one): the roll ends done with that reason and waits for no
// rollout, since nothing merged. A run from before the change ends it failed.
func TestWorkflowRollWithNothingToRollEndsDone(t *testing.T) {
	why := "nothing to roll: the images of c2c2c2c are main's pins, every one"
	for name, tc := range map[string]struct {
		before      bool
		final, want string
	}{
		"today":  {false, "done", why},
		"before": {true, "failed", "roll commit: " + why},
	} {
		t.Run(name, func(t *testing.T) {
			g, n := newRollConflictHarness(t, func(n int) (string, error) {
				if n == 1 {
					return "r1", nil
				}
				return "", nothingToRollError("%s", why)
			})
			if tc.before {
				g.env.OnGetVersion("roll-nothing", workflow.DefaultVersion, versionRollNothing).Return(workflow.DefaultVersion)
			}
			if final := g.run(t); final != tc.final {
				t.Fatalf("final = %q, finals %v", final, g.finals)
			}
			if *n != 2 || len(g.finals) == 0 || g.finals[0][1] != tc.want || slices.Contains(g.steps, "rollout") {
				t.Fatalf("RollCommit calls %d, finals %v, steps %v: want %q and no rollout wait", *n, g.finals, g.steps, tc.want)
			}
		})
	}
}

// A roll recorded on a dev server before the change, whose RollCommit failed
// with a terminal error, ended failed there and replays on today's code: the
// change's marker is read only where RollCommit fails, which the recording
// took the old way. The test refuses a recording without that failure.
func TestWorkflowReplaysARollRecordedBeforeNothingToRoll(t *testing.T) {
	f := "testdata/devserver-roll-commit-failed.json"
	hist := loadHistory(t, f)
	failed := false
	for _, e := range hist.Events {
		if a := e.GetActivityTaskFailedEventAttributes(); a != nil && strings.Contains(a.GetFailure().GetMessage(), "the next run that holds it rolls") {
			failed = true
		}
	}
	if !failed {
		t.Fatal("the recording holds no failed RollCommit")
	}
	replayHistory(t, f)
}
