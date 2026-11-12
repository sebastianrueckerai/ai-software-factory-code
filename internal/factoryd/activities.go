package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/temporal"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// prBase is the branch every task pull request targets.
const prBase = "main"

// WorkflowDirs are the top-level directories whose contents run as
// workflows on push; an agent branch must never change any of them.
// The worker refuses the push, OpenPullRequest re-checks.
var WorkflowDirs = []string{".forgejo", ".gitea", ".github"}

// WorkflowDir is the workflow directory name names in any case, and
// whether it names one: .FORGEJO is .forgejo to every guard, so a case
// variant beside the directory, or a rename into one, is a change to it.
func WorkflowDir(name string) (string, bool) {
	dir := strings.ToLower(name)
	return dir, slices.Contains(WorkflowDirs, dir)
}

// HeartbeatEvery is how often a long activity reports progress and the
// longest the worker holds a heartbeat back (MaxHeartbeatThrottleInterval):
// a workflow's cancel reaches a running activity in the answer to its next
// heartbeat, so within this. The heartbeat timeout is 2 minutes.
const HeartbeatEvery = 10 * time.Second

// contentFilterRefusal opens the escalation of a session the model provider
// refused with its content check: every next session sends the same brief and
// is refused the same way, so only a human changing the wording helps.
const contentFilterRefusal = "the model provider's content filter refused this session's model call (z.ai error 1301): "

// contentFiltered reports whether a failed session's summary carries z.ai's
// refusal of filtered content: business code 1301, HTTP 400, "System detected
// potentially unsafe or sensitive content in input or generation", which
// LiteLLM passes on inside the provider's error body.
func contentFiltered(summary string) bool {
	return strings.Contains(summary, "1301") && strings.Contains(summary, "unsafe or sensitive content")
}

// providerStatus is the status an error_upstream audit reason names when
// the provider is out: 429, 402 or 5xx as "upstream <code>",
// and 502 for a call the gateway could not make at all; 0 for any other.
func providerStatus(reason string) int {
	code, ok := strings.CutPrefix(reason, "upstream ")
	if !ok {
		return http.StatusBadGateway
	}
	switch n, _ := strconv.Atoi(code); {
	case n == http.StatusTooManyRequests, n == http.StatusPaymentRequired, n >= 500 && n < 600:
		return n
	}
	return 0
}

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
		ActivitySetGate:                     r.SetGate,
		ActivityRunSession:                  r.RunSession,
		ActivityOpenPullRequest:             r.OpenPullRequest,
		ActivityCopyUpstreamImages:          r.CopyUpstreamImages,
		ActivityWaitChecks:                  r.WaitChecks,
		ActivityHeadChecks:                  r.HeadChecks,
		ActivityRerunChecks:                 r.RerunChecks,
		ActivityBranchHead:                  r.BranchHead,
		ActivityPullRequestMerged:           r.PullRequestMerged,
		ActivityRecordStep:                  r.RecordStep,
		ActivityRecordNotRun:                r.RecordNotRun,
		ActivityFinish:                      r.Finish,
		ActivityReconcile:                   r.Reconcile,
		ActivityRecordUsage:                 r.RecordUsage,
		ActivityReadUsage:                   r.ReadUsage,
		ActivitySuccessor:                   r.Successor,
		ActivityGatePolicy:                  r.GatePolicy,
		ActivityGateMigration:               r.GateMigration,
		ActivityGateReproduction:            r.GateReproduction,
		ActivityGateCriteria:                r.GateCriteria,
		ActivityHoldoutSettlement:           r.SettleHoldouts,
		ActivityModelReview:                 r.ModelReview,
		ActivityRequestApproval:             r.RequestApproval,
		ActivityMerge:                       r.Merge,
		ActivityReportMergeFailure:          r.ReportMergeFailure,
		ActivityReportMergeRetriesExhausted: r.ReportMergeRetriesExhausted,
		ActivityUnjudged:                    r.Unjudged,
		ActivityHoldMerge:                   r.HoldMerge,
		ActivitySuperviseGate:               r.SuperviseGate,
		ActivityDropTrip:                    r.DropTrip,
		ActivityWithdrawQuestion:            r.WithdrawQuestion,
		ActivityAutoMergeNote:               r.AutoMergeNote,
		ActivityRequestInfo:                 r.RequestInfo,
		ActivityCloseNotReproduced:          r.CloseNotReproduced,
		ActivityScreen:                      r.Screen,
		ActivityJudgeScreening:              r.JudgeScreening,
		ActivityScreenAnswer:                r.ScreenAnswer,
		ActivitySizeCard:                    r.SizeCard,
		ActivityRunReport:                   r.RunReport,
		ActivityRollCommit:                  r.RollCommit,
		ActivityGateAudit:                   r.GateAudit,
		ActivityGateCompanion:               r.GateCompanion,
		ActivityTaskConfig:                  r.TaskConfig,
		ActivityDependencies:                r.Dependencies,
		ActivityEnterQueue:                  r.EnterQueue,
		ActivityAdmitResume:                 r.AdmitResume,
		ActivitySyncWithMain:                r.SyncWithMain,
		ActivityLeaveQueue:                  r.LeaveQueue,
		ActivityWithdrawApproval:            r.WithdrawApproval,
		ActivityRunPlan:                     r.RunPlan,
		ActivityRequestPlan:                 r.RequestPlan,
		ActivityOpenPlanQuestions:           r.OpenPlanQuestions,
		ActivityAwaitRollout:                r.AwaitRollout,
		ActivityProviderWait:                r.ProviderWait,
		ActivityAlertPreCheck:               r.AlertPreCheck,
		ActivitySessionFunded:               r.SessionFunded,
		ActivitySessionAnswered:             r.SessionAnswered,
		ActivityCapResume:                   r.CapResume,
		ActivityEndCase:                     r.EndCase,
		ActivityCaseState:                   r.CaseState,
		ActivityRecordGrant:                 r.RecordGrant,
		ActivityAuditDraw:                   r.AuditDraw,
		ActivityOpenTrip:                    r.openTrip,
		ActivityRoutineRun:                  r.RoutineRun,
		ActivityRoutineRunFailed:            r.RoutineRunFailed,
		ActivityParkForTool:                 r.ParkForTool,
		ActivityImageArrived:                r.ImageArrived,
		ActivityLeaveImagePark:              r.LeaveImagePark,
		ActivityBecomePlan:                  r.BecomePlan,
		ActivityReplanChild:                 r.ReplanChild,
		ActivitySupersedeApproval:           r.SupersedeApproval,
		ActivityReplanStopped:               r.ReplanStopped,
		ActivityProofsAtMain:                r.ProofsAtMain,
	} {
		w.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
}

// TerminalForgejoErrors is the worker interceptor that ends an activity's
// retries on a Forgejo refusal or on a Forgejo 404 that escaped it, and
// scrubs every activity error of Scrub's secrets before it leaves the
// worker: no retry fixes a 4xx, and the activities
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

// named prefixes err with the step of an activity it came from. Temporal
// reads an activity error's type and non-retryable flag off the returned
// error itself, never off one it wraps: a returned ApplicationError stays
// one, its message prefixed.
func named(step string, err error) error {
	if app, ok := err.(*temporal.ApplicationError); ok { //nolint:errorlint // the SDK's own test, on the error itself
		return temporal.NewApplicationErrorWithOptions(step+": "+app.Message(), app.Type(), temporal.ApplicationErrorOptions{
			NonRetryable:   app.NonRetryable(),
			Cause:          app.Unwrap(),
			NextRetryDelay: app.NextRetryDelay(),
			Category:       app.Category(),
		})
	}
	return fmt.Errorf("%s: %w", step, err)
}

// forgejoTerminal is a Forgejo answer no retry changes: a refusal or a 404.
func forgejoTerminal(err error) bool {
	return errors.Is(err, ErrRefused) || errors.Is(err, errForgejoNotFound)
}

// docRepo is the document repository of task t's project under the
// pinned policy version (chat.doc_repo); the zero Repo when it has none.
func (r *Runner) docRepo(ctx context.Context, t Task, pin int64) (Repo, error) {
	v, err := r.pinnedVersion(ctx, pin)
	if err != nil {
		return Repo{}, err
	}
	name, project, ok := v.ProjectOf(t.Repository)
	if !ok {
		return Repo{}, nil
	}
	doc := v.Document.ChatOf(project).DocRepo
	if doc == "" {
		return Repo{}, nil
	}
	repo, err := SplitRepo(doc)
	if err != nil {
		return Repo{}, fmt.Errorf("project %s chat.doc_repo: %w", name, err)
	}
	return repo, nil
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
// pull request is open), or a running task back to the wait a refused
// switch to the plan track woke it from, and mirrors the state onto the
// issue.
func (r *Runner) SetState(ctx context.Context, taskID, state string) error {
	from := map[string][]string{
		"running": {"queued", "waiting-deps", "running", "in-review", "waiting-human", "waiting-poster", "waiting-image"},
		// waiting-human: a human answered a gate's escalation and the gate runs
		// again; waiting-poster: a cap right after the author's answer
		// verifies the branch's head
		"in-review":      {"running", "in-review", "waiting-human", "waiting-poster"},
		"queued":         {"running"},
		"waiting-deps":   {"running"},
		"waiting-human":  {"running"},
		"waiting-poster": {"running"},
		"waiting-image":  {"running"},
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

// SetGate moves the task's row to gate since, for a gate the workflow
// enters after its activity's schedule: a gate beside the checks wait
// (the review, the audit), once its stage awaits it (activityBeside).
func (r *Runner) SetGate(ctx context.Context, taskID, gate string, since time.Time) error {
	return r.store.SetGate(ctx, taskID, gate, since)
}

// sessionProgress is RunSession's heartbeat: a retry after a crashed worker
// reads it to continue the dead session's chain.
type sessionProgress struct {
	SessionID string
	// Running is set once the worker pod runs: a retry after a setup or
	// pod-create failure has no dead session to continue.
	Running bool
	Events  int
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

func (h *heartbeat) update(events int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.p.Running, h.p.Events = true, events
}

func (h *heartbeat) snapshot() sessionProgress {
	h.mu.Lock()
	defer h.mu.Unlock()
	return sessionProgress{SessionID: h.p.SessionID, Running: h.p.Running, Events: h.p.Events}
}

// ReadUsage reads the caps the task's row holds: the answer wake reloads
// them, so a raise an answer made while the card waited (an extend)
// is not overwritten by the usage snapshot the last session recorded.
func (r *Runner) ReadUsage(ctx context.Context, taskID string) (TaskUsage, error) {
	t, _, err := r.store.Task(ctx, taskID)
	if err != nil {
		return TaskUsage{}, err
	}
	return t.Usage, nil
}

// SuccessorInput is the successor a card closes into: the card, the
// findings so far as the card's ask carried them (a packed block of
// lines) and the pull request whose patch it carries.
type SuccessorInput struct {
	TaskID   string
	Findings string
	PR       int64
}

// SourceSuccessor is a card a stuck card closed into, carrying the patch
// and the findings: it starts from main's tip, or from the old card's
// branch when that holds the old card's work over its line bound.
const SourceSuccessor = "successor"

// successorFindings heads the findings a successor's objective carries.
const successorFindings = "Findings so far:"

// continuesOverBoundWork: t is a successor in a line where a card already
// closed on work over its line bound, as factoryd's note, the first
// finding that close carried, says. One successor per line takes such
// work on.
func continuesOverBoundWork(t Task) bool {
	return t.Source == SourceSuccessor && (strings.Contains(t.Objective, successorFindings+"\n- "+overBoundNote) ||
		strings.Contains(t.Objective, successorFindings+"\n- "+overBoundUnpushed))
}

// overBoundAgain is the case of a successor of over-bound work whose own
// work ended over the bound again: its line count against the bound, where
// the work is (overBound, the result's note), and the cut a human makes.
func overBoundAgain(t Task, res SessionResult, overBound string) string {
	measure := "over the card's line bound"
	if m := overBoundMeasure.FindStringSubmatch(res.Summary); m != nil {
		over, _ := strconv.Atoi(m[1])
		bound, _ := strconv.Atoi(m[2])
		measure = fmt.Sprintf("%d changed lines against the card's %d-line bound", over+bound, bound)
	}
	where := "on " + taskBranch(t)
	if why, ok := strings.CutPrefix(overBound, overBoundUnpushed); ok {
		where = fmt.Sprintf("but the work is not on %s: it was not pushed (%s)", taskBranch(t), why)
		if res.Commit != "" {
			where += ", and the branch holds an earlier push at " + res.Commit
		}
	} else if res.Commit != "" {
		where += " at " + res.Commit
	}
	return fmt.Sprintf("This card continues work an earlier card left over its line bound, and its session ended over the bound again: %s, %s. "+
		"Cut the work into cards that each fit the bound; no further successor is filed.", measure, where)
}

// SuccessorID is the id of card id's successor, so a filing that stopped
// part way finds it.
func SuccessorID(id string) string {
	sum := sha256.Sum256([]byte("successor " + id))
	return "t-" + hex.EncodeToString(sum[:6])
}

// successorMarker marks the issue of card id's successor, so a filing that
// stopped between the issue and its row finds it again.
func successorMarker(id string) string {
	return fmt.Sprintf("<!-- factoryd successor of %s -->", id)
}

// Successor files the successor of the stuck card in (task 6): a card of
// the same objective with the findings so far, the carried pull request
// and the card's size and budget, queued on the direct track under the
// work bound. Its first session starts from main's tip, with no base of
// its own, unless the findings' first line, factoryd's note, says the old
// card's work over its line bound is an interim commit on its branch: then
// the successor continues that branch with the old card's base. The row
// lands first and its issue after, so a filing that stops part way runs
// again to the same one card: the row's identity holds the card and the
// marker holds the issue.
func (r *Runner) Successor(ctx context.Context, in SuccessorInput) (string, error) {
	old, repo, err := r.task(ctx, in.TaskID)
	if err != nil {
		return "", err
	}
	t, found, err := r.store.Task(ctx, SuccessorID(old.ID))
	if err != nil {
		return "", err
	}
	pr := cmp.Or(in.PR, old.PRNumber)
	if !found {
		p, ok, err := r.store.ProjectByRepository(ctx, old.Repository)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errNoTrack(p)
		}
		var lines []string
		if in.Findings != "" {
			lines = append(lines, successorFindings+in.Findings)
		}
		if pr > 0 {
			lines = append(lines, fmt.Sprintf("Carries: #%d", pr))
		}
		if old.Size != "" {
			lines = append(lines, "Size: "+old.Size)
		}
		lines = append(lines, fmt.Sprintf("Budget: %.2f EUR", old.BudgetEUR))
		// work an end push the line bound refused sits on the old card's
		// branch as an interim commit: the successor continues that branch,
		// as a plan card that goes on from another does, so its first session
		// starts at that head and its clone holds the work
		head, err := "", error(nil)
		if strings.HasPrefix(in.Findings, "\n- "+overBoundNote) {
			head, err = r.ContinueBranch(ctx, old, Task{ID: SuccessorID(old.ID)})
			if err != nil && !errors.Is(err, ErrNoBranch) {
				return "", err
			}
		}
		if t, err = fileEntry(p, Task{ID: SuccessorID(old.ID), Priority: old.Priority, RiskClass: old.RiskClass,
			BudgetEUR: old.BudgetEUR, Size: old.Size, Objective: old.Objective + "\n\n" + r.untrusted(old, strings.Join(lines, "\n")),
			Repository: old.Repository, Source: SourceSuccessor, Parent: old.Parent, PlanKey: old.PlanKey, Rung: old.Rung,
			Image: old.Image, Author: old.Author, AuthorWriter: old.AuthorWriter, CarryPR: pr}); err != nil {
			return "", err
		}
		// fileEntry gives a fresh intake card the project's process; the
		// successor continues the card it came from, like a retry does
		t.Process = old.Process
		if head != "" {
			t.SHA = old.SHA
		}
		if err := r.store.CreateTask(ctx, t); err != nil {
			return "", err
		}
	}
	if t.IssueNumber == 0 {
		marker := successorMarker(old.ID)
		all, err := r.forgejo.Issues(ctx, repo.Owner, repo.Name)
		if err != nil {
			return "", fmt.Errorf("the successor of %s: search the issues: %w", old.ID, err)
		}
		title, body, _ := strings.Cut(t.Objective, "\n\n")
		if i := slices.IndexFunc(all, func(is webhookIssue) bool {
			return is.Poster.UserName == r.forgejo.BotUser && strings.Contains(is.Body, marker)
		}); i >= 0 {
			t.IssueNumber = all[i].Index
		} else {
			ref, err := r.forgejo.CreateIssue(ctx, repo.Owner, repo.Name, title, body+"\n\n"+marker)
			if err != nil {
				return "", fmt.Errorf("the successor of %s: file its issue: %w", old.ID, err)
			}
			t.IssueNumber = ref.Number
		}
		if err := r.forgejo.ReplaceIssueLabels(ctx, repo.Owner, repo.Name, t.IssueNumber, []string{LabelAgent, "state/queued"}); err != nil {
			return "", fmt.Errorf("the successor of %s: label its issue #%d: %w", old.ID, t.IssueNumber, err)
		}
		if err := r.store.SetTaskIssue(ctx, t.ID, t.IssueNumber); err != nil {
			return "", fmt.Errorf("the successor of %s: link its issue #%d: %w", old.ID, t.IssueNumber, err)
		}
		r.timeline.recordEvent(ctx, t.ID, EventCardCreated, map[string]any{"by": r.forgejo.BotUser, "issue": t.IssueNumber, "successor_of": old.ID, "track": t.Track, "budget_eur": t.BudgetEUR})
	}
	if err := r.cfg.Workflows.Start(ctx, t.ID); err != nil {
		log.Printf("task %s: start the successor %s: %v", old.ID, t.ID, err)
	}
	return t.ID, nil
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
		process, track := processTrack(ctx, r.store, t)
		r.cfg.Metrics.Intervened(ctx, InterventionExtraTime, process, track)
	}
	return nil
}

// capRefusals open the card of a task whose attempt the day's, the
// month's or the project's month cap cannot fund (roadmap 243), by the
// refusal of the cap that bounds it: no raise of the task's budget
// answers them.
var capRefusals = map[string]string{
	policy.RefusedDayCap:     "the day cap cannot fund a call",
	policy.RefusedMonthCap:   "the month cap cannot fund a call",
	policy.RefusedProjectCap: "the project's month cap cannot fund a call",
}

// CapDecision is the decision of the cap whose attempt floor refusal why
// is: every cap resets on its own, so the console takes an answer, and the
// workflow waits on the cap (capWait).
func CapDecision(why string) (decision string, ok bool) {
	for decision, opening := range capRefusals {
		if strings.HasPrefix(why, opening) {
			return decision, true
		}
	}
	return "", false
}

// floorUnread opens the floor's refusal of a model it cannot price.
const floorUnread = "the attempt's floor on "

// floorRefused reports whether reason is an attempt floor's refusal for
// money: the budget's or a cap's.
func floorRefused(reason string) bool {
	for _, opening := range capRefusals {
		if strings.HasPrefix(reason, opening) {
			return true
		}
	}
	return BudgetRefusal(reason)
}

// openingRefused is the start failure of a fresh session the task's own
// money cannot open (SessionFunded), nil when it can or another refusal
// binds: at a cap the gateway refuses the session's first call, and the
// loop waits on the cap; a model the floor cannot price (floorUnread) is
// not about the task's money, and opens too.
func (r *Runner) openingRefused(ctx context.Context, taskID, model string) (*startFailure, error) {
	why, err := r.SessionFunded(ctx, taskID, model)
	if err != nil || !BudgetRefusal(why) {
		return nil, err
	}
	return &startFailure{step: stepBudget, text: why}, nil
}

// SessionAnswered reports whether the session of a task that began at in.Since
// got a model answer, and records the named start of one that did: a pod that
// started and made no answered call is no session. The ledger, not
// the timeline, is the fact the session cap counts. A session whose activity
// failed has no id: it counts, and no record names it.
func (r *Runner) SessionAnswered(ctx context.Context, in SessionAnswerInput) (bool, error) {
	answered, err := r.store.ModelAnswered(ctx, in.TaskID, in.Since)
	if err != nil || !answered {
		return false, err
	}
	if in.SessionID != "" {
		if err := r.startFact(ctx, in); err != nil {
			return false, err
		}
	}
	return true, nil
}

// startFact writes the named start's fact row beside its timeline line
// (roadmap 336): one row per card and session, whichever retry or
// reattachment records it — the identity keeps the row single, so the
// sessions panel's lifetime count stays stable. A failed write is the
// activity's error, not a log line: its retry writes the row instead of
// losing the start. The timeline line stays whatever the row does.
func (r *Runner) startFact(ctx context.Context, in SessionAnswerInput) error {
	fact := SessionStartFact{Task: in.TaskID, Session: in.SessionID, Model: in.Model, Attempt: in.Attempt}
	if r.timeline != nil {
		entry := map[string]any{"event": EventSessionStarted}
		maps.Copy(entry, fact.detail())
		if err := r.timeline.Append(ctx, in.TaskID, StepEvent, entry); err != nil {
			log.Printf("task %s: event %s: %v", in.TaskID, EventSessionStarted, err)
		}
	}
	t, ok, err := r.store.Task(ctx, in.TaskID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("task %s: no such task", in.TaskID)
	}
	return r.timeline.RecordFactRow(ctx, fact.Event(in.TaskID, t.Repository, t.IssueNumber, time.Now()))
}

// price is alias's price with its output limit (policy.Document.Limited).
func (r *Runner) price(ctx context.Context, alias string) (policy.Price, error) {
	p, err := r.cfg.Prices.Price(ctx, alias)
	if err != nil {
		return policy.Price{}, err
	}
	return r.cfg.Policy().Document.Limited(alias, p)
}

// SessionFunded is the floor of every session's start and of the console's
// answer (roadmap 243, plan A): "" when the free headroom can fund a
// session's opening calls on model and the policy's floor of its rung,
// else the refusal naming the amount; the console offers Fund on the
// budget's. The gateway's hold math itself decides — what the holds take
// (policy.Fit, policy.Opening.Hold) against the free task, day, month and
// project headroom the gateway grants against (policy.FitResource;
// outstanding holds netted, roadmap 243) under the caps of the version in
// force — so the floor is read here, never restated. A model that cannot
// be priced refuses the attempt: nothing could bound its calls. No model
// at all, a card with no alias whose class names no session_model, is not
// judged.
func (r *Runner) SessionFunded(ctx context.Context, taskID, model string) (string, error) {
	t, ok, err := r.store.Task(ctx, taskID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", terminalError("no task row for %s", taskID)
	}
	if r.cfg.Prices == nil {
		return "", nil
	}
	// the version in force, as the floors below read it; the session reads
	// its task's pinned one (reportSession), which differs only when a
	// commit changed the class's session_model since the task started
	v := r.cfg.Policy()
	model = sessionModel(v.Document, t, model)
	if model == "" {
		return "", nil
	}
	n, err := r.needOf(ctx, v, t, model, true)
	if err != nil || n.unread != "" {
		return n.unread, err
	}
	req, need := n.req, n.eur
	own, free, err := r.store.Free(ctx, req)
	if err != nil {
		return "", err
	}
	_, err = policy.FitResource(free, policy.HoldRequest{TaskID: taskID, Kind: policy.KindModel, Ref: req.Ref, Project: req.Project, EUR: need})
	if err == nil {
		return "", nil
	}
	var refused *policy.Refusal
	if !errors.As(err, &refused) {
		return "", err
	}

	// A session's figure is its ceiling, the reserve held out: it is
	// negative once the gates spent into the reserve, and the raise the
	// stop names (fundFor) covers that part too
	ceiling := policy.SessionHeadroom(own.Free(), req.ReserveEUR)
	left := math.Floor(ceiling*1000+1e-6) / 1000
	opens, state := budgetRefused, fmt.Sprintf("%.3f EUR of the %.3f EUR budget remains", max(left, 0), t.BudgetEUR)
	if free.Reserve > 0 {
		state = fmt.Sprintf("The card spent %.2f EUR of its %.2f EUR budget", own.Spent, t.BudgetEUR)
	}
	var fund string
	act := "Raise the budget (Fund on the board) and answer, or close the card."
	switch refused.Decision {
	case policy.RefusedDayCap:
		state = fmt.Sprintf("%.6f EUR of the day's %.2f EUR (budget.day_eur) is free", max(free.Day, 0), req.DayEUR)
		act = "Answer, and the attempt waits on its own for the UTC day's headroom, or raise budget.day_eur in policy.yaml by a commit."
	case policy.RefusedMonthCap:
		state = fmt.Sprintf("%.6f EUR of the month's %.2f EUR (budget.month_eur) is free", max(free.Month, 0), req.MonthEUR)
		act = "Raise budget.month_eur in policy.yaml by a commit and answer, or wait for the next UTC month and answer."
	case policy.RefusedProjectCap:
		file := "projects/" + req.Project + ".yaml"
		state = fmt.Sprintf("%.6f EUR of the project's %.2f EUR month (budget.month_eur in %s) is free", max(free.Project, 0), req.ProjectEUR, file)
		act = "Raise budget.month_eur in " + file + " by a commit and answer, or wait for the next UTC month and answer."
	default:
		if own.Holds > 0 {
			if free.Reserve > 0 {
				state += fmt.Sprintf("; %.3f EUR is held by calls in flight (%d, the earliest expiring %s)", own.Held, own.Holds, stamp(own.Expires))
			} else {
				state += fmt.Sprintf(" after outstanding holds of %.3f EUR (%d, the earliest expiring %s)", own.Held, own.Holds, stamp(own.Expires))
			}
			act = "Wait for the holds to settle and answer (a released hold books its call's cost, an expired one is booked in full), or raise the budget (Fund on the board) and answer."
		}
		if free.Reserve > 0 {
			// the money stop: a session that reached its ceiling says
			// what the card spent, what the review and the audit still
			// keep of the reserve the ceiling held out and what they
			// need, and what a fund that opens a session costs
			state += fmt.Sprintf("; %.2f EUR is held for the review and the audit, which need %.2f EUR", free.Kept(), free.Reserve)
			fund = fmt.Sprintf(", so a fund of %.2f EUR opens it", fundFor(need, ceiling))
		}
	}
	if o, ok := capRefusals[refused.Decision]; ok {
		opens = o
	}
	// the first line is the card's question row (escalationQuestion), cut
	// at 300 characters: it carries the actions, the state follows
	return fmt.Sprintf("%s: %s\n%s, and %s needs %.6f EUR (%s)%s; %s.", opens, act, state, n.call, need, n.measure, fund, n.then), nil
}

// attemptNeed is what an attempt of card t on model needs free to start,
// as the gateway's holds take it: the cheapest call, or with opening a
// session's opening calls as the newest recorded openings made them, and
// at least the floor of the model's rung. Beside the amount it keeps the
// hold request, the call it is for, how it was measured and what an
// attempt short of it meets. The amount is rounded up to the micro-euro,
// the grain the stop prints and every admission compares at. A price that cannot be read leaves unread
// the refusal that names it, and no amount.
type attemptNeed struct {
	req                         policy.HoldRequest
	eur                         float64
	call, measure, then, unread string
}

// needOf reads card t's attemptNeed on model under version v.
func (r *Runner) needOf(ctx context.Context, v policy.Version, t Task, model string, opening bool) (attemptNeed, error) {
	alias := aliasOf(model)
	price, err := r.price(ctx, alias)
	if err != nil {
		// a model nobody can price is a hold nobody can bound: the
		// attempt is refused (roadmap 222, item 5)
		return attemptNeed{unread: fmt.Sprintf(floorUnread+"%s cannot be read: %s", alias, r.Scrub.String(err.Error()))}, nil
	}
	project, projectEUR := v.ProjectCap(t.Repository)
	req := policy.HoldRequest{TaskID: t.ID, Kind: policy.KindModel, Ref: alias, Price: price,
		DayEUR: v.Document.Budget.DayEUR, MonthEUR: v.Document.Budget.MonthEUR, Project: project, ProjectEUR: projectEUR}
	if opening {
		// a session's floor holds the card's gate reserve out of the
		// headroom as the gateway holds its calls to
		// (Limits.GateReserve): the session ceiling it reads is
		// budget - spent - reserve, and a card spent to it meets the
		// money stop, which names the spent EUR and the EUR held for
		// the review and the audit; without opening the cheapest call
		// reads the full headroom and its first call alone: the gateway
		// holds its calls to the session ceiling all the same
		req.ReserveEUR = v.Document.Limits.GateReserve()
	}
	// the cheapest call: no prompt and policy.MinCallTokens output tokens
	inf, cheapest := math.Inf(1), req
	cheapest.WantOut = policy.MinCallTokens
	h, _ := policy.Fit(policy.Free{Task: inf, Day: inf, Month: inf, Project: inf}, cheapest)
	need, call, measure, then := h.EUR, "the cheapest call on "+model, fmt.Sprintf("%d output tokens at the hold machinery's minimum", policy.MinCallTokens),
		"an answer would start an attempt whose first call cannot run"
	if opening {
		o, err := r.store.Opening(ctx, t.ID, alias)
		if err != nil {
			return attemptNeed{}, err
		}
		call, then = "a session's opening on "+model, "a session started now could not make its opening calls"
		measure = "no session's opening is recorded: " + measure
		eur, calls, err := o.Hold(req, func(ref string) (policy.Price, error) { return r.price(ctx, ref) })
		if err != nil {
			return attemptNeed{unread: fmt.Sprintf(floorUnread+"%s cannot be read: %s", alias, r.Scrub.String(err.Error()))}, nil
		}
		if calls != nil {
			on := "it"
			if o.AnyRef {
				on = "any model, none on it being recorded"
			}
			asked := make([]string, len(calls))
			for i, c := range calls {
				asked[i] = fmt.Sprintf("%d bytes on %s asking %d output tokens", c.Bytes, c.Ref, c.WantOut)
				if c.WantOut == 0 {
					asked[i] = fmt.Sprintf("%d bytes on %s asking no output limit", c.Bytes, c.Ref)
				}
			}
			what := asked[0]
			if len(calls) > 1 {
				what = fmt.Sprintf("%d calls at once: %s", len(calls), strings.Join(asked, ", "))
			}
			need, measure = eur, fmt.Sprintf("%s; the largest of the newest %d sessions' openings on %s, as the holds take them", what, len(o.Sessions), on)
		}
		frontier := r.frontier(model)
		if floor := v.Document.Budget.SessionFloor(frontier); floor > need {
			key := "budget.session_floor_cheap_eur"
			if frontier {
				key = "budget.session_floor_frontier_eur"
			}
			need, call, measure, then = floor, "a session on "+model, "the floor of its rung, "+key+" in policy.yaml",
				"a session started now would run out of money before its work is done"
		}
	}
	return attemptNeed{req: req, eur: policy.CeilMicro(need), call: call, measure: measure, then: then}, nil
}

// CapCheck is a wait on the cap Decision refused: Turned says its period
// turned, RefusedAt is set by the check before an ask to the refusal it
// would ask about.
type CapCheck struct {
	TaskID, Model, Decision string
	Turned                  bool
	RefusedAt               time.Time `json:",omitzero"`
}

// CapResumed is a wait's verdict: Resume ends it, Settles is when the
// version in force binds at the gateway, if not yet.
type CapResumed struct {
	Resume  bool
	Settles time.Time `json:",omitzero"`
}

// CapResume reports whether a wait on a cap ends: at the period's turn, or
// once the cap no longer binds the session that runs next. The gateway may
// enforce the previous policy version for PolicyTTL after a load, so no
// wait ends before the version in force settles (Settles), nor on a
// refusal that came after it did: the cap as enforced refused it, and a
// floor that funds meanwhile would loop sessions with no wait. Past that,
// the session's start must be funded as SessionFunded reads it, every
// outstanding hold netted as the gateway nets it; the cap still refusing,
// or a model that cannot be priced, waits for the turn, and any other
// refusal ends the wait, the attempt's own floor asking its question. An
// end is on the timeline.
func (r *Runner) CapResume(ctx context.Context, in CapCheck) (CapResumed, error) {
	o, known := capOpenings[in.Decision]
	if !known {
		return CapResumed{}, terminalError("no cap is named %q", in.Decision)
	}
	why := "the UTC " + o.period + " turned"
	if !in.Turned {
		if r.cfg.Prices == nil {
			return CapResumed{}, nil
		}
		v := r.cfg.Policy()
		if settles := v.ActivatedAt.Add(gateway.PolicyTTL); r.clock().Before(settles) {
			return CapResumed{Settles: settles}, nil
		} else if !in.RefusedAt.IsZero() && !in.RefusedAt.Before(settles) {
			return CapResumed{}, nil
		}
		refusal, err := r.SessionFunded(ctx, in.TaskID, in.Model)
		if err != nil {
			return CapResumed{}, err
		}
		if strings.HasPrefix(refusal, floorUnread) {
			return CapResumed{}, nil
		}
		if d, _ := CapDecision(refusal); refusal != "" && d == in.Decision {
			return CapResumed{}, nil
		}
		why = fmt.Sprintf("the %s no longer binds a session (policy version %d)", o.name, v.ID)
		if refusal != "" {
			why = "the attempt's floor refuses otherwise: " + strings.SplitN(refusal, "\n", 2)[0]
		}
	}
	r.record(ctx, in.TaskID, "cap_resumed", map[string]any{"cap": in.Decision, "why": why})
	return CapResumed{Resume: true}, nil
}

// RunSession is one attempt: a fresh namespace with this session's own
// Forgejo token and service account (so the gateway mints this session its
// own LiteLLM key), one worker pod, supervision, and the session record.
// A retry after a crashed or stopped worker finds the dead session in the
// heartbeat, removes its namespace and pod with its credentials, and
// starts a new session carrying the dead one's findings, and its id as
// parent once it recorded an event.
// factoryd stopping ends the session gracefully first: the worker pushes
// its work in progress and the heartbeat carries its report.
func (r *Runner) RunSession(ctx context.Context, in SessionInput) (SessionResult, error) {
	t, repo, err := r.task(ctx, in.TaskID)
	if err != nil {
		return SessionResult{}, named("task", err)
	}
	info := activity.GetInfo(ctx)
	s := sessionSpec{id: newID("s-"), owner: r.ownerID(), activity: info.WorkflowExecution.RunID + "/" + info.ActivityID, parent: in.ParentSessionID, parentStop: in.ParentStop, parentOutcome: in.ParentOutcome, parentTrip: in.ParentTrip, parentDeadEnd: in.ParentDeadEnd,
		findings: in.Findings, answers: in.Answers, posterInfo: in.PosterInfo, model: in.Model, repo: repo, attempt: in.Attempt, checkInAsk: in.CheckInAsk,
		lastGates: in.LastGates, handover: in.Handover}
	if s.policy, err = r.pinned(ctx, in.PolicyVersion); err != nil {
		return SessionResult{}, named("policy", err)
	}
	// the project decides whether replan is offered and which companions
	// the session reads: a read error offers none and clones none, and the
	// session runs
	if project, _, err := r.store.ProjectByRepository(ctx, t.Repository); err != nil {
		log.Printf("task %s: read the project of %s, so replan is not offered: %v", t.ID, t.Repository, err)
	} else {
		_, s.planTrack = trackClass(project, policy.TrackPlan)
		s.companions = project.Companions
	}
	s.sessions = cmp.Or(in.Sessions, s.policy.Limits.Sessions)
	s.agentTime, s.checkIn = cmp.Or(in.AgentTime, s.policy.Limits.AgentTime), cmp.Or(in.CheckIn, s.policy.Limits.CheckIn)
	s.checkinExtra, s.remaining = in.CheckinExtra, in.Remaining
	if s.docs, err = r.docRepo(ctx, t, in.PolicyVersion); err != nil {
		return SessionResult{}, named("document repository", err)
	}
	plan, spec, role, dirs, err := r.planOf(ctx, t)
	if err != nil {
		return SessionResult{}, named("plan card", err)
	}
	s.spec, s.cardRole, s.directories = spec, role, dirs
	opened, err := r.openedProofs(ctx, t, plan)
	if err != nil {
		return SessionResult{}, named("opened proofs", err)
	}
	// the proofs are new files of the card's pull request: their lines
	// raise the card's line bound, set again by every session
	lines := 0
	for _, pp := range opened {
		s.proofs = append(s.proofs, harness.ProofFile{Path: pp.Path, Content: pp.Code})
		lines += codeLines(pp.Code)
	}
	if lines != t.Usage.ProofLines {
		if err := r.store.SetTaskProofLines(ctx, t.ID, lines); err != nil {
			return SessionResult{}, named("proof lines", err)
		}
		t.Usage.ProofLines = lines
	}
	// a carry card's pull request: its head reaches the clone as branch
	// pull/<n>, and the spec says so, because the sandbox holds no git
	// credential to fetch it mid-session
	if t.CarryPR != 0 {
		s.pull = t.CarryPR
		s.spec += fmt.Sprintf("\n\nThis card carries pull request #%[1]d: its head is in your clone as branch pull/%[1]d. The sandbox holds no git credential and cannot fetch it, so work from that branch.", t.CarryPR)
	}
	// a plan card's first session pins the revision it runs against
	// (piece 10); the merge refuses a card the plan has since passed
	if t.Source == SourcePlan && t.Parent != "" && t.PlanRevision == 0 {
		rev, ok, err := r.store.PlanRevision(ctx, t.Parent, 0)
		if err != nil {
			return SessionResult{}, named("plan revision", err)
		}
		if ok {
			if err := r.store.SetTaskPlanRevision(ctx, t.ID, rev.Revision); err != nil {
				return SessionResult{}, named("plan revision", err)
			}
			t.PlanRevision = rev.Revision
		}
	}
	if t.Source != SourcePlan && t.Standing() == harness.StandingWriter {
		_, body, _ := strings.Cut(t.Objective, "\n\n")
		s.directories = askedDirectories(body)
	}
	if err := r.forgejo.EnsureFork(ctx, repo); err != nil {
		return SessionResult{}, named("fork", err)
	}
	// a plan's work its first revert took off main is on its salvage
	// branch, which a card that lands it again applies from
	if t.Source == SourcePlan && t.Parent != "" {
		pair, err := r.salvagePair(ctx, repo, t.Parent)
		if err != nil {
			return SessionResult{}, named("salvage branch", err)
		}
		if pair != "" {
			s.branches = []string{pair}
		}
	}
	// the branch head, when the branch exists, is where every attempt
	// continues: the push of an earlier attempt, or of a session that died
	// after pushing, which a non-force push must build on
	if s.startRef, err = r.startRef(ctx, &t, repo, in.StartRef); err != nil {
		return SessionResult{}, named("start ref", err)
	}
	s.base = t.SHA
	var dead sessionProgress
	if activity.HasHeartbeatDetails(ctx) && activity.GetHeartbeatDetails(ctx, &dead) == nil && dead.SessionID != "" {
		if dead.Events > 0 {
			// the dead session left a record, which every event extends
			s.parent, s.parentOutcome, s.parentTrip, s.parentDeadEnd = dead.SessionID, "", "", false
			s.parentStop = fmt.Sprintf("factoryd's run of it ended without an outcome after at least %d events", dead.Events)
		}
		if dead.Running {
			s.findings = slices.Concat(in.Findings, []string{fmt.Sprintf(
				"session %s stopped after %d events without finishing; continue from the branch as it is", dead.SessionID, dead.Events)})
		}
	}
	// a session the stopped factoryd detached from runs on in its pod:
	// this activity's retry reattaches to it; a dead pod's row goes and the
	// attempt starts over from the branch; a pod that finished while
	// factoryd was away still holds the result line the old factoryd had
	// not read. Another activity's row, a session that ended without its
	// release or a pod whose delete is pending, is no session of this one:
	// it goes, and its pod with the fresh start's namespace, unread
	var live *LiveSession
	if l, ok, err := r.store.LiveSession(ctx, t.ID); err != nil {
		return SessionResult{}, named("live session", err)
	} else if ok {
		phase, _, perr := r.kube.PodStatus(ctx, l.Namespace, l.Pod)
		switch {
		case perr != nil && !errors.Is(perr, ErrNotFound):
			// an unknown status is no dead pod: nothing is deleted or
			// revoked, and the activity runs again
			return SessionResult{}, named("pod status", perr)
		case perr == nil && l.Activity == s.activity && (phase == "Running" || phase == "Pending" || phase == "Succeeded" || phase == "Failed"):
			// the takeover is a compare-and-set: a row another attempt took
			// since the read is theirs, and this attempt starts fresh
			if taken, err := r.store.TakeLiveSession(ctx, t.ID, l.Owner, s.owner); err != nil {
				return SessionResult{}, named("live session", err)
			} else if taken {
				live = &l
				s.id, s.parent, s.parentStop, s.parentOutcome, s.parentTrip, s.parentDeadEnd, s.findings = l.SessionID, in.ParentSessionID, in.ParentStop, in.ParentOutcome, in.ParentTrip, in.ParentDeadEnd, in.Findings
			}
		case perr == nil && l.Activity == s.activity:
			// the phase Unknown, or none: the node may be lost while the
			// pod runs, so it is no dead pod either
			return SessionResult{}, named("pod status", fmt.Errorf("pod %s is in phase %q", l.Pod, phase))
		default:
			if err := r.store.DeleteLiveSession(ctx, t.ID); err != nil {
				return SessionResult{}, named("live session", err)
			}
		}
	}
	hb := startHeartbeat(ctx, s.id)
	defer hb.stop()
	if s.startRef != s.base {
		if s.startWip, s.interimStart, err = r.interimStart(ctx, repo, s.startRef, s.base); err != nil {
			return SessionResult{}, named("start commit", err)
		}
	}

	if live == nil {
		if err := r.completion.reconcile(ctx, t.ID); err != nil {
			return SessionResult{}, named("revoke leftover credentials", err)
		}
	}
	cleanup := context.WithoutCancel(ctx)
	// detached: factoryd is stopping and the session runs on for the
	// retry; its sandbox and credentials stay
	detached, superseded := false, false
	defer func() {
		if detached || superseded {
			return
		}
		r.forgetNamespace(cleanup, t.ID)
		if err := r.completion.reconcile(cleanup, t.ID); err != nil {
			log.Printf("task %s: revoke session %s credentials: %v", t.ID, s.id, err)
		}
	}()
	var deadline, startedAt time.Time
	if live == nil {
		if f, err := r.openingRefused(ctx, t.ID, s.model); err != nil {
			return SessionResult{}, named("budget floor", err)
		} else if f != nil {
			res := sessionResult(s, sessionOutcome{start: f})
			res.Model = s.model
			return res, nil
		}
		// least privilege: read on the task's repository, the
		// bot's fork (a resumed branch's head is on the fork) and the
		// project's document repository for the clone init container,
		// nothing else on sf; the write token on the fork is minted when
		// the worker asks for it, its agent dead (answerPush)
		base, err := r.forgejo.MintToken(ctx, BaseTokenName(t.ID), "read:repository", s.readable(r.forgejo.Fork(repo)))
		if err != nil {
			return SessionResult{}, named("read token", err)
		}
		// freshNamespace deletes a leftover namespace first, and with it any
		// pod a dead session left running
		if s.cache, s.image, err = r.setupNamespace(ctx, &t, base); err != nil {
			return SessionResult{}, named("namespace", err)
		}
		deadline, startedAt = sessionDeadline(activity.GetInfo(ctx).Deadline, s.agentTime), time.Now()
		if s.checkinExtra > 0 {
			deadline = earliest(deadline, activity.GetInfo(ctx).StartedTime.Add(s.remaining))
			s.ceiling = activity.GetInfo(ctx).Deadline.Add(-sessionGrace)
		}
		r.record(ctx, t.ID, "session_start", map[string]any{
			"session": s.id, "parent": s.parent, "model": s.model, "attempt": in.Attempt, "start_ref": s.startRef, "findings": s.findings, "answers": s.answers,
			"poster_info": s.posterInfo})
	} else {
		deadline, startedAt = sessionDeadline(activity.GetInfo(ctx).Deadline, time.Until(live.Deadline)), live.StartedAt
		r.record(ctx, t.ID, "session_reattached", map[string]any{"session": s.id, "pod": live.Pod, "lines": live.Lines, "attempt": in.Attempt})
	}
	began := rfc3339(startedAt)
	out, err := r.runSession(ctx, t, r.agentNamespace(t.ID), s, deadline, hb.update, activity.GetWorkerStopChannel(ctx), live)
	superseded = out.superseded
	if out.superseded {
		// the newer attempt records the session's end: this one writes
		// nothing more
		return SessionResult{}, errSuperseded
	}
	if err != nil {
		if errors.Is(err, errWorkerStop) {
			// the row and the last heartbeat are what the retry resumes from
			detached = true
			activity.RecordHeartbeat(ctx, hb.snapshot())
			r.record(cleanup, t.ID, "session_detached", map[string]any{"session": s.id, "attempt": in.Attempt, "started": began})
			return SessionResult{}, err
		}
		r.record(cleanup, t.ID, "session_end", map[string]any{"session": s.id, "parent": s.parent, "model": s.model, "attempt": in.Attempt,
			"outcome": "aborted", "summary": err.Error(), "commit": out.commit, "started": began})
		return SessionResult{}, err
	}
	if out.model != "" {
		s.model = out.model
	}
	if out.overHold, err = r.store.OverHoldCall(ctx, t.ID, startedAt); err != nil {
		return SessionResult{}, err
	}
	res := sessionResult(s, out)
	res.Model, res.Started = s.model, startedAt
	if res.Outcome == OutcomeNoPush && out.commit != "" {
		// the card's budget stopped the session after its worker pushed the
		// work in progress: the branch goes to verification, no later
		// session of the card could pick the work up
		refused, err := r.store.BudgetRefused(ctx, t.ID, startedAt)
		if err != nil {
			return SessionResult{}, err
		}
		if refused {
			res.Outcome = OutcomePushed
		}
	}
	if res.Outcome == OutcomePushed {
		r.timeline.recordEvent(ctx, t.ID, EventSessionPushed, map[string]any{"session": s.id, "model": aliasOf(s.model), "attempt": in.Attempt,
			"minutes": math.Round(time.Since(startedAt).Minutes()*10) / 10})
	}
	if res.Outcome != OutcomePushed {
		// no later session could spend: the card waits on the cap
		// (awaitCap). A month's or a project's stays an escalation, whose
		// wait ends on its own as the day's does
		refused, err := r.store.CapRefusal(ctx, t.ID, startedAt)
		if err != nil {
			return SessionResult{}, err
		}
		switch {
		case refused == nil:
		case refused.Decision == policy.RefusedDayCap:
			res.Outcome, res.Capped = OutcomeDayCap, refused
		default:
			res.Outcome, res.Reason, res.Capped = OutcomeEscalate, capRefused(refused.Refusal), refused
		}
	}
	if res.Outcome == OutcomeNoPush {
		refused, err := r.providerRefusal(ctx, t.ID, startedAt, res.Summary)
		if err != nil {
			return SessionResult{}, err
		}
		if refused.Status != 0 {
			res.Outcome, res.Status, res.Reason = OutcomeProviderRefused, refused.Status, refused.Reason
		}
	}
	if res.Outcome == OutcomeNoPush && strings.Contains(res.Summary, pushOverBoundMark) && continuesOverBoundWork(t) {
		// the workflow closes an over-bound end into a successor: a line
		// that already passed such work on goes to a human instead
		res.Outcome, res.Reason = OutcomeEscalate, overBoundAgain(t, res, out.overBound)
	}
	events := r.sessionTranscript(ctx, t.ID, s.id)
	r.record(ctx, t.ID, "session_end", map[string]any{"session": s.id, "parent": s.parent, "model": s.model, "attempt": in.Attempt,
		"outcome": res.Outcome, "summary": res.Summary, "started": began})
	if res.Outcome != OutcomeNotStarted && !res.StartFailed {
		// the end's fact row beside its timeline line (roadmap 336): no
		// summary, which the line alone keeps; a session the worker never
		// started or could not start is no session to end. A failed write
		// is the session's error, not a log line: the retry runs a new
		// session whose own end is the fact the identity keeps single.
		ended := time.Now()
		end := SessionEndFact{Session: s.id, Attempt: in.Attempt, Outcome: string(res.Outcome), Started: startedAt}
		if err := r.timeline.RecordFactRow(ctx, end.Event(t.ID, t.Repository, t.IssueNumber, ended)); err != nil {
			return SessionResult{}, named("session end fact", err)
		}
		// where the session's time went, from its transcript; a transcript
		// that could not be read gives no row rather than a session of no
		// commands
		if events != nil {
			_, project, _ := r.cfg.Policy().ProjectOf(t.Repository)
			spent := SessionTimeOf(events, project.CommandClassOf())
			spent.Session, spent.Attempt, spent.Started = s.id, in.Attempt, startedAt
			if err := r.timeline.RecordFactRow(ctx, spent.Event(t.ID, t.Repository, t.IssueNumber, ended)); err != nil {
				return SessionResult{}, named("session time fact", err)
			}
		}
	}
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

// sessionTranscript is the session's recorded events; nil when none was
// kept or the read failed, which is logged: an unreadable transcript must
// not fail the session that wrote it.
func (r *Runner) sessionTranscript(ctx context.Context, taskID, session string) []harness.Event {
	if r.timeline == nil || r.timeline.Objects == nil {
		return nil
	}
	b, err := r.timeline.Objects.Get(ctx, "sessions/"+taskID+"/"+session+".jsonl")
	if err != nil {
		log.Printf("task %s: session %s's transcript: %v", taskID, session, err)
		return nil
	}
	var events []harness.Event
	for len(b) > 0 {
		var line []byte
		line, b, _ = bytes.Cut(b, []byte("\n"))
		var ev harness.Event
		if err := json.Unmarshal(line, &ev); err == nil {
			events = append(events, ev)
		}
	}
	return events
}

// providerRefusal is the provider's refusal of the session begun at since
// (Status 0: none), with the worker's message said, else the audit's.
func (r *Runner) providerRefusal(ctx context.Context, taskID string, since time.Time, said string) (ProviderWait, error) {
	reason, ok, err := r.store.ProviderRefusal(ctx, taskID, since)
	if err != nil || !ok || providerStatus(reason) == 0 {
		return ProviderWait{}, err
	}
	return ProviderWait{Status: providerStatus(reason), Reason: cmp.Or(said, reason)}, nil
}

// startRef is where a session starts: the branch's head, else the given
// ref, else the task's stamped base — main's head when the row carries no
// stamp yet, written once. A retry, or any later session start, reads the
// existing stamp, the fork point the first stamp recorded, instead of
// re-reading main's head: a main that moved during the session must not
// re-stamp a base the branch was not cut from.
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
	case t.SHA != "":
		return t.SHA, nil
	}
	main, err := r.forgejo.BranchHeadSHA(ctx, repo.Owner, repo.Name, prBase)
	if err != nil {
		return "", fmt.Errorf("main head: %w", err)
	}
	if err := r.store.SetTaskSHA(ctx, t.ID, main); err != nil {
		return "", err
	}
	t.SHA = main
	return main, nil
}

// maxWipWalk bounds how many interim commits interimStart follows.
const maxWipWalk = 32

// wipCommit reads sha on the fork: whether the interim push wrote it (its
// committer is wipEmail), and its first parent. The one predicate every
// path asks of a head: an interim commit is never judged, approved, opened
// as a pull request or merged as finished work.
func (r *Runner) wipCommit(ctx context.Context, repo Repo, sha string) (wip bool, parent string, err error) {
	fork := r.forgejo.Fork(repo)
	email, parent, err := r.forgejo.CommitCommitter(ctx, fork.Owner, fork.Name, sha)
	return email == wipEmail, parent, err
}

// refuseWip is the refusal of a gate asked to judge, approve or merge an
// interim commit.
func (r *Runner) refuseWip(ctx context.Context, repo Repo, sha string) error {
	if sha == "" {
		return nil
	}
	if wip, _, err := r.wipCommit(ctx, repo, sha); err != nil {
		return fmt.Errorf("read commit %s: %w", sha, err)
	} else if wip {
		return temporal.NewNonRetryableApplicationError(fmt.Sprintf("commit %s is an interim push's work in progress, not finished work: no gate judges, approves or merges it, and the session's end push lands the commit they judge", sha), errWipHead, nil)
	}
	return nil
}

// interimStart reads whether a session that starts on start has pushed
// nothing of its own: wip says start itself is an interim commit; interim
// says the interim commits stacked on it lead down to the card's base
// without a session's push between, so no push credit is lost. A walk past
// maxWipWalk commits counts as a push.
func (r *Runner) interimStart(ctx context.Context, repo Repo, start, base string) (wip, interim bool, err error) {
	head := start
	for i := 0; i < maxWipWalk && head != base && head != ""; i++ {
		isWip, parent, err := r.wipCommit(ctx, repo, head)
		if err != nil {
			return false, false, err
		}
		if i == 0 {
			wip = isWip
		}
		if !isWip {
			return wip, false, nil
		}
		head = parent
	}
	return wip, head == base, nil
}

// sessionResult maps a session's end onto the outcome the workflow acts on.
// Where the over-bound work went leads the findings on every path.
func sessionResult(s sessionSpec, out sessionOutcome) (res SessionResult) {
	defer func() {
		if out.overBound != "" {
			res.Findings = slices.Insert(res.Findings, 0, out.overBound)
		}
	}()
	res = SessionResult{SessionID: s.id, Outcome: OutcomeNoPush, CheckIn: out.checkIn, Killed: out.killed}
	if out.start != nil {
		// every next session would fail the same way: a human fixes the
		// cause, and the answer starts the next session
		res.Outcome, res.Summary, res.Reason, res.StartFailed = OutcomeEscalate, out.start.Error(), out.start.escalation(thenAnswer), true
		return res
	}
	// a session that finished and pushed goes to verification, even when a
	// stop or a check-in raced its finish
	if out.result != nil && out.result.Success && out.commit != "" {
		res.Outcome, res.Commit, res.Summary = OutcomePushed, out.commit, out.result.Summary
		return res
	}
	// a session that finished with nothing new on top of the work in
	// progress an earlier stop pushed: that work is its result
	if out.result != nil && out.result.Success && out.commit == "" && out.wip == "" && s.startRef != "" && s.startRef != s.base && !s.startWip {
		res.Outcome, res.Commit, res.Summary = OutcomePushed, s.startRef, out.result.Summary+"; the branch already held "+s.startRef
		return res
	}
	res.LastMessage = textutil.Truncate(out.agentText, maxHandoverMessage)
	if out.trip != nil {
		// the trip's commit is the session's own push, else the last
		// interim push of its work in progress
		res.Trip, res.Commit = string(out.trip.Kind), cmp.Or(out.commit, out.wip)
		res.Findings = out.stopReport()
		if out.trip.Stuck != "" {
			res.Findings = append([]string{out.trip.Stuck}, res.Findings...)
		}
	}
	// the check-in at the session's start stopped it: no conversation ran,
	// so no adapter verdict or trip can apply
	if out.checkIn != nil {
		switch out.checkIn.Decision {
		case CheckHuman:
			res.Outcome, res.Summary, res.Reason = OutcomeCheckinHuman, out.checkIn.Reason, out.checkIn.Reason
			return res
		case CheckDroppedAll:
			res.Outcome, res.Summary = OutcomeCheckinDropped, "the check-in dropped every finding"
			return res
		case CheckHandover:
			// a mid-session handover is not the escalate a stop would read;
			// a start-moment one falls through to the result's own shape
			if out.trip != nil {
				res.Summary = fmt.Sprintf("check-in: handover after %s", out.trip.Kind)
				return res
			}
		}
	}
	switch {
	case out.trip != nil && out.trip.Kind == TripWallClock:
		res.Outcome, res.Summary = OutcomeWallClock, "supervision trip: "+string(TripWallClock)
		return res
	case out.trip != nil && out.decision != nil:
		d := out.decision
		res.Summary = fmt.Sprintf("check-in: %s after %s: %s", d.Action, out.trip.Kind, out.trip.Detail)
		if d.Guidance != "" {
			res.Findings = append(res.Findings, "supervisor guidance: "+d.Guidance)
		}
		if out.trip.Kind == TripLoop {
			res.Findings = append(res.Findings, loopFinding(out.trip.Detail))
		}
		switch {
		case out.trip.Kind == TripAgentInfoRequest:
			res.Outcome, res.Reason = OutcomeNeedsInfo, d.Reason
		case out.trip.Kind == TripAgentToolRequest:
			res.Outcome, res.Reason, res.Tool, res.Need, res.Commit = OutcomeMissingTool, d.Reason, d.Tool, d.Need, cmp.Or(out.commit, out.wip)
		case d.Action == ActionRedirect:
			res.Outcome, res.DeadEnd = OutcomeRedirect, d.Judged
		case d.Action == ActionRetryStronger:
			res.Outcome, res.DeadEnd = OutcomeRetryStronger, d.Judged
		case d.Action == ActionReplan:
			res.Outcome, res.Reason, res.Replan = OutcomeReplan, d.Reason, replanText(out.trip, *d)
		default:
			res.Outcome, res.Choices = OutcomeEscalate, d.Choices
			res.Reason = fmt.Sprintf("%s\n\nDetector: %s (%s)\nProgress: %s", d.Reason, out.trip.Kind, out.trip.Detail, out.digest)
		}
		return res
	case out.result != nil:
		res.Summary = out.result.Summary
		if !out.result.Success && contentFiltered(res.Summary) {
			// the same brief is refused the same way: only a human helps
			res.Outcome = OutcomeEscalate
			res.Reason = contentFilterRefusal + res.Summary + ". Close this card and file it again with the wording changed, or answer to try once more."
			return res
		}
	default:
		res.Summary = out.noResult
	}
	if out.overHold != nil {
		res.Summary = strings.TrimPrefix(res.Summary+"; "+out.overHold.state(), "; ")
	}
	res.Commit = cmp.Or(out.commit, out.wip)
	if out.result != nil && out.result.Success && out.commit == "" && out.wip != "" && out.wip != s.startRef {
		// the end carries nothing past the start, yet an interim push put
		// work on the branch that the end no longer holds
		res.Summary += "; the branch still holds the interim push " + out.wip + ", which the session's final tree no longer carries"
	}
	ended := "previous session ended without a pushed commit: "
	if res.Commit != "" {
		ended = "previous session ended unfinished: "
	}
	res.Findings = []string{ended + res.Summary}
	if out.pending != nil {
		res.Outcome = OutcomeNotStarted
	}
	return res
}

// replanGuidanceMarker leads the check-in's guidance in a replan's text; the
// digest renders what follows it.
const replanGuidanceMarker = "\nIts guidance: "

// replanText is the planner's text of a replan: the check-in's names the
// trip it judged, with its reason and guidance, and a person's is their
// reason, each bounded at maxHandoverMessage. Two trips' details are
// quoted: the size trip's, factoryd's own line counts, and the agent's
// request_cut, the parts it proposes. That detail stands whole outside
// the bound, which would drop parts: its reason and parts are bounded
// already. Every other detail can quote the agent's calls.
func replanText(trip *Trip, d Decision) string {
	if trip.Kind == TripReplan {
		return textutil.Truncate(d.Reason, maxHandoverMessage)
	}
	judged := d.Reason
	if d.Guidance != "" {
		judged += replanGuidanceMarker + d.Guidance
	}
	after := string(trip.Kind) + " trip"
	switch trip.Kind {
	case TripSize:
		after += " (" + trip.Detail + ")"
	case TripAgentCut:
		return "The check-in decided replan after the " + after + " (" + trip.Detail + "): " + textutil.Truncate(judged, maxHandoverMessage)
	}
	return textutil.Truncate("The check-in decided replan after the "+after+": "+judged, maxHandoverMessage)
}

// OverHoldCall is a model call the gateway booked beyond its hold (COST
// OVER HOLD): its alias, its usage and output limit, its hold and its cost.
type OverHoldCall struct {
	Alias                                    string
	PromptTokens, CachedTokens, OutputTokens int
	OutputLimit                              int
	HoldEUR, CostEUR                         float64
}

// state names the call's tokens, hold and cost and the action that fits:
// an output that ran to its limit wants a shorter one; below it, the
// prompt cost more than its hold, and a smaller context fits.
func (c OverHoldCall) state() string {
	s := fmt.Sprintf("a model call on %s crossed its cost hold: %d prompt tokens (%d cached) and %d output tokens cost %.6f EUR against a hold of %.6f EUR; ",
		c.Alias, c.PromptTokens, c.CachedTokens, c.OutputTokens, c.CostEUR, c.HoldEUR)
	if c.OutputLimit > 0 && c.OutputTokens >= c.OutputLimit {
		return s + fmt.Sprintf("its output ran to the limit of %d tokens, so a shorter output fits: write the change in smaller pieces", c.OutputLimit)
	}
	return s + "its prompt cost more than the hold priced, so a smaller context fits: read fewer and smaller files"
}

// loopFinding is the next session's brief line on a loop: how the session
// ended and what it repeated, a fact.
func loopFinding(how string) string { return "the previous session ended in a loop after " + how }

// lastToolRefusal is the refusal the task's newest session ended on: the
// last tool call its record holds, when that call did not complete. The
// record is scrubbed where it was written, so the refusal quotes as it
// stands.
func (r *Runner) lastToolRefusal(ctx context.Context, taskID string) (tool, refusal string, ok bool) {
	sessions, err := r.store.RecentSessions(ctx, taskID, time.Now(), 1)
	if err != nil {
		log.Printf("task %s: read the sessions for the ask's refusal: %v", taskID, err)
		return "", "", false
	}
	if len(sessions) == 0 {
		return "", "", false
	}
	b, err := r.objects.Get(ctx, "sessions/"+taskID+"/"+sessions[0].ID+".jsonl")
	if err != nil || len(b) == 0 {
		return "", "", false
	}
	var last struct{ Tool, Status, Err string }
	for line := range strings.SplitSeq(string(b), "\n") {
		var ev struct {
			Kind   string         `json:"kind"`
			Detail map[string]any `json:"detail"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Kind != string(harness.EventToolCall) {
			continue
		}
		last.Tool, _ = ev.Detail["tool"].(string)
		last.Status, _ = ev.Detail["status"].(string)
		last.Err, _ = ev.Detail["error"].(string)
	}
	if last.Status == "completed" || last.Err == "" {
		return "", "", false
	}
	return last.Tool, last.Err, true
}

// RequestInfo asks the issue's author for what the agent needs:
// state waiting-poster, mirrored on the issue, and a comment with the
// request that mentions the author and the project's notify list (the
// project's approvers when it is empty), whose Forgejo bells ring; an
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
		pullQueued(ctx, r.store, r.cfg.Workflows, t.Repository)
	}
	notify := r.cfg.Policy().ApproversOf(t.Repository)
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
// not reproducible: a comment, then the close.
func (r *Runner) CloseNotReproduced(ctx context.Context, taskID string) error {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return err
	}
	if closed, err := r.forgejo.IssueClosed(ctx, repo.Owner, repo.Name, t.IssueNumber); err != nil || closed {
		return err
	}
	if err := closeIssueAsFactory(ctx, r.store, r.forgejo, t, "factoryd: "+notReproducedClose+
		". Closing this issue; a new issue with more detail starts a new task."); err != nil {
		return err
	}
	r.record(ctx, taskID, "closed_not_reproduced", map[string]any{"issue": t.IssueNumber})
	return nil
}

// record appends to the timeline; a failed append is logged, never fails
// the step it describes.
func (r *Runner) record(ctx context.Context, taskID, step string, detail map[string]any) {
	if r.timeline == nil {
		return
	}
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

// RecordNotRun writes the review's or the audit's did-not-run row (kind
// policy.StageReview or policy.StageAudit) when its activity failed for
// good, with the failure's reason: the row the monitor's
// review_refused_not_run reads, in the shape the run activity writes for
// a call that never started. The workflow calls it beside its own failure
// ask, under the queue-waits gate.
func (r *Runner) RecordNotRun(ctx context.Context, taskID, kind string, pr int64, commit string, round int, reason string) error {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return err
	}
	if kind == policy.StageAudit {
		res := AuditResult{Model: r.callModel(t, policy.PurposeAudit),
			Findings: []AuditFinding{{Item: auditNoVerdict, Why: reason}}}
		return r.auditFact(ctx, t, PullRequest{Number: pr}, commit, round, res)
	}
	fact := ReviewAttemptFact{PR: pr, Commit: commit, Round: round, Model: r.callModel(t, policy.PurposeReview), Note: reviewNotStarted + reason}
	return r.reviewFact(ctx, t, fact)
}

// ProviderWait records that the task waits w.Backoff on the model
// provider; its schedule puts the task in gate provider (activityGates).
func (r *Runner) ProviderWait(ctx context.Context, taskID string, w ProviderWait) error {
	return r.timeline.Append(ctx, taskID, StepProviderRefused, map[string]any{"status": w.Status, "reason": w.Reason, "backoff_s": int(w.Backoff.Seconds())})
}

// finishClosesIssue reports whether Finish closes the issue of card t as
// it ends final: a closed card's, and a done card's off the plan track
// unless it ends after its merge. A failed card's waits for its retry.
func finishClosesIssue(t Task, final string, afterMerge bool) bool {
	return final == "closed" || final == "done" && !afterMerge && t.Track != policy.TrackPlan
}

// mergedBefore reports whether a card's events hold its move to merged: a
// card that ends done after it finishes after its merge.
func mergedBefore(events []Event) bool {
	return slices.ContainsFunc(events, func(e Event) bool { return e.Kind == EventTaskStatus && e.Detail["status"] == "merged" })
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
// task's base gets no pull request: PR workflows run
// from the PR's own tree, so a task branch must never carry an edit to its
// own gate. If an earlier push already opened one, it is closed with the
// reason.
func (r *Runner) OpenPullRequest(ctx context.Context, taskID, commit string) (PullRequest, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return PullRequest{}, err
	}
	if err := r.refuseWip(ctx, repo, commit); err != nil {
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

// upstreamImagesPath is the list of upstream images the factory's store
// holds copies of.
const upstreamImagesPath = "upstream-images.txt"

// CopyUpstreamImages copies the image lines the task's branch head adds to
// upstream-images.txt, against its base's, into the store before the
// pull request's checks read it; its findings are the lines it could not
// copy. Without a copier it does nothing.
func (r *Runner) CopyUpstreamImages(ctx context.Context, taskID, commit string) ([]string, error) {
	if r.Upstream == nil {
		return nil, nil
	}
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return nil, err
	}
	base := t.SHA
	if base == "" {
		if base, err = r.forgejo.BranchHeadSHA(ctx, repo.Owner, repo.Name, prBase); err != nil {
			return nil, err
		}
	}
	holder := r.forgejo.Fork(repo)
	if t.PRNumber != 0 {
		st, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, t.PRNumber)
		if err != nil {
			return nil, err
		}
		if holder, err = SplitRepo(cmp.Or(st.HeadRepo, repo.String())); err != nil {
			return nil, err
		}
	}
	head, err := r.forgejo.FileAt(ctx, holder.Owner, holder.Name, commit, upstreamImagesPath)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	was, err := r.forgejo.FileAt(ctx, repo.Owner, repo.Name, base, upstreamImagesPath)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return r.Upstream.Copy(ctx, string(was), string(head))
}

// refuseWorkflowDirChange returns a terminal error when any workflow
// directory's tree entry in commit (on the fork, where the branch was
// pushed) differs from the task base's, main's current head's and their
// merge-base's (on the task's repository): a branch that merges main
// forward carries main's own workflow edits — the head's when it merged
// the newest main, the merge-base's when main has moved past the main it
// merged — and only an entry no main ever held is the task's own change.
func (r *Runner) refuseWorkflowDirChange(ctx context.Context, t Task, repo Repo, commit string) error {
	base := t.SHA
	mainHead, err := r.forgejo.BranchHeadSHA(ctx, repo.Owner, repo.Name, prBase)
	if err != nil {
		return err
	}
	if base == "" {
		base = mainHead
	}
	was, err := r.forgejo.WorkflowTreeEntries(ctx, repo.Owner, repo.Name, base)
	if err != nil {
		return err
	}
	var main map[string]string
	if mainHead != base {
		if main, err = r.forgejo.WorkflowTreeEntries(ctx, repo.Owner, repo.Name, mainHead); err != nil {
			return err
		}
	}
	fork := r.forgejo.Fork(repo)
	now, err := r.forgejo.WorkflowTreeEntries(ctx, fork.Owner, fork.Name, commit)
	if err != nil {
		return err
	}
	var merged map[string]string
	mergeBase := base
	for _, dir := range WorkflowDirs {
		if now[dir] == was[dir] || main != nil && now[dir] == main[dir] {
			continue
		}
		if merged == nil {
			if mergeBase, err = r.mergeBase(ctx, repo, fork, commit, base, mainHead); err != nil {
				return err
			}
			if merged, err = r.forgejo.WorkflowTreeEntries(ctx, repo.Owner, repo.Name, mergeBase); err != nil {
				return err
			}
		}
		if now[dir] == merged[dir] {
			continue
		}
		return terminalError("branch %s changes %s/ (commit %s against base %s, main %s and their merge-base %s); factoryd opens no pull request that edits a workflow directory",
			taskBranch(t), dir, commit, base, mainHead, mergeBase)
	}
	return nil
}

// mainChainDepth caps the walk down main's first-parent chain looking for
// the task base; mergeBaseSteps caps the walk over the pushed commit's own
// history, the branch's sessions' commits above the merge. Both walks find
// their target long before the cap in any real repository; past it the
// merge-base reads as the base and the gate compares as it did before the
// merge-base was added.
const (
	mainChainDepth = 512
	mergeBaseSteps = 128
)

// mergeBase is the merge-base of the pushed commit and main's head,
// whichever main the branch merged: the newest commit of main's
// first-parent chain, walked from the head towards the task base on the
// task's repository, that the pushed commit's history reaches on the
// fork. A branch that merged nothing reaches no chain commit above the
// base, so the base is its merge-base. The chain walk stops at the base —
// a main older than the task is not the branch's merge — and the fork
// walk stops at any chain commit it reaches, whose history is main's own.
func (r *Runner) mergeBase(ctx context.Context, repo, fork Repo, commit, base, mainHead string) (string, error) {
	if mainHead == base {
		return base, nil
	}
	chain := []string{mainHead}
	for chain[len(chain)-1] != base && len(chain) < mainChainDepth {
		parents, err := r.forgejo.CommitParents(ctx, repo.Owner, repo.Name, chain[len(chain)-1])
		if err != nil {
			return "", err
		}
		if len(parents) == 0 {
			break
		}
		chain = append(chain, parents[0])
	}
	at := make(map[string]int, len(chain))
	for i, sha := range chain {
		at[sha] = i
	}
	queue, seen, best := []string{commit}, map[string]bool{}, -1
	for len(queue) > 0 && len(seen) < mergeBaseSteps {
		cur := queue[0]
		queue = queue[1:]
		if cur == "" || seen[cur] {
			continue
		}
		seen[cur] = true
		if i, ok := at[cur]; ok {
			if best < 0 || i < best {
				best = i
			}
			continue
		}
		parents, err := r.forgejo.CommitParents(ctx, fork.Owner, fork.Name, cur)
		if err != nil {
			return "", err
		}
		queue = append(queue, parents...)
	}
	if best < 0 {
		return base, nil
	}
	return chain[best], nil
}

// TaskConfig reads the task's row and its process in the pinned version:
// the row's process, else the process the task's project follows in that
// version (a roll's, a factory pull request's card's), and the class of
// the row's track. A report task runs its own class, the screening's.
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
	tc := TaskConfig{Process: name, Track: t.Track, Stages: p.Stages, Queue: p.Merge.Queue, Dependencies: p.Dependencies}
	if plan, ok := p.Tracks.Track(policy.TrackPlan); ok {
		tc.PlanShape = plan.Plan
	}
	switch track, ok := p.Tracks.Track(t.Track); {
	case t.Report() && len(t.RequiredCapabilities) == 0:
		return TaskConfig{}, terminalError("task %s: a report task with no class", t.ID)
	case t.Report():
		tc.Class = t.RequiredCapabilities[0]
	case !ok:
		return TaskConfig{}, terminalError("task %s: process %s has no track %q in configuration version %d", t.ID, name, t.Track, v.ID)
	default:
		tc.Class = track.Class
	}
	tc.Session = v.Document.Classes[tc.Class].Session
	tc.Stronger = tc.Session == policy.SessionCode && (project.Model == policy.ModelStronger || t.Rung == RungFrontier)
	// stronger_after is 1 for a medium or large card (roadmap 222, item 5)
	if t.Size == "medium" || t.Size == "large" {
		tc.StrongerAfter = 1
	}
	tc.AgentTime, tc.CheckIn = v.Document.TimeOf(tc.Class, p, project)
	tc.ChecksBound, tc.SupervisorTimeout = v.ChecksBound(t.Repository), v.Document.Limits.SupervisorTimeout
	tc.NoAudit = !project.Audit
	tc.Sessions = SessionCap(t, 0)
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
	st := RunState{Status: t.Status, Usage: t.Usage, Size: t.Size, Standing: t.Standing(), Report: t.Report(), Stronger: t.Source == SourceScreening,
		Roll: t.Source == SourceRoll, FactoryPR: factoryPRSources(t.Source), Source: t.Source}
	if t.Status == "closing" {
		st.ClosingFinal, st.ClosingReason = t.ClosingFinal, t.ClosingReason
	}
	if Final(t.Status) || t.Kind == "human" {
		st.Skip = true
		return st, nil
	}
	if st.Report {
		cases, err := r.store.CasesOf(ctx, t.ID)
		if err != nil {
			return RunState{}, err
		}
		st.Answers = resolvedNotes(cases)
		// a stop whose case is still open is the question the run waits on
		open, err := r.store.NewestAskCase(ctx, t.ID)
		if err != nil {
			return RunState{}, err
		}
		if open.ID != 0 {
			reason, choices := faceOf(open)
			st.Question, st.Choices = escalationQuestion(reason), choices
			if d := capWaitDecision(reason); d != "" {
				// a cap's stop also ends at the turn of the cap's period
				st.Cap, st.CapUntil = reason, capTurn(d, open.OpenedAt)
			}
		}
		return st, nil
	}
	if !st.FactoryPR { // its object store is the card's session's, which never ran
		if err := r.stageMemory(ctx, t.ID, &st); err != nil {
			return RunState{}, err
		}
	}
	if !st.ScreeningPassed {
		if st.ScreeningPassed, err = r.store.ScreeningLetRun(ctx, t.ID); err != nil {
			return RunState{}, err
		}
	}
	var pr *PullRequestRef
	switch {
	case t.PRNumber != 0:
		got, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, t.PRNumber)
		if err != nil {
			return RunState{}, err
		}
		pr = &got
	case !st.FactoryPR: // a card of a factory pull request has the one on its row
		if pr, err = r.forgejo.FindPullRequestByHead(ctx, repo.Owner, repo.Name, prBase, r.forgejo.Fork(repo), taskBranch(t)); err != nil {
			return RunState{}, err
		}
	}
	switch {
	case pr == nil:
		if !st.FactoryPR && !st.Roll {
			if st.Head, err = r.branchHead(ctx, t, repo); err != nil {
				return RunState{}, err
			}
		}
	case pr.Merged:
		st.Merged = true
	case pr.State == "closed":
		st.PRClosed = true
	default:
		st.PR, st.Head = PullRequest{Number: pr.Number, URL: pr.URL}, pr.HeadSHA
		if st.Unfinished, _, err = r.wipCommit(ctx, repo, pr.HeadSHA); err != nil {
			return RunState{}, err
		}
		pending, err := r.store.UnaddressedChangeRequests(ctx, taskID)
		if err != nil {
			return RunState{}, err
		}
		for _, e := range pending {
			st.Findings = append(st.Findings, changeRequested(e.Author, e.Body, e.CreatedAt))
		}
	}
	events, err := r.store.IntakeEvents(ctx, taskID)
	if err != nil {
		return RunState{}, err
	}
	if t.IssueNumber != 0 {
		closer, at, err := r.forgejo.IssueClosedBy(ctx, repo.Owner, repo.Name, t.IssueNumber)
		if errors.Is(err, errNoCloser) {
			log.Printf("task %s: issue #%d closed, its timeline names no closer: not counted", t.ID, t.IssueNumber)
		} else if err != nil {
			return RunState{}, err
		}
		// a close counts as on the webhook: by a user who writes the
		// repository, or the bot where factoryd recorded it as its own
		if closer != "" && !untrustedCloseRecorded(events, at) {
			if st.IssueClosed, err = closeCounts(ctx, r.store, r.forgejo.BotUser, r.Writes, t, closer); err != nil {
				return RunState{}, err
			}
		}
	}
	asks, err := r.store.AskAnchors(ctx, taskID)
	if err != nil {
		return RunState{}, err
	}
	st.Answers, st.Question, st.Choices = answersFrom(events, asks)
	st.Cap, st.CapUntil = capWaitOf(asks)
	st.Screening = waitsOnScreening(asks)
	st.ScreeningAnswered = st.Screening && screeningAnswered(events, asks)
	st.InfoRequest, st.InfoRequestedAt, st.PosterInfo, st.InfoAnswered = posterInfoFrom(events, asks)
	if p, ok, err := r.store.ImagePark(ctx, taskID); err != nil {
		return RunState{}, err
	} else if ok {
		st.Park = &p
	}
	r.record(ctx, taskID, "reconcile", map[string]any{"status": st.Status, "pr": st.PR.Number, "head": st.Head,
		"merged": st.Merged, "pr_closed": st.PRClosed, "issue_closed": st.IssueClosed, "skip": st.Skip, "findings": len(st.Findings),
		"answers": len(st.Answers)})
	return st, nil
}

// stageMemory rebuilds what the stages remembered from the task's
// timeline: the stage_findings steps (returns to the agent, the history),
// a dependencies_started step (the card started despite a dependency),
// the model_review steps (the reviews), the last round, the checkin steps
// (the verdicts a check-in left on the reviews' findings and the priors
// its guards read, plan D), and the finding_adjudicated steps (the
// overrules an approval recorded on the reviews' findings).
func (r *Runner) stageMemory(ctx context.Context, taskID string, st *RunState) error {
	b, err := r.objects.Get(ctx, "sessions/"+taskID+"/timeline.jsonl")
	if err != nil {
		return fmt.Errorf("read the timeline: %w", err)
	}
	stageMemoryOf(b, st)
	return nil
}

// stageMemoryOf is stageMemory of the timeline tl.
func stageMemoryOf(tl []byte, st *RunState) {
	for line := range strings.SplitSeq(string(tl), "\n") {
		var step struct {
			Step, Stage, To, Verdict, Summary, Note, Reason string
			By, Finding, Why                                string
			Round, Attempt                                  int
			Answer                                          bool
			Findings                                        []string
			Verdicts                                        map[string]CheckVerdict
			Dropped                                         []Dropped
			Unsettled, Upheld                               []string
			Time                                            time.Time
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
		case "screening":
			st.ScreeningPassed = st.ScreeningPassed || !step.Answer && step.Verdict == ScreenClear
		case "model_review":
			st.Reviews = append(st.Reviews, Review{Verdict: step.Verdict, Summary: step.Summary, Findings: step.Findings, Note: step.Note,
				Dropped: step.Dropped, Unsettled: step.Unsettled, Upheld: step.Upheld})
			st.Round = max(st.Round, step.Round)
		case "checkin":
			if step.Round >= 1 && step.Round <= len(st.Reviews) {
				st.Reviews[step.Round-1].Verdicts = step.Verdicts
			}
			if len(step.Verdicts) > 0 {
				st.CheckinVerdicts = append(st.CheckinVerdicts, step.Verdicts)
			}
		case stepAdjudicated:
			for i := range st.Reviews {
				if !slices.Contains(st.Reviews[i].Findings, step.Finding) {
					continue
				}
				if st.Reviews[i].Adjudicated == nil {
					st.Reviews[i].Adjudicated = map[string]Adjudication{}
				}
				st.Reviews[i].Adjudicated[step.Finding] = Adjudication{By: step.By, Why: step.Why, When: step.Time}
			}
		}
	}
}

// waitsOnScreening: the task's last ask is its screening's suspect verdict.
func waitsOnScreening(asks []AskAnchor) bool {
	return len(asks) > 0 && strings.HasPrefix(asks[len(asks)-1].Body, screeningAsks)
}

// screeningAnswered: a person answered the task's last ask: a trusted
// comment answers one of its faces (answerOf).
func screeningAnswered(events []IntakeEvent, asks []AskAnchor) bool {
	if len(asks) == 0 {
		return false
	}
	anchors, _, _ := escalationAnchors(events, asks)
	asked := asks[len(asks)-1].CaseID
	first := slices.IndexFunc(asks, func(a AskAnchor) bool { return a.CaseID == asked })
	return slices.ContainsFunc(events, func(e IntakeEvent) bool {
		a := answerOf(anchors, e.CommentID)
		return e.Kind == "issue_comment" && e.Trusted && a != nil && a.comment >= asks[first].Comment
	})
}

// answersFrom rebuilds the human's answers from a task's intake events
// (oldest first) and its ask anchors: every trusted issue comment that
// answers an ask by the comments' ids (answerOf), tagged with that ask's
// question, in answerKey's order. question is the last one asked, choices
// the answers its agent offered. The live run tags its answers alike
// (awaitHuman, awaitHumanAgain).
func answersFrom(events []IntakeEvent, asks []AskAnchor) (answers []string, question string, choices []string) {
	anchors, question, choices := escalationAnchors(events, asks)
	type answer struct {
		key  answerKey
		text string
	}
	var got []answer
	for _, e := range events {
		if e.Kind != "issue_comment" || !e.Trusted {
			continue
		}
		if a := answerOf(anchors, e.CommentID); a != nil {
			got = append(got, answer{answerKey{escalation: a.comment, comment: e.CommentID}, answerText(e.Author, a.question, e.Body, e.CreatedAt)})
		}
	}
	slices.SortStableFunc(got, func(x, y answer) int { return x.key.compare(y.key) })
	for _, a := range got {
		answers = append(answers, a.text)
	}
	return answers, question, choices
}

// resolvedNotes rebuilds a report card's answers, which no issue comment
// carries, from its cases (oldest first): the note of each resolution that
// came with no comment, tagged with its stop's question, in the order the
// live run takes them (awaitAnswer). A withdrawal is no answer.
func resolvedNotes(cases []Case) (answers []string) {
	for _, c := range cases {
		res := c.Resolution
		if res == nil || c.ResolvedAt == nil || res.Action == ActWithdraw || strings.TrimSpace(res.Reason) == "" || res.Args["comment"] != nil {
			continue
		}
		answers = append(answers, answerText(res.By, escalationQuestion(askBody(c)), strings.TrimSpace(res.Reason), *c.ResolvedAt))
	}
	return answers
}

// escalationAnchors are the anchors of a task's comments: each face of an
// ask (the case's withdrawal ends them all) and each information request;
// question and choices are the last ask's.
func escalationAnchors(events []IntakeEvent, asks []AskAnchor) (anchors []anchor, question string, choices []string) {
	for _, a := range asks {
		question, choices = a.Question, a.Choices
		anchors = append(anchors, anchor{comment: a.Comment, question: a.Question, withdrawn: a.Withdrawn})
	}
	for _, e := range events {
		if e.Kind == IntakeInfoRequest {
			anchors = append(anchors, anchor{comment: e.CommentID, request: true})
		}
	}
	return anchors, question, choices
}

// anchor is a comment on the task's issue that later comments answer, by
// the comments' ids (answerOf): a face of an ask, which asked question, or
// an information request (request); withdrawn is the comment of the ask's
// withdrawal, 0 while none.
type anchor struct {
	comment   int64
	question  string
	request   bool
	withdrawn int64
}

// answerOf is the escalation of anchors that comment c answers, by the
// comments' ids alone, so a late webhook changes nothing: the newest
// anchor before c, unless that is an information request or was withdrawn
// before c; nil for none. Reconcile (answersFrom) and the live run
// (fileAnswer) read the human's answers by it.
func answerOf(anchors []anchor, c int64) *anchor {
	var a *anchor
	for i := range anchors {
		if anchors[i].comment < c && (a == nil || anchors[i].comment > a.comment) {
			a = &anchors[i]
		}
	}
	if a == nil || a.request || a.withdrawn != 0 && a.withdrawn < c {
		return nil
	}
	return a
}

// answerKey names an answer by its escalation's comment and its own: the
// human's answers go newest escalation first, each escalation's in the
// order they were written.
type answerKey struct{ escalation, comment, caseID int64 }

func (k answerKey) compare(o answerKey) int {
	return cmp.Or(cmp.Compare(o.escalation, k.escalation), cmp.Compare(k.comment, o.comment), cmp.Compare(k.caseID, o.caseID))
}

// posterInfoFrom rebuilds the answers to the task's information requests
// from its intake events (oldest first): every comment of the issue's
// author, or a trusted one, newer than an information request's comment
// and before the next face of an ask; and the last request's comment and
// time.
func posterInfoFrom(events []IntakeEvent, asks []AskAnchor) (request int64, at time.Time, info []string, answered bool) {
	var since int64
	for _, e := range events {
		switch {
		case e.Kind == IntakeInfoRequest:
			since, request, at, answered = e.CommentID, e.CommentID, e.CreatedAt, false
		case since != 0 && e.CommentID > since && (e.Kind == IntakePosterComment || e.Kind == "issue_comment" && e.Trusted):
			if slices.ContainsFunc(asks, func(a AskAnchor) bool { return a.Comment > since && a.Comment < e.CommentID }) {
				continue
			}
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
// settles the verdict while slower jobs still run, except that a job killed
// by a signal is re-run once first (killedChecks) and its re-run awaited.
func (r *Runner) WaitChecks(ctx context.Context, taskID, sha string) (ChecksResult, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return ChecksResult{}, err
	}
	if err := r.refuseWip(ctx, repo, sha); err != nil {
		return ChecksResult{}, err
	}
	deadline := activity.GetInfo(ctx).Deadline.Add(-r.checksMargin)
	var reran []rerunNote
	for {
		cs, err := r.forgejo.CombinedStatus(ctx, repo.Owner, repo.Name, sha)
		if err != nil {
			return ChecksResult{}, err
		}
		activity.RecordHeartbeat(ctx, cs.State)
		logOf := r.jobLogOf(ctx, t, repo)
		res := checksResult(cs, r.forgejo.PublicURL, logOf)
		if res.State == ChecksFailure {
			if unsettled, notes := r.killedChecks(ctx, t, repo, cs, logOf, &reran); unsettled {
				res.State = ""
			} else {
				res.Findings = append(res.Findings, notes...)
			}
		}
		if res.State == "" && !time.Now().Before(deadline) {
			res.State = ChecksTimeout
		}
		if res.State != "" {
			if res.State == ChecksFailure {
				r.annotateMainFailures(ctx, repo, &res)
			}
			detail := map[string]any{"commit": sha, "state": res.State, "findings": res.Findings}
			if len(reran) > 0 {
				detail["reran"] = reran
			}
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

// HeadChecks reads the staged head's combined commit status once, without
// the checks wait's polling: the read the approver does by hand, and the
// one the approval stage asks before its request. A
// red head answers its failing checks as WaitChecks reports them; one
// whose checks are still running, or that no check has reported on yet,
// answers pending with those checks as the findings, which the approval
// stage waits out within its bound.
func (r *Runner) HeadChecks(ctx context.Context, taskID, sha string) (ChecksResult, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return ChecksResult{}, err
	}
	cs, err := r.forgejo.CombinedStatus(ctx, repo.Owner, repo.Name, sha)
	if err != nil {
		return ChecksResult{}, err
	}
	res := checksResult(cs, r.forgejo.PublicURL, r.jobLogOf(ctx, t, repo))
	switch res.State {
	case ChecksFailure:
		r.annotateMainFailures(ctx, repo, &res)
	case ChecksSuccess:
	default:
		res = ChecksResult{State: ChecksPending, Findings: pendingCheckFindings(sha, cs)}
	}
	return res, nil
}

// jobLogOf is logOf for a head's failed checks: the tail of a check's job
// log, when its target is an actions run of the task's own repository.
// The bot collaborates on every project repository, so its session can
// read any of their logs; a status could name one outside this task's own
// run and pull another project's log (M1, review-task115.md).
func (r *Runner) jobLogOf(ctx context.Context, t Task, repo Repo) func(target string) ([]byte, error) {
	prefix := "/" + repo.Owner + "/" + repo.Name + "/actions/runs/"
	return func(target string) ([]byte, error) {
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
	}
}

// pendingCheckFindings name the checks that have not finished on sha: a
// combined state of pending, or none reported yet.
func pendingCheckFindings(sha string, cs CombinedStatus) []string {
	var out []string
	for _, s := range cs.Statuses {
		if s.State != "success" && s.State != "warning" && s.State != "skipped" {
			finding := fmt.Sprintf("check %q is still %s on %s", s.Context, cmp.Or(s.State, "pending"), sha)
			if s.Description != "" {
				finding += " (" + s.Description + ")"
			}
			out = append(out, finding)
		}
	}
	if len(out) == 0 && len(cs.Statuses) > 0 {
		out = append(out, fmt.Sprintf("the checks on %s are not settled (combined state %q)", sha, cs.State))
	}
	if len(out) == 0 {
		out = append(out, "no check has reported on "+sha+" yet")
	}
	return out
}

// BranchHead is the head of the task's branch on the fork when it holds
// finished work past the task's base, "" when it is at the base, missing or
// at an interim commit.
func (r *Runner) BranchHead(ctx context.Context, taskID string) (string, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return "", err
	}
	head, err := r.branchHead(ctx, t, repo)
	if err != nil || head == "" {
		return "", err
	}
	if wip, _, err := r.wipCommit(ctx, repo, head); err != nil || wip {
		return "", err
	}
	return head, nil
}

// workHead is the head of taskID's branch past the base, whoever pushed it:
// the work the card holds on its branch, interim work included.
func (r *Runner) workHead(ctx context.Context, taskID string) (string, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return "", err
	}
	return r.branchHead(ctx, t, repo)
}

// branchHead is the task's branch head past the base, whoever pushed it.
func (r *Runner) branchHead(ctx context.Context, t Task, repo Repo) (string, error) {
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
	f, fork, branch, err := r.headBranch(ctx, t, repo)
	if err != nil {
		return "", err
	}
	head, err := f.BranchHeadSHA(ctx, fork.Owner, fork.Name, branch)
	if err != nil {
		return "", fmt.Errorf("branch head: %w", err)
	}
	if head == sha {
		if head, err = f.CommitFiles(ctx, fork.Owner, fork.Name, branch, "",
			Attribution{Role: "checks", Task: t.ID, Human: t.Author}, "factoryd: run the checks of "+sha+" again", nil); err != nil {
			return "", fmt.Errorf("empty commit: %w", err)
		}
	}
	r.record(ctx, taskID, stepChecksRerun, map[string]any{"commit": sha, "head": head})
	return head, nil
}

// headBranch is where task t's head is written, as whom: its branch on the
// bot's fork, or for a factory pull request card (roadmap 264) its pull
// request's head, on the fork of the bot or of the projects user that
// opened it. A head on any other fork is no one's factoryd writes.
func (r *Runner) headBranch(ctx context.Context, t Task, repo Repo) (*Forgejo, Repo, string, error) {
	if !factoryPRSources(t.Source) {
		return r.forgejo, r.forgejo.Fork(repo), taskBranch(t), nil
	}
	pr, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, t.PRNumber)
	if err != nil {
		return nil, Repo{}, "", fmt.Errorf("pull request #%d: %w", t.PRNumber, err)
	}
	fork, err := SplitRepo(pr.HeadRepo)
	switch {
	case err != nil:
		return nil, Repo{}, "", terminalError("pull request #%d's head repository %q: %v", t.PRNumber, pr.HeadRepo, err)
	case fork.Owner == r.forgejo.BotUser:
		return r.forgejo, fork, pr.Head, nil
	case fork.Owner == r.cfg.ProjectsUser && r.cfg.ProjectsUser != "":
		return r.forgejo.As(r.cfg.ProjectsUser, r.forgejo.ProjectsToken), fork, pr.Head, nil
	}
	return nil, Repo{}, "", terminalError("pull request #%d's head is on %s, a fork factoryd does not write: its checks run again from Forgejo's run page", t.PRNumber, pr.HeadRepo)
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
	case "success", "warning", "skipped":
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
					if tail := logTail(log, jobLogMaxLines, jobLogMaxBytes); tail != "" {
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

// mainFailureSuffix opens the note a head finding carries when the same
// check fails on main too, closed with main's failing context and a
// parenthesis: the defect is pre-existing there, not this card's (Stage
// 19, finding 195).
const mainFailureSuffix = " (also failing on main: "

// annotateMainFailures reads main's state when a head run failed and marks
// each finding whose check also fails on main — the check, not the context
// name, for main does not run the pull request's workflow: a head reports
// pr / test (pull_request) where main reports ci / test (push) — so the
// agent's session and the supervisor's guidance point at the real defect
// instead of the card. A main run that has not reported the check counts
// as passing. Any error (no main branch, an unreadable status) logs and
// leaves the findings as they are: a pre-existing red must never sink the
// gate's verdict.
func (r *Runner) annotateMainFailures(ctx context.Context, repo Repo, res *ChecksResult) {
	mainSHA, err := r.forgejo.BranchHeadSHA(ctx, repo.Owner, repo.Name, prBase)
	if err != nil {
		log.Printf("checks gate: main head of %s/%s: %v", repo.Owner, repo.Name, err)
		return
	}
	if mainSHA == "" || slices.ContainsFunc(res.Findings, func(f string) bool { return strings.Contains(f, mainFailureSuffix) }) {
		// the status of sha itself answers when no sha is given; an empty
		// head would repeat it and mark the head's own failure as main's
		return
	}
	cs, err := r.forgejo.CombinedStatus(ctx, repo.Owner, repo.Name, mainSHA)
	if err != nil {
		log.Printf("checks gate: combined status of %s/%s at %s: %v", repo.Owner, repo.Name, mainSHA, err)
		return
	}
	failing := failingChecks(cs)
	if len(failing) == 0 {
		return
	}
	for i, finding := range res.Findings {
		if ctxName, _, ok := strings.Cut(strings.TrimPrefix(finding, `check "`), `"`); ok {
			if mainCtx, onMain := failing[checkOf(ctxName)]; onMain {
				res.Findings[i] += mainFailureSuffix + mainCtx + ")"
			}
		}
	}
}

// failingChecks maps the check of each context a combined status reports
// as failure or error to that context: main's failing context, which
// closes the note a head finding carries, for main's context names the
// push run, not the pull request's.
func failingChecks(cs CombinedStatus) map[string]string {
	by := map[string]string{}
	for _, s := range cs.Statuses {
		if s.State == "failure" || s.State == "error" {
			by[checkOf(s.Context)] = s.Context
		}
	}
	return by
}

// checkOf names the check a context runs, without the workflow before
// the separator or the event after it: pr / test (pull_request) and
// ci / test (push) both run test, which plain test already names.
func checkOf(context string) string {
	check := context
	if _, after, ok := strings.Cut(check, " / "); ok {
		check = after
	}
	if before, _, ok := strings.Cut(check, " ("); ok {
		check = before
	}
	return check
}

// jobLogMaxLines and jobLogMaxBytes bound the log tail a finding carries
// (design 4.4): enough to show the failure, never enough to flood a
// message.
const (
	jobLogMaxLines = 40
	jobLogMaxBytes = 4096
)

// logTail is log's last maxLines lines, each with its leading Forgejo
// Actions timestamp stripped, cut down to at most maxBytes by dropping
// whole lines from the front: the kept text always starts at a line, never
// mid-line. A single line past the byte cap on its own is cut hard at the
// nearest UTF-8 rune boundary, since no line boundary can satisfy it.
func logTail(log []byte, maxLines, maxBytes int) string {
	lines := strings.Split(strings.TrimRight(string(log), "\n"), "\n")
	if section := resultsSection(lines); len(section) > 0 {
		lines = section
	} else if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	for i, line := range lines {
		lines[i] = stripLogTimestamp(line)
	}
	tail := strings.Join(lines, "\n")
	for len(tail) > maxBytes {
		_, rest, ok := strings.Cut(tail, "\n")
		if !ok {
			cut := len(tail) - maxBytes
			for cut < len(tail) && !utf8.RuneStart(tail[cut]) {
				cut++
			}
			return tail[cut:]
		}
		tail = rest
	}
	return tail
}

// resultsSection is what a failed job's log says of its failure when it
// has a scanner's shape: the bare "Results:" line through the "Summary:"
// block its indented counts close. The per-file checking chatter before
// the section and the runner's post-amble after it never reach a finding.
// nil for a log without the shape, where the tail still speaks.
func resultsSection(lines []string) []string {
	start := slices.IndexFunc(lines, func(l string) bool { return strings.TrimSpace(l) == "Results:" })
	if start < 0 {
		return nil
	}
	end := len(lines) - 1
	if off := slices.IndexFunc(lines[start+1:], func(l string) bool { return strings.TrimSpace(l) == "Summary:" }); off >= 0 {
		end = start + 1 + off
		for j := end + 1; j < len(lines) && strings.HasPrefix(lines[j], "  "); j++ {
			end = j
		}
	}
	return lines[start : end+1]
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

// AlertPreCheck is an alert card's pre-check before its first model call
// (roadmap 181, Stage 18 item 3.12): it reads the task's alert fingerprint
// and its alerts row, the one record of whether the alert fires (kept true
// by the resolve deliveries, FluxPass and SweepSigNoz), and reports whether
// it still fires: no resolve after its start. An alert that no longer
// fires was a transient burst: the card closes itself without spending on
// a model call. An event (eventLabel) still fires: it happened, and its
// resolve only says the rule's window moved on. No fingerprint, or no row
// of this card, says it still fires: the check never stops a card on its
// own absence.
func (r *Runner) AlertPreCheck(ctx context.Context, taskID string) (bool, error) {
	t, ok, err := r.store.Task(ctx, taskID)
	if err != nil {
		return false, err
	}
	if !ok || t.AlertFingerprint == "" {
		return true, nil
	}
	row, seen, err := r.store.AlertByFingerprint(ctx, AlertSigNoz, t.AlertFingerprint)
	if err != nil {
		return false, err
	}
	if !seen || row.TaskID != taskID {
		return true, nil
	}
	_, firing := firingSince(row)
	return firing || row.Detail["event"] == true, nil
}

// Finish is the terminal step. A failed or closed task's open pull request
// is closed first, with the reason, and a closed or done task's issue
// still open too (a factory-PR card's pull request carries no "Closes
// #N"), but a plan card's, done each time a
// revision files its cards, and not again when the chain covers a merged
// card (an issue a person reopened since stays open); a done or closed
// task's branch on the fork is deleted. Both return a transient error, so a Forgejo blip retries them before the task
// turns final, while one Forgejo refuses is logged and passed: no retry
// fixes it, and failing here would have the heal pass restart the
// workflow every minute. Then completion (credentials revoked, keys blocked, final status,
// issue label) and, for a failure, the reason as an issue comment. The
// end of a plan's revert card ends its pause (revertEnded).
// Finishing an already-finished task, or a merged one, is a no-op, so a
// retried Finish never double-completes and never ends a merged card
// without its chain. A done end of a card whose pull request merged
// records its merge commit (cardMerged); one that waits for its chain
// (endsMerged) runs every step of done and lands in merged, not final and
// without task_ended: EndCovered ends it at live, CloseMerged at a
// human's close.
func (r *Runner) Finish(ctx context.Context, taskID, final, reason string) error {
	return r.finish(ctx, taskID, final, reason, false)
}

// EndCovered ends merged card taskID done: run, a green run of its
// chain, covers it. The chain poller's reconciler calls it every tick, so
// a card already done is a no-op.
func (r *Runner) EndCovered(ctx context.Context, taskID string, run int64) error {
	return r.finish(ctx, taskID, "done", fmt.Sprintf("live: covered by run %d", run), true)
}

// CloseMerged ends merged card taskID closed on closer's close of its
// issue or its card: its change merged but no chain ran it live, so the
// card is not done.
func (r *Runner) CloseMerged(ctx context.Context, taskID, closer string) error {
	return r.finish(ctx, taskID, "closed", "closed by "+closer+": ended without a live chain", true)
}

// finish is the one end of a task. afterMerge picks the cards it ends: a
// merged card for the ends after the merge, else one neither merged nor
// final.
func (r *Runner) finish(ctx context.Context, taskID, final, reason string, afterMerge bool) error {
	t, ok, err := r.store.Task(ctx, taskID)
	if err != nil {
		return err
	}
	if !ok || Final(t.Status) || (t.Status == "merged") != afterMerge {
		return nil // nothing to finish here: gone, already final, or not this entry's
	}
	// the end the run decided is stored before any Forgejo read or
	// cleanup, so a run restarted after one that kept failing finishes
	// with it; a merged card stays merged
	if !afterMerge {
		if err := r.store.CloseTask(ctx, taskID, closable, final, reason); err != nil {
			return err
		}
	}
	// a malformed repository still completes the task, only uncommented
	repo, repoErr := SplitRepo(t.Repository)
	state := final
	merged := afterMerge
	if !afterMerge && final != "failed" && repoErr == nil && t.PRNumber > 0 {
		if merged, err = r.cardMerged(ctx, repo, t); err != nil {
			return err
		}
	}
	if final == "done" && !afterMerge && merged && endsMerged(t, r.cfg.Policy().Tree) {
		state = "merged"
	}
	if err := r.store.DeleteLiveSession(ctx, taskID); err != nil {
		return err
	}
	r.forgetNamespace(ctx, taskID)
	why := r.untrusted(t, reason)
	msg := "factoryd: task " + taskID + " failed: " + why
	if repoErr == nil {
		if final != "done" {
			if final == "closed" {
				msg = "factoryd: task " + taskID + " closed: " + why
			}
			if err := cleanup(taskID, "close its pull request", r.closeTaskPullRequest(ctx, repo, t, msg)); err != nil {
				return err
			}
		}
		if finishClosesIssue(t, final, afterMerge) && t.IssueNumber != 0 {
			closing := msg
			switch {
			case final == "done" && merged:
				closing = fmt.Sprintf("factoryd: pull request #%d merged; this card ends with it.", t.PRNumber)
			case final == "done":
				closing = "factoryd: " + why + "; this card ends with it."
			}
			if err := cleanup(taskID, "close its issue", r.closeTaskIssue(ctx, repo, t, closing)); err != nil {
				return err
			}
		}
		if merged {
			fork := r.forgejo.Fork(repo)
			if err := cleanup(taskID, "delete its branch", r.forgejo.DeleteBranch(ctx, fork.Owner, fork.Name, taskBranch(t))); err != nil {
				return err
			}
		}
	}
	if err := r.store.ResolveApprovals(ctx, taskID, "", "denied", FactoryDecider, "the task ended "+state+": "+reason); err != nil {
		return err
	}
	if _, err := r.leaveQueue(ctx, taskID, "the task ended "+state); err != nil {
		return err
	}
	v := alertVerdict(final, reason)
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
	if err := r.store.JudgeOpenTaskAlerts(ctx, taskID, VerdictUnjudged, FactoryDecider); err != nil {
		return err
	}
	if err := r.completion.Complete(ctx, taskID, state, final, reason); err != nil {
		return err
	}
	if repoErr == nil && final == "failed" && t.IssueNumber != 0 {
		if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, msg); err != nil {
			log.Printf("task %s: comment failure reason: %v", taskID, err)
		}
	}
	r.record(ctx, taskID, "final", map[string]any{"state": state, "reason": reason})
	r.revertEnded(ctx, t, final, merged)
	if state == "merged" {
		return nil
	}
	// after the final state, never ahead of an end that may not come; a
	// crash between the two loses only this record
	taskEnded(ctx, r.timeline, r.store, r.forgejo, r.cfg.Workflows, t, final, why)
	if final == "failed" {
		// the end is done; a failed send leaves the impasse to the sweep
		if err := r.impasse(ctx, t); err != nil {
			log.Printf("task %s: impasse: %v", taskID, err)
		}
	}
	return nil
}

// endsMerged tells a card that waits for its chain once its pull request
// merged: one in a project of tree whose chain runs. A repository no
// project file names runs no chain. A roll is live when its own rollout
// says so, before it finishes.
func endsMerged(t Task, tree policy.Tree) bool {
	_, p, ok := tree.ProjectOf(t.Repository)
	return ok && t.PRNumber > 0 && t.Source != SourceRoll && p.Chain.Resolved().Mode == policy.ChainModeRun
}

// cardMerged tells whether card t's pull request merged: a recorded merge
// commit says so without a read, else Forgejo answers and its merge
// commit is recorded, whatever path merged the card, a roll a human
// merged included. An answer no retry changes (a refusal, a pull request
// Forgejo no longer has) is logged and reads as no merge, whichever end
// the card takes: its credentials are revoked, rather than failing its end
// every minute.
func (r *Runner) cardMerged(ctx context.Context, repo Repo, t Task) (bool, error) {
	if t.MergeCommit != "" {
		return true, nil
	}
	pr, err := r.forgejo.PullRequestState(ctx, repo.Owner, repo.Name, t.PRNumber)
	if forgejoTerminal(err) {
		log.Printf("task %s: finish: could not read pull request #%d, reading it as unmerged: %v", t.ID, t.PRNumber, err)
		return false, nil
	}
	if err != nil || !pr.Merged {
		return false, err
	}
	return true, r.store.SetTaskMergeCommit(ctx, t.ID, pr.MergeCommit)
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

// closeTaskIssue closes an ending task's issue while it is still open, as
// factoryd's recorded close with msg as the reason, as
// closeTaskPullRequest does on the pull request: the card a change request
// closed reads closed on its issue as on the
// board, and so does a done card whose end closed no issue. An issue a
// human or the merge's "Closes #N" closed is left.
func (r *Runner) closeTaskIssue(ctx context.Context, repo Repo, t Task, msg string) error {
	closed, err := r.forgejo.IssueClosed(ctx, repo.Owner, repo.Name, t.IssueNumber)
	if err != nil || closed {
		return err
	}
	return closeIssueAsFactory(ctx, r.store, r.forgejo, t, msg)
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
