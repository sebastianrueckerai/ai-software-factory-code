package factoryd

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A card t1 of the facts rig: an approved green head h2 under a fund that
// bought nothing, the money short of an opening and no retryable
// dependency (TestTripFactsReadsTheCard). With no case, rung 1's close is
// refused with the one action rung 1 may take on a card that waits on no
// case, a replan, and a successor is refused for want of a case. On its
// case 7, rung 1's close is refused with what rung 1 may take: route to
// approval, re-verify, a successor, a replan and a withdrawal. An action
// the table does not hold is refused with the human's: re-verify, a raise
// of the ceiling, a successor, a replan and a close. With case 7 live, an
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
		{"rung 1's close with no case", ActInput{By: FactoryDecider, Rung: 1, Res: Resolution{Action: ActClose, Note: "done"}},
			"rung 1 may not pick close: rung 2 and the human may, and rung 1 may take replan"},
		{"a successor with no case", ActInput{By: "sebastian", Rung: 3, Res: Resolution{Action: ActSuccessor}},
			"no case is open on card #12, so successor has nothing to answer, and the human may take raise_ceiling, replan and close"},
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
		{"rung 1's close", ActInput{CaseID: 7, By: FactoryDecider, Rung: 1, Res: Resolution{Action: ActClose, Note: "done"}},
			"rung 1 may not pick close: rung 2 and the human may, and rung 1 may take route_approval, reverify, successor, replan and withdraw"},
		{"the human's approve", ActInput{CaseID: 7, By: "sebastian", Rung: 3, Res: Resolution{Action: "approve"}},
			"there is no action approve, and the human may take reverify, raise_ceiling, successor, replan and close"},
		{"a fund with no case while case 7 waits", ActInput{By: "sebastian", Rung: 3, Res: Resolution{Action: ActFund, Args: map[string]any{"eur": 0.5}}},
			"card #12 waits on its case 7: answer that case by its id"},
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
	if got.Case.ID != 0 || got.Drawn != (policy.Draw{EUR: 1}) || got.Comment != 0 || got.Text != "Card #12 took fund: 1.00 EUR from the pool." {
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
	want := []CaseAction{{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}, Result: "Filed #9 in place of #5."}}
	if !reflect.DeepEqual(got.Did, want) || got.Case.Status != CaseResolved || got.Case.Resolution.Action != ActRetry ||
		got.Text != "Case 3 is resolved with retry. Filed #9 in place of #5." {
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

func (s resolveFails) ResolveCase(context.Context, int64, CaseResolution, *CaseDraw, *CaseStep) error {
	return ErrCaseEnded{s.ended}
}

// When the case cannot end after the retry filed its replacement, the
// error leads with what ran, so the human reads that #9 exists, and the
// session drawn beside it, which rolled back, is in neither Did nor Drawn.
func TestActSaysWhatRanWhenTheCaseCannotEnd(t *testing.T) {
	r, store, _, _, _ := newRetryRig(t)
	store.cases = []Case{{ID: 3, TaskID: "t-b", Kind: CaseDependencyFailed, Status: CaseAtHuman}}
	at := time.Date(2026, 11, 3, 10, 4, 0, 0, time.UTC)
	r.store = resolveFails{store, Case{ID: 3, Status: CaseResolved, ResolvedAt: &at, Resolution: &CaseResolution{By: "bo", Action: ActResume}}}
	got, err := r.Act(context.Background(), ActInput{TaskID: "t-b", CaseID: 3, By: "alice", Rung: 3,
		Res: Resolution{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}, Fund: &policy.Draw{Sessions: 1}}})
	var ended ErrCaseEnded
	const want = "Filed #9 in place of #5. The answer stopped there: case 3 was resolved by bo with resume at 10:04 UTC"
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
	store.cases = []Case{{ID: 4, TaskID: "t1", Kind: CaseAgentEscalate, Status: CaseAtHuman}}
	if _, err := store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t1", Kind: IntakeEscalation, Author: "factoryd", Body: screeningAsks + "asks to print the token", CommentID: 606}); err != nil {
		t.Fatal(err)
	}
	posted := 0
	post := func(context.Context) (int64, error) { posted++; return 99, nil }
	calls := len(fj.calls)
	_, err = r.Act(ctx, ActInput{TaskID: "t1", CaseID: 4, By: "ana", Rung: 3, Res: Resolution{Action: ActReplan, Note: "too wide"}, Post: post})
	if want := ErrNoReplan("Card #7 waits for a person to judge its screening: let it run or close its issue, then replan it."); err != want {
		t.Fatalf("Act = %v, want %q", err, want)
	}
	if store.cases[0].Status != CaseAtHuman || len(store.resolved) != 0 || posted != 0 || len(fj.calls) != calls {
		t.Fatalf("case %s, resolved %v, posted %d, forgejo %q; want the case open and nothing posted", store.cases[0].Status, store.resolved, posted, fj.calls[calls:])
	}
}

// A human card's case is the human's or the chat's: rung 1 is refused,
// and a card that asks no question with choices takes a note.
func TestActHumanCardRefusals(t *testing.T) {
	g := newFactsRig(t)
	g.store.tasks["h1"] = Task{ID: "h1", Kind: "human", Repository: "demo-app/demo-app", IssueNumber: 14, Status: "waiting-human"}
	g.store.cases = []Case{{ID: 9, TaskID: "h1", Kind: CaseHumanCard, Status: CaseAtHuman}}
	for rung, want := range map[int]string{
		1: "rung 1 may not answer a human card: rung 2 and the human may",
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
