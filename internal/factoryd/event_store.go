package factoryd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// The ways an event reached the factory (one event whatever
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
	// EventFreezeRetried is the retry press on a freeze card, a human's or
	// the project agent's, which cleared its class's hold: detail
	// {"class", "closed", the freeze cards it closed, "woken" of "frozen",
	// the frozen cards it woke}.
	EventFreezeRetried = "freeze_retried"
	// EventCardHeld is a human's hold put on a card, or put on it again:
	// detail {"reason", and "release", the release card's issue, when it
	// names one}.
	EventCardHeld = "card_held"
	// EventCardHoldEnded is a card's hold ended: by a human's release,
	// detail {"reason"}; as factoryd's row by its release card, detail
	// {"release", "status"}; or by the card's own move to a final status
	// or the human lane, detail {"reason", "card_status"}.
	EventCardHoldEnded = "card_hold_ended"
	// EventCardHoldStranded is factoryd's row on a held card whose release
	// card ended failed or closed and so releases nothing, once per hold
	// and end, which the Poker tells the human: detail {"release_task",
	// "release", the release card's name and its plan's, "release_issue",
	// "status"}.
	EventCardHoldStranded = "card_hold_stranded"
	// EventCardExtended is a raise of a card's session cap and agent-time
	// cap, the starts and the time of Give more room, whose money a
	// card_funded row holds: detail {"sessions", "agent_time_s",
	// "amount_eur", "from_eur", "to_eur"}.
	EventCardExtended = "card_extended"
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
// question_answered answers: detail {"question", "comment", "source",
// "reason", and "choices" when the agent offered any}; source and
// reason name the case the ask opens (caseKindOf). "settled" carries the
// answer factoryd gave as it asked (an identical plan re-file's
// confirmation): such an ask pokes and wakes no one.
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

// EventIssueClosed is the webhook's record of an ended card's issue
// closed, by its closer: detail {"issue"}. The monitor reads it, so the
// case that asks the issue closed ends with it.
const EventIssueClosed = "issue_closed"

// EventIssueClosedWithoutWriteAccess is factoryd's note, once per close, that
// the actor, a user who cannot write the repository, closed a live card's
// issue and the close ended nothing: detail {"issue"}.
const EventIssueClosedWithoutWriteAccess = "issue_closed_without_write_access"

// EventFreezeIssueRefused is factoryd's record that Forgejo refused a
// freeze card's issue for good, a deleted issue or a 4xx, so the card is
// given up and the class's next arm files a new one: detail {"class",
// "issue", "error"}.
const EventFreezeIssueRefused = "freeze_issue_refused"

// EventPlanReplanned is factoryd's record of a replan's revision filed on
// its plan: detail {"revision", "kept", "changed", "dropped", "added",
// "origin", "by", "comment"}, the old cards' fates and the new cards
// without a from, and who asked: a person, or the cut of a card; a
// round's: {"revision", "origin": "round", "round", "cards"}, the issues
// of the round's cards in filing order.
const EventPlanReplanned = "plan_replanned"

// EventPlanRevision is the row of a planner session's filed revision.
const EventPlanRevision = "plan_revision"

// EventChatBudgetSpent is factoryd's record, on a project's chat task of
// the month, of the judgment budget being spent: the gateway refusing a
// chat call for that task's own budget (refused_budget), or a case that
// went to the human unjudged for it. Detail {"budget_eur", "spent_eur",
// "suggest_eur"}: the budget it stopped at, the spend then and the fund
// the chat offers (SuggestFund). The chat stands while the task's budget is not above
// budget_eur, until a fund or the policy's raise or the month's turn. The
// poke it raises asks at the needs-you level.
const EventChatBudgetSpent = "chat_budget_spent"

// EventChatTokenMade and EventChatTokenRevoked are the console's record of
// a human making or revoking a chat token: detail {"token": {"id", "name"},
// "project"}, the token as a token's stamp names it, and its repository.
const (
	EventChatTokenMade    = "chat_token_made"    // #nosec G101 -- an event kind
	EventChatTokenRevoked = "chat_token_revoked" // #nosec G101 -- an event kind
)

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
	// TaskEventsOfKind lists task id's events of kind newest first, at
	// most limit, however many events of other kinds follow them.
	TaskEventsOfKind(ctx context.Context, id, kind string, limit int) ([]Event, error)
	// TaskEventsFor is TaskEvents over many cards at once, keyed by task
	// id. A card with no event gets no entry.
	TaskEventsFor(ctx context.Context, ids []string, limit int) (map[string][]Event, error)
	// NewestTaskEvents are the newest event of each of tasks, keyed by task
	// id: what the board's cards read their failure and stale ring from. A
	// task with no event gets no entry.
	NewestTaskEvents(ctx context.Context, tasks []string) (map[string]Event, error)
	// ClosedAsks lists the cards of tasks whose newest ask or answer is the
	// factory's withdrawal of an ask with a reason, keyed by task id.
	ClosedAsks(ctx context.Context, tasks []string) (map[string]ClosedAsk, error)
}

func (s *PGStore) Record(ctx context.Context, e Event) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after the commit
	e, inserted, err := s.insertEvent(ctx, tx, e)
	if err != nil || !inserted {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
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

// insertEvent writes e as a row of tx, and the cases it opens, appends
// to or ends (casesFromEvent); inserted is false for the row a conflict
// dropped, which touches no case.
func (s *PGStore) insertEvent(ctx context.Context, tx *sql.Tx, e Event) (_ Event, inserted bool, _ error) {
	if e.Repository == "" || e.Kind == "" {
		return e, false, errors.New("event: repository and kind are required")
	}
	if e.Kind == EventPlanClosed && e.TaskID != "" {
		// the plan's task row serializes its closers: one plan_closed row
		var stands bool
		if _, err := tx.ExecContext(ctx, `SELECT 1 FROM tasks WHERE id = $1 FOR UPDATE`, e.TaskID); err != nil {
			return e, false, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE task_id = $1 AND kind = $2)`, e.TaskID, EventPlanClosed).Scan(&stands); err != nil || stands {
			return e, false, err
		}
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
	err := tx.QueryRowContext(ctx,
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
	if err := s.casesFromEvent(ctx, tx, e); err != nil {
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

// Filing is the console's card_filed row of an issue as the intake reads
// it: By, the human a chat filing was made for, Unchecked, a brief that
// filed on its writer's last try with its refusals standing, and Chat,
// the project agent wrote the card's text, whoever confirmed it.
type Filing struct {
	By        string
	Unchecked bool
	Chat      bool
}

// FiledFor is the console's card_filed row of repository's issue.
func (s *PGStore) FiledFor(ctx context.Context, repository string, issue int64) (Filing, bool, error) {
	var f Filing
	err := s.db.QueryRowContext(ctx, `SELECT actor, COALESCE((detail->>'brief_unchecked')::boolean, false), via = $4 FROM events
		WHERE repository = $1 AND issue_number = $2 AND kind = $3 ORDER BY id LIMIT 1`,
		repository, issue, EventCardFiled, ViaChat).Scan(&f.By, &f.Unchecked, &f.Chat)
	if errors.Is(err, sql.ErrNoRows) {
		return Filing{}, false, nil
	}
	return f, err == nil, err
}

// MarkChatFiled gives the card of repository's issue its chat filing's
// marks, when the issue's card_filed row is a chat filing's: the card is
// the project agent's, unchecked as the row says, and its why line the
// filing's own, why, in place of the one a human's issue takes from its
// text. The console records its row after it posts the issue, so the
// intake may make the card first; both call it once the card or the row
// is theirs, and whichever comes second marks the card.
func (s *PGStore) MarkChatFiled(ctx context.Context, repository string, issue int64, why string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET chat_filed = true, brief_unchecked = f.unchecked, why = $5
		FROM (SELECT via, COALESCE((detail->>'brief_unchecked')::boolean, false) AS unchecked FROM events
			WHERE repository = $1 AND issue_number = $2 AND kind = $3 ORDER BY id LIMIT 1) f
		WHERE f.via = $4 AND tasks.repository = $1 AND tasks.issue_number = $2 AND NOT tasks.chat_filed`,
		repository, issue, EventCardFiled, ViaChat, why)
	return err
}

func (s *PGStore) TaskEvents(ctx context.Context, id string, limit int) ([]Event, error) {
	return s.events(ctx, `SELECT `+eventColumns+` FROM events WHERE task_id = $1 ORDER BY id DESC LIMIT $2`, id, limit)
}

// TaskEventsOfKind is task id's events of kind newest first, at most limit.
func (s *PGStore) TaskEventsOfKind(ctx context.Context, id, kind string, limit int) ([]Event, error) {
	return s.events(ctx, `SELECT `+eventColumns+` FROM events WHERE task_id = $1 AND kind = $2 ORDER BY id DESC LIMIT $3`, id, kind, limit)
}

// TaskEventsFor is TaskEvents over many cards at once, keyed by task id:
// one query, each card its own newest-first rows under its own limit. A
// card with no event gets no entry.
func (s *PGStore) TaskEventsFor(ctx context.Context, ids []string, limit int) (map[string][]Event, error) {
	out := map[string][]Event{}
	if len(ids) == 0 {
		return out, nil
	}
	// each card's newest rows off the (task_id, id) index, not its whole
	// history
	rows, err := s.db.QueryContext(ctx, `SELECT `+eventColumns+` FROM unnest($1::text[]) AS card(task),
		LATERAL (SELECT * FROM events WHERE task_id = card.task ORDER BY id DESC LIMIT $2) newest
		ORDER BY task_id, id DESC`, ids, limit)
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

// A ClosedAsk is the factory's close of a card's ask at the human: when it
// closed it, its reason, the moments the reason names and how many distinct
// statuses the card's task_status rows have named since.
type ClosedAsk struct {
	At       time.Time
	Reason   string
	Moments  Moments
	Statuses int
}

// ClosedAsks is read as the Events interface says: a card is in the map
// when its newest question_asked or settling event is a question_withdrawn
// by factoryd with a reason.
func (s *PGStore) ClosedAsks(ctx context.Context, tasks []string) (map[string]ClosedAsk, error) {
	out := map[string]ClosedAsk{}
	if len(tasks) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, w.at, w.detail,
		(SELECT count(DISTINCT s.detail->>'status') FROM events s WHERE s.task_id = t.id AND s.kind = $4 AND s.id > w.id)
		FROM unnest($1::text[]) AS t(id)
		CROSS JOIN LATERAL (SELECT id, at, kind, actor, detail FROM events
			WHERE task_id = t.id AND kind = ANY($2) ORDER BY id DESC LIMIT 1) w
		WHERE w.kind = $3 AND w.actor = $5 AND COALESCE(w.detail->>'reason', '') <> ''`,
		tasks, append([]string{EventQuestionAsked}, answerKinds...), EventQuestionWithdrawn, EventTaskStatus, FactoryDecider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var c ClosedAsk
		var raw []byte
		if err := rows.Scan(&id, &c.At, &raw, &c.Statuses); err != nil {
			return nil, err
		}
		var detail map[string]any
		if err := json.Unmarshal(raw, &detail); err != nil {
			return nil, err
		}
		c.Reason, _ = detail["reason"].(string)
		c.Moments = MomentsOf(detail)
		out[id] = c
	}
	return out, rows.Err()
}

// SessionsStartedSince counts the session_started rows of every card after
// since.
func (s *PGStore) SessionsStartedSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE kind = $1 AND at > $2`, EventSessionStarted, since).Scan(&n)
	return n, err
}

// eventRecorder is the Timeline's sink: the store that turns factoryd's own
// events into rows beside their timeline lines. A nil recorder records
// nothing, so a Timeline without one keeps writing only timeline lines.
type eventRecorder interface {
	Record(ctx context.Context, e Event) (int64, error)
}
