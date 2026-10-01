package factoryd

import (
	"bytes"
	"context"
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
// for outside accounts, every other stage on.
func testStages() []policy.Stage {
	return []policy.Stage{
		{Name: "screening", OnFindings: "human", Enabled: true, When: "outsider"},
		{Name: "checks", OnFindings: "agent", Enabled: true},
		{Name: "policy", OnFindings: "fail", Enabled: true},
		{Name: "migration", OnFindings: "human", Enabled: true},
		{Name: "reproduction", OnFindings: "agent", Enabled: true},
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
	escalated   []string // Escalate reasons, in order
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
		ActivityEnsureIssue:       func(ctx context.Context, taskID string) error { return nil },
		ActivitySetState:          func(ctx context.Context, taskID, state string) error { return nil },
		ActivityRunSession:        func(ctx context.Context, in SessionInput) (SessionResult, error) { return SessionResult{}, nil },
		ActivityOpenPullRequest:   func(ctx context.Context, taskID, sha string) (PullRequest, error) { return PullRequest{}, nil },
		ActivityWaitChecks:        func(ctx context.Context, taskID, sha string) (ChecksResult, error) { return ChecksResult{}, nil },
		ActivityPullRequestMerged: func(ctx context.Context, taskID string, number int64) (bool, error) { return false, nil },
		ActivityRecordStep:        func(ctx context.Context, taskID, step string, detail map[string]any) error { return nil },
		ActivityFinish:            func(ctx context.Context, taskID, final, reason string) error { return nil },
		ActivityReconcile:         func(ctx context.Context, taskID string) (RunState, error) { return RunState{}, nil },
		ActivityEscalate:          func(ctx context.Context, taskID, reason string) (int64, error) { return 0, nil },
		ActivityRecordUsage:       func(ctx context.Context, taskID string, u TaskUsage) error { return nil },
		ActivityGatePolicy: func(ctx context.Context, taskID string, pr int64, commit string, pin int64) (PolicyGate, error) {
			return PolicyGate{}, nil
		},
		ActivityGateMigration: func(ctx context.Context, taskID string, pr int64, migrations []string) error { return nil },
		ActivityGateReproduction: func(ctx context.Context, taskID string, pr int64, commit string) (ReproResult, error) {
			return ReproResult{}, nil
		},
		ActivityModelReview: func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review) (Review, error) {
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
		ActivityMerge: func(ctx context.Context, taskID string, pr int64, commit, approver string) (MergeResult, error) {
			return MergeResult{}, nil
		},
		ActivityReportMergeFailure:          func(ctx context.Context, taskID string, pr int64, commit, approver, reason string) error { return nil },
		ActivityReportMergeRetriesExhausted: func(ctx context.Context, taskID string, pr int64, commit, approver string) error { return nil },
		ActivitySuperviseGate: func(ctx context.Context, taskID string, in GateSupervision) (GateDecision, error) {
			return GateDecision{}, nil
		},
		ActivityAutoMergeNote: func(ctx context.Context, taskID string, pr int64, commit string, g GateResults, rv Review) error {
			return nil
		},
		ActivityRequestInfo:        func(ctx context.Context, taskID, reason string) (int64, error) { return 0, nil },
		ActivityCloseNotReproduced: func(ctx context.Context, taskID string) error { return nil },
		ActivityRollCommit:         func(ctx context.Context, taskID string) (string, error) { return "", nil },
		ActivityRerunChecks:        func(ctx context.Context, taskID, sha string) (string, error) { return "", nil },
		ActivityGateAudit: func(ctx context.Context, taskID string, pr PullRequest, commit string, pin int64) (AuditResult, error) {
			return AuditResult{}, nil
		},
		ActivityTaskConfig:   func(ctx context.Context, taskID string, pin int64) (TaskConfig, error) { return TaskConfig{}, nil },
		ActivityBranchHead:   func(ctx context.Context, taskID string) (string, error) { return "", nil },
		ActivityDependencies: func(ctx context.Context, taskID string) (DepState, error) { return DepState{}, nil },
		ActivityEnterQueue:   func(ctx context.Context, taskID string) (int, error) { return 0, nil },
		ActivitySyncWithMain: func(ctx context.Context, taskID string, pr int64, from string) (SyncResult, error) {
			return SyncResult{}, nil
		},
		ActivityLeaveQueue: func(ctx context.Context, taskID, why string) error { return nil },
		ActivityRunPlan: func(ctx context.Context, taskID, model string, pin int64, findings []string) (PlanResult, error) {
			return PlanResult{}, nil
		},
		ActivityRequestPlan: func(ctx context.Context, taskID string, revision int) (PlanRef, error) { return PlanRef{}, nil },
		ActivityCreateChildren: func(ctx context.Context, taskID string, revision int, approver string, pin int64) (ChildrenResult, error) {
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
	h.env.OnActivity(ActivityEscalate, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, reason string) (int64, error) {
			h.escalated = append(h.escalated, reason)
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
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review) (Review, error) {
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
// session, which starts from the last findings.
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

	if final := h.run(t); final != "failed" {
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

	if final := h.run(t); final != "failed" {
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
	// the timeout is tried MaximumAttempts (3) times, all one session
	if want := []string{"pr c1 after 0 cards", "approval c1"}; !slices.Equal(steps, want) || len(h.sessions) != 3 || h.branchReads != 1 {
		t.Fatalf("steps = %v, session tries = %d, branch reads = %d; want %v after three tries and one read", steps, len(h.sessions), h.branchReads, want)
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

func TestWorkflowIssueClosedWithoutMergeFails(t *testing.T) {
	for _, merged := range []bool{false, true} {
		h := newWorkflowHarness(t)
		h.onSessions(pushed("s1", "c1"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.env.OnActivity(ActivityPullRequestMerged, mock.Anything, "t1", int64(9)).Return(merged, nil)
		h.signalAfter(time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

		want, reason := "failed", "issue closed without merge"
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

func TestWorkflowIssueClosedDuringFirstAttemptOpensNoPR(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(10*time.Minute).Return(pushed("s1", "c1"), nil)
	h.signalAfter(5*time.Minute, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "failed" || h.finals[0][1] != "issue closed while no pull request of this workflow was open" {
		t.Fatalf("final = %q (%q)", final, h.finals[0][1])
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
		"issue closed":   {RunState{Status: "in-review", IssueClosed: true}, "failed", "issue closed without merge"},
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
	if final := h.run(t); final != "failed" || h.finishAt.IsZero() || h.finishAt.Sub(start) >= 10*time.Minute {
		t.Fatalf("session: final = %q after %s, want failed before the 10 min session returned", final, h.finishAt.Sub(start))
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

	if final := h.run(t); final != "failed" {
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
		SessionResult{Outcome: OutcomeEscalate, SessionID: "s1", Reason: "the check cannot pass\n\nProgress: 9 commands", Findings: []string{"ran x"}},
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
	if len(h.escalated) != 1 || !strings.Contains(h.escalated[0], "the check cannot pass") {
		t.Fatalf("escalations = %q", h.escalated)
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

	if final := h.run(t); final != "failed" || len(h.escalated) != 1 || !strings.HasPrefix(h.escalated[0], capReached) {
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

func TestWorkflowEscalationIssueClosedFails(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(SessionResult{Outcome: OutcomeEscalate, SessionID: "s1", Reason: "impossible"})
	h.signalAfter(time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "failed" || len(h.sessions) != 1 {
		t.Fatalf("final = %q after %d sessions, want failed after one", final, len(h.sessions))
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
// escalations the third session carries both answers.
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

// Workflows started before versionAnswers keep an answer as a finding of
// the next session only.
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
	if s := h.sessions[2]; s.Answers != nil || !slices.Equal(s.Findings, []string{"ran y", "human sebastian answered the escalation: write ok"}) {
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
// answer to its re-asked question with the original question.
func TestWorkflowHealedRunKeepsEarlierAnswers(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "waiting-human", Answers: []string{`sebastian, to "Which file?": a.txt`}, Question: "Which text?"}
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
}
