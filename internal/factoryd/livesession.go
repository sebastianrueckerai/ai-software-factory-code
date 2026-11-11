package factoryd

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// LiveSession is a running session's sandbox: the pod a factoryd that
// restarts reattaches to (roadmap 222, item 2), the token its message port
// takes, the deadline the session keeps, and how many of the pod's log
// lines the session loop consumed, so the reattached loop reads on from
// there.
type LiveSession struct {
	TaskID, SessionID, Namespace, Pod, MessageToken string
	// Owner names the RunSession attempt that runs the session: a retry
	// that reattaches takes the row over, and the attempt it replaced,
	// which Temporal may have timed out while its goroutine still runs,
	// leaves the pod, the namespace and the credentials alone.
	Owner string
	// Activity is the RunSession activity the session belongs to, its
	// workflow run and activity id, the same across the activity's retries:
	// only that activity reattaches to the pod and reads its result.
	Activity            string
	StartedAt, Deadline time.Time
	Lines               int
	// SpentAtStart is the card's enforced spend when the session started;
	// nil where it could not be read.
	SpentAtStart *float64
}

func (s *PGStore) PutLiveSession(ctx context.Context, l LiveSession) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO live_sessions (task_id, session_id, namespace, pod, message_token, started_at, deadline, lines, spent_at_start, owner, activity)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (task_id) DO UPDATE SET session_id = EXCLUDED.session_id, namespace = EXCLUDED.namespace, pod = EXCLUDED.pod,
		message_token = EXCLUDED.message_token, started_at = EXCLUDED.started_at, deadline = EXCLUDED.deadline, lines = EXCLUDED.lines,
		spent_at_start = EXCLUDED.spent_at_start, owner = EXCLUDED.owner, activity = EXCLUDED.activity`,
		l.TaskID, l.SessionID, l.Namespace, l.Pod, l.MessageToken, l.StartedAt, l.Deadline, l.Lines, l.SpentAtStart, l.Owner, l.Activity)
	return err
}

func (s *PGStore) SetLiveSessionLines(ctx context.Context, taskID string, lines int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE live_sessions SET lines = $2 WHERE task_id = $1`, taskID, lines)
	return err
}

func (s *PGStore) LiveSession(ctx context.Context, taskID string) (LiveSession, bool, error) {
	var l LiveSession
	err := s.db.QueryRowContext(ctx, `SELECT task_id, session_id, namespace, pod, message_token, started_at, deadline, lines, spent_at_start, owner, activity
		FROM live_sessions WHERE task_id = $1`, taskID).Scan(&l.TaskID, &l.SessionID, &l.Namespace, &l.Pod, &l.MessageToken, &l.StartedAt, &l.Deadline, &l.Lines, &l.SpentAtStart, &l.Owner, &l.Activity)
	if errors.Is(err, sql.ErrNoRows) {
		return LiveSession{}, false, nil
	}
	return l, err == nil, err
}

func (s *PGStore) ReleaseLiveSession(ctx context.Context, taskID, owner string) (bool, error) {
	return s.rowChanged(ctx, `DELETE FROM live_sessions WHERE task_id = $1 AND owner = $2`, taskID, owner)
}

func (s *PGStore) TakeLiveSession(ctx context.Context, taskID, from, to string) (bool, error) {
	return s.rowChanged(ctx, `UPDATE live_sessions SET owner = $3 WHERE task_id = $1 AND owner = $2`, taskID, from, to)
}

func (s *PGStore) rowChanged(ctx context.Context, query string, args ...any) (bool, error) {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *PGStore) DeleteLiveSession(ctx context.Context, taskID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM live_sessions WHERE task_id = $1`, taskID)
	return err
}
