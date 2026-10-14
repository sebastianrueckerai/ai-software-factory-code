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
	StartedAt, Deadline                             time.Time
	Lines                                           int
}

func (s *PGStore) PutLiveSession(ctx context.Context, l LiveSession) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO live_sessions (task_id, session_id, namespace, pod, message_token, started_at, deadline, lines)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (task_id) DO UPDATE SET session_id = EXCLUDED.session_id, namespace = EXCLUDED.namespace, pod = EXCLUDED.pod,
		message_token = EXCLUDED.message_token, started_at = EXCLUDED.started_at, deadline = EXCLUDED.deadline, lines = EXCLUDED.lines`,
		l.TaskID, l.SessionID, l.Namespace, l.Pod, l.MessageToken, l.StartedAt, l.Deadline, l.Lines)
	return err
}

func (s *PGStore) SetLiveSessionLines(ctx context.Context, taskID string, lines int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE live_sessions SET lines = $2 WHERE task_id = $1`, taskID, lines)
	return err
}

func (s *PGStore) LiveSession(ctx context.Context, taskID string) (LiveSession, bool, error) {
	var l LiveSession
	err := s.db.QueryRowContext(ctx, `SELECT task_id, session_id, namespace, pod, message_token, started_at, deadline, lines
		FROM live_sessions WHERE task_id = $1`, taskID).Scan(&l.TaskID, &l.SessionID, &l.Namespace, &l.Pod, &l.MessageToken, &l.StartedAt, &l.Deadline, &l.Lines)
	if errors.Is(err, sql.ErrNoRows) {
		return LiveSession{}, false, nil
	}
	return l, err == nil, err
}

func (s *PGStore) DeleteLiveSession(ctx context.Context, taskID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM live_sessions WHERE task_id = $1`, taskID)
	return err
}
