package factoryd

import (
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// TaskQueue is the Temporal task queue factoryd's worker polls.
const TaskQueue = "factory-tasks"

// WorkflowID is a task's workflow id: Temporal refuses a second start for
// the same task, so no application code has to.
func WorkflowID(taskID string) string { return "task-" + taskID }

// Signals the webhook intake delivers to a task's workflow.
const (
	SignalPRMerged         = "pr_merged"
	SignalChangesRequested = "changes_requested"
	SignalIssueClosed      = "issue_closed"
)

// TaskSignal is a signal's payload: who acted, and what they wrote.
type TaskSignal struct {
	Author string
	Body   string
}

// Activity names, registered by Runner.RegisterActivities.
const (
	ActivityEnsureIssue       = "EnsureIssue"
	ActivitySetState          = "SetState"
	ActivityRunSession        = "RunSession"
	ActivityOpenPullRequest   = "OpenPullRequest"
	ActivityWaitChecks        = "WaitChecks"
	ActivityPullRequestMerged = "PullRequestMerged"
	ActivityRecordStep        = "RecordStep"
	ActivityFinish            = "Finish"
)

// errTerminal marks an activity error no retry can fix; the workflow fails
// the task with its message.
const errTerminal = "terminal"

func terminalError(format string, args ...any) error {
	return temporal.NewNonRetryableApplicationError(fmt.Sprintf(format, args...), errTerminal, nil)
}

// ModelLadder is the one place the model aliases live (ruling 86): every
// task starts on Start; a stronger retry (red checks, a repetition trip)
// climbs to Stronger.
type ModelLadder struct {
	Start    string
	Stronger string
}

// WorkflowConfig is captured into each workflow's history when it starts, so
// a config change on a later deploy never changes a running workflow's
// replay.
type WorkflowConfig struct {
	Models      ModelLadder
	MaxSessions int
	// WallClockCap bounds the agent's working time across all sessions of
	// a task; waiting on checks or on a reviewer does not count.
	WallClockCap  time.Duration
	ChecksTimeout time.Duration
}

// SessionOutcome is how one RunSession attempt ended; the workflow switches
// on it. Task 44's supervisor adds its verdicts here.
type SessionOutcome string

const (
	OutcomePushed        SessionOutcome = "pushed"
	OutcomeNoPush        SessionOutcome = "no_push"
	OutcomeRetryStronger SessionOutcome = "retry_stronger"
	OutcomeWallClock     SessionOutcome = "wall_clock"
)

// SessionInput is one attempt: StartRef is the commit the worker starts
// from (empty = the task's base SHA).
type SessionInput struct {
	TaskID          string
	Attempt         int
	Model           string
	Findings        []string
	StartRef        string
	ParentSessionID string
}

type SessionResult struct {
	Outcome   SessionOutcome
	SessionID string
	Commit    string
	Summary   string
	Findings  []string
}

type PullRequest struct {
	Number int64
	URL    string
}

// ChecksState is WaitChecks' verdict on the PR head's combined status.
type ChecksState string

const (
	ChecksSuccess ChecksState = "success"
	ChecksFailure ChecksState = "failure"
	ChecksTimeout ChecksState = "timeout"
)

type ChecksResult struct {
	State    ChecksState
	Findings []string
}

// sessionGrace is how long past its own supervisor deadline a session may
// take to clean up before Temporal times the activity out.
const sessionGrace = 2 * time.Minute

// TaskWorkflow is a task's whole lifecycle (architecture 5.1, 6.5, 6.6):
// attempts, pull request, checks, review signals, final state.
type TaskWorkflow struct {
	Config WorkflowConfig
}

func (w TaskWorkflow) Run(ctx workflow.Context, taskID string) (string, error) {
	var cfg WorkflowConfig
	if err := workflow.SideEffect(ctx, func(workflow.Context) any { return w.Config }).Get(&cfg); err != nil {
		return "", err
	}
	short := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        5 * time.Second,
			MaximumInterval:        time.Minute,
			MaximumAttempts:        10,
			NonRetryableErrorTypes: []string{errTerminal},
		},
	})
	run := taskRun{ctx: ctx, short: short, cfg: cfg, taskID: taskID}
	return run.lifecycle()
}

type taskRun struct {
	ctx, short workflow.Context
	cfg        WorkflowConfig
	taskID     string
}

func (t *taskRun) lifecycle() (string, error) {
	if err := workflow.ExecuteActivity(t.short, ActivityEnsureIssue, t.taskID).Get(t.ctx, nil); err != nil {
		return t.finish("failed", "ensure issue: "+errMessage(err))
	}
	model := t.cfg.Models.Start
	var findings []string
	startRef, parent := "", ""
	var used time.Duration
	reason := "no session produced a result"
	for attempt := 1; attempt <= t.cfg.MaxSessions; attempt++ {
		remaining := t.cfg.WallClockCap - used
		if remaining <= 0 {
			return t.finish("failed", "wall-clock cap reached")
		}
		if err := workflow.ExecuteActivity(t.short, ActivitySetState, t.taskID, "running").Get(t.ctx, nil); err != nil {
			return t.finish("failed", "set running: "+errMessage(err))
		}
		began := workflow.Now(t.ctx)
		res, err := t.session(SessionInput{
			TaskID: t.taskID, Attempt: attempt, Model: model,
			Findings: findings, StartRef: startRef, ParentSessionID: parent,
		}, remaining)
		used += workflow.Now(t.ctx).Sub(began)
		if isWallClockTimeout(err) {
			return t.finish("failed", "wall-clock cap reached")
		}
		if err != nil {
			reason = "session failed: " + errMessage(err)
			continue
		}
		parent = res.SessionID
		reason = res.Summary
		switch res.Outcome {
		case OutcomeWallClock:
			return t.finish("failed", "wall-clock cap reached")
		case OutcomeRetryStronger:
			model, findings = t.cfg.Models.Stronger, res.Findings
			continue
		case OutcomeNoPush:
			findings = res.Findings
			continue
		}
		startRef = res.Commit

		var pr PullRequest
		if err := workflow.ExecuteActivity(t.short, ActivityOpenPullRequest, t.taskID, res.Commit).Get(t.ctx, &pr); err != nil {
			return t.finish("failed", "open pull request: "+errMessage(err))
		}
		if err := workflow.ExecuteActivity(t.short, ActivitySetState, t.taskID, "in-review").Get(t.ctx, nil); err != nil {
			return t.finish("failed", "set in-review: "+errMessage(err))
		}
		checks, err := t.waitChecks(res.Commit)
		if err != nil {
			return t.finish("failed", "wait for checks: "+errMessage(err))
		}
		switch checks.State {
		case ChecksFailure:
			model, findings = t.cfg.Models.Stronger, checks.Findings
			reason = "checks failed"
			continue
		case ChecksTimeout:
			return t.finish("failed", fmt.Sprintf("checks did not finish within %s", t.cfg.ChecksTimeout))
		}

		name, sig := t.awaitReview()
		if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "signal",
			map[string]any{"signal": name, "author": sig.Author}).Get(t.ctx, nil); err != nil {
			workflow.GetLogger(t.ctx).Warn("record signal", "error", err)
		}
		switch name {
		case SignalPRMerged:
			return t.finish("done", "pull request merged")
		case SignalIssueClosed:
			var merged bool
			if err := workflow.ExecuteActivity(t.short, ActivityPullRequestMerged, t.taskID, pr.Number).Get(t.ctx, &merged); err != nil {
				return t.finish("failed", "issue closed; merge state unknown: "+errMessage(err))
			}
			if merged {
				return t.finish("done", "pull request merged")
			}
			return t.finish("failed", "issue closed without merge")
		}
		findings = []string{"reviewer " + sig.Author + " requested changes: " + sig.Body}
		reason = "changes requested"
	}
	return t.finish("failed", fmt.Sprintf("%d attempts exhausted; last: %s", t.cfg.MaxSessions, reason))
}

// session runs one RunSession attempt. The activity heartbeats; a factoryd
// that dies mid-session misses heartbeats, and Temporal retries the attempt
// on the next worker, which resumes from the dead session's heartbeat.
// Retries share one schedule-to-close budget: the task's remaining agent
// time.
func (t *taskRun) session(in SessionInput, remaining time.Duration) (SessionResult, error) {
	ctx := workflow.WithActivityOptions(t.ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    remaining + sessionGrace,
		ScheduleToCloseTimeout: remaining + sessionGrace,
		HeartbeatTimeout:       2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        10 * time.Second,
			MaximumAttempts:        3,
			NonRetryableErrorTypes: []string{errTerminal},
		},
	})
	var res SessionResult
	err := workflow.ExecuteActivity(ctx, ActivityRunSession, in).Get(t.ctx, &res)
	return res, err
}

func (t *taskRun) waitChecks(sha string) (ChecksResult, error) {
	ctx := workflow.WithActivityOptions(t.ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    t.cfg.ChecksTimeout + time.Minute,
		ScheduleToCloseTimeout: t.cfg.ChecksTimeout + time.Minute,
		HeartbeatTimeout:       2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        10 * time.Second,
			MaximumAttempts:        5,
			NonRetryableErrorTypes: []string{errTerminal},
		},
	})
	var res ChecksResult
	err := workflow.ExecuteActivity(ctx, ActivityWaitChecks, t.taskID, sha).Get(t.ctx, &res)
	return res, err
}

// awaitReview blocks, at no cost, until a reviewer or the issue decides.
func (t *taskRun) awaitReview() (string, TaskSignal) {
	var name string
	var sig TaskSignal
	sel := workflow.NewSelector(t.ctx)
	for _, n := range []string{SignalPRMerged, SignalChangesRequested, SignalIssueClosed} {
		sel.AddReceive(workflow.GetSignalChannel(t.ctx, n), func(c workflow.ReceiveChannel, more bool) {
			c.Receive(t.ctx, &sig)
			name = n
		})
	}
	sel.Select(t.ctx)
	return name, sig
}

func (t *taskRun) finish(final, reason string) (string, error) {
	if err := workflow.ExecuteActivity(t.short, ActivityFinish, t.taskID, final, reason).Get(t.ctx, nil); err != nil {
		return "", err
	}
	return final, nil
}

func errMessage(err error) string {
	var app *temporal.ApplicationError
	if errors.As(err, &app) {
		return app.Message()
	}
	return err.Error()
}

// isWallClockTimeout is a RunSession attempt that ran out of the task's
// agent time, as opposed to one whose worker stopped heartbeating.
func isWallClockTimeout(err error) bool {
	var timeout *temporal.TimeoutError
	if !errors.As(err, &timeout) {
		return false
	}
	switch timeout.TimeoutType() {
	case enumspb.TIMEOUT_TYPE_START_TO_CLOSE, enumspb.TIMEOUT_TYPE_SCHEDULE_TO_CLOSE:
		return true
	}
	return false
}

func isTerminal(err error) bool {
	var app *temporal.ApplicationError
	return errors.As(err, &app) && app.Type() == errTerminal
}
