package factoryd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
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
	// HalfWayDone: the task's half-way check-in fired (once per task).
	HalfWayDone bool
	Usage       TaskUsage
	// ReproCommand is the task's reproduction (the issue's fenced block
	// labelled repro, or the API's repro_command): GateReproduction runs it
	// against the pull request head and it must pass. Empty = none.
	ReproCommand string
	// CreatedAt is when the work was asked for: the issue's created_at, or
	// the API call (zero at CreateTask: now).
	CreatedAt  time.Time
	FinishedAt *time.Time
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
}

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

// FinalStatuses end a task: merged or given up, and closed, an issue
// closed as not reproducible (ruling 150).
var FinalStatuses = []string{"done", "failed", "closed"}

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
	// MarkStarted moves a queued, running, in-review, waiting-human or
	// waiting-poster task to running.
	MarkStarted(ctx context.Context, id string) error
	// ListTasksByStatus lists tasks in status, highest priority first.
	ListTasksByStatus(ctx context.Context, status string) ([]Task, error)
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
	// MarkHalfWay records that the task's half-way check-in fired.
	MarkHalfWay(ctx context.Context, id string) error
	// SetAgentSummary records the agent's last message of its latest
	// pushed session.
	SetAgentSummary(ctx context.Context, id, summary string) error
	// MonthCapRefusal is the reason of the task's newest refused_month_cap
	// audit row since since, if any.
	MonthCapRefusal(ctx context.Context, taskID string, since time.Time) (string, bool, error)
	// PromoteSuggested starts a suggested improvement (a writer's agent
	// label): queued at base sha, in the intake gate from now; false when
	// the task was not suggested.
	PromoteSuggested(ctx context.Context, id, sha string) (bool, error)
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
	// OpenApproval records a pending approval request and returns its id;
	// a merge request denies the task's earlier pending one (a retry, or
	// a new head) as superseded.
	OpenApproval(ctx context.Context, a ApprovalRequest) (int64, error)
	// ResolveApprovals decides the task's pending approval requests of
	// kind ("" = every kind): status approved or denied, by whom
	// ("factoryd" for the factory's own decisions) and why.
	ResolveApprovals(ctx context.Context, taskID, kind, status, by, note string) error
	// NoteApproval sets the note of the task's pending requests of kind:
	// why an approval has not taken effect yet (a refused merge).
	NoteApproval(ctx context.Context, taskID, kind, note string) error
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
	// UncommittedRolls and MarkRollsCommitted are a roll task's requests
	// its branch does not carry yet (RollCommit).
	UncommittedRolls(ctx context.Context, taskID string) ([]RollRequest, error)
	MarkRollsCommitted(ctx context.Context, taskID string, shas []string, head string) error
	// RollHead is the branch commit RollCommit last wrote for a roll task.
	RollHead(ctx context.Context, taskID string) (string, error)
	// SetTaskSHA sets the commit a roll task's branch was cut from.
	SetTaskSHA(ctx context.Context, id, sha string) error
}

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
	db *sql.DB
}

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

// insertTask inserts t, onConflict the insert's conflict clause.
func (s *PGStore) insertTask(ctx context.Context, t Task, onConflict string) error {
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
		                    objective, repository, sha, kind, issue_number, source, repro_command, created_at, gate, gate_since,
		                    author, author_writer, parent_task)
		 VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, COALESCE($15, now()),
		         CASE WHEN $11 = 'agent' AND $7 <> 'suggested' THEN $16 ELSE '' END,
		         CASE WHEN $11 = 'agent' AND $7 <> 'suggested' THEN COALESCE($15, now()) END, $17, $18, NULLIF($19, ''))
		 ` + onConflict // #nosec G202 -- onConflict is one of insertTask's two constant clauses
	_, err = s.db.ExecContext(ctx, q,
		t.ID, t.Priority, caps, t.RiskClass, t.BudgetEUR, t.SpentEUR, t.Status,
		t.Objective, t.Repository, t.SHA, defaultKind(t.Kind), t.IssueNumber, t.Source, t.ReproCommand, created, GateIntake,
		t.Author, t.AuthorWriter, t.Parent)
	return err
}

func (s *PGStore) OpenReportTask(ctx context.Context, t Task) (Task, error) {
	if err := s.insertTask(ctx, t, `ON CONFLICT (parent_task, source) WHERE parent_task IS NOT NULL DO NOTHING`); err != nil {
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

func defaultKind(kind string) string {
	if kind == "" {
		return "agent"
	}
	return kind
}

const taskColumns = `id, priority, required_capabilities, risk_class, budget_eur::float8, spent_eur::float8, status,
	objective, repository, sha, kind, issue_number, source, pr_number, half_way_done,
	agent_time_ns, attempts, extra_time_ns, extra_attempts, poster_attempts, repro_command, created_at, finished_at, gate, gate_since,
	author, author_writer, agent_summary, COALESCE(parent_task, ''), model_alias, reverted_at`

func scanTask(scan func(dest ...any) error) (Task, error) {
	var t Task
	var caps []byte
	if err := scan(&t.ID, &t.Priority, &caps, &t.RiskClass, &t.BudgetEUR, &t.SpentEUR, &t.Status,
		&t.Objective, &t.Repository, &t.SHA, &t.Kind, &t.IssueNumber, &t.Source, &t.PRNumber, &t.HalfWayDone,
		&t.Usage.AgentTime, &t.Usage.Attempts, &t.Usage.ExtraTime, &t.Usage.ExtraAttempts, &t.Usage.PosterAttempts, &t.ReproCommand,
		&t.CreatedAt, &t.FinishedAt, &t.Gate, &t.GateSince, &t.Author, &t.AuthorWriter, &t.AgentSummary, &t.Parent,
		&t.ModelAlias, &t.RevertedAt); err != nil {
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

func (s *PGStore) InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO intake_events (task_id, kind, author, body, delivery_id, comment_id, trusted, signalled)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING RETURNING id`,
		e.TaskID, e.Kind, e.Author, e.Body, e.DeliveryID, e.CommentID, e.Trusted, e.Signalled).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

func (s *PGStore) IntakeEvents(ctx context.Context, taskID string) ([]IntakeEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_id, kind, author, body, created_at, delivery_id, comment_id, trusted, signalled
		 FROM intake_events WHERE task_id = $1 ORDER BY id`, taskID)
	return scanIntakeEvents(rows, err)
}

func (s *PGStore) UnaddressedChangeRequests(ctx context.Context, taskID string) ([]IntakeEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_id, kind, author, body, created_at, delivery_id, comment_id, trusted, signalled
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
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Kind, &e.Author, &e.Body, &e.CreatedAt, &e.DeliveryID, &e.CommentID, &e.Trusted, &e.Signalled); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
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

func (s *PGStore) MarkHalfWay(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET half_way_done = true WHERE id = $1`, id)
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

func (s *PGStore) MonthCapRefusal(ctx context.Context, taskID string, since time.Time) (string, bool, error) {
	var reason string
	err := s.db.QueryRowContext(ctx,
		`SELECT reason FROM audit WHERE task_id = $1 AND decision = $2 AND ts >= $3 ORDER BY ts DESC, id DESC LIMIT 1`,
		taskID, policy.RefusedMonthCap, since).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return reason, err == nil, err
}

// MonthSpend is the UTC calendar month's variable spend over the ledger,
// standing lines left out, as the gateway's month headroom counts it, and
// whether the gateway refused a call at the month cap this month.
func (s *PGStore) MonthSpend(ctx context.Context) (MonthSpend, error) {
	var m MonthSpend
	err := s.db.QueryRowContext(ctx,
		`SELECT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM'),
		        `+policy.MonthSpentSQL+`::float8,
		        EXISTS (SELECT 1 FROM audit WHERE decision = $1 AND ts >= date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')`,
		policy.RefusedMonthCap).Scan(&m.Month, &m.EUR, &m.Refused)
	return m, err
}

func (s *PGStore) SetAgentSummary(ctx context.Context, id, summary string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET agent_summary = $2 WHERE id = $1`, id, summary)
	return oneRow(res, err, "task "+id)
}

func (s *PGStore) ToHumanLane(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET kind = 'human', status = 'waiting-human', gate = '', gate_since = NULL
		WHERE id = $1 AND kind = 'agent' AND status = 'queued'`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *PGStore) PromoteSuggested(ctx context.Context, id, sha string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'queued', sha = $2, gate = $3, gate_since = now()
		WHERE id = $1 AND status = 'suggested'`, id, sha, GateIntake)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
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
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status = $2,
		        finished_at = CASE WHEN $2 IN ('done', 'failed', 'closed') THEN now() ELSE finished_at END,
		        gate = CASE WHEN $2 IN ('done', 'failed', 'closed') THEN '' ELSE gate END,
		        gate_since = CASE WHEN $2 IN ('done', 'failed', 'closed') THEN NULL ELSE gate_since END
		 WHERE id = $1 AND status = ANY($3)`, id, to, from)
	if err := oneRow(res, err, "task "+id); err != nil {
		return fmt.Errorf("%w (not in %v, cannot move to %s)", err, from, to)
	}
	return nil
}

func (s *PGStore) SetGate(ctx context.Context, id, gate string, since time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET gate = $2, gate_since = $3
		WHERE id = $1 AND status NOT IN ('done', 'failed', 'closed', 'suggested')`, id, gate, since)
	return err
}

func (s *PGStore) MarkStarted(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'running'
		 WHERE id = $1 AND status IN ('queued', 'running', 'in-review', 'waiting-human', 'waiting-poster')`, id)
	return oneRow(res, err, "task "+id)
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
		// on the task's own day, so the day cap of that day is what was charged
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger (task_id, day, kind, ref, eur) VALUES ($1, $2::date, $3, 'litellm-final', $4)`,
			id, finished, policy.KindSettlement, delta); err != nil {
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

func (s *PGStore) OpenApproval(ctx context.Context, a ApprovalRequest) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after Commit; on an earlier error the tx is abandoned anyway
	if a.Kind == "merge" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE approval_requests SET status = 'denied', decided_by = $2, decided_at = now(), decision_note = 'superseded by a newer request'
			 WHERE task_id = $1 AND kind = 'merge' AND status = 'pending'`, a.TaskID, FactoryDecider); err != nil {
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

func (s *PGStore) NoteApproval(ctx context.Context, taskID, kind, note string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE approval_requests SET decision_note = $3
		WHERE task_id = $1 AND kind = $2 AND status = 'pending'`, taskID, kind, note)
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
