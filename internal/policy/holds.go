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

// MinCallTokens is the smallest output limit worth a call: a floor any
// model needs room to fill, whether it reasons before it answers or answers
// straight away. A call that cannot fit this many output tokens in the
// headroom is refused, unless the caller asked for fewer.
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
	// Bytes is a model call's request body length. Earlier is the task's
	// newest settled call on the same ref with usage, which calibrates the
	// prompt tokens Bytes carries; without one, a token covers at least
	// one byte. CacheFloor is the share of Earlier's prompt the provider
	// serves from its cache at the least, as measured on the ref
	// (cacheFloor); 0 when none is measured.
	Bytes      int
	Earlier    Usage
	CacheFloor float64
	Price      Price
	WantOut    int // the caller's output limit; 0 = none
	DayEUR     float64
	MonthEUR   float64
	// Project names the task's project and ProjectEUR its month cap, its
	// file's budget.month_eur (Tree.ProjectCap): +Inf for none.
	Project    string
	ProjectEUR float64
	// EUR is a tool call's worst case, held as is, and Cap the task's caps
	// on the tool.
	EUR float64
	Cap ToolCap
}

// Hold is headroom taken for one call: EUR at the request's prompt as Fit
// prices it and MaxOut output tokens. Headroom is the smallest of the
// task's, the day's, the month's and the project's month headroom before
// any hold, the figure a key's max_budget takes; Bound names which one it
// is, as the refusal it would cause (refused_budget, refused_day_cap,
// refused_month_cap or refused_project_cap).
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

// HeadroomBound is the smallest of the task's, the day's, the month's and
// the project's month headroom and the refusal it causes when spent.
func HeadroomBound(task, day, month, project float64) (float64, string) {
	switch {
	case project < min(task, day, month):
		return project, RefusedProjectCap
	case month < min(task, day):
		return month, RefusedMonthCap
	case day < task:
		return day, RefusedDayCap
	}
	return task, "refused_budget"
}

// RefusedDayCap is the refusal of a call the UTC day's headroom cannot
// fit.
const RefusedDayCap = "refused_day_cap"

// RefusedMonthCap is the refusal of a call the month's headroom cannot fit.
const RefusedMonthCap = "refused_month_cap"

// RefusedProjectCap is the refusal of a call the project's month headroom
// cannot fit.
const RefusedProjectCap = "refused_project_cap"

// Refusal is a call the headroom or a tool cap cannot fit.
type Refusal struct {
	Decision string // refused_budget, refused_day_cap, refused_month_cap, refused_project_cap, refused_tool_requests, refused_tool_eur or refused_task_status
	Reason   string
}

// refusal is the headroom refusal decision with reason; the month cap's
// and project's cap's say what it refused and name the key a human raises,
// the project's in its file. Only a model call finds a cap spent: a
// reservation or a tool call's worst case may exceed what is left while
// smaller calls still fit.
func refusal(decision, kind, project, reason string) *Refusal {
	headroom, limit := "the month's remaining headroom (budget.month_eur)", "the month cap budget.month_eur in policy.yaml"
	if decision == RefusedProjectCap {
		file := "projects/" + project + ".yaml"
		headroom, limit = "the project's remaining month headroom (budget.month_eur in "+file+")", "the project cap budget.month_eur in "+file
	}
	if decision == RefusedMonthCap || decision == RefusedProjectCap {
		switch kind {
		case KindResource:
			reason += "; the reservation exceeds " + headroom
		case KindTool:
			reason += "; the tool call's worst case exceeds " + headroom
		default:
			reason += "; " + limit + " is spent"
		}
		reason += ", and only a human raises it, by a commit"
	}
	return &Refusal{Decision: decision, Reason: reason}
}

func (r *Refusal) Error() string { return r.Reason }

// Usage is a model call's prompt, cached prompt and output tokens as the
// provider reported them, and its request body's bytes.
type Usage struct {
	PromptTokens, CachedTokens, OutputTokens, BodyBytes int
}

// Fit clamps a call's output limit to what the free task, day, month and
// project headroom can pay for, or refuses it. Calibrated by an earlier
// call (ruling 274), the prompt is about Bytes times that call's tokens per
// byte. Of the part up to that call's prompt tokens, the share the
// provider's measured floor vouches for (CacheFloor) is at the cache-read
// price; the rest and a margin of a quarter are at the input price. No
// provider promise is trusted: without a measured floor the whole prompt
// is at the input price. Without an earlier call it is at most Bytes
// tokens at the input price.
func Fit(f Free, r HoldRequest) (Hold, error) {
	free, decision := HeadroomBound(f.Task, f.Day, f.Month, f.Project)
	tokens, measure := float64(r.Bytes), "at most"
	inEUR := tokens * r.Price.In
	if e := r.Earlier; e.PromptTokens > 0 && e.BodyBytes > 0 {
		tokens, measure = float64(r.Bytes)*float64(e.PromptTokens)/float64(e.BodyBytes), "about"
		cached := min(tokens, float64(e.PromptTokens)) * min(r.CacheFloor, 1)
		inEUR = cached*r.Price.CacheRead + (tokens-cached+tokens/4)*r.Price.In
	}
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
		return Hold{}, refusal(decision, KindModel, r.Project, fmt.Sprintf(
			"free headroom %.6f EUR (%s) fits %d output tokens after a request of %d bytes (%s %.0f prompt tokens), below the minimum call of %d",
			free, f.figures(), max(outFit, 0), r.Bytes, measure, tokens, MinCallTokens))
	}
	return Hold{TaskID: r.TaskID, Kind: r.Kind, Ref: r.Ref, MaxOut: maxOut,
		EUR: inEUR + float64(maxOut)*r.Price.Out}, nil
}

// FitTool admits a tool call at its worst case if the task has requests
// and euros left on the tool (used so far: requests, usedEUR, holds
// included) and the free headroom pays for it.
func FitTool(f Free, requests int, usedEUR float64, r HoldRequest) (Hold, error) {
	if requests >= r.Cap.MaxRequests {
		return Hold{}, &Refusal{Decision: "refused_tool_requests", Reason: fmt.Sprintf(
			"%s: %d of the task's %d requests used", r.Ref, requests, r.Cap.MaxRequests)}
	}
	if usedEUR+r.EUR > r.Cap.MaxEUR+1e-12 {
		return Hold{}, &Refusal{Decision: "refused_tool_eur", Reason: fmt.Sprintf(
			"%s: %.6f EUR used and a worst case of %.6f EUR exceed the task's cap of %.6f EUR on it", r.Ref, usedEUR, r.EUR, r.Cap.MaxEUR)}
	}
	return FitResource(f, r)
}

// FitResource admits r.EUR as is if the free task, day, month and project
// headroom pay for it: a tool call's worst case, or a commissioned
// resource's reservation.
func FitResource(f Free, r HoldRequest) (Hold, error) {
	free, decision := HeadroomBound(f.Task, f.Day, f.Month, f.Project)
	if r.EUR > free {
		return Hold{}, refusal(decision, r.Kind, r.Project, fmt.Sprintf(
			"%s: worst case %.6f EUR exceeds the free headroom %.6f EUR (%s)", r.Ref, r.EUR, free, f.figures()))
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
	free, err := FreeHeadroom(ctx, tx, r)
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
		h, err = FitTool(free, requests, usedEUR, r)
	case KindResource:
		h, err = FitResource(free, r)
	default:
		err = tx.QueryRowContext(ctx,
			`SELECT prompt_tokens, body_bytes FROM ledger WHERE task_id = $1 AND kind = 'model' AND ref = $2 AND prompt_tokens > 0 AND body_bytes > 0
			 ORDER BY id DESC LIMIT 1`, r.TaskID, r.Ref).Scan(&r.Earlier.PromptTokens, &r.Earlier.BodyBytes)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Hold{}, err
		}
		if r.CacheFloor, err = cacheFloor(ctx, tx, r.Ref); err != nil {
			return Hold{}, err
		}
		h, err = Fit(free, r)
	}
	if err != nil {
		return Hold{}, err
	}
	h.Headroom, h.Bound = HeadroomBound(free.TaskHeadroom, free.DayHeadroom, free.MonthHeadroom, free.ProjectHeadroom)
	if h.ID, err = InsertHold(ctx, tx, h, HoldTTL); err != nil {
		return Hold{}, err
	}
	return h, tx.Commit()
}

// cacheFloorCalls is how many of a ref's newest settled calls measure its
// cache floor.
const cacheFloorCalls = 100

// cacheFloor is the least share of its earlier call's prompt (the prefix
// Fit prices) the provider served from its cache, over those of the newest
// cacheFloorCalls settled calls on ref that followed an earlier call of
// their task on ref, Earlier as TakeHold reads it; 0 when none did.
func cacheFloor(ctx context.Context, tx *sql.Tx, ref string) (float64, error) {
	var floor float64
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(min(c.cached_tokens::float8 / LEAST(c.prompt_tokens, e.prompt_tokens)), 0)::float8
		 FROM (SELECT id, task_id, prompt_tokens, cached_tokens FROM ledger
		       WHERE kind = 'model' AND ref = $1 AND prompt_tokens > 0 AND body_bytes > 0
		       ORDER BY id DESC LIMIT $2) c
		 CROSS JOIN LATERAL (SELECT prompt_tokens FROM ledger
		       WHERE task_id = c.task_id AND kind = 'model' AND ref = $1 AND prompt_tokens > 0 AND body_bytes > 0 AND id < c.id
		       ORDER BY id DESC LIMIT 1) e`, ref, cacheFloorCalls).Scan(&floor)
	return floor, err
}

// LockHeadroom serializes everything that takes headroom, until tx ends.
func LockHeadroom(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('factory-headroom'))`)
	return err
}

// Free is a task's, the day's, the month's and the task's project's month
// headroom: before any hold, and free (after every outstanding hold). The
// project's is +Inf for a project without a cap.
type Free struct {
	TaskHeadroom, DayHeadroom, MonthHeadroom, ProjectHeadroom float64
	Task, Day, Month, Project                                 float64
}

// figures are the free headroom a refusal quotes; the project's only under
// a cap.
func (f Free) figures() string {
	s := fmt.Sprintf("task %.6f, day %.6f, month %.6f", f.Task, f.Day, f.Month)
	if !math.IsInf(f.Project, 1) {
		s += fmt.Sprintf(", project %.6f", f.Project)
	}
	return s
}

// MonthSpentSQL is the spend over the ledger in the UTC calendar month of
// at, a timestamptz expression; a standing line is its project's monthly
// cost, not variable spend.
func MonthSpentSQL(at string) string {
	return `COALESCE((SELECT sum(eur) FROM ledger WHERE day >= date_trunc('month', ` + at + ` AT TIME ZONE 'UTC')::date AND kind <> 'standing'), 0)`
}

// now is the clock of the day's and the month's headroom.
var now = time.Now

// FreeHeadroom reads r's task's, the UTC day's, the UTC calendar month's
// and the task's project's month headroom inside tx (under LockHeadroom).
// The project's is r.ProjectEUR less the month's ledger rows and the
// outstanding holds of every task of the task's repository, read only
// under a cap (+Inf otherwise). Every outstanding hold counts against the
// day and the month. A task that is neither running nor in review is
// refused: settlement relies on a finished task taking no new hold.
func FreeHeadroom(ctx context.Context, tx *sql.Tx, r HoldRequest) (Free, error) {
	taskID, at := r.TaskID, now()
	t, err := ReadTaskFree(ctx, tx, taskID)
	if err != nil {
		return Free{}, err
	}
	if !slices.Contains(holdStatuses, t.Status) {
		return Free{}, &Refusal{Decision: "refused_task_status", Reason: fmt.Sprintf("task %s is %s; only a running or in-review task takes a hold", taskID, t.Status)}
	}
	var f Free
	var allHeld float64
	if err := tx.QueryRowContext(ctx,
		`SELECT ($1 - COALESCE((SELECT sum(eur) FROM ledger WHERE day = ($3::timestamptz AT TIME ZONE 'UTC')::date AND kind <> 'standing'), 0))::float8,
		        ($2 - `+MonthSpentSQL("$3::timestamptz")+`)::float8,
		        COALESCE((SELECT sum(eur) FROM holds), 0)::float8`, r.DayEUR, r.MonthEUR, at,
	).Scan(&f.DayHeadroom, &f.MonthHeadroom, &allHeld); err != nil {
		return Free{}, err
	}
	f.TaskHeadroom, f.Task = t.Headroom, t.Free()
	f.Day, f.Month = f.DayHeadroom-allHeld, f.MonthHeadroom-allHeld
	f.ProjectHeadroom, f.Project = math.Inf(1), math.Inf(1)
	if math.IsInf(r.ProjectEUR, 1) {
		return f, nil
	}
	// a standing line's task id is standing:<repository>: it joins no task
	var spent, held float64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT sum(l.eur) FROM ledger l JOIN tasks p ON p.id = l.task_id
		                  WHERE p.repository = t.repository AND l.day >= date_trunc('month', $2::timestamptz AT TIME ZONE 'UTC')::date), 0)::float8,
		        COALESCE((SELECT sum(h.eur) FROM holds h JOIN tasks p ON p.id = h.task_id WHERE p.repository = t.repository), 0)::float8
		 FROM tasks t WHERE t.id = $1`, taskID, at).Scan(&spent, &held); err != nil {
		return Free{}, err
	}
	f.ProjectHeadroom = r.ProjectEUR - spent
	f.Project = f.ProjectHeadroom - held
	return f, nil
}

// TaskFree is a task's own headroom: its budget less its spend before any
// hold (Headroom), and its outstanding holds, Held EUR in Holds of them,
// the earliest expiring at Expires (zero without one). Status is the
// task's, which decides whether it may take a hold.
type TaskFree struct {
	Status   string
	Headroom float64
	Held     float64
	Holds    int
	Expires  time.Time
}

// Free is what the task's next call fits into: its headroom after every
// outstanding hold.
func (t TaskFree) Free() float64 { return t.Headroom - t.Held }

// ReadTaskFree reads taskID's own headroom and outstanding holds in one
// statement: FreeHeadroom's task figures inside its transaction, and the
// attempt floor's (factoryd's AttemptFunded) on the pool.
func ReadTaskFree(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, taskID string) (TaskFree, error) {
	var t TaskFree
	var expires sql.NullTime
	err := q.QueryRowContext(ctx,
		`SELECT status, (budget_eur - spent_eur)::float8, h.eur, h.n, h.expires
		 FROM tasks, (SELECT COALESCE(sum(eur), 0)::float8 AS eur, count(*) AS n, min(expires_at) AS expires FROM holds WHERE task_id = $1) h
		 WHERE id = $1`, taskID).Scan(&t.Status, &t.Headroom, &t.Held, &t.Holds, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskFree{}, fmt.Errorf("hold: no task row for %s", taskID)
	}
	t.Expires = expires.Time
	return t, err
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
// hold's full amount (a worst case, and settlement only ever raises), on a
// ledger row with the call's usage (a model call's; zero else). A
// cost that is not a finite non-negative number counts as unknown. It books
// the task's spent_eur and the ledger row in the transaction that deletes
// the hold, and returns the task's spend. A tool call's row is written even
// at 0 EUR: the rows count the task's requests of the tool. A hold the
// sweep already booked in full books only what the known cost exceeds it
// by. A known cost above the hold is booked in full and flagged (OverHold).
func ReleaseHold(ctx context.Context, db *sql.DB, h Hold, cost *float64, u Usage) (Booking, error) {
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur, prompt_tokens, cached_tokens, output_tokens, body_bytes)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, h.TaskID, h.Kind, h.Ref, amount, u.PromptTokens, u.CachedTokens, u.OutputTokens, u.BodyBytes); err != nil {
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

// SweepHolds books every expired model and tool hold at its full amount,
// writes each one's audit row (decision swept: the call's caller, cut by a
// restart, never wrote one) and returns how many it booked. The row's kind
// and alias come from the ref: purpose/alias for a factoryd call, the alias
// for an agent's, the tool's name for a tool call. A resource reservation
// is never swept: accrual books its resource's hours and drops it, so a
// sweep would book the same hours twice.
func SweepHolds(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`WITH gone AS (DELETE FROM holds WHERE expires_at < now() AND kind <> 'resource' RETURNING id, task_id, kind, ref, eur),
		      t AS (UPDATE tasks SET spent_eur = spent_eur + g.eur
		            FROM (SELECT task_id, sum(eur) AS eur FROM gone GROUP BY task_id) g WHERE tasks.id = g.task_id),
		      l AS (INSERT INTO ledger (task_id, kind, ref, eur) SELECT task_id, kind, ref, eur FROM gone WHERE eur > 0),
		      a AS (INSERT INTO audit (task_id, decision, reason, kind, model_alias, tool, spend_eur, hold_eur)
		            SELECT task_id, 'swept', 'hold ' || id || ' expired unreleased; booked at its full amount',
		                   CASE WHEN kind = 'model' AND strpos(ref, '/') > 0 THEN split_part(ref, '/', 1) ELSE 'agent' END,
		                   CASE WHEN kind = 'model' THEN substr(ref, strpos(ref, '/') + 1) ELSE '' END,
		                   CASE WHEN kind = 'tool' THEN ref ELSE '' END,
		                   eur, eur FROM gone)
		 SELECT count(*) FROM gone`).Scan(&n)
	return n, err
}
