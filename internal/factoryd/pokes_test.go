package factoryd

import (
	"reflect"
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Each poke class's row, hand-derived from the event and its card
// (roadmap 239, C14's inbox).
func TestPokeOf(t *testing.T) {
	card := Task{ID: "t-1", Kind: "agent", IssueNumber: 603, Objective: "Session store: pick the table's store\n\nAcceptance:\n- one store", SpentEUR: 0.214}
	child := card
	child.Parent = "t-plan"
	plan := Task{ID: "t-plan", Kind: "agent", IssueNumber: 558, Objective: "Members page"}
	agentDid := func(kind, level string, more map[string]any) Event {
		d := map[string]any{"autonomy": level, "merge_rule": "human", "decider": "agent"}
		for k, v := range more {
			d[k] = v
		}
		return Event{Kind: kind, TaskID: "t-1", Actor: "sebastian", Detail: d}
	}
	for _, tt := range []struct {
		name string
		e    Event
		t    Task
		want ChatMessage
		ok   bool
	}{
		{"a question", Event{Kind: EventQuestionAsked, TaskID: "t-1", Detail: map[string]any{"question": "Which store?", "source": "agent",
			"choices": []any{"Postgres", "SQLite"}}}, card,
			ChatMessage{Kind: MessageEscalation, Content: "#603 needs you: Which store?", Choices: []string{"Postgres", "SQLite"}}, true},
		{"a budget stop", Event{Kind: EventQuestionAsked, TaskID: "t-1", Detail: map[string]any{"question": "the remaining budget cannot fund a call", "source": "budget"}}, card,
			ChatMessage{Kind: MessageEscalation, Content: "#603 is stopped by its budget: the remaining budget cannot fund a call"}, true},
		// a row after the card's escalation, so the inbox's answer row
		// offers it no more
		{"a withdrawn question", Event{Kind: EventQuestionWithdrawn, TaskID: "t-1", Detail: map[string]any{"question": "Which store?", "reason": "the checks failed"}}, card,
			ChatMessage{Kind: MessagePoke, Content: "#603 needs you no more: its question is withdrawn because the checks failed."}, true},
		{"a merge", Event{Kind: EventApprovalRequested, TaskID: "t-1", Detail: map[string]any{"kind": "merge", "pr": float64(612), "class": "high"}}, card,
			ChatMessage{Kind: MessageEscalation, Content: "Merge PR #612 of #603 waits for you: risk class high.", Choices: []string{"Approve"}}, true},
		{"a plan", Event{Kind: EventApprovalRequested, TaskID: "t-plan", Detail: map[string]any{"kind": "plan", "revision": float64(2), "cards": float64(6)}}, plan,
			ChatMessage{Kind: MessageEscalation, Content: `Plan #558 "Members page" waits for your acceptance: revision 2, 6 cards.`, Choices: []string{"Accept"}}, true},
		{"a failure", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "failed"}}, card,
			ChatMessage{Kind: MessagePoke, Content: "#603 failed: Session store: pick the table's store.", Choices: []string{"Retry", "Leave it"}}, true},
		{"a card done", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "done"}}, card,
			ChatMessage{Kind: MessagePoke, Content: "#603 is done: Session store: pick the table's store. €0.21 in all."}, true},
		{"a plan's card done", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "done"}}, child, ChatMessage{}, false},
		{"a closed card", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "closed"}}, card, ChatMessage{}, false},
		{"a chat task", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "failed"}}, Task{ID: "t-1", Kind: KindChat}, ChatMessage{}, false},
		{"a plan done", Event{Kind: EventAllChildrenDone, TaskID: "t-plan", Detail: map[string]any{"cards": float64(6)}}, plan,
			ChatMessage{Kind: MessagePoke, Content: "Plan #558 is done: all 6 cards."}, true},
		{"a level change", Event{Kind: EventAutonomyChanged, Actor: "sebastian", Detail: map[string]any{"from": "manual", "to": "assisted"}}, Task{},
			ChatMessage{Kind: MessagePoke, Content: "sebastian set the autonomy from manual to assisted: the agent starts and files ready work by itself; direction, money and merges still ask."}, true},
		{"a cap change", Event{Kind: EventAutonomyChanged, Actor: "sebastian", Detail: map[string]any{"from": "assisted", "to": "assisted", "backlog_cap": float64(5)}}, Task{},
			ChatMessage{Kind: MessagePoke, Content: "sebastian set the backlog cap to 5."}, true},
		{"the agent's start", agentDid(EventCardMoved, "assisted", map[string]any{"lane": "ready"}), card,
			ChatMessage{Kind: MessagePoke, Content: `Started #603 "Session store: pick the table's store".`, Decider: "assisted level · started by the agent"}, true},
		{"the agent's filing at full auto", agentDid(EventCardFiled, "autonomous", map[string]any{"merge_rule": "auto"}), card,
			ChatMessage{Kind: MessagePoke, Content: `Filed #603 "Session store: pick the table's store".`, Decider: "full auto level · filed by the agent"}, true},
		{"the agent's move at manual", agentDid(EventCardMoved, "manual", map[string]any{"lane": "ready"}), card, ChatMessage{}, false},
		{"the member's start", Event{Kind: EventCardMoved, TaskID: "t-1", Detail: map[string]any{"autonomy": "assisted", "decider": "member", "lane": "ready"}}, card, ChatMessage{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := pokeOf(tt.e, tt.t)
			if ok != tt.ok {
				t.Fatalf("pokeOf ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if !slices.Contains(pokeKinds, tt.e.Kind) {
				t.Fatalf("the writer reads no %s event", tt.e.Kind)
			}
			tt.want.Role = "assistant"
			if tt.e.Kind != EventAutonomyChanged {
				tt.want.TaskID = tt.e.TaskID
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("pokeOf = %+v,\nwant %+v", got, tt.want)
			}
		})
	}
}

// The writer on a real database: the first pass starts at the newest
// event, so no history pokes; each poke lands in the inbox thread of the
// project's notify list, else its approvers; a repeat of the newest poke
// about a card counts on its row; an event that pokes nobody moves the
// cursor only.
func TestPGStorePokes(t *testing.T) {
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_pokes"))
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`TRUNCATE chat_messages, chat_conversations, chat_seen, chat_poke_cursor, events, tasks CASCADE`); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, task := range []Task{
		{ID: "t-1", Kind: "agent", Repository: "sf/code", IssueNumber: 603, Status: "waiting-human", Objective: "Session store"},
		{ID: "t-2", Kind: "agent", Repository: "sf/other", IssueNumber: 7, Status: "failed", Objective: "Feed page"},
	} {
		if err := s.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	record := func(e Event) {
		t.Helper()
		if _, err := s.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	tree := policy.Tree{Document: policy.Document{Approvers: []string{"anna"}},
		Projects: map[string]policy.ProjectConfig{"code": {Repository: "sf/code", Notify: []string{"sebastian", "bob"}}}}
	line := &busyLine{busy: map[string]bool{}, landed: map[string][]int64{}}
	p := &Poker{Store: s, Current: func() policy.Version { return policy.Version{Tree: tree} }, Line: line}
	asked := Event{Repository: "sf/code", TaskID: "t-1", Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"question": "Which store?", "choices": []string{"Postgres", "SQLite"}}}

	record(asked) // before the first pass: history
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if threads, _ := s.Conversations(ctx, "sf/code", "sebastian"); len(threads) != 0 {
		t.Fatalf("the first pass poked the history: %+v", threads)
	}

	record(asked)
	record(Event{Repository: "sf/code", TaskID: "t-1", Kind: EventCardCommented, Actor: "sebastian", Via: ViaButton})
	record(asked)
	record(Event{Repository: "sf/other", TaskID: "t-2", Kind: EventTaskEnded, Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"state": "failed"}})
	record(Event{Repository: "sf/code", TaskID: "t-1", Kind: EventTaskEnded, Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"state": "closed"}})
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	for _, login := range []string{"sebastian", "bob"} {
		// the inbox first, then the empty current thread 1 stored with it
		threads, err := s.Conversations(ctx, "sf/code", login)
		if err != nil || len(threads) != 2 || !threads[0].Inbox || threads[0].Number != 2 || threads[0].Title != InboxTitle || threads[0].Unread != 1 ||
			threads[1].Number != 1 || !threads[1].Current || threads[1].Messages != 0 {
			t.Fatalf("%s's threads = %+v, %v, want the inbox 2 with one unread poke and the empty current thread 1", login, threads, err)
		}
		rows, err := s.ChatMessages(ctx, "sf/code", login, threads[0].Number, 10)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s's inbox = %+v, %v, want one row", login, rows, err)
		}
		got := rows[0]
		want := ChatMessage{ID: got.ID, At: got.At, Conversation: threads[0].Number, Role: "assistant", Kind: MessageEscalation,
			Content: "#603 needs you: Which store?", Choices: []string{"Postgres", "SQLite"}, TaskID: "t-1", Repeats: 1}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s's poke = %+v,\nwant %+v", login, got, want)
		}
	}
	threads, _ := s.Conversations(ctx, "sf/other", "anna")
	if len(threads) != 2 || threads[0].Messages != 1 {
		t.Fatalf("the approver's threads on sf/other = %+v, want the inbox with the failure", threads)
	}
	var cursor, newest int64
	if err := s.db.QueryRow(`SELECT (SELECT event_id FROM chat_poke_cursor), (SELECT max(id) FROM events)`).Scan(&cursor, &newest); err != nil || cursor != newest {
		t.Fatalf("the cursor = %d, the newest event %d (%v): want them equal", cursor, newest, err)
	}

	// a later question on the same card is a new poke, not a repeat
	record(Event{Repository: "sf/code", TaskID: "t-1", Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"question": "Which port?"}})
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	inbox, _ := s.EnsureInbox(ctx, "sf/code", "bob")
	rows, _ := s.ChatMessages(ctx, "sf/code", "bob", inbox, 10)
	if len(rows) != 2 || rows[1].Content != "#603 needs you: Which port?" || rows[1].Repeats != 0 {
		t.Fatalf("bob's inbox = %+v, want the second question as its own row", rows)
	}

	// its withdrawal is the inbox's newest row: the answer row is gone
	record(Event{Repository: "sf/code", TaskID: "t-1", Kind: EventQuestionWithdrawn, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"question": "Which port?", "reason": "the checks failed"}})
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ChatMessages(ctx, "sf/code", "bob", inbox, 10)
	if len(rows) != 3 || rows[2].Kind != MessagePoke || rows[2].Content != "#603 needs you no more: its question is withdrawn because the checks failed." {
		t.Fatalf("bob's inbox = %+v, want the withdrawal as its newest row", rows)
	}

	// a turn runs in bob's inbox: his poke waits queued behind it and goes
	// to the line to land; sebastian's lands at once (roadmap 263)
	line.busy["bob"] = true
	record(Event{Repository: "sf/code", TaskID: "t-1", Kind: EventTaskEnded, Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"state": "done"}})
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ChatMessages(ctx, "sf/code", "bob", inbox, 10)
	if len(rows) != 4 || !rows[3].Queued || rows[3].Content != "#603 is done: Session store. €0.00 in all." || !slices.Equal(line.landed["bob"], []int64{rows[3].ID}) {
		t.Fatalf("bob's inbox = %+v, landed %v: want the poke queued as its newest row and its id handed to the line", rows, line.landed["bob"])
	}
	mine, _ := s.EnsureInbox(ctx, "sf/code", "sebastian")
	rows, _ = s.ChatMessages(ctx, "sf/code", "sebastian", mine, 10)
	if last := rows[len(rows)-1]; last.Queued || last.Content != "#603 is done: Session store. €0.00 in all." || line.landed["sebastian"] != nil {
		t.Fatalf("sebastian's newest row = %+v, landed %v: want the poke at once", last, line.landed["sebastian"])
	}
}

// busyLine is the chat's line as the poker sees it: a turn runs in the
// inbox of each login busy marks, and landed holds the queued rows handed
// to land there.
type busyLine struct {
	busy   map[string]bool
	landed map[string][]int64
}

func (l *busyLine) Behind(_, login string, _ int64) (bool, func(...int64)) {
	return l.busy[login], func(ids ...int64) {
		if l.busy[login] {
			l.landed[login] = append(l.landed[login], ids...)
		}
	}
}
