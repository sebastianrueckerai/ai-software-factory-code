package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

// proofRowsPlan is the stored-proof tests' plan: four test criteria, an
// invariant and a judgement.
func proofRowsPlan() Plan {
	test := func(id string) PlanCriterion {
		return PlanCriterion{ID: id, Kind: CriterionTest, Claim: "claim of " + id, Inputs: "inputs of " + id, Expected: "expected of " + id}
	}
	return Plan{Criteria: []PlanCriterion{
		test("size_bound"), test("group_open"), test("door"), test("no_proof"),
		{ID: "all_green", Kind: CriterionInvariant, Suite: "./..."},
		{ID: "judged", Kind: CriterionJudgement, Claim: "the page reads well"},
	}}
}

// proofRows are the stored-proof tests' rows: size_bound stored,
// group_open opened, door stored but flagged, no row for no_proof; the
// hold-out key_log stored, kept_out okayed with no test yet, never_red
// stored but flagged.
func proofRows() []PlanProof {
	return []PlanProof{
		{Idx: 1, Origin: ProofOriginCriterion, Criterion: "size_bound", ID: "size_bound", Suite: "proofs/size_bound", Name: "TestSizeBound",
			Path: "proofs/size_bound/size_bound_test.go", Code: "package size_bound", State: ProofStored},
		{Idx: 2, Origin: ProofOriginCriterion, Criterion: "group_open", ID: "group_open", Suite: "./acceptance", Name: "TestGroupOpen",
			Path: "acceptance/group_open_test.go", Code: "package acceptance", State: ProofOpened},
		{Idx: 3, Origin: ProofOriginCriterion, Criterion: "door", ID: "door", Suite: "proofs/door", Name: "TestDoor",
			Path: "proofs/door/door_test.go", Code: "package door", State: ProofStored, Flag: ProofFlagGreen},
		{Idx: 4, Origin: ProofOriginHoldout, ID: "key_log", Suite: "proofs/key_log", Name: "TestKeyLog",
			Path: "proofs/key_log/key_log_test.go", Code: "package key_log", State: ProofStored},
		{Idx: 5, Origin: ProofOriginHoldout, ID: "kept_out", Claim: "a kept claim", State: ProofOkayed},
		{Idx: 6, Origin: ProofOriginHoldout, ID: "never_red", Suite: "proofs/never_red", Name: "TestNeverRed",
			Path: "proofs/never_red/never_red_test.go", Code: "package never_red", State: ProofStored, Flag: ProofFlagUnrunnable},
	}
}

// A stored proof row is a runner entry: a criterion's by its id, a
// hold-out's by its entry id, its suite a directory of the repository,
// its file written first, except an opened one's, which the tree holds.
func TestStoredProofIsARunnerEntry(t *testing.T) {
	row := proofRows()[0]
	want := harness.Proof{ID: "size_bound", Suite: "./proofs/size_bound", Name: "TestSizeBound",
		File: &harness.ProofFile{Path: "proofs/size_bound/size_bound_test.go", Content: "package size_bound"}}
	if got := storedProof(row); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored = %+v %+v, want %+v %+v", got, got.File, want, want.File)
	}
	row.State = ProofOpened
	if got, want := storedProof(row), (harness.Proof{ID: "size_bound", Suite: "./proofs/size_bound", Name: "TestSizeBound"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("opened = %+v, want %+v with no file", got, want)
	}
	if got := storedProof(proofRows()[3]); got.ID != "holdout-4" || got.Suite != "./proofs/key_log" || got.File == nil {
		t.Fatalf("hold-out = %+v, want holdout-4 in ./proofs/key_log with its file", got)
	}
}

// The gate's runs of a plan's criteria, in plan order, on its proof rows:
// an invariant runs its suite as a directory of the repository; a test
// criterion runs its stored or opened proof, and is held when its proof
// is flagged or not stored yet; judgement and human criteria are outside
// the gate. The hold-outs it runs blind are the stored ones with no flag.
func TestGateRunsTakeTheSuiteAndHoldTheTests(t *testing.T) {
	rows := proofRows()
	runs, holdouts := gateRuns(proofRowsPlan(), rows)
	want := []criterionRun{
		{id: "size_bound", kind: CriterionTest, suite: "./proofs/size_bound", name: "TestSizeBound",
			file: &harness.ProofFile{Path: "proofs/size_bound/size_bound_test.go", Content: "package size_bound"}},
		{id: "group_open", kind: CriterionTest, suite: "./acceptance", name: "TestGroupOpen"},
		{id: "door", kind: CriterionTest, held: "criterion door: its proof is flagged green on main: no blind run"},
		{id: "no_proof", kind: CriterionTest, held: "criterion no_proof: no stored proof yet"},
		{id: "all_green", kind: CriterionInvariant, suite: "./..."},
	}
	if !reflect.DeepEqual(runs, want) {
		t.Fatalf("runs = %+v, want %+v", runs, want)
	}
	if !reflect.DeepEqual(holdouts, rows[3:4]) {
		t.Fatalf("holdouts = %+v, want key_log alone", holdouts)
	}
	bare := proofRowsPlan()
	bare.Criteria = append(bare.Criteria, PlanCriterion{ID: "bare_suite", Kind: CriterionInvariant, Suite: "internal/..."})
	if runs, _ := gateRuns(bare, nil); runs[len(runs)-1].suite != "./internal/..." || runs[0].held != "criterion size_bound: no stored proof yet" {
		t.Fatalf("runs = %+v, want a bare suite read as a directory and every test held", runs)
	}
}

// The report's verdicts, hand-derived one per outcome and kind: only a
// pass meets its criterion; an outcome the run lost is no report (a gate
// that runs and reports nothing does not pass); a held criterion is unmet
// without a run and no finding, since the builder cannot act on it; the
// evidence is one line of at most 200 characters.
func TestCriteriaReportVerdicts(t *testing.T) {
	held := "criterion t_held: no stored proof yet"
	runs := []criterionRun{
		{id: "t_pass", kind: CriterionTest, suite: "./p", name: "TestPass"},
		{id: "t_fail", kind: CriterionTest, suite: "./p", name: "TestFail"},
		{id: "t_missing", kind: CriterionTest, suite: "./p", name: "TestMissing"},
		{id: "t_broken", kind: CriterionTest, suite: "./p", name: "TestBroken"},
		{id: "t_skip", kind: CriterionTest, suite: "./p", name: "TestSkip"},
		{id: "t_noreport", kind: CriterionTest, suite: "./p", name: "TestNoReport"},
		{id: "t_lost", kind: CriterionTest, suite: "./p", name: "TestLost"},
		{id: "t_long", kind: CriterionTest, suite: "./p", name: "TestLong"},
		{id: "t_held", kind: CriterionTest, held: held},
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
		held,
		"criterion i_pass: suite ./... passed (3 passed, 1 skipped)",
		"criterion i_fail: suite ./x failed: Failed FAIL ./x",
		"criterion i_empty: suite ./empty ran no tests",
		"criterion i_skip: suite ./s skipped every test: no node",
	}
	if !slices.Equal(lines, wantLines) {
		t.Fatalf("lines =\n%q\nwant\n%q", lines, wantLines)
	}
	wantFindings := slices.Concat(wantLines[1:8], wantLines[10:])
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
		want := gateProof{ID: r.id, Kind: r.kind, Suite: r.suite, Name: r.name, Outcome: o.Outcome,
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

// Evidence cuts on a character boundary, and a run of bytes that starts
// no character cuts to nothing.
func TestShortenEvidenceCutsOnACharacterBoundary(t *testing.T) {
	for in, want := range map[string]string{
		"a" + strings.Repeat("é", 150): "a" + strings.Repeat("é", 99) + "...",
		strings.Repeat("\x80", 250):    "...",
	} {
		if got := shortenEvidence(in); got != want {
			t.Errorf("shortenEvidence(%q) = %q, want %q", in, got, want)
		}
	}
}

// A run's outcome, or why there is none: the entry never reported (the
// worker's own error first), check test wrote no report (its exit and the
// output's last line say why), or the report's reading. A package that
// did not build is followed by the compiler's errors: gotestsum's ===
// Errors block, at most 20 lines, or the output's last lines less the
// module downloads and pg-test.sh's banner, so the card's line shows the
// compiler's line.
func TestProofResultReadsTheRunOrSaysWhyThereIsNone(t *testing.T) {
	moved := "proofs mode: refs/pull/9/head is at c2, not the gated head c1: the pull request moved"
	busy := "another bash check (pid 7) runs in this checkout: wait for it to end, then run this one again"
	broken := junitGz(t, wrap(`<testcase classname="" name="TestMain"><failure message="Failed">FAIL&#x9;x/b [build failed]&#xA;</failure></testcase>`))
	start := "pg-test.sh: postgres (PostgreSQL) 17.6 on /tmp/tmp.k3Jq\n" +
		"go: downloading gotest.tools/gotestsum v1.13.0\n" +
		"go: downloading github.com/dnephin/pflag v1.0.7\n" +
		"go: downloading github.com/fatih/color v1.18.0\n" +
		"go: downloading golang.org/x/tools v0.30.0\n"
	summary := start + "✖  x/b\n\n=== Failed\n=== FAIL: x/b  (0.00s)\nFAIL\tx/b [build failed]\n\n=== Errors\n" +
		"b/b_test.go:5:32: undefined: Door\nb/b_test.go:6:2: undefined: Lock\n\n\nDONE 0 tests, 1 failure, 1 error in 0.000s\n"
	plain := start + "# x/b [x/b.test]\nb/b_test.go:5:32: undefined: Door\nFAIL\tx/b [build failed]\n"
	var many []string
	for i := range 22 {
		many = append(many, fmt.Sprintf("b/b.go:%d:1: undefined: x%d", i+1, i))
	}
	long := start + "=== Errors\n" + strings.Join(many, "\n") + "\n\nDONE 0 tests, 1 error in 0.000s\n"
	const failed = "Failed\nFAIL\tx/b [build failed]\n"
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
		{harness.ProofRun{Exit: 1, Report: broken, Output: summary}, true, "", "TestB",
			proofOutcome{proofMissing, failed + "b/b_test.go:5:32: undefined: Door\nb/b_test.go:6:2: undefined: Lock"}},
		{harness.ProofRun{Exit: 1, Report: broken, Output: plain}, true, "", "TestB",
			proofOutcome{proofMissing, failed + "# x/b [x/b.test]\nb/b_test.go:5:32: undefined: Door\nFAIL\tx/b [build failed]"}},
		{harness.ProofRun{Exit: 1, Report: broken, Output: long}, true, "", "TestB",
			proofOutcome{proofMissing, failed + strings.Join(many[:20], "\n")}},
		{harness.ProofRun{Exit: 1, Report: broken}, true, "", "TestB", proofOutcome{proofMissing, "Failed\nFAIL\tx/b [build failed]"}},
		{harness.ProofRun{Exit: 1, Report: junitGz(t, wrap(`<testcase name="TestB"><failure message="Failed">want 3</failure></testcase>`)), Output: summary},
			true, "", "TestB", proofOutcome{proofFailed, "Failed\nwant 3"}},
	} {
		if got := proofResult(c.run, c.ok, c.runErr, c.name); got != c.want {
			t.Errorf("proofResult(%+v, %v, %q, %q) = %+v, want %+v", c.run, c.ok, c.runErr, c.name, got, c.want)
		}
	}
	o := proofResult(harness.ProofRun{Exit: 1, Report: broken, Output: summary}, true, "", "TestB")
	run := criterionRun{id: "b_guard", kind: CriterionTest, suite: "./b", name: "TestB"}
	if got, want := criterionLine(run, o), "criterion b_guard: no test TestB in ./b: Failed FAIL x/b [build failed] "+
		"b/b_test.go:5:32: undefined: Door b/b_test.go:6:2: undefined: Lock"; got != want {
		t.Errorf("the card's line = %q, want %q", got, want)
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
		`{"id":"a_green","kind":"invariant","suite":"./a"}`)
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
// no sandbox. A plan whose gate criteria are all tests runs no sandbox
// either: each test criterion, with no stored proof yet, is its unmet
// gate row and no finding, so the plan's close never counts it unjudged
// and the builder is never sent after a proof it cannot write.
func TestGateCriteriaFindsAPlanWithoutARun(t *testing.T) {
	const held = "criterion green_login: no stored proof yet"
	for name, criteria := range map[string]string{
		"no criteria": "",
		"only test criteria": `{"id":"green_login","kind":"test","claim":"a login with the right password answers 200",` +
			`"inputs":"POST /login as ada with the right password","expected":"200 and a session cookie"}`,
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
			case "only test criteria":
				wantRows = []CriterionVerdict{{Criterion: "green_login", Verdict: "unmet", Evidence: held, Kind: CriterionTest,
					Source: CriteriaSourceGate, DecidedBy: FactoryDecider}}
			}
			if !slices.Equal(res.Findings, want) || len(kube.podOrder) != 0 || len(hub.minted()) != 0 {
				t.Fatalf("result = %+v, pods %d, mints %v; want findings %q and no sandbox", res, len(kube.podOrder), hub.minted(), want)
			}
			if rows, _ := store.CriteriaVerdicts(context.Background(), "t1"); !slices.Equal(rows, wantRows) {
				t.Fatalf("verdict rows = %+v, want %+v", rows, wantRows)
			}
			if name == "no criteria" {
				if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "no criterion ran") || !strings.Contains(hub.comments[0], want[0]) {
					t.Fatalf("comments = %q", hub.comments)
				}
			} else if len(hub.comments) != 0 {
				t.Fatalf("comments = %q, want none for a plan whose gate ran nothing", hub.comments)
			}
			if name == "only test criteria" {
				proofs, _ := timelineStep(t, objects, "t1", "gate_criteria")["proofs"].([]any)
				if len(proofs) != 1 || proofs[0].(map[string]any)["outcome"] != "" || proofs[0].(map[string]any)["id"] != "green_login" ||
					proofs[0].(map[string]any)["line"] != held {
					t.Fatalf("timeline proofs = %v, want green_login held with no outcome", proofs)
				}
			}
		})
	}
}

// proofsWorker is a worker endpoint that takes the proofs pod's list.
func proofsWorker(t *testing.T, r *Runner) *fakeWorkerEndpoint {
	t.Helper()
	w := &fakeWorkerEndpoint{status: http.StatusNoContent}
	w.start(t, r)
	return w
}

// seedProofRows files proofRowsPlan for the integration card, its rows
// beside it.
func seedProofRows(t *testing.T, store *fakeStore) {
	t.Helper()
	b, err := json.Marshal(proofRowsPlan().Criteria)
	if err != nil {
		t.Fatal(err)
	}
	seedPlan(t, store, map[string]string{"integ": RoleIntegration}, strings.TrimSuffix(strings.TrimPrefix(string(b), "["), "]"))
	if _, err := store.PutPlanProofs(context.Background(), "plan1", 1, proofRows()); err != nil {
		t.Fatal(err)
	}
}

// The gate runs the plan's stored proofs and invariants in one sandbox
// pod, its list posted to the worker's message port under a bearer only
// the pod's hash names, never in the pod's spec. A stored proof is
// written first, an opened one runs from the tree; a flagged proof or a
// missing one is an unmet row that says so, and no finding. The stored
// hold-outs run last, blind: their verdicts are on the timeline line
// alone, by entry id, and nothing the gate writes names one.
func TestGateCriteriaRunsTheStoredProofs(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, objects)
	worker := proofsWorker(t, r)
	seedProofRows(t, store)
	done := make(chan struct{})
	var res CriteriaResult
	var err error
	go func() {
		defer close(done)
		res, err = runCriteria(t, r)
	}()
	scriptReproPods(kube, proofLines(
		harness.ProofRun{ID: "size_bound", Report: junitGz(t, wrap(`<testcase name="TestSizeBound"></testcase>`))},
		harness.ProofRun{ID: "group_open", Exit: 1, Report: junitGz(t, wrap(`<testcase name="TestGroupOpen"><failure message="Failed">--- FAIL: TestGroupOpen (0.00s)</failure></testcase>`))},
		harness.ProofRun{ID: "all_green", Report: junitGz(t, wrap(`<testcase name="TestA"></testcase>`))},
		harness.ProofRun{ID: "holdout-4", Exit: 1, Report: junitGz(t, wrap(`<testcase name="TestKeyLog"><failure message="Failed">--- FAIL: TestKeyLog (0.00s)</failure></testcase>`))},
	))
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if len(kube.podOrder) != 1 || len(hub.minted()) != 1 {
		t.Fatalf("pods %d, mints %v; want the one pod and its read token", len(kube.podOrder), hub.minted())
	}
	env := podEnv(kube.podOrder[0])
	if len(worker.lists) != 1 || len(worker.auth) != 1 {
		t.Fatalf("posts %q, want the one list", worker.texts)
	}
	bearer, _ := strings.CutPrefix(worker.auth[0], "Bearer ")
	if env["MODE"] != "proofs" || env["MESSAGE_TOKEN_SHA256"] != messageTokenHash(bearer) || bearer == "" {
		t.Fatalf("pod env = %v, bearer %q; want the proofs mode holding the hash of the post's bearer", env, bearer)
	}
	var list []harness.Proof
	wantList := []harness.Proof{
		{ID: "size_bound", Suite: "./proofs/size_bound", Name: "TestSizeBound", File: &harness.ProofFile{Path: "proofs/size_bound/size_bound_test.go", Content: "package size_bound"}},
		{ID: "group_open", Suite: "./acceptance", Name: "TestGroupOpen"},
		{ID: "all_green", Suite: "./..."},
		{ID: "holdout-4", Suite: "./proofs/key_log", Name: "TestKeyLog", File: &harness.ProofFile{Path: "proofs/key_log/key_log_test.go", Content: "package key_log"}},
	}
	if err := json.Unmarshal([]byte(worker.lists[0]), &list); err != nil || !reflect.DeepEqual(list, wantList) {
		t.Fatalf("list = %s (%v), want %+v", worker.lists[0], err, wantList)
	}
	spec := string(mustJSON(kube.podOrder[0].spec))
	if _, ok := env["PROOFS"]; ok || strings.Contains(spec, "package size_bound") || strings.Contains(spec, "package key_log") {
		t.Fatalf("the pod's spec carries the list: %s", spec)
	}
	lines := []string{
		"criterion size_bound: TestSizeBound in ./proofs/size_bound passed",
		"criterion group_open: TestGroupOpen in ./acceptance failed: Failed --- FAIL: TestGroupOpen (0.00s)",
		"criterion door: its proof is flagged green on main: no blind run",
		"criterion no_proof: no stored proof yet",
		"criterion all_green: suite ./... passed (1 passed, 0 skipped)",
	}
	if !slices.Equal(res.Findings, lines[1:2]) || res.Skipped != "" || res.Escalate != "" || res.Holdouts != nil {
		t.Fatalf("result = %+v, want the finding %q", res, lines[1])
	}
	gate := func(id, verdict, line string) CriterionVerdict {
		return CriterionVerdict{Criterion: id, Verdict: verdict, Evidence: line, Kind: CriterionTest, Source: CriteriaSourceGate, DecidedBy: FactoryDecider}
	}
	wantRows := []CriterionVerdict{
		gate("size_bound", "met", lines[0]), gate("group_open", "unmet", lines[1]), gate("door", "unmet", lines[2]),
		gate("no_proof", "unmet", lines[3]), gate("all_green", "met", lines[4]),
	}
	wantRows[4].Kind = CriterionInvariant
	rows, _ := store.CriteriaVerdicts(context.Background(), "t1")
	if !slices.Equal(rows, wantRows) {
		t.Fatalf("verdict rows =\n%+v\nwant\n%+v", rows, wantRows)
	}
	if len(hub.comments) != 1 {
		t.Fatalf("comments = %q", hub.comments)
	}
	for _, want := range append([]string{"factoryd criteria gate on c1: 2 of 5 criteria met (judgement and human criteria ride the review and the approval)."}, lines...) {
		if !strings.Contains(hub.comments[0], want) {
			t.Fatalf("comment = %q, want it to contain %q", hub.comments[0], want)
		}
	}
	step := timelineStep(t, objects, "t1", "gate_criteria")
	if got := string(mustJSON(step["holdouts"])); got != `[{"id":"holdout-4","verdict":"red"}]` {
		t.Fatalf("timeline holdouts = %s, want holdout-4 red alone", got)
	}
	if proofs, _ := step["proofs"].([]any); len(proofs) != 5 {
		t.Fatalf("timeline proofs = %v, want one per gate criterion", step["proofs"])
	}
	surfaces := map[string]string{"the comment": hub.comments[0], "the timeline line": string(mustJSON(step)), "the findings": strings.Join(res.Findings, "\n")}
	for _, row := range rows {
		surfaces["row "+row.Criterion] = row.Evidence
	}
	for _, leak := range []string{"key_log", "TestKeyLog", "proofs/key_log", "kept_out", "never_red", "package size_bound", "package acceptance"} {
		for surface, text := range surfaces {
			if strings.Contains(text, leak) {
				t.Fatalf("%s carries %q: %s", surface, leak, text)
			}
		}
	}
}

// A read of the plan's proof rows that fails fails the gate, and the
// activity runs again: a gate without its stored proofs would mark every
// test criterion unmet. No pod, no row.
func TestGateCriteriaRetriesAProofsReadThatFails(t *testing.T) {
	kube := newFakeKube()
	r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, &fakeObjectStore{})
	seedProofRows(t, store)
	r.store = &proofsUnread{store}
	if _, err := runCriteria(t, r); err == nil || !strings.Contains(err.Error(), "plan_proofs: connection reset") {
		t.Fatalf("err = %v, want the read's", err)
	}
	if rows, _ := store.CriteriaVerdicts(context.Background(), "t1"); len(rows) != 0 || len(kube.podOrder) != 0 || len(hub.minted()) != 0 {
		t.Fatalf("rows %+v, pods %d, mints %v; want none", rows, len(kube.podOrder), hub.minted())
	}
}

// proofsUnread is a store whose read of the plan proofs fails.
type proofsUnread struct{ *fakeStore }

func (proofsUnread) PlanProofs(context.Context, string) ([]PlanProof, error) {
	return nil, errors.New("plan_proofs: connection reset")
}

// A proofs pod that does not take its list fails the gate, and the
// activity runs again: nothing ran, so nothing lands.
func TestGateCriteriaFailsWhenThePodTakesNoList(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, objects)
	(&fakeWorkerEndpoint{status: http.StatusInternalServerError}).start(t, r)
	seedProofRows(t, store)
	_, err := runCriteria(t, r)
	if err == nil || !strings.Contains(err.Error(), "hand the proofs pod its list: worker answered 500") {
		t.Fatalf("err = %v, want the failed post", err)
	}
	if rows, _ := store.CriteriaVerdicts(context.Background(), "t1"); len(rows) != 0 || len(hub.comments) != 0 ||
		strings.Contains(timeline(t, objects), `"step":"gate_criteria"`) {
		t.Fatalf("rows %+v, comments %q: want nothing landed", rows, hub.comments)
	}
}

// A proofs pod whose log ends before its closing line (killed, evicted,
// drained) keeps the runs it reported; the criterion it did not reach is
// no report, saying the pod ended early, an unmet row, and the gate does
// not fail while no hold-out went unreached.
func TestGateCriteriaKeepsTheRunsOfAPodThatEndedEarly(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, _, store := newTestRunner(t, seedCriteriaPlan(t), kube, objects)
	proofsWorker(t, r)
	seedPlan(t, store, map[string]string{"integ": RoleIntegration},
		`{"id":"green_login","kind":"invariant","suite":"./internal/auth"}`+","+
			`{"id":"state_guard","kind":"invariant","suite":"./internal/state"}`+","+
			`{"id":"db_guard","kind":"invariant","suite":"./internal/store"}`)
	done := make(chan struct{})
	var res CriteriaResult
	var err error
	go func() {
		defer close(done)
		res, err = runCriteria(t, r)
	}()
	body := proofLines(
		harness.ProofRun{ID: "green_login", Report: junitGz(t, wrap(`<testcase name="TestLogin"></testcase>`))},
		harness.ProofRun{ID: "state_guard", Exit: 1, Report: junitGz(t, wrap(`<testcase name="TestState"><failure message="Failed">--- FAIL: TestState (0.01s)</failure></testcase>`))},
	)
	scriptReproPods(kube, body[:strings.LastIndex(body, "\n")])
	<-done
	if err != nil {
		t.Fatalf("the gate failed: %v", err)
	}
	want := []string{
		"criterion state_guard: suite ./internal/state failed: Failed --- FAIL: TestState (0.01s)",
		"criterion db_guard: suite ./internal/store: the proofs pod ended early, before it reported this entry",
	}
	if !slices.Equal(res.Findings, want) {
		t.Fatalf("findings = %q, want %q", res.Findings, want)
	}
	proofs, _ := timelineStep(t, objects, "t1", "gate_criteria")["proofs"].([]any)
	if len(proofs) != 3 {
		t.Fatalf("timeline proofs = %v, want three", proofs)
	}
	for i, outcome := range []string{proofPassed, proofFailed, proofNoReport} {
		if e, _ := proofs[i].(map[string]any); e["outcome"] != outcome {
			t.Fatalf("proof %d = %v, want outcome %s", i, e, outcome)
		}
	}
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
		`{"id":"all_green","kind":"invariant","suite":"./..."}`)
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
		`{"id":"all_green","kind":"invariant","suite":"./..."}`)
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
