package factoryd

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// The heal opens a case from an ask with no offers; the offers pass stores the
// table's after the ask's own choices, once.
func TestTheOffersPassStoresTheOffersOfAHealOpenedCase(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET status = 'waiting-human' WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	q := HumanQuestion{Ask: "Which file?", Why: "two match", Info: "a.txt is older", Choices: []QuestionChoice{{Value: "say:a.txt", Label: "a.txt"}}}
	escalate(t, g.s, IntakeEvent{TaskID: "t1", Kind: IntakeEscalation, Author: "factoryd", Body: AgentAsks + "Which file?", CommentID: 701, Question: &q})
	if _, err := g.s.db.ExecContext(ctx, `INSERT INTO events (repository, task_id, kind, actor, via, detail) VALUES ('demo-app/demo-app', 't1', $1, $2, $3, $4::jsonb)`,
		EventQuestionAsked, FactoryDecider, ViaFactoryd, `{"question": "Which file?", "ask": "Which file?", "info": "a.txt is older", "comment": 701, "source": "agent",
		"offers": [{"value": "say:a.txt", "label": "a.txt"}]}`); err != nil {
		t.Fatal(err)
	}
	if rep, err := g.s.ReconcileCases(ctx, "demo-app/demo-app"); err != nil || rep.Opened != 1 {
		t.Fatalf("heal = %+v, %v; want one case opened", rep, err)
	}
	cases, _ := g.s.OpenCasesOf(ctx, "t1")
	if len(cases) != 1 {
		t.Fatalf("cases = %+v, want one", cases)
	}
	ask, ok := g.r.caseAsk(ctx, cases[0])
	if !ok || len(storedOffers(t, ask.Detail)) != 0 {
		t.Fatalf("the heal-opened case's ask = %+v, %v; want it found, with no table offers", ask, ok)
	}
	g.r.StoreMissedOffers(ctx)
	ask, _ = g.r.caseAsk(ctx, cases[0])
	table := offerValues(storedOffers(t, ask.Detail))
	if !slices.Contains(table, ActClose) || !slices.Contains(table, ActNote) || len(askOffers(ask.Detail)) != 1 {
		t.Fatalf("after the pass the ask holds table offers %v and choices %v; want close and note after the one own choice", table, askOffers(ask.Detail))
	}
	if err := g.s.SetCaseOffers(ctx, cases[0].ID, []AskOffer{{Value: ActNote, Label: "Note", Effect: "marker"}}, nil); err != nil {
		t.Fatal(err)
	}
	g.r.StoreMissedOffers(ctx)
	ask, _ = g.r.caseAsk(ctx, cases[0])
	if got := offerValues(storedOffers(t, ask.Detail)); !slices.Equal(got, []string{ActNote}) {
		t.Fatalf("the pass stored again over offers it had stored: %v", got)
	}
}

// restart_plan's work closes the plan with the real store write: the request
// is decided there and Act resolves the case, once, as restart_plan.
func TestRestartPlanOnACriteriaCaseIsResolvedByAct(t *testing.T) {
	g, stub, c := criteriaRig(t)
	ctx := context.Background()
	if _, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3,
		Res: Resolution{Action: ActRestartPlan, Note: "split it in two"}}); err != nil {
		t.Fatalf("restart_plan on the criteria case = %v", err)
	}
	got := caseByID(t, g.s, c.ID)
	if got.Status != CaseResolved || got.Resolution == nil || got.Resolution.Action != ActRestartPlan || got.Resolution.By != "sebastian" {
		t.Fatalf("case = %s %+v, want resolved by sebastian as restart_plan", got.Status, got.Resolution)
	}
	var status string
	if err := g.s.db.QueryRowContext(ctx, `SELECT status FROM approval_requests WHERE task_id = 't1' AND kind = 'criteria'`).Scan(&status); err != nil || status != "approved" {
		t.Fatalf("the request is %q, %v; want approved by the close", status, err)
	}
}

// The plan card's own close, outside Act, still ends the request's case.
func TestAPlanCloseOutsideActEndsTheRequestsCase(t *testing.T) {
	g, _, c := criteriaRig(t)
	if err := g.s.ClosePlanWithWaivers(context.Background(), "t1", "sebastian", func(context.Context) (PlanClose, error) {
		return PlanClose{Home: "t1", Words: "Closed by sebastian"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := caseByID(t, g.s, c.ID); got.Status != CaseResolved {
		t.Fatalf("case = %s, want it resolved by the close", got.Status)
	}
}

// confirm_criteria resolves its case in Act and leaves the request pending
// until the criteria pass; the heal pass opens no case for that request again.
func TestAConfirmedCriteriaRequestGetsNoCaseFromTheHeal(t *testing.T) {
	g, stub, c := criteriaRig(t)
	ctx := context.Background()
	if _, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3,
		Res: Resolution{Action: ActConfirmCriteria, Note: "the cards deliver it"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.db.ExecContext(ctx, `UPDATE approval_requests SET created_at = now() - interval '5 minutes'`); err != nil {
		t.Fatal(err)
	}
	rep, err := g.s.ReconcileCases(ctx, "demo-app/demo-app")
	open, _ := g.s.OpenCasesOf(ctx, "t1")
	if err != nil || rep.Opened != 0 || slices.ContainsFunc(open, func(k Case) bool { return k.Kind == CasePlanCriteria }) {
		t.Fatalf("heal after the confirm = %+v, %v, live cases %+v; want none opened", rep, err, open)
	}
}

// A month cap's human card keeps its kind, and its question's choices answer
// it through the real Act: "Decided, close" closes the card, "Let it stand"
// leaves its case at the human.
func TestAMonthCapHumanCardIsAnsweredByItsChoices(t *testing.T) {
	for _, tc := range []struct {
		value  string
		status string
	}{{AnswerClose, CaseResolved}, {AnswerPark, CaseAtHuman}} {
		t.Run(tc.value, func(t *testing.T) {
			g := newTripRig(t, "success")
			ctx := context.Background()
			if err := g.s.CreateTask(ctx, Task{ID: "h1", Kind: "human", Status: "waiting-human", Objective: "x", Repository: "demo-app/demo-app", IssueNumber: 51, Source: SourceMonthCap}); err != nil {
				t.Fatal(err)
			}
			h, _, _ := g.r.task(ctx, "h1")
			if err := recordAsk(ctx, g.s, g.r.timeline, h, monthCapCardQuestion(100, "2026-11", 100, "", ""), "factoryd", SourceMonthCap, ViaFactoryd, "", nil); err != nil {
				t.Fatal(err)
			}
			cases, _ := g.s.OpenCasesOf(ctx, "h1")
			if len(cases) != 1 || cases[0].Kind != CaseMonthCap || !ChoiceCase(cases[0], h.Kind) || AnswersByOffers(cases[0], h.Kind) {
				t.Fatalf("cases %+v; want the month cap kept, answered by its choices and with no table offers", cases)
			}
			if _, err := g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: cases[0].ID, By: "sebastian", Via: ViaButton, Rung: 3, Res: Resolution{Action: tc.value}}); err != nil {
				t.Fatalf("the choice %s = %v", tc.value, err)
			}
			if got := caseByID(t, g.s, cases[0].ID); got.Status != tc.status {
				t.Fatalf("after %s the case is %s, want %s", tc.value, got.Status, tc.status)
			}
		})
	}
}

// The offers an ask stores are the live facts', not the case's stored ones:
// a case whose own facts have gone stale is faced anew on what is true now.
func TestAnAskStoresTheOffersOfTheLiveFacts(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	if _, err := g.r.Escalate(ctx, "t1", capReached+"\nlast: no session produced a result", nil); err != nil {
		t.Fatal(err)
	}
	cases, _ := g.s.OpenCasesOf(ctx, "t1")
	if len(cases) != 1 {
		t.Fatalf("cases = %+v, want one", cases)
	}
	stale := cases[0]
	stale.Facts = nil
	task, _, _ := g.r.task(ctx, "t1")
	live, err := g.r.factsAtCase(ctx, task, cases[0])
	if err != nil {
		t.Fatal(err)
	}
	liveOffers, _ := OffersFor(live)
	staleOffers, _ := OffersFor(FactsOf(stale))
	if slices.Equal(offerValues(liveOffers), offerValues(staleOffers)) {
		t.Fatalf("the live facts and the case's own offer the same %v: the test cannot tell them apart", offerValues(liveOffers))
	}
	if _, err := g.r.askHuman(ctx, task, stale, capReached, "next", nil, nil); err != nil {
		t.Fatal(err)
	}
	asks := g.asks(t)
	if got := offerValues(storedOffers(t, asks[len(asks)-1].Detail)); !slices.Equal(got, offerValues(liveOffers)) {
		t.Fatalf("stored offers %v, want the live facts' %v (the stale case's own would be %v)", got, offerValues(liveOffers), offerValues(staleOffers))
	}
}

// An error from the answer's own work refreshes the stored offers as a
// table refusal does: the face then shows what fits the state.
func TestAFailedWorkRefreshesTheStoredOffers(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	if _, err := g.r.Escalate(ctx, "t1", capReached+"\nlast: no session produced a result", nil); err != nil {
		t.Fatal(err)
	}
	cases, _ := g.s.OpenCasesOf(ctx, "t1")
	if err := g.s.SetCaseOffers(ctx, cases[0].ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	_, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: cases[0].ID, By: "sebastian", Via: ViaConsole, Rung: 3,
		Res:  Resolution{Action: ActClose, Note: "done"},
		Work: func(context.Context) (int64, error) { return 0, errors.New("the forge refused") }})
	if err == nil {
		t.Fatal("Act took an answer whose work failed")
	}
	if got := storedOffers(t, g.asks(t)[0].Detail); len(got) == 0 {
		t.Fatal("the offers were not stored anew after the work's failure")
	}
}
