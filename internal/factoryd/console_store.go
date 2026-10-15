package factoryd

import (
	"cmp"
	"context"
	"database/sql"
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
// queue.
type BoardTask struct {
	Task
	Escalation      string
	EscalatedAt     time.Time
	Choices         []string
	EscalationAlert int64
	WaitingOn       string
	Dependencies    string
	QueueAhead      int
}

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
		        AND (o.entered_at, o.task_id) < (me.entered_at, me.task_id) WHERE me.task_id = tasks.id)`

const boardTaskFrom = `
		 FROM tasks LEFT JOIN LATERAL (SELECT e.body, e.created_at AS at, e.choices FROM intake_events e
		        WHERE e.task_id = tasks.id AND e.kind = $1 ORDER BY e.id DESC LIMIT 1) esc ON true`

func scanBoardTask(scan func(dest ...any) error) (BoardTask, error) {
	var b BoardTask
	var at sql.NullTime
	var choices []byte
	t, err := scanTask(func(dest ...any) error {
		return scan(append(dest, &b.Escalation, &at, &choices, &b.EscalationAlert, &b.WaitingOn, &b.Dependencies, &b.QueueAhead)...)
	})
	if err != nil {
		return b, err
	}
	b.Task, b.EscalatedAt = t, at.Time
	return b, unmarshalChoices(choices, &b.Choices)
}

// BoardTasks lists repository's open tasks for the console's board, most
// urgent first; a chat task is no card. The escalation reason comes from
// the escalation's intake event, so a poll reads no timeline object.
func (s *PGStore) BoardTasks(ctx context.Context, repository string) ([]BoardTask, error) {
	rows, err := s.db.QueryContext(ctx, boardTaskQuery+`
		 WHERE repository = $4 AND kind <> $3 AND status NOT IN ('done', 'failed', 'closed')
		 ORDER BY priority DESC, finished_at DESC NULLS LAST, id`, IntakeEscalation, AlertEscalation, KindChat, repository)
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
// card, nor a finished human task.
func (s *PGStore) FinishedTasks(ctx context.Context, q FinishedQuery) ([]BoardTask, int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+boardTaskColumns+`, count(*) OVER ()`+boardTaskFrom+`
		 WHERE repository = $3 AND status = ANY($4) AND kind NOT IN ($5, 'human')
		   AND (finished_at > $6 OR status = 'failed' AND plan_key IS NOT NULL AND EXISTS (SELECT 1 FROM task_deps td JOIN tasks w ON w.id = td.task_id
		       WHERE td.depends_on = tasks.id AND w.status = 'waiting-deps'))
		   AND ($8 = '' OR COALESCE(finished_at, created_at) = $7 AND id > $8
		       OR CASE WHEN $9 THEN COALESCE(finished_at, created_at) > $7 ELSE COALESCE(finished_at, created_at) < $7 END)
		 ORDER BY CASE WHEN $9 THEN COALESCE(finished_at, created_at) END, COALESCE(finished_at, created_at) DESC, id
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
// window.
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
			(t.FinishedAt == nil || t.FinishedAt.After(q.Since)) && (q.After.ID == "" || order(Task{ID: q.After.ID, FinishedAt: &q.After.At}, t.Task) < 0) {
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
