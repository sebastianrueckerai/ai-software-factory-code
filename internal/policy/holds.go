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

// HoldRequest asks for the headroom of one model or tool call.
type HoldRequest struct {
	TaskID string
	Kind   string // KindModel, KindTool or KindResource
	Ref    string // the alias of a model call, the name of a tool call
	// InTokens bounds the input tokens: the request body's byte length,
	// since a token covers at least one byte.
	InTokens int
	Price    Price
	WantOut  int // the caller's output limit; 0 = none
	DayEUR   float64
	// EUR is a tool call's worst case, held as is, and Cap the task's caps
	// on the tool.
	EUR float64
	Cap ToolCap
}

// Hold is headroom taken for one call: EUR at the worst case of InTokens
// and MaxOut output tokens. Headroom is the smaller of the task's and the
// day's headroom before any hold, the figure a key's max_budget takes;
// Bound names which one it is, as the refusal it would cause
// (refused_budget or refused_day_cap).
type Hold struct {
	ID       int64
	TaskID   string
	Kind     string
	Ref      string
	EUR      float64
	MaxOut   int
	Headroom float64
	Bound    string
}

// HeadroomBound is the smaller of the task's and the day's headroom and the
// refusal it causes when spent.
func HeadroomBound(task, day float64) (float64, string) {
	if day < task {
		return day, "refused_day_cap"
	}
	return task, "refused_budget"
}

// Refusal is a call the headroom or a tool cap cannot fit.
type Refusal struct {
	Decision string // refused_budget, refused_day_cap, refused_tool_requests or refused_tool_eur
	Reason   string
}

func (r *Refusal) Error() string { return r.Reason }

// Fit clamps a call's output limit to what the free task and day headroom
// can pay for, or refuses it.
func Fit(taskFree, dayFree float64, r HoldRequest) (Hold, error) {
	free, decision := HeadroomBound(taskFree, dayFree)
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

// FitTool admits a tool call at its worst case if the task has requests
// and euros left on the tool (used so far: requests, usedEUR, holds
// included) and the free task and day headroom pay for it.
func FitTool(taskFree, dayFree float64, requests int, usedEUR float64, r HoldRequest) (Hold, error) {
	if requests >= r.Cap.MaxRequests {
		return Hold{}, &Refusal{Decision: "refused_tool_requests", Reason: fmt.Sprintf(
			"%s: %d of the task's %d requests used", r.Ref, requests, r.Cap.MaxRequests)}
	}
	if usedEUR+r.EUR > r.Cap.MaxEUR+1e-12 {
		return Hold{}, &Refusal{Decision: "refused_tool_eur", Reason: fmt.Sprintf(
			"%s: %.6f EUR used and a worst case of %.6f EUR exceed the task's cap of %.6f EUR on it", r.Ref, usedEUR, r.EUR, r.Cap.MaxEUR)}
	}
	free, decision := taskFree, "refused_budget"
	if dayFree < taskFree {
		free, decision = dayFree, "refused_day_cap"
	}
	if r.EUR > free {
		return Hold{}, &Refusal{Decision: decision, Reason: fmt.Sprintf(
			"%s: worst case %.6f EUR exceeds the free headroom %.6f EUR (task %.6f, day %.6f)", r.Ref, r.EUR, free, taskFree, dayFree)}
	}
	return Hold{TaskID: r.TaskID, Kind: r.Kind, Ref: r.Ref, EUR: r.EUR}, nil
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
	var h Hold
	if r.Kind == KindTool {
		var requests int
		var usedEUR float64
		if err := tx.QueryRowContext(ctx,
			`SELECT (SELECT count(*) FROM ledger WHERE task_id = $1 AND kind = 'tool' AND ref = $2)
			      + (SELECT count(*) FROM holds WHERE task_id = $1 AND kind = 'tool' AND ref = $2),
			        (COALESCE((SELECT sum(eur) FROM ledger WHERE task_id = $1 AND kind = 'tool' AND ref = $2), 0)
			      + COALESCE((SELECT sum(eur) FROM holds WHERE task_id = $1 AND kind = 'tool' AND ref = $2), 0))::float8`,
			r.TaskID, r.Ref).Scan(&requests, &usedEUR); err != nil {
			return Hold{}, err
		}
		h, err = FitTool(taskHeadroom-taskHeld, dayHeadroom-dayHeld, requests, usedEUR, r)
	} else {
		h, err = Fit(taskHeadroom-taskHeld, dayHeadroom-dayHeld, r)
	}
	if err != nil {
		return Hold{}, err
	}
	h.Headroom, h.Bound = HeadroomBound(taskHeadroom, dayHeadroom)
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO holds (task_id, kind, ref, eur, expires_at) VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))
		 RETURNING id`, h.TaskID, h.Kind, h.Ref, h.EUR, HoldTTL.Seconds()).Scan(&h.ID); err != nil {
		return Hold{}, err
	}
	return h, tx.Commit()
}

// ReleaseHold turns a hold into spend: the call's cost when known, else the
// hold's full amount (a worst case, and settlement only ever raises). A
// cost that is not a finite non-negative number counts as unknown. It books
// the task's spent_eur and the ledger row in the transaction that deletes
// the hold, and returns the task's spend. A tool call's row is written even
// at 0 EUR: the rows count the task's requests of the tool. A hold the
// sweep already booked in full books only what the known cost exceeds it
// by.
func ReleaseHold(ctx context.Context, db *sql.DB, h Hold, cost *float64) (float64, error) {
	cost = KnownCost(cost)
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
	found := err == nil
	if errors.Is(err, sql.ErrNoRows) {
		amount = 0
		if cost != nil {
			amount = max(0, *cost-h.EUR)
		}
	} else if err != nil {
		return 0, err
	}
	var spent float64
	if err := tx.QueryRowContext(ctx,
		`UPDATE tasks SET spent_eur = spent_eur + $2 WHERE id = $1 RETURNING spent_eur::float8`, h.TaskID, amount,
	).Scan(&spent); err != nil {
		return 0, fmt.Errorf("release hold %d: task %s: %w", h.ID, h.TaskID, err)
	}
	if amount > 0 || found && h.Kind == KindTool {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur) VALUES ($1, $2, $3, $4)`,
			h.TaskID, h.Kind, h.Ref, amount); err != nil {
			return 0, err
		}
	}
	return spent, tx.Commit()
}

// KnownCost is cost if it is a finite non-negative number, else nil
// (unknown).
func KnownCost(cost *float64) *float64 {
	if cost == nil || math.IsNaN(*cost) || math.IsInf(*cost, 0) || *cost < 0 {
		return nil
	}
	return cost
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
