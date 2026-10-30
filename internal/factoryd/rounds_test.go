package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// planCardTask is plan t-plan's card of entry c (issue n) in status, merged
// as merge when merge is set, run against revision 1.
func planCardTask(c PlanCard, n int64, status, merge string) Task {
	return Task{ID: "t-" + c.Key, PlanKey: c.Key, IssueNumber: n, Status: status, MergeCommit: merge, PlanRevision: 1, BudgetEUR: 2,
		Objective: c.Title + "\n\n" + c.Brief, Kind: "agent", Track: policy.TrackDirect, Source: SourcePlan, Parent: "t-plan",
		Repository: "linkshort/linkshort", SHA: "abc123"}
}

func TestSettledCards(t *testing.T) {
	prev := roundOne()
	cards := []Task{planCardTask(prev.Cards[0], 10, "done", "m10"), planCardTask(prev.Cards[1], 11, "done", "m11"), planCardTask(prev.Cards[2], 12, "closed", "")}
	keys := func(cs []PlanCard) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Key)
		}
		return out
	}
	if got := settledCards(prev, cards, 2); !reflect.DeepEqual(got, prev.Cards[:2]) {
		t.Fatalf("round 2: %q, want the foundation and links-api entries", keys(got))
	}
	reverted := slices.Clone(cards)
	at := time.Now()
	reverted[1].RevertedAt = &at
	if got := settledCards(prev, reverted, 2); !reflect.DeepEqual(got, prev.Cards[:1]) {
		t.Fatalf("links-api taken off main: %q, want the foundation's alone", keys(got))
	}
	if got := settledCards(prev, cards, 1); len(got) != 0 {
		t.Fatalf("round 1: %q, want none", keys(got))
	}

	whole := testPlan()
	whole.Round = 1
	var done []Task
	for i := range whole.Cards {
		whole.Cards[i].Round = 1
		done = append(done, planCardTask(whole.Cards[i], int64(10+i), "done", "m"))
	}
	if got := keys(settledCards(whole, done, 2)); !slices.Equal(got, []string{"foundation", "links-api", "redirects"}) {
		t.Fatalf("a whole round done: %q, want foundation, links-api and redirects, no integration", got)
	}
}

// filePlanRig is a runner over running plan t-plan under the fixture
// version, in force and pinned as 7.
func filePlanRig(t *testing.T) (*Runner, *fakeStore, *fakeKube) {
	t.Helper()
	kube := newFakeKube()
	r, _, store := newTestRunner(t, recordPlanTask(), kube, &fakeObjectStore{})
	v := fixtureVersion(t)
	r.cfg.Policy = func() policy.Version { return v }
	r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return v, id == 7, nil }
	return r, store, kube
}

// roundTwoRig is filePlanRig with revision 1 roundOne(), adjusted by
// edit when not nil, its three cards done with merge commits, extra
// cards beside them, and the pending replan step of round 2.
func roundTwoRig(t *testing.T, edit func(*Plan), extra ...Task) (*Runner, *fakeStore, *fakeKube, Plan) {
	t.Helper()
	ctx := context.Background()
	r, store, kube := filePlanRig(t)
	one := roundOne()
	if edit != nil {
		edit(&one)
	}
	if _, err := store.AddPlanRevision(ctx, "t-plan", json.RawMessage(planJSON(t, one)), "sha-1"); err != nil {
		t.Fatal(err)
	}
	for i, c := range one.Cards {
		extra = append(extra, planCardTask(c, int64(10+i), "done", "m"+c.Key))
	}
	for _, k := range extra {
		if err := store.CreateTask(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.timeline.Append(ctx, "t-plan", stepReplan, map[string]any{"origin": ReplanRound, "revision": 1, "round": 2}); err != nil {
		t.Fatal(err)
	}
	return r, store, kube, one
}

// roundTwoAnswer is the planner's answer for twoRounds' second round:
// its spec, summary and criteria and the cards stats and integration,
// with no round field and no outline.
func roundTwoAnswer() Plan {
	two := twoRounds()
	cards := slices.Clone(two.Cards[3:])
	for i := range cards {
		cards[i].Round = 0
	}
	return Plan{Spec: two.Spec, Summary: two.Summary, Criteria: two.Criteria, Cards: cards}
}

// storedPlan is t-plan's revision n decoded.
func storedPlan(t *testing.T, store *fakeStore, n int) Plan {
	t.Helper()
	rev, ok, err := store.PlanRevision(context.Background(), "t-plan", n)
	if err != nil || !ok {
		t.Fatalf("revision %d: %v, %v", n, ok, err)
	}
	var p Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A plan's first filing is round 1, on the plan and every card, whatever
// round a card's entry names.
func TestFilePlanStampsTheFirstRound(t *testing.T) {
	r, store, _ := filePlanRig(t)
	answer := testPlan()
	answer.Cards[1].Round = 7
	res, err := r.FilePlan(context.Background(), "t-plan", planJSON(t, answer))
	if err != nil || res.Revision != 1 || len(res.Findings) != 0 {
		t.Fatalf("filed = %+v, %v; want revision 1", res, err)
	}
	p := storedPlan(t, store, 1)
	if p.Round != 1 {
		t.Fatalf("the plan's round %d, want 1", p.Round)
	}
	for _, c := range p.Cards {
		if c.Round != 1 {
			t.Fatalf("card %s: round %d, want 1", c.Key, c.Round)
		}
	}
}

// The second round files on top of the first: the planner answers its
// new cards alone, and factoryd puts the merged cards of round 1 first,
// their entries as revision 1 has them.
func TestFilePlanFilesARoundOnTopOfTheSettled(t *testing.T) {
	r, store, _, one := roundTwoRig(t, nil)
	res, err := r.FilePlan(context.Background(), "t-plan", planJSON(t, roundTwoAnswer()))
	if err != nil || res.Revision != 2 || len(res.Findings) != 0 {
		t.Fatalf("filed = %+v, %v; want revision 2", res, err)
	}
	p := storedPlan(t, store, 2)
	if p.Round != 2 || len(p.Cards) != 5 {
		t.Fatalf("revision 2: round %d, %d cards; want round 2 and 5 cards", p.Round, len(p.Cards))
	}
	if !reflect.DeepEqual(p.Cards[:3], one.Cards) {
		t.Fatalf("the settled cards %+v, want revision 1's entries %+v", p.Cards[:3], one.Cards)
	}
	for i, key := range []string{"stats", "integration"} {
		if c := p.Cards[3+i]; c.Key != key || c.Round != 2 {
			t.Fatalf("card %d: %s of round %d, want %s of round 2", 4+i, c.Key, c.Round, key)
		}
	}
}

// A settled card that started from an old card keeps its from: the
// revision before held it for the same key, so the round files.
func TestFilePlanFilesARoundOverASettledFrom(t *testing.T) {
	old := planCardTask(PlanCard{Key: "links-old", Title: "Links", Brief: "The first links API."}, 9, "closed", "")
	r, store, _, one := roundTwoRig(t, func(p *Plan) { p.Cards[1].From = "links-old" }, old)
	res, err := r.FilePlan(context.Background(), "t-plan", planJSON(t, roundTwoAnswer()))
	if err != nil || res.Revision != 2 || len(res.Findings) != 0 {
		t.Fatalf("filed = %+v, %v; want revision 2", res, err)
	}
	if got := storedPlan(t, store, 2).Cards[1]; !reflect.DeepEqual(got, one.Cards[1]) || got.From != "links-old" {
		t.Fatalf("links-api in revision 2 %+v, want revision 1's %+v", got, one.Cards[1])
	}
}

// The key of a merged card of an earlier round is factoryd's to keep: an
// answer that holds it is refused alone, and nothing is stored.
func TestFilePlanRefusesAnEarlierRoundsKey(t *testing.T) {
	r, store, _, _ := roundTwoRig(t, nil)
	answer := roundTwoAnswer()
	links := roundOne().Cards[1]
	links.Round = 0
	answer.Cards = append(answer.Cards, links)
	res, err := r.FilePlan(context.Background(), "t-plan", planJSON(t, answer))
	want := []string{"plan: card links-api: the key of a merged card of round 1; the plan keeps that card as it is, so leave it out and give a new card a new key"}
	if err != nil || res.Revision != 0 || !slices.Equal(res.Findings, want) {
		t.Fatalf("filed = %+v, %v; want the findings %q", res, err, want)
	}
	if rev, _, _ := store.PlanRevision(context.Background(), "t-plan", 0); rev.Revision != 1 {
		t.Fatalf("a refused round stored revision %d", rev.Revision)
	}
}

// A settled card stays on main: take_off_main naming it gets the one
// finding that says so, once however often it names it, and not
// replanFindings' which asks to drop a kept key the planner cannot drop.
func TestFilePlanRefusesASettledKeyInTakeOffMain(t *testing.T) {
	r, store, _, _ := roundTwoRig(t, nil)
	want := []string{"plan: take_off_main names card links-api, settled in round 1: it stays on main as it is; a person's off-main replan can take it off main"}
	for _, off := range [][]string{{"links-api"}, {"links-api", "links-api"}} {
		answer := roundTwoAnswer()
		answer.TakeOffMain = off
		res, err := r.FilePlan(context.Background(), "t-plan", planJSON(t, answer))
		if err != nil || res.Revision != 0 || !slices.Equal(res.Findings, want) {
			t.Fatalf("take_off_main %q: filed = %+v, %v; want the findings %q", off, res, err, want)
		}
		for _, f := range res.Findings {
			if strings.Contains(f, "which this revision keeps") {
				t.Fatalf("finding %q asks to leave a settled key out", f)
			}
		}
	}
	if rev, _, _ := store.PlanRevision(context.Background(), "t-plan", 0); rev.Revision != 1 {
		t.Fatalf("a refused round stored revision %d", rev.Revision)
	}
}

// A card of a later round cannot start from a settled card: its work is
// on main, and phase 4's merged rule says so.
func TestFilePlanRefusesAFromOfASettledCard(t *testing.T) {
	r, store, _, _ := roundTwoRig(t, nil)
	answer := roundTwoAnswer()
	answer.Cards[0].From = "links-api"
	res, err := r.FilePlan(context.Background(), "t-plan", planJSON(t, answer))
	want := []string{"card stats: from links-api, which merged: its work is on main, so start from main and leave from out"}
	if err != nil || res.Revision != 0 || !slices.Equal(res.Findings, want) {
		t.Fatalf("filed = %+v, %v; want the findings %q", res, err, want)
	}
	if rev, _, _ := store.PlanRevision(context.Background(), "t-plan", 0); rev.Revision != 1 {
		t.Fatalf("a refused round stored revision %d", rev.Revision)
	}
}

// A revision of round 2 without a round step, a person's replan, still
// files round 2 on top of round 1's merged cards.
func TestFilePlanSettlesWithoutARoundStep(t *testing.T) {
	ctx := context.Background()
	r, store, _ := filePlanRig(t)
	one, two := roundOne(), twoRounds()
	for i, p := range []Plan{one, two} {
		if _, err := store.AddPlanRevision(ctx, "t-plan", json.RawMessage(planJSON(t, p)), fmt.Sprintf("sha-%d", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range two.Cards {
		k := planCardTask(c, int64(10+i), "done", "m"+c.Key)
		if c.Round == 2 {
			k.Status, k.MergeCommit, k.PlanRevision = "queued", "", 0
		}
		if err := store.CreateTask(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.timeline.Append(ctx, "t-plan", stepReplan, map[string]any{"origin": ReplanPerson, "by": "ana", "comment": "count unique clicks", "revision": 2}); err != nil {
		t.Fatal(err)
	}
	answer := roundTwoAnswer()
	answer.Cards[0].Brief = "GET /api/links/{id}/stats answers the unique clicks."
	res, err := r.FilePlan(ctx, "t-plan", planJSON(t, answer))
	if err != nil || res.Revision != 3 || len(res.Findings) != 0 {
		t.Fatalf("filed = %+v, %v; want revision 3", res, err)
	}
	p := storedPlan(t, store, 3)
	if p.Round != 2 || len(p.Cards) != 5 || !reflect.DeepEqual(p.Cards[:3], one.Cards) {
		t.Fatalf("revision 3: round %d, cards %+v; want round 2 over round 1's entries", p.Round, p.Cards)
	}
	if c := p.Cards[3]; c.Key != "stats" || c.Round != 2 || c.Brief != answer.Cards[0].Brief {
		t.Fatalf("card 4 %+v, want the changed stats card of round 2", c)
	}
}

// timelineWrites is timelineReads that counts each timeline's writes too:
// an append reads the timeline once and writes it once.
type timelineWrites struct {
	*timelineReads
	puts map[string]int
}

func (o *timelineWrites) Put(ctx context.Context, key string, body []byte) error {
	if id, ok := strings.CutSuffix(strings.TrimPrefix(key, "sessions/"), "/timeline.jsonl"); ok {
		o.mu.Lock()
		o.puts[id]++
		o.mu.Unlock()
	}
	return o.fakeObjectStore.Put(ctx, key, body)
}

// RunPlan reads the plan's timeline once: the pending step, the work so
// far and the replan's record read the same bytes. Its steps' appends read
// it besides, one read per write.
func TestRunPlanReadsTheTimelineOnce(t *testing.T) {
	r, _, kube, _ := roundTwoRig(t, nil)
	objs := &timelineWrites{timelineReads: &timelineReads{fakeObjectStore: r.objects.(*fakeObjectStore), n: map[string]int{}}, puts: map[string]int{}}
	r.objects, r.timeline.Objects = objs, objs
	done := make(chan error)
	go func() {
		_, err := execActivity(t, r, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string(nil))
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, resultLine(true, "no plan"))
	if err := <-done; err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if reads := objs.n["t-plan"] - objs.puts["t-plan"]; reads != 1 {
		t.Fatalf("RunPlan read the plan's timeline %d times beside its %d appends, want once", reads, objs.puts["t-plan"])
	}
}

// The planner's last plan leaves out the merged cards of earlier rounds,
// which factoryd keeps, and carries none of factoryd's round stamps. A
// round with nothing settled, and a person's replan of a plan filed
// before rounds, read as today.
func TestRunPlanShowsTheRoundWithoutTheSettled(t *testing.T) {
	round2 := roundOne()
	round2.Round, round2.Cards = 0, []PlanCard{}
	round1 := roundOne()
	round1.Round = 0
	for i := range round1.Cards {
		round1.Cards[i].Round = 0
	}
	oneSettled := roundOne()
	oneSettled.Round, oneSettled.Cards = 0, slices.Clone(oneSettled.Cards[1:])
	for i := range oneSettled.Cards {
		oneSettled.Cards[i].Round = 0
	}
	roundTwo := twoRounds()
	roundTwo.Round, roundTwo.Cards = 0, slices.Clone(roundTwo.Cards[3:])
	for i := range roundTwo.Cards {
		roundTwo.Cards[i].Round = 0
	}
	roundStep := map[string]any{"origin": ReplanRound, "revision": 1, "round": 2}
	for name, tc := range map[string]struct {
		last    Plan
		step    map[string]any
		want    string
		holds   []string
		missing []string
		closed  []string
	}{
		"round 2 over round 1's one merged card": {roundOne(), roundStep,
			"your last plan, revision 1, without the 1 merged card of its earlier rounds, which the plan keeps as it is: answer it again, changed as the findings ask:\n```json\n" +
				planJSON(t, oneSettled) + "\n```",
			[]string{`"key":"links-api"`, `"key":"redirects"`}, []string{`"round"`, `"key":"foundation"`}, []string{"links-api", "redirects"}},
		"a person's replan in round 2": {twoRounds(), map[string]any{"origin": ReplanPerson, "by": "ana", "comment": "count unique clicks", "revision": 1},
			"your last plan, revision 1, without the 3 merged cards of its earlier rounds, which the plan keeps as they are: answer it again, changed as the findings ask:\n```json\n" +
				planJSON(t, roundTwo) + "\n```",
			[]string{`"key":"stats"`, `"key":"integration"`}, []string{`"round"`, `"key":"foundation"`}, nil},
		"round 2 over round 1's three merged cards": {roundOne(), roundStep,
			"your last plan, revision 1, without the 3 merged cards of its earlier rounds, which the plan keeps as they are: answer it again, changed as the findings ask:\n```json\n" +
				planJSON(t, round2) + "\n```",
			[]string{`"cards":[]`, `"outline":["click stats","the acceptance tests"]`}, []string{`"round"`, `"key"`}, nil},
		"round 1 with nothing settled": {roundOne(), nil,
			"your last plan, revision 1: answer the whole plan again, changed as the findings ask:\n```json\n" + planJSON(t, round1) + "\n```",
			[]string{`"key":"foundation"`, `"key":"links-api"`, `"key":"redirects"`}, []string{`"round"`}, nil},
		"a person's replan of a plan filed before rounds": {testPlan(), map[string]any{"origin": ReplanPerson, "by": "ana", "comment": "too wide", "revision": 1},
			"your last plan, revision 1: answer the whole plan again, changed as the findings ask:\n```json\n" + planJSON(t, testPlan()) + "\n```",
			[]string{`"key":"integration"`}, []string{`"round"`}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, store, kube := filePlanRig(t)
			if _, err := store.AddPlanRevision(ctx, "t-plan", json.RawMessage(planJSON(t, tc.last)), "sha-1"); err != nil {
				t.Fatal(err)
			}
			for i, c := range tc.last.Cards {
				k := planCardTask(c, int64(10+i), "done", "m"+c.Key)
				if slices.Contains(tc.closed, c.Key) {
					k.Status, k.MergeCommit = "closed", ""
				}
				if err := store.CreateTask(ctx, k); err != nil {
					t.Fatal(err)
				}
			}
			if tc.step != nil {
				if err := r.timeline.Append(ctx, "t-plan", stepReplan, tc.step); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error)
			go func() {
				_, err := execActivity(t, r, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string(nil))
				done <- err
			}()
			awaitPod(t, kube, done)
			kube.scriptPods(0, resultLine(true, "no plan"))
			if err := <-done; err != nil {
				t.Fatalf("RunPlan: %v", err)
			}
			var got []string
			if err := json.Unmarshal([]byte(podEnv(kube.podAt(0))["FINDINGS"]), &got); err != nil || len(got) == 0 || got[0] != tc.want {
				t.Fatalf("FINDINGS %q (%v), want first\n%q", got, err, tc.want)
			}
			for _, s := range tc.holds {
				if !strings.Contains(got[0], s) {
					t.Fatalf("the last plan %q lacks %s", got[0], s)
				}
			}
			for _, s := range tc.missing {
				if strings.Contains(got[0], s) {
					t.Fatalf("the last plan %q holds %s", got[0], s)
				}
			}
		})
	}
}

// roundTwoFiling is fileRig with revision 1 one and revision 2 two, the
// cards of one done on issues 2, 3 and 4 with their Forgejo dependencies,
// extra tasks beside them, and the filing step of revision 1.
func roundTwoFiling(t *testing.T, one, two Plan, extra ...Task) (*Runner, *fakeStore, *fakeObjectStore, *planForgejo, *fakeWorkflows) {
	t.Helper()
	ctx := context.Background()
	r, store, objects, fj, wf := fileRig(t, one)
	if _, err := store.AddPlanRevision(ctx, "t-plan", json.RawMessage(planJSON(t, two)), "sha-2"); err != nil {
		t.Fatal(err)
	}
	for i, c := range one.Cards {
		extra = append(extra, planCardTask(c, int64(2+i), "done", "m"+c.Key))
	}
	for _, k := range extra {
		if err := store.CreateTask(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	for n, deps := range map[int64][]int64{2: nil, 3: {2}, 4: {2, 3}} {
		fj.issues[n] = &planIssue{title: one.Cards[n-2].Title, poster: "factory-agent", state: "open", deps: deps}
	}
	fj.next = 4
	if err := r.timeline.Append(ctx, "t-plan", stepFiling, map[string]any{"revision": 1}); err != nil {
		t.Fatal(err)
	}
	return r, store, objects, fj, wf
}

// fileRound runs fileRevision of t-plan's revision n under the fixture
// version.
func fileRound(t *testing.T, r *Runner, store *fakeStore, n int) ChildrenResult {
	t.Helper()
	ctx := context.Background()
	plan, repo, err := r.task(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	rev, ok, err := store.PlanRevision(ctx, "t-plan", n)
	if err != nil || !ok {
		t.Fatalf("revision %d: %v, %v", n, ok, err)
	}
	res, err := r.fileRevision(ctx, plan, repo, fixtureVersion(t), rev)
	if err != nil {
		t.Fatalf("fileRevision of revision %d: %v", n, err)
	}
	return res
}

// A round files its own cards. The merged cards of earlier rounds are
// looked up for the plan's comment alone: no issue, dependency, branch or
// start of theirs, though one started from an old card. The comment lists
// every card with its issue and its round.
func TestFileRevisionFilesOnlyTheRound(t *testing.T) {
	one, two := roundOne(), twoRounds()
	one.Cards[1].From, two.Cards[1].From = "links-old", "links-old"
	old := planCardTask(PlanCard{Key: "links-old", Title: "Links", Brief: "The first links API."}, 9, "closed", "")
	r, store, objects, fj, wf := roundTwoFiling(t, one, two, old)
	res := fileRound(t, r, store, 2)
	cards := cardsByKey(t, store)
	stats, integration := cards["stats"], cards["integration"]
	if len(res.Refused) != 0 || !slices.Equal(res.Issues, []int64{5, 6}) || stats.IssueNumber != 5 || integration.IssueNumber != 6 || fj.next != 6 {
		t.Fatalf("filed %+v, stats #%d, integration #%d, last issue #%d; want stats #5 and integration #6 alone", res, stats.IssueNumber, integration.IssueNumber, fj.next)
	}
	if len(store.deps[stats.ID]) != 0 || !slices.Equal(store.deps[integration.ID], []string{stats.ID}) {
		t.Fatalf("task_deps: stats %v, integration %v; want none and [%s]", store.deps[stats.ID], store.deps[integration.ID], stats.ID)
	}
	for _, k := range []string{"t-foundation", "t-links-api", "t-redirects"} {
		if len(store.deps[k]) != 0 {
			t.Fatalf("task_deps of the settled %s: %v, want none", k, store.deps[k])
		}
	}
	for _, c := range fj.calls {
		if slices.Contains([]string{"GET issues/2/dependencies", "GET issues/3/dependencies", "GET issues/4/dependencies", "GET fork branch factory/t-links-old"}, c) {
			t.Fatalf("the filing called %q; all calls %q", c, fj.calls)
		}
	}
	if !slices.Equal(wf.started, []string{stats.ID, integration.ID}) {
		t.Fatalf("started %v, want stats and integration", wf.started)
	}
	if steps := planSteps(t, objects, "t-plan"); !slices.Equal(steps, []string{"children_created [5 6]"}) {
		t.Fatalf("plan steps %v", steps)
	}
	if len(fj.comments) != 1 || fj.comments[0].issue != 1 {
		t.Fatalf("comments %+v, want the plan's comment", fj.comments)
	}
	want := "| # | key | role | title | size | depends on | issue | round |\n|---|---|---|---|---|---|---|---|\n" +
		"| 1 | foundation | foundation | Foundation: layout, contract, schema | medium |  | #2 | 1 |\n" +
		"| 2 | links-api | feature | Links API | small | foundation | #3 | 1 |\n" +
		"| 3 | redirects | feature | Redirects and clicks | small | foundation, links-api | #4 | 1 |\n" +
		"| 4 | stats | feature | Click stats | small |  | #5 | 2 |\n" +
		"| 5 | integration | integration | Integration: acceptance tests | medium | stats | #6 | 2 |\n"
	if !strings.Contains(fj.comments[0].body, want) {
		t.Fatalf("the plan's comment lacks\n%s\nin\n%s", want, fj.comments[0].body)
	}
}

// A card of an earlier round is its task: a revision that names one with
// no task files nothing of its round, and no retry heals it.
func TestFileRevisionWantsTheTasksOfEarlierRounds(t *testing.T) {
	ctx := context.Background()
	r, store, _, fj, _ := roundTwoFiling(t, roundOne(), twoRounds())
	delete(store.tasks, "t-redirects")
	plan, repo, err := r.task(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	rev, _, _ := store.PlanRevision(ctx, "t-plan", 2)
	_, err = r.fileRevision(ctx, plan, repo, fixtureVersion(t), rev)
	var app *temporal.ApplicationError
	if !errors.As(err, &app) || app.Type() != errTerminal || app.Message() != "card redirects of round 1 has no task" || fj.next != 4 {
		t.Fatalf("fileRevision = %v, last issue #%d; want the missing task named, terminal, and no issue filed", err, fj.next)
	}
}

// The deploy owner a Deploy line needs may come in any round up to the
// last: a round with an outline files without one, the last round needs
// one, and a merged card of an earlier round owning deploy/ counts.
func TestFileRevisionLeavesTheDeployOwnerToTheLastRound(t *testing.T) {
	deploy := func(store *fakeStore) {
		plan := store.tasks["t-plan"]
		plan.Objective = deploySpec
		store.tasks["t-plan"] = plan
	}
	r, store, _, _, _ := fileRig(t, roundOne())
	deploy(store)
	if ref := requestPlan(t, r); !slices.Equal(ref.Filed, []int64{2, 3, 4}) || len(ref.Refused) != 0 {
		t.Fatalf("round 1 without a deploy owner: %+v, want its three cards filed", ref)
	}

	r, store, _, fj, _ := roundTwoFiling(t, roundOne(), twoRounds())
	deploy(store)
	if res := fileRound(t, r, store, 2); !slices.Equal(res.Refused, []string{noDeployOwner}) || len(res.Issues) != 0 || fj.next != 4 {
		t.Fatalf("the last round without a deploy owner: %+v, last issue #%d; want the refusal", res, fj.next)
	}

	one, two := roundOne(), twoRounds()
	one.Cards[0].Directories = []string{"cmd/server", "internal/", "deploy/"}
	two.Cards[0].Directories = one.Cards[0].Directories
	r, store, _, fj, _ = roundTwoFiling(t, one, two)
	deploy(store)
	if res := fileRound(t, r, store, 2); len(res.Refused) != 0 || !slices.Equal(res.Issues, []int64{5, 6}) {
		t.Fatalf("the last round over a settled deploy owner: %+v, want stats #5 and integration #6", res)
	}
	if want := "| # | key | role | title | size | depends on | issue | round | deploy |\n|---|---|---|---|---|---|---|---|---|\n" +
		"| 1 | foundation | foundation | Foundation: layout, contract, schema | medium |  | #2 | 1 | deploy |\n"; len(fj.comments) != 1 || !strings.Contains(fj.comments[0].body, want) {
		t.Fatalf("the plan's comment lacks\n%s\nin %+v", want, fj.comments)
	}
}

// The plan's comment names the round: the outline still to plan, or that
// the round is the last. A later round's table carries each card's round,
// and its details blocks are the round's cards. A plan filed before rounds
// reads as before.
func TestPlanCommentNamesTheRound(t *testing.T) {
	plan := Task{ID: "t-plan", Objective: "Link shortener"}
	details := func(body string) []string {
		var out []string
		for _, s := range strings.Split(body, "<details><summary>")[1:] {
			key, _, _ := strings.Cut(s, ":")
			out = append(out, key)
		}
		return out
	}
	one := planComment(plan, 1, roundOne(), nil)
	if !strings.Contains(one, "- click: one redirect served\n\nRound 1. Still to plan, in later rounds:\n- click stats\n- the acceptance tests\n\n"+
		"| # | key | role | title | size | depends on | issue |\n|---|---|---|---|---|---|---|\n") || strings.Contains(one, "round |") ||
		!slices.Equal(details(one), []string{"foundation", "links-api", "redirects"}) {
		t.Fatalf("round 1's comment:\n%s", one)
	}
	two := planComment(plan, 2, twoRounds(), nil)
	if !strings.Contains(two, "- click: one redirect served\n\nRound 2, the last.\n\n"+
		"| # | key | role | title | size | depends on | issue | round |\n|---|---|---|---|---|---|---|---|\n"+
		"| 1 | foundation | foundation | Foundation: layout, contract, schema | medium |  |  | 1 |\n") ||
		!strings.Contains(two, "| 4 | stats | feature | Click stats | small |  |  | 2 |\n") ||
		!slices.Equal(details(two), []string{"stats", "integration"}) {
		t.Fatalf("round 2's comment:\n%s", two)
	}
	before := planComment(plan, 1, testPlan(), nil)
	if strings.Contains(before, "Round ") || strings.Contains(before, "round |") ||
		!slices.Equal(details(before), []string{"foundation", "links-api", "redirects", "integration"}) {
		t.Fatalf("the comment of a plan filed before rounds:\n%s", before)
	}
}

// The confirmation names the parts later rounds plan; a plan without an
// outline asks as before.
func TestConfirmationNamesTheOutline(t *testing.T) {
	if got := confirmationQuestion(1, roundOne()); !strings.HasSuffix(got,
		".\nStill to plan, in later rounds: click stats; the acceptance tests.\nThe whole plan is in the plan's comment on this issue.") {
		t.Fatalf("round 1's confirmation:\n%s", got)
	}
	want := "Confirmation of the spec and the criteria of plan revision 1: answer okay to confirm them, or answer with what changes; " +
		"the criteria are fixed only when this is answered.\n" +
		"Outcome: A link shortener: links with slugs that redirect and count clicks.\n" +
		"Glossary: slug: the short key a link gets; click: one redirect served\n" +
		"Criteria: links_crud (test), redirect_302 (test), all_green (invariant).\n" +
		"The whole plan is in the plan's comment on this issue."
	if got := confirmationQuestion(1, testPlan()); got != want {
		t.Fatalf("a plan without an outline:\n%s\nwant\n%s", got, want)
	}
}

// deltaRound runs draftDelta on twoRounds with ten more features of
// round 2 (extras), 12 cards in the round, every card filed and done,
// redirect_302 unmet in the integration card's review, and answer as the
// delta round's model answer: the store and the plan's timeline after.
func deltaRound(t *testing.T, answer string) (*fakeStore, string) {
	t.Helper()
	ctx := context.Background()
	p := extras(10)
	r, store, objects, fj, _ := fileRig(t, p)
	v := fixtureVersion(t)
	r.cfg.Policy = func() policy.Version { return v }
	plan := store.tasks["t-plan"]
	plan.RequiredCapabilities = []string{policy.ClassCoder}
	store.tasks["t-plan"] = plan
	for i, c := range p.Cards {
		n := int64(2 + i)
		if err := store.CreateTask(ctx, planCardTask(c, n, "done", "m"+c.Key)); err != nil {
			t.Fatal(err)
		}
		fj.issues[n] = &planIssue{title: c.Title, poster: "factory-agent", state: "open"}
	}
	fj.next = int64(1 + len(p.Cards))
	if err := store.PutCriteriaVerdicts(ctx, "t-integration", "review", FactoryDecider, []CriterionVerdict{
		{Criterion: "links_crud", Verdict: "met", Kind: CriterionTest},
		{Criterion: "redirect_302", Verdict: "unmet", Kind: CriterionTest, Evidence: "a redirect counts twice in internal/stats", Fix: "planner"},
		{Criterion: "all_green", Verdict: "met", Kind: CriterionInvariant},
	}); err != nil {
		t.Fatal(err)
	}
	r.supervisor = testSupervisorModel((&fakeLiteLLM{content: answer}).srv(t).URL)
	t1, repo, err := r.task(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	r.draftDelta(ctx, t1, repo, []PlanCatch{{TaskID: "t-plan", Revision: 1, Kind: CatchDelta, Finder: FinderApprover, Detail: "redirect_302 grant 9"}})
	tl, _ := objects.Get(ctx, "sessions/t-plan/timeline.jsonl")
	return store, string(tl)
}

// A delta card joins the last round on top of its 12 cards: the round's
// cap does not count it, and it waits for the cards of its round it names,
// never for a merged card of an earlier round.
func TestDeltaCardJoinsTheLastRound(t *testing.T) {
	store, tl := deltaRound(t, `{"card": {"key": "fix-stats", "role": "feature", "title": "Fix the stats", "brief": "Count each redirect once.", `+
		`"depends_on": ["redirects", "stats"], "acceptance": [{"criterion": "redirect_302", "text": "TestStats in internal/stats: two redirects: clicks 2"}], "size": "small"}}`)
	fix, filed := cardsByKey(t, store)["fix-stats"]
	if !filed || !strings.Contains(tl, `"step":"delta_card"`) || strings.Contains(tl, `"step":"delta_refused"`) {
		t.Fatalf("fix-stats filed %v; the plan's timeline:\n%s", filed, tl)
	}
	if !slices.Equal(store.deps[fix.ID], []string{"t-stats"}) {
		t.Fatalf("task_deps of fix-stats %v, want [t-stats]", store.deps[fix.ID])
	}
}

// A delta card is a feature card: a foundation answer, which a later round
// would take as its first, is refused, and no finished card of the round
// is made to wait for it.
func TestDeltaCardIsAFeature(t *testing.T) {
	store, tl := deltaRound(t, `{"card": {"key": "stats-contract", "role": "foundation", "title": "Stats contract", "brief": "The stats schema.", `+
		`"acceptance": [{"criterion": "redirect_302", "text": "TestStats in internal/stats: two redirects: clicks 2"}], "size": "small"}}`)
	if _, filed := cardsByKey(t, store)["stats-contract"]; filed || strings.Contains(tl, `"step":"delta_card"`) ||
		!strings.Contains(tl, `"why":"the delta card's role is foundation; a delta card is a feature card"`) {
		t.Fatalf("stats-contract filed %v; the plan's timeline:\n%s", filed, tl)
	}
	if len(store.deps) != 0 {
		t.Fatalf("task_deps %v, want none", store.deps)
	}
}

// A round is complete when every card of it ended done or closed, one at
// least done, and no card outside the plan's keys is still open. A card
// of an earlier round counts for its own round only.
func TestRoundComplete(t *testing.T) {
	p := Plan{Round: 2, Cards: []PlanCard{{Key: "a", Round: 1}, {Key: "b", Round: 2}, {Key: "c", Round: 2}}}
	card := func(key, status string) Task { return Task{ID: "t-" + key, PlanKey: key, Status: status} }
	for _, tc := range []struct {
		a, b, c, fix string
		want         bool
	}{
		{"done", "done", "done", "", true},
		{"done", "done", "closed", "", true},
		{"done", "closed", "closed", "", false},
		{"done", "done", "merged", "", false},
		{"done", "done", "failed", "", false},
		{"done", "done", "done", "queued", false},
		{"done", "done", "done", "closed", true},
		{"closed", "done", "done", "", true},
	} {
		cards := []Task{card("a", tc.a), card("b", tc.b), card("c", tc.c)}
		if tc.fix != "" {
			cards = append(cards, card("fix", tc.fix))
		}
		if got := roundComplete(p, cards); got != tc.want {
			t.Errorf("a %s, b %s, c %s, fix %q: %t, want %t", tc.a, tc.b, tc.c, tc.fix, got, tc.want)
		}
	}
}

// roundsOfFive is a Round-2 plan with two parts still to plan: card a of
// round 1, done, and five cards of round 2, four of them done.
func roundsOfFive() (Plan, []Task) {
	p := Plan{Round: 2, Outline: []string{"the console", "the scheduler"}, Cards: []PlanCard{{Key: "a", Round: 1}}}
	cards := []Task{{ID: "t-a", PlanKey: "a", Status: "done"}}
	for i, status := range []string{"done", "done", "running", "done", "done"} {
		key := fmt.Sprint("b", i)
		p.Cards = append(p.Cards, PlanCard{Key: key, Round: 2})
		cards = append(cards, Task{ID: "t-" + key, PlanKey: key, Status: status})
	}
	return p, cards
}

// roundCase is one plan of the board's round tests: its cards' tasks.
type roundCase struct {
	name  string
	p     Plan
	cards []Task
}

// roundCases are roundsOfFive; a last round 3 holding one queued card
// alone; round 1 with one done card and x to plan; round 1 without an
// outline; and a plan filed before rounds.
func roundCases() []roundCase {
	five, cards := roundsOfFive()
	one := []Task{{ID: "t-a", PlanKey: "a", Status: "done"}}
	return []roundCase{
		{"round 2 of five", five, cards},
		{"round 3", Plan{Round: 3, Cards: []PlanCard{{Key: "z", Round: 3}}}, []Task{{ID: "t-z", PlanKey: "z", Status: "queued"}}},
		{"round 1 with x", Plan{Round: 1, Outline: []string{"x"}, Cards: []PlanCard{{Key: "a", Round: 1}}}, one},
		{"round 1 alone", Plan{Round: 1, Cards: []PlanCard{{Key: "a", Round: 1}}}, one},
		{"before rounds", Plan{Cards: []PlanCard{{Key: "a"}}}, one},
	}
}

func TestRoundLine(t *testing.T) {
	want := []string{
		"round 2: 5 cards, 4 done. Still to plan: the console, the scheduler.",
		"round 3: 1 card, 0 done.",
		"round 1: 1 card, 1 done. Still to plan: x.",
		"",
		"",
	}
	for i, c := range roundCases() {
		if got := RoundLine(c.p, c.cards); got != want[i] {
			t.Errorf("%s: %q, want %q", c.name, got, want[i])
		}
	}
}

func TestRoundItems(t *testing.T) {
	want := [][]RoundItem{
		{{"round 1: 1 card, 1 done.", false}, {"round 2: 5 cards, 4 done. This round.", true},
			{"still to plan: the console", false}, {"still to plan: the scheduler", false}},
		{{"round 3: 1 card, 0 done. This round.", true}},
		{{"round 1: 1 card, 1 done. This round.", true}, {"still to plan: x", false}},
		nil,
		nil,
	}
	for i, c := range roundCases() {
		if got := RoundItems(c.p, c.cards); !reflect.DeepEqual(got, want[i]) {
			t.Errorf("%s: %+v, want %+v", c.name, got, want[i])
		}
	}
}

// roundRig is replanRig's plan p (issue 10, open; done and finished)
// without its cards c and s: revision 1 is roundOne, and its three cards
// are done with merge commits. sebastian alone writes sf/repo.
func roundRig(t *testing.T) (*Runner, *fakeStore, *prForgejo) {
	t.Helper()
	return roundRigOf(t, roundOne())
}

// roundRigOf is roundRig with revision 1 one, whose cards (issue 20 on)
// are done with merge commits.
func roundRigOf(t *testing.T, one Plan) (*Runner, *fakeStore, *prForgejo) {
	t.Helper()
	ctx := context.Background()
	r, store, fj := replanRig(t, "done", 0)
	r.Writes = func(_ context.Context, repository, login string) (bool, error) {
		return repository == "sf/repo" && login == "sebastian", nil
	}
	delete(store.tasks, "c")
	delete(store.tasks, "s")
	finished := replanAt
	p := store.tasks["p"]
	p.FinishedAt = &finished
	store.tasks["p"] = p
	if _, err := store.AddPlanRevision(ctx, "p", json.RawMessage(planJSON(t, one)), "sha-1"); err != nil {
		t.Fatal(err)
	}
	for i, c := range one.Cards {
		k := planCardTask(c, int64(20+i), "done", "m"+c.Key)
		k.Parent, k.Repository, k.FinishedAt = "p", "sf/repo", &finished
		if err := store.CreateTask(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	return r, store, fj
}

// setStatus puts task id of store in status.
func setStatus(store *fakeStore, id, status string) {
	k := store.tasks[id]
	k.Status = status
	store.tasks[id] = k
}

// roundTwoStep is the replan step that starts roundRig's second round.
var roundTwoStep = []ReplanStep{{Origin: ReplanRound, Revision: 1, Round: 2, Time: replanAt}}

// A plan whose round is done and whose outline still holds parts goes
// back to its planner for the next round: the replan step of round 2,
// the plan queued with its counters at zero and its workflow reopened,
// its open issue left as it is. The second check finds the plan queued
// and does nothing.
func TestRoundCheckStartsTheNextRound(t *testing.T) {
	r, store, fj := roundRig(t)
	for check := 1; check <= 2; check++ {
		r.roundCheck(context.Background(), "p")
		if got := replanSteps(t, r, "p"); !reflect.DeepEqual(got, roundTwoStep) {
			t.Fatalf("check %d: replan steps %+v, want %+v", check, got, roundTwoStep)
		}
		if p := store.tasks["p"]; p.Status != "queued" || p.Usage != (TaskUsage{}) || p.FinishedAt != nil {
			t.Fatalf("check %d: plan %s with usage %+v, finished %v; want queued with none", check, p.Status, p.Usage, p.FinishedAt)
		}
		if got := reopens(r); !reflect.DeepEqual(got, []reopenCall{{taskID: "p"}}) {
			t.Fatalf("check %d: reopens %+v, want p's alone", check, got)
		}
		if len(fj.reopened) != 0 {
			t.Fatalf("check %d: issues reopened %v, want none", check, fj.reopened)
		}
	}
}

// A plan whose issue a writer closed when its next round would start was
// closed by a person, whose close the webhook did not end: the plan card
// ends closed, nothing starts and the issue stays closed, as when the
// round's step was written and its reopen lost. The close check then
// closes the group, every card being done.
func TestRoundCheckEndsAPlanWhoseIssueIsClosed(t *testing.T) {
	ctx := context.Background()
	for _, written := range []bool{false, true} {
		r, store, fj := roundRig(t)
		fj.issueOff, fj.closer = true, "sebastian"
		if written {
			if err := r.timeline.Append(ctx, "p", stepReplan, map[string]any{"origin": ReplanRound, "revision": 1, "round": 2}); err != nil {
				t.Fatal(err)
			}
		}
		steps := len(replanSteps(t, r, "p"))
		r.roundCheck(ctx, "p")
		if got := store.tasks["p"].Status; got != "closed" || len(replanSteps(t, r, "p")) != steps || len(reopens(r)) != 0 || len(fj.reopened) != 0 || !fj.issueOff {
			t.Fatalf("step written %t: plan %s, replan steps %d, reopens %+v, issues reopened %v; want closed, %d, none, none",
				written, got, len(replanSteps(t, r, "p")), reopens(r), fj.reopened, steps)
		}
		r.planCloseCheck(ctx, "p")
		r.planCloseCheck(ctx, "p")
		want := []string{"factoryd: the plan card was closed; the plan is closed, and its merged cards stay on main."}
		if n := len(stepsNamed(t, r, "p", "plan_closed")); n != 1 || !slices.Equal(fj.comments, want) {
			t.Fatalf("step written %t: plan_closed %d times, comments %q; want 1 and %q", written, n, fj.comments, want)
		}
	}
}

// A close the webhook does not count does not count at a round's start
// either: an author without write access, the bot account without
// factoryd's record of its close, a timeline that names no closer. The
// issue opens again and the round starts. The bot's close factoryd
// recorded ends the plan, as a writer's does.
func TestRoundCheckCountsTheWebhooksClosesAlone(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, closer       string
		recorded, noCloser bool
		after              string
	}{
		{name: "an author without write access", closer: "mallory", after: "queued"},
		{name: "the bot without a record", closer: "factory-agent", after: "queued"},
		{name: "a timeline that names no closer", noCloser: true, after: "queued"},
		{name: "the bot with factoryd's record", closer: "factory-agent", recorded: true, after: "closed"},
	} {
		r, store, fj := roundRig(t)
		fj.issueOff, fj.closer, fj.noCloser = true, tc.closer, tc.noCloser
		if tc.recorded {
			store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "p", Kind: IntakeFactoryClosed, Author: "factoryd", CommentID: 10})
		}
		r.roundCheck(ctx, "p")
		want, reopened, wf := roundTwoStep, []int64{10}, []reopenCall{{taskID: "p"}}
		if tc.after == "closed" {
			want, reopened, wf = nil, nil, nil
		}
		if got := store.tasks["p"].Status; got != tc.after || !reflect.DeepEqual(replanSteps(t, r, "p"), want) ||
			!slices.Equal(fj.reopened, reopened) || !reflect.DeepEqual(reopens(r), wf) {
			t.Fatalf("%s: plan %s, replan steps %+v, issues reopened %v, reopens %+v; want %s, %+v, %v, %+v",
				tc.name, got, replanSteps(t, r, "p"), fj.reopened, reopens(r), tc.after, want, reopened, wf)
		}
	}
}

// A round's start reads the plan's issue once and leaves it as it read it:
// a person who closes it right after that read keeps it closed, and the
// planner's run, which reads the close, ends the plan.
func TestRoundCheckLeavesAnIssueClosedAfterItsRead(t *testing.T) {
	r, store, fj := roundRig(t)
	fj.issueOff, fj.openReads = true, 1
	r.roundCheck(context.Background(), "p")
	if got := store.tasks["p"].Status; got != "queued" || len(fj.reopened) != 0 || !fj.issueOff || !reflect.DeepEqual(reopens(r), []reopenCall{{taskID: "p"}}) {
		t.Fatalf("plan %s, issues reopened %v, issue closed %t, reopens %+v; want queued, none, closed, p's", got, fj.reopened, fj.issueOff, reopens(r))
	}
}

// A plan between rounds whose cards carry no merge commit has no open
// group and no verdict rows: the minute pass still visits it and starts
// its next round.
func TestCriteriaPassVisitsAPlanBetweenRounds(t *testing.T) {
	r, store, _ := roundRig(t)
	for _, id := range []string{"t-foundation", "t-links-api", "t-redirects"} {
		k := store.tasks[id]
		k.MergeCommit = ""
		store.tasks[id] = k
	}
	r.CriteriaPass(context.Background())
	if got := replanSteps(t, r, "p"); !reflect.DeepEqual(got, roundTwoStep) || store.tasks["p"].Status != "queued" {
		t.Fatalf("replan steps %+v, plan %s; want %+v and queued", got, store.tasks["p"].Status, roundTwoStep)
	}
}

// Such a plan whose card a person closed is visited too, and closes: one
// plan_closed and the closed card's comment.
func TestCriteriaPassClosesAClosedPlanBetweenRounds(t *testing.T) {
	r, store, fj := roundRig(t)
	for _, id := range []string{"t-foundation", "t-links-api", "t-redirects"} {
		k := store.tasks[id]
		k.MergeCommit = ""
		store.tasks[id] = k
	}
	setStatus(store, "p", "closed")
	want := []string{"factoryd: the plan card was closed; the plan is closed, and its merged cards stay on main."}
	r.CriteriaPass(context.Background())
	r.CriteriaPass(context.Background())
	if n := len(stepsNamed(t, r, "p", "plan_closed")); n != 1 || !slices.Equal(fj.comments, want) {
		t.Fatalf("plan_closed %d times, comments %q; want 1 and %q", n, fj.comments, want)
	}
}

// The round check starts nothing while the plan's round or the plan
// itself is not done with it.
func TestRoundCheckWaits(t *testing.T) {
	ctx := context.Background()
	for name, change := range map[string]func(*testing.T, *Runner, *fakeStore){
		"the newest revision has no outline": func(t *testing.T, r *Runner, store *fakeStore) {
			last := roundOne()
			last.Outline = nil
			if _, err := store.AddPlanRevision(ctx, "p", json.RawMessage(planJSON(t, last)), "sha-2"); err != nil {
				t.Fatal(err)
			}
		},
		"redirects merged": func(t *testing.T, r *Runner, store *fakeStore) { setStatus(store, "t-redirects", "merged") },
		"redirects failed": func(t *testing.T, r *Runner, store *fakeStore) { setStatus(store, "t-redirects", "failed") },
		"every card closed": func(t *testing.T, r *Runner, store *fakeStore) {
			for _, id := range []string{"t-foundation", "t-links-api", "t-redirects"} {
				setStatus(store, id, "closed")
			}
		},
		"an open delta card": func(t *testing.T, r *Runner, store *fakeStore) {
			if err := store.CreateTask(ctx, Task{ID: "t-fix", PlanKey: "fix", Status: "queued", Kind: "agent", Track: policy.TrackDirect,
				Source: SourcePlan, Parent: "p", Repository: "sf/repo"}); err != nil {
				t.Fatal(err)
			}
		},
		"the plan runs":      func(t *testing.T, r *Runner, store *fakeStore) { setStatus(store, "p", "running") },
		"the plan is queued": func(t *testing.T, r *Runner, store *fakeStore) { setStatus(store, "p", "queued") },
		"the plan is closed": func(t *testing.T, r *Runner, store *fakeStore) { setStatus(store, "p", "closed") },
		"a person's replan is pending": func(t *testing.T, r *Runner, store *fakeStore) {
			if err := r.timeline.Append(ctx, "p", stepReplan, map[string]any{"origin": ReplanPerson, "by": "sebastian", "revision": 1}); err != nil {
				t.Fatal(err)
			}
		},
		"the plan_closed row": func(t *testing.T, r *Runner, store *fakeStore) { store.closed["p"] = replanAt },
	} {
		t.Run(name, func(t *testing.T) {
			r, store, _ := roundRig(t)
			change(t, r, store)
			steps := len(replanSteps(t, r, "p"))
			r.roundCheck(ctx, "p")
			if got := len(replanSteps(t, r, "p")); got != steps || len(reopens(r)) != 0 {
				t.Fatalf("replan steps %d, want %d; reopens %+v, want none", got, steps, reopens(r))
			}
		})
	}
}

// While the project's queue pause names the plan, its own group revert
// runs, and the next round waits for its end; a pause of another plan
// does not hold it.
func TestRoundCheckWaitsForThePlansRevert(t *testing.T) {
	for plan, want := range map[string][]ReplanStep{"p": nil, "other": roundTwoStep} {
		r, store, _ := roundRig(t)
		store.pauses = map[string]QueuePause{"sf/repo": {Project: "sf/repo", Plan: plan, RevertTask: "t-revert"}}
		r.roundCheck(context.Background(), "p")
		if got := replanSteps(t, r, "p"); !reflect.DeepEqual(got, want) || len(reopens(r)) != len(want) {
			t.Fatalf("a pause of plan %s: replan steps %+v, reopens %+v; want %+v and as many reopens", plan, got, reopens(r), want)
		}
	}
}

// A round step written whose reopen was lost is healed: the plan, still
// done, reopens, and no second step is written. A queued plan is left to
// its run.
func TestRoundCheckReopensAfterAFailedReopen(t *testing.T) {
	ctx := context.Background()
	for status, want := range map[string]int{"done": 1, "queued": 0} {
		r, store, _ := roundRig(t)
		setStatus(store, "p", status)
		if err := r.timeline.Append(ctx, "p", stepReplan, map[string]any{"origin": ReplanRound, "revision": 1, "round": 2}); err != nil {
			t.Fatal(err)
		}
		r.roundCheck(ctx, "p")
		if got := store.tasks["p"].Status; got != "queued" || len(reopens(r)) != want {
			t.Fatalf("plan %s: now %s with reopens %+v; want queued and %d", status, got, reopens(r), want)
		}
		if got := replanSteps(t, r, "p"); !reflect.DeepEqual(got, roundTwoStep) {
			t.Fatalf("plan %s: replan steps %+v, want %+v", status, got, roundTwoStep)
		}
	}
}

// The minute pass visits the plan through its open group and starts its
// next round.
func TestCriteriaPassStartsTheNextRound(t *testing.T) {
	r, store, _ := roundRig(t)
	r.CriteriaPass(context.Background())
	if got := replanSteps(t, r, "p"); !reflect.DeepEqual(got, roundTwoStep) || store.tasks["p"].Status != "queued" {
		t.Fatalf("replan steps %+v, plan %s; want %+v and queued", got, store.tasks["p"].Status, roundTwoStep)
	}
}

// Review Focus 5: a person's replan of a plan whose round is complete
// files the next round, and its step names it; the round check then adds
// no step. A round that still runs replans itself, and a plan filed
// before rounds replans as it did: their step names no round. A last
// round that is complete files a round after it.
func TestReplanAfterACompleteRoundFilesTheNext(t *testing.T) {
	ctx := context.Background()
	r, _, _ := roundRig(t)
	if _, err := r.Replan(ctx, "p", ReplanAsk{By: "ana", Comment: "stats first"}); err != nil {
		t.Fatal(err)
	}
	want := []ReplanStep{{Origin: ReplanPerson, By: "ana", Comment: "stats first", Revision: 1, Round: 2, Time: replanAt}}
	if got := replanSteps(t, r, "p"); !reflect.DeepEqual(got, want) {
		t.Fatalf("replan steps %+v, want %+v", got, want)
	}
	r.roundCheck(ctx, "p")
	if got := replanSteps(t, r, "p"); !reflect.DeepEqual(got, want) {
		t.Fatalf("replan steps after the round check %+v, want %+v", got, want)
	}

	last := testPlan()
	last.Round = 1
	for i := range last.Cards {
		last.Cards[i].Round = 1
	}
	for _, tc := range []struct {
		name    string
		one     Plan
		running bool
		round   any
	}{
		{"redirects running", roundOne(), true, nil},
		{"a last round 1 done", last, false, 2.0},
		{"a plan filed before rounds", testPlan(), false, nil},
	} {
		r, store, _ := roundRigOf(t, tc.one)
		if tc.running {
			k := store.tasks["t-redirects"]
			k.Status, k.MergeCommit = "running", ""
			store.tasks["t-redirects"] = k
		}
		if _, err := r.Replan(ctx, "p", ReplanAsk{By: "ana", Comment: "stats first"}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		lines := stepsNamed(t, r, "p", stepReplan)
		var step map[string]any
		if len(lines) != 1 || json.Unmarshal(lines[0], &step) != nil {
			t.Fatalf("%s: replan steps %s", tc.name, lines)
		}
		if round, ok := step["round"]; round != tc.round || ok != (tc.round != nil) {
			t.Fatalf("%s: the step's round %v (set %t), want %v", tc.name, round, ok, tc.round)
		}
	}
}

// A round's replan opens its planner's record with the round, and the
// merged cards of earlier rounds, which the plan keeps as they are, have
// no line, file or branch there: a card of the round that closed has. A
// person's replan that files a round says so after its opener.
func TestReplanRecordOpensARound(t *testing.T) {
	ctx := context.Background()
	record := func(t *testing.T, step map[string]any, closed bool) replanRecord {
		t.Helper()
		r, store, fj := roundRig(t)
		fj.branchGone = true
		if closed {
			k := store.tasks["t-redirects"]
			k.Status, k.MergeCommit = "closed", ""
			store.tasks["t-redirects"] = k
		}
		if err := r.timeline.Append(ctx, "p", stepReplan, step); err != nil {
			t.Fatal(err)
		}
		rec, ok, err := r.replanRecord(ctx, store.tasks["p"], timelineOf(t, r, "p"))
		if err != nil || !ok {
			t.Fatalf("replanRecord: %t, %v", ok, err)
		}
		return rec
	}
	opener := "This is round 2 of plan #10: the cards of round 1 are done. Plan round 2 from your outline, on top of what the earlier rounds built."
	round := map[string]any{"origin": ReplanRound, "revision": 1, "round": 2}
	if rec := record(t, round, false); !slices.Equal(rec.findings, []string{opener}) || len(rec.files) != 0 || len(rec.branches) != 0 {
		t.Fatalf("the round's record %+v, want the opener alone", rec)
	}

	rec := record(t, round, true)
	line := "#22 Redirects and clicks (feature, key redirects): ended closed, no commit past its base; spent 0.00 of 2.00 EUR in 0 sessions; its record is card-22.md"
	if !slices.Equal(rec.findings, []string{opener, line}) || len(rec.files) != 1 || rec.files[0].Name != "card-22.md" || len(rec.branches) != 0 {
		t.Fatalf("the record with redirects closed %+v, want the opener, then redirects' line and file alone", rec)
	}

	rec = record(t, map[string]any{"origin": ReplanPerson, "by": "ana", "comment": "x", "round": 3}, false)
	if want := []string{"This is a replan of plan #10, asked by ana: x", "It files round 3: every card of round 2 ended."}; !slices.Equal(rec.findings, want) {
		t.Fatalf("a person's round findings %q, want %q", rec.findings, want)
	}
}

// A round's filing records its cards, in filing order, on the plan's
// issue once, with the parts still to plan or that it is the last round;
// the plan's replanned step and its event name them. An old card gets no
// step, and no card is counted kept or dropped.
func TestRecordReplanOfARound(t *testing.T) {
	ctx := context.Background()
	console := extras(2)
	console.Outline = []string{"the console"}
	console.Cards = slices.DeleteFunc(console.Cards, func(c PlanCard) bool { return c.Role == RoleIntegration })
	console.Cards[4].DependsOn = []string{"extra-1"}
	for _, tc := range []struct {
		name    string
		next    Plan
		issues  map[string]int64
		comment string
		cards   []int64
	}{
		{"the last round", twoRounds(), map[string]int64{"stats": 21, "integration": 22},
			"factoryd: round 2 planned at revision 3, 2 cards: #21, #22. It is the last round.", []int64{21, 22}},
		{"the console still to plan", console, map[string]int64{"stats": 21, "extra-1": 22, "extra-0": 23},
			"factoryd: round 2 planned at revision 3, 3 cards: #21, #22, #23. Still to plan: the console.", []int64{21, 22, 23}},
	} {
		r, store, fj := roundRig(t)
		if err := r.timeline.Append(ctx, "p", stepReplan, map[string]any{"origin": ReplanRound, "revision": 1, "round": 2}); err != nil {
			t.Fatal(err)
		}
		p, repo, err := r.task(ctx, "p")
		if err != nil {
			t.Fatal(err)
		}
		step, _, err := r.pendingReplan(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := r.recordReplan(ctx, p, repo, 3, tc.next, step, tc.issues); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
		}
		if !slices.Equal(fj.comments, []string{tc.comment}) {
			t.Fatalf("%s: comments %q, want %q once", tc.name, fj.comments, tc.comment)
		}
		var cards []any
		for _, n := range tc.cards {
			cards = append(cards, float64(n))
		}
		lines := stepsNamed(t, r, "p", "replanned")
		var got map[string]any
		if len(lines) == 0 || json.Unmarshal(lines[0], &got) != nil {
			t.Fatalf("%s: the plan's replanned steps %s", tc.name, lines)
		}
		delete(got, "step")
		delete(got, "time")
		if want := map[string]any{"revision": 3.0, "origin": "round", "round": 2.0, "cards": cards}; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: the plan's replanned step %v, want %v", tc.name, got, want)
		}
		i := slices.IndexFunc(store.events, func(e Event) bool { return e.Kind == EventPlanReplanned })
		if want := map[string]any{"revision": 3, "origin": ReplanRound, "round": 2, "cards": tc.cards}; i < 0 || store.events[i].TaskID != "p" || !reflect.DeepEqual(store.events[i].Detail, want) {
			t.Fatalf("%s: events %+v, want plan_replanned on p with %v", tc.name, store.events, want)
		}
		for _, id := range []string{"t-foundation", "t-links-api", "t-redirects"} {
			if lines := stepsNamed(t, r, id, "replanned"); len(lines) != 0 {
				t.Fatalf("%s: %s's replanned steps %s, want none", tc.name, id, lines)
			}
		}
		if _, pending, err := r.pendingReplan(ctx, store.tasks["p"]); err != nil || pending {
			t.Fatalf("%s: pending %t, %v after the record; want ended", tc.name, pending, err)
		}
	}
}
