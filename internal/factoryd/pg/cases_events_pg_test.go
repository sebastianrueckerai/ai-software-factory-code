package pg

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"
)

// askAgent records an agent's ask as Escalate writes it.
func askAgent(t *testing.T, s *PGStore, question string, comment int64) {
	t.Helper()
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, map[string]any{
		"question": question, "ask": question, "comment": comment, "source": "agent",
		"reason": AgentAsks + question, "why": "two files match"})
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
		"question": CapReached, "comment": int64(27002), "source": "supervisor", "reason": HealedReason})
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
		"question": CapReached, "comment": int64(27002), "source": "supervisor", "reason": HealedReason})

	c := onlyOpenCase(t, s)
	if c.Kind != CaseOther || c.Status != CaseAtHuman || c.Problem != CapReached || c.CommentID != 27002 {
		t.Fatalf("case = %+v; want the restart's ask at the human as other", c)
	}
	if steps := caseStepsOf(t, s, c.ID); len(steps) != 1 || steps[0].Found != "asked again after a restart" || steps[0].Outcome != StepInProgress {
		t.Fatalf("steps = %+v; want one running step found asked again after a restart", steps)
	}
}

func TestAnAnswerResolvesTheNewestOpenAskCase(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	askAgent(t, s, "Which file should change?", 27001)
	ask := onlyOpenCase(t, s)
	recordCaseEvent(t, s, EventQuestionAnswered, "sebastian", ViaConsole, map[string]any{"answer": "the shared one", "comment": int64(27001)})

	got := caseByID(t, s, ask.ID)
	want := &CaseResolution{By: "sebastian", Rung: 3, Action: "answer", Args: map[string]any{"answer": "the shared one", "comment": float64(27001)}}
	if got.Status != CaseResolved || !reflect.DeepEqual(got.Resolution, want) {
		t.Fatalf("answered case = %+v, resolution %+v; want resolved by %+v", got, got.Resolution, want)
	}
	if steps := caseStepsOf(t, s, ask.ID); len(steps) != 1 || steps[0].Outcome != StepResolved || steps[0].EndedAt == nil {
		t.Fatalf("steps = %+v; want the one step resolved", steps)
	}

	// two live asks: an answer ends the newer; a grant at the session cap
	// is the console's answer, as question_answered is the webhook's
	askAgent(t, s, "Which store?", 27002)
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

// The webhook's copy of a comment on the card's issue resolves no cap ask
// (a reply alone starts none), so the second ask opens a second live case
// beside the cap's, and the card's newest live ask is the newer one.
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
	if first.Status != CaseAtHuman || first.Kind != CaseSessionCap || first.Resolution != nil {
		t.Fatalf("first case = %+v; want the cap still at the human: a reply alone starts none", first)
	}
	if second.Status != CaseAtHuman || second.Kind != CaseAgentEscalate || second.Follows != 0 || second.StateKey != "7987915008e1e10e" || second.Money != nil {
		t.Fatalf("second case = %+v; want the agent's ask at the human beside the cap", second)
	}
	if ask, err := s.NewestAskCase(ctx, caseTask); err != nil || ask.ID != second.ID {
		t.Fatalf("NewestAskCase = %+v, %v; want the agent's ask %d", ask, err, second.ID)
	}
}

// seededAsk opens task taskID's live ask case of kind by recording its
// ask as the kind's own source writes it, and tells the case's ask
// sentence.
func seededAsk(t *testing.T, s *PGStore, taskID string, issue int64, kind CaseKind, comment int64) Case {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateTask(ctx, Task{ID: taskID, Kind: "agent", Status: "waiting-human", Objective: "x", Repository: "sf/r",
		IssueNumber: issue, Track: "direct", BudgetEUR: 5}); err != nil {
		t.Fatalf("CreateTask %s: %v", taskID, err)
	}
	source, reason := "cap", CapReached
	switch kind {
	case CaseAgentTimeCap:
		reason = TimeCapReached
	case CaseDayCap:
		source, reason = "day_cap", DayCapReached+" for 2026-11-02"
	case CaseMonthCap:
		source, reason = SourceMonthCap, monthCapReached+" for 2026-11"
	case CaseProjectCap:
		source, reason = "project_cap", projectCapReached+" for sf/r"
	case CaseBudgetFloor:
		source, reason = "budget", BudgetRefused+": 0.02 EUR left"
	}
	if _, err := s.Record(ctx, Event{Repository: "sf/r", TaskID: taskID, IssueNumber: issue, Kind: EventQuestionAsked,
		Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"question": reason, "ask": reason,
			"comment": comment, "source": source, "reason": reason}}); err != nil {
		t.Fatalf("Record %s's ask: %v", taskID, err)
	}
	open, err := s.OpenCasesOf(ctx, taskID)
	if err != nil || len(open) != 1 || open[0].Kind != kind {
		t.Fatalf("%s's cases = %+v, %v; want one live %s ask", taskID, open, err, kind)
	}
	return open[0]
}

// answerOn records taskID's answer row: kind, actor, via and detail.
func answerOn(t *testing.T, s *PGStore, taskID string, issue int64, kind, actor, via string, detail map[string]any) {
	t.Helper()
	if _, err := s.Record(context.Background(), Event{Repository: "sf/r", TaskID: taskID, IssueNumber: issue, Kind: kind,
		Actor: actor, Via: via, Detail: detail}); err != nil {
		t.Fatalf("Record %s's %s: %v", taskID, kind, err)
	}
}

// The reply-alone contract, seeded as a table of all six kinds: a live
// ask of every kind whose ladder line reads "A reply alone starts none" —
// the five caps and the budget floor — stays at the human under a
// question_answered row that names no action, the webhook's copy of a
// comment on the card's issue, and each card's newest live ask is
// unchanged, the budget floor's fund ask included.
func TestAnEmptyAnswerKeepsAReplyAloneAskLive(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	kinds := []struct {
		task string
		kind CaseKind
	}{
		{"t-at", CaseAgentTimeCap}, {"t-sc", CaseSessionCap}, {"t-dc", CaseDayCap},
		{"t-mc", CaseMonthCap}, {"t-pc", CaseProjectCap}, {"t-bf", CaseBudgetFloor},
	}
	live := map[CaseKind]Case{}
	for n, k := range kinds {
		live[k.kind] = seededAsk(t, s, k.task, int64(1600+n), k.kind, int64(28100+n))
	}
	for n, k := range kinds {
		// the webhook's copy of a comment on the card's issue: only the
		// comment id, no action
		answerOn(t, s, k.task, int64(1600+n), EventQuestionAnswered, "sebastian", ViaWebhook,
			map[string]any{"comment": int64(28200 + n)})
	}
	for _, k := range kinds {
		want := live[k.kind]
		got := caseByID(t, s, want.ID)
		if got.Status != CaseAtHuman || got.Resolution != nil || got.ResolvedAt != nil {
			t.Fatalf("%s's %s case = %+v; want it still at the human, unresolved", k.task, k.kind, got)
		}
		ask, err := s.NewestAskCase(ctx, k.task)
		if err != nil || ask.ID != want.ID || ask.Kind != k.kind {
			t.Fatalf("%s's newest live ask = %+v, %v; want case %d, the ask that stood", k.task, ask, err, want.ID)
		}
	}
}

// An answer that carries its action still answers a cap ask: a
// question_answered row naming extend with agent time resolves the
// agent-time cap case, and a session_granted row at a session cap
// resolves it as today, a grant being itself the action.
func TestAnActionBearingAnswerStillResolvesACapAsk(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	timeCap := seededAsk(t, s, "t-at", 1610, CaseAgentTimeCap, 28110)
	sessionCap := seededAsk(t, s, "t-sc", 1611, CaseSessionCap, 28111)

	answerOn(t, s, "t-at", 1610, EventQuestionAnswered, "sebastian", ViaChat, map[string]any{
		"action": ActExtend, "agent_time_s": 1800, "comment": int64(28210)})
	answerOn(t, s, "t-sc", 1611, EventSessionGranted, "sebastian", ViaButton, map[string]any{"comment": int64(28211)})

	got := caseByID(t, s, timeCap.ID)
	extend := &CaseResolution{By: "sebastian", Rung: 3, Action: ActExtend, Args: map[string]any{"answer": "", "comment": float64(28210)}}
	if got.Status != CaseResolved || !reflect.DeepEqual(got.Resolution, extend) {
		t.Fatalf("the agent-time cap = %s, %+v; want resolved by %+v", got.Status, got.Resolution, extend)
	}
	grant := caseByID(t, s, sessionCap.ID)
	want := &CaseResolution{By: "sebastian", Rung: 3, Action: CaseAnswer, Args: map[string]any{"answer": "", "comment": float64(28211)}}
	if grant.Status != CaseResolved || !reflect.DeepEqual(grant.Resolution, want) {
		t.Fatalf("the session cap = %s, %+v; want resolved by %+v", grant.Status, grant.Resolution, want)
	}
	for _, taskID := range []string{"t-at", "t-sc"} {
		if ask, err := s.NewestAskCase(ctx, taskID); err != nil || ask.ID != 0 {
			t.Fatalf("%s's newest live ask = %+v, %v; want none", taskID, ask, err)
		}
	}
}

func TestAWithdrawalWithdrawsTheCase(t *testing.T) {
	s := caseEventStore(t)
	askCap(t, s, 27001)
	c := onlyOpenCase(t, s)
	recordCaseEvent(t, s, EventQuestionWithdrawn, FactoryDecider, ViaFactoryd, map[string]any{
		"question": CapReached, "comment": int64(27001), "reason": "the checks passed while the question waited"})

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

	if err := s.DecideApproval(ctx, id, ApprovalMerge, "approved", "sebastian", ConsoleApproved); err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	got := caseByID(t, s, c.ID)
	if want := (&CaseResolution{By: "sebastian", Rung: 3, Action: ActApproveMerge, Reason: ConsoleApproved}); got.Status != CaseResolved ||
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
	if err := s.SetCaseBrief(ctx, c.ID, "the brief before the undo", "grounds"); err != nil {
		t.Fatalf("SetCaseBrief: %v", err)
	}
	if err := s.DecideApproval(ctx, id, ApprovalMerge, "approved", "sebastian", ConsoleApproved); err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	if err := s.UndecideApproval(ctx, id, "no workflow for t1410"); err != nil {
		t.Fatalf("UndecideApproval: %v", err)
	}
	got := onlyOpenCase(t, s)
	if got.Brief != "" || got.BriefDetail != "" {
		t.Fatalf("the undone case keeps its brief %q / %q; want it cleared for the writer", got.Brief, got.BriefDetail)
	}
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
	if err := r.AskConfirmation(ctx, task, 2, testPlan()); err != nil {
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
		{"cap", run.PgSessionCapCard(CapReached, "the review asked for changes", []string{"the test is red"}), CaseSessionCap, nil},
		{"cap", run.PgSessionCapCard(TimeCapReached, "the session ran out of time", nil), CaseAgentTimeCap, nil},
		{"cap", run.PgPlannerTimeCard(TaskUsage{}), CaseAgentTimeCap, nil},
		{"supervisor", "check-in: " + CheckPolicyClass + ": the change touches a migration", CasePolicyClass, map[string]any{"reason_kind": CheckPolicyClass}},
		{"supervisor", "check-in: " + CheckAccess + ": the card needs a key the factory lacks", CaseCheckinHuman, map[string]any{"reason_kind": CheckAccess}},
		{"supervisor", "check-in: " + CheckSpend + ": " + spentStop + " (0.40 of 0.40 EUR)", CaseCheckinShare, nil},
		{"supervisor", CheckinSpendAsk + DeadlockAskMark + ": the head c1 and the card's limits are unchanged since the last stop. Extend the card, overrule the review, end the card, or leave it waiting.",
			CaseFindingsRepeated, nil},
		{"supervisor", GateStopped + ActivityModelReview + " after 3 tries: the review timed out", CaseGateStop, map[string]any{"gate": ActivityModelReview}},
		{"supervisor", screeningAsks + "the report asks for a token", CaseScreeningSuspect, nil},
		{"supervisor", DepEndedReason(Dependency{Issue: 1400, Key: "api", Status: "closed"}), CaseDependencyClosed, nil},
		{"supervisor", HealedReason, CaseOther, nil},
		{"budget", BudgetRefused + ": 0.02 EUR left", CaseBudgetFloor, nil},
		{SourceMonthCap, monthCapReached + " for 2026-11", CaseMonthCap, nil},
		{"project_cap", projectCapReached + " for sf/r", CaseProjectCap, nil},
		{"day_cap", DayCapReached + " for 2026-11-02", CaseDayCap, nil},
		{"provider", providerRefused + "glm-5", CaseProviderRefused, nil},
		{"queue", leftQueue, CaseGateStop, nil},
		{"capacity", podNeverStarted + "3 tries", CasePodNeverStarted, nil},
		{"no_answer", noModelAnswer + "3 sessions", CaseNoAnswer, nil},
		{"content_filter", ContentFilterRefusal + "the prompt", CaseContentFilter, nil},
		{"image", ImageWaited + "sf/img:1", CaseImageWait, nil},
		{"agent", AgentAsks + "Which file should change?", CaseAgentEscalate, nil},
		{"stale", staleQuiet + "2 h 0 min while running: is its run gone?", CaseWaitExpired, nil},
		{SourceChat, "Should the API keep v1?", CaseAgentEscalate, nil},
		{"start", startRefused + "c1: the image pull failed", CaseOther, nil},
		{"", "anything", CaseOther, nil},
	} {
		if escalated := tc.source != SourceChat && tc.source != ""; escalated && EscalationSource(tc.reason) != tc.source {
			t.Errorf("escalationSource(%q) = %q, want %q", tc.reason, EscalationSource(tc.reason), tc.source)
		}
		kind, facts := CaseKindOf(tc.source, tc.reason)
		if kind != tc.want || !reflect.DeepEqual(facts, tc.facts) {
			t.Errorf("caseKindOf(%q, %q) = %s, %v; want %s, %v", tc.source, tc.reason, kind, facts, tc.want, tc.facts)
		}
	}
}

func TestCaseKindOfNamesTheBoardCardsBySource(t *testing.T) {
	for source, want := range map[string]CaseKind{SourceProductionHeld: CaseRollHeld, SourceConfig: CaseConfigRefused, SourceStillRed: CaseRedMain} {
		if kind, facts := CaseKindOf(source, "the ask"); kind != want || facts != nil {
			t.Errorf("caseKindOf(%q) = %s, %v; want %s", source, kind, facts, want)
		}
	}
}

func TestCaseFromAskNamesAHumanCardsAskAndRefusesNoAsk(t *testing.T) {
	human := Task{ID: "h1", Kind: "human", Repository: "sf/r", BudgetEUR: 1}
	c, err := CaseFromAsk(human, Event{Kind: EventQuestionAsked, Detail: map[string]any{"question": "Roll  the release?", "source": SourceRollRefused}}, "c1")
	if err != nil || c.Kind != CaseHumanCard || c.StateKey != "bec366272b7d4a4f@c1" || c.Problem != "Roll  the release?" || c.Money != nil || c.Status != CaseAtHuman {
		t.Fatalf("caseFromAsk of a human card = %+v, %v", c, err)
	}
	if _, err := CaseFromAsk(human, Event{Kind: EventQuestionAsked, Detail: map[string]any{"source": "agent"}}, ""); err == nil {
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
	c, err := CaseFromAsk(task, Event{Kind: EventQuestionAsked, Detail: map[string]any{"question": "Which?", "choices": []any{"a", "b"},
		"recommended": "b", "grounds": "because", "revision": 1, "index": 1}}, "")
	if err != nil || c.Kind != CasePlanQuestion || c.Facts["recommended"] != "b" || c.Facts["grounds"] != "because" {
		t.Fatalf("case %+v (%v), want a plan_question with recommended b and its grounds in the facts", c, err)
	}
}

// withRoom is an agent's ask row with the facts of a card that has room
// for a session, so its answers can happen.
func withRoom(detail map[string]any) map[string]any {
	maps.Copy(detail, map[string]any{"attempts": 1, "session_cap": 3, "agent_time_cap_s": 7200.0, "session_least_s": 300.0,
		"budget_eur": 2.0, "left_eur": 1.5, "floor_eur": 0.3})
	return detail
}

// An ask that names no case still opens its case and is stamped with the
// case's id, so the desk finds it; a proposal on one of two cases at the
// human lands on its own case's face alone, and a recommendation's words
// show only when it names an answer.
func TestTheDeskFindsEveryAskByItsCaseAndAProposalLandsOnItsOwn(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, withRoom(map[string]any{"question": "Which store?", "ask": "Which store?", "info": "Postgres is shared.",
		"recommendation": "Postgres is the one store", "recommended_by": "the agent", "preselected": map[string]any{"action": "say:Postgres"},
		"comment": int64(31), "source": "agent", "choices": []string{"Postgres", "SQLite"}}))
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
	face := CaseFace(agent, caseCard, d.Asks[agent.ID])
	if face.Info != "Postgres is shared." || len(face.Offers) == 0 || face.Offers[0].Value != "say:Postgres" || face.Recommendation != "Postgres is the one store" ||
		face.Source != "the agent" || face.Preselected != "say:Postgres" {
		t.Errorf("the agent case's face = %+v, want its ask's info, offers and recommendation by the agent", face)
	}
	pre := CaseAction{Action: ActExtend, Args: map[string]any{"sessions": 1}}.PgPreselected()
	if err := s.SetQuestionProposal(ctx, caseTask, cap.ID, "one session fixes it", "the factory agent", pre); err != nil {
		t.Fatal(err)
	}
	d, _ = s.CaseDesk(ctx, "sf/r")
	capFace := CaseFace(cap, caseCard, d.Asks[cap.ID])
	agentFace := CaseFace(agent, caseCard, d.Asks[agent.ID])
	if capFace.Recommendation != "one session fixes it" || capFace.Source != "the factory agent" || capFace.Preselected != ActExtend {
		t.Errorf("the capped case's face = %+v, want the proposal", capFace)
	}
	if agentFace.Preselected != "say:Postgres" || agentFace.Recommendation != "Postgres is the one store" {
		t.Errorf("the agent case's face = %+v, want it untouched by the other case's proposal", agentFace)
	}
	if got := CaseFace(cap, caseCard, CaseAsk{Detail: map[string]any{"recommendation": "x"}}); got.Recommendation != "" {
		t.Errorf("a recommendation with no writer shows: %+v", got)
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
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, withRoom(map[string]any{"case_id": old.ID, "comment": int64(77), "question": "Which store?", "ask": "Which store?",
		"info": "Postgres is shared.", "source": "agent", "choices": []string{"Postgres", "SQLite"}}))
	d, err := s.CaseDesk(ctx, "sf/r")
	if err != nil || len(d.Cases) != 1 || d.Cases[0].ID == old.ID {
		t.Fatalf("desk = %+v, %v; want one new case", d, err)
	}
	face := CaseFace(d.Cases[0], caseCard, d.Asks[d.Cases[0].ID])
	if face.Info != "Postgres is shared." || len(face.Offers) == 0 || face.Offers[0].Value != "say:Postgres" {
		t.Errorf("the new case's face = %+v, want the ask's info and choice", face)
	}
}

// An ask that names a case that never was goes to a case by its words and
// is stamped with that case's id.
func TestAnAskNamingNoCaseAtAllNamesTheNewOne(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	recordCaseEvent(t, s, EventQuestionAsked, FactoryDecider, ViaFactoryd, withRoom(map[string]any{"case_id": int64(99999), "comment": int64(78), "question": "Which port?", "ask": "Which port?",
		"info": "8080 is free.", "source": "agent", "choices": []string{"8080", "9090"}}))
	d, err := s.CaseDesk(ctx, "sf/r")
	if err != nil || len(d.Cases) != 1 || d.Cases[0].ID == 99999 {
		t.Fatalf("desk = %+v, %v; want one real case", d, err)
	}
	if face := CaseFace(d.Cases[0], caseCard, d.Asks[d.Cases[0].ID]); face.Info != "8080 is free." || len(face.Offers) == 0 || face.Offers[0].Value != "say:8080" {
		t.Errorf("the case's face = %+v, want the ask's info and choice", face)
	}
}

// The board shows the ask row of the card's own case: a newer question_asked
// row of another card that names the same case id is not read for the card's
// question or choices.
func TestTheBoardReadsOnlyTheAskRowsOfItsOwnCard(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	askAgent(t, s, "Which store?", 27001)
	c := onlyOpenCase(t, s)
	if _, err := s.Record(ctx, Event{Repository: "sf/r", TaskID: "t-other", Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"case_id": c.ID, "ask": "Which colour?", "question": "Which colour?", "choices": []string{"Red"}, "comment": int64(27002)}}); err != nil {
		t.Fatal(err)
	}
	b, ok, err := s.BoardTask(ctx, caseTask)
	if err != nil || !ok {
		t.Fatalf("BoardTask = %v, %v", ok, err)
	}
	if b.Question == nil || b.Question.Ask != "Which store?" || len(b.Choices) != 0 || b.QuestionComment != 27001 {
		t.Fatalf("board ask %+v, choices %v, comment %d; want the card's own ask, no choices, comment 27001", b.Question, b.Choices, b.QuestionComment)
	}
}

// CreateHumanCard writes the card's row and its ask row in one transaction:
// an ask the store refuses leaves no card, the ask row carries the card's id
// and issue, a card of the id that stands answers itself and writes no
// second ask, a plan card whose key already has its row answers that row and
// writes no second ask, and the ask opens its case at the human.
func TestCreateHumanCardWritesTheCardAndItsAskOrNeither(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	card := Task{ID: "t-h", Kind: "human", Status: "waiting-human", Repository: "sf/r", IssueNumber: 31, BudgetEUR: 1, Objective: "x"}
	ask := HumanAsk{Q: HumanQuestion{Ask: "Which?"}, By: "factoryd", Asker: "webhook", Via: ViaFactoryd}
	bad := ask
	bad.Facts = map[string]any{"unwritable": make(chan int)}
	if _, _, err := s.CreateHumanCard(ctx, card, bad); err == nil {
		t.Fatal("CreateHumanCard with an ask the store refuses succeeded")
	}
	if _, ok, err := s.Task(ctx, "t-h"); err != nil || ok {
		t.Fatalf("card after the failed transaction: stands %v, %v; want no row", ok, err)
	}
	if _, created, err := s.CreateHumanCard(ctx, card, ask); err != nil || !created {
		t.Fatalf("CreateHumanCard = %v, %v", created, err)
	}
	if cs, err := s.CasesOf(ctx, "t-h"); err != nil || len(cs) != 1 || cs[0].Status != CaseAtHuman {
		t.Fatalf("cases = %+v, %v; want the ask's case at the human", cs, err)
	}
	second := card
	second.IssueNumber = 32
	if got, created, err := s.CreateHumanCard(ctx, second, ask); err != nil || created || got.ID != "t-h" || got.IssueNumber != 31 {
		t.Fatalf("the card again = %+v, %v, %v; want the card that stands, on #31", got, created, err)
	}
	if evs, err := s.TaskEvents(ctx, "t-h", 10); err != nil || len(evs) != 1 || evs[0].TaskID != "t-h" || evs[0].IssueNumber != 31 || evs[0].Detail["ask"] != "Which?" {
		t.Fatalf("card events = %+v, %v; want its one ask on #31", evs, err)
	}

	plan := Task{ID: "t-p1", Kind: "human", Status: "waiting-human", Repository: "sf/r", IssueNumber: 41, BudgetEUR: 1, Objective: "y", Source: SourcePlan, Parent: caseTask, PlanKey: "wf"}
	if _, created, err := s.CreateHumanCard(ctx, plan, ask); err != nil || !created {
		t.Fatalf("plan card = %v, %v", created, err)
	}
	again := plan
	again.ID, again.IssueNumber = "t-p2", 42
	got, created, err := s.CreateHumanCard(ctx, again, ask)
	if err != nil || created || got.ID != "t-p1" {
		t.Fatalf("plan card again = %+v, %v, %v; want the first row and no new card", got, created, err)
	}
	if evs, err := s.TaskEvents(ctx, "t-p1", 10); err != nil || len(evs) != 1 || evs[0].IssueNumber != 41 {
		t.Fatalf("plan card events = %+v, %v; want its one ask on #41", evs, err)
	}
}

// Every way a card comes to ask leaves a case at the human: a human card
// filed with its ask, an agent's ask by its words, an ask that names its
// case at a rung, an ask by words that meets its live case at a rung, and
// a restart's ask again. An ask with no words is refused, so no event
// stands for it and no card is filed with it.
func TestEveryAskingCardHoldsACaseAtTheHuman(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	atHuman := func(name, taskID string) {
		t.Helper()
		cs, err := s.CasesOf(ctx, taskID)
		if err != nil || !slices.ContainsFunc(cs, func(c Case) bool { return c.Status == CaseAtHuman }) {
			t.Errorf("%s: cases of %s = %+v, %v; want one at the human", name, taskID, cs, err)
		}
	}
	card := func(id string, issue int64) {
		t.Helper()
		if err := s.CreateTask(ctx, Task{ID: id, Kind: "agent", Status: "running", Objective: "x", Repository: "sf/r", IssueNumber: issue, Track: "direct", BudgetEUR: 5}); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
	}
	ask := func(taskID string, detail map[string]any) error {
		_, err := s.Record(ctx, Event{Repository: "sf/r", TaskID: taskID, Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd, Detail: detail})
		return err
	}
	atRung := func(taskID, key string) Case {
		t.Helper()
		c, _, err := s.OpenCase(ctx, Case{TaskID: taskID, Repository: "sf/r", Kind: CaseAgentEscalate, StateKey: key, Problem: "Which store?", Status: CaseOpen})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	if _, created, err := s.CreateHumanCard(ctx, Task{ID: "t-human", Kind: "human", Repository: "sf/r", IssueNumber: 61, BudgetEUR: 1, Objective: "Renew the certificate"},
		HumanAsk{Q: IssueQuestion("Renew the certificate", "It expires Friday.", "alice", true), By: "factoryd", Asker: SourceWebhook, Via: ViaFactoryd}); err != nil || !created {
		t.Fatalf("CreateHumanCard = %v, %v", created, err)
	}
	atHuman("a human card", "t-human")

	card("t-words", 62)
	if err := ask("t-words", map[string]any{"ask": "Which store?", "question": "Which store?", "source": "agent", "reason": AgentAsks + "Which store?"}); err != nil {
		t.Fatal(err)
	}
	atHuman("an agent's ask by its words", "t-words")

	card("t-named", 63)
	named := atRung("t-named", "named")
	if err := ask("t-named", map[string]any{"ask": "Which store?", "case_id": named.ID, "source": "agent"}); err != nil {
		t.Fatal(err)
	}
	atHuman("an ask that names its case at a rung", "t-named")

	card("t-again", 64)
	words := map[string]any{"ask": "Which store?", "question": "Which store?", "source": "agent", "reason": AgentAsks + "Which store?"}
	probe, err := CaseFromAsk(Task{ID: "t-again", Kind: "agent", Repository: "sf/r"}, Event{Kind: EventQuestionAsked, Detail: words}, "")
	if err != nil {
		t.Fatal(err)
	}
	atRung("t-again", probe.StateKey)
	if err := ask("t-again", words); err != nil {
		t.Fatal(err)
	}
	atHuman("an ask by words that meets its live case at a rung", "t-again")

	card("t-healed", 65)
	atRung("t-healed", "healed")
	if err := ask("t-healed", map[string]any{"ask": "Which store?", "question": "Which store?", "source": "agent", "reason": HealedReason}); err != nil {
		t.Fatal(err)
	}
	atHuman("a restart's ask again", "t-healed")

	card("t-mute", 66)
	if err := ask("t-mute", map[string]any{"source": "agent"}); err == nil {
		t.Error("an ask with no words was recorded")
	}
	if evs, err := s.TaskEvents(ctx, "t-mute", 10); err != nil || len(evs) != 0 {
		t.Errorf("events of the refused ask = %+v, %v; want none", evs, err)
	}
	if _, _, err := s.CreateHumanCard(ctx, Task{ID: "t-mute-human", Kind: "human", Repository: "sf/r", IssueNumber: 67, BudgetEUR: 1, Objective: "x"},
		HumanAsk{Q: HumanQuestion{}, By: "factoryd", Asker: SourceWebhook, Via: ViaFactoryd}); err == nil {
		t.Error("a human card was filed with an ask of no words")
	}
	if _, ok, err := s.Task(ctx, "t-mute-human"); err != nil || ok {
		t.Errorf("the card of the refused ask stands: %v, %v", ok, err)
	}
}

// The criteria pass resolves a plan's criteria_rerun requests on every pass;
// with none pending it decides nothing, so the plan's criteria case, which
// its pending criteria request owns, stays at the human until that request
// is decided.
func TestAResolveWithNothingDecidedLeavesThePlansCriteriaCase(t *testing.T) {
	ctx := context.Background()
	s := caseEventStore(t)
	if _, err := s.PgDB().ExecContext(ctx, `UPDATE tasks SET status = 'done', track = 'plan', finished_at = now() WHERE id = $1`, caseTask); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(CriteriaSpec{Task: caseTask})
	id, err := s.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalCriteria, TaskID: caseTask, Project: "sf/r", Spec: spec,
		Reason: "criteria not met once every card of the plan ended: c1"})
	if err != nil {
		t.Fatalf("OpenApproval: %v", err)
	}
	recordCaseEvent(t, s, EventApprovalRequested, FactoryDecider, ViaFactoryd, map[string]any{"approval": id, "kind": ApprovalCriteria})
	c := onlyOpenCase(t, s)
	if c.Kind != CasePlanCriteria || c.Status != CaseAtHuman {
		t.Fatalf("case = %+v; want the plan's criteria case at the human", c)
	}

	if err := s.ResolveApprovals(ctx, caseTask, ApprovalCriteriaRerun, "approved", FactoryDecider, "every criterion has a verdict row"); err != nil {
		t.Fatalf("ResolveApprovals rerun: %v", err)
	}
	if got := caseByID(t, s, c.ID); got.Status != CaseAtHuman || got.Resolution != nil {
		t.Fatalf("case after a rerun resolve with none pending = %+v, resolution %+v; want it at the human", got, got.Resolution)
	}

	if err := s.ResolveApprovals(ctx, caseTask, ApprovalCriteria, "approved", "sebastian", "granted one card"); err != nil {
		t.Fatalf("ResolveApprovals criteria: %v", err)
	}
	want := &CaseResolution{By: "sebastian", Rung: 3, Action: ActAnswerCriteria, Reason: "granted one card"}
	if got := caseByID(t, s, c.ID); got.Status != CaseResolved || !reflect.DeepEqual(got.Resolution, want) {
		t.Fatalf("case after its request's decision = %+v, resolution %+v; want resolved %+v", got, got.Resolution, want)
	}
}
