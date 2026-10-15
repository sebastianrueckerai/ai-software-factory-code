package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// Roadmap 210: a card that outlives its budget has an operator path — an
// approver's Fund action on the capped card (the same cards ruling 322's
// unfunded answer refuses), which raises budget_eur by the stated amount
// and lands in the event spine as its own row. The answer box is
// unchanged: the card resumes on the next answer, now with headroom.

// budgetRefusal is what the floor answers for a card whose remaining
// budget cannot fund a call (factoryd's Runner.SessionFunded).
const budgetRefusal = "the remaining budget cannot fund a call: 0.170 EUR of the 2.000 EUR budget remains, and the cheapest call needs more. Raise the budget (Fund on the board) and answer, or close the card."

// cappedCard is a waiting card of sf/code whose remaining budget cannot
// fund a call, with both floors refusing it.
func cappedCard(r *rig, t *testing.T) {
	t.Helper()
	r.tasks.set(budgetRefusal, factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human",
		Repository: "sf/code", IssueNumber: 13, Objective: "Use a replica?", BudgetEUR: 2, ModelAlias: "factory/coding-cheap"})
	r.c.cfg.SessionFunded = func(context.Context, string, string) (string, error) { return budgetRefusal, nil }
}

// Card #704 (roadmap 243): its remaining budget cannot fund a session's
// opening calls, so the answer box stays and the Fund action is offered
// beside it, and a raise goes through.
func TestFundOpensWhereASessionCannotStart(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	cappedCard(r, t)
	var asked string
	r.c.cfg.SessionFunded = func(_ context.Context, taskID, model string) (string, error) {
		asked = taskID + " " + model
		return budgetRefusal, nil
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{`action="/tasks/t-e/answer"`, `action="/tasks/t-e/fund"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the card's escalation lacks %s", want)
		}
	}
	rec := r.do(post("/tasks/t-e/fund", url.Values{"csrf": {s.csrf}, "eur": {"0.5"}, "reason": {"a session cannot start"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("fund = %d: %s", rec.Code, rec.Body)
	}
	if task, _, _ := r.tasks.Task(t.Context(), "t-e"); task.BudgetEUR != 2.5 || asked != "t-e factory/coding-cheap" {
		t.Fatalf("budget_eur = %v, the fund's floor read %q; want 2.5 on t-e factory/coding-cheap", task.BudgetEUR, asked)
	}
}

// An approver's raise increases budget_eur by exactly the stated amount,
// and the board offers the Fund action beside the answer box only on the
// capped card.
func TestFundRaisesACappedCardsBudget(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	cappedCard(r, t)
	// the board offers the Fund action beside the answer box, and the
	// answer box itself is unchanged
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{`action="/tasks/t-e/answer"`, `action="/tasks/t-e/fund"`, `name="eur"`, `name="reason"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the capped card's escalation lacks %s", want)
		}
	}
	rec := r.do(post("/tasks/t-e/fund", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"eur": {"0.5"}, "reason": {"the card's work is worth it"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("fund = %d: %s", rec.Code, rec.Body)
	}
	task, _, _ := r.tasks.Task(t.Context(), "t-e")
	if task.BudgetEUR != 2.5 {
		t.Fatalf("budget_eur = %v, want 2.5 (2 raised by 0.5)", task.BudgetEUR)
	}
	if task.Status != "waiting-human" || task.SpentEUR != 0 {
		t.Fatalf("the raise moved the card: %+v", task)
	}
}

// A card whose model cannot be priced is refused by the floor too, but no
// raise lets it start: the board offers no Fund, and a raise is refused
// with the floor's reason, the budget unchanged (roadmap 249).
func TestFundIsNotOfferedWhereNoBudgetHelps(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	cappedCard(r, t)
	const unpriced = "the attempt's floor on factory/coding-cheap cannot be read: no price for coding-cheap"
	r.c.cfg.SessionFunded = func(context.Context, string, string) (string, error) { return unpriced, nil }
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if !strings.Contains(body, `action="/tasks/t-e/answer"`) || strings.Contains(body, `action="/tasks/t-e/fund"`) {
		t.Error("the board offers Fund for a card no budget lets start, or lost its answer box")
	}
	rec := r.do(post("/tasks/t-e/fund", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"eur": {"0.5"}, "reason": {"try more"}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no budget lets it start") || !strings.Contains(rec.Body.String(), unpriced) {
		t.Fatalf("fund = %d: %s, want 409 naming the floor's reason", rec.Code, rec.Body)
	}
	if task, _, _ := r.tasks.Task(t.Context(), "t-e"); task.BudgetEUR != 2 {
		t.Fatalf("budget_eur = %v, want 2 unchanged", task.BudgetEUR)
	}
}

// A plain member, a writer of the project, gets the action refused and
// raises nothing.
func TestFundIsAnApproversAction(t *testing.T) {
	r := newRig(t)
	r.writers.users = []string{"sebastian", "morgan"}
	ck, s := memberLogin(t, r, "morgan")
	cappedCard(r, t)
	rec := r.do(post("/tasks/t-e/fund", url.Values{"csrf": {s.csrf}, "eur": {"0.5"}, "reason": {"go on"}}), ck)
	if rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != "Only the approvers of project ai-software-factory-code may fund a card." {
		t.Fatalf("fund = %d: %s", rec.Code, rec.Body)
	}
	task, _, _ := r.tasks.Task(t.Context(), "t-e")
	if task.BudgetEUR != 2 {
		t.Fatalf("a refused raise moved budget_eur to %v", task.BudgetEUR)
	}
	if len(r.events.rows) != 0 {
		t.Fatalf("a refused raise wrote %d event rows", len(r.events.rows))
	}
}

// A raise on a card that is not capped is refused: one whose remaining
// budget can fund a session's opening calls, and one the floor cannot judge
// (no session yet).
func TestFundRefusesACardThatIsNotCapped(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set(budgetRefusal,
		factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code",
			IssueNumber: 13, Objective: "Use a replica?", BudgetEUR: 2, ModelAlias: "factory/coding-cheap"},
		factoryd.Task{ID: "t-new", Kind: "agent", Status: "waiting-human", Repository: "sf/code",
			IssueNumber: 15, Objective: "Start.", BudgetEUR: 2})
	r.c.cfg.SessionFunded = func(context.Context, string, string) (string, error) { return "", nil }
	for _, id := range []string{"t-e", "t-new"} {
		rec := r.do(post("/tasks/"+id+"/fund", url.Values{"csrf": {s.csrf}, "eur": {"0.5"}, "reason": {"go on"}}), ck)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "can fund a session's opening calls") {
			t.Fatalf("fund of %s = %d: %s", id, rec.Code, rec.Body)
		}
		task, _, _ := r.tasks.Task(t.Context(), id)
		if task.BudgetEUR != 2 {
			t.Fatalf("%s's budget_eur = %v, want the refused raise to leave 2", id, task.BudgetEUR)
		}
	}
	// the board offers no Fund action on either card
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if strings.Contains(body, "/fund") {
		t.Error("the board offers a Fund action on a card that is not capped")
	}
}

// The raise's audit row carries the actor, the card, the old and the new
// budget and the stated reason.
func TestFundWritesTheAuditRow(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	cappedCard(r, t)
	rec := r.do(post("/tasks/t-e/fund", url.Values{"csrf": {s.csrf}, "eur": {"1.25"},
		"reason": {"the planner's next request is worth it"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("fund = %d: %s", rec.Code, rec.Body)
	}
	rows := r.events.rows
	if len(rows) != 2 || rows[1].Kind != factoryd.EventQuestionAnswered {
		t.Fatalf("the raise wrote %d event rows, want the raise and the answer it carries", len(rows))
	}
	e := rows[0]
	switch {
	case e.Kind != factoryd.EventCardFunded:
		t.Errorf("kind = %s, want %s", e.Kind, factoryd.EventCardFunded)
	case e.Actor != "sebastian" || e.Via != factoryd.ViaButton:
		t.Errorf("actor %s via %s, want sebastian via %s", e.Actor, e.Via, factoryd.ViaButton)
	case e.TaskID != "t-e" || e.IssueNumber != 13 || e.Repository != "sf/code":
		t.Errorf("the row names task %s issue %d of %s, want card t-e #13 of sf/code", e.TaskID, e.IssueNumber, e.Repository)
	case e.Detail["from_eur"] != 2.0 || e.Detail["to_eur"] != 3.25 || e.Detail["amount_eur"] != 1.25:
		t.Errorf("the row's budgets are %v (from), %v (to), %v (amount), want 2, 3.25, 1.25",
			e.Detail["from_eur"], e.Detail["to_eur"], e.Detail["amount_eur"])
	case e.Detail["reason"] != "the planner's next request is worth it":
		t.Errorf("the row's reason is %v", e.Detail["reason"])
	}
}

// The raise is refused when the card is not waiting, and a bad amount or
// an empty reason never reaches the store.
func TestFundRefusesACardThatIsNotWaiting(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-r", Kind: "agent", Status: "running", Repository: "sf/code",
		IssueNumber: 14, Objective: "Speed up the board.", BudgetEUR: 2, ModelAlias: "factory/coding-cheap"})
	r.c.cfg.SessionFunded = func(context.Context, string, string) (string, error) { return budgetRefusal, nil }
	rec := r.do(post("/tasks/t-r/fund", url.Values{"csrf": {s.csrf}, "eur": {"0.5"}, "reason": {"go on"}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Card #14 runs an agent session, not a card waiting on a human") {
		t.Fatalf("fund = %d: %s", rec.Code, rec.Body)
	}
	task, _, _ := r.tasks.Task(t.Context(), "t-r")
	if task.BudgetEUR != 2 {
		t.Fatalf("a refused raise moved budget_eur to %v", task.BudgetEUR)
	}
	if len(r.events.rows) != 0 {
		t.Fatalf("a refused raise wrote %d event rows", len(r.events.rows))
	}
	if rec := r.do(post("/tasks/t-missing/fund", url.Values{"csrf": {s.csrf}, "eur": {"0.5"}, "reason": {"x"}}), ck); rec.Code != http.StatusNotFound {
		t.Fatalf("fund of no card = %d: %s", rec.Code, rec.Body)
	}
	for _, form := range []url.Values{
		{"csrf": {s.csrf}, "eur": {"0.5"}, "reason": {" "}},
		{"csrf": {s.csrf}, "eur": {"half"}, "reason": {"go on"}},
		{"csrf": {s.csrf}, "eur": {"0"}, "reason": {"go on"}},
	} {
		if rec := r.do(post("/tasks/t-r/fund", form), ck); rec.Code != http.StatusBadRequest {
			t.Fatalf("fund with %v = %d: %s", form, rec.Code, rec.Body)
		}
	}
}

// Fund takes a note and resumes the card in one step (plan A): the note is
// posted on the issue as the member's answer, so a fund is the answer and no
// second form is owed. A note over the answer bound is refused before the budget moves.
func TestFundIsTheAnswer(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	cappedCard(r, t)
	rec := r.do(post("/tasks/t-e/fund", url.Values{"csrf": {s.csrf}, "eur": {"0.5"}, "reason": {"funded; the fix is in store.go"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("fund = %d: %s", rec.Code, rec.Body)
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; got != "funded; the fix is in store.go" {
		t.Fatalf("issue comment = %q, want the note as the answer", got)
	}
	if rows := r.events.rows; len(rows) != 2 || rows[0].Kind != factoryd.EventCardFunded || rows[1].Kind != factoryd.EventQuestionAnswered {
		t.Fatalf("event rows = %+v, want card_funded then question_answered", rows)
	}

	r = newRig(t)
	ck, s = r.loggedIn(t)
	cappedCard(r, t)
	rec = r.do(post("/tasks/t-e/fund", url.Values{"csrf": {s.csrf}, "eur": {"0.5"}, "reason": {strings.Repeat("x", factoryd.MaxAnswer+1)}}), ck)
	if task, _, _ := r.tasks.Task(t.Context(), "t-e"); rec.Code != http.StatusBadRequest || task.BudgetEUR != 2 || len(r.fg.comments) != 0 {
		t.Fatalf("fund with a long note = %d, budget %v, %d comments; want 400, 2, none", rec.Code, task.BudgetEUR, len(r.fg.comments))
	}
}
