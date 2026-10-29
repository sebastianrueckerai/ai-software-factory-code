package policy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"slices"
	"strings"
	"time"
)

// MinCallTokens is the smallest output limit worth a call: a floor any
// model needs room to fill, whether it reasons before it answers or answers
// straight away. A call that cannot fit this many output tokens in the
// headroom is refused, unless the caller asked for fewer.
const MinCallTokens = 1024

// holdStatuses are the task statuses that spend: running (the agent, its
// supervisor) and in-review (the gates' model review). A task waiting on
// a person takes one hold besides: factoryd's call that writes its brief.
var holdStatuses = []string{"running", "in-review"}

// LedgerRef is a model call's ref on its hold and ledger row: the alias of
// an agent's call, purpose/alias of factoryd's.
func LedgerRef(purpose, alias string) string {
	if purpose == "" {
		return alias
	}
	return purpose + "/" + alias
}

// HoldTTL bounds a hold a crashed caller never released; SweepHolds books
// an expired hold at its full amount.
const HoldTTL = time.Hour

// HoldRequest asks for the headroom of one model or tool call.
type HoldRequest struct {
	TaskID string
	Kind   string // KindModel, KindTool or KindResource
	Ref    string // the alias of a model call, the name of a tool call
	// Session is the session a sandbox's call belongs to, as the gateway
	// read it from the call's token; "" for factoryd's calls and a
	// resource's reservation.
	Session string
	// Bytes is a model call's request body length. Earlier is the task's
	// newest settled call on the same ref with usage, which calibrates the
	// prompt tokens Bytes carries; without one, a token covers at least
	// one byte. CacheFloor is the share of Earlier's prompt the provider
	// serves from its cache at the least, as measured on the ref
	// (cacheFloor); 0 when none is measured.
	Bytes int
	// ImageTokens is the worst-case prompt tokens a vision call's one
	// image bills, sniffed from its header; the payload's characters are
	// already out of Bytes.
	ImageTokens int
	Earlier     Usage
	CacheFloor  float64
	Price       Price
	WantOut     int // the caller's output limit; 0 = none
	DayEUR      float64
	MonthEUR    float64
	// Project names the task's project and ProjectEUR its month cap, its
	// file's budget.month_eur (Tree.ProjectCap): +Inf for none.
	Project    string
	ProjectEUR float64
	// PremiumEUR is the premium month cap (budget.premium_month_eur) of a
	// call on Premium; 0 for any other call, or no such cap.
	PremiumEUR float64
	// EUR is a tool call's worst case, held as is, and Cap the task's caps
	// on the tool.
	EUR float64
	Cap ToolCap
}

// Hold is headroom taken for one call: EUR at the request's prompt as Fit
// prices it and MaxOut output tokens, of the WantOut the call asked for.
type Hold struct {
	ID      int64
	TaskID  string
	Kind    string
	Ref     string
	Session string
	EUR     float64
	MaxOut  int
	WantOut int
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
	return task, RefusedBudget
}

// RefusedBudget is the refusal of a call the task's own budget cannot fit.
const RefusedBudget = "refused_budget"

// RefusedDayCap is the refusal of a call the UTC day's headroom cannot
// fit.
const RefusedDayCap = "refused_day_cap"

// RefusedMonthCap is the refusal of a call the month's headroom cannot fit.
const RefusedMonthCap = "refused_month_cap"

// RefusedProjectCap is the refusal of a call the project's month headroom
// cannot fit.
const RefusedProjectCap = "refused_project_cap"

// RefusedPremiumCap is the refusal of a premium call the premium month
// headroom cannot fit; the gateway serves the call on Big instead.
const RefusedPremiumCap = "refused_premium_cap"

// Refusal is a call the headroom or a tool cap cannot fit.
type Refusal struct {
	Decision string // refused_budget, refused_day_cap, refused_month_cap, refused_project_cap, refused_premium_cap, refused_tool_requests, refused_tool_eur or refused_task_status
	Reason   string
}

// refusal is the headroom refusal decision with reason; the month cap's
// and project's cap's say what it refused and name the key a human raises,
// the project's in its file, and the premium cap's names its key. Only a
// model call finds a cap spent: a reservation or a tool call's worst case
// may exceed what is left while smaller calls still fit.
func refusal(decision, kind, project, reason string) *Refusal {
	if decision == RefusedPremiumCap {
		return &Refusal{Decision: decision, Reason: reason + "; the premium cap budget.premium_month_eur in policy.yaml cannot pay for it"}
	}
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

// Fit clamps a call's output limit, what it asked (WantOut) up to the
// alias's limit (Price.MaxOutput, Document.Limited), the limit alone when
// it asked none, to what the free task, day, month and project headroom
// can pay for, or refuses it. An image's ImageTokens price at the input
// rate beside the bytes. Calibrated by an earlier
// call (ruling 274), the prompt is about Bytes times that call's tokens per
// byte. Of the part up to that call's prompt tokens, the share the
// provider's measured floor vouches for (CacheFloor) is at the cache-read
// price; the rest and a margin of a quarter are at the input price. No
// provider promise is trusted: without a measured floor the whole prompt
// is at the input price. Without an earlier call it is at most Bytes
// tokens at the input price.
func Fit(f Free, r HoldRequest) (Hold, error) {
	free, decision := f.bound()
	img := float64(r.ImageTokens)
	tokens, measure := float64(r.Bytes)+img, "at most"
	inEUR := tokens * r.Price.In
	if e := r.Earlier; e.PromptTokens > 0 && e.BodyBytes > 0 {
		text := float64(r.Bytes) * float64(e.PromptTokens) / float64(e.BodyBytes)
		tokens, measure = text+img, "about"
		cached := min(text, float64(e.PromptTokens)) * min(r.CacheFloor, 1)
		inEUR = cached*r.Price.CacheRead + (text-cached+text/4+img)*r.Price.In
	}
	outFit := 0
	if left := free - inEUR; left > 0 {
		// the epsilon keeps an exact quotient (0.009/4e-6) from flooring low
		outFit = int(math.Min(math.Floor(left/r.Price.Out+1e-9), math.MaxInt32))
	}
	full := r.Price.MaxOutput
	if r.WantOut > 0 {
		full = min(full, r.WantOut)
	}
	maxOut := min(outFit, full)
	if maxOut <= 0 || maxOut < MinCallTokens && maxOut != r.WantOut {
		return Hold{}, refusal(decision, KindModel, r.Project, fmt.Sprintf(
			"free headroom %.6f EUR (%s) fits %d output tokens after a request of %d bytes (%s %.0f prompt tokens), below the minimum call of %d",
			free, f.figures(), max(outFit, 0), r.Bytes, measure, tokens, MinCallTokens))
	}
	// a premium call keeps its whole output or runs on big: a reasoning
	// model at max effort can spend a clamped limit before it answers
	if decision == RefusedPremiumCap && maxOut < full {
		return Hold{}, refusal(decision, KindModel, r.Project, fmt.Sprintf(
			"free headroom %.6f EUR (%s) fits %d of the call's %d output tokens after a request of %d bytes (%s %.0f prompt tokens)",
			free, f.figures(), maxOut, full, r.Bytes, measure, tokens))
	}
	return Hold{TaskID: r.TaskID, Kind: r.Kind, Ref: r.Ref, Session: r.Session, MaxOut: maxOut, WantOut: r.WantOut,
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
	free, decision := f.bound()
	if r.EUR > free {
		return Hold{}, refusal(decision, r.Kind, r.Project, fmt.Sprintf(
			"%s: worst case %.6f EUR exceeds the free headroom %.6f EUR (%s)", r.Ref, r.EUR, free, f.figures()))
	}
	return Hold{TaskID: r.TaskID, Kind: r.Kind, Ref: r.Ref, Session: r.Session, EUR: r.EUR}, nil
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
		if err := calibrate(ctx, tx, &r); err != nil {
			return Hold{}, err
		}
		h, err = Fit(free, r)
	}
	if err != nil {
		return Hold{}, err
	}
	if h.ID, err = InsertHold(ctx, tx, h, HoldTTL); err != nil {
		return Hold{}, err
	}
	return h, tx.Commit()
}

// rowQuerier is a pool or a transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// calibrate reads what Fit prices model call r from: r's task's newest
// settled call on r.Ref with usage (Earlier) and the ref's cache floor.
func calibrate(ctx context.Context, q rowQuerier, r *HoldRequest) error {
	err := q.QueryRowContext(ctx,
		`SELECT prompt_tokens, body_bytes FROM ledger WHERE task_id = $1 AND kind = 'model' AND ref = $2 AND prompt_tokens > 0 AND body_bytes > 0
		 ORDER BY id DESC LIMIT 1`, r.TaskID, r.Ref).Scan(&r.Earlier.PromptTokens, &r.Earlier.BodyBytes)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	r.CacheFloor, err = cacheFloor(ctx, q, r.Ref)
	return err
}

// CalibrateEstimate is r calibrated for an estimate of calls not yet made,
// and the mean output tokens of the ref's newest cacheFloorCalls settled
// calls, 0 without one. Without a settled call of r's task on r.Ref,
// Earlier is the ref's newest settled call of any task, the scope
// cacheFloor reads; TakeHold keeps its own first hold, a token a byte.
func CalibrateEstimate(ctx context.Context, q rowQuerier, r HoldRequest) (HoldRequest, int, error) {
	if err := calibrate(ctx, q, &r); err != nil {
		return r, 0, err
	}
	if r.Earlier.PromptTokens == 0 {
		err := q.QueryRowContext(ctx,
			`SELECT prompt_tokens, body_bytes FROM ledger WHERE kind = 'model' AND ref = $1 AND prompt_tokens > 0 AND body_bytes > 0
			 ORDER BY id DESC LIMIT 1`, r.Ref).Scan(&r.Earlier.PromptTokens, &r.Earlier.BodyBytes)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return r, 0, err
		}
	}
	var mean float64
	err := q.QueryRowContext(ctx,
		`SELECT COALESCE(avg(output_tokens), 0)::float8 FROM (SELECT output_tokens FROM ledger
		 WHERE kind = 'model' AND ref = $1 AND prompt_tokens > 0 AND body_bytes > 0 ORDER BY id DESC LIMIT $2) c`,
		r.Ref, cacheFloorCalls).Scan(&mean)
	return r, int(math.Ceil(mean)), err
}

// cacheFloorCalls is how many of a ref's newest settled calls measure its
// cache floor.
const cacheFloorCalls = 100

// cacheFloor is the least share of its earlier call's prompt (the prefix
// Fit prices) the provider served from its cache, over those of the newest
// cacheFloorCalls settled calls on ref that followed an earlier call of
// their task on ref, Earlier as TakeHold reads it; 0 when none did.
func cacheFloor(ctx context.Context, q rowQuerier, ref string) (float64, error) {
	var floor float64
	err := q.QueryRowContext(ctx,
		`SELECT COALESCE(min(c.cached_tokens::float8 / LEAST(c.prompt_tokens, e.prompt_tokens)), 0)::float8
		 FROM (SELECT id, task_id, prompt_tokens, cached_tokens FROM ledger
		       WHERE kind = 'model' AND ref = $1 AND prompt_tokens > 0 AND body_bytes > 0
		       ORDER BY id DESC LIMIT $2) c
		 CROSS JOIN LATERAL (SELECT prompt_tokens FROM ledger
		       WHERE task_id = c.task_id AND kind = 'model' AND ref = $1 AND prompt_tokens > 0 AND body_bytes > 0 AND id < c.id
		       ORDER BY id DESC LIMIT 1) e`, ref, cacheFloorCalls).Scan(&floor)
	return floor, err
}

// openingSessions is how many of the newest sessions measure a session's
// opening, and the largest of their openings is the one priced: a brief
// grows with an attempt's findings, so a median or a mean would price an
// early attempt's. A floor too high only lets a human fund a card that
// might have started; one too low funds an attempt that dies on its
// opening calls.
const openingSessions = 20

// OpeningCall is a call of a session's opening: its ref, its request's
// bytes, the output limit it asked for (0 for none), and what TakeHold
// prices the task's call on the ref from (HoldRequest's Earlier and
// CacheFloor).
type OpeningCall struct {
	Ref            string
	Bytes, WantOut int
	Earlier        Usage
	CacheFloor     float64
}

// Opening is how a session opens on a ref, read for a task. A session's
// opening is every model call that started before its first one ended,
// the first the one whose hold was taken first: the harness opens with
// calls at once, a small one beside the agent's first prompt, so their
// holds are taken together. Sessions are the openings of the newest
// openingSessions sessions whose first call was on the ref, newest first,
// each in the order its holds were taken; a session whose first call has
// no measure (a swept hold) counts none. With none on the ref, AnyRef:
// every ref's count, since a session opens with the harness's same calls
// whatever its model, a call on the first call's ref read as on the ref.
type Opening struct {
	Sessions [][]OpeningCall
	AnyRef   bool
}

// Hold is what the holds of the largest opening take for a session of r's
// task, and that opening: each call's hold as Fit prices it with the
// headroom unbounded, on its ref at price(ref), its prompt and the output
// it asked for up to that price's limit, summed. 0 and nil without a
// session; a call whose ref cannot be priced fails it.
func (o Opening) Hold(r HoldRequest, price func(ref string) (Price, error)) (float64, []OpeningCall, error) {
	inf := math.Inf(1)
	var most float64
	var largest []OpeningCall
	for _, calls := range o.Sessions {
		var eur float64
		for _, c := range calls {
			p, err := price(c.Ref)
			if err != nil {
				return 0, nil, err
			}
			r.Ref, r.Price, r.Bytes, r.WantOut, r.Earlier, r.CacheFloor = c.Ref, p, c.Bytes, c.WantOut, c.Earlier, c.CacheFloor
			h, _ := Fit(Free{Task: inf, Day: inf, Month: inf, Project: inf}, r)
			eur += h.EUR
		}
		if largest == nil || eur > most {
			most, largest = eur, calls
		}
	}
	return most, largest, nil
}

// ReadOpening reads the opening of a session of taskID on ref.
func ReadOpening(ctx context.Context, q *sql.DB, taskID, ref string) (Opening, error) {
	var o Opening
	for _, on := range []string{ref, ""} {
		// newest first, a call is its session's first when no call of the
		// session started earlier: the scan ends at the window
		rows, err := q.QueryContext(ctx,
			`WITH first AS (SELECT l.id, l.session, l.ref, l.created_at AS ended FROM ledger l
			      WHERE l.kind = 'model' AND l.session <> '' AND l.held_at IS NOT NULL AND l.body_bytes > 0 AND (l.ref = $1 OR $1 = '')
			        AND NOT EXISTS (SELECT 1 FROM ledger e WHERE e.kind = 'model' AND e.session = l.session
			                        AND (e.held_at < l.held_at OR e.held_at = l.held_at AND e.id < l.id))
			      ORDER BY l.id DESC LIMIT $2)
			 SELECT f.id, f.ref, o.ref, o.body_bytes, o.want_out FROM first f
			 JOIN ledger o ON o.kind = 'model' AND o.session = f.session AND o.held_at < f.ended
			 ORDER BY f.id DESC, o.held_at, o.id`, on, openingSessions)
		if err != nil {
			return Opening{}, err
		}
		var session int64
		for rows.Next() {
			var id int64
			var first string
			var c OpeningCall
			if err := rows.Scan(&id, &first, &c.Ref, &c.Bytes, &c.WantOut); err != nil {
				return Opening{}, errors.Join(err, rows.Close())
			}
			if c.Ref == first {
				c.Ref = ref
			}
			if id != session {
				o.Sessions, session = append(o.Sessions, nil), id
			}
			o.Sessions[len(o.Sessions)-1] = append(o.Sessions[len(o.Sessions)-1], c)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return Opening{}, err
		}
		o.AnyRef = on == ""
		if len(o.Sessions) > 0 {
			break
		}
	}
	calibrated := map[string]HoldRequest{}
	for _, calls := range o.Sessions {
		for i, c := range calls {
			r, ok := calibrated[c.Ref]
			if !ok {
				r = HoldRequest{TaskID: taskID, Ref: c.Ref}
				if err := calibrate(ctx, q, &r); err != nil {
					return Opening{}, err
				}
				calibrated[c.Ref] = r
			}
			calls[i].Earlier, calls[i].CacheFloor = r.Earlier, r.CacheFloor
		}
	}
	return o, nil
}

// LockHeadroom serializes everything that takes headroom, until tx ends.
func LockHeadroom(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('factory-headroom'))`)
	return err
}

// Free is a task's, the day's, the month's and the task's project's month
// headroom after every outstanding hold. The project's is +Inf for a
// project without a cap. Premium is the premium month headroom of a call
// on Premium under a premium cap, nil for any other call.
type Free struct {
	Task, Day, Month, Project float64
	Premium                   *float64
}

// bound is HeadroomBound over f, and the premium headroom below it.
func (f Free) bound() (float64, string) {
	free, decision := HeadroomBound(f.Task, f.Day, f.Month, f.Project)
	if f.Premium != nil && *f.Premium < free {
		return *f.Premium, RefusedPremiumCap
	}
	return free, decision
}

// figures are the free headroom a refusal quotes; the project's only under
// a cap, the premium's only on a premium call under its cap.
func (f Free) figures() string {
	s := fmt.Sprintf("task %.6f, day %.6f, month %.6f", f.Task, f.Day, f.Month)
	if !math.IsInf(f.Project, 1) {
		s += fmt.Sprintf(", project %.6f", f.Project)
	}
	if f.Premium != nil {
		s += fmt.Sprintf(", premium %.6f", *f.Premium)
	}
	return s
}

// MonthSpentSQL is the spend over the ledger in the UTC calendar month of
// at, a timestamptz expression; a standing line is its project's monthly
// cost, not variable spend.
func MonthSpentSQL(at string) string {
	return `COALESCE((SELECT sum(eur) FROM ledger WHERE day >= date_trunc('month', ` + at + ` AT TIME ZONE 'UTC')::date AND kind <> 'standing'), 0)`
}

// daySpentSQL is the spend over the ledger on the UTC day of at, a
// timestamptz expression, standing lines left out; heldSQL is every
// outstanding hold, which counts against the day and the month.
func daySpentSQL(at string) string {
	return `COALESCE((SELECT sum(eur) FROM ledger WHERE day = (` + at + ` AT TIME ZONE 'UTC')::date AND kind <> 'standing'), 0)`
}

const heldSQL = `COALESCE((SELECT sum(eur) FROM holds), 0)`

// premiumRef holds a ledger or hold row on Premium: an agent's ref is the
// alias, factoryd's purpose/alias.
const premiumRef = `kind = 'model' AND regexp_replace(ref, '^.*/', '') = '` + Premium + `'`

// PremiumMonthSpentSQL is the spend on Premium over the ledger in the UTC
// calendar month of at, a timestamptz expression.
func PremiumMonthSpentSQL(at string) string {
	return `COALESCE((SELECT sum(eur) FROM ledger WHERE day >= date_trunc('month', ` + at + ` AT TIME ZONE 'UTC')::date AND ` + premiumRef + `), 0)`
}

// DayFree is the UTC day's free headroom under dayEUR at at, the day's
// figure FreeHeadroom fits a hold into: dayEUR less the day's spend and
// every outstanding hold.
func DayFree(ctx context.Context, db *sql.DB, dayEUR float64, at time.Time) (float64, error) {
	var free float64
	err := db.QueryRowContext(ctx, `SELECT ($1 - `+daySpentSQL("$2::timestamptz")+` - `+heldSQL+`)::float8`, dayEUR, at).Scan(&free)
	return free, err
}

// now is the clock of the day's and the month's headroom.
var now = time.Now

// FreeHeadroom reads r's task's, the UTC day's, the UTC calendar month's
// and the task's project's month headroom inside tx (under LockHeadroom),
// as ReadFree does. A task that is neither running nor in review is
// refused, but for a waiting-human task's brief: settlement relies on a
// finished task taking no new hold.
func FreeHeadroom(ctx context.Context, tx *sql.Tx, r HoldRequest) (Free, error) {
	t, f, err := ReadFree(ctx, tx, r)
	if err != nil {
		return Free{}, err
	}
	brief := t.Status == "waiting-human" && r.Session == "" && strings.HasPrefix(r.Ref, PurposeBrief+"/")
	if !slices.Contains(holdStatuses, t.Status) && !brief {
		return Free{}, &Refusal{Decision: "refused_task_status", Reason: fmt.Sprintf("task %s is %s; only a running or in-review task takes a hold, and a task waiting on a person its brief's alone", r.TaskID, t.Status)}
	}
	return f, nil
}

// ReadFree reads r's task's own headroom and outstanding holds, and the
// task's, the UTC day's, the UTC calendar month's and the task's
// project's month headroom: FreeHeadroom's figures, and the attempt
// floors' (factoryd's AttemptFunded and SessionFunded) on the pool. The
// project's is r.ProjectEUR less the month's ledger rows and the
// outstanding holds of every task of the task's repository, read only
// under a cap (+Inf otherwise). Every outstanding hold counts against the
// day and the month. A call under a premium cap (r.PremiumEUR) also reads
// the premium month headroom: the cap less the month's spend and the
// outstanding holds on Premium.
func ReadFree(ctx context.Context, q rowQuerier, r HoldRequest) (TaskFree, Free, error) {
	taskID, at := r.TaskID, now()
	t, err := readTaskFree(ctx, q, taskID)
	if err != nil {
		return TaskFree{}, Free{}, err
	}
	var f Free
	var day, month, allHeld float64
	if err := q.QueryRowContext(ctx,
		`SELECT ($1 - `+daySpentSQL("$3::timestamptz")+`)::float8,
		        ($2 - `+MonthSpentSQL("$3::timestamptz")+`)::float8,
		        `+heldSQL+`::float8`, r.DayEUR, r.MonthEUR, at,
	).Scan(&day, &month, &allHeld); err != nil {
		return TaskFree{}, Free{}, err
	}
	f.Task, f.Day, f.Month, f.Project = t.Free(), day-allHeld, month-allHeld, math.Inf(1)
	if r.PremiumEUR > 0 {
		var premium float64
		if err := q.QueryRowContext(ctx,
			`SELECT ($1 - `+PremiumMonthSpentSQL("$2::timestamptz")+` - COALESCE((SELECT sum(eur) FROM holds WHERE `+premiumRef+`), 0))::float8`,
			r.PremiumEUR, at).Scan(&premium); err != nil {
			return TaskFree{}, Free{}, err
		}
		f.Premium = &premium
	}
	if math.IsInf(r.ProjectEUR, 1) {
		return t, f, nil
	}
	// a standing line's task id is standing:<repository>: it joins no task
	var spent, held float64
	if err := q.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT sum(l.eur) FROM ledger l JOIN tasks p ON p.id = l.task_id
		                  WHERE p.repository = t.repository AND l.day >= date_trunc('month', $2::timestamptz AT TIME ZONE 'UTC')::date), 0)::float8,
		        COALESCE((SELECT sum(h.eur) FROM holds h JOIN tasks p ON p.id = h.task_id WHERE p.repository = t.repository), 0)::float8
		 FROM tasks t WHERE t.id = $1`, taskID, at).Scan(&spent, &held); err != nil {
		return TaskFree{}, Free{}, err
	}
	f.Project = r.ProjectEUR - spent - held
	return t, f, nil
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

// readTaskFree reads taskID's own headroom and outstanding holds in one
// statement: ReadFree's task figures.
func readTaskFree(ctx context.Context, q rowQuerier, taskID string) (TaskFree, error) {
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
		`INSERT INTO holds (task_id, kind, ref, session, want_out, eur, expires_at) VALUES ($1, $2, $3, $4, $5, $6, now() + make_interval(secs => $7))
		 RETURNING id`, h.TaskID, h.Kind, h.Ref, h.Session, h.WantOut, h.EUR, ttl.Seconds()).Scan(&id)
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
// ledger row with the call's usage (a model call's; zero else), its
// session, when its hold was taken and the output it asked for. A
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
	var heldAt sql.NullTime
	err = tx.QueryRowContext(ctx, `DELETE FROM holds WHERE id = $1 RETURNING eur::float8, created_at`, h.ID).Scan(&held, &heldAt)
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, session, held_at, want_out, eur, prompt_tokens, cached_tokens, output_tokens, body_bytes)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`, h.TaskID, h.Kind, h.Ref, h.Session, heldAt, h.WantOut, amount,
			u.PromptTokens, u.CachedTokens, u.OutputTokens, u.BodyBytes); err != nil {
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

// AuditAuthorSQL is the author of an audit row of the task taskID names,
// an SQL expression: the task's issue author, the human behind its calls
// (roadmap 41); empty for a call of no known task.
func AuditAuthorSQL(taskID string) string {
	return `COALESCE((SELECT author FROM tasks WHERE tasks.id = ` + taskID + `), '')`
}

// SweepHolds books every expired model and tool hold at its full amount,
// with its session, writes each one's audit row (decision swept: the
// call's caller, cut by a restart, never wrote one) and returns how many it
// booked. The row's kind and alias come from the ref: purpose/alias for a
// factoryd call, the alias for an agent's, the tool's name for a tool
// call; its author is the task's, as on the gateway's rows. A resource
// reservation is never swept: accrual books its resource's hours and drops
// it, so a sweep would book the same hours twice.
func SweepHolds(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`WITH gone AS (DELETE FROM holds WHERE expires_at < now() AND kind <> 'resource' RETURNING id, task_id, kind, ref, session, want_out, created_at, eur),
		      t AS (UPDATE tasks SET spent_eur = spent_eur + g.eur
		            FROM (SELECT task_id, sum(eur) AS eur FROM gone GROUP BY task_id) g WHERE tasks.id = g.task_id),
		      l AS (INSERT INTO ledger (task_id, kind, ref, session, held_at, want_out, eur)
		            SELECT task_id, kind, ref, session, created_at, want_out, eur FROM gone WHERE eur > 0),
		      a AS (INSERT INTO audit (task_id, decision, reason, kind, model_alias, tool, spend_eur, hold_eur, author)
		            SELECT task_id, 'swept', 'hold ' || id || ' expired unreleased; booked at its full amount',
		                   CASE WHEN kind = 'model' AND strpos(ref, '/') > 0 THEN split_part(ref, '/', 1) ELSE 'agent' END,
		                   CASE WHEN kind = 'model' THEN substr(ref, strpos(ref, '/') + 1) ELSE '' END,
		                   CASE WHEN kind = 'tool' THEN ref ELSE '' END,
		                   eur, eur, `+AuditAuthorSQL("gone.task_id")+` FROM gone)
		 SELECT count(*) FROM gone`).Scan(&n)
	return n, err
}
