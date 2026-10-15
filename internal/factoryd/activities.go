package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// prBase is the branch every task pull request targets.
const prBase = "main"

// WorkflowDirs are the top-level directories whose contents run as
// workflows on push; an agent branch must never change any of them
// (ruling 100). The worker refuses the push, OpenPullRequest re-checks.
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
const contentFilterRefusal = "the model provider's content filter refused this session's model call (DataInspectionFailed): "

// providerStatus is the status an error_upstream audit reason names when
// the provider is out (ruling 267): 429, 402 or 5xx as "upstream <code>",
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
		ActivityWaitChecks:                  r.WaitChecks,
		ActivityHeadChecks:                  r.HeadChecks,
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
		ActivityDropTrip:                    r.DropTrip,
		ActivityWithdrawQuestion:            r.WithdrawQuestion,
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
		ActivityWithdrawApproval:            r.WithdrawApproval,
		ActivityRunPlan:                     r.RunPlan,
		ActivityRequestPlan:                 r.RequestPlan,
		ActivityCreateChildren:              r.CreateChildren,
		ActivityOpenPlanQuestions:           r.OpenPlanQuestions,
		ActivityAwaitSessionGap:             r.AwaitSessionGap,
		ActivityAwaitRollHold:               r.AwaitRollHold,
		ActivityClearRollHold:               r.ClearRollHold,
		ActivityProviderWait:                r.ProviderWait,
		ActivityAlertPreCheck:               r.AlertPreCheck,
		ActivityAttemptFunded:               r.AttemptFunded,
		ActivitySessionFunded:               r.SessionFunded,
		ActivitySessionAnswered:             r.SessionAnswered,
		ActivityDayResume:                   r.DayResume,
		ActivityRunSplit:                    r.RunSplit,
		ActivityFileSplit:                   r.FileSplit,
		ActivityRoutineRun:                  r.RoutineRun,
		ActivityRoutineRunFailed:            r.RoutineRunFailed,
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

// capRefusals open the card of a task whose attempt the day's, the
// month's or the project's month cap cannot fund (roadmap 243), by the
// refusal of the cap that bounds it: no raise of the task's budget
// answers them.
var capRefusals = map[string]string{
	policy.RefusedDayCap:     "the day cap cannot fund a call",
	policy.RefusedMonthCap:   "the month cap cannot fund a call",
	policy.RefusedProjectCap: "the project's month cap cannot fund a call",
}

// DayCapped reports whether why is an attempt floor's refusal at the day
// cap, which resets on its own: the workflow waits for the day (awaitDay),
// and the console takes an answer.
func DayCapped(why string) bool { return strings.HasPrefix(why, capRefusals[policy.RefusedDayCap]) }

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

// AttemptFunded is the answer path's floor (ruling 322, finding 215): ""
// when the free headroom can fund the cheapest call of model, a call of
// no prompt and policy.MinCallTokens output tokens, else the refusal the
// waiting-human card carries. It guards the console's answer (DayCapped)
// and every attempt of a run from before versionSessionLadder, which
// schedules it on replay; a newer run asks SessionFunded before each
// session.
func (r *Runner) AttemptFunded(ctx context.Context, taskID, model string) (string, error) {
	return r.funded(ctx, taskID, model, false)
}

// SessionFunded is the floor of every session's start (roadmap 243, plan
// A): "" when the free headroom can fund a session's opening calls on
// model and the policy's floor of its rung, else the refusal naming the
// amount. A card the answer path's floor admits may still hold too little
// to start a session; the console offers it the Fund action.
func (r *Runner) SessionFunded(ctx context.Context, taskID, model string) (string, error) {
	return r.funded(ctx, taskID, model, true)
}

// SessionAnswered reports whether the session of a task that began at in.Since
// got a model answer, and records the session_started event of one that did:
// a pod that started and made no answered call is no session. The ledger, not
// the timeline, is the fact the session cap counts.
func (r *Runner) SessionAnswered(ctx context.Context, in SessionAnswerInput) (bool, error) {
	answered, err := r.store.ModelAnswered(ctx, in.TaskID, in.Since)
	if err != nil || !answered {
		return false, err
	}
	r.timeline.recordEvent(ctx, in.TaskID, EventSessionStarted, map[string]any{"session": in.SessionID, "model": aliasOf(in.Model), "attempt": in.Attempt})
	return true, nil
}

// price is alias's price with its output limit (policy.Document.Limited).
func (r *Runner) price(ctx context.Context, alias string) (policy.Price, error) {
	p, err := r.cfg.Prices.Price(ctx, alias)
	if err != nil {
		return policy.Price{}, err
	}
	return r.cfg.Policy().Document.Limited(alias, p)
}

// funded is the attempt floors' read. The gateway's hold math itself
// decides — what the holds take (policy.Fit, policy.Opening.Hold) against
// the free task, day, month and project headroom the gateway grants
// against (policy.FitResource; outstanding holds netted, roadmap 243)
// under the caps of the version in force — so the floor is read here,
// never restated. A model that cannot be priced refuses the attempt:
// nothing could bound its calls.
func (r *Runner) funded(ctx context.Context, taskID, model string, opening bool) (string, error) {
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
	alias := aliasOf(model)
	price, err := r.price(ctx, alias)
	if err != nil {
		// a model nobody can price is a hold nobody can bound: the
		// attempt is refused (roadmap 222, item 5)
		return fmt.Sprintf("the attempt's floor on %s cannot be read: %v", alias, err), nil
	}
	v := r.cfg.Policy()
	project, projectEUR := v.ProjectCap(t.Repository)
	req := policy.HoldRequest{TaskID: taskID, Kind: policy.KindModel, Ref: alias, Price: price,
		DayEUR: v.Document.Budget.DayEUR, MonthEUR: v.Document.Budget.MonthEUR, Project: project, ProjectEUR: projectEUR}
	// the cheapest call: no prompt and policy.MinCallTokens output tokens
	inf, cheapest := math.Inf(1), req
	cheapest.WantOut = policy.MinCallTokens
	h, _ := policy.Fit(policy.Free{Task: inf, Day: inf, Month: inf, Project: inf}, cheapest)
	need, call, measure, then := h.EUR, "the cheapest call on "+model, fmt.Sprintf("%d output tokens at the hold machinery's minimum", policy.MinCallTokens),
		"an answer would start an attempt whose first call cannot run"
	if opening {
		o, err := r.store.Opening(ctx, taskID, alias)
		if err != nil {
			return "", err
		}
		call, then = "a session's opening on "+model, "a session started now could not make its opening calls"
		measure = "no session's opening is recorded: " + measure
		eur, calls, err := o.Hold(req, func(ref string) (policy.Price, error) { return r.price(ctx, ref) })
		if err != nil {
			return fmt.Sprintf("the attempt's floor on %s cannot be read: %v", alias, err), nil
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
		frontier := r.cfg.Models.Stronger != "" && aliasOf(model) == aliasOf(r.cfg.Models.Stronger)
		if floor := v.Document.Budget.SessionFloor(frontier); floor > need {
			key := "budget.session_floor_cheap_eur"
			if frontier {
				key = "budget.session_floor_frontier_eur"
			}
			need, call, measure, then = floor, "a session on "+model, "the floor of its rung, "+key+" in policy.yaml",
				"a session started now would end before it could push"
		}
	}
	own, free, err := r.store.Free(ctx, req)
	if err != nil {
		return "", err
	}
	_, err = policy.FitResource(free, policy.HoldRequest{TaskID: taskID, Kind: policy.KindModel, Ref: alias, Project: project, EUR: need})
	if err == nil {
		return "", nil
	}
	var refused *policy.Refusal
	if !errors.As(err, &refused) {
		return "", err
	}

	opens, state := budgetRefused, fmt.Sprintf("%.3f EUR of the %.3f EUR budget remains", own.Headroom, t.BudgetEUR)
	act := "Raise the budget (Fund on the board) and answer, or close the card."
	switch refused.Decision {
	case policy.RefusedDayCap:
		state = fmt.Sprintf("%.6f EUR of the day's %.2f EUR (budget.day_eur) is free", max(free.Day, 0), req.DayEUR)
		act = "Answer, and the attempt waits on its own for the UTC day's headroom, or raise budget.day_eur in policy.yaml by a commit."
	case policy.RefusedMonthCap:
		state = fmt.Sprintf("%.6f EUR of the month's %.2f EUR (budget.month_eur) is free", max(free.Month, 0), req.MonthEUR)
		act = "Raise budget.month_eur in policy.yaml by a commit and answer, or wait for the next UTC month and answer."
	case policy.RefusedProjectCap:
		file := "projects/" + project + ".yaml"
		state = fmt.Sprintf("%.6f EUR of the project's %.2f EUR month (budget.month_eur in %s) is free", max(free.Project, 0), projectEUR, file)
		act = "Raise budget.month_eur in " + file + " by a commit and answer, or wait for the next UTC month and answer."
	default:
		if own.Holds > 0 {
			state += fmt.Sprintf(", outstanding holds take %.6f EUR of it (%d, the earliest expiring %s), so %.6f EUR is free",
				own.Held, own.Holds, stamp(own.Expires), max(own.Free(), 0))
			act = "Wait for the holds to settle and answer (a released hold books its call's cost, an expired one is booked in full), or raise the budget (Fund on the board) and answer."
		}
	}
	if o, ok := capRefusals[refused.Decision]; ok {
		opens = o
	}
	// the first line is the card's question row (escalationQuestion), cut
	// at 300 characters: it carries the actions, the state follows
	return fmt.Sprintf("%s: %s\n%s, and %s needs %.6f EUR (%s); %s.", opens, act, state, call, need, measure, then), nil
}

// startFits says free headroom f fits the cheapest call of model, a
// session's start as the attempt's floor prices it.
func startFits(f policy.Free, taskID, model string, price policy.Price) bool {
	_, err := policy.Fit(f, policy.HoldRequest{TaskID: taskID, Kind: policy.KindModel, Ref: model, Price: price})
	return err == nil
}

// DayResume reports whether a wait on the day cap ends (versionDayCap): at
// the UTC day's turn (turned), or once the day's free headroom under the
// budget.day_eur in force, every outstanding hold netted as the gateway
// nets it, fits a session's start on model (startFits). An unpriced model
// waits for the turn. An end resolves the task's open day-cap escalation
// alerts, real and the factory's, and is on the timeline.
func (r *Runner) DayResume(ctx context.Context, taskID, model string, turned bool) (bool, error) {
	why := "the UTC day turned"
	if !turned {
		if r.cfg.Prices == nil {
			return false, nil
		}
		price, err := r.price(ctx, aliasOf(model))
		if err != nil {
			return false, nil
		}
		free, err := r.store.DayFree(ctx, r.cfg.Policy().Document.Budget.DayEUR)
		if err != nil {
			return false, err
		}
		if !startFits(policy.Free{Task: math.Inf(1), Day: free, Month: math.Inf(1), Project: math.Inf(1)}, taskID, model, price) {
			return false, nil
		}
		why = fmt.Sprintf("the day's free headroom of %.6f EUR funds a session", free)
	}
	if err := r.store.JudgeEscalations(ctx, taskID, EscalationFilter{Source: "day_cap"}, VerdictReal, FactoryDecider); err != nil {
		return false, err
	}
	r.record(ctx, taskID, "day_cap_resumed", map[string]any{"why": why})
	return true, nil
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
		return SessionResult{}, named("task", err)
	}
	defer r.sessionRuns(t.ID)()
	s := sessionSpec{id: newID("s-"), parent: in.ParentSessionID, findings: in.Findings, answers: in.Answers, posterInfo: in.PosterInfo, model: in.Model, repo: repo, attempt: in.Attempt}
	if s.policy, err = r.pinned(ctx, in.PolicyVersion); err != nil {
		return SessionResult{}, named("policy", err)
	}
	s.agentTime, s.checkIn = cmp.Or(in.AgentTime, s.policy.Limits.AgentTime), cmp.Or(in.CheckIn, s.policy.Limits.CheckIn)
	if s.docs, err = r.docRepo(ctx, t, in.PolicyVersion); err != nil {
		return SessionResult{}, named("document repository", err)
	}
	if s.spec, s.cardRole, s.directories, s.pull, err = r.planOf(ctx, t); err != nil {
		return SessionResult{}, named("plan card", err)
	}
	if t.Source != SourcePlan && t.Standing() == harness.StandingWriter {
		_, body, _ := strings.Cut(t.Objective, "\n\n")
		s.directories = askedDirectories(body)
	}
	if err := r.forgejo.EnsureFork(ctx, repo); err != nil {
		return SessionResult{}, named("fork", err)
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
		s.parent = dead.SessionID
		if dead.Running {
			s.findings = slices.Concat(in.Findings, []string{fmt.Sprintf(
				"session %s stopped after %d events without finishing; continue from the branch as it is", dead.SessionID, dead.Events)})
		}
	}
	// a session the stopped factoryd detached from runs on in its pod:
	// this retry reattaches to it; a dead pod's row goes and the attempt
	// starts over from the branch
	var live *LiveSession
	if l, ok, err := r.store.LiveSession(ctx, t.ID); err != nil {
		return SessionResult{}, named("live session", err)
	} else if ok {
		if phase, _, perr := r.kube.PodStatus(ctx, l.Namespace, l.Pod); perr == nil && (phase == "Running" || phase == "Pending") {
			live = &l
			s.id, s.parent, s.findings = l.SessionID, in.ParentSessionID, in.Findings
		} else if err := r.store.DeleteLiveSession(ctx, t.ID); err != nil {
			return SessionResult{}, named("live session", err)
		}
	}
	hb := startHeartbeat(ctx, s.id)
	defer hb.stop()

	if live == nil {
		if err := r.completion.reconcile(ctx, t.ID); err != nil {
			return SessionResult{}, named("revoke leftover credentials", err)
		}
	}
	cleanup := context.WithoutCancel(ctx)
	// detached: factoryd is stopping and the session runs on for the
	// retry; its sandbox and credentials stay
	detached := false
	defer func() {
		if detached {
			return
		}
		r.forgetNamespace(cleanup, t.ID)
		if err := r.completion.reconcile(cleanup, t.ID); err != nil {
			log.Printf("task %s: revoke session %s credentials: %v", t.ID, s.id, err)
		}
	}()
	var deadline, startedAt time.Time
	if live == nil {
		// least privilege (ruling 97): read on the task's repository, the
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
		if s.cache, err = r.setupNamespace(ctx, t, base); err != nil {
			return SessionResult{}, named("namespace", err)
		}
		deadline, startedAt = sessionDeadline(activity.GetInfo(ctx).Deadline, s.agentTime), time.Now()
		r.record(ctx, t.ID, "session_start", map[string]any{
			"session": s.id, "parent": s.parent, "model": s.model, "attempt": in.Attempt, "start_ref": s.startRef, "findings": s.findings, "answers": s.answers,
			"poster_info": s.posterInfo})
	} else {
		deadline, startedAt = sessionDeadline(activity.GetInfo(ctx).Deadline, time.Until(live.Deadline)), live.StartedAt
		r.record(ctx, t.ID, "session_reattached", map[string]any{"session": s.id, "pod": live.Pod, "lines": live.Lines, "attempt": in.Attempt})
	}
	began := rfc3339(startedAt)
	out, err := r.runSession(ctx, t, r.agentNamespace(t.ID), s, deadline, hb.update, activity.GetWorkerStopChannel(ctx), live)
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
	res.Model = s.model
	if res.Outcome == OutcomePushed {
		r.timeline.recordEvent(ctx, t.ID, EventSessionPushed, map[string]any{"session": s.id, "model": aliasOf(s.model), "attempt": in.Attempt,
			"minutes": math.Round(time.Since(startedAt).Minutes()*10) / 10})
	}
	if res.Outcome != OutcomePushed {
		// no later session could spend: at a month cap a human raises it
		// or waits for the month to end; at the day cap the card waits for
		// the day's headroom
		refused, err := r.store.CapRefusal(ctx, t.ID, startedAt)
		if err != nil {
			return SessionResult{}, err
		}
		switch {
		case refused == nil:
		case refused.Decision == policy.RefusedDayCap:
			res.Outcome, res.Capped = OutcomeDayCap, refused
		default:
			res.Outcome, res.Reason = OutcomeEscalate, refused.reason()
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
	if res.Outcome != OutcomePushed {
		// the session may have proven the card itself (roadmap 208): one
		// of its plan's acceptance commands run green. A read that fails
		// costs the card nothing: it ends as it always did.
		if acceptance, err := r.planAcceptance(ctx, t); err != nil {
			log.Printf("task %s: its plan's acceptance: %v", t.ID, err)
		} else if hit := r.sessionAcceptance(ctx, t, s.id, acceptance); hit != nil {
			res.Acceptance = hit
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

// sessionResult maps a session's end onto the outcome the workflow acts on.
func sessionResult(s sessionSpec, out sessionOutcome) SessionResult {
	res := SessionResult{SessionID: s.id, Outcome: OutcomeNoPush}
	if out.start != nil {
		// every next session would fail the same way: a human fixes the
		// cause, and the answer starts the next session
		res.Outcome, res.Summary, res.Reason, res.StartFailed = OutcomeEscalate, out.start.Error(), out.start.escalation(thenAnswer), true
		return res
	}
	// a session that finished and pushed goes to verification, even when a
	// stop raced its finish
	if out.result != nil && out.result.Success && out.commit != "" {
		res.Outcome, res.Commit, res.Summary = OutcomePushed, out.commit, out.result.Summary
		return res
	}
	// a session that finished with nothing new on top of the work in
	// progress an earlier stop pushed: that work is its result
	if out.result != nil && out.result.Success && out.commit == "" && s.startRef != "" && s.startRef != s.base {
		res.Outcome, res.Commit, res.Summary = OutcomePushed, s.startRef, out.result.Summary+"; the branch already held "+s.startRef
		return res
	}
	res.LastMessage = textutil.Truncate(out.agentText, maxHandoverMessage)
	if out.trip != nil {
		res.Trip, res.Commit = string(out.trip.Kind), out.commit
		res.Findings = out.stopReport()
		if out.trip.Stuck != "" {
			res.Findings = append([]string{out.trip.Stuck}, res.Findings...)
		}
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
		if out.trip.Kind == TripLoop {
			res.Findings = append(res.Findings, loopFinding(out.trip.Detail))
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
		if !out.result.Success && strings.Contains(res.Summary, providerRepetition) && strings.Contains(res.Summary, providerRefusalStatus) {
			res.Outcome, res.Trip, res.Commit = OutcomeRedirect, string(TripLoop), out.commit
			res.Findings = []string{loopFinding("the model provider refused its repeated tool calls: " + res.Summary)}
			return res
		}
	default:
		res.Summary = out.noResult
	}
	if out.overHold != nil {
		res.Summary = strings.TrimPrefix(res.Summary+"; "+out.overHold.state(), "; ")
	}
	ended := "previous session ended without a pushed commit: "
	if out.commit != "" {
		ended = "previous session ended unfinished: "
	}
	res.Commit = out.commit
	res.Findings = []string{ended + res.Summary}
	if out.pending != nil {
		res.Outcome = OutcomeNotStarted
	}
	return res
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

// providerRepetition is Dashscope's refusal of a conversation that repeats
// its tool calls (Stage 15, Stage 16): it comes after many calls, so it is
// no outage, and the session ends as a loop. providerRefusalStatus is how
// the opencode parser ends that error event, which the model's own text
// quoting the phrase lacks.
const (
	providerRepetition    = "Repetitive tool calls detected"
	providerRefusalStatus = "(status 400)"
)

// loopFinding is the next session's brief line on a loop: how the session
// ended and what it repeated, a fact (ruling 263).
func loopFinding(how string) string { return "the previous session ended in a loop after " + how }

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
	next := "Reply on this issue to start a new attempt with your comment as guidance, or close the issue to fail the task."
	if strings.HasPrefix(reason, splitFailed) || strings.HasPrefix(reason, splitCapped) {
		// the approval card answers it; the approval wait reads no reply
		next = "Approve, deny or split it on the board."
	}
	if floorRefused(reason) {
		// no answer can run (ruling 322): the card names why and the way out
		next = "An answer starts an attempt only once the budget can fund a call, as above; close this issue to fail the task."
	}
	if strings.HasPrefix(reason, dayCapResumes) {
		next = "The task resumes on its own as above. Reply on this issue to start a new attempt sooner with your comment as guidance, or close the issue to fail the task."
	}
	if strings.HasPrefix(reason, leftQueue) {
		next = "Reply on this issue to put the pull request back at the end of the merge queue, request changes on it to send the task back to the agent, or close the issue to fail the task."
	}
	safe := r.untrusted(t, reason)
	body := "factoryd: task " + taskID + " needs a human.\n\n" + fenced(safe)
	if links := sanitize.Links(safe, r.linkHosts(t)); len(links) > 0 {
		// the fence renders no link: the ones the sanitiser keeps follow it
		body += "\n\nLinks:\n- " + strings.Join(links, "\n- ")
	}
	body += "\n\n" + next
	if strings.HasPrefix(reason, screeningAsks) {
		// the reasons go to the card only: they would teach the author
		body = screenedLine
	}
	var id int64
	if t.IssueNumber != 0 {
		// a task without one (a report task) has its question on the card alone
		if id, err = r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, body); err != nil {
			return 0, err
		}
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
	asked := map[string]any{"question": textutil.Truncate(question, 300), "comment": id, "source": escalationSource(reason)}
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

// ProviderWait records that the task waits w.Backoff on the model
// provider; its schedule puts the task in gate provider (activityGates).
func (r *Runner) ProviderWait(ctx context.Context, taskID string, w ProviderWait) error {
	return r.timeline.Append(ctx, taskID, StepProviderRefused, map[string]any{"status": w.Status, "reason": w.Reason, "backoff_s": int(w.Backoff.Seconds())})
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
// the row's process, else (a row the loader's backfill has not reached)
// the process the task's project follows in that version, and the class
// of the row's lane. A report task runs its own class, the write-up's or
// the screening's, with no lane; a roll has no lane and runs ready's
// class, whose program is the lifecycle. A card for a pull request
// factoryd opened outside a card's workflow (roadmap 187) has no lane
// either and runs ready's process and class.
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
	laneRow := cmp.Or(t.Lane, policy.LaneReady)
	if (t.Source == SourceRoll || factoryPRSources(t.Source)) && t.Lane == "" {
		laneRow = policy.LaneReady
	}
	tc := TaskConfig{Process: name, Lane: t.Lane, Stages: p.Stages, Queue: p.Merge.Queue, Dependencies: p.Dependencies}
	if plan, ok := p.Lane(policy.LanePlan); ok {
		tc.PlanShape = plan.Plan
	}
	switch lane, ok := p.Lane(laneRow); {
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
	tc.Stronger = tc.Session == policy.SessionCode && (project.Model == policy.ModelStronger || t.Rung == RungFrontier)
	// stronger_after is 1 for a medium or large card (roadmap 222, item 5)
	if t.Size == "medium" || t.Size == "large" {
		tc.StrongerAfter = 1
	}
	tc.AgentTime, tc.CheckIn = v.Document.TimeOf(tc.Class, p, project)
	tc.NoAudit = !project.Audit
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
		Roll: t.Source == SourceRoll, FactoryPR: factoryPRSources(t.Source), Source: t.Source}
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
	if !st.FactoryPR { // its object store is the card's session's, which never ran
		if err := r.stageMemory(ctx, t.ID, &st); err != nil {
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
			st.Findings = append(st.Findings, changeRequested(e.Author, e.Body, e.CreatedAt))
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
	st.DayCap, st.DayCapUntil = dayCapWait(events)
	st.Screening = waitsOnScreening(events)
	st.InfoRequest, st.InfoRequestedAt, st.PosterInfo, st.InfoAnswered = posterInfoFrom(events)
	r.record(ctx, taskID, "reconcile", map[string]any{"status": st.Status, "pr": st.PR.Number, "head": st.Head,
		"merged": st.Merged, "pr_closed": st.PRClosed, "issue_closed": st.IssueClosed, "skip": st.Skip, "findings": len(st.Findings),
		"answers": len(st.Answers)})
	return st, nil
}

// stageMemory rebuilds what the stages remembered from the task's
// timeline: the stage_findings steps (returns to the agent, the history),
// a dependencies_started step (the card started despite a dependency),
// the model_review steps (the reviews), the last round, and the
// finding_adjudicated steps (the overrules an approval recorded on the
// reviews' findings, versionAdjudicated).
func (r *Runner) stageMemory(ctx context.Context, taskID string, st *RunState) error {
	b, err := r.objects.Get(ctx, "sessions/"+taskID+"/timeline.jsonl")
	if err != nil {
		return fmt.Errorf("read the timeline: %w", err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		var step struct {
			Step, Stage, To, Verdict, Summary, Note, Reason string
			By, Finding, Why                                string
			Round, Attempt                                  int
			Findings                                        []string
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
		case "model_review":
			st.Reviews = append(st.Reviews, Review{Verdict: step.Verdict, Summary: step.Summary, Findings: step.Findings, Note: step.Note})
			st.Round = max(st.Round, step.Round)
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
// (oldest first): every trusted issue comment that answers an escalation
// by the comments' ids (answerOf), tagged with that escalation's question,
// its first line (a healed re-ask keeps the question it re-asks, its own
// with none before it), in answerKey's order. question is the last one
// asked, choices the answers its agent offered. The live run tags its
// answers alike (awaitHuman, awaitHumanAgain).
func answersFrom(events []IntakeEvent) (answers []string, question string, choices []string) {
	var anchors []anchor
	for _, e := range events {
		switch e.Kind {
		case IntakeEscalation:
			if e.Body != healedReason || question == "" {
				question, choices = escalationQuestion(e.Body), e.Choices
			}
			anchors = append(anchors, anchor{comment: e.CommentID, question: question})
		case IntakeInfoRequest:
			anchors = append(anchors, anchor{comment: e.CommentID, request: true})
		}
	}
	for _, e := range events {
		if i := slices.IndexFunc(anchors, func(a anchor) bool { return !a.request && a.comment == e.Withdraws }); e.Kind == IntakeEscalationWithdrawn && i >= 0 {
			anchors[i].withdrawn = e.CommentID
		}
	}
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
			got = append(got, answer{answerKey{a.comment, e.CommentID}, answerText(e.Author, a.question, e.Body, e.CreatedAt)})
		}
	}
	slices.SortStableFunc(got, func(x, y answer) int { return x.key.compare(y.key) })
	for _, a := range got {
		answers = append(answers, a.text)
	}
	return answers, question, choices
}

// anchor is a comment on the task's issue that later comments answer, by
// the comments' ids (answerOf): an escalation, which asked question, or
// an information request (request); withdrawn is the comment of an
// escalation's withdrawal, 0 while none.
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
type answerKey struct{ escalation, comment int64 }

func (k answerKey) compare(o answerKey) int {
	return cmp.Or(cmp.Compare(o.escalation, k.escalation), cmp.Compare(k.comment, o.comment))
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
		res := checksResult(cs, r.forgejo.PublicURL, r.jobLogOf(ctx, t, repo))
		if res.State == "" && !time.Now().Before(deadline) {
			res.State = ChecksTimeout
		}
		if res.State != "" {
			if res.State == ChecksFailure {
				r.annotateMainFailures(ctx, repo, &res)
			}
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

// HeadChecks reads the staged head's combined commit status once, without
// the checks wait's polling: the read the approver does by hand, and the
// one the approval stage asks before its request (versionApprovalGreen). A
// red head answers its failing checks as WaitChecks reports them; one
// whose checks are still running, or that no check has reported on yet,
// answers the pending checks as the findings, and the approval sends them
// back to the agent, which waits in the checks stage.
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
		res = ChecksResult{State: ChecksFailure, Findings: pendingCheckFindings(sha, cs)}
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
		if s.State != "success" && s.State != "warning" {
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
	r.record(ctx, taskID, "checks_rerun", map[string]any{"commit": sha, "head": head})
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

// alertStoreReader is what an alert card's pre-check reads of the alert
// store: the newest alerts row of a fingerprint. PGStore has it; a test
// store without it takes the check as passing.
type alertStoreReader interface {
	AlertByFingerprint(ctx context.Context, kind, fingerprint string) (Alert, bool, error)
}

// AlertPreCheck is an alert card's pre-check before its first model call
// (roadmap 181, Stage 18 item 3.12): it reads the task's alert fingerprint
// and its alerts row, the one record of whether the alert fires (kept true
// by the resolve deliveries, FluxPass and SweepSigNoz), and reports whether
// it still fires: no resolve after its start. An alert that no longer
// fires was a transient burst: the card closes itself without spending on
// a model call. No fingerprint, or no row of this card, says it still
// fires: the check never stops a card on its own absence.
func (r *Runner) AlertPreCheck(ctx context.Context, taskID string) (bool, error) {
	t, ok, err := r.store.Task(ctx, taskID)
	if err != nil {
		return false, err
	}
	if !ok || t.AlertFingerprint == "" {
		return true, nil
	}
	fr, ok := r.store.(alertStoreReader)
	if !ok {
		return true, nil
	}
	row, seen, err := fr.AlertByFingerprint(ctx, AlertSigNoz, t.AlertFingerprint)
	if err != nil {
		return false, err
	}
	if !seen || row.TaskID != taskID {
		return true, nil
	}
	_, firing := firingSince(row)
	return firing, nil
}

// Finish is the terminal step. A failed or closed task's open pull request
// is closed first, with the reason, and a closed task's issue still open
// too; a done or closed task's branch on the fork is deleted. Both return a transient error, so a Forgejo blip retries them before the task
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
	if t.Source == SourceRoll {
		if err := r.ClearRollHold(ctx, taskID, final == "done"); err != nil {
			return err
		}
	}
	if err := r.store.DeleteLiveSession(ctx, taskID); err != nil {
		return err
	}
	r.forgetNamespace(ctx, taskID)
	why := r.untrusted(t, reason)
	msg := "factoryd: task " + taskID + " failed: " + why
	// a malformed repository still completes the task, only uncommented
	repo, repoErr := SplitRepo(t.Repository)
	if repoErr == nil {
		if final != "done" {
			if final == "closed" {
				msg = "factoryd: task " + taskID + " closed: " + why
			}
			if err := cleanup(taskID, "close its pull request", r.closeTaskPullRequest(ctx, repo, t, msg)); err != nil {
				return err
			}
		}
		if final == "closed" && t.IssueNumber != 0 {
			if err := cleanup(taskID, "close its issue", r.closeTaskIssue(ctx, repo, t, msg)); err != nil {
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
	taskEnded(ctx, r.timeline, r.store, r.forgejo, r.cfg.Workflows, t, final, why)
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

// closeTaskIssue closes a closed task's issue while it is still open, as
// factoryd's recorded close, then leaves msg as the reason, as
// closeTaskPullRequest does on the pull request: the card a change request
// closed (versionFactoryPRActions) reads closed on its issue as on the
// board. An issue a human closed, the usual way to a closed task, is left.
func (r *Runner) closeTaskIssue(ctx context.Context, repo Repo, t Task, msg string) error {
	closed, err := r.forgejo.IssueClosed(ctx, repo.Owner, repo.Name, t.IssueNumber)
	if err != nil || closed {
		return err
	}
	if err := recordFactoryClose(ctx, r.store, t); err != nil {
		return err
	}
	if err := r.forgejo.CloseIssue(ctx, repo.Owner, repo.Name, t.IssueNumber); err != nil {
		return err
	}
	if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, msg); err != nil {
		log.Printf("task %s: comment the close's reason on issue #%d: %v", t.ID, t.IssueNumber, err)
	}
	return nil
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
