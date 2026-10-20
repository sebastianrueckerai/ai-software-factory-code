package factoryd

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The threads (roadmap 239): one row per conversation with its title,
// state, inbox flag and current pointer; the seen and clear markers; the
// lazy inbox; the hard delete's count. Hand-derived from the plan's data
// model.
func TestPGStoreChatThreads(t *testing.T) {
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_threads"))
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`TRUNCATE chat_messages, chat_conversations, chat_seen, chat_proposals, tasks CASCADE`); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	repo, login := "sf/code", "sebastian"

	// the first message lands in conversation 1 and marks it current
	for _, text := range []string{"What is stuck?", "Nothing: two cards run."} {
		if _, err := s.AddChatMessage(ctx, repo, login, ChatMessage{Role: "assistant", Content: text}); err != nil {
			t.Fatal(err)
		}
	}
	threads, err := s.Conversations(ctx, repo, login)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0].Number != 1 || threads[0].Messages != 2 || threads[0].State != "active" || !threads[0].Current {
		t.Fatalf("threads = %+v, want one active current thread 1 with two messages", threads)
	}
	if n, _ := s.CurrentConversation(ctx, repo, login); n != 1 {
		t.Fatalf("CurrentConversation = %d, want 1", n)
	}

	// /new makes thread 2 the current one; its rows land there
	if n, err := s.NewConversation(ctx, repo, login); err != nil || n != 2 {
		t.Fatalf("NewConversation = %d, %v, want 2", n, err)
	}
	if _, err := s.AddChatMessage(ctx, repo, login, ChatMessage{Role: "user", Content: "Fresh start."}); err != nil {
		t.Fatal(err)
	}
	// seen: the member read thread 1; a late answer there marks it unread
	if err := s.MarkSeen(ctx, repo, login, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddChatMessage(ctx, repo, login, ChatMessage{Role: "assistant", Content: "Late answer.", Conversation: 1}); err != nil {
		t.Fatal(err)
	}
	threads, _ = s.Conversations(ctx, repo, login)
	for _, th := range threads {
		switch th.Number {
		case 1:
			if th.Unread != 1 || th.Current {
				t.Errorf("thread 1 = %+v, want one unread, not current after /new", th)
			}
		case 2:
			if th.Unread != 0 || !th.Current || th.Messages != 1 {
				t.Errorf("thread 2 = %+v, want current with its one message, a member's own", th)
			}
		}
	}
	if err := s.MarkSeen(ctx, repo, login, 1); err != nil {
		t.Fatal(err)
	}
	if th, ok, err := s.Conversation(ctx, repo, login, 1); err != nil || !ok || th.Unread != 0 || th.Unswept != 3 {
		t.Errorf("thread 1 after it was read = %+v, %v, %v, want no unread and three rows past the marker", th, ok, err)
	}

	// clear: the member's sweep of thread 1's rows in view; the marker
	// sits at the thread's newest row, a row that lands after it stays in
	// view, and a second sweep takes that row in
	if err := s.MarkCleared(ctx, repo, login, 1); err != nil {
		t.Fatal(err)
	}
	if th, ok, err := s.Conversation(ctx, repo, login, 1); err != nil || !ok || th.ClearedUpto != 4 || th.Unswept != 0 {
		t.Errorf("thread 1 after the sweep = %+v, %v, %v, want the marker at its newest row 4 and no row past it", th, ok, err)
	}
	if _, err := s.AddChatMessage(ctx, repo, login, ChatMessage{Role: "assistant", Content: "Later still.", Conversation: 1}); err != nil {
		t.Fatal(err)
	}
	if th, _, _ := s.Conversation(ctx, repo, login, 1); th.ClearedUpto != 4 || th.Unswept != 1 {
		t.Errorf("the marker = %d with %d rows past it after a late row, want it unmoved at 4 with one row past", th.ClearedUpto, th.Unswept)
	}
	if err := s.MarkCleared(ctx, repo, login, 1); err != nil {
		t.Fatal(err)
	}
	if th, _, _ := s.Conversation(ctx, repo, login, 1); th.ClearedUpto != 5 || th.Unswept != 0 {
		t.Errorf("the marker after the second sweep = %d with %d rows past it, want 5 and none", th.ClearedUpto, th.Unswept)
	}

	// a queued message and a queued poke wait as queued, the poke keeping
	// its kind, and move after the rows of the turn before them, in their
	// order, once their place comes
	q, err := s.AddChatMessage(ctx, repo, login, ChatMessage{Role: "user", Content: "And the second?", Conversation: 2, Queued: true})
	if err != nil {
		t.Fatal(err)
	}
	qp, err := s.AddChatMessage(ctx, repo, login, ChatMessage{Role: "assistant", Kind: MessagePoke, Content: "Filed #4.", Conversation: 2, Queued: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddChatMessage(ctx, repo, login, ChatMessage{Role: "assistant", Content: "The first answer.", Conversation: 2}); err != nil {
		t.Fatal(err)
	}
	order := func() []string {
		t.Helper()
		msgs, err := s.ChatMessages(ctx, repo, login, 2, 10)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range msgs {
			out = append(out, fmt.Sprintf("%s:%s queued %t", m.Kind, m.Content, m.Queued))
		}
		return out
	}
	if got, want := order(), []string{":Fresh start. queued false", ":And the second? queued true", "poke:Filed #4. queued true",
		":The first answer. queued false"}; !slices.Equal(got, want) {
		t.Errorf("thread 2 while the turn runs = %q, want %q", got, want)
	}
	for _, id := range []int64{q, qp} {
		if err := s.StartQueued(ctx, repo, login, id); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := order(), []string{":Fresh start. queued false", ":The first answer. queued false", ":And the second? queued false",
		"poke:Filed #4. queued false"}; !slices.Equal(got, want) {
		t.Errorf("thread 2 = %q, want %q", got, want)
	}

	// rename, archive, restore
	if ok, err := s.RenameConversation(ctx, repo, login, 1, "The cache plans"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := s.ArchiveConversation(ctx, repo, login, 2, true); err != nil || !ok {
		t.Fatal(ok, err)
	}
	threads, _ = s.Conversations(ctx, repo, login)
	for _, th := range threads {
		if th.Number == 1 && th.Title != "The cache plans" {
			t.Errorf("thread 1 title = %q", th.Title)
		}
		if th.Number == 2 && th.State != "archived" {
			t.Errorf("thread 2 state = %q, want archived", th.State)
		}
	}
	if ok, err := s.ArchiveConversation(ctx, repo, login, 2, false); err != nil || !ok {
		t.Fatal(ok, err)
	}

	// the inbox: lazy-created once, the poke lands in it with its choices;
	// the archive left no current thread, so the next one, 3, gets its row
	// first and the inbox is 4
	n, err := s.EnsureInbox(ctx, repo, login)
	if err != nil || n != 4 {
		t.Fatalf("EnsureInbox = %d, %v, want 4", n, err)
	}
	if n, err := s.EnsureInbox(ctx, repo, login); err != nil || n != 4 {
		t.Fatalf("EnsureInbox again = %d, %v, want the same 4", n, err)
	}
	if n, err := s.CurrentConversation(ctx, repo, login); err != nil || n != 3 {
		t.Fatalf("the current thread = %d, %v, want 3", n, err)
	}
	// a member with no thread yet: their current thread gets its row
	// first, so the inbox never takes the number their first message uses
	if n, err := s.EnsureInbox(ctx, repo, "bob"); err != nil || n != 2 {
		t.Fatalf("bob's EnsureInbox = %d, %v, want 2", n, err)
	}
	if n, err := s.CurrentConversation(ctx, repo, "bob"); err != nil || n != 1 {
		t.Fatalf("bob's current thread = %d, %v, want 1", n, err)
	}
	poke := ChatMessage{Role: "assistant", Kind: "poke", Content: "Card #9 waits on you.", Choices: []string{"Answer it", "Skip"}, Conversation: n}
	if _, err := s.AddChatMessage(ctx, repo, login, poke); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.ChatMessages(ctx, repo, login, n, 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("the inbox rows = %v, %v, want one", msgs, err)
	}
	if msgs[0].Kind != "poke" || len(msgs[0].Choices) != 2 {
		t.Errorf("the poke row = %+v, want kind poke with two choices", msgs[0])
	}

	// open switches the current thread
	if err := s.OpenConversation(ctx, repo, login, 1); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CurrentConversation(ctx, repo, login); n != 1 {
		t.Fatalf("CurrentConversation = %d, want 1 after the switch", n)
	}

	// the hard delete removes the member's rows, the thread and the seen
	// marker (the sweep's marker rides it), and answers the count (the
	// event is the console's to write)
	gone, _, err := s.DeleteConversation(ctx, repo, login, 1)
	if err != nil || gone != 4 {
		t.Fatalf("DeleteConversation = %d, %v, want 4 rows gone", gone, err)
	}
	if _, _, err := s.DeleteConversation(ctx, repo, login, n); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting the inbox = %v, want ErrNotFound: the inbox stays", err)
	}
	if ok, err := s.ArchiveConversation(ctx, repo, login, n, true); err != nil || ok {
		t.Errorf("archiving the inbox = %v, %v, want refused", ok, err)
	}
	msgs, _ = s.ChatMessages(ctx, repo, login, 1, 10)
	if len(msgs) != 0 {
		t.Errorf("thread 1 rows = %v, want none", msgs)
	}
	threads, _ = s.Conversations(ctx, repo, login)
	for _, th := range threads {
		if th.Number == 1 {
			t.Errorf("thread 1 survived its delete: %+v", th)
		}
	}

	// the images ride the delete: their rows go in the thread's own
	// transaction, their ids answered for the objects' sweep
	img, err := s.AddChatMessage(ctx, repo, login, ChatMessage{Role: "user", Content: "look at img1", Conversation: 2})
	if err != nil {
		t.Fatal(err)
	}
	var want []int64
	for _, name := range []string{"img1", "img2"} {
		a, err := s.AddChatAttachment(ctx, repo, login, ChatAttachment{Conversation: 2, MessageID: img,
			Name: name, FileName: name + ".png", ContentType: "image/png", Bytes: 70})
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, a.ID)
	}
	if _, ids, err := s.DeleteConversation(ctx, repo, login, 2); err != nil || !slices.Equal(ids, want) {
		t.Fatalf("the delete's images = %v, %v, want %v", ids, err, want)
	}
	if left, err := s.ChatAttachments(ctx, repo, login, 2); err != nil || len(left) != 0 {
		t.Fatalf("thread 2's images after the delete = %v, %v, want none", left, err)
	}

	// a message whose image could not be stored is withdrawn whole: its
	// row, its image rows, the ids answered for the sweep
	keep, err := s.AddChatMessage(ctx, repo, login, ChatMessage{Role: "user", Content: "this one stays", Conversation: 3})
	if err != nil {
		t.Fatal(err)
	}
	half, err := s.AddChatMessage(ctx, repo, login, ChatMessage{Role: "user", Content: "look at img1", Conversation: 3})
	if err != nil {
		t.Fatal(err)
	}
	ha, err := s.AddChatAttachment(ctx, repo, login, ChatAttachment{Conversation: 3, MessageID: half,
		Name: "img1", FileName: "shot.png", ContentType: "image/png", Bytes: 70})
	if err != nil {
		t.Fatal(err)
	}
	if ids, err := s.AbandonChatMessage(ctx, repo, login, 3, half); err != nil || !slices.Equal(ids, []int64{ha.ID}) {
		t.Fatalf("the withdrawn message's images = %v, %v, want [%d]", ids, err, ha.ID)
	}
	rows, err := s.ChatMessages(ctx, repo, login, 3, 10)
	if err != nil || len(rows) != 1 || rows[0].ID != keep {
		t.Fatalf("thread 3's rows after the withdrawal = %v, %v, want only the message that stays", rows, err)
	}
	if left, err := s.ChatAttachments(ctx, repo, login, 3); err != nil || len(left) != 0 {
		t.Fatalf("thread 3's images after the withdrawal = %v, %v, want none", left, err)
	}
}

// A compaction is kept on its thread and counted; the thread's newest
// operator call after a time is its token projection, and its calls of
// every purpose sum to its totals (roadmap 239).
func TestPGStoreThreadSummaryAndTokens(t *testing.T) {
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_thread_tokens"))
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`TRUNCATE chat_messages, chat_conversations, chat_seen, audit CASCADE`); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := s.AddChatMessage(ctx, "sf/code", "anna", ChatMessage{Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSummary(ctx, "sf/code", "anna", 1, ThreadSummary{Text: "the summary", Model: "pulse", Upto: 1, Before: 900, After: 120}); err != nil {
		t.Fatal(err)
	}
	th, ok, err := s.Conversation(ctx, "sf/code", "anna", 1)
	if err != nil || !ok || th.Summary != "the summary" || th.SummaryUpto != 1 || th.Compactions != 1 || th.SummaryAt.IsZero() {
		t.Fatalf("thread 1 = %+v, %v, %v: want its summary up to row 1, counted once", th, ok, err)
	}
	thread := "sf/code:anna:1"
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		kind, thread string
		prompt, out  int
		eur          float64
		minutes      int
	}{
		{"operator", thread, 1000, 200, 0.01, 0},
		{"compact", thread, 3000, 300, 0.001, 1},
		{"operator", thread, 4000, 500, 0.02, 2},
		{"operator", "sf/code:anna:2", 9000, 900, 0.5, 3},
		{"operator", thread, 0, 0, 0, 4}, // a refusal: no tokens
	} {
		if _, err := s.db.Exec(`INSERT INTO audit (task_id, decision, reason, model_alias, kind, thread, prompt_tokens, output_tokens, call_cost_eur, ts)
			VALUES ('t-chat', 'allowed', '', 'operator', $1, $2, $3, $4, $5, $6)`, row.kind, row.thread, row.prompt, row.out, row.eur, at.Add(time.Duration(row.minutes)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		since time.Time
		want  int
	}{
		{time.Time{}, 4500},
		{at.Add(90 * time.Second), 4500},
		{at.Add(3 * time.Minute), 0},
	} {
		if n, err := s.ThreadTokens(ctx, thread, tt.since); err != nil || n != tt.want {
			t.Errorf("ThreadTokens(since %v) = %d, %v, want %d", tt.since, n, err, tt.want)
		}
	}
	tokens, eur, err := s.ThreadTotals(ctx, thread)
	if err != nil || tokens != 9000 || eur < 0.0309 || eur > 0.0311 {
		t.Errorf("ThreadTotals = %d, %v, %v, want 9000 tokens and €0.031", tokens, eur, err)
	}
}
