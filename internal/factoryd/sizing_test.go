package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// sizingCard is t1, a writer's queued direct card of issue 5 with the
// default budget of 5 EUR, unsized.
func sizingCard() Task {
	return Task{ID: "t1", Status: "queued", Objective: "Fix the cache\n\nCount hits.", Repository: "sf/r", SHA: "abc123", IssueNumber: 5,
		Author: "sebastian", AuthorWriter: true, Source: "webhook", Track: policy.TrackDirect, RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 5}
}

// sizingRig runs card under a test policy of default budget 5 and task
// maximum 15; the sizing task it starts ends status with answer.
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
	v := r.cfg.Policy()
	v.Document.Budget.TaskDefaultEUR, v.Document.Budget.TaskMaxEUR = 5, 15
	r.cfg.Policy = func() policy.Version { return v }
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
	if card := store.tasks["t1"]; card.Size != "small" || card.BudgetEUR != 4 {
		t.Fatalf("card size %q, budget %v, want small and 4", card.Size, card.BudgetEUR)
	}
	if len(wf.started) != 1 {
		t.Fatalf("started %v, want one sizing task", wf.started)
	}
	child := store.tasks[wf.started[0]]
	if child.Parent != "t1" || child.Source != SourceSizing || !slices.Equal(child.RequiredCapabilities, []string{policy.ClassAnalyzer}) || child.SHA != "abc123" {
		t.Fatalf("sizing task = %+v", child)
	}
	wantSizingStep(t, objects, map[string]any{"size": "small", "filed": "", "sized": "small", "reason": "one handler and its test",
		"files": []any{"internal/x/h.go", "internal/x/h_test.go"}, "report": child.ID, "plan": false, "held_cents": 500.0, "budget_eur": 4.0})
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
		"files": []any{"a.go", "b.go"}, "report": wf.started[0], "plan": true, "held_cents": 500.0, "budget_eur": 5.0})
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

// An outsider's filed size is no floor: the session sizes the card. Filed
// large on the frontier rung it holds 10 EUR, its filing's; sized small it
// goes to a strong card's 5.
func TestSizeCardSizesAnOutsidersCard(t *testing.T) {
	card := sizingCard()
	card.Author, card.AuthorWriter, card.Size, card.Rung, card.BudgetEUR = "mallory", false, "large", RungFrontier, 10
	r, store, objects, wf := sizingRig(t, card, "done", `{"size": "small", "reason": "one handler", "files": ["h.go"]}`)
	if res := sizeCard(t, r); res != (SizeResult{}) || len(wf.started) != 1 {
		t.Fatalf("result %+v, started %v, want one sizing and the card to stay direct", res, wf.started)
	}
	if got := store.tasks["t1"]; got.Size != "small" || got.BudgetEUR != 5 {
		t.Fatalf("size %q, budget %v, want small and 5", got.Size, got.BudgetEUR)
	}
	wantSizingStep(t, objects, map[string]any{"size": "small", "filed": "large", "sized": "small", "reason": "one handler",
		"files": []any{"h.go"}, "report": wf.started[0], "plan": false, "held_cents": 1000.0, "budget_eur": 5.0})
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
			if card := store.tasks["t1"]; card.Size != "" || card.BudgetEUR != 5 {
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
	first := sizeCard(t, r)
	// the size is stored: a call that finds the step writes nothing more
	again := sizeCard(t, r)
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

// flakyObjects fails its next fails puts.
type flakyObjects struct {
	*fakeObjectStore
	fails int
}

func (s *flakyObjects) Put(ctx context.Context, key string, body []byte) error {
	if s.fails > 0 {
		s.fails--
		return errors.New("the object store is down")
	}
	return s.fakeObjectStore.Put(ctx, key, body)
}

// The step is recorded before the size write: a retry after a failed write
// completes it from the step, the sizing's files kept.
func TestSizeCardCompletesAFailedWriteOnTheRetry(t *testing.T) {
	r, store, objects, wf := sizingRig(t, sizingCard(), "done", `{"size": "medium", "reason": "two packages", "files": ["a.go"]}`)
	store.sizeFails = 1
	if _, err := execActivity(t, r, ActivitySizeCard, "t1"); err == nil {
		t.Fatal("a sizing whose size write failed succeeded")
	}
	if card, steps := store.tasks["t1"], sizingSteps(t, objects); card.Size != "" || card.BudgetEUR != 5 || len(steps) != 1 {
		t.Fatalf("after the failed write: size %q, budget %v, steps %v, want as filed and the step", card.Size, card.BudgetEUR, steps)
	}
	res := sizeCard(t, r)
	want := PlanSwitch{Reason: "medium, not small: two packages", Work: "The sizing step expects the change to touch these files:\n- a.go"}
	if res.ToPlan == nil || *res.ToPlan != want {
		t.Fatalf("to plan = %+v, want %+v", res.ToPlan, want)
	}
	if card := store.tasks["t1"]; card.Size != "medium" || card.BudgetEUR != 5 {
		t.Fatalf("card size %q, budget %v, want medium and 5", card.Size, card.BudgetEUR)
	}
	if steps := sizingSteps(t, objects); len(wf.started) != 1 || len(steps) != 1 {
		t.Fatalf("started %v, steps %v, want one of each", wf.started, steps)
	}
}

// A record that fails writes nothing: the retry sizes against the same
// sizing task.
func TestSizeCardRetriesAFailedRecord(t *testing.T) {
	r, store, objects, wf := sizingRig(t, sizingCard(), "done", `{"size": "medium", "reason": "two packages", "files": ["a.go"]}`)
	flaky := &flakyObjects{fakeObjectStore: objects, fails: 1}
	r.objects, r.timeline.Objects = flaky, flaky
	if _, err := execActivity(t, r, ActivitySizeCard, "t1"); err == nil {
		t.Fatal("a sizing whose record failed succeeded")
	}
	if card, steps := store.tasks["t1"], sizingSteps(t, objects); card.Size != "" || card.BudgetEUR != 5 || len(steps) != 0 {
		t.Fatalf("after the failed record: size %q, budget %v, steps %v, want nothing written", card.Size, card.BudgetEUR, steps)
	}
	res := sizeCard(t, r)
	want := PlanSwitch{Reason: "medium, not small: two packages", Work: "The sizing step expects the change to touch these files:\n- a.go"}
	if res.ToPlan == nil || *res.ToPlan != want {
		t.Fatalf("to plan = %+v, want %+v", res.ToPlan, want)
	}
	if card := store.tasks["t1"]; card.Size != "medium" || card.BudgetEUR != 5 {
		t.Fatalf("card size %q, budget %v, want medium and 5", card.Size, card.BudgetEUR)
	}
	if steps := sizingSteps(t, objects); len(wf.started) != 1 || len(steps) != 1 {
		t.Fatalf("started %v, steps %v, want one sizing task and one step", wf.started, steps)
	}
}

// sizedBudget sizes t1 (filed as card) to size and answers the card.
func sizedBudget(t *testing.T, card Task, size string) Task {
	t.Helper()
	r, store, _, _ := sizingRig(t, card, "done", `{"size": "`+size+`", "reason": "r"}`)
	sizeCard(t, r)
	return store.tasks["t1"]
}

// The default 5 EUR card is the filing's budget: the sizing gives it its
// size's budget, small 4, medium 5, large 10.
func TestSizeCardSetsTheBudgetOfAnUnsizedCard(t *testing.T) {
	for size, want := range map[string]float64{"small": 4, "medium": 5, "large": 10} {
		if got := sizedBudget(t, sizingCard(), size); got.Size != size || got.BudgetEUR != want {
			t.Errorf("sized %s: size %q, budget %v, want %v", size, got.Size, got.BudgetEUR, want)
		}
	}
}

// A budget that is not the filing's 5 (a Budget: line, a fund) keeps its value.
func TestSizeCardKeepsARaisedBudget(t *testing.T) {
	card := sizingCard()
	card.BudgetEUR = 7
	if got := sizedBudget(t, card, "small"); got.Size != "small" || got.BudgetEUR != 7 {
		t.Fatalf("card size %q, budget %v, want small and the raised 7", got.Size, got.BudgetEUR)
	}
}

// A strong card holds at least a medium card's budget: unsized 5, small
// stays 5, large goes to 10.
func TestSizeCardOfAStrongCard(t *testing.T) {
	for size, want := range map[string]float64{"small": 5, "large": 10} {
		card := sizingCard()
		card.Rung = RungFrontier
		if got := sizedBudget(t, card, size); got.BudgetEUR != want {
			t.Errorf("sized %s: budget %v, want %v", size, got.BudgetEUR, want)
		}
	}
}

// A project whose card maximum is 3 files its cards at 3, whatever their size.
func TestSizeCardUnderAProjectMaximum(t *testing.T) {
	for _, size := range []string{"small", "medium", "large"} {
		card := sizingCard()
		card.BudgetEUR = 3
		r, store, _, _ := sizingRig(t, card, "done", `{"size": "`+size+`", "reason": "r"}`)
		store.projects = []Project{{Name: "r", Repository: "sf/r", Budget: &policy.ProjectBudget{MonthEUR: 50, TaskMaxEUR: 3}}}
		sizeCard(t, r)
		if got := store.tasks["t1"]; got.Size != size || got.BudgetEUR != 3 {
			t.Errorf("sized %s: size %q, budget %v, want 3", size, got.Size, got.BudgetEUR)
		}
	}
}

// A Budget: line is a person's own amount, even where it equals the
// filing's budget: no size changes it.
func TestSizeCardKeepsAskedBudgetEqualToTheDefault(t *testing.T) {
	for _, line := range []string{"Budget: 5", "Budget: 5 EUR"} {
		size := map[string]string{"Budget: 5": "small", "Budget: 5 EUR": "large"}[line]
		card := sizingCard()
		card.Objective += "\n\n" + line
		if got := sizedBudget(t, card, size); got.Size != size || got.BudgetEUR != 5 {
			t.Errorf("sized %s: size %q, budget %v, want the asked 5", size, got.Size, got.BudgetEUR)
		}
	}
}

// A person raises the budget after the write, onto the held cents; a new run leaves it.
func TestSizeCardKeepsARaiseOfANewRun(t *testing.T) {
	r, store, _, _ := sizingRig(t, sizingCard(), "done", `{"size": "small", "reason": "r"}`)
	sizeCard(t, r)
	card := store.tasks["t1"]
	card.BudgetEUR = 5
	store.tasks["t1"] = card
	sizeCard(t, r)
	if got := store.tasks["t1"]; got.Size != "small" || got.BudgetEUR != 5 {
		t.Fatalf("card size %q, budget %v, want small and the raise 5 kept", got.Size, got.BudgetEUR)
	}
}

// A raise between the record and the write keeps its value.
func TestSizeCardKeepsARaiseBeforeTheWrite(t *testing.T) {
	r, store, _, _ := sizingRig(t, sizingCard(), "done", `{"size": "small", "reason": "r"}`)
	store.sizeFails = 1
	if _, err := execActivity(t, r, ActivitySizeCard, "t1"); err == nil {
		t.Fatal("a sizing whose write failed succeeded")
	}
	card := store.tasks["t1"]
	card.BudgetEUR = 7
	store.tasks["t1"] = card
	sizeCard(t, r)
	if got := store.tasks["t1"]; got.Size != "small" || got.BudgetEUR != 7 {
		t.Fatalf("card size %q, budget %v, want small and the raise 7", got.Size, got.BudgetEUR)
	}
}

// A card filed Size: small holds 4 EUR at filing: sized small, it changes
// nothing and the step holds no write.
func TestSizeCardWritesNothingForTheFiledSize(t *testing.T) {
	card := sizingCard()
	card.Size, card.BudgetEUR = "small", 4
	r, store, objects, _ := sizingRig(t, card, "done", `{"size": "small", "reason": "r"}`)
	store.sizeFails = 1
	sizeCard(t, r)
	if got := store.tasks["t1"]; got.Size != "small" || got.BudgetEUR != 4 || store.sizeFails != 1 {
		t.Fatalf("card size %q, budget %v, write attempts left %d, want small, 4 and no write", got.Size, got.BudgetEUR, store.sizeFails)
	}
	if step := sizingSteps(t, objects)[0]; step["held_cents"] != nil {
		t.Fatalf("step %v, want no held_cents", step)
	}
}
