package policy

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
)

// Version is one loaded policy document, immutable once written.
type Version struct {
	ID       int64     `json:"id"`
	SHA256   string    `json:"sha256"`
	Document Document  `json:"document"`
	LoadedAt time.Time `json:"loaded_at"`
}

func Migrate(ctx context.Context, db *sql.DB) error {
	for _, stmt := range strings.Split(schema, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate policy schema: %w", err)
		}
	}
	return nil
}

// Record stores doc under its file's sha256 unless that content is known
// already, and makes it the current version. inserted is false when the
// content was loaded before (a revert makes the earlier row current again).
func Record(ctx context.Context, db *sql.DB, sha string, doc Document) (v Version, inserted bool, err error) {
	body, err := json.Marshal(doc)
	if err != nil {
		return Version{}, false, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Version{}, false, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after Commit; on an earlier error the tx is abandoned anyway
	err = tx.QueryRowContext(ctx,
		`INSERT INTO policy_versions (sha256, document) VALUES ($1, $2::jsonb)
		 ON CONFLICT (sha256) DO NOTHING RETURNING id`, sha, body).Scan(&v.ID)
	inserted = err == nil
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT id FROM policy_versions WHERE sha256 = $1`, sha).Scan(&v.ID)
	}
	if err != nil {
		return Version{}, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO policy_current (version_id) VALUES ($1)
		 ON CONFLICT (singleton) DO UPDATE SET version_id = EXCLUDED.version_id`, v.ID); err != nil {
		return Version{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Version{}, false, err
	}
	v, _, err = Current(ctx, db)
	return v, inserted, err
}

// Current is the version being enforced; false when none was loaded yet.
func Current(ctx context.Context, db *sql.DB) (Version, bool, error) {
	var v Version
	var body []byte
	err := db.QueryRowContext(ctx,
		`SELECT v.id, v.sha256, v.document, v.loaded_at
		 FROM policy_current c JOIN policy_versions v ON v.id = c.version_id`,
	).Scan(&v.ID, &v.SHA256, &body, &v.LoadedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, false, nil
	}
	if err != nil {
		return Version{}, false, err
	}
	if err := json.Unmarshal(body, &v.Document); err != nil {
		return Version{}, false, fmt.Errorf("policy version %d: %w", v.ID, err)
	}
	return v, true, nil
}

// DayTotal is today's (UTC) ledger total over all tasks.
func DayTotal(ctx context.Context, db *sql.DB) (float64, error) {
	var total float64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(sum(eur), 0)::float8 FROM ledger WHERE day = (now() AT TIME ZONE 'UTC')::date`).Scan(&total)
	return total, err
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
	TaskID string  `json:"task_id"`
	EUR    float64 `json:"eur"`
}

// DateLayout is the ledger's day format.
const DateLayout = "2006-01-02"

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
	for rows.Next() {
		var t TaskCost
		if err := rows.Scan(&t.TaskID, &t.EUR); err != nil {
			return Costs{}, err
		}
		c.Tasks = append(c.Tasks, t)
	}
	return c, rows.Err()
}
