package policy

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const valid = `
version: 1
budget:
  task_default_eur: 2
  task_max_eur: 5
  day_eur: 20
classes:
  coder:
    models: [coding-cheap, coding-frontier]
    tools: {}
`

func TestParseValid(t *testing.T) {
	d, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if d.Budget != (Budget{TaskDefaultEUR: 2, TaskMaxEUR: 5, DayEUR: 20}) {
		t.Fatalf("budget = %+v", d.Budget)
	}
	if got := d.Classes["coder"].Models; len(got) != 2 || got[0] != "coding-cheap" || got[1] != "coding-frontier" {
		t.Fatalf("coder models = %v", got)
	}
}

func TestParseRejects(t *testing.T) {
	for name, tc := range map[string]struct{ doc, want string }{
		"empty":           {"", "empty document"},
		"unknown top key": {valid + "risk: {}\n", "field risk not found"},
		"unknown budget key": {strings.Replace(valid, "day_eur: 20", "day_eur: 20\n  month_eur: 1", 1),
			"field month_eur not found"},
		"version":           {strings.Replace(valid, "version: 1", "version: 2", 1), "version 2"},
		"negative day cap":  {strings.Replace(valid, "day_eur: 20", "day_eur: -1", 1), "must not be negative"},
		"zero task max":     {strings.Replace(valid, "task_max_eur: 5", "task_max_eur: 0", 1), "must be positive"},
		"default above max": {strings.Replace(valid, "task_default_eur: 2", "task_default_eur: 6", 1), "exceeds task_max_eur"},
		"no classes":        {"version: 1\nbudget: {task_default_eur: 1, task_max_eur: 1, day_eur: 1}\n", "no capability classes"},
		"class no models":   {strings.Replace(valid, "[coding-cheap, coding-frontier]", "[]", 1), "allows no models"},
		"empty model":       {strings.Replace(valid, "[coding-cheap, coding-frontier]", `[coding-cheap, ""]`, 1), "empty model"},
		"tool without caps": {strings.Replace(valid, "tools: {}", "tools: {firecrawl: {max_requests: 5}}", 1), "positive max_requests and max_eur"},
		"two documents":     {valid + "---\nversion: 1\n", "more than one YAML document"},
		"not yaml":          {"version: [1", "policy:"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestDayCapZeroStopsSpend(t *testing.T) {
	d, err := Parse([]byte(strings.Replace(valid, "day_eur: 20", "day_eur: 0", 1)))
	if err != nil || d.Budget.DayEUR != 0 {
		t.Fatalf("day_eur 0 = (%+v, %v), want a valid kill switch", d.Budget, err)
	}
	var r *Refusal
	if _, err := Fit(1, 0, HoldRequest{Price: Price{In: 1e-6, Out: 4e-6}}); !errors.As(err, &r) || r.Decision != "refused_day_cap" {
		t.Fatalf("Fit under a day cap of 0 = %v, want refused_day_cap", err)
	}
}

// Hand-derived: in 1e-6, out 4e-6 per token, 1000 input tokens cost 0.001.
func TestFit(t *testing.T) {
	p := Price{In: 1e-6, Out: 4e-6}
	for _, tc := range []struct {
		name            string
		task, day       float64
		want, maxOutput int
		wantOut         int
		wantEUR         float64
		refused         string
	}{
		{name: "headroom 0.01 buys 2250 output tokens", task: 0.01, day: 5, wantOut: 2250, wantEUR: 0.01},
		{name: "the day binds", task: 5, day: 0.01, wantOut: 2250, wantEUR: 0.01},
		{name: "the caller's smaller limit stands", task: 0.01, day: 5, want: 2000, wantOut: 2000, wantEUR: 0.009},
		{name: "the alias's output limit binds", task: 0.01, day: 5, maxOutput: 1500, wantOut: 1500, wantEUR: 0.007},
		{name: "750 tokens is below the minimum call", task: 0.004, day: 5, refused: "refused_budget"},
		{name: "750 tokens under the day cap", task: 5, day: 0.004, refused: "refused_day_cap"},
		{name: "a small call that fits whole", task: 0.004, day: 5, want: 16, wantOut: 16, wantEUR: 0.001064},
		{name: "a small call that does not fit", task: 0.00105, day: 5, want: 16, refused: "refused_budget"},
		{name: "the input alone exceeds the headroom", task: 0.0005, day: 5, refused: "refused_budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := p
			pr.MaxOutput = tc.maxOutput
			h, err := Fit(tc.task, tc.day, HoldRequest{InTokens: 1000, Price: pr, WantOut: tc.want})
			if tc.refused != "" {
				var r *Refusal
				if !errors.As(err, &r) || r.Decision != tc.refused {
					t.Fatalf("Fit = (%+v, %v), want %s", h, err, tc.refused)
				}
				return
			}
			if err != nil || h.MaxOut != tc.wantOut || math.Abs(h.EUR-tc.wantEUR) > 1e-12 {
				t.Fatalf("Fit = (%+v, %v), want %d tokens for %g EUR", h, err, tc.wantOut, tc.wantEUR)
			}
		})
	}
}

func TestParseModelInfo(t *testing.T) {
	body := `{"data":[
	 {"model_name":"coding-cheap","model_info":{"input_cost_per_token":2.8e-7,"output_cost_per_token":4.2e-7,"max_output_tokens":8192}},
	 {"model_name":"coding-cheap","model_info":{"input_cost_per_token":1.5e-7,"output_cost_per_token":4.7e-7,"max_output_tokens":131072}},
	 {"model_name":"coding-frontier","model_info":{"input_cost_per_token":1.4e-6,"output_cost_per_token":4.4e-6,"output_cost_per_reasoning_token":5e-6,"max_output_tokens":null}},
	 {"model_name":"half-priced","model_info":{"input_cost_per_token":1e-7,"output_cost_per_token":1e-7}},
	 {"model_name":"half-priced","model_info":{"input_cost_per_token":null,"output_cost_per_token":1e-7}},
	 {"model_name":"free","model_info":{"input_cost_per_token":0,"output_cost_per_token":0}}]}`
	prices, unpriced, err := parseModelInfo([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Price{
		"coding-cheap":    {In: 2.8e-7, Out: 4.7e-7, MaxOutput: 131072},
		"coding-frontier": {In: 1.4e-6, Out: 5e-6},
	}
	if !reflect.DeepEqual(prices, want) || !unpriced["half-priced"] || !unpriced["free"] {
		t.Fatalf("prices = %+v, unpriced = %v; want %+v with half-priced and free unpriced", prices, unpriced, want)
	}
}

func TestPriceBook(t *testing.T) {
	fetches, fail := 0, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		if r.URL.Path != "/model/info" || r.Header.Get("Authorization") != "Bearer mk" {
			t.Errorf("request %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		io.WriteString(w, `{"data":[{"model_name":"a","model_info":{"input_cost_per_token":1e-6,"output_cost_per_token":4e-6}},
		                            {"model_name":"b","model_info":{"input_cost_per_token":1e-6}}]}`)
	}))
	defer srv.Close()
	b := &PriceBook{BaseURL: srv.URL, MasterKey: "mk", TTL: time.Hour}
	ctx := context.Background()
	if p, err := b.Price(ctx, "a"); err != nil || p != (Price{In: 1e-6, Out: 4e-6}) {
		t.Fatalf("Price(a) = (%+v, %v)", p, err)
	}
	if _, err := b.Price(ctx, "b"); err == nil || !strings.Contains(err.Error(), "no input or output price") {
		t.Fatalf("Price(b) = %v, want unpriced", err)
	}
	if _, err := b.Price(ctx, "c"); err == nil || !strings.Contains(err.Error(), "no model") {
		t.Fatalf("Price(c) = %v, want unknown", err)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d within the TTL, want 1", fetches)
	}
	// past the TTL a failed fetch keeps the old table
	b.TTL, fail = time.Nanosecond, true
	if p, err := b.Price(ctx, "a"); err != nil || p.Out != 4e-6 || fetches != 2 {
		t.Fatalf("Price(a) after a failed refresh = (%+v, %v) after %d fetches", p, err, fetches)
	}
	// without any table a failed fetch is an error
	if _, err := (&PriceBook{BaseURL: srv.URL, MasterKey: "mk"}).Price(ctx, "a"); err == nil {
		t.Fatal("a price without any table succeeded")
	}
}

func TestAllowsModel(t *testing.T) {
	d, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		classes []string
		model   string
		want    bool
	}{
		{[]string{"coder"}, "coding-cheap", true},
		{[]string{"coder"}, "coding-frontier", true},
		{[]string{"coder"}, "review-frontier", false},
		{[]string{"reviewer", "coder"}, "coding-cheap", true},
		{[]string{"reviewer"}, "coding-cheap", false},
		{nil, "coding-cheap", false},
	} {
		if got := d.AllowsModel(tc.classes, tc.model); got != tc.want {
			t.Errorf("AllowsModel(%v, %s) = %v, want %v", tc.classes, tc.model, got, tc.want)
		}
	}
}

func TestClampBudget(t *testing.T) {
	b := Budget{TaskDefaultEUR: 2, TaskMaxEUR: 5, DayEUR: 20}
	for requested, want := range map[float64]float64{0: 2, -1: 2, 0.01: 0.01, 3: 3, 5: 5, 9: 5} {
		if got := b.ClampBudget(requested); got != want {
			t.Errorf("ClampBudget(%g) = %g, want %g", requested, got, want)
		}
	}
}
