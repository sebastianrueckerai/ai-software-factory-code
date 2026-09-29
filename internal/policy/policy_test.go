package policy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const valid = `
version: 1
limits: {sessions: 3, agent_time: 20m, supervisor_calls: 3, supervisor_timeout: 6m, stronger_after: 1}
budget:
  task_default_eur: 2
  task_max_eur: 5
  day_eur: 20
classes:
  coder:
    models: [coding-cheap, coding-frontier]
    tools: {}
risk:
  classes:
    - {name: forbidden, outcome: forbidden}
    - {name: migration, outcome: requires_approval, migration: true}
    - {name: high, outcome: requires_approval}
    - {name: normal, outcome: requires_approval}
  rules:
    - {class: forbidden, paths: [".forgejo/**", ".gitea/**", ".github/**"]}
    - {class: migration, paths: ["internal/*/migrations/**", "internal/policy/schema.sql"]}
    - {class: high, paths: ["k8s/**", "**/Dockerfile*", "go.mod", "go.sum"]}
  default: normal
envelope_default:
  monthly_eur: 10
  server_types: [cx23, cx33]
  max_count: 2
  max_lease_hours: 4
  volume_max_gb: 20
approvers: [sebastian]
`

func TestParseValid(t *testing.T) {
	d, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if d.Budget != (Budget{TaskDefaultEUR: 2, TaskMaxEUR: 5, DayEUR: 20}) {
		t.Fatalf("budget = %+v", d.Budget)
	}
	want := Envelope{MonthlyEUR: 10, ServerTypes: []string{"cx23", "cx33"}, MaxCount: 2, MaxLeaseHours: 4, VolumeMaxGB: 20}
	if !reflect.DeepEqual(d.EnvelopeDefault, want) {
		t.Fatalf("envelope_default = %+v, want %+v", d.EnvelopeDefault, want)
	}
	if !reflect.DeepEqual(d.Approvers, []string{"sebastian"}) {
		t.Fatalf("approvers = %v", d.Approvers)
	}
	if got := d.Classes["coder"].Models; len(got) != 2 || got[0] != "coding-cheap" || got[1] != "coding-frontier" {
		t.Fatalf("coder models = %v", got)
	}
}

func TestParseRejects(t *testing.T) {
	for name, tc := range map[string]struct{ doc, want string }{
		"empty":           {"", "empty document"},
		"unknown top key": {valid + "surprise: {}\n", "field surprise not found"},
		"unknown budget key": {strings.Replace(valid, "day_eur: 20", "day_eur: 20\n  month_eur: 1", 1),
			"field month_eur not found"},
		"no envelope":         {valid[:strings.Index(valid, "envelope_default")], "envelope_default.server_types"},
		"envelope zero cap":   {strings.Replace(valid, "monthly_eur: 10", "monthly_eur: 0", 1), "envelope_default.monthly_eur"},
		"envelope no count":   {strings.Replace(valid, "max_count: 2", "max_count: 0", 1), "envelope_default.max_count"},
		"envelope no lease":   {strings.Replace(valid, "max_lease_hours: 4", "max_lease_hours: 0", 1), "envelope_default.max_lease_hours"},
		"envelope empty type": {strings.Replace(valid, "[cx23, cx33]", `[cx23, ""]`, 1), "envelope_default.server_types"},
		"envelope volume":     {strings.Replace(valid, "volume_max_gb: 20", "volume_max_gb: 5", 1), "envelope_default.volume_max_gb"},
		"version":             {strings.Replace(valid, "version: 1", "version: 2", 1), "version 2"},
		"negative day cap":    {strings.Replace(valid, "day_eur: 20", "day_eur: -1", 1), "must not be negative"},
		"zero task max":       {strings.Replace(valid, "task_max_eur: 5", "task_max_eur: 0", 1), "must be positive"},
		"default above max":   {strings.Replace(valid, "task_default_eur: 2", "task_default_eur: 6", 1), "exceeds task_max_eur"},
		"no classes":          {"version: 1\nbudget: {task_default_eur: 1, task_max_eur: 1, day_eur: 1}\nlimits: {sessions: 3, agent_time: 20m, supervisor_calls: 3, supervisor_timeout: 6m, stronger_after: 1}\n", "no capability classes"},
		"class no models":     {strings.Replace(valid, "[coding-cheap, coding-frontier]", "[]", 1), "allows no models"},
		"empty model":         {strings.Replace(valid, "[coding-cheap, coding-frontier]", `[coding-cheap, ""]`, 1), "empty model"},
		"tool without caps":   {strings.Replace(valid, "tools: {}", "tools: {web_scrape: {max_requests: 5}}", 1), "positive max_requests and max_eur"},
		"unknown tool":        {strings.Replace(valid, "tools: {}", "tools: {firecrawl: {max_requests: 5, max_eur: 1}}", 1), `unknown tool "firecrawl"`},
		"free tool with caps": {strings.Replace(valid, "tools: {}", "tools: {escalate_to_human: {max_requests: 5, max_eur: 1}}", 1), "tool escalate_to_human is free: list it as {}"},
		"scrape unpriced":     {strings.Replace(valid, "tools: {}", "tools: {web_scrape: {max_requests: 5, max_eur: 1}}", 1), "prices.firecrawl needs"},
		"replicate sku without rate": {strings.Replace(valid, "tools: {}", "tools: {replicate_predict: {max_requests: 5, max_eur: 1}}", 1) +
			"prices: {replicate: {max_seconds: 90, models: {a/b: gpu-t4}, eur_per_second: {cpu: 0.0001}}}\n", `runs on "gpu-t4", which has no positive eur_per_second`},
		"replicate max_seconds below the cancel floor": {strings.Replace(valid, "tools: {}", "tools: {replicate_predict: {max_requests: 5, max_eur: 1}}", 1) +
			"prices: {replicate: {max_seconds: 9, models: {a/b: cpu}, eur_per_second: {cpu: 0.0001}}}\n", "max_seconds must be whole seconds, at least 10"},
		"replicate fractional max_seconds": {strings.Replace(valid, "tools: {}", "tools: {replicate_predict: {max_requests: 5, max_eur: 1}}", 1) +
			"prices: {replicate: {max_seconds: 30.5, models: {a/b: cpu}, eur_per_second: {cpu: 0.0001}}}\n", "max_seconds must be whole seconds, at least 10"},
		"fal unbounded":      {strings.Replace(valid, "tools: {}", "tools: {fal_run: {max_requests: 5, max_eur: 1}}", 1), "prices.fal needs"},
		"two documents":      {valid + "---\nversion: 1\n", "more than one YAML document"},
		"empty approver":     {strings.Replace(valid, "approvers: [sebastian]", `approvers: [sebastian, ""]`, 1), "approvers must be distinct"},
		"duplicate approver": {strings.Replace(valid, "approvers: [sebastian]", "approvers: [sebastian, sebastian]", 1), "approvers must be distinct"},
		"link host a URL":    {valid + "link_hosts: [https://example.org]\n", `link_hosts: "https://example.org" is not a host name`},
		"not yaml":           {"version: [1", "policy:"},
		"zero steps":         {strings.Replace(valid, "stronger_after: 1}", "stronger_after: 1, steps: 0}", 1), "minimum"},
		"negative steps":     {strings.Replace(valid, "stronger_after: 1}", "stronger_after: 1, steps: -1}", 1), "limits.steps must not be negative"},
		"zero nudge_after":   {strings.Replace(valid, "stronger_after: 1}", "stronger_after: 1, nudge_after: 0}", 1), "minimum"},
		"one nudge_after":    {strings.Replace(valid, "stronger_after: 1}", "stronger_after: 1, nudge_after: 1}", 1), "minimum"},
		"zero nudges":        {strings.Replace(valid, "stronger_after: 1}", "stronger_after: 1, nudges: 0}", 1), "minimum"},
		"negative nudges":    {strings.Replace(valid, "stronger_after: 1}", "stronger_after: 1, nudges: -2}", 1), "limits.nudges must not be negative"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

const withTools = `
version: 1
limits: {sessions: 3, agent_time: 20m, supervisor_calls: 3, supervisor_timeout: 6m, stronger_after: 1}
budget: {task_default_eur: 2, task_max_eur: 5, day_eur: 20}
classes:
  coder:
    models: [coding-cheap]
    tools:
      web_scrape: {max_requests: 30, max_eur: 0.2}
      replicate_predict: {max_requests: 5, max_eur: 0.5}
  researcher:
    models: [coding-cheap]
    tools:
      web_scrape: {max_requests: 10, max_eur: 1}
prices:
  firecrawl: {eur_per_credit: 0.005, scrape_credits: 1, search_credits: 2}
  replicate:
    max_seconds: 90
    models: {replicate/hello-world: cpu}
    eur_per_second: {cpu: 0.0001}
risk:
  classes:
    - {name: forbidden, outcome: forbidden}
    - {name: migration, outcome: requires_approval, migration: true}
    - {name: high, outcome: requires_approval}
    - {name: normal, outcome: requires_approval}
  rules:
    - {class: forbidden, paths: [".forgejo/**", ".gitea/**", ".github/**"]}
    - {class: migration, paths: ["internal/*/migrations/**", "internal/policy/schema.sql"]}
    - {class: high, paths: ["k8s/**", "**/Dockerfile*", "go.mod", "go.sum"]}
  default: normal
envelope_default: {monthly_eur: 10, server_types: [cx23], max_count: 2, max_lease_hours: 4, volume_max_gb: 20}
`

// Tools are the tools the classes may call together, sorted: coder's
// two, researcher's one, and nothing for a class the policy lacks.
func TestTools(t *testing.T) {
	d, err := Parse([]byte(withTools))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		classes []string
		want    []string
	}{
		{[]string{"researcher"}, []string{"web_scrape"}},
		{[]string{"coder", "researcher"}, []string{"replicate_predict", "web_scrape"}},
		{[]string{"nobody"}, nil},
	} {
		if got := d.Tools(tc.classes); !slices.Equal(got, tc.want) {
			t.Errorf("Tools(%v) = %v, want %v", tc.classes, got, tc.want)
		}
	}
}

func TestToolCap(t *testing.T) {
	d, err := Parse([]byte(withTools))
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := d.ToolCap([]string{"coder"}, ToolWebScrape); !ok || c != (ToolCap{MaxRequests: 30, MaxEUR: 0.2}) {
		t.Fatalf("coder web_scrape = (%+v, %v)", c, ok)
	}
	// two classes: the most generous of each cap
	if c, ok := d.ToolCap([]string{"coder", "researcher"}, ToolWebScrape); !ok || c != (ToolCap{MaxRequests: 30, MaxEUR: 1}) {
		t.Fatalf("coder+researcher web_scrape = (%+v, %v)", c, ok)
	}
	if _, ok := d.ToolCap([]string{"researcher"}, ToolReplicate); ok {
		t.Fatal("researcher got replicate_predict")
	}
	if _, ok := d.ToolCap([]string{"coder"}, ToolFal); ok {
		t.Fatal("coder got fal_run, which the document does not list")
	}
}

// Hand-derived: cap 3 requests / 0.10 EUR, worst case 0.03.
func TestFitTool(t *testing.T) {
	r := HoldRequest{TaskID: "t", Kind: KindTool, Ref: ToolWebScrape, EUR: 0.03, Cap: ToolCap{MaxRequests: 3, MaxEUR: 0.1}}
	var ref *Refusal
	for name, tc := range map[string]struct {
		taskFree, dayFree, usedEUR float64
		requests                   int
		want                       string
	}{
		"fits":           {1, 1, 0.07, 2, ""},
		"requests used":  {1, 1, 0, 3, "refused_tool_requests"},
		"euros used":     {1, 1, 0.071, 2, "refused_tool_eur"},
		"task headroom":  {0.029, 1, 0, 0, "refused_budget"},
		"day headroom":   {1, 0.029, 0, 0, "refused_day_cap"},
		"exact headroom": {0.03, 0.03, 0, 0, ""},
	} {
		h, err := FitTool(tc.taskFree, tc.dayFree, tc.requests, tc.usedEUR, r)
		switch {
		case tc.want == "" && (err != nil || h.EUR != 0.03):
			t.Errorf("%s: (%+v, %v), want a hold of 0.03", name, h, err)
		case tc.want != "" && (!errors.As(err, &ref) || ref.Decision != tc.want):
			t.Errorf("%s: %v, want %s", name, err, tc.want)
		}
	}
}

func TestFitResource(t *testing.T) {
	r := HoldRequest{TaskID: "t", Kind: KindResource, Ref: "server", EUR: 0.0384}
	h, err := FitResource(0.05, 1, r)
	if err != nil || h.EUR != 0.0384 || h.Kind != KindResource {
		t.Fatalf("fit = %+v, %v", h, err)
	}
	var ref *Refusal
	if _, err := FitResource(0.03, 1, r); !errors.As(err, &ref) || ref.Decision != "refused_budget" {
		t.Fatalf("task headroom 0.03 err = %v, want refused_budget", err)
	}
	if _, err := FitResource(1, 0.01, r); !errors.As(err, &ref) || ref.Decision != "refused_day_cap" {
		t.Fatalf("day headroom 0.01 err = %v, want refused_day_cap", err)
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
	 {"model_name":"coding-cheap","model_info":{"input_cost_per_token":2.8e-7,"output_cost_per_token":4.2e-7,"cache_read_input_token_cost":2.8e-8,"cache_creation_input_token_cost":0.0,"max_output_tokens":8192,"search_context_cost_per_query":null}},
	 {"model_name":"coding-cheap","model_info":{"input_cost_per_token":1.5e-7,"output_cost_per_token":4.7e-7,"cache_creation_input_token_cost":2e-7,"max_output_tokens":131072}},
	 {"model_name":"coding-frontier","model_info":{"input_cost_per_token":1.4e-6,"output_cost_per_token":4.4e-6,"output_cost_per_reasoning_token":5e-6,"max_output_tokens":null}},
	 {"model_name":"long-context","model_info":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6,
	   "input_cost_per_token_above_200k_tokens":3e-6,"output_cost_per_token_above_200k_tokens":6e-6,"output_cost_per_token_priority":7e-6}},
	 {"model_name":"tiered","model_info":{"input_cost_per_token":1e-7,"output_cost_per_token":1e-7,
	   "tiered_pricing":[{"range":[0,32000],"input_cost_per_token":1e-7,"output_cost_per_token":1e-7},
	                     {"range":[32000,256000],"input_cost_per_token":8e-7,"output_cost_per_token":9e-6,"cache_read_input_token_cost":1e-8}]}},
	 {"model_name":"cache-write","model_info":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6,"cache_creation_input_token_cost":1.25e-6}},
	 {"model_name":"per-query","model_info":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6,"input_cost_per_query":0.01}},
	 {"model_name":"web-search","model_info":{"litellm_provider":"groq","input_cost_per_token":7.5e-8,"output_cost_per_token":3e-7,"cache_read_input_token_cost":3.75e-8,
	   "search_context_cost_per_query":{"search_context_size_high":0.005,"search_context_size_low":0.005,"search_context_size_medium":0.005}}},
	 {"model_name":"off-peak","model_info":{"input_cost_per_token":3e-7,"output_cost_per_token":1.2e-6,"cache_read_input_token_cost":6e-9,"max_output_tokens":393216,
	   "off_peak_pricing":{"cache_read_input_token_cost":3e-9,"input_cost_per_token":1.5e-7,"output_cost_per_token":6e-7,
	     "windows":[{"hours_utc":["00:00-01:00","04:00-06:00","10:00-00:00"],"weekdays":[1,2,3,4,5]},{"hours_utc":"00:00-00:00","weekdays":[6,7]}]}}},
	 {"model_name":"off-peak-dearer","model_info":{"input_cost_per_token":1e-7,"output_cost_per_token":2e-7,
	   "off_peak_pricing":{"input_cost_per_token":4e-7,"output_cost_per_token":5e-7,"windows":[]}}},
	 {"model_name":"sonar","model_info":{"litellm_provider":"perplexity","input_cost_per_token":1e-6,"output_cost_per_token":1e-6,
	   "search_context_cost_per_query":{"search_context_size_low":0.005,"search_context_size_medium":0.008,"search_context_size_high":0.012}}},
	 {"model_name":"regional","model_info":{"litellm_provider":"vertex_ai","input_cost_per_token":1e-6,"output_cost_per_token":2e-6,"regional_endpoint_uplift_multiplier":1.1}},
	 {"model_name":"residency","model_info":{"litellm_provider":"openai","input_cost_per_token":1e-6,"output_cost_per_token":2e-6,"regional_processing_uplift_multiplier_eu":1.1}},
	 {"model_name":"off-peak-flat","model_info":{"input_cost_per_token":3e-7,"output_cost_per_token":1.2e-6,
	   "off_peak_pricing":{"input_cost_per_token":5e-7,"output_cost_per_token":6e-7,"hours_utc":"16:30-00:30","weekday_timezone":"Asia/Shanghai"}}},
	 {"model_name":"off-peak-odd","model_info":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6,"off_peak_pricing":{"output_cost_per_second":1,"windows":[]}}},
	 {"model_name":"odd-tier","model_info":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6,"tiered_pricing":[{"output_cost_per_second":1}]}},
	 {"model_name":"half-priced","model_info":{"input_cost_per_token":1e-7,"output_cost_per_token":1e-7}},
	 {"model_name":"half-priced","model_info":{"input_cost_per_token":null,"output_cost_per_token":1e-7}},
	 {"model_name":"free","model_info":{"input_cost_per_token":0,"output_cost_per_token":0}},
	 {"model_name":"negative","model_info":{"input_cost_per_token":-1e-6,"output_cost_per_token":2e-6}}]}`
	prices, unpriced, err := parseModelInfo([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Price{
		"coding-cheap":    {In: 2.8e-7, Out: 4.7e-7, MaxOutput: 131072},
		"coding-frontier": {In: 1.4e-6, Out: 5e-6},
		"long-context":    {In: 3e-6, Out: 7e-6},
		"tiered":          {In: 8e-7, Out: 9e-6},
		"cache-write":     {In: 1.25e-6, Out: 2e-6},
		"web-search":      {In: 7.5e-8, Out: 3e-7},
		"off-peak":        {In: 3e-7, Out: 1.2e-6, MaxOutput: 393216},
		"off-peak-dearer": {In: 4e-7, Out: 5e-7},
		"off-peak-flat":   {In: 5e-7, Out: 1.2e-6},
	}
	if !reflect.DeepEqual(prices, want) {
		t.Fatalf("prices = %+v, want %+v", prices, want)
	}
	for _, alias := range []string{"per-query", "odd-tier", "off-peak-odd", "sonar", "regional", "residency", "half-priced", "free", "negative"} {
		if !unpriced[alias] {
			t.Errorf("%s priced, want unpriced", alias)
		}
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
	if _, err := b.Price(ctx, "b"); err == nil || !strings.Contains(err.Error(), "cannot price") {
		t.Fatalf("Price(b) = %v, want unpriced", err)
	}
	if _, err := b.Price(ctx, "c"); err == nil || !strings.Contains(err.Error(), "no model") {
		t.Fatalf("Price(c) = %v, want unknown", err)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d within the TTL, want 1", fetches)
	}
	// past the TTL a failed fetch keeps the old table, up to 3 TTLs old
	fail = true
	b.at = time.Now().Add(-2 * time.Hour)
	if p, err := b.Price(ctx, "a"); err != nil || p.Out != 4e-6 || fetches != 2 {
		t.Fatalf("Price(a) after a failed refresh = (%+v, %v) after %d fetches", p, err, fetches)
	}
	b.at = time.Now().Add(-3 * time.Hour)
	if _, err := b.Price(ctx, "a"); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("Price(a) with a 3-TTL-old table and a failed refresh = %v, want a refusal", err)
	}
	// a later successful fetch serves again
	fail = false
	if p, err := b.Price(ctx, "a"); err != nil || p.Out != 4e-6 {
		t.Fatalf("Price(a) after a good refresh = (%+v, %v)", p, err)
	}
	// without any table a failed fetch is an error
	fail = true
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

const withFactoryModels = `
version: 1
limits: {sessions: 3, agent_time: 20m, supervisor_calls: 3, supervisor_timeout: 6m, stronger_after: 1}
budget: {task_default_eur: 2, task_max_eur: 5, day_eur: 20}
classes:
  coder:
    models: [coding-cheap]
    review_model: review-frontier
    supervisor_model: supervisor
    audit_model: coding-frontier
  researcher:
    models: [coding-cheap]
    review_model: review-cheap
  plain:
    models: [coding-cheap]
risk:
  classes: [{name: normal, outcome: requires_approval}]
  default: normal
envelope_default: {monthly_eur: 10, server_types: [cx23], max_count: 2, max_lease_hours: 4, volume_max_gb: 20}
`

func TestFactoryModel(t *testing.T) {
	d, err := Parse([]byte(withFactoryModels))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		classes []string
		purpose string
		want    string
	}{
		{[]string{"coder"}, PurposeReview, "review-frontier"},
		{[]string{"coder"}, PurposeSupervisor, "supervisor"},
		{[]string{"coder"}, PurposeGateSupervisor, "supervisor"},
		{[]string{"coder"}, PurposeAudit, "coding-frontier"},
		{[]string{"researcher"}, PurposeAudit, ""},
		{[]string{"researcher", "coder"}, PurposeReview, "review-cheap"},
		{[]string{"researcher", "coder"}, PurposeSupervisor, "supervisor"},
		{[]string{"plain"}, PurposeReview, ""},
		{[]string{"coder"}, "session", ""},
		{nil, PurposeReview, ""},
	} {
		if got := d.FactoryModel(tc.classes, tc.purpose); got != tc.want {
			t.Errorf("FactoryModel(%v, %s) = %q, want %q", tc.classes, tc.purpose, got, tc.want)
		}
	}
	if d.AllowsModel([]string{"coder"}, "review-frontier") || d.AllowsModel([]string{"coder"}, "supervisor") {
		t.Fatal("a factory model is not an agent model")
	}
	if got := d.FactoryModels([]string{"researcher", "coder"}); !reflect.DeepEqual(got, []string{"coding-frontier", "review-cheap", "supervisor"}) {
		t.Fatalf("FactoryModels = %v", got)
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

// Absent bounds are the defaults; a bounds section overrides per key and
// its default covers the activities without a key of their own.
func TestBounds(t *testing.T) {
	d, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]time.Duration{
		"RunSession": 21 * time.Minute, "WaitChecks": 10 * time.Minute, "GateReproduction": 15 * time.Minute,
		"ModelReview": 3 * time.Minute, "Merge": 90 * time.Second, "GatePolicy": 2 * time.Minute, "Finish": 2 * time.Minute,
		"SuperviseGate": 8 * time.Minute,
	} {
		if got := d.Bound(name); got != want {
			t.Errorf("default Bound(%s) = %s, want %s", name, got, want)
		}
	}
	// a session outlives the agent time by a minute, the gate supervisor
	// its call's timeout by two
	d.Limits.AgentTime, d.Limits.SupervisorTimeout = 30*time.Minute, 10*time.Minute
	if got := d.Bound("RunSession"); got != 31*time.Minute {
		t.Errorf("Bound(RunSession) under agent_time 30m = %s, want 31m", got)
	}
	if got := d.Bound("SuperviseGate"); got != 12*time.Minute {
		t.Errorf("Bound(SuperviseGate) under supervisor_timeout 10m = %s, want 12m", got)
	}
	d, err = Parse([]byte(valid + "bounds: {WaitChecks: 900, default: 60}\n"))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]time.Duration{
		"WaitChecks": 15 * time.Minute, "RunSession": 21 * time.Minute, "GatePolicy": time.Minute, "Finish": time.Minute,
	} {
		if got := d.Bound(name); got != want {
			t.Errorf("Bound(%s) = %s, want %s", name, got, want)
		}
	}
}

func TestBoundsRejects(t *testing.T) {
	for name, tc := range map[string]struct{ bounds, want string }{
		"unknown activity": {"{RunSesion: 60}", `bounds: unknown activity "RunSesion"`},
		"zero seconds":     {"{Merge: 0}", "bounds.Merge must be a positive number of seconds"},
		"negative default": {"{default: -5}", "bounds.default must be a positive number of seconds"},
		"not seconds":      {"{Merge: 5m}", "policy:"},
		"not a map":        {"[RunSession]", "policy:"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(valid + "bounds: " + tc.bounds + "\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestDetectors(t *testing.T) {
	d, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Detector(DetectorNoWrite); got != 5*time.Minute {
		t.Errorf("default no_write = %s, want 5m", got)
	}
	d, err = Parse([]byte(valid + "supervisor: {no_write: 60}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Detector(DetectorNoWrite); got != time.Minute {
		t.Errorf("no_write = %s, want 1m", got)
	}
}

// A policy without limits.steps, and a version pinned before the key
// existed, cap a session at DefaultSteps.
func TestStepCap(t *testing.T) {
	d, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Limits.StepCap(); got != 200 {
		t.Errorf("default step cap = %d, want 200", got)
	}
	d, err = Parse([]byte(strings.Replace(valid, "stronger_after: 1}", "stronger_after: 1, steps: 40}", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Limits.StepCap(); got != 40 {
		t.Errorf("step cap = %d, want 40", got)
	}
	var pinned Limits
	if err := json.Unmarshal([]byte(`{"sessions":3,"agent_time":"20m0s","supervisor_calls":3,"supervisor_timeout":"6m0s","stronger_after":1}`), &pinned); err != nil {
		t.Fatal(err)
	}
	if got := pinned.StepCap(); got != 200 {
		t.Errorf("pinned version without steps: step cap = %d, want 200", got)
	}
}

// A policy without the nudge keys, and a version pinned before they
// existed, nudge once after three identical calls in a row.
func TestNudge(t *testing.T) {
	d, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if after, nudges := d.Limits.Nudge(); after != 3 || nudges != 1 {
		t.Errorf("default nudge = %d after, %d nudges, want 3, 1", after, nudges)
	}
	d, err = Parse([]byte(strings.Replace(valid, "stronger_after: 1}", "stronger_after: 1, nudge_after: 5, nudges: 2}", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if after, nudges := d.Limits.Nudge(); after != 5 || nudges != 2 {
		t.Errorf("nudge = %d after, %d nudges, want 5, 2", after, nudges)
	}
	var pinned Limits
	if err := json.Unmarshal([]byte(`{"sessions":3,"agent_time":"20m0s","supervisor_calls":3,"supervisor_timeout":"6m0s","stronger_after":1,"steps":200}`), &pinned); err != nil {
		t.Fatal(err)
	}
	if after, nudges := pinned.Nudge(); after != 3 || nudges != 1 {
		t.Errorf("pinned version without the nudge keys: %d after, %d nudges, want 3, 1", after, nudges)
	}
}

func TestDetectorsRejects(t *testing.T) {
	for name, tc := range map[string]struct{ section, want string }{
		"dropped detector": {"{no_commit: 60}", `supervisor: unknown detector "no_commit"`},
		"zero seconds":     {"{no_write: 0}", "supervisor.no_write must be a positive number of seconds"},
		"negative":         {"{no_write: -1}", "supervisor.no_write must be a positive number of seconds"},
		"not seconds":      {"{no_write: 15m}", "policy:"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(valid + "supervisor: " + tc.section + "\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// request_info is a free tool a class allows by listing it without caps.
func TestRequestInfoIsAFreeTool(t *testing.T) {
	d, err := Parse([]byte(strings.Replace(withTools, "      replicate_predict:", "      request_info: {}\n      replicate_predict:", 1)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !d.AllowsTool([]string{"coder"}, ToolRequestInfo) || d.AllowsTool([]string{"researcher"}, ToolRequestInfo) {
		t.Fatal("request_info: want allowed for coder only")
	}
}
