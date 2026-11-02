package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// fakeFigures is the metrics seam: it answers the report it was seeded
// with and records the windows it was asked for.
type fakeFigures struct {
	report factoryd.MetricsReport
	asked  []string // "repository from to" per ProjectMetrics
	fail   error
}

func (f *fakeFigures) ProjectMetrics(_ context.Context, repository string, from, to time.Time) (factoryd.MetricsReport, error) {
	f.asked = append(f.asked, repository+" "+from.Format(time.RFC3339)+" "+to.Format(time.RFC3339))
	if f.fail != nil {
		return factoryd.MetricsReport{}, f.fail
	}
	return f.report, nil
}

// SetMetrics answers the figures the lab's scene seeds.
func (f *fakeFigures) SetMetrics(m factoryd.MetricsReport) { f.report = m }

// metricsFiguresReport is one report the seam answers whole: every figure
// carries a value that survives the JSON round trip.
func metricsFiguresReport() factoryd.MetricsReport {
	return factoryd.MetricsReport{
		Repository: "sf/code",
		From:       time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC),
		To:         time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC),
		Deploys:    factoryd.Figure{Value: 2, Prev: 2, Has: true, PrevHas: true, Weeks: [7]float64{1, 0, 0, 1, 1, 0, 2}},
		LeadTime:   factoryd.Figure{Value: 3570000, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 3570000}},
	}
}

// metricsBodyKeys are the fields the metrics JSON has carried since the
// deck first shipped; the divide-and-conquer block adds to them, it
// never renames or drops one.
var metricsBodyKeys = []string{"repository", "from", "to", "deploys", "lead_time", "card_cycle", "throughput",
	"change_failure", "reverted_refiled", "time_to_restore", "spend", "per_card", "tokens_per_card", "model_spend", "served_by", "tool_spend", "tool_mix",
	"interventions", "escalations", "escalated_share", "escalations_handled", "human_blocked", "review_acceptance", "self_deploys", "diff_size",
	"chat_tool_work", "ci_duration"}

func TestProjectMetricsJSON(t *testing.T) {
	r := newRig(t)
	r.figures.report = metricsFiguresReport()
	ck, _ := r.loggedIn(t)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/metrics?format=json", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET metrics = %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range metricsBodyKeys {
		if _, ok := body[key]; !ok {
			t.Errorf("body lacks %s", key)
		}
	}
	if body["repository"] != "sf/code" {
		t.Errorf("repository = %v", body["repository"])
	}
	deploys, ok := body["deploys"].(map[string]any)
	if !ok {
		t.Fatalf("deploys = %v", body["deploys"])
	}
	if v, ok := deploys["value"].(float64); !ok || v != 2 {
		t.Errorf("deploys.value = %v", deploys["value"])
	}
	weeks, ok := deploys["weeks"].([]any)
	if !ok || len(weeks) != 7 || weeks[6] != float64(2) {
		t.Errorf("deploys.weeks = %v", deploys["weeks"])
	}
	today := time.Now().UTC()
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	wantAsk := "sf/code " + today.AddDate(0, 0, -27).Format(time.RFC3339) + " " + today.Format(time.RFC3339)
	if len(r.figures.asked) != 1 || r.figures.asked[0] != wantAsk {
		t.Fatalf("asked = %v, want %q", r.figures.asked, wantAsk)
	}
}

// TestDivideConquerMetricsContract pins the additive divide-and-conquer
// block: the asked window's dates reach the figures port, every field
// the JSON has always carried survives, the page keeps its shape with
// the block in the report, and the block keeps unknown, undefined and
// zero apart. The fixture's window sits inside its CoveredFrom: the
// measured values are covered ones; the zero CoveredFrom of
// pre-coverage reads as the zero time, unknown, its own check below.
func TestDivideConquerMetricsContract(t *testing.T) {
	r := newRig(t)
	report := metricsFiguresReport()
	report.DivideConquer = factoryd.DivideConquer{
		CoveredFrom: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC),
		// a target panel's measured zero carries has=true
		GateAboveBound:     factoryd.Figure{Value: 0, Has: true, PrevHas: true},
		ReviewsNotReviewed: factoryd.Figure{Value: 1, Has: true},
		// the review panel's evidence: the denominator the count above
		// stands over and the hole no usable evidence covers
		ReviewEvidence: factoryd.ReviewEvidence{
			Answers: factoryd.Figure{Value: 3, Has: true},
			Unknown: factoryd.Figure{Value: 1, Has: true},
		},
		SessionsPerCard: []factoryd.SizeFigure{
			{Size: "small", Figure: factoryd.Figure{Value: 1.2, Has: true}},
			{Size: "medium", Figure: factoryd.Figure{Value: 2.4, Has: true}},
		},
		SessionEnds: []factoryd.NamedCount{
			{Name: "pushed", Count: 18},
			{Name: "wall_clock", Count: 2},
		},
		// no merged line in the window: undefined, never zero
		EurPerMergedLine: factoryd.Figure{},
		FixedEurPerCard: []factoryd.SizeIntercept{
			{Size: "small", EUR: 2.1, Available: true},
			{Size: "large", Available: false}, // too few cards to fit
		},
		Cuts:             factoryd.Figure{Value: 3, Has: true},
		CutGroupsClosed:  factoryd.Figure{Value: 2, Has: true},
		GroupOpenHours:   factoryd.Figure{Value: 96.5, Has: true},
		HoldWaitHours:    factoryd.Figure{Value: 7.25, Has: true},
		PlansByRounds:    []factoryd.NamedCount{{Name: "1", Count: 6}, {Name: "2", Count: 2}},
		CardsFirstRound:  factoryd.Figure{Value: 5.5, Has: true},
		CardsLaterRounds: factoryd.Figure{Value: 2, Has: true},
		ReplansByOrigin:  []factoryd.NamedCount{{Name: "person", Count: 1}, {Name: "round", Count: 3}},
	}
	r.figures.report = report
	ck, _ := r.loggedIn(t)
	// the window lies inside the block's coverage, so the values the
	// fixture pins are measured ones
	rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/metrics?format=json&from=2026-10-29&to=2026-10-30", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET metrics = %d: %s", rec.Code, rec.Body)
	}
	if last := r.figures.asked[len(r.figures.asked)-1]; last != "sf/code 2026-10-29T00:00:00Z 2026-10-30T00:00:00Z" {
		t.Fatalf("asked = %q, want the covered window's dates", last)
	}
	body := rec.Body.Bytes()
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range metricsBodyKeys {
		if _, ok := raw[key]; !ok {
			t.Errorf("body lacks %s", key)
		}
	}
	dc, ok := raw["divide_conquer"].(map[string]any)
	if !ok {
		t.Fatalf("divide_conquer = %v", raw["divide_conquer"])
	}
	for _, key := range []string{"covered_from", "gate_above_bound", "sessions_per_card", "session_ends",
		"eur_per_merged_line", "fixed_eur_per_card", "cuts", "cut_groups_closed", "group_open_hours",
		"hold_wait_hours", "reviews_not_reviewed", "review_evidence", "plans_by_rounds", "cards_first_round",
		"cards_later_rounds", "replans_by_origin"} {
		if _, ok := dc[key]; !ok {
			t.Errorf("divide_conquer lacks %s", key)
		}
	}
	// covered: the block names the moment its facts start
	if got := dc["covered_from"]; got != "2026-10-29T00:00:00Z" {
		t.Errorf("covered_from = %v, want 2026-10-29T00:00:00Z", got)
	}
	// zero: a target's none is a measured zero, not a dash
	gate := dc["gate_above_bound"].(map[string]any)
	if gate["value"] != float64(0) || gate["has"] != true {
		t.Errorf("gate_above_bound = %v, want a measured zero", gate)
	}
	// evidence: the review count's denominator and its hole survive the
	// boundary apart — the answers a fact even at zero, the unknown rows
	// never an inferred success
	ev := dc["review_evidence"].(map[string]any)
	if answers := ev["answers"].(map[string]any); answers["value"] != float64(3) || answers["has"] != true {
		t.Errorf("review_evidence answers = %v, want the measured denominator 3", answers)
	}
	if holes := ev["unknown"].(map[string]any); holes["value"] != float64(1) || holes["has"] != true {
		t.Errorf("review_evidence unknown = %v, want the named hole", holes)
	}
	// undefined: no merged line and an unfit intercept read absent,
	// never zero
	eur := dc["eur_per_merged_line"].(map[string]any)
	if eur["has"] != false || eur["value"] != float64(0) {
		t.Errorf("eur_per_merged_line = %v, want undefined", eur)
	}
	fixed := dc["fixed_eur_per_card"].([]any)
	small, large := fixed[0].(map[string]any), fixed[1].(map[string]any)
	if small["eur"] != float64(2.1) || small["available"] != true {
		t.Errorf("fixed_eur_per_card small = %v", small)
	}
	if large["available"] != false || large["eur"] != float64(0) {
		t.Errorf("fixed_eur_per_card large = %v, want unavailable", large)
	}
	// hours stay hours, fraction and all, and the typed distributions
	// keep their names
	if hours := dc["group_open_hours"].(map[string]any)["value"]; hours != float64(96.5) {
		t.Errorf("group_open_hours = %v, want 96.5", hours)
	}
	if first := dc["session_ends"].([]any)[0].(map[string]any); first["name"] != "pushed" || first["count"] != float64(18) {
		t.Errorf("session_ends[0] = %v", first)
	}
	if o := dc["replans_by_origin"].([]any)[1].(map[string]any); o["name"] != "round" || o["count"] != float64(3) {
		t.Errorf("replans_by_origin[1] = %v", o)
	}
	// the block and the deck's fields survive the round trip whole
	var back factoryd.MetricsReport
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if !reflect.DeepEqual(back.DivideConquer, report.DivideConquer) {
		t.Fatalf("divide block lost shape: %+v", back.DivideConquer)
	}
	if back.Repository != report.Repository || back.Deploys != report.Deploys || back.LeadTime != report.LeadTime {
		t.Errorf("existing fields changed: %+v", back)
	}
	// the page renders with the block in the report: the deck's sections
	// and rows stand, the additive block disturbs none of them (the
	// block's own panels are the console-metric-panels card's)
	page := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/metrics", nil), ck)
	if page.Code != http.StatusOK {
		t.Fatalf("GET metrics page = %d: %s", page.Code, page.Body)
	}
	for _, want := range []string{`class="page page-metrics"`, "Delivery", "Stability", "Cost", "Autonomy",
		"deploys / week", "lead time, push to live", `class="mname" title="`} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// unknown: before coverage the block names no moment — the zero time,
	// a date no panel can mistake for coverage it has
	r.figures.report.DivideConquer = factoryd.DivideConquer{}
	rec = r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/metrics?format=json", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET metrics pre-coverage = %d: %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode pre-coverage: %v", err)
	}
	if dc2, ok := raw["divide_conquer"].(map[string]any); !ok || dc2["covered_from"] != "0001-01-01T00:00:00Z" {
		t.Errorf("pre-coverage covered_from = %v, want the zero time", raw["divide_conquer"])
	}
}

// dividePanelsReport seeds every divide-and-conquer panel with a
// measured answer: the twelve panels' values, their coverage holes and
// their named comparisons, precise milliseconds beside the legacy hour
// fields the JSON keeps.
func dividePanelsReport() factoryd.DivideConquer {
	move := func(value, prev float64, weeks [7]float64) factoryd.Figure {
		return factoryd.Figure{Value: value, Prev: prev, Has: true, PrevHas: true, Weeks: weeks}
	}
	count := func(value float64) factoryd.Figure {
		return factoryd.Figure{Value: value, Has: true}
	}
	return factoryd.DivideConquer{
		CoveredFrom: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC),
		// panel 1: one card over its bound, against the target of none
		GateAboveBound:    move(1, 0, [7]float64{0, 1, 0, 0, 0, 0, 1}),
		GateBoundCoverage: factoryd.GateBoundCoverage{Readings: 3, Measured: 2, UnknownSize: 1},
		// panel 2: two sessions per merged card, small and medium alike
		SessionsPerCard: []factoryd.SizeFigure{
			{Size: "small", Figure: move(2.0, 1.0, [7]float64{1, 2, 2, 1, 2, 2, 2})},
			{Size: "medium", Figure: move(2.0, 2.5, [7]float64{2, 2, 3, 2, 2, 2, 2})},
		},
		SessionsCoverage: factoryd.SessionsCoverage{Cards: 5, UnknownSize: 1, MissingStarts: 1},
		// panel 3: the causes the runner's outcomes name
		SessionEnds: []factoryd.NamedCount{{Name: "pushed", Count: 2}, {Name: "wall_clock", Count: 1}},
		// panels 4 and 5: the window's economics
		EurPerMergedLine: move(0.02, 0.01, [7]float64{0.01, 0.02, 0.02, 0.01, 0.02, 0.02, 0.02}),
		FixedEurPerCard: []factoryd.SizeIntercept{
			{Size: "small", EUR: 2, Available: true, Cards: 3},
			{Size: "medium", EUR: 3, Available: true, Cards: 4},
			{Size: "large", Available: false},
		},
		EconomicsCoverage: factoryd.EconomicsCoverage{Cards: 7, MissingCost: 1, MissingLines: 1, MissingSize: 1},
		// panel 6: two cuts, one cut group the window saw closed
		Cuts:            move(2, 1, [7]float64{0, 1, 0, 1, 0, 0, 2}),
		CutGroupsClosed: count(1),
		// panel 7: 21,600,000 ms is the 6.0 hours the panel words
		GroupOpenMillis:    move(21.6e6, 7.2e6, [7]float64{5e6, 6e6, 7e6, 6e6, 5e6, 6e6, 6e6}),
		GroupOpenHours:     move(6, 2, [7]float64{5, 6, 7, 6, 5, 6, 6}),
		GroupHoursCoverage: factoryd.GroupHoursCoverage{Groups: 2, Open: 1, MissingStart: 1},
		// panel 8: 13,500,000 ms is the 3.75 hours the panel words
		HoldWaitMillis:   move(13.5e6, 0, [7]float64{0, 0, 13.5e6, 0, 0, 0, 13.5e6}),
		HoldWaitHours:    move(3.75, 0, [7]float64{0, 0, 3.75, 0, 0, 0, 3.75}),
		HoldWaitCoverage: factoryd.HoldWaitCoverage{Ended: 2, Open: 1, Missing: 1},
		// panel 9: two not-reviewed answers over three genuine ones
		ReviewsNotReviewed: count(2),
		ReviewEvidence: factoryd.ReviewEvidence{
			Answers: count(3),
			Unknown: count(1),
		},
		// panels 10 to 12: the named comparisons, current, prior and all
		// seven weeks
		PlansByRoundsSpans: []factoryd.NamedFigure{
			{Name: "1", Figure: move(1, 2, [7]float64{2, 1, 0, 0, 0, 1, 1})},
			{Name: "2", Figure: move(1, 0, [7]float64{0, 0, 1, 0, 0, 0, 1})},
		},
		CardsFirstRound:  count(4),
		CardsLaterRounds: count(3),
		CardsByRound: []factoryd.NamedFigure{
			{Name: "1", Figure: move(4, 3, [7]float64{1, 1, 1, 0, 1, 1, 1})},
			{Name: "2", Figure: move(2, 1, [7]float64{0, 1, 0, 0, 0, 1, 1})},
			{Name: "3", Figure: move(1, 0, [7]float64{0, 0, 0, 0, 0, 0, 1})},
		},
		ReplansByOriginSpans: []factoryd.NamedFigure{
			{Name: "person", Figure: move(1, 0, [7]float64{0, 1, 0, 0, 0, 0, 1})},
			{Name: "cut", Figure: move(1, 0, [7]float64{0, 0, 1, 0, 0, 0, 1})},
			{Name: "round", Figure: move(1, 1, [7]float64{0, 0, 0, 1, 0, 0, 1})},
			{Name: "unknown", Figure: move(2, 1, [7]float64{0, 0, 0, 0, 1, 1, 1})},
		},
	}
}

// TestDivideConquerMetricPanels drives the real authenticated metrics
// route over the fake figures seam and pins the twelve
// divide-and-conquer panels (roadmap 336): the literal heading, every
// panel's proof label and own numbers — the named comparisons' current,
// prior and seven-week figures among them — the three states of
// measured zero, unknown evidence and undefined kept apart, and the old
// deck, the auth floor and the additive JSON untouched.
func TestDivideConquerMetricPanels(t *testing.T) {
	r := newRig(t)
	report := metricsFiguresReport()
	report.DivideConquer = dividePanelsReport()
	r.figures.report = report
	ck, _ := r.loggedIn(t)
	page := func() string {
		t.Helper()
		rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/metrics", nil), ck)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET metrics page = %d: %s", rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	body := page()
	if !strings.Contains(body, "Divide and conquer (roadmap 336)") {
		t.Error("page lacks the literal divide-and-conquer heading")
	}
	// exactly twelve panels, each printing its proof label
	if n := strings.Count(body, `class="mproof"`); n != 12 {
		t.Errorf("%d panels print a proof label, want 12", n)
	}
	for proof, want := range map[string]int{
		"TestDivideConquerSizeBounds in acceptance":     1,
		"TestDivideConquerSessions in acceptance":       2,
		"TestDivideConquerEconomics in acceptance":      2,
		"TestDivideConquerCuts in acceptance":           1,
		"TestDivideConquerGroupHours in acceptance":     1,
		"TestDivideConquerProductionHold in acceptance": 1,
		"TestDivideConquerReviewCoverage in acceptance": 1,
		"TestDivideConquerPlanning in acceptance":       3,
	} {
		if n := strings.Count(body, proof); n != want {
			t.Errorf("proof %q prints %d times, want %d", proof, n, want)
		}
	}
	// every panel's own numbers, its coverage and its named detail
	for _, want := range []string{
		">1 · target 0<", // panel 1: one card over, the target none
		"3 readings · 2 measured · 1 unknown size",    // panel 1's evidence, its hole named
		"small 2.0 · medium 2.0",                      // panel 2's per-size averages
		"5 cards · 1 unknown size · 1 missing starts", // panel 2's cohort and holes
		"pushed 2 · wall_clock 1",                     // panel 3's causes
		"€0.0200",                                     // panel 4's euros per merged line
		"7 cards · 1 missing cost · 1 missing lines · 1 missing size",
		"small 2.00 / medium 3.00 / large unavailable", // panel 5's fits, unavailable never zero
		">2 / 1<",                                // panel 6's cuts and closed cut groups
		">6.0 hours<",                            // panel 7's mean from precise milliseconds
		"cohort 2 · open 1 · missing start 1",    // panel 7's open and missing coverage apart
		">3.75 hours<",                           // panel 8's total from precise milliseconds
		"ended 2 · open 1 · missing 1",           // panel 8's evidence
		">2 of 3 · target 0<",                    // panel 9's count, denominator and target
		"1 unknown · ",                           // panel 9's unknown evidence, never a success
		"1 round:1 / 2 rounds:1",                 // panel 10's rounds, from the named comparison
		"first 4 · later 3",                      // panel 11's first and later totals
		"1 round:4 / 2 rounds:2 / 3 rounds:1",    // panel 11's per-round detail, uncollapsed
		"person:1 / cut:1 / round:1 / unknown:2", // panel 12's origins, unknown apart
		`class="mmove">−1<`,                      // a named round's move against the prior window
		`class="mmove">&#43;4.0 hours<`,          // panel 7's move, hours out of milliseconds
		`viewBox="0 0 240 30"`,                   // the named rounds keep their sparks
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// the old deck renders unchanged beside the panels
	for _, want := range []string{"Delivery", "Stability", "Cost", "Autonomy", "The factory itself",
		"deploys / week", "chat tool work", `<span class="mval">2</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// the three states never collapse: undefined reads its dash, a hole
	// its own label, and missing evidence can never masquerade as the
	// target met
	r.figures.report.DivideConquer.GateAboveBound = factoryd.Figure{}
	r.figures.report.DivideConquer.EurPerMergedLine = factoryd.Figure{}
	body = page()
	if !strings.Contains(body, ">— · target 0<") {
		t.Error("an uncovered target reads a dash, not a zero")
	}
	if strings.Contains(body, ">0 · target 0<") {
		t.Error("missing evidence masquerades as the target met")
	}
	if !strings.Contains(body, `<span class="mval">—</span>`) {
		t.Error("an undefined rate reads a dash, not a zero")
	}
	r.figures.report.DivideConquer.GateAboveBound = factoryd.Figure{Value: 0, Has: true}
	body = page()
	if !strings.Contains(body, ">0 · target 0<") || strings.Contains(body, ">— · target 0<") {
		t.Error("a measured zero reads 0, apart from the dash and the hole")
	}
	// the JSON stays additive: the deck's keys stand and the block keeps
	// its legacy hour semantics beside the precise milliseconds the page
	// reads
	rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/metrics?format=json", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET metrics json = %d: %s", rec.Code, rec.Body)
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range metricsBodyKeys {
		if _, ok := raw[key]; !ok {
			t.Errorf("body lacks %s", key)
		}
	}
	dc, ok := raw["divide_conquer"].(map[string]any)
	if !ok {
		t.Fatalf("divide_conquer = %v", raw["divide_conquer"])
	}
	if ms := dc["hold_wait_milliseconds"].(map[string]any); ms["value"] != float64(13500000) {
		t.Errorf("hold_wait_milliseconds = %v, want the precise 13500000", ms["value"])
	}
	if h := dc["hold_wait_hours"].(map[string]any); h["value"] != float64(3.75) {
		t.Errorf("hold_wait_hours = %v, want the legacy 3.75", h["value"])
	}
	// the authenticated scoping stands: no session redirects to the
	// login, a writer outside the project is refused
	rec = r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/metrics", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("unauthenticated = %d %q, want the login redirect", rec.Code, rec.Header().Get("Location"))
	}
	r.repoWriters["sf/code"] = []string{"someone-else"}
	r.forgetWriters()
	if rec = r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/metrics", nil), ck); rec.Code != http.StatusForbidden {
		t.Errorf("non-writer = %d, want 403", rec.Code)
	}
}

func TestProjectMetricsWindowAndRefusals(t *testing.T) {
	r := newRig(t)
	r.figures.report = metricsFiguresReport()
	ck, _ := r.loggedIn(t)
	get := func(raw string) *httptest.ResponseRecorder {
		t.Helper()
		return r.do(httptest.NewRequest(http.MethodGet, raw, nil), ck)
	}
	rec := get("/projects/ai-software-factory-code/metrics?format=json&from=2026-10-19&to=2026-10-20")
	if rec.Code != http.StatusOK {
		t.Fatalf("windowed = %d: %s", rec.Code, rec.Body)
	}
	if last := r.figures.asked[len(r.figures.asked)-1]; last != "sf/code 2026-10-19T00:00:00Z 2026-10-20T00:00:00Z" {
		t.Fatalf("asked = %q", last)
	}
	for _, raw := range []string{
		"/projects/ai-software-factory-code/metrics?format=json&from=nonsense",
		"/projects/ai-software-factory-code/metrics?format=json&to=2026-10-1x",
		"/projects/ai-software-factory-code/metrics?format=json&from=2026-10-20&to=2026-10-19",
	} {
		if rec := get(raw); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d: %s", raw, rec.Code, rec.Body)
		}
	}
	if rec := get("/projects/ai-software-factory-code/metrics?format=json&from=nonsense"); rec.Body.String() != "from: want YYYY-MM-DD\n" {
		t.Errorf("from refusal = %q", rec.Body)
	}
	if rec := get("/projects/no-such/metrics?format=json"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown project = %d, want 404", rec.Code)
	}
}

// A figures read that fails answers 500: the page and the JSON share
// the one refusal.
func TestProjectMetricsAnswers500WhenTheFiguresRefuse(t *testing.T) {
	r := newRig(t)
	r.figures.fail = errors.New("figures down")
	ck, _ := r.loggedIn(t)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/metrics", nil), ck)
	if rec.Code != http.StatusInternalServerError || strings.TrimSpace(rec.Body.String()) != "cannot read the figures" {
		t.Fatalf("metrics = %d: %s", rec.Code, rec.Body)
	}
}

func TestProjectMetricsAnswers403ForNonWriter(t *testing.T) {
	r := newRig(t)
	r.repoWriters["sf/code"] = []string{"someone-else"}
	r.forgetWriters()
	ck, _ := r.loggedIn(t)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/metrics?format=json", nil), ck)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-writer = %d, want 403", rec.Code)
	}
}

// TestProjectMetricsPage pins the page's structure: the deck's five
// sections and 22 row names, the window control, the measure, the open
// page's word, and one figure rendered from the report.
func TestProjectMetricsPage(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/metrics", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET metrics page = %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	// every row's name carries its one-sentence explanation as a tooltip:
	// the deck's 22 rows and the divide section's twelve panel rows (the
	// panels' named detail rows carry no tooltip of their own)
	if n := strings.Count(body, `class="mname" title="`); n != 34 {
		t.Fatalf("%d row names carry a title, want 34", n)
	}
	wants := []string{
		`class="page page-metrics"`,
		"Delivery", "Stability", "Cost", "Autonomy", "The factory itself",
		"deploys / week", "lead time, push to live", "card cycle", "throughput",
		"change failure", "reverted · refiled", "time to restore",
		"spend / week", "per card, median", "tokens / card", "spend by model", "served by", "tools / week", "tool mix",
		"interventions / card", "escalations", "escalations handled", "human-blocked", "review acceptance",
		"self-deploys / week", "diff size, median", "chat tool work",
		"How the project runs its work, over the window and against the one before.",
		`aria-pressed="true">28 days`,
		`>Metrics</a>`,
		`<span class="mval">2</span>`, // the report's deploys value renders
		`class="mval">—`,              // an absent median reads as a dash
		`viewBox="0 0 240 30"`,
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, word := range []string{"7 days", "28 days", "90 days"} {
		if !strings.Contains(body, ">"+word) {
			t.Errorf("page lacks window word %q", word)
		}
	}
	// a figure with a previous window carries its move; one without shows
	// none — there is nothing to move against
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.Contains(line, ">deploys / week<"):
			if !strings.Contains(line, "mmove") {
				t.Errorf("deploys row lacks its move: %s", line)
			}
		case strings.Contains(line, ">lead time, push to live<"):
			if strings.Contains(line, "mmove") {
				t.Errorf("lead time row shows a move without a previous window: %s", line)
			}
		}
	}
	if strings.Contains(body, `aria-pressed="true">7 days`) || strings.Contains(body, `aria-pressed="true">90 days`) {
		t.Error("an inactive window word carries aria-pressed=true")
	}
	today := time.Now().UTC()
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	want7 := fmt.Sprintf(`href="/projects/ai-software-factory-code/metrics?from=%s&amp;to=%s"`,
		today.AddDate(0, 0, -6).Format("2006-01-02"), today.Format("2006-01-02"))
	if !strings.Contains(body, want7) {
		t.Errorf("page lacks the 7-day link %q", want7)
	}
}

// TestMetricsFormatters pins every deck string the rows read from.
func TestMetricsFormatters(t *testing.T) {
	for _, c := range []struct {
		name string
		got  string
		want string
	}{
		{"mDur hours minutes", mDur(12000000), "3 h 20"},
		{"mDur padded minutes", mDur(21900000), "6 h 05"},
		{"mDur hours half", mDur(34200000), "9 h 30"},
		{"mDur bare hour", mDur(3600000), "1 h"},
		{"mDur minutes", mDur(1500000), "25 m"},
		{"mDurMove down minutes", mDurMove(-1500000), "−25 m"},
		{"mDurMove up minutes", mDurMove(2400000), "+40 m"},
		{"mDurMove down hour", mDurMove(-3600000), "−1 h"},
		{"mPct", mPct(0.06), "6 %"},
		{"mPct rounded", mPct(0.824), "82 %"},
		{"mPctMove down", mPctMove(-0.02), "−2"},
		{"mPctMove up", mPctMove(0.03), "+3"},
		{"mCount", mCount(23), "23"},
		{"mCountMove", mCountMove(6), "+6"},
		{"mCountMove down", mCountMove(-38), "−38"},
		{"mK", mK(84000), "84 k"},
		{"mK fractional", mK(84500), "84.5 k"},
		{"mK millions", mK(2249900), "2.2 M"},
		{"mK whole millions", mK(3000000), "3 M"},
		{"mEur", mEur(87.4), "€87.40"},
		{"mEur two sixty", mEur(2.6), "€2.60"},
		{"mEurMove up", mEurMove(9.1), "+€9.10"},
		{"mEurMove down", mEurMove(-0.3), "−€0.30"},
		{"mDec", mDec(1.4), "1.4"},
		{"mDecMove", mDecMove(-0.3), "−0.3"},
		{"mShare", mShare(71.4), "71 %"},
		{"mShare under one", mShare(0.3), "< 1 %"},
		{"mShare zero", mShare(0), "0 %"},
		{"mDiff", mDiff(factoryd.DiffSize{Files: 9, Insertions: 240, Deletions: 95, Has: true}), "9 files · +240 −95"},
		{"mDiff one file", mDiff(factoryd.DiffSize{Files: 1, Insertions: 3, Deletions: 1, Has: true}), "1 file · +3 −1"},
		{"mDiff absent", mDiff(factoryd.DiffSize{}), "—"},
		{"mChat", mChat(factoryd.ChatWork{Calls: 312, Minutes: 41}), "312 calls · 41 min"},
		{"mChat under a minute", mChat(factoryd.ChatWork{Calls: 170, Minutes: 0.04}), "170 calls · 2 s"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// sparkPoints pulls the x,y pairs out of a spark's path.
func sparkPoints(t *testing.T, svg string) [][2]float64 {
	t.Helper()
	i := strings.Index(svg, `d="M`)
	if i < 0 {
		t.Fatalf("no path in %q", svg)
	}
	seg := svg[i+4:]
	if j := strings.Index(seg, `"`); j >= 0 {
		seg = seg[:j]
	}
	var pts [][2]float64
	for _, op := range strings.Split(seg, "L") {
		f := strings.Fields(strings.TrimPrefix(op, "M"))
		x, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			t.Fatalf("path point %q: %v", op, err)
		}
		y, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			t.Fatalf("path point %q: %v", op, err)
		}
		pts = append(pts, [2]float64{x, y})
	}
	return pts
}

// TestSparkWidensNearConstant: a small wiggle around a large middle
// stays a near-level line instead of a full-swing zigzag.
func TestSparkWidensNearConstant(t *testing.T) {
	pts := sparkPoints(t, string(sparkSVG([7]float64{82, 84, 83, 84, 84, 83, 84})))
	if len(pts) != 7 {
		t.Fatalf("%d points, want 7", len(pts))
	}
	lo, hi := math.MaxFloat64, -math.MaxFloat64
	for _, p := range pts {
		lo, hi = math.Min(lo, p[1]), math.Max(hi, p[1])
	}
	if hi-lo > 2 {
		t.Errorf("near-constant weeks swing %.1f px, want ≤ 2", hi-lo)
	}
	if pts[0][0] != 1 || pts[6][0] != 239 {
		t.Errorf("x ends = %.1f..%.1f, want 1..239", pts[0][0], pts[6][0])
	}
}

// TestSparkAllEqual: identical weeks draw one level line.
func TestSparkAllEqual(t *testing.T) {
	pts := sparkPoints(t, string(sparkSVG([7]float64{5, 5, 5, 5, 5, 5, 5})))
	for i, p := range pts {
		if p[1] != pts[0][1] {
			t.Errorf("point %d y = %.1f, want level %.1f", i, p[1], pts[0][1])
		}
	}
	if pts[0][1] != 15 {
		t.Errorf("level line at y = %.1f, want 15", pts[0][1])
	}
}

// TestSparkOutlier: a real range uses the whole band.
func TestSparkOutlier(t *testing.T) {
	pts := sparkPoints(t, string(sparkSVG([7]float64{1, 1, 1, 1, 1, 1, 100})))
	lo, hi := math.MaxFloat64, -math.MaxFloat64
	for _, p := range pts {
		lo, hi = math.Min(lo, p[1]), math.Max(hi, p[1])
	}
	if lo != 5 || hi != 25 {
		t.Errorf("y band = %.1f..%.1f, want 5..25", lo, hi)
	}
}
