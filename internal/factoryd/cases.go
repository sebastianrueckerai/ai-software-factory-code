package factoryd

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// CaseKind names the problem a case is about.
type CaseKind string

const (
	CaseSessionCap       CaseKind = "session_cap"
	CaseAgentTimeCap     CaseKind = "agent_time_cap"
	CaseBudgetFloor      CaseKind = "budget_floor"
	CaseDayCap           CaseKind = "day_cap"
	CaseMonthCap         CaseKind = "month_cap"
	CaseProjectCap       CaseKind = "project_cap"
	CaseCheckinHuman     CaseKind = "checkin_human"
	CaseCheckinShare     CaseKind = "checkin_share"
	CaseGateStop         CaseKind = "gate_stop"
	CaseFindingsRepeated CaseKind = "findings_repeated"
	CaseDependencyClosed CaseKind = "dependency_closed"
	CaseDependencyFailed CaseKind = "dependency_failed"
	CaseWaitExpired      CaseKind = "wait_expired"
	CasePodNeverStarted  CaseKind = "pod_never_started"
	CaseProviderRefused  CaseKind = "provider_refused"
	CaseNoAnswer         CaseKind = "no_answer"
	CaseContentFilter    CaseKind = "content_filter"
	CaseImageWait        CaseKind = "image_wait"
	CaseScreeningSuspect CaseKind = "screening_suspect"
	CaseAgentEscalate    CaseKind = "agent_escalate"
	CaseAgentRequestInfo CaseKind = "agent_request_info"
	CasePlanQuestion     CaseKind = "plan_question"
	CasePlanConfirmation CaseKind = "plan_confirmation"
	CaseMergeApproval    CaseKind = "merge_approval"
	CaseHumanCard        CaseKind = "human_card"
	CaseOther            CaseKind = "other"
	CaseMonitorFlag      CaseKind = "monitor_flag"
)

// Case statuses. An open or at_human case is live: a task has at most one
// live case per kind and state key.
const (
	CaseOpen      = "open"
	CaseAtHuman   = "at_human"
	CaseResolved  = "resolved"
	CaseWithdrawn = "withdrawn"
)

// caseLiveStatuses are a live case's statuses, the predicate of the
// cases_open_key index.
const caseLiveStatuses = `('open', 'at_human')`

// caseLive is the SQL test for a live case in a query on cases alone.
const caseLive = `status IN ` + caseLiveStatuses

// Case is one problem on a card, from its opening to its end. Follows is
// the case it comes after, 0 for none; CommentID is its issue comment, 0
// for none. OwnerRung is the rung that owns its next move, 3 on opening;
// WakeOn are the events that end its wait, and NextWakeAt is when the
// sweep wakes it if none does, nil for never. SetCaseWake sets all three.
type Case struct {
	ID         int64
	TaskID     string
	Repository string
	Kind       CaseKind
	StateKey   string
	Status     string
	Problem    string
	Facts      map[string]any
	Money      *CaseMoney
	CommentID  int64
	Follows    int64
	OpenedAt   time.Time
	ResolvedAt *time.Time
	Resolution *CaseResolution
	OwnerRung  int
	NextWakeAt *time.Time
	WakeOn     []string
}

// CaseMoney is the money a case is about: the budget of kind, what is
// spent of it, what more it needs and what that buys.
type CaseMoney struct {
	Kind      string  `json:"kind"`
	BudgetEUR float64 `json:"budget_eur"`
	SpentEUR  float64 `json:"spent_eur"`
	NeedEUR   float64 `json:"need_eur,omitempty"`
	Buys      string  `json:"buys,omitempty"`
}

// CaseResolution is how a case ended: who ended it, on which rung, with
// which action and why.
type CaseResolution struct {
	By     string         `json:"by"`
	Rung   int            `json:"rung"`
	Action string         `json:"action"`
	Args   map[string]any `json:"args,omitempty"`
	Reason string         `json:"reason,omitempty"`
}

// CaseStep is one rung's work on a case: what it read, what it found,
// what it did, what it decided and on what grounds, and what it cost.
// DueAt is when its rung must have answered, nil for no bound.
type CaseStep struct {
	ID        int64
	CaseID    int64
	Rung      int
	Actor     string
	StartedAt time.Time
	EndedAt   *time.Time
	Read      []CaseRead
	Found     string
	Did       []CaseAction
	Decision  string
	Grounds   string
	CostEUR   float64
	Outcome   string
	DueAt     *time.Time
}

// CaseRead is one tool call a step read through.
type CaseRead struct {
	Tool  string `json:"tool"`
	Args  string `json:"args,omitempty"`
	Bytes int    `json:"bytes"`
}

// CaseAction is one action a step took.
type CaseAction struct {
	Action string         `json:"action"`
	Args   map[string]any `json:"args,omitempty"`
	Result string         `json:"result,omitempty"`
}

// Case step outcomes.
const (
	StepInProgress = "in_progress"
	StepResolved   = "resolved"
	StepHandedUp   = "handed_up"
	StepWithdrawn  = "withdrawn"
)

const caseColumns = `id, task_id, repository, kind, state_key, status, problem, facts, money, comment_id, COALESCE(follows, 0), opened_at, resolved_at, resolution, owner_rung, next_wake_at, wake_on`

func scanCase(row rowScanner) (Case, error) {
	var c Case
	var facts, money, resolution []byte
	// the array arrives as a string through database/sql; pgx's scanner
	// decodes it into the slice
	if err := row.Scan(&c.ID, &c.TaskID, &c.Repository, &c.Kind, &c.StateKey, &c.Status, &c.Problem, &facts, &money,
		&c.CommentID, &c.Follows, &c.OpenedAt, &c.ResolvedAt, &resolution, &c.OwnerRung, &c.NextWakeAt, pgtype.NewMap().SQLScanner(&c.WakeOn)); err != nil {
		return Case{}, err
	}
	if err := json.Unmarshal(facts, &c.Facts); err != nil {
		return Case{}, err
	}
	if len(money) > 0 {
		if err := json.Unmarshal(money, &c.Money); err != nil {
			return Case{}, err
		}
	}
	if len(resolution) > 0 {
		if err := json.Unmarshal(resolution, &c.Resolution); err != nil {
			return Case{}, err
		}
	}
	return c, nil
}

// OpenCase inserts c unless an open or at_human case with the same
// task, kind and state key exists; then it returns that case and false,
// and changes nothing on it. An empty Status opens the case as open; a
// status other than open or at_human is refused.
func (s *PGStore) OpenCase(ctx context.Context, c Case) (Case, bool, error) {
	return openCase(ctx, s.db, c)
}

func openCase(ctx context.Context, q rowQuerier, c Case) (Case, bool, error) {
	c.Status = cmp.Or(c.Status, CaseOpen)
	if c.Status != CaseOpen && c.Status != CaseAtHuman {
		return Case{}, false, fmt.Errorf("OpenCase opens a case only as open or at_human, not %s; ResolveCase or WithdrawCase ends it", c.Status)
	}
	if c.Facts == nil {
		c.Facts = map[string]any{}
	}
	facts, err := toJSON(c.Facts)
	if err != nil {
		return Case{}, false, err
	}
	var money any
	if c.Money != nil {
		if money, err = toJSON(c.Money); err != nil {
			return Case{}, false, err
		}
	}
	// the live case the insert met may end before the select reads it:
	// then the insert goes in on its second try
	for try := 0; ; try++ {
		got, err := scanCase(q.QueryRowContext(ctx,
			`INSERT INTO cases (task_id, repository, kind, state_key, status, problem, facts, money, comment_id, follows)
			 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9, NULLIF($10::bigint, 0))
			 ON CONFLICT (task_id, kind, state_key) WHERE `+caseLive+` DO NOTHING
			 RETURNING `+caseColumns,
			c.TaskID, c.Repository, c.Kind, c.StateKey, c.Status, c.Problem, facts, money, c.CommentID, c.Follows))
		if !errors.Is(err, sql.ErrNoRows) {
			return got, err == nil, err
		}
		got, err = scanCase(q.QueryRowContext(ctx,
			`SELECT `+caseColumns+` FROM cases WHERE task_id = $1 AND kind = $2 AND state_key = $3 AND `+caseLive,
			c.TaskID, c.Kind, c.StateKey))
		if !errors.Is(err, sql.ErrNoRows) || try == 1 {
			return got, false, err
		}
	}
}

// AppendCaseStep inserts step st of its case and answers its id. StartedAt
// is the store's now; an empty Outcome is in_progress; a nil DueAt bounds
// nothing.
func (s *PGStore) AppendCaseStep(ctx context.Context, st CaseStep) (int64, error) {
	return appendCaseStep(ctx, s.db, st)
}

func appendCaseStep(ctx context.Context, q rowQuerier, st CaseStep) (int64, error) {
	read, err := toJSON(nonNil(st.Read))
	if err != nil {
		return 0, err
	}
	did, err := toJSON(nonNil(st.Did))
	if err != nil {
		return 0, err
	}
	var id int64
	err = q.QueryRowContext(ctx,
		`INSERT INTO case_steps (case_id, rung, actor, read, found, did, decision, grounds, cost_eur, outcome, due_at)
		 VALUES ($1, $2, $3, $4::jsonb, $5, $6::jsonb, $7, $8, $9, $10, $11) RETURNING id`,
		st.CaseID, st.Rung, st.Actor, read, st.Found, did, st.Decision, st.Grounds, st.CostEUR, cmp.Or(st.Outcome, StepInProgress), st.DueAt).Scan(&id)
	return id, err
}

// EndCaseStep ends in-progress step id with outcome and what the step
// found, did, decided and cost; ErrNotFound when no such step is in
// progress, as after its case ended.
func (s *PGStore) EndCaseStep(ctx context.Context, id int64, outcome, found string, did []CaseAction, decision, grounds string, costEUR float64) error {
	if outcome == StepInProgress {
		return errors.New("EndCaseStep ends a step as resolved, handed_up or withdrawn, not in_progress")
	}
	b, err := toJSON(nonNil(did))
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE case_steps SET outcome = $2, found = $3, did = $4::jsonb, decision = $5, grounds = $6, cost_eur = $7, ended_at = now()
		 WHERE id = $1 AND outcome = $8`,
		id, outcome, found, b, decision, grounds, costEUR, StepInProgress)
	return oneRow(res, err, fmt.Sprintf("case step %d in progress", id))
}

// SetCaseStatus moves live case id to status open or at_human;
// ErrNotFound when the case is not live. ResolveCase and WithdrawCase
// end a case.
func (s *PGStore) SetCaseStatus(ctx context.Context, id int64, status string) error {
	if status != CaseOpen && status != CaseAtHuman {
		return errors.New("SetCaseStatus moves a case only to open or at_human; ResolveCase or WithdrawCase ends it")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE cases SET status = $2 WHERE id = $1 AND `+caseLive, id, status)
	return oneRow(res, err, fmt.Sprintf("live case %d", id))
}

// SetCaseWake sets who owns the next move on live case id, the events that
// end its wait, and when the sweep wakes it if none does; a nil at clears
// the wake. ErrNotFound when the case is not live.
func (s *PGStore) SetCaseWake(ctx context.Context, id int64, rung int, on []string, at *time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE cases SET owner_rung = $2, wake_on = $3, next_wake_at = $4 WHERE id = $1 AND `+caseLive,
		id, rung, nonNil(on), at)
	return oneRow(res, err, fmt.Sprintf("live case %d", id))
}

// DueCases are the live cases whose wake is at or before now, the oldest
// wake first, at most limit.
func (s *PGStore) DueCases(ctx context.Context, now time.Time, limit int) ([]Case, error) {
	return s.cases(ctx, `SELECT `+caseColumns+` FROM cases WHERE `+caseLive+` AND next_wake_at <= $1 ORDER BY next_wake_at, id LIMIT $2`, now, limit)
}

// ExpireSteps hands up every in-progress step whose due_at is before now,
// its Found naming the bound its rung did not answer within after what it
// found before, and answers them, oldest first.
func (s *PGStore) ExpireSteps(ctx context.Context, now time.Time) ([]CaseStep, error) {
	// the constant outcome lets the partial index case_steps_due serve
	rows, err := s.db.QueryContext(ctx, `SELECT id, started_at, due_at, found FROM case_steps WHERE outcome = '`+StepInProgress+`' AND due_at < $1`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	var found []string
	for rows.Next() {
		var id int64
		var started, due time.Time
		var before string
		if err := rows.Scan(&id, &started, &due, &before); err != nil {
			return nil, err
		}
		ids, found = append(ids, id), append(found, stepExpired(before, started, due))
	}
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return nil, err
	}
	// a step its rung ended since the read keeps its own end
	return s.caseSteps(ctx,
		`WITH ended AS (
		     UPDATE case_steps c SET outcome = $3, found = e.found, ended_at = now()
		     FROM unnest($1::bigint[], $2::text[]) AS e(id, found)
		     WHERE c.id = e.id AND c.outcome = $4
		     RETURNING c.*
		 )
		 SELECT `+caseStepColumns+` FROM ended ORDER BY id`, ids, found, StepHandedUp, StepInProgress)
}

// stepExpired is the Found of a step its rung did not answer by due, after
// found, what the step found before: its bound, due less its start, to the
// minute as Go prints a duration less its zero units, as 35m, 1h30m or 2h.
func stepExpired(found string, started, due time.Time) string {
	bound := due.Sub(started).Round(time.Minute).String()
	if b, ok := strings.CutSuffix(bound, "m0s"); ok {
		bound = b + "m"
	}
	if b, ok := strings.CutSuffix(bound, "h0m"); ok {
		bound = b + "h"
	}
	expired := "the rung did not answer within " + bound
	if found == "" {
		return expired
	}
	return found + ", then " + expired
}

// caseAnswer is the action of a case an answer row resolved: an answer
// comment, or a press that names no action.
const caseAnswer = "answer"

// ErrCaseEnded refuses an answer on a case that ended: Case says how.
type ErrCaseEnded struct{ Case Case }

func (e ErrCaseEnded) Error() string {
	c := e.Case
	if c.Resolution == nil || c.ResolvedAt == nil {
		return fmt.Sprintf("case %d is %s", c.ID, c.Status)
	}
	at := c.ResolvedAt.UTC().Format("15:04")
	if c.Status == CaseWithdrawn {
		return fmt.Sprintf("case %d was withdrawn at %s UTC: %s", c.ID, at, c.Resolution.Reason)
	}
	return fmt.Sprintf("case %d was resolved by %s with %s at %s UTC", c.ID, c.Resolution.By, c.Resolution.Action, at)
}

// CaseDraw is what an answer on Rung draws beside its resolution: a raise
// of the card's ceiling by Raise (0 for none), then Extend's sessions and
// agent time with Lines and the check-in's Shares, which draw nothing
// from the pool, then FundEUR into the budget, which the card's
// ceiling bounds, else PolicyCeiling. CardBound and Pool are the card's
// draw bound, which rungs 0 and 1 draw within, and the project's month of
// draws, which rungs 0 to 2 draw within. RunGrants: the card's run grants
// Extend's sessions and agent time itself, so the rows record them against
// the pool and the card's row raises neither. Event is the rows'
// repository, issue, actor, via and detail.
type CaseDraw struct {
	Rung          int
	Raise         float64
	Extend        policy.Draw
	Lines         int
	Shares        int
	FundEUR       float64
	PolicyCeiling float64
	CardBound     policy.Draw
	Pool          policy.Draw
	RunGrants     bool
	Event         Event
}

// amount is what d takes from the pool: its sessions, agent time and
// money, the raise's included.
func (d CaseDraw) amount() policy.Draw {
	return d.Extend.Plus(policy.Draw{EUR: d.FundEUR + d.Raise})
}

// rows are d's events: the ceiling's, the extend's and the fund's. A
// raise moves no money, so its row is no draw from the pool.
func (d CaseDraw) rows() (ceiling, extend, fund Event) {
	ceiling, extend, fund = d.Event, d.Event, d.Event
	ceiling.Detail = maps.Clone(d.Event.Detail)
	delete(ceiling.Detail, "pool")
	extend.Kind, fund.Kind = EventCardExtended, EventCardFunded
	return ceiling, extend, fund
}

// ResolveCase ends live case id by r, withdrawn when r withdraws it,
// else resolved, its in-progress steps with it, and in the same
// transaction writes draw d for the case's card and appends step st (nil
// for none). A case the webhook's copy of r's answer comment
// (r.Args["comment"]) resolved first as a plain answer is resolved again
// by r. A case that ended otherwise is ErrCaseEnded, no case ErrNotFound.
func (s *PGStore) ResolveCase(ctx context.Context, id int64, r CaseResolution, d *CaseDraw, st *CaseStep) error {
	comment, _ := toInt(r.Args["comment"])
	status, outcome := CaseResolved, StepResolved
	if r.Action == ActWithdraw {
		status, outcome = CaseWithdrawn, StepWithdrawn
	}
	return s.commitRows(ctx, func(tx *sql.Tx) ([]Event, error) {
		taskID, err := endCase(ctx, tx, id, status, outcome, r, int64(comment))
		if errors.Is(err, ErrNotFound) {
			c, cerr := scanCase(tx.QueryRowContext(ctx, `SELECT `+caseColumns+` FROM cases WHERE id = $1`, id))
			if errors.Is(cerr, sql.ErrNoRows) {
				return nil, fmt.Errorf("case %d: %w", id, ErrNotFound)
			}
			if cerr != nil {
				return nil, cerr
			}
			return nil, ErrCaseEnded{c}
		}
		if err != nil {
			return nil, err
		}
		var rows []Event
		if d != nil {
			if rows, err = s.drawCase(ctx, tx, taskID, *d); err != nil {
				return nil, err
			}
		}
		if st != nil {
			step := *st
			step.CaseID = id
			if _, err := appendCaseStep(ctx, tx, step); err != nil {
				return nil, err
			}
		}
		return rows, nil
	})
}

// drawCase writes d for card taskID inside tx once its rung's bounds fit
// it (drawFits), the raise first, and answers the rows it wrote: a row its
// identity dropped (ID 0) is no row to publish.
func (s *PGStore) drawCase(ctx context.Context, tx *sql.Tx, taskID string, d CaseDraw) ([]Event, error) {
	if err := s.drawFits(ctx, tx, taskID, d); err != nil {
		return nil, err
	}
	ceiling, extend, fund := d.rows()
	var rows []Event
	wrote := func(stored Event) {
		if stored.ID != 0 {
			rows = append(rows, stored)
		}
	}
	if d.Raise != 0 {
		stored, err := s.raiseCeiling(ctx, tx, taskID, d.Raise, d.PolicyCeiling, ceiling)
		if err != nil {
			return nil, err
		}
		wrote(stored)
	}
	if !d.Extend.IsZero() || d.Lines > 0 || d.Shares > 0 {
		_, _, stored, err := s.extendCard(ctx, tx, taskID, d.Extend, d.Lines, d.Shares, d.RunGrants, extend)
		if err != nil {
			return nil, err
		}
		wrote(stored)
	}
	if d.FundEUR > 0 {
		_, _, stored, err := s.fundCard(ctx, tx, taskID, d.FundEUR, d.PolicyCeiling, fund)
		if err != nil {
			return nil, err
		}
		wrote(stored)
	}
	return rows, nil
}

func resolveCase(ctx context.Context, q rowQuerier, id int64, r CaseResolution) error {
	_, err := endCase(ctx, q, id, CaseResolved, StepResolved, r, 0)
	return err
}

// WithdrawCase ends live case id as withdrawn by the factory for reason,
// and its in-progress steps with it; ErrNotFound when the case is not
// live.
func (s *PGStore) WithdrawCase(ctx context.Context, id int64, reason string) error {
	return withdrawCase(ctx, s.db, id, reason)
}

func withdrawCase(ctx context.Context, q rowQuerier, id int64, reason string) error {
	_, err := endCase(ctx, q, id, CaseWithdrawn, StepWithdrawn, CaseResolution{By: FactoryDecider, Rung: 0, Action: "withdraw", Reason: reason}, 0)
	return err
}

// endCase ends case id with status by r, its in-progress steps with
// outcome, and answers its card. It ends a live case, and with answered
// set also a case the webhook's copy of that answer comment resolved as a
// plain answer. Any other case is ErrNotFound.
func endCase(ctx context.Context, q rowQuerier, id int64, status, outcome string, r CaseResolution, answered int64) (string, error) {
	b, err := toJSON(r)
	if err != nil {
		return "", err
	}
	var taskID string
	err = q.QueryRowContext(ctx,
		`WITH ended AS (
		     UPDATE cases SET status = $2, resolved_at = now(), resolution = $3::jsonb
		     WHERE id = $1 AND (`+caseLive+` OR $6::bigint <> 0 AND status = $7 AND resolution->>'action' = $8
		         AND resolution->'args'->>'comment' = $6::bigint::text)
		     RETURNING id, task_id
		 ), steps AS (
		     UPDATE case_steps SET outcome = $4, ended_at = now() WHERE case_id IN (SELECT id FROM ended) AND outcome = $5
		 )
		 SELECT task_id FROM ended`,
		id, status, b, outcome, StepInProgress, answered, CaseResolved, caseAnswer).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		err = fmt.Errorf("live case %d: %w", id, ErrNotFound)
	}
	return taskID, err
}

// CaseByID is case id; ErrNotFound when there is none.
func (s *PGStore) CaseByID(ctx context.Context, id int64) (Case, error) {
	c, err := scanCase(s.db.QueryRowContext(ctx, `SELECT `+caseColumns+` FROM cases WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Case{}, fmt.Errorf("case %d: %w", id, ErrNotFound)
	}
	return c, err
}

// OpenCasesOf are the open and at_human cases of task taskID, oldest
// first.
func (s *PGStore) OpenCasesOf(ctx context.Context, taskID string) ([]Case, error) {
	return s.cases(ctx, `SELECT `+caseColumns+` FROM cases WHERE task_id = $1 AND `+caseLive+` ORDER BY id`, taskID)
}

// CasesOf are every case of task taskID, oldest first.
func (s *PGStore) CasesOf(ctx context.Context, taskID string) ([]Case, error) {
	return s.cases(ctx, `SELECT `+caseColumns+` FROM cases WHERE task_id = $1 ORDER BY id`, taskID)
}

// OpenCasesIn are the open and at_human cases of repository, oldest
// first.
func (s *PGStore) OpenCasesIn(ctx context.Context, repository string) ([]Case, error) {
	return s.cases(ctx, `SELECT `+caseColumns+` FROM cases WHERE repository = $1 AND `+caseLive+` ORDER BY id`, repository)
}

func (s *PGStore) cases(ctx context.Context, query string, args ...any) ([]Case, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Case
	for rows.Next() {
		c, err := scanCase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

const caseStepColumns = `id, case_id, rung, actor, started_at, ended_at, read, found, did, decision, grounds, cost_eur::float8, outcome, due_at`

// CaseSteps are the steps of case caseID, oldest first.
func (s *PGStore) CaseSteps(ctx context.Context, caseID int64) ([]CaseStep, error) {
	return s.caseSteps(ctx, `SELECT `+caseStepColumns+` FROM case_steps WHERE case_id = $1 ORDER BY id`, caseID)
}

func (s *PGStore) caseSteps(ctx context.Context, query string, args ...any) ([]CaseStep, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CaseStep
	for rows.Next() {
		var st CaseStep
		var read, did []byte
		if err := rows.Scan(&st.ID, &st.CaseID, &st.Rung, &st.Actor, &st.StartedAt, &st.EndedAt, &read, &st.Found, &did,
			&st.Decision, &st.Grounds, &st.CostEUR, &st.Outcome, &st.DueAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(read, &st.Read); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(did, &st.Did); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
