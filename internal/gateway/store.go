package gateway

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Task struct {
	ID        string
	BudgetEUR float64
	SpentEUR  float64
	Status    string
	// Classes are the task's capability classes (tasks.required_capabilities).
	Classes []string
}

type AuditRow struct {
	TaskID     string
	Decision   string
	Reason     string
	ModelAlias string
	SpendEUR   *float64
	// PolicyVersion is the policy version the decision was made under; nil
	// only when no version could be read.
	PolicyVersion *int64
}

// Store is the task and audit state every gateway decision reads and writes.
// The production implementation is Postgres; tests use an in-memory fake.
type Store interface {
	Task(ctx context.Context, id string) (Task, bool, error)
	// AddSpent books one model call's cost onto the task and the ledger and
	// returns the task's new total.
	AddSpent(ctx context.Context, id, alias string, cost float64) (float64, error)
	InsertAudit(ctx context.Context, row AuditRow) error
	CurrentPolicy(ctx context.Context) (policy.Version, bool, error)
	// DayTotal is today's (UTC) ledger total over all tasks.
	DayTotal(ctx context.Context) (float64, error)
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
	return policy.Migrate(ctx, s.db)
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

func (s *PGStore) Task(ctx context.Context, id string) (Task, bool, error) {
	var t Task
	var classes []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT id, budget_eur::float8, spent_eur::float8, status, required_capabilities FROM tasks WHERE id = $1`, id,
	).Scan(&t.ID, &t.BudgetEUR, &t.SpentEUR, &t.Status, &classes)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, false, nil
	}
	if err != nil {
		return Task{}, false, err
	}
	if err := json.Unmarshal(classes, &t.Classes); err != nil {
		return Task{}, false, fmt.Errorf("task %s required_capabilities: %w", id, err)
	}
	return t, true, nil
}

// AddSpent is one statement, so the task's spend and its ledger row are
// booked together or not at all; the increment is atomic, so concurrent
// calls of one task add up.
func (s *PGStore) AddSpent(ctx context.Context, id, alias string, cost float64) (float64, error) {
	var total float64
	err := s.db.QueryRowContext(ctx,
		`WITH t AS (UPDATE tasks SET spent_eur = spent_eur + $2 WHERE id = $1 RETURNING spent_eur),
		      l AS (INSERT INTO ledger (task_id, kind, ref, eur) SELECT $1, $3, $4, $2 FROM t)
		 SELECT spent_eur::float8 FROM t`, id, cost, policy.KindModel, alias,
	).Scan(&total)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("add spent: no task row for %s", id)
	}
	return total, err
}

func (s *PGStore) InsertAudit(ctx context.Context, row AuditRow) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit (task_id, decision, reason, model_alias, spend_eur, policy_version_id) VALUES ($1, $2, $3, $4, $5, $6)`,
		row.TaskID, row.Decision, row.Reason, row.ModelAlias, row.SpendEUR, row.PolicyVersion)
	return err
}

func (s *PGStore) CurrentPolicy(ctx context.Context) (policy.Version, bool, error) {
	return policy.Current(ctx, s.db)
}

func (s *PGStore) DayTotal(ctx context.Context) (float64, error) {
	return policy.DayTotal(ctx, s.db)
}
