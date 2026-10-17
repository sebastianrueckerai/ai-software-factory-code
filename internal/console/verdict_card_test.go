package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		`<legend>c1 (test): met</legend>`,
		"go test ./...",
		`<p class="reason">go test ./... exits 0, 214 tests</p>`,
		`<legend>c2 (human): open</legend>`,
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
	if card := board(); !strings.Contains(card, `<legend>c3 (invariant): waived</legend>`) ||
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
