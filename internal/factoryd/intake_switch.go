package factoryd

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The intake switch: the chat header's, an approver flips it, and a
// closed intake stops exactly one thing — the autonomous pull of the doc
// repository's roadmap. Everything else a turn does at intake stays
// open: a wake's reactive filing still files, and the human's own ask
// still files and starts. One row per repository keeps the state; no row
// is an open intake.

// SetChatIntake upserts repository's intake: closed stops the roadmap
// pull.
func (s *PGStore) SetChatIntake(ctx context.Context, repository string, closed bool, by string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_intake (repository, closed, set_by, at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (repository) DO UPDATE SET closed = excluded.closed, set_by = excluded.set_by, at = excluded.at`,
		repository, closed, by, at)
	return err
}

// ChatIntakeClosed reads whether repository's intake is closed; no row:
// open. The waker's roadmap pull is what it gates.
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

// IntakeRow is a repository's intake switch row: its closed flag, who
// set it and when, for the board's switch line.
type IntakeRow struct {
	Closed bool
	By     string
	At     time.Time
}

// ChatIntake reads repository's intake switch row whole; no row: open,
// with no setter.
func (s *PGStore) ChatIntake(ctx context.Context, repository string) (IntakeRow, bool, error) {
	var row IntakeRow
	err := s.db.QueryRowContext(ctx, `SELECT closed, set_by, at FROM chat_intake WHERE repository = $1`, repository).
		Scan(&row.Closed, &row.By, &row.At)
	if errors.Is(err, sql.ErrNoRows) {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	return row, true, nil
}
