package factoryd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const unjudgedSuffix = " (not judged, judgment budget spent)"

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

// Six rung 1 steps on the card's cases and no rung1_runs key: the next
// trip runs no conversation, goes to the human and its ask names the count.
func TestRung1PerCardCountBound(t *testing.T) {
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
	if out.Rung != 3 || g.chat.calls != 0 {
		t.Fatalf("outcome %+v, %d conversations; want the face and none", out, g.chat.calls)
	}
	asks := g.asks(t)
	if len(asks) != 1 || !strings.Contains(asks[0].Detail["ask"].(string), "per-card count of 6") {
		t.Fatalf("asks = %+v, want one naming the per-card count of 6", asks)
	}
	if !slices.ContainsFunc(g.steps(t, out.CaseID), func(s CaseStep) bool {
		return s.Rung == 0 && s.Found == "rung 1 reached its per-card count of 6 on this card"
	}) {
		t.Fatalf("steps %+v, want rung 0's step naming the count", g.steps(t, out.CaseID))
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
