package factoryd

import (
	"context"
	"errors"
	"maps"
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
// a raise of the ceiling, a successor, a replan, a close and a note. With case 7 live, an
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
		{"rung 1's close", ActInput{CaseID: 7, By: "factory-checkin", Rung: 1, Res: Resolution{Action: ActClose, Note: "done"}},
			"rung 1 may not pick close: rung 2 and the human may, and rung 1 may take route_approval, reverify, successor and replan"},
		{"the human's approve", ActInput{CaseID: 7, By: "sebastian", Rung: 3, Res: Resolution{Action: "approve"}},
			"there is no action approve, and the human may take reverify, raise_ceiling, successor, replan, close and note"},
		{"a fund with no case while case 7 waits", ActInput{By: "sebastian", Rung: 3, Res: Resolution{Action: ActFund, Args: map[string]any{"eur": 0.5}}},
			"card #12 waits on the session cap: answer that case, not the card"},
		{"a fund with no case via the chat while case 7 waits", ActInput{By: "sebastian", Via: ViaChat, Rung: 3, Res: Resolution{Action: ActFund, Args: map[string]any{"eur": 0.5}}},
			"card #12 waits on the session cap (case 7): answer that case, not the card"},
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
	want := []CaseAction{{Action: ActRetry, Args: map[string]any{"task_id": "t-a", "replacement": int64(9)}, Result: "Filed #9 in place of #5."}}
	if !reflect.DeepEqual(got.Did, want) || got.Case.Status != CaseResolved || got.Case.Resolution.Action != ActRetry ||
		got.Text != "The failed dependency on #6 is resolved with retry. Filed #9 in place of #5." {
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
		{ActInput{By: "sebastian", Rung: 0}, "rung 0 is factoryd's own: answer on rung 1, 2 or 3"},
		{ActInput{By: FactoryDecider, Rung: 2}, "rung 0 is factoryd's own, and factoryd answers on rung 0 alone: answer on rung 0, not rung 2"},
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
// refused with the stop's own words before it draws — no row, no raise, the
// case open — so the stop's next answer is not already weighed as a draw
// that bought nothing. The same extend with its minutes of agent time
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
		!strings.HasPrefix(err.Error(), "this card stopped for agent time: an extend of its agent time starts the next session; sessions, lines or check-in shares alone buy none") {
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
	if err := g.r.RecordGrant(ctx, "t1", "sebastian", policy.Draw{Sessions: 1}, identityKey(EventCardExtended, "run-1", "3")); err != nil {
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
	if err := g.r.AuditDraw(ctx, "t1", policy.Draw{Sessions: 1}, identityKey(EventCardExtended, "run-1", "1", "0s")); err != nil {
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
	if err := g.r.AuditDraw(ctx, "t1", policy.Draw{Sessions: 99}, identityKey(EventCardExtended, "run-1", "2", "0s")); !errors.As(err, &app) || app.Type() != errDrawRefused {
		t.Fatalf("past the bound = %v, want a typed draw-refused", err)
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
		Resolution: &CaseResolution{By: "sebastian", Rung: 3, Action: caseAnswer, Args: map[string]any{"answer": "go on", "comment": 77}}}}
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
		why, err := r.humanChoice(context.Background(), task, ActInput{Rung: 3, Res: res})
		if err != nil || !strings.Contains(why, "card #12 is a human card") {
			t.Errorf("%s: refusal %q, %v; want a human card's refusal", name, why, err)
		}
	}
}
