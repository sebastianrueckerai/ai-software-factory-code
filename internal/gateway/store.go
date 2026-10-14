package gateway

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema"
)

type Task struct {
	ID        string
	BudgetEUR float64
	SpentEUR  float64
	Status    string
	// Classes are the task's capability classes (tasks.required_capabilities).
	Classes []string
	// Repository is the task's owner/name: the project of its resource
	// envelope.
	Repository string
	// ModelAlias is the alias of the task's current session
	// (tasks.model_alias), set before its pod starts: the one alias its
	// agent may call.
	ModelAlias string
}

type AuditRow struct {
	TaskID string
	// Kind is the factoryd purpose of the call (policy.Purposes), factoryd
	// for a factoryd call refused before its purpose is known, "" for an
	// agent's: stored as agent.
	Kind string
	// Thread is the chat thread a factoryd call served (roadmap 239), ""
	// for every other call.
	Thread     string
	Decision   string
	Reason     string
	ModelAlias string
	// Tool is the tool of an MCP tool call ("" for a model call).
	Tool     string
	SpendEUR *float64
	// PolicyVersion is the policy version the decision was made under; nil
	// only when no version could be read.
	PolicyVersion *int64
	// OutputLimit and HoldEUR are an admitted call's clamped output limit
	// and the worst case held for it.
	OutputLimit *int
	HoldEUR     *float64
	// OverHold is the booking's COST OVER HOLD flag (policy.OverHold), ""
	// within the hold or without a booking.
	OverHold string
	// Deployment is the LiteLLM deployment that answered a model call, as
	// LiteLLM names it (provider/model); "" when LiteLLM named none.
	Deployment string
	// The call's own truth (roadmap 198 item 1): the tokens the provider
	// reported, 0 on a call that never reported them; the modeled
	// pay-as-you-go cost LiteLLM computed from them, nil when none was
	// reported (under the GLM Coding Plan no money is billed: tokens are
	// the stored fact, every EUR figure a modeled equivalent); how long
	// the call ran from its admit to its booking; and the upstream HTTP
	// status, "" when the call never went upstream.
	PromptTokens, CachedTokens, OutputTokens int
	CallCostEUR                              *float64
	DurationMS                               int
	Status                                   string
}

func (row AuditRow) kind() string { return cmp.Or(row.Kind, AuditKindAgent) }

// AuditKindAgent is the audit kind of an agent's call.
const AuditKindAgent = "agent"

// Store is the task and audit state every gateway decision reads and writes.
// The production implementation is Postgres; tests use an in-memory fake.
type Store interface {
	Task(ctx context.Context, id string) (Task, bool, error)
	InsertAudit(ctx context.Context, row AuditRow) error
	CurrentPolicy(ctx context.Context) (policy.Version, bool, error)
	// TakeHold admits a call into the free task and day headroom (see
	// policy.TakeHold); a *policy.Refusal when it does not fit.
	TakeHold(ctx context.Context, r policy.HoldRequest) (policy.Hold, error)
	// ReleaseHold books the call's cost (nil = the hold) onto the task and
	// the ledger, with the call's usage, and returns the task's spend.
	ReleaseHold(ctx context.Context, h policy.Hold, cost *float64, u policy.Usage) (policy.Booking, error)
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
	if err := schema.Migrate(ctx, db); err != nil {
		db.Close() // #nosec G104 -- already returning the migrate error; the close error would only mask it
		return nil, err
	}
	return s, nil
}

func (s *PGStore) Task(ctx context.Context, id string) (Task, bool, error) {
	var t Task
	var classes []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT id, budget_eur::float8, spent_eur::float8, status, required_capabilities, repository, model_alias FROM tasks WHERE id = $1`, id,
	).Scan(&t.ID, &t.BudgetEUR, &t.SpentEUR, &t.Status, &classes, &t.Repository, &t.ModelAlias)
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

func (s *PGStore) InsertAudit(ctx context.Context, row AuditRow) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit (task_id, decision, reason, model_alias, tool, spend_eur, policy_version_id, output_limit, hold_eur, over_hold, kind, deployment,
		 prompt_tokens, cached_tokens, output_tokens, call_cost_eur, duration_ms, status, thread)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`,
		row.TaskID, row.Decision, row.Reason, row.ModelAlias, row.Tool, row.SpendEUR, row.PolicyVersion, row.OutputLimit, row.HoldEUR, row.OverHold, row.kind(), row.Deployment,
		row.PromptTokens, row.CachedTokens, row.OutputTokens, row.CallCostEUR, row.DurationMS, row.Status, row.Thread)
	return err
}

func (s *PGStore) CurrentPolicy(ctx context.Context) (policy.Version, bool, error) {
	return policy.Current(ctx, s.db)
}

func (s *PGStore) TakeHold(ctx context.Context, r policy.HoldRequest) (policy.Hold, error) {
	return policy.TakeHold(ctx, s.db, r)
}

func (s *PGStore) ReleaseHold(ctx context.Context, h policy.Hold, cost *float64, u policy.Usage) (policy.Booking, error) {
	return policy.ReleaseHold(ctx, s.db, h, cost, u)
}

// DB is the store's pool, shared with the cloud registry on the same
// database.
func (s *PGStore) DB() *sql.DB { return s.db }
