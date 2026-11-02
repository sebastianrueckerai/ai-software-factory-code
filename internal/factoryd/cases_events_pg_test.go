package factoryd

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

const caseTask = "t1410"

// caseEventStore is a store whose one task, caseTask, waits on a human
// with a budget of 5 EUR, its cases, events and approvals empty.
func caseEventStore(t *testing.T) *PGStore {
	t.Helper()
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "cases_events"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.db.Close() })
	if _, err := s.db.ExecContext(ctx, `TRUNCATE cases, events, tasks, approval_requests, plan_revisions, plan_questions CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := s.CreateTask(ctx, Task{ID: caseTask, Kind: "agent", Status: "waiting-human", Objective: "x", Repository: "sf/r",
		IssueNumber: 1410, Track: "direct", BudgetEUR: 5}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return s
}

func recordCaseEvent(t *testing.T, s *PGStore, kind, actor, via string, detail map[string]any) {
	t.Helper()
	if _, err := s.Record(context.Background(), Event{Repository: "sf/r", TaskID: caseTask, IssueNumber: 1410, Kind: kind,
		Actor: actor, Via: via, Detail: detail}); err != nil {
		t.Fatalf("Record %s: %v", kind, err)
	}
}

const capAsk = "Card 1410 asks: grant one more session?"

// askCap records the session cap's ask as Escalate writes it.
func askCap(t *testing.T, s *PGStore, comment int64) {
	t.Helper()
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{
		"question": capAsk, "ask": capAsk, "comment": comment, "source": "cap",
		"reason": capReached + "\nlast: the review asked for changes",
		"why":    "the card used its 3 sessions", "info": "a grant buys one session",
		"choices": []string{"Grant one session", "Leave it waiting"}})
}

// askAgent records an agent's ask as Escalate writes it.
func askAgent(t *testing.T, s *PGStore, question string, comment int64) {
	t.Helper()
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{
		"question": question, "ask": question, "comment": comment, "source": "agent",
		"reason": AgentAsks + question, "why": "two files match", "info": plainInfo})
}

func onlyOpenCase(t *testing.T, s *PGStore) Case {
	t.Helper()
	open, err := s.OpenCasesOf(context.Background(), caseTask)
	if err != nil || len(open) != 1 {
		t.Fatalf("OpenCasesOf = %+v, %v; want one case", open, err)
	}
	return open[0]
}

func caseByID(t *testing.T, s *PGStore, id int64) Case {
	t.Helper()
	c, err := s.CaseByID(context.Background(), id)
	if err != nil {
		t.Fatalf("CaseByID %d: %v", id, err)
	}
	return c
}

func caseStepsOf(t *testing.T, s *PGStore, id int64) []CaseStep {
	t.Helper()
	steps, err := s.CaseSteps(context.Background(), id)
	if err != nil {
		t.Fatalf("CaseSteps %d: %v", id, err)
	}
	return steps
}

func TestAQuestionAskedOpensACaseAtTheHuman(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	recordCaseEvent(t, s, EventGateHead, FactoryDecider, ViaFactoryd, map[string]any{"gate": GateMerge, "head": "c0ffee1"})
	askCap(t, s, 27001)

	c := onlyOpenCase(t, s)
	if c.Kind != CaseSessionCap || c.Status != CaseAtHuman || c.Problem != capAsk || c.CommentID != 27001 || c.Follows != 0 ||
		c.Repository != "sf/r" || c.StateKey != "c49c754a725936fd@c0ffee1" {
		t.Fatalf("case = %+v", c)
	}
	if choices, _ := c.Facts["choices"].([]any); c.Facts["source"] != "cap" || len(choices) != 2 {
		t.Fatalf("facts = %v; want source cap and two choices", c.Facts)
	}
	if c.Money == nil || *c.Money != (CaseMoney{Kind: "session_cap", BudgetEUR: 5}) {
		t.Fatalf("money = %+v; want the session cap on the card's 5 EUR budget", c.Money)
	}
	steps := caseStepsOf(t, s, c.ID)
	if len(steps) != 1 || steps[0].Rung != 3 || steps[0].Actor != "factory" || steps[0].Outcome != StepInProgress || steps[0].EndedAt != nil {
		t.Fatalf("steps = %+v; want one running rung 3 step", steps)
	}

	if _, err := s.Record(ctx, Event{Repository: "sf/r", TaskID: "t-gone", Kind: EventQuestionAsked, Actor: FactoryDecider,
		Via: ViaFactoryd, Detail: map[string]any{"question": "Is it gone?"}}); err != nil {
		t.Fatalf("Record the ask of a card with no row: %v", err)
	}
	if cs, err := s.CasesOf(ctx, "t-gone"); err != nil || len(cs) != 0 {
		t.Fatalf("CasesOf a card with no row = %+v, %v; want none", cs, err)
	}
	if evs, err := s.TaskEvents(ctx, "t-gone", 5); err != nil || len(evs) != 1 {
		t.Fatalf("TaskEvents of a card with no row = %+v, %v; want its ask", evs, err)
	}
}

func TestAHealedReAskAppendsAStepAndOpensNoSecondCase(t *testing.T) {
	s := caseEventStore(t)
	askCap(t, s, 27001)
	// a restart asks again as Escalate does: the earlier reason's first
	// line, with no five parts and the source of its own reason
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{
		"question": capReached, "comment": int64(27002), "source": "supervisor", "reason": healedReason})
	askCap(t, s, 27003)

	c := onlyOpenCase(t, s)
	steps := caseStepsOf(t, s, c.ID)
	var found []string
	for _, st := range steps {
		if st.Outcome != StepInProgress || st.Rung != 3 || st.Actor != "factory" {
			t.Fatalf("step = %+v; want a running rung 3 step", st)
		}
		found = append(found, st.Found)
	}
	if want := []string{"", "asked again after a restart", "asked again: " + capAsk}; !slices.Equal(found, want) {
		t.Fatalf("steps found %q, want %q", found, want)
	}
}

func TestAnAnswerResolvesTheNewestOpenAskCase(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	askCap(t, s, 27001)
	capCase := onlyOpenCase(t, s)
	recordCaseEvent(t, s, EventQuestionAnswered, "sebastian", ViaConsole, map[string]any{"answer": "Grant one session", "comment": int64(27001)})

	got := caseByID(t, s, capCase.ID)
	want := &CaseResolution{By: "sebastian", Rung: 3, Action: "answer", Args: map[string]any{"answer": "Grant one session", "comment": float64(27001)}}
	if got.Status != CaseResolved || !reflect.DeepEqual(got.Resolution, want) {
		t.Fatalf("answered case = %+v, resolution %+v; want resolved by %+v", got, got.Resolution, want)
	}
	if steps := caseStepsOf(t, s, capCase.ID); len(steps) != 1 || steps[0].Outcome != StepResolved || steps[0].EndedAt == nil {
		t.Fatalf("steps = %+v; want the one step resolved", steps)
	}

	// two live asks: an answer ends the newer; a grant at the session cap
	// is the console's answer, as question_answered is the webhook's
	askAgent(t, s, "Which file should change?", 27002)
	agent := onlyOpenCase(t, s)
	askCap(t, s, 27003)
	recordCaseEvent(t, s, EventSessionGranted, "sebastian", ViaButton, map[string]any{"comment": int64(27004)})
	if open := onlyOpenCase(t, s); open.ID != agent.ID {
		t.Fatalf("open case = %d, want the older ask %d", open.ID, agent.ID)
	}
	all, err := s.CasesOf(ctx, caseTask)
	if err != nil || len(all) != 3 {
		t.Fatalf("CasesOf = %+v, %v; want three cases", all, err)
	}
	grant := &CaseResolution{By: "sebastian", Rung: 3, Action: "answer", Args: map[string]any{"answer": "", "comment": float64(27004)}}
	if all[2].Status != CaseResolved || !reflect.DeepEqual(all[2].Resolution, grant) {
		t.Fatalf("granted case = %+v, resolution %+v; want %+v", all[2], all[2].Resolution, grant)
	}
}

func TestASecondAskAfterAnAnswerOpensASecondCase(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	askCap(t, s, 27001)
	recordCaseEvent(t, s, EventQuestionAnswered, "sebastian", ViaWebhook, map[string]any{"comment": int64(27002)})
	askAgent(t, s, "Which file should change?", 27003)

	all, err := s.CasesOf(ctx, caseTask)
	if err != nil || len(all) != 2 {
		t.Fatalf("CasesOf = %+v, %v; want two cases", all, err)
	}
	first, second := all[0], all[1]
	if first.Status != CaseResolved || first.Kind != CaseSessionCap {
		t.Fatalf("first case = %+v; want the cap resolved", first)
	}
	if second.Status != CaseAtHuman || second.Kind != CaseAgentEscalate || second.Follows != first.ID || second.StateKey != "7987915008e1e10e" || second.Money != nil {
		t.Fatalf("second case = %+v; want the agent's ask at the human, following %d", second, first.ID)
	}
}

func TestAWithdrawalWithdrawsTheCase(t *testing.T) {
	s := caseEventStore(t)
	askCap(t, s, 27001)
	c := onlyOpenCase(t, s)
	recordCaseEvent(t, s, EventQuestionWithdrawn, FactoryDecider, ViaFactoryd, map[string]any{
		"question": capReached, "comment": int64(27001), "reason": "the checks passed while the question waited"})

	got := caseByID(t, s, c.ID)
	if got.Status != CaseWithdrawn || got.Resolution == nil || got.Resolution.Reason != "the checks passed while the question waited" {
		t.Fatalf("withdrawn case = %+v, resolution %+v", got, got.Resolution)
	}
	if steps := caseStepsOf(t, s, c.ID); len(steps) != 1 || steps[0].Outcome != StepWithdrawn {
		t.Fatalf("steps = %+v; want the one step withdrawn", steps)
	}
}

// openMerge opens a merge approval request of caseTask for pull request
// 1479 at head and records its approval_requested row as openMergeRequest
// does.
func openMerge(t *testing.T, s *PGStore, head string) int64 {
	t.Helper()
	spec, err := json.Marshal(MergeSpec{RiskClass: "standard", HeadSHA: head, RedChecks: true})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.OpenApproval(context.Background(), ApprovalRequest{Kind: ApprovalMerge, TaskID: caseTask, Project: "sf/r", PRNumber: 1479, Spec: spec})
	if err != nil {
		t.Fatalf("OpenApproval: %v", err)
	}
	recordCaseEvent(t, s, EventApprovalRequested, FactoryDecider, ViaFactoryd, map[string]any{
		"approval": id, "kind": ApprovalMerge, "pr": int64(1479), "class": "standard", "factory": false, "red_checks": true})
	return id
}

func TestAnApprovalRequestOpensAMergeCaseAndItsDecisionResolvesIt(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	id := openMerge(t, s, "abc1479")

	c := onlyOpenCase(t, s)
	if c.Kind != CaseMergeApproval || c.StateKey != "pr:1479@abc1479" || c.Status != CaseAtHuman ||
		c.Problem != "Pull request #1479 waits for a merge approval" || c.Repository != "sf/r" {
		t.Fatalf("merge case = %+v", c)
	}
	if c.Facts["pr"] != float64(1479) || c.Facts["class"] != "standard" || c.Facts["red_checks"] != true || c.Facts["approval"] != float64(id) {
		t.Fatalf("merge facts = %v", c.Facts)
	}
	if steps := caseStepsOf(t, s, c.ID); len(steps) != 1 || steps[0].Rung != 3 || steps[0].Outcome != StepInProgress {
		t.Fatalf("steps = %+v; want one running rung 3 step", steps)
	}
	// an answer is no word on a merge
	recordCaseEvent(t, s, EventQuestionAnswered, "sebastian", ViaWebhook, map[string]any{"comment": int64(27009)})
	if got := caseByID(t, s, c.ID); got.Status != CaseAtHuman {
		t.Fatalf("merge case after an answer = %+v; want it at the human", got)
	}

	if err := s.DecideApproval(ctx, id, ApprovalMerge, "approved", "sebastian", consoleApproved); err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	got := caseByID(t, s, c.ID)
	if want := (&CaseResolution{By: "sebastian", Rung: 3, Action: "approve", Reason: consoleApproved}); got.Status != CaseResolved ||
		!reflect.DeepEqual(got.Resolution, want) {
		t.Fatalf("decided merge case = %+v, resolution %+v; want %+v", got, got.Resolution, want)
	}
}

// Every write that moves a merge request out of pending ends its case,
// and every write that moves it back opens one.
func TestEveryWayOutOfPendingEndsTheMergeCase(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	openMerge(t, s, "h1")
	first := onlyOpenCase(t, s)
	b := openMerge(t, s, "h2")
	second := onlyOpenCase(t, s)
	superseded := &CaseResolution{By: FactoryDecider, Rung: 0, Action: "deny", Reason: "superseded by a newer request"}
	if got := caseByID(t, s, first.ID); got.Status != CaseResolved || !reflect.DeepEqual(got.Resolution, superseded) {
		t.Fatalf("superseded case = %+v, resolution %+v; want %+v", got, got.Resolution, superseded)
	}
	if second.StateKey != "pr:1479@h2" || second.Follows != first.ID {
		t.Fatalf("second merge case = %+v; want pr:1479@h2 following %d", second, first.ID)
	}

	if err := s.ResolveApprovals(ctx, caseTask, ApprovalMerge, "approved", "sebastian", "merged by factory-merge at h2"); err != nil {
		t.Fatalf("ResolveApprovals: %v", err)
	}
	if got := caseByID(t, s, second.ID); got.Status != CaseResolved || got.Resolution.Action != "approve" || got.Resolution.By != "sebastian" {
		t.Fatalf("merged case = %+v, resolution %+v", got, got.Resolution)
	}

	if err := s.ReopenApproval(ctx, caseTask, ApprovalMerge, "waiting: Forgejo refused the merge"); err != nil {
		t.Fatalf("ReopenApproval: %v", err)
	}
	third := onlyOpenCase(t, s)
	if third.StateKey != "pr:1479@h2" || third.Follows != second.ID || third.Status != CaseAtHuman {
		t.Fatalf("reopened merge case = %+v; want pr:1479@h2 at the human following %d", third, second.ID)
	}

	if err := s.DecideApproval(ctx, b, ApprovalMerge, "denied", "sebastian", "changes requested: no"); err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	if got := caseByID(t, s, third.ID); got.Status != CaseResolved || got.Resolution.Action != "deny" {
		t.Fatalf("denied case = %+v, resolution %+v", got, got.Resolution)
	}
	if err := s.UndecideApproval(ctx, b); err != nil {
		t.Fatalf("UndecideApproval: %v", err)
	}
	if fourth := onlyOpenCase(t, s); fourth.Follows != third.ID || fourth.StateKey != "pr:1479@h2" {
		t.Fatalf("undecided merge case = %+v; want pr:1479@h2 following %d", fourth, third.ID)
	}
}

func TestAPlanAnswerResolvesItsQuestionsCase(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	rev, err := s.AddPlanRevision(ctx, caseTask, json.RawMessage(`{"summary": "one"}`), "s1")
	if err != nil {
		t.Fatalf("AddPlanRevision: %v", err)
	}
	if _, err := s.PutPlanQuestions(ctx, caseTask, rev, []PlanQuestion{{Text: "Which database?", Choices: []string{"Postgres", "SQLite"}}}); err != nil {
		t.Fatalf("PutPlanQuestions: %v", err)
	}
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{
		"question": "Which database?", "choices": []string{"Postgres", "SQLite"}, "revision": rev, "index": 1})
	c := onlyOpenCase(t, s)
	if c.Kind != CasePlanQuestion || c.Problem != "Which database?" || c.StateKey != "77a551c3111d00d9" {
		t.Fatalf("plan case = %+v", c)
	}
	recordCaseEvent(t, s, EventPlanQuestionAnswered, "sebastian", ViaButton, map[string]any{"revision": rev, "index": 1, "answer": "Postgres"})
	got := caseByID(t, s, c.ID)
	if want := (&CaseResolution{By: "sebastian", Rung: 3, Action: "answer", Args: map[string]any{"answer": "Postgres"}}); got.Status != CaseResolved ||
		!reflect.DeepEqual(got.Resolution, want) {
		t.Fatalf("answered plan case = %+v, resolution %+v; want %+v", got, got.Resolution, want)
	}
}

func TestCaseKindOfReadsTheSourceAndThePrefixes(t *testing.T) {
	run := &taskRun{}
	for _, tc := range []struct {
		source, reason string
		want           CaseKind
		facts          map[string]any
	}{
		{"cap", run.sessionCapCard(capReached, "the review asked for changes", []string{"the test is red"}), CaseSessionCap, nil},
		{"cap", run.sessionCapCard(timeCapReached, "the session ran out of time", nil), CaseAgentTimeCap, nil},
		{"cap", run.plannerTimeCard(TaskUsage{}), CaseAgentTimeCap, nil},
		{"supervisor", "check-in: " + CheckPolicyClass + ": the change touches a migration", CaseCheckinHuman, map[string]any{"reason_kind": CheckPolicyClass}},
		{"supervisor", "check-in: " + CheckAccess + ": the card needs a key the factory lacks", CaseCheckinHuman, map[string]any{"reason_kind": CheckAccess}},
		{"supervisor", checkinShareStop + " (0.40 of 0.40 EUR)", CaseCheckinShare, nil},
		{"supervisor", checkinSpendAsk + deadlockAskMark + ": the head c1 and the card's limits are unchanged since the last stop. Extend the card, overrule the review, end the card, or leave it waiting.",
			CaseFindingsRepeated, nil},
		{"supervisor", gateStopped + ActivityModelReview + " after 3 tries: the review timed out", CaseGateStop, map[string]any{"gate": ActivityModelReview}},
		{"supervisor", screeningAsks + "the report asks for a token", CaseScreeningSuspect, nil},
		{"supervisor", depEndedReason(Dependency{Issue: 1400, Key: "api", Status: "closed"}), CaseDependencyClosed, nil},
		{"supervisor", healedReason, CaseOther, nil},
		{"budget", budgetRefused + ": 0.02 EUR left", CaseBudgetFloor, nil},
		{SourceMonthCap, monthCapReached + " for 2026-11", CaseMonthCap, nil},
		{"project_cap", projectCapReached + " for sf/r", CaseProjectCap, nil},
		{"day_cap", dayCapReached + " for 2026-11-02", CaseDayCap, nil},
		{"provider", providerRefused + "glm-5", CaseProviderRefused, nil},
		{"queue", leftQueue, CaseGateStop, nil},
		{"capacity", podNeverStarted + "3 tries", CasePodNeverStarted, nil},
		{"no_answer", noModelAnswer + "3 sessions", CaseNoAnswer, nil},
		{"content_filter", contentFilterRefusal + "the prompt", CaseContentFilter, nil},
		{"image", imageWaited + "sf/img:1", CaseImageWait, nil},
		{"agent", AgentAsks + "Which file should change?", CaseAgentEscalate, nil},
		{"stale", "The card was quiet for 2 h 0 min while running: is its run gone?", CaseWaitExpired, nil},
		{SourceChat, "Should the API keep v1?", CaseAgentEscalate, nil},
		{"start", startRefused + "c1: the image pull failed", CaseOther, nil},
		{"", "anything", CaseOther, nil},
	} {
		if escalated := tc.source != "stale" && tc.source != SourceChat && tc.source != ""; escalated && escalationSource(tc.reason) != tc.source {
			t.Errorf("escalationSource(%q) = %q, want %q", tc.reason, escalationSource(tc.reason), tc.source)
		}
		kind, facts := caseKindOf(tc.source, tc.reason)
		if kind != tc.want || !reflect.DeepEqual(facts, tc.facts) {
			t.Errorf("caseKindOf(%q, %q) = %s, %v; want %s, %v", tc.source, tc.reason, kind, facts, tc.want, tc.facts)
		}
	}
}

func TestCaseFromAskNamesAHumanCardsAskAndRefusesNoAsk(t *testing.T) {
	human := Task{ID: "h1", Kind: "human", Repository: "sf/r", BudgetEUR: 1}
	c, err := caseFromAsk(human, Event{Kind: EventQuestionAsked, Detail: map[string]any{"question": "Roll  the release?", "source": SourceRollRefused}}, "c1")
	if err != nil || c.Kind != CaseHumanCard || c.StateKey != "bec366272b7d4a4f@c1" || c.Problem != "Roll  the release?" || c.Money != nil || c.Status != CaseAtHuman {
		t.Fatalf("caseFromAsk of a human card = %+v, %v", c, err)
	}
	if _, err := caseFromAsk(human, Event{Kind: EventQuestionAsked, Detail: map[string]any{"source": "agent"}}, ""); err == nil {
		t.Fatal("caseFromAsk of an ask with no words = nil error")
	}
}
