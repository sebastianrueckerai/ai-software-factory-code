package factoryd

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
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
// for none.
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

const caseColumns = `id, task_id, repository, kind, state_key, status, problem, facts, money, comment_id, COALESCE(follows, 0), opened_at, resolved_at, resolution`

func scanCase(row rowScanner) (Case, error) {
	var c Case
	var facts, money, resolution []byte
	if err := row.Scan(&c.ID, &c.TaskID, &c.Repository, &c.Kind, &c.StateKey, &c.Status, &c.Problem, &facts, &money,
		&c.CommentID, &c.Follows, &c.OpenedAt, &c.ResolvedAt, &resolution); err != nil {
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
// is the store's now; an empty Outcome is in_progress.
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
		`INSERT INTO case_steps (case_id, rung, actor, read, found, did, decision, grounds, cost_eur, outcome)
		 VALUES ($1, $2, $3, $4::jsonb, $5, $6::jsonb, $7, $8, $9, $10) RETURNING id`,
		st.CaseID, st.Rung, st.Actor, read, st.Found, did, st.Decision, st.Grounds, st.CostEUR, cmp.Or(st.Outcome, StepInProgress)).Scan(&id)
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

// ResolveCase ends live case id as resolved by r, and its in-progress
// steps with it; ErrNotFound when the case is not live.
func (s *PGStore) ResolveCase(ctx context.Context, id int64, r CaseResolution) error {
	return resolveCase(ctx, s.db, id, r)
}

func resolveCase(ctx context.Context, q rowQuerier, id int64, r CaseResolution) error {
	return endCase(ctx, q, id, CaseResolved, StepResolved, r)
}

// WithdrawCase ends live case id as withdrawn by the factory for reason,
// and its in-progress steps with it; ErrNotFound when the case is not
// live.
func (s *PGStore) WithdrawCase(ctx context.Context, id int64, reason string) error {
	return withdrawCase(ctx, s.db, id, reason)
}

func withdrawCase(ctx context.Context, q rowQuerier, id int64, reason string) error {
	return endCase(ctx, q, id, CaseWithdrawn, StepWithdrawn, CaseResolution{By: FactoryDecider, Rung: 0, Action: "withdraw", Reason: reason})
}

func endCase(ctx context.Context, q rowQuerier, id int64, status, outcome string, r CaseResolution) error {
	b, err := toJSON(r)
	if err != nil {
		return err
	}
	var n int
	err = q.QueryRowContext(ctx,
		`WITH ended AS (
		     UPDATE cases SET status = $2, resolved_at = now(), resolution = $3::jsonb WHERE id = $1 AND `+caseLive+` RETURNING id
		 ), steps AS (
		     UPDATE case_steps SET outcome = $4, ended_at = now() WHERE case_id IN (SELECT id FROM ended) AND outcome = $5
		 )
		 SELECT count(*) FROM ended`,
		id, status, b, outcome, StepInProgress).Scan(&n)
	if err == nil && n == 0 {
		err = fmt.Errorf("live case %d: %w", id, ErrNotFound)
	}
	return err
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

// CaseSteps are the steps of case caseID, oldest first.
func (s *PGStore) CaseSteps(ctx context.Context, caseID int64) ([]CaseStep, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, case_id, rung, actor, started_at, ended_at, read, found, did, decision, grounds, cost_eur::float8, outcome
		 FROM case_steps WHERE case_id = $1 ORDER BY id`, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CaseStep
	for rows.Next() {
		var st CaseStep
		var read, did []byte
		if err := rows.Scan(&st.ID, &st.CaseID, &st.Rung, &st.Actor, &st.StartedAt, &st.EndedAt, &read, &st.Found, &did,
			&st.Decision, &st.Grounds, &st.CostEUR, &st.Outcome); err != nil {
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
