package factoryd

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// proofsAtMainPlan is the plan the proofs at main read: the feature card
// sizes cites all_green, size_bound and group_open (size_bound twice),
// the card doors cites door, the foundation card base nothing and the
// integration card size_bound.
func proofsAtMainPlan() Plan {
	test := func(id string) PlanCriterion {
		return PlanCriterion{ID: id, Kind: CriterionTest, Claim: "claim of " + id, Inputs: "inputs of " + id, Expected: "expected of " + id}
	}
	item := func(id string) PlanItem { return PlanItem{Criterion: id, Text: "the item of " + id} }
	return Plan{Summary: "s",
		Criteria: []PlanCriterion{test("size_bound"), test("group_open"), test("door"), {ID: "all_green", Kind: CriterionInvariant, Suite: "./..."}},
		Cards: []PlanCard{
			{Key: "base", Role: RoleFoundation, Acceptance: []PlanItem{{Text: "the contracts compile"}}},
			{Key: "sizes", Role: RoleFeature, Acceptance: []PlanItem{item("all_green"), item("size_bound"), item("group_open"), item("size_bound")}},
			{Key: "doors", Role: RoleFeature, Acceptance: []PlanItem{item("door")}},
			{Key: "integ", Role: RoleIntegration, Acceptance: []PlanItem{item("size_bound")}},
		}}
}

// proofsAtMainRows are its rows: size_bound stored, door stored, no row
// for group_open, and the hold-out key_log stored.
func proofsAtMainRows() []PlanProof {
	return []PlanProof{
		{Idx: 1, Origin: ProofOriginCriterion, Criterion: "size_bound", ID: "size_bound", State: ProofStored, Suite: "proofs/size_bound", Name: "TestSizeBound",
			Path: "proofs/size_bound/size_bound_test.go", Code: sizeBoundCode},
		{Idx: 2, Origin: ProofOriginCriterion, Criterion: "door", ID: "door", State: ProofStored, Suite: "proofs/door", Name: "TestDoor",
			Path: "proofs/door/door_test.go", Code: "package door"},
		{Idx: 3, Origin: ProofOriginHoldout, ID: "key_log", Claim: "Every key turn lands in the log", Inputs: "three turns of key 7", Expected: "three log rows",
			State: ProofStored, Suite: "proofs/key_log", Name: "TestKeyLog", Path: "proofs/key_log/key_log_test.go", Code: "package key_log"},
	}
}

// proofsAtMainRig is card t1 of key under plan1, whose plan is p and
// whose rows are rows, with main at 29b0d33aa.
type proofsAtMainRig struct {
	r       *Runner
	hub     *forgejoHub
	store   *fakeStore
	kube    *fakeKube
	objects *fakeObjectStore
	worker  *fakeWorkerEndpoint
}

func newProofsAtMainRig(t *testing.T, key string, p Plan, rows []PlanProof) *proofsAtMainRig {
	t.Helper()
	task := gateTask
	task.Status, task.PRNumber = "running", 0
	task.Source, task.Parent, task.PlanKey = SourcePlan, "plan1", key
	g := &proofsAtMainRig{kube: newFakeKube(), objects: &fakeObjectStore{}}
	g.r, g.hub, g.store = newTestRunner(t, task, g.kube, g.objects)
	g.hub.mainHead = "29b0d33aa"
	g.worker = proofsWorker(t, g.r)
	g.store.tasks["plan1"] = Task{ID: "plan1", Status: "running", Repository: "sf/repo", Kind: "agent", Track: policy.TrackPlan}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.store.AddPlanRevision(context.Background(), "plan1", b, "sha1"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.store.PutPlanProofs(context.Background(), "plan1", 1, rows); err != nil {
		t.Fatal(err)
	}
	return g
}

// run runs ProofsAtMain on t1 while its proofs pod prints body, if one
// starts.
func (g *proofsAtMainRig) run(t *testing.T, body ...string) (ProofsAtMain, error) {
	t.Helper()
	done := make(chan struct{})
	var res ProofsAtMain
	var err error
	go func() {
		defer close(done)
		val, e := execActivity(t, g.r, ActivityProofsAtMain, "t1")
		if err = e; err == nil {
			err = val.Get(&res)
		}
	}()
	scriptReproPods(g.kube, body...)
	<-done
	return res, err
}

// list is the one list the proofs pod was handed.
func (g *proofsAtMainRig) list(t *testing.T) []harness.Proof {
	t.Helper()
	var list []harness.Proof
	if len(g.worker.lists) != 1 || json.Unmarshal([]byte(g.worker.lists[0]), &list) != nil {
		t.Fatalf("posted lists = %q, want one", g.worker.lists)
	}
	return list
}

// untouched: the run opened nothing, recorded no event and wrote no
// verdict row on any card.
func (g *proofsAtMainRig) untouched(t *testing.T) {
	t.Helper()
	rows, _ := g.store.PlanProofs(context.Background(), "plan1")
	for _, pp := range rows {
		if pp.State != ProofStored {
			t.Fatalf("row %d is %s, want every row still stored", pp.Idx, pp.State)
		}
	}
	if len(g.store.events) != 0 {
		t.Fatalf("events = %+v, want none", g.store.events)
	}
	for task, rows := range g.store.criteriaVerdicts {
		if len(rows) != 0 {
			t.Fatalf("verdict rows on %s = %+v, want none", task, rows)
		}
	}
}

func passedRun(t *testing.T, id, name string) harness.ProofRun {
	return harness.ProofRun{ID: id, Started: true, Report: junitGz(t, wrap(`<testcase name="`+name+`"></testcase>`))}
}

// A plan card whose every cited criterion passes at main's head closes on
// it: main's head and the criteria in the plan's order. The pod fetches
// main at its head and runs each cited criterion once, a stored proof
// with its file and an invariant's suite, and no criterion the card does
// not cite, nor a hold-out. Nothing opens and no row is written.
func TestProofsAtMainClosesOnAFullPass(t *testing.T) {
	p := proofsAtMainPlan()
	p.Cards[1].Acceptance = p.Cards[1].Acceptance[:2]
	g := newProofsAtMainRig(t, "sizes", p, proofsAtMainRows())
	res, err := g.run(t, proofLines(passedRun(t, "size_bound", "TestSizeBound"), passedRun(t, "all_green", "TestA")))
	if err != nil {
		t.Fatal(err)
	}
	if want := (ProofsAtMain{Head: "29b0d33aa", Met: true, Reason: "proofs met at 29b0d33: size_bound, all_green"}); !reflect.DeepEqual(res, want) {
		t.Fatalf("result = %+v, want %+v", res, want)
	}
	if len(g.kube.podOrder) != 1 {
		t.Fatalf("pods = %d, want one", len(g.kube.podOrder))
	}
	fetch := containerEnv(initContainer(t, g.kube.podAt(0).spec["spec"].(map[string]any), "fetch"))
	if fetch["PR_REF"] != "refs/heads/main" || fetch["REPO_SHA"] != "29b0d33aa" {
		t.Fatalf("fetch env = %v, want main at 29b0d33aa", fetch)
	}
	want := []harness.Proof{
		{ID: "size_bound", Suite: "./proofs/size_bound", Name: "TestSizeBound", File: &harness.ProofFile{Path: "proofs/size_bound/size_bound_test.go", Content: sizeBoundCode}},
		{ID: "all_green", Suite: "./..."},
	}
	if list := g.list(t); !reflect.DeepEqual(list, want) {
		t.Fatalf("list = %+v, want %+v", list, want)
	}
	step := timelineStep(t, g.objects, "t1", "proofs_at_main")
	if got := string(mustJSON(step["criteria"])); step["head"] != "29b0d33aa" ||
		got != `[{"id":"size_bound","outcome":"passed"},{"id":"all_green","outcome":"passed"}]` {
		t.Fatalf("line = %v", step)
	}
	g.untouched(t)
}

// A criterion not met at main is named by its id alone in the one
// finding for the next session: a red proof, a criterion with no proof
// and a flagged proof alike, the flagged one making no run. Neither the
// result nor the line carries a test's name, its code or its output.
func TestProofsAtMainNamesOnlyTheCriteriaNotMet(t *testing.T) {
	failed := harness.ProofRun{ID: "size_bound", Started: true, Exit: 1, Output: "=== RUN   TestSizeBound\n--- FAIL: TestSizeBound (0.00s)\n    want 400, got 404\nFAIL",
		Report: junitGz(t, wrap(`<testcase name="TestSizeBound"><failure message="Failed">--- FAIL: TestSizeBound (0.00s)&#xA;    want 400, got 404</failure></testcase>`))}
	green := passedRun(t, "all_green", "TestA")
	t.Run("two not met", func(t *testing.T) {
		g := newProofsAtMainRig(t, "sizes", proofsAtMainPlan(), proofsAtMainRows())
		res, err := g.run(t, proofLines(failed, green))
		if err != nil {
			t.Fatal(err)
		}
		if want := (ProofsAtMain{Head: "29b0d33aa", Findings: []string{"the proofs of size_bound and group_open are not met at main 29b0d33"}}); !reflect.DeepEqual(res, want) {
			t.Fatalf("result = %+v, want %+v", res, want)
		}
		step := timelineStep(t, g.objects, "t1", "proofs_at_main")
		if got := string(mustJSON(step["criteria"])); got != `[{"id":"size_bound","outcome":"failed"},{"id":"group_open","outcome":"no proof"},{"id":"all_green","outcome":"passed"}]` {
			t.Fatalf("line criteria = %s", got)
		}
		for _, surface := range []string{string(mustJSON(res)), string(mustJSON(step))} {
			for _, leak := range []string{"want 400", "TestSizeBound", "package", "proofs/size_bound", "key_log"} {
				if strings.Contains(surface, leak) {
					t.Fatalf("%s carries %q", surface, leak)
				}
			}
		}
		g.untouched(t)
	})
	t.Run("one not met", func(t *testing.T) {
		p := proofsAtMainPlan()
		p.Cards[1].Acceptance = p.Cards[1].Acceptance[:2]
		g := newProofsAtMainRig(t, "sizes", p, proofsAtMainRows())
		res, err := g.run(t, proofLines(failed, green))
		if err != nil {
			t.Fatal(err)
		}
		if want := (ProofsAtMain{Head: "29b0d33aa", Findings: []string{"the proof of size_bound is not met at main 29b0d33"}}); !reflect.DeepEqual(res, want) {
			t.Fatalf("result = %+v, want %+v", res, want)
		}
		g.untouched(t)
	})
	t.Run("flagged", func(t *testing.T) {
		p := proofsAtMainPlan()
		p.Cards[1].Acceptance = p.Cards[1].Acceptance[:2]
		rows := proofsAtMainRows()
		rows[0].Flag = ProofFlagGreen
		g := newProofsAtMainRig(t, "sizes", p, rows)
		res, err := g.run(t, proofLines(green))
		if err != nil {
			t.Fatal(err)
		}
		if want := (ProofsAtMain{Head: "29b0d33aa", Findings: []string{"the proof of size_bound is not met at main 29b0d33"}}); !reflect.DeepEqual(res, want) {
			t.Fatalf("result = %+v, want %+v", res, want)
		}
		if list := g.list(t); !reflect.DeepEqual(list, []harness.Proof{{ID: "all_green", Suite: "./..."}}) {
			t.Fatalf("list = %+v, want all_green's suite alone", list)
		}
		step := timelineStep(t, g.objects, "t1", "proofs_at_main")
		if got := string(mustJSON(step["criteria"])); got != `[{"id":"size_bound","outcome":"flagged"},{"id":"all_green","outcome":"passed"}]` {
			t.Fatalf("line criteria = %s", got)
		}
		g.untouched(t)
	})
}

// A card that cites nothing, the integration card, which closes through
// the criteria gate alone, and a task no plan filed run nothing: no pod,
// no token, no line.
func TestProofsAtMainOfACardThatCitesNothing(t *testing.T) {
	for name, key := range map[string]string{"the foundation card": "base", "the integration card": "integ", "no plan filed": ""} {
		t.Run(name, func(t *testing.T) {
			g := newProofsAtMainRig(t, key, proofsAtMainPlan(), proofsAtMainRows())
			if key == "" {
				task := gateTask
				task.Status, task.PRNumber = "running", 0
				g.store.tasks["t1"] = task
			}
			res, err := g.run(t)
			if err != nil || !reflect.DeepEqual(res, ProofsAtMain{}) {
				t.Fatalf("result = %+v, %v; want none", res, err)
			}
			if len(g.kube.podOrder) != 0 || len(g.hub.minted()) != 0 || strings.Contains(timeline(t, g.objects), "proofs_at_main") {
				t.Fatalf("pods %d, mints %v, timeline %s; want none", len(g.kube.podOrder), g.hub.minted(), timeline(t, g.objects))
			}
		})
	}
}

// A delta card, whose key the plan's revision does not hold, cites the
// criteria of the newest delta_card line on the plan's timeline that
// names its key.
func TestProofsAtMainOfADeltaCard(t *testing.T) {
	g := newProofsAtMainRig(t, "delta1", proofsAtMainPlan(), proofsAtMainRows())
	ctx := context.Background()
	for _, line := range []map[string]any{
		{"key": "delta1", "criteria": []string{"door"}},
		{"key": "delta1", "criteria": []string{"size_bound"}},
		{"key": "delta2", "criteria": []string{"group_open"}},
	} {
		if err := g.r.timeline.Append(ctx, "plan1", "delta_card", line); err != nil {
			t.Fatal(err)
		}
	}
	res, err := g.run(t, proofLines(passedRun(t, "size_bound", "TestSizeBound")))
	if err != nil {
		t.Fatal(err)
	}
	if want := (ProofsAtMain{Head: "29b0d33aa", Met: true, Reason: "proofs met at 29b0d33: size_bound"}); !reflect.DeepEqual(res, want) {
		t.Fatalf("result = %+v, want %+v", res, want)
	}
	if list := g.list(t); len(list) != 1 || list[0].ID != "size_bound" {
		t.Fatalf("list = %+v, want size_bound's proof alone", list)
	}
	g.untouched(t)
}

// A pod that never starts is the start-failed line and no close, no
// finding and no error: the card goes on.
func TestProofsAtMainThatCannotStart(t *testing.T) {
	g := newProofsAtMainRig(t, "sizes", proofsAtMainPlan(), proofsAtMainRows())
	g.kube.onCreatePod = func() {
		p := g.kube.podOrder[len(g.kube.podOrder)-1]
		p.phase, p.containers = "Pending", []ContainerState{{Name: "fetch", State: "waiting", Reason: "ErrImagePull", Message: imageNotFound}}
	}
	res, err := g.run(t)
	if err != nil || !reflect.DeepEqual(res, ProofsAtMain{}) {
		t.Fatalf("result = %+v, %v; want none and no error", res, err)
	}
	step := timelineStep(t, g.objects, "t1", "proofs_at_main")
	if failed, _ := step["start_failed"].(string); step["head"] != "29b0d33aa" || !strings.HasPrefix(failed, "start failed: image pull: ") || step["criteria"] != nil {
		t.Fatalf("line = %v, want main's head and the start failure", step)
	}
	g.untouched(t)
}

// A pod that ends before its closing line is no error: the criterion it
// reached keeps its outcome and the one it never reached is no report,
// among the criteria not met.
func TestProofsAtMainThatEndsEarly(t *testing.T) {
	p := proofsAtMainPlan()
	p.Cards[1].Acceptance = p.Cards[1].Acceptance[:2]
	g := newProofsAtMainRig(t, "sizes", p, proofsAtMainRows())
	ended := proofLines(passedRun(t, "size_bound", "TestSizeBound"))
	res, err := g.run(t, ended[:strings.LastIndex(ended, "\n")])
	if err != nil {
		t.Fatal(err)
	}
	if want := (ProofsAtMain{Head: "29b0d33aa", Findings: []string{"the proof of all_green is not met at main 29b0d33"}}); !reflect.DeepEqual(res, want) {
		t.Fatalf("result = %+v, want %+v", res, want)
	}
	step := timelineStep(t, g.objects, "t1", "proofs_at_main")
	if got := string(mustJSON(step["criteria"])); got != `[{"id":"size_bound","outcome":"passed"},{"id":"all_green","outcome":"no report"}]` {
		t.Fatalf("line criteria = %s", got)
	}
	g.untouched(t)
}
