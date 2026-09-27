package factoryd

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

var testWorkflowConfig = WorkflowConfig{
	Models:        ModelLadder{Start: "factory/coding-cheap", Stronger: "factory/coding-frontier"},
	MaxSessions:   3,
	WallClockCap:  20 * time.Minute,
	ChecksTimeout: 45 * time.Minute,
}

// workflowHarness drives TaskWorkflow with every activity stubbed: the
// defaults are a clean first session, a PR, green checks; tests override
// the step they are about.
type workflowHarness struct {
	env      *testsuite.TestWorkflowEnvironment
	sessions []SessionInput
	finals   [][2]string
	states   []string
}

func newWorkflowHarness(t *testing.T) *workflowHarness {
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	h := &workflowHarness{env: suite.NewTestWorkflowEnvironment()}
	h.env.RegisterWorkflowWithOptions(TaskWorkflow{Config: testWorkflowConfig}.Run, workflow.RegisterOptions{Name: "TaskWorkflow"})
	for name, fn := range map[string]any{
		ActivityEnsureIssue:       func(ctx context.Context, taskID string) error { return nil },
		ActivitySetState:          func(ctx context.Context, taskID, state string) error { return nil },
		ActivityRunSession:        func(ctx context.Context, in SessionInput) (SessionResult, error) { return SessionResult{}, nil },
		ActivityOpenPullRequest:   func(ctx context.Context, taskID, sha string) (PullRequest, error) { return PullRequest{}, nil },
		ActivityWaitChecks:        func(ctx context.Context, taskID, sha string) (ChecksResult, error) { return ChecksResult{}, nil },
		ActivityPullRequestMerged: func(ctx context.Context, taskID string, number int64) (bool, error) { return false, nil },
		ActivityRecordStep:        func(ctx context.Context, taskID, step string, detail map[string]any) error { return nil },
		ActivityFinish:            func(ctx context.Context, taskID, final, reason string) error { return nil },
	} {
		h.env.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
	h.env.OnActivity(ActivityEnsureIssue, mock.Anything, "t1").Return(nil)
	h.env.OnActivity(ActivitySetState, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, state string) error { h.states = append(h.states, state); return nil })
	h.env.OnActivity(ActivityRecordStep, mock.Anything, "t1", mock.Anything, mock.Anything).Return(nil)
	h.env.OnActivity(ActivityFinish, mock.Anything, "t1", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, final, reason string) error {
			h.finals = append(h.finals, [2]string{final, reason})
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

func TestWorkflowRedChecksRetryStrongerFromBranchHead(t *testing.T) {
	h := newWorkflowHarness(t)
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

func TestWorkflowAttemptExhaustionFails(t *testing.T) {
	h := newWorkflowHarness(t)
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
		replayer.RegisterWorkflowWithOptions(TaskWorkflow{Config: testWorkflowConfig}.Run, workflow.RegisterOptions{Name: "TaskWorkflow"})
		if err := replayer.ReplayWorkflowHistoryFromJSONFile(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), f); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}

// A Forgejo or LiteLLM outage longer than any bounded retry must not strand
// the task: Finish retries without limit. (The test environment caps
// unlimited retries at 10, so the policy itself is what is checked.)
func TestWorkflowFinishRetriesWithoutLimit(t *testing.T) {
	opts := finishOptions()
	if opts.ScheduleToCloseTimeout != 0 || opts.RetryPolicy.MaximumAttempts != 0 || opts.RetryPolicy.MaximumInterval != 5*time.Minute {
		t.Fatalf("finish options = %+v / %+v, want unlimited attempts, no schedule-to-close, 5 min backoff cap", opts, *opts.RetryPolicy)
	}
}
