package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// threeOpenHeld is what the issue of criteriaPlan's plan hears while it is
// held with its three promises open and none met on main.
const threeOpenHeld = "Close this plan and drop its 3 open promises? Until it closes, the plan holds production deploys.\n\n" +
	"A human closed the plan's card while these promises were open:\n- A link is stored.\n- The feature reads well.\n- The tests in ./... stay green.\n\n" +
	"The answers on the board:\n" +
	"- Drop them and close: Drops the open promises with your reason and closes the plan, so production deploys go on.\n" +
	"- Keep the plan open: The plan stays open and holds production deploys until an approver closes it."

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
		commented := slices.Contains(*requests, "comment /api/v1/repos/sf/ai-software-factory-code/issues/4/comments: "+threeOpenHeld)
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
			"Close this plan and drop its 3 open promises? Until it closes, the plan holds production deploys.\n\n" +
				"A human closed the plan's card while these promises were open:\n- A link made through the API is kept.\n- A short link sends its visitor on.\n- The existing tests stay green.\n\n" +
				"The answers on the board:\n" +
				"- Drop them and close: Drops the open promises with your reason and closes the plan, so production deploys go on.\n" +
				"- Keep the plan open: The plan stays open and holds production deploys until an approver closes it."}},
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

// The hold's words say what a Close on the console waives. The criteria
// gate's run on main (10-02 12:00) met c1 and c3; then the human closed
// the failed card, so the gate's rows no longer judge the cards as they
// stand, and every criterion no approver's row settles holds production.
// With c2 unsettled the hold counts 3 and a Close waives c2 alone, an
// approver's with a note; once an approver met c2 the hold counts 2 and a
// Close waives none, a writer's without a note. A writer's close of the
// done plan's issue and the close check of the closed plan say the same.
func TestHoldCommentSaysWhatTheCloseWaives(t *testing.T) {
	ctx := context.Background()
	ran := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	later := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name     string
		c2Met    bool
		comment  string
		writer   error
		by, note string
		approver bool
		waived   int
	}{
		{"the gate met two of three", false,
			"Close this plan and drop its 3 open promises? Until it closes, the plan holds production deploys.\n\n" +
				"A human closed the plan's card while these promises were open:\n- A link is stored.\n- The feature reads well.\n- The tests in ./... stay green.\n\n" +
				"The last run of the plan's tests on main met 2 of them, so closing drops only the other 1.\n\n" +
				"The answers on the board:\n" +
				"- Drop them and close: Drops the open promises with your reason and closes the plan, so production deploys go on.\n" +
				"- Keep the plan open: The plan stays open and holds production deploys until an approver closes it.",
			ErrNotApprover, "sebastian", "moved on", true, 1},
		{"the gate met every open one", true,
			"Close this plan now? Until it closes, the plan holds production deploys.\n\n" +
				"A human closed the plan's card while these promises were open:\n- A link is stored.\n- The tests in ./... stay green.\n\n" +
				"The last run of the plan's tests on main met all of them, so closing drops none.\n\n" +
				"The answers on the board:\n" +
				"- Close the plan now: Closes the plan now, so production deploys go on.\n" +
				"- Keep the plan open: The plan stays open and holds production deploys until an approver closes it.",
			nil, "mallory", "", false, 0},
	} {
		for _, path := range []string{"issue close", "close check"} {
			t.Run(c.name+", "+path, func(t *testing.T) {
				fj := &prForgejo{pulls: map[int64]string{9: `{"number":9,"state":"closed","merged":true,"head":{"sha":"h9"},"base":{"ref_name":"main"}}`},
					mainHead: "m1", statusesBySHA: map[string][]string{"m1": nil}}
				r, s := gatePlanOn(t, fj, 5, 5, func(s *fakeStore) func(context.Context, string, int64, string, string) (CriteriaResult, error) {
					return func(_ context.Context, taskID string, _ int64, _, _ string) (CriteriaResult, error) {
						return CriteriaResult{}, gateMet(ctx, s, taskID)
					}
				})
				integrationAs("closed", 2085, "")(s)
				s.criteriaVerdicts["t-int"] = nil
				plan := s.tasks["t-plan"]
				plan.Track = policy.TrackPlan
				s.tasks["t-plan"] = plan
				mergeCard(s, "card-foundation")
				failed := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
				s.tasks["card-extra"] = Task{ID: "card-extra", Repository: "sf/code", Parent: "t-plan", Source: SourcePlan, PlanKey: "extra", Status: "failed", FinishedAt: &failed}
				sweep(ctx, r)
				gateRunsAt(s, ran.Add(-time.Hour), ran)
				card := s.tasks["card-extra"]
				card.Status, card.FinishedAt = "closed", &later
				s.tasks["card-extra"] = card
				if c.c2Met {
					if err := s.PutCriteriaVerdicts(ctx, "t-int", CriteriaSourceApprover, "sebastian", []CriterionVerdict{{Criterion: "c2", Verdict: "met", Evidence: "reads well", Kind: CriterionHuman}}); err != nil {
						t.Fatal(err)
					}
				}
				fj.comments = nil
				reopened := []int64{7}
				if path == "issue close" {
					if err := r.planHolder().endDonePlan(ctx, s.tasks["t-plan"]); err != nil {
						t.Fatal(err)
					}
				} else {
					setStatus(s, "t-plan", "closed")
					r.planCloseCheck(ctx, "t-plan")
					reopened = nil
				}
				if !slices.Equal(fj.comments, []string{c.comment}) || !slices.Equal(fj.reopened, reopened) || closeRows(s) != 0 {
					t.Fatalf("comments %q, reopened %v, plan_closed rows %d; want %q, %v, none", fj.comments, fj.reopened, closeRows(s), c.comment, reopened)
				}
				if err := r.CheckClosePlan(ctx, "t-plan", "mallory", "", false); !errors.Is(err, c.writer) || (err == nil) != (c.writer == nil) {
					t.Fatalf("a writer's Close without a note: %v, want %v", err, c.writer)
				}
				if n, _, _, err := r.CloseHeldPlan(ctx, "t-plan", c.by, c.note, c.approver); err != nil || n != c.waived || closeRows(s) != 1 {
					t.Fatalf("CloseHeldPlan by %s = %d, %v, plan_closed rows %d; want %d waived and the plan closed", c.by, n, err, closeRows(s), c.waived)
				}
			})
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
		want := []string{"#1 The plan card was closed, so the plan is closed. Its merged cards stay on main."}
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

// A Close while a key's work runs on in the successor of its failed card
// records no plan_closed: the close check records it once the successor
// ends.
func TestCloseHeldPlanWaitsForARunningSuccessor(t *testing.T) {
	ctx := context.Background()
	r, s, _ := plannedCards(t, []string{"later"}, "done", "failed")
	succ := SuccessorID("card-2")
	s.tasks[succ] = Task{ID: succ, Repository: "linkshort/linkshort", Parent: "t-plan", Source: SourceSuccessor, PlanKey: "card-2", Status: "running"}
	n, _, _, err := r.CloseHeldPlan(ctx, "t-plan", "mallory", "", false)
	if err != nil || n != 0 || closeRows(s) != 0 {
		t.Fatalf("CloseHeldPlan = %d, %v, plan_closed rows %d; want 0, no error, 0", n, err, closeRows(s))
	}
	setStatus(s, "t-plan", "closed")
	setStatus(s, succ, "done")
	r.planCloseCheck(ctx, "t-plan")
	if closeRows(s) != 1 {
		t.Errorf("plan_closed rows %d after the successor ended, want 1", closeRows(s))
	}
}

// In its last round a plan with criteria and no integration card keeps its
// rows on the plan card, and every criterion without one is open: a
// non-approver's Close is refused, and so is the project agent's, which
// its caller calls an approver's, since a waiver is a person's; an
// approver's waives them there.
func TestCloseHeldPlanInTheLastRoundWithoutAnIntegrationCard(t *testing.T) {
	ctx := context.Background()
	r, s, _ := plannedCards(t, nil, "done")
	if _, _, _, err := r.CloseHeldPlan(ctx, "t-plan", "mallory", "why", false); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("a writer's close: %v, want ErrNotApprover", err)
	}
	if _, _, _, err := r.CloseHeldPlan(ctx, "t-plan", ChatUser, "why", true); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("the project agent's close: %v, want ErrNotApprover", err)
	}
	// the check refuses what the Close refuses, so the console changes nothing before it
	if err := r.CheckClosePlan(ctx, "t-plan", ChatUser, "why", true); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("the check of the project agent's close: %v, want ErrNotApprover", err)
	}
	if err := r.CheckClosePlan(ctx, "t-plan", "sebastian", "why", true); err != nil {
		t.Fatalf("the check of an approver's close: %v", err)
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
// hold: the close check records no plan_closed, until an approver's Close
// with a note waives them, which also takes a plan at rest in any status.
// A closed plan asks on the board, once per set of open promises, to close
// and drop them or to stay open, and its issue hears the same words once;
// the Close ends the ask as its drop.
func TestClosedPlanWithOpenCriteriaKeepsItsHold(t *testing.T) {
	ctx := context.Background()
	words := "Close this plan and drop its 10 open promises? Until it closes, the plan holds production deploys.\n\n" +
		"A human closed the plan's card while these promises were open:\n" + strings.Repeat("- The tests in ./... stay green.\n", 10) + "\n" +
		"The answers on the board:\n" +
		"- Drop them and close: Drops the open promises with your reason and closes the plan, so production deploys go on.\n" +
		"- Keep the plan open: The plan stays open and holds production deploys until an approver closes it."
	held := func(s *fakeStore) []Case {
		var out []Case
		for _, c := range s.cases {
			if c.TaskID == "t-plan" && c.Kind == CasePlanHeld {
				out = append(out, c)
			}
		}
		return out
	}
	for _, status := range []string{"closed", "failed"} {
		r, s, fj := tenCriteriaPlan(t, "done")
		setStatus(s, "t-plan", status)
		for range 2 {
			r.planCloseCheck(ctx, "t-plan")
		}
		if got := commentBodies(fj); status == "closed" && (closeRows(s) != 0 || !slices.Equal(got, []string{"#1 " + words}) || len(held(s)) != 1 || held(s)[0].Status != CaseAtHuman) {
			t.Fatalf("closed plan: comments %q, plan_closed rows %d, asks %+v; want the ask once, said once, and no row", got, closeRows(s), held(s))
		}
		if _, _, _, err := r.CloseHeldPlan(ctx, "t-plan", "mallory", "why", false); !errors.Is(err, ErrNotApprover) {
			t.Fatalf("%s plan, a writer's Close: %v, want ErrNotApprover", status, err)
		}
		if n, _, _, err := r.CloseHeldPlan(ctx, "t-plan", "sebastian", "moved on", true); err != nil || n != 10 || closeRows(s) != 1 {
			t.Fatalf("%s plan, an approver's Close = %d, %v, plan_closed rows %d; want 10, no error, 1", status, n, err, closeRows(s))
		}
		if status == "closed" {
			if k := held(s)[0]; k.Status != CaseResolved || k.Resolution == nil || k.Resolution.Action != AnswerDropAndEnd {
				t.Fatalf("the Close left the ask %s %+v, want it resolved as the drop", k.Status, k.Resolution)
			}
		}
	}
}

// Keep the plan open settles the held plan's ask and leaves the hold: the
// close check asks again only once the set of open promises changes.
func TestKeepThePlanOpenSettlesTheAsk(t *testing.T) {
	ctx := context.Background()
	r, s, fj := tenCriteriaPlan(t, "done")
	setStatus(s, "t-plan", "closed")
	r.planCloseCheck(ctx, "t-plan")
	var c Case
	for _, k := range s.cases {
		if k.Kind == CasePlanHeld {
			c = k
		}
	}
	if _, err := r.Act(ctx, ActInput{TaskID: "t-plan", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActSettle}}); err != nil {
		t.Fatal(err)
	}
	r.planCloseCheck(ctx, "t-plan")
	asks := 0
	for _, k := range s.cases {
		if k.Kind == CasePlanHeld {
			asks++
		}
	}
	said := 0
	for _, body := range commentBodies(fj) {
		if strings.Contains(body, "Close this plan and drop") {
			said++
		}
	}
	if k, _ := s.CaseByID(ctx, c.ID); k.Status != CaseResolved || asks != 1 || said != 1 || closeRows(s) != 0 {
		t.Fatalf("the ask is %s with %d asks, %d comments and %d plan_closed rows; want it settled, asked once, said once, the plan held: %q", k.Status, asks, len(commentBodies(fj)), closeRows(s), commentBodies(fj))
	}
	if err := s.PutCriteriaVerdicts(ctx, "card-1", CriteriaSourceApprover, "sebastian", []CriterionVerdict{{Criterion: "k1", Verdict: "waived", Evidence: "moved", Kind: CriterionInvariant}}); err != nil {
		t.Fatal(err)
	}
	r.planCloseCheck(ctx, "t-plan")
	asks = 0
	for _, k := range s.cases {
		if k.Kind == CasePlanHeld && k.Status == CaseAtHuman && len(factStrings(k, "open")) == 9 {
			asks++
		}
	}
	if asks != 1 {
		t.Fatalf("a changed set of open promises asks %d times, want once", asks)
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
	held := newFakeStore()
	lastRoundCriteriaPlan(t, held, "t-plan", "sf/code", 4)
	holder := planHolder{store: held, forgejo: f, untrusted: func(_ Task, s string) string { return s }}
	if err := holder.endDonePlan(ctx, held.tasks["t-plan"]); err == nil || len(stub.writes) != 0 {
		t.Fatalf("the hold of a done plan's issue with a failing comment: %v, writes %q; want an error and no write", err, stub.writes)
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
	unmergeCard(s, "card-1")
	setStatus(s, "t-plan", "failed")
	if n, _, _, err := r.CloseHeldPlan(ctx, "t-plan", "mallory", "", false); err != nil || n != 0 {
		t.Fatalf("a writer's Close of an unmerged failed plan = %d, %v; want 0 and no error", n, err)
	}
	_, s, _ = plannedCards(t, nil, "done")
	reverted := time.Now()
	card := s.tasks["card-1"]
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
	unmergeCard(s, "card-1")
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
	if err := r.CheckClosePlan(ctx, "t-plan", "mallory", "why", false); !errors.Is(err, ErrNotApprover) {
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
		unmergeCard(s, "card-1")
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
		switch merge {
		case "none":
			unmergeCard(s, "card-1")
		case "row":
			unmergeCard(s, "card-1")
			s.Record(ctx, Event{Repository: "sf/code", TaskID: "card-1", Kind: "pull_request_merged"})
		case "reverted":
			now := time.Now()
			card := s.tasks["card-1"]
			card.RevertedAt = &now
			s.tasks["card-1"] = card
		}
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
						for _, by := range []string{"mallory", ChatUser} {
							r, _ := shape(status, merge, row)
							check := r.CheckClosePlan(ctx, "t-plan", by, note, approver)
							r, _ = shape(status, merge, row)
							_, _, _, closeErr := r.CloseHeldPlan(ctx, "t-plan", by, note, approver)
							if !errors.Is(check, closeErr) && !errors.Is(closeErr, check) {
								t.Errorf("status %s, merge %s, row %t, approver %t, note %d bytes, by %s: check %v, Close %v", status, merge, row, approver, len(note), by, check, closeErr)
							}
						}
					}
				}
			}
		}
	}
}

// A closed plan's ask reaches the inbox as every case at the human does:
// one ask row naming its case, whose message says the card's why, and
// open on the closed card until the plan's close answers it.
func TestAHeldPlansAskReachesTheInbox(t *testing.T) {
	ctx := context.Background()
	r, s, _ := tenCriteriaPlan(t, "done")
	setStatus(s, "t-plan", "closed")
	for range 2 {
		r.planCloseCheck(ctx, "t-plan")
	}
	var asks []Event
	for _, e := range s.events {
		if e.TaskID == "t-plan" && e.Kind == EventQuestionAsked {
			asks = append(asks, e)
		}
	}
	if len(asks) != 1 || asks[0].Detail["case_kind"] != string(CasePlanHeld) {
		t.Fatalf("the plan's ask rows %+v, want one naming the held plan's case", asks)
	}
	plan := s.tasks["t-plan"]
	m, ok := PokeOf(asks[0], plan, Task{})
	if !ok || m.Content != "#1 needs you. Until it closes, the plan holds production deploys." {
		t.Fatalf("the inbox's message = %q, %v", m.Content, ok)
	}
	// the plan's events as the store reads them: numbered, a minute apart
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	read := func() []Event {
		var out []Event
		for i, e := range s.events {
			if e.TaskID == "t-plan" {
				e.ID, e.At = int64(i+1), at.Add(time.Duration(i)*time.Minute)
				out = append(out, e)
			}
		}
		return out
	}
	events := read()
	m.At = events[len(events)-1].At.Add(time.Second)
	if st := SettleOf(m, events, plan.Status, false); !st.Open {
		t.Fatalf("the message on the closed plan reads %+v, want it open", st)
	}
	if _, _, _, err := r.CloseHeldPlan(ctx, "t-plan", "sebastian", "moved on", true); err != nil {
		t.Fatal(err)
	}
	if st := SettleOf(m, read(), plan.Status, false); st.Open || st.Word != "answered" || st.By != "sebastian" {
		t.Fatalf("after the Close the message reads %+v, want it answered by sebastian", st)
	}
}

// A closed plan asks again once its ask was withdrawn, and a changed set
// of open promises takes the place of the live ask of the older set: one
// ask stands at a time, and Close ends it.
func TestAHeldPlanAsksAgainAfterAWithdrawalAndOnANewSet(t *testing.T) {
	ctx := context.Background()
	r, s, _ := tenCriteriaPlan(t, "done")
	setStatus(s, "t-plan", "closed")
	held := func() (live []Case, all int) {
		for _, c := range s.cases {
			if c.Kind == CasePlanHeld {
				all++
				if c.Status == CaseAtHuman {
					live = append(live, c)
				}
			}
		}
		return live, all
	}
	r.planCloseCheck(ctx, "t-plan")
	first, _ := held()
	if err := s.WithdrawCase(ctx, first[0].ID, "card ended closed"); err != nil {
		t.Fatal(err)
	}
	r.planCloseCheck(ctx, "t-plan")
	if live, all := held(); len(live) != 1 || all != 2 || live[0].ID == first[0].ID {
		t.Fatalf("after a withdrawal: live %+v of %d asks, want a new one of 2", live, all)
	}
	if err := s.PutCriteriaVerdicts(ctx, "card-1", CriteriaSourceApprover, "sebastian", []CriterionVerdict{{Criterion: "k1", Verdict: "waived", Evidence: "moved", Kind: CriterionInvariant}}); err != nil {
		t.Fatal(err)
	}
	r.planCloseCheck(ctx, "t-plan")
	live, all := held()
	if len(live) != 1 || all != 3 || len(factStrings(live[0], "open")) != 9 {
		t.Fatalf("after a waiver: live %+v of %d asks, want the one of 9 open promises of 3", live, all)
	}
	if _, _, _, err := r.CloseHeldPlan(ctx, "t-plan", "sebastian", "moved on", true); err != nil {
		t.Fatal(err)
	}
	if live, _ := held(); len(live) != 0 {
		t.Fatalf("after the Close %d asks stand, want none", len(live))
	}
}

// A held plan whose last open promise gets its verdict without the Close,
// by Drop this promise or by a gate run's rows, closes by the factory's
// own close check, and that close ends the held ask: the factory
// withdraws it, as every promise is settled, and none stands at the
// human on the closed plan.
func TestTheFactorysCloseEndsTheHeldAsk(t *testing.T) {
	ctx := context.Background()
	for _, path := range []string{"drop", "gate rows"} {
		t.Run(path, func(t *testing.T) {
			r, s, _ := tenCriteriaPlan(t, "done")
			setStatus(s, "t-plan", "closed")
			r.planCloseCheck(ctx, "t-plan")
			i := slices.IndexFunc(s.cases, func(c Case) bool { return c.Kind == CasePlanHeld })
			if i < 0 || s.cases[i].Status != CaseAtHuman || slices.ContainsFunc(s.cases[i+1:], func(c Case) bool { return c.Kind == CasePlanHeld }) {
				t.Fatalf("the held plan's cases = %+v, want one held ask at the human", s.cases)
			}
			heldID := s.cases[i].ID
			var settle []CriterionVerdict
			for i := 1; i <= 10; i++ {
				id := fmt.Sprintf("k%d", i)
				if path == "drop" {
					if err := DecideCriterion(ctx, s, "t-plan", id, "waived", "sebastian", "the suite moved to another plan"); err != nil {
						t.Fatal(err)
					}
					continue
				}
				settle = append(settle, CriterionVerdict{Criterion: id, Verdict: "met", Evidence: "suite ./... passed", Kind: CriterionInvariant})
			}
			if path == "gate rows" {
				if err := s.PutCriteriaVerdicts(ctx, "card-1", CriteriaSourceGate, FactoryDecider, settle); err != nil {
					t.Fatal(err)
				}
			}
			r.planCloseCheck(ctx, "t-plan")
			if closeRows(s) != 1 {
				t.Fatalf("plan_closed rows %d, want the factory's close", closeRows(s))
			}
			for _, c := range s.cases {
				if c.Kind == CasePlanHeld && (c.Status == CaseOpen || c.Status == CaseAtHuman) {
					t.Fatalf("the held ask %d stands %s on the closed plan", c.ID, c.Status)
				}
			}
			ended := s.cases[slices.IndexFunc(s.cases, func(c Case) bool { return c.ID == heldID })]
			if want := (CaseResolution{By: FactoryDecider, Rung: 0, Action: "withdraw", Reason: "every promise of the plan is settled, and the plan closed"}); ended.Status != CaseWithdrawn ||
				ended.Resolution == nil || !reflect.DeepEqual(*ended.Resolution, want) {
				t.Fatalf("the held ask ended %s by %+v, want withdrawn by %+v", ended.Status, ended.Resolution, want)
			}
		})
	}
}

// A held plan's comment names its open promises, which the planner wrote,
// and posts them through the outbound sanitiser: no mention and no link
// to an outside host reaches the issue as the factory's words.
func TestAHeldPlansCommentSanitisesThePromises(t *testing.T) {
	ctx := context.Background()
	r, s, _, fj := planWithoutCriteria(t, "done")
	p := Plan{Summary: "the plan", Cards: []PlanCard{{Key: "card-1", Role: RoleIntegration, Title: "Integration", Directories: []string{"a"}}},
		Criteria: []PlanCriterion{{ID: "k1", Kind: CriterionInvariant, Suite: "./...", Promise: "@mallory says the export works, see https://evil.test/x"}}}
	newRevision(t, s, "t-plan", p)
	plan := s.tasks["t-plan"]
	plan.Track = policy.TrackPlan
	s.tasks["t-plan"] = plan
	mergeCard(s, "card-1")
	setStatus(s, "t-plan", "closed")
	r.planCloseCheck(ctx, "t-plan")
	var held string
	for _, c := range fj.comments {
		if strings.Contains(c.body, "mallory") {
			held = c.body
		}
	}
	if held == "" || strings.Contains(held, "@mallory") || strings.Contains(held, "https://evil.test") {
		t.Fatalf("the held plan's comment = %q, want the promise with its mention and outside link made harmless", held)
	}
}
