package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A wake turn runs without a request: the Waker's call alone writes the
// user row with its birth kind and answers as the model. The member never
// logged in here; the turn needs no login of theirs.
func TestStartWakeRunsATurnWithoutHTTP(t *testing.T) {
	r, chat, g := newChatRig(t)
	g.replies = [][]string{{say("I looked.")}}
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "wake_ask", "Wake: 2 asks wait."); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	if calls := g.calls(); len(calls) != 1 {
		t.Fatalf("%d model calls, want 1", len(calls))
	}
	rows, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", conv, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Role != "user" || rows[0].BirthKind != "wake_ask" || rows[0].Content != "Wake: 2 asks wait." ||
		rows[1].Role != "assistant" || rows[1].Content != "I looked." {
		t.Fatalf("the wake's rows = %+v", rows)
	}
}

// Each turn runs on its origin's class of the chat block, its model calls
// and its window read alike: a member's own turn on model, an MCP
// client's on mcp_model, a wake's on wake_model.
func TestEachTurnRunsOnItsOriginsModel(t *testing.T) {
	r, chat, g := newChatRig(t)
	r.ledger.chat = &policy.ChatConfig{Model: policy.Premium, MCPModel: policy.Small, WakeModel: policy.Big}
	g.window = 200000
	ck, s := r.loggedIn(t)
	g.alias, g.replies = policy.Premium, [][]string{{say("Two cards wait.")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"what is open?"}})
	g.alias, g.replies = policy.Small, [][]string{{say("hi")}}
	answerOf(t, tokenSession(t, r), "chat_send", map[string]any{"message": "hi"})
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	g.alias, g.replies = policy.Big, [][]string{{say("I looked.")}}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "wake_ask", "Wake: 1 ask waits."); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	var models []any
	for _, c := range g.calls() {
		models = append(models, c["model"])
	}
	equal(t, "the turns' classes", models, []any{policy.Premium, policy.Small, policy.Big})
	equal(t, "the windows asked for", g.windows, []string{policy.Premium, policy.Small, policy.Big})
}

// A wake whose model cannot be reached ends in a row that says so, under
// the wake's own: no member reads its stream, so the thread keeps the
// failure.
func TestAWakesFailureStaysInItsThread(t *testing.T) {
	r, chat, g := newChatRig(t)
	g.replies = [][]string{nil}
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "wake_ask", "Wake: 1 ask waits for a human."); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	rows, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", conv, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].BirthKind != "wake_ask" ||
		rows[1].Role != "assistant" || rows[1].Content != "This turn failed: The model could not be reached; try again." {
		t.Fatalf("the wake's rows = %+v", rows)
	}
}

// The inbox takes presses only, so an ask there without choices is
// refused, naming choices and attach_analysis, and stores no ask row,
// whatever started the turn: a wake or a press. A wake in a thread of the
// human's own hears the ask's plain bounds instead, and an ask with
// choices asks in the inbox as anywhere.
func TestInboxAskNeedsChoices(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	ctx := t.Context()
	inbox, err := chat.EnsureInbox(ctx, "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chat.AddChatMessage(ctx, "sf/code", "sebastian", factoryd.ChatMessage{Role: "user", Content: "what is open?", Conversation: 1}); err != nil {
		t.Fatal(err)
	}
	const bare = `{"ask":"Which store keeps the snapshots?"}`
	const refused = "Refused: The inbox takes buttons only: give choices, or record what you found with attach_analysis."
	rowsIn := func(conv int64) []factoryd.ChatMessage {
		t.Helper()
		rows, err := chat.ChatMessages(ctx, "sf/code", "sebastian", conv, 50)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	lastAsk := func(conv int64) string {
		t.Helper()
		result := ""
		for _, m := range rowsIn(conv) {
			for _, c := range m.ToolCalls {
				if c.Name == "ask" {
					result = c.Result
				}
			}
		}
		return result
	}
	wake := func(conv int64, args string, replies ...[]string) {
		t.Helper()
		g.replies = append([][]string{{call(1, "ask", args)}}, replies...)
		if err := r.c.StartWake(ctx, "ai-software-factory-code", "sebastian", conv, "wake_ask", "#13 asks which store."); err != nil {
			t.Fatalf("StartWake: %v", err)
		}
	}

	wake(inbox, bare, []string{say("I recorded it instead.")})
	equal(t, "a wake's bare ask in the inbox", lastAsk(inbox), refused)
	wake(1, bare, []string{say("I will ask with two choices.")})
	equal(t, "a wake's bare ask in an own thread", lastAsk(1), "Refused: choices: 2 to 5 answers of 1 to 60 characters")

	id, err := chat.AddChatMessage(ctx, "sf/code", "sebastian", factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageAsk,
		Content: "Snapshots nightly?", Choices: []string{"Yes", "No"}, Conversation: inbox})
	if err != nil {
		t.Fatal(err)
	}
	g.replies = [][]string{{call(1, "ask", bare)}, {say("I recorded it instead.")}}
	code, events, _ := chatPost(t, r, ck, s, chatPath+"/answer",
		url.Values{"conversation": {strconv.FormatInt(inbox, 10)}, "message": {strconv.FormatInt(id, 10)}, "choice": {"Yes"}})
	want := `tool {"arguments":` + bare + `,"duration_ms":0,"name":"ask","result":"` + refused + `"}`
	if code != http.StatusOK || !slices.Contains(events, want) {
		t.Fatalf("a press's turn asking bare in the inbox = %d %v, want %s", code, events, want)
	}
	if slices.ContainsFunc(rowsIn(inbox), func(m factoryd.ChatMessage) bool { return m.Content == "Which store keeps the snapshots?" }) {
		t.Fatalf("a refused ask stored a row: %+v", rowsIn(inbox))
	}

	wake(inbox, `{"ask":"Which store keeps the snapshots?","choices":["Postgres","SQLite"]}`)
	rows := rowsIn(inbox)
	ask := rows[len(rows)-1]
	if ask.Kind != factoryd.MessageAsk || ask.Content != "Which store keeps the snapshots?" || !slices.Equal(ask.Choices, []string{"Postgres", "SQLite"}) ||
		len(ask.ToolCalls) != 1 || ask.ToolCalls[0].Result != "Asked the human; this turn ends. A press answers it, and in a thread of their own so does their next message." {
		t.Fatalf("the wake's ask row = %+v", ask)
	}
}

// A wake on a thread whose turn runs waits for it: its user row lands
// queued while the member's turn holds the thread, and its turn starts
// only after theirs ends.
func TestStartWakeQueuesBehindALiveTurn(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{say("the member's answer")}, {say("the wake's answer")}}
	g.hold, g.arrived = make(chan struct{}), make(chan struct{}, 2)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		code, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"what is open?"}})
		if code != 200 || len(events) == 0 {
			t.Errorf("the member's turn = %d %v", code, events)
		}
	}()
	<-g.arrived // the member's model call is in flight and held
	wakeDone := make(chan error, 1)
	go func() {
		wakeDone <- r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", 1, "wake_ask", "Wake: 1 ask waits.")
	}()
	queued := waitFor(t, time.Second, func() bool {
		rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 1, 10)
		return len(rows) == 2 && rows[1].Queued && rows[1].BirthKind == "wake_ask"
	})
	if !queued {
		rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 1, 10)
		t.Fatalf("the wake's row did not land queued while the member's turn ran: %+v", rows)
	}
	close(g.hold)
	if err := <-wakeDone; err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	wg.Wait()
	rows, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	var user, assistant int
	for _, m := range rows {
		if m.BirthKind == "wake_ask" && m.Role == "user" {
			user++
			if m.Queued {
				t.Errorf("the wake's user row stayed queued: %+v", m)
			}
		}
		if m.Role == "assistant" && m.Content == "the wake's answer" {
			assistant++
		}
	}
	if user != 1 || assistant != 1 {
		t.Fatalf("the wake's rows after both turns = %+v", rows)
	}
}

// A wake on a chat its budget stopped is refused before any row or model
// call, with the sentinel the Waker holds the wake on; once a fund lifts
// the stop, the same wake runs.
func TestStartWakeRefusesAStoppedChat(t *testing.T) {
	r, chat, g := newChatRig(t)
	task := stoppedChat(t, r)
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	err = r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "wake_ask", "Wake: 1 ask waits.")
	if !errors.Is(err, factoryd.ErrChatUnfunded) {
		t.Fatalf("StartWake on the stopped chat = %v, want ErrChatUnfunded", err)
	}
	if rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", conv, 10); len(rows) != 0 || len(g.calls()) != 0 {
		t.Fatalf("the refused wake left rows %+v and %d model calls", rows, len(g.calls()))
	}
	if _, _, err := r.tasks.RaiseTaskBudget(t.Context(), task.ID, 3); err != nil {
		t.Fatal(err)
	}
	g.replies = [][]string{{say("I looked.")}}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "wake_ask", "Wake: 1 ask waits."); err != nil {
		t.Fatalf("StartWake after the fund: %v", err)
	}
	if rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", conv, 10); len(rows) != 2 || rows[1].Content != "I looked." {
		t.Fatalf("the wake after the fund = %+v", rows)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, within time.Duration, cond func() bool) bool {
	t.Helper()
	for start := time.Now(); time.Since(start) < within; {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// The two refusals a wake cannot outlive name the sentinel the Waker
// drops its row on: a thread that is not there any more and an archived
// one are both gone for good, never a cooldown.
func TestStartWakeRefusalNamesAVenueThatIsGone(t *testing.T) {
	r, chat, _ := newChatRig(t)
	conv, err := chat.NewConversation(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chat.ArchiveConversation(t.Context(), "sf/code", "sebastian", conv, true); err != nil {
		t.Fatal(err)
	}
	for name, n := range map[string]int64{"archived": conv, "missing": conv + 400} {
		err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", n, "wake_ask", "Wake: 1 ask waits.")
		if !errors.Is(err, factoryd.ErrWakeVenueGone) {
			t.Fatalf("%s venue: StartWake returned %v, want ErrWakeVenueGone", name, err)
		}
	}
}

// A wake is the project agent's own turn: every Forgejo call of it goes
// out as factory-chat with the project's chat token. The woken member's
// login, whose tokens here expire within the refresh margin, is neither
// read nor refreshed.
func TestAWakeActsAsTheChatBot(t *testing.T) {
	r, chat, g := newChatRig(t)
	r.fg.expiresIn = 30
	r.loggedIn(t)
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	r.fg.mu.Lock()
	calls, grants := len(r.fg.calls), len(r.fg.grants)
	r.fg.mu.Unlock()
	r.sessions.mu.Lock()
	reads, updates := r.sessions.reads, r.sessions.tokens
	r.sessions.mu.Unlock()
	g.replies = [][]string{{call(1, "comment", `{"task_id":"t-m","text":"Looks right."}`)}, {say("Commented on #5.")}}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "wake_ask", "Wake: 1 ask waits."); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	r.fg.mu.Lock()
	wake, granted := slices.Clone(r.fg.calls[calls:]), len(r.fg.grants)-grants
	r.fg.mu.Unlock()
	if !slices.Contains(wake, "POST /api/v1/repos/sf/code/issues/5/comments Bearer chat-token") {
		t.Fatalf("the wake's Forgejo calls = %q, want the comment posted with the chat token", wake)
	}
	for _, c := range wake {
		if !strings.HasSuffix(c, " Bearer chat-token") {
			t.Errorf("the wake called %q, not as the chat bot", c)
		}
	}
	if got := r.fg.commentBodies("/api/v1/repos/sf/code/issues/5/comments"); !slices.Equal(got, []string{"Looks right."}) {
		t.Errorf("comments on #5 = %q", got)
	}
	r.sessions.mu.Lock()
	defer r.sessions.mu.Unlock()
	if granted != 0 || r.sessions.reads != reads || r.sessions.tokens != updates {
		t.Errorf("the wake made %d token grants, %d session reads and %d token updates; want none", granted, r.sessions.reads-reads, r.sessions.tokens-updates)
	}
}

// A project without a chat token has no identity a wake can act as: the
// wake is refused before the model is called or anything is stored, and
// the refusal names tokens.chat.
func TestAWakeOnAProjectWithoutAChatTokenIsRefused(t *testing.T) {
	r, chat, g := newChatRig(t)
	r.chatToken = ""
	r.loggedIn(t)
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	g.replies = [][]string{{call(1, "comment", `{"task_id":"t-m","text":"Looks right."}`)}, {say("Commented on #5.")}}
	err = r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "wake_ask", "Wake: 1 ask waits.")
	if err == nil || !strings.Contains(err.Error(), "ai-software-factory-code") || !strings.Contains(err.Error(), "tokens.chat") {
		t.Fatalf("StartWake = %v, want a refusal naming the project and tokens.chat", err)
	}
	rows, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", conv, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || len(g.calls()) != 0 || len(r.fg.commentBodies("/api/v1/repos/sf/code/issues/5/comments")) != 0 {
		t.Fatalf("the refused wake stored %+v, made %d model calls and commented", rows, len(g.calls()))
	}
}

// An approval the agent decided is the factory's own, in a wake or
// unconfirmed in a member's turn: the merge request's approval goes to the
// workflow as factory-chat's, and no review is posted (the merge gate's
// review carries it at the merge). A red-checks request reads only the
// head's checks, with the turn's token. A resource request is decided as
// factory-chat. The events stay the woken or chatting member's, decided by
// the agent. What the member decides stays theirs: their review, their
// name.
func TestAnApprovalTheAgentDecidedIsTheFactorysOwn(t *testing.T) {
	inWake := func(t *testing.T, r *rig) actor {
		t.Helper()
		s, err := r.c.wakeSession(factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}, "sebastian")
		if err != nil {
			t.Fatal(err)
		}
		return actor{s: s, repository: "sf/code", via: factoryd.ViaChat, wake: true, decider: "agent", level: policy.AutonomyAutonomous, mergeRule: policy.MergeAuto}
	}
	inTurn := func(t *testing.T, r *rig, decider string) actor {
		t.Helper()
		_, s := r.loggedIn(t)
		r.fg.calls = nil
		return actor{s: s, repository: "sf/code", via: factoryd.ViaChat, decider: decider, level: policy.AutonomyAutonomous, mergeRule: policy.MergeAuto}
	}
	approve := func(t *testing.T, r *rig, a actor, id int64) string {
		t.Helper()
		note, err := r.c.doApprove(t.Context(), a, id)
		if err != nil {
			t.Fatalf("approve %d: %v", id, err)
		}
		return note
	}
	approved := func(t *testing.T, r *rig, decider string) {
		t.Helper()
		var rows []factoryd.Event
		for _, e := range r.events.all() {
			if e.Kind == factoryd.EventMergeApproved {
				rows = append(rows, e)
			}
		}
		if len(rows) != 1 || rows[0].Actor != "sebastian" || rows[0].Detail["decider"] != decider {
			t.Fatalf("merge_approved rows = %+v, want one of sebastian decided by the %s", rows, decider)
		}
	}
	t.Run("merge in a wake", func(t *testing.T) {
		r := newRig(t)
		if note := approve(t, r, inWake(t, r), 4); note != "Approved pull request #9; factoryd merges it." {
			t.Fatalf("note = %q", note)
		}
		if !slices.Equal(r.fg.calls, []string{"SIGNAL approved t-m #9 by factory-chat"}) || len(r.fg.reviews) != 0 {
			t.Fatalf("calls %q, reviews %v: want the approval signalled as factory-chat's, no review", r.fg.calls, r.fg.reviews)
		}
		approved(t, r, "agent")
	})
	t.Run("merge with red checks in a wake", func(t *testing.T) {
		r := newRig(t)
		r.approvals.rows[1].Spec = json.RawMessage(`{"risk_class":"standard","head_sha":"abc123","red_checks":true}`)
		r.fg.statuses = map[string]string{"abc123": `{"state":"success","statuses":[{"context":"ci / test","status":"success"}]}`}
		approve(t, r, inWake(t, r), 4)
		if !slices.Equal(r.fg.calls, []string{"GET /api/v1/repos/sf/code/commits/abc123/status Bearer chat-token", "SIGNAL approved t-m #9 by factory-chat"}) ||
			len(r.fg.reviews) != 0 {
			t.Fatalf("calls %q, reviews %v: want the head's checks read with the chat token, the approval signalled, no review", r.fg.calls, r.fg.reviews)
		}
		approved(t, r, "agent")
	})
	t.Run("resource in a wake", func(t *testing.T) {
		r := newRig(t)
		if note := approve(t, r, inWake(t, r), 3); note != "Approved the resource for its task." {
			t.Fatalf("note = %q", note)
		}
		if q := r.approvals.get(3); q.Status != "approved" || q.DecidedBy != "factory-chat" || len(r.fg.calls) != 0 {
			t.Fatalf("request 3 %s by %s, calls %q: want it approved by factory-chat with no Forgejo call", q.Status, q.DecidedBy, r.fg.calls)
		}
		approved(t, r, "agent")
	})
	t.Run("merge in a member's turn the agent decided", func(t *testing.T) {
		r := newRig(t)
		approve(t, r, inTurn(t, r, "agent"), 4)
		if !slices.Equal(r.fg.calls, []string{"SIGNAL approved t-m #9 by factory-chat"}) || len(r.fg.reviews) != 0 {
			t.Fatalf("calls %q, reviews %v: want the approval signalled as factory-chat's, no review", r.fg.calls, r.fg.reviews)
		}
		approved(t, r, "agent")
	})
	t.Run("merge the member decided", func(t *testing.T) {
		r := newRig(t)
		approve(t, r, inTurn(t, r, "member"), 4)
		if !slices.Equal(r.fg.calls, []string{"POST /api/v1/repos/sf/code/pulls/9/reviews Bearer access-1", "SIGNAL approved t-m #9 by sebastian"}) {
			t.Fatalf("calls %q: want the member's review and their approval", r.fg.calls)
		}
		approved(t, r, "member")
	})
}

// wakeCall is a stored tool call of a wake's turn: its name, result,
// whether it stored a proposal, and its refusal status.
type wakeCall struct {
	name, result string
	proposed     bool
	status       int
}

// mergeWake runs a merge row's wake on sebastian's inbox, or with inbox
// false on his own thread 1, at autonomous under merge rule rule, the
// model answering replies, and answers the rig, its chat and the turn's
// stored tool calls.
func mergeWake(t *testing.T, rule string, inbox bool, setup func(*rig), replies ...[]string) (*rig, *fakeChat, []wakeCall) {
	t.Helper()
	r, chat, g := newChatRig(t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", Autonomy: policy.AutonomyAutonomous, MergeRule: rule}}
	if setup != nil {
		setup(r)
	}
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if !inbox {
		if _, err := chat.AddChatMessage(t.Context(), "sf/code", "sebastian", factoryd.ChatMessage{Role: "user", Content: "what is open?", Conversation: 1}); err != nil {
			t.Fatal(err)
		}
		conv = 1
	}
	g.replies = replies
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "wake_ask",
		"Wake: 1 ask waits for a human.\n#5: merge of PR #9, high risk"); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	rows, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", conv, 20)
	if err != nil {
		t.Fatal(err)
	}
	var calls []wakeCall
	for _, m := range rows {
		for _, c := range m.ToolCalls {
			calls = append(calls, wakeCall{name: c.Name, result: c.Result, proposed: c.Proposal != "", status: c.Status})
		}
	}
	return r, chat, calls
}

// The inbox decides no merge at merge rule human: a wake's approve_merge
// and deny_merge there refuse naming attach_analysis before any proposal
// is stored, and the request stays pending. A wake-flagged turn on the
// human's own thread, a fund's resume or a retry, proposes them as
// before, and at merge rule auto the inbox's wake decides the merge as the
// factory's own.
func TestWakeApproveMergeRefusedOnMergeRuleHuman(t *testing.T) {
	const refused = "Refused: A human approves merges on merge rule human: record what you found with attach_analysis."
	merges := []string{call(1, "approve_merge", `{"task_id":"t-m"}`, "deny_merge", `{"task_id":"t-m","action":"reject","note":"The column breaks the index."}`)}
	pending := func(t *testing.T, r *rig, chat *fakeChat, want int) {
		t.Helper()
		chat.mu.Lock()
		proposals := len(chat.proposals)
		chat.mu.Unlock()
		if q := r.approvals.get(4); proposals != want || q.Status != "pending" || len(r.fg.reviews) != 0 {
			t.Fatalf("%d proposals, request 4 %s, reviews %v: want %d proposals, the request pending, no review", proposals, q.Status, r.fg.reviews, want)
		}
	}

	r, chat, calls := mergeWake(t, policy.MergeHuman, true, nil, merges, []string{say("A human decides this merge.")})
	equal(t, "the inbox wake's calls at merge rule human", calls, []wakeCall{
		{name: "approve_merge", result: refused, status: http.StatusForbidden},
		{name: "deny_merge", result: refused, status: http.StatusForbidden}})
	pending(t, r, chat, 0)

	r, chat, calls = mergeWake(t, policy.MergeHuman, false, nil, merges)
	equal(t, "the own thread's wake calls at merge rule human", calls, []wakeCall{
		{name: "approve_merge", proposed: true,
			result: `Proposed to sebastian: Approve a merge, #5 t-m "Fix the cache. It drops entries.". Nothing runs until they press Do it.`},
		{name: "deny_merge", proposed: true,
			result: `Proposed to sebastian: Deny a merge, #5 t-m "Fix the cache. It drops entries.", action reject, note The column breaks the index. Nothing runs until they press Do it.`}})
	pending(t, r, chat, 2)

	r, _, calls = mergeWake(t, policy.MergeAuto, true, nil,
		[]string{call(1, "approve_merge", `{"task_id":"t-m"}`)}, []string{say("Approved #5.")})
	equal(t, "the inbox wake's call at merge rule auto", calls, []wakeCall{
		{name: "approve_merge", result: "Approved pull request #9; factoryd merges it. It cannot be undone."}})
	if q := r.approvals.get(4); q.Status != "approved" || q.DecidedBy != factoryd.ChatUser {
		t.Fatalf("request 4 %s by %s, want approved by %s", q.Status, q.DecidedBy, factoryd.ChatUser)
	}
}

// A human who approved first settled the merge: the wake's approve_merge
// finds no pending merge request and refuses naming the card's state, and
// attach_analysis finds the ask settled, naming who approved it.
func TestWakeApproveMergeAfterAHumanApprovedAnswers409(t *testing.T) {
	humanFirst := func(r *rig) {
		r.tasks.mu.Lock()
		for i := range r.tasks.tasks {
			if k := &r.tasks.tasks[i]; k.ID == "t-m" {
				k.Status, k.PRNumber = "in-review", 9
			}
		}
		r.tasks.mu.Unlock()
		_, s := r.loggedIn(t)
		if _, err := r.c.doApprove(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaConsole}, 4); err != nil {
			t.Fatalf("the human's approval: %v", err)
		}
		// the store's answer on a card whose merge request a human approved
		r.c.cfg.AttachAnalysis = func(_ context.Context, taskID, _, _ string, _ factoryd.Event) error {
			if q := r.approvals.get(4); q.TaskID == taskID && q.Status == "approved" {
				return &factoryd.AskSettledError{Kind: factoryd.ApprovalMerge, Word: "approved", By: q.DecidedBy}
			}
			return nil
		}
	}
	_, _, calls := mergeWake(t, policy.MergeAuto, true, humanFirst,
		[]string{call(1, "approve_merge", `{"task_id":"t-m"}`, "attach_analysis", `{"task_id":"t-m","found":"The column is added with its default."}`)},
		[]string{say("A human approved #5 already.")})
	equal(t, "the wake's calls after the human's approval", calls, []wakeCall{
		{name: "approve_merge", status: http.StatusConflict,
			result: "Refused: Card #5 passes the gates after its session, not a pending merge request: its gates run; nothing to do until they end."},
		{name: "attach_analysis", status: http.StatusConflict,
			result: "Refused: The merge request of #5 is approved by sebastian. A settled ask is done; escalate files a new one when you need a person."}})
}

// A card a wake files is the woken member's: the issue goes out as the
// chat bot, and the console's card_filed row, which the intake reads for
// the card's author (factoryd.Intake.Filings), names the member. When the
// row cannot be written the issue would be no card: the console comments
// why, closes it and refuses naming it.
func TestAWakesFilingIsTheWokenMembers(t *testing.T) {
	file := func(t *testing.T, r *rig) (int64, error) {
		t.Helper()
		s, err := r.c.wakeSession(factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}, "sebastian")
		if err != nil {
			t.Fatal(err)
		}
		a := actor{s: s, repository: "sf/code", via: factoryd.ViaChat, wake: true, decider: "agent"}
		_, n, err := r.c.doFileCard(t.Context(), a, "ai-software-factory-code", "Bump the dep", "Bump it.\n\nAcceptance:\n- go build passes", "direct", "a routine bump", "", "small", "", "", "", 0, 0)
		return n, err
	}
	t.Run("recorded", func(t *testing.T) {
		r := newRig(t)
		if n, err := file(t, r); err != nil || n != 41 {
			t.Fatalf("file = #%d, %v", n, err)
		}
		if !slices.Contains(r.fg.calls, "POST /api/v1/repos/sf/code/issues Bearer chat-token") {
			t.Fatalf("calls %q: want the issue filed with the chat token", r.fg.calls)
		}
		var filed []factoryd.Event
		for _, e := range r.events.all() {
			if e.Kind == factoryd.EventCardFiled {
				filed = append(filed, e)
			}
		}
		if len(filed) != 1 || filed[0].Actor != "sebastian" || filed[0].IssueNumber != 41 || filed[0].Repository != "sf/code" {
			t.Fatalf("card_filed rows = %+v, want one of sebastian for sf/code#41", filed)
		}
		if len(r.fg.patched) != 0 {
			t.Fatalf("patched %q: want the recorded filing left open", r.fg.patched)
		}
	})
	t.Run("not recorded", func(t *testing.T) {
		r := newRig(t)
		r.events.fail = errors.New("the database is down")
		n, err := file(t, r)
		var no *refusal
		if !errors.As(err, &no) || no.msg != "The factory could not record the filing of sf/code#41, so it is no card; the issue is closed. File the card again." || n != 0 {
			t.Fatalf("file = #%d, %v", n, err)
		}
		if got := r.fg.commentBodies("/api/v1/repos/sf/code/issues/41/comments"); !slices.Equal(got, []string{"factoryd could not record this filing, so it is no card: the project agent closed it."}) {
			t.Fatalf("comments on #41 = %q", got)
		}
		if !slices.Equal(r.fg.patched, []string{`/api/v1/repos/sf/code/issues/41 {"state":"closed"}`}) ||
			!slices.Contains(r.fg.calls, "PATCH /api/v1/repos/sf/code/issues/41 Bearer chat-token") {
			t.Fatalf("patched %q, calls %q: want #41 closed as the chat bot", r.fg.patched, r.fg.calls)
		}
	})
}

// A change request the agent decided goes out under the chat identity,
// whoever is online: in a member's turn at full auto the review carries
// the chat token and the request is decided by factory-chat. The member's
// own change request stays their review.
func TestTheAgentsChangeRequestIsTheChatBots(t *testing.T) {
	for _, tc := range []struct {
		decider, bearer, by string
	}{{"agent", "Bearer chat-token", "factory-chat"}, {"member", "Bearer access-1", "sebastian"}} {
		t.Run(tc.decider, func(t *testing.T) {
			r := newRig(t)
			_, s := r.loggedIn(t)
			r.fg.calls = nil
			a := actor{s: s, repository: "sf/code", via: factoryd.ViaChat, decider: tc.decider, level: policy.AutonomyAutonomous, mergeRule: policy.MergeAuto}
			if _, err := r.c.doDeny(t.Context(), a, 4, "request_changes", "Split the handler."); err != nil {
				t.Fatalf("deny: %v", err)
			}
			if !slices.Equal(r.fg.calls, []string{"POST /api/v1/repos/sf/code/pulls/9/reviews " + tc.bearer}) {
				t.Fatalf("calls %q: want the change request with %s", r.fg.calls, tc.bearer)
			}
			if rv := r.fg.reviews[0]; rv["event"] != "REQUEST_CHANGES" || rv["body"] != "Split the handler." || rv["commit_id"] != "abc123" {
				t.Fatalf("review = %v", rv)
			}
			if q := r.approvals.get(4); q.Status != "denied" || q.DecidedBy != tc.by {
				t.Fatalf("request 4 %s by %s, want denied by %s", q.Status, q.DecidedBy, tc.by)
			}
		})
	}
}
