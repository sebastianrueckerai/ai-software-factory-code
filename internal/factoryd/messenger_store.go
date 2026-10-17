package factoryd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"slices"
	"time"
)

// The messengers (roadmap 145): a member connects Telegram or Discord once
// and sets a level per project; the poke writer copies each inbox row their
// level passes into the outbox, and the sender carries it to the venue as
// one line with a link to the card (the plan of 2026-10-14).

// The venues a chat connects on.
const (
	VenueTelegram = "telegram"
	VenueDiscord  = "discord"
)

// The notification levels: off pages nothing, needs-you the asks, all the
// asks and the informs.
const (
	LevelOff      = "off"
	LevelNeedsYou = "needs-you"
	LevelAll      = "all"
)

// MessengerLink is one connected chat: venue, its address (the Telegram
// chat or the Discord DM channel) and the member it belongs to.
type MessengerLink struct {
	ID        int64
	Login     string
	Venue     string
	Address   string
	VenueUser string
	CreatedAt time.Time
}

// codeLife is how long a connect code lives.
const codeLife = 10 * time.Minute

// IssueMessengerCode makes sebastian's one-time connect code: 128 random
// bits in base64url, stored as its hash, one live code per login.
func (s *PGStore) IssueMessengerCode(ctx context.Context, login string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	_, err := s.db.ExecContext(ctx, `DELETE FROM messenger_codes WHERE login = $1`, login)
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO messenger_codes (code_hash, login, expires_at) VALUES ($1, $2, now() + $3)`,
		CodeHash(code), login, codeLife)
	return code, err
}

// CodeHash is the code's stored form.
func CodeHash(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// ConsumeMessengerCode binds venue's chat address to the member whose code
// this is, with their venue user id, and answers their login; false for a
// code that is missing, used or expired. The bind replaces the member's
// earlier link on the venue and steals the address from another member.
func (s *PGStore) ConsumeMessengerCode(ctx context.Context, code, venue, address, venueUser string) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after the commit
	var login string
	err = tx.QueryRowContext(ctx, `DELETE FROM messenger_codes WHERE code_hash = $1 AND expires_at > now() RETURNING login`,
		CodeHash(code)).Scan(&login)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messenger_links WHERE venue = $1 AND address = $2`, venue, address); err != nil {
		return "", false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messenger_links (login, venue, address, venue_user) VALUES ($1, $2, $3, $4)
		ON CONFLICT (login, venue) DO UPDATE SET address = excluded.address, venue_user = excluded.venue_user, created_at = now()`,
		login, venue, address, venueUser); err != nil {
		return "", false, err
	}
	return login, true, tx.Commit()
}

// MessengerLinks are the member's connected chats.
func (s *PGStore) MessengerLinks(ctx context.Context, login string) ([]MessengerLink, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, login, venue, address, venue_user, created_at FROM messenger_links WHERE login = $1 ORDER BY venue`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessengerLink
	for rows.Next() {
		var l MessengerLink
		if err := rows.Scan(&l.ID, &l.Login, &l.Venue, &l.Address, &l.VenueUser, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// UnlinkMessenger drops the member's link on venue (a disconnect).
func (s *PGStore) UnlinkMessenger(ctx context.Context, login, venue string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM messenger_links WHERE login = $1 AND venue = $2`, login, venue)
	return err
}

// SetNotifyLevel stores the member's level for a project.
func (s *PGStore) SetNotifyLevel(ctx context.Context, login, repository, level string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO notify_levels (login, repository, level) VALUES ($1, $2, $3)
		ON CONFLICT (login, repository) DO UPDATE SET level = excluded.level`, login, repository, level)
	return err
}

// NotifyLevelsOf are the member's explicit level rows, by repository.
func (s *PGStore) NotifyLevelsOf(ctx context.Context, login string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repository, level FROM notify_levels WHERE login = $1`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var repo, level string
		if err := rows.Scan(&repo, &level); err != nil {
			return nil, err
		}
		out[repo] = level
	}
	return out, rows.Err()
}

// DeleteNotifyLevel drops the member's level row for a project (a lost
// read).
func (s *PGStore) DeleteNotifyLevel(ctx context.Context, login, repository string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM notify_levels WHERE login = $1 AND repository = $2`, login, repository)
	return err
}

// PokeRecipients are a poke's recipients on repository: base plus every
// member whose explicit level row (needs-you or all) joins them. A member
// of base keeps their rows whatever their level.
func (s *PGStore) PokeRecipients(ctx context.Context, repository string, base []string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT login FROM notify_levels WHERE repository = $1 AND level IN ($2, $3)
		AND login <> ALL($4) ORDER BY login`, repository, LevelNeedsYou, LevelAll, base)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	joined := slices.Clone(base)
	for rows.Next() {
		var login string
		if err := rows.Scan(&login); err != nil {
			return nil, err
		}
		joined = append(joined, login)
	}
	return joined, rows.Err()
}

// queueDeliveries writes one pending outbox row per connection whose
// member's effective level passes plan's row level, inside the poke's
// transaction, so a notification is never lost between the event and the
// send. The explicit level row wins; without one the derived default is
// needs-you inside plan's notify list, off outside it.
func queueDeliveries(ctx context.Context, tx *sql.Tx, repository string, logins []string, ids []int64, plan PokeDelivery) error {
	if plan.RowLevel == "" || !plan.On {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT l.id, l.login, v.level FROM messenger_links l
		LEFT JOIN notify_levels v ON v.login = l.login AND v.repository = $1`, repository)
	if err != nil {
		return err
	}
	defer rows.Close()
	links, explicit := map[string][]int64{}, map[string]string{}
	for rows.Next() {
		var id int64
		var login string
		var level sql.NullString
		if err := rows.Scan(&id, &login, &level); err != nil {
			return err
		}
		links[login] = append(links[login], id)
		if level.Valid {
			explicit[login] = level.String
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	row := levelRank(plan.RowLevel)
	for i, login := range logins {
		if ids[i] == 0 {
			continue // a repeat sends nothing new
		}
		member := explicit[login]
		if member == "" {
			member = LevelOff
			if slices.Contains(plan.Derived, login) {
				member = LevelNeedsYou
			}
		}
		if levelRank(member) < row {
			continue
		}
		for _, link := range links[login] {
			if _, err := tx.ExecContext(ctx, `INSERT INTO notify_deliveries (message_id, link_id) VALUES ($1, $2)`, ids[i], link); err != nil {
				return err
			}
		}
	}
	return nil
}

// levelRank orders the levels for a row's pass check.
func levelRank(level string) int {
	switch level {
	case LevelAll:
		return 2
	case LevelNeedsYou:
		return 1
	}
	return 0
}

// Delivery is one outbox row with what its send needs: the link, the inbox
// row and the project it copies.
type Delivery struct {
	ID         int64
	MessageID  int64
	LinkID     int64
	Login      string
	Venue      string
	Address    string
	Repository string
	Content    string
	Kind       string
	BirthKind  string
	TaskID     string
	At         time.Time
	Tries      int
	LastError  string
	Ref        string
}

// QueueDelivery adds a pending outbox row for inbox row message on link.
func (s *PGStore) QueueDelivery(ctx context.Context, message, link int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO notify_deliveries (message_id, link_id) VALUES ($1, $2)`, message, link)
	return err
}

const deliveryColumns = `d.id, d.message_id, d.link_id, l.login, l.venue, l.address, m.repository, m.content, m.kind, m.birth_kind, m.task_id, m.at, d.tries, d.last_error, d.ref`

// DeliveriesDue are the pending rows whose next try has come, oldest
// first, at most limit, each joined with its link and its landed inbox
// row; a row still queued in the inbox is not due.
func (s *PGStore) DeliveriesDue(ctx context.Context, limit int) ([]Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deliveryColumns+` FROM notify_deliveries d
		JOIN messenger_links l ON l.id = d.link_id
		JOIN chat_messages m ON m.id = d.message_id
		WHERE d.state = 'pending' AND d.next_try <= now() AND NOT m.queued
		ORDER BY d.next_try LIMIT $1`, limit)
	return scanDeliveries(rows, err)
}

// DeliveriesSent are the sent rows within window whose settle edit has
// not landed, oldest first.
func (s *PGStore) DeliveriesSent(ctx context.Context, window time.Duration) ([]Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deliveryColumns+` FROM notify_deliveries d
		JOIN messenger_links l ON l.id = d.link_id
		JOIN chat_messages m ON m.id = d.message_id
		WHERE d.state = 'sent' AND d.edited_at IS NULL AND d.sent_at > now() - $1::interval
		ORDER BY d.sent_at`, window)
	return scanDeliveries(rows, err)
}

func scanDeliveries(rows *sql.Rows, err error) ([]Delivery, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.MessageID, &d.LinkID, &d.Login, &d.Venue, &d.Address, &d.Repository, &d.Content,
			&d.Kind, &d.BirthKind, &d.TaskID, &d.At, &d.Tries, &d.LastError, &d.Ref); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RecordDeliverySent marks the row sent with its venue message id ref.
func (s *PGStore) RecordDeliverySent(ctx context.Context, id int64, ref string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notify_deliveries SET state = 'sent', ref = $2, sent_at = now(), next_try = 'infinity' WHERE id = $1`, id, ref)
	return err
}

// RetryDelivery counts a failed try and waits before the next.
func (s *PGStore) RetryDelivery(ctx context.Context, id int64, wait time.Duration, why string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notify_deliveries SET tries = tries + 1, next_try = now() + $2, last_error = $3 WHERE id = $1`, id, wait, why)
	return err
}

// FailDelivery closes the row after its last try.
func (s *PGStore) FailDelivery(ctx context.Context, id int64, why string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notify_deliveries SET state = 'failed', tries = tries + 1, last_error = $2 WHERE id = $1`, id, why)
	return err
}

// DropDelivery removes a pending row that will never send (its ask settled
// first).
func (s *PGStore) DropDelivery(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM notify_deliveries WHERE id = $1`, id)
	return err
}

// MarkDeliveryEdited stamps the settle edit.
func (s *PGStore) MarkDeliveryEdited(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notify_deliveries SET edited_at = now() WHERE id = $1`, id)
	return err
}

// SameSecret reports two equal secrets without leaking where they differ.
func SameSecret(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
