package policy

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Schema is shared by factoryd and the gateway: both apply it on boot, so
// whichever starts first on an empty database finds the tables.
//
//go:embed schema.sql
var schema string

// Ledger kinds.
const (
	KindModel      = "model"
	KindTool       = "tool"
	KindResource   = "resource"
	KindSettlement = "settlement"
	// KindStanding is a standing resource's accrual, booked to its project
	// (cloudres.StandingLedgerTask), outside any task and day headroom.
	KindStanding = "standing"
)

// Version is the configuration tree at one commit of the infra
// repository's main, immutable once written. ActivatedAt is when it last
// became the version in force.
type Version struct {
	ID     int64  `json:"id"`
	Commit string `json:"commit"`
	Tree
	LoadedAt    time.Time `json:"loaded_at"`
	ActivatedAt time.Time `json:"activated_at"`
}

// Migrate applies the schema as one statement batch: one implicit
// transaction under an advisory lock, so two binaries booting at once do
// not race on it.
func Migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate policy schema: %w", err)
	}
	return nil
}

// Record stores tree under its commit unless that commit is known already,
// and activates it unless it is in force already. inserted is false when
// the commit was loaded before.
func Record(ctx context.Context, db *sql.DB, commit string, tree Tree) (v Version, inserted bool, err error) {
	body, err := json.Marshal(tree)
	if err != nil {
		return Version{}, false, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Version{}, false, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after Commit; on an earlier error the tx is abandoned anyway
	if _, err := tx.ExecContext(ctx, `LOCK TABLE policy_activations IN EXCLUSIVE MODE`); err != nil {
		return Version{}, false, err
	}
	err = tx.QueryRowContext(ctx,
		`INSERT INTO policy_versions (commit_sha, document) VALUES ($1, $2::jsonb)
		 ON CONFLICT (commit_sha) DO NOTHING RETURNING id`, commit, body).Scan(&v.ID)
	inserted = err == nil
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT id FROM policy_versions WHERE commit_sha = $1`, commit).Scan(&v.ID)
	}
	if err != nil {
		return Version{}, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO policy_activations (version_id)
		 SELECT $1 WHERE $1 IS DISTINCT FROM (SELECT version_id FROM policy_activations ORDER BY id DESC LIMIT 1)`,
		v.ID); err != nil {
		return Version{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Version{}, false, err
	}
	v, _, err = Current(ctx, db)
	return v, inserted, err
}

// Current is the version in force, the latest activation; false when none
// was loaded yet.
func Current(ctx context.Context, db *sql.DB) (Version, bool, error) {
	return scanVersion(db.QueryRowContext(ctx,
		`SELECT v.id, v.commit_sha, v.document, v.loaded_at, a.activated_at
		 FROM policy_activations a JOIN policy_versions v ON v.id = a.version_id
		 ORDER BY a.id DESC LIMIT 1`))
}

// VersionByID is version id, the one a task pinned; ActivatedAt is its
// latest activation. false when there is none.
func VersionByID(ctx context.Context, db *sql.DB, id int64) (Version, bool, error) {
	return scanVersion(db.QueryRowContext(ctx,
		`SELECT v.id, v.commit_sha, v.document, v.loaded_at, COALESCE(max(a.activated_at), v.loaded_at)
		 FROM policy_versions v LEFT JOIN policy_activations a ON a.version_id = v.id
		 WHERE v.id = $1 GROUP BY v.id`, id))
}

func scanVersion(row *sql.Row) (Version, bool, error) {
	var v Version
	var body []byte
	err := row.Scan(&v.ID, &v.Commit, &body, &v.LoadedAt, &v.ActivatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, false, nil
	}
	if err != nil {
		return Version{}, false, err
	}
	if err := json.Unmarshal(body, &v.Tree); err != nil {
		return Version{}, false, fmt.Errorf("policy version %d: %w", v.ID, err)
	}
	return v, true, nil
}

// Costs is the ledger between two UTC days, both included.
type Costs struct {
	From  string     `json:"from"`
	To    string     `json:"to"`
	Days  []DayCost  `json:"days"`
	Tasks []TaskCost `json:"tasks"`
}

type DayCost struct {
	Day    string             `json:"day"`
	ByKind map[string]float64 `json:"by_kind"`
	Total  float64            `json:"total"`
}

type TaskCost struct {
	TaskID string     `json:"task_id"`
	EUR    float64    `json:"eur"`
	Lines  []CostLine `json:"lines"`
}

// CostLine is a task's spend on one ledger kind and ref: a model alias, a
// factoryd purpose and its alias (review/review-frontier), a tool, a
// resource.
type CostLine struct {
	Kind string  `json:"kind"`
	Ref  string  `json:"ref"`
	EUR  float64 `json:"eur"`
}

// DateLayout is the ledger's day format.
const DateLayout = "2006-01-02"

// LedgerDaySpend is the ledger's total of one UTC day.
func LedgerDaySpend(ctx context.Context, db *sql.DB, day time.Time) (float64, error) {
	var eur float64
	err := db.QueryRowContext(ctx, `SELECT COALESCE(sum(eur), 0)::float8 FROM ledger WHERE day = $1::date`, day.UTC().Format(DateLayout)).Scan(&eur)
	return eur, err
}

func LedgerCosts(ctx context.Context, db *sql.DB, from, to time.Time) (Costs, error) {
	c := Costs{From: from.Format(DateLayout), To: to.Format(DateLayout), Days: []DayCost{}, Tasks: []TaskCost{}}
	rows, err := db.QueryContext(ctx,
		`SELECT to_char(day, 'YYYY-MM-DD'), kind, sum(eur)::float8 FROM ledger
		 WHERE day BETWEEN $1::date AND $2::date GROUP BY day, kind ORDER BY day, kind`, c.From, c.To)
	if err != nil {
		return Costs{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var day, kind string
		var eur float64
		if err := rows.Scan(&day, &kind, &eur); err != nil {
			return Costs{}, err
		}
		if n := len(c.Days); n == 0 || c.Days[n-1].Day != day {
			c.Days = append(c.Days, DayCost{Day: day, ByKind: map[string]float64{}})
		}
		d := &c.Days[len(c.Days)-1]
		d.ByKind[kind] = eur
		d.Total += eur
	}
	if err := rows.Err(); err != nil {
		return Costs{}, err
	}
	rows, err = db.QueryContext(ctx,
		`SELECT task_id, sum(eur)::float8 FROM ledger WHERE day BETWEEN $1::date AND $2::date
		 GROUP BY task_id ORDER BY 2 DESC, task_id`, c.From, c.To)
	if err != nil {
		return Costs{}, err
	}
	defer rows.Close()
	byTask := map[string]int{}
	for rows.Next() {
		var t TaskCost
		if err := rows.Scan(&t.TaskID, &t.EUR); err != nil {
			return Costs{}, err
		}
		byTask[t.TaskID] = len(c.Tasks)
		c.Tasks = append(c.Tasks, t)
	}
	if err := rows.Err(); err != nil {
		return Costs{}, err
	}
	rows, err = db.QueryContext(ctx,
		`SELECT task_id, kind, ref, sum(eur)::float8 FROM ledger WHERE day BETWEEN $1::date AND $2::date
		 GROUP BY task_id, kind, ref ORDER BY task_id, kind, ref`, c.From, c.To)
	if err != nil {
		return Costs{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var l CostLine
		if err := rows.Scan(&id, &l.Kind, &l.Ref, &l.EUR); err != nil {
			return Costs{}, err
		}
		if i, ok := byTask[id]; ok {
			c.Tasks[i].Lines = append(c.Tasks[i].Lines, l)
		}
	}
	return c, rows.Err()
}

// ModelCall is one model call's ledger row: its ref (the alias, or a
// factoryd purpose and its alias), when it was booked, its EUR and usage.
type ModelCall struct {
	Ref string
	At  time.Time
	EUR float64
	Usage
}

// CallTotals is a task's audit call rows summed (roadmap 206): the tokens
// its calls reported — the stored fact — and, derived at read time, the
// modeled pay-as-you-go cost LiteLLM computed for them and the credits
// the GLM Coding Plan prices the tokens of the deployments that answered
// in. Never money billed; never stored.
type CallTotals struct {
	Prompt, Cached, Output int
	ModeledEUR             float64
	Credits                float64
}

// ModelCalls are task taskID's model calls in the ledger, oldest first.
func ModelCalls(ctx context.Context, db *sql.DB, taskID string) ([]ModelCall, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT ref, created_at, eur::float8, prompt_tokens, cached_tokens, output_tokens, body_bytes FROM ledger
		 WHERE task_id = $1 AND kind = 'model' ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var calls []ModelCall
	for rows.Next() {
		var c ModelCall
		if err := rows.Scan(&c.Ref, &c.At, &c.EUR, &c.PromptTokens, &c.CachedTokens, &c.OutputTokens, &c.BodyBytes); err != nil {
			return nil, err
		}
		calls = append(calls, c)
	}
	return calls, rows.Err()
}
