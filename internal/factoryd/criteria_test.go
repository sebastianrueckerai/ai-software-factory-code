package factoryd

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// The gate's split of a plan's criteria (piece 4): kind test and kind
// invariant parse into runs, judgement and human criteria are outside the
// gate, and a field off the house format is a finding, never a run.
func TestCriteriaRunsSplitsKindsAndOffFormatFields(t *testing.T) {
	p := Plan{Criteria: []PlanCriterion{
		{ID: "green_login", Kind: CriterionTest, Test: "TestLogin in ./internal/auth"},
		{ID: "no_format", Kind: CriterionTest, Test: "a login test"},
		{ID: "all_green", Kind: CriterionInvariant, Test: "go test ./... passes"},
		{ID: "no_suite", Kind: CriterionInvariant, Test: "the app stays green"},
		{ID: "judged", Kind: CriterionJudgement, Claim: "the page reads well"},
		{ID: "human_eye", Kind: CriterionHuman, Claim: "the copy is true"},
	}}
	runs, findings := criteriaRuns(p)
	want := []criterionRun{
		{id: "green_login", test: "TestLogin", pkg: "./internal/auth"},
		{id: "all_green", suite: "./..."},
	}
	if !slices.Equal(runs, want) {
		t.Fatalf("runs = %+v, want %+v", runs, want)
	}
	wantFindings := []string{
		`criterion no_format: test "a login test" is not TestXxx in <package>`,
		`criterion no_suite: suite "the app stays green" is not go test <suite> passes`,
	}
	if !slices.Equal(findings, wantFindings) {
		t.Fatalf("findings = %q, want %q", findings, wantFindings)
	}
}

// The script the sandbox runs: one go test per criterion in the plan's
// order, every output to its own file, the marker line the only stdout.
// The printf quotes its arguments, so test output cannot word-split.
func TestCriteriaScriptRunsEachCriterionInPlanOrder(t *testing.T) {
	got := criteriaScript([]criterionRun{
		{id: "green_login", test: "TestLogin", pkg: "./internal/auth"},
		{id: "all_green", suite: "./..."},
	})
	want := "o=/tmp/k1\n" +
		"go test -v -run '^TestLogin$' ./internal/auth >\"$o\" 2>&1\n" +
		"rc=$?\n" +
		"ran=$(grep -c '^=== RUN   TestLogin$' \"$o\")\n" +
		"fail=$(grep -c '^--- FAIL: TestLogin ' \"$o\")\n" +
		"ev=$(grep -m1 '^--- FAIL: ' \"$o\")\n" +
		"[ -n \"$ev\" ] || ev=$(head -n 1 \"$o\")\n" +
		"printf '__criterion green_login exit=%s ran=%s fail=%s %s\\n' \"$rc\" \"$ran\" \"$fail\" \"$ev\"\n" +
		"o=/tmp/k2\n" +
		"go test ./... >\"$o\" 2>&1\n" +
		"rc=$?\n" +
		"ran=$(grep -c '^ok' \"$o\")\n" +
		"fail=$(grep -c '^FAIL' \"$o\")\n" +
		"ev=$(grep -m1 '^--- FAIL: ' \"$o\")\n" +
		"[ -n \"$ev\" ] || ev=$(head -n 1 \"$o\")\n" +
		"printf '__criterion all_green exit=%s ran=%s fail=%s %s\\n' \"$rc\" \"$ran\" \"$fail\" \"$ev\"\n"
	if got != want {
		t.Fatalf("script =\n%s\nwant\n%s", got, want)
	}
}

// The marker parser: one entry per criterion id, the last line of one
// winning, and nothing else on stdout read. A forged marker inside a
// criterion's evidence stays evidence: the fields parse positionally, and
// an id needs its house charset, two characters at least.
func TestParseCriteriaMarkers(t *testing.T) {
	forged := "__criterion a2 exit=1 ran=1 fail=1 x"
	got := parseCriteriaMarkers(strings.Join([]string{
		"agent-worker: probing",
		"__criterion a1 exit=0 ran=1 fail=0 --- PASS: TestA",
		"__criterion b1 exit=1 ran=2 fail=1 --- FAIL: TestB",
		"__criterion b1 exit=0 ran=2 fail=0 ok",
		"__criterion a1 exit=0 ran=9 fail=9 noise " + forged,
		"__criterion A1 exit=0 ran=1 fail=0 uppercase id",
		"__criterion a exit=0 ran=1 fail=0 one character id",
		"__criterion c1 exit no fields",
	}, "\n"))
	want := map[string]criterionVerdict{
		"a1": {exit: 0, ranCount: 9, failCount: 9, evidence: "noise " + forged},
		"b1": {exit: 0, ranCount: 2, failCount: 0, evidence: "ok"},
	}
	if len(got) != len(want) {
		t.Fatalf("markers = %+v, want %+v", got, want)
	}
	for id, v := range want {
		if got[id] != v {
			t.Fatalf("marker %s = %+v, want %+v", id, got[id], v)
		}
	}
}

// The report's verdicts, hand-derived one per arm: a lost marker is no
// verdict (a gate that runs and reports nothing does not pass), a suite
// needs its tests and a green exit, a test needs its run count, its
// failures and a green exit, and long evidence shortens to 200 characters.
func TestCriteriaReportVerdicts(t *testing.T) {
	runs := []criterionRun{
		{id: "t_pass", test: "TestPass", pkg: "./p"},
		{id: "t_nothing", test: "TestNothing", pkg: "./p"},
		{id: "t_fail", test: "TestFail", pkg: "./p"},
		{id: "t_exit", test: "TestExit", pkg: "./p"},
		{id: "t_lost", test: "TestLost", pkg: "./p"},
		{id: "t_long", test: "TestLong", pkg: "./p"},
		{id: "i_pass", suite: "./..."},
		{id: "i_fail", suite: "./x"},
		{id: "i_empty", suite: "./empty"},
	}
	long := strings.Repeat("a", 250)
	markers := map[string]criterionVerdict{
		"t_pass":    {exit: 0, ranCount: 1, failCount: 0},
		"t_nothing": {exit: 0, ranCount: 0, failCount: 0},
		"t_fail":    {exit: 1, ranCount: 1, failCount: 1, evidence: "--- FAIL: TestFail (0.00s)"},
		"t_exit":    {exit: 2, ranCount: 1, failCount: 0, evidence: "exit status 2"},
		"t_long":    {exit: 1, ranCount: 1, failCount: 1, evidence: long},
		"i_pass":    {exit: 0, ranCount: 3, failCount: 0},
		"i_fail":    {exit: 1, ranCount: 1, failCount: 1, evidence: "FAIL ./x"},
		"i_empty":   {exit: 0, ranCount: 0, failCount: 0},
	}
	findings, lines, entries := criteriaReport(runs, markers)
	wantLines := []string{
		"criterion t_pass: TestPass in ./p passed",
		"criterion t_nothing: no test TestNothing in ./p",
		"criterion t_fail: TestFail in ./p failed: --- FAIL: TestFail (0.00s)",
		"criterion t_exit: TestExit in ./p did not run green (exit 2): exit status 2",
		"criterion t_lost: no verdict from the run",
		"criterion t_long: TestLong in ./p failed: " + strings.Repeat("a", 200) + "...",
		"criterion i_pass: suite ./... passed",
		"criterion i_fail: suite ./x failed: FAIL ./x",
		"criterion i_empty: suite ./empty ran no tests",
	}
	if !slices.Equal(lines, wantLines) {
		t.Fatalf("lines = %q, want %q", lines, wantLines)
	}
	wantFindings := []string{wantLines[1], wantLines[2], wantLines[3], wantLines[4], wantLines[5], wantLines[7], wantLines[8]}
	if !slices.Equal(findings, wantFindings) {
		t.Fatalf("findings = %q, want %q", findings, wantFindings)
	}
	wantVerdicts := []string{"passed", "no test", "failed", "failed", "no verdict", "failed", "passed", "failed", "no tests"}
	for i, v := range wantVerdicts {
		if entries[i]["verdict"] != v {
			t.Fatalf("entry %d = %v, want verdict %q", i, entries[i], v)
		}
	}
}

// The plan the gate reads: one integration card, criteria of every kind.
func seedCriteriaPlan(t *testing.T) Task {
	t.Helper()
	task := gateTask
	task.Source, task.Parent, task.PlanKey = SourcePlan, "plan1", "integ"
	return task
}

// seedPlan files parent's accepted plan revision: cards with the given
// roles, and criteria verbatim.
func seedPlan(t *testing.T, store *fakeStore, roles map[string]string, criteria string) {
	t.Helper()
	cards := ""
	for key, role := range roles {
		if cards != "" {
			cards += ","
		}
		cards += `{"key":"` + key + `","role":"` + role + `","title":"t","brief":"b"}`
	}
	plan := `{"summary":"s","criteria":[` + criteria + `],"cards":[` + cards + `]}`
	if _, err := store.AddPlanRevision(context.Background(), "plan1", json.RawMessage(plan), "sha1"); err != nil {
		t.Fatal(err)
	}
}

// A task no plan filed skips the gate: no pod, no token, the timeline
// says why.
func TestGateCriteriaSkipsWithoutAPlan(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, _ := newTestRunner(t, gateTask, kube, objects)
	val, err := execActivity(t, r, ActivityGateCriteria, "t1", int64(9), "c1")
	res := getInto[CriteriaResult](t, val, err)
	if res.Skipped != "no plan filed" || len(res.Findings) != 0 || res.Escalate != "" || len(kube.podOrder) != 0 || len(hub.minted()) != 0 || len(hub.comments) != 0 {
		t.Fatalf("result = %+v, pods %d, mints %v, comments %q", res, len(kube.podOrder), hub.minted(), hub.comments)
	}
	if tl := timeline(t, objects); !strings.Contains(tl, `"step":"gate_criteria"`) || !strings.Contains(tl, `"skipped":"no plan filed"`) {
		t.Fatalf("timeline = %s", tl)
	}
}

// A card that is not the plan's integration card skips the gate: the
// gate runs once, at the end, on one card.
func TestGateCriteriaSkipsOffTheIntegrationCard(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	task := seedCriteriaPlan(t)
	task.PlanKey = "feat1"
	r, _, store := newTestRunner(t, task, kube, objects)
	seedPlan(t, store, map[string]string{"feat1": RoleFeature, "integ": RoleIntegration},
		`{"id":"a_green","kind":"test","test":"TestA in ./a"}`)
	val, err := execActivity(t, r, ActivityGateCriteria, "t1", int64(9), "c1")
	res := getInto[CriteriaResult](t, val, err)
	if res.Skipped != "not the plan's integration card" || len(res.Findings) != 0 || len(kube.podOrder) != 0 {
		t.Fatalf("result = %+v, pods %d", res, len(kube.podOrder))
	}
	if tl := timeline(t, objects); !strings.Contains(tl, `"skipped":"not the plan's integration card"`) {
		t.Fatalf("timeline = %s", tl)
	}
}

// A plan that names no criteria names no proof: a finding for the human,
// no sandbox. So every runnable criterion off the house format.
func TestGateCriteriaFindsAPlanWithoutARun(t *testing.T) {
	for name, criteria := range map[string]string{
		"no criteria":         "",
		"off the format":      `{"id":"no_format","kind":"test","test":"a login test"}`,
		"judgement and human": `{"id":"judged","kind":"judgement","claim":"c"},{"id":"human_eye","kind":"human","claim":"c"}`,
	} {
		t.Run(name, func(t *testing.T) {
			kube := newFakeKube()
			objects := &fakeObjectStore{}
			r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, objects)
			seedPlan(t, store, map[string]string{"integ": RoleIntegration}, criteria)
			val, err := execActivity(t, r, ActivityGateCriteria, "t1", int64(9), "c1")
			res := getInto[CriteriaResult](t, val, err)
			var want []string
			switch name {
			case "no criteria":
				want = []string{"criteria gate: the plan names no criteria"}
			case "off the format":
				want = []string{`criterion no_format: test "a login test" is not TestXxx in <package>`}
			default:
				want = nil
			}
			if !slices.Equal(res.Findings, want) || len(kube.podOrder) != 0 || len(hub.minted()) != 0 {
				t.Fatalf("result = %+v, pods %d, mints %v; want findings %q and no sandbox", res, len(kube.podOrder), hub.minted(), want)
			}
			if name != "judgement and human" {
				if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "no criterion ran") || !strings.Contains(hub.comments[0], want[0]) {
					t.Fatalf("comments = %q", hub.comments)
				}
			} else if len(hub.comments) != 0 {
				t.Fatalf("comments = %q, want none for criteria all outside the gate", hub.comments)
			}
		})
	}
}

// The gate runs the plan's criteria in one sandbox pod: one marker line
// per criterion on the pod's stdout, a finding per criterion not met, the
// comment names the head and the verdicts, and the timeline holds one
// entry per criterion.
func TestGateCriteriaRunsThePlanCriteriaInTheSandbox(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, objects)
	seedPlan(t, store, map[string]string{"integ": RoleIntegration},
		`{"id":"green_login","kind":"test","test":"TestLogin in ./internal/auth"}`+","+
			`{"id":"state_guard","kind":"test","test":"TestState in ./internal/auth"}`+","+
			`{"id":"all_green","kind":"invariant","test":"go test ./... passes"}`)
	from := time.Now()
	done := make(chan struct{})
	var res CriteriaResult
	var err error
	go func() {
		defer close(done)
		res, err = runCriteria(t, r)
	}()
	scriptReproPods(kube, reproLine(0, strings.Join([]string{
		"agent-worker: probing",
		"__criterion green_login exit=0 ran=1 fail=0 --- PASS: TestLogin",
		"__criterion state_guard exit=1 ran=1 fail=1 --- FAIL: TestState (0.01s)",
		"__criterion all_green exit=0 ran=42 fail=0",
	}, "\n")))
	<-done
	if err != nil {
		t.Fatal(err)
	}
	wantFindings := []string{"criterion state_guard: TestState in ./internal/auth failed: --- FAIL: TestState (0.01s)"}
	if !slices.Equal(res.Findings, wantFindings) || res.Skipped != "" || res.Escalate != "" {
		t.Fatalf("result = %+v, want the one finding %q", res, wantFindings)
	}
	if len(kube.podOrder) != 1 || len(hub.minted()) != 1 {
		t.Fatalf("pods %d, mints %v; want the one pod and its read token", len(kube.podOrder), hub.minted())
	}
	if len(hub.comments) != 1 {
		t.Fatalf("comments = %q", hub.comments)
	}
	for _, want := range []string{
		"factoryd criteria gate on c1: 2 of 3 criteria met (judgement and human criteria ride the review and the approval).",
		"criterion green_login: TestLogin in ./internal/auth passed",
		"criterion state_guard: TestState in ./internal/auth failed: --- FAIL: TestState (0.01s)",
		"criterion all_green: suite ./... passed",
	} {
		if !strings.Contains(hub.comments[0], want) {
			t.Fatalf("comment = %q, want it to contain %q", hub.comments[0], want)
		}
	}
	step := timelineStep(t, objects, "t1", "gate_criteria")
	entries, _ := step["criteria"].([]any)
	if len(entries) != 3 {
		t.Fatalf("timeline criteria = %v", step["criteria"])
	}
	verdicts := []string{"passed", "failed", "passed"}
	for i, v := range verdicts {
		e, _ := entries[i].(map[string]any)
		if e["id"] != runs3[i] || e["verdict"] != v {
			t.Fatalf("entry %d = %v, want %s %s", i, e, runs3[i], v)
		}
	}
	startedWithin(t, objects, "gate_criteria", from)
}

var runs3 = []string{"green_login", "state_guard", "all_green"}

// runCriteria executes the GateCriteria activity in a test environment.
func runCriteria(t *testing.T, r *Runner) (CriteriaResult, error) {
	t.Helper()
	val, err := execActivity(t, r, ActivityGateCriteria, "t1", int64(9), "c1")
	if err != nil {
		return CriteriaResult{}, err
	}
	return getInto[CriteriaResult](t, val, nil), nil
}

// A sandbox that cannot start is the start escalation: the question names
// the action, and the answer runs the gate again, never a failed task.
func TestGateCriteriaThatCannotStartEscalates(t *testing.T) {
	kube := newFakeKube()
	kube.onCreatePod = func() {
		p := kube.podOrder[len(kube.podOrder)-1]
		p.phase, p.containers = "Pending", []ContainerState{{Name: "fetch", State: "waiting", Reason: "ErrImagePull", Message: imageNotFound}}
	}
	objects := &fakeObjectStore{}
	r, _, store := newTestRunner(t, seedCriteriaPlan(t), kube, objects)
	seedPlan(t, store, map[string]string{"integ": RoleIntegration},
		`{"id":"green_login","kind":"test","test":"TestLogin in ./internal/auth"}`)
	res, err := runCriteria(t, r)
	want := "the sandbox cannot start at image pull, and no session can fix it: push the worker image or correct its reference, " +
		"then answer to run the criteria gate again, or close the card.\nstart failed: image pull: container fetch waits with ErrImagePull: " + imageNotFound
	if err != nil || res.Escalate != want || len(res.Findings) != 0 || res.Skipped != "" || escalationSource(res.Escalate) != "start" {
		t.Fatalf("result = (%+v, %v), want the start escalation:\n%s", res, err, want)
	}
	if tl := timeline(t, objects); !strings.Contains(tl, `"start_failed":"`) {
		t.Fatalf("timeline = %s", tl)
	}
}

// The gate's findings ride the approval card (piece 4): the human reads
// them with the review's verdict, and nothing merges until the approval
// answers them.
func TestWorkflowCriteriaFindingsRideTheApprovalCard(t *testing.T) {
	const finding = "criterion green_login: no test TestLogin in ./internal/auth"
	g := newGateHarness(t, func(g *gateHarness) {
		g.criteria = []CriteriaResult{{Findings: []string{finding}}}
	})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if !slices.Contains(g.approvals[0].WhyHuman, finding) {
		t.Fatalf("why human = %q, want the criteria finding on the card", g.approvals[0].WhyHuman)
	}
	if want := []string{"policy", "repro c1", "review c1", "criteria c1", "approval c1", "merge"}; !slices.Equal(g.steps, want) {
		t.Fatalf("steps = %v, want %v", g.steps, want)
	}
}
