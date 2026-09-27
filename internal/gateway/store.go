package gateway

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Task struct {
	ID        string
	BudgetEUR float64
	SpentEUR  float64
	Status    string
}

type AuditRow struct {
	TaskID     string
	Decision   string
	Reason     string
	ModelAlias string
	SpendEUR   *float64
}

// Store is the task and audit state every gateway decision reads and writes.
// The production implementation is Postgres; tests use an in-memory fake.
type Store interface {
	Task(ctx context.Context, id string) (Task, bool, error)
	AddSpent(ctx context.Context, id string, cost float64) (float64, error)
	InsertAudit(ctx context.Context, row AuditRow) error
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

func (s *PGStore) Task(ctx context.Context, id string) (Task, bool, error) {
	var t Task
	err := s.db.QueryRowContext(ctx,
		`SELECT id, budget_eur::float8, spent_eur::float8, status FROM tasks WHERE id = $1`, id,
	).Scan(&t.ID, &t.BudgetEUR, &t.SpentEUR, &t.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, false, nil
	}
	if err != nil {
		return Task{}, false, err
	}
	return t, true, nil
}

// AddSpent books one model call's cost onto the task and returns the new
// total. The increment is atomic, so concurrent calls of one task add up.
func (s *PGStore) AddSpent(ctx context.Context, id string, cost float64) (float64, error) {
	var total float64
	err := s.db.QueryRowContext(ctx,
		`UPDATE tasks SET spent_eur = spent_eur + $2 WHERE id = $1 RETURNING spent_eur::float8`, id, cost,
	).Scan(&total)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("add spent: no task row for %s", id)
	}
	return total, err
}

func (s *PGStore) InsertAudit(ctx context.Context, row AuditRow) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit (task_id, decision, reason, model_alias, spend_eur) VALUES ($1, $2, $3, $4, $5)`,
		row.TaskID, row.Decision, row.Reason, row.ModelAlias, row.SpendEUR)
	return err
}
