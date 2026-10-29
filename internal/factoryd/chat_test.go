package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

func chatStore(t *testing.T) *PGStore {
	t.Helper()
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_chat"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.db.Close() })
	s.SetSealKey([]byte("0123456789abcdef0123456789abcdef"))
	if _, err := s.db.ExecContext(t.Context(), `TRUNCATE tasks, chat_messages, chat_attachments, chat_conversations, chat_proposals, chat_seen, ledger, events CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

// A project's chat task is filed once a month, running, with the month's
// budget; a later call that month changes nothing, the next month files a
// new task and closes the last month's, and none is a card of the board.
func TestPGStoreChatTaskPerMonth(t *testing.T) {
	s, ctx := chatStore(t), context.Background()
	oct := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	got, err := s.EnsureChatTask(ctx, "linkshort", "linkshort/linkshort", 5, oct)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "t-chat-linkshort-2026-10" || got.Kind != KindChat || got.Status != "running" || got.BudgetEUR != 5 ||
		got.Repository != "linkshort/linkshort" || got.Gate != "" || !slices.Equal(got.RequiredCapabilities, []string{policy.ClassChat}) {
		t.Fatalf("chat task = %+v", got)
	}
	again, err := s.EnsureChatTask(ctx, "linkshort", "linkshort/linkshort", 3, oct.Add(20*24*time.Hour))
	if err != nil || again.ID != got.ID || again.BudgetEUR != 5 {
		t.Fatalf("the month's second call = %+v, %v: want the first task, budget 5", again, err)
	}
	other, err := s.EnsureChatTask(ctx, "code", "sf/code", 5, oct)
	if err != nil {
		t.Fatal(err)
	}
	nov, err := s.EnsureChatTask(ctx, "linkshort", "linkshort/linkshort", 3, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || nov.ID != "t-chat-linkshort-2026-11" || nov.BudgetEUR != 3 || nov.Status != "running" {
		t.Fatalf("November = %+v, %v", nov, err)
	}
	if was, _, err := s.Task(ctx, got.ID); err != nil || was.Status != "closed" || was.FinishedAt == nil {
		t.Fatalf("October once November is filed = %+v, %v: want closed, finished", was, err)
	}
	if code, _, err := s.Task(ctx, other.ID); err != nil || code.Status != "running" {
		t.Fatalf("another project's chat task = %+v, %v: want it running", code, err)
	}
	if err := s.CreateTask(ctx, Task{ID: "t-card", Kind: "agent", Status: "queued", Repository: "linkshort/linkshort", IssueNumber: 4, Track: "direct"}); err != nil {
		t.Fatal(err)
	}
	board, err := s.BoardTasks(ctx, "linkshort/linkshort")
	if err != nil {
		t.Fatal(err)
	}
	if len(board) != 1 || board[0].ID != "t-card" {
		t.Fatalf("board = %+v, want only t-card", board)
	}
	if other, err := s.BoardTasks(ctx, "sf/code"); err != nil || len(other) != 0 {
		t.Fatalf("sf/code's board = %+v, %v: want no card, its open task is a chat's", other, err)
	}
	if _, ok, err := s.BoardTask(ctx, got.ID); ok || err != nil {
		t.Fatalf("BoardTask(chat) = %v, %v: want none", ok, err)
	}
}

// A raise of the policy's monthly chat budget reaches the month's running
// chat task at its next ensure, and a policy below a funded budget leaves
// the fund standing: the budget never falls.
func TestPGStoreChatTaskTakesAPolicyRaise(t *testing.T) {
	s, ctx := chatStore(t), context.Background()
	oct := time.Date(2026, 10, 28, 22, 0, 0, 0, time.UTC)
	if got, err := s.EnsureChatTask(ctx, "code", "sf/code", 15, oct); err != nil || got.BudgetEUR != 15 {
		t.Fatalf("the first ensure = %+v, %v: want budget 15", got, err)
	}
	if got, err := s.EnsureChatTask(ctx, "code", "sf/code", 30, oct.Add(time.Hour)); err != nil || got.BudgetEUR != 30 {
		t.Fatalf("the ensure under a policy of 30 = %+v, %v: want budget 30", got, err)
	}
	if _, err := s.EnsureChatTask(ctx, "linkshort", "linkshort/linkshort", 15, oct); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RaiseTaskBudget(ctx, "t-chat-linkshort-2026-10", 3); err != nil {
		t.Fatal(err)
	}
	if got, err := s.EnsureChatTask(ctx, "linkshort", "linkshort/linkshort", 5, oct.Add(time.Hour)); err != nil || got.BudgetEUR != 18 {
		t.Fatalf("the ensure under a policy of 5 after a fund to 18 = %+v, %v: want budget 18", got, err)
	}
}

// A conversation is one member's on one project: its newest messages,
// oldest first, the tool calls with their results as stored.
func TestPGStoreChatMessages(t *testing.T) {
	s, ctx := chatStore(t), context.Background()
	calls := []ChatToolCall{{ID: "call_1", Name: "board", Arguments: "{}", Result: "Backlog (1): #4"}}
	for _, m := range []struct {
		repo, login string
		m           ChatMessage
	}{
		{"linkshort/linkshort", "anna", ChatMessage{Role: "user", Content: "what is open?"}},
		{"linkshort/linkshort", "bob", ChatMessage{Role: "user", Content: "bob's"}},
		{"sf/code", "anna", ChatMessage{Role: "user", Content: "other project"}},
		{"linkshort/linkshort", "anna", ChatMessage{Role: "assistant", ToolCalls: calls}},
		{"linkshort/linkshort", "anna", ChatMessage{Role: "assistant", Content: "One card, #4."}},
	} {
		if _, err := s.AddChatMessage(ctx, m.repo, m.login, m.m); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.ChatMessages(ctx, "linkshort/linkshort", "anna", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var got []ChatMessage
	for _, m := range all {
		if m.ID == 0 || m.At.IsZero() {
			t.Fatalf("message without id or time: %+v", m)
		}
		got = append(got, ChatMessage{Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls})
	}
	if len(got) != 3 || got[0].Role != "user" || got[0].Content != "what is open?" ||
		got[1].Role != "assistant" || len(got[1].ToolCalls) != 1 ||
		got[1].ToolCalls[0].ID != calls[0].ID || got[1].ToolCalls[0].Name != calls[0].Name ||
		got[1].ToolCalls[0].Result != calls[0].Result || got[1].ToolCalls[0].ReadAt.IsZero() ||
		got[2].Role != "assistant" || got[2].Content != "One card, #4." {
		t.Fatalf("conversation = %+v", got)
	}
	newest, err := s.ChatMessages(ctx, "linkshort/linkshort", "anna", 0, 2)
	if err != nil || len(newest) != 2 || newest[0].ID != all[1].ID || newest[1].ID != all[2].ID {
		t.Fatalf("newest 2 = %+v, %v", newest, err)
	}
}

// Chat attachments store with their message and list oldest first under
// the conversation; by id a row of any member's thread reads, another
// project's id is none.
func TestPGStoreChatAttachments(t *testing.T) {
	s, ctx := chatStore(t), context.Background()
	id, err := s.AddChatMessage(ctx, "linkshort/linkshort", "anna", ChatMessage{Role: "user", Content: "look", Conversation: 1})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.AddChatAttachment(ctx, "linkshort/linkshort", "anna", ChatAttachment{Conversation: 1, MessageID: id,
		Name: "img1", FileName: "shot.png", ContentType: "image/png", Bytes: 95})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AddChatAttachment(ctx, "linkshort/linkshort", "anna", ChatAttachment{Conversation: 1, MessageID: id,
		Name: "img2", FileName: "chart.png", ContentType: "image/png", Bytes: 95})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == 0 || first.At.IsZero() || first.ID == second.ID {
		t.Fatalf("attachments without ids or times: %+v %+v", first, second)
	}
	atts, err := s.ChatAttachments(ctx, "linkshort/linkshort", "anna", 1)
	if err != nil || len(atts) != 2 || atts[0].Name != "img1" || atts[0].FileName != "shot.png" ||
		atts[0].ContentType != "image/png" || atts[0].Bytes != 95 || atts[0].MessageID != id || atts[1].Name != "img2" {
		t.Fatalf("the conversation's attachments = %+v, %v", atts, err)
	}
	if other, err := s.ChatAttachments(ctx, "linkshort/linkshort", "bob", 1); err != nil || len(other) != 0 {
		t.Fatalf("another member's attachments = %+v, %v: want none", other, err)
	}
	got, ok, err := s.ChatAttachmentByID(ctx, "linkshort/linkshort", first.ID)
	if err != nil || !ok || got.Name != "img1" || got.Conversation != 1 {
		t.Fatalf("by id = %+v, %v, %v", got, ok, err)
	}
	if _, ok, err := s.ChatAttachmentByID(ctx, "sf/code", first.ID); err != nil || ok {
		t.Fatalf("another project's id = %v, %v: want none", ok, err)
	}
}

// A project's month spend sums its tasks' ledger rows of that UTC month,
// the chat's as its own line; another project's rows and another month's
// stay out.
func TestPGStoreProjectMonthSpend(t *testing.T) {
	s, ctx := chatStore(t), context.Background()
	oct := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	chat, err := s.EnsureChatTask(ctx, "linkshort", "linkshort/linkshort", 5, oct)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []Task{{ID: "t-card", Kind: "agent", Status: "running", Repository: "linkshort/linkshort", IssueNumber: 4, Track: "direct"},
		{ID: "t-other", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 4, Track: "direct"}} {
		if err := s.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		task, day string
		eur       float64
	}{
		{chat.ID, "2026-10-02", 0.25}, {chat.ID, "2026-10-11", 0.5}, {"t-card", "2026-10-05", 1.5},
		{"t-card", "2026-09-30", 4}, {"t-card", "2026-11-01", 8}, {"t-other", "2026-10-05", 16},
	} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO ledger (task_id, day, kind, ref, eur) VALUES ($1, $2, 'model', 'x', $3)`,
			row.task, row.day, row.eur); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ProjectMonthSpend(ctx, "linkshort/linkshort", oct)
	if err != nil {
		t.Fatal(err)
	}
	if got != (ProjectSpend{EUR: 2.25, ChatEUR: 0.75}) {
		t.Fatalf("spend = %+v, want 2.25 with the chat's 0.75", got)
	}
}

// A task's spend since a time sums its ledger rows booked from that time
// on: an earlier row of the task and another task's row stay out.
func TestPGStoreTaskSpendSince(t *testing.T) {
	s, ctx := chatStore(t), context.Background()
	since := time.Date(2026, 10, 21, 22, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		task string
		at   time.Time
		eur  float64
	}{
		{"t-chat-code-2026-10", since.Add(-time.Second), 4}, {"t-chat-code-2026-10", since, 0.25},
		{"t-chat-code-2026-10", since.Add(6 * 24 * time.Hour), 0.5}, {"t-card", since.Add(time.Hour), 8},
	} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur, created_at) VALUES ($1, 'model', 'x', $2, $3)`,
			row.task, row.eur, row.at); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.TaskSpendSince(ctx, "t-chat-code-2026-10", since); err != nil || got != 0.75 {
		t.Fatalf("the spend since %s = %v, %v; want 0.75", since, got, err)
	}
}

// The threads a fund resumes are those whose newest row is the chat
// budget's stop: a thread answered after its stop, a stop row of a factory
// cap, an archived thread and another project's thread are not.
func TestPGStoreBudgetStoppedThreads(t *testing.T) {
	s, ctx := chatStore(t), context.Background()
	stop := ChatMessage{Role: "assistant", Kind: MessageBudget, BirthKind: EventChatBudgetSpent, Content: "The chat's budget is used up."}
	for _, m := range []struct {
		repo, login string
		m           ChatMessage
	}{
		{"sf/code", "anna", ChatMessage{Role: "user", Content: "what is open?", Conversation: 1}},
		{"sf/code", "anna", withConversation(stop, 1)},
		{"sf/code", "anna", withConversation(stop, 2)},
		{"sf/code", "anna", ChatMessage{Role: "user", Content: "go on", Conversation: 2}},
		{"sf/code", "anna", ChatMessage{Role: "assistant", Kind: MessageBudget, Content: "The factory's month cap is used up.", Conversation: 3}},
		{"sf/code", "anna", withConversation(stop, 4)},
		{"sf/code", "bob", withConversation(stop, 1)},
		{"linkshort/linkshort", "anna", withConversation(stop, 1)},
	} {
		if _, err := s.AddChatMessage(ctx, m.repo, m.login, m.m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ArchiveConversation(ctx, "sf/code", "anna", 4, true); err != nil {
		t.Fatal(err)
	}
	got, err := s.BudgetStoppedThreads(ctx, "sf/code")
	if err != nil {
		t.Fatal(err)
	}
	if want := []ChatConversation{{Login: "anna", Number: 1}, {Login: "bob", Number: 1}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the stopped threads = %+v, want %+v", got, want)
	}
}

// withConversation is m in thread n.
func withConversation(m ChatMessage, n int64) ChatMessage {
	m.Conversation = n
	return m
}

// A card's history is its own events, newest first, at most limit.
func TestPGStoreTaskEvents(t *testing.T) {
	s, ctx := chatStore(t), context.Background()
	var ids []int64
	for _, e := range []Event{
		{Repository: "sf/r", TaskID: "t-1", Kind: EventCardFiled, Actor: "a", Via: ViaButton},
		{Repository: "sf/r", TaskID: "t-2", Kind: EventCardRetried, Actor: "a", Via: ViaButton},
		{Repository: "sf/r", TaskID: "t-1", Kind: EventSummarySet, Actor: "b", Via: ViaChat},
		{Repository: "sf/r", TaskID: "t-1", Kind: EventAccentSet, Actor: "b", Via: ViaChat},
	} {
		id, err := s.Record(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	got, err := s.TaskEvents(ctx, "t-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	var gotIDs []int64
	for _, e := range got {
		gotIDs = append(gotIDs, e.ID)
	}
	if !slices.Equal(gotIDs, []int64{ids[3], ids[2]}) || got[0].Kind != EventAccentSet || got[0].Via != ViaChat {
		t.Fatalf("TaskEvents(t-1, 2) = %+v, want ids %d, %d", got, ids[3], ids[2])
	}
}

// A proposal is taken once, as its kind, by its member, on its project,
// until it expires: another member's take leaves it, another project's or
// another kind's finds none and leaves it too.
func TestPGStoreTakeProposal(t *testing.T) {
	s, ctx := chatStore(t), context.Background()
	now := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	p := ChatProposal{Token: "tok-1", Kind: "proposal", Repository: "sf/code", Login: "anna", Tool: "new_card",
		Args: json.RawMessage(`{"title": "Cache metrics", "track": "direct"}`), Description: "File a card, title Cache metrics, track direct",
		ExpiresAt: now.Add(30 * time.Minute)}
	if err := s.PutProposal(ctx, p); err != nil {
		t.Fatal(err)
	}
	usedAt := func(token string) *time.Time {
		var at *time.Time
		if err := s.db.QueryRowContext(ctx, `SELECT used_at FROM chat_proposals WHERE token = $1`, token).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	if _, err := s.TakeProposal(ctx, "proposal", "tok-1", "sf/code", "bob", now); !errors.Is(err, ErrOthersProposal) {
		t.Fatalf("bob's take = %v, want ErrOthersProposal", err)
	}
	if at := usedAt("tok-1"); at != nil {
		t.Fatalf("used_at after bob's take = %v, want NULL", at)
	}
	if _, err := s.TakeProposal(ctx, "proposal", "tok-1", "linkshort/linkshort", "anna", now); !errors.Is(err, ErrNoProposal) {
		t.Fatalf("a take on another project = %v, want ErrNoProposal", err)
	}
	if _, err := s.TakeProposal(ctx, "undo", "tok-1", "sf/code", "anna", now); !errors.Is(err, ErrNoProposal) {
		t.Fatalf("a take as an undo = %v, want ErrNoProposal", err)
	}
	if at := usedAt("tok-1"); at != nil {
		t.Fatalf("used_at after the take as an undo = %v, want NULL", at)
	}
	got, err := s.TakeProposal(ctx, "proposal", "tok-1", "sf/code", "anna", now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ExpiresAt.Equal(p.ExpiresAt) {
		t.Fatalf("expires_at = %v, want %v", got.ExpiresAt, p.ExpiresAt)
	}
	got.ExpiresAt = p.ExpiresAt
	if !reflect.DeepEqual(got, p) {
		t.Fatalf("taken = %+v\nwant    %+v", got, p)
	}
	if at := usedAt("tok-1"); at == nil || !at.Equal(now) {
		t.Fatalf("used_at = %v, want %v", at, now)
	}
	if _, err := s.TakeProposal(ctx, "proposal", "tok-1", "sf/code", "anna", now); !errors.Is(err, ErrNoProposal) {
		t.Fatalf("a second take = %v, want ErrNoProposal", err)
	}
	undo := ChatProposal{Token: "tok-2", Kind: "undo", Repository: "sf/code", Login: "anna", Tool: "set_accent",
		Args: json.RawMessage(`{"accent": "blue", "task_id": "t-m"}`), Description: "set_accent", ExpiresAt: now}
	if err := s.PutProposal(ctx, undo); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeProposal(ctx, "undo", "tok-2", "sf/code", "anna", now.Add(time.Second)); !errors.Is(err, ErrNoProposal) {
		t.Fatalf("a take a second after expiry = %v, want ErrNoProposal", err)
	}
}

// The purge deletes the rows expired before its time, used or not, but a
// used proposal, which stays so the conversation reads it used; it keeps
// the rows not yet expired.
func TestPGStorePurgeProposals(t *testing.T) {
	s, ctx := chatStore(t), context.Background()
	now := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	for _, p := range []struct {
		token, kind string
		expires     time.Time
	}{
		{"old-1", "proposal", now.Add(-10 * time.Minute)}, {"old-2", "proposal", now.Add(-time.Second)},
		{"used", "proposal", now.Add(20 * time.Minute)}, {"open", "proposal", now.Add(20 * time.Minute)},
		{"used-old", "proposal", now.Add(-time.Second)}, {"undo-old", "undo", now.Add(-time.Second)},
	} {
		if err := s.PutProposal(ctx, ChatProposal{Token: p.token, Kind: p.kind, Repository: "sf/code", Login: "anna", Tool: "new_card",
			Args: json.RawMessage(`{}`), ExpiresAt: p.expires}); err != nil {
			t.Fatal(err)
		}
	}
	for _, take := range []struct {
		kind, token string
		at          time.Time
	}{{"proposal", "used", now.Add(-5 * time.Minute)}, {"proposal", "used-old", now.Add(-10 * time.Minute)}, {"undo", "undo-old", now.Add(-10 * time.Minute)}} {
		if _, err := s.TakeProposal(ctx, take.kind, take.token, "sf/code", "anna", take.at); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PurgeProposals(ctx, now)
	if err != nil || n != 3 {
		t.Fatalf("purged %d, %v, want 3", n, err)
	}
	var left []string
	rows, err := s.db.QueryContext(ctx, `SELECT token FROM chat_proposals ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			t.Fatal(err)
		}
		left = append(left, token)
	}
	if want := []string{"open", "used", "used-old"}; !slices.Equal(left, want) {
		t.Fatalf("rows left = %q, want %q", left, want)
	}
}

// A member's rows land in their current conversation: 1 until they start
// a new one, which is one past their highest with rows, so a second start
// without a message keeps the number. ChatMessages reads the current one
// unless told another, Conversations lists those with rows, newest first,
// and another member's or project's conversation stays as it was.
func TestPGStoreChatConversations(t *testing.T) {
	s, ctx := chatStore(t), context.Background()
	add := func(repo, login, content string) {
		t.Helper()
		if _, err := s.AddChatMessage(ctx, repo, login, ChatMessage{Role: "user", Content: content}); err != nil {
			t.Fatal(err)
		}
	}
	read := func(repo, login string, conversation int64) []string {
		t.Helper()
		msgs, err := s.ChatMessages(ctx, repo, login, conversation, 100)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range msgs {
			out = append(out, fmt.Sprintf("%d %s", m.Conversation, m.Content))
		}
		return out
	}
	number := func(what string, n int64, err error, want int64) {
		t.Helper()
		if err != nil || n != want {
			t.Fatalf("%s = %d, %v, want %d", what, n, err, want)
		}
	}
	n, err := s.CurrentConversation(ctx, "sf/code", "anna")
	number("anna's current before any row", n, err, 1)
	add("sf/code", "anna", "first")
	add("sf/code", "anna", "second")
	add("sf/code", "bob", "bob's")
	add("linkshort/linkshort", "anna", "elsewhere")
	n, err = s.NewConversation(ctx, "sf/code", "anna")
	number("anna's new conversation", n, err, 2)
	add("sf/code", "anna", "third")
	if got, want := read("sf/code", "anna", 0), []string{"2 third"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("anna's current conversation = %q, want %q", got, want)
	}
	if got, want := read("sf/code", "anna", 1), []string{"1 first", "1 second"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("anna's conversation 1 = %q, want %q", got, want)
	}
	old, err := s.ChatMessages(ctx, "sf/code", "anna", 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	third, err := s.ChatMessages(ctx, "sf/code", "anna", 2, 100)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.Conversations(ctx, "sf/code", "anna")
	if want := []ChatConversation{{Login: "anna", Number: 2, Title: "third", State: ThreadActive, Current: true, FirstAt: third[0].At, LastAt: third[0].At, Messages: 1, Unswept: 1},
		{Login: "anna", Number: 1, Title: "first", State: ThreadActive, FirstAt: old[0].At, LastAt: old[1].At, Messages: 2, Unswept: 2}}; err != nil || !reflect.DeepEqual(list, want) {
		t.Fatalf("anna's conversations = %+v, %v, want %+v", list, err, want)
	}
	n, err = s.NewConversation(ctx, "sf/code", "anna")
	number("anna's next conversation", n, err, 3)
	n, err = s.NewConversation(ctx, "sf/code", "anna")
	number("anna's next conversation, pressed again", n, err, 3)
	n, err = s.CurrentConversation(ctx, "sf/code", "anna")
	number("anna's current", n, err, 3)
	if got := read("sf/code", "anna", 0); got != nil {
		t.Fatalf("anna's new conversation = %q, want none", got)
	}
	n, err = s.CurrentConversation(ctx, "sf/code", "bob")
	number("bob's current", n, err, 1)
	if got, want := read("sf/code", "bob", 0), []string{"1 bob's"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bob's conversation = %q, want %q", got, want)
	}
	n, err = s.CurrentConversation(ctx, "linkshort/linkshort", "anna")
	number("anna's current on linkshort", n, err, 1)
	if got, want := read("linkshort/linkshort", "anna", 0), []string{"1 elsewhere"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("anna's conversation on linkshort = %q, want %q", got, want)
	}
}

// ProposalStates answers each token's state in the order asked: pending
// while unused and unexpired, used once taken, expired after its time; a
// token with no proposal row of the member on the project counts as
// expired, as does an undo's.
func TestPGStoreProposalStates(t *testing.T) {
	s, ctx := chatStore(t), context.Background()
	now := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	for _, p := range []ChatProposal{
		{Token: "pending", Kind: "proposal", Login: "anna", ExpiresAt: now},
		{Token: "used", Kind: "proposal", Login: "anna", ExpiresAt: now.Add(30 * time.Minute)},
		{Token: "expired", Kind: "proposal", Login: "anna", ExpiresAt: now.Add(-time.Second)},
		{Token: "bobs", Kind: "proposal", Login: "bob", ExpiresAt: now.Add(30 * time.Minute)},
		{Token: "undo", Kind: "undo", Login: "anna", ExpiresAt: now.Add(30 * time.Minute)},
	} {
		p.Repository, p.Tool, p.Args = "sf/code", "new_card", json.RawMessage(`{}`)
		if err := s.PutProposal(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.TakeProposal(ctx, "proposal", "used", "sf/code", "anna", now); err != nil {
		t.Fatal(err)
	}
	states, err := s.ProposalStates(ctx, "sf/code", "anna", []string{"pending", "used", "expired", "missing", "bobs", "undo"}, now)
	want := []ProposalState{{"pending", "pending"}, {"used", "used"}, {"expired", "expired"}, {"missing", "expired"}, {"bobs", "expired"}, {"undo", "expired"}}
	if err != nil || !reflect.DeepEqual(states, want) {
		t.Fatalf("states = %+v, %v, want %+v", states, err, want)
	}
	if states, err := s.ProposalStates(ctx, "sf/code", "anna", nil, now); err != nil || len(states) != 0 {
		t.Fatalf("states of no token = %+v, %v, want none", states, err)
	}
}
