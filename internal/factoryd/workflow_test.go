package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

var testWorkflowConfig = WorkflowConfig{
	Models:        ModelLadder{Start: "factory/small", Stronger: "factory/big"},
	MaxSessions:   3,
	WallClockCap:  20 * time.Minute,
	AnswerSlice:   10 * time.Minute,
	ChecksTimeout: 45 * time.Minute,
	PolicyVersion: 7,
	StrongerAfter: 1,
	Stages:        testStages(),
}

// testStages is the Stage 9 workflow: screening on
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
	escalated   []string // Escalate's and OpenTrip's reasons, in order
	escalatedAt []time.Time
	choices     [][]string       // Escalate choices, in order
	successions []SuccessorInput // Successor filings, in order
	usage       []TaskUsage
	rowUsage    TaskUsage     // what ReadUsage reads back: the last recorded usage
	logs        *lockedBuffer // the workflow's log
	recorded    []recordedStep
	infoAsked   []string    // RequestInfo reasons, in order
	closedAt    []time.Time // CloseNotReproduced calls
	autoNotes   []string    // AutoMergeNote calls: "pr commit"
	escalateErr error       // Escalate's error, nil = none
	// escalationIDs are Escalate's comment ids in order; none: 601, 602, ...
	escalationIDs []int64
	// taskConfig answers TaskConfig when set; configPins are its calls' pins
	taskConfig *TaskConfig
	configPins []int64
	// reconcileAfter is how long Reconcile takes on the workflow's clock
	reconcileAfter time.Duration
	// escalateAfter is how long Escalate takes on the workflow's clock
	escalateAfter time.Duration
	// branchHead answers BranchHead; branchReads counts its calls
	branchHead  string
	branchReads int
	// headChecks are the heads HeadChecks (the approval stage's read) ran on
	headChecks []string
	// finish is a real Finish behind the stub; nil: none
	finish func(ctx context.Context, taskID, final, reason string) error
	// parked are ParkForTool's asks as "tool|need", arrived ImageArrived's
	// calls as "image|tool" and arrivedAt their times, left
	// LeaveImagePark's tool cards (onPark), leaveErr its error
	parked    []string
	arrived   []string
	arrivedAt []time.Time
	left      []string
	leaveErr  error // LeaveImagePark's error, nil = none
	// trips are OpenTrip's inputs in order. rungZero, when set, is the
	// resolution rung 0 takes on the nth trip (from 1), nil to ask. A trip
	// that asks opens case 700+n with Escalate's comment. ended are
	// EndCase's calls as "case reason"
	trips    []TripInput
	rungZero func(n int, in TripInput) *Resolution
	ended    []string
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
		ActivityUnjudgedCriteria:  func(ctx context.Context, taskID string) ([]string, error) { return nil, nil },
		ActivityHoldMerge:         func(ctx context.Context, taskID string, pr int64, commit, approver string) error { return nil },
		ActivityFinish:            func(ctx context.Context, taskID, final, reason string) error { return nil },
		ActivityReconcile:         func(ctx context.Context, taskID string) (RunState, error) { return RunState{}, nil },
		ActivityEscalate:          func(ctx context.Context, taskID, reason string, choices []string) (int64, error) { return 0, nil },
		ActivityRecordUsage: func(ctx context.Context, taskID string, u TaskUsage) error {
			h.rowUsage = u
			return nil
		},
		ActivityReadUsage: func(ctx context.Context, taskID string) (TaskUsage, error) { return h.rowUsage, nil },
		ActivitySuccessor: func(ctx context.Context, in SuccessorInput) (string, error) {
			h.successions = append(h.successions, in)
			return "t-succ", nil
		},
		ActivityGatePolicy: func(ctx context.Context, taskID string, pr int64, commit string, pin int64) (PolicyGate, error) {
			return PolicyGate{}, nil
		},
		ActivityGateMigration: func(ctx context.Context, taskID string, pr int64, migrations []string) error { return nil },
		ActivityGateReproduction: func(ctx context.Context, taskID string, pr int64, commit string) (ReproResult, error) {
			return ReproResult{}, nil
		},
		ActivityGateCriteria: func(ctx context.Context, taskID string, pr int64, commit string) (CriteriaResult, error) {
			return CriteriaResult{}, nil
		},
		ActivityHoldoutSettlement: func(ctx context.Context, taskID string, pin, pr int64, commit string, reds []HoldoutGateResult) (HoldoutSettlement, error) {
			return HoldoutSettlement{Settled: true}, nil
		},
		ActivityModelReview: func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			return Review{}, nil
		},
		ActivityGateCompanion: func(ctx context.Context, taskID string) ([]CompanionGate, error) {
			return nil, nil
		},
		ActivityScreen:         func(ctx context.Context, taskID string) (ScreenResult, error) { return ScreenResult{}, nil },
		ActivityJudgeScreening: func(ctx context.Context, taskID, verdict string) error { return nil },
		ActivityScreenAnswer: func(ctx context.Context, taskID, text string, pin int64) (ScreenResult, error) {
			return ScreenResult{}, nil
		},
		ActivitySizeCard: func(ctx context.Context, taskID string) (SizeResult, error) { return SizeResult{}, nil },
		ActivityRunReport: func(ctx context.Context, taskID, model string, pin int64) (ReportResult, error) {
			return ReportResult{}, nil
		},
		ActivityRequestApproval: func(ctx context.Context, taskID string, in ApprovalInput) error { return nil },
		ActivityMerge: func(ctx context.Context, taskID string, pr int64, commit, approver, checkedHead string, met []string) (MergeResult, error) {
			return MergeResult{}, nil
		},
		ActivityReportMergeFailure:          func(ctx context.Context, taskID string, pr int64, commit, approver, reason string) error { return nil },
		ActivityReportMergeRetriesExhausted: func(ctx context.Context, taskID string, pr int64, commit, approver string) error { return nil },
		ActivitySuperviseGate: func(ctx context.Context, taskID string, in GateSupervision) (GateDecision, error) {
			return GateDecision{}, nil
		},
		ActivityDropTrip: func(ctx context.Context, taskID string, trip GateTrip) error { return nil },
		ActivityWithdrawQuestion: func(ctx context.Context, taskID, reason string, comment int64, why string) (int64, error) {
			return 0, nil
		},
		ActivityAutoMergeNote: func(ctx context.Context, taskID string, pr int64, commit string, g GateResults, rv Review) error {
			return nil
		},
		ActivityRequestInfo:        func(ctx context.Context, taskID, reason string) (int64, error) { return 0, nil },
		ActivityCloseNotReproduced: func(ctx context.Context, taskID string) error { return nil },
		ActivityRollCommit:         func(ctx context.Context, taskID string) (string, error) { return "", nil },
		ActivityRerunChecks:        func(ctx context.Context, taskID, sha string) (string, error) { return "", nil },
		ActivityGateAudit: func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, round int) (AuditResult, error) {
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
		ActivityRecordNotRun: func(ctx context.Context, taskID, kind string, pr int64, commit string, round int, reason string) error {
			return nil
		},
		ActivityWithdrawApproval: func(ctx context.Context, taskID string) error { return nil },
		ActivityRunPlan: func(ctx context.Context, taskID, model string, pin int64, findings []string) (PlanResult, error) {
			return PlanResult{}, nil
		},
		ActivityRequestPlan: func(ctx context.Context, taskID string, revision int, pin int64) (PlanRef, error) {
			return PlanRef{}, nil
		},
		ActivityOpenPlanQuestions: func(ctx context.Context, taskID string, revision int) (int, error) { return 0, nil },
		ActivityAwaitSessionGap:   func(ctx context.Context, taskID string) error { return nil },
		ActivityAwaitRollHold:     func(ctx context.Context, taskID string) error { return nil },
		ActivityClearRollHold:     func(ctx context.Context, taskID string, merged bool) error { return nil },
		ActivityAwaitRollout: func(ctx context.Context, taskID string, pr int64, bound time.Duration) (RolloutResult, error) {
			return RolloutResult{}, nil
		},
		ActivityProviderWait:    func(ctx context.Context, taskID string, w ProviderWait) error { return nil },
		ActivityAlertPreCheck:   func(ctx context.Context, taskID string) (bool, error) { return true, nil },
		ActivityAttemptFunded:   func(ctx context.Context, taskID, model string) (string, error) { return "", nil },
		ActivitySessionFunded:   func(ctx context.Context, taskID, model string) (string, error) { return "", nil },
		ActivitySessionAnswered: func(ctx context.Context, in SessionAnswerInput) (bool, error) { return true, nil },
		ActivityDayResume:       func(ctx context.Context, taskID, model string, turned bool) (bool, error) { return turned, nil },
		ActivityCapResume:       func(ctx context.Context, in CapCheck) (CapResumed, error) { return CapResumed{Resume: in.Turned}, nil },
		ActivityEndCase:         func(ctx context.Context, caseID int64, reason string) error { return nil },
		ActivityRecordGrant:     func(ctx context.Context, taskID, by string, d policy.Draw, key string) error { return nil },
		ActivityOpenTrip:        func(ctx context.Context, in TripInput) (TripOutcome, error) { return TripOutcome{}, nil },
		ActivityParkForTool:     func(ctx context.Context, taskID, tool, need string) (ToolPark, error) { return ToolPark{}, nil },
		ActivityImageArrived:    func(ctx context.Context, taskID, image, tool string) (string, error) { return "", nil },
		ActivityLeaveImagePark:  func(ctx context.Context, taskID, toolTask string) error { return nil },
		ActivityBecomePlan: func(ctx context.Context, taskID string, sw PlanSwitch) (PlanSwitchResult, error) {
			return PlanSwitchResult{}, nil
		},
		ActivityReplanChild: func(ctx context.Context, childID, work string) (ReplanChildResult, error) {
			return ReplanChildResult{}, nil
		},
		ActivityReplanStopped:     func(ctx context.Context, planID string) (ReplanWait, error) { return ReplanWait{Stopped: true}, nil },
		ActivitySupersedeApproval: func(ctx context.Context, taskID, reason string) error { return nil },
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
			tc := TaskConfig{Process: "maintenance", Track: policy.TrackDirect, Class: policy.ClassCoder, Session: policy.SessionCode,
				Stages: w.Config().Stages, Queue: policy.QueueNone}
			if h.state.Report {
				tc.Class, tc.Session = policy.ClassAnalyzer, policy.SessionReport
			}
			return tc, nil
		})
	h.env.OnActivity(ActivityBranchHead, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) (string, error) { h.branchReads++; return h.branchHead, nil })
	h.env.OnActivity(ActivityReconcile, mock.Anything, "t1").AfterFn(func() time.Duration { return h.reconcileAfter }).Return(
		func(ctx context.Context, taskID string) (RunState, error) { return h.state, nil })
	h.env.OnActivity(ActivitySetState, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, state string) error { h.states = append(h.states, state); return nil })
	h.env.OnActivity(ActivityRecordStep, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, step string, detail map[string]any) error {
			h.recorded = append(h.recorded, recordedStep{step, detail, h.env.Now()})
			return nil
		})
	h.env.OnActivity(ActivityRecordUsage, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID string, u TaskUsage) error {
			h.usage, h.rowUsage = append(h.usage, u), u
			return nil
		})
	h.env.OnActivity(ActivityEscalate, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, reason string, choices []string) (int64, error) {
			h.escalated = append(h.escalated, reason)
			h.escalatedAt = append(h.escalatedAt, h.env.Now())
			h.choices = append(h.choices, choices)
			if n := len(h.escalated); n <= len(h.escalationIDs) {
				return h.escalationIDs[n-1], h.escalateErr
			}
			return int64(600 + len(h.escalated)), h.escalateErr
		}).AfterFn(func() time.Duration { return h.escalateAfter })
	h.env.OnActivity(ActivityOpenTrip, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in TripInput) (TripOutcome, error) {
			h.trips = append(h.trips, in)
			n := len(h.trips)
			if h.rungZero != nil {
				if res := h.rungZero(n, in); res != nil {
					return TripOutcome{CaseID: int64(700 + n), Resolution: res}, nil
				}
			}
			h.escalated = append(h.escalated, in.Reason)
			h.escalatedAt = append(h.escalatedAt, h.env.Now())
			h.choices = append(h.choices, in.Choices)
			comment := int64(600 + len(h.escalated))
			if k := len(h.escalated); k <= len(h.escalationIDs) {
				comment = h.escalationIDs[k-1]
			}
			return TripOutcome{CaseID: int64(700 + n), Comment: comment, Rung: 3}, h.escalateErr
		}).AfterFn(func() time.Duration { return h.escalateAfter })
	h.env.OnActivity(ActivityEndCase, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, caseID int64, reason string) error {
			h.ended = append(h.ended, fmt.Sprintf("%d %s", caseID, reason))
			return nil
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
			if h.finish != nil {
				return h.finish(ctx, taskID, final, reason)
			}
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

// beforeCases runs the workflow as a run from before versionCases: it asks
// through Escalate, an answer buys a session and agent time, and the agent
// time's cap fails a card with nothing pushed.
func (h *workflowHarness) beforeCases() {
	h.env.OnGetVersion("cases", workflow.DefaultVersion, versionCases).Return(workflow.DefaultVersion)
}

// beforeQueueWaits runs the workflow as a run from before
// versionQueueWaits: a queued card holds its entry through the approval
// wait and the merge.
func (h *workflowHarness) beforeQueueWaits() {
	h.env.OnGetVersion("queue-waits", workflow.DefaultVersion, versionQueueWaits).Return(workflow.DefaultVersion)
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

// checkinPushed is a session whose check-in handed over and whose
// conversation then pushed commit.
func checkinPushed(session, commit string, ck *CheckInOutcome) SessionResult {
	r := pushed(session, commit)
	r.CheckIn = ck
	return r
}

// checkinStopped is a session the check-in stopped before any conversation
// ran: outcome is what its decision maps to.
func checkinStopped(session string, outcome SessionOutcome, ck *CheckInOutcome) SessionResult {
	return SessionResult{SessionID: session, Outcome: outcome, Reason: ck.Reason, CheckIn: ck}
}

// askTaskConfig is the harness's default TaskConfig with the check-in
// bought on.
func askTaskConfig() *TaskConfig {
	return &TaskConfig{Process: "maintenance", Track: policy.TrackDirect, Class: policy.ClassCoder, Session: policy.SessionCode,
		Stages: testStages(), Queue: policy.QueueNone, CheckIn: time.Minute}
}

func TestWorkflowHappyPathToMerged(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(pushed("s1", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if len(h.sessions) != 1 || h.sessions[0].Model != "factory/small" || h.sessions[0].StartRef != "" || h.sessions[0].Attempt != 1 {
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
	h.taskConfig = &TaskConfig{Process: "maintenance", Track: policy.TrackDirect, Class: policy.ClassCoder, Session: policy.SessionCode,
		Stages: testStages(), Queue: policy.QueueNone, Stronger: true}
	h.onSessions(pushed("s1", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if len(h.sessions) != 1 || h.sessions[0].Model != "factory/big" {
		t.Fatalf("sessions = %+v, want the first session on the stronger rung factory/big", h.sessions)
	}
}

// The version the workflow started under reaches every session and the
// model review, which read their limits from it.
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
	h.taskConfig = &TaskConfig{Process: "maintenance", Track: policy.TrackDirect, Class: policy.ClassCoder, Session: policy.SessionCode,
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
	if second.Model != "factory/big" || second.StartRef != "c1" || second.ParentSessionID != "s1" ||
		!slices.Equal(second.Findings, red) || second.Attempt != 2 {
		t.Fatalf("second attempt = %+v, want frontier from c1, parent s1, the red checks as findings", second)
	}
}

// The next session is told how its parent stopped: the reason, the
// outcome and the trip ride its input, for the working state's heading.
func TestSecondSessionInputNamesHowTheFirstStopped(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "supervision trip: wall_clock", Trip: "wall_clock"}, pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if len(h.sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(h.sessions))
	}
	if first := h.sessions[0]; first.ParentStop != "" || first.ParentOutcome != "" || first.ParentTrip != "" {
		t.Fatalf("first session's input = %+v, want no parent stop, no parent outcome and no parent trip", first)
	}
	if second := h.sessions[1]; second.ParentSessionID != "s1" || second.ParentStop != "supervision trip: wall_clock" || second.ParentOutcome != OutcomeNoPush || second.ParentTrip != "wall_clock" {
		t.Fatalf("second session's input = %+v, want parent s1 stopped on the wall-clock trip with no push", second)
	}
}

// A stop a check-in judged a dead end rides the next input as such; any
// other stop does not.
func TestNextSessionInputCarriesTheDeadEnd(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(SessionResult{Outcome: OutcomeRetryStronger, SessionID: "s1", Summary: "check-in: retry_stronger after repetition: d", Trip: "repetition", DeadEnd: true},
		SessionResult{Outcome: OutcomeRetryStronger, SessionID: "s2", Summary: "check-in: retry_stronger after cheap_cap: d", Trip: "cheap_cap"},
		pushed("s3", "c3"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if len(h.sessions) != 3 {
		t.Fatalf("sessions = %d, want 3", len(h.sessions))
	}
	if h.sessions[0].ParentDeadEnd || !h.sessions[1].ParentDeadEnd || h.sessions[2].ParentDeadEnd {
		t.Fatalf("dead ends = %v, %v, %v; want only the second input's", h.sessions[0].ParentDeadEnd, h.sessions[1].ParentDeadEnd, h.sessions[2].ParentDeadEnd)
	}
}

// The parent's stop rides every next input cut at the working state's
// bound: the agent's long last message is not copied into the history
// whole.
func TestParentStopIsCutAtTheCarryBound(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: strings.Repeat("é", 150)}, pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if len(h.sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(h.sessions))
	}
	if got, want := h.sessions[1].ParentStop, strings.Repeat("é", 100)+"…"; got != want {
		t.Fatalf("ParentStop = %q (%d bytes), want 100 é and the mark", got, len(got))
	}
}

// A session that ran no conversation left no record and never becomes the
// parent: after a check-in that stopped its session for a human, or a
// start that failed, the session the answer buys continues the last one
// that ran.
func TestThirdSessionContinuesTheLastSessionThatRan(t *testing.T) {
	for name, stopped := range map[string]SessionResult{
		"checkin_human": checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman,
			Reason: "the session died with its work unpushed twice",
			Record: &CheckInRecord{Moment: CheckInDead, Session: "s2", ParentSession: "s1"}}),
		"start failed": {Outcome: OutcomeEscalate, SessionID: "s2", StartFailed: true,
			Summary: "the image cannot be pulled", Reason: "the image cannot be pulled; fix it, then answer"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			h.taskConfig = askTaskConfig()
			deadSessions(h, SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "supervision trip: wall_clock"}, stopped, pushed("s3", "c3"))
			h.onChecks(ChecksResult{State: ChecksSuccess})
			h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
			h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

			if final := h.run(t); final != "done" {
				t.Fatalf("final = %q, escalations = %q", final, h.escalated)
			}
			if len(h.sessions) != 3 {
				t.Fatalf("sessions = %d, want 3", len(h.sessions))
			}
			if second := h.sessions[1]; second.ParentSessionID != "s1" {
				t.Fatalf("second session's parent = %q, want s1", second.ParentSessionID)
			}
			if third := h.sessions[2]; third.ParentSessionID != "s1" || third.ParentStop != "supervision trip: wall_clock" || third.ParentOutcome != OutcomeNoPush {
				t.Fatalf("third session's input = %+v, want parent s1 stopped on the wall-clock trip with no push", third)
			}
		})
	}
	// a check-in that dropped every finding stopped its pod before the
	// conversation: the session after the re-review continues s1
	t.Run("checkin_dropped", func(t *testing.T) {
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
			checkinStopped("s3", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, Reason: "re-raised after a drop"}))
		g.onChecks(ChecksResult{State: ChecksSuccess})
		g.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

		if final := g.run(t); final != "closed" {
			t.Fatalf("final = %q, want closed", final)
		}
		if len(g.sessions) != 3 {
			t.Fatalf("sessions = %d, want 3", len(g.sessions))
		}
		if third := g.sessions[2]; third.ParentSessionID != "s1" || third.ParentOutcome != OutcomePushed {
			t.Fatalf("third session's input = %+v, want parent s1, which pushed", third)
		}
	})
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
	if second.StartRef != "c1" || second.Model != "factory/small" || len(second.Findings) != 1 ||
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
	if h.sessions[2].Model != "factory/small" || len(h.escalated) != 0 {
		t.Fatalf("third attempt on %s, escalations %q; want the start rung and no card", h.sessions[2].Model, h.escalated)
	}
}

// Sessions that run out of attempts without a pushed commit
// open the card with every finding so far; the answer buys one more
// session, which starts with the last session's own findings, the
// earlier ones' being stale (roadmap 201).
func TestWorkflowSessionCapOpensTheCard(t *testing.T) {
	h := newWorkflowHarness(t)
	h.beforeCases()
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
	if fourth.Attempt != 4 || fourth.Model != "factory/big" || fourth.ParentSessionID != "s3" ||
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
	h.beforeCases()
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

// A group revert's agent card has two sessions, not the policy's three:
// its TaskConfig says so, and the workflow asks a person after the second.
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
	h.beforeCases()
	h.taskConfig = &TaskConfig{Process: "maintenance", Track: policy.TrackDirect, Class: policy.ClassCoder, Session: policy.SessionCode,
		Stages: testStages(), Queue: policy.QueueNone, Sessions: groupRevertSessions}
	h.onSessions(
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "no changes to push", Findings: []string{"f1"}},
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "no changes to push", Findings: []string{"f2"}},
		pushed("s3", "c3"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "keep the later fix", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	wantCard := capReached + "\nlast: no changes to push\n- session, attempt 1: f1\n- session, attempt 2: f2"
	if !slices.Equal(h.escalated, []string{wantCard}) {
		t.Fatalf("escalations = %q, want the card after the second session %q", h.escalated, wantCard)
	}
	if len(h.sessions) != 3 || h.sessions[0].Sessions != 2 || h.sessions[2].Attempt != 3 {
		t.Fatalf("sessions = %+v, want two of two, then the one the answer bought", h.sessions)
	}
}

// A run that starts with every attempt used opens the card before any
// session.
func TestWorkflowRunAtTheSessionCapOpensTheCard(t *testing.T) {
	h := newWorkflowHarness(t)
	h.beforeCases()
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

// A card whose task's remaining budget cannot
// fund a call refuses the attempt an answer would buy. No session starts
// (no key is minted), no failure round is added, and the waiting-human
// card says why an answer will not run: each answer re-asks the same
// refusal, and only a close ends the task. The answer's grant is a run's
// from before versionCases.
func TestWorkflowUnfundedAnswerRefusesTheAttempt(t *testing.T) {
	h := newWorkflowHarness(t)
	h.beforeCases()
	h.state = RunState{Status: "waiting-human", Question: "which alias?"}
	unfunded := budgetRefused + ": 0.080 EUR of the 2.000 EUR budget remains, and the cheapest call needs more. Raise the budget (Fund on the board) and answer, or close the card."
	h.env.OnActivity(ActivitySessionFunded, mock.Anything, "t1", mock.Anything).Return(unfunded, nil)
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

// A fundable remainder grants the attempt exactly as before (the
// attempt grant's floor guards only the unfunded one): the answer at the
// session cap buys the next session once the floor read clears it.
func TestWorkflowFundedAnswerGrantsTheAttemptAsBefore(t *testing.T) {
	h := newWorkflowHarness(t)
	h.beforeCases()
	h.state = RunState{Status: "running", Usage: TaskUsage{AgentTime: 12 * time.Minute, Attempts: 3}}
	floors := 0
	h.env.OnActivity(ActivitySessionFunded, mock.Anything, "t1", mock.Anything).Return(
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

// refusedThenFunded answers the funding floor: the nth call (1-based) in
// refusing is refused with a budget refusal whose remainder is n tenths of a
// euro, the others funded. Both the floor of the new gate and the old one
// answer it; the reads it returns count each one's calls.
func (h *workflowHarness) refusedThenFunded(refusing ...int) map[string]int {
	reads, calls := map[string]int{}, 0
	floor := func(activity string) func(ctx context.Context, taskID, model string) (string, error) {
		return func(ctx context.Context, taskID, model string) (string, error) {
			calls++
			reads[activity]++
			if slices.Contains(refusing, calls) {
				return fmt.Sprintf("%s: Raise the budget (Fund on the board) and answer, or close the card.\n%.3f EUR of the 2.000 EUR budget remains, and a session on %s needs 0.300000 EUR",
					budgetRefused, float64(calls)/10, model), nil
			}
			return "", nil
		}
	}
	h.env.OnActivity(ActivitySessionFunded, mock.Anything, "t1", mock.Anything).Return(floor(ActivitySessionFunded))
	h.env.OnActivity(ActivityAttemptFunded, mock.Anything, "t1", mock.Anything).Return(floor(ActivityAttemptFunded))
	return reads
}

// An answer to a session that took no number buys none: the start failure
// is no session, so its answer buys only agent time, and the cap of 3 asks
// after three answered sessions. Attempts 1 (the start failure), 1, 2 and 3
// run, the cap question comes, and its answer buys attempt 4.
func TestWorkflowAnswerToAnUncountedSessionBuysNoAttempt(t *testing.T) {
	h := newWorkflowHarness(t)
	h.beforeCases()
	h.onSessions(
		SessionResult{Outcome: OutcomeEscalate, SessionID: "starved", StartFailed: true, Reason: "the pod could not start: image pull refused"},
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s2"}, SessionResult{Outcome: OutcomeNoPush, SessionID: "s3"},
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s4"}, pushed("s5", "c5"))
	h.env.OnActivity(ActivitySessionAnswered, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionAnswerInput) (bool, error) { return in.SessionID != "starved", nil })
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the registry is back", Comment: 602})
	h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 603})
	h.signalAfter(4*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, escalations = %q", final, h.escalated)
	}
	var attempts []int
	for _, in := range h.sessions {
		attempts = append(attempts, in.Attempt)
	}
	if !slices.Equal(attempts, []int{1, 1, 2, 3, 4}) || len(h.escalated) != 2 || !strings.HasPrefix(h.escalated[1], capReached) {
		t.Fatalf("attempts = %v, escalations %q; want 1 1 2 3 4 and the cap question after the third answered session", attempts, h.escalated)
	}
	if u := h.usage[len(h.usage)-1]; u.Attempts != 4 || u.ExtraAttempts != 1 {
		t.Fatalf("usage = %+v, want four sessions counted and the cap answer's one extra attempt", u)
	}
}

// A session the gateway starved is no session: #839 ran attempts 1, 2, 3 and
// 5 and #854 1, 2 and 4 because refused attempts took numbers. A session that
// got no model answer counts none, whatever ended it, so the fourth run here
// is the second session and the cap of 3 is never reached.
func TestWorkflowStarvedSessionsTakeNoNumber(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "the gateway refused a call"},
		SessionResult{Outcome: OutcomeNoPush, SessionID: "starved", Summary: "the gateway refused a call"},
		SessionResult{Outcome: OutcomeNoPush, SessionID: "starved", Summary: "the gateway refused a call"},
		pushed("s4", "c4"))
	h.env.OnActivity(ActivitySessionAnswered, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionAnswerInput) (bool, error) { return in.SessionID != "starved", nil })
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, escalations = %q", final, h.escalated)
	}
	var attempts []int
	for _, in := range h.sessions {
		attempts = append(attempts, in.Attempt)
	}
	if !slices.Equal(attempts, []int{1, 2, 2, 2}) || len(h.escalated) != 0 {
		t.Fatalf("attempts = %v, escalations %q; want 1 2 2 2 and no cap question", attempts, h.escalated)
	}
	if u := h.usage[len(h.usage)-1]; u.Attempts != 2 {
		t.Fatalf("usage = %+v, want two sessions counted", u)
	}
}

// A run from before the gate counts every session that ran.
func TestWorkflowBeforeSessionLadderStarvedSessionsTookNumbers(t *testing.T) {
	h := newWorkflowHarness(t)
	h.beforeCases()
	h.env.OnGetVersion("session-ladder", workflow.DefaultVersion, versionSessionLadder).Return(workflow.DefaultVersion)
	h.onSessions(
		SessionResult{Outcome: OutcomeNoPush, SessionID: "starved"}, SessionResult{Outcome: OutcomeNoPush, SessionID: "starved"},
		SessionResult{Outcome: OutcomeNoPush, SessionID: "starved"}, pushed("s4", "c4"))
	h.env.OnActivity(ActivitySessionAnswered, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionAnswerInput) (bool, error) { return in.SessionID != "starved", nil })
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(3*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(6*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 4 || h.sessions[3].Attempt != 4 || len(h.escalated) != 1 || !strings.HasPrefix(h.escalated[0], capReached) {
		t.Fatalf("final = %q, sessions %d, escalations %q; want the cap question after three counted sessions and the fourth as attempt 4", final, len(h.sessions), h.escalated)
	}
}

// Three sessions in a row that got no model answer ask a human, naming the
// last stop, and a session that got one starts the count again. Two starved
// sessions, an answered one and three starved ones (the fifth climbing the
// rung) run as attempts 1 1 1 2 2 2; the question comes after the sixth, and
// its answer buys agent time but no attempt, so the seventh runs as attempt 2
// on the stronger rung.
func TestWorkflowThreeUnansweredSessionsInARowAskAHuman(t *testing.T) {
	h := newWorkflowHarness(t)
	starved := func(o SessionOutcome, stop string) SessionResult {
		return SessionResult{Outcome: o, SessionID: "starved", Summary: stop}
	}
	h.onSessions(starved(OutcomeNoPush, "stop 1"), starved(OutcomeNoPush, "stop 2"),
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s3", Summary: "no changes to push"},
		starved(OutcomeNoPush, "stop 4"), starved(OutcomeRetryStronger, "stop 5"), starved(OutcomeNoPush, "stop 6"),
		pushed("s7", "c7"))
	h.env.OnActivity(ActivitySessionAnswered, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionAnswerInput) (bool, error) { return in.SessionID != "starved", nil })
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the gateway is back", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, escalations = %q", final, h.escalated)
	}
	var attempts []int
	for _, in := range h.sessions {
		attempts = append(attempts, in.Attempt)
	}
	if !slices.Equal(attempts, []int{1, 1, 1, 2, 2, 2, 2}) {
		t.Fatalf("attempts = %v, want 1 1 1 2 2 2 2", attempts)
	}
	want := "No session got a model answer: 3 sessions in a row ended without one and took no number of the session cap. " +
		"Find what kept the model from answering (the gateway, the provider or the session's pod), then answer to start the next session. The last stop: stop 6"
	if !slices.Equal(h.escalated, []string{want}) || escalationSource(want) != "no_answer" {
		t.Fatalf("escalations = %q, want %q from source no_answer", h.escalated, want)
	}
	if in := h.sessions[6]; in.Model != "factory/big" || len(in.Answers) != 1 || !strings.HasSuffix(in.Answers[0], ": the gateway is back") {
		t.Fatalf("seventh session = %+v, want the stronger rung with the answer", in)
	}
	if u := h.usage[len(h.usage)-1]; u != (TaskUsage{Attempts: 2}) {
		t.Fatalf("usage = %+v, want two sessions counted and no attempt bought", u)
	}
}

// The session_started record names the model the session started on; a
// climb inside the session is model_switched's record (roadmap 222, item 5).
func TestWorkflowSessionAnsweredNamesTheStartModel(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Model: "factory/big", Summary: "no changes to push"}, pushed("s2", "c2"))
	var models []string
	h.env.OnActivity(ActivitySessionAnswered, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionAnswerInput) (bool, error) {
			models = append(models, in.Model)
			return true, nil
		})
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if want := []string{"factory/small", "factory/big"}; !slices.Equal(models, want) {
		t.Fatalf("recorded models = %q, want %q: the first session started cheap and climbed, the second started where it ended", models, want)
	}
}

// A session reattached after a restart began before this run: its own start
// is the since of the answer read, so the calls it made before the restart
// count. A fresh session carries no start and the read takes the run's.
func TestWorkflowReattachedSessionIsReadSinceItsOwnStart(t *testing.T) {
	h := newWorkflowHarness(t)
	podStart := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	var ran []time.Time
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			ran = append(ran, h.env.Now())
			if len(h.sessions) == 1 {
				return SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "no changes to push", Started: podStart}, nil
			}
			return pushed("s2", "c2"), nil
		})
	var since []time.Time
	h.env.OnActivity(ActivitySessionAnswered, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionAnswerInput) (bool, error) {
			since = append(since, in.Since)
			return true, nil
		})
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if len(since) != 2 || !since[0].Equal(podStart) || !since[1].Equal(ran[1]) {
		t.Fatalf("since = %v, want the reattached pod's start %v, then the second session's run %v", since, podStart, ran[1])
	}
}

// The budget branch used to discard the change requests that answered it
// and to count the refused attempt: session 1 pushes, the red checks go back,
// the floor refuses session 2, a change request on the pull request answers
// the wait, and session 2 starts as attempt 2 with both findings.
func TestWorkflowFundRefusalKeepsTheAnswerAndTheNumber(t *testing.T) {
	change := "reviewer sebastian requested changes: rename it"
	run := func(t *testing.T, pinned bool) (*workflowHarness, map[string]int) {
		h := newWorkflowHarness(t)
		if pinned {
			h.env.OnGetVersion("session-ladder", workflow.DefaultVersion, versionSessionLadder).Return(workflow.DefaultVersion)
		}
		reads := h.refusedThenFunded(2)
		h.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
		h.onChecks(ChecksResult{State: ChecksFailure, Findings: []string{"red"}}, ChecksResult{State: ChecksSuccess})
		h.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", Body: "rename it", PR: 9})
		h.signalAfter(4*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
		if final := h.run(t); final != "done" {
			t.Fatalf("final = %q, finals %v", final, h.finals)
		}
		return h, reads
	}
	h, reads := run(t, false)
	if len(h.sessions) != 2 || h.sessions[1].Attempt != 2 || !slices.Equal(h.sessions[1].Findings, []string{"red", change}) {
		t.Fatalf("sessions = %+v, want the second as attempt 2 with the red check and the change request", h.sessions)
	}
	if reads[ActivitySessionFunded] != 3 || reads[ActivityAttemptFunded] != 0 {
		t.Fatalf("floor reads = %v, want three of SessionFunded and none of AttemptFunded", reads)
	}
	h, reads = run(t, true)
	if len(h.sessions) != 2 || h.sessions[1].Attempt != 3 || !slices.Equal(h.sessions[1].Findings, []string{"red"}) {
		t.Fatalf("pinned sessions = %+v, want the old run's attempt 3 and the dropped change request", h.sessions)
	}
	if reads[ActivityAttemptFunded] != 3 || reads[ActivitySessionFunded] != 0 {
		t.Fatalf("pinned floor reads = %v, want three of AttemptFunded and none of SessionFunded", reads)
	}
}

// A fund too small to clear the floor asks again, each time naming the
// amount; the session starts once the floor is met.
func TestWorkflowFundThatMissesTheFloorAsksAgain(t *testing.T) {
	h := newWorkflowHarness(t)
	reads := h.refusedThenFunded(1, 2)
	h.onSessions(pushed("s1", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "funded 0.1", Comment: 602})
	h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "funded 1.0", Comment: 603})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if len(h.escalated) != 2 || !strings.Contains(h.escalated[0], "0.100 EUR of the 2.000 EUR budget remains, and a session on factory/small needs 0.300000 EUR") ||
		!strings.Contains(h.escalated[1], "0.200 EUR of the 2.000 EUR budget remains, and a session on factory/small needs 0.300000 EUR") || reads[ActivitySessionFunded] != 3 {
		t.Fatalf("escalations %q after floor reads %v; want two questions, each naming its read's remainder and the amount, then the session", h.escalated, reads)
	}
	if len(h.sessions) != 1 || h.sessions[0].Attempt != 1 || len(h.sessions[0].Answers) != 2 {
		t.Fatalf("sessions = %+v, want one attempt-1 session with both answers", h.sessions)
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

// The granted session after a cap card runs a check-in when the round
// left gate findings: the ask carries both the checks' finding (from the
// verification) and the change request, each named by where it came from,
// and a handover decision records the handover step.
func TestWorkflowCapReturnCarriesCheckin(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(time.Minute).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			// Temporal retries a failed session: record each attempt once
			if len(h.sessions) < in.Attempt {
				h.sessions = append(h.sessions, in)
			}
			switch in.Attempt {
			case 1:
				return pushed("s1", "c1"), nil
			case 3:
				return checkinPushed("s3", "c3", &CheckInOutcome{Decision: CheckHandover, Handover: "the red test is next",
					Verdicts: map[string]CheckVerdict{"red": {State: CheckConfirmed, Round: 1}}}), nil
			}
			return SessionResult{}, errors.New("pod lost")
		})
	h.onChecks(ChecksResult{State: ChecksFailure, Findings: []string{"red"}})
	h.signalAfter(90*time.Second, SignalChangesRequested, TaskSignal{Author: "sebastian", Body: "rename it", PR: 9})
	h.signalAfter(5*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" {
		t.Fatalf("final = %q, want closed", final)
	}
	change := "reviewer sebastian requested changes: rename it"
	if len(h.sessions) < 3 || h.sessions[2].CheckInAsk == nil {
		t.Fatalf("sessions = %+v, want the granted session carrying an ask", h.sessions)
	}
	ask := h.sessions[2].CheckInAsk
	if ask.Moment != CheckInGate || ask.ParentSession != "s1" ||
		ask.Findings[0] != (CheckFinding{Text: "red", Source: "review"}) || ask.Findings[1] != (CheckFinding{Text: change, Source: "changes"}) {
		t.Fatalf("the ask = %+v, want the red checks from the review and the change from the changes", ask)
	}
	var handover *recordedStep
	for i := range h.recorded {
		if h.recorded[i].step == "handover" {
			handover = &h.recorded[i]
		}
	}
	if handover == nil || handover.detail["handover"] != "the red test is next" || handover.detail["session"] != "s3" {
		t.Fatalf("handover steps = %+v, want s3's note", h.recorded)
	}
}

// The next session's input carries what the last round judged and what
// the last session handed over: the gates once a review round gated, nil
// before any review ran, the handover empty until the workflow sets it.
func TestSessionInputCarriesLastGatesAndHandover(t *testing.T) {
	h := newWorkflowHarness(t)
	rounds := 0
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			if rounds++; rounds == 1 {
				return Review{Verdict: "concerns", Summary: "the handler races", Findings: []string{"the handler races"}}, nil
			}
			return Review{}, nil
		})
	h.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" || len(h.sessions) != 2 {
		t.Fatalf("final = %q after %d sessions, finals = %v", final, len(h.sessions), h.finals)
	}
	if h.sessions[1].LastGates == nil || !reflect.DeepEqual(*h.sessions[1].LastGates, GateResults{Pin: 7, ChecksPassed: true}) {
		t.Fatalf("second session's gates = %+v, want the review round's (pin 7, checks passed)", h.sessions[1].LastGates)
	}
	if h.sessions[1].Handover != "" {
		t.Fatalf("second session's handover = %q, want none yet", h.sessions[1].Handover)
	}

	fresh := newWorkflowHarness(t)
	fresh.onSessions(pushed("s1", "c1"))
	fresh.onChecks(ChecksResult{State: ChecksSuccess})
	fresh.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := fresh.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if fresh.sessions[0].LastGates != nil || fresh.sessions[0].Handover != "" {
		t.Fatalf("first session = %+v, want nil gates and an empty handover before any review", fresh.sessions[0])
	}
}

// A restart between the findings and the check-in is a stated degradation:
// Reconcile rebuilds the reviews but not gateFindings, so the round runs
// as it did before the check-in existed — the reviewer's findings reach
// the session with no ask — and never skips a human.
func TestWorkflowRestartBetweenFindingsAndCheckinRunsNoCheckin(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	change := "reviewer sebastian requested changes: rename it"
	h.state = RunState{Status: "in-review", PR: PullRequest{Number: 9}, Head: "c1", Findings: []string{change}}
	h.onSessions(pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if len(h.sessions) != 1 || h.sessions[0].CheckInAsk != nil || !slices.Equal(h.sessions[0].Findings, []string{change}) {
		t.Fatalf("sessions = %+v, want one on the change request with no check-in", h.sessions)
	}
}

// The card keeps the newest findings within maxCard, each cut at
// maxCardLine, and says how many earlier ones it left out.
func TestSessionCapCardIsBounded(t *testing.T) {
	var run taskRun
	for i := range 20 {
		run.history = append(run.history, fmt.Sprintf("%02d", i)+strings.Repeat("x", 1498))
	}
	card := run.sessionCapCard(capReached, strings.Repeat("y", 1500), []string{"new"})
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
		{"repeat trip", RunState{Returns: map[string]int{"supervisor:repetition": 2}}, "factory/big"},
		{"retry_stronger", RunState{Climbed: true}, "factory/big"},
		{"one return", RunState{Returns: map[string]int{"supervisor:repetition": 1, "checks": 1}}, "factory/small"},
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
		{TripRepetition, "factory/big"},
		{TripNoProgress, "factory/small"},
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
		if want := []string{"factory/small", "factory/small", c.want}; !slices.Equal(models, want) {
			t.Fatalf("second trip %s: models = %v, want %v", c.second, models, want)
		}
	}
}

func TestWorkflowWallClockExpiryFails(t *testing.T) {
	h := newWorkflowHarness(t)
	h.beforeCases()
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
	h.beforeCases()
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
	h.beforeCases()
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
		h.beforeCases()
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
	h.beforeCases()
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
	h.beforeCases()
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
	if want := []string{timeCapReached + "\nlast: checks findings went back to the agent\n- checks, round 1: gosec G706"}; !slices.Equal(h.escalated, want) {
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
	h.beforeCases()
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
	if want := []string{timeCapReached + "\nlast: no session produced a result\n- " + change}; !slices.Equal(h.escalated, want) {
		t.Fatalf("escalations = %q, want %q", h.escalated, want)
	}
}

// A writer's change request on the merge card with no session
// left is the human's answer. It buys the next session, which gets the
// change request, and no cap card follows: at the last regular attempt
// and at the session cap an extra attempt, with no agent time left and
// attempts to spare the time alone (versionSessionLadder); a run from
// before the gate bought an attempt there too.
func TestChangeRequestWithNoSessionLeftIsTheAnswer(t *testing.T) {
	change := "reviewer sebastian requested changes: keep the old flag"
	for name, tc := range map[string]struct {
		pinned  bool
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
			usage:   TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1, ExtraTime: 5 * time.Minute},
		},
		"no agent time left, before the gate": {
			pinned:  true,
			state:   RunState{Status: "in-review", PR: PullRequest{Number: 9}, Head: "c3", Usage: TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1}},
			results: []SessionResult{pushed("s2", "c4")},
			want:    []string{"pr c3 after 0 cards", "approval c3", "session 2 after 0 cards", "pr c4 after 0 cards", "approval c4"},
			usage:   TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1, ExtraTime: 5 * time.Minute, ExtraAttempts: 1},
		},
	} {
		h := newWorkflowHarness(t)
		if tc.pinned {
			h.env.OnGetVersion("session-ladder", workflow.DefaultVersion, versionSessionLadder).Return(workflow.DefaultVersion)
		}
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

// timedSession is a RunSession answer that takes its agent time.
type timedSession struct {
	took time.Duration
	res  SessionResult
}

// onTimedSessions answers RunSession attempts in order, each after its
// time, recording each input; the last repeats.
func (h *workflowHarness) onTimedSessions(sessions ...timedSession) {
	for i, s := range sessions {
		call := h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(s.took).Return(
			func(ctx context.Context, in SessionInput) (SessionResult, error) {
				h.sessions = append(h.sessions, in)
				return s.res, nil
			})
		if i < len(sessions)-1 {
			call.Once()
		}
	}
}

// A change request that answers the agent-time cap buys an attempt only at
// the session cap (versionSessionLadder): the starved first session took
// no number and spent the 20 minutes, the request on its head c1 buys the
// 5 minute slice alone, and the next session runs as attempt 1. A run from
// before the gate counted the starved session and bought an attempt too:
// the next session ran as attempt 2.
func TestWorkflowChangeRequestAtTheTimeCapBuysTheTimeAlone(t *testing.T) {
	change := "reviewer sebastian requested changes: rename it"
	for name, tc := range map[string]struct {
		pinned   bool
		attempts []int
		usage    []TaskUsage
	}{
		"gated": {false, []int{1, 1}, []TaskUsage{
			{AgentTime: 20 * time.Minute, LastUncounted: true}, {AgentTime: 20 * time.Minute, ExtraTime: 5 * time.Minute, LastUncounted: true},
			{AgentTime: 21 * time.Minute, Attempts: 1, ExtraTime: 5 * time.Minute}}},
		"before the gate": {true, []int{1, 2}, []TaskUsage{
			{AgentTime: 20 * time.Minute, Attempts: 1}, {AgentTime: 20 * time.Minute, Attempts: 1, ExtraTime: 5 * time.Minute, ExtraAttempts: 1},
			{AgentTime: 21 * time.Minute, Attempts: 2, ExtraTime: 5 * time.Minute, ExtraAttempts: 1}}},
	} {
		h := newWorkflowHarness(t)
		if tc.pinned {
			h.env.OnGetVersion("session-ladder", workflow.DefaultVersion, versionSessionLadder).Return(workflow.DefaultVersion)
		}
		h.branchHead = "c1"
		var steps []string
		h.onVerify(&steps, nil)
		h.onTimedSessions(timedSession{20 * time.Minute, wallClock("starved", "c1")}, timedSession{time.Minute, pushed("s2", "c2")})
		h.env.OnActivity(ActivitySessionAnswered, mock.Anything, mock.Anything).Return(
			func(ctx context.Context, in SessionAnswerInput) (bool, error) { return in.SessionID != "starved", nil })
		h.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", Body: "rename it", PR: 9})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

		if final := h.run(t); final != "done" {
			t.Fatalf("%s: final = %q, finals = %v", name, final, h.finals)
		}
		var attempts []int
		for _, in := range h.sessions {
			attempts = append(attempts, in.Attempt)
		}
		if !slices.Equal(attempts, tc.attempts) || len(h.escalated) != 0 || !slices.Equal(h.sessions[1].Findings, []string{change}) {
			t.Fatalf("%s: attempts = %v, escalations %q, second findings %q; want %v, no card and the change request",
				name, attempts, h.escalated, h.sessions[1].Findings, tc.attempts)
		}
		if !slices.Equal(h.usage, tc.usage) {
			t.Fatalf("%s: usage = %+v, want %+v", name, h.usage, tc.usage)
		}
	}
}

// The agent-time cap card's answer buys an attempt only at the session cap
// (versionSessionLadder), whatever the last session counted: a restarted
// run finds attempt 1 and the 20 minutes spent, and its cap card's answer
// buys the 5 minute slice alone, and the next session runs as attempt 2. Attempt
// 3 spending the last 5 minutes is at the cap: the answer buys attempt 4
// with the slice. A run from before the gate bought an attempt with every
// answer.
func TestWorkflowTimeCapAnswerBuysAnAttemptOnlyAtTheSessionCap(t *testing.T) {
	for name, tc := range map[string]struct {
		pinned   bool
		used     TaskUsage
		sessions []timedSession
		attempts []int
		granted  TaskUsage // the usage the answer records
		head     string    // the cap the card names
	}{
		"restarted": {
			head:     timeCapReached,
			used:     TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1},
			sessions: []timedSession{{time.Minute, pushed("s2", "c2")}},
			attempts: []int{2},
			granted:  TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1, ExtraTime: 5 * time.Minute},
		},
		"restarted, before the gate": {
			head:     timeCapReached,
			pinned:   true,
			used:     TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1},
			sessions: []timedSession{{time.Minute, pushed("s2", "c2")}},
			attempts: []int{2},
			granted:  TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1, ExtraTime: 5 * time.Minute, ExtraAttempts: 1},
		},
		"at the session cap": {
			head:     capReached,
			used:     TaskUsage{AgentTime: 15 * time.Minute, Attempts: 2},
			sessions: []timedSession{{5 * time.Minute, wallClock("s3", "c1")}, {time.Minute, pushed("s4", "c2")}},
			attempts: []int{3, 4},
			granted:  TaskUsage{AgentTime: 20 * time.Minute, Attempts: 3, ExtraTime: 5 * time.Minute, ExtraAttempts: 1},
		},
	} {
		h := newWorkflowHarness(t)
		h.beforeCases()
		if tc.pinned {
			h.env.OnGetVersion("session-ladder", workflow.DefaultVersion, versionSessionLadder).Return(workflow.DefaultVersion)
		}
		h.branchHead = "c1"
		h.state = RunState{Status: "running", Usage: tc.used}
		var steps []string
		h.onVerify(&steps, nil, gosecRed, ChecksResult{State: ChecksSuccess})
		h.onTimedSessions(tc.sessions...)
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

		if final := h.run(t); final != "done" {
			t.Fatalf("%s: final = %q, finals = %v", name, final, h.finals)
		}
		var attempts []int
		for _, in := range h.sessions {
			attempts = append(attempts, in.Attempt)
		}
		if !slices.Equal(attempts, tc.attempts) || len(h.escalated) != 1 || !strings.HasPrefix(h.escalated[0], tc.head) {
			t.Fatalf("%s: attempts = %v, escalations %q; want %v after the one cap card", name, attempts, h.escalated, tc.attempts)
		}
		if i := slices.Index(h.usage, tc.granted); i < 0 || i != len(h.usage)-2 {
			t.Fatalf("%s: usage = %+v, want %+v recorded by the answer, before the last session's", name, h.usage, tc.granted)
		}
	}
}

// A run restarted while the cap question waits takes its answer by the
// same rule (versionSessionLadder): attempt 1 and a session that took no
// number spent the 20 minutes, so the answer buys the 5 minute slice alone
// and the next session runs as attempt 2; at the session cap it buys
// attempt 4 with the slice. A run from before the gate bought an attempt
// with the answer.
func TestWorkflowRestartedCapQuestionBuysAnAttemptOnlyAtTheSessionCap(t *testing.T) {
	for name, tc := range map[string]struct {
		pinned  bool
		used    TaskUsage
		attempt int
		granted TaskUsage
	}{
		"time cap": {used: TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1}, attempt: 2,
			granted: TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1, ExtraTime: 5 * time.Minute}},
		"session cap": {used: TaskUsage{AgentTime: 20 * time.Minute, Attempts: 3}, attempt: 4,
			granted: TaskUsage{AgentTime: 20 * time.Minute, Attempts: 3, ExtraTime: 5 * time.Minute, ExtraAttempts: 1}},
		"time cap, before the gate": {pinned: true, used: TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1}, attempt: 2,
			granted: TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1, ExtraTime: 5 * time.Minute, ExtraAttempts: 1}},
	} {
		h := newWorkflowHarness(t)
		h.beforeCases()
		if tc.pinned {
			h.env.OnGetVersion("session-ladder", workflow.DefaultVersion, versionSessionLadder).Return(workflow.DefaultVersion)
		}
		h.state = RunState{Status: "waiting-human", Question: capReached, Usage: tc.used}
		h.onTimedSessions(timedSession{time.Minute, pushed("s2", "c2")})
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

		if final := h.run(t); final != "done" {
			t.Fatalf("%s: final = %q, finals = %v", name, final, h.finals)
		}
		if len(h.sessions) != 1 || h.sessions[0].Attempt != tc.attempt {
			t.Fatalf("%s: sessions = %+v, want one, attempt %d", name, h.sessions, tc.attempt)
		}
		if len(h.usage) != 2 || h.usage[0] != tc.granted {
			t.Fatalf("%s: usage = %+v, want %+v recorded by the answer, then the session's", name, h.usage, tc.granted)
		}
	}
}

// A run restarted while a question that is not the cap's waits takes its
// answer by the rule of the loop's (versionSessionLadder): the row says
// whether the last session took a number. After one that took none the
// answer buys the 5 minute slice alone, after one that took one it buys an
// attempt with it, and an answer to a wait the live loop answers with the
// next session alone buys nothing: a floor's refusal (the budget's, or a
// model it cannot price), pods that never started, the provider's refusals.
// The session after it is attempt 2, a minute long. A run from before the
// gate bought an attempt with any answer.
func TestWorkflowRestartedQuestionBuysByTheLoopsRule(t *testing.T) {
	const pod = "the pod could not start: image pull refused"
	floor := budgetRefused + ": 0.080 EUR of the 2.000 EUR budget remains, and a session on factory/small needs 0.300000 EUR"
	type row struct {
		pinned   bool
		question string
		used     TaskUsage
		usage    []TaskUsage
	}
	rows := map[string]row{
		"uncounted": {question: pod, used: TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1, LastUncounted: true}, usage: []TaskUsage{
			{AgentTime: 20 * time.Minute, Attempts: 1, ExtraTime: 5 * time.Minute, LastUncounted: true},
			{AgentTime: 21 * time.Minute, Attempts: 2, ExtraTime: 5 * time.Minute}}},
		"counted": {question: pod, used: TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1}, usage: []TaskUsage{
			{AgentTime: 20 * time.Minute, Attempts: 1, ExtraTime: 5 * time.Minute, ExtraAttempts: 1},
			{AgentTime: 21 * time.Minute, Attempts: 2, ExtraTime: 5 * time.Minute, ExtraAttempts: 1}}},
		"uncounted, before the gate": {pinned: true, question: pod, used: TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1, LastUncounted: true}, usage: []TaskUsage{
			{AgentTime: 20 * time.Minute, Attempts: 1, ExtraTime: 5 * time.Minute, ExtraAttempts: 1, LastUncounted: true},
			{AgentTime: 21 * time.Minute, Attempts: 2, ExtraTime: 5 * time.Minute, ExtraAttempts: 1}}},
	}
	five := TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}
	for kind, question := range map[string]string{
		"the budget's floor":        floor,
		"an unpriced model's floor": "the attempt's floor on small cannot be read: LiteLLM has no model",
		"pods that never started": "No node started the session's pod: 3 sessions in a row waited for it, and no agent ran. Free what it waits on (a node's cpu, " +
			"memory or ephemeral storage, or the registry of its image), then answer to start the next session. The last wait: Pending",
		"the provider's refusals": "the model provider refused the first call of 6 sessions in a row (status 429): rate limited",
	} {
		rows[kind] = row{question: question, used: five, usage: []TaskUsage{{AgentTime: 6 * time.Minute, Attempts: 2}}}
		rows[kind+", before the gate"] = row{pinned: true, question: question, used: five, usage: []TaskUsage{
			{AgentTime: 5 * time.Minute, Attempts: 1, ExtraAttempts: 1},
			{AgentTime: 6 * time.Minute, Attempts: 2, ExtraAttempts: 1}}}
	}
	for name, tc := range rows {
		h := newWorkflowHarness(t)
		h.beforeCases()
		if tc.pinned {
			h.env.OnGetVersion("session-ladder", workflow.DefaultVersion, versionSessionLadder).Return(workflow.DefaultVersion)
		}
		h.state = RunState{Status: "waiting-human", Question: tc.question, Usage: tc.used}
		h.onTimedSessions(timedSession{time.Minute, pushed("s2", "c2")})
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the registry is back", Comment: 602})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

		if final := h.run(t); final != "done" {
			t.Fatalf("%s: final = %q, finals = %v", name, final, h.finals)
		}
		if len(h.sessions) != 1 || h.sessions[0].Attempt != 2 {
			t.Fatalf("%s: sessions = %+v, want one, attempt 2", name, h.sessions)
		}
		if !slices.Equal(h.usage, tc.usage) {
			t.Errorf("%s: usage = %+v, want %+v", name, h.usage, tc.usage)
		}
	}
}

// The answer to the check-in's spend stop buys the stretch its own
// check-in moments (issue 959): beside the attempt, the answer grants the
// card one more share of the check-in budget — TaskUsage.ExtraCheckinShare,
// which widens the share the runner counts — so a long card that spent
// its share stops once per human answer, not once per check-in moment,
// and the card's own budget stays the cap. Without the grant the next
// moment stops the card again before any model call, and the human pays
// one answer per stretch.
// The share's grant is a run's from before versionCases.
func TestWorkflowCheckSpendAnswerGrantsNextCheckin(t *testing.T) {
	h := newWorkflowHarness(t)
	h.beforeCases()
	h.state = RunState{Status: "waiting-human",
		Question: "check-in: spend_above_policy: the check-in's share of the budget is spent",
		Usage:    TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}}
	h.onTimedSessions(timedSession{time.Minute, pushed("s2", "c2")})
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if len(h.sessions) != 1 || h.sessions[0].Attempt != 2 {
		t.Fatalf("sessions = %+v, want the next session to run on the answer, attempt 2: the check-in moment is not stopped again", h.sessions)
	}
	if len(h.escalated) != 1 {
		t.Fatalf("escalations = %q, want the one re-asked stop and no second spend stop", h.escalated)
	}
	if u := h.usage[len(h.usage)-1]; u.ExtraCheckinShare != 1 {
		t.Fatalf("usage = %+v, want the answer's grant of one more check-in share recorded", u)
	}
}

// The live loop's answer to the check-in's spend stop grants the share the
// same way (issue 959): the next session runs on the answer and its card
// stops there, not at every check-in moment to come. A run from before the
// change buys the attempt alone — the next moment would stop the card
// again.
// The share's grant is a run's from before versionCases.
func TestWorkflowCheckSpendAnswerInTheLoopGrantsTheShare(t *testing.T) {
	for name, pinned := range map[string]bool{"gated": false, "before the gate": true} {
		h := newWorkflowHarness(t)
		h.beforeCases()
		if pinned {
			h.env.OnGetVersion("checkin-grant", workflow.DefaultVersion, versionCheckinGrant).Return(workflow.DefaultVersion)
		}
		h.taskConfig = askTaskConfig()
		h.onTimedSessions(
			timedSession{time.Minute, checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman,
				ReasonKind: CheckSpend, Reason: CheckinShareSpent,
				Record: &CheckInRecord{Moment: CheckInGate, Session: "s2", ParentSession: "s1"}})},
			timedSession{time.Minute, pushed("s3", "c3")})
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

		if final := h.run(t); final != "done" {
			t.Fatalf("%s: final = %q, escalations = %q", name, final, h.escalated)
		}
		if len(h.sessions) != 2 || h.sessions[1].Attempt != 2 {
			t.Fatalf("%s: sessions = %+v, want the answered session to run, attempt 2", name, h.sessions)
		}
		if !slices.Equal(h.escalated, []string{checkinShareStop}) {
			t.Fatalf("%s: escalations = %q, want the one spend stop", name, h.escalated)
		}
		want := 1
		if pinned {
			want = 0
		}
		if u := h.usage[len(h.usage)-1]; u.ExtraCheckinShare != want {
			t.Fatalf("%s: usage = %+v, want ExtraCheckinShare %d", name, u, want)
		}
	}
}

// A run restarted while a gate's escalation waits takes the answer as the
// live gate does, by verifying again, here from the first stage
// (versionSessionLadder): the head c2 goes through the review again and the
// approval is asked again, no session starts and no attempt is bought. A
// run from before the gate bought an attempt and started a session, whose
// push c3 went through verification instead, and so does a gated run whose
// Reconcile found no pull request: there is no head to verify.
func TestWorkflowRestartedGateEscalationVerifiesTheHead(t *testing.T) {
	questions := map[string]string{
		"the gate supervisor's": "The gate supervisor stopped ModelReview (attempt 1) past its bound of 10m0s: hangs",
		"the merge queue's": "the checks are still red after the merge queue's turns: main moved on 1 of 3 turns, and the red checks ran again on 2, " +
			"so the pull request left the queue until a human answers",
		"the reproduction's start": "the sandbox cannot start at image pull, and no session can fix it: push the worker image or correct its reference, " +
			"then answer to run the reproduction again, or close the card.",
	}
	for kind, question := range questions {
		for _, variant := range []string{"", ", before the gate", ", no pull request"} {
			name := kind + variant
			h := newWorkflowHarness(t)
			h.beforeCases()
			if variant == ", before the gate" {
				h.env.OnGetVersion("session-ladder", workflow.DefaultVersion, versionSessionLadder).Return(workflow.DefaultVersion)
			}
			h.state = RunState{Status: "waiting-human", Question: question, PR: PullRequest{Number: 9}, Head: "c2",
				Usage: TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}}
			if variant == ", no pull request" {
				h.state.PR, h.state.Head = PullRequest{}, ""
			}
			var steps []string
			h.onVerify(&steps, []SessionResult{pushed("s2", "c3")})
			reviewed := []string{}
			h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
				func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
					reviewed = append(reviewed, commit)
					return Review{}, nil
				})
			h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the gateway is back", Comment: 602})
			h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

			if final := h.run(t); final != "done" {
				t.Fatalf("%s: final = %q, finals = %v", name, final, h.finals)
			}
			want := struct {
				sessions int
				reviewed []string
				steps    []string
				usage    []TaskUsage
			}{0, []string{"c2"}, []string{"pr c2 after 1 cards", "approval c2"}, nil}
			if variant != "" {
				want.sessions, want.reviewed, want.steps = 1, []string{"c3"}, []string{"session 2 after 1 cards", "pr c3 after 1 cards", "approval c3"}
				want.usage = []TaskUsage{{AgentTime: 5 * time.Minute, Attempts: 1, ExtraAttempts: 1}, {AgentTime: 5 * time.Minute, Attempts: 2, ExtraAttempts: 1}}
			}
			if len(h.sessions) != want.sessions || !slices.Equal(reviewed, want.reviewed) || !slices.Equal(steps, want.steps) || !slices.Equal(h.usage, want.usage) {
				t.Errorf("%s: sessions %d, reviewed %q, steps %q, usage %+v; want %d, %q, %q, %+v",
					name, len(h.sessions), reviewed, steps, h.usage, want.sessions, want.reviewed, want.steps, want.usage)
			}
		}
	}
}

// A run restarted on a check-in stopped at its spent budget takes the
// answer as the gate's stop does, by verifying the head again: no session
// starts, for a bought one would stop at the same cap before any call.
func TestCheckinSpendResumeReVerifiesOpenPR(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "waiting-human", Question: "check-in: spend_above_policy: the check-in's share of the budget is spent",
		PR: PullRequest{Number: 9}, Head: "c2", Usage: TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}}
	var steps []string
	h.onVerify(&steps, []SessionResult{pushed("s2", "c3")})
	reviewed := []string{}
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			reviewed = append(reviewed, commit)
			return Review{}, nil
		})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the cap is spent; review the head", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if want := []string{"pr c2 after 1 cards", "approval c2"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want %v", steps, want)
	}
	if len(h.sessions) != 0 || !slices.Equal(reviewed, []string{"c2"}) {
		t.Fatalf("sessions = %d, reviewed = %q; want no session and the head c2 alone reviewed", len(h.sessions), reviewed)
	}
}

// A run restarted on a check-in whose pull request head moved after its
// checks verdict was recorded holds the moved head, as the gate path
// does: the answer's verify adopts the branch's head, waits its checks
// and asks the approval there — a run from before the head-holds change
// meets the gate's refusal whole, for its gates hold no moved head
// (2026-10-31: t-6eadec8912ab answered, PR #1385's head had moved, the
// task died with its fix unmerged). A run from before the change keeps
// that end.
func TestCheckinSpendResumeHoldsAMovedHead(t *testing.T) {
	for _, variant := range []string{"", ", before the change"} {
		h := newWorkflowHarness(t)
		if variant != "" {
			h.env.OnGetVersion("resume-hold", workflow.DefaultVersion, versionResumeHold).Return(workflow.DefaultVersion)
		}
		h.env.OnGetVersion("head-holds", workflow.DefaultVersion, versionHeadHolds).Return(workflow.DefaultVersion)
		h.state = RunState{Status: "waiting-human", Question: "check-in: spend_above_policy: the check-in's share of the budget is spent",
			PR: PullRequest{Number: 9}, Head: "c1", Usage: TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}}
		h.branchHead = "c2"
		h.env.OnActivity(ActivityGatePolicy, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(
			func(ctx context.Context, taskID string, pr int64, commit string, pin int64) (PolicyGate, error) {
				if commit == "c1" {
					return PolicyGate{}, headMovedError("pull request #9's head moved to c2 after its checks passed on c1")
				}
				return PolicyGate{}, nil
			})
		var steps []string
		h.onVerify(&steps, []SessionResult{pushed("s2", "c3")})
		reviewed := []string{}
		h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
			func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
				reviewed = append(reviewed, commit)
				return Review{}, nil
			})
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the cap is spent; review the head", Comment: 602})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

		if variant == "" {
			if final := h.run(t); final != "done" {
				t.Fatalf("final = %q, finals = %v", final, h.finals)
			}
			if want := []string{"pr c1 after 1 cards", "pr c2 after 1 cards", "approval c2"}; !slices.Equal(steps, want) {
				t.Fatalf("steps = %v, want %v", steps, want)
			}
			if len(h.sessions) != 0 || !slices.Equal(reviewed, []string{"c2"}) {
				t.Fatalf("sessions = %d, reviewed = %q; want no session and the moved head c2 alone reviewed", len(h.sessions), reviewed)
			}
		} else if final := h.run(t); final != "failed" || !strings.Contains(h.finals[0][1], "head moved to c2") {
			t.Fatalf("final = %q, finals = %v; want the head-moved refusal's end", final, h.finals)
		}
	}
}

// A run restarted on a gate escalation's wait verifies the head again
// from the first stage, and a head the agent moved past the gates is
// held there too: adopted from the branch, its checks waited on, the
// approval asked on it. A run from before the change dies on the gate's
// refusal.
func TestGateEscalationResumeHoldsAMovedHead(t *testing.T) {
	for _, variant := range []string{"", ", before the change"} {
		h := newWorkflowHarness(t)
		if variant != "" {
			h.env.OnGetVersion("resume-hold", workflow.DefaultVersion, versionResumeHold).Return(workflow.DefaultVersion)
		}
		h.env.OnGetVersion("head-holds", workflow.DefaultVersion, versionHeadHolds).Return(workflow.DefaultVersion)
		h.state = RunState{Status: "waiting-human",
			Question: "The gate supervisor stopped ModelReview (attempt 1) past its bound of 10m0s: hangs",
			PR:       PullRequest{Number: 9}, Head: "c1", Usage: TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}}
		h.branchHead = "c2"
		h.env.OnActivity(ActivityGatePolicy, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(
			func(ctx context.Context, taskID string, pr int64, commit string, pin int64) (PolicyGate, error) {
				if commit == "c1" {
					return PolicyGate{}, headMovedError("pull request #9's head moved to c2 after its checks passed on c1")
				}
				return PolicyGate{}, nil
			})
		var steps []string
		h.onVerify(&steps, []SessionResult{pushed("s2", "c3")})
		reviewed := []string{}
		h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
			func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
				reviewed = append(reviewed, commit)
				return Review{}, nil
			})
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the gateway is back", Comment: 602})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

		if variant == "" {
			if final := h.run(t); final != "done" {
				t.Fatalf("final = %q, finals = %v", final, h.finals)
			}
			if want := []string{"pr c1 after 1 cards", "pr c2 after 1 cards", "approval c2"}; !slices.Equal(steps, want) {
				t.Fatalf("steps = %v, want %v", steps, want)
			}
			if len(h.sessions) != 0 || !slices.Equal(reviewed, []string{"c2"}) {
				t.Fatalf("sessions = %d, reviewed = %q; want no session and the moved head c2 alone reviewed", len(h.sessions), reviewed)
			}
		} else if final := h.run(t); final != "failed" || !strings.Contains(h.finals[0][1], "head moved to c2") {
			t.Fatalf("final = %q, finals = %v; want the head-moved refusal's end", final, h.finals)
		}
	}
}

// An answer to a check-in stopped at its call cap buys the session, and a
// card with no pull request leaves the loop.
func TestCheckinCallCapAnswerBuysTheSession(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(checkinStopped("s1", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman,
		ReasonKind: CheckSpend, Reason: "check-in call cap reached",
		Record: &CheckInRecord{Moment: CheckInGate, Session: "s1"}}), pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "one more check-in round", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if len(h.sessions) != 2 {
		t.Fatalf("sessions = %d, want the stopped one and the one the answer bought", len(h.sessions))
	}
}

// A run restarted on a check-in stopped at its call cap with no pull
// request does the same: the answer buys the session, and it runs.
func TestCheckinCallCapResumeBuysTheSession(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "waiting-human", Question: "check-in: spend_above_policy: check-in call cap reached",
		Usage: TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}}
	var steps []string
	h.onVerify(&steps, []SessionResult{pushed("s2", "c3")})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "one more check-in round", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if want := []string{"session 2 after 1 cards", "pr c3 after 1 cards", "approval c3"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want %v", steps, want)
	}
}

// A run restarted on a check-in stopped at its call cap with the pull
// request open buys the session too, never the spend case's verify of the
// open head, and that session runs.
func TestCheckinCallCapResumeWithOpenPRBuysTheSession(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "waiting-human", Question: "check-in: spend_above_policy: check-in call cap reached",
		PR: PullRequest{Number: 9}, Head: "c2", Usage: TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}}
	var steps []string
	h.onVerify(&steps, []SessionResult{pushed("s2", "c3")})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "one more check-in round", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if want := []string{"session 2 after 1 cards", "pr c3 after 1 cards", "approval c3"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want %v: the answer buys the session, not a verify of c2", steps, want)
	}
}

// A check-in stopped at its call cap with the pull request open
// (versionCheckinRaise): the answer buys the session, never the spend
// case's verify of the head, and that session runs.
func TestCheckinCallCapLiveWithOpenPRBuysTheSession(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	var steps []string
	h.onVerify(&steps, []SessionResult{
		pushed("s1", "c1"),
		checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
			Reason: "check-in call cap reached",
			Record: &CheckInRecord{Moment: CheckInGate, Session: "s2", ParentSession: "s1"}}),
		pushed("s3", "c2")})
	reviews := 0
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			if reviews++; reviews == 1 {
				return Review{Verdict: "concerns", Summary: "one fault", Findings: []string{"rename the flag"}}, nil
			}
			return Review{}, nil
		})
	h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "one more check-in round", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if len(h.sessions) != 3 {
		t.Fatalf("sessions = %d, want three: the work, the stopped check-in's own and the one the answer bought", len(h.sessions))
	}
	if want := []string{"session 1 after 0 cards", "pr c1 after 0 cards", "session 2 after 0 cards",
		"session 3 after 1 cards", "pr c2 after 1 cards", "approval c2"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want %v: the answer buys the session, no verify of c1", steps, want)
	}
}

// A change request answering a restarted gate's escalation goes back to the
// agent, as the live gate's does: no verification of c2, the next session
// is attempt 2 with the request, and nothing is bought below the cap.
func TestWorkflowRestartedGateEscalationTakesAChangeRequestToTheAgent(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "waiting-human", Question: "The gate supervisor stopped ModelReview (attempt 1) past its bound of 10m0s: hangs",
		PR: PullRequest{Number: 9}, Head: "c2", Usage: TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}}
	var steps []string
	h.onVerify(&steps, []SessionResult{pushed("s2", "c3")})
	h.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", Body: "rename it", PR: 9})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if want := []string{"session 2 after 1 cards", "pr c3 after 1 cards", "approval c3"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %q, want %q", steps, want)
	}
	if f := h.sessions[0].Findings; len(f) != 1 || !strings.HasSuffix(f[0], "requested changes: rename it") {
		t.Fatalf("findings = %q, want the change request", f)
	}
	if want := []TaskUsage{{AgentTime: 5 * time.Minute, Attempts: 2}}; !slices.Equal(h.usage, want) {
		t.Fatalf("usage = %+v, want %+v", h.usage, want)
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
	if h.sessions[1].Model != "factory/big" || h.sessions[1].StartRef != "" || !slices.Equal(h.sessions[1].Findings, []string{"changed a.go"}) {
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

// Histories recorded live on the cluster replay without a nondeterminism
// error, and the newest input each activity recorded across them decodes
// into today's activity of that name: a roll retries an activity in
// flight with the input the old worker recorded. Older inputs are history
// the rolls since have passed and keep only their replay.
func TestWorkflowReplaysLiveHistories(t *testing.T) {
	files, _ := filepath.Glob("testdata/history-*.json")
	if len(files) == 0 {
		t.Fatal("no recorded histories")
	}
	newest := map[string]recordedInput{}
	for _, f := range files {
		replayHistory(t, f)
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		hist, err := client.HistoryFromJSON(bytes.NewReader(data), client.HistoryJSONOptions{})
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		newestInputs(newest, f, hist)
	}
	for _, bad := range undecodableInputs(newest, activityParams(), retyped) {
		t.Error(bad)
	}
}

// Histories a dev server recorded under stub activities replay too: a run
// that consumed the chain-done marker and waits in review takes today's
// end. Their stub inputs are no live input, so no decode reads them.
func TestWorkflowReplaysDevServerHistories(t *testing.T) {
	files, _ := filepath.Glob("testdata/devserver-*.json")
	if len(files) == 0 {
		t.Fatal("no dev server histories")
	}
	for _, f := range files {
		replayHistory(t, f)
	}
}

// replayHistory replays the history in file f against TaskWorkflow.
func replayHistory(t *testing.T, f string) {
	t.Helper()
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflowWithOptions(TaskWorkflow{Config: func() WorkflowConfig { return testWorkflowConfig }}.Run, workflow.RegisterOptions{Name: "TaskWorkflow"})
	if err := replayer.ReplayWorkflowHistoryFromJSONFile(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), f); err != nil {
		t.Fatalf("%s: %v", f, err)
	}
}

// retyped lists each activity whose parameters a commit changed on
// purpose, so that an input recorded before it does not decode. Such a
// commit rolls only while no task runs the activity, and the activity's
// decode waits for a history recorded after it. Once a task has run the
// activity after the roll, add its history and remove the entry:
//
//	kubectl exec -n platform deploy/temporal-admintools -- temporal workflow show --workflow-id task-<task> --output json > internal/factoryd/testdata/history-<task>-<what>.json
var retyped []retype

// retype is an activity whose parameters the commit, made at, changed.
type retype struct {
	activity, commit string
	at               time.Time
}

// The newest GateAudit input decides: one recorded before audit-beside
// passed the pin where GateAudit now takes the GateResults. Undeclared it
// fails; declared it waits for a history after the declaration, and one
// still with the pin fails again.
func TestUndecodableInputsDecodeTheNewestInput(t *testing.T) {
	audit := func(id int64, at time.Time, last any) *historypb.History {
		input, err := converter.GetDefaultDataConverter().ToPayloads("t1", PullRequest{Number: 9, URL: "https://git.example/sf/repo/pulls/9"}, "c1", last)
		if err != nil {
			t.Fatal(err)
		}
		return &historypb.History{Events: []*historypb.HistoryEvent{{EventId: id, EventTime: timestamppb.New(at), EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED,
			Attributes: &historypb.HistoryEvent_ActivityTaskScheduledEventAttributes{ActivityTaskScheduledEventAttributes: &historypb.ActivityTaskScheduledEventAttributes{
				ActivityType: &commonpb.ActivityType{Name: ActivityGateAudit}, Input: input}}}}}
	}
	day := time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC)
	pinFails := []string{"pin.json event 5: GateAudit argument 4 does not decode into factoryd.GateResults: unable to decode: json: cannot unmarshal number into Go value of type factoryd.GateResults"}
	for _, tc := range []struct {
		name           string
		pinAt, gatesAt time.Time
		retyped        []retype
		want           []string
	}{
		{"undeclared", day, day.Add(-time.Hour), nil, pinFails},
		{"declared", day, day.Add(-time.Hour), []retype{{ActivityGateAudit, "5c250e7", day.Add(time.Hour)}}, nil},
		{"declared before a newer pin", day, day.Add(-time.Hour), []retype{{ActivityGateAudit, "5c250e7", day.Add(-time.Minute)}}, pinFails},
		{"refreshed", day, day.Add(time.Hour), nil, nil},
	} {
		newest := map[string]recordedInput{}
		newestInputs(newest, "pin.json", audit(5, tc.pinAt, int64(656)))
		newestInputs(newest, "gates.json", audit(7, tc.gatesAt, GateResults{Pin: 656}))
		if got := undecodableInputs(newest, activityParams(), tc.retyped); !slices.Equal(got, tc.want) {
			t.Errorf("%s: undecodable = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// activityParams maps each activity RegisterActivities registers to the
// types of its arguments, the parameters after the context.
func activityParams() map[string][]reflect.Type {
	reg := activityRegistry{}
	(&Runner{}).RegisterActivities(reg)
	params := map[string][]reflect.Type{}
	for name, fn := range reg {
		for i := 1; i < fn.NumIn(); i++ {
			params[name] = append(params[name], fn.In(i))
		}
	}
	return params
}

type activityRegistry map[string]reflect.Type

func (r activityRegistry) RegisterActivityWithOptions(fn any, o activity.RegisterOptions) {
	r[o.Name] = reflect.TypeOf(fn)
}

// recordedInput is the input a history's ActivityTaskScheduled event
// recorded.
type recordedInput struct {
	file     string
	event    int64
	at       time.Time
	payloads []*commonpb.Payload
}

// newestInputs keeps in newest, for each activity hist schedules, the
// input recorded last.
func newestInputs(newest map[string]recordedInput, file string, hist *historypb.History) {
	for _, e := range hist.GetEvents() {
		a := e.GetActivityTaskScheduledEventAttributes()
		if a == nil {
			continue
		}
		name := a.GetActivityType().GetName()
		if at := e.GetEventTime().AsTime(); at.After(newest[name].at) {
			newest[name] = recordedInput{file, e.GetEventId(), at, a.GetInput().GetPayloads()}
		}
	}
}

// undecodableInputs names each newest input the worker would fail to
// decode on a retry: payload i into argument i through the SDK's data
// converter, extra payloads ignored as the SDK ignores them. An activity
// in retypes is skipped while its newest input is older than the retype.
func undecodableInputs(newest map[string]recordedInput, params map[string][]reflect.Type, retypes []retype) []string {
	dc := converter.GetDefaultDataConverter()
	var bad []string
	for name, in := range newest {
		if slices.ContainsFunc(retypes, func(r retype) bool { return r.activity == name && in.at.Before(r.at) }) {
			continue
		}
		args, ok := params[name]
		if !ok {
			bad = append(bad, fmt.Sprintf("%s event %d: no activity %s is registered", in.file, in.event, name))
			continue
		}
		for i, p := range in.payloads[:min(len(in.payloads), len(args))] {
			if err := dc.FromPayload(p, reflect.New(args[i]).Interface()); err != nil {
				bad = append(bad, fmt.Sprintf("%s event %d: %s argument %d does not decode into %s: %v", in.file, in.event, name, i+1, args[i], err))
			}
		}
	}
	slices.Sort(bad)
	return bad
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

// A provider's refusal at a session's first call is no attempt:
// the task waits on the provider 2 minutes, then 4, in gate provider
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

	if final := h.run(t); final != "closed" || len(h.escalated) != 1 || !strings.HasPrefix(h.escalated[0], timeCapReached) {
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

// capRun is what a capHarness saw: when each session's result came
// (began); when CapResume checked before an ask (asks), in a wait before
// the period's turn (checks) and at it (turns), with each check's input
// (inputs); and when a run from before versionCapWait checked through
// DayResume (dayChecks, dayTurns). A check finds the cap no longer binding
// from funded on (zero: never), or as verdict says when set; each takes
// takes.
type capRun struct {
	began, asks, checks, turns, dayChecks, dayTurns []time.Time
	inputs                                          []CapCheck
	funded                                          time.Time
	verdict                                         func(now time.Time) CapResumed
	takes                                           time.Duration
}

// capHarness starts at 2026-10-14 20:43 UTC, card #784's session, and
// answers RunSession with results in order, each session taking took.
func capHarness(t *testing.T, took time.Duration, results ...SessionResult) (*workflowHarness, *capRun) {
	h, run := newWorkflowHarness(t), &capRun{}
	h.env.SetStartTime(oct(14, 20, 43))
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions, run.began = append(h.sessions, in), append(run.began, h.env.Now())
			return results[min(len(h.sessions), len(results))-1], nil
		}).After(took)
	run.onChecks(h)
	return h, run
}

// onChecks answers h's CapResume and DayResume checks by the run's
// script and records them; a turn's check ends the wait.
func (run *capRun) onChecks(h *workflowHarness) {
	verdict := func(now time.Time) CapResumed {
		if run.verdict != nil {
			return run.verdict(now)
		}
		return CapResumed{Resume: !run.funded.IsZero() && !now.Before(run.funded)}
	}
	h.env.OnActivity(ActivityCapResume, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in CapCheck) (CapResumed, error) {
			now := h.env.Now()
			run.inputs = append(run.inputs, in)
			switch {
			case in.Turned:
				run.turns = append(run.turns, now)
				return CapResumed{Resume: true}, nil
			case !in.RefusedAt.IsZero():
				run.asks = append(run.asks, now)
			default:
				run.checks = append(run.checks, now)
			}
			return verdict(now), nil
		}).AfterFn(func() time.Duration { return run.takes })
	h.env.OnActivity(ActivityDayResume, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, model string, turned bool) (bool, error) {
			if turned {
				run.dayTurns = append(run.dayTurns, h.env.Now())
				return true, nil
			}
			run.dayChecks = append(run.dayChecks, h.env.Now())
			return verdict(h.env.Now()).Resume, nil
		})
}

// oct is 2026-10-day at h:m UTC.
func oct(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, time.UTC) }

// oct31 is 2026-10-31 at h:m:s UTC, the night of cards #1352 and #1353.
func oct31(h, m, s int) time.Time { return time.Date(2026, 10, 31, h, m, s, 0, time.UTC) }

// novFirst is 2026-11-01 00:00 UTC, the turn of October's month.
var novFirst = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// sameTimes says a and b hold the same instants.
func sameTimes(a, b []time.Time) bool { return slices.EqualFunc(a, b, time.Time.Equal) }

// every is the times from first to last, step apart.
func every(first, last time.Time, step time.Duration) []time.Time {
	var out []time.Time
	for t := first; !t.After(last); t = t.Add(step) {
		out = append(out, t)
	}
	return out
}

// isCheck says in is a check of task t1 on model at cap decision, before
// an ask about the refusal at refusedAt (zero: in a wait).
func isCheck(in CapCheck, model, decision string, refusedAt time.Time) bool {
	return in.TaskID == "t1" && in.Model == model && in.Decision == decision && !in.Turned && in.RefusedAt.Equal(refusedAt)
}

var (
	dayRefusal = CapRefusal{Refusal: policy.Refusal{Decision: policy.RefusedDayCap, Reason: "free headroom -23.439417 EUR (task 2.000000, day -23.439417, month 54.546612)"},
		At: oct(14, 20, 43)}
	dayCapped = SessionResult{Outcome: OutcomeDayCap, SessionID: "s1", Capped: &dayRefusal,
		Findings: []string{"previous session ended without a pushed commit: the model call failed: refused_day_cap"}}
	dayQuestion = "the day cap is reached: the card resumes on its own once the day's free headroom " +
		"funds a session, at the latest when the UTC day turns at 2026-10-15T00:00:00Z. Raise budget.day_eur in policy.yaml to resume it sooner, or close the card."
	dayReason = dayQuestion + "\nthe gateway refused this task's spend (refused_day_cap): free headroom -23.439417 EUR (task 2.000000, day -23.439417, month 54.546612)"

	// monthRefusal is card #1352's: the gateway's own hold under policy
	// version 971 refused its first call at 23:22:52
	monthRefusal = CapRefusal{Refusal: policy.Refusal{Decision: policy.RefusedMonthCap, Reason: "free headroom 0.000001 EUR (task 2.000000, day 12.000000, month 0.000001)"},
		At: oct31(23, 22, 52)}
	// monthCapped is the session as RunSession returns it: an escalation
	// with the reason a run from before versionCapWait asks, and the
	// refusal
	monthCapped = SessionResult{Outcome: OutcomeEscalate, SessionID: "s1", Reason: capRefused(monthRefusal.Refusal), Capped: &monthRefusal,
		Findings: []string{"previous session ended without a pushed commit: the model call failed: refused_month_cap"}}
	monthQuestion = "the month cap is reached: the card resumes on its own once the month's free headroom " +
		"funds a session, at the latest when the UTC month turns at 2026-11-01T00:00:00Z. Raise budget.month_eur in policy.yaml to resume it sooner, or close the card."
	monthReason = monthQuestion + "\nthe gateway refused this task's spend (refused_month_cap): free headroom 0.000001 EUR (task 2.000000, day 12.000000, month 0.000001)"
)

// monthCappedAt is monthCapped refused at at, by a call the gateway
// allowed one of the session's before (allowed) or its first.
func monthCappedAt(at time.Time, allowed bool) SessionResult {
	refused := monthRefusal
	refused.At, refused.Allowed = at, allowed
	res := monthCapped
	res.Capped = &refused
	return res
}

// A session the day cap refused at its first call (card #784, roadmap 240)
// spends no further session that day and is no attempt: the check before
// the ask, at the refusal's 20:43, finds the day spent, so the card waits
// on a human with the refusal, checks the day's headroom every five
// minutes, and at 00:00 UTC runs the session again with the same input.
func TestWorkflowDayCapWaitsForTheDayToTurn(t *testing.T) {
	h, run := capHarness(t, 0, dayCapped, pushed("s2", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if !slices.Equal(h.escalated, []string{dayReason}) || escalationSource(h.escalated[0]) != "day_cap" {
		t.Fatalf("escalations = %q, want the day cap's refusal once", h.escalated)
	}
	if !sameTimes(run.asks, []time.Time{oct(14, 20, 43)}) || !isCheck(run.inputs[0], "factory/small", policy.RefusedDayCap, oct(14, 20, 43)) {
		t.Fatalf("checks before the ask at %v, first input %+v; want one at 20:43 about the 20:43 refusal", run.asks, run.inputs[0])
	}
	// 197 minutes to the turn: 39 checks five minutes apart, the turn two
	// minutes after the last
	if !sameTimes(run.checks, every(oct(14, 20, 48), oct(14, 23, 58), dayCheck)) || !sameTimes(run.turns, []time.Time{oct(15, 0, 0)}) {
		t.Fatalf("checks %v, turns %v; want every five minutes from 20:48 to 23:58, the turn at 00:00", run.checks, run.turns)
	}
	if len(run.dayChecks) != 0 || len(run.dayTurns) != 0 {
		t.Fatalf("DayResume ran at %v and %v, want CapResume alone", run.dayChecks, run.dayTurns)
	}
	if len(run.began) != 2 || !run.began[1].Equal(oct(15, 0, 0)) {
		t.Fatalf("sessions began at %v, want the second at 00:00", run.began)
	}
	if in := h.sessions[1]; in.Attempt != 1 || in.ParentSessionID != "" || len(in.Findings) != 0 {
		t.Fatalf("second session = %+v, want attempt 1 with the first's input", in)
	}
	if len(h.usage) == 0 || slices.ContainsFunc(h.usage, func(u TaskUsage) bool { return u.Attempts != 1 }) {
		t.Fatalf("usage = %+v, want every record at attempt 1", h.usage)
	}
}

// A run from before versionCapWait checks the day cap's wait through
// DayResume every five minutes from 20:48, its turn at 00:00, with no
// check before the ask, and a policy load does not wake it.
func TestWorkflowDayCapBeforeCapWaitChecksThroughDayResume(t *testing.T) {
	h, run := capHarness(t, 0, dayCapped, pushed("s2", "c1"))
	h.env.OnGetVersion("cap-wait", workflow.DefaultVersion, versionCapWait).Return(workflow.DefaultVersion)
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalPolicyLoaded, TaskSignal{Author: "factoryd"})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || !slices.Equal(h.escalated, []string{dayReason}) {
		t.Fatalf("final %q, escalations %q; want done after the day cap's ask", final, h.escalated)
	}
	if !sameTimes(run.dayChecks, every(oct(14, 20, 48), oct(14, 23, 58), dayCheck)) || !sameTimes(run.dayTurns, []time.Time{oct(15, 0, 0)}) || len(run.inputs) != 0 {
		t.Fatalf("DayResume checks %v, turns %v, CapResume inputs %+v; want DayResume every five minutes from 20:48, the turn at 00:00, no CapResume",
			run.dayChecks, run.dayTurns, run.inputs)
	}
	if len(run.began) != 2 || !run.began[1].Equal(oct(15, 0, 0)) || h.sessions[1].Attempt != 1 {
		t.Fatalf("sessions began at %v (%+v), want attempt 1 again at 00:00", run.began, h.sessions)
	}
}

// An attempt the floor refuses at the day cap waits for the day as a
// session the day cap refused does, with no human and no check before the
// ask, the floor having just read the cap: the escalation is the day
// cap's, and at 00:00 UTC the floor funds the attempt, which starts as
// attempt 1.
func TestWorkflowDayCapAtTheGateWaitsForTheDay(t *testing.T) {
	h, run := capHarness(t, 0, pushed("s1", "c1"))
	const state = "0.010000 EUR of the day's 20.00 EUR (budget.day_eur) is free, and the cheapest call on factory/small needs 0.015360 EUR " +
		"(1024 output tokens at the hold machinery's minimum); an answer would start an attempt whose first call cannot run."
	h.env.OnActivity(ActivitySessionFunded, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, model string) (string, error) {
			if h.env.Now().Before(oct(15, 0, 0)) {
				return "the day cap cannot fund a call: Answer, and the attempt waits on its own for the UTC day's headroom, or raise budget.day_eur in policy.yaml by a commit.\n" + state, nil
			}
			return "", nil
		})
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if want := dayQuestion + "\nthe attempt's floor refused the attempt before it started (refused_day_cap): " + state; !slices.Equal(h.escalated, []string{want}) || escalationSource(h.escalated[0]) != "day_cap" {
		t.Fatalf("escalations = %q, want the day cap's wait once: %q", h.escalated, want)
	}
	if len(run.began) != 1 || !run.began[0].Equal(oct(15, 0, 0)) || h.sessions[0].Attempt != 1 || !sameTimes(run.turns, []time.Time{oct(15, 0, 0)}) || len(run.asks) != 0 {
		t.Fatalf("sessions began at %v (%+v), turns %v, checks before an ask %v; want attempt 1 at 00:00 and no check before the ask", run.began, h.sessions, run.turns, run.asks)
	}
}

// A check that finds the day's free headroom funding a session (holds
// settled, budget.day_eur raised) ends the wait before the turn; the check
// before the ask at 20:43 came before the 21:10 raise.
func TestWorkflowDayCapResumesWhenTheDayFundsASession(t *testing.T) {
	h, run := capHarness(t, 0, dayCapped, pushed("s2", "c1"))
	run.funded = oct(14, 21, 10)
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(run.began) != 2 || !run.began[1].Equal(oct(14, 21, 13)) || len(run.turns) != 0 ||
		!sameTimes(run.asks, []time.Time{oct(14, 20, 43)}) || !sameTimes(run.checks, every(oct(14, 20, 48), oct(14, 21, 13), dayCheck)) {
		t.Fatalf("final %q, sessions at %v, checks before the ask %v, checks %v, turns %v; want the second session at the 21:13 check",
			final, run.began, run.asks, run.checks, run.turns)
	}
}

// A session that made model calls before the day cap refused one counts
// its attempt and its agent time, and the next hands on its findings.
func TestWorkflowDayCapCountsASessionThatMadeCalls(t *testing.T) {
	worked := dayRefusal
	worked.Allowed = true
	capped := dayCapped
	capped.Capped = &worked
	h, run := capHarness(t, 10*time.Minute, capped, pushed("s2", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	// each session's time is its end's, ten minutes after its start: the
	// check before the ask runs at the first's end, about its 20:43
	// refusal, and the wait from 20:53 ends at the turn
	if final := h.run(t); final != "done" || !sameTimes(run.began, []time.Time{oct(14, 20, 53), oct(15, 0, 10)}) {
		t.Fatalf("final %q, sessions ended at %v; want done, the first at 20:53, the second at 00:10", final, run.began)
	}
	if !sameTimes(run.asks, []time.Time{oct(14, 20, 53)}) || !isCheck(run.inputs[0], "factory/small", policy.RefusedDayCap, oct(14, 20, 43)) {
		t.Fatalf("checks before the ask at %v, first input %+v; want one at 20:53 about the 20:43 refusal", run.asks, run.inputs[0])
	}
	if u := h.usage[0]; u.Attempts != 1 || u.AgentTime != 10*time.Minute {
		t.Fatalf("usage after the refused session = %+v, want attempt 1 and its 10 minutes", u)
	}
	if in := h.sessions[1]; in.Attempt != 2 || in.ParentSessionID != "s1" || !slices.Equal(in.Findings, capped.Findings) {
		t.Fatalf("second session = %+v, want attempt 2, child of s1, with its findings", in)
	}
}

// A human's answer during the wait runs the next session after its burst
// and stops the checks; a closed issue ends the card at once.
func TestWorkflowDayCapWaitTakesAnAnswerOrTheClose(t *testing.T) {
	h, run := capHarness(t, 0, dayCapped, pushed("s2", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "day_eur raised", Comment: 700})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 2 {
		t.Fatalf("final = %q after %d sessions, want done after 2", final, len(h.sessions))
	}
	if want := oct(14, 21, 43).Add(30 * time.Second); !run.began[1].Equal(want) {
		t.Fatalf("second session began at %v, want %v: the answer and its quiet window", run.began[1], want)
	}
	if !sameTimes(run.checks, every(oct(14, 20, 48), oct(14, 21, 43), dayCheck)) || len(run.turns) != 0 {
		t.Fatalf("checks %v, turns %v; want none after the answer", run.checks, run.turns)
	}
	if want := []string{fmt.Sprintf("sebastian, to %q: day_eur raised", dayQuestion)}; !slices.Equal(h.sessions[1].Answers, want) {
		t.Fatalf("answers = %q, want %q", h.sessions[1].Answers, want)
	}

	h, _ = capHarness(t, 0, dayCapped)
	h.signalAfter(time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	if final := h.run(t); final != "closed" || len(h.sessions) != 1 || !h.finishAt.Equal(oct(14, 21, 43)) {
		t.Fatalf("final = %q after %d sessions at %v, want closed after one at 21:43", final, len(h.sessions), h.finishAt)
	}
}

// A refusal whose day turned before the workflow read it waits for
// nothing, and runs no check.
func TestWorkflowDayCapPastItsTurnRunsAtOnce(t *testing.T) {
	yesterday := dayRefusal
	yesterday.At = oct(13, 23, 59)
	turned := dayCapped
	turned.Capped = &yesterday
	h, run := capHarness(t, 0, turned, pushed("s2", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.escalated) != 0 || len(run.began) != 2 || !run.began[1].Equal(run.began[0]) || len(run.inputs) != 0 {
		t.Fatalf("final = %q, escalations %q, sessions at %v, checks %+v; want done, none asked or checked, the second at once", final, h.escalated, run.began, run.inputs)
	}
}

// A healed run of a card waiting on the day cap waits on with the day
// cap's reason, not the restart's, checks at once and then every five
// minutes, and resumes on its own: checks from 20:43 to 23:58, the turn at
// 00:00.
func TestWorkflowDayCapWaitSurvivesAHeal(t *testing.T) {
	h, run := capHarness(t, 0, pushed("s1", "c1"))
	h.state = RunState{Status: "waiting-human", Question: dayQuestion, Cap: dayReason, CapUntil: oct(15, 0, 0)}
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || !slices.Equal(h.escalated, []string{dayReason}) || len(run.began) != 1 || !run.began[0].Equal(oct(15, 0, 0)) {
		t.Fatalf("final %q, escalations %q, sessions at %v; want the day cap's reason again and the session at 00:00", final, h.escalated, run.began)
	}
	if !sameTimes(run.checks, every(oct(14, 20, 43), oct(14, 23, 58), dayCheck)) || !sameTimes(run.turns, []time.Time{oct(15, 0, 0)}) {
		t.Fatalf("checks %v, turns %v; want every five minutes from 20:43, the turn at 00:00", run.checks, run.turns)
	}
	if in := h.sessions[0]; in.Attempt != 1 {
		t.Fatalf("session = %+v, want attempt 1", in)
	}
}

// A run from before versionDayCap takes a day cap's refusal as no push.
func TestWorkflowDayCapBeforeTheChange(t *testing.T) {
	h, run := capHarness(t, 0, dayCapped, pushed("s2", "c1"))
	h.env.OnGetVersion("day-cap", workflow.DefaultVersion, versionDayCap).Return(workflow.DefaultVersion)
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.escalated) != 0 || len(run.began) != 2 || !run.began[1].Equal(run.began[0]) {
		t.Fatalf("final = %q, escalations %q, sessions at %v; want done, none asked, the second at once", final, h.escalated, run.began)
	}
	if in := h.sessions[1]; in.Attempt != 2 || !slices.Equal(in.Findings, dayCapped.Findings) {
		t.Fatalf("second session = %+v, want attempt 2 with the refusal's findings", in)
	}
}

// Card #1352 (2026-10-31): the month cap refused its first call at
// 23:22:52. The check before the ask finds the cap binding, so the card
// asks the month's wait and waits on its own. Version 972's load at
// 23:31:02 wakes the wait, whose check 30 seconds later
// (gateway.PolicyTTL), at 23:31:32, finds the raise and resumes the card
// with no answer: the next session starts then as attempt 1 with the
// first's input. No check ran between the ask and the wake: the hourly one
// would have come at 00:00, the month's turn.
func TestWorkflowMonthCapResumesOnAPolicyLoad(t *testing.T) {
	h, run := capHarness(t, 0, monthCapped, pushed("s2", "c1"))
	h.env.SetStartTime(oct31(23, 22, 52))
	run.funded = oct31(23, 31, 32)
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(8*time.Minute+10*time.Second, SignalPolicyLoaded, TaskSignal{Author: "factoryd"}) // 23:31:02
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if !slices.Equal(h.escalated, []string{monthReason}) || escalationSource(h.escalated[0]) != SourceMonthCap {
		t.Fatalf("escalations = %q, want the month cap's wait once, source month_cap: %q", h.escalated, monthReason)
	}
	if !sameTimes(run.asks, []time.Time{oct31(23, 22, 52)}) || !isCheck(run.inputs[0], "factory/small", policy.RefusedMonthCap, oct31(23, 22, 52)) {
		t.Fatalf("checks before the ask at %v, first input %+v; want one at 23:22:52 about the 23:22:52 refusal", run.asks, run.inputs[0])
	}
	if !sameTimes(run.checks, []time.Time{oct31(23, 31, 32)}) || len(run.turns) != 0 || !isCheck(run.inputs[1], "factory/small", policy.RefusedMonthCap, time.Time{}) {
		t.Fatalf("checks %v, turns %v, inputs %+v; want the month's check at 23:31:32 alone", run.checks, run.turns, run.inputs)
	}
	if !sameTimes(run.began, []time.Time{oct31(23, 22, 52), oct31(23, 31, 32)}) {
		t.Fatalf("sessions began at %v, want the second at 23:31:32", run.began)
	}
	if in := h.sessions[1]; in.Attempt != 1 || in.ParentSessionID != "" || len(in.Findings) != 0 || len(in.Answers) != 0 {
		t.Fatalf("second session = %+v, want attempt 1 with the first's input and no answer", in)
	}
}

// Card #1353 (2026-10-31): a session that made calls was refused at the
// month cap at 23:28 and ended at 23:51:36, after version 972's raise
// settled at 23:31:32. The check before the ask, about the 23:28 refusal,
// finds the cap lifted: the card asks nothing, and the next session starts
// at once as attempt 2, a child of the first with its findings, its
// check-in asked about the dead round.
func TestWorkflowMonthCapLiftedBeforeTheAskAsksNothing(t *testing.T) {
	h, run := capHarness(t, 0, monthCappedAt(oct31(23, 28, 0), true), pushed("s2", "c1"))
	h.env.SetStartTime(oct31(23, 51, 36))
	h.taskConfig = askTaskConfig()
	run.funded = oct31(23, 31, 32)
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.escalated) != 0 {
		t.Fatalf("final %q, escalations %q; want done with nothing asked", final, h.escalated)
	}
	if !sameTimes(run.asks, []time.Time{oct31(23, 51, 36)}) || !isCheck(run.inputs[0], "factory/small", policy.RefusedMonthCap, oct31(23, 28, 0)) || len(run.checks) != 0 {
		t.Fatalf("checks before the ask at %v, inputs %+v, wait checks %v; want one at 23:51:36 about the 23:28 refusal, no wait", run.asks, run.inputs, run.checks)
	}
	if !sameTimes(run.began, []time.Time{oct31(23, 51, 36), oct31(23, 51, 36)}) {
		t.Fatalf("sessions began at %v, want the second at once", run.began)
	}
	in := h.sessions[1]
	if in.Attempt != 2 || in.ParentSessionID != "s1" || !slices.Equal(in.Findings, monthCapped.Findings) {
		t.Fatalf("second session = %+v, want attempt 2, child of s1, with its findings", in)
	}
	if ask := in.CheckInAsk; ask == nil || ask.Moment != CheckInDead || ask.ParentSession != "s1" {
		t.Fatalf("check-in ask = %+v, want the dead round of s1", ask)
	}
}

// A month cap's wait that nothing lifts checks every hour and ends at the
// UTC month's turn: refused at 21:10 on 2026-10-31, it checks at 22:10 and
// 23:10, then 50 minutes later at 00:00 on 2026-11-01, the turn's check,
// and the session runs then as attempt 1.
func TestWorkflowMonthCapWaitEndsAtTheMonthsTurn(t *testing.T) {
	h, run := capHarness(t, 0, monthCappedAt(oct31(21, 10, 0), false), pushed("s2", "c1"))
	h.env.SetStartTime(oct31(21, 10, 0))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || !slices.Equal(h.escalated, []string{monthReason}) {
		t.Fatalf("final %q, escalations %q; want done after the month cap's wait", final, h.escalated)
	}
	if !sameTimes(run.asks, []time.Time{oct31(21, 10, 0)}) || !sameTimes(run.checks, []time.Time{oct31(22, 10, 0), oct31(23, 10, 0)}) || !sameTimes(run.turns, []time.Time{novFirst}) {
		t.Fatalf("checks before the ask %v, checks %v, turns %v; want 21:10, then 22:10 and 23:10, the turn at 00:00", run.asks, run.checks, run.turns)
	}
	if !sameTimes(run.began, []time.Time{oct31(21, 10, 0), novFirst}) || h.sessions[1].Attempt != 1 {
		t.Fatalf("sessions began at %v (%+v), want attempt 1 again at 00:00", run.began, h.sessions)
	}
}

// An attempt the floor refuses at the month cap waits on its own as the
// day cap's does, with no check before the ask, the floor having just
// read the cap: the card asks the month's wait with the floor's state,
// checks every hour from 21:10, and the 23:10 check, the first after the
// 22:30 raise, starts attempt 1 with no answer. The policy load at 21:09,
// during the two minutes Reconcile takes from 21:08, came before the
// floor's read and wakes nothing (a wake would check at 21:10:30).
func TestWorkflowMonthCapAtTheGateWaitsOnItsOwn(t *testing.T) {
	h, run := capHarness(t, 0, pushed("s1", "c1"))
	h.env.SetStartTime(oct31(21, 8, 0))
	h.reconcileAfter = 2 * time.Minute
	h.signalAfter(time.Minute, SignalPolicyLoaded, TaskSignal{Author: "factoryd"})
	run.funded = oct31(22, 30, 0)
	const state = "0.000001 EUR of the month's 544.00 EUR (budget.month_eur) is free, and the cheapest call on factory/small needs 0.015360 EUR " +
		"(1024 output tokens at the hold machinery's minimum); an answer would start an attempt whose first call cannot run."
	h.env.OnActivity(ActivitySessionFunded, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, model string) (string, error) {
			if h.env.Now().Before(run.funded) {
				return "the month cap cannot fund a call: Raise budget.month_eur in policy.yaml by a commit and answer, or wait for the next UTC month and answer.\n" + state, nil
			}
			return "", nil
		})
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	want := "the month cap is reached: the card resumes on its own once the month's free headroom funds a session, at the latest when the UTC month turns at " +
		"2026-11-01T00:00:00Z. Raise budget.month_eur in policy.yaml to resume it sooner, or close the card.\n" +
		"the attempt's floor refused the attempt before it started (refused_month_cap): " + state
	if !slices.Equal(h.escalated, []string{want}) || escalationSource(h.escalated[0]) != SourceMonthCap {
		t.Fatalf("escalations = %q, want the month cap's wait once: %q", h.escalated, want)
	}
	if len(run.asks) != 0 || !sameTimes(run.checks, []time.Time{oct31(22, 10, 0), oct31(23, 10, 0)}) || len(run.turns) != 0 ||
		!isCheck(run.inputs[0], "factory/small", policy.RefusedMonthCap, time.Time{}) {
		t.Fatalf("checks before an ask %v, checks %v, turns %v, inputs %+v; want hourly month checks at 22:10 and 23:10 alone", run.asks, run.checks, run.turns, run.inputs)
	}
	if !sameTimes(run.began, []time.Time{oct31(23, 10, 0)}) || h.sessions[0].Attempt != 1 || len(h.sessions[0].Answers) != 0 {
		t.Fatalf("sessions began at %v (%+v), want attempt 1 at 23:10 with no answer", run.began, h.sessions)
	}
}

// A healed run of a card waiting on the month cap asks the month's wait
// again and checks at once, so a raise during the restart is seen then,
// and hourly after: from 21:10 the checks at 21:10 and 22:10, the latter
// past the 22:00 raise, which starts the session. The check at once reads
// the cap in force, so the policy load at 21:09, during the two minutes
// Reconcile takes from 21:08, wakes nothing (a wake would check again at
// 21:10:30).
func TestWorkflowMonthCapWaitSurvivesAHeal(t *testing.T) {
	h, run := capHarness(t, 0, pushed("s1", "c1"))
	h.env.SetStartTime(oct31(21, 8, 0))
	h.reconcileAfter = 2 * time.Minute
	h.signalAfter(time.Minute, SignalPolicyLoaded, TaskSignal{Author: "factoryd"})
	run.funded = oct31(22, 0, 0)
	h.state = RunState{Status: "waiting-human", Question: monthQuestion, Cap: monthReason, CapUntil: novFirst}
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || !slices.Equal(h.escalated, []string{monthReason}) {
		t.Fatalf("final %q, escalations %q; want the month cap's reason again", final, h.escalated)
	}
	if !sameTimes(run.checks, []time.Time{oct31(21, 10, 0), oct31(22, 10, 0)}) || len(run.asks) != 0 || len(run.turns) != 0 ||
		!isCheck(run.inputs[0], "factory/small", policy.RefusedMonthCap, time.Time{}) {
		t.Fatalf("checks %v, checks before an ask %v, turns %v, inputs %+v; want month checks at 21:10 and 22:10", run.checks, run.asks, run.turns, run.inputs)
	}
	if !sameTimes(run.began, []time.Time{oct31(22, 10, 0)}) || h.sessions[0].Attempt != 1 {
		t.Fatalf("sessions began at %v (%+v), want attempt 1 at 22:10", run.began, h.sessions)
	}
}

// A run from before versionCapWait waits on a human at the month cap: it
// asks capRefused's reason, runs no check of either kind, the policy load
// at 23:31:02 changes nothing, the refused session counts as an attempt,
// and only the answer at 00:22:52 starts the next, after its 30-second
// quiet window.
func TestWorkflowMonthCapBeforeTheChangeWaitsOnAHuman(t *testing.T) {
	h, run := capHarness(t, 0, monthCapped, pushed("s2", "c1"))
	h.env.SetStartTime(oct31(23, 22, 52))
	h.env.OnGetVersion("cap-wait", workflow.DefaultVersion, versionCapWait).Return(workflow.DefaultVersion)
	run.funded = oct31(23, 31, 32)
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(8*time.Minute+10*time.Second, SignalPolicyLoaded, TaskSignal{Author: "factoryd"})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "raised", Comment: 700})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	want := "the month cap is reached: the gateway refused this task's spend (refused_month_cap): free headroom 0.000001 EUR (task 2.000000, day 12.000000, month 0.000001)"
	if !slices.Equal(h.escalated, []string{want}) {
		t.Fatalf("escalations = %q, want %q", h.escalated, want)
	}
	if len(run.inputs) != 0 || len(run.dayChecks) != 0 || len(run.dayTurns) != 0 {
		t.Fatalf("CapResume inputs %+v, DayResume checks %v and turns %v; want no check", run.inputs, run.dayChecks, run.dayTurns)
	}
	if !sameTimes(run.began, []time.Time{oct31(23, 22, 52), time.Date(2026, 11, 1, 0, 23, 22, 0, time.UTC)}) {
		t.Fatalf("sessions began at %v, want the second at 00:23:22, the answer's quiet window after 00:22:52", run.began)
	}
	if h.usage[0].Attempts != 1 || h.sessions[1].Attempt != 2 {
		t.Fatalf("usage %+v, second session %+v; want the refused session counted, the next attempt 2", h.usage, h.sessions[1])
	}
}

// A refusal inside the gateway's cache window still asks: the first call
// was refused at 23:31:10 under the cached version 971, after version 972
// activated at 23:31:02. The check before the ask finds 972 binding at the
// gateway only from 23:31:32 (Settles), so the card asks, and the wait's
// first check runs then, 22 seconds later rather than an hour, and
// resumes the card. No session ran between.
func TestWorkflowMonthCapRefusedInTheCacheWindow(t *testing.T) {
	h, run := capHarness(t, 0, monthCappedAt(oct31(23, 31, 10), false), pushed("s2", "c1"))
	h.env.SetStartTime(oct31(23, 31, 10))
	settles := oct31(23, 31, 32)
	run.verdict = func(now time.Time) CapResumed {
		if now.Before(settles) {
			return CapResumed{Settles: settles}
		}
		return CapResumed{Resume: true}
	}
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || !slices.Equal(h.escalated, []string{monthReason}) {
		t.Fatalf("final %q, escalations %q; want the month cap asked", final, h.escalated)
	}
	if !sameTimes(run.asks, []time.Time{oct31(23, 31, 10)}) || !sameTimes(run.checks, []time.Time{settles}) {
		t.Fatalf("checks before the ask %v, checks %v; want 23:31:10, then 23:31:32", run.asks, run.checks)
	}
	if !sameTimes(run.began, []time.Time{oct31(23, 31, 10), settles}) {
		t.Fatalf("sessions began at %v, want the second at 23:31:32 and none between", run.began)
	}
}

// A policy load drains where the cap was last read, the check before the
// ask. One signalled before that read brings no early check: the load at
// 21:05 came during the session that ended at 21:10 (each session's time
// is its end's), so the wait's first check is the hourly one at 22:10,
// which finds the 22:00 raise; a wake would have checked at 21:10:30.
// Loads after the read, during the minute Escalate takes from 21:10, wake
// the wait that starts at 21:11: its check comes 30 seconds later, at
// 21:11:30, and finds the 21:11 raise. Three of them in one burst arm one
// 30-second timer.
func TestWorkflowCapWaitWakesOnLoadsAfterTheRead(t *testing.T) {
	h, run := capHarness(t, 10*time.Minute, monthCappedAt(oct31(21, 0, 0), false), pushed("s2", "c1"))
	h.env.SetStartTime(oct31(21, 0, 0))
	run.funded = oct31(22, 0, 0)
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Minute, SignalPolicyLoaded, TaskSignal{Author: "factoryd"})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || !slices.Equal(h.escalated, []string{monthReason}) {
		t.Fatalf("final %q, escalations %q; want the month cap asked", final, h.escalated)
	}
	if !sameTimes(run.asks, []time.Time{oct31(21, 10, 0)}) || !sameTimes(run.checks, []time.Time{oct31(22, 10, 0)}) {
		t.Fatalf("checks before the ask %v, checks %v; want 21:10, then 22:10 alone", run.asks, run.checks)
	}
	if !sameTimes(run.began, []time.Time{oct31(21, 10, 0), oct31(22, 20, 0)}) {
		t.Fatalf("sessions ended at %v, want the second to start at 22:10 and end at 22:20", run.began)
	}

	h, run = capHarness(t, 0, monthCappedAt(oct31(21, 10, 0), false), pushed("s2", "c1"))
	h.env.SetStartTime(oct31(21, 10, 0))
	h.escalateAfter = time.Minute
	run.funded = oct31(21, 11, 0)
	var wakes []time.Time
	h.env.SetOnTimerScheduledListener(func(_ string, d time.Duration) {
		// answerQuiet is 30 s too; only the burst's re-arm before the first check is a wake.
		if d == gateway.PolicyTTL && len(run.checks) == 0 {
			wakes = append(wakes, h.env.Now())
		}
	})
	h.onChecks(ChecksResult{State: ChecksSuccess})
	for range 3 {
		h.signalAfter(30*time.Second, SignalPolicyLoaded, TaskSignal{Author: "factoryd"})
	}
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || !slices.Equal(h.escalated, []string{monthReason}) {
		t.Fatalf("final %q, escalations %q; want the month cap asked", final, h.escalated)
	}
	if !sameTimes(run.asks, []time.Time{oct31(21, 10, 0)}) || !sameTimes(run.checks, []time.Time{oct31(21, 11, 30)}) || !sameTimes(wakes, []time.Time{oct31(21, 11, 0)}) {
		t.Fatalf("checks before the ask %v, checks %v, 30-second timers at %v; want 21:10, then 21:11:30, one timer at 21:11", run.asks, run.checks, wakes)
	}
	if !sameTimes(run.began, []time.Time{oct31(21, 10, 0), oct31(21, 11, 30)}) {
		t.Fatalf("sessions began at %v, want the second at 21:11:30", run.began)
	}
}

// A check whose Settles the workflow's clock already passed (the runner's
// clock lags it) brings the next check settleRetry later, not an hour
// later: the check before the ask at 21:10 and the check at 21:10:10 both
// answer Settles 21:09:50, and the check at 21:10:20 resumes the card.
func TestWorkflowCapWaitChecksSoonOnAPassedSettles(t *testing.T) {
	h, run := capHarness(t, 0, monthCappedAt(oct31(21, 10, 0), false), pushed("s2", "c1"))
	h.env.SetStartTime(oct31(21, 10, 0))
	run.verdict = func(now time.Time) CapResumed {
		if now.Before(oct31(21, 10, 20)) {
			return CapResumed{Settles: oct31(21, 9, 50)}
		}
		return CapResumed{Resume: true}
	}
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || !slices.Equal(h.escalated, []string{monthReason}) {
		t.Fatalf("final %q, escalations %q; want the month cap asked", final, h.escalated)
	}
	if !sameTimes(run.asks, []time.Time{oct31(21, 10, 0)}) || !sameTimes(run.checks, []time.Time{oct31(21, 10, 10), oct31(21, 10, 20)}) {
		t.Fatalf("checks before the ask %v, checks %v; want 21:10, then 21:10:10 and 21:10:20", run.asks, run.checks)
	}
	if !sameTimes(run.began, []time.Time{oct31(21, 10, 0), oct31(21, 10, 20)}) {
		t.Fatalf("sessions began at %v, want the second at 21:10:20", run.began)
	}
}

// The project's month cap waits as the month's does: refused at its first
// call at 21:10, the check before the ask finds it binding, the card asks
// with the project's file named, checks every hour, and the 23:10 check,
// the first after the 22:30 raise, starts attempt 1 with no answer.
func TestWorkflowProjectCapWaitsOnItsOwn(t *testing.T) {
	const refusal = "free headroom 0.000001 EUR (task 2.000000, day 12.000000, month 40.000000, project 0.000001); " +
		"the project cap budget.month_eur in projects/linkshort.yaml is spent, and only a human raises it, by a commit"
	refused := CapRefusal{Refusal: policy.Refusal{Decision: policy.RefusedProjectCap, Reason: refusal}, At: oct31(21, 10, 0)}
	capped := SessionResult{Outcome: OutcomeEscalate, SessionID: "s1", Reason: capRefused(refused.Refusal), Capped: &refused}
	h, run := capHarness(t, 0, capped, pushed("s2", "c1"))
	h.env.SetStartTime(oct31(21, 10, 0))
	run.funded = oct31(22, 30, 0)
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	want := "the project's month cap is reached: the card resumes on its own once the project's free month headroom funds a session, " +
		"at the latest when the UTC month turns at 2026-11-01T00:00:00Z. Raise budget.month_eur in projects/linkshort.yaml to resume it sooner, or close the card.\n" +
		"the gateway refused this task's spend (refused_project_cap): " + refusal
	if !slices.Equal(h.escalated, []string{want}) || escalationSource(h.escalated[0]) != "project_cap" {
		t.Fatalf("escalations = %q, want the project cap's wait once: %q", h.escalated, want)
	}
	if !sameTimes(run.asks, []time.Time{oct31(21, 10, 0)}) || !isCheck(run.inputs[0], "factory/small", policy.RefusedProjectCap, oct31(21, 10, 0)) ||
		!sameTimes(run.checks, []time.Time{oct31(22, 10, 0), oct31(23, 10, 0)}) || !isCheck(run.inputs[1], "factory/small", policy.RefusedProjectCap, time.Time{}) {
		t.Fatalf("checks before the ask %v, checks %v, inputs %+v; want 21:10, then project checks at 22:10 and 23:10", run.asks, run.checks, run.inputs)
	}
	if !sameTimes(run.began, []time.Time{oct31(21, 10, 0), oct31(23, 10, 0)}) || h.sessions[1].Attempt != 1 || len(h.sessions[1].Answers) != 0 {
		t.Fatalf("sessions began at %v (%+v), want attempt 1 again at 23:10 with no answer", run.began, h.sessions)
	}
}

// A policy load while a check runs arms the next check gateway.PolicyTTL
// after that check, not an hour after it. Each check takes a minute, its
// time its end's: the check before the ask ends at 21:11, the hourly one
// runs from 22:11 to 22:12 with the load at 22:11:30 during it, and the
// next runs from 22:12:30 to 22:13:30, past the 22:13 raise.
func TestWorkflowCapWaitTakesALoadDuringACheck(t *testing.T) {
	h, run := capHarness(t, 0, monthCappedAt(oct31(21, 10, 0), false), pushed("s2", "c1"))
	h.env.SetStartTime(oct31(21, 10, 0))
	run.funded, run.takes = oct31(22, 13, 0), time.Minute
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour+90*time.Second, SignalPolicyLoaded, TaskSignal{Author: "factoryd"}) // 22:11:30
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || !slices.Equal(h.escalated, []string{monthReason}) {
		t.Fatalf("final %q, escalations %q; want the month cap asked", final, h.escalated)
	}
	if !sameTimes(run.asks, []time.Time{oct31(21, 11, 0)}) || !sameTimes(run.checks, []time.Time{oct31(22, 12, 0), oct31(22, 13, 30)}) {
		t.Fatalf("checks before the ask %v, checks %v; want 21:11, then 22:12 and 22:13:30", run.asks, run.checks)
	}
	if !sameTimes(run.began, []time.Time{oct31(21, 10, 0), oct31(22, 13, 30)}) {
		t.Fatalf("sessions began at %v, want the second at 22:13:30", run.began)
	}
}

// stampedPolicy is a policy store that activates each version it records at
// activate, the test environment's clock.
type stampedPolicy struct {
	memPolicy
	activate time.Time
}

func (p *stampedPolicy) RecordPolicy(ctx context.Context, commit string, tree policy.Tree) (policy.Version, bool, error) {
	v, inserted, err := p.memPolicy.RecordPolicy(ctx, commit, tree)
	if inserted {
		v.ActivatedAt = p.activate
		p.memPolicy.mu.Lock()
		p.memPolicy.versions[len(p.memPolicy.versions)-1] = v
		p.memPolicy.mu.Unlock()
	}
	return v, inserted, err
}

// A card waits at the month cap of 544 EUR, its session refused at its
// first call (21:10). A commit raising budget.month_eur to 600 loads at
// 21:30: the loader's Wake signals the waiting workflow, and the check
// gateway.PolicyTTL later, at 21:30:30, finds the cap lifted by the version
// now in force. The card starts its next session then, with no answer and
// no restart of any part; the timeline holds cap_resumed. The hourly check
// (22:10) never came.
func TestWorkflowPolicyLoadResumesAMonthCapWait(t *testing.T) {
	ctx := context.Background()
	h, run := capHarness(t, 0, monthCappedAt(oct31(21, 10, 0), false), pushed("s2", "c1"))
	h.env.SetStartTime(oct31(21, 10, 0))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	g := newCapRig(t)
	if _, err := g.store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t1", Kind: IntakeEscalation, Author: "factoryd", Body: monthReason, CreatedAt: oct31(21, 10, 0)}); err != nil {
		t.Fatal(err)
	}
	src, pol := &memSource{}, &stampedPolicy{activate: oct31(20, 0, 0)}
	policyYAML := func(monthEUR string) map[string][]byte {
		return testFiles(strings.Replace(testPolicyYAML, "month_eur: 50", "month_eur: "+monthEUR, 1))
	}
	l := &PolicyLoader{Source: src, Store: pol}
	waits := &CapWaits{Store: g.store, Signal: func(_ context.Context, id string) error {
		h.env.SignalWorkflow(SignalPolicyLoaded, TaskSignal{Author: "factoryd"})
		return nil
	}}
	src.push("c1", policyYAML("544"))
	if err := l.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	l.Wake = waits.Wake
	g.r.now, g.r.cfg.Policy = h.env.Now, l.Current
	run.verdict = func(time.Time) CapResumed {
		g.store.capsFree = &policy.Free{Day: math.Inf(1), Month: l.Current().Document.Budget.MonthEUR - 544, Project: math.Inf(1)}
		res, err := g.r.CapResume(ctx, run.inputs[len(run.inputs)-1])
		if err != nil {
			t.Errorf("CapResume: %v", err)
		}
		return res
	}
	h.env.RegisterDelayedCallback(func() {
		src.push("c2", policyYAML("600"))
		pol.activate = h.env.Now()
		if err := l.Load(ctx); err != nil {
			t.Errorf("Load: %v", err)
		}
	}, 20*time.Minute)

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if !sameTimes(run.began, []time.Time{oct31(21, 10, 0), oct31(21, 30, 30)}) || len(h.sessions) != 2 || h.sessions[1].Attempt != 1 || len(h.sessions[1].Answers) != 0 {
		t.Fatalf("sessions began at %v (%+v), want the second at 21:30:30, attempt 1, no answer", run.began, h.sessions)
	}
	if !sameTimes(run.checks, []time.Time{oct31(21, 30, 30)}) {
		t.Fatalf("checks at %v, want the one 30 s after the load", run.checks)
	}
	if !slices.Equal(h.escalated, []string{monthReason}) {
		t.Fatalf("escalations = %q, want the cap's wait once", h.escalated)
	}
	if step := timelineStep(t, g.objects, "t1", "cap_resumed"); step["cap"] != policy.RefusedMonthCap || step["why"] != "the month cap no longer binds a session (policy version 2)" {
		t.Fatalf("cap_resumed = %v", step)
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

// podPendingWhy is why a session's pod never started: the scheduler found
// no node with the ephemeral-storage its emptyDirs request.
func podPendingWhy(session string) string {
	return "pod worker-" + session + " still pending after 10m0s: Unschedulable: 0/3 nodes are available: 3 Insufficient ephemeral-storage."
}

// notStartedSession is a session whose pod never left Pending, as
// sessionResult ends it.
func notStartedSession(session string) SessionResult {
	why := podPendingWhy(session)
	return SessionResult{Outcome: OutcomeNotStarted, SessionID: session, Summary: why,
		Findings: []string{"previous session ended without a pushed commit: " + why}}
}

// podWaitHarness answers RunSession with waits sessions whose pods never
// started, 10 minutes each, then with then after 5 minutes.
func podWaitHarness(t *testing.T, waits int, then SessionResult) *workflowHarness {
	h := newWorkflowHarness(t)
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(10 * time.Minute).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			return notStartedSession(fmt.Sprintf("s%d", len(h.sessions))), nil
		}).Times(waits)
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(5 * time.Minute).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			return then, nil
		})
	return h
}

// A session whose pod never left Pending ran no agent (versionPodWait):
// no attempt, no agent time, and the next starts at once with the same
// input. Two pods waited 10 minutes each and the third session pushed
// after 5: one attempt, 5 minutes of agent time, no one asked.
func TestWorkflowSessionWhosePodNeverStartedIsNoAttempt(t *testing.T) {
	h := podWaitHarness(t, 2, pushed("s3", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 3 || len(h.escalated) != 0 {
		t.Fatalf("final = %q after %d sessions, escalations %q; want done after 3, none asked", final, len(h.sessions), h.escalated)
	}
	first := h.sessions[0]
	for i, in := range h.sessions {
		if in.Attempt != 1 || in.ParentSessionID != "" || in.Model != first.Model || !slices.Equal(in.Findings, first.Findings) {
			t.Fatalf("session %d = %+v, want attempt 1 with the first's input", i+1, in)
		}
	}
	if want := (TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}); len(h.usage) == 0 || h.usage[len(h.usage)-1] != want {
		t.Fatalf("usage = %+v, want %+v", h.usage, want)
	}
}

// Three sessions in a row whose pods never started ask a human, naming
// what the last one waited on (source capacity); the answer starts the
// fourth, still attempt 1, and the 30 minutes of waiting are no agent
// time.
func TestWorkflowPodsThatNeverStartAskAHumanAtTheBound(t *testing.T) {
	h := podWaitHarness(t, 3, pushed("s4", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "a node is free", Comment: 700})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 4 {
		t.Fatalf("final = %q after %d sessions, want done after 4", final, len(h.sessions))
	}
	want := "No node started the session's pod: 3 sessions in a row waited for it, and no agent ran. Free what it waits on (a node's cpu, " +
		"memory or ephemeral storage, or the registry of its image), then answer to start the next session. The last wait: " + podPendingWhy("s3")
	if !slices.Equal(h.escalated, []string{want}) || escalationSource(want) != "capacity" {
		t.Fatalf("escalations = %q, want %q from source capacity", h.escalated, want)
	}
	if in := h.sessions[3]; in.Attempt != 1 || len(in.Answers) != 1 || !strings.HasSuffix(in.Answers[0], ": a node is free") {
		t.Fatalf("fourth session = %+v, want attempt 1 with the answer", in)
	}
	if want := (TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}); h.usage[len(h.usage)-1] != want {
		t.Fatalf("usage = %+v, want %+v", h.usage, want)
	}
}

// A run from before versionPodWait takes such a session as no push: an
// attempt with its 10 minutes, its finding handed on.
func TestWorkflowPodNeverStartedBeforeTheChange(t *testing.T) {
	h := podWaitHarness(t, 1, pushed("s2", "c1"))
	h.env.OnGetVersion("pod-wait", workflow.DefaultVersion, versionPodWait).Return(workflow.DefaultVersion)
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 2 || len(h.escalated) != 0 {
		t.Fatalf("final = %q after %d sessions, escalations %q; want done after 2", final, len(h.sessions), h.escalated)
	}
	if in := h.sessions[1]; in.Attempt != 2 || in.ParentSessionID != "s1" || !slices.Contains(in.Findings, notStartedSession("s1").Findings[0]) {
		t.Fatalf("second session = %+v, want attempt 2, child of s1, with its finding", in)
	}
	if want := (TaskUsage{AgentTime: 15 * time.Minute, Attempts: 2}); h.usage[len(h.usage)-1] != want {
		t.Fatalf("usage = %+v, want %+v", h.usage, want)
	}
}

// A report session whose pod never started fails no report task
// (versionPodWait): it runs again at once. A run from before the change
// fails as before.
func TestWorkflowReportRunsAgainWhenItsPodNeverStarted(t *testing.T) {
	waited := ReportResult{NotStarted: podPendingWhy("s1")}
	h, began, _ := reportHarness(t, waited, ReportResult{})
	if final := h.run(t); final != "done" || len(h.escalated) != 0 || len(*began) != 2 || !(*began)[1].Equal((*began)[0]) {
		t.Fatalf("final %q, escalations %q, sessions at %v; want done after a second session at once", final, h.escalated, *began)
	}

	h, _, _ = reportHarness(t, waited)
	h.env.OnGetVersion("pod-wait", workflow.DefaultVersion, versionPodWait).Return(workflow.DefaultVersion)
	if final := h.run(t); final != "failed" || h.finals[0][1] != "report session: the report session ended without a report: "+podPendingWhy("s1") {
		t.Fatalf("finals %v, want the report task failed", h.finals)
	}
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

// A report session the gateway refused at a cap fails no report task
// (roadmap 240): it waits on its own and runs again, at the day cap for
// the day's turn. At the month's (versionCapWait) the check before the ask
// at 21:10 finds the month spent, and the hourly check at 22:10, past the
// 22:00 raise, runs the session again. A run from before versionCapWait
// waits on a human at a month's cap, and one from before versionDayCap
// fails as before.
func TestWorkflowReportWaitsOnACap(t *testing.T) {
	h, began, _ := reportHarness(t, ReportResult{Capped: &dayRefusal}, ReportResult{})
	h.env.SetStartTime(oct(14, 20, 43))
	if final := h.run(t); final != "done" || !slices.Equal(h.escalated, []string{dayReason}) || len(*began) != 2 || !(*began)[1].Equal(oct(15, 0, 0)) {
		t.Fatalf("final %q, escalations %q, sessions at %v; want done after the second session at 00:00", final, h.escalated, *began)
	}

	refused := monthRefusal
	refused.At = oct31(21, 10, 0)
	h, began, _ = reportHarness(t, ReportResult{Capped: &refused}, ReportResult{})
	h.env.SetStartTime(oct31(21, 10, 0))
	run := &capRun{funded: oct31(22, 0, 0)}
	run.onChecks(h)
	if final := h.run(t); final != "done" || !slices.Equal(h.escalated, []string{monthReason}) || !sameTimes(*began, []time.Time{oct31(21, 10, 0), oct31(22, 10, 0)}) {
		t.Fatalf("final %q, escalations %q, sessions at %v; want done after the month's wait, the second session at 22:10", final, h.escalated, *began)
	}
	if !sameTimes(run.asks, []time.Time{oct31(21, 10, 0)}) || !sameTimes(run.checks, []time.Time{oct31(22, 10, 0)}) {
		t.Fatalf("checks before the ask %v, checks %v; want 21:10, then 22:10", run.asks, run.checks)
	}

	month := CapRefusal{Refusal: policy.Refusal{Decision: policy.RefusedMonthCap, Reason: "spent"}}
	h, began, _ = reportHarness(t, ReportResult{Capped: &month}, ReportResult{})
	h.env.OnGetVersion("cap-wait", workflow.DefaultVersion, versionCapWait).Return(workflow.DefaultVersion)
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "raised", Comment: 700})
	if final := h.run(t); final != "done" || !slices.Equal(h.escalated, []string{capRefused(month.Refusal)}) || len(*began) != 2 {
		t.Fatalf("final %q, escalations %q, %d sessions; want done after a human's answer", final, h.escalated, len(*began))
	}

	// a history from before day-cap is from before cap-wait too
	h, _, _ = reportHarness(t, ReportResult{Capped: &month})
	h.env.OnGetVersion("day-cap", workflow.DefaultVersion, versionDayCap).Return(workflow.DefaultVersion)
	h.env.OnGetVersion("cap-wait", workflow.DefaultVersion, versionCapWait).Return(workflow.DefaultVersion)
	if final := h.run(t); final != "failed" || h.finals[0][1] != "report session: the report session ended without a report: "+capRefused(month.Refusal) {
		t.Fatalf("finals %v, want the report task failed", h.finals)
	}
}

var refusedReport = ReportResult{Refused: ProviderWait{Status: 429, Reason: refusedSession.Reason}}

// A report session the provider refused fails no report task (roadmap
// 155, 170): it waits 2 minutes, then 4, and runs the session again.
func TestWorkflowReportWaitsOnTheProvider(t *testing.T) {
	h, began, backoffs := reportHarness(t, refusedReport, refusedReport, ReportResult{})
	if final := h.run(t); final != "done" || h.finals[0][1] != "report kept on the task, no issue comment" {
		t.Fatalf("finals %v, want done, the report kept", h.finals)
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
	h, began, backoffs := reportHarness(t, refusedReport, ReportResult{})
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

// The review and the audit make as many calls as their diff has parts, so
// the activity carries no cap of its own beyond 6 hours: the heartbeat
// finds a lost worker, and the gate bound raises the alert. The gate
// supervisor's and the screening answer's one call keep the bound a
// review had before it fanned out: 3 calls of 15 minutes and 2, 47
// minutes, within 3 x 45 + 10, 2 h 25.
func TestFanOutOptionsHaveNoPartCap(t *testing.T) {
	opts := fanOutOptions()
	if opts.StartToCloseTimeout != 6*time.Hour || opts.ScheduleToCloseTimeout != 18*time.Hour ||
		opts.HeartbeatTimeout != 2*time.Minute || opts.RetryPolicy.MaximumAttempts != 3 {
		t.Fatalf("fan-out options = %+v", opts)
	}
	opts = reviewOptions()
	if opts.StartToCloseTimeout != 47*time.Minute || opts.ScheduleToCloseTimeout != 2*time.Hour+25*time.Minute ||
		opts.HeartbeatTimeout != 2*time.Minute || opts.RetryPolicy.MaximumAttempts != 3 {
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
	if second.Model != "factory/small" || second.ParentSessionID != "s1" || !slices.Equal(second.Findings, []string{"supervisor guidance: read the script", "the branch holds c-wip; the gates have not judged it"}) {
		t.Fatalf("second attempt = %+v, want the same rung, parent s1, the guidance and the branch as findings", second)
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
	if !slices.Equal(models, []string{"factory/small", "factory/big", "factory/big"}) {
		t.Fatalf("models = %v", models)
	}
}

// escalate: the workflow asks a human (state waiting-human, comment), a
// comment older than the escalation does not answer, a newer one starts a
// new attempt with the answer and one attempt more.
func TestWorkflowEscalationWaitsForAHumanAnswer(t *testing.T) {
	h := newWorkflowHarness(t)
	h.beforeCases()
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
	h.beforeCases()
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
	h.beforeCases()
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
// never born under the 5 minutes a session needs to start (change id
// "answer-slice"). Each case escalates with no agent time left,
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
		h.beforeCases()
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
	h.beforeCases()
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

// A new run continues the task's usage from its row (the caps
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

// Every human answer of the task travels with every later
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

func TestAnswersAndChangeRequestsCarryTheirDate(t *testing.T) {
	at := time.Date(2026, 10, 15, 13, 30, 0, 0, time.UTC)
	if got, want := answerText("sebastian", "which file?", "docs/a.txt", at), `sebastian (2026-10-15), to "which file?": docs/a.txt`; got != want {
		t.Errorf("answer = %q, want %q", got, want)
	}
	if got, want := answerText("sebastian", "which file?", "docs/a.txt", time.Time{}), `sebastian, to "which file?": docs/a.txt`; got != want {
		t.Errorf("undated answer = %q, want %q", got, want)
	}
	if got, want := changeRequested("sebastian", "rename it", at), "reviewer sebastian requested changes (2026-10-15): rename it"; got != want {
		t.Errorf("change request = %q, want %q", got, want)
	}
	late := time.Date(2026, 10, 16, 1, 30, 0, 0, time.FixedZone("CEST", 2*3600))
	if got := changeRequested("a", "b", late); got != "reviewer a requested changes (2026-10-15): b" {
		t.Errorf("a time in another zone = %q, want its UTC date, 2026-10-15", got)
	}
}

// A live answer is tagged with its comment's date and the question it answered.
func TestWorkflowAnswerReachesTheSessionDated(t *testing.T) {
	h := newWorkflowHarness(t)
	h.refusedThenFunded(1)
	h.onSessions(pushed("s1", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	at := time.Date(2026, 10, 15, 13, 30, 0, 0, time.UTC)
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602, At: at})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.escalated) != 1 || len(h.sessions) != 1 {
		t.Fatalf("final = %q, escalations %q, sessions %d", final, h.escalated, len(h.sessions))
	}
	want := fmt.Sprintf("sebastian (2026-10-15), to %q: go on", escalationQuestion(h.escalated[0]))
	if !slices.Equal(h.sessions[0].Answers, []string{want}) {
		t.Fatalf("answers = %q, want %q", h.sessions[0].Answers, want)
	}
}

// A session that ended without a push hands on its stop line, the commit its
// branch holds with the gates' judgment of it and the agent's last message.
func TestWorkflowUnpushedSessionHandsOnTheBranchAndTheLastMessage(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "stopped", Commit: "c-wip", LastMessage: "tests still fail",
			Findings: []string{"previous session ended unfinished: stopped"}},
		pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 2 {
		t.Fatalf("final = %q after %d sessions", final, len(h.sessions))
	}
	want := []string{"previous session ended unfinished: stopped", "the branch holds c-wip; the gates have not judged it", "the agent's last message: tests still fail"}
	if !slices.Equal(h.sessions[1].Findings, want) {
		t.Fatalf("second session's findings = %q, want %q", h.sessions[1].Findings, want)
	}
}

// A pushed session hands on no branch line and no last message: its commit
// goes to verification, and the next session gets the checks' findings
// alone.
func TestWorkflowPushedSessionHandsOnNoHandoverLines(t *testing.T) {
	h := newWorkflowHarness(t)
	first := pushed("s1", "c1")
	first.LastMessage = "done"
	h.onSessions(first, pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksFailure, Findings: []string{"red"}}, ChecksResult{State: ChecksSuccess})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 2 {
		t.Fatalf("final = %q after %d sessions", final, len(h.sessions))
	}
	if got := h.sessions[1].Findings; !slices.Equal(got, []string{"red"}) {
		t.Fatalf("second session's findings = %q, want the checks finding alone", got)
	}
}

func TestHandoverLinesNameTheJudgmentOfTheBranch(t *testing.T) {
	run := taskRun{judged: []string{"c1"}}
	want := []string{"the branch holds c1; the gates have judged it", "the agent's last message: done"}
	if got := run.handoverLines(SessionResult{Commit: "c1", LastMessage: "done"}); !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	if got := run.handoverLines(SessionResult{}); len(got) != 0 {
		t.Fatalf("lines = %q, want none for a session that left no branch and no message", got)
	}
}

// #837: a review that answered concerns with no finding cost a session that
// fixed nothing. It is a remark on the approval card: one session, one
// request for approval, the remark among the reasons for a human.
func TestWorkflowConcernsWithoutFindingsIsARemark(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(pushed("s1", "c1"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		Review{Verdict: "concerns", Summary: "a human decides the merge"}, nil)
	var asked []ApprovalInput
	h.env.OnActivity(ActivityRequestApproval, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID string, in ApprovalInput) error {
			asked = append(asked, in)
			return nil
		})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q", final)
	}
	if len(h.sessions) != 1 || len(asked) != 1 || !slices.Equal(asked[0].WhyHuman, []string{"review: remark, no finding: a human decides the merge"}) {
		t.Fatalf("sessions %d, approvals %+v; want one session and the remark as the reason for a human", len(h.sessions), asked)
	}
}

// A run from before the gate turns it into the finding "model review: ..." and
// pays a session for it.
func TestWorkflowBeforeReviewRemarkConcernsWithoutFindingsCostASession(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnGetVersion("review-remark", workflow.DefaultVersion, versionReviewRemark).Return(workflow.DefaultVersion)
	h.onSessions(pushed("s1", "c1"), pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	reviews := 0
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			if reviews++; reviews == 1 {
				return Review{Verdict: "concerns", Summary: "a human decides the merge"}, nil
			}
			return Review{Verdict: "approve", Summary: "fine"}, nil
		})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 2 || !slices.Equal(h.sessions[1].Findings, []string{"model review: a human decides the merge"}) {
		t.Fatalf("final = %q, sessions = %+v; want the second session sent back with the summary as its finding", final, h.sessions)
	}
}

// deadSessions answers RunSession one minute in, in order, recording each
// input: the delay lets a signal land while a session runs, so the next
// round's top drains it before the next input is built.
func deadSessions(h *workflowHarness, results ...SessionResult) {
	h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(time.Minute).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			h.sessions = append(h.sessions, in)
			return results[min(len(h.sessions), len(results))-1], nil
		})
}

// A session that ran and ended without a push is the dead moment: the next
// session's check-in asks about the stop — the dead session as the ask's
// parent, the stop reason on it, no findings waiting — while the dead
// session's stop lines ride beside it. A session whose own check-in already
// ran hands its stop lines on without a second ask, and the moment
// dissolves: no ask and no note ride the third input.
func TestWorkflowDeadSessionGetsCheckin(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	note := "the branch holds the schema change; finish the migration on top"
	deadSessions(h,
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "stopped", Commit: "c1w", LastMessage: "tests still fail",
			Findings: []string{"previous session ended unfinished: stopped"}},
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "stopped again",
			Findings: []string{"previous session ended unfinished: stopped"},
			CheckIn: &CheckInOutcome{Decision: CheckHandover, Handover: note,
				Record: &CheckInRecord{Moment: CheckInDead, Session: "s2", ParentSession: "s1"}}},
		pushed("s3", "c3"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 3 {
		t.Fatalf("final = %q after %d sessions", final, len(h.sessions))
	}
	ask := h.sessions[1].CheckInAsk
	if ask == nil || ask.Moment != CheckInDead || ask.ParentSession != "s1" || ask.Stop != "stopped" || len(ask.Findings) != 0 {
		t.Fatalf("the dead ask = %+v, want the dead moment, s1 as parent and s1's stop on it", ask)
	}
	want := []string{"previous session ended unfinished: stopped", "the branch holds c1w; the gates have not judged it", "the agent's last message: tests still fail"}
	if !slices.Equal(h.sessions[1].Findings, want) {
		t.Fatalf("the dead session's brief = %q, want %q", h.sessions[1].Findings, want)
	}
	if h.sessions[2].CheckInAsk != nil || h.sessions[2].Handover != "" {
		t.Fatalf("third session = %+v, want no second dead ask and no note: s2's check-in already ran", h.sessions[2])
	}
	if !slices.Equal(h.sessions[2].Findings, []string{"previous session ended unfinished: stopped"}) {
		t.Fatalf("third session's findings = %q, want s2's stop lines alone: its check-in replaced the handover lines", h.sessions[2].Findings)
	}
	var handover *recordedStep
	for i := range h.recorded {
		if h.recorded[i].step == "handover" {
			handover = &h.recorded[i]
		}
	}
	if handover == nil || handover.detail["handover"] != note || handover.detail["session"] != "s2" {
		t.Fatalf("handover steps = %+v, want s2's dead-moment note on the timeline", h.recorded)
	}
}

// A dead-moment check-in that needs a human stops the round before another
// session is bought: the ask names the dead moment when the check-in gave
// no reason kind, and no third session runs.
func TestWorkflowDeadCheckinHumanStopsBeforeTheConversation(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	deadSessions(h,
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "stopped",
			Findings: []string{"previous session ended unfinished: stopped"}},
		checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman,
			Reason: "the session died with its work unpushed twice",
			Record: &CheckInRecord{Moment: CheckInDead, Session: "s2", ParentSession: "s1"}}))
	h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" {
		t.Fatalf("final = %q, escalations = %q", final, h.escalated)
	}
	if len(h.sessions) != 2 {
		t.Fatalf("sessions = %d, want two: the dead session and the check-in's own", len(h.sessions))
	}
	if !slices.Equal(h.escalated, []string{"check-in: dead: the session died with its work unpushed twice"}) {
		t.Fatalf("escalations = %q, want the dead moment named", h.escalated)
	}
}

// A check-in stopped at its spent budget with the pull request open waits
// for a human, and the answer verifies the head the check-in judged again,
// like a dropped check-in: no session is bought, for it would stop at the
// same cap before any model call while the head sits green.
func TestCheckinSpendLiveReVerifiesOpenPR(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	var steps []string
	h.onVerify(&steps, []SessionResult{
		pushed("s1", "c1"),
		checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
			Reason: "the check-in's share of the budget is spent",
			Record: &CheckInRecord{Moment: CheckInGate, Session: "s2", ParentSession: "s1"}})})
	reviews := 0
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			if reviews++; reviews == 1 {
				return Review{Verdict: "concerns", Summary: "one fault", Findings: []string{"rename the flag"}}, nil
			}
			return Review{}, nil
		})
	h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the cap is spent; review the head", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if len(h.sessions) != 2 {
		t.Fatalf("sessions = %d, want two: the work and the spent check-in's own", len(h.sessions))
	}
	if h.sessions[1].CheckInAsk == nil || h.sessions[1].CheckInAsk.Moment != CheckInGate {
		t.Fatalf("the second session's ask = %+v, want the gate moment over the review's finding", h.sessions[1].CheckInAsk)
	}
	if !slices.Equal(h.escalated, []string{"check-in: spend_above_policy: the check-in's share of the budget is spent"}) {
		t.Fatalf("escalations = %q, want the spend stop alone", h.escalated)
	}
	want := []string{"session 1 after 0 cards", "pr c1 after 0 cards", "session 2 after 0 cards", "pr c1 after 1 cards", "approval c1"}
	if !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want %v: the answer verifies the same head again", steps, want)
	}
}

// The fabricated spend stop carries its work state: the findings and the
// last review verdict ride the escalation under the spend reason, so the
// card's ask holds the work, not the spend number alone.
func TestCheckinSpendStopCarriesTheWorkState(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	var steps []string
	h.onVerify(&steps, []SessionResult{
		pushed("s1", "c1"),
		checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
			Reason:   CheckinShareSpent,
			Findings: []CheckFinding{{Text: "the migration runs outside its transaction"}, {Text: "the flag is read before it is set"}},
			Review:   &Review{Verdict: "concerns", Summary: "two faults block the merge"},
			Record:   &CheckInRecord{Moment: CheckInGate, Session: "s2", ParentSession: "s1"}})})
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			return Review{Verdict: "concerns", Summary: "one fault", Findings: []string{"rename the flag"}}, nil
		})
	h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" {
		t.Fatalf("final = %q, escalations = %q", final, h.escalated)
	}
	if len(h.escalated) != 1 {
		t.Fatalf("escalations = %q, want the one spend stop", h.escalated)
	}
	for _, want := range []string{CheckinShareSpent, "- the migration runs outside its transaction", "- the flag is read before it is set", "- review: concerns"} {
		if !strings.Contains(h.escalated[0], want) {
			t.Errorf("the escalation %q misses %q", h.escalated[0], want)
		}
	}
}

// A spend stop repeated on the same head and the same limits is not asked
// again: the second ask states plainly that no reply can change it and
// names the exits. Here a run from before the checkin-answer change, whose
// answer to the share stop with the pull request open granted nothing; at
// the change the grant widens the share, and the repeat is asked as a stop
// of its own (TestCheckinRepeatGuardSeesARaise).
func TestCheckinSpendStopRepeatedWithNoChangeNamesTheExits(t *testing.T) {
	h := newWorkflowHarness(t)
	h.beforeCheckinAnswer()
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
	if len(h.escalated) != 2 {
		t.Fatalf("escalations = %q, want the stop and its repeat", h.escalated)
	}
	if h.escalated[1] == h.escalated[0] {
		t.Fatalf("the repeat ask equals the first ask: %q", h.escalated[1])
	}
	for _, want := range []string{"No reply can change this", "the head c1", "Extend the card, overrule the review, end the card, or leave it waiting"} {
		if !strings.Contains(h.escalated[1], want) {
			t.Errorf("the repeat ask %q misses %q", h.escalated[1], want)
		}
	}
}

// An answer that bought a restart which pushed nothing names the unchanged
// commit (the parked spec's loop: each grant re-ran the gates on the same
// head).
func TestCheckinRestartThatPushedNothingNamesTheCommit(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	var steps []string
	h.onVerify(&steps, []SessionResult{
		pushed("s1", "c1"),
		checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
			Reason: checkinCallCapStop, Record: &CheckInRecord{Moment: CheckInGate, Session: "s2", ParentSession: "s1"}}),
		checkinStopped("s3", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
			Reason: checkinCallCapStop, Record: &CheckInRecord{Moment: CheckInGate, Session: "s3", ParentSession: "s2"}})})
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			return Review{Verdict: "concerns", Summary: "one fault", Findings: []string{"rename the flag"}}, nil
		})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" {
		t.Fatalf("final = %q, escalations = %q", final, h.escalated)
	}
	if len(h.escalated) != 2 {
		t.Fatalf("escalations = %q, want the stop and its repeat", h.escalated)
	}
	for _, want := range []string{"no new commit", "the head is still c1"} {
		if !strings.Contains(h.escalated[1], want) {
			t.Errorf("the repeat ask %q misses %q", h.escalated[1], want)
		}
	}
}

// A stop whose head moved since the last one is asked normally: the guard
// compares the head, and a session that pushed clears it.
func TestCheckinStopAfterAMovedHeadAsksAgainNormally(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	h.taskConfig.Sessions = 6
	var steps []string
	h.onVerify(&steps, []SessionResult{
		pushed("s1", "c1"),
		checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
			Reason: CheckinShareSpent, Record: &CheckInRecord{Moment: CheckInGate, Session: "s2", ParentSession: "s1"}}),
		pushed("s3", "c2"),
		checkinStopped("s4", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
			Reason: CheckinShareSpent, Record: &CheckInRecord{Moment: CheckInGate, Session: "s4", ParentSession: "s3"}})})
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			return Review{Verdict: "concerns", Summary: "one fault", Findings: []string{"rename the flag"}}, nil
		})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" {
		t.Fatalf("final = %q, escalations = %q", final, h.escalated)
	}
	if len(h.escalated) != 2 || !strings.Contains(h.escalated[1], CheckinShareSpent) || strings.Contains(h.escalated[1], "No reply can change this") {
		t.Fatalf("escalations = %q, want the second stop asked ordinarily on the moved head", h.escalated)
	}
}

// A run from before the change repeats its stop as before: the guard is
// versioned, and an old history never sees the replacement ask.
func TestCheckinRepeatGuardIsVersioned(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	h.env.OnGetVersion("checkin-repeat", workflow.DefaultVersion, versionCheckinRepeat).Return(workflow.DefaultVersion - 1)
	var steps []string
	h.onVerify(&steps, []SessionResult{
		pushed("s1", "c1"),
		checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
			Reason: checkinCallCapStop, Record: &CheckInRecord{Moment: CheckInGate, Session: "s2", ParentSession: "s1"}}),
		checkinStopped("s3", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
			Reason: checkinCallCapStop, Record: &CheckInRecord{Moment: CheckInGate, Session: "s3", ParentSession: "s2"}})})
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			return Review{Verdict: "concerns", Summary: "one fault", Findings: []string{"rename the flag"}}, nil
		})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" {
		t.Fatalf("final = %q, escalations = %q", final, h.escalated)
	}
	if len(h.escalated) != 2 || h.escalated[1] != h.escalated[0] {
		t.Fatalf("escalations = %q, want the unversioned run repeating its ask", h.escalated)
	}
}

// A dead-moment check-in that refuses runs the round on: its session was
// sent back with the first session's stop lines and an empty handover
// brief, and the dead moment dissolves into those stop lines — the next
// input takes them with no ask.
func TestWorkflowDeadCheckinRefusedRunsOn(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	deadSessions(h,
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "stopped", Commit: "c1w", LastMessage: "tests still fail",
			Findings: []string{"previous session ended unfinished: stopped"}},
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "stopped again",
			Findings: []string{"previous session ended unfinished: stopped"},
			CheckIn: &CheckInOutcome{Decision: CheckHandover,
				Record: &CheckInRecord{Moment: CheckInDead, Session: "s2", ParentSession: "s1"}}},
		pushed("s3", "c3"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 3 {
		t.Fatalf("final = %q after %d sessions", final, len(h.sessions))
	}
	if h.sessions[1].CheckInAsk == nil || h.sessions[1].CheckInAsk.Moment != CheckInDead {
		t.Fatalf("the dead ask = %+v, want it riding the refused session", h.sessions[1].CheckInAsk)
	}
	want := []string{"previous session ended unfinished: stopped", "the branch holds c1w; the gates have not judged it", "the agent's last message: tests still fail"}
	if !slices.Equal(h.sessions[1].Findings, want) {
		t.Fatalf("the refused session's brief = %q, want %q", h.sessions[1].Findings, want)
	}
	if h.sessions[2].CheckInAsk != nil {
		t.Fatalf("third session's ask = %+v, want none: no stale dead moment survives the refused check-in", h.sessions[2].CheckInAsk)
	}
	if !slices.Equal(h.sessions[2].Findings, []string{"previous session ended unfinished: stopped"}) {
		t.Fatalf("third session's findings = %q, want the second session's stop lines alone", h.sessions[2].Findings)
	}
}

// A dead session and fresh gate findings in the same round ask once: the
// gate moment wins — the findings need judging — and the dead session's
// stop lines ride the ask's findings as the session's own. The pull
// request from the first session's push is what makes the change requests
// count; the second one lands while the dead session runs. Its check-in
// did not run (the runner skips it when it cannot), so the round is dead
// under the plan's rule.
func TestWorkflowGateAskWinsOverDead(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	change1 := "reviewer sebastian requested changes: rename it"
	change2 := "reviewer sebastian requested changes: rename the page"
	deadSessions(h,
		pushed("s1", "c1"),
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "stopped",
			Findings: []string{"previous session ended unfinished: stopped"}},
		checkinStopped("s3", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckGoalConflict,
			Reason: "the change request conflicts with the card's guardrails",
			Record: &CheckInRecord{Moment: CheckInGate, Session: "s3", ParentSession: "s2"}}))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(90*time.Second, SignalChangesRequested, TaskSignal{Author: "sebastian", PR: 9, Body: "rename it"})
	h.signalAfter(100*time.Second, SignalChangesRequested, TaskSignal{Author: "sebastian", PR: 9, Body: "rename the page"})
	h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" {
		t.Fatalf("final = %q, escalations = %q", final, h.escalated)
	}
	if len(h.sessions) != 3 {
		t.Fatalf("sessions = %d, want three", len(h.sessions))
	}
	if h.sessions[1].CheckInAsk == nil || h.sessions[1].CheckInAsk.Moment != CheckInGate {
		t.Fatalf("the second session's ask = %+v, want the gate moment from the first change request", h.sessions[1].CheckInAsk)
	}
	ask := h.sessions[2].CheckInAsk
	if ask == nil || ask.Moment != CheckInGate || ask.ParentSession != "s2" ||
		!slices.Equal(ask.Findings, []CheckFinding{{Text: change1, Source: "changes"}, {Text: change2, Source: "changes"}, {Text: "previous session ended unfinished: stopped", Source: "session"}}) {
		t.Fatalf("the third session's ask = %+v, want the gate moment over the dead one, both change requests and the stop lines", ask)
	}
	if !slices.Equal(h.escalated, []string{"check-in: goal_conflict: the change request conflicts with the card's guardrails"}) {
		t.Fatalf("escalations = %q", h.escalated)
	}
}

// A mid-session handover rides the next session's brief when no ask goes
// with it: the note the check-in's model wrote reaches the pod as the
// brief's handover section, not as a finding. The first session's push
// opens the pull request whose change request buys the second session at
// the gate; the note's session runs ask-less, so the third carries the
// note.
func TestWorkflowMidSessionHandoverRidesTheBrief(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	note := "the pod dies at the next restart: continue from the branch"
	change := "reviewer sebastian requested changes: rename it"
	deadSessions(h,
		pushed("s1", "c1"),
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "stopped", Commit: "c2w",
			LastMessage: "the migration is half applied",
			Findings:    []string{"previous session ended unfinished: stopped"},
			CheckIn: &CheckInOutcome{Decision: CheckHandover, Handover: note,
				Record: &CheckInRecord{Moment: CheckInSession, Session: "s2"}}},
		pushed("s3", "c3"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(90*time.Second, SignalChangesRequested, TaskSignal{Author: "sebastian", PR: 9, Body: "rename it"})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" || len(h.sessions) != 3 {
		t.Fatalf("final = %q after %d sessions", final, len(h.sessions))
	}
	if h.sessions[1].CheckInAsk == nil || h.sessions[1].CheckInAsk.Moment != CheckInGate {
		t.Fatalf("the second session's ask = %+v, want the gate moment from the change request", h.sessions[1].CheckInAsk)
	}
	if h.sessions[2].Handover != note {
		t.Fatalf("third session's handover = %q, want the mid-session note riding the brief", h.sessions[2].Handover)
	}
	if h.sessions[2].CheckInAsk != nil {
		t.Fatalf("third session's ask = %+v, want none: the handover needs no check-in", h.sessions[2].CheckInAsk)
	}
	if !slices.Equal(h.sessions[2].Findings, []string{change, "previous session ended unfinished: stopped"}) {
		t.Fatalf("third session's findings = %q, want the change request and the stop lines without the note", h.sessions[2].Findings)
	}
}

// A mid-session handover whose next session carries an ask is timeline
// only: the ask crowds the note out of the brief, the handover step holds
// it for the ask's round to read, and the handover lines the note
// replaces do not ride either. The pull request from the first session's
// push makes both change requests count; the second lands while the
// note's session runs. That session is ask-less — the second session's
// check-in ran at the gate and refused — so without the ask the note
// would ride.
func TestWorkflowMidSessionHandoverBeforeAnAskIsTimelineOnly(t *testing.T) {
	cfg := testWorkflowConfig
	cfg.MaxSessions = 4
	h := newWorkflowHarness(t, TaskWorkflow{Config: func() WorkflowConfig { return cfg }})
	h.taskConfig = askTaskConfig()
	note := "stay on the branch: the change request renames the page"
	change1 := "reviewer sebastian requested changes: rename it"
	change2 := "reviewer sebastian requested changes: rename the page"
	deadSessions(h,
		pushed("s1", "c1"),
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "stopped",
			Findings: []string{"previous session ended unfinished: stopped"},
			CheckIn: &CheckInOutcome{Decision: CheckHandover,
				Record: &CheckInRecord{Moment: CheckInGate, Session: "s2", ParentSession: "s1"}}},
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s3", Summary: "stopped again", Commit: "c3w",
			LastMessage: "the page still renders",
			Findings:    []string{"previous session ended unfinished: stopped"},
			CheckIn: &CheckInOutcome{Decision: CheckHandover, Handover: note,
				Record: &CheckInRecord{Moment: CheckInSession, Session: "s3", ParentSession: "s2"}}},
		checkinStopped("s4", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckGoalConflict,
			Reason: "the change request asks for a rename the card forbids",
			Record: &CheckInRecord{Moment: CheckInGate, Session: "s4", ParentSession: "s3"}}))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(90*time.Second, SignalChangesRequested, TaskSignal{Author: "sebastian", PR: 9, Body: "rename it"})
	h.signalAfter(3*time.Minute, SignalChangesRequested, TaskSignal{Author: "sebastian", PR: 9, Body: "rename the page"})
	h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" {
		t.Fatalf("final = %q, escalations = %q", final, h.escalated)
	}
	if len(h.sessions) != 4 {
		t.Fatalf("sessions = %d, want four", len(h.sessions))
	}
	if h.sessions[2].CheckInAsk != nil {
		t.Fatalf("the note session's ask = %+v, want none", h.sessions[2].CheckInAsk)
	}
	ask := h.sessions[3].CheckInAsk
	if ask == nil || ask.Moment != CheckInGate {
		t.Fatalf("the fourth session's ask = %+v, want the gate moment", ask)
	}
	if h.sessions[3].Handover != "" {
		t.Fatalf("fourth session's handover = %q, want none: an ask crowds the note out to the timeline", h.sessions[3].Handover)
	}
	if !slices.Equal(h.sessions[3].Findings, []string{change1, change2, "previous session ended unfinished: stopped"}) {
		t.Fatalf("fourth session's findings = %q, want the change requests and the stop lines without the handover lines the note replaced", h.sessions[3].Findings)
	}
	var handover *recordedStep
	for i := range h.recorded {
		if h.recorded[i].step == "handover" {
			handover = &h.recorded[i]
		}
	}
	if handover == nil || handover.detail["handover"] != note || handover.detail["session"] != "s3" {
		t.Fatalf("handover steps = %+v, want s3's note on the timeline for the ask's round to read", h.recorded)
	}
}

// activityOrder records the activities the run starts, in order, a
// RecordStep with its step.
func (h *workflowHarness) activityOrder() *[]string {
	var order []string
	h.env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, args converter.EncodedValues) {
		name := info.ActivityType.Name
		if name == ActivityRecordStep {
			var id, step string
			if err := args.Get(&id, &step); err == nil {
				name += " " + step
			}
		}
		order = append(order, name)
	})
	h.env.SetOnActivityCanceledListener(func(info *activity.Info) {
		order = append(order, "cancel "+info.ActivityType.Name)
	})
	return &order
}

// before says a comes before b in order, both there.
func before(order []string, a, b string) bool {
	i, j := slices.Index(order, a), slices.Index(order, b)
	return i >= 0 && j >= 0 && i < j
}

// replanSteps are the details of the replan steps recorded, in order.
func (h *workflowHarness) replanSteps() []map[string]any {
	var out []map[string]any
	for _, s := range h.recorded {
		if s.step == stepReplan {
			out = append(out, s.detail)
		}
	}
	return out
}

// beforeBecomePlan says the replan step ran right before BecomePlan.
func beforeBecomePlan(order []string) bool {
	i := slices.Index(order, ActivityBecomePlan)
	return i > 0 && order[i-1] == ActivityRecordStep+" "+stepReplan
}

func becomeRefused(why string) func() (PlanSwitchResult, error) {
	return func() (PlanSwitchResult, error) { return PlanSwitchResult{Refused: why}, nil }
}

// The check-in's replan of a direct card cuts it in place: the replan step
// of the cut, then BecomePlan with the check-in's reason and text, then the
// plan loop; no second coding session.
func TestCheckinReplanSwitchesADirectCard(t *testing.T) {
	h, calls := toPlanHarness(t, becomeSwitched,
		timedSession{res: SessionResult{Outcome: OutcomeReplan, SessionID: "s1", Trip: "size", Reason: "two packages", Replan: "R"}})
	order := h.activityOrder()
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	want := map[string]any{"origin": ReplanCut, "card": "t1", "work": "R"}
	if got := h.replanSteps(); len(got) != 1 || !reflect.DeepEqual(got[0], want) || !beforeBecomePlan(*order) {
		t.Fatalf("replan steps %v, activities %q; want %v right before BecomePlan", got, *order, want)
	}
	if want := []becomePlanCall{{"t1", PlanSwitch{Reason: "cut: two packages", Work: "R"}, "running"}}; !slices.Equal(*calls, want) {
		t.Fatalf("BecomePlan calls %+v, want %+v", *calls, want)
	}
	if len(h.sessions) != 1 || len(h.plans) != 1 {
		t.Fatalf("coding sessions %d, planner sessions %d; want 1, 1", len(h.sessions), len(h.plans))
	}

	// a person's replan sent while the check-in cut the card joins the
	// cut: their name and note on its step, their words after its own
	h, calls = toPlanHarness(t, becomeSwitched,
		timedSession{took: 10 * time.Minute, res: SessionResult{Outcome: OutcomeReplan, SessionID: "s1", Trip: "size", Reason: "two packages", Replan: "R"}})
	h.toPlanAfter(5*time.Minute, toPlanTooBig)
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("with a person's replan: finals %v", h.finals)
	}
	want = map[string]any{"origin": ReplanCut, "card": "t1", "work": "R", "by": "ana", "comment": "too big"}
	if got := h.replanSteps(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("with a person's replan: replan steps %v, want %v", got, want)
	}
	if want := []becomePlanCall{{"t1", PlanSwitch{Reason: "cut: two packages; too big", Work: "R\nAsked by ana."}, "running"}}; !slices.Equal(*calls, want) {
		t.Fatalf("with a person's replan: BecomePlan calls %+v, want %+v", *calls, want)
	}
}

// A person's replan stops the running session; the stop takes the
// person's to_plan, sent before it: their name and note, no cut. With no
// to_plan pending the stop's own reason and text stand in.
func TestPersonsReplanOfARunningDirectCard(t *testing.T) {
	stop := SessionResult{Outcome: OutcomeReplan, SessionID: "s1", Trip: string(TripReplan), Reason: "replanned by ana: too big", Replan: "replanned by ana: too big"}
	h, calls := toPlanHarness(t, becomeSwitched, timedSession{took: 10 * time.Minute, res: stop})
	h.toPlanAfter(5*time.Minute, toPlanTooBig)
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	want := map[string]any{"origin": ReplanPerson, "by": "ana", "comment": "too big", "card": "t1"}
	if got := h.replanSteps(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("replan steps %v, want %v", got, want)
	}
	if want := []becomePlanCall{{"t1", toPlanAsked, "running"}}; !slices.Equal(*calls, want) {
		t.Fatalf("BecomePlan calls %+v, want %+v", *calls, want)
	}

	h, calls = toPlanHarness(t, becomeSwitched, timedSession{took: 10 * time.Minute, res: stop})
	if final := h.run(t); final != "done" {
		t.Fatalf("finals %v", h.finals)
	}
	want = map[string]any{"origin": ReplanPerson, "comment": "replanned by ana: too big", "card": "t1"}
	if got := h.replanSteps(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("replan steps %v, want %v", got, want)
	}
	if want := []becomePlanCall{{"t1", PlanSwitch{Reason: "replanned by ana: too big", Work: "replanned by ana: too big"}, "running"}}; !slices.Equal(*calls, want) {
		t.Fatalf("BecomePlan calls %+v, want %+v", *calls, want)
	}
}

// onReplanChild answers ReplanChild with res and records the work of each
// call.
func (h *workflowHarness) onReplanChild(res ReplanChildResult) *[]string {
	var works []string
	h.env.OnActivity(ActivityReplanChild, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, childID, work string) (ReplanChildResult, error) {
			works = append(works, work)
			return res, nil
		})
	return &works
}

// The check-in's replan of a plan's child goes to its plan: the child ends
// closed with ReplanChild's reason and turns no plan-track. A plan that
// cannot take it asks a person with the refusal, whose answer buys the
// next session.
func TestCheckinReplanClosesAPlansChild(t *testing.T) {
	cut := SessionResult{Outcome: OutcomeReplan, SessionID: "s1", Trip: "size", Reason: "two packages", Replan: "R"}
	h := newWorkflowHarness(t)
	h.state.Source = SourcePlan
	became := h.onBecomePlan(PlanSwitchResult{Switched: true})
	works := h.onReplanChild(ReplanChildResult{Reason: "cut: plan #10 replans its work"})
	h.onSessions(cut)
	if final := h.run(t); final != "closed" || h.finals[0][1] != "cut: plan #10 replans its work" {
		t.Fatalf("finals %v", h.finals)
	}
	if !slices.Equal(*works, []string{"R"}) || *became != 0 || len(h.sessions) != 1 {
		t.Fatalf("ReplanChild works %q, BecomePlan calls %d, sessions %d; want [R], 0, 1", *works, *became, len(h.sessions))
	}

	refused := "Plan #10 is closed, so this card's work cannot go back to its planner: answer to go on with the card, or close it."
	h = newWorkflowHarness(t)
	h.state.Source = SourcePlan
	became = h.onBecomePlan(PlanSwitchResult{Switched: true})
	h.onReplanChild(ReplanChildResult{Refused: refused})
	h.onSessions(cut, pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" || h.finals[0][1] != "pull request merged" {
		t.Fatalf("finals %v", h.finals)
	}
	if !slices.Equal(h.escalated, []string{refused}) || len(h.sessions) != 2 || *became != 0 {
		t.Fatalf("escalated %q, sessions %d, BecomePlan calls %d; want the refusal, 2, 0", h.escalated, len(h.sessions), *became)
	}
}

// A person's replan wakes a card that waits on a person: running, the
// replan step, BecomePlan, the plan loop. A comment that came with it
// answers nothing: the switch records it as ignored. A refused switch
// moves the card back to waiting-human, and a later comment answers the
// wait.
func TestReplanWakesACardWaitingOnAPerson(t *testing.T) {
	noPush := SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "no changes to push"}
	h, calls := toPlanHarness(t, becomeSwitched, timedSession{res: noPush})
	h.beforeCases()
	h.state = RunState{Status: "running", Usage: TaskUsage{Attempts: 2}}
	order := h.activityOrder()
	h.toPlanAfter(time.Hour, toPlanTooBig)
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(h.escalated) != 1 || !strings.HasPrefix(h.escalated[0], capReached) {
		t.Fatalf("escalations %q, want the session cap's", h.escalated)
	}
	want := map[string]any{"origin": ReplanPerson, "by": "ana", "comment": "too big", "card": "t1"}
	if got := h.replanSteps(); len(got) != 1 || !reflect.DeepEqual(got[0], want) || !beforeBecomePlan(*order) {
		t.Fatalf("replan steps %v, activities %q; want %v right before BecomePlan", got, *order, want)
	}
	if want := []becomePlanCall{{"t1", toPlanAsked, "running running"}}; !slices.Equal(*calls, want) {
		t.Fatalf("BecomePlan calls %+v, want %+v", *calls, want)
	}
	if len(h.sessions) != 1 || len(h.plans) != 1 || len(h.plans[0]) != 0 {
		t.Fatalf("coding sessions %d, planner sessions %q; want 1, one with no findings", len(h.sessions), h.plans)
	}
	if !slices.ContainsFunc(h.recorded, func(s recordedStep) bool {
		return s.step == "signal" && cmpOrAny(s.detail["comment"]) == "602" && s.detail["ignored"] == "written before the card went to the planner"
	}) {
		t.Fatalf("steps %+v, want comment 602 recorded as written before the card went to the planner", h.recorded)
	}

	refused := "Card #1's project follows no process with a plan track, so its work cannot go back to the planner: split the work into smaller cards."
	h, calls = toPlanHarness(t, becomeRefused(refused), timedSession{res: noPush}, timedSession{res: pushed("s2", "c2")})
	h.beforeCases()
	h.state = RunState{Status: "running", Usage: TaskUsage{Attempts: 2}}
	h.toPlanAfter(time.Hour, toPlanTooBig)
	h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" || h.finals[0][1] != "pull request merged" {
		t.Fatalf("finals %v", h.finals)
	}
	if !slices.Equal(h.states[:3], []string{"running", "running", "waiting-human"}) || len(*calls) != 1 {
		t.Fatalf("states %q, BecomePlan calls %d; want running, running, waiting-human first, one call", h.states, len(*calls))
	}
	if !slices.ContainsFunc(h.recorded, func(s recordedStep) bool { return s.step == "to_plan_refused" && s.detail["reason"] == refused }) {
		t.Fatalf("steps %+v, want to_plan_refused with %q", h.recorded, refused)
	}
	if len(h.sessions) != 2 || len(h.plans) != 0 {
		t.Fatalf("coding sessions %d, planner sessions %d; want 2, 0", len(h.sessions), len(h.plans))
	}
}

// onRequestApproval records the head of each RequestApproval call.
func (h *workflowHarness) onRequestApproval() *[]string {
	var heads []string
	h.env.OnActivity(ActivityRequestApproval, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID string, in ApprovalInput) error {
			heads = append(heads, in.Commit)
			return nil
		})
	return &heads
}

// A person's replan wakes the approval wait: the switch runs, and the old
// pull request's close after it leaves the plan loop running. A refused
// switch moves the card back to in-review and asks for the approval
// again, which the move to running had voided. A roll's and a plan
// child's approval wait leave the signal untaken.
func TestReplanWakesTheApprovalWait(t *testing.T) {
	h, calls := toPlanHarness(t, becomeSwitched, timedSession{res: pushed("s1", "c1")})
	h.toPlanAfter(time.Hour, toPlanTooBig)
	h.signalAfter(time.Hour+2*time.Minute, SignalPRClosed, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if want := []becomePlanCall{{"t1", toPlanAsked, "running in-review running"}}; !slices.Equal(*calls, want) {
		t.Fatalf("BecomePlan calls %+v, want %+v", *calls, want)
	}
	if len(h.replanSteps()) != 1 || len(h.plans) != 1 {
		t.Fatalf("replan steps %v, planner sessions %d; want one each", h.replanSteps(), len(h.plans))
	}

	refused := "Card #1's project follows no process with a plan track, so its work cannot go back to the planner: split the work into smaller cards."
	h, calls = toPlanHarness(t, becomeRefused(refused), timedSession{res: pushed("s1", "c1")})
	asked := h.onRequestApproval()
	h.onHeadChecks(ChecksResult{State: ChecksSuccess})
	superseded := h.onSupersedeApproval()
	order := h.activityOrder()
	h.toPlanAfter(time.Hour, toPlanTooBig)
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" || h.finals[0][1] != "pull request merged" {
		t.Fatalf("finals %v", h.finals)
	}
	if !slices.Equal(h.states, []string{"running", "in-review", "running", "in-review"}) || len(*calls) != 1 || !slices.Equal(*asked, []string{"c1", "c1"}) {
		t.Fatalf("states %q, BecomePlan calls %d, approval requests on %q; want running, in-review, running, in-review, 1, [c1 c1]", h.states, len(*calls), *asked)
	}
	if !slices.ContainsFunc(h.recorded, func(s recordedStep) bool { return s.step == "to_plan_refused" && s.detail["reason"] == refused }) {
		t.Fatalf("steps %+v, want to_plan_refused with %q", h.recorded, refused)
	}
	want := "superseded: ana asked to send the card back to the planner"
	i := slices.Index(*order, ActivitySupersedeApproval)
	if !slices.Equal(*superseded, []string{want}) || i < 0 || i+1 >= len(*order) || (*order)[i+1] != ActivitySetState || len(h.headChecks) != 1 {
		t.Fatalf("superseded %q, activities %q, head reads %q; want [%q] right before the move to running, one head read", *superseded, *order, h.headChecks, want)
	}

	g, _ := newRollHarness(t, "r1")
	g.state.Source = SourceRoll
	g.onChecks(ChecksResult{State: ChecksSuccess})
	became := g.onBecomePlan(PlanSwitchResult{Switched: true})
	g.signalAfter(30*time.Minute, SignalToPlan, toPlanTooBig)
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || *became != 0 {
		t.Fatalf("roll: finals %v, BecomePlan calls %d; want done, 0", g.finals, *became)
	}

	g = newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.state.Source = SourcePlan
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	became = g.onBecomePlan(PlanSwitchResult{Switched: true})
	g.signalAfter(30*time.Minute, SignalToPlan, toPlanTooBig)
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || *became != 0 {
		t.Fatalf("plan child: finals %v, BecomePlan calls %d; want done, 0", g.finals, *became)
	}
}

// A replan that comes during a roll hold switches the card when the hold
// ends, before any session.
func TestReplanDuringTheRollHoldSwitchesBeforeTheSession(t *testing.T) {
	h, calls := toPlanHarness(t, becomeSwitched, timedSession{res: pushed("s1", "c1")})
	h.env.OnGetVersion("roll-gap", workflow.DefaultVersion, versionRollNoGap).Return(versionRollGap)
	h.env.OnActivity(ActivityAwaitRollHold, mock.Anything, "t1").After(10 * time.Minute).Return(nil)
	h.toPlanAfter(5*time.Minute, toPlanTooBig)
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if want := []becomePlanCall{{"t1", toPlanAsked, "running"}}; !slices.Equal(*calls, want) || len(h.sessions) != 0 {
		t.Fatalf("BecomePlan calls %+v, coding sessions %d; want %+v, 0", *calls, len(h.sessions), want)
	}
}

// A final card reopened for a replan switches before its closed pull
// request ends it; a pull request that merged takes no replan.
func TestReopenedFinalCardSwitchesFirst(t *testing.T) {
	h, calls := toPlanHarness(t, becomeSwitched, timedSession{res: pushed("s1", "c1")})
	h.state = RunState{Status: "queued", PRClosed: true}
	h.toPlanAfter(30*time.Second, toPlanTooBig)
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if want := []becomePlanCall{{"t1", toPlanAsked, "running"}}; !slices.Equal(*calls, want) {
		t.Fatalf("BecomePlan calls %+v, want %+v", *calls, want)
	}

	h, calls = toPlanHarness(t, becomeSwitched, timedSession{res: pushed("s1", "c1")})
	h.state = RunState{Status: "queued", Merged: true}
	h.toPlanAfter(30*time.Second, toPlanTooBig)
	if final := h.run(t); final != "done" || h.finals[0][1] != "pull request merged" || len(*calls) != 0 {
		t.Fatalf("finals %v, BecomePlan calls %d; want merged, 0", h.finals, len(*calls))
	}
}

// A run from before versionReplan takes the check-in's replan as a
// question for a person, whose answer buys the next session.
func TestReplanOutcomeBelowTheVersionAsksAPerson(t *testing.T) {
	h, calls := toPlanHarness(t, becomeSwitched,
		timedSession{res: SessionResult{Outcome: OutcomeReplan, SessionID: "s1", Trip: "size", Reason: "two packages", Replan: "R"}},
		timedSession{res: pushed("s2", "c2")})
	h.env.OnGetVersion("replan", workflow.DefaultVersion, versionReplan).Return(workflow.DefaultVersion)
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" || h.finals[0][1] != "pull request merged" {
		t.Fatalf("finals %v", h.finals)
	}
	want := "The check-in asks to send this card back to the planner: two packages Replan it on the board, or answer to go on."
	if !slices.Equal(h.escalated, []string{want}) || len(h.sessions) != 2 || len(*calls) != 0 || len(h.replanSteps()) != 0 {
		t.Fatalf("escalated %q, sessions %d, BecomePlan calls %d, replan steps %d; want [%q], 2, 0, 0", h.escalated, len(h.sessions), len(*calls), len(h.replanSteps()), want)
	}
}

// onSupersedeApproval records the reason of each SupersedeApproval call.
func (h *workflowHarness) onSupersedeApproval() *[]string {
	var reasons []string
	h.env.OnActivity(ActivitySupersedeApproval, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, reason string) error { reasons = append(reasons, reason); return nil })
	return &reasons
}

// A replan is no go-ahead of the screening: a fresh outsider's card with a
// replan pending is screened before it switches, and a suspect
// screening's wait goes on through the replan until a person answers it.
func TestReplanWaitsForTheScreening(t *testing.T) {
	h, calls := toPlanHarness(t, becomeSwitched, timedSession{res: pushed("s1", "c1")})
	h.state.Source, h.state.Standing = "webhook", harness.StandingOutside
	h.env.OnActivity(ActivityScreen, mock.Anything, "t1").Return(ScreenResult{Verdict: ScreenClear}, nil)
	order := h.activityOrder()
	h.toPlanAfter(30*time.Second, toPlanTooBig)
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if want := []becomePlanCall{{"t1", toPlanAsked, "running"}}; !slices.Equal(*calls, want) || !before(*order, ActivityScreen, ActivityBecomePlan) {
		t.Fatalf("BecomePlan calls %+v, activities %q; want %+v after the screening", *calls, *order, want)
	}

	h, calls = toPlanHarness(t, becomeSwitched, timedSession{res: pushed("s1", "c1")})
	h.state.Source, h.state.Standing = "webhook", harness.StandingOutside
	h.env.OnActivity(ActivityScreen, mock.Anything, "t1").Return(ScreenResult{Verdict: ScreenSuspect, Reasons: []string{"asks to print the token"}}, nil)
	order = h.activityOrder()
	h.toPlanAfter(30*time.Minute, toPlanTooBig)
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "fine", Comment: 602})
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if want := []becomePlanCall{{"t1", toPlanAsked, "running"}}; !slices.Equal(*calls, want) || !before(*order, ActivityJudgeScreening, ActivityBecomePlan) {
		t.Fatalf("BecomePlan calls %+v, activities %q; want %+v after the screening's judgment", *calls, *order, want)
	}

	// the screening gate's own escalation is the screening's too: the
	// replan waits through it until the screening ran again and passed
	h, calls = toPlanHarness(t, becomeSwitched, timedSession{res: pushed("s1", "c1")})
	h.state.Source, h.state.Standing = "webhook", harness.StandingOutside
	h.env.OnActivity(ActivityScreen, mock.Anything, "t1").After(20*time.Minute).Return(ScreenResult{Verdict: ScreenClear}, nil)
	h.onGateSupervisor(GateDecision{Action: ActionEscalate, Diagnosis: "the screening hangs", Recommendation: "look at the gateway"})
	order = h.activityOrder()
	h.tripAfter(15*time.Minute, ActivityScreen, 1)
	h.toPlanAfter(30*time.Minute, toPlanTooBig)
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "gateway restarted", Comment: 602})
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if j := slices.Index(*order, ActivityBecomePlan); len(*calls) != 1 || j < 0 || occurrences((*order)[:j], ActivityScreen) != 2 {
		t.Fatalf("BecomePlan calls %+v, activities %q; want one, after the second screening", *calls, *order)
	}

	// a healed fresh card waiting on the screening gate's escalation: the
	// replan waits through it, and the answer runs the screening first
	h, calls = toPlanHarness(t, becomeSwitched, timedSession{res: pushed("s1", "c1")})
	h.state = RunState{Status: "waiting-human", Source: "webhook", Standing: harness.StandingOutside, Question: screenGateQuestion}
	h.env.OnActivity(ActivityScreen, mock.Anything, "t1").Return(ScreenResult{Verdict: ScreenClear}, nil)
	order = h.activityOrder()
	h.toPlanAfter(30*time.Minute, toPlanTooBig)
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if j := slices.Index(*order, ActivityBecomePlan); len(*calls) != 1 || j < 0 || occurrences((*order)[:j], ActivityScreen) != 1 {
		t.Fatalf("BecomePlan calls %+v, activities %q; want one, after the screening", *calls, *order)
	}
}

// screenGateQuestion is the question of the screening gate's escalation.
const screenGateQuestion = "The gate supervisor stopped Screen (attempt 1) past its bound of 10m0s: the screening hangs"

// occurrences is how often v stands in s.
func occurrences(s []string, v string) int {
	n := 0
	for _, x := range s {
		if x == v {
			n++
		}
	}
	return n
}

// A healed fresh card that waited on the screening gate's escalation runs
// its screening after the answer, before its first session, as a live one.
func TestHealedScreeningEscalationScreensBeforeTheSession(t *testing.T) {
	h, _ := toPlanHarness(t, becomeSwitched, timedSession{res: pushed("s1", "c1")})
	h.state = RunState{Status: "waiting-human", Source: "webhook", Standing: harness.StandingOutside, Question: screenGateQuestion}
	h.env.OnActivity(ActivityScreen, mock.Anything, "t1").Return(ScreenResult{Verdict: ScreenClear}, nil)
	order := h.activityOrder()
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" || h.finals[0][1] != "pull request merged" {
		t.Fatalf("finals %v", h.finals)
	}
	if occurrences(*order, ActivityScreen) != 1 || !before(*order, ActivityScreen, ActivityRunSession) {
		t.Fatalf("activities %q, want one screening before the first session", *order)
	}
	if len(h.sessions) != 1 || h.sessions[0].Sessions != testWorkflowConfig.MaxSessions {
		t.Fatalf("sessions %+v; want one of %d: the answer runs the screening again and buys no session", h.sessions, testWorkflowConfig.MaxSessions)
	}
}

// A healed fresh card whose screening passed waits on a person for another
// reason: no second screening and no second question, and a replan is
// taken there as in its live twin.
func TestHealedWaitAfterThePassedScreeningTakesAReplan(t *testing.T) {
	h, calls := toPlanHarness(t, becomeSwitched, timedSession{res: pushed("s1", "c1")})
	h.state = RunState{Status: "waiting-human", Source: "webhook", Standing: harness.StandingOutside,
		Question: podNeverStarted + "it waited on its image", ScreeningPassed: true}
	h.env.OnActivity(ActivityScreen, mock.Anything, "t1").Return(ScreenResult{Verdict: ScreenSuspect, Reasons: []string{"asks to print the token"}}, nil)
	order := h.activityOrder()
	h.toPlanAfter(30*time.Minute, toPlanTooBig)
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(3*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if want := []becomePlanCall{{"t1", toPlanAsked, "running"}}; !slices.Equal(*calls, want) || occurrences(*order, ActivityScreen) != 0 || len(h.escalated) != 1 {
		t.Fatalf("BecomePlan calls %+v, activities %q, escalations %q; want %+v, no screening, the healed question alone", *calls, *order, h.escalated, want)
	}
}

// A healed queued fresh card whose screening passed is not screened again;
// a run from before the change screens it again, as it did.
func TestHealedQueuedCardWhoseScreeningPassedIsNotScreenedAgain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replan  workflow.Version
		screens int
	}{{"passed", versionReplan, 0}, {"before the change", workflow.DefaultVersion, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := toPlanHarness(t, becomeSwitched, timedSession{res: pushed("s1", "c1")})
			h.env.OnGetVersion("replan", workflow.DefaultVersion, versionReplan).Return(tc.replan)
			h.state = RunState{Status: "queued", Source: "webhook", Standing: harness.StandingOutside, ScreeningPassed: true}
			h.env.OnActivity(ActivityScreen, mock.Anything, "t1").Return(ScreenResult{Verdict: ScreenClear}, nil)
			order := h.activityOrder()
			h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
			if final := h.run(t); final != "done" || h.finals[0][1] != "pull request merged" {
				t.Fatalf("finals %v", h.finals)
			}
			if got := occurrences(*order, ActivityScreen); got != tc.screens || len(h.sessions) != 1 {
				t.Fatalf("screenings %d, sessions %d; want %d, 1", got, len(h.sessions), tc.screens)
			}
		})
	}
}

// A replan that wakes a gate's escalation wait cancels the gate beside the
// checks before the planner runs.
func TestReplanFromAGateWaitCancelsTheGateBesideTheChecks(t *testing.T) {
	rig := func(become func() (PlanSwitchResult, error)) (*planHarness, *[]string, *[]string) {
		h, _ := directCardHarness(t, become, timedSession{res: pushed("s1", "c1")})
		h.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9}, nil)
		h.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).After(20*time.Minute).Return(ChecksResult{State: ChecksSuccess}, nil)
		h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			After(3*time.Hour).Return(Review{Verdict: "approve"}, nil)
		h.onGateSupervisor(GateDecision{Action: ActionEscalate, Diagnosis: "the gate hangs", Recommendation: "look at the runner"})
		var withdrawn []string
		h.env.OnActivity(ActivityWithdrawQuestion, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(
			func(ctx context.Context, taskID, reason string, comment int64, why string) (int64, error) {
				withdrawn = append(withdrawn, fmt.Sprintf("%d because %s", comment, why))
				return 900, nil
			})
		h.tripAfter(15*time.Minute, ActivityWaitChecks, 1)
		h.toPlanAfter(30*time.Minute, toPlanTooBig)
		return h, h.activityOrder(), &withdrawn
	}

	h, order, _ := rig(becomeSwitched)
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(h.escalated) != 1 || !before(*order, ActivityBecomePlan, "cancel "+ActivityModelReview) || !before(*order, "cancel "+ActivityModelReview, ActivityRunPlan) {
		t.Fatalf("escalations %q, activities %q; want one, the review beside the checks cancelled after the switch, before the planner ran", h.escalated, *order)
	}

	// the question the review beside the checks asked is withdrawn, naming
	// the replan
	h, order, withdrawn := rig(becomeSwitched)
	h.tripAfter(5*time.Minute, ActivityModelReview, 1)
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if want := []string{"601 because the card went back to the planner"}; !slices.Equal(*withdrawn, want) ||
		!before(*order, ActivityBecomePlan, ActivityWithdrawQuestion) || !before(*order, ActivityWithdrawQuestion, ActivityRunPlan) {
		t.Fatalf("withdrawn %q, activities %q; want %q after the switch, before the planner ran", *withdrawn, *order, want)
	}

	// a refused switch leaves the review beside the checks running
	refused := "Card #1's project follows no process with a plan track, so its work cannot go back to the planner: split the work into smaller cards."
	h, order, _ = rig(becomeRefused(refused))
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "runner fixed", Comment: 602})
	h.signalAfter(5*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" || h.finals[0][1] != "pull request merged" {
		t.Fatalf("finals %v", h.finals)
	}
	if slices.Contains(*order, "cancel "+ActivityModelReview) || occurrences(*order, ActivityModelReview) != 1 {
		t.Fatalf("activities %q, want the one review beside the checks never cancelled", *order)
	}
}

// A replan wakes the wait for the issue author's answer.
func TestReplanWakesThePosterWait(t *testing.T) {
	h, calls := toPlanHarness(t, becomeSwitched, timedSession{res: SessionResult{Outcome: OutcomeNeedsInfo, SessionID: "s1", Reason: "which browser?"}})
	h.toPlanAfter(time.Hour, toPlanTooBig)
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if want := []becomePlanCall{{"t1", toPlanAsked, "running running"}}; !slices.Equal(*calls, want) || len(h.infoAsked) != 1 || len(h.plans) != 1 {
		t.Fatalf("BecomePlan calls %+v, information requests %q, planner sessions %d; want %+v, one, one", *calls, h.infoAsked, len(h.plans), want)
	}
}

// A replan wakes a card parked on its image before any person is asked:
// the switch leaves the park (BecomePlan), the wait leaves nothing after.
func TestReplanWakesTheImageWait(t *testing.T) {
	h, calls := toPlanHarness(t, becomeSwitched, timedSession{res: missingTool("s1", "ffmpeg", "cut the demo video")})
	h.onPark(ffmpegPark())
	h.toPlanAfter(time.Hour, toPlanTooBig)
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if want := []becomePlanCall{{"t1", toPlanAsked, "running running"}}; !slices.Equal(*calls, want) || len(h.escalated) != 0 || len(h.left) != 0 {
		t.Fatalf("BecomePlan calls %+v, escalations %q, parks left %q; want %+v, none, none", *calls, h.escalated, h.left, want)
	}
}

// A replan wakes a card that waits for its turn in the merge queue.
func TestReplanWakesTheQueueTurn(t *testing.T) {
	h, calls := toPlanHarness(t, becomeSwitched, timedSession{res: pushed("s1", "c1")})
	h.taskConfig.Queue = policy.QueueProject
	h.env.OnActivity(ActivityEnterQueue, mock.Anything, "t1").Return(1, nil)
	h.toPlanAfter(time.Hour, toPlanTooBig)
	h.signalAfter(3*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
		t.Fatalf("finals %v", h.finals)
	}
	if want := []becomePlanCall{{"t1", toPlanAsked, "running in-review running"}}; !slices.Equal(*calls, want) || len(h.plans) != 1 {
		t.Fatalf("BecomePlan calls %+v, planner sessions %d; want %+v, one", *calls, len(h.plans), want)
	}
}

// A replan that comes while the checks run, which take none, is taken
// right before the auto merge, queued or not: the card goes to the
// planner, and nothing merges. A refused switch moves the card back to
// in-review, and it merges.
func TestReplanBeforeAnAutoMerge(t *testing.T) {
	rig := func(t *testing.T, become func() (PlanSwitchResult, error), queue string) (*planHarness, *[]becomePlanCall, *int) {
		h, calls := directCardHarness(t, become, timedSession{res: pushed("s1", "c1")})
		h.taskConfig.Queue = queue
		h.env.OnActivity(ActivityOpenPullRequest, mock.Anything, "t1", mock.Anything).Return(PullRequest{Number: 9}, nil)
		h.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).After(20*time.Minute).Return(ChecksResult{State: ChecksSuccess}, nil)
		h.env.OnActivity(ActivitySyncWithMain, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
			func(ctx context.Context, taskID string, pr int64, from string) (SyncResult, error) {
				return SyncResult{To: from}, nil
			})
		h.env.OnActivity(ActivityGatePolicy, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(
			PolicyGate{Class: "normal", Outcome: policy.OutcomeAuto, MergeRule: policy.MergeAuto}, nil)
		h.env.OnActivity(ActivityGateReproduction, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
			ReproResult{Ran: true, Exit: 0, BaseExit: 1, Command: "go test ./x"}, nil)
		yes := true
		h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
			Review{Verdict: "approve", Summary: "fine", StrictImprovement: &yes}, nil)
		merges := 0
		h.env.OnActivity(ActivityMerge, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
			func(ctx context.Context, taskID string, pr int64, commit, approver, checkedHead string, met []string) (MergeResult, error) {
				merges++
				return MergeResult{Merged: true}, nil
			})
		h.toPlanAfter(10*time.Minute, toPlanTooBig)
		return h, calls, &merges
	}
	for _, queue := range []string{policy.QueueNone, policy.QueueProject} {
		t.Run(queue, func(t *testing.T) {
			h, calls, merges := rig(t, becomeSwitched, queue)
			if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 1 filed 3 cards" {
				t.Fatalf("finals %v", h.finals)
			}
			if want := []becomePlanCall{{"t1", toPlanAsked, "running in-review running"}}; !slices.Equal(*calls, want) || *merges != 0 || len(h.autoNotes) != 0 {
				t.Fatalf("BecomePlan calls %+v, merges %d, auto merge notes %q; want %+v, none, none", *calls, *merges, h.autoNotes, want)
			}
		})
	}

	refused := "Card #1's project follows no process with a plan track, so its work cannot go back to the planner: split the work into smaller cards."
	h, calls, merges := rig(t, becomeRefused(refused), policy.QueueNone)
	if final := h.run(t); final != "done" || h.finals[0][1] != "pull request merged by factory-merge on the merge rule auto" {
		t.Fatalf("refused: finals %v", h.finals)
	}
	if !slices.Equal(h.states, []string{"running", "in-review", "running", "in-review"}) || len(*calls) != 1 || *merges != 1 {
		t.Fatalf("refused: states %q, BecomePlan calls %d, merges %d; want running, in-review, running, in-review, 1, 1", h.states, len(*calls), *merges)
	}
}

// A replan pending when a card in review starts its run is refused: the
// card goes back to in-review, and its head is verified again.
func TestReplanRefusedAtTheStartVerifiesAgain(t *testing.T) {
	refused := "Card #1's project follows no process with a plan track, so its work cannot go back to the planner: split the work into smaller cards."
	h, calls := toPlanHarness(t, becomeRefused(refused), timedSession{res: pushed("s2", "c2")})
	h.state = RunState{Status: "in-review", PR: PullRequest{Number: 9}, Head: "c1", Usage: TaskUsage{Attempts: 1}}
	asked := h.onRequestApproval()
	h.toPlanAfter(30*time.Second, toPlanTooBig)
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" || h.finals[0][1] != "pull request merged" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(*calls) != 1 || len(h.states) < 3 || !slices.Equal(h.states[:3], []string{"running", "in-review", "in-review"}) {
		t.Fatalf("BecomePlan calls %d, states %q; want one, then running, in-review (back), in-review (verify)", len(*calls), h.states)
	}
	if len(h.sessions) != 0 || !slices.Equal(*asked, []string{"c1"}) {
		t.Fatalf("sessions %d, approval requests on %q; want none, [c1]", len(h.sessions), *asked)
	}
}

// An extend answer raises the limits the guard reads: the row the extend
// form raised while the card waited reaches the run when the answer wakes
// it, so the stop after an extend is an ordinary ask, not the deadlock
// headline again.
func TestExtendAnswerRaisesTheLimitsTheGuardReads(t *testing.T) {
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	h.taskConfig.Sessions = 6
	var steps []string
	h.onVerify(&steps, []SessionResult{
		pushed("s1", "c1"),
		checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
			Reason: CheckinShareSpent, Record: &CheckInRecord{Moment: CheckInGate, Session: "s2", ParentSession: "s1"}}),
		checkinStopped("s3", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend,
			Reason: CheckinShareSpent, Record: &CheckInRecord{Moment: CheckInGate, Session: "s3", ParentSession: "s2"}})})
	// the extend lands while the card waits at its first stop
	h.env.OnActivity(ActivityReadUsage, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) (TaskUsage, error) {
			if len(h.escalated) == 0 {
				return h.rowUsage, nil
			}
			raised := h.rowUsage
			raised.ExtraLines = 500
			return raised, nil
		})
	h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
			return Review{Verdict: "concerns", Summary: "one fault", Findings: []string{"rename the flag"}}, nil
		})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "extend it", Comment: 602})
	h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

	if final := h.run(t); final != "closed" {
		t.Fatalf("final = %q, escalations = %q", final, h.escalated)
	}
	if len(h.escalated) != 2 {
		t.Fatalf("escalations = %q, want the stop and its repeat", h.escalated)
	}
	if strings.Contains(h.escalated[1], "No reply can change this") || !strings.Contains(h.escalated[1], CheckinShareSpent) {
		t.Errorf("the ask after the extend = %q, want the ordinary share ask: the raise changed the limits", h.escalated[1])
	}
}

// A card's agent time follows its size (change id "size-time"): the
// sizing step's large card gets one and a half of the class's 20 minutes,
// so a 25-minute session that pushes is within its time, where the class's
// time alone would have reached the cap; a small card gets half, so a
// 12-minute session without a push reaches it.
func TestSizedCardAgentTimeFollowsItsSize(t *testing.T) {
	for name, tc := range map[string]struct {
		size   string
		took   time.Duration
		result SessionResult
		final  string
	}{
		"large": {"large", 25 * time.Minute, pushed("s1", "c1"), "done"},
		"small": {"small", 12 * time.Minute, SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "no changes to push"}, "failed"},
	} {
		h := newWorkflowHarness(t)
		h.beforeCases()
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
		h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

		if final := h.run(t); final != tc.final || len(h.sessions) != 1 {
			t.Fatalf("%s: final = %q after %d sessions, finals = %v; want %s after one session", name, final, len(h.sessions), h.finals, tc.final)
		}
		if tc.final == "failed" && h.finals[0][1] != "wall-clock cap reached" {
			t.Fatalf("%s: reason = %q, want the cap", name, h.finals[0][1])
		}
	}
}

// The console's Extend reaches a running card (change id "raise-reload"):
// the session it ran during ends at the policy's cap of 3, but the cap
// check reads the raised session from the row first, so a fourth session
// runs with no human asked, and the usage the run records leaves the
// raise for the row to keep.
func TestRaiseDuringASessionReachesTheCapCheck(t *testing.T) {
	h := newWorkflowHarness(t)
	h.onSessions(
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "no changes to push"},
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "no changes to push"},
		SessionResult{Outcome: OutcomeNoPush, SessionID: "s3", Summary: "no changes to push"},
		pushed("s4", "c4"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	// the Extend lands while the third session runs
	h.env.OnActivity(ActivityReadUsage, mock.Anything, "t1").Return(
		func(ctx context.Context, taskID string) (TaskUsage, error) {
			row := h.rowUsage
			if len(h.sessions) >= 3 {
				row.RaisedSessions, row.ExtraLines = 1, 150
			}
			return row, nil
		})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v, escalations %q", final, h.finals, h.escalated)
	}
	if len(h.sessions) != 4 || len(h.escalated) != 0 || h.sessions[3].Attempt != 4 || h.sessions[3].Sessions != 4 {
		t.Fatalf("sessions = %d, escalations %q, fourth %+v; want a fourth session of 4 and no ask", len(h.sessions), h.escalated, h.sessions[len(h.sessions)-1])
	}
	if last := h.usage[len(h.usage)-1]; last.Attempts != 4 || last.ExtraAttempts != 0 {
		t.Fatalf("usage = %+v, want four attempts and no grant: the raise bought the fourth", last)
	}
}
