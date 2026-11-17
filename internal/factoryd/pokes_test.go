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
// (roadmap 239, C14's inbox): an ask is the card, its why line and its
// suggestion, a notice one line.
func TestPokeOf(t *testing.T) {
	card := Task{ID: "t-1", Kind: "agent", IssueNumber: 603, Objective: "Session store: pick the table's store\n\nAcceptance:\n- one store", SpentEUR: 0.214, Track: "direct"}
	child := card
	child.Parent = "t-plan"
	plan := Task{ID: "t-plan", Kind: "agent", IssueNumber: 558, Objective: "Members page", Track: "plan"}
	agentDid := func(kind, level string, more map[string]any) Event {
		d := map[string]any{"autonomy": level, "merge_rule": "human", "decider": "agent"}
		for k, v := range more {
			d[k] = v
		}
		return Event{Kind: kind, TaskID: "t-1", Actor: "sebastian", Detail: d}
	}
	const named = "#603 (Session store: pick the table's store)"
	for _, tt := range []struct {
		name string
		e    Event
		t    Task
		want ChatMessage
		ok   bool
	}{
		{"a question with no why names the question", Event{Kind: EventQuestionAsked, TaskID: "t-1", Detail: map[string]any{"question": "Which store?", "source": "agent",
			"choices": []any{"Postgres", "SQLite"}}}, card,
			ChatMessage{Kind: MessageEscalation, Content: named + " needs you. Which store?", Choices: []string{"Postgres", "SQLite"}}, true},
		{"a question with its why", Event{Kind: EventQuestionAsked, TaskID: "t-1", Detail: map[string]any{"question": "Which store?", "why": "Both stores pass the load test", "source": "agent"}}, card,
			ChatMessage{Kind: MessageEscalation, Content: named + " needs you. Both stores pass the load test."}, true},
		{"a session cap names its why, not the budget", Event{Kind: EventQuestionAsked, TaskID: "t-1", Detail: map[string]any{"question": "The card used all 3 sessions. Allow more, or end the card?",
			"why": "A card runs only the sessions it was given, and this one has used all 3.", "source": "cap",
			"offers":      []any{map[string]any{"value": "extend", "label": "Allow more sessions", "effect": "Gives the card more sessions or agent time, and it goes on."}},
			"preselected": map[string]any{"action": "extend"}, "recommended_by": "the factory agent"}}, card,
			ChatMessage{Kind: MessageEscalation, Content: named + " needs you. A card runs only the sessions it was given, and this one has used all 3. The factory agent suggests “Allow more sessions”."}, true},
		// a row after the card's escalation, so the inbox's answer row
		// offers it no more
		{"a withdrawn question", Event{Kind: EventQuestionWithdrawn, TaskID: "t-1", Detail: map[string]any{"question": "Which store?", "reason": "the checks failed"}}, card,
			ChatMessage{Kind: MessagePoke, Content: named + " needs you no more: the checks failed."}, true},
		// a monitor's case sits beside the card's own ask: its row names the flag
		{"a monitor case's withdrawn question", Event{Kind: EventQuestionWithdrawn, TaskID: "t-1", Detail: map[string]any{"case_id": float64(12), "case_kind": string(CaseMonitorFlag), "reason": "the wait ended", "problem": "The card waits past its bound."}}, card,
			ChatMessage{Kind: MessagePoke, Content: "The flag on " + named + " needs you no more: the wait ended."}, true},
		{"a case's withdrawn face is the card's own", Event{Kind: EventQuestionWithdrawn, TaskID: "t-1", Detail: map[string]any{"case_id": float64(14), "case_kind": string(CaseSessionCap), "question": "Which store?", "reason": "reviewer sebastian requested changes"}}, card,
			ChatMessage{Kind: MessagePoke, Content: named + " needs you no more: reviewer sebastian requested changes."}, true},
		{"a withdrawal past a notice's length drops its reason", Event{Kind: EventQuestionWithdrawn, TaskID: "t-1", Detail: map[string]any{"reason": "factoryd closed pull request #12: the branch it came from was deleted, and no other branch holds its commits any more"}}, card,
			ChatMessage{Kind: MessagePoke, Content: named + " needs you no more."}, true},
		{"a merge", Event{Kind: EventApprovalRequested, TaskID: "t-1", Detail: map[string]any{"kind": "merge", "pr": float64(612), "class": "high"}}, card,
			ChatMessage{Kind: MessageEscalation, Content: named + " needs you. The change is high risk, and the factory's rules let only a human merge it.", Choices: []string{"Merge it"}}, true},
		{"a merge the review approved suggests it", Event{Kind: EventApprovalRequested, TaskID: "t-1", Detail: map[string]any{"kind": "merge", "pr": float64(612), "class": "high",
			"pr_open": true, "head": "c1", "checks": "green", "review_approved": true, "review_summary": "The change is small and tested."}}, card,
			ChatMessage{Kind: MessageEscalation, Content: named + " needs you. The change is high risk, and the factory's rules let only a human merge it. The model review suggests “Merge it”.", Choices: []string{"Merge it"}}, true},
		{"an agent's merge on red checks offers no merge", Event{Kind: EventApprovalRequested, TaskID: "t-1", Detail: map[string]any{"kind": "merge", "pr": float64(612), "class": "normal", "factory": false, "red_checks": true}}, card,
			ChatMessage{Kind: MessageEscalation, Content: named + " needs you. The change is of normal risk, and the factory's rules let only a human merge it."}, true},
		{"a factory merge on red checks offers the rerun", Event{Kind: EventApprovalRequested, TaskID: "t-1", Detail: map[string]any{"kind": "merge", "pr": float64(612), "class": "normal", "factory": true, "red_checks": true}}, card,
			ChatMessage{Kind: MessageEscalation, Content: named + " needs you. The change is of normal risk, and the factory's rules let only a human merge it.", Choices: []string{"Run the failed checks again"}}, true},
		{"a plan", Event{Kind: EventApprovalRequested, TaskID: "t-plan", Detail: map[string]any{"kind": "plan", "revision": float64(2), "cards": float64(6)}}, plan,
			ChatMessage{Kind: MessageEscalation, Content: "#558 (Members page) needs you. The plan's cards and promises are fixed only once a human confirms them."}, true},
		{"a criteria gap", Event{Kind: EventApprovalRequested, TaskID: "t-plan", Detail: map[string]any{"kind": "criteria"}}, plan,
			ChatMessage{Kind: MessageEscalation, Content: "#558 (Members page) needs you. Until each promise is met or dropped, the plan stays open and holds production deploys."}, true},
		{"criteria never judged", Event{Kind: EventApprovalRequested, TaskID: "t-plan", Detail: map[string]any{"kind": "criteria_rerun"}}, plan,
			ChatMessage{Kind: MessageEscalation, Content: "#558 (Members page) needs you. Until each promise is met or dropped, the plan stays open and holds production deploys."}, true},
		{"a resource request pokes nobody", Event{Kind: EventApprovalRequested, TaskID: "t-1", Detail: map[string]any{"kind": "resource"}}, card, ChatMessage{}, false},
		{"a failure", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "failed",
			"reason": "policy: policy version 669: risk class forbidden is forbidden to agents: .forgejo/workflows/ci.yml, .forgejo/workflows/pr.yml"}}, card,
			ChatMessage{Kind: MessagePoke, Content: named + " failed.", Choices: []string{"Try the failed card again", "Not now"}}, true},
		{"a failure that fits names its reason's first line", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "failed",
			"reason": "checks: the checks failed on c1.\noutput tail:\nFAIL TestStore"}}, card,
			ChatMessage{Kind: MessagePoke, Content: named + " failed: checks: the checks failed on c1.", Choices: []string{"Try the failed card again", "Not now"}}, true},
		{"a card done", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "done"}}, card,
			ChatMessage{Kind: MessagePoke, Content: named + " is done and live. It cost 0.21 EUR."}, true},
		{"a plan's card done", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "done"}}, child, ChatMessage{}, false},
		// a merged card informs once at its merge; the done poke comes at
		// live, so a card already done by then (live at once) is quiet here
		{"a merge waits for the chain", Event{Kind: EventPullRequestMerged, TaskID: "t-1", Detail: map[string]any{"pr": float64(9), "by": MergeUser}}, func() Task { c := card; c.Status = "merged"; return c }(),
			ChatMessage{Kind: MessagePoke, Content: named + " is merged. The checks on main run now."}, true},
		{"a merge already done informs nobody", Event{Kind: EventPullRequestMerged, TaskID: "t-1", Detail: map[string]any{"pr": float64(9), "by": MergeUser}}, func() Task { c := card; c.Status = "done"; return c }(),
			ChatMessage{}, false},
		{"a closed card", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "closed"}}, card, ChatMessage{}, false},
		{"a chat task", Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "failed"}}, Task{ID: "t-1", Kind: KindChat}, ChatMessage{}, false},
		{"a plan done", Event{Kind: EventAllChildrenDone, TaskID: "t-plan", Detail: map[string]any{"cards": float64(6)}}, plan,
			ChatMessage{Kind: MessagePoke, Content: "The plan #558 (Members page) is done: all 6 cards."}, true},
		{"a chat budget spent", Event{Kind: EventChatBudgetSpent, TaskID: "t-chat-linkshort-2000-01", Actor: FactoryDecider, Via: ViaFactoryd,
			Detail: map[string]any{"budget_eur": float64(15), "spent_eur": 14.96, "suggest_eur": float64(3)}}, Task{ID: "t-chat-linkshort-2000-01", Kind: KindChat},
			ChatMessage{Kind: MessagePoke, Content: "The chat's budget for this month is used up: 14.96 of 15.00 EUR spent. Its turns wait until it is funded or the month turns."}, true},
		{"a level change", Event{Kind: EventAutonomyChanged, Actor: "sebastian", Detail: map[string]any{"from": "manual", "to": "assisted"}}, Task{},
			ChatMessage{Kind: MessagePoke, Content: "sebastian set the autonomy from manual to assisted."}, true},
		{"a cap change", Event{Kind: EventAutonomyChanged, Actor: "sebastian", Detail: map[string]any{"from": "assisted", "to": "assisted", "queue_cap": float64(5)}}, Task{},
			ChatMessage{Kind: MessagePoke, Content: "sebastian set the queue cap to 5."}, true},
		{"the agent's retry", agentDid(EventCardRetried, "assisted", map[string]any{}), card,
			ChatMessage{Kind: MessagePoke, Content: "The agent tried " + named + " again.", Decider: "assisted level · retried by the agent"}, true},
		{"the agent's filing at full auto", agentDid(EventCardFiled, "autonomous", map[string]any{"merge_rule": "auto"}), card,
			ChatMessage{Kind: MessagePoke, Content: "The agent filed " + named + ".", Decider: "full auto level · filed by the agent"}, true},
		{"the agent's grant", agentDid(EventSessionGranted, "assisted", map[string]any{}), card,
			ChatMessage{Kind: MessagePoke, Content: "The agent gave " + named + " one more session.", Decider: "assisted level · granted by the agent"}, true},
		{"the agent's filing at manual", agentDid(EventCardFiled, "manual", map[string]any{}), card, ChatMessage{}, false},
		{"the member's retry", Event{Kind: EventCardRetried, TaskID: "t-1", Detail: map[string]any{"autonomy": "assisted", "decider": "member"}}, card, ChatMessage{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := PokeOf(tt.e, tt.t)
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
			if tt.e.Kind != EventAutonomyChanged && tt.e.Kind != EventChatBudgetSpent {
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
	merged := Task{ID: "t-1", Kind: "agent", IssueNumber: 603, Status: "merged", FinishedAt: justNow(), Track: "direct"}
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
		{"the chain running", merge("running", 0, false), "#603 is merged. The checks on main run now.", true},
		{"the chain red with its repair", merge("failure", 1139, false), "#603 is merged, and the checks on main failed. #1139 repairs them.", true},
		{"the chain red before its repair card files", merge("failure", 0, false), "#603 is merged, and the checks on main failed.", true},
		{"another head's red, its repair on it", merge("running", 1139, false), "#603 is merged, and the checks on main failed. #1139 repairs them.", true},
		{"a chain that already covers the card", merge("failure", 0, true), "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := PokeOf(tt.e, merged)
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
	m, ok := PokeOf(Event{Kind: EventTaskEnded, TaskID: "t-1", Detail: map[string]any{"state": "done"}},
		Task{ID: "t-1", Kind: "agent", IssueNumber: 603, Status: "done", SpentEUR: 0.214,
			Objective: "Session store: pick the table's store\n\nAcceptance:\n- one store", Track: "direct"})
	if !ok {
		t.Fatal("pokeOf ok = false, want the done poke")
	}
	want := "#603 (Session store: pick the table's store) is done and live. It cost 0.21 EUR."
	if m.Content != want {
		t.Fatalf("poke = %q, want %q", m.Content, want)
	}
}

// A chat budget's poke asks at the needs-you level: the messenger's copy
// goes to every member whose plan carries the level, an inform though it
// is, for the chat stands until someone funds it.
func TestChatBudgetPokeDeliversAtNeedsYou(t *testing.T) {
	e := Event{Kind: EventChatBudgetSpent, TaskID: "t-chat-linkshort-2000-01", Detail: map[string]any{"budget_eur": float64(5)}}
	m, ok := PokeOf(e, Task{ID: "t-chat-linkshort-2000-01", Kind: KindChat})
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
		{ID: "t-1", Kind: "agent", Repository: "sf/code", IssueNumber: 603, Status: "waiting-human", Objective: "Session store", Track: "direct"},
		{ID: "t-2", Kind: "agent", Repository: "sf/other", IssueNumber: 7, Status: "failed", Objective: "Feed page", Track: "direct"},
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
			BirthKind: asked.Kind, Content: "#603 (Session store) needs you. Which store?", Choices: []string{"Postgres", "SQLite"}, TaskID: "t-1", Repeats: 1}
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
	if len(rows) != 2 || rows[1].Content != "#603 (Session store) needs you. Which port?" || rows[1].Repeats != 0 {
		t.Fatalf("bob's inbox = %+v, want the second question as its own row", rows)
	}

	// its withdrawal is the inbox's newest row: the answer row is gone
	record(Event{Repository: "sf/code", TaskID: "t-1", Kind: EventQuestionWithdrawn, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"question": "Which port?", "reason": "the checks failed"}})
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ChatMessages(ctx, "sf/code", "bob", inbox, 10)
	if len(rows) != 3 || rows[2].Kind != MessagePoke || rows[2].Content != "#603 (Session store) needs you no more: the checks failed." {
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
	if len(rows) != 4 || !rows[3].Queued || rows[3].Content != "#603 (Session store) is done and live. It cost 0.00 EUR." || !slices.Equal(line.landed["bob"], []int64{rows[3].ID}) {
		t.Fatalf("bob's inbox = %+v, landed %v: want the poke queued as its newest row and its id handed to the line", rows, line.landed["bob"])
	}
	mine, _ := s.EnsureInbox(ctx, "sf/code", "sebastian")
	rows, _ = s.ChatMessages(ctx, "sf/code", "sebastian", mine, 10)
	if last := rows[len(rows)-1]; last.Queued || last.Content != "#603 (Session store) is done and live. It cost 0.00 EUR." || line.landed["sebastian"] != nil {
		t.Fatalf("sebastian's newest row = %+v, landed %v: want the poke at once", last, line.landed["sebastian"])
	}

	// a merged card informs once at its merge, its words the chain's facts
	// as the pass reads them: a red chain names the repair card on it
	merged := Task{ID: "t-3", Kind: "agent", Repository: "sf/code", IssueNumber: 604, PRNumber: 9, Status: "merged", FinishedAt: justNow(), Objective: "Feed page", Track: "direct"}
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
	if err := s.RecordChainRun(ctx, "sf/code", 9, "ffeeddccb00112233445566778899aabbccddeeff", 21, 0, "failure"); err != nil {
		t.Fatal(err)
	}
	record(Event{Repository: "sf/code", TaskID: "t-3", Kind: EventPullRequestMerged, Actor: "sebastian", Via: ViaButton,
		Detail: map[string]any{"pr": float64(9), "by": MergeUser}})
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ChatMessages(ctx, "sf/code", "bob", inbox, 20)
	if len(rows) != 5 || rows[4].Content != "#604 (Feed page) is merged, and the checks on main failed. #1139 repairs them." {
		t.Fatalf("bob's inbox = %+v, want the merged inform naming the repair as its newest row", rows)
	}
}

// A failed card's poke is made from the reason its issue comment carries:
// Finish records it on the task_ended event. A policy gate's refusal runs
// past a notice's length, so the poke names the failed card alone and the
// card holds the reason.
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
	m, ok := PokeOf(Event{Kind: EventTaskEnded, TaskID: "t1", Detail: detail}, task)
	if want := "#4 (Add a make check target) failed."; !ok || m.Content != want {
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
		{ID: "t-rev", Kind: "agent", Repository: "sf/code", IssueNumber: 601, PRNumber: 9, Status: "in-review", Objective: "Feed page", Track: "direct"},
		{ID: "t-cls", Kind: "agent", Repository: "sf/code", IssueNumber: 602, PRNumber: 10, Status: "closing", Objective: "Feed page", Track: "direct"},
		{ID: "t-roll", Kind: "agent", Repository: "sf/code", IssueNumber: 603, PRNumber: 11, Status: "closing", Source: SourceRoll, Objective: "roll", Track: "direct"},
		{ID: "t-none", Kind: "agent", Repository: "sf/demo", IssueNumber: 604, PRNumber: 3, Status: "closing", Objective: "Feed page", Track: "direct"},
		{ID: "t-gone", Kind: "agent", Repository: "sf/gone", IssueNumber: 605, PRNumber: 4, Status: "closing", Objective: "Feed page", Track: "direct"},
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
	if want := []string{"#601 (Feed page) is merged. The checks on main run now.", "#602 (Feed page) is merged. The checks on main run now."}; !slices.Equal(got, want) {
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
	if len(rows) != 1 || rows[0].Content != "sebastian set the autonomy from manual to assisted." {
		t.Fatalf("bob's inbox = %+v, want the level change", rows)
	}
	mine, _ := s.EnsureInbox(ctx, "sf/code", "sebastian")
	if rows, _ := s.ChatMessages(ctx, "sf/code", "sebastian", mine, 10); len(rows) != 0 {
		t.Fatalf("sebastian's inbox = %+v, want his own level change kept out", rows)
	}
}

// The card's own ask and a monitor's reach the inbox in one pass, both
// rows written after both asks: the monitor's withdrawal names its case,
// so it settles that row alone, read through either inbox read and the
// messenger's copy, and its own row names the case. On the card once it
// ended, the monitor's ask that asks its issue closed stays open until a
// row names its case, and another monitor's ask moves on.
func TestTheInboxSettlesAMonitorsAskByItsCase(t *testing.T) {
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_poke_cases"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()
	ctx := t.Context()
	if _, err := s.db.Exec(`TRUNCATE chat_messages, chat_conversations, chat_seen, chat_poke_cursor, events, tasks, cases CASCADE`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, Task{ID: "t-1", Kind: "agent", Repository: "sf/code", IssueNumber: 603, Status: "waiting-human",
		Objective: "Session store", Track: "direct"}); err != nil {
		t.Fatal(err)
	}
	tree := policy.Tree{Projects: map[string]policy.ProjectConfig{"code": {Repository: "sf/code", Notify: []string{"sebastian"}}}}
	p := &Poker{Store: s, Current: func() policy.Version { return policy.Version{Tree: tree} }, Line: &busyLine{busy: map[string]bool{}, landed: map[string][]int64{}}}
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	flag, _, err := s.OpenCase(ctx, Case{TaskID: "t-1", Repository: "sf/code", Kind: CaseMonitorFlag, StateKey: "wait_over_bound|x",
		Problem: "Monitor flag: wait_over_bound"})
	if err != nil {
		t.Fatal(err)
	}
	record := func(kind string, detail map[string]any) {
		t.Helper()
		if _, err := s.Record(ctx, Event{Repository: "sf/code", TaskID: "t-1", IssueNumber: 603, Kind: kind, Actor: FactoryDecider, Via: ViaFactoryd, Detail: detail}); err != nil {
			t.Fatal(err)
		}
	}
	const flagged = "The monitor flagged this card: what should happen? Answer it in the project chat."
	record(EventQuestionAsked, map[string]any{"question": "Use a replica?"})
	record(EventQuestionAsked, map[string]any{"question": flagged, "case_id": flag.ID, "case_kind": string(CaseMonitorFlag)})
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	record(EventQuestionWithdrawn, map[string]any{"case_id": flag.ID, "case_kind": string(CaseMonitorFlag), "reason": "the wait ended"})
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	events, err := s.TaskEvents(ctx, "t-1", 500)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := s.EnsureInbox(ctx, "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.ChatMessages(ctx, "sf/code", "sebastian", inbox, 10)
	if err != nil {
		t.Fatal(err)
	}
	asks, err := s.ChatAsks(ctx, "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if want := "The flag on #603 (Session store) needs you no more: the wait ended."; len(rows) != 3 || len(asks) != 3 || rows[2].Content != want {
		t.Fatalf("the inbox = %+v, asks %+v; want the two asks and the withdrawal %q", rows, asks, want)
	}
	askRows := make([]ChatMessage, len(asks))
	for i, a := range asks {
		askRows[i] = a.ChatMessage
	}
	for read, got := range map[string][]ChatMessage{"ChatMessages": rows[:2], "ChatAsks": askRows[:2]} {
		var words []string
		for _, m := range got {
			words = append(words, SettleOf(m, events, "waiting-human", false).Word)
		}
		if !slices.Equal(words, []string{"", "withdrawn"}) {
			t.Fatalf("%s: the asks settle as %q, want the card's own open and the monitor's withdrawn", read, words)
		}
	}
	// the messenger's copy reads its row as the inbox does
	var copies []string
	for _, m := range rows[:2] {
		st, err := (&Sender{Store: s}).settleState(ctx, Delivery{Kind: m.Kind, BirthKind: m.BirthKind, TaskID: m.TaskID, At: m.At, Content: m.Content})
		if err != nil {
			t.Fatal(err)
		}
		copies = append(copies, st.Word)
	}
	if !slices.Equal(copies, []string{"", "withdrawn"}) {
		t.Fatalf("the messenger's copies settle as %q, want the card's own open and the monitor's withdrawn", copies)
	}

	// on the card once it ended, the monitor's ask that asks its issue
	// closed stays open until a row names its case, and another monitor's
	// ask moves on
	if _, err := s.db.Exec(`UPDATE tasks SET status = 'done', finished_at = now() WHERE id = 't-1'`); err != nil {
		t.Fatal(err)
	}
	var faced []int64
	for _, flag := range []string{"issue_open_after_end", "checkin_cut"} {
		c, _, err := s.OpenCase(ctx, Case{TaskID: "t-1", Repository: "sf/code", Kind: CaseMonitorFlag, StateKey: flag + "|x", Problem: "Monitor flag: " + flag})
		if err != nil {
			t.Fatal(err)
		}
		faced = append(faced, c.ID)
		record(EventQuestionAsked, map[string]any{"question": "The monitor flagged this card (" + flag + "): what should happen? Answer it in the project chat.",
			"case_id": c.ID, "case_kind": string(CaseMonitorFlag), "flag": flag})
	}
	copyWords := func() []string {
		t.Helper()
		if err := p.Pass(ctx); err != nil {
			t.Fatal(err)
		}
		if rows, err = s.ChatMessages(ctx, "sf/code", "sebastian", inbox, 10); err != nil || len(rows) < 5 {
			t.Fatalf("the inbox = %+v, %v; want the two asks as its fourth and fifth rows", rows, err)
		}
		var words []string
		for _, m := range rows[3:5] {
			st, err := (&Sender{Store: s}).settleState(ctx, Delivery{Kind: m.Kind, BirthKind: m.BirthKind, TaskID: m.TaskID, At: m.At, Content: m.Content})
			if err != nil {
				t.Fatal(err)
			}
			words = append(words, st.Word)
		}
		return words
	}
	if words := copyWords(); !slices.Equal(words, []string{"", "moved on"}) {
		t.Fatalf("the messenger's copies of the ended card's monitor asks settle as %q, want the issue's open and the other moved on", words)
	}
	record(EventQuestionWithdrawn, map[string]any{"case_id": faced[0], "reason": "the issue is closed"})
	if words := copyWords(); !slices.Equal(words, []string{"withdrawn", "moved on"}) {
		t.Fatalf("after the issue's withdrawal the copies settle as %q, want the issue's withdrawn", words)
	}
}
