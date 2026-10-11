package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/temporal"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// prBase is the branch every task pull request targets.
const prBase = "main"

// WorkflowDirs are the top-level directories whose contents run as
// workflows on push; an agent branch must never change any of them
// (ruling 100). The worker refuses the push, OpenPullRequest re-checks.
var WorkflowDirs = []string{".forgejo", ".gitea", ".github"}

// HeartbeatEvery is how often a long activity reports progress and the
// longest the worker holds a heartbeat back (MaxHeartbeatThrottleInterval):
// a workflow's cancel reaches a running activity in the answer to its next
// heartbeat, so within this. The heartbeat timeout is 2 minutes.
const HeartbeatEvery = 10 * time.Second

// contentFilterRefusal opens the escalation of a session the model provider
// refused with its content check: every next session sends the same brief and
// is refused the same way, so only a human changing the wording helps.
const contentFilterRefusal = "the model provider's content filter refused this session's model call (DataInspectionFailed): "

// RegisterActivities registers the runner's activities under the names the
// workflow calls, on a worker or a test environment. No argument or result
// carries a credential: each activity mints or reads the tokens it uses
// itself, so none reaches the workflow's history
// (TestTaskHistoryCarriesNoCredential).
func (r *Runner) RegisterActivities(w interface {
	RegisterActivityWithOptions(a any, options activity.RegisterOptions)
}) {
	for name, fn := range map[string]any{
		ActivityEnsureIssue:                 r.EnsureIssue,
		ActivitySetState:                    r.SetState,
		ActivityRunSession:                  r.RunSession,
		ActivityOpenPullRequest:             r.OpenPullRequest,
		ActivityWaitChecks:                  r.WaitChecks,
		ActivityRerunChecks:                 r.RerunChecks,
		ActivityBranchHead:                  r.BranchHead,
		ActivityPullRequestMerged:           r.PullRequestMerged,
		ActivityRecordStep:                  r.RecordStep,
		ActivityFinish:                      r.Finish,
		ActivityReconcile:                   r.Reconcile,
		ActivityEscalate:                    r.Escalate,
		ActivityRecordUsage:                 r.RecordUsage,
		ActivityGatePolicy:                  r.GatePolicy,
		ActivityGateMigration:               r.GateMigration,
		ActivityGateReproduction:            r.GateReproduction,
		ActivityModelReview:                 r.ModelReview,
		ActivityRequestApproval:             r.RequestApproval,
		ActivityMerge:                       r.Merge,
		ActivityReportMergeFailure:          r.ReportMergeFailure,
		ActivityReportMergeRetriesExhausted: r.ReportMergeRetriesExhausted,
		ActivitySuperviseGate:               r.SuperviseGate,
		ActivityAutoMergeNote:               r.AutoMergeNote,
		ActivityRequestInfo:                 r.RequestInfo,
		ActivityCloseNotReproduced:          r.CloseNotReproduced,
		ActivityScreen:                      r.Screen,
		ActivityJudgeScreening:              r.JudgeScreening,
		ActivityScreenAnswer:                r.ScreenAnswer,
		ActivityRunReport:                   r.RunReport,
		ActivityRollCommit:                  r.RollCommit,
		ActivityGateAudit:                   r.GateAudit,
		ActivityTaskConfig:                  r.TaskConfig,
		ActivityDependencies:                r.Dependencies,
		ActivityEnterQueue:                  r.EnterQueue,
		ActivitySyncWithMain:                r.SyncWithMain,
		ActivityLeaveQueue:                  r.LeaveQueue,
		ActivityRunPlan:                     r.RunPlan,
		ActivityRequestPlan:                 r.RequestPlan,
		ActivityCreateChildren:              r.CreateChildren,
	} {
		w.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
}

// TerminalForgejoErrors is the worker interceptor that ends an activity's
// retries on a Forgejo refusal or on a Forgejo 404 that escaped it, and
// scrubs every activity error of Scrub's secrets before it leaves the
// worker (ruling 152, item 48): no retry fixes a 4xx, and the activities
// that tolerate an absent object swallow ErrNotFound themselves, so one
// that escapes names an object gone for good. A kube, S3 or store
// not-found keeps retrying. Temporal reads the non-retryable type off the
// returned error itself, never off one it wraps, so the classification
// sits at the activity boundary.
type TerminalForgejoErrors struct {
	interceptor.WorkerInterceptorBase
	Scrub *redact.Scrubber
}

func (t *TerminalForgejoErrors) InterceptActivity(ctx context.Context, next interceptor.ActivityInboundInterceptor) interceptor.ActivityInboundInterceptor {
	return &terminalForgejoInbound{
		ActivityInboundInterceptorBase: interceptor.ActivityInboundInterceptorBase{Next: next},
		scrub:                          cmp.Or(t.Scrub, redact.New()),
	}
}

type terminalForgejoInbound struct {
	interceptor.ActivityInboundInterceptorBase
	scrub *redact.Scrubber
}

func (i *terminalForgejoInbound) ExecuteActivity(ctx context.Context, in *interceptor.ExecuteActivityInput) (any, error) {
	out, err := i.Next.ExecuteActivity(ctx, in)
	if err == nil || temporal.IsCanceledError(err) {
		return out, err
	}
	if forgejoTerminal(err) && !isTerminal(err) {
		err = temporal.NewNonRetryableApplicationError(err.Error(), errTerminal, nil)
	}
	return out, scrubActivityError(err, i.scrub)
}

// scrubActivityError is err with every secret s knows replaced, crossing
// the worker boundary clean. A message the scrub leaves alone stays err
// itself; one it changes keeps its ApplicationError type and non-retryable
// flag but carries no cause, so Temporal does not record it twice.
func scrubActivityError(err error, s *redact.Scrubber) error {
	var app *temporal.ApplicationError
	if errors.As(err, &app) {
		msg := app.Message()
		if clean := s.String(msg); clean != msg {
			return temporal.NewApplicationErrorWithOptions(clean, app.Type(), temporal.ApplicationErrorOptions{
				NonRetryable:   app.NonRetryable(),
				NextRetryDelay: app.NextRetryDelay(),
				Category:       app.Category(),
			})
		}
		return err
	}
	msg := err.Error()
	if clean := s.String(msg); clean != msg {
		return errors.New(clean)
	}
	return err
}

// forgejoTerminal is a Forgejo answer no retry changes: a refusal or a 404.
func forgejoTerminal(err error) bool {
	return errors.Is(err, ErrRefused) || errors.Is(err, errForgejoNotFound)
}

func (r *Runner) task(ctx context.Context, id string) (Task, Repo, error) {
	t, ok, err := r.store.Task(ctx, id)
	if err != nil {
		return Task{}, Repo{}, err
	}
	if !ok {
		return Task{}, Repo{}, terminalError("task %s not found", id)
	}
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return Task{}, Repo{}, terminalError("%v", err)
	}
	return t, repo, nil
}

// EnsureIssue checks the task is mirrored by a Forgejo issue (both intake
// paths link one before the workflow starts) and opens the timeline.
func (r *Runner) EnsureIssue(ctx context.Context, taskID string) error {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return err
	}
	// a report task comments on its parent's issue
	if t.IssueNumber == 0 && !t.Report() {
		return terminalError("task %s has no linked issue", taskID)
	}
	return r.timeline.Append(ctx, taskID, "intake", map[string]any{"source": t.Source, "issue": t.IssueNumber, "started": rfc3339(t.CreatedAt)})
}

// SetState moves the task to running (a session starts) or in-review (its
// pull request is open) and mirrors the state onto the issue.
func (r *Runner) SetState(ctx context.Context, taskID, state string) error {
	from := map[string][]string{
		"running": {"queued", "waiting-deps", "running", "in-review", "waiting-human", "waiting-poster"},
		// waiting-human: a human answered a gate's escalation and the gate runs
		// again; waiting-poster: a cap right after the author's answer
		// verifies the branch's head
		"in-review": {"running", "in-review", "waiting-human", "waiting-poster"},
	}[state]
	if from == nil {
		return terminalError("unknown task state %q", state)
	}
	cur, _, err := r.task(ctx, taskID)
	if err != nil {
		return err
	}
	if !slices.Contains(from, cur.Status) {
		return terminalError("task %s is %s, cannot move to %s", taskID, cur.Status, state)
	}
	if state == "running" {
		// the pull request's head is about to change: its approval
		// request is moot
		if err := r.store.ResolveApprovals(ctx, taskID, ApprovalMerge, "denied", FactoryDecider, "superseded: a new attempt changes the pull request"); err != nil {
			return err
		}
		err = r.store.MarkStarted(ctx, taskID)
	} else {
		err = r.store.TransitionTask(ctx, taskID, from, state)
	}
	if err != nil {
		return err
	}
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return err
	}
	mirrorTaskState(ctx, r.forgejo, t, state)
	return nil
}

// sessionProgress is RunSession's heartbeat: a retry after a crashed worker
// reads it to continue the dead session's chain.
type sessionProgress struct {
	SessionID string
	// Running is set once the worker pod runs: a retry after a setup or
	// pod-create failure has no dead session to continue.
	Running  bool
	Events   int
	Findings []string
}

// heartbeat records the latest session progress now and every
// HeartbeatEvery until stopped, including through namespace setup and a
// slow image pull, when no events flow.
type heartbeat struct {
	mu   sync.Mutex
	p    sessionProgress
	stop context.CancelFunc
}

func startHeartbeat(ctx context.Context, sessionID string) *heartbeat {
	hbCtx, stop := context.WithCancel(ctx)
	h := &heartbeat{p: sessionProgress{SessionID: sessionID}, stop: stop}
	activity.RecordHeartbeat(ctx, h.snapshot())
	go func() {
		tick := time.NewTicker(HeartbeatEvery)
		defer tick.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-tick.C:
				activity.RecordHeartbeat(ctx, h.snapshot())
			}
		}
	}()
	return h
}

func (h *heartbeat) update(events int, findings []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.p.Running, h.p.Events, h.p.Findings = true, events, findings
}

func (h *heartbeat) snapshot() sessionProgress {
	h.mu.Lock()
	defer h.mu.Unlock()
	return sessionProgress{SessionID: h.p.SessionID, Running: h.p.Running, Events: h.p.Events, Findings: slices.Clone(h.p.Findings)}
}

// RecordUsage keeps what the task used of its caps on its row; agent time
// a human's answer granted counts an extra_time intervention.
func (r *Runner) RecordUsage(ctx context.Context, taskID string, u TaskUsage) error {
	t, _, err := r.store.Task(ctx, taskID)
	if err != nil {
		return err
	}
	if err := r.store.SetTaskUsage(ctx, taskID, u); err != nil {
		return err
	}
	if u.ExtraTime > t.Usage.ExtraTime {
		process, lane := processLane(ctx, r.store, t)
		r.cfg.Metrics.Intervened(ctx, InterventionExtraTime, process, lane)
	}
	return nil
}

// RunSession is one attempt: a fresh namespace with this session's own
// Forgejo token and service account (so the gateway mints this session its
// own LiteLLM key), one worker pod, supervision, and the session record.
// A retry after a crashed or stopped worker finds the dead session in the
// heartbeat, removes its namespace and pod with its credentials, and
// starts a new session carrying the dead one's findings and id as parent.
// factoryd stopping ends the session gracefully first: the worker pushes
// its work in progress and the heartbeat carries its report.
func (r *Runner) RunSession(ctx context.Context, in SessionInput) (SessionResult, error) {
	t, repo, err := r.task(ctx, in.TaskID)
	if err != nil {
		return SessionResult{}, err
	}
	s := sessionSpec{id: newID("s-"), parent: in.ParentSessionID, findings: in.Findings, answers: in.Answers, posterInfo: in.PosterInfo, model: in.Model, repo: repo}
	if s.policy, err = r.pinned(ctx, in.PolicyVersion); err != nil {
		return SessionResult{}, err
	}
	s.agentTime, s.checkIn = cmp.Or(in.AgentTime, s.policy.Limits.AgentTime), cmp.Or(in.CheckIn, s.policy.Limits.CheckIn)
	if s.spec, s.cardRole, err = r.planOf(ctx, t); err != nil {
		return SessionResult{}, err
	}
	if err := r.forgejo.EnsureFork(ctx, repo); err != nil {
		return SessionResult{}, err
	}
	// the branch head, when the branch exists, is where every attempt
	// continues: the push of an earlier attempt, or of a session that died
	// after pushing, which a non-force push must build on
	if s.startRef, err = r.startRef(ctx, &t, repo, in.StartRef); err != nil {
		return SessionResult{}, err
	}
	s.base = t.SHA
	var dead sessionProgress
	if activity.HasHeartbeatDetails(ctx) && activity.GetHeartbeatDetails(ctx, &dead) == nil && dead.SessionID != "" {
		s.parent = dead.SessionID
		if dead.Running {
			s.findings = slices.Concat(in.Findings, dead.Findings, []string{fmt.Sprintf(
				"session %s stopped after %d events without finishing; continue from the branch as it is", dead.SessionID, dead.Events)})
		}
	}
	hb := startHeartbeat(ctx, s.id)
	defer hb.stop()

	if err := r.completion.reconcile(ctx, t.ID); err != nil {
		return SessionResult{}, fmt.Errorf("revoke leftover credentials: %w", err)
	}
	cleanup := context.WithoutCancel(ctx)
	defer func() {
		r.forgetNamespace(cleanup, t.ID)
		if err := r.completion.reconcile(cleanup, t.ID); err != nil {
			log.Printf("task %s: revoke session %s credentials: %v", t.ID, s.id, err)
		}
	}()
	// least privilege (ruling 97): read on the task's repository and the
	// bot's fork for the clone init container (a resumed branch's head is
	// on the fork), nothing else on sf; the write token on the fork is
	// minted when the worker asks for it, its agent dead (answerPush)
	base, err := r.forgejo.MintToken(ctx, BaseTokenName(t.ID), "read:repository", []Repo{repo, r.forgejo.Fork(repo)})
	if err != nil {
		return SessionResult{}, err
	}
	// freshNamespace deletes a leftover namespace first, and with it any
	// pod a dead session left running
	if s.cache, err = r.setupNamespace(ctx, t, base); err != nil {
		return SessionResult{}, err
	}
	deadline := sessionDeadline(activity.GetInfo(ctx).Deadline, s.agentTime)
	startedAt := time.Now()
	began := rfc3339(startedAt)
	r.record(ctx, t.ID, "session_start", map[string]any{
		"session": s.id, "parent": s.parent, "model": s.model, "attempt": in.Attempt, "start_ref": s.startRef, "findings": s.findings, "answers": s.answers,
		"poster_info": s.posterInfo})
	out, err := r.runSession(ctx, t, r.agentNamespace(t.ID), s, deadline, hb.update, activity.GetWorkerStopChannel(ctx))
	if err != nil {
		outcome := "aborted"
		if errors.Is(err, errWorkerStop) {
			// the last heartbeat is what the retry resumes from
			outcome = "stopped"
			activity.RecordHeartbeat(ctx, hb.snapshot())
		}
		r.record(cleanup, t.ID, "session_end", map[string]any{"session": s.id, "parent": s.parent, "model": s.model, "attempt": in.Attempt,
			"outcome": outcome, "summary": err.Error(), "commit": out.commit, "started": began})
		return SessionResult{}, err
	}
	res := sessionResult(s, out)
	if res.Outcome != OutcomePushed {
		// no later session could spend: a human raises the cap or waits
		// for the month to end
		refused, err := r.store.MonthCapRefusal(ctx, t.ID, startedAt)
		if err != nil {
			return SessionResult{}, err
		}
		if refused != nil {
			res.Outcome, res.Reason = OutcomeEscalate, capRefused(*refused)
		}
	}
	r.record(ctx, t.ID, "session_end", map[string]any{"session": s.id, "parent": s.parent, "model": s.model, "attempt": in.Attempt,
		"outcome": res.Outcome, "summary": res.Summary, "started": began})
	if res.Outcome == OutcomePushed {
		r.record(ctx, t.ID, "pushed", map[string]any{"session": s.id, "branch": taskBranch(t), "commit": res.Commit})
		// a session that ended without its last word keeps the one before
		if out.agentText != "" {
			if err := r.store.SetAgentSummary(ctx, t.ID, textutil.Truncate(out.agentText, maxAgentSummary)); err != nil {
				return SessionResult{}, err
			}
		}
	}
	return res, nil
}

// startRef is where a session starts: the branch's head, else the given
// ref, else main's current head, which becomes the task's base.
func (r *Runner) startRef(ctx context.Context, t *Task, repo Repo, given string) (string, error) {
	fork := r.forgejo.Fork(repo)
	head, err := r.forgejo.BranchHeadSHA(ctx, fork.Owner, fork.Name, taskBranch(*t))
	switch {
	case err == nil:
		return head, nil
	case !errors.Is(err, ErrNotFound):
		return "", fmt.Errorf("branch head: %w", err)
	case given != "":
		return given, nil
	}
	main, err := r.forgejo.BranchHeadSHA(ctx, repo.Owner, repo.Name, prBase)
	if err != nil {
		return "", fmt.Errorf("main head: %w", err)
	}
	if main != t.SHA {
		if err := r.store.SetTaskSHA(ctx, t.ID, main); err != nil {
			return "", err
		}
		t.SHA = main
	}
	return main, nil
}

// sessionResult maps a session's end onto the outcome the workflow acts on.
func sessionResult(s sessionSpec, out sessionOutcome) SessionResult {
	res := SessionResult{SessionID: s.id, Outcome: OutcomeNoPush}
	// a session that finished and pushed goes to verification, even when a
	// stop raced its finish
	if out.result != nil && out.result.Success && out.commit != "" {
		res.Outcome, res.Commit, res.Summary = OutcomePushed, out.commit, out.result.Summary
		res.Findings = out.findings
		return res
	}
	// a session that finished with nothing new on top of the work in
	// progress an earlier stop pushed: that work is its result
	if out.result != nil && out.result.Success && out.commit == "" && s.startRef != "" && s.startRef != s.base {
		res.Outcome, res.Commit, res.Summary = OutcomePushed, s.startRef, out.result.Summary+"; the branch already held "+s.startRef
		res.Findings = out.findings
		return res
	}
	if out.trip != nil {
		res.Trip, res.Commit = string(out.trip.Kind), out.commit
		res.Findings = append(slices.Clone(out.trip.Findings), out.stopReport()...)
	}
	switch {
	case out.trip != nil && out.trip.Kind == TripWallClock:
		res.Outcome, res.Summary = OutcomeWallClock, "supervision trip: "+string(TripWallClock)
		return res
	case out.trip != nil && out.decision != nil:
		d := out.decision
		res.Summary = fmt.Sprintf("supervisor: %s after %s: %s", d.Action, out.trip.Kind, out.trip.Detail)
		if d.Guidance != "" {
			res.Findings = append(res.Findings, "supervisor guidance: "+d.Guidance)
		}
		switch {
		case out.trip.Kind == TripAgentInfoRequest:
			res.Outcome, res.Reason = OutcomeNeedsInfo, d.Reason
		case d.Action == ActionRedirect:
			res.Outcome = OutcomeRedirect
		case d.Action == ActionRetryStronger:
			res.Outcome = OutcomeRetryStronger
		default:
			res.Outcome, res.Choices = OutcomeEscalate, d.Choices
			res.Reason = fmt.Sprintf("%s\n\nDetector: %s (%s)\nProgress: %s", d.Reason, out.trip.Kind, out.trip.Detail, out.digest)
		}
		return res
	case out.result != nil:
		res.Summary = out.result.Summary
		if !out.result.Success && (strings.Contains(res.Summary, "DataInspectionFailed") || strings.Contains(res.Summary, "data_inspection_failed")) {
			// the same brief is refused the same way: only a human helps
			res.Outcome = OutcomeEscalate
			res.Reason = contentFilterRefusal + res.Summary + ". Close this card and file it again with the wording changed, or answer to try once more."
			return res
		}
	default:
		res.Summary = out.noResult
	}
	res.Findings = append(slices.Clone(out.findings), "previous session ended without a pushed commit: "+res.Summary)
	return res
}

// Escalate hands the task to a human (architecture 6.6): state
// waiting-human, mirrored on the issue, a comment with the reason and a
// question_asked row with its question; the agent's choices, if any, ride
// on that row, the timeline's step and the intake event the card reads. It
// returns the comment's id: only later comments answer it.
func (r *Runner) Escalate(ctx context.Context, taskID, reason string, choices []string) (int64, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return 0, err
	}
	if t.Status != "waiting-human" {
		// queued: the screening stage's suspect verdict, before any session;
		// waiting-deps: a dependency ended failed or closed; waiting-poster:
		// the author's answer screened suspect, or a cap reached on it
		if err := r.store.TransitionTask(ctx, taskID, []string{"queued", "waiting-deps", "running", "in-review", "waiting-poster"}, "waiting-human"); err != nil {
			return 0, terminalError("task %s is %s, cannot wait for a human: %v", taskID, t.Status, err)
		}
		mirrorTaskState(ctx, r.forgejo, t, "waiting-human")
	}
	// the reason in a code block: a mention the agent wrote rings no bell
	body := "factoryd: task " + taskID + " needs a human.\n\n" + fenced(r.untrusted(t, reason)) +
		"\n\nReply on this issue to start a new attempt with your comment as guidance, or close the issue to fail the task."
	if strings.HasPrefix(reason, screeningAsks) {
		// the reasons go to the card only: they would teach the author
		body = screenedLine
	}
	id, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, body)
	if err != nil {
		return 0, err
	}
	question := escalationQuestion(reason)
	if reason == healedReason {
		// the restart's sentence asks nothing: the row names the question it asks again
		events, err := r.store.IntakeEvents(ctx, taskID)
		if err != nil {
			log.Printf("task %s: the question a healed run asks again: %v", taskID, err)
		}
		_, again, _ := answersFrom(events)
		question = cmp.Or(again, question)
	}
	// a clicked choice is posted as the member's comment: agent text,
	// sanitised like the reason
	choices = slices.Clone(choices)
	for i, c := range choices {
		choices[i] = r.untrusted(t, c)
	}
	step := map[string]any{"reason": reason, "comment": id}
	asked := map[string]any{"question": textutil.Truncate(question, 300), "comment": id}
	if len(choices) > 0 {
		step["choices"], asked["choices"] = choices, choices
	}
	r.record(ctx, taskID, "escalated", step)
	// the durable anchor Reconcile rebuilds the answers from; a retry
	// with the same comment records nothing twice
	if _, err := r.store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: taskID, Kind: IntakeEscalation, Author: "factoryd", Body: reason, CommentID: id,
		Choices: choices}); err != nil {
		log.Printf("task %s: record escalation comment %d: %v", taskID, id, err)
	}
	r.timeline.RecordRow(ctx, taskID, EventQuestionAsked, FactoryDecider, ViaFactoryd, asked)
	if reason == healedReason || strings.HasPrefix(reason, screeningAsks) {
		return id, nil // the question asked again, or the screening's: its alert is open already
	}
	if _, err := r.store.InsertAlert(ctx, Alert{Kind: AlertEscalation, Source: escalationSource(reason), TaskID: taskID,
		Detail: map[string]any{"comment": id, "reason": textutil.Truncate(reason, 2000)}}); err != nil {
		log.Printf("task %s: record the escalation alert: %v", taskID, err)
	}
	return id, nil
}

// RequestInfo asks the issue's author for what the agent needs (ruling
// 150): state waiting-poster, mirrored on the issue, and a comment with the
// request that mentions the author and the project's notify list (the
// policy's approvers when it is empty), whose Forgejo bells ring; an
// info_request alert. It returns the comment's id: only later comments
// answer it.
func (r *Runner) RequestInfo(ctx context.Context, taskID, reason string) (int64, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return 0, err
	}
	if t.Status != "waiting-poster" {
		if err := r.store.TransitionTask(ctx, taskID, []string{"running", "in-review"}, "waiting-poster"); err != nil {
			return 0, terminalError("task %s is %s, cannot wait for the issue's author: %v", taskID, t.Status, err)
		}
		mirrorTaskState(ctx, r.forgejo, t, "waiting-poster")
	}
	notify := r.cfg.Policy().Document.Approvers
	if p, ok, err := r.store.ProjectByRepository(ctx, t.Repository); err != nil {
		return 0, err
	} else if ok {
		notify = p.NotifyOrApprovers(notify)
	}
	id, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, infoRequestComment(t, r.untrusted(t, reason), notify))
	if err != nil {
		return 0, err
	}
	r.record(ctx, taskID, "info_requested", map[string]any{"reason": reason, "comment": id, "notify": notify})
	// the durable anchor Reconcile rebuilds the wait from
	if _, err := r.store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: taskID, Kind: IntakeInfoRequest, Author: "factoryd", Body: reason, CommentID: id}); err != nil {
		log.Printf("task %s: record information request comment %d: %v", taskID, id, err)
	}
	if _, err := r.store.InsertAlert(ctx, Alert{Kind: AlertInfoRequest, Source: "agent", TaskID: taskID,
		Detail: map[string]any{"comment": id, "reason": textutil.Truncate(reason, 2000), "notify": notify}}); err != nil {
		log.Printf("task %s: record the info_request alert: %v", taskID, err)
	}
	return id, nil
}

// infoRequestComment is the information request on the issue: its author
// named, the request in a code block (a mention the agent wrote rings no
// bell), and every notified user mentioned once.
func infoRequestComment(t Task, reason string, notify []string) string {
	var mentions []string
	for _, u := range append([]string{t.Author}, notify...) {
		if m := "@" + u; u != "" && !slices.Contains(mentions, m) {
			mentions = append(mentions, m)
		}
	}
	return fmt.Sprintf("factoryd: task %s could not reproduce this issue and found no fault. It needs more information from the issue's author.\n\n%s\n\n"+
		"Reply on this issue with what is asked; the task resumes with your comment. Without an answer in 14 days the issue closes as not reproducible.\n\ncc %s",
		t.ID, fenced(strings.TrimPrefix(reason, agentAsksAuthor)), strings.Join(mentions, " "))
}

// CloseNotReproduced closes the task's issue, unanswered for 14 days, as
// not reproducible (ruling 150): a comment, then the close.
func (r *Runner) CloseNotReproduced(ctx context.Context, taskID string) error {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return err
	}
	if closed, err := r.forgejo.IssueClosed(ctx, repo.Owner, repo.Name, t.IssueNumber); err != nil || closed {
		return err
	}
	if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, "factoryd: "+notReproducedClose+
		". Closing this issue; a new issue with more detail starts a new task."); err != nil {
		return err
	}
	if err := recordFactoryClose(ctx, r.store, t); err != nil {
		return err
	}
	if err := r.forgejo.CloseIssue(ctx, repo.Owner, repo.Name, t.IssueNumber); err != nil {
		return err
	}
	r.record(ctx, taskID, "closed_not_reproduced", map[string]any{"issue": t.IssueNumber})
	return nil
}

// record appends to the timeline; a failed append is logged, never fails
// the step it describes.
func (r *Runner) record(ctx context.Context, taskID, step string, detail map[string]any) {
	if err := r.timeline.Append(ctx, taskID, step, detail); err != nil {
		log.Printf("task %s: timeline %s: %v", taskID, step, err)
	}
}

// Timeline is the runner's task timeline, which every writer of a task's
// timeline shares: its appends are serialized per task.
func (r *Runner) Timeline() *Timeline { return r.timeline }

// RecordStep appends a workflow-side step (a received signal) to the
// timeline.
func (r *Runner) RecordStep(ctx context.Context, taskID, step string, detail map[string]any) error {
	return r.timeline.Append(ctx, taskID, step, detail)
}

func earliest(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// sessionDeadline is a session's overall deadline: the earlier of
// activityDeadline (with sessionGrace to record the outcome after) and now
// plus agentTime, the task's resolved agent time (SessionInput.AgentTime,
// or the pinned policy's limits when the task carries none).
func sessionDeadline(activityDeadline time.Time, agentTime time.Duration) time.Time {
	return earliest(activityDeadline.Add(-sessionGrace), time.Now().Add(agentTime))
}

// OpenPullRequest opens the task's pull request from its branch on the
// bot's fork into main, as the bot, or reuses the one already open for the
// branch (the idempotency key: a retried or repeated call never opens a
// second). A commit whose .forgejo/, .gitea/ or .github/ differs from the
// task's base gets no pull request (rulings 90, 100): PR workflows run
// from the PR's own tree, so a task branch must never carry an edit to its
// own gate. If an earlier push already opened one, it is closed with the
// reason.
func (r *Runner) OpenPullRequest(ctx context.Context, taskID, commit string) (PullRequest, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return PullRequest{}, err
	}
	branch, fork := taskBranch(t), r.forgejo.Fork(repo)
	existing, err := r.forgejo.FindOpenPullRequestForBranch(ctx, repo.Owner, repo.Name, fork, branch)
	if err != nil {
		return PullRequest{}, err
	}
	if err := r.refuseWorkflowDirChange(ctx, t, repo, commit); err != nil {
		if existing != nil && isTerminal(err) {
			msg := "factoryd: closing this pull request: " + errMessage(err)
			if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, existing.Number, msg); err != nil {
				return PullRequest{}, err
			}
			if err := r.forgejo.ClosePullRequest(ctx, repo.Owner, repo.Name, existing.Number); err != nil {
				return PullRequest{}, err
			}
		}
		return PullRequest{}, err
	}
	var ref PullRequestRef
	if existing != nil {
		ref = *existing
	} else {
		objective := r.untrusted(t, t.Objective)
		body := fmt.Sprintf("%s\n\nCloses #%d\n\nOpened by factoryd for task %s.", objective, t.IssueNumber, t.ID)
		if ref, err = r.forgejo.CreatePullRequest(ctx, repo.Owner, repo.Name, fork.Owner+":"+branch, prBase, FirstLine(objective), body); err != nil {
			return PullRequest{}, err
		}
	}
	if err := r.store.SetTaskPR(ctx, taskID, ref.Number); err != nil {
		return PullRequest{}, err
	}
	r.record(ctx, taskID, "pr_opened", map[string]any{"number": ref.Number, "url": ref.URL, "commit": commit, "reused": existing != nil})
	return PullRequest{Number: ref.Number, URL: ref.URL}, nil
}

// refuseWorkflowDirChange returns a terminal error when any workflow
// directory's tree entry in commit (on the fork, where the branch was
// pushed) differs from the task base's (on the task's repository).
func (r *Runner) refuseWorkflowDirChange(ctx context.Context, t Task, repo Repo, commit string) error {
	base := t.SHA
	if base == "" {
		head, err := r.forgejo.BranchHeadSHA(ctx, repo.Owner, repo.Name, prBase)
		if err != nil {
			return err
		}
		base = head
	}
	was, err := r.forgejo.TreeEntries(ctx, repo.Owner, repo.Name, base, WorkflowDirs...)
	if err != nil {
		return err
	}
	fork := r.forgejo.Fork(repo)
	now, err := r.forgejo.TreeEntries(ctx, fork.Owner, fork.Name, commit, WorkflowDirs...)
	if err != nil {
		return err
	}
	for _, dir := range WorkflowDirs {
		if was[dir] != now[dir] {
			return terminalError("branch %s changes %s/ (commit %s against base %s); factoryd opens no pull request that edits a workflow directory", taskBranch(t), dir, commit, base)
		}
	}
	return nil
}

// TaskConfig reads the task's row and its process in the pinned version:
// the row's process, else (a row the loader's backfill has not reached)
// the process the task's project follows in that version, and the class
// of the row's lane. A report task runs its own class, the write-up's or
// the screening's, with no lane; a roll has no lane and runs ready's
// class, whose program is the lifecycle.
func (r *Runner) TaskConfig(ctx context.Context, taskID string, pin int64) (TaskConfig, error) {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return TaskConfig{}, err
	}
	v, err := r.pinnedVersion(ctx, pin)
	if err != nil {
		return TaskConfig{}, err
	}
	name := t.Process
	_, project, _ := v.ProjectOf(t.Repository)
	if name == "" {
		name = project.Process
	}
	p, ok := v.Processes[name]
	if !ok {
		return TaskConfig{}, terminalError("task %s: repository %s follows no process in configuration version %d", t.ID, t.Repository, v.ID)
	}
	tc := TaskConfig{Process: name, Lane: t.Lane, Stages: p.Stages, Queue: p.Merge.Queue, Dependencies: p.Dependencies}
	if plan, ok := p.Lane(policy.LanePlan); ok {
		tc.PlanShape = plan.Plan
	}
	switch lane, ok := p.Lane(cmp.Or(t.Lane, policy.LaneReady)); {
	case t.Report() && len(t.RequiredCapabilities) == 0:
		return TaskConfig{}, terminalError("task %s: a report task with no class", t.ID)
	case t.Report():
		tc.Class = t.RequiredCapabilities[0]
	case !ok:
		return TaskConfig{}, terminalError("task %s: process %s has no lane %s in configuration version %d", t.ID, name, t.Lane, v.ID)
	case lane.Class == "":
		return TaskConfig{}, terminalError("task %s: lane %s of process %s starts nothing", t.ID, t.Lane, name)
	default:
		tc.Class = lane.Class
	}
	tc.Session = v.Document.Classes[tc.Class].Session
	tc.Stronger = tc.Session == policy.SessionCode && project.Model == policy.ModelStronger
	tc.AgentTime, tc.CheckIn = v.Document.TimeOf(tc.Class, p, project)
	return tc, nil
}

// Reconcile reads where a task stands before a run acts, so that a run
// started by the heal pass or by a signal continues the task instead of
// redoing it: the row's status, the task's pull request (the recorded one,
// else the branch's open one) and whether it merged or closed, and whether
// the issue closed.
func (r *Runner) Reconcile(ctx context.Context, taskID string) (RunState, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return RunState{}, err
	}
	st := RunState{Status: t.Status, Usage: t.Usage, Standing: t.Standing(), Report: t.Report(), Stronger: t.Source == SourceScreening,
		Roll: t.Source == SourceRoll}
	if Final(t.Status) || t.Kind == "human" {
		st.Skip = true
		return st, nil
	}
	if st.Report {
		return st, nil
	}
	if st.WriteUp, err = r.writeUp(ctx, t.ID); err != nil {
		return RunState{}, err
	}
	if err := r.stageMemory(ctx, t.ID, &st); err != nil {
		return RunState{}, err
	}
	var pr *PullRequestRef
	if t.PRNumber != 0 {
		got, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, t.PRNumber)
		if err != nil {
			return RunState{}, err
		}
		pr = &got
	} else if pr, err = r.forgejo.FindPullRequestByHead(ctx, repo.Owner, repo.Name, prBase, r.forgejo.Fork(repo), taskBranch(t)); err != nil {
		return RunState{}, err
	}
	switch {
	case pr == nil:
	case pr.Merged:
		st.Merged = true
	case pr.State == "closed":
		st.PRClosed = true
	default:
		st.PR, st.Head = PullRequest{Number: pr.Number, URL: pr.URL}, pr.HeadSHA
		pending, err := r.store.UnaddressedChangeRequests(ctx, taskID)
		if err != nil {
			return RunState{}, err
		}
		for _, e := range pending {
			st.Findings = append(st.Findings, changeRequested(e.Author, e.Body))
		}
	}
	events, err := r.store.IntakeEvents(ctx, taskID)
	if err != nil {
		return RunState{}, err
	}
	if t.IssueNumber != 0 {
		closer, err := r.forgejo.IssueClosedBy(ctx, repo.Owner, repo.Name, t.IssueNumber)
		if errors.Is(err, errNoCloser) {
			log.Printf("task %s: issue #%d closed, its timeline names no closer: not counted", t.ID, t.IssueNumber)
		} else if err != nil {
			return RunState{}, err
		}
		// the bot account's close counts only where factoryd recorded it,
		// as on the webhook (review-task73 m8, m9)
		st.IssueClosed = closer != "" && (closer != r.forgejo.BotUser || botCloseRecorded(events, t))
		if closer == r.forgejo.BotUser && !st.IssueClosed {
			log.Printf("task %s: issue #%d closed by %s, which factoryd did not record as its own: not counted", t.ID, t.IssueNumber, closer)
		}
	}
	st.Answers, st.Question, st.Choices = answersFrom(events)
	st.Screening = waitsOnScreening(events)
	st.InfoRequest, st.InfoRequestedAt, st.PosterInfo, st.InfoAnswered = posterInfoFrom(events)
	r.record(ctx, taskID, "reconcile", map[string]any{"status": st.Status, "pr": st.PR.Number, "head": st.Head,
		"merged": st.Merged, "pr_closed": st.PRClosed, "issue_closed": st.IssueClosed, "skip": st.Skip, "findings": len(st.Findings),
		"answers": len(st.Answers)})
	return st, nil
}

// stageMemory rebuilds what the stages remembered from the task's
// timeline: the stage_findings steps (returns to the agent, the history),
// a dependencies_started step (the card started despite a dependency)
// and the model_review steps (the reviews), the last round.
func (r *Runner) stageMemory(ctx context.Context, taskID string, st *RunState) error {
	b, err := r.objects.Get(ctx, "sessions/"+taskID+"/timeline.jsonl")
	if err != nil {
		return fmt.Errorf("read the timeline: %w", err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		var step struct {
			Step, Stage, To, Verdict, Summary, Note, Reason string
			Round, Attempt                                  int
			Findings                                        []string
		}
		if json.Unmarshal([]byte(line), &step) != nil {
			continue
		}
		switch step.Step {
		case "stage_findings":
			for _, f := range step.Findings {
				st.History = append(st.History, historyLine(step.Stage, step.Round, step.Attempt, f))
			}
			st.Climbed = st.Climbed || step.To == "stronger"
			if step.To == "agent" {
				if st.Returns == nil {
					st.Returns = map[string]int{}
				}
				st.Returns[step.Stage]++
			}
			st.Round = max(st.Round, step.Round)
		case stepDepsStarted:
			st.DepStarted = step.Reason
		case "model_review":
			st.Reviews = append(st.Reviews, Review{Verdict: step.Verdict, Summary: step.Summary, Findings: step.Findings, Note: step.Note})
			st.Round = max(st.Round, step.Round)
		}
	}
	return nil
}

// waitsOnScreening: the task's last escalation, not counting a healed
// run's re-ask, is its screening's suspect verdict.
func waitsOnScreening(events []IntakeEvent) bool {
	last := ""
	for _, e := range events {
		if e.Kind == IntakeEscalation && e.Body != healedReason {
			last = e.Body
		}
	}
	return strings.HasPrefix(last, screeningAsks)
}

// answersFrom rebuilds the human's answers from a task's intake events
// (oldest first), newest first: every trusted issue comment newer than an
// escalation's comment, tagged with that escalation's question (a healed
// re-ask keeps the question it re-asks). question is the last one asked,
// choices the answers its agent offered.
func answersFrom(events []IntakeEvent) (answers []string, question string, choices []string) {
	var since int64
	for _, e := range events {
		switch {
		case e.Kind == IntakeEscalation:
			since = e.CommentID
			if e.Body != healedReason {
				question, choices = escalationQuestion(e.Body), e.Choices
			}
		case e.Kind == IntakeInfoRequest:
			since = 0 // what follows answers the information request
		case e.Kind == "issue_comment" && e.Trusted && since != 0 && e.CommentID > since:
			answers = append([]string{answerText(e.Author, question, e.Body)}, answers...)
		}
	}
	return answers, question, choices
}

// posterInfoFrom rebuilds the answers to the task's information requests
// from its intake events (oldest first): every comment of the issue's
// author, or a trusted one, newer than an information request's comment
// and before the next escalation; and the last request's comment and time.
func posterInfoFrom(events []IntakeEvent) (request int64, at time.Time, info []string, answered bool) {
	var since int64
	for _, e := range events {
		switch {
		case e.Kind == IntakeInfoRequest:
			since, request, at, answered = e.CommentID, e.CommentID, e.CreatedAt, false
		case e.Kind == IntakeEscalation:
			since = 0
		case since != 0 && e.CommentID > since && (e.Kind == IntakePosterComment || e.Kind == "issue_comment" && e.Trusted):
			info = append(info, posterText(e.Author, e.Body))
			answered = since == request
		}
	}
	return request, at, info, answered
}

// FirstLine is an objective's first line: the issue and pull request title.
func FirstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// WaitChecks polls the combined commit status of sha (Forgejo Actions is the
// first verification gate, architecture 7) until it settles or the checks
// timeout passes. Forgejo combines worst-state-wins, so a failed job
// settles the verdict while slower jobs still run.
func (r *Runner) WaitChecks(ctx context.Context, taskID, sha string) (ChecksResult, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return ChecksResult{}, err
	}
	deadline := earliest(activity.GetInfo(ctx).Deadline.Add(-time.Minute), time.Now().Add(r.cfg.ChecksTimeout))
	for {
		cs, err := r.forgejo.CombinedStatus(ctx, repo.Owner, repo.Name, sha)
		if err != nil {
			return ChecksResult{}, err
		}
		activity.RecordHeartbeat(ctx, cs.State)
		res := checksResult(cs, r.forgejo.PublicURL, func(target string) ([]byte, error) {
			// The bot collaborates on every project repository, so its
			// session can read any of their logs; a status could name one
			// outside this task's own run and pull another project's log
			// (M1, review-task115.md).
			prefix := "/" + repo.Owner + "/" + repo.Name + "/actions/runs/"
			if !strings.HasPrefix(target, prefix) {
				err := fmt.Errorf("target %q outside %s", target, prefix)
				log.Printf("task %s: job log %s: %v", t.ID, target, err)
				return nil, err
			}
			body, err := r.forgejo.JobLog(ctx, target)
			if err != nil {
				log.Printf("task %s: job log %s: %v", t.ID, target, err)
			}
			return body, err
		})
		if res.State == "" && !time.Now().Before(deadline) {
			res.State = ChecksTimeout
		}
		if res.State != "" {
			detail := map[string]any{"commit": sha, "state": res.State, "findings": res.Findings}
			if p, ok, err := r.store.ProjectByRepository(ctx, t.Repository); err != nil || !ok {
				log.Printf("task %s: ci jobs of %s: no project for %s (%v)", t.ID, sha, t.Repository, err)
			} else if jobs, err := CIJobsOf(ctx, r.forgejo, r.forgejo.Access(p).Actions, repo.Owner, repo.Name, sha, r.cfg.CIQueue); err != nil {
				log.Printf("task %s: ci jobs of %s: %v", t.ID, sha, err)
			} else {
				detail["jobs"] = jobs
			}
			r.record(ctx, t.ID, "checks", gateEnd(t, GateChecks, detail))
			return res, nil
		}
		select {
		case <-ctx.Done():
			return ChecksResult{}, ctx.Err()
		case <-time.After(min(r.cfg.ChecksPoll, time.Until(deadline))):
		}
	}
}

// BranchHead is the head of the task's branch on the fork when it holds
// commits past the task's base, "" when it is at the base or missing.
func (r *Runner) BranchHead(ctx context.Context, taskID string) (string, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return "", err
	}
	fork := r.forgejo.Fork(repo)
	head, err := r.forgejo.BranchHeadSHA(ctx, fork.Owner, fork.Name, taskBranch(t))
	switch {
	case errors.Is(err, ErrNotFound):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("branch head: %w", err)
	case head == t.SHA:
		return "", nil
	}
	return head, nil
}

// RerunChecks runs the checks of sha, the head of the task's branch, once
// more: an empty commit on top of it, which Forgejo's pull_request
// synchronize runs the workflows on (Forgejo 15 has no API to re-run a
// job). A branch that moved on (this activity's own earlier attempt; no
// session runs meanwhile) answers its head.
func (r *Runner) RerunChecks(ctx context.Context, taskID, sha string) (string, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return "", err
	}
	fork := r.forgejo.Fork(repo)
	head, err := r.forgejo.BranchHeadSHA(ctx, fork.Owner, fork.Name, taskBranch(t))
	if err != nil {
		return "", fmt.Errorf("branch head: %w", err)
	}
	if head == sha {
		if head, err = r.forgejo.CommitFiles(ctx, fork.Owner, fork.Name, taskBranch(t), "", "factoryd: run the checks of "+sha+" again", nil); err != nil {
			return "", fmt.Errorf("empty commit: %w", err)
		}
	}
	r.record(ctx, taskID, "checks_rerun", map[string]any{"commit": sha, "head": head})
	return head, nil
}

// checksResult maps a combined status onto a verdict; empty means still
// pending (no status reported yet counts as pending, not as success). For
// each failed status, logOf's answer (nil skips fetching entirely, an
// error just leaves the finding without a tail: a log factoryd cannot read
// must not sink the whole check) has its last 40 lines, at most 4 KiB,
// appended to the finding (backlog 85, design 4.4).
func checksResult(cs CombinedStatus, baseURL string, logOf func(targetURL string) ([]byte, error)) ChecksResult {
	if len(cs.Statuses) == 0 {
		return ChecksResult{}
	}
	switch cs.State {
	case "success", "warning":
		return ChecksResult{State: ChecksSuccess}
	case "failure", "error":
		res := ChecksResult{State: ChecksFailure}
		for _, s := range cs.Statuses {
			if s.State != "failure" && s.State != "error" {
				continue
			}
			target := s.TargetURL
			display := target
			if strings.HasPrefix(display, "/") {
				display = strings.TrimSuffix(baseURL, "/") + display
			}
			finding := fmt.Sprintf("check %q: %s — %s (%s)", s.Context, s.State, s.Description, display)
			if logOf != nil && target != "" {
				if log, err := logOf(target); err == nil {
					if tail := logTail(log); tail != "" {
						finding += "\n" + tail
					}
				}
			}
			res.Findings = append(res.Findings, finding)
		}
		return res
	}
	return ChecksResult{}
}

// jobLogMaxLines and jobLogMaxBytes bound the log tail a finding carries
// (design 4.4): enough to show the failure, never enough to flood a
// message.
const (
	jobLogMaxLines = 40
	jobLogMaxBytes = 4096
)

// logTail is log's last jobLogMaxLines lines, each with its leading
// Forgejo Actions timestamp stripped, cut down to at most jobLogMaxBytes by
// dropping whole lines from the front: the kept text always starts at a
// line, never mid-line. A single line past the byte cap on its own is cut
// hard at the nearest UTF-8 rune boundary, since no line boundary can
// satisfy it.
func logTail(log []byte) string {
	lines := strings.Split(strings.TrimRight(string(log), "\n"), "\n")
	if len(lines) > jobLogMaxLines {
		lines = lines[len(lines)-jobLogMaxLines:]
	}
	for i, line := range lines {
		lines[i] = stripLogTimestamp(line)
	}
	tail := strings.Join(lines, "\n")
	for len(tail) > jobLogMaxBytes {
		_, rest, ok := strings.Cut(tail, "\n")
		if !ok {
			cut := len(tail) - jobLogMaxBytes
			for cut < len(tail) && !utf8.RuneStart(tail[cut]) {
				cut++
			}
			return tail[cut:]
		}
		tail = rest
	}
	return tail
}

// stripLogTimestamp removes line's leading RFC3339Nano timestamp and
// space, Forgejo's raw Actions log format (e.g.
// "2026-09-30T22:50:22.9434409Z go test ./..."), which otherwise spends
// about 1.1 KiB of jobLogMaxBytes across a full 40-line tail on timestamps
// alone (M2, review-task115.md). A line without one is kept as-is.
func stripLogTimestamp(line string) string {
	ts, rest, ok := strings.Cut(line, " ")
	if !ok {
		return line
	}
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		return line
	}
	return rest
}

// PullRequestMerged tells a merge (which closes the issue through "Closes
// #N") from a plain close of the issue.
func (r *Runner) PullRequestMerged(ctx context.Context, taskID string, number int64) (bool, error) {
	_, repo, err := r.task(ctx, taskID)
	if err != nil {
		return false, err
	}
	pr, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, number)
	return pr.Merged, err
}

// Finish is the terminal step. A failed task's open pull request is closed
// first, with the reason; a done or closed task's branch on the fork is
// deleted. Both return a transient error, so a Forgejo blip retries them before the task
// turns final, while one Forgejo refuses is logged and passed: no retry
// fixes it, and failing here would have the heal pass restart the
// workflow every minute. Then completion (credentials revoked, keys blocked, final status,
// issue label) and, for a failure, the reason as an issue comment.
// Finishing an already-finished task is a no-op, so a retried Finish never
// double-completes.
func (r *Runner) Finish(ctx context.Context, taskID, final, reason string) error {
	t, ok, err := r.store.Task(ctx, taskID)
	if err != nil {
		return err
	}
	if !ok || Final(t.Status) {
		return nil // nothing to finish: gone, or already final
	}
	r.forgetNamespace(ctx, taskID)
	msg := "factoryd: task " + taskID + " failed: " + r.untrusted(t, reason)
	// a malformed repository still completes the task, only uncommented
	repo, repoErr := SplitRepo(t.Repository)
	if repoErr == nil {
		if final != "done" {
			if final == "closed" {
				msg = "factoryd: task " + taskID + " closed: " + r.untrusted(t, reason)
			}
			if err := cleanup(taskID, "close its pull request", r.closeTaskPullRequest(ctx, repo, t, msg)); err != nil {
				return err
			}
		}
		if final == "done" || final == "closed" {
			fork := r.forgejo.Fork(repo)
			if err := cleanup(taskID, "delete its branch", r.forgejo.DeleteBranch(ctx, fork.Owner, fork.Name, taskBranch(t))); err != nil {
				return err
			}
		}
	}
	if err := r.store.ResolveApprovals(ctx, taskID, "", "denied", FactoryDecider, "the task ended "+final+": "+reason); err != nil {
		return err
	}
	if err := r.leaveQueue(ctx, taskID, "the task ended "+final); err != nil {
		return err
	}
	if v := alertVerdict(final, reason); v != "" {
		if v == VerdictNoise && t.Source == SourceAlert && repoErr == nil {
			labels, err := r.forgejo.IssueLabels(ctx, repo.Owner, repo.Name, t.IssueNumber)
			if err := cleanup(taskID, "read its issue's labels", err); err != nil {
				return err
			}
			if slices.Contains(labels, LabelReal) {
				v = VerdictReal
			}
		}
		if err := r.store.JudgeTaskAlerts(ctx, taskID, AlertSigNoz, v, FactoryDecider); err != nil {
			return err
		}
	}
	for _, kind := range []string{AlertEscalation, AlertInfoRequest} {
		if err := r.store.JudgeTaskAlerts(ctx, taskID, kind, VerdictUnjudged, FactoryDecider); err != nil {
			return err
		}
	}
	if err := r.completion.Complete(ctx, taskID, final); err != nil {
		return err
	}
	if repoErr == nil && final == "failed" && t.IssueNumber != 0 {
		if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, msg); err != nil {
			log.Printf("task %s: comment failure reason: %v", taskID, err)
		}
	}
	r.record(ctx, taskID, "final", map[string]any{"state": final, "reason": reason})
	// after the final state, never ahead of an end that may not come; a
	// crash between the two loses only this record
	taskEnded(ctx, r.timeline, r.store, r.forgejo, r.cfg.Workflows, t, final)
	return nil
}

// cleanup passes a Finish cleanup step's Forgejo refusal, logged, and
// returns any other error for a retry.
func cleanup(taskID, step string, err error) error {
	if forgejoTerminal(err) {
		log.Printf("task %s: finish: could not %s, completing anyway: %v", taskID, step, err)
		return nil
	}
	return err
}

// closeTaskPullRequest closes the task's pull request while it is still
// open, then leaves msg as the reason: a failed task leaves no pull
// request behind. The comment follows the close so a retried close never
// repeats it, and a retry finds the pull request closed and posts nothing.
// A task whose number never got recorded (a crash between opening and
// recording) is found by its branch on the fork; a recorded one Forgejo
// no longer has leaves nothing to close.
func (r *Runner) closeTaskPullRequest(ctx context.Context, repo Repo, t Task, msg string) error {
	var pr *PullRequestRef
	if t.PRNumber != 0 {
		state, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, t.PRNumber)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		pr = &state
	} else {
		var err error
		if pr, err = r.forgejo.FindOpenPullRequestForBranch(ctx, repo.Owner, repo.Name, r.forgejo.Fork(repo), taskBranch(t)); err != nil {
			return err
		}
	}
	if pr == nil || pr.State != "open" {
		return nil
	}
	if err := r.forgejo.ClosePullRequest(ctx, repo.Owner, repo.Name, pr.Number); err != nil {
		return err
	}
	if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, pr.Number, msg); err != nil {
		log.Printf("task %s: comment failure reason on pull request #%d: %v", t.ID, pr.Number, err)
	}
	return nil
}
