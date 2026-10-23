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

// Piece 10's recording rule and restart rules: an amendment that replaces
// links_crud under a new id, drops redirect_302 and its citing card, and
// rebriefs the foundation records each criterion's fate, closes the
// dropped card with its issue, rebriefs every card that never started,
// and leaves a started card and a final one alone.
func TestFilePlanAmendmentRecordsAndRestarts(t *testing.T) {
	ctx := context.Background()
	r, store, _, fj := newPlanRunner(t, testPlan())
	pinned := fixtureVersion(t)
	r.cfg.Policy = func() policy.Version { return pinned }
	if res := createChildren(t, r); len(res.Refused) != 0 || !slices.Equal(res.Issues, []int64{2, 3, 4, 5}) {
		t.Fatalf("the children: %+v", res)
	}
	runPlanTask(store)
	cards := cardsByKey(t, store)
	started := cards["links-api"]
	started.Status, started.PlanRevision = "running", 1
	store.tasks[started.ID] = started
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
	amend.Cards[1].Acceptance[0].Criterion = "links_crud_v2"
	amend.Cards[2].Acceptance = []PlanItem{amend.Cards[2].Acceptance[0], amend.Cards[2].Acceptance[1]}
	amend.Cards[2].Acceptance[1].Criterion = "links_crud_v2"
	res, err := r.FilePlan(ctx, "t-plan", planJSON(t, amend))
	if err != nil || len(res.Findings) != 0 || res.Revision != 2 {
		t.Fatalf("the amendment: %+v, %v, findings %q", res, err, res.Findings)
	}

	catch, _ := store.PlanCatches(ctx, "t-plan")
	if len(catch) != 2 || catch[0].Revision != 2 || catch[0].Kind != CatchCriterion || catch[0].Finder != FinderPlanner ||
		catch[0].Detail != "links_crud_v2 replaces links_crud" || catch[1].Detail != "redirect_302 removed" {
		t.Fatalf("the records: %+v", catch)
	}

	cards = cardsByKey(t, store)
	if c := cards["foundation"]; c.Status != "backlog" || !strings.Contains(c.Objective, "Foundation: layout, contract, schema, migrations.") {
		t.Fatalf("the never-started card kept its old brief:\n%s", c.Objective)
	}
	if c := cards["links-api"]; c.Objective != oldStarted || c.PlanRevision != 1 {
		t.Fatalf("the started card was touched: revision %d\n%s", c.PlanRevision, c.Objective)
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
}

// A card that never started takes the new revision's image with its
// brief; a started one keeps the image it runs on.
func TestFilePlanAmendmentMovesAnUnstartedCardsImage(t *testing.T) {
	ctx := context.Background()
	r, store, _, _ := newPlanRunner(t, testPlan())
	pinned := fixtureVersion(t)
	name, c, _ := pinned.ProjectOf("linkshort/linkshort")
	c.Images = map[string]policy.ImagePin{"browser": {Digest: "sha256:" + strings.Repeat("b", 64), Key: keyBrowser, Tools: []string{"chromium"}}}
	pinned.Projects[name] = c
	r.cfg.Policy = func() policy.Version { return pinned }
	if res := createChildren(t, r); len(res.Refused) != 0 {
		t.Fatalf("the children: %+v", res)
	}
	runPlanTask(store)
	started := cardsByKey(t, store)["redirects"]
	started.Status, started.PlanRevision = "running", 1
	store.tasks[started.ID] = started

	amend := testPlan()
	amend.Cards[1].Image, amend.Cards[2].Image = "browser", "browser"
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
	if _, err := r.fileChildren(ctx, plan, repo, pinned, "spec", "plan #1", p, FactoryDecider); err != nil {
		t.Fatal(err)
	}
	if got := cardsByKey(t, store)[p.Cards[1].Key]; got.ID == "" || got.Image != "" {
		t.Fatalf("card %s = %+v, want it filed naming no image", p.Cards[1].Key, got)
	}
}

// A card that never started takes the new revision's rung and size with
// its brief, which states them; a started one keeps its own.
func TestFilePlanAmendmentRefreshesAnUnstartedCardsRungAndSize(t *testing.T) {
	ctx := context.Background()
	r, store, _, _ := newPlanRunner(t, testPlan())
	pinned := fixtureVersion(t)
	r.cfg.Policy = func() policy.Version { return pinned }
	if res := createChildren(t, r); len(res.Refused) != 0 {
		t.Fatalf("the children: %+v", res)
	}
	runPlanTask(store)
	started := cardsByKey(t, store)["redirects"]
	started.Status, started.PlanRevision = "running", 1
	store.tasks[started.ID] = started

	amend := testPlan()
	for _, i := range []int{1, 2} {
		amend.Cards[i].Size, amend.Cards[i].Rung, amend.Cards[i].RungReason = "medium", RungFrontier, "the store's schema moves"
	}
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
		RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 2, Objective: "Links API"}
	kube, objects := newFakeKube(), &fakeObjectStore{}
	runner, _, store := newTestRunner(t, card, kube, objects)
	store.tasks["t-plan"] = Task{ID: "t-plan", Kind: "agent", Status: "done", Lane: policy.LanePlan, Process: "spec",
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
// stays for the plan's re-run (piece 10).
func TestStaleCardNeitherAsksNorMerges(t *testing.T) {
	ctx := context.Background()
	fj := &gateForgejo{mergeStatus: []int{200}}
	task := gateTask
	task.Source, task.Parent, task.PlanRevision = SourcePlan, "t-plan", 1
	r, store, _ := newGateRunner(t, task, fj)
	store.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary":"one"}`), "s1")
	store.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary":"two"}`), "s2")
	if err := r.RequestApproval(ctx, "t1", ApprovalInput{}); err == nil || !strings.Contains(err.Error(), "the plan moved to revision 2, the card ran against revision 1") ||
		!strings.Contains(err.Error(), "type: terminal") {
		t.Fatalf("RequestApproval: %v", err)
	}
	val, err := execActivity(t, r, ActivityMerge, "t1", int64(9), "c1", "sebastian", "")
	res := getInto[MergeResult](t, val, err)
	if res.Merged || len(fj.merges) != 0 || !strings.Contains(res.Refusal, "the plan moved to revision 2, the card ran against revision 1") {
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
