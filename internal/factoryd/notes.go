package factoryd

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// Notes is a project's scratchpad (roadmap 239): working notes every
// thread of its chat reads and writes, which steer nothing.
type Notes struct {
	Content string
	At      time.Time
	By      string
}

// Hash is the content's sha256, the base a write carries.
func (n Notes) Hash() string { return ContentHash(n.Content) }

// ContentHash is text's sha256 in hex.
func ContentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Notes is repository's scratchpad, empty before its first write.
func (s *PGStore) Notes(ctx context.Context, repository string) (Notes, error) {
	var n Notes
	err := s.db.QueryRowContext(ctx, `SELECT content, updated_at, updated_by FROM chat_notes WHERE repository = $1`, repository).
		Scan(&n.Content, &n.At, &n.By)
	if errors.Is(err, sql.ErrNoRows) {
		return Notes{}, nil
	}
	return n, err
}

// PutNotes replaces repository's scratchpad with content as login, when
// the scratchpad still hashes to base: false, and nothing written, when
// another write came between.
func (s *PGStore) PutNotes(ctx context.Context, repository, login, content, base string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after the commit
	// the row first, so two first writes queue on its lock
	if _, err := tx.ExecContext(ctx, `INSERT INTO chat_notes (repository, content, updated_by) VALUES ($1, '', '')
		ON CONFLICT (repository) DO NOTHING`, repository); err != nil {
		return false, err
	}
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT content FROM chat_notes WHERE repository = $1 FOR UPDATE`, repository).Scan(&current); err != nil {
		return false, err
	}
	if ContentHash(current) != base {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_notes SET content = $2, updated_at = now(), updated_by = $3 WHERE repository = $1`,
		repository, content, login); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
