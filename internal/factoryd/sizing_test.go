package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// sizingCard is t1, a writer's queued direct card of issue 5 with the
// default budget, unsized.
func sizingCard() Task {
	return Task{ID: "t1", Status: "queued", Objective: "Fix the cache\n\nCount hits.", Repository: "sf/r", SHA: "abc123", IssueNumber: 5,
		Author: "sebastian", AuthorWriter: true, Source: "webhook", Track: policy.TrackDirect, RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 2}
}

// sizingRig runs card under a test policy of default budget 2 and task
// maximum 5; the sizing task it starts ends status with answer.
func sizingRig(t *testing.T, card Task, status, answer string) (*Runner, *fakeStore, *fakeObjectStore, *fakeWorkflows) {
	t.Helper()
	objects := &fakeObjectStore{}
	r, _, store := newTestRunner(t, card, newFakeKube(), objects)
	wf := &fakeWorkflows{}
	wf.onStart = func(id string) {
		child := store.tasks[id]
		child.Status, child.AgentSummary = status, answer
		store.tasks[id] = child
	}
	r.cfg.Workflows = wf
	return r, store, objects, wf
}

func sizeCard(t *testing.T, r *Runner) SizeResult {
	t.Helper()
	val, err := execActivity(t, r, ActivitySizeCard, "t1")
	return getInto[SizeResult](t, val, err)
}

// sizingSteps are t1's sizing steps, oldest first, without their time,
// name and gate start.
func sizingSteps(t *testing.T, objects *fakeObjectStore) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(objGet(objects, "sessions/t1/timeline.jsonl"), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(line, &m) == nil && m["step"] == StepSizing {
			delete(m, "time")
			delete(m, "step")
			delete(m, "started")
			out = append(out, m)
		}
	}
	return out
}

func wantSizingStep(t *testing.T, objects *fakeObjectStore, want map[string]any) {
	t.Helper()
	if got := sizingSteps(t, objects); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("sizing steps = %v, want one %v", got, want)
	}
}

func TestSizeCardKeepsASmallCardDirect(t *testing.T) {
	r, store, objects, wf := sizingRig(t, sizingCard(), "done",
		`{"size": "small", "reason": "one handler and its test", "files": ["internal/x/h.go", "internal/x/h_test.go"]}`)
	if res := sizeCard(t, r); res != (SizeResult{}) {
		t.Fatalf("result = %+v, want the card to stay direct", res)
	}
	if card := store.tasks["t1"]; card.Size != "small" || card.BudgetEUR != 2 {
		t.Fatalf("card size %q, budget %v, want small and 2", card.Size, card.BudgetEUR)
	}
	if len(wf.started) != 1 {
		t.Fatalf("started %v, want one sizing task", wf.started)
	}
	child := store.tasks[wf.started[0]]
	if child.Parent != "t1" || child.Source != SourceSizing || !slices.Equal(child.RequiredCapabilities, []string{policy.ClassAnalyzer}) || child.SHA != "abc123" {
		t.Fatalf("sizing task = %+v", child)
	}
	wantSizingStep(t, objects, map[string]any{"size": "small", "filed": "", "sized": "small", "reason": "one handler and its test",
		"files": []any{"internal/x/h.go", "internal/x/h_test.go"}, "report": child.ID, "plan": false})
	if step := timelineStep(t, objects, "t1", StepSizing); step["started"] == nil {
		t.Fatalf("sizing step %v, want the gate's start", step)
	}
}

func TestSizeCardSendsAMediumCardToThePlanner(t *testing.T) {
	r, store, objects, wf := sizingRig(t, sizingCard(), "done",
		`{"size": "medium", "reason": "touches the scheduler and the store", "files": ["a.go", "b.go"]}`)
	res := sizeCard(t, r)
	want := PlanSwitch{Reason: "medium, not small: touches the scheduler and the store",
		Work: "The sizing step expects the change to touch these files:\n- a.go\n- b.go"}
	if res.ToPlan == nil || *res.ToPlan != want {
		t.Fatalf("to plan = %+v, want %+v", res.ToPlan, want)
	}
	if card := store.tasks["t1"]; card.Size != "medium" || card.BudgetEUR != 5 {
		t.Fatalf("card size %q, budget %v, want medium and 5", card.Size, card.BudgetEUR)
	}
	wantSizingStep(t, objects, map[string]any{"size": "medium", "filed": "", "sized": "medium", "reason": "touches the scheduler and the store",
		"files": []any{"a.go", "b.go"}, "report": wf.started[0], "plan": true, "budget_eur": 5.0})
}

// A card that holds its filing's budget gets its size's; a budget it was
// funded to or asked for stays.
func TestSizeCardBudget(t *testing.T) {
	const medium = `{"size": "medium", "reason": "r", "files": []}`
	for name, tc := range map[string]struct {
		held      float64
		objective string
		answer    string
		want      float64
	}{
		"the filing's default": {2, "Fix the cache\n\nCount hits.", medium, 5},
		"funded":               {4, "Fix the cache\n\nCount hits.", medium, 4},
		"asked for":            {3, "Fix the cache\n\nCount hits.\nBudget: 3 EUR", medium, 3},
		"small":                {2, "Fix the cache\n\nCount hits.", `{"size": "small", "reason": "r"}`, 2},
	} {
		t.Run(name, func(t *testing.T) {
			card := sizingCard()
			card.BudgetEUR, card.Objective = tc.held, tc.objective
			r, store, objects, _ := sizingRig(t, card, "done", tc.answer)
			sizeCard(t, r)
			if got := store.tasks["t1"].BudgetEUR; got != tc.want {
				t.Fatalf("budget = %v, want %v", got, tc.want)
			}
			steps := sizingSteps(t, objects)
			if len(steps) != 1 {
				t.Fatalf("sizing steps = %v", steps)
			}
			got, recorded := steps[0]["budget_eur"]
			if changed := tc.want != tc.held; recorded != changed || changed && got != tc.want {
				t.Fatalf("step budget_eur = %v (recorded %v), want %v recorded only when it changed", got, recorded, tc.want)
			}
		})
	}
}

func TestSizeCardTakesTheFilersSizeAsAFloor(t *testing.T) {
	card := sizingCard()
	card.Size = "large"
	r, _, objects, wf := sizingRig(t, card, "done", `{"size": "small", "reason": "r"}`)
	res := sizeCard(t, r)
	if len(wf.started) != 0 {
		t.Fatalf("started %v, want no sizing session for a filer's large", wf.started)
	}
	if want := (PlanSwitch{Reason: "large, not small: its filer sized it large"}); res.ToPlan == nil || *res.ToPlan != want {
		t.Fatalf("to plan = %+v, want %+v", res.ToPlan, want)
	}
	wantSizingStep(t, objects, map[string]any{"size": "large", "filed": "large", "reason": "its filer sized it large", "plan": true})

	card.Size = "small"
	r, store, _, wf := sizingRig(t, card, "done", `{"size": "medium", "reason": "two packages", "files": ["a.go"]}`)
	if res := sizeCard(t, r); len(wf.started) != 1 || res.ToPlan == nil {
		t.Fatalf("a filer's small: started %v, result %+v, want one sizing and the switch", wf.started, res)
	}
	if got := store.tasks["t1"].Size; got != "medium" {
		t.Fatalf("size = %q, want medium", got)
	}
}

func TestSizeCardWithoutAVerdictKeepsTheCardAsFiled(t *testing.T) {
	// reason "": the failed sizing task's own line
	for name, tc := range map[string]struct{ status, answer, reason string }{
		"failed":  {"failed", "", ""},
		"no size": {"done", "it is big", "the sizing's answer is no size: it is big"},
	} {
		t.Run(name, func(t *testing.T) {
			r, store, objects, wf := sizingRig(t, sizingCard(), tc.status, tc.answer)
			if res := sizeCard(t, r); res != (SizeResult{}) || len(wf.started) != 1 {
				t.Fatalf("result = %+v, started %v, want one sizing and the card to stay direct", res, wf.started)
			}
			if card := store.tasks["t1"]; card.Size != "" || card.BudgetEUR != 2 {
				t.Fatalf("card size %q, budget %v, want as filed", card.Size, card.BudgetEUR)
			}
			reason := cmp.Or(tc.reason, "the sizing task "+wf.started[0]+" ended failed")
			wantSizingStep(t, objects, map[string]any{"size": "", "filed": "", "reason": reason, "report": wf.started[0], "plan": false})
		})
	}
}

func TestSizeCardSkipsACardThatCarriesAPullRequest(t *testing.T) {
	card := sizingCard()
	card.CarryPR = 9
	r, _, objects, wf := sizingRig(t, card, "done", `{"size": "medium", "reason": "r"}`)
	if res := sizeCard(t, r); res != (SizeResult{}) || len(wf.started) != 0 {
		t.Fatalf("result %+v, started %v, want no sizing", res, wf.started)
	}
	wantSizingStep(t, objects, map[string]any{"skipped": "the card carries pull request #9, whose work exists"})
}

func TestSizeCardIsRetrySafe(t *testing.T) {
	r, store, objects, _ := sizingRig(t, sizingCard(), "done", `{"size": "medium", "reason": "two packages", "files": ["a.go"]}`)
	first, again := sizeCard(t, r), sizeCard(t, r)
	if first.ToPlan == nil || again.ToPlan == nil || *first.ToPlan != *again.ToPlan {
		t.Fatalf("first %+v, again %+v, want the same switch", first.ToPlan, again.ToPlan)
	}
	n := 0
	for _, task := range store.tasks {
		if task.Parent == "t1" {
			n++
		}
	}
	if steps := sizingSteps(t, objects); n != 1 || len(steps) != 1 {
		t.Fatalf("%d sizing tasks, steps %v, want one of each", n, steps)
	}

	r, _, objects, wf := sizingRig(t, sizingCard(), "done", `{"size": "medium", "reason": "r"}`)
	if err := r.timeline.Append(context.Background(), "t1", StepSizing, map[string]any{"size": "small", "reason": "one file", "plan": false}); err != nil {
		t.Fatal(err)
	}
	if res := sizeCard(t, r); res != (SizeResult{}) || len(wf.started) != 0 || len(sizingSteps(t, objects)) != 1 {
		t.Fatalf("a card sized small: result %+v, started %v, steps %v", res, wf.started, sizingSteps(t, objects))
	}
}
