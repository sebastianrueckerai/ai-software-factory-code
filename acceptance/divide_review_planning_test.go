package acceptance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

func TestDivideConquerReviewCoverage(t *testing.T) {
	t.Run("fake", func(t *testing.T) {
		rig, _ := NewMetricsRig(t, &accFigures{report: accPanelsReport()})
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
		rig, _ := NewMetricsRig(t, s)
		holds(t, rig.page(), []string{
			">2 of 3 · target 0<",
			"1 unknown",
			"TestDivideConquerReviewCoverage in acceptance",
		}, nil)
	})
}

func TestDivideConquerPlanning(t *testing.T) {
	t.Run("fake", func(t *testing.T) {
		rig, _ := NewMetricsRig(t, &accFigures{report: accPanelsReport()})
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
		filing := func(plan string, revision int, origin string, card int64, key string, issue int64, at time.Time) {
			accRecord(t, s, factoryd.PlanFilingFact{Plan: plan, Revision: revision, Origin: origin,
				Card: card, Key: key}.Event("sf/code", issue, nil), at)
		}
		// plan one runs three rounds: four cards the first round filed,
		// two the second, one the third — its replan a person's, its
		// third round's.
		accPlan(t, s, "t-plan-1", "sf/code", 81)
		filing("t-plan-1", 1, "", 0, "", 81, at)
		for _, card := range []int64{91, 92, 93, 94} {
			filing("t-plan-1", 1, "", card, "card", 91, at)
		}
		filing("t-plan-1", 2, factoryd.ReplanPerson, 0, "", 81, at.Add(time.Hour))
		filing("t-plan-1", 2, factoryd.ReplanPerson, 95, "card", 91, at.Add(time.Hour))
		filing("t-plan-1", 2, factoryd.ReplanPerson, 96, "card", 91, at.Add(time.Hour))
		filing("t-plan-1", 3, factoryd.ReplanCut, 0, "", 81, at.Add(2*time.Hour))
		filing("t-plan-1", 3, factoryd.ReplanCut, 97, "card", 91, at.Add(2*time.Hour))
		accClosed(t, s, "t-plan-1", "sf/code", at.Add(3*time.Hour))
		// plan two runs two rounds, its replan a round's.
		accPlan(t, s, "t-plan-2", "sf/code", 82)
		filing("t-plan-2", 1, "", 0, "", 82, at)
		filing("t-plan-2", 2, "round", 0, "", 82, at.Add(time.Hour))
		accClosed(t, s, "t-plan-2", "sf/code", at.Add(3*time.Hour))
		rig, _ := NewMetricsRig(t, s)
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
	rig, _ := NewMetricsRig(t, &accFigures{report: accPanelsReport()})
	body := rig.jsonBody()
	dc, ok := body["divide_conquer"].(map[string]any)
	if !ok {
		t.Fatalf("json lacks divide_conquer: keys %v", keysOf(body))
	}
	// the seam stays additive: the legacy panels keep their names.
	for _, legacy := range []string{"deploys", "lead_time", "review_acceptance"} {
		if _, ok := body[legacy]; !ok {
			t.Errorf("json lost legacy key %q", legacy)
		}
	}
	fig, ok := dc["eur_per_merged_line"].(map[string]any)
	if !ok {
		t.Fatalf("divide_conquer lacks eur_per_merged_line: keys %v", keysOf(dc))
	}
	weeks, ok := fig["weeks"].([]any)
	if !ok || len(weeks) != 7 {
		t.Errorf("eur_per_merged_line weeks = %v, want seven", weeks)
	}
	if _, ok := dc["gate_bound_coverage"]; !ok {
		t.Error("divide_conquer lacks gate_bound_coverage")
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
