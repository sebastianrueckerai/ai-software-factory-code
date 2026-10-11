package factoryd

import (
	"context"
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
	if _, err := s.db.ExecContext(t.Context(), `TRUNCATE tasks, chat_messages, ledger, events CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

// A project's chat task is filed once a month, running, with the month's
// budget; a later call that month changes nothing, the next month files a
// new task, and neither is a card of the board.
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
	nov, err := s.EnsureChatTask(ctx, "linkshort", "linkshort/linkshort", 3, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || nov.ID != "t-chat-linkshort-2026-11" || nov.BudgetEUR != 3 {
		t.Fatalf("November = %+v, %v", nov, err)
	}
	if err := s.CreateTask(ctx, Task{ID: "t-card", Kind: "agent", Status: "queued", Repository: "linkshort/linkshort", IssueNumber: 4}); err != nil {
		t.Fatal(err)
	}
	board, err := s.BoardTasks(ctx, oct.Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(board) != 1 || board[0].ID != "t-card" {
		t.Fatalf("board = %+v, want only t-card", board)
	}
	if _, ok, err := s.BoardTask(ctx, got.ID); ok || err != nil {
		t.Fatalf("BoardTask(chat) = %v, %v: want none", ok, err)
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
		if err := s.AddChatMessage(ctx, m.repo, m.login, m.m); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.ChatMessages(ctx, "linkshort/linkshort", "anna", 100)
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
	want := []ChatMessage{{Role: "user", Content: "what is open?"}, {Role: "assistant", ToolCalls: calls}, {Role: "assistant", Content: "One card, #4."}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("conversation = %+v, want %+v", got, want)
	}
	newest, err := s.ChatMessages(ctx, "linkshort/linkshort", "anna", 2)
	if err != nil || len(newest) != 2 || newest[0].ID != all[1].ID || newest[1].ID != all[2].ID {
		t.Fatalf("newest 2 = %+v, %v", newest, err)
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
	for _, task := range []Task{{ID: "t-card", Kind: "agent", Status: "running", Repository: "linkshort/linkshort", IssueNumber: 4},
		{ID: "t-other", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 4}} {
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

// A card's history is its own events, newest first, at most limit.
func TestPGStoreTaskEvents(t *testing.T) {
	s, ctx := chatStore(t), context.Background()
	var ids []int64
	for _, e := range []Event{
		{Repository: "sf/r", TaskID: "t-1", Kind: EventCardFiled, Actor: "a", Via: ViaButton},
		{Repository: "sf/r", TaskID: "t-2", Kind: EventCardMoved, Actor: "a", Via: ViaButton},
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
