package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"go.temporal.io/sdk/temporal"
)

// A card t1 of the facts rig: an approved green head h2 under a fund that
// bought nothing, the money short of an opening and no retryable
// dependency (TestTripFactsReadsTheCard). With no case, rung 1's close is
// refused with the one action rung 1 may take on a card that waits on no
// case, a replan, and a successor is refused for want of a case. On its
// case 7, rung 1's close is refused with what rung 1 may take: route to
// approval, re-verify, a successor, a replan, a withdrawal and a note. An
// action the table does not hold is refused with the human's: re-verify,
// a raise of the ceiling, a successor, a replan, a close and a note; no
// confirm_audit, as the rig's audit finding is at no file. With case 7 live, an
// answer with no case is refused naming it. Nothing is written.
func TestActRefusesAnActionTheTableDoesNotOffer(t *testing.T) {
	g := newFactsRig(t)
	g.card()
	g.timeline(t)
	events := len(g.store.events)
	ctx := context.Background()
	for _, c := range []struct {
		name string
		in   ActInput
		want string
	}{
		{"rung 1's close with no case", ActInput{By: "factory-checkin", Rung: 1, Res: Resolution{Action: ActClose, Note: "done"}},
			"the check-in may not pick `close`: the factory agent and the human may, and the check-in may take `replan`"},
		{"a successor with no case", ActInput{By: "sebastian", Rung: 3, Res: Resolution{Action: ActSuccessor}},
			"no ask is open on card #12, so `successor` has nothing to answer, and the human may take `raise_ceiling`, `replan` and `close`"},
	} {
		c.in.TaskID = "t1"
		var refused ErrActRefused
		if _, err := g.r.Act(ctx, c.in); !errors.As(err, &refused) || err.Error() != c.want {
			t.Errorf("%s = %v, want the refusal %q", c.name, err, c.want)
		}
	}
	g.store.cases = append(g.store.cases, Case{ID: 7, TaskID: "t1", Kind: CaseSessionCap, Status: CaseAtHuman})
	for _, c := range []struct {
		name string
		in   ActInput
		want string
	}{
		{"rung 1's close", ActInput{CaseID: 7, By: "factory-checkin", Rung: 1, Res: Resolution{Action: ActClose, Note: "done"}},
			"the check-in may not pick `close`: the factory agent and the human may, and the check-in may take `route_approval`, `reverify`, `successor` and `replan`"},
		{"the human's approve", ActInput{CaseID: 7, By: "sebastian", Rung: 3, Res: Resolution{Action: "approve"}},
			"there is no action `approve`, and the human may take `reverify`, `raise_ceiling`, `successor`, `replan`, `close` and `note`"},
		{"a fund with no case while case 7 waits", ActInput{By: "sebastian", Rung: 3, Res: Resolution{Action: ActFund, Args: map[string]any{"eur": 0.5}}},
			"Card #12 waits on the session cap: answer that ask, not the card."},
		{"a fund with no case via the chat while case 7 waits", ActInput{By: "sebastian", Via: ViaChat, Rung: 3, Res: Resolution{Action: ActFund, Args: map[string]any{"eur": 0.5}}},
			"Card #12 waits on the session cap (`case_id 7`): answer that ask, not the card."},
	} {
		c.in.TaskID = "t1"
		var refused ErrActRefused
		if _, err := g.r.Act(ctx, c.in); !errors.As(err, &refused) || err.Error() != c.want {
			t.Errorf("%s = %v, want the refusal %q", c.name, err, c.want)
		}
	}
	if len(g.store.events) != events || g.store.tasks["t1"].BudgetEUR != 5 || len(g.store.resolved) != 0 || g.store.cases[len(g.store.cases)-1].Status != CaseAtHuman {
		t.Fatalf("a refused act wrote %d events, left the budget at %v, resolved %v; want none, 5 and none",
			len(g.store.events)-events, g.store.tasks["t1"].BudgetEUR, g.store.resolved)
	}
}

// With no case, a fund of 1 EUR on a running card raises its budget from
// 3.50 to 4.50 EUR under the ceiling of 5 and writes the card_funded row as
// a pool draw of rung 3 with the head, checks and findings it was weighed
// on. No case is resolved, and no answer row, comment or signal follows.
func TestActWithNoCaseFundsTheCard(t *testing.T) {
	g := newFactsRig(t)
	card := g.store.tasks["t1"]
	card.Status, card.BudgetEUR = "running", 3.5
	g.store.tasks["t1"] = card
	got, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", By: "sebastian", Via: ViaConsole, Rung: 3,
		Res: Resolution{Action: ActFund, Args: map[string]any{"eur": 1.0}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Case.ID != 0 || got.Drawn != (policy.Draw{EUR: 1}) || got.Comment != 0 || got.Text != "Card #12 took “Add money”: 1.00 EUR from the pool." {
		t.Fatalf("result = %+v, want no case, 1 EUR drawn, no comment", got)
	}
	if b := g.store.tasks["t1"].BudgetEUR; b != 4.5 {
		t.Fatalf("budget = %v, want 4.5", b)
	}
	if len(g.store.events) != 1 || len(g.store.resolved) != 0 {
		t.Fatalf("events %+v, cases resolved %v; want the one card_funded row and none", g.store.events, g.store.resolved)
	}
	e := g.store.events[0]
	d := e.Detail
	if e.Kind != EventCardFunded || e.Actor != "sebastian" || e.Via != ViaConsole || e.Repository != "demo-app/demo-app" ||
		d["pool"] != true || d["rung"] != 3 || d["case_id"] != int64(0) || d["head"] != "" || d["checks"] != "" ||
		d["amount_eur"] != 1.0 || d["from_eur"] != 3.5 || d["to_eur"] != 4.5 {
		t.Fatalf("the row = %+v, want card_funded by sebastian via console, pool true, rung 3, case 0, 1 EUR from 3.5 to 4.5", e)
	}
	if ids, ok := d["findings"].([]string); !ok || len(ids) != 0 {
		t.Fatalf("findings = %#v, want the empty list the read timeline gave", d["findings"])
	}
	if s := g.r.cfg.Workflows.(*fakeWorkflows).signals; len(s) != 0 {
		t.Fatalf("signals = %v, want none", s)
	}
}

// The retry rig's card t-b (#6) waits on its failed dependency t-a (#5).
// alice's retry on t-b's case 3 files #9 in place of #5 before the case
// resolves with it, and the step says so.
func TestActRetriesTheFailedDependency(t *testing.T) {
	r, store, _, _, wf := newRetryRig(t)
	store.cases = []Case{{ID: 3, TaskID: "t-b", Kind: CaseDependencyFailed, Status: CaseAtHuman}}
	got, err := r.Act(context.Background(), ActInput{TaskID: "t-b", CaseID: 3, By: "alice", Rung: 3,
		Res: Resolution{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []CaseAction{{Action: ActRetry, Args: map[string]any{"task_id": "t-a", "replacement": int64(9)}, Result: "Filed #9 in place of #5."}}
	if !reflect.DeepEqual(got.Did, want) || got.Case.Status != CaseResolved || got.Case.Resolution.Action != ActRetry ||
		got.Text != "The failed dependency on #6 is resolved with “Try the failed card again”. Filed #9 in place of #5." {
		t.Fatalf("result = %+v, want case 3 resolved with the retry that filed #9", got)
	}
	if steps := store.caseSteps[3]; len(steps) != 1 || !reflect.DeepEqual(steps[0].Did, want) || steps[0].Outcome != StepResolved {
		t.Fatalf("steps = %+v, want the one resolved step naming the retry", steps)
	}
	if !slices.ContainsFunc(wf.signals, func(s string) bool { return strings.HasPrefix(s, "t-b "+SignalCaseResolved+" alice") }) {
		t.Fatalf("signals = %q, want case_resolved to t-b", wf.signals)
	}
}

// resolveFails is a store whose ResolveCase finds its case ended.
type resolveFails struct {
	*fakeStore
	ended Case
}

func (s resolveFails) ResolveCase(context.Context, int64, CaseResolution, *CaseDraw, *CaseStep, *TaskSignal) error {
	return ErrCaseEnded{Case: s.ended}
}

// When the case cannot end after the retry filed its replacement, the
// error leads with what ran, so the human reads that #9 exists, and the
// session drawn beside it, which rolled back, is in neither Did nor Drawn.
func TestActSaysWhatRanWhenTheCaseCannotEnd(t *testing.T) {
	r, store, _, _, _ := newRetryRig(t)
	store.cases = []Case{{ID: 3, TaskID: "t-b", Kind: CaseDependencyFailed, Status: CaseAtHuman}}
	at := time.Date(2026, 11, 3, 10, 4, 0, 0, time.UTC)
	r.store = resolveFails{store, Case{ID: 3, Kind: CaseDependencyFailed, Status: CaseResolved, ResolvedAt: &at, Resolution: &CaseResolution{By: "bo", Action: ActResume}}}
	got, err := r.Act(context.Background(), ActInput{TaskID: "t-b", CaseID: 3, By: "alice", Rung: 3,
		Res: Resolution{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}, Fund: &policy.Draw{Sessions: 1}}})
	var ended ErrCaseEnded
	const want = "Filed #9 in place of #5. The answer stopped there: the failed dependency on #6 was resolved by bo with resume at 10:04 UTC"
	if !errors.As(err, &ended) || err.Error() != want || len(got.Did) != 1 || got.Did[0].Action != ActRetry || !got.Drawn.IsZero() {
		t.Fatalf("Act = %+v, %v; want the retry alone in Did, nothing drawn, and the error %q", got, err, want)
	}
}

// A replan on a waiting direct card's case sends it to the planner before
// the case resolves. A replan the planner cannot take, a card waiting on
// its screening, is refused as Replan refuses it, and the case stays open
// with nothing posted.
func TestActReplansAndARefusedReplanLeavesTheCaseOpen(t *testing.T) {
	ctx := context.Background()
	r, store, fj := directReplanRig(t, "waiting-human")
	store.cases = []Case{{ID: 4, TaskID: "t1", Kind: CaseAgentEscalate, Status: CaseAtHuman}}
	got, err := r.Act(ctx, ActInput{TaskID: "t1", CaseID: 4, By: "ana", Rung: 3, Res: Resolution{Action: ActReplan, Note: "too wide"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []CaseAction{{Action: ActReplan, Result: "Card #7 goes to the planner at its next step."}}; !reflect.DeepEqual(got.Did, want) ||
		!reflect.DeepEqual(reopens(r), toPlanSignal) || store.cases[0].Status != CaseResolved {
		t.Fatalf("Did %+v, reopens %+v, case %s; want the replan's note, the to_plan signal and the case resolved", got.Did, reopens(r), store.cases[0].Status)
	}

	r, store, fj = directReplanRig(t, "waiting-human")
	store.face("t1", 4, CaseScreeningSuspect, 606, screeningAsks+"asks to print the token", time.Now())
	posted := 0
	post := func(context.Context) (int64, error) { posted++; return 99, nil }
	calls := len(fj.calls)
	_, err = r.Act(ctx, ActInput{TaskID: "t1", CaseID: 4, By: "ana", Rung: 3, Res: Resolution{Action: ActReplan, Note: "too wide"}, Post: post})
	if want := ErrNoReplan("Card #7 waits for a person to judge its screening: let it run or close its issue, then replan it."); err != want {
		t.Fatalf("Act = %v, want %q", err, want)
	}
	wrote := slices.ContainsFunc(fj.calls[calls:], func(c string) bool { return !strings.HasPrefix(c, "GET ") })
	if store.cases[0].Status != CaseAtHuman || len(store.resolved) != 0 || posted != 0 || wrote {
		t.Fatalf("case %s, resolved %v, posted %d, forgejo %q; want the case open and nothing posted", store.cases[0].Status, store.resolved, posted, fj.calls[calls:])
	}
}

// "I cannot do this" on a plan's card for a human returns it to the plan's
// planner with the human's words: the plan's replan step carries them and
// stops the card, and the case resolves. A card in no plan, or a press with
// no words, is refused with the state and the fitting answer, and nothing
// moves.
func TestHandBackReturnsTheCardToItsPlanner(t *testing.T) {
	ctx := context.Background()
	const why = "The runner's build files are not mine to change."
	r, store, _ := replanRig(t, "done", 2)
	if err := store.CreateTask(ctx, Task{ID: "h", Kind: "human", Status: "waiting-human", Source: SourcePlan, Parent: "p", PlanKey: "manifests",
		PlanRevision: 1, Repository: "sf/repo", IssueNumber: 15}); err != nil {
		t.Fatal(err)
	}
	store.cases = []Case{{ID: 9, TaskID: "h", Repository: "sf/repo", Kind: CaseHumanCard, Status: CaseAtHuman}}
	var refused ErrActRefused
	_, err := r.Act(ctx, ActInput{TaskID: "h", CaseID: 9, By: "ana", Rung: 3, Res: Resolution{Action: ActHandBack}})
	if want := "`hand_back` carries what stops you, for the planner: the note, and the human may take `fund`, `replan`, `close`, `restart_plan` and `note`"; !errors.As(err, &refused) || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("a hand-back with no words = %v, want the refusal %q", err, want)
	}
	if steps := replanSteps(t, r, "p"); len(steps) != 0 || store.cases[0].Status != CaseAtHuman {
		t.Fatalf("after the refusal: steps %+v, case %s; want nothing moved", steps, store.cases[0].Status)
	}
	got, err := r.Act(ctx, ActInput{TaskID: "h", CaseID: 9, By: "ana", Rung: 3, Res: Resolution{Action: ActHandBack, Note: why}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []CaseAction{{Action: ActHandBack, Result: "Plan #10 goes back to the planner: 1 card stops."}}; !reflect.DeepEqual(got.Did, want) {
		t.Errorf("Did %+v, want %+v", got.Did, want)
	}
	want := []ReplanStep{{Origin: ReplanPerson, By: "ana", Comment: why, Stops: []string{"h"}, Revision: 2, Time: replanAt}}
	if steps := replanSteps(t, r, "p"); !reflect.DeepEqual(steps, want) {
		t.Errorf("the plan's replan steps %+v, want %+v", steps, want)
	}
	if h := store.tasks["h"]; h.Status != "closed" || store.cases[0].Status != CaseResolved {
		t.Errorf("the card is %s and its case %s, want closed and resolved", h.Status, store.cases[0].Status)
	}
	// a second card's hand-back while that replan is pending would lose its
	// words, even where the press would heal a reopen that failed partway
	p := store.tasks["p"]
	p.Status = "done"
	store.tasks["p"] = p
	if err := store.CreateTask(ctx, Task{ID: "h2", Kind: "human", Status: "waiting-human", Source: SourcePlan, Parent: "p", PlanKey: "docs",
		PlanRevision: 1, Repository: "sf/repo", IssueNumber: 16}); err != nil {
		t.Fatal(err)
	}
	store.cases = append(store.cases, Case{ID: 10, TaskID: "h2", Repository: "sf/repo", Kind: CaseHumanCard, Status: CaseAtHuman})
	_, err = r.Act(ctx, ActInput{TaskID: "h2", CaseID: 10, By: "ana", Rung: 3, Res: Resolution{Action: ActHandBack, Note: "Not mine either."}})
	if want := "Plan #10 is being replanned: wait for its revision."; !errors.As(err, &refused) || err.Error() != want || store.cases[1].Status != CaseAtHuman || store.tasks["h2"].Status != "waiting-human" {
		t.Errorf("a hand-back during the pending replan = %v, case %s, card %s; want %q and nothing moved", err, store.cases[1].Status, store.tasks["h2"].Status, want)
	}
	if p := store.tasks["p"]; p.Status != "queued" {
		t.Errorf("the plan is %s after the refused hand-back, want the stalled reopen healed to queued, so its revision comes", p.Status)
	}

	r, store, _ = replanRig(t, "done", 2)
	if err := store.CreateTask(ctx, Task{ID: "h", Kind: "human", Status: "waiting-human", Source: SourceWebhook, Repository: "sf/repo", IssueNumber: 15}); err != nil {
		t.Fatal(err)
	}
	store.cases = []Case{{ID: 9, TaskID: "h", Repository: "sf/repo", Kind: CaseHumanCard, Status: CaseAtHuman}}
	_, err = r.Act(ctx, ActInput{TaskID: "h", CaseID: 9, By: "ana", Rung: 3, Res: Resolution{Action: ActHandBack, Note: why}})
	if want := "this card is in no plan, so no planner takes it back: have the factory do it, or end the card with your note"; !errors.As(err, &refused) || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("a direct card's hand-back = %v, want the refusal %q", err, want)
	}
	if steps := replanSteps(t, r, "p"); len(steps) != 0 || store.tasks["h"].Status != "waiting-human" || store.cases[0].Status != CaseAtHuman {
		t.Errorf("after the refusals: steps %+v, card %s, case %s; want nothing moved", steps, store.tasks["h"].Status, store.cases[0].Status)
	}
}

// humanCardRig is the facts rig with plan card h1 (#14) for a human,
// waiting for d1 (#13, queued) and asking as its plan files it, its case 9
// at the human, and card k1 (#16) waiting for it; and card pr (#15), the factory's pull request #21 on a
// project without agents, its case 10 at the human.
func humanCardRig(t *testing.T) factsRig {
	t.Helper()
	g := newFactsRig(t)
	const repo = "demo-app/demo-app"
	h := Task{ID: "h1", Kind: "human", Repository: repo, IssueNumber: 14, Status: "waiting-human", Source: SourcePlan, Parent: "p"}
	pr := Task{ID: "pr", Kind: "human", Repository: repo, IssueNumber: 15, Status: "waiting-human", Source: SourceTemplateUpdate, PRNumber: 21}
	g.store.tasks["h1"], g.store.tasks["pr"] = h, pr
	g.store.tasks["d1"] = Task{ID: "d1", Kind: "agent", Repository: repo, IssueNumber: 13, Status: "queued"}
	g.store.tasks["k1"] = Task{ID: "k1", Kind: "agent", Repository: repo, IssueNumber: 16, PlanKey: "image", Status: "waiting-deps", Source: SourcePlan, Parent: "p"}
	g.store.deps = map[string][]string{"h1": {"d1"}, "k1": {"h1"}}
	g.store.cases = []Case{{ID: 9, TaskID: "h1", Repository: repo, Kind: CaseHumanCard, Status: CaseAtHuman},
		{ID: 10, TaskID: "pr", Repository: repo, Kind: CaseHumanCard, Status: CaseAtHuman}}
	for _, a := range []struct {
		t      Task
		ask    HumanAsk
		caseID int
	}{
		{h, PlanHumanAsk(Task{ID: "p", IssueNumber: 12}, "Change two lines in the runner's build files"), 9},
		{pr, HumanAsk{Q: FactoryPRQuestion(BriefText{Title: "Update the project from its template go-service"}, 21, "https://git.example/pulls/21")}, 10},
	} {
		e := a.ask.Event(a.t)
		e.Detail["case_id"] = a.caseID
		g.store.events = append(g.store.events, e)
	}
	return g
}

// "I did it" and "End the card" are two answers. Act refuses "I did it"
// while the card waits for a card that is not done, naming that card and
// the answers that fit, and nothing moves; "End the card" takes the
// human's words and ends the case. Only a human hands the card back.
func TestIDidItWaitsForTheCardBeforeIt(t *testing.T) {
	ctx := context.Background()
	g := humanCardRig(t)
	var refused ErrActRefused
	_, err := g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: 9, By: "sebastian", Rung: 3, Res: Resolution{Action: AnswerDone}})
	const end = "End the card (close) with your note, and the cards that wait for it ask you first"
	if want := "card #14 waits for #13 (queued), so its work cannot be done yet: answer Not now (park) until it is done, or " + end; !errors.As(err, &refused) || err.Error() != want {
		t.Fatalf("I did it while #13 waits = %v, want %q", err, want)
	}
	g.store.deps["k1"] = nil
	_, err = g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: 9, By: "sebastian", Rung: 3, Res: Resolution{Action: AnswerDone}})
	if want := "card #14 waits for #13 (queued), so its work cannot be done yet: answer Not now (park) until it is done, or End the card (close) with your note"; err == nil || err.Error() != want {
		t.Fatalf("I did it with no card waiting for #14 = %v, want %q", err, want)
	}
	g.store.deps["k1"] = []string{"h1"}
	d := g.store.tasks["d1"]
	d.Status = "closed"
	g.store.tasks["d1"] = d
	_, err = g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: 9, By: "sebastian", Rung: 3, Res: Resolution{Action: AnswerDone}})
	if want := "card #14 waits for #13, which was closed and will never be done: " + end; !errors.As(err, &refused) || err.Error() != want {
		t.Fatalf("I did it while #13 is closed = %v, want %q", err, want)
	}
	if g.store.cases[0].Status != CaseAtHuman || len(g.store.resolved) != 0 {
		t.Fatalf("case %s, resolved %v; want nothing moved", g.store.cases[0].Status, g.store.resolved)
	}
	_, err = g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: 9, By: "sebastian", Rung: 2, Res: Resolution{Action: ActHandBack, Note: "not mine"}})
	if want := "the factory agent may not pick `hand_back`: the human may, and the factory agent may take `fund`, `replan`, `close` and `note`"; !errors.As(err, &refused) || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("the chat's hand-back = %v, want the refusal %q", err, want)
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: 9, By: "sebastian", Rung: 3, Res: Resolution{Action: ActClose, Note: "Done another way."}}); err != nil {
		t.Fatalf("End the card = %v, want it taken", err)
	}
	if g.store.cases[0].Status != CaseResolved {
		t.Fatalf("case %s after End the card, want resolved", g.store.cases[0].Status)
	}
	d = g.store.tasks["d1"]
	d.Status = "done"
	g.store.tasks["d1"] = d
	g.store.cases[0].Status = CaseAtHuman
	if _, err := g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: 9, By: "sebastian", Rung: 3, Res: Resolution{Action: AnswerDone}}); err != nil {
		t.Fatalf("I did it once #13 is done = %v, want it taken", err)
	}
}

// End the card on a plan's human card ends it closed: the issue's close,
// which may reach factoryd before Act ends the case, takes the answer that
// ended the card, so the card after it finds its dependency ended closed
// and asks the human (dependency_closed). "I did it" ends it done, and the
// card after it starts.
func TestEndTheCardEndsAHumanCardClosed(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		res   Resolution
		final string
	}{{Resolution{Action: ActClose, Note: "The runner team did it in their repository."}, "closed"}, {Resolution{Action: AnswerDone}, "done"}} {
		g := humanCardRig(t)
		d := g.store.tasks["d1"]
		d.Status = "done"
		g.store.tasks["d1"] = d
		h := g.store.tasks["h1"]
		h.PlanKey = "manifests"
		g.store.tasks["h1"] = h
		in := intakeOn(t, g.store)
		// the issue's close reaches the intake while the answer's work runs
		closeIssue := func(ctx context.Context) (int64, error) {
			return 0, in.endByClose(ctx, g.store.tasks["h1"], "sebastian")
		}
		if _, err := g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: 9, By: "sebastian", Rung: 3, Res: tc.res, Work: closeIssue}); err != nil {
			t.Fatalf("%s: %v", tc.res.Action, err)
		}
		if got := g.store.tasks["h1"].Status; got != tc.final {
			t.Fatalf("%s ends the card %s, want %s", tc.res.Action, got, tc.final)
		}
		st, err := g.r.Dependencies(ctx, "k1")
		switch {
		case err != nil:
			t.Fatal(err)
		case tc.final == "closed" && (st.Ended == nil || DepEndedReason(*st.Ended) != "dependency #14 (manifests) ended closed: close this card, or answer to start it anyway"):
			t.Fatalf("after End the card the next card's dependencies = %+v, want #14 ended closed, which asks", st)
		case tc.final == "done" && (st.Ended != nil || len(st.Waiting) != 0):
			t.Fatalf("after I did it the next card's dependencies = %+v, want none left, so it starts", st)
		}
	}
}

// End the card says that the cards waiting for a human card ask first
// wherever such cards wait, in a plan or outside one, by the card's facts.
func TestEndTheCardSaysTheWaitingCardsAsk(t *testing.T) {
	card := Task{ID: "h", Kind: "human", Repository: "demo-app/demo-app", IssueNumber: 21, Status: "waiting-human", Source: SourceWebhook}
	c, err := CaseFromAsk(card, HumanAsk{Q: IssueQuestion("Rotate the bot token", "The token leaked.", "alice", false)}.Event(card), "")
	if err != nil {
		t.Fatal(err)
	}
	for waiters, want := range map[int]string{0: "Ends the card and closes its issue. Nothing more runs.",
		1: "Ends the card and closes its issue. The cards that wait for it ask you first."} {
		f := FactsOf(c)
		f.Waiters = waiters
		face := FaceOf(c, Facts{CaseFacts: f, Card: card})
		i := slices.IndexFunc(slices.Concat(face.Answers, face.More), func(a Answer) bool { return a.Key() == ActClose })
		if i < 0 || slices.Concat(face.Answers, face.More)[i].Effect != want {
			t.Errorf("End the card with %d cards waiting: %+v, want %q", waiters, slices.Concat(face.Answers, face.More), want)
		}
	}
}

// intakeOn is an intake on store, with a stub Forgejo.
func intakeOn(t *testing.T, store *fakeStore) *Intake {
	forgejo, _ := intakeForgejoStub(t)
	return &Intake{Store: store, Forgejo: forgejo, Workflows: &fakeWorkflows{}, BotUser: "factory-agent",
		Timeline: &Timeline{Objects: &fakeObjectStore{}, Store: store, Events: store}}
}

// An End the card whose work failed leaves no end behind: Not now, pressed
// after it, records that the card ends done, so a close in the forge later
// ends it done.
func TestNotNowAfterAFailedEndTheCard(t *testing.T) {
	ctx := context.Background()
	g := humanCardRig(t)
	refused := func(context.Context) (int64, error) { return 0, errors.New("forgejo refused the close") }
	if _, err := g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: 9, By: "sebastian", Rung: 3, Res: Resolution{Action: ActClose}, Work: refused}); err == nil {
		t.Fatal("End the card with a failing close = nil, want its failure")
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: 9, By: "sebastian", Rung: 3, Res: Resolution{Action: AnswerPark}}); err != nil {
		t.Fatalf("Not now = %v, want it taken", err)
	}
	if err := intakeOn(t, g.store).endByClose(ctx, g.store.tasks["h1"], "sebastian"); err != nil {
		t.Fatal(err)
	}
	if got := g.store.tasks["h1"].Status; got != "done" {
		t.Fatalf("a close in the forge after Not now ends the card %s, want done", got)
	}
}

// No pull request the factory opened on a project without agents merges or
// closes without a human: the project agent's merge and rejection are
// refused at the card, naming the state and that a human presses them; the
// human's are taken.
func TestAFactoryPullRequestWaitsForAHuman(t *testing.T) {
	ctx := context.Background()
	for _, action := range []string{ActApproveMerge, ActDenyMerge} {
		g := humanCardRig(t)
		work := 0
		in := ActInput{TaskID: "pr", CaseID: 10, By: "sebastian", Rung: 2, Res: Resolution{Action: action, Note: "Looks right."},
			Work: func(context.Context) (int64, error) { work++; return 0, nil }}
		_, err := g.r.Act(ctx, in)
		const want = "Pull request #21 of card #15 was opened by the factory, and no gate checked it: a human must press Merge it or Reject and close it on the card. Record your analysis and the option you recommend (`attach_analysis`)."
		var refused ErrActRefused
		if !errors.As(err, &refused) || err.Error() != want || work != 0 || g.store.cases[1].Status != CaseAtHuman {
			t.Fatalf("the project agent's %s = %v, work %d, case %s; want %q and nothing done", action, err, work, g.store.cases[1].Status, want)
		}
		in.Rung = 3
		if _, err := g.r.Act(ctx, in); err != nil || work != 1 || g.store.cases[1].Status != CaseResolved {
			t.Fatalf("the human's %s = %v, work %d, case %s; want it taken", action, err, work, g.store.cases[1].Status)
		}
	}
}

// A human card's case is the human's or the chat's: rung 1 is refused,
// and a card that asks no question with choices takes a note.
func TestActHumanCardRefusals(t *testing.T) {
	g := newFactsRig(t)
	g.store.tasks["h1"] = Task{ID: "h1", Kind: "human", Repository: "demo-app/demo-app", IssueNumber: 14, Status: "waiting-human"}
	g.store.cases = []Case{{ID: 9, TaskID: "h1", Kind: CaseHumanCard, Status: CaseAtHuman}}
	for rung, want := range map[int]string{
		1: "the check-in may not answer a human card: the factory agent and the human may",
		3: "card #14 asks no question with choices: answer it with a note",
	} {
		_, err := g.r.Act(context.Background(), ActInput{TaskID: "h1", CaseID: 9, By: "sebastian", Rung: rung, Res: Resolution{Action: AnswerDismiss}})
		var refused ErrActRefused
		if !errors.As(err, &refused) || err.Error() != want {
			t.Errorf("rung %d = %v, want %q", rung, err, want)
		}
	}
	if len(g.store.resolved) != 0 {
		t.Fatalf("resolved %v, want none", g.store.resolved)
	}
}

// Rung 0 is factoryd's, and factoryd answers on rung 0 alone: a human's
// answer on rung 0 and factoryd's on rung 2 are refused, and nothing is
// written.
func TestActKeysRungZeroOnFactoryd(t *testing.T) {
	g := newFactsRig(t)
	g.card()
	g.timeline(t)
	g.store.cases = append(g.store.cases, Case{ID: 7, TaskID: "t1", Kind: CaseSessionCap, Status: CaseAtHuman})
	events := len(g.store.events)
	for _, c := range []struct {
		in   ActInput
		want string
	}{
		{ActInput{By: "sebastian", Rung: 0}, "Only the factory answers for itself. Answer as the check-in, the factory agent or a human."},
		{ActInput{By: FactoryDecider, Rung: 2}, "The factory answers for itself alone, not as the factory agent."},
	} {
		c.in.TaskID, c.in.CaseID, c.in.Res = "t1", 7, Resolution{Action: ActWithdraw}
		var refused ErrActRefused
		if _, err := g.r.Act(context.Background(), c.in); !errors.As(err, &refused) || err.Error() != c.want {
			t.Errorf("%s on rung %d = %v, want the refusal %q", c.in.By, c.in.Rung, err, c.want)
		}
	}
	if len(g.store.events) != events || len(g.store.resolved) != 0 || g.store.cases[len(g.store.cases)-1].Status != CaseAtHuman {
		t.Fatalf("a refused act wrote %d events, resolved %v; want none", len(g.store.events)-events, g.store.resolved)
	}
}

// Card t-b (#6) and card t-c (#7) wait on failed t-a (#5), each on its
// own dependency case. alice's retry on t-b's case files #9 in place of
// #5, which both cards then wait on. Her retry on t-c's case, the same
// failed dependency, files nothing more: it resolves the case naming #9,
// and #9 is the one replacement.
func TestActRetryOfARetriedDependencyNamesTheReplacement(t *testing.T) {
	for _, second := range []ActInput{{By: "alice", Rung: 3}, {By: FactoryDecider, Rung: 0}} {
		retriedTwice(t, second)
	}
}

// retriedTwice is the replacement test's run, t-c's case answered by
// second's answerer: the human on rung 3, or rung 0.
func retriedTwice(t *testing.T, second ActInput) {
	t.Helper()
	r, store, _, fj, _ := newRetryRig(t)
	ctx := context.Background()
	if err := store.CreateTask(ctx, Task{ID: "t-c", Kind: "agent", Status: "waiting-human", Track: policy.TrackDirect, Process: "spec",
		Repository: "linkshort/linkshort", IssueNumber: 7, Source: SourcePlan, Parent: "t-plan", PlanKey: "stats", BudgetEUR: 2,
		Author: "sebastian", AuthorWriter: true, SHA: "b0", Objective: "Stats"}); err != nil {
		t.Fatal(err)
	}
	store.AddTaskDeps(ctx, "t-c", []string{"t-a"})
	store.cases = []Case{{ID: 3, TaskID: "t-b", Kind: CaseDependencyFailed, StateKey: "t-a", Status: CaseAtHuman},
		{ID: 4, TaskID: "t-c", Kind: CaseDependencyFailed, StateKey: "t-a", Status: CaseAtHuman}}
	retry := Resolution{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}}
	if _, err := r.Act(ctx, ActInput{TaskID: "t-b", CaseID: 3, By: "alice", Rung: 3, Res: retry}); err != nil {
		t.Fatal(err)
	}
	issues := len(fj.issues)
	got, err := r.Act(ctx, ActInput{TaskID: "t-c", CaseID: 4, By: second.By, Rung: second.Rung, Res: retry})
	if err != nil {
		t.Fatalf("%s: %v", second.By, err)
	}
	want := []CaseAction{{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}, Result: "#5 was retried as #9 already."}}
	if !reflect.DeepEqual(got.Did, want) || got.Case.Status != CaseResolved || got.Case.Resolution.Action != ActRetry {
		t.Fatalf("result = %+v, want case 4 resolved with the retry naming #9", got)
	}
	if cards := cardsByKey(t, store); len(fj.issues) != issues || len(cards) != 4 || cards["links-api"].IssueNumber != 9 {
		t.Fatalf("issues %d, plan cards %v; want no new issue and #9 the one replacement of the four", len(fj.issues), slices.Sorted(maps.Keys(cards)))
	}
}

// A card stopped at its agent time cap takes an extend of its agent time,
// not one of the sessions alone: that raises nothing the stop names, is
// refused with the stop's own words and the minutes each rung may ask for
// (the large card's draw of 90 minutes, the pool's 30 hours) before it
// draws — no row, no raise, the case open — so the stop's next answer is
// not already weighed as a draw that bought nothing. The same extend with its minutes of agent time
// raises the card and resolves the stop.
func TestActRefusesASessionsOnlyExtendAtTheTimeCap(t *testing.T) {
	g := newFactsRig(t)
	g.store.cases = append(g.store.cases, Case{ID: 7, TaskID: "t1", Kind: CaseAgentTimeCap, Status: CaseAtHuman,
		Facts: map[string]any{"trip": "trip-701"}})
	events, usage := len(g.store.events), g.store.tasks["t1"].Usage
	in := ActInput{TaskID: "t1", CaseID: 7, By: "sebastian", Via: ViaConsole, Rung: 3,
		Res: Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}, Fund: &policy.Draw{EUR: 1}}}
	var refused ErrActRefused
	if _, err := g.r.Act(context.Background(), in); !errors.As(err, &refused) ||
		!strings.HasPrefix(err.Error(), "this card stopped for agent time: an extend of its agent time starts the next session, and sessions or lines alone buy none. "+
			"The card's draw leaves 90 minutes and the pool 1800 minutes of agent time, so the check-in may ask for up to 90 minutes, the factory agent for up to 600 minutes and the human for up to 600 minutes, ") {
		t.Fatalf("a sessions-only extend at the time cap = %v, want the stop's own refusal", err)
	}
	if len(g.store.events) != events || g.store.tasks["t1"].Usage != usage || len(g.store.resolved) != 0 ||
		g.store.cases[len(g.store.cases)-1].Status != CaseAtHuman {
		t.Fatalf("the refused extend wrote rows (%d events, usage %+v, resolved %v); want nothing drawn and the case open",
			len(g.store.events)-events, g.store.tasks["t1"].Usage, g.store.resolved)
	}
	in.Res = Resolution{Action: ActExtend, Args: map[string]any{"agent_time_s": 300}}
	in.Post = func(context.Context) (int64, error) { return 801, nil }
	out, err := g.r.Act(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Drawn.AgentTime != 5*time.Minute || g.store.tasks["t1"].Usage.RaisedTime != usage.RaisedTime+5*time.Minute {
		t.Fatalf("drawn %+v, usage %+v; want the 5 minutes of agent time drawn and raised", out.Drawn, g.store.tasks["t1"].Usage)
	}
}

// An agent's question at its session cap is the cap's case first. Rung 2's
// extend on the case while it sits on rung 2 is refused, so the rung hands
// it up with the extend as its recommendation. The human's extend draws the session and leaves the question
// open, unanswered and unsignalled, and the face stored again is the
// question's, with the agent's answers.
func TestAnExtendOnAQuestionAtTheCapLeavesTheQuestionOpen(t *testing.T) {
	g := newFactsRig(t)
	ctx := context.Background()
	q := Case{ID: 7, TaskID: "t1", Kind: CaseAgentEscalate, Status: CaseAtHuman,
		Facts: map[string]any{"trip": "trip-7", "reason": AgentAsks + "Which store keeps the snapshots?\nWhy: two fit\nRecommendation: SQLite", "choices": []any{"Postgres", "SQLite"}}}
	g.store.cases = append(g.store.cases, q)
	card := g.store.tasks["t1"]
	card.SpentEUR, card.HeldEUR, card.CeilingEUR = 1, 0, 10
	f, err := g.r.FactsAtCase(ctx, card, q)
	if err != nil {
		t.Fatal(err)
	}
	card.Usage.Attempts = f.SessionCap
	g.store.tasks["t1"] = card
	face := func() AskFace {
		t.Helper()
		f, err := g.r.FactsAtCase(ctx, g.store.tasks["t1"], q)
		if err != nil {
			t.Fatal(err)
		}
		return FaceOf(q, Facts{CaseFacts: f, Card: g.store.tasks["t1"]})
	}
	capped := face()
	if len(capped.Answers) == 0 || capped.Answers[0].Action != ActExtend || capped.Story.Happened != "The agent asks: Which store keeps the snapshots?" {
		t.Fatalf("at the cap the face is %q with %v and story %+v; want the cap's extend first and the question as a story line", capped.Ask, capped.Answers, capped.Story)
	}
	// the ladder's rung 2 working the case on its own rung draws nothing
	extend := ActInput{TaskID: "t1", CaseID: 7, By: "sebastian", Via: ViaChat, Rung: 2, Res: Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}}}
	at := len(g.store.cases) - 1
	g.store.cases[at].Status = CaseOpen
	var refused ErrActRefused
	if _, err := g.r.Act(ctx, extend); !errors.As(err, &refused) ||
		!strings.HasPrefix(err.Error(), "this ask is the agent's question, which a draw does not answer: hand it up with the draw as your recommendation") {
		t.Fatalf("rung 2's extend on the question on its rung = %v, want it refused, to be handed up", err)
	}
	g.store.cases[at].Status = CaseAtHuman
	f, _ = g.r.FactsAtCase(ctx, card, q)
	res, err := capped.Answers[0].Press(Facts{CaseFacts: f, Card: card, Case: q}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	out, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: 7, By: "sebastian", Via: ViaConsole, Rung: 3, Res: res})
	if err != nil {
		t.Fatal(err)
	}
	if out.Drawn.Sessions != 1 || len(g.store.resolved) != 0 || g.store.cases[len(g.store.cases)-1].Status != CaseAtHuman || len(g.store.pending) != 0 {
		t.Fatalf("drawn %+v, resolved %v, case %s, signals %v; want one session drawn and the question open", out.Drawn, g.store.resolved,
			g.store.cases[len(g.store.cases)-1].Status, g.store.pending)
	}
	open := face()
	if open.Ask != "Which store keeps the snapshots?" || !slices.ContainsFunc(open.Answers, func(a Answer) bool { return a.Key() == "say:SQLite" }) {
		t.Fatalf("with room the face is %q with %v; want the question with the agent's answers", open.Ask, open.Answers)
	}
	if stored := g.store.faces[7]; stored == nil || stored["ask"] != open.Ask {
		t.Fatalf("the stored face is %v; want the question's, stored again", stored)
	}
}

// The project agent's fund in a human's thread on a card whose question
// waits at the human, out of money, draws: the question stays open,
// unanswered, and its face is stored again, the question's once the money
// funds a session. 4.50 of 5.00 EUR spent leaves 0.50 EUR, under the
// rig's opening of about 1 EUR; 2.00 EUR more leaves 2.50 EUR.
func TestTheChatsFundOnAWaitingQuestionDrawsAndLeavesItOpen(t *testing.T) {
	g := newFactsRig(t)
	ctx := context.Background()
	q := Case{ID: 7, TaskID: "t1", Kind: CaseAgentEscalate, Status: CaseAtHuman,
		Facts: map[string]any{"trip": "trip-7", "reason": AgentAsks + "Which store keeps the snapshots?\nWhy: two fit\nRecommendation: SQLite", "choices": []any{"Postgres", "SQLite"}}}
	g.store.cases = append(g.store.cases, q)
	card := g.store.tasks["t1"]
	card.HeldEUR, card.CeilingEUR, card.Usage.Attempts = 0, 10, 1
	g.store.tasks["t1"] = card
	out, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: 7, By: "sebastian", Via: ViaChat, Rung: 2, Res: Resolution{Action: ActFund, Args: map[string]any{"eur": 2.0}}})
	if err != nil {
		t.Fatalf("the chat's fund = %v, want it drawn", err)
	}
	if got := g.store.tasks["t1"].BudgetEUR; got != 7 || len(g.store.resolved) != 0 || g.store.cases[len(g.store.cases)-1].Status != CaseAtHuman || out.Drawn.EUR != 2 {
		t.Fatalf("budget %.2f, drawn %+v, resolved %v; want 7.00 EUR and the question open", got, out.Drawn, g.store.resolved)
	}
	if stored := g.store.faces[7]; stored == nil || stored["ask"] != "Which store keeps the snapshots?" {
		t.Fatalf("the stored face is %v; want the question's", stored)
	}
}

// The minute pass keeps an agent's question's face with its caps, both
// ways, on facts read whole, with no timer of its own: a cap that binds
// after the ask stores the cap's face; a cap that lifts, at its reset or by
// a raise outside the card, stores the question with the agent's answers;
// a move from one cap to another stores the new cap's; a face that still
// fits, or facts whose limits could not be read, store nothing.
func TestTheOffersPassKeepsAQuestionsFaceWithItsCaps(t *testing.T) {
	g := newFactsRig(t)
	ctx := context.Background()
	q := Case{ID: 7, TaskID: "t1", Kind: CaseAgentEscalate, Status: CaseAtHuman,
		Facts: map[string]any{"trip": "trip-7", "reason": AgentAsks + "Which store keeps the snapshots?\nWhy: two fit\nRecommendation: SQLite", "choices": []any{"Postgres", "SQLite"}}}
	g.store.cases = append(g.store.cases, q)
	card := g.store.tasks["t1"]
	card.SpentEUR, card.HeldEUR = 1, 0
	g.store.tasks["t1"] = card
	f, err := g.r.FactsAtCase(ctx, card, q)
	if err != nil {
		t.Fatal(err)
	}
	limit := f.SessionCap
	set := func(attempts int, spent float64) {
		card := g.store.tasks["t1"]
		card.Usage.Attempts, card.SpentEUR = attempts, spent
		g.store.tasks["t1"] = card
	}
	// asked writes the ask row of the face on the card as it stands now
	asked := func() {
		f, err := g.r.FactsAtCase(ctx, g.store.tasks["t1"], q)
		if err != nil {
			t.Fatal(err)
		}
		g.event("t1", factsNow, EventQuestionAsked, FaceRow(q, g.store.tasks["t1"], f, "", nil, func(s string) string { return s }, func(s string) string { return s }))
		g.store.events[len(g.store.events)-1].Detail["case_id"] = q.ID
	}
	pass := func() any {
		g.store.faces = nil
		g.r.StoreMissedOffers(ctx)
		if stored := g.store.faces[7]; stored != nil {
			return stored["ask"]
		}
		return nil
	}
	const question, sessions = "Which store keeps the snapshots?", "The card used all %d sessions. How should it go on?"
	set(limit-1, 1)
	asked()
	if got := pass(); got != nil {
		t.Errorf("a face that fits: the pass stored %v, want nothing", got)
	}
	set(limit, 1)
	if got, want := pass(), fmt.Sprintf(sessions, limit); got != want {
		t.Errorf("the session cap bound after the ask: the pass stored %v, want %q", got, want)
	}
	asked()
	set(limit-1, 1)
	if got := pass(); got != question {
		t.Errorf("the cap lifted: the pass stored %v, want the question", got)
	}
	set(limit, 1)
	asked()
	set(limit, 4.99)
	if got := pass(); got == nil || !strings.HasPrefix(fmt.Sprint(got), "The card's money left does not pay its next session") {
		t.Errorf("the money ran out while the sessions were used up: the pass stored %v, want the budget floor's ask", got)
	}
	g.r.cfg.Prices = aliasPrices{}
	set(limit-1, 1)
	if got := pass(); got != nil {
		t.Errorf("the floor could not be read: the pass stored %v, want nothing", got)
	}
}

// A fund on a card that ended is refused naming its end, with no case
// needed, and nothing is drawn.
func TestActRefusesAFundOnAnEndedCard(t *testing.T) {
	g := newFactsRig(t)
	card := g.store.tasks["t1"]
	card.Status = "done"
	g.store.tasks["t1"] = card
	_, err := g.r.Act(context.Background(), ActInput{TaskID: "t1", By: "sebastian", Rung: 3, Res: Resolution{Action: ActFund, Args: map[string]any{"eur": 1.0}}})
	var refused ErrActRefused
	if !errors.As(err, &refused) || !strings.HasPrefix(err.Error(), "this card ended done: a fund buys nothing, file a new card") {
		t.Fatalf("a fund on a done card = %v, want the refusal naming its end", err)
	}
	if len(g.store.events) != 0 || g.store.tasks["t1"].BudgetEUR != 5 {
		t.Fatalf("events %+v, budget %v; want nothing written and 5 EUR", g.store.events, g.store.tasks["t1"].BudgetEUR)
	}
}

// A grant the run made itself, the session a change request bought at the
// cap, is the writer's draw against the pool: a card_extended row of rung
// 3 that the pool counts, never refused past the pool, and no raise on the
// card's row, whose run granted it already.
func TestRecordGrantDrawsAgainstThePool(t *testing.T) {
	g := newFactsRig(t)
	ctx := context.Background()
	before := g.store.tasks["t1"].Usage
	drawnBefore, _ := g.store.PoolDrawn(ctx, "demo-app/demo-app", time.Now())
	if err := g.r.RecordGrant(ctx, "t1", "sebastian", policy.Draw{Sessions: 1}, IdentityKey(EventCardExtended, "run-1", "3")); err != nil {
		t.Fatal(err)
	}
	drawn, _ := g.store.PoolDrawn(ctx, "demo-app/demo-app", time.Now())
	if drawn.Minus(drawnBefore) != (policy.Draw{Sessions: 1}) {
		t.Fatalf("the pool drew %+v more, want 1 session", drawn.Minus(drawnBefore))
	}
	last := g.store.events[len(g.store.events)-1]
	if last.Kind != EventCardExtended || last.Actor != "sebastian" || last.Detail["rung"] != 3 || last.Detail["pool"] != true {
		t.Fatalf("the row = %+v, want sebastian's pool draw on rung 3", last)
	}
	if u := g.store.tasks["t1"].Usage; u.RaisedSessions != before.RaisedSessions || u.RaisedTime != before.RaisedTime {
		t.Fatalf("usage = %+v, want the raises of %+v", u, before)
	}
}

// The audit's grant at a cap, which no writer's answer bought, is the
// factory's own draw (versionAuditDraw): a card_extended row of rung 0
// that the pool counts, that raises nothing on the card's row and is
// refused past a bound as a typed draw-refused, never a raise.
func TestAuditDrawIsTheFactoryRungZeroDraw(t *testing.T) {
	g := newFactsRig(t)
	ctx := context.Background()
	before := g.store.tasks["t1"].Usage
	drawnBefore, _ := g.store.PoolDrawn(ctx, "demo-app/demo-app", time.Now())
	if err := g.r.AuditDraw(ctx, "t1", policy.Draw{Sessions: 1}, IdentityKey(EventCardExtended, "run-1", "1", "0s")); err != nil {
		t.Fatal(err)
	}
	drawn, _ := g.store.PoolDrawn(ctx, "demo-app/demo-app", time.Now())
	if drawn.Minus(drawnBefore) != (policy.Draw{Sessions: 1}) {
		t.Fatalf("the pool drew %+v more, want 1 session", drawn.Minus(drawnBefore))
	}
	last := g.store.events[len(g.store.events)-1]
	if last.Kind != EventCardExtended || last.Actor != FactoryDecider || last.Detail["rung"] != 0 || last.Detail["pool"] != true {
		t.Fatalf("the row = %+v, want the factory's pool draw on rung 0", last)
	}
	if u := g.store.tasks["t1"].Usage; u.RaisedSessions != before.RaisedSessions || u.RaisedTime != before.RaisedTime {
		t.Fatalf("usage = %+v, want the raises of %+v", u, before)
	}
	var app *temporal.ApplicationError
	if err := g.r.AuditDraw(ctx, "t1", policy.Draw{Sessions: 99}, IdentityKey(EventCardExtended, "run-1", "2", "0s")); !errors.As(err, &app) || app.Type() != ErrDrawRefused {
		t.Fatalf("past the bound = %v, want a typed draw-refused", err)
	}
}

// By hand: the project agent's change request at the cap is rung 2's draw:
// a card_extended row of rung 2 by factory-chat that the pool counts,
// raising nothing on the card's row, and refused past the pool with a
// typed draw-refused, as rung 2's own extend is.
func TestAgentDrawIsRungTwosDrawAgainstThePool(t *testing.T) {
	g := newFactsRig(t)
	ctx := context.Background()
	before := g.store.tasks["t1"].Usage
	drawnBefore, _ := g.store.PoolDrawn(ctx, "demo-app/demo-app", time.Now())
	if err := g.r.AgentDraw(ctx, "t1", ChatUser, policy.Draw{Sessions: 1}, IdentityKey(EventCardExtended, "run-1", "1", "0s")); err != nil {
		t.Fatal(err)
	}
	drawn, _ := g.store.PoolDrawn(ctx, "demo-app/demo-app", time.Now())
	if drawn.Minus(drawnBefore) != (policy.Draw{Sessions: 1}) {
		t.Fatalf("the pool drew %+v more, want 1 session", drawn.Minus(drawnBefore))
	}
	last := g.store.events[len(g.store.events)-1]
	if last.Kind != EventCardExtended || last.Actor != ChatUser || last.Detail["rung"] != 2 || last.Detail["pool"] != true {
		t.Fatalf("the row = %+v, want factory-chat's pool draw on rung 2", last)
	}
	if u := g.store.tasks["t1"].Usage; u.RaisedSessions != before.RaisedSessions || u.RaisedTime != before.RaisedTime {
		t.Fatalf("usage = %+v, want the raises of %+v", u, before)
	}
	rows := len(g.store.events)
	var app *temporal.ApplicationError
	if err := g.r.AgentDraw(ctx, "t1", ChatUser, policy.Draw{Sessions: 99}, IdentityKey(EventCardExtended, "run-1", "2", "0s")); !errors.As(err, &app) || app.Type() != ErrDrawRefused {
		t.Fatalf("past the pool = %v, want a typed draw-refused", err)
	}
	if len(g.store.events) != rows {
		t.Fatalf("the refused draw wrote %+v", g.store.events[rows:])
	}
}

// Forgejo was down when a retry of failed t-a (#5) filed its replacement's
// issue: the replacement stands in its place and t-b waits on it, with no
// issue. alice's retry on t-b's case finishes that retry: #9 is filed for
// the replacement and #5 closes, and the case resolves naming #9.
func TestActFinishesAStoppedRetry(t *testing.T) {
	const replacement = "t-33198a8c2c69"
	r, store, _, fj, _ := newRetryRig(t)
	ctx := context.Background()
	fj.failCall = "POST issues"
	if _, err := r.RetryCard(ctx, "t-a", "alice"); err == nil || store.tasks[replacement].IssueNumber != 0 {
		t.Fatalf("first retry: %v, replacement on #%d; want it stopped before its issue", err, store.tasks[replacement].IssueNumber)
	}
	store.cases = []Case{{ID: 3, TaskID: "t-b", Kind: CaseDependencyFailed, StateKey: "t-a", Status: CaseAtHuman}}
	got, err := r.Act(ctx, ActInput{TaskID: "t-b", CaseID: 3, By: "alice", Rung: 3, Res: Resolution{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []CaseAction{{Action: ActRetry, Args: map[string]any{"task_id": "t-a", "replacement": int64(9)}, Result: "Filed #9 in place of #5."}}
	if !reflect.DeepEqual(got.Did, want) || got.Case.Status != CaseResolved {
		t.Fatalf("result = %+v, want case 3 resolved with the retry that filed #9", got)
	}
	if store.tasks[replacement].IssueNumber != 9 || fj.issues[5].state != "closed" {
		t.Fatalf("replacement on #%d, #5 %s; want #9 and #5 closed", store.tasks[replacement].IssueNumber, fj.issues[5].state)
	}
}

// The fake ends a case as PGStore's endCase does: the webhook's copy of
// comment 77 resolved case 3 as a plain answer, and the answer that posted
// 77 resolves it again with its own action. An answer on another comment
// finds it ended.
func TestFakeResolveCaseTakesOverTheWebhooksCopy(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	at := time.Now()
	store.cases = []Case{{ID: 3, TaskID: "t1", Status: CaseResolved, ResolvedAt: &at,
		Resolution: &CaseResolution{By: "sebastian", Rung: 3, Action: CaseAnswer, Args: map[string]any{"answer": "go on", "comment": 77}}}}
	if err := store.ResolveCase(ctx, 3, CaseResolution{By: "ana", Rung: 3, Action: ActContinue, Args: map[string]any{"comment": int64(78)}}, nil, nil, nil); !errors.As(err, new(ErrCaseEnded)) {
		t.Fatalf("an answer on comment 78 = %v, want the case ended", err)
	}
	if err := store.ResolveCase(ctx, 3, CaseResolution{By: "sebastian", Rung: 3, Action: ActExtend, Args: map[string]any{"comment": int64(77)}}, nil, nil, nil); err != nil {
		t.Fatalf("the answer on comment 77 = %v, want it resolved again", err)
	}
	if c := store.cases[0]; c.Status != CaseResolved || c.Resolution.Action != ActExtend {
		t.Fatalf("case = %+v, %+v; want resolved again with extend", c, c.Resolution)
	}
}

// A checks wait's stop answered with wait_checks is accepted on a card at
// its agent time cap, where resume is refused for want of agent time: the
// wait spends none. The same action on any other stop is refused.
func TestActWaitChecksIsAcceptedAtTheAgentTimeCap(t *testing.T) {
	g := newFactsRig(t)
	g.card()
	g.timeline(t)
	card := g.store.tasks["t1"]
	card.Usage.AgentTime = 24 * time.Hour
	g.store.tasks["t1"] = card
	g.store.cases = append(g.store.cases,
		Case{ID: 7, TaskID: "t1", Kind: CaseGateStop, Status: CaseAtHuman, Facts: map[string]any{"gate": ActivityWaitChecks}},
		Case{ID: 8, TaskID: "t1", Kind: CaseGateStop, Status: CaseAtHuman, Facts: map[string]any{"gate": ActivityMerge}})
	ctx := context.Background()
	var refused ErrActRefused
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: 7, By: "sebastian", Rung: 3, Res: Resolution{Action: ActResume}}); !errors.As(err, &refused) ||
		!strings.Contains(err.Error(), "agent time cap") {
		t.Fatalf("resume at the cap = %v, want the agent time cap's refusal", err)
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: 8, By: "sebastian", Rung: 3, Res: Resolution{Action: ActWaitChecks}}); !errors.As(err, &refused) ||
		!strings.Contains(err.Error(), "no checks wait's stop") {
		t.Fatalf("wait_checks on a merge's stop = %v, want the refusal naming it", err)
	}
	out, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: 7, By: "sebastian", Rung: 3, Res: Resolution{Action: ActWaitChecks},
		Post: func(context.Context) (int64, error) { return 5, nil }})
	if err != nil {
		t.Fatalf("wait_checks at the cap = %v, want it accepted", err)
	}
	if out.Case.Resolution == nil || out.Case.Resolution.Action != ActWaitChecks {
		t.Fatalf("case = %+v, want it resolved by wait_checks", out.Case)
	}
}

// A human card's answer draws nothing and takes no args: Act refuses a fund or
// args beside a choice, whichever entry sent it.
func TestHumanChoiceTakesNoFundAndNoArgs(t *testing.T) {
	r := new(Runner)
	task := Task{ID: "t1", IssueNumber: 12}
	for name, res := range map[string]Resolution{"fund": {Action: ActClose, Fund: &policy.Draw{EUR: 1}}, "args": {Action: ActClose, Args: map[string]any{"sessions": 1}}} {
		why, _ := r.humanChoice(context.Background(), task, Case{Kind: CaseHumanCard}, ActInput{Rung: 3, Res: res})
		if !strings.Contains(why, "card #12 is a human card") {
			t.Errorf("%s: refusal %q; want a human card's refusal", name, why)
		}
	}
}

// The agent card's settle with a park arg reaches Act as a settle: the
// case resolves, the ruling is the answer's row, and the run hears
// case_resolved. A human card's "Not now" (its park) keeps its case live
// as the chain stall's rerun_again does: the card goes on waiting with its
// ask open, the press a step on its case, no ruling and no run signalled.
func TestASettleEndsTheCaseAndAHumanCardsParkKeepsIt(t *testing.T) {
	g := newFactsRig(t)
	g.card()
	g.timeline(t)
	g.r.timeline = &Timeline{Events: g.store, Store: g.store}
	ctx := context.Background()
	g.store.cases = append(g.store.cases, Case{ID: 7, TaskID: "t1", Kind: CaseSessionCap, Status: CaseAtHuman})
	got, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: 7, By: "sebastian", Via: ViaChat, Rung: 3,
		Res:  Resolution{Action: ActSettle, Args: map[string]any{"park": AnswerPark}, Note: "Park it"},
		Post: func(context.Context) (int64, error) { return 5, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if got.Case.Status != CaseResolved || got.Comment != 5 || !slices.Equal(g.store.resolved, []int64{7}) || !strings.Contains(got.Text, "resolved with “It is fine, go on”") {
		t.Fatalf("result = %+v, resolved %v; want case 7 settled by the park, the member's comment its own", got, g.store.resolved)
	}
	answered := 0
	for _, e := range g.store.events {
		if e.Kind == EventQuestionAnswered {
			answered++
			if e.TaskID != "t1" || e.Actor != "sebastian" || e.Via != ViaChat || e.Detail["case_id"] != int64(7) ||
				e.Detail["action"] != ActSettle || e.Detail["comment"] != int64(5) {
				t.Fatalf("the ruling's row = %+v, want sebastian's settle of case 7 via chat", e)
			}
		}
	}
	if answered != 1 {
		t.Fatalf("%d answer rows, want the park's alone", answered)
	}
	if !slices.ContainsFunc(g.r.cfg.Workflows.(*fakeWorkflows).signals, func(s string) bool {
		return strings.HasPrefix(s, "t1 "+SignalCaseResolved+" sebastian")
	}) {
		t.Fatalf("signals = %q, want case_resolved to t1's run", g.r.cfg.Workflows.(*fakeWorkflows).signals)
	}

	g.store.tasks["h1"] = Task{ID: "h1", Kind: "human", Repository: "demo-app/demo-app", IssueNumber: 14, Status: "waiting-human"}
	for id, tc := range map[int64]struct {
		value, want string
	}{9: {AnswerPark, CaseAtHuman}, 10: {AnswerRerunAgain, CaseAtHuman}} {
		g.store.cases = append(g.store.cases, Case{ID: id, TaskID: "h1", Kind: CaseHumanCard, Status: CaseAtHuman})
		g.event("h1", factsNow, EventQuestionAsked, map[string]any{"case_id": int64(id),
			"offers": []AskOffer{{Value: tc.value, Label: "the choice"}}})
		got, err := g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: id, By: "sebastian", Rung: 3, Res: Resolution{Action: tc.value, Note: "the choice"}})
		if err != nil {
			t.Fatalf("the choice %s: %v", tc.value, err)
		}
		if got.Case.Status != tc.want {
			t.Fatalf("after %s the case is %s, want %s", tc.value, got.Case.Status, tc.want)
		}
		if s := g.store.tasks["h1"].Status; s != "waiting-human" {
			t.Fatalf("after %s the card is %s, want it parked in waiting-human", tc.value, s)
		}
		if len(g.store.caseSteps[id]) == 0 {
			t.Fatalf("after %s case %d holds no step of the press", tc.value, id)
		}
	}
	answered = 0
	for _, e := range g.store.events {
		if e.Kind == EventQuestionAnswered && e.TaskID == "h1" {
			answered++
		}
	}
	if answered != 0 || slices.ContainsFunc(g.r.cfg.Workflows.(*fakeWorkflows).signals, func(s string) bool {
		return strings.HasPrefix(s, "h1 ")
	}) {
		t.Fatalf("%d answer rows, signals %q; want none for the human card", answered, g.r.cfg.Workflows.(*fakeWorkflows).signals)
	}
}

// The human's confirm_audit on the gate stop's case records the audit
// findings it names by their ids as audit_finding_confirmed steps, each
// the finding's whole text with who and why, and ends the case; a second
// one of a finding already recorded records nothing again. Of the newest
// audit's two findings, the one at no file is never recorded. The project
// agent's press, a press without a reason, one naming no finding, one
// naming an id no open finding has, one naming a finding by its text and
// one naming the finding at no file are refused, and write nothing.
func TestActConfirmAuditRecordsAPersonsConfirmations(t *testing.T) {
	g := newFactsRig(t)
	g.card()
	g.timeline(t)
	g.r.timeline = g.tl
	const rig = "secrets at acceptance/rig_test.go"
	g.tl.now = func() time.Time { return factsAt(9, 50) }
	if err := g.tl.Append(context.Background(), "t1", "audit", map[string]any{"commit": "h2", "verdict": "findings", "findings": []AuditFinding{
		{Item: "secrets", File: "acceptance/rig_test.go", Line: 118, Why: "a rig literal"},
		{Item: "... and 2 more", Why: "the card lists the first 50 of the parts' findings"}}}); err != nil {
		t.Fatal(err)
	}
	t1 := g.store.tasks["t1"]
	t1.IssueNumber = 0 // the answer's factoryd comment needs no issue here
	g.store.tasks["t1"] = t1
	g.store.cases = append(g.store.cases, Case{ID: 9, TaskID: "t1", Kind: CaseGateStop, Status: CaseAtHuman,
		Facts: map[string]any{"head": "h2", "gate": "GateCriteria"}})
	ctx := context.Background()
	steps := func() []map[string]any {
		b, _ := g.tl.Objects.Get(ctx, "sessions/t1/timeline.jsonl")
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil && m["step"] == stepAuditConfirmed {
				out = append(out, m)
			}
		}
		return out
	}
	for _, tc := range []struct {
		name string
		in   ActInput
		want string // a refusal's prefix, "" for a confirmation
	}{
		{"without a reason", ActInput{TaskID: "t1", CaseID: 9, By: "sebastian", Rung: 3,
			Res: Resolution{Action: ActConfirmAudit}}, "a one-line reason is required"},
		{"by the project agent", ActInput{TaskID: "t1", CaseID: 9, By: ChatUser, Rung: 2,
			Res: Resolution{Action: ActConfirmAudit, Note: "fixtures"}}, "the factory agent may not pick `confirm_audit`: the human may, and the factory agent may take `route_approval`, `reverify`, `extend`, `raise_ceiling`, `successor`, `replan`, `close` and `note`"},
		{"naming an unknown finding", ActInput{TaskID: "t1", CaseID: 9, By: "sebastian", Rung: 3,
			Res: Resolution{Action: ActConfirmAudit, Note: "fixtures",
				Args: map[string]any{"findings": FindingID("secrets at acceptance/rig_test.go:118")}}},
			"finding " + FindingID("secrets at acceptance/rig_test.go:118") + " is not open on the head's newest audit"},
		{"naming a finding by its text", ActInput{TaskID: "t1", CaseID: 9, By: "sebastian", Rung: 3,
			Res: Resolution{Action: ActConfirmAudit, Note: "fixtures", Args: map[string]any{"findings": rig}}},
			"finding " + rig + " is not open on the head's newest audit"},
		{"naming no finding", ActInput{TaskID: "t1", CaseID: 9, By: "sebastian", Rung: 3,
			Res: Resolution{Action: ActConfirmAudit, Note: "fixtures"}}, "no finding is ticked: tick at least one finding to clear"},
		{"naming the finding at no file", ActInput{TaskID: "t1", CaseID: 9, By: "sebastian", Rung: 3,
			Res: Resolution{Action: ActConfirmAudit, Note: "fixtures", Args: map[string]any{"findings": FindingID("... and 2 more")}}},
			"finding " + FindingID("... and 2 more") + " names no file, which no confirmation covers"},
	} {
		var refused ErrActRefused
		if _, err := g.r.Act(ctx, tc.in); !errors.As(err, &refused) || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("%s = %v, want the refusal %q", tc.name, err, tc.want)
		}
	}
	if got := steps(); len(got) != 0 {
		t.Fatalf("the refusals wrote %v, want nothing", got)
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: 9, By: "sebastian", Rung: 3,
		Res: Resolution{Action: ActConfirmAudit, Note: "the rig's fakes, not secrets", Args: map[string]any{"findings": FindingID(rig)}}}); err != nil {
		t.Fatal(err)
	}
	got := steps()
	if len(got) != 1 || got[0]["finding"] != rig || got[0]["by"] != "sebastian" || got[0]["why"] != "the rig's fakes, not secrets" {
		t.Fatalf("confirmations = %v, want %q confirmed by sebastian with the reason", got, rig)
	}
	if c, err := g.r.store.CaseByID(ctx, 9); err != nil || c.Status != CaseResolved {
		t.Fatalf("the case is %s (%v), want it resolved", c.Status, err)
	}
	// a finding already recorded: the case answers, nothing records again
	g.store.cases = append(g.store.cases, Case{ID: 10, TaskID: "t1", Kind: CaseGateStop, Status: CaseAtHuman,
		Facts: map[string]any{"head": "h2", "gate": "GateCriteria"}})
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: 10, By: "sebastian", Rung: 3,
		Res: Resolution{Action: ActConfirmAudit, Note: "again", Args: map[string]any{"findings": FindingID(rig)}}}); err != nil {
		t.Fatal(err)
	}
	if got := steps(); len(got) != 1 {
		t.Fatalf("confirmations = %v, want the one standing, the second press recording nothing again", got)
	}
}

// The card's box, confirm_audit and the gate read one finding the same
// way, by its id (FindingID): two findings of the head's audit that differ
// only in letter case have one id and one box, and one press confirms
// both. It records one confirmation, under the text the card shows, and
// the gate covers each of the two.
func TestActConfirmAuditCoversFindingsOfOneID(t *testing.T) {
	g := newFactsRig(t)
	g.card()
	g.timeline(t)
	g.r.timeline = g.tl
	lower := AuditFinding{Item: "secrets", File: "acceptance/rig_test.go", Line: 118, Why: "a rig literal"}
	upper := AuditFinding{Item: "secrets", File: "Acceptance/Rig_test.go", Line: 12, Why: "a rig literal"}
	ctx := context.Background()
	g.tl.now = func() time.Time { return factsAt(9, 50) }
	if err := g.tl.Append(ctx, "t1", "audit", map[string]any{"commit": "h2", "verdict": "findings", "findings": []AuditFinding{lower, upper}}); err != nil {
		t.Fatal(err)
	}
	t1 := g.store.tasks["t1"]
	t1.IssueNumber = 0
	g.store.tasks["t1"] = t1
	g.store.cases = append(g.store.cases, Case{ID: 9, TaskID: "t1", Kind: CaseGateStop, Status: CaseAtHuman,
		Facts: map[string]any{"head": "h2", "gate": "GateCriteria"}})
	covered := func() (bool, bool, []string) {
		confirmed, err := g.r.auditConfirmations(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		return auditConfirmedOf(lower, confirmed), auditConfirmedOf(upper, confirmed), slices.Sorted(maps.Keys(confirmed))
	}
	id := FindingID(lower.Text())
	if id != FindingID(upper.Text()) || lower.Text() == upper.Text() {
		t.Fatalf("the texts %q and %q have the ids %s and %s, want two texts of one id", lower.Text(), upper.Text(), id, FindingID(upper.Text()))
	}
	if a, b, texts := covered(); a || b || len(texts) != 0 {
		t.Fatalf("before the press the gate covers %t and %t by %v, want neither", a, b, texts)
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: 9, By: "sebastian", Rung: 3,
		Res: Resolution{Action: ActConfirmAudit, Note: "the rig's fakes, not secrets", Args: map[string]any{"findings": id}}}); err != nil {
		t.Fatal(err)
	}
	if a, b, texts := covered(); !a || !b || !slices.Equal(texts, []string{"secrets at acceptance/rig_test.go"}) {
		t.Fatalf("after the press the gate covers %t and %t by %v, want both by the one text the card shows", a, b, texts)
	}
}

// A confirmation clears exactly the findings ticked, by their ids: a
// finding whose token comes first, and one of two findings that differ
// only in their token, each records its own whole text and no other.
func TestActConfirmAuditClearsExactlyTheTickedFindings(t *testing.T) {
	g := newFactsRig(t)
	g.card()
	g.r.timeline = g.tl
	t1 := g.store.tasks["t1"]
	t1.IssueNumber = 0
	g.store.tasks["t1"] = t1
	const file = "acceptance/rig_test.go"
	first := "ghp_" + strings.Repeat("a", 36) + " sits in the rig"
	tokenA, tokenB := "the client sends ghp_"+strings.Repeat("b", 36), "the client sends ghp_"+strings.Repeat("c", 36)
	ctx := context.Background()
	if err := g.tl.Append(ctx, "t1", "audit", map[string]any{"commit": "h2", "verdict": "findings",
		"findings": []map[string]any{{"item": first, "file": file}, {"item": tokenA, "file": file}, {"item": tokenB, "file": file}}}); err != nil {
		t.Fatal(err)
	}
	confirmed := func() []string {
		b, _ := g.tl.Objects.Get(ctx, "sessions/t1/timeline.jsonl")
		var out []string
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil && m["step"] == stepAuditConfirmed {
				out = append(out, m["finding"].(string))
			}
		}
		return out
	}
	// a finding's text is its item at its file
	first, tokenB = first+" at "+file, tokenB+" at "+file
	for i, ticked := range []string{tokenB, first} {
		id := int64(20 + i)
		g.store.cases = append(g.store.cases, Case{ID: id, TaskID: "t1", Kind: CaseGateStop, Status: CaseAtHuman, Facts: map[string]any{"head": "h2"}})
		if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: id, By: "sebastian", Rung: 3,
			Res: Resolution{Action: ActConfirmAudit, Note: "the rig's fakes", Args: map[string]any{"findings": FindingID(ticked)}}}); err != nil {
			t.Fatal(err)
		}
	}
	if got := confirmed(); !slices.Equal(got, []string{tokenB, first}) {
		t.Fatalf("confirmations = %q, want exactly the two ticked findings' texts", got)
	}
}

// A rung's recommendation on an agent's question lives in the case's
// steps, and the face follows it through a cap: asked with room, the
// rung's switch of the model leads with its grounds; the session cap binds
// and the pass stores the cap's face, which cannot lead it; the cap lifts,
// and the pass stores the question with the rung's pick and its grounds
// leading again.
func TestAQuestionKeepsItsRecommendationThroughACap(t *testing.T) {
	g := newFactsRig(t)
	ctx := context.Background()
	q := Case{ID: 7, TaskID: "t1", Kind: CaseAgentEscalate, Status: CaseAtHuman,
		Facts: map[string]any{"trip": "trip-7", "reason": AgentAsks + "Which store keeps the snapshots?\nWhy: two fit\nRecommendation: SQLite", "choices": []any{"Postgres", "SQLite"}}}
	g.store.cases = append(g.store.cases, q)
	g.store.caseSteps = map[int64][]CaseStep{7: {{CaseID: 7, Rung: 2, Actor: ChatUser, Outcome: StepHandedUp, Decision: ActHandover,
		Grounds: "a stronger model reads the schema", Did: []CaseAction{{Action: ActHandover, Args: map[string]any{"model": "stronger"}, Result: resultRecommended}}}}}
	card := g.store.tasks["t1"]
	card.SpentEUR, card.HeldEUR = 1, 0
	g.store.tasks["t1"] = card
	f, err := g.r.FactsAtCase(ctx, card, q)
	if err != nil {
		t.Fatal(err)
	}
	limit := f.SessionCap
	set := func(attempts int) {
		card := g.store.tasks["t1"]
		card.Usage.Attempts = attempts
		g.store.tasks["t1"] = card
	}
	row := func() map[string]any {
		events, _ := g.store.TaskEventsOfKind(ctx, "t1", EventQuestionAsked, 1)
		return events[0].Detail
	}
	set(limit - 1)
	f, _ = g.r.FactsAtCase(ctx, g.store.tasks["t1"], q)
	fence := func(s string) string { return g.r.untrusted(card, s) }
	g.event("t1", factsNow, EventQuestionAsked, FaceRow(q, g.store.tasks["t1"], f, "", HandedUpBy(g.store.caseSteps[7], 2, ""), fence, g.r.scrub))
	row()["case_id"] = q.ID
	want := Recommendation{Action: ActHandover, By: "the factory agent", Grounds: "a stronger model reads the schema"}
	if got := RecommendationOf(row()); got != want {
		t.Fatalf("asked with room, the row recommends %+v, want %+v", got, want)
	}
	set(limit)
	g.r.StoreMissedOffers(ctx)
	if got := row()["ask"]; got != fmt.Sprintf("The card used all %d sessions. How should it go on?", limit) {
		t.Fatalf("the cap bound: the row asks %v, want the cap's ask", got)
	}
	set(limit - 1)
	g.r.StoreMissedOffers(ctx)
	if got := RecommendationOf(row()); row()["ask"] != "Which store keeps the snapshots?" || got != want {
		t.Fatalf("the cap lifted: the row asks %v and recommends %+v, want the question with %+v", row()["ask"], got, want)
	}
}

// The pass stores a question's face only on facts read whole: with the
// card's events or its checks unread, a cap's face would lose the draw
// that lifts it, and nothing would repair it.
func TestAQuestionsFaceWaitsForWholeReads(t *testing.T) {
	f := CaseFacts{Kind: CaseAgentEscalate, Attempts: 3, SessionCap: 3, AgentTimeS: 300, AgentTimeCapS: 7200, SessionLeastS: 300, LeftEUR: 1, FloorEUR: 0.3}
	if !f.readWhole() {
		t.Error("facts read whole read as partial")
	}
	for _, partial := range []string{"events", "checks", "floor"} {
		g := f
		g.Partial = []string{partial}
		if g.readWhole() {
			t.Errorf("with %s unread the facts read as whole", partial)
		}
	}
	g := f
	g.Partial = []string{"siblings"}
	if !g.readWhole() {
		t.Error("an unread plan's siblings keep a question's face from its store")
	}
}

// The minute pass withdraws a card's asks a newer ask at the human
// replaced: an answer on the card ends its newest live ask only, so an
// older one at the human, or still on a rung, can never be answered. The
// newest ask and the asks answered by their own number (a merge, a
// monitor flag) stay. A newer ask still on a rung replaces nothing yet:
// settled there, it leaves the older one the card's live ask again.
func TestTheMinutePassWithdrawsAnAskANewerOneReplaced(t *testing.T) {
	g := newFactsRig(t)
	ctx := context.Background()
	g.store.tasks["t2"] = Task{ID: "t2", Kind: "agent", Repository: "demo-app/demo-app", Status: "waiting-human"}
	g.store.cases = append(g.store.cases,
		Case{ID: 5, TaskID: "t1", Kind: CaseSessionCap, Status: CaseOpen},
		Case{ID: 7, TaskID: "t1", Kind: CaseAgentEscalate, Status: CaseAtHuman, Facts: map[string]any{"reason": AgentAsks + "Which store keeps the snapshots?"}},
		Case{ID: 8, TaskID: "t1", Kind: CaseMonitorFlag, Status: CaseAtHuman},
		Case{ID: 9, TaskID: "t1", Kind: CaseMergeApproval, Status: CaseAtHuman},
		Case{ID: 11, TaskID: "t1", Kind: CaseBudgetFloor, Status: CaseAtHuman},
		Case{ID: 20, TaskID: "t2", Kind: CaseAgentEscalate, Status: CaseAtHuman, Facts: map[string]any{"reason": AgentAsks + "Which store keeps the snapshots?"}},
		Case{ID: 21, TaskID: "t2", Kind: CaseSessionCap, Status: CaseOpen})
	g.r.StoreMissedOffers(ctx)
	status := map[int64]string{}
	for _, c := range g.store.cases {
		status[c.ID] = c.Status
	}
	if want := map[int64]string{5: CaseWithdrawn, 7: CaseWithdrawn, 8: CaseAtHuman, 9: CaseAtHuman, 11: CaseAtHuman, 20: CaseAtHuman, 21: CaseOpen}; !maps.Equal(status, want) {
		t.Errorf("after the pass the cases stand %v, want %v", status, want)
	}
}

// A withdrawn question leaves its card waiting on the human only for
// another ask an answer on the card ends: a monitor flag beside it puts
// no card in waiting-human, so the card goes back to review.
func TestAWithdrawnQuestionBesideAFlagMovesItsCard(t *testing.T) {
	g := newFactsRig(t)
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":501}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(forge.Close)
	g.r.forgejo = &Forgejo{BaseURL: forge.URL, BotUser: "factory-agent", BotPassword: "pw"}
	g.store.cases = append(g.store.cases,
		Case{ID: 7, TaskID: "t1", Kind: CaseAgentEscalate, Status: CaseAtHuman, CommentID: 300, Facts: map[string]any{"reason": AgentAsks + "Which store keeps the snapshots?"}},
		Case{ID: 8, TaskID: "t1", Kind: CaseMonitorFlag, Status: CaseAtHuman})
	if err := g.r.EndCase(context.Background(), 7, "the run left the wait"); err != nil {
		t.Fatal(err)
	}
	if got := g.store.tasks["t1"].Status; got != "in-review" {
		t.Errorf("the card is %s beside a live flag, want in-review", got)
	}
}

// A replaced ask whose face reached the human withdraws without moving its
// card: the card still waits on the human with the newer ask and its
// answers, and the issue says the older question needs no answer. A case
// whose answer lock is held is Act's to end, and the pass leaves it.
func TestTheMinutePassWithdrawsAReplacedAskAndTheCardStillWaits(t *testing.T) {
	g := newFactsRig(t)
	ctx := context.Background()
	var calls []string
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/demo-app/demo-app/commits/h2/status":
			fmt.Fprint(w, `{"state":"success","statuses":[{"status":"success","context":"ci / test"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/demo-app/demo-app/issues/12/comments":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":501}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(forge.Close)
	g.r.forgejo = &Forgejo{BaseURL: forge.URL, BotUser: "factory-agent", BotPassword: "pw"}
	g.store.cases = append(g.store.cases,
		Case{ID: 7, TaskID: "t1", Kind: CaseAgentEscalate, Status: CaseAtHuman, CommentID: 300, Facts: map[string]any{"reason": AgentAsks + "Which store keeps the snapshots?"}},
		Case{ID: 9, TaskID: "t1", Kind: CaseSessionCap, Status: CaseAtHuman, CommentID: 305},
		Case{ID: 11, TaskID: "t1", Kind: CaseBudgetFloor, Status: CaseAtHuman, CommentID: 310})
	for _, id := range []int64{7, 9, 11} {
		g.event("t1", factsNow, EventQuestionAsked, map[string]any{"case_id": id, "comment": 300 + id})
	}
	unlock, ok := g.r.TryLockCase(9)
	if !ok {
		t.Fatal("case 9's lock is held")
	}
	g.r.StoreMissedOffers(ctx)
	unlock()
	status := map[int64]string{}
	for _, c := range g.store.cases {
		status[c.ID] = c.Status
	}
	if want := map[int64]string{7: CaseWithdrawn, 9: CaseAtHuman, 11: CaseAtHuman}; !maps.Equal(status, want) {
		t.Errorf("after the pass the cases stand %v, want %v", status, want)
	}
	if got := g.store.tasks["t1"].Status; got != "waiting-human" {
		t.Errorf("after the pass the card is %s, want waiting-human with its newer ask", got)
	}
	var labels []string
	for _, o := range g.store.faces[11]["offers"].([]AskOffer) {
		labels = append(labels, o.Label)
	}
	// t1's budget of 5 EUR is the policy's ceiling (task_max_eur 5), so the
	// floor asks to raise the limit, not to add money under it
	if want := []string{"Raise the spend limit", "Start a fresh card on this work", "End the card", "Not now"}; !slices.Equal(labels, want) {
		t.Errorf("the newer ask's answers are %q, want %q", labels, want)
	}
	if !slices.Contains(calls, "POST /api/v1/repos/demo-app/demo-app/issues/12/comments") || slices.ContainsFunc(calls, func(c string) bool { return strings.Contains(c, "/labels") }) {
		t.Errorf("the forge saw %q, want the withdrawal's comment and no state label", calls)
	}
}
