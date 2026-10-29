package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// criteriaPlan is a plan with one revision holding three criteria (a test
// one, a human one, an invariant one) and two cards, foundation and
// integration, as the verdict tests seed it.
func criteriaPlan() Plan {
	return Plan{
		Summary: "the plan",
		Criteria: []PlanCriterion{
			{ID: "c1", Kind: CriterionTest, Test: "go test ./..."},
			{ID: "c2", Kind: CriterionHuman, Claim: "the feature reads well"},
			{ID: "c3", Kind: CriterionInvariant, Test: "no endpoint times out"},
		},
		Cards: []PlanCard{
			{Key: "foundation", Role: "feature", Title: "Foundation", Directories: []string{"a"}},
			{Key: "integration", Role: RoleIntegration, Title: "Integration", Directories: []string{"a"}},
		},
	}
}

// seedCriteriaPlan stores a criteria plan on task plan with revision
// json, its integration card integrationID (status sets the card's, and
// every other card is done), and returns the store.
func seedVerdictPlan(t *testing.T, plan, integrationID, status string) *fakeStore {
	t.Helper()
	s := newFakeStore()
	b, err := json.Marshal(criteriaPlan())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPlanRevision(context.Background(), plan, b, "sha"); err != nil {
		t.Fatal(err)
	}
	s.tasks[plan] = Task{ID: plan, Repository: "sf/code", IssueNumber: 7, Source: SourceChat}
	s.tasks["card-foundation"] = Task{ID: "card-foundation", Repository: "sf/code", Parent: plan, Source: SourcePlan, PlanKey: "foundation", Status: "done"}
	s.tasks[integrationID] = Task{ID: integrationID, Repository: "sf/code", Parent: plan, Source: SourcePlan, PlanKey: "integration", Status: status}
	return s
}

func TestNormalizeCriteria(t *testing.T) {
	p := criteriaPlan()
	answer := []CriterionVerdict{
		{Criterion: "c1", Verdict: "met", Evidence: "go test ./... exits 0, 214 tests", Fix: "card"},
		{Criterion: "c2", Verdict: "met", Evidence: "the list page reads clean"},
		{Criterion: "c3", Verdict: "not-a-verdict", Evidence: "x"},
		{Criterion: "c9", Verdict: "met"},
		{Criterion: "c1", Verdict: "unmet"},
		{Criterion: "c3", Verdict: "unmet", Fix: "wizardry"},
	}
	rows, behaviours := normalizeCriteria(p, answer, []string{"  the door opens only for a key that fits  ", "", "   "})
	want := []CriterionVerdict{
		{Criterion: "c1", Verdict: "met", Evidence: "go test ./... exits 0, 214 tests", Fix: "card", Kind: CriterionTest},
		{Criterion: "c2", Verdict: "", Evidence: "the list page reads clean", Fix: "", Kind: CriterionHuman},
		// the invalid first c3 answer drops whole; the second answer is the row
		{Criterion: "c3", Verdict: "unmet", Fix: "", Kind: CriterionInvariant},
	}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Errorf("rows =\n%v\nwant\n%v", rows, want)
	}
	if fmt.Sprint(behaviours) != fmt.Sprint([]string{"the door opens only for a key that fits"}) {
		t.Errorf("behaviours = %v", behaviours)
	}
}

func TestEffectiveVerdictAndOpenCriteria(t *testing.T) {
	p := criteriaPlan()
	rows := []CriterionVerdict{
		{Criterion: "c1", Source: "review", Verdict: "met"},
		{Criterion: "c2", Source: "review", Verdict: ""},
		{Criterion: "c3", Source: "review", Verdict: "unmet"},
		{Criterion: "c3", Source: "approver", Verdict: "waived"},
	}
	for id, want := range map[string]string{"c1": "met", "c2": "", "c3": "waived", "c9": ""} {
		if got := effectiveVerdict(rows, id); got != want {
			t.Errorf("effectiveVerdict(%s) = %q, want %q", id, got, want)
		}
	}
	if open := openCriteria(p, rows); len(open) != 1 || open[0].ID != "c2" {
		t.Errorf("openCriteria = %v, want c2 alone", open)
	}
}

func TestDecideCriterion(t *testing.T) {
	ctx := context.Background()
	s := seedVerdictPlan(t, "t-plan", "t-int", "done")
	if err := DecideCriterion(ctx, s, "t-plan", "c1", "wizardry", "sebastian", "why"); err == nil {
		t.Error("a bogus verdict accepted")
	}
	if err := DecideCriterion(ctx, s, "t-plan", "c1", "met", "sebastian", "looks done"); !errors.Is(err, ErrNotHuman) {
		t.Errorf("met on a test criterion: %v, want ErrNotHuman", err)
	}
	if err := DecideCriterion(ctx, s, "t-plan", "c1", "waived", "sebastian", ""); err == nil {
		t.Error("empty reason accepted")
	}
	if err := DecideCriterion(ctx, s, "t-plan", "c1", "waived", "sebastian", strings.Repeat("x", 2001)); err == nil {
		t.Error("2001-character reason accepted")
	}
	if err := DecideCriterion(ctx, s, "t-plan", "c9", "waived", "sebastian", "not this one"); !errors.Is(err, ErrNoCriterion) {
		t.Errorf("unknown criterion: %v, want ErrNoCriterion", err)
	}
	if err := DecideCriterion(ctx, s, "t-none", "c1", "waived", "sebastian", "why"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown plan: %v, want ErrNotFound", err)
	}
	if err := DecideCriterion(ctx, s, "t-plan", "c3", "waived", "sebastian", "the timeout is the provider's SLA"); err != nil {
		t.Fatalf("waive c3: %v", err)
	}
	if err := DecideCriterion(ctx, s, "t-plan", "c2", "met", "sebastian", "I read it, it works"); err != nil {
		t.Fatalf("met on the human criterion: %v", err)
	}
	rows, err := s.CriteriaVerdicts(ctx, "t-int")
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(rows, func(a, b CriterionVerdict) int { return strings.Compare(a.Criterion, b.Criterion) })
	want := []CriterionVerdict{
		{Criterion: "c2", Verdict: "met", Evidence: "I read it, it works", Kind: CriterionHuman, Source: "approver", DecidedBy: "sebastian"},
		{Criterion: "c3", Verdict: "waived", Evidence: "the timeout is the provider's SLA", Kind: CriterionInvariant, Source: "approver", DecidedBy: "sebastian"},
	}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Errorf("rows =\n%v\nwant\n%v", rows, want)
	}
}

// The delta round's prompt shows a card's image and lists the project's
// pinned images as the planner's guide does, only when the project pins
// images.
func TestDeltaPromptListsTheProjectsImages(t *testing.T) {
	bare := fmt.Sprintf(deltaSystem, "")
	if got := deltaPrompt(nil); got != bare || strings.Contains(got, `"image"`) || !strings.Contains(got, `"rung_reason": "..."}}. `) {
		t.Fatalf("no pins: %q, want deltaSystem without the image field", got)
	}
	want := strings.Replace(bare, `"rung_reason": "..."}}`, `"rung_reason": "...", "image": "..."}}`, 1) +
		" Images: browser (chromium), video (ffmpeg); a card that needs one names it in image, any other card runs the default."
	if got := deltaPrompt(map[string]policy.ImagePin{"video": {Tools: []string{"ffmpeg"}}, "browser": {Tools: []string{"chromium"}}}); got != want {
		t.Fatalf("deltaPrompt =\n%q\nwant\n%q", got, want)
	}
}

func TestGrantCriteria(t *testing.T) {
	ctx := context.Background()
	s := seedVerdictPlan(t, "t-plan", "t-int", "done")
	if err := s.PutCriteriaVerdicts(ctx, "t-int", "review", FactoryDecider, []CriterionVerdict{
		{Criterion: "c1", Verdict: "met", Kind: CriterionTest},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := GrantCriteria(ctx, s, "t-plan", "sebastian", 7)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("grant covers %d criteria, want 2 (c2, c3)", n)
	}
	dues, err := s.PlanCatches(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	var details []string
	for _, c := range dues {
		if c.Kind == CatchDelta && c.Finder == FinderApprover {
			details = append(details, c.Detail)
		}
	}
	if fmt.Sprint(details) != fmt.Sprint([]string{"c2 grant 7", "c3 grant 7"}) {
		t.Errorf("dues = %v", details)
	}
	// every criterion met or waived grants nothing
	if err := s.PutCriteriaVerdicts(ctx, "t-int", "approver", "sebastian", []CriterionVerdict{
		{Criterion: "c2", Verdict: "met", Kind: CriterionHuman},
		{Criterion: "c3", Verdict: "waived", Kind: CriterionInvariant},
	}); err != nil {
		t.Fatal(err)
	}
	if n, err = GrantCriteria(ctx, s, "t-plan", "sebastian", 8); err != nil || n != 0 {
		t.Fatalf("second grant = %d, %v; want 0, nil", n, err)
	}
}

func TestRecordHumanCriteria(t *testing.T) {
	ctx := context.Background()
	s := seedVerdictPlan(t, "t-plan", "t-int", "done")
	n, err := RecordHumanCriteria(ctx, s, "t-int", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("recorded %d criteria, want 1 (c2 alone)", n)
	}
	rows, _ := s.CriteriaVerdicts(ctx, "t-int")
	want := []CriterionVerdict{{Criterion: "c2", Verdict: "met", Evidence: "merge approved by sebastian", Kind: CriterionHuman, Source: "approver", DecidedBy: "sebastian"}}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Errorf("rows =\n%v\nwant\n%v", rows, want)
	}
	if n, err := RecordHumanCriteria(ctx, s, "card-foundation", "sebastian"); err != nil || n != 0 {
		t.Errorf("a foundation card records %d, %v; want 0, nil", n, err)
	}
}

// featurePlan is a plan with the criteria c1 and c2 and three cards: A
// cites c1, B cites c2, and I, the integration card, cites both.
func featurePlan() Plan {
	return Plan{
		Summary: "the feature",
		Criteria: []PlanCriterion{
			{ID: "c1", Kind: CriterionTest, Test: "TestA in ./a"},
			{ID: "c2", Kind: CriterionTest, Test: "TestB in ./b"},
		},
		Cards: []PlanCard{
			{Key: "A", Role: RoleFeature, Title: "Card A title", Brief: "build a", Acceptance: []PlanItem{{Criterion: "c1", Text: "go test ./a"}}},
			{Key: "B", Role: RoleFeature, Title: "Card B title", Brief: "build b", Acceptance: []PlanItem{{Criterion: "c2", Text: "go test ./b"}}},
			{Key: "I", Role: RoleIntegration, Title: "Card I title", Brief: "wire a and b",
				Acceptance: []PlanItem{{Criterion: "c1", Text: "go test ./a"}, {Text: "an extra check"}, {Criterion: "c2", Text: "go test ./b"}}},
		},
	}
}

// markedFile is a new file's diff that defines function fn and adds the
// comment line marker, which only the diff text holds.
func markedFile(path, fn, marker string) string {
	return fmt.Sprintf("diff --git a/%s b/%s\n--- /dev/null\n+++ b/%s\n@@ -0,0 +1,2 @@\n+func %s() {}\n+// %s\n", path, path, path, fn, marker)
}

// featureRunner is a gate runner whose task t1 is card key of plan t-plan
// (featurePlan) with pull request 9. cards files the other cards, the
// plan's and those no plan revision holds, by key with their pull
// request's number; diffs are the pull requests' diffs by number.
func featureRunner(t *testing.T, key string, cards map[string]int64, diffs map[int64]string) (*Runner, *fakeStore) {
	t.Helper()
	task := gateTask
	task.Source, task.Parent, task.PlanKey, task.IssueNumber = SourcePlan, "t-plan", key, 9
	r, store, _ := newGateRunner(t, task, &gateForgejo{diffs: diffs})
	b, err := json.Marshal(featurePlan())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPlanRevision(context.Background(), "t-plan", b, "sha"); err != nil {
		t.Fatal(err)
	}
	store.tasks["t-plan"] = Task{ID: "t-plan", Repository: "sf/repo", IssueNumber: 7, Source: SourceChat}
	for k, pr := range cards {
		store.tasks["t-"+k] = Task{ID: "t-" + k, Repository: "sf/repo", Parent: "t-plan", Source: SourcePlan, PlanKey: k, PRNumber: pr,
			Objective: "Card " + k + " title\n\nthe card's body", Status: "done"}
	}
	return r, store
}

// featureDiffs are the diffs of I (9), A (11) and B (12), one marked file
// each.
var featureDiffs = map[int64]string{
	9:  markedFile("i.go", "FromI", "MARK-I"),
	11: markedFile("a.go", "FromA", "MARK-A"),
	12: markedFile("b.go", "FromB", "MARK-B"),
}

// callOf names a recorded call by its input: review, the criterion's id,
// or behaviours.
func callOf(user string) string {
	var in struct {
		Criterion *PlanCriterion  `json:"criterion"`
		Criteria  json.RawMessage `json:"criteria"`
	}
	json.Unmarshal([]byte(user), &in)
	switch {
	case in.Criterion != nil:
		return in.Criterion.ID
	case in.Criteria != nil:
		return "behaviours"
	}
	return "review"
}

// featureAnswer answers the card's own review approve, c1 met, c2 unmet
// with fix card, and the behaviours with one sentence.
func featureAnswer(user string) string {
	switch callOf(user) {
	case "c1":
		return `{"verdict":"met","evidence":"TestA covers a.go","fix":"card"}`
	case "c2":
		return `{"verdict":"unmet","evidence":"b.go never registers its route","fix":"card"}`
	case "behaviours":
		return `{"behaviours":["a request to /a answers from FromA"]}`
	}
	return `{"verdict":"approve","summary":"the wiring is sound","strict_improvement":true}`
}

// featureCalls are the recorded calls' user messages by callOf.
func featureCalls(ll *fakeLiteLLM) map[string][]string {
	out := map[string][]string{}
	for _, chat := range ll.chats {
		user := userMessage(chat)
		out[callOf(user)] = append(out[callOf(user)], user)
	}
	return out
}

// The integration card's review reads its own diff in one call, then each
// criterion in a call of its own over the diffs of the cards that cite it
// and its own, then the behaviours: four calls. The verdicts land as the
// integration card's rows, and the unmet one is a finding.
func TestIntegrationReviewMakesOneCallPerCriterion(t *testing.T) {
	r, store := featureRunner(t, "I", map[string]int64{"A": 11, "B": 12}, featureDiffs)
	ll := &fakeLiteLLM{answer: featureAnswer}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	val, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	rv := getInto[Review](t, val, err)
	calls := featureCalls(ll)
	if len(ll.chats) != 4 || len(calls["review"]) != 1 || len(calls["c1"]) != 1 || len(calls["c2"]) != 1 || len(calls["behaviours"]) != 1 {
		t.Fatalf("%d calls, by kind %v: want the own review, c1, c2 and behaviours", len(ll.chats), slices.Sorted(maps.Keys(calls)))
	}
	if own := calls["review"][0]; !strings.Contains(own, "+++ b/i.go") || strings.Contains(own, "a.go") || strings.Contains(own, "b.go") || strings.Contains(own, `"criteria"`) {
		t.Fatalf("the own review reads more than its own diff: %s", own)
	}
	for id, want := range map[string]struct{ in, out string }{"c1": {"a.go", "b.go"}, "c2": {"b.go", "a.go"}} {
		user := calls[id][0]
		if !strings.Contains(user, "+++ b/"+want.in) || !strings.Contains(user, "+++ b/i.go") || strings.Contains(user, want.out) {
			t.Fatalf("call %s reads %s and i.go and not %s? %s", id, want.in, want.out, user)
		}
	}
	rows, _ := store.CriteriaVerdicts(context.Background(), "t1")
	want := []CriterionVerdict{
		{Criterion: "c1", Verdict: "met", Evidence: "TestA covers a.go", Fix: "card", Kind: CriterionTest, Source: "review", DecidedBy: FactoryDecider},
		{Criterion: "c2", Verdict: "unmet", Evidence: "b.go never registers its route", Fix: "card", Kind: CriterionTest, Source: "review", DecidedBy: FactoryDecider},
	}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("rows =\n%v\nwant\n%v", rows, want)
	}
	if rv.Verdict != "concerns" || !slices.Contains(rv.Findings, "criterion c2 not met (unmet): b.go never registers its route") {
		t.Fatalf("review = %+v, want concerns with the c2 finding", rv)
	}
}

// The behaviours call reads every card's files, the names they add and
// the cards' titles, and no line of any diff.
func TestBehavioursCallReadsNoDiffText(t *testing.T) {
	r, _ := featureRunner(t, "I", map[string]int64{"A": 11, "B": 12}, featureDiffs)
	ll := &fakeLiteLLM{answer: featureAnswer}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	val, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	if rv := getInto[Review](t, val, err); !slices.Equal(rv.Behaviours, []string{"a request to /a answers from FromA"}) {
		t.Fatalf("review = %+v, want the behaviour", rv)
	}
	calls := featureCalls(ll)["behaviours"]
	if len(calls) != 1 {
		t.Fatalf("%d behaviours calls, want 1", len(calls))
	}
	user := calls[0]
	for _, want := range []string{"a.go +2 -0 (card 1)", "b.go +2 -0 (card 2)", "i.go +2 -0 (card 3)", `"names":"functions: FromA, FromB, FromI"`, "Card A title", "Card B title", "Card I title"} {
		if !strings.Contains(user, want) {
			t.Fatalf("the behaviours call lacks %q: %s", want, user)
		}
	}
	for _, marker := range []string{"MARK-A", "MARK-B", "MARK-I"} {
		if strings.Contains(user, marker) {
			t.Fatalf("the behaviours call reads diff text %s: %s", marker, user)
		}
	}
}

// A's 130 KiB do not fit one part of 48 KiB: c1 reads in parts, each named,
// and the worst part's verdict stands.
func TestCriterionOverOnePartReadsInParts(t *testing.T) {
	diffs := maps.Clone(featureDiffs)
	diffs[11] = hunkFile("a.go", 13, 10<<10).Text
	r, store := featureRunner(t, "I", map[string]int64{"A": 11, "B": 12}, diffs)
	ll := &fakeLiteLLM{answer: func(user string) string {
		var in struct {
			Part string `json:"part"`
		}
		json.Unmarshal([]byte(user), &in)
		if callOf(user) == "c1" {
			if strings.HasSuffix(in.Part, ", part 2 of 4") {
				return `{"verdict":"unmet","evidence":"a.go drops the empty input","fix":"card"}`
			}
			return `{"verdict":"met","evidence":"this part holds no gap","fix":"card"}`
		}
		return featureAnswer(user)
	}}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	val, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	if rv := getInto[Review](t, val, err); rv.Note != "" {
		t.Fatalf("review = %+v", rv)
	}
	calls := featureCalls(ll)["c1"]
	if len(calls) < 3 {
		t.Fatalf("%d c1 calls, want 3 or more", len(calls))
	}
	labelled := regexp.MustCompile(`, part \d+ of 4$`)
	for _, user := range calls {
		var in struct {
			Part  string `json:"part"`
			Files string `json:"files"`
		}
		json.Unmarshal([]byte(user), &in)
		if !labelled.MatchString(in.Part) || !strings.Contains(in.Files, "i.go +2 -0 (part 4)") {
			t.Fatalf("a c1 call without its part label or the file list: part %q, files %q", in.Part, in.Files)
		}
	}
	rows, _ := store.CriteriaVerdicts(context.Background(), "t1")
	if len(rows) != 2 || rows[0].Criterion != "c1" || rows[0].Verdict != "unmet" {
		t.Fatalf("rows = %v, want c1 unmet, the worst of its parts", rows)
	}
}

// A card no plan revision holds arrived by a delta round: its review
// answers the criteria, and every criterion call reads every delta card's
// diff, D's here.
func TestDeltaCardReviewGetsEveryCriterionCall(t *testing.T) {
	diffs := map[int64]string{
		9:  markedFile("e.go", "FromE", "MARK-E"),
		11: featureDiffs[11], 12: featureDiffs[12], 13: featureDiffs[9],
		14: markedFile("d.go", "FromD", "MARK-D"),
	}
	r, _ := featureRunner(t, "E", map[string]int64{"A": 11, "B": 12, "I": 13, "D": 14}, diffs)
	ll := &fakeLiteLLM{answer: featureAnswer}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	val, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	if rv := getInto[Review](t, val, err); rv.Note != "" {
		t.Fatalf("review = %+v", rv)
	}
	calls := featureCalls(ll)
	for _, id := range []string{"c1", "c2"} {
		if len(calls[id]) != 1 || !strings.Contains(calls[id][0], "+++ b/d.go") || !strings.Contains(calls[id][0], "+++ b/e.go") {
			t.Fatalf("call %s does not read the delta card D's diff and its own: %v", id, calls[id])
		}
	}
}

// A call without an answer is the review's note, named by its call.
func TestIntegrationReviewNamesTheCallWithoutAnAnswer(t *testing.T) {
	for _, call := range []string{"c2", "behaviours"} {
		t.Run(call, func(t *testing.T) {
			r, store := featureRunner(t, "I", map[string]int64{"A": 11, "B": 12}, featureDiffs)
			ll := &fakeLiteLLM{answer: func(user string) string {
				if callOf(user) == call {
					return "LGTM"
				}
				return featureAnswer(user)
			}}
			r.supervisor = testSupervisorModel(ll.srv(t).URL)
			val, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{})
			want := map[string]string{"c2": "criterion c2: model review answer invalid", "behaviours": "behaviours: model review answer invalid"}[call]
			if rv := getInto[Review](t, val, err); rv.Verdict != "" || !strings.HasPrefix(rv.Note, want) {
				t.Fatalf("review = %+v, want a note starting %q", rv, want)
			}
			if rows, _ := store.CriteriaVerdicts(context.Background(), "t1"); len(rows) != 0 {
				t.Fatalf("rows = %v, want none without every answer", rows)
			}
		})
	}
}

// A task at its budget cap: every call is refused, and the note is the
// own review's, which reads one part and so names none. No verdict row is
// written.
func TestIntegrationReviewBudgetRefusalIsANote(t *testing.T) {
	r, store := featureRunner(t, "I", map[string]int64{"A": 11, "B": 12}, featureDiffs)
	ll := &fakeLiteLLM{status: http.StatusTooManyRequests, decision: policy.RefusedBudget}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	val, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	rv := getInto[Review](t, val, err)
	if rv.Verdict != "" || !strings.HasPrefix(rv.Note, "model review refused by the gateway (refused_budget)") {
		t.Fatalf("review = %+v", rv)
	}
	if rows, _ := store.CriteriaVerdicts(context.Background(), "t1"); len(rows) != 0 {
		t.Fatalf("rows = %v, want none", rows)
	}
}

// A feature card's diff past the 8 MiB bound, or the feature's diffs past
// it together (5 MiB and 5 MiB beside the card's own): the review notes
// it, makes no call and writes no row.
func TestIntegrationReviewOfAFeaturePastTheBoundIsANote(t *testing.T) {
	for name, tc := range map[string]struct {
		a, b, want string
	}{
		"one card": {strings.Repeat("d", 8<<20+1), featureDiffs[12],
			"model review not run: card A: the diff is longer than 8 MiB, the most factoryd reads: review the change yourself, or split it"},
		"together": {diffFile("a.go", 5<<20), diffFile("b.go", 5<<20),
			"model review not run: the feature's cards together: the diff is longer than 8 MiB, the most factoryd reads: review the change yourself, or split it"},
	} {
		t.Run(name, func(t *testing.T) {
			r, store := featureRunner(t, "I", map[string]int64{"A": 11, "B": 12}, map[int64]string{9: featureDiffs[9], 11: tc.a, 12: tc.b})
			ll := &fakeLiteLLM{answer: featureAnswer}
			r.supervisor = testSupervisorModel(ll.srv(t).URL)
			val, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{})
			if rv := getInto[Review](t, val, err); rv.Verdict != "" || rv.Note != tc.want || len(ll.chats) != 0 {
				t.Fatalf("review = %+v after %d calls, want the note %q", rv, len(ll.chats), tc.want)
			}
			if rows, _ := store.CriteriaVerdicts(context.Background(), "t1"); len(rows) != 0 {
				t.Fatalf("rows = %v, want none", rows)
			}
		})
	}
}

// A criterion whose one call answers absent is stored unmet, a finding on
// the card: absent never reaches a row.
func TestIntegrationReviewAbsentIsUnmet(t *testing.T) {
	r, store := featureRunner(t, "I", map[string]int64{"A": 11, "B": 12}, featureDiffs)
	ll := &fakeLiteLLM{answer: func(user string) string {
		if callOf(user) == "c1" {
			return `{"verdict":"absent","evidence":"no test of a","fix":"card"}`
		}
		return featureAnswer(user)
	}}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	val, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	rv := getInto[Review](t, val, err)
	evidence := "no part of the diff holds the criterion's work: no test of a"
	rows, _ := store.CriteriaVerdicts(context.Background(), "t1")
	want := []CriterionVerdict{
		{Criterion: "c1", Verdict: "unmet", Evidence: evidence, Fix: "card", Kind: CriterionTest, Source: "review", DecidedBy: FactoryDecider},
		{Criterion: "c2", Verdict: "unmet", Evidence: "b.go never registers its route", Fix: "card", Kind: CriterionTest, Source: "review", DecidedBy: FactoryDecider},
	}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("rows =\n%v\nwant\n%v", rows, want)
	}
	if !slices.Contains(rv.Findings, "criterion c1 not met (unmet): "+evidence) {
		t.Fatalf("findings = %q, want c1's", rv.Findings)
	}
}

// The criterion prompt offers absent and says what met and absent mean.
func TestCriterionPromptOffersAbsent(t *testing.T) {
	for _, want := range []string{`"verdict": "met|partial|unmet|absent"`, "met when it shows work that meets the criterion", "absent when it holds none of the criterion's work"} {
		if !strings.Contains(reviewCriterionPrompt, want) {
			t.Errorf("the criterion prompt lacks %q", want)
		}
	}
}

func TestCitingListsCardsByAcceptance(t *testing.T) {
	p := featurePlan()
	for id, want := range map[string][]string{"c1": {"A", "I"}, "c2": {"B", "I"}, "c9": nil} {
		if got := citing(p, id); !slices.Equal(got, want) {
			t.Errorf("citing(%s) = %v, want %v", id, got, want)
		}
	}
}

func TestCombineCriterion(t *testing.T) {
	a, b := strings.Repeat("a", 600), strings.Repeat("b", 600)
	for name, tc := range map[string]struct {
		parts []CriterionVerdict
		want  CriterionVerdict
	}{
		// a met part's evidence and fix count only when the result is met
		"met, partial": {[]CriterionVerdict{{Verdict: "met", Evidence: "x"}, {Verdict: "partial", Evidence: "y"}},
			CriterionVerdict{Criterion: "c1", Verdict: "partial", Evidence: "y"}},
		"met, unmet, partial": {[]CriterionVerdict{{Verdict: "met", Evidence: "x"}, {Verdict: "unmet", Evidence: "y"}, {Verdict: "partial", Evidence: "z"}},
			CriterionVerdict{Criterion: "c1", Verdict: "unmet", Evidence: "y | z"}},
		"met, unmet": {[]CriterionVerdict{{Verdict: "met", Evidence: "x"}, {Verdict: "unmet", Evidence: "y"}},
			CriterionVerdict{Criterion: "c1", Verdict: "unmet", Evidence: "y"}},
		"met planner, unmet card": {[]CriterionVerdict{{Verdict: "met", Fix: "planner", Evidence: "x"}, {Verdict: "unmet", Fix: "card", Evidence: "y"}},
			CriterionVerdict{Criterion: "c1", Verdict: "unmet", Fix: "card", Evidence: "y"}},
		// absent is no work in a part: work elsewhere stands, none anywhere is unmet
		"absent, absent": {[]CriterionVerdict{{Verdict: "absent"}, {Verdict: "absent"}},
			CriterionVerdict{Criterion: "c1", Verdict: "unmet", Evidence: "no part of the diff holds the criterion's work"}},
		"absent with evidence": {[]CriterionVerdict{{Verdict: "absent", Fix: "card", Evidence: "p"}, {Verdict: "absent", Evidence: "q"}},
			CriterionVerdict{Criterion: "c1", Verdict: "unmet", Fix: "card", Evidence: "no part of the diff holds the criterion's work: p | q"}},
		"one part absent": {[]CriterionVerdict{{Verdict: "absent", Evidence: "p"}},
			CriterionVerdict{Criterion: "c1", Verdict: "unmet", Evidence: "no part of the diff holds the criterion's work: p"}},
		"absent, met": {[]CriterionVerdict{{Verdict: "absent", Fix: "planner", Evidence: "p"}, {Verdict: "met", Evidence: "x"}},
			CriterionVerdict{Criterion: "c1", Verdict: "met", Evidence: "x"}},
		"absent, partial": {[]CriterionVerdict{{Verdict: "absent", Evidence: "p"}, {Verdict: "partial", Fix: "card", Evidence: "y"}},
			CriterionVerdict{Criterion: "c1", Verdict: "partial", Fix: "card", Evidence: "y"}},
		"card, planner": {[]CriterionVerdict{{Verdict: "unmet", Fix: "card", Evidence: "x"}, {Verdict: "partial", Fix: "planner", Evidence: "y"}},
			CriterionVerdict{Criterion: "c1", Verdict: "unmet", Fix: "planner", Evidence: "x | y"}},
		"planner, card": {[]CriterionVerdict{{Verdict: "unmet", Fix: "planner", Evidence: "x"}, {Verdict: "unmet", Fix: "card", Evidence: "y"}},
			CriterionVerdict{Criterion: "c1", Verdict: "unmet", Fix: "planner", Evidence: "x | y"}},
		"human":      {[]CriterionVerdict{{Evidence: "x"}, {Evidence: "y"}}, CriterionVerdict{Criterion: "c1", Evidence: "x | y"}},
		"over a cap": {[]CriterionVerdict{{Verdict: "met", Evidence: a}, {Verdict: "met", Evidence: b}}, CriterionVerdict{Criterion: "c1", Verdict: "met", Evidence: a + " | " + b[:397]}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := combineCriterion("c1", tc.parts); got != tc.want {
				t.Fatalf("combineCriterion =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

func TestParseCriterionAnswer(t *testing.T) {
	test, human := PlanCriterion{ID: "c1", Kind: CriterionTest}, PlanCriterion{ID: "c2", Kind: CriterionHuman}
	for name, tc := range map[string]struct {
		c       PlanCriterion
		content string
		want    CriterionVerdict
		problem string
	}{
		"valid": {test, `{"verdict":"unmet","evidence":" TestA fails on empty input ","fix":"planner"}`,
			CriterionVerdict{Criterion: "c1", Verdict: "unmet", Evidence: "TestA fails on empty input", Fix: "planner"}, ""},
		"fenced": {test, "```json\n{\"verdict\":\"met\",\"evidence\":\"TestA passes\",\"fix\":\"card\"}\n```",
			CriterionVerdict{Criterion: "c1", Verdict: "met", Evidence: "TestA passes", Fix: "card"}, ""},
		"absent": {test, `{"verdict":"absent","evidence":"this part adds no handler","fix":"card"}`,
			CriterionVerdict{Criterion: "c1", Verdict: "absent", Evidence: "this part adds no handler", Fix: "card"}, ""},
		"unknown verdict": {test, `{"verdict":"done","evidence":"x","fix":"card"}`, CriterionVerdict{}, `unknown verdict "done"`},
		"no verdict":      {test, `{"evidence":"x"}`, CriterionVerdict{}, `unknown verdict ""`},
		"not json":        {test, "LGTM", CriterionVerdict{}, "invalid character"},
		"extra field":     {test, `{"verdict":"met","evidence":"x","criterion":"c1"}`, CriterionVerdict{}, `unknown field "criterion"`},
		"human keeps evidence": {human, `{"verdict":"met","evidence":"open the list page","fix":"card"}`,
			CriterionVerdict{Criterion: "c2", Evidence: "open the list page"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, problem := parseCriterionAnswer(tc.content, tc.c)
			if tc.problem != "" {
				if !strings.Contains(problem, tc.problem) {
					t.Fatalf("problem = %q, want it to name %q", problem, tc.problem)
				}
				return
			}
			if problem != "" || got != tc.want {
				t.Fatalf("parseCriterionAnswer = %+v, %q; want %+v", got, problem, tc.want)
			}
		})
	}
}

func TestParseBehavioursAnswer(t *testing.T) {
	if got, problem := parseBehavioursAnswer(`{"behaviours":["a link redirects with 302","a missing link answers 404"]}`); problem != "" ||
		!slices.Equal(got, []string{"a link redirects with 302", "a missing link answers 404"}) {
		t.Fatalf("parseBehavioursAnswer = %v, %q", got, problem)
	}
	if _, problem := parseBehavioursAnswer(`{"behaviours":["x"],"verdict":"met"}`); !strings.Contains(problem, `unknown field "verdict"`) {
		t.Fatalf("problem = %q, want the unknown field named", problem)
	}
}

func TestReviewCriteriaStepFilesDuesAndFindings(t *testing.T) {
	ctx := context.Background()
	s := seedVerdictPlan(t, "t-plan", "t-int", "in-review")
	r := &Runner{store: s}
	rv := Review{Verdict: "approve", Summary: "the diff is sound", Criteria: []CriterionVerdict{
		{Criterion: "c1", Verdict: "met", Evidence: "go test exits 0", Fix: "card"},
		{Criterion: "c2", Evidence: "the list page reads clean"},
		{Criterion: "c3", Verdict: "unmet", Evidence: "the endpoint times out at 30 s", Fix: "planner"},
	}, Behaviours: []string{"the door opens only for a key that fits"}}
	r.reviewCriteriaStep(ctx, s.tasks["t-int"], Repo{}, criteriaPlan(), 1, 2, &rv)
	rows, _ := s.CriteriaVerdicts(ctx, "t-int")
	if len(rows) != 3 {
		t.Fatalf("rows = %v, want 3", rows)
	}
	catches, _ := s.PlanCatches(ctx, "t-plan")
	var deltas []PlanCatch
	behaviours := 0
	for _, c := range catches {
		switch c.Kind {
		case CatchDelta:
			deltas = append(deltas, c)
		case CatchBehaviour:
			if c.Detail == "the door opens only for a key that fits" {
				behaviours++
			}
		}
	}
	if len(deltas) != 1 || deltas[0].Finder != FinderReview || deltas[0].Detail != "c3 at integration round 2" {
		t.Errorf("dues = %+v, want one c3 planner due naming the card and round", deltas)
	}
	if behaviours != 1 {
		t.Errorf("behaviour rows = %d, want 1 (%+v)", behaviours, catches)
	}
	// the human criterion keeps no verdict; a fix of card with unmet was
	// not asked, so the approve verdict stands unless a finding lands
	if len(rv.Findings) != 0 {
		t.Errorf("findings = %v, want none (the planner due is not a finding)", rv.Findings)
	}
}

func TestPlanCloseCheckClosesAndGaps(t *testing.T) {
	ctx := context.Background()
	s := seedVerdictPlan(t, "t-plan", "t-int", "done")

	// every criterion met or waived closes the plan once
	if err := s.PutCriteriaVerdicts(ctx, "t-int", "review", FactoryDecider, []CriterionVerdict{
		{Criterion: "c1", Verdict: "met", Kind: CriterionTest},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCriteriaVerdicts(ctx, "t-int", "approver", "sebastian", []CriterionVerdict{
		{Criterion: "c2", Verdict: "met", Kind: CriterionHuman},
		{Criterion: "c3", Verdict: "waived", Kind: CriterionInvariant},
	}); err != nil {
		t.Fatal(err)
	}
	objs := &fakeObjectStore{}
	r := &Runner{store: s, timeline: &Timeline{Objects: objs, Events: s, Store: s}}
	r.CriteriaPass(ctx)
	body, err := objs.Get(ctx, "sessions/t-plan/timeline.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(body), `"event":"`+EventPlanClosed+`"`); got != 1 {
		t.Fatalf("plan_closed recorded %d times, want 1:\n%s", got, body)
	}
	r.CriteriaPass(ctx)
	if body, _ = objs.Get(ctx, "sessions/t-plan/timeline.jsonl"); strings.Count(string(body), `"event":"`+EventPlanClosed+`"`) != 1 {
		t.Error("the close recorded again")
	}

	// a plan whose cards are done with a criterion open asks its gap once
	s2 := seedVerdictPlan(t, "t-gap", "t-gap-int", "done")
	r2 := &Runner{store: s2, timeline: &Timeline{Objects: &fakeObjectStore{}}}
	if err := s2.PutCriteriaVerdicts(ctx, "t-gap-int", "review", FactoryDecider, []CriterionVerdict{
		{Criterion: "c1", Verdict: "met", Kind: CriterionTest},
		{Criterion: "c2", Evidence: "read it"},
		{Criterion: "c3", Verdict: "unmet", Evidence: "times out", Fix: "planner"},
	}); err != nil {
		t.Fatal(err)
	}
	r2.CriteriaPass(ctx)
	pending, _ := s2.ApprovalRequests(ctx, "pending")
	if len(pending) != 1 || pending[0].Kind != ApprovalCriteria || pending[0].TaskID != "t-gap" {
		t.Fatalf("pending = %+v, want one criteria request of t-gap", pending)
	}
	want := "criteria not met with the plan's cards done: c2, c3"
	if pending[0].Reason != want {
		t.Errorf("reason = %q, want %q", pending[0].Reason, want)
	}
	var spec CriteriaSpec
	if err := json.Unmarshal(pending[0].Spec, &spec); err != nil || spec.Task != "t-gap-int" {
		t.Errorf("spec = %s (%v)", pending[0].Spec, err)
	}
	// the same gap never asks again; a denied one with the same reason
	// never reopens
	r2.CriteriaPass(ctx)
	if pending, _ = s2.ApprovalRequests(ctx, "pending"); len(pending) != 1 {
		t.Fatalf("the gap asked again: %+v", pending)
	}
	if err := s2.DecideApproval(ctx, pending[0].ID, ApprovalCriteria, "denied", "sebastian", want); err != nil {
		t.Fatal(err)
	}
	r2.CriteriaPass(ctx)
	if pending, _ = s2.ApprovalRequests(ctx, "pending"); len(pending) != 0 {
		t.Fatalf("the denied gap re-opened: %+v", pending)
	}
	// a changed gap (another criterion still open) asks again
	if err := s2.PutCriteriaVerdicts(ctx, "t-gap-int", "approver", "sebastian", []CriterionVerdict{
		{Criterion: "c2", Verdict: "met", Kind: CriterionHuman},
	}); err != nil {
		t.Fatal(err)
	}
	r2.CriteriaPass(ctx)
	if pending, _ = s2.ApprovalRequests(ctx, "pending"); len(pending) != 1 || pending[0].Reason != "criteria not met with the plan's cards done: c3" {
		t.Fatalf("the changed gap did not re-open: %+v", pending)
	}

	// a card still running asks nothing
	s3 := seedVerdictPlan(t, "t-run", "t-run-int", "in-review")
	if err := s3.PutCriteriaVerdicts(ctx, "t-run-int", "review", FactoryDecider, []CriterionVerdict{
		{Criterion: "c1", Verdict: "unmet", Evidence: "x", Kind: CriterionTest},
	}); err != nil {
		t.Fatal(err)
	}
	(&Runner{store: s3, timeline: &Timeline{Objects: &fakeObjectStore{}}}).CriteriaPass(ctx)
	if pending, _ := s3.ApprovalRequests(ctx, "pending"); len(pending) != 0 {
		t.Fatalf("a running plan asked its gap: %+v", pending)
	}
}

// planWithoutCriteria stores a plan without criteria on task t-plan, issue
// #1 of linkshort/linkshort, with one card per status, card-1 first, and
// a runner whose comments land on the returned Forgejo.
func planWithoutCriteria(t *testing.T, statuses ...string) (*Runner, *fakeStore, *fakeObjectStore, *planForgejo) {
	t.Helper()
	s := newFakeStore()
	b, err := json.Marshal(Plan{Summary: "the plan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPlanRevision(context.Background(), "t-plan", b, "sha"); err != nil {
		t.Fatal(err)
	}
	s.tasks["t-plan"] = Task{ID: "t-plan", Repository: "linkshort/linkshort", IssueNumber: 1, Source: SourceChat, Status: "done"}
	for i, status := range statuses {
		id := fmt.Sprintf("card-%d", i+1)
		s.tasks[id] = Task{ID: id, Repository: "linkshort/linkshort", Parent: "t-plan", Source: SourcePlan, PlanKey: id, Status: status}
	}
	fj := &planForgejo{issues: map[int64]*planIssue{1: {state: "open"}}}
	objs := &fakeObjectStore{}
	r := &Runner{store: s, timeline: &Timeline{Objects: objs, Events: s, Store: s},
		forgejo: &Forgejo{BaseURL: fj.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw"}}
	return r, s, objs, fj
}

// mergeCard gives card id of s a merge commit and its finish.
func mergeCard(s *fakeStore, id string) {
	finished := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	card := s.tasks[id]
	card.MergeCommit, card.FinishedAt = "abc", &finished
	s.tasks[id] = card
}

// closeRows counts t-plan's plan_closed event rows.
func closeRows(s *fakeStore) int {
	n := 0
	for _, e := range s.events {
		if e.TaskID == "t-plan" && e.Kind == EventPlanClosed {
			n++
		}
	}
	return n
}

// openPlans are the plans of s's open groups, every repository's.
func openPlans(t *testing.T, s *fakeStore) []string {
	t.Helper()
	groups, err := s.OpenGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, open := range groups {
		for _, g := range open {
			out = append(out, g.Plan)
		}
	}
	slices.Sort(out)
	return out
}

// flakyRecorder fails its first fails rows, then records them in s.
type flakyRecorder struct {
	fails int
	s     *fakeStore
}

func (f *flakyRecorder) Record(ctx context.Context, e Event) (int64, error) {
	if f.fails > 0 {
		f.fails--
		return 0, errors.New("events table down")
	}
	return f.s.Record(ctx, e)
}

// planCloses counts the plan_closed events of t-plan's timeline that
// carry no criteria.
func planCloses(t *testing.T, objs *fakeObjectStore) int {
	t.Helper()
	body, err := objs.Get(context.Background(), "sessions/t-plan/timeline.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(body), `"criteria":0,"event":"`+EventPlanClosed+`"`)
}

func commentBodies(fj *planForgejo) []string {
	var out []string
	for _, c := range fj.comments {
		out = append(out, fmt.Sprintf("#%d %s", c.issue, c.body))
	}
	return out
}

func TestPlanCloseCheckClosesAPlanWithoutCriteria(t *testing.T) {
	ctx := context.Background()
	r, _, objs, fj := planWithoutCriteria(t, "done", "done", "closed")
	want := []string{"#1 factoryd: every card of this plan is done or closed; the plan is closed."}
	r.planCloseCheck(ctx, "t-plan")
	if n := planCloses(t, objs); n != 1 {
		t.Fatalf("plan_closed with criteria 0 recorded %d times, want 1", n)
	}
	if got := commentBodies(fj); !slices.Equal(got, want) {
		t.Fatalf("comments = %q, want %q", got, want)
	}
	r.planCloseCheck(ctx, "t-plan")
	if n := planCloses(t, objs); n != 1 {
		t.Errorf("the second check: plan_closed recorded %d times, want 1", n)
	}
	if got := commentBodies(fj); !slices.Equal(got, want) {
		t.Errorf("the second check: comments = %q, want %q", got, want)
	}
}

// The comment that every card is done already told the issue; the close
// records itself and adds no second comment.
func TestPlanCloseCheckAddsNoCommentAfterAllChildrenDone(t *testing.T) {
	ctx := context.Background()
	r, _, objs, fj := planWithoutCriteria(t, "done", "done")
	r.timeline.recordEvent(ctx, "t-plan", EventAllChildrenDone, map[string]any{"cards": 2})
	r.planCloseCheck(ctx, "t-plan")
	if n := planCloses(t, objs); n != 1 || len(fj.comments) != 0 {
		t.Fatalf("plan_closed %d times, comments %q; want 1 and none", n, commentBodies(fj))
	}
}

func TestPlanCloseCheckKeepsAPlanWithoutCriteriaOpen(t *testing.T) {
	ctx := context.Background()
	for _, statuses := range [][]string{{"done", "failed"}, {"done", "merged"}} {
		r, _, objs, fj := planWithoutCriteria(t, statuses...)
		r.planCloseCheck(ctx, "t-plan")
		if n := planCloses(t, objs); n != 0 || len(fj.comments) != 0 {
			t.Errorf("cards %v: plan_closed %d times, comments %q; want none", statuses, n, commentBodies(fj))
		}
	}
}

func TestCriteriaPassVisitsOpenGroups(t *testing.T) {
	ctx := context.Background()
	r, s, objs, fj := planWithoutCriteria(t, "done")
	mergeCard(s, "card-1")
	if got := openPlans(t, s); !slices.Equal(got, []string{"t-plan"}) {
		t.Fatalf("open groups = %v, want t-plan", got)
	}
	want := []string{"#1 factoryd: every card of this plan is done or closed; the plan is closed."}
	for pass := 1; pass <= 2; pass++ {
		r.CriteriaPass(ctx)
		if n, rows := planCloses(t, objs), closeRows(s); n != 1 || rows != 1 {
			t.Fatalf("pass %d: plan_closed on the timeline %d times, as rows %d times; want 1 and 1", pass, n, rows)
		}
		if got := openPlans(t, s); len(got) != 0 {
			t.Errorf("pass %d: open groups = %v, want none", pass, got)
		}
		if got := commentBodies(fj); !slices.Equal(got, want) {
			t.Errorf("pass %d: comments = %q, want %q", pass, got, want)
		}
	}
}

// A close whose events row failed leaves the group open; the next pass
// writes the row, and the issue hears of the close once.
func TestCriteriaPassRetriesALostCloseRow(t *testing.T) {
	ctx := context.Background()
	r, s, objs, fj := planWithoutCriteria(t, "done")
	mergeCard(s, "card-1")
	r.timeline.Events = &flakyRecorder{fails: 1, s: s}
	r.CriteriaPass(ctx)
	if n, rows, open := planCloses(t, objs), closeRows(s), openPlans(t, s); n != 1 || rows != 0 || !slices.Equal(open, []string{"t-plan"}) {
		t.Fatalf("the failed row: plan_closed on the timeline %d times, rows %d, open groups %v; want 1, 0, t-plan", n, rows, open)
	}
	for pass := 2; pass <= 3; pass++ {
		r.CriteriaPass(ctx)
		if rows, open := closeRows(s), openPlans(t, s); rows != 1 || len(open) != 0 {
			t.Fatalf("pass %d: rows %d, open groups %v; want 1 and none", pass, rows, open)
		}
	}
	if want := []string{"#1 factoryd: every card of this plan is done or closed; the plan is closed."}; !slices.Equal(commentBodies(fj), want) {
		t.Errorf("comments = %q, want %q", commentBodies(fj), want)
	}
}

func TestCriteriaPassSkipsAnsweredDeltaDues(t *testing.T) {
	ctx := context.Background()
	s := seedVerdictPlan(t, "t-plan", "t-int", "done")
	if _, err := s.PutPlanCatches(ctx, "t-plan", 1, CatchDelta, FinderApprover, []string{"c3 grant 9"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutPlanCatches(ctx, "t-plan", 1, CatchDeltaFiled, FinderApprover, []string{"c3 grant 9"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCriteriaVerdicts(ctx, "t-int", "review", FactoryDecider, []CriterionVerdict{
		{Criterion: "c1", Verdict: "met", Kind: CriterionTest},
		{Criterion: "c2", Evidence: "read it"},
		{Criterion: "c3", Verdict: "unmet", Evidence: "times out", Fix: "planner"},
	}); err != nil {
		t.Fatal(err)
	}
	// the marker holds the sweep's round back: no model call, and the gap
	// question opens (the cards are done, c2 and c3 open)
	r := &Runner{store: s, timeline: &Timeline{Objects: &fakeObjectStore{}}}
	r.CriteriaPass(ctx)
	if pending, _ := s.ApprovalRequests(ctx, "pending"); len(pending) != 1 || pending[0].Kind != ApprovalCriteria {
		t.Fatalf("pending = %+v, want the gap question alone", pending)
	}
}
