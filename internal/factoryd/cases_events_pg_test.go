package factoryd

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

const caseTask = "t1410"

// caseEventStore is a store whose one task, caseTask, waits on a human
// with a budget of 5 EUR, its cases, events, intake rows, approvals and
// plans empty.
func caseEventStore(t *testing.T) *PGStore {
	t.Helper()
	return cardCaseStore(t, "cases_events")
}

// cardCaseStore is caseEventStore on database name.
func cardCaseStore(t *testing.T, name string) *PGStore {
	t.Helper()
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, name))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.db.Close() })
	if _, err := s.db.ExecContext(ctx, `TRUNCATE cases, events, intake_events, tasks, approval_requests, plan_revisions, plan_questions CASCADE`); err != nil {
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
	if len(steps) != 1 || steps[0].Rung != 3 || steps[0].Actor != FactoryDecider || steps[0].Outcome != StepInProgress || steps[0].EndedAt != nil {
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
		if st.Outcome != StepInProgress || st.Rung != 3 || st.Actor != FactoryDecider {
			t.Fatalf("step = %+v; want a running rung 3 step", st)
		}
		found = append(found, st.Found)
	}
	if want := []string{"", "asked again after a restart", "asked again: " + capAsk}; !slices.Equal(found, want) {
		t.Fatalf("steps found %q, want %q", found, want)
	}
}

func TestAHealedAskWithNoLiveAskOpensAnOtherCase(t *testing.T) {
	s := caseEventStore(t)
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{
		"question": capReached, "comment": int64(27002), "source": "supervisor", "reason": healedReason})

	c := onlyOpenCase(t, s)
	if c.Kind != CaseOther || c.Status != CaseAtHuman || c.Problem != capReached || c.CommentID != 27002 {
		t.Fatalf("case = %+v; want the restart's ask at the human as other", c)
	}
	if steps := caseStepsOf(t, s, c.ID); len(steps) != 1 || steps[0].Found != "asked again after a restart" || steps[0].Outcome != StepInProgress {
		t.Fatalf("steps = %+v; want one running step found asked again after a restart", steps)
	}
}

func TestAnAskWithNoWordsKeepsItsEventAndOpensNoCase(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{"source": "agent", "comment": int64(27001)})

	if cs, err := s.CasesOf(ctx, caseTask); err != nil || len(cs) != 0 {
		t.Fatalf("CasesOf = %+v, %v; want none", cs, err)
	}
	if evs, err := s.TaskEvents(ctx, caseTask, 5); err != nil || len(evs) != 1 || evs[0].Kind != EventQuestionAsked {
		t.Fatalf("TaskEvents = %+v, %v; want the ask", evs, err)
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

// The console's grant and the webhook's answer name the same comment:
// the events' index on the card's answer comment drops the second row,
// so it ends no second case.
func TestOneAnswerCommentEndsOneCase(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	if err := s.CreateTask(ctx, Task{ID: "t1411", Kind: "agent", Status: "waiting-human", Objective: "x", Repository: "sf/r",
		IssueNumber: 1411, Track: "direct", BudgetEUR: 5}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	askAgent(t, s, "Which file should change?", 27001)
	askCap(t, s, 27002)
	if _, err := s.Record(ctx, Event{Repository: "sf/r", TaskID: "t1411", IssueNumber: 1411, Kind: EventQuestionAsked, Actor: FactoryDecider,
		Via: ViaFactoryd, Detail: map[string]any{"question": "Which test is flaky?", "ask": "Which test is flaky?", "comment": int64(27003),
			"source": "agent", "reason": AgentAsks + "Which test is flaky?"}}); err != nil {
		t.Fatalf("Record the other card's ask: %v", err)
	}
	recordCaseEvent(t, s, EventSessionGranted, "sebastian", ViaButton, map[string]any{"comment": int64(27004)})
	recordCaseEvent(t, s, EventQuestionAnswered, "sebastian", ViaWebhook, map[string]any{"answer": "Grant one session", "comment": int64(27004)})

	var resolved []Case
	for _, task := range []string{caseTask, "t1411"} {
		cs, err := s.CasesOf(ctx, task)
		if err != nil {
			t.Fatalf("CasesOf %s: %v", task, err)
		}
		for _, c := range cs {
			if c.Status == CaseResolved {
				resolved = append(resolved, c)
			}
		}
	}
	if len(resolved) != 1 || resolved[0].Kind != CaseSessionCap || resolved[0].TaskID != caseTask {
		t.Fatalf("resolved cases = %+v; want only the cap's", resolved)
	}
}

// An answer row that names its case resolves that case, not the newest
// ask, and nothing when that case ended. A row with no case whose comment
// already answered a case of the card (Act resolved it first) ends no
// second one.
func TestAnAnswerNamingItsCaseResolvesThatCase(t *testing.T) {
	s := caseEventStore(t)
	askAgent(t, s, "Which file should change?", 27001)
	older := onlyOpenCase(t, s)
	askCap(t, s, 27002)
	recordCaseEvent(t, s, EventQuestionAnswered, "sebastian", ViaConsole, map[string]any{"action": ActReplan, "case_id": older.ID, "comment": int64(27003)})
	got := caseByID(t, s, older.ID)
	want := &CaseResolution{By: "sebastian", Rung: 3, Action: ActReplan, Args: map[string]any{"answer": "", "comment": float64(27003)}}
	if got.Status != CaseResolved || !reflect.DeepEqual(got.Resolution, want) {
		t.Fatalf("the named case = %s, %+v; want resolved by %+v", got.Status, got.Resolution, want)
	}
	newer := onlyOpenCase(t, s)
	recordCaseEvent(t, s, EventSessionGranted, "sebastian", ViaConsole, map[string]any{"action": ActExtend, "case_id": older.ID, "comment": int64(27004)})
	if open := onlyOpenCase(t, s); open.ID != newer.ID {
		t.Fatalf("open case = %d, want the newer ask %d left live", open.ID, newer.ID)
	}
	if err := s.ResolveCase(context.Background(), newer.ID, CaseResolution{By: "sebastian", Rung: 3, Action: ActExtend,
		Args: map[string]any{"sessions": 1, "comment": int64(27005)}}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	askAgent(t, s, "Which test is flaky?", 27006)
	third := onlyOpenCase(t, s)
	recordCaseEvent(t, s, EventQuestionAnswered, "sebastian", ViaWebhook, map[string]any{"comment": int64(27005)})
	if open := onlyOpenCase(t, s); open.ID != third.ID {
		t.Fatalf("open case = %d, want %d left live by the webhook's copy of an answered comment", open.ID, third.ID)
	}
}

// A row ends only its own card's case: an answer or a withdrawal on
// caseTask that names another card's case leaves that case at the human.
func TestARowNamingAnotherCardsCaseEndsNothing(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	if err := s.CreateTask(ctx, Task{ID: "t1411", Kind: "agent", Status: "waiting-human", Objective: "y", Repository: "sf/r",
		IssueNumber: 1411, Track: "direct", BudgetEUR: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(ctx, Event{Repository: "sf/r", TaskID: "t1411", IssueNumber: 1411, Kind: EventQuestionAsked, Actor: FactoryDecider,
		Via: ViaFactoryd, Detail: map[string]any{"question": "Which file?", "ask": "Which file?", "comment": int64(27101), "source": "agent",
			"reason": AgentAsks + "Which file?"}}); err != nil {
		t.Fatal(err)
	}
	open, err := s.OpenCasesOf(ctx, "t1411")
	if err != nil || len(open) != 1 {
		t.Fatalf("t1411's cases = %+v, %v; want its ask", open, err)
	}
	other := open[0]
	recordCaseEvent(t, s, EventQuestionAnswered, "sebastian", ViaChat, map[string]any{"action": ActContinue, "case_id": other.ID})
	recordCaseEvent(t, s, EventQuestionWithdrawn, FactoryDecider, ViaFactoryd, map[string]any{"case_id": other.ID, "reason": "the wait ended"})
	if got := caseByID(t, s, other.ID); got.Status != CaseAtHuman || got.Resolution != nil {
		t.Fatalf("t1411's case = %s, %+v; want it at the human", got.Status, got.Resolution)
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
	if want := (&CaseResolution{By: "sebastian", Rung: 3, Action: ActApproveMerge, Reason: consoleApproved}); got.Status != CaseResolved ||
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
	// the factory's denial withdraws: no one decided the merge
	superseded := &CaseResolution{By: FactoryDecider, Rung: 0, Action: "withdraw", Reason: "superseded by a newer request"}
	if got := caseByID(t, s, first.ID); got.Status != CaseWithdrawn || !reflect.DeepEqual(got.Resolution, superseded) {
		t.Fatalf("superseded case = %+v, resolution %+v; want withdrawn %+v", got, got.Resolution, superseded)
	}
	if steps := caseStepsOf(t, s, first.ID); len(steps) != 1 || steps[0].Outcome != StepWithdrawn {
		t.Fatalf("superseded case's steps = %+v; want the one step withdrawn", steps)
	}
	if second.StateKey != "pr:1479@h2" || second.Follows != first.ID {
		t.Fatalf("second merge case = %+v; want pr:1479@h2 following %d", second, first.ID)
	}

	if err := s.ResolveApprovals(ctx, caseTask, ApprovalMerge, "approved", "sebastian", "merged by factory-merge at h2"); err != nil {
		t.Fatalf("ResolveApprovals: %v", err)
	}
	if got := caseByID(t, s, second.ID); got.Status != CaseResolved || got.Resolution.Action != ActApproveMerge || got.Resolution.By != "sebastian" {
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
	if got := caseByID(t, s, third.ID); got.Status != CaseResolved || got.Resolution.Action != ActDenyMerge {
		t.Fatalf("denied case = %+v, resolution %+v", got, got.Resolution)
	}
	if err := s.UndecideApproval(ctx, b, "the signal failed"); err != nil {
		t.Fatalf("UndecideApproval: %v", err)
	}
	if again := onlyOpenCase(t, s); again.ID != third.ID || again.Status != CaseAtHuman || again.Resolution != nil {
		t.Fatalf("undecided merge case = %+v; want case %d at the human again", again, third.ID)
	}
}

// A decision whose signal failed is undone: the merge case it resolved
// waits at the human again with a step that says why, and no second case
// opens, so the digest reads no answer that changed nothing. The wake it
// carried before its end is gone: it is the human's, and not due.
func TestUndecideApprovalReopensTheMergeCaseItsDecisionResolved(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	id := openMerge(t, s, "abc1479")
	c := onlyOpenCase(t, s)
	past := time.Now().Add(-time.Hour)
	if err := s.SetCaseWake(ctx, c.ID, 0, []string{"approval_decided"}, &past); err != nil {
		t.Fatalf("SetCaseWake: %v", err)
	}
	if err := s.DecideApproval(ctx, id, ApprovalMerge, "approved", "sebastian", consoleApproved); err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	if err := s.UndecideApproval(ctx, id, "no workflow for t1410"); err != nil {
		t.Fatalf("UndecideApproval: %v", err)
	}
	got := onlyOpenCase(t, s)
	if got.ID != c.ID || got.Status != CaseAtHuman || got.Resolution != nil || got.ResolvedAt != nil {
		t.Fatalf("undone merge case = %+v, resolution %+v; want case %d at the human, unresolved", got, got.Resolution, c.ID)
	}
	if got.OwnerRung != 3 || len(got.WakeOn) != 0 || got.NextWakeAt != nil {
		t.Fatalf("undone merge case = rung %d, on %v, at %v; want rung 3, no events, no wake", got.OwnerRung, got.WakeOn, got.NextWakeAt)
	}
	if due, err := s.DueCases(ctx, time.Now(), 10); err != nil || len(due) != 0 {
		t.Fatalf("DueCases = %v, %v; want none", caseIDs(due), err)
	}
	if cs, err := s.CasesOf(ctx, caseTask); err != nil || len(cs) != 1 {
		t.Fatalf("CasesOf = %+v, %v; want the one merge case", cs, err)
	}
	steps := caseStepsOf(t, s, c.ID)
	if len(steps) != 2 || steps[0].Outcome != StepResolved || steps[1].Rung != 3 || steps[1].Actor != FactoryDecider ||
		steps[1].Found != "approval undone: no workflow for t1410" || steps[1].Outcome != StepInProgress {
		t.Fatalf("steps = %+v; want the decided step and a running step naming the undo", steps)
	}

	if err := s.DecideApproval(ctx, id, ApprovalMerge, "denied", "sebastian", "changes requested: no"); err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	if got := caseByID(t, s, c.ID); got.Status != CaseResolved || got.Resolution.Action != ActDenyMerge {
		t.Fatalf("decided again = %+v, resolution %+v; want it denied", got, got.Resolution)
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
	if c.Kind != CasePlanQuestion || c.Problem != "Which database?" || c.StateKey != "77a551c3111d00d9@r1" {
		t.Fatalf("plan case = %+v", c)
	}
	recordCaseEvent(t, s, EventPlanQuestionAnswered, "sebastian", ViaButton, map[string]any{"revision": rev, "index": 1, "answer": "Postgres"})
	got := caseByID(t, s, c.ID)
	if want := (&CaseResolution{By: "sebastian", Rung: 3, Action: "answer", Args: map[string]any{"answer": "Postgres"}}); got.Status != CaseResolved ||
		!reflect.DeepEqual(got.Resolution, want) {
		t.Fatalf("answered plan case = %+v, resolution %+v; want %+v", got, got.Resolution, want)
	}
}

// One answer of a plan with three open questions resolves its own question's
// case only; the other two stay at the human, their facts holding the
// planner's recommended answer and grounds.
func TestAPlanAnswerResolvesOnlyItsOwnQuestionsCase(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	rev, err := s.AddPlanRevision(ctx, caseTask, json.RawMessage(`{"summary": "one"}`), "s1")
	if err != nil {
		t.Fatalf("AddPlanRevision: %v", err)
	}
	qs := []PlanQuestion{
		{Text: "Which database?", Choices: []string{"Postgres", "SQLite"}, Recommended: "Postgres", Grounds: "it is shared"},
		{Text: "Which queue?", Choices: []string{"NATS", "none"}, Recommended: "none", Grounds: "one process"},
		{Text: "Which cache?", Choices: []string{"none", "Redis"}, Recommended: "none", Grounds: "no hot path"},
	}
	if _, err := s.PutPlanQuestions(ctx, caseTask, rev, qs); err != nil {
		t.Fatalf("PutPlanQuestions: %v", err)
	}
	for i, q := range qs {
		recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{"question": q.Text, "choices": q.Choices,
			"recommended": q.Recommended, "grounds": q.Grounds, "revision": rev, "index": i + 1})
	}
	recordCaseEvent(t, s, EventPlanQuestionAnswered, "sebastian", ViaButton, map[string]any{"revision": rev, "index": 2, "answer": "none"})
	open, err := s.OpenCasesOf(ctx, caseTask)
	if err != nil || len(open) != 2 || open[0].Problem != "Which database?" || open[1].Problem != "Which cache?" ||
		open[0].Facts["recommended"] != "Postgres" || open[1].Facts["grounds"] != "no hot path" {
		t.Fatalf("open cases after answering question 2 = %+v, %v; want questions 1 and 3", open, err)
	}
}

// The identical re-file's confirmation opens its case and the factory's
// answer resolves it in the same filing, at rung 0, with no human asked.
func TestIdenticalPlanRefileResolvesItsCase(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	raw, err := json.Marshal(testPlan())
	if err != nil {
		t.Fatal(err)
	}
	for n, sha := range []string{"s1", "s2"} {
		if got, err := s.AddPlanRevision(ctx, caseTask, raw, sha); err != nil || got != n+1 {
			t.Fatalf("AddPlanRevision %d: %d, %v", n+1, got, err)
		}
	}
	if _, _, err := s.PutPlanConfirmation(ctx, caseTask, 1, PlanQuestion{Text: "Confirmation of revision 1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AnswerPlanQuestion(ctx, caseTask, 1, 1, DecisionConfirm, "", "sebastian"); err != nil {
		t.Fatal(err)
	}
	r, err := NewRunner(testRunnerConfig(), s, newFakeKube(), &Forgejo{BaseURL: "http://127.0.0.1:1", BotUser: "factory-agent", BotPassword: "bot-pw"},
		&Completion{Store: s}, &fakeObjectStore{}, testSupervisorModel("http://127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := s.Task(ctx, caseTask)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.askConfirmation(ctx, task, 2, testPlan()); err != nil {
		t.Fatalf("askConfirmation: %v", err)
	}
	cs, err := s.CasesOf(ctx, caseTask)
	if err != nil || len(cs) != 1 {
		t.Fatalf("CasesOf = %+v, %v; want the confirmation's one case", cs, err)
	}
	want := &CaseResolution{By: FactoryDecider, Rung: 0, Action: "answer",
		Args: map[string]any{"answer": "the same plan as revision 1, confirmed there", "decision": DecisionConfirm}}
	if c := cs[0]; c.Kind != CasePlanConfirmation || c.Status != CaseResolved || !reflect.DeepEqual(c.Resolution, want) {
		t.Fatalf("case = %+v, resolution %+v; want the plan confirmation resolved %+v", c, c.Resolution, want)
	}
	if open, err := s.OpenPlanQuestions(ctx, caseTask, 2); err != nil || open != 0 {
		t.Fatalf("OpenPlanQuestions = %d, %v; want none", open, err)
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
		{"cap", (&taskRun{cases: versionCases}).plannerTimeCard(TaskUsage{}), CaseAgentTimeCap, nil},
		{"supervisor", "check-in: " + CheckPolicyClass + ": the change touches a migration", CasePolicyClass, map[string]any{"reason_kind": CheckPolicyClass}},
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
		{"stale", staleQuiet + "2 h 0 min while running: is its run gone?", CaseWaitExpired, nil},
		{SourceChat, "Should the API keep v1?", CaseAgentEscalate, nil},
		{"start", startRefused + "c1: the image pull failed", CaseOther, nil},
		{"", "anything", CaseOther, nil},
	} {
		if escalated := tc.source != SourceChat && tc.source != ""; escalated && escalationSource(tc.reason) != tc.source {
			t.Errorf("escalationSource(%q) = %q, want %q", tc.reason, escalationSource(tc.reason), tc.source)
		}
		kind, facts := caseKindOf(tc.source, tc.reason)
		if kind != tc.want || !reflect.DeepEqual(facts, tc.facts) {
			t.Errorf("caseKindOf(%q, %q) = %s, %v; want %s, %v", tc.source, tc.reason, kind, facts, tc.want, tc.facts)
		}
	}
}

func TestCaseKindOfNamesTheBoardCardsBySource(t *testing.T) {
	for source, want := range map[string]CaseKind{SourceProductionHeld: CaseRollHeld, SourceConfig: CaseConfigRefused, SourceStillRed: CaseRedMain} {
		if kind, facts := caseKindOf(source, "the ask"); kind != want || facts != nil {
			t.Errorf("caseKindOf(%q) = %s, %v; want %s", source, kind, facts, want)
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

// NewestAskCase is the one read of the case an answer on the card ends:
// the newest live one, the merge and the plan's questions aside, which
// their own answers end; none once that case ended.
func TestNewestAskCaseIsTheCardsLiveAsk(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	open := func(kind CaseKind, key string) Case {
		c, _, err := s.OpenCase(ctx, Case{TaskID: caseTask, Repository: "sf/r", Kind: kind, StateKey: key, Problem: key, Status: CaseAtHuman})
		if err != nil {
			t.Fatalf("OpenCase %s: %v", kind, err)
		}
		return c
	}
	if c, err := s.NewestAskCase(ctx, caseTask); err != nil || c.ID != 0 {
		t.Fatalf("NewestAskCase with none = %+v, %v; want the zero case", c, err)
	}
	older := open(CaseSessionCap, "h1")
	ask := open(CaseAgentEscalate, "which")
	open(CaseMergeApproval, "pr:9@h1")
	open(CasePlanQuestion, "q1@1")
	if c, err := s.NewestAskCase(ctx, caseTask); err != nil || c.ID != ask.ID || c.Kind != CaseAgentEscalate || c.Problem != "which" {
		t.Fatalf("NewestAskCase = %+v, %v; want case %d, the agent's ask", c, err, ask.ID)
	}
	if err := s.WithdrawCase(ctx, ask.ID, "moved on"); err != nil {
		t.Fatal(err)
	}
	if c, err := s.NewestAskCase(ctx, caseTask); err != nil || c.ID != older.ID {
		t.Fatalf("NewestAskCase after the ask ended = %+v, %v; want case %d", c, err, older.ID)
	}
}

func TestCaseFromAskCarriesTheRecommendedAnswer(t *testing.T) {
	task := Task{ID: "t-plan", Kind: "agent"}
	c, err := caseFromAsk(task, Event{Kind: EventQuestionAsked, Detail: map[string]any{"question": "Which?", "choices": []any{"a", "b"},
		"recommended": "b", "grounds": "because", "revision": 1, "index": 1}}, "")
	if err != nil || c.Kind != CasePlanQuestion || c.Facts["recommended"] != "b" || c.Facts["grounds"] != "because" {
		t.Fatalf("case %+v (%v), want a plan_question with recommended b and its grounds in the facts", c, err)
	}
}

// An ask that names no case still opens its case and is stamped with the
// case's id, so the desk finds it; a proposal on one of two cases at the
// human lands on its own case's face alone, and a recommendation shows
// only with its writer.
func TestTheDeskFindsEveryAskByItsCaseAndAProposalLandsOnItsOwn(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{"question": "Which store?", "ask": "Which store?", "info": "Postgres is shared.",
		"recommendation": "Postgres", "recommended_by": "the agent", "comment": int64(31), "source": "agent",
		"offers": []QuestionChoice{{Value: "say:Postgres", Label: "Postgres"}}})
	cap, _, err := s.OpenCase(ctx, Case{TaskID: caseTask, Repository: "sf/r", Kind: CaseSessionCap, StateKey: "cap", Status: CaseOpen, Problem: "capped"})
	if err != nil {
		t.Fatal(err)
	}
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{"case_id": cap.ID, "ask": "capped?", "info": "cap info"})
	d, err := s.CaseDesk(ctx, "sf/r")
	if err != nil || len(d.Cases) != 2 {
		t.Fatalf("desk = %+v, %v; want two cases", d, err)
	}
	var agent Case
	for _, c := range d.Cases {
		if c.ID != cap.ID {
			agent = c
		}
	}
	face := CaseFace(agent, nil, FactsOf(agent), d.Asks[agent.ID], nil)
	if face.Info != "Postgres is shared." || len(face.Choices) != 1 || face.Choices[0].Value != "say:Postgres" || face.Recommendation != "Postgres" || face.Source != "the agent" {
		t.Errorf("the agent case's face = %+v, want its ask's info, offers and recommendation by the agent", face)
	}
	pre := CaseAction{Action: ActExtend, Args: map[string]any{"sessions": 1}}.preselected()
	if err := s.SetQuestionProposal(ctx, caseTask, cap.ID, false, "extend: one session fixes it", "the factory agent", pre); err != nil {
		t.Fatal(err)
	}
	d, _ = s.CaseDesk(ctx, "sf/r")
	capFace := CaseFace(cap, nil, FactsOf(cap), d.Asks[cap.ID], nil)
	agentFace := CaseFace(agent, nil, FactsOf(agent), d.Asks[agent.ID], nil)
	if capFace.Recommendation != "extend: one session fixes it" || capFace.Source != "the factory agent" || capFace.Preselected != ActExtend {
		t.Errorf("the capped case's face = %+v, want the proposal", capFace)
	}
	if agentFace.Preselected != "" || agentFace.Recommendation != "Postgres" {
		t.Errorf("the agent case's face = %+v, want it untouched by the other case's proposal", agentFace)
	}
	if got := CaseFace(cap, nil, FactsOf(cap), CaseAsk{Detail: map[string]any{"recommendation": "x"}}, nil); got.Recommendation != "" {
		t.Errorf("a recommendation with no writer shows: %+v", got)
	}
}

// A human's answer, not a rung's, is the card's last answer.
func TestCaseDeskLastAnswerIsAHumans(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	c, _, err := s.OpenCase(ctx, Case{TaskID: caseTask, Repository: "sf/r", Kind: CaseOther, StateKey: "k", Status: CaseOpen, Problem: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveCase(ctx, c.ID, CaseResolution{By: "factoryd", Rung: 1, Action: ActContinue}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if d, err := s.CaseDesk(ctx, "sf/r"); err != nil || len(d.Answered) != 0 {
		t.Errorf("a rung's resolution counts as an answer: %v, %v", d.Answered, err)
	}
	c2, _, _ := s.OpenCase(ctx, Case{TaskID: caseTask, Repository: "sf/r", Kind: CaseOther, StateKey: "k2", Status: CaseOpen, Problem: "p2"})
	if err := s.ResolveCase(ctx, c2.ID, CaseResolution{By: "sebastian", Rung: 3, Action: ActContinue}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if d, err := s.CaseDesk(ctx, "sf/r"); err != nil || d.Answered[caseTask].IsZero() {
		t.Errorf("a human's resolution is no answer: %v, %v", d.Answered, err)
	}
}

// An ask that names a case that has ended goes to a new case by its words
// and is stamped with the new case's id, so the face finds it.
func TestAnAskNamingAnEndedCaseNamesTheNewOne(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	old, _, err := s.OpenCase(ctx, Case{TaskID: caseTask, Repository: "sf/r", Kind: CaseOther, StateKey: "old", Status: CaseAtHuman, Problem: "Which store?"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveCase(ctx, old.ID, CaseResolution{By: "sebastian", Rung: 3, Action: ActContinue}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{"case_id": old.ID, "comment": int64(77), "question": "Which store?", "ask": "Which store?",
		"info": "Postgres is shared.", "source": "agent", "offers": []QuestionChoice{{Value: "say:Postgres", Label: "Postgres"}}})
	d, err := s.CaseDesk(ctx, "sf/r")
	if err != nil || len(d.Cases) != 1 || d.Cases[0].ID == old.ID {
		t.Fatalf("desk = %+v, %v; want one new case", d, err)
	}
	face := CaseFace(d.Cases[0], nil, FactsOf(d.Cases[0]), d.Asks[d.Cases[0].ID], nil)
	if face.Info != "Postgres is shared." || len(face.Choices) != 1 {
		t.Errorf("the new case's face = %+v, want the ask's info and choice", face)
	}
}

// An ask that names a case that never was goes to a case by its words and
// is stamped with that case's id.
func TestAnAskNamingNoCaseAtAllNamesTheNewOne(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{"case_id": int64(99999), "comment": int64(78), "question": "Which port?", "ask": "Which port?",
		"info": "8080 is free.", "source": "agent", "offers": []QuestionChoice{{Value: "say:8080", Label: "8080"}}})
	d, err := s.CaseDesk(ctx, "sf/r")
	if err != nil || len(d.Cases) != 1 || d.Cases[0].ID == 99999 {
		t.Fatalf("desk = %+v, %v; want one real case", d, err)
	}
	if face := CaseFace(d.Cases[0], nil, FactsOf(d.Cases[0]), d.Asks[d.Cases[0].ID], nil); face.Info != "8080 is free." || len(face.Choices) != 1 {
		t.Errorf("the case's face = %+v, want the ask's info and choice", face)
	}
}
