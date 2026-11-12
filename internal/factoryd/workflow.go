package factoryd

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
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
	// it, never an escalation.
	SignalPosterComment = "poster_comment"
	// SignalAnswerWhole is the console's notice that issue comment Comment,
	// which its answer box posted, is a whole answer: the wait it answers
	// resumes once it took that comment, with no quiet window
	// (versionAnswerBurst).
	SignalAnswerWhole = "answer_whole"
	// SignalRollFolded is the roller's notice that newer images were
	// folded into a roll task: it wakes the approval wait and
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
	// SignalToPlan asks a running direct card to go to the planner, its
	// payload a PlanSwitch: the lifecycle takes it before its next session
	// (versionDirectToPlan).
	SignalToPlan = "to_plan"
	// SignalCompanionLanded is the chain poller's wake of a card held at
	// its approval on a companion pin (versionCompanions): the companion
	// reached its pin, so the gates read again and the merge can go.
	SignalCompanionLanded = "companion_landed"
	// SignalPolicyLoaded is the policy loader's wake of a card waiting on
	// a cap (versionCapWait): a new policy version is in force.
	SignalPolicyLoaded = "policy_loaded"
	// SignalCaseResolved is Act's notice that the run's case is answered:
	// TaskSignal's Case. A run with no receiver for it drops it.
	SignalCaseResolved = "case_resolved"
	// SignalCaseFaced is the notice of the comment of the human face of the
	// run's case, sent whenever a rung's climb posts it: a trip that rung 1
	// held returned no comment, and for any other the run holds it already
	// and ignores the signal. TaskSignal's Case carries the case id and the
	// comment (versionCaseFaced).
	SignalCaseFaced = "case_faced"
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
	// Case is case_resolved's.
	Case *CaseSignal `json:",omitempty"`
}

// CaseSignal is case_resolved's payload: the case, its resolution, who
// resolved it on which rung, and the issue comment that carries the
// answer (0 when none), so the webhook's copy of that comment is not a
// second answer.
type CaseSignal struct {
	CaseID  int64
	Rung    int
	By      string
	Res     Resolution
	Comment int64
}

// Activity names, registered by Runner.RegisterActivities.
const (
	ActivityEnsureIssue                 = "EnsureIssue"
	ActivitySetState                    = "SetState"
	ActivitySetGate                     = "SetGate"
	ActivityRunSession                  = "RunSession"
	ActivityOpenPullRequest             = "OpenPullRequest"
	ActivityCopyUpstreamImages          = "CopyUpstreamImages"
	ActivityWaitChecks                  = "WaitChecks"
	ActivityHeadChecks                  = "HeadChecks"
	ActivityPullRequestMerged           = "PullRequestMerged"
	ActivityRecordStep                  = "RecordStep"
	ActivityRecordNotRun                = "RecordNotRun"
	ActivityFinish                      = "Finish"
	ActivityReconcile                   = "Reconcile"
	ActivityRecordUsage                 = "RecordUsage"
	ActivityReadUsage                   = "ReadUsage"
	ActivitySuccessor                   = "Successor"
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
	ActivityUnjudged                    = "Unjudged"
	ActivityHoldMerge                   = "HoldMerge"
	ActivitySuperviseGate               = "SuperviseGate"
	ActivityDropTrip                    = "DropTrip"
	ActivityAutoMergeNote               = "AutoMergeNote"
	ActivityRequestInfo                 = "RequestInfo"
	ActivityCloseNotReproduced          = "CloseNotReproduced"
	ActivityScreen                      = "Screen"
	ActivityJudgeScreening              = "JudgeScreening"
	ActivityScreenAnswer                = "ScreenAnswer"
	ActivitySizeCard                    = "SizeCard"
	ActivityRunReport                   = "RunReport"
	ActivityRollCommit                  = "RollCommit"
	ActivityRerunChecks                 = "RerunChecks"
	ActivityGateAudit                   = "GateAudit"
	ActivityGateCompanion               = "GateCompanion"
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
	ActivityCapResume                   = "CapResume"
	ActivityParkForTool                 = "ParkForTool"
	ActivityImageArrived                = "ImageArrived"
	ActivityLeaveImagePark              = "LeaveImagePark"
	ActivityBecomePlan                  = "BecomePlan"
	ActivityReplanChild                 = "ReplanChild"
	ActivitySupersedeApproval           = "SupersedeApproval"
	ActivityReplanStopped               = "ReplanStopped"
	ActivityProofsAtMain                = "ProofsAtMain"
)

// errTerminal marks an activity error no retry can fix; the workflow fails
// the task with its message.
const errTerminal = "terminal"

// errWipHead types the refusal of a gate asked about an interim commit.
const errWipHead = "wip_head"

func terminalError(format string, args ...any) error {
	return temporal.NewNonRetryableApplicationError(fmt.Sprintf(format, args...), errTerminal, nil)
}

// errHeadMoved types a gate's refusal of a head the agent moved past it:
// not terminal — the workflow holds the moved head (versionHeadHolds).
const errHeadMoved = "head-moved"

func headMovedError(format string, args ...any) error {
	return temporal.NewNonRetryableApplicationError(fmt.Sprintf(format, args...), errHeadMoved, nil)
}

// ModelLadder is the one place the model aliases live: every
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
	AnswerSlice time.Duration
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
	// ChecksBound is the checks wait's bound for the task's repository
	// (policy.Tree.ChecksBound), which its timeout follows; zero for a run
	// from before versionChecksWait.
	ChecksBound time.Duration
	// SupervisorTimeout is the pinned limits.supervisor_timeout, which the
	// checks wait's timeout leaves the gate supervisor after its trip.
	SupervisorTimeout time.Duration
	// Stronger: the project's file sets model: stronger and this is a
	// coding card (Session code); the card's first session runs on
	// WorkflowConfig.Models.Stronger instead of Models.Start. A run from
	// before this field existed decodes it false. The planner and report
	// sessions keep their own rung.
	Stronger bool
	// StrongerAfter overrides WorkflowConfig's for the task when above 0:
	// 1 for a medium or large card (roadmap 222, item 5)
	StrongerAfter int
	// NoAudit: the task's project does not switch the audit on,
	// so the audit stage is skipped at its turn and starts nothing
	// beside the checks (versionAuditBeside).
	NoAudit bool
	// Sessions overrides WorkflowConfig.MaxSessions for the task when
	// above 0: groupRevertSessions for a group revert's agent card. A run
	// from before it decodes 0.
	Sessions int
}

// SessionOutcome is how one RunSession attempt ended; the workflow switches
// on it. redirect, retry_stronger and escalate are the supervisor's
// verdicts (architecture 6.6); needs_info is the agent's
// request_info, missing_tool its request_tool.
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
	// first call and every one after; the session never
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
	// OutcomeReplan: the check-in or a person stopped the session for the
	// card's replan (Replan carries the text for the planner); its work is
	// pushed and kept.
	OutcomeReplan SessionOutcome = "replan"
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
	// ParentStop, ParentOutcome and ParentTrip are how the parent session
	// ended: its SessionResult's Summary cut at carryStopBytes, its Outcome
	// and its Trip; the working state copied from its record names them.
	// All "" for a first session.
	ParentStop    string         `json:",omitempty"`
	ParentOutcome SessionOutcome `json:",omitempty"`
	ParentTrip    string         `json:",omitempty"`
	// ParentDeadEnd is the parent's SessionResult.DeadEnd: its todo list
	// comes labelled as the dead end's.
	ParentDeadEnd bool `json:",omitempty"`
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
	// CheckinExtra is the time the workflow added to the session's activity
	// beside its own, for the check-in that opens it (versionCheckinClock).
	CheckinExtra time.Duration `json:",omitempty"`
	// Remaining is the agent time the card had left when the session was
	// scheduled, set with CheckinExtra: the session's own time ends there
	// and the check-in's time comes after it.
	Remaining time.Duration `json:",omitempty"`
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
	// DeadEnd: a check-in model judged the session's approach wrong and it
	// stopped on redirect or retry_stronger; a fixed rule's stop is none.
	DeadEnd bool `json:",omitempty"`
	// Status is the provider's HTTP status of a provider_refused session.
	Status int `json:",omitempty"`
	// StartFailed: the session's pod could not start, on a cause no
	// session fixes (Outcome escalate). No agent ran, so the session took
	// no agent time and made no model call.
	StartFailed bool `json:",omitempty"`
	// Capped is the refusal of the cap that stopped the session, at any cap.
	Capped *CapRefusal `json:",omitempty"`
	// Started is when the session began, its pod's first start for a
	// session reattached after a restart: the since of its answer read.
	Started time.Time `json:",omitzero"`
	// Tool and Need are a missing_tool session's request_tool.
	Tool string `json:",omitempty"`
	Need string `json:",omitempty"`
	// Replan is a replan session's text for the planner (replanText).
	Replan string `json:",omitempty"`
	// Killed is the memory kill that ended the session, nil for none.
	Killed *SessionKill `json:",omitempty"`
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
	Head        string      // its head commit; with no pull request, the task branch's head past the base (versionWipStart)
	Unfinished  bool        // Head is an interim push's work in progress, which no gate judges (versionWipStart)
	Merged      bool        // the task's pull request merged
	PRClosed    bool        // the task's pull request closed without merge
	IssueClosed bool
	// Findings are the trusted change requests signalled after the last
	// session began and never taken up: a run that died between the
	// signal and its next session starts one with them.
	Findings []string
	Usage    TaskUsage // what earlier runs used of the caps and were granted
	Size     string    // the card's size, "" before the sizing step or for a card no size binds
	// Answers are the human's answers to the task's escalations rebuilt
	// from the intake events, newest first, and Question the
	// question of its last escalation, which a healed run asks again with
	// its Choices.
	Answers  []string
	Question string
	Choices  []string
	// InfoRequest is the comment of the task's last information request
	// and InfoRequestedAt its time, PosterInfo the answers to them, oldest
	// first: a healed run waiting on the issue's author
	// waits on, for what is left of the 14 days.
	InfoRequest     int64
	InfoRequestedAt time.Time
	PosterInfo      []string
	// InfoAnswered: an answer to the last information request is among
	// PosterInfo already; the healed run resumes without waiting.
	InfoAnswered bool
	// Report: the task is a report session's, no stage.
	Report bool
	// Standing is the issue author's (Task.Standing), for a stage's when.
	Standing string
	// Screening: the human the task waits on answers its screening's
	// suspect verdict; ScreeningAnswered: one did, before the run ended.
	Screening, ScreeningAnswered bool
	// ScreeningPassed: the card's own screening recorded a clear verdict,
	// or a person let its suspect screening run (its alert judged noise).
	ScreeningPassed bool
	// Stronger: a report task that runs on the stronger rung (the
	// screening).
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
	// Roll: a roll task, whose commit RollCommit writes.
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
	// Cap is the reason of the task's last escalation when a cap stopped it
	// and the card resumes on its own, CapUntil the turn of that cap's
	// period after the escalation: a healed run waits on (versionDayCap).
	// The names of the JSON keys are those of the day cap's wait.
	Cap      string    `json:"DayCap"`
	CapUntil time.Time `json:"DayCapUntil,omitzero"`
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
	run.lastRoundFindings = workflow.GetVersion(ctx, "last-round-findings", workflow.DefaultVersion, versionLastRoundFindings)
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
	run.checkinClock = workflow.GetVersion(ctx, "checkin-clock", workflow.DefaultVersion, versionCheckinClock)
	run.sessionKill = workflow.GetVersion(ctx, "session-kill", workflow.DefaultVersion, versionSessionKill)
	run.checkinSpend = workflow.GetVersion(ctx, "checkin-spend", workflow.DefaultVersion, versionCheckinSpend)
	workflow.GetVersion(ctx, "checkin-grant", workflow.DefaultVersion, versionCheckinGrant)
	run.checkinRepeat = workflow.GetVersion(ctx, "checkin-repeat", workflow.DefaultVersion, versionCheckinRepeat)
	run.extendRaise = workflow.GetVersion(ctx, "extend-raise", workflow.DefaultVersion, versionExtendRaise)
	run.successor = workflow.GetVersion(ctx, "successor", workflow.DefaultVersion, versionSuccessor)

	run.headHolds = workflow.GetVersion(ctx, "head-holds", workflow.DefaultVersion, versionHeadHolds)
	run.toolPark = workflow.GetVersion(ctx, "tool-park", workflow.DefaultVersion, versionToolPark)
	// its value is unused; the call stays until no running history carries the marker
	workflow.GetVersion(ctx, "chain-done", workflow.DefaultVersion, versionChainDone)
	run.directToPlan = workflow.GetVersion(ctx, "direct-to-plan", workflow.DefaultVersion, versionDirectToPlan)
	run.sizing = workflow.GetVersion(ctx, "sizing", workflow.DefaultVersion, versionSizing)
	run.replan = workflow.GetVersion(ctx, "replan", workflow.DefaultVersion, versionReplan)
	run.answeredRouting = workflow.GetVersion(ctx, "answered-routing", workflow.DefaultVersion, versionAnsweredRouting)
	run.companions = workflow.GetVersion(ctx, "companions", workflow.DefaultVersion, versionCompanions)
	run.resumeHold = workflow.GetVersion(ctx, "resume-hold", workflow.DefaultVersion, versionResumeHold)
	run.capWaits = workflow.GetVersion(ctx, "cap-wait", workflow.DefaultVersion, versionCapWait)
	run.raiseReload = workflow.GetVersion(ctx, "raise-reload", workflow.DefaultVersion, versionRaiseReload)
	run.sizeTime = workflow.GetVersion(ctx, "size-time", workflow.DefaultVersion, versionSizeTime)
	workflow.GetVersion(ctx, "cases", versionCases, versionCases)
	run.auditDraw = workflow.GetVersion(ctx, "audit-draw", workflow.DefaultVersion, versionAuditDraw)
	run.agentDraw = workflow.GetVersion(ctx, "agent-draw", workflow.DefaultVersion, versionAgentDraw)
	run.caseQueue = workflow.GetVersion(ctx, "case-queue", workflow.DefaultVersion, versionCaseQueue)
	run.queueWaits = workflow.GetVersion(ctx, "queue-waits", workflow.DefaultVersion, versionQueueWaits)
	run.checksWait = workflow.GetVersion(ctx, "checks-wait", workflow.DefaultVersion, versionChecksWait)
	run.upstreamCopy = workflow.GetVersion(ctx, "upstream-copy", workflow.DefaultVersion, versionUpstreamCopy)
	workflow.GetVersion(ctx, "checkin-judgment", versionCheckinJudgment, versionCheckinJudgment)
	run.surge = workflow.GetVersion(ctx, "surge-resume", workflow.DefaultVersion, versionSurgeResume)
	run.wipStart = workflow.GetVersion(ctx, "wip-start", workflow.DefaultVersion, versionWipStart)
	// its value is unused; both bounds at the drained version, so only a
	// history that carries the marker runs on
	workflow.GetVersion(ctx, "blind-proofs", 1, 1)
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
	if tc.Sessions != 0 {
		t.cfg.MaxSessions = tc.Sessions
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
		SignalSlotFree, SignalPlanAnswered, SignalAnswerWhole, SignalImageReady, SignalToPlan, SignalCompanionLanded, SignalPolicyLoaded, SignalCaseResolved, SignalCaseFaced} {
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
	// versionAnswers (change id "answers"): a human's answer to
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
	// versionMergeRetryCap (change id "merge-retry-cap"): the
	// retries of one approval's merge stop after maxMergeRetries (an hour):
	// one exhausted report, no timer; a new approval restarts the count.
	versionMergeRetryCap workflow.Version = 1
	// versionGateStarted (change id "gate-started"): the
	// signal that ends the approval wait (an approval, a close, a merge, a
	// change request) carries in its timeline step when the wait
	// began: the approval request, or the last refused merge or the
	// exhausted retries that ask for a new approval.
	versionGateStarted workflow.Version = 1
	// versionGateSupervisor (change id "gate-supervisor"): a long gate's
	// activity (the session, the checks, the
	// reproduction, the model review and the merge) takes GateWatch's
	// gate_bound signal while it runs and asks SuperviseGate once per trip:
	// wait and retry act in the activity's worker, at most once each per
	// run (no wait for a session, no retry on the last attempt); cancel
	// stops the activity and fails the task with the diagnosis; escalate
	// stops it and waits for a human like the session supervisor's
	// escalation, whose answer buys a session after a session and runs a
	// gate again (a merge with its approval).
	versionGateSupervisor workflow.Version = 1
	// versionGateSupervisorRace (change id "gate-supervisor-race"):
	// the gate supervisor's call runs beside the gate; a gate that
	// ends first wins and the late answer is dropped (SuperviseGate notes
	// it on the alert).
	versionGateSupervisorRace workflow.Version = 1
	// versionAutoMerge (change id "auto-merge"): after the
	// model review, a pull request AutoMerge admits (the project's rule
	// auto, class outcome auto, the reproduction ran and passed, the
	// review approves, a strict improvement where not reproduced) is
	// approved as factory-gate and merged as factory-merge with no card;
	// every other one, and an auto merge Forgejo refuses or that fails,
	// gets the card with why no auto merge.
	versionAutoMerge workflow.Version = 1
	// versionNeedsInfo (change id "needs-info"): a session
	// ending needs_info asks the issue's author (RequestInfo: the comment
	// with the mentions, waiting-poster, the info_request alert) and
	// waits for the author's or a writer's comment, which the next
	// session gets as poster information, or a change request; after
	// infoWait without one, CloseNotReproduced closes the issue and the
	// task ends closed. A healed run waiting on the author waits on for
	// the rest of infoWait.
	versionNeedsInfo workflow.Version = 1
	// versionStages (change id "stages"): the gates
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
	// versionRoll (change id "roll"): a roll task runs no
	// session; RollCommit writes its branch and the head goes through the
	// pull request, the checks and the gates. A fold of newer images
	// (roll_folded, which wakes the approval wait and is no finding) has
	// RollCommit write again; findings on a head RollCommit leaves as it
	// is fail the task.
	versionRoll workflow.Version = 1
	// versionSessionCap (change id "session-cap"): a session
	// loop out of attempts, however its last round ended and a run that
	// starts at the cap too, opens the card with every finding so far, and
	// a human answer buys one more session; a redirect or no push
	// counts as a return from stage "supervisor:<trip>" ("session" for no
	// trip), recorded like a stage's findings with its attempt, and a
	// repeat past StrongerAfter takes the stronger rung; a new run starts
	// on the rung the last one left.
	versionSessionCap workflow.Version = 1
	// versionAudit (change id "audit"): the audit stage runs
	// GateAudit on the reviewed head; its findings, or its failure, go to
	// the card beside the review, never to the agent; a roll task skips
	// it.
	versionAudit workflow.Version = 1
	// versionReviewVerdict (change id "review-verdict", backlog 68): a
	// review stage that ran and gave no verdict (a failed call, an invalid
	// answer, a head that moved) puts "the review gave no verdict: <why>"
	// on the card, whatever its on_findings; a roll's review (no model
	// call) is no finding.
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
	// versionPosterCap (change id "poster-cap"): the answers
	// to information requests buy at most maxPosterAttempts attempts per
	// task; a later answer still reaches the sessions but buys none.
	versionPosterCap workflow.Version = 1
	// versionProcesses (change id "processes"): the first
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
	// versionAnswerSlice (change id "answer-slice"):
	// a human answer's slice is a quarter of the task's resolved agent
	// time, never under min(minSession, WallClockCap): WorkflowConfig's
	// own AnswerSlice no longer bounds it. A run from before the change
	// keeps that field's value instead (grant).
	versionAnswerSlice workflow.Version = 1
	// versionRollGap (change id "roll-gap"): a roll's merge
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
	// versionRollout (change id "roll-rollout"): a roll's
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
	// versionProviderWait (change id "provider-wait"): a
	// provider_refused session is no attempt and takes no agent time: the
	// task waits on the provider (ProviderWait, then a timer raced by the
	// terminal signals) and runs the session again with the same input;
	// the providerRefusals-th refusal in a row goes to a human instead. A
	// run from before the change takes a refusal as no push.
	versionProviderWait workflow.Version = 1
	// versionPlanAnswers (change id "plan-answers"): the plan's
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
	// versionAttemptFunded (change id "attempt-funded", finding 215): an
	// attempt whose task's remaining budget cannot fund
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
	// at its first call is no attempt and takes no agent time. A run from
	// before the change takes a code session's day cap as no push, a
	// plan's as no plan, and fails a report task's.
	versionDayCap workflow.Version = 1
	// versionFactoryPRSource (change id "factory-pr-source"): a card of a
	// pull request factoryd opened outside any card's workflow is known by
	// its source before the waiting states: whatever state the heal pass
	// restarts it in, its run takes the gates on the pull request linked
	// to it and never a session, and one with none linked fails. A run
	// from before the change answers a waiting state as a coding card does.
	versionFactoryPRSource workflow.Version = 1
	// versionPodWait (change id "pod-wait"): a code, plan or report session
	// whose pod never left Pending within its start's wait ran no agent.
	// It is no attempt and takes no agent time, and the next starts at
	// once with the same input, the pod's own wait having been the wait;
	// the podWaits-th in a row asks a human, naming what the pod waited on
	// (notStarted). A run from before the change takes it as no push, a
	// plan's as no plan, and fails a report task's.
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
	// extend buys its agent time. A run from before the change fails at the
	// limit at once.
	versionPlannerRetry workflow.Version = 1
	// versionAnsweredRouting (change id "answered-routing"): an answer
	// that confirmed the revision with no open question and no writer's
	// change request files its cards with no planner session. A run from
	// before the change starts the session instead, keeping the routing
	// its history was written under.
	versionAnsweredRouting workflow.Version = 1
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
	// plan's integration card and its delta cards (the activity decides
	// which card is one); the criteria's findings go on the approval card,
	// for the human. A run from before asks its approval without the gate.
	versionCriteriaGate workflow.Version = 1
	// versionCheckinRaise (change id "checkin-raise"): an answer to a
	// check-in stopped at its call cap buys the session, whose check-in
	// runs with the policy's calls, never the spend stop's verify of the
	// head; the budget's share still refuses on spent money.
	versionCheckinRaise workflow.Version = 1
	// versionCheckinGrant (change id "checkin-grant"): the marker every run
	// since the change records, called bare at the start; the share grant it
	// gated is gone, and a recorded history holds the marker.
	versionCheckinGrant workflow.Version = 1
	// versionCheckinRepeat (change id "checkin-repeat"): a check-in stop
	// whose answer changed nothing — neither the head nor the limits — is
	// not asked again: the repeat names the unchanged commit when its
	// answer bought a restart, else states that no reply can change it and
	// names the exits. A run from before the change repeats its ask.
	versionCheckinRepeat workflow.Version = 1
	// versionExtendRaise (change id "extend-raise"): the answer wake
	// reloads the caps from the row, so a raise the console's Extend made
	// while the card waited reaches the run — its guard's marks and the
	// usage the next session records — instead of the wake's grant
	// overwriting the raise with the snapshot the last session recorded.
	// A run from before the change keeps its in-memory usage.
	versionExtendRaise workflow.Version = 1
	// versionChainDone (change id "chain-done"): a run that recorded it
	// waited at its merge for the chain; none waits now.
	versionChainDone workflow.Version = 1
	// versionSuccessor (change id "successor"): a session that ends on the
	// named push refusal — the merge of main cannot fit the card's line
	// bound — and the successor exit's answer close the card into its
	// successor, which starts from main's tip and carries the patch and
	// the findings. A run from before the change loops on the refusal.
	versionSuccessor workflow.Version = 1
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
	// versionDirectToPlan (change id "direct-to-plan", roadmap 336): a
	// direct card's lifecycle takes SignalToPlan before its next session
	// (switchToPlan): BecomePlan turns the card plan-track and the plan
	// loop runs on; a refusal is recorded and the card runs on as it was.
	// A run from before the change leaves the signal unread.
	versionDirectToPlan workflow.Version = 1
	// versionSizing (change id "sizing", roadmap 336): before the first
	// session of a card a person or the chat filed, in a process with a
	// plan track, the sizing step sizes it (size), and a card that is not
	// small turns plan-track (switchToPlan). A run from before sizes
	// nothing.
	versionSizing workflow.Version = 1
	// versionReplan (change id "replan", roadmap 336): a plan's first
	// planner session of a run waits for the cards its pending replan
	// stops (awaitReplanStops), each filing of a revision, the healed one
	// too, waits for those and the started cards the newest revision drops
	// (awaitFilingStops), and the plan loop takes the replan's own note on
	// the issue for no writer's change request (planComments). A
	// direct card's switch records its replan step first (replanToPlan); a
	// session stopped for a replan cuts the card in place, or its plan's
	// child goes back to its plan (replanCut); a person's replan wakes a
	// direct card's waits for a person and its approval wait
	// (switchFromWait), and is taken after a roll hold too. A run from
	// before neither waits, before a session or a filing, nor knows the
	// note, takes to_plan only before a session, and asks a person about a
	// replan stop.
	versionReplan workflow.Version = 1
	// versionCompanions (change id "companions"): a plan card carries the
	// companion pins its filing stamped, the gates read them live
	// (GateCompanion at the policy stage) and AutoMerge holds the merge on
	// a pin not met, and the chain poller's companion_landed wakes the
	// held approval, which re-runs the gates from the first stage. A run
	// from before reads no pins and takes no wake.
	versionCompanions workflow.Version = 1
	// versionResumeHold (change id "resume-hold"): a resumed run's
	// answer-verify — a spent check-in's or a gate escalation's answer,
	// which verifies the head again from the first stage — holds a head
	// the agent moved past the gates: the refusal (headIs's typed
	// head-moved error) adopts the branch's head and verifies it instead,
	// its checks waited on, the same hold the normal gate path holds
	// (versionHeadHolds), bounded by maxHeadHolds. A run from before the
	// head-holds change meets the refusal whole — its gates hold no moved
	// head — and one from before this change keeps that end (2026-10-31:
	// t-6eadec8912ab's answer re-entered the gate on PR #1385's moved head
	// and the task died, its fix unmerged).
	versionResumeHold workflow.Version = 1
	// versionCapWait (change id "cap-wait"): a code, plan or report session
	// the gateway refused at the month's or the project's cap, or an
	// attempt its floor refused there, waits as one at the day cap does:
	// the card asks with the refusal and runs the next session on its own
	// once the cap no longer binds one (CapResume, every monthCheck) or the
	// UTC month turns, or sooner on an answer, and a session refused at its
	// first call is no attempt. Before it asks, a check reads the cap in
	// force, and a refusal the version in force already lifted asks
	// nothing. A policy load wakes every cap's wait, which checks
	// gateway.PolicyTTL later (SignalPolicyLoaded); a healed run's wait
	// checks at once. A run from before the change waits on a human at
	// the month's and the project's cap, and its day cap's wait checks
	// every dayCheck alone (DayResume).
	versionCapWait workflow.Version = 1
	// versionRaiseReload (change id "raise-reload"): the lifecycle reads
	// the console's raises from the row (readRaise) before each session's
	// cap check and after every answer wake, so an Extend made while the
	// card ran or waited at any stop — the session cap, the agent time, a
	// gate — reaches the run. The raises live in columns the workflow
	// never writes (TaskUsage.RaisedSessions, RaisedTime, ExtraLines), so no usage
	// record writes them away. A run from before the change reloads the
	// whole row at the check-in's stop alone (versionExtendRaise).
	versionRaiseReload workflow.Version = 1
	// versionSizeTime (change id "size-time"): a card's agent time follows
	// its size as its budget and line bound do (policy.SizeAgentTime): half the
	// class's time for a small card, one and a half for a large one, set
	// when the sizing step writes the size and read from the row on a
	// later run. A run from before the change keeps the class's time.
	versionSizeTime workflow.Version = 1
	// versionCases (change id "cases"): every stop opens its case through
	// OpenTrip, whose rung 0 may resolve it in place, and the wait takes the
	// case's resolution from case_resolved, a plain comment being a
	// continue. The run acts on it after the wait: a successor closes the
	// card into one, a handover sets the model, and a re-verification takes
	// the head through the gates with no session. A draw is the row's, so
	// an answer buys no session or agent time of its own, at a cap or after
	// a session, in the card loop and the plan loop alike, and the loop
	// reads the caps again. A wait that ends on its
	// own withdraws its case, and a dependency that failed with no
	// replacement is a trip. A run from before asks through Escalate and
	// takes none of it.
	versionCases workflow.Version = 1
	// versionQueueWaits (change id "queue-waits"): a card in a project's
	// merge queue leaves it when its approval waits for a human, and an
	// approval takes a turn again before the merge: main merged in, the
	// merge on the approval when main's new commits share no file with the
	// pull request, else the checks on the new head and a new request. A
	// run from before holds its entry through the approval wait and the
	// merge.
	versionQueueWaits workflow.Version = 1
	// versionChecksWait (change id "checks-wait"): the checks wait's own
	// activity timeout follows its bound, the waits row waits.checks, so
	// the wait outlives it by the supervisor's whole budget. A run from
	// before keeps its recorded 45m + 1m options.
	versionChecksWait workflow.Version = 1
	// versionSurgeResume (change id "surge-resume"): a card back from a
	// wait starts its session through the surge bound, AdmitResume: while
	// its project's live count is under work_in_progress plus surge, else
	// it waits queued. A run from before resumes at the bound at once.
	versionSurgeResume workflow.Version = 1
	// versionCheckinAnswer (change id "checkin-answer"): the answer to a
	// check-in stop goes on from the session's own push, not the head it
	// started from; a repeated stop compares the branch's head at the stop
	// and the sessions the console raised. Read where an
	// answer or a repeat first differs, not at the run's start, so a run
	// waiting at a stop at the roll takes it. A run from before verifies the
	// start head, and its next check-in stops at the same spend stop before
	// any model call.
	versionCheckinAnswer workflow.Version = 1
	// versionVerifiedHead (change id "verified-head"): verify returns the
	// head its stages ended on (a moved head held, the merge queue's sync,
	// a re-run of the checks), and its callers keep that head and count it
	// judged. Read where a verification ended on another head than it
	// began. A run from before keeps the head it verified, and an answer
	// verifies it again and spends a head hold on the move.
	versionVerifiedHead workflow.Version = 1
	// versionConflictFact (change id "conflict-fact"): Forgejo's refusal to
	// merge main into the pull request goes to the next session as its
	// findings with no check-in: a fact, not a finding a model raised. Read
	// where a conflict's findings are taken. A run from before asks the
	// next session's check-in to judge it.
	versionConflictFact workflow.Version = 1
	// versionStoppedParent (change id "stopped-parent"): a session its
	// mid-session check-in stopped ran its conversation and is the next
	// session's parent, whose working state that one continues; a session
	// stopped at its start check-in ran nothing and is none. Read at such a
	// stop. A run from before keeps the last session that ended on its own.
	versionStoppedParent workflow.Version = 1
	// versionCheckinClock (change id "checkin-clock"): the time a session's
	// check-in took is no agent time of the card, and a session that opens
	// with a check-in gets the longest check-in a policy allows on top of
	// its own time (SessionInput.CheckinExtra). Read where a session's time
	// is counted and its activity is scheduled. A run from before counts
	// the check-in's time and gives the session no more.
	versionCheckinClock workflow.Version = 1
	// versionSessionKill (change id "session-kill"): a session the sandbox
	// killed for its memory leaves its kill and last command on the card's
	// next stops, as a fact of their cases. Read where a session's result
	// is taken. A run from before carries none.
	versionSessionKill workflow.Version = 1
	// versionAuditDraw (change id "audit-draw"): the audit's gap verdict
	// at a cap — the hold-out settlement's findings, which name no writer —
	// buys its session as the factory's own draw, rung 0 against the pool,
	// refused once the pool is empty, when the card asks a human instead.
	// Read where such a verdict buys a session or time. A run from before
	// buys it ungated.
	versionAuditDraw workflow.Version = 1
	// versionAgentDraw (change id "agent-draw"): a change request at a cap
	// that no person wrote, the project agent's REQUEST_CHANGES review as
	// factory-chat, buys its session as rung 2's draw against the pool,
	// refused once the pool is empty, when the grant goes back and the card
	// asks a human, as the audit's draw does. A person's change request
	// keeps the rung 3 draw. Read where such a request buys a session or
	// time. A run from before draws every writer's request at rung 3,
	// unbounded.
	versionAgentDraw workflow.Version = 1
	// versionCaseQueue (change id "case-queue"): a case_resolved of a
	// case the run does not wait on, as a beside question the review
	// asked beside the checks, answered while the run waits on another
	// case (a session cap, a gate's stop), is kept per case and taken
	// when a wait comes to that case — the repeat guard holds the case
	// for its stop — applied as one taken live, its answer counted once.
	// A run from before drops it, and the comment's words alone answer
	// the question.
	versionCaseQueue workflow.Version = 1
	// versionUpstreamCopy (change id "upstream-copy"): before the pull
	// request of a verified head opens, or its stages wait when a factory
	// pull request is open already, the image lines the head adds to
	// upstream-images.txt are copied into the store, so the pull request's
	// image build finds them there; a line the upstream cannot serve goes
	// to the card as a finding and the run goes on. A run from before
	// copies nothing.
	versionUpstreamCopy workflow.Version = 1
	// versionCheckinJudgment (change id "checkin-judgment"): the check-in
	// moments of a session pay from the project's chat task, the judgment
	// budget, and no card stops for its own check-in share. Its minimum is
	// the version: only a history that carries the marker runs on.
	versionCheckinJudgment workflow.Version = 1
	// versionSizedLeast (change id "sized-least"): the agent time a session
	// needs to start is minSession, or the card's whole agent time when
	// shorter, as the card's size sets it, so a small card whose class gives
	// under 10 minutes starts its first session. Read where it differs from
	// the class's unsized time. A run from before reads that, and such a
	// card asks the agent-time cap before its first session.
	versionSizedLeast workflow.Version = 1
	// versionTripModel (change id "trip-model"): a trip of a run that reads
	// a session's opening floor (the lifecycle's, a report's) carries the
	// model its next session opens on, so the stop's facts weigh the floor
	// of that model, a retry_stronger's the stronger rung's. Read where a
	// trip opens. A run from before carries none, and the facts weigh the
	// card's last session's model.
	versionTripModel workflow.Version = 1
	// versionLastRoundFindings (change id "last-round-findings"): at the
	// last round a stage's findings meant for the agent go back to the loop,
	// whose session cap trips a case the rungs judge, as at any earlier
	// round; a red head at the last round no longer opens an approval. Read
	// where findings meet the last round. A run from before puts them on the
	// approval card as capped.
	versionLastRoundFindings workflow.Version = 1
	// versionCaseVerdicts (change id "case-verdicts"): the verdicts a rung
	// gave on a case's findings, which its resolution carries, join the
	// run's check-in verdicts when the resolution is taken, as a session's
	// check-in's do: the next review reads a dropped finding as dropped.
	// Read where a resolution carrying verdicts is taken. A run from before
	// takes none.
	versionCaseVerdicts workflow.Version = 1
	// versionCheckinPriors (change id "checkin-priors"): a finding's priors
	// are counted by its finding id, the key the review's findings carry,
	// so a re-raise that differs in case or white space still counts. Read
	// where the check-in's ask is built. A run from before counts by exact
	// text.
	versionCheckinPriors workflow.Version = 1
	// versionCaseFaced (change id "case-faced"): the run learns the comment
	// of a face from the case_faced signal and counts the human's replies
	// after it as it does after a trip's own comment. Read where the
	// signal is taken with a comment (takeFaced), never at the start of a
	// wait, so a run that waited on a case before the change takes it as
	// well. No history from before holds such a signal.
	versionCaseFaced workflow.Version = 1
	// versionEnsureIssueClose (change id "ensure-issue-close"): a close that
	// is pending when the run's issue step refuses its card, a row that
	// never got an issue, ends the card as the close decides it, not
	// failed. Read where that step fails. A run from before ends it failed.
	versionEnsureIssueClose workflow.Version = 1
	// versionCaseHeld (change id "case-held"): a wait on a case whose face
	// it does not know keeps each reply newer than every question, after it
	// read the face from the store (CaseState), so a lost or late case_faced
	// costs nothing once a human replies; the kept replies are taken once
	// the face is known, and filed or recorded when the wait ends. A wait
	// that ends with no answer of its case ends the case (a change request,
	// the run's end, the switch to the planner), and so does a gate beside
	// the checks whose ask rung 1 holds when the head goes back or the run
	// ends: no rung faces a stop the run left. A resolution that carries no
	// comment keys its answer by its case, so the next one's is no copy of
	// it. Read where each first runs (caseHeld): a run from before reads it
	// first where a wait on a case ended, and keeps its case after its
	// waits.
	versionCaseHeld workflow.Version = 1
	// versionPlanCaseHeld (change id "plan-case-held"): the plan loop's wait
	// that ends with no answer of its case (a writer's comment, the plan's
	// questions answered, the cap's own end, the run's end) ends the case,
	// as awaitAnswer's does: no rung faces a stop the loop left. Read where
	// such a wait ended (leavePlanWait). A run from before keeps the case.
	versionPlanCaseHeld workflow.Version = 1
	// versionWipStart (change id "wip-start"): a session that pushed and
	// ended without a verdict (no_push, redirect, retry_stronger,
	// provider_refused, day_cap) hands its head to the next
	// session as its start, and a healed card with no pull request starts
	// its first session from the branch's head. A run from before starts
	// each from the base.
	versionWipStart workflow.Version = 1
)

// budgetRefused opens the card of a task whose remaining budget cannot
// fund a call: an answer would start an attempt whose first
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

// checkinStopMark is a check-in stop's state: what a repeated stop
// compares itself against (versionCheckinRepeat). Head is the head the
// session started from, Tip the branch's head at the stop: the session's
// own push, else Head (versionCheckinAnswer). ExtraLines and
// RaisedSessions are the console's raises of the line bound and the
// sessions; Granted says the stop's answer bought a restart that pushed nothing.
type checkinStopMark struct {
	Kind, Head, Tip string
	ExtraLines      int
	RaisedSessions  int
	Granted         bool
}

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
	attemptFunded     workflow.Version // the attempt grant's floor
	processes         workflow.Version
	depWait           workflow.Version
	lastRoundFindings workflow.Version
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
	checkinRaise      workflow.Version // a call-cap stop's answer buys the session (versionCheckinRaise)
	checkinClock      workflow.Version // a session's check-in is no agent time (versionCheckinClock)
	sessionKill       workflow.Version // a memory kill rides the next stops as a fact (versionSessionKill)
	killed            *SessionKill     // the last session's memory kill (versionSessionKill)
	floorModel        *string          // the model the next session opens on, whose floor the run reads and a trip carries (versionTripModel)
	checkinRepeat     workflow.Version // a stop whose answer changed nothing is not asked again (versionCheckinRepeat)
	extendRaise       workflow.Version // the answer wake reloads the row's caps (versionExtendRaise)
	successor         workflow.Version // the push-bound refusal and its exit close into a successor (versionSuccessor)
	// lastCheckinStop is the last check-in stop's mark, what a repeat
	// compares against (versionCheckinRepeat); zero keeps a run from before
	// the change silent
	lastCheckinStop   checkinStopMark
	checkinStop       string           // the check-in spend stop the next trip is (CheckinStop*), "" for any other
	headHolds         workflow.Version // a moved head is held, not refused (versionHeadHolds)
	upstreamCopy      workflow.Version // a verified head's new upstream image lines are copied first (versionUpstreamCopy)
	wipStart          workflow.Version // a dead session's pushed head, and a healed branch's, is the next start (versionWipStart)
	copiedHeads       []string         // the heads whose lines were copied this run, once each
	holds             int              // heads adopted this run (versionHeadHolds)
	head              string           // the head the running verification's last stage ran on, which verify returns: the head the loop knows between verifications, which a trip carries
	resumeHold        workflow.Version // a resumed run's answer-verify holds a moved head (versionResumeHold)
	toolPark          workflow.Version // a missing tool parks the card on an image (versionToolPark)
	directToPlan      workflow.Version // a direct card takes to_plan before its next session (versionDirectToPlan)
	sizing            workflow.Version // a person's card is sized before its first session (versionSizing)
	replan            workflow.Version // a plan's run waits for its replan's stops and knows its note (versionReplan)
	answeredRouting   workflow.Version // a confirming answer files with no session (versionAnsweredRouting)
	companions        workflow.Version // the gates read the plan card's companion pins and a landed pin wakes the approval (versionCompanions)
	replanBy          string           // the pending replan's person (versionReplan)
	replanNote        string           // the pending replan's note (versionReplan)
	planRefused       string           // why the last switch to the plan track was refused (switchToPlan)
	screening         bool             // the card's screening has not passed: no wait takes a replan (screen, screeningWait)
	switched          *programEnd      // the run's end once the card switched to the plan track, which finish returns (switchToPlan)
	gateRuns          map[string]int   // the long gates' runs so far, by activity (versionTripRouting)
	dayCap            workflow.Version // a day cap's refusal waits for the day to turn (versionDayCap)
	capWaits          workflow.Version // every cap's wait ends on its own and wakes on a policy load (versionCapWait)
	raiseReload       workflow.Version // the console's raises are read before each cap check and after each answer (versionRaiseReload)
	sizeTime          workflow.Version // the agent time follows the card's size (versionSizeTime)
	agentTimeBase     time.Duration    // the class's agent time, which the size scales (sizeAgentTime)
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
	// planTaken are the answer comments the plan loop took (versionCases):
	// the other copy of one, the webhook's or its case's, is no second
	// answer
	planTaken []int64
	// cases: every stop is a case, and the wait takes its resolution
	// (versionCases). caseID is the case the run waits on, 0 for none.
	// resolution is the one the last wait took, which the waits' callers
	// take (takeResolution). stopFindings are the check-in stop's work
	// state, which a successor chosen at its wait carries. reverify and
	// rerun make the loop take the head through the gates with no session,
	// its checks run again first. nextModel is the model a handover at a
	// gate's wait chose for the next session, "" for none
	auditDraw       workflow.Version
	agentDraw       workflow.Version
	caseQueue       workflow.Version
	caseID          int64
	resolution      *Resolution
	stopFindings    string
	reverify, rerun bool
	nextModel       string
	// resolvedQueue holds the resolutions case_resolved carried for cases
	// this run does not wait on, by their case's id, taken when a wait
	// comes to the case (versionCaseQueue)
	resolvedQueue map[int64]queuedCase
	// inCaseWait: awaitAnswer waits on caseID; kept are the replies it
	// read while it knew no face comment (versionCaseHeld)
	inCaseWait bool
	kept       []TaskSignal
	// changesBy is the writer of the newest change request drained, whose
	// session a change request at the cap draws (versionCases)
	changesBy string
	// checksWait: the checks wait's own timeout follows its bound
	// (versionChecksWait)
	checksWait workflow.Version
	// surge: a resumed card's session passes the surge bound
	// (versionSurgeResume)
	surge workflow.Version
	// waitEnded is when the wait of a human ended, the time a resumed card
	// queues by for a slot (versionSurgeResume)
	waitEnded time.Time
	// queueWaits: the approval wait is out of the merge queue
	// (versionQueueWaits)
	queueWaits workflow.Version
	// asked is the newest merge ask's input, askedCard the card's lines it
	// held before AutoMerge's, which regate's ask again carries
	asked     ApprovalInput
	askedCard []string
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
	// requested: the findings are a writer's change request, or the
	// hold-out settlement's of a run from before; by is that writer, ""
	// for the settlement's
	requested bool
	by        string
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
	// reask: a refused switch to the plan track voided the approval
	// request, which is asked again (versionReplan)
	reask bool
	// recheck: a companion reached its pin while the approval waited, so
	// the gates read again from the first stage before any merge
	// (versionCompanions)
	recheck bool
	// head: the head verify's stages ended on, its commit or one a stage
	// adopted past it (taskRun.head)
	head string
	// conflict: the findings are Forgejo's refusal to merge main into the
	// pull request (versionConflictFact)
	conflict bool
	// synced: the turn an approval took merged in main's commits, which
	// change a file of the pull request; head is the head the sync made
	// (versionQueueWaits)
	synced bool
	// wip: the head is an interim push's, which no gate judged; findings
	// is the fact the next session works from, which no check-in judges
	// (versionWipStart)
	wip bool
}

func (t *taskRun) lifecycle() (string, error) {
	drained := t.v >= versionSignalsDrained
	t.agentTimeBase = t.cfg.WallClockCap
	if err := workflow.ExecuteActivity(t.short, ActivityEnsureIssue, t.taskID).Get(t.ctx, nil); err != nil {
		if workflow.GetVersion(t.ctx, "ensure-issue-close", workflow.DefaultVersion, versionEnsureIssueClose) >= versionEnsureIssueClose {
			if final, reason, ok := t.decided(); ok {
				return t.finish(final, reason)
			}
		}
		return t.finish("failed", "ensure issue: "+errMessage(err))
	}
	model := t.cfg.Models.Start
	if t.cfg.Task.Stronger {
		model = t.cfg.Models.Stronger
	}
	t.floorModel = &model
	// findings come from outside the sessions (a verification, a human, the
	// issue's author); own are the newest unpushed session's own lines,
	// which replace an earlier session's. sources names where each of
	// findings came from — the verification, the changes a human or
	// reviewer asked for — for the check-in's ask.
	var findings, own []string
	var sources []string
	var handoverNote string // a mid-session check-in's note, riding the next ask-less brief (plan E)
	var wipLine string      // the newest interim head's refusal among findings (versionWipStart)
	setFindings := func(fs []string, from string) {
		findings, sources = fs, slices.Repeat([]string{from}, len(fs))
	}
	addFindings := func(fs []string, from string) {
		findings, sources = append(findings, fs...), append(sources, slices.Repeat([]string{from}, len(fs))...)
	}
	// parent is the last session that left a record; parentStop,
	// parentOutcome and parentTrip say how it ended, for its working state
	startRef, parent := "", ""
	var parentStop, parentTrip string
	var parentOutcome SessionOutcome
	var parentDeadEnd bool
	var u TaskUsage
	reason := "no session produced a result"
	refusals := 0 // provider_refused sessions in a row (versionProviderWait)
	// least is the agent time a session needs to start (versionKeepWork):
	// minSession, or the card's whole agent time when shorter, as its size
	// sets it (versionSizedLeast); a run from before reads the class's
	// agent time before sizing, unsized
	unsized := min(minSession, t.cfg.WallClockCap)
	least := func() time.Duration {
		sized := min(minSession, t.cfg.WallClockCap)
		if sized != unsized && workflow.GetVersion(t.ctx, "sized-least", workflow.DefaultVersion, versionSizedLeast) >= versionSizedLeast {
			return sized
		}
		return unsized
	}
	// capAnswer is what an answer at a cap buys: an attempt only at the
	// session cap, else the agent time alone (versionSessionLadder)
	capAnswer := func() {
		if t.sessionLadder >= versionSessionLadder && u.Attempts < t.cfg.MaxSessions+u.SessionsAdded() {
			t.grantTime(&u)
			return
		}
		t.grant(&u)
	}
	// apply takes a verification's findings for the next session. A
	// writer's change request with no session left is the human's answer
	// (versionKeepWork): it buys the session, answered, and what it bought is
	// the writer's draw against the pool
	apply := func(v verdict) (answered bool) {
		if v.wip {
			// what the next session was told stays beside the fact, which
			// replaces an earlier interim head's
			if i := slices.Index(findings, wipLine); wipLine != "" && i >= 0 {
				findings = slices.Clone(findings)
				findings[i] = v.findings[0]
			} else {
				addFindings(v.findings, "review")
			}
			wipLine, reason = v.findings[0], v.reason
			return false
		}
		if v.stronger {
			model = t.cfg.Models.Stronger
		}
		from := "review"
		if v.requested {
			from = "changes"
		}
		setFindings(v.findings, from)
		if len(v.findings) > 0 && !(v.conflict && workflow.GetVersion(t.ctx, "conflict-fact", workflow.DefaultVersion, versionConflictFact) >= versionConflictFact) {
			t.gateFindings = true
		}
		own, reason = nil, v.reason
		if t.keepWork >= versionKeepWork && v.requested &&
			(u.Attempts >= t.cfg.MaxSessions+u.SessionsAdded() || t.cfg.WallClockCap+u.TimeAdded()-u.AgentTime < least()) {
			before := u
			capAnswer()
			draw := policy.Draw{Sessions: u.ExtraAttempts - before.ExtraAttempts, AgentTime: u.ExtraTime - before.ExtraTime}
			refused := false
			switch {
			case v.by != "" && (isPerson(v.by) || t.agentDraw < versionAgentDraw):
				t.recordGrant(v.by, draw, u)
			case v.by != "":
				refused = !t.recordAgentDraw(v.by, draw, u)
			case t.auditDraw >= versionAuditDraw:
				refused = !t.recordAuditDraw(draw, u)
			}
			if refused {
				// the pool (or the card) is empty: the grant goes back and
				// the card asks a human
				u = before
				t.recordUsage(u)
				return false
			}
			return true
		}
		return false
	}
	// refund is an attempt, or agent time alone after a session that took
	// no number (u.LastUncounted): what a park on an image gives back for
	// the session that asked for its tool
	refund := func() {
		if u.LastUncounted {
			t.grantTime(&u)
			return
		}
		t.grant(&u)
	}
	// buy is what an answer after a session buys: refund's. A draw is the
	// row's: the slice after a session that took no number alone stays the
	// run's
	buy := func() {
		if u.LastUncounted {
			refund()
		}
	}
	// took acts on the resolution the last wait took (versionCases): a
	// successor closes the card into one with the check-in stop's work
	// state, a close waits for the issue's close Act made, a handover sets
	// the next session's model, a re-verification takes the head through
	// the gates at the loop's top. The rest need nothing more: the raises
	// are the row's, the note is in t.answers
	took := func() (final, why string) {
		res, findings := t.takeResolution(), t.stopFindings
		t.stopFindings = ""
		if res == nil {
			return "", ""
		}
		switch res.Action {
		case ActSuccessor:
			return t.closeIntoSuccessor(findings)
		case ActClose:
			return t.awaitClose()
		case ActHandover:
			model = t.handoverModel(model, res.Args)
		case ActReverify, ActRouteApproval, ActRerunChecks:
			t.reverify, t.rerun = true, res.Action == ActRerunChecks
		}
		return "", ""
	}
	// held ends every wait for a human of this loop: the task's final state
	// when the wait decided one, else the console's raises are read from
	// the row (versionRaiseReload), the change requests that answered
	// it join the next session's findings (the human's comments are in
	// t.answers) and its resolution is taken (versionCases)
	held := func(final, why string, answer []string) (string, string) {
		t.waitEnded = workflow.Now(t.ctx)
		if final == "" {
			t.readRaise(&u)
			addFindings(answer, "changes")
			if len(answer) > 0 {
				t.gateFindings = true
			}
			return took()
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
			startRef = t.adopted(v, head)
			answered = apply(v)
		}
		return head, v, answered
	}
	// timeCap is the agent-time cap (versionKeepWork): findings on its head go
	// to the cap card, whose answer buys time. The card asks with or without
	// a head: an extend draws the time.
	timeCap := func() (final, why string) {
		_, v, answered := verifyAtCap()
		switch {
		case v.final != "":
			return v.final, v.reason
		case answered:
			return "", ""
		}
		capHead := t.timeCapHead()
		if u.Attempts >= t.cfg.MaxSessions+u.SessionsAdded() {
			capHead = t.capHead()
		}
		if final, why := held(t.awaitHuman(t.sessionCapCard(capHead, reason, slices.Concat(findings, own)), nil)); final != "" {
			return final, why
		}
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
		if t.wipStart < versionWipStart && st.PR.Number == 0 {
			st.Head = ""
		}
		t.sizeAgentTime(st.Size)
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
		if st.PR.Number != 0 && (t.wipStart < versionWipStart || !st.Unfinished) {
			t.judged = append(t.judged, st.Head)
		}
		if t.v16 >= versionStages {
			t.returns, t.history, t.reviews, t.round = st.Returns, st.History, st.Reviews, st.Round
			t.checkinVerdicts = st.CheckinVerdicts
		}
		// a person's replan pending at the start of a card with work: a
		// reopened final card switches before its closed pull request ends
		// it. A merge not yet recorded on the row, a closed issue and the
		// screening's wait take none; a fresh card takes it at the loop
		// top, after its screening and its sizing.
		if t.replans() && (u.Attempts > 0 || st.PR.Number != 0 || st.PRClosed) &&
			!st.Skip && !st.Merged && !st.IssueClosed && !st.Screening && st.Status != "closing" {
			if sw, step, ok := t.takeToPlan(); ok {
				if final, why, done := t.switchFromWait(sw, step, st.Status); done {
					return t.finish(final, why)
				}
			}
		}
		// a fresh card's whole time before its screening passed is the
		// screening's: no wait takes a replan (versionReplan)
		t.screening = t.replan >= versionReplan && t.v16 >= versionStages && u.Attempts == 0 && st.PR.Number == 0 && !st.ScreeningPassed && t.screens()
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
				return t.finish("failed", alertNoLongerFires)
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
			// session, whose rebuilt answers are screened again. One who
			// answered before the run ended is not asked again: the answer,
			// in the rebuilt ones, judges the screening (versionReplan)
			t.pr, startRef = st.PR, st.Head
			wait := func() (string, string, []string) { return t.awaitHumanAgain(st.Question, nil) }
			if t.replan >= versionReplan && st.ScreeningAnswered {
				wait = func() (string, string, []string) { return "", "", nil }
			}
			final, why, answer := t.screeningWait(wait)
			if final == "" {
				final, why = took()
			}
			if final != "" {
				return t.finish(final, why)
			}
			setFindings(answer, "changes")
		case t.dayCap >= versionDayCap && st.Status == "waiting-human" && st.Cap != "":
			// the cap's wait, its own end included
			t.pr, startRef = st.PR, st.Head
			final, why, answer := t.awaitCapWait(st.Cap, &capWait{until: st.CapUntil, model: model, decision: capWaitDecision(st.Cap), restored: true})
			if final == "" {
				final, why = took()
			}
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
				refund()
			}
		case t.v3 >= versionSupervisor && st.Status == "waiting-human":
			t.pr, startRef = st.PR, st.Head
			final, why, answer := t.awaitHumanAgain(st.Question, st.Choices)
			if final == "" {
				final, why = took()
			}
			if final != "" {
				return t.finish(final, why)
			}
			t.readRaise(&u)
			setFindings(answer, "changes")
			switch {
			case t.reverify:
				// the loop's top takes the head through the gates
			case t.v5 < versionPersistUsage:
			case AtSessionCap(st.Question):
			case t.sessionLadder < versionSessionLadder:
				t.grant(&u)
			case floorRefused(st.Question), strings.HasPrefix(st.Question, floorUnread),
				strings.HasPrefix(st.Question, podNeverStarted), strings.HasPrefix(st.Question, providerRefused):
				// the live loop answers these waits with the next
				// session alone
			case atCheckinSpend(st.Question):
				stop := checkinStop{spend: true, callCap: atCheckinCallCap(st.Question)}
				if _, verify := t.checkinAnswer(&u, stop, st.PR.Number != 0, "", st.Head); !verify {
					buy()
					break
				}
				// a head the agent moved past the gates is held, not died
				// on (versionResumeHold)
				t.lastRound = u.Attempts >= t.cfg.MaxSessions+u.SessionsAdded()
				v := t.resumeVerify(&st.Head)
				startRef = st.Head
				if v.final != "" {
					return t.finish(v.final, v.reason)
				}
				apply(v)
			case st.PR.Number != 0 && (atGateStop(st.Question) || atQueueLeft(st.Question) || atReproStart(st.Question)):
				// a gate's answer verifies again, as the live gate does:
				// here from the first stage, change requests back to the
				// agent. A head the agent moved past the gates is held,
				// not died on (versionResumeHold)
				if len(answer) > 0 {
					apply(verdict{findings: answer, reason: "changes requested", requested: true, by: t.changesBy})
					break
				}
				t.lastRound = u.Attempts >= t.cfg.MaxSessions+u.SessionsAdded()
				v := t.resumeVerify(&st.Head)
				startRef = st.Head
				if v.final != "" {
					return t.finish(v.final, v.reason)
				}
				apply(v)
			case t.replan >= versionReplan && atScreenStop(st.Question):
				// the screening gate's stop: its answer runs the screening
				// again below, as the live gate's does, and buys no session
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
		case t.wipStart >= versionWipStart && st.PR.Number != 0 && st.Unfinished:
			// the pull request's head is a session's interim push: the work
			// goes on from it in a session, not to the gates
			t.pr, startRef = st.PR, st.Head
			setFindings(append(slices.Clone(st.Findings), "the pull request's head is a session's work in progress, which ended unfinished: continue from the branch as it is"), "changes")
		case t.v4 >= versionReconcileFindings && st.PR.Number != 0 && len(st.Findings) > 0:
			// the reviewer asked for changes on the pushed head and no
			// session took them up: start one
			t.pr, startRef = st.PR, st.Head
			setFindings(st.Findings, "changes")
		case st.PR.Number != 0:
			// the task's work is pushed and in review: verify it, no new session
			t.pr = st.PR
			t.lastRound = u.Attempts >= t.cfg.MaxSessions+u.SessionsAdded()
			v := t.verify(st.Head)
			if v.final != "" {
				return t.finish(v.final, v.reason)
			}
			startRef = t.adopted(v, st.Head)
			apply(v)
		case t.v16 >= versionStages && u.Attempts == 0 && (t.replan < versionReplan || !st.ScreeningPassed):
			final, why, more := t.screen()
			if final != "" {
				return t.finish(final, why)
			}
			addFindings(more, "changes")
		}
		if t.wipStart >= versionWipStart && st.PR.Number == 0 {
			startRef = cmp.Or(startRef, st.Head)
		}
		if t.screening {
			// a healed fresh card answered a wait before its screening
			// passed: it is screened now, as a live one before its first
			// session
			final, why, more := t.screen()
			if final != "" {
				return t.finish(final, why)
			}
			addFindings(more, "changes")
		}
		if t.sizing >= versionSizing && u.Attempts == 0 && st.PR.Number == 0 && t.cfg.Task.PlanShape != nil && slices.Contains(sizedSources, st.Source) {
			final, why, more, toPlan := t.size()
			if final != "" {
				return t.finish(final, why)
			}
			addFindings(more, "changes")
			if toPlan != nil {
				// running: the plan loop reads a waiting-human row as its own
				// healed wait, and an answered screening leaves the row there
				sw := *toPlan
				if asked, _, ok := t.takeToPlan(); ok {
					sw = sw.and(asked)
				}
				if final, why, done := t.switchFromWait(sw, ReplanStep{}, "running"); done {
					return t.finish(final, why)
				}
			}
		}
	}
	// taken is how many numbers the budget's refusals took since the last
	// session, in a run from before versionSessionLadder
	taken := 0
	for {
		t.readRaise(&u)
		if t.nextModel != "" {
			model, t.nextModel = t.nextModel, ""
		}
		if startRef != "" {
			// the head a trip of this pass stops at
			t.head = startRef
		}
		if t.reverify {
			// an answer took the head through the gates again, with no
			// session: its checks first when it ran them again
			// (versionCases)
			head := cmp.Or(startRef, t.head)
			t.reverify = false
			if t.rerun {
				t.rerun = false
				if err := workflow.ExecuteActivity(t.short, ActivityRerunChecks, t.taskID, head).Get(t.ctx, &head); err != nil {
					return t.finish("failed", "re-run the checks: "+errMessage(err))
				}
				t.rerunHead = head
			}
			if head != "" {
				t.lastRound = u.Attempts >= t.cfg.MaxSessions+u.SessionsAdded()
				v := t.verify(head)
				if v.final != "" {
					return t.finish(v.final, v.reason)
				}
				startRef = t.adopted(v, head)
				apply(v)
				continue
			}
		}
		attempt := u.Attempts + 1 + taken
		if t.attemptFunded >= versionAttemptFunded {
			// the session is refused before it starts: no session, no key,
			// no failure round, and the card says why an answer will not run
			if why := t.unfunded(model); why != "" {
				if d, ok := CapDecision(why); ok && (d == policy.RefusedDayCap || t.capWaits >= versionCapWait) {
					// a spent cap resets on its own: the cap's wait, not a
					// human's, and no session ran
					_, state, _ := strings.Cut(why, "\n")
					until := capTurn(d, workflow.Now(t.ctx))
					if final, end := held(t.awaitCapWait(capWaitReason(d, capFloor(d), state, until), &capWait{until: until, model: model, decision: d})); final != "" {
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
		if attempt > t.cfg.MaxSessions+u.SessionsAdded() {
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
				if final, why := held(t.awaitHuman(t.sessionCapCard(t.capHead(), reason, slices.Concat(findings, own)), nil)); final != "" {
					return t.finish(final, why)
				}
				// a draw is the row's: the loop's top reads the cap again
				continue
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
		remaining := t.cfg.WallClockCap + u.TimeAdded() - u.AgentTime
		if t.keepWork >= versionKeepWork && remaining < least() {
			if final, why := timeCap(); final != "" {
				return t.finish(final, why)
			}
			// a draw is the row's: the loop's top reads the time again
			continue
		}
		if remaining <= 0 {
			return t.finish("failed", "wall-clock cap reached")
		}
		// the screening is no wait: the card queues by the answer's time
		answered := t.waitEnded
		if final, why := held(t.screenAnswers()); final != "" {
			return t.finish(final, why)
		}
		if !answered.IsZero() {
			t.waitEnded = answered
		}
		if t.reverify {
			continue
		}
		if t.surge >= versionSurgeResume {
			if final, why := t.awaitSlot(); final != "" {
				return t.finish(final, why)
			}
		}
		if err := workflow.ExecuteActivity(t.short, ActivitySetState, t.taskID, "running").Get(t.ctx, nil); err != nil {
			return t.finish("failed", "set running: "+errMessage(err))
		}
		// only here is the card running, a status BecomePlan accepts: a
		// to_plan that waited behind a person's wait is not refused
		if t.directToPlan >= versionDirectToPlan {
			if sw, step, ok := t.takeToPlan(); ok {
				if final, switched, err := t.replanToPlan(step, sw); switched {
					return final, err
				}
			}
		}
		if v, decided := t.awaitRollHold(); decided {
			return t.finish(v.final, v.reason)
		}
		// a replan that came during the roll hold goes before the session
		if t.replan >= versionReplan {
			if sw, step, ok := t.takeToPlan(); ok {
				if final, switched, err := t.replanToPlan(step, sw); switched {
					return final, err
				}
			}
		}
		began := workflow.Now(t.ctx)
		var res SessionResult
		in := SessionInput{
			TaskID: t.taskID, Attempt: attempt, Sessions: t.cfg.MaxSessions + u.SessionsAdded(), Model: model,
			Findings: slices.Concat(findings, own), Answers: t.answers, PosterInfo: t.posterInfo, StartRef: startRef, ParentSessionID: parent,
			ParentStop: parentStop, ParentOutcome: parentOutcome, ParentTrip: parentTrip, ParentDeadEnd: parentDeadEnd,
			PolicyVersion: t.cfg.PolicyVersion, AgentTime: t.cfg.Task.AgentTime, CheckIn: t.cfg.Task.CheckIn,
		}
		checkinPending := (t.gateFindings || t.deadFrom != "") && t.cfg.Task.CheckIn > 0
		var gates *GateResults
		if len(t.reviews) > 0 {
			g := t.lastGates
			gates = &g
		}
		in.LastGates = gates
		window := remaining
		if checkinPending {
			if t.checkinClock >= versionCheckinClock {
				in.CheckinExtra, in.Remaining = policy.MaxCheckinTime, remaining
				window += in.CheckinExtra
			}
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
		v, decided, err := t.race(t.sessionOptions(window), &res, ActivityRunSession, in)
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
		if t.wipStart >= versionWipStart && err == nil && !decided && res.Commit != "" {
			switch res.Outcome {
			case OutcomeNoPush, OutcomeRedirect, OutcomeRetryStronger, OutcomeProviderRefused, OutcomeDayCap, OutcomeWallClock:
				startRef = res.Commit
				if res.Outcome == OutcomeProviderRefused {
					parent, parentStop, parentOutcome, parentTrip, parentDeadEnd = res.SessionID, textutil.Truncate(res.Summary, carryStopBytes), res.Outcome, res.Trip, res.DeadEnd
				}
			}
		}
		if t.providerWait >= versionProviderWait && err == nil && !decided && res.Outcome == OutcomeProviderRefused {
			if ask := t.providerRefused(&refusals, ProviderWait{Status: res.Status, Reason: res.Reason}); ask != "" {
				if final, why := held(t.awaitHuman(ask, nil)); final != "" {
					return t.finish(final, why)
				}
			}
			continue
		}
		// at versionCapWait a month's or a project's cap takes the day
		// cap's path, before the escalation its outcome is
		capped := t.dayCap >= versionDayCap && err == nil && !decided && res.Capped != nil &&
			(res.Outcome == OutcomeDayCap || t.capWaits >= versionCapWait)
		if capped && !res.Capped.Allowed {
			// refused at its first call: no attempt, the same input after
			if final, why := held(t.awaitCap(*res.Capped, model)); final != "" {
				return t.finish(final, why)
			}
			continue
		}
		if !res.StartFailed {
			t.providerAnswered(&refusals, err)
			u.AgentTime += workflow.Now(t.ctx).Sub(began) - t.checkinTime(res)
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
		last := attempt >= t.cfg.MaxSessions+u.SessionsAdded()
		if err != nil {
			reason = "session failed: " + errMessage(err)
			if final, why := askUnanswered(); final != "" {
				return t.finish(final, why)
			}
			continue
		}
		ran := res.Outcome == OutcomeCheckinHuman && res.CheckIn != nil && res.CheckIn.Record != nil && res.CheckIn.Record.Moment == CheckInSession
		if !res.StartFailed && (res.Outcome != OutcomeCheckinHuman && res.Outcome != OutcomeCheckinDropped ||
			ran && workflow.GetVersion(t.ctx, "stopped-parent", workflow.DefaultVersion, versionStoppedParent) >= versionStoppedParent) {
			parent, parentStop, parentOutcome, parentTrip, parentDeadEnd = res.SessionID, textutil.Truncate(res.Summary, carryStopBytes), res.Outcome, res.Trip, res.DeadEnd
		}
		if res.Model != "" {
			model = res.Model
		}
		reason = res.Summary
		if t.sessionKill >= versionSessionKill {
			t.killed = res.Killed
		}
		if res.CheckIn != nil {
			t.recordCheckin(res)
			if len(res.CheckIn.Verdicts) > 0 {
				t.checkinVerdicts = append(t.checkinVerdicts, res.CheckIn.Verdicts)
				if len(t.reviews) > 0 {
					t.reviews[len(t.reviews)-1].Verdicts = res.CheckIn.Verdicts
				}
			}
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
				case OutcomeEscalate:
					if capped {
						// a month's or a project's cap stopped a session
						// that made calls, as the day's does
						t.deadFrom = res.SessionID
					}
				}
			} else if midNote {
				handoverNote = res.CheckIn.Handover
			}
		}
		if t.source == SourcePlan && res.Outcome == OutcomeNoPush {
			var atMain ProofsAtMain
			if err := workflow.ExecuteActivity(workflow.WithActivityOptions(t.ctx, reproOptions()), ActivityProofsAtMain, t.taskID).Get(t.ctx, &atMain); err != nil {
				workflow.GetLogger(t.ctx).Warn("proofs at main", "task", t.taskID, "error", err)
			}
			if atMain.Met {
				return t.finish("done", atMain.Reason)
			}
			own = append(own, atMain.Findings...)
		}
		if capped {
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
			// the work state rides the ask when no judgment ran, so the
			// card's question holds the findings and the review verdict
			reason := "check-in: " + cmp.Or(res.CheckIn.ReasonKind, cmp.Or(moment, "gate")) + ": " + res.Reason + workStateLines(res.CheckIn.Findings, res.CheckIn.Review)
			if t.checkinRepeat >= versionCheckinRepeat {
				mark := checkinStopMark{Kind: res.CheckIn.ReasonKind, Head: startRef, Tip: cmp.Or(res.Commit, startRef),
					ExtraLines: u.ExtraLines, RaisedSessions: u.RaisedSessions}
				if prev := t.lastCheckinStop; stopFamily(prev.Kind) == stopFamily(mark.Kind) {
					ask := repeatAsk(prev, mark, false)
					if fixed := repeatAsk(prev, mark, true); fixed != ask && t.checkinAnswers() {
						ask = fixed
					}
					reason = cmp.Or(ask, reason)
				}
				t.lastCheckinStop = mark
			}
			t.stopFindings = workStateLines(res.CheckIn.Findings, res.CheckIn.Review)
			t.head = cmp.Or(res.Commit, startRef, t.head)
			t.checkinStop = checkinStopOf(res)
			if final, why := held(t.awaitHuman(reason, nil)); final != "" {
				return t.finish(final, why)
			}
			if t.reverify {
				// the session's push is the head the answer takes through
				// the gates
				startRef = cmp.Or(res.Commit, startRef)
				continue
			}
			if t.extendRaise >= versionExtendRaise && t.raiseReload < versionRaiseReload {
				// the row holds what the console's Extend raised while the
				// card waited; the wake's grant and the next stop's marks
				// read it, or the usage the next session records would write
				// the raise away
				u = t.readUsage(u)
			}
			if t.successor >= versionSuccessor && successorChosen(t.answers) {
				// the successor exit's answer closes the card with its work
				// carried on, before any branch of the chain could buy a
				// session the close makes pointless
				return t.finish(t.closeIntoSuccessor(workStateLines(res.CheckIn.Findings, res.CheckIn.Review)))
			}
			spend := checkinSpendKind(res.CheckIn.ReasonKind)
			stop := checkinStop{spend: spend,
				callCap: spend && (strings.HasPrefix(res.Reason, checkinCallCapStop) || strings.HasPrefix(res.Reason, checkinTimeCapStop))}
			var verify bool
			if startRef, verify = t.checkinAnswer(&u, stop, t.pr.Number != 0 && startRef != "", res.Commit, startRef); verify {
				// verified below, as a dropped check-in's head is
				res.Commit = startRef
				break
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
			if t.successor >= versionSuccessor && res.Outcome == OutcomeNoPush && strings.Contains(res.Summary, pushOverBoundMark) {
				// the merge of main cannot fit the card's line bound: the
				// card closes into a successor that starts from main's tip
				// and carries the patch and the findings
				packed, _ := packCardLines(slices.Clone(res.Findings))
				return t.finish(t.closeIntoSuccessor(packed))
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
			refund()
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
		case OutcomeReplan:
			ask := "The check-in asks to send this card back to the planner: " + res.Reason + " Replan it on the board, or answer to go on."
			if t.replan >= versionReplan {
				final, refused, done, err := t.replanCut(res)
				if done {
					return final, err
				}
				ask = refused
			}
			if final, why := held(t.awaitHuman(ask, nil)); final != "" {
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
		startRef = t.adopted(v, res.Commit)
		apply(v)
	}
	return t.finish("failed", fmt.Sprintf("%d attempts exhausted; last: %s", t.cfg.MaxSessions+u.SessionsAdded(), reason))
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
	byID := workflow.GetVersion(t.ctx, "checkin-priors", workflow.DefaultVersion, versionCheckinPriors) >= versionCheckinPriors
	var groups map[string]*PriorFinding
	var alias map[string]string
	if byID {
		ask.Priors, groups, alias = t.priorFindings()
	}
	for i, f := range findings {
		c := CheckFinding{Text: f}
		if i < len(sources) {
			c.Source = sources[i]
		}
		if byID {
			c.Group = canonicalFinding(alias, findingID(f))
			if p := groups[c.Group]; p != nil {
				c.PriorConfirmed, c.PriorDropped = p.Confirmed, p.Dropped
			} else {
				c.Group = ""
			}
		} else {
			for _, verdicts := range t.checkinVerdicts {
				switch verdicts[f].State {
				case CheckConfirmed:
					c.PriorConfirmed++
				case CheckDropped:
					c.PriorDropped++
				}
			}
		}
		ask.Findings = append(ask.Findings, c)
	}
	if vs := lastVerdict(t.reviews); vs != nil {
		ask.Review = &vs[0]
	}
	return ask
}

// maxPriorFindings bounds the earlier findings a statement lists: the most
// recently judged ones.
const maxPriorFindings = 20

// canonicalFinding is the id a finding counts under: the earlier finding a
// check-in named it a repeat of, followed to the first, else its own id.
func canonicalFinding(alias map[string]string, id string) string {
	for range len(alias) + 1 {
		next, ok := alias[id]
		if !ok || next == id {
			break
		}
		id = next
	}
	return id
}

// priorFindings groups every check-in's verdicts by the finding they judged:
// a verdict counts under its finding id, or under the id of the finding it
// repeats. A check-in's verdicts on one finding count once. It returns the
// maxPriorFindings groups judged most recently, newest first, all groups by
// id, and the aliases that made them.
func (t *taskRun) priorFindings() (list []PriorFinding, groups map[string]*PriorFinding, alias map[string]string) {
	groups, alias = map[string]*PriorFinding{}, map[string]string{}
	var order []string
	for _, verdicts := range t.checkinVerdicts {
		for _, text := range slices.Sorted(maps.Keys(verdicts)) {
			if id := findingID(text); verdicts[text].Repeats != "" && verdicts[text].Repeats != id {
				alias[id] = verdicts[text].Repeats
			}
		}
	}
	for _, verdicts := range t.checkinVerdicts {
		counted := map[string]bool{}
		for _, text := range slices.Sorted(maps.Keys(verdicts)) {
			id := canonicalFinding(alias, findingID(text))
			p := groups[id]
			if p == nil {
				p = &PriorFinding{ID: id, Text: text}
				groups[id] = p
			}
			if counted[id] {
				continue
			}
			counted[id] = true
			order = append(slices.DeleteFunc(order, func(o string) bool { return o == id }), id)
			switch verdicts[text].State {
			case CheckConfirmed:
				p.Confirmed++
			case CheckDropped:
				p.Dropped++
			}
		}
	}
	slices.Reverse(order)
	for _, id := range order[:min(len(order), maxPriorFindings)] {
		list = append(list, *groups[id])
	}
	return list, groups, alias
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
		head = t.adopted(last, head)
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

// drainCompanions takes a pending companion_landed (versionCompanions):
// the poller sent it because every pin the card carries reads met, so the
// gates read again from the first stage.
func (t *taskRun) drainCompanions() bool {
	var sig TaskSignal
	return workflow.GetSignalChannel(t.ctx, SignalCompanionLanded).ReceiveAsync(&sig)
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
// guards the money. It reads the caps in force, so the policy loads
// signalled before it are dropped (drainLoads).
func (t *taskRun) unfunded(model string) string {
	t.drainLoads()
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

// repeatAsk is the ask a check-in stop with mark asks instead of its
// own when it repeats the stop with prev, "" for none: the restart's
// unchanged head when prev's answer bought one, else the deadlock's when
// the head and the card's limits are unchanged. fixed compares as a run at
// versionCheckinAnswer does: the branch's head and the sessions the console
// raised.
func repeatAsk(prev, mark checkinStopMark, fixed bool) string {
	was, head := prev.Head, mark.Head
	same := prev.ExtraLines == mark.ExtraLines
	if fixed {
		was, head = prev.Tip, mark.Tip
		same = same && prev.RaisedSessions == mark.RaisedSessions
	}
	if was == "" || was != head {
		return ""
	}
	sha := head
	if len(sha) > 7 {
		sha = sha[:7]
	}
	label := checkinSpendAsk
	if checkinFailedKind(mark.Kind) {
		label = "check-in: " + mark.Kind + ": "
	}
	switch {
	case prev.Granted:
		return label + fmt.Sprintf("The restart produced no new commit — the head is still %s. What should the agent do?", sha)
	case same && !checkinFailedKind(mark.Kind):
		return label + fmt.Sprintf("%s: the head %s and the card's limits are unchanged since the last stop. Extend the card, overrule the review, end the card, or leave it waiting.", deadlockAskMark, sha)
	}
	return ""
}

// checkinStop is a check-in stop as its answer reads it: under spend, and
// of those the call cap or the time cap reached.
type checkinStop struct{ spend, callCap bool }

// checkinAnswer takes a human's answer to a check-in stop whose session
// started from head and pushed pushed, "" for nothing. from is the head
// the card goes on from: the session's own push (versionCheckinAnswer).
// verify: the answer verifies from again instead of buying the session,
// as a spend stop's does with the pull request open (versionCheckinSpend);
// the call cap's and the time cap's buys the session (versionCheckinRaise).
func (t *taskRun) checkinAnswer(u *TaskUsage, s checkinStop, open bool, pushed, head string) (from string, verify bool) {
	if pushed != "" && pushed != head && t.checkinAnswers() {
		head = pushed
	}
	switch {
	case s.callCap && t.checkinRaise >= versionCheckinRaise:
		t.lastCheckinStop.Granted = true
	case s.spend && open && t.checkinSpend >= versionCheckinSpend:
		return head, true
	}
	return head, false
}

// checkinTime is the time res's start check-in took, which is no agent time
// of the card (versionCheckinClock): the check-in is the factory's own work,
// and a mid-session one runs inside the session's own clock.
func (t *taskRun) checkinTime(res SessionResult) time.Duration {
	if t.checkinClock < versionCheckinClock || res.CheckIn == nil || res.CheckIn.Record == nil || res.CheckIn.Record.Moment == CheckInSession {
		return 0
	}
	return res.CheckIn.Record.Duration
}

// checkinStopOf is the check-in spend stop res is (CheckinStop*), from its
// check-in's own outcome, "" for a stop that is not a spend stop's.
func checkinStopOf(res SessionResult) string {
	switch {
	case res.CheckIn == nil || !checkinSpendKind(res.CheckIn.ReasonKind):
		return ""
	case strings.HasPrefix(res.Reason, checkinTimeCapStop):
		return CheckinStopTimeCap
	case strings.HasPrefix(res.Reason, checkinCallCapStop):
		return CheckinStopCallCap
	}
	return ""
}

// checkinAnswers: the run takes the checkin-answer change
// (versionCheckinAnswer), read where an answer or a repeat first differs.
func (t *taskRun) checkinAnswers() bool {
	return workflow.GetVersion(t.ctx, "checkin-answer", workflow.DefaultVersion, versionCheckinAnswer) >= versionCheckinAnswer
}

// grantTime is an answer's agent time: when less than answerTime of agent
// time is left, the difference, so the attempt is not born at the cap.
func (t *taskRun) grantTime(u *TaskUsage) {
	if t.v5 < versionPersistUsage {
		return
	}
	if left, slice := t.cfg.WallClockCap+u.TimeAdded()-u.AgentTime, t.answerTime(); left < slice {
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
		return answerSlice(t.cfg.WallClockCap)
	}
	return min(t.cfg.AnswerSlice, t.cfg.WallClockCap)
}

// answerSlice is the agent time an answer leaves the next session under
// the cap: a quarter of it, never under the 5 minutes a session needs to
// start, never over the cap itself.
func answerSlice(cap time.Duration) time.Duration {
	return max(cap/4, min(minSession, cap))
}

// sizeAgentTime sets the card's agent time by its size (versionSizeTime):
// the class's time halved for a small card, raised by half for a large
// one (policy.SizeAgentTime). A card no size binds keeps the class's.
func (t *taskRun) sizeAgentTime(size string) {
	if t.sizeTime >= versionSizeTime && size != "" {
		t.cfg.WallClockCap = policy.SizeAgentTime(size, t.agentTimeBase)
	}
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

// recordGrant records grant d the run made on by's answer, u the usage
// after it, as by's draw against the pool (versionCases), once (grantKey).
// The grant stands when the row fails, as the usage's does.
func (t *taskRun) recordGrant(by string, d policy.Draw, u TaskUsage) {
	key := grantKey(workflow.GetInfo(t.ctx).WorkflowExecution.RunID, u)
	if err := workflow.ExecuteActivity(t.short, ActivityRecordGrant, t.taskID, by, d, key).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("record the grant's draw", "task", t.taskID, "error", err)
	}
}

// recordAuditDraw records grant d, which no writer's answer bought (the
// audit's gap verdict at a cap), u the usage after it, as the factory's
// own draw, rung 0 against the pool (versionAuditDraw), once (grantKey).
// The grant stands when the row fails, as the usage's does; a refused
// draw (an empty pool) returns false: it buys nothing and the card asks
// a human.
func (t *taskRun) recordAuditDraw(d policy.Draw, u TaskUsage) bool {
	key := grantKey(workflow.GetInfo(t.ctx).WorkflowExecution.RunID, u)
	if err := workflow.ExecuteActivity(t.short, ActivityAuditDraw, t.taskID, d, key).Get(t.ctx, nil); err != nil {
		if drawRefused(err) {
			return false
		}
		workflow.GetLogger(t.ctx).Warn("record the audit's draw", "task", t.taskID, "error", err)
	}
	return true
}

// recordAgentDraw records grant d, which the project agent's change request
// at the cap bought (by, no person), u the usage after it, as rung 2's draw
// against the pool (versionAgentDraw), once (grantKey). The grant stands
// when the row fails, as the usage's does; a refused draw (an empty pool)
// returns false: it buys nothing and the card asks a human.
func (t *taskRun) recordAgentDraw(by string, d policy.Draw, u TaskUsage) bool {
	key := grantKey(workflow.GetInfo(t.ctx).WorkflowExecution.RunID, u)
	if err := workflow.ExecuteActivity(t.short, ActivityAgentDraw, t.taskID, by, d, key).Get(t.ctx, nil); err != nil {
		if drawRefused(err) {
			return false
		}
		workflow.GetLogger(t.ctx).Warn("record the agent's draw", "task", t.taskID, "error", err)
	}
	return true
}

// grantKey is the identity of a grant of the run runID, u the usage after
// it: the run and the grant's own counters, which every grant raises, so
// two grants at one attempt are two rows and a retry of one meets its own.
func grantKey(runID string, u TaskUsage) string {
	return identityKey(EventCardExtended, runID, strconv.Itoa(u.ExtraAttempts), u.ExtraTime.String())
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

// readUsage reads the caps the task's row holds (ActivityReadUsage): the
// answer wake reloads them, so a raise the console's Extend made while the
// card waited survives the wake's own grant (versionExtendRaise). A
// failure keeps the usage the run holds.
func (t *taskRun) readUsage(held TaskUsage) TaskUsage {
	var u TaskUsage
	if err := workflow.ExecuteActivity(t.short, ActivityReadUsage, t.taskID).Get(t.ctx, &u); err != nil {
		workflow.GetLogger(t.ctx).Warn("read usage", "task", t.taskID, "error", err)
		return held
	}
	return u
}

// readRaise takes the console's raises from the row into u
// (versionRaiseReload): the sessions, agent time and lines an Extend
// added, which the workflow never writes, so the row's numbers are the
// truth and the rest of u stays the run's own.
func (t *taskRun) readRaise(u *TaskUsage) {
	if t.raiseReload < versionRaiseReload {
		return
	}
	row := t.readUsage(*u)
	u.RaisedSessions, u.RaisedTime, u.ExtraLines = row.RaisedSessions, row.RaisedTime, row.ExtraLines
}

// verify takes a pushed commit through its pull request, the checks and
// the review wait. A card for a pull request factoryd opened outside any
// card's workflow (roadmap 187) has that pull request already: with
// versionFactoryPRCard and RunState's FactoryPR set, verify skips
// OpenPullRequest and takes the head straight to the stages.
func (t *taskRun) verify(commit string) (v verdict) {
	t.judged = append(t.judged, commit)
	t.head = commit
	defer func() { v.head = t.head }()
	if t.factoryPRCard >= versionFactoryPRCard && t.orphanPR {
		if err := t.copyUpstream(commit); err != nil {
			return verdict{final: "failed", reason: "copy upstream images: " + errMessage(err)}
		}
		if err := workflow.ExecuteActivity(t.short, ActivitySetState, t.taskID, "in-review").Get(t.ctx, nil); err != nil {
			return verdict{final: "failed", reason: "set in-review: " + errMessage(err)}
		}
		return t.stages(commit)
	}
	if err := t.copyUpstream(commit); err != nil {
		return verdict{final: "failed", reason: "copy upstream images: " + errMessage(err)}
	}
	if err := workflow.ExecuteActivity(t.short, ActivityOpenPullRequest, t.taskID, commit).Get(t.ctx, &t.pr); err != nil {
		if t.wipStart >= versionWipStart && isWipHead(err) {
			// an interim head is no finished work and no gate judged it: a
			// session goes on from it
			t.judged = t.judged[:len(t.judged)-1]
			return verdict{wip: true, findings: []string{"the branch's head " + commit + " is a session's work in progress, which no gate judges: continue from it and finish the work"}, reason: "work in progress"}
		}
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
		return verdict{final: "failed", reason: fmt.Sprintf("checks did not finish within %s", t.checksBound())}
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
	return verdict{findings: requested, reason: "changes requested", requested: true, by: t.changesBy}
}

// resumeVerify is a resumed run's answer-verify (versionResumeHold):
// verify from the first stage and, where the gates refuse a head the
// agent moved past them — a run from before the head-holds change meets
// the refusal whole, for its gates hold no moved head — adopt the
// branch's head and verify it instead, its checks waited on, the same
// hold the normal gate path holds (versionHeadHolds), bounded by
// maxHeadHolds. head is the head the answer re-entered the gate on; it
// takes the head the run adopted.
func (t *taskRun) resumeVerify(head *string) verdict {
	v := t.verify(*head)
	for t.resumeHold >= versionResumeHold && v.final != "" && v.moved && t.holds < maxHeadHolds {
		var branch string
		if err := workflow.ExecuteActivity(t.short, ActivityBranchHead, t.taskID).Get(t.ctx, &branch); err != nil {
			return verdict{final: "failed", reason: "read the branch head: " + errMessage(err)}
		}
		if branch == "" || branch == *head {
			break // a raced read: the gate's refusal stands
		}
		t.holds++
		*head = branch
		v = t.verify(branch)
	}
	*head = t.adopted(v, *head)
	return v
}

// adopted is the head a verification of commit ended on (verdict.head):
// one a stage adopted past commit, judged as commit was
// (versionVerifiedHead); commit when none did, or in a run from before.
func (t *taskRun) adopted(v verdict, commit string) string {
	if v.head == "" || v.head == commit ||
		workflow.GetVersion(t.ctx, "verified-head", workflow.DefaultVersion, versionVerifiedHead) < versionVerifiedHead {
		return commit
	}
	t.judged = append(t.judged, v.head)
	return v.head
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
	if g.Policy.OverBound != "" {
		return verdict{stronger: true, reason: "over the card's line bound", findings: []string{g.Policy.OverBound}}
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
		v, decided, err := t.gateActivity(true, fanOutOptions(), &review, ActivityModelReview, t.taskID, t.pr, commit, g)
		if decided {
			return v
		}
		if err != nil {
			review = Review{Note: "model review failed: " + errMessage(err)}
		}
	} else {
		reviewCtx := workflow.WithActivityOptions(t.ctx, fanOutOptions())
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
	// an approval signalled before this request (for an earlier head) is
	// not one of this one
	var sig TaskSignal
	for workflow.GetSignalChannel(t.ctx, SignalApproved).ReceiveAsync(&sig) {
		t.recordIgnored(SignalApproved, sig, "sent before this approval request")
	}
	if err := workflow.ExecuteActivity(t.short, ActivityRequestApproval, t.taskID,
		ApprovalInput{PR: t.pr, Commit: commit, Gates: g, Review: review, WhyHuman: whyHuman}).Get(t.ctx, nil); err != nil {
		return verdict{final: "failed", reason: "request approval: " + errMessage(err)}
	}
	if t.v11 >= versionGateStarted {
		t.approvalRequested = workflow.Now(t.ctx)
	}
	return t.awaitApproval(commit, g, nil)
}

// autoMerge merges on the project's merge rule auto (versionAutoMerge):
// the note on the timeline, then the gate's approval naming met and the
// merge as factory-merge. done with the task's verdict, or why it did not
// go through: the card then. A person's replan that waited behind the
// gates goes first (versionReplan): the card goes to the planner, or, on
// a refused switch, merges.
func (t *taskRun) autoMerge(commit string, g GateResults, rv Review, met []string) (v verdict, done bool, failed string) {
	if t.replans() {
		if sw, step, ok := t.takeToPlan(); ok {
			if final, why, switched := t.switchFromWait(sw, step, "in-review"); switched {
				return verdict{final: final, reason: why}, true, ""
			}
		}
	}
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
// task, a reviewer requests changes, a member re-runs a factory pull
// request's checks (versionFactoryPRActions), a person's replan sends a
// direct card to the planner (versionReplan), or an approval merges the
// pull request as factory-merge. A queued card waits out of its merge
// queue, and an approval takes a turn first (versionQueueWaits): a head
// the turn moved merges on the approval when main's new commits share no
// file with the pull request (the cheap turn), else it goes back to
// approve as synced. A failed merge keeps the turn through its retries,
// and a new approval wait leaves the queue again. An approval over
// findings the card showed adjudicates them (versionAdjudicated). A merge
// Forgejo refuses keeps the wait, but a queue head's red checks take it
// back through its turn while re-runs are left (versionRedHeadRestage);
// one that fails otherwise keeps the approval and is tried again after
// mergeRetryAfter (versionGatesPinned). A press on a plan's integration
// card runs the criteria gate again for its own criteria and holds for a
// human's (regate), the gate's stand kept in g, whose AutoMerge met rides
// the merge.
func (t *taskRun) awaitApproval(commit string, g GateResults, covered []string) verdict {
	approver := ""
	var retry workflow.Future
	var cancelRetry workflow.CancelFunc
	failures := 0 // failed retries of the current approval's merge (versionMergeRetryCap)
	for {
		retried := false
		sel := workflow.NewSelector(t.ctx)
		for _, n := range []string{SignalPRMerged, SignalPRClosed, SignalIssueClosed, SignalChangesRequested, SignalApproved} {
			sel.AddReceive(workflow.GetSignalChannel(t.ctx, n), func(workflow.ReceiveChannel, bool) {})
		}
		if t.rolling {
			sel.AddReceive(workflow.GetSignalChannel(t.ctx, SignalRollFolded), func(workflow.ReceiveChannel, bool) {})
		}
		if t.factoryActions() {
			sel.AddReceive(workflow.GetSignalChannel(t.ctx, SignalRerunChecks), func(workflow.ReceiveChannel, bool) {})
		}
		if t.replans() {
			sel.AddReceive(workflow.GetSignalChannel(t.ctx, SignalToPlan), func(workflow.ReceiveChannel, bool) {})
		}
		if t.companions >= versionCompanions {
			sel.AddReceive(workflow.GetSignalChannel(t.ctx, SignalCompanionLanded), func(workflow.ReceiveChannel, bool) {})
		}
		if retry != nil {
			sel.AddFuture(retry, func(workflow.Future) { retried = true })
		}
		sel.Select(t.ctx)
		if final, why, ok := t.decided(); ok {
			return verdict{final: final, reason: why}
		}
		if findings := t.drainChanges(); len(findings) > 0 {
			return verdict{findings: findings, reason: "changes requested", requested: true, by: t.changesBy}
		}
		if t.rolling && t.drainFolds() {
			return verdict{reason: "newer images folded in"}
		}
		if t.factoryActions() && t.drainReruns() {
			return verdict{rerun: true, reason: "the checks run again"}
		}
		if t.companions >= versionCompanions && t.drainCompanions() {
			return verdict{recheck: true, reason: "a companion reached its pin"}
		}
		if t.replans() {
			if sw, step, ok := t.takeToPlan(); ok {
				if final, why, done := t.switchFromWait(sw, step, "in-review"); done {
					return verdict{final: final, reason: why}
				}
				return verdict{reask: true, reason: "the switch to the planner was refused"}
			}
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
		if signalled && t.source == SourcePlan {
			var stand *CriteriaStand
			v, stop, held, stand := t.regate(commit, g, approver)
			if stand != nil {
				g.Criteria = stand
			}
			if stop {
				return v
			}
			if held {
				approver = ""
				if t.v11 >= versionGateStarted {
					t.approvalRequested = workflow.Now(t.ctx)
				}
				continue
			}
		}
		if signalled && t.waitsOutOfQueue() {
			sync, v, stop := t.turn(commit)
			if stop {
				return v
			}
			if sync.To != commit {
				if t.cheapTurn < versionCheapTurn || len(sync.Overlap) > 0 {
					return verdict{synced: true, head: sync.To}
				}
				// the cheap turn: main's new commits share no file with the
				// pull request, so Forgejo keeps the approval on the head
				// the sync made, and the checks that held for commit hold
				t.checkedHead = cmp.Or(t.checkedHead, commit)
				commit, t.syncedHead, t.head = sync.To, sync.To, sync.To
			}
		}
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
		var met []string
		if g.Criteria != nil {
			_, met, _ = AutoMerge(g, t.newestReview(), t.source)
		}
		var res MergeResult
		v, decided, err := t.gateActivity(false, opts, &res, ActivityMerge, t.mergeArgs(commit, approver, met)...)
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
			if t.waitsOutOfQueue() {
				t.leaveQueue(approvalWaitLeft)
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
			if t.waitsOutOfQueue() {
				t.leaveQueue(approvalWaitLeft)
			}
		}
	}
}

// regate runs the gate again, on a press that would merge a plan's
// integration card, for its never-judged criteria, and holds for a
// human's; stand is the gate's stand after its run again, nil when it did
// not run. The gate's findings go where approval's do: stop sends them
// back to the agent; at the cap a new merge ask carries them, since the
// press decided the old one, and held answers. Its card lines, which no
// session settles, ride a new ask too, held. A criterion still without a
// row holds the merge (HoldMerge), held too. Neither lets the merge go. A
// failed first read of the criteria leaves the decision to the merge's
// guard.
func (t *taskRun) regate(commit string, g GateResults, approver string) (v verdict, stop, held bool, stand *CriteriaStand) {
	var never Unjudged
	if err := workflow.ExecuteActivity(t.short, ActivityUnjudged, t.taskID).Get(t.ctx, &never); err != nil {
		workflow.GetLogger(t.ctx).Warn("unjudged criteria", "task", t.taskID, "error", err)
		return verdict{}, false, false, nil
	}
	if len(never.Gate) > 0 {
		var res CriteriaResult
		v, decided, err := t.criteria(&res, commit)
		if err != nil {
			return verdict{final: "failed", reason: "criteria gate: " + errMessage(err)}, true, false, nil
		}
		if decided {
			return v, true, false, nil
		}
		stand, g.Criteria = res.Stand, res.Stand
		var capped, card []string
		if len(res.Findings) > 0 {
			if v, stop := t.onFindings(criteriaStage, g, res.Findings, &capped, &card); stop {
				return v, true, false, stand
			}
		}
		var lines []string
		if len(capped) > 0 {
			lines = slices.Concat(capped, t.history, card)
		}
		if lines = append(lines, res.Card...); len(lines) > 0 {
			if err := t.askAgain(commit, g, lines); err != nil {
				return verdict{final: "failed", reason: "request approval: " + errMessage(err)}, true, false, stand
			}
			held = true
			if len(capped) > 0 {
				return verdict{}, false, true, stand
			}
		}
		never = Unjudged{}
		if err := workflow.ExecuteActivity(t.short, ActivityUnjudged, t.taskID).Get(t.ctx, &never); err != nil {
			workflow.GetLogger(t.ctx).Warn("unjudged criteria", "task", t.taskID, "error", err)
			return verdict{}, false, held, stand
		}
	}
	if len(never.Gate) == 0 && len(never.Human) == 0 {
		return verdict{}, false, held, stand
	}
	if err := workflow.ExecuteActivity(t.short, ActivityHoldMerge, t.taskID, t.pr.Number, commit, approver).Get(t.ctx, nil); err != nil {
		// the press decided the request, so a hold that failed leaves the
		// card waiting with no ask to press: the log names it
		workflow.GetLogger(t.ctx).Error("hold the merge: the ask may be off the human's surface", "task", t.taskID, "error", err)
	}
	return verdict{}, false, true, stand
}

// askAgain files the merge ask on commit again, the press having decided
// the last one: lines first, then the last ask's card lines, then why
// AutoMerge on g leaves the merge to a human, with the last ask's reviews,
// as approve asks. An approval sent before it is none of it.
func (t *taskRun) askAgain(commit string, g GateResults, lines []string) error {
	var sig TaskSignal
	for workflow.GetSignalChannel(t.ctx, SignalApproved).ReceiveAsync(&sig) {
		t.recordIgnored(SignalApproved, sig, "sent before this approval request")
	}
	card := slices.Clone(lines)
	for _, l := range t.askedCard {
		card = appendNew(card, l)
	}
	in := t.asked
	in.Commit, in.Gates, in.WhyHuman = commit, g, slices.Clone(card)
	_, _, unmet := AutoMerge(g, in.Review, t.source)
	for _, l := range unmet {
		in.WhyHuman = appendNew(in.WhyHuman, l)
	}
	if err := workflow.ExecuteActivity(t.short, ActivityRequestApproval, t.taskID, in).Get(t.ctx, nil); err != nil {
		return err
	}
	t.asked, t.askedCard = in, card
	return nil
}

// newestReview is the run's newest model review, none before the first.
func (t *taskRun) newestReview() Review {
	if len(t.reviews) == 0 {
		return Review{}
	}
	return t.reviews[len(t.reviews)-1]
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
// mergeRetryAfter, an hour.
const maxMergeRetries = 6

// mergeRetryAfter is how long a merge that failed (Forgejo unreachable,
// not refusing) waits before the kept approval is tried again.
const mergeRetryAfter = 10 * time.Minute

// mergeWaitAfter is how long a merge refused with 405 "Please try again
// later" (Forgejo's conflict check on the head still running) waits
// before the pull request is read again and the merge tried once more.
const mergeWaitAfter = 2 * time.Second

// maxMergeWaits bounds those waits within one merge.
const maxMergeWaits = 5

// conflictVerdict sends a pull request that no longer merges into main
// back to the agent. The worker bases its commit on the main commit the
// merge brings in, so main's changes, .forgejo/ among them, stay main's.
func conflictVerdict(refusal string) verdict {
	return verdict{reason: "the pull request no longer merges into main", conflict: true, findings: []string{
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

// requestPlanOptions: asking for a plan also proposes its hold-outs, one
// review model call; it heartbeats while the call runs.
func requestPlanOptions() workflow.ActivityOptions {
	budget := policy.ReviewParts*policy.MaxSupervisorTimeout + 10*time.Minute
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

// reviewActivityMax bounds one attempt of a review or audit, whatever the
// parts of its diff: the heartbeat finds a lost worker and a cancel reaches
// the calls; the gate watch alerts at the parts' bound (boundByParts),
// fanOutTripMargin before it at the latest.
const reviewActivityMax = 6 * time.Hour

// fanOutOptions: one review or audit makes a call for each part of its
// diff; it heartbeats.
func fanOutOptions() workflow.ActivityOptions {
	o := reviewOptions()
	o.StartToCloseTimeout, o.ScheduleToCloseTimeout = reviewActivityMax, 3*reviewActivityMax
	return o
}

// reviewOptions: the gate supervisor's call and a screening answer's,
// bounded as a review of policy.ReviewParts calls of the longest
// supervisor_timeout was; they heartbeat, so a cancel reaches the call.
func reviewOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		HeartbeatTimeout:       2 * time.Minute,
		StartToCloseTimeout:    policy.ReviewParts*policy.MaxSupervisorTimeout + 2*time.Minute,
		ScheduleToCloseTimeout: 3*policy.ReviewParts*policy.MaxSupervisorTimeout + 10*time.Minute,
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
		// SettleHoldouts, which only a history that recorded hold-out reds
		// calls, settles at once: a short activity's options bound it
		v, decided, err = t.gateActivity(true, finishOptions(), &res, ActivityHoldoutSettlement, t.taskID, t.cfg.PolicyVersion, t.pr.Number, commit, out.Holdouts)
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
			return verdict{findings: res.Findings, reason: "the hold-out settlement has findings", requested: true}, true, nil
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
// gateEscalated describes. A successor chosen there
// closes the card into one, a close waits for the issue's close,
// rerun_checks runs the head's checks again and takes the head the rerun
// made through the gates, and a handover sets the next session's model
// before the gate runs again.
func (t *taskRun) gateAnswered(final, why string, changes []string) (verdict, bool) {
	res := t.takeResolution()
	switch {
	case final != "":
		return verdict{final: final, reason: why}, true
	case len(changes) > 0:
		return verdict{findings: changes, reason: "changes requested", requested: true, by: t.changesBy}, true
	case res != nil && res.Action == ActSuccessor:
		final, why := t.closeIntoSuccessor("")
		return verdict{final: final, reason: why}, true
	case res != nil && res.Action == ActClose:
		final, why := t.awaitClose()
		return verdict{final: final, reason: why}, true
	case res != nil && res.Action == ActRerunChecks:
		head := t.head
		if err := workflow.ExecuteActivity(t.short, ActivityRerunChecks, t.taskID, head).Get(t.ctx, &head); err != nil {
			return verdict{final: "failed", reason: "re-run the checks: " + errMessage(err)}, true
		}
		t.rerunHead = head
		// the gates beside the old head's checks stop: the new head's
		// checks wait runs its own
		t.cancelBeside(checksFailed)
		return t.verify(head), true
	case res != nil && res.Action == ActHandover:
		t.nextModel = t.handoverModel("", res.Args)
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
// changes nothing, as run's.
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
	// 2 x 90 s against 2 min; the checks wait from versionChecksWait on,
	// whose poll ends a minute after the first trip's supervisor budget, so
	// a second wait could not run, offers none);
	// a retry on the last attempt would fail the activity
	maxAttempts := run.opts.RetryPolicy.MaximumAttempts
	in := GateSupervision{Trip: trip, Input: run.args, CanWait: !run.waited && trip.Activity != ActivityRunSession && 2*trip.Bound < run.opts.StartToCloseTimeout &&
		!(trip.Activity == ActivityWaitChecks && t.checksWait >= versionChecksWait),
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

// copyUpstream copies the image lines commit adds to upstream-images.txt
// into the store (versionUpstreamCopy); what the upstream cannot serve is
// the card's finding, and the run goes on.
func (t *taskRun) copyUpstream(commit string) error {
	if t.upstreamCopy < versionUpstreamCopy || slices.Contains(t.copiedHeads, commit) {
		return nil
	}
	ctx := workflow.WithActivityOptions(t.ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Duration(policy.DefaultBounds[ActivityCopyUpstreamImages]) * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        10 * time.Second,
			MaximumAttempts:        3,
			NonRetryableErrorTypes: []string{errTerminal},
		},
	})
	var findings []string
	if err := workflow.ExecuteActivity(ctx, ActivityCopyUpstreamImages, t.taskID, commit).Get(ctx, &findings); err != nil {
		return err
	}
	t.copiedHeads = append(t.copiedHeads, commit)
	if len(findings) > 0 {
		t.recordFindings("upstream-copy", "card", 0, findings)
		t.remember("upstream-copy", 0, findings)
	}
	return nil
}

// checksPollMargin is how long the checks wait's poll ends before its
// activity's deadline.
const checksPollMargin = time.Minute

// legacyChecksTimeout is the checks wait's limit for a run from before
// versionChecksWait.
const legacyChecksTimeout = 45 * time.Minute

// checksBound is how long the checks wait runs before the gate watch trips:
// the bound of the card's project (waits.checks, policy.Tree.ChecksBound);
// a run from before versionChecksWait keeps 45m.
func (t *taskRun) checksBound() time.Duration {
	if t.checksWait < versionChecksWait {
		return legacyChecksTimeout
	}
	return cmp.Or(t.cfg.Task.ChecksBound, policy.DefaultWaits.Checks)
}

// checksOptions: the checks wait outlives its bound by the gate
// supervisor's whole budget, so the trip at the bound is the one stop path
// (versionChecksWait): its start-to-close is the bound, a minute of the
// supervisor's reads (bundleReads), the pinned supervisor_timeout and two
// minutes; its poll ends one minute before that. A run from before keeps
// its recorded 45m + 1m.
func (t *taskRun) checksOptions() workflow.ActivityOptions {
	timeout := t.checksBound() + checksPollMargin
	if t.checksWait >= versionChecksWait {
		timeout += bundleReads + cmp.Or(t.cfg.Task.SupervisorTimeout, policy.MaxSupervisorTimeout) + time.Minute
	}
	return workflow.ActivityOptions{
		StartToCloseTimeout:    timeout,
		ScheduleToCloseTimeout: timeout,
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
			t.changesBy = sig.Author
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

// awaitClose waits for the issue's close a close resolution made
// (versionCases): Act closed the issue before it resolved the case, and
// the run ends on its issue_closed. A change request that comes first
// changes nothing on a closing card.
func (t *taskRun) awaitClose() (final, why string) {
	for {
		if final, why, _ := t.awaitDecision(); final != "" {
			return final, why
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

// waitEnd ends a wait for a human on its own, with no answer: a cap's
// own end (capWait) or an image's arrival (imageReady). name says what
// ended it.
type waitEnd interface {
	start(t *taskRun)
	add(sel workflow.Selector)
	over() bool
	stop()
	name() string
}

// awaitHumanUntil is awaitHuman that also ends, with no answer, when end
// does (versionDayCap, versionToolPark), and nil never does. A stop rung 0
// resolved waits for nothing (versionCases).
func (t *taskRun) awaitHumanUntil(reason string, choices []string, end waitEnd) (final, why string, answer []string) {
	question := escalationQuestion(reason)
	if err := t.ask(reason, question, choices); err != nil {
		return "failed", escalateFailed + errMessage(err), nil
	}
	if t.resolution != nil {
		return "", "", nil
	}
	return t.awaitAnswer(question, end)
}

// queuedCase is a case_resolved this run took for a case it did not wait
// on (versionCaseQueue): its signal, and when the answer came.
type queuedCase struct {
	signal CaseSignal
	at     time.Time
}

// ask escalates the task with reason, which asks question, and the
// agent's choices, if any: only comments newer than its comment answer
// it (answerOf). The stop opens its case (OpenTrip). The
// resolution of one rung 0 resolved is the run's, and no one was asked,
// else the run waits on the case and its ask's comment. At versionCaseQueue
// a wait that comes to a case whose answer came while the run waited on
// another takes it at once, as one taken live.
func (t *taskRun) ask(reason, question string, choices []string) error {
	t.resolution, t.caseID = nil, 0
	var out TripOutcome
	in := TripInput{TaskID: t.taskID, Reason: reason, Choices: choices, Head: t.head, CheckinStop: t.checkinStop, Killed: t.killed}
	if t.floorModel != nil && workflow.GetVersion(t.ctx, "trip-model", workflow.DefaultVersion, versionTripModel) >= versionTripModel {
		in.Model = *t.floorModel
	}
	t.checkinStop = ""
	if err := workflow.ExecuteActivity(t.short, ActivityOpenTrip, in).Get(t.ctx, &out); err != nil {
		return err
	}
	if out.Resolution != nil {
		t.resolution = out.Resolution
		return nil
	}
	t.caseID, t.escalation = out.CaseID, out.Comment
	t.anchors = append(t.anchors, anchor{comment: t.escalation, question: question})
	if q, ok := t.resolvedQueue[out.CaseID]; ok && t.caseQueue >= versionCaseQueue {
		// the answer came while the run waited on another case: this wait
		// takes it at once, as one taken live (versionCaseQueue)
		delete(t.resolvedQueue, out.CaseID)
		res := q.signal.Res
		t.resolution = &res
		if note := strings.TrimSpace(res.Note); note != "" || q.signal.Comment != 0 {
			t.insertAnswer(t.resolutionKey(q.signal.CaseID, q.signal.Comment), answerText(q.signal.By, question, cmp.Or(note, res.Sentence()), q.at))
		}
	}
	return nil
}

// newestAnchor is the comment of the newest question the run asked.
func (t *taskRun) newestAnchor() int64 {
	var newest int64
	for _, a := range t.anchors {
		if !a.request {
			newest = max(newest, a.comment)
		}
	}
	return newest
}

// takeFaced takes a case_faced signal (versionCaseFaced, read where the
// signal is taken, so a run that waited on a case before the change takes
// it as well): the comment of a face posted after the trip returned
// becomes the escalation the wait counts replies after, and the comment of
// the gate beside the checks whose ask the case is. It answers whether the
// wait's own case was faced.
func (t *taskRun) takeFaced(sig TaskSignal, question string, stopped ...*besideRun) bool {
	c := sig.Case
	if c == nil || c.Comment == 0 || workflow.GetVersion(t.ctx, "case-faced", workflow.DefaultVersion, versionCaseFaced) < versionCaseFaced {
		return false
	}
	for _, f := range slices.Concat(t.beside, stopped) {
		if f.caseID == c.CaseID && f.comment == 0 {
			f.comment = c.Comment
			t.anchors = append(t.anchors, anchor{comment: c.Comment, question: escalationQuestion(f.asked)})
		}
	}
	if t.caseID == 0 || c.CaseID != t.caseID || c.Comment <= t.escalation {
		return false
	}
	t.escalation = c.Comment
	t.anchors = append(t.anchors, anchor{comment: c.Comment, question: question})
	return true
}

// takeResolution is the resolution the last wait took, which it clears:
// nil for none.
func (t *taskRun) takeResolution() *Resolution {
	res := t.resolution
	t.resolution = nil
	if res != nil && len(res.Verdicts) > 0 && workflow.GetVersion(t.ctx, "case-verdicts", workflow.DefaultVersion, versionCaseVerdicts) >= versionCaseVerdicts {
		t.checkinVerdicts = append(t.checkinVerdicts, res.Verdicts)
		if len(t.reviews) > 0 {
			t.reviews[len(t.reviews)-1].Verdicts = res.Verdicts
		}
	}
	return res
}

// handoverModel is the model a handover's args name against model, the
// session's: the stronger rung's, the cheap rung's, or model itself.
func (t *taskRun) handoverModel(model string, args map[string]any) string {
	switch args["model"] {
	case "stronger":
		return t.cfg.Models.Stronger
	case "cheaper":
		return t.cfg.Models.Start
	}
	return model
}

// awaitAnswer is awaitHumanUntil's wait for the answer to the escalation
// asked last, which asked question. A person's replan of a direct coding
// card ends it with the switch to the planner (versionReplan). The case's
// resolution (case_resolved) ends it at once, its
// note an answer under the comment that carries it, so the webhook's copy
// of that comment is no second answer. A plain comment is a continue with
// it as the note, and a wait that ends on its own withdraws its case.
func (t *taskRun) awaitAnswer(question string, end waitEnd) (final, why string, answer []string) {
	if end != nil {
		end.start(t)
		defer end.stop()
	}
	wake := []string{SignalPRMerged, SignalPRClosed, SignalIssueClosed, SignalChangesRequested, SignalHumanComment, SignalCaseResolved, SignalCaseFaced}
	t.inCaseWait, t.kept = true, nil
	defer t.leaveWait("")
	replans := t.replans()
	if replans {
		wake = append(wake, SignalToPlan)
	}
	// at is where the wait's next answer goes in t.answers, newest first:
	// a burst reads in the order it was written (versionAnswerBurst)
	at := 0
	// resolved: a case_resolved of the wait's case came (versionCases)
	resolved := false
	pass := func() (heard []int64, stop bool) {
		if final, why, stop = t.decided(); stop {
			return nil, true
		}
		// before the comments: a switch records those that came with it
		// as written before it
		if replans {
			if sw, step, ok := t.takeToPlan(); ok {
				if final, why, stop = t.switchFromWait(sw, step, "waiting-human"); stop {
					return nil, true
				}
			}
		}
		var shown TaskSignal
		var replay []TaskSignal
		for workflow.GetSignalChannel(t.ctx, SignalCaseFaced).ReceiveAsync(&shown) {
			if t.takeFaced(shown, question) {
				replay, t.kept = append(replay, t.kept...), nil
			}
			shown = TaskSignal{}
		}
		var done TaskSignal
		for workflow.GetSignalChannel(t.ctx, SignalCaseResolved).ReceiveAsync(&done) {
			switch c := done.Case; {
			case c != nil && t.caseID != 0 && c.CaseID == t.caseID:
				res := c.Res
				t.resolution, resolved = &res, true
				if note := strings.TrimSpace(res.Note); note != "" || c.Comment != 0 {
					t.insertAnswer(t.resolutionKey(c.CaseID, c.Comment), answerText(c.By, question, cmp.Or(note, res.Sentence()), done.At))
				}
				heard = append(heard, c.Comment)
			case t.caseQueue >= versionCaseQueue && c != nil && c.CaseID != 0:
				// a case the run does not wait on: another case's answer
				// waits for the wait that comes to it
				// (versionCaseQueue)
				if t.resolvedQueue == nil {
					t.resolvedQueue = map[int64]queuedCase{}
				}
				t.resolvedQueue[c.CaseID] = queuedCase{signal: *c, at: done.At}
			}
			done = TaskSignal{}
		}
		var take func(sig TaskSignal)
		take = func(sig TaskSignal) {
			if t.caseID != 0 && t.escalation == 0 && sig.Comment > t.newestAnchor() && t.caseHeld() {
				// the face's comment is unknown, and this reply is newer than
				// every question asked: it may answer the face, which the
				// store holds when its signal is late or lost
				switch face := t.caseState(); {
				case face.Comment != 0:
					if t.takeFaced(TaskSignal{Author: FactoryDecider, Comment: face.Comment, Case: &CaseSignal{CaseID: t.caseID, Rung: 3, By: FactoryDecider, Comment: face.Comment}}, question) {
						kept := t.kept
						t.kept = nil
						for _, e := range kept {
							take(e)
						}
					}
				case face.Ended:
					// no face comes: the replies kept for one go now
					t.fileKept()
				default:
					t.kept = append(t.kept, sig)
					return
				}
			}
			if t.besideLadder >= versionBesideLadder && t.fileAnswer(sig, t.escalation) {
				return
			}
			if !t.counted(SignalHumanComment, sig) {
				return
			}
			if slices.Contains(t.answerKeys, answerKey{escalation: t.escalation, comment: sig.Comment}) {
				return // the webhook's copy of a resolution's comment
			}
			if t.resolution == nil {
				t.resolution = &Resolution{Action: ActContinue, Note: sig.Body}
			}
			heard = append(heard, sig.Comment)
			if t.v7 < versionAnswers {
				who := "human " + sig.Author
				if sig.Author == ChatUser {
					who = answerer(sig.Author)
				}
				answer = append(answer, who+" answered the escalation: "+sig.Body)
				return
			}
			if t.besideLadder >= versionBesideLadder {
				t.insertAnswer(answerKey{escalation: t.escalation, comment: sig.Comment}, answerText(sig.Author, question, sig.Body, sig.At))
				return
			}
			t.answers = slices.Insert(t.answers, at, answerText(sig.Author, question, sig.Body, sig.At))
			if t.answerBurst >= versionAnswerBurst {
				at++
			}
		}
		for _, sig := range replay {
			take(sig)
		}
		var sig TaskSignal
		for workflow.GetSignalChannel(t.ctx, SignalHumanComment).ReceiveAsync(&sig) {
			take(sig)
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
		if stop {
			t.leaveWait("the run ended: " + why)
			return final, why, nil
		}
		if resolved {
			return "", "", answer
		}
		if len(heard) > 0 && t.answerBurst >= versionAnswerBurst && t.awaitBurst(wake, pass, heard) {
			return final, why, nil
		}
		if len(heard) > 0 || len(answer) > 0 || end != nil && end.over() {
			switch {
			case len(heard) > 0:
			case len(answer) == 0:
				t.endCase("the wait ended on its own: " + end.name())
			default:
				t.leaveWait("reviewer " + t.changesBy + " requested changes")
			}
			return "", "", answer
		}
	}
}

// resolutionKey is the answer key of case caseID's resolution, carried by
// comment: one that carries none names its case (versionCaseHeld), so the
// answer of a later case with none, as rung 1's, is not taken for a copy.
func (t *taskRun) resolutionKey(caseID, comment int64) answerKey {
	k := answerKey{escalation: t.escalation, comment: comment}
	if comment == 0 && t.caseHeld() {
		k.caseID = caseID
	}
	return k
}

// caseHeld reports whether the run is at versionCaseHeld.
func (t *taskRun) caseHeld() bool {
	return workflow.GetVersion(t.ctx, "case-held", workflow.DefaultVersion, versionCaseHeld) >= versionCaseHeld
}

// caseState reads the face of the case the run waits on from the store
// (versionCaseHeld); none when the read fails.
func (t *taskRun) caseState() CaseState {
	var face CaseState
	if err := workflow.ExecuteActivity(t.short, ActivityCaseState, t.caseID).Get(t.ctx, &face); err != nil {
		workflow.GetLogger(t.ctx).Warn("read the case's face", "task", t.taskID, "case", t.caseID, "error", err)
	}
	return face
}

// fileKept files each reply kept for a face that did not come under the
// question it answers, or records it as no answer.
func (t *taskRun) fileKept() {
	kept := t.kept
	t.kept = nil
	for _, sig := range kept {
		if !(t.besideLadder >= versionBesideLadder && t.fileAnswer(sig, t.escalation)) {
			t.counted(SignalHumanComment, sig)
		}
	}
}

// leaveWait leaves the wait on a case awaitAnswer is in: the replies kept
// for a face are filed, and for why, what moved the run on with no answer
// of the case, the case ends (versionCaseHeld). "" ends no case.
func (t *taskRun) leaveWait(why string) {
	if !t.inCaseWait {
		return
	}
	t.inCaseWait = false
	t.fileKept()
	if why != "" && t.caseID != 0 && t.caseHeld() {
		t.endCase(why)
	}
}

// endCase withdraws the case the run waits on for reason (versionCases).
func (t *taskRun) endCase(reason string) {
	if err := workflow.ExecuteActivity(t.short, ActivityEndCase, t.caseID, reason).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("end the case", "task", t.taskID, "case", t.caseID, "error", err)
	}
}

// capUntil is when cap refusal c resets on its own, at the turn of its
// period: the UTC day's at the day cap, the UTC month's at the month's and
// the project's; zero at those in a run from before versionCapWait, whose
// wait only a human ends.
func (t *taskRun) capUntil(c CapRefusal) time.Time {
	if c.Decision != policy.RefusedDayCap && t.capWaits < versionCapWait {
		return time.Time{}
	}
	return capTurn(c.Decision, c.At)
}

// capReason is the card's escalation of cap refusal c: a wait that ends
// on its own says when (capWaitReason), one only a human ends asks as a
// run from before versionCapWait asks (capRefused).
func (t *taskRun) capReason(c CapRefusal) string {
	until := t.capUntil(c)
	if until.IsZero() {
		return capRefused(c.Refusal)
	}
	return capWaitReason(c.Decision, capGateway(c.Decision), c.Reason, until)
}

// capAsk is the question about cap refusal c of a session on model and
// the wait that ends on its own (nil: only a human ends it). "" asks
// nothing: the cap's period turned, or, at versionCapWait, the check
// before the ask (CapResume with RefusedAt), which drops the policy loads
// signalled before it (drainLoads), finds the cap lifted by a version the
// refusal came before; a check that fails asks. That check also says when
// the version in force binds at the gateway, which brings the wait's
// first check soon (settles).
func (t *taskRun) capAsk(c CapRefusal, model string) (reason string, w *capWait) {
	until := t.capUntil(c)
	if until.IsZero() {
		return t.capReason(c), nil
	}
	if !workflow.Now(t.ctx).Before(until) {
		return "", nil
	}
	w = &capWait{until: until, model: model, decision: c.Decision}
	if t.capWaits >= versionCapWait {
		t.drainLoads()
		var res CapResumed
		in := CapCheck{TaskID: t.taskID, Model: model, Decision: c.Decision, RefusedAt: c.At}
		if err := workflow.ExecuteActivity(t.short, ActivityCapResume, in).Get(t.ctx, &res); err != nil {
			workflow.GetLogger(t.ctx).Warn("cap check before the ask", "task", t.taskID, "cap", c.Decision, "error", err)
		} else if res.Resume {
			return "", nil
		}
		w.settles = res.Settles
	}
	return t.capReason(c), w
}

// awaitCap escalates a session the gateway refused at cap c, unless
// capAsk asks nothing, and waits for a human or the wait's own end.
func (t *taskRun) awaitCap(c CapRefusal, model string) (final, why string, answer []string) {
	reason, w := t.capAsk(c, model)
	switch {
	case reason == "":
		return "", "", nil
	case w == nil:
		return t.awaitHuman(reason, nil)
	}
	return t.awaitCapWait(reason, w)
}

// awaitCapWait escalates a card a cap stopped with reason and waits for
// an answer or w's own end; a period that turned at w.until already waits
// for nothing.
func (t *taskRun) awaitCapWait(reason string, w *capWait) (final, why string, answer []string) {
	if !workflow.Now(t.ctx).Before(w.until) {
		return "", "", nil
	}
	return t.awaitHumanUntil(reason, nil, w)
}

// drainLoads drops the policy loads signalled so far, right before a read
// of the cap in force (versionCapWait): the wait that follows the read
// wakes on later ones alone.
func (t *taskRun) drainLoads() {
	if t.capWaits < versionCapWait {
		return
	}
	for workflow.GetSignalChannel(t.ctx, SignalPolicyLoaded).ReceiveAsync(nil) {
	}
}

// dayCheck is how often a wait on the day cap reads the day's free
// headroom: before the UTC day turns it frees only as outstanding holds
// settle or a human raises budget.day_eur, both seen within it, and a
// day's wait takes at most 288 checks into the run's history.
const dayCheck = 5 * time.Minute

// monthCheck is how often a wait on the month's or the project's cap
// checks (versionCapWait): a 31-day wait at dayCheck would write about
// 8,900 checks into one run's history against Temporal's limit of 51,200
// events; hourly writes at most 744.
const monthCheck = time.Hour

// settleRetry is the least time before the next check after one that
// found the version in force not yet binding: the runner's clock may lag
// the workflow's.
const settleRetry = 10 * time.Second

// capWait is a wait on a cap (versionDayCap): it ends on its own at until,
// the turn of the cap's period, or at a check, every dayCheck at the day
// cap and every monthCheck at the month's and the project's, that finds
// the cap no longer binds a session's start on model (CapResume). A
// policy load wakes it (SignalPolicyLoaded): its next check comes
// gateway.PolicyTTL later, once the gateway enforces the loaded version.
// A run from before versionCapWait waits on the day cap alone, checks
// through DayResume and takes no wake. A nil wait never ends on its own.
type capWait struct {
	until    time.Time
	model    string
	decision string    // the cap that refused (versionCapWait)
	settles  time.Time // when the version in force binds at the gateway, by the last check: the next check comes soon (versionCapWait)
	restored bool      // a healed run's wait, whose first check runs at once (versionCapWait)
	t        *taskRun
	ctx      workflow.Context
	cancel   workflow.CancelFunc
	// next is the next check's timer, which disarm cancels
	// (versionCapWait), or the check that runs (checking); turned: that
	// check is the period's turn's; woken: a policy load came while it ran
	next                    workflow.Future
	disarm                  workflow.CancelFunc
	checking, turned, woken bool
	ended                   bool
}

// start arms the wait's first check; a restored wait (versionCapWait)
// checks at once, which reads the cap in force, so the policy loads
// signalled before it are dropped.
func (d *capWait) start(t *taskRun) {
	if d == nil {
		return
	}
	d.t = t
	d.ctx, d.cancel = workflow.WithCancel(t.ctx)
	if t.capWaits >= versionCapWait && d.restored {
		t.drainLoads()
		d.check()
		return
	}
	d.arm(d.every())
}

// every is the time between the wait's checks.
func (d *capWait) every() time.Duration {
	if d.t.capWaits < versionCapWait || d.decision == policy.RefusedDayCap {
		return dayCheck
	}
	return monthCheck
}

// arm sets the timer of the next check, after, at the period's turn at
// the latest. At versionCapWait a settles bounds it too, settleRetry at
// the least, and the timer replaces the last under its own context, which
// a wake cancels (cancelling a timer that fired issues no command).
func (d *capWait) arm(after time.Duration) {
	d.checking = false
	now := workflow.Now(d.ctx)
	if d.t.capWaits < versionCapWait {
		d.next = workflow.NewTimer(d.ctx, min(after, d.until.Sub(now)))
		return
	}
	if !d.settles.IsZero() {
		after = min(after, max(d.settles.Sub(now), settleRetry))
	}
	after = min(after, d.until.Sub(now))
	if d.disarm != nil {
		d.disarm()
	}
	var timer workflow.Context
	timer, d.disarm = workflow.WithCancel(d.ctx)
	d.next = workflow.NewTimer(timer, after)
}

// check runs the wait's check, the turn's once the period turned.
func (d *capWait) check() {
	d.checking, d.turned = true, !workflow.Now(d.ctx).Before(d.until)
	opts := workflow.WithActivityOptions(d.ctx, finishOptions())
	if d.t.capWaits < versionCapWait {
		d.next = workflow.ExecuteActivity(opts, ActivityDayResume, d.t.taskID, d.model, d.turned)
		return
	}
	d.next = workflow.ExecuteActivity(opts, ActivityCapResume, CapCheck{TaskID: d.t.taskID, Model: d.model, Decision: d.decision, Turned: d.turned})
}

// resumed reads check f: whether it ends the wait. One that failed waits
// on; settles becomes the check's, set when it found the version in force
// not yet binding at the gateway.
func (d *capWait) resumed(f workflow.Future) bool {
	if d.t.capWaits < versionCapWait {
		var funded bool
		if err := f.Get(d.ctx, &funded); err != nil {
			workflow.GetLogger(d.ctx).Warn("day cap check", "task", d.t.taskID, "error", err)
		}
		return funded
	}
	var res CapResumed
	if err := f.Get(d.ctx, &res); err != nil {
		workflow.GetLogger(d.ctx).Warn("cap check", "task", d.t.taskID, "cap", d.decision, "error", err)
	}
	d.settles = res.Settles
	return res.Resume
}

// add makes sel wake on the wait's next step: a timer starts a check; a
// check that finds the cap no longer binding, or the turn's, ends the
// wait, and any other arms the next. At versionCapWait a policy load wakes
// it too: the next check comes gateway.PolicyTTL after the wake, or after
// the check that runs; a burst of loads is one wake.
func (d *capWait) add(sel workflow.Selector) {
	if d == nil || d.ended {
		return
	}
	sel.AddFuture(d.next, func(f workflow.Future) {
		if !d.checking {
			d.check()
			return
		}
		if d.ended = d.resumed(f) || d.turned; d.ended {
			return
		}
		after := d.every()
		if d.woken {
			d.woken, after = false, gateway.PolicyTTL
		}
		d.arm(after)
	})
	if d.t.capWaits < versionCapWait {
		return
	}
	sel.AddReceive(workflow.GetSignalChannel(d.ctx, SignalPolicyLoaded), func(c workflow.ReceiveChannel, _ bool) {
		for c.ReceiveAsync(nil) {
		}
		if d.checking {
			d.woken = true
			return
		}
		d.arm(gateway.PolicyTTL)
	})
}

// over says the wait ended on its own.
func (d *capWait) over() bool { return d != nil && d.ended }

func (d *capWait) name() string { return "the cap no longer binds a session" }

// stop cancels the wait's timer and its running check.
func (d *capWait) stop() {
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
// decides the task, a person's replan sends a direct card to the planner
// (versionReplan), or infoWait after since passes: then the issue closes
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
	replans := t.replans()
	if replans {
		wake = append(wake, SignalToPlan)
	}
	pass := func() (heard []int64, stop bool) {
		if final, why, stop = t.decided(); stop {
			return nil, true
		}
		if replans {
			if sw, step, ok := t.takeToPlan(); ok {
				if final, why, stop = t.switchFromWait(sw, step, "waiting-poster"); stop {
					return nil, true
				}
			}
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
// pushOverBoundMark is the worker's named refusal in a session's summary
// (errPushOverBound, cmd/agent-worker): the branch's diff against its
// merge base does not fit the card's line bound even after the merge of
// main.
const pushOverBoundMark = "push refused: over the card's line bound"

// successorChosen reports whether the answer the run woke on chose the
// successor exit: the newest answer's body carries its label.
func successorChosen(answers []string) bool {
	return len(answers) > 0 && strings.Contains(answers[0], ": "+SuccessorChoice)
}

// closeIntoSuccessor files the card's successor with the findings so far
// (a packed block of lines) and is the card's end, failed, the successor
// named in its reason, which its callers finish.
func (t *taskRun) closeIntoSuccessor(findings string) (final, reason string) {
	var succ string
	in := SuccessorInput{TaskID: t.taskID, Findings: findings, PR: t.pr.Number}
	if err := workflow.ExecuteActivity(t.short, ActivitySuccessor, in).Get(t.ctx, &succ); err != nil {
		return "failed", "close into the successor: " + errMessage(err)
	}
	return "failed", "closed into successor " + succ
}

func (t *taskRun) finish(final, reason string) (string, error) {
	if t.switched != nil {
		// the plan program finished the card; a wait it ran in returns here
		return t.switched.final, t.switched.err
	}
	if final == "done" && t.rolling && t.rollout >= versionRollout {
		final, reason = t.awaitRollout(reason)
	}
	ctx := workflow.WithActivityOptions(t.ctx, finishOptions())
	if err := workflow.ExecuteActivity(ctx, ActivityFinish, t.taskID, final, reason).Get(t.ctx, nil); err != nil {
		return "", err
	}
	return final, nil
}

// awaitRollout is a roll's last wait: after its merge the
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

func isWipHead(err error) bool {
	var app *temporal.ApplicationError
	return errors.As(err, &app) && app.Type() == errWipHead
}

func isHeadMoved(err error) bool {
	var app *temporal.ApplicationError
	return errors.As(err, &app) && app.Type() == errHeadMoved
}

// errDrawRefused types a draw's refusal past a bound (an empty pool): the
// grant it was to record buys nothing and the card asks a human.
const errDrawRefused = "draw-refused"

func drawRefused(err error) bool {
	var app *temporal.ApplicationError
	return errors.As(err, &app) && app.Type() == errDrawRefused
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
