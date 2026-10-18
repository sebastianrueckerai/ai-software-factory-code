package factoryd

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The intake switch: the chat header's, an approver flips it, and a
// closed intake refuses the wake's own filings — the turns the Waker
// starts without the member's ask — while the member's own ask still
// files and starts. One row per repository keeps the state; no row is an
// open intake.

// SetChatIntake upserts repository's intake: closed refuses the wake's
// own filings.
func (s *PGStore) SetChatIntake(ctx context.Context, repository string, closed bool, by string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_intake (repository, closed, set_by, at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (repository) DO UPDATE SET closed = excluded.closed, set_by = excluded.set_by, at = excluded.at`,
		repository, closed, by, at)
	return err
}

// ChatIntakeClosed reads whether repository's intake is closed; no row:
// open.
func (s *PGStore) ChatIntakeClosed(ctx context.Context, repository string) (bool, error) {
	var closed bool
	err := s.db.QueryRowContext(ctx, `SELECT closed FROM chat_intake WHERE repository = $1`, repository).Scan(&closed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return closed, nil
}
