package policy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

// MinCallTokens is the smallest output limit worth a call: GLM reasons
// 200+ tokens before any answer. A call that cannot fit this many output
// tokens in the headroom is refused, unless the caller asked for fewer.
const MinCallTokens = 1024

// HoldTTL bounds a hold a crashed caller never released; SweepHolds books
// an expired hold at its full amount.
const HoldTTL = time.Hour

// HoldRequest asks for the headroom of one model call.
type HoldRequest struct {
	TaskID string
	Kind   string // KindModel, KindTool or KindResource
	Ref    string // the alias for a model call
	// InTokens bounds the input tokens: the request body's byte length,
	// since a token covers at least one byte.
	InTokens int
	Price    Price
	WantOut  int // the caller's output limit; 0 = none
	DayEUR   float64
}

// Hold is headroom taken for one call: EUR at the worst case of InTokens
// and MaxOut output tokens. Headroom is the smaller of the task's and the
// day's headroom before any hold, the figure a key's max_budget takes.
type Hold struct {
	ID       int64
	TaskID   string
	Kind     string
	Ref      string
	EUR      float64
	MaxOut   int
	Headroom float64
}

// Refusal is a call the headroom cannot fit.
type Refusal struct {
	Decision string // refused_budget or refused_day_cap
	Reason   string
}

func (r *Refusal) Error() string { return r.Reason }

// Fit clamps a call's output limit to what the free task and day headroom
// can pay for, or refuses it.
func Fit(taskFree, dayFree float64, r HoldRequest) (Hold, error) {
	free, decision := taskFree, "refused_budget"
	if dayFree < taskFree {
		free, decision = dayFree, "refused_day_cap"
	}
	inEUR := float64(r.InTokens) * r.Price.In
	outFit := 0
	if left := free - inEUR; left > 0 {
		// the epsilon keeps an exact quotient (0.009/4e-6) from flooring low
		outFit = int(math.Min(math.Floor(left/r.Price.Out+1e-9), math.MaxInt32))
	}
	maxOut := outFit
	if r.Price.MaxOutput > 0 {
		maxOut = min(maxOut, r.Price.MaxOutput)
	}
	if r.WantOut > 0 {
		maxOut = min(maxOut, r.WantOut)
	}
	if maxOut <= 0 || maxOut < MinCallTokens && maxOut != r.WantOut {
		return Hold{}, &Refusal{Decision: decision, Reason: fmt.Sprintf(
			"free headroom %.6f EUR (task %.6f, day %.6f) fits %d output tokens after at most %d input tokens, below the minimum call of %d",
			free, taskFree, dayFree, max(outFit, 0), r.InTokens, MinCallTokens)}
	}
	return Hold{TaskID: r.TaskID, Kind: r.Kind, Ref: r.Ref, MaxOut: maxOut,
		EUR: inEUR + float64(maxOut)*r.Price.Out}, nil
}

// TakeHold fits the call into the headroom left after spend and every
// outstanding hold, and records the hold, in one transaction under one
// advisory lock: two calls never both take the last headroom.
func TakeHold(ctx context.Context, db *sql.DB, r HoldRequest) (Hold, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Hold{}, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after Commit; on an earlier error the tx is abandoned anyway
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('factory-headroom'))`); err != nil {
		return Hold{}, err
	}
	var taskHeadroom, taskHeld, dayHeadroom, dayHeld float64
	err = tx.QueryRowContext(ctx,
		`SELECT (budget_eur - spent_eur)::float8,
		        COALESCE((SELECT sum(eur) FROM holds WHERE task_id = $1), 0)::float8,
		        ($2 - COALESCE((SELECT sum(eur) FROM ledger WHERE day = (now() AT TIME ZONE 'UTC')::date), 0))::float8,
		        COALESCE((SELECT sum(eur) FROM holds), 0)::float8
		 FROM tasks WHERE id = $1`, r.TaskID, r.DayEUR,
	).Scan(&taskHeadroom, &taskHeld, &dayHeadroom, &dayHeld)
	if errors.Is(err, sql.ErrNoRows) {
		return Hold{}, fmt.Errorf("hold: no task row for %s", r.TaskID)
	}
	if err != nil {
		return Hold{}, err
	}
	h, err := Fit(taskHeadroom-taskHeld, dayHeadroom-dayHeld, r)
	if err != nil {
		return Hold{}, err
	}
	h.Headroom = min(taskHeadroom, dayHeadroom)
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO holds (task_id, kind, ref, eur, expires_at) VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))
		 RETURNING id`, h.TaskID, h.Kind, h.Ref, h.EUR, HoldTTL.Seconds()).Scan(&h.ID); err != nil {
		return Hold{}, err
	}
	return h, tx.Commit()
}

// ReleaseHold turns a hold into spend: the call's cost when known, else the
// hold's full amount (a worst case, and settlement only ever raises). It
// books the task's spent_eur and the ledger row in the transaction that
// deletes the hold, and returns the task's spend. A hold the sweep already
// booked is not booked twice.
func ReleaseHold(ctx context.Context, db *sql.DB, h Hold, cost *float64) (float64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after Commit; on an earlier error the tx is abandoned anyway
	var held float64
	err = tx.QueryRowContext(ctx, `DELETE FROM holds WHERE id = $1 RETURNING eur::float8`, h.ID).Scan(&held)
	amount := held
	if cost != nil {
		amount = *cost
	}
	if errors.Is(err, sql.ErrNoRows) {
		amount = 0
	} else if err != nil {
		return 0, err
	}
	var spent float64
	if err := tx.QueryRowContext(ctx,
		`UPDATE tasks SET spent_eur = spent_eur + $2 WHERE id = $1 RETURNING spent_eur::float8`, h.TaskID, amount,
	).Scan(&spent); err != nil {
		return 0, fmt.Errorf("release hold %d: task %s: %w", h.ID, h.TaskID, err)
	}
	if amount > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur) VALUES ($1, $2, $3, $4)`,
			h.TaskID, h.Kind, h.Ref, amount); err != nil {
			return 0, err
		}
	}
	return spent, tx.Commit()
}

// SweepHolds books every expired hold at its full amount and returns how
// many it booked.
func SweepHolds(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`WITH gone AS (DELETE FROM holds WHERE expires_at < now() RETURNING task_id, kind, ref, eur),
		      t AS (UPDATE tasks SET spent_eur = spent_eur + g.eur
		            FROM (SELECT task_id, sum(eur) AS eur FROM gone GROUP BY task_id) g WHERE tasks.id = g.task_id),
		      l AS (INSERT INTO ledger (task_id, kind, ref, eur) SELECT task_id, kind, ref, eur FROM gone WHERE eur > 0)
		 SELECT count(*) FROM gone`).Scan(&n)
	return n, err
}
