package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
)

// SessionBrief is the text a card's session gave its first model call, as
// its worker built it: Session is its number among the card's sessions,
// from 1.
type SessionBrief struct {
	Session int
	Brief   string
	At      time.Time
}

// ErrSessionBriefStored refuses a second text for a session that stored
// one: a session's worker builds its text once, so the first stays the
// record of what the session read.
type ErrSessionBriefStored struct {
	Task    string
	Session int
}

func (e ErrSessionBriefStored) Error() string {
	return fmt.Sprintf("session %d of %s stored the text it got already", e.Session, e.Task)
}

// SessionBriefs are the texts task taskID's sessions got, oldest first.
func (s *PGStore) SessionBriefs(ctx context.Context, taskID string) ([]SessionBrief, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session, brief, created_at FROM session_briefs WHERE task_id = $1 ORDER BY session`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionBrief
	for rows.Next() {
		var b SessionBrief
		if err := rows.Scan(&b.Session, &b.Brief, &b.At); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// StoreSessionBrief stores text, the first message of session sessionID
// of task taskID, as the session's number among the task's sessions, which
// it returns. A session that is none of the task's is ErrNotFound; the
// same text again is stored once, and another refused
// (ErrSessionBriefStored).
func (s *PGStore) StoreSessionBrief(ctx context.Context, taskID, sessionID, text string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after the commit
	var n int
	var inserted bool
	if err := tx.QueryRowContext(ctx, `WITH me AS (SELECT created_at, id FROM sessions WHERE id = $2 AND task_id = $1),
			n AS (SELECT count(*)::int AS n FROM sessions s, me WHERE s.task_id = $1 AND (s.created_at, s.id) <= (me.created_at, me.id)),
			ins AS (INSERT INTO session_briefs (task_id, session, brief) SELECT $1, n, $3 FROM n WHERE n > 0
				ON CONFLICT (task_id, session) DO NOTHING RETURNING 1)
		SELECT n, EXISTS (SELECT 1 FROM ins) FROM n`, taskID, sessionID, text).Scan(&n, &inserted); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, fmt.Errorf("session %s of %s: %w", sessionID, taskID, ErrNotFound)
	}
	if !inserted {
		// a statement of its own: its snapshot holds the row a store that
		// raced this one committed while the insert waited on it
		var same bool
		if err := tx.QueryRowContext(ctx, `SELECT brief = $3 FROM session_briefs WHERE task_id = $1 AND session = $2`, taskID, n, text).Scan(&same); err != nil {
			return 0, err
		}
		if !same {
			return n, ErrSessionBriefStored{Task: taskID, Session: n}
		}
	}
	return n, tx.Commit()
}

// maxSessionBriefBody bounds the body of the gateway's hand-over: a text at
// gateway.MaxModelBody, which JSON may grow sixfold.
const maxSessionBriefBody = 6*gateway.MaxModelBody + 64<<10

// SessionBriefHandler stores the text a session's worker gave its first
// model call (POST /internal/session-brief, body {"task_id", "session",
// "text"}): the gateway's hand-over of its POST /worker/session-brief, the
// session the worker pod's token names. A text past gateway.MaxModelBody
// reached no model and is refused whole.
func SessionBriefHandler(store interface {
	StoreSessionBrief(ctx context.Context, taskID, sessionID, text string) (int, error)
}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			TaskID  string `json:"task_id"`
			Session string `json:"session"`
			Text    string `json:"text"`
		}
		err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSessionBriefBody)).Decode(&in)
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge) || len(in.Text) > gateway.MaxModelBody:
			http.Error(w, fmt.Sprintf("the session's text is over %d bytes, the bound of a model call's body, so no model read it", gateway.MaxModelBody), http.StatusRequestEntityTooLarge)
			return
		case err != nil || in.TaskID == "" || in.Session == "" || in.Text == "":
			http.Error(w, "the body names no task_id, session or text", http.StatusBadRequest)
			return
		}
		_, err = store.StoreSessionBrief(r.Context(), in.TaskID, in.Session, in.Text)
		var stored ErrSessionBriefStored
		switch {
		case errors.Is(err, ErrNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.As(err, &stored):
			http.Error(w, err.Error(), http.StatusConflict)
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}
