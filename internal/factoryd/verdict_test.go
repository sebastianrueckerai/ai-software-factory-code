package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestParseReviewCriteriaAnswer(t *testing.T) {
	answer := `{"verdict": "concerns", "summary": "one criterion is not met", "findings": [],
	"criteria": [{"criterion": "c1", "verdict": "unmet", "evidence": "go test exits 1", "fix": "planner"}, {"criterion": "", "verdict": "met"}],
	"behaviours": [" the door opens only for a key that fits ", ""]}`
	rv := parseReview(answer)
	if rv.Note != "" {
		t.Fatalf("note: %s", rv.Note)
	}
	if len(rv.Criteria) != 1 || rv.Criteria[0].Criterion != "c1" || rv.Criteria[0].Fix != "planner" {
		t.Errorf("criteria = %v", rv.Criteria)
	}
	if fmt.Sprint(rv.Behaviours) != fmt.Sprint([]string{"the door opens only for a key that fits"}) {
		t.Errorf("behaviours = %v", rv.Behaviours)
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
	r := &Runner{store: s, timeline: &Timeline{Objects: objs}}
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
	r := &Runner{store: s, timeline: &Timeline{Objects: objs},
		forgejo: &Forgejo{BaseURL: fj.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw"}}
	return r, s, objs, fj
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
	want := []string{"#1 factoryd: every card of this plan is done; the plan is closed."}
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
	finished := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	card := s.tasks["card-1"]
	card.MergeCommit, card.FinishedAt = "abc", &finished
	s.tasks["card-1"] = card
	groups, err := s.OpenGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if g := groups["linkshort/linkshort"]; len(g) != 1 || g[0].Plan != "t-plan" {
		t.Fatalf("open groups = %+v, want t-plan's", groups)
	}
	r.CriteriaPass(ctx)
	if n := planCloses(t, objs); n != 1 {
		t.Fatalf("plan_closed with criteria 0 recorded %d times, want 1", n)
	}
	if want := []string{"#1 factoryd: every card of this plan is done; the plan is closed."}; !slices.Equal(commentBodies(fj), want) {
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
