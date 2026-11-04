package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// readsCalm is a judgement criterion whose proof writes the sizes page;
// pagesOK a human one whose proof writes the pages' rows.
var (
	readsCalm = PlanCriterion{ID: "reads_calm", Kind: CriterionJudgement, Claim: "The sizes page reads calm", Inputs: "the sizes page with three sizes",
		Prompt: "Does the page read calm? Answer met or unmet."}
	pagesOK = PlanCriterion{ID: "pages_ok", Kind: CriterionHuman, Claim: "The pages read well", Evidence: "Open /sizes and /doors at 390 px"}
)

// artifactRows are the stored proofs of readsCalm (idx 6) and pagesOK
// (idx 7).
func artifactRows() []PlanProof {
	return []PlanProof{
		{Idx: 6, Origin: ProofOriginCriterion, Criterion: "reads_calm", ID: "reads_calm", State: ProofStored, Suite: "proofs/reads_calm", Name: "TestReadsCalm",
			Path: "proofs/reads_calm/reads_calm_test.go", Code: "package reads_calm", Artifact: "reads_calm.html"},
		{Idx: 7, Origin: ProofOriginCriterion, Criterion: "pages_ok", ID: "pages_ok", State: ProofStored, Suite: "proofs/pages_ok", Name: "TestPagesOK",
			Path: "proofs/pages_ok/pages_ok_test.go", Code: "package pages_ok", Artifact: "pages_ok.txt"},
	}
}

const sizesPage = "<main><h1>Sizes</h1></main>"

// judgementLines are t1's judgement lines, in order.
func judgementLines(t *testing.T, objects *fakeObjectStore) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(timelineText(t, objects)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["step"] == "judgement" {
			delete(m, "step")
			delete(m, "time")
			out = append(out, m)
		}
	}
	return out
}

// calmPage is readsCalm's artifact at head.
func calmPage(head string) GateArtifact {
	return GateArtifact{Criterion: "reads_calm", Name: "reads_calm.html", Head: head, Content: sizesPage}
}

// withSupervisorTimeout is the test policy with the supervisor timeout d.
func withSupervisorTimeout(d time.Duration) func() policy.Version {
	return func() policy.Version {
		v := testPolicyVersion()
		v.Document.Limits.SupervisorTimeout = d
		return v
	}
}

// The judgement is one call of the review model: the criterion's pinned
// prompt with the answer's form as the system prompt, the artifact's name
// and content alone as the input, no tool. Its row names the model and the
// prompt's hash; its line adds the verdict, the reason and the task's
// spend booked since the call began.
func TestJudgeCallsTheModelWithThePromptAndTheArtifactAlone(t *testing.T) {
	objects := &fakeObjectStore{}
	r, _, store := newTestRunner(t, gateTask, newFakeKube(), objects)
	store.spendSince = 0.04
	ll := &fakeLiteLLM{content: "```json\n{\"verdict\":\"met\",\"reason\":\"the page names every size\"}\n```"}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	before := time.Now()
	findings, met, err := r.judgeArtifacts(context.Background(), gateTask, "t1", Plan{Criteria: []PlanCriterion{readsCalm}}, []GateArtifact{calmPage("c1")})
	if err != nil || len(findings) != 0 || met != 1 {
		t.Fatalf("judgeArtifacts = %q, %d, %v; want one met and no finding", findings, met, err)
	}
	if len(ll.chats) != 1 || !slices.Equal(ll.purposes, []string{policy.PurposeReview}) || ll.chats[0]["model"] != "big" || ll.chats[0]["tools"] != nil {
		t.Fatalf("chats %v, purposes %v: want one review call on big with no tools", ll.chats, ll.purposes)
	}
	if got, want := systemMessage(ll.chats[0]), readsCalm.Prompt+"\n\n"+judgementAnswer; got != want {
		t.Fatalf("system prompt =\n%s\nwant\n%s", got, want)
	}
	var in map[string]string
	if err := json.Unmarshal([]byte(userMessage(ll.chats[0])), &in); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"artifact": "reads_calm.html", "content": sizesPage}; !maps.Equal(in, want) {
		t.Fatalf("input = %v, want %v", in, want)
	}
	if got, want := rowsOf(store, "reads_calm"), []CriterionVerdict{{Criterion: "reads_calm", Verdict: "met", Evidence: "the page names every size (model big, prompt 6b2355d78a19)",
		Kind: CriterionJudgement, Source: CriteriaSourceJudgement, DecidedBy: FactoryDecider}}; !slices.Equal(got, want) {
		t.Fatalf("rows = %+v, want %+v", got, want)
	}
	want := []map[string]any{{"criterion": "reads_calm", "artifact": "reads_calm.html", "model": "big", "prompt_hash": "6b2355d78a19",
		"verdict": "met", "reason": "the page names every size", "cost": 0.04}}
	if got := judgementLines(t, objects); !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %v, want %v", got, want)
	}
	if store.spendFrom.Before(before) || store.spendFrom.After(time.Now()) {
		t.Fatalf("the spend is read from %v, want the call's start", store.spendFrom)
	}
	if got := promptHash(readsCalm.Prompt); got != "6b2355d78a19" {
		t.Fatalf("promptHash = %q", got)
	}
}

// An answer that is not the verdict, a verdict that is neither met nor
// unmet, a gateway that refuses and a call cut at the supervisor timeout
// give no verdict: the row is unmet and says why, so no earlier head's
// row stands, and a finding names the criterion and the reason.
func TestJudgeWithoutAVerdictIsUnmet(t *testing.T) {
	for name, tc := range map[string]struct {
		ll     *fakeLiteLLM
		reason string
		prefix bool
	}{
		"prose":      {&fakeLiteLLM{content: "it reads calm"}, "the answer is not the verdict json: ", true},
		"partly":     {&fakeLiteLLM{content: `{"verdict": "partly"}`}, `the verdict "partly" is neither met nor unmet`, false},
		"no gateway": {&fakeLiteLLM{status: http.StatusInternalServerError}, `status 500: {"error":{"message":"task headroom 0.000000 EUR","type":"budget_exceeded"}}`, false},
		"timed out":  {&fakeLiteLLM{content: `{"verdict":"met","reason":"late"}`, delay: 5 * time.Second}, "timed out", false},
	} {
		t.Run(name, func(t *testing.T) {
			objects := &fakeObjectStore{}
			r, _, store := newTestRunner(t, gateTask, newFakeKube(), objects)
			r.cfg.Policy = withSupervisorTimeout(300 * time.Millisecond)
			r.supervisor = testSupervisorModel(tc.ll.srv(t).URL)
			findings, met, err := r.judgeArtifacts(context.Background(), gateTask, "t1", Plan{Criteria: []PlanCriterion{readsCalm}}, []GateArtifact{calmPage("c1")})
			if err != nil || met != 0 {
				t.Fatalf("judgeArtifacts = %q, %d, %v", findings, met, err)
			}
			lines := judgementLines(t, objects)
			if len(lines) != 1 || lines[0]["verdict"] != "" {
				t.Fatalf("lines = %v, want one with no verdict", lines)
			}
			reason, _ := lines[0]["reason"].(string)
			if (tc.prefix && !strings.HasPrefix(reason, tc.reason)) || (!tc.prefix && reason != tc.reason) {
				t.Fatalf("reason = %q, want %q", reason, tc.reason)
			}
			if got, want := rowsOf(store, "reads_calm"), []CriterionVerdict{{Criterion: "reads_calm", Verdict: "unmet", Evidence: "no verdict: " + reason,
				Kind: CriterionJudgement, Source: CriteriaSourceJudgement, DecidedBy: FactoryDecider}}; !slices.Equal(got, want) {
				t.Fatalf("rows = %+v, want %+v", got, want)
			}
			if want := "criterion reads_calm: the judgement over reads_calm.html at c1 gave no verdict: " + reason; !slices.Equal(findings, []string{want}) {
				t.Fatalf("findings = %q, want %q", findings, want)
			}
		})
	}
}

// A call that gives no verdict at a later head leaves no earlier head's
// met standing: the criterion is unmet, and says why.
func TestJudgementWithoutAVerdictReplacesAnEarlierHeadsMet(t *testing.T) {
	ctx := context.Background()
	r, _, store := newTestRunner(t, gateTask, newFakeKube(), &fakeObjectStore{})
	p := Plan{Criteria: []PlanCriterion{readsCalm}}
	r.supervisor = testSupervisorModel((&fakeLiteLLM{content: `{"verdict":"met","reason":"the page names every size"}`}).srv(t).URL)
	if _, met, err := r.judgeArtifacts(ctx, gateTask, "t1", p, []GateArtifact{calmPage("c1")}); err != nil || met != 1 {
		t.Fatalf("at c1: met %d, %v", met, err)
	}
	r.supervisor = testSupervisorModel((&fakeLiteLLM{status: http.StatusInternalServerError}).srv(t).URL)
	findings, _, err := r.judgeArtifacts(ctx, gateTask, "t1", p, []GateArtifact{calmPage("c2")})
	if err != nil {
		t.Fatal(err)
	}
	const reason = `status 500: {"error":{"message":"task headroom 0.000000 EUR","type":"budget_exceeded"}}`
	rows, _ := store.CriteriaVerdicts(ctx, "t1")
	if v, ok := StandingRow(rows, readsCalm); !ok || v.Verdict != "unmet" || v.Evidence != "no verdict: "+reason {
		t.Fatalf("standing row = %+v, %v; want unmet, no verdict: %s", v, ok, reason)
	}
	if want := "criterion reads_calm: the judgement over reads_calm.html at c2 gave no verdict: " + reason; !slices.Equal(findings, []string{want}) {
		t.Fatalf("findings = %q, want %q", findings, want)
	}
}

// The judgement criteria's calls run at once, each cut at the supervisor
// timeout: a call the cut ends gives no verdict, timed out.
func TestJudgeArtifactsCallsAtOnce(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	bArrived := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()
		defer func() {
			mu.Lock()
			inFlight--
			mu.Unlock()
		}()
		if strings.Contains(string(body), "b.html") {
			once.Do(func() { close(bArrived) })
			<-req.Context().Done()
			return
		}
		select {
		case <-bArrived:
		case <-req.Context().Done():
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"{\"verdict\":\"met\",\"reason\":\"calm\"}"}}]}`)
	}))
	t.Cleanup(srv.Close)
	r, _, store := newTestRunner(t, gateTask, newFakeKube(), &fakeObjectStore{})
	r.cfg.Policy = withSupervisorTimeout(time.Second)
	r.supervisor = testSupervisorModel(srv.URL)
	a, b := readsCalm, readsCalm
	a.ID, b.ID = "a", "b"
	findings, met, err := r.judgeArtifacts(context.Background(), gateTask, "t1", Plan{Criteria: []PlanCriterion{a, b}}, []GateArtifact{
		{Criterion: "a", Name: "a.html", Head: "c1", Content: "<p>a</p>"}, {Criterion: "b", Name: "b.html", Head: "c1", Content: "<p>b</p>"}})
	if err != nil || met != 1 {
		t.Fatalf("judgeArtifacts = %q, %d, %v; want a met", findings, met, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak != 2 {
		t.Fatalf("at most %d calls at once, want 2", peak)
	}
	if got := rowsOf(store, "b"); len(got) != 1 || got[0].Verdict != "unmet" || got[0].Evidence != "no verdict: timed out" {
		t.Fatalf("b's rows = %+v, want unmet, no verdict: timed out", got)
	}
	if want := "criterion b: the judgement over b.html at c1 gave no verdict: timed out"; !slices.Equal(findings, []string{want}) {
		t.Fatalf("findings = %q, want %q", findings, want)
	}
}

// A ledger read that fails loses the cost, never the verdict: the line
// says the cost is unknown, and the row stands.
func TestJudgeArtifactsKeepsTheVerdictWithoutItsCost(t *testing.T) {
	objects := &fakeObjectStore{}
	r, _, store := newTestRunner(t, gateTask, newFakeKube(), objects)
	store.spendErr = errors.New("the ledger is down")
	r.supervisor = testSupervisorModel((&fakeLiteLLM{content: `{"verdict":"met","reason":"the page names every size"}`}).srv(t).URL)
	if _, met, err := r.judgeArtifacts(context.Background(), gateTask, "t1", Plan{Criteria: []PlanCriterion{readsCalm}}, []GateArtifact{calmPage("c1")}); err != nil || met != 1 {
		t.Fatalf("met %d, %v; want the verdict", met, err)
	}
	if lines := judgementLines(t, objects); len(lines) != 1 || lines[0]["cost"] != nil || lines[0]["verdict"] != "met" {
		t.Fatalf("lines = %v, want the met line with no cost", lines)
	}
	if got := rowsOf(store, "reads_calm"); len(got) != 1 || got[0].Verdict != "met" {
		t.Fatalf("rows = %+v, want the met row", got)
	}
}

// judgedGate runs the gate on blindPlan with readsCalm and pagesOK, their
// proofs stored, the judgement answering answer; reads_calm's run is run.
func judgedGate(t *testing.T, answer string, readsCalmRun harness.ProofRun) (CriteriaResult, *fakeStore, *fakeObjectStore, *forgejoHub, *fakeWorkerEndpoint, *fakeLiteLLM) {
	t.Helper()
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, store := newTestRunner(t, seedCriteriaPlan(t), kube, objects)
	hub.files = map[string]string{"c1:acceptance/group_open_test.go": "package acceptance // group_open v2"}
	worker := proofsWorker(t, r)
	ll := &fakeLiteLLM{content: answer}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	p := blindPlan()
	p.Criteria = append(p.Criteria, readsCalm, pagesOK)
	seedBlindPlan(t, store, p, append(blindRows(), artifactRows()...))
	results, errs := runCriteriaWithPods(t, r, kube, proofLines(
		harness.ProofRun{ID: "size_bound", Started: true, Exit: 1, Output: "=== RUN   TestSizeBound\n--- FAIL: TestSizeBound (0.00s)\n", Report: junitGz(t, sizeBoundFail)},
		harness.ProofRun{ID: "group_open", Started: true, Report: junitGz(t, wrap(`<testcase name="TestGroupOpen"></testcase>`))},
		harness.ProofRun{ID: "all_green", Started: true, Report: junitGz(t, wrap(`<testcase name="TestA"></testcase>`))},
		readsCalmRun,
		harness.ProofRun{ID: "pages_ok", Started: true, Output: "pages_ok run output", Report: junitGz(t, wrap(`<testcase name="TestPagesOK"></testcase>`)),
			Artifacts: map[string][]byte{"pages_ok.txt": []byte("sizes: 3 rows\ndoors: 2 rows")}},
		harness.ProofRun{ID: "holdout-4", Started: true, Exit: 1, Report: junitGz(t, wrap(`<testcase name="TestKeyLog"><failure message="Failed">--- FAIL: TestKeyLog (0.00s)</failure></testcase>`))},
		harness.ProofRun{ID: "holdout-5", Started: true, Report: junitGz(t, wrap(`<testcase name="TestLockLog"></testcase>`))},
	))
	if errs[0] != nil {
		t.Fatal(errs[0])
	}
	return results[0], store, objects, hub, worker, ll
}

// readsCalmWrote is reads_calm's green run that wrote the sizes page.
func readsCalmWrote(t *testing.T) harness.ProofRun {
	return harness.ProofRun{ID: "reads_calm", Started: true, Output: "reads_calm run output", Report: junitGz(t, wrap(`<testcase name="TestReadsCalm"></testcase>`)),
		Artifacts: map[string][]byte{"reads_calm.html": []byte(sizesPage)}}
}

// lineProof is the gate_criteria line's entry of id.
func lineProof(t *testing.T, objects *fakeObjectStore, id string) gateProof {
	t.Helper()
	var proofs []gateProof
	if err := json.Unmarshal(mustJSON(timelineStep(t, objects, "t1", "gate_criteria")["proofs"]), &proofs); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(proofs, func(g gateProof) bool { return g.ID == id })
	if i < 0 {
		t.Fatalf("the line has no entry %s: %+v", id, proofs)
	}
	return proofs[i]
}

// rowsOf are the verdict rows of criterion id on t1, every source's.
func rowsOf(store *fakeStore, id string) []CriterionVerdict {
	rows, _ := store.CriteriaVerdicts(context.Background(), "t1")
	return slices.DeleteFunc(rows, func(v CriterionVerdict) bool { return v.Criterion != id })
}

// A judgement and a human criterion's proofs run blind beside the
// others: each artifact is stored for the console, the judgement one
// call judges, and its row is the judgement's. Both proofs stay stored,
// their code and output on no surface; the human criterion gets no row.
// An unmet judgement is a finding with its reason.
func TestGateCriteriaJudgesTheArtifacts(t *testing.T) {
	res, store, objects, hub, worker, ll := judgedGate(t, `{"verdict":"met","reason":"the page names every size"}`, readsCalmWrote(t))
	file := func(path, code string) *harness.ProofFile { return &harness.ProofFile{Path: path, Content: code} }
	wantList := []harness.Proof{
		{ID: "size_bound", Suite: "./proofs/size_bound", Name: "TestSizeBound", File: file("proofs/size_bound/size_bound_test.go", sizeBoundCode)},
		{ID: "group_open", Suite: "./acceptance", Name: "TestGroupOpen"},
		{ID: "all_green", Suite: "./..."},
		{ID: "reads_calm", Suite: "./proofs/reads_calm", Name: "TestReadsCalm", File: file("proofs/reads_calm/reads_calm_test.go", "package reads_calm")},
		{ID: "pages_ok", Suite: "./proofs/pages_ok", Name: "TestPagesOK", File: file("proofs/pages_ok/pages_ok_test.go", "package pages_ok")},
		{ID: "holdout-4", Suite: "./proofs/key_log", Name: "TestKeyLog", File: file("proofs/key_log/key_log_test.go", "package key_log")},
		{ID: "holdout-5", Suite: "./proofs/lock_log", Name: "TestLockLog", File: file("proofs/lock_log/lock_log_test.go", "package lock_log")},
	}
	var list []harness.Proof
	if len(worker.lists) != 1 || json.Unmarshal([]byte(worker.lists[0]), &list) != nil || !reflect.DeepEqual(list, wantList) {
		t.Fatalf("posted lists = %q, want %+v", worker.lists, wantList)
	}
	for id, want := range map[string]GateArtifact{
		"reads_calm": {Criterion: "reads_calm", Name: "reads_calm.html", Head: "c1", Content: sizesPage},
		"pages_ok":   {Criterion: "pages_ok", Name: "pages_ok.txt", Head: "c1", Content: "sizes: 3 rows\ndoors: 2 rows"},
	} {
		var got GateArtifact
		b, _ := objects.Get(context.Background(), ArtifactKey("t1", id))
		if err := json.Unmarshal(b, &got); err != nil || got != want {
			t.Fatalf("object %s = %s, want %+v", ArtifactKey("t1", id), b, want)
		}
	}
	if len(ll.chats) != 1 || !strings.Contains(userMessage(ll.chats[0]), "reads_calm.html") {
		t.Fatalf("chats = %v, want the one judgement of reads_calm", ll.chats)
	}
	if got, want := rowsOf(store, "reads_calm"), []CriterionVerdict{{Criterion: "reads_calm", Verdict: "met", Evidence: "the page names every size (model big, prompt 6b2355d78a19)",
		Kind: CriterionJudgement, Source: CriteriaSourceJudgement, DecidedBy: FactoryDecider}}; !slices.Equal(got, want) {
		t.Fatalf("reads_calm rows = %+v, want %+v", got, want)
	}
	if got := rowsOf(store, "pages_ok"); len(got) != 0 {
		t.Fatalf("pages_ok rows = %+v, want none", got)
	}
	if got, want := lineProof(t, objects, "reads_calm"), (gateProof{ID: "reads_calm", Kind: CriterionJudgement, Suite: "./proofs/reads_calm", Name: "TestReadsCalm",
		Outcome: proofPassed, Line: "criterion reads_calm: TestReadsCalm in ./proofs/reads_calm wrote reads_calm.html", State: proofBlind,
		Path: "proofs/reads_calm/reads_calm_test.go", Artifact: "reads_calm.html"}); got != want {
		t.Fatalf("the line's reads_calm =\n%+v\nwant\n%+v", got, want)
	}
	if got, want := lineProof(t, objects, "pages_ok"), (gateProof{ID: "pages_ok", Kind: CriterionHuman, Suite: "./proofs/pages_ok", Name: "TestPagesOK",
		Outcome: proofPassed, Line: "criterion pages_ok: TestPagesOK in ./proofs/pages_ok wrote pages_ok.txt", State: proofBlind,
		Path: "proofs/pages_ok/pages_ok_test.go", Artifact: "pages_ok.txt"}); got != want {
		t.Fatalf("the line's pages_ok =\n%+v\nwant\n%+v", got, want)
	}
	if step := timelineStep(t, objects, "t1", "judgement"); step["criterion"] != "reads_calm" || step["verdict"] != "met" || step["prompt_hash"] != "6b2355d78a19" {
		t.Fatalf("the judgement line = %v", step)
	}
	after, _ := store.PlanProofs(context.Background(), "plan1")
	for _, pp := range after {
		if pp.Idx >= 6 && pp.State != ProofStored {
			t.Fatalf("row %d is %s, want stored", pp.Idx, pp.State)
		}
	}
	const head = "factoryd criteria gate on c1: 3 of 7 criteria met (human criteria ride the approval)."
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], head) {
		t.Fatalf("comments = %q, want one holding %q", hub.comments, head)
	}
	for _, f := range res.Findings {
		if strings.Contains(f, "judgement over") {
			t.Fatalf("a met judgement is a finding: %q", f)
		}
	}
	surfaces := map[string]string{"the comment": hub.comments[0], "the line": timelineText(t, objects), "the findings": strings.Join(res.Findings, "\n"),
		"the events": string(mustJSON(store.events)), "the rows": string(mustJSON(rowsOf(store, "reads_calm")))}
	for _, leak := range []string{"package reads_calm", "reads_calm run output", "package pages_ok", "pages_ok run output"} {
		for surface, text := range surfaces {
			if strings.Contains(text, leak) {
				t.Fatalf("%s carries %q: %s", surface, leak, text)
			}
		}
	}

	res, store, _, _, _, _ = judgedGate(t, `{"verdict":"unmet","reason":"the page shows two sizes"}`, readsCalmWrote(t))
	if got := rowsOf(store, "reads_calm"); len(got) != 1 || got[0].Verdict != "unmet" || got[0].Source != CriteriaSourceJudgement {
		t.Fatalf("reads_calm rows = %+v, want the judgement's unmet", got)
	}
	if want := "criterion reads_calm: the judgement over reads_calm.html at c1 is unmet: the page shows two sizes"; !slices.Contains(res.Findings, want) {
		t.Fatalf("findings = %q, want %q", res.Findings, want)
	}
}

// timelineText is t1's whole timeline.
func timelineText(t *testing.T, objects *fakeObjectStore) string {
	t.Helper()
	b, _ := objects.Get(context.Background(), "sessions/t1/timeline.jsonl")
	return string(b)
}

// A blind proof that wrote no artifact has no verdict: it opens, as a red
// test does, and its finding hands the builder the code and the output.
// An artifact that is not text is no verdict either, and the proof stays
// stored. Neither makes a call, and each is an unmet row that says why,
// so no earlier head's row stands.
func TestGateCriteriaOpensAProofThatWroteNoArtifact(t *testing.T) {
	ctx := context.Background()
	res, store, objects, _, _, ll := judgedGate(t, "", harness.ProofRun{ID: "reads_calm", Started: true, Exit: 1, Output: "panic: no page\n",
		Report: junitGz(t, wrap(`<testcase name="TestReadsCalm"><failure message="Failed">panic: no page</failure></testcase>`))})
	if len(ll.chats) != 0 {
		t.Fatalf("chats = %v, want none", ll.chats)
	}
	noVerdict := func(evidence string) []CriterionVerdict {
		return []CriterionVerdict{{Criterion: "reads_calm", Verdict: "unmet", Evidence: "no verdict: " + evidence, Kind: CriterionJudgement,
			Source: CriteriaSourceJudgement, DecidedBy: FactoryDecider}}
	}
	if got, want := rowsOf(store, "reads_calm"), noVerdict("its blind proof wrote no artifact reads_calm.html at c1"); !slices.Equal(got, want) {
		t.Fatalf("reads_calm rows = %+v, want %+v", got, want)
	}
	after, _ := store.PlanProofs(ctx, "plan1")
	if i := slices.IndexFunc(after, func(pp PlanProof) bool { return pp.Idx == 6 }); after[i].State != ProofOpened {
		t.Fatalf("row 6 is %s, want opened", after[i].State)
	}
	events := gateEvents(store, "t1")
	i := slices.IndexFunc(events, func(e map[string]any) bool { return e["criterion"] == "reads_calm" })
	if i < 0 || events[i]["outcome"] != proofNoArtifact || events[i]["message"] != "Failed\npanic: no page" {
		t.Fatalf("proof_opened events = %v, want reads_calm's with outcome %q", events, proofNoArtifact)
	}
	if got := string(mustJSON(timelineStep(t, objects, "t1", "gate_criteria")["opened"])); got != `["size_bound","door","reads_calm","key_log"]` {
		t.Fatalf("the line's opened = %s", got)
	}
	k := slices.IndexFunc(res.Findings, func(f string) bool { return strings.HasPrefix(f, "criterion reads_calm: ") })
	if k < 0 {
		t.Fatalf("findings = %q, want reads_calm's", res.Findings)
	}
	f := res.Findings[k]
	const opening = "criterion reads_calm: its blind proof wrote no artifact reads_calm.html at c1. It is open now. Commit the test below at proofs/reads_calm/reads_calm_test.go"
	if !strings.HasPrefix(f, opening) {
		t.Fatalf("finding = %q, want it to start %q", f, opening)
	}
	for _, want := range []string{"The criterion: The sizes page reads calm. The artifact shows: the sizes page with three sizes.", "package reads_calm", "Failed\npanic: no page"} {
		if !strings.Contains(f, want) {
			t.Fatalf("finding = %q, want it to hold %q", f, want)
		}
	}

	res, store, objects, _, _, ll = judgedGate(t, "", harness.ProofRun{ID: "reads_calm", Started: true, Report: junitGz(t, wrap(`<testcase name="TestReadsCalm"></testcase>`)),
		Artifacts: map[string][]byte{"reads_calm.html": []byte("<main>\x00</main>")}})
	if got, want := rowsOf(store, "reads_calm"), noVerdict("its artifact reads_calm.html is not text"); len(ll.chats) != 0 || !slices.Equal(got, want) {
		t.Fatalf("chats %v, rows %+v; want none and %+v", ll.chats, got, want)
	}
	if want := "criterion reads_calm: its artifact reads_calm.html is not text: no verdict"; !slices.Contains(res.Findings, want) {
		t.Fatalf("findings = %q, want %q", res.Findings, want)
	}
	if b, _ := objects.Get(ctx, ArtifactKey("t1", "reads_calm")); b != nil {
		t.Fatalf("the artifact that is not text is stored: %q", b)
	}
	after, _ = store.PlanProofs(ctx, "plan1")
	if i := slices.IndexFunc(after, func(pp PlanProof) bool { return pp.Idx == 6 }); after[i].State != ProofStored {
		t.Fatalf("row 6 is %s, want stored", after[i].State)
	}
	if strings.Contains(strings.Join(res.Findings, "\n"), "package reads_calm") {
		t.Fatalf("the findings carry the stored proof's code: %q", res.Findings)
	}
}

// An artifact the run's line had no room for is no verdict: a finding
// names it, the row is unmet and says why, and the proof stays stored,
// since the test ran and wrote it.
func TestGateCriteriaFindsAnArtifactTooLargeForTheLine(t *testing.T) {
	res, store, objects, _, _, ll := judgedGate(t, "", harness.ProofRun{ID: "reads_calm", Started: true, Report: junitGz(t, wrap(`<testcase name="TestReadsCalm"></testcase>`)),
		Dropped: []string{"reads_calm.html"}})
	const why = "artifact reads_calm.html was too large for the run's line"
	if !slices.Contains(res.Findings, "criterion reads_calm: "+why) || len(ll.chats) != 0 {
		t.Fatalf("findings %q, chats %v; want the finding and no call", res.Findings, ll.chats)
	}
	if got := rowsOf(store, "reads_calm"); len(got) != 1 || got[0].Verdict != "unmet" || got[0].Evidence != "no verdict: "+why || got[0].Source != CriteriaSourceJudgement {
		t.Fatalf("rows = %+v, want the unmet judgement row", got)
	}
	after, _ := store.PlanProofs(context.Background(), "plan1")
	if i := slices.IndexFunc(after, func(pp PlanProof) bool { return pp.Idx == 6 }); after[i].State != ProofStored {
		t.Fatalf("row 6 is %s, want stored", after[i].State)
	}
	if got := string(mustJSON(timelineStep(t, objects, "t1", "gate_criteria")["opened"])); strings.Contains(got, "reads_calm") {
		t.Fatalf("the line's opened = %s, want no reads_calm", got)
	}
}
