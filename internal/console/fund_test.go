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
// approver's Fund action on the capped card (the same cards the
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
// beside it, and a press is a fund through Act on the card's case, the
// human's on rung 3 with its note the answer.
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
	for _, want := range []string{`action="/tasks/t-e/answer"`, `action="/tasks/t-e/fund"`, `name="eur"`, `name="reason"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the card's escalation lacks %s", want)
		}
	}
	if asked != "t-e factory/small" {
		t.Errorf("the board's floor read %q, want t-e factory/small", asked)
	}
	rec := r.do(htmxPost("/tasks/t-e/fund", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"eur": {"0.5"}, "reason": {"a session cannot start"}}), ck)
	if !pressRefreshes(rec) {
		t.Fatalf("fund by htmx = %d (%q): %s", rec.Code, rec.Header().Get("HX-Trigger"), rec.Body)
	}
	got, posts := actedOnce(t, r)
	equal(t, "the fund", []any{got, posts}, []any{fundOf(0.5, "a session cannot start", 3, factoryd.ViaButton), true})
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; got != "a session cannot start" {
		t.Fatalf("issue comment = %q, want the note as the answer", got)
	}
}

// A card whose model cannot be priced is refused by the floor too, but no
// raise lets it start: the board offers no Fund (roadmap 249), nor on a
// card whose budget can fund a session's opening calls.
func TestFundIsNotOfferedWhereNoBudgetHelps(t *testing.T) {
	for _, floor := range []string{"the attempt's floor on factory/small cannot be read: no price for small", ""} {
		r := newRig(t)
		ck, _ := r.loggedIn(t)
		cappedCard(r, t)
		r.c.cfg.SessionFunded = func(context.Context, string, string) (string, error) { return floor, nil }
		body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
		if !strings.Contains(body, `action="/tasks/t-e/answer"`) || strings.Contains(body, `action="/tasks/t-e/fund"`) {
			t.Errorf("floor %q: the board offers Fund where no raise helps, or lost its answer box", floor)
		}
	}
}

// A fund on a card that waits on no case, as a running one, draws on no
// case; a bad amount or an empty reason never reaches Act, and no card is
// 404.
func TestFundOnARunningCardDrawsOnNoCase(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-r", Kind: "agent", Status: "running", Repository: "sf/code",
		IssueNumber: 14, Objective: "Speed up the board.", BudgetEUR: 2, ModelAlias: "factory/small"})
	if rec := r.do(post("/tasks/t-r/fund", url.Values{"csrf": {s.csrf}, "eur": {"0.5"}, "reason": {"go on"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("fund = %d: %s", rec.Code, rec.Body)
	}
	got, _ := actedOnce(t, r)
	if got.TaskID != "t-r" || got.CaseID != 0 || got.Res.Action != factoryd.ActFund {
		t.Fatalf("Act took %+v, want a fund of t-r on no case", got)
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
	if len(r.acted) != 1 {
		t.Fatalf("the refused funds reached Act: %+v", r.acted)
	}
}

// Fund takes a note and resumes the card in one step (plan A): Act has the
// note posted on the issue as the human's answer, so a fund is the answer
// and no second form is owed. A note over the answer bound is refused
// before Act.
func TestFundIsTheAnswer(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	cappedCard(r, t)
	// the note is bounded on the board as the answer box is
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); !strings.Contains(body, `name="reason" maxlength="4000" required hx-preserve>`) {
		t.Errorf("the fund note is not bounded at the answer's 4000 bytes:\n%s", body)
	}
	rec := r.do(post("/tasks/t-e/fund", url.Values{"csrf": {s.csrf}, "eur": {"0.5"}, "reason": {"funded; the fix is in store.go"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("fund = %d: %s", rec.Code, rec.Body)
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; got != "funded; the fix is in store.go" {
		t.Fatalf("issue comment = %q, want the note as the answer", got)
	}

	r = newRig(t)
	ck, s = r.loggedIn(t)
	cappedCard(r, t)
	rec = r.do(post("/tasks/t-e/fund", url.Values{"csrf": {s.csrf}, "eur": {"0.5"}, "reason": {strings.Repeat("x", factoryd.MaxAnswer+1)}}), ck)
	if rec.Code != http.StatusBadRequest || len(r.acted) != 0 || len(r.fg.comments) != 0 {
		t.Fatalf("fund with a long note = %d, %d Act calls, %d comments; want 400, none, none", rec.Code, len(r.acted), len(r.fg.comments))
	}
}

// Forgejo refusing the note answers 502 naming the post: Act stops there,
// before its case ends and its draw is written.
func TestFundWhoseNoteForgejoRefusesAnswers502(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	cappedCard(r, t)
	r.fg.failAPI = true
	rec := r.do(post("/tasks/t-e/fund", url.Values{"csrf": {s.csrf}, "eur": {"0.5"}, "reason": {"funded"}}), ck)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "Forgejo refused to post the answer") {
		t.Fatalf("fund = %d: %s, want 502 naming the post", rec.Code, rec.Body)
	}
	if len(r.events.rows) != 0 {
		t.Fatalf("event rows = %+v, want none", r.events.rows)
	}
}

// The money an unwritten brief names nets the gateway's holds: 4.50
// settled and 0.40 held of 5.00 leaves 0.10.
func TestBriefShortNetsHolds(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 8, PRNumber: 10,
		BudgetEUR: 5, SpentEUR: 4.5, HeldEUR: 0.4})
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 6, Kind: factoryd.ApprovalMerge, TaskID: "t-m", Project: "sf/code",
		PRNumber: 10, Status: "pending", BriefError: "refused_budget: free headroom 0.100000 EUR fits 30 output tokens",
		Spec: json.RawMessage(`{"risk_class":"normal"}`)})
	want := "The card has 0.10 EUR of its 5.00 EUR budget left, too little for the call that writes its brief."
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); !strings.Contains(body, want) {
		t.Errorf("the merge card lacks %s", want)
	}
}

// A merge card whose own budget refused its brief says so in the brief's
// place, and an approver's Fund there is a fund through Act on no case,
// the merge case aside, which queues the brief again: it neither answers
// nor moves the card.
func TestFundAnswersABriefTheBudgetRefused(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 8, PRNumber: 10,
		BudgetEUR: 2, SpentEUR: 1.99})
	r.openCases = map[string][]factoryd.Case{"t-m": {{ID: 4, TaskID: "t-m", Kind: factoryd.CaseMergeApproval, Status: factoryd.CaseAtHuman, Facts: map[string]any{"approval": 6.0}}}}
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
	if !pressRefreshes(rec) {
		t.Fatalf("fund by htmx = %d: %s", rec.Code, rec.Body)
	}
	got, _ := actedOnce(t, r)
	equal(t, "the fund", got, factoryd.ActInput{TaskID: "t-m", By: "sebastian", Via: factoryd.ViaButton, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActFund, Args: map[string]any{"eur": 0.5}, Note: "for the brief"}})
	if a := r.approvals.get(6); a.BriefError != "" || a.Status != "pending" {
		t.Fatalf("the request after the raise = %+v, want it pending with no brief error", a)
	}
	if len(r.fg.comments) != 0 {
		t.Fatalf("the brief's fund posted %v", r.fg.comments)
	}
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); strings.Contains(body, `action="/tasks/t-m/fund"`) {
		t.Error("the card still asks for the brief's money after the raise")
	}
}
