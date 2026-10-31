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
	// ViaConsole is the documents' editor: a member's save in the
	// console, neither a button nor the chat (roadmap 277)
	ViaConsole = "console"
)

// The kinds a human's action in the console leaves behind, one per
// action whether a button or the project agent's tool ran it; factoryd's
// own events keep the names of events.go.
const (
	EventCardFiled            = "card_filed"
	EventQuestionAnswered     = "question_answered"
	EventPlanQuestionAnswered = "plan_question_answered"
	EventHoldoutDecided       = "holdout_decided"
	EventPlanChangesRequested = "plan_changes_requested"
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
	EventGithubSynced         = "github_synced"
	EventMemberInvited        = "member_invited"
	EventCardClosed           = "card_closed"
	EventCardEdited           = "card_edited"
	EventSessionGranted       = "session_granted"
	EventDependencySet        = "dependency_set"
	EventCardReplanned        = "card_replanned"
	EventCardFunded           = "card_funded"
	// EventChatFunded is an approver's raise of a project's chat task's
	// monthly budget, the console's Fund the chat: detail {"from_eur",
	// "to_eur", "amount_eur", "reason"}.
	EventChatFunded = "chat_funded"
	// EventRoutineSkipped is a Skip once set or, with undo, cleared:
	// detail {"project", "routine", "undo"}.
	EventRoutineSkipped = "routine_skipped"
	// EventRoutineNeedsMe is the live toggle set or, with undo, cleared:
	// detail {"project", "routine", "undo", "needs_me"}.
	EventRoutineNeedsMe = "routine_needs_me"
	// EventRoutineRunNow is a Run now: detail {"project", "routine",
	// "outcome"}, the run's outcome or "pending" when none came in time.
	EventRoutineRunNow = "routine_run_now"
	// EventRoutineWritten is a routine created or changed in its
	// project's file: detail {"project", "routine", "created"}.
	EventRoutineWritten = "routine_written"
	// EventRoutineDeleted is a routine taken out of its project's file:
	// detail {"project", "routine"}.
	EventRoutineDeleted = "routine_deleted"
	// EventRoutineRan is a project routine's run reaching its outcome:
	// detail {"project", "routine", "run", "outcome", "schedule"}, with
	// "note" when the run has one and "poke", the inbox's words, when the
	// run tells the inbox, which the Poker writes.
	EventRoutineRan = "routine_ran"

	// EventDocWritten is one write of a document through the door,
	// whatever wrote it: the chat's write_doc, a close-out, the member's
	// save in the console's editor. Detail {"path", "hash", "commit"}.
	EventDocWritten = "doc_written"
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

// EventAnalysisAttached is the chat agent's record of what it found about
// an ask (the main agent concept): detail {"found", "source": "agent",
// and "recommendation" when it offers one}. The spine is append-only, so
// the analysis is its own row; the ask's own event stays what it was.
const EventAnalysisAttached = "analysis_attached"

// EventQuestionWithdrawn is factoryd's record of an escalation it
// withdrew unanswered, the head it asked about gone back to the agent:
// detail {"question", "comment", "reason"}.
const EventQuestionWithdrawn = "question_withdrawn"

// EventPlanReplanned is factoryd's record of a replan's revision filed on
// its plan: detail {"revision", "kept", "changed", "dropped", "added",
// "origin", "by", "comment"}, the old cards' fates and the new cards
// without a from, and who asked: a person, or the cut of a card; a
// round's: {"revision", "origin": "round", "round", "cards"}, the issues
// of the round's cards in filing order.
const EventPlanReplanned = "plan_replanned"

// EventChatBudgetSpent is factoryd's record, on a project's chat task of
// the month, of the gateway refusing a chat call for that task's own
// budget (refused_budget): detail {"budget_eur", "spent_eur",
// "suggest_eur"}, the budget it stopped at, the spend then and the fund
// the chat offers. The chat stands while the task's budget is not above
// budget_eur, until a fund or the policy's raise or the month's turn. The
// poke it raises asks at the needs-you level.
const EventChatBudgetSpent = "chat_budget_spent"

// The pulse's own events, historical kinds: the pulse band the console
// once showed under a project's status line had a cheap model write it,
// and a computation started (pulse_computing) and ended (pulse, detail:
// sentence, bullets, prominent). No code writes them since the band's
// removal, but rows live on in the table, and a project's events list
// leaves them out.
const (
	EventPulseComputing = "pulse_computing"
	EventPulse          = "pulse"
)

// Event is one row of the events table: what happened, to which card or
// project, who did it, and how it reached the factory. Repository is
// owner/name, the project the event belongs to; TaskID and IssueNumber are
// empty for a project-level event. Detail is free-form JSON. IdentityKey
// is the fact's identity, the metrics contracts' one-row-per-fact key;
// empty leaves the row unkeyed, as every row before the contracts is.
// The key is written, never read back: reads go by kind and detail.
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
	IdentityKey string
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
	// (historical) kinds aside.
	ProjectEvents(ctx context.Context, repository string, after int64, limit int) ([]Event, error)
	// TaskEvents lists task id's events newest first, at most limit.
	TaskEvents(ctx context.Context, id string, limit int) ([]Event, error)
	// TaskEventsFor is TaskEvents over many cards at once, keyed by task
	// id. A card with no event gets no entry.
	TaskEventsFor(ctx context.Context, ids []string, limit int) (map[string][]Event, error)
	// NewestTaskEvents are the newest event of each of tasks, keyed by task
	// id: the board's activity line. A task with no event gets no entry.
	NewestTaskEvents(ctx context.Context, tasks []string) (map[string]Event, error)
}

func (s *PGStore) Record(ctx context.Context, e Event) (int64, error) {
	e, inserted, err := insertEvent(ctx, s.db, e)
	if err != nil || !inserted {
		return 0, err
	}
	s.publish(e)
	return e.ID, nil
}

func (s *PGStore) publish(e Event) {
	if s.bus != nil {
		s.bus.Publish(e)
	}
}

// rowQuerier is the one-row query of a database or a transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// insertEvent writes e as a row of q; inserted is false for the row a
// conflict dropped.
func insertEvent(ctx context.Context, q rowQuerier, e Event) (_ Event, inserted bool, _ error) {
	if e.Repository == "" || e.Kind == "" {
		return e, false, errors.New("event: repository and kind are required")
	}
	detail := any(nil)
	if e.Detail != nil {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			return e, false, err
		}
		detail = b
	}
	// a second row naming an identity another row already holds — the
	// answer comment of 20261011170500_events_grant_once.sql, the
	// IdentityKey of the metrics-identity migration — conflicts and
	// drops; every unique index on events is such an identity
	err := q.QueryRowContext(ctx,
		`INSERT INTO events (at, repository, task_id, issue_number, kind, actor, via, detail, identity_key)
		 VALUES (COALESCE($1, now()), $2, NULLIF($3, ''), NULLIF($4, 0), $5, $6, $7, $8::jsonb, NULLIF($9, ''))
		 ON CONFLICT DO NOTHING
		 RETURNING id, at`,
		nullTime(e.At), e.Repository, e.TaskID, e.IssueNumber, e.Kind, e.Actor, e.Via, detail, e.IdentityKey).Scan(&e.ID, &e.At)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return e, false, nil
	case err != nil:
		return e, false, err
	}
	return e, true, nil
}

// nullTime is t, or SQL NULL for the zero time.
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

// FiledFor is the actor of the console's card_filed row of repository's
// issue: the member a chat filing was made for.
func (s *PGStore) FiledFor(ctx context.Context, repository string, issue int64) (string, bool, error) {
	var actor string
	err := s.db.QueryRowContext(ctx, `SELECT actor FROM events WHERE repository = $1 AND issue_number = $2 AND kind = $3 ORDER BY id LIMIT 1`,
		repository, issue, EventCardFiled).Scan(&actor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return actor, err == nil, err
}

func (s *PGStore) TaskEvents(ctx context.Context, id string, limit int) ([]Event, error) {
	return s.events(ctx, `SELECT `+eventColumns+` FROM events WHERE task_id = $1 ORDER BY id DESC LIMIT $2`, id, limit)
}

// TaskEventsFor is TaskEvents over many cards at once, keyed by task id:
// one query, each card its own newest-first rows under its own limit. A
// card with no event gets no entry.
func (s *PGStore) TaskEventsFor(ctx context.Context, ids []string, limit int) (map[string][]Event, error) {
	out := map[string][]Event{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+eventColumns+` FROM
		(SELECT *, row_number() OVER (PARTITION BY task_id ORDER BY id DESC) AS rn
			FROM events WHERE task_id = ANY($1)) ranked
		WHERE rn <= $2 ORDER BY task_id, id DESC`, ids, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanEvent(rows.Scan)
		if err != nil {
			return nil, err
		}
		out[e.TaskID] = append(out[e.TaskID], e)
	}
	return out, rows.Err()
}

// ReachedReview reads changeStatus's record: a task_status event that
// says in-review.
func (s *PGStore) ReachedReview(ctx context.Context, id string) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE task_id = $1 AND kind = $2 AND detail->>'status' = 'in-review')`,
		id, EventTaskStatus).Scan(&ok)
	return ok, err
}

// FiledPR reads project name's last project_filed event: the infra pull
// request the console last filed the project with; false for none.
func (s *PGStore) FiledPR(ctx context.Context, name string) (int64, bool, error) {
	var pr int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE((detail->>'pr')::bigint, 0) FROM events WHERE repository = $1 AND kind = $2
		ORDER BY id DESC LIMIT 1`, name+"/"+name, EventProjectFiled).Scan(&pr)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return pr, err == nil && pr > 0, err
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
