package factoryd

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// BoardTask is a task on the console's board with the reason of its
// latest escalation ("" when it never escalated), its open escalation
// alert (0 when none awaits a verdict), a card in waiting-deps the issues
// it waits on (WaitingOn: "#3, #5"), in any status the issues of the
// cards it depends on that are not done (Dependencies: "#3, #5", for the
// face's "blocked by n" chip), and the entries ahead of it in its merge
// queue.
type BoardTask struct {
	Task
	Escalation      string
	EscalatedAt     time.Time
	EscalationAlert int64
	WaitingOn       string
	Dependencies    string
	QueueAhead      int
}

// boardTaskQuery reads tasks as BoardTasks with $1 the escalation's intake
// kind and $2 the escalation alert's kind; the caller adds the WHERE.
const boardTaskQuery = `SELECT ` + taskColumns + `, COALESCE(esc.body, ''), esc.at,
		        COALESCE((SELECT a.id FROM alerts a
		        WHERE a.task_id = tasks.id AND a.kind = $2 AND a.verdict IS NULL ORDER BY a.id DESC LIMIT 1), 0),
		        CASE WHEN status = 'waiting-deps' THEN COALESCE((SELECT string_agg('#' || d.issue_number, ', ' ORDER BY d.issue_number)
		        FROM task_deps td JOIN tasks d ON d.id = td.depends_on WHERE td.task_id = tasks.id AND d.status <> 'done'), '') ELSE '' END,
		        COALESCE((SELECT string_agg('#' || d.issue_number, ', ' ORDER BY d.issue_number)
		        FROM task_deps td JOIN tasks d ON d.id = td.depends_on WHERE td.task_id = tasks.id AND d.issue_number > 0 AND d.status <> 'done'), ''),
		        (SELECT count(*) FROM merge_queue me JOIN merge_queue o ON o.project = me.project
		        AND (o.entered_at, o.task_id) < (me.entered_at, me.task_id) WHERE me.task_id = tasks.id)
		 FROM tasks LEFT JOIN LATERAL (SELECT e.body, e.created_at AS at FROM intake_events e
		        WHERE e.task_id = tasks.id AND e.kind = $1 ORDER BY e.id DESC LIMIT 1) esc ON true`

func scanBoardTask(scan func(dest ...any) error) (BoardTask, error) {
	var b BoardTask
	var at sql.NullTime
	t, err := scanTask(func(dest ...any) error {
		return scan(append(dest, &b.Escalation, &at, &b.EscalationAlert, &b.WaitingOn, &b.Dependencies, &b.QueueAhead)...)
	})
	b.Task, b.EscalatedAt = t, at.Time
	return b, err
}

// BoardTasks lists at most limit tasks for the console's board: those not
// done or failed first, most urgent first, then those finished after
// since, or failed holding a plan key while a card waits on them (their
// Retry), newest first; a chat task is no card. The escalation reason
// comes from the escalation's intake event, so a poll reads no timeline
// object.
func (s *PGStore) BoardTasks(ctx context.Context, since time.Time, limit int) ([]BoardTask, error) {
	rows, err := s.db.QueryContext(ctx, boardTaskQuery+`
		 WHERE kind <> $5 AND (status NOT IN ('done', 'failed', 'closed') OR finished_at > $3
		    OR status = 'failed' AND plan_key IS NOT NULL AND EXISTS (SELECT 1 FROM task_deps td JOIN tasks w ON w.id = td.task_id
		       WHERE td.depends_on = tasks.id AND w.status = 'waiting-deps'))
		 ORDER BY status IN ('done', 'failed', 'closed'), priority DESC, finished_at DESC NULLS FIRST, id
		 LIMIT $4`, IntakeEscalation, AlertEscalation, since, limit, KindChat)
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
