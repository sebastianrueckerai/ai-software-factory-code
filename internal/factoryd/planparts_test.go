package factoryd

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.temporal.io/sdk/converter"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/answers"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// partOfPlan is the part of step that carries p's fields, as the step's
// tool takes them: the plan's keys the step's answer type holds, a card
// without the stamps factoryd adds.
func partOfPlan(step string, p Plan) (json.RawMessage, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	keys := map[string][]string{
		stepPlanSpec:     {"spec", "summary", "questions"},
		stepPlanCriteria: {"criteria"},
		stepPlanCards:    {"outline", "cards", "questions"},
		stepPlanReplan:   {"spec", "summary", "outline", "criteria", "cards", "questions"},
	}[step]
	part := map[string]any{}
	for _, k := range keys {
		raw, ok := all[k]
		if !ok {
			continue
		}
		if k == "cards" {
			var cards []map[string]json.RawMessage
			if err := json.Unmarshal(raw, &cards); err != nil {
				return nil, err
			}
			for _, c := range cards {
				delete(c, "round")
				delete(c, "companions")
				delete(c, "brief_unchecked")
			}
			part[k] = cards
			continue
		}
		part[k] = raw
	}
	if step == stepPlanRevert {
		cards := []answers.Revert{}
		for _, k := range p.TakeOffMain {
			cards = append(cards, answers.Revert{Key: k, Reason: p.TakeOffReasons[k]})
		}
		part["cards"] = cards
	}
	return json.Marshal(part)
}

// fileWholePlan files plan, a whole plan as json, through the parts the
// task's run holds steps for: each step's part in order, the first refusal
// or the stored revision as the result.
func fileWholePlan(r *Runner, ctx context.Context, taskID, plan string) (PlanResult, error) {
	p, found := decodePlan("```json\n" + plan + "\n```")
	if len(found) > 0 {
		return PlanResult{Findings: found}, nil
	}
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return PlanResult{}, err
	}
	shape, _, err := planShape(r.cfg.Policy(), t)
	if err != nil {
		return PlanResult{}, err
	}
	tl, err := r.timeline.Objects.Get(ctx, "sessions/"+taskID+"/timeline.jsonl")
	if err != nil {
		return PlanResult{}, err
	}
	newest, _, err := r.planAt(ctx, taskID, 0)
	if err != nil {
		return PlanResult{}, err
	}
	step, pending, err := pendingIn(tl, taskID)
	if err != nil {
		return PlanResult{}, err
	}
	var res PlanResult
	for _, name := range planPath(shape, newest, step, pending) {
		if name == stepRepoMap {
			continue
		}
		part, err := partOfPlan(name, p)
		if err != nil {
			return PlanResult{}, err
		}
		if res, err = r.FilePart(ctx, taskID, name, part); err != nil || len(res.Findings) > 0 {
			return res, err
		}
	}
	return res, nil
}

// The four paths of the state table, repo-map first in each, and the revert
// before it when a replan takes work off main.
func TestPlanStepsFor(t *testing.T) {
	last := &Plan{}
	for name, tc := range map[string]struct {
		last    *Plan
		step    ReplanStep
		pending bool
		want    []string
	}{
		"no revision":                {nil, ReplanStep{}, false, []string{"repo-map", "planner-spec", "planner-criteria", "planner-cards"}},
		"origin round":               {last, ReplanStep{Origin: ReplanRound}, true, []string{"repo-map", "planner-cards"}},
		"a revision alone":           {last, ReplanStep{}, false, []string{"repo-map", "planner-replan"}},
		"a person's comment":         {last, ReplanStep{Origin: ReplanPerson}, true, []string{"repo-map", "planner-replan"}},
		"a cut":                      {last, ReplanStep{Origin: ReplanCut}, true, []string{"repo-map", "planner-replan"}},
		"off main":                   {last, ReplanStep{Origin: ReplanPerson, OffMain: true}, true, []string{"planner-revert", "repo-map", "planner-replan"}},
		"a step that is not pending": {last, ReplanStep{Origin: ReplanRound, OffMain: true}, false, []string{"repo-map", "planner-replan"}},
	} {
		if got := PlanStepsFor(tc.last, tc.step, tc.pending); !slices.Equal(got, tc.want) {
			t.Errorf("%s: PlanStepsFor = %q, want %q", name, got, tc.want)
		}
	}
	if got, want := planPath(policy.PlanShape{MaxCards: 12}, nil, ReplanStep{}, false), []string{"repo-map", "planner-spec", "planner-cards"}; !slices.Equal(got, want) {
		t.Errorf("a shape with no integration card runs %q, want %q: it holds no criteria", got, want)
	}
}

// jsonKeys are the property names of struct type t, by its json tags.
func jsonKeys(t reflect.Type) []string {
	var keys []string
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		keys = append(keys, name)
	}
	return keys
}

// answers.Card writes the plan card's fields and leaves out the stamps
// factoryd adds: round, companions and brief_unchecked.
func TestAnswerTypesMatchThePlan(t *testing.T) {
	card, plan := jsonKeys(reflect.TypeFor[answers.Card]()), jsonKeys(reflect.TypeFor[PlanCard]())
	for _, k := range card {
		if !slices.Contains(plan, k) {
			t.Errorf("answers.Card has %q, which PlanCard lacks", k)
		}
	}
	var others []string
	for _, k := range plan {
		if !slices.Contains(card, k) {
			others = append(others, k)
		}
	}
	if want := []string{"round", "companions", "brief_unchecked"}; !slices.Equal(others, want) {
		t.Errorf("PlanCard keys answers.Card lacks = %q, want %q", others, want)
	}
	if !slices.Equal(jsonKeys(reflect.TypeFor[answers.Criterion]()), jsonKeys(reflect.TypeFor[PlanCriterion]())) {
		t.Error("answers.Criterion and PlanCriterion hold different keys")
	}
}

// The spec, criteria and cards parts of a first path each pass their door and
// stay in the plan draft; only the cards part, the path's last, stores a
// revision, and it equals the plan the parts make.
func TestFilePartMergesIntoTheDraft(t *testing.T) {
	r, store, _ := draftRig(t)
	ctx := context.Background()
	plan := testPlan()
	parts := map[string]json.RawMessage{}
	for _, step := range []string{stepPlanSpec, stepPlanCriteria, stepPlanCards} {
		part, err := partOfPlan(step, plan)
		if err != nil {
			t.Fatal(err)
		}
		parts[step] = part
	}
	for i, step := range []string{stepPlanSpec, stepPlanCriteria} {
		res, err := r.FilePart(ctx, "t-plan", step, parts[step])
		if err != nil || len(res.Findings) != 0 || res.Revision != 0 {
			t.Fatalf("%s: %+v, %v; want it kept, with no revision", step, res, err)
		}
		held, err := r.heldParts(ctx, "t-plan")
		if err != nil || len(held) != i+1 || held[step] == nil {
			t.Fatalf("after %s the draft holds %v, %v", step, slices.Collect(func(yield func(string) bool) {
				for k := range held {
					yield(k)
				}
			}), err)
		}
		if _, ok, _ := store.PlanRevision(ctx, "t-plan", 0); ok {
			t.Fatalf("a revision is stored after %s", step)
		}
	}
	res, err := r.FilePart(ctx, "t-plan", stepPlanCards, parts[stepPlanCards])
	if err != nil || len(res.Findings) != 0 || res.Revision != 1 || !res.Gate {
		t.Fatalf("the cards part: %+v, %v; want revision 1 waiting on the confirmation", res, err)
	}
	want := testPlan()
	want.Round = 1
	for i := range want.Cards {
		want.Cards[i].Round = 1
	}
	got, revision, err := r.planAt(ctx, "t-plan", 0)
	if err != nil || revision != 1 || !reflect.DeepEqual(*got, want) {
		t.Fatalf("revision %d:\n%+v\nwant\n%+v\n%v", revision, got, want, err)
	}
	if _, ok, _ := store.PlanDraft(ctx, "t-plan"); ok {
		t.Fatal("the draft outlived the stored revision")
	}
}

// A part's refusal names the field and keeps the part with its findings: a
// duplicate criterion id is the finding criteriaFindings gives, by hand
// "plan: criterion id links_crud is used twice" and nothing else.
func TestFilePartRefusalNamesTheField(t *testing.T) {
	r, store, _ := draftRig(t)
	ctx := context.Background()
	plan := testPlan()
	plan.Criteria[1].ID = "links_crud"
	part, err := partOfPlan(stepPlanCriteria, plan)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.FilePart(ctx, "t-plan", stepPlanCriteria, part)
	want := []string{"plan: criterion id links_crud is used twice"}
	if err != nil || !slices.Equal(res.Findings, want) {
		t.Fatalf("findings %q, %v; want %q", res.Findings, err, want)
	}
	if found := criteriaFindings(plan, testShape); !slices.Contains(found, want[0]) {
		t.Fatalf("criteriaFindings gives %q, which lacks %q", found, want[0])
	}
	d, ok, _ := store.PlanDraft(ctx, "t-plan")
	if !ok || !slices.Equal(d.Findings, want) || !strings.Contains(string(d.Plan), `"planner-criteria"`) {
		t.Fatalf("draft %+v, %v; want the part kept with its finding", d, ok)
	}
	plan = testPlan()
	if part, err = partOfPlan(stepPlanCriteria, plan); err != nil {
		t.Fatal(err)
	}
	if res, err = r.FilePart(ctx, "t-plan", stepPlanCriteria, part); err != nil || len(res.Findings) != 0 {
		t.Fatalf("the sound part: %+v, %v", res, err)
	}
	if d, _, _ := store.PlanDraft(ctx, "t-plan"); len(d.Findings) != 0 {
		t.Fatalf("a sound part left findings %q", d.Findings)
	}
}

// A filing for a step the run does not hold is refused with the tools of the
// run, and a field the answer type lacks is refused.
func TestFilePartRefusesAStepOffThePath(t *testing.T) {
	r, _, _ := draftRig(t)
	ctx := context.Background()
	res, err := r.FilePart(ctx, "t-plan", stepPlanReplan, json.RawMessage(`{}`))
	want := "plan: file_replan files no part of this plan's run now: its steps are file_spec, file_criteria, file_cards. File with the tool of your step"
	if err != nil || !slices.Equal(res.Findings, []string{want}) {
		t.Fatalf("findings %q, %v; want %q", res.Findings, err, want)
	}
	res, err = r.FilePart(ctx, "t-plan", stepPlanSpec, json.RawMessage(`{"spec":{"outcome":"o"},"summary":"s","round":1}`))
	if err != nil || len(res.Findings) != 1 || !strings.HasPrefix(res.Findings[0], "file_spec: the answer does not decode") {
		t.Fatalf("an unknown field: %q, %v", res.Findings, err)
	}
}

// The plan draft keeps the parts between sessions, with no schema change: the
// draft's plan is one JSON object, each part one key beside the map. A map
// written between two parts leaves them, the draft's state tells the step a
// refusal came at, and a part the draft holds is no refused filing while it
// holds no findings. This settles the plan's open question for the fake
// store; the Postgres store keeps the object in a jsonb column, which
// TestPGStorePlanDraft covers for any object.
func TestPlanDraftHoldsPartsAcrossSessions(t *testing.T) {
	r, store, _ := draftRig(t)
	ctx := context.Background()
	plan := testPlan()
	spec, _ := partOfPlan(stepPlanSpec, plan)
	criteria, _ := partOfPlan(stepPlanCriteria, plan)
	if res, err := r.FilePart(ctx, "t-plan", stepPlanSpec, spec); err != nil || len(res.Findings) != 0 {
		t.Fatalf("spec: %+v, %v", res, err)
	}
	seedMap(t, r, "t-plan", "abc123")
	if res, err := r.FilePart(ctx, "t-plan", stepPlanCriteria, criteria); err != nil || len(res.Findings) != 0 {
		t.Fatalf("criteria: %+v, %v", res, err)
	}
	path := []string{stepRepoMap, stepPlanSpec, stepPlanCriteria, stepPlanCards}
	state, err := r.planDraftState(ctx, "t-plan", path)
	if err != nil || !state.held[stepPlanSpec] || !state.held[stepPlanCriteria] || state.held[stepPlanCards] || state.refused != "" {
		t.Fatalf("state %+v, %v; want spec and criteria held, none refused", state, err)
	}
	if held, err := r.heldMap(ctx, "t-plan"); err != nil || held == nil || held.Head != "abc123" {
		t.Fatalf("the map after two parts: %+v, %v", held, err)
	}
	if strings.Contains(string(state.d.Plan), `"map"`) || !strings.Contains(string(state.d.Plan), `"planner-spec"`) {
		t.Fatalf("the draft's plan %s, want the parts without the map", state.d.Plan)
	}
	if note, err := r.planDraftNote(ctx, "t-plan", PlannerOutOfTime); err != nil || note != "" {
		t.Fatalf("held parts are a refused filing: %q, %v", note, err)
	}
	plan.Criteria[1].ID = "links_crud"
	bad, _ := partOfPlan(stepPlanCriteria, plan)
	if res, err := r.FilePart(ctx, "t-plan", stepPlanCriteria, bad); err != nil || len(res.Findings) != 1 {
		t.Fatalf("a duplicate: %+v, %v", res, err)
	}
	state, err = r.planDraftState(ctx, "t-plan", path)
	if err != nil || state.refused != stepPlanCriteria || !state.held[stepPlanSpec] {
		t.Fatalf("state %+v, %v; want the refusal at the criteria step and the spec held", state, err)
	}
	d, _, _ := store.PlanDraft(ctx, "t-plan")
	if len(d.Findings) != 1 {
		t.Fatalf("draft findings %q", d.Findings)
	}
}

// The revert's findings: a key named twice, a key the record lacks and a
// reason left out or blank, each by name.
func TestRevertFindings(t *testing.T) {
	st := replanState{old: []Task{{ID: "k1", PlanKey: "links-api", MergeCommit: "m1"}}, merged: map[string]bool{"k1": true}, reverted: map[string]bool{}, integration: map[string]bool{}}
	got := revertFindings(st, answers.RevertAnswer{Cards: []answers.Revert{
		{Key: "links-api", Reason: "the redirects break"}, {Key: "links-api", Reason: "again"}, {Key: "ghost", Reason: "x"}, {Key: "other"}, {Key: "blank", Reason: " \n"}}})
	want := []string{
		"plan: the card links-api is named twice",
		"plan: the revision takes ghost off main, and ghost is no card of this plan's record. Name the key of a merged card",
		"plan: the revision takes other off main, and other is no card of this plan's record. Name the key of a merged card",
		"plan: the card other has no reason: say what breaks if its work stays on main, or leave other out",
		"plan: the revision takes blank off main, and blank is no card of this plan's record. Name the key of a merged card",
		"plan: the card blank has no reason: say what breaks if its work stays on main, or leave blank out",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("findings\n%q\nwant\n%q", got, want)
	}
	if got := revertFindings(st, answers.RevertAnswer{Cards: []answers.Revert{{Key: "links-api", Reason: "the redirects break"}}}); len(got) != 0 {
		t.Fatalf("a merged card with a reason: %q", got)
	}
}

// RunPlan on a first path runs the map step and the three planner steps in
// order, each on its own model: the map on small, the planner steps on big;
// each session files its part, and the last one's filing is the run's
// revision.
func TestRunPlanRunsEachStepOfItsPath(t *testing.T) {
	r, _, kube := filePlanRig(t)
	ctx := context.Background()
	if err := r.setMap(ctx, "t-plan", nil); err != nil {
		t.Fatal(err)
	}
	plan := testPlan()
	mapJSON, _ := json.Marshal(testMap())
	done := make(chan error)
	var out converter.EncodedValue
	go func() {
		var err error
		out, err = execActivity(t, r, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string(nil))
		done <- err
	}()
	for i, f := range []struct {
		tool string
		part json.RawMessage
	}{
		{policy.ToolFileMap, mapJSON},
		{policy.ToolFileSpec, mustPart(t, stepPlanSpec, plan)},
		{policy.ToolFileCriteria, mustPart(t, stepPlanCriteria, plan)},
		{policy.ToolFileCards, mustPart(t, stepPlanCards, plan)},
	} {
		awaitPods(t, kube, done, i+1)
		if found, err := r.FileAnswer(ctx, "t-plan", f.tool, f.part); err != nil || len(found) != 0 {
			t.Fatalf("%s = %q, %v", f.tool, found, err)
		}
		kube.scriptPods(i, resultLine(true, "filed"))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var res PlanResult
	if err := out.Get(&res); err != nil || res.Revision != 1 || len(res.Findings) != 0 {
		t.Fatalf("result %+v, %v; want revision 1", res, err)
	}
	var got []string
	for i := range kube.podOrder {
		env := podEnv(kube.podAt(i))
		got = append(got, env["STEP"]+"@"+env["MODEL"])
	}
	want := []string{"repo-map@factory/small", "planner-spec@factory/big", "planner-criteria@factory/big", "planner-cards@factory/big"}
	if !slices.Equal(got, want) {
		t.Fatalf("sessions %q, want %q", got, want)
	}
	if env := podEnv(kube.podAt(3)); env["REPO_MAP"] != testMapText || !strings.Contains(env["REPORT_GUIDE"], "What the spec step filed") || !strings.Contains(env["REPORT_GUIDE"], "What the criteria step filed") {
		t.Fatalf("the cards pod: REPO_MAP %q, guide %q; want the map and both earlier filings", env["REPO_MAP"], env["REPORT_GUIDE"])
	}
}

func mustPart(t *testing.T, step string, p Plan) json.RawMessage {
	t.Helper()
	part, err := partOfPlan(step, p)
	if err != nil {
		t.Fatal(err)
	}
	return part
}

// A run starts where its draft stops: a step whose part the draft holds is
// not run again, and the step a refusal came at runs again with the draft's
// brief. The draft here holds the spec and a refused criteria part.
func TestRunPlanSkipsTheStepsItsDraftHolds(t *testing.T) {
	r, _, kube := filePlanRig(t)
	ctx := context.Background()
	seedMap(t, r, "t-plan", "abc123")
	plan := testPlan()
	if res, err := r.FilePart(ctx, "t-plan", stepPlanSpec, mustPart(t, stepPlanSpec, plan)); err != nil || len(res.Findings) != 0 {
		t.Fatalf("spec: %+v, %v", res, err)
	}
	dup := testPlan()
	dup.Criteria[1].ID = "links_crud"
	if res, err := r.FilePart(ctx, "t-plan", stepPlanCriteria, mustPart(t, stepPlanCriteria, dup)); err != nil || len(res.Findings) != 1 {
		t.Fatalf("criteria: %+v, %v", res, err)
	}
	done := make(chan error)
	var out converter.EncodedValue
	go func() {
		var err error
		out, err = execActivity(t, r, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string{"an earlier finding"})
		done <- err
	}()
	for i, f := range []struct {
		tool string
		part json.RawMessage
	}{
		{policy.ToolFileCriteria, mustPart(t, stepPlanCriteria, plan)},
		{policy.ToolFileCards, mustPart(t, stepPlanCards, plan)},
	} {
		awaitPods(t, kube, done, i+1)
		if found, err := r.FileAnswer(ctx, "t-plan", f.tool, f.part); err != nil || len(found) != 0 {
			t.Fatalf("%s = %q, %v", f.tool, found, err)
		}
		kube.scriptPods(i, resultLine(true, "filed"))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var res PlanResult
	if err := out.Get(&res); err != nil || res.Revision != 1 {
		t.Fatalf("result %+v, %v; want revision 1", res, err)
	}
	if len(kube.podOrder) != 2 || podEnv(kube.podAt(0))["STEP"] != "planner-criteria" || podEnv(kube.podAt(1))["STEP"] != "planner-cards" {
		t.Fatalf("%d sessions, the first %q: want the criteria and the cards, no spec session", len(kube.podOrder), podEnv(kube.podAt(0))["STEP"])
	}
	var first, second []string
	if err := json.Unmarshal([]byte(podEnv(kube.podAt(0))["FINDINGS"]), &first); err != nil || len(first) != 2 || first[0] != "an earlier finding" ||
		!strings.Contains(first[1], "your last filing was refused and kept as a draft") || !strings.Contains(first[1], "criterion id links_crud is used twice") {
		t.Fatalf("the criteria session's findings %q, %v; want the earlier finding and the draft's brief", first, err)
	}
	if err := json.Unmarshal([]byte(podEnv(kube.podAt(1))["FINDINGS"]), &second); err == nil && len(second) != 0 {
		t.Fatalf("the cards session was told the criteria session's findings: %q", second)
	}
}

// seedEarlierParts files p's spec and criteria parts, so that a run of the
// first path starts at its cards step.
func seedEarlierParts(t *testing.T, r *Runner, p Plan) {
	t.Helper()
	for _, step := range []string{stepPlanSpec, stepPlanCriteria} {
		if res, err := r.FilePart(context.Background(), "t-plan", step, mustPart(t, step, p)); err != nil || len(res.Findings) != 0 {
			t.Fatalf("%s: %+v, %v", step, res, err)
		}
	}
}

// filePlanPart files the part of step that carries the plan json plan, as a
// session of that step files it, with no read of the record before the filing.
func filePlanPart(r *Runner, step, plan string) (PlanResult, error) {
	p, found := decodePlan("```json\n" + plan + "\n```")
	if len(found) > 0 {
		return PlanResult{Findings: found}, nil
	}
	part, err := partOfPlan(step, p)
	if err != nil {
		return PlanResult{}, err
	}
	return r.FilePart(context.Background(), "t-plan", step, part)
}
