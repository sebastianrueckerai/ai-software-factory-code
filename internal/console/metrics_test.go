package console

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
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
	for _, key := range []string{"repository", "from", "to", "deploys", "lead_time", "card_cycle", "throughput",
		"change_failure", "reverted_refiled", "time_to_restore", "spend", "per_card", "tokens_per_card", "model_spend", "served_by", "tool_spend", "tool_mix",
		"interventions", "escalations", "escalated_share", "human_blocked", "review_acceptance", "self_deploys", "diff_size",
		"chat_tool_work", "ci_duration"} {
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

func TestProjectMetricsAnswers403ForNonWriter(t *testing.T) {
	r := newRig(t)
	r.repoWriters["sf/code"] = []string{"someone-else"}
	ck, _ := r.loggedIn(t)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/metrics?format=json", nil), ck)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-writer = %d, want 403", rec.Code)
	}
}

// TestProjectMetricsPage pins the page's structure: the deck's five
// sections and 18 row names, the window control, the measure, the open
// page's word, and one figure rendered from the report.
func TestProjectMetricsPage(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/metrics", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET metrics page = %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	wants := []string{
		`class="page page-metrics"`,
		"Delivery", "Stability", "Cost", "Autonomy", "The factory itself",
		"deploys / week", "lead time, push to live", "card cycle", "throughput",
		"change failure", "reverted · refiled", "time to restore",
		"spend / week", "per card, median", "tokens / card", "spend by model", "served by", "tools / week", "tool mix",
		"interventions / card", "escalations", "human-blocked", "review acceptance",
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
		{"mMix", mMix([]factoryd.MixShare{{Alias: "big", Share: 71}, {Alias: "big", Share: 18}, {Alias: "pulse", Share: 11}}), "big 71 % · big 18 % · pulse 11 %"},
		{"mMix empty", mMix(nil), "—"},
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
