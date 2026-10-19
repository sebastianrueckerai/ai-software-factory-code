package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		"change_failure", "reverted_refiled", "time_to_restore", "spend", "per_card", "tokens_per_card", "model_mix",
		"interventions", "escalations", "human_blocked", "review_acceptance", "self_deploys", "diff_size",
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
