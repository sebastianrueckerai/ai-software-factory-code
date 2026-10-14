package factoryd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema"
)

// Task is a factory task row. factoryd owns these rows; the gateway reads
// budget/spend and refuses model spend unless status is running.
type Task struct {
	ID                   string
	Priority             int
	RequiredCapabilities []string
	RiskClass            string
	BudgetEUR            float64
	SpentEUR             float64
	Status               string
	Objective            string
	Repository           string
	SHA                  string
	// Kind is "agent" (an agent run, the default) or "human" (Stage 7's
	// console lane: no agent run).
	Kind string
	// IssueNumber is the Forgejo issue mirroring this task's state; 0 means
	// none is linked yet.
	IssueNumber int64
	// Source is how the task arrived: "api" (POST /tasks) or "webhook" (an
	// issue labelled in Forgejo).
	Source string
	// PRNumber is the pull request the task's workflow opened; 0 = none yet.
	PRNumber int64
	Usage    TaskUsage
	// ReproCommand is the task's reproduction (the issue's fenced block
	// labelled repro, or the API's repro_command): GateReproduction runs it
	// against the pull request head and it must pass. Empty = none.
	ReproCommand string
	// ReproBase is a signal card's pinned reproduction base (roadmap 194):
	// the commit that was live when its alert fired, recorded at filing
	// from the deployed build's roll. GateReproduction runs the base
	// against that tree even though main has moved on; "" for every other
	// card, whose base is SHA.
	ReproBase string
	// CreatedAt is when the work was asked for: the issue's created_at, or
	// the API call (zero at CreateTask: now).
	CreatedAt  time.Time
	FinishedAt *time.Time
	// AdmittedAt is when the card took one of its project's
	// work_in_progress slots; nil while it has not started. A queued card
	// with it set holds its slot while its workflow starts, or while a
	// run that died before running restarts.
	AdmittedAt *time.Time
	// Gate is the gate the task is in (Gate* constants) since GateSince;
	// none once it finished, and never for the human lane.
	Gate      string
	GateSince *time.Time
	// Author is who filed the task's issue and AuthorWriter whether they
	// could write the repository then (ruling 152): the brief and the
	// model review weigh an outside account's report as untrusted input.
	Author       string
	AuthorWriter bool
	// AgentSummary is the agent's last message of its latest pushed
	// session: its recommendation, and a not_reproduced line when it
	// fixed a fault it did not reproduce exactly (ruling 149); a report
	// task's report.
	AgentSummary string
	// Parent is the task whose issue a report task reads and comments
	// on (ruling 185); "" for every other task.
	Parent string
	// ModelAlias is the alias of the task's last session's model, ""
	// before its first.
	ModelAlias string
	// RevertedAt is when the daily revert pass found the task's merged
	// work undone (RevertPass); nil while it holds.
	RevertedAt *time.Time
	// Lane is the card's lane (backlog, plan, ready); "" for the human
	// lane, rolls and report tasks. Process is the process it follows,
	// "" where it has no lane or the loader has not filled it yet.
	Lane, Process string
	// PlanKey is a plan's card's key in the plan (source plan).
	PlanKey string
	// Rung is the model rung the card starts on (RungCheap, RungFrontier,
	// "" for the ladder's start) and Size its card size, both from its
	// filing (roadmap 222, item 5).
	Rung, Size string
	// HumanSummary is what a writer of the task's project says the card
	// is about, shown on its face instead of the generated title; "" when
	// nothing is set (the column is NULL).
	HumanSummary string
	// Accent is the card's colour label (one of Accents), "" when none.
	// It never changes a state rendering and never enters a brief.
	Accent string
	// GitHubPR is the card's pull request on GitHub, from an export of
	// mode pull_requests, 0 for none; GitHubPRState is open, merged,
	// closed or "failed: <why>", "" before the export took the card.
	GitHubPR                   int64
	GitHubPRURL, GitHubPRState string
	// AlertFingerprint is the SigNoz alert that filed an alert card's
	// task (alert-precheck, roadmap 181 item 3.12); "" for every other
	// task. The pre-check reads its firing state by it.
	AlertFingerprint string
}

// Accents are the accent colours a human may put on a card, in the order
// the console offers them. Their hues are fixed and never a status's hue
// at that status's chroma (see console.css); an accent outside this list
// is refused.
var Accents = []string{"red", "orange", "amber", "green", "teal", "blue", "violet", "pink"}

// IsAccent reports whether name is one of Accents.
func IsAccent(name string) bool { return slices.Contains(Accents, name) }

// Report tells a report task (ruling 185) by its source: a plan's cards
// have a parent too and are no report.
func (t Task) Report() bool { return t.Source == SourceScreening || t.Source == SourcePresort }

// Standing is the issue author's standing (ruling 152); "" for a task
// from before authors were kept.
func (t Task) Standing() string {
	switch {
	case t.Author == "":
		return ""
	case t.Source == SourceAlert:
		return harness.StandingTelemetry
	case t.AuthorWriter:
		return harness.StandingWriter
	}
	return harness.StandingOutside
}

// maxAgentSummary bounds the agent's last word kept on the task row.
const maxAgentSummary = 2000

// Final reports whether status ends a task.
func Final(status string) bool { return slices.Contains(FinalStatuses, status) }

// TaskUsage is what a task has used of its caps and what a human granted
// on top; the workflow keeps it on the row so a new run continues the
// count (ruling 93: the caps hold across attempts, so across runs).
type TaskUsage struct {
	AgentTime     time.Duration // the agent's working time, summed over the sessions
	Attempts      int
	ExtraTime     time.Duration // granted by human answers near the cap
	ExtraAttempts int           // one per human answer
	// PosterAttempts are the ExtraAttempts the issue author's answers bought
	PosterAttempts int
}

// IntakeEvent is a Forgejo comment, review or decision (issue closed, pull
// request merged or closed) recorded against a task's issue or pull
// request; the ones that steer or end the task are also signalled to the
// task's workflow.
type IntakeEvent struct {
	ID        int64
	TaskID    string
	Kind      string // issue_comment, pull_request_comment, issue_closed, pull_request_merged, pull_request_closed, or the review's X-Forgejo-Event-Type
	Author    string
	Body      string
	CreatedAt time.Time
	// DeliveryID is the webhook delivery that produced it: a redelivery
	// inserts no second row.
	DeliveryID string
	// CommentID is Forgejo's id of the comment (0 for a review; for a
	// decision, the number of the issue or pull request it ended): the
	// heal pass re-reads comments and states, and each is recorded once
	// whether the webhook or the re-read saw it first.
	CommentID int64
	// Trusted: the author could steer the task when the event was recorded.
	Trusted bool
	// Signalled: the task's workflow received it; a trusted event not yet
	// signalled (factoryd was down or erroring at delivery) is signalled by
	// the heal pass.
	Signalled bool
	// Choices are the answers an agent offered with its question, on its
	// ask and on the escalation that put the question to a human.
	Choices []string
}

// changeRequestKinds are the intake event kinds that ask for changes on
// the task's pull request.
var changeRequestKinds = []string{"pull_request_comment", "pull_request_review_rejected"}

// Session is one agent worker run of a task. ParentSessionID chains a
// supervision-triggered resume to the session it continues; Findings is the
// compact snapshot carried into the next attempt; RestartState is the small
// JSON blob the harness resumes from. The full history lives in S3.
type Session struct {
	ID              string
	TaskID          string
	ParentSessionID string
	Findings        []string
	RestartState    []byte
	CreatedAt       time.Time
	// Model is the gateway alias the session runs on; inserting the
	// session makes it the task's ModelAlias.
	Model string
}

// Store is the durable task and session state. The production implementation
// is Postgres; tests use an in-memory fake.
type Store interface {
	CreateTask(ctx context.Context, t Task) error
	Task(ctx context.Context, id string) (Task, bool, error)
	// TransitionTask moves a task to status `to` only from one of the
	// statuses in from; any other current status is an error. Entering done
	// or failed stamps the finish time and leaves the gate.
	TransitionTask(ctx context.Context, id string, from []string, to string) error
	// MarkStarted moves a queued, waiting-deps, running, in-review,
	// waiting-human or waiting-poster task to running.
	MarkStarted(ctx context.Context, id string) error
	// SetAdmitted stamps task id's admission (admitted_at, once): the
	// moment it took one of its project's work_in_progress slots.
	SetAdmitted(ctx context.Context, id string) error
	// ListTasksByStatus lists tasks in status, highest priority first.
	ListTasksByStatus(ctx context.Context, status string) ([]Task, error)
	// CountAdmitted counts repository's cards holding a work slot: those
	// in a status a started card holds until it settles, plus queued
	// cards already admitted. A lane-less card (a roll, a factory pull
	// request) and human and chat cards hold none.
	CountAdmitted(ctx context.Context, repository string) (int, error)
	// ListQueuedByRepository lists repository's queued cards in filing
	// order, the oldest first: the order a freed slot admits them in.
	// created_at ties (Forgejo stamps whole seconds) break by issue
	// number, the filing sequence, so a same-second burst keeps its order.
	ListQueuedByRepository(ctx context.Context, repository string) ([]Task, error)
	// RecentlyFinishedTasks lists tasks that finished (done or failed)
	// within the last `within`, on the store's clock, and have no open model
	// hold: a call still streaming books its own cost, and settling it too
	// would book it twice. Tool and resource holds never touch LiteLLM key
	// spend, so they never hold up settlement.
	RecentlyFinishedTasks(ctx context.Context, within time.Duration) ([]string, error)
	// SettleSpend raises the task's booked model spend (its model and
	// settlement rows) to at least reported, LiteLLM's model spend: it books
	// the raise as a settlement row, sets spent_eur to the task's ledger sum
	// and reports whether it booked one.
	SettleSpend(ctx context.Context, id string, reported float64) (bool, error)
	// UnsettledTasks lists tasks that finished at least olderThan ago, on
	// the store's clock, with no open model hold and no final settlement
	// yet.
	UnsettledTasks(ctx context.Context, olderThan time.Duration) ([]string, error)
	// SettleSpendFinal sets the task's booked model spend to reported,
	// LiteLLM's final figure, down as well as up (ruling 196): it books the
	// difference as a settlement row, sets spent_eur to the task's ledger
	// sum, marks the task settled and reports whether it booked a row; a
	// settled task is never listed again.
	SettleSpendFinal(ctx context.Context, id string, reported float64) (bool, error)
	// TaskByIssue looks up a task by the Forgejo issue mirroring it, for
	// webhook redelivery idempotency; false means no task is linked to that
	// issue yet.
	TaskByIssue(ctx context.Context, repository string, issueNumber int64) (Task, bool, error)
	// TaskByPR looks up a task by the pull request factoryd opened outside
	// any card's workflow (roadmap 187); false means no task is linked to
	// that pull request yet.
	TaskByPR(ctx context.Context, repository string, pr int64) (Task, bool, error)
	// InsertIntakeEvent records e and returns its id; 0 when the same
	// delivery or comment was recorded before.
	InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error)
	// IntakeEvents lists a task's events, oldest first.
	IntakeEvents(ctx context.Context, taskID string) ([]IntakeEvent, error)
	// SetSignalled sets whether event id reached the task's workflow and
	// reports whether that changed it: setting true is the claim to signal
	// it, exclusive between the webhook path and the heal pass; setting
	// false releases a claim whose signal failed.
	SetSignalled(ctx context.Context, id int64, signalled bool) (bool, error)
	// UnaddressedChangeRequests lists the signalled change requests no
	// session has started on yet: newer than the task's latest session.
	UnaddressedChangeRequests(ctx context.Context, taskID string) ([]IntakeEvent, error)
	// SetTaskPR records the pull request a task's workflow opened.
	SetTaskPR(ctx context.Context, id string, number int64) error
	// SetAgentSummary records the agent's last message of its latest
	// pushed session.
	SetAgentSummary(ctx context.Context, id, summary string) error
	// MonthCapRefusal is the task's newest refused_month_cap or
	// refused_project_cap audit row since since, nil for none.
	MonthCapRefusal(ctx context.Context, taskID string, since time.Time) (*policy.Refusal, error)
	// ProviderRefusal is the reason of the task's agent's first model call
	// since since when the gateway audited it error_upstream and no model
	// call of the agent since was allowed; ok false otherwise.
	ProviderRefusal(ctx context.Context, taskID string, since time.Time) (reason string, ok bool, err error)
	// SetTaskModelAlias moves a running session to alias: the gateway
	// admits the session's calls on it from the next one (the in-session
	// climb, roadmap 222, item 5).
	SetTaskModelAlias(ctx context.Context, id, alias string) error
	// PauseQueue pauses a project's merge queue while main is red
	// (mainfix.go); QueuePause reads it, SetQueuePauseRevert names the
	// revert card, ClearQueuePause ends it.
	PauseQueue(ctx context.Context, p QueuePause) error
	QueuePause(ctx context.Context, project string) (QueuePause, bool, error)
	SetQueuePauseRevert(ctx context.Context, project, taskID string) error
	ClearQueuePause(ctx context.Context, project string) error
	// PutLiveSession records a running session's sandbox; a retried
	// RunSession reattaches to it (LiveSession), SetLiveSessionLines
	// keeps how much of the pod's log the loop consumed, and the session's
	// end deletes it.
	PutLiveSession(ctx context.Context, l LiveSession) error
	SetLiveSessionLines(ctx context.Context, taskID string, lines int) error
	LiveSession(ctx context.Context, taskID string) (LiveSession, bool, error)
	DeleteLiveSession(ctx context.Context, taskID string) error
	// SetRollHold holds new sessions for roll task taskID from since to
	// until; ClearRollHold ends its hold; RollHold is the newest hold in
	// force at now, if any.
	SetRollHold(ctx context.Context, taskID string, since, until time.Time) error
	ClearRollHold(ctx context.Context, taskID string) error
	RollHold(ctx context.Context, now time.Time) (RollHold, bool, error)
	// EnterLane moves backlog card id into starting lane lane: queued at
	// base sha with the lane's class as its capabilities, in the intake
	// gate from now; false when the card was not in the backlog.
	EnterLane(ctx context.Context, id, lane, class, sha string) (bool, error)
	// ToHumanLane makes queued agent task id a human-lane task waiting on
	// a human; false when it was not a queued agent task.
	ToHumanLane(ctx context.Context, id string) (bool, error)
	// ProjectByRepository is the project row of repository (owner/name).
	ProjectByRepository(ctx context.Context, repository string) (Project, bool, error)
	// SetTaskUsage records what the task has used of its caps.
	SetTaskUsage(ctx context.Context, id string, u TaskUsage) error
	// DeliverySeen reports whether a webhook delivery was processed.
	DeliverySeen(ctx context.Context, id string) (bool, error)
	// RecordDelivery marks a webhook delivery processed.
	RecordDelivery(ctx context.Context, id string) error
	// PruneDeliveries forgets processed deliveries older than age.
	PruneDeliveries(ctx context.Context, age time.Duration) error
	InsertSession(ctx context.Context, s Session) error
	Session(ctx context.Context, id string) (Session, bool, error)
	UpdateSession(ctx context.Context, id string, findings []string, restartState []byte) error
	// SetTaskRisk records the risk class GatePolicy gave the task's pull
	// request.
	SetTaskRisk(ctx context.Context, id, class string) error
	// SetTaskAlertFingerprint records the SigNoz alert fingerprint that
	// filed an alert card's task (alert-precheck).
	SetTaskAlertFingerprint(ctx context.Context, id, fingerprint string) error
	// OpenApproval records a pending approval request and returns its id;
	// a merge or plan request denies the task's earlier pending one of its
	// kind (a retry, a new head, a new plan revision) as superseded.
	OpenApproval(ctx context.Context, a ApprovalRequest) (int64, error)
	// ResolveApprovals decides the task's pending approval requests of
	// kind ("" = every kind): status approved or denied, by whom
	// ("factoryd" for the factory's own decisions) and why.
	ResolveApprovals(ctx context.Context, taskID, kind, status, by, note string) error
	// DecideApproval decides one pending request of kind; ErrNotFound when
	// it is not pending.
	DecideApproval(ctx context.Context, id int64, kind, status, by, note string) error
	// UndecideApproval puts request id back to pending, undecided.
	UndecideApproval(ctx context.Context, id int64) error
	// ReopenApproval puts the task's newest request of kind, pending or
	// approved, back to pending with note: why the approval has not taken
	// effect (a refused or failed merge); a denied one stays denied.
	ReopenApproval(ctx context.Context, taskID, kind, note string) error
	// ApprovalRequests lists the requests in status, oldest first: the
	// console's approval cards.
	ApprovalRequests(ctx context.Context, status string) ([]ApprovalRequest, error)
	// SetGate records that the task entered gate at since; a finished
	// task enters none.
	SetGate(ctx context.Context, id, gate string, since time.Time) error
	// InsertAlert records an alert and returns its id; 0 when the task
	// has its screening alert already (one per task).
	InsertAlert(ctx context.Context, a Alert) (int64, error)
	// ResolveAlert records the gate supervisor's verdict on an open
	// gate_bound alert and merges detail into the alert's.
	ResolveAlert(ctx context.Context, id int64, kind, verdict, by string, detail map[string]any) error
	// JudgeTaskAlerts sets verdict on the task's alert rows of kind that
	// have none.
	JudgeTaskAlerts(ctx context.Context, taskID, kind, verdict, by string) error
	// OpenReportTask creates report task t unless its parent has one of
	// its source, and returns the parent's report task of that source.
	OpenReportTask(ctx context.Context, t Task) (Task, error)
	// ReportOf is parent's report task of source.
	ReportOf(ctx context.Context, parent, source string) (Task, bool, error)
	// PlanCards are the cards plan filed (source plan) that hold their
	// key: a retried card's replacement in its place.
	PlanCards(ctx context.Context, plan string) ([]Task, error)
	// AddTaskDeps records that task id waits on each of dependsOn; an
	// edge recorded before stays as it is.
	AddTaskDeps(ctx context.Context, id string, dependsOn []string) error
	// OpenDependencies are the cards task id waits on that are not done,
	// each with its status, by issue number.
	OpenDependencies(ctx context.Context, id string) ([]Dependency, error)
	// WaitingDependents are the tasks in waiting-deps that wait on id.
	WaitingDependents(ctx context.Context, id string) ([]string, error)
	// ReplaceFailedCard puts plan card t in failed card id's place in one
	// transaction: id lets go of its key, which t holds; t is inserted with
	// every dependency of id, and every task waiting on id waits on t.
	// False, and nothing changed, when id is no failed card holding the
	// key.
	ReplaceFailedCard(ctx context.Context, id string, t Task) (bool, error)
	// AddPlanRevision stores plan as task id's next revision and returns
	// its number; a plan equal to the newest (sha) is that revision.
	AddPlanRevision(ctx context.Context, id string, plan json.RawMessage, sha string) (int, error)
	// PlanRevision is revision n of task id's plan, the newest for 0.
	PlanRevision(ctx context.Context, id string, n int) (PlanRevision, bool, error)
	// PutPlanQuestions stores revision's questions of plan task id, numbered
	// from 1, unless it has them already, and reports whether it did.
	PutPlanQuestions(ctx context.Context, id string, revision int, qs []PlanQuestion) (bool, error)
	// PlanQuestions are revision's questions of plan task id in order,
	// each with its answer once given.
	PlanQuestions(ctx context.Context, id string, revision int) ([]PlanQuestion, error)
	// AnswerPlanQuestion records by's answer to question idx of revision:
	// ErrAnswered when it has one, ErrNotFound when there is no such
	// question.
	AnswerPlanQuestion(ctx context.Context, id string, revision, idx int, answer, by string) error
	// AnswerPlanQuestions records by's answers to plan task id's revision
	// in one statement: ErrAnswered when any question has an answer,
	// ErrNotFound when there is no such question; none is stored then.
	AnswerPlanQuestions(ctx context.Context, id string, revision int, answers []PlanAnswer, by string) error
	// OpenPlanQuestions is how many of revision's questions have no answer.
	OpenPlanQuestions(ctx context.Context, id string, revision int) (int, error)
	// CreatePlanCard inserts plan card t unless its plan has a card of its
	// key, and returns the plan's card of that key.
	CreatePlanCard(ctx context.Context, t Task) (Task, error)
	// SetTaskIssue links task id to its issue.
	SetTaskIssue(ctx context.Context, id string, issue int64) error
	// UncommittedRolls and MarkRollsCommitted are a roll task's requests
	// its branch does not carry yet (RollCommit).
	UncommittedRolls(ctx context.Context, taskID string) ([]RollRequest, error)
	MarkRollsCommitted(ctx context.Context, taskID string, shas []string, head string) error
	// RollHead is the branch commit RollCommit last wrote for a roll task.
	RollHead(ctx context.Context, taskID string) (string, error)
	// RebaseRolls frees task's roll markings made past the head its branch
	// sits at, and reports whether any request is marked at that head at
	// all (RebaseRolls).
	RebaseRolls(ctx context.Context, taskID, head string) (bool, error)
	// SetTaskSHA sets the commit a task's branch was cut from, its base.
	SetTaskSHA(ctx context.Context, id, sha string) error
	// The merge queue (queue.go).
	EnterQueue(ctx context.Context, project, taskID string) (QueueSpot, error)
	QueueHead(ctx context.Context, project string) (string, bool, error)
	LeaveQueue(ctx context.Context, taskID string) (project string, left bool, err error)
	WaitingQueueHeads(ctx context.Context) ([]string, error)
}

// Approval kinds: a task's pull request through its gates, a commissioning
// request (cloudres files these), a planner's plan.
const (
	ApprovalMerge    = "merge"
	ApprovalResource = "resource"
	// ApprovalPlan is a plan revision for approvers to accept (Spec a
	// PlanSpec).
	ApprovalPlan = "plan"
)

// ApprovalRequest is a decision the factory waits on a human for (ruling
// 116): kind merge is a task's pull request through its gates (Spec a
// MergeSpec), kind resource a commissioning request (Task 53). Status:
// pending, approved or denied.
type ApprovalRequest struct {
	ID           int64
	Kind         string
	TaskID       string
	Project      string // owner/name
	PRNumber     int64  // 0 (null) for a resource
	Spec         json.RawMessage
	Reason       string
	MonthlyEUR   *float64
	Status       string
	DecidedBy    string
	DecidedAt    *time.Time
	DecisionNote string
	CreatedAt    time.Time
}

// FactoryDecider is decided_by for the factory's own decisions.
const FactoryDecider = "factoryd"

// PGStore is the Postgres task store, pgx/v5 in stdlib mode.
type PGStore struct {
	db  *sql.DB
	bus *EventBus
}

// SetBus wires the event bus: Record publishes every row through it so the
// console's stream can fan out without polling the table.
func (s *PGStore) SetBus(b *EventBus) { s.bus = b }

// OpenStore opens the task store and applies pending migrations on boot.
// An empty databaseURL makes pgx assemble the connection from the PG* vars.
func OpenStore(ctx context.Context, databaseURL string) (*PGStore, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}
	s := &PGStore{db: db}
	if err := schema.Migrate(ctx, db); err != nil {
		db.Close() // #nosec G104 -- already returning the migrate error; the close error would only mask it
		return nil, err
	}
	return s, nil
}

func toJSON(v any) ([]byte, error) {
	if b, ok := v.([]byte); ok {
		return b, nil
	}
	return json.Marshal(v)
}

func fromJSON(b []byte) ([]string, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PGStore) CreateTask(ctx context.Context, t Task) error {
	return s.insertTask(ctx, t, "")
}

// insertTask inserts t through db, onConflict the insert's conflict
// clause, so a retry's transaction reuses it (Task 135).
func insertTask(ctx context.Context, db interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}, t Task, onConflict string) error {
	caps, err := toJSON(t.RequiredCapabilities)
	if err != nil {
		return err
	}
	var created *time.Time
	if !t.CreatedAt.IsZero() {
		created = &t.CreatedAt
	}
	// an agent task starts in intake, since the work was asked for
	q := `INSERT INTO tasks (id, priority, required_capabilities, risk_class, budget_eur, spent_eur, status,
		                    objective, repository, sha, repro_base, kind, issue_number, source, repro_command, created_at, gate, gate_since,
		                    author, author_writer, parent_task, lane, process, plan_key, alert_fingerprint, rung, size)
		 VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, ''), $12, $13, $14, $15, COALESCE($16, now()),
	         CASE WHEN $12 = 'agent' AND $7 <> 'backlog' THEN $17 ELSE '' END,
	         CASE WHEN $12 = 'agent' AND $7 <> 'backlog' THEN COALESCE($16, now()) END, $18, $19, NULLIF($20, ''),
	         NULLIF($21, ''), NULLIF($22, ''), NULLIF($23, ''), NULLIF($24, ''), $25, $26)
		 ` + onConflict // #nosec G202 -- onConflict is one of insertTask's constant clauses
	_, err = db.ExecContext(ctx, q,
		t.ID, t.Priority, caps, t.RiskClass, t.BudgetEUR, t.SpentEUR, t.Status,
		t.Objective, t.Repository, t.SHA, t.ReproBase, defaultKind(t.Kind), t.IssueNumber, t.Source, t.ReproCommand, created, GateIntake,
		t.Author, t.AuthorWriter, t.Parent, t.Lane, t.Process, t.PlanKey, t.AlertFingerprint, t.Rung, t.Size)
	return err
}

func (s *PGStore) insertTask(ctx context.Context, t Task, onConflict string) error {
	return insertTask(ctx, s.db, t, onConflict)
}

func (s *PGStore) OpenReportTask(ctx context.Context, t Task) (Task, error) {
	if err := s.insertTask(ctx, t, `ON CONFLICT (parent_task, source) WHERE parent_task IS NOT NULL AND source IN ('screening', 'presort') DO NOTHING`); err != nil {
		return Task{}, err
	}
	got, ok, err := s.ReportOf(ctx, t.Parent, t.Source)
	if err == nil && !ok {
		err = fmt.Errorf("report task of %s (%s) not found after its insert", t.Parent, t.Source)
	}
	return got, err
}

func (s *PGStore) ReportOf(ctx context.Context, parent, source string) (Task, bool, error) {
	return s.oneTask(ctx, `parent_task = $1 AND source = $2`, parent, source)
}

func (s *PGStore) PlanCards(ctx context.Context, plan string) ([]Task, error) {
	return s.tasks(ctx, `SELECT `+taskColumns+` FROM tasks WHERE parent_task = $1 AND source = $2 AND plan_key IS NOT NULL ORDER BY id`, plan, SourcePlan)
}

func defaultKind(kind string) string {
	if kind == "" {
		return "agent"
	}
	return kind
}

const taskColumns = `id, priority, required_capabilities, risk_class, budget_eur::float8, spent_eur::float8, status,
	objective, repository, sha, COALESCE(repro_base, ''), kind, issue_number, source, pr_number,
	agent_time_ns, attempts, extra_time_ns, extra_attempts, poster_attempts, repro_command, created_at, finished_at, gate, gate_since,
	author, author_writer, agent_summary, COALESCE(parent_task, ''), model_alias, reverted_at, COALESCE(lane, ''), COALESCE(process, ''), COALESCE(plan_key, ''),
	COALESCE(human_summary, ''), COALESCE(accent, ''), github_pr, github_pr_url, github_pr_state,
	COALESCE(alert_fingerprint, ''), admitted_at, rung, size`

func scanTask(scan func(dest ...any) error) (Task, error) {
	var t Task
	var caps []byte
	if err := scan(&t.ID, &t.Priority, &caps, &t.RiskClass, &t.BudgetEUR, &t.SpentEUR, &t.Status,
		&t.Objective, &t.Repository, &t.SHA, &t.ReproBase, &t.Kind, &t.IssueNumber, &t.Source, &t.PRNumber,
		&t.Usage.AgentTime, &t.Usage.Attempts, &t.Usage.ExtraTime, &t.Usage.ExtraAttempts, &t.Usage.PosterAttempts, &t.ReproCommand,
		&t.CreatedAt, &t.FinishedAt, &t.Gate, &t.GateSince, &t.Author, &t.AuthorWriter, &t.AgentSummary, &t.Parent,
		&t.ModelAlias, &t.RevertedAt, &t.Lane, &t.Process, &t.PlanKey, &t.HumanSummary, &t.Accent,
		&t.GitHubPR, &t.GitHubPRURL, &t.GitHubPRState, &t.AlertFingerprint, &t.AdmittedAt, &t.Rung, &t.Size); err != nil {
		return Task{}, err
	}
	var err error
	if t.RequiredCapabilities, err = fromJSON(caps); err != nil {
		return Task{}, fmt.Errorf("required_capabilities: %w", err)
	}
	return t, nil
}

func (s *PGStore) oneTask(ctx context.Context, where string, args ...any) (Task, bool, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE `+where, args...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, false, nil
	}
	if err != nil {
		return Task{}, false, err
	}
	return t, true, nil
}

func (s *PGStore) Task(ctx context.Context, id string) (Task, bool, error) {
	return s.oneTask(ctx, `id = $1`, id)
}

func (s *PGStore) TaskByIssue(ctx context.Context, repository string, issueNumber int64) (Task, bool, error) {
	return s.oneTask(ctx, `repository = $1 AND issue_number = $2`, repository, issueNumber)
}

// TaskByPR is the card whose row links pull request number on repository:
// the template pass reads it to tell whether a pull request already has
// its card (roadmap 187).
func (s *PGStore) TaskByPR(ctx context.Context, repository string, pr int64) (Task, bool, error) {
	return s.oneTask(ctx, `repository = $1 AND pr_number = $2`, repository, pr)
}

func (s *PGStore) InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error) {
	choices := any(nil)
	if len(e.Choices) > 0 {
		b, err := json.Marshal(e.Choices)
		if err != nil {
			return 0, err
		}
		choices = b
	}
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO intake_events (task_id, kind, author, body, delivery_id, comment_id, trusted, signalled, choices)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb) ON CONFLICT DO NOTHING RETURNING id`,
		e.TaskID, e.Kind, e.Author, e.Body, e.DeliveryID, e.CommentID, e.Trusted, e.Signalled, choices).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

func (s *PGStore) IntakeEvents(ctx context.Context, taskID string) ([]IntakeEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_id, kind, author, body, created_at, delivery_id, comment_id, trusted, signalled, choices
		 FROM intake_events WHERE task_id = $1 ORDER BY id`, taskID)
	return scanIntakeEvents(rows, err)
}

func (s *PGStore) UnaddressedChangeRequests(ctx context.Context, taskID string) ([]IntakeEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_id, kind, author, body, created_at, delivery_id, comment_id, trusted, signalled, choices
		 FROM intake_events WHERE task_id = $1 AND signalled AND kind = ANY($2)
		 AND created_at > COALESCE((SELECT max(created_at) FROM sessions WHERE task_id = $1), '-infinity')
		 ORDER BY id`, taskID, changeRequestKinds)
	return scanIntakeEvents(rows, err)
}

func scanIntakeEvents(rows *sql.Rows, err error) ([]IntakeEvent, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IntakeEvent
	for rows.Next() {
		var e IntakeEvent
		var choices []byte
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Kind, &e.Author, &e.Body, &e.CreatedAt, &e.DeliveryID, &e.CommentID, &e.Trusted, &e.Signalled, &choices); err != nil {
			return nil, err
		}
		if err := unmarshalChoices(choices, &e.Choices); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// unmarshalChoices reads a choices column into choices; NULL leaves none.
func unmarshalChoices(b []byte, choices *[]string) error {
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, choices)
}

func (s *PGStore) SetSignalled(ctx context.Context, id int64, signalled bool) (bool, error) {
	var changed bool
	err := s.db.QueryRowContext(ctx,
		`WITH before AS (SELECT signalled FROM intake_events WHERE id = $1 FOR UPDATE)
		 UPDATE intake_events e SET signalled = $2 FROM before WHERE e.id = $1 RETURNING before.signalled <> $2`,
		id, signalled).Scan(&changed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("intake event %d: no such row", id)
	}
	return changed, err
}

func (s *PGStore) SetTaskPR(ctx context.Context, id string, number int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET pr_number = $2 WHERE id = $1`, id, number)
	return oneRow(res, err, "task "+id)
}

func (s *PGStore) SetTaskUsage(ctx context.Context, id string, u TaskUsage) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET agent_time_ns = $2, attempts = $3, extra_time_ns = $4, extra_attempts = $5, poster_attempts = $6 WHERE id = $1`,
		id, int64(u.AgentTime), u.Attempts, int64(u.ExtraTime), u.ExtraAttempts, u.PosterAttempts)
	return oneRow(res, err, "task "+id)
}

func (s *PGStore) SetTaskSHA(ctx context.Context, id, sha string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET sha = $2 WHERE id = $1`, id, sha)
	return oneRow(res, err, "task "+id)
}

func (s *PGStore) MonthCapRefusal(ctx context.Context, taskID string, since time.Time) (*policy.Refusal, error) {
	var r policy.Refusal
	err := s.db.QueryRowContext(ctx,
		`SELECT decision, reason FROM audit WHERE task_id = $1 AND decision IN ($2, $3) AND ts >= $4 ORDER BY ts DESC, id DESC LIMIT 1`,
		taskID, policy.RefusedMonthCap, policy.RefusedProjectCap, since).Scan(&r.Decision, &r.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *PGStore) ProviderRefusal(ctx context.Context, taskID string, since time.Time) (string, bool, error) {
	var reason string
	err := s.db.QueryRowContext(ctx,
		`WITH calls AS (SELECT id, ts, decision, reason FROM audit WHERE task_id = $1 AND kind = $2 AND tool = '' AND ts >= $3)
		 SELECT reason FROM (SELECT decision, reason FROM calls ORDER BY ts, id LIMIT 1) first
		 WHERE decision = 'error_upstream' AND NOT EXISTS (SELECT 1 FROM calls WHERE decision = 'allowed')`,
		taskID, gateway.AuditKindAgent, since).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return reason, err == nil, err
}

// MonthSpend is the variable spend over the ledger in the UTC calendar
// month of at, standing lines left out, as the gateway's month headroom
// counts it, and the reason of the month's first refused_month_cap of a
// model call: only a model call finds the cap spent, a reservation or a
// tool call's worst case may be refused while euros remain.
func (s *PGStore) MonthSpend(ctx context.Context, at time.Time) (MonthSpend, error) {
	var m MonthSpend
	err := s.db.QueryRowContext(ctx,
		`SELECT to_char($2::timestamptz AT TIME ZONE 'UTC', 'YYYY-MM'),
		        `+policy.MonthSpentSQL("$2::timestamptz")+`::float8,
		        COALESCE((SELECT reason FROM audit WHERE decision = $1 AND tool = ''
		                    AND ts >= date_trunc('month', $2::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
		                    AND ts < (date_trunc('month', $2::timestamptz AT TIME ZONE 'UTC') + interval '1 month') AT TIME ZONE 'UTC'
		                  ORDER BY ts, id LIMIT 1), '')`,
		policy.RefusedMonthCap, at).Scan(&m.Month, &m.EUR, &m.Refusal)
	return m, err
}

func (s *PGStore) SetTaskModelAlias(ctx context.Context, id, alias string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET model_alias = $2 WHERE id = $1`, id, alias)
	return err
}

func (s *PGStore) SetAgentSummary(ctx context.Context, id, summary string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET agent_summary = $2 WHERE id = $1`, id, summary)
	return oneRow(res, err, "task "+id)
}

// SetHumanSummary sets task id's human summary; "" restores the generated
// title by storing NULL.
func (s *PGStore) SetHumanSummary(ctx context.Context, id, summary string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET human_summary = NULLIF($2, '') WHERE id = $1`, id, summary)
	return oneRow(res, err, "task "+id)
}

// SetObjective replaces task id's objective, which the next session's
// brief reads.
func (s *PGStore) SetObjective(ctx context.Context, id, objective string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET objective = $2 WHERE id = $1`, id, objective)
	return oneRow(res, err, "task "+id)
}

// RaiseTaskBudget raises task id's budget_eur by eur in one transaction
// (roadmap 210: the console's Fund action) and answers the budget before
// and after the raise, the one statement both doing the raise and reading
// what the audit row carries.
func (s *PGStore) RaiseTaskBudget(ctx context.Context, id string, eur float64) (from, to float64, err error) {
	err = s.db.QueryRowContext(ctx,
		`UPDATE tasks SET budget_eur = budget_eur + $2 WHERE id = $1
		 RETURNING (budget_eur - $2::numeric)::float8, budget_eur::float8`,
		id, eur).Scan(&from, &to)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrNotFound
	}
	return from, to, err
}

// SetTaskAccent sets task id's accent colour; "" removes it (NULL).
func (s *PGStore) SetTaskAccent(ctx context.Context, id, accent string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET accent = NULLIF($2, '') WHERE id = $1`, id, accent)
	return oneRow(res, err, "task "+id)
}

func (s *PGStore) ToHumanLane(ctx context.Context, id string) (bool, error) {
	return s.changeStatus(ctx, `kind = 'human', status = 'waiting-human', gate = '', gate_since = NULL, lane = NULL, process = NULL`,
		`id = $1 AND kind = 'agent' AND status = 'queued'`, id)
}

func (s *PGStore) BackfillProcess(ctx context.Context, repository, process string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET process = $2
		WHERE repository = $1 AND lane IS NOT NULL AND process IS NULL AND status NOT IN ('done', 'failed', 'closed')`, repository, process)
	return err
}

func (s *PGStore) EnterLane(ctx context.Context, id, lane, class, sha string) (bool, error) {
	return s.changeStatus(ctx, `status = 'queued', lane = $2, required_capabilities = jsonb_build_array($3::text),
		sha = $4, gate = $5, gate_since = now()`, `id = $1 AND status = 'backlog'`, id, lane, class, sha, GateIntake)
}

func (s *PGStore) DeliverySeen(ctx context.Context, id string) (bool, error) {
	var seen bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM webhook_deliveries WHERE id = $1)`, id).Scan(&seen)
	return seen, err
}

func (s *PGStore) RecordDelivery(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO webhook_deliveries (id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
	return err
}

func (s *PGStore) PruneDeliveries(ctx context.Context, age time.Duration) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE received_at < now() - make_interval(secs => $1)`, age.Seconds())
	return err
}

// CloudResources is the cloud resource registry on the same database.
func (s *PGStore) CloudResources() cloudres.PG { return cloudres.PG{DB: s.db} }

// Close releases the database connection pool.
func (s *PGStore) Close() error {
	return s.db.Close()
}

// Ping checks database connectivity for readiness probing.
func (s *PGStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *PGStore) TransitionTask(ctx context.Context, id string, from []string, to string) error {
	changed, err := s.changeStatus(ctx,
		`status = $2,
		 finished_at = CASE WHEN $2 IN ('done', 'failed', 'closed') THEN now() ELSE finished_at END,
		 gate = CASE WHEN $2 IN ('done', 'failed', 'closed') THEN '' ELSE gate END,
		 gate_since = CASE WHEN $2 IN ('done', 'failed', 'closed') THEN NULL ELSE gate_since END`,
		`id = $1 AND status = ANY($3)`, id, to, from)
	if err == nil && !changed {
		err = fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("%w (not in %v, cannot move to %s)", err, from, to)
	}
	return nil
}

func (s *PGStore) SetGate(ctx context.Context, id, gate string, since time.Time) error {
	_, err := s.changeStatus(ctx, `gate = $2, gate_since = $3`,
		`id = $1 AND status NOT IN ('done', 'failed', 'closed', 'backlog')`, id, gate, since)
	return err
}

func (s *PGStore) MarkStarted(ctx context.Context, id string) error {
	changed, err := s.changeStatus(ctx, `status = 'running'`,
		`id = $1 AND status IN ('queued', 'waiting-deps', 'running', 'in-review', 'waiting-human', 'waiting-poster')`, id)
	if err == nil && !changed {
		err = fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	return err
}

func (s *PGStore) SetAdmitted(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET admitted_at = now() WHERE id = $1 AND admitted_at IS NULL`, id)
	return err
}

func (s *PGStore) CountAdmitted(ctx context.Context, repository string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM tasks
		WHERE repository = $1 AND kind = 'agent' AND COALESCE(lane, '') <> ''
			AND (status IN ('waiting-deps', 'running', 'in-review', 'waiting-human', 'waiting-poster', 'closing')
				OR (status = 'queued' AND admitted_at IS NOT NULL))`, repository).Scan(&n)
	return n, err
}

func (s *PGStore) ListQueuedByRepository(ctx context.Context, repository string) ([]Task, error) {
	return s.tasks(ctx, `SELECT `+taskColumns+`
		FROM tasks WHERE repository = $1 AND kind = 'agent' AND COALESCE(lane, '') <> '' AND status = 'queued'
		ORDER BY created_at, issue_number, id`, repository)
}

// changeStatus updates the one task $1 names, SET set WHERE where, and
// records the task_status event of the change: the task's status and gate
// after it, by factoryd, through Record like every other event, so the
// console's stream carries it. It answers false when where matched no row;
// such an update, and one that left status and gate as they were, records
// nothing. A failed record is logged: it never fails the change.
func (s *PGStore) changeStatus(ctx context.Context, set, where string, args ...any) (bool, error) {
	e := Event{Kind: EventTaskStatus, Actor: FactoryDecider, Via: ViaFactoryd}
	var status, gate, wasStatus, wasGate string
	err := s.db.QueryRowContext(ctx, `UPDATE tasks SET `+set+`
		FROM (SELECT status AS was_status, gate AS was_gate FROM tasks WHERE id = $1 FOR UPDATE) was
		WHERE `+where+` RETURNING id, repository, issue_number, status, gate, was_status, was_gate`, args...).
		Scan(&e.TaskID, &e.Repository, &e.IssueNumber, &status, &gate, &wasStatus, &wasGate)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if status == wasStatus && gate == wasGate {
		return true, nil
	}
	e.Detail = map[string]any{"status": status, "gate": gate}
	if _, err := s.Record(ctx, e); err != nil {
		log.Printf("task %s: event %s: %v", e.TaskID, e.Kind, err)
	}
	return true, nil
}

func (s *PGStore) ListTasksByStatus(ctx context.Context, status string) ([]Task, error) {
	return s.tasks(ctx, `SELECT `+taskColumns+` FROM tasks WHERE status = $1 ORDER BY priority DESC, id`, status)
}

// tasks runs query, which returns taskColumns, and scans its rows.
func (s *PGStore) tasks(ctx context.Context, query string, args ...any) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		t, err := scanTask(rows.Scan)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func (s *PGStore) RecentlyFinishedTasks(ctx context.Context, within time.Duration) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM tasks WHERE status IN ('done', 'failed', 'closed') AND finished_at > now() - make_interval(secs => $1)
		 AND NOT EXISTS (SELECT 1 FROM holds WHERE holds.task_id = tasks.id AND holds.kind = $2)
		 ORDER BY finished_at`, within.Seconds(), policy.KindModel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// settleEpsilon is the smallest raise settlement books.
const settleEpsilon = 1e-9

// SettleSpend locks the task row, so the delta is taken from the model
// spend the raise replaces even while the gateway books calls. LiteLLM
// reports model spend only, so it is weighed against the task's model and
// settlement rows, never against spent_eur, which carries tool rows too.
func (s *PGStore) SettleSpend(ctx context.Context, id string, reported float64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after Commit; on an earlier error the tx is abandoned anyway
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = $1 FOR UPDATE`, id).Scan(new(int))
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return false, err
	}
	var booked float64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(sum(eur), 0)::float8 FROM ledger WHERE task_id = $1 AND kind IN ($2, $3)`,
		id, policy.KindModel, policy.KindSettlement).Scan(&booked); err != nil {
		return false, err
	}
	// below a nano-euro the two sums differ by float noise only
	if reported-booked < settleEpsilon {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur) VALUES ($1, $2, 'litellm', $3)`,
		id, policy.KindSettlement, reported-booked); err != nil {
		return false, err
	}
	if err := spendFromLedger(ctx, tx, id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *PGStore) UnsettledTasks(ctx context.Context, olderThan time.Duration) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM tasks WHERE status IN ('done', 'failed', 'closed') AND settled_at IS NULL
		 AND finished_at <= now() - make_interval(secs => $1)
		 AND NOT EXISTS (SELECT 1 FROM holds WHERE holds.task_id = tasks.id AND holds.kind = $2)
		 ORDER BY finished_at`, olderThan.Seconds(), policy.KindModel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *PGStore) SettleSpendFinal(ctx context.Context, id string, reported float64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after Commit; on an earlier error the tx is abandoned anyway
	var finished time.Time
	err = tx.QueryRowContext(ctx, `SELECT finished_at FROM tasks WHERE id = $1 AND settled_at IS NULL FOR UPDATE`, id).Scan(&finished)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return false, err
	}
	var booked float64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(sum(eur), 0)::float8 FROM ledger WHERE task_id = $1 AND kind IN ($2, $3)`,
		id, policy.KindModel, policy.KindSettlement).Scan(&booked); err != nil {
		return false, err
	}
	delta := reported - booked
	changed := delta >= settleEpsilon || delta <= -settleEpsilon
	if changed {
		// on the task's own UTC day, so the day cap of that day is what was charged
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger (task_id, day, kind, ref, eur) VALUES ($1, $2::date, $3, 'litellm-final', $4)`,
			id, finished.UTC(), policy.KindSettlement, delta); err != nil {
			return false, err
		}
		if err := spendFromLedger(ctx, tx, id); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET settled_at = now() WHERE id = $1`, id); err != nil {
		return false, err
	}
	return changed, tx.Commit()
}

// spendFromLedger sets the task's spent_eur to the sum of its ledger rows.
// Every booking writes both, so they agree, except on a task whose spend
// predates the ledger: spent_eur holds that spend with no row, and a
// settlement delta added to it would count it twice.
func spendFromLedger(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `UPDATE tasks SET spent_eur = (SELECT COALESCE(sum(eur), 0) FROM ledger WHERE task_id = $1) WHERE id = $1`, id)
	return err
}

func oneRow(res sql.Result, err error, what string) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return nil
}

func (s *PGStore) InsertSession(ctx context.Context, sess Session) error {
	findings, err := toJSON(sess.Findings)
	if err != nil {
		return err
	}
	restart, err := toJSON(sess.RestartState)
	if err != nil {
		return err
	}
	if restart == nil {
		restart = []byte("{}") // NOT NULL column; a fresh session has no restart state
	}
	var created any
	if sess.CreatedAt.IsZero() {
		created = nil
	} else {
		created = sess.CreatedAt
	}
	_, err = s.db.ExecContext(ctx,
		`WITH s AS (INSERT INTO sessions (id, task_id, parent_session_id, findings, restart_state, created_at, model)
		            VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, COALESCE($6, now()), $7))
		 UPDATE tasks SET model_alias = $7 WHERE id = $2`,
		sess.ID, sess.TaskID, sess.ParentSessionID, findings, restart, created, sess.Model)
	return err
}

func (s *PGStore) Session(ctx context.Context, id string) (Session, bool, error) {
	var sess Session
	var findings, restart []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT id, task_id, parent_session_id, findings, restart_state, created_at
		 FROM sessions WHERE id = $1`, id,
	).Scan(&sess.ID, &sess.TaskID, &sess.ParentSessionID, &findings, &restart, &sess.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}
	if sess.Findings, err = fromJSON(findings); err != nil {
		return Session{}, false, fmt.Errorf("findings: %w", err)
	}
	sess.RestartState = restart
	return sess, true, nil
}

func (s *PGStore) UpdateSession(ctx context.Context, id string, findings []string, restartState []byte) error {
	fb, err := toJSON(findings)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET findings = $2::jsonb, restart_state = $3::jsonb WHERE id = $1`,
		id, fb, restartState)
	return oneRow(res, err, "session "+id)
}

func (s *PGStore) SetTaskRisk(ctx context.Context, id, class string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET risk_class = $2 WHERE id = $1`, id, class)
	return oneRow(res, err, "task "+id)
}

// SetTaskAlertFingerprint records the SigNoz alert fingerprint that filed
// an alert card's task (alert-precheck, roadmap 181 item 3.12).
func (s *PGStore) SetTaskAlertFingerprint(ctx context.Context, id, fingerprint string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET alert_fingerprint = $2 WHERE id = $1`, id, fingerprint)
	return oneRow(res, err, "task "+id)
}

func (s *PGStore) OpenApproval(ctx context.Context, a ApprovalRequest) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after Commit; on an earlier error the tx is abandoned anyway
	if a.Kind == ApprovalMerge || a.Kind == ApprovalPlan {
		if _, err := tx.ExecContext(ctx,
			`UPDATE approval_requests SET status = 'denied', decided_by = $2, decided_at = now(), decision_note = 'superseded by a newer request'
			 WHERE task_id = $1 AND kind = $3 AND status = 'pending'`, a.TaskID, FactoryDecider, a.Kind); err != nil {
			return 0, err
		}
	}
	spec := a.Spec
	if spec == nil {
		spec = json.RawMessage(`{}`)
	}
	var id int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO approval_requests (kind, task_id, project, pr_number, spec, reason, monthly_eur)
		 VALUES ($1, $2, $3, NULLIF($4, 0), $5::jsonb, $6, $7) RETURNING id`,
		a.Kind, a.TaskID, a.Project, a.PRNumber, []byte(spec), a.Reason, a.MonthlyEUR).Scan(&id); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *PGStore) ResolveApprovals(ctx context.Context, taskID, kind, status, by, note string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE approval_requests SET status = $3, decided_by = $4, decided_at = now(), decision_note = $5
		 WHERE task_id = $1 AND ($2 = '' OR kind = $2) AND status = 'pending'`, taskID, kind, status, by, note)
	return err
}

func (s *PGStore) UndecideApproval(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE approval_requests SET status = 'pending', decided_by = '', decided_at = NULL, decision_note = ''
		WHERE id = $1`, id)
	return err
}

func (s *PGStore) ReopenApproval(ctx context.Context, taskID, kind, note string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE approval_requests SET status = 'pending', decided_by = '', decided_at = NULL, decision_note = $3
		WHERE id = (SELECT max(id) FROM approval_requests WHERE task_id = $1 AND kind = $2) AND status <> 'denied'`, taskID, kind, note)
	return err
}

// DecideApproval decides one pending approval request of kind: a human's
// approve or deny in the console. ErrNotFound when no such request is
// pending.
func (s *PGStore) DecideApproval(ctx context.Context, id int64, kind, status, by, note string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE approval_requests SET status = $3, decided_by = $4, decided_at = now(), decision_note = $5
		 WHERE id = $1 AND kind = $2 AND status = 'pending'`, id, kind, status, by, note)
	return oneRow(res, err, fmt.Sprintf("pending %s approval request %d", kind, id))
}

func (s *PGStore) ApprovalRequests(ctx context.Context, status string) ([]ApprovalRequest, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, task_id, project, COALESCE(pr_number, 0), spec, reason, monthly_eur::float8,
		        status, decided_by, decided_at, decision_note, created_at
		 FROM approval_requests WHERE status = $1 ORDER BY id`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApprovalRequest
	for rows.Next() {
		var a ApprovalRequest
		var spec []byte
		if err := rows.Scan(&a.ID, &a.Kind, &a.TaskID, &a.Project, &a.PRNumber, &spec, &a.Reason, &a.MonthlyEUR,
			&a.Status, &a.DecidedBy, &a.DecidedAt, &a.DecisionNote, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Spec = spec
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *PGStore) RollRecorded(ctx context.Context, sha, image string) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM rolls WHERE sha = $1 AND image = $2)`, sha, image).Scan(&found)
	return found, err
}

func (s *PGStore) RecordRoll(ctx context.Context, r Roll) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO rolls (sha, image, deployment, pushed_at, applied_at, live_at) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (sha, image) DO NOTHING`, r.SHA, r.Image, r.Deployment, r.PushedAt, r.AppliedAt, r.LiveAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DeployedCommit is the commit of deployment's newest recorded roll: the
// tree its digest pins last rolled from, the one live when an alert on it
// fires (roadmap 194). ok false when no roll of it is recorded.
func (s *PGStore) DeployedCommit(ctx context.Context, deployment string) (string, bool, error) {
	var sha string
	err := s.db.QueryRowContext(ctx,
		`SELECT sha FROM rolls WHERE deployment = $1 ORDER BY live_at DESC, applied_at DESC, pushed_at DESC, sha LIMIT 1`,
		deployment).Scan(&sha)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return sha, true, nil
}
