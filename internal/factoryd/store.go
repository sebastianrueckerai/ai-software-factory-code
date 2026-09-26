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
	Branch               string
}

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
	SetTaskStatus(ctx context.Context, id, status string) error
	ListTasksByStatus(ctx context.Context, status string) ([]Task, error)
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
		db.Close()
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

func fromJSON(b []byte) []string {
	var out []string
	json.Unmarshal(b, &out)
	return out
}

func (s *PGStore) CreateTask(ctx context.Context, t Task) error {
	caps, err := toJSON(t.RequiredCapabilities)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, priority, required_capabilities, risk_class, budget_eur, spent_eur, status,
		                    objective, repository, sha, branch)
		 VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7, $8, $9, $10, $11)`,
		t.ID, t.Priority, caps, t.RiskClass, t.BudgetEUR, t.SpentEUR, t.Status,
		t.Objective, t.Repository, t.SHA, t.Branch)
	return err
}

func (s *PGStore) Task(ctx context.Context, id string) (Task, bool, error) {
	var t Task
	var caps []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT id, priority, required_capabilities, risk_class, budget_eur::float8, spent_eur::float8, status,
		       objective, repository, sha, branch
		 FROM tasks WHERE id = $1`, id,
	).Scan(&t.ID, &t.Priority, &caps, &t.RiskClass, &t.BudgetEUR, &t.SpentEUR, &t.Status,
		&t.Objective, &t.Repository, &t.SHA, &t.Branch)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, false, nil
	}
	if err != nil {
		return Task{}, false, err
	}
	t.RequiredCapabilities = fromJSON(caps)
	return t, true, nil
}

// Close releases the database connection pool.
func (s *PGStore) Close() error {
	return s.db.Close()
}

// Ping checks database connectivity for readiness probing.
func (s *PGStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *PGStore) ListTasksByStatus(ctx context.Context, status string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, priority, required_capabilities, risk_class, budget_eur::float8, spent_eur::float8, status,
		        objective, repository, sha, branch
		 FROM tasks WHERE status = $1 ORDER BY priority DESC, id`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		var t Task
		var caps []byte
		if err := rows.Scan(&t.ID, &t.Priority, &caps, &t.RiskClass, &t.BudgetEUR, &t.SpentEUR, &t.Status,
			&t.Objective, &t.Repository, &t.SHA, &t.Branch); err != nil {
			return nil, err
		}
		t.RequiredCapabilities = fromJSON(caps)
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func (s *PGStore) SetTaskStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = $2 WHERE id = $1`, id, status)
	return err
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
	sess.Findings = fromJSON(findings)
	sess.RestartState = restart
	return sess, true, nil
}

func (s *PGStore) UpdateSession(ctx context.Context, id string, findings []string, restartState []byte) error {
	fb, err := toJSON(findings)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE sessions SET findings = $2::jsonb, restart_state = $3::jsonb WHERE id = $1`,
		id, fb, restartState)
	return err
}
