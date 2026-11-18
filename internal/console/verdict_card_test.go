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
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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
			Repository: "sf/code", IssueNumber: 3, Track: policy.TrackPlan}},
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
		Project: "sf/code", Status: "pending", Reason: "criteria not met once every card of the plan ended: c2, c3",
		Spec: json.RawMessage(`{"revision":0,"task":"t-int"}`)})
	gateRanOnMain(r, "t-plan", "t-int", "rev 1 t-found:done t-int:done")
	seatRequestCases(r)
}

// gateRanOnMain records a run of plan's criteria gate at main's head for
// the plan as mark names it, so the rows on card task judge main.
func gateRanOnMain(r *rig, plan, task, mark string) {
	r.events.rows = append(r.events.rows, factoryd.Event{TaskID: plan, Repository: "sf/code", Kind: factoryd.EventCriteriaGateRun, At: time.Now(),
		Detail: map[string]any{"task": task, "pr": 0, "cards": mark, "outcome": "ran"}})
}

// seatRequestCases puts the case each pending request of r opens on the
// desk, as the store's desk reads every live case.
func seatRequestCases(r *rig) {
	if r.openCases == nil {
		r.openCases = map[string][]factoryd.Case{}
	}
	for _, a := range r.approvals.rows {
		for _, q := range r.requestCases(context.Background(), a.TaskID) {
			if q.req.ID == a.ID && !slices.ContainsFunc(r.openCases[a.TaskID], func(k factoryd.Case) bool { return k.ID == q.c.ID }) {
				r.openCases[a.TaskID] = append(r.openCases[a.TaskID], q.c)
			}
		}
	}
}

// criteriaCard is the plan's criteria approval on its opened card.
func criteriaCard(t *testing.T, r *rig, ck *http.Cookie) string {
	t.Helper()
	return approvalFaceOf(t, r.do(httptest.NewRequest(http.MethodGet, "/cards/t-plan/drawer?tab=overview&project=ai-software-factory-code", nil), ck).Body.String(), "t-plan")
}

// page is the opened card of t-plan at its page key, "" for the first.
func criteriaPage(t *testing.T, r *rig, ck *http.Cookie, key string) string {
	t.Helper()
	path := "/cards/t-plan/drawer?tab=overview&project=ai-software-factory-code"
	if key != "" {
		path += "&case=t-plan:" + key
	}
	return getAs(r, ck, path, false).Body.String()
}

// Pieces 7 and 8: each open promise of the criteria gap is a page of its
// own: the ask counts the promises not met of the plan's 3 and says what
// stays blocked; each page names its promise and what happened to it, and
// offers Build the missing part with its price, Drop this promise and, on
// a human promise, It is met. Each answer posts the page's promise unseen
// to its case; a note shows only after the press. The small card opens on
// page 1 of 2. The full brief holds a row per promise in its words.
func TestEachOpenPromiseOfAGapIsAPage(t *testing.T) {
	r := newRig(t)
	verdictRig(r)
	ck, _ := r.loggedIn(t)
	const ask = `<p class="o-question">Plan “Shortener” finished, but 2 of 3 promises are not met. Settle each one.</p>`
	const why = `<p class="o-askwhy">Until both are settled, the plan stays open and holds production deploys.</p>`
	small := faceOf(t, needsOf(t, r, ck, "/"), "t-plan")
	for _, want := range []string{`data-case="7030.1"`, `<strong class="title">Plan “Shortener” finished, but 2 of 3 promises are not met. Settle each one.</strong>`,
		`<span class="pos" aria-live="polite">1 of 2</span>`} {
		if !strings.Contains(small, want) {
			t.Errorf("the small card lacks %s:\n%s", want, small)
		}
	}
	for i, tc := range []struct {
		key, story string
		answers    []string
		criterion  string
	}{
		{"", "<li>The plan promised: The pages read well.</li><li>No one judged it yet.</li>",
			[]string{factoryd.ActGrantCriteria, factoryd.ActAnswerCriteria + "/waive", factoryd.ActAnswerCriteria}, "c2"},
		{"7030.2", "<li>The plan promised: The tests in ./internal/server stay green.</li><li>No test checked it yet.</li>",
			[]string{factoryd.ActGrantCriteria, factoryd.ActAnswerCriteria + "/waive"}, "c3"},
	} {
		page := criteriaPage(t, r, ck, tc.key)
		block := askBlockOf(t, page)
		for _, want := range []string{fmt.Sprintf("Your call · %d of 2", i+1), ask, why, tc.story,
			`<span class="effect">Files one more card for the open promises, which spends up to 4.00, 5.00 or 10.00 EUR by the size its planner gives it.</span>`} {
			if !strings.Contains(block, want) {
				t.Errorf("page %d lacks %q:\n%s", i+1, want, block)
			}
		}
		var got []string
		for _, m := range regexp.MustCompile(`class="answer[^"]*" data-answer="([^"]+)"`).FindAllStringSubmatch(block, -1) {
			got = append(got, m[1])
		}
		equal(t, fmt.Sprintf("page %d's answers", i+1), got, tc.answers)
		for _, key := range tc.answers[1:] {
			p := answerForm(t, page, key)
			if p.Action != "/tasks/t-plan/resolve" || p.Values.Get("case_id") != "7030" || p.Values.Get(key+".criterion") != tc.criterion {
				t.Errorf("page %d: %s posts %s %v; want case 7030 and promise %s", i+1, key, p.Action, p.Values, tc.criterion)
			}
		}
	}
	brief := criteriaCard(t, r, ck)
	for _, want := range []string{
		`<b>A link is stored.</b> met</p>` + "\n" + `<p class="reason">go test ./... exits 0, 214 tests</p>`,
		`<b>The pages read well.</b> open</p>`,
		`<b>The tests in ./internal/server stay green.</b> open</p>`,
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("the full brief lacks %q:\n%s", want, brief)
		}
	}
}

// A promise the gate met or a human dropped since the request opened is no
// page, and the ask counts the open ones: with c3 dropped, one page asks
// c2. The gate's row stands over the review's: c1's failed test makes it
// a page that names the failure.
func TestTheGapsPagesFollowItsRows(t *testing.T) {
	r := newRig(t)
	verdictRig(r)
	r.tasks.criteria["t-int"] = append(r.tasks.criteria["t-int"],
		factoryd.CriterionVerdict{Criterion: "c3", Verdict: "waived", Evidence: "not worth the money", Kind: factoryd.CriterionInvariant, Source: "approver"})
	ck, _ := r.loggedIn(t)
	block := askBlockOf(t, criteriaPage(t, r, ck, ""))
	for _, want := range []string{`<p class="o-question">Plan “Shortener” finished, but 1 of 3 promises is not met. Settle each one.</p>`,
		`<p class="o-askwhy">Until this promise is settled, the plan stays open and holds production deploys.</p>`, "<li>The plan promised: The pages read well.</li>"} {
		if !strings.Contains(block, want) {
			t.Errorf("the one page lacks %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, "o-pages") {
		t.Errorf("one open promise shows a carousel:\n%s", block)
	}
	r.tasks.criteria["t-int"] = append(r.tasks.criteria["t-int"], factoryd.CriterionVerdict{Criterion: "c1", Verdict: "unmet",
		Evidence: "criterion c1: suite ./... failed: Failed boom", Kind: factoryd.CriterionTest, Source: factoryd.CriteriaSourceGate})
	first := askBlockOf(t, criteriaPage(t, r, ck, ""))
	for _, want := range []string{"Your call · 1 of 2", `but 2 of 3 promises are not met.`,
		"<li>The plan promised: A link is stored.</li><li>Its test ran and failed: Failed boom.</li>"} {
		if !strings.Contains(first, want) {
			t.Errorf("c1's page lacks %q:\n%s", want, first)
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
		return criteriaCard(t, r, ck)
	}
	rec := r.do(post("/approvals/30/criterion", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"criterion": {"c3"}, "verb": {"waived"}, "reason": {"the timeout is the provider's SLA"}}), ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?project=ai-software-factory-code" {
		t.Fatalf("waive = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// the waiver settles c3: its page goes, c2's stands, and c3's row reads
	// its standing verdict
	if card := board(); !strings.Contains(card, `name="answer_criteria/waive.criterion" value="c2"`) || strings.Contains(card, `value="c3"`) ||
		!strings.Contains(card, `<b>The tests in ./internal/server stay green.</b> waived</p>`+"\n"+`<p class="reason">the timeout is the provider&#39;s SLA</p>`) {
		t.Errorf("after the waiver the card offers c3 still, or lacks its verdict:\n%s", card)
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
	const (
		met    = "This request records no Met for one criterion. Confirm the delivered cards with your evidence, which meets every open criterion, or waive this criterion with your reason."
		notMet = "This request records no Not met for one criterion. Grant one more bounded card for the open criteria, or waive this criterion with your reason."
	)
	for _, kind := range []string{factoryd.ApprovalCriteria, factoryd.ApprovalCriteriaRerun} {
		r.approvals.rows[len(r.approvals.rows)-1].Kind = kind
		for _, press := range [][4]string{{"c2", "met", "", met}, {"c1", "met", "", met}, {"c2", "not_met", "x", notMet}} {
			_, err := r.c.doDecide(t.Context(), a, 30, press[0], press[1], press[2])
			refusedAs(t, err, http.StatusConflict, press[3])
		}
	}
	r.approvals.rows[len(r.approvals.rows)-1].Kind = factoryd.ApprovalCriteria
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

// The work is enough is keyed on every child of the plan done or merged
// (spec piece 2): it records the human's confirmation of the delivered
// cards from Other actions, and a child still open shows no such answer.
// The request closes on the sweep, not here.
func TestCriteriaGapConfirm(t *testing.T) {
	r := newRig(t)
	verdictRig(r)
	var calls []string
	r.c.cfg.ConfirmDeliveredCriteria = func(_ context.Context, id, by, line string) (int, error) {
		calls = append(calls, fmt.Sprintf("confirm %s by %s: %q", id, by, line))
		return 2, nil
	}
	ck, s := r.loggedIn(t)
	// The work is enough answers the whole request, not one promise: it
	// stands in Other actions
	if p := answerForm(t, drawer(t, r, ck, "t-plan", "overview"), factoryd.ActConfirmCriteria); p.Action != "/tasks/t-plan/resolve" || p.Label != "The work is enough" {
		t.Errorf("the opened card's The work is enough posts %+v", p)
	}
	rec := r.do(post("/approvals/30/confirm", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?project=ai-software-factory-code" {
		t.Fatalf("confirm = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// the answer's own note reaches the confirmation through its form
	send := answerForm(t, drawer(t, r, ck, "t-plan", "overview"), factoryd.ActConfirmCriteria)
	send.Values.Set(factoryd.ActConfirmCriteria+".note", "The board shows both pages.")
	if rec := r.do(post(send.Action, send.Values), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm with its note = %d: %s", rec.Code, rec.Body)
	}
	if want := []string{`confirm t-plan by sebastian: ""`, `confirm t-plan by sebastian: "The board shows both pages."`}; fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	// a note longer than an answer the sessions read is refused, as a
	// case's answer is, and confirms nothing
	send.Values.Set(factoryd.ActConfirmCriteria+".note", strings.Repeat("a", factoryd.MaxAnswer+1))
	if rec := r.do(post(send.Action, send.Values), ck); rec.Code != http.StatusBadRequest || len(calls) != 2 {
		t.Errorf("confirm with a note of %d bytes = %d, calls %q; want 400 and no confirmation", factoryd.MaxAnswer+1, rec.Code, calls)
	}

	// a child not done or merged shows no button
	r.tasks.mu.Lock()
	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].ID == "t-int" {
			r.tasks.tasks[i].Status = "queued"
		}
	}
	r.tasks.mu.Unlock()
	if card := drawer(t, r, ck, "t-plan", "overview"); strings.Contains(card, `data-answer="confirm_criteria"`) {
		t.Errorf("a plan with a child still open offers The work is enough:\n%s", card)
	}
}

// A card cut into a successor that is merged counts as delivered: the
// criteria request offers The work is enough, and one whose successor is
// still open does not. The route records the confirmation
// through the same port the face reads.
func TestCriteriaRerunDeliveredFollowsACutCardToItsSuccessor(t *testing.T) {
	r := newRig(t)
	verdictRig(r)
	cut := factoryd.BoardTask{Task: factoryd.Task{ID: "t-cut-card", Kind: "agent", Repository: "sf/code", Status: "closed",
		Parent: "t-plan", Source: factoryd.SourcePlan, PlanKey: "cut"}}
	succ := factoryd.BoardTask{Task: factoryd.Task{ID: factoryd.SuccessorID(cut.ID), Kind: "agent", Repository: "sf/code", Status: "merged",
		Parent: "t-plan", Source: factoryd.SourceSuccessor, PlanKey: "cut"}}
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks, cut, succ)
	r.tasks.mu.Unlock()
	var calls []string
	r.c.cfg.ConfirmDeliveredCriteria = func(_ context.Context, id, by, line string) (int, error) {
		calls = append(calls, fmt.Sprintf("confirm %s by %s: %q", id, by, line))
		return 2, nil
	}
	ck, s := r.loggedIn(t)
	if card := drawer(t, r, ck, "t-plan", "overview"); !strings.Contains(card, `data-answer="confirm_criteria"`) {
		t.Errorf("the criteria card lacks The work is enough when the cut card's successor merged:\n%s", card)
	}
	rec := r.do(post("/approvals/30/confirm", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm = %d %q", rec.Code, rec.Body.String())
	}
	if want := []string{`confirm t-plan by sebastian: ""`}; fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}

	// an open successor keeps the button off
	r.tasks.mu.Lock()
	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].ID == succ.ID {
			r.tasks.tasks[i].Status = "queued"
		}
	}
	r.tasks.mu.Unlock()
	if card := drawer(t, r, ck, "t-plan", "overview"); strings.Contains(card, `data-answer="confirm_criteria"`) {
		t.Errorf("a plan with a cut card whose successor is open offers The work is enough:\n%s", card)
	}
}

// A request whose promises no test checked offers Build the missing part
// too: its press reaches Act as one grant_criteria and approves the
// request.
func TestCriteriaNeverJudgedGrantsOneCard(t *testing.T) {
	r := newRig(t)
	verdictRig(r)
	r.approvals.rows[len(r.approvals.rows)-1].Kind = factoryd.ApprovalCriteriaRerun
	r.c.cfg.GrantCriteria = func(context.Context, string, string, int64) (int, error) { return 2, nil }
	ck, s := r.loggedIn(t)
	grant := answerForm(t, drawer(t, r, ck, "t-plan", "overview"), factoryd.ActGrantCriteria)
	if grant.Action != "/tasks/t-plan/resolve" || grant.Label != "Build the missing part" {
		t.Fatalf("the opened card's grant posts %+v", grant)
	}
	rec := r.do(post("/approvals/30/approve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
	if rec.Code != http.StatusSeeOther || len(r.acted) != 1 || r.acted[0].Res.Action != factoryd.ActGrantCriteria {
		t.Fatalf("approve = %d %q, Act took %+v, want one grant_criteria", rec.Code, rec.Body.String(), r.acted)
	}
	if a := r.approvals.get(30); a.Status != "approved" {
		t.Errorf("the grant left the request %+v", a)
	}
}

// A card of the plan closed with no successor (a replan cut it, or a
// human closed it) does not keep The work is enough off: its press takes
// the evidence typed with it, a field the form requires, and the store's
// refusal of an empty line names the closed card and what to type. A
// failed card keeps the answer off.
func TestCriteriaRerunConfirmsOverAClosedCard(t *testing.T) {
	r := newRig(t)
	verdictRig(r)
	r.approvals.rows[len(r.approvals.rows)-1].Kind = factoryd.ApprovalCriteriaRerun
	closed := factoryd.BoardTask{Task: factoryd.Task{ID: "t-cut-card", Kind: "agent", Repository: "sf/code", Status: "closed",
		Parent: "t-plan", Source: factoryd.SourcePlan, PlanKey: "cut"}}
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks, closed)
	r.tasks.mu.Unlock()
	gateRanOnMain(r, "t-plan", "t-int", "rev 1 t-cut-card:closed t-found:done t-int:done")
	var calls []string
	r.c.cfg.ConfirmDeliveredCriteria = func(_ context.Context, id, by, line string) (int, error) {
		calls = append(calls, fmt.Sprintf("confirm %s by %s: %q", id, by, line))
		if line == "" {
			return 0, fmt.Errorf("t-cut-card is closed: %w", factoryd.ErrNoEvidence)
		}
		return 3, nil
	}
	ck, s := r.loggedIn(t)
	card := drawer(t, r, ck, "t-plan", "overview")
	confirm := answerForm(t, card, factoryd.ActConfirmCriteria)
	if confirm.Action != "/tasks/t-plan/resolve" || confirm.Label != "The work is enough" {
		t.Fatalf("a plan with a closed card lacks The work is enough: %+v", confirm)
	}
	if !regexp.MustCompile(`<textarea[^>]* name="confirm_criteria\.note"[^>]* required[ >]`).MatchString(card) || !strings.Contains(card, "What shows it") {
		t.Errorf("the answer's form lacks its required evidence line:\n%s", card)
	}
	rec := r.do(post("/approvals/30/confirm", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
	if want := "Cannot confirm the delivered cards: t-cut-card is closed: a confirmation over closed cards needs your evidence. " +
		"Type the line that shows main meets the criteria, then confirm again."; rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("an empty confirm = %d %q, want 400 with %q", rec.Code, rec.Body.String(), want)
	}
	const line = "main at 6e3b46e: the tests pass and ci / test is green"
	rec = r.do(post("/approvals/30/confirm", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "note": {line}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm with evidence = %d %q", rec.Code, rec.Body.String())
	}
	if want := []string{`confirm t-plan by sebastian: ""`, fmt.Sprintf("confirm t-plan by sebastian: %q", line)}; !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}

	r.tasks.mu.Lock()
	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].ID == closed.ID {
			r.tasks.tasks[i].Status = "failed"
		}
	}
	r.tasks.mu.Unlock()
	if card := drawer(t, r, ck, "t-plan", "overview"); strings.Contains(card, `data-answer="confirm_criteria"`) {
		t.Errorf("a plan with a failed card offers The work is enough:\n%s", card)
	}
}

// Fault 9: typed text on the opened card is draft state: every typed
// field carries the preserve treatment, so one answer's press does not
// erase another's words. Each answer's typed fields are its own, so a
// press sends no other's words, and the card says what stays blocked.
func TestTheBoardKeepsTypedTextAndNamesItsClose(t *testing.T) {
	r := newRig(t)
	verdictRig(r)
	ck, _ := r.loggedIn(t)
	card := criteriaCard(t, r, ck)
	if !strings.Contains(card, `<p class="o-askwhy">Until both are settled, the plan stays open and holds production deploys.</p>`) {
		t.Errorf("the card does not say what stays blocked:\n%s", card)
	}
	ask := askBlockOf(t, drawer(t, r, ck, "t-plan", "overview"))
	for _, form := range strings.Split(ask, "<form ")[1:] {
		if n := len(regexp.MustCompile(`<input[^>]*type="text"[^>]*>|<textarea[^>]*>`).FindAllString(form, -1)); n > 1 {
			t.Errorf("an answer holds %d typed fields:\n%s", n, form)
		}
	}
	if n := len(regexp.MustCompile(`<textarea[^>]*>`).FindAllString(ask, -1)); n != 1 {
		t.Errorf("the ask holds %d typed fields, want Drop this promise's reason:\n%s", n, ask)
	}
	for _, f := range regexp.MustCompile(`<input[^>]*type="text"[^>]*>|<textarea[^>]*>`).FindAllString(card, -1) {
		if !strings.Contains(f, " hx-preserve") || !strings.Contains(f, ` id="`) {
			t.Errorf("a typed field loses its text on a swap: %s", f)
		}
	}
}

// The grant answers an open criterion: with every criterion met or
// waived, the card offers no Build the missing part, asks to close the
// plan now or wait for its close, and a posted grant decides nothing
// (fault 9's approved grant with no criterion left open). Close the plan
// now closes it with no note.
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
	card := criteriaCard(t, r, ck)
	if strings.Contains(card, `action="/approvals/30/approve"`) {
		t.Errorf("the grant is offered with no criterion open:\n%s", card)
	}
	for _, want := range []string{`<p class="o-question">Every promise of plan “Shortener” is met or dropped. Close it now?</p>`,
		`<button type="submit" class="pick">Close the plan now</button> <span class="effect">Closes the plan now, so production deploys go on.</span>`,
		`<span class="effect">The plan closes by itself within minutes.</span>`, `<input type="hidden" name="action" value="close">`} {
		if !strings.Contains(card, want) {
			t.Errorf("the card does not ask to close the plan now (%s):\n%s", want, card)
		}
	}
	var closes []string
	r.c.cfg.CheckClosePlan = func(context.Context, string, string, string, bool) error { return nil }
	r.c.cfg.CloseHeldPlan = func(_ context.Context, id, by, note string, _ bool) (int, string, bool, error) {
		closes = append(closes, id+" by "+by+": "+note)
		return 0, factoryd.PlanCloseWords(by, note, false), false, nil
	}
	if rec := r.do(post("/tasks/t-plan/resolve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "action": {factoryd.ActClose}}), ck); rec.Code >= 400 ||
		len(closes) != 1 || closes[0] != "t-plan by sebastian: " {
		t.Errorf("Close the plan now = %d %q, closes %q; want the plan closed once with no note", rec.Code, rec.Body.String(), closes)
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
	seatRequestCases(r)
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
		// each press is an answer_criteria on the request's merge case, the third refused by its work
		if len(r.acted) != 3 || r.acted[0].Res.Action != factoryd.ActAnswerCriteria || r.acted[0].CaseID != 7031 ||
			r.acted[0].Res.Args["criterion"] != "pages_ok" || r.acted[0].Res.Args["verdict"] != "met" || r.acted[1].Res.Args["criterion"] != "page_links" {
			t.Errorf("Act calls = %+v, want three answer_criteria of met on case 7031", r.acted)
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
		refusedAs(t, err, http.StatusConflict, "Approval request 31 waits for no answer any more. Reload the board.")
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
			{31, "pages_ok", "not_met", "", http.StatusBadRequest, "“It is not met” and “Drop this promise” need a one-line reason of up to 2000 characters."},
			{31, "pages_ok", "waived", " ", http.StatusBadRequest, "“It is not met” and “Drop this promise” need a one-line reason of up to 2000 characters."},
			{31, "pages_ok", "maybe", "", http.StatusBadRequest, "Choose “It is met”, “It is not met” or “Drop this promise” (`met`, `not_met` or `waived`)."},
			{31, "pages_ok", "met", strings.Repeat("x", 2001), http.StatusBadRequest, "“It is met” takes up to 1974 characters of words beside what you saw."},
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

// The integration card's merge asks each human promise with no verdict on
// a page of its own, before the merge: page 1 of 3 asks pages_ok, with what
// to look at, and offers It is met, It is not met and Drop this promise,
// each posting the page's promise unseen to the merge's case; the merge's
// page holds Merge it disabled and says why. The full brief holds a row
// per promise in the plan's order, in its words, with no form: a gate row
// met blind or open says so, the judgement's row shows the model's reason,
// the red invariant its failure, and each human row its artifact or its
// evidence. A feature card's merge has no rows and Merge it enabled; once
// every human row has a verdict the pages go and Merge it opens. An
// artifact past 8 KiB shows its first 8 KiB, cut on a character, and a
// failed read says so.
func TestTheMergeCardHoldsARowPerCriterion(t *testing.T) {
	r := newRig(t)
	humanRig(r)
	ck, _ := r.loggedIn(t)
	page := func(task, key string) string {
		t.Helper()
		path := "/cards/" + task + "/drawer?tab=overview"
		if key != "" {
			path += "&case=" + task + ":" + key
		}
		return getAs(r, ck, path, false).Body.String()
	}
	card := func(id int64) string {
		t.Helper()
		task := map[int64]string{31: "t-int", 32: "t-sizes"}[id]
		return approvalFaceOf(t, page(task, ""), task)
	}
	rowRE := regexp.MustCompile(`(?s)<div class="criterion"><p><b>([^<]*)</b> (open|met|unmet|waived|not_met)( \(blind\))?</p>.*?</div>`)
	rows := func(card string) map[string]string {
		out := map[string]string{}
		for _, m := range rowRE.FindAllStringSubmatch(card, -1) {
			out[m[1]] = m[0]
		}
		return out
	}
	k := card(31)
	var order []string
	for _, m := range rowRE.FindAllStringSubmatch(k, -1) {
		order = append(order, m[1]+": "+m[2]+m[3])
	}
	equal(t, "the rows", order, []string{"Every size row stays under its bound.: met (blind)", "The sizes page reads calm.: met",
		"The tests in ./... stay green.: unmet", "The pages read well.: open", "Every page links home.: open"})
	row := rows(k)
	for id, want := range map[string][]string{
		"Every size row stays under its bound.": {`<p class="reason">TestSizeBound in ./proofs/size_bound passed</p>`},
		"The sizes page reads calm.":            {`<p class="reason">the page names every size (model big, prompt 6b2355d78a19)</p>`},
		"The tests in ./... stay green.":        {`<p class="reason">suite ./... failed: Failed boom</p>`},
		"The pages read well.":                  {`<details class="fold"><summary>pages_ok.txt at 8e3a1b0, 1 KiB</summary><pre class="artifact" tabindex="0">sizes: 3 rows` + "\n" + `doors: 2 rows</pre></details>`},
		"Every page links home.":                {`<p class="meta">Evidence: the links on /help</p>`},
	} {
		for _, w := range want {
			if !strings.Contains(row[id], w) {
				t.Errorf("the %s row lacks %q:\n%s", id, w, row[id])
			}
		}
	}
	for id, r := range row {
		if strings.Contains(r, "<form") || strings.Contains(r, "blind: ") {
			t.Errorf("the %s row holds a form or its state twice:\n%s", id, r)
		}
	}
	first := page("t-int", "")
	for _, want := range []string{"Your call · 1 of 3", `<p class="o-question">Is this promise of the plan met?</p>`,
		`<p class="o-askwhy">The pull request merges only once each of the 2 promises a human judges has a verdict.</p>`,
		"<li>The plan promised: The pages read well.</li><li>What to look at: Open /sizes and /doors at 390 px.</li>"} {
		if !strings.Contains(askBlockOf(t, first), want) {
			t.Errorf("the first page lacks %q:\n%s", want, askBlockOf(t, first))
		}
	}
	for _, key := range []string{factoryd.ActAnswerCriteria, factoryd.ActAnswerCriteria + "/not_met", factoryd.ActAnswerCriteria + "/waive"} {
		if p := answerForm(t, first, key); p.Action != "/tasks/t-int/resolve" || p.Values.Get("case_id") != "7031" || p.Values.Get(key+".criterion") != "pages_ok" {
			t.Errorf("%s posts %s %v; want case 7031 and pages_ok", key, p.Action, p.Values)
		}
	}
	if second := askBlockOf(t, page("t-int", "7031.2")); !strings.Contains(second, "<li>The plan promised: Every page links home.</li><li>What to look at: the links on /help.</li>") {
		t.Errorf("the second page:\n%s", second)
	}
	merge := askBlockOf(t, page("t-int", "7031"))
	if !strings.Contains(merge, "Your call · 3 of 3") || !strings.Contains(merge, `<p class="o-note">Merge it opens once the 2 promises on the pages before this one have a verdict.</p>`) ||
		!strings.Contains(merge, `<button type="button" class="pick" disabled>Merge it</button>`) {
		t.Errorf("the merge's page offers Merge it with human promises open, or does not say why:\n%s", merge)
	}
	if k := card(32); strings.Contains(k, `<div class="criterion">`) || answerForm(t, page("t-sizes", ""), factoryd.ActApproveMerge).Action != "/tasks/t-sizes/resolve" {
		t.Errorf("the feature card's merge holds rows or no Merge it:\n%s", k)
	}

	r.tasks.mu.Lock()
	for _, cid := range []string{"pages_ok", "page_links"} {
		r.tasks.criteria["t-int"] = append(r.tasks.criteria["t-int"], factoryd.CriterionVerdict{Criterion: cid, Verdict: "met",
			Evidence: "met (the criterion's evidence)", Kind: factoryd.CriterionHuman, Source: factoryd.CriteriaSourceApprover, DecidedBy: "sebastian"})
	}
	r.tasks.mu.Unlock()
	k = card(31)
	if answerForm(t, page("t-int", ""), factoryd.ActApproveMerge).Action != "/tasks/t-int/resolve" || strings.Contains(k, "Merge it opens once") ||
		!strings.Contains(rows(k)["The pages read well."], `<b>The pages read well.</b> met</p>`) || strings.Contains(k, "Your call · 1 of") {
		t.Errorf("with every human row met the card reads:\n%s", k)
	}

	for _, tc := range []struct{ content, shown string }{
		{strings.Repeat("0123456789", 900), strings.Repeat("0123456789", 819) + "01"},
		{strings.Repeat("a", 8191) + "é" + strings.Repeat("b", 807), strings.Repeat("a", 8191)},
	} {
		_ = r.c.cfg.Timelines.Put(t.Context(), factoryd.ArtifactKey("t-int", "pages_ok"), []byte(pagesOK(tc.content)))
		pages := rows(card(31))["The pages read well."]
		if !strings.Contains(pages, `<pre class="artifact" tabindex="0">`+tc.shown+`</pre>`) ||
			!strings.Contains(pages, `<details class="fold"><summary>pages_ok.txt at 8e3a1b0, the first 8 KiB of 9 KiB</summary>`) {
			t.Errorf("an artifact of %d bytes shows:\n%.400s", len(tc.content), pages)
		}
	}

	r.c.cfg.Timelines = failingObjects{}
	if k := drawer(t, r, ck, "t-int", "overview"); !strings.Contains(k, `<p class="problem" role="status">The console could not read the criterion rows: the artifact of pages_ok: s3: 503. Reload the board.</p>`) {
		t.Errorf("a failed read of the rows is not said on the card:\n%s", k)
	}
}

// A request no case holds takes its page's answers too: Close the plan now
// is the plan's own close, open to every writer as the server's close is
// once nothing is open, and Not now posts a press that leaves the card as
// it is and opens no note, for no ask holds one.
func TestARequestsPageTakesCloseAndNotNow(t *testing.T) {
	v := &boardView{csrf: "c1", user: "ana"}
	k := card{Task: factoryd.Task{ID: "t-plan", Repository: "sf/code", Status: "done", Track: policy.TrackPlan}}
	ap := approvalCard{Request: factoryd.Request{ApprovalRequest: factoryd.ApprovalRequest{ID: 30, Kind: factoryd.ApprovalCriteria, TaskID: "t-plan"}}}
	closePlan, _ := factoryd.AnswerOf(factoryd.AnswerClosePlan)
	notNow, _ := factoryd.AnswerOf(factoryd.KeyPark)
	if p, name := requestForm(v, k, ap, closePlan); p == nil || p.Post != "/tasks/t-plan/resolve" || len(p.Values) != 1 || p.Values[0] != (hidden{"action", factoryd.ActClose}) ||
		p.DataTo != "closed" || name("note") != "close.note" {
		t.Errorf("Close the plan now on a request posts %+v, want the plan's own close", p)
	}
	if p, _ := requestForm(v, k, ap, notNow); p == nil || p.Post != "/tasks/t-plan/resolve" || len(p.Values) != 1 || p.Values[0] != (hidden{"action", factoryd.KeyPark}) ||
		p.DataTo != "" || notNow.Label != "Not now" || len(notNow.Fields) != 0 {
		t.Errorf("Not now on a request posts %+v with fields %v, want the card's resolve that keeps it where it stands and opens no note", p, notNow.Fields)
	}
	plain, _ := factoryd.AnswerOf(factoryd.ActNote)
	if p, _ := requestForm(v, k, ap, plain); p != nil {
		t.Errorf("a Not now that keeps a note posts %+v on a request, and no case holds the note", p)
	}
	dropAndEnd, _ := factoryd.AnswerOf(factoryd.AnswerDropAndEnd)
	if p, _ := requestForm(v, k, ap, dropAndEnd); p != nil {
		t.Errorf("Drop them and close posts %+v on a request, and no request offers it past the approver's check", p)
	}
	if !mayAnswer(v, k, factoryd.AnswerClosePlan) || mayAnswer(v, k, factoryd.AnswerDropAndEnd) {
		t.Error("a writer who approves nothing: Close the plan now is not theirs, or Drop them and close is")
	}

	r := newRig(t)
	verdictRig(r)
	ck, s := r.loggedIn(t)
	before := len(r.approvals.rows)
	if rec := r.do(post("/tasks/t-plan/resolve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "action": {factoryd.KeyPark}}), ck); rec.Code >= 400 ||
		len(r.approvals.rows) != before || len(r.acted) != 0 {
		t.Errorf("Not now with no case = %d %q, Act took %+v; want it taken and nothing changed", rec.Code, rec.Body.String(), r.acted)
	}
}

// A merge request whose spec does not read says so in its own words, and
// not as rows it could not read.
func TestAMergeRequestWhoseSpecDoesNotReadSaysSo(t *testing.T) {
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-res", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 21, Track: policy.TrackDirect})
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 77, Kind: factoryd.ApprovalMerge, TaskID: "t-res", Project: "sf/code",
		Status: "pending", PRNumber: 9, Spec: json.RawMessage(`[]`)})
	ck, _ := r.loggedIn(t)
	body := drawer(t, r, ck, "t-res", "overview")
	if !strings.Contains(body, "The console could not read the merge request: the request&#39;s spec does not read") || strings.Contains(body, "could not read the criterion rows") {
		t.Errorf("the drawer does not name the spec that does not read:\n%s", body)
	}
}
