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
	s := cardCaseStore(t, "cases_heal")
	askCap(t, s, 27001)
	c := onlyOpenCase(t, s)
	setCaseTaskStatus(t, s, "done")

	reconcile(t, s, CaseHealReport{Withdrawn: 1})
	withdrawnFor(t, s, c.ID, "card ended done")
	reconcile(t, s, CaseHealReport{})
}

func TestReconcileCasesOpensACaseForAnAskThatPredatesTheTable(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "cases_heal")
	ask := "Which file should change?"
	for _, e := range []IntakeEvent{
		{TaskID: caseTask, Kind: IntakeEscalation, Author: FactoryDecider, Body: AgentAsks + "Which test is flaky?", CommentID: 27001,
			Question: &HumanQuestion{Ask: "Which test is flaky?", Why: "it failed twice", Info: plainInfo}},
		{TaskID: caseTask, Kind: IntakeEscalation, Author: FactoryDecider, Body: AgentAsks + ask, CommentID: 27002,
			Question: &HumanQuestion{Ask: ask, Why: "two files match", Info: plainInfo, Choices: []QuestionChoice{{Value: "a", Label: "a.go"}}}},
		// a restart's re-ask carries no question of its own
		{TaskID: caseTask, Kind: IntakeEscalation, Author: FactoryDecider, Body: healedReason, CommentID: 27003},
	} {
		if _, err := s.InsertIntakeEvent(ctx, e); err != nil {
			t.Fatalf("InsertIntakeEvent: %v", err)
		}
	}
	// the card's own move into waiting-human after its ask
	if _, err := s.Record(ctx, Event{Repository: "sf/r", TaskID: caseTask, Kind: EventTaskStatus, Actor: FactoryDecider, Via: ViaFactoryd,
		At: time.Now().Add(time.Second), Detail: map[string]any{"status": "waiting-human", "gate": ""}}); err != nil {
		t.Fatalf("Record task_status: %v", err)
	}

	reconcile(t, s, CaseHealReport{Opened: 1})
	c := onlyOpenCase(t, s)
	if c.Kind != CaseAgentEscalate || c.CommentID != 27002 || c.Problem != ask || c.Status != CaseAtHuman || c.StateKey != "7987915008e1e10e" {
		t.Fatalf("case = %+v; want the newest ask at the human as agent_escalate", c)
	}
	if choices, _ := c.Facts["choices"].([]any); len(choices) != 1 || choices[0] != "a.go" || c.Facts["why"] != "two files match" {
		t.Fatalf("facts = %v; want the ask's why and its one choice", c.Facts)
	}
	if steps := caseStepsOf(t, s, c.ID); len(steps) != 1 || steps[0].Rung != 3 || steps[0].Outcome != StepInProgress {
		t.Fatalf("steps = %+v; want one running rung 3 step", steps)
	}
	reconcile(t, s, CaseHealReport{})
}

// A card that left waiting-human after its ask and came back waits on
// something else, as a planner waits on its plan's questions.
func TestReconcileCasesOpensNoCaseForAnAskTheCardMovedOnFrom(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "cases_heal")
	if _, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: caseTask, Kind: IntakeEscalation, Author: FactoryDecider, Body: AgentAsks + "Which file?",
		CommentID: 27001, Question: &HumanQuestion{Ask: "Which file?", Why: "two files match", Info: plainInfo}}); err != nil {
		t.Fatalf("InsertIntakeEvent: %v", err)
	}
	if _, err := s.Record(ctx, Event{Repository: "sf/r", TaskID: caseTask, Kind: EventTaskStatus, Actor: FactoryDecider, Via: ViaFactoryd,
		At: time.Now().Add(time.Second), Detail: map[string]any{"status": "running", "gate": ""}}); err != nil {
		t.Fatalf("Record task_status: %v", err)
	}

	reconcile(t, s, CaseHealReport{})
	if cs, err := s.CasesOf(ctx, caseTask); err != nil || len(cs) != 0 {
		t.Fatalf("CasesOf = %+v, %v; want none", cs, err)
	}
}

// An ask answered while its card still waits had its case: the heal
// pass opens no second one from its escalation row.
func TestReconcileCasesOpensNoSecondCaseForAnAnsweredAsk(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "cases_heal")
	ask := "Which file should change?"
	// Escalate writes the escalation row before the ask's event
	if _, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: caseTask, Kind: IntakeEscalation, Author: FactoryDecider, Body: AgentAsks + ask,
		CommentID: 27001, Question: &HumanQuestion{Ask: ask, Why: "two files match", Info: plainInfo}}); err != nil {
		t.Fatalf("InsertIntakeEvent: %v", err)
	}
	askAgent(t, s, ask, 27001)
	recordCaseEvent(t, s, EventQuestionAnswered, "sebastian", ViaWebhook, map[string]any{"answer": "a.go", "comment": int64(27002)})

	reconcile(t, s, CaseHealReport{})
	if cs, err := s.CasesOf(ctx, caseTask); err != nil || len(cs) != 1 || cs[0].Status != CaseResolved {
		t.Fatalf("CasesOf = %+v, %v; want the one answered case", cs, err)
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
	if want := (&CaseResolution{By: "sebastian", Rung: 3, Action: "approve", Reason: consoleApproved}); got.Status != CaseResolved ||
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
