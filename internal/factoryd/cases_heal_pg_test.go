package factoryd

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func reconcile(t *testing.T, s *PGStore, want CaseHealReport) {
	t.Helper()
	if got, err := s.ReconcileCases(context.Background(), "sf/r"); err != nil || got != want {
		t.Fatalf("ReconcileCases = %+v, %v; want %+v", got, err, want)
	}
}

func setCaseTaskStatus(t *testing.T, s *PGStore, status string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `UPDATE tasks SET status = $2,
		finished_at = CASE WHEN $2 IN ('done', 'failed', 'closed', 'merged') THEN now() END WHERE id = $1`, caseTask, status); err != nil {
		t.Fatalf("set status %s: %v", status, err)
	}
}

func withdrawnFor(t *testing.T, s *PGStore, id int64, reason string) {
	t.Helper()
	got := caseByID(t, s, id)
	want := &CaseResolution{By: FactoryDecider, Rung: 0, Action: "withdraw", Reason: reason}
	if got.Status != CaseWithdrawn || !reflect.DeepEqual(got.Resolution, want) {
		t.Fatalf("case %d = %+v, resolution %+v; want withdrawn %+v", id, got, got.Resolution, want)
	}
	if steps := caseStepsOf(t, s, id); len(steps) != 1 || steps[0].Outcome != StepWithdrawn {
		t.Fatalf("case %d steps = %+v; want the one step withdrawn", id, steps)
	}
}

func askPlan(t *testing.T, s *PGStore, question string, revision int, kind string) {
	t.Helper()
	detail := map[string]any{"question": question, "revision": revision, "index": 1}
	if kind != "" {
		detail["kind"] = kind
	}
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, detail)
}

func TestReconcileCasesWithdrawsTheCasesOfEndedCards(t *testing.T) {
	for _, status := range []string{"done", "failed", "closed"} {
		t.Run(status, func(t *testing.T) {
			s := cardCaseStore(t, "cases_heal")
			askCap(t, s, 27001)
			c := onlyOpenCase(t, s)
			setCaseTaskStatus(t, s, status)

			reconcile(t, s, CaseHealReport{Withdrawn: 1})
			withdrawnFor(t, s, c.ID, "card ended "+status)
			reconcile(t, s, CaseHealReport{})
		})
	}
}

// A monitor case on an ended card is the monitor's to withdraw: the heal
// withdraws the card's own ask and leaves the issue_open_after_end case at
// the human, which asks the issue closed until the issue's close
// withdraws it.
func TestReconcileCasesLeavesAnEndedCardsMonitorCase(t *testing.T) {
	s := cardCaseStore(t, "cases_heal")
	askCap(t, s, 27001)
	own := onlyOpenCase(t, s)
	flag, _, err := s.OpenCase(context.Background(), Case{TaskID: caseTask, Repository: "sf/r", Kind: CaseMonitorFlag, StateKey: "issue_open_after_end|x",
		Status: CaseAtHuman, Problem: "Monitor flag: issue_open_after_end"})
	if err != nil {
		t.Fatal(err)
	}
	setCaseTaskStatus(t, s, "done")
	reconcile(t, s, CaseHealReport{Withdrawn: 1})
	withdrawnFor(t, s, own.ID, "card ended done")
	if got := caseByID(t, s, flag.ID); got.Status != CaseAtHuman {
		t.Fatalf("the monitor case = %s, want it at the human", got.Status)
	}
}

func TestReconcileCasesResolvesAMergeCaseWhoseApprovalWasDecided(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "cases_heal")
	id := openMerge(t, s, "abc1479")
	c := onlyOpenCase(t, s)
	// a decision the events writer did not see, as one before the table,
	// on a card that ended since: the decision is what ended the case
	if _, err := s.db.ExecContext(ctx, `UPDATE approval_requests SET status = 'approved', decided_by = 'sebastian', decided_at = now(),
		decision_note = $2 WHERE id = $1`, id, consoleApproved); err != nil {
		t.Fatalf("approve: %v", err)
	}
	setCaseTaskStatus(t, s, "done")

	reconcile(t, s, CaseHealReport{Resolved: 1})
	got := caseByID(t, s, c.ID)
	if want := (&CaseResolution{By: "sebastian", Rung: 3, Action: ActApproveMerge, Reason: consoleApproved}); got.Status != CaseResolved ||
		!reflect.DeepEqual(got.Resolution, want) {
		t.Fatalf("merge case = %+v, resolution %+v; want resolved %+v", got, got.Resolution, want)
	}

	// the factory's denial decides no merge: it withdraws
	id = openMerge(t, s, "abc1480")
	c = onlyOpenCase(t, s)
	if _, err := s.db.ExecContext(ctx, `UPDATE approval_requests SET status = 'denied', decided_by = $2, decided_at = now(),
		decision_note = $3 WHERE id = $1`, id, FactoryDecider, leftQueue); err != nil {
		t.Fatalf("deny: %v", err)
	}
	reconcile(t, s, CaseHealReport{Withdrawn: 1})
	withdrawnFor(t, s, c.ID, leftQueue)
}

// agedMerge opens a merge request of caseTask for pull request 1479 at
// head abc1479 with no approval_requested row, as one older than the
// cases table, and ages it by age.
func agedMerge(t *testing.T, s *PGStore, age string) int64 {
	t.Helper()
	ctx := context.Background()
	spec, err := json.Marshal(MergeSpec{RiskClass: "standard", HeadSHA: "abc1479"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalMerge, TaskID: caseTask, Project: "sf/r", PRNumber: 1479, Spec: spec})
	if err != nil {
		t.Fatalf("OpenApproval: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE approval_requests SET created_at = now() - $2::interval WHERE id = $1`, id, age); err != nil {
		t.Fatalf("age the request: %v", err)
	}
	return id
}

func TestReconcileCasesOpensTheCaseOfAPendingMergeWithNone(t *testing.T) {
	s := cardCaseStore(t, "cases_heal")
	setCaseTaskStatus(t, s, "in-review")
	id := agedMerge(t, s, "30 seconds")
	// a request younger than a minute is its approval_requested row's
	reconcile(t, s, CaseHealReport{})

	if _, err := s.db.ExecContext(context.Background(), `UPDATE approval_requests SET created_at = now() - interval '2 minutes'`); err != nil {
		t.Fatalf("age the request: %v", err)
	}
	reconcile(t, s, CaseHealReport{Opened: 1})
	c := onlyOpenCase(t, s)
	if c.Kind != CaseMergeApproval || c.StateKey != "pr:1479@abc1479" || c.Status != CaseAtHuman || c.Facts["approval"] != float64(id) {
		t.Fatalf("case = %+v; want the pending request's merge case at the human", c)
	}
	if steps := caseStepsOf(t, s, c.ID); len(steps) != 1 || steps[0].Rung != 3 || steps[0].Outcome != StepInProgress {
		t.Fatalf("steps = %+v; want one running rung 3 step", steps)
	}
	reconcile(t, s, CaseHealReport{})
}

// A pending request on an ended card opens nothing: the ended rule would
// withdraw its case the next minute.
func TestReconcileCasesOpensNoMergeCaseOnAnEndedCard(t *testing.T) {
	s := cardCaseStore(t, "cases_heal")
	setCaseTaskStatus(t, s, "done")
	agedMerge(t, s, "2 minutes")

	reconcile(t, s, CaseHealReport{})
	if cs, err := s.CasesOf(context.Background(), caseTask); err != nil || len(cs) != 0 {
		t.Fatalf("CasesOf = %+v, %v; want none", cs, err)
	}
}

func TestReconcileCasesWithdrawsThePlanQuestionsANewerRevisionReplaced(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "cases_heal")
	for _, sha := range []string{"s1", "s2"} {
		if _, err := s.AddPlanRevision(ctx, caseTask, json.RawMessage(`{"summary": "`+sha+`"}`), sha); err != nil {
			t.Fatalf("AddPlanRevision: %v", err)
		}
	}
	askPlan(t, s, "Which database?", 1, "")
	askPlan(t, s, "Does the spec hold?", 1, QuestionConfirmation)
	askPlan(t, s, "Which cache?", 2, "")
	open, err := s.OpenCasesOf(ctx, caseTask)
	if err != nil || len(open) != 3 {
		t.Fatalf("OpenCasesOf = %+v, %v; want three plan cases", open, err)
	}

	reconcile(t, s, CaseHealReport{Withdrawn: 2})
	withdrawnFor(t, s, open[0].ID, "plan revision 2 replaced it")
	withdrawnFor(t, s, open[1].ID, "plan revision 2 replaced it")
	if live := onlyOpenCase(t, s); live.ID != open[2].ID {
		t.Fatalf("live case = %+v; want revision 2's question", live)
	}
}

// A question a later revision asks again in the same words is that
// revision's own case: the earlier revision's case goes, the later stays.
func TestReconcileCasesKeepsAQuestionALaterRevisionAsksAgain(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "cases_heal")
	for _, sha := range []string{"s1", "s2"} {
		if _, err := s.AddPlanRevision(ctx, caseTask, json.RawMessage(`{"summary": "`+sha+`"}`), sha); err != nil {
			t.Fatalf("AddPlanRevision: %v", err)
		}
	}
	askPlan(t, s, "Which database?", 1, "")
	first := onlyOpenCase(t, s)
	askPlan(t, s, "Which database?", 2, "")
	open, err := s.OpenCasesOf(ctx, caseTask)
	if err != nil || len(open) != 2 || open[1].StateKey != "77a551c3111d00d9@r2" || open[1].Facts["revision"] != float64(2) {
		t.Fatalf("OpenCasesOf = %+v, %v; want a second case keyed to revision 2", open, err)
	}

	reconcile(t, s, CaseHealReport{Withdrawn: 1})
	withdrawnFor(t, s, first.ID, "plan revision 2 replaced it")
	if live := onlyOpenCase(t, s); live.ID != open[1].ID || live.Status != CaseAtHuman {
		t.Fatalf("live case = %+v; want revision 2's question at the human", live)
	}
}

// A plan with no revision row has no newer revision: its case stays.
func TestReconcileCasesKeepsThePlanCaseOfACardWithNoRevision(t *testing.T) {
	s := cardCaseStore(t, "cases_heal")
	askPlan(t, s, "Which database?", 1, "")
	c := onlyOpenCase(t, s)

	reconcile(t, s, CaseHealReport{})
	if got := caseByID(t, s, c.ID); got.Status != CaseAtHuman {
		t.Fatalf("plan case = %+v; want it at the human", got)
	}
}

func TestReconcileCasesWithdrawsTheAsksOfACardThatMovedOn(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "cases_heal")
	askCap(t, s, 27001)
	capCase := onlyOpenCase(t, s)
	askPlan(t, s, "Which database?", 1, "")
	openMerge(t, s, "abc1479")
	setCaseTaskStatus(t, s, "running")

	reconcile(t, s, CaseHealReport{Moved: 1})
	withdrawnFor(t, s, capCase.ID, "the card moved on to running")
	open, err := s.OpenCasesOf(ctx, caseTask)
	if err != nil || len(open) != 2 || open[0].Kind != CasePlanQuestion || open[1].Kind != CaseMergeApproval {
		t.Fatalf("OpenCasesOf = %+v, %v; want the plan question and the merge live", open, err)
	}
}

// openTripCase opens a session cap case on caseTask as OpenTrip opens it,
// open with no face yet, opened ago before now.
func openTripCase(t *testing.T, s *PGStore, ago string) Case {
	t.Helper()
	ctx := context.Background()
	c, _, err := s.OpenCase(ctx, Case{TaskID: caseTask, Repository: "sf/r", Kind: CaseSessionCap, StateKey: "c0ffee1", Problem: capAsk})
	if err != nil {
		t.Fatalf("OpenCase: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE cases SET opened_at = now() - $2::interval WHERE id = $1`, c.ID, ago); err != nil {
		t.Fatalf("age the case: %v", err)
	}
	return c
}

// A trip's case opens before its face, while its card still runs: the
// moved-on sweep leaves a case opened seconds ago alone, and withdraws one
// left open past the ten minutes a face takes at most.
func TestReconcileCasesLeavesATripsCaseToItsFace(t *testing.T) {
	s := cardCaseStore(t, "cases_heal")
	setCaseTaskStatus(t, s, "running")
	fresh := openTripCase(t, s, "5 seconds")

	reconcile(t, s, CaseHealReport{})
	if got := caseByID(t, s, fresh.ID); got.Status != CaseOpen {
		t.Fatalf("case = %+v; want it open, its face to come", got)
	}

	if _, err := s.db.ExecContext(context.Background(), `UPDATE cases SET opened_at = now() - interval '11 minutes' WHERE id = $1`, fresh.ID); err != nil {
		t.Fatalf("age the case: %v", err)
	}
	reconcile(t, s, CaseHealReport{Moved: 1})
	want := &CaseResolution{By: FactoryDecider, Rung: 0, Action: "withdraw", Reason: "the card moved on to running"}
	if got := caseByID(t, s, fresh.ID); got.Status != CaseWithdrawn || !reflect.DeepEqual(got.Resolution, want) {
		t.Fatalf("case = %+v, resolution %+v; want withdrawn %+v", got, got.Resolution, want)
	}
}

// A case a rung below the human holds with a wake is the monitor's sweep's,
// whatever the time: before the wake and past it the moved-on sweep leaves
// it, as the sweep's expiry faces it at the due. Owned by the human, it
// goes.
func TestReconcileCasesLeavesACaseARungHoldsToTheSweep(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "cases_heal")
	setCaseTaskStatus(t, s, "running")
	held := openTripCase(t, s, "36 minutes")
	for _, wake := range []time.Time{time.Now().Add(24 * time.Minute), time.Now().Add(-time.Minute)} {
		if err := s.SetCaseWake(ctx, held.ID, 1, nil, &wake); err != nil {
			t.Fatal(err)
		}
		reconcile(t, s, CaseHealReport{})
		if got := caseByID(t, s, held.ID); got.Status != CaseOpen {
			t.Fatalf("wake %s: case = %+v; want it open, the sweep's", wake, got)
		}
	}
	past := time.Now().Add(-time.Minute)
	if err := s.SetCaseWake(ctx, held.ID, 3, nil, &past); err != nil {
		t.Fatal(err)
	}
	reconcile(t, s, CaseHealReport{Moved: 1})
}
