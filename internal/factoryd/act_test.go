package factoryd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A raise of the ceiling moves no money into the budget, so the bought-
// nothing rule and the reads it is weighed on refuse an extend, a fund and
// a draw beside the raise, never the raise alone.
func TestRaiseCeilingIsNoDrawThatBuys(t *testing.T) {
	at := time.Date(2026, 11, 3, 10, 4, 0, 0, time.UTC)
	stuck := CaseFacts{Head: "h1", Checks: "red", Findings: []string{idA}, LastDrawAt: &at, DrawHead: "h1", DrawChecks: "red",
		DrawFindings: []string{idA}, PoolLeft: policy.Draw{Sessions: 5, AgentTime: 5 * time.Hour, EUR: 20}, BudgetEUR: 5, CeilingEUR: 6}
	raise, _ := ActionByName(ActRaiseCeiling)
	byTwo := Resolution{Action: ActRaiseCeiling, Args: map[string]any{"eur": 2.0}}
	beside := byTwo
	beside.Fund = &policy.Draw{EUR: 1}
	const refused = "the last draw at 10:04 UTC bought nothing, with no push since and the head, the checks and the findings unchanged: " +
		"hand up, or hand over, start a successor, replan or close"
	if got := raise.refusal(stuck, 3, byTwo); got != "" {
		t.Errorf("the human's raise after a draw that bought nothing = %q, want none", got)
	}
	if got := raise.refusal(stuck, 3, beside); got != refused {
		t.Errorf("a fund beside the raise = %q, want %q", got, refused)
	}
	blind := stuck
	blind.LastDrawAt, blind.Partial = nil, []string{"events"}
	if got := raise.refusal(blind, 2, byTwo); got != "" {
		t.Errorf("rung 2's raise with the events unread = %q, want none", got)
	}
	if got, want := raise.refusal(blind, 2, beside), "the card's events could not be read, so a draw cannot be weighed against the last one: hand up"; got != want {
		t.Errorf("a fund beside the raise with the events unread = %q, want %q", got, want)
	}
}

// An unread timeline leaves the findings unknown: a draw is weighed on the
// head and the checks alone, never taken as one whose findings went.
func TestBoughtNothingWeighsHeadAndChecksWhenTheTimelineIsUnread(t *testing.T) {
	at := time.Date(2026, 11, 3, 10, 4, 0, 0, time.UTC)
	f := CaseFacts{Head: "h1", Checks: "red", LastDrawAt: &at, DrawHead: "h1", DrawChecks: "red", DrawFindings: []string{idA}}
	if boughtNothing(f) {
		t.Error("the timeline read with no findings against idA at the draw: the draw bought something")
	}
	f.Partial = []string{"timeline"}
	if !boughtNothing(f) {
		t.Error("the timeline unread, head and checks as at the draw: want bought nothing")
	}
	f.Checks = "green"
	if boughtNothing(f) {
		t.Error("the timeline unread, the checks turned green: the draw bought something")
	}
}

// A card t1 of the facts rig: an approved green head h2 under a fund that
// bought nothing, the money short of an opening and no retryable
// dependency (TestTripFactsReadsTheCard). Rung 1's close is refused with
// what rung 1 may take: route to approval, re-verify, a successor, a replan
// and a withdrawal. An action the table does not hold is refused with the
// human's: re-verify, a raise of the ceiling, a successor, a replan and a
// close. Nothing is written.
func TestActRefusesAnActionTheTableDoesNotOffer(t *testing.T) {
	g := newFactsRig(t)
	g.card()
	g.timeline(t)
	events := len(g.store.events)
	ctx := context.Background()
	var refused ErrActRefused
	_, err := g.r.Act(ctx, ActInput{TaskID: "t1", By: FactoryDecider, Rung: 1, Res: Resolution{Action: ActClose, Note: "done"}})
	want := "rung 1 may not pick close: rung 2 and the human may, and rung 1 may take route_approval, reverify, successor, replan and withdraw"
	if !errors.As(err, &refused) || err.Error() != want {
		t.Fatalf("rung 1's close = %v, want the refusal %q", err, want)
	}
	_, err = g.r.Act(ctx, ActInput{TaskID: "t1", By: "sebastian", Rung: 3, Res: Resolution{Action: "approve"}})
	want = "there is no action approve, and the human may take reverify, raise_ceiling, successor, replan and close"
	if !errors.As(err, &refused) || err.Error() != want {
		t.Fatalf("the human's approve = %v, want the refusal %q", err, want)
	}
	_, err = g.r.Act(ctx, ActInput{TaskID: "t1", By: "sebastian", Rung: 3, Res: Resolution{Action: ActSuccessor}})
	want = "successor answers a case and none is named: answer the card's open case"
	if !errors.As(err, &refused) || err.Error() != want {
		t.Fatalf("a successor with no case = %v, want the refusal %q", err, want)
	}
	if len(g.store.events) != events || g.store.tasks["t1"].BudgetEUR != 5 {
		t.Fatalf("a refused act wrote %d events and left the budget at %v, want none and 5", len(g.store.events)-events, g.store.tasks["t1"].BudgetEUR)
	}
}

// With no case, a fund of 1 EUR on a running card raises its budget from
// 3.50 to 4.50 EUR under the ceiling of 5 and writes the card_funded row as
// a pool draw of rung 3 with the head, checks and findings it was weighed
// on. No case, no answer row, no comment and no signal.
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
	if got.Case.ID != 0 || got.Drawn != (policy.Draw{EUR: 1}) || got.Comment != 0 {
		t.Fatalf("result = %+v, want no case, 1 EUR drawn and no comment", got)
	}
	if b := g.store.tasks["t1"].BudgetEUR; b != 4.5 {
		t.Fatalf("budget = %v, want 4.5", b)
	}
	if len(g.store.events) != 1 {
		t.Fatalf("events = %+v, want the one card_funded row", g.store.events)
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
