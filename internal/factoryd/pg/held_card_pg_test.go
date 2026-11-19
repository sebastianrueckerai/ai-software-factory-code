package pg

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func cardOf(t *testing.T, s *PGStore, id string) Task {
	t.Helper()
	card, ok, err := s.Task(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("card %s: %v, found %v", id, err, ok)
	}
	return card
}

// The held production's ask at autonomous. By hand from newHeldRig: the
// keeper's pass files the hold's card in waiting-agent with no issue on the
// forge; its one ask row is held; its case is open at rung 0, due at that
// row, with no step, and nobody is poked. Rung 2's wait is refused while
// plan #1580 is open on main, naming the open plan, and its deploy_now runs
// and resolves the case on rung 2 with no issue filed.
func TestAHeldProductionAskIsDecidedOutOfSight(t *testing.T) {
	h := newHeldRig(t, policy.AutonomyAutonomous)
	ctx := context.Background()
	card := cardOf(t, h.s, h.cardID)
	asks := taskRows(t, h.s, h.cardID, EventQuestionAsked)
	c := caseByID(t, h.s, h.caseID)
	if card.Status != "waiting-agent" || card.IssueNumber != 0 || len(h.forge.issues) != 0 {
		t.Fatalf("card %s #%d, issues %q; want waiting-agent with no issue filed", card.Status, card.IssueNumber, h.forge.issues)
	}
	if len(asks) != 1 || asks[0].Detail["held"] != true {
		t.Fatalf("asks = %+v, want the keeper's one held row", asks)
	}
	if c.Status != CaseOpen || c.OwnerRung != 0 || c.NextWakeAt == nil || !c.NextWakeAt.Equal(asks[0].At) || len(h.steps(t, c.ID)) != 0 {
		t.Fatalf("case = %+v with steps %+v; want it open at rung 0, due at the ask, with no step", c, h.steps(t, c.ID))
	}
	if pokes := h.pokeRowsOf(t, h.cardID); len(pokes) != 0 {
		t.Fatalf("pokes = %v, want none before the face", pokes)
	}
	_, err := h.r.Act(ctx, ActInput{TaskID: h.cardID, CaseID: h.caseID, By: ChatUser, Via: ViaChat, Rung: 2, Res: Resolution{Action: AnswerHoldWait, Note: "Wait"}})
	want := "card " + h.cardID + "'s condition stands: a plan is open on main of demo-app/demo-app: hand it up with attach_analysis and hold_wait as the recommended action"
	if err == nil || err.Error() != want {
		t.Fatalf("rung 2's wait = %v,\nwant %q", err, want)
	}
	if _, err := h.deployAct(2, ChatUser); err != nil {
		t.Fatalf("rung 2's deploy_now: %v", err)
	}
	c = caseByID(t, h.s, h.caseID)
	if c.Status != CaseResolved || c.Resolution == nil || c.Resolution.Rung != 2 || c.Resolution.By != ChatUser || len(h.forge.issues) != 0 {
		t.Fatalf("case %s, resolution %+v, issues %q; want it resolved on rung 2 by %s with no issue", c.Status, c.Resolution, h.forge.issues, ChatUser)
	}
}

// The real Deploy now on the held card: main's newest push run is green at
// 9d9d9d9, so production goes ahead, and the keeper's close of the ask, on
// a card with no issue, is its record alone. The next heal pass ends the
// card done, with no issue filed or closed.
func TestADeployNowEndsTheHeldCardWithNoIssue(t *testing.T) {
	h := newHeldRig(t, policy.AutonomyAutonomous)
	ctx := context.Background()
	const commit = "9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d"
	h.forge.runs = []map[string]any{{"id": 77, "index_in_repo": 77, "commit_sha": commit, "prettyref": "main", "event": "push", "status": "success"}}
	h.r.DeployNow = h.keeper.DeployNow
	if _, err := h.deployAct(2, ChatUser); err != nil {
		t.Fatalf("rung 2's deploy_now: %v", err)
	}
	if card := cardOf(t, h.s, h.cardID); card.Status != "waiting-agent" {
		t.Fatalf("card %s right after Deploy now, want it waiting-agent until the heal pass reads its close", card.Status)
	}
	in := &Intake{Store: h.s, Forgejo: h.r.PgForgejo(), Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Timeline: h.r.PgTimeline()}
	if err := in.HealFactoryCloses(ctx); err != nil {
		t.Fatalf("HealFactoryCloses: %v", err)
	}
	card := cardOf(t, h.s, h.cardID)
	left, err := h.s.UnconfirmedFactoryCloses(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if card.Status != "done" || card.IssueNumber != 0 || len(left) != 0 || len(h.forge.issues) != 0 || len(h.forge.closed) != 0 {
		t.Fatalf("card %s #%d, unconfirmed closes %+v, issues %q, closed %q; want it done with no issue and its close confirmed",
			card.Status, card.IssueNumber, left, h.forge.issues, h.forge.closed)
	}
}

// stillRedRig is the rung 2 rig at autonomous with main of demo-app still
// red at abcdef1 after the revert #12 merged: factoryd's card for it, filed
// as the roller files it, with the merge queue paused on it.
func stillRedRig(t *testing.T) (rungRig, *chainForgejo, Task, Case) {
	t.Helper()
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	g.atAutonomy(policy.AutonomyAutonomous)
	forge := &chainForgejo{}
	f := forge.srv(t)
	g.r.PgSetForgejo(f)
	ctx := context.Background()
	card, err := factoryd.EscalateToHuman(ctx, g.s, f, Repo{Owner: "demo-app", Name: "demo-app"}, HumanQuestion{
		Ask:     "main is still red at abcdef1 after the revert #12 merged: a person decides.",
		Why:     "Main CI's push run 5 of abcdef1234 failed after the revert #12 merged.",
		Info:    "The run: https://forgejo.example/demo-app/demo-app/actions/runs/5",
		Choices: []QuestionChoice{{Value: AnswerClose, Label: "Fixed, close"}},
	}, "factoryd", Task{SHA: "abcdef1234", Source: SourceStillRed})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.PauseQueue(ctx, factoryd.QueuePause{Project: "demo-app/demo-app", RunID: 5, SHA: "abcdef1234", Since: time.Now(), FixTask: card.ID}); err != nil {
		t.Fatal(err)
	}
	c := onlyLiveCase(t, g.s, card.ID)
	if card.Status != "waiting-agent" || card.IssueNumber != 0 || len(forge.issues) != 0 || c.Kind != CaseRedMain || c.Status != CaseOpen {
		t.Fatalf("card %s #%d, issues %q, case %s %s; want waiting-agent with no issue and a red_main case open", card.Status, card.IssueNumber, forge.issues, c.Kind, c.Status)
	}
	return g, forge, card, c
}

// Main still red climbs to its face. The case-wake pass hands it from rung
// 0 to rung 2. Rung 2's close is refused while the merge queue stays
// paused on the card. Its hand-up files one issue, whose body carries what
// the factory agent found, moves the card to waiting-human on that issue,
// #31, with no comment, and the face's row, unheld and of the card's
// source, pokes once.
func TestMainStillRedClimbsToItsFace(t *testing.T) {
	g, forge, card, c := stillRedRig(t)
	ctx := context.Background()
	g.wakePass(t)
	if steps := g.steps(t, c.ID); len(steps) != 2 || steps[0].Rung != 0 || steps[1].Rung != 2 || steps[1].Outcome != StepInProgress {
		t.Fatalf("steps = %+v, want rung 0's hand-up and rung 2 in progress", steps)
	}
	_, err := g.r.Act(ctx, ActInput{TaskID: card.ID, CaseID: c.ID, By: ChatUser, Via: ViaChat, Rung: 2, Res: Resolution{Action: ActClose, Note: "Fixed, close"}})
	want := "card " + card.ID + "'s condition stands: main of demo-app/demo-app is red and its merge queue paused on it: hand it up with attach_analysis and close as the recommended action"
	if err == nil || err.Error() != want {
		t.Fatalf("rung 2's close = %v,\nwant %q", err, want)
	}
	if _, err := g.r.HandUpRung2(ctx, card.ID, c.ID, "the revert merged and main's run 5 is red at abcdef1", RungTwoRecommendation{Action: ActClose, Grounds: "fix main, then close"}, nil); err != nil {
		t.Fatalf("HandUpRung2: %v", err)
	}
	faced := cardOf(t, g.s, card.ID)
	if faced.Status != "waiting-human" || faced.IssueNumber != 31 || len(forge.issues) != 1 || len(forge.comments) != 0 {
		t.Fatalf("card %s #%d, issues %q, comments %q; want waiting-human on the one issue filed, #31, with no comment", faced.Status, faced.IssueNumber, forge.issues, forge.comments)
	}
	if !strings.HasPrefix(forge.issues[0], "main is still red at abcdef1 after the revert #12 merged: a person decides.\n\n") ||
		!strings.Contains(forge.issues[0], "The factory agent found: the revert merged and main's run 5 is red at abcdef1") ||
		!strings.Contains(forge.issues[0], "Recommendation: close: fix main, then close") {
		t.Fatalf("the issue = %q, want the ask as its title and the factory agent's finding and recommendation in its body", forge.issues[0])
	}
	if got := caseByID(t, g.s, c.ID); got.Status != CaseAtHuman {
		t.Fatalf("case %s, want it at the human", got.Status)
	}
	asks := taskRows(t, g.s, card.ID, EventQuestionAsked)
	if len(asks) != 2 || asks[1].Detail["held"] != nil || asks[1].Detail["source"] != SourceStillRed || asks[1].IssueNumber != 31 {
		t.Fatalf("asks = %+v, want the held row and the face's, unheld, of source %s on #31", asks, SourceStillRed)
	}
	if pokes := g.pokeRowsOf(t, card.ID); len(pokes) != 1 || pokes[0] != EventQuestionAsked {
		t.Fatalf("pokes = %v, want the face's one", pokes)
	}
}

// With main green again, the pause cleared, rung 2's close passes and
// resolves the case on rung 2; the console's close of the card, which has no
// issue, ends it done at once. No issue is ever filed.
func TestMainGreenLetsRung2CloseTheHeldCard(t *testing.T) {
	g, forge, card, c := stillRedRig(t)
	ctx := context.Background()
	g.wakePass(t)
	if err := g.s.ClearQueuePause(ctx, "demo-app/demo-app"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: card.ID, CaseID: c.ID, By: ChatUser, Via: ViaChat, Rung: 2, Res: Resolution{Action: ActClose, Note: "Fixed, close"}}); err != nil {
		t.Fatalf("rung 2's close: %v", err)
	}
	if got := caseByID(t, g.s, c.ID); got.Status != CaseResolved || got.Resolution == nil || got.Resolution.Rung != 2 || got.Resolution.Action != ActClose {
		t.Fatalf("case %s, resolution %+v; want it resolved on rung 2 with close", got.Status, got.Resolution)
	}
	in := &Intake{Store: g.s, Forgejo: g.r.PgForgejo(), Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Timeline: g.r.PgTimeline()}
	if err := in.CloseCard(ctx, card.ID, ChatUser); err != nil {
		t.Fatalf("CloseCard: %v", err)
	}
	if ended := cardOf(t, g.s, card.ID); ended.Status != "done" || ended.IssueNumber != 0 || len(forge.issues) != 0 {
		t.Fatalf("card %s #%d, issues %q; want it done with no issue ever filed", ended.Status, ended.IssueNumber, forge.issues)
	}
}

// The chat's escalate asks at once: its card waits on a human on its issue,
// #31, and its case is at the human with the agent's filing as rung 2's
// step, handed up with the recommendation as its grounds and the why as
// what it found, then the human's step. The Waker wakes nobody on it.
func TestTheChatsEscalationIsItsRungTwoStep(t *testing.T) {
	g, w, starter := rungTwoRig(t, policy.JudgmentAlways)
	forge := &chainForgejo{}
	ctx := context.Background()
	card, err := factoryd.EscalateToHuman(ctx, g.s, forge.srv(t), Repo{Owner: "demo-app", Name: "demo-app"}, HumanQuestion{
		Ask: "Nothing on the roadmap is ready. Write item 12's criteria?", Why: "the build cannot start without them", Info: "item 12 stops at its goal",
		Recommendation: "Write them from the roadmap's section", Source: "the project agent",
		Choices: []QuestionChoice{{Value: "say:Write them", Label: "Write them"}, {Value: "say:Park", Label: "Park"}},
	}, "sebastian", Task{Source: SourceChat})
	if err != nil {
		t.Fatal(err)
	}
	c := onlyLiveCase(t, g.s, card.ID)
	steps := g.steps(t, c.ID)
	if card.Status != "waiting-human" || card.IssueNumber != 31 || c.Status != CaseAtHuman || len(steps) != 2 {
		t.Fatalf("card %s #%d, case %s, steps %+v; want it on #31 at the human with two steps", card.Status, card.IssueNumber, c.Status, steps)
	}
	if s := steps[0]; s.Rung != 2 || s.Actor != ChatUser || s.Outcome != StepHandedUp || s.Grounds != "Write them from the roadmap's section" || s.Found != "the build cannot start without them" {
		t.Fatalf("the first step = %+v, want rung 2's filing by %s, handed up", s, ChatUser)
	}
	if s := steps[1]; s.Rung != 3 || s.Actor != FactoryDecider {
		t.Fatalf("the second step = %+v, want the human's", s)
	}
	if err := drainPass(t, w); err != nil {
		t.Fatal(err)
	}
	if calls := starter.recorded(); len(calls) != 0 {
		t.Fatalf("wakes = %+v, want none", calls)
	}
}

// heldRung2Rig is the rung 2 rig at state with factoryd's held card of
// source on demo-app asking q, and the card's case after the case-wake
// pass: rung 2 at work on it.
func heldRung2Rig(t *testing.T, state, id, source string, q HumanQuestion) (rungRig, *chainForgejo, Task, Case) {
	t.Helper()
	g, _, _ := rungTwoRig(t, policy.JudgmentAlways)
	g.atAutonomy(state)
	forge := &chainForgejo{}
	f := forge.srv(t)
	g.r.PgSetForgejo(f)
	card, err := factoryd.EscalateToHuman(context.Background(), g.s, f, Repo{Owner: "demo-app", Name: "demo-app"}, q, "factoryd", Task{ID: id, SHA: "abcdef1234", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	c := onlyLiveCase(t, g.s, card.ID)
	g.wakePass(t)
	if card.Status != "waiting-agent" || card.IssueNumber != 0 || len(g.stepsOfRung(t, c.ID, 2)) != 1 {
		t.Fatalf("card %s #%d, rung 2 steps %+v; want it held with no issue and rung 2 at work", card.Status, card.IssueNumber, g.stepsOfRung(t, c.ID, 2))
	}
	return g, forge, card, c
}

// A routine's card, which its filer routed to a human, is handed to an
// agent by a human alone: at full auto rung 2's go is refused, naming the
// card and why, and rung 2 hands the case up with go as its
// recommendation. The face files the card's issue, #31, and the human's go
// at rung 3 resolves the case.
func TestOnlyAHumanHandsAHeldRoutineCardToAnAgent(t *testing.T) {
	g, _, card, c := heldRung2Rig(t, policy.AutonomyFullAuto, "t-bump", SourceRoutine, HumanQuestion{
		Ask: "Weekly bump of the pinned outside inputs", Why: "the routine weekly-bump fired and files to a person", Info: "Bump each pinned input.",
		Choices: []QuestionChoice{{Value: AnswerGo, Label: "Have the factory do it"}, {Value: AnswerClose, Label: "Done"}},
	})
	ctx := context.Background()
	_, err := g.r.Act(ctx, ActInput{TaskID: card.ID, CaseID: c.ID, By: ChatUser, Via: ViaChat, Rung: 2, Res: Resolution{Action: AnswerGo, Note: "Have the factory do it"}})
	want := "card t-bump's filer routed it to a human, so only a human hands it to an agent: hand it up with attach_analysis and go as the recommended action"
	if err == nil || err.Error() != want {
		t.Fatalf("rung 2's go = %v,\nwant %q", err, want)
	}
	if _, err := g.r.HandUpRung2(ctx, card.ID, c.ID, "the bump is work an agent does", RungTwoRecommendation{Action: AnswerGo, Grounds: "an agent bumps the pins"}, nil); err != nil {
		t.Fatalf("HandUpRung2: %v", err)
	}
	if faced := cardOf(t, g.s, card.ID); faced.Status != "waiting-human" || faced.IssueNumber != 31 {
		t.Fatalf("card %s #%d, want waiting-human on #31", faced.Status, faced.IssueNumber)
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: card.ID, CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: AnswerGo, Note: "Have the factory do it"}}); err != nil {
		t.Fatalf("the human's go: %v", err)
	}
	if got := caseByID(t, g.s, c.ID); got.Status != CaseResolved || got.Resolution == nil || got.Resolution.Rung != 3 || got.Resolution.Action != AnswerGo || got.Resolution.By != "sebastian" {
		t.Fatalf("case %s, resolution %+v; want it resolved by sebastian's go on rung 3", got.Status, got.Resolution)
	}
}

// Rung 2's rerun_again on a held stall card keeps the card waiting for the
// re-run's result: rung 2's step stays at work until its bound, 35
// minutes, and then the case faces the human. The face says that rung 2
// took rerun_again and the card still waits after 35m, and nowhere that
// rung 2 did not answer.
func TestAFaceAfterRung2sRerunSaysTheCardStillWaits(t *testing.T) {
	g, forge, card, c := heldRung2Rig(t, policy.AutonomyAutonomous, "t-stall", factoryd.SourceChainStall, HumanQuestion{
		Ask:  "The chain of demo-app/demo-app has been pending at abcdef1 since 2026-11-18 10:00 UTC. Re-run it, check again or dismiss?",
		Why:  "Pending past the stall bound: run 5 pending.",
		Info: "No merged card waits on the chain.",
		Choices: []QuestionChoice{{Value: factoryd.AnswerRerunAgain, Label: "Re-run again"}, {Value: AnswerCheckAgain, Label: "Check again"},
			{Value: AnswerDismiss, Label: "Dismiss"}},
	})
	ctx := context.Background()
	if _, err := g.r.Act(ctx, ActInput{TaskID: card.ID, CaseID: c.ID, By: ChatUser, Via: ViaChat, Rung: 2, Res: Resolution{Action: factoryd.AnswerRerunAgain, Note: "Re-run again"}}); err != nil {
		t.Fatalf("rung 2's rerun_again: %v", err)
	}
	own := g.stepsOfRung(t, c.ID, 2)
	if len(own) != 2 || own[0].Outcome != StepInProgress || own[0].DueAt == nil || own[1].Outcome != StepResolved || own[1].Decision != factoryd.AnswerRerunAgain {
		t.Fatalf("rung 2 steps = %+v, want its step at work and its resolved rerun_again", own)
	}
	g.r.PgSetNow(func() time.Time { return own[0].DueAt.Add(time.Minute) })
	expired, err := g.s.ExpireSteps(ctx, g.r.Clock())
	if err != nil {
		t.Fatal(err)
	}
	const still = "rung 2 took rerun_again and the card still waits after 35m"
	if len(expired) != 1 || expired[0].Found != still {
		t.Fatalf("expired = %+v, want rung 2's step with %q", expired, still)
	}
	if err := g.r.ClimbExpired(ctx, expired); err != nil {
		t.Fatal(err)
	}
	g.settle()
	if faced := cardOf(t, g.s, card.ID); faced.Status != "waiting-human" || faced.IssueNumber != 31 || len(forge.issues) != 1 {
		t.Fatalf("card %s #%d, issues %d; want it faced on #31", faced.Status, faced.IssueNumber, len(forge.issues))
	}
	asks := taskRows(t, g.s, card.ID, EventQuestionAsked)
	info, _ := asks[len(asks)-1].Detail["info"].(string)
	if !strings.Contains(forge.issues[0], still) || !strings.Contains(info, still) || strings.Contains(forge.issues[0], "did not answer") || strings.Contains(info, "did not answer") {
		t.Fatalf("the issue %q, the face's info %q; want %q and no \"did not answer\"", forge.issues[0], info, still)
	}
}

// A held card a closer ended leaves nothing open at the next heal pass. The
// keeper's pass closes the hold's ask once plan #1580 is no longer open on
// main; the card has no issue, so the close is factoryd's record alone. The
// heal pass's close heal ends the card done at once, with no wait for a
// webhook, and its case reconcile withdraws the case it left open.
func TestAHeldCardAClosersEndedLeavesNoOpenCase(t *testing.T) {
	h := newHeldRig(t, policy.AutonomyAutonomous)
	ctx := context.Background()
	if _, err := h.s.Record(ctx, Event{Repository: heldRepo, TaskID: "p1580", Kind: EventPlanClosed, Actor: FactoryDecider, Via: ViaFactoryd}); err != nil {
		t.Fatal(err)
	}
	if err := h.keeper.Pass(ctx); err != nil {
		t.Fatalf("the keeper's pass: %v", err)
	}
	if card := cardOf(t, h.s, h.cardID); card.Status != "waiting-agent" || len(h.forge.issues) != 0 {
		t.Fatalf("card %s, issues %q; want it held until the heal pass reads its close", card.Status, h.forge.issues)
	}
	in := &Intake{Store: h.s, Forgejo: h.r.PgForgejo(), Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Timeline: h.r.PgTimeline()}
	if err := in.HealFactoryCloses(ctx); err != nil {
		t.Fatalf("HealFactoryCloses: %v", err)
	}
	if _, err := h.s.ReconcileCases(ctx, heldRepo); err != nil {
		t.Fatalf("ReconcileCases: %v", err)
	}
	open, err := h.s.OpenCasesOf(ctx, h.cardID)
	if card := cardOf(t, h.s, h.cardID); err != nil || card.Status != "done" || len(open) != 0 || len(h.forge.issues) != 0 {
		t.Fatalf("card %s, open cases %+v (%v), issues %q; want it done with no open case and no issue", card.Status, open, err, h.forge.issues)
	}
}
