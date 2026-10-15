package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

var testWorkflowConfig = WorkflowConfig{
	Models:        ModelLadder{Start: "factory/coding-cheap", Stronger: "factory/coding-frontier"},
	MaxSessions:   3,
	WallClockCap:  20 * time.Minute,
	AnswerSlice:   10 * time.Minute,
	ChecksTimeout: 45 * time.Minute,
	PolicyVersion: 7,
	StrongerAfter: 1,
	Stages:        testStages(),
}

// testStages is the Stage 9 workflow (rulings 177, 182): screening on
// for outside accounts, every other stage on; the local gates (policy,
// migration, reproduction) come before the checks, whose wait the review
// runs beside (roadmap 222, item b).
func testStages() []policy.Stage {
	return []policy.Stage{
		{Name: "screening", OnFindings: "human", Enabled: true, When: "outsider"},
		{Name: "policy", OnFindings: "fail", Enabled: true},
		{Name: "migration", OnFindings: "human", Enabled: true},
		{Name: "reproduction", OnFindings: "agent", Enabled: true},
		{Name: "checks", OnFindings: "agent", Enabled: true},
		{Name: "review", OnFindings: "agent", Enabled: true},
		{Name: "approval", OnFindings: "human", Enabled: true},
		{Name: "merge", OnFindings: "human", Enabled: true},
	}
}

// workflowHarness drives TaskWorkflow with every activity stubbed: the
// defaults are a clean first session, a PR, green checks; tests override
// the step they are about.
type workflowHarness struct {
	env         *testsuite.TestWorkflowEnvironment
	sessions    []SessionInput
	finals      [][2]string
	states      []string
	state       RunState // what Reconcile reports; zero = a fresh task
	finishAt    time.Time
	escalated   []string   // Escalate reasons, in order
	choices     [][]string // Escalate choices, in order
	usage       []TaskUsage
	logs        *lockedBuffer // the workflow's log
	recorded    []recordedStep
	infoAsked   []string    // RequestInfo reasons, in order
	closedAt    []time.Time // CloseNotReproduced calls
	autoNotes   []string    // AutoMergeNote calls: "pr commit"
	escalateErr error       // Escalate's error, nil = none
	// taskConfig answers TaskConfig when set; configPins are its calls' pins
	taskConfig *TaskConfig
	configPins []int64
	// branchHead answers BranchHead; branchReads counts its calls
	branchHead  string
	branchReads int
	// headChecks are the heads HeadChecks (the approval stage's read) ran on
	headChecks []string
}

// recordedStep is a RecordStep call and when on the workflow's clock.
type recordedStep struct {
	step   string
	detail map[string]any
	at     time.Time
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// newWorkflowHarness runs wf, TaskWorkflow on testWorkflowConfig when
// none is given.
func newWorkflowHarness(t *testing.T, wf ...TaskWorkflow) *workflowHarness {
	w := TaskWorkflow{Config: func() WorkflowConfig { return testWorkflowConfig }}
	if len(wf) > 0 {
		w = wf[0]
	}
	var suite testsuite.WorkflowTestSuite
	logs := &lockedBuffer{}
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(logs, nil))))
	h := &workflowHarness{env: suite.NewTestWorkflowEnvironment(), logs: logs}
	h.env.RegisterWorkflowWithOptions(w.Run, workflow.RegisterOptions{Name: "TaskWorkflow"})
	for name, fn := range map[string]any{
		ActivityEnsureIssue:     func(ctx context.Context, taskID string) error { return nil },
		ActivitySetState:        func(ctx context.Context, taskID, state string) error { return nil },
		ActivitySetGate:         func(ctx context.Context, taskID, gate string, since time.Time) error { return nil },
		ActivityRunSession:      func(ctx context.Context, in SessionInput) (SessionResult, error) { return SessionResult{}, nil },
		ActivityOpenPullRequest: func(ctx context.Context, taskID, sha string) (PullRequest, error) { return PullRequest{}, nil },
		ActivityWaitChecks:      func(ctx context.Context, taskID, sha string) (ChecksResult, error) { return ChecksResult{}, nil },
		ActivityHeadChecks: func(ctx context.Context, taskID, sha string) (ChecksResult, error) {
			return ChecksResult{State: ChecksSuccess}, nil
		},
		ActivityPullRequestMerged: func(ctx context.Context, taskID string, number int64) (bool, error) { return false, nil },
		ActivityRecordStep:        func(ctx context.Context, taskID, step string, detail map[string]any) error { return nil },
		ActivityFinish:            func(ctx context.Context, taskID, final, reason string) error { return nil },
		ActivityReconcile:         func(ctx context.Context, taskID string) (RunState, error) { return RunState{}, nil },
		ActivityEscalate:          func(ctx context.Context, taskID, reason string, choices []string) (int64, error) { return 0, nil },
		ActivityRecordUsage:       func(ctx context.Context, taskID string, u TaskUsage) error { return nil },
		ActivityGatePolicy: func(ctx context.Context, taskID string, pr int64, commit string, pin int64) (PolicyGate, error) {
			return PolicyGate{}, nil
		},
		ActivityGateMigration: func(ctx context.Context, taskID string, pr int64, migrations []string) error { return nil },
		ActivityGateReproduction: func(ctx context.Context, taskID string, pr int64, commit string) (ReproResult, error) {
			return ReproResult{}, nil
		},
		ActivityModelReview: func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			return Review{}, nil
		},
		ActivityScreen:         func(ctx context.Context, taskID string) (ScreenResult, error) { return ScreenResult{}, nil },
		ActivityJudgeScreening: func(ctx context.Context, taskID, verdict string) error { return nil },
		ActivityScreenAnswer: func(ctx context.Context, taskID, text string, pin int64) (ScreenResult, error) {
			return ScreenResult{}, nil
		},
		ActivityRunReport: func(ctx context.Context, taskID, model string, pin int64) (ReportResult, error) {
			return ReportResult{}, nil
		},
		ActivityRequestApproval: func(ctx context.Context, taskID string, in ApprovalInput) error { return nil },
		ActivityMerge: func(ctx context.Context, taskID string, pr int64, commit, approver, checkedHead string) (MergeResult, error) {
			return MergeResult{}, nil
		},
		ActivityReportMergeFailure:          func(ctx context.Context, taskID string, pr int64, commit, approver, reason string) error { return nil },
		ActivityReportMergeRetriesExhausted: func(ctx context.Context, taskID string, pr int64, commit, approver string) error { return nil },
		ActivitySuperviseGate: func(ctx context.Context, taskID string, in GateSupervision) (GateDecision, error) {
			return GateDecision{}, nil
		},
		ActivityDropTrip: func(ctx context.Context, taskID string, trip GateTrip) error { return nil },
		ActivityAutoMergeNote: func(ctx context.Context, taskID string, pr int64, commit string, g GateResults, rv Review) error {
			return nil
		},
		ActivityRequestInfo:        func(ctx context.Context, taskID, reason string) (int64, error) { return 0, nil },
		ActivityCloseNotReproduced: func(ctx context.Context, taskID string) error { return nil },
		ActivityRollCommit:         func(ctx context.Context, taskID string) (string, error) { return "", nil },
		ActivityRerunChecks:        func(ctx context.Context, taskID, sha string) (string, error) { return "", nil },
		ActivityGateAudit: func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults) (AuditResult, error) {
			return AuditResult{}, nil
		},
		ActivityTaskConfig:   func(ctx context.Context, taskID string, pin int64) (TaskConfig, error) { return TaskConfig{}, nil },
		ActivityBranchHead:   func(ctx context.Context, taskID string) (string, error) { return "", nil },
		ActivityDependencies: func(ctx context.Context, taskID string) (DepState, error) { return DepState{}, nil },
		ActivityEnterQueue:   func(ctx context.Context, taskID string) (int, error) { return 0, nil },
		ActivitySyncWithMain: func(ctx context.Context, taskID string, pr int64, from string) (SyncResult, error) {
			return SyncResult{}, nil
		},
		ActivityLeaveQueue:       func(ctx context.Context, taskID, why string) error { return nil },
		ActivityWithdrawApproval: func(ctx context.Context, taskID string) error { return nil },
		ActivityRunPlan: func(ctx context.Context, taskID, model string, pin int64, findings []string) (PlanResult, error) {
			return PlanResult{}, nil
		},
		ActivityRequestPlan: func(ctx context.Context, taskID string, revision int) (PlanRef, error) { return PlanRef{}, nil },
		ActivityCreateChildren: func(ctx context.Context, taskID string, revision int, approver string, pin int64) (ChildrenResult, error) {
			return ChildrenResult{}, nil
		},
		ActivityOpenPlanQuestions: func(ctx context.Context, taskID string, revision int) (int, error) { return 0, nil },
		ActivityAwaitSessionGap:   func(ctx context.Context, taskID string) error { return nil },
		ActivityAwaitRollHold:     func(ctx context.Context, taskID string) error { return nil },
		ActivityClearRollHold:     func(ctx context.Context, taskID string, merged bool) error { return nil },
		ActivityProviderWait:      func(ctx context.Context, taskID string, w ProviderWait) error { return nil },
		ActivityAlertPreCheck:     func(ctx context.Context, taskID string) (bool, error) { return true, nil },
		ActivityAttemptFunded:     func(ctx context.Context, taskID, model string) (string, error) { return "", nil },
		ActivityRunSplit: func(ctx context.Context, taskID, model string, pin int64, findings []string) (PlanResult, error) {
			return PlanResult{}, nil
		},
		ActivityFileSplit: func(ctx context.Context, taskID string, revision int, approver string, pin int64) (ChildrenResult, error) {
			return ChildrenResult{}, nil
		},
	} {
		h.env.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
	h.env.OnActivity(ActivityEnsureIssue, mock.Anything, "t1").Return(nil)
	// the real TaskConfig's answer for a task of process maintenance: a
	// report task (h.state.Report, a parent's) runs the analyzer
	h.env.OnActivity(ActivityTaskConfig, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID string, pin int64) (TaskConfig, error) {
			h.configPins = append(h.configPins, pin)
			if h.taskConfig != nil {
				return *h.taskConfig, nil
			}
			tc := TaskConfig{Process: "maintenance", Lane: policy.LaneReady, Class: policy.ClassCoder, Session: policy.SessionCode,
				Stages: w.Config().Stages, Queue: policy.QueueNone}
			if h.state.Report {
				tc.Lane, tc.Class, tc.Session = "", policy.ClassAnalyzer, policy.SessionReport
			}
			return tc, nil
		})
	h.env.OnActivity(ActivityBranchHead, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) (string, error) { h.branchReads++; return h.branchHead, nil })
	h.env.OnActivity(ActivityReconcile, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) (RunState, error) { return h.state, nil })
	h.env.OnActivity(ActivitySetState, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, state string) error { h.states = append(h.states, state); return nil })
	h.env.OnActivity(ActivityRecordStep, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, step string, detail map[string]any) error {
			h.recorded = append(h.recorded, recordedStep{step, detail, h.env.Now()})
			return nil
		})
	h.env.OnActivity(ActivityRecordUsage, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID string, u TaskUsage) error { h.usage = append(h.usage, u); return nil })
	h.env.OnActivity(ActivityEscalate, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, reason string, choices []string) (int64, error) {
			h.escalated = append(h.escalated, reason)
			h.choices = append(h.choices, choices)
			return int64(600 + len(h.escalated)), h.escalateErr
		})
	h.env.OnActivity(ActivityRequestInfo, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, reason string) (int64, error) {
			h.infoAsked = append(h.infoAsked, reason)
			return int64(650 + len(h.infoAsked)), nil
		})
	h.env.OnActivity(ActivityCloseNotReproduced, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) error {
			h.closedAt = append(h.closedAt, h.env.Now())
			return nil
		})
	h.env.OnActivity(ActivityAutoMergeNote, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr int64, commit string, g GateResults, rv Review) error {
			h.autoNotes = append(h.autoNotes, fmt.Sprintf("%d %s", pr, commit))
			return nil
		})
	h.env.OnActivity(ActivityFinish, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, final, reason string) error {
			h.finals = append(h.finals, [2]string{final, reason})
			h.finishAt = h.env.Now()
			return nil
		})
	return h
}

// onSessions answers RunSession attempts in order with results, recording
// each input.
func (h *workflowHarness) onSessions(results ...SessionResult) {
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			return results[min(len(h.sessions), len(results))-1], nil
		})
}

// onChecks answers WaitChecks in order with results, behind a PR #9 that
// every OpenPullRequest reuses.
func (h *workflowHarness) onChecks(results ...ChecksResult) {
	h.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9, URL: "https://git.example/sf/r/pulls/9"}, nil)
	n := 0
	h.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, sha string) (ChecksResult, error) {
			n++
			return results[min(n, len(results))-1], nil
		})
}

// onHeadChecks answers HeadChecks, the approval stage's read of the staged
// head, in order with results, the last repeating, and records the heads
// it read.
func (h *workflowHarness) onHeadChecks(results ...ChecksResult) {
	n := 0
	h.env.OnActivity(ActivityHeadChecks, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, sha string) (ChecksResult, error) {
			h.headChecks = append(h.headChecks, sha)
			n++
			return results[min(n, len(results))-1], nil
		})
}

func (h *workflowHarness) signalAfter(d time.Duration, name string, sig TaskSignal) {
	h.env.RegisterDelayedCallback(func() { h.env.SignalWorkflow(name, sig) }, d)
}

func (h *workflowHarness) run(t *testing.T) string {
	t.Helper()
	h.env.ExecuteWorkflow("TaskWorkflow", "t1")
	if !h.env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := h.env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var final string
	if err := h.env.GetWorkflowResult(&final); err != nil {
		t.Fatal(err)
	}
	if len(h.finals) != 1 || h.finals[0][0] != final {
		t.Fatalf("Finish calls = %v, want exactly one for %q", h.finals, final)
	}
	return final
}

func pushed(session, commit string) SessionResult {
	return SessionResult{Outcome: OutcomePushed, SessionID: session, Commit: commit, Summary: "pushed branch factory/t1 at " + commit}
}

func TestWorkflowHappyPathToMerged(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(pushed("s1", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if len(h.sessions) != 1 || h.sessions[0].Model != "factory/coding-cheap" || h.sessions[0].StartRef != "" || h.sessions[0].Attempt != 1 {
		t.Fatalf("sessions = %+v, want one cheap attempt from the base", h.sessions)
	}
	if !slices.Equal(h.states, []string{"running", "in-review"}) {
		t.Fatalf("states = %v, want [running in-review]", h.states)
	}
}

// A project whose file sets model: stronger (TaskConfig.Stronger) starts
// its coding card's first session on Models.Stronger, not Models.Start.
func TestStrongerProjectStartsOnTheStrongerRung(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = &TaskConfig{Process: "maintenance", Lane: policy.LaneReady, Class: policy.ClassCoder, Session: policy.SessionCode,
		Stages: testStages(), Queue: policy.QueueNone, Stronger: true}
	h.onSessions(pushed("s1", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if len(h.sessions) != 1 || h.sessions[0].Model != "factory/coding-frontier" {
		t.Fatalf("sessions = %+v, want the first session on the stronger rung factory/coding-frontier", h.sessions)
	}
}

// The version the workflow started under reaches every session and the
// model review, which read their limits from it (ruling 183).
func TestWorkflowSessionsAndReviewCarryThePin(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(pushed("s1", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	var reviewed []int64
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			reviewed = append(reviewed, g.Pin)
			return Review{}, nil
		})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if len(h.sessions) != 1 || h.sessions[0].PolicyVersion != 7 || !slices.Equal(reviewed, []int64{7}) {
		t.Fatalf("sessions %+v, reviews pinned to %v; want version 7 in both", h.sessions, reviewed)
	}
}

// The task's own class time, not the workflow's fallback cap, anchors the
// session (review-task110.md I2): program() carries TaskConfig's
// AgentTime and CheckIn into t.cfg.Task and, when set, into
// t.cfg.WallClockCap; the lifecycle's SessionInput and sessionOptions both
// read from there. testWorkflowConfig's 20 minute cap is deliberately
// smaller than the 2h/45m class below, so a session anchored on the cap
// instead of the class shows up in either assertion. Breaking any of the
// three lines this depends on (program()'s t.cfg.Task assignment, its
// WallClockCap override, or the lifecycle's SessionInput literal) turns
// this red.
func TestWorkflowSessionCarriesTheTasksOwnAgentTimeAndCheckIn(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = &TaskConfig{Process: "maintenance", Lane: policy.LaneReady, Class: policy.ClassCoder, Session: policy.SessionCode,
		Stages: testStages(), Queue: policy.QueueNone, AgentTime: 2 * time.Hour, CheckIn: 45 * time.Minute}
	var deadlines []time.Duration
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			deadlines = append(deadlines, activity.GetInfo(ctx).StartToCloseTimeout)
			return pushed("s1", "c1"), nil
		})
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if len(h.sessions) != 1 || h.sessions[0].AgentTime != 2*time.Hour || h.sessions[0].CheckIn != 45*time.Minute {
		t.Fatalf("sessions = %+v, want the class's AgentTime 2h, CheckIn 45m", h.sessions)
	}
	if want := 2*time.Hour + sessionGrace; len(deadlines) != 1 || deadlines[0] != want {
		t.Fatalf("activity StartToCloseTimeout = %v, want %v (the class's own agent_time plus grace, not the workflow's 20m cap)", deadlines, want)
	}
}

// Before versionStages red checks climbed at once.
func TestWorkflowBeforeStagesRedChecksRetryStrongerFromBranchHead(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnGetVersion("stages", workflow.DefaultVersion, versionStages).Return(workflow.DefaultVersion)
	h.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	red := []string{"pr / test (pull_request): failure — Failure after 2m (https://git.example/sf/r/actions/runs/7/jobs/0)"}
	h.onChecks(ChecksResult{State: ChecksFailure, Findings: red}, ChecksResult{State: ChecksSuccess})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if len(h.sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(h.sessions))
	}
	second := h.sessions[1]
	if second.Model != "factory/coding-frontier" || second.StartRef != "c1" || second.ParentSessionID != "s1" ||
		!slices.Equal(second.Findings, red) || second.Attempt != 2 {
		t.Fatalf("second attempt = %+v, want frontier from c1, parent s1, the red checks as findings", second)
	}
}

func TestWorkflowChangesRequestedStartsNewAttempt(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", Body: "rename the flag to --dry-run", PR: 9})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if len(h.sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(h.sessions))
	}
	second := h.sessions[1]
	if second.StartRef != "c1" || second.Model != "factory/coding-cheap" || len(second.Findings) != 1 ||
		!strings.Contains(second.Findings[0], "rename the flag to --dry-run") || !strings.Contains(second.Findings[0], "sebastian") {
		t.Fatalf("second attempt = %+v, want the review text as findings, from c1, same model", second)
	}
}

// Before versionSessionCap the attempts ran out into a failure, and no
// repeat of a trip changed the rung.
func TestWorkflowBeforeSessionCapAttemptExhaustionFails(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnGetVersion("session-cap", workflow.DefaultVersion, versionSessionCap).Return(workflow.DefaultVersion)
	h.onSessions(SessionResult{Outcome: OutcomeNoPush, SessionID: "s", Summary: "pod exited without a result", Findings: []string{"ran `go test`"}})

	if final := h.run(t); final != "failed" {
		t.Fatalf("final = %q, want failed", final)
	}
	if len(h.sessions) != 3 {
		t.Fatalf("sessions = %d, want MaxSessions (3)", len(h.sessions))
	}
	if !strings.Contains(h.finals[0][1], "3 attempts exhausted") || !strings.Contains(h.finals[0][1], "pod exited without a result") {
		t.Fatalf("reason = %q", h.finals[0][1])
	}
	if !slices.Equal(h.sessions[1].Findings, []string{"ran `go test`"}) || h.sessions[1].ParentSessionID != "s" {
		t.Fatalf("second attempt = %+v, want the first session's findings and parent", h.sessions[1])
	}
	if h.sessions[2].Model != "factory/coding-cheap" || len(h.escalated) != 0 {
		t.Fatalf("third attempt on %s, escalations %q; want the start rung and no card", h.sessions[2].Model, h.escalated)
	}
}

// Ruling 177: sessions that run out of attempts without a pushed commit
// open the card with every finding so far; the answer buys one more
// session, which starts with the last session's own findings, the
// earlier ones' being stale (roadmap 201).
func TestWorkflowSessionCapOpensTheCard(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(
		SessionResult{Outcome: OutcomeRedirect, SessionID: "s1", Trip: "repetition", Summary: "supervisor: redirect after repetition", Findings: []string{"f1"}},
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "no changes to push", Findings: []string{"f2"}},
		SessionResult{Outcome: OutcomeRetryStronger, SessionID: "s3", Trip: "half_way", Summary: "supervisor: retry_stronger after half_way", Findings: []string{"f3"}},
		pushed("s4", "c4"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the key is blocked, use the other one", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	wantCard := capReached + "\nlast: supervisor: retry_stronger after half_way" +
		"\n- supervisor:repetition, attempt 1: f1" +
		"\n- session, attempt 2: f2" +
		"\n- supervisor:half_way, attempt 3: f3"
	if !slices.Equal(h.escalated, []string{wantCard}) {
		t.Fatalf("escalations = %q, want the card %q", h.escalated, wantCard)
	}
	if len(h.sessions) != 4 {
		t.Fatalf("sessions = %d, want 4: the answer bought one more than MaxSessions", len(h.sessions))
	}
	fourth := h.sessions[3]
	if fourth.Attempt != 4 || fourth.Model != "factory/coding-frontier" || fourth.ParentSessionID != "s3" ||
		!slices.Equal(fourth.Findings, []string{"f3"}) ||
		!slices.Equal(fourth.Answers, []string{`sebastian, to "` + capReached + `": the key is blocked, use the other one`}) {
		t.Fatalf("fourth attempt = %+v", fourth)
	}
	var to []string
	for _, r := range h.recorded {
		if r.step == "stage_findings" {
			to = append(to, fmt.Sprintf("%s>%s", r.detail["stage"], r.detail["to"]))
		}
	}
	if want := []string{"supervisor:repetition>agent", "session>agent", "supervisor:half_way>stronger"}; !slices.Equal(to, want) {
		t.Fatalf("stage_findings = %v, want %v", to, want)
	}
	if last := h.usage[len(h.usage)-1]; last != (TaskUsage{Attempts: 4, ExtraAttempts: 1}) {
		t.Fatalf("usage = %+v, want four attempts, one of them granted by the answer", last)
	}
}

// Before versionKeepWork a change request on the last round, after a pushed commit, is the
// cap's last finding on the card; the answer buys a session with it.
func TestWorkflowSessionCapAfterAChangeRequestOpensTheCard(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnGetVersion("keep-work", workflow.DefaultVersion, versionKeepWork).Return(workflow.DefaultVersion)
	h.state = RunState{Status: "running", Usage: TaskUsage{Attempts: 2}}
	h.onSessions(pushed("s3", "c3"), pushed("s4", "c4"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", Body: "keep the old flag", PR: 9})
	h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "alias it", Comment: 602})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	change := "reviewer sebastian requested changes: keep the old flag"
	if want := []string{capReached + "\nlast: changes requested\n- " + change}; !slices.Equal(h.escalated, want) {
		t.Fatalf("escalations = %q, want %q", h.escalated, want)
	}
	if len(h.sessions) != 2 || h.sessions[1].Attempt != 4 || h.sessions[1].StartRef != "c3" ||
		!slices.Equal(h.sessions[1].Findings, []string{change}) ||
		!slices.Equal(h.sessions[1].Answers, []string{`sebastian, to "` + capReached + `": alias it`}) {
		t.Fatalf("sessions = %+v, want the fourth from c3 with the change request and the answer", h.sessions)
	}
}

// A run that starts with every attempt used opens the card before any
// session.
func TestWorkflowRunAtTheSessionCapOpensTheCard(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "running", Usage: TaskUsage{AgentTime: 12 * time.Minute, Attempts: 3}}
	h.onSessions(pushed("s4", "c4"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if want := []string{capReached + "\nlast: no session produced a result"}; !slices.Equal(h.escalated, want) {
		t.Fatalf("escalations = %q, want %q", h.escalated, want)
	}
	if len(h.sessions) != 1 || h.sessions[0].Attempt != 4 {
		t.Fatalf("sessions = %+v, want one, the fourth, after the answer", h.sessions)
	}
	if h.usage[0] != (TaskUsage{AgentTime: 12 * time.Minute, Attempts: 3, ExtraAttempts: 1}) {
		t.Fatalf("usage = %+v, want the answer's grant first: an attempt, no extra time (8m left already covers the 5m slice)", h.usage)
	}
}

// Ruling 322 (finding 215): a card whose task's remaining budget cannot
// fund a call refuses the attempt an answer would buy. No session starts
// (no key is minted), no failure round is added, and the waiting-human
// card says why an answer will not run: each answer re-asks the same
// refusal, and only a close ends the task.
func TestWorkflowUnfundedAnswerRefusesTheAttempt(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "waiting-human", Question: "which alias?"}
	unfunded := budgetRefused + ": 0.080 EUR of the 2.000 EUR budget remains, and the cheapest call needs more. Raise the budget (Fund on the board) and answer, or close the card."
	h.env.OnActivity(ActivityAttemptFunded, mock.Anything, "t1", mock.Anything).Return(unfunded, nil)
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "again", Comment: 603})
	h.signalAfter(3*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" {
		t.Fatalf("final = %q, want closed", final)
	}
	if len(h.sessions) != 0 {
		t.Fatalf("sessions = %d, want none: the attempt is refused before it starts", len(h.sessions))
	}
	if len(h.escalated) != 3 || h.escalated[0] != healedReason ||
		!strings.HasPrefix(h.escalated[1], budgetRefused) || !strings.HasPrefix(h.escalated[2], budgetRefused) {
		t.Fatalf("escalations = %q, want the healed question, then the budget refusal after each answer", h.escalated)
	}
	if u := h.usage[len(h.usage)-1]; u.Attempts != 0 || u.ExtraAttempts != 1 {
		t.Fatalf("usage = %+v, want the answer's grant and no attempt run: no failure round", u)
	}
}

// A fundable remainder grants the attempt exactly as before (ruling 322
// guards only the unfunded one): the answer at the session cap buys the
// next session once the floor read clears it.
func TestWorkflowFundedAnswerGrantsTheAttemptAsBefore(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "running", Usage: TaskUsage{AgentTime: 12 * time.Minute, Attempts: 3}}
	floors := 0
	h.env.OnActivity(ActivityAttemptFunded, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, model string) (string, error) { floors++; return "", nil })
	h.onSessions(pushed("s4", "c4"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if len(h.sessions) != 1 || h.sessions[0].Attempt != 4 || floors != 1 {
		t.Fatalf("sessions = %+v, floor reads = %d, want the fourth attempt after one floor read", h.sessions, floors)
	}
}

// The card lists every finding once: stage findings already in the history
// are not repeated, and a change request drained before a session that
// failed is on it.
func TestWorkflowSessionCapCardListsEachFindingOnce(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(time.Minute).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			// Temporal retries a failed session: record each attempt once
			if len(h.sessions) < in.Attempt {
				h.sessions = append(h.sessions, in)
			}
			if in.Attempt == 1 {
				return pushed("s1", "c1"), nil
			}
			return SessionResult{}, errors.New("pod lost")
		})
	h.onChecks(ChecksResult{State: ChecksFailure, Findings: []string{"red"}})
	h.signalAfter(90*time.Second, SignalChangesRequested, TaskSignal{Author: "sebastian", Body: "rename it", PR: 9})
	h.signalAfter(5*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" {
		t.Fatalf("final = %q", final)
	}
	change := "reviewer sebastian requested changes: rename it"
	want := capReached + "\nlast: session failed: pod lost\n- checks, round 1: red\n- " + change
	if !slices.Equal(h.escalated, []string{want}) {
		t.Fatalf("escalations = %q, want %q", h.escalated, want)
	}
	if len(h.sessions) != 3 || !slices.Equal(h.sessions[2].Findings, []string{"red", change}) {
		t.Fatalf("sessions = %+v, want the third with the red checks and the change request", h.sessions)
	}
}

// The card keeps the newest findings within maxCard, each cut at
// maxCardLine, and says how many earlier ones it left out.
func TestSessionCapCardIsBounded(t *testing.T) {
	var run taskRun
	for i := range 20 {
		run.history = append(run.history, fmt.Sprintf("%02d", i)+strings.Repeat("x", 1498))
	}
	card := run.sessionCapCard(strings.Repeat("y", 1500), []string{"new"})
	lines := strings.Split(card, "\n")
	// each history line is "- " + 1000 bytes + "…": 1006 bytes with its
	// newline, "- new" 6; 7 history lines and "new" fit 8000, 8 do not
	if len(lines) != 11 || lines[1] != "last: "+strings.Repeat("y", 1000)+"…" || lines[2] != "- (13 earlier findings left out)" ||
		!strings.HasPrefix(lines[3], "- 13x") || lines[9] != "- 19"+strings.Repeat("x", 998)+"…" || lines[10] != "- new" {
		t.Fatalf("card has %d lines: %.120q", len(lines), lines)
	}
}

// A new run starts on the stronger rung when the last one left the task
// there: a return past StrongerAfter or a retry_stronger.
func TestWorkflowNewRunKeepsTheRung(t *testing.T) {
	for _, c := range []struct {
		name string
		st   RunState
		want string
	}{
		{"repeat trip", RunState{Returns: map[string]int{"supervisor:repetition": 2}}, "factory/coding-frontier"},
		{"retry_stronger", RunState{Climbed: true}, "factory/coding-frontier"},
		{"one return", RunState{Returns: map[string]int{"supervisor:repetition": 1, "checks": 1}}, "factory/coding-cheap"},
	} {
		h := newWorkflowHarness(t)
		h.state = c.st
		h.state.Status, h.state.Usage = "running", TaskUsage{Attempts: 1}
		h.onSessions(pushed("s2", "c2"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
		if final := h.run(t); final != "done" || h.sessions[0].Model != c.want {
			t.Fatalf("%s: final %q on %s, want done on %s", c.name, final, h.sessions[0].Model, c.want)
		}
	}
}

// A retry_stronger on the last attempt is recorded "stronger", so a new
// run after a restart at the card starts on the rung it climbed to.
func TestWorkflowRetryStrongerOnTheLastAttemptIsRecordedStronger(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "running", Usage: TaskUsage{Attempts: 2}}
	h.onSessions(SessionResult{Outcome: OutcomeRetryStronger, SessionID: "s3", Trip: "half_way", Findings: []string{"f3"}})
	h.signalAfter(time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" {
		t.Fatalf("final = %q", final)
	}
	var to []string
	for _, r := range h.recorded {
		if r.step == "stage_findings" {
			to = append(to, fmt.Sprintf("%s>%s", r.detail["stage"], r.detail["to"]))
		}
	}
	if want := []string{"supervisor:half_way>stronger"}; !slices.Equal(to, want) {
		t.Fatalf("stage_findings = %v, want %v", to, want)
	}
}

// A second return on the same trip takes the stronger rung
// (limits.stronger_after 1); a different trip keeps the model.
func TestWorkflowRepeatTripTakesTheStrongerRung(t *testing.T) {
	for _, c := range []struct {
		second TripKind
		want   string
	}{
		{TripRepetition, "factory/coding-frontier"},
		{TripNoProgress, "factory/coding-cheap"},
	} {
		h := newWorkflowHarness(t)
		h.onSessions(
			SessionResult{Outcome: OutcomeRedirect, SessionID: "s1", Trip: string(TripRepetition), Findings: []string{"a"}},
			SessionResult{Outcome: OutcomeRedirect, SessionID: "s2", Trip: string(c.second), Findings: []string{"b"}},
			pushed("s3", "c3"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

		if final := h.run(t); final != "done" {
			t.Fatalf("%s: final = %q", c.second, final)
		}
		var models []string
		for _, in := range h.sessions {
			models = append(models, in.Model)
		}
		if want := []string{"factory/coding-cheap", "factory/coding-cheap", c.want}; !slices.Equal(models, want) {
			t.Fatalf("second trip %s: models = %v, want %v", c.second, models, want)
		}
	}
}

func TestWorkflowWallClockExpiryFails(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(21 * time.Minute).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			return SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "no-progress trip"}, nil
		})

	if final := h.run(t); final != "failed" {
		t.Fatalf("final = %q, want failed", final)
	}
	if len(h.sessions) != 1 || h.finals[0][1] != "wall-clock cap reached" {
		t.Fatalf("sessions = %d, reason = %q; want one session, then the cap", len(h.sessions), h.finals[0][1])
	}
}

func TestWorkflowSessionWallClockTripFails(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(SessionResult{Outcome: OutcomeWallClock, SessionID: "s1"})

	if final := h.run(t); final != "failed" || h.finals[0][1] != "wall-clock cap reached" || len(h.sessions) != 1 {
		t.Fatalf("final = %q, reason = %q, sessions = %d", final, h.finals[0][1], len(h.sessions))
	}
}

// wallClock is a session the supervisor stopped at the agent-time cap,
// its work in progress pushed as commit ("" for none).
func wallClock(session, commit string) SessionResult {
	return SessionResult{Outcome: OutcomeWallClock, SessionID: session, Commit: commit, Summary: "supervision trip: wall_clock"}
}

// onVerify records into steps, in order, each OpenPullRequest as "pr
// <commit> after <n> cards" (PR #9), each RequestApproval as "approval
// <commit>" and each RunSession as "session <attempt> after <n> cards"
// (answered by results in order, none registered for nil); WaitChecks answers checks in order, the
// last repeating, green when none are given.
func (h *workflowHarness) onVerify(steps *[]string, results []SessionResult, checks ...ChecksResult) {
	if len(checks) == 0 {
		checks = []ChecksResult{{State: ChecksSuccess}}
	}
	h.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, sha string) (PullRequest, error) {
			*steps = append(*steps, fmt.Sprintf("pr %s after %d cards", sha, len(h.escalated)))
			return PullRequest{Number: 9, URL: "https://git.example/sf/r/pulls/9"}, nil
		})
	h.env.OnActivity(ActivityRequestApproval, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID string, in ApprovalInput) error {
			*steps = append(*steps, "approval "+in.Commit)
			return nil
		})
	n := 0
	h.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, sha string) (ChecksResult, error) {
			n++
			return checks[min(n, len(checks))-1], nil
		})
	if results == nil {
		return
	}
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			*steps = append(*steps, fmt.Sprintf("session %d after %d cards", in.Attempt, len(h.escalated)))
			return results[min(len(h.sessions), len(results))-1], nil
		})
}

var gosecRed = ChecksResult{State: ChecksFailure, Findings: []string{"gosec G706"}}

// A session stopped at the cap with pushed work: the branch's head goes
// through verification instead of failing the task.
func TestCapWithCommitsGoesToVerification(t *testing.T) {
	h := newWorkflowHarness(t)
	h.branchHead = "c1"
	var steps []string
	h.onVerify(&steps, []SessionResult{wallClock("s1", "c1")})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if want := []string{"session 1 after 0 cards", "pr c1 after 0 cards", "approval c1"}; !slices.Equal(steps, want) || h.branchReads != 1 {
		t.Fatalf("steps = %v, branch reads = %d; want %v and one read", steps, h.branchReads, want)
	}
}

// A session activity timed out at the cap: the same, the head c1 goes
// through verification.
func TestCapTimeoutWithCommitsGoesToVerification(t *testing.T) {
	h := newWorkflowHarness(t)
	h.branchHead = "c1"
	var steps []string
	h.onVerify(&steps, nil)
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			return SessionResult{}, temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_START_TO_CLOSE, nil)
		})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	// the timeout is tried MaximumAttempts (10) times, all one session
	if want := []string{"pr c1 after 0 cards", "approval c1"}; !slices.Equal(steps, want) || len(h.sessions) != 10 || h.branchReads != 1 {
		t.Fatalf("steps = %v, session tries = %d, branch reads = %d; want %v after ten tries and one read", steps, len(h.sessions), h.branchReads, want)
	}
}

// A branch with nothing past the base fails at the cap as before.
func TestCapWithoutCommitsFails(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(wallClock("s1", ""))

	if final := h.run(t); final != "failed" || h.finals[0][1] != "wall-clock cap reached" || h.branchReads != 1 {
		t.Fatalf("final = %q, finals = %v, branch reads = %d", final, h.finals, h.branchReads)
	}
}

// With 20m of agent time and 16m used, 4m are left: no session starts,
// the cap reads the branch. 15m used leaves 5m: a session starts. An
// agent time of 3m, unused, starts a session too; with 1m used, 2m are
// left: none.
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
		h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

		final := h.run(t)
		if len(h.sessions) != tc.sessions || h.branchReads != 1-tc.sessions {
			t.Fatalf("%s: sessions = %d, branch reads = %d; want %d and %d", name, len(h.sessions), h.branchReads, tc.sessions, 1-tc.sessions)
		}
		if want := map[int]string{0: "failed", 1: "done"}[tc.sessions]; final != want {
			t.Fatalf("%s: final = %q, finals = %v; want %s", name, final, h.finals, want)
		}
	}
}

// The last session stops with work in progress (a redirect with c2): at
// the session cap c2 goes through verification; its red checks go to the
// one cap card, not to an approval card, and the answer's session gets
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
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if want := []string{"session 1 after 0 cards", "session 2 after 0 cards", "session 3 after 0 cards", "pr c2 after 0 cards",
		"session 4 after 1 cards", "pr c4 after 1 cards", "approval c4"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want %v", steps, want)
	}
	if want := []string{capReached + "\nlast: checks findings went back to the agent\n- supervisor:repetition, attempt 3: f3\n- checks, round 1: gosec G706"}; !slices.Equal(h.escalated, want) {
		t.Fatalf("escalations = %q, want %q", h.escalated, want)
	}
	if h.sessions[3].StartRef != "c2" || !slices.Equal(h.sessions[3].Findings, []string{"gosec G706"}) {
		t.Fatalf("fourth session = %+v, want it from c2 with the checks finding", h.sessions[3])
	}
}

// At the agent-time cap an unverified head's red checks go to the one cap
// card, not to an approval card; the session after the answer gets them.
func TestTimeCapFindingsGoToTheOneCard(t *testing.T) {
	h := newWorkflowHarness(t)
	h.branchHead = "c1"
	h.state = RunState{Status: "running", Usage: TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1}}
	var steps []string
	h.onVerify(&steps, []SessionResult{pushed("s2", "c2")}, gosecRed, ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if want := []string{"pr c1 after 0 cards", "session 2 after 1 cards", "pr c2 after 1 cards", "approval c2"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want %v", steps, want)
	}
	if want := []string{capReached + "\nlast: checks findings went back to the agent\n- checks, round 1: gosec G706"}; !slices.Equal(h.escalated, want) {
		t.Fatalf("escalations = %q, want %q", h.escalated, want)
	}
	if !slices.Equal(h.sessions[0].Findings, []string{"gosec G706"}) {
		t.Fatalf("session findings = %q, want the checks finding", h.sessions[0].Findings)
	}
}

// A run restarted with a change request pending on its PR's head c1 and
// the whole 20m used: c1 counts as judged, so the cap card opens with no
// second verification of c1.
func TestTimeCapAdoptedHeadIsJudged(t *testing.T) {
	h := newWorkflowHarness(t)
	h.branchHead = "c1"
	change := "reviewer sebastian requested changes: rename it"
	h.state = RunState{Status: "in-review", PR: PullRequest{Number: 9}, Head: "c1", Findings: []string{change},
		Usage: TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1}}
	var steps []string
	h.onVerify(&steps, []SessionResult{pushed("s2", "c2")})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if want := []string{"session 2 after 1 cards", "pr c2 after 1 cards", "approval c2"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want %v", steps, want)
	}
	if want := []string{capReached + "\nlast: no session produced a result\n- " + change}; !slices.Equal(h.escalated, want) {
		t.Fatalf("escalations = %q, want %q", h.escalated, want)
	}
}

// Ruling 225: a writer's change request on the merge card with no session
// left is the human's answer. It buys the one extra session, which gets
// the change request, and no cap card follows: at the last regular
// attempt, at the session cap and with no agent time left.
func TestChangeRequestWithNoSessionLeftIsTheAnswer(t *testing.T) {
	change := "reviewer sebastian requested changes: keep the old flag"
	for name, tc := range map[string]struct {
		state   RunState
		head    string
		results []SessionResult
		want    []string
		usage   TaskUsage // after the grant
	}{
		"last regular attempt": {
			state:   RunState{Status: "running", Usage: TaskUsage{Attempts: 2}},
			results: []SessionResult{pushed("s3", "c3"), pushed("s4", "c4")},
			want: []string{"session 3 after 0 cards", "pr c3 after 0 cards", "approval c3",
				"session 4 after 0 cards", "pr c4 after 0 cards", "approval c4"},
			usage: TaskUsage{Attempts: 3, ExtraAttempts: 1},
		},
		"session cap": {
			head: "c3",
			results: []SessionResult{
				{Outcome: OutcomeNoPush, SessionID: "s1"},
				{Outcome: OutcomeNoPush, SessionID: "s2"},
				{Outcome: OutcomeRedirect, SessionID: "s3", Commit: "c3", Trip: "repetition"},
				pushed("s4", "c4")},
			want: []string{"session 1 after 0 cards", "session 2 after 0 cards", "session 3 after 0 cards", "pr c3 after 0 cards", "approval c3",
				"session 4 after 0 cards", "pr c4 after 0 cards", "approval c4"},
			usage: TaskUsage{Attempts: 3, ExtraAttempts: 1},
		},
		"no agent time left": {
			state:   RunState{Status: "in-review", PR: PullRequest{Number: 9}, Head: "c3", Usage: TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1}},
			results: []SessionResult{pushed("s2", "c4")},
			want:    []string{"pr c3 after 0 cards", "approval c3", "session 2 after 0 cards", "pr c4 after 0 cards", "approval c4"},
			usage:   TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1, ExtraTime: 5 * time.Minute, ExtraAttempts: 1},
		},
	} {
		h := newWorkflowHarness(t)
		h.state, h.branchHead = tc.state, tc.head
		var steps []string
		h.onVerify(&steps, tc.results)
		h.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", Body: "keep the old flag", PR: 9})
		h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

		if final := h.run(t); final != "done" {
			t.Fatalf("%s: final = %q, finals = %v", name, final, h.finals)
		}
		if !slices.Equal(steps, tc.want) || len(h.escalated) != 0 {
			t.Fatalf("%s: steps = %v, escalations = %q; want %v and none", name, steps, h.escalated, tc.want)
		}
		if last := h.sessions[len(h.sessions)-1]; !slices.Equal(last.Findings, []string{change}) {
			t.Fatalf("%s: last session findings = %q, want the change request", name, last.Findings)
		}
		if !slices.Contains(h.usage, tc.usage) {
			t.Fatalf("%s: usage = %+v, want %+v recorded", name, h.usage, tc.usage)
		}
	}
}

func TestWorkflowRepetitionTripClimbsTheLadder(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(SessionResult{Outcome: OutcomeRetryStronger, SessionID: "s1", Findings: []string{"changed a.go"}}, pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if h.sessions[1].Model != "factory/coding-frontier" || h.sessions[1].StartRef != "" || !slices.Equal(h.sessions[1].Findings, []string{"changed a.go"}) {
		t.Fatalf("second attempt = %+v", h.sessions[1])
	}
}

func TestWorkflowIssueClosedWithoutMergeEndsClosed(t *testing.T) {
	for _, merged := range []bool{false, true} {
		h := newWorkflowHarness(t)
		h.onSessions(pushed("s1", "c1"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.env.OnActivity(ActivityPullRequestMerged, mock.Anything, "t1", int64(9)).Return(merged, nil)
		h.signalAfter(time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

		want, reason := "closed", "issue closed without merge"
		if merged {
			want, reason = "done", "pull request merged"
		}
		if final := h.run(t); final != want || h.finals[0][1] != reason {
			t.Fatalf("merged=%v: final = %q (%q), want %q (%q)", merged, final, h.finals[0][1], want, reason)
		}
	}
}

func TestWorkflowPullRequestRefusalFailsTask(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(pushed("s1", "c1"))
	h.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", "c1").Return(PullRequest{},
		terminalError("branch factory/t1 changes .forgejo/workflows/pr.yml; refusing to open a pull request"))

	if final := h.run(t); final != "failed" || !strings.Contains(h.finals[0][1], ".forgejo/workflows/pr.yml") {
		t.Fatalf("final = %q, reason = %q", final, h.finals[0][1])
	}
}

func TestWorkflowChecksTimeoutFails(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(pushed("s1", "c1"))
	h.onChecks(ChecksResult{State: ChecksTimeout})

	if final := h.run(t); final != "failed" || !strings.Contains(h.finals[0][1], "checks did not finish within 45m0s") {
		t.Fatalf("final = %q, reason = %q", final, h.finals[0][1])
	}
}

func TestWorkflowPullRequestClosedWithoutMergeFails(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(pushed("s1", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalPRClosed, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "failed" || h.finals[0][1] != "pull request closed without merge" {
		t.Fatalf("final = %q (%q)", final, h.finals[0][1])
	}
}

// A rejection or a merge that lands while checks run decides the task:
// no further attempt, no second PR.
func TestWorkflowTerminalSignalDuringChecksDecides(t *testing.T) {
	for name, want := range map[string][2]string{
		SignalPRClosed: {"failed", "pull request closed without merge"},
		SignalPRMerged: {"done", "pull request merged"},
	} {
		h := newWorkflowHarness(t)
		h.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
		h.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9}, nil)
		// the checks' own verdict (here a timeout) must not override the signal
		h.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).After(30*time.Minute).
			Return(ChecksResult{State: ChecksTimeout}, nil)
		h.signalAfter(10*time.Minute, name, TaskSignal{Author: "sebastian", PR: 9})

		if final := h.run(t); final != want[0] || h.finals[0][1] != want[1] || len(h.sessions) != 1 {
			t.Fatalf("%s: final = %q (%q), sessions = %d; want %v after one session", name, final, h.finals[0][1], len(h.sessions), want)
		}
	}
}

// A human's close of a running card's issue ends it closed (roadmap 173);
// a run from before versionClosedByHuman ends it failed.
func TestWorkflowIssueClosedDuringFirstAttemptOpensNoPR(t *testing.T) {
	for v, want := range map[workflow.Version]string{versionClosedByHuman: "closed: issue closed without merge",
		workflow.DefaultVersion: "failed: issue closed while no pull request of this workflow was open"} {
		h := newWorkflowHarness(t)
		h.env.OnGetVersion("closed-by-human", workflow.DefaultVersion, versionClosedByHuman).Return(v)
		h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(10*time.Minute).Return(pushed("s1", "c1"), nil)
		h.signalAfter(5*time.Minute, SignalIssueClosed, TaskSignal{Author: "sebastian"})
		if final := h.run(t); final+": "+h.finals[0][1] != want {
			t.Fatalf("version %d: final = %q (%q), want %q", v, final, h.finals[0][1], want)
		}
	}
}

// One review with three inline comments is one new attempt, not three.
func TestWorkflowCoalescesChangeRequests(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	for _, body := range []string{"a", "b", "c"} {
		h.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", Body: body, PR: 9})
	}
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 2 || len(h.sessions[1].Findings) != 3 {
		t.Fatalf("final = %q, sessions = %d, second findings = %q; want 2 sessions, 3 findings", final, len(h.sessions), h.sessions[len(h.sessions)-1].Findings)
	}
}

// A hand-opened PR on the task branch is not the task's: its close or
// merge decides nothing.
func TestWorkflowIgnoresOtherPullRequests(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(pushed("s1", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalPRClosed, TaskSignal{Author: "someone", PR: 99})
	h.signalAfter(2*time.Hour, SignalChangesRequested, TaskSignal{Author: "someone", Body: "x", PR: 99})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 1 {
		t.Fatalf("final = %q, sessions = %d; want done after one session", final, len(h.sessions))
	}
}

func TestWorkflowTerminalSessionErrorFinishes(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			return SessionResult{}, terminalError("repository %q is not owner/name", "x")
		})

	if final := h.run(t); final != "failed" || len(h.sessions) != 1 || !strings.Contains(h.finals[0][1], "not owner/name") {
		t.Fatalf("final = %q, sessions = %d, reason = %q", final, len(h.sessions), h.finals[0][1])
	}
}

// Workflows started before versionSignalsDrained keep their review wait:
// the first signal decides, a change request is one attempt.
func TestWorkflowDefaultVersionKeepsFirstSignalReview(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnGetVersion("signals-drained", workflow.DefaultVersion, versionSignalsDrained).Return(workflow.DefaultVersion)
	h.env.OnGetVersion("gates", workflow.DefaultVersion, versionGates).Return(workflow.DefaultVersion)
	h.env.OnGetVersion("stages", workflow.DefaultVersion, versionStages).Return(workflow.DefaultVersion)
	h.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", Body: "a"})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{})

	if final := h.run(t); final != "done" || len(h.sessions) != 2 {
		t.Fatalf("final = %q, sessions = %d", final, len(h.sessions))
	}
}

// Histories recorded live on the cluster before versionSignalsDrained
// replay without a nondeterminism error.
func TestWorkflowReplaysLiveHistories(t *testing.T) {
	files, _ := filepath.Glob("testdata/history-*.json")
	if len(files) == 0 {
		t.Fatal("no recorded histories")
	}
	for _, f := range files {
		replayer := worker.NewWorkflowReplayer()
		replayer.RegisterWorkflowWithOptions(TaskWorkflow{Config: func() WorkflowConfig { return testWorkflowConfig }}.Run, workflow.RegisterOptions{Name: "TaskWorkflow"})
		if err := replayer.ReplayWorkflowHistoryFromJSONFile(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), f); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}

// providerHarness answers RunSession with results in order and records
// when each session and each provider wait began.
func providerHarness(t *testing.T, results ...SessionResult) (h *workflowHarness, began *[]time.Time, waits *[]recordedStep) {
	h, began, waits = newWorkflowHarness(t), &[]time.Time{}, &[]recordedStep{}
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions, *began = append(h.sessions, in), append(*began, h.env.Now())
			return results[min(len(h.sessions), len(results))-1], nil
		})
	h.env.OnActivity(ActivityProviderWait, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID string, w ProviderWait) error {
			*waits = append(*waits, recordedStep{StepProviderRefused, map[string]any{"status": w.Status, "reason": w.Reason, "backoff_s": int(w.Backoff.Seconds())}, h.env.Now()})
			return nil
		})
	return h, began, waits
}

var refusedSession = SessionResult{Outcome: OutcomeProviderRefused, SessionID: "s0", Status: 429, Reason: "DashscopeException: budget exhausted (status 429)",
	Findings: []string{"previous session ended without a pushed commit: DashscopeException: budget exhausted (status 429)"}}

// A provider's refusal at a session's first call is no attempt (ruling
// 267): the task waits on the provider 2 minutes, then 4, in gate provider
// (ProviderWait), and runs the session again with the same input; the log
// says so once at the first refusal and once at the recovery.
func TestWorkflowProviderRefusalIsNoAttempt(t *testing.T) {
	h, began, waits := providerHarness(t, refusedSession, refusedSession, pushed("s3", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if len(h.sessions) != 3 {
		t.Fatalf("%d sessions, want 3", len(h.sessions))
	}
	for i, in := range h.sessions {
		if in.Attempt != 1 || len(in.Findings) != 0 || in.ParentSessionID != "" {
			t.Errorf("session %d = %+v, want attempt 1 with no findings and no parent", i+1, in)
		}
	}
	reason := refusedSession.Reason
	want := []map[string]any{{"status": 429, "reason": reason, "backoff_s": 120}, {"status": 429, "reason": reason, "backoff_s": 240}}
	if len(*waits) != 2 || fmt.Sprint((*waits)[0].detail, (*waits)[1].detail) != fmt.Sprint(want[0], want[1]) {
		t.Fatalf("provider waits = %+v, want %v", *waits, want)
	}
	// each wait between the sessions it follows and precedes, as long as its backoff
	for i, w := range *waits {
		if !w.at.Equal((*began)[i]) || (*began)[i+1].Sub(w.at) != time.Duration(w.detail["backoff_s"].(int))*time.Second {
			t.Errorf("wait %d at %v between sessions at %v and %v, want its backoff", i+1, w.at, (*began)[i], (*began)[i+1])
		}
	}
	if len(h.usage) == 0 || slices.ContainsFunc(h.usage, func(u TaskUsage) bool { return u.Attempts != 1 }) {
		t.Fatalf("usage = %+v, want every record at attempt 1", h.usage)
	}
	logs := h.logs.String()
	if n, m := strings.Count(logs, "the model provider refused a session's first call"), strings.Count(logs, "the model provider answers again"); n != 1 || m != 1 {
		t.Fatalf("log: %d refusal lines, %d recovery lines, want one each:\n%s", n, m, logs)
	}
}

// Six refusals in a row go to a human with the provider's reason, after
// waits of 2, 4, 8, 16 and 30 minutes; no attempt is counted.
func TestWorkflowSixProviderRefusalsAskAHuman(t *testing.T) {
	h, _, waits := providerHarness(t, refusedSession)
	h.signalAfter(3*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" || len(h.sessions) != 6 {
		t.Fatalf("final = %q after %d sessions, want closed after 6", final, len(h.sessions))
	}
	var backoffs []int
	for _, w := range *waits {
		backoffs = append(backoffs, w.detail["backoff_s"].(int))
	}
	if !slices.Equal(backoffs, []int{120, 240, 480, 960, 1800}) {
		t.Fatalf("backoffs = %v, want 120 240 480 960 1800", backoffs)
	}
	if want := "the model provider refused the first call of 6 sessions in a row (status 429): " + refusedSession.Reason; !slices.Equal(h.escalated, []string{want}) {
		t.Fatalf("escalations = %q, want %q", h.escalated, want)
	}
	if got := escalationSource(h.escalated[0]); got != "provider" {
		t.Fatalf("escalation source = %q, want provider", got)
	}
	if len(h.usage) != 0 {
		t.Fatalf("usage = %+v, want no attempt recorded", h.usage)
	}
}

// A session that ran into the wall clock made calls: the provider
// answered, so the next refusal waits 2 minutes again.
func TestWorkflowWallClockSessionResetsTheProviderBackoff(t *testing.T) {
	h := newWorkflowHarness(t)
	h.branchHead = "c1"
	var steps []string
	h.onVerify(&steps, nil, gosecRed)
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			// the second session's ten tries time out
			if n := len(h.sessions); n >= 2 && n <= 11 {
				return SessionResult{}, temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_START_TO_CLOSE, nil)
			}
			return refusedSession, nil
		})
	var backoffs []int
	h.env.OnActivity(ActivityProviderWait, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID string, w ProviderWait) error {
			backoffs = append(backoffs, int(w.Backoff.Seconds()))
			return nil
		})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 700})
	h.signalAfter(61*time.Minute, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" || len(h.escalated) != 1 || !strings.HasPrefix(h.escalated[0], capReached) {
		t.Fatalf("final = %q, escalations %q; want closed after the cap card", final, h.escalated)
	}
	if !slices.Equal(backoffs, []int{120, 120}) {
		t.Fatalf("backoffs = %v, want 120 before and 120 after the wall-clock session", backoffs)
	}
	if n := strings.Count(h.logs.String(), "the model provider answers again"); n != 1 {
		t.Fatalf("%d recovery lines, want 1", n)
	}
}

// A terminal signal ends the wait on the provider at once.
func TestWorkflowProviderWaitEndsOnTheIssuesClose(t *testing.T) {
	h, began, _ := providerHarness(t, refusedSession)
	h.signalAfter(time.Minute, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" || len(h.sessions) != 1 || h.finishAt.Sub((*began)[0]) >= providerBackoff(1) {
		t.Fatalf("final = %q after %d sessions at %v, want closed after one, within the first wait", final, len(h.sessions), h.finishAt.Sub((*began)[0]))
	}
}

// A run from before versionProviderWait takes a refusal as no push.
func TestWorkflowProviderRefusalBeforeTheChange(t *testing.T) {
	h, _, waits := providerHarness(t, refusedSession, pushed("s2", "c1"))
	h.env.OnGetVersion("provider-wait", workflow.DefaultVersion, versionProviderWait).Return(workflow.DefaultVersion)
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 2 || len(*waits) != 0 {
		t.Fatalf("final = %q, %d sessions, %d waits; want done after 2 sessions, no wait", final, len(h.sessions), len(*waits))
	}
	if h.sessions[1].Attempt != 2 || !slices.Equal(h.sessions[1].Findings, refusedSession.Findings) {
		t.Fatalf("second session = %+v, want attempt 2 with the refusal's findings", h.sessions[1])
	}
}

// dayCapHarness starts at 2026-10-14 20:43 UTC, card #784's session, and
// answers RunSession with results in order, recording when each began. Its
// first session is refused at the day cap, which resets at 00:00 UTC.
func dayCapHarness(t *testing.T, results ...SessionResult) (h *workflowHarness, began *[]time.Time) {
	h, began = newWorkflowHarness(t), &[]time.Time{}
	h.env.SetStartTime(time.Date(2026, 10, 14, 20, 43, 0, 0, time.UTC))
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions, *began = append(h.sessions, in), append(*began, h.env.Now())
			return results[min(len(h.sessions), len(results))-1], nil
		})
	return h, began
}

var dayCapped = SessionResult{Outcome: OutcomeDayCap, SessionID: "s1", Until: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC),
	Reason:   dayCapReached + ": the card waits for the UTC day to turn and resumes on its own at 2026-10-15T00:00:00Z.\nfree headroom -23.439417 EUR",
	Findings: []string{"previous session ended without a pushed commit: the model call failed: 402 refused_day_cap"}}

// A session the day cap refused spends no further session that day (roadmap
// 240): the card waits on a human with the refusal, and at 00:00 UTC runs
// the next session on its own, still the first attempt, with the refused
// session's findings and as its child.
func TestWorkflowDayCapWaitsForTheDayToTurn(t *testing.T) {
	h, began := dayCapHarness(t, dayCapped, pushed("s2", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if !slices.Equal(h.escalated, []string{dayCapped.Reason}) || escalationSource(h.escalated[0]) != "day_cap" {
		t.Fatalf("escalations = %q, want the day cap's refusal once", h.escalated)
	}
	if len(*began) != 2 || !(*began)[1].Equal(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("sessions began at %v, want the second at 2026-10-15 00:00 UTC", *began)
	}
	if in := h.sessions[1]; in.Attempt != 1 || in.ParentSessionID != "s1" || !slices.Equal(in.Findings, dayCapped.Findings) {
		t.Fatalf("second session = %+v, want attempt 1, child of s1, with s1's findings", in)
	}
	if !slices.Equal(h.states[:3], []string{"running", "running", "in-review"}) {
		t.Fatalf("states = %q, want running for each session, then in-review", h.states)
	}
	if len(h.usage) == 0 || slices.ContainsFunc(h.usage, func(u TaskUsage) bool { return u.Attempts != 1 }) {
		t.Fatalf("usage = %+v, want every record at attempt 1: the refused session is no attempt", h.usage)
	}
}

// A human's answer during the wait runs the next session at once, after its
// burst, and the day's turn starts no second one; a closed issue ends the
// card at once.
func TestWorkflowDayCapWaitTakesAnAnswerOrTheClose(t *testing.T) {
	h, began := dayCapHarness(t, dayCapped, pushed("s2", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "day_eur raised", Comment: 700})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 2 {
		t.Fatalf("final = %q after %d sessions, want done after 2", final, len(h.sessions))
	}
	if want := time.Date(2026, 10, 14, 21, 43, 30, 0, time.UTC); !(*began)[1].Equal(want) {
		t.Fatalf("second session began at %v, want %v: the answer and its quiet window", (*began)[1], want)
	}
	question := dayCapReached + ": the card waits for the UTC day to turn and resumes on its own at 2026-10-15T00:00:00Z."
	if want := []string{fmt.Sprintf("sebastian, to %q: day_eur raised", question)}; !slices.Equal(h.sessions[1].Answers, want) {
		t.Fatalf("answers = %q, want %q", h.sessions[1].Answers, want)
	}

	h, _ = dayCapHarness(t, dayCapped)
	h.signalAfter(time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	if final := h.run(t); final != "closed" || len(h.sessions) != 1 || !h.finishAt.Equal(time.Date(2026, 10, 14, 21, 43, 0, 0, time.UTC)) {
		t.Fatalf("final = %q after %d sessions at %v, want closed after one at 21:43", final, len(h.sessions), h.finishAt)
	}
}

// A refusal whose day turned before the workflow read it waits for nothing:
// the next session runs at once, asking no one.
func TestWorkflowDayCapPastItsTurnRunsAtOnce(t *testing.T) {
	turned := dayCapped
	turned.Until = time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	h, began := dayCapHarness(t, turned, pushed("s2", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.escalated) != 0 || len(*began) != 2 || !(*began)[1].Equal((*began)[0]) {
		t.Fatalf("final = %q, escalations %q, sessions at %v; want done, none asked, the second at once", final, h.escalated, *began)
	}
	if in := h.sessions[1]; in.Attempt != 1 || in.ParentSessionID != "s1" {
		t.Fatalf("second session = %+v, want attempt 1, child of s1", in)
	}
}

// A run from before versionDayCap takes a day cap's refusal as no push.
func TestWorkflowDayCapBeforeTheChange(t *testing.T) {
	h, began := dayCapHarness(t, dayCapped, pushed("s2", "c1"))
	h.env.OnGetVersion("day-cap", workflow.DefaultVersion, versionDayCap).Return(workflow.DefaultVersion)
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.escalated) != 0 || len(*began) != 2 || !(*began)[1].Equal((*began)[0]) {
		t.Fatalf("final = %q, escalations %q, sessions at %v; want done, none asked, the second at once", final, h.escalated, *began)
	}
	if in := h.sessions[1]; in.Attempt != 2 || !slices.Equal(in.Findings, dayCapped.Findings) {
		t.Fatalf("second session = %+v, want attempt 2 with the refusal's findings", in)
	}
}

// providerBackoffs records the backoff of each of h's provider waits.
func providerBackoffs(h *workflowHarness) *[]int {
	backoffs := &[]int{}
	h.env.OnActivity(ActivityProviderWait, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID string, w ProviderWait) error {
			*backoffs = append(*backoffs, int(w.Backoff.Seconds()))
			return nil
		})
	return backoffs
}

// reportHarness runs a report task whose sessions answer results in order.
func reportHarness(t *testing.T, results ...ReportResult) (h *workflowHarness, began *[]time.Time, backoffs *[]int) {
	h, began = newWorkflowHarness(t), &[]time.Time{}
	h.state.Report = true
	h.env.OnActivity(ActivityRunReport, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, model string, pin int64) (ReportResult, error) {
			*began = append(*began, h.env.Now())
			return results[min(len(*began), len(results))-1], nil
		})
	return h, began, providerBackoffs(h)
}

var refusedReport = ReportResult{Refused: ProviderWait{Status: 429, Reason: refusedSession.Reason}}

// A report session the provider refused fails no report task (roadmap
// 155, 170): it waits 2 minutes, then 4, and runs the session again.
func TestWorkflowReportWaitsOnTheProvider(t *testing.T) {
	h, began, backoffs := reportHarness(t, refusedReport, refusedReport, ReportResult{Comment: 77})
	if final := h.run(t); final != "done" || h.finals[0][1] != "report posted as issue comment 77" {
		t.Fatalf("finals %v, want done with comment 77", h.finals)
	}
	if b := *began; len(b) != 3 || b[1].Sub(b[0]) != 2*time.Minute || b[2].Sub(b[1]) != 4*time.Minute {
		t.Fatalf("sessions began %v, want three, 2 and 4 minutes apart", b)
	}
	if !slices.Equal(*backoffs, []int{120, 240}) || !slices.Equal(h.states, []string{"running", "running", "running"}) {
		t.Fatalf("backoffs %v, states %v", *backoffs, h.states)
	}
}

// Six refused report sessions in a row ask a human with source provider;
// the card's close (Intake.CloseCard signals issue_closed) ends it closed.
func TestWorkflowReportSixProviderRefusalsAskAHuman(t *testing.T) {
	h, began, backoffs := reportHarness(t, refusedReport)
	h.signalAfter(3*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	if final := h.run(t); final != "closed" || h.finals[0][1] != "issue closed without merge" || len(*began) != 6 || !slices.Equal(*backoffs, []int{120, 240, 480, 960, 1800}) {
		t.Fatalf("finals %v after %d sessions, backoffs %v; want closed after 6", h.finals, len(*began), *backoffs)
	}
	want := "the model provider refused the first call of 6 sessions in a row (status 429): " + refusedSession.Reason
	if !slices.Equal(h.escalated, []string{want}) || escalationSource(h.escalated[0]) != "provider" {
		t.Fatalf("escalations = %q, want %q from source provider", h.escalated, want)
	}
}

// The card's close during a provider wait ends the report task at once.
func TestWorkflowReportProviderWaitEndsOnTheClose(t *testing.T) {
	h, began, _ := reportHarness(t, refusedReport)
	h.signalAfter(time.Minute, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	if final := h.run(t); final != "closed" || len(*began) != 1 || h.finishAt.Sub((*began)[0]) != time.Minute {
		t.Fatalf("final = %q after %d sessions at %v, want closed after one, at the close", final, len(*began), h.finishAt.Sub((*began)[0]))
	}
}

// A report run from before versionSessionRefusals fails on a refusal as
// it failed on the session without a report.
func TestWorkflowReportRefusalBeforeTheChange(t *testing.T) {
	h, began, backoffs := reportHarness(t, refusedReport, ReportResult{Comment: 77})
	h.env.OnGetVersion("provider-wait-sessions", workflow.DefaultVersion, versionSessionRefusals).Return(workflow.DefaultVersion)
	want := "report session: the report session ended without a report: " + refusedSession.Reason
	if final := h.run(t); final != "failed" || h.finals[0][1] != want || len(*began) != 1 || len(*backoffs) != 0 {
		t.Fatalf("finals %v after %d sessions, %d waits; want failed %q after one, no wait", h.finals, len(*began), len(*backoffs), want)
	}
}

// A Forgejo or LiteLLM outage longer than any bounded retry must not strand
// the task: Finish and the other short activities retry for a day, backoff
// capped at five minutes; only a refusal ends them sooner. (The test
// environment caps unlimited retries at 10, so the policy itself is what
// is checked.)
func TestWorkflowShortActivitiesRetryForADay(t *testing.T) {
	opts := finishOptions()
	if opts.ScheduleToCloseTimeout != 24*time.Hour || opts.RetryPolicy.MaximumAttempts != 0 || opts.RetryPolicy.MaximumInterval != 5*time.Minute {
		t.Fatalf("finish options = %+v / %+v, want unlimited attempts within 24 h, 5 min backoff cap", opts, *opts.RetryPolicy)
	}
}

// The review and the audit make up to three calls of at most 15 minutes
// in one attempt: 3 x 15 + 2 = 47 minutes per attempt, and three attempts
// within 3 x 45 + 10 = 145 minutes.
func TestReviewOptionsHoldThreeCalls(t *testing.T) {
	if opts := reviewOptions(); opts.StartToCloseTimeout != 47*time.Minute || opts.ScheduleToCloseTimeout != 145*time.Minute || opts.RetryPolicy.MaximumAttempts != 3 {
		t.Fatalf("review options = %+v", opts)
	}
}

// A run started by the heal pass or by a signal reconciles first.

// Case 1+2: a healed in-review task adopts its open PR: no new session,
// checks on the PR head, and the earlier run's PR counts, so its merge (or
// the issue close "Closes #N" causes) finishes the task done.
func TestWorkflowHealedRunAdoptsTheOpenPR(t *testing.T) {
	for _, sig := range []string{SignalPRMerged, SignalIssueClosed} {
		h := newWorkflowHarness(t)
		h.state = RunState{Status: "in-review", PR: PullRequest{Number: 9}, Head: "c7"}
		h.onSessions(pushed("s1", "c1"))
		var checked []string
		h.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9}, nil)
		h.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).Return(
			func(ctx context.Context, taskID, sha string) (ChecksResult, error) {
				checked = append(checked, sha)
				return ChecksResult{State: ChecksSuccess}, nil
			})
		h.env.OnActivity(ActivityPullRequestMerged, mock.Anything, "t1", int64(9)).Return(true, nil)
		h.signalAfter(time.Hour, sig, TaskSignal{Author: "sebastian", PR: 9})

		if final := h.run(t); final != "done" || len(h.sessions) != 0 || len(checked) != 1 || checked[0] != "c7" {
			t.Fatalf("%s: final = %q, sessions = %d, checked = %v; want done, no session, checks on c7", sig, final, len(h.sessions), checked)
		}
	}
}

// A healed in-review task whose reviewer already asked for changes (the
// signal reached the run that died before its next session) starts a
// session with those findings on the PR head, without verifying the head
// again; the same request arriving once more as a signal counts once.
func TestWorkflowHealedRunTakesUpReconciledChangeRequests(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "in-review", PR: PullRequest{Number: 9}, Head: "c7",
		Findings: []string{"reviewer sebastian requested changes: rename it"}}
	h.onSessions(pushed("s2", "c8"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.env.OnActivity(ActivityPullRequestMerged, mock.Anything, "t1", int64(9)).Return(true, nil)
	h.env.RegisterDelayedCallback(func() {
		h.env.SignalWorkflow(SignalChangesRequested, TaskSignal{Author: "sebastian", Body: "rename it", PR: 9})
	}, 0)
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 1 {
		t.Fatalf("final = %q, sessions = %d; want done after one session", final, len(h.sessions))
	}
	if in := h.sessions[0]; in.StartRef != "c7" || !slices.Equal(in.Findings, []string{"reviewer sebastian requested changes: rename it"}) {
		t.Fatalf("session = %+v; want StartRef c7 and the change request once", in)
	}
}

// Case 1 variant and 3: the merge (or the close) already happened while no
// run existed, e.g. a signal started this run.
func TestWorkflowReconcileFinishesAnAlreadyDecidedTask(t *testing.T) {
	for name, tc := range map[string]struct {
		state  RunState
		final  string
		reason string
	}{
		"merged":         {RunState{Status: "in-review", Merged: true, IssueClosed: true}, "done", "pull request merged"},
		"issue closed":   {RunState{Status: "in-review", IssueClosed: true}, "closed", "issue closed without merge"},
		"pr closed":      {RunState{Status: "in-review", PRClosed: true}, "failed", "pull request closed without merge"},
		"closing merged": {RunState{Status: "closing", Merged: true}, "done", "pull request merged"},
		"closing":        {RunState{Status: "closing"}, "failed", "completion resumed after an interrupted run"},
	} {
		h := newWorkflowHarness(t)
		h.state = tc.state
		h.onSessions(pushed("s1", "c1"))
		if final := h.run(t); final != tc.final || h.finals[0][1] != tc.reason || len(h.sessions) != 0 || len(h.states) != 0 {
			t.Fatalf("%s: final = %q (%q), sessions = %d, states = %v", name, final, h.finals[0][1], len(h.sessions), h.states)
		}
	}
}

// A signal for a finished task (its workflow gone to retention) starts a
// run that does nothing.
func TestWorkflowReconcileSkipsAFinishedTask(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Skip: true, Status: "done"}
	h.env.ExecuteWorkflow("TaskWorkflow", "t1")
	var final string
	if err := h.env.GetWorkflowError(); err != nil || h.env.GetWorkflowResult(&final) != nil || final != "done" || len(h.finals) != 0 || len(h.sessions) != 0 {
		t.Fatalf("err = %v, final = %q, finishes = %v, sessions = %d", err, final, h.finals, len(h.sessions))
	}
}

// A terminal signal cancels the running session or check wait at once
// instead of after it returns.
func TestWorkflowTerminalSignalCancelsTheRunningActivity(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(10*time.Minute).Return(pushed("s1", "c1"), nil)
	start := h.env.Now()
	h.signalAfter(3*time.Minute, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	if final := h.run(t); final != "closed" || h.finishAt.IsZero() || h.finishAt.Sub(start) >= 10*time.Minute {
		t.Fatalf("session: final = %q after %s, want closed before the 10 min session returned", final, h.finishAt.Sub(start))
	}

	h = newWorkflowHarness(t)
	h.onSessions(pushed("s1", "c1"))
	h.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9}, nil)
	h.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).After(30*time.Minute).Return(ChecksResult{State: ChecksSuccess}, nil)
	start = h.env.Now()
	h.signalAfter(5*time.Minute, SignalPRClosed, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "failed" || h.finishAt.IsZero() || h.finishAt.Sub(start) >= 30*time.Minute {
		t.Fatalf("checks: final = %q after %s, want failed before the 30 min check wait returned", final, h.finishAt.Sub(start))
	}
}

func TestWorkflowRedirectResumesOnTheSameRungWithGuidance(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(
		SessionResult{Outcome: OutcomeRedirect, SessionID: "s1", Commit: "c-wip", Findings: []string{"supervisor guidance: read the script"}},
		pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	second := h.sessions[1]
	if second.Model != "factory/coding-cheap" || second.ParentSessionID != "s1" || !slices.Equal(second.Findings, []string{"supervisor guidance: read the script"}) {
		t.Fatalf("second attempt = %+v, want the same rung, parent s1, the guidance as findings", second)
	}
}

// retry_stronger climbs one rung; on the top rung it stays there, which is
// a redirect. No path raises more than the next rung.
func TestWorkflowRetryStrongerClimbsOneRungOnly(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(
		SessionResult{Outcome: OutcomeRetryStronger, SessionID: "s1", Findings: []string{"a"}},
		SessionResult{Outcome: OutcomeRetryStronger, SessionID: "s2", Findings: []string{"b"}},
		SessionResult{Outcome: OutcomeRetryStronger, SessionID: "s3", Findings: []string{"c"}})
	h.signalAfter(time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" {
		t.Fatalf("final = %q", final)
	}
	var models []string
	for _, in := range h.sessions {
		models = append(models, in.Model)
	}
	if !slices.Equal(models, []string{"factory/coding-cheap", "factory/coding-frontier", "factory/coding-frontier"}) {
		t.Fatalf("models = %v", models)
	}
}

// escalate: the workflow asks a human (state waiting-human, comment), a
// comment older than the escalation does not answer, a newer one starts a
// new attempt with the answer and one attempt more.
func TestWorkflowEscalationWaitsForAHumanAnswer(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(
		SessionResult{Outcome: OutcomeEscalate, SessionID: "s1", Reason: "the check cannot pass\n\nProgress: 9 commands", Findings: []string{"ran x"},
			Choices: []string{"edit the script", "skip the check"}},
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s2", Findings: []string{"n2"}},
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s3", Findings: []string{"n3"}},
		pushed("s4", "c4"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "stale note", Comment: 600})
	h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "you may edit the script", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if len(h.escalated) != 1 || !strings.Contains(h.escalated[0], "the check cannot pass") ||
		!slices.Equal(h.choices[0], []string{"edit the script", "skip the check"}) {
		t.Fatalf("escalations = %q with choices %q", h.escalated, h.choices)
	}
	if len(h.sessions) != 4 {
		t.Fatalf("sessions = %d, want 4: the answer bought one more than MaxSessions", len(h.sessions))
	}
	second := h.sessions[1]
	if !slices.Equal(second.Findings, []string{"ran x"}) || !slices.Equal(second.Answers, []string{`sebastian, to "the check cannot pass": you may edit the script`}) || second.ParentSessionID != "s1" {
		t.Fatalf("second attempt = %+v", second)
	}
	if !slices.Contains(h.states, "running") {
		t.Fatalf("states = %v", h.states)
	}
	if last := h.usage[len(h.usage)-1]; last != (TaskUsage{Attempts: 4, ExtraAttempts: 1}) {
		t.Fatalf("usage = %+v, want four attempts, one of them granted by the answer", last)
	}
}

// A human answer near the cap gets a fresh slice: at least a quarter of
// the task's agent time is left for the attempt it buys, granted on the
// task's row.
func TestWorkflowHumanAnswerGrantsATimeSlice(t *testing.T) {
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
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "try the other lock", Comment: 602})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 2 {
		t.Fatalf("final = %q after %d sessions, finals = %v; want done after the answer's attempt", final, len(h.sessions), h.finals)
	}
	want := TaskUsage{AgentTime: 24 * time.Minute, Attempts: 2, ExtraTime: 4 * time.Minute, ExtraAttempts: 1}
	if last := h.usage[len(h.usage)-1]; last != want {
		t.Fatalf("usage = %+v, want %+v", last, want)
	}
}

// A session that cannot start goes to a human like any escalation, but
// it ran no agent: its minute is no agent time, so the answer finds the
// whole 20 minutes left and grants no slice, and the session it buys runs
// on the same rung.
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
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the image is pushed", Comment: 602})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 2 {
		t.Fatalf("final = %q after %d sessions, finals = %v; want done after the answer's attempt", final, len(h.sessions), h.finals)
	}
	if !slices.Equal(h.escalated, []string{reason}) {
		t.Fatalf("escalations = %q, want the start failure's", h.escalated)
	}
	if second := h.sessions[1]; second.Model != h.sessions[0].Model {
		t.Fatalf("second session = %+v, want the first's model", second)
	}
	want := TaskUsage{AgentTime: 5 * time.Minute, Attempts: 2, ExtraAttempts: 1}
	if last := h.usage[len(h.usage)-1]; last != want {
		t.Fatalf("usage = %+v, want %+v", last, want)
	}
}

// A session that ends without a push hands on the findings it was given
// from outside the sessions, with its own lines in place of an earlier
// session's (roadmap 201): the red checks a session was sent to fix reach
// every next session, after a supervisor's climb and a human's answer
// too, and a session's own lines reach the next one only.
func TestWorkflowUnpushedSessionHandsOnItsFindings(t *testing.T) {
	cfg := testWorkflowConfig
	cfg.MaxSessions = 6
	h := newWorkflowHarness(t, TaskWorkflow{Config: func() WorkflowConfig { return cfg }})
	red := []string{"check ci/test failed: TestParse: want 3, got 4"}
	died := "previous session ended without a pushed commit: pod exited without a result"
	h.onSessions(pushed("s1", "c1"),
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "pod exited without a result", Findings: []string{died}},
		SessionResult{Outcome: OutcomeRetryStronger, SessionID: "s3", Findings: []string{"supervisor: looped on go test"}},
		SessionResult{Outcome: OutcomeEscalate, SessionID: "s4", Reason: "which parser?"},
		pushed("s5", "c5"))
	h.onChecks(ChecksResult{State: ChecksFailure, Findings: red}, ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the strict one", Comment: 602})
	h.signalAfter(8*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 5 {
		t.Fatalf("final = %q after %d sessions, finals = %v", final, len(h.sessions), h.finals)
	}
	want := [][]string{nil, red, {red[0], died}, {red[0], "supervisor: looped on go test"}, red}
	for i, w := range want {
		if !slices.Equal(h.sessions[i].Findings, w) {
			t.Errorf("session %d findings = %q, want %q", i+1, h.sessions[i].Findings, w)
		}
	}
}

// Roadmap 201: a review's findings outlast any run of unpushed sessions,
// which hand on only the newest one's own lines: after three unpushed
// sessions of 25 lines each the fifth session gets the review's 10 and
// the fourth's 25, and its FINDINGS, at most 60, holds every review line.
// Carrying every session's lines (85) would leave out the 25 oldest, the
// review's first.
func TestWorkflowReviewFindingsOutlastUnpushedSessions(t *testing.T) {
	cfg := testWorkflowConfig
	cfg.MaxSessions = 6
	h := newWorkflowHarness(t, TaskWorkflow{Config: func() WorkflowConfig { return cfg }})
	var review []string
	for i := range 10 {
		review = append(review, fmt.Sprintf("main.go: finding %d", i))
	}
	lines := func(session int) []string {
		var l []string
		for i := range 25 {
			l = append(l, fmt.Sprintf("s%d: ran x%d", session, i))
		}
		return l
	}
	h.onSessions(pushed("s1", "c1"),
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s2", Findings: lines(2)},
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s3", Findings: lines(3)},
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s4", Findings: lines(4)},
		pushed("s5", "c5"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	reviews := 0
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			if reviews++; reviews == 1 {
				return Review{Verdict: "concerns", Summary: "three faults", Findings: review}, nil
			}
			return Review{}, nil
		})
	h.signalAfter(8*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" || len(h.sessions) != 5 {
		t.Fatalf("final = %q after %d sessions, finals = %v", final, len(h.sessions), h.finals)
	}
	if got, want := h.sessions[4].Findings, slices.Concat(review, lines(4)); !slices.Equal(got, want) {
		t.Fatalf("fifth session's findings = %q, want %q", got, want)
	}
	pod := (&Runner{cfg: testRunnerConfig(), forgejo: &Forgejo{BotUser: "factory-agent"}}).workerPodSpec(Task{ID: "t1"}, sessionSpec{id: "s-5", findings: h.sessions[4].Findings})
	var carried []string
	if err := json.Unmarshal([]byte(containerEnv(pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any))["FINDINGS"]), &carried); err != nil {
		t.Fatal(err)
	}
	for _, r := range review {
		if !slices.Contains(carried, r) {
			t.Errorf("FINDINGS lacks the review's %q: %q", r, carried)
		}
	}
}

// A session that never started has no lines of its own: the next session
// gets the lines of the last one that ran (roadmap 201).
func TestWorkflowSessionThatCannotStartKeepsTheLastLines(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Findings: []string{"ran x"}},
		SessionResult{Outcome: OutcomeEscalate, SessionID: "s2", Reason: startEscalation("image pull", "push the worker image", "not found"), StartFailed: true},
		pushed("s3", "c3"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the image is pushed", Comment: 602})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" || len(h.sessions) != 3 {
		t.Fatalf("final = %q after %d sessions, finals = %v", final, len(h.sessions), h.finals)
	}
	if got := h.sessions[2].Findings; !slices.Equal(got, []string{"ran x"}) {
		t.Fatalf("third session's findings = %q, want the first's lines", got)
	}
}

// The slice a human answer buys is a quarter of the task's resolved agent
// time, floored at min(minSession, WallClockCap) so the attempt it buys is
// never born under the 5 minutes a session needs to start (ruling 237,
// change id "answer-slice"). Each case escalates with no agent time left,
// so the grant's ExtraTime is the slice itself, and the session after it
// (1 minute, well inside even the smallest slice) completes: the slice was
// the next session's remaining budget too.
func TestAnswerSliceIsAQuarterOfTheAgentTime(t *testing.T) {
	for name, tc := range map[string]struct {
		cap       time.Duration
		wantExtra time.Duration
	}{
		"2h, used 2h, a quarter":   {cap: 2 * time.Hour, wantExtra: 30 * time.Minute},
		"10m, used 10m, the floor": {cap: 10 * time.Minute, wantExtra: 5 * time.Minute},
		"30m, used 30m, a quarter": {cap: 30 * time.Minute, wantExtra: 7*time.Minute + 30*time.Second},
	} {
		cfg := testWorkflowConfig
		cfg.WallClockCap = tc.cap
		h := newWorkflowHarness(t, TaskWorkflow{Config: func() WorkflowConfig { return cfg }})
		h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(tc.cap).Return(
			func(ctx context.Context, in SessionInput) (SessionResult, error) {
				h.sessions = append(h.sessions, in)
				return SessionResult{Outcome: OutcomeEscalate, SessionID: "s1", Reason: "stuck"}, nil
			}).Once()
		h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(time.Minute).Return(
			func(ctx context.Context, in SessionInput) (SessionResult, error) {
				h.sessions = append(h.sessions, in)
				return pushed("s2", "c2"), nil
			})
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(tc.cap+5*time.Minute, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "try again", Comment: 602})
		h.signalAfter(tc.cap+20*time.Minute, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

		if final := h.run(t); final != "done" || len(h.sessions) != 2 {
			t.Fatalf("%s: final = %q after %d sessions, finals = %v; want done after the answer's attempt", name, final, len(h.sessions), h.finals)
		}
		want := TaskUsage{AgentTime: tc.cap + time.Minute, Attempts: 2, ExtraTime: tc.wantExtra, ExtraAttempts: 1}
		if last := h.usage[len(h.usage)-1]; last != want {
			t.Fatalf("%s: usage = %+v, want %+v", name, last, want)
		}
	}
}

// A run from before the change id "answer-slice" keeps WorkflowConfig's own
// AnswerSlice instead of a quarter of the cap: with cap 2h, a quarter would
// be 30m, but AnswerSlice 10m (never more than the cap) governs, as it did
// before the change.
func TestAnswerSliceBeforeTheChange(t *testing.T) {
	cfg := testWorkflowConfig
	cfg.WallClockCap, cfg.AnswerSlice = 2*time.Hour, 10*time.Minute
	h := newWorkflowHarness(t, TaskWorkflow{Config: func() WorkflowConfig { return cfg }})
	h.env.OnGetVersion("answer-slice", workflow.DefaultVersion, versionAnswerSlice).Return(workflow.DefaultVersion)
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(2 * time.Hour).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			return SessionResult{Outcome: OutcomeEscalate, SessionID: "s1", Reason: "stuck"}, nil
		}).Once()
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(time.Minute).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			return pushed("s2", "c2"), nil
		})
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(2*time.Hour+5*time.Minute, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "try again", Comment: 602})
	h.signalAfter(2*time.Hour+20*time.Minute, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 2 {
		t.Fatalf("final = %q after %d sessions, finals = %v; want done after the answer's attempt", final, len(h.sessions), h.finals)
	}
	want := TaskUsage{AgentTime: 2*time.Hour + time.Minute, Attempts: 2, ExtraTime: 10 * time.Minute, ExtraAttempts: 1}
	if last := h.usage[len(h.usage)-1]; last != want {
		t.Fatalf("usage = %+v, want %+v", last, want)
	}
}

// A new run continues the task's usage from its row (ruling 93: the caps
// count across attempts, so across runs too): the attempts left and the
// agent time left are what the earlier run left.
func TestWorkflowNewRunResumesAttemptsFromTheRow(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "running", Usage: TaskUsage{AgentTime: 12 * time.Minute, Attempts: 2}}
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(3 * time.Minute).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			return SessionResult{Outcome: OutcomeNoPush, SessionID: "s3"}, nil
		})

	h.signalAfter(time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" || len(h.escalated) != 1 || !strings.HasPrefix(h.escalated[0], capReached) {
		t.Fatalf("final = %q, finals = %v, escalations %q; want the last attempt, then the card, closed", final, h.finals, h.escalated)
	}
	if len(h.sessions) != 1 || h.sessions[0].Attempt != 3 {
		t.Fatalf("sessions = %+v, want one, the third", h.sessions)
	}
	if !slices.Equal(h.usage, []TaskUsage{{AgentTime: 15 * time.Minute, Attempts: 3}}) {
		t.Fatalf("usage = %+v", h.usage)
	}
}

// Before versionKeepWork a session started with any time left.
func TestWorkflowNewRunResumesAgentTimeFromTheRow(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnGetVersion("keep-work", workflow.DefaultVersion, versionKeepWork).Return(workflow.DefaultVersion)
	h.state = RunState{Status: "running", Usage: TaskUsage{AgentTime: 18 * time.Minute}}
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(3 * time.Minute).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			return SessionResult{Outcome: OutcomeNoPush, SessionID: "s1"}, nil
		})

	if final := h.run(t); final != "failed" || h.finals[0][1] != "wall-clock cap reached" || len(h.sessions) != 1 {
		t.Fatalf("final = %q, finals = %v, sessions = %d; want one session, then the cap", final, h.finals, len(h.sessions))
	}
	if !slices.Equal(h.usage, []TaskUsage{{AgentTime: 21 * time.Minute, Attempts: 1}}) {
		t.Fatalf("usage = %+v", h.usage)
	}
}

func TestWorkflowEscalationIssueClosedEndsClosed(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(SessionResult{Outcome: OutcomeEscalate, SessionID: "s1", Reason: "impossible"})
	h.signalAfter(time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" || len(h.sessions) != 1 {
		t.Fatalf("final = %q after %d sessions, want closed after one", final, len(h.sessions))
	}
	if !strings.Contains(h.finals[0][1], "issue closed") {
		t.Fatalf("reason = %q", h.finals[0][1])
	}
}

// A run that finds its task waiting for a human (factoryd restarted, the
// workflow was healed) asks again and waits; it runs no session first.
func TestWorkflowHealedRunWaitingForAHumanWaitsAgain(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "waiting-human"}
	h.onSessions(pushed("s1", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 900})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if len(h.escalated) != 1 || !strings.Contains(h.escalated[0], "restarted") {
		t.Fatalf("escalations = %q", h.escalated)
	}
	if len(h.sessions) != 1 || h.sessions[0].Findings != nil || !slices.Equal(h.sessions[0].Answers, []string{`sebastian, to "factoryd restarted while the task waited for a human; the question above still stands.": go on`}) {
		t.Fatalf("sessions = %+v", h.sessions)
	}
}

// Ruling 118: every human answer of the task travels with every later
// session as its own list, newest first, not as a finding, each with the
// question it answers (the escalation's first line): after two
// escalations the third session carries both answers, and the second
// session's own findings.
func TestWorkflowKeepsEveryHumanAnswer(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(
		SessionResult{Outcome: OutcomeEscalate, SessionID: "s1", Reason: "which file?", Findings: []string{"ran x"}},
		SessionResult{Outcome: OutcomeEscalate, SessionID: "s2", Reason: "which text?", Findings: []string{"ran y"}},
		pushed("s3", "c3"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "docs/a.txt", Comment: 602})
	h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "write ok", Comment: 603})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 3 {
		t.Fatalf("final = %q after %d sessions, finals = %v", final, len(h.sessions), h.finals)
	}
	if s := h.sessions[1]; !slices.Equal(s.Answers, []string{`sebastian, to "which file?": docs/a.txt`}) || !slices.Equal(s.Findings, []string{"ran x"}) {
		t.Fatalf("second session = %+v", s)
	}
	if s := h.sessions[2]; !slices.Equal(s.Answers, []string{`sebastian, to "which text?": write ok`, `sebastian, to "which file?": docs/a.txt`}) || !slices.Equal(s.Findings, []string{"ran y"}) {
		t.Fatalf("third session = %+v", s)
	}
}

// Workflows started before versionAnswers keep an answer as a finding,
// handed on before the last session's own findings.
func TestWorkflowDefaultVersionAnswersAsFindings(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnGetVersion("answers", workflow.DefaultVersion, versionAnswers).Return(workflow.DefaultVersion)
	h.onSessions(
		SessionResult{Outcome: OutcomeEscalate, SessionID: "s1", Reason: "which file?", Findings: []string{"ran x"}},
		SessionResult{Outcome: OutcomeEscalate, SessionID: "s2", Reason: "which text?", Findings: []string{"ran y"}},
		pushed("s3", "c3"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "docs/a.txt", Comment: 602})
	h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "write ok", Comment: 603})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 3 {
		t.Fatalf("final = %q after %d sessions", final, len(h.sessions))
	}
	if s := h.sessions[2]; s.Answers != nil || !slices.Equal(s.Findings, []string{"human sebastian answered the escalation: docs/a.txt",
		"human sebastian answered the escalation: write ok", "ran y"}) {
		t.Fatalf("third session = %+v", s)
	}
}

// An answer's body is cut to 4000 bytes in the list every later session
// gets (Linux caps one env string at 128 KiB); the issue keeps it whole.
func TestWorkflowCutsLongAnswers(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(SessionResult{Outcome: OutcomeEscalate, SessionID: "s1", Reason: "q?"}, pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: strings.Repeat("x", 200<<10), Comment: 602})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 2 {
		t.Fatalf("final = %q after %d sessions", final, len(h.sessions))
	}
	if want := `sebastian, to "q?": ` + strings.Repeat("x", 4000) + "…"; !slices.Equal(h.sessions[1].Answers, []string{want}) {
		t.Fatalf("answer of %d bytes, want %d", len(h.sessions[1].Answers[0]), len(want))
	}
}

// A healed run rebuilds the task's answers from Reconcile and tags the
// answer to its re-asked question with the original question; the re-ask
// keeps its choices.
func TestWorkflowHealedRunKeepsEarlierAnswers(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "waiting-human", Answers: []string{`sebastian, to "Which file?": a.txt`}, Question: "Which text?", Choices: []string{"hello", "bye"}}
	h.onSessions(pushed("s1", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "ok", Comment: 900})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if want := []string{`sebastian, to "Which text?": ok`, `sebastian, to "Which file?": a.txt`}; len(h.sessions) != 1 || !slices.Equal(h.sessions[0].Answers, want) {
		t.Fatalf("sessions = %+v, want answers %q", h.sessions, want)
	}
	// the re-ask offers the question's choices again
	if !slices.Equal(h.escalated, []string{healedReason}) || len(h.choices) != 1 || !slices.Equal(h.choices[0], []string{"hello", "bye"}) {
		t.Fatalf("escalations = %q with choices %q, want the healed re-ask with hello and bye", h.escalated, h.choices)
	}
}

// An alert card checks before its first model call that its alert still
// fires (roadmap 181, Stage 18 item 3.12): a burst whose alert is gone
// closes the card itself, failed with no session ever run. A run from
// before the version gate never asks.
func TestAlertCardClosesItselfBeforeFirstModelCall(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state.Source = SourceAlert
	prechecked := false
	h.env.OnActivity(ActivityAlertPreCheck, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) (bool, error) { prechecked = true; return false, nil })
	h.onSessions(SessionResult{Outcome: OutcomePushed, SessionID: "s1", Summary: "pushed"})
	if final := h.run(t); final != "failed" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if !prechecked {
		t.Fatal("the alert card ran no pre-check")
	}
	if len(h.sessions) != 0 {
		t.Fatalf("sessions = %d, want none: the card closed itself before its first model call", len(h.sessions))
	}
	if h.finals[0][1] != "alert no longer fires" {
		t.Fatalf("reason = %q, want %q", h.finals[0][1], "alert no longer fires")
	}

	// a firing alert lets the card run its session as any other would
	h2 := newWorkflowHarness(t)
	h2.state.Source = SourceAlert
	h2.env.OnActivity(ActivityAlertPreCheck, mock.Anything, "t1").Return(true, nil)
	h2.onSessions(pushed("s1", "c1"))
	h2.onChecks(ChecksResult{State: ChecksSuccess})
	h2.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h2.run(t); final != "done" {
		t.Fatalf("firing: final = %q, finals = %v", final, h2.finals)
	}
	if len(h2.sessions) != 1 {
		t.Fatalf("firing: sessions = %d, want the card's session", len(h2.sessions))
	}
}
