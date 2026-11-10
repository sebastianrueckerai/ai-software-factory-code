package factoryd

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const holdComment = "factoryd: this plan still holds production with 3 open criteria. " +
	"Closing its issue waives nothing: an approver's Close with a note on the console waives them and ends the plan."

// lastRoundCriteriaPlan is a done plan in its last round with the criteria
// of criteriaPlan and its integration card merged, on store s as task id.
func lastRoundCriteriaPlan(t *testing.T, s *fakeStore, id, repo string, issue int64) {
	t.Helper()
	p := criteriaPlan()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPlanRevision(context.Background(), id, b, "sha"); err != nil {
		t.Fatal(err)
	}
	s.tasks[id] = Task{ID: id, Repository: repo, IssueNumber: issue, Status: "done", Kind: "agent", Track: policy.TrackPlan}
	s.tasks[id+"-int"] = Task{ID: id + "-int", Repository: repo, Parent: id, Source: SourcePlan, PlanKey: "integration", Status: "done"}
	mergeCard(s, id+"-int")
}

// A writer's close of a done plan's issue on Forgejo ends the production
// hold only when the plan's plan_closed row stands or no criterion is open:
// otherwise the plan stays done, its issue opens again and says once that an
// approver's Close ends it. The webhook and the sweep take the same rule.
func TestDonePlanForgejoCloseKeepsTheHold(t *testing.T) {
	ctx := context.Background()
	closeBody, _ := json.Marshal(map[string]any{
		"action":     "closed",
		"issue":      map[string]any{"number": 4, "user": map[string]string{"login": "sebastian"}},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		"sender":     map[string]string{"login": "sebastian"},
	})
	for _, tc := range []struct {
		name   string
		setup  func(s *fakeStore)
		after  string
		reopen bool
	}{
		{"three criteria open", func(*fakeStore) {}, "done", true},
		{"the plan_closed row stands", func(s *fakeStore) { s.closed["t-plan"] = replanAt }, "closed", false},
		{"every criterion waived", func(s *fakeStore) {
			s.PutCriteriaVerdicts(ctx, "t-plan-int", CriteriaSourceApprover, "sebastian", []CriterionVerdict{
				{Criterion: "c1", Verdict: "waived", Evidence: "x", Kind: CriterionTest}, {Criterion: "c2", Verdict: "waived", Evidence: "x", Kind: CriterionHuman},
				{Criterion: "c3", Verdict: "waived", Evidence: "x", Kind: CriterionInvariant}})
		}, "closed", false},
	} {
		in, store, requests := newTestIntake(t)
		lastRoundCriteriaPlan(t, store, "t-plan", "sf/ai-software-factory-code", 4)
		tc.setup(store)
		if rr := postWebhook(in, "issues", closeBody, sign(webhookSecret, closeBody)); rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.name, rr.Code)
		}
		reopened := slices.Contains(*requests, "close /api/v1/repos/sf/ai-software-factory-code/issues/4")
		commented := slices.Contains(*requests, "comment /api/v1/repos/sf/ai-software-factory-code/issues/4/comments: "+holdComment)
		if got := store.tasks["t-plan"].Status; got != tc.after || reopened != tc.reopen || commented != tc.reopen {
			t.Errorf("webhook, %s: status %q, reopened %t, commented %t; want %q, %t, %t", tc.name, got, reopened, commented, tc.after, tc.reopen, tc.reopen)
		}
	}
}

// The sweep takes a missed close the same way, through the same rule; a
// close that is the bot's own, recorded, leaves the plan alone.
func TestRoundCheckHoldsAPlanWhoseOpenCriteriaAWriterClosed(t *testing.T) {
	ctx := context.Background()
	last := twoRounds()
	last.Outline = nil
	for _, tc := range []struct {
		name, closer string
		botRecorded  bool
		waived       bool
		after        string
		reopened     int
		comments     []string
	}{
		{name: "a writer's close, criteria open", closer: "sebastian", after: "done", reopened: 1, comments: []string{
			"factoryd: this plan still holds production with 3 open criteria. Closing its issue waives nothing: an approver's Close with a note on the console waives them and ends the plan."}},
		{name: "a writer's close, criteria waived", closer: "sebastian", waived: true, after: "closed"},
		{name: "the bot's recorded close, criteria waived", closer: "factory-agent", botRecorded: true, waived: true, after: "done"},
		{name: "an open issue", after: "done"},
	} {
		r, store, fj := roundRigOf(t, last)
		fj.issueOff, fj.closer = tc.closer != "", tc.closer
		if tc.botRecorded {
			store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "p", Kind: IntakeFactoryClosed, Author: "factoryd", CommentID: 10})
		}
		if tc.waived {
			var rows []CriterionVerdict
			for _, c := range last.Criteria {
				rows = append(rows, CriterionVerdict{Criterion: c.ID, Verdict: "waived", Evidence: "x", Kind: c.Kind})
			}
			store.PutCriteriaVerdicts(ctx, "t-integration", CriteriaSourceApprover, "sebastian", rows)
		}
		if err := r.nextRound(ctx, "p"); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := store.tasks["p"].Status; got != tc.after || len(fj.reopened) != tc.reopened || !slices.Equal(fj.comments, tc.comments) {
			t.Errorf("%s: status %q, reopened %v, comments %q; want %q, %d, %q", tc.name, got, fj.reopened, fj.comments, tc.after, tc.reopened, tc.comments)
		}
	}
}
