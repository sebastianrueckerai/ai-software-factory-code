package factoryd

import (
	"context"
	"time"
)

// BoardTask is a task on the console's board with the reason of its
// latest escalation ("" when it never escalated), its open escalation
// alert (0 when none awaits a verdict), a card in waiting-deps the issues
// it waits on (WaitingOn: "#3, #5"), and the entries ahead of it in its
// merge queue.
type BoardTask struct {
	Task
	Escalation      string
	EscalationAlert int64
	WaitingOn       string
	QueueAhead      int
}

// BoardTasks lists at most limit tasks for the console's board: those not
// done or failed first, most urgent first, then those finished after
// since, newest first. The escalation reason comes from the escalation's
// intake event, so a poll reads no timeline object.
func (s *PGStore) BoardTasks(ctx context.Context, since time.Time, limit int) ([]BoardTask, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+taskColumns+`, COALESCE((SELECT e.body FROM intake_events e
		        WHERE e.task_id = tasks.id AND e.kind = $2 ORDER BY e.id DESC LIMIT 1), ''),
		        COALESCE((SELECT a.id FROM alerts a
		        WHERE a.task_id = tasks.id AND a.kind = $4 AND a.verdict IS NULL ORDER BY a.id DESC LIMIT 1), 0),
		        CASE WHEN status = 'waiting-deps' THEN COALESCE((SELECT string_agg('#' || d.issue_number, ', ' ORDER BY d.issue_number)
		        FROM task_deps td JOIN tasks d ON d.id = td.depends_on WHERE td.task_id = tasks.id AND d.status <> 'done'), '') ELSE '' END,
		        (SELECT count(*) FROM merge_queue me JOIN merge_queue o ON o.project = me.project
		        AND (o.entered_at, o.task_id) < (me.entered_at, me.task_id) WHERE me.task_id = tasks.id)
		 FROM tasks WHERE status NOT IN ('done', 'failed', 'closed') OR finished_at > $1
		 ORDER BY status IN ('done', 'failed', 'closed'), priority DESC, finished_at DESC NULLS FIRST, id
		 LIMIT $3`, since, IntakeEscalation, limit, AlertEscalation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []BoardTask
	for rows.Next() {
		var b BoardTask
		t, err := scanTask(func(dest ...any) error {
			return rows.Scan(append(dest, &b.Escalation, &b.EscalationAlert, &b.WaitingOn, &b.QueueAhead)...)
		})
		if err != nil {
			return nil, err
		}
		b.Task = t
		tasks = append(tasks, b)
	}
	return tasks, rows.Err()
}
