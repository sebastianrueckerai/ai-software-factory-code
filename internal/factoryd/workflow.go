package factoryd

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
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
	// SignalHumanComment is a non-bot comment on the task's issue; it
	// answers an escalation when it is newer than the escalation comment.
	SignalHumanComment = "human_comment"
	// SignalApproved is the console's approve (ApproveMerge), sent after
	// the member's APPROVED review is on the pull request, or with none
	// when the project agent decided (Author ChatUser): the workflow merges
	// as factory-merge.
	SignalApproved = "approved"
	// SignalPosterComment is the issue author's comment when they cannot
	// write the repository: it answers an information request newer than
	// it (ruling 150), never an escalation.
	SignalPosterComment = "poster_comment"
	// SignalAnswerWhole is the console's notice that issue comment Comment,
	// which its answer box posted, is a whole answer: the wait it answers
	// resumes once it took that comment, with no quiet window
	// (versionAnswerBurst).
	SignalAnswerWhole = "answer_whole"
	// SignalRollFolded is the roller's notice that newer images were
	// folded into a roll task (ruling 178): it wakes the approval wait and
	// is never a finding.
	SignalRollFolded = "roll_folded"
	// SignalRerunChecks is the console's re-run of a factory pull request
	// card's red checks (versionFactoryPRActions): the approval wait runs
	// them again on an empty commit and asks again with what they say.
	SignalRerunChecks = "rerun_checks"
	// SignalStaleNudge is the stale sweep's wake-up of a live-status card
	// quiet for the stale window: SignalWithStart starts a workflow that
	// died; a live one has no handler and drops it.
	SignalStaleNudge = "stale_nudge"
	// SignalImageReady is the imager's wake of a card parked on an image
	// (versionToolPark): TaskSignal's Image, Refusal and At, the park's
	// created_at, which tells its own park from an earlier one.
	SignalImageReady = "image_ready"
)

// TaskSignal is a signal's payload: who acted, what they wrote, and on
// which pull request (0 for an issue event). The workflow counts a pull
// request signal only for the pull request it opened itself.
type TaskSignal struct {
	Author  string
	Body    string
	PR      int64
	Comment int64     // the issue comment's id, for human_comment
	At      time.Time // when the comment or review was made; zero for a signal from before dates
	// Revision and Index are the plan question plan_answered answers.
	Revision int
	Index    int
	// Image and Refusal are image_ready's: the image the card waited on,
	// and why it does not run on it ("" when it does).
	Image   string `json:",omitempty"`
	Refusal string `json:",omitempty"`
}

// Activity names, registered by Runner.RegisterActivities.
const (
	ActivityEnsureIssue                 = "EnsureIssue"
	ActivitySetState                    = "SetState"
	ActivitySetGate                     = "SetGate"
	ActivityRunSession                  = "RunSession"
	ActivityOpenPullRequest             = "OpenPullRequest"
	ActivityWaitChecks                  = "WaitChecks"
	ActivityHeadChecks                  = "HeadChecks"
	ActivityPullRequestMerged           = "PullRequestMerged"
	ActivityRecordStep                  = "RecordStep"
	ActivityFinish                      = "Finish"
	ActivityReconcile                   = "Reconcile"
	ActivityEscalate                    = "Escalate"
	ActivityRecordUsage                 = "RecordUsage"
	ActivityGatePolicy                  = "GatePolicy"
	ActivityGateMigration               = "GateMigration"
	ActivityGateReproduction            = "GateReproduction"
	ActivityGateCriteria                = "GateCriteria"
	ActivityHoldoutSettlement           = "SettleHoldouts"
	ActivityModelReview                 = "ModelReview"
	ActivityRequestApproval             = "RequestApproval"
	ActivityMerge                       = "Merge"
	ActivityReportMergeFailure          = "ReportMergeFailure"
	ActivityReportMergeRetriesExhausted = "ReportMergeRetriesExhausted"
	ActivitySuperviseGate               = "SuperviseGate"
	ActivityDropTrip                    = "DropTrip"
	ActivityAutoMergeNote               = "AutoMergeNote"
	ActivityRequestInfo                 = "RequestInfo"
	ActivityCloseNotReproduced          = "CloseNotReproduced"
	ActivityScreen                      = "Screen"
	ActivityJudgeScreening              = "JudgeScreening"
	ActivityScreenAnswer                = "ScreenAnswer"
	ActivityRunReport                   = "RunReport"
	ActivityRollCommit                  = "RollCommit"
	ActivityRerunChecks                 = "RerunChecks"
	ActivityGateAudit                   = "GateAudit"
	ActivityTaskConfig                  = "TaskConfig"
	ActivityBranchHead                  = "BranchHead"
	ActivityAwaitSessionGap             = "AwaitSessionGap"
	ActivityAwaitRollHold               = "AwaitRollHold"
	ActivityClearRollHold               = "ClearRollHold"
	ActivityAwaitRollout                = "AwaitRollout"
	ActivityProviderWait                = "ProviderWait"
	ActivityAlertPreCheck               = "AlertPreCheck"
	ActivityAttemptFunded               = "AttemptFunded"
	ActivitySessionFunded               = "SessionFunded"
	ActivitySessionAnswered             = "SessionAnswered"
	ActivityDayResume                   = "DayResume"
	ActivityParkForTool                 = "ParkForTool"
	ActivityImageArrived                = "ImageArrived"
	ActivityLeaveImagePark              = "LeaveImagePark"
)

// errTerminal marks an activity error no retry can fix; the workflow fails
// the task with its message.
const errTerminal = "terminal"

func terminalError(format string, args ...any) error {
	return temporal.NewNonRetryableApplicationError(fmt.Sprintf(format, args...), errTerminal, nil)
}

// errHeadMoved types a gate's refusal of a head the agent moved past it:
// not terminal — the workflow holds the moved head (versionHeadHolds).
const errHeadMoved = "head-moved"

func headMovedError(format string, args ...any) error {
	return temporal.NewNonRetryableApplicationError(fmt.Sprintf(format, args...), errHeadMoved, nil)
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
	WallClockCap time.Duration
	// AnswerSlice is the agent time a human answer guaranteed the attempt
	// it bought, for a run from before the change id "answer-slice": kept
	// only so that history still replays. A current run computes the
	// slice as a quarter of WallClockCap (grant).
	AnswerSlice   time.Duration
	ChecksTimeout time.Duration
	// StrongerAfter is how many returns from the same stage keep the
	// model; the next takes the stronger rung (limits.stronger_after).
	StrongerAfter int
	// PolicyVersion is the configuration version the task started under,
	// whose tree its stages read.
	PolicyVersion int64
	// Stages are the stages the task runs (versionStages): Task's, or for a
	// run from before versionProcesses the ones its history recorded.
	Stages []policy.Stage
	// Task is TaskConfig's answer (versionProcesses); zero before it.
	Task TaskConfig
}

// TaskConfig is what the task's process in its pinned version makes of it:
// the track it runs on, the class and the session mode that choose its
// program, the stages, the merge queue, the dependency wait and the plan
// track's shape.
type TaskConfig struct {
	Process      string
	Track        string
	Class        string
	Session      string
	Stages       []policy.Stage
	Queue        string
	Dependencies *policy.Dependencies
	PlanShape    *policy.PlanShape
	// AgentTime and CheckIn are policy.Document.TimeOf's answer for the
	// task's class, process and project; zero for a run from before this
	// resolution existed, which keeps WorkflowConfig.WallClockCap and the
	// session's own policy limits.
	AgentTime time.Duration
	CheckIn   time.Duration
	// Stronger: the project's file sets model: stronger and this is a
	// coding card (Session code); the card's first session runs on
	// WorkflowConfig.Models.Stronger instead of Models.Start. A run from
	// before this field existed decodes it false. The planner and report
	// sessions keep their own rung.
	Stronger bool
	// StrongerAfter overrides WorkflowConfig's for the task when above 0:
	// 1 for a medium or large card (roadmap 222, item 5)
	StrongerAfter int
	// NoAudit: the task's project does not switch the audit on (ruling
	// 188), so the audit stage is skipped at its turn and starts nothing
	// beside the checks (versionAuditBeside).
	NoAudit bool
}

// SessionOutcome is how one RunSession attempt ended; the workflow switches
// on it. redirect, retry_stronger and escalate are the supervisor's
// verdicts (architecture 6.6, ruling 87); needs_info is the agent's
// request_info (ruling 150), missing_tool its request_tool.
type SessionOutcome string

const (
	OutcomePushed        SessionOutcome = "pushed"
	OutcomeNoPush        SessionOutcome = "no_push"
	OutcomeRedirect      SessionOutcome = "redirect"
	OutcomeRetryStronger SessionOutcome = "retry_stronger"
	OutcomeEscalate      SessionOutcome = "escalate"
	OutcomeWallClock     SessionOutcome = "wall_clock"
	OutcomeNeedsInfo     SessionOutcome = "needs_info"
	OutcomeMissingTool   SessionOutcome = "missing_tool"
	// OutcomeProviderRefused: the model provider refused the session's
	// first call and every one after (ruling 267); the session never
	// worked, so it is no attempt.
	OutcomeProviderRefused SessionOutcome = "provider_refused"
	// OutcomeDayCap: the gateway refused a model call of the session at
	// the day cap (SessionResult.Capped), which resets when the UTC day
	// turns.
	OutcomeDayCap SessionOutcome = "day_cap"
	// OutcomeNotStarted: the session's pod never left Pending within its
	// start's wait (Summary says on what): no session ran.
	OutcomeNotStarted SessionOutcome = "not_started"
	// OutcomeCheckinHuman: the check-in at the session's start ended on
	// only a human can act (CheckIn carries the reason and its kind); no
	// conversation ran, so the session spent nothing and is no attempt.
	OutcomeCheckinHuman SessionOutcome = "checkin_human"
	// OutcomeCheckinDropped: the check-in dropped every finding against
	// evidence (CheckIn carries the verdicts); the round verifies again
	// instead of buying a session.
	OutcomeCheckinDropped SessionOutcome = "checkin_dropped"
)

// StepProviderRefused is the timeline step of a task waiting on the model
// provider after a refused session.
const StepProviderRefused = "provider_refused"

// ProviderWait is a provider_refused session's wait: the provider's
// status and the worker's message.
type ProviderWait struct {
	Status  int
	Reason  string
	Backoff time.Duration
}

// SessionAnswerInput asks whether the session that began at Since got a
// model answer (versionSessionLadder); the rest is its record.
type SessionAnswerInput struct {
	TaskID, SessionID, Model string
	Attempt                  int
	Since                    time.Time
}

// SessionInput is one attempt: StartRef is the commit the worker starts
// from (empty = the task's base SHA).
type SessionInput struct {
	TaskID  string
	Attempt int
	// Sessions is the cap Attempt counts against: the session limit and
	// the attempts human answers granted
	Sessions int
	Model    string
	Findings []string
	// Answers are every human answer to the task's escalations, newest
	// first (versionAnswers).
	Answers []string
	// PosterInfo is what the issue's author answered the agent's
	// information requests, oldest first (versionNeedsInfo).
	PosterInfo      []string
	StartRef        string
	ParentSessionID string
	// PolicyVersion is WorkflowConfig.PolicyVersion: the session reads
	// its limits from it; 0 (a workflow from before the pin) reads the
	// version in force.
	PolicyVersion int64
	// AgentTime and CheckIn are TaskConfig's, the task's resolved values;
	// zero falls back to the pinned policy's limits.
	AgentTime time.Duration
	CheckIn   time.Duration
	// CheckInAsk is the check-in the workflow asks of the session's
	// start; nil: none runs.
	CheckInAsk *CheckInAsk
	// LastGates are the gates of the round that sent the session back to
	// the agent; nil before any round gated. The in-session check-in
	// reads them beside the trip.
	LastGates *GateResults `json:",omitempty"`
	// Handover is the note the session's brief opens with (a mid-session
	// handover); "" for the plain brief.
	Handover string `json:",omitempty"`
}

type SessionResult struct {
	// Model is the alias the session ended on: the stronger one after an
	// in-session climb (roadmap 222, item 5)
	Model     string
	Outcome   SessionOutcome
	SessionID string
	Commit    string // pushed; for a supervisor stop, the work in progress
	Summary   string
	Findings  []string
	// LastMessage is the agent's last message, cut at maxHandoverMessage, ""
	// when it said none; the next session is told it beside the stop reason.
	LastMessage string
	// Reason is the supervisor's escalation reason with the progress
	// digest's summary, for the issue; Choices the answers the agent
	// offered with its own escalation.
	Reason  string
	Choices []string
	// CheckIn is the check-in that ran at the session's start; nil when
	// none ran. The workflow records its steps from it.
	CheckIn *CheckInOutcome `json:",omitempty"`
	// Trip is the TripKind the supervisor stopped the session on, loop for
	// the provider's refusal of repeated calls too, "" for none.
	Trip string
	// Status is the provider's HTTP status of a provider_refused session.
	Status int `json:",omitempty"`
	// StartFailed: the session's pod could not start, on a cause no
	// session fixes (Outcome escalate). No agent ran, so the session took
	// no agent time and made no model call.
	StartFailed bool `json:",omitempty"`
	// Acceptance is the plan acceptance command the session ran with exit
	// 0 (roadmap 208), when it ran one; a card it proves closes done on
	// the fact, no model verdict and no human's answer owed.
	Acceptance *AcceptanceHit
	// Capped is a day_cap session's refusal.
	Capped *CapRefusal `json:",omitempty"`
	// Started is when the session began, its pod's first start for a
	// session reattached after a restart: the since of its answer read.
	Started time.Time `json:",omitzero"`
	// Tool and Need are a missing_tool session's request_tool.
	Tool string `json:",omitempty"`
	Need string `json:",omitempty"`
}

type PullRequest struct {
	Number int64
	URL    string
}

// RunState is where a task stands when a run starts (Reconcile).
type RunState struct {
	Skip        bool // already done or failed, or a human-lane task: nothing to run
	Status      string
	PR          PullRequest // the task's open pull request, adopted by the run
	Head        string      // its head commit
	Merged      bool        // the task's pull request merged
	PRClosed    bool        // the task's pull request closed without merge
	IssueClosed bool
	// Findings are the trusted change requests signalled after the last
	// session began and never taken up: a run that died between the
	// signal and its next session starts one with them.
	Findings []string
	Usage    TaskUsage // what earlier runs used of the caps and were granted
	// Answers are the human's answers to the task's escalations rebuilt
	// from the intake events, newest first (ruling 120), and Question the
	// question of its last escalation, which a healed run asks again with
	// its Choices.
	Answers  []string
	Question string
	Choices  []string
	// InfoRequest is the comment of the task's last information request
	// and InfoRequestedAt its time, PosterInfo the answers to them, oldest
	// first (ruling 150): a healed run waiting on the issue's author
	// waits on, for what is left of the 14 days.
	InfoRequest     int64
	InfoRequestedAt time.Time
	PosterInfo      []string
	// InfoAnswered: an answer to the last information request is among
	// PosterInfo already; the healed run resumes without waiting.
	InfoAnswered bool
	// Report: the task is a report session's (ruling 185), no stage.
	Report bool
	// Standing is the issue author's (Task.Standing), for a stage's when.
	Standing string
	// Screening: the human the task waits on answers its screening's
	// suspect verdict.
	Screening bool
	// Stronger: a report task that runs on the stronger rung (the
	// screening, ruling 191).
	Stronger bool
	// The stage memory the timeline kept (versionStages): each stage's
	// returns to the agent, every finding sent there (or to the card at a
	// cap) tagged with stage and round, the model reviews oldest first,
	// and the last round.
	Returns map[string]int
	History []string
	Reviews []Review
	Round   int
	// CheckinVerdicts: every check-in's verdicts in timeline order (plan
	// D), the priors its guards read, rebuilt as the reviews' are.
	CheckinVerdicts []map[string]CheckVerdict
	// Roll: a roll task (ruling 178), whose commit RollCommit writes.
	Roll bool
	// FactoryPR: the task's card of a pull request factoryd opened outside
	// any card's workflow (roadmap 187): its run verifies that pull request,
	// it opens none of its own.
	FactoryPR bool
	// Climbed: a session's findings went to the agent with the stronger
	// rung (retry_stronger).
	Climbed bool
	// DepStarted: a human started the card despite a dependency that ended
	// failed or closed (stepDepsStarted), with the reason (versionDepWait).
	DepStarted string
	// DayCap is the reason of the task's last escalation when the day cap
	// stopped it, DayCapUntil the turn of that escalation's UTC day: a
	// healed run waits on (versionDayCap).
	DayCap      string
	DayCapUntil time.Time `json:",omitzero"`
	// Source is the task row's arrival (alert-precheck, roadmap 181 item
	// 3.12): an alert card checks its alert still fires before its first
	// model call.
	Source string
	// Park is the task's latest park on an image (image_parks), nil for
	// none: a healed run of a card in waiting-image waits on in it.
	Park *ImagePark `json:",omitempty"`
}

// ChecksState is WaitChecks' verdict on the PR head's combined status.
type ChecksState string

const (
	ChecksSuccess ChecksState = "success"
	ChecksFailure ChecksState = "failure"
	ChecksTimeout ChecksState = "timeout"
	// ChecksPending is HeadChecks' answer for a head whose checks have
	// not settled (still running, or none reported yet): not red — the
	// approval stage waits their conclusion out (versionApprovalPending).
	ChecksPending ChecksState = "pending"
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
	// Config is read once, at the start, into the history.
	Config func() WorkflowConfig
}

func (w TaskWorkflow) Run(ctx workflow.Context, taskID string) (string, error) {
	var cfg WorkflowConfig
	if err := workflow.SideEffect(ctx, func(workflow.Context) any { return w.Config() }).Get(&cfg); err != nil {
		return "", err
	}
	// short activities (Reconcile, SetState, OpenPullRequest, Escalate,
	// ...) talk to Postgres, Forgejo or the cluster; an outage of any of
	// them is retried with capped backoff for up to a day, like finish
	short := workflow.WithActivityOptions(ctx, finishOptions())
	run := taskRun{ctx: ctx, short: short, cfg: cfg, taskID: taskID,
		v:   workflow.GetVersion(ctx, "signals-drained", workflow.DefaultVersion, versionSignalsDrained),
		v2:  workflow.GetVersion(ctx, "reconcile-race", workflow.DefaultVersion, versionReconcileRace),
		v3:  workflow.GetVersion(ctx, "supervisor", workflow.DefaultVersion, versionSupervisor),
		v4:  workflow.GetVersion(ctx, "reconcile-findings", workflow.DefaultVersion, versionReconcileFindings),
		v5:  workflow.GetVersion(ctx, "persist-usage", workflow.DefaultVersion, versionPersistUsage),
		v6:  workflow.GetVersion(ctx, "gates", workflow.DefaultVersion, versionGates),
		v7:  workflow.GetVersion(ctx, "answers", workflow.DefaultVersion, versionAnswers),
		v8:  workflow.GetVersion(ctx, "gates-pinned", workflow.DefaultVersion, versionGatesPinned),
		v9:  workflow.GetVersion(ctx, "merge-failure-reported", workflow.DefaultVersion, versionMergeFailureReported),
		v10: workflow.GetVersion(ctx, "merge-retry-cap", workflow.DefaultVersion, versionMergeRetryCap),
		v11: workflow.GetVersion(ctx, "gate-started", workflow.DefaultVersion, versionGateStarted),
		v12: workflow.GetVersion(ctx, "gate-supervisor", workflow.DefaultVersion, versionGateSupervisor),
		v13: workflow.GetVersion(ctx, "auto-merge", workflow.DefaultVersion, versionAutoMerge),
		v14: workflow.GetVersion(ctx, "needs-info", workflow.DefaultVersion, versionNeedsInfo),
		v15: workflow.GetVersion(ctx, "gate-supervisor-race", workflow.DefaultVersion, versionGateSupervisorRace),
		v16: workflow.GetVersion(ctx, "stages", workflow.DefaultVersion, versionStages),
		v17: workflow.GetVersion(ctx, "roll", workflow.DefaultVersion, versionRoll),
		v18: workflow.GetVersion(ctx, "session-cap", workflow.DefaultVersion, versionSessionCap),
		v19: workflow.GetVersion(ctx, "audit", workflow.DefaultVersion, versionAudit),
		v20: workflow.GetVersion(ctx, "review-verdict", workflow.DefaultVersion, versionReviewVerdict),
		v21: workflow.GetVersion(ctx, "checks-rerun", workflow.DefaultVersion, versionChecksRerun),
		v22: workflow.GetVersion(ctx, "poster-cap", workflow.DefaultVersion, versionPosterCap),
		v23: workflow.GetVersion(ctx, "merge-conflict", workflow.DefaultVersion, versionMergeConflict)}
	run.attemptFunded = workflow.GetVersion(ctx, "attempt-funded", workflow.DefaultVersion, versionAttemptFunded)
	run.processes = workflow.GetVersion(ctx, "processes", workflow.DefaultVersion, versionProcesses)
	run.depWait = workflow.GetVersion(ctx, "dep-wait", workflow.DefaultVersion, versionDepWait)
	run.mergeQueue = workflow.GetVersion(ctx, "merge-queue", workflow.DefaultVersion, versionMergeQueue)
	run.plan = workflow.GetVersion(ctx, "plan", workflow.DefaultVersion, versionPlan)
	run.keepWork = workflow.GetVersion(ctx, "keep-work", workflow.DefaultVersion, versionKeepWork)
	run.answerScreening = workflow.GetVersion(ctx, "screen-answers", workflow.DefaultVersion, versionScreenAnswers)
	run.answerSlice = workflow.GetVersion(ctx, "answer-slice", workflow.DefaultVersion, versionAnswerSlice)
	run.rollGap = workflow.GetVersion(ctx, "roll-gap", workflow.DefaultVersion, versionRollNoGap)
	run.rollQueue = workflow.GetVersion(ctx, "roll-no-queue", workflow.DefaultVersion, versionRollNoQueue)
	run.rollout = workflow.GetVersion(ctx, "roll-rollout", workflow.DefaultVersion, versionRollout)
	run.cheapTurn = workflow.GetVersion(ctx, "cheap-turn", workflow.DefaultVersion, versionCheapTurn)
	run.providerWait = workflow.GetVersion(ctx, "provider-wait", workflow.DefaultVersion, versionProviderWait)
	run.planAnswers = workflow.GetVersion(ctx, "plan-answers", workflow.DefaultVersion, versionPlanAnswers)
	run.sessionRefusals = workflow.GetVersion(ctx, "provider-wait-sessions", workflow.DefaultVersion, versionSessionRefusals)
	run.closedByHuman = workflow.GetVersion(ctx, "closed-by-human", workflow.DefaultVersion, versionClosedByHuman)
	run.factoryPRCard = workflow.GetVersion(ctx, "factory-pr-card", workflow.DefaultVersion, versionFactoryPRCard)
	run.alertPreCheck = workflow.GetVersion(ctx, "alert-precheck", workflow.DefaultVersion, versionAlertPreCheck)
	run.approvalGreen = workflow.GetVersion(ctx, "approval-green", workflow.DefaultVersion, versionApprovalGreen)
	run.adjudicated = workflow.GetVersion(ctx, "adjudicated", workflow.DefaultVersion, versionAdjudicated)
	run.reviewParallel = workflow.GetVersion(ctx, "review-parallel", workflow.DefaultVersion, versionReviewSupervised)
	run.answerBurst = workflow.GetVersion(ctx, "answer-burst", workflow.DefaultVersion, versionAnswerBurst)
	run.planBurst = workflow.GetVersion(ctx, "plan-burst", workflow.DefaultVersion, versionPlanBurst)
	run.redRestage = workflow.GetVersion(ctx, "red-head-restage", workflow.DefaultVersion, versionRedHeadRestage)
	run.checkedRead = workflow.GetVersion(ctx, "checked-head", workflow.DefaultVersion, versionCheckedHead)
	run.tripRouting = workflow.GetVersion(ctx, "trip-routing", workflow.DefaultVersion, versionTripRouting)
	run.auditBeside = workflow.GetVersion(ctx, "audit-beside", workflow.DefaultVersion, versionAuditBeside)
	// its value is unused; the call stays until no running history carries the marker
	workflow.GetVersion(ctx, "burst-accept", workflow.DefaultVersion, versionBurstAccept)
	run.queueTurns = workflow.GetVersion(ctx, "queue-turns", workflow.DefaultVersion, versionQueueTurns)
	run.dayCap = workflow.GetVersion(ctx, "day-cap", workflow.DefaultVersion, versionDayCap)
	run.factoryPRSource = workflow.GetVersion(ctx, "factory-pr-source", workflow.DefaultVersion, versionFactoryPRSource)
	run.podWait = workflow.GetVersion(ctx, "pod-wait", workflow.DefaultVersion, versionPodWait)
	run.besideLadder = workflow.GetVersion(ctx, "beside-ladder", workflow.DefaultVersion, versionBesideLadder)
	run.factoryPRFindings = workflow.GetVersion(ctx, "factory-pr-findings", workflow.DefaultVersion, versionFactoryPRFindings)
	run.factoryPRActions = workflow.GetVersion(ctx, "factory-pr-actions", workflow.DefaultVersion, versionFactoryPRActions)
	run.sessionLadder = workflow.GetVersion(ctx, "session-ladder", workflow.DefaultVersion, versionSessionLadder)
	run.reviewRemark = workflow.GetVersion(ctx, "review-remark", workflow.DefaultVersion, versionReviewRemark)
	run.plannerRetry = workflow.GetVersion(ctx, "planner-retry", workflow.DefaultVersion, versionPlannerRetry)
	run.approvalPending = workflow.GetVersion(ctx, "approval-pending", workflow.DefaultVersion, versionApprovalPending)
	run.criteriaGate = workflow.GetVersion(ctx, "criteria-gate", workflow.DefaultVersion, versionCriteriaGate)
	run.checkinRaise = workflow.GetVersion(ctx, "checkin-raise", workflow.DefaultVersion, versionCheckinRaise)
	run.checkinSpend = workflow.GetVersion(ctx, "checkin-spend", workflow.DefaultVersion, versionCheckinSpend)
	run.checkinGrant = workflow.GetVersion(ctx, "checkin-grant", workflow.DefaultVersion, versionCheckinGrant)

	run.headHolds = workflow.GetVersion(ctx, "head-holds", workflow.DefaultVersion, versionHeadHolds)
	run.toolPark = workflow.GetVersion(ctx, "tool-park", workflow.DefaultVersion, versionToolPark)
	// its value is unused; the call stays until no running history carries the marker
	workflow.GetVersion(ctx, "chain-done", workflow.DefaultVersion, versionChainDone)
	final, err := run.program()
	run.endTrips()
	drainSignals(ctx)
	return final, err
}

// program runs the task's program, chosen by its class's session mode: code
// (and a run from before versionProcesses) the lifecycle, report the
// lifecycle's write-up (report), plan the plan loop (versionPlan).
func (t *taskRun) program() (string, error) {
	if t.processes < versionProcesses {
		return t.lifecycle()
	}
	var tc TaskConfig
	if err := workflow.ExecuteActivity(t.short, ActivityTaskConfig, t.taskID, t.cfg.PolicyVersion).Get(t.ctx, &tc); err != nil {
		return t.finish("failed", "task config: "+errMessage(err))
	}
	t.cfg.Task, t.cfg.Stages = tc, tc.Stages
	if tc.AgentTime != 0 {
		t.cfg.WallClockCap = tc.AgentTime
	}
	switch tc.Session {
	case policy.SessionCode, policy.SessionReport:
		return t.lifecycle()
	case policy.SessionPlan:
		if t.plan >= versionPlan {
			return t.planLoop()
		}
	}
	return t.finish("failed", fmt.Sprintf("session %s is not built yet", tc.Session))
}

// isReport tells a report task's run: its session mode, or for a run from
// before versionProcesses the task row's parent.
func (t *taskRun) isReport(st RunState) bool {
	if t.processes >= versionProcesses {
		return t.cfg.Task.Session == policy.SessionReport
	}
	return st.Report
}

// drainSignals drops the signals still pending when the run has decided
// (the bot merge's own pr_merged, the issue's close): they change nothing
// now. ReceiveAsync adds no command, so replay is unaffected.
func drainSignals(ctx workflow.Context) {
	for _, name := range []string{SignalPRMerged, SignalPRClosed, SignalChangesRequested, SignalIssueClosed, SignalHumanComment, SignalApproved, SignalGateBound, SignalPosterComment, SignalRollFolded, SignalDepEnded, SignalQueueTurn,
		SignalPlanAnswered, SignalSplit, SignalAnswerWhole, SignalImageReady} {
		for workflow.GetSignalChannel(ctx, name).ReceiveAsync(nil) {
		}
	}
}

// Workflow versions. A running workflow can wait days for its reviewer, so
// every change to the command sequence TaskWorkflow issues must be gated
// with workflow.GetVersion: add a version constant, branch on it, and keep
// the old branch until no workflow started before the change is still
// running (temporal workflow list --query 'ExecutionStatus="Running"').
const (
	// versionCheckinSpend (change id "checkin-spend"): a spent check-in's
	// answer verifies the open pull request's head again instead of buying
	// a session, which would stop at the same cap before any model call;
	// histories recorded before it answered by buying, so the new branch
	// is gated and the old runs keep their answer-buys-session wait.
	versionCheckinSpend workflow.Version = 1
	// versionSignalsDrained: terminal signals are acted on at every
	// decision point, change requests are coalesced, pull request signals
	// count only for the task's own PR, a terminal session error finishes
	// the task.
	versionSignalsDrained workflow.Version = 1
	// versionReconcileRace (change id "reconcile-race"): every run starts
	// with Reconcile and adopts the task's open PR instead of redoing the
	// work; RunSession and WaitChecks race the terminal signals and are
	// cancelled when one decides.
	versionReconcileRace workflow.Version = 1
	// versionSupervisor (change id "supervisor"): the supervisor's
	// verdicts. redirect resumes on the same rung with its guidance,
	// escalate hands the task to a human (Escalate, then a wait for an
	// answering issue comment, which buys one more attempt, or the issue's
	// close); a run that finds its task waiting for a human waits again.
	versionSupervisor workflow.Version = 1
	// versionReconcileFindings (change id "reconcile-findings"): a run
	// that adopts an open PR whose reviewer already asked for changes
	// (Reconcile's Findings) starts a session with them instead of
	// verifying the judged head; a change request both reconciled and
	// signalled counts once.
	versionReconcileFindings workflow.Version = 1
	// versionPersistUsage (change id "persist-usage"): a run starts from
	// the task row's usage (agent time, attempts, human grants) and
	// records it after every session and every answer, so the caps hold
	// across runs; a human answer near the cap grants a fresh slice of
	// agent time, and one after a healed wait buys an attempt like any.
	versionPersistUsage workflow.Version = 1
	// versionGates (change id "gates"): after green checks the gates run
	// (GatePolicy, GateMigration when migrations changed, GateReproduction,
	// then ModelReview), a merge approval request is opened, and the wait
	// takes the console's approved signal, on which the workflow merges as
	// factory-merge; a refused merge keeps it waiting. A forbidden risk
	// class fails the task, a failed reproduction goes back to the agent
	// like red checks.
	versionGates workflow.Version = 1
	// versionAnswers (change id "answers", ruling 118): a human's answer to
	// an escalation is kept on the run, newest first, and every later
	// session gets all of them as SessionInput.Answers instead of the
	// newest as a finding.
	versionAnswers workflow.Version = 1
	// versionGatesPinned (change id "gates-pinned"): GatePolicy gets the
	// gated commit and refuses a pull request whose head moved; the model
	// review races the terminal signals; a merge that fails other than by
	// Forgejo's refusal (after its bounded retries) keeps the approval and
	// is tried again after mergeRetryAfter.
	versionGatesPinned workflow.Version = 1
	// versionMergeFailureReported (change id "merge-failure-reported"): a
	// Merge that ends in an error (a 404, a timeout of its last attempt)
	// is reported like a failed one (ReportMergeFailure: comment and
	// timeline) and tried again after mergeRetryAfter with the same
	// approval; a newer approval cancels a pending retry timer.
	versionMergeFailureReported workflow.Version = 1
	// versionMergeRetryCap (change id "merge-retry-cap", ruling 129): the
	// retries of one approval's merge stop after maxMergeRetries (an hour):
	// one exhausted report, no timer; a new approval restarts the count.
	versionMergeRetryCap workflow.Version = 1
	// versionGateStarted (change id "gate-started", ruling 140): the
	// signal that ends the approval wait (an approval, a close, a merge, a
	// change request) carries in its timeline step when the wait
	// began: the approval request, or the last refused merge or the
	// exhausted retries that ask for a new approval.
	versionGateStarted workflow.Version = 1
	// versionGateSupervisor (change id "gate-supervisor", rulings 141,
	// 162): a long gate's activity (the session, the checks, the
	// reproduction, the model review and the merge) takes GateWatch's
	// gate_bound signal while it runs and asks SuperviseGate once per trip:
	// wait and retry act in the activity's worker, at most once each per
	// run (no wait for a session, no retry on the last attempt); cancel
	// stops the activity and fails the task with the diagnosis; escalate
	// stops it and waits for a human like the session supervisor's
	// escalation, whose answer buys a session after a session and runs a
	// gate again (a merge with its approval).
	versionGateSupervisor workflow.Version = 1
	// versionGateSupervisorRace (change id "gate-supervisor-race", ruling
	// 171): the gate supervisor's call runs beside the gate; a gate that
	// ends first wins and the late answer is dropped (SuperviseGate notes
	// it on the alert).
	versionGateSupervisorRace workflow.Version = 1
	// versionAutoMerge (change id "auto-merge", ruling 149): after the
	// model review, a pull request AutoMerge admits (the project's rule
	// auto, class outcome auto, the reproduction ran and passed, the
	// review approves, a strict improvement where not reproduced) is
	// approved as factory-gate and merged as factory-merge with no card;
	// every other one, and an auto merge Forgejo refuses or that fails,
	// gets the card with why no auto merge.
	versionAutoMerge workflow.Version = 1
	// versionNeedsInfo (change id "needs-info", ruling 150): a session
	// ending needs_info asks the issue's author (RequestInfo: the comment
	// with the mentions, waiting-poster, the info_request alert) and
	// waits for the author's or a writer's comment, which the next
	// session gets as poster information, or a change request; after
	// infoWait without one, CloseNotReproduced closes the issue and the
	// task ends closed. A healed run waiting on the author waits on for
	// the rest of infoWait.
	versionNeedsInfo workflow.Version = 1
	// versionStages (change id "stages", rulings 177, 182, 185): the gates
	// are the pinned workflow's stages in order, each with its
	// on_findings (agent: back to the agent, the second return from one
	// stage on the stronger rung, the card at the session cap; human: the
	// card; fail: the task fails); a disabled stage or one whose when does
	// not hold is recorded as skipped; the screening stage runs before the
	// first session and a suspect verdict waits on a human; each model
	// review is given the earlier ones and the card shows them; a report
	// task runs RunReport and ends done; a promoted improvement's first
	// session gets the analyzer's write-up.
	versionStages workflow.Version = 1
	// versionRoll (change id "roll", ruling 178): a roll task runs no
	// session; RollCommit writes its branch and the head goes through the
	// pull request, the checks and the gates. A fold of newer images
	// (roll_folded, which wakes the approval wait and is no finding) has
	// RollCommit write again; findings on a head RollCommit leaves as it
	// is fail the task.
	versionRoll workflow.Version = 1
	// versionSessionCap (change id "session-cap", ruling 177): a session
	// loop out of attempts, however its last round ended and a run that
	// starts at the cap too, opens the card with every finding so far, and
	// a human answer buys one more session; a redirect or no push
	// counts as a return from stage "supervisor:<trip>" ("session" for no
	// trip), recorded like a stage's findings with its attempt, and a
	// repeat past StrongerAfter takes the stronger rung; a new run starts
	// on the rung the last one left.
	versionSessionCap workflow.Version = 1
	// versionAudit (change id "audit", ruling 188): the audit stage runs
	// GateAudit on the reviewed head; its findings, or its failure, go to
	// the card beside the review, never to the agent; a roll task skips
	// it (ruling 178).
	versionAudit workflow.Version = 1
	// versionReviewVerdict (change id "review-verdict", backlog 68): a
	// review stage that ran and gave no verdict (a failed call, an invalid
	// answer, a head that moved) puts "the review gave no verdict: <why>"
	// on the card, whatever its on_findings; a roll's review (no model
	// call, ruling 178) is no finding.
	versionReviewVerdict workflow.Version = 1
	// versionChecksRerun (change id "checks-rerun"): red checks on the
	// head a session left as it was (the agent could not act on them)
	// run once more on an empty commit, RerunChecks, before their
	// findings go back to the agent; a re-run's head is not re-run.
	versionChecksRerun workflow.Version = 1
	// versionMergeConflict (change id "merge-conflict", backlog 70): a
	// merge refused because the pull request no longer merges into main
	// goes back to the agent, which merges main into the branch.
	versionMergeConflict workflow.Version = 1
	// versionPosterCap (change id "poster-cap", ruling 203): the answers
	// to information requests buy at most maxPosterAttempts attempts per
	// task; a later answer still reaches the sessions but buys none.
	versionPosterCap workflow.Version = 1
	// versionProcesses (change id "processes", ruling 206): the first
	// activity, TaskConfig, reads the task's process in the pinned version;
	// its stages replace the ones WorkflowConfig recorded, and its class's
	// session mode chooses the program.
	versionProcesses workflow.Version = 1
	// versionDepWait (change id "dep-wait", stage 11 section 1.5): a card
	// of a process with the dependencies block, before its first session,
	// waits in waiting-deps until every card it depends on is done; one
	// that ended failed or closed asks a human first.
	versionDepWait workflow.Version = 1
	// versionMergeQueue (change id "merge-queue", stage11-design.md 1.4):
	// a process with merge queue project enters the project's merge queue
	// at the approval stage and holds the turn through the approval and
	// the merge; at its turn main is merged into the head and the checks
	// run again on a head that moved; every way out leaves the queue.
	versionMergeQueue workflow.Version = 1
	// versionPlan (change id "plan", stage 11 section 1.2): a task of
	// session mode plan runs planLoop: planner sessions until a valid plan,
	// the approvers' wait on it, the cards filed on acceptance.
	versionPlan workflow.Version = 1
	// versionKeepWork (change id "keep-work", stage12-design section 3):
	// at every cap (agent time, sessions, under minSession left) the
	// branch's head past the base that no verification judged goes to
	// verify; findings from it with nothing left go to the cap card, whose
	// answer buys time; a branch with nothing past the base fails at the
	// agent-time cap as before.
	versionKeepWork workflow.Version = 1
	// versionScreenAnswers (change id "screen-answers", item 132): before
	// each session, the answers of the issue's author no screening read yet
	// go through ScreenAnswer on a task whose author is not a writer and
	// whose screening stage applies; a suspect one waits on a human as the
	// screening stage's verdict does. A healed run screens the answers it
	// rebuilt again.
	versionScreenAnswers workflow.Version = 1
	// versionAnswerSlice (change id "answer-slice", ruling 237, item 136):
	// a human answer's slice is a quarter of the task's resolved agent
	// time, never under min(minSession, WallClockCap): WorkflowConfig's
	// own AnswerSlice no longer bounds it. A run from before the change
	// keeps that field's value instead (grant).
	versionAnswerSlice workflow.Version = 1
	// versionRollGap (change id "roll-gap", ruling 269): a roll's merge
	// waits for a gap between sessions (AwaitSessionGap, whose hold keeps
	// new sessions from starting, raced by the terminal signals) and
	// ClearRollHold follows every merge, which a merged one keeps a while
	// (rollSettle); its card says so (RollGapNote).
	// Every code and plan session first waits while a roll holds sessions
	// (AwaitRollHold), outside its agent time.
	versionRollGap workflow.Version = 1
	// versionRollNoGap (the same change id, roadmap 222, item 2): sessions
	// survive factoryd's restart (RunSession reattaches to the pod from
	// the live_sessions row), so a roll merges at once after its approval
	// and no session waits for a hold. The gap, the hold and the note stay
	// for the runs of version 1.
	versionRollNoGap workflow.Version = 2
	// versionRollNoQueue (change id "roll-no-queue", roadmap 222, the
	// speed round's item 1): a roll takes no turn in its project's merge
	// queue: its stages go straight to the approval, so cards merge past
	// a roll that waits, and it merges without a sync (only rolls touch
	// k8s/*.yaml, one roll task is open per repository).
	versionRollNoQueue workflow.Version = 1
	// versionRollout (change id "roll-rollout", ruling 178): a roll's
	// merge is not its end. The run waits for the rollout the merge
	// brings — every image the merge changed live on its Deployment
	// (AwaitRollout) — and finishes done naming what went live; a rollout
	// the task's wall-clock cap passes unconfirmed fails the roll naming
	// the rollout, the cap its backstop.
	versionRollout workflow.Version = 1
	// versionCheapTurn (change id "cheap-turn", roadmap 222, item 4): at
	// the queue's turn a sync that moved the head without touching a file
	// of the pull request runs no second checks stage; the approval comes
	// at once and Merge reads the checks' green on the head they ran on
	// (checkedHead, its fifth argument).
	versionCheapTurn workflow.Version = 1
	// versionProviderWait (change id "provider-wait", ruling 267): a
	// provider_refused session is no attempt and takes no agent time: the
	// task waits on the provider (ProviderWait, then a timer raced by the
	// terminal signals) and runs the session again with the same input;
	// the providerRefusals-th refusal in a row goes to a human instead. A
	// run from before the change takes a refusal as no push.
	versionProviderWait workflow.Version = 1
	// versionPlanAnswers (change id "plan-answers", ruling 264): the plan's
	// wait takes the members' answers to its questions (SignalPlanAnswered)
	// and sends the planner back once none is open.
	versionPlanAnswers workflow.Version = 1
	// versionSessionRefusals (change id "provider-wait-sessions", roadmap
	// 155): a plan or report session the provider refused waits on the
	// provider as a code session does. A run from before takes it as a
	// plan without a last message, or a report task's failure.
	versionSessionRefusals workflow.Version = 1
	// versionClosedByHuman (change id "closed-by-human", roadmap 173): a
	// task whose issue a human closed without a merge ends closed.
	versionClosedByHuman workflow.Version = 1
	// versionFactoryPRCard (change id "factory-pr-card", roadmap 187): a
	// card for a pull request factoryd opened outside any card's workflow
	// verifies that pull request: verify takes the head it reconciled
	// straight to the checks and opens no new one.
	versionFactoryPRCard workflow.Version = 1
	// versionAlertPreCheck (change id "alert-precheck", roadmap 181, Stage
	// 18 item 3.12): an alert card checks before its first model call that
	// its alert still fires; one that stopped firing was a transient burst
	// and the card closes itself as failed with no session.
	versionAlertPreCheck workflow.Version = 1
	// versionAttemptFunded (change id "attempt-funded", ruling 322,
	// finding 215): an attempt whose task's remaining budget cannot fund
	// a call is refused before it starts — no session, no key, no failure
	// round — at the session loop's gate, wherever the answer that bought
	// it came from (the console, a comment, a healed run). The card says
	// why an answer will not run and what frees the budget.
	versionAttemptFunded workflow.Version = 1
	// versionApprovalGreen (change id "approval-green"): the approval
	// stage reads the staged head's combined status once before its
	// request; a head that is red or pending goes back to the agent
	// through the checks stage's findings, so a merge approval request
	// exists only for a head whose checks were green at its creation.
	versionApprovalGreen workflow.Version = 1
	// versionAdjudicated (change id "adjudicated", roadmap 192): an
	// approval over review findings records the overrule on the finding
	// itself — who, why, when — and a review that repeats an adjudicated
	// finding puts it on the card as evidence and bounces no session for
	// it; the approval decision keeps seeing the finding.
	versionAdjudicated workflow.Version = 1
	// versionReviewParallel (change id "review-parallel", roadmap 222,
	// item b): the checks wait schedules the model review beside it as a
	// future on its head — the local gates (policy, migration,
	// reproduction) ran before the checks, so the review's inputs are all
	// known — and the review stage awaits that future; red checks cancel
	// it before their findings return to the agent, so no review comment
	// lands on a head that goes back. Runs from before the change keep
	// the sequential review after green checks.
	versionReviewParallel workflow.Version = 1
	// versionReviewSupervised (the same change id, roadmap 245, 246): the
	// review beside the checks wait opens no gate at its schedule
	// (GateResults.Beside), so the wait holds the task's row; the review
	// stage moves the row to gate review while it awaits the review, and
	// acts on the review's gate trips as on the sequential review's, those
	// that came while another wait ran kept for it.
	versionReviewSupervised workflow.Version = 2
	// versionAnswerBurst (change id "answer-burst", roadmap 237): the wait
	// for a human's or the issue author's answer takes a comment that
	// answers as the first of a burst (awaitBurst) and resumes once the
	// answering party posted nothing for answerQuiet, at the latest
	// answerMax after the first, with every comment of the burst, which
	// t.answers keeps in the order it was written; a comment the console
	// names whole (SignalAnswerWhole) resumes it at once. A run from before
	// the change resumes on the first comment.
	versionAnswerBurst workflow.Version = 1
	// versionPlanBurst (change id "plan-burst", roadmap 237): the plan's
	// wait, and a planner session's end with findings, take a writer's
	// comment as the first of a burst (awaitBurst), so the planner goes
	// back once with every part of a change request in parts. A run from
	// before the change goes back on the first comment.
	versionPlanBurst workflow.Version = 1
	// versionRedHeadRestage (change id "red-head-restage", roadmap 202
	// with 67): a queue head whose checks are red at its merge goes back
	// through its turn instead of waiting for a human, its approval
	// request withdrawn meanwhile: main is merged in, or its red checks
	// run again on an empty commit, at most maxRestages times per queue
	// entry (restage); red checks that pass on the same tree are recorded
	// flaky, never findings. A head still red after the last re-run
	// leaves the queue and waits outside it for a human, the escalation
	// naming the checks; an answer puts it back at the queue's end.
	versionRedHeadRestage workflow.Version = 1
	// versionCheckedHead (change id "checked-head"): the approval stage's
	// green read (headGreen) reads the cheap turn's checked head, whose
	// checks hold for the synced head, not the synced head whose own
	// checks just started; and a pass ends with no checked head, so a
	// later pass reads and merges on its own head's checks.
	versionCheckedHead workflow.Version = 1
	// versionTripRouting (change id "trip-routing", roadmap 246): each
	// long gate's run has its own activity id, the activity's name and
	// its run's number (startGate), and a trip reaches the run its id
	// names alone; a trip of a run that ended is dropped, never acted on
	// against a later run of the same activity, and every trip the
	// workflow drops resolves its gate_bound alert (DropTrip). A run from
	// before the change routes trips by the activity's name and leaves a
	// dropped trip's alert open.
	versionTripRouting workflow.Version = 1
	// versionAuditBeside (change id "audit-beside", roadmap 222): the
	// checks wait schedules the security audit beside it as a future on
	// its head under the review's rule (scheduleBeside). The audit stage
	// takes its result at its turn, so the findings keep their order, and
	// supervises its trips then; red checks cancel it with the review. A
	// project that does not switch the audit on skips the stage at its
	// turn (TaskConfig.NoAudit), the call never made. Runs from before the
	// change keep the sequential audit, which finds the project's switch
	// itself.
	versionAuditBeside workflow.Version = 1
	// versionBurstAccept (change id "burst-accept", roadmap 237): the burst
	// window of a planner session's end took a plan acceptance. Plans take
	// no acceptance since roadmap 225.
	versionBurstAccept workflow.Version = 1
	// versionQueueTurns (change id "queue-turns", roadmap 202): a queue head
	// red at its merge gets its maxRestages re-runs, the turns on which main
	// did not move, whatever main did on the others, and takes at most
	// maxTurns turns per queue entry, then leaves the queue on restage's
	// escalation (leaveRed). A run from before the change counts the
	// re-runs' budget in turns, moves too, and bounds only the turns whose
	// sync moved nothing.
	versionQueueTurns workflow.Version = 1
	// versionDayCap (change id "day-cap", roadmap 240): a code, plan or
	// report session the gateway refused at a cap waits as a code session
	// waits at the month's or the project's, on a human. At the day's the
	// card escalates with the refusal and runs the next session on its own
	// once the day's free headroom funds one (DayResume, every dayCheck)
	// or the UTC day turns, which resolves its alert, or sooner on an
	// answer; a healed run keeps that wait. A session the day cap refused
	// at its first call is no attempt and takes no agent time. A split the
	// cap refused asks a human at once. A run from before the change takes
	// a code session's day cap as no push, a plan's or a split's as no
	// plan, and fails a report task's.
	versionDayCap workflow.Version = 1
	// versionFactoryPRSource (change id "factory-pr-source"): a card of a
	// pull request factoryd opened outside any card's workflow is known by
	// its source before the waiting states: whatever state the heal pass
	// restarts it in, its run takes the gates on the pull request linked
	// to it and never a session, and one with none linked fails. A run
	// from before the change answers a waiting state as a coding card does.
	versionFactoryPRSource workflow.Version = 1
	// versionPodWait (change id "pod-wait"): a code, plan, split or report
	// session whose pod never left Pending within its start's wait ran no
	// agent. It is no attempt and takes no agent time, and the next starts
	// at once with the same input, the pod's own wait having been the
	// wait; the podWaits-th in a row asks a human, naming what the pod
	// waited on (notStarted). A run from before the change takes it as no
	// push, a plan's or a split's as no plan, and fails a report task's.
	versionPodWait workflow.Version = 1
	// versionBesideLadder (change id "beside-ladder", roadmap 246): each
	// running gate has its own supervision. A trip of a gate beside the
	// checks wait (the review, the audit) is supervised when it comes,
	// beside the checks wait's own trips, each run with its own supervisor
	// call (watchRuns): the supervisor's wait and retry act at once, its
	// cancel fails the task at once, and its escalation asks the human at
	// once while the checks wait goes on (askBeside). Red checks, or any
	// other return of the head to the agent, withdraw that question, on
	// the card and the issue; green checks hand it to the gate's stage,
	// which waits for the answer and runs the gate again (besideAnswered).
	// A run from before the change keeps a beside gate's trips for its
	// stage.
	versionBesideLadder workflow.Version = 1
	// versionFactoryPRFindings (change id "factory-pr-findings"): a card of
	// a pull request factoryd opened has no agent, so the findings a stage
	// would send an agent's card back with (red checks, the review's
	// concerns, an audit's findings) go on its approval card under noAgent,
	// for a human. A run from before the change fails the card on them.
	versionFactoryPRFindings workflow.Version = 1
	// versionFactoryPRActions (change id "factory-pr-actions"): a factory
	// pull request card's approval offers what fits its head (roadmap 264):
	// red checks run again at a human's word (SignalRerunChecks) and ask
	// again with what they say, and a change request closes the card and
	// its pull request with the human's words, since no agent answers it.
	// A run from before the change hears no re-run and fails on a change
	// request.
	versionFactoryPRActions workflow.Version = 1
	// versionSessionLadder (change id "session-ladder"): the session cap
	// counts the sessions that got a model answer (SessionAnswered), read
	// after every session whatever ended it, since the session's own start
	// (a reattached session's pod start), so a starved or refused session
	// takes no number and no branch of the loop corrects the count; an
	// answer after a session that took none buys agent time but no attempt,
	// in a restarted run too (TaskUsage.LastUncounted), and the
	// unansweredSessions-th such session in a row asks a human; a restarted
	// run's answer to a floor's refusal, to pods that never started or to
	// the provider's refusals buys nothing, as the live wait's does, and
	// its answer to a gate's escalation verifies the head again from the
	// first stage (the review runs again, the approval is asked again); an
	// answer at a cap (a change request, the cap card, the cap question
	// after a restart) buys an attempt only at the session cap, whatever
	// the last session counted, else the agent time alone; the check before
	// a session is SessionFunded (the opening calls and the floor of the
	// rung); every wait for a human adds the change requests that answered
	// it to the next session's findings. A run from before counts every
	// session that ran and every attempt the budget refused, buys an attempt
	// with every answer at a cap or after a restart, funds on AttemptFunded
	// and drops the budget wait's change requests.
	versionSessionLadder workflow.Version = 1
	// versionReviewRemark (change id "review-remark"): a review that
	// answers concerns and names no finding is a remark on the card, not a
	// finding for the agent and not a session. A run from before turns it into
	// the finding "model review: <summary>".
	versionReviewRemark workflow.Version = 1
	// versionPlannerRetry (change id "planner-retry"): a plan card with
	// less than min(minSession, cap) of its agent time left gets one fresh
	// planner session with its findings, topped up to half the cap by
	// TaskUsage.RetryTime, which a human's extra time leaves alone; a cap
	// whose half is under that minimum gets none. Out of time with no such
	// session to come, the planner asks a human (plannerTimeCard), and an
	// answer to any wait then buys agent time (grantTime). A run from
	// before the change fails at the limit at once.
	versionPlannerRetry workflow.Version = 1
	// versionApprovalPending (change id "approval-pending"): a head whose
	// checks are pending at the approval stage's read — not started, not
	// settled — is not red (a request opened before they started would
	// else record them red and park the pull request un-approvable):
	// headGreen waits for their conclusion as the checks stage does,
	// within the task's checks timeout, and only the conclusion proceeds
	// — green to the request, red back with the findings, none the
	// checks stage's timeout. A run from before sends the pending checks
	// back as findings, as it did.
	versionApprovalPending workflow.Version = 1
	// versionCriteriaGate (change id "criteria-gate", roadmap 287): the
	// approval stage runs the criteria gate once before its request, on the
	// plan's integration card alone (the activity decides which card
	// is one); the criteria's findings go on the approval card, for the
	// human. A run from before asks its approval without the gate.
	versionCriteriaGate workflow.Version = 1
	// versionCheckinRaise (change id "checkin-raise"): an answer to a
	// check-in stopped at its call cap buys the card one more turn of
	// the cap (TaskUsage.ExtraCheckin), and the budget's share still
	// refuses on spent money.
	versionCheckinRaise workflow.Version = 1
	// versionCheckinGrant (change id "checkin-grant", issue 959): the
	// answer to a check-in's share stop grants the card one more share of
	// the check-in budget beside its attempt (TaskUsage.ExtraCheckinShare
	// widens the runner's share), so a long card stops once per human
	// answer and not once per check-in moment; the call cap and the card's
	// own budget stay the caps they were. A run from before the change
	// buys the attempt alone and its next check-in moment stops the card
	// again.
	versionCheckinGrant workflow.Version = 1
	// versionChainDone (change id "chain-done"): a run that recorded it
	// waited at its merge for the chain; none waits now.
	versionChainDone workflow.Version = 1
	// versionToolPark (change id "tool-park"): a session that ends in
	// missing_tool (request_tool) parks the card on an image (ParkForTool):
	// a refusal goes to the next session at once, or to a human when the
	// card was refused that image before; else the card waits for the
	// imager's image_ready, and after imageWait for a human too, and the
	// next session runs on the image with the arrival, or why it did not
	// come, in its findings. The asking session takes no number of the
	// cap, as an answered question does. A session start whose image has no
	// pin (ErrImageNotPinned) parks the same way with no tool card and takes
	// nothing. A run from before asks a human for the tool, and an unpinned
	// image fails the session as before.
	versionToolPark workflow.Version = 1

	// versionHeadHolds (change id "head-holds"): a head the agent moved
	// past its gates (the policy gate's typed head-moved refusal, a merge
	// Forgejo refused for its moved head) is adopted from the branch and
	// its checks waited on — a green one continues the gates or asks the
	// approval again, its granted request voided; a red one fails or goes
	// back to the agent as the checks stage's findings. Before, the policy
	// gate's refusal was terminal (2026-10-23: two tasks died of a retry
	// commit pushed after checks passed) and the merge's kept the approval
	// waiting. maxHeadHolds bounds the adoptions.
	versionHeadHolds workflow.Version = 1
)

// budgetRefused opens the card of a task whose remaining budget cannot
// fund a call (ruling 322): an answer would start an attempt whose first
// call the gateway must refuse, so none is granted.
const budgetRefused = "the remaining budget cannot fund a call"

// BudgetRefusal reports whether why, a refusal of AttemptFunded or
// SessionFunded, is one a raise of the budget answers: a cap and a model
// that cannot be priced refuse too, and no raise helps there.
func BudgetRefusal(why string) bool { return strings.HasPrefix(why, budgetRefused) }

// providerRefusals is how many sessions in a row the provider may refuse
// before a human hears of it, with providerRefused opening the reason.
const (
	providerRefusals = 6
	providerRefused  = "the model provider refused the first call of "
)

// providerBackoff is the wait after the nth refusal in a row: 2 minutes,
// doubling, at most 30.
func providerBackoff(n int) time.Duration {
	return min(2*time.Minute<<min(n-1, 4), 30*time.Minute)
}

// minSession is the least agent time a session starts with, or the whole
// agent time when that is shorter: pod start and the first build take one
// to two minutes.
const minSession = 5 * time.Minute

// maxPosterAttempts caps the attempts the issue author's answers buy.
const maxPosterAttempts = 2

// infoWait is how long an information request waits for the issue
// author's answer before the issue closes as not reproducible.
const infoWait = 14 * 24 * time.Hour

// answerQuiet and answerMax bound an answer in parts (versionAnswerBurst):
// an answer longer than MaxAnswer travels as several comments.
const (
	answerQuiet = 30 * time.Second
	answerMax   = 3 * time.Minute
)

type taskRun struct {
	ctx, short        workflow.Context
	cfg               WorkflowConfig
	taskID            string
	v, v2, v3, v4, v5 workflow.Version
	v6, v7, v8, v9    workflow.Version
	v10, v11, v12     workflow.Version
	v13, v14, v15     workflow.Version
	v16, v17, v18     workflow.Version
	v19, v20, v21     workflow.Version
	v22, v23          workflow.Version
	attemptFunded     workflow.Version // the attempt grant's floor (ruling 322)
	processes         workflow.Version
	depWait           workflow.Version
	mergeQueue        workflow.Version
	syncedHead        string // the head the merge queue's last sync made (versionMergeQueue)
	plan              workflow.Version
	keepWork          workflow.Version
	judged            []string // the heads a verification took up (versionKeepWork)
	gateFindings      bool     // findings wait for the check-in of the next session (plan D); not rebuilt by Reconcile
	deadFrom          string   // the session that ended without a push and without a check-in (plan E); not rebuilt by Reconcile
	lastGates         GateResults
	checkinVerdicts   []map[string]CheckVerdict // every check-in's verdicts, the priors the guards read
	answerScreening   workflow.Version
	screened          int // how many of posterInfo the screening read (versionScreenAnswers)
	answerSlice       workflow.Version
	rollGap           workflow.Version
	rollQueue         workflow.Version
	rollout           workflow.Version // a roll's end waits its rollout confirmed (versionRollout)
	cheapTurn         workflow.Version
	// checkedHead is the head whose checks hold for the synced head the
	// cheap turn merges (versionCheapTurn); "" when the merged head's own
	// checks ran
	checkedHead       string
	providerWait      workflow.Version
	planAnswers       workflow.Version
	sessionRefusals   workflow.Version
	closedByHuman     workflow.Version
	factoryPRCard     workflow.Version
	alertPreCheck     workflow.Version
	approvalGreen     workflow.Version // the approval stage's green head read (versionApprovalGreen)
	adjudicated       workflow.Version // approvals over review findings record overrules on the findings (versionAdjudicated)
	reviewParallel    workflow.Version // the review beside the checks wait (versionReviewParallel, versionReviewSupervised)
	answerBurst       workflow.Version // an answer's wait resumes after its burst (versionAnswerBurst)
	planBurst         workflow.Version // the plan's wait takes a writer's comments as a burst (versionPlanBurst)
	redRestage        workflow.Version // a queue head red at its merge is re-staged (versionRedHeadRestage)
	checkedRead       workflow.Version // headGreen reads the checked head, which ends with its pass (versionCheckedHead)
	tripRouting       workflow.Version // trips route by their run's activity id (versionTripRouting)
	auditBeside       workflow.Version // the audit beside the checks wait (versionAuditBeside)
	queueTurns        workflow.Version // every turn of a red queue head counts (versionQueueTurns)
	factoryPRSource   workflow.Version // a factory pull request card is known by its source (versionFactoryPRSource)
	besideLadder      workflow.Version // each running gate supervised on its own (versionBesideLadder)
	factoryPRFindings workflow.Version // a factory pull request card's findings go to a human (versionFactoryPRFindings)
	factoryPRActions  workflow.Version // a factory pull request card's approval fits its head (versionFactoryPRActions)
	sessionLadder     workflow.Version // the cap counts answered sessions (versionSessionLadder)
	reviewRemark      workflow.Version // concerns without findings is a remark (versionReviewRemark)
	plannerRetry      workflow.Version // a used-up planner gets one fresh session (versionPlannerRetry)
	approvalPending   workflow.Version // a pending head at the approval waits its checks out (versionApprovalPending)
	criteriaGate      workflow.Version // the criteria gate runs once before the approval's request (versionCriteriaGate)
	checkinSpend      workflow.Version // a spent check-in's answer verifies the head, not buys a session (versionCheckinSpend)
	checkinRaise      workflow.Version // a call-cap stop's answer buys one more turn of the cap (versionCheckinRaise)
	checkinGrant      workflow.Version // the answer to a check-in share stop grants a share (versionCheckinGrant)
	headHolds         workflow.Version // a moved head is held, not refused (versionHeadHolds)
	holds             int              // heads adopted this run (versionHeadHolds)
	toolPark          workflow.Version // a missing tool parks the card on an image (versionToolPark)
	gateRuns          map[string]int   // the long gates' runs so far, by activity (versionTripRouting)
	dayCap            workflow.Version // a day cap's refusal waits for the day to turn (versionDayCap)
	podWait           workflow.Version // a session whose pod never started is no attempt (versionPodWait)
	pendingPods       int              // sessions in a row whose pod never started (versionPodWait)
	pr                PullRequest
	approvalRequested time.Time // the current approval request's time (versionGateStarted)
	answers           []string  // the human's answers, newest first (versionAnswers)
	escalation        int64     // the escalation comment human answers must be newer than
	infoRequest       int64     // the information request comment the author's answers must be newer than
	posterInfo        []string  // the author's answers, oldest first (versionNeedsInfo)
	// versionStages
	standing  string         // the issue author's, for a stage's when
	lastRound bool           // the session that pushed the commit in the stages is the task's last
	round     int            // the stages' pass, from 1
	returns   map[string]int // how often each stage sent findings to the agent
	history   []string       // every finding sent to the agent, tagged with its stage and round
	reviews   []Review       // the model reviews so far, oldest first
	source    string         // the task row's arrival
	rolling   bool           // a roll task's run (versionRoll): a fold wakes the approval wait
	orphanPR  bool           // the card of a pull request factoryd opened outside a card's workflow (versionFactoryPRCard): verify opens none
	// versionChecksRerun
	redHead   string // the last head whose checks failed
	rerunHead string // the head the last RerunChecks made
	// beside are the gates scheduled beside the checks wait, in their
	// stages' order (versionReviewParallel, versionAuditBeside)
	beside []*besideRun
	// drops are the DropTrip calls endTrips joins (versionTripRouting)
	drops []workflow.Future
	// answerKeys name each answer this run took, the head of answers,
	// those Reconcile rebuilt following them (insertAnswer); anchors are
	// the escalations and information requests this run asked (answerOf)
	answerKeys []answerKey
	anchors    []anchor
}

// verdict is how one pushed commit ended: a final state, or the findings
// (and ladder rung) the next attempt starts with.
type verdict struct {
	final, reason string
	findings      []string
	stronger      bool
	// escalate is the gate supervisor's reason for a human, cancelled its
	// cancel of the activity (final failed)
	escalate  string
	cancelled bool
	// requested: the findings are a writer's change request
	requested bool
	// moved: the head the gates judged was moved past them; its findings
	// (the moved head's checks) are the checks stage's
	// (versionHeadHolds)
	moved bool
	// restage: the merge found the queue head's checks red, and its
	// turn takes it again (versionRedHeadRestage)
	restage bool
	// rerun: a human asked to run a factory pull request's red checks
	// again (versionFactoryPRActions)
	rerun bool
}

func (t *taskRun) lifecycle() (string, error) {
	drained := t.v >= versionSignalsDrained
	if err := workflow.ExecuteActivity(t.short, ActivityEnsureIssue, t.taskID).Get(t.ctx, nil); err != nil {
		return t.finish("failed", "ensure issue: "+errMessage(err))
	}
	model := t.cfg.Models.Start
	if t.cfg.Task.Stronger {
		model = t.cfg.Models.Stronger
	}
	// findings come from outside the sessions (a verification, a human, the
	// issue's author); own are the newest unpushed session's own lines,
	// which replace an earlier session's. sources names where each of
	// findings came from — the verification, the changes a human or
	// reviewer asked for — for the check-in's ask.
	var findings, own []string
	var sources []string
	var handoverNote string // a mid-session check-in's note, riding the next ask-less brief (plan E)
	setFindings := func(fs []string, from string) {
		findings, sources = fs, slices.Repeat([]string{from}, len(fs))
	}
	addFindings := func(fs []string, from string) {
		findings, sources = append(findings, fs...), append(sources, slices.Repeat([]string{from}, len(fs))...)
	}
	startRef, parent := "", ""
	var u TaskUsage
	reason := "no session produced a result"
	refusals := 0 // provider_refused sessions in a row (versionProviderWait)
	// least is the agent time a session needs to start (versionKeepWork)
	least := min(minSession, t.cfg.WallClockCap)
	// capAnswer is what an answer at a cap buys: an attempt only at the
	// session cap, else the agent time alone (versionSessionLadder)
	capAnswer := func() {
		if t.sessionLadder >= versionSessionLadder && u.Attempts < t.cfg.MaxSessions+u.ExtraAttempts {
			t.grantTime(&u)
			return
		}
		t.grant(&u)
	}
	// apply takes a verification's findings for the next session. A
	// writer's change request with no session left is the human's answer
	// (versionKeepWork, ruling 225): it buys the session, answered.
	apply := func(v verdict) (answered bool) {
		if v.stronger {
			model = t.cfg.Models.Stronger
		}
		from := "review"
		if v.requested {
			from = "changes"
		}
		setFindings(v.findings, from)
		if len(v.findings) > 0 {
			t.gateFindings = true
		}
		own, reason = nil, v.reason
		if t.keepWork >= versionKeepWork && v.requested &&
			(u.Attempts >= t.cfg.MaxSessions+u.ExtraAttempts || t.cfg.WallClockCap+u.ExtraTime+u.RetryTime-u.AgentTime < least) {
			capAnswer()
			return true
		}
		return false
	}
	// buy is what an answer after a session buys: an attempt, or agent time
	// alone after one that took no number (u.LastUncounted)
	buy := func() {
		if u.LastUncounted {
			t.grantTime(&u)
			return
		}
		t.grant(&u)
	}
	// held ends every wait for a human of this loop: the task's final state
	// when the wait decided one, else the change requests that answered it join
	// the next session's findings (the human's comments are in t.answers)
	held := func(final, why string, answer []string) (string, string) {
		if final == "" {
			addFindings(answer, "changes")
			if len(answer) > 0 {
				t.gateFindings = true
			}
		}
		return final, why
	}
	// unanswered is how many sessions in a row got no model answer
	// (versionSessionLadder); at unansweredSessions of them askUnanswered
	// asks a human, whose answer buys agent time, and counts again
	unanswered := 0
	askUnanswered := func() (final, why string) {
		if unanswered < unansweredSessions {
			return "", ""
		}
		ask := fmt.Sprintf(noModelAnswer+"%d sessions in a row ended without one and took no number of the session cap. "+
			"Find what kept the model from answering (the gateway, the provider or the session's pod), then answer to start the next session. The last stop: %s",
			unanswered, reason)
		unanswered = 0
		if final, why := held(t.awaitHuman(ask, nil)); final != "" {
			return final, why
		}
		buy()
		return "", ""
	}
	// verifyAtCap takes the branch's head past the base through verify
	// when no verification judged it (versionKeepWork): head "" for none.
	verifyAtCap := func() (head string, v verdict, answered bool) {
		if err := workflow.ExecuteActivity(t.short, ActivityBranchHead, t.taskID).Get(t.ctx, &head); err != nil {
			return "", verdict{final: "failed", reason: "branch head: " + errMessage(err)}, false
		}
		if head == "" || slices.Contains(t.judged, head) || head == t.rerunHead || head == t.syncedHead {
			return head, verdict{}, false
		}
		// the cap card, not the approval card, takes the findings
		t.lastRound, startRef = false, head
		if v = t.verify(head); v.final == "" {
			answered = apply(v)
		}
		return head, v, answered
	}
	// timeCap is the agent-time cap (versionKeepWork): a branch with nothing
	// past the base fails; else findings on its head go to the cap card,
	// whose answer buys time.
	timeCap := func() (final, why string) {
		head, v, answered := verifyAtCap()
		switch {
		case v.final != "":
			return v.final, v.reason
		case head == "":
			return "failed", "wall-clock cap reached"
		case answered:
			return "", ""
		}
		if final, why := held(t.awaitHuman(t.sessionCapCard(reason, slices.Concat(findings, own)), nil)); final != "" {
			return final, why
		}
		capAnswer()
		return "", ""
	}
	if t.v2 >= versionReconcileRace {
		var st RunState
		if err := workflow.ExecuteActivity(t.short, ActivityReconcile, t.taskID).Get(t.ctx, &st); err != nil {
			return t.finish("failed", "reconcile: "+errMessage(err))
		}
		if t.v5 >= versionPersistUsage {
			u = st.Usage
		}
		if t.v7 >= versionAnswers {
			t.answers = st.Answers
		}
		if t.v14 >= versionNeedsInfo {
			t.posterInfo, t.infoRequest = st.PosterInfo, st.InfoRequest
		}
		t.standing, t.source = st.Standing, st.Source
		if t.factoryPRCard >= versionFactoryPRCard {
			t.orphanPR = st.FactoryPR && st.PR.Number != 0
		}
		if st.PR.Number != 0 {
			t.judged = append(t.judged, st.Head)
		}
		if t.v16 >= versionStages {
			t.returns, t.history, t.reviews, t.round = st.Returns, st.History, st.Reviews, st.Round
			t.checkinVerdicts = st.CheckinVerdicts
		}
		if t.waitsForDependencies(st, u) {
			final, why, answer := t.awaitDependencies(st)
			if final != "" {
				return t.finish(final, why)
			}
			// the card starts: the first session's path below
			setFindings(answer, "changes")
			st.Status = "queued"
		}
		// alert-precheck (roadmap 181, Stage 18 item 3.12): an alert card
		// whose alert no longer fires closes itself before its first model
		// call; a transient burst files no work. A dependency wait that ran
		// here is over: only a card that would start or resume work checks.
		if t.alertPreCheck >= versionAlertPreCheck && st.Source == SourceAlert && !st.Skip && !st.Merged &&
			!st.IssueClosed && !st.PRClosed && !st.Roll && st.Status != "closing" &&
			st.Status != "waiting-human" && st.Status != "waiting-poster" && st.Status != "waiting-image" && st.PR.Number == 0 {
			var firing bool
			if err := workflow.ExecuteActivity(t.short, ActivityAlertPreCheck, t.taskID).Get(t.ctx, &firing); err != nil {
				return t.finish("failed", "alert pre-check: "+errMessage(err))
			}
			if !firing {
				return t.finish("failed", "alert no longer fires")
			}
		}
		if t.v18 >= versionSessionCap && t.climbed(st.Climbed) {
			model = t.cfg.Models.Stronger
		}
		switch {
		case st.Skip:
			return st.Status, nil
		case t.v16 >= versionStages && t.isReport(st):
			return t.report(st.Stronger, 0)
		case st.Merged:
			// a healed run of a roll whose pull request merged waits its
			// rollout like a run that merged it (versionRollout): the
			// wait needs the pull request and the roll's marks
			if st.Roll {
				t.pr, t.rolling = st.PR, true
			}
			return t.finish("done", "pull request merged")
		case st.Status == "closing":
			return t.finish("failed", "completion resumed after an interrupted run")
		case st.IssueClosed:
			return t.finish(t.closedWithoutMerge(), ReasonIssueClosed+" without merge")
		case st.PRClosed:
			return t.finish("failed", "pull request closed without merge")
		case t.v17 >= versionRoll && st.Roll:
			t.pr = st.PR
			return t.roll(st.Head)
		case t.factoryPRSource >= versionFactoryPRSource && st.FactoryPR:
			// a card for a pull request factoryd opened outside any card's
			// workflow (roadmap 187) has no session work in any state: its
			// work is the pull request linked to it, so one without it
			// (FileFactoryPRTask files none) fails
			if st.PR.Number == 0 {
				return t.finish("failed", "factory pull request: none is linked to the card")
			}
			t.pr = st.PR
			return t.factoryPR(st.Head)
		case t.v16 >= versionStages && st.Status == "waiting-human" && st.Screening:
			// the screening's human, asked again; the answer runs the next
			// session, whose rebuilt answers are screened again
			t.pr, startRef = st.PR, st.Head
			final, why, answer := t.judgeScreening(t.awaitHumanAgain(st.Question, nil))
			if final != "" {
				return t.finish(final, why)
			}
			setFindings(answer, "changes")
		case t.dayCap >= versionDayCap && st.Status == "waiting-human" && st.DayCap != "":
			// the day cap's wait, its own end included
			t.pr, startRef = st.PR, st.Head
			final, why, answer := t.awaitDay(st.DayCap, st.DayCapUntil, model)
			if final != "" {
				return t.finish(final, why)
			}
			setFindings(answer, "changes")
		case t.toolPark >= versionToolPark && st.Park != nil &&
			(st.Status == "waiting-image" || st.Status == "waiting-human" && strings.HasPrefix(st.Question, imageWaited)):
			// a card parked on its image waits on, asked again where it asked
			t.pr, startRef = st.PR, st.Head
			asked := ""
			if st.Status == "waiting-human" {
				asked = st.Question
			}
			final, why, answer, notes := t.awaitImage(*st.Park, st.Park.CreatedAt, asked)
			if final, why := held(final, why, answer); final != "" {
				return t.finish(final, why)
			}
			addFindings(notes, "image")
			if st.Park.Tool != "" {
				buy()
			}
		case t.v3 >= versionSupervisor && st.Status == "waiting-human":
			t.pr, startRef = st.PR, st.Head
			final, why, answer := t.awaitHumanAgain(st.Question, st.Choices)
			if final != "" {
				return t.finish(final, why)
			}
			setFindings(answer, "changes")
			switch {
			case t.v5 < versionPersistUsage:
			case AtSessionCap(st.Question):
				capAnswer()
			case t.sessionLadder < versionSessionLadder:
				t.grant(&u)
			case floorRefused(st.Question), strings.HasPrefix(st.Question, floorUnread),
				strings.HasPrefix(st.Question, podNeverStarted), strings.HasPrefix(st.Question, providerRefused):
				// the live loop answers these waits with the next
				// session alone
			case t.checkinRaise >= versionCheckinRaise && atCheckinCallCap(st.Question):
				// the call cap's stop: the answer buys the card the
				// wider cap beside the session, and that session runs
				// instead of stopping at the same cap before any model
				// call; placed before the spend case, for a call-cap
				// stop is a spend stop too
				u.ExtraCheckin++
				buy()
			case t.checkinSpend >= versionCheckinSpend && st.PR.Number != 0 && atCheckinSpend(st.Question):
				// a spent check-in's answer verifies again, as the gate's
				// stop does, but with no session: a bought one would stop
				// at the same cap before any model call, so the answer
				// rides the findings the review weighs instead
				t.lastRound = u.Attempts >= t.cfg.MaxSessions+u.ExtraAttempts
				v := t.verify(st.Head)
				if v.final != "" {
					return t.finish(v.final, v.reason)
				}
				apply(v)
			case t.checkinGrant >= versionCheckinGrant && AtCheckinShareStop(st.Question):
				// the share stop (issue 959): the answer grants the card
				// one more share of the check-in budget beside the
				// session it buys, so the next check-in moment runs where
				// this one stopped the card; a run from before the change
				// replays its answer-buys-session wait below, unanswered
				t.grantCheckin(&u, true)
				buy()
			case st.PR.Number != 0 && (atGateStop(st.Question) || atQueueLeft(st.Question) || atReproStart(st.Question)):
				// a gate's answer verifies again, as the live gate does:
				// here from the first stage, change requests back to the agent
				if len(answer) > 0 {
					apply(verdict{findings: answer, reason: "changes requested", requested: true})
					break
				}
				t.lastRound = u.Attempts >= t.cfg.MaxSessions+u.ExtraAttempts
				v := t.verify(st.Head)
				if v.final != "" {
					return t.finish(v.final, v.reason)
				}
				apply(v)
			default:
				buy()
			}
		case t.v14 >= versionNeedsInfo && st.Status == "waiting-poster":
			t.pr, startRef = st.PR, st.Head
			// an answer signalled before this run is in st.PosterInfo, and
			// its buffered signal is not counted twice: the run goes on
			if !st.InfoAnswered {
				final, why, answer := t.waitPoster(st.InfoRequestedAt)
				if final != "" {
					return t.finish(final, why)
				}
				setFindings(answer, "changes")
			}
			t.grantPoster(&u)
		case t.factoryPRCard >= versionFactoryPRCard && st.FactoryPR && st.PR.Number != 0:
			// a card for a pull request factoryd opened outside any card's
			// workflow (roadmap 187): like a roll it holds no session — its
			// work is the pull request linked to it — and takes the gates
			// from running, not straight from queued. Unlike a roll no
			// RollCommit writes its branch, the pull request exists already.
			// Only a run from before versionFactoryPRSource comes here.
			t.pr = st.PR
			return t.factoryPR(st.Head)
		case t.v4 >= versionReconcileFindings && st.PR.Number != 0 && len(st.Findings) > 0:
			// the reviewer asked for changes on the pushed head and no
			// session took them up: start one
			t.pr, startRef = st.PR, st.Head
			setFindings(st.Findings, "changes")
		case st.PR.Number != 0:
			// the task's work is pushed and in review: verify it, no new session
			t.pr, startRef = st.PR, st.Head
			t.lastRound = u.Attempts >= t.cfg.MaxSessions+u.ExtraAttempts
			v := t.verify(st.Head)
			if v.final != "" {
				return t.finish(v.final, v.reason)
			}
			apply(v)
		case t.v16 >= versionStages && u.Attempts == 0:
			final, why, more := t.screen()
			if final != "" {
				return t.finish(final, why)
			}
			addFindings(more, "changes")
		}
	}
	// taken is how many numbers the budget's refusals took since the last
	// session, in a run from before versionSessionLadder
	taken := 0
	for {
		attempt := u.Attempts + 1 + taken
		if t.attemptFunded >= versionAttemptFunded {
			// the session is refused before it starts: no session, no key,
			// no failure round, and the card says why an answer will not run
			if why := t.unfunded(model); why != "" {
				if DayCapped(why) {
					// a spent day cap resets on its own: the day cap's
					// wait, not a human's, and no session ran
					_, state, _ := strings.Cut(why, "\n")
					until := dayTurn(workflow.Now(t.ctx))
					if final, end := held(t.awaitDay(dayCapRefused(dayCapFloor, state, until), until, model)); final != "" {
						return t.finish(final, end)
					}
					continue
				}
				if t.sessionLadder >= versionSessionLadder {
					if final, end := held(t.awaitHuman(why, nil)); final != "" {
						return t.finish(final, end)
					}
					continue
				}
				taken++
				if final, end, _ := t.awaitHuman(why, nil); final != "" {
					return t.finish(final, end)
				}
				continue
			}
		}
		if attempt > t.cfg.MaxSessions+u.ExtraAttempts {
			if t.v18 < versionSessionCap {
				break
			}
			answered := false
			if t.keepWork >= versionKeepWork {
				var v verdict
				if _, v, answered = verifyAtCap(); v.final != "" {
					return t.finish(v.final, v.reason)
				}
			}
			if !answered {
				if final, why := held(t.awaitHuman(t.sessionCapCard(reason, slices.Concat(findings, own)), nil)); final != "" {
					return t.finish(final, why)
				}
				t.grant(&u)
			}
		}
		if drained {
			if final, why, ok := t.decided(); ok {
				return t.finish(final, why)
			}
			for _, f := range t.drainChanges() {
				if t.v4 < versionReconcileFindings || !slices.Contains(findings, f) {
					addFindings([]string{f}, "changes")
					t.gateFindings = true
				}
			}
		}
		remaining := t.cfg.WallClockCap + u.ExtraTime + u.RetryTime - u.AgentTime
		if t.keepWork >= versionKeepWork && remaining < least {
			if final, why := timeCap(); final != "" {
				return t.finish(final, why)
			}
			remaining = t.cfg.WallClockCap + u.ExtraTime + u.RetryTime - u.AgentTime
		}
		if remaining <= 0 {
			return t.finish("failed", "wall-clock cap reached")
		}
		if final, why := held(t.screenAnswers()); final != "" {
			return t.finish(final, why)
		}
		if err := workflow.ExecuteActivity(t.short, ActivitySetState, t.taskID, "running").Get(t.ctx, nil); err != nil {
			return t.finish("failed", "set running: "+errMessage(err))
		}
		if v, decided := t.awaitRollHold(); decided {
			return t.finish(v.final, v.reason)
		}
		began := workflow.Now(t.ctx)
		var res SessionResult
		in := SessionInput{
			TaskID: t.taskID, Attempt: attempt, Sessions: t.cfg.MaxSessions + u.ExtraAttempts, Model: model,
			Findings: slices.Concat(findings, own), Answers: t.answers, PosterInfo: t.posterInfo, StartRef: startRef, ParentSessionID: parent,
			PolicyVersion: t.cfg.PolicyVersion, AgentTime: t.cfg.Task.AgentTime, CheckIn: t.cfg.Task.CheckIn,
		}
		checkinPending := (t.gateFindings || t.deadFrom != "") && t.cfg.Task.CheckIn > 0
		var gates *GateResults
		if len(t.reviews) > 0 {
			g := t.lastGates
			gates = &g
		}
		in.LastGates = gates
		if checkinPending {
			switch {
			case t.gateFindings:
				in.CheckInAsk = t.checkinAsk(parent, slices.Concat(findings, own),
					append(slices.Clone(sources), slices.Repeat([]string{"session"}, len(own))...), t.lastGates)
			default:
				in.CheckInAsk = t.deadAsk(reason, gates)
			}
		} else {
			in.Handover = handoverNote
		}
		v, decided, err := t.race(t.sessionOptions(remaining), &res, ActivityRunSession, in)
		t.gateFindings, t.deadFrom, handoverNote = false, "", ""
		if t.toolPark >= versionToolPark && !decided && imageNotPinned(err) {
			// no sandbox started: no attempt, no agent time, the same input
			// once the image is pinned
			final, why, answer, notes := t.parkOnImage("", "")
			if final, why := held(final, why, answer); final != "" {
				return t.finish(final, why)
			}
			addFindings(notes, "image")
			continue
		}
		if t.podWait >= versionPodWait && err == nil && !decided && res.Outcome == OutcomeNotStarted {
			// no agent ran: no attempt, no agent time, the same input after
			if ask := t.notStarted(res.Summary); ask != "" {
				if final, why := held(t.awaitHuman(ask, nil)); final != "" {
					return t.finish(final, why)
				}
			}
			continue
		}
		t.pendingPods = 0
		if t.providerWait >= versionProviderWait && err == nil && !decided && res.Outcome == OutcomeProviderRefused {
			if ask := t.providerRefused(&refusals, ProviderWait{Status: res.Status, Reason: res.Reason}); ask != "" {
				if final, why := held(t.awaitHuman(ask, nil)); final != "" {
					return t.finish(final, why)
				}
			}
			continue
		}
		dayCapped := t.dayCap >= versionDayCap && err == nil && !decided && res.Outcome == OutcomeDayCap && res.Capped != nil && res.Acceptance == nil
		if dayCapped && !res.Capped.Allowed {
			// refused at its first call: no attempt, the same input after
			if final, why := held(t.awaitCap(*res.Capped, model)); final != "" {
				return t.finish(final, why)
			}
			continue
		}
		if !res.StartFailed {
			t.providerAnswered(&refusals, err)
			u.AgentTime += workflow.Now(t.ctx).Sub(began)
		}
		u.LastUncounted = t.sessionLadder >= versionSessionLadder && !t.sessionAnswered(res, began, attempt, model)
		if u.LastUncounted {
			unanswered++
		} else {
			u.Attempts, taken, unanswered = attempt, 0, 0
		}
		t.recordUsage(u)
		if decided && v.escalate != "" {
			// a human's answer to a stopped session buys the next one
			if final, why := held(t.awaitHuman(v.escalate, nil)); final != "" {
				return t.finish(final, why)
			}
			buy()
			continue
		}
		if decided {
			return t.finish(v.final, v.reason)
		}
		if isWallClockTimeout(err) {
			if t.keepWork < versionKeepWork {
				return t.finish("failed", "wall-clock cap reached")
			}
			reason = "wall-clock cap reached"
			if final, why := timeCap(); final != "" {
				return t.finish(final, why)
			}
			continue
		}
		if drained && isTerminal(err) {
			return t.finish("failed", "session: "+errMessage(err))
		}
		last := attempt >= t.cfg.MaxSessions+u.ExtraAttempts
		if err != nil {
			reason = "session failed: " + errMessage(err)
			if final, why := askUnanswered(); final != "" {
				return t.finish(final, why)
			}
			continue
		}
		parent = res.SessionID
		if res.Model != "" {
			model = res.Model
		}
		reason = res.Summary
		if res.CheckIn != nil {
			t.recordCheckin(res)
			if len(res.CheckIn.Verdicts) > 0 {
				t.checkinVerdicts = append(t.checkinVerdicts, res.CheckIn.Verdicts)
				if len(t.reviews) > 0 {
					t.reviews[len(t.reviews)-1].Verdicts = res.CheckIn.Verdicts
				}
			}
		}
		if res.Acceptance != nil && res.Outcome != OutcomePushed {
			// the session ran one of the plan's acceptance commands and it
			// exited 0 (roadmap 208): the fact closes the card, no model
			// verdict, no further session and no answer owed. A pushed
			// session keeps its verification, where the same commands run
			// again on the branch it pushed.
			return t.finish("done", acceptanceReason(res.Acceptance))
		}
		// a session that ends without a push hands on the findings it was
		// given with its own lines: the next one may be the first to act on
		// them, and an earlier session's lines are stale. One that never
		// started has no lines of its own; a pushed one's commit goes to
		// verification, whose apply drops them.
		midNote := res.CheckIn != nil && res.CheckIn.Record != nil && res.CheckIn.Record.Moment == CheckInSession
		if !res.StartFailed {
			own = res.Findings
			if !checkinPending && !midNote {
				// the check-in read these; its judgment replaces them
				own = slices.Concat(own, t.handoverLines(res))
			}
			if res.CheckIn == nil {
				// the check-in did not run: the round ended dead, and its
				// stop reason asks the next session's check-in (plan E)
				switch res.Outcome {
				case OutcomeNoPush, OutcomeRedirect, OutcomeRetryStronger, OutcomeWallClock:
					t.deadFrom = res.SessionID
				case OutcomeDayCap:
					if res.Capped == nil || res.Capped.Allowed {
						t.deadFrom = res.SessionID
					}
				}
			} else if midNote {
				handoverNote = res.CheckIn.Handover
			}
		}
		if dayCapped {
			// it made calls before the cap refused one: an attempt
			if final, why := held(t.awaitCap(*res.Capped, model)); final != "" {
				return t.finish(final, why)
			}
			continue
		}
		switch res.Outcome {
		case OutcomeWallClock:
			if t.keepWork < versionKeepWork {
				return t.finish("failed", "wall-clock cap reached")
			}
			if final, why := timeCap(); final != "" {
				return t.finish(final, why)
			}
			continue
		case OutcomeRetryStronger:
			// at the top rung this is a redirect: Stronger stays Stronger
			model = t.cfg.Models.Stronger
			if t.v18 >= versionSessionCap {
				t.sessionReturn(res, attempt, last)
			}
			if final, why := askUnanswered(); final != "" {
				return t.finish(final, why)
			}
			continue
		case OutcomeCheckinHuman:
			var moment string
			if res.CheckIn.Record != nil {
				moment = res.CheckIn.Record.Moment
			}
			if final, why := held(t.awaitHuman("check-in: "+cmp.Or(res.CheckIn.ReasonKind, cmp.Or(moment, "gate"))+": "+res.Reason, nil)); final != "" {
				return t.finish(final, why)
			}
			// the call cap's stop grants the card its wider cap with the
			// session (versionCheckinRaise) and falls through to buy(),
			// never the verify break: the conversation the raise buys
			// runs where the cap stopped it
			if t.checkinRaise >= versionCheckinRaise && res.CheckIn.ReasonKind == CheckSpend && res.Reason == checkinCallCapStop {
				u.ExtraCheckin++
			} else if t.checkinSpend >= versionCheckinSpend && res.CheckIn.ReasonKind == CheckSpend && t.pr.Number != 0 && startRef != "" {
				// a spent check-in with the pull request open verifies the
				// head it judged below, like a dropped check-in: a bought
				// session would stop at the same cap before any model call
				// while the head sits waiting on the approval
				res.Commit = startRef
				break
			} else if t.checkinGrant >= versionCheckinGrant && res.CheckIn.ReasonKind == CheckSpend && res.CheckIn.Reason == CheckinShareSpent {
				// the share stop (issue 959): the answer grants the card
				// one more share of the check-in budget beside the session
				// it buys (versionCheckinGrant), so the next check-in
				// moment runs where this one stopped the card
				t.grantCheckin(&u, true)
			}
			buy()
			continue
		case OutcomeCheckinDropped:
			// every finding dropped with evidence: the head verifies again
			// below on the drop record, and only the re-review's own approve
			// merges; a finding it re-raises comes back as checkin_human
			// through the runner's re-raise guard. The check-in's pod pushed
			// nothing: the head it judged is startRef.
			res.Commit = startRef
		case OutcomeNoPush, OutcomeRedirect, OutcomeProviderRefused, OutcomeDayCap, OutcomeNotStarted:
			if t.v18 >= versionSessionCap {
				if t.sessionReturn(res, attempt, last) {
					model = t.cfg.Models.Stronger
				}
			}
			if final, why := askUnanswered(); final != "" {
				return t.finish(final, why)
			}
			continue
		case OutcomeMissingTool:
			if t.toolPark < versionToolPark {
				if final, why := held(t.awaitHuman(res.Reason, nil)); final != "" {
					return t.finish(final, why)
				}
				buy()
				continue
			}
			final, why, answer, notes := t.parkOnImage(res.Tool, res.Need)
			if final, why := held(final, why, answer); final != "" {
				return t.finish(final, why)
			}
			addFindings(notes, "image")
			buy()
			continue
		case OutcomeNeedsInfo, OutcomeEscalate:
			if res.Outcome == OutcomeNeedsInfo && t.v14 >= versionNeedsInfo {
				if final, why := held(t.awaitPoster(res.Reason)); final != "" {
					return t.finish(final, why)
				}
				t.grantPoster(&u)
				continue
			}
			// a run from before request_info takes it as an escalation
			if t.v3 < versionSupervisor {
				return t.finish("failed", "supervisor escalated: "+res.Reason)
			}
			if final, why := held(t.awaitHuman(res.Reason, res.Choices)); final != "" {
				return t.finish(final, why)
			}
			buy()
			continue
		}
		startRef = res.Commit
		if drained {
			if final, why, ok := t.decided(); ok {
				return t.finish(final, why)
			}
		}
		t.lastRound = last
		v = t.verify(res.Commit)
		if v.final != "" {
			return t.finish(v.final, v.reason)
		}
		apply(v)
	}
	return t.finish("failed", fmt.Sprintf("%d attempts exhausted; last: %s", t.cfg.MaxSessions+u.ExtraAttempts, reason))
}

// providerRefused takes a session the provider refused: the task waits
// on the provider, or at the providerRefusals-th refusal in a row ask is
// what to ask a human, and the count starts again.
func (t *taskRun) providerRefused(refusals *int, w ProviderWait) (ask string) {
	*refusals++
	if *refusals == 1 {
		workflow.GetLogger(t.ctx).Info("the model provider refused a session's first call; the task waits on it", "task", t.taskID, "status", w.Status, "reason", w.Reason)
	}
	if *refusals < providerRefusals {
		w.Backoff = providerBackoff(*refusals)
		t.waitOnProvider(w)
		return ""
	}
	ask = fmt.Sprintf(providerRefused+"%d sessions in a row (status %d): %s", *refusals, w.Status, w.Reason)
	*refusals = 0
	return ask
}

// podWaits is how many sessions in a row may find their pod never
// started before the card asks a human.
const podWaits = 3

// unansweredSessions is how many sessions in a row may get no model answer
// before the card asks a human, with noModelAnswer opening the question
// (versionSessionLadder).
const (
	unansweredSessions = 3
	noModelAnswer      = "No session got a model answer: "
)

// podNeverStarted opens the question of a card whose pods never started.
const podNeverStarted = "No node started the session's pod: "

// handoverLines are what the next session is told beside a session's stop
// lines when that ended without a push: the commit its branch holds and
// whether the gates judged it, and the agent's last message. The check-in
// agent's handover note replaces them.
func (t *taskRun) handoverLines(res SessionResult) []string {
	var lines []string
	if res.Commit != "" {
		judged := "the gates have not judged it"
		if slices.Contains(t.judged, res.Commit) {
			judged = "the gates have judged it"
		}
		lines = append(lines, fmt.Sprintf("the branch holds %s; %s", res.Commit, judged))
	}
	if res.LastMessage != "" {
		lines = append(lines, lastMessageLine+res.LastMessage)
	}
	return lines
}

// lastMessageLine opens the handover line of the agent's last message,
// which the brief fences as untrusted (briefText).
const lastMessageLine = "the agent's last message: "

// checkinAsk is what the next session's check-in answers (plan D): the
// findings waiting on it, each with where it came from and the priors its
// guards read, the last review's verdict, and the gates that raised them.
// parent: the session whose work the findings judge.
func (t *taskRun) checkinAsk(parent string, findings, sources []string, g GateResults) *CheckInAsk {
	ask := &CheckInAsk{Moment: CheckInGate, ParentSession: parent, Gates: &g}
	for i, f := range findings {
		c := CheckFinding{Text: f}
		if i < len(sources) {
			c.Source = sources[i]
		}
		for _, verdicts := range t.checkinVerdicts {
			switch verdicts[f].State {
			case CheckConfirmed:
				c.PriorConfirmed++
			case CheckDropped:
				c.PriorDropped++
			}
		}
		ask.Findings = append(ask.Findings, c)
	}
	if vs := lastVerdict(t.reviews); vs != nil {
		ask.Review = &vs[0]
	}
	return ask
}

// deadAsk is the dead moment's ask (plan E): no findings wait, the stop
// reason is what the last session stopped on, and the handover is the ask.
func (t *taskRun) deadAsk(stop string, gates *GateResults) *CheckInAsk {
	return &CheckInAsk{Moment: CheckInDead, ParentSession: t.deadFrom, Stop: stop, Gates: gates}
}

// recordCheckin keeps a check-in on the timeline beside the review it
// judged, and its handover note as its own step: a new run rebuilds the
// reviews' verdicts from the checkin steps (Reconcile).
func (t *taskRun) recordCheckin(res SessionResult) {
	ck := res.CheckIn
	detail := map[string]any{
		"round": len(t.reviews), "decision": ck.Decision, "reason_kind": ck.ReasonKind, "reason": ck.Reason,
	}
	if len(ck.Verdicts) > 0 {
		detail["verdicts"] = ck.Verdicts
	}
	if ck.Record != nil {
		detail["record"] = ck.Record
	}
	if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "checkin", detail).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("record check-in", "error", err)
	}
	if ck.Decision != CheckHandover {
		return
	}
	handover := map[string]any{"handover": ck.Handover, "session": res.SessionID}
	if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "handover", handover).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("record handover", "error", err)
	}
}

// notStarted takes a session whose pod never left Pending, why its
// summary, which names what the pod waited on (versionPodWait): the next
// session starts at once, or at the podWaits-th in a row ask is what to
// ask a human, and the count starts again.
func (t *taskRun) notStarted(why string) (ask string) {
	t.pendingPods++
	workflow.GetLogger(t.ctx).Info("a session's pod never started; the next starts at once", "task", t.taskID, "in_a_row", t.pendingPods, "why", why)
	if t.pendingPods < podWaits {
		return ""
	}
	t.pendingPods = 0
	return fmt.Sprintf(podNeverStarted+"%d sessions in a row waited for it, and no agent ran. Free what it waits on (a node's cpu, "+
		"memory or ephemeral storage, or the registry of its image), then answer to start the next session. The last wait: %s", podWaits, why)
}

// providerAnswered ends the refusals in a row after a session that made
// calls: one that ended, or ran into the wall clock.
func (t *taskRun) providerAnswered(refusals *int, err error) {
	if *refusals > 0 && (err == nil || isWallClockTimeout(err)) {
		workflow.GetLogger(t.ctx).Info("the model provider answers again", "task", t.taskID, "refusals", *refusals)
		*refusals = 0
	}
}

// waitOnProvider records the wait, which puts the task in gate provider,
// and waits it out unless a terminal signal comes first.
func (t *taskRun) waitOnProvider(w ProviderWait) {
	if err := workflow.ExecuteActivity(t.short, ActivityProviderWait, t.taskID, w).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("record the provider wait", "task", t.taskID, "error", err)
	}
	timerCtx, cancel := workflow.WithCancel(t.ctx)
	defer cancel()
	sel := workflow.NewSelector(t.ctx)
	for _, n := range []string{SignalPRMerged, SignalPRClosed, SignalIssueClosed} {
		sel.AddReceive(workflow.GetSignalChannel(t.ctx, n), func(workflow.ReceiveChannel, bool) {})
	}
	sel.AddFuture(workflow.NewTimer(timerCtx, w.Backoff), func(workflow.Future) {})
	sel.Select(t.ctx)
}

// roll runs a roll task from head, its open pull request's head ("" for
// none): each round RollCommit writes what the branch lacks and the head
// goes through verify. Change requests pending before a round are taken
// by its RollCommit; findings on a head it leaves as it is end the task.
func (t *taskRun) roll(head string) (string, error) {
	t.rolling = true
	var last verdict
	for {
		if final, why, ok := t.decided(); ok {
			return t.finish(final, why)
		}
		t.drainFolds()
		last.findings = append(last.findings, t.drainChanges()...)
		if err := workflow.ExecuteActivity(t.short, ActivitySetState, t.taskID, "running").Get(t.ctx, nil); err != nil {
			return t.finish("failed", "set running: "+errMessage(err))
		}
		var next string
		if err := workflow.ExecuteActivity(t.short, ActivityRollCommit, t.taskID).Get(t.ctx, &next); err != nil {
			return t.finish("failed", "roll commit: "+errMessage(err))
		}
		if next == head && len(last.findings) > 0 {
			reason := last.reason
			if reason == "" {
				reason = "changes requested"
			}
			return t.finish("failed", "roll: "+reason+": "+strings.Join(last.findings, "; "))
		}
		head = next
		if last = t.verify(head); last.final != "" {
			return t.finish(last.final, last.reason)
		}
	}
}

// factoryPR runs a card for a pull request factoryd opened outside any
// card's workflow (roadmap 187) from the head of the pull request linked
// to it: like a roll's run it holds no session and takes the gates from
// running (set running, then verify sets in-review and runs the stages).
// Unlike a roll it runs no RollCommit — nothing writes its branch, the
// pull request exists already — and no agent answers its findings: they go
// to the human's approval (versionFactoryPRFindings), and a human's change
// request ends the task where a roll's next round would write what it asks:
// it closes the card and its pull request with the human's words
// (versionFactoryPRActions), as Finish closes a closed card's issue
// (verify opens no pull request: the linked one is taken, t.orphanPR).
func (t *taskRun) factoryPR(head string) (string, error) {
	if final, why, ok := t.decided(); ok {
		return t.finish(final, why)
	}
	if err := workflow.ExecuteActivity(t.short, ActivitySetState, t.taskID, "running").Get(t.ctx, nil); err != nil {
		return t.finish("failed", "set running: "+errMessage(err))
	}
	last := t.verify(head)
	if last.final != "" {
		return t.finish(last.final, last.reason)
	}
	if last.requested && t.factoryActions() {
		return t.finish("closed", factoryPRClosed+": "+strings.Join(last.findings, "; "))
	}
	reason := last.reason
	if reason == "" {
		reason = "changes requested"
	}
	return t.finish("failed", "factory pull request: "+reason+": "+strings.Join(last.findings, "; "))
}

// factoryPRClosed heads the reason of a factory pull request card a change
// request closed (versionFactoryPRActions).
const factoryPRClosed = "no agent answers a change request on a pull request factoryd opened, so it closes"

// factoryActions: the card's approval offers what fits a factory pull
// request's head (versionFactoryPRActions).
func (t *taskRun) factoryActions() bool {
	return t.orphanPR && t.factoryPRActions >= versionFactoryPRActions
}

// drainReruns takes the pending re-runs of the checks and reports whether
// one counts.
func (t *taskRun) drainReruns() bool {
	rerun := false
	var sig TaskSignal
	for workflow.GetSignalChannel(t.ctx, SignalRerunChecks).ReceiveAsync(&sig) {
		rerun = t.counted(SignalRerunChecks, sig) || rerun
	}
	return rerun
}

// drainFolds takes the pending roll_folded signals; RollCommit writes what
// they announce.
func (t *taskRun) drainFolds() bool {
	folded := false
	for workflow.GetSignalChannel(t.ctx, SignalRollFolded).ReceiveAsync(nil) {
		folded = true
	}
	return folded
}

// climbed: an earlier run left the task on the stronger rung, by a
// retry_stronger (stronger) or a return past StrongerAfter.
//
// strongerAfter is the task's: its config's when set, else the policy's.
func (t *taskRun) strongerAfter() int {
	if t.cfg.Task.StrongerAfter > 0 {
		return t.cfg.Task.StrongerAfter
	}
	return t.cfg.StrongerAfter
}

func (t *taskRun) climbed(stronger bool) bool {
	for _, n := range t.returns {
		if n > t.strongerAfter() {
			return true
		}
	}
	return stronger
}

// unfunded is the session's floor: "" when the task's remaining budget can
// fund the session on model, else the refusal the card carries
// (SessionFunded; a run from before versionSessionLadder asks
// AttemptFunded). A failed read is no refusal: the per-call hold still
// guards the money.
func (t *taskRun) unfunded(model string) string {
	activity := ActivityAttemptFunded
	if t.sessionLadder >= versionSessionLadder {
		activity = ActivitySessionFunded
	}
	var why string
	if err := workflow.ExecuteActivity(t.short, activity, t.taskID, model).Get(t.ctx, &why); err != nil {
		workflow.GetLogger(t.ctx).Warn("read the attempt's funding", "task", t.taskID, "error", err)
		return ""
	}
	return why
}

// sessionAnswered reports whether the session that started on model got a
// model answer (versionSessionLadder), and records its start. The read runs
// from the session's own start when its result carries one, which for a
// session reattached after a restart lies before began. A failed read
// counts the session: a broken ledger must not loosen the cap.
func (t *taskRun) sessionAnswered(res SessionResult, began time.Time, attempt int, model string) bool {
	var ok bool
	in := SessionAnswerInput{TaskID: t.taskID, SessionID: res.SessionID, Model: model, Attempt: attempt, Since: cmp.Or(res.Started, began)}
	if err := workflow.ExecuteActivity(t.short, ActivitySessionAnswered, in).Get(t.ctx, &ok); err != nil {
		workflow.GetLogger(t.ctx).Warn("read whether the session got an answer", "task", t.taskID, "error", err)
		return true
	}
	return ok
}

// grant is what a human answer buys; only a human raises a limit. One
// more attempt, and grantTime's agent time.
func (t *taskRun) grant(u *TaskUsage) {
	u.ExtraAttempts++
	t.grantTime(u)
}

// grantCheckin is the share of the check-in budget an answer to its share
// stop buys beside the attempt (versionCheckinGrant, issue 959): one more
// of the policy's CheckinShareOf widths, so the stretch the answer pays
// for can run its own check-in moments and a long card stops once per
// human answer, not once per check-in moment. The card's own budget and
// the check-in call cap stay the caps they were; the call cap's stop buys
// its own raise (versionCheckinRaise) and grants nothing here. The usage
// is recorded with the attempt the same answer buys.
func (t *taskRun) grantCheckin(u *TaskUsage, share bool) {
	if share && t.checkinGrant >= versionCheckinGrant {
		u.ExtraCheckinShare++
	}
}

// grantTime is an answer's agent time: when less than answerTime of agent
// time is left, the difference, so the attempt is not born at the cap.
func (t *taskRun) grantTime(u *TaskUsage) {
	if t.v5 < versionPersistUsage {
		return
	}
	if left, slice := t.cfg.WallClockCap+u.ExtraTime+u.RetryTime-u.AgentTime, t.answerTime(); left < slice {
		u.ExtraTime += slice - left
	}
	t.recordUsage(*u)
}

// answerTime is the agent time an answer leaves the next session: a
// quarter of the resolved agent time, never under the 5 minutes a session
// needs to start (versionAnswerSlice); a run from before the change id
// keeps WorkflowConfig.AnswerSlice instead.
func (t *taskRun) answerTime() time.Duration {
	if t.answerSlice >= versionAnswerSlice {
		return max(t.cfg.WallClockCap/4, min(minSession, t.cfg.WallClockCap))
	}
	return min(t.cfg.AnswerSlice, t.cfg.WallClockCap)
}

// grantPoster is grant for an answer to an information request, up to
// maxPosterAttempts per task (versionPosterCap).
func (t *taskRun) grantPoster(u *TaskUsage) {
	if t.v22 >= versionPosterCap {
		if u.PosterAttempts >= maxPosterAttempts {
			return
		}
		u.PosterAttempts++
	}
	t.grant(u)
}

// recordUsage puts the usage on the task row for the next run; the row is
// bookkeeping, so a failure (the workflow cancelled mid-retry) is logged,
// not the task's end.
func (t *taskRun) recordUsage(u TaskUsage) {
	if t.v5 < versionPersistUsage {
		return
	}
	if err := workflow.ExecuteActivity(t.short, ActivityRecordUsage, t.taskID, u).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("record usage", "task", t.taskID, "error", err)
	}
}

// verify takes a pushed commit through its pull request, the checks and
// the review wait. A card for a pull request factoryd opened outside any
// card's workflow (roadmap 187) has that pull request already: with
// versionFactoryPRCard and RunState's FactoryPR set, verify skips
// OpenPullRequest and takes the head straight to the stages.
func (t *taskRun) verify(commit string) verdict {
	t.judged = append(t.judged, commit)
	if t.factoryPRCard >= versionFactoryPRCard && t.orphanPR {
		if err := workflow.ExecuteActivity(t.short, ActivitySetState, t.taskID, "in-review").Get(t.ctx, nil); err != nil {
			return verdict{final: "failed", reason: "set in-review: " + errMessage(err)}
		}
		return t.stages(commit)
	}
	if err := workflow.ExecuteActivity(t.short, ActivityOpenPullRequest, t.taskID, commit).Get(t.ctx, &t.pr); err != nil {
		return verdict{final: "failed", reason: "open pull request: " + errMessage(err)}
	}
	if err := workflow.ExecuteActivity(t.short, ActivitySetState, t.taskID, "in-review").Get(t.ctx, nil); err != nil {
		return verdict{final: "failed", reason: "set in-review: " + errMessage(err)}
	}
	if t.v16 >= versionStages {
		return t.stages(commit)
	}
	var checks ChecksResult
	v, decided, err := t.gateActivity(true, t.checksOptions(), &checks, ActivityWaitChecks, t.taskID, commit)
	if decided {
		return v
	}
	if err != nil {
		return verdict{final: "failed", reason: "wait for checks: " + errMessage(err)}
	}
	if t.v >= versionSignalsDrained {
		if final, why, ok := t.decided(); ok {
			return verdict{final: final, reason: why}
		}
	}
	switch checks.State {
	case ChecksFailure:
		return verdict{findings: checks.Findings, stronger: true, reason: "checks failed"}
	case ChecksTimeout:
		return verdict{final: "failed", reason: fmt.Sprintf("checks did not finish within %s", t.cfg.ChecksTimeout)}
	}
	if t.v6 >= versionGates {
		return t.gate(commit)
	}
	await := t.awaitDecision
	if t.v < versionSignalsDrained {
		await = t.reviewV0
	}
	final, why, requested := await()
	if final != "" {
		return verdict{final: final, reason: why}
	}
	return verdict{findings: requested, reason: "changes requested", requested: true}
}

// gate takes a commit with green checks through the gates, the model
// review and the human's approval to its merge.
func (t *taskRun) gate(commit string) verdict {
	// before versionStages the gates run after green checks only
	g := GateResults{ChecksPassed: true}
	policyArgs := []any{t.taskID, t.pr.Number}
	if t.v8 >= versionGatesPinned {
		policyArgs = append(policyArgs, commit)
	}
	if err := workflow.ExecuteActivity(t.short, ActivityGatePolicy, policyArgs...).Get(t.ctx, &g.Policy); err != nil {
		return verdict{final: "failed", reason: "policy gate: " + errMessage(err)}
	}
	if g.Policy.Outcome == policy.OutcomeForbidden {
		return verdict{final: "failed", reason: fmt.Sprintf("policy version %d: risk class %s is forbidden to agents: %s",
			g.Policy.PolicyVersion, g.Policy.Class, strings.Join(g.Policy.Paths, ", "))}
	}
	if len(g.Policy.Migrations) > 0 {
		if err := workflow.ExecuteActivity(t.short, ActivityGateMigration, t.taskID, t.pr.Number, g.Policy.Migrations).Get(t.ctx, nil); err != nil {
			return verdict{final: "failed", reason: "migration gate: " + errMessage(err)}
		}
	}
	v, decided, err := t.reproduction(&g.Reproduction, commit)
	if decided {
		return v
	}
	if err != nil {
		return verdict{final: "failed", reason: "reproduction gate: " + errMessage(err)}
	}
	if !g.Reproduction.Passed() {
		return verdict{stronger: true, reason: "reproduction failed", findings: []string{fmt.Sprintf(
			"the task's reproduction command failed (exit %d) against %s: %s\noutput tail:\n%s",
			g.Reproduction.Exit, commit, g.Reproduction.Command, g.Reproduction.Output)}}
	}
	var review Review
	g.Pin = t.cfg.PolicyVersion
	if t.v8 >= versionGatesPinned {
		v, decided, err := t.gateActivity(true, reviewOptions(), &review, ActivityModelReview, t.taskID, t.pr, commit, g)
		if decided {
			return v
		}
		if err != nil {
			review = Review{Note: "model review failed: " + errMessage(err)}
		}
	} else {
		reviewCtx := workflow.WithActivityOptions(t.ctx, reviewOptions())
		if err := workflow.ExecuteActivity(reviewCtx, ActivityModelReview, t.taskID, t.pr, commit, g).Get(t.ctx, &review); err != nil {
			review = Review{Note: "model review failed: " + errMessage(err)}
		}
	}
	if final, why, ok := t.decided(); ok {
		return verdict{final: final, reason: why}
	}
	var whyHuman []string
	if t.v13 >= versionAutoMerge {
		auto, met, unmet := AutoMerge(g, review, t.source)
		if !auto {
			whyHuman = unmet
		} else {
			v, done, failed := t.autoMerge(commit, g, review, met)
			if done {
				return v
			}
			whyHuman = []string{failed}
		}
	}
	// an approval or a split signalled before this request (for an earlier
	// head) is not one of this one
	var sig TaskSignal
	for workflow.GetSignalChannel(t.ctx, SignalApproved).ReceiveAsync(&sig) {
		t.recordIgnored(SignalApproved, sig, "sent before this approval request")
	}
	for workflow.GetSignalChannel(t.ctx, SignalSplit).ReceiveAsync(nil) {
	}
	if err := workflow.ExecuteActivity(t.short, ActivityRequestApproval, t.taskID,
		ApprovalInput{PR: t.pr, Commit: commit, Gates: g, Review: review, WhyHuman: whyHuman}).Get(t.ctx, nil); err != nil {
		return verdict{final: "failed", reason: "request approval: " + errMessage(err)}
	}
	if t.v11 >= versionGateStarted {
		t.approvalRequested = workflow.Now(t.ctx)
	}
	return t.awaitApproval(commit, nil)
}

// autoMerge merges on the project's merge rule auto (versionAutoMerge):
// the note on the timeline, then the gate's approval naming met and the
// merge as factory-merge. done with the task's verdict, or why it did not
// go through: the card then.
func (t *taskRun) autoMerge(commit string, g GateResults, rv Review, met []string) (v verdict, done bool, failed string) {
	if err := workflow.ExecuteActivity(t.short, ActivityAutoMergeNote, t.taskID, t.pr.Number, commit, g, rv).Get(t.ctx, nil); err != nil {
		return verdict{}, false, "the auto merge's note failed: " + errMessage(err)
	}
	var res MergeResult
	v, decided, err := t.gateActivity(false, mergeOptions(), &res, ActivityMerge, t.mergeArgs(commit, AutoApprover, met)...)
	switch {
	case decided:
		return v, true, ""
	case err != nil:
		return verdict{}, false, "the auto merge failed: " + errMessage(err)
	case res.Merged && res.By != "":
		return verdict{final: "done", reason: "pull request merged by " + res.By}, true, ""
	case res.Merged:
		return verdict{final: "done", reason: "pull request merged by factory-merge on the merge rule auto"}, true, ""
	case res.Conflict && t.v23 >= versionMergeConflict:
		return conflictVerdict(res.Refusal), true, ""
	case res.RedHead && t.restageable():
		return verdict{restage: true, reason: res.Refusal}, true, ""
	case res.RedHead:
		return verdict{}, false, res.Refusal
	case res.Refusal != "":
		return verdict{}, false, "Forgejo refused the auto merge: " + res.Refusal
	}
	return verdict{}, false, "the auto merge failed: " + res.Failed
}

// awaitApproval blocks, at no cost, until a terminal signal decides the
// task, a reviewer requests changes, a member's split files its cards
// (versionSplit), a member re-runs a factory pull request's checks
// (versionFactoryPRActions), or an approval merges the pull request as
// factory-merge. An approval over findings the card showed adjudicates
// them (versionAdjudicated). A merge Forgejo refuses keeps the wait, but
// a queue head's red checks take it back through its turn while re-runs
// are left (versionRedHeadRestage); one that fails otherwise keeps the
// approval and is tried again after mergeRetryAfter (versionGatesPinned).
func (t *taskRun) awaitApproval(commit string, covered []string) verdict {
	approver := ""
	var retry workflow.Future
	var cancelRetry workflow.CancelFunc
	failures := 0 // failed retries of the current approval's merge (versionMergeRetryCap)
	for {
		retried := false
		sel := workflow.NewSelector(t.ctx)
		for _, n := range []string{SignalPRMerged, SignalPRClosed, SignalIssueClosed, SignalChangesRequested, SignalApproved, SignalSplit} {
			sel.AddReceive(workflow.GetSignalChannel(t.ctx, n), func(workflow.ReceiveChannel, bool) {})
		}
		if t.rolling {
			sel.AddReceive(workflow.GetSignalChannel(t.ctx, SignalRollFolded), func(workflow.ReceiveChannel, bool) {})
		}
		if t.factoryActions() {
			sel.AddReceive(workflow.GetSignalChannel(t.ctx, SignalRerunChecks), func(workflow.ReceiveChannel, bool) {})
		}
		if retry != nil {
			sel.AddFuture(retry, func(workflow.Future) { retried = true })
		}
		sel.Select(t.ctx)
		if final, why, ok := t.decided(); ok {
			return verdict{final: final, reason: why}
		}
		if findings := t.drainChanges(); len(findings) > 0 {
			return verdict{findings: findings, reason: "changes requested", requested: true}
		}
		if t.rolling && t.drainFolds() {
			return verdict{reason: "newer images folded in"}
		}
		if t.factoryActions() && t.drainReruns() {
			return verdict{rerun: true, reason: "the checks run again"}
		}
		var split TaskSignal
		if workflow.GetSignalChannel(t.ctx, SignalSplit).ReceiveAsync(&split) {
			if v, done := t.split(split); done {
				return v
			}
			continue
		}
		if t.v8 < versionGatesPinned {
			approver = ""
		}
		var sig TaskSignal
		signalled := false
		for workflow.GetSignalChannel(t.ctx, SignalApproved).ReceiveAsync(&sig) {
			if t.counted(SignalApproved, sig) {
				approver, signalled = sig.Author, true
			}
		}
		if approver == "" || !signalled && !retried {
			continue
		}
		// a newer approval replaces a pending retry: its timer goes, and
		// its merge gets a fresh retry count
		if cancelRetry != nil && !retried {
			cancelRetry()
		}
		if signalled {
			failures = 0
			t.adjudicate(covered, sig)
		}
		cancelRetry = nil
		retry = nil
		opts := finishOptions()
		if t.v8 >= versionGatesPinned {
			opts = mergeOptions()
		}
		gap := t.rolling && t.rollGap == versionRollGap
		if gap {
			if v, decided := t.awaitSessionGap(); decided {
				return v
			}
			// a roll_folded that landed during the gap wait: the wait's
			// race takes no roll_folded, so the loop's own drain ran
			// before it was sent; the merge goes only to the head the
			// fold's round stages
			if t.rolling && t.drainFolds() {
				return verdict{reason: "newer images folded in"}
			}
		}
		var res MergeResult
		v, decided, err := t.gateActivity(false, opts, &res, ActivityMerge, t.mergeArgs(commit, approver, nil)...)
		if gap {
			if err := workflow.ExecuteActivity(t.short, ActivityClearRollHold, t.taskID, res.Merged).Get(t.ctx, nil); err != nil {
				workflow.GetLogger(t.ctx).Warn("clear roll hold", "task", t.taskID, "error", err)
			}
		}
		if decided {
			return v
		}
		if err != nil {
			workflow.GetLogger(t.ctx).Warn("merge", "task", t.taskID, "error", err)
			if t.v9 < versionMergeFailureReported {
				continue
			}
			if err := workflow.ExecuteActivity(t.short, ActivityReportMergeFailure, t.taskID, t.pr.Number, commit, approver, errMessage(err)).Get(t.ctx, nil); err != nil {
				workflow.GetLogger(t.ctx).Warn("report merge failure", "task", t.taskID, "error", err)
			}
			res = MergeResult{Failed: errMessage(err)}
		}
		switch {
		case res.Merged && res.By != "":
			return verdict{final: "done", reason: "pull request merged by " + res.By}
		case res.Merged:
			return verdict{final: "done", reason: "pull request merged by factory-merge after " + approver + "'s approval"}
		case res.Failed != "" && t.v10 >= versionMergeRetryCap && failures >= maxMergeRetries:
			if err := workflow.ExecuteActivity(t.short, ActivityReportMergeRetriesExhausted, t.taskID, t.pr.Number, commit, approver).Get(t.ctx, nil); err != nil {
				workflow.GetLogger(t.ctx).Warn("report merge retries exhausted", "task", t.taskID, "error", err)
			}
			approver = ""
			if t.v11 >= versionGateStarted {
				t.approvalRequested = workflow.Now(t.ctx)
			}
		case res.Failed != "" && t.v9 >= versionMergeFailureReported:
			failures++
			timerCtx, cancel := workflow.WithCancel(t.ctx)
			retry, cancelRetry = workflow.NewTimer(timerCtx, mergeRetryAfter), cancel
		case res.Failed != "":
			retry = workflow.NewTimer(t.ctx, mergeRetryAfter)
		case res.MovedHead && t.headHolds >= versionHeadHolds:
			// Merge voided the granted approval for the moved head
			// (versionHeadHolds)
			return verdict{moved: true, reason: res.Refusal}
		case res.Conflict && t.v23 >= versionMergeConflict:
			return conflictVerdict(res.Refusal)
		case res.RedHead && t.restageable():
			return verdict{restage: true, reason: res.Refusal}
		default:
			// refused: a new approval wait
			approver = ""
			if t.v11 >= versionGateStarted {
				t.approvalRequested = workflow.Now(t.ctx)
			}
		}
	}
}

// awaitSessionGap is a roll's wait for a session gap before its merge
// (versionRollGap); a failed wait merges as before it.
func (t *taskRun) awaitSessionGap() (verdict, bool) {
	v, decided, err := t.race(rollWaitOptions(), nil, ActivityAwaitSessionGap, t.taskID)
	if err != nil {
		workflow.GetLogger(t.ctx).Warn("await session gap", "task", t.taskID, "error", err)
	}
	return v, decided
}

// awaitRollHold waits before a session while a roll holds sessions
// (versionRollGap), outside the session's agent time; a failed wait lets
// the session start.
func (t *taskRun) awaitRollHold() (verdict, bool) {
	if t.rollGap != versionRollGap {
		return verdict{}, false
	}
	v, decided, err := t.race(rollWaitOptions(), nil, ActivityAwaitRollHold, t.taskID)
	if err != nil {
		workflow.GetLogger(t.ctx).Warn("await roll hold", "task", t.taskID, "error", err)
	}
	return v, decided
}

// rollWaitOptions: a roll's waits end at their own bound (gapBound), at
// most the longest agent time any policy allows and rollGapSlack; they
// heartbeat every poll, and a factoryd that stops (a roll's merge) has
// the next one retry them.
func rollWaitOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: policy.MaxAgentTime + rollGapSlack + rollWaitSlack,
		HeartbeatTimeout:    time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        10 * time.Second,
			MaximumInterval:        time.Minute,
			MaximumAttempts:        10,
			NonRetryableErrorTypes: []string{errTerminal},
		},
	}
}

// mergeArgs are Merge's arguments: the checked head, set only under
// versionCheapTurn, and met, the conditions the merge gate found on the
// merge rule auto.
func (t *taskRun) mergeArgs(commit, approver string, met []string) []any {
	return []any{t.taskID, t.pr.Number, commit, approver, t.checkedHead, met}
}

// maxMergeRetries bounds the retries of one approval's merge: with
// mergeRetryAfter, an hour (ruling 129).
const maxMergeRetries = 6

// mergeRetryAfter is how long a merge that failed (Forgejo unreachable,
// not refusing) waits before the kept approval is tried again.
const mergeRetryAfter = 10 * time.Minute

// conflictVerdict sends a pull request that no longer merges into main
// back to the agent. The worker bases its commit on the main commit the
// merge brings in, so main's changes, .forgejo/ among them, stay main's.
func conflictVerdict(refusal string) verdict {
	return verdict{reason: "the pull request no longer merges into main", findings: []string{
		"the pull request no longer merges into main: main moved under it (Forgejo: " + refusal + "). " +
			"Merge main into the branch with `git merge origin/main` (the clone's origin/main is main at this session's start), " +
			"resolve every conflict keeping both main's changes and this task's, run the tests of the packages you touched, and end the session. " +
			"Do not rebase, and leave .forgejo/ as the merge brings it from main."}}
}

// mergeOptions: a merge is retried a few times; its last attempt reports
// the failure (timeline, comment) instead of failing, and the workflow
// tries again later with the same approval.
func mergeOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout:    2 * time.Minute,
		ScheduleToCloseTimeout: 20 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        10 * time.Second,
			MaximumInterval:        time.Minute,
			MaximumAttempts:        mergeAttempts,
			NonRetryableErrorTypes: []string{errTerminal},
		},
	}
}

const mergeAttempts = 5

// recordIgnored records a received signal that does not count.
func (t *taskRun) recordIgnored(name string, sig TaskSignal, why string) {
	detail := map[string]any{"signal": name, "author": sig.Author, "pr": sig.PR, "ignored": why}
	if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "signal", detail).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("record signal", "error", err)
	}
}

// reproOptions: the reproduction activity heartbeats while its pod runs;
// a dead factoryd's attempt is retried once on the next.
func reproOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout:    reproActivityTimeout,
		ScheduleToCloseTimeout: 2*reproActivityTimeout + time.Minute,
		HeartbeatTimeout:       2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        10 * time.Second,
			MaximumAttempts:        2,
			NonRetryableErrorTypes: []string{errTerminal},
		},
	}
}

// reproActivityTimeout bounds one reproduction attempt: two runs, at the
// base and at the head, each a pod start (an image pull) and the command,
// at most MaxReproTimeout.
const reproActivityTimeout = 2 * (MaxReproTimeout + 10*time.Minute)

// MaxReproTimeout is the longest reproduction command run.
const MaxReproTimeout = 15 * time.Minute

// requestPlanOptions: asking for a plan also drafts hold-outs, which is
// two model calls and up to two dry runs; it heartbeats while they run.
func requestPlanOptions() workflow.ActivityOptions {
	budget := 2*(reviewChunks*policy.MaxSupervisorTimeout) + 2*(MaxReproTimeout+10*time.Minute) + 10*time.Minute
	return workflow.ActivityOptions{
		HeartbeatTimeout:       2 * time.Minute,
		StartToCloseTimeout:    budget,
		ScheduleToCloseTimeout: 2 * budget,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        10 * time.Second,
			MaximumAttempts:        3,
			NonRetryableErrorTypes: []string{errTerminal},
		},
	}
}

// holdoutOptions: one settlement runs an audit session per red hold-out
// (each bounded as a review) and at most one dry run (bounded as a
// reproduction); it heartbeats while it works.
func holdoutOptions() workflow.ActivityOptions {
	budget := 3*(reviewChunks*policy.MaxSupervisorTimeout) + reproActivityTimeout + 10*time.Minute
	return workflow.ActivityOptions{
		HeartbeatTimeout:       2 * time.Minute,
		StartToCloseTimeout:    budget,
		ScheduleToCloseTimeout: 2 * budget,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        10 * time.Second,
			MaximumAttempts:        3,
			NonRetryableErrorTypes: []string{errTerminal},
		},
	}
}

// reviewOptions: one review or audit makes up to reviewChunks calls, each
// bounded by the policy's supervisor_timeout, at most
// policy.MaxSupervisorTimeout; it heartbeats, so a cancel reaches the call
// and a lost worker is noticed.
func reviewOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		HeartbeatTimeout:       2 * time.Minute,
		StartToCloseTimeout:    reviewChunks*policy.MaxSupervisorTimeout + 2*time.Minute,
		ScheduleToCloseTimeout: 3*reviewChunks*policy.MaxSupervisorTimeout + 10*time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        10 * time.Second,
			MaximumAttempts:        3,
			NonRetryableErrorTypes: []string{errTerminal},
		},
	}
}

// race runs a long activity. From versionReconcileRace on it races the
// terminal signals: when one decides the task, the activity is cancelled
// (RunSession then deletes its pod and namespace and revokes its
// credentials; the workflow waits for that) and the decision returned.
// From versionGateSupervisor on it takes the gate supervisor's trips too.
func (t *taskRun) race(opts workflow.ActivityOptions, out any, activity string, args ...any) (verdict, bool, error) {
	if t.v2 < versionReconcileRace {
		return verdict{}, false, workflow.ExecuteActivity(workflow.WithActivityOptions(t.ctx, opts), activity, args...).Get(t.ctx, out)
	}
	return t.watch(true, opts, out, activity, args...)
}

// gateActivity runs a long gate's activity: WaitChecks, GateReproduction
// and ModelReview race the terminal signals (terminal), Merge does not.
// From versionGateSupervisor on it takes the gate supervisor's trips, and
// a human's answer to its escalation runs the gate again with the same
// arguments (a merge keeps its approval); a change request sent meanwhile
// goes back to the agent.
func (t *taskRun) gateActivity(terminal bool, opts workflow.ActivityOptions, out any, activity string, args ...any) (verdict, bool, error) {
	if !terminal && t.v12 < versionGateSupervisor {
		return verdict{}, false, workflow.ExecuteActivity(workflow.WithActivityOptions(t.ctx, opts), activity, args...).Get(t.ctx, out)
	}
	for {
		var v verdict
		var decided bool
		var err error
		if terminal {
			v, decided, err = t.race(opts, out, activity, args...)
		} else {
			v, decided, err = t.watch(false, opts, out, activity, args...)
		}
		if !decided || v.escalate == "" {
			return v, decided, err
		}
		if v, stop := t.gateEscalated(v.escalate); stop {
			return v, true, nil
		}
	}
}

// reproduction runs the reproduction gate on commit into out: a sandbox
// that could not start asks the human (its start escalation), and the
// answer runs the gate again, as gateEscalated's.
func (t *taskRun) reproduction(out *ReproResult, commit string) (verdict, bool, error) {
	for {
		*out = ReproResult{}
		v, decided, err := t.gateActivity(true, reproOptions(), out, ActivityGateReproduction, t.taskID, t.pr.Number, commit)
		if decided || err != nil || out.Escalate == "" {
			return v, decided, err
		}
		if v, stop := t.gateEscalated(out.Escalate); stop {
			return v, true, nil
		}
	}
}

// criteria runs the criteria gate on commit into out: a sandbox that
// could not start asks the human (its start escalation), and the answer
// runs the gate again, as gateEscalated's. A hold-out red or unrunnable
// at the head goes to the settlement activity before the gate's verdict
// stands: its escalation or its findings decide the card, anything else
// runs the gate again.
func (t *taskRun) criteria(out *CriteriaResult, commit string) (verdict, bool, error) {
	for {
		*out = CriteriaResult{}
		v, decided, err := t.gateActivity(true, reproOptions(), out, ActivityGateCriteria, t.taskID, t.pr.Number, commit)
		if err != nil {
			return v, decided, err
		}
		if decided {
			return v, true, nil
		}
		if out.Escalate != "" {
			if v, stop := t.gateEscalated(out.Escalate); stop {
				return v, true, nil
			}
			continue
		}
		if len(out.Holdouts) == 0 {
			return v, decided, nil
		}
		var res HoldoutSettlement
		v, decided, err = t.gateActivity(true, holdoutOptions(), &res, ActivityHoldoutSettlement, t.taskID, t.cfg.PolicyVersion, t.pr.Number, commit, out.Holdouts)
		if err != nil {
			return v, decided, err
		}
		if decided {
			return v, true, nil
		}
		switch {
		case res.Ask != "":
			if v, stop := t.gateEscalated(res.Ask); stop {
				return v, true, nil
			}
		case len(res.Findings) > 0:
			return verdict{findings: res.Findings, reason: "a real gap the audit found", requested: true}, true, nil
		}
		continue // settled, or the answer said run it again: the gate runs again
	}
}

// gateEscalated waits on the human a gate supervisor's escalation asks:
// stop with the task's verdict when the answer decides it (a close, or a
// change request back to the agent), else the task is in review again and
// the gate runs again.
func (t *taskRun) gateEscalated(reason string) (verdict, bool) {
	return t.gateAnswered(t.awaitHuman(reason, nil))
}

// gateAnswered goes on from the human's answer to a gate's escalation, as
// gateEscalated describes.
func (t *taskRun) gateAnswered(final, why string, changes []string) (verdict, bool) {
	switch {
	case final != "":
		return verdict{final: final, reason: why}, true
	case len(changes) > 0:
		return verdict{findings: changes, reason: "changes requested", requested: true}, true
	}
	if err := workflow.ExecuteActivity(t.short, ActivitySetState, t.taskID, "in-review").Get(t.ctx, nil); err != nil {
		return verdict{final: "failed", reason: "set in-review: " + errMessage(err)}, true
	}
	return verdict{}, false
}

// watch runs the activity until it returns, a terminal signal decides the
// task (terminal), or the gate supervisor stops it. decided: the verdict
// is the task's, the activity cancelled and waited for.
func (t *taskRun) watch(terminal bool, opts workflow.ActivityOptions, out any, activity string, args ...any) (verdict, bool, error) {
	supervised := t.v12 >= versionGateSupervisor
	if supervised {
		// trips of runs that ended before this one: none is its own; a
		// gate beside the checks wait keeps its own, the rest are dropped
		var trip GateTrip
		for workflow.GetSignalChannel(t.ctx, SignalGateBound).ReceiveAsync(&trip) {
			t.keepTrip(trip)
		}
	}
	return t.watchRun(terminal, supervised, t.startGate(opts, activity, args...), out)
}

// startGate schedules a long gate's activity, which a cancel waits for.
// From versionTripRouting on the run's activity id is the activity's name
// and the run's number among its runs, which the run's trips carry.
func (t *taskRun) startGate(opts workflow.ActivityOptions, activity string, args ...any) *gateRun {
	ctx, cancel := workflow.WithCancel(t.ctx)
	opts.WaitForCancellation = true
	if t.tripRouting >= versionTripRouting {
		if t.gateRuns == nil {
			t.gateRuns = map[string]int{}
		}
		t.gateRuns[activity]++
		opts.ActivityID = fmt.Sprintf("%s-%d", activity, t.gateRuns[activity])
	}
	return &gateRun{activity: activity, opts: opts, args: args, cancel: cancel,
		future: workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, opts), activity, args...)}
}

// watchRun waits for run as watch describes; supervised: it takes the gate
// supervisor's trips of run, those kept for it first, and from
// versionBesideLadder on those of the gates beside the checks wait too
// (watchRuns).
func (t *taskRun) watchRun(terminal, supervised bool, run *gateRun, out any) (verdict, bool, error) {
	if supervised && t.besideLadder >= versionBesideLadder {
		return t.watchRuns(terminal, run, out)
	}
	trips := workflow.GetSignalChannel(t.ctx, SignalGateBound)
	var asking workflow.Future // the supervisor's call on trip (versionGateSupervisorRace)
	var trip GateTrip
	for {
		done, answered := false, false
		sel := workflow.NewSelector(t.ctx)
		sel.AddFuture(run.future, func(workflow.Future) { done = true })
		if terminal {
			for _, n := range []string{SignalPRMerged, SignalPRClosed, SignalIssueClosed} {
				sel.AddReceive(workflow.GetSignalChannel(t.ctx, n), func(workflow.ReceiveChannel, bool) {})
			}
		}
		switch {
		case asking != nil:
			sel.AddFuture(asking, func(workflow.Future) { answered = true })
		case supervised && len(run.trips) > 0:
			// a trip kept for it is taken at once
			sel.AddDefault(func() {})
		case supervised:
			sel.AddReceive(trips, func(workflow.ReceiveChannel, bool) {})
		}
		sel.Select(t.ctx)
		if done {
			return verdict{}, false, run.future.Get(t.ctx, out)
		}
		if terminal {
			if final, reason, ok := t.decided(); ok {
				run.cancel()
				_ = run.future.Get(t.ctx, nil)
				return verdict{final: final, reason: reason}, true, nil
			}
		}
		if answered {
			d := t.gateAnswer(asking)
			asking = nil
			if v, stop := t.actOnGate(trip, d, run); stop {
				run.cancel()
				_ = run.future.Get(t.ctx, nil)
				return v, true, nil
			}
			continue
		}
		for asking == nil && supervised && run.nextTrip(trips, &trip) {
			if !run.owns(trip) {
				t.keepTrip(trip)
				continue
			}
			ask := t.askGate(trip, run)
			if t.v15 >= versionGateSupervisorRace {
				asking = ask
				break
			}
			d := t.gateAnswer(ask)
			if run.future.IsReady() {
				continue
			}
			if v, stop := t.actOnGate(trip, d, run); stop {
				run.cancel()
				_ = run.future.Get(t.ctx, nil)
				return v, true, nil
			}
		}
	}
}

// watchRuns waits for run as watchRun does, from versionBesideLadder on:
// run and each gate still running beside the checks wait have their own
// supervision, a trip asked about when it comes in its run's own
// supervisor call, so no run's trip waits for another run's answer. A
// beside gate's wait and retry act on it at once and its cancel ends the
// task, as run's do; its escalation asks the human at once while run goes
// on (askBeside). A beside gate's answer that comes after its call ended
// changes nothing, as run's (ruling 171).
func (t *taskRun) watchRuns(terminal bool, run *gateRun, out any) (verdict, bool, error) {
	trips := workflow.GetSignalChannel(t.ctx, SignalGateBound)
	for {
		runs := []*gateRun{run}
		for _, f := range t.beside {
			if f.gateRun != run && f.asked == "" && !f.future.IsReady() {
				runs = append(runs, f.gateRun)
			}
		}
		done, kept := false, false
		var answered *gateRun
		sel := workflow.NewSelector(t.ctx)
		sel.AddFuture(run.future, func(workflow.Future) { done = true })
		if terminal {
			for _, n := range []string{SignalPRMerged, SignalPRClosed, SignalIssueClosed} {
				sel.AddReceive(workflow.GetSignalChannel(t.ctx, n), func(workflow.ReceiveChannel, bool) {})
			}
		}
		for _, r := range runs {
			if r.asking != nil {
				sel.AddFuture(r.asking, func(workflow.Future) { answered = r })
			}
			kept = kept || r.asking == nil && len(r.trips) > 0
		}
		if kept {
			// a trip kept for a run is taken at once
			sel.AddDefault(func() {})
		} else {
			sel.AddReceive(trips, func(workflow.ReceiveChannel, bool) {})
		}
		sel.Select(t.ctx)
		if done {
			return verdict{}, false, run.future.Get(t.ctx, out)
		}
		if terminal {
			if final, reason, ok := t.decided(); ok {
				run.cancel()
				_ = run.future.Get(t.ctx, nil)
				return verdict{final: final, reason: reason}, true, nil
			}
		}
		if answered != nil {
			d := t.gateAnswer(answered.asking)
			answered.asking = nil
			if answered != run && answered.future.IsReady() {
				continue
			}
			v, stop := t.actOnGate(answered.trip, d, answered)
			if stop && answered != run && v.escalate != "" {
				v, stop = t.askBeside(answered, v.escalate)
			}
			if stop {
				run.cancel()
				_ = run.future.Get(t.ctx, nil)
				return v, true, nil
			}
			continue
		}
		for _, r := range runs {
			if r.asking == nil && len(r.trips) > 0 {
				r.trip, r.trips = r.trips[0], r.trips[1:]
				r.asking = t.askGate(r.trip, r)
			}
		}
		var trip GateTrip
		for trips.ReceiveAsync(&trip) {
			i := slices.IndexFunc(runs, func(r *gateRun) bool { return r.owns(trip) })
			switch {
			case i < 0:
				t.keepTrip(trip)
			case runs[i].asking != nil:
				runs[i].trips = append(runs[i].trips, trip)
			default:
				runs[i].trip, runs[i].asking = trip, t.askGate(trip, runs[i])
			}
		}
	}
}

// gateRun is one run of a long gate's activity: the call, the trips kept
// for it while the workflow awaited another activity (a gate beside the
// checks wait) or while its supervisor call ran, that call on trip
// (watchRuns), and what it used of the gate supervisor.
type gateRun struct {
	activity        string
	opts            workflow.ActivityOptions
	args            []any
	future          workflow.Future
	cancel          workflow.CancelFunc
	trips           []GateTrip
	asking          workflow.Future
	trip            GateTrip
	waited, retried bool
}

// owns: trip is one of the run's, by the activity id the run has
// (versionTripRouting), else by the activity's name.
func (r *gateRun) owns(trip GateTrip) bool {
	if r.opts.ActivityID != "" {
		return trip.ActivityID == r.opts.ActivityID
	}
	return trip.Activity == r.activity
}

// nextTrip is the run's next trip: one kept for it, else the channel's.
func (r *gateRun) nextTrip(trips workflow.ReceiveChannel, trip *GateTrip) bool {
	if len(r.trips) == 0 {
		return trips.ReceiveAsync(trip)
	}
	*trip, r.trips = r.trips[0], r.trips[1:]
	return true
}

// dropTrips resolves the gate_bound alert of each trip no run takes
// (versionTripRouting): its run ended before the gate supervisor took
// it. The calls run beside the workflow, so a store that does not answer
// holds no wait; endTrips joins them. A run from before the change
// leaves the alerts open.
func (t *taskRun) dropTrips(trips ...GateTrip) {
	if t.tripRouting < versionTripRouting {
		return
	}
	for _, trip := range trips {
		t.drops = append(t.drops, workflow.ExecuteActivity(t.short, ActivityDropTrip, t.taskID, trip))
	}
}

// endTrips drops the trips no run took, their runs ended with the task,
// and waits for every drop, so none is abandoned with the workflow.
func (t *taskRun) endTrips() {
	for {
		var trip GateTrip
		for workflow.GetSignalChannel(t.ctx, SignalGateBound).ReceiveAsync(&trip) {
			t.dropTrips(trip)
		}
		if len(t.drops) == 0 {
			return
		}
		for _, f := range t.drops {
			if err := f.Get(t.ctx, nil); err != nil {
				workflow.GetLogger(t.ctx).Warn("resolve a dropped trip's alert", "task", t.taskID, "error", err)
			}
		}
		t.drops = nil
	}
}

// askGate starts SuperviseGate on one trip of the running activity.
func (t *taskRun) askGate(trip GateTrip, run *gateRun) workflow.Future {
	// a session's wait would trip again past its own start-to-close, and
	// so would any wait whose doubled bound outlasts the attempt (a merge:
	// 2 x 90 s against 2 min); a retry on the last attempt would fail the
	// activity
	maxAttempts := run.opts.RetryPolicy.MaximumAttempts
	in := GateSupervision{Trip: trip, Input: run.args, CanWait: !run.waited && trip.Activity != ActivityRunSession && 2*trip.Bound < run.opts.StartToCloseTimeout,
		CanRetry: !run.retried && (maxAttempts == 0 || trip.Attempt < maxAttempts), PolicyVersion: t.cfg.PolicyVersion}
	return workflow.ExecuteActivity(workflow.WithActivityOptions(t.ctx, reviewOptions()), ActivitySuperviseGate, t.taskID, in)
}

// gateStopped opens the gate supervisor's escalation.
const gateStopped = "The gate supervisor stopped "

// gateAnswer waits for SuperviseGate's decision; a failed call escalates.
func (t *taskRun) gateAnswer(ask workflow.Future) GateDecision {
	var d GateDecision
	if err := ask.Get(t.ctx, &d); err != nil {
		return GateDecision{Action: ActionEscalate, Diagnosis: "the gate supervisor failed: " + errMessage(err),
			Recommendation: "look at the activity and its alert yourself"}
	}
	return d
}

// actOnGate takes the decision on the still running activity: stop with
// the task's verdict on cancel and escalate.
func (t *taskRun) actOnGate(trip GateTrip, d GateDecision, run *gateRun) (verdict, bool) {
	switch d.Action {
	case ActionWait:
		run.waited = true
	case ActionRetry:
		run.retried = true
	case ActionCancel:
		return verdict{final: "failed", reason: "the gate supervisor cancelled " + trip.Activity + ": " + d.Diagnosis, cancelled: true}, true
	case ActionEscalate:
		reason := fmt.Sprintf(gateStopped+"%s (attempt %d) past its bound of %s: %s", trip.Activity, trip.Attempt, trip.Bound,
			strings.ReplaceAll(d.Diagnosis, "\n", " "))
		if d.Recommendation != "" {
			reason += "\nRecommendation: " + d.Recommendation
		}
		return verdict{escalate: reason}, true
	}
	return verdict{}, false
}

// sessionOptions: the RunSession activity heartbeats; a factoryd that dies
// mid-session misses heartbeats, and Temporal retries the attempt on the
// next worker, which resumes from the dead session's heartbeat. Retries
// share one schedule-to-close budget: the task's remaining agent time.
func (t *taskRun) sessionOptions(remaining time.Duration) workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout:    remaining + sessionGrace,
		ScheduleToCloseTimeout: remaining + sessionGrace,
		HeartbeatTimeout:       2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 10 * time.Second,
			// every factoryd restart costs the session one attempt (a detach
			// and a reattach, roadmap 222, item 2)
			MaximumAttempts:        10,
			NonRetryableErrorTypes: []string{errTerminal},
		},
	}
}

func (t *taskRun) checksOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout:    t.cfg.ChecksTimeout + time.Minute,
		ScheduleToCloseTimeout: t.cfg.ChecksTimeout + time.Minute,
		HeartbeatTimeout:       2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        10 * time.Second,
			MaximumAttempts:        5,
			NonRetryableErrorTypes: []string{errTerminal},
		},
	}
}

// decided drains the terminal signals without blocking and reports the
// decision they make, if any. It runs before every attempt, before a pull
// request is opened and after the checks, so a merge or a rejection is
// never overridden by another attempt.
func (t *taskRun) decided() (final, reason string, ok bool) {
	var sig TaskSignal
	for workflow.GetSignalChannel(t.ctx, SignalPRMerged).ReceiveAsync(&sig) {
		if final, reason, ok := t.decide(SignalPRMerged, sig); ok {
			return final, reason, true
		}
	}
	for workflow.GetSignalChannel(t.ctx, SignalPRClosed).ReceiveAsync(&sig) {
		if final, reason, ok := t.decide(SignalPRClosed, sig); ok {
			return final, reason, true
		}
	}
	if workflow.GetSignalChannel(t.ctx, SignalIssueClosed).ReceiveAsync(&sig) {
		return t.decide(SignalIssueClosed, sig)
	}
	return "", "", false
}

// decide is the verdict one received terminal signal makes, counted as
// decided counts it: ok false when it does not count (another pull
// request's event). An issue's close always decides, by whether its pull
// request merged.
func (t *taskRun) decide(name string, sig TaskSignal) (final, reason string, ok bool) {
	switch name {
	case SignalPRMerged:
		if t.counted(name, sig) {
			return "done", "pull request merged", true
		}
	case SignalPRClosed:
		if t.counted(name, sig) {
			return "failed", "pull request closed without merge", true
		}
	case SignalIssueClosed:
		t.counted(name, sig)
		final, reason, _ := t.issueClosed()
		return final, reason, true
	}
	return "", "", false
}

// changeRequested phrases a reviewer's change request as a finding, the
// same way from a signal and from Reconcile so the two coincide.
func changeRequested(author, body string, at time.Time) string {
	return "reviewer " + author + " requested changes" + dated(at) + ": " + body
}

// drainChanges coalesces every pending change request (one review with
// many comments sends many) into one findings list for one attempt.
func (t *taskRun) drainChanges() []string {
	var out []string
	var sig TaskSignal
	for workflow.GetSignalChannel(t.ctx, SignalChangesRequested).ReceiveAsync(&sig) {
		if t.counted(SignalChangesRequested, sig) {
			out = append(out, changeRequested(sig.Author, sig.Body, sig.At))
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

// awaitHuman escalates the task with the agent's choices, if any, and
// blocks, at no cost, until a human answers on the issue (a comment newer
// than the escalation and the rest of its burst, or a change request on
// the task's PR) or a terminal signal decides the task. answer is the
// change requests; the human's answers go to t.answers (versionAnswers),
// or into answer before it, each tagged with the escalation's question,
// its first line, as Reconcile tags it (answersFrom).
func (t *taskRun) awaitHuman(reason string, choices []string) (final, why string, answer []string) {
	return t.awaitHumanUntil(reason, choices, nil)
}

// awaitHumanAgain is awaitHuman of a healed run that asks question again
// (healedReason), the one Reconcile read as the last asked; its answers
// answer question, as Reconcile tags them.
func (t *taskRun) awaitHumanAgain(question string, choices []string) (final, why string, answer []string) {
	question = cmp.Or(question, escalationQuestion(healedReason))
	if err := t.ask(healedReason, question, choices); err != nil {
		return "failed", escalateFailed + errMessage(err), nil
	}
	return t.awaitAnswer(question, nil)
}

// waitEnd ends a wait for a human on its own, with no answer: the day
// cap's turn (dayWait) or an image's arrival (imageReady).
type waitEnd interface {
	start(t *taskRun)
	add(sel workflow.Selector)
	over() bool
	stop()
}

// awaitHumanUntil is awaitHuman that also ends, with no answer, when end
// does (versionDayCap, versionToolPark); nil never does.
func (t *taskRun) awaitHumanUntil(reason string, choices []string, end waitEnd) (final, why string, answer []string) {
	question := escalationQuestion(reason)
	if err := t.ask(reason, question, choices); err != nil {
		return "failed", escalateFailed + errMessage(err), nil
	}
	return t.awaitAnswer(question, end)
}

// ask escalates the task with reason, which asks question, and the
// agent's choices, if any: only comments newer than its comment answer
// it (answerOf).
func (t *taskRun) ask(reason, question string, choices []string) error {
	if err := workflow.ExecuteActivity(t.short, ActivityEscalate, t.taskID, reason, choices).Get(t.ctx, &t.escalation); err != nil {
		return err
	}
	t.anchors = append(t.anchors, anchor{comment: t.escalation, question: question})
	return nil
}

// awaitAnswer is awaitHumanUntil's wait for the answer to the escalation
// asked last, which asked question.
func (t *taskRun) awaitAnswer(question string, end waitEnd) (final, why string, answer []string) {
	if end != nil {
		end.start(t)
		defer end.stop()
	}
	wake := []string{SignalPRMerged, SignalPRClosed, SignalIssueClosed, SignalChangesRequested, SignalHumanComment}
	// at is where the wait's next answer goes in t.answers, newest first:
	// a burst reads in the order it was written (versionAnswerBurst)
	at := 0
	pass := func() (heard []int64, stop bool) {
		if final, why, stop = t.decided(); stop {
			return nil, true
		}
		var sig TaskSignal
		for workflow.GetSignalChannel(t.ctx, SignalHumanComment).ReceiveAsync(&sig) {
			if t.besideLadder >= versionBesideLadder && t.fileAnswer(sig, t.escalation) {
				continue
			}
			if !t.counted(SignalHumanComment, sig) {
				continue
			}
			heard = append(heard, sig.Comment)
			if t.v7 < versionAnswers {
				who := "human " + sig.Author
				if sig.Author == ChatUser {
					who = answerer(sig.Author)
				}
				answer = append(answer, who+" answered the escalation: "+sig.Body)
				continue
			}
			if t.besideLadder >= versionBesideLadder {
				t.insertAnswer(answerKey{t.escalation, sig.Comment}, answerText(sig.Author, question, sig.Body, sig.At))
				continue
			}
			t.answers = slices.Insert(t.answers, at, answerText(sig.Author, question, sig.Body, sig.At))
			if t.answerBurst >= versionAnswerBurst {
				at++
			}
		}
		answer = append(answer, t.drainChanges()...)
		return heard, false
	}
	for {
		sel := workflow.NewSelector(t.ctx)
		for _, n := range wake {
			sel.AddReceive(workflow.GetSignalChannel(t.ctx, n), func(workflow.ReceiveChannel, bool) {})
		}
		if end != nil {
			end.add(sel)
		}
		sel.Select(t.ctx)
		heard, stop := pass()
		if stop || len(heard) > 0 && t.answerBurst >= versionAnswerBurst && t.awaitBurst(wake, pass, heard) {
			return final, why, nil
		}
		if len(heard) > 0 || len(answer) > 0 || end != nil && end.over() {
			return "", "", answer
		}
	}
}

// awaitCap escalates a session the gateway refused at cap c and waits for
// a human, at the day cap also for the wait's own end (awaitDay).
func (t *taskRun) awaitCap(c CapRefusal, model string) (final, why string, answer []string) {
	if until := c.until(); !until.IsZero() {
		return t.awaitDay(c.reason(), until, model)
	}
	return t.awaitHuman(c.reason(), nil)
}

// awaitDay escalates a session the day cap stopped with reason and waits
// for an answer or the wait's own end; a day that turned at until already
// waits for nothing.
func (t *taskRun) awaitDay(reason string, until time.Time, model string) (final, why string, answer []string) {
	if !workflow.Now(t.ctx).Before(until) {
		return "", "", nil
	}
	return t.awaitHumanUntil(reason, nil, &dayWait{until: until, model: model})
}

// dayCheck is how often a wait on the day cap reads the day's free
// headroom: before the UTC day turns it frees only as outstanding holds
// settle or a human raises budget.day_eur, both seen within it, and a
// day's wait takes at most 288 checks into the run's history.
const dayCheck = 5 * time.Minute

// dayWait is a wait on the day cap (versionDayCap): it ends on its own at
// until, the UTC day's turn, or at a DayResume check, every dayCheck, that
// finds the day's free headroom funds a session's start on model. A nil
// wait never ends on its own.
type dayWait struct {
	until  time.Time
	model  string
	t      *taskRun
	ctx    workflow.Context
	cancel workflow.CancelFunc
	// next is the next check's timer, or the check that runs (checking);
	// turned: that check is the day's turn's
	next             workflow.Future
	checking, turned bool
	ended            bool
}

// start arms the wait's first check.
func (d *dayWait) start(t *taskRun) {
	if d == nil {
		return
	}
	d.t = t
	d.ctx, d.cancel = workflow.WithCancel(t.ctx)
	d.arm()
}

// arm sets the timer of the next check, the day's turn's at the latest.
func (d *dayWait) arm() {
	d.checking = false
	d.next = workflow.NewTimer(d.ctx, min(dayCheck, d.until.Sub(workflow.Now(d.ctx))))
}

// add makes sel wake on the wait's next step: a timer starts a check; a
// check that finds the session's start funded, or the turn's, ends the
// wait, and any other arms the next.
func (d *dayWait) add(sel workflow.Selector) {
	if d == nil || d.ended {
		return
	}
	sel.AddFuture(d.next, func(f workflow.Future) {
		if !d.checking {
			d.checking, d.turned = true, !workflow.Now(d.ctx).Before(d.until)
			opts := workflow.WithActivityOptions(d.ctx, finishOptions())
			d.next = workflow.ExecuteActivity(opts, ActivityDayResume, d.t.taskID, d.model, d.turned)
			return
		}
		var funded bool
		if err := f.Get(d.ctx, &funded); err != nil {
			workflow.GetLogger(d.ctx).Warn("day cap check", "task", d.t.taskID, "error", err)
		}
		if d.ended = funded || d.turned; !d.ended {
			d.arm()
		}
	})
}

// over says the wait ended on its own.
func (d *dayWait) over() bool { return d != nil && d.ended }

// stop cancels the wait's timer and its running check.
func (d *dayWait) stop() {
	if d != nil && d.cancel != nil {
		d.cancel()
	}
}

// awaitBurst holds a wait whose pass took heard, the first comments of an
// answer, until the answering party has posted nothing for answerQuiet,
// at the latest answerMax after them, so an answer in parts lands whole;
// once the wait took the comment the console names whole
// (SignalAnswerWhole) it resumes at once. wake are the wait's signals;
// pass takes what a wake brought and returns the comments that answer,
// or stop when it ended the wait otherwise, which awaitBurst reports.
func (t *taskRun) awaitBurst(wake []string, pass func() (heard []int64, stop bool), heard []int64) bool {
	end := workflow.Now(t.ctx).Add(answerMax)
	timerCtx, cancel := workflow.WithCancel(t.ctx)
	defer func() { cancel() }()
	quiet := workflow.NewTimer(timerCtx, answerQuiet)
	var whole int64
	for {
		closed := false
		sel := workflow.NewSelector(t.ctx)
		for _, n := range append([]string{SignalAnswerWhole}, wake...) {
			sel.AddReceive(workflow.GetSignalChannel(t.ctx, n), func(workflow.ReceiveChannel, bool) {})
		}
		sel.AddFuture(quiet, func(workflow.Future) { closed = true })
		sel.Select(t.ctx)
		var sig TaskSignal
		for workflow.GetSignalChannel(t.ctx, SignalAnswerWhole).ReceiveAsync(&sig) {
			whole = max(whole, sig.Comment)
		}
		more, stop := pass()
		heard = append(heard, more...)
		if stop || closed || slices.Contains(heard, whole) {
			return stop
		}
		if len(more) > 0 {
			cancel()
			timerCtx, cancel = workflow.WithCancel(t.ctx)
			quiet = workflow.NewTimer(timerCtx, min(answerQuiet, end.Sub(workflow.Now(t.ctx))))
		}
	}
}

// awaitPoster asks the issue's author for what the agent needs (reason:
// the agent's information request) and waits for the answer
// (versionNeedsInfo).
func (t *taskRun) awaitPoster(reason string) (final, why string, changes []string) {
	if err := workflow.ExecuteActivity(t.short, ActivityRequestInfo, t.taskID, reason).Get(t.ctx, &t.infoRequest); err != nil {
		return "failed", "request information: " + errMessage(err), nil
	}
	t.anchors = append(t.anchors, anchor{comment: t.infoRequest, request: true})
	return t.waitPoster(workflow.Now(t.ctx))
}

// waitPoster blocks, at no cost, until the issue's author (or a writer)
// comments after the information request of since (and the burst that
// comment opens is over), a reviewer asks for changes, a terminal signal
// decides the task, or infoWait after since passes: then the issue closes
// as not reproducible and the task ends closed. An answer goes to
// t.posterInfo; changes are the change requests.
func (t *taskRun) waitPoster(since time.Time) (final, why string, changes []string) {
	timerCtx, cancel := workflow.WithCancel(t.ctx)
	defer cancel()
	var expiry workflow.Future
	if left := infoWait - workflow.Now(t.ctx).Sub(since); left > 0 {
		expiry = workflow.NewTimer(timerCtx, left)
	}
	wake := []string{SignalPRMerged, SignalPRClosed, SignalIssueClosed, SignalChangesRequested, SignalHumanComment, SignalPosterComment}
	pass := func() (heard []int64, stop bool) {
		if final, why, stop = t.decided(); stop {
			return nil, true
		}
		for _, n := range []string{SignalHumanComment, SignalPosterComment} {
			var sig TaskSignal
			for workflow.GetSignalChannel(t.ctx, n).ReceiveAsync(&sig) {
				if t.answersInfoRequest(n, sig) {
					heard = append(heard, sig.Comment)
					t.posterInfo = append(t.posterInfo, posterText(sig.Author, sig.Body))
				}
			}
		}
		changes = append(changes, t.drainChanges()...)
		return heard, false
	}
	for {
		expired := expiry == nil
		if !expired {
			sel := workflow.NewSelector(t.ctx)
			for _, n := range wake {
				sel.AddReceive(workflow.GetSignalChannel(t.ctx, n), func(workflow.ReceiveChannel, bool) {})
			}
			sel.AddFuture(expiry, func(workflow.Future) { expired = true })
			sel.Select(t.ctx)
		}
		heard, stop := pass()
		if stop || len(heard) > 0 && t.answerBurst >= versionAnswerBurst && t.awaitBurst(wake, pass, heard) {
			return final, why, nil
		}
		if len(heard) > 0 || len(changes) > 0 {
			return "", "", changes
		}
		if expired {
			if err := workflow.ExecuteActivity(t.short, ActivityCloseNotReproduced, t.taskID).Get(t.ctx, nil); err != nil {
				return "failed", "close as not reproducible: " + errMessage(err), nil
			}
			return "closed", notReproducedClose, nil
		}
	}
}

// notReproducedClose is the comment, and the task's final reason, of an
// information request no one answered within infoWait.
const notReproducedClose = "not reproducible, no answer in 14 days"

// answersInfoRequest records a comment signal received while waiting on
// the issue's author and reports whether it answers the request: newer
// than its comment.
func (t *taskRun) answersInfoRequest(name string, sig TaskSignal) bool {
	counts := t.infoRequest != 0 && sig.Comment > t.infoRequest
	detail := map[string]any{"signal": name, "author": sig.Author, "comment": sig.Comment}
	if !counts {
		detail["ignored"] = "older than the information request"
	}
	if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "signal", detail).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("record signal", "error", err)
	}
	return counts
}

// posterText is one answer of the issue's author as the sessions get it.
func posterText(author, body string) string {
	return author + ": " + textutil.Truncate(body, MaxAnswer)
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
	// every counted signal but a human comment ends the approval wait
	// (only an approval wait sets approvalRequested): its step carries
	// the wait's start, once
	if counts && name != SignalHumanComment && !t.approvalRequested.IsZero() {
		detail["started"] = rfc3339(t.approvalRequested)
		t.approvalRequested = time.Time{}
	}
	if name == SignalHumanComment {
		counts = t.escalation != 0 && sig.Comment > t.escalation
		detail = map[string]any{"signal": name, "author": sig.Author, "comment": sig.Comment}
		if !counts {
			detail["ignored"] = "older than the escalation"
		}
	}
	if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "signal", detail).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("record signal", "error", err)
	}
	return counts
}

// issueClosed tells a merge, which closes the issue through "Closes #N",
// from a plain close.
func (t *taskRun) issueClosed() (final, reason string, findings []string) {
	if t.pr.Number == 0 && t.closedByHuman < versionClosedByHuman {
		return "failed", ReasonIssueClosed + " while no pull request of this workflow was open", nil
	}
	if t.pr.Number == 0 {
		return "closed", ReasonIssueClosed + " without merge", nil
	}
	var merged bool
	if err := workflow.ExecuteActivity(t.short, ActivityPullRequestMerged, t.taskID, t.pr.Number).Get(t.ctx, &merged); err != nil {
		return "failed", ReasonIssueClosed + ReasonMergeUnknown + errMessage(err), nil
	}
	if merged {
		return "done", "pull request merged", nil
	}
	return t.closedWithoutMerge(), ReasonIssueClosed + " without merge", nil
}

// closedWithoutMerge is the final state of a task whose issue a human
// closed without a merge.
func (t *taskRun) closedWithoutMerge() string {
	if t.closedByHuman >= versionClosedByHuman {
		return "closed"
	}
	return "failed"
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
	return "", "", []string{changeRequested(sig.Author, sig.Body, sig.At)}
}

// finish is the one exit of every path: a roll's done waits its rollout,
// then the one Finish activity every path shares, which decides the
// card's state. It retries for a day (backoff capped at five minutes). A
// Finish still failing then fails the workflow; the heal pass starts a
// new run, which reconciles the task and decides it again (one found
// closing is finished at once), so no task is left running or closing
// with its credentials live.
func (t *taskRun) finish(final, reason string) (string, error) {
	if final == "done" && t.rolling && t.rollout >= versionRollout {
		final, reason = t.awaitRollout(reason)
	}
	ctx := workflow.WithActivityOptions(t.ctx, finishOptions())
	if err := workflow.ExecuteActivity(ctx, ActivityFinish, t.taskID, final, reason).Get(t.ctx, nil); err != nil {
		return "", err
	}
	return final, nil
}

// awaitRollout is a roll's last wait (ruling 178): after its merge the
// run waits until the rollout is confirmed — every image the merge
// changed runs on its Deployment and is available — and finishes done
// naming what went live. A rollout the task's wall-clock cap passes
// unconfirmed fails the roll naming the rollout, the cap its backstop;
// an error the wait cannot outlive ends it done with the merge's
// reason: the merge went through, and no retry of the wait changes
// that.
func (t *taskRun) awaitRollout(reason string) (final, why string) {
	ctx := workflow.WithActivityOptions(t.ctx, rolloutWaitOptions(t.cfg.WallClockCap))
	var res RolloutResult
	if err := workflow.ExecuteActivity(ctx, ActivityAwaitRollout, t.taskID, t.pr.Number, t.cfg.WallClockCap).Get(t.ctx, &res); err != nil {
		workflow.GetLogger(t.ctx).Warn("await rollout", "task", t.taskID, "error", err)
		return "done", reason
	}
	switch {
	case !res.Confirmed:
		return "failed", reason + "; rollout not confirmed within the wall-clock cap"
	case len(res.Live) == 0:
		return "done", reason + "; rollout confirmed: the merge changed no image"
	}
	return "done", reason + "; rollout live: " + strings.Join(res.Live, ", ")
}

// rolloutWaitOptions bound a roll's rollout wait by the task's
// wall-clock cap and rollWaitSlack; it heartbeats every poll, and a
// factoryd that stops (the rollout's own restart) has the next one
// retry it.
func rolloutWaitOptions(bound time.Duration) workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: bound + rollWaitSlack,
		HeartbeatTimeout:    time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        10 * time.Second,
			MaximumInterval:        time.Minute,
			MaximumAttempts:        10,
			NonRetryableErrorTypes: []string{errTerminal},
		},
	}
}

// finishOptions are the short activities' options: MaximumAttempts 0 is
// unlimited within the day the schedule-to-close allows; a terminal error
// (a Forgejo refusal among them) ends the retries sooner.
func finishOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout:    2 * time.Minute,
		ScheduleToCloseTimeout: 24 * time.Hour,
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

func isHeadMoved(err error) bool {
	var app *temporal.ApplicationError
	return errors.As(err, &app) && app.Type() == errHeadMoved
}

// imageNotPinned is a RunSession that refused its card's image as one with
// no pin in force (ErrImageNotPinned).
func imageNotPinned(err error) bool {
	var app *temporal.ApplicationError
	return errors.As(err, &app) && app.Type() == errImageNotPinned
}

// escalateFailed opens the final reason of a task whose escalation failed:
// no human decided.
const escalateFailed = "escalate: "

// healedReason is what a healed run waiting for a human asks again.
const healedReason = "factoryd restarted while the task waited for a human; the question above still stands."

// MaxAnswer bounds an answer's body, in bytes, in the list every later
// session gets as the ANSWERS or POSTER_INFO env var (envFit and
// maxCarriedAnswers bound the list); the issue keeps it whole, and the
// console refuses a longer one.
const MaxAnswer = 4000

// escalationQuestion is an escalation reason's first line, an agent's
// question without its prefix.
func escalationQuestion(reason string) string {
	q, _, _ := strings.Cut(strings.TrimPrefix(reason, AgentAsks), "\n")
	return q
}

// answerText is one human answer as the sessions get it, tagged with the
// question it answers and its date.
func answerText(author, question, body string, at time.Time) string {
	return fmt.Sprintf("%s%s, to %q: %s", answerer(author), dated(at), textutil.Truncate(question, 300), textutil.Truncate(body, MaxAnswer))
}

// answerer names an escalation's answerer to the agent: a person by their
// login, the project agent's own user as what it is, never a human.
func answerer(author string) string {
	if author == ChatUser {
		return "the project agent " + ChatUser
	}
	return author
}

// dated is " (YYYY-MM-DD)" in UTC, "" for no time.
func dated(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return " (" + at.UTC().Format("2006-01-02") + ")"
}
