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
		{Idx: 6, Origin: ProofOriginHoldout, ID: "never_red", Claim: "A lock turn is never lost", Inputs: "one turn of key 9", Expected: "one log row",
			Suite: "proofs/never_red", Name: "TestNeverRed",
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

// sizeBoundCode is size_bound's stored test; sizeBoundFail its red report,
// whose message reads sizeBoundMsg.
const sizeBoundCode = "package size_bound\n\nfunc TestSizeBound(t *testing.T) {}\n"

var sizeBoundFail = wrap(`<testcase name="TestSizeBound"><failure message="Failed">=== RUN   TestSizeBound&#xA;    size_bound_test.go:12: want 3.75, got 3.5&#xA;--- FAIL: TestSizeBound (0.00s)</failure></testcase>`)

const sizeBoundMsg = "Failed\n=== RUN   TestSizeBound\n    size_bound_test.go:12: want 3.75, got 3.5\n--- FAIL: TestSizeBound (0.00s)"

// blindPlan is the blind proof tests' plan: four test criteria, an
// invariant and a judgement.
func blindPlan() Plan {
	return Plan{Criteria: []PlanCriterion{
		{ID: "size_bound", Kind: CriterionTest, Claim: "A size over the bound is refused", Inputs: "POST /api/sizes with 3.75 against a bound of 3.5",
			Expected: "400 and no row in the store"},
		{ID: "group_open", Kind: CriterionTest, Claim: "A group opens at its hours", Inputs: "GET /groups/7 at 09:00", Expected: "200 and open true"},
		{ID: "door", Kind: CriterionTest, Claim: "A door opens for its key", Inputs: "POST /doors/1/open with key 7", Expected: "200"},
		{ID: "no_proof", Kind: CriterionTest, Claim: "c", Inputs: "i", Expected: "e"},
		{ID: "all_green", Kind: CriterionInvariant, Suite: "./..."},
		{ID: "judged", Kind: CriterionJudgement, Claim: "j", Inputs: "i", Prompt: "p"},
	}}
}

// blindRows are blindPlan's rows: size_bound stored, group_open opened,
// door stored but flagged, no row for no_proof; the hold-outs key_log and
// lock_log stored.
func blindRows() []PlanProof {
	return []PlanProof{
		{Idx: 1, Origin: ProofOriginCriterion, Criterion: "size_bound", ID: "size_bound", State: ProofStored, Suite: "proofs/size_bound", Name: "TestSizeBound",
			Path: "proofs/size_bound/size_bound_test.go", Code: sizeBoundCode},
		{Idx: 2, Origin: ProofOriginCriterion, Criterion: "group_open", ID: "group_open", State: ProofOpened, Suite: "./acceptance", Name: "TestGroupOpen",
			Path: "acceptance/group_open_test.go", Code: "package acceptance // group_open v1"},
		{Idx: 3, Origin: ProofOriginCriterion, Criterion: "door", ID: "door", State: ProofStored, Flag: ProofFlagGreen, Suite: "proofs/door", Name: "TestDoor",
			Path: "proofs/door/door_test.go", Code: "package door // flagged"},
		{Idx: 4, Origin: ProofOriginHoldout, ID: "key_log", Claim: "Every key turn lands in the log", Inputs: "three turns of key 7", Expected: "three log rows",
			State: ProofStored, Suite: "proofs/key_log", Name: "TestKeyLog", Path: "proofs/key_log/key_log_test.go", Code: "package key_log"},
		{Idx: 5, Origin: ProofOriginHoldout, ID: "lock_log", Claim: "Each lock turn lands", Inputs: "one turn", Expected: "one row",
			State: ProofStored, Suite: "proofs/lock_log", Name: "TestLockLog", Path: "proofs/lock_log/lock_log_test.go", Code: "package lock_log"},
	}
}

// seedBlindPlan files p for the integration card t1 under the plan task
// plan1, which holds the plan's events, its rows beside it.
func seedBlindPlan(t *testing.T, store *fakeStore, p Plan, rows []PlanProof) {
	t.Helper()
	b, err := json.Marshal(p.Criteria)
	if err != nil {
		t.Fatal(err)
	}
	seedPlan(t, store, map[string]string{"integ": RoleIntegration}, strings.TrimSuffix(strings.TrimPrefix(string(b), "["), "]"))
	store.tasks["plan1"] = Task{ID: "plan1", Status: "running", Repository: "sf/repo", Kind: "agent", Track: policy.TrackPlan}
	if _, err := store.PutPlanProofs(context.Background(), "plan1", 1, rows); err != nil {
		t.Fatal(err)
	}
}

// The gate's runs of a plan's criteria, in plan order, on its proof rows,
// then the hold-outs in idx order: an invariant runs its suite as a
// directory of the repository; a stored proof runs blind with its file, a
// flagged one too, which makes no entry and opens; an opened proof runs
// by suite and name from the tree; a test criterion without a stored or
// opened row is held, late when the store happened without it, and never
// opens; a judgement criterion's proof runs and is held as a test
// criterion's, and a human one's runs, and it makes no run without one.
func TestGateRunsOpenAndHoldTheProofs(t *testing.T) {
	rows := blindRows()
	runs := gateRuns(blindPlan(), rows)
	file := func(pp PlanProof) *harness.ProofFile { return &harness.ProofFile{Path: pp.Path, Content: pp.Code} }
	want := []criterionRun{
		{id: "size_bound", kind: CriterionTest, suite: "./proofs/size_bound", name: "TestSizeBound",
			file: &harness.ProofFile{Path: "proofs/size_bound/size_bound_test.go", Content: sizeBoundCode}, stored: &rows[0]},
		{id: "group_open", kind: CriterionTest, suite: "./acceptance", name: "TestGroupOpen", stored: &rows[1]},
		{id: "door", kind: CriterionTest, suite: "./proofs/door", name: "TestDoor", file: file(rows[2]), stored: &rows[2]},
		{id: "no_proof", kind: CriterionTest, held: "Criterion no_proof has no stored proof: the prover filed before a later revision added it.", late: true},
		{id: "all_green", kind: CriterionInvariant, suite: "./..."},
		{id: "judged", kind: CriterionJudgement, held: "Criterion judged has no stored proof: the prover filed before a later revision added it.", late: true},
		{id: "holdout-4", kind: CriterionTest, suite: "./proofs/key_log", name: "TestKeyLog", file: file(rows[3]), stored: &rows[3]},
		{id: "holdout-5", kind: CriterionTest, suite: "./proofs/lock_log", name: "TestLockLog", file: file(rows[4]), stored: &rows[4]},
	}
	if !reflect.DeepEqual(runs, want) {
		t.Fatalf("runs = %+v, want %+v", runs, want)
	}
	var states, labels []string
	for _, c := range runs {
		states, labels = append(states, c.state()), append(labels, c.label())
	}
	if want := []string{proofBlind, proofOpen, proofBlind, "", "", "", proofBlind, proofBlind}; !slices.Equal(states, want) {
		t.Fatalf("states = %q, want %q", states, want)
	}
	if want := []string{"criterion size_bound", "criterion group_open", "criterion door", "criterion no_proof", "criterion all_green",
		"criterion judged", "hold-out key_log", "hold-out lock_log"}; !slices.Equal(labels, want) {
		t.Fatalf("labels = %q, want %q", labels, want)
	}
	bare := blindPlan()
	bare.Criteria = append(bare.Criteria, PlanCriterion{ID: "bare_suite", Kind: CriterionInvariant, Suite: "internal/..."})
	if runs := gateRuns(bare, nil); runs[len(runs)-1].suite != "./internal/..." || runs[0].held != "criterion size_bound: no stored proof yet" || runs[0].late ||
		runs[5].held != "criterion judged: no stored proof yet" || runs[5].late {
		t.Fatalf("runs = %+v, want a bare suite read as a directory and every test and judgement held", runs)
	}
	withArtifacts := blindPlan()
	withArtifacts.Criteria = append(withArtifacts.Criteria, readsCalm, pagesOK)
	rows = append(blindRows(), artifactRows()...)
	runs = gateRuns(withArtifacts, rows)
	want = slices.Insert(want, 6,
		criterionRun{id: "reads_calm", kind: CriterionJudgement, suite: "./proofs/reads_calm", name: "TestReadsCalm", file: file(rows[5]), stored: &rows[5]},
		criterionRun{id: "pages_ok", kind: CriterionHuman, suite: "./proofs/pages_ok", name: "TestPagesOK", file: file(rows[6]), stored: &rows[6]})
	if !reflect.DeepEqual(runs, want) {
		t.Fatalf("runs with artifacts = %+v, want %+v", runs, want)
	}
}

// An opened proof's finding names the criterion or the hold-out, why it
// opened at the head, its path, its claim with its inputs and expected
// numbers, its code and the output the builder needs; a flagged one had
// no blind try and has no output.
func TestOpenedFindingCarriesTheCriterionTheCodeAndTheOutput(t *testing.T) {
	runs := gateRuns(blindPlan(), blindRows())
	const sizeText = "The criterion: A size over the bound is refused. Inputs: POST /api/sizes with 3.75 against a bound of 3.5. Expected: 400 and no row in the store."
	const rest = ". It is open now. Commit the test below at proofs/size_bound/size_bound_test.go and make it pass: " +
		"fix the implementation, or the test where it is wrong, or raise the question on the card when the criterion cannot hold.\n" +
		sizeText + "\n" +
		"TestSizeBound in ./proofs/size_bound:\n```\npackage size_bound\n\nfunc TestSizeBound(t *testing.T) {}\n```\n" +
		"Its output:\n```\n" + sizeBoundMsg + "\n```"
	for outcome, clause := range map[string]string{
		proofFailed:   "its blind proof ran red at c1",
		proofMissing:  "its blind proof ran no test of its name at c1",
		proofSkipped:  "its blind proof skipped at c1",
		proofNoReport: "its blind proof wrote no report at c1",
	} {
		o := openedProof{run: runs[0], outcome: outcome, output: sizeBoundMsg, text: sizeText}
		if got, want := openedFinding(o, "c1"), "criterion size_bound: "+clause+rest; got != want {
			t.Errorf("the %s finding =\n%s\nwant\n%s", outcome, got, want)
		}
	}
	door := openedProof{run: runs[2], outcome: proofFlagged, text: "The criterion: A door opens for its key. Inputs: POST /doors/1/open with key 7. Expected: 200."}
	if got, want := openedFinding(door, "c1"), "criterion door: its proof was flagged green on main by its dry run, so it had no blind try. It is open now. "+
		"Commit the test below at proofs/door/door_test.go and make it pass: "+
		"fix the implementation, or the test where it is wrong, or raise the question on the card when the criterion cannot hold.\n"+
		"The criterion: A door opens for its key. Inputs: POST /doors/1/open with key 7. Expected: 200.\n"+
		"TestDoor in ./proofs/door:\n```\npackage door // flagged\n```"; got != want {
		t.Errorf("the flagged finding =\n%s\nwant\n%s", got, want)
	}
	keyLog := openedProof{run: runs[6], outcome: proofFailed, output: "Failed\n--- FAIL: TestKeyLog (0.00s)",
		text: "The hold-out: Every key turn lands in the log. Inputs: three turns of key 7. Expected: three log rows."}
	if got, want := openedFinding(keyLog, "8e3a1b0c9d"), "hold-out key_log: its blind proof ran red at 8e3a1b0. It is open now. "+
		"Commit the test below at proofs/key_log/key_log_test.go and make it pass: "+
		"fix the implementation, or the test where it is wrong, or raise the question on the card when the criterion cannot hold.\n"+
		"The hold-out: Every key turn lands in the log. Inputs: three turns of key 7. Expected: three log rows.\n"+
		"TestKeyLog in ./proofs/key_log:\n```\npackage key_log\n```\n"+
		"Its output:\n```\nFailed\n--- FAIL: TestKeyLog (0.00s)\n```"; got != want {
		t.Errorf("the hold-out's finding =\n%s\nwant\n%s", got, want)
	}
}

// The report's verdicts, hand-derived one per outcome and kind: only a
// pass meets its criterion; an outcome the run lost is no report (a gate
// that runs and reports nothing does not pass); a held criterion is unmet
// without a run and no finding, since the builder cannot act on it, and
// a late one is a card line, for the human; the evidence is one line of
// at most 200 characters.
func TestCriteriaReportVerdicts(t *testing.T) {
	held := "criterion t_held: no stored proof yet"
	late := "Criterion t_late has no stored proof: the prover filed before a later revision added it."
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
		{id: "t_late", kind: CriterionTest, held: late, late: true},
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
	results := map[string]gateResult{}
	for id, o := range outcomes {
		results[id] = gateResult{outcome: o, ran: true}
	}
	rep := criteriaReport(Plan{}, "c1", runs, results)
	findings, lines, proofs := rep.findings, rep.lines, rep.proofs
	if rep.met != 2 || len(rep.opened) != 0 {
		t.Fatalf("met %d, opened %+v; want 2 and none", rep.met, rep.opened)
	}
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
		late,
		"criterion i_pass: suite ./... passed (3 passed, 1 skipped)",
		"criterion i_fail: suite ./x failed: Failed FAIL ./x",
		"criterion i_empty: suite ./empty ran no tests",
		"criterion i_skip: suite ./s skipped every test: no node",
	}
	if !slices.Equal(lines, wantLines) {
		t.Fatalf("lines =\n%q\nwant\n%q", lines, wantLines)
	}
	wantFindings := slices.Concat(wantLines[1:8], wantLines[11:])
	if !slices.Equal(findings, wantFindings) || !slices.Equal(rep.card, []string{late}) {
		t.Fatalf("findings = %q, card %q; want %q and the late line", findings, rep.card, wantFindings)
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

// A feature card of the plan skips the gate: it runs on the cards that
// carry the whole feature, the integration card and a delta card.
func TestGateCriteriaSkipsAFeatureCard(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	task := seedCriteriaPlan(t)
	task.PlanKey = "sizes"
	r, _, store := newTestRunner(t, task, kube, objects)
	seedPlan(t, store, map[string]string{"sizes": RoleFeature, "integ": RoleIntegration},
		`{"id":"a_green","kind":"invariant","suite":"./a"}`)
	val, err := execActivity(t, r, ActivityGateCriteria, "t1", int64(9), "c1")
	res := getInto[CriteriaResult](t, val, err)
	if res.Skipped != "not the plan's integration card or a delta card" || len(res.Findings) != 0 || res.Stand != nil || len(kube.podOrder) != 0 {
		t.Fatalf("result = %+v, pods %d", res, len(kube.podOrder))
	}
	if tl := timeline(t, objects); !strings.Contains(tl, `"skipped":"not the plan's integration card or a delta card"`) {
		t.Fatalf("timeline = %s", tl)
	}
}

// A delta card, whose key the plan's newest revision does not hold, runs
// the gate on its own head; the rows land on the integration card, where
// the plan's rows stand, and the line on the gated card.
func TestGateCriteriaRunsOnADeltaCard(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	task := seedCriteriaPlan(t)
	task.PlanKey = "delta1"
	r, _, store := newTestRunner(t, task, kube, objects)
	proofsWorker(t, r)
	seedPlan(t, store, map[string]string{"integ": RoleIntegration, "sizes": RoleFeature},
		`{"id":"size_bound","kind":"test","claim":"c","inputs":"i","expected":"e"}`)
	store.tasks["t-int"] = Task{ID: "t-int", Status: "in-review", Repository: "sf/repo", Parent: "plan1", Source: SourcePlan, PlanKey: "integ",
		Kind: "agent", Track: "direct"}
	if _, err := store.PutPlanProofs(context.Background(), "plan1", 1, blindRows()[:1]); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var res CriteriaResult
	var err error
	go func() {
		defer close(done)
		res, err = runCriteria(t, r)
	}()
	scriptReproPods(kube, proofLines(harness.ProofRun{ID: "size_bound", Report: junitGz(t, wrap(`<testcase name="TestSizeBound"></testcase>`))}))
	<-done
	if err != nil || res.Skipped != "" || len(res.Findings) != 0 || len(kube.podOrder) != 1 {
		t.Fatalf("result = %+v, %v, pods %d; want one pod and no finding", res, err, len(kube.podOrder))
	}
	want := []CriterionVerdict{{Criterion: "size_bound", Verdict: "met", Evidence: "blind: criterion size_bound: TestSizeBound in ./proofs/size_bound passed",
		Kind: CriterionTest, Source: CriteriaSourceGate, DecidedBy: FactoryDecider}}
	if rows, _ := store.CriteriaVerdicts(context.Background(), "t-int"); !slices.Equal(rows, want) {
		t.Fatalf("rows on the integration card = %+v, want %+v", rows, want)
	}
	if rows, _ := store.CriteriaVerdicts(context.Background(), "t1"); len(rows) != 0 {
		t.Fatalf("rows on the delta card = %+v, want none", rows)
	}
	if proofs, _ := timelineStep(t, objects, "t1", "gate_criteria")["proofs"].([]any); len(proofs) != 1 {
		t.Fatalf("the delta card's line proofs = %v, want size_bound's", proofs)
	}
}

// A plan that names no criteria names no proof: a card finding for the
// human, no sandbox, no stand. A plan whose gate criteria are all tests or judgements runs
// no sandbox either: each, with no stored proof yet, is its unmet row and
// no finding, a test one's the gate's and a judgement one's with no
// verdict, so the plan's close never counts it unjudged and the builder
// is never sent after a proof it cannot write.
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
				if res.Stand != nil {
					t.Fatalf("stand = %+v, want none for a plan without criteria", res.Stand)
				}
			case "only test criteria":
				wantRows = []CriterionVerdict{{Criterion: "green_login", Verdict: "unmet", Evidence: held, Kind: CriterionTest,
					Source: CriteriaSourceGate, DecidedBy: FactoryDecider}}
			case "judgement and human":
				wantRows = []CriterionVerdict{{Criterion: "judged", Verdict: "unmet", Evidence: "no verdict: criterion judged: no stored proof yet", Kind: CriterionJudgement,
					Source: CriteriaSourceJudgement, DecidedBy: FactoryDecider}}
			}
			if len(res.Findings) != 0 || !slices.Equal(res.Card, want) || len(kube.podOrder) != 0 || len(hub.minted()) != 0 {
				t.Fatalf("result = %+v, pods %d, mints %v; want no finding, the card's %q and no sandbox", res, len(kube.podOrder), hub.minted(), want)
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

// A test criterion whose proof the plan's prover has not stored yet asks
// the human to wait, while the prover is open or still to open: the held
// rows stand, and the answer runs the gate again. A prover that ended
// without a store, or one that never opens because the plan's foundation
// card ended closed or failed, asks nothing: each test criterion is an
// unmet row.
func TestGateCriteriaWaitsForAProverThatIsNotDone(t *testing.T) {
	const tests = `{"id":"green_login","kind":"test","claim":"a login answers 200","inputs":"POST /login","expected":"200"},` +
		`{"id":"door","kind":"test","claim":"a door opens","inputs":"POST /doors/1/open","expected":"200"}`
	for _, tc := range []struct {
		name       string
		prover     string // the prover's status, "" for none filed
		foundation string // the foundation card's status, "" for a plan without one
		policy     func() policy.Version
		want       string
	}{
		{"a running prover", "running", "", proverPolicy, proofsNotStored},
		{"a queued prover", "queued", "", proverPolicy, proofsNotStored},
		{"a prover still to open", "", "", proverPolicy, proofsNotStored},
		{"a prover still to open after a foundation in review", "", "in-review", proverPolicy, proofsNotStored},
		{"a prover that failed", "failed", "", proverPolicy, ""},
		{"a policy that runs no prover", "", "", testPolicyVersion, ""},
		{"a foundation card closed", "", "closed", proverPolicy, ""},
		{"a foundation card failed", "", "failed", proverPolicy, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kube := newFakeKube()
			r, _, store := newTestRunner(t, seedCriteriaPlan(t), kube, &fakeObjectStore{})
			r.cfg.Policy = tc.policy
			roles := map[string]string{"integ": RoleIntegration}
			if tc.foundation != "" {
				roles["found"] = RoleFoundation
				store.tasks["t-found"] = Task{ID: "t-found", Status: tc.foundation, Repository: "sf/repo", Parent: "plan1", Source: SourcePlan,
					PlanKey: "found", Kind: "agent"}
			}
			seedPlan(t, store, roles, tests)
			if tc.prover != "" {
				store.tasks["t-prove"] = Task{ID: "t-prove", Status: tc.prover, Repository: "sf/repo", Parent: "plan1", Source: SourceProver, Kind: "agent"}
			}
			res, err := runCriteria(t, r)
			if err != nil || res.Escalate != tc.want || len(res.Findings) != 0 {
				t.Fatalf("result = %+v, %v; want the escalation %q and no finding", res, err, tc.want)
			}
			held := func(id string) CriterionVerdict {
				return CriterionVerdict{Criterion: id, Verdict: "unmet", Evidence: "criterion " + id + ": no stored proof yet", Kind: CriterionTest,
					Source: CriteriaSourceGate, DecidedBy: FactoryDecider}
			}
			want := []CriterionVerdict{held("green_login"), held("door")}
			if rows, _ := store.CriteriaVerdicts(context.Background(), "t1"); !slices.Equal(rows, want) {
				t.Fatalf("verdict rows = %+v, want %+v", rows, want)
			}
		})
	}
}

// While the gate waits for the prover it runs nothing: no pod for an
// invariant, no comment, the held rows alone, and its timeline line names
// the ask. The answer runs the whole gate again.
func TestGateCriteriaWaitingRunsNoPod(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, objects)
	r.cfg.Policy = proverPolicy
	seedPlan(t, store, map[string]string{"integ": RoleIntegration},
		`{"id":"green_login","kind":"test","claim":"a login answers 200","inputs":"POST /login","expected":"200"},{"id":"all_green","kind":"invariant","suite":"./..."}`)
	store.tasks["t-prove"] = Task{ID: "t-prove", Status: "running", Repository: "sf/repo", Parent: "plan1", Source: SourceProver, Kind: "agent"}
	res, err := runCriteria(t, r)
	const ask = "The plan's prover stored no proof yet. Once it does, answer to run the criteria gate again."
	if err != nil || res.Escalate != ask {
		t.Fatalf("result = %+v, %v; want the escalation %q", res, err, ask)
	}
	if len(kube.podOrder) != 0 || len(hub.minted()) != 0 || len(hub.comments) != 0 {
		t.Fatalf("pods %d, mints %v, comments %q; want none", len(kube.podOrder), hub.minted(), hub.comments)
	}
	want := []CriterionVerdict{{Criterion: "green_login", Verdict: "unmet", Evidence: "criterion green_login: no stored proof yet", Kind: CriterionTest,
		Source: CriteriaSourceGate, DecidedBy: FactoryDecider}}
	if rows, _ := store.CriteriaVerdicts(context.Background(), "t1"); !slices.Equal(rows, want) {
		t.Fatalf("verdict rows = %+v, want %+v", rows, want)
	}
	if got := timelineStep(t, objects, "t1", "gate_criteria")["escalate"]; got != ask {
		t.Fatalf("the gate_criteria line's escalate = %v, want %q", got, ask)
	}
}

// A judgement criterion a later revision added after the prover filed is
// held as a test criterion is: its line and a card finding say so, for
// the human, since no session can prove it, and its row is an unmet
// judgement with no verdict, so it counts as judged. A
// plan of such criteria alone runs nothing and says so on the pull
// request.
func TestGateCriteriaHoldsALateJudgementCriterion(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, objects)
	seedPlan(t, store, map[string]string{"integ": RoleIntegration},
		`{"id":"late_calm","kind":"judgement","claim":"The list reads calm","inputs":"the list page","prompt":"Calm?"}`)
	if _, err := store.PutPlanProofs(context.Background(), "plan1", 1, []PlanProof{{Idx: 1, Origin: ProofOriginCriterion, Criterion: "dropped", ID: "dropped",
		State: ProofStored, Suite: "proofs/dropped", Name: "TestDropped", Path: "proofs/dropped/dropped_test.go", Code: "package dropped"}}); err != nil {
		t.Fatal(err)
	}
	res, err := runCriteria(t, r)
	const late = "Criterion late_calm has no stored proof: the prover filed before a later revision added it."
	if err != nil || len(res.Findings) != 0 || !slices.Equal(res.Card, []string{late}) {
		t.Fatalf("result = %+v, %v; want the late line on the card alone", res, err)
	}
	want := []CriterionVerdict{{Criterion: "late_calm", Verdict: "unmet", Evidence: "no verdict: " + late, Kind: CriterionJudgement,
		Source: CriteriaSourceJudgement, DecidedBy: FactoryDecider}}
	if rows, _ := store.CriteriaVerdicts(context.Background(), "t1"); len(kube.podOrder) != 0 || !slices.Equal(rows, want) {
		t.Fatalf("pods %d, rows %+v; want none and %+v", len(kube.podOrder), rows, want)
	}
	var proofs []gateProof
	if err := json.Unmarshal(mustJSON(timelineStep(t, objects, "t1", "gate_criteria")["proofs"]), &proofs); err != nil ||
		!slices.Equal(proofs, []gateProof{{ID: "late_calm", Kind: CriterionJudgement, Line: late}}) {
		t.Fatalf("the line's proofs = %+v, %v; want late_calm's late line", proofs, err)
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "factoryd criteria gate on c1: no criterion ran.") || !strings.Contains(hub.comments[0], late) {
		t.Fatalf("comments = %q, want the one saying no criterion ran, with the late line", hub.comments)
	}
}

// A judgement or a human criterion's proof is the prover's too: a plan
// with no test criterion waits for it as well and runs nothing. The
// judgement criterion's row is unmet with no verdict, held; the human
// one gets none.
func TestGateCriteriaWaitsForAJudgementOrHumanProof(t *testing.T) {
	for kind, tc := range map[string]struct {
		criterion string
		rows      []CriterionVerdict
	}{
		CriterionJudgement: {`{"id":"reads_calm","kind":"judgement","claim":"The sizes page reads calm","inputs":"the sizes page","prompt":"Calm?"}`,
			[]CriterionVerdict{{Criterion: "reads_calm", Verdict: "unmet", Evidence: "no verdict: criterion reads_calm: no stored proof yet", Kind: CriterionJudgement,
				Source: CriteriaSourceJudgement, DecidedBy: FactoryDecider}}},
		CriterionHuman: {`{"id":"pages_ok","kind":"human","claim":"The pages read well","evidence":"Open /sizes"}`, nil},
	} {
		t.Run(kind, func(t *testing.T) {
			kube := newFakeKube()
			r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, &fakeObjectStore{})
			r.cfg.Policy = proverPolicy
			seedPlan(t, store, map[string]string{"integ": RoleIntegration}, tc.criterion+`,{"id":"all_green","kind":"invariant","suite":"./..."}`)
			store.tasks["t-prove"] = Task{ID: "t-prove", Status: "running", Repository: "sf/repo", Parent: "plan1", Source: SourceProver, Kind: "agent"}
			res, err := runCriteria(t, r)
			if err != nil || res.Escalate != proofsNotStored {
				t.Fatalf("result = %+v, %v; want the escalation %q", res, err, proofsNotStored)
			}
			if rows, _ := store.CriteriaVerdicts(context.Background(), "t1"); len(kube.podOrder) != 0 || len(hub.comments) != 0 || !slices.Equal(rows, tc.rows) {
				t.Fatalf("pods %d, comments %q, rows %+v; want none and %+v", len(kube.podOrder), hub.comments, rows, tc.rows)
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
// written first, an opened one runs from the tree; a flagged proof makes
// no run and opens, a criterion's or a hold-out's. A criterion with no
// proof once the store happened came after the prover filed: its unmet
// row says so, and so does a finding. The stored hold-outs run last,
// blind: a green one's verdict is on the timeline line alone, by entry
// id, and nothing the gate writes names it.
func TestGateCriteriaRunsTheStoredProofs(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, objects)
	hub.files = map[string]string{"c1:acceptance/group_open_test.go": "package acceptance"}
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
		harness.ProofRun{ID: "holdout-4", Report: junitGz(t, wrap(`<testcase name="TestKeyLog"></testcase>`))},
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
		"criterion door: its proof was flagged green on main by its dry run, so it had no blind try",
		"Criterion no_proof has no stored proof: the prover filed before a later revision added it.",
		"criterion all_green: suite ./... passed (1 passed, 0 skipped)",
		"Criterion judged has no stored proof: the prover filed before a later revision added it.",
		"hold-out never_red: its proof was flagged unrunnable by its dry run, so it had no blind try",
	}
	if len(res.Findings) != 3 || res.Findings[0] != lines[1] || !strings.HasPrefix(res.Findings[1], lines[2]+". It is open now.") ||
		!strings.HasPrefix(res.Findings[2], lines[6]+". It is open now.") ||
		!strings.Contains(res.Findings[2], "\nThe hold-out: A lock turn is never lost. Inputs: one turn of key 9. Expected: one log row.\n") ||
		!slices.Equal(res.Card, []string{lines[3], lines[5]}) || res.Skipped != "" || res.Escalate != "" || res.Holdouts != nil {
		t.Fatalf("result = %+v, want group_open's line, door's opening and never_red's opening, and no_proof's and judged's lines on the card", res)
	}
	gate := func(id, verdict, line string) CriterionVerdict {
		return CriterionVerdict{Criterion: id, Verdict: verdict, Evidence: line, Kind: CriterionTest, Source: CriteriaSourceGate, DecidedBy: FactoryDecider}
	}
	wantRows := []CriterionVerdict{
		gate("size_bound", "met", "blind: "+lines[0]), gate("group_open", "unmet", "open: "+lines[1]), gate("door", "unmet", "blind: "+lines[2]),
		gate("no_proof", "unmet", lines[3]), gate("all_green", "met", lines[4]),
		{Criterion: "judged", Verdict: "unmet", Evidence: "no verdict: " + lines[5], Kind: CriterionJudgement, Source: CriteriaSourceJudgement, DecidedBy: FactoryDecider},
	}
	wantRows[4].Kind = CriterionInvariant
	rows, _ := store.CriteriaVerdicts(context.Background(), "t1")
	if !slices.Equal(rows, wantRows) {
		t.Fatalf("verdict rows =\n%+v\nwant\n%+v", rows, wantRows)
	}
	if len(hub.comments) != 1 {
		t.Fatalf("comments = %q", hub.comments)
	}
	for _, want := range append([]string{"factoryd criteria gate on c1: 2 of 6 criteria met (human criteria ride the approval)."}, lines...) {
		if !strings.Contains(hub.comments[0], want) {
			t.Fatalf("comment = %q, want it to contain %q", hub.comments[0], want)
		}
	}
	step := timelineStep(t, objects, "t1", "gate_criteria")
	if got := string(mustJSON(step["holdouts"])); got != `[{"id":"holdout-4","verdict":"green"}]` {
		t.Fatalf("timeline holdouts = %s, want holdout-4 green alone", got)
	}
	if proofs, _ := step["proofs"].([]any); len(proofs) != 7 {
		t.Fatalf("timeline proofs = %v, want one per gate criterion and never_red's", step["proofs"])
	}
	if got := string(mustJSON(step["opened"])); got != `["door","never_red"]` {
		t.Fatalf("timeline opened = %s, want door and never_red", got)
	}
	surfaces := map[string]string{"the comment": hub.comments[0], "the timeline line": string(mustJSON(step)), "the findings": strings.Join(res.Findings, "\n")}
	for _, row := range rows {
		surfaces["row "+row.Criterion] = row.Evidence
	}
	for _, leak := range []string{"key_log", "TestKeyLog", "proofs/key_log", "kept_out", "package size_bound", "package acceptance"} {
		for surface, text := range surfaces {
			if strings.Contains(text, leak) {
				t.Fatalf("%s carries %q: %s", surface, leak, text)
			}
		}
	}
}

// gateEvents are the proof_opened events' details on task id, in order.
func gateEvents(store *fakeStore, id string) []map[string]any {
	var out []map[string]any
	for _, e := range store.events {
		if e.TaskID == id && e.Kind == EventProofOpened {
			out = append(out, e.Detail)
		}
	}
	return out
}

// runCriteriaWithPods runs the gate as often as bodies hold, each time
// on c1, while the n-th proofs pod prints the n-th body.
func runCriteriaWithPods(t *testing.T, r *Runner, kube *fakeKube, bodies ...string) ([]CriteriaResult, []error) {
	t.Helper()
	done := make(chan struct{})
	res, errs := make([]CriteriaResult, len(bodies)), make([]error, len(bodies))
	go func() {
		defer close(done)
		for i := range bodies {
			res[i], errs[i] = runCriteria(t, r)
		}
	}()
	scriptReproPods(kube, bodies...)
	<-done
	return res, errs
}

// The gate's one blind try per stored proof: a green one stays stored and
// meets its criterion, blind; any other outcome, and a flagged proof,
// opens it: the finding carries the criterion, the code and the output,
// a proof_opened event on the card and on the plan records it, and its
// row turns opened. An opened proof runs from the tree, marked changed
// when its file there is not its code. A green hold-out stays out of
// every surface; one that opens is named by its own id, its claim public.
func TestGateCriteriaOpensTheBlindProofsThatAreNotGreen(t *testing.T) {
	ctx := context.Background()
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, objects)
	hub.files = map[string]string{"c1:acceptance/group_open_test.go": "package acceptance // group_open v2"}
	worker := proofsWorker(t, r)
	seedBlindPlan(t, store, blindPlan(), blindRows())
	results, errs := runCriteriaWithPods(t, r, kube, proofLines(
		harness.ProofRun{ID: "size_bound", Started: true, Exit: 1, Output: "=== RUN   TestSizeBound\n--- FAIL: TestSizeBound (0.00s)\n", Report: junitGz(t, sizeBoundFail)},
		harness.ProofRun{ID: "group_open", Started: true, Report: junitGz(t, wrap(`<testcase name="TestGroupOpen"></testcase>`))},
		harness.ProofRun{ID: "all_green", Started: true, Report: junitGz(t, wrap(`<testcase name="TestA"></testcase>`))},
		harness.ProofRun{ID: "holdout-4", Started: true, Exit: 1, Report: junitGz(t, wrap(`<testcase name="TestKeyLog"><failure message="Failed">--- FAIL: TestKeyLog (0.00s)</failure></testcase>`))},
		harness.ProofRun{ID: "holdout-5", Started: true, Report: junitGz(t, wrap(`<testcase name="TestLockLog"></testcase>`))},
	))
	res, err := results[0], errs[0]
	if err != nil {
		t.Fatal(err)
	}
	var list []harness.Proof
	file := func(path, code string) *harness.ProofFile { return &harness.ProofFile{Path: path, Content: code} }
	wantList := []harness.Proof{
		{ID: "size_bound", Suite: "./proofs/size_bound", Name: "TestSizeBound", File: file("proofs/size_bound/size_bound_test.go", sizeBoundCode)},
		{ID: "group_open", Suite: "./acceptance", Name: "TestGroupOpen"},
		{ID: "all_green", Suite: "./..."},
		{ID: "holdout-4", Suite: "./proofs/key_log", Name: "TestKeyLog", File: file("proofs/key_log/key_log_test.go", "package key_log")},
		{ID: "holdout-5", Suite: "./proofs/lock_log", Name: "TestLockLog", File: file("proofs/lock_log/lock_log_test.go", "package lock_log")},
	}
	if len(worker.lists) != 1 || json.Unmarshal([]byte(worker.lists[0]), &list) != nil || !reflect.DeepEqual(list, wantList) {
		t.Fatalf("posted lists = %q, want %+v", worker.lists, wantList)
	}

	runs := gateRuns(blindPlan(), blindRows())
	sizeBound := openedProof{run: runs[0], outcome: proofFailed, output: sizeBoundMsg,
		text: "The criterion: A size over the bound is refused. Inputs: POST /api/sizes with 3.75 against a bound of 3.5. Expected: 400 and no row in the store."}
	door := openedProof{run: runs[2], outcome: proofFlagged, text: "The criterion: A door opens for its key. Inputs: POST /doors/1/open with key 7. Expected: 200."}
	keyLog := openedProof{run: runs[6], outcome: proofFailed, output: "Failed\n--- FAIL: TestKeyLog (0.00s)",
		text: "The hold-out: Every key turn lands in the log. Inputs: three turns of key 7. Expected: three log rows."}
	const late = "Criterion no_proof has no stored proof: the prover filed before a later revision added it."
	const lateJudged = "Criterion judged has no stored proof: the prover filed before a later revision added it."
	wantFindings := []string{openedFinding(sizeBound, "c1"), openedFinding(door, "c1"), openedFinding(keyLog, "c1")}
	if !slices.Equal(res.Findings, wantFindings) || !slices.Equal(res.Card, []string{late, lateJudged}) || res.Escalate != "" || res.Skipped != "" {
		t.Fatalf("findings =\n%s\nwant\n%s\ncard %q, want the late lines", strings.Join(res.Findings, "\n--\n"), strings.Join(wantFindings, "\n--\n"), res.Card)
	}
	wantStand := &CriteriaStand{IDs: []string{"size_bound", "group_open", "door", "no_proof", "all_green", "judged"},
		Open: []string{"size_bound", "door", "no_proof", "judged"}}
	if !reflect.DeepEqual(res.Stand, wantStand) {
		t.Fatalf("stand = %+v, want %+v", res.Stand, wantStand)
	}

	lines := []string{
		"criterion size_bound: TestSizeBound in ./proofs/size_bound failed: Failed === RUN TestSizeBound size_bound_test.go:12: want 3.75, got 3.5 --- FAIL: TestSizeBound (0.00s)",
		"criterion group_open: TestGroupOpen in ./acceptance passed",
		"criterion door: its proof was flagged green on main by its dry run, so it had no blind try",
		late,
		"criterion all_green: suite ./... passed (1 passed, 0 skipped)",
		lateJudged,
		"hold-out key_log: TestKeyLog in ./proofs/key_log failed: Failed --- FAIL: TestKeyLog (0.00s)",
	}
	gate := func(id, verdict, evidence, kind string) CriterionVerdict {
		return CriterionVerdict{Criterion: id, Verdict: verdict, Evidence: evidence, Kind: kind, Source: CriteriaSourceGate, DecidedBy: FactoryDecider}
	}
	wantRows := []CriterionVerdict{
		gate("size_bound", "unmet", "blind: "+lines[0], CriterionTest), gate("group_open", "met", "open: "+lines[1], CriterionTest),
		gate("door", "unmet", "blind: "+lines[2], CriterionTest), gate("no_proof", "unmet", late, CriterionTest),
		gate("all_green", "met", lines[4], CriterionInvariant),
		{Criterion: "judged", Verdict: "unmet", Evidence: "no verdict: " + lateJudged, Kind: CriterionJudgement, Source: CriteriaSourceJudgement, DecidedBy: FactoryDecider},
	}
	rows, _ := store.CriteriaVerdicts(ctx, "t1")
	if !slices.Equal(rows, wantRows) {
		t.Fatalf("rows =\n%+v\nwant\n%+v", rows, wantRows)
	}

	after, _ := store.PlanProofs(ctx, "plan1")
	var states []string
	for _, pp := range after {
		states = append(states, pp.State)
	}
	if want := []string{ProofOpened, ProofOpened, ProofOpened, ProofOpened, ProofStored}; !slices.Equal(states, want) {
		t.Fatalf("row states = %q, want %q", states, want)
	}

	wantEvents := []map[string]any{
		{"criterion": "size_bound", "head": "c1", "outcome": proofFailed, "message": sizeBoundMsg, "path": "proofs/size_bound/size_bound_test.go",
			"suite": "./proofs/size_bound", "name": "TestSizeBound", "code": sizeBoundCode},
		{"criterion": "door", "head": "c1", "outcome": proofFlagged, "message": ProofFlagGreen, "path": "proofs/door/door_test.go",
			"suite": "./proofs/door", "name": "TestDoor", "code": "package door // flagged"},
		{"criterion": "key_log", "head": "c1", "outcome": proofFailed, "message": "Failed\n--- FAIL: TestKeyLog (0.00s)", "path": "proofs/key_log/key_log_test.go",
			"suite": "./proofs/key_log", "name": "TestKeyLog", "code": "package key_log", "holdout": "Every key turn lands in the log: three turns of key 7: three log rows"},
	}
	for _, id := range []string{"t1", "plan1"} {
		if got := gateEvents(store, id); !reflect.DeepEqual(got, wantEvents) {
			t.Fatalf("proof_opened on %s =\n%v\nwant\n%v", id, got, wantEvents)
		}
	}

	step := timelineStep(t, objects, "t1", "gate_criteria")
	if got := string(mustJSON(step["opened"])); got != `["size_bound","door","key_log"]` {
		t.Fatalf("the line's opened = %s", got)
	}
	if got := string(mustJSON(step["holdouts"])); got != `[{"id":"holdout-5","verdict":"green"}]` {
		t.Fatalf("the line's holdouts = %s, want holdout-5 green alone", got)
	}
	var proofs []gateProof
	if err := json.Unmarshal(mustJSON(step["proofs"]), &proofs); err != nil {
		t.Fatal(err)
	}
	wantProofs := []gateProof{
		{ID: "size_bound", Kind: CriterionTest, Suite: "./proofs/size_bound", Name: "TestSizeBound", Outcome: proofFailed, Line: lines[0], Message: sizeBoundMsg,
			State: proofBlind, Path: "proofs/size_bound/size_bound_test.go"},
		{ID: "group_open", Kind: CriterionTest, Suite: "./acceptance", Name: "TestGroupOpen", Outcome: proofPassed, Met: true, Line: lines[1],
			State: proofOpen, Path: "acceptance/group_open_test.go", Changed: true},
		{ID: "door", Kind: CriterionTest, Suite: "./proofs/door", Name: "TestDoor", Line: lines[2], State: proofBlind, Path: "proofs/door/door_test.go"},
		{ID: "no_proof", Kind: CriterionTest, Line: late},
		{ID: "all_green", Kind: CriterionInvariant, Suite: "./...", Outcome: proofPassed, Met: true, Line: lines[4], Message: "1 passed, 0 skipped"},
		{ID: "judged", Kind: CriterionJudgement, Line: lateJudged},
		{ID: "key_log", Kind: CriterionTest, Suite: "./proofs/key_log", Name: "TestKeyLog", Outcome: proofFailed, Line: lines[6],
			Message: "Failed\n--- FAIL: TestKeyLog (0.00s)", State: proofBlind, Path: "proofs/key_log/key_log_test.go", Holdout: true},
	}
	if !slices.Equal(proofs, wantProofs) {
		t.Fatalf("the line's proofs =\n%+v\nwant\n%+v", proofs, wantProofs)
	}

	if len(hub.comments) != 1 {
		t.Fatalf("comments = %q, want one", hub.comments)
	}
	for _, want := range append([]string{"factoryd criteria gate on c1: 2 of 6 criteria met (human criteria ride the approval)."}, lines...) {
		if !strings.Contains(hub.comments[0], want) {
			t.Fatalf("comment = %q, want it to hold %q", hub.comments[0], want)
		}
	}
	surfaces := map[string]string{"the comment": hub.comments[0], "the line": string(mustJSON(step)), "the findings": strings.Join(slices.Concat(res.Findings, res.Card), "\n"),
		"the events": string(mustJSON(store.events))}
	for _, row := range rows {
		surfaces["row "+row.Criterion] = row.Evidence
	}
	for _, leak := range []string{"lock_log", "TestLockLog", "Each lock turn"} {
		for surface, text := range surfaces {
			if strings.Contains(text, leak) {
				t.Fatalf("%s carries %q: %s", surface, leak, text)
			}
		}
	}
}

// An opened proof runs from the tree at the head: a path that holds no
// file there makes no run, and the finding names the path and carries the
// code; a file there runs, and a red run is its line, as any criterion's;
// a file past Forgejo's answer bound runs too, changed. None opens
// anything.
func TestGateCriteriaRunsAnOpenProofFromTheTree(t *testing.T) {
	ctx := context.Background()
	plan := Plan{Criteria: blindPlan().Criteria[1:2]}
	const absent = "criterion group_open: its open proof is not at acceptance/group_open_test.go in c1"

	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, objects)
	seedBlindPlan(t, store, plan, blindRows()[1:2])
	res, err := runCriteria(t, r)
	want := absent + ": commit the test below there.\nTestGroupOpen in ./acceptance:\n```\npackage acceptance // group_open v1\n```"
	if err != nil || !slices.Equal(res.Findings, []string{want}) || len(kube.podOrder) != 0 {
		t.Fatalf("result = %+v, %v, pods %d; want no pod and the finding\n%s", res, err, len(kube.podOrder), want)
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "0 of 1 criteria met") || !strings.Contains(hub.comments[0], absent+"\n") ||
		strings.Contains(hub.comments[0], "package acceptance") {
		t.Fatalf("comments = %q, want the line alone", hub.comments)
	}
	wantRow := CriterionVerdict{Criterion: "group_open", Verdict: "unmet", Evidence: "open: " + absent, Kind: CriterionTest,
		Source: CriteriaSourceGate, DecidedBy: FactoryDecider}
	if rows, _ := store.CriteriaVerdicts(ctx, "t1"); !slices.Equal(rows, []CriterionVerdict{wantRow}) {
		t.Fatalf("rows = %+v, want %+v", rows, wantRow)
	}
	var proofs []gateProof
	if err := json.Unmarshal(mustJSON(timelineStep(t, objects, "t1", "gate_criteria")["proofs"]), &proofs); err != nil || len(proofs) != 1 ||
		proofs[0].State != proofOpen || proofs[0].Outcome != "" || proofs[0].Path != "acceptance/group_open_test.go" {
		t.Fatalf("the line's proofs = %+v, %v; want group_open open with no outcome", proofs, err)
	}

	kube = newFakeKube()
	objects = &fakeObjectStore{}
	r, hub, store = newTestRunner(t, seedCriteriaPlan(t), kube, objects)
	hub.files = map[string]string{"c1:acceptance/group_open_test.go": "package acceptance // group_open v1"}
	proofsWorker(t, r)
	seedBlindPlan(t, store, plan, blindRows()[1:2])
	results, errs := runCriteriaWithPods(t, r, kube, proofLines(harness.ProofRun{ID: "group_open", Exit: 1,
		Report: junitGz(t, wrap(`<testcase name="TestGroupOpen"><failure message="Failed">--- FAIL: TestGroupOpen (0.00s)</failure></testcase>`))}))
	if want := []string{"criterion group_open: TestGroupOpen in ./acceptance failed: Failed --- FAIL: TestGroupOpen (0.00s)"}; errs[0] != nil ||
		!slices.Equal(results[0].Findings, want) {
		t.Fatalf("result = %+v, %v; want the findings %q", results[0], errs[0], want)
	}
	proofs = nil
	if err := json.Unmarshal(mustJSON(timelineStep(t, objects, "t1", "gate_criteria")["proofs"]), &proofs); err != nil || len(proofs) != 1 ||
		proofs[0].Changed || proofs[0].Outcome != proofFailed {
		t.Fatalf("the line's proofs = %+v, %v; want group_open failed, not changed", proofs, err)
	}
	if n := len(gateEvents(store, "t1")) + len(gateEvents(store, "plan1")); n != 0 {
		t.Fatalf("%d proof_opened events, want none", n)
	}

	kube = newFakeKube()
	objects = &fakeObjectStore{}
	r, hub, store = newTestRunner(t, seedCriteriaPlan(t), kube, objects)
	hub.files = map[string]string{"c1:acceptance/group_open_test.go": strings.Repeat("a", maxForgejoAnswer+1)}
	worker := proofsWorker(t, r)
	seedBlindPlan(t, store, plan, blindRows()[1:2])
	results, errs = runCriteriaWithPods(t, r, kube, proofLines(harness.ProofRun{ID: "group_open", Started: true,
		Report: junitGz(t, wrap(`<testcase name="TestGroupOpen"></testcase>`))}))
	if errs[0] != nil || len(results[0].Findings) != 0 || len(worker.lists) != 1 {
		t.Fatalf("a file past the answer's bound: result = %+v, %v, posts %d; want it run and met", results[0], errs[0], len(worker.lists))
	}
	proofs = nil
	if err := json.Unmarshal(mustJSON(timelineStep(t, objects, "t1", "gate_criteria")["proofs"]), &proofs); err != nil || len(proofs) != 1 ||
		!proofs[0].Changed || !proofs[0].Met {
		t.Fatalf("the line's proofs = %+v, %v; want group_open met and changed", proofs, err)
	}
}

// A proof opens once: the next gate at the same head runs it from the
// tree. A factoryd that stops between the event and the row runs the
// blind try again, records no second event and writes the row.
func TestGateCriteriaOpensAProofOnce(t *testing.T) {
	ctx := context.Background()
	plan := Plan{Criteria: blindPlan().Criteria[:1]}
	red := proofLines(harness.ProofRun{ID: "size_bound", Started: true, Exit: 1, Report: junitGz(t, sizeBoundFail)})
	opened := func(store *fakeStore) bool {
		rows, _ := store.PlanProofs(ctx, "plan1")
		return len(rows) == 1 && rows[0].State == ProofOpened
	}

	kube := newFakeKube()
	r, _, store := newTestRunner(t, seedCriteriaPlan(t), kube, &fakeObjectStore{})
	proofsWorker(t, r)
	seedBlindPlan(t, store, plan, blindRows()[:1])
	results, errs := runCriteriaWithPods(t, r, kube, red)
	if errs[0] != nil || len(results[0].Findings) != 1 || !strings.HasPrefix(results[0].Findings[0], "criterion size_bound: its blind proof ran red at c1.") {
		t.Fatalf("the first gate = %+v, %v; want size_bound open", results[0], errs[0])
	}
	second, err := runCriteria(t, r)
	want := "criterion size_bound: its open proof is not at proofs/size_bound/size_bound_test.go in c1: commit the test below there.\n" +
		"TestSizeBound in ./proofs/size_bound:\n```\n" + sizeBoundCode + "```"
	if err != nil || !slices.Equal(second.Findings, []string{want}) || len(kube.podOrder) != 1 {
		t.Fatalf("the second gate = %+v, %v, pods %d; want no pod and the finding\n%s", second, err, len(kube.podOrder), want)
	}
	if !opened(store) || len(gateEvents(store, "t1")) != 1 || len(gateEvents(store, "plan1")) != 1 {
		t.Fatalf("rows %+v, events %v on t1 and %v on plan1; want size_bound opened once", store.proofs["plan1"], gateEvents(store, "t1"), gateEvents(store, "plan1"))
	}

	kube = newFakeKube()
	r, _, store = newTestRunner(t, seedCriteriaPlan(t), kube, &fakeObjectStore{})
	proofsWorker(t, r)
	seedBlindPlan(t, store, plan, blindRows()[:1])
	store.openErr = errors.New("plan_proofs: connection reset")
	results, errs = runCriteriaWithPods(t, r, kube, red, red)
	if errs[0] == nil || !strings.Contains(errs[0].Error(), "plan_proofs: connection reset") {
		t.Fatalf("the first gate's err = %v, want the open's", errs[0])
	}
	if errs[1] != nil || len(results[1].Findings) != 1 || !strings.HasPrefix(results[1].Findings[0], "criterion size_bound: its blind proof ran red at c1.") ||
		len(kube.podOrder) != 2 {
		t.Fatalf("the retry = %+v, %v, pods %d; want the blind try again", results[1], errs[1], len(kube.podOrder))
	}
	if !opened(store) || len(gateEvents(store, "t1")) != 1 || len(gateEvents(store, "plan1")) != 1 {
		t.Fatalf("rows %+v, events %v on t1 and %v on plan1; want size_bound opened, one event each", store.proofs["plan1"], gateEvents(store, "t1"), gateEvents(store, "plan1"))
	}
}

// A blind proof the worker never started had no try: a pod that ended
// before it, a worker that ran no entry, or an entry the worker could not
// start (its budget ran out, or its file could not be placed). It stays
// stored and opens nothing; a criterion's row is unmet and its line,
// which carries no code, is the finding; a hold-out stays out of every
// surface, its verdict on the line unrunnable.
func TestGateCriteriaKeepsABlindProofThePodNeverRan(t *testing.T) {
	ctx := context.Background()
	plan := Plan{Criteria: []PlanCriterion{{ID: "all_green", Kind: CriterionInvariant, Suite: "./..."}, blindPlan().Criteria[0]}}
	green := harness.ProofRun{ID: "all_green", Started: true, Report: junitGz(t, wrap(`<testcase name="TestA"></testcase>`))}
	ended := proofLines(green)
	closing, _ := json.Marshal(harness.WorkerEnvelope{Type: "proofs", Proofs: &harness.Proofs{Error: "proofs mode: the fetch failed"}})
	const outOfBudget = "proofs mode: the run's 35m0s ran out before this entry"
	unstarted := proofLines(green, harness.ProofRun{ID: "size_bound", Exit: -1, Output: outOfBudget},
		harness.ProofRun{ID: "holdout-4", Exit: -1, Output: "proofs mode: write proofs/key_log/key_log_test.go: no room"})
	for name, tc := range map[string]struct {
		body, why string
		rows      []PlanProof
		holdouts  string
	}{
		"a pod that ended early":           {ended[:strings.LastIndex(ended, "\n")], proofsEndedEarly, blindRows()[:1], `[]`},
		"a worker that ran none":           {string(closing), "proofs mode: the fetch failed", []PlanProof{blindRows()[0], blindRows()[3]}, `[{"id":"holdout-4","verdict":"unrunnable"}]`},
		"entries the worker did not start": {unstarted, "check test wrote no report (exit -1): " + outOfBudget, []PlanProof{blindRows()[0], blindRows()[3]}, `[{"id":"holdout-4","verdict":"unrunnable"}]`},
	} {
		t.Run(name, func(t *testing.T) {
			kube := newFakeKube()
			objects := &fakeObjectStore{}
			r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, objects)
			proofsWorker(t, r)
			seedBlindPlan(t, store, plan, tc.rows)
			results, errs := runCriteriaWithPods(t, r, kube, tc.body)
			line := "criterion size_bound: TestSizeBound in ./proofs/size_bound: " + tc.why
			if errs[0] != nil || !slices.Contains(results[0].Findings, line) {
				t.Fatalf("result = %+v, %v; want the finding %q", results[0], errs[0], line)
			}
			rows, _ := store.CriteriaVerdicts(ctx, "t1")
			if i := slices.IndexFunc(rows, func(v CriterionVerdict) bool { return v.Criterion == "size_bound" }); i < 0 ||
				rows[i].Verdict != "unmet" || rows[i].Evidence != "blind: "+line {
				t.Fatalf("rows = %+v, want size_bound unmet, blind", rows)
			}
			proofs, _ := store.PlanProofs(ctx, "plan1")
			for _, pp := range proofs {
				if pp.State != ProofStored {
					t.Fatalf("rows = %+v, want every one stored", proofs)
				}
			}
			if n := len(gateEvents(store, "t1")) + len(gateEvents(store, "plan1")); n != 0 || strings.Contains(strings.Join(hub.comments, "\n"), "package size_bound") {
				t.Fatalf("%d events, comments %q; want no event and no code", n, hub.comments)
			}
			step := timelineStep(t, objects, "t1", "gate_criteria")
			if got := string(mustJSON(step["holdouts"])); got != tc.holdouts {
				t.Fatalf("the line's holdouts = %s, want %s", got, tc.holdouts)
			}
			surfaces := map[string]string{"the comments": strings.Join(hub.comments, "\n"), "the line": string(mustJSON(step)),
				"the findings": strings.Join(results[0].Findings, "\n"), "the events": string(mustJSON(store.events)), "the rows": string(mustJSON(rows))}
			for _, leak := range []string{"key_log", "TestKeyLog", "proofs/key_log", "Every key turn"} {
				for surface, text := range surfaces {
					if strings.Contains(text, leak) {
						t.Fatalf("%s carries %q: %s", surface, leak, text)
					}
				}
			}
		})
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
// activity runs again; nothing ran, so nothing lands. A worker that fails
// (5xx) is asked again within the wait; one that refuses the list (4xx),
// or a pod that has ended, is asked once or not at all, since no retry
// changes its answer.
func TestGateCriteriaFailsWhenThePodTakesNoList(t *testing.T) {
	const wait = 2 * time.Second
	for _, tc := range []struct {
		name   string
		status int
		ended  bool
		want   string
		posts  func(int) bool
		final  bool // given up before the wait
	}{
		{"a failing worker", http.StatusInternalServerError, false, "worker answered 500", func(n int) bool { return n > 1 }, false},
		{"a bad list", http.StatusBadRequest, false, "worker answered 400", func(n int) bool { return n == 1 }, true},
		{"a wrong bearer", http.StatusUnauthorized, false, "worker answered 401", func(n int) bool { return n == 1 }, true},
		{"an ended pod", http.StatusNoContent, true, "is Succeeded: it has ended", func(n int) bool { return n == 0 }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kube := newFakeKube()
			if tc.ended {
				kube.onCreatePod = func() { kube.podOrder[len(kube.podOrder)-1].phase = "Succeeded" }
			}
			objects := &fakeObjectStore{}
			r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, objects)
			r.endpointWait, r.startPoll = wait, 50*time.Millisecond
			worker := &fakeWorkerEndpoint{status: tc.status}
			worker.start(t, r)
			seedProofRows(t, store)
			began := time.Now()
			_, err := runCriteria(t, r)
			if took := time.Since(began); tc.final == (took >= wait) {
				t.Fatalf("the gate took %s against a %s wait, want final %v", took, wait, tc.final)
			}
			if err == nil || !strings.Contains(err.Error(), "hand the proofs pod its list: ") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want the failed post: %s", err, tc.want)
			}
			if !tc.posts(len(worker.lists)) {
				t.Fatalf("%d posts of the list", len(worker.lists))
			}
			if rows, _ := store.CriteriaVerdicts(context.Background(), "t1"); len(rows) != 0 || len(hub.comments) != 0 ||
				strings.Contains(timeline(t, objects), `"step":"gate_criteria"`) {
				t.Fatalf("rows %+v, comments %q: want nothing landed", rows, hub.comments)
			}
		})
	}
}

// A list past the worker's bound would be refused at every try: the gate
// fails for good, naming the size, and no pod starts.
func TestGateCriteriaRefusesAListPastTheWorkersBound(t *testing.T) {
	kube := newFakeKube()
	r, _, store := newTestRunner(t, seedCriteriaPlan(t), kube, &fakeObjectStore{})
	worker := proofsWorker(t, r)
	seedPlan(t, store, map[string]string{"integ": RoleIntegration},
		`{"id":"size_bound","kind":"test","claim":"c","inputs":"i","expected":"e"}`)
	if _, err := store.PutPlanProofs(context.Background(), "plan1", 1, []PlanProof{{Idx: 1, Origin: ProofOriginCriterion, Criterion: "size_bound",
		ID: "size_bound", Suite: "proofs/size_bound", Name: "TestSizeBound", Path: "proofs/size_bound/size_bound_test.go",
		Code: strings.Repeat("a", harness.MaxProofsList), State: ProofStored}}); err != nil {
		t.Fatal(err)
	}
	_, err := runCriteria(t, r)
	if !isTerminal(err) || !strings.Contains(err.Error(), "the proofs list is ") || !strings.Contains(err.Error(), " bytes, past the 4 MiB the worker takes") {
		t.Fatalf("err = %v, want the terminal refusal naming the size", err)
	}
	if len(kube.podOrder) != 0 || len(worker.lists) != 0 {
		t.Fatalf("pods %d, posts %d; want none", len(kube.podOrder), len(worker.lists))
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

// hungDelete is a kube whose namespace delete hangs until its context
// ends, as an API server that takes the call and never answers.
type hungDelete struct{ *fakeKube }

func (hungDelete) DeleteNamespace(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

// GateCriteria outside an activity, as the criteria gate pass runs it,
// ends with its context: the proofs pod never leaves Pending, so the gate
// waits on its start until the context's 100 ms end, and the sandbox's
// teardown, detached from that context, ends at forgetNamespaceBound
// (50 ms here) although the namespace delete hangs.
func TestGateCriteriaEndsWithItsContext(t *testing.T) {
	defer func(d time.Duration) { forgetNamespaceBound = d }(forgetNamespaceBound)
	forgetNamespaceBound = 50 * time.Millisecond
	kube := newFakeKube()
	kube.onCreatePod = func() { kube.podOrder[len(kube.podOrder)-1].phase = "Pending" }
	r, _, store := newTestRunner(t, seedCriteriaPlan(t), kube, &fakeObjectStore{})
	r.kube = hungDelete{kube}
	seedPlan(t, store, map[string]string{"integ": RoleIntegration}, `{"id":"all_green","kind":"invariant","suite":"./..."}`)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := r.GateCriteria(ctx, "t1", 9, "c1")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want the context's deadline", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("GateCriteria still runs 10 s after its context ended")
	}
	if len(kube.podOrder) != 1 {
		t.Errorf("pods %d, want the one proofs pod", len(kube.podOrder))
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
