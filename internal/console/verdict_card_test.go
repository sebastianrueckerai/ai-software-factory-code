package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// verdictRig is sf/code's board with a plan whose cards are done and c2
// (human) and c3 (invariant) still open: its pending criteria approval
// asks the grant or a waiver, and the review's rows stand on the
// integration card (c1 met).
func verdictRig(r *rig) {
	plan, _ := json.Marshal(factoryd.Plan{Summary: "The shortener", Criteria: []factoryd.PlanCriterion{
		{ID: "c1", Kind: factoryd.CriterionTest, Test: "go test ./..."},
		{ID: "c2", Kind: factoryd.CriterionHuman, Claim: "the pages read well"},
		{ID: "c3", Kind: factoryd.CriterionInvariant, Test: "no endpoint times out"},
	}, Cards: []factoryd.PlanCard{
		{Key: "foundation", Role: "foundation", Title: "Foundation"},
		{Key: "integration", Role: "integration", Title: "Integration"},
	}})
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "done", Process: "spec", Objective: "Shortener",
			Repository: "sf/code", IssueNumber: 3}},
		{Task: factoryd.Task{ID: "t-found", Kind: "agent", Status: "done", Repository: "sf/code", IssueNumber: 4,
			Parent: "t-plan", Source: factoryd.SourcePlan, PlanKey: "foundation"}},
		{Task: factoryd.Task{ID: "t-int", Kind: "agent", Status: "done", Repository: "sf/code", IssueNumber: 5,
			Parent: "t-plan", Source: factoryd.SourcePlan, PlanKey: "integration"}},
	}
	r.tasks.plans = map[string][]factoryd.PlanRevision{"t-plan": {{TaskID: "t-plan", Revision: 1, Plan: plan, SHA256: "sha-1"}}}
	r.tasks.criteria = map[string][]factoryd.CriterionVerdict{"t-int": {
		{Criterion: "c1", Verdict: "met", Evidence: "go test ./... exits 0, 214 tests", Kind: factoryd.CriterionTest, Source: "review"},
	}}
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 30, Kind: factoryd.ApprovalCriteria, TaskID: "t-plan",
		Project: "sf/code", Status: "pending", Reason: "criteria not met with the plan's cards done: c2, c3",
		Spec: json.RawMessage(`{"revision":0,"task":"t-int"}`)})
}

// Pieces 7 and 8: the criteria gap card shows every criterion with its
// standing verdict and evidence, a waiver form under each one the waiver
// answers (never a met or waived one), and the grant's Approve.
func TestCriteriaGapCardHoldsVerdictsAndForms(t *testing.T) {
	r := newRig(t)
	verdictRig(r)
	ck, _ := r.loggedIn(t)
	card := col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String(), "Needs you")
	for _, want := range []string{
		"Criteria gap",
		"The plan's cards ran out with a criterion still unmet. Approve grants one more bounded card, which the planner drafts; or waive a criterion with your reason.",
		`<fieldset class="question settled"><legend>c1 (test): met</legend>`,
		"go test ./...",
		`<p class="reason">go test ./... exits 0, 214 tests</p>`,
		`<fieldset class="question"><legend>c2 (human): open</legend>`,
		"the pages read well",
		`<legend>c3 (invariant): open</legend>`,
		`<input type="hidden" name="criterion" value="c2">`,
		`<input type="hidden" name="criterion" value="c3">`,
		`name="reason" maxlength="2000" required`,
		`aria-label="Waive criterion c3 of t-plan"`,
		`aria-label="Approve the criteria grant 30 of t-plan"`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("the criteria card lacks %q:\n%s", want, card)
		}
	}
	if n := strings.Count(card, `action="/approvals/30/waive"`); n != 2 {
		t.Errorf("waiver forms = %d, want 2 (the open criteria alone):\n%s", n, card)
	}
}

// The gate's row stands over the review's: c1's failed test is its
// verdict on the card, and the waiver answers it.
func TestCriteriaCardStandsOnTheGatesRowOverTheReviews(t *testing.T) {
	r := newRig(t)
	verdictRig(r)
	r.tasks.criteria["t-int"] = append(r.tasks.criteria["t-int"], factoryd.CriterionVerdict{Criterion: "c1", Verdict: "unmet",
		Evidence: "criterion c1: suite ./... failed: Failed boom", Kind: factoryd.CriterionTest, Source: factoryd.CriteriaSourceGate})
	ck, _ := r.loggedIn(t)
	card := col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String(), "Needs you")
	for _, want := range []string{
		"<legend>c1 (test): unmet</legend>",
		`<p class="reason">criterion c1: suite ./... failed: Failed boom</p>`,
		`<input type="hidden" name="criterion" value="c1">`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("the criteria card lacks %q:\n%s", want, card)
		}
	}
}

// The waiver records the approver's decision on one criterion and the
// board shows it standing; the grant approves the request and hands the
// planner its dues. A waiver of an unknown criterion refuses.
func TestCriteriaGapWaiveAndGrant(t *testing.T) {
	r := newRig(t)
	verdictRig(r)
	var calls []string
	r.c.cfg.DecideCriterion = func(_ context.Context, id, cid, verdict, by, reason string) error {
		if cid != "c2" && cid != "c3" {
			return factoryd.ErrNoCriterion
		}
		calls = append(calls, fmt.Sprintf("%s %s %s by %s: %s", id, cid, verdict, by, reason))
		r.tasks.mu.Lock()
		defer r.tasks.mu.Unlock()
		r.tasks.criteria["t-int"] = append(r.tasks.criteria["t-int"], factoryd.CriterionVerdict{
			Criterion: cid, Verdict: verdict, Evidence: reason, Kind: factoryd.CriterionInvariant, Source: "approver", DecidedBy: by})
		return nil
	}
	r.c.cfg.GrantCriteria = func(_ context.Context, id, by string, approval int64) (int, error) {
		calls = append(calls, fmt.Sprintf("grant %s by %s approval %d", id, by, approval))
		return 1, nil
	}
	ck, s := r.loggedIn(t)
	board := func() string {
		return col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String(), "Needs you")
	}
	rec := r.do(post("/approvals/30/waive", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"criterion": {"c3"}, "reason": {"the timeout is the provider's SLA"}}), ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?project=ai-software-factory-code" {
		t.Fatalf("waive = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if card := board(); !strings.Contains(card, `<fieldset class="question settled"><legend>c3 (invariant): waived</legend>`) ||
		!strings.Contains(card, "the timeout is the provider&#39;s SLA") || !strings.Contains(card, `<legend>c2 (human): open</legend>`) {
		t.Errorf("after the waiver the card misses the standing verdict:\n%s", card)
	}
	if a := r.approvals.get(30); a.Status != "pending" {
		t.Errorf("a waiver decided the request itself: %+v", a)
	}
	rec = r.do(post("/approvals/30/waive", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"criterion": {"c9"}, "reason": {"no such one"}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "The plan holds no criterion c9") {
		t.Fatalf("an unknown criterion's waiver = %d %q", rec.Code, rec.Body.String())
	}
	rec = r.do(post("/approvals/30/approve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve = %d %q", rec.Code, rec.Body.String())
	}
	want := []string{
		"t-plan c3 waived by sebastian: the timeout is the provider's SLA",
		"grant t-plan by sebastian approval 30",
	}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	if a := r.approvals.get(30); a.Status != "approved" || a.DecidedBy != "sebastian" || a.DecisionNote != "granted: one more bounded card" {
		t.Errorf("the grant left the request %+v", a)
	}
}

// A criteria_rerun ask is the criteria card under its own name: it offers
// waive, confirm and deny, and no Approve, since nothing here can pay for
// the review that never judged them; an approve that reaches it refuses
// naming the answers it has.
func TestCriteriaNeverJudgedCardOffersWaiveAndConfirm(t *testing.T) {
	r := newRig(t)
	verdictRig(r)
	r.approvals.rows[len(r.approvals.rows)-1] = factoryd.ApprovalRequest{ID: 30, Kind: factoryd.ApprovalCriteriaRerun, TaskID: "t-plan",
		Project: "sf/code", Status: "pending", Reason: "criteria never judged: the review of card #5 did not run (the review never ran): c2, c3",
		Spec: json.RawMessage(`{"revision":0,"task":"t-int"}`)}
	var calls []string
	r.c.cfg.DecideCriterion = func(_ context.Context, id, cid, verdict, by, reason string) error {
		calls = append(calls, fmt.Sprintf("%s %s %s", id, cid, verdict))
		return nil
	}
	ck, s := r.loggedIn(t)
	card := col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String(), "Needs you")
	for _, want := range []string{
		"Criteria never judged",
		"No review judged these criteria, and no button here runs one: waive a criterion with your reason, or confirm the delivered cards.",
		`<legend>c2 (human): open</legend>`,
		`aria-label="Waive criterion c3 of t-plan"`,
		`aria-label="Deny the criteria never judged 30 of t-plan"`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("the card lacks %q:\n%s", want, card)
		}
	}
	// the column holds the rig's resource request too: the claims are the
	// gap's name and an Approve of this request, which it never carries
	if strings.Contains(card, "Criteria gap") || strings.Contains(card, `action="/approvals/30/approve"`) ||
		strings.Contains(card, "Every criterion is met or waived") {
		t.Errorf("the card reads as the gap or offers an Approve:\n%s", card)
	}
	form := url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}
	if rec := r.do(post("/approvals/30/approve", form), ck); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "waive a criterion with your reason, or confirm the delivered cards") {
		t.Fatalf("approve = %d %q, want the refusal naming the answers", rec.Code, rec.Body.String())
	}
	waive := url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "criterion": {"c3"}, "reason": {"the provider's SLA"}}
	if rec := r.do(post("/approvals/30/waive", waive), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("waive = %d %q", rec.Code, rec.Body.String())
	}
	deny := url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "note": {"not worth the money"}}
	if rec := r.do(post("/approvals/30/deny", deny), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("deny = %d %q", rec.Code, rec.Body.String())
	}
	if a := r.approvals.get(30); a.Status != "denied" {
		t.Errorf("the denial left the request %+v", a)
	}
	if want := []string{"t-plan c3 waived"}; fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
}

// The confirm button is keyed on every child of the plan done or merged
// (spec piece 2): it records the human's confirmation of the delivered
// cards, and a child still open shows no button. The request closes on
// the sweep, not here.
func TestCriteriaGapConfirm(t *testing.T) {
	r := newRig(t)
	verdictRig(r)
	var calls []string
	r.c.cfg.ConfirmDeliveredCriteria = func(_ context.Context, id, by, line string) (int, error) {
		calls = append(calls, fmt.Sprintf("confirm %s by %s: %q", id, by, line))
		return 2, nil
	}
	ck, s := r.loggedIn(t)
	board := func() string {
		return col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String(), "Needs you")
	}
	if card := board(); !strings.Contains(card, `action="/approvals/30/confirm"`) ||
		!strings.Contains(card, `aria-label="Confirm the delivered cards of t-plan"`) {
		t.Errorf("the criteria card lacks the confirm button:\n%s", card)
	}
	rec := r.do(post("/approvals/30/confirm", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?project=ai-software-factory-code" {
		t.Fatalf("confirm = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if want := []string{`confirm t-plan by sebastian: ""`}; fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}

	// a child not done or merged shows no button
	r.tasks.mu.Lock()
	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].ID == "t-int" {
			r.tasks.tasks[i].Status = "queued"
		}
	}
	r.tasks.mu.Unlock()
	if card := board(); strings.Contains(card, `action="/approvals/30/confirm"`) {
		t.Errorf("a plan with a child still open offers the confirm button:\n%s", card)
	}
}

// Fault 9: typed text on the board is draft state — every typed field it
// renders carries the preserve treatment the find's form already carries,
// so one criterion's Waive does not erase its siblings' reasons — and the
// card states its closing rule.
func TestTheBoardKeepsTypedTextAndNamesItsClose(t *testing.T) {
	r := newRig(t)
	verdictRig(r)
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()
	card := col(t, body, "Needs you")
	if !strings.Contains(card, "Every criterion met or waived ends the request on the next sweep; no final press.") {
		t.Errorf("the card does not state its closing rule:\n%s", card)
	}
	typed := regexp.MustCompile(`<input[^>]*type="text"[^>]*>|<textarea[^>]*>`).FindAllString(body, -1)
	if len(typed) < 4 { // the find field, the deny note and the two waiver reasons
		t.Fatalf("the board's typed fields = %v", typed)
	}
	for _, f := range typed {
		// the find field rides its form's preserve; its own mark would nest
		// and the swap would carry the box out of the form
		if strings.Contains(f, `id="q"`) {
			continue
		}
		if !strings.Contains(f, "hx-preserve") {
			t.Errorf("a typed field loses its text on a swap: %s", f)
		}
	}
}

// The grant answers an open criterion: with every criterion met or
// waived, the card offers no Approve, says the request closes, and a
// posted grant decides nothing (fault 9's approved grant with no
// criterion left open).
func TestTheGrantNeedsAnOpenCriterion(t *testing.T) {
	r := newRig(t)
	verdictRig(r)
	r.tasks.mu.Lock()
	r.tasks.criteria["t-int"] = append(r.tasks.criteria["t-int"],
		factoryd.CriterionVerdict{Criterion: "c2", Verdict: "met", Evidence: "read them", Kind: factoryd.CriterionHuman, Source: "review"},
		factoryd.CriterionVerdict{Criterion: "c3", Verdict: "waived", Evidence: "the SLA", Kind: factoryd.CriterionInvariant, Source: "review"})
	r.tasks.mu.Unlock()
	var grants int
	r.c.cfg.GrantCriteria = func(_ context.Context, id, by string, approval int64) (int, error) {
		grants++
		return 0, nil
	}
	ck, s := r.loggedIn(t)
	card := col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String(), "Needs you")
	if strings.Contains(card, `action="/approvals/30/approve"`) {
		t.Errorf("the grant is offered with no criterion open:\n%s", card)
	}
	if !strings.Contains(card, "Every criterion is met or waived; the request closes on the next sweep.") {
		t.Errorf("the card does not say its close:\n%s", card)
	}
	rec := r.do(post("/approvals/30/approve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Cannot grant one more card: every criterion is met or waived already") {
		t.Fatalf("a grant with nothing open = %d %q", rec.Code, rec.Body.String())
	}
	if a := r.approvals.get(30); a.Status != "pending" || grants != 1 {
		t.Errorf("the empty grant decided the request: %+v, grants %d", a, grants)
	}
}

// A waiver of a criterion that stands met refuses, naming the criterion
// and the reload (the stale board, fault 9).
func TestCriteriaGapWaiveOfAMetOneRefuses(t *testing.T) {
	r := newRig(t)
	verdictRig(r)
	var calls int
	r.c.cfg.DecideCriterion = func(_ context.Context, id, cid, verdict, by, reason string) error {
		calls++
		return factoryd.ErrCriterionMet
	}
	ck, s := r.loggedIn(t)
	rec := r.do(post("/approvals/30/waive", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"criterion": {"c1"}, "reason": {"it holds"}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Criterion c1 is met already; reload the board.") {
		t.Fatalf("a waiver of a met criterion = %d %q", rec.Code, rec.Body.String())
	}
	if calls != 1 || r.approvals.get(30).Status != "pending" {
		t.Errorf("the refused waiver reached the store %d times or decided the request", calls)
	}
}
