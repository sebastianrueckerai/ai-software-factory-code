package acceptance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// accEconCard inserts a card already merged: finished when created, so
// its spend settles, its merge and reading recorded by the caller.
func accEconCard(t *testing.T, s *factoryd.PGStore, id, size string, lines int, eur float64, at time.Time) {
	t.Helper()
	task := factoryd.Task{ID: id, Status: "merged", Objective: "the divide panels' fixture",
		Repository: "sf/code", Kind: "agent", Source: "api", Track: "direct", BudgetEUR: 5,
		IssueNumber: 41, SHA: "h" + id, Size: size, CreatedAt: at}
	if err := s.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleSpendFinal(context.Background(), id, eur); err != nil {
		t.Fatal(err)
	}
	accReading(t, s, id, "sf/code", "h"+id, size, 500, lines, 0, 41, at)
	accMerged(t, s, id, "sf/code", at.Add(time.Hour))
}

func TestDivideConquerEconomics(t *testing.T) {
	t.Run("fake", func(t *testing.T) {
		rig, _ := NewMetricsRig(t, &accFigures{report: accPanelsReport()})
		body := rig.page()
		holds(t, body, []string{
			"€0.0200",
			"small 2.00 / medium 3.00",
			"4 cards",
			"TestDivideConquerEconomics in acceptance",
		}, nil)
		if n := strings.Count(body, "TestDivideConquerEconomics in acceptance"); n != 2 {
			t.Errorf("proof label prints %d times, want 2", n)
		}
	})

	t.Run("store", func(t *testing.T) {
		s, _ := divideStore(t)
		at := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
		// e1 small 100 lines 3 EUR, e2 small 300 lines 5 EUR, e3 medium
		// 200 lines 5 EUR, e4 medium 400 lines 7 EUR: 20 EUR over 1000
		// merged changed lines answers €0.0200, the small fit 2.00 and
		// the medium 3.00.
		accEconCard(t, s, "t-econ-1", "small", 100, 3, at)
		accEconCard(t, s, "t-econ-2", "small", 300, 5, at)
		accEconCard(t, s, "t-econ-3", "medium", 200, 5, at)
		accEconCard(t, s, "t-econ-4", "medium", 400, 7, at)
		rig, _ := NewMetricsRig(t, s)
		holds(t, rig.page(), []string{
			"€0.0200",
			"medium 3.00 / small 2.00",
			"4 cards",
			"TestDivideConquerEconomics in acceptance",
		}, nil)
	})
}

func TestDivideConquerCuts(t *testing.T) {
	t.Run("fake", func(t *testing.T) {
		rig, _ := NewMetricsRig(t, &accFigures{report: accPanelsReport()})
		holds(t, rig.page(), []string{
			">2 / 1<",
			"TestDivideConquerCuts in acceptance",
		}, nil)
	})

	t.Run("store", func(t *testing.T) {
		s, _ := divideStore(t)
		from, _ := divideWindow()
		accPlan(t, s, "t-cut-a", "sf/code", 51)
		accPlanChild(t, s, "a1", "t-cut-a", "sf/code", 61)
		accPlanChild(t, s, "a2", "t-cut-a", "sf/code", 62)
		cut := func(card string, issue int64, at time.Time) {
			accRecord(t, s, factoryd.CutFact{Plan: "t-cut-a", Card: card, Issue: issue,
				Group: "t-cut-a", Revision: 2, Into: []int64{71, 72}, Session: 5, Spent: 1.5}.
				Event("sf/code", issue), at)
		}
		cut("a1", 61, from.Add(24*time.Hour))
		cut("a2", 62, from.Add(25*time.Hour))
		// the retried cut answers id 0: one card, one cut, however often
		// its filing runs again.
		if id := accRecord(t, s, factoryd.CutFact{Plan: "t-cut-a", Card: "a1", Issue: 61,
			Group: "t-cut-a", Revision: 2, Into: []int64{71, 72}, Session: 5, Spent: 1.5}.
			Event("sf/code", 61), from.Add(26*time.Hour)); id != 0 {
			t.Errorf("retried cut answered id %d, want 0", id)
		}
		// a cut before the window counts only the previous span.
		cut("t-cut-prev", 63, from.Add(-time.Hour))
		accClosed(t, s, "t-cut-a", "sf/code", from.Add(48*time.Hour))
		rig, _ := NewMetricsRig(t, s)
		holds(t, rig.page(), []string{
			">2 / 1<",
			"TestDivideConquerCuts in acceptance",
		}, nil)
	})
}

func TestDivideConquerGroupHours(t *testing.T) {
	t.Run("fake", func(t *testing.T) {
		rig, _ := NewMetricsRig(t, &accFigures{report: accPanelsReport()})
		holds(t, rig.page(), []string{
			">6.0 hours<",
			"cohort 2",
			"TestDivideConquerGroupHours in acceptance",
		}, nil)
	})

	t.Run("store", func(t *testing.T) {
		s, _ := divideStore(t)
		from, _ := divideWindow()
		// g-open 4 hours and g-late 8 hours: a mean of 6.0 over the two
		// groups the window's own end saw closed.
		for _, g := range []struct {
			plan string
			open time.Duration
		}{
			{"t-group-1", 4 * time.Hour},
			{"t-group-2", 8 * time.Hour},
		} {
			accPlan(t, s, g.plan, "sf/code", 52)
			accPlanChild(t, s, g.plan+"-c", g.plan, "sf/code", 53)
			merged := from.Add(24 * time.Hour)
			accMerged(t, s, g.plan+"-c", "sf/code", merged)
			accClosed(t, s, g.plan, "sf/code", merged.Add(g.open))
		}
		rig, _ := NewMetricsRig(t, s)
		holds(t, rig.page(), []string{
			">6.0 hours<",
			"cohort 2",
			"TestDivideConquerGroupHours in acceptance",
		}, nil)
	})
}

func TestDivideConquerProductionHold(t *testing.T) {
	t.Run("fake", func(t *testing.T) {
		rig, _ := NewMetricsRig(t, &accFigures{report: accPanelsReport()})
		holds(t, rig.page(), []string{
			">3.75 hours<",
			"ended 2",
			"TestDivideConquerProductionHold in acceptance",
		}, nil)
	})

	t.Run("store", func(t *testing.T) {
		s, _ := divideStore(t)
		from, _ := divideWindow()
		hold := func(since time.Time, hours float64) {
			accRecord(t, s, factoryd.HoldIntervalFact{Repository: "sf/code",
				HeldSince: since, How: "closed", Hours: hours}.Event(),
				since.Add(time.Duration(hours*float64(time.Hour))))
		}
		hold(from.Add(time.Hour), 1.5)
		hold(from.Add(2*time.Hour), 2.25)
		rig, _ := NewMetricsRig(t, s)
		holds(t, rig.page(), []string{
			">3.75 hours<",
			"ended 2",
			"TestDivideConquerProductionHold in acceptance",
		}, nil)
	})
}
