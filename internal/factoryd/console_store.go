package factoryd

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// BoardTask is a task on the console's board with the reason of its
// latest escalation ("" when it never escalated) and the Choices its agent
// offered with that question, its open escalation alert (0 when none
// awaits a verdict), a card in waiting-deps the issues it waits on
// (WaitingOn: "#3, #5"), in any status the issues of the
// cards it depends on that are not done (Dependencies: "#3, #5", for the
// face's "blocked by n" chip), and the entries ahead of it in its merge
// queue. Question is the escalation's five-part question when a template
// shaped it (nil on the older and unshaped rows); AnswersPending counts
// the trusted answers the card's workflow has not taken: a card that
// holds some cannot process another, so its answer box yields to the
// liveness warning. ChainState is the verdict of the newest run that
// carries the card's pull request ("" for none) — its commit's AND of
// runs and required contexts: a merged card reads it as its chain word
// (roadmap 297), the chain's own fact, no roll in it.
// ChainLive is deployLive read as a column: whether the card's change runs
// on the cluster — a green run covers it, it never opened a pull request,
// or it finished before the chain began recording — the fact a finished
// card's own page waits on.
type BoardTask struct {
	Task
	Escalation      string
	EscalatedAt     time.Time
	Choices         []string
	EscalationAlert int64
	WaitingOn       string
	Dependencies    string
	QueueAhead      int
	Question        *HumanQuestion
	AnswersPending  int
	ChainState      string
	ChainLive       bool
	// RollID and RollIssue name the roll task that carries a merged card's
	// pull request in an opened or merged request; "" and 0 for none.
	RollID    string
	RollIssue int64
}

// deployLive is the SQL test that a finished card's change runs on the
// cluster (roadmap 297): a card that never opened a pull request, or that
// a green run covers — the run that carried its change is recorded and
// the newest green run on main, the head row or any card's carry row,
// sits at or after it, so a green run later than its own red carry
// deploys it — or that no chain names at all and that finished before the
// chain began recording runs (none recorded yet: every card, while none
// is; a done card only — a merged one is live through coverage alone).
// A carry run with no green run at or after it keeps the card
// waiting; every row's status is its commit's verdict — the runs AND
// the required contexts — so coverage demands them too.
const deployLive = `(tasks.pr_number = 0
	OR ` + chainCovers + `
	OR tasks.status = 'done'
	   AND NOT EXISTS (SELECT 1 FROM chain_runs cr WHERE cr.repository = tasks.repository AND cr.pr_number = tasks.pr_number)
	   AND COALESCE((SELECT min(updated_at) FROM chain_runs WHERE repository = tasks.repository), 'infinity'::timestamptz) > COALESCE(tasks.finished_at, tasks.created_at))`

// chainGreen is the newest green run of the card's repository, 0 for none.
const chainGreen = `COALESCE((SELECT max(green.run_id) FROM chain_runs green WHERE green.repository = tasks.repository AND green.status = 'success'), 0)`

// chainCovers is the SQL test that a green run covers the card's change:
// the run that carried it is recorded and the newest green run sits at or
// after it. deployLive and the reconciler's ChainCovered both read it.
const chainCovers = `(EXISTS (SELECT 1 FROM chain_runs carry WHERE carry.repository = tasks.repository AND carry.pr_number = tasks.pr_number)
	AND ` + chainGreen + `
	   >= (SELECT min(mine.run_id) FROM chain_runs mine WHERE mine.repository = tasks.repository AND mine.pr_number = tasks.pr_number))`

// rollCarries is the SQL test that request rr lists the card's pull request.
const rollCarries = `tasks.pr_number > 0 AND rr.repository = tasks.repository AND rr.prs @> ARRAY[tasks.pr_number]::bigint[]`

// boardTaskQuery reads tasks as BoardTasks with $1 the escalation's intake
// kind and $2 the escalation alert's kind; the caller adds the WHERE.
const boardTaskQuery = `SELECT ` + boardTaskColumns + boardTaskFrom

// boardTaskColumns and boardTaskFrom are boardTaskQuery's two halves, so
// FinishedTasks reads one column more.
const boardTaskColumns = taskColumns + `, COALESCE(esc.body, ''), esc.at, esc.choices,
		        COALESCE((SELECT a.id FROM alerts a
		        WHERE a.task_id = tasks.id AND a.kind = $2 AND a.verdict IS NULL ORDER BY a.id DESC LIMIT 1), 0),
		        CASE WHEN status = 'waiting-deps' THEN COALESCE((SELECT string_agg('#' || d.issue_number, ', ' ORDER BY d.issue_number)
		        FROM task_deps td JOIN tasks d ON d.id = td.depends_on WHERE td.task_id = tasks.id AND d.status <> 'done'), '') ELSE '' END,
		        COALESCE((SELECT string_agg('#' || d.issue_number, ', ' ORDER BY d.issue_number)
		        FROM task_deps td JOIN tasks d ON d.id = td.depends_on WHERE td.task_id = tasks.id AND d.issue_number > 0 AND d.status <> 'done'), ''),
		        (SELECT count(*) FROM merge_queue me JOIN merge_queue o ON o.project = me.project
		        AND (o.entered_at, o.task_id) < (me.entered_at, me.task_id) WHERE me.task_id = tasks.id),
		        esc.question,
		        (SELECT count(*) FROM intake_events ans WHERE ans.task_id = tasks.id AND ans.kind = 'issue_comment'
		        AND ans.trusted AND NOT ans.signalled AND ans.id > COALESCE(esc.esc_id, 0)),
		        COALESCE((SELECT cr.status FROM chain_runs cr WHERE tasks.pr_number > 0 AND cr.repository = tasks.repository AND cr.pr_number = tasks.pr_number
		        ORDER BY cr.run_id DESC LIMIT 1), ''),
		        (` + deployLive + `),
		        COALESCE(rl.roll_id, ''), COALESCE(rl.roll_issue, 0)`

const boardTaskFrom = `
		 FROM tasks LEFT JOIN LATERAL (SELECT roll.id AS roll_id, roll.issue_number AS roll_issue FROM roll_requests rr JOIN tasks roll ON roll.id = rr.task_id
		        WHERE tasks.status = 'merged' AND ` + rollCarries + ` AND rr.state IN ('opened', 'merged')
		        ORDER BY rr.created_at DESC LIMIT 1) rl ON true
		 LEFT JOIN LATERAL (SELECT e.id AS esc_id, e.body, e.created_at AS at, e.choices, e.question FROM intake_events e
		        WHERE e.task_id = tasks.id AND e.kind = $1 ORDER BY e.id DESC LIMIT 1) esc ON true`

func scanBoardTask(scan func(dest ...any) error) (BoardTask, error) {
	var b BoardTask
	var at sql.NullTime
	var choices, question []byte
	t, err := scanTask(func(dest ...any) error {
		return scan(append(dest, &b.Escalation, &at, &choices, &b.EscalationAlert, &b.WaitingOn, &b.Dependencies, &b.QueueAhead, &question, &b.AnswersPending, &b.ChainState, &b.ChainLive, &b.RollID, &b.RollIssue)...)
	})
	if err != nil {
		return b, err
	}
	if len(question) > 0 {
		q := &HumanQuestion{}
		if err := json.Unmarshal(question, q); err != nil {
			return b, err
		}
		b.Question = q
	}
	b.Task, b.EscalatedAt = t, at.Time
	return b, unmarshalChoices(choices, &b.Choices)
}

// BoardTasks lists repository's open tasks for the console's board, most
// urgent first; a chat task is no card. A merged task is open: its card
// waits until the chain runs its change live. The escalation reason comes
// from the escalation's intake event, so a poll reads no timeline object.
func (s *PGStore) BoardTasks(ctx context.Context, repository string) ([]BoardTask, error) {
	rows, err := s.db.QueryContext(ctx, boardTaskQuery+`
		 WHERE repository = $4 AND kind <> $3 AND status NOT IN ('done', 'failed', 'closed')
		 ORDER BY priority DESC, finished_at DESC NULLS LAST, tasks.id`, IntakeEscalation, AlertEscalation, KindChat, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []BoardTask
	for rows.Next() {
		b, err := scanBoardTask(rows.Scan)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, b)
	}
	return tasks, rows.Err()
}

// RollCarries are the cards the requests of roll task id carry: those
// whose pull request one of them lists, by issue.
func (s *PGStore) RollCarries(ctx context.Context, id string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE EXISTS (SELECT 1 FROM roll_requests rr
		WHERE rr.task_id = $1 AND `+rollCarries+`) ORDER BY tasks.issue_number, tasks.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// FinishedQuery asks for a page of one project's finished cards on the
// board: those in Statuses finished after Since, or failed holding a plan
// key while a card waits on them (their Retry), by arrival, newest first
// (oldest first with Oldest), ties by id; the page after the card After
// names, at most Limit of them, all for 0.
type FinishedQuery struct {
	Repository string
	Statuses   []string
	Since      time.Time
	Oldest     bool
	After      Cursor
	Limit      int
}

// Cursor names the card a page of finished cards ends with: its arrival
// and its id; the zero Cursor is before the first.
type Cursor struct {
	At time.Time
	ID string
}

// CursorOf is the cursor of t, the last card of a page.
func CursorOf(t Task) Cursor { return Cursor{At: arrival(t), ID: t.ID} }

// arrival is when finished card t arrived in its column, as the console
// orders it: its finish stamp, else its filing.
func arrival(t Task) time.Time {
	if t.FinishedAt != nil {
		return *t.FinishedAt
	}
	return t.CreatedAt
}

// FinishedTasks lists the page q asks for, and how many cards the read
// leaves from the page on, the page's own included; a chat task is no
// card, nor a finished human task. A done card whose change is not live
// yet is no finished card: it waits in the board's progress (roadmap 296).
func (s *PGStore) FinishedTasks(ctx context.Context, q FinishedQuery) ([]BoardTask, int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+boardTaskColumns+`, count(*) OVER ()`+boardTaskFrom+`
		 WHERE repository = $3 AND status = ANY($4) AND kind NOT IN ($5, 'human')
		   AND (finished_at > $6 OR status = 'failed' AND plan_key IS NOT NULL AND EXISTS (SELECT 1 FROM task_deps td JOIN tasks w ON w.id = td.task_id
		       WHERE td.depends_on = tasks.id AND w.status = 'waiting-deps'))
		   AND (status <> 'done' OR `+deployLive+`)
		   AND ($8 = '' OR COALESCE(finished_at, created_at) = $7 AND tasks.id > $8
		       OR CASE WHEN $9 THEN COALESCE(finished_at, created_at) > $7 ELSE COALESCE(finished_at, created_at) < $7 END)
		 ORDER BY CASE WHEN $9 THEN COALESCE(finished_at, created_at) END, COALESCE(finished_at, created_at) DESC, tasks.id
		 LIMIT NULLIF($10, 0)`,
		IntakeEscalation, AlertEscalation, q.Repository, q.Statuses, KindChat, q.Since, q.After.At, q.After.ID, q.Oldest, q.Limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var tasks []BoardTask
	rest := 0
	for rows.Next() {
		b, err := scanBoardTask(func(dest ...any) error { return rows.Scan(append(dest, &rest)...) })
		if err != nil {
			return nil, 0, err
		}
		tasks = append(tasks, b)
	}
	return tasks, rest, rows.Err()
}

// BoardTask is task id as BoardTasks reads it, whether or not the board
// lists it; false when there is no such task, or it is a chat task.
func (s *PGStore) BoardTask(ctx context.Context, id string) (BoardTask, bool, error) {
	b, err := scanBoardTask(s.db.QueryRowContext(ctx, boardTaskQuery+` WHERE tasks.id = $3 AND kind <> $4`,
		IntakeEscalation, AlertEscalation, id, KindChat).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return BoardTask{}, false, nil
	}
	return b, err == nil, err
}

// SortBoardTasks orders open tasks in memory as BoardTasks' ORDER BY
// does: most urgent first, then by finish time, newest first, a finish
// stamp missing last. The in-memory fakes sort with it, so the test path
// and the store cannot drift apart; ties keep their order, where the SQL
// tie-breaks by id.
func SortBoardTasks(tasks []BoardTask) {
	slices.SortStableFunc(tasks, func(a, b BoardTask) int {
		return cmp.Or(cmp.Compare(b.Priority, a.Priority), byFinish(a.Task, b.Task))
	})
}

// FinishedPage is FinishedTasks over tasks in memory, for the fakes,
// which keep no dependencies: a card without a finish stamp is in the
// window. A done card whose change is not live yet is no finished card, as
// FinishedTasks reads it; the fakes model a chain row as the task's
// ChainState at its arrival.
func FinishedPage(tasks []BoardTask, q FinishedQuery) ([]BoardTask, int) {
	order := func(a, b Task) int {
		by := arrival(b).Compare(arrival(a))
		if q.Oldest {
			by = -by
		}
		return cmp.Or(by, cmp.Compare(a.ID, b.ID))
	}
	var out []BoardTask
	for _, t := range tasks {
		if t.Repository == q.Repository && slices.Contains(q.Statuses, t.Status) && t.Kind != KindChat && t.Kind != "human" &&
			(t.FinishedAt == nil || t.FinishedAt.After(q.Since)) && (q.After.ID == "" || order(Task{ID: q.After.ID, FinishedAt: &q.After.At}, t.Task) < 0) &&
			(t.Status != "done" || DeployLive(tasks, t)) {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b BoardTask) int { return order(a.Task, b.Task) })
	rest := len(out)
	if q.Limit > 0 {
		out = out[:min(q.Limit, len(out))]
	}
	return out, rest
}

// byFinish compares two cards by their finish stamps, newest first, a
// stamp missing last.
func byFinish(a, b Task) int {
	switch {
	case a.FinishedAt == nil && b.FinishedAt == nil:
		return 0
	case a.FinishedAt == nil:
		return 1
	case b.FinishedAt == nil:
		return -1
	}
	return b.FinishedAt.Compare(*a.FinishedAt)
}

// DeployLive is deployLive for the fakes: a card without a pull request,
// or whose chain verdict went green, or that no chain names and that
// arrived before the first chain the set records. A chain that names it
// as not green keeps it waiting.
func DeployLive(tasks []BoardTask, t BoardTask) bool {
	if t.PRNumber == 0 || t.ChainState == "success" {
		return true
	}
	if t.ChainState != "" {
		return false
	}
	for _, o := range tasks {
		if o.ChainState != "" && !arrival(o.Task).After(arrival(t.Task)) {
			return false
		}
	}
	return true
}
