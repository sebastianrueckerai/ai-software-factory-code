package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// The plan's door takes each criterion with its promise, the claim in
// plain words a human reads on the board: one missing or past 80 runes is
// refused, each in a line that names the criterion and what to write. A
// question offers at most 3 choices, as the human's own words are the
// fourth answer on its face.
func TestPromiseIsChecked(t *testing.T) {
	file := func(edit func(*Plan)) []string {
		t.Helper()
		r, _ := plannerRig(t, nil)
		p := withBrief(acceptedBrief)
		edit(&p)
		res, err := r.FilePlan(context.Background(), "t-plan", planJSON(t, p))
		if err != nil {
			t.Fatal(err)
		}
		return res.Findings
	}
	if got := file(func(*Plan) {}); len(got) != 0 {
		t.Fatalf("a plan whose every criterion holds its promise is refused: %q", got)
	}
	long := strings.Repeat("a", 81)
	for _, tc := range []struct {
		name    string
		promise string
		want    string
	}{
		{"none", "", "plan: criterion redirect_302: promise empty: write the claim in plain words for a human who did not read the plan, at most 80 characters"},
		{"81 runes", long, "plan: criterion redirect_302: promise has 81 characters, at most 80"},
		{"a semicolon", "A short link redirects; it counts the click.", "plan: criterion redirect_302: promise: The text holds a semicolon. Write two sentences."},
	} {
		got := file(func(p *Plan) { p.Criteria[1].Promise = tc.promise })
		if !slices.Equal(got, []string{tc.want}) {
			t.Errorf("%s: findings %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := file(func(p *Plan) { p.Criteria[1].Promise = strings.Repeat("ü", 80) }); len(got) != 0 {
		t.Errorf("an 80-rune promise is refused: %q", got)
	}
	four := file(func(p *Plan) {
		p.Questions = []PlanQuestion{{Text: "Which rows does the feed read?", Choices: []string{"One day", "One week", "One month", "Every row"},
			Recommended: "One week", Grounds: "A week holds the rows a human looks back on."}}
	})
	if want := []string{"plan: question 1: 4 choices; want 2 to 3, as the human's own words are the fourth answer"}; !slices.Equal(four, want) {
		t.Errorf("four choices: findings %q, want %q", four, want)
	}
}

// A press of "Run the tests now" on a plan whose last card closed without a
// merge hands the plan to the criteria gate pass, which runs the tests of
// its promises no verdict judged at main's head in one pod and writes one
// verdict per promise on that card: size_bound's stored proof fails, door's
// and all_green's pass, and the review model judges the page reads_calm's
// test wrote. group_open has no proof, so no test runs for it and no row
// stands; the hold-out runs nowhere, and pages_ok waits for a human. The
// case stays at the human: the rows settle the request.
func TestRunCriteriaRunsTheGate(t *testing.T) {
	ctx := context.Background()
	p := proofsAtMainPlan()
	p.Criteria = append(p.Criteria, readsCalm, pagesOK)
	g := newProofsAtMainRig(t, "integ", p, slices.Concat(proofsAtMainRows(), artifactRows()))
	ll := &fakeLiteLLM{content: `{"verdict":"met","reason":"the page names every size"}`}
	g.r.supervisor = testSupervisorModel(ll.srv(t).URL)
	home := g.store.tasks["t1"]
	home.Status, home.PRNumber = "closed", 0
	g.store.tasks["t1"] = home
	plan := g.store.tasks["plan1"]
	plan.Status = "done"
	g.store.tasks["plan1"] = plan
	c, _, err := g.store.OpenCase(ctx, Case{TaskID: "plan1", Repository: "sf/repo", Kind: CasePlanCriteria, Status: CaseAtHuman, StateKey: "criteria_rerun:9",
		Problem: "criteria never judged", Facts: map[string]any{"approval": 9, "approval_kind": ApprovalCriteriaRerun}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := g.r.Act(ctx, ActInput{TaskID: "plan1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActRunCriteria}})
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if !strings.Contains(out.Text, "The tests of 4 promises run within minutes") {
		t.Fatalf("the press says %q", out.Text)
	}
	if len(g.kube.podOrder) != 0 {
		t.Fatal("the press ran a pod: the gate pass runs it")
	}
	failed := harness.ProofRun{ID: "size_bound", Started: true, Exit: 1, Output: "--- FAIL: TestSizeBound (0.00s)\n    want 400, got 404\nFAIL",
		Report: junitGz(t, wrap(`<testcase name="TestSizeBound"><failure message="Failed">want 400, got 404</failure></testcase>`))}
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.r.CriteriaGatePass(ctx)
	}()
	scriptReproPods(g.kube, proofLines(failed, passedRun(t, "door", "TestDoor"), passedRun(t, "all_green", "TestA"), readsCalmWrote(t)))
	<-done
	fetch := containerEnv(initContainer(t, g.kube.podAt(0).spec["spec"].(map[string]any), "fetch"))
	if fetch["PR_REF"] != "refs/heads/main" || fetch["REPO_SHA"] != "29b0d33aa" {
		t.Fatalf("fetch env = %v, want main at 29b0d33aa", fetch)
	}
	var ids []string
	for _, e := range g.list(t) {
		ids = append(ids, e.ID)
	}
	if want := []string{"size_bound", "door", "all_green", "reads_calm"}; !slices.Equal(ids, want) {
		t.Fatalf("the pod ran %v, want %v", ids, want)
	}
	got := map[string]string{}
	for _, row := range g.store.criteriaVerdicts["t1"] {
		got[row.Criterion] = row.Verdict + " " + row.Evidence
	}
	calm, _, _ := strings.Cut(got["reads_calm"], " (model ")
	if calm != "met the page names every size" || len(ll.chats) != 1 {
		t.Fatalf("reads_calm's row %q after %d calls, want met on the model's reason after one", got["reads_calm"], len(ll.chats))
	}
	delete(got, "reads_calm")
	// each line is criterionLine's: the failure's message and text, the
	// suite's count (junit.go)
	want := map[string]string{
		"size_bound": "unmet blind: criterion size_bound: TestSizeBound in ./proofs/size_bound failed: Failed want 400, got 404",
		"door":       "met blind: criterion door: TestDoor in ./proofs/door passed",
		"all_green":  "met criterion all_green: suite ./... passed (1 passed, 0 skipped)",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	if k, err := g.store.CaseByID(ctx, c.ID); err != nil || k.Status != CaseAtHuman {
		t.Fatalf("the case is %+v, %v; want it at the human until the rows settle the request", k, err)
	}
}

// "Drop this promise" on the last open promise of a done plan closes the
// plan on the next close check: nine promises stand met, the drop waives
// the tenth, the plan records plan_closed and its criteria request ends
// approved, as every promise is met or dropped. Before the drop the plan
// stays open.
func TestDroppingTheLastPromiseClosesThePlan(t *testing.T) {
	ctx := context.Background()
	r, s, _ := tenCriteriaPlan(t, "done")
	var rows []CriterionVerdict
	for i := 1; i <= 9; i++ {
		rows = append(rows, CriterionVerdict{Criterion: "k" + strconv.Itoa(i), Verdict: "met", Kind: CriterionInvariant})
	}
	if err := s.PutCriteriaVerdicts(ctx, "card-1", CriteriaSourceGate, FactoryDecider, rows); err != nil {
		t.Fatal(err)
	}
	id, err := s.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalCriteria, TaskID: "t-plan", Project: "sf/repo", Reason: CriteriaGapReason([]string{"k10"}),
		Spec: json.RawMessage(`{"task":"card-1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	r.planCloseCheck(ctx, "t-plan")
	if closeRows(s) != 0 {
		t.Fatal("the plan closed with k10 open")
	}
	if err := DecideCriterion(ctx, s, "t-plan", "k10", "waived", "sebastian", "the suite moved to another plan"); err != nil {
		t.Fatal(err)
	}
	r.planCloseCheck(ctx, "t-plan")
	if closeRows(s) != 1 {
		t.Fatalf("plan_closed rows %d after the last promise dropped, want 1", closeRows(s))
	}
	approved, err := s.ApprovalRequests(ctx, "approved")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(approved, func(a ApprovalRequest) bool { return a.ID == id }) {
		t.Fatalf("request %d is not approved: %+v", id, approved)
	}
}

// A plan whose last card, #2011, closed without a merge never ran its
// tests: the close check opens the criteria-never-judged request with why
// in words, and each of its 5 promises asks on a page of its own: the
// plan finished and no test checked them, what stays blocked, the promise,
// why no test ran and that none checked it, and exactly "Run the tests
// now" and "Drop this promise".
func TestCriteriaNeverJudgedTellsWhy(t *testing.T) {
	ctx := context.Background()
	r, s, _ := tenCriteriaPlan(t, "closed")
	p := Plan{Summary: "the plan", Cards: []PlanCard{{Key: "card-1", Role: RoleIntegration, Title: "Integration", Directories: []string{"a"}}}}
	for i := 1; i <= 5; i++ {
		p.Criteria = append(p.Criteria, PlanCriterion{ID: fmt.Sprintf("k%d", i), Kind: CriterionInvariant, Promise: fmt.Sprintf("Suite %d stays green.", i), Suite: "./..."})
	}
	newRevision(t, s, "t-plan", p)
	card := s.tasks["card-1"]
	card.IssueNumber, card.MergeCommit, card.FinishedAt = 2011, "", nil
	s.tasks["card-1"] = card
	plan := s.tasks["t-plan"]
	plan.PlanName = "one face"
	s.tasks["t-plan"] = plan
	r.planCloseCheck(ctx, "t-plan")
	pending, err := s.ApprovalRequests(ctx, "pending")
	if err != nil || len(pending) != 1 || pending[0].Kind != ApprovalCriteriaRerun {
		t.Fatalf("pending requests %+v, %v; want one of criteria never judged", pending, err)
	}
	const notRun = "The last card, #2011, closed without a merge, so the factory never ran the plan's tests."
	var spec CriteriaSpec
	if err := json.Unmarshal(pending[0].Spec, &spec); err != nil || spec.NotRun != notRun {
		t.Fatalf("the request's spec %s, want NotRun %q", pending[0].Spec, notRun)
	}
	k, err := RequestCase(pending[0])
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range p.Criteria {
		page := OnPromise(k, PromiseFact{ID: c.ID, Promise: PromiseOf(c), Kind: c.Kind}, 5, 5)
		face := FaceOf(page, Facts{CaseFacts: FactsOf(page), Card: s.tasks["t-plan"], Case: page})
		var labels []string
		for _, a := range face.Answers {
			labels = append(labels, a.Label)
		}
		want := Story{Planned: fmt.Sprintf("The plan promised: Suite %d stays green.", i+1), Happened: notRun, Left: "No test checked it."}
		if face.Ask != "Plan “one face” finished, but no test checked its 5 promises. Settle each one." ||
			face.Why != "Until all 5 are settled, the plan stays open and holds production deploys." || face.Story != want ||
			!slices.Equal(labels, []string{"Run the tests now", "Drop this promise"}) {
			t.Errorf("page %d: %q / %q / %+v / %q", i+1, face.Ask, face.Why, face.Story, labels)
		}
	}
}
