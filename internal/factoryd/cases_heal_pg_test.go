package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"reflect"
	"strconv"
	"strings"
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

// escalate inserts escalation row e as written two minutes ago: the heal
// pass leaves a newer ask to its writer.
func escalate(t *testing.T, s *PGStore, e IntakeEvent) {
	t.Helper()
	ctx := context.Background()
	id, err := s.InsertIntakeEvent(ctx, e)
	if err != nil {
		t.Fatalf("InsertIntakeEvent: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE intake_events SET created_at = created_at - interval '2 minutes' WHERE id = $1`, id); err != nil {
		t.Fatalf("age the escalation row: %v", err)
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
		escalate(t, s, e)
	}
	// the card's own move into waiting-human after its ask
	if _, err := s.Record(ctx, Event{Repository: "sf/r", TaskID: caseTask, Kind: EventTaskStatus, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"status": "waiting-human", "gate": ""}}); err != nil {
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

// An ask younger than a minute is its writer's: Escalate records the
// ask's event only after it moves the card and mirrors it.
func TestReconcileCasesLeavesAFreshAskToItsWriter(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "cases_heal")
	id, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: caseTask, Kind: IntakeEscalation, Author: FactoryDecider, Body: AgentAsks + "Which file?",
		CommentID: 27001, Question: &HumanQuestion{Ask: "Which file?", Why: "two files match", Info: plainInfo}})
	if err != nil {
		t.Fatalf("InsertIntakeEvent: %v", err)
	}
	reconcile(t, s, CaseHealReport{})

	if _, err := s.db.ExecContext(ctx, `UPDATE intake_events SET created_at = now() - interval '61 seconds' WHERE id = $1`, id); err != nil {
		t.Fatalf("age the escalation row: %v", err)
	}
	reconcile(t, s, CaseHealReport{Opened: 1})
}

// The heal pass opens a human card's ask as recordAsk's event does: its
// kind from the card's source, with no reason, so the writer's own ask
// finds the same case.
func TestReconcileCasesOpensAMonthCapCardsAskAsAMonthCap(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "cases_heal")
	if err := s.CreateTask(ctx, Task{ID: "t1412", Kind: "human", Status: "waiting-human", Objective: "x", Repository: "sf/r",
		IssueNumber: 1412, Source: SourceMonthCap}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// a title that reads as a supervisor's reason is still the card's ask
	q := HumanQuestion{Ask: "check-in: raise the month cap of sf/r?", Why: "the project spent its month cap", Info: "a raise lets its cards run",
		Choices: []QuestionChoice{{Value: "raise", Label: "Raise the cap"}}}
	escalate(t, s, IntakeEvent{TaskID: "t1412", Kind: IntakeEscalation, Author: "factoryd", Body: q.Ask, Question: &q, Choices: q.Labels()})

	reconcile(t, s, CaseHealReport{Opened: 1})
	open, err := s.OpenCasesOf(ctx, "t1412")
	if err != nil || len(open) != 1 || open[0].Kind != CaseMonthCap || open[0].StateKey != "46282f0a8570b00b" || open[0].Problem != q.Ask {
		t.Fatalf("OpenCasesOf = %+v, %v; want the ask as a month cap", open, err)
	}
	// no row keeps who asked, so the heal claims no asker
	if by, ok := open[0].Facts["by"]; ok {
		t.Fatalf("facts by = %v; want none", by)
	}
	if _, err := s.Record(ctx, Event{Repository: "sf/r", TaskID: "t1412", IssueNumber: 1412, Kind: EventQuestionAsked, Actor: FactoryDecider,
		Via: ViaFactoryd, Detail: map[string]any{"question": q.Ask, "ask": q.Ask, "why": q.Why, "info": q.Info, "source": SourceMonthCap,
			"by": "factoryd", "choices": q.Labels()}}); err != nil {
		t.Fatalf("Record the card's ask: %v", err)
	}
	if again, err := s.OpenCasesOf(ctx, "t1412"); err != nil || len(again) != 1 || again[0].ID != open[0].ID {
		t.Fatalf("OpenCasesOf after the writer's ask = %+v, %v; want the one case", again, err)
	}
}

// The stale sweep's ask opens as wait_expired, as its event does.
func TestReconcileCasesOpensAStaleAskAsAWaitExpired(t *testing.T) {
	s := cardCaseStore(t, "cases_heal")
	// a stale sweep's ask for a card quiet 2 h while running
	reason := staleQuiet + "2 h 0 min while running: is its run gone?\nWhy: nothing reached its timeline or events for that long, and a nudge signal went unanswered.\nRecommendation: answer on the issue."
	escalate(t, s, IntakeEvent{TaskID: caseTask, Kind: IntakeEscalation, Author: FactoryDecider, Body: reason, CommentID: 27001,
		Question: ShapedQuestion(reason, QuestionFacts{}, nil)})

	reconcile(t, s, CaseHealReport{Opened: 1})
	ask := "The card has sat quiet 2 h 0 min while running. Keep waiting?"
	c := onlyOpenCase(t, s)
	if c.Kind != CaseWaitExpired || c.StateKey != "f36b9eb595cc11d6" || c.Problem != ask || c.Facts["source"] != "stale" {
		t.Fatalf("case = %+v; want the sweep's ask as wait_expired", c)
	}
	if reason, _ := c.Facts["reason"].(string); !strings.HasPrefix(reason, staleQuiet+"2 h 0 min while running") {
		t.Fatalf("facts reason = %q; want the sweep's reason", reason)
	}
}

// An escalation row whose question has no ask, or an ask of white space
// alone, opens nothing and logs nothing: no ask, no case.
func TestReconcileCasesPassesOverAQuestionWithNoAsk(t *testing.T) {
	for _, ask := range []string{"", " \t\n "} {
		t.Run(strconv.Quote(ask), func(t *testing.T) {
			ctx := context.Background()
			s := cardCaseStore(t, "cases_heal")
			escalate(t, s, IntakeEvent{TaskID: caseTask, Kind: IntakeEscalation, Author: FactoryDecider, Body: "Which file?",
				Question: &HumanQuestion{Ask: ask, Why: "two files match"}})
			var logs bytes.Buffer
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			reconcile(t, s, CaseHealReport{})
			if strings.Contains(logs.String(), "opens no case") {
				t.Fatalf("logs = %q; want no line for a question with no ask", logs.String())
			}
			if cs, err := s.CasesOf(ctx, caseTask); err != nil || len(cs) != 0 {
				t.Fatalf("CasesOf = %+v, %v; want none", cs, err)
			}
		})
	}
}

// A card that left waiting-human after its ask and came back waits on
// something else, as a planner waits on its plan's questions.
func TestReconcileCasesOpensNoCaseForAnAskTheCardMovedOnFrom(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "cases_heal")
	escalate(t, s, IntakeEvent{TaskID: caseTask, Kind: IntakeEscalation, Author: FactoryDecider, Body: AgentAsks + "Which file?",
		CommentID: 27001, Question: &HumanQuestion{Ask: "Which file?", Why: "two files match", Info: plainInfo}})
	if _, err := s.Record(ctx, Event{Repository: "sf/r", TaskID: caseTask, Kind: EventTaskStatus, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"status": "running", "gate": ""}}); err != nil {
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
	escalate(t, s, IntakeEvent{TaskID: caseTask, Kind: IntakeEscalation, Author: FactoryDecider, Body: AgentAsks + ask,
		CommentID: 27001, Question: &HumanQuestion{Ask: ask, Why: "two files match", Info: plainInfo}})
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

// The human resolves the trip's case after its ask, and the card still
// waits on the human until its run moves it: the heal pass opens no case
// for the ask the resolution answered.
func TestReconcileCasesOpensNoStrayCaseForAnAskResolvedAfterIt(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "cases_heal")
	c := openTripCase(t, s, "3 minutes")
	escalate(t, s, IntakeEvent{TaskID: caseTask, Kind: IntakeEscalation, Author: FactoryDecider, Body: capReached + "\nlast: x",
		CommentID: 27001, Question: &HumanQuestion{Ask: capAsk, Why: "the card used its 3 sessions", Info: plainInfo}})
	if err := s.ResolveCase(ctx, c.ID, CaseResolution{By: "sebastian", Rung: 3, Action: ActExtend}, nil, nil, nil); err != nil {
		t.Fatalf("ResolveCase: %v", err)
	}

	reconcile(t, s, CaseHealReport{})
	if cs, err := s.CasesOf(ctx, caseTask); err != nil || len(cs) != 1 || cs[0].Status != CaseResolved {
		t.Fatalf("CasesOf = %+v, %v; want the one resolved case", cs, err)
	}
}

// A monitor case beside a waiting card is no ask of the card's: the heal
// opens the card's missed ask all the same.
func TestReconcileCasesOpensAMissedAskBesideAMonitorCase(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "cases_heal")
	if _, _, err := s.OpenCase(ctx, Case{TaskID: caseTask, Repository: "sf/r", Kind: CaseMonitorFlag, StateKey: "checkin_cut|+0:20",
		Status: CaseAtHuman, Problem: "Monitor flag: checkin_cut(+0:20)"}); err != nil {
		t.Fatal(err)
	}
	escalate(t, s, IntakeEvent{TaskID: caseTask, Kind: IntakeEscalation, Author: FactoryDecider, Body: AgentAsks + "Which file?", CommentID: 27001,
		Question: &HumanQuestion{Ask: "Which file?", Why: "two files match", Info: plainInfo}})
	reconcile(t, s, CaseHealReport{Opened: 1})
	if c, err := s.NewestAskCase(ctx, caseTask); err != nil || c.Kind != CaseAgentEscalate || c.CommentID != 27001 {
		t.Fatalf("the card's ask = %+v, %v; want the missed ask opened as agent_escalate", c, err)
	}
}
