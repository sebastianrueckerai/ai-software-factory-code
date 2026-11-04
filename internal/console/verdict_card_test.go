package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
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
		{ID: "c1", Kind: factoryd.CriterionTest, Claim: "a link is stored", Inputs: "POST /api/links with a url", Expected: "201"},
		{ID: "c2", Kind: factoryd.CriterionHuman, Claim: "the pages read well"},
		{ID: "c3", Kind: factoryd.CriterionInvariant, Suite: "./internal/server"},
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
		`<p class="meta">a link is stored</p>`,
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
	if n := strings.Count(card, `action="/approvals/30/criterion"`); n != 2 || strings.Count(card, `<button type="submit" class="deny" name="verb" value="waived"`) != 2 {
		t.Errorf("waiver forms = %d, want 2 (the open criteria alone):\n%s", n, card)
	}
}

// A criterion's line on the card is its claim, or an invariant's suite.
func TestCriteriaCardShowsTheClaimOrTheSuite(t *testing.T) {
	r := newRig(t)
	verdictRig(r)
	plan, _ := json.Marshal(factoryd.Plan{Summary: "The shortener", Criteria: []factoryd.PlanCriterion{
		{ID: "c1", Kind: factoryd.CriterionInvariant, Suite: "./..."},
		{ID: "c2", Kind: factoryd.CriterionTest, Claim: "a slug redirects"},
	}, Cards: []factoryd.PlanCard{
		{Key: "foundation", Role: "foundation", Title: "Foundation"},
		{Key: "integration", Role: "integration", Title: "Integration"},
	}})
	r.tasks.plans["t-plan"][0].Plan = plan
	ck, _ := r.loggedIn(t)
	card := col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String(), "Needs you")
	for _, want := range []string{`<p class="meta">./...</p>`, `<p class="meta">a slug redirects</p>`} {
		if !strings.Contains(card, want) {
			t.Errorf("the criteria card lacks %q:\n%s", want, card)
		}
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
// planner its dues. A waiver of an unknown criterion refuses, and so do
// Met and Not met, which the gap card does not offer.
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
	rec := r.do(post("/approvals/30/criterion", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"criterion": {"c3"}, "verb": {"waived"}, "reason": {"the timeout is the provider's SLA"}}), ck)
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
	rec = r.do(post("/approvals/30/criterion", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"criterion": {"c9"}, "verb": {"waived"}, "reason": {"no such one"}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "The plan holds no criterion c9") {
		t.Fatalf("an unknown criterion's waiver = %d %q", rec.Code, rec.Body.String())
	}
	a := actor{s: s, via: factoryd.ViaButton}
	for _, press := range [][3]string{{"c2", "met", ""}, {"c2", "not_met", "x"}} {
		_, err := r.c.doDecide(t.Context(), a, 30, press[0], press[1], press[2])
		refusedAs(t, err, http.StatusConflict, "This card has no criterion rows.")
	}
	if a := r.approvals.get(30); a.Status != "pending" {
		t.Errorf("a refused press decided the request: %+v", a)
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
	equal(t, "the decisions' events", decided(r), []string{
		"t-plan #3 map[approval:30 criterion:c3 reason:the timeout is the provider's SLA verb:waived]",
		"t-plan #3 map[approval:30 criteria:1 verb:granted]",
	})
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
	waive := url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "criterion": {"c3"}, "verb": {"waived"}, "reason": {"the provider's SLA"}}
	if rec := r.do(post("/approvals/30/criterion", waive), ck); rec.Code != http.StatusSeeOther {
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
	rec := r.do(post("/approvals/30/criterion", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"criterion": {"c1"}, "verb": {"waived"}, "reason": {"it holds"}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Criterion c1 is met already. Reload the board.") {
		t.Fatalf("a waiver of a met criterion = %d %q", rec.Code, rec.Body.String())
	}
	if calls != 1 || r.approvals.get(30).Status != "pending" || len(decided(r)) != 0 {
		t.Errorf("the refused waiver reached the store %d times, decided the request or recorded %v", calls, decided(r))
	}
}

// decided are r's criterion_decided events, each its card, issue and
// detail.
func decided(r *rig) []string {
	var out []string
	for _, e := range r.events.all() {
		if e.Kind == factoryd.EventCriterionDecided {
			out = append(out, fmt.Sprintf("%s #%d %v", e.TaskID, e.IssueNumber, e.Detail))
		}
	}
	return out
}

// refusedAs fails t unless err is the refusal status with msg.
func refusedAs(t *testing.T, err error, status int, msg string) {
	t.Helper()
	var no *refusal
	if !errors.As(err, &no) || no.status != status || no.msg != msg {
		t.Fatalf("the refusal = %v, want %d %q", err, status, msg)
	}
}

// humanCriteria are humanBoard's plan's criteria.
var humanCriteria = []factoryd.PlanCriterion{
	{ID: "size_bound", Kind: factoryd.CriterionTest, Claim: "every size row stays under its bound", Inputs: "GET /sizes", Expected: "3 rows"},
	{ID: "reads_calm", Kind: factoryd.CriterionJudgement, Claim: "the sizes page reads calm", Prompt: "Judge whether the page names every size."},
	{ID: "all_green", Kind: factoryd.CriterionInvariant, Suite: "./..."},
	{ID: "pages_ok", Kind: factoryd.CriterionHuman, Claim: "The pages read well", Evidence: "Open /sizes and /doors at 390 px"},
	{ID: "page_links", Kind: factoryd.CriterionHuman, Claim: "Every page links home", Evidence: "the links on /help"},
}

// humanBoard is sf/code's board with plan t-plan's integration card t-int
// (#5) in review on merge request 31 (pull request #12): the gate's and
// the judgement's rows stand on it, the human criteria pages_ok and
// page_links have none, and pages_ok's proof wrote its artifact. The
// feature card t-sizes (#6) waits on merge request 32 (pull request #13).
func humanBoard() labScene {
	plan, _ := json.Marshal(factoryd.Plan{Summary: "The sizes and doors pages", Criteria: humanCriteria, Cards: []factoryd.PlanCard{
		{Key: "sizes", Role: factoryd.RoleFeature, Title: "The sizes page"},
		{Key: "integration", Role: factoryd.RoleIntegration, Title: "The pages together"},
	}})
	spec := json.RawMessage(`{"risk_class":"standard","head_sha":"8e3a1b0c"}`)
	return labScene{
		tasks: []factoryd.BoardTask{
			{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "done", Process: "spec", Objective: "Sizes and doors",
				Repository: "sf/code", IssueNumber: 3}},
			{Task: factoryd.Task{ID: "t-sizes", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 6, PRNumber: 13,
				Parent: "t-plan", Source: factoryd.SourcePlan, PlanKey: "sizes"}},
			{Task: factoryd.Task{ID: "t-int", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 5, PRNumber: 12,
				Parent: "t-plan", Source: factoryd.SourcePlan, PlanKey: "integration"}},
		},
		plans: map[string][]factoryd.PlanRevision{"t-plan": {{TaskID: "t-plan", Revision: 1, Plan: plan, SHA256: "sha-1"}}},
		criteria: map[string][]factoryd.CriterionVerdict{"t-int": {
			{Criterion: "size_bound", Verdict: "met", Evidence: "blind: criterion size_bound: TestSizeBound in ./proofs/size_bound passed",
				Kind: factoryd.CriterionTest, Source: factoryd.CriteriaSourceGate},
			{Criterion: "reads_calm", Verdict: "met", Evidence: "the page names every size (model big, prompt 6b2355d78a19)",
				Kind: factoryd.CriterionJudgement, Source: factoryd.CriteriaSourceJudgement},
			{Criterion: "all_green", Verdict: "unmet", Evidence: "criterion all_green: suite ./... failed: Failed boom",
				Kind: factoryd.CriterionInvariant, Source: factoryd.CriteriaSourceGate},
		}},
		objects: map[string]string{factoryd.ArtifactKey("t-int", "pages_ok"): pagesOK("sizes: 3 rows\ndoors: 2 rows")},
		approvals: []factoryd.ApprovalRequest{
			{ID: 31, Kind: factoryd.ApprovalMerge, TaskID: "t-int", Project: "sf/code", PRNumber: 12, Status: "pending",
				Reason: "the project's merge rule is human", Spec: spec},
			{ID: 32, Kind: factoryd.ApprovalMerge, TaskID: "t-sizes", Project: "sf/code", PRNumber: 13, Status: "pending",
				Reason: "the project's merge rule is human", Spec: spec},
		},
	}
}

// pagesOK is the stored artifact pages_ok's proof wrote at 8e3a1b0c, its
// content content.
func pagesOK(content string) string {
	b, _ := json.Marshal(factoryd.GateArtifact{Criterion: "pages_ok", Name: "pages_ok.txt", Head: "8e3a1b0c", Content: content})
	return string(b)
}

// humanRig seeds r with humanBoard. A decided criterion lands as the
// approver's row on t-int, and the calls it took are returned.
func humanRig(r *rig) *[]string {
	s := humanBoard()
	r.tasks.tasks, r.tasks.plans, r.tasks.criteria = s.tasks, s.plans, s.criteria
	for k, v := range s.objects {
		_ = r.c.cfg.Timelines.Put(context.Background(), k, []byte(v))
	}
	r.approvals.rows = append(r.approvals.rows, s.approvals...)
	var calls []string
	r.c.cfg.DecideCriterion = func(_ context.Context, id, cid, verdict, by, reason string) error {
		i := slices.IndexFunc(humanCriteria, func(c factoryd.PlanCriterion) bool { return c.ID == cid })
		if i < 0 {
			return factoryd.ErrNoCriterion
		}
		calls = append(calls, fmt.Sprintf("%s %s %s by %s: %s", id, cid, verdict, by, reason))
		r.tasks.mu.Lock()
		defer r.tasks.mu.Unlock()
		r.tasks.criteria["t-int"] = append(r.tasks.criteria["t-int"], factoryd.CriterionVerdict{Criterion: cid, Verdict: verdict,
			Evidence: reason, Kind: humanCriteria[i].Kind, Source: factoryd.CriteriaSourceApprover, DecidedBy: by})
		return nil
	}
	return &calls
}

// The human settles each human criterion row of the integration card's
// merge request: Met records their words beside what they saw, Not met
// sends the card back to its agent with their words, and Waive takes any
// row. Met and Not met stay a human criterion's, the reason is required
// for Not met and Waive, a row met once takes no second press, and a card
// that is not the plan's integration card has no rows. Approve waits for
// every row's verdict and writes no row itself.
func TestTheHumanRowsPresses(t *testing.T) {
	press := func(t *testing.T, r *rig, id int64, cid, verb, reason string) (string, error) {
		t.Helper()
		_, s := r.loggedIn(t)
		return r.c.doDecide(t.Context(), actor{s: s, via: factoryd.ViaButton}, id, cid, verb, reason)
	}
	pressed := func(t *testing.T, r *rig, id int64, cid, verb, reason, want string) {
		t.Helper()
		if note, err := press(t, r, id, cid, verb, reason); err != nil || note != want {
			t.Fatalf("%s %s = %q, %v; want %q", verb, cid, note, err, want)
		}
	}
	t.Run("met", func(t *testing.T) {
		r := newRig(t)
		calls := humanRig(r)
		pressed(t, r, 31, "pages_ok", "met", "", "Criterion pages_ok met.")
		pressed(t, r, 31, "page_links", "met", "All four pages link home.", "Criterion page_links met.")
		_, err := press(t, r, 31, "pages_ok", "met", "")
		refusedAs(t, err, http.StatusConflict, "Criterion pages_ok is met already. Reload the board.")
		equal(t, "the calls", *calls, []string{
			"t-plan pages_ok met by sebastian: met (pages_ok.txt at 8e3a1b0)",
			"t-plan page_links met by sebastian: All four pages link home. (the criterion's evidence)",
		})
		equal(t, "the decisions' events", decided(r), []string{
			"t-int #5 map[approval:31 criterion:pages_ok reason:met (pages_ok.txt at 8e3a1b0) verb:met]",
			"t-int #5 map[approval:31 criterion:page_links reason:All four pages link home. (the criterion's evidence) verb:met]",
		})
		if q := r.approvals.get(31); q.Status != "pending" {
			t.Errorf("a met decided the merge request: %+v", q)
		}
	})
	t.Run("not met", func(t *testing.T) {
		r := newRig(t)
		calls := humanRig(r)
		pressed(t, r, 31, "page_links", "not_met", "The help page has no links yet.",
			"Criterion page_links is not met. The card goes back to its agent with your words.")
		const words = "criterion page_links not met: The help page has no links yet."
		if len(*calls) != 0 {
			t.Errorf("Not met reached DecideCriterion: %q", *calls)
		}
		equal(t, "the decisions' events", decided(r), []string{
			"t-int #5 map[approval:31 criterion:page_links reason:The help page has no links yet. verb:not_met]",
		})
		if len(r.fg.reviews) != 1 || r.fg.reviews[0]["event"] != "REQUEST_CHANGES" || r.fg.reviews[0]["body"] != words {
			t.Errorf("the pull request's reviews = %v, want one REQUEST_CHANGES with %q", r.fg.reviews, words)
		}
		if q := r.approvals.get(31); q.Status != "denied" || !strings.Contains(q.DecisionNote, words) {
			t.Errorf("the merge request = %+v, want it denied with %q", q, words)
		}
		_, err := press(t, r, 31, "pages_ok", "met", "")
		refusedAs(t, err, http.StatusConflict, "approval request 31 is not pending (any more); reload the board")
		if len(*calls) != 0 || len(decided(r)) != 1 {
			t.Errorf("a press on the decided request took calls %q and events %v", *calls, decided(r))
		}
	})
	t.Run("waive", func(t *testing.T) {
		r := newRig(t)
		calls := humanRig(r)
		pressed(t, r, 31, "all_green", "waived", "CI covers it.", "Criterion all_green waived.")
		equal(t, "the calls", *calls, []string{"t-plan all_green waived by sebastian: CI covers it."})
		equal(t, "the decisions' events", decided(r), []string{
			"t-int #5 map[approval:31 criterion:all_green reason:CI covers it. verb:waived]",
		})
	})
	t.Run("refusals", func(t *testing.T) {
		r := newRig(t)
		calls := humanRig(r)
		for _, tc := range []struct {
			id                int64
			cid, verb, reason string
			status            int
			want              string
		}{
			{31, "all_green", "met", "", http.StatusConflict, "Criterion all_green is the gate's to judge."},
			{31, "all_green", "not_met", "It is red on main.", http.StatusConflict, "Criterion all_green is the gate's to judge."},
			{31, "pages_ok", "not_met", "", http.StatusBadRequest, "a one-line reason of up to 2000 characters is required for Not met and Waive"},
			{31, "pages_ok", "waived", " ", http.StatusBadRequest, "a one-line reason of up to 2000 characters is required for Not met and Waive"},
			{31, "pages_ok", "maybe", "", http.StatusBadRequest, "Choose Met, Not met or Waive: the verb is met, not_met or waived."},
			{31, "pages_ok", "met", strings.Repeat("x", 2001), http.StatusBadRequest, "Met takes up to 1974 characters of words beside what you saw."},
			{31, "c9", "waived", "no such one", http.StatusConflict, "The plan holds no criterion c9. Reload the board."},
			{32, "pages_ok", "waived", "x", http.StatusConflict, "This card has no criterion rows."},
			{3, "pages_ok", "waived", "x", http.StatusConflict, "This card has no criterion rows."},
		} {
			_, err := press(t, r, tc.id, tc.cid, tc.verb, tc.reason)
			refusedAs(t, err, tc.status, tc.want)
		}
		if len(*calls) != 0 || len(decided(r)) != 0 || len(r.fg.reviews) != 0 {
			t.Errorf("a refused press took calls %q, events %v, reviews %v", *calls, decided(r), r.fg.reviews)
		}
		for _, id := range []int64{3, 31, 32} {
			if q := r.approvals.get(id); q.Status != "pending" {
				t.Errorf("a refused press decided request %d: %+v", id, q)
			}
		}
	})
	t.Run("the press", func(t *testing.T) {
		r := newRig(t)
		humanRig(r)
		ck, s := r.loggedIn(t)
		rec := r.do(post("/approvals/31/criterion", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
			"criterion": {"pages_ok"}, "verb": {"met"}, "reason": {""}}), ck)
		if rec.Code != http.StatusSeeOther || rec.Body.Len() != 0 {
			t.Fatalf("the press = %d %q, want 303 with no body", rec.Code, rec.Body.String())
		}
		r.tasks.mu.Lock()
		v, ok := factoryd.StandingRow(r.tasks.criteria["t-int"], factoryd.PlanCriterion{ID: "pages_ok", Kind: factoryd.CriterionHuman})
		r.tasks.mu.Unlock()
		if !ok || v.Verdict != "met" || v.Source != factoryd.CriteriaSourceApprover || v.DecidedBy != "sebastian" {
			t.Errorf("pages_ok's standing row = %+v, %v; want the approver's met", v, ok)
		}
	})
	t.Run("approve", func(t *testing.T) {
		r := newRig(t)
		humanRig(r)
		ck, s := r.loggedIn(t)
		r.fg.calls = nil
		approve := func() *httptest.ResponseRecorder {
			return r.do(post("/approvals/31/approve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
		}
		met := func(cid string) {
			r.tasks.mu.Lock()
			defer r.tasks.mu.Unlock()
			r.tasks.criteria["t-int"] = append(r.tasks.criteria["t-int"], factoryd.CriterionVerdict{Criterion: cid, Verdict: "met",
				Evidence: "met (the criterion's evidence)", Kind: factoryd.CriterionHuman, Source: factoryd.CriteriaSourceApprover, DecidedBy: "sebastian"})
		}
		if rec := approve(); rec.Code != http.StatusConflict ||
			strings.TrimSpace(rec.Body.String()) != "Criteria pages_ok and page_links have no verdict yet: settle each row on the card first." {
			t.Fatalf("approve with two rows open = %d %q", rec.Code, rec.Body.String())
		}
		met("pages_ok")
		if rec := approve(); rec.Code != http.StatusConflict ||
			strings.TrimSpace(rec.Body.String()) != "Criterion page_links has no verdict yet: settle its row on the card first." {
			t.Fatalf("approve with one row open = %d %q", rec.Code, rec.Body.String())
		}
		if len(r.fg.calls) != 0 || len(r.fg.reviews) != 0 || r.approvals.get(31).Status != "pending" {
			t.Fatalf("a refused approve called %q, reviewed %v or decided the request", r.fg.calls, r.fg.reviews)
		}
		met("page_links")
		r.tasks.mu.Lock()
		rows := len(r.tasks.criteria["t-int"])
		r.tasks.mu.Unlock()
		if rec := approve(); rec.Code != http.StatusSeeOther {
			t.Fatalf("approve with every row settled = %d %q", rec.Code, rec.Body.String())
		}
		if !slices.Contains(r.fg.calls, "SIGNAL approved t-int #12 by sebastian") {
			t.Errorf("the approve did not signal the merge: %q", r.fg.calls)
		}
		r.tasks.mu.Lock()
		after := len(r.tasks.criteria["t-int"])
		r.tasks.mu.Unlock()
		if after != rows || len(decided(r)) != 0 {
			t.Errorf("the approve wrote %d rows and the events %v, want none", after-rows, decided(r))
		}
	})
	// settled is humanRig's board with both human rows met by the approver.
	settled := func(r *rig) {
		r.tasks.mu.Lock()
		defer r.tasks.mu.Unlock()
		for _, cid := range []string{"pages_ok", "page_links"} {
			r.tasks.criteria["t-int"] = append(r.tasks.criteria["t-int"], factoryd.CriterionVerdict{Criterion: cid, Verdict: "met",
				Evidence: "met (the criterion's evidence)", Kind: factoryd.CriterionHuman, Source: factoryd.CriteriaSourceApprover, DecidedBy: "sebastian"})
		}
	}
	approved := func(t *testing.T, r *rig) {
		t.Helper()
		_, s := r.loggedIn(t)
		r.fg.calls = nil
		if note, err := r.c.doApprove(t.Context(), actor{s: s, via: factoryd.ViaButton}, 31); err != nil || note != "Approved pull request #12; factoryd merges it." {
			t.Fatalf("approve = %q, %v", note, err)
		}
		if !slices.Contains(r.fg.calls, "SIGNAL approved t-int #12 by sebastian") {
			t.Errorf("the approve did not signal the merge: %q", r.fg.calls)
		}
	}
	t.Run("a gate criterion with no row", func(t *testing.T) {
		r := newRig(t)
		humanRig(r)
		settled(r)
		r.tasks.mu.Lock()
		r.tasks.criteria["t-int"] = slices.DeleteFunc(r.tasks.criteria["t-int"], func(v factoryd.CriterionVerdict) bool { return v.Criterion == "reads_calm" })
		r.tasks.mu.Unlock()
		approved(t, r)
	})
	t.Run("approve reads no artifact", func(t *testing.T) {
		r := newRig(t)
		humanRig(r)
		settled(r)
		r.c.cfg.Timelines = failingObjects{}
		approved(t, r)
	})
	t.Run("a plan with no criteria", func(t *testing.T) {
		r := newRig(t)
		calls := humanRig(r)
		plan, _ := json.Marshal(factoryd.Plan{Summary: "The sizes and doors pages", Cards: []factoryd.PlanCard{
			{Key: "sizes", Role: factoryd.RoleFeature, Title: "The sizes page"},
			{Key: "integration", Role: factoryd.RoleIntegration, Title: "The pages together"},
		}})
		r.tasks.plans["t-plan"][0].Plan = plan
		_, err := press(t, r, 31, "pages_ok", "waived", "x")
		refusedAs(t, err, http.StatusConflict, "This card has no criterion rows.")
		if len(*calls) != 0 {
			t.Errorf("the press took calls %q", *calls)
		}
		approved(t, r)
	})
}

// The integration card's merge card holds a row per criterion in the
// plan's order: a gate row met blind or open says so and reads ended, the
// judgement's row shows the model's reason, the red invariant offers its
// Waive alone, and each human row its artifact or its evidence with Met,
// Not met and Waive. Approve waits on the human rows with no verdict and
// names them. Enter in a human row's field presses nothing, since its
// form's first submit button is a disabled one, while Enter in a row with
// Waive alone presses its Waive. A feature card's merge card has no rows,
// an artifact past 8 KiB shows its first 8 KiB, cut on a character, and a
// failed read says so.
func TestTheMergeCardHoldsARowPerCriterion(t *testing.T) {
	r := newRig(t)
	humanRig(r)
	ck, _ := r.loggedIn(t)
	card := func(id int64) string {
		t.Helper()
		column := col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String(), "Needs you")
		start := strings.Index(column, fmt.Sprintf(`aria-labelledby="approval-%d"`, id))
		if start < 0 {
			t.Fatalf("Needs you holds no approval %d:\n%s", id, column)
		}
		return column[start : start+strings.Index(column[start:], "</article>")]
	}
	rows := func(card string) map[string]string {
		out := map[string]string{}
		for _, f := range regexp.MustCompile(`(?s)<fieldset class="question[^"]*"><legend>.*?</fieldset>`).FindAllString(card, -1) {
			out[f[strings.Index(f, "<legend>")+len("<legend>"):strings.Index(f, " (")]] = f
		}
		return out
	}
	k := card(31)
	legends := regexp.MustCompile(`<fieldset class="question[^"]*"><legend>([^<]*)</legend>`).FindAllStringSubmatch(k, -1)
	var order []string
	for _, l := range legends {
		order = append(order, l[1])
	}
	equal(t, "the rows", order, []string{"size_bound (test): met (blind)", "reads_calm (judgement): met", "all_green (invariant): unmet",
		"pages_ok (human): open", "page_links (human): open"})
	row := rows(k)
	for id, want := range map[string][]string{
		"size_bound": {`<fieldset class="question settled"><legend>size_bound (test): met (blind)</legend>`,
			`<p class="reason">criterion size_bound: TestSizeBound in ./proofs/size_bound passed</p>`},
		"reads_calm": {`<fieldset class="question settled"><legend>reads_calm (judgement): met</legend>`,
			`<p class="reason">the page names every size (model big, prompt 6b2355d78a19)</p>`},
		"all_green": {`<legend>all_green (invariant): unmet</legend>`, `<p class="reason">criterion all_green: suite ./... failed: Failed boom</p>`,
			`action="/approvals/31/criterion"`, `name="reason" maxlength="2000" required hx-preserve`, `aria-label="Waive criterion all_green of t-int"`},
		"pages_ok": {`<legend>pages_ok (human): open</legend>`, `<p class="meta">The pages read well</p>`, `<p class="meta">pages_ok.txt at 8e3a1b0</p>`,
			`<pre class="artifact" tabindex="0">sizes: 3 rows` + "\n" + `doors: 2 rows</pre>`, `name="reason" maxlength="2000" hx-preserve`,
			`name="verb" value="met" aria-label="Met: criterion pages_ok of t-int"`, `name="verb" value="not_met" aria-label="Not met: criterion pages_ok of t-int"`,
			`name="verb" value="waived" aria-label="Waive criterion pages_ok of t-int"`},
		"page_links": {`<legend>page_links (human): open</legend>`, `<p class="meta">Evidence: the links on /help</p>`,
			`aria-label="Met: criterion page_links of t-int"`},
	} {
		for _, w := range want {
			if !strings.Contains(row[id], w) {
				t.Errorf("the %s row lacks %q:\n%s", id, w, row[id])
			}
		}
	}
	for id, not := range map[string][]string{
		"size_bound": {"<form", "blind: "}, "reads_calm": {"<form"}, "all_green": {`value="met"`, `value="not_met"`},
		"pages_ok": {"Evidence:", "Open /sizes and /doors"}, "page_links": {"<pre"},
	} {
		for _, n := range not {
			if strings.Contains(row[id], n) {
				t.Errorf("the %s row holds %q:\n%s", id, n, row[id])
			}
		}
	}
	// the button Enter presses is a form's first submit button
	first := func(row string) string {
		i := strings.Index(row, `<button type="submit"`)
		if i < 0 {
			return ""
		}
		return row[i : i+strings.Index(row[i:], "</button>")+len("</button>")]
	}
	for id, want := range map[string]string{
		"pages_ok":   `<button type="submit" disabled hidden></button>`,
		"page_links": `<button type="submit" disabled hidden></button>`,
		"all_green":  `<button type="submit" class="deny" name="verb" value="waived" aria-label="Waive criterion all_green of t-int">Waive</button>`,
	} {
		if got := first(row[id]); got != want {
			t.Errorf("Enter in the %s row presses %q, want %q", id, got, want)
		}
	}
	if !strings.Contains(k, `<p class="meta">Approve opens once every human criterion has a verdict: pages_ok and page_links.</p>`) ||
		strings.Contains(k, `action="/approvals/31/approve"`) || strings.Contains(k, "yours to judge") {
		t.Errorf("the merge card offers Approve with human rows open, or does not name them:\n%s", k)
	}
	if k := card(32); strings.Contains(k, `<fieldset class="question`) || !strings.Contains(k, `action="/approvals/32/approve"`) {
		t.Errorf("the feature card's merge card holds rows or no Approve:\n%s", k)
	}

	r.tasks.mu.Lock()
	for _, cid := range []string{"pages_ok", "page_links"} {
		r.tasks.criteria["t-int"] = append(r.tasks.criteria["t-int"], factoryd.CriterionVerdict{Criterion: cid, Verdict: "met",
			Evidence: "met (the criterion's evidence)", Kind: factoryd.CriterionHuman, Source: factoryd.CriteriaSourceApprover, DecidedBy: "sebastian"})
	}
	r.tasks.mu.Unlock()
	k = card(31)
	if !strings.Contains(k, `action="/approvals/31/approve"`) || strings.Contains(k, "Approve opens once") ||
		!strings.Contains(rows(k)["pages_ok"], `<fieldset class="question settled"><legend>pages_ok (human): met</legend>`) ||
		strings.Contains(rows(k)["pages_ok"], "<form") {
		t.Errorf("with every human row met the card reads:\n%s", k)
	}

	for _, tc := range []struct{ content, shown string }{
		{strings.Repeat("0123456789", 900), strings.Repeat("0123456789", 819) + "01"},
		{strings.Repeat("a", 8191) + "é" + strings.Repeat("b", 807), strings.Repeat("a", 8191)},
	} {
		_ = r.c.cfg.Timelines.Put(t.Context(), factoryd.ArtifactKey("t-int", "pages_ok"), []byte(pagesOK(tc.content)))
		pages := rows(card(31))["pages_ok"]
		if !strings.Contains(pages, `<pre class="artifact" tabindex="0">`+tc.shown+`</pre>`) ||
			!strings.Contains(pages, `<p class="meta">pages_ok.txt at 8e3a1b0, the first 8 KiB of 9 KiB</p>`) {
			t.Errorf("an artifact of %d bytes shows:\n%.400s", len(tc.content), pages)
		}
	}

	r.c.cfg.Timelines = failingObjects{}
	if k := card(31); !strings.Contains(k, `<p class="problem">The console could not read the criterion rows. See the factoryd log.</p>`) {
		t.Errorf("a failed read of the rows is not said on the card:\n%s", k)
	}
}
