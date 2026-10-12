package factoryd

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ConsoleSession is one row of console_sessions: a login of the console,
// kept so it survives a restart of factoryd (roadmap 164). IDHash is the
// SHA-256 hex of the cookie's id, never the id. Access and Refresh are
// the member's Forgejo OAuth tokens encrypted with AES-256-GCM under a
// key derived from the console's session key; TokenExpiry is when the
// access token runs out. LastSeen moves at most once a minute.
type ConsoleSession struct {
	IDHash      string
	Login       string
	CSRF        string
	Access      string
	Refresh     string
	TokenExpiry time.Time
	CreatedAt   time.Time
	LastSeen    time.Time
	ExpiresAt   time.Time
}

// SaveConsoleSession stores the row s, replacing the row of the same hash.
func (s *PGStore) SaveConsoleSession(ctx context.Context, sess ConsoleSession) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO console_sessions (id_hash, login, csrf, access, refresh, token_expiry, created_at, last_seen, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (id_hash) DO UPDATE SET login = EXCLUDED.login, csrf = EXCLUDED.csrf, access = EXCLUDED.access,
			refresh = EXCLUDED.refresh, token_expiry = EXCLUDED.token_expiry, created_at = EXCLUDED.created_at,
			last_seen = EXCLUDED.last_seen, expires_at = EXCLUDED.expires_at`,
		sess.IDHash, sess.Login, sess.CSRF, sess.Access, sess.Refresh, sess.TokenExpiry, sess.CreatedAt, sess.LastSeen, sess.ExpiresAt)
	return err
}

// ConsoleSessionByHash reads the row of the hash; false when there is
// none. An expired row is refused and deleted here, so a lookup both
// reads and sweeps its own row.
func (s *PGStore) ConsoleSessionByHash(ctx context.Context, hash string) (ConsoleSession, bool, error) {
	var sess ConsoleSession
	var expiry sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT id_hash, login, csrf, access, refresh, token_expiry, created_at, last_seen, expires_at
		FROM console_sessions WHERE id_hash = $1 AND expires_at >= now()`, hash).
		Scan(&sess.IDHash, &sess.Login, &sess.CSRF, &sess.Access, &sess.Refresh, &expiry, &sess.CreatedAt, &sess.LastSeen, &sess.ExpiresAt)
	switch {
	case err == nil:
		sess.TokenExpiry = expiry.Time
		return sess, true, nil
	case errors.Is(err, sql.ErrNoRows):
		if _, delErr := s.db.ExecContext(ctx, `DELETE FROM console_sessions WHERE id_hash = $1 AND expires_at < now()`, hash); delErr != nil {
			return ConsoleSession{}, false, delErr
		}
		return ConsoleSession{}, false, nil
	default:
		return ConsoleSession{}, false, err
	}
}

// DeleteConsoleSession removes the row of the hash (a logout).
func (s *PGStore) DeleteConsoleSession(ctx context.Context, hash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM console_sessions WHERE id_hash = $1`, hash)
	return err
}

// TouchConsoleSession moves the row's last_seen to now when it says older
// than notAfter; false when it did not move (the row is gone, or it was
// seen since). A caller passes the time of its own read plus a minute, so
// a minute of requests moves the column at most once: the next request
// reads the moved row, and its threshold sits a minute past the fresh
// last_seen, which the gate refuses.
func (s *PGStore) TouchConsoleSession(ctx context.Context, hash string, notAfter time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE console_sessions SET last_seen = now() WHERE id_hash = $1 AND last_seen < $2`, hash, notAfter)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// UpdateConsoleSessionTokens stores the refreshed tokens on the row.
func (s *PGStore) UpdateConsoleSessionTokens(ctx context.Context, hash, access, refresh string, expiry time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE console_sessions SET access = $2, refresh = $3, token_expiry = $4 WHERE id_hash = $1`,
		hash, access, refresh, expiry)
	return err
}

// PurgeConsoleSessions deletes the rows expired before at and answers how
// many.
func (s *PGStore) PurgeConsoleSessions(ctx context.Context, at time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM console_sessions WHERE expires_at < $1`, at)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
