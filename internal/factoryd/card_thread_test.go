package factoryd

import (
	"fmt"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// newCardThreadStore opens a store with the chat tables empty.
func newCardThreadStore(t *testing.T) *PGStore {
	t.Helper()
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_card_threads"))
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.db.Exec(`TRUNCATE chat_messages, chat_conversations, chat_seen, chat_poke_cursor, events, tasks CASCADE`); err != nil {
		t.Fatal(err)
	}
	return s
}

// A card's thread is one per card: the first member who opens the door
// binds it, every later caller gets the same conversation, and the
// binding row carries the card and its title.
func TestEnsureCardThreadIsOnePerCard(t *testing.T) {
	s := newCardThreadStore(t)
	ctx := t.Context()
	first, err := s.EnsureCardThread(ctx, "sf/code", "sebastian", "t-1")
	if err != nil {
		t.Fatalf("the first call: %v", err)
	}
	if second, err := s.EnsureCardThread(ctx, "sf/code", "bob", "t-1"); err != nil || second != first {
		t.Fatalf("the second call: %d (%v), want the bound thread %d", second, err, first)
	}
	if who, n, ok, err := s.CardThreadFor(ctx, "sf/code", "t-1"); err != nil || !ok || who != "sebastian" || n != first {
		t.Fatalf("CardThreadFor = %q %d %t %v, want sebastian %d", who, n, ok, err, first)
	}
	var login, taskID, title string
	if err := s.db.QueryRow(`SELECT login, task_id, title FROM chat_conversations
		WHERE repository = $1 AND conversation = $2`, "sf/code", first).Scan(&login, &taskID, &title); err != nil {
		t.Fatal(err)
	}
	if login != "sebastian" || taskID != "t-1" || title != "#t-1" {
		t.Fatalf("the binding row: %q %q %q, want sebastian t-1 #t-1", login, taskID, title)
	}
	// another card's thread is its own binding, even on the same number
	// of another member
	if _, err := s.EnsureCardThread(ctx, "sf/code", "bob", "t-2"); err != nil {
		t.Fatal(err)
	}
	if who, n, ok, err := s.CardThreadFor(ctx, "sf/code", "t-2"); err != nil || !ok || who != "bob" {
		t.Fatalf("t-2's binding = %q %d %t %v, want bob's own thread", who, n, ok, err)
	}
}

// A card with a bound thread: every later ask lands in the notify inbox
// and in the card's own thread, so the door's thread holds the card's
// whole ask record.
func TestPokerAppendsLaterAsksToCardThread(t *testing.T) {
	s := newCardThreadStore(t)
	ctx := t.Context()
	if err := s.CreateTask(ctx, Task{ID: "t-1", Kind: "agent", Repository: "sf/code", IssueNumber: 603,
		Status: "waiting-human", Objective: "Session store"}); err != nil {
		t.Fatal(err)
	}
	thread, err := s.EnsureCardThread(ctx, "sf/code", "sebastian", "t-1")
	if err != nil {
		t.Fatal(err)
	}
	tree := policy.Tree{Document: policy.Document{Approvers: []string{"anna"}},
		Projects: map[string]policy.ProjectConfig{"code": {Repository: "sf/code", Notify: []string{"sebastian", "bob"}}}}
	p := &Poker{Store: s, Current: func() policy.Version { return policy.Version{Tree: tree} },
		Line: &busyLine{busy: map[string]bool{}, landed: map[string][]int64{}}}
	if err := p.Pass(ctx); err != nil { // the first pass sets the cursor
		t.Fatal(err)
	}
	ask := func(question string) {
		t.Helper()
		if _, err := s.Record(ctx, Event{Repository: "sf/code", TaskID: "t-1", Kind: EventQuestionAsked,
			Actor: FactoryDecider, Via: ViaFactoryd,
			Detail: map[string]any{"question": question, "choices": []string{"Postgres", "SQLite"}}}); err != nil {
			t.Fatal(err)
		}
		if err := p.Pass(ctx); err != nil {
			t.Fatal(err)
		}
	}
	ask("Which store?")
	ask("Which port?")
	inbox, err := s.EnsureInbox(ctx, "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	for _, th := range []struct {
		name string
		n    int64
	}{{"inbox", inbox}, {"card thread", thread}} {
		rows, err := s.ChatMessages(ctx, "sf/code", "sebastian", th.n, 10)
		if err != nil || len(rows) != 2 {
			t.Fatalf("sebastian's %s after two asks = %d rows (%v), want 2", th.name, len(rows), err)
		}
		for i, want := range []string{"#603 - Session store needs you: Which store?", "#603 - Session store needs you: Which port?"} {
			if rows[i].Content != want || rows[i].Kind != MessageEscalation || rows[i].TaskID != "t-1" {
				t.Fatalf("the %s's row %d = %+v, want the ask %q", th.name, i, rows[i], want)
			}
		}
	}
}

// ProjectConversations lists every member's threads on a repository,
// each named by its owner: the same number under two members is two
// threads, not one merged.
func TestProjectConversationsNamesEveryOwner(t *testing.T) {
	s := newCardThreadStore(t)
	ctx := t.Context()
	for _, login := range []string{"sebastian", "bob"} {
		if _, err := s.EnsureInbox(ctx, "sf/code", login); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		login string
		rows  int
	}{
		{"sebastian", 2},
		{"bob", 1},
	} {
		for i := range row.rows {
			if _, err := s.AddChatMessage(ctx, "sf/code", row.login, ChatMessage{Role: "user",
				Content: fmt.Sprintf("%s's row %d", row.login, i), Conversation: 1}); err != nil {
				t.Fatal(err)
			}
		}
	}
	list, err := s.ProjectConversations(ctx, "sf/code")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for _, th := range list {
		counts[fmt.Sprintf("%s %d %t", th.Login, th.Number, th.Inbox)] = th.Messages
	}
	want := map[string]int64{
		"sebastian 1 false": 2,
		"bob 1 false":       1,
	}
	for k, n := range want {
		if counts[k] != n {
			t.Errorf("thread %s = %d messages, want %d (all rows: %v)", k, counts[k], n, counts)
		}
	}
	inboxes := 0
	for _, th := range list {
		if th.Inbox {
			inboxes++
		}
	}
	if inboxes != 2 || len(list) != 4 {
		t.Fatalf("%d threads, %d inboxes: want 4 threads, one inbox per member: %+v", len(list), inboxes, list)
	}
}

// CardThreads is CardThreadFor for many cards at once: one read of the
// project's bindings, cards with no thread left out.
func TestCardThreads(t *testing.T) {
	s := newCardThreadStore(t)
	ctx := t.Context()
	one, err := s.EnsureCardThread(ctx, "sf/code", "sebastian", "t-1")
	if err != nil {
		t.Fatalf("bind t-1: %v", err)
	}
	two, err := s.EnsureCardThread(ctx, "sf/code", "bob", "t-2")
	if err != nil {
		t.Fatalf("bind t-2: %v", err)
	}
	if _, err := s.NewConversation(ctx, "sf/code", "sebastian"); err != nil {
		t.Fatal(err)
	}
	threads, err := s.CardThreads(ctx, "sf/code", []string{"t-1", "t-2", "t-9"})
	if err != nil {
		t.Fatalf("CardThreads: %v", err)
	}
	if len(threads) != 2 ||
		threads["t-1"] != (CardThread{Login: "sebastian", Conversation: one}) ||
		threads["t-2"] != (CardThread{Login: "bob", Conversation: two}) {
		t.Fatalf("CardThreads = %+v, want sebastian %d and bob %d", threads, one, two)
	}
}
