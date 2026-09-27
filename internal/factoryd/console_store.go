package factoryd

import (
	"context"
	"time"
)

// BoardTasks lists every task not done or failed, and those finished after
// since: the console's board, most urgent first.
func (s *PGStore) BoardTasks(ctx context.Context, since time.Time) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE status NOT IN ('done', 'failed') OR finished_at > $1
		 ORDER BY priority DESC, finished_at DESC NULLS FIRST, id`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		t, err := scanTask(rows.Scan)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}
