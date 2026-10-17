package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// spentChat is the project's chat task of this month with its budget of
// 5 EUR spent, as the pulse's month leaves it, on the rig's tasks.
func spentChat(r *rig, t *testing.T) factoryd.Task {
	t.Helper()
	chat := factoryd.Task{ID: factoryd.ChatTaskID("ai-software-factory-code", time.Now()), Kind: factoryd.KindChat,
		Status: "running", Repository: "sf/code", BudgetEUR: 5, SpentEUR: 5}
	r.tasks.set("", chat)
	return chat
}

// A spent chat budget stands the pulse and the chat, and the strip says
// so beside the spend; the approver's Fund the chat raises the budget by
// the stated amount and lands as the raise's one event row.
func TestChatFundRaisesASpentChatBudget(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	chat := spentChat(r, t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{`>chat budget spent</span>`, `action="/projects/ai-software-factory-code/chat/fund"`,
		`name="eur"`, `name="reason"`, "Fund the chat", "the chat's monthly budget is spent"} {
		if !strings.Contains(body, want) {
			t.Errorf("the board lacks %s", want)
		}
	}
	rec := r.do(post("/projects/ai-software-factory-code/chat/fund",
		url.Values{"csrf": {s.csrf}, "eur": {"1.5"}, "reason": {"the month is young"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("fund = %d: %s", rec.Code, rec.Body)
	}
	task, _, _ := r.tasks.Task(t.Context(), chat.ID)
	if task.BudgetEUR != 6.5 || task.SpentEUR != 5 {
		t.Fatalf("budget_eur = %v, spent_eur = %v; want 6.5 (5 raised by 1.5) with the spend kept", task.BudgetEUR, task.SpentEUR)
	}
	var rows []factoryd.Event
	for _, e := range r.events.all() {
		if e.Kind == factoryd.EventChatFunded {
			rows = append(rows, e)
		}
	}
	if len(rows) != 1 || rows[0].TaskID != chat.ID || rows[0].Repository != "sf/code" || rows[0].Actor != "sebastian" ||
		rows[0].Via != factoryd.ViaButton || !reflect.DeepEqual(rows[0].Detail,
		map[string]any{"from_eur": 5.0, "to_eur": 6.5, "amount_eur": 1.5, "reason": "the month is young"}) {
		t.Fatalf("the raise's event rows: %+v", rows)
	}
}

// What needs no raise is refused: a chat whose budget is not spent, a
// month with no chat task yet, an amount that is no EUR above zero, and a
// reason left empty; none moves a budget or writes an event row.
func TestChatFundRefusesWhatNeedsNoRaise(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	chat := spentChat(r, t)
	chat.SpentEUR, chat.BudgetEUR = 4.9, 5
	r.tasks.set("", chat)
	for _, tt := range []struct {
		name   string
		form   url.Values
		code   int
		answer string
	}{
		{"a budget not spent", url.Values{"csrf": {s.csrf}, "eur": {"1.5"}, "reason": {"no"}}, http.StatusConflict,
			"the chat's budget of €5.00 is not spent: it needs no raise"},
		{"no chat task", url.Values{"csrf": {s.csrf}, "eur": {"1.5"}, "reason": {"no"}}, http.StatusConflict,
			"the project's chat has spent no budget: its task starts with the chat's or the pulse's first turn of the month"},
		{"no amount", url.Values{"csrf": {s.csrf}, "eur": {"0"}, "reason": {"no"}}, http.StatusBadRequest,
			"an amount in EUR above zero is required"},
		{"no reason", url.Values{"csrf": {s.csrf}, "eur": {"1.5"}, "reason": {""}}, http.StatusBadRequest,
			"an answer of 1 to"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "no chat task" {
				r.tasks.set("")
			}
			rec := r.do(post("/projects/ai-software-factory-code/chat/fund", tt.form), ck)
			if rec.Code != tt.code || !strings.Contains(rec.Body.String(), tt.answer) {
				t.Fatalf("fund = %d: %s, want %d with %q", rec.Code, rec.Body, tt.code, tt.answer)
			}
			if tt.name == "a budget not spent" {
				id := factoryd.ChatTaskID("ai-software-factory-code", time.Now())
				if task, _, _ := r.tasks.Task(t.Context(), id); task.BudgetEUR != 5 {
					t.Fatalf("a refused raise moved budget_eur to %v", task.BudgetEUR)
				}
			}
			if len(r.events.rows) != 0 {
				t.Fatalf("a refused raise wrote %d event rows", len(r.events.rows))
			}
		})
	}
}

// Fund the chat is an approver's action: a plain writer of the project is
// refused, raises nothing and writes no row, and their board carries the
// spent cell but no form.
func TestChatFundIsAnApproversAction(t *testing.T) {
	r := newRig(t)
	r.writers.users = []string{"sebastian", "morgan"}
	r.repoWriters["sf/code"] = []string{"sebastian", "morgan"}
	ck, s := memberLogin(t, r, "morgan")
	spentChat(r, t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if !strings.Contains(body, `>chat budget spent</span>`) || strings.Contains(body, "chat/fund") {
		t.Error("a plain writer sees no Fund the chat form, or not the spent cell")
	}
	rec := r.do(post("/projects/ai-software-factory-code/chat/fund",
		url.Values{"csrf": {s.csrf}, "eur": {"1.5"}, "reason": {"go on"}}), ck)
	if rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != "Only the approvers of project ai-software-factory-code may fund the chat." {
		t.Fatalf("fund = %d: %s", rec.Code, rec.Body)
	}
	if task, _, _ := r.tasks.Task(t.Context(), factoryd.ChatTaskID("ai-software-factory-code", time.Now())); task.BudgetEUR != 5 {
		t.Fatalf("a refused raise moved budget_eur to %v", task.BudgetEUR)
	}
	if len(r.events.rows) != 0 {
		t.Fatalf("a refused raise wrote %d event rows", len(r.events.rows))
	}
}
