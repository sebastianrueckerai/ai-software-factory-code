package factoryd

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrations embed.FS

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
}

// TaskUsage is what a task has used of its caps and what a human granted
// on top; the workflow keeps it on the row so a new run continues the
// count (ruling 93: the caps hold across attempts, so across runs).
type TaskUsage struct {
	AgentTime     time.Duration // the agent's working time, summed over the sessions
	Attempts      int
	ExtraTime     time.Duration // granted by human answers near the cap
	ExtraAttempts int           // one per human answer
}

// IntakeEvent is a Forgejo comment or review recorded against a task's issue
// or pull request; the ones that ask for changes are also signalled to the
// task's workflow.
type IntakeEvent struct {
	ID        int64
	TaskID    string
	Kind      string // issue_comment, pull_request_comment, or the review's X-Forgejo-Event-Type
	Author    string
	Body      string
	CreatedAt time.Time
	// DeliveryID is the webhook delivery that produced it: a redelivery
	// inserts no second row.
	DeliveryID string
	// CommentID is Forgejo's id of the comment (0 for a review): the heal
	// pass re-reads comments, and a comment is recorded once whether the
	// webhook or the re-read saw it first.
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
}

// Store is the durable task and session state. The production implementation
// is Postgres; tests use an in-memory fake.
type Store interface {
	CreateTask(ctx context.Context, t Task) error
	Task(ctx context.Context, id string) (Task, bool, error)
	// TransitionTask moves a task to status `to` only from one of the
	// statuses in from; any other current status is an error. Entering done
	// or failed stamps the finish time.
	TransitionTask(ctx context.Context, id string, from []string, to string) error
	// MarkStarted moves a queued, running, in-review or waiting-human task to running and
	// stamps started_at on the first call only.
	MarkStarted(ctx context.Context, id string) error
	// ListTasksByStatus lists tasks in status, highest priority first.
	ListTasksByStatus(ctx context.Context, status string) ([]Task, error)
	// RecentlyFinishedTasks lists tasks that finished (done or failed)
	// within the last `within`, on the store's clock.
	RecentlyFinishedTasks(ctx context.Context, within time.Duration) ([]string, error)
	// SettleSpend raises spent_eur to at least reported.
	SettleSpend(ctx context.Context, id string, reported float64) error
	// TaskByIssue looks up a task by the Forgejo issue mirroring it, for
	// webhook redelivery idempotency; false means no task is linked to that
	// issue yet.
	TaskByIssue(ctx context.Context, repository string, issueNumber int64) (Task, bool, error)
	// InsertIntakeEvent records e and returns its id; 0 when the same
	// delivery or comment was recorded before.
	InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error)
	// IntakeEvents lists a task's events, oldest first.
	IntakeEvents(ctx context.Context, taskID string) ([]IntakeEvent, error)
	// MarkSignalled records that event id reached the task's workflow.
	MarkSignalled(ctx context.Context, id int64) error
	// UnaddressedChangeRequests lists the signalled change requests no
	// session has started on yet: newer than the task's latest session.
	UnaddressedChangeRequests(ctx context.Context, taskID string) ([]IntakeEvent, error)
	// SetTaskPR records the pull request a task's workflow opened.
	SetTaskPR(ctx context.Context, id string, number int64) error
	// MarkHalfWay records that the task's half-way check-in fired.
	MarkHalfWay(ctx context.Context, id string) error
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
}

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
	if err := s.migrate(ctx); err != nil {
		db.Close() // #nosec G104 -- already returning the migrate error; the close error would only mask it
		return nil, err
	}
	return s, nil
}

func (s *PGStore) migrate(ctx context.Context) error {
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		sqlText, err := migrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		for _, stmt := range splitSQL(string(sqlText)) {
			if _, err := s.db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migrate %s: %w", e.Name(), err)
			}
		}
	}
	return nil
}

func splitSQL(sqlText string) []string {
	var stmts []string
	for _, stmt := range strings.Split(sqlText, ";") {
		if strings.TrimSpace(stmt) != "" {
			stmts = append(stmts, stmt)
		}
	}
	return stmts
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
	caps, err := toJSON(t.RequiredCapabilities)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, priority, required_capabilities, risk_class, budget_eur, spent_eur, status,
		                    objective, repository, sha, kind, issue_number, source)
		 VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		t.ID, t.Priority, caps, t.RiskClass, t.BudgetEUR, t.SpentEUR, t.Status,
		t.Objective, t.Repository, t.SHA, defaultKind(t.Kind), t.IssueNumber, t.Source)
	return err
}

func defaultKind(kind string) string {
	if kind == "" {
		return "agent"
	}
	return kind
}

const taskColumns = `id, priority, required_capabilities, risk_class, budget_eur::float8, spent_eur::float8, status,
	objective, repository, sha, kind, issue_number, source, pr_number, half_way_done,
	agent_time_ns, attempts, extra_time_ns, extra_attempts`

func scanTask(scan func(dest ...any) error) (Task, error) {
	var t Task
	var caps []byte
	if err := scan(&t.ID, &t.Priority, &caps, &t.RiskClass, &t.BudgetEUR, &t.SpentEUR, &t.Status,
		&t.Objective, &t.Repository, &t.SHA, &t.Kind, &t.IssueNumber, &t.Source, &t.PRNumber, &t.HalfWayDone,
		&t.Usage.AgentTime, &t.Usage.Attempts, &t.Usage.ExtraTime, &t.Usage.ExtraAttempts); err != nil {
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

func (s *PGStore) MarkSignalled(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE intake_events SET signalled = true WHERE id = $1`, id)
	return oneRow(res, err, fmt.Sprintf("intake event %d", id))
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
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET agent_time_ns = $2, attempts = $3, extra_time_ns = $4, extra_attempts = $5 WHERE id = $1`,
		id, int64(u.AgentTime), u.Attempts, int64(u.ExtraTime), u.ExtraAttempts)
	return oneRow(res, err, "task "+id)
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
		        finished_at = CASE WHEN $2 IN ('done', 'failed') THEN now() ELSE finished_at END
		 WHERE id = $1 AND status = ANY($3)`, id, to, from)
	if err := oneRow(res, err, "task "+id); err != nil {
		return fmt.Errorf("%w (not in %v, cannot move to %s)", err, from, to)
	}
	return nil
}

func (s *PGStore) MarkStarted(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'running', started_at = COALESCE(started_at, now())
		 WHERE id = $1 AND status IN ('queued', 'running', 'in-review', 'waiting-human')`, id)
	return oneRow(res, err, "task "+id)
}

func (s *PGStore) ListTasksByStatus(ctx context.Context, status string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE status = $1 ORDER BY priority DESC, id`, status)
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
		`SELECT id FROM tasks WHERE status IN ('done', 'failed') AND finished_at > now() - make_interval(secs => $1)
		 ORDER BY finished_at`, within.Seconds())
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

func (s *PGStore) SettleSpend(ctx context.Context, id string, reported float64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET spent_eur = GREATEST(spent_eur, $2) WHERE id = $1`, id, reported)
	return oneRow(res, err, "task "+id)
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
		`INSERT INTO sessions (id, task_id, parent_session_id, findings, restart_state, created_at)
		 VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, COALESCE($6, now()))`,
		sess.ID, sess.TaskID, sess.ParentSessionID, findings, restart, created)
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
