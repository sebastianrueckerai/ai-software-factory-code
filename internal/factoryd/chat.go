package factoryd

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// KindChat is the kind of a project's chat task: the standing task the
// project agent's model calls book under, its one capability
// policy.ClassChat. It runs no workflow and is no card: the board, the
// heal pass and the card actions leave it out.
const KindChat = "chat"

// ChatTaskID is project name's chat task in the UTC month of at: a new
// task each month, so its budget is the month's.
func ChatTaskID(name string, at time.Time) string {
	return "t-chat-" + name + "-" + at.UTC().Format("2006-01")
}

// ChatMessage is one row of a member's conversation with the project
// agent: the member's text (role user), or the agent's text and the tool
// calls it made with their results (role assistant), in the member's
// conversation numbered Conversation.
type ChatMessage struct {
	ID           int64          `json:"id"`
	Role         string         `json:"role"`
	Content      string         `json:"content"`
	ToolCalls    []ChatToolCall `json:"tool_calls,omitempty"`
	At           time.Time      `json:"at"`
	Conversation int64          `json:"conversation"`
	// Kind names a row factoryd wrote itself: a deterministic poke, an
	// escalation's face; "" for a member's or a turn's row.
	Kind string `json:"kind,omitempty"`
	// Queued: the row waits for the turn before it to end (roadmap 263):
	// stored at once, it moves to the end of its thread when its place
	// comes, so no writer's rows land inside a turn.
	Queued bool `json:"queued,omitempty"`
	// Choices are a poke's or an escalation's answers, offered as buttons.
	Choices []string `json:"choices,omitempty"`
	// Decider names who acted on a poke's action, the level and the agent,
	// when the agent acted alone ("assisted level · started by the agent").
	Decider string `json:"decider,omitempty"`
	// TaskID is the card a poke is about, Repeats how often the same poke
	// came again after it (74's rule: repeats only count).
	TaskID  string `json:"task_id,omitempty"`
	Repeats int    `json:"repeats,omitempty"`
	// BirthKind is the kind of the event that wrote a poke, which tells
	// the row's family in the settle rule (the notification-settling
	// concept); "" for a row no event wrote.
	BirthKind string `json:"birth_kind,omitempty"`
	// Settled and Reason are the console's read of a poke row at render:
	// the row wants nothing any more, an ask with its reason line. They
	// are derived, never stored.
	Settled bool   `json:"settled,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Thread is the conversation bound to the ask's card (the card door),
	// the reader's own; derived, never stored.
	Thread int64 `json:"thread,omitempty"`
	// ThreadOwner is that thread's member when it is another member's;
	// derived, never stored.
	ThreadOwner string `json:"thread_owner,omitempty"`
}

// ChatToolCall is one tool call of the agent and what it answered.
type ChatToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Result    string `json:"result"`
	// ReadAt is when the result was read: a later turn replays a read
	// without its result, for its time and age.
	ReadAt time.Time `json:"read_at,omitempty"`
	// Proposal is the token of a call proposed to the member.
	Proposal string `json:"proposal,omitempty"`
}

// StampReadAt gives each of calls without one the time now, so every tool
// result a turn stores carries when it was read.
func StampReadAt(calls []ChatToolCall, now time.Time) []ChatToolCall {
	for i := range calls {
		if calls[i].ReadAt.IsZero() {
			calls[i].ReadAt = now
		}
	}
	return calls
}

// EnsureChatTask is project name's chat task for the month of at, filed
// running with budget the first time: a later call that month changes
// nothing. An earlier month's task still running closes (finished now),
// so the settlement pass settles it and the gateway takes no more of its
// calls.
func (s *PGStore) EnsureChatTask(ctx context.Context, name, repository string, budget float64, at time.Time) (Task, error) {
	t := Task{ID: ChatTaskID(name, at), Kind: KindChat, Status: "running", BudgetEUR: budget, Repository: repository,
		RequiredCapabilities: []string{policy.ClassChat},
		Objective:            "the project agent's chat of " + name + " in " + at.UTC().Format("2006-01"), Source: "chat"}
	if err := s.insertTask(ctx, t, `ON CONFLICT (id) DO NOTHING`); err != nil {
		return Task{}, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'closed', finished_at = now()
		WHERE kind = $1 AND repository = $2 AND id <> $3 AND status = 'running'`, KindChat, repository, t.ID); err != nil {
		return Task{}, err
	}
	got, ok, err := s.Task(ctx, t.ID)
	if err == nil && !ok {
		err = fmt.Errorf("chat task %s not found after its insert", t.ID)
	}
	return got, err
}

// currentConversation is the current conversation of login $2 on
// repository $1: the thread they opened last; with none, one past their
// highest (1 before their first).
const currentConversation = `coalesce(
	(SELECT conversation FROM chat_conversations WHERE repository = $1 AND login = $2 AND current),
	(SELECT max(conversation) + 1 FROM chat_conversations WHERE repository = $1 AND login = $2), 1)`

// CurrentConversation is login's current conversation on repository.
func (s *PGStore) CurrentConversation(ctx context.Context, repository, login string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT `+currentConversation, repository, login).Scan(&n)
	return n, err
}

// NewConversation makes a new thread login's current one on repository
// and answers its number, one past their highest; started again before a
// message, the empty current thread stays the one.
func (s *PGStore) NewConversation(ctx context.Context, repository, login string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after the commit
	var cur int64
	var inbox bool
	var rows int64
	switch err := tx.QueryRowContext(ctx, `SELECT c.conversation, c.inbox,
		(SELECT count(*) FROM chat_messages m WHERE m.repository = c.repository AND m.login = c.login AND m.conversation = c.conversation)
		FROM chat_conversations c WHERE c.repository = $1 AND c.login = $2 AND c.current`, repository, login).Scan(&cur, &inbox, &rows); {
	case err == nil && !inbox && rows == 0:
		return cur, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return 0, err
	}
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(conversation), 1) + 1 FROM chat_conversations WHERE repository = $1 AND login = $2`,
		repository, login).Scan(&n); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_conversations SET current = false WHERE repository = $1 AND login = $2 AND current`, repository, login); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO chat_conversations (repository, login, conversation, current) VALUES ($1, $2, $3, true)`,
		repository, login, n); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// The states of a thread.
const (
	ThreadActive   = "active"
	ThreadArchived = "archived"
)

// ChatConversation is one thread of a member on a project (roadmap 239):
// its number, title, state, whether it is the member's inbox and their
// current thread, its first and last message's times (the thread's own
// when it has none), its rows, and its unread turns after the member's
// seen marker. Working is the console's: a turn runs on it now. NeedsYou
// and NewRows are the console's too: the settle rule's split of the
// unread turns, the open asks and the rest (the notification-settling
// concept).
type ChatConversation struct {
	// Login is the thread's owner, the member whose conversation number
	// it is; every listing fills it, the project's threads included.
	Login    string    `json:"login,omitempty"`
	Number   int64     `json:"number"`
	Title    string    `json:"title"`
	State    string    `json:"state"`
	Inbox    bool      `json:"inbox,omitempty"`
	Current  bool      `json:"current,omitempty"`
	FirstAt  time.Time `json:"first_at,omitzero"`
	LastAt   time.Time `json:"last_at,omitzero"`
	Messages int64     `json:"messages"`
	Unread   int64     `json:"unread"`
	Working  bool      `json:"working,omitempty"`
	NeedsYou int64     `json:"needs_you,omitempty"`
	NewRows  int64     `json:"new_rows,omitempty"`
	SeenUpto int64     `json:"-"`
	// ClearedUpto is the member's cleared marker: the console's Clear old
	// swept the rows up to it from view, the record keeps them. Cleared is
	// the console's count of the rows the marker hid on this read.
	ClearedUpto int64 `json:"-"`
	Cleared     int64 `json:"cleared,omitempty"`
	// Summary is the thread's running summary of its rows up to
	// SummaryUpto, which compaction keeps (roadmap 239); "" before the
	// first, and Compactions counts them.
	Summary     string    `json:"summary,omitempty"`
	SummaryUpto int64     `json:"summary_upto,omitempty"`
	SummaryAt   time.Time `json:"-"`
	Compactions int       `json:"compactions,omitempty"`
	// Tokens and CostEUR are the thread's model calls' totals, the
	// console's read of the audit.
	Tokens  int     `json:"tokens,omitempty"`
	CostEUR float64 `json:"cost_eur,omitempty"`
}

// unreadTurn is a row that ends a turn of the agent the member may not have
// read: an agent row of text or with a proposal, a poke, never a queued
// row.
const unreadTurn = `m.role = 'assistant' AND NOT m.queued AND (m.tool_calls IS NULL OR m.tool_calls @? '$[*].proposal')`

const conversationSelect = `SELECT c.login, c.conversation, c.title, c.state, c.inbox, c.current, c.summary, c.summary_upto, coalesce(c.summary_at, 'epoch'), c.compact_count,
	coalesce(min(m.at), c.created_at), coalesce(max(m.at), c.created_at), count(m.id),
	count(m.id) FILTER (WHERE m.id > coalesce(seen.last_seen_message_id, 0) AND ` + unreadTurn + `),
	coalesce(seen.last_seen_message_id, 0), coalesce(seen.cleared_message_id, 0)
	FROM chat_conversations c
	LEFT JOIN chat_messages m ON m.repository = c.repository AND m.login = c.login AND m.conversation = c.conversation
	LEFT JOIN chat_seen seen ON seen.repository = c.repository AND seen.login = c.login AND seen.conversation = c.conversation
	WHERE c.repository = $1`

// conversationGroup is every conversation query's grouping and order: the
// inbox first, then the threads by their last message, newest first, the
// archived last, each member's threads their own.
const conversationGroup = ` GROUP BY c.login, c.conversation, c.title, c.state, c.inbox, c.current, c.summary, c.summary_upto, c.summary_at, c.compact_count, c.created_at, seen.last_seen_message_id, seen.cleared_message_id
	ORDER BY c.inbox DESC, c.state = 'archived', coalesce(max(m.at), c.created_at) DESC, c.login DESC, c.conversation DESC`

func scanConversation(scan func(...any) error) (ChatConversation, error) {
	var c ChatConversation
	var at time.Time
	err := scan(&c.Login, &c.Number, &c.Title, &c.State, &c.Inbox, &c.Current, &c.Summary, &c.SummaryUpto, &at, &c.Compactions, &c.FirstAt, &c.LastAt, &c.Messages, &c.Unread, &c.SeenUpto, &c.ClearedUpto)
	if at.After(time.Unix(0, 0)) {
		c.SummaryAt = at
	}
	return c, err
}

// Conversations lists login's threads on repository: the inbox first,
// then the active by their last message, newest first, the archived last.
func (s *PGStore) Conversations(ctx context.Context, repository, login string) ([]ChatConversation, error) {
	rows, err := s.db.QueryContext(ctx, conversationSelect+` AND c.login = $2`+conversationGroup, repository, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatConversation
	for rows.Next() {
		c, err := scanConversation(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ProjectConversations lists every member's threads on repository, each
// named by its owner, the busiest first; there are no private threads.
func (s *PGStore) ProjectConversations(ctx context.Context, repository string) ([]ChatConversation, error) {
	rows, err := s.db.QueryContext(ctx, conversationSelect+conversationGroup, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatConversation
	for rows.Next() {
		c, err := scanConversation(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Conversation is login's thread n on repository; false for none.
func (s *PGStore) Conversation(ctx context.Context, repository, login string, n int64) (ChatConversation, bool, error) {
	c, err := scanConversation(s.db.QueryRowContext(ctx, conversationSelect+` AND c.login = $2 AND c.conversation = $3`+conversationGroup, repository, login, n).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatConversation{}, false, nil
	}
	return c, err == nil, err
}

// OpenConversation makes thread n login's current one on repository;
// ErrNotFound for none.
func (s *PGStore) OpenConversation(ctx context.Context, repository, login string, n int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after the commit
	if _, err := tx.ExecContext(ctx, `UPDATE chat_conversations SET current = false WHERE repository = $1 AND login = $2 AND current`, repository, login); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE chat_conversations SET current = true WHERE repository = $1 AND login = $2 AND conversation = $3`, repository, login, n)
	if err != nil {
		return err
	}
	if k, err := res.RowsAffected(); err != nil || k == 0 {
		return cmp.Or(err, ErrNotFound)
	}
	return tx.Commit()
}

// RenameConversation sets thread n's title; false for no such thread.
func (s *PGStore) RenameConversation(ctx context.Context, repository, login string, n int64, title string) (bool, error) {
	return s.changeThread(ctx, `title = $4`, `true`, repository, login, n, title)
}

// ArchiveConversation archives thread n, or restores it; the inbox stays.
// An archived thread is no one's current. False for no such thread.
func (s *PGStore) ArchiveConversation(ctx context.Context, repository, login string, n int64, archived bool) (bool, error) {
	if archived {
		return s.changeThread(ctx, `state = 'archived', current = false`, `NOT inbox`, repository, login, n)
	}
	return s.changeThread(ctx, `state = 'active'`, `NOT inbox`, repository, login, n)
}

// changeThread updates thread n SET set WHERE where, its args after the
// thread's three; false when no row matched.
func (s *PGStore) changeThread(ctx context.Context, set, where, repository, login string, n int64, args ...any) (bool, error) {
	q := `UPDATE chat_conversations SET ` + set + ` WHERE repository = $1 AND login = $2 AND conversation = $3 AND ` + where // #nosec G202 -- set and where are the callers' literals
	res, err := s.db.ExecContext(ctx, q, append([]any{repository, login, n}, args...)...)
	if err != nil {
		return false, err
	}
	k, err := res.RowsAffected()
	return k > 0, err
}

// DeleteConversation hard-deletes login's thread n on repository: its
// messages, its proposals and undos, its seen marker and the thread, and
// answers how many messages went. The inbox stays: ErrNotFound for it,
// as for no thread.
func (s *PGStore) DeleteConversation(ctx context.Context, repository, login string, n int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after the commit
	res, err := tx.ExecContext(ctx, `DELETE FROM chat_conversations WHERE repository = $1 AND login = $2 AND conversation = $3 AND NOT inbox`,
		repository, login, n)
	if err != nil {
		return 0, err
	}
	if k, err := res.RowsAffected(); err != nil || k == 0 {
		return 0, cmp.Or(err, ErrNotFound)
	}
	res, err = tx.ExecContext(ctx, `DELETE FROM chat_messages WHERE repository = $1 AND login = $2 AND conversation = $3`, repository, login, n)
	if err != nil {
		return 0, err
	}
	gone, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	for _, q := range []string{`DELETE FROM chat_seen WHERE repository = $1 AND login = $2 AND conversation = $3`,
		`DELETE FROM chat_proposals WHERE repository = $1 AND login = $2 AND conversation = $3`} {
		if _, err := tx.ExecContext(ctx, q, repository, login, n); err != nil {
			return 0, err
		}
	}
	return gone, tx.Commit()
}

// ThreadSummary is one compaction's result (roadmap 239): the summary of
// the thread's rows up to Upto, the alias that wrote it, and the context's
// estimated tokens before and after.
type ThreadSummary struct {
	Text          string
	Model         string
	Upto          int64
	Before, After int
}

// SaveSummary stores compaction s on login's thread n on repository and
// counts it.
func (s *PGStore) SaveSummary(ctx context.Context, repository, login string, n int64, sum ThreadSummary) error {
	_, err := s.db.ExecContext(ctx, `UPDATE chat_conversations SET summary = $4, summary_model = $5, summary_upto = $6, summary_at = now(),
		compact_count = compact_count + 1, compact_tokens_before = $7, compact_tokens_after = $8
		WHERE repository = $1 AND login = $2 AND conversation = $3`, repository, login, n, sum.Text, sum.Model, sum.Upto, sum.Before, sum.After)
	return err
}

// ThreadTokens is the newest operator call of chat thread thread after
// since as the gateway's audit booked it: its prompt and its answer's
// tokens, which the thread's next prompt starts from; 0 for none.
func (s *PGStore) ThreadTokens(ctx context.Context, thread string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT coalesce((SELECT prompt_tokens + output_tokens FROM audit
		WHERE thread = $1 AND kind = $2 AND prompt_tokens > 0 AND ts > $3 ORDER BY ts DESC, id DESC LIMIT 1), 0)`,
		thread, policy.PurposeOperator, since).Scan(&n)
	return n, err
}

// ThreadTotals are chat thread thread's model calls' tokens and modeled
// cost, every purpose's.
func (s *PGStore) ThreadTotals(ctx context.Context, thread string) (int, float64, error) {
	var tokens int
	var eur float64
	err := s.db.QueryRowContext(ctx, `SELECT coalesce(sum(prompt_tokens + output_tokens), 0)::int, coalesce(sum(call_cost_eur), 0)::float8
		FROM audit WHERE thread = $1`, thread).Scan(&tokens, &eur)
	return tokens, eur, err
}

// MarkSeen moves login's seen marker of thread n on repository to its
// newest row: the thread reads as read.
func (s *PGStore) MarkSeen(ctx context.Context, repository, login string, n int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_seen (repository, login, conversation, last_seen_message_id)
		VALUES ($1, $2, $3, (SELECT coalesce(max(id), 0) FROM chat_messages WHERE repository = $1 AND login = $2 AND conversation = $3))
		ON CONFLICT (repository, login, conversation) DO UPDATE
		SET last_seen_message_id = greatest(chat_seen.last_seen_message_id, excluded.last_seen_message_id)`, repository, login, n)
	return err
}

// MarkCleared moves login's cleared marker of thread n on repository to
// its newest row: the console's Clear old sweeps the thread's old rows
// from the member's view, the record keeps them. An open ask never leaves
// with the sweep; the console's read keeps it whatever the marker says.
func (s *PGStore) MarkCleared(ctx context.Context, repository, login string, n int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_seen (repository, login, conversation, last_seen_message_id, cleared_message_id)
		VALUES ($1, $2, $3, (SELECT coalesce(max(id), 0) FROM chat_messages WHERE repository = $1 AND login = $2 AND conversation = $3),
			(SELECT coalesce(max(id), 0) FROM chat_messages WHERE repository = $1 AND login = $2 AND conversation = $3))
		ON CONFLICT (repository, login, conversation) DO UPDATE
		SET cleared_message_id = greatest(chat_seen.cleared_message_id, excluded.cleared_message_id)`, repository, login, n)
	return err
}

// InboxTitle is the inbox thread's title until the member renames it.
const InboxTitle = "Inbox"

// EnsureInbox is login's inbox thread on repository, created the first
// time one past their highest: the one thread the agent and factoryd
// speak in uninvited (roadmap 239). It never becomes their current one:
// a current thread with no row yet gets its row first, so the inbox never
// takes its number.
func (s *PGStore) EnsureInbox(ctx context.Context, repository, login string) (int64, error) {
	return ensureInbox(ctx, s.db, repository, login)
}

// sqlRunner is a database or a transaction.
type sqlRunner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func ensureInbox(ctx context.Context, db sqlRunner, repository, login string) (int64, error) {
	if _, err := db.ExecContext(ctx, `INSERT INTO chat_conversations (repository, login, conversation, current)
		SELECT $1, $2, `+currentConversation+`, true
		WHERE NOT EXISTS (SELECT 1 FROM chat_conversations WHERE repository = $1 AND login = $2 AND (current OR inbox))
		ON CONFLICT DO NOTHING`, repository, login); err != nil {
		return 0, err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO chat_conversations (repository, login, conversation, title, inbox)
		SELECT $1, $2, coalesce(max(conversation), 0) + 1, $3, true FROM chat_conversations WHERE repository = $1 AND login = $2
		ON CONFLICT (repository, login) WHERE inbox DO NOTHING`, repository, login, InboxTitle); err != nil {
		return 0, err
	}
	var n int64
	err := db.QueryRowContext(ctx, `SELECT conversation FROM chat_conversations WHERE repository = $1 AND login = $2 AND inbox`,
		repository, login).Scan(&n)
	return n, err
}

// cardThreadOf answers the login and conversation of repository's thread
// bound to card taskID, if it has one.
func cardThreadOf(ctx context.Context, db sqlRunner, repository, taskID string) (string, int64, bool, error) {
	var login string
	var n int64
	err := db.QueryRowContext(ctx, `SELECT login, conversation FROM chat_conversations
		WHERE repository = $1 AND task_id = $2`, repository, taskID).Scan(&login, &n)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return login, n, true, nil
}

// CardThreadFor answers the login and conversation of repository's thread
// bound to card taskID, if it has one.
func (s *PGStore) CardThreadFor(ctx context.Context, repository, taskID string) (string, int64, bool, error) {
	return cardThreadOf(ctx, s.db, repository, taskID)
}

// CardThread is a card's bound thread: the member whose thread it is and
// its conversation number.
type CardThread struct {
	Login        string
	Conversation int64
}

// CardThreads is CardThreadFor over many cards at once: one query for the
// project's bindings. A card with no thread gets no entry.
func (s *PGStore) CardThreads(ctx context.Context, repository string, ids []string) (map[string]CardThread, error) {
	out := map[string]CardThread{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT task_id, login, conversation FROM chat_conversations
		WHERE repository = $1 AND task_id = ANY($2)`, repository, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var c CardThread
		if err := rows.Scan(&id, &c.Login, &c.Conversation); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

// EnsureCardThread answers the conversation bound to repository's card
// taskID, making it for login when the card has none: the first caller
// binds the thread, titled #taskID, and every later caller, whoever they
// are, gets the same one (the card door).
func (s *PGStore) EnsureCardThread(ctx context.Context, repository, login, taskID string) (int64, error) {
	for i := 0; i < 3; i++ {
		// another racing maker can eat this number; a card bound meanwhile
		// answers below
		if _, err := s.db.ExecContext(ctx, `INSERT INTO chat_conversations (repository, login, conversation, title, task_id)
			SELECT $1, $2, coalesce(max(conversation), 0) + 1, $3, $4 FROM chat_conversations WHERE repository = $1 AND login = $2
			ON CONFLICT DO NOTHING`, repository, login, "#"+taskID, taskID); err != nil {
			return 0, err
		}
		_, n, ok, err := cardThreadOf(ctx, s.db, repository, taskID)
		switch {
		case err != nil:
			return 0, err
		case ok:
			return n, nil
		}
	}
	return 0, fmt.Errorf("the thread of card %s on %s lost every race to be made", taskID, repository)
}

// threadTitle is a thread's title from its first message: its words on
// one line, at most 80 characters.
func threadTitle(content string) string {
	return textutil.Truncate(strings.Join(strings.Fields(content), " "), 80)
}

// ChatWatch is a member's stored subscription on a card: the card's
// moves wake the watching thread (the timers concept).
type ChatWatch struct {
	Repository   string
	Login        string
	Conversation int64
	TaskID       string
}

// ChatTimer is a stored one-shot timer: the Waker wakes its thread with
// its note when it comes due and the row is gone, so a rearm is a new
// row. TaskID binds it to a card, whose end takes it with the watch.
type ChatTimer struct {
	ID           int64
	Repository   string
	Login        string
	Conversation int64
	DueAt        time.Time
	Note         string
	TaskID       string
}

// wakeCapped is the refusal of an add past a project's cap of watches
// and timers: it names the count, so the tool's answer carries it.
func wakeCapped(n int) error {
	return fmt.Errorf("%w: it holds %d of %d, end one first.", ErrChatWakeCap, n, WakeTimersMax)
}

// WatchTakenError names the member whose watch already holds a card:
// one watch per card, a second member's row would wake nobody new.
type WatchTakenError struct{ Login string }

func (e *WatchTakenError) Error() string {
	return fmt.Sprintf("%s already watches this card", e.Login)
}

// AddChatWatch stores login's watch of taskID on the thread
// conversation, the row it already has for the card when there is one.
// A card another member watches is refused with the watcher named.
func (s *PGStore) AddChatWatch(ctx context.Context, repository, login string, conversation int64, taskID string) error {
	var other string
	err := s.db.QueryRowContext(ctx, `SELECT login FROM chat_watches
		WHERE repository = $1 AND task_id = $2 AND login <> $3 LIMIT 1`,
		repository, taskID, login).Scan(&other)
	if err == nil {
		return &WatchTakenError{Login: other}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// a watch the member already holds replaces itself, so it never meets
	// the cap
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM chat_watches WHERE repository = $1)
		+ (SELECT count(*) FROM chat_timers WHERE repository = $1)
		- count(*) FROM chat_watches WHERE repository = $1 AND login = $2 AND task_id = $3`,
		repository, login, taskID).Scan(&n); err != nil {
		return err
	}
	if n >= WakeTimersMax {
		return wakeCapped(n)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO chat_watches (repository, login, conversation, task_id)
		VALUES ($1, $2, $3, $4) ON CONFLICT (repository, login, task_id) DO UPDATE SET conversation = excluded.conversation`,
		repository, login, conversation, taskID)
	return err
}

// RemoveChatWatch ends login's watch of taskID and says whether there
// was one.
func (s *PGStore) RemoveChatWatch(ctx context.Context, repository, login, taskID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM chat_watches WHERE repository = $1 AND login = $2 AND task_id = $3`,
		repository, login, taskID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// AddChatTimer stores a one-shot timer that wakes the thread
// conversation with its note at due, bound to taskID's card when it is
// not empty.
func (s *PGStore) AddChatTimer(ctx context.Context, repository, login string, conversation int64, due time.Time, note, taskID string) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM chat_watches WHERE repository = $1)
		+ (SELECT count(*) FROM chat_timers WHERE repository = $1)`, repository).Scan(&n); err != nil {
		return err
	}
	if n >= WakeTimersMax {
		return wakeCapped(n)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_timers (repository, login, conversation, due_at, note, task_id)
		VALUES ($1, $2, $3, $4, $5, nullif($6, ''))`, repository, login, conversation, due, note, taskID)
	return err
}

// DueChatTimers answers repository's timers due by now, the oldest
// first.
func (s *PGStore) DueChatTimers(ctx context.Context, repository string, now time.Time) ([]ChatTimer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, repository, login, conversation, due_at, note, coalesce(task_id, '')
		FROM chat_timers WHERE repository = $1 AND due_at <= $2 ORDER BY due_at, id`, repository, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ts []ChatTimer
	for rows.Next() {
		var t ChatTimer
		if err := rows.Scan(&t.ID, &t.Repository, &t.Login, &t.Conversation, &t.DueAt, &t.Note, &t.TaskID); err != nil {
			return nil, err
		}
		ts = append(ts, t)
	}
	return ts, rows.Err()
}

// DeleteChatTimer takes repository's timer id, the fired row.
func (s *PGStore) DeleteChatTimer(ctx context.Context, repository string, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM chat_timers WHERE repository = $1 AND id = $2`, repository, id)
	return err
}

// ChatWatchesFor answers repository's watches of taskID's card, the
// oldest first.
func (s *PGStore) ChatWatchesFor(ctx context.Context, repository, taskID string) ([]ChatWatch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repository, login, conversation, task_id
		FROM chat_watches WHERE repository = $1 AND task_id = $2 ORDER BY id`, repository, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ws []ChatWatch
	for rows.Next() {
		var w ChatWatch
		if err := rows.Scan(&w.Repository, &w.Login, &w.Conversation, &w.TaskID); err != nil {
			return nil, err
		}
		ws = append(ws, w)
	}
	return ws, rows.Err()
}

// DeleteChatWatchesOfTask ends every watch of taskID's card on
// repository and takes the timers bound to the card with them.
func (s *PGStore) DeleteChatWatchesOfTask(ctx context.Context, repository, taskID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM chat_watches WHERE repository = $1 AND task_id = $2`, repository, taskID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM chat_timers WHERE repository = $1 AND task_id = $2`, repository, taskID)
	return err
}

// AddChatMessage appends m to login's thread m.Conversation on
// repository, their current one for 0, and answers its id; the thread's
// row is made with its first message's title when it has none, the
// member's current one when they have none.
func (s *PGStore) AddChatMessage(ctx context.Context, repository, login string, m ChatMessage) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after the commit
	id, err := insertChatMessage(ctx, tx, repository, login, m)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// insertChatMessage inserts m through tx as AddChatMessage stores it.
func insertChatMessage(ctx context.Context, tx sqlRunner, repository, login string, m ChatMessage) (int64, error) {
	calls := any(nil)
	if len(m.ToolCalls) > 0 {
		b, err := json.Marshal(StampReadAt(m.ToolCalls, time.Now()))
		if err != nil {
			return 0, err
		}
		calls = b
	}
	choices := any(nil)
	if len(m.Choices) > 0 {
		b, err := json.Marshal(m.Choices)
		if err != nil {
			return 0, err
		}
		choices = b
	}
	conv := m.Conversation
	if conv <= 0 {
		if err := tx.QueryRowContext(ctx, `SELECT `+currentConversation, repository, login).Scan(&conv); err != nil {
			return 0, err
		}
	}
	title := ""
	if m.Role == "user" || m.Kind != "" {
		title = threadTitle(m.Content)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO chat_conversations (repository, login, conversation, title, current, last_turn_at)
		VALUES ($1, $2, $3, $4, NOT EXISTS (SELECT 1 FROM chat_conversations WHERE repository = $1 AND login = $2 AND current), now())
		ON CONFLICT (repository, login, conversation) DO UPDATE SET last_turn_at = now(),
			title = CASE WHEN chat_conversations.title = '' THEN excluded.title ELSE chat_conversations.title END`,
		repository, login, conv, title); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO chat_messages (repository, login, role, content, tool_calls, conversation, kind, choices, decider, task_id, queued, birth_kind)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8::jsonb, $9, $10, $11, $12) RETURNING id`,
		repository, login, m.Role, m.Content, calls, conv, m.Kind, choices, m.Decider, m.TaskID, m.Queued, m.BirthKind).Scan(&id)
	return id, err
}

// StartQueued lands queued row id in its place: the row takes the next
// id, after every row of the turns before it, and is queued no longer.
// The row's pending delivery rows follow it to the landing id, so a
// notification born mid-turn still names a row the console shows.
func (s *PGStore) StartQueued(ctx context.Context, repository, login string, id int64) error {
	_, err := s.db.ExecContext(ctx, `WITH landed AS (
		UPDATE chat_messages SET id = nextval(pg_get_serial_sequence('chat_messages', 'id')), queued = false, at = now()
		WHERE id = $1 AND repository = $2 AND login = $3 AND queued RETURNING id)
		UPDATE notify_deliveries SET message_id = (SELECT id FROM landed) WHERE message_id = $1`, id, repository, login)
	return err
}

// ChatMessages are the newest limit messages of login's conversation on
// repository, oldest first: the current one for conversation 0.
func (s *PGStore) ChatMessages(ctx context.Context, repository, login string, conversation int64, limit int) ([]ChatMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, role, content, tool_calls, at, conversation, kind, choices, decider, task_id, repeats, queued, birth_kind FROM (
		SELECT * FROM chat_messages WHERE repository = $1 AND login = $2 AND conversation = coalesce(nullif($3::bigint, 0), `+currentConversation+`)
		ORDER BY id DESC LIMIT $4) newest ORDER BY id`,
		repository, login, conversation, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatMessage
	for rows.Next() {
		var m ChatMessage
		var calls, choices []byte
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &calls, &m.At, &m.Conversation, &m.Kind, &choices, &m.Decider, &m.TaskID, &m.Repeats, &m.Queued, &m.BirthKind); err != nil {
			return nil, err
		}
		if len(calls) > 0 {
			if err := json.Unmarshal(calls, &m.ToolCalls); err != nil {
				return nil, err
			}
			// a row stored before read_at was written reads back with the
			// message's own time, so no result is ever undated
			StampReadAt(m.ToolCalls, m.At)
		}
		if len(choices) > 0 {
			if err := json.Unmarshal(choices, &m.Choices); err != nil {
				return nil, err
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ChatAsks are login's ask rows on repository, every thread's, oldest
// first: the poke rows about a card that waits for a person, whose state
// the settle rule derives at read (the notification-settling concept).
func (s *PGStore) ChatAsks(ctx context.Context, repository, login string) ([]ChatMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, conversation, task_id, birth_kind FROM chat_messages
		WHERE repository = $1 AND login = $2 AND kind = $3 AND task_id <> '' ORDER BY id`, repository, login, MessageEscalation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatMessage
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.ID, &m.At, &m.Conversation, &m.TaskID, &m.BirthKind); err != nil {
			return nil, err
		}
		m.Kind = MessageEscalation
		out = append(out, m)
	}
	return out, rows.Err()
}

// ErrNoAsk says a card has no ask row in the chat: nothing waits on a
// person there for an analysis to enrich.
var ErrNoAsk = errors.New("the card asks nothing")

// AskSettledError says the ask an analysis targeted is settled already:
// Word the settling event's reason word, By its actor.
type AskSettledError struct{ Word, By string }

func (e *AskSettledError) Error() string {
	if e.By != "" {
		return fmt.Sprintf("the ask is %s by %s", e.Word, e.By)
	}
	return "the ask is " + e.Word
}

// AttachAnalysis records what the chat agent found about the card's open
// ask (the main agent concept): an analysis_attached row on the spine and
// the ask's escalation rows rewritten to carry the findings after the
// question, so every member reads them with the ask. A settled ask
// refuses as AskSettledError, a card without an ask row as ErrNoAsk.
func (s *PGStore) AttachAnalysis(ctx context.Context, repository, taskID, found, recommendation, by string) error {
	t, ok, err := s.Task(ctx, taskID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoAsk
	}
	events, err := s.TaskEvents(ctx, taskID, 500)
	if err != nil {
		return err
	}
	var ask *Event
	for i := range events { // newest first
		if events[i].Kind == EventQuestionAsked {
			ask = &events[i]
			break
		}
	}
	if ask == nil {
		return ErrNoAsk
	}
	if st := SettleOf(ChatMessage{Kind: MessageEscalation, BirthKind: EventQuestionAsked, At: ask.At},
		events, t.Status == "waiting-human", false); !st.Open {
		return &AskSettledError{Word: st.Word, By: st.By}
	}
	// the ask's rows: the poke line the Poker wrote, alone or with an
	// earlier analysis a new attach replaces; rows of an older ask keep
	// their own question
	question, _ := ask.Detail["question"].(string)
	ref := cardRef(*ask, t)
	plain := ref + " needs you: " + question
	budget := ref + " is stopped by its budget: " + question
	analysis := "\nAnalysis: " + found
	if recommendation != "" {
		analysis += "\nRecommendation: " + recommendation
	}
	detail := map[string]any{"found": found, "source": "agent"}
	if recommendation != "" {
		detail["recommendation"] = recommendation
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after the commit
	rows, err := tx.QueryContext(ctx, `SELECT id, content FROM chat_messages
		WHERE repository = $1 AND task_id = $2 AND kind = $3 AND birth_kind = $4`,
		repository, taskID, MessageEscalation, EventQuestionAsked)
	if err != nil {
		return err
	}
	var ids []int64
	var bases []string
	for rows.Next() {
		var id int64
		var content string
		if err := rows.Scan(&id, &content); err != nil {
			rows.Close() // #nosec G104 -- the scan's error is the one returned
			return err
		}
		switch {
		case content == plain, content == budget:
			ids, bases = append(ids, id), append(bases, content)
		case strings.HasPrefix(content, plain+"\nAnalysis: "):
			ids, bases = append(ids, id), append(bases, plain)
		case strings.HasPrefix(content, budget+"\nAnalysis: "):
			ids, bases = append(ids, id), append(bases, budget)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close() // #nosec G104 -- the iteration's error is the one returned
		return err
	}
	rows.Close() // #nosec G104 -- the rows read whole; the updates' errors return below
	if len(ids) == 0 {
		return ErrNoAsk
	}
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE chat_messages SET content = $1 WHERE id = $2`, bases[i]+analysis, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (at, repository, task_id, issue_number, kind, actor, via, detail)
		VALUES (now(), $1, $2, $3, $4, $5, $6, $7::jsonb)`,
		repository, taskID, t.IssueNumber, EventAnalysisAttached, by, ViaChat, b); err != nil {
		return err
	}
	return tx.Commit()
}

// ProposalState is a proposal's state as GET /chat answers it: pending
// while unused and unexpired, used once taken, expired otherwise.
type ProposalState struct {
	Token string `json:"token"`
	State string `json:"state"`
}

// ProposalStates is the state of each of tokens, login's proposals on
// repository, at now, in their order; a token with no such row is expired.
func (s *PGStore) ProposalStates(ctx context.Context, repository, login string, tokens []string, now time.Time) ([]ProposalState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT token, used_at, expires_at FROM chat_proposals
		WHERE kind = 'proposal' AND repository = $1 AND login = $2 AND token = ANY($3)`, repository, login, tokens)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[string]string{}
	for rows.Next() {
		var token string
		var usedAt sql.NullTime
		var expiresAt time.Time
		if err := rows.Scan(&token, &usedAt, &expiresAt); err != nil {
			return nil, err
		}
		switch {
		case usedAt.Valid:
			found[token] = "used"
		case expiresAt.Before(now):
			found[token] = "expired"
		default:
			found[token] = "pending"
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ProposalState, len(tokens))
	for i, t := range tokens {
		state, ok := found[t]
		if !ok {
			state = "expired"
		}
		out[i] = ProposalState{Token: t, State: state}
	}
	return out, nil
}

// ProjectSpend is a project's ledger spend in one UTC month, and the part
// its chat tasks booked.
type ProjectSpend struct {
	EUR, ChatEUR float64
}

// ProjectMonthSpend is repository's spend over the ledger in the UTC month
// of at, as the gateway's project headroom counts it.
func (s *PGStore) ProjectMonthSpend(ctx context.Context, repository string, at time.Time) (ProjectSpend, error) {
	var p ProjectSpend
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(sum(l.eur), 0)::float8, COALESCE(sum(l.eur) FILTER (WHERE t.kind = $3), 0)::float8
		FROM ledger l JOIN tasks t ON t.id = l.task_id
		WHERE t.repository = $1 AND l.day >= date_trunc('month', $2::timestamptz AT TIME ZONE 'UTC')::date
		AND l.day < (date_trunc('month', $2::timestamptz AT TIME ZONE 'UTC') + interval '1 month')::date`,
		repository, at, KindChat).Scan(&p.EUR, &p.ChatEUR)
	return p, err
}

// ChatProposal is a proposal of the project agent waiting for its
// member's Do it (kind proposal), or an applied action's undo (kind undo):
// tool with args, bound to the member and the project until it expires.
// Description is a proposal's line for the member, or the name of the
// tool whose action an undo undoes.
type ChatProposal struct {
	Token, Kind, Repository, Login, Tool string
	Args                                 json.RawMessage
	Description                          string
	ExpiresAt                            time.Time
	// Conversation is the thread that raised it, where its Do it's or its
	// undo's row lands (roadmap 239); 0: the member's current one.
	Conversation int64
}

// TakeProposal's refusals.
var (
	ErrNoProposal     = errors.New("no such proposal or undo")
	ErrOthersProposal = errors.New("the proposal or undo is another member's")
)

// PutProposal stores p.
func (s *PGStore) PutProposal(ctx context.Context, p ChatProposal) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_proposals (token, kind, repository, login, tool, args, description, expires_at, conversation)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9)`,
		p.Token, p.Kind, p.Repository, p.Login, p.Tool, []byte(p.Args), p.Description, p.ExpiresAt, p.Conversation)
	return err
}

// TakeProposal uses token of kind for login on repository at now, once:
// the row unused and unexpired, marked used. ErrOthersProposal for such a
// row of another login, which stays unused; ErrNoProposal for none.
func (s *PGStore) TakeProposal(ctx context.Context, kind, token, repository, login string, now time.Time) (ChatProposal, error) {
	p := ChatProposal{Token: token, Kind: kind, Repository: repository, Login: login}
	var args []byte
	err := s.db.QueryRowContext(ctx, `UPDATE chat_proposals SET used_at = $4
		WHERE token = $1 AND repository = $2 AND login = $3 AND used_at IS NULL AND expires_at >= $4 AND kind = $5
		RETURNING tool, args, description, expires_at, conversation`, token, repository, login, now, kind).
		Scan(&p.Tool, &args, &p.Description, &p.ExpiresAt, &p.Conversation)
	switch {
	case err == nil:
		p.Args = args
		return p, nil
	case !errors.Is(err, sql.ErrNoRows):
		return ChatProposal{}, err
	}
	var others bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM chat_proposals
		WHERE token = $1 AND repository = $2 AND used_at IS NULL AND expires_at >= $3 AND kind = $4)`, token, repository, now, kind).Scan(&others); err != nil {
		return ChatProposal{}, err
	}
	if others {
		return ChatProposal{}, ErrOthersProposal
	}
	return ChatProposal{}, ErrNoProposal
}

// PurgeProposals deletes the rows expired before before, but a used
// proposal, which stays so the conversation reads it used, and answers
// how many.
func (s *PGStore) PurgeProposals(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM chat_proposals WHERE expires_at < $1 AND (used_at IS NULL OR kind = 'undo')`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
