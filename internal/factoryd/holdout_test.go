package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The dry run writes every draft into a scratch package of its own and
// reports one marker line per draft; the heredoc marker carries the
// draft's idx, so a drafted test cannot forge another draft's marker.
func TestHoldoutScriptRunsEachDraftInItsOwnPackage(t *testing.T) {
	drafts := []Holdout{{Idx: 1, TestName: "TestDoorShut", TestCode: "door code"}, {Idx: 2, TestName: "TestKeyLog", TestCode: "log code"}}
	want := `mkdir -p .holdout1
cat > .holdout1/holdout_test.go <<'FACTORYD_HOLDOUT1'
package holdout
door code
FACTORYD_HOLDOUT1
o=/tmp/ho1
go test -v -run '^TestDoorShut$' ./.holdout1 >"$o" 2>&1
rc=$?
ran=$(grep -c '^=== RUN   TestDoorShut$' "$o")
fail=$(grep -c '^--- FAIL: TestDoorShut ' "$o")
ev=$(grep -m1 '^--- FAIL: ' "$o")
[ -n "$ev" ] || ev=$(head -n 1 "$o")
printf '__holdout 1 exit=%s ran=%s fail=%s %s\n' "$rc" "$ran" "$fail" "$ev"
` + `mkdir -p .holdout2
cat > .holdout2/holdout_test.go <<'FACTORYD_HOLDOUT2'
package holdout
log code
FACTORYD_HOLDOUT2
o=/tmp/ho2
go test -v -run '^TestKeyLog$' ./.holdout2 >"$o" 2>&1
rc=$?
ran=$(grep -c '^=== RUN   TestKeyLog$' "$o")
fail=$(grep -c '^--- FAIL: TestKeyLog ' "$o")
ev=$(grep -m1 '^--- FAIL: ' "$o")
[ -n "$ev" ] || ev=$(head -n 1 "$o")
printf '__holdout 2 exit=%s ran=%s fail=%s %s\n' "$rc" "$ran" "$fail" "$ev"
`
	if got := holdoutScript(drafts); got != want {
		t.Fatalf("script = %q, want %q", got, want)
	}
}

// Marker lines parse however the sandbox printed them; the last marker
// of a draft wins and the evidence is optional.
func TestParseHoldoutMarkers(t *testing.T) {
	out := strings.Join([]string{
		"agent-worker: probing",
		"__holdout 1 exit=1 ran=1 fail=1 first try",
		"not a marker: __holdout 9 exit=1 ran=1 fail=1 x",
		"__holdout 1 exit=0 ran=2 fail=0 second try",
		"__holdout 2 exit=2 ran=0 fail=0",
	}, "\n")
	m := parseHoldoutMarkers(out)
	if len(m) != 2 {
		t.Fatalf("markers = %v", m)
	}
	if m[1] != (criterionVerdict{exit: 0, ranCount: 2, failCount: 0, evidence: "second try"}) {
		t.Fatalf("marker 1 = %+v", m[1])
	}
	if m[2] != (criterionVerdict{exit: 2, evidence: ""}) {
		t.Fatalf("marker 2 = %+v", m[2])
	}
	if holdoutRan(m, 3) || !holdoutRan(m, 2) {
		t.Fatalf("holdoutRan: 3 runs, 2 does not; markers %v", m)
	}
}

// The dry verdict, hand-derived one arm at a time: a lost marker is no
// verdict, a failure is red even when nothing ran, a run without the
// test or a bad exit is unrunnable, and long evidence shortens.
func TestHoldoutDryVerdict(t *testing.T) {
	for name, tc := range map[string]struct {
		v       criterionVerdict
		ok      bool
		wantDry string
		wantEv  string
	}{
		"no marker":       {ok: false, wantDry: HoldoutUnrunnable, wantEv: "no verdict from the run"},
		"red":             {v: criterionVerdict{exit: 1, ranCount: 1, failCount: 1, evidence: "--- FAIL: TestX (0.00s)"}, ok: true, wantDry: "red", wantEv: "--- FAIL: TestX (0.00s)"},
		"red without ran": {v: criterionVerdict{exit: 1, ranCount: 0, failCount: 1, evidence: "boom"}, ok: true, wantDry: "red", wantEv: "boom"},
		"never ran":       {v: criterionVerdict{exit: 1, ranCount: 0, failCount: 0, evidence: "no tests to run"}, ok: true, wantDry: HoldoutUnrunnable, wantEv: "no tests to run"},
		"bad exit":        {v: criterionVerdict{exit: 2, ranCount: 1, failCount: 0, evidence: "exit status 2"}, ok: true, wantDry: HoldoutUnrunnable, wantEv: "exit status 2"},
		"green":           {v: criterionVerdict{exit: 0, ranCount: 1}, ok: true, wantDry: "green"},
		"long evidence":   {v: criterionVerdict{exit: 1, ranCount: 1, failCount: 1, evidence: strings.Repeat("a", 250)}, ok: true, wantDry: "red", wantEv: strings.Repeat("a", 200) + "..."},
	} {
		t.Run(name, func(t *testing.T) {
			dry, ev := holdoutDryVerdict(tc.v, tc.ok)
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
		Process: "maintenance", RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 2}
}

// askHoldouts runs the whole round: the review model proposes two
// hold-outs beside the project's past failures, the drafts dry-run on
// the main head, the one that cannot run is rewritten once and runs
// again, and the round lands proposed with its verdicts. A rewritten
// draft never lands without its own run.
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
		Criteria: []PlanCriterion{{ID: "green_login", Kind: "test", Test: "TestLogin in ./internal/auth"}}}
	done := make(chan error, 1)
	go func() {
		done <- r.askHoldouts(context.Background(), holdoutPlanTask(), 1, p)
	}()
	scriptReproPods(kube,
		reproLine(0, "__holdout 1 exit=1 ran=1 fail=1 --- FAIL: TestDoorShut (0.00s)\n__holdout 2 exit=1 ran=0 fail=0 no tests to run"),
		reproLine(0, "__holdout 2 exit=1 ran=1 fail=1 --- FAIL: TestKeyLog (0.00s)"))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	rows, err := store.PlanHoldouts(context.Background(), "plan1")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	want1 := Holdout{Idx: 1, Round: 1, ID: "door_state", Origin: HoldoutDrafted,
		Criterion: "The door stays shut until a key that fits turns the lock", State: HoldoutProposed,
		DryHead: "base", DryVerdict: "red", DryOutput: "--- FAIL: TestDoorShut (0.00s)",
		TestName: "TestDoorShut", TestCode: "func TestDoorShut(t *testing.T) {\n\tt.Fatal(\"shut\")\n}"}
	want2 := Holdout{Idx: 2, Round: 1, ID: "key_log", Origin: HoldoutMechanical,
		Criterion: "Every key turn lands in the log", State: HoldoutProposed,
		DryHead: "base", DryVerdict: "red", DryOutput: "--- FAIL: TestKeyLog (0.00s)",
		TestName: "TestKeyLog", TestCode: "func TestKeyLog(t *testing.T) {\n\tt.Fatal(\"log\")\n}"}
	if rows[0] != want1 || rows[1] != want2 {
		t.Fatalf("rows =\n%+v\n%+v\nwant\n%+v\n%+v", rows[0], rows[1], want1, want2)
	}
	if len(kube.podOrder) != 2 {
		t.Fatalf("pods %d, want the dry run and the corrective one", len(kube.podOrder))
	}
	if mints := hub.minted(); len(mints) != 0 {
		t.Fatalf("a dry run mints nothing, minted %v", mints)
	}
	if len(ll.chats) != 2 || ll.chats[0]["model"] != "review-frontier" || !slices.Equal(ll.purposes, []string{"review", "review"}) {
		t.Fatalf("chats %v, purposes %v", ll.chats, ll.purposes)
	}
	first, second := userMessage(ll.chats[0]), userMessage(ll.chats[1])
	for _, want := range []string{"the slug broke on BAD000", "green_login", "The door opens only for a key that fits the lock"} {
		if !strings.Contains(first, want) {
			t.Fatalf("the proposal call lacks %q: %s", want, first)
		}
	}
	for _, want := range []string{"TestKeyLog", "no tests to run"} {
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
		if err := DecideHoldout(ctx, store, "t-plan", 1, 1, "okay", "sebastian"); err == nil || err.Error() != "task t-plan has no plan waiting for acceptance" {
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
		if err := store.AnswerPlanQuestion(ctx, "t-plan", 1, 1, "confirm", "sebastian"); err != nil {
			t.Fatal(err)
		}
		if err := DecideHoldout(ctx, store, "t-plan", 1, 1, "okay", "sebastian"); !errors.Is(err, ErrAnswered) {
			t.Fatalf("after the confirmation: %v", err)
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

// The audit's filing of its verdict, one refused arm at a time: only
// the audit of that hold-out running on that task files, once; a
// rewrite keeps its test's name, stays inside the bounds and quotes
// the spec's words; a real gap or a spec question files no code.
func TestFileVerdict(t *testing.T) {
	ctx := context.Background()
	card := seedCriteriaPlan(t)
	card.Process, card.ModelAlias = "maintenance", "factory/coding-cheap"
	r, _, store := newTestRunner(t, card, newFakeKube(), &fakeObjectStore{})
	plan := `{"summary":"s","spec":{"outcome":"The door opens only for a key that fits the lock","glossary":["the lock"]},` +
		`"criteria":[{"id":"green_login","kind":"test","test":"TestLogin in ./internal/auth"}],` +
		`"cards":[{"key":"integ","role":"integration","title":"t","brief":"b"}]}`
	if _, err := store.AddPlanRevision(ctx, "plan1", json.RawMessage(plan), "sha1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutPlanHoldouts(ctx, "plan1", 1, []Holdout{{Idx: 1, Round: 1, ID: "door_state",
		State: HoldoutStored, DryVerdict: "red", TestName: "TestDoorShut", TestCode: "orig"}}); err != nil {
		t.Fatal(err)
	}
	r.startAudit("t1", 1)
	if findings, err := r.FileVerdict(ctx, HoldoutVerdict{TaskID: "t1", Idx: 9, Verdict: AuditWrongTest}); err != nil ||
		!slices.Equal(findings, []string{"file_verdict: the card holds no such hold-out"}) {
		t.Fatalf("missing row: %v, %q", err, findings)
	}
	for name, in := range map[string]HoldoutVerdict{
		"no such verdict":                 {TaskID: "t1", Idx: 1, Verdict: "nope"},
		"a gap files no code":             {TaskID: "t1", Idx: 1, Verdict: AuditRealGap, TestCode: "func X()"},
		"a question carries its question": {TaskID: "t1", Idx: 1, Verdict: AuditSpecQuestion},
		"the reason is too long":          {TaskID: "t1", Idx: 1, Verdict: AuditRealGap, Reason: strings.Repeat("r", 401)},
		"the rewrite keeps its name":      {TaskID: "t1", Idx: 1, Verdict: AuditWrongTest, TestCode: "func TestOther(t *testing.T) {}", Quote: "The door opens only for a key that fits"},
		"the quote is short":              {TaskID: "t1", Idx: 1, Verdict: AuditWrongTest, TestCode: "func TestDoorShut(t *testing.T) {}", Quote: "too short"},
		"the quote is not the spec's":     {TaskID: "t1", Idx: 1, Verdict: AuditWrongTest, TestCode: "func TestDoorShut(t *testing.T) {}", Quote: "a sentence the spec never says out loud"},
	} {
		t.Run(name, func(t *testing.T) {
			findings, err := r.FileVerdict(ctx, in)
			if err != nil || len(findings) != 1 || !strings.HasPrefix(findings[0], "file_verdict: ") {
				t.Fatalf("findings = %q, %v", findings, err)
			}
			rows, _ := store.PlanHoldouts(ctx, "plan1")
			if rows[0].AuditVerdict != "" {
				t.Fatalf("a refused filing changed the row: %+v", rows[0])
			}
		})
	}
	rewrite := "func TestDoorShut(t *testing.T) {\n\tt.Fatal(\"locked\")\n}"
	if findings, err := r.FileVerdict(ctx, HoldoutVerdict{TaskID: "t1", Idx: 1, Verdict: AuditWrongTest,
		Reason: "tests the hinge, not the lock", Quote: "The door opens only for a key that fits", TestCode: rewrite}); err != nil || len(findings) != 0 {
		t.Fatalf("the filing: %v, %q", err, findings)
	}
	rows, _ := store.PlanHoldouts(ctx, "plan1")
	if rows[0].AuditVerdict != AuditWrongTest || rows[0].AuditReason != "tests the hinge, not the lock" ||
		rows[0].Rewrite != rewrite || rows[0].State != HoldoutStored {
		t.Fatalf("row = %+v", rows[0])
	}
	if _, err := r.FileVerdict(ctx, HoldoutVerdict{TaskID: "t1", Idx: 1, Verdict: AuditRealGap, Reason: "again"}); !errors.Is(err, ErrAnswered) {
		t.Fatalf("a second filing: %v", err)
	}
	r.endAudit("t1")
	if findings, err := r.FileVerdict(ctx, HoldoutVerdict{TaskID: "t1", Idx: 1, Verdict: AuditRealGap, Reason: "no audit"}); err != nil ||
		!slices.Equal(findings, []string{"file_verdict: no audit of this hold-out is running"}) {
		t.Fatalf("no live audit: %v, %q", err, findings)
	}
}

// The gateway's way in mirrors the plan filer's: a broken body or a
// nameless filing is the gateway's 400, findings answer what the agent
// corrects, and a filed verdict answers filed true.
func TestAuditVerdictHandler(t *testing.T) {
	card := seedCriteriaPlan(t)
	card.Process = "maintenance"
	r, _, _ := newTestRunner(t, card, newFakeKube(), &fakeObjectStore{})
	post := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		r.AuditVerdictHandler(rr, httptest.NewRequest("POST", "/internal/holdout-verdict", strings.NewReader(body)))
		return rr
	}
	if rr := post(t, "{"); rr.Code != 400 || !strings.Contains(rr.Body.String(), "holdout verdict: ") {
		t.Fatalf("broken body: %d %s", rr.Code, rr.Body)
	}
	if rr := post(t, `{"task_id":"t1"}`); rr.Code != 400 || !strings.Contains(rr.Body.String(), "task_id and idx are required") {
		t.Fatalf("nameless filing: %d %s", rr.Code, rr.Body)
	}
	rr := post(t, `{"task_id":"t1","idx":1,"verdict":"nope"}`)
	if rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != `{"filed":false,"findings":["file_verdict: the verdict is one of wrong_test, real_gap, spec_question"]}` {
		t.Fatalf("findings: %d %s", rr.Code, rr.Body)
	}
}

// The confirming answer waits for the round's hold-out clicks; a normal
// question's answer does not. Once every proposal is decided the
// confirmation answer lands, sweeps stragglers of older rounds and
// signals the planner.
func TestAnswerPlanWaitsForTheHoldoutDecisions(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.tasks["t-plan"] = Task{ID: "t-plan", Kind: "agent", Status: "waiting-human", Lane: "plan", IssueNumber: 20, Repository: "linkshort/linkshort"}
	raw, sha, _ := planSHA(questionPlan())
	store.AddPlanRevision(ctx, "t-plan", raw, sha)
	store.PutPlanQuestions(ctx, "t-plan", 1, questionPlan().Questions)
	if _, _, err := store.PutPlanConfirmation(ctx, "t-plan", 1, PlanQuestion{Text: "Does the plan hold against the spec?", Choices: []string{"confirm", "revise"}}); err != nil {
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
	if err := AnswerPlan(ctx, store, wf, "t-plan", 1, 1, "60 seconds", "alice"); err != nil {
		t.Fatalf("a normal answer waits on nothing: %v", err)
	}
	if err := AnswerPlan(ctx, store, wf, "t-plan", 1, 3, "confirm", "sebastian"); !errors.Is(err, ErrHoldoutsOpen) {
		t.Fatalf("the confirming answer: %v", err)
	}
	if err := DecideHoldout(ctx, store, "t-plan", 1, 1, "okay", "sebastian"); err != nil {
		t.Fatal(err)
	}
	if err := AnswerPlan(ctx, store, wf, "t-plan", 1, 3, "confirm", "sebastian"); err != nil {
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
// criteria, blind to the agent: a red one reaches the settlement as a
// Holdout, never the card's findings or comment; a green one rides
// silently.
func TestGateCriteriaRunsTheStoredHoldoutsBlindToTheAgent(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, objects)
	seedPlan(t, store, map[string]string{"integ": RoleIntegration},
		`{"id":"a_green","kind":"test","test":"TestA in ./a"}`)
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
	scriptReproPods(kube, reproLine(0, strings.Join([]string{
		"__criterion a_green exit=0 ran=1 fail=0 --- PASS: TestA",
		"__holdout 1 exit=1 ran=1 fail=1 --- FAIL: TestDoorState (0.01s)",
		"__holdout 2 exit=0 ran=1 fail=0",
	}, "\n")))
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 0 || res.Skipped != "" || res.Escalate != "" {
		t.Fatalf("result = %+v, want the criteria met", res)
	}
	wantHoldouts := []HoldoutGateResult{{Idx: 1, ID: "door_state", Verdict: "red", Evidence: "--- FAIL: TestDoorState (0.01s)"}}
	if !slices.Equal(res.Holdouts, wantHoldouts) {
		t.Fatalf("holdouts = %+v, want %+v", res.Holdouts, wantHoldouts)
	}
	if len(kube.podOrder) != 1 || len(hub.minted()) != 1 {
		t.Fatalf("pods %d, mints %v", len(kube.podOrder), hub.minted())
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "1 of 1 criteria met") {
		t.Fatalf("comments = %q", hub.comments)
	}
	for _, leak := range []string{"door_state", "key_log", "TestDoorState"} {
		if strings.Contains(hub.comments[0], leak) {
			t.Fatalf("the comment carries the hold-out %q: %s", leak, hub.comments[0])
		}
	}
	step := timelineStep(t, objects, "t1", "gate_criteria")
	entries, _ := step["holdouts"].([]any)
	if len(entries) != 2 {
		t.Fatalf("timeline holdouts = %v", step["holdouts"])
	}
	for i, want := range []struct{ id, verdict string }{{"door_state", "red"}, {"key_log", "green"}} {
		e, _ := entries[i].(map[string]any)
		if e["id"] != want.id || e["verdict"] != want.verdict {
			t.Fatalf("entry %d = %v, want %s %s", i, e, want.id, want.verdict)
		}
	}
}

// settleRig is the settlement's rig: the integration card of a plan
// whose spec the audits quote, with the plan's hold-out rows seeded.
func settleRig(t *testing.T, rows ...Holdout) (*Runner, *fakeKube, *fakeStore) {
	t.Helper()
	kube := newFakeKube()
	card := seedCriteriaPlan(t)
	card.Process, card.ModelAlias = "maintenance", "factory/coding-cheap"
	r, _, store := newTestRunner(t, card, kube, &fakeObjectStore{})
	plan := `{"summary":"s","spec":{"outcome":"The door opens only for a key that fits the lock","glossary":["the lock"]},` +
		`"criteria":[{"id":"green_login","kind":"test","test":"TestLogin in ./internal/auth"}],` +
		`"cards":[{"key":"integ","role":"integration","title":"t","brief":"b"}]}`
	if _, err := store.AddPlanRevision(context.Background(), "plan1", json.RawMessage(plan), "sha1"); err != nil {
		t.Fatal(err)
	}
	if len(rows) > 0 {
		if _, err := store.PutPlanHoldouts(context.Background(), "plan1", 1, rows); err != nil {
			t.Fatal(err)
		}
	}
	return r, kube, store
}

// sessionLog is what an audit session's sandbox answers: it finished
// green with its summary.
func sessionLog(summary string) string {
	return envLine(harness.Event{Kind: harness.EventFinished, Time: time.Now(), Detail: map[string]any{"success": true, "summary": summary}}) +
		"\n" + resultLine(true, summary)
}

// settle runs the settlement activity, files the audit's verdict while
// its session runs and answers its pods in order.
func settle(t *testing.T, r *Runner, kube *fakeKube, reds []HoldoutGateResult, verdict *HoldoutVerdict, bodies ...string) (HoldoutSettlement, error) {
	t.Helper()
	done := make(chan error, 1)
	var res HoldoutSettlement
	go func() {
		val, err := execActivity(t, r, ActivityHoldoutSettlement, "t1", int64(7), int64(9), "c1", reds)
		if err == nil {
			err = val.Get(&res)
		}
		done <- err
	}()
	if len(bodies) > 0 {
		awaitPod(t, kube, done)
		if verdict != nil {
			if findings, err := r.FileVerdict(context.Background(), *verdict); err != nil || len(findings) != 0 {
				t.Fatalf("the audit's filing: %v, %q", err, findings)
			}
		}
		kube.scriptPods(0, bodies[0])
	}
	if len(bodies) > 1 {
		awaitPodN(t, kube, 1, done)
		kube.scriptPods(1, bodies[1])
	}
	return res, <-done
}

// awaitPodN waits for the kube's pod i, or fails when the activity
// ends first.
func awaitPodN(t *testing.T, kube *fakeKube, i int, done chan error) {
	t.Helper()
	for {
		kube.mu.Lock()
		n := len(kube.podOrder)
		kube.mu.Unlock()
		if n > i {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("the activity ended before pod %d: %v", i, err)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// A wrong test with no rewrite drops at the settlement: the red lands
// in the ledger, the row keeps the audit's verdict and the card
// settles. The audit session is a report-mode sandbox that changes no
// code and pushes nothing.
func TestSettleHoldoutsDropsAWrongTest(t *testing.T) {
	row := Holdout{Idx: 1, Round: 1, ID: "door_state", Criterion: "The door stays shut until a key that fits turns the lock",
		State: HoldoutStored, DryVerdict: "red", DryHead: "base", TestName: "TestDoorShut", TestCode: "orig"}
	r, kube, store := settleRig(t, row)
	reds := []HoldoutGateResult{{Idx: 1, ID: "door_state", Verdict: "red", Evidence: "--- FAIL: TestDoorShut (0.01s)"}}
	res, err := settle(t, r, kube, reds,
		&HoldoutVerdict{TaskID: "t1", Idx: 1, Verdict: AuditWrongTest, Reason: "tests the hinge, not the lock"},
		sessionLog("the test names the hinge"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Settled || res.Ask != "" || len(res.Findings) != 0 {
		t.Fatalf("settlement = %+v", res)
	}
	if got := store.catches["plan1 1 "+CatchHoldoutRed+" "+FinderGate]; !slices.Equal(got,
		[]string{"hold-out door_state red at the gate: --- FAIL: TestDoorShut (0.01s)"}) {
		t.Fatalf("ledger = %q", got)
	}
	rows, _ := store.PlanHoldouts(context.Background(), "plan1")
	if rows[0].State != HoldoutDropped || rows[0].AuditVerdict != AuditWrongTest || rows[0].AuditReason != "tests the hinge, not the lock" {
		t.Fatalf("row = %+v", rows[0])
	}
	env := podEnv(kube.podAt(0))
	if env["MODE"] != "report" || !strings.Contains(env["REPORT_GUIDE"], "Hold-out idx 1 (file_verdict names it): The door stays shut until a key that fits turns the lock") ||
		!strings.Contains(env["REPORT_GUIDE"], "The test (dry-run on main head base: red):") {
		t.Fatalf("the audit session's env = %v", env)
	}
}

// A rewrite that holds replaces the hold-out's test: it dry-runs on the
// base head, red means it still tests the feature's absence, and the
// row carries it with its verdict.
func TestSettleHoldoutsKeepsARewriteThatHolds(t *testing.T) {
	row := Holdout{Idx: 1, Round: 1, ID: "door_state", Criterion: "The door stays shut until a key that fits turns the lock",
		State: HoldoutStored, DryVerdict: "red", DryHead: "base", TestName: "TestDoorShut", TestCode: "orig"}
	r, kube, store := settleRig(t, row)
	reds := []HoldoutGateResult{{Idx: 1, ID: "door_state", Verdict: "red", Evidence: "--- FAIL: TestDoorShut (0.01s)"}}
	rewrite := "func TestDoorShut(t *testing.T) {\n\tt.Fatal(\"locked\")\n}"
	res, err := settle(t, r, kube, reds,
		&HoldoutVerdict{TaskID: "t1", Idx: 1, Verdict: AuditWrongTest, Reason: "tests the hinge, not the lock",
			Quote: "The door opens only for a key that fits", TestCode: rewrite},
		sessionLog("audited"),
		reproLine(0, "__holdout 1 exit=1 ran=1 fail=1 --- FAIL: TestDoorShut (0.00s)"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Settled {
		t.Fatalf("settlement = %+v", res)
	}
	rows, _ := store.PlanHoldouts(context.Background(), "plan1")
	if rows[0].TestCode != rewrite || rows[0].DryVerdict != "red" || rows[0].DryOutput != "--- FAIL: TestDoorShut (0.00s)" ||
		rows[0].AuditVerdict != "" || rows[0].Rewrite != "" || rows[0].State != HoldoutStored {
		t.Fatalf("row = %+v", rows[0])
	}
}

// A rewrite that passes on the base head bends: it tests nothing. The
// audit's fields clear, the hold-out keeps its own test and the human
// is asked.
func TestSettleHoldoutsRefusesARewriteThatBends(t *testing.T) {
	row := Holdout{Idx: 1, Round: 1, ID: "door_state", Criterion: "The door stays shut until a key that fits turns the lock",
		State: HoldoutStored, DryVerdict: "red", DryHead: "base", TestName: "TestDoorShut", TestCode: "orig"}
	r, kube, store := settleRig(t, row)
	reds := []HoldoutGateResult{{Idx: 1, ID: "door_state", Verdict: "red", Evidence: "--- FAIL: TestDoorShut (0.01s)"}}
	res, err := settle(t, r, kube, reds,
		&HoldoutVerdict{TaskID: "t1", Idx: 1, Verdict: AuditWrongTest, Reason: "tests the hinge, not the lock",
			Quote: "The door opens only for a key that fits", TestCode: "func TestDoorShut(t *testing.T) {}"},
		sessionLog("audited"),
		reproLine(0, "__holdout 1 exit=0 ran=1 fail=0"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Settled || !strings.Contains(res.Ask, "the audit's rewrite of hold-out door_state green on the base head") ||
		!strings.Contains(res.Ask, "the hold-out keeps its own test") {
		t.Fatalf("settlement = %+v", res)
	}
	rows, _ := store.PlanHoldouts(context.Background(), "plan1")
	if rows[0].TestCode != "orig" || rows[0].AuditVerdict != "" || rows[0].State != HoldoutStored {
		t.Fatalf("row = %+v", rows[0])
	}
}

// A real gap opens the criterion: it becomes public as a finding for
// the builder, the audit lands in the ledger and the round's close
// waits for the card to work it.
func TestSettleHoldoutsOpensARealGap(t *testing.T) {
	row := Holdout{Idx: 1, Round: 1, ID: "key_log", Criterion: "Every key turn lands in the log",
		State: HoldoutStored, DryVerdict: "red", DryHead: "base", TestName: "TestKeyLog", TestCode: "orig"}
	r, kube, store := settleRig(t, row)
	reds := []HoldoutGateResult{{Idx: 1, ID: "key_log", Verdict: "red", Evidence: "--- FAIL: TestKeyLog (0.01s)"}}
	res, err := settle(t, r, kube, reds,
		&HoldoutVerdict{TaskID: "t1", Idx: 1, Verdict: AuditRealGap, Reason: "the log never lands"},
		sessionLog("audited"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Settled || res.Ask != "" || !slices.Equal(res.Findings, []string{
		"Every key turn lands in the log (hold-out key_log): the audit found a real gap the spec names and the plan's proof does not cover; work the criterion."}) {
		t.Fatalf("settlement = %+v", res)
	}
	if got := store.catches["plan1 1 "+CatchAudit+" "+FinderAuditor]; !slices.Equal(got,
		[]string{"hold-out key_log: the audit found a real gap: the log never lands"}) {
		t.Fatalf("ledger = %q", got)
	}
	rows, _ := store.PlanHoldouts(context.Background(), "plan1")
	if rows[0].State != HoldoutOpened || rows[0].AuditVerdict != AuditRealGap {
		t.Fatalf("row = %+v", rows[0])
	}
}

// A spec question goes to the human verbatim; the answer starts a new
// planner round or runs the gate again.
func TestSettleHoldoutsAsksTheSpecQuestion(t *testing.T) {
	row := Holdout{Idx: 1, Round: 1, ID: "door_state", Criterion: "The door stays shut until a key that fits turns the lock",
		State: HoldoutStored, DryVerdict: "red", DryHead: "base", TestName: "TestDoorShut", TestCode: "orig"}
	r, kube, store := settleRig(t, row)
	reds := []HoldoutGateResult{{Idx: 1, ID: "door_state", Verdict: "red", Evidence: "--- FAIL: TestDoorShut (0.01s)"}}
	res, err := settle(t, r, kube, reds,
		&HoldoutVerdict{TaskID: "t1", Idx: 1, Verdict: AuditSpecQuestion, Reason: "must the log keep the key id?"},
		sessionLog("audited"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Settled || !strings.Contains(res.Ask, "hold-out door_state asks a question the spec cannot settle: must the log keep the key id?") {
		t.Fatalf("settlement = %+v", res)
	}
	rows, _ := store.PlanHoldouts(context.Background(), "plan1")
	if rows[0].State != HoldoutStored || rows[0].AuditVerdict != AuditSpecQuestion {
		t.Fatalf("row = %+v", rows[0])
	}
}

// A hold-out the audit opened that stays red at the gate asks the
// human: the cycle counts on the row and lands in the ledger, and no
// second audit runs.
func TestSettleHoldoutsAsksTheHumanWhenAnOpenedHoldoutStaysRed(t *testing.T) {
	row := Holdout{Idx: 1, Round: 1, ID: "door_state", Criterion: "The door stays shut until a key that fits turns the lock",
		State: HoldoutOpened, DryVerdict: "red", DryHead: "base", TestName: "TestDoorShut", TestCode: "orig"}
	r, kube, store := settleRig(t, row)
	reds := []HoldoutGateResult{{Idx: 1, ID: "door_state", Verdict: "red", Evidence: "--- FAIL: TestDoorShut (0.01s)"}}
	res, err := settle(t, r, kube, reds, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Settled || !strings.Contains(res.Ask, "hold-out door_state (The door stays shut until a key that fits turns the lock) stayed red after its criterion was opened") {
		t.Fatalf("settlement = %+v", res)
	}
	if got := store.catches["plan1 1 "+CatchHoldoutRed+" "+FinderGate]; !slices.Equal(got,
		[]string{"hold-out door_state red at the gate again: --- FAIL: TestDoorShut (0.01s)"}) {
		t.Fatalf("ledger = %q", got)
	}
	rows, _ := store.PlanHoldouts(context.Background(), "plan1")
	if rows[0].RedCycles != 1 || rows[0].State != HoldoutOpened {
		t.Fatalf("row = %+v", rows[0])
	}
	if len(kube.podOrder) != 0 {
		t.Fatalf("pods %d, want no audit of an opened hold-out", len(kube.podOrder))
	}
}

// An audit session that ends without filing a verdict asks the human
// with its own words; the hold-out stands as it was.
func TestSettleHoldoutsEscalatesAnAuditWithoutAVerdict(t *testing.T) {
	row := Holdout{Idx: 1, Round: 1, ID: "door_state", Criterion: "The door stays shut until a key that fits turns the lock",
		State: HoldoutStored, DryVerdict: "red", DryHead: "base", TestName: "TestDoorShut", TestCode: "orig"}
	r, kube, store := settleRig(t, row)
	reds := []HoldoutGateResult{{Idx: 1, ID: "door_state", Verdict: "red", Evidence: "--- FAIL: TestDoorShut (0.01s)"}}
	res, err := settle(t, r, kube, reds, nil, sessionLog("the test names the hinge, not the lock"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Settled || !strings.Contains(res.Ask, "the audit session of hold-out door_state ended without a verdict: the test names the hinge, not the lock") {
		t.Fatalf("settlement = %+v", res)
	}
	rows, _ := store.PlanHoldouts(context.Background(), "plan1")
	if rows[0].State != HoldoutStored || rows[0].AuditVerdict != "" {
		t.Fatalf("row = %+v", rows[0])
	}
}
