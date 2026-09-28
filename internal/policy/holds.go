package policy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"slices"
	"time"
)

// MinCallTokens is the smallest output limit worth a call: GLM reasons
// 200+ tokens before any answer. A call that cannot fit this many output
// tokens in the headroom is refused, unless the caller asked for fewer.
const MinCallTokens = 1024

// holdStatuses are the task statuses that spend: running (the agent, its
// supervisor) and in-review (the gates' model review).
var holdStatuses = []string{"running", "in-review"}

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
	Decision string // refused_budget, refused_day_cap, refused_tool_requests, refused_tool_eur or refused_task_status
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
	return FitResource(taskFree, dayFree, r)
}

// FitResource admits r.EUR as is if the free task and day headroom pay for
// it: a tool call's worst case, or a commissioned resource's reservation.
func FitResource(taskFree, dayFree float64, r HoldRequest) (Hold, error) {
	free, decision := HeadroomBound(taskFree, dayFree)
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
	if err := LockHeadroom(ctx, tx); err != nil {
		return Hold{}, err
	}
	free, err := FreeHeadroom(ctx, tx, r.TaskID, r.DayEUR)
	if err != nil {
		return Hold{}, err
	}
	var h Hold
	switch r.Kind {
	case KindTool:
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
		h, err = FitTool(free.Task, free.Day, requests, usedEUR, r)
	case KindResource:
		h, err = FitResource(free.Task, free.Day, r)
	default:
		h, err = Fit(free.Task, free.Day, r)
	}
	if err != nil {
		return Hold{}, err
	}
	h.Headroom, h.Bound = HeadroomBound(free.TaskHeadroom, free.DayHeadroom)
	if h.ID, err = InsertHold(ctx, tx, h, HoldTTL); err != nil {
		return Hold{}, err
	}
	return h, tx.Commit()
}

// LockHeadroom serializes everything that takes headroom, until tx ends.
func LockHeadroom(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('factory-headroom'))`)
	return err
}

// Free is a task's and the day's headroom: before any hold, and free
// (after every outstanding hold).
type Free struct {
	TaskHeadroom, DayHeadroom float64
	Task, Day                 float64
}

// FreeHeadroom reads the task's and the UTC day's headroom inside tx
// (under LockHeadroom). A task that is neither running nor in review is
// refused: settlement relies on a finished task taking no new hold.
func FreeHeadroom(ctx context.Context, tx *sql.Tx, taskID string, dayEUR float64) (Free, error) {
	var f Free
	var status string
	var taskHeld, dayHeld float64
	err := tx.QueryRowContext(ctx,
		`SELECT status, (budget_eur - spent_eur)::float8,
		        COALESCE((SELECT sum(eur) FROM holds WHERE task_id = $1), 0)::float8,
		        ($2 - COALESCE((SELECT sum(eur) FROM ledger WHERE day = (now() AT TIME ZONE 'UTC')::date), 0))::float8,
		        COALESCE((SELECT sum(eur) FROM holds), 0)::float8
		 FROM tasks WHERE id = $1`, taskID, dayEUR,
	).Scan(&status, &f.TaskHeadroom, &taskHeld, &f.DayHeadroom, &dayHeld)
	if errors.Is(err, sql.ErrNoRows) {
		return Free{}, fmt.Errorf("hold: no task row for %s", taskID)
	}
	if err != nil {
		return Free{}, err
	}
	if !slices.Contains(holdStatuses, status) {
		return Free{}, &Refusal{Decision: "refused_task_status", Reason: fmt.Sprintf("task %s is %s; only a running or in-review task takes a hold", taskID, status)}
	}
	f.Task, f.Day = f.TaskHeadroom-taskHeld, f.DayHeadroom-dayHeld
	return f, nil
}

// InsertHold records h inside tx, expiring after ttl, and returns its id.
func InsertHold(ctx context.Context, tx *sql.Tx, h Hold, ttl time.Duration) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`INSERT INTO holds (task_id, kind, ref, eur, expires_at) VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))
		 RETURNING id`, h.TaskID, h.Kind, h.Ref, h.EUR, ttl.Seconds()).Scan(&id)
	return id, err
}

// Booking is what ReleaseHold booked: the task's spend after it, and the
// COST OVER HOLD flag when the known cost exceeded the hold ("" within it).
type Booking struct {
	Spent float64
	// Booked is what this release added to the task's spend.
	Booked   float64
	OverHold string
}

// ReleaseHold turns a hold into spend: the call's cost when known, else the
// hold's full amount (a worst case, and settlement only ever raises). A
// cost that is not a finite non-negative number counts as unknown. It books
// the task's spent_eur and the ledger row in the transaction that deletes
// the hold, and returns the task's spend. A tool call's row is written even
// at 0 EUR: the rows count the task's requests of the tool. A hold the
// sweep already booked in full books only what the known cost exceeds it
// by. A known cost above the hold is booked in full and flagged (OverHold).
func ReleaseHold(ctx context.Context, db *sql.DB, h Hold, cost *float64) (Booking, error) {
	cost = KnownCost(cost)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Booking{}, err
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
		return Booking{}, err
	}
	var b Booking
	if err := tx.QueryRowContext(ctx,
		`UPDATE tasks SET spent_eur = spent_eur + $2 WHERE id = $1 RETURNING spent_eur::float8`, h.TaskID, amount,
	).Scan(&b.Spent); err != nil {
		return Booking{}, fmt.Errorf("release hold %d: task %s: %w", h.ID, h.TaskID, err)
	}
	if amount > 0 || found && h.Kind == KindTool {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur) VALUES ($1, $2, $3, $4)`,
			h.TaskID, h.Kind, h.Ref, amount); err != nil {
			return Booking{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Booking{}, err
	}
	b.Booked = amount
	if cost != nil {
		b.OverHold = OverHold(h.TaskID, h.Kind+" "+h.Ref, *cost, h.EUR)
	}
	return b, nil
}

// OverHold flags a booking of eur beyond the held amount, every path's one
// flag (ruling 117): it logs the COST OVER HOLD line and returns it, "" when
// eur is within held.
func OverHold(taskID, what string, eur, held float64) string {
	if eur <= held+1e-12 {
		return ""
	}
	reason := fmt.Sprintf("COST OVER HOLD: %s cost %.6f EUR against a hold of %.6f EUR; booked in full", what, eur, held)
	log.Printf("task %s: %s", taskID, reason) // #nosec G706 -- the task id is a stored row's; what is a kind and a policy alias, tool or resource id
	return reason
}

// KnownCost is cost if it is a finite non-negative number, else nil
// (unknown).
func KnownCost(cost *float64) *float64 {
	if cost == nil || math.IsNaN(*cost) || math.IsInf(*cost, 0) || *cost < 0 {
		return nil
	}
	return cost
}

// SweepHolds books every expired model and tool hold at its full amount
// and returns how many it booked. A resource reservation is never swept:
// accrual books its resource's hours and drops it, so a sweep would book
// the same hours twice.
func SweepHolds(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`WITH gone AS (DELETE FROM holds WHERE expires_at < now() AND kind <> 'resource' RETURNING task_id, kind, ref, eur),
		      t AS (UPDATE tasks SET spent_eur = spent_eur + g.eur
		            FROM (SELECT task_id, sum(eur) AS eur FROM gone GROUP BY task_id) g WHERE tasks.id = g.task_id),
		      l AS (INSERT INTO ledger (task_id, kind, ref, eur) SELECT task_id, kind, ref, eur FROM gone WHERE eur > 0)
		 SELECT count(*) FROM gone`).Scan(&n)
	return n, err
}
