package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// rowsOfKind are the events rows of one kind, oldest first.
func rowsOfKind(store *fakeStore, kind string) []Event {
	var out []Event
	for _, e := range store.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// cardsFiledRound counts the card_filed rows of one round in store.
func cardsFiledRound(store *fakeStore, round int) int {
	n := 0
	for _, e := range rowsOfKind(store, EventPlanCardFiled) {
		if e.Detail["round"] == round {
			n++
		}
	}
	return n
}

// replanOrigins counts the plan_replanned rows of each origin in store.
func replanOrigins(store *fakeStore) map[string]int {
	out := map[string]int{}
	for _, e := range rowsOfKind(store, EventPlanReplanned) {
		if origin, ok := e.Detail["origin"].(string); ok {
			out[origin]++
		}
	}
	return out
}

// failingEvents is a Timeline sink that fails the first fails Record
// calls of kind, then records through its store.
type failingEvents struct {
	*fakeStore
	kind  string
	fails int
	err   error
}

func (f *failingEvents) Record(ctx context.Context, e Event) (int64, error) {
	if f.fails > 0 && e.Kind == f.kind {
		f.fails--
		return 0, f.err
	}
	return f.fakeStore.Record(ctx, e)
}

// Two cuts, two groups. A pending cut step is the request, not the cut:
// the row comes with the applied cut, whose group is the group the cut
// card belongs to — its plan for a plan's child, the plan itself for a
// direct card's conversion — and lands once however often the record runs
// again. A closed group carries its closure timestamp; an open one none.
func TestDivideConquerCutFacts(t *testing.T) {
	ctx := context.Background()
	at := time.Now().Add(time.Hour).UTC()
	feature := func(key, from string) PlanCard {
		return PlanCard{Key: key, Role: RoleFeature, Title: key, Brief: key, Size: "small", From: from}
	}

	// a plan's child, cut by its plan's replan
	r, store, _, step := recordRig(t, at, map[string]any{"origin": ReplanCut, "card": "t-cut", "work": "the list endpoint", "stops": []string{"t-cut"}, "revision": 2},
		Task{ID: "t-cut", PlanKey: "api", IssueNumber: 50, Status: "closed", PlanRevision: 2, SpentEUR: 4.2, Usage: TaskUsage{Attempts: 3}},
		Task{ID: "t-api2", PlanKey: "api2", IssueNumber: 51, Status: "queued", CreatedAt: at.Add(10 * time.Minute)},
		Task{ID: "t-docs2", PlanKey: "docs2", IssueNumber: 52, Status: "queued", CreatedAt: at.Add(10 * time.Minute)})
	p, repo, err := r.task(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	next := Plan{Summary: "s", Cards: []PlanCard{feature("api2", "api"), feature("docs2", "")}}
	if got := rowsOfKind(store, EventCut); len(got) != 0 {
		t.Fatalf("cut rows while the step is only the request: %+v", got)
	}
	// a filing that runs again records the cut once
	for range 2 {
		if err := r.recordReplan(ctx, p, repo, 3, next, step, map[string]int64{"api2": 51, "docs2": 52}); err != nil {
			t.Fatal(err)
		}
	}
	rows := rowsOfKind(store, EventCut)
	if len(rows) != 1 {
		t.Fatalf("cut rows %+v, want one", rows)
	}
	row := rows[0]
	into, ok := row.Detail["into"].([]int64)
	if row.TaskID != "t-cut" || row.IssueNumber != 50 || row.IdentityKey != identityKey(EventCut, "t-plan", "t-cut") || !ok ||
		!slices.Equal(into, []int64{51, 52}) || row.Detail["plan"] != "t-plan" || row.Detail["group"] != "t-plan" ||
		row.Detail["revision"] != 3 || row.Detail["session"] != 3 || row.Detail["spent"] != 4.2 {
		t.Fatalf("the cut's row %+v", row)
	}
	if _, closed, err := store.PlanClosed(ctx, "t-plan"); err != nil || closed {
		t.Fatalf("the cut's group reads closed before its close: %t, %v", closed, err)
	}
	r.timeline.recordEvent(ctx, "t-plan", EventPlanClosed, map[string]any{"criteria": 3})
	closedAt, closed, err := store.PlanClosed(ctx, "t-plan")
	if err != nil || !closed || closedAt.IsZero() {
		t.Fatalf("the closed group: %t, %v", closed, err)
	}

	// a direct card's cut, the plan's own conversion: the group is the
	// plan itself, and it stays open
	r, store, _, _ = recordRig(t, at, map[string]any{"origin": ReplanCut, "card": "t-plan", "work": "the rest"})
	if err := r.timeline.Append(ctx, "t-plan", stepToPlan, map[string]any{"reason": "cut: too big", "session": 2, "spent": 1.5}); err != nil {
		t.Fatal(err)
	}
	plan := store.tasks["t-plan"]
	plan.SpentEUR, plan.Usage = 2.3, TaskUsage{Attempts: 1}
	store.tasks["t-plan"] = plan
	p, repo, err = r.task(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	step, _, err = r.pendingReplan(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.recordReplan(ctx, p, repo, 1, Plan{Summary: "s", Cards: []PlanCard{feature("cli", "#1")}}, step, map[string]int64{"cli": 61}); err != nil {
		t.Fatal(err)
	}
	rows = rowsOfKind(store, EventCut)
	if len(rows) != 1 {
		t.Fatalf("the direct cut's rows %+v, want one", rows)
	}
	if rows[0].TaskID != "t-plan" || rows[0].Detail["group"] != "t-plan" || rows[0].Detail["revision"] != 1 {
		t.Fatalf("the direct cut's row %+v", rows[0])
	}
	if _, closed, _ := store.PlanClosed(ctx, "t-plan"); closed {
		t.Fatal("the direct cut's group reads closed")
	}
}

// The planning facts of two plans. Plan A stays in round 1 across a
// person's replan and a cut that changes a card; plan B files a second
// round. The first round's filings stay four — a retained card is no new
// filing — the later round's stay three, the plans' highest rounds stay
// one and two though their revisions run three and two, and each replan
// origin lands exactly once. The cards round 2 files over were seeded
// without facts: a legacy row stays unknown, never backfilled.
func TestDivideConquerPlanningFacts(t *testing.T) {
	ctx := context.Background()

	// plan A: three cards of round 1, a person's replan that keeps them,
	// a cut that changes one into a new card of round 1
	rA, storeA, _, fj, _ := fileRig(t, roundOne())
	pinned := fixtureVersion(t)
	rA.cfg.Policy = func() policy.Version { return pinned }
	if ref := requestPlan(t, rA); len(ref.Refused) != 0 || len(ref.Filed) != 3 {
		t.Fatalf("plan A's revision 1: %+v", ref)
	}
	rebrief := roundOne()
	rebrief.Cards[1].Brief = "Create, list and delete links, and count them."
	if _, err := storeA.AddPlanRevision(ctx, "t-plan", json.RawMessage(planJSON(t, rebrief)), "sha-2"); err != nil {
		t.Fatal(err)
	}
	if err := rA.timeline.Append(ctx, "t-plan", stepReplan, map[string]any{"origin": ReplanPerson, "by": "ana", "comment": "rebrief the api", "revision": 1}); err != nil {
		t.Fatal(err)
	}
	plan, repo, err := rA.task(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	personStep, _, err := rA.pendingReplan(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if res := fileRound(t, rA, storeA, 2); len(res.Refused) != 0 || len(res.Issues) != 3 {
		t.Fatalf("plan A's revision 2: %+v", res)
	}
	byKey := map[string]int64{}
	for key, c := range cardsByKey(t, storeA) {
		byKey[key] = c.IssueNumber
	}
	// the record runs again: no row multiplies
	if err := rA.recordReplan(ctx, plan, repo, 2, rebrief, personStep, byKey); err != nil {
		t.Fatal(err)
	}

	setStatus := func(id, status string) {
		k := storeA.tasks[id]
		k.Status = status
		storeA.tasks[id] = k
	}
	cut := cardsByKey(t, storeA)["links-api"]
	cut.Status, cut.PlanRevision = "running", 1
	storeA.tasks[cut.ID] = cut
	fj.branches = map[string]string{"factory/" + cut.ID: "c7"}
	setStatus("t-plan", "done")
	if res := replanChild(t, rA, cut.ID, "the list endpoint is left"); res.Refused != "" {
		t.Fatalf("ReplanChild = %+v", res)
	}
	if err := rA.Finish(ctx, cut.ID, "closed", "cut: plan #1 replans its work"); err != nil {
		t.Fatal(err)
	}
	setStatus("t-plan", "running")
	next := rebrief
	next.Cards = slices.Clone(rebrief.Cards)
	next.Cards[1].Key, next.Cards[1].From = "links-api-2", "links-api"
	next.Cards[2].DependsOn = []string{"links-api-2"}
	if res, err := rA.FilePlan(ctx, "t-plan", planJSON(t, next)); err != nil || len(res.Findings) != 0 || res.Revision != 3 {
		t.Fatalf("plan A's revision 3: %+v, %v", res, err)
	}
	plan, repo, err = rA.task(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	cutStep, _, err := rA.pendingReplan(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if res := fileRound(t, rA, storeA, 3); len(res.Refused) != 0 || len(res.Issues) != 3 {
		t.Fatalf("plan A's revision 3: %+v", res)
	}
	byKey = map[string]int64{}
	for key, c := range cardsByKey(t, storeA) {
		byKey[key] = c.IssueNumber
	}
	if err := rA.recordReplan(ctx, plan, repo, 3, next, cutStep, byKey); err != nil {
		t.Fatal(err)
	}

	// plan B: a second round of three cards over round 1's settled ones
	two := twoRounds()
	two.Cards = append(slices.Clone(two.Cards), PlanCard{Key: "clicks", Role: RoleFeature, Title: "Click counts",
		Brief: "Count every click of a redirect.", Directories: []string{"internal/clicks"},
		Acceptance: []PlanItem{{Criterion: "links_crud", Text: "TestClicks in internal/clicks: two redirects count two clicks"}},
		Size:       "small", Round: 2})
	rB, storeB, _, _, _ := roundTwoFiling(t, roundOne(), two)
	if err := rB.timeline.Append(ctx, "t-plan", stepReplan, map[string]any{"origin": ReplanRound, "revision": 1, "round": 2}); err != nil {
		t.Fatal(err)
	}
	if res := fileRound(t, rB, storeB, 2); len(res.Refused) != 0 || len(res.Issues) != 3 {
		t.Fatalf("plan B's round 2: %+v", res)
	}

	// the filings: four of the first round, three of the second, the
	// seeded round-1 rows of plan B never backfilled
	if got := cardsFiledRound(storeA, 1); got != 4 {
		t.Fatalf("plan A's first-round filings %d, want 4", got)
	}
	if got := cardsFiledRound(storeB, 1); got != 0 {
		t.Fatalf("plan B's first-round filings %d, want the legacy rows left alone", got)
	}
	if got := cardsFiledRound(storeB, 2); got != 3 {
		t.Fatalf("plan B's later-round filings %d, want 3", got)
	}
	retained := 0
	for _, e := range rowsOfKind(storeA, EventPlanCardFiled) {
		if e.Detail["key"] == "foundation" {
			retained++
			if e.Detail["round"] != 1 || e.Detail["revision"] != 1 {
				t.Fatalf("the retained card's row %+v, want round 1 of revision 1", e)
			}
		}
		if e.IdentityKey == "" {
			t.Fatalf("a card_filed row without an identity: %+v", e)
		}
	}
	if retained != 1 {
		t.Fatalf("the card kept by every revision filed %d rows, want one", retained)
	}

	// the rounds: a plan's round is not its revision count
	for _, tc := range []struct {
		store    *fakeStore
		round    int
		revision int
	}{{storeA, 1, 3}, {storeB, 2, 2}} {
		p := storedPlan(t, tc.store, 0)
		rev, _, err := tc.store.PlanRevision(ctx, "t-plan", 0)
		if err != nil || p.Round != tc.round || rev.Revision != tc.revision {
			t.Fatalf("round %d of revision %d: round %d, revision %d, %v", tc.round, tc.revision, p.Round, rev.Revision, err)
		}
	}

	// the origins: one person, one cut, one round
	for store, want := range map[*fakeStore]map[string]int{
		storeA: {ReplanPerson: 1, ReplanCut: 1},
		storeB: {ReplanRound: 1},
	} {
		if got := replanOrigins(store); !reflect.DeepEqual(got, want) {
			t.Fatalf("plan_replanned origins %v, want %v", got, want)
		}
	}
	for _, e := range append(rowsOfKind(storeA, EventPlanReplanned), rowsOfKind(storeB, EventPlanReplanned)...) {
		if e.IdentityKey == "" {
			t.Fatalf("a plan_replanned row without an identity: %+v", e)
		}
	}
}

// The ledger under failure and retry. A failed row write fails the filing
// and its record — observable, not swallowed — and the healed retry
// records each row once: cuts, replans and card filings never multiply,
// a replay of a whole filing adds no row and stores no revision, and a
// legacy row recorded before the facts existed stays unknown.
func TestDivideConquerPlanningRetries(t *testing.T) {
	ctx := context.Background()
	at := time.Now().Add(time.Hour).UTC()
	feature := func(key, from string) PlanCard {
		return PlanCard{Key: key, Role: RoleFeature, Title: key, Brief: key, Size: "small", From: from}
	}

	// a legacy replan row, recorded before the facts existed: no origin,
	// no identity, and it stays that way
	r, store, _, step := recordRig(t, at, map[string]any{"origin": ReplanCut, "card": "t-cut", "work": "the list endpoint", "stops": []string{"t-cut"}, "revision": 2},
		Task{ID: "t-cut", PlanKey: "api", IssueNumber: 50, Status: "closed", PlanRevision: 2, SpentEUR: 4.2, Usage: TaskUsage{Attempts: 3}},
		Task{ID: "t-api2", PlanKey: "api2", IssueNumber: 51, Status: "queued", CreatedAt: at.Add(10 * time.Minute)},
		Task{ID: "t-docs2", PlanKey: "docs2", IssueNumber: 52, Status: "queued", CreatedAt: at.Add(10 * time.Minute)})
	if _, err := store.Record(ctx, Event{Repository: "linkshort/linkshort", TaskID: "t-plan", IssueNumber: 1,
		Kind: EventPlanReplanned, Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"revision": 1}}); err != nil {
		t.Fatal(err)
	}
	p, repo, err := r.task(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	next := Plan{Summary: "s", Cards: []PlanCard{feature("api2", "api"), feature("docs2", "")}}

	// the cut's row fails: the record says so, and no cut landed
	sink := &failingEvents{fakeStore: store, kind: EventCut, fails: 1, err: errors.New("the sink is down")}
	r.timeline.Events = sink
	err = r.recordReplan(ctx, p, repo, 3, next, step, map[string]int64{"api2": 51, "docs2": 52})
	if err == nil || !strings.Contains(err.Error(), "record the cut's row") || !strings.Contains(err.Error(), "the sink is down") {
		t.Fatalf("the failed cut row: %v", err)
	}
	if rows := rowsOfKind(store, EventCut); len(rows) != 0 {
		t.Fatalf("cut rows after the failed write: %+v", rows)
	}
	// the healed retry records the cut and the replan once, beside the
	// legacy row that stays unknown
	r.timeline.Events = store
	if err := r.recordReplan(ctx, p, repo, 3, next, step, map[string]int64{"api2": 51, "docs2": 52}); err != nil {
		t.Fatal(err)
	}
	if rows := rowsOfKind(store, EventCut); len(rows) != 1 || rows[0].IdentityKey == "" {
		t.Fatalf("cut rows after the retry: %+v", rows)
	}
	replans := rowsOfKind(store, EventPlanReplanned)
	if len(replans) != 2 {
		t.Fatalf("plan_replanned rows %+v, want the legacy one and the cut's", replans)
	}
	if _, ok := replans[0].Detail["origin"]; ok || replans[0].IdentityKey != "" {
		t.Fatalf("the legacy row %+v gained a provenance", replans[0])
	}
	if replans[1].Detail["origin"] != ReplanCut || replans[1].IdentityKey == "" {
		t.Fatalf("the cut's replan row %+v", replans[1])
	}

	// a card's filing row fails mid-filing: the filing says so, and the
	// healed retry files the rest with one row a card
	r, store, _, _, _ = fileRig(t, roundOne())
	if ref := requestPlan(t, r); len(ref.Refused) != 0 || len(ref.Filed) != 3 {
		t.Fatalf("revision 1: %+v", ref)
	}
	two := twoRounds()
	if _, err := store.AddPlanRevision(ctx, "t-plan", json.RawMessage(planJSON(t, two)), "sha-2"); err != nil {
		t.Fatal(err)
	}
	for i, c := range roundOne().Cards {
		k := planCardTask(c, int64(2+i), "done", "m"+c.Key)
		if err := store.CreateTask(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.timeline.Append(ctx, "t-plan", stepReplan, map[string]any{"origin": ReplanRound, "revision": 1, "round": 2}); err != nil {
		t.Fatal(err)
	}
	plan, repo, err := r.task(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	rev, ok, err := store.PlanRevision(ctx, "t-plan", 2)
	if err != nil || !ok {
		t.Fatalf("revision 2: %t, %v", ok, err)
	}
	sink = &failingEvents{fakeStore: store, kind: EventPlanCardFiled, fails: 1, err: errors.New("the sink is down")}
	r.timeline.Events = sink
	_, err = r.fileRevision(ctx, plan, repo, fixtureVersion(t), rev)
	if err == nil || !strings.Contains(err.Error(), "record its filing row") || !strings.Contains(err.Error(), "the sink is down") {
		t.Fatalf("the failed card row: %v", err)
	}
	r.timeline.Events = store
	res, err := r.fileRevision(ctx, plan, repo, fixtureVersion(t), rev)
	if err != nil || len(res.Refused) != 0 || len(res.Issues) != 2 {
		t.Fatalf("the retried filing: %+v, %v", res, err)
	}
	if rows := rowsOfKind(store, EventPlanCardFiled); len(rows) != 5 {
		t.Fatalf("card_filed rows %d after the retry, want three of round 1 and two of round 2", len(rows))
	} else if rows[3].Detail["round"] != 2 || rows[4].Detail["round"] != 2 {
		t.Fatalf("the round-2 rows %+v %+v", rows[3], rows[4])
	}
	// the filing runs again whole: no row and no revision multiplies
	if res, err := r.fileRevision(ctx, plan, repo, fixtureVersion(t), rev); err != nil || len(res.Refused) != 0 {
		t.Fatalf("the replayed filing: %+v, %v", res, err)
	}
	if rows := rowsOfKind(store, EventPlanCardFiled); len(rows) != 5 {
		t.Fatalf("card_filed rows %d after the replay, want 5", len(rows))
	}
	if rows := rowsOfKind(store, EventPlanReplanned); len(rows) != 1 || rows[0].Detail["origin"] != ReplanRound {
		t.Fatalf("plan_replanned rows %+v after the replay, want the round's alone", rows)
	}
	if rev, _, _ := store.PlanRevision(ctx, "t-plan", 0); rev.Revision != 2 {
		t.Fatalf("the replay stored revision %d, want 2", rev.Revision)
	}
}
