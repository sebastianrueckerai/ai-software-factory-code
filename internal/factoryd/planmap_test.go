package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/converter"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/answers"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"
)

// testMap is the map every plan rig's draft holds for the task's head, so
// that a planner session runs without a map session before it.
func testMap() answers.MapAnswer {
	return answers.MapAnswer{
		Directories: []answers.DirNote{{Path: "internal/store", Role: "the Postgres store"}, {Path: "cmd/app", Role: "the binary"}},
		Conventions: []string{"tests sit beside the code"},
		Check:       "bash check changed",
	}
}

// testMapText is testMap as the planner reads it.
const testMapText = "Repository map, filed by the map step at the commit planning starts from:\n" +
	"Directories:\n- internal/store: the Postgres store\n- cmd/app: the binary\n" +
	"Conventions every card follows:\n- tests sit beside the code\n" +
	"Check: bash check changed"

// seedMap has task taskID's plan draft hold testMap for head.
func seedMap(t *testing.T, r *Runner, taskID, head string) {
	t.Helper()
	if err := r.setMap(context.Background(), taskID, &draftMap{Head: head, Answer: testMap()}); err != nil {
		t.Fatal(err)
	}
}

// execRunPlan is RunPlan on a plan task whose draft holds the map of its head,
// unless it holds one already: the planner runs without a map session before
// it. args are the activity's, from the task's id on.
func execRunPlan(t *testing.T, r *Runner, args ...any) (converter.EncodedValue, error) {
	t.Helper()
	id := args[0].(string)
	tk, ok, err := r.store.Task(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("task %s: %v, %v", id, ok, err)
	}
	if held, err := r.heldMap(context.Background(), id); err != nil {
		t.Fatal(err)
	} else if held == nil {
		seedMap(t, r, id, cmp.Or(tk.SHA, "m0"))
	}
	return execActivity(t, r, ActivityRunPlan, args...)
}

// awaitPods waits until the fake cluster holds n pods.
func awaitPods(t *testing.T, kube *fakeKube, done chan error, n int) {
	t.Helper()
	for {
		kube.mu.Lock()
		have := len(kube.podOrder)
		kube.mu.Unlock()
		if have >= n {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("the activity ended with %d of %d pods: %v", have, n, err)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// runPlanOnce runs RunPlan with a planner that files nothing, and the pods
// of the run scripted: the map session, when one runs, files filed before it
// ends. It returns how many pods the cluster holds afterwards.
func runPlanOnce(t *testing.T, r *Runner, kube *fakeKube, mapSession bool, filed string) int {
	t.Helper()
	before := len(kube.podOrder)
	done := make(chan error)
	go func() {
		_, err := execActivity(t, r, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string(nil))
		done <- err
	}()
	if mapSession {
		awaitPods(t, kube, done, before+1)
		if found, err := r.FileAnswer(context.Background(), "t-plan", policy.ToolFileMap, json.RawMessage(filed)); err != nil || len(found) != 0 {
			t.Fatalf("file_map = %v, %v", found, err)
		}
		kube.scriptPods(before, resultLine(true, "mapped"))
		before++
	}
	awaitPods(t, kube, done, before+1)
	kube.scriptPods(before, resultLine(true, "no plan"))
	if err := <-done; err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	return len(kube.podOrder)
}

// One head, one map session: the first RunPlan runs the map step on the small
// alias before its planner, the second, on the same head, runs the planner
// alone, and a task whose head moved maps again. Pods by hand: 2, then 3,
// then 5.
func TestPlanMapRunsOncePerHead(t *testing.T) {
	r, store, kube := filePlanRig(t)
	if err := r.setMap(context.Background(), "t-plan", nil); err != nil {
		t.Fatal(err)
	}
	filed := `{"directories":[{"path":"internal/store","role":"the Postgres store"},{"path":"cmd/app","role":"the binary"}],"conventions":["tests sit beside the code"],"check":"bash check changed"}`
	if got := runPlanOnce(t, r, kube, true, filed); got != 2 {
		t.Fatalf("the first run left %d pods, want the map session's and the planner's", got)
	}
	steps := func() []string {
		var out []string
		for i := range kube.podOrder {
			out = append(out, podEnv(kube.podAt(i))["STEP"]+"@"+podEnv(kube.podAt(i))["MODEL"])
		}
		return out
	}
	if want := []string{"repo-map@factory/small", "planner-spec@factory/big"}; !slices.Equal(steps(), want) {
		t.Fatalf("sessions = %q, want %q", steps(), want)
	}
	if got := runPlanOnce(t, r, kube, false, ""); got != 3 {
		t.Fatalf("the second run on the same head left %d pods, want one planner session more", got)
	}
	tk := store.tasks["t-plan"]
	tk.SHA = "def456"
	store.tasks["t-plan"] = tk
	if got := runPlanOnce(t, r, kube, true, filed); got != 5 {
		t.Fatalf("the run on a moved head left %d pods, want a map session and a planner more", got)
	}
	if held, err := r.heldMap(context.Background(), "t-plan"); err != nil || held == nil || held.Head != "def456" {
		t.Fatalf("held map = %+v, %v, want one stamped with the new head", held, err)
	}
}

// The map step starts from the repository root's map, which the worker reads,
// at the head it was asked for, and the session that ends without filing a
// map is an error that names it.
func TestPlanMapSessionReadsTheRootAtTheHead(t *testing.T) {
	r, _, kube := filePlanRig(t)
	if err := r.setMap(context.Background(), "t-plan", nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() {
		_, err := execActivity(t, r, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string(nil))
		done <- err
	}()
	awaitPods(t, kube, done, 1)
	env := podEnv(kube.podAt(0))
	if env["DIRECTORIES"] != `["."]` || env["STEP"] != "repo-map" || env["MODE"] != "report" || env["REPO_MAP"] != "" {
		t.Fatalf("map pod env = %v, want step repo-map in report mode over directory .", env)
	}
	kube.scriptPods(0, resultLine(true, "I read it all"))
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "ended without filing a map: I read it all") {
		t.Fatalf("RunPlan = %v, want the map session's missing filing named with its last message", err)
	}
}

// file_map keeps what it checks: every text on one line and cut at its bound,
// blank entries dropped, lists capped, and a map with no directory refused.
// Bounds by hand: 40 directories, 12 conventions, a role of 160 bytes, a
// convention and the check of 200.
func TestMapAnswerBounds(t *testing.T) {
	dirs := make([]answers.DirNote, 41)
	for i := range dirs {
		dirs[i] = answers.DirNote{Path: "d" + strings.Repeat("x", i), Role: "r"}
	}
	conventions := make([]string, 13)
	for i := range conventions {
		conventions[i] = "c" + strings.Repeat("y", i)
	}
	raw, _ := json.Marshal(answers.MapAnswer{
		Directories: append([]answers.DirNote{{Path: " a/b ", Role: strings.Repeat("a", 200)}, {Path: " ", Role: "no path"}, {Path: "e", Role: "two\nlines\t here"}}, dirs...),
		Conventions: append([]string{"  ", "one\nline"}, conventions...),
		Check:       strings.Repeat("k", 201),
	})
	got, found := checkMapFiling(raw)
	if len(found) != 0 {
		t.Fatalf("findings %q", found)
	}
	if len(got.Directories) != 40 || got.Directories[0] != (answers.DirNote{Path: "a/b", Role: strings.Repeat("a", 160) + "…"}) ||
		got.Directories[1] != (answers.DirNote{Path: "e", Role: "two lines here"}) {
		t.Errorf("directories = %d, first %+v, second %+v; want 40, a role cut at 160 and marked, newline and tab turned into one space", len(got.Directories), got.Directories[0], got.Directories[1])
	}
	if len(got.Conventions) != 12 || got.Conventions[0] != "one line" || got.Conventions[1] != "c" {
		t.Errorf("conventions = %d, %q, %q; want 12 with the blank one dropped and the newline joined", len(got.Conventions), got.Conventions[0], got.Conventions[1])
	}
	if got.Check != strings.Repeat("k", 200)+"…" {
		t.Errorf("check = %d bytes, want 200 and the cut mark", len(got.Check))
	}
	if _, found := checkMapFiling(json.RawMessage(`{"directories":[{"path":" ","role":"x"}],"conventions":[],"check":""}`)); len(found) != 1 || !strings.Contains(found[0], "names no directory") {
		t.Errorf("a map with no directory: findings %q", found)
	}
	if _, found := checkMapFiling(json.RawMessage(`{"directories":[],"conventions":[],"check":"","extra":1}`)); len(found) != 1 || !strings.HasPrefix(found[0], "file_map: the answer does not decode") {
		t.Errorf("an unknown field: findings %q", found)
	}
	schema, _ := json.Marshal(prompts.AnswerTool(reflect.TypeFor[answers.MapAnswer]()).Parameters)
	for _, want := range []string{`"maxItems":40`, `"maxItems":12`, `"maxLength":160`, `"maxLength":200`} {
		if !strings.Contains(string(schema), want) {
			t.Errorf("the map's tool schema lacks %s: %s", want, schema)
		}
	}
}

// The planner's facts carry the map: the planner pod's REPO_MAP is the map
// as the planner reads it, built from the draft's, and the map session's
// filing is checked at file_map, not at the planner.
func TestPlannerFactsCarryTheMap(t *testing.T) {
	r, _, kube := filePlanRig(t)
	seedMap(t, r, "t-plan", "abc123")
	if got := runPlanOnce(t, r, kube, false, ""); got != 1 {
		t.Fatalf("pods = %d, want the planner's alone: the draft holds the map", got)
	}
	env := podEnv(kube.podAt(0))
	if env["STEP"] != "planner-spec" || env["REPO_MAP"] != testMapText {
		t.Fatalf("planner pod: step %q, REPO_MAP\n%s\nwant\n%s", env["STEP"], env["REPO_MAP"], testMapText)
	}
}

// The plan draft holds the map beside a planner's refused filing without a
// schema change: the draft's plan is one JSON object, and the map is one key
// of it. A refused filing keeps the map, the planner's draft brief shows the
// plan without it, a map alone is no refused filing, and a stored revision
// leaves the map for the next path on the same head.
func TestPlanDraftHoldsTheMapBesideARefusedFiling(t *testing.T) {
	r, store, _ := draftRig(t)
	ctx := context.Background()
	seedMap(t, r, "t-plan", "abc123")
	d, ok, _ := store.PlanDraft(ctx, "t-plan")
	if !ok || len(d.Findings) != 0 {
		t.Fatalf("draft %+v, %v, want the map with no findings", d, ok)
	}
	if note, err := r.planDraftNote(ctx, "t-plan", PlannerOutOfTime); err != nil || note != "" {
		t.Fatalf("a map alone is a refused filing: %q, %v", note, err)
	}
	over := testPlan()
	over.Cards[1].Size = "huge"
	if _, err := fileWholePlan(r, ctx, "t-plan", planJSON(t, over)); err != nil {
		t.Fatal(err)
	}
	d, _, _ = store.PlanDraft(ctx, "t-plan")
	rest, held, err := splitMap(d.Plan)
	if err != nil || held == nil || held.Head != "abc123" || !reflect.DeepEqual(held.Answer, testMap()) || !strings.Contains(string(rest), `"huge"`) || strings.Contains(string(rest), `"map"`) {
		t.Fatalf("after a refusal: held %+v, rest %s, %v; want the map, the plan and no map in the plan", held, rest, err)
	}
	if brief := r.draftBrief(PlanDraft{Plan: rest, Findings: d.Findings}, nil); strings.Contains(brief, "Postgres store") {
		t.Fatalf("the draft brief shows the map:\n%s", brief)
	}
	if res, err := fileWholePlan(r, ctx, "t-plan", planJSON(t, testPlan())); err != nil || res.Revision != 1 {
		t.Fatalf("filed %+v, %v", res, err)
	}
	d, ok, _ = store.PlanDraft(ctx, "t-plan")
	if held, err := r.heldMap(ctx, "t-plan"); !ok || len(d.Findings) != 0 || err != nil || held == nil || held.Head != "abc123" {
		t.Fatalf("after the revision: draft %+v, %v, map %+v, %v; want the map alone", d, ok, held, err)
	}
	if err := r.setMap(ctx, "t-plan", nil); err != nil {
		t.Fatal(err)
	}
	if held, _ := r.heldMap(ctx, "t-plan"); held != nil {
		t.Fatalf("setMap(nil) left %+v", held)
	}
}

func TestRenderMapLeavesOutEmptySections(t *testing.T) {
	got := renderMap(answers.MapAnswer{Directories: []answers.DirNote{{Path: "a", Role: "b"}}})
	if want := "Repository map, filed by the map step at the commit planning starts from:\nDirectories:\n- a: b"; got != want {
		t.Fatalf("renderMap = %q, want %q", got, want)
	}
	if got := renderMap(testMap()); got != testMapText {
		t.Fatalf("renderMap = %q, want %q", got, testMapText)
	}
}
