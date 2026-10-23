package factoryd

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
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
			ChatMessage{Kind: MessageEscalation, Content: "#603 - Session store: pick the table's store needs you: Which store?", Choices: []string{"Postgres", "SQLite"}}, true},
		{"a question with its why", Event{Kind: EventQuestionAsked, TaskID: "t-1", Detail: map[string]any{"question": "Which store?", "why": "both pass", "source": "agent"}}, card,
			ChatMessage{Kind: MessageEscalation, Content: "#603 - Session store: pick the table's store needs you: Which store?\nWhy: both pass"}, true},
		{"a budget stop", Event{Kind: EventQuestionAsked, TaskID: "t-1", Detail: map[string]any{"question": "the remaining budget cannot fund a call", "source": "budget"}}, card,
			ChatMessage{Kind: MessageEscalation, Content: "#603 - Session store: pick the table's store is stopped by its budget: the remaining budget cannot fund a call"}, true},
		// a row after the card's escalation, so the inbox's answer row
		// offers it no more
		{"a withdrawn question", Event{Kind: EventQuestionWithdrawn, TaskID: "t-1", Detail: map[string]any{"question": "Which store?", "reason": "the checks failed"}}, card,
			ChatMessage{Kind: MessagePoke, Content: "#603 needs you no more: its question is withdrawn because the checks failed."}, true},
		{"a merge", Event{Kind: EventApprovalRequested, TaskID: "t-1", Detail: map[string]any{"kind": "merge", "pr": float64(612), "class": "high"}}, card,
			ChatMessage{Kind: MessageEscalation, Content: "#603 waits for you: merge PR #612, high risk.", Choices: []string{"Approve"}}, true},
		{"an agent's merge on red checks offers no Approve", Event{Kind: EventApprovalRequested, TaskID: "t-1", Detail: map[string]any{"kind": "merge", "pr": float64(612), "class": "normal", "factory": false, "red_checks": true}}, card,
			ChatMessage{Kind: MessageEscalation, Content: "#603 waits for you: PR #612 cannot merge, its checks are red; send it back to the agent with what to change, or close it."}, true},
		{"a factory merge on red checks offers the re-run", Event{Kind: EventApprovalRequested, TaskID: "t-1", Detail: map[string]any{"kind": "merge", "pr": float64(612), "class": "normal", "factory": true, "red_checks": true}}, card,
			ChatMessage{Kind: MessageEscalation, Content: "#603 waits for you: the checks of PR #612 are red, so it cannot merge; run them again, or close it with your words.", Choices: []string{ChoiceRerun}}, true},
		{"a plan", Event{Kind: EventApprovalRequested, TaskID: "t-plan", Detail: map[string]any{"kind": "plan", "revision": float64(2), "cards": float64(6)}}, plan,
			ChatMessage{Kind: MessageEscalation, Content: `Plan #558 "Members page" waits for your acceptance: revision 2, 6 cards.`, Choices: []string{"Accept"}}, true},
		{"a failure", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "failed",
			"reason": "policy: policy version 669: risk class forbidden is forbidden to agents: .forgejo/workflows/ci.yml, .forgejo/workflows/pr.yml"}}, card,
			ChatMessage{Kind: MessagePoke, Content: "#603 failed: policy: policy version 669: risk class forbidden is forbidden to agents: .forgejo/workflows/ci.yml, .forgejo/workflows/pr.yml.",
				Choices: []string{"Retry", "Leave it"}}, true},
		{"a failure's reason of many lines", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "failed",
			"reason": "checks: the checks failed on c1.\noutput tail:\nFAIL TestStore"}}, card,
			ChatMessage{Kind: MessagePoke, Content: "#603 failed: checks: the checks failed on c1.", Choices: []string{"Retry", "Leave it"}}, true},
		{"a card done", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "done"}}, card,
			ChatMessage{Kind: MessagePoke, Content: "#603 is done and live: Session store: pick the table's store. €0.21 in all."}, true},
		{"a plan's card done", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "done"}}, child, ChatMessage{}, false},
		// a merged card informs once at its merge; the done poke comes at
		// live, so a card already done by then (live at once) is quiet here
		{"a merge waits for the chain", Event{Kind: EventPullRequestMerged, TaskID: "t-1", Detail: map[string]any{"pr": float64(9), "by": MergeUser}}, func() Task { c := card; c.Status = "merged"; return c }(),
			ChatMessage{Kind: MessagePoke, Content: "#603 merged; the chain is running."}, true},
		{"a merge already done informs nobody", Event{Kind: EventPullRequestMerged, TaskID: "t-1", Detail: map[string]any{"pr": float64(9), "by": MergeUser}}, func() Task { c := card; c.Status = "done"; return c }(),
			ChatMessage{}, false},
		{"a closed card", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "closed"}}, card, ChatMessage{}, false},
		{"a chat task", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "failed"}}, Task{ID: "t-1", Kind: KindChat}, ChatMessage{}, false},
		{"a plan done", Event{Kind: EventAllChildrenDone, TaskID: "t-plan", Detail: map[string]any{"cards": float64(6)}}, plan,
			ChatMessage{Kind: MessagePoke, Content: "Plan #558 is done: all 6 cards."}, true},
		{"a chat budget spent", Event{Kind: EventChatBudgetSpent, TaskID: "t-chat-linkshort-2000-01", Actor: FactoryDecider, Via: ViaFactoryd,
			Detail: map[string]any{"budget_eur": float64(5)}}, Task{ID: "t-chat-linkshort-2000-01", Kind: KindChat},
			ChatMessage{Kind: MessagePoke, Content: "The chat's monthly budget of €5.00 is spent: the chat stands until it is raised (Fund the chat on the board) or the month turns."}, true},
		{"a level change", Event{Kind: EventAutonomyChanged, Actor: "sebastian", Detail: map[string]any{"from": "manual", "to": "assisted"}}, Task{},
			ChatMessage{Kind: MessagePoke, Content: "sebastian set the autonomy from manual to assisted: the agent starts and files ready work by itself; direction, money beyond a card's budget and merges still ask."}, true},
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
			tt.want.BirthKind = tt.e.Kind
			if tt.e.Kind != EventAutonomyChanged {
				tt.want.TaskID = tt.e.TaskID
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("pokeOf = %+v,\nwant %+v", got, tt.want)
			}
		})
	}
}

// The merged inform is one poke at the merge move, its words the chain's
// facts as the pass reads them (detail's chain, repair and covered —
// Pass's late read): running tells the chain, a red chain names the
// repair on it, and a chain that already covers the card — the poller
// concluded between merge and poke — informs nobody, its done poke its
// only tell.
func TestMergedPokeInformsOnce(t *testing.T) {
	merged := Task{ID: "t-1", Kind: "agent", IssueNumber: 603, Status: "merged", FinishedAt: justNow()}
	merge := func(chain string, repair float64, covered bool) Event {
		return Event{Kind: EventPullRequestMerged, TaskID: "t-1",
			Detail: map[string]any{"pr": float64(9), "by": MergeUser, "chain": chain, "repair": repair, "covered": covered}}
	}
	for _, tt := range []struct {
		name string
		e    Event
		want string
		ok   bool
	}{
		{"the chain running", merge("running", 0, false), "#603 merged; the chain is running.", true},
		{"the chain red with its repair", merge("failure", 1139, false), "#603 merged; the chain failed; repair #1139 is on it.", true},
		{"the chain red before its repair card files", merge("failure", 0, false), "#603 merged; the chain failed.", true},
		{"another head's red, its repair on it", merge("running", 1139, false), "#603 merged; the chain failed; repair #1139 is on it.", true},
		{"a chain that already covers the card", merge("failure", 0, true), "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := pokeOf(tt.e, merged)
			if ok != tt.ok {
				t.Fatalf("pokeOf ok = %v, want %v", ok, tt.ok)
			}
			if ok && m.Content != tt.want {
				t.Fatalf("poke = %q, want %q", m.Content, tt.want)
			}
		})
	}
}

// Done pokes at live and its words state it, the whole card's cost beside
// them.
func TestDonePokeSaysLive(t *testing.T) {
	m, ok := pokeOf(Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "done"}},
		Task{ID: "t-1", Kind: "agent", IssueNumber: 603, Status: "done", SpentEUR: 0.214,
			Objective: "Session store: pick the table's store\n\nAcceptance:\n- one store"})
	if !ok {
		t.Fatal("pokeOf ok = false, want the done poke")
	}
	want := "#603 is done and live: Session store: pick the table's store. €0.21 in all."
	if m.Content != want {
		t.Fatalf("poke = %q, want %q", m.Content, want)
	}
}

// A chat budget's poke asks at the needs-you level: the messenger's copy
// goes to every member whose plan carries the level, an inform though it
// is, for the chat task's row settles an escalation the moment it is
// read.
func TestChatBudgetPokeDeliversAtNeedsYou(t *testing.T) {
	e := Event{Kind: EventChatBudgetSpent, TaskID: "t-chat-linkshort-2000-01", Detail: map[string]any{"budget_eur": float64(5)}}
	m, ok := pokeOf(e, Task{ID: "t-chat-linkshort-2000-01", Kind: KindChat})
	if !ok || m.Kind != MessagePoke {
		t.Fatalf("pokeOf = %+v, %v", m, ok)
	}
	if got := deliveryLevel(m); got != LevelNeedsYou {
		t.Fatalf("deliveryLevel = %q, want %q", got, LevelNeedsYou)
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
	if _, err := s.db.Exec(`TRUNCATE chat_messages, chat_conversations, chat_seen, chat_poke_cursor, events, tasks, chain_runs CASCADE`); err != nil {
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
			BirthKind: asked.Kind, Content: "#603 - Session store needs you: Which store?", Choices: []string{"Postgres", "SQLite"}, TaskID: "t-1", Repeats: 1}
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
	if len(rows) != 2 || rows[1].Content != "#603 - Session store needs you: Which port?" || rows[1].Repeats != 0 {
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
	if len(rows) != 4 || !rows[3].Queued || rows[3].Content != "#603 is done and live: Session store. €0.00 in all." || !slices.Equal(line.landed["bob"], []int64{rows[3].ID}) {
		t.Fatalf("bob's inbox = %+v, landed %v: want the poke queued as its newest row and its id handed to the line", rows, line.landed["bob"])
	}
	mine, _ := s.EnsureInbox(ctx, "sf/code", "sebastian")
	rows, _ = s.ChatMessages(ctx, "sf/code", "sebastian", mine, 10)
	if last := rows[len(rows)-1]; last.Queued || last.Content != "#603 is done and live: Session store. €0.00 in all." || line.landed["sebastian"] != nil {
		t.Fatalf("sebastian's newest row = %+v, landed %v: want the poke at once", last, line.landed["sebastian"])
	}

	// a merged card informs once at its merge, its words the chain's facts
	// as the pass reads them: a red chain names the repair card on it
	merged := Task{ID: "t-3", Kind: "agent", Repository: "sf/code", IssueNumber: 604, PRNumber: 9, Status: "merged", FinishedAt: justNow(), Objective: "Feed page"}
	if err := s.CreateTask(ctx, merged); err != nil {
		t.Fatal(err)
	}
	// an earlier repair card, merged, stands beside the acting one: its fix
	// is in main, so it is no longer on the red
	old := merged
	old.ID, old.Source, old.IssueNumber, old.PRNumber = "t-r0", SourceChainRepair, 1100, 0
	if err := s.CreateTask(ctx, old); err != nil {
		t.Fatal(err)
	}
	repair := merged
	repair.ID, repair.Status, repair.Source, repair.IssueNumber, repair.PRNumber = "t-r", "queued", SourceChainRepair, 1139, 0
	if err := s.CreateTask(ctx, repair); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChainRun(ctx, "sf/code", 9, "ffeeddccb00112233445566778899aabbccddeeff", 21, "failure"); err != nil {
		t.Fatal(err)
	}
	record(Event{Repository: "sf/code", TaskID: "t-3", Kind: EventPullRequestMerged, Actor: "sebastian", Via: ViaButton,
		Detail: map[string]any{"pr": float64(9), "by": MergeUser}})
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ChatMessages(ctx, "sf/code", "bob", inbox, 20)
	if len(rows) != 5 || rows[4].Content != "#604 merged; the chain failed; repair #1139 is on it." {
		t.Fatalf("bob's inbox = %+v, want the merged inform naming the repair as its newest row", rows)
	}
}

// A failed card's poke tells the reason its issue comment carries: Finish
// records it on the task_ended event the poke is made from, so a policy
// gate's refusal reads in the inbox as on the issue.
func TestFailurePokeCarriesTheIssueCommentsReason(t *testing.T) {
	fj := &prForgejo{labels: map[int64][]string{4: {"agent", "state/running"}}}
	task := prTask
	task.Kind = "agent"
	r, _, objects := newActivityRunner(t, task, fj)
	reason := "policy: policy version 669: risk class forbidden is forbidden to agents: .forgejo/workflows/ci.yml, .forgejo/workflows/pr.yml"
	if _, err := execActivity(t, r, ActivityFinish, "t1", "failed", reason); err != nil {
		t.Fatal(err)
	}
	if want := "factoryd: task t1 failed: " + reason; len(fj.comments) != 1 || fj.comments[0] != want {
		t.Fatalf("the issue's comments = %q, want %q", fj.comments, want)
	}
	b, err := objects.Get(t.Context(), "sessions/t1/timeline.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var detail map[string]any
	for line := range strings.Lines(string(b)) {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err == nil && e["event"] == EventTaskEnded {
			detail = e
		}
	}
	m, ok := pokeOf(Event{Kind: EventTaskEnded, TaskID: "t1", Detail: detail}, task)
	if want := "#4 failed: " + reason + "."; !ok || m.Content != want {
		t.Fatalf("the poke = %+v, %t, want %q", m, ok, want)
	}
}

// Finish lands a merged card in merged after its cleanup, so a pass can
// read the merge while the card is on its way there: one in review or
// closing in a project whose chain runs informs as a merged one does; a
// roll, a card of a project without a chain and one of a repository no
// project file names end done at the merge and inform nobody.
func TestTheMergedInformReachesACardOnItsWayToMerged(t *testing.T) {
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_pokes"))
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`TRUNCATE chat_messages, chat_conversations, chat_seen, chat_poke_cursor, events, tasks, chain_runs CASCADE`); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	tasks := []Task{
		{ID: "t-rev", Kind: "agent", Repository: "sf/code", IssueNumber: 601, PRNumber: 9, Status: "in-review", Objective: "Feed page"},
		{ID: "t-cls", Kind: "agent", Repository: "sf/code", IssueNumber: 602, PRNumber: 10, Status: "closing", Objective: "Feed page"},
		{ID: "t-roll", Kind: "agent", Repository: "sf/code", IssueNumber: 603, PRNumber: 11, Status: "closing", Source: SourceRoll, Objective: "roll"},
		{ID: "t-none", Kind: "agent", Repository: "sf/demo", IssueNumber: 604, PRNumber: 3, Status: "closing", Objective: "Feed page"},
		{ID: "t-gone", Kind: "agent", Repository: "sf/gone", IssueNumber: 605, PRNumber: 4, Status: "closing", Objective: "Feed page"},
	}
	for _, task := range tasks {
		if err := s.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	tree := policy.Tree{Document: policy.Document{Approvers: []string{"bob"}}, Projects: map[string]policy.ProjectConfig{
		"code": {Repository: "sf/code", Notify: []string{"bob"}},
		"demo": {Repository: "sf/demo", Notify: []string{"bob"}, Chain: policy.ChainConfig{Mode: chainModeNone}},
	}}
	p := &Poker{Store: s, Current: func() policy.Version { return policy.Version{Tree: tree} }, Line: &busyLine{busy: map[string]bool{}, landed: map[string][]int64{}}}
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if _, err := s.Record(ctx, Event{Repository: task.Repository, TaskID: task.ID, Kind: EventPullRequestMerged, Actor: MergeUser, Via: ViaFactoryd,
			Detail: map[string]any{"pr": float64(task.PRNumber), "by": MergeUser}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, repository := range []string{"sf/code", "sf/demo", "sf/gone"} {
		inbox, _ := s.EnsureInbox(ctx, repository, "bob")
		rows, _ := s.ChatMessages(ctx, repository, "bob", inbox, 10)
		for _, r := range rows {
			got = append(got, r.Content)
		}
	}
	if want := []string{"#601 merged; the chain is running.", "#602 merged; the chain is running."}; !slices.Equal(got, want) {
		t.Fatalf("bob's inboxes = %q, want %q", got, want)
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

// A level change pokes the project's other members and not the member who
// set it: they know what they set (Sebastian, 2026-10-21).
func TestPGStorePokeLevelChangeTellsTheOthers(t *testing.T) {
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_pokes_level"))
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	ctx := t.Context()
	tree := policy.Tree{Projects: map[string]policy.ProjectConfig{"code": {Repository: "sf/code", Notify: []string{"sebastian", "bob"}}}}
	p := &Poker{Store: s, Current: func() policy.Version { return policy.Version{Tree: tree} }, Line: &busyLine{busy: map[string]bool{}, landed: map[string][]int64{}}}
	if err := p.Pass(ctx); err != nil { // the first pass: history
		t.Fatal(err)
	}
	if _, err := s.Record(ctx, Event{Repository: "sf/code", Kind: EventAutonomyChanged, Actor: "sebastian", Via: ViaButton,
		Detail: map[string]any{"from": "manual", "to": "assisted"}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	inbox, _ := s.EnsureInbox(ctx, "sf/code", "bob")
	rows, _ := s.ChatMessages(ctx, "sf/code", "bob", inbox, 10)
	if len(rows) != 1 || rows[0].Content != "sebastian set the autonomy from manual to assisted: "+policy.AutonomyMeaning[policy.AutonomyAssisted]+"." {
		t.Fatalf("bob's inbox = %+v, want the level change", rows)
	}
	mine, _ := s.EnsureInbox(ctx, "sf/code", "sebastian")
	if rows, _ := s.ChatMessages(ctx, "sf/code", "sebastian", mine, 10); len(rows) != 0 {
		t.Fatalf("sebastian's inbox = %+v, want his own level change kept out", rows)
	}
}
