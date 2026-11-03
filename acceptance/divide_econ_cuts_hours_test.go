package acceptance

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// accEconCard inserts a card already merged on its own issue: finished
// when created, its budget deliberately unlike its spend, the spend booked
// as the model rows of the phases models names and the final settlement
// that brings the card's life to final. Its merge and reading are recorded
// at at.
func accEconCard(t *testing.T, s *factoryd.PGStore, db *sql.DB, id, size string, lines int, budget float64, models []float64, final float64, issue int64, at time.Time) {
	t.Helper()
	task := factoryd.Task{ID: id, Status: "merged", Objective: "the divide panels' fixture",
		Repository: "sf/code", Kind: "agent", Source: "api", Track: "direct", BudgetEUR: budget,
		IssueNumber: issue, SHA: "h" + id, Size: size, CreatedAt: at}
	if err := s.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	for i, eur := range models {
		if _, err := db.Exec(`INSERT INTO ledger (task_id, kind, ref, eur) VALUES ($1, 'model', $2, $3)`,
			id, fmt.Sprintf("phase-%d", i), eur); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SettleSpendFinal(context.Background(), id, final); err != nil {
		t.Fatal(err)
	}
	accReading(t, s, id, "sf/code", "h"+id, size, 500, lines, 0, issue, at)
	accMerged(t, s, id, "sf/code", at)
}

// TestDivideConquerEconomics proves panels 4 and 5: the window's actual
// euros over its merged changed lines, and each size's fixed euros per
// card as the intercept of its fit. The store case gives every card a
// budget unlike its spend and books the spend in several phases, so a
// budget-based or one-phase reading cannot pass; unmerged, other-
// repository, duplicate-merge, no-cost and no-reading cards stay out.
func TestDivideConquerEconomics(t *testing.T) {
	t.Run("fake", func(t *testing.T) {
		rig := newDivideRig(t, &accFigures{report: accPanelsReport()})
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
		s, db := divideStore(t)
		// e1 and e2 small, e3 and e4 medium, el large: 22 EUR over 1100
		// merged changed lines is €0.0200; the small fit through (100, 3)
		// and (300, 5) meets the euros axis at 2.00, the medium through
		// (200, 5) and (400, 7) at 3.00, and one large card holds no fit
		accEconCard(t, s, db, "t-econ-1", "small", 100, 9, []float64{1}, 3, 401, accDay(3))
		accEconCard(t, s, db, "t-econ-2", "small", 300, 8, []float64{2, 1}, 5, 402, accDay(3))
		accEconCard(t, s, db, "t-econ-3", "medium", 200, 7, nil, 5, 403, accDay(4))
		accEconCard(t, s, db, "t-econ-4", "medium", 400, 6, []float64{3}, 7, 404, accDay(4))
		accEconCard(t, s, db, "t-econ-l", "large", 100, 9, nil, 2, 405, accDay(5))
		// the window before: one small card, 1 EUR over 100 lines
		accEconCard(t, s, db, "t-econ-p", "small", 100, 9, nil, 1, 406, accDay(-5))
		// a second merge row of e1 fans no card out
		accMerged(t, s, "t-econ-1", "sf/code", accDay(3).Add(2*time.Hour))
		// an unmerged card with spend and a reading, and another
		// repository's merged card, belong to no cohort of this window
		if err := s.CreateTask(context.Background(), factoryd.Task{ID: "t-econ-x", Status: "in-review", Objective: "the divide panels' fixture",
			Repository: "sf/code", Kind: "agent", Source: "api", Track: "direct", BudgetEUR: 4, IssueNumber: 407, SHA: "hx", Size: "small"}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO ledger (task_id, kind, ref, eur) VALUES ('t-econ-x', 'model', 'phase-0', 4)`); err != nil {
			t.Fatal(err)
		}
		accReading(t, s, "t-econ-x", "sf/code", "hx", "small", 300, 500, 0, 407, accDay(6))
		accCard(t, s, "t-econ-o", "sf/other", "medium", "ho", 408)
		if _, err := db.Exec(`INSERT INTO ledger (task_id, kind, ref, eur) VALUES ('t-econ-o', 'model', 'phase-0', 6)`); err != nil {
			t.Fatal(err)
		}
		accReading(t, s, "t-econ-o", "sf/other", "ho", "medium", 600, 400, 0, 408, accDay(6))
		accMerged(t, s, "t-econ-o", "sf/other", accDay(6))
		// a merged card whose spend no row books is unknown, never free;
		// one with no reading kept no diff statistics
		accCard(t, s, "t-econ-nc", "sf/code", "small", "hnc", 409)
		accReading(t, s, "t-econ-nc", "sf/code", "hnc", "small", 300, 90, 10, 409, accDay(8))
		accMerged(t, s, "t-econ-nc", "sf/code", accDay(8))
		accCard(t, s, "t-econ-nl", "sf/code", "medium", "hnl", 410)
		if _, err := db.Exec(`INSERT INTO ledger (task_id, kind, ref, eur) VALUES ('t-econ-nl', 'model', 'phase-0', 5)`); err != nil {
			t.Fatal(err)
		}
		accMerged(t, s, "t-econ-nl", "sf/code", accDay(9))

		rig := newDivideRig(t, s)
		dc := rig.report()
		same(t, "eur_per_merged_line", dc.EurPerMergedLine, accFig(0.02, 0.01, [7]float64{0, 0, 0, 0, 0.01, 0.02, 0}))
		same(t, "fixed_eur_per_card", dc.FixedEurPerCard, []factoryd.SizeIntercept{
			{Size: "large", Cards: 1},
			{Size: "medium", EUR: 3, Available: true, Cards: 2},
			{Size: "small", EUR: 2, Available: true, Cards: 2},
		})
		same(t, "economics_coverage", dc.EconomicsCoverage, factoryd.EconomicsCoverage{Cards: 7, MissingCost: 1, MissingLines: 1})
		body := rig.page()
		holds(t, panel(t, body, "EUR per merged line"), []string{"€0.0200", "7 cards · 1 missing cost · 1 missing lines",
			"TestDivideConquerEconomics in acceptance"}, []string{"€0.0182", "€0.0354"})
		holds(t, panel(t, body, "fixed EUR per card"), []string{"large unavailable / medium 3.00 / small 2.00",
			"TestDivideConquerEconomics in acceptance"}, []string{"large 0.00"})

		// a window of two cards that both changed no line: no denominator
		// (an em dash, never zero euros per line), and no variance in
		// their lines, so the fit is unavailable
		accEconCard(t, s, db, "t-econ-z1", "small", 0, 5, nil, 4, 411, accDay(22))
		accEconCard(t, s, db, "t-econ-z2", "small", 0, 5, nil, 6, 412, accDay(23))
		zero := rig.reportAt(accSpan(accDay(21), accDay(27))).DivideConquer
		same(t, "zero-line eur_per_merged_line has", zero.EurPerMergedLine.Has, false)
		same(t, "zero-line fits", zero.FixedEurPerCard, []factoryd.SizeIntercept{{Size: "small", Cards: 2}})
		page := rig.pageQuery(accSpan(accDay(21), accDay(27)))
		holds(t, panel(t, page, "EUR per merged line"), []string{">—<", "2 cards"}, []string{"€0.0000"})
		holds(t, panel(t, page, "fixed EUR per card"), []string{"small unavailable"}, []string{"small 0.00"})
		// a window with no merged card at all
		empty := rig.pageQuery(accSpan(accDay(100), accDay(106)))
		holds(t, panel(t, empty, "EUR per merged line"), []string{">—<", "0 cards"}, nil)
		holds(t, panel(t, empty, "fixed EUR per card"), []string{">—<"}, nil)
	})
}

// TestDivideConquerCuts proves panel 6: the window's applied cuts, and
// the groups they left that the window's own end saw closed. The store
// case holds two groups cut in the window, one closed inside it and one
// closed after its end, a retried cut, a repeated closure, a request, a
// closed group no cut touched and another repository's cut; the answer
// stays 2 / 1 after the late closure lands.
func TestDivideConquerCuts(t *testing.T) {
	t.Run("fake", func(t *testing.T) {
		rig := newDivideRig(t, &accFigures{report: accPanelsReport()})
		holds(t, rig.page(), []string{
			">2 / 1<",
			"TestDivideConquerCuts in acceptance",
		}, nil)
	})

	t.Run("store", func(t *testing.T) {
		s, _ := divideStore(t)
		cut := func(plan, card string, issue int64, at time.Time) int64 {
			return accRecord(t, s, factoryd.CutFact{Plan: plan, Card: card, Issue: issue,
				Group: plan, Revision: 2, Into: []int64{71, 72}, Session: 5, Spent: 1.5}.Event("sf/code", issue), at)
		}
		group := func(plan, card string, planIssue, issue int64) {
			accPlan(t, s, plan, "sf/code", planIssue)
			accPlanChild(t, s, card, plan, "sf/code", issue)
		}
		group("t-cut-a", "a1", 51, 61)
		group("t-cut-b", "b1", 52, 62)
		group("t-cut-c", "c1", 53, 63)
		group("t-cut-d", "d1", 54, 64)
		group("t-cut-p", "p1", 55, 65)
		cut("t-cut-a", "a1", 61, accDay(1))
		// the retried cut answers id 0: one card, one cut
		if id := cut("t-cut-a", "a1", 61, accDay(2)); id != 0 {
			t.Errorf("retried cut answered id %d, want 0", id)
		}
		cut("t-cut-b", "b1", 62, accDay(3))
		cut("t-cut-p", "p1", 65, accDay(-1))
		// c1's cut is only asked for: the console's replan request, no cut
		accRecord(t, s, factoryd.Event{Repository: "sf/code", TaskID: "c1", IssueNumber: 63, Kind: "card_replanned",
			Actor: "sebastian", Via: factoryd.ViaButton, Detail: map[string]any{"verdict": "cut"}}, accDay(4))
		// another repository's cut and closed group
		accPlan(t, s, "t-cut-o", "sf/other", 56)
		accPlanChild(t, s, "o1", "t-cut-o", "sf/other", 66)
		accRecord(t, s, factoryd.CutFact{Plan: "t-cut-o", Card: "o1", Issue: 66, Group: "t-cut-o", Revision: 2}.Event("sf/other", 66), accDay(2))
		accClosed(t, s, "t-cut-o", "sf/other", accDay(3))
		// a: closed in the window, twice; c and d: closed, never cut; p:
		// closed inside the window before
		accClosed(t, s, "t-cut-a", "sf/code", accDay(5))
		accClosed(t, s, "t-cut-a", "sf/code", accDay(6))
		accClosed(t, s, "t-cut-c", "sf/code", accDay(4))
		accClosed(t, s, "t-cut-d", "sf/code", accDay(4))
		accClosed(t, s, "t-cut-p", "sf/code", accDay(-1).Add(2*time.Hour))

		rig := newDivideRig(t, s)
		want := func(dc factoryd.DivideConquer) {
			t.Helper()
			same(t, "cuts", dc.Cuts, accFig(2, 1, [7]float64{0, 0, 0, 0, 1, 2, 0}))
			same(t, "cut_groups_closed", dc.CutGroupsClosed, accFig(1, 1, [7]float64{0, 0, 0, 0, 1, 1, 0}))
			holds(t, panel(t, rig.page(), "cuts / closed cut groups"), []string{">2 / 1<", "TestDivideConquerCuts in acceptance"}, nil)
		}
		want(rig.report())
		// b closes after the window ended: the window's answer stays, as
		// of its own end
		accClosed(t, s, "t-cut-b", "sf/code", accDay(15))
		want(rig.report())
	})
}

// TestDivideConquerGroupHours proves panel 7: the mean hours a group
// stayed open from its first child merge to its first closure in the
// window. Both measured groups open before or inside the window and are
// measured whole, the one opened before the window in full; a duplicate
// merge and a repeated closure inflate nothing; a group still open and a
// closed one no merge covers are counted apart.
func TestDivideConquerGroupHours(t *testing.T) {
	t.Run("fake", func(t *testing.T) {
		rig := newDivideRig(t, &accFigures{report: accPanelsReport()})
		holds(t, rig.page(), []string{
			">6.0 hours<",
			"cohort 2",
			"TestDivideConquerGroupHours in acceptance",
		}, nil)
	})

	t.Run("store", func(t *testing.T) {
		s, _ := divideStore(t)
		group := func(plan, repository string, issue int64) {
			accPlan(t, s, plan, repository, issue)
			accPlanChild(t, s, plan+"-c", plan, repository, issue+100)
		}
		// g1 opens two hours before the window and closes two hours into
		// it: 4 hours, the half before the window kept; its child merged
		// twice and a second child merged later
		group("t-group-1", "sf/code", 51)
		accMerged(t, s, "t-group-1-c", "sf/code", accDay(0).Add(-2*time.Hour))
		accMerged(t, s, "t-group-1-c", "sf/code", accDay(0).Add(-time.Hour))
		accPlanChild(t, s, "t-group-1-d", "t-group-1", "sf/code", 161)
		accMerged(t, s, "t-group-1-d", "sf/code", accDay(1))
		accClosed(t, s, "t-group-1", "sf/code", accDay(0).Add(2*time.Hour))
		// g2: 8 hours, its closure recorded again a day later
		group("t-group-2", "sf/code", 52)
		accMerged(t, s, "t-group-2-c", "sf/code", accDay(3))
		accClosed(t, s, "t-group-2", "sf/code", accDay(3).Add(8*time.Hour))
		accClosed(t, s, "t-group-2", "sf/code", accDay(4))
		// g3 is open at the report; g4 closed in the window and no child
		// of it ever merged; g5 is another repository's
		group("t-group-3", "sf/code", 53)
		accMerged(t, s, "t-group-3-c", "sf/code", accDay(5))
		accPlan(t, s, "t-group-4", "sf/code", 54)
		accClosed(t, s, "t-group-4", "sf/code", accDay(6))
		group("t-group-5", "sf/other", 55)
		accMerged(t, s, "t-group-5-c", "sf/other", accDay(1))
		accClosed(t, s, "t-group-5", "sf/other", accDay(2))
		// g6 ran 2 hours in the window before
		group("t-group-6", "sf/code", 56)
		accMerged(t, s, "t-group-6-c", "sf/code", accDay(-9))
		accClosed(t, s, "t-group-6", "sf/code", accDay(-9).Add(2*time.Hour))

		rig := newDivideRig(t, s)
		dc := rig.report()
		// the mean of 4 and 8 hours is 6: 21600000 milliseconds
		same(t, "group_open_milliseconds", dc.GroupOpenMillis, accFig(21.6e6, 7.2e6, [7]float64{0, 0, 0, 7.2e6, 0, 21.6e6, 0}))
		same(t, "group_open_hours", dc.GroupOpenHours, accFig(6, 2, [7]float64{0, 0, 0, 2, 0, 6, 0}))
		same(t, "group_hours_coverage", dc.GroupHoursCoverage, factoryd.GroupHoursCoverage{Groups: 2, MissingStart: 1, Open: 1})
		holds(t, panel(t, rig.page(), "group open hours"), []string{
			">6.0 hours<",
			"cohort 2 · open 1 · missing start 1",
			"TestDivideConquerGroupHours in acceptance",
		}, nil)
	})
}

// TestDivideConquerProductionHold proves panel 8: the milliseconds
// production waited on hold, the holds' union inside the window. The
// store case seeds the first hold twice (a legacy double with no
// identity), two overlapping holds, a hold ending exactly at the
// window's start, another repository's hold and a row with no usable
// start, every row's rounded hours deliberately wrong, then a hold the
// releases row still holds, clipped to the sooner of the report's as-of
// and the window's end.
func TestDivideConquerProductionHold(t *testing.T) {
	t.Run("fake", func(t *testing.T) {
		rig := newDivideRig(t, &accFigures{report: accPanelsReport()})
		holds(t, rig.page(), []string{
			">3.75 hours<",
			"ended 2",
			"TestDivideConquerProductionHold in acceptance",
		}, nil)
	})

	t.Run("store", func(t *testing.T) {
		s, db := divideStore(t)
		hold := func(repository string, since, until time.Time, roundedHours float64) {
			accRecord(t, s, factoryd.HoldIntervalFact{Repository: repository,
				HeldSince: since, How: "closed", Hours: roundedHours}.Event(), until)
		}
		// [d1, d1+1.5h); the same hold once more, written before the
		// identity contract; [d3, d3+2h) and [d3+1h, d3+2.25h), which
		// overlap: the union is 1.5 + 2.25 = 3.75 hours, 13500000
		// milliseconds, whatever the rows' hours say
		hold("sf/code", accDay(1), accDay(1).Add(90*time.Minute), 1)
		accRecord(t, s, factoryd.Event{Repository: "sf/code", Kind: factoryd.EventProductionReleased,
			Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd,
			Detail: map[string]any{"how": "closed", "held_since": accDay(1).Format(time.RFC3339), "hours": 1.5}},
			accDay(1).Add(90*time.Minute))
		hold("sf/code", accDay(3), accDay(3).Add(2*time.Hour), 9)
		hold("sf/code", accDay(3).Add(time.Hour), accDay(3).Add(135*time.Minute), 9)
		// a day that ended at the window's first instant: all of it the
		// previous window's
		hold("sf/code", accDay(-1), accDay(0), 1)
		hold("sf/other", accDay(6), accDay(6).Add(5*time.Hour), 5)
		accRecord(t, s, factoryd.Event{Repository: "sf/code", Kind: factoryd.EventProductionReleased,
			Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd,
			Detail: map[string]any{"how": "closed", "held_since": "not a time", "hours": 7}}, accDay(7))

		rig := newDivideRig(t, s)
		dc := rig.report()
		same(t, "hold_wait_milliseconds", dc.HoldWaitMillis, accFig(13.5e6, 86.4e6, [7]float64{0, 0, 0, 0, 86.4e6, 13.5e6, 0}))
		same(t, "hold_wait_hours", dc.HoldWaitHours, accFig(3.75, 24, [7]float64{0, 0, 0, 0, 24, 3.75, 0}))
		same(t, "hold_wait_coverage", dc.HoldWaitCoverage, factoryd.HoldWaitCoverage{Ended: 4, Missing: 1})
		holds(t, panel(t, rig.page(), "production hold hours"), []string{
			">3.75 hours<",
			"ended 4 · missing 1",
			"TestDivideConquerProductionHold in acceptance",
		}, nil)

		// a hold the releases clock still holds, since day 10: in the one
		// day of it, the day's 24 hours, not the weeks the clock has run
		open := accDay(10)
		if _, err := db.Exec(`INSERT INTO releases (repository, held_since) VALUES ('sf/code', $1)`, open); err != nil {
			t.Fatal(err)
		}
		oneDay := rig.reportAt(accSpan(open, open)).DivideConquer
		same(t, "one-day hold_wait_milliseconds", oneDay.HoldWaitMillis.Value, 86.4e6)
		same(t, "one-day coverage", oneDay.HoldWaitCoverage, factoryd.HoldWaitCoverage{Ended: 4, Open: 1, Missing: 1})
		holds(t, panel(t, rig.pageQuery(accSpan(open, open)), "production hold hours"), []string{">24.0 hours<", "ended 4 · open 1 · missing 1"}, nil)

		// a window that ends after the report is taken: the open hold ends
		// at the report's own as-of, not at the window's end tomorrow
		tomorrow := time.Now().UTC().AddDate(0, 0, 1).Truncate(24 * time.Hour)
		before := time.Now()
		got := rig.reportAt(accSpan(open, tomorrow)).DivideConquer.HoldWaitMillis.Value
		after := time.Now()
		lo, hi := float64(before.Sub(open).Milliseconds())-1, float64(after.Sub(open).Milliseconds())+1
		if got < lo || got > hi {
			t.Errorf("open hold waited %v ms, want the report's as-of, between %v and %v", got, lo, hi)
		}
		if end := float64(tomorrow.AddDate(0, 0, 1).Sub(open).Milliseconds()); got >= end {
			t.Errorf("open hold waited %v ms, not clipped to the report's as-of before the window's end %v", got, end)
		}
	})
}
