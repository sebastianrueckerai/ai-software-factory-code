package factoryd

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ChatToken is a human's chat token: the bearer's hash, the human it acts
// as and the project it reaches, the human's Forgejo token sealed by the
// console, and the thread of its own.
type ChatToken struct {
	ID                int64
	Hash              string // SHA-256 hex of the bearer
	Login, Repository string
	Name              string
	Sealed            string // the human's Forgejo token, sealed by the console
	Conversation      int64  // the token's own thread; 0 before its first turn
	Created, Expires  time.Time
	LastUsed, Revoked time.Time // zero: never
}

const chatTokenColumns = `id, hash, login, repository, name, sealed, conversation, created_at, expires_at, last_used, revoked_at`

func scanChatToken(row interface{ Scan(...any) error }) (ChatToken, error) {
	var t ChatToken
	var used, revoked sql.NullTime
	if err := row.Scan(&t.ID, &t.Hash, &t.Login, &t.Repository, &t.Name, &t.Sealed, &t.Conversation, &t.Created, &t.Expires, &used, &revoked); err != nil {
		return ChatToken{}, err
	}
	t.LastUsed, t.Revoked = used.Time, revoked.Time
	return t, nil
}

// AddChatToken stores t and answers its id. Created is the store's now.
func (s *PGStore) AddChatToken(ctx context.Context, t ChatToken) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO chat_tokens (hash, login, repository, name, sealed, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, t.Hash, t.Login, t.Repository, t.Name, t.Sealed, t.Expires).Scan(&id)
	return id, err
}

// ChatTokenByHash is the token with hash, revoked or not; ok false when none.
func (s *PGStore) ChatTokenByHash(ctx context.Context, hash string) (ChatToken, bool, error) {
	t, err := scanChatToken(s.db.QueryRowContext(ctx, `SELECT `+chatTokenColumns+` FROM chat_tokens WHERE hash = $1`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return ChatToken{}, false, nil
	}
	return t, err == nil, err
}

// ChatTokensOf are login's tokens not revoked, newest first.
func (s *PGStore) ChatTokensOf(ctx context.Context, login string) ([]ChatToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+chatTokenColumns+` FROM chat_tokens
		WHERE login = $1 AND revoked_at IS NULL ORDER BY created_at DESC, id DESC`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatToken
	for rows.Next() {
		t, err := scanChatToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeChatToken revokes login's token id at now; false when the token
// is not login's or is revoked already.
func (s *PGStore) RevokeChatToken(ctx context.Context, id int64, login string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE chat_tokens SET revoked_at = $3 WHERE id = $1 AND login = $2 AND revoked_at IS NULL`, id, login, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// TouchChatToken moves the token's last use to now when it is older than
// a minute.
func (s *PGStore) TouchChatToken(ctx context.Context, id int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE chat_tokens SET last_used = $2::timestamptz
		WHERE id = $1 AND (last_used IS NULL OR last_used < $2::timestamptz - interval '1 minute')`, id, now)
	return err
}

// ClaimChatTokenConversation gives the token thread n when it has none
// and answers the thread in force.
func (s *PGStore) ClaimChatTokenConversation(ctx context.Context, id, n int64) (int64, error) {
	var got int64
	err := s.db.QueryRowContext(ctx, `UPDATE chat_tokens SET conversation = CASE WHEN conversation = 0 THEN $2 ELSE conversation END
		WHERE id = $1 RETURNING conversation`, id, n).Scan(&got)
	return got, err
}

// SideConversation opens a thread titled title one past login's highest
// on repository and answers its number. The current thread stays the
// current one, and an empty current thread is never reused.
func (s *PGStore) SideConversation(ctx context.Context, repository, login, title string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO chat_conversations (repository, login, conversation, title, current)
		SELECT $1, $2, coalesce(max(conversation), 0) + 1, $3, false FROM chat_conversations WHERE repository = $1 AND login = $2
		RETURNING conversation`, repository, login, title).Scan(&n)
	return n, err
}
