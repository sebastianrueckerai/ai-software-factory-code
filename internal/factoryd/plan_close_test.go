package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

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
		commented := slices.Contains(*requests, "comment /api/v1/repos/sf/ai-software-factory-code/issues/4/comments: "+holdComment(3))
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

// plannedCards files a done plan t-plan of the three criteria of
// criteriaPlan with the given cards (no integration card among them: the
// plan is between rounds with an outline, or in its last round without
// one) on a runner.
func plannedCards(t *testing.T, outline []string, statuses ...string) (*Runner, *fakeStore, *planForgejo) {
	t.Helper()
	r, s, _, fj := planWithoutCriteria(t, statuses...)
	p := criteriaPlan()
	p.Cards, p.Outline = []PlanCard{{Key: "card-1", Role: RoleFeature, Title: "A card", Directories: []string{"a"}}}, outline
	newRevision(t, s, "t-plan", p)
	plan := s.tasks["t-plan"]
	plan.Track = policy.TrackPlan
	s.tasks["t-plan"] = plan
	mergeCard(s, "card-1")
	return r, s, fj
}

// A Close while a card of the plan runs records the waivers and the closing
// words' asks but no plan_closed, whichever shape the plan has: the close
// check records it when the last card ends, so a card that merges later
// still opens its group in the meantime.
func TestCloseHeldPlanWaitsForRunningCards(t *testing.T) {
	ctx := context.Background()
	t.Run("between rounds, no integration card", func(t *testing.T) {
		r, s, fj := plannedCards(t, []string{"later"}, "done", "running")
		n, _, _, err := r.CloseHeldPlan(ctx, "t-plan", "mallory", "", false)
		if err != nil || n != 0 || closeRows(s) != 0 {
			t.Fatalf("CloseHeldPlan = %d, %v, plan_closed rows %d; want 0, no error, 0", n, err, closeRows(s))
		}
		setStatus(s, "t-plan", "closed")
		r.planCloseCheck(ctx, "t-plan")
		if closeRows(s) != 0 {
			t.Fatalf("plan_closed recorded with a card running")
		}
		setStatus(s, "card-2", "done")
		r.planCloseCheck(ctx, "t-plan")
		want := []string{"#1 factoryd: the plan card was closed; the plan is closed, and its merged cards stay on main."}
		if got := commentBodies(fj); closeRows(s) != 1 || !slices.Equal(got, want) {
			t.Errorf("comments %q, plan_closed rows %d; want %q and 1", got, closeRows(s), want)
		}
	})
	t.Run("ten open criteria, the integration card running", func(t *testing.T) {
		r, s, _ := tenCriteriaPlan(t, "running")
		setStatus(s, "card-1", "running")
		n, _, _, err := r.CloseHeldPlan(ctx, "t-plan", "sebastian", "moved to the rebuild", true)
		if err != nil || n != 10 || closeRows(s) != 0 {
			t.Fatalf("CloseHeldPlan = %d, %v, plan_closed rows %d; want 10, no error, 0", n, err, closeRows(s))
		}
		setStatus(s, "card-1", "done")
		r.planCloseCheck(ctx, "t-plan")
		if closeRows(s) != 1 {
			t.Errorf("plan_closed rows %d after the card ended, want 1", closeRows(s))
		}
	})
}

// In its last round a plan with criteria and no integration card keeps its
// rows on the plan card, and every criterion without one is open: a
// non-approver's Close is refused, an approver's waives them there.
func TestCloseHeldPlanInTheLastRoundWithoutAnIntegrationCard(t *testing.T) {
	ctx := context.Background()
	r, s, _ := plannedCards(t, nil, "done")
	if _, _, _, err := r.CloseHeldPlan(ctx, "t-plan", "mallory", "why", false); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("a writer's close: %v, want ErrNotApprover", err)
	}
	if closeRows(s) != 0 {
		t.Fatalf("a refused close recorded plan_closed")
	}
	n, _, _, err := r.CloseHeldPlan(ctx, "t-plan", "sebastian", "moved to the rebuild", true)
	rows, _ := s.CriteriaVerdicts(ctx, "t-plan")
	if err != nil || n != 3 || len(rows) != 3 || closeRows(s) != 1 {
		t.Fatalf("CloseHeldPlan = %d, %v, rows %d, plan_closed rows %d; want 3 waived on the plan card and 1 row", n, err, len(rows), closeRows(s))
	}
}

// A plan card closed by an older path (a writer's close of a failed plan,
// or of a plan under a replan) with criteria open and merged cards keeps its
// hold: the close check records no plan_closed and says so once, until an
// approver's Close with a note waives them, which also takes a plan at
// rest in any status.
func TestClosedPlanWithOpenCriteriaKeepsItsHold(t *testing.T) {
	ctx := context.Background()
	for _, status := range []string{"closed", "failed"} {
		r, s, fj := tenCriteriaPlan(t, "done")
		setStatus(s, "t-plan", status)
		for range 2 {
			r.planCloseCheck(ctx, "t-plan")
		}
		hold := "#1 " + holdComment(10)
		if got := commentBodies(fj); status == "closed" && (closeRows(s) != 0 || !slices.Equal(got, []string{hold})) {
			t.Fatalf("closed plan: comments %q, plan_closed rows %d; want the hold said once and no row", got, closeRows(s))
		}
		if _, _, _, err := r.CloseHeldPlan(ctx, "t-plan", "mallory", "why", false); !errors.Is(err, ErrNotApprover) {
			t.Fatalf("%s plan, a writer's Close: %v, want ErrNotApprover", status, err)
		}
		if n, _, _, err := r.CloseHeldPlan(ctx, "t-plan", "sebastian", "moved on", true); err != nil || n != 10 || closeRows(s) != 1 {
			t.Fatalf("%s plan, an approver's Close = %d, %v, plan_closed rows %d; want 10, no error, 1", status, n, err, closeRows(s))
		}
	}
}

// issueStub is a Forgejo that records its writes and answers the comment
// reads with commentsStatus.
type issueStub struct {
	writes         []string
	commentsStatus int
}

func (s *issueStub) forgejo(t *testing.T) *Forgejo {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			w.WriteHeader(s.commentsStatus)
			io.WriteString(w, `[]`)
		case r.Method == http.MethodGet:
			io.WriteString(w, `{"state":"open"}`)
		default:
			s.writes = append(s.writes, r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":1}`)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
}

// A closing comment that cannot be read or posted leaves the issue as it
// is: the hold reopens it only after saying so, and the finish of a close
// closes it only after the words stand; the next pass tries again.
func TestPlanIssueActsOnlyAfterTheComment(t *testing.T) {
	ctx := context.Background()
	stub := &issueStub{commentsStatus: http.StatusInternalServerError}
	f := stub.forgejo(t)
	plan := Task{ID: "t-plan", Repository: "sf/code", IssueNumber: 4, Status: "done", Track: policy.TrackPlan}
	if err := holdDonePlan(ctx, f, plan, 3); err == nil || len(stub.writes) != 0 {
		t.Fatalf("holdDonePlan with a failing comment: %v, writes %q; want an error and no write", err, stub.writes)
	}
	r, s, _ := tenCriteriaPlan(t, "done")
	if _, _, _, err := r.CloseHeldPlan(ctx, "t-plan", "sebastian", "moved on", true); err != nil {
		t.Fatal(err)
	}
	r.forgejo = f
	r.finishPlanClose(ctx, "t-plan", "closed by sebastian; its merged cards stay on main")
	if len(stub.writes) != 0 || s.tasks["t-plan"].Status != "done" {
		t.Fatalf("finishPlanClose with a failing comment: writes %q, status %q; want none and done", stub.writes, s.tasks["t-plan"].Status)
	}
	stub.commentsStatus = http.StatusOK
	r.finishPlanClose(ctx, "t-plan", "closed by sebastian; its merged cards stay on main")
	if len(stub.writes) == 0 || !strings.HasPrefix(stub.writes[0], "POST ") {
		t.Fatalf("the next pass: writes %q; want the comment first", stub.writes)
	}
}

// A plan none of whose cards holds an unreverted merge holds nothing in
// production: nothing is open and a writer's Close takes it; a reverted
// merge counts as none, and a pull_request_merged row counts as a merge
// without a merge commit, as the production hold reads it.
func TestPlanWithoutAMergeHoldsNothing(t *testing.T) {
	ctx := context.Background()
	r, s, _ := plannedCards(t, nil, "done")
	card := s.tasks["card-1"]
	card.MergeCommit = ""
	s.tasks["card-1"] = card
	setStatus(s, "t-plan", "failed")
	if n, _, _, err := r.CloseHeldPlan(ctx, "t-plan", "mallory", "", false); err != nil || n != 0 {
		t.Fatalf("a writer's Close of an unmerged failed plan = %d, %v; want 0 and no error", n, err)
	}
	_, s, _ = plannedCards(t, nil, "done")
	reverted := time.Now()
	card = s.tasks["card-1"]
	card.RevertedAt = &reverted
	s.tasks["card-1"] = card
	if open, _, err := planOpen(ctx, s, "t-plan", criteriaPlan()); err != nil || len(open) != 0 {
		t.Errorf("planOpen with only a reverted merge = %v, %v; want none", open, err)
	}
	if open, _, err := planOpen(ctx, s, "t-plan", Plan{Criteria: criteriaPlan().Criteria}); err != nil || len(open) != 0 {
		t.Errorf("planOpen again = %v, %v; want none", open, err)
	}
}

// A plan whose every criterion stands met closes only once its other cards
// are settled too: a card still running keeps the plan open, so a card that
// merges later still opens its group.
func TestMetCriteriaWaitForARunningCard(t *testing.T) {
	ctx := context.Background()
	r, s, _ := tenCriteriaPlan(t, "done", "running")
	var rows []CriterionVerdict
	for i := 1; i <= 10; i++ {
		rows = append(rows, CriterionVerdict{Criterion: "k" + strconv.Itoa(i), Verdict: "met", Kind: CriterionInvariant})
	}
	if err := s.PutCriteriaVerdicts(ctx, "card-1", "review", FactoryDecider, rows); err != nil {
		t.Fatal(err)
	}
	r.planCloseCheck(ctx, "t-plan")
	if closeRows(s) != 0 {
		t.Fatalf("plan_closed recorded with card-2 running")
	}
	setStatus(s, "card-2", "done")
	r.planCloseCheck(ctx, "t-plan")
	if closeRows(s) != 1 {
		t.Errorf("plan_closed rows %d once card-2 ended, want 1", closeRows(s))
	}
}

// The finish takes a failed plan whose close recorded its row too.
func TestCriteriaPassFinishesAFailedPlanClose(t *testing.T) {
	ctx := context.Background()
	r, s, fj := tenCriteriaPlan(t, "done")
	setStatus(s, "t-plan", "failed")
	if _, _, _, err := r.CloseHeldPlan(ctx, "t-plan", "sebastian", "moved on", true); err != nil {
		t.Fatal(err)
	}
	r.CriteriaPass(ctx)
	if got, status := commentBodies(fj), s.tasks["t-plan"].Status; len(got) != 1 || status != "closed" || fj.issues[1].state != "closed" {
		t.Errorf("comments %q, status %q, issue %q; want the words once, closed, closed", got, status, fj.issues[1].state)
	}
}

// A card with a pull_request_merged row and no merge commit (a merge whose
// commit read failed, or one from before merge commits were recorded) holds
// production: its criteria are open and a writer's Close is refused.
func TestPlanMergedByARowAloneHolds(t *testing.T) {
	ctx := context.Background()
	r, s, _ := tenCriteriaPlan(t, "done")
	card := s.tasks["card-1"]
	card.MergeCommit = ""
	s.tasks["card-1"] = card
	if merged, err := s.PlanMerged(ctx, "t-plan"); err != nil || merged {
		t.Fatalf("PlanMerged with no commit and no row = %t, %v; want false", merged, err)
	}
	s.Record(ctx, Event{Repository: "sf/code", TaskID: "card-1", Kind: "pull_request_merged"})
	if merged, err := s.PlanMerged(ctx, "t-plan"); err != nil || !merged {
		t.Fatalf("PlanMerged with a pull_request_merged row = %t, %v; want true", merged, err)
	}
	if _, _, _, err := r.CloseHeldPlan(ctx, "t-plan", "mallory", "why", false); !errors.Is(err, ErrNotApprover) {
		t.Errorf("a writer's Close: %v, want ErrNotApprover", err)
	}
	if err := r.CheckClosePlan(ctx, "t-plan", "why", false); !errors.Is(err, ErrNotApprover) {
		t.Errorf("a writer's check: %v, want ErrNotApprover", err)
	}
}

// A failed plan that factoryd closed itself keeps its card failed (the
// bot's close does not end it): a person's Close ends it closed, and the
// sweep does the same without one.
func TestFailedPlanClosedByFactorydEnds(t *testing.T) {
	ctx := context.Background()
	for _, byPerson := range []bool{true, false} {
		r, s, fj := plannedCards(t, nil, "done")
		card := s.tasks["card-1"]
		card.MergeCommit = ""
		s.tasks["card-1"] = card
		setStatus(s, "t-plan", "failed")
		s.Record(ctx, Event{Repository: "linkshort/linkshort", TaskID: "t-plan", Kind: EventPlanClosed, Actor: FactoryDecider, Via: ViaFactoryd})
		if byPerson {
			if n, words, _, err := r.CloseHeldPlan(ctx, "t-plan", "mallory", "", false); err != nil || n != 0 || words != "closed by mallory" {
				t.Fatalf("a person's Close = %d, %q, %v; want 0, the words without merged cards, no error", n, words, err)
			}
		} else {
			r.CriteriaPass(ctx)
		}
		if got := s.tasks["t-plan"].Status; got != "closed" {
			t.Errorf("by a person %t: status %q, want closed", byPerson, got)
		}
		if len(fj.comments) != 0 {
			t.Errorf("by a person %t: comments %q, want none (a close without words posts no empty comment)", byPerson, commentBodies(fj))
		}
	}
}

// The check and the Close answer alike over every shape of a plan's state:
// status, merge (none, commit, row only, reverted), plan_closed row,
// approver and note. The check is what keeps a refused Close from touching
// the issue, so it must refuse exactly what the Close refuses.
func TestCheckClosePlanAgreesWithCloseHeldPlan(t *testing.T) {
	ctx := context.Background()
	shape := func(status, merge string, row bool) (*Runner, *fakeStore) {
		r, s, _ := tenCriteriaPlan(t, "done")
		setStatus(s, "t-plan", status)
		card := s.tasks["card-1"]
		switch merge {
		case "none":
			card.MergeCommit = ""
		case "row":
			card.MergeCommit = ""
			s.Record(ctx, Event{Repository: "sf/code", TaskID: "card-1", Kind: "pull_request_merged"})
		case "reverted":
			now := time.Now()
			card.RevertedAt = &now
		}
		s.tasks["card-1"] = card
		if row {
			s.Record(ctx, Event{Repository: "linkshort/linkshort", TaskID: "t-plan", Kind: EventPlanClosed, Actor: FactoryDecider, Via: ViaFactoryd})
		}
		return r, s
	}
	for _, status := range []string{"done", "failed", "closed", "running"} {
		for _, merge := range []string{"none", "commit", "row", "reverted"} {
			for _, row := range []bool{false, true} {
				for _, approver := range []bool{false, true} {
					for _, note := range []string{"", "why", strings.Repeat("x", MaxWaiverNote+1)} {
						r, _ := shape(status, merge, row)
						check := r.CheckClosePlan(ctx, "t-plan", note, approver)
						r, _ = shape(status, merge, row)
						_, _, _, closeErr := r.CloseHeldPlan(ctx, "t-plan", "mallory", note, approver)
						if !errors.Is(check, closeErr) && !errors.Is(closeErr, check) {
							t.Errorf("status %s, merge %s, row %t, approver %t, note %d bytes: check %v, Close %v", status, merge, row, approver, len(note), check, closeErr)
						}
					}
				}
			}
		}
	}
}
