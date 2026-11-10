package factoryd

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The first raise a session's money ask offers always opens the session,
// on every cent of budget and spend: the ceiling the floor reads and the
// raise the ask computes round alike, so the 0.40 EUR reserve, which no
// binary float holds, never leaves the card one ulp short. The grid is
// budgets of 1.00 to 1.30 EUR, every spend from budget - 0.90 to
// budget + 0.10, the cheap floor 0.30 and the frontier floor 0.60, with
// no hold and with one of 0.10 EUR outstanding, on the real store.
func TestTheFirstRaiseOpensTheSessionOnEveryCent(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_grid"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, holds, ledger, audit CASCADE`); err != nil {
		t.Fatal(err)
	}
	if err := s.insertTask(ctx, Task{ID: "t-grid", Repository: "o/r", Kind: "agent", Status: "running", BudgetEUR: 1, Objective: "b", Track: "direct"}, ""); err != nil {
		t.Fatal(err)
	}
	v := testPolicyVersion()
	v.Tree.Document.Budget.SessionFloorCheapEUR = 0.3
	v.Tree.Document.Budget.SessionFloorFrontierEUR = 0.6
	prices := aliasPrices{"big": {In: 3e-6, CacheRead: 3e-7, Out: 1.5e-5, MaxOutput: 32768},
		"small": {In: 1e-6, CacheRead: 1e-7, Out: 4e-6, MaxOutput: 32768}}
	r := &Runner{store: s, cfg: RunnerConfig{Prices: prices, Policy: func() policy.Version { return v },
		Models: ModelLadder{Start: "factory/small", Stronger: "factory/big"}}}
	set := func(budgetCents, spentCents int) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET budget_eur = $1::numeric / 100, spent_eur = $2::numeric / 100 WHERE id = 't-grid'`, budgetCents, spentCents); err != nil {
			t.Fatal(err)
		}
	}
	refused, failed := 0, 0
	for _, hold := range []bool{false, true} {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM holds`); err != nil {
			t.Fatal(err)
		}
		if hold {
			if _, err := s.db.ExecContext(ctx, `INSERT INTO holds (task_id, kind, ref, eur, expires_at) VALUES ('t-grid', 'model', 'small', 0.10, now() + interval '1 hour')`); err != nil {
				t.Fatal(err)
			}
		}
		for _, model := range []string{"factory/small", "factory/big"} {
			for budget := 100; budget <= 130; budget++ {
				for spent := budget - 90; spent <= budget+10; spent++ {
					set(budget, spent)
					why, err := r.SessionFunded(ctx, "t-grid", model)
					if err != nil {
						t.Fatal(err)
					}
					if why == "" {
						continue
					}
					refused++
					card, _, err := s.Task(ctx, "t-grid")
					if err != nil {
						t.Fatal(err)
					}
					if own, _, err := s.Free(ctx, policy.HoldRequest{TaskID: "t-grid"}); err != nil || own.Free() != card.Left() {
						t.Fatalf("%s budget %d spent %d hold %v: the floor reads %v free, the ask %v: %v", model, budget, spent, hold, own.Free(), card.Left(), err)
					}
					q := budgetQuestion(why, r.questionFacts(ctx, card, why))
					raise, err := strconv.ParseFloat(strings.TrimPrefix(q.Choices[0].Value, AnswerFund+":"), 64)
					if err != nil || !strings.HasPrefix(q.Choices[0].Value, AnswerFund+":") {
						t.Fatalf("%s budget %d spent %d hold %v: first choice %v", model, budget, spent, hold, q.Choices)
					}
					set(budget+int(raise*100+0.5), spent)
					if again, err := r.SessionFunded(ctx, "t-grid", model); err != nil || again != "" {
						failed++
						if failed <= 5 {
							t.Errorf("%s budget %.2f spent %.2f hold %v: raised %.2f, still refused: %v", model, float64(budget)/100, float64(spent)/100, hold, raise, strings.SplitN(again, "\n", 3)[len(strings.SplitN(again, "\n", 3))-1])
						}
					}
				}
			}
		}
	}
	if refused == 0 {
		t.Fatal("no refused state on the grid")
	}
	if failed > 0 {
		t.Fatalf("%d of %d refused states stay refused after the first raise", failed, refused)
	}
}
