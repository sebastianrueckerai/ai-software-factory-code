package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// budgetReason is the gateway's refused_budget reason as it read live: the
// call it shrinks to the crumbs left fits no output token.
const budgetReason = "free headroom 0.038793 EUR (task 0.038793 of 15.000000) fits 0 output tokens after a request of 117558 bytes " +
	"(measured 29390 prompt tokens), below the minimum call of 1024"

// crumbsChat files the project's chat task of this month with 14.96 of its
// 15 EUR spent, as the gateway leaves it: it shrinks every call to what is
// left, so the spend never reaches the budget.
func crumbsChat(t *testing.T, r *rig) factoryd.Task {
	t.Helper()
	task, err := r.chat.EnsureChatTask(t.Context(), "ai-software-factory-code", "sf/code", 15, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r.tasks.mu.Lock()
	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].ID == task.ID {
			r.tasks.tasks[i].SpentEUR = 14.96
		}
	}
	r.tasks.mu.Unlock()
	task.SpentEUR = 14.96
	return task
}

// stoppedChat is crumbsChat with its stop recorded as the gateway's first
// refusal records it (askFund).
func stoppedChat(t *testing.T, r *rig) factoryd.Task {
	t.Helper()
	task := crumbsChat(t, r)
	r.c.askFund(t.Context(), factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}, task)
	return task
}

// eventsOf are the rig's event rows of kind so far.
func eventsOf(r *rig, kind string) []factoryd.Event {
	var rows []factoryd.Event
	for _, e := range r.events.all() {
		if e.Kind == kind {
			rows = append(rows, e)
		}
	}
	return rows
}

// chatRead is GET path's answer to the member of ck.
func chatRead(t *testing.T, r *rig, ck *http.Cookie, path string) chatGetAnswer {
	t.Helper()
	rec := r.do(httptest.NewRequest(http.MethodGet, path, nil), ck)
	var answer chatGetAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("GET %s = %d %s: %v", path, rec.Code, rec.Body, err)
	}
	return answer
}

// stopOf is the budget event of a turn's stream that ended on a stop,
// whose events are the stop and done.
func stopOf(t *testing.T, events []string) (budgetEvent, map[string]json.RawMessage) {
	t.Helper()
	data, ok := strings.CutPrefix(events[0], "budget ")
	var got budgetEvent
	var keys map[string]json.RawMessage
	if len(events) != 2 || !ok || events[1] != "done {}" || json.Unmarshal([]byte(data), &got) != nil || json.Unmarshal([]byte(data), &keys) != nil {
		t.Fatalf("the stream = %q, want the stop's budget event and done", events)
	}
	return got, keys
}

// The gateway's refused_budget on a chat call with crumbs left (14.96 of
// 15 spent) ends the turn on a stop row of kind budget, its first line
// plain and the gateway's reason the next, and the stream carries the row
// with the fund state: the budget, the spend, the fund suggested (a spend
// of nothing over the week suggests the floor of 1) and the approver's
// right to fund. The first refusal records the stop once, the
// chat_budget_spent event on the month's chat task; the second, while the
// chat stands stopped, records none, and GET /chat offers the fund.
func TestChatBudgetStopIsARowAndOneAsk(t *testing.T) {
	r, _, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	task := crumbsChat(t, r)
	g.replies = [][]string{{refuseFor(policy.RefusedBudget, budgetReason)}, {refuseFor(policy.RefusedBudget, budgetReason)}}
	stop := "The chat's budget for " + time.Now().UTC().Format("January") +
		" is used up: €14.96 of €15.00 spent, and the next call does not fit what is left.\n" + budgetReason
	fund := &chatBudget{BudgetEUR: 15, SpentEUR: 14.96, SuggestEUR: 1, CanFund: true}
	for _, message := range []string{"what is open?", "and now?"} {
		_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {message}})
		got, _ := stopOf(t, events)
		if got.Message.Kind != factoryd.MessageBudget || got.Message.BirthKind != factoryd.EventChatBudgetSpent || got.Message.Role != "assistant" ||
			got.Message.Content != stop || got.Message.ID == 0 || !reflect.DeepEqual(got.Budget, fund) {
			t.Fatalf("the stop of %q = %+v, budget %+v; want the row %q and the fund %+v", message, got.Message, got.Budget, stop, fund)
		}
	}
	asks := eventsOf(r, factoryd.EventChatBudgetSpent)
	if len(asks) != 1 || asks[0].Repository != "sf/code" || asks[0].TaskID != task.ID || asks[0].Actor != factoryd.FactoryDecider ||
		asks[0].Via != factoryd.ViaFactoryd || !reflect.DeepEqual(asks[0].Detail, map[string]any{"budget_eur": 15.0, "spent_eur": 14.96, "suggest_eur": 1.0}) {
		t.Fatalf("the stop's asks = %+v, want the first refusal's alone", asks)
	}
	answer := chatRead(t, r, ck, chatPath)
	var kinds []string
	for _, m := range answer.Messages {
		kinds = append(kinds, m.Role+" "+m.Kind)
		if m.Settled {
			t.Errorf("row %+v settled while the chat stands stopped", m)
		}
	}
	if !slices.Equal(kinds, []string{"user ", "assistant budget", "user ", "assistant budget"}) || !reflect.DeepEqual(answer.Budget, fund) {
		t.Fatalf("GET /chat: rows %q, budget %+v; want the two messages each with its stop, and the fund %+v", kinds, answer.Budget, fund)
	}
}

// A factory cap's refusal ends the turn on a budget row too, naming the
// cap and what raises it, the gateway's reason the next line; it asks for
// no fund of the chat and offers none.
func TestChatCapStopNamesTheCap(t *testing.T) {
	const reason = "free headroom 0.000000 EUR (month 0.000000 of 300.000000); the month cap budget.month_eur in policy.yaml is spent, and only a human raises it, by a commit"
	for _, tc := range []struct{ decision, first string }{
		{policy.RefusedDayCap, "The factory's day cap is used up: the chat goes on when the UTC day turns, or once a commit raises budget.day_eur in policy.yaml."},
		{policy.RefusedMonthCap, "The factory's month cap is used up: the chat goes on when the UTC month turns, or once a commit raises budget.month_eur in policy.yaml."},
		{policy.RefusedProjectCap, "The project's month cap is used up: the chat goes on when the UTC month turns, or once a commit raises budget.month_eur in projects/ai-software-factory-code.yaml."},
	} {
		t.Run(tc.decision, func(t *testing.T) {
			r, _, g := newChatRig(t)
			ck, s := r.loggedIn(t)
			crumbsChat(t, r)
			g.replies = [][]string{{refuseFor(tc.decision, reason)}}
			_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"what is open?"}})
			got, keys := stopOf(t, events)
			if _, ok := keys["budget"]; ok || got.Message.Kind != factoryd.MessageBudget || got.Message.BirthKind != "" || got.Message.Content != tc.first+"\n"+reason {
				t.Fatalf("the stop = %+v with keys %v; want the row %q and no fund", got.Message, keys, tc.first+"\n"+reason)
			}
			if asks := eventsOf(r, factoryd.EventChatBudgetSpent); len(asks) != 0 {
				t.Fatalf("a cap's stop asked %+v", asks)
			}
			if answer := chatRead(t, r, ck, chatPath); answer.Budget != nil {
				t.Fatalf("GET /chat offers the fund %+v on a cap's stop", answer.Budget)
			}
		})
	}
}

// The fund a stop suggests carries the chat's daily rate over the last 7
// days, or the month's days so far when fewer, a day at least, to the
// next UTC month, in whole euros and at least 1.
func TestSuggestFund(t *testing.T) {
	for _, tc := range []struct {
		name   string
		spent7 float64
		now    time.Time
		want   float64
	}{
		{"1 a day, 11 days left", 7, time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC), 11},
		{"under 1 euro", 0.5, time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC), 1},
		{"nothing spent", 0, time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC), 1},
		{"the live stop: 5.47 over 7 days, 3 days 1h20 left", 5.47, time.Date(2026, 10, 28, 22, 40, 0, 0, time.UTC), 3},
		{"day 3: 3 over 2 days, 29 days left", 3, time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC), 44},
		{"6 hours in: 2 over a day, 29.75 days left", 2, time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC), 60},
	} {
		if got := suggestFund(tc.spent7, tc.now); got != tc.want {
			t.Errorf("%s: suggestFund(%v, %s) = %v, want %v", tc.name, tc.spent7, tc.now, got, tc.want)
		}
	}
}

// The stop stands while the chat task's budget is not above the budget it
// recorded: the board's cell and GET /chat's fund say so. A fund from the
// board lifts it, the stop row settles as funded, and the stopped turn
// goes on by itself; a later stop is lifted by a raise of the policy's
// monthly budget, which the month's task takes at its next ensure, that
// stop row settling as moved on.
func TestChatStopLiftsOnAFundOrAPolicyRaise(t *testing.T) {
	r, chat, g := newChatRig(t)
	chat.now = time.Now
	ck, s := r.loggedIn(t)
	crumbsChat(t, r)
	g.replies = [][]string{{refuseFor(policy.RefusedBudget, budgetReason)}, {say("Two cards are open.")}, {refuseFor(policy.RefusedBudget, budgetReason)}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"what is open?"}})
	if answer := chatRead(t, r, ck, chatPath); answer.Budget == nil || answer.Messages[1].Settled {
		t.Fatalf("GET /chat after the stop: budget %+v, the stop %+v; want the fund and the stop open", answer.Budget, answer.Messages[1])
	}
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); !strings.Contains(body, `>chat budget spent</span>`) {
		t.Fatal("the board lacks the stopped chat's cell")
	}
	rec := r.do(post("/projects/ai-software-factory-code/chat/fund", url.Values{"csrf": {s.csrf}, "eur": {"3"}, "reason": {"the month is young"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("fund = %d: %s", rec.Code, rec.Body)
	}
	if !waitFor(t, 2*time.Second, func() bool {
		rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 1, 1)
		return len(rows) == 1 && rows[0].Content == "Two cards are open."
	}) {
		t.Fatal("the stopped turn did not go on after the fund")
	}
	funds := eventsOf(r, factoryd.EventChatFunded)
	answer := chatRead(t, r, ck, chatPath)
	if want := "funded by sebastian · " + funds[0].At.UTC().Format("15:04"); answer.Budget != nil || !answer.Messages[1].Settled || answer.Messages[1].Reason != want {
		t.Fatalf("GET /chat after the fund: budget %+v, the stop %+v; want no fund and the stop settled %q", answer.Budget, answer.Messages[1], want)
	}
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); strings.Contains(body, `>chat budget spent</span>`) {
		t.Fatal("the board keeps the stopped chat's cell after the fund")
	}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"and the cache?"}})
	if asks := eventsOf(r, factoryd.EventChatBudgetSpent); len(asks) != 2 || asks[1].Detail["budget_eur"] != 18.0 {
		t.Fatalf("the stops = %+v, want a second one at the funded budget of 18", asks)
	}
	if answer := chatRead(t, r, ck, chatPath); answer.Budget == nil {
		t.Fatal("GET /chat offers no fund after the second stop")
	}
	if _, err := chat.EnsureChatTask(t.Context(), "ai-software-factory-code", "sf/code", 30, time.Now()); err != nil {
		t.Fatal(err)
	}
	answer = chatRead(t, r, ck, chatPath)
	last := answer.Messages[len(answer.Messages)-1]
	if answer.Budget != nil || !last.Settled || last.Reason != "moved on" {
		t.Fatalf("GET /chat after the policy's raise to 30: budget %+v, the stop %+v; want no fund and the stop moved on", answer.Budget, last)
	}
}

// Fund the chat raises the budget by the stated amount and lands as the
// raise's one event row, whether the chat stands stopped, its cell and
// form on the board, or still has money left: raising before the money
// runs out is allowed.
func TestChatFundRaisesTheChatBudget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		chat    func(*testing.T, *rig) factoryd.Task
		stopped bool
	}{
		{"stopped", stoppedChat, true},
		{"with money left", crumbsChat, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			ck, s := r.loggedIn(t)
			chat := tc.chat(t, r)
			body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
			for _, want := range []string{`>chat budget spent</span>`, `action="/projects/ai-software-factory-code/chat/fund"`,
				`name="eur"`, `name="reason"`, "Fund the chat", "the chat's monthly budget is spent"} {
				if strings.Contains(body, want) != tc.stopped {
					t.Errorf("the board holds %s: %v, want %v", want, !tc.stopped, tc.stopped)
				}
			}
			rec := r.do(post("/projects/ai-software-factory-code/chat/fund",
				url.Values{"csrf": {s.csrf}, "eur": {"1.5"}, "reason": {"the month is young"}}), ck)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("fund = %d: %s", rec.Code, rec.Body)
			}
			task, _, _ := r.tasks.Task(t.Context(), chat.ID)
			if task.BudgetEUR != 16.5 || task.SpentEUR != 14.96 {
				t.Fatalf("budget_eur = %v, spent_eur = %v; want 16.5 (15 raised by 1.5) with the spend kept", task.BudgetEUR, task.SpentEUR)
			}
			rows := eventsOf(r, factoryd.EventChatFunded)
			if len(rows) != 1 || rows[0].TaskID != chat.ID || rows[0].Repository != "sf/code" || rows[0].Actor != "sebastian" ||
				rows[0].Via != factoryd.ViaButton || !reflect.DeepEqual(rows[0].Detail,
				map[string]any{"from_eur": 15.0, "to_eur": 16.5, "amount_eur": 1.5, "reason": "the month is young"}) {
				t.Fatalf("the raise's event rows: %+v", rows)
			}
			if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); strings.Contains(body, `>chat budget spent</span>`) {
				t.Error("the board keeps the stopped chat's cell after the fund")
			}
		})
	}
}

// What is no raise is refused: a month with no chat task yet, an amount
// that is no EUR above zero, and a reason left empty; none moves a budget
// or writes a fund's event row.
func TestChatFundRefusesWhatIsNoRaise(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	chat := stoppedChat(t, r)
	for _, tt := range []struct {
		name   string
		form   url.Values
		code   int
		answer string
	}{
		{"no amount", url.Values{"csrf": {s.csrf}, "eur": {"0"}, "reason": {"no"}}, http.StatusBadRequest,
			"an amount in EUR above zero is required"},
		{"no reason", url.Values{"csrf": {s.csrf}, "eur": {"1.5"}, "reason": {""}}, http.StatusBadRequest,
			"an answer of 1 to"},
		{"no chat task", url.Values{"csrf": {s.csrf}, "eur": {"1.5"}, "reason": {"no"}}, http.StatusConflict,
			"the project's chat has spent no budget: its task starts with the chat's first turn of the month"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "no chat task" {
				r.tasks.set("")
			}
			rec := r.do(post("/projects/ai-software-factory-code/chat/fund", tt.form), ck)
			if rec.Code != tt.code || !strings.Contains(rec.Body.String(), tt.answer) {
				t.Fatalf("fund = %d: %s, want %d with %q", rec.Code, rec.Body, tt.code, tt.answer)
			}
			if task, ok, _ := r.tasks.Task(t.Context(), chat.ID); ok && task.BudgetEUR != 15 {
				t.Fatalf("a refused raise moved budget_eur to %v", task.BudgetEUR)
			}
			if rows := eventsOf(r, factoryd.EventChatFunded); len(rows) != 0 {
				t.Fatalf("a refused raise wrote %+v", rows)
			}
		})
	}
}

// Funding the chat is an approver's action, from the board and from the
// chat alike: a plain writer of the project is refused with the role's
// words, raises nothing and writes no row; their board carries the
// stopped cell but no form, and their chat the line of who may fund.
func TestChatFundIsAnApproversAction(t *testing.T) {
	r, _, _ := newChatRig(t)
	r.writers.users = []string{"sebastian", "morgan"}
	r.repoWriters["sf/code"] = []string{"sebastian", "morgan"}
	ck, s := memberLogin(t, r, "morgan")
	chat := stoppedChat(t, r)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if !strings.Contains(body, `>chat budget spent</span>`) || strings.Contains(body, "chat/fund") {
		t.Error("a plain writer sees a Fund the chat form, or not the stopped cell")
	}
	if answer := chatRead(t, r, ck, chatPath); answer.Budget == nil || answer.Budget.CanFund {
		t.Errorf("a plain writer's fund state = %+v, want the stop without the right to fund", answer.Budget)
	}
	for _, path := range []string{"/projects/ai-software-factory-code/chat/fund", "/projects/ai-software-factory-code/chat/budget"} {
		rec := r.do(post(path, url.Values{"csrf": {s.csrf}, "eur": {"1.5"}, "reason": {"go on"}}), ck)
		if rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != "Only the approvers of project ai-software-factory-code may fund the chat." {
			t.Fatalf("POST %s = %d: %s", path, rec.Code, rec.Body)
		}
	}
	if task, _, _ := r.tasks.Task(t.Context(), chat.ID); task.BudgetEUR != 15 {
		t.Fatalf("a refused raise moved budget_eur to %v", task.BudgetEUR)
	}
	if rows := eventsOf(r, factoryd.EventChatFunded); len(rows) != 0 {
		t.Fatalf("a refused raise wrote %+v", rows)
	}
}

// Fund and go on in a stopped thread raises the chat's budget, streams
// the raise's line and goes on with that thread's turn, whose context
// leaves the stop row out and ends on the message it stopped at. Every
// other stopped thread goes on by itself; a thread whose newest row is no
// stop stays as it was. Pressed where no turn stopped, it raises and ends.
func TestChatFundAndGoResumesTheStoppedTurns(t *testing.T) {
	r, chat, g := newChatRig(t)
	chat.now = time.Now
	ck, s := r.loggedIn(t)
	crumbsChat(t, r)
	g.replies = [][]string{{say("Hello.")}, {refuseFor(policy.RefusedBudget, budgetReason)}, {refuseFor(policy.RefusedBudget, budgetReason)},
		{say("Going on.")}, {say("Going on.")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"hello"}})
	for _, message := range []string{"what is open?", "and the cache?"} {
		if _, err := chat.NewConversation(t.Context(), "sf/code", "sebastian"); err != nil {
			t.Fatal(err)
		}
		chatPost(t, r, ck, s, chatPath, url.Values{"message": {message}})
	}
	_, events, _ := chatPost(t, r, ck, s, chatPath+"/budget", url.Values{"eur": {"3"}, "conversation": {"3"}})
	equal(t, "Fund and go on", events, []string{`applied {"result":"Raised the chat's budget from €15.00 to €18.00.","tool":"fund the chat"}`,
		`text {"text":"Going on."}`, `done {}`})
	if !waitFor(t, 2*time.Second, func() bool {
		rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 2, 1)
		return len(rows) == 1 && rows[0].Content == "Going on."
	}) {
		t.Fatal("the other stopped thread did not go on")
	}
	if rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 1, 1); len(rows) != 1 || rows[0].Content != "Hello." {
		t.Fatalf("the thread with no stop = %+v, want it ending on its answer", rows)
	}
	calls := g.calls()
	if len(calls) != 5 {
		t.Fatalf("%d model calls, want 5: the answer, the two refused and the two turns going on", len(calls))
	}
	var resumed []string
	for _, call := range calls[3:] {
		msgs := messages(t, call)
		for _, m := range msgs {
			if content, _ := m["content"].(string); strings.Contains(content, "is used up") {
				t.Errorf("a resumed turn reads the stop row: %q", content)
			}
		}
		resumed = append(resumed, msgs[len(msgs)-1]["content"].(string))
	}
	if slices.Sort(resumed); !slices.Equal(resumed, []string{"and the cache?", "what is open?"}) {
		t.Fatalf("the resumed turns end on %q, want each on its stopped message", resumed)
	}
	funds := eventsOf(r, factoryd.EventChatFunded)
	if len(funds) != 1 || funds[0].Actor != "sebastian" || funds[0].Via != factoryd.ViaChat || funds[0].Detail["reason"] != "funded in the chat" ||
		funds[0].Detail["from_eur"] != 15.0 || funds[0].Detail["to_eur"] != 18.0 || funds[0].Detail["amount_eur"] != 3.0 {
		t.Fatalf("the fund's event rows = %+v", funds)
	}
	_, events, _ = chatPost(t, r, ck, s, chatPath+"/budget", url.Values{"eur": {"1"}, "conversation": {"1"}})
	equal(t, "a fund where no turn stopped", events, []string{`applied {"result":"Raised the chat's budget from €18.00 to €19.00.","tool":"fund the chat"}`, `done {}`})
	if n := len(g.calls()); n != 5 {
		t.Fatalf("%d model calls after a fund where no turn stopped, want 5", n)
	}
}
