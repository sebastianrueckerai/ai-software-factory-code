package factoryd

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// junitGz is a JUnit report as the worker returns it: gzipped.
func junitGz(t *testing.T, xml string) []byte {
	t.Helper()
	return gzipped(t, []byte(xml))
}

// proofLines is what a proofs pod prints: one line per run, then the
// closing line.
func proofLines(runs ...harness.ProofRun) string {
	var lines []string
	for _, run := range runs {
		b, _ := json.Marshal(harness.WorkerEnvelope{Type: "proof", Proof: &run})
		lines = append(lines, string(b))
	}
	b, _ := json.Marshal(harness.WorkerEnvelope{Type: "proofs", Proofs: &harness.Proofs{}})
	return strings.Join(append(lines, string(b)), "\n")
}

// A bare path is a directory of the repository, never a standard library
// package; a path that starts with a dash cannot pass for a flag.
func TestGoPackageReadsABarePathAsADirectory(t *testing.T) {
	for in, want := range map[string]string{
		"acceptance":       "./acceptance",
		"internal/console": "./internal/console",
		"./a":              "./a",
		"../b":             "../b",
		"./...":            "./...",
		"...":              "...",
		"github.com/a/b":   "github.com/a/b",
		"/abs/p":           "/abs/p",
		"-c.x":             "./-c.x",
	} {
		if got := goPackage(in); got != want {
			t.Errorf("goPackage(%q) = %q, want %q", in, got, want)
		}
	}
}

// The gate's split of a plan's criteria (piece 4): kind test and kind
// invariant become runs in plan order, judgement and human criteria are
// outside the gate, and a field off the house format is a run refused,
// never one executed.
func TestCriteriaRunsSplitsKindsAndOffFormatFields(t *testing.T) {
	p := Plan{Criteria: []PlanCriterion{
		{ID: "green_login", Kind: CriterionTest, Test: "TestLogin in ./internal/auth"},
		{ID: "no_format", Kind: CriterionTest, Test: "a login test"},
		{ID: "all_green", Kind: CriterionInvariant, Test: "go test ./... passes"},
		{ID: "no_suite", Kind: CriterionInvariant, Test: "the app stays green"},
		{ID: "judged", Kind: CriterionJudgement, Claim: "the page reads well"},
		{ID: "human_eye", Kind: CriterionHuman, Claim: "the copy is true"},
		{ID: "bare_pkg", Kind: CriterionTest, Test: "TestDivideConquerSizeBounds in acceptance"},
		{ID: "bare_suite", Kind: CriterionInvariant, Test: "go test internal/... passes"},
	}}
	want := []criterionRun{
		{id: "green_login", kind: CriterionTest, test: "TestLogin", pkg: "./internal/auth"},
		{id: "no_format", kind: CriterionTest, refused: `criterion no_format: test "a login test" is not TestXxx in <package>`},
		{id: "all_green", kind: CriterionInvariant, suite: "./..."},
		{id: "no_suite", kind: CriterionInvariant, refused: `criterion no_suite: suite "the app stays green" is not go test <suite> passes`},
		{id: "bare_pkg", kind: CriterionTest, test: "TestDivideConquerSizeBounds", pkg: "./acceptance"},
		{id: "bare_suite", kind: CriterionInvariant, suite: "./internal/..."},
	}
	runs := criteriaRuns(p)
	if !slices.Equal(runs, want) {
		t.Fatalf("runs = %+v, want %+v", runs, want)
	}
	if got, want := runs[0].proof(), (harness.Proof{ID: "green_login", Suite: "./internal/auth", Name: "TestLogin"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("green_login proof = %+v, want %+v", got, want)
	}
	if got, want := runs[2].proof(), (harness.Proof{ID: "all_green", Suite: "./..."}); !reflect.DeepEqual(got, want) {
		t.Fatalf("all_green proof = %+v, want %+v", got, want)
	}
}

// The report's verdicts, hand-derived one per outcome and kind: only a
// pass meets its criterion; an outcome the run lost is no report (a gate
// that runs and reports nothing does not pass); a refused field is unmet
// without a run; the evidence is one line of at most 200 characters.
func TestCriteriaReportVerdicts(t *testing.T) {
	refused := `criterion t_refused: test "a login test" is not TestXxx in <package>`
	runs := []criterionRun{
		{id: "t_pass", kind: CriterionTest, test: "TestPass", pkg: "./p"},
		{id: "t_fail", kind: CriterionTest, test: "TestFail", pkg: "./p"},
		{id: "t_missing", kind: CriterionTest, test: "TestMissing", pkg: "./p"},
		{id: "t_broken", kind: CriterionTest, test: "TestBroken", pkg: "./p"},
		{id: "t_skip", kind: CriterionTest, test: "TestSkip", pkg: "./p"},
		{id: "t_noreport", kind: CriterionTest, test: "TestNoReport", pkg: "./p"},
		{id: "t_lost", kind: CriterionTest, test: "TestLost", pkg: "./p"},
		{id: "t_long", kind: CriterionTest, test: "TestLong", pkg: "./p"},
		{id: "t_refused", kind: CriterionTest, refused: refused},
		{id: "i_pass", kind: CriterionInvariant, suite: "./..."},
		{id: "i_fail", kind: CriterionInvariant, suite: "./x"},
		{id: "i_empty", kind: CriterionInvariant, suite: "./empty"},
		{id: "i_skip", kind: CriterionInvariant, suite: "./s"},
	}
	outcomes := map[string]proofOutcome{
		"t_pass":     {proofPassed, ""},
		"t_fail":     {proofFailed, "Failed\n=== RUN   TestFail\n    p_test.go:9: want 3, got 2\n--- FAIL: TestFail (0.00s)"},
		"t_missing":  {proofMissing, ""},
		"t_broken":   {proofMissing, "Failed\nFAIL\tx/p [build failed]"},
		"t_skip":     {proofSkipped, "p_test.go:31: FACTORYD_TEST_DATABASE_URL not set"},
		"t_noreport": {proofNoReport, "check test wrote no report (exit 1)"},
		"t_long":     {proofFailed, strings.Repeat("a", 250)},
		"i_pass":     {proofPassed, "3 passed, 1 skipped"},
		"i_fail":     {proofFailed, "Failed\nFAIL ./x"},
		"i_empty":    {proofMissing, ""},
		"i_skip":     {proofSkipped, "no node"},
	}
	findings, lines, proofs := criteriaReport(runs, outcomes)
	wantLines := []string{
		"criterion t_pass: TestPass in ./p passed",
		"criterion t_fail: TestFail in ./p failed: Failed === RUN TestFail p_test.go:9: want 3, got 2 --- FAIL: TestFail (0.00s)",
		"criterion t_missing: no test TestMissing in ./p",
		"criterion t_broken: no test TestBroken in ./p: Failed FAIL x/p [build failed]",
		"criterion t_skip: TestSkip in ./p skipped: p_test.go:31: FACTORYD_TEST_DATABASE_URL not set",
		"criterion t_noreport: TestNoReport in ./p: check test wrote no report (exit 1)",
		"criterion t_lost: TestLost in ./p: the run reported nothing",
		"criterion t_long: TestLong in ./p failed: " + strings.Repeat("a", 200) + "...",
		refused,
		"criterion i_pass: suite ./... passed (3 passed, 1 skipped)",
		"criterion i_fail: suite ./x failed: Failed FAIL ./x",
		"criterion i_empty: suite ./empty ran no tests",
		"criterion i_skip: suite ./s skipped every test: no node",
	}
	if !slices.Equal(lines, wantLines) {
		t.Fatalf("lines =\n%q\nwant\n%q", lines, wantLines)
	}
	wantFindings := slices.Concat(wantLines[1:9], wantLines[10:])
	if !slices.Equal(findings, wantFindings) {
		t.Fatalf("findings = %q, want %q", findings, wantFindings)
	}
	if len(proofs) != len(runs) {
		t.Fatalf("proofs = %+v, want one per run", proofs)
	}
	for i, p := range proofs {
		r := runs[i]
		o, ok := outcomes[r.id]
		if r.id == "t_lost" {
			o, ok = proofOutcome{proofNoReport, "the run reported nothing"}, true
		}
		want := gateProof{ID: r.id, Kind: r.kind, Suite: r.pkg + r.suite, Name: r.test, Outcome: o.Outcome,
			Met: r.id == "t_pass" || r.id == "i_pass", Line: wantLines[i], Message: o.Message}
		if !ok {
			want.Outcome, want.Message = "", ""
		}
		if p != want {
			t.Errorf("proof %d = %+v, want %+v", i, p, want)
		}
	}
	rows := gateRows(proofs)
	if len(rows) != len(proofs) {
		t.Fatalf("rows = %+v, want one per criterion", rows)
	}
	for i, row := range rows {
		verdict := "unmet"
		if proofs[i].Met {
			verdict = "met"
		}
		if want := (CriterionVerdict{Criterion: runs[i].id, Verdict: verdict, Evidence: wantLines[i], Kind: runs[i].kind}); row != want {
			t.Errorf("row %d = %+v, want %+v", i, row, want)
		}
	}
}

// A run's outcome, or why there is none: the entry never reported (the
// worker's own error first), check test wrote no report (its exit and the
// output's last line say why), or the report's reading.
func TestProofResultReadsTheRunOrSaysWhyThereIsNone(t *testing.T) {
	moved := "proofs mode: refs/pull/9/head is at c2, not the gated head c1: the pull request moved"
	busy := "another bash check (pid 7) runs in this checkout: wait for it to end, then run this one again"
	for _, c := range []struct {
		run    harness.ProofRun
		ok     bool
		runErr string
		name   string
		want   proofOutcome
	}{
		{harness.ProofRun{}, false, "", "TestA", proofOutcome{proofNoReport, "the run reported nothing for this entry"}},
		{harness.ProofRun{}, false, moved, "TestA", proofOutcome{proofNoReport, moved}},
		{harness.ProofRun{Exit: 1, Output: "go: downloading x\n" + busy + "\n"}, true, "", "TestA",
			proofOutcome{proofNoReport, "check test wrote no report (exit 1): " + busy}},
		{harness.ProofRun{Exit: -1}, true, "", "", proofOutcome{proofNoReport, "check test wrote no report (exit -1)"}},
		{harness.ProofRun{Report: junitGz(t, wrap(`<testcase name="TestA"></testcase>`))}, true, "", "TestA", proofOutcome{proofPassed, ""}},
	} {
		if got := proofResult(c.run, c.ok, c.runErr, c.name); got != c.want {
			t.Errorf("proofResult(%+v, %v, %q, %q) = %+v, want %+v", c.run, c.ok, c.runErr, c.name, got, c.want)
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
// no sandbox. So every runnable criterion off the house format, whose
// refusal is its unmet gate row: the plan's close never counts it unjudged.
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
			var wantRows []CriterionVerdict
			switch name {
			case "no criteria":
				want = []string{"criteria gate: the plan names no criteria"}
			case "off the format":
				want = []string{`criterion no_format: test "a login test" is not TestXxx in <package>`}
				wantRows = []CriterionVerdict{{Criterion: "no_format", Verdict: "unmet", Evidence: want[0], Kind: CriterionTest,
					Source: CriteriaSourceGate, DecidedBy: FactoryDecider}}
			default:
				want = nil
			}
			if !slices.Equal(res.Findings, want) || len(kube.podOrder) != 0 || len(hub.minted()) != 0 {
				t.Fatalf("result = %+v, pods %d, mints %v; want findings %q and no sandbox", res, len(kube.podOrder), hub.minted(), want)
			}
			if rows, _ := store.CriteriaVerdicts(context.Background(), "t1"); !slices.Equal(rows, wantRows) {
				t.Fatalf("verdict rows = %+v, want %+v", rows, wantRows)
			}
			if name != "judgement and human" {
				if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "no criterion ran") || !strings.Contains(hub.comments[0], want[0]) {
					t.Fatalf("comments = %q", hub.comments)
				}
			} else if len(hub.comments) != 0 {
				t.Fatalf("comments = %q, want none for criteria all outside the gate", hub.comments)
			}
			if name == "off the format" {
				proofs, _ := timelineStep(t, objects, "t1", "gate_criteria")["proofs"].([]any)
				if len(proofs) != 1 || proofs[0].(map[string]any)["outcome"] != "" || proofs[0].(map[string]any)["id"] != "no_format" {
					t.Fatalf("timeline proofs = %v, want no_format with no outcome", proofs)
				}
			}
		})
	}
}

// The gate runs the plan's criteria in one sandbox pod: the runner's
// proofs mode, one entry per criterion, each read from its JUnit report;
// a finding per criterion not met, the comment names the head and the
// verdicts, one gate row per criterion, and the timeline holds one entry
// per criterion.
func TestGateCriteriaRunsThePlanCriteriaInTheSandbox(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, objects)
	seedPlan(t, store, map[string]string{"integ": RoleIntegration},
		`{"id":"green_login","kind":"test","test":"TestLogin in ./internal/auth"}`+","+
			`{"id":"state_guard","kind":"test","test":"TestState in internal/auth"}`+","+
			`{"id":"db_guard","kind":"test","test":"TestDB in ./internal/store"}`+","+
			`{"id":"all_green","kind":"invariant","test":"go test ./... passes"}`+","+
			`{"id":"judged","kind":"judgement","claim":"the page reads well"}`)
	from := time.Now()
	done := make(chan struct{})
	var res CriteriaResult
	var err error
	go func() {
		defer close(done)
		res, err = runCriteria(t, r)
	}()
	scriptReproPods(kube, proofLines(
		harness.ProofRun{ID: "green_login", Report: junitGz(t, wrap(`<testcase name="TestLogin"></testcase>`))},
		harness.ProofRun{ID: "state_guard", Exit: 1, Report: junitGz(t, wrap(`<testcase name="TestState"><failure message="Failed">--- FAIL: TestState (0.01s)</failure></testcase>`))},
		harness.ProofRun{ID: "db_guard", Report: junitGz(t, wrap(`<testcase name="TestDB"><skipped message="FACTORYD_TEST_DATABASE_URL not set"></skipped></testcase>`))},
		harness.ProofRun{ID: "all_green", Report: junitGz(t, wrap(`<testcase name="TestA"></testcase><testcase name="TestB"><skipped message="no node"></skipped></testcase>`))},
	))
	<-done
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{
		"criterion green_login: TestLogin in ./internal/auth passed",
		"criterion state_guard: TestState in ./internal/auth failed: Failed --- FAIL: TestState (0.01s)",
		"criterion db_guard: TestDB in ./internal/store skipped: FACTORYD_TEST_DATABASE_URL not set",
		"criterion all_green: suite ./... passed (1 passed, 1 skipped)",
	}
	if !slices.Equal(res.Findings, lines[1:3]) || res.Skipped != "" || res.Escalate != "" {
		t.Fatalf("result = %+v, want the findings %q", res, lines[1:3])
	}
	if len(kube.podOrder) != 1 || len(hub.minted()) != 1 {
		t.Fatalf("pods %d, mints %v; want the one pod and its read token", len(kube.podOrder), hub.minted())
	}
	env := podEnv(kube.podOrder[0])
	if _, repro := env["REPRO_COMMAND"]; env["MODE"] != "proofs" || env["PROOFS_BUDGET"] != "2100" || repro {
		t.Fatalf("pod env = %v, want the proofs mode with its budget and no command", env)
	}
	var list []harness.Proof
	wantList := []harness.Proof{
		{ID: "green_login", Suite: "./internal/auth", Name: "TestLogin"},
		{ID: "state_guard", Suite: "./internal/auth", Name: "TestState"},
		{ID: "db_guard", Suite: "./internal/store", Name: "TestDB"},
		{ID: "all_green", Suite: "./..."},
	}
	if err := json.Unmarshal([]byte(env["PROOFS"]), &list); err != nil || !reflect.DeepEqual(list, wantList) {
		t.Fatalf("PROOFS = %s (%v), want %+v", env["PROOFS"], err, wantList)
	}
	if len(hub.comments) != 1 {
		t.Fatalf("comments = %q", hub.comments)
	}
	for _, want := range []string{
		"factoryd criteria gate on c1: 2 of 4 criteria met (judgement and human criteria ride the review and the approval).",
		lines[0], lines[3],
	} {
		if !strings.Contains(hub.comments[0], want) {
			t.Fatalf("comment = %q, want it to contain %q", hub.comments[0], want)
		}
	}
	gate := func(id, verdict, kind, line string) CriterionVerdict {
		return CriterionVerdict{Criterion: id, Verdict: verdict, Evidence: line, Kind: kind, Source: CriteriaSourceGate, DecidedBy: FactoryDecider}
	}
	wantRows := []CriterionVerdict{
		gate("green_login", "met", CriterionTest, lines[0]),
		gate("state_guard", "unmet", CriterionTest, lines[1]),
		gate("db_guard", "unmet", CriterionTest, lines[2]),
		gate("all_green", "met", CriterionInvariant, lines[3]),
	}
	if rows, _ := store.CriteriaVerdicts(context.Background(), "t1"); !slices.Equal(rows, wantRows) {
		t.Fatalf("verdict rows =\n%+v\nwant\n%+v", rows, wantRows)
	}
	step := timelineStep(t, objects, "t1", "gate_criteria")
	proofs, _ := step["proofs"].([]any)
	if step["commit"] != "c1" || len(proofs) != 4 {
		t.Fatalf("timeline = %v", step)
	}
	for i, want := range []struct {
		id, outcome string
		met         bool
	}{{"green_login", "passed", true}, {"state_guard", "failed", false}, {"db_guard", "skipped", false}, {"all_green", "passed", true}} {
		e, _ := proofs[i].(map[string]any)
		if e["id"] != want.id || e["outcome"] != want.outcome || e["met"] != want.met || e["line"] != lines[i] {
			t.Fatalf("proof %d = %v, want %s %s met %v", i, e, want.id, want.outcome, want.met)
		}
	}
	startedWithin(t, objects, "gate_criteria", from)
}

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
	if rows, _ := store.CriteriaVerdicts(context.Background(), "t1"); len(rows) != 0 {
		t.Fatalf("verdict rows = %+v, want none from a gate that never ran", rows)
	}

	// a card whose image lost its pin since its session: the same start
	// escalation, naming the state and the action
	task := seedCriteriaPlan(t)
	task.Image = "video"
	r, _, store = newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
	v := r.cfg.Policy()
	v.Projects = map[string]policy.ProjectConfig{"repo": {Repository: "sf/repo"}}
	r.cfg.Policy = func() policy.Version { return v }
	seedPlan(t, store, map[string]string{"integ": RoleIntegration},
		`{"id":"green_login","kind":"test","test":"TestLogin in ./internal/auth"}`)
	res, err = runCriteria(t, r)
	want = "the sandbox cannot start at image pin, and no session can fix it: see that factory/images.yaml on main declares the image and that its build lands, so the factory pins it, " +
		"then answer to run the criteria gate again, or close the card.\nimage video of project repo has no pin in force: no sandbox runs on it until the factory pins it"
	if err != nil || res.Escalate != want {
		t.Fatalf("an unpinned image: result = (%+v, %v), want:\n%s", res, err, want)
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
