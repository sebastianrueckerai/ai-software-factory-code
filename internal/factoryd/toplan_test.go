package factoryd

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// directCard is t1, a running direct card of issue 7 on sf/repo: its base
// b0, two sessions spent.
func directCard() Task {
	return Task{ID: "t1", Kind: "agent", Status: "running", Track: policy.TrackDirect, Source: "webhook", Repository: "sf/repo",
		SHA: "b0", IssueNumber: 7, RequiredCapabilities: []string{policy.ClassCoder}, Usage: TaskUsage{Attempts: 2}}
}

// becomePlanRig runs card in a project of process spec with tracks; the
// fork's branch is at c7 and issue 7 carries agent and state/running.
func becomePlanRig(t *testing.T, card Task, tracks policy.Tracks) (*Runner, *fakeStore, *fakeObjectStore, *prForgejo) {
	t.Helper()
	fj := &prForgejo{head: "c7", labels: map[int64][]string{7: {LabelAgent, "state/running"}}}
	r, store, objects := newActivityRunner(t, card, fj)
	p := seedProject("sf/repo")
	p.Process, p.Tracks = "spec", tracks
	store.projects = []Project{p}
	return r, store, objects, fj
}

func becomePlan(t *testing.T, r *Runner, sw PlanSwitch) PlanSwitchResult {
	t.Helper()
	val, err := execActivity(t, r, ActivityBecomePlan, "t1", sw)
	return getInto[PlanSwitchResult](t, val, err)
}

// toPlanSteps are t1's to_plan steps, oldest first.
func toPlanSteps(t *testing.T, objects *fakeObjectStore) []map[string]any {
	t.Helper()
	b, _ := objects.Get(context.Background(), "sessions/t1/timeline.jsonl")
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["step"] == "to_plan" {
			out = append(out, m)
		}
	}
	return out
}

// labelWrites counts the label replacements of issue 7.
func labelWrites(fj *prForgejo) int {
	fj.mu.Lock()
	defer fj.mu.Unlock()
	n := 0
	for _, c := range fj.calls {
		if c == "PUT /api/v1/repos/sf/repo/issues/7/labels" {
			n++
		}
	}
	return n
}

// A running direct card turns plan-track: the planner's class, its
// session counters back to zero, its issue labelled plan in place of
// agent, and a to_plan step holding the planner's work-so-far block.
func TestBecomePlanTurnsADirectCard(t *testing.T) {
	r, store, objects, fj := becomePlanRig(t, directCard(), specTracks)
	if got := becomePlan(t, r, PlanSwitch{Reason: "too big", Work: "split by the check-in"}); got != (PlanSwitchResult{Switched: true}) {
		t.Fatalf("BecomePlan = %+v, want switched", got)
	}
	card := store.tasks["t1"]
	if card.Track != policy.TrackPlan || !slices.Equal(card.RequiredCapabilities, []string{"planner"}) || card.Usage != (TaskUsage{}) {
		t.Fatalf("card track %q, capabilities %q, usage %+v; want plan, [planner], zero", card.Track, card.RequiredCapabilities, card.Usage)
	}
	if got := fj.labels[7]; !slices.Equal(got, []string{LabelPlan, "state/running"}) {
		t.Fatalf("issue labels %q, want [plan state/running]", got)
	}
	steps := toPlanSteps(t, objects)
	if len(steps) != 1 || steps[0]["reason"] != "too big" ||
		steps[0]["work_so_far"] != "Branch head: c7\nsplit by the check-in\nReason for the switch: too big" {
		t.Fatalf("to_plan steps %v", steps)
	}
}

// A card whose branch holds no commit past its base says so in the block.
func TestBecomePlanWithoutCommits(t *testing.T) {
	card := directCard()
	card.SHA = "c7"
	r, _, objects, _ := becomePlanRig(t, card, specTracks)
	if got := becomePlan(t, r, PlanSwitch{Reason: "too big", Work: "w"}); !got.Switched {
		t.Fatalf("BecomePlan = %+v, want switched", got)
	}
	if steps := toPlanSteps(t, objects); len(steps) != 1 || steps[0]["work_so_far"] != "No commit past the base.\nw\nReason for the switch: too big" {
		t.Fatalf("to_plan steps %v", steps)
	}
}

// A retry after the track moved answers switched again and writes
// nothing: one step, one label write.
func TestBecomePlanIsRetrySafe(t *testing.T) {
	r, _, objects, fj := becomePlanRig(t, directCard(), specTracks)
	sw := PlanSwitch{Reason: "too big", Work: "split by the check-in"}
	for i := range 2 {
		if got := becomePlan(t, r, sw); got != (PlanSwitchResult{Switched: true}) {
			t.Fatalf("call %d: BecomePlan = %+v, want switched", i+1, got)
		}
	}
	if steps, writes := len(toPlanSteps(t, objects)), labelWrites(fj); steps != 1 || writes != 1 {
		t.Fatalf("%d to_plan steps, %d label writes; want 1 and 1", steps, writes)
	}
}

// A label write that fails after the row moved fails the attempt; the
// retry finishes the labels and records no second step.
func TestBecomePlanCompletesAfterAFailedLabelWrite(t *testing.T) {
	r, store, objects, fj := becomePlanRig(t, directCard(), specTracks)
	fj.labelFail = 1
	sw := PlanSwitch{Reason: "too big", Work: "split by the check-in"}
	if _, err := execActivity(t, r, ActivityBecomePlan, "t1", sw); err == nil {
		t.Fatal("the first attempt succeeded, want the label write's failure")
	}
	if card := store.tasks["t1"]; card.Track != policy.TrackPlan || len(toPlanSteps(t, objects)) != 1 ||
		!slices.Equal(fj.labels[7], []string{LabelAgent, "state/running"}) {
		t.Fatalf("after the failed attempt: track %q, steps %v, labels %q; want plan, one step, the labels as they were", card.Track, toPlanSteps(t, objects), fj.labels[7])
	}
	if got := becomePlan(t, r, sw); got != (PlanSwitchResult{Switched: true}) {
		t.Fatalf("the retry: BecomePlan = %+v, want switched", got)
	}
	if steps, writes := len(toPlanSteps(t, objects)), labelWrites(fj); steps != 1 || writes != 2 ||
		!slices.Equal(fj.labels[7], []string{LabelPlan, "state/running"}) || store.tasks["t1"].Usage != (TaskUsage{}) {
		t.Fatalf("%d to_plan steps, %d label writes, labels %q, usage %+v; want 1, 2 (one refused), [plan state/running], zero",
			steps, writes, fj.labels[7], store.tasks["t1"].Usage)
	}
}

// A card found plan-track is one an earlier attempt moved before it
// stopped: the attempt records the missing step, zeroes the counters and
// relabels the issue.
func TestBecomePlanCompletesAnEarlierAttempt(t *testing.T) {
	card := directCard()
	card.Track, card.RequiredCapabilities = policy.TrackPlan, []string{"planner"}
	r, store, objects, fj := becomePlanRig(t, card, specTracks)
	if got := becomePlan(t, r, PlanSwitch{Reason: "too big", Work: "split by the check-in"}); got != (PlanSwitchResult{Switched: true}) {
		t.Fatalf("BecomePlan = %+v, want switched", got)
	}
	steps := toPlanSteps(t, objects)
	if len(steps) != 1 || steps[0]["work_so_far"] != "Branch head: c7\nsplit by the check-in\nReason for the switch: too big" ||
		store.tasks["t1"].Usage != (TaskUsage{}) || !slices.Equal(fj.labels[7], []string{LabelPlan, "state/running"}) {
		t.Fatalf("to_plan steps %v, usage %+v, labels %q", steps, store.tasks["t1"].Usage, fj.labels[7])
	}
}

// A card that may not turn plan-track is refused with its reason and left
// exactly as it was.
func TestBecomePlanRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*Task)
		tracks policy.Tracks
		want   string
	}{
		"open pull request": {change: func(c *Task) { c.PRNumber = 12 }, tracks: specTracks,
			want: "Card #7 has an open pull request, so its work cannot go back to the planner: close the pull request first."},
		"a plan's child":     {change: func(c *Task) { c.Source, c.Parent, c.PlanKey = SourcePlan, "t-plan", "api" }, tracks: specTracks},
		"a roll":             {change: func(c *Task) { c.Source = SourceRoll }, tracks: specTracks},
		"a human card":       {change: func(c *Task) { c.Kind, c.Status, c.Track, c.RequiredCapabilities = "human", "waiting-human", "", nil }, tracks: specTracks},
		"waiting on a human": {change: func(c *Task) { c.Status = "waiting-human" }, tracks: specTracks},
		"another track": {change: func(c *Task) { c.Track = "lane" }, tracks: specTracks,
			want: `Card #7 is on track "lane", not the direct track, so its work cannot go back to the planner: file its work as a direct card first.`},
		"no plan track": {change: func(*Task) {}, tracks: maintenanceTracks},
	} {
		t.Run(name, func(t *testing.T) {
			card := directCard()
			tc.change(&card)
			r, store, objects, fj := becomePlanRig(t, card, tc.tracks)
			got := becomePlan(t, r, PlanSwitch{Reason: "too big", Work: "w"})
			if got.Switched || got.Refused == "" || !strings.Contains(got.Refused, "#7") || (tc.want != "" && got.Refused != tc.want) {
				t.Fatalf("BecomePlan = %+v, want refused naming #7 (%q)", got, tc.want)
			}
			untouched(t, store, objects, fj, card)
		})
	}
}

// untouched fails unless t1 is card as it was: its track, class and
// usage, no to_plan step, no label write.
func untouched(t *testing.T, store *fakeStore, objects *fakeObjectStore, fj *prForgejo, card Task) {
	t.Helper()
	after := store.tasks["t1"]
	if after.Track != card.Track || !slices.Equal(after.RequiredCapabilities, card.RequiredCapabilities) || after.Usage != card.Usage {
		t.Fatalf("card changed: %+v", after)
	}
	if steps, writes := len(toPlanSteps(t, objects)), labelWrites(fj); steps != 0 || writes != 0 ||
		!slices.Equal(fj.labels[7], []string{LabelAgent, "state/running"}) {
		t.Fatalf("%d to_plan steps, %d label writes, labels %q; want none and the labels as they were", steps, writes, fj.labels[7])
	}
}

// racingStore changes the card just before its row moves, as a write
// racing the switch would.
type racingStore struct {
	*fakeStore
	change func(*Task)
}

func (s racingStore) SwitchToPlan(ctx context.Context, id string, capabilities []string) (bool, error) {
	t := s.tasks[id]
	s.change(&t)
	s.tasks[id] = t
	return s.fakeStore.SwitchToPlan(ctx, id, capabilities)
}

// A row that moved on before the switch is refused by what moved it, with
// nothing written: a card closed meanwhile by its status, one that got a
// pull request meanwhile by the pull request.
func TestBecomePlanAfterARace(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*Task)
		want   string
	}{
		"closed meanwhile": {func(c *Task) { c.Status = "closed" },
			"Card #7 is closed, so its work cannot go back to the planner: switch it while it is queued, waiting on its dependencies or running."},
		"a pull request meanwhile": {func(c *Task) { c.PRNumber = 12 },
			"Card #7 has an open pull request, so its work cannot go back to the planner: close the pull request first."},
	} {
		t.Run(name, func(t *testing.T) {
			r, store, objects, fj := becomePlanRig(t, directCard(), specTracks)
			r.store = racingStore{store, tc.change}
			if got := becomePlan(t, r, PlanSwitch{Reason: "too big", Work: "w"}); got != (PlanSwitchResult{Refused: tc.want}) {
				t.Fatalf("BecomePlan = %+v, want refused %q", got, tc.want)
			}
			untouched(t, store, objects, fj, directCard())
		})
	}
}

// One an earlier attempt switched meanwhile is switched, and this attempt
// completes it.
func TestBecomePlanAfterAnEarlierAttemptSwitched(t *testing.T) {
	r, store, objects, fj := becomePlanRig(t, directCard(), specTracks)
	r.store = racingStore{store, func(c *Task) { c.Track = policy.TrackPlan }}
	if got := becomePlan(t, r, PlanSwitch{Reason: "too big", Work: "w"}); got != (PlanSwitchResult{Switched: true}) {
		t.Fatalf("BecomePlan = %+v, want switched", got)
	}
	if steps := toPlanSteps(t, objects); len(steps) != 1 || store.tasks["t1"].Usage != (TaskUsage{}) ||
		!slices.Equal(fj.labels[7], []string{LabelPlan, "state/running"}) {
		t.Fatalf("to_plan steps %v, usage %+v, labels %q", steps, store.tasks["t1"].Usage, fj.labels[7])
	}
}
