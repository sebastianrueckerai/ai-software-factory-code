package acceptance

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// TestDivideConquerReviewCoverage proves panel 9: the model reviews that
// answered not reviewed, over the genuine answers. The store case runs
// the Runner's ModelReview itself against a fake Forgejo and model
// gateway: a concerns verdict judged the head and stays reviewed, a
// refused and an unreadable answer did not, a replayed attempt is one
// answer, a second round and another head are their own, another
// repository's review stays out, and a keyed row with no classification
// is counted unknown, never an answer.
func TestDivideConquerReviewCoverage(t *testing.T) {
	t.Run("fake", func(t *testing.T) {
		rig := newDivideRig(t, &accFigures{report: accPanelsReport()})
		holds(t, rig.page(), []string{
			">2 of 3 · target 0<",
			"1 unknown",
			"TestDivideConquerReviewCoverage in acceptance",
		}, nil)
	})

	t.Run("store", func(t *testing.T) {
		s, db := divideStore(t)
		accDriveReviews(t, s, db)
		rig := newDivideRig(t, s)
		today := time.Now().UTC().Truncate(24 * time.Hour)
		span := accSpan(today.AddDate(0, 0, -6), today)
		dc := rig.reportAt(span).DivideConquer
		same(t, "reviews_not_reviewed", dc.ReviewsNotReviewed, factoryd.Figure{Value: 2, Has: true, Weeks: [7]float64{6: 2}})
		same(t, "review answers", dc.ReviewEvidence.Answers, factoryd.Figure{Value: 4, Has: true, Weeks: [7]float64{6: 4}})
		same(t, "review unknown", dc.ReviewEvidence.Unknown, factoryd.Figure{Value: 1, Has: true, Weeks: [7]float64{6: 1}})
		holds(t, panel(t, rig.pageQuery(span), "not reviewed answers"),
			[]string{">2 of 4 · target 0<", "1 unknown", "TestDivideConquerReviewCoverage in acceptance"}, nil)
		// a span no review covers is unknown, not a measured zero
		holds(t, panel(t, rig.pageQuery(accSpan(accDay(100), accDay(106))), "not reviewed answers"), []string{">— · target 0<"}, nil)
	})
}

// TestDivideConquerPlanning proves panels 10 to 12. The store case seeds
// plan filings through their fact payloads: a plan's rounds are the
// highest explicit round at its closure, not its revisions (three
// revisions, two rounds), a round filed after the closure and a legacy
// double of a canonical revision add nothing, a card refiled in a later
// window counts once at its original filing, a plan filed before rounds
// keeps the unknown bucket whatever canonical row arrives after the
// window, and another repository's plan stays out. The report's own
// previous window and seven weeks are asserted for every name.
func TestDivideConquerPlanning(t *testing.T) {
	t.Run("fake", func(t *testing.T) {
		rig := newDivideRig(t, &accFigures{report: accPanelsReport()})
		body := rig.page()
		holds(t, body, []string{
			"1 round:1 / 2 rounds:1",
			"first 4 · later 3 · 1 round:4 / 2 rounds:2 / 3 rounds:1",
			"person:1 / cut:1 / round:1 / unknown:2",
			"TestDivideConquerPlanning in acceptance",
		}, nil)
		if n := strings.Count(body, "TestDivideConquerPlanning in acceptance"); n != 3 {
			t.Errorf("proof label prints %d times, want 3", n)
		}
		// the scripted later-round total is what its detail adds up to
		r := accPanelsReport().DivideConquer
		var detail float64
		for _, nf := range r.CardsByRound[1:] {
			detail += nf.Figure.Value
		}
		if detail != r.CardsLaterRounds.Value {
			t.Errorf("per-round detail adds to %v, the total says %v", detail, r.CardsLaterRounds.Value)
		}
	})

	t.Run("store", func(t *testing.T) {
		s, _ := divideStore(t)
		filing := func(plan string, revision int, origin string, card int64, key string, round int, at time.Time) {
			extra := map[string]any{}
			if round > 0 {
				extra["round"] = round
			}
			accRecord(t, s, factoryd.PlanFilingFact{Plan: plan, Revision: revision, Origin: origin, Card: card, Key: key}.
				Event("sf/code", 80, extra), at)
		}
		plan := func(id string, issue int64) { accPlan(t, s, id, "sf/code", issue) }
		// P1: revision 1 files k1 and k2, revision 2 (a person's, still
		// round 1) k3 and refiles k1, revision 3 (a cut's, round 2) k4 and
		// k5; a legacy double of revision 3 naming round 7 came first.
		// Three revisions, two rounds; closed twice.
		plan("t-plan-1", 81)
		filing("t-plan-1", 1, "", 0, "", 1, accDay(1))
		filing("t-plan-1", 1, "", 91, "k1", 1, accDay(1))
		filing("t-plan-1", 1, "", 92, "k2", 1, accDay(1))
		filing("t-plan-1", 2, factoryd.ReplanPerson, 0, "", 1, accDay(2))
		filing("t-plan-1", 2, factoryd.ReplanPerson, 93, "k3", 1, accDay(2))
		filing("t-plan-1", 2, factoryd.ReplanPerson, 91, "k1", 1, accDay(2))
		accRecord(t, s, factoryd.Event{Repository: "sf/code", TaskID: "t-plan-1", IssueNumber: 81, Kind: factoryd.EventPlanReplanned,
			Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd, Detail: map[string]any{"revision": 3, "origin": "", "round": 7}},
			accDay(3).Add(8*time.Hour))
		filing("t-plan-1", 3, factoryd.ReplanCut, 0, "", 2, accDay(3).Add(9*time.Hour))
		filing("t-plan-1", 3, factoryd.ReplanCut, 94, "k4", 2, accDay(3).Add(9*time.Hour))
		filing("t-plan-1", 3, factoryd.ReplanCut, 95, "k5", 2, accDay(3).Add(9*time.Hour))
		accClosed(t, s, "t-plan-1", "sf/code", accDay(3).Add(12*time.Hour))
		accClosed(t, s, "t-plan-1", "sf/code", accDay(4))
		// P2: round 1, closed on day 5; a third round filed on day 9, after
		// the closure, counts as filings and a replan but not as its rounds
		plan("t-plan-2", 82)
		filing("t-plan-2", 1, "", 0, "", 1, accDay(1))
		filing("t-plan-2", 1, "", 96, "q1", 1, accDay(1))
		accClosed(t, s, "t-plan-2", "sf/code", accDay(5))
		filing("t-plan-2", 2, factoryd.ReplanRound, 0, "", 3, accDay(9))
		filing("t-plan-2", 2, factoryd.ReplanRound, 97, "z1", 3, accDay(9))
		// P3: filed before rounds — a card with no round, a legacy
		// revision row of an unrecognised origin — closed on day 8; its
		// canonical replacement arrives on day 20, after the window
		plan("t-plan-3", 83)
		filing("t-plan-3", 1, "", 98, "u1", 0, accDay(4))
		accRecord(t, s, factoryd.Event{Repository: "sf/code", TaskID: "t-plan-3", IssueNumber: 83, Kind: factoryd.EventPlanReplanned,
			Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd, Detail: map[string]any{"revision": 1, "origin": "legacy"}}, accDay(6))
		accClosed(t, s, "t-plan-3", "sf/code", accDay(8))
		filing("t-plan-3", 1, factoryd.ReplanCut, 0, "", 4, accDay(20))
		// P0: the window before's plan, three rounds, whose card o1 was
		// first filed then and refiled on day 2 of this window
		plan("t-plan-0", 80)
		filing("t-plan-0", 1, "", 0, "", 3, accDay(-6))
		filing("t-plan-0", 1, "", 99, "o1", 1, accDay(-5))
		filing("t-plan-0", 2, factoryd.ReplanRound, 99, "o1", 2, accDay(2))
		accClosed(t, s, "t-plan-0", "sf/code", accDay(-4))
		// P4 closes after the window and P5 is another repository's
		plan("t-plan-4", 84)
		filing("t-plan-4", 1, "", 0, "", 2, accDay(-40))
		accClosed(t, s, "t-plan-4", "sf/code", accDay(15))
		accPlan(t, s, "t-plan-5", "sf/other", 85)
		accRecord(t, s, factoryd.PlanFilingFact{Plan: "t-plan-5", Revision: 1, Origin: factoryd.ReplanCut, Card: 100, Key: "x1"}.
			Event("sf/other", 85, map[string]any{"round": 5}), accDay(3))
		accClosed(t, s, "t-plan-5", "sf/other", accDay(3))

		rig := newDivideRig(t, s)
		dc := rig.report()
		named := func(name string, value, prev float64, weeks [7]float64) factoryd.NamedFigure {
			return factoryd.NamedFigure{Name: name, Figure: accFig(value, prev, weeks)}
		}
		same(t, "plans_by_rounds", dc.PlansByRounds, []factoryd.NamedCount{{Name: "1", Count: 1}, {Name: "2", Count: 1}, {Name: "unknown", Count: 1}})
		same(t, "plans_by_rounds_spans", dc.PlansByRoundsSpans, []factoryd.NamedFigure{
			named("1", 1, 0, [7]float64{5: 1}), named("2", 1, 0, [7]float64{5: 1}),
			named("3", 0, 1, [7]float64{4: 1}), named("unknown", 1, 0, [7]float64{6: 1})})
		same(t, "cards_by_round", dc.CardsByRound, []factoryd.NamedFigure{
			named("1", 4, 1, [7]float64{4: 1, 5: 4}), named("2", 2, 0, [7]float64{5: 2}),
			named("3", 1, 0, [7]float64{6: 1}), named("unknown", 1, 0, [7]float64{5: 1})})
		same(t, "cards_first_round", dc.CardsFirstRound, accFig(4, 1, [7]float64{4: 1, 5: 4}))
		same(t, "cards_later_rounds", dc.CardsLaterRounds, accFig(3, 0, [7]float64{5: 2, 6: 1}))
		same(t, "replans_by_origin", dc.ReplansByOrigin, []factoryd.NamedCount{
			{Name: "unknown", Count: 3}, {Name: "cut", Count: 1}, {Name: "person", Count: 1}, {Name: "round", Count: 1}})
		same(t, "replans_by_origin_spans", dc.ReplansByOriginSpans, []factoryd.NamedFigure{
			named("cut", 1, 0, [7]float64{5: 1}), named("person", 1, 0, [7]float64{5: 1}),
			named("round", 1, 0, [7]float64{6: 1}), named("unknown", 3, 1, [7]float64{4: 1, 5: 3})})
		body := rig.page()
		holds(t, panel(t, body, "plans by rounds"), []string{"1 round:1 / 2 rounds:1 / 3 rounds:0 / unknown:1", "TestDivideConquerPlanning in acceptance"}, nil)
		holds(t, panel(t, body, "first / later round cards"), []string{"first 4 · later 3 · 1 round:4 / 2 rounds:2 / 3 rounds:1 / unknown:1"}, nil)
		holds(t, panel(t, body, "replans by origin"), []string{"cut:1 / person:1 / round:1 / unknown:3"}, nil)
	})
}

// TestDivideConquerMetricsCompatibility proves the page and its seam
// keep their old figures and gain the section: the heading, the twelve
// named panels and every proof label; the login rule, an unauthorized
// login and an unknown project; the window's dates, their errors, the
// 7, 28 and 90 day links and the default 28 UTC days; and the old JSON
// keys and values unchanged beside the additive divide block, scoped to
// the project, with measured zero, unknown and undefined apart.
func TestDivideConquerMetricsCompatibility(t *testing.T) {
	figs := &accFigures{report: accPanelsReport()}
	figs.report.Throughput = accFig(7, 6, [7]float64{})
	rig := newDivideRig(t, figs)
	body := rig.page()
	holds(t, body, []string{"Divide and conquer (roadmap 336)"}, nil)
	if n := strings.Count(body, "mproof"); n != 12 {
		t.Errorf("%d named panels, want twelve", n)
	}
	for _, label := range []string{
		"TestDivideConquerSizeBounds in acceptance",
		"TestDivideConquerSessions in acceptance",
		"TestDivideConquerEconomics in acceptance",
		"TestDivideConquerCuts in acceptance",
		"TestDivideConquerGroupHours in acceptance",
		"TestDivideConquerProductionHold in acceptance",
		"TestDivideConquerReviewCoverage in acceptance",
		"TestDivideConquerPlanning in acceptance",
	} {
		if !strings.Contains(body, label) {
			t.Errorf("page lacks proof label %q", label)
		}
	}
	holds(t, panel(t, body, "throughput"), []string{">7 cards<"}, nil)
	// the quick spans end on the window's last date and reach back 7, 28 and 90 days
	holds(t, body, []string{
		`/metrics?from=2026-10-05&amp;to=2026-10-11`,
		`/metrics?from=2026-09-14&amp;to=2026-10-11`,
		`/metrics?from=2026-07-14&amp;to=2026-10-11`,
	}, nil)

	// the login rule: no login, a session nobody holds (a login that
	// writes no project is refused at the callback), and a
	// project that does not exist
	path := "/projects/" + accProjectName + "/metrics" + accQuery
	rec := httptest.NewRecorder()
	rig.a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "Divide and conquer") {
		t.Errorf("metrics without the login = %d, want no figures", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: rig.ck.Name, Value: "forged"})
	rec = httptest.NewRecorder()
	rig.a.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "Divide and conquer") {
		t.Errorf("metrics for a session nobody holds = %d, want no figures", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/projects/no-such-project/metrics"+accQuery, nil)
	req.AddCookie(rig.ck)
	rec = httptest.NewRecorder()
	rig.a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("metrics of an unknown project = %d, want 404", rec.Code)
	}
	// the window: exact dates and their errors
	for bad, want := range map[string]string{
		"?from=not-a-date":               "from: want YYYY-MM-DD",
		"?to=2026-13-40":                 "to: want YYYY-MM-DD",
		"?from=2026-10-11&to=2026-09-28": "from is after to",
	} {
		if rec := rig.get(bad); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("window %s = %d %q, want 400 %q", bad, rec.Code, rec.Body, want)
		}
	}
	// without dates the page asks for the default window of 28 UTC days,
	// today included
	figs.asked = nil
	rig.pageQuery("")
	if len(figs.asked) != 1 {
		t.Fatalf("%d figure calls, want the one report", len(figs.asked))
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if want := "sf/code " + today.AddDate(0, 0, -27).Format(time.RFC3339) + " " + today.Format(time.RFC3339); figs.asked[0] != want {
		t.Errorf("default window asked %q, want %q", figs.asked[0], want)
	}

	// the seam over a real store: the old keys keep their values beside the
	// additive block, and only this project's facts count
	s, _ := divideStore(t)
	done := func(id, repository string, issue int64, at time.Time) {
		t.Helper()
		if err := s.CreateTask(t.Context(), factoryd.Task{ID: id, Status: "done", Objective: "the divide panels' fixture",
			Repository: repository, Kind: "agent", Source: "api", Track: "direct", BudgetEUR: 5, IssueNumber: issue,
			CreatedAt: at.Add(-time.Hour), FinishedAt: &at}); err != nil {
			t.Fatal(err)
		}
	}
	done("t-done-1", "sf/code", 1, accDay(2))
	done("t-done-2", "sf/code", 2, accDay(9))
	done("t-done-3", "sf/code", 3, accDay(-3))
	done("t-done-4", "sf/other", 4, accDay(2))
	rig = newDivideRig(t, s)
	report := rig.reportAt(accQuery)
	same(t, "throughput", report.Throughput, accFig(2, 1, [7]float64{4: 1, 5: 1, 6: 1}))
	var raw map[string]json.RawMessage
	if rec := rig.get(accQuery + "&format=json"); json.Unmarshal(rec.Body.Bytes(), &raw) != nil {
		t.Fatal("the seam's json does not decode")
	}
	for _, legacy := range []string{"deploys", "lead_time", "throughput", "spend", "review_acceptance", "divide_conquer"} {
		if _, ok := raw[legacy]; !ok {
			t.Errorf("json lost key %q", legacy)
		}
	}
	page := rig.page()
	holds(t, panel(t, page, "throughput"), []string{">2 cards<"}, nil)
	if n := strings.Count(page, "mproof"); n != 12 {
		t.Errorf("%d named panels over the store, want twelve", n)
	}
	// measured zero, unknown and undefined stay three words: the hold
	// total of a span with one empty week is zero hours, the size bound
	// without a reading unknown, the euros per line undefined
	empty := rig.pageQuery(accSpan(accDay(100), accDay(106)))
	holds(t, panel(t, empty, "production hold hours"), []string{">0.0 hours<"}, nil)
	holds(t, panel(t, empty, "cards over size bound"), []string{">— · target 0<"}, nil)
	holds(t, panel(t, empty, "EUR per merged line"), []string{">—<"}, nil)
	holds(t, panel(t, empty, "cuts / closed cut groups"), []string{">0 / 0<"}, nil)
	holds(t, panel(t, empty, "throughput"), []string{">0 cards<"}, nil)
}
