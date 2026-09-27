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
	SignalPRClosed         = "pr_closed"
	SignalChangesRequested = "changes_requested"
	SignalIssueClosed      = "issue_closed"
)

// TaskSignal is a signal's payload: who acted, what they wrote, and on
// which pull request (0 for an issue event). The workflow counts a pull
// request signal only for the pull request it opened itself.
type TaskSignal struct {
	Author string
	Body   string
	PR     int64
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
	run := taskRun{ctx: ctx, short: short, cfg: cfg, taskID: taskID,
		v: workflow.GetVersion(ctx, "signals-drained", workflow.DefaultVersion, versionSignalsDrained)}
	return run.lifecycle()
}

// Workflow versions. A running workflow can wait days for its reviewer, so
// every change to the command sequence TaskWorkflow issues must be gated
// with workflow.GetVersion: add a version constant, branch on it, and keep
// the old branch until no workflow started before the change is still
// running (temporal workflow list --query 'ExecutionStatus="Running"').
const (
	// versionSignalsDrained: terminal signals are acted on at every
	// decision point, change requests are coalesced, pull request signals
	// count only for the task's own PR, a terminal session error finishes
	// the task.
	versionSignalsDrained workflow.Version = 1
)

type taskRun struct {
	ctx, short workflow.Context
	cfg        WorkflowConfig
	taskID     string
	v          workflow.Version
	pr         PullRequest
}

func (t *taskRun) lifecycle() (string, error) {
	drained := t.v >= versionSignalsDrained
	if err := workflow.ExecuteActivity(t.short, ActivityEnsureIssue, t.taskID).Get(t.ctx, nil); err != nil {
		return t.finish("failed", "ensure issue: "+errMessage(err))
	}
	model := t.cfg.Models.Start
	var findings []string
	startRef, parent := "", ""
	var used time.Duration
	reason := "no session produced a result"
	for attempt := 1; attempt <= t.cfg.MaxSessions; attempt++ {
		if drained {
			if final, why, ok := t.decided(); ok {
				return t.finish(final, why)
			}
			findings = append(findings, t.drainChanges()...)
		}
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
		if drained && isTerminal(err) {
			return t.finish("failed", "session: "+errMessage(err))
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
		if drained {
			if final, why, ok := t.decided(); ok {
				return t.finish(final, why)
			}
		}

		if err := workflow.ExecuteActivity(t.short, ActivityOpenPullRequest, t.taskID, res.Commit).Get(t.ctx, &t.pr); err != nil {
			return t.finish("failed", "open pull request: "+errMessage(err))
		}
		if err := workflow.ExecuteActivity(t.short, ActivitySetState, t.taskID, "in-review").Get(t.ctx, nil); err != nil {
			return t.finish("failed", "set in-review: "+errMessage(err))
		}
		checks, err := t.waitChecks(res.Commit)
		if err != nil {
			return t.finish("failed", "wait for checks: "+errMessage(err))
		}
		if drained {
			if final, why, ok := t.decided(); ok {
				return t.finish(final, why)
			}
		}
		switch checks.State {
		case ChecksFailure:
			model, findings = t.cfg.Models.Stronger, checks.Findings
			reason = "checks failed"
			continue
		case ChecksTimeout:
			return t.finish("failed", fmt.Sprintf("checks did not finish within %s", t.cfg.ChecksTimeout))
		}

		await := t.awaitDecision
		if !drained {
			await = t.reviewV0
		}
		final, why, requested := await()
		if final != "" {
			return t.finish(final, why)
		}
		findings = requested
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

// decided drains the terminal signals without blocking and reports the
// decision they make, if any. It runs before every attempt, before a pull
// request is opened and after the checks, so a merge or a rejection is
// never overridden by another attempt.
func (t *taskRun) decided() (final, reason string, ok bool) {
	var sig TaskSignal
	for workflow.GetSignalChannel(t.ctx, SignalPRMerged).ReceiveAsync(&sig) {
		if t.counted(SignalPRMerged, sig) {
			return "done", "pull request merged", true
		}
	}
	for workflow.GetSignalChannel(t.ctx, SignalPRClosed).ReceiveAsync(&sig) {
		if t.counted(SignalPRClosed, sig) {
			return "failed", "pull request closed without merge", true
		}
	}
	if workflow.GetSignalChannel(t.ctx, SignalIssueClosed).ReceiveAsync(&sig) {
		t.counted(SignalIssueClosed, sig)
		final, reason, _ := t.issueClosed()
		return final, reason, true
	}
	return "", "", false
}

// drainChanges coalesces every pending change request (one review with
// many comments sends many) into one findings list for one attempt.
func (t *taskRun) drainChanges() []string {
	var out []string
	var sig TaskSignal
	for workflow.GetSignalChannel(t.ctx, SignalChangesRequested).ReceiveAsync(&sig) {
		if t.counted(SignalChangesRequested, sig) {
			out = append(out, "reviewer "+sig.Author+" requested changes: "+sig.Body)
		}
	}
	return out
}

// awaitDecision blocks, at no cost, until a terminal signal decides the
// task or a reviewer requests changes.
func (t *taskRun) awaitDecision() (final, reason string, findings []string) {
	for {
		sel := workflow.NewSelector(t.ctx)
		for _, n := range []string{SignalPRMerged, SignalPRClosed, SignalIssueClosed, SignalChangesRequested} {
			sel.AddReceive(workflow.GetSignalChannel(t.ctx, n), func(workflow.ReceiveChannel, bool) {})
		}
		sel.Select(t.ctx)
		if final, reason, ok := t.decided(); ok {
			return final, reason, nil
		}
		if findings := t.drainChanges(); len(findings) > 0 {
			return "", "", findings
		}
	}
}

// counted records a received signal on the timeline and reports whether it
// counts: an issue event always, a pull request event only for the pull
// request this workflow opened (a hand-opened PR on the task branch is not
// the task's).
func (t *taskRun) counted(name string, sig TaskSignal) bool {
	counts := name == SignalIssueClosed || (t.pr.Number != 0 && sig.PR == t.pr.Number)
	detail := map[string]any{"signal": name, "author": sig.Author, "pr": sig.PR}
	if !counts {
		detail["ignored"] = "not the task's pull request"
	}
	if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "signal", detail).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("record signal", "error", err)
	}
	return counts
}

// issueClosed tells a merge, which closes the issue through "Closes #N",
// from a plain close.
func (t *taskRun) issueClosed() (final, reason string, findings []string) {
	if t.pr.Number == 0 {
		return "failed", "issue closed while no pull request of this workflow was open", nil
	}
	var merged bool
	if err := workflow.ExecuteActivity(t.short, ActivityPullRequestMerged, t.taskID, t.pr.Number).Get(t.ctx, &merged); err != nil {
		return "failed", "issue closed; merge state unknown: " + errMessage(err), nil
	}
	if merged {
		return "done", "pull request merged", nil
	}
	return "failed", "issue closed without merge", nil
}

// reviewV0 is the review wait of workflows started before
// versionSignalsDrained: the first signal decides.
func (t *taskRun) reviewV0() (final, reason string, findings []string) {
	var name string
	var sig TaskSignal
	sel := workflow.NewSelector(t.ctx)
	for _, n := range []string{SignalPRMerged, SignalPRClosed, SignalChangesRequested, SignalIssueClosed} {
		sel.AddReceive(workflow.GetSignalChannel(t.ctx, n), func(c workflow.ReceiveChannel, more bool) {
			c.Receive(t.ctx, &sig)
			name = n
		})
	}
	sel.Select(t.ctx)
	if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "signal",
		map[string]any{"signal": name, "author": sig.Author}).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("record signal", "error", err)
	}
	switch name {
	case SignalPRMerged:
		return "done", "pull request merged", nil
	case SignalPRClosed:
		return "failed", "pull request closed without merge", nil
	case SignalIssueClosed:
		return t.issueClosed()
	}
	return "", "", []string{"reviewer " + sig.Author + " requested changes: " + sig.Body}
}

// finish is the one exit of every path. It retries without limit (backoff
// capped at five minutes): giving up would leave the task running or
// closing with its credentials live.
func (t *taskRun) finish(final, reason string) (string, error) {
	ctx := workflow.WithActivityOptions(t.ctx, finishOptions())
	if err := workflow.ExecuteActivity(ctx, ActivityFinish, t.taskID, final, reason).Get(t.ctx, nil); err != nil {
		return "", err
	}
	return final, nil
}

// finishOptions: MaximumAttempts 0 is unlimited, and no schedule-to-close
// bounds the retries.
func finishOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        5 * time.Second,
			MaximumInterval:        5 * time.Minute,
			MaximumAttempts:        0,
			NonRetryableErrorTypes: []string{errTerminal},
		},
	}
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
