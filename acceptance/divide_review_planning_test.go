package acceptance

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

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
		s, _ := divideStore(t)
		from, _ := divideWindow()
		accCard(t, s, "t-review", "sf/code", "small", "hr1", 55)
		review := func(commit string, round int, verdict string, at time.Time) int64 {
			return accRecord(t, s, factoryd.ReviewAttemptFact{PR: 9, Commit: commit, Round: round,
				Model: "big", Verdict: verdict, Note: "the answer's bounded note"}.
				Event("t-review", "sf/code", 55), at)
		}
		at := from.Add(time.Hour)
		// two answers never judged the head, one approved it: 2 of 3.
		review("rc1", 1, "", at)
		review("rc2", 1, "", at)
		review("rc3", 1, "approve", at)
		// the retried attempt answers id 0: one attempt, one row.
		if id := review("rc1", 1, "", at.Add(time.Minute)); id != 0 {
			t.Errorf("retried review answered id %d, want 0", id)
		}
		// an answer before the window counts only the previous span.
		review("rc0", 1, "", from.Add(-time.Hour))
		// a row no identity covers is unknown evidence, never an
		// inferred success.
		accRecord(t, s, factoryd.Event{Repository: "sf/code", TaskID: "t-review", IssueNumber: 55,
			Kind: factoryd.EventModelReview, Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd,
			Detail: map[string]any{"reviewed": false}}, at.Add(2*time.Hour))
		rig := newDivideRig(t, s)
		holds(t, rig.page(), []string{
			">2 of 3 · target 0<",
			"1 unknown",
			"TestDivideConquerReviewCoverage in acceptance",
		}, nil)
	})
}

func TestDivideConquerPlanning(t *testing.T) {
	t.Run("fake", func(t *testing.T) {
		rig := newDivideRig(t, &accFigures{report: accPanelsReport()})
		body := rig.page()
		holds(t, body, []string{
			"1 round:1 / 2 rounds:1",
			"first 4 · later 3",
			"person:1 / cut:1 / round:1 / unknown:2",
			"TestDivideConquerPlanning in acceptance",
		}, nil)
		if n := strings.Count(body, "TestDivideConquerPlanning in acceptance"); n != 3 {
			t.Errorf("proof label prints %d times, want 3", n)
		}
	})

	t.Run("store", func(t *testing.T) {
		s, _ := divideStore(t)
		from, _ := divideWindow()
		at := from.Add(time.Hour)
		// filing files one plan revision: its own row on the plan, a row
		// per card it filed, each stamped with its round.
		filing := func(plan string, revision int, origin string, card int64, key string, issue int64, round int, at time.Time) {
			extra := map[string]any{}
			if round > 0 {
				extra["round"] = round
			}
			accRecord(t, s, factoryd.PlanFilingFact{Plan: plan, Revision: revision, Origin: origin,
				Card: card, Key: key}.Event("sf/code", issue, extra), at)
		}
		// plan one runs three rounds — four cards the first round filed,
		// two the second, one the third — its second round a person's
		// replan, its third a cut's.
		accPlan(t, s, "t-plan-1", "sf/code", 81)
		filing("t-plan-1", 1, "", 0, "", 81, 1, at)
		for _, card := range []int64{91, 92, 93, 94} {
			filing("t-plan-1", 1, "", card, fmt.Sprintf("card-%d", card), 91, 1, at)
		}
		filing("t-plan-1", 2, factoryd.ReplanPerson, 0, "", 81, 2, at.Add(time.Hour))
		filing("t-plan-1", 2, factoryd.ReplanPerson, 95, "card-95", 91, 2, at.Add(time.Hour))
		filing("t-plan-1", 2, factoryd.ReplanPerson, 96, "card-96", 91, 2, at.Add(time.Hour))
		filing("t-plan-1", 3, factoryd.ReplanCut, 0, "", 81, 3, at.Add(2*time.Hour))
		filing("t-plan-1", 3, factoryd.ReplanCut, 97, "card-97", 91, 3, at.Add(2*time.Hour))
		accClosed(t, s, "t-plan-1", "sf/code", at.Add(3*time.Hour))
		// plan two runs two rounds, its second a round's own replan.
		accPlan(t, s, "t-plan-2", "sf/code", 82)
		filing("t-plan-2", 1, "", 0, "", 82, 1, at)
		filing("t-plan-2", 2, factoryd.ReplanRound, 0, "", 82, 2, at.Add(time.Hour))
		accClosed(t, s, "t-plan-2", "sf/code", at.Add(3*time.Hour))
		rig := newDivideRig(t, s)
		holds(t, rig.page(), []string{
			"first 4 · later 3",
			"person:1",
			"cut:1",
			"round:1",
			"2 rounds:1",
			"3 rounds:1",
			"TestDivideConquerPlanning in acceptance",
		}, nil)
	})
}

func TestDivideConquerMetricsCompatibility(t *testing.T) {
	figs := &accFigures{report: accPanelsReport()}
	rig := newDivideRig(t, figs)
	body := rig.page()
	// the section's own heading, twelve named panels, every literal
	// proof label — and the page stands only behind the member login.
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
	noAuth := httptest.NewRequest(http.MethodGet, "/projects/"+accProjectName+"/metrics"+accQuery, nil)
	rec := httptest.NewRecorder()
	rig.a.Handler().ServeHTTP(rec, noAuth)
	if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "Divide and conquer") {
		t.Errorf("metrics without the login = %d, want no figures", rec.Code)
	}
	// the window: exact dates, their errors, and the quick spans' links.
	for bad, want := range map[string]string{
		"?from=not-a-date":               "from: want YYYY-MM-DD",
		"?from=2026-10-11&to=2026-09-28": "from is after to",
	} {
		req := httptest.NewRequest(http.MethodGet, "/projects/"+accProjectName+"/metrics"+bad, nil)
		req.AddCookie(rig.ck)
		rec := httptest.NewRecorder()
		rig.a.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("window %s = %d %q, want 400 %q", bad, rec.Code, rec.Body, want)
		}
	}
	if n := strings.Count(body, "/metrics?from="); n < 3 {
		t.Errorf("%d quick-span links, want the 7, 28 and 90 day ones", n)
	}
	// without dates the page asks for the default window of 28 UTC days,
	// today included — and one report answers the prior span beside it.
	figs.asked = nil
	rig.pageQuery("")
	if len(figs.asked) != 1 {
		t.Fatalf("%d figure calls, want the one report", len(figs.asked))
	}
	now := time.Now().UTC()
	wantFrom, wantTo := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -27),
		time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if got := figs.asked[0]; got != "sf/code "+wantFrom.Format(time.RFC3339)+" "+wantTo.Format(time.RFC3339) {
		t.Errorf("default window asked %q", got)
	}

	// the seam stays additive: the legacy panels keep their names, the
	// divide figure carries its prior span and its seven weeks beside
	// its value.
	jsonBody := map[string]any{}
	dc, ok := jsonBody["divide_conquer"].(map[string]any)
	if !ok {
		t.Fatalf("json lacks divide_conquer: keys %v", keysOf(jsonBody))
	}
	for _, legacy := range []string{"deploys", "lead_time", "review_acceptance"} {
		if _, ok := jsonBody[legacy]; !ok {
			t.Errorf("json lost legacy key %q", legacy)
		}
	}
	for _, name := range []string{"eur_per_merged_line", "gate_above_bound", "cuts"} {
		fig, ok := dc[name].(map[string]any)
		if !ok {
			t.Fatalf("divide_conquer lacks %s: keys %v", name, keysOf(dc))
		}
		if weeks, ok := fig["weeks"].([]any); !ok || len(weeks) != 7 {
			t.Errorf("%s weeks = %v, want seven", name, fig["weeks"])
		}
	}
}

// keysOf names a decoded object's keys for a failure message.
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
