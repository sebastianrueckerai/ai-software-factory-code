package factoryd

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// CaseKind names the problem a case is about.
type CaseKind string

const (
	CaseSessionCap       CaseKind = "session_cap"
	CaseAgentTimeCap     CaseKind = "agent_time_cap"
	CaseBudgetFloor      CaseKind = "budget_floor"
	CaseDayCap           CaseKind = "day_cap"
	CaseMonthCap         CaseKind = "month_cap"
	CaseFactoryMonthCap  CaseKind = "factory_month_cap"
	CaseProjectCap       CaseKind = "project_cap"
	CaseCheckinHuman     CaseKind = "checkin_human"
	CaseCheckinShare     CaseKind = "checkin_share"
	CaseCheckinFailed    CaseKind = "checkin_failed"
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
	CaseJudgmentBudget   CaseKind = "judgment_budget"
	CasePolicyClass      CaseKind = "policy_class"
	CaseRedMain          CaseKind = "red_main"
	CaseRollHeld         CaseKind = "roll_held"
	CaseConfigRefused    CaseKind = "config_refused"
	CasePlanCriteria     CaseKind = "plan_criteria"
	// CasePlanHeld is a plan whose card a human closed while promises
	// were open: it holds production deploys until it closes
	CasePlanHeld CaseKind = "plan_held"
	// CaseProviderFreeze is the ask of the card a provider's limit on a
	// model class files
	CaseProviderFreeze CaseKind = "provider_freeze"
	// CasePlatformFault is a card whose sessions the platform ended
	// platformDeathsAsk times in a row
	CasePlatformFault CaseKind = "platform_fault"
	// CasePlatformDeaths is the ask of the human card filed when the
	// platform ends work on any card again and again
	CasePlatformDeaths CaseKind = "platform_deaths"
)

// caseKindRow is what the ladder does with a case of one kind.
type caseKindRow struct {
	Kind CaseKind
	// Words say the kind as a human reads it.
	Words string
	// Rung0 names the rows of rung0 tried on the kind.
	Rung0 []string
	// NoRung1 is why rung 1 never judges the kind, "" when it may.
	NoRung1 string
	// Rung2 is what rung 2 does with the kind, and Rung2Why why it does no
	// more than analyse it, or nothing.
	Rung2, Rung2Why string
	// Choices says its question's choices answer it.
	Choices bool
	// Exception is why a case of the kind passes the door with no step of
	// rung 1 or rung 2, "" for none.
	Exception string
}

// What rung 2 does with a case kind: decides it where the action table
// lets it, analyses it for the human, who alone decides, or nothing.
const (
	rung2Decides  = "decides"
	rung2Analyses = "analyses"
	rung2Nothing  = "nothing"
)

// The reasons the kind table gives.
const (
	checkinJudged    = "the check-in judged the stop: its hand to the human is rung 1's step"
	aboutTheBoard    = "it is about the board, not one card's session, which rung 1 reads"
	choicesAnswer    = "its question's choices answer it, and rung 1 answers with the action table's actions"
	noRungRunsBudget = "the judgment budget that pays the rungs is spent, so no rung can run"
	noRungRunsCap    = "the factory's month cap refuses the agent's own model calls, so no rung can run"
	authorAnswers    = "the agent asks its issue's author, whose answer alone the card's wait takes"
	humanClosedPlan  = "a human closed the plan's card, and only a human says whether its open promises are dropped"
	limitHoldsRungs  = "the provider's limit holds a model class, the rungs' own calls among its waiters, so no rung is sure to run"
	platformNoRung1  = "the check-in cannot judge the platform"
	nodesNeedAdmin   = "a fix of the nodes needs their admin key, which no agent holds"
)

// headRows are rung 0's rows for a stop at the card's head.
var headRows = []string{"route_approval", "rerun_checks"}

// caseKinds is the kind table, in the order the kinds are declared: for
// every case kind, its words, its rows at rung 0, why rung 1 does not judge
// it, what rung 2 does with it, whether its question's choices answer it,
// and why it passes the door with no rung's step.
var caseKinds = []caseKindRow{
	{Kind: CaseSessionCap, Words: "session cap", Rung0: headRows, Rung2: rung2Decides},
	{Kind: CaseAgentTimeCap, Words: "agent time cap", Rung0: headRows, Rung2: rung2Decides},
	{Kind: CaseBudgetFloor, Words: "budget floor", Rung0: headRows, Rung2: rung2Decides},
	{Kind: CaseDayCap, Words: "day cap", Rung0: []string{"cap_wait"}, NoRung1: "only a commit raises the day cap",
		Rung2: rung2Analyses, Rung2Why: "only a commit raises the day cap"},
	{Kind: CaseMonthCap, Words: "month cap", Rung0: []string{"cap_wait"}, NoRung1: "only a commit raises the month cap",
		Rung2: rung2Analyses, Rung2Why: "only a commit raises the month cap"},
	{Kind: CaseFactoryMonthCap, Words: "factory's month cap", NoRung1: noRungRunsCap, Rung2: rung2Nothing, Rung2Why: noRungRunsCap,
		Choices: true, Exception: noRungRunsCap},
	{Kind: CaseProjectCap, Words: "project cap", Rung0: []string{"cap_wait"}, NoRung1: "only a commit raises the project cap",
		Rung2: rung2Analyses, Rung2Why: "only a commit raises the project cap"},
	{Kind: CaseCheckinHuman, Words: "check-in with a human", NoRung1: checkinJudged, Rung2: rung2Decides},
	{Kind: CaseCheckinShare, Words: "check-in spend", Rung0: headRows, Rung2: rung2Decides},
	{Kind: CaseCheckinFailed, Words: "failed check-in", Rung0: headRows, Rung2: rung2Decides},
	{Kind: CaseGateStop, Words: "gate stop", Rung0: append(slices.Clone(headRows), "wait_checks"), Rung2: rung2Decides},
	{Kind: CaseFindingsRepeated, Words: "repeated findings", Rung2: rung2Decides},
	{Kind: CaseDependencyClosed, Words: "closed dependency", NoRung1: aboutTheBoard, Rung2: rung2Decides},
	{Kind: CaseDependencyFailed, Words: "failed dependency", Rung0: []string{"retry_dependency", "impasse"}, NoRung1: aboutTheBoard, Rung2: rung2Decides},
	{Kind: CaseWaitExpired, Words: "expired wait", Rung2: rung2Decides},
	{Kind: CasePodNeverStarted, Words: "pod that never started", Rung2: rung2Decides},
	{Kind: CaseProviderRefused, Words: "provider refusal", Rung2: rung2Decides},
	{Kind: CaseNoAnswer, Words: "missing answer", Rung2: rung2Decides},
	{Kind: CaseContentFilter, Words: "content filter stop", Rung2: rung2Decides},
	{Kind: CaseImageWait, Words: "image wait", Rung2: rung2Decides},
	{Kind: CaseScreeningSuspect, Words: "screening suspect", Rung2: rung2Decides},
	{Kind: CaseAgentEscalate, Words: "agent's escalation", Rung2: rung2Decides},
	{Kind: CaseAgentRequestInfo, Words: "agent's request for information", NoRung1: authorAnswers, Rung2: rung2Nothing, Rung2Why: authorAnswers,
		Exception: authorAnswers},
	{Kind: CasePlanQuestion, Words: "plan question", NoRung1: "the plan guard refuses every plan answer of rung 1", Rung2: rung2Decides},
	{Kind: CasePlanConfirmation, Words: "plan confirmation", NoRung1: "the plan guard refuses every plan answer of rung 1", Rung2: rung2Decides},
	{Kind: CaseMergeApproval, Words: "merge approval", NoRung1: "a merge is the merge gate's question, not one card's session's, which rung 1 reads",
		Rung2: rung2Decides},
	{Kind: CaseHumanCard, Words: "human card", NoRung1: choicesAnswer, Rung2: rung2Decides, Choices: true},
	{Kind: CaseOther, Words: "open problem", Rung2: rung2Decides},
	{Kind: CaseMonitorFlag, Words: "monitor flag", Rung0: []string{"close_ended", "expire_step"}, Rung2: rung2Decides},
	{Kind: CaseJudgmentBudget, Words: "judgment budget", NoRung1: noRungRunsBudget, Rung2: rung2Nothing, Rung2Why: noRungRunsBudget,
		Choices: true, Exception: noRungRunsBudget},
	{Kind: CasePolicyClass, Words: "policy class", Rung0: []string{"route_approval"}, NoRung1: checkinJudged,
		Rung2: rung2Analyses, Rung2Why: "the merge gate marks the policy class human"},
	{Kind: CaseRedMain, Words: "red main", NoRung1: choicesAnswer, Rung2: rung2Decides, Choices: true},
	{Kind: CaseRollHeld, Words: "held roll", NoRung1: choicesAnswer, Rung2: rung2Decides, Choices: true},
	{Kind: CaseConfigRefused, Words: "refused configuration", NoRung1: choicesAnswer, Rung2: rung2Decides, Choices: true},
	{Kind: CasePlanCriteria, Words: "plan's criteria", Rung0: []string{"rerun_criteria_gate"},
		NoRung1: "a plan's criteria are the criteria gate's question, not one card's session's, which rung 1 reads", Rung2: rung2Decides},
	{Kind: CasePlanHeld, Words: "held plan", NoRung1: humanClosedPlan, Rung2: rung2Nothing, Rung2Why: humanClosedPlan, Exception: humanClosedPlan},
	{Kind: CaseProviderFreeze, Words: "provider freeze", NoRung1: limitHoldsRungs, Rung2: rung2Nothing, Rung2Why: limitHoldsRungs,
		Choices: true, Exception: limitHoldsRungs},
	{Kind: CasePlatformFault, Words: "sessions the platform ended", NoRung1: platformNoRung1, Rung2: rung2Decides},
	{Kind: CasePlatformDeaths, Words: "platform deaths", NoRung1: nodesNeedAdmin, Rung2: rung2Nothing, Rung2Why: nodesNeedAdmin,
		Choices: true, Exception: nodesNeedAdmin},
}

// kindRow is kind k's row of the kind table, the zero row for a kind it
// does not hold.
func kindRow(k CaseKind) caseKindRow {
	if i := slices.IndexFunc(caseKinds, func(r caseKindRow) bool { return r.Kind == k }); i >= 0 {
		return caseKinds[i]
	}
	return caseKindRow{}
}

// caseWords says case c of card as a human reads it, with no case id: the
// kind in words, the card, and the problem cut short.
func caseWords(card string, c Case) string {
	kind := cmp.Or(kindRow(c.Kind).Words, strings.ReplaceAll(string(c.Kind), "_", " "), "ask")
	words := "the " + kind
	if card != "" {
		words += " on " + card
	}
	if problem := strings.Join(strings.Fields(c.Problem), " "); problem != "" {
		words += " (\"" + textutil.Truncate(problem, 80) + "\")"
	}
	return words
}

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
// the case it comes after, 0 for none; CommentID is the comment of its newest
// face, read from its face steps, 0 for none. OwnerRung is the rung that
// owns its next move: 0 on an open opening, 3 on one at the human. WakeOn
// are the events that end its wait, and NextWakeAt is when the case-wake
// pass wakes it if none does, nil for never; an opener gives it.
// SetCaseWake sets all three.
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
	// Brief and BriefDetail are the case writer's lines of the story, what
	// happened and what is left; BriefError marks a writer that failed.
	Brief       string
	BriefDetail string
	BriefError  string
}

// CaseMoney is the money a case is about: the budget of kind, what is
// spent of it and what one more session buys.
type CaseMoney struct {
	Kind      string  `json:"kind"`
	BudgetEUR float64 `json:"budget_eur"`
	SpentEUR  float64 `json:"spent_eur"`
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
	// CommentID is the issue comment of the face this step put, 0 on any
	// other step: each face of an ask case is an anchor its comments answer.
	CommentID int64
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
	Room   *RoomAsk       `json:"room,omitempty"`
	Result string         `json:"result,omitempty"`
}

// The results of an action rung 2 did not run: proposed on its own
// authority at a level below autonomous, recommended in an analysis. Both
// reach the face as the preselected action.
const (
	resultProposed    = "proposed"
	resultRecommended = "recommended"
)

// Case step outcomes.
const (
	StepInProgress = "in_progress"
	StepResolved   = "resolved"
	StepHandedUp   = "handed_up"
	StepWithdrawn  = "withdrawn"
)

const caseColumns = `id, task_id, repository, kind, state_key, status, problem, facts, money, case_face_comment(id), COALESCE(follows, 0), opened_at, resolved_at, resolution, owner_rung, next_wake_at, wake_on, brief, brief_detail, brief_error`

func scanCase(row rowScanner) (Case, error) {
	var c Case
	var facts, money, resolution []byte
	// the array arrives as a string through database/sql; pgx's scanner
	// decodes it into the slice
	if err := row.Scan(&c.ID, &c.TaskID, &c.Repository, &c.Kind, &c.StateKey, &c.Status, &c.Problem, &facts, &money,
		&c.CommentID, &c.Follows, &c.OpenedAt, &c.ResolvedAt, &resolution, &c.OwnerRung, &c.NextWakeAt, pgtype.NewMap().SQLScanner(&c.WakeOn), &c.Brief, &c.BriefDetail, &c.BriefError); err != nil {
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
// and changes nothing on it. The case opens open, owned by rung 0 and due
// at c.NextWakeAt; any other status is refused, for only the door
// (ToHuman) moves a case to the human.
func (s *PGStore) OpenCase(ctx context.Context, c Case) (Case, bool, error) {
	return openCase(ctx, s.db, c)
}

func openCase(ctx context.Context, q rowQuerier, c Case) (Case, bool, error) {
	if c.Status = cmp.Or(c.Status, CaseOpen); c.Status != CaseOpen {
		return Case{}, false, errOpenOnly
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
			`INSERT INTO cases (task_id, repository, kind, state_key, status, problem, facts, money, follows, owner_rung, next_wake_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, NULLIF($9::bigint, 0), $10, $11)
			 ON CONFLICT (task_id, kind, state_key) WHERE `+caseLive+` DO NOTHING
			 RETURNING `+caseColumns,
			c.TaskID, c.Repository, c.Kind, c.StateKey, c.Status, c.Problem, facts, money, c.Follows, 0, c.NextWakeAt))
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
		`INSERT INTO case_steps (case_id, rung, actor, read, found, did, decision, grounds, cost_eur, outcome, due_at, comment_id)
		 VALUES ($1, $2, $3, $4::jsonb, $5, $6::jsonb, $7, $8, $9, $10, $11, $12) RETURNING id`,
		st.CaseID, st.Rung, st.Actor, read, st.Found, did, st.Decision, st.Grounds, st.CostEUR, cmp.Or(st.Outcome, StepInProgress), st.DueAt, st.CommentID).Scan(&id)
	return id, err
}

// EndCaseStep ends in-progress step st.ID with st.Outcome and what the
// step found, did, decided and cost; a nil st.Read keeps the list the step
// began with. ErrNotFound when no such step is in progress, as after its
// case ended.
func (s *PGStore) EndCaseStep(ctx context.Context, st CaseStep) error {
	if st.Outcome == StepInProgress {
		return errors.New("EndCaseStep ends a step as resolved, handed_up or withdrawn, not in_progress")
	}
	did, err := toJSON(nonNil(st.Did))
	if err != nil {
		return err
	}
	var read any
	if st.Read != nil {
		if read, err = toJSON(st.Read); err != nil {
			return err
		}
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE case_steps SET outcome = $2, found = $3, did = $4::jsonb, decision = $5, grounds = $6, cost_eur = $7, read = COALESCE($9::jsonb, read), ended_at = now()
		 WHERE id = $1 AND outcome = $8`,
		st.ID, st.Outcome, st.Found, did, st.Decision, st.Grounds, st.CostEUR, StepInProgress, read)
	return oneRow(res, err, fmt.Sprintf("case step %d in progress", st.ID))
}

// AddRungCost adds eur to the cost of case caseID's newest step of rung, the
// turn's spend once the turn that ended it is over.
func (s *PGStore) AddRungCost(ctx context.Context, caseID int64, rung int, eur float64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE case_steps SET cost_eur = cost_eur + $3
		WHERE id = (SELECT max(id) FROM case_steps WHERE case_id = $1 AND rung = $2)`, caseID, rung, eur)
	return oneRow(res, err, fmt.Sprintf("a step of rung %d on case %d", rung, caseID))
}

// errOpenOnly refuses a case opened at any status but open.
var errOpenOnly = errors.New("OpenCase opens a case only as open: the door moves it to the human")

// ToHuman moves live case id to the human, the one writer of at_human:
// owned by rung 3 with no wake, once doorRefusal passes it. A case at the
// human already stays as it is. ErrNotFound when the case is not live.
func (s *PGStore) ToHuman(ctx context.Context, id int64) error {
	return toHuman(ctx, s.db, id)
}

// toHuman is ToHuman inside q, the events writer's transaction for a face's
// ask row and the asks that reach the human at their opening.
func toHuman(ctx context.Context, q rowQuerier, id int64) error {
	c, err := scanCase(q.QueryRowContext(ctx, `SELECT `+caseColumns+` FROM cases WHERE id = $1 AND `+caseLive, id))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("live case %d: %w", id, ErrNotFound)
	}
	if err != nil || c.Status == CaseAtHuman {
		// a case at the human already moves nowhere
		return err
	}
	var climbed bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM case_steps WHERE case_id = $1 AND (rung IN (1, 2)
		OR rung = 0 AND EXISTS (SELECT 1 FROM unnest($2::text[]) mark WHERE starts_with(found, mark))))`, id, notJudgedMarks).Scan(&climbed); err != nil {
		return err
	}
	if err := doorRefusal(c, climbed); err != nil {
		return err
	}
	err = q.QueryRowContext(ctx, `UPDATE cases SET status = $2, owner_rung = 3, next_wake_at = NULL WHERE id = $1 AND `+caseLive+` RETURNING id`,
		id, CaseAtHuman).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("live case %d: %w", id, ErrNotFound)
	}
	return err
}

// doorRefusal is why case c may not reach the human, nil when it may: a
// step lets it through (climbed), or the kind table names its kind an
// exception.
func doorRefusal(c Case, climbed bool) error {
	if climbed || kindRow(c.Kind).Exception != "" {
		return nil
	}
	return fmt.Errorf("case %d (%s) has no rung 1 or rung 2 step and no exception: the case climbs first", c.ID, c.Kind)
}

// notJudgedMarks lead rung 0's step on a case no rung could judge.
var notJudgedMarks = []string{markJudgmentSpent, markJudgmentUnread, markProviderRefused}

// climbed says a step among steps lets its case through the door: one of
// rung 1 or rung 2, or rung 0's that marks the case not judged.
func climbed(steps []CaseStep) bool {
	return slices.ContainsFunc(steps, func(s CaseStep) bool {
		return s.Rung == 1 || s.Rung == 2 ||
			s.Rung == 0 && slices.ContainsFunc(notJudgedMarks, func(mark string) bool { return strings.HasPrefix(s.Found, mark) })
	})
}

func (s *PGStore) SetCaseFact(ctx context.Context, id int64, key, value string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE cases SET facts = CASE WHEN $3::text = '' THEN facts - $2::text
		ELSE jsonb_set(facts, ARRAY[$2::text], to_jsonb($3::text)) END WHERE id = $1 AND `+caseLive, id, key, value)
	return oneRow(res, err, fmt.Sprintf("live case %d", id))
}

func (s *PGStore) SetCaseFacts(ctx context.Context, id int64, facts map[string]any) error {
	b, err := json.Marshal(facts)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE cases SET facts = facts || $2::jsonb WHERE id = $1 AND `+caseLive, id, string(b))
	return oneRow(res, err, fmt.Sprintf("live case %d", id))
}

func (s *PGStore) StartRung1Step(ctx context.Context, taskID string, limit int, st CaseStep) (int64, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	var locked string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id = $1 FOR NO KEY UPDATE`, taskID).Scan(&locked); err != nil {
		return 0, false, err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM case_steps s JOIN cases c ON c.id = s.case_id WHERE c.task_id = $1 AND s.rung = 1`, taskID).Scan(&n); err != nil {
		return 0, false, err
	}
	if n >= limit {
		return 0, true, nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE cases SET owner_rung = $2, wake_on = '{}', next_wake_at = $3 WHERE id = $1 AND `+caseLive, st.CaseID, st.Rung, st.DueAt)
	if err := oneRow(res, err, fmt.Sprintf("live case %d", st.CaseID)); err != nil {
		return 0, false, err
	}
	id, err := appendCaseStep(ctx, tx, st)
	if err != nil {
		return 0, false, err
	}
	return id, false, tx.Commit()
}

// StartRungStep appends st, the in-progress step of a rung that works on its
// case until st.DueAt, and makes that rung the case's owner with its wake at
// the due, in one transaction: no step holds a case the case-wake pass would not wake.
// ErrNotFound when the case is not live.
func (s *PGStore) StartRungStep(ctx context.Context, st CaseStep) (int64, error) {
	var id int64
	err := s.commitRows(ctx, func(tx *sql.Tx) ([]Event, error) {
		res, err := tx.ExecContext(ctx, `UPDATE cases SET owner_rung = $2, wake_on = '{}', next_wake_at = $3 WHERE id = $1 AND `+caseLive,
			st.CaseID, st.Rung, st.DueAt)
		if err := oneRow(res, err, fmt.Sprintf("live case %d", st.CaseID)); err != nil {
			return nil, err
		}
		id, err = appendCaseStep(ctx, tx, st)
		return nil, err
	})
	return id, err
}

// PendingSignal is a case's signal to its card's run that no send delivered
// yet (case_signals): Name and Signal as the run takes them, Tries the
// heal's failed sends, Status the card's.
type PendingSignal struct {
	CaseID int64
	TaskID string
	Name   string
	Signal TaskSignal
	Tries  int
	Status string
	Kind   string
}

// pendCaseSignal writes name, case caseID's signal sig to card taskID's run,
// as pending in tx, the transaction that writes what it announces.
func pendCaseSignal(ctx context.Context, tx *sql.Tx, caseID int64, taskID, name string, sig TaskSignal) error {
	payload, err := toJSON(sig)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO case_signals (case_id, signal, task_id, payload) VALUES ($1, $2, $3, $4::jsonb)
		ON CONFLICT (case_id, signal) DO UPDATE SET payload = EXCLUDED.payload, pending_since = now(), tries = 0`, caseID, name, taskID, payload)
	return err
}

// PendingCaseSignals lists the case signals pending since before at, the
// oldest first, at most limit, with their cards' status and kind.
func (s *PGStore) PendingCaseSignals(ctx context.Context, at time.Time, limit int) ([]PendingSignal, error) {
	return queryAll(ctx, s.db, `SELECT p.case_id, p.task_id, p.signal, p.payload, p.tries, t.status, t.kind
		FROM case_signals p JOIN tasks t ON t.id = p.task_id WHERE p.pending_since < $1 ORDER BY p.pending_since, p.case_id LIMIT $2`,
		[]any{at, limit}, func(row rowScanner) (p PendingSignal, err error) {
			var payload []byte
			if err := row.Scan(&p.CaseID, &p.TaskID, &p.Name, &payload, &p.Tries, &p.Status, &p.Kind); err != nil {
				return p, err
			}
			return p, json.Unmarshal(payload, &p.Signal)
		})
}

// SettleCaseSignal ends case caseID's pending signal name when done, and
// else counts a failed send of it. No such row is no error.
func (s *PGStore) SettleCaseSignal(ctx context.Context, caseID int64, name string, done bool) error {
	q := `UPDATE case_signals SET tries = tries + 1 WHERE case_id = $1 AND signal = $2`
	if done {
		q = `DELETE FROM case_signals WHERE case_id = $1 AND signal = $2`
	}
	_, err := s.db.ExecContext(ctx, q, caseID, name)
	return err
}

// SetCaseWake sets who owns the next move on live case id, the events that
// end its wait, and when the case-wake pass wakes it if none does; a nil at clears
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
// found before, or what its rung took since, and answers them, oldest
// first.
func (s *PGStore) ExpireSteps(ctx context.Context, now time.Time) ([]CaseStep, error) {
	// the constant outcome lets the partial index case_steps_due serve
	rows, err := s.db.QueryContext(ctx, `SELECT s.id, s.rung, s.started_at, s.due_at, s.found,
		COALESCE((SELECT r.decision FROM case_steps r WHERE r.case_id = s.case_id AND r.rung = s.rung AND r.id > s.id AND r.outcome = '`+StepResolved+`'
			ORDER BY r.id DESC LIMIT 1), '')
		FROM case_steps s WHERE s.outcome = '`+StepInProgress+`' AND s.due_at < $1`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	var found []string
	for rows.Next() {
		var id int64
		var rung int
		var started, due time.Time
		var before, took string
		if err := rows.Scan(&id, &rung, &started, &due, &before, &took); err != nil {
			return nil, err
		}
		ids, found = append(ids, id), append(found, stepExpired(before, rung, took, started, due))
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

// stepExpired is the Found of a step of rung that ended at due, after
// found, what the step found before: its bound, due less its start, to the
// minute as DurationText writes it, which its rung did not answer within,
// or after which the card still waits on took, the choice the rung took
// since (tookSince).
func stepExpired(found string, rung int, took string, started, due time.Time) string {
	expired := expiredWords(rung, took, due.Sub(started))
	if found == "" {
		return expired
	}
	return found + ", then " + expired
}

// expiredWords say how a step of rung ended at its bound, after bound: its
// rung did not answer, or it took took, a choice that keeps the card
// waiting.
func expiredWords(rung int, took string, bound time.Duration) string {
	if took != "" {
		return fmt.Sprintf("rung %d took %s and the card still waits after %s", rung, took, DurationText(bound.Round(time.Minute)))
	}
	return expiredMark + DurationText(bound.Round(time.Minute))
}

// tookSince is the decision of the newest step of st's rung that resolved
// after st on its case, among steps: a choice that kept the card waiting,
// such as rerun_again. "" for none.
func tookSince(steps []CaseStep, st CaseStep) string {
	for k := len(steps) - 1; k >= 0; k-- {
		if s := steps[k]; s.CaseID == st.CaseID && s.Rung == st.Rung && s.ID > st.ID && s.Outcome == StepResolved {
			return s.Decision
		}
	}
	return ""
}

// DurationText is d, whole minutes, as Go prints a duration less its zero
// units: 35m, 1h30m, 2h.
func DurationText(d time.Duration) string {
	s := d.String()
	if m, ok := strings.CutSuffix(s, "m0s"); ok {
		s = m + "m"
	}
	if h, ok := strings.CutSuffix(s, "h0m"); ok {
		s = h + "h"
	}
	return s
}

// caseAnswer is the action of a case an answer row resolved: an answer
// comment, or a press that names no action.
const CaseAnswer = "answer"

// ErrCaseEnded refuses an answer on a case that ended: Case says how.
type ErrCaseEnded struct {
	Case Case
	Card string
}

func (e ErrCaseEnded) Error() string {
	c := e.Case
	words := caseWords(cmp.Or(e.Card, c.TaskID), Case{Kind: c.Kind})
	if c.Resolution == nil || c.ResolvedAt == nil {
		return fmt.Sprintf("%s is %s", words, c.Status)
	}
	at := c.ResolvedAt.UTC().Format("15:04")
	if c.Status == CaseWithdrawn {
		return fmt.Sprintf("%s was withdrawn at %s UTC: %s", words, at, c.Resolution.Reason)
	}
	return fmt.Sprintf("%s was resolved by %s with %s at %s UTC", words, c.Resolution.By, c.Resolution.Action, at)
}

// CaseDraw is what an answer on Rung draws beside its resolution: a raise
// of the card's ceiling by Raise (0 for none), then Extend's sessions and
// agent time, then FundEUR into the budget, which the card's
// ceiling bounds, else PolicyCeiling. CardBound and Pool are the card's
// draw bound, which rungs 0 and 1 draw within, and the project's month of
// draws, which rungs 0 to 2 draw within. Below are the rooms of the counts
// under the one asked, with the money as asked, smallest first, among
// which a refusal past a bound names the largest that fits; none for a
// draw no room made. RunGrants: the card's run grants
// Extend's sessions and agent time itself, so the rows record them against
// the pool and the card's row raises neither. Event is the rows'
// repository, issue, actor, via and detail.
type CaseDraw struct {
	Rung          int
	Raise         float64
	Extend        policy.Draw
	FundEUR       float64
	PolicyCeiling float64
	CardBound     policy.Draw
	Pool          policy.Draw
	Below         []Room
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
// by r. A step st that names an in-progress step (ID set) is that step
// ended with st's content, not a second step. sig, nil for none, is the
// case_resolved the card's run is told, pending until a send delivers it.
// A case that ended otherwise is ErrCaseEnded, no case ErrNotFound.
func (s *PGStore) ResolveCase(ctx context.Context, id int64, r CaseResolution, d *CaseDraw, st *CaseStep, sig *TaskSignal) error {
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
			return nil, ErrCaseEnded{Case: c}
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
		if sig != nil {
			if err := pendCaseSignal(ctx, tx, id, taskID, SignalCaseResolved, *sig); err != nil {
				return nil, err
			}
		}
		if st != nil {
			step := *st
			step.CaseID = id
			if step.ID != 0 {
				if err := endOwnStep(ctx, tx, step); err != nil {
					return nil, err
				}
			} else if _, err := appendCaseStep(ctx, tx, step); err != nil {
				return nil, err
			}
		}
		return rows, nil
	})
}

// endOwnStep writes what step st did over the row of the in-progress step
// it names, which the case's end ended with its outcome alone.
func endOwnStep(ctx context.Context, tx *sql.Tx, st CaseStep) error {
	read, err := toJSON(nonNil(st.Read))
	if err != nil {
		return err
	}
	did, err := toJSON(nonNil(st.Did))
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE case_steps SET outcome = $3, read = $4::jsonb, found = $5, did = $6::jsonb, decision = $7, grounds = $8, cost_eur = $9, actor = $10, ended_at = now()
		 WHERE id = $1 AND case_id = $2`,
		st.ID, st.CaseID, st.Outcome, read, st.Found, did, st.Decision, st.Grounds, st.CostEUR, st.Actor)
	return oneRow(res, err, fmt.Sprintf("case step %d", st.ID))
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
	if !d.Extend.IsZero() {
		_, _, stored, err := s.extendCard(ctx, tx, taskID, d.Extend, d.RunGrants, extend)
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
		id, status, b, outcome, StepInProgress, answered, CaseResolved, CaseAnswer).Scan(&taskID)
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

// OpenCasesOfKinds are the open and at_human cases of kinds in every
// repository whose card has not ended, oldest first.
func (s *PGStore) OpenCasesOfKinds(ctx context.Context, kinds ...CaseKind) ([]Case, error) {
	return s.cases(ctx, `SELECT `+caseColumns+` FROM cases c WHERE `+caseLive+` AND kind = ANY($1::text[])
		AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.id = c.task_id AND t.status = ANY($2)) ORDER BY id`, kindNames(kinds), FinalStatuses)
}

// kindNames are kinds as the text a query's array parameter takes.
func kindNames(kinds []CaseKind) []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
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

const caseStepColumns = `id, case_id, rung, actor, started_at, ended_at, read, found, did, decision, grounds, cost_eur::float8, outcome, due_at, comment_id`

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
			&st.Decision, &st.Grounds, &st.CostEUR, &st.Outcome, &st.DueAt, &st.CommentID); err != nil {
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
