package factoryd

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// runPlanTask puts the plan task back into its running session, the state
// the file_plan tool files from.
func runPlanTask(store *fakeStore) {
	t := store.tasks["t-plan"]
	t.Status = "running"
	store.tasks["t-plan"] = t
}

// An amendment never changes a criterion under its own id, and a replaces
// names an id the previous revision holds; anything else files clean, and
// the same bytes re-filed land no new revision.
func TestFilePlanRefusesABrokenAmendment(t *testing.T) {
	ctx := context.Background()
	r, store, _, _ := newPlanRunner(t, testPlan())
	pinned := fixtureVersion(t)
	r.cfg.Policy = func() policy.Version { return pinned }
	runPlanTask(store)

	renamed := testPlan()
	renamed.Criteria[0].ID = "links_crud_v2"
	renamed.Criteria[0].Replaces = "gone"
	renamed.Cards[1].Acceptance[0].Criterion = "links_crud_v2"
	renamed.Cards[3].Acceptance[1].Criterion = "links_crud_v2"
	changed := testPlan()
	changed.Criteria[0].Expected = "201 and the audit row"
	for name, tc := range map[string]struct {
		p    Plan
		want string
	}{
		"replaces an id the previous revision does not hold": {renamed, "criterion links_crud_v2: replaces gone, an id the previous revision does not hold"},
		"the same id with another definition":                {changed, "criterion links_crud: the previous revision holds this id with another definition; a changed criterion files under a new id"},
	} {
		res, err := r.FilePlan(ctx, "t-plan", planJSON(t, tc.p))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Findings) != 1 || res.Findings[0] != tc.want {
			t.Fatalf("%s: findings = %q, want [%q]", name, res.Findings, tc.want)
		}
		if rev, _, _ := store.PlanRevision(ctx, "t-plan", 0); rev.Revision != 1 {
			t.Fatalf("%s: a refused amendment landed as revision %d", name, rev.Revision)
		}
	}
	// the same bytes re-filed return the revision they already are and
	// write no records
	res, err := r.FilePlan(ctx, "t-plan", planJSON(t, testPlan()))
	if err != nil || res.Revision != 1 {
		t.Fatalf("re-filed the same plan: %+v, %v", res, err)
	}
	if catches, _ := store.PlanCatches(ctx, "t-plan"); len(catches) != 0 {
		t.Fatalf("the re-file wrote records: %+v", catches)
	}
}

// Piece 10's recording rule and restart rules. The amendment replaces
// links_crud under a new id, drops redirect_302 and its citing card,
// rebriefs the foundation, and changes the started links-api by a new
// card, links-api-2, that starts from its work. It records each
// criterion's fate, closes the dropped card that never started with its
// issue, rebriefs every card that never started, stops the started card
// the revision drops, leaves a final one alone, and its filing continues
// links-api's branch on links-api-2's.
func TestFilePlanAmendmentRecordsAndRestarts(t *testing.T) {
	ctx := context.Background()
	r, store, _, fj, _ := fileRig(t, testPlan())
	pinned := fixtureVersion(t)
	r.cfg.Policy = func() policy.Version { return pinned }
	if ref := requestPlan(t, r); len(ref.Refused) != 0 || !slices.Equal(ref.Filed, []int64{2, 3, 4, 5}) {
		t.Fatalf("the children: %+v", ref)
	}
	runPlanTask(store)
	cards := cardsByKey(t, store)
	started := cards["links-api"]
	started.Status, started.PlanRevision, started.SHA = "running", 1, "b0"
	store.tasks[started.ID] = started
	fj.branches = map[string]string{"factory/" + started.ID: "c7"}
	final := cards["integration"]
	final.Status = "done"
	store.tasks[final.ID] = final
	oldStarted, oldFinal := started.Objective, final.Objective

	amend := testPlan()
	amend.Criteria[0].ID = "links_crud_v2"
	amend.Criteria[0].Replaces = "links_crud"
	amend.Criteria[0].Expected = "201 and the audit row"
	amend.Criteria = append(amend.Criteria[:1], amend.Criteria[2:]...) // redirect_302 goes
	amend.Cards[0].Brief = "Foundation: layout, contract, schema, migrations."
	amend.Cards = append(amend.Cards[:2], amend.Cards[3:]...) // the redirects card goes
	amend.Cards[1].Key, amend.Cards[1].From = "links-api-2", "links-api"
	amend.Cards[1].Acceptance[0].Criterion = "links_crud_v2"
	amend.Cards[2].Acceptance = []PlanItem{amend.Cards[2].Acceptance[0], amend.Cards[2].Acceptance[1]}
	amend.Cards[2].Acceptance[1].Criterion = "links_crud_v2"
	res, err := r.FilePlan(ctx, "t-plan", planJSON(t, amend))
	if err != nil || len(res.Findings) != 0 || res.Revision != 2 {
		t.Fatalf("the amendment: %+v, %v, findings %q", res, err, res.Findings)
	}
	rev, _, _ := store.PlanRevision(ctx, "t-plan", 2)
	var stored Plan
	if err := json.Unmarshal(rev.Plan, &stored); err != nil || len(stored.Cards) != 3 || stored.Cards[1].Key != "links-api-2" || stored.Cards[1].From != "links-api" {
		t.Fatalf("revision 2 = %s, %v: want links-api-2 from links-api in links-api's place", rev.Plan, err)
	}

	catch, _ := store.PlanCatches(ctx, "t-plan")
	if len(catch) != 2 || catch[0].Revision != 2 || catch[0].Kind != CatchCriterion || catch[0].Finder != FinderPlanner ||
		catch[0].Detail != "links_crud_v2 replaces links_crud" || catch[1].Detail != "redirect_302 removed" {
		t.Fatalf("the records: %+v", catch)
	}

	cards = cardsByKey(t, store)
	if c := cards["foundation"]; c.Status != "queued" || !strings.Contains(c.Objective, "Foundation: layout, contract, schema, migrations.") {
		t.Fatalf("the never-started card kept its old brief:\n%s", c.Objective)
	}
	if c := cards["links-api"]; c.Status != "closed" || c.Objective != oldStarted || c.PlanRevision != 1 {
		t.Fatalf("the started card the revision drops is %s, revision %d, want closed with its brief\n%s", c.Status, c.PlanRevision, c.Objective)
	}
	if !slices.ContainsFunc(fj.comments, func(c *stubComment) bool {
		return c.issue == started.IssueNumber && strings.Contains(c.body, "closed: the plan's revision 2 drops it")
	}) {
		t.Fatalf("the stopped card's issue #%d carries no close reason: %+v", started.IssueNumber, fj.comments)
	}
	if c := cards["integration"]; c.Status != "done" || c.Objective != oldFinal {
		t.Fatalf("the final card was touched:\n%s", c.Objective)
	}
	dropped := cards["redirects"]
	if dropped.Status != "closed" {
		t.Fatalf("the dropped card is %s, want closed", dropped.Status)
	}
	if is := fj.issues[dropped.IssueNumber]; is.state != "closed" {
		t.Fatalf("the dropped card's issue #%d is %q, want closed", dropped.IssueNumber, is.state)
	}
	var reason string
	for _, c := range fj.comments {
		if c.issue == dropped.IssueNumber && strings.Contains(c.body, "the plan's revision 2 drops the card redirects") {
			reason = c.body
		}
	}
	if reason == "" {
		t.Fatalf("the dropped card's issue carries no close reason: %+v", fj.comments)
	}

	plan, repo, err := r.task(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := r.fileRevision(ctx, plan, repo, pinned, rev); err != nil || len(res.Refused) != 0 || len(res.Issues) != 3 {
		t.Fatalf("filing revision 2: %+v, %v", res, err)
	}
	next := cardsByKey(t, store)["links-api-2"]
	if next.SHA != "b0" || fj.branches["factory/"+next.ID] != "c7" {
		t.Fatalf("links-api-2 filed at %q, its branch at %q; want links-api's base b0 and its head c7", next.SHA, fj.branches["factory/"+next.ID])
	}
}

// A card that never started takes the new revision's image with its
// brief; a started one keeps the image it runs on and its brief, though
// the revision adds a card, and a revision that changes its image is
// refused.
func TestFilePlanAmendmentMovesAnUnstartedCardsImage(t *testing.T) {
	ctx := context.Background()
	r, store, _, _, _ := fileRig(t, testPlan())
	pinned := fixtureVersion(t)
	name, c, _ := pinned.ProjectOf("linkshort/linkshort")
	c.Images = map[string]policy.ImagePin{"browser": {Digest: "sha256:" + strings.Repeat("b", 64), Key: keyBrowser, Tools: []string{"chromium"}}}
	pinned.Projects[name] = c
	r.cfg.Policy = func() policy.Version { return pinned }
	if ref := requestPlan(t, r); len(ref.Filed) != 4 {
		t.Fatalf("the children: %+v", ref)
	}
	runPlanTask(store)
	started := cardsByKey(t, store)["redirects"]
	started.Status, started.PlanRevision = "running", 1
	store.tasks[started.ID] = started

	amend := testPlan()
	amend.Cards[1].Image = "browser"
	// a rebrief would count the cards "of 5"
	amend.Cards = slices.Insert(amend.Cards, 3, PlanCard{Key: "stats", Role: RoleFeature, Title: "Click stats", Brief: "GET /api/links/<slug>/stats answers the clicks.",
		Directories: []string{"internal/stats"}, Acceptance: []PlanItem{{Criterion: "links_crud", Text: "TestStats in internal/stats answers 200"}}, Size: "small"})
	if res, err := r.FilePlan(ctx, "t-plan", planJSON(t, amend)); err != nil || len(res.Findings) != 0 || res.Revision != 2 {
		t.Fatalf("the amendment: %+v, %v", res, err)
	}
	cards := cardsByKey(t, store)
	if got := cards["links-api"].Image; got != "browser" {
		t.Errorf("the never-started card's image %q, want browser", got)
	}
	if got := cards["redirects"].Image; got != "" {
		t.Errorf("the started card's image %q, want its own", got)
	}
	if got := cards["redirects"].Objective; got != started.Objective {
		t.Errorf("the started card was rebriefed:\n%s\nwant:\n%s", got, started.Objective)
	}
	amend.Cards[2].Image = "browser"
	refusedStartedChange(t, r, store, amend)
}

// refusedStartedChange files p, which changes the entry of the started
// card redirects: the one finding is R7's, and no revision 3 is stored.
func refusedStartedChange(t *testing.T, r *Runner, store *fakeStore, p Plan) {
	t.Helper()
	ctx := context.Background()
	res, err := r.FilePlan(ctx, "t-plan", planJSON(t, p))
	want := []string{"card redirects started against revision 1: keep its entry as revision 1 has it, or leave its key out to stop it"}
	if err != nil || !slices.Equal(res.Findings, want) {
		t.Fatalf("a started card's entry changed: %+v, %v, want the findings %q", res, err, want)
	}
	if rev, _, _ := store.PlanRevision(ctx, "t-plan", 0); rev.Revision != 2 {
		t.Fatalf("a refused amendment landed as revision %d", rev.Revision)
	}
}

// A card that names image default is filed naming none, from any plan
// fileChildren files, the delta round's, read by no readPlan, too.
func TestFileChildrenStoresDefaultAsNoImage(t *testing.T) {
	ctx := context.Background()
	r, store, _, _ := newPlanRunner(t, testPlan())
	pinned := fixtureVersion(t)
	r.cfg.Policy = func() policy.Version { return pinned }
	p := testPlan()
	p.Cards[1].Image = "default"
	plan, repo, err := r.task(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.fileChildren(ctx, plan, repo, pinned, "spec", "plan #1", p, FactoryDecider, true, "m1", nil); err != nil {
		t.Fatal(err)
	}
	if got := cardsByKey(t, store)[p.Cards[1].Key]; got.ID == "" || got.Image != "" {
		t.Fatalf("card %s = %+v, want it filed naming no image", p.Cards[1].Key, got)
	}
}

// A revision's card whose from names an old card with no row of its own
// key starts from that card's work: it files at the old card's base, its
// branch is the old card's head before its issue exists, and its brief
// says so. A from whose branch is gone files at main's head and says that
// instead, with no copy. A keyless from, the plan card's own work after a
// direct card's cut, continues the plan's branch.
func TestFileChildrenContinuesTheOldBranch(t *testing.T) {
	ctx := context.Background()
	pinned := fixtureVersion(t)
	ui2 := PlanCard{Key: "ui2", Role: RoleFeature, Title: "UI again", Brief: "Finish the UI.", Size: "small", From: "ui",
		Acceptance: []PlanItem{{Text: "the page renders"}}}
	file := func(t *testing.T, branches map[string]string, c PlanCard) (Task, *planForgejo) {
		t.Helper()
		r, store, _, fj := newPlanRunner(t, testPlan())
		r.cfg.Policy = func() policy.Version { return pinned }
		fj.branches = branches
		if err := store.CreateTask(ctx, Task{ID: "t-ui", Kind: "agent", Track: policy.TrackDirect, Source: SourcePlan, Parent: "t-plan", PlanKey: "ui",
			Repository: "linkshort/linkshort", IssueNumber: 31, SHA: "b0", Status: "closed", PlanRevision: 1}); err != nil {
			t.Fatal(err)
		}
		plan := store.tasks["t-plan"]
		plan.IssueNumber, plan.SHA = 40, "p0"
		store.tasks["t-plan"] = plan
		if err := r.timeline.Append(ctx, "t-plan", stepReplan, map[string]any{"origin": ReplanCut, "card": "t-plan", "work": "the half"}); err != nil {
			t.Fatal(err)
		}
		_, repo, err := r.task(ctx, "t-plan")
		if err != nil {
			t.Fatal(err)
		}
		old, err := r.oldCards(ctx, plan)
		if err != nil {
			t.Fatal(err)
		}
		cards, err := r.fileChildren(ctx, plan, repo, pinned, "spec", "plan #40", Plan{Summary: "s", Cards: []PlanCard{c}}, FactoryDecider, true, "m1", old)
		if err != nil || len(cards) != 1 {
			t.Fatalf("fileChildren = %+v, %v", cards, err)
		}
		return store.tasks[cards[0].ID], fj
	}
	before := func(calls []string, a, b string) bool {
		i, j := slices.Index(calls, a), slices.Index(calls, b)
		return i >= 0 && j > i
	}

	card, fj := file(t, map[string]string{"factory/t-ui": "c7"}, ui2)
	start := "Starts from the work of #31 (ui), on this card's branch: finish what it holds, make it build green, and land it."
	if card.SHA != "b0" || fj.branches["factory/"+card.ID] != "c7" {
		t.Fatalf("ui2 filed at %q, its branch at %q; want ui's base b0 and its head c7", card.SHA, fj.branches["factory/"+card.ID])
	}
	if !strings.Contains(card.Objective, start) || !strings.Contains(fj.issues[card.IssueNumber].body, start) {
		t.Fatalf("ui2's brief and issue lack the start line:\n%s", card.Objective)
	}
	if !before(fj.calls, "POST fork branches factory/"+card.ID, "POST issues") {
		t.Fatalf("the branch was not in place before the issue: %q", fj.calls)
	}

	card, fj = file(t, map[string]string{}, ui2)
	gone := "The branch of #31 is gone: start from main."
	if card.SHA != "m1" || !strings.Contains(card.Objective, gone) || strings.Contains(card.Objective, "Starts from") {
		t.Fatalf("ui2 without ui's branch filed at %q with:\n%s", card.SHA, card.Objective)
	}
	if slices.ContainsFunc(fj.calls, func(c string) bool {
		return c == "GET fork branch factory/"+card.ID || strings.HasPrefix(c, "POST fork branches")
	}) {
		t.Fatalf("a gone branch was continued: %q", fj.calls)
	}

	cli := PlanCard{Key: "cli", Role: RoleFeature, Title: "CLI", Brief: "Land the cut work.", Size: "small", From: "#40",
		Acceptance: []PlanItem{{Text: "the command runs"}}}
	card, fj = file(t, map[string]string{"factory/t-plan": "d4"}, cli)
	if card.SHA != "p0" || fj.branches["factory/"+card.ID] != "d4" ||
		!strings.Contains(card.Objective, "Starts from the work of #40, on this card's branch: finish what it holds, make it build green, and land it.") {
		t.Fatalf("cli filed at %q, its branch at %q, with:\n%s", card.SHA, fj.branches["factory/"+card.ID], card.Objective)
	}
}

// A rebrief keeps the start line of a card that started from an old
// card's work, or found its branch gone, and names a dependency that has
// no issue yet by its key.
func TestRestartChildrenKeepsTheStartLine(t *testing.T) {
	ctx := context.Background()
	pinned := fixtureVersion(t)
	r, store, _, fj := newPlanRunner(t, testPlan())
	r.cfg.Policy = func() policy.Version { return pinned }
	fj.branches = map[string]string{"factory/t-ui": "c7"}
	for _, k := range []Task{
		{ID: "t-ui", PlanKey: "ui", IssueNumber: 31, SHA: "b0", Status: "closed", PlanRevision: 1},
		{ID: "t-api", PlanKey: "api", IssueNumber: 32, SHA: "b0", Status: "closed", PlanRevision: 1},
	} {
		k.Kind, k.Track, k.Source, k.Parent, k.Repository = "agent", policy.TrackDirect, SourcePlan, "t-plan", "linkshort/linkshort"
		if err := store.CreateTask(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	feature := func(key, from, brief string, deps ...string) PlanCard {
		return PlanCard{Key: key, Role: RoleFeature, Title: key, Brief: brief, Size: "small", From: from, DependsOn: deps,
			Acceptance: []PlanItem{{Text: key + " works"}}}
	}
	plan, repo, err := r.task(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	filed := Plan{Summary: "two", Cards: []PlanCard{feature("ui2", "ui", "Finish the UI."), feature("api2", "api", "Finish the API.")}}
	old, err := r.oldCards(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.fileChildren(ctx, plan, repo, pinned, "spec", "plan #1", filed, FactoryDecider, true, "m1", old); err != nil {
		t.Fatal(err)
	}
	next := Plan{Summary: "three", Cards: []PlanCard{
		feature("ui2", "ui", "Finish the UI, then the footer.", "footer"), feature("api2", "api", "Finish the API and its docs."), feature("footer", "", "The footer."),
	}}
	r.restartChildren(ctx, plan, repo, 2, next)
	cards := cardsByKey(t, store)
	for key, want := range map[string][]string{
		"ui2": {"Finish the UI, then the footer.", "Depends on: footer\n",
			"Starts from the work of #31 (ui), on this card's branch: finish what it holds, make it build green, and land it."},
		"api2": {"Finish the API and its docs.", "The branch of #32 is gone: start from main."},
	} {
		for _, w := range want {
			if !strings.Contains(cards[key].Objective, w) {
				t.Fatalf("%s's rebrief lacks %q:\n%s", key, w, cards[key].Objective)
			}
		}
	}

	// a branch read that fails still rebriefs, without the line
	fj.failCall = "GET /api/v1/repos/factory-agent/linkshort/branches/factory/" + cards["ui2"].ID
	next.Cards[0].Brief = "Finish the UI, the footer and the header."
	r.restartChildren(ctx, plan, repo, 3, next)
	if got := cardsByKey(t, store)["ui2"].Objective; !strings.Contains(got, "Finish the UI, the footer and the header.") || strings.Contains(got, "Starts from") {
		t.Fatalf("ui2's rebrief after a failed branch read:\n%s", got)
	}
}

// A started open card the revision drops is stopped for the replan: with
// no live session its dependents wait on the plan and it ends closed with
// the reason.
func TestRestartChildrenStopsADroppedStartedCard(t *testing.T) {
	ctx := context.Background()
	r, store, _, fj, _ := fileRig(t, testPlan())
	pinned := fixtureVersion(t)
	r.cfg.Policy = func() policy.Version { return pinned }
	if ref := requestPlan(t, r); len(ref.Refused) != 0 || len(ref.Filed) != 4 {
		t.Fatalf("the children: %+v", ref)
	}
	cards := cardsByKey(t, store)
	started := cards["links-api"]
	started.Status, started.PlanRevision = "running", 1
	store.tasks[started.ID] = started
	next := testPlan()
	next.Cards = append(next.Cards[:1], next.Cards[2:]...)
	next.Cards[1].DependsOn = nil
	_, repo, err := r.task(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	r.restartChildren(ctx, store.tasks["t-plan"], repo, 2, next)

	if c := store.tasks[started.ID]; c.Status != "closed" {
		t.Fatalf("the dropped started card is %s, want closed", c.Status)
	}
	if got := store.deps[cards["redirects"].ID]; !slices.Equal(got, []string{cards["foundation"].ID, "t-plan"}) {
		t.Fatalf("redirects waits on %v, want the foundation and the plan", got)
	}
	finals := stepsNamed(t, r, started.ID, "final")
	var final struct{ State, Reason string }
	if len(finals) != 1 || json.Unmarshal(finals[0], &final) != nil || final.State != "closed" || final.Reason != "the plan's revision 2 drops it" {
		t.Fatalf("final steps %s, want closed with the plan's revision 2 drops it", finals)
	}
	if is := fj.issues[started.IssueNumber]; is.state != "closed" {
		t.Fatalf("the stopped card's issue is %q, want closed", is.state)
	}
}

// A card that never started takes the new revision's rung and size with
// its brief, which states them; a started one keeps its own, and a
// revision that changes them is refused.
func TestFilePlanAmendmentRefreshesAnUnstartedCardsRungAndSize(t *testing.T) {
	ctx := context.Background()
	r, store, _, _, _ := fileRig(t, testPlan())
	pinned := fixtureVersion(t)
	r.cfg.Policy = func() policy.Version { return pinned }
	if ref := requestPlan(t, r); len(ref.Filed) != 4 {
		t.Fatalf("the children: %+v", ref)
	}
	runPlanTask(store)
	started := cardsByKey(t, store)["redirects"]
	started.Status, started.PlanRevision = "running", 1
	store.tasks[started.ID] = started

	amend := testPlan()
	amend.Cards[1].Size, amend.Cards[1].Rung, amend.Cards[1].RungReason = "medium", RungFrontier, "the store's schema moves"
	if res, err := r.FilePlan(ctx, "t-plan", planJSON(t, amend)); err != nil || len(res.Findings) != 0 || res.Revision != 2 {
		t.Fatalf("the amendment: %+v, %v", res, err)
	}
	cards := cardsByKey(t, store)
	if c := cards["links-api"]; c.Rung != RungFrontier || c.Size != "medium" || !strings.Contains(c.Objective, "Size: medium") {
		t.Errorf("the never-started card: rung %q, size %q, want frontier and medium", c.Rung, c.Size)
	}
	if c := cards["redirects"]; c.Rung != "" || c.Size != "small" {
		t.Errorf("the started card: rung %q, size %q, want its own", c.Rung, c.Size)
	}
	amend.Cards[2].Size, amend.Cards[2].Rung, amend.Cards[2].RungReason = "medium", RungFrontier, "the store's schema moves"
	refusedStartedChange(t, r, store, amend)
}

// planJSON marshals v or fails the test.
func planJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A plan card's first session pins the plan revision it runs against
// (piece 10): the stamp happens before the session's pod, and a card
// already pinned keeps its revision.
func TestRunSessionStampsThePlanRevision(t *testing.T) {
	ctx := context.Background()
	plan := testPlan()
	card := Task{ID: "t-card", Kind: "agent", Status: "running", Process: "spec", Repository: "sf/r",
		SHA: "abc123", IssueNumber: 3, Author: "alice", Parent: "t-plan", Source: SourcePlan, PlanKey: "links-api",
		RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 2, Objective: "Links API", Track: "direct"}
	kube, objects := newFakeKube(), &fakeObjectStore{}
	runner, _, store := newTestRunner(t, card, kube, objects)
	store.tasks["t-plan"] = Task{ID: "t-plan", Kind: "agent", Status: "done", Track: policy.TrackPlan, Process: "spec",
		Repository: "sf/r", IssueNumber: 1, Author: "alice", AuthorWriter: true, Objective: "Link shortener"}
	pinned := fixtureVersion(t)
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	runner.cfg.Policy = func() policy.Version { return pinned }
	store.AddPlanRevision(ctx, "t-plan", json.RawMessage(planJSON(t, plan)), "s1")
	store.AddPlanRevision(ctx, "t-plan", json.RawMessage(planJSON(t, plan)), "s2")

	logs := pushRequestLine("c1") + "\n" + envLine(harness.Event{Kind: harness.EventFinished, Time: time.Now(), Detail: map[string]any{"success": true, "summary": "Links API done."}}) +
		"\n" + resultLine(true, "Links API done.")
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunSession, SessionInput{TaskID: "t-card", Attempt: 1, Model: "factory/big"})
		if err == nil {
			var res SessionResult
			err = val.Get(&res)
		}
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, logs)
	<-done
	if got, _, _ := store.Task(ctx, "t-card"); got.PlanRevision != 2 {
		t.Fatalf("the first session stamped plan revision %d, want 2", got.PlanRevision)
	}
}

// A stale card's merge approval never opens: the request fails the card
// with the reason, and the merge itself refuses it too, so its branch
// stays for the plan's re-run (piece 10). A card is stale when the newest
// revision drops its key.
func TestStaleCardNeitherAsksNorMerges(t *testing.T) {
	ctx := context.Background()
	fj := &gateForgejo{mergeStatus: []int{200}}
	task := gateTask
	task.Source, task.Parent, task.PlanKey, task.PlanRevision = SourcePlan, "t-plan", "make-check", 1
	r, store, _ := newGateRunner(t, task, fj)
	store.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary":"one","cards":[{"key":"make-check"}]}`), "s1")
	store.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary":"two"}`), "s2")
	if err := r.RequestApproval(ctx, "t1", ApprovalInput{}); err == nil || !strings.Contains(err.Error(), "the plan's revision 2 drops the card make-check") ||
		!strings.Contains(err.Error(), "type: terminal") {
		t.Fatalf("RequestApproval: %v", err)
	}
	val, err := execActivity(t, r, ActivityMerge, "t1", int64(9), "c1", "sebastian", "")
	res := getInto[MergeResult](t, val, err)
	if res.Merged || len(fj.merges) != 0 || !strings.Contains(res.Refusal, "the plan's revision 2 drops the card make-check") {
		t.Fatalf("merge = %+v, merges %d: a stale card merged", res, len(fj.merges))
	}
}

// The plan comment's criteria table names the id a criterion replaces.
func TestPlanCommentNamesTheReplacedCriterion(t *testing.T) {
	p := testPlan()
	p.Criteria[0].ID = "links_crud_v2"
	p.Criteria[0].Replaces = "links_crud"
	p.Cards[1].Acceptance[0].Criterion = "links_crud_v2"
	p.Cards[3].Acceptance[1].Criterion = "links_crud_v2"
	body := planComment(Task{ID: "t-plan", Objective: "Link shortener"}, 2, p, nil)
	if !strings.Contains(body, "links_crud_v2 (replaces links_crud)") {
		t.Fatalf("the comment hides the replaced id:\n%s", body)
	}
}
