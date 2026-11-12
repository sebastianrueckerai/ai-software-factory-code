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
		"the same id with another definition":                {changed, "criterion links_crud: revision 1 holds this id with another definition; a changed criterion files under a new id"},
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

// An id never changes meaning in any revision: a criterion revision 2
// dropped comes back in revision 3 only with the definition revision 1
// gave it, so its old rows never stand for another criterion.
func TestFilePlanRefusesADroppedIdBackWithAnotherDefinition(t *testing.T) {
	ctx := context.Background()
	withPages := func(claim string) Plan {
		p := testPlan()
		p.Criteria = append(p.Criteria, PlanCriterion{ID: "pages_ok", Kind: CriterionHuman, Claim: claim, Evidence: "the list page"})
		p.Cards[2].Acceptance = append(p.Cards[2].Acceptance, PlanItem{Criterion: "pages_ok", Text: "the list page reads well"})
		p.Cards[3].Acceptance = append(p.Cards[3].Acceptance, PlanItem{Criterion: "pages_ok", Text: "the pages read well"})
		return p
	}
	r, store, _, _ := newPlanRunner(t, withPages("the pages read well"))
	pinned := fixtureVersion(t)
	r.cfg.Policy = func() policy.Version { return pinned }
	runPlanTask(store)
	if res, err := r.FilePlan(ctx, "t-plan", planJSON(t, testPlan())); err != nil || len(res.Findings) != 0 || res.Revision != 2 {
		t.Fatalf("revision 2 drops pages_ok: %+v, %v, findings %q", res, err, res.Findings)
	}
	res, err := r.FilePlan(ctx, "t-plan", planJSON(t, withPages("the pages read calm")))
	want := []string{"criterion pages_ok: revision 1 holds this id with another definition; a changed criterion files under a new id"}
	if err != nil || !slices.Equal(res.Findings, want) {
		t.Fatalf("pages_ok back with a new claim: %+v, %v; want the findings %q", res, err, want)
	}
	if rev, _, _ := store.PlanRevision(ctx, "t-plan", 0); rev.Revision != 2 {
		t.Fatalf("a refused amendment landed as revision %d", rev.Revision)
	}
	if res, err := r.FilePlan(ctx, "t-plan", planJSON(t, withPages("the pages read well"))); err != nil || len(res.Findings) != 0 || res.Revision != 3 {
		t.Fatalf("pages_ok back as it was: %+v, %v, findings %q", res, err, res.Findings)
	}
}

// The night's loop: a planner re-files the newest revision itself, and
// the amendment rules judge it as an amendment of itself — its replaces
// markers name ids the newest no longer holds. A re-file of the newest
// by value is a no-op: the revision it already is, no records, no
// restarts. By value, not by hash: a stored row whose hash drifted still
// lands the no-op.
func TestFilePlanRefileOfTheNewestIsANoop(t *testing.T) {
	ctx := context.Background()
	r, store, _, _, wf := fileRig(t, testPlan())
	pinned := fixtureVersion(t)
	r.cfg.Policy = func() policy.Version { return pinned }

	amend := testPlan()
	amend.Criteria[0].ID = "links_crud_v2"
	amend.Criteria[0].Replaces = "links_crud"
	amend.Cards[1].Acceptance[0].Criterion = "links_crud_v2"
	amend.Cards[3].Acceptance[1].Criterion = "links_crud_v2"
	if res, err := r.FilePlan(ctx, "t-plan", planJSON(t, amend)); err != nil || len(res.Findings) != 0 || res.Revision != 2 {
		t.Fatalf("the amendment: %+v, %v, findings %q", res, err, res.Findings)
	}
	starts := len(wf.started)
	before, _ := store.PlanCatches(ctx, "t-plan")

	res, err := r.FilePlan(ctx, "t-plan", planJSON(t, amend))
	if err != nil || len(res.Findings) != 0 || res.Revision != 2 {
		t.Fatalf("the re-file: %+v, %v, findings %q", res, err, res.Findings)
	}
	if rev, _, _ := store.PlanRevision(ctx, "t-plan", 0); rev.Revision != 2 {
		t.Fatalf("the re-file landed as revision %d", rev.Revision)
	}
	if catches, _ := store.PlanCatches(ctx, "t-plan"); len(catches) != len(before) {
		t.Fatalf("the re-file wrote records: %+v", catches)
	}
	if len(wf.started) != starts {
		t.Fatalf("the re-file restarted children: %v", wf.started)
	}

	rev, _, _ := store.PlanRevision(ctx, "t-plan", 2)
	rev.SHA256 = "drifted"
	store.plans["t-plan"][1] = rev
	if res, err := r.FilePlan(ctx, "t-plan", planJSON(t, amend)); err != nil || len(res.Findings) != 0 || res.Revision != 2 {
		t.Fatalf("the re-file after the hash drifted: %+v, %v, findings %q", res, err, res.Findings)
	}
	if rev, _, _ := store.PlanRevision(ctx, "t-plan", 0); rev.Revision != 2 {
		t.Fatalf("the drifted re-file landed as revision %d", rev.Revision)
	}
}

// A revision that holds a criterion with the exact definition the
// previous one holds keeps its replaces marker: the marker is history,
// not a fresh replacement. The whole-plan no-op catches the identical
// re-file; a re-file that changes something else alongside (here the
// summary) walks this path and files.
func TestFilePlanUnchangedCriterionKeepsItsReplacesMarker(t *testing.T) {
	ctx := context.Background()
	r, store, _, _, _ := fileRig(t, testPlan())
	pinned := fixtureVersion(t)
	r.cfg.Policy = func() policy.Version { return pinned }

	amend := testPlan()
	amend.Criteria[0].ID = "links_crud_v2"
	amend.Criteria[0].Replaces = "links_crud"
	amend.Cards[1].Acceptance[0].Criterion = "links_crud_v2"
	amend.Cards[3].Acceptance[1].Criterion = "links_crud_v2"
	if res, err := r.FilePlan(ctx, "t-plan", planJSON(t, amend)); err != nil || len(res.Findings) != 0 || res.Revision != 2 {
		t.Fatalf("the amendment: %+v, %v, findings %q", res, err, res.Findings)
	}

	retold := amend
	retold.Summary = amend.Summary + " A card's brief changed with it."
	if res, err := r.FilePlan(ctx, "t-plan", planJSON(t, retold)); err != nil || len(res.Findings) != 0 || res.Revision != 3 {
		t.Fatalf("the re-file with the retold summary: %+v, %v, findings %q", res, err, res.Findings)
	}
	if rev, _, _ := store.PlanRevision(ctx, "t-plan", 0); rev.Revision != 3 {
		t.Fatalf("the re-file landed as revision %d", rev.Revision)
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
	if _, err := r.fileChildren(ctx, plan, repo, pinned, "spec", "plan #1", p, FactoryDecider, true, 1, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := cardsByKey(t, store)[p.Cards[1].Key]; got.ID == "" || got.Image != "" {
		t.Fatalf("card %s = %+v, want it filed naming no image", p.Cards[1].Key, got)
	}
}

// A revision's card whose from names an old card with no row of its own
// key starts from that card's work: it files at the old card's base, its
// branch is the old card's head before its issue exists, and its brief
// says so. A from whose branch is gone files with no base stamp, its first
// session's start at main's head, and says that instead, with no copy. A keyless from, the plan card's own work after a
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
		cards, err := r.fileChildren(ctx, plan, repo, pinned, "spec", "plan #40", Plan{Summary: "s", Cards: []PlanCard{c}}, FactoryDecider, true, 2, ReplanPerson, old)
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
	if card.SHA != "" || !strings.Contains(card.Objective, gone) || strings.Contains(card.Objective, "Starts from") {
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
	if _, err := r.fileChildren(ctx, plan, repo, pinned, "spec", "plan #1", filed, FactoryDecider, true, 1, "", old); err != nil {
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

// cardsFiledRows are the cards' filing rows in store, of every round.
func cardsFiledRows(store *fakeStore) []Event {
	var out []Event
	for _, e := range rowsOfKind(store, EventPlanReplanned) {
		if _, card := e.Detail["key"]; card {
			out = append(out, e)
		}
	}
	return out
}

// cardsFiledRound counts the cards' filing rows of one round in store.
func cardsFiledRound(store *fakeStore, round int) int {
	n := 0
	for _, e := range cardsFiledRows(store) {
		if e.Detail["round"] == round {
			n++
		}
	}
	return n
}

// revisionRows are the plan_replanned rows of revisions — a legacy row
// without provenance among them — the cards' filing rows aside.
func revisionRows(store *fakeStore) []Event {
	var out []Event
	for _, e := range rowsOfKind(store, EventPlanReplanned) {
		if _, card := e.Detail["key"]; !card {
			out = append(out, e)
		}
	}
	return out
}

// replanOrigins counts the plan_replanned rows of each origin in store.
func replanOrigins(store *fakeStore) map[string]int {
	out := map[string]int{}
	for _, e := range revisionRows(store) {
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
	for _, e := range cardsFiledRows(storeA) {
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
// legacy row recorded before the facts existed stays unknown. The
// legacy card is of the very round the failing filing files: kept by it,
// its missing provenance is never backfilled.
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
	replans := revisionRows(store)
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
	var fj *planForgejo
	r, store, _, fj, _ = fileRig(t, roundOne())
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
	// the round's last card stands as its row already is — its issue,
	// queued, and no fact row, filed before the rows were: the round's
	// filing keeps it, and its missing provenance stays unknown
	legacy := planCardTask(two.Cards[len(two.Cards)-1], 6, "queued", "")
	if err := store.CreateTask(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	fj.issues[6] = &planIssue{title: legacy.PlanKey, poster: "factory-agent", state: "open"}
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
	sink = &failingEvents{fakeStore: store, kind: EventPlanReplanned, fails: 1, err: errors.New("the sink is down")}
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
	rows := cardsFiledRows(store)
	if len(rows) != 4 {
		t.Fatalf("the cards' filing rows %d after the retry, want three of round 1 and one of round 2", len(rows))
	}
	if rows[3].Detail["round"] != 2 || rows[3].Detail["key"] != "stats" || rows[3].Detail["revision"] != 2 {
		t.Fatalf("the round-2 row %+v, want stats of round 2 at revision 2", rows[3])
	}
	for _, e := range rows {
		if e.Detail["key"] == "integration" {
			t.Fatalf("the legacy card the round's filing kept filed a row: %+v", e)
		}
	}
	// the filing runs again whole: no row and no revision multiplies
	if res, err := r.fileRevision(ctx, plan, repo, fixtureVersion(t), rev); err != nil || len(res.Refused) != 0 {
		t.Fatalf("the replayed filing: %+v, %v", res, err)
	}
	if rows := cardsFiledRows(store); len(rows) != 4 {
		t.Fatalf("the cards' filing rows %d after the replay, want 4", len(rows))
	}
	if rows := revisionRows(store); len(rows) != 1 || rows[0].Detail["origin"] != ReplanRound {
		t.Fatalf("the revision's plan_replanned rows %+v after the replay, want the round's alone", rows)
	}
	if rev, _, _ := store.PlanRevision(ctx, "t-plan", 0); rev.Revision != 2 {
		t.Fatalf("the replay stored revision %d, want 2", rev.Revision)
	}
}

// A filed revision leaves one plan_revision row, the one the bought-nothing
// guard reads as what a planner's draw bought; the same plan filed again
// is no new revision and leaves none.
func TestFilePlanRecordsTheRevisionOnce(t *testing.T) {
	ctx := context.Background()
	first := testPlan()
	first.Criteria = append(first.Criteria, PlanCriterion{ID: "pages_ok", Kind: CriterionHuman, Claim: "the pages read well", Evidence: "the list page"})
	first.Cards[2].Acceptance = append(first.Cards[2].Acceptance, PlanItem{Criterion: "pages_ok", Text: "the list page reads well"})
	first.Cards[3].Acceptance = append(first.Cards[3].Acceptance, PlanItem{Criterion: "pages_ok", Text: "the pages read well"})
	r, store, _, _ := newPlanRunner(t, first)
	pinned := fixtureVersion(t)
	r.cfg.Policy = func() policy.Version { return pinned }
	rec := &fakeEventRecorder{}
	r.timeline.Events = rec
	runPlanTask(store)
	for range 2 {
		if res, err := r.FilePlan(ctx, "t-plan", planJSON(t, testPlan())); err != nil || res.Revision != 2 {
			t.Fatalf("filing revision 2: %+v, %v", res, err)
		}
	}
	rows := 0
	for _, e := range rec.rows {
		if e.Kind == EventPlanRevision && e.TaskID == "t-plan" {
			rows++
		}
	}
	if rows != 1 {
		t.Fatalf("plan_revision rows = %d after filing revision 2 twice, want 1", rows)
	}
}

// A filing that differs from the stored plan by value but hashes to its
// sha (an empty list where the stored plan has none, which the JSON omits)
// is no new revision: the store returns the revision it already holds, and
// no plan_revision row is written for it.
func TestFilePlanRecordsNoRowForARevisionTheStoreAlreadyHolds(t *testing.T) {
	ctx := context.Background()
	r, store, _, _ := newPlanRunner(t, testPlan())
	pinned := fixtureVersion(t)
	r.cfg.Policy = func() policy.Version { return pinned }
	rec := &fakeEventRecorder{}
	r.timeline.Events = rec
	runPlanTask(store)
	var doc map[string]any
	if err := json.Unmarshal([]byte(planJSON(t, testPlan())), &doc); err != nil {
		t.Fatal(err)
	}
	doc["take_off_main"] = []string{}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.FilePlan(ctx, "t-plan", string(raw))
	if err != nil || res.Revision != 1 || len(res.Findings) != 0 {
		t.Fatalf("filing the same plan with an empty take_off_main: %+v, %v", res, err)
	}
	if len(rec.rows) != 0 {
		t.Fatalf("rows written for a revision already stored: %+v", rec.rows)
	}
}

// A card whose directories all sit under a workflow directory is filed as a
// human's card: no queued row, no start, no capability, and an ask that
// names the push rule. A card with one directory outside them, or with
// none, is filed as before.
func TestFileChildrenFilesAWorkflowOnlyCardForAHuman(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		dirs  []string
		human bool
	}{
		{"one workflow file", []string{".forgejo/workflows/pr.yml"}, true},
		{"two workflow directories", []string{".github/x.yml", ".gitea/y.yml"}, true},
		{"a workflow file and a source file", []string{".forgejo/workflows/pr.yml", "internal/x.go"}, false},
		{"no directories", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, store, _, _ := newPlanRunner(t, testPlan())
			wf := &fakeWorkflows{}
			r.cfg.Workflows = wf
			pinned := fixtureVersion(t)
			r.cfg.Policy = func() policy.Version { return pinned }
			p := Plan{Summary: "s", Cards: []PlanCard{{Key: "ci", Role: RoleFeature, Title: "Fix the CI", Brief: "Change the workflow.", Size: "small",
				Directories: tc.dirs, Acceptance: []PlanItem{{Text: "it runs"}}}}}
			plan, repo, err := r.task(ctx, "t-plan")
			if err != nil {
				t.Fatal(err)
			}
			cards, err := r.fileChildren(ctx, plan, repo, pinned, "spec", "plan #1", p, FactoryDecider, true, 1, "", nil)
			if err != nil || len(cards) != 1 {
				t.Fatalf("fileChildren = %+v, %v", cards, err)
			}
			got := store.tasks[cards[0].ID]
			var asks []HumanQuestion
			for _, e := range store.askedRows(got.ID) {
				if e.Question != nil {
					asks = append(asks, *e.Question)
				}
			}
			if !tc.human {
				if got.Kind != "agent" || got.Status != "queued" || len(asks) != 0 || !slices.Equal(wf.started, []string{got.ID}) {
					t.Fatalf("card = %+v, asks %v, started %q; want a queued agent card started, no ask", got, asks, wf.started)
				}
				return
			}
			if got.Kind != "human" || got.Status != "waiting-human" || got.Track != "" || len(wf.started) != 0 || got.EnforcedSpent() != 0 {
				t.Fatalf("card = %+v, started %q; want a human's card waiting, no start, no spend", got, wf.started)
			}
			if len(asks) != 1 || !strings.Contains(asks[0].Why, "push rule refuses changes to workflow files") ||
				!strings.Contains(asks[0].Info, "A human opens the pull request") || !strings.Contains(asks[0].Info, "Change the workflow.") {
				t.Fatalf("asks = %+v, want one that names the push rule, the human and the brief", asks)
			}
		})
	}
}

// A plan that holds a card of workflow paths only passes the validator and
// files that card for a human; the plan's other cards file and start as
// before, and a card that waits on the human's card starts after its
// Done. A card that mixes workflow paths with others is refused with a
// finding that names the way out.
func TestFilePlanFilesAWorkflowCardForAHuman(t *testing.T) {
	ctx := context.Background()
	r, store, _, _, wf := fileRig(t, testPlan())
	pinned := fixtureVersion(t)
	r.cfg.Policy = func() policy.Version { return pinned }

	mixed := testPlan()
	mixed.Cards[1].Directories = []string{".forgejo/workflows/pr.yml", "internal/links"}
	res, err := r.FilePlan(ctx, "t-plan", planJSON(t, mixed))
	want := []string{"plan: card links-api: directories mix workflow paths (.forgejo, .gitea, .github) with others: put the workflow paths in a card of their own, which a human does"}
	if err != nil || !slices.Equal(res.Findings, want) {
		t.Fatalf("a mixed card: findings %q, %v; want %q", res.Findings, err, want)
	}

	p := testPlan()
	ci := PlanCard{Key: "ci", Role: RoleFeature, Title: "Fix the pull request build", Brief: "Change the pr workflow.", Size: "small",
		Directories: []string{".forgejo/workflows/pr.yml"}, Acceptance: []PlanItem{{Criterion: "all_green", Text: "go test ./... passes with the new workflow"}}}
	p.Cards = slices.Insert(p.Cards, 1, ci)
	p.Cards[2].DependsOn = []string{"ci"}
	res, err = r.FilePlan(ctx, "t-plan", planJSON(t, p))
	if err != nil || len(res.Findings) != 0 {
		t.Fatalf("the plan: %+v, %v", res, err)
	}
	rev, _, _ := store.PlanRevision(ctx, "t-plan", 0)
	plan, repo, err := r.task(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	if filed, err := r.fileRevision(ctx, plan, repo, pinned, rev); err != nil || len(filed.Refused) != 0 || len(filed.Issues) != 5 {
		t.Fatalf("filing the revision: %+v, %v; want five issues", filed, err)
	}
	cards := cardsByKey(t, store)
	human := cards["ci"]
	if human.Kind != "human" || human.Status != "waiting-human" || human.Track != "" || slices.Contains(wf.started, human.ID) || human.EnforcedSpent() != 0 {
		t.Fatalf("the workflow card = %+v, started %q; want a human's card waiting, never started", human, wf.started)
	}
	for _, k := range []string{"foundation", "links-api", "redirects", "integration"} {
		if cards[k].Kind != "agent" || cards[k].Status != "queued" || !slices.Contains(wf.started, cards[k].ID) {
			t.Fatalf("card %s = %+v; want a queued agent card started", k, cards[k])
		}
	}
	if open, _ := store.OpenDependencies(ctx, cards["links-api"].ID); !slices.ContainsFunc(open, func(d Dependency) bool { return d.TaskID == human.ID }) {
		t.Fatalf("links-api waits on %+v, want the human's card among them", open)
	}

	for k, c := range cards {
		if k != "ci" {
			c.Status = "done"
			store.tasks[c.ID] = c
		}
	}
	if ok, open, err := PlanDelivered(ctx, store, "t-plan"); err != nil || ok || open.ID != human.ID {
		t.Fatalf("PlanDelivered with the human's card waiting = %v, %q, %v; want false on %s", ok, open.ID, err, human.ID)
	}
	human.Status = "done"
	store.tasks[human.ID] = human
	if open, _ := store.OpenDependencies(ctx, cards["links-api"].ID); len(open) != 0 {
		t.Fatalf("links-api still waits on %+v after the human's Done", open)
	}
	if ok, open, err := PlanDelivered(ctx, store, "t-plan"); err != nil || !ok {
		t.Fatalf("PlanDelivered after Done = %v, %q, %v; want true", ok, open.ID, err)
	}
}
