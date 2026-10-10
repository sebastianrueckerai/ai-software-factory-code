package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// The ways an event reached the factory (ruling 252: one event whatever
// the input): a human's button in the console, the project agent's chat,
// factoryd itself, or a webhook delivery.
const (
	ViaButton   = "button"
	ViaChat     = "chat"
	ViaFactoryd = "factoryd"
	ViaWebhook  = "webhook"
)

// The kinds a human's action in the console leaves behind, one per POST
// handler; factoryd's own events keep the names of events.go.
const (
	EventCardFiled            = "card_filed"
	EventCardMoved            = "card_moved"
	EventQuestionAnswered     = "question_answered"
	EventPlanStarted          = "plan_started"
	EventPlanAccepted         = "plan_accepted"
	EventPlanChangesRequested = "plan_changes_requested"
	EventPlanRejected         = "plan_rejected"
	EventCardRetried          = "card_retried"
	EventMergeApproved        = "merge_approved"
	EventMergeDenied          = "merge_denied"
	EventAlertVerdict         = "alert_verdict"
	EventResourcePromoted     = "resource_promoted"
	EventResourceReleased     = "resource_released"
	EventProjectFiled         = "project_filed"
	EventProjectMerged        = "project_merged"
)

// Event is one row of the events table: what happened, to which card or
// project, who did it, and how it reached the factory. Repository is
// owner/name, the project the event belongs to; TaskID and IssueNumber are
// empty for a project-level event. Detail is free-form JSON.
type Event struct {
	ID          int64
	At          time.Time
	Repository  string
	TaskID      string
	IssueNumber int64
	Kind        string
	Actor       string
	Via         string
	Detail      map[string]any
}

// Events is the event spine: one function records every event, whatever
// the input. The console holds it as a port like the other stores;
// factoryd's Timeline writes a row beside each timeline line. A nil
// recorder records nothing and lists none.
type Events interface {
	// Record inserts e and returns its id.
	Record(ctx context.Context, e Event) (int64, error)
	// ProjectEvents lists repository's events newest first: those with an
	// id above after (0 = every event), at most limit.
	ProjectEvents(ctx context.Context, repository string, after int64, limit int) ([]Event, error)
	// NewestTaskEvents are the newest event of each of tasks, keyed by task
	// id: the board's activity line. A task with no event gets no entry.
	NewestTaskEvents(ctx context.Context, tasks []string) (map[string]Event, error)
}

func (s *PGStore) Record(ctx context.Context, e Event) (int64, error) {
	if e.Repository == "" || e.Kind == "" {
		return 0, errors.New("event: repository and kind are required")
	}
	detail := any(nil)
	if e.Detail != nil {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			return 0, err
		}
		detail = b
	}
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO events (at, repository, task_id, issue_number, kind, actor, via, detail)
		 VALUES (COALESCE($1, now()), $2, NULLIF($3, ''), NULLIF($4, 0), $5, $6, $7, $8::jsonb) RETURNING id`,
		nullTime(e.At), e.Repository, e.TaskID, e.IssueNumber, e.Kind, e.Actor, e.Via, detail).Scan(&id)
	return id, err
}

// nullTime is t as a SQL NULL when it is the zero time, so the column
// default (now()) takes it.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

const eventColumns = `id, at, repository, COALESCE(task_id, ''), COALESCE(issue_number, 0), kind, actor, via, detail`

func scanEvent(scan func(dest ...any) error) (Event, error) {
	var e Event
	var detail []byte
	if err := scan(&e.ID, &e.At, &e.Repository, &e.TaskID, &e.IssueNumber, &e.Kind, &e.Actor, &e.Via, &detail); err != nil {
		return Event{}, err
	}
	if len(detail) > 0 {
		if err := json.Unmarshal(detail, &e.Detail); err != nil {
			return Event{}, err
		}
	}
	return e, nil
}

func (s *PGStore) ProjectEvents(ctx context.Context, repository string, after int64, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+eventColumns+` FROM events WHERE repository = $1 AND id > $2 ORDER BY id DESC LIMIT $3`,
		repository, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *PGStore) NewestTaskEvents(ctx context.Context, tasks []string) (map[string]Event, error) {
	out := map[string]Event{}
	if len(tasks) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT ON (task_id) `+eventColumns+` FROM events WHERE task_id = ANY($1) ORDER BY task_id, id DESC`, tasks)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanEvent(rows.Scan)
		if err != nil {
			return nil, err
		}
		out[e.TaskID] = e
	}
	return out, rows.Err()
}

// eventRecorder is the Timeline's sink: the store that turns factoryd's own
// events into rows beside their timeline lines. A nil recorder records
// nothing, so a Timeline without one keeps writing only timeline lines.
type eventRecorder interface {
	Record(ctx context.Context, e Event) (int64, error)
	// ButtonCommentIDs are the comment ids among taskID's question_answered
	// rows left by a button: intake stays silent for those comments, so an
	// answer recorded once is never recorded twice.
	ButtonCommentIDs(ctx context.Context, taskID string) ([]int64, error)
}

var _ eventRecorder = (*PGStore)(nil)

// ButtonCommentIDs lists the "comment" of taskID's question_answered rows
// recorded through a button, newest first.
func (s *PGStore) ButtonCommentIDs(ctx context.Context, taskID string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT (detail->>'comment')::bigint FROM events
		 WHERE task_id = $1 AND kind = $2 AND via = $3 AND detail ? 'comment' ORDER BY id DESC`,
		taskID, EventQuestionAnswered, ViaButton)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// eventStoreOf is store's row-writing half for the Timeline, or nil when it
// writes none: a test store that only answers tasks records no rows.
func eventStoreOf(store Store) eventRecorder {
	r, ok := store.(eventRecorder)
	if !ok {
		return nil
	}
	return r
}
