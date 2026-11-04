package factoryd

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// testProverClassYAML is the infra tree's prover class. The shared test
// policy has none, so the other tests' sweeps open no prover and their
// bounds read the limits alone.
const testProverClassYAML = `  prover:
    session: report
    models: [small, big]
    session_model: big
    supervisor_model: big
    time: {agent_time: 90m, check_in: 0s}
    tools: {file_proofs: {}, manual: {}}
`

// proverPolicy is version 7 of the test policy with the prover class.
func proverPolicy() policy.Version {
	v := testPolicyVersion()
	v.Tree = testTree(strings.Replace(testPolicyYAML, "\nprices:", "\n"+testProverClassYAML+"prices:", 1))
	return v
}

// newProverRunner is newTestRunner under proverPolicy.
func newProverRunner(t *testing.T, task Task, kube *fakeKube, objects ObjectStore) (*Runner, *forgejoHub, *fakeStore) {
	t.Helper()
	r, hub, store := newTestRunner(t, task, kube, objects)
	r.cfg.Policy = proverPolicy
	r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return proverPolicy(), id == 7, nil }
	return r, hub, store
}

// proverPlan is the prover tests' plan: two test criteria, a judgement
// and an invariant; a foundation, a feature and the integration card.
func proverPlan() Plan {
	return Plan{
		Spec:    &Spec{Outcome: "Sizes over the bound are refused, and every key turn is logged.", Glossary: []string{"bound: the largest size a request may name"}},
		Summary: "s",
		Criteria: []PlanCriterion{
			{ID: "size_bound", Kind: CriterionTest, Claim: "A size over the bound is refused", Inputs: "POST /api/sizes with 3.75 against a bound of 3.5",
				Expected: "400 and no row in the store"},
			{ID: "door", Kind: CriterionTest, Claim: "A door opens for its key", Inputs: "POST /doors/1/open with key 7", Expected: "200"},
			{ID: "reads_calm", Kind: CriterionJudgement, Claim: "The sizes page reads calm", Inputs: "the sizes page with three sizes",
				Prompt: "Does the page read calm? Answer met or unmet."},
			{ID: "all_green", Kind: CriterionInvariant, Suite: "./..."},
		},
		Cards: []PlanCard{
			{Key: "foundation", Role: RoleFoundation, Title: "t",
				Brief: "Lay out internal/server.New, the Store interface in internal/store and the user a handler reads from auth.User."},
			{Key: "sizes", Role: RoleFeature, Title: "t", Brief: "b"},
			{Key: "integration", Role: RoleIntegration, Title: "t", Brief: "b"},
		},
	}
}

// keyLog is the plan's okayed hold-out.
func keyLog() PlanProof {
	return PlanProof{Idx: 4, Round: 1, ID: "key_log", Origin: ProofOriginHoldout, Claim: "Every key turn lands in the log",
		Inputs: "three turns of key 7", Expected: "three log rows", State: ProofOkayed}
}

// filedEntry is a valid entry for id: its own file and package, red on main.
func filedEntry(id, name string) FiledProof {
	return FiledProof{Criterion: id, Path: "proofs/" + id + "/" + id + "_test.go", Suite: "./proofs/" + id, Name: name, Code: "package " + id, Outcome: onMainRed}
}

// validFiling is one valid entry per proof proverPlan with keyLog owes.
func validFiling() []FiledProof {
	calm := filedEntry("reads_calm", "TestReadsCalm")
	calm.Artifact = "reads_calm.html"
	return []FiledProof{filedEntry("size_bound", "TestSizeBound"), filedEntry("door", "TestDoor"), calm, filedEntry("key_log", "TestKeyLog")}
}

func planTask() Task {
	return Task{ID: "t-plan", Status: "done", Objective: "size the doors", Repository: "sf/r", SHA: "m0", IssueNumber: 3,
		Track: policy.TrackPlan, Kind: "agent", Source: "webhook", RequiredCapabilities: []string{"planner"}}
}

func proverTask() Task {
	return Task{ID: "t-prove", Status: "running", Objective: "size the doors", Repository: "sf/r", SHA: "m0", Parent: "t-plan", Process: "maintenance",
		Source: SourceProver, Track: policy.TrackDirect, Kind: "agent", RequiredCapabilities: []string{policy.ClassProver}, BudgetEUR: 2}
}

// seedProver files p as t-plan's revision 1 beside rows, with the plan
// task in the store.
func seedProver(t *testing.T, store *fakeStore, p Plan, rows ...PlanProof) {
	t.Helper()
	ctx := context.Background()
	if _, ok, _ := store.Task(ctx, "t-plan"); !ok {
		if err := store.CreateTask(ctx, planTask()); err != nil {
			t.Fatal(err)
		}
	}
	b := mustJSON(p)
	if _, err := store.AddPlanRevision(ctx, "t-plan", b, fmt.Sprintf("%x", sha256.Sum256(b))); err != nil {
		t.Fatal(err)
	}
	if len(rows) > 0 {
		if _, err := store.PutPlanProofs(ctx, "t-plan", 1, rows); err != nil {
			t.Fatal(err)
		}
	}
}

// planEvents are t-plan's events of kind, oldest first.
func planEvents(store *fakeStore, kind string) []Event {
	var out []Event
	for _, e := range store.events {
		if e.TaskID == "t-plan" && e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// A filing's shape is checked at once, every finding named: per entry in
// filing order, then the proofs missing in due order. No test runs.
func TestProofFindingsCheckTheShape(t *testing.T) {
	due := []proofDue{{"size_bound", CriterionTest, 0}, {"reads_calm", CriterionJudgement, 0}, {"pages_ok", CriterionHuman, 0}, {"key_log", CriterionTest, 4}}
	valid := func() []FiledProof {
		calm := filedEntry("reads_calm", "TestReadsCalm")
		calm.Artifact = "reads_calm.html"
		pages := filedEntry("pages_ok", "TestPagesOk")
		pages.Outcome, pages.Artifact = onMainGreen, "pages_ok.txt"
		return []FiledProof{filedEntry("size_bound", "TestSizeBound"), calm, pages, filedEntry("key_log", "TestKeyLog")}
	}
	if got := proofFindings(due, valid()); got != nil {
		t.Fatalf("the valid filing = %q, want no finding", got)
	}
	const path = `file_proofs: size_bound: path `
	for name, tc := range map[string]struct {
		change func([]FiledProof) []FiledProof
		want   []string
	}{
		"left out": {func(f []FiledProof) []FiledProof { return slices.Delete(f, 2, 3) }, []string{"file_proofs: no proof for pages_ok"}},
		"a ghost": {func(f []FiledProof) []FiledProof { return append(f, filedEntry("ghost", "TestGhost")) },
			[]string{"file_proofs: ghost is no criterion or hold-out of this plan"}},
		"twice": {func(f []FiledProof) []FiledProof { return append(f, f[0]) }, []string{"file_proofs: size_bound is filed twice"}},
		"outside": {func(f []FiledProof) []FiledProof { f[0].Path = "../x_test.go"; return f },
			[]string{path + `"../x_test.go": a relative path inside the repository`}},
		"absolute": {func(f []FiledProof) []FiledProof { f[0].Path = "/abs/x_test.go"; return f },
			[]string{path + `"/abs/x_test.go": a relative path inside the repository`}},
		"a workflow": {func(f []FiledProof) []FiledProof { f[0].Path = ".forgejo/workflows/x_test.go"; return f },
			[]string{path + `".forgejo/workflows/x_test.go": closed to agents, the template owns it`}},
		"a workflow in capitals": {func(f []FiledProof) []FiledProof { f[0].Path = ".GitHub/x_test.go"; return f },
			[]string{path + `".GitHub/x_test.go": closed to agents, the template owns it`}},
		"no test file": {func(f []FiledProof) []FiledProof { f[0].Path = "proofs/size_bound/notes.go"; return f },
			[]string{path + `"proofs/size_bound/notes.go": a test file the project's tools run, its name ending in _test.go, .test.ts or .spec.ts`}},
		"a shared path": {func(f []FiledProof) []FiledProof { f[3].Path = f[0].Path; return f },
			[]string{`file_proofs: key_log: path "proofs/size_bound/size_bound_test.go" is size_bound's too`}},
		"a shared path once cleaned": {func(f []FiledProof) []FiledProof { f[3].Path = "proofs/x/../size_bound/size_bound_test.go"; return f },
			[]string{`file_proofs: key_log: path "proofs/x/../size_bound/size_bound_test.go" is size_bound's too`}},
		"a flag for a suite": {func(f []FiledProof) []FiledProof { f[0].Suite = "-race"; return f },
			[]string{`file_proofs: size_bound: suite "-race": a package path such as ./proofs/size_bound`}},
		"a suite outside": {func(f []FiledProof) []FiledProof { f[0].Suite = "proofs/../.."; return f },
			[]string{`file_proofs: size_bound: suite "proofs/../..": a package path such as ./proofs/size_bound`}},
		"no name": {func(f []FiledProof) []FiledProof { f[0].Name = ""; return f }, []string{"file_proofs: size_bound: name is empty"}},
		"a pattern for a name": {func(f []FiledProof) []FiledProof { f[0].Name = "Test.*|TestOther"; return f },
			[]string{`file_proofs: size_bound: name "Test.*|TestOther": a test's name of letters, digits, _ and the / of a subtest, such as TestSizeBound or TestSizeBound/over`}},
		"no code": {func(f []FiledProof) []FiledProof { f[0].Code = ""; return f }, []string{"file_proofs: size_bound: code is empty"}},
		"too much code": {func(f []FiledProof) []FiledProof { f[0].Code = strings.Repeat("x", 16385); return f },
			[]string{"file_proofs: size_bound: code is 16385 bytes; at most 16384"}},
		"no such outcome": {func(f []FiledProof) []FiledProof { f[0].Outcome = "passed"; return f },
			[]string{`file_proofs: size_bound: outcome "passed": red, green or unrunnable`}},
		"green on main": {func(f []FiledProof) []FiledProof { f[0].Outcome = onMainGreen; return f },
			[]string{"file_proofs: size_bound: your run on main was green: a test of kind test must fail there, where the feature is not built yet"}},
		"a hold-out unrunnable": {func(f []FiledProof) []FiledProof { f[3].Outcome = onMainUnrunnable; return f },
			[]string{"file_proofs: key_log: your run on main was unrunnable: a test of kind test must run there and fail"}},
		"a judgement unrunnable": {func(f []FiledProof) []FiledProof { f[1].Outcome = onMainUnrunnable; return f },
			[]string{"file_proofs: reads_calm: your run on main was unrunnable: a test of kind judgement must run there and write its artifact"}},
		"a test's artifact": {func(f []FiledProof) []FiledProof { f[0].Artifact = "x.txt"; return f },
			[]string{"file_proofs: size_bound: a test of kind test writes no artifact"}},
		"a human's artifact elsewhere": {func(f []FiledProof) []FiledProof { f[2].Artifact = "pages_ok/shot.png"; return f },
			[]string{"file_proofs: pages_ok: a human criterion names the file its test writes under CHECK_ARTIFACTS: pages_ok or pages_ok.<ext>"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := proofFindings(due, tc.change(valid())); !slices.Equal(got, tc.want) {
				t.Fatalf("findings =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
	for _, artifact := range []string{"", "out/reads_calm.html", "page.html", "reads_calm.", "reads_calm.h-t"} {
		f := valid()
		f[1].Artifact = artifact
		want := []string{"file_proofs: reads_calm: a judgement criterion names the file its test writes under CHECK_ARTIFACTS: reads_calm or reads_calm.<ext>"}
		if got := proofFindings(due, f); !slices.Equal(got, want) {
			t.Errorf("artifact %q: findings = %q, want %q", artifact, got, want)
		}
	}
	f := valid()
	f[1].Artifact = "reads_calm"
	if got := proofFindings(due, f); got != nil {
		t.Errorf("the artifact named by the criterion alone = %q, want no finding", got)
	}
}

// A filing's findings as the plan's timeline holds them name a hold-out by
// its entry id alone, with none of its test's path, suite or name.
func TestProofFindingsLogAHoldoutByItsEntryAlone(t *testing.T) {
	due := []proofDue{{"size_bound", CriterionTest, 0}, {"key_log", CriterionTest, 4}}
	f := []FiledProof{filedEntry("size_bound", "TestSizeBound"), filedEntry("key_log", "TestKeyLog")}
	f[0].Path = f[1].Path
	f[1].Path, f[1].Suite, f[1].Name, f[1].Outcome = "proofs/key_log/key_log.go", "-key_log", "TestKeyLog?", "green-ish key_log"
	var logged []string
	for _, x := range checkProofs(due, append(f, f[1])) {
		logged = append(logged, x.logged)
	}
	want := []string{
		"file_proofs: holdout-4: path: a test file the project's tools run, its name ending in _test.go, .test.ts or .spec.ts",
		"file_proofs: holdout-4: suite: a package path inside the repository",
		"file_proofs: holdout-4: name: a test's name of letters, digits, _ and the / of a subtest, such as TestSizeBound or TestSizeBound/over",
		"file_proofs: holdout-4: outcome: red, green or unrunnable",
		"file_proofs: holdout-4 is filed twice",
	}
	if !slices.Equal(logged, want) {
		t.Fatalf("logged =\n%q\nwant\n%q", logged, want)
	}
	f = []FiledProof{filedEntry("size_bound", "TestSizeBound"), filedEntry("key_log", "TestKeyLog")}
	f[1].Path = f[0].Path
	got := checkProofs(due, f)
	if len(got) != 1 || got[0].logged != "file_proofs: holdout-4: path is size_bound's too" {
		t.Fatalf("a hold-out on a criterion's path = %+v", got)
	}
	got = checkProofs(due, f[:1])
	if len(got) != 1 || got[0].text != "file_proofs: no proof for key_log" || got[0].logged != "file_proofs: no proof for holdout-4" {
		t.Fatalf("a missing hold-out = %+v", got)
	}
}

// One filing per call; an accepted one is the plan's rows in state filed,
// and the next accepted one replaces it in place. The three filings are
// counted from the plan's proofs_filed events, so a runner built anew
// counts on, and no event carries a test's code.
func TestFileProofsKeepsTheFilingAndCountsThree(t *testing.T) {
	ctx := context.Background()
	objects := &fakeObjectStore{}
	r, _, store := newProverRunner(t, proverTask(), newFakeKube(), objects)
	p := Plan{Summary: "s", Criteria: []PlanCriterion{proverPlan().Criteria[0], proverPlan().Criteria[3]}}
	seedProver(t, store, p)
	check := func(n int, proofs []FiledProof, want ProofsFiled) {
		t.Helper()
		got, err := r.FileProofs(ctx, "t-prove", proofs)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("call %d = %+v, %v; want %+v", n, got, err, want)
		}
	}
	check(1, nil, ProofsFiled{Filing: 1, Left: 2, Findings: []string{"file_proofs: no proof for size_bound"}})
	if rows, _ := store.PlanProofs(ctx, "t-plan"); len(rows) != 0 {
		t.Fatalf("rows after a refused filing = %+v", rows)
	}
	check(2, []FiledProof{filedEntry("size_bound", "TestSizeBound")}, ProofsFiled{Filing: 2, Left: 1})
	row := PlanProof{Idx: 1, Round: 1, ID: "size_bound", Origin: ProofOriginCriterion, Criterion: "size_bound", Claim: "A size over the bound is refused",
		Inputs: "POST /api/sizes with 3.75 against a bound of 3.5", Expected: "400 and no row in the store", State: ProofFiled,
		Path: "proofs/size_bound/size_bound_test.go", Suite: "./proofs/size_bound", Name: "TestSizeBound", Code: "package size_bound"}
	if rows, _ := store.PlanProofs(ctx, "t-plan"); !slices.Equal(rows, []PlanProof{row}) {
		t.Fatalf("rows = %+v, want %+v", rows, row)
	}
	third := filedEntry("size_bound", "TestSizeBound")
	third.Code = "package size_bound // v3"
	check(3, []FiledProof{third}, ProofsFiled{Filing: 3, Left: 0})
	row.Code = third.Code
	if rows, _ := store.PlanProofs(ctx, "t-plan"); !slices.Equal(rows, []PlanProof{row}) {
		t.Fatalf("rows after the third = %+v, want %+v", rows, row)
	}
	r = &Runner{store: store, cfg: r.cfg, timeline: &Timeline{Objects: objects, Events: store, Store: store}}
	check(4, []FiledProof{filedEntry("size_bound", "TestSizeBound")}, ProofsFiled{Filing: 3, Left: 0,
		Findings: []string{"file_proofs: three filings are this session's limit, and filing 3, the last one that passed, stands: end the session"}})
	if rows, _ := store.PlanProofs(ctx, "t-plan"); !slices.Equal(rows, []PlanProof{row}) {
		t.Fatalf("rows after the cap = %+v, want them as they were", rows)
	}
	events := planEvents(store, EventProofsFiled)
	if len(events) != 3 {
		t.Fatalf("proofs_filed events = %+v, want three", events)
	}
	for i, e := range events {
		if e.Detail["filing"] != i+1 || e.Detail["prover"] != "t-prove" {
			t.Fatalf("event %d = %+v", i, e.Detail)
		}
		if strings.Contains(string(mustJSON(e.Detail)), "package size_bound") {
			t.Fatalf("event %d carries the code: %s", i, mustJSON(e.Detail))
		}
	}
	if got, want := string(mustJSON(events[0].Detail)),
		`{"filing":1,"proofs":[],"prover":"t-prove","refusals":["file_proofs: no proof for size_bound"]}`; got != want {
		t.Fatalf("filing 1 = %s, want %s", got, want)
	}
	if got, want := string(mustJSON(events[1].Detail)), `{"filing":2,"proofs":[{"criterion":"size_bound","name":"TestSizeBound","outcome":"red",`+
		`"path":"proofs/size_bound/size_bound_test.go","suite":"./proofs/size_bound"}],"prover":"t-prove","refusals":[]}`; got != want {
		t.Fatalf("filing 2 = %s, want %s", got, want)
	}
}

// Only a running prover of a plan files: a done one, or a task that is no
// prover, is told so and records nothing.
func TestFileProofsRefusesATaskThatIsNoRunningProver(t *testing.T) {
	ctx := context.Background()
	done := proverTask()
	done.Status = "done"
	r, _, store := newProverRunner(t, done, newFakeKube(), &fakeObjectStore{})
	seedProver(t, store, proverPlan())
	if got, err := r.FileProofs(ctx, "t-prove", validFiling()); err != nil ||
		!reflect.DeepEqual(got, ProofsFiled{Findings: []string{"file_proofs: the task is done, not running"}}) {
		t.Fatalf("a done prover = %+v, %v", got, err)
	}
	coder := parentTask()
	coder.Status = "running"
	if err := store.CreateTask(ctx, coder); err != nil {
		t.Fatal(err)
	}
	if got, err := r.FileProofs(ctx, coder.ID, validFiling()); err != nil ||
		!reflect.DeepEqual(got, ProofsFiled{Findings: []string{"file_proofs: the task is no plan's prover"}}) {
		t.Fatalf("a coder task = %+v, %v", got, err)
	}
	if events := planEvents(store, EventProofsFiled); len(events) != 0 {
		t.Fatalf("events = %+v, want none", events)
	}
}

// Outside the prover's brief a hold-out is its entry id alone: the
// filing's event names it holdout-<idx> with its outcome, and neither an
// accepted filing's nor a refused one's detail holds its id, its test's
// name or path, or its claim. Its row takes the test, its claim kept.
func TestFileProofsNamesAHoldoutByItsEntryAlone(t *testing.T) {
	ctx := context.Background()
	r, _, store := newProverRunner(t, proverTask(), newFakeKube(), &fakeObjectStore{})
	seedProver(t, store, Plan{Summary: "s", Criteria: proverPlan().Criteria[:1]}, keyLog())
	refused := []FiledProof{filedEntry("key_log", ""), filedEntry("size_bound", "TestSizeBound"), filedEntry("key_lg", "TestKeyLg")}
	refused[1].Path, refused[1].Suite = refused[0].Path, "-proofs/key_log"
	refused[2].Outcome = "red, key_log in proofs/key_log"
	if got, err := r.FileProofs(ctx, "t-prove", refused); err != nil || len(got.Findings) != 4 ||
		got.Findings[3] != "file_proofs: key_lg is no criterion or hold-out of this plan" {
		t.Fatalf("the refused filing = %+v, %v; want four findings, the prover told the id it filed", got, err)
	}
	filing := []FiledProof{filedEntry("size_bound", "TestSizeBound"), filedEntry("key_log", "TestKeyLog")}
	if got, err := r.FileProofs(ctx, "t-prove", filing); err != nil || len(got.Findings) != 0 {
		t.Fatalf("the filing = %+v, %v", got, err)
	}
	events := planEvents(store, EventProofsFiled)
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	if got, want := string(mustJSON(events[0].Detail["refusals"])),
		`["file_proofs: holdout-4: name is empty","file_proofs: size_bound: path is holdout-4's too",`+
			`"file_proofs: size_bound: suite: a package path inside the repository","file_proofs: an entry names no criterion or hold-out of this plan"]`; got != want {
		t.Fatalf("the refusals = %s, want %s", got, want)
	}
	if got, want := string(mustJSON(events[0].Detail["proofs"])), `[{"criterion":"holdout-4","outcome":"red"},{"criterion":"size_bound","outcome":"red"},{"outcome":"invalid"}]`; got != want {
		t.Fatalf("the refused entries = %s, want %s: the criterion on the hold-out's path by its id alone", got, want)
	}
	entries := events[1].Detail["proofs"].([]map[string]any)
	if len(entries) != 2 || !reflect.DeepEqual(entries[1], map[string]any{"criterion": "holdout-4", "outcome": "red"}) {
		t.Fatalf("entries = %v, want the hold-out as holdout-4 and its outcome", entries)
	}
	for _, e := range events {
		detail := string(mustJSON(e.Detail))
		for _, leak := range []string{"key_log", "key_lg", "TestKeyLog", "proofs/key_log", "Every key turn"} {
			if strings.Contains(detail, leak) {
				t.Fatalf("the detail holds %q: %s", leak, detail)
			}
		}
	}
	rows, _ := store.PlanProofs(ctx, "t-plan")
	i := slices.IndexFunc(rows, func(pp PlanProof) bool { return pp.Idx == 4 })
	want := keyLog()
	want.State, want.Path, want.Suite, want.Name, want.Code = ProofFiled, "proofs/key_log/key_log_test.go", "./proofs/key_log", "TestKeyLog", "package key_log"
	if i < 0 || rows[i] != want {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
}

// The gateway's file_proofs call reaches the runner as filed: the
// gateway's client and factoryd's handler speak one body.
func TestProofsFileHandlerTakesTheGatewaysCall(t *testing.T) {
	r, _, store := newProverRunner(t, proverTask(), newFakeKube(), &fakeObjectStore{})
	seedProver(t, store, proverPlan(), keyLog())
	srv := httptest.NewServer(ProofsFileHandler(r))
	defer srv.Close()
	var in []gateway.ProofIn
	for _, e := range validFiling() {
		in = append(in, gateway.ProofIn{Criterion: e.Criterion, Path: e.Path, Suite: e.Suite, Name: e.Name, Code: e.Code, Outcome: e.Outcome, Artifact: e.Artifact})
	}
	f := &gateway.Factoryd{BaseURL: srv.URL, HTTP: srv.Client()}
	got, err := f.FileProofs(context.Background(), "t-prove", in)
	if err != nil || !reflect.DeepEqual(got, gateway.ProofsFiled{Filing: 1, Left: 2}) {
		t.Fatalf("filed = %+v, %v", got, err)
	}
	rows, _ := store.PlanProofs(context.Background(), "t-plan")
	if len(rows) != 4 || rows[3].Artifact != "reads_calm.html" || rows[3].Criterion != "reads_calm" || rows[0].Code != "package key_log" {
		t.Fatalf("rows = %+v", rows)
	}
	if _, err := f.FileProofs(context.Background(), "t-none", in); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("an unknown task = %v, want factoryd's 404", err)
	}
}

func TestProverGuideHoldsTheCriteriaTheSurfaceAndTheRun(t *testing.T) {
	foundation := proverPlan().Cards[0]
	g := proverGuide(proverPlan(), []PlanProof{keyLog()}, &foundation)
	if !strings.HasPrefix(g, "You are the prover. ") {
		t.Fatalf("guide = %q", g)
	}
	for _, want := range []string{
		"Spec: Sizes over the bound are refused, and every key turn is logged.\nGlossary:\n- bound: the largest size a request may name\n",
		"- size_bound (test): A size over the bound is refused. Inputs: POST /api/sizes with 3.75 against a bound of 3.5. Expected: 400 and no row in the store.\n",
		"- door (test): A door opens for its key. Inputs: POST /doors/1/open with key 7. Expected: 200.\n",
		"- reads_calm (judgement): The sizes page reads calm. The artifact shows: the sizes page with three sizes. Write it to $CHECK_ARTIFACTS/reads_calm.<ext>.\n",
		"- key_log (hold-out, kind test): Every key turn lands in the log. Inputs: three turns of key 7. Expected: three log rows.\n",
		"the HTTP API through the server constructor the foundation names (internal/server.New unless it names another)",
		"The foundation card foundation is on main. Its brief named the contracts every card builds on; read them there:\nLay out internal/server.New, ",
		"mkdir -p /tmp/art && CHECK_REPORT=/tmp/<id>.xml CHECK_ARTIFACTS=/tmp/art bash check test <suite> <name>",
		"three filings at most; the last filing stands",
	} {
		if !strings.Contains(g, want) {
			t.Errorf("guide lacks %q:\n%s", want, g)
		}
	}
	for _, leak := range []string{"all_green", "Does the page read calm"} {
		if strings.Contains(g, leak) {
			t.Errorf("guide holds %q", leak)
		}
	}
	human := Plan{Criteria: []PlanCriterion{{ID: "pages_ok", Kind: CriterionHuman, Claim: "The pages look right.", Evidence: "a screenshot of each page"}}}
	if g := proverGuide(human, nil, nil); strings.Contains(g, "The foundation card") || strings.Contains(g, "Spec:") ||
		!strings.Contains(g, "- pages_ok (human): The pages look right. The artifact shows: a screenshot of each page. Write it to $CHECK_ARTIFACTS/pages_ok.<ext>.\n") {
		t.Fatalf("guide without a foundation or a spec = %q", g)
	}
}

func TestReportTimeKeepsTheDryRunsShare(t *testing.T) {
	for _, tc := range []struct{ agent, reserve, want time.Duration }{{2 * time.Hour, 0, 2 * time.Hour}, {90 * time.Minute, 50 * time.Minute, 40 * time.Minute}} {
		if got, err := reportTime(tc.agent, tc.reserve); err != nil || got != tc.want {
			t.Errorf("reportTime(%s, %s) = %s, %v; want %s", tc.agent, tc.reserve, got, err, tc.want)
		}
	}
	_, err := reportTime(55*time.Minute, 50*time.Minute)
	if want := "agent_time 55m0s leaves the session 5m0s once 50m0s are kept for the work after it: raise the class's agent_time to at least 1h0m0s"; err == nil ||
		!isTerminal(err) || errMessage(err) != want {
		t.Fatalf("err = %v, want the terminal %q", err, want)
	}
}

func TestProofFlag(t *testing.T) {
	html := harness.ProofRun{Artifacts: map[string][]byte{"a.html": []byte("<p>")}}
	txt := harness.ProofRun{Artifacts: map[string][]byte{"a.txt": []byte("t")}}
	for _, tc := range []struct {
		kind, outcome string
		run           harness.ProofRun
		artifact      string
		want          string
	}{
		{CriterionTest, proofFailed, harness.ProofRun{}, "", ""},
		{CriterionTest, proofPassed, harness.ProofRun{}, "", ProofFlagGreen},
		{CriterionTest, proofMissing, harness.ProofRun{}, "", ProofFlagUnrunnable},
		{CriterionTest, proofSkipped, harness.ProofRun{}, "", ProofFlagUnrunnable},
		{CriterionTest, proofNoReport, harness.ProofRun{}, "", ProofFlagUnrunnable},
		{CriterionJudgement, proofFailed, html, "a.html", ""},
		{CriterionJudgement, proofPassed, html, "a.html", ""},
		{CriterionJudgement, proofFailed, harness.ProofRun{}, "a.html", ProofFlagUnrunnable},
		{CriterionJudgement, proofPassed, txt, "a.html", ProofFlagUnrunnable},
		{CriterionHuman, proofMissing, txt, "a.txt", ProofFlagUnrunnable},
	} {
		if got := proofFlag(tc.kind, proofOutcome{Outcome: tc.outcome}, tc.run, tc.artifact); got != tc.want {
			t.Errorf("proofFlag(%s, %s, %v, %q) = %q, want %q", tc.kind, tc.outcome, tc.run.Artifacts, tc.artifact, got, tc.want)
		}
	}
}

// The dry run's pod: the four filed proofs on main, the first failing, the
// judgement passing with its artifact (no flag, where a test would be
// green), door passing, the hold-out not building.
func dryRunLines(t *testing.T) string {
	return proofLines(
		harness.ProofRun{ID: "size_bound", Exit: 1, Report: junitGz(t, wrap(`<testcase name="TestSizeBound"><failure message="Failed">want 400, got 404</failure></testcase>`))},
		harness.ProofRun{ID: "reads_calm", Report: junitGz(t, wrap(`<testcase name="TestReadsCalm"></testcase>`)),
			Artifacts: map[string][]byte{"reads_calm.html": []byte("<p>no page yet</p>")}},
		harness.ProofRun{ID: "door", Report: junitGz(t, wrap(`<testcase name="TestDoor"></testcase>`))},
		harness.ProofRun{ID: "holdout-4", Exit: 1, Output: "FAIL ./proofs/key_log [build failed]"},
	)
}

// withoutDryRunShare runs a test's prover sessions with no time kept for
// the dry run: the test activity's 10 minutes hold no 50 to keep.
func withoutDryRunShare(t *testing.T) {
	t.Helper()
	keep := proverDryRun
	proverDryRun = 0
	t.Cleanup(func() { proverDryRun = keep })
}

// The prover's report session gets its guide and writes its role on its
// lines; its filing is the proofs the dry run then runs on main. Its
// summary is factoryd's words, never the model's last message.
func TestRunReportOfTheProverHandsTheSessionItsGuideAndRole(t *testing.T) {
	withoutDryRunShare(t)
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, _, store := newProverRunner(t, proverTask(), kube, objects)
	proofsWorker(t, r)
	seedProver(t, store, proverPlan(), keyLog())
	ctx := context.Background()
	if err := store.CreateTask(ctx, Task{ID: "t-found", Status: "done", Repository: "sf/r", Kind: "agent", Source: SourcePlan, Parent: "t-plan",
		PlanKey: "foundation", Track: policy.TrackDirect, MergeCommit: "f1"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() {
		_, err := execActivity(t, r, ActivityRunReport, "t-prove", "factory/small")
		done <- err
	}()
	awaitPod(t, kube, done)
	if got, err := r.FileProofs(ctx, "t-prove", validFiling()); err != nil || len(got.Findings) != 0 {
		t.Fatalf("the filing = %+v, %v", got, err)
	}
	const last = "filed four proofs: key_log in proofs/key_log, package key_log"
	session := envLine(harness.Event{Kind: harness.EventFinished, Time: time.Now(), Detail: map[string]any{"success": true, "summary": last}}) +
		"\n" + resultLine(true, last)
	scriptReproPods(kube, session, dryRunLines(t))
	if err := <-done; err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	if guide := podEnv(kube.podAt(0))["REPORT_GUIDE"]; !strings.HasPrefix(guide, "You are the prover.") || !strings.Contains(guide, "- size_bound (test): ") {
		t.Fatalf("REPORT_GUIDE = %q", guide)
	}
	if env := podEnv(kube.podAt(1)); env["MODE"] != "proofs" {
		t.Fatalf("the second pod's env = %v, want the proofs pod", env)
	}
	for _, step := range []string{"session_start", "session_end"} {
		if role := timelineStep(t, objects, "t-prove", step)["role"]; role != "prover" {
			t.Errorf("%s role = %v, want prover", step, role)
		}
	}
	const summary = "filed 4 proofs, filing 1"
	if got, _, _ := store.Task(ctx, "t-prove"); got.AgentSummary != summary {
		t.Fatalf("AgentSummary = %q, want %q", got.AgentSummary, summary)
	}
	if got := timelineStep(t, objects, "t-prove", "session_end")["summary"]; got != summary {
		t.Fatalf("session_end summary = %v, want %q", got, summary)
	}
	b, _ := objects.Get(ctx, "sessions/t-prove/timeline.jsonl")
	if strings.Contains(string(b), `"step":"report"`) || strings.Contains(string(b), "key_log") || strings.Contains(string(b), "four proofs") {
		t.Fatalf("the prover's timeline holds a report line or the model's words: %s", b)
	}
	if n := len(planEvents(store, EventProofsStored)); n != 1 {
		t.Fatalf("proofs_stored events = %d, want 1", n)
	}
}

// After the session the filing runs on main, fetched at main's head and
// checked out at the commit the session read: each row is stored, a test
// green there or a proof that did not run flagged. A retry stores nothing
// twice and starts no pod.
func TestRunReportOfTheProverDryRunsTheFilingOnMain(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	task := proverTask()
	task.AgentSummary = "filed 4 proofs, filing 1"
	r, hub, store := newProverRunner(t, task, kube, objects)
	hub.mainHead = "m1"
	worker := proofsWorker(t, r)
	filed := func(idx int, e FiledProof) PlanProof {
		return PlanProof{Idx: idx, Round: 1, ID: e.Criterion, Origin: ProofOriginCriterion, Criterion: e.Criterion, State: ProofFiled,
			Path: e.Path, Suite: e.Suite, Name: e.Name, Code: e.Code, Artifact: e.Artifact}
	}
	f := validFiling()
	hold := keyLog()
	hold.State, hold.Path, hold.Suite, hold.Name, hold.Code = ProofFiled, f[3].Path, f[3].Suite, f[3].Name, f[3].Code
	seedProver(t, store, proverPlan(), filed(1, f[0]), filed(2, f[2]), filed(3, f[1]), hold)
	done := make(chan error)
	go func() {
		_, err := execActivity(t, r, ActivityRunReport, "t-prove", "factory/small")
		done <- err
	}()
	scriptReproPods(kube, dryRunLines(t))
	if err := <-done; err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	if len(kube.podOrder) != 1 {
		t.Fatalf("pods = %d, want the proofs pod alone", len(kube.podOrder))
	}
	fetch := containerEnv(initContainer(t, kube.podAt(0).spec["spec"].(map[string]any), "fetch"))
	if fetch["PR_REF"] != "refs/heads/main" || fetch["REPO_SHA"] != "m1" || fetch["CHECKOUT_SHA"] != "m0" {
		t.Fatalf("fetch env = %v, want main at m1, checked out at m0", fetch)
	}
	var list []harness.Proof
	if len(worker.lists) != 1 || json.Unmarshal([]byte(worker.lists[0]), &list) != nil {
		t.Fatalf("posted lists = %q", worker.lists)
	}
	want := []harness.Proof{storedProof(filed(1, f[0])), storedProof(filed(2, f[2])), storedProof(filed(3, f[1])), storedProof(hold)}
	if !reflect.DeepEqual(list, want) || list[3].ID != "holdout-4" || list[0].File == nil {
		t.Fatalf("list = %+v, want %+v", list, want)
	}
	rows, _ := store.PlanProofs(context.Background(), "t-plan")
	var got []string
	for _, pp := range rows {
		got = append(got, pp.ID+" "+pp.State+" "+pp.Flag)
	}
	if want := []string{"size_bound stored ", "reads_calm stored ", "door stored green on main", "key_log stored unrunnable"}; !slices.Equal(got, want) {
		t.Fatalf("rows = %q, want %q", got, want)
	}
	stored := planEvents(store, EventProofsStored)
	if len(stored) != 1 {
		t.Fatalf("proofs_stored = %+v", stored)
	}
	if got, want := string(mustJSON(stored[0].Detail)), `{"commit":"m0","flagged":[{"criterion":"door","flag":"green on main"},`+
		`{"criterion":"holdout-4","flag":"unrunnable"}],"prover":"t-prove","stored":["size_bound","reads_calm","door","holdout-4"]}`; got != want {
		t.Fatalf("proofs_stored = %s, want %s", got, want)
	}
	if _, err := execActivity(t, r, ActivityRunReport, "t-prove", "factory/small"); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if len(kube.podOrder) != 1 || len(planEvents(store, EventProofsStored)) != 1 {
		t.Fatalf("pods %d, proofs_stored %d after the retry; want one each", len(kube.podOrder), len(planEvents(store, EventProofsStored)))
	}
}

// A store that went through before its event landed is not stored twice:
// the retry finds no filed row, stored ones, and ends with no pod.
func TestRunReportOfTheProverKeepsAStoreWhoseEventDidNotLand(t *testing.T) {
	kube := newFakeKube()
	task := proverTask()
	task.AgentSummary = "filed one proof"
	r, _, store := newProverRunner(t, task, kube, &fakeObjectStore{})
	row := PlanProof{Idx: 1, Round: 1, ID: "size_bound", Origin: ProofOriginCriterion, Criterion: "size_bound", State: ProofStored,
		Path: "proofs/size_bound/size_bound_test.go", Suite: "./proofs/size_bound", Name: "TestSizeBound", Code: "package size_bound"}
	seedProver(t, store, proverPlan(), row)
	if _, err := execActivity(t, r, ActivityRunReport, "t-prove", "factory/small"); err != nil || len(kube.podOrder) != 0 {
		t.Fatalf("err %v, pods %d; want none", err, len(kube.podOrder))
	}
}

// A prover that filed nothing fails in factoryd's words, with no pod.
func TestRunReportOfTheProverWithoutAFilingFails(t *testing.T) {
	kube := newFakeKube()
	task := proverTask()
	task.AgentSummary = "the prover filed no proofs"
	r, _, store := newProverRunner(t, task, kube, &fakeObjectStore{})
	seedProver(t, store, proverPlan(), keyLog())
	_, err := execActivity(t, r, ActivityRunReport, "t-prove", "factory/small")
	if err == nil || !isTerminal(err) || errMessage(err) != "the prover filed no proofs" {
		t.Fatalf("err = %v, want the terminal error in factoryd's words", err)
	}
	if len(kube.podOrder) != 0 {
		t.Fatalf("pods = %d, want none", len(kube.podOrder))
	}
}

// A first attempt runs its session, whose time reportTime checked: a
// class of 60 minutes leaves it 10 once the dry run's 50 are kept, and the
// policy calls that enough. A retry with too little time left goes to the
// dry run with a filing that stands, and with none fails, naming the
// times; a retry with time enough runs its session.
func TestProverSessionDueOnAFirstAttemptAndARetry(t *testing.T) {
	ctx := context.Background()
	r, _, store := newProverRunner(t, proverTask(), newFakeKube(), &fakeObjectStore{})
	seedProver(t, store, proverPlan(), keyLog())
	hour := proverPolicy()
	hour.Document.Classes[policy.ClassProver].Time.AgentTime = time.Hour
	if why := proverUnfit(hour, "sf/r"); why != "" {
		t.Fatalf("unfit at 60m = %q", why)
	}
	if got, err := reportTime(time.Hour, proverDryRun); err != nil || got != 10*time.Minute {
		t.Fatalf("reportTime(1h) = %s, %v", got, err)
	}
	// the activity's bound is the agent time and the grace
	bound := time.Now().Add(time.Hour + sessionGrace)
	if due, err := r.sessionDue(ctx, proverTask(), 1, bound); err != nil || !due {
		t.Fatalf("the first attempt at 60m = %v, %v; want its session", due, err)
	}
	_, err := r.sessionDue(ctx, proverTask(), 2, bound)
	if err == nil || !isTerminal(err) || !strings.Contains(errMessage(err), "once the dry run's 50m0s are kept, under the 10m0s a session needs, and no filing stands") {
		t.Fatalf("a retry with no filing = %v", err)
	}
	if due, err := r.sessionDue(ctx, proverTask(), 2, time.Now().Add(92*time.Minute)); err != nil || !due {
		t.Fatalf("a retry with 40 minutes left = %v, %v; want its session", due, err)
	}
	if err := store.FilePlanProofs(ctx, "t-plan", []PlanProof{{Round: 1, ID: "size_bound", Origin: ProofOriginCriterion, Criterion: "size_bound",
		Path: "proofs/size_bound/size_bound_test.go", Suite: "./proofs/size_bound", Name: "TestSizeBound", Code: "package size_bound"}}); err != nil {
		t.Fatal(err)
	}
	if due, err := r.sessionDue(ctx, proverTask(), 2, bound); err != nil || due {
		t.Fatalf("a retry with a filing = %v, %v; want the dry run alone", due, err)
	}
}

// A session's deadline keeps the reserve before the activity's: a retry
// 30 minutes into the prover's 92-minute run gets 10 minutes, not 40, so
// the dry run's 50 stand.
func TestReportDeadlineKeepsTheReserve(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		activity time.Time
		want     time.Duration
	}{{now.Add(92 * time.Minute), 40 * time.Minute}, {now.Add(62 * time.Minute), 10 * time.Minute}} {
		got := reportDeadline(tc.activity, 40*time.Minute, 50*time.Minute)
		if d := got.Sub(now) - tc.want; d < -time.Second || d > time.Second {
			t.Errorf("deadline for an activity ending in %s = %s from now, want %s", tc.activity.Sub(now), got.Sub(now), tc.want)
		}
	}
}

// seedProverCards files t-plan's card tasks for keys, the foundation's
// merge commit merged.
func seedProverCards(t *testing.T, store *fakeStore, merged string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		c := Task{ID: "t-" + k, Status: "queued", Repository: "sf/r", Kind: "agent", Source: SourcePlan, Parent: "t-plan", PlanKey: k, Track: policy.TrackDirect}
		if k == "foundation" {
			c.MergeCommit = merged
		}
		if err := store.CreateTask(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProverPassOpensTheProverOnceTheFoundationMerged(t *testing.T) {
	ctx := context.Background()
	open := planTask()
	open.Status = "done"
	r, _, store := newProverRunner(t, open, newFakeKube(), &fakeObjectStore{})
	seedProver(t, store, proverPlan())
	seedProverCards(t, store, "", "foundation", "sizes", "integration")
	wf := r.cfg.Workflows.(*fakeWorkflows)
	r.proverPass(ctx)
	if _, ok, _ := store.ReportOf(ctx, "t-plan", SourceProver); ok || len(wf.started) != 0 {
		t.Fatalf("a prover before the foundation merged; started %v", wf.started)
	}
	found := store.tasks["t-foundation"]
	found.MergeCommit = "abc"
	store.tasks["t-foundation"] = found
	r.proverPass(ctx)
	r.proverPass(ctx)
	got, ok, _ := store.ReportOf(ctx, "t-plan", SourceProver)
	if !ok || got.Parent != "t-plan" || !slices.Equal(got.RequiredCapabilities, []string{policy.ClassProver}) || got.Track != policy.TrackDirect ||
		got.SHA != "m0" || got.Status != "queued" || got.Source != SourceProver {
		t.Fatalf("prover = %+v, %v", got, ok)
	}
	if !slices.Equal(wf.started, []string{got.ID}) {
		t.Fatalf("started = %v, want the prover once", wf.started)
	}
}

func TestProverPassOpensAtConfirmationWithoutAFoundation(t *testing.T) {
	ctx := context.Background()
	r, _, store := newProverRunner(t, planTask(), newFakeKube(), &fakeObjectStore{})
	p := proverPlan()
	p.Cards = p.Cards[1:]
	seedProver(t, store, p)
	seedProverCards(t, store, "", "sizes", "integration")
	r.CriteriaPass(ctx)
	if got, ok, _ := store.ReportOf(ctx, "t-plan", SourceProver); !ok || got.Status != "queued" {
		t.Fatalf("prover = %+v, %v; want one queued on the first pass", got, ok)
	}
}

func TestProverPassLeavesAPlanItCannotServe(t *testing.T) {
	ctx := context.Background()
	r, _, store := newProverRunner(t, planTask(), newFakeKube(), &fakeObjectStore{})
	invariants := proverPlan()
	invariants.Criteria = invariants.Criteria[3:]
	seedProver(t, store, invariants)
	seedProverCards(t, store, "abc", "foundation", "sizes", "integration")
	r.proverPass(ctx)
	if _, ok, _ := store.ReportOf(ctx, "t-plan", SourceProver); ok {
		t.Fatal("a prover for a plan that owes no proof")
	}
	seedProver(t, store, proverPlan())
	r.cfg.Policy = testPolicyVersion
	r.proverPass(ctx)
	short := proverPolicy()
	short.Document.Classes[policy.ClassProver].Time.AgentTime = 55 * time.Minute
	r.cfg.Policy = func() policy.Version { return short }
	r.proverPass(ctx)
	if _, ok, _ := store.ReportOf(ctx, "t-plan", SourceProver); ok || len(r.cfg.Workflows.(*fakeWorkflows).started) != 0 {
		t.Fatal("a prover under a policy that cannot run one")
	}
	if why := proverUnfit(short, "sf/r"); !strings.Contains(why, "raise the class's agent_time to at least 1h0m0s") {
		t.Fatalf("unfit = %q", why)
	}
}

// Three refused filings are the limit too, and the refusal says none
// passed.
func TestFileProofsCapNamesNoFilingThatPassed(t *testing.T) {
	r, _, store := newProverRunner(t, proverTask(), newFakeKube(), &fakeObjectStore{})
	seedProver(t, store, proverPlan(), keyLog())
	for range 3 {
		if got, err := r.FileProofs(context.Background(), "t-prove", nil); err != nil || len(got.Findings) == 0 {
			t.Fatalf("a refused filing = %+v, %v", got, err)
		}
	}
	got, err := r.FileProofs(context.Background(), "t-prove", validFiling())
	if want := (ProofsFiled{Filing: 3, Findings: []string{"file_proofs: three filings are this session's limit, and none of them passed: end the session"}}); err != nil ||
		!reflect.DeepEqual(got, want) {
		t.Fatalf("the fourth = %+v, %v; want %+v", got, err, want)
	}
}

// A hold-out whose id a later criterion holds is its entry id in the
// prover's brief and filing: the criterion's test never stands for it.
func TestProofsDueNamesAHoldoutACriterionShadowsByItsEntry(t *testing.T) {
	r, _, store := newProverRunner(t, proverTask(), newFakeKube(), &fakeObjectStore{})
	p := Plan{Summary: "s", Criteria: []PlanCriterion{{ID: "key_log", Kind: CriterionTest, Claim: "A key turn is logged", Inputs: "one turn", Expected: "one row"}}}
	seedProver(t, store, p, keyLog())
	rows, _ := store.PlanProofs(context.Background(), "t-plan")
	if due := proofsDue(p, rows); !reflect.DeepEqual(due, []proofDue{{"key_log", CriterionTest, 0}, {"holdout-4", CriterionTest, 4}}) {
		t.Fatalf("due = %+v", due)
	}
	if g := proverGuide(p, rows, nil); !strings.Contains(g, "- holdout-4 (hold-out, kind test): Every key turn lands in the log.") {
		t.Fatalf("guide = %s", g)
	}
	if got, _ := r.FileProofs(context.Background(), "t-prove", []FiledProof{filedEntry("key_log", "TestKeyLog")}); !slices.Equal(got.Findings, []string{"file_proofs: no proof for holdout-4"}) {
		t.Fatalf("the criterion's test alone = %+v, want the hold-out missing", got)
	}
	hold := filedEntry("holdout-4", "TestHoldout4")
	hold.Path, hold.Suite = "proofs/holdout4/holdout4_test.go", "./proofs/holdout4"
	if got, _ := r.FileProofs(context.Background(), "t-prove", []FiledProof{filedEntry("key_log", "TestKeyLog"), hold}); len(got.Findings) != 0 {
		t.Fatalf("both = %+v", got)
	}
	rows, _ = store.PlanProofs(context.Background(), "t-plan")
	i := slices.IndexFunc(rows, func(pp PlanProof) bool { return pp.Idx == 4 })
	if i < 0 || rows[i].ID != "key_log" || rows[i].Name != "TestHoldout4" || rows[i].State != ProofFiled {
		t.Fatalf("rows = %+v, want the hold-out's own test on idx 4", rows)
	}
}

// A closed plan takes no filing and no store: the prover is told why, and
// the store leaves a line that says so, with no pod.
func TestProverStopsAtAClosedPlan(t *testing.T) {
	ctx := context.Background()
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	task := proverTask()
	task.AgentSummary = "filed 1 proof, filing 1"
	r, _, store := newProverRunner(t, task, kube, objects)
	seedProver(t, store, Plan{Summary: "s", Criteria: proverPlan().Criteria[:1]}, PlanProof{Idx: 1, Round: 1, ID: "size_bound", Origin: ProofOriginCriterion,
		Criterion: "size_bound", State: ProofFiled, Path: "proofs/size_bound/size_bound_test.go", Suite: "./proofs/size_bound", Name: "TestSizeBound", Code: "package size_bound"})
	for name, close := range map[string]func(){
		"plan_closed":      func() { store.closed["t-plan"] = time.Now() },
		"a person's close": func() { plan := store.tasks["t-plan"]; plan.Status = "closed"; store.tasks["t-plan"] = plan },
	} {
		delete(store.closed, "t-plan")
		plan := store.tasks["t-plan"]
		plan.Status = "done"
		store.tasks["t-plan"] = plan
		close()
		if got, err := r.FileProofs(ctx, "t-prove", []FiledProof{filedEntry("size_bound", "TestSizeBound")}); err != nil ||
			!slices.Equal(got.Findings, []string{"file_proofs: plan t-plan is closed, so no proof of it is filed: end the session"}) {
			t.Fatalf("%s: the filing = %+v, %v", name, got, err)
		}
		if _, err := execActivity(t, r, ActivityRunReport, "t-prove", "factory/small"); err != nil {
			t.Fatalf("%s: RunReport: %v", name, err)
		}
		if line := timelineStep(t, objects, "t-prove", "proofs_not_stored"); line["reason"] != "plan t-plan is closed" {
			t.Fatalf("%s: line = %v", name, line)
		}
	}
	if len(kube.podOrder) != 0 || len(planEvents(store, EventProofsFiled)) != 0 || len(planEvents(store, EventProofsStored)) != 0 {
		t.Fatalf("pods %d, events %+v; want none", len(kube.podOrder), store.events)
	}
	if rows, _ := store.PlanProofs(ctx, "t-plan"); rows[0].State != ProofFiled {
		t.Fatalf("rows = %+v, want the filing as it was", rows)
	}
}

// A prover whose plan closed before its session runs none: it ends in
// factoryd's words with no pod and a line that says why. A retry of a
// prover that stored its proofs before the plan closed writes no such
// line: the store ran.
func TestProverRunsNoSessionForAClosedPlan(t *testing.T) {
	ctx := context.Background()
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, _, store := newProverRunner(t, proverTask(), kube, objects)
	seedProver(t, store, proverPlan(), keyLog())
	store.closed["t-plan"] = time.Now()
	if _, err := execActivity(t, r, ActivityRunReport, "t-prove", "factory/small"); err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	if got, _, _ := store.Task(ctx, "t-prove"); got.AgentSummary != "plan t-plan is closed: no proof of it is stored" || len(kube.podOrder) != 0 {
		t.Fatalf("AgentSummary %q, pods %d; want factoryd's words and no pod", got.AgentSummary, len(kube.podOrder))
	}
	if line := timelineStep(t, objects, "t-prove", "proofs_not_stored"); line["reason"] != "plan t-plan is closed" {
		t.Fatalf("line = %v", line)
	}

	task := proverTask()
	task.ID, task.Parent, task.AgentSummary = "t-prove-s", "t-plan-s", "filed 1 proof, filing 1"
	if err := store.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	plan := planTask()
	plan.ID = "t-plan-s"
	if err := store.CreateTask(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPlanRevision(ctx, "t-plan-s", mustJSON(proverPlan()), "sha-s"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutPlanProofs(ctx, "t-plan-s", 1, []PlanProof{{Idx: 1, ID: "size_bound", Origin: ProofOriginCriterion, Criterion: "size_bound",
		State: ProofStored, Path: "proofs/size_bound/size_bound_test.go", Suite: "./proofs/size_bound", Name: "TestSizeBound", Code: "package size_bound"}}); err != nil {
		t.Fatal(err)
	}
	store.closed["t-plan-s"] = time.Now()
	if _, err := execActivity(t, r, ActivityRunReport, "t-prove-s", "factory/small"); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if b, _ := objects.Get(ctx, "sessions/t-prove-s/timeline.jsonl"); strings.Contains(string(b), "proofs_not_stored") {
		t.Fatalf("a stored plan's retry wrote that nothing was stored: %s", b)
	}
}

// The prover is a report task no work slot bounds: it starts through the
// work bound with every slot of its project taken, and it files no issue.
func TestProverPassStartsTheProverPastTheWorkBound(t *testing.T) {
	ctx := context.Background()
	r, _, store := newProverRunner(t, planTask(), newFakeKube(), &fakeObjectStore{})
	p := proverPlan()
	p.Cards = p.Cards[1:]
	seedProver(t, store, p)
	seedProverCards(t, store, "", "sizes", "integration")
	if err := store.CreateTask(ctx, Task{ID: "t-busy", Status: "running", Repository: "sf/r", Kind: "agent", Source: "webhook", Track: policy.TrackDirect,
		IssueNumber: 9}); err != nil {
		t.Fatal(err)
	}
	rec := &recordingWorkflows{}
	r.cfg.Workflows = BoundedWorkflows{Workflows: rec, Store: store, Policy: func() policy.Tree {
		return policy.Tree{Projects: map[string]policy.ProjectConfig{"r": {Repository: "sf/r", WorkInProgress: 1}}}
	}}
	r.proverPass(ctx)
	got, ok, _ := store.ReportOf(ctx, "t-plan", SourceProver)
	if !ok || got.IssueNumber != 0 || !slices.Equal(rec.starts, []string{got.ID}) {
		t.Fatalf("prover %+v (%v), starts %v; want it started with no issue", got, ok, rec.starts)
	}
	if n, _ := store.CountAdmitted(ctx, "sf/r"); n != 1 {
		t.Fatalf("admitted = %d, want the running card's slot alone", n)
	}
}
