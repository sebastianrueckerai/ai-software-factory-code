package console

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// Roadmap 210: a card that outlives its budget has a human's path: an
// approver's "Add money" on the card, which raises budget_eur by the
// stated amount through Act and lands in the event spine as its own row;
// on the card's case its press is the answer, so the card resumes at once.

// budgetRefusal is what the floor answers for a card whose remaining
// budget cannot fund a call (factoryd's Runner.SessionFunded).
const budgetRefusal = "the remaining budget cannot fund a call: 0.17 EUR of the 2.00 EUR budget remains, and the cheapest call needs more. Raise the budget (Fund on the board) and answer, or close the card."

// cappedCard is a waiting card of sf/code whose remaining budget of 2 EUR
// cannot fund a call.
func cappedCard(r *rig, t *testing.T) {
	t.Helper()
	r.tasks.set(budgetRefusal, factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human",
		Repository: "sf/code", IssueNumber: 13, Objective: "Use a replica?", BudgetEUR: 2, ModelAlias: "factory/small"})
}

// Card #704 (roadmap 243): its remaining budget cannot fund a session's
// opening calls, so its opened card offers "Add money", and a press is a
// fund through Act on the card's case, the human's on rung 3, which posts
// its answer.
func TestFundOpensWhereASessionCannotStart(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	cappedCard(r, t)
	seatAsk(r, "t-e", factoryd.CaseBudgetFloor, roomyFacts(func(f *factoryd.CaseFacts) { f.Attempts, f.LeftEUR = 1, 0.2 }))
	fund := answerForm(t, drawer(t, r, ck, "t-e", "overview"), factoryd.ActFund)
	if fund.Action != "/tasks/t-e/resolve" || fund.Label != "Add money" || !fund.Values.Has("fund.eur") {
		t.Fatalf("the card's fund is %+v, want Add money with its amount on the case's resolve", fund)
	}
	fund.Values.Set("fund.eur", "0.5")
	rec := r.do(htmxPost(fund.Action, fund.Values), ck)
	if !pressRefreshes(rec) {
		t.Fatalf("fund by htmx = %d (%q): %s", rec.Code, rec.Header().Get("HX-Trigger"), rec.Body)
	}
	got, posts := actedOnce(t, r)
	equal(t, "the fund", []any{got, posts}, []any{fundOf(0.5, "", 3, factoryd.ViaButton), true})
	if got, want := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"], "“Add money”: 0.50 EUR from the pool"; got != want {
		t.Fatalf("the fund posted %q on the issue, want the answer in its words %q", got, want)
	}
}

// A fund on a card that waits on no case, as a running one, draws on no
// case; a bad amount never reaches Act, and no card is 404.
func TestFundOnARunningCardDrawsOnNoCase(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-r", Kind: "agent", Status: "running", Repository: "sf/code",
		IssueNumber: 14, Objective: "Speed up the board.", BudgetEUR: 2, ModelAlias: "factory/small"})
	fund := answerForm(t, drawer(t, r, ck, "t-r", "overview"), factoryd.ActFund)
	fund.Values.Set("fund.eur", "0.5")
	if rec := r.do(post(fund.Action, fund.Values), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("fund = %d: %s", rec.Code, rec.Body)
	}
	got, _ := actedOnce(t, r)
	if got.TaskID != "t-r" || got.CaseID != 0 || got.Res.Action != factoryd.ActFund || got.Res.Args["eur"] != 0.5 {
		t.Fatalf("Act took %+v, want a fund of t-r on no case", got)
	}
	if rec := r.do(post("/tasks/t-missing/resolve", url.Values{"csrf": {s.csrf}, "action": {factoryd.ActFund}, "fund.eur": {"0.5"}}), ck); rec.Code != http.StatusNotFound {
		t.Fatalf("fund of no card = %d: %s", rec.Code, rec.Body)
	}
	for _, eur := range []string{"half", "0"} {
		if rec := r.do(post("/tasks/t-r/resolve", url.Values{"csrf": {s.csrf}, "action": {factoryd.ActFund}, "fund.eur": {eur}}), ck); rec.Code != http.StatusBadRequest {
			t.Fatalf("fund of %q = %d: %s", eur, rec.Code, rec.Body)
		}
	}
	if len(r.acted) != 1 {
		t.Fatalf("the refused funds reached Act: %+v", r.acted)
	}
}

// Forgejo refusing the answer's post answers 502 naming the post: Act
// stops there, before its case ends and its draw is written.
func TestFundWhoseNoteForgejoRefusesAnswers502(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	cappedCard(r, t)
	seatAsk(r, "t-e", factoryd.CaseBudgetFloor, roomyFacts(func(f *factoryd.CaseFacts) { f.Attempts, f.LeftEUR = 1, 0.2 }))
	r.fg.failAPI = true
	rec := r.do(post("/tasks/t-e/resolve", url.Values{"csrf": {s.csrf}, "case_id": {"1"}, "action": {factoryd.ActFund}, "fund.eur": {"0.5"}}), ck)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "Forgejo refused to post the answer") {
		t.Fatalf("fund = %d: %s, want 502 naming the post", rec.Code, rec.Body)
	}
	if len(r.events.rows) != 0 {
		t.Fatalf("event rows = %+v, want none", r.events.rows)
	}
}

// A fund on a merge card is a fund through Act on no case, the merge case
// aside: it neither answers nor moves the card.
func TestFundOnAMergeCardLeavesItsCaseOpen(t *testing.T) {
	r := newRig(t)
	_, s := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 8, PRNumber: 10,
		BudgetEUR: 2, SpentEUR: 1.99})
	r.openCases = map[string][]factoryd.Case{"t-m": {{ID: 4, TaskID: "t-m", Kind: factoryd.CaseMergeApproval, Status: factoryd.CaseAtHuman, Facts: map[string]any{"approval": 6.0}}}}
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 6, Kind: factoryd.ApprovalMerge, TaskID: "t-m", Project: "sf/code",
		PRNumber: 10, Status: "pending",
		Spec: json.RawMessage(`{"risk_class":"normal"}`)})
	if _, err := r.c.doFund(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaButton}, "t-m", 0.5, "for the brief", 0); err != nil {
		t.Fatalf("fund: %v", err)
	}
	got, _ := actedOnce(t, r)
	equal(t, "the fund", got, factoryd.ActInput{TaskID: "t-m", By: "sebastian", Via: factoryd.ViaButton, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActFund, Args: map[string]any{"eur": 0.5}, Note: "for the brief"}})
	if a := r.approvals.get(6); a.Status != "pending" {
		t.Fatalf("the request after the raise = %+v, want it pending", a)
	}
	if len(r.fg.comments) != 0 {
		t.Fatalf("the fund posted %v", r.fg.comments)
	}
}
