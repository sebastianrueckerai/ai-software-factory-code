package factoryd

import (
	"context"
	"time"
)

// BoardTask is a task on the console's board with the reason of its
// latest escalation ("" when it never escalated).
type BoardTask struct {
	Task
	Escalation string
}

// BoardTasks lists at most limit tasks for the console's board: those not
// done or failed first, most urgent first, then those finished after
// since, newest first. The escalation reason comes from the escalation's
// intake event, so a poll reads no timeline object.
func (s *PGStore) BoardTasks(ctx context.Context, since time.Time, limit int) ([]BoardTask, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+taskColumns+`, COALESCE((SELECT e.body FROM intake_events e
		        WHERE e.task_id = tasks.id AND e.kind = $2 ORDER BY e.id DESC LIMIT 1), '')
		 FROM tasks WHERE status NOT IN ('done', 'failed') OR finished_at > $1
		 ORDER BY status IN ('done', 'failed'), priority DESC, finished_at DESC NULLS FIRST, id
		 LIMIT $3`, since, IntakeEscalation, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []BoardTask
	for rows.Next() {
		var b BoardTask
		t, err := scanTask(func(dest ...any) error { return rows.Scan(append(dest, &b.Escalation)...) })
		if err != nil {
			return nil, err
		}
		b.Task = t
		tasks = append(tasks, b)
	}
	return tasks, rows.Err()
}
