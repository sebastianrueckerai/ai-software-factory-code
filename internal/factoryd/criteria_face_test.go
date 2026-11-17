package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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
			Recommended: "One week", Grounds: "A week holds the rows a human looks back on.",
			Changes: []string{"The feed reads a day.", "The feed reads a week.", "The feed reads a month.", "The feed reads every row."}}}
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
		Problem: "criteria never judged", Facts: map[string]any{"approval": 9, "approval_kind": ApprovalCriteriaRerun, "runnable": []string{"size_bound", "door", "all_green", "reads_calm"}}})
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
	var outcomes []string
	for _, e := range g.store.events {
		if e.Kind == EventCriteriaGateRun && e.Detail["by"] == "sebastian" && e.Detail["task"] == "t1" {
			outcomes = append(outcomes, e.Detail["outcome"].(string))
		}
	}
	if want := []string{"asked", "started", "ran"}; !slices.Equal(outcomes, want) {
		t.Fatalf("the run's records %v, want %v: the press on record before the pass, then the gate's own", outcomes, want)
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
		page := OnPromise(k, PromiseFact{ID: c.ID, Promise: PromiseOf(c), Kind: c.Kind})
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

// A request asked before its case held its promises reads them on the
// next close check, as they stand: those it asks about, those nothing
// judged yet, the plan's count, and for criteria never judged those a run
// can check. Its ask then counts them, never "0 promises".
func TestAPendingRequestCountsItsPromisesAsTheyStand(t *testing.T) {
	ctx := context.Background()
	ids := []string{"k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9", "k10"}
	for _, tc := range []struct {
		kind      string
		unchecked []string
		ask       string
	}{
		{ApprovalCriteriaRerun, ids, "This plan finished, but no test checked its 10 promises. Settle each one."},
		{ApprovalCriteria, nil, "This plan finished, but 10 of 10 promises are not met. Settle each one."},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			r, s, _ := tenCriteriaPlan(t, "closed")
			card := s.tasks["card-1"]
			card.IssueNumber = 2011
			if tc.kind == ApprovalCriteriaRerun {
				card.MergeCommit, card.FinishedAt = "", nil
			} else {
				var unmet []CriterionVerdict
				for _, id := range ids {
					unmet = append(unmet, CriterionVerdict{Criterion: id, Verdict: "unmet", Evidence: "suite ./... failed", Kind: CriterionInvariant})
				}
				if err := s.PutCriteriaVerdicts(ctx, "card-1", CriteriaSourceGate, FactoryDecider, unmet); err != nil {
					t.Fatal(err)
				}
			}
			s.tasks["card-1"] = card
			r.planCloseCheck(ctx, "t-plan")
			pending, err := s.ApprovalRequests(ctx, "pending")
			if err != nil || len(pending) != 1 || pending[0].Kind != tc.kind {
				t.Fatalf("pending requests %+v, %v; want one of %s", pending, err, tc.kind)
			}
			k, err := RequestCase(pending[0])
			if err != nil {
				t.Fatal(err)
			}
			for _, fact := range []string{"runnable", "open", "unchecked", "of"} {
				delete(k.Facts, fact)
			}
			c, _, err := s.OpenCase(ctx, k)
			if err != nil {
				t.Fatal(err)
			}
			r.planCloseCheck(ctx, "t-plan")
			got, err := s.CaseByID(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			runnable := ids
			if tc.kind == ApprovalCriteria {
				runnable = nil
			}
			of, _ := toInt(got.Facts["of"])
			if !slices.Equal(factStrings(got, "open"), ids) || !slices.Equal(factStrings(got, "unchecked"), tc.unchecked) || of != 10 ||
				!slices.Equal(factStrings(got, "runnable"), runnable) {
				t.Fatalf("the request's case holds %v", got.Facts)
			}
			if ask := FaceOf(got, Facts{CaseFacts: FactsOf(got), Case: got}).Ask; ask != tc.ask {
				t.Fatalf("its ask = %q, want %q", ask, tc.ask)
			}
		})
	}
}

// Each page of a never-judged request tells what is true of its promise
// and offers what can happen to it: a verdict that stands reads its line
// and takes the gap's answers; a human's promise with no verdict is the
// human's to judge; a promise a run can check offers the run until a run
// is on its way, and after it what that run did; one no test checks says
// so and offers the missing part. The head counts the promises no test
// checked apart from those not met.
func TestTheNeverJudgedPagesFollowEachPromise(t *testing.T) {
	const notRun = "The last card, #2011, closed without a merge, so the factory never ran the plan's tests."
	at := time.Date(2026, 11, 17, 14, 2, 0, 0, time.UTC)
	k := Case{Kind: CasePlanCriteria, Status: CaseAtHuman, Facts: map[string]any{"approval_kind": ApprovalCriteriaRerun, "not_run": notRun,
		"runnable": []string{"door", "size_bound"}}}
	k = OnOpen(k, []string{"size_bound", "pages_ok", "links_ok", "door", "group_open"}, []string{"links_ok", "door", "group_open"}, 6)
	plan := Task{ID: "t-plan", Track: policy.TrackPlan, Status: "done", PlanName: "doors"}
	face := func(p PromiseFact, run *RunFact) AskFace {
		page := OnPromise(k, p)
		if run != nil {
			page = OnRun(page, *run)
		}
		return FaceOf(page, Facts{CaseFacts: FactsOf(page), Card: plan, Case: page})
	}
	labels := func(f AskFace) []string {
		var out []string
		for _, a := range f.Answers {
			out = append(out, a.Label)
		}
		return out
	}
	door := PromiseFact{ID: "door", Promise: "A door opens for its key.", Kind: CriterionTest}
	for _, tc := range []struct {
		name    string
		p       PromiseFact
		run     *RunFact
		story   Story
		answers []string
	}{
		{"its test ran and failed", PromiseFact{ID: "size_bound", Promise: "A size over its bound is refused.", Kind: CriterionTest, Verdict: "unmet",
			Evidence: "blind: criterion size_bound: TestSizeBound in ./proofs/size_bound failed: want 400, got 201"}, nil,
			Story{Planned: "The plan promised: A size over its bound is refused.", Happened: "Its test ran and failed: want 400, got 201."},
			[]string{"Build the missing part", "Drop this promise"}},
		{"a human judged it not met", PromiseFact{ID: "pages_ok", Promise: "The pages read well.", Kind: CriterionHuman, Verdict: "unmet",
			Evidence: "the help page names no panel"}, nil,
			Story{Planned: "The plan promised: The pages read well.", Happened: "A human judged it not met: the help page names no panel."},
			[]string{"Build the missing part", "Drop this promise", "It is met"}},
		{"a human's, not judged", PromiseFact{ID: "links_ok", Promise: "Every page links home.", Kind: CriterionHuman}, nil,
			Story{Planned: "The plan promised: Every page links home.", Left: "No human judged it yet."},
			[]string{"It is met", "It is not met", "Drop this promise"}},
		{"a run can check it", door, nil,
			Story{Planned: "The plan promised: A door opens for its key.", Happened: notRun, Left: "No test checked it."},
			[]string{"Run the tests now", "Drop this promise"}},
		{"no test exists", PromiseFact{ID: "group_open", Promise: "A group opens at its hours.", Kind: CriterionTest}, nil,
			Story{Planned: "The plan promised: A group opens at its hours.", Happened: notRun, Left: "No test exists for it yet."},
			[]string{"Build the missing part", "Drop this promise"}},
		{"a run on its way", door, &RunFact{Outcome: gateStarted, By: "sebastian", At: at},
			Story{Planned: "The plan promised: A door opens for its key.", Happened: "The tests run now, as sebastian asked at 2026-11-17 14:02 UTC.",
				Left: "Each result lands on its promise."},
			[]string{"Drop this promise"}},
		{"the run stopped", door, &RunFact{Outcome: gateCut, By: "sebastian", Why: "its sandbox did not run", At: at},
			Story{Planned: "The plan promised: A door opens for its key.",
				Happened: "The tests sebastian asked for at 2026-11-17 14:02 UTC stopped before they finished: its sandbox did not run.", Left: "No test checked it."},
			[]string{"Run the tests now", "Drop this promise"}},
		{"the card had no room", door, &RunFact{Outcome: gateNoRoom, By: ChatUser, At: at},
			Story{Planned: "The plan promised: A door opens for its key.",
				Happened: "The last card had no room under its spend limit for the tests the project agent asked for, so they did not run.",
				Left:     "A new card brings a budget of its own."},
			[]string{"Build the missing part", "Drop this promise"}},
		{"the run left it no result", door, &RunFact{Outcome: gateDone, By: "sebastian", At: at},
			Story{Planned: "The plan promised: A door opens for its key.", Happened: "The tests ran at 2026-11-17 14:02 UTC, but left no result for it.",
				Left: "No test checked it."},
			[]string{"Run the tests now", "Drop this promise"}},
		{"no test exists, while a run is on its way", PromiseFact{ID: "group_open", Promise: "A group opens at its hours.", Kind: CriterionTest},
			&RunFact{Outcome: gateStarted, By: "sebastian", At: at},
			Story{Planned: "The plan promised: A group opens at its hours.", Happened: notRun, Left: "No test exists for it yet."},
			[]string{"Build the missing part", "Drop this promise"}},
		{"no test exists, after a run", PromiseFact{ID: "group_open", Promise: "A group opens at its hours.", Kind: CriterionTest},
			&RunFact{Outcome: gateDone, By: "sebastian", At: at},
			Story{Planned: "The plan promised: A group opens at its hours.", Happened: notRun, Left: "No test exists for it yet."},
			[]string{"Build the missing part", "Drop this promise"}},
	} {
		f := face(tc.p, tc.run)
		if f.Story != tc.story || !slices.Equal(labels(f), tc.answers) {
			t.Errorf("%s: %+v with %q, want %+v with %q", tc.name, f.Story, labels(f), tc.story, tc.answers)
		}
		if f.Ask != "Plan “doors” finished, but no test checked 3 promises, and 2 are not met. Settle each one." {
			t.Errorf("%s: asks %q", tc.name, f.Ask)
		}
	}
}

// askedRig is the proofs-at-main rig of a done plan whose last card, t1,
// closed without a merge, its never-judged request's case at the human,
// with the promises a run can check.
func askedRig(t *testing.T) (*proofsAtMainRig, Case) {
	t.Helper()
	g := newProofsAtMainRig(t, "integ", proofsAtMainPlan(), proofsAtMainRows())
	home := g.store.tasks["t1"]
	home.Status, home.PRNumber = "closed", 0
	g.store.tasks["t1"] = home
	plan := g.store.tasks["plan1"]
	plan.Status = "done"
	g.store.tasks["plan1"] = plan
	c, _, err := g.store.OpenCase(context.Background(), Case{TaskID: "plan1", Repository: "sf/repo", Kind: CasePlanCriteria, Status: CaseAtHuman,
		StateKey: "criteria_rerun:9", Problem: "criteria never judged",
		Facts: map[string]any{"approval": 9, "approval_kind": ApprovalCriteriaRerun, "runnable": []string{"size_bound", "door", "all_green"}}})
	if err != nil {
		t.Fatal(err)
	}
	return g, c
}

// asked are the records of the runs sebastian asked for on t1: each
// outcome, with why for a cut.
func (g *proofsAtMainRig) asked() []string {
	var out []string
	for _, e := range g.store.events {
		if e.Kind == EventCriteriaGateRun && e.Detail["by"] == "sebastian" && e.Detail["task"] == "t1" {
			out = append(out, strings.TrimSuffix(fmt.Sprintf("%v %v", e.Detail["outcome"], cmp.Or(e.Detail["why"], "")), " "))
		}
	}
	return out
}

// A press of Run the tests now is on record before any pass runs it: a
// restart that loses the queue in memory loses no press, as the close
// check queues it again; and a run whose start no outcome followed past
// the pass's bound is cut, saying why, so its page offers the run again.
// A second press while one is on its way is refused.
func TestARunPressedSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	g, c := askedRig(t)
	press := func() error {
		_, err := g.r.Act(ctx, ActInput{TaskID: "plan1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActRunCriteria}})
		return err
	}
	if err := press(); err != nil {
		t.Fatal(err)
	}
	if err := press(); err == nil || !strings.Contains(err.Error(), "the plan's tests run now") {
		t.Fatalf("a second press = %v, want the refusal that one runs now", err)
	}
	g.r.testRuns = nil
	g.r.planCloseCheck(ctx, "plan1")
	if !slices.Equal(g.r.testRuns, []string{"plan1"}) || !slices.Equal(g.asked(), []string{"asked"}) {
		t.Fatalf("after the restart the queue holds %v and the records %v; want plan1 queued again from its asked row", g.r.testRuns, g.asked())
	}
	g.r.testRuns = nil
	g.store.events = append(g.store.events, Event{Kind: EventCriteriaGateRun, TaskID: "plan1", At: time.Now().Add(-CriteriaGatePassBound - time.Minute),
		Detail: map[string]any{"task": "t1", "outcome": gateStarted, "attempt": "1", "by": "sebastian"}})
	g.r.planCloseCheck(ctx, "plan1")
	if want := []string{"asked", "started", "cut the factory restarted while they ran"}; !slices.Equal(g.asked(), want) || len(g.r.testRuns) != 0 {
		t.Fatalf("records %v, queue %v; want the lost start cut and nothing queued", g.asked(), g.r.testRuns)
	}
	if err := press(); err != nil {
		t.Fatalf("a press after the cut = %v", err)
	}
}

// The tests a human asked for meet the criteria gate's own guard: a card
// whose spend limit leaves no room for the gate reserve records no_room
// and starts no pod, and a sandbox that does not run records its cut with
// why it stopped, which the promise's page shows.
func TestTheAskedTestsMeetTheGatesGuard(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, g *proofsAtMainRig, c Case) {
		t.Helper()
		if _, err := g.r.Act(ctx, ActInput{TaskID: "plan1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActRunCriteria}}); err != nil {
			t.Fatal(err)
		}
		g.r.CriteriaGatePass(ctx)
	}
	t.Run("no room", func(t *testing.T) {
		g, c := askedRig(t)
		home := g.store.tasks["t1"]
		home.BudgetEUR, home.SpentEUR, home.CeilingEUR = 2, 2, 2
		g.store.tasks["t1"] = home
		run(t, g, c)
		if want := []string{"asked", "no_room"}; !slices.Equal(g.asked(), want) || len(g.kube.podOrder) != 0 {
			t.Fatalf("records %v with %d pods, want %v and none", g.asked(), len(g.kube.podOrder), want)
		}
	})
	t.Run("the sandbox does not run", func(t *testing.T) {
		g, c := askedRig(t)
		g.kube.onCreatePod = func() {
			p := g.kube.podOrder[len(g.kube.podOrder)-1]
			p.phase, p.containers = "Pending", []ContainerState{{Name: "fetch", State: "waiting", Reason: "ErrImagePull", Message: imageNotFound}}
		}
		run(t, g, c)
		if want := []string{"asked", "started", "cut its sandbox did not run"}; !slices.Equal(g.asked(), want) {
			t.Fatalf("records %v, want %v", g.asked(), want)
		}
	})
	t.Run("no test is left to run", func(t *testing.T) {
		g, c := askedRig(t)
		if _, err := g.r.Act(ctx, ActInput{TaskID: "plan1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActRunCriteria}}); err != nil {
			t.Fatal(err)
		}
		var waived []CriterionVerdict
		for _, c := range proofsAtMainPlan().Criteria {
			waived = append(waived, CriterionVerdict{Criterion: c.ID, Verdict: "waived", Evidence: "dropped", Kind: c.Kind})
		}
		if err := g.store.PutCriteriaVerdicts(ctx, "t1", CriteriaSourceApprover, "sebastian", waived); err != nil {
			t.Fatal(err)
		}
		g.r.CriteriaGatePass(ctx)
		if want := []string{"asked", "cut no test was left to run"}; !slices.Equal(g.asked(), want) || len(g.kube.podOrder) != 0 {
			t.Fatalf("records %v with %d pods, want %v and none", g.asked(), len(g.kube.podOrder), want)
		}
	})
}

// A plan question's choices each say what they change: the door refuses a
// question without a line per choice, and one whose line fails the
// guide's check, naming the choice and the fault. On its face each
// choice's effect is its line, and the story names the plan's outcome in
// one line. A question stored before the lines were asked sends its
// choice to the planner.
func TestAPlanQuestionSaysWhatEachChoiceChanges(t *testing.T) {
	q := PlanQuestion{Text: "Which rows does the feed read?", Choices: []string{"One day", "One week"}, Recommended: "One week",
		Grounds: "A week holds the rows a human looks back on.", Changes: []string{"The feed reads one day of rows.", "The feed reads a week of rows."}}
	file := func(q PlanQuestion) []string {
		t.Helper()
		r, _ := plannerRig(t, nil)
		p := withBrief(acceptedBrief)
		p.Questions = []PlanQuestion{q}
		res, err := r.FilePlan(context.Background(), "t-plan", planJSON(t, p))
		if err != nil {
			t.Fatal(err)
		}
		return res.Findings
	}
	if got := file(q); len(got) != 0 {
		t.Fatalf("a question whose every choice says what it changes is refused: %q", got)
	}
	none, semicolon := q, q
	none.Changes = nil
	semicolon.Changes = []string{"The feed reads one day; it pages less.", "The feed reads a week of rows."}
	for name, tc := range map[string]struct {
		q    PlanQuestion
		want string
	}{
		"no lines":    {none, "plan: question 1: changes: 0 lines for 2 choices; write one short line per choice, in its order, saying what the choice changes in the plan"},
		"a semicolon": {semicolon, `plan: question 1: changes of "One day": The text holds a semicolon. Write two sentences.`},
	} {
		if got := file(tc.q); !slices.Equal(got, []string{tc.want}) {
			t.Errorf("%s: findings %q, want %q", name, got, tc.want)
		}
	}
	plan := Task{ID: "t-plan", Track: policy.TrackPlan, Status: "waiting-human", PlanName: "feed"}
	face := func(q PlanQuestion) AskFace {
		c, err := CaseFromAsk(plan, Event{TaskID: plan.ID, Kind: EventQuestionAsked, Detail: PlanQuestionRow(q, 1, 1)}, "")
		if err != nil {
			t.Fatal(err)
		}
		c = OnPlan(c, PlanFacts{Outcome: "A writer sees who did what beside the board. The feed pages back by rows."})
		return FaceOf(c, Facts{CaseFacts: FactsOf(c), Card: plan, Case: c})
	}
	effects := func(f AskFace) map[string]string {
		out := map[string]string{}
		for _, a := range f.Answers {
			out[a.Label] = a.Effect
		}
		return out
	}
	f := face(q)
	if f.Story.Planned != "A writer sees who did what beside the board." || effects(f)["One day"] != "The feed reads one day of rows." ||
		effects(f)["One week"] != "The feed reads a week of rows." {
		t.Errorf("the face: %+v with effects %v", f.Story, effects(f))
	}
	if old := effects(face(none)); old["One day"] != "Sends this answer to the planner." {
		t.Errorf("a stored question's choice: %q", old["One day"])
	}
}

// The gate's row reads back in its parts through the one reader: its
// state, and its line without the state and the criterion's lead, which
// the face and the console's brief both show. A row another writer wrote
// reads whole.
func TestGateEvidenceReadsWhatTheGateWrote(t *testing.T) {
	run := criterionRun{id: "size_bound", kind: CriterionTest, suite: "./proofs/size_bound", name: "TestSizeBound"}
	for _, tc := range []struct {
		state   string
		outcome proofOutcome
		met     bool
		line    string
	}{
		{proofBlind, proofOutcome{Outcome: proofFailed, Message: "want 1 over, got 0"}, false, "TestSizeBound in ./proofs/size_bound failed: want 1 over, got 0"},
		{proofOpen, proofOutcome{Outcome: proofPassed}, true, "TestSizeBound in ./proofs/size_bound passed"},
		{"", proofOutcome{Outcome: proofMissing}, false, "no test TestSizeBound in ./proofs/size_bound"},
	} {
		rows := gateRows([]gateProof{{ID: run.id, Kind: run.kind, Met: tc.met, State: tc.state, Line: criterionLine(run, tc.outcome)}})
		if state, line := GateEvidence(run.id, rows[0].Evidence); state != tc.state || line != tc.line {
			t.Errorf("%q reads %q, %q; want %q, %q", rows[0].Evidence, state, line, tc.state, tc.line)
		}
	}
	if state, line := GateEvidence("pages_ok", "the help page names no panel"); state != "" || line != "the help page names no panel" {
		t.Errorf("a human's row reads %q, %q", state, line)
	}
}

// A merge says what its card built: its summary's first sentence, else
// its title's line.
func TestAMergeSaysWhatItsCardBuilt(t *testing.T) {
	c, err := RequestCase(ApprovalRequest{ID: 7, Kind: ApprovalMerge, TaskID: "t-1", Project: "sf/r", PRNumber: 12, Spec: []byte(`{"risk_class":"high","head_sha":"c1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	card := Task{ID: "t-1", Objective: "Chat proposals survive a restart", Summary: "Moves the chat's proposals into a table. The pass purges them."}
	for _, tc := range []struct {
		summary, want string
	}{
		{card.Summary, "Moves the chat's proposals into a table."},
		{"", "This card builds “Chat proposals survive a restart”."},
	} {
		card.Summary = tc.summary
		if got := FaceOf(c, Facts{CaseFacts: FactsOf(c), Card: card, Case: c}).Story.Planned; got != tc.want {
			t.Errorf("summary %q: the story plans %q, want %q", tc.summary, got, tc.want)
		}
	}
}
