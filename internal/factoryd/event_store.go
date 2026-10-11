package factoryd

import (
	"context"
	"database/sql"
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

// The kinds a human's action in the console leaves behind, one per
// action whether a button or the project agent's tool ran it; factoryd's
// own events keep the names of events.go.
const (
	EventCardFiled            = "card_filed"
	EventCardMoved            = "card_moved"
	EventQuestionAnswered     = "question_answered"
	EventPlanQuestionAnswered = "plan_question_answered"
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
	EventSummarySet           = "summary_set"
	EventAccentSet            = "accent_set"
	EventCardCommented        = "card_commented"
	EventCommentDeleted       = "comment_deleted"
	EventCardSteered          = "card_steered"
	EventGithubExportSet      = "github_export_set"
	EventGithubExportRemoved  = "github_export_removed"
	EventMemberInvited        = "member_invited"
	EventCardClosed           = "card_closed"
	EventCardEdited           = "card_edited"
	EventSessionGranted       = "session_granted"
	EventDependencySet        = "dependency_set"
)

// EventTaskStatus is factoryd's own record of a task's status or gate
// after the store changed either: detail {"status", "gate"}.
const EventTaskStatus = "task_status"

// EventSessionSaid is the agent's text between its tool calls in a
// running session: published on the bus for the drawer's Chat, never
// stored, as the session's transcript is its record. Detail {"session",
// "text"}.
const EventSessionSaid = "session_said"

// EventQuestionAsked is factoryd's record of an escalation, the question
// question_answered answers: detail {"question", "comment", and "choices"
// when the agent offered any}.
const EventQuestionAsked = "question_asked"

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
	// Record inserts e and returns its id, or 0 and no error when e
	// names an answer comment already recorded for its task.
	Record(ctx context.Context, e Event) (int64, error)
	// ProjectEvents lists repository's events newest first: those with an
	// id above after (0 = every event), at most limit, the pulse's own
	// aside.
	ProjectEvents(ctx context.Context, repository string, after int64, limit int) ([]Event, error)
	// TaskEvents lists task id's events newest first, at most limit.
	TaskEvents(ctx context.Context, id string, limit int) ([]Event, error)
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
	// only a second row naming the same answer comment conflicts (the
	// partial unique index of 20261011170500_events_grant_once.sql)
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO events (at, repository, task_id, issue_number, kind, actor, via, detail)
		 VALUES (COALESCE($1, now()), $2, NULLIF($3, ''), NULLIF($4, 0), $5, $6, $7, $8::jsonb)
		 ON CONFLICT (task_id, (detail->>'comment')) WHERE kind IN ('question_answered', 'session_granted') DO NOTHING
		 RETURNING id, at`,
		nullTime(e.At), e.Repository, e.TaskID, e.IssueNumber, e.Kind, e.Actor, e.Via, detail).Scan(&e.ID, &e.At)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, err
	}
	if s.bus != nil {
		s.bus.Publish(e)
	}
	return e.ID, nil
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
	return s.events(ctx, `SELECT `+eventColumns+` FROM events WHERE repository = $1 AND id > $2
		AND kind NOT IN ('`+EventPulse+`', '`+EventPulseComputing+`') ORDER BY id DESC LIMIT $3`,
		repository, after, limit)
}

func (s *PGStore) TaskEvents(ctx context.Context, id string, limit int) ([]Event, error) {
	return s.events(ctx, `SELECT `+eventColumns+` FROM events WHERE task_id = $1 ORDER BY id DESC LIMIT $2`, id, limit)
}

// events runs query, which returns eventColumns, and scans its rows.
func (s *PGStore) events(ctx context.Context, query string, args ...any) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
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
}

var _ eventRecorder = (*PGStore)(nil)

// eventStoreOf is store's row-writing half for the Timeline, or nil when it
// writes none: a test store that only answers tasks records no rows.
func eventStoreOf(store Store) eventRecorder {
	r, ok := store.(eventRecorder)
	if !ok {
		return nil
	}
	return r
}
