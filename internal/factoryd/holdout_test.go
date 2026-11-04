package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A draft is one proof of its own: its file written into a scratch
// package inside the repository, its test run by name there.
func TestHoldoutProofWritesTheDraftIntoItsOwnPackage(t *testing.T) {
	got := holdoutProof(Holdout{Idx: 2, TestName: "TestKeyLog", TestCode: "func TestKeyLog(t *testing.T) {}"})
	want := harness.Proof{ID: "holdout-2", Suite: "./.holdout2", Name: "TestKeyLog",
		File: &harness.ProofFile{Path: ".holdout2/holdout_test.go", Content: "package holdout\nfunc TestKeyLog(t *testing.T) {}\n"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("proof = %+v %+v, want %+v %+v", got, got.File, want, want.File)
	}
}

// The dry verdict, hand-derived one outcome at a time: a failure is red,
// a pass green, and anything else (no such test, a build that fails, a
// skip, no report) unrunnable; the evidence is one line, shortened.
func TestHoldoutDryVerdict(t *testing.T) {
	for name, tc := range map[string]struct {
		o       proofOutcome
		wantDry string
		wantEv  string
	}{
		"red":           {proofOutcome{proofFailed, "Failed\n--- FAIL: TestX (0.00s)"}, "red", "Failed --- FAIL: TestX (0.00s)"},
		"green":         {proofOutcome{proofPassed, ""}, "green", ""},
		"build failed":  {proofOutcome{proofMissing, "Failed\nFAIL\tsf/repo/.holdout2 [build failed]"}, HoldoutUnrunnable, "Failed FAIL sf/repo/.holdout2 [build failed]"},
		"skipped":       {proofOutcome{proofSkipped, "no db"}, HoldoutUnrunnable, "no db"},
		"no report":     {proofOutcome{proofNoReport, "check test wrote no report (exit 1)"}, HoldoutUnrunnable, "check test wrote no report (exit 1)"},
		"long evidence": {proofOutcome{proofFailed, strings.Repeat("a", 250)}, "red", strings.Repeat("a", 200) + "..."},
	} {
		t.Run(name, func(t *testing.T) {
			dry, ev := holdoutDryVerdict(tc.o)
			if dry != tc.wantDry || ev != tc.wantEv {
				t.Fatalf("dry verdict = (%q, %q), want (%q, %q)", dry, ev, tc.wantDry, tc.wantEv)
			}
		})
	}
}

// A plan task for an askHoldouts round: coder-classed so the review
// model answers, on the repo the reverts read from.
func holdoutPlanTask() Task {
	return Task{ID: "plan1", Kind: "agent", Status: "running", Repository: "sf/repo", SHA: "base",
		Process: "maintenance", RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 2, Track: "direct"}
}

// askHoldouts runs the whole round: the review model proposes two
// hold-outs beside the project's past failures, the drafts dry-run on
// the main head in a sandbox of their own, the one that cannot run is
// rewritten once and runs again, and the round lands proposed with its
// verdicts. A rewritten draft never lands without its own run.
func TestAskHoldoutsDraftsDriesRedraftsAndStores(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, store := newTestRunner(t, holdoutPlanTask(), kube, objects)
	store.tasks["r1"] = Task{ID: "r1", Repository: "sf/repo", Source: SourceRevert, Objective: "the slug broke on BAD000\nsecond line"}
	proposal := `{"holdouts":[` +
		`{"id":"door_state","criterion":"The door stays shut until a key that fits turns the lock","test_name":"TestDoorShut","test_code":"func TestDoorShut(t *testing.T) {\n\tt.Fatal(\"shut\")\n}"},` +
		`{"id":"key_log","criterion":"Every key turn lands in the log","test_name":"TestKeyLog","test_code":"func TestKeyLog(t *testing.T) {\n\tt.Logx(\"log\")\n}","from":"the slug broke on BAD000"}` +
		`]}`
	redraft := `{"holdouts":[{"id":"key_log","test_code":"func TestKeyLog(t *testing.T) {\n\tt.Fatal(\"log\")\n}"}]}`
	ll := &fakeLiteLLM{contents: []string{proposal, redraft}}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	p := Plan{Spec: &Spec{Outcome: "The door opens only for a key that fits the lock", Glossary: []string{"the lock"}},
		Criteria: []PlanCriterion{{ID: "green_login", Kind: "test", Claim: "a login answers 200", Inputs: "POST /login", Expected: "200"}}}
	done := make(chan error, 1)
	go func() {
		done <- r.askHoldouts(context.Background(), holdoutPlanTask(), 1, p)
	}()
	scriptReproPods(kube,
		proofLines(
			harness.ProofRun{ID: "holdout-1", Exit: 1, Report: junitGz(t, wrap(`<testcase name="TestDoorShut"><failure message="Failed">--- FAIL: TestDoorShut (0.00s)</failure></testcase>`))},
			harness.ProofRun{ID: "holdout-2", Exit: 1, Report: junitGz(t, wrap(`<testcase classname="" name="TestMain"><failure message="Failed">FAIL&#x9;sf/repo/.holdout2 [build failed]</failure></testcase>`))}),
		proofLines(
			harness.ProofRun{ID: "holdout-2", Exit: 1, Report: junitGz(t, wrap(`<testcase name="TestKeyLog"><failure message="Failed">--- FAIL: TestKeyLog (0.00s)</failure></testcase>`))}))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	rows, err := store.PlanHoldouts(context.Background(), "plan1")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	want1 := Holdout{Idx: 1, Round: 1, ID: "door_state", Origin: HoldoutDrafted,
		Criterion: "The door stays shut until a key that fits turns the lock", State: HoldoutProposed,
		DryHead: "base", DryVerdict: "red", DryOutput: "Failed --- FAIL: TestDoorShut (0.00s)",
		TestName: "TestDoorShut", TestCode: "func TestDoorShut(t *testing.T) {\n\tt.Fatal(\"shut\")\n}"}
	want2 := Holdout{Idx: 2, Round: 1, ID: "key_log", Origin: HoldoutMechanical,
		Criterion: "Every key turn lands in the log", State: HoldoutProposed,
		DryHead: "base", DryVerdict: "red", DryOutput: "Failed --- FAIL: TestKeyLog (0.00s)",
		TestName: "TestKeyLog", TestCode: "func TestKeyLog(t *testing.T) {\n\tt.Fatal(\"log\")\n}"}
	if rows[0] != want1 || rows[1] != want2 {
		t.Fatalf("rows =\n%+v\n%+v\nwant\n%+v\n%+v", rows[0], rows[1], want1, want2)
	}
	if len(kube.podOrder) != 2 {
		t.Fatalf("pods %d, want the dry run and the corrective one", len(kube.podOrder))
	}
	// the second pass runs the redrafted draft alone
	for i, want := range [][]string{{"holdout-1", "holdout-2"}, {"holdout-2"}} {
		var list []harness.Proof
		if err := json.Unmarshal([]byte(podEnv(kube.podOrder[i])["PROOFS"]), &list); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, p := range list {
			ids = append(ids, p.ID)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("pod %d runs %v, want %v", i, ids, want)
		}
	}
	// the planner's sandbox went with its session: the dry runs build
	// their own, with a read token, and tear it down
	if mints := hub.minted(); !slices.Equal(mints, []string{"task-plan1-base"}) || !slices.Contains(kube.deletedNs, "factory-agents-plan1") {
		t.Fatalf("minted %v, deleted namespaces %v; want one read token and the sandbox gone", mints, kube.deletedNs)
	}
	if len(ll.chats) != 2 || ll.chats[0]["model"] != "big" || !slices.Equal(ll.purposes, []string{"review", "review"}) {
		t.Fatalf("chats %v, purposes %v", ll.chats, ll.purposes)
	}
	first, second := userMessage(ll.chats[0]), userMessage(ll.chats[1])
	for _, want := range []string{"the slug broke on BAD000", "green_login", "The door opens only for a key that fits the lock"} {
		if !strings.Contains(first, want) {
			t.Fatalf("the proposal call lacks %q: %s", want, first)
		}
	}
	for _, want := range []string{"TestKeyLog", "build failed"} {
		if !strings.Contains(second, want) {
			t.Fatalf("the redraft call lacks %q: %s", want, second)
		}
	}
	tl := string(objGet(objects, "sessions/plan1/timeline.jsonl"))
	for _, want := range []string{`"step":"holdouts_proposed"`, `"round":1`, `"verdicts":{"door_state":"red","key_log":"red"}`} {
		if !strings.Contains(tl, want) {
			t.Fatalf("timeline lacks %s: %s", want, tl)
		}
	}
}

// A refusal leaves the round empty and the timeline names why: the
// confirmation question stands on its own, nothing waits on the model.
func TestAskHoldoutsRecordsARefusalAndLeavesTheRoundEmpty(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, _, store := newTestRunner(t, holdoutPlanTask(), kube, objects)
	ll := &fakeLiteLLM{contents: []string{"not the json"}}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	if err := r.askHoldouts(context.Background(), holdoutPlanTask(), 1, Plan{}); err != nil {
		t.Fatal(err)
	}
	if rows, _ := store.PlanHoldouts(context.Background(), "plan1"); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none", rows)
	}
	if len(kube.podOrder) != 0 || len(ll.chats) != 1 {
		t.Fatalf("pods %d, chats %d", len(kube.podOrder), len(ll.chats))
	}
	if tl := string(objGet(objects, "sessions/plan1/timeline.jsonl")); !strings.Contains(tl, `"step":"holdouts_not_drafted"`) || !strings.Contains(tl, "the answer is not the hold-outs json") {
		t.Fatalf("timeline = %s", tl)
	}
}

// A dry-run pod that ends before it reports every draft leaves the round
// empty and the timeline names why: an unreached draft is not one that
// cannot run, so nothing goes to the redraft.
func TestAskHoldoutsLeavesTheRoundEmptyWhenTheDryRunEndsEarly(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, _, store := newTestRunner(t, holdoutPlanTask(), kube, objects)
	proposal := `{"holdouts":[` +
		`{"id":"door_state","criterion":"The door stays shut until a key that fits turns the lock","test_name":"TestDoorShut","test_code":"func TestDoorShut(t *testing.T) {}"},` +
		`{"id":"key_log","criterion":"Every key turn lands in the log","test_name":"TestKeyLog","test_code":"func TestKeyLog(t *testing.T) {}"}` +
		`]}`
	ll := &fakeLiteLLM{contents: []string{proposal}}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	done := make(chan error, 1)
	go func() {
		done <- r.askHoldouts(context.Background(), holdoutPlanTask(), 1, Plan{Spec: &Spec{Outcome: "The door opens only for a key that fits the lock"}})
	}()
	body := proofLines(harness.ProofRun{ID: "holdout-1", Exit: 1,
		Report: junitGz(t, wrap(`<testcase name="TestDoorShut"><failure message="Failed">--- FAIL: TestDoorShut (0.00s)</failure></testcase>`))})
	scriptReproPods(kube, body[:strings.LastIndex(body, "\n")])
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if rows, _ := store.PlanHoldouts(context.Background(), "plan1"); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none", rows)
	}
	if len(kube.podOrder) != 1 || len(ll.chats) != 1 {
		t.Fatalf("pods %d, chats %d; want the one dry run and no redraft", len(kube.podOrder), len(ll.chats))
	}
	if tl := string(objGet(objects, "sessions/plan1/timeline.jsonl")); !strings.Contains(tl, `"step":"holdouts_not_drafted"`) ||
		!strings.Contains(tl, "dry run: the proofs pod ended early: 1 of 2 entries unreported") {
		t.Fatalf("timeline = %s", tl)
	}
}

// A round asks once: rows of the round mean an activity retry, no
// second model call, no second sandbox.
func TestAskHoldoutsAsksOnceARound(t *testing.T) {
	kube := newFakeKube()
	r, _, store := newTestRunner(t, holdoutPlanTask(), kube, &fakeObjectStore{})
	seeded := []Holdout{{Idx: 1, Round: 1, ID: "door_state", State: HoldoutProposed, DryVerdict: "red"}}
	if _, err := store.PutPlanHoldouts(context.Background(), "plan1", 1, seeded); err != nil {
		t.Fatal(err)
	}
	if err := r.askHoldouts(context.Background(), holdoutPlanTask(), 1, Plan{}); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.PlanHoldouts(context.Background(), "plan1")
	if len(rows) != 1 || rows[0] != seeded[0] {
		t.Fatalf("rows = %+v, want the round's own untouched", rows)
	}
	if len(kube.podOrder) != 0 {
		t.Fatalf("pods %d, want none", len(kube.podOrder))
	}
}

// A new round sweeps the previous round's undecided proposals before it
// asks; the model may then propose none.
func TestAskHoldoutsSweepsThePreviousRoundsProposals(t *testing.T) {
	kube := newFakeKube()
	r, _, store := newTestRunner(t, holdoutPlanTask(), kube, &fakeObjectStore{})
	if _, err := store.PutPlanHoldouts(context.Background(), "plan1", 0,
		[]Holdout{{Idx: 1, Round: 0, ID: "old_gap", State: HoldoutProposed, DryVerdict: "red"}}); err != nil {
		t.Fatal(err)
	}
	ll := &fakeLiteLLM{contents: []string{`{"holdouts":[]}`}}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	if err := r.askHoldouts(context.Background(), holdoutPlanTask(), 1, Plan{}); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.PlanHoldouts(context.Background(), "plan1")
	if len(rows) != 1 || rows[0].State != HoldoutDropped || rows[0].ID != "old_gap" {
		t.Fatalf("rows = %+v, want the round 0 proposal dropped", rows)
	}
	if len(ll.chats) != 1 || len(kube.podOrder) != 0 {
		t.Fatalf("chats %d, pods %d", len(ll.chats), len(kube.podOrder))
	}
}

// DecideHoldout takes the console's okay or drop: only the newest round
// while its confirmation stands unanswered, only a click the row's state
// takes (an okay needs a verdict of red or green; unrunnable takes only
// drop), and each click lands once.
func TestDecideHoldout(t *testing.T) {
	ctx := context.Background()
	newStore := func(t *testing.T) *fakeStore {
		store := newFakeStore()
		store.tasks["t-plan"] = Task{ID: "t-plan", Kind: "agent", Status: "waiting-human", Repository: "sf/repo"}
		if _, err := store.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary":"s"}`), "sha1"); err != nil {
			t.Fatal(err)
		}
		store.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalPlan, TaskID: "t-plan"})
		return store
	}
	proposed := func() []Holdout {
		return []Holdout{{Idx: 1, Round: 1, ID: "door_state", State: HoldoutProposed, DryVerdict: "red"}}
	}
	t.Run("no plan waiting", func(t *testing.T) {
		store := newFakeStore()
		store.tasks["t-plan"] = Task{ID: "t-plan"}
		if err := DecideHoldout(ctx, store, "t-plan", 1, 1, "okay", "sebastian"); err == nil || err.Error() != "task t-plan has no plan waiting for answers" {
			t.Fatalf("no approval: %v", err)
		}
	})
	t.Run("stale round", func(t *testing.T) {
		store := newStore(t)
		err := DecideHoldout(ctx, store, "t-plan", 2, 1, "okay", "sebastian")
		var stale ErrStalePlan
		if !errors.As(err, &stale) || stale.Newest != 1 || err.Error() != "the plan changed to revision 1; reload" {
			t.Fatalf("stale round: %v", err)
		}
	})
	t.Run("the confirmation answered", func(t *testing.T) {
		store := newStore(t)
		if _, _, err := store.PutPlanConfirmation(ctx, "t-plan", 1, PlanQuestion{Text: "Confirm the plan?"}); err != nil {
			t.Fatal(err)
		}
		if err := store.AnswerPlanQuestion(ctx, "t-plan", 1, 1, DecisionConfirm, "", "sebastian"); err != nil {
			t.Fatal(err)
		}
		if err := DecideHoldout(ctx, store, "t-plan", 1, 1, "okay", "sebastian"); !errors.Is(err, ErrAnswered) {
			t.Fatalf("after the confirmation: %v", err)
		}
	})
	t.Run("the confirmation revised", func(t *testing.T) {
		store := newStore(t)
		if _, _, err := store.PutPlanConfirmation(ctx, "t-plan", 1, PlanQuestion{Text: "Confirm the plan?"}); err != nil {
			t.Fatal(err)
		}
		if err := store.AnswerPlanQuestion(ctx, "t-plan", 1, 1, DecisionRevise, "keep the slate clean", "sebastian"); err != nil {
			t.Fatal(err)
		}
		if err := DecideHoldout(ctx, store, "t-plan", 1, 1, "okay", "sebastian"); !errors.Is(err, ErrAnswered) {
			t.Fatalf("after the revise: %v", err)
		}
	})
	t.Run("no such verb", func(t *testing.T) {
		store := newStore(t)
		if _, err := store.PutPlanHoldouts(ctx, "t-plan", 1, proposed()); err != nil {
			t.Fatal(err)
		}
		err := DecideHoldout(ctx, store, "t-plan", 1, 1, "maybe", "sebastian")
		if !errors.Is(err, ErrHoldoutClick) || err.Error() != "the hold-out takes no such click now: maybe" {
			t.Fatalf("verb: %v", err)
		}
	})
	t.Run("no such hold-out", func(t *testing.T) {
		store := newStore(t)
		if err := DecideHoldout(ctx, store, "t-plan", 1, 9, "drop", "sebastian"); !errors.Is(err, ErrNoHoldout) {
			t.Fatalf("missing row: %v", err)
		}
	})
	t.Run("okay stores a red proposal", func(t *testing.T) {
		store := newStore(t)
		if _, err := store.PutPlanHoldouts(ctx, "t-plan", 1, proposed()); err != nil {
			t.Fatal(err)
		}
		if err := DecideHoldout(ctx, store, "t-plan", 1, 1, "okay", "sebastian"); err != nil {
			t.Fatal(err)
		}
		rows, _ := store.PlanHoldouts(ctx, "t-plan")
		if rows[0].State != HoldoutStored {
			t.Fatalf("row = %+v", rows[0])
		}
		if err := DecideHoldout(ctx, store, "t-plan", 1, 1, "drop", "sebastian"); !errors.Is(err, ErrHoldoutClick) {
			t.Fatalf("a second click: %v", err)
		}
	})
	t.Run("a confirmation asked again", func(t *testing.T) {
		store := newStore(t)
		for _, sha := range []string{"sha2", "sha3"} {
			if _, err := store.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary":"`+sha+`"}`), sha); err != nil {
				t.Fatal(err)
			}
		}
		confirm := func(revision int) {
			if _, _, err := store.PutPlanConfirmation(ctx, "t-plan", revision, PlanQuestion{Text: "Confirm the plan?"}); err != nil {
				t.Fatal(err)
			}
			if err := store.AnswerPlanQuestion(ctx, "t-plan", revision, 1, DecisionConfirm, "", "sebastian"); err != nil {
				t.Fatal(err)
			}
		}
		confirm(2)
		third := proposed()
		third[0].Round = 3
		if _, err := store.PutPlanHoldouts(ctx, "t-plan", 3, third); err != nil {
			t.Fatal(err)
		}
		if err := DecideHoldout(ctx, store, "t-plan", 3, 1, "okay", "sebastian"); err != nil {
			t.Fatalf("revision 2 confirmed, round 3's hold-out: %v", err)
		}
		if rows, _ := store.PlanHoldouts(ctx, "t-plan"); rows[0].State != HoldoutStored {
			t.Fatalf("row = %+v", rows[0])
		}
		confirm(3)
		if err := DecideHoldout(ctx, store, "t-plan", 3, 1, "drop", "sebastian"); !errors.Is(err, ErrAnswered) {
			t.Fatalf("revision 3 confirmed: %v", err)
		}
	})
	t.Run("unrunnable takes only drop", func(t *testing.T) {
		store := newStore(t)
		if _, err := store.PutPlanHoldouts(ctx, "t-plan", 1,
			[]Holdout{{Idx: 1, Round: 1, ID: "door_state", State: HoldoutUnrunnable}}); err != nil {
			t.Fatal(err)
		}
		if err := DecideHoldout(ctx, store, "t-plan", 1, 1, "okay", "sebastian"); !errors.Is(err, ErrHoldoutClick) {
			t.Fatalf("okay on unrunnable: %v", err)
		}
		if err := DecideHoldout(ctx, store, "t-plan", 1, 1, "drop", "sebastian"); err != nil {
			t.Fatal(err)
		}
		rows, _ := store.PlanHoldouts(ctx, "t-plan")
		if rows[0].State != HoldoutDropped {
			t.Fatalf("row = %+v", rows[0])
		}
	})
}

// The confirming answer waits for the round's hold-out clicks; a normal
// question's answer does not. Once every proposal is decided the
// confirmation answer lands, sweeps stragglers of older rounds and
// signals the planner.
func TestAnswerPlanWaitsForTheHoldoutDecisions(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.tasks["t-plan"] = Task{ID: "t-plan", Kind: "agent", Status: "waiting-human", Track: "plan", IssueNumber: 20, Repository: "linkshort/linkshort"}
	raw, sha, _ := planSHA(questionPlan())
	store.AddPlanRevision(ctx, "t-plan", raw, sha)
	store.PutPlanQuestions(ctx, "t-plan", 1, questionPlan().Questions)
	if _, _, err := store.PutPlanConfirmation(ctx, "t-plan", 1, PlanQuestion{Text: "Does the plan hold against the spec?"}); err != nil {
		t.Fatal(err)
	}
	store.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalPlan, TaskID: "t-plan"})
	if _, err := store.PutPlanHoldouts(ctx, "t-plan", 1,
		[]Holdout{{Idx: 1, Round: 1, ID: "door_state", State: HoldoutProposed, DryVerdict: "red"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutPlanHoldouts(ctx, "t-plan", 0,
		[]Holdout{{Idx: 2, Round: 0, ID: "old_gap", State: HoldoutProposed, DryVerdict: "red"}}); err != nil {
		t.Fatal(err)
	}
	wf := &signalLog{}
	if err := AnswerPlan(ctx, store, wf, "t-plan", 1, 1, "", "60 seconds", "alice"); err != nil {
		t.Fatalf("a normal answer waits on nothing: %v", err)
	}
	if err := AnswerPlan(ctx, store, wf, "t-plan", 1, 3, DecisionConfirm, "", "sebastian"); !errors.Is(err, ErrHoldoutsOpen) {
		t.Fatalf("the confirming answer: %v", err)
	}
	if err := DecideHoldout(ctx, store, "t-plan", 1, 1, "okay", "sebastian"); err != nil {
		t.Fatal(err)
	}
	if err := AnswerPlan(ctx, store, wf, "t-plan", 1, 3, DecisionConfirm, "", "sebastian"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(wf.got, []string{"t-plan plan_answered alice revision 1 index 1", "t-plan plan_answered sebastian revision 1 index 3"}) {
		t.Fatalf("signals %q", wf.got)
	}
	rows, _ := store.PlanHoldouts(ctx, "t-plan")
	if len(rows) != 2 || rows[0].State != HoldoutStored || rows[1].State != HoldoutDropped {
		t.Fatalf("rows = %+v, want the round's okayed and the old round swept", rows)
	}
	if open, _ := store.UndecidedHoldouts(ctx, "t-plan", 1); open != 0 {
		t.Fatalf("round 1 holds %d undecided", open)
	}
}

// The criteria gate runs the plan's stored hold-outs beside its
// criteria, blind to the agent: red or green, a hold-out is its verdict
// on the timeline line alone, never the card's findings, its comment or
// the result's Holdouts, which no audit settles.
func TestGateCriteriaRunsTheStoredHoldoutsBlindToTheAgent(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, objects)
	seedPlan(t, store, map[string]string{"integ": RoleIntegration},
		`{"id":"a_green","kind":"invariant","suite":"./a"}`)
	rows := []Holdout{
		{Idx: 1, Round: 1, ID: "door_state", State: HoldoutStored, DryVerdict: "red",
			TestName: "TestDoorState", TestCode: "func TestDoorState(t *testing.T) {}"},
		{Idx: 2, Round: 1, ID: "key_log", State: HoldoutStored, DryVerdict: "green",
			TestName: "TestKeyLog", TestCode: "func TestKeyLog(t *testing.T) {}"},
	}
	if _, err := store.PutPlanHoldouts(context.Background(), "plan1", 1, rows); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var res CriteriaResult
	var err error
	go func() {
		defer close(done)
		res, err = runCriteria(t, r)
	}()
	scriptReproPods(kube, proofLines(
		harness.ProofRun{ID: "a_green", Report: junitGz(t, wrap(`<testcase name="TestA"></testcase>`))},
		harness.ProofRun{ID: "holdout-1", Exit: 1, Report: junitGz(t, wrap(`<testcase name="TestDoorState"><failure message="Failed">--- FAIL: TestDoorState (0.01s)</failure></testcase>`))},
		harness.ProofRun{ID: "holdout-2", Report: junitGz(t, wrap(`<testcase name="TestKeyLog"></testcase>`))},
	))
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 0 || res.Skipped != "" || res.Escalate != "" {
		t.Fatalf("result = %+v, want the criteria met", res)
	}
	if res.Holdouts != nil {
		t.Fatalf("holdouts = %+v, want none while holdout-1 failed", res.Holdouts)
	}
	if len(kube.podOrder) != 1 || len(hub.minted()) != 1 {
		t.Fatalf("pods %d, mints %v", len(kube.podOrder), hub.minted())
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "1 of 1 criteria met") {
		t.Fatalf("comments = %q", hub.comments)
	}
	var list []harness.Proof
	if err := json.Unmarshal([]byte(podEnv(kube.podOrder[0])["PROOFS"]), &list); err != nil || len(list) != 3 {
		t.Fatalf("PROOFS = %+v (%v), want the criterion and the two hold-outs", list, err)
	}
	for i, h := range rows {
		if want := holdoutProof(h); !reflect.DeepEqual(list[1+i], want) || list[1+i].File == nil {
			t.Fatalf("entry %d = %+v, want %+v with its file", 1+i, list[1+i], want)
		}
	}
	verdicts, _ := store.CriteriaVerdicts(context.Background(), "t1")
	if len(verdicts) != 1 || verdicts[0].Criterion != "a_green" || verdicts[0].Verdict != "met" {
		t.Fatalf("verdict rows = %+v, want a_green's alone", verdicts)
	}
	step := timelineStep(t, objects, "t1", "gate_criteria")
	if proofs, _ := step["proofs"].([]any); len(proofs) != 1 {
		t.Fatalf("timeline proofs = %v, want a_green's", step["proofs"])
	}
	for _, leak := range []string{"door_state", "key_log", "TestDoorState"} {
		for surface, text := range map[string]string{
			"the comment":         hub.comments[0],
			"a row":               verdicts[0].Evidence,
			"the timeline line":   string(mustJSON(step)),
			"the card's findings": strings.Join(res.Findings, "\n"),
		} {
			if strings.Contains(text, leak) {
				t.Fatalf("%s carries the hold-out %q: %s", surface, leak, text)
			}
		}
	}
	entries, _ := step["holdouts"].([]any)
	if len(entries) != 2 {
		t.Fatalf("timeline holdouts = %v", step["holdouts"])
	}
	for i, want := range []struct{ id, verdict string }{{"holdout-1", "red"}, {"holdout-2", "green"}} {
		e, _ := entries[i].(map[string]any)
		if e["id"] != want.id || e["verdict"] != want.verdict {
			t.Fatalf("entry %d = %v, want %s %s", i, e, want.id, want.verdict)
		}
	}
}

// A pod that ends before it reaches the hold-outs fails the gate, so the
// activity runs again: an unreached hold-out has no verdict, and the
// settlement never reads one as unrunnable. Nothing lands: no row, no
// comment, no line.
func TestGateCriteriaFailsWhenThePodEndsBeforeTheHoldouts(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, objects)
	seedPlan(t, store, map[string]string{"integ": RoleIntegration},
		`{"id":"a_green","kind":"invariant","suite":"./a"}`)
	if _, err := store.PutPlanHoldouts(context.Background(), "plan1", 1, []Holdout{
		{Idx: 1, Round: 1, ID: "door_state", State: HoldoutStored, DryVerdict: "red", TestName: "TestDoorState", TestCode: "func TestDoorState(t *testing.T) {}"},
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		_, err = runCriteria(t, r)
	}()
	body := proofLines(harness.ProofRun{ID: "a_green", Report: junitGz(t, wrap(`<testcase name="TestA"></testcase>`))})
	scriptReproPods(kube, body[:strings.LastIndex(body, "\n")])
	<-done
	if err == nil || !strings.Contains(err.Error(), "the proofs pod ended early: 1 of 2 entries unreported") {
		t.Fatalf("err = %v, want the early end", err)
	}
	if rows, _ := store.CriteriaVerdicts(context.Background(), "t1"); len(rows) != 0 || len(hub.comments) != 0 ||
		strings.Contains(timeline(t, objects), `"step":"gate_criteria"`) {
		t.Fatalf("rows %+v, comments %q, timeline %s: want nothing landed", rows, hub.comments, timeline(t, objects))
	}
}

// The settlement settles at once: no audit runs, so a history that
// recorded a red hold-out ends its criteria loop. No pod, no token, no
// model call, the row as it was and nothing in the ledger.
func TestSettleHoldoutsSettlesAtOnce(t *testing.T) {
	ctx := context.Background()
	kube := newFakeKube()
	r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, &fakeObjectStore{})
	seedPlan(t, store, map[string]string{"integ": RoleIntegration},
		`{"id":"a_green","kind":"invariant","suite":"./a"}`)
	if _, err := store.PutPlanHoldouts(ctx, "plan1", 1, []Holdout{{Idx: 1, Round: 1, ID: "door_state", State: HoldoutStored,
		DryVerdict: "red", TestName: "TestDoorState", TestCode: "func TestDoorState(t *testing.T) {}"}}); err != nil {
		t.Fatal(err)
	}
	before, _ := store.PlanHoldouts(ctx, "plan1")
	val, err := execActivity(t, r, ActivityHoldoutSettlement, "t1", int64(0), int64(9), "c1",
		[]HoldoutGateResult{{Idx: 1, ID: "door_state", Verdict: "red", Evidence: "Failed --- FAIL: TestDoorState (0.01s)"}})
	if err != nil {
		t.Fatal(err)
	}
	var res HoldoutSettlement
	if err := val.Get(&res); err != nil || !reflect.DeepEqual(res, HoldoutSettlement{Settled: true}) {
		t.Fatalf("settlement = %+v, %v; want settled alone", res, err)
	}
	if len(kube.podOrder) != 0 || len(hub.minted()) != 0 {
		t.Fatalf("pods %d, mints %v; want none", len(kube.podOrder), hub.minted())
	}
	if after, _ := store.PlanHoldouts(ctx, "plan1"); !reflect.DeepEqual(after, before) {
		t.Fatalf("rows = %+v, want %+v", after, before)
	}
	if catches, err := store.PlanCatches(ctx, "plan1"); err != nil || len(catches) != 0 {
		t.Fatalf("catches = %+v, %v; want none", catches, err)
	}
}
