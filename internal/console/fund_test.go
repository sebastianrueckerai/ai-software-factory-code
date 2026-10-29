package console

import (
	"context"
	"encoding/json"
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
// and lands in the event spine as its own row. The answer box stays
// beside it: Fund posts its note as the answer, so the card resumes at
// once, now with headroom.

// budgetRefusal is what the floor answers for a card whose remaining
// budget cannot fund a call (factoryd's Runner.SessionFunded).
const budgetRefusal = "the remaining budget cannot fund a call: 0.170 EUR of the 2.000 EUR budget remains, and the cheapest call needs more. Raise the budget (Fund on the board) and answer, or close the card."

// cappedCard is a waiting card of sf/code whose remaining budget of 2 EUR
// cannot fund a call, with both floors refusing it until a raise.
func cappedCard(r *rig, t *testing.T) {
	t.Helper()
	r.tasks.set(budgetRefusal, factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human",
		Repository: "sf/code", IssueNumber: 13, Objective: "Use a replica?", BudgetEUR: 2, ModelAlias: "factory/small"})
	r.c.cfg.SessionFunded = func(ctx context.Context, id, _ string) (string, error) { return r.raisedFloor(ctx, id, ""), nil }
}

// raisedFloor is the floor's answer for a card that is funded once its
// budget is raised above 2 EUR: budgetRefusal until then, after it raised.
func (r *rig) raisedFloor(ctx context.Context, id, raised string) string {
	if task, _, _ := r.tasks.Task(ctx, id); task.BudgetEUR > 2 {
		return raised
	}
	return budgetRefusal
}

// Card #704 (roadmap 243): its remaining budget cannot fund a session's
// opening calls, so the answer box stays and the Fund action is offered
// beside it, and a raise goes through.
func TestFundOpensWhereASessionCannotStart(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	cappedCard(r, t)
	var asked string
	r.c.cfg.SessionFunded = func(ctx context.Context, taskID, model string) (string, error) {
		asked = taskID + " " + model
		return r.raisedFloor(ctx, taskID, ""), nil
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
	if task, _, _ := r.tasks.Task(t.Context(), "t-e"); task.BudgetEUR != 2.5 || asked != "t-e factory/small" {
		t.Fatalf("budget_eur = %v, the fund's floor read %q; want 2.5 on t-e factory/small", task.BudgetEUR, asked)
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
	rec := r.do(htmxPost("/tasks/t-e/fund", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"eur": {"0.5"}, "reason": {"the card's work is worth it"}}), ck)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), `<div id="board"`) ||
		!strings.Contains(rec.Body.String(), `<p class="note" role="status">Raised #13&#39;s budget by 0.50 EUR to 2.50 and posted your note as the answer; the card resumes.</p>`) {
		t.Fatalf("fund by htmx = %d: %s", rec.Code, rec.Body)
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
	const unpriced = "the attempt's floor on factory/small cannot be read: no price for small"
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

// A raise on a card that is not capped is refused: one whose remaining
// budget can fund a session's opening calls, and one the floor cannot judge
// (no session yet).
func TestFundRefusesACardThatIsNotCapped(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set(budgetRefusal,
		factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code",
			IssueNumber: 13, Objective: "Use a replica?", BudgetEUR: 2, ModelAlias: "factory/small"},
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
		IssueNumber: 14, Objective: "Speed up the board.", BudgetEUR: 2, ModelAlias: "factory/small"})
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
	// the note is bounded on the board as the answer box is
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); !strings.Contains(body, `name="reason" maxlength="4000" required>`) {
		t.Errorf("the fund note is not bounded at the answer's 4000 bytes:\n%s", body)
	}
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

// A raise too small for a session still raises and still posts the note as
// the answer, and the member hears at once what the floor still refuses,
// its amount named, not on the card's next question: 2 EUR raised by 0.10.
func TestFundTooSmallSaysWhatTheFloorStillRefuses(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	cappedCard(r, t)
	const short = "the remaining budget cannot fund a call: Raise the budget (Fund on the board) and answer, or close the card.\n" +
		"0.270 EUR of the 2.100 EUR budget remains, and a session on factory/small needs 0.300000 EUR"
	r.c.cfg.SessionFunded = func(ctx context.Context, id, _ string) (string, error) { return r.raisedFloor(ctx, id, short), nil }
	rec := r.do(post("/tasks/t-e/fund", url.Values{"csrf": {s.csrf}, "eur": {"0.1"}, "reason": {"funded a little"}}), ck)
	want := "Raised #13's budget by 0.10 EUR to 2.10 and posted your note as the answer, but a session still cannot start: " + short
	if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("fund = %d: %s, want 409 %q", rec.Code, rec.Body, want)
	}
	if task, _, _ := r.tasks.Task(t.Context(), "t-e"); task.BudgetEUR != 2.1 {
		t.Fatalf("budget_eur = %v, want 2.1", task.BudgetEUR)
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; got != "funded a little" {
		t.Fatalf("issue comment = %q, want the note as the answer", got)
	}
	if rows := r.events.rows; len(rows) != 2 || rows[0].Kind != factoryd.EventCardFunded || rows[1].Kind != factoryd.EventQuestionAnswered {
		t.Fatalf("event rows = %+v, want card_funded then question_answered", rows)
	}
}

// Forgejo refusing the note after the raise answers 502: the budget stays
// raised, its one card_funded row written, and the card waits on the human
// still, whom the refusal sends to the board's answer.
func TestFundWhoseNoteForgejoRefusesKeepsTheRaise(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	cappedCard(r, t)
	r.fg.failAPI = true
	rec := r.do(post("/tasks/t-e/fund", url.Values{"csrf": {s.csrf}, "eur": {"0.5"}, "reason": {"funded"}}), ck)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "Raised #13's budget by 0.50 EUR to 2.50, but Forgejo refused the note") {
		t.Fatalf("fund = %d: %s, want 502 naming the raise", rec.Code, rec.Body)
	}
	if task, _, _ := r.tasks.Task(t.Context(), "t-e"); task.BudgetEUR != 2.5 || task.Status != "waiting-human" {
		t.Fatalf("card = %+v, want budget 2.5 and waiting-human", task)
	}
	funded := 0
	for _, e := range r.events.rows {
		if e.Kind == factoryd.EventCardFunded {
			funded++
		}
	}
	if funded != 1 || len(r.events.rows) != 1 {
		t.Fatalf("event rows = %+v, want the one card_funded row alone", r.events.rows)
	}
}

// A merge card whose own budget refused its brief says so in the brief's
// place, and an approver's Fund there raises the budget and queues the
// brief again: it neither answers nor moves the card.
func TestFundAnswersABriefTheBudgetRefused(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 8, PRNumber: 10,
		BudgetEUR: 2, SpentEUR: 1.99})
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 6, Kind: factoryd.ApprovalMerge, TaskID: "t-m", Project: "sf/code",
		PRNumber: 10, Status: "pending", BriefError: "refused_budget: free headroom 0.010000 EUR fits 3 output tokens",
		Spec: json.RawMessage(`{"risk_class":"normal"}`)})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{"The card has 0.01 EUR of its 2.00 EUR budget left, too little for the call that writes its brief.", `action="/tasks/t-m/fund"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the merge card lacks %s", want)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	tool, _ := r.c.chatTool("card")
	text, _, err := tool.run(&turn{c: r.c, r: req.WithContext(context.WithValue(req.Context(), ctxKey{}, s)), a: actor{s: s, repository: "sf/code", via: factoryd.ViaChat},
		p: factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}}, toolInput{TaskID: "t-m"})
	if err != nil || !strings.Contains(text, "merge request 6 pending; its brief is unwritten: the card's budget cannot pay for it, and a fund queues it again") {
		t.Fatalf("the card read = %q, %v; want the unfunded brief", text, err)
	}
	rec := r.do(htmxPost("/tasks/t-m/fund", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"eur": {"0.5"}, "reason": {"for the brief"}}), ck)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<p class="note" role="status">Raised #8&#39;s budget by 0.50 EUR to 2.50; the factory writes the card&#39;s brief within seconds.</p>`) {
		t.Fatalf("fund by htmx = %d: %s", rec.Code, rec.Body)
	}
	if task, _, _ := r.tasks.Task(t.Context(), "t-m"); task.BudgetEUR != 2.5 || task.Status != "in-review" {
		t.Fatalf("the raise left %+v, want budget 2.5 and the card in review", task)
	}
	if a := r.approvals.get(6); a.BriefError != "" || a.Status != "pending" {
		t.Fatalf("the request after the raise = %+v, want it pending with no brief error", a)
	}
	if len(r.events.rows) != 1 || r.events.rows[0].Kind != factoryd.EventCardFunded || r.events.rows[0].Detail["brief"] != int64(6) {
		t.Fatalf("events = %+v, want the one card_funded row naming the brief", r.events.rows)
	}
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); strings.Contains(body, `action="/tasks/t-m/fund"`) {
		t.Error("the card still asks for the brief's money after the raise")
	}
}
