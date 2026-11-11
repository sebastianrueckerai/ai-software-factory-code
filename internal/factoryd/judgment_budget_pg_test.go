package factoryd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const unjudgedSuffix = " (not judged, judgment budget spent)"

// askOf and infoOf are the question the face asked on case id.
func (g rungRig) askOf(t *testing.T, id int64) string { return g.asked(t, id, "ask") }

func (g rungRig) infoOf(t *testing.T, id int64) string { return g.asked(t, id, "info") }

func (g rungRig) asked(t *testing.T, id int64, key string) string {
	t.Helper()
	for _, e := range g.asks(t) {
		if n, _ := toInt(e.Detail["case_id"]); int64(n) == id {
			s, _ := e.Detail[key].(string)
			return s
		}
	}
	t.Fatalf("no question asked on case %d", id)
	return ""
}

// spendChat spends the whole month budget of the chat task of demo-app.
func (g rungRig) spendChat(t *testing.T) Task {
	t.Helper()
	ctx := context.Background()
	chat, err := g.r.judgmentTask(ctx, Task{Repository: "demo-app/demo-app"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET spent_eur = budget_eur WHERE id = $1`, chat.ID); err != nil {
		t.Fatal(err)
	}
	chat.SpentEUR = chat.BudgetEUR
	return chat
}

// A chat task with 0.00 of 10.00 EUR left (the default budget): a session
// cap that rung 0 does not resolve goes to the human on card t1 and on
// card t2, no conversation runs, each problem line ends with the mark, and
// the project has one chat_budget card and one judgment_budget case.
func TestSpentJudgmentBudgetFacesTheHumanUnjudged(t *testing.T) {
	g := newRungRig(t, "success")
	g.r.store = projectStore{g.s}
	g.r.cfg.Prices = nil
	cardForge, _ := newAlertForgejo(t)
	g.r.ChatBudget = &ChatBudgetCards{Store: projectStore{g.s}, Forgejo: cardForge, Timeline: g.r.timeline}
	ctx := context.Background()
	chat := g.spendChat(t)
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET status = 'running' WHERE id = $1`, chat.ID); err != nil {
		t.Fatal(err)
	}
	if err := g.s.CreateTask(ctx, Task{ID: "t2", Kind: "agent", Status: "running", Objective: "y", Repository: "demo-app/demo-app",
		Track: policy.TrackDirect, Size: "medium", BudgetEUR: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET required_capabilities = '["coder"]'::jsonb, size = 'medium' WHERE id = 't2'`); err != nil {
		t.Fatal(err)
	}
	a := g.trip(t, tripCap)
	b, err := g.r.OpenTrip(ctx, TripInput{TaskID: "t2", Reason: tripCap}, tripAttempt{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	g.settle()
	if g.chat.calls != 0 {
		t.Fatalf("%d conversations, want none", g.chat.calls)
	}
	for _, out := range []TripOutcome{a, b} {
		c := caseByID(t, g.s, out.CaseID)
		if out.Rung != 3 || c.Status != CaseAtHuman || !strings.HasSuffix(c.Problem, unjudgedSuffix) || strings.Count(c.Problem, unjudgedSuffix) != 1 {
			t.Errorf("outcome %+v, case %d %s %q; want at the human with one mark", out, c.ID, c.Status, c.Problem)
		}
		if out.CaseID == a.CaseID {
			if !strings.HasSuffix(g.askOf(t, c.ID), unjudgedSuffix) {
				t.Errorf("the ask of case %d does not end with the mark: %q", c.ID, g.askOf(t, c.ID))
			}
			if info := g.infoOf(t, c.ID); !strings.Contains(info, "The factory found: not judged, judgment budget spent") || strings.Contains(info, "The check-in found") {
				t.Errorf("the info of case %d = %q, want it led by the factory", c.ID, info)
			}
		}
		steps := g.steps(t, c.ID)
		want := fmt.Sprintf("not judged, judgment budget spent: the chat task %s has 0.00 of 10.00 EUR left", chat.ID)
		if !slices.ContainsFunc(steps, func(s CaseStep) bool {
			return s.Rung == 0 && s.Actor == FactoryDecider && s.Found == want && s.Outcome == StepHandedUp
		}) {
			t.Errorf("steps %+v, want rung 0 by the factory finding %q", steps, want)
		}
	}
	var cards, cases int
	if err := g.s.db.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE source = $1 AND parent_task = $2`, SourceChatBudget, chat.ID).Scan(&cards); err != nil {
		t.Fatal(err)
	}
	if err := g.s.db.QueryRowContext(ctx, `SELECT count(*) FROM cases WHERE kind = 'judgment_budget'`).Scan(&cases); err != nil {
		t.Fatal(err)
	}
	if cards != 1 || cases != 1 {
		t.Fatalf("%d chat_budget cards and %d judgment_budget cases, want one of each", cards, cases)
	}
}

// Six rung 1 steps on the card's cases and no rung1_runs key: the next trip
// runs no rung 1 conversation, and the count does not bound rung 2: the case
// goes to rung 2 with rung 0's step naming the count, no comment, one
// case_climbed row; the face follows rung 2's bound.
func TestRung1PerCardCountSendsTheCaseToRung2(t *testing.T) {
	g := newRungRig(t, "success")
	ctx := context.Background()
	for i := range 2 {
		c, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseOther, StateKey: fmt.Sprint("k", i), Status: CaseAtHuman, Problem: "old"})
		if err != nil {
			t.Fatal(err)
		}
		for range 3 {
			now := time.Now()
			if _, err := g.s.AppendCaseStep(ctx, CaseStep{CaseID: c.ID, Rung: 1, Actor: CheckinDecider, Outcome: StepHandedUp, EndedAt: &now}); err != nil {
				t.Fatal(err)
			}
		}
	}
	out := g.trip(t, tripCap)
	g.settle()
	if out.Rung != 2 || g.chat.calls != 0 || len(g.forge.comments) != 0 || len(g.rows(t, EventCaseClimbed)) != 1 {
		t.Fatalf("outcome %+v, %d conversations, %d comments; want rung 2 and no conversation of rung 1", out, g.chat.calls, len(g.forge.comments))
	}
	if !slices.ContainsFunc(g.steps(t, out.CaseID), func(s CaseStep) bool {
		return s.Rung == 0 && s.Found == "rung 1 reached its per-card count of 6 on this card"
	}) || len(g.stepsOfRung(t, out.CaseID, 2)) != 1 {
		t.Fatalf("steps %+v, want rung 0's step naming the count and rung 2's", g.steps(t, out.CaseID))
	}
	g.afterRung2(t, out.CaseID)
	if c := caseByID(t, g.s, out.CaseID); c.Status != CaseAtHuman || len(g.forge.comments) != 1 {
		t.Fatalf("case %s, %d comments; want the face after rung 2's bound", c.Status, len(g.forge.comments))
	}
}

// With rung1_runs 7 the same six steps leave room: the seventh runs.
func TestRung1PerCardCountFollowsPolicy(t *testing.T) {
	g := newRungRig(t, "success")
	v := g.r.cfg.Policy()
	v.Document.Escalation.Rung1Runs = 7
	g.r.cfg.Policy = func() policy.Version { return v }
	ctx := context.Background()
	c, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseOther, StateKey: "k", Status: CaseAtHuman, Problem: "old"})
	if err != nil {
		t.Fatal(err)
	}
	for range 6 {
		now := time.Now()
		if _, err := g.s.AppendCaseStep(ctx, CaseStep{CaseID: c.ID, Rung: 1, Actor: CheckinDecider, Outcome: StepHandedUp, EndedAt: &now}); err != nil {
			t.Fatal(err)
		}
	}
	g.chat.answers = []CheckinResponse{g.handUp(t)}
	out := g.trip(t, tripCap)
	g.settle()
	if g.chat.calls != 1 || out.Rung != 1 {
		t.Fatalf("outcome %+v, %d conversations; want the seventh to run", out, g.chat.calls)
	}
}

// The chat task's left money below the cheapest call on the check-in
// alias is spent: with big at 1.5e-5 EUR per output token and the minimum
// call of 1024 tokens a call needs 0.01536 EUR. 0.01 left is spent, 0.02
// left is not.
func TestJudgmentBudgetSpentBelowOneCall(t *testing.T) {
	for left, spent := range map[float64]bool{0.01: true, 0.02: false} {
		g := newRungRig(t, "success")
		g.chat.answers = []CheckinResponse{g.handUp(t)}
		chat := g.spendChat(t)
		if _, err := g.s.db.ExecContext(context.Background(), `UPDATE tasks SET spent_eur = budget_eur - $2 WHERE id = $1`, chat.ID, left); err != nil {
			t.Fatal(err)
		}
		out := g.trip(t, tripCap)
		g.settle()
		if got := out.Rung == 3 && g.chat.calls == 0; got != spent {
			t.Errorf("left %v: outcome %+v, %d conversations; spent = %v want %v", left, out, g.chat.calls, got, spent)
		}
	}
}

// A case rung 1 judged already, climbing at a spent budget, is not marked
// unjudged: its problem stays as it was, the face keeps rung 1's
// recommendation and adds the line that the factory agent did not judge it,
// and the one money case is filed.
func TestJudgedCaseIsNotMarkedUnjudged(t *testing.T) {
	g := newRungRig(t, "success")
	g.r.cfg.Prices = nil
	g.r.store = projectStore{g.s}
	cardForge, _ := newAlertForgejo(t)
	g.r.ChatBudget = &ChatBudgetCards{Store: projectStore{g.s}, Forgejo: cardForge, Timeline: g.r.timeline}
	ctx := context.Background()
	chat := g.spendChat(t)
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET status = 'running' WHERE id = $1`, chat.ID); err != nil {
		t.Fatal(err)
	}
	c, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseOther, StateKey: "k", Status: CaseOpen, Problem: "stuck"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := g.s.AppendCaseStep(ctx, CaseStep{CaseID: c.ID, Rung: 1, Actor: CheckinDecider, Outcome: StepHandedUp, Found: "the stop reads as gate_stop", Decision: "close", Grounds: "done on main", EndedAt: &now}); err != nil {
		t.Fatal(err)
	}
	task, _, err := g.r.task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.r.climb(ctx, task, c, CaseFacts{}, 1, "the stop reads as gate_stop"); err != nil {
		t.Fatal(err)
	}
	if got := caseByID(t, g.s, c.ID); got.Problem != "stuck" || got.Status != CaseAtHuman {
		t.Fatalf("case %s %q, want at the human, unmarked", got.Status, got.Problem)
	}
	var rec, info, ask string
	for _, e := range g.asks(t) {
		rec, _ = e.Detail["recommendation"].(string)
		info, _ = e.Detail["info"].(string)
		ask, _ = e.Detail["ask"].(string)
	}
	var money int
	if err := g.s.db.QueryRowContext(ctx, `SELECT count(*) FROM cases WHERE kind = 'judgment_budget'`).Scan(&money); err != nil {
		t.Fatal(err)
	}
	for _, st := range g.steps(t, c.ID) {
		if st.Rung == 0 && (strings.Contains(st.Found, "not judged") || !strings.HasPrefix(st.Found, noJudgmentLine)) && strings.Contains(st.Found, "chat task") {
			t.Fatalf("rung 0's step reads %q, want the missing rung worded, no \"not judged\"", st.Found)
		}
	}
	if !strings.Contains(rec, "recommends close: done on main") || !strings.Contains(info, "The check-in found: the stop reads as gate_stop") ||
		!strings.Contains(info, "The factory agent did not judge it: the judgment budget is spent.") || strings.Contains(ask, "not judged") || money != 1 || g.chat.calls != 0 {
		t.Fatalf("recommendation %q, info %q, ask %q, %d money cases, %d conversations; want rung 1's judgment with the missing line and one money case", rec, info, ask, money, g.chat.calls)
	}
}

// Many writers at one limit: the count and the step are one write, so a
// card gets exactly limit steps of rung 1 across its cases.
func TestRung1StepsStopAtTheLimitUnderRace(t *testing.T) {
	g := newRungRig(t, "success")
	ctx := context.Background()
	var cases []Case
	for i := range 4 {
		c, _, err := g.s.OpenCase(ctx, Case{TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseOther, StateKey: fmt.Sprint("k", i), Status: CaseOpen, Problem: "p"})
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, c)
	}
	var wg sync.WaitGroup
	var started atomic.Int32
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, atLimit, err := g.s.AppendRung1Step(ctx, "t1", 3, CaseStep{CaseID: cases[i%4].ID, Rung: 1, Actor: CheckinDecider})
			if err != nil {
				t.Error(err)
			}
			if !atLimit {
				started.Add(1)
			}
		}()
	}
	wg.Wait()
	if started.Load() != 3 {
		t.Fatalf("%d steps written, want 3", started.Load())
	}
}

// A card the filing of the money case fails for still faces its human; the
// failure is retried by the card loop from the recorded stop, and a second
// pass of the case adds no second step of rung 0.
func TestFailedMoneyCardDoesNotHoldTheFace(t *testing.T) {
	g := newRungRig(t, "success")
	g.r.cfg.Prices = nil
	g.r.store = projectStore{g.s}
	ctx := context.Background()
	dead := &Forgejo{BaseURL: "http://127.0.0.1:1", BotUser: "factory-agent", BotPassword: "pw"}
	g.r.ChatBudget = &ChatBudgetCards{Store: projectStore{g.s}, Forgejo: dead, Timeline: g.r.timeline}
	chat := g.spendChat(t)
	out := g.trip(t, tripCap)
	if out.Rung != 3 || out.Comment == 0 {
		t.Fatalf("outcome %+v, want the face", out)
	}
	task, _, _ := g.r.task(ctx, "t1")
	c := caseByID(t, g.s, out.CaseID)
	for range 2 {
		if _, err := g.r.spentJudgment(ctx, task, c); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, s := range g.steps(t, c.ID) {
		if s.Rung == 0 && strings.HasPrefix(s.Found, markJudgmentSpent) {
			n++
		}
	}
	stops, _ := g.s.TaskEventsOfKind(ctx, chat.ID, EventChatBudgetSpent, 5)
	if n != 1 || len(stops) != 1 {
		t.Fatalf("%d spent steps and %d stops, want one of each", n, len(stops))
	}
	cardForge, _ := newAlertForgejo(t)
	cards := &ChatBudgetCards{Store: projectStore{g.s}, Forgejo: cardForge, Timeline: g.r.timeline}
	if err := cards.Check(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	var made int
	if err := g.s.db.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE source = $1 AND parent_task = $2`, SourceChatBudget, chat.ID).Scan(&made); err != nil || made != 1 {
		t.Fatalf("%d money cards after the loop, %v; want 1", made, err)
	}
}
