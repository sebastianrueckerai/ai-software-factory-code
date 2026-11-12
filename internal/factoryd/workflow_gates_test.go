package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// gateHarness scripts the gates: steps records every gate, review,
// approval and merge call in order.
type gateHarness struct {
	*workflowHarness
	steps        []string
	approvals    []ApprovalInput
	merges       []string   // "pr commit approver"
	checkedHeads []string   // Merge's checkedHead per call
	mergeMet     [][]string // Merge's met conditions per call
	notRun       []string   // RecordNotRun's rows as "kind commit round reason"
	gate         PolicyGate
	gateSeq      []PolicyGate // in order, the last repeating; empty: gate
	policyErrs   []error      // GatePolicy's error per call, in order; nil or short = none
	policyCalls  int
	repro        []ReproResult       // in order, the last repeating
	criteria     []CriteriaResult    // in order, the last repeating; empty: no finding
	settlements  []HoldoutSettlement // in order, the last repeating; empty: settled
	review       Review
	reviews      []Review          // in order, the last repeating; empty: review
	earlier      [][]Review        // the earlier reviews each ModelReview call was given
	rounds       []int             // the round each ModelReview call was given
	reviewGates  []GateResults     // the gates each ModelReview call was given
	companions   [][]CompanionGate // in order, the last repeating; empty: no companion pinned
	mu           sync.Mutex        // what the mocks record: the review and the audit run beside other activities, and a cancelled call still finishes in the test env
	audits       []AuditResult     // in order, the last repeating; empty: clear
	auditErr     error
	auditGates   []GateResults   // the gates each GateAudit call was given
	auditRounds  []int           // the stages' round each GateAudit call was given
	screened     int             // Screen calls
	judged       []string        // JudgeScreening verdicts
	merge        []MergeResult   // in order, the last repeating
	mergeErrs    []error         // Merge's error per call, in order; nil or short = none
	rollout      []RolloutResult // in order, the last repeating; empty: confirmed, nothing live
	reported     []string        // ReportMergeFailure reasons
	exhausted    int             // ReportMergeRetriesExhausted calls
	// policyDelay is how long GatePolicy takes; reviewErr fails ModelReview
	policyDelay   time.Duration
	reviewErr     error
	reviewDelay   time.Duration
	auditDelay    time.Duration
	mergeDelay    time.Duration
	reproDelay    time.Duration
	gapDelay      time.Duration // how long AwaitSessionGap takes
	policyCommits []string
	policyPins    []int64
	started       time.Time
	requested     []time.Time // when each approval was requested, on the workflow's clock
}

// newGateHarness registers the gate mocks after setup adjusted the
// defaults: a normal class, no reproduction, an approving review, a
// merge that goes through.
func newGateHarness(t *testing.T, setup ...func(*gateHarness)) *gateHarness {
	return newGateHarnessWith(t, TaskWorkflow{Config: func() WorkflowConfig { return testWorkflowConfig }}, setup...)
}

// newGateHarnessWith is newGateHarness running wf.
func newGateHarnessWith(t *testing.T, wf TaskWorkflow, setup ...func(*gateHarness)) *gateHarness {
	g := &gateHarness{workflowHarness: newWorkflowHarness(t, wf),
		gate:   PolicyGate{Class: "normal", Outcome: policy.OutcomeRequiresApproval, PolicyVersion: 7, Changed: 2},
		repro:  []ReproResult{{}},
		review: Review{Verdict: "approve", Summary: "does what the brief asks"},
		merge:  []MergeResult{{Merged: true}},
	}
	for _, f := range setup {
		f(g)
	}
	g.started = g.env.Now()
	g.env.OnActivity(ActivityGatePolicy, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).After(g.policyDelay).Return(
		func(ctx context.Context, taskID string, pr int64, commit string, pin int64) (PolicyGate, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.policyPins = append(g.policyPins, pin)
			g.policyCommits = append(g.policyCommits, commit)
			g.steps = append(g.steps, "policy")
			g.policyCalls++
			if g.policyCalls <= len(g.policyErrs) && g.policyErrs[g.policyCalls-1] != nil {
				return PolicyGate{}, g.policyErrs[g.policyCalls-1]
			}
			if len(g.gateSeq) > 0 {
				return g.gateSeq[min(g.policyCalls, len(g.gateSeq))-1], nil
			}
			return g.gate, nil
		})
	g.env.OnActivity(ActivityGateMigration, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr int64, migrations []string) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.steps = append(g.steps, "migration "+strings.Join(migrations, ","))
			return nil
		})
	n := 0
	g.env.OnActivity(ActivityGateReproduction, mock.Anything, "t1", mock.Anything, mock.Anything).After(g.reproDelay).Return(
		func(ctx context.Context, taskID string, pr int64, commit string) (ReproResult, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			n++
			g.steps = append(g.steps, "repro "+commit)
			return g.repro[min(n, len(g.repro))-1], nil
		})
	k := 0
	g.env.OnActivity(ActivityGateCriteria, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr int64, commit string) (CriteriaResult, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			k++
			g.steps = append(g.steps, "criteria "+commit)
			if len(g.criteria) == 0 {
				return CriteriaResult{}, nil
			}
			return g.criteria[min(k, len(g.criteria))-1], nil
		})
	s := 0
	g.env.OnActivity(ActivityHoldoutSettlement, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pin, pr int64, commit string, reds []HoldoutGateResult) (HoldoutSettlement, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			s++
			g.steps = append(g.steps, "settle "+commit)
			if len(g.settlements) == 0 {
				return HoldoutSettlement{Settled: true}, nil
			}
			return g.settlements[min(s, len(g.settlements))-1], nil
		})
	g.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).After(g.reviewDelay).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, gates GateResults, earlier []Review, round int) (Review, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.steps = append(g.steps, "review "+commit)
			g.earlier, g.rounds = append(g.earlier, earlier), append(g.rounds, round)
			g.reviewGates = append(g.reviewGates, gates)
			if len(g.reviews) > 0 {
				return g.reviews[min(len(g.earlier), len(g.reviews))-1], g.reviewErr
			}
			return g.review, g.reviewErr
		})
	ci := 0
	g.env.OnActivity(ActivityGateCompanion, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) ([]CompanionGate, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			ci++
			if len(g.companions) == 0 {
				return nil, nil
			}
			return g.companions[min(ci, len(g.companions))-1], nil
		})
	a := 0
	g.env.OnActivity(ActivityGateAudit, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything).After(g.auditDelay).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, gates GateResults, round int) (AuditResult, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			a++
			g.steps = append(g.steps, "audit "+commit)
			g.auditRounds = append(g.auditRounds, round)
			g.auditGates = append(g.auditGates, gates)
			if len(g.audits) == 0 {
				return AuditResult{Verdict: AuditClear}, g.auditErr
			}
			return g.audits[min(a, len(g.audits))-1], g.auditErr
		})
	g.env.OnActivity(ActivityJudgeScreening, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, verdict string) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.judged = append(g.judged, verdict)
			return nil
		})
	g.env.OnActivity(ActivityRecordNotRun, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, kind string, pr int64, commit string, round int, reason string) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.notRun = append(g.notRun, fmt.Sprintf("%s %s %d %s", kind, commit, round, reason))
			return nil
		})
	g.env.OnActivity(ActivityRequestApproval, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID string, in ApprovalInput) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.requested = append(g.requested, g.env.Now())
			g.steps = append(g.steps, "approval "+in.Commit)
			g.approvals = append(g.approvals, in)
			return nil
		})
	m := 0
	g.env.OnActivity(ActivityMerge, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).After(g.mergeDelay).Return(
		func(ctx context.Context, taskID string, pr int64, commit, approver, checkedHead string, met []string) (MergeResult, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.checkedHeads = append(g.checkedHeads, checkedHead)
			g.mergeMet = append(g.mergeMet, met)
			m++
			if m <= len(g.mergeErrs) && g.mergeErrs[m-1] != nil {
				g.merges = append(g.merges, fmt.Sprintf("%d %s %s error", pr, commit, approver))
				return MergeResult{}, g.mergeErrs[m-1]
			}
			g.steps = append(g.steps, "merge")
			g.merges = append(g.merges, fmt.Sprintf("%d %s %s", pr, commit, approver))
			return g.merge[min(m, len(g.merge))-1], nil
		})
	g.env.OnActivity(ActivityAwaitSessionGap, mock.Anything, "t1").After(g.gapDelay).Return(
		func(ctx context.Context, taskID string) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.steps = append(g.steps, "gap")
			return nil
		})
	g.env.OnActivity(ActivityClearRollHold, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID string, merged bool) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.steps = append(g.steps, map[bool]string{false: "clear", true: "settle"}[merged])
			return nil
		})
	g.env.OnActivity(ActivityReportMergeRetriesExhausted, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr int64, commit, approver string) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.exhausted++
			return nil
		})
	g.env.OnActivity(ActivityReportMergeFailure, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr int64, commit, approver, reason string) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.reported = append(g.reported, reason)
			return nil
		})
	return g
}

// onScreen answers Screen with res.
func (g *gateHarness) onScreen(res ScreenResult) {
	g.env.OnActivity(ActivityScreen, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) (ScreenResult, error) {
			g.screened++
			return res, nil
		})
}

// Green checks go through the policy gate, the reproduction and the model
// review to an approval request; the console's approved signal merges as
// factory-merge at the gated head and finishes the task done.
// The bot's merge makes Forgejo send pr_merged, and closes the issue:
// those signals land after the decision and are drained, so the run ends
// without unhandled signals.
func TestWorkflowEndsWithoutUnhandledSignalsAfterItsMerge(t *testing.T) {
	g := newGateHarness(t, func(g *gateHarness) { g.mergeDelay = time.Minute })
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(time.Hour+10*time.Second, SignalPRMerged, TaskSignal{Author: "factory-merge", PR: 9})
	g.signalAfter(time.Hour+20*time.Second, SignalIssueClosed, TaskSignal{Author: "factory-merge"})
	if final := g.run(t); final != "done" || !slices.Contains(g.steps, "merge") {
		t.Fatalf("final = %q, steps %v", final, g.steps)
	}
	if strings.Contains(g.logs.String(), "unhandled signals") {
		t.Fatalf("log:\n%s", g.logs.String())
	}
}

func TestWorkflowGatesThenApprovalMerges(t *testing.T) {
	g := newGateHarness(t)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, g.finals)
	}
	if want := []string{"policy", "repro c1", "review c1", "criteria c1", "approval c1", "merge"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
	if !slices.Equal(g.merges, []string{"9 c1 sebastian"}) {
		t.Fatalf("merges = %v", g.merges)
	}
	in := g.approvals[0]
	if in.PR.Number != 9 || in.Gates.Policy.Class != "normal" || in.Review.Verdict != "approve" {
		t.Fatalf("approval input = %+v", in)
	}
	if !strings.Contains(g.finals[0][1], "factory-merge after sebastian's approval") {
		t.Fatalf("reason = %q", g.finals[0][1])
	}
}

// A companion pin the plan card carries holds an otherwise auto merge at
// its approval: the ask names the companion still behind its pin, and the
// chain poller's companion_landed signal wakes the card, which re-runs
// its gates from the first stage at the now-met pin and merges without a
// human.
func TestCompanionLandedWakesTheHeldApproval(t *testing.T) {
	pin, head := "82ab246"+strings.Repeat("0", 33), "fee3ff16"+strings.Repeat("1", 32)
	g := newGateHarness(t, autoGates, func(g *gateHarness) {
		g.companions = [][]CompanionGate{
			{{Repository: "sf/ai-software-factory-infra", Pin: pin, Head: head}},
			{{Repository: "sf/ai-software-factory-infra", Pin: pin, Head: pin, Met: true}},
		}
	})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalCompanionLanded, TaskSignal{})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if want := []string{"policy", "repro c1", "review c1", "criteria c1", "approval c1",
		"policy", "repro c1", "review c1", "criteria c1", "merge"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
	if len(g.approvals) != 1 || !slices.Contains(g.approvals[0].WhyHuman,
		"the companion sf/ai-software-factory-infra is still behind 82ab246, at fee3ff1") {
		t.Fatalf("approvals = %+v, want one ask naming the companion behind its pin", g.approvals)
	}
	if !slices.Equal(g.merges, []string{"9 c1 " + AutoApprover}) || len(g.autoNotes) != 1 {
		t.Fatalf("merges %q, notes %q: want one auto merge with its note", g.merges, g.autoNotes)
	}
	if len(g.reviewGates) == 0 || len(g.reviewGates[len(g.reviewGates)-1].Companions) != 1 {
		t.Fatalf("review gates = %+v, want the companion gates beside the review", g.reviewGates)
	}
}

// A head the agent moved after its checks passed is not refused: the
// workflow adopts the branch's new head, waits its checks and runs its
// gates there; the approval and the merge name the new head (2026-10-23:
// two tasks died of the policy gate's terminal refusal).
func TestWorkflowHoldsAMovedHeadAtItsGates(t *testing.T) {
	g := newGateHarness(t)
	g.onSessions(pushed("s1", "c1"))
	g.branchHead = "c2"
	g.policyErrs = []error{headMovedError("pull request #9's head moved to c2 after its checks passed on c1")}
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, g.finals)
	}
	if want := []string{"policy", "policy", "repro c2", "review c2", "criteria c2", "approval c2", "merge"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
	if !slices.Equal(g.policyCommits, []string{"c1", "c2"}) || !slices.Equal(g.merges, []string{"9 c2 sebastian"}) {
		t.Fatalf("policy commits %v, merges %v", g.policyCommits, g.merges)
	}
	if len(g.approvals) != 1 || g.approvals[0].Commit != "c2" {
		t.Fatalf("approvals = %+v, want one on c2", g.approvals)
	}
}

// A merge refused because the agent had moved the head past its gates
// voids the granted approval and asks again: the new head's checks are
// waited on and the second approval merges it (2026-10-23).
func TestWorkflowHoldsAMovedHeadAtItsMerge(t *testing.T) {
	g := newGateHarness(t)
	g.onSessions(pushed("s1", "c1"))
	g.branchHead = "c2"
	g.merge = []MergeResult{{MovedHead: true, Refusal: "pull request #9's head moved to c2 after its gates ran on c1"},
		{Merged: true}}
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(4*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, g.finals)
	}
	if !slices.Equal(g.merges, []string{"9 c1 sebastian", "9 c2 sebastian"}) {
		t.Fatalf("merges = %v", g.merges)
	}
	if len(g.approvals) != 2 || g.approvals[0].Commit != "c1" || g.approvals[1].Commit != "c2" {
		t.Fatalf("approvals = %+v, want one on c1 and one asked again on c2", g.approvals)
	}
}

// On the merge rule auto, the head the merge gate's own update made is
// held: its checks run, the old head asks no human, and the gate merges the
// new head on the rule, its red checks read as the queue's sync's.
func TestWorkflowAutoMergeHoldsAHeadTheGateMoved(t *testing.T) {
	g := newGateHarness(t, autoGates, func(g *gateHarness) {
		g.repro = []ReproResult{{}, {}}
		g.criteria = []CriteriaResult{{Stand: &CriteriaStand{IDs: []string{"size_bound"}}}, {Stand: &CriteriaStand{IDs: []string{"size_bound"}}}}
	})
	g.state.Source = SourcePlan
	g.onSessions(pushed("s1", "c1"))
	g.branchHead = "c2"
	g.onDiff(map[string]string{"c1": "d1", "c2": "d1"})
	g.merge = []MergeResult{{MovedHead: true, Refusal: "a head that moved after its gates: the pull request's head is c2, not the gated c1"}, {Merged: true}}
	g.onChecks(ChecksResult{State: ChecksSuccess})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if !slices.Equal(g.merges, []string{"9 c1 " + AutoApprover, "9 c2 " + AutoApprover}) || len(g.approvals) != 0 {
		t.Fatalf("merges %q, approvals %d; want the auto merge again on c2 and no human asked", g.merges, len(g.approvals))
	}
	if want := "policy, repro c1, review c1, criteria c1, merge, merge"; strings.Join(g.steps, ", ") != want {
		t.Fatalf("steps = %v, want %s: the synced head runs no gate again", g.steps, want)
	}
}

// Red checks on a moved head that changes what the gates judged unchanged
// read as the queue's sync's: the agent is told main's commits came in, with the failing check.
func TestWorkflowAutoMergeSyncedHeadRedChecksCarryTheQueuePrefix(t *testing.T) {
	g := newGateHarness(t, autoGates, func(g *gateHarness) {
		g.repro = []ReproResult{{}, {}, {}}
		g.criteria = []CriteriaResult{{Stand: &CriteriaStand{IDs: []string{"size_bound"}}}}
	})
	g.state.Source = SourcePlan
	g.onSessions(pushed("s1", "c1"), pushed("s2", "c3"))
	g.onDiff(map[string]string{"c1": "d1", "c2": "d1", "c3": "d1"})
	g.branchHead = "c2"
	g.merge = []MergeResult{{MovedHead: true, Refusal: "moved"}, {Merged: true}}
	red := ChecksResult{State: ChecksFailure, Findings: []string{"test X failed"}}
	g.onChecks(ChecksResult{State: ChecksSuccess}, red, red, ChecksResult{State: ChecksSuccess})
	g.onHeadChecks(ChecksResult{State: ChecksSuccess}, red, ChecksResult{State: ChecksSuccess})
	g.run(t)
	if len(g.sessions) < 2 || !strings.Contains(strings.Join(g.sessions[1].Findings, "\n"), queuePrefix+"test X failed") {
		t.Fatalf("sessions = %+v, want the second session's findings to carry %q", g.sessions, queuePrefix)
	}
}

// A head whose changed lines differ from the ones the gates judged (an
// agent's push after the gates) goes back through every gate, the review,
// the policy and the audit among them, on the merge rule auto, and never
// merges on the old head's verdicts.
func TestWorkflowAutoMergeVerifiesAHeadAnAgentMovedAfterItsGates(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, withAudit), autoGates, func(g *gateHarness) {
		g.repro = []ReproResult{{}, {}}
		g.criteria = []CriteriaResult{{Stand: &CriteriaStand{IDs: []string{"size_bound"}}}, {Stand: &CriteriaStand{IDs: []string{"size_bound"}}}}
	})
	g.state.Source = SourcePlan
	g.onSessions(pushed("s1", "c1"))
	g.branchHead = "c2"
	g.merge = []MergeResult{{MovedHead: true, Refusal: "a head that moved after its gates: the pull request's head is c2, not the gated c1"}, {Merged: true}}
	g.onChecks(ChecksResult{State: ChecksSuccess})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if !slices.Equal(g.merges, []string{"9 c1 " + AutoApprover, "9 c2 " + AutoApprover}) || len(g.approvals) != 0 {
		t.Fatalf("merges %q, approvals %d; want the auto merge again on c2 and no human asked", g.merges, len(g.approvals))
	}
	for _, step := range []string{"policy", "review c2", "audit c2", "criteria c2"} {
		if !slices.Contains(g.steps, step) {
			t.Fatalf("steps = %v, want %q: every gate judges the new head", g.steps, step)
		}
	}
	if g.policyCommits[len(g.policyCommits)-1] != "c2" {
		t.Fatalf("policy commits = %v, want the last on c2", g.policyCommits)
	}
}

// A run from before the change keeps its path: the moved head's refusal
// asks a human.
func TestWorkflowAutoMergeMovedHeadKeepsItsOldPathBeforeTheChange(t *testing.T) {
	g := newGateHarness(t, autoGates, func(g *gateHarness) {
		g.repro = []ReproResult{{}}
		g.criteria = []CriteriaResult{{Stand: &CriteriaStand{IDs: []string{"size_bound"}}}}
	})
	g.env.OnGetVersion("auto-moved-head", workflow.DefaultVersion, versionAutoMovedHead).Return(workflow.DefaultVersion)
	g.state.Source = SourcePlan
	g.onSessions(pushed("s1", "c1"))
	g.branchHead = "c2"
	g.merge = []MergeResult{{MovedHead: true, Refusal: "moved"}, {Merged: true}}
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(g.approvals) < 1 {
		t.Fatalf("approvals %d; want a human asked, as before the change", len(g.approvals))
	}
}

// onDiff answers DiffHash with the diff hash of each commit; a commit it
// does not name has an unreadable diff ("").
func (h *workflowHarness) onDiff(byCommit map[string]string) {
	h.env.OnActivity(ActivityDiffHash, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr int64, commit string) (string, error) { return byCommit[commit], nil })
}

// On the approval rule, a head an agent pushed after the gates is judged by
// every gate before a human is asked again: the second approval request
// carries the review of c2, not c1's. A force push that restores the gated
// changed lines (the same diff hash) runs its checks only, then asks again.
func TestWorkflowMovedHeadOnTheApprovalRuleIsJudgedAgainUnlessItsDiffIsTheGatedOne(t *testing.T) {
	for _, tc := range []struct {
		name  string
		diffs map[string]string
		want  string
	}{
		{"an agent's push", map[string]string{"c1": "d1", "c2": "d2"}, "policy, repro c1, review c1, criteria c1, approval c1, merge, policy, repro c2, review c2, criteria c2, approval c2, merge"},
		{"an unreadable diff", map[string]string{"c1": "d1"}, "policy, repro c1, review c1, criteria c1, approval c1, merge, policy, repro c2, review c2, criteria c2, approval c2, merge"},
		{"a force push restoring the gated diff", map[string]string{"c1": "d1", "c2": "d1"}, "policy, repro c1, review c1, criteria c1, approval c1, merge, approval c2, merge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateHarness(t)
			g.onSessions(pushed("s1", "c1"))
			g.branchHead = "c2"
			g.onDiff(tc.diffs)
			g.merge = []MergeResult{{MovedHead: true, Refusal: "moved"}, {Merged: true}}
			g.onChecks(ChecksResult{State: ChecksSuccess})
			g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
			g.signalAfter(4*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
			if final := g.run(t); final != "done" {
				t.Fatalf("final = %q, finals %v", final, g.finals)
			}
			if got := strings.Join(g.steps, ", "); got != tc.want {
				t.Fatalf("steps = %s\nwant    %s", got, tc.want)
			}
			if len(g.approvals) != 2 || g.approvals[1].Commit != "c2" {
				t.Fatalf("approvals = %+v, want a second request on c2", g.approvals)
			}
		})
	}
}

// Red checks on a moved head are the checks stage's findings: the agent
// gets them back, and its fix goes through the gates on the new head.
func TestWorkflowMovedHeadRedChecksGoToTheAgent(t *testing.T) {
	g := newGateHarness(t)
	g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	g.branchHead = "c2"
	failing := `check "pr / test": failure — 1 test failed (https://git.example/sf/r/actions/runs/95)`
	g.policyErrs = []error{headMovedError("pull request #9's head moved to c2 after its checks passed on c1")}
	g.onChecks(ChecksResult{State: ChecksFailure, Findings: []string{failing}}, ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || len(g.sessions) != 2 {
		t.Fatalf("final = %q, %d sessions, finals %v", final, len(g.sessions), g.finals)
	}
	if f := g.sessions[1].Findings; len(f) == 0 || f[0] != failing {
		t.Fatalf("second session's findings = %q, want the red checks on the moved head", f)
	}
	if len(g.approvals) != 1 || g.approvals[0].Commit != "c2" || !slices.Equal(g.merges, []string{"9 c2 sebastian"}) {
		t.Fatalf("approvals %+v, merges %v: no approval before the red head was fixed", g.approvals, g.merges)
	}
}

// The approval wait runs from the request to the approval: the approved
// signal's step carries the request's time. A signal before the request
// is ignored and carries none.
func TestWorkflowApprovalCarriesTheRequestsTime(t *testing.T) {
	g := newGateHarness(t)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	var approved []recordedStep
	for _, r := range g.recorded {
		if r.step == "signal" && r.detail["signal"] == SignalApproved {
			approved = append(approved, r)
		}
	}
	if len(approved) != 1 || len(g.requested) != 1 {
		t.Fatalf("approved steps %+v, requests %v", approved, g.requested)
	}
	if got, want := approved[0].detail["started"], g.requested[0].UTC().Format(time.RFC3339Nano); got != want {
		t.Fatalf("approved started = %v, want the request's %s", got, want)
	}
	if wait := approved[0].at.Sub(g.requested[0]); wait != time.Hour {
		t.Fatalf("the approval came %s after the request, want the signal's hour", wait)
	}
}

// Workflows started before versionGateStarted record the approval as
// before, without started.
func TestWorkflowBeforeGateStartedRecordsNoStart(t *testing.T) {
	g := newGateHarness(t)
	g.env.OnGetVersion("gate-started", workflow.DefaultVersion, versionGateStarted).Return(workflow.DefaultVersion)
	g.env.OnGetVersion("stages", workflow.DefaultVersion, versionStages).Return(workflow.DefaultVersion)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	for _, r := range g.recorded {
		if _, ok := r.detail["started"]; ok {
			t.Fatalf("step %s carries started: %v", r.step, r.detail)
		}
	}
}

// A forbidden risk class fails the task with the paths; no review, no
// approval, no merge.
func TestWorkflowForbiddenClassFailsTheTask(t *testing.T) {
	g := newGateHarness(t)
	g.gate = PolicyGate{Class: "forbidden", Outcome: policy.OutcomeForbidden, Paths: []string{".forgejo/workflows/pr.yml"}, PolicyVersion: 7}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})

	if final := g.run(t); final != "failed" {
		t.Fatalf("final = %q", final)
	}
	if !slices.Equal(g.steps, []string{"policy"}) || !strings.Contains(g.finals[0][1], ".forgejo/workflows/pr.yml") || !strings.Contains(g.finals[0][1], "policy version 7") {
		t.Fatalf("steps = %v, reason = %q", g.steps, g.finals[0][1])
	}
}

// Changed migrations are flagged before the reproduction; a PR without
// any gets no migration gate.
func TestWorkflowMigrationsAreFlagged(t *testing.T) {
	g := newGateHarness(t)
	g.gate.Class, g.gate.Migrations = "migration", []string{"internal/factoryd/migrations/0016_x.sql"}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if g.steps[1] != "migration internal/factoryd/migrations/0016_x.sql" || g.approvals[0].Gates.Policy.Migrations[0] != "internal/factoryd/migrations/0016_x.sql" {
		t.Fatalf("steps = %v", g.steps)
	}
}

// Before versionStages a failed reproduction sent the next attempt back
// to the agent on the stronger rung at once, like red checks.
func TestWorkflowBeforeStagesFailedReproductionIsFindings(t *testing.T) {
	g := newGateHarness(t)
	g.env.OnGetVersion("stages", workflow.DefaultVersion, versionStages).Return(workflow.DefaultVersion)
	g.repro = []ReproResult{{Ran: true, Command: "go test ./x", Exit: 1, Output: "--- FAIL: TestX"}, {Ran: true, Command: "go test ./x"}}
	g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if want := []string{"policy", "repro c1", "policy", "repro c2", "review c2", "approval c2", "merge"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
	second := g.sessions[1]
	if second.Model != "factory/big" || len(second.Findings) != 1 ||
		!strings.Contains(second.Findings[0], "exit 1") || !strings.Contains(second.Findings[0], "--- FAIL: TestX") || !strings.Contains(second.Findings[0], "go test ./x") {
		t.Fatalf("second attempt = %+v", second)
	}
}

// A cheat found by the policy gate goes back to the agent through the
// checks stage's rule, before the checks wait and the review start; the
// next session's findings name it.
func TestWorkflowCheatsGoBackToTheAgent(t *testing.T) {
	g := newGateHarness(t)
	g.gateSeq = []PolicyGate{
		{Class: "normal", Outcome: policy.OutcomeRequiresApproval, PolicyVersion: 7, Cheats: []string{"anti-cheat: x_test.go:11 adds a skip to a test: t.Skip(\"flaky\")"}},
		{Class: "normal", Outcome: policy.OutcomeRequiresApproval, PolicyVersion: 7},
	}
	g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if len(g.sessions) != 2 || len(g.sessions[1].Findings) != 1 || !strings.Contains(g.sessions[1].Findings[0], "adds a skip to a test") {
		t.Fatalf("sessions = %+v, want a second session holding the cheat", g.sessions)
	}
	if want := []string{"policy", "policy", "repro c2", "review c2", "criteria c2", "approval c2", "merge"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
}

// The images file's findings at the head go back to the agent as the
// cheats do: the next session's findings name them.
func TestWorkflowImagesFindingsGoBackToTheAgent(t *testing.T) {
	finding := `factory/images.yaml at the pull request's head: image "video": declares no tool: apt or upstream needs an entry, or the image equals the base image`
	g := newGateHarness(t)
	g.gateSeq = []PolicyGate{
		{Class: "normal", Outcome: policy.OutcomeRequiresApproval, PolicyVersion: 7, Images: []string{finding}},
		{Class: "normal", Outcome: policy.OutcomeRequiresApproval, PolicyVersion: 7},
	}
	g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if len(g.sessions) != 2 || !slices.Equal(g.sessions[1].Findings, []string{finding}) {
		t.Fatalf("sessions = %+v, want a second session holding the finding", g.sessions)
	}
}

// A reproduction whose sandbox could not start asks the human with its
// start escalation, before the staged gates and after them alike, and the
// answer runs the reproduction again on the same commit: no new session,
// and the review sees the run that started.
func TestWorkflowReproductionThatCannotStartAsksTheHuman(t *testing.T) {
	const reason = "the sandbox cannot start at egress probe, and no session can fix it: check the task namespace's worker-egress policy, " +
		"or the pod's uid, against the probe named below, then answer to run the reproduction again, or close the card.\n" +
		"start failed: egress probe: direct IP 1.1.1.1:443 answered: connected to 1.1.1.1:443"
	for name, staged := range map[string]bool{"staged": true, "before stages": false} {
		t.Run(name, func(t *testing.T) {
			g := newGateHarness(t, func(g *gateHarness) { g.repro = []ReproResult{{Escalate: reason}, {}} })
			if !staged {
				g.env.OnGetVersion("stages", workflow.DefaultVersion, versionStages).Return(workflow.DefaultVersion)
			}
			g.onSessions(pushed("s1", "c1"))
			g.onChecks(ChecksResult{State: ChecksSuccess})
			g.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the policy is back", Comment: 700})
			g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

			if final := g.run(t); final != "done" {
				t.Fatalf("final = %q, finals %v", final, g.finals)
			}
			if len(g.sessions) != 1 || !slices.Equal(g.escalated, []string{reason}) {
				t.Fatalf("sessions %d, escalations %q; want one session and the start escalation", len(g.sessions), g.escalated)
			}
			want := []string{"policy", "repro c1", "repro c1", "review c1", "approval c1", "merge"}
			if staged { // the criteria gate rides the approval stage
				want = slices.Insert(want, 4, "criteria c1")
			}
			if !slices.Equal(g.steps, want) {
				t.Fatalf("steps = %v, want %v", g.steps, want)
			}
			if got := g.reviewGates[0].Reproduction; got != (ReproResult{}) {
				t.Fatalf("the review saw the reproduction %+v, want the run that started", got)
			}
		})
	}
}

// A merge Forgejo refuses keeps the wait; the next approval merges.
func TestWorkflowRefusedMergeKeepsWaiting(t *testing.T) {
	g := newGateHarness(t)
	g.merge = []MergeResult{{Refusal: "405 not approved"}, {Merged: true}}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" || len(g.merges) != 2 {
		t.Fatalf("final = %q after merges %v", final, g.merges)
	}
	// the refusal starts a new approval wait: the second approval's
	// started is the first one's hour later
	var started []string
	for _, r := range g.recorded {
		if r.step == "signal" && r.detail["signal"] == SignalApproved {
			started = append(started, fmt.Sprint(r.detail["started"]))
		}
	}
	req := g.requested[0].UTC()
	if want := []string{req.Format(time.RFC3339Nano), req.Add(time.Hour).Format(time.RFC3339Nano)}; !slices.Equal(started, want) {
		t.Fatalf("approvals started %q, want %q", started, want)
	}
}

// A merge refused because main moved under the pull request (a conflict)
// goes back to the agent with the instruction to merge main into the
// branch; the approval of the new head merges.
func TestWorkflowMergeConflictGoesBackToTheAgent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*gateHarness)
		steps []string
	}{
		{name: "approval", setup: func(*gateHarness) {},
			steps: []string{"policy", "repro c1", "review c1", "criteria c1", "approval c1", "merge", "policy", "repro c2", "review c2", "criteria c2", "approval c2", "merge"}},
		{name: "auto", setup: autoGates,
			steps: []string{"policy", "repro c1", "review c1", "criteria c1", "merge", "policy", "repro c2", "review c2", "criteria c2", "merge"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateHarness(t, tc.setup)
			g.merge = []MergeResult{{Refusal: "forgejo POST .../merge: status 405: Please try again later", Conflict: true}, {Merged: true}}
			g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
			g.onChecks(ChecksResult{State: ChecksSuccess})
			g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
			g.signalAfter(3*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

			if final := g.run(t); final != "done" || len(g.merges) != 2 {
				t.Fatalf("final = %q after merges %v", final, g.merges)
			}
			if !slices.Equal(g.steps, tc.steps) {
				t.Fatalf("steps = %v, want %v", g.steps, tc.steps)
			}
			if f := g.sessions[1].Findings; len(f) != 1 || !strings.Contains(f[0], "no longer merges into main") ||
				!strings.Contains(f[0], "git merge origin/main") || !strings.Contains(f[0], "status 405") {
				t.Fatalf("second session's findings = %q", f)
			}
		})
	}
}

// A red head is no conflict: under an approval the merge's own guard
// keeps the wait and a human's change request sends the agent back with
// it. Before that, the approval stage's own guard never lets a red head
// reach a card or a merge: on the rule auto it sends the failing checks
// back to the agent itself. The agent never gets the merge-main finding.
func TestWorkflowRedHeadWaitsForTheHuman(t *testing.T) {
	const line = "the checks are red on c1: the merge waits for a green head"
	red := MergeResult{Refusal: line, RedHead: true}
	failing := `check "pr / test": failure — 1 test failed (https://git.example/sf/r/actions/runs/95)`
	for _, tc := range []struct {
		name     string
		setup    func(*gateHarness)
		merge    []MergeResult
		heads    []ChecksResult // HeadChecks' answers; nil: green every read
		findings string         // the second session's first finding
		merges   []string
	}{
		{name: "approval", setup: func(*gateHarness) {}, merge: []MergeResult{red, {Merged: true}},
			findings: "reviewer sebastian requested changes: the checks are red: fix them",
			merges:   []string{"9 c1 sebastian", "9 c2 sebastian"}},
		{name: "auto", setup: autoGates, merge: []MergeResult{{Merged: true}},
			heads:    []ChecksResult{{State: ChecksFailure, Findings: []string{failing}}, {State: ChecksSuccess}},
			findings: failing,
			merges:   []string{"9 c2 " + AutoApprover}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateHarness(t, tc.setup)
			g.merge = tc.merge
			g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
			g.onChecks(ChecksResult{State: ChecksSuccess})
			if tc.heads != nil {
				g.onHeadChecks(tc.heads...)
			}
			g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
			g.signalAfter(2*time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", PR: 9, Body: "the checks are red: fix them"})
			g.signalAfter(4*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

			if final := g.run(t); final != "done" || len(g.sessions) != 2 {
				t.Fatalf("final = %q, %d sessions, merges %v", final, len(g.sessions), g.merges)
			}
			if f := g.sessions[1].Findings; len(f) == 0 || f[0] != tc.findings || strings.Contains(strings.Join(f, "\n"), "no longer merges into main") {
				t.Fatalf("second session's findings = %q, want %q and no merge-main finding", f, tc.findings)
			}
			if !slices.Equal(g.merges, tc.merges) {
				t.Fatalf("merges = %v, want %v", g.merges, tc.merges)
			}
			if tc.name == "auto" && (len(g.approvals) != 0 || !slices.Equal(g.autoNotes, []string{"9 c2"})) {
				t.Fatalf("approvals %+v, notes %v: no card and no merge attempt on the red head c1", g.approvals, g.autoNotes)
			}
		})
	}
}

// Before the change id the conflict is a refusal: the wait goes on.
func TestWorkflowBeforeMergeConflictKeepsWaiting(t *testing.T) {
	g := newGateHarness(t)
	g.env.OnGetVersion("merge-conflict", workflow.DefaultVersion, versionMergeConflict).Return(workflow.DefaultVersion)
	g.merge = []MergeResult{{Refusal: "405", Conflict: true}, {Merged: true}}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" || len(g.merges) != 2 || len(g.sessions) != 1 {
		t.Fatalf("final = %q after merges %v and %d sessions", final, g.merges, len(g.sessions))
	}
}

// A refused merge followed by a human's merge in Forgejo stays valid.
func TestWorkflowHumanMergeAfterARefusalIsDone(t *testing.T) {
	g := newGateHarness(t)
	g.merge = []MergeResult{{Refusal: "405"}}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" || g.finals[0][1] != "pull request merged" || len(g.merges) != 1 {
		t.Fatalf("final = %q (%v) after merges %v", final, g.finals, g.merges)
	}
}

// An approval for another pull request, or one sent before the approval
// request was opened, merges nothing.
func TestWorkflowIgnoresStrayApprovals(t *testing.T) {
	g := newGateHarness(t, func(g *gateHarness) { g.policyDelay = time.Hour })
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(30*time.Minute, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 8})
	g.signalAfter(3*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := g.run(t); final != "closed" || len(g.merges) != 0 {
		t.Fatalf("final = %q after merges %v", final, g.merges)
	}
}

// A change request during the approval wait starts a new attempt; its
// head goes through the gates again.
func TestWorkflowChangesDuringApprovalStartANewAttempt(t *testing.T) {
	g := newGateHarness(t)
	g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", Body: "rename it", PR: 9})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if !slices.Equal(g.merges, []string{"9 c2 sebastian"}) || len(g.approvals) != 2 {
		t.Fatalf("merges = %v, approvals = %d", g.merges, len(g.approvals))
	}
	if f := g.sessions[1].Findings; len(f) != 1 || f[0] != "reviewer sebastian requested changes: rename it" {
		t.Fatalf("findings = %v", f)
	}
}

// A model review that errors is a note on the card, not the task's end.
func TestWorkflowFailedModelReviewIsANote(t *testing.T) {
	g := newGateHarness(t, func(g *gateHarness) { g.reviewErr = terminalError("litellm down") })
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if note := g.approvals[0].Review.Note; !strings.Contains(note, "litellm down") {
		t.Fatalf("review note = %q", note)
	}
}

// Workflows started before versionGates keep the plain review wait: no
// gate runs, an approval signal does nothing, a human's merge finishes.
func TestWorkflowBeforeGatesRunsNoGate(t *testing.T) {
	g := newGateHarness(t)
	g.env.OnGetVersion("gates", workflow.DefaultVersion, versionGates).Return(workflow.DefaultVersion)
	g.env.OnGetVersion("stages", workflow.DefaultVersion, versionStages).Return(workflow.DefaultVersion)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" || len(g.steps) != 0 {
		t.Fatalf("final = %q, steps = %v", final, g.steps)
	}
}

// GatePolicy gets the gated commit (versionGatesPinned).
func TestWorkflowGatePolicyGetsTheGatedCommit(t *testing.T) {
	g := newGateHarness(t)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || !slices.Equal(g.policyCommits, []string{"c1"}) {
		t.Fatalf("final = %q, GatePolicy commits = %q", final, g.policyCommits)
	}
}

// A terminal signal during the model review decides at once: no approval
// request, no waiting for the review.
func TestWorkflowTerminalSignalDuringTheReviewDecides(t *testing.T) {
	g := newGateHarness(t, func(g *gateHarness) { g.reviewDelay = 3 * time.Hour })
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(10*time.Minute, SignalPRClosed, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "failed" || len(g.approvals) != 0 {
		t.Fatalf("final = %q, approvals %d", final, len(g.approvals))
	}
	if g.finishAt.Sub(g.started) > time.Hour {
		t.Fatalf("finished %s after the start, want right after the close", g.finishAt.Sub(g.started))
	}
}

// A merge that failed (not refused) keeps the approval: it is tried again
// after mergeRetryAfter with the same approver, without a new signal.
func TestWorkflowFailedMergeIsRetriedWithTheSameApproval(t *testing.T) {
	g := newGateHarness(t)
	g.merge = []MergeResult{{Failed: "forgejo 502"}, {Merged: true}}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || !slices.Equal(g.merges, []string{"9 c1 sebastian", "9 c1 sebastian"}) {
		t.Fatalf("final = %q, merges = %v", final, g.merges)
	}
}

// A human's merge found by Merge finishes the task credited to them.
func TestWorkflowMergeFoundDoneByAHuman(t *testing.T) {
	g := newGateHarness(t)
	g.merge = []MergeResult{{Merged: true, By: "sebastian"}}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "alice", PR: 9})
	if final := g.run(t); final != "done" || g.finals[0][1] != "pull request merged by sebastian" {
		t.Fatalf("final = %q (%v)", final, g.finals)
	}
}

// A Merge that ends in an error (a 404, its last attempt timing out) is
// reported and tried again after mergeRetryAfter with the same approval
// (versionMergeFailureReported).
func TestWorkflowMergeErrorIsReportedAndRetried(t *testing.T) {
	g := newGateHarness(t, func(g *gateHarness) { g.mergeErrs = []error{terminalError("forgejo: pull request not found")} })
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || !slices.Equal(g.merges, []string{"9 c1 sebastian error", "9 c1 sebastian"}) {
		t.Fatalf("final = %q, merges = %v", final, g.merges)
	}
	if len(g.reported) != 1 || !strings.Contains(g.reported[0], "pull request not found") {
		t.Fatalf("reported = %q", g.reported)
	}
}

// Before versionMergeFailureReported a Merge error only waits for the next
// approval: nothing is reported, no retry is scheduled.
func TestWorkflowMergeErrorBeforeReportingWaits(t *testing.T) {
	g := newGateHarness(t, func(g *gateHarness) { g.mergeErrs = []error{terminalError("gone")} })
	g.env.OnGetVersion("merge-failure-reported", workflow.DefaultVersion, versionMergeFailureReported).Return(workflow.DefaultVersion)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(5*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || len(g.reported) != 0 || g.finishAt.Sub(g.started) < 5*time.Hour {
		t.Fatalf("final = %q, reported %v, finished after %s", final, g.reported, g.finishAt.Sub(g.started))
	}
}

// A newer approval while a retry is pending merges at once and cancels
// the retry timer, which then never merges again.
func TestWorkflowNewApprovalReplacesThePendingRetry(t *testing.T) {
	g := newGateHarness(t)
	g.merge = []MergeResult{{Failed: "forgejo 502"}, {Merged: true}}
	var canceled []string
	g.env.SetOnTimerCanceledListener(func(id string) { canceled = append(canceled, id) })
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(time.Hour+time.Minute, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || len(g.merges) != 2 || len(canceled) != 1 {
		t.Fatalf("final = %q, merges = %v, cancelled timers %v", final, g.merges, canceled)
	}
	if d := g.finishAt.Sub(g.started); d > time.Hour+5*time.Minute {
		t.Fatalf("finished %s after the start, want right after the second approval", d)
	}
}

// Merge retries are bounded: the first failure and six
// retries, one hour, then one exhausted report and no timer; the task
// waits for a signal. A new approval restarts the merge with a fresh count.
func TestWorkflowMergeRetriesAreCapped(t *testing.T) {
	g := newGateHarness(t)
	g.merge = []MergeResult{{Failed: "1"}, {Failed: "2"}, {Failed: "3"}, {Failed: "4"}, {Failed: "5"}, {Failed: "6"}, {Failed: "7"}, {Merged: true}}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(10*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || len(g.merges) != 8 || g.exhausted != 1 {
		t.Fatalf("final = %q, merges %d, exhausted reports %d; want 7 failed attempts, one report, then the approval's merge", final, len(g.merges), g.exhausted)
	}
	if d := g.finishAt.Sub(g.started); d < 10*time.Hour {
		t.Fatalf("finished %s after the start: a timer merged before the new approval", d)
	}
	// the exhausted retries start a new approval wait: the seventh
	// failed merge, 1 h + 6 x 10 min after the request
	var started []string
	for _, r := range g.recorded {
		if r.step == "signal" && r.detail["signal"] == SignalApproved {
			started = append(started, fmt.Sprint(r.detail["started"]))
		}
	}
	req := g.requested[0].UTC()
	if want := []string{req.Format(time.RFC3339Nano), req.Add(2 * time.Hour).Format(time.RFC3339Nano)}; !slices.Equal(started, want) {
		t.Fatalf("approvals started %q, want %q", started, want)
	}
}

// A run from before the cap keeps retrying (versionMergeRetryCap).
func TestWorkflowMergeRetriesBeforeTheCap(t *testing.T) {
	g := newGateHarness(t)
	g.env.OnGetVersion("merge-retry-cap", workflow.DefaultVersion, versionMergeRetryCap).Return(workflow.DefaultVersion)
	g.merge = []MergeResult{{Failed: "1"}, {Failed: "2"}, {Failed: "3"}, {Failed: "4"}, {Failed: "5"}, {Failed: "6"}, {Failed: "7"}, {Merged: true}}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || len(g.merges) != 8 || g.exhausted != 0 {
		t.Fatalf("final = %q, merges %d, exhausted %d", final, len(g.merges), g.exhausted)
	}
}

// An approval's start is used once: a second approval while the failed
// merge's retry is pending ends no new approval wait, so it carries none.
func TestWorkflowApprovalDuringAMergeRetryCarriesNoStart(t *testing.T) {
	g := newGateHarness(t)
	g.merge = []MergeResult{{Failed: "forgejo 502"}, {Merged: true}}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(time.Hour+5*time.Minute, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	var started []string
	for _, r := range g.recorded {
		if r.step == "signal" && r.detail["signal"] == SignalApproved {
			s, _ := r.detail["started"].(string)
			started = append(started, s)
		}
	}
	if want := []string{g.requested[0].UTC().Format(time.RFC3339Nano), ""}; !slices.Equal(started, want) {
		t.Fatalf("approvals started %q, want %q", started, want)
	}
}

// An issue closed during the approval wait ends the wait: its signal step
// carries the request's time, two hours before the close.
func TestWorkflowIssueCloseEndsTheApprovalWait(t *testing.T) {
	g := newGateHarness(t)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.env.OnActivity(ActivityPullRequestMerged, mock.Anything, "t1", mock.Anything).Return(false, nil)
	g.signalAfter(2*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	if final := g.run(t); final != "closed" {
		t.Fatalf("final = %q", final)
	}
	var closed []recordedStep
	for _, r := range g.recorded {
		if r.step == "signal" && r.detail["signal"] == SignalIssueClosed {
			closed = append(closed, r)
		}
	}
	if len(closed) != 1 || closed[0].detail["started"] != g.requested[0].UTC().Format(time.RFC3339Nano) {
		t.Fatalf("issue_closed steps %+v, want one carrying the request's %s", closed, g.requested[0])
	}
	if wait := closed[0].at.Sub(g.requested[0]); wait != 2*time.Hour {
		t.Fatalf("the wait lasted %s, want 2h", wait)
	}
}

// A review the gateway refused at the task's budget cap is the approval
// card with the refusal as the review's note: nothing merges until a human
// approves, and a closed issue ends the task unmerged.
func TestWorkflowRefusedReviewIsTheCardNeverAMerge(t *testing.T) {
	note := "model review refused by the gateway (refused_budget): free headroom 0.000000 EUR (task 0.000000, day 12.000000)"
	g := newGateHarness(t, func(g *gateHarness) { g.review = Review{Note: note} })
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.env.OnActivity(ActivityPullRequestMerged, mock.Anything, "t1", mock.Anything).Return(false, nil)
	g.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	if final := g.run(t); final != "closed" {
		t.Fatalf("final = %q", final)
	}
	if len(g.approvals) != 1 || g.approvals[0].Review.Note != note || g.approvals[0].Review.Verdict != "" {
		t.Fatalf("approvals = %+v, want one card carrying the refusal", g.approvals)
	}
	if len(g.merges) != 0 {
		t.Fatalf("merges = %v, want none", g.merges)
	}
	if strings.Join(g.steps, ", ") != "policy, repro c1, review c1, criteria c1, approval c1" {
		t.Fatalf("steps = %v", g.steps)
	}
}

// regateHarness is a plan card's gate harness whose review the month cap
// refused, then reviews in order, with Unjudged kinds per call, each the
// last repeating; holds records HoldMerge's calls.
func regateHarness(t *testing.T, kinds []Unjudged, reviews ...Review) (g *gateHarness, holds *[]string) {
	return regateHarnessWith(t, TaskWorkflow{Config: func() WorkflowConfig { return testWorkflowConfig }}, kinds, reviews...)
}

// regateHarnessWith is regateHarness running wf.
func regateHarnessWith(t *testing.T, wf TaskWorkflow, kinds []Unjudged, reviews ...Review) (g *gateHarness, holds *[]string) {
	g = newGateHarnessWith(t, wf, func(g *gateHarness) {
		g.reviews = append([]Review{{Note: refusedMonthNote}}, reviews...)
	})
	g.state.Source = SourcePlan
	holds = &[]string{}
	k := 0
	g.env.OnActivity(ActivityUnjudged, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) (Unjudged, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			k++
			if len(kinds) == 0 {
				return Unjudged{}, nil
			}
			return kinds[min(k, len(kinds))-1], nil
		})
	g.env.OnActivity(ActivityHoldMerge, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr int64, commit, approver string) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.steps = append(g.steps, "hold")
			*holds = append(*holds, fmt.Sprintf("%d %s %s", pr, commit, approver))
			return nil
		})
	return g, holds
}

const refusedMonthNote = "model review refused by the gateway (refused_month_cap): month headroom 0.00 EUR"

// opened is the criteria gate's finding for a blind proof that ran red.
const opened = "criterion size_bound: its blind proof ran red at c1. It is open now."

// whyHolds: some line of in's why holds s.
func whyHolds(in ApprovalInput, s string) bool {
	return slices.ContainsFunc(in.WhyHuman, func(l string) bool { return strings.Contains(l, s) })
}

// The criteria gate's findings go back to the agent as a stage's do: the
// next session holds them, and the card asks on the head whose gate found
// nothing, never naming them.
func TestWorkflowGateFindingsGoBackToTheAgent(t *testing.T) {
	g := newGateHarness(t, func(g *gateHarness) { g.criteria = []CriteriaResult{{Findings: []string{opened}}, {}} })
	g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(g.sessions) != 2 || !slices.Contains(g.sessions[1].Findings, opened) {
		t.Fatalf("sessions %d, the second's findings %q: want the gate's finding", len(g.sessions), g.sessions[len(g.sessions)-1].Findings)
	}
	if len(g.approvals) != 1 || whyHolds(g.approvals[0], opened) {
		t.Fatalf("approvals %+v: want one, without the gate's finding", g.approvals)
	}
	if want := "policy, repro c1, review c1, criteria c1, policy, repro c2, review c2, criteria c2, approval c2, merge"; strings.Join(g.steps, ", ") != want {
		t.Fatalf("steps = %v, want %s", g.steps, want)
	}
}

// At the session cap the gate's findings ride the approval card with the
// cap's line, as a stage's do.
func TestWorkflowGateFindingsRideTheCardAtTheCap(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(1, nil), func(g *gateHarness) { g.criteria = []CriteriaResult{{Findings: []string{opened}}} })
	g.beforeLastRoundFindings()
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(g.sessions) != 1 || len(g.approvals) != 1 || !whyHolds(g.approvals[0], opened) || !whyHolds(g.approvals[0], capReached) {
		t.Fatalf("sessions %d, approvals %+v: want one session and the card with the finding at the cap", len(g.sessions), g.approvals)
	}
}

// The gate's card findings, which no session can settle, ride the card
// and send the agent nothing.
func TestWorkflowGateCardFindingsRideTheCard(t *testing.T) {
	const late = "Criterion no_proof has no stored proof: the prover filed before a later revision added it."
	g := newGateHarness(t, func(g *gateHarness) { g.criteria = []CriteriaResult{{Card: []string{late}}} })
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(g.sessions) != 1 || len(g.approvals) != 1 || !slices.Contains(g.approvals[0].WhyHuman, late) {
		t.Fatalf("sessions %d, approvals %+v: want one session and the late line on the card", len(g.sessions), g.approvals)
	}
}

// On the merge rule auto a plan card merges on the gate's standing rows:
// the gate's approval names them, and no card asks.
func TestWorkflowAutoMergesOnTheGatesRows(t *testing.T) {
	g := newGateHarness(t, autoGates, func(g *gateHarness) {
		g.repro = []ReproResult{{}}
		g.criteria = []CriteriaResult{{Stand: &CriteriaStand{IDs: []string{"size_bound"}}}}
	})
	g.state.Source = SourcePlan
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if !slices.Equal(g.merges, []string{"9 c1 " + AutoApprover}) || len(g.approvals) != 0 ||
		len(g.mergeMet) != 1 || !slices.Contains(g.mergeMet[0], "the plan's criteria all stand met or waived: size_bound") {
		t.Fatalf("merges %q, approvals %d, met %q: want the auto merge on the gate's rows", g.merges, len(g.approvals), g.mergeMet)
	}
}

// A press on an integration card whose never-judged criteria are the
// gate's runs the gate again, no model review: a gate that now stands
// every criterion merges on the one press, the stand riding the merge.
func TestWorkflowPressRunsTheGateAgainForItsCriteria(t *testing.T) {
	g, holds := regateHarness(t, []Unjudged{{Gate: []string{"size_bound"}}, {}})
	g.criteria = []CriteriaResult{{Stand: &CriteriaStand{IDs: []string{"size_bound"}, Open: []string{"size_bound"}}}, {Stand: &CriteriaStand{IDs: []string{"size_bound"}}}}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if want := "policy, repro c1, review c1, criteria c1, approval c1, criteria c1, merge"; strings.Join(g.steps, ", ") != want {
		t.Fatalf("steps = %v, want %s", g.steps, want)
	}
	if !slices.Equal(g.merges, []string{"9 c1 sebastian"}) || len(*holds) != 0 {
		t.Fatalf("merges %v, holds %v: want the merge and no hold", g.merges, *holds)
	}
	if len(g.mergeMet) != 1 || !slices.Contains(g.mergeMet[0], "the plan's criteria all stand met or waived: size_bound") {
		t.Fatalf("merge met %q: want the gate's stand riding the merge", g.mergeMet)
	}
}

// The gate run again on a press sends its findings back to the agent,
// with no merge; at the cap they ride a new merge ask, which the card
// waits on.
func TestWorkflowPressSendsTheGatesFindingsBack(t *testing.T) {
	after := func(steps []string, step string) []string { return steps[slices.Index(steps, step)+1:] }
	t.Run("to the agent", func(t *testing.T) {
		g, holds := regateHarness(t, []Unjudged{{Gate: []string{"size_bound"}}, {}})
		g.criteria = []CriteriaResult{{}, {Findings: []string{opened}}, {}}
		g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
		g.onChecks(ChecksResult{State: ChecksSuccess})
		g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		g.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
		if final := g.run(t); final != "closed" {
			t.Fatalf("final = %q, finals %v", final, g.finals)
		}
		if len(g.sessions) != 2 || !slices.Contains(g.sessions[1].Findings, opened) {
			t.Fatalf("sessions %d, the last's findings %q: want the gate's finding in the second", len(g.sessions), g.sessions[len(g.sessions)-1].Findings)
		}
		if rest := after(g.steps, "approval c1"); len(rest) < 2 || rest[0] != "criteria c1" || rest[1] != "policy" || len(g.merges) != 0 || len(*holds) != 0 {
			t.Fatalf("steps %v, merges %v, holds %v: want the gate again, then the second session, and no merge", g.steps, g.merges, *holds)
		}
	})
	t.Run("at the cap", func(t *testing.T) {
		g, holds := regateHarnessWith(t, stagesWorkflow(1, nil), []Unjudged{{Gate: []string{"size_bound"}}, {}})
		g.beforeLastRoundFindings()
		g.criteria = []CriteriaResult{{}, {Findings: []string{opened}}}
		g.onSessions(pushed("s1", "c1"))
		g.onChecks(ChecksResult{State: ChecksSuccess})
		g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
		g.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
		if final := g.run(t); final != "closed" {
			t.Fatalf("final = %q, finals %v", final, g.finals)
		}
		if len(g.sessions) != 1 || len(g.merges) != 0 || len(*holds) != 0 {
			t.Fatalf("sessions %d, merges %v, holds %v: want no second session, no merge, no hold", len(g.sessions), g.merges, *holds)
		}
		if len(g.approvals) != 2 || !whyHolds(g.approvals[1], opened) || !whyHolds(g.approvals[1], capReached) || g.approvals[1].Commit != "c1" {
			t.Fatalf("approvals %+v: want the merge asked again on c1 with the finding at the cap", g.approvals)
		}
		if rest := after(g.steps, "approval c1"); strings.Join(rest, ", ") != "criteria c1, approval c1" {
			t.Fatalf("steps %v: want the gate again, then the ask", g.steps)
		}
	})
}

// A gate run again on a press whose lines are the card's alone, a
// criterion the prover never proved among them, asks the merge again
// with them beside the earlier card's lines and reviews: the human sees
// them before any merge. An approval sent while the gate ran, here while
// it waited on the human's answer, is no approval of the new ask.
func TestWorkflowPressFilesTheGatesCardLines(t *testing.T) {
	const late = "Criterion no_proof has no stored proof: the prover filed before a later revision added it."
	g, holds := regateHarness(t, []Unjudged{{Gate: []string{"no_proof"}}, {}})
	g.criteria = []CriteriaResult{{}, {Escalate: proofsNotStored}, {Card: []string{late}}}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(90*time.Minute, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the prover filed", Comment: 700})
	g.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	if final := g.run(t); final != "closed" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(g.merges) != 0 || len(*holds) != 0 || len(g.approvals) != 2 {
		t.Fatalf("merges %v, holds %v, approvals %d: want the ask again and no merge", g.merges, *holds, len(g.approvals))
	}
	first, again := g.approvals[0], g.approvals[1]
	if again.Commit != "c1" || !slices.Contains(again.WhyHuman, late) || !reflect.DeepEqual(again.EarlierReviews, first.EarlierReviews) ||
		!reflect.DeepEqual(again.Review, first.Review) {
		t.Fatalf("the ask again = %+v, want the late line with the first ask's reviews", again)
	}
	for _, l := range first.WhyHuman {
		if !slices.Contains(again.WhyHuman, l) {
			t.Fatalf("the ask again's why %q drops the first card's %q", again.WhyHuman, l)
		}
	}
	if !slices.ContainsFunc(g.recorded, func(r recordedStep) bool {
		return r.step == "signal" && r.detail["ignored"] == "sent before this approval request"
	}) {
		t.Fatalf("recorded %+v: want the approval sent during the gate ignored", g.recorded)
	}
}

// The ask filed again keeps why a human decides, as AutoMerge reads the
// gates now: the merge rule and the risk class beside the gate's lines.
func TestWorkflowPressAskAgainKeepsWhyAHuman(t *testing.T) {
	const late = "Criterion no_proof has no stored proof: the prover filed before a later revision added it."
	g, _ := regateHarness(t, []Unjudged{{Gate: []string{"no_proof"}}, {}})
	g.reviews = nil
	g.criteria = []CriteriaResult{{}, {Card: []string{late}}}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	if final := g.run(t); final != "closed" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	const rule, class = "the project's merge rule is human", "risk class normal is requires_approval"
	if len(g.approvals) != 2 || !slices.Contains(g.approvals[0].WhyHuman, rule) {
		t.Fatalf("approvals %+v: want two, the first naming the merge rule", g.approvals)
	}
	if why := g.approvals[1].WhyHuman; !slices.Contains(why, late) || !slices.Contains(why, rule) || !slices.Contains(why, class) {
		t.Fatalf("the ask again's why = %q, want the late line, the merge rule and the risk class", why)
	}
}

// A judgement call that fails again on the press leaves its criterion
// without a row: the merge asks again with the card's line naming the
// failure and holds, the gate's to settle on the next press.
func TestWorkflowPressHoldsAJudgementTheCallLeftOpen(t *testing.T) {
	const failed = "criterion reads_calm: the judgement over reads_calm.html at c1 gave no verdict: timed out"
	g, holds := regateHarness(t, []Unjudged{{Gate: []string{"reads_calm"}}})
	g.criteria = []CriteriaResult{{Card: []string{failed}}}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	if final := g.run(t); final != "closed" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if want := "policy, repro c1, review c1, criteria c1, approval c1, criteria c1, approval c1, hold"; strings.Join(g.steps, ", ") != want {
		t.Fatalf("steps = %v, want %s", g.steps, want)
	}
	if !slices.Equal(*holds, []string{"9 c1 sebastian"}) || len(g.merges) != 0 || !slices.Contains(g.approvals[1].WhyHuman, failed) {
		t.Fatalf("holds %v, merges %v, approvals %+v: want the ask with the failure and the hold", *holds, g.merges, g.approvals)
	}
}

// A gate run again that still leaves a criterion of its own without a
// row holds the merge.
func TestWorkflowPressHoldsAGateCriterionLeftWithoutARow(t *testing.T) {
	g, holds := regateHarness(t, []Unjudged{{Gate: []string{"size_bound"}}})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	if final := g.run(t); final != "closed" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if want := "policy, repro c1, review c1, criteria c1, approval c1, criteria c1, hold"; strings.Join(g.steps, ", ") != want {
		t.Fatalf("steps = %v, want %s", g.steps, want)
	}
	if !slices.Equal(*holds, []string{"9 c1 sebastian"}) || len(g.merges) != 0 {
		t.Fatalf("holds %v, merges %v: want the one hold", *holds, g.merges)
	}
}

// The gate run again on a press may ask the human, as while the plan's
// prover may still store a proof: the answer runs it again, and a gate
// that then settles every criterion merges.
func TestWorkflowPressAsksThroughTheGatesEscalation(t *testing.T) {
	g, holds := regateHarness(t, []Unjudged{{Gate: []string{"size_bound"}}, {}})
	g.criteria = []CriteriaResult{{}, {Escalate: proofsNotStored}, {Stand: &CriteriaStand{IDs: []string{"size_bound"}}}}
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the prover stored it", Comment: 700})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if want := "policy, repro c1, review c1, criteria c1, approval c1, criteria c1, criteria c1, merge"; strings.Join(g.steps, ", ") != want {
		t.Fatalf("steps = %v, want %s", g.steps, want)
	}
	if !slices.Equal(g.escalated, []string{proofsNotStored}) || len(*holds) != 0 || !slices.Equal(g.merges, []string{"9 c1 sebastian"}) {
		t.Fatalf("escalations %q, holds %v, merges %v: want the one ask, then the merge", g.escalated, *holds, g.merges)
	}
}

// A press whose never-judged criteria are a human's holds the merge, its
// words the human's, and runs neither the gate nor a model review.
func TestWorkflowPressHoldsForAHumanRow(t *testing.T) {
	g, holds := regateHarness(t, []Unjudged{{Human: []string{"pages_ok"}}})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	g.signalAfter(2*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	if final := g.run(t); final != "closed" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if want := "policy, repro c1, review c1, criteria c1, approval c1, hold"; strings.Join(g.steps, ", ") != want {
		t.Fatalf("steps = %v, want %s", g.steps, want)
	}
	if !slices.Equal(*holds, []string{"9 c1 sebastian"}) || len(g.merges) != 0 {
		t.Fatalf("holds %v, merges %v: want the one hold, no merge", *holds, g.merges)
	}
}

// A review whose every finding the grep dropped starts no session: the
// pull request goes to the approval card with the drops, and the verdict
// stays concerns, so the merge rule auto does not merge it.
func TestWorkflowDroppedFindingsStartNoSession(t *testing.T) {
	g := newGateHarness(t, func(g *gateHarness) {
		g.review = Review{Verdict: "concerns", Summary: "Sym is missing",
			Dropped: []Dropped{{Finding: "b.go: Sym is never defined", Kind: ClaimUndefined, Symbol: "Sym", Evidence: "a.go:3 at head: func Sym() {}"}}}
	})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if len(g.sessions) != 1 || len(g.approvals) != 1 || len(g.approvals[0].Review.Dropped) != 1 || g.approvals[0].Review.Verdict != "concerns" {
		t.Fatalf("sessions %d, approvals %+v", len(g.sessions), g.approvals)
	}
}

// A gate's findings meet the check-in before the next session is bought:
// the review's finding reaches the ask with its source, the review and
// the gates that raised it, and a check-in that needs a human ends the
// round as the ask — no third session runs.
func TestWorkflowGateFindingsRunTheCheckin(t *testing.T) {
	finding := "a.go: x is missing"
	g := newGateHarness(t, func(g *gateHarness) {
		g.taskConfig = askTaskConfig()
		g.reviews = []Review{{Verdict: "concerns", Summary: "the file lacks the check", Findings: []string{finding}}}
	})
	drop := map[string]CheckVerdict{finding: {State: CheckDropped, Evidence: "the test never ran", Round: 1}}
	g.onSessions(pushed("s1", "c1"),
		checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckGoalConflict,
			Reason: "the objective conflicts with the guardrails", Verdicts: drop}))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := g.run(t); final != "closed" {
		t.Fatalf("final = %q, want closed", final)
	}
	if len(g.sessions) != 2 {
		t.Fatalf("sessions = %d, want two: the push and the check-in's pod", len(g.sessions))
	}
	ask := g.sessions[1].CheckInAsk
	if ask == nil || ask.Moment != CheckInGate || ask.ParentSession != "s1" || len(ask.Findings) != 1 ||
		ask.Findings[0] != (CheckFinding{Text: finding, Source: "review"}) {
		t.Fatalf("the ask = %+v, want the review's finding named from the review", ask)
	}
	if ask.Review == nil || ask.Review.Verdict != "concerns" || ask.Gates == nil || ask.Gates.Pin != 7 {
		t.Fatalf("the ask = %+v, want the raising review and gates", ask)
	}
	if len(g.escalated) != 1 || g.escalated[0] != "check-in: goal_conflict: the objective conflicts with the guardrails" {
		t.Fatalf("escalations = %q, want the check-in's ask", g.escalated)
	}
	var checkin *recordedStep
	for i := range g.recorded {
		if g.recorded[i].step == "checkin" {
			checkin = &g.recorded[i]
		}
	}
	// the activity boundary round-trips detail through JSON: numbers and
	// verdict maps come back decoded
	var verdicts map[string]CheckVerdict
	if checkin != nil {
		if b, err := json.Marshal(checkin.detail["verdicts"]); err != nil {
			t.Fatal(err)
		} else if err := json.Unmarshal(b, &verdicts); err != nil {
			t.Fatal(err)
		}
	}
	if checkin == nil || checkin.detail["round"] != float64(1) || checkin.detail["decision"] != CheckHuman || !maps.Equal(drop, verdicts) {
		t.Fatalf("checkin steps = %+v, want round 1's decision with the verdicts", g.recorded)
	}
}

// A round whose every finding the check-in dropped verifies the same head
// again: the re-review reads the drop record as its earlier input, and
// only its own approve merges — one approval, one merge, no session on
// the dropped round.
func TestWorkflowCheckinDroppedVerifiesAgain(t *testing.T) {
	finding := "a.go: x is missing"
	g := newGateHarness(t, func(g *gateHarness) {
		g.taskConfig = askTaskConfig()
		g.reviews = []Review{
			{Verdict: "concerns", Summary: "the file lacks the check", Findings: []string{finding}},
			{Verdict: "approve", Summary: "the head answers the finding"},
		}
	})
	drop := map[string]CheckVerdict{finding: {State: CheckDropped, Evidence: "the test never ran", Round: 1}}
	g.onSessions(pushed("s1", "c1"),
		checkinStopped("s2", OutcomeCheckinDropped, &CheckInOutcome{Decision: CheckDroppedAll, Verdicts: drop}))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if len(g.earlier) != 2 || !slices.Equal(g.rounds, []int{1, 2}) || len(g.earlier[1]) != 1 || !maps.Equal(g.earlier[1][0].Verdicts, drop) {
		t.Fatalf("review calls: rounds %v, earlier %+v; want the re-review reading the drop record", g.rounds, g.earlier)
	}
	if len(g.sessions) != 2 || len(g.approvals) != 1 || len(g.merges) != 1 {
		t.Fatalf("sessions %d, approvals %d, merges %d; want no session on the dropped round, one approval, one merge",
			len(g.sessions), len(g.approvals), len(g.merges))
	}
}

// A finding the re-review raises again after a drop goes to the human
// with both records: the ask carries the drop as a prior, and the
// escalation names the finding and the evidence that dropped it.
func TestWorkflowReraisedDropGoesToTheHuman(t *testing.T) {
	finding := "a.go: x is missing"
	g := newGateHarness(t, func(g *gateHarness) {
		g.taskConfig = askTaskConfig()
		g.reviews = []Review{
			{Verdict: "concerns", Summary: "the file lacks the check", Findings: []string{finding}},
			{Verdict: "concerns", Summary: "the head still lacks it", Findings: []string{finding}},
		}
	})
	drop := map[string]CheckVerdict{finding: {State: CheckDropped, Evidence: "the test never ran", Round: 1}}
	g.onSessions(pushed("s1", "c1"),
		checkinStopped("s2", OutcomeCheckinDropped, &CheckInOutcome{Decision: CheckDroppedAll, Verdicts: drop}),
		checkinStopped("s3", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman,
			Reason: "re-raised after a drop: " + finding + " — the check-in dropped it on: the test never ran"}))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := g.run(t); final != "closed" {
		t.Fatalf("final = %q, want closed", final)
	}
	if len(g.sessions) != 3 || g.sessions[2].CheckInAsk == nil || g.sessions[2].CheckInAsk.Findings[0].PriorDropped != 1 {
		t.Fatalf("sessions = %+v, want the third pod's ask carrying the drop as a prior", g.sessions)
	}
	if len(g.escalated) != 1 {
		t.Fatalf("escalations = %q", g.escalated)
	}
	for _, s := range []string{"re-raised after a drop", finding, "the test never ran"} {
		if !strings.Contains(g.escalated[0], s) {
			t.Errorf("the escalation lacks %q: %q", s, g.escalated[0])
		}
	}
}

// A finding the check-in confirmed as many times as checkin_sessions
// grants sessions goes to the human: the ask counts the confirmations,
// and an outcome with no reason kind falls back to the gate moment.
func TestWorkflowConfirmedTwiceGoesToTheHuman(t *testing.T) {
	finding := "a.go: x is missing"
	g := newGateHarness(t, func(g *gateHarness) {
		g.taskConfig = askTaskConfig()
		g.reviews = []Review{
			{Verdict: "concerns", Summary: "the file lacks the check", Findings: []string{finding}},
			{Verdict: "concerns", Summary: "the head still lacks it", Findings: []string{finding}},
		}
	})
	g.onSessions(pushed("s1", "c1"),
		checkinPushed("s2", "c2", &CheckInOutcome{Decision: CheckHandover, Handover: "the finding stands: fix a.go",
			Verdicts: map[string]CheckVerdict{finding: {State: CheckConfirmed, Round: 1}}}),
		checkinStopped("s3", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman,
			Reason: "confirmed 1 times: " + finding}))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := g.run(t); final != "closed" {
		t.Fatalf("final = %q, want closed", final)
	}
	if len(g.sessions) != 3 || g.sessions[2].CheckInAsk == nil || g.sessions[2].CheckInAsk.Findings[0].PriorConfirmed != 1 {
		t.Fatalf("sessions = %+v, want the third pod's ask counting the confirmation", g.sessions)
	}
	if len(g.escalated) != 1 || g.escalated[0] != "check-in: gate: confirmed 1 times: "+finding {
		t.Fatalf("escalations = %q, want the gate moment as the reason kind's fallback", g.escalated)
	}
	var handover *recordedStep
	for i := range g.recorded {
		if g.recorded[i].step == "handover" {
			handover = &g.recorded[i]
		}
	}
	if handover == nil || handover.detail["handover"] != "the finding stands: fix a.go" || handover.detail["session"] != "s2" {
		t.Fatalf("handover steps = %+v, want s2's note", g.recorded)
	}
}

// A restart keeps the check-in's priors: the verdicts the timeline holds
// come back as the priors the next check-in's ask counts, so a finding
// confirmed before the restart cannot be confirmed again unnoticed. The
// resume's own verification of the pushed head raises the finding, and
// the session it sends back counts the confirmation the earlier run's
// check-in left.
func TestWorkflowRestartKeepsCheckinPriors(t *testing.T) {
	finding := "a.go: x is missing"
	g := newGateHarness(t, func(g *gateHarness) {
		g.taskConfig = askTaskConfig()
		g.reviews = []Review{{Verdict: "concerns", Summary: "the file lacks the check", Findings: []string{finding}}}
	})
	g.state = RunState{Status: "in-review", PR: PullRequest{Number: 9}, Head: "c1",
		CheckinVerdicts: []map[string]CheckVerdict{{finding: {State: CheckConfirmed, Round: 1}}}}
	g.onSessions(pushed("s1", "c1"),
		checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman,
			Reason: "confirmed 2 times: " + finding}))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := g.run(t); final != "closed" {
		t.Fatalf("final = %q, want closed", final)
	}
	if len(g.sessions) != 2 || g.sessions[0].CheckInAsk == nil {
		t.Fatalf("sessions = %+v, want the resume's round to arm the check-in", g.sessions)
	}
	ask := g.sessions[0].CheckInAsk
	if len(ask.Findings) != 1 || ask.Findings[0] != (CheckFinding{Text: finding, Source: "review", PriorConfirmed: 1, Group: findingID(finding)}) {
		t.Fatalf("the ask = %+v, want the finding counting the pre-restart confirmation", ask.Findings)
	}
}

// A checks finding the check-in dropped ends on the card when the checks
// re-raise it: the third pod's ask names the finding from the review with
// the drop as a prior, and no coding session ran on the dropped round.
func TestWorkflowChecksFindingsDroppedEndOnTheCard(t *testing.T) {
	finding := "the build fails: red"
	g := newGateHarness(t, func(g *gateHarness) {
		g.taskConfig = askTaskConfig()
		g.reviews = []Review{
			{Verdict: "approve", Summary: "clean"},
			{Verdict: "concerns", Summary: "the drop did not hold", Findings: []string{finding}},
		}
		// the checks land on the workflow's clock after each round's gate
		// chain, which takes none: a mock that answers at once races the
		// chain, and a round-1 review it cancels shifts the review answers
		g.checksDelay = time.Minute
	})
	drop := map[string]CheckVerdict{finding: {State: CheckDropped, Evidence: "the flake is back: job 41", Round: 1}}
	g.onSessions(pushed("s1", "c1"),
		checkinStopped("s2", OutcomeCheckinDropped, &CheckInOutcome{Decision: CheckDroppedAll, Verdicts: drop}),
		checkinStopped("s3", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman,
			Reason: "re-raised after a drop: " + finding + " — the check-in dropped it on: the flake is back: job 41"}))
	// round 1's red checks raise the finding; the re-verification on the
	// drop record finds them green and the re-review re-raises it instead
	g.onChecks(ChecksResult{State: ChecksFailure, Findings: []string{finding}}, ChecksResult{State: ChecksSuccess})
	g.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := g.run(t); final != "closed" {
		t.Fatalf("final = %q, want closed", final)
	}
	if len(g.sessions) != 3 || g.sessions[2].CheckInAsk == nil {
		t.Fatalf("sessions = %+v, want three pods", g.sessions)
	}
	f := g.sessions[2].CheckInAsk.Findings
	if len(f) != 1 || f[0] != (CheckFinding{Text: finding, Source: "review", PriorDropped: 1, Group: findingID(finding)}) {
		t.Fatalf("the ask's findings = %+v, want the re-raised finding with the drop as a prior", f)
	}
	if !strings.Contains(g.escalated[0], "re-raised after a drop") {
		t.Fatalf("escalations = %q, want the re-raise to reach a human", g.escalated)
	}
}

// The check-in's handover replaces the stop lines: a no-push session that
// ran a check-in hands the next session its own findings without the
// branch line and the agent's last message, and the handover step records
// the note; with no check-in coming, handoverLines stands (the existing
// no-push tests pin that).
func TestWorkflowHandoverReplacesHandoverLines(t *testing.T) {
	finding := "a.go: x is missing"
	g := newGateHarness(t, func(g *gateHarness) {
		g.taskConfig = askTaskConfig()
		g.reviews = []Review{
			{Verdict: "concerns", Summary: "the file lacks the check", Findings: []string{finding}},
			{Verdict: "approve", Summary: "fixed"},
		}
	})
	g.onSessions(pushed("s1", "c1"),
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s2", Findings: []string{"stopped: the model balked"},
			LastMessage: "I could not finish", Commit: "c1",
			CheckIn: &CheckInOutcome{Decision: CheckHandover, Handover: "the finding stands: fix a.go",
				Verdicts: map[string]CheckVerdict{finding: {State: CheckConfirmed, Round: 1}}}},
		checkinStopped("s3", OutcomeCheckinDropped, &CheckInOutcome{Decision: CheckDroppedAll,
			Verdicts: map[string]CheckVerdict{finding: {State: CheckDropped, Evidence: "the test never ran", Round: 1}}}))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if g.sessions[1].CheckInAsk == nil {
		t.Fatalf("the no-push session ran no check-in: %+v", g.sessions[1])
	}
	got := g.sessions[2].Findings
	if !slices.Contains(got, finding) || !slices.Contains(got, "stopped: the model balked") ||
		slices.ContainsFunc(got, func(l string) bool {
			return strings.HasPrefix(l, lastMessageLine) || strings.Contains(l, "the branch holds")
		}) {
		t.Fatalf("the next session's findings = %q, want the finding and the stop line, no handover lines", got)
	}
}

// A hold-out red at the head goes to the settlement before the criteria
// gate's verdict stands: the settlement's spec question asks the human,
// the answer runs the gate again on the same commit, and green then the
// card goes to its approval and merges.
func TestWorkflowHoldoutSettlementAsksAndTheAnswerRunsTheGateAgain(t *testing.T) {
	ask := "hold-out door_state asks a question the spec cannot settle: must the log keep the key id?"
	g := newGateHarness(t, func(g *gateHarness) {
		g.criteria = []CriteriaResult{
			{Holdouts: []HoldoutGateResult{{Idx: 1, ID: "door_state", Verdict: "red", Evidence: "--- FAIL: TestDoorShut (0.01s)"}}},
			{},
		}
		g.settlements = []HoldoutSettlement{{Ask: ask}}
	})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the log keeps the key id", Comment: 700})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if len(g.sessions) != 1 || !slices.Equal(g.escalated, []string{ask}) {
		t.Fatalf("sessions %d, escalations %q, want one session and the settlement's ask", len(g.sessions), g.escalated)
	}
	want := []string{"policy", "repro c1", "review c1", "criteria c1", "settle c1", "criteria c1", "approval c1", "merge"}
	if !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
}

// A settlement that opens a real gap routes the card back to a session
// with the audit's finding, as a reviewer's finding does: a new commit,
// the gates again from the policy gate, and the approval merges it.
func TestWorkflowHoldoutSettlementRealGapRoutesTheCardToASession(t *testing.T) {
	finding := "Every key turn lands in the log (hold-out key_log): the audit found a real gap the spec names and the plan's proof does not cover; work the criterion."
	g := newGateHarness(t, func(g *gateHarness) {
		g.criteria = []CriteriaResult{
			{Holdouts: []HoldoutGateResult{{Idx: 1, ID: "key_log", Verdict: "red", Evidence: "--- FAIL: TestKeyLog (0.01s)"}}},
			{},
		}
		g.settlements = []HoldoutSettlement{{Findings: []string{finding}}}
	})
	g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if len(g.sessions) != 2 || !slices.Contains(g.sessions[1].Findings, finding) {
		t.Fatalf("sessions = %+v, want a second session holding the audit's finding", g.sessions)
	}
	want := []string{"policy", "repro c1", "review c1", "criteria c1", "settle c1", "policy", "repro c2", "review c2", "criteria c2", "approval c2", "merge"}
	if !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
}

// A head over the card's line bound is refused to the agent, however the
// over-bound diff was made: no approval is asked on it, the finding
// goes back, and the card continues when the agent brings the head
// under the bound (or a human extends it).
func TestWorkflowOverTheCardLineBoundIsFindingsNotApproval(t *testing.T) {
	over := "the pull request's diff is 20 lines over the card's 300-line bound; merge main, run the checks, and push again — if the merge cannot fit, the card closes into a successor"
	g := newGateHarness(t)
	g.gateSeq = []PolicyGate{
		{Class: "normal", Outcome: policy.OutcomeRequiresApproval, PolicyVersion: 7, OverBound: over},
		{Class: "normal", Outcome: policy.OutcomeRequiresApproval, PolicyVersion: 7},
	}
	g.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if len(g.sessions) != 2 || !slices.Equal(g.sessions[1].Findings, []string{over}) {
		t.Fatalf("sessions = %+v, want a second session holding the over-bound finding", g.sessions)
	}
	if len(g.approvals) != 1 || g.approvals[0].Commit != "c2" {
		t.Fatalf("approvals = %+v, want one, on the head under the bound only", g.approvals)
	}
	if want := []string{"policy", "policy", "repro c2", "review c2", "criteria c2", "approval c2", "merge"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
}
