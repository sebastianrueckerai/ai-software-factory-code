package policy

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
  month_eur: 50
classes:
  coder:
    session: code
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
	if d.Budget != (Budget{TaskDefaultEUR: 2, TaskMaxEUR: 5, DayEUR: 20, MonthEUR: 50}) {
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

func TestParseSessionFloors(t *testing.T) {
	doc := strings.Replace(valid, "month_eur: 50", "month_eur: 50\n  session_floor_cheap_eur: 0.3\n  session_floor_frontier_eur: 0.6", 1)
	d, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if d.Budget.SessionFloorCheapEUR != 0.3 || d.Budget.SessionFloorFrontierEUR != 0.6 {
		t.Fatalf("budget = %+v", d.Budget)
	}
}

func TestParseRejects(t *testing.T) {
	for name, tc := range map[string]struct{ doc, want string }{
		"empty":           {"", "empty document"},
		"unknown top key": {valid + "surprise: {}\n", "field surprise not found"},
		"unknown budget key": {strings.Replace(valid, "day_eur: 20", "day_eur: 20\n  week_eur: 1", 1),
			"field week_eur not found"},
		"negative month cap":  {strings.Replace(valid, "month_eur: 50", "month_eur: -1", 1), "budget.month_eur must not be negative (0 stops all spend)"},
		"no month cap":        {strings.Replace(valid, "  month_eur: 50\n", "", 1), "month_eur"},
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
		"negative session floor": {strings.Replace(valid, "month_eur: 50", "month_eur: 50\n  session_floor_cheap_eur: -1", 1),
			"session_floor_cheap_eur and session_floor_frontier_eur must not be negative"},
		"session floor above the card maximum": {strings.Replace(valid, "month_eur: 50", "month_eur: 50\n  session_floor_frontier_eur: 6", 1),
			"exceeds budget.task_max_eur"},
		"no classes":          {"version: 1\nbudget: {task_default_eur: 1, task_max_eur: 1, day_eur: 1, month_eur: 1}\nlimits: {sessions: 3, agent_time: 20m, supervisor_calls: 3, supervisor_timeout: 6m, stronger_after: 1}\n", "no capability classes"},
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
		"zero output limit":  {valid + "prices: {max_output_tokens: {coding-frontier: 0}}\n", "prices.max_output_tokens of coding-frontier must be positive"},
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
		"one same_call":      {strings.Replace(valid, "stronger_after: 1}", "stronger_after: 1, same_call: 1}", 1), "limits.nudge_after (3) must be below limits.same_call (1)"},
		"nudge at same_call": {strings.Replace(valid, "stronger_after: 1}", "stronger_after: 1, nudge_after: 4}", 1), "limits.nudge_after (4) must be below limits.same_call (4)"},
		"same_call at nudge": {strings.Replace(valid, "stronger_after: 1}", "stronger_after: 1, same_call: 3}", 1), "limits.nudge_after (3) must be below limits.same_call (3)"},
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
budget: {task_default_eur: 2, task_max_eur: 5, day_eur: 20, month_eur: 50}
classes:
  coder:
    session: code
    models: [coding-cheap]
    tools:
      web_scrape: {max_requests: 30, max_eur: 0.2}
      replicate_predict: {max_requests: 5, max_eur: 0.5}
  researcher:
    session: code
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
		free     Free
		usedEUR  float64
		requests int
		want     string
	}{
		"fits":           {Free{Task: 1, Day: 1, Month: 1, Project: inf}, 0.07, 2, ""},
		"requests used":  {Free{Task: 1, Day: 1, Month: 1, Project: inf}, 0, 3, "refused_tool_requests"},
		"euros used":     {Free{Task: 1, Day: 1, Month: 1, Project: inf}, 0.071, 2, "refused_tool_eur"},
		"task headroom":  {Free{Task: 0.029, Day: 1, Month: 1, Project: inf}, 0, 0, "refused_budget"},
		"day headroom":   {Free{Task: 1, Day: 0.029, Month: 1, Project: inf}, 0, 0, "refused_day_cap"},
		"month headroom": {Free{Task: 1, Day: 1, Month: 0.029, Project: inf}, 0, 0, "refused_month_cap"},
		"exact headroom": {Free{Task: 0.03, Day: 0.03, Month: 0.03, Project: inf}, 0, 0, ""},
	} {
		h, err := FitTool(tc.free, tc.requests, tc.usedEUR, r)
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
	h, err := FitResource(Free{Task: 0.05, Day: 1, Month: 1, Project: inf}, r)
	if err != nil || h.EUR != 0.0384 || h.Kind != KindResource {
		t.Fatalf("fit = %+v, %v", h, err)
	}
	var ref *Refusal
	if _, err := FitResource(Free{Task: 0.03, Day: 1, Month: 1, Project: inf}, r); !errors.As(err, &ref) || ref.Decision != "refused_budget" {
		t.Fatalf("task headroom 0.03 err = %v, want refused_budget", err)
	}
	if _, err := FitResource(Free{Task: 1, Day: 0.01, Month: 1, Project: inf}, r); !errors.As(err, &ref) || ref.Decision != "refused_day_cap" {
		t.Fatalf("day headroom 0.01 err = %v, want refused_day_cap", err)
	}
	// a reservation the month cannot fit leaves euros for smaller calls
	if _, err := FitResource(Free{Task: 1, Day: 1, Month: 0.0383, Project: inf}, r); !errors.As(err, &ref) || ref.Decision != "refused_month_cap" ||
		!strings.Contains(ref.Reason, "the reservation exceeds the month's remaining headroom (budget.month_eur)") || strings.Contains(ref.Reason, "is spent") {
		t.Fatalf("month headroom 0.0383 err = %v, want refused_month_cap: the reservation exceeds the month's remaining headroom", err)
	}
	tool := HoldRequest{Kind: KindTool, Ref: ToolWebScrape, EUR: 0.03}
	if _, err := FitResource(Free{Task: 1, Day: 1, Month: 0.029, Project: inf}, tool); !errors.As(err, &ref) ||
		!strings.Contains(ref.Reason, "the tool call's worst case exceeds the month's remaining headroom (budget.month_eur)") {
		t.Fatalf("tool call past the month = %v, want its own reason", err)
	}
	if _, err := Fit(Free{Task: 1, Day: 1, Month: 0.001, Project: inf}, HoldRequest{Price: Price{In: 1e-6, Out: 4e-6, MaxOutput: unbound}}); !errors.As(err, &ref) ||
		!strings.Contains(ref.Reason, "the month cap budget.month_eur in policy.yaml is spent") {
		t.Fatalf("model call past the month = %v, want the cap spent", err)
	}
	// the project's cap names the project's file; its figure shows only
	// under a cap
	project := HoldRequest{TaskID: "t", Kind: KindResource, Ref: "server", EUR: 0.0384, Project: "linkshort"}
	if _, err := FitResource(Free{Task: 1, Day: 1, Month: 1, Project: 0.0383}, project); !errors.As(err, &ref) || ref.Decision != RefusedProjectCap ||
		ref.Reason != "server: worst case 0.038400 EUR exceeds the free headroom 0.038300 EUR (task 1.000000, day 1.000000, month 1.000000, project 0.038300); "+
			"the reservation exceeds the project's remaining month headroom (budget.month_eur in projects/linkshort.yaml), and only a human raises it, by a commit" {
		t.Fatalf("reservation past the project's month = %v", err)
	}
	project.Kind, project.Ref = KindTool, ToolWebScrape
	if _, err := FitResource(Free{Task: 1, Day: 1, Month: 1, Project: 0.0383}, project); !errors.As(err, &ref) || ref.Decision != RefusedProjectCap ||
		!strings.HasSuffix(ref.Reason, "; the tool call's worst case exceeds the project's remaining month headroom (budget.month_eur in projects/linkshort.yaml), and only a human raises it, by a commit") {
		t.Fatalf("tool call past the project's month = %v", err)
	}
	if _, err := Fit(Free{Task: 1, Day: 1, Month: 1, Project: 0.001}, HoldRequest{Price: Price{In: 1e-6, Out: 4e-6, MaxOutput: unbound}, Project: "linkshort"}); !errors.As(err, &ref) ||
		ref.Reason != "free headroom 0.001000 EUR (task 1.000000, day 1.000000, month 1.000000, project 0.001000) fits 250 output tokens after a request of 0 bytes (at most 0 prompt tokens), below the minimum call of 1024; "+
			"the project cap budget.month_eur in projects/linkshort.yaml is spent, and only a human raises it, by a commit" {
		t.Fatalf("model call past the project's month = %v", err)
	}
	if _, err := Fit(Free{Task: 0.001, Day: 1, Month: 1, Project: inf}, HoldRequest{Price: Price{In: 1e-6, Out: 4e-6, MaxOutput: unbound}}); !errors.As(err, &ref) ||
		ref.Reason != "free headroom 0.001000 EUR (task 0.001000, day 1.000000, month 1.000000) fits 250 output tokens after a request of 0 bytes (at most 0 prompt tokens), below the minimum call of 1024" {
		t.Fatalf("model call past the task's budget without a project cap = %v", err)
	}
}

var inf = math.Inf(1)

// An opening's hold sums its calls' holds as Fit prices them with the
// headroom unbounded, each at its own alias's rate: coding-frontier 3e-6
// in, 1.5e-5 out, coding-cheap 1e-6 in, 4e-6 out, both limited to 32,768
// output tokens. s1's 44,342 × 3e-6 + 24,576 × 1.5e-5 = 0.501666 and
// 7,031 × 1e-6 + 24,576 × 4e-6 = 0.105335 make 0.607001, less than s2's
// 80,000 × 3e-6 + 0.36864 = 0.60864 (at coding-frontier's rate alone s1
// would take 0.891399). A call asking no limit takes the limit: 1,000 ×
// 3e-6 + 32,768 × 1.5e-5 = 0.49452; under a limit of 8,192, it and a call
// asking 50,000 take 8,192: 0.003 + 0.12288 = 0.12588.
func TestOpeningHoldSumsWhatTheHoldsTake(t *testing.T) {
	prices := map[string]Price{"coding-frontier": {In: 3e-6, Out: 1.5e-5, MaxOutput: 32768}, "coding-cheap": {In: 1e-6, Out: 4e-6, MaxOutput: 32768}}
	price := func(ref string) (Price, error) {
		if p, ok := prices[ref]; ok {
			return p, nil
		}
		return Price{}, fmt.Errorf("LiteLLM has no model %q", ref)
	}
	r := HoldRequest{Kind: KindModel}
	s1 := []OpeningCall{{Ref: "coding-frontier", Bytes: 44342, WantOut: 24576}, {Ref: "coding-cheap", Bytes: 7031, WantOut: 24576}}
	s2 := []OpeningCall{{Ref: "coding-frontier", Bytes: 80000, WantOut: 24576}}
	if eur, calls, err := (Opening{Sessions: [][]OpeningCall{s1, s2}}).Hold(r, price); err != nil || !near(eur, 0.60864) || !reflect.DeepEqual(calls, s2) {
		t.Fatalf("hold = (%v, %v, %v), want s2's 0.60864", eur, calls, err)
	}
	unasked := Opening{Sessions: [][]OpeningCall{{{Ref: "coding-frontier", Bytes: 1000}}}}
	if eur, _, err := unasked.Hold(r, price); err != nil || !near(eur, 0.49452) {
		t.Fatalf("hold of a call asking no limit = (%v, %v), want the limit's 0.49452", eur, err)
	}
	prices["coding-frontier"] = Price{In: 3e-6, Out: 1.5e-5, MaxOutput: 8192}
	if eur, _, err := unasked.Hold(r, price); err != nil || !near(eur, 0.12588) {
		t.Fatalf("hold of a call asking no limit = (%v, %v), want 0.12588 under 8192", eur, err)
	}
	if eur, _, err := (Opening{Sessions: [][]OpeningCall{{{Ref: "coding-frontier", Bytes: 1000, WantOut: 50000}}}}).Hold(r, price); err != nil || !near(eur, 0.12588) {
		t.Fatalf("hold of a call asking past the limit = (%v, %v), want 0.12588 under 8192", eur, err)
	}
	if _, _, err := (Opening{Sessions: [][]OpeningCall{s1, {{Ref: "unpriced", Bytes: 1000}}}}).Hold(r, price); err == nil || err.Error() != `LiteLLM has no model "unpriced"` {
		t.Fatalf("hold with an unpriced call = %v, want its price's error", err)
	}
	if eur, calls, err := (Opening{}).Hold(r, price); eur != 0 || calls != nil || err != nil {
		t.Fatalf("hold without a session = (%v, %v, %v), want none", eur, calls, err)
	}
}

func TestHeadroomBound(t *testing.T) {
	for _, tc := range []struct {
		task, day, month, project float64
		want                      float64
		bound                     string
	}{
		{1, 3, 2, inf, 1, "refused_budget"},
		{5, 1, 2, inf, 1, "refused_day_cap"},
		{5, 3, 2, inf, 2, "refused_month_cap"},
		{1, 1, 1, inf, 1, "refused_budget"},
		{2, 1, 1, inf, 1, "refused_day_cap"},
		{2, 2, 1, inf, 1, "refused_month_cap"},
		{2, 20, 50, 0.1, 0.1, "refused_project_cap"},
		{2, 20, 0.1, 0.1, 0.1, "refused_month_cap"},
		{0.1, 20, 50, 0.1, 0.1, "refused_budget"},
		{2, 20, 50, 0, 0, "refused_project_cap"},
	} {
		if got, bound := HeadroomBound(tc.task, tc.day, tc.month, tc.project); got != tc.want || bound != tc.bound {
			t.Errorf("HeadroomBound(%g, %g, %g, %g) = (%g, %s), want (%g, %s)", tc.task, tc.day, tc.month, tc.project, got, bound, tc.want, tc.bound)
		}
	}
}

func TestDayCapZeroStopsSpend(t *testing.T) {
	d, err := Parse([]byte(strings.Replace(valid, "day_eur: 20", "day_eur: 0", 1)))
	if err != nil || d.Budget.DayEUR != 0 {
		t.Fatalf("day_eur 0 = (%+v, %v), want a valid kill switch", d.Budget, err)
	}
	var r *Refusal
	if _, err := Fit(Free{Task: 1, Day: 0, Month: 1, Project: inf}, HoldRequest{Price: Price{In: 1e-6, Out: 4e-6, MaxOutput: unbound}}); !errors.As(err, &r) || r.Decision != "refused_day_cap" {
		t.Fatalf("Fit under a day cap of 0 = %v, want refused_day_cap", err)
	}
}

func TestMonthCapZeroStopsSpend(t *testing.T) {
	d, err := Parse([]byte(strings.Replace(valid, "month_eur: 50", "month_eur: 0", 1)))
	if err != nil || d.Budget.MonthEUR != 0 {
		t.Fatalf("month_eur 0 = (%+v, %v), want a valid kill switch", d.Budget, err)
	}
	var r *Refusal
	if _, err := Fit(Free{Task: 1, Day: 1, Month: 0, Project: inf}, HoldRequest{Price: Price{In: 1e-6, Out: 4e-6, MaxOutput: unbound}, WantOut: 1}); !errors.As(err, &r) || r.Decision != "refused_month_cap" {
		t.Fatalf("Fit under a month cap of 0 = %v, want refused_month_cap", err)
	}
	if _, err := FitResource(Free{Task: 1, Day: 1, Month: 0, Project: inf}, HoldRequest{EUR: 0.0001}); !errors.As(err, &r) || r.Decision != "refused_month_cap" {
		t.Fatalf("FitResource under a month cap of 0 = %v, want refused_month_cap", err)
	}
}

// Hand-derived: in 1e-6, out 4e-6 per token, 1000 input tokens cost 0.001.
func TestFit(t *testing.T) {
	p := Price{In: 1e-6, Out: 4e-6, MaxOutput: unbound}
	for _, tc := range []struct {
		name            string
		task, day       float64
		month           float64 // 0: 5
		want, maxOutput int
		wantOut         int
		wantEUR         float64
		refused         string
	}{
		{name: "headroom 0.01 buys 2250 output tokens", task: 0.01, day: 5, wantOut: 2250, wantEUR: 0.01},
		{name: "the day binds", task: 5, day: 0.01, wantOut: 2250, wantEUR: 0.01},
		{name: "the month binds", task: 5, day: 5, month: 0.01, wantOut: 2250, wantEUR: 0.01},
		{name: "the caller's smaller limit stands", task: 0.01, day: 5, want: 2000, wantOut: 2000, wantEUR: 0.009},
		{name: "the alias's output limit binds", task: 0.01, day: 5, maxOutput: 1500, wantOut: 1500, wantEUR: 0.007},
		{name: "750 tokens is below the minimum call", task: 0.004, day: 5, refused: "refused_budget"},
		{name: "750 tokens under the day cap", task: 5, day: 0.004, refused: "refused_day_cap"},
		{name: "750 tokens under the month cap", task: 5, day: 5, month: 0.004, refused: "refused_month_cap"},
		{name: "a small call that fits whole", task: 0.004, day: 5, want: 16, wantOut: 16, wantEUR: 0.001064},
		{name: "a small call that does not fit", task: 0.00105, day: 5, want: 16, refused: "refused_budget"},
		{name: "the input alone exceeds the headroom", task: 0.0005, day: 5, refused: "refused_budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := p
			pr.MaxOutput = cmp.Or(tc.maxOutput, p.MaxOutput)
			h, err := Fit(Free{Task: tc.task, Day: tc.day, Month: cmp.Or(tc.month, 5), Project: inf}, HoldRequest{Bytes: 1000, Price: pr, WantOut: tc.want})
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

// An alias's output limit is the policy's, lowered by a smaller one its
// deployments publish; without the policy's, no call of it can be held,
// whatever its deployments publish.
func TestLimited(t *testing.T) {
	d, err := Parse([]byte(valid + "prices: {max_output_tokens: {coding-frontier: 8192}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		alias     string
		published int
		want      int
		err       string
	}{
		{alias: "coding-frontier", want: 8192},
		{alias: "coding-frontier", published: 4096, want: 4096},
		{alias: "coding-frontier", published: 32768, want: 8192},
		{alias: "coding-cheap", published: 4096, err: "coding-cheap has no output limit"},
	} {
		p, err := d.Limited(tc.alias, Price{In: 1e-6, Out: 4e-6, MaxOutput: tc.published})
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("Limited(%s, published %d) = (%+v, %v), want %q", tc.alias, tc.published, p, err, tc.err)
			}
			continue
		}
		if err != nil || p != (Price{In: 1e-6, Out: 4e-6, MaxOutput: tc.want}) {
			t.Fatalf("Limited(%s, published %d) = (%+v, %v), want a limit of %d", tc.alias, tc.published, p, err, tc.want)
		}
	}
}

// Hand-derived at in 0.30, cache read 0.03, out 1.50 EUR per million: a
// 495,000-byte request after a call of 480,000 bytes and 135,000 prompt
// tokens is about 495,000 × 135,000 / 480,000 = 139,218.75 tokens, its
// margin of a quarter 34,804.6875, and 4,096 output tokens cost 0.006144.
// A floor of the whole prefix holds the 135,000 at the cache-read price
// (0.00405), 4,218.75 and the margin at the input price (0.01170703125):
// 0.02190103125 EUR. A floor of zero, or none measured, holds all
// 174,023.4375 at the input price (0.05220703125): 0.05835103125. A floor
// of half holds 67,500 at the cache-read price (0.002025) and 106,523.4375
// at the input price (0.03195703125): 0.04012603125. The first call of a
// task holds the 495,000 bytes as tokens at the input price: 0.1485 +
// 0.006144.
func TestFitCalibrated(t *testing.T) {
	p := Price{In: 0.30e-6, CacheRead: 0.03e-6, Out: 1.50e-6, MaxOutput: unbound}
	earlier := Usage{PromptTokens: 135000, CachedTokens: 131600, OutputTokens: 900, BodyBytes: 480000}
	free := Free{Task: 1, Day: 5, Month: 5, Project: inf}
	for _, tc := range []struct {
		name    string
		earlier Usage
		floor   float64
		want    float64
	}{
		{"a floor of the whole prefix keeps the cache-read price", earlier, 1, 0.02190103125},
		{"a floor above the whole prefix counts as the whole", earlier, 1.4, 0.02190103125},
		{"a floor of zero prices the prefix at the input price", earlier, 0, 0.05835103125},
		{"a floor of half prices half the prefix at the cache-read price", earlier, 0.5, 0.04012603125},
		{"the first call keeps the byte bound", Usage{}, 1, 0.154644},
		{"an earlier call without usage keeps the byte bound", Usage{BodyBytes: 480000}, 1, 0.154644},
	} {
		h, err := Fit(free, HoldRequest{Bytes: 495000, Earlier: tc.earlier, CacheFloor: tc.floor, Price: p, WantOut: 4096})
		if err != nil || h.MaxOut != 4096 || math.Abs(h.EUR-tc.want) > 1e-12 {
			t.Errorf("%s: Fit = (%+v, %v), want 4096 tokens for %v EUR", tc.name, h, err, tc.want)
		}
	}
	var r *Refusal
	free.Task = 0.015
	if _, err := Fit(free, HoldRequest{Bytes: 495000, Earlier: earlier, Price: p, WantOut: 4096}); !errors.As(err, &r) ||
		r.Reason != "free headroom 0.015000 EUR (task 0.015000, day 5.000000, month 5.000000) fits 0 output tokens after a request of 495000 bytes (about 139219 prompt tokens), below the minimum call of 1024" {
		t.Errorf("calibrated refusal = %v", err)
	}
	if _, err := Fit(free, HoldRequest{Bytes: 495000, Price: p, WantOut: 4096}); !errors.As(err, &r) ||
		r.Reason != "free headroom 0.015000 EUR (task 0.015000, day 5.000000, month 5.000000) fits 0 output tokens after a request of 495000 bytes (at most 495000 prompt tokens), below the minimum call of 1024" {
		t.Errorf("first-call refusal = %v", err)
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
		"coding-cheap":    {In: 2.8e-7, CacheRead: 2e-7, Out: 4.7e-7, MaxOutput: 131072},
		"coding-frontier": {In: 1.4e-6, CacheRead: 1.4e-6, Out: 5e-6},
		"long-context":    {In: 3e-6, CacheRead: 3e-6, Out: 7e-6},
		"tiered":          {In: 8e-7, CacheRead: 1e-8, Out: 9e-6},
		"cache-write":     {In: 1.25e-6, CacheRead: 1.25e-6, Out: 2e-6},
		"off-peak":        {In: 3e-7, CacheRead: 6e-9, Out: 1.2e-6, MaxOutput: 393216},
		"off-peak-dearer": {In: 4e-7, CacheRead: 4e-7, Out: 5e-7},
		"off-peak-flat":   {In: 5e-7, CacheRead: 5e-7, Out: 1.2e-6},
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
	if p, err := b.Price(ctx, "a"); err != nil || p != (Price{In: 1e-6, CacheRead: 1e-6, Out: 4e-6}) {
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
budget: {task_default_eur: 2, task_max_eur: 5, day_eur: 20, month_eur: 50}
classes:
  coder:
    session: code
    models: [coding-cheap]
    review_model: review-frontier
    supervisor_model: supervisor
    audit_model: coding-frontier
  researcher:
    session: code
    models: [coding-cheap]
    review_model: review-cheap
  plain:
    session: code
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
		{[]string{"coder"}, PurposeOperator, ""},
		{[]string{ClassChat}, PurposeOperator, "operator"},
		{[]string{"coder"}, PurposePulse, ""},
		{[]string{ClassChat}, PurposePulse, "pulse"},
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
	b := Budget{TaskDefaultEUR: 2, TaskMaxEUR: 5, DayEUR: 20, MonthEUR: 50}
	for requested, want := range map[float64]float64{0: 2, -1: 2, 0.01: 0.01, 3: 3, 5: 5, 9: 5} {
		if got := b.ClampBudget(requested); got != want {
			t.Errorf("ClampBudget(%g) = %g, want %g", requested, got, want)
		}
	}
}

// A card's budget follows its size: small 2, medium 5, large 10, anything
// else the policy's default; an asked budget wins over the size, bounded by
// the project's card maximum (else a large card's budget) and the policy's.
func TestCardBudget(t *testing.T) {
	b := Budget{TaskDefaultEUR: 5, TaskMaxEUR: 15}
	for size, want := range map[string]float64{"small": 2, "medium": 5, "large": 10, "": 5, "huge": 5} {
		if got := b.SizeBudget(size); got != want {
			t.Errorf("SizeBudget(%q) = %g, want %g", size, got, want)
		}
	}
	for _, tc := range []struct {
		b         Budget
		p         *ProjectBudget
		size      string
		requested float64
		want      float64
	}{
		{b, nil, "large", 0, 10},
		{b, &ProjectBudget{TaskMaxEUR: 15}, "large", 0, 10},
		{b, &ProjectBudget{TaskMaxEUR: 15}, "", 15, 15},
		{b, nil, "", 15, 10},
		{b, &ProjectBudget{TaskMaxEUR: 1}, "small", 0, 1},
		{b, &ProjectBudget{TaskMaxEUR: 3}, "", 0, 3},
		{b, &ProjectBudget{MonthEUR: 50}, "small", 9, 9},
		{Budget{TaskDefaultEUR: 5, TaskMaxEUR: 4}, nil, "medium", 0, 4},
		{Budget{TaskDefaultEUR: 5, TaskMaxEUR: 4}, nil, "", 0, 4},
		{Budget{TaskDefaultEUR: 5, TaskMaxEUR: 4}, &ProjectBudget{TaskMaxEUR: 15}, "", 0, 4},
	} {
		if got := CardBudget(tc.b, tc.p, tc.size, false, tc.requested); got != tc.want {
			t.Errorf("CardBudget(%+v, %+v, %q, %g) = %g, want %g", tc.b, tc.p, tc.size, tc.requested, got, tc.want)
		}
	}
}

// A card filed on the strong rung gets at least the medium budget, whatever
// its size or the default; an asked budget stays the filer's, and the
// project's card maximum still bounds it.
func TestCardBudgetOfAStrongCard(t *testing.T) {
	b := Budget{TaskDefaultEUR: 2, TaskMaxEUR: 15}
	for _, tc := range []struct {
		p         *ProjectBudget
		size      string
		requested float64
		want      float64
	}{
		{nil, "small", 0, 5},
		{nil, "medium", 0, 5},
		{nil, "large", 0, 10},
		{nil, "", 0, 5},
		{nil, "small", 1, 1},
		{&ProjectBudget{TaskMaxEUR: 3}, "small", 0, 3},
		{&ProjectBudget{TaskMaxEUR: 3}, "", 0, 3},
	} {
		if got := CardBudget(b, tc.p, tc.size, true, tc.requested); got != tc.want {
			t.Errorf("CardBudget(strong, %+v, %q, %g) = %g, want %g", tc.p, tc.size, tc.requested, got, tc.want)
		}
	}
	if got := CardBudget(b, nil, "small", false, 0); got != 2 {
		t.Errorf("CardBudget(cheap, small) = %g, want 2", got)
	}
}

func TestSessionFloorByRung(t *testing.T) {
	b := Budget{SessionFloorCheapEUR: 0.3, SessionFloorFrontierEUR: 0.6}
	if b.SessionFloor(false) != 0.3 || b.SessionFloor(true) != 0.6 {
		t.Fatalf("floors = %g, %g, want 0.3 on cheap and 0.6 on frontier", b.SessionFloor(false), b.SessionFloor(true))
	}
	if (Budget{}).SessionFloor(true) != 0 {
		t.Fatal("an absent floor asks nothing")
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
		"ModelReview": 20 * time.Minute, "GateAudit": 20 * time.Minute, "Merge": 90 * time.Second, "GatePolicy": 2 * time.Minute,
		"Finish": 2 * time.Minute, "SuperviseGate": 8 * time.Minute,
	} {
		if got := d.Bound(name); got != want {
			t.Errorf("default Bound(%s) = %s, want %s", name, got, want)
		}
	}
	// a session outlives the agent time by a minute, the gate supervisor
	// its call's timeout by two, the review its three calls' by two
	d.Limits.AgentTime, d.Limits.SupervisorTimeout = 30*time.Minute, 10*time.Minute
	if got := d.Bound("RunSession"); got != 31*time.Minute {
		t.Errorf("Bound(RunSession) under agent_time 30m = %s, want 31m", got)
	}
	if got := d.Bound("SuperviseGate"); got != 12*time.Minute {
		t.Errorf("Bound(SuperviseGate) under supervisor_timeout 10m = %s, want 12m", got)
	}
	if got := d.Bound("ModelReview"); got != 32*time.Minute {
		t.Errorf("Bound(ModelReview) under supervisor_timeout 10m = %s, want 32m", got)
	}
	// the model review and the audit follow the parts their diff reads in
	// (PartsBound): at supervisor_timeout 6m one part trips at 8m, two at
	// 14m, three at Bound's 20m; parts change no other activity's
	d.Limits.SupervisorTimeout = 6 * time.Minute
	for _, tc := range []struct {
		name  string
		parts int
		want  time.Duration
	}{{"ModelReview", 1, 8 * time.Minute}, {"GateAudit", 2, 14 * time.Minute}, {"ModelReview", 3, 20 * time.Minute}, {"WaitChecks", 1, 10 * time.Minute}} {
		if got := d.PartsBound(tc.name, tc.parts); got != tc.want {
			t.Errorf("PartsBound(%s, %d) = %s, want %s", tc.name, tc.parts, got, tc.want)
		}
	}
	// a review the policy bounds itself keeps that bound, whatever its parts
	if b, err := Parse([]byte(valid + "bounds: {ModelReview: 900}\n")); err != nil || b.PartsBound("ModelReview", 1) != 15*time.Minute {
		t.Errorf("PartsBound(ModelReview, 1) under bounds.ModelReview 900 = %s (%v), want 15m", b.PartsBound("ModelReview", 1), err)
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
	d, err = Parse([]byte(strings.Replace(valid, "stronger_after: 1}", "stronger_after: 1, nudge_after: 5, nudges: 2, same_call: 6}", 1)))
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
	if got := pinned.SameCallAfter(); got != 4 {
		t.Errorf("pinned version without same_call: %d, want 4", got)
	}
	d, err = Parse([]byte(strings.Replace(valid, "stronger_after: 1}", "stronger_after: 1, nudge_after: 5, same_call: 6}", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Limits.SameCallAfter(); got != 6 {
		t.Errorf("same_call = %d, want 6", got)
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

func ptr[T any](v T) *T { return &v }

// TimeOf resolves agent_time and check_in field by field, the most
// specific level that sets a field winning: project over process over
// class over limits.
func TestTimeMostSpecificWins(t *testing.T) {
	d := Document{Limits: Limits{AgentTime: 20 * time.Minute}, Classes: map[string]Class{
		"coder": {Time: &Time{AgentTime: 2 * time.Hour, CheckIn: ptr(45 * time.Minute)}},
	}}
	process := Process{Time: &Time{AgentTime: 3 * time.Hour}}
	project := ProjectConfig{Time: &Time{AgentTime: 90 * time.Minute, CheckIn: ptr(30 * time.Minute)}}

	if agentTime, checkIn := d.TimeOf("coder", process, project); agentTime != 90*time.Minute || checkIn != 30*time.Minute {
		t.Errorf("class+process+project: TimeOf = %s, %s, want 90m, 30m", agentTime, checkIn)
	}
	if agentTime, checkIn := d.TimeOf("coder", process, ProjectConfig{}); agentTime != 3*time.Hour || checkIn != 45*time.Minute {
		t.Errorf("without project: TimeOf = %s, %s, want 3h, 45m", agentTime, checkIn)
	}
	if agentTime, checkIn := d.TimeOf("coder", Process{}, ProjectConfig{}); agentTime != 2*time.Hour || checkIn != 45*time.Minute {
		t.Errorf("without process and project: TimeOf = %s, %s, want 2h, 45m", agentTime, checkIn)
	}
	if agentTime, checkIn := d.TimeOf("researcher", Process{}, ProjectConfig{}); agentTime != 20*time.Minute || checkIn != 0 {
		t.Errorf("class without Time: TimeOf = %s, %s, want the limits, 20m, 0", agentTime, checkIn)
	}
	// The process shortens agent_time below the class's check_in without
	// overriding check_in itself: a check-in that would land at or after
	// the session's own deadline is no check-in.
	if agentTime, checkIn := d.TimeOf("coder", Process{Time: &Time{AgentTime: 30 * time.Minute}}, ProjectConfig{}); agentTime != 30*time.Minute || checkIn != 0 {
		t.Errorf("check_in past agent_time: TimeOf = %s, %s, want 30m, 0", agentTime, checkIn)
	}
}

// agent_time lies between 1m and 72h, check_in is 0 (no check-in) or
// between 10m and agent_time (when both are set).
func TestTimeValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		time Time
		ok   bool
	}{
		"unset":                     {Time{}, true},
		"agent_time 1m":             {Time{AgentTime: time.Minute}, true},
		"agent_time 72h":            {Time{AgentTime: 72 * time.Hour}, true},
		"agent_time 72h1m refused":  {Time{AgentTime: 72*time.Hour + time.Minute}, false},
		"agent_time under 1m":       {Time{AgentTime: 30 * time.Second}, false},
		"check_in 0 accepted":       {Time{CheckIn: ptr(time.Duration(0))}, true},
		"check_in 5m refused":       {Time{CheckIn: ptr(5 * time.Minute)}, false},
		"check_in 10m":              {Time{CheckIn: ptr(10 * time.Minute)}, true},
		"check_in above agent_time": {Time{AgentTime: 10 * time.Minute, CheckIn: ptr(20 * time.Minute)}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if err := tc.time.validate(); tc.ok != (err == nil) {
				t.Fatalf("validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
	base := Limits{Sessions: 1, AgentTime: 20 * time.Minute, SupervisorCalls: 1, SupervisorTimeout: time.Minute}
	for name, tc := range map[string]struct {
		limits Limits
		ok     bool
	}{
		"valid":            {base, true},
		"agent_time 72h1m": {func() Limits { l := base; l.AgentTime = 72*time.Hour + time.Minute; return l }(), false},
		"check_in 0":       {func() Limits { l := base; l.CheckIn = 0; return l }(), true},
		"check_in 5m":      {func() Limits { l := base; l.CheckIn = 5 * time.Minute; return l }(), false},
	} {
		t.Run("limits "+name, func(t *testing.T) {
			if err := tc.limits.validate(); tc.ok != (err == nil) {
				t.Fatalf("validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestLimitsStopValues(t *testing.T) {
	d, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	l := d.Limits
	if l.WarnShare() != 0.80 || l.PlanWarnShare() != 0.67 || l.NoPushShare() != 0.60 || l.ContextCap() != 100000 {
		t.Errorf("defaults = %v %v %v %d, want 0.8 0.67 0.6 100000", l.WarnShare(), l.PlanWarnShare(), l.NoPushShare(), l.ContextCap())
	}
	d, err = Parse([]byte(strings.Replace(valid, "stronger_after: 1}", "stronger_after: 1, warn_at: 75, plan_warn_at: 50, no_push_spend: 40, context_tokens: 90000}", 1)))
	if err != nil {
		t.Fatal(err)
	}
	l = d.Limits
	if l.WarnShare() != 0.75 || l.PlanWarnShare() != 0.50 || l.NoPushShare() != 0.40 || l.ContextCap() != 90000 {
		t.Errorf("set = %v %v %v %d, want 0.75 0.5 0.4 90000", l.WarnShare(), l.PlanWarnShare(), l.NoPushShare(), l.ContextCap())
	}
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	var back Limits
	if err := json.Unmarshal(b, &back); err != nil || back != l {
		t.Errorf("json round trip = (%+v, %v), want %+v", back, err, l)
	}
	var pinned Limits
	if err := json.Unmarshal([]byte(`{"sessions":3,"agent_time":"20m0s","supervisor_calls":3,"supervisor_timeout":"6m0s","stronger_after":1,"steps":200}`), &pinned); err != nil {
		t.Fatal(err)
	}
	if pinned.WarnShare() != 0.80 || pinned.ContextCap() != 100000 {
		t.Errorf("pinned version without the keys: %v, %d, want the defaults", pinned.WarnShare(), pinned.ContextCap())
	}
}

func TestLimitsStopValuesAreBounded(t *testing.T) {
	d, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		mutate func(*Limits)
		want   string
	}{
		"warn_at 100":         {func(l *Limits) { l.WarnAt = 100 }, "limits.warn_at must lie between 1 and 99 percent when set in policy.yaml (0 means absent, as in a version pinned before the key), is 100"},
		"plan_warn_at -1":     {func(l *Limits) { l.PlanWarnAt = -1 }, "limits.plan_warn_at must lie between 1 and 99 percent when set in policy.yaml (0 means absent, as in a version pinned before the key), is -1"},
		"no_push_spend 100":   {func(l *Limits) { l.NoPushSpend = 100 }, "limits.no_push_spend must lie between 1 and 99 percent when set in policy.yaml (0 means absent, as in a version pinned before the key), is 100"},
		"context_tokens 1000": {func(l *Limits) { l.ContextTokens = 1000 }, "limits.context_tokens must lie between 40000 and 200000 when set in policy.yaml (0 means absent, as in a version pinned before the key), is 1000"},
		"context past window": {func(l *Limits) { l.ContextTokens = 200001 }, "limits.context_tokens must lie between 40000 and 200000 when set in policy.yaml (0 means absent, as in a version pinned before the key), is 200001"},
	} {
		l := d.Limits
		tc.mutate(&l)
		if err := l.validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: validate = %v, want %q", name, err, tc.want)
		}
	}
	for doc, want := range map[string]string{
		"warn_at: 0":           "properties/warn_at: minimum: 0/1 is less than 1",
		"plan_warn_at: 0":      "properties/plan_warn_at: minimum: 0/1 is less than 1",
		"no_push_spend: 0":     "properties/no_push_spend: minimum: 0/1 is less than 1",
		"context_tokens: 0":    "properties/context_tokens: minimum: 0/1 is less than 40000",
		"context_tokens: 1000": "limits.context_tokens must lie between 40000 and 200000 when set in policy.yaml (0 means absent, as in a version pinned before the key), is 1000",
		"no_push_spend: 100":   "limits.no_push_spend must lie between 1 and 99 percent when set in policy.yaml (0 means absent, as in a version pinned before the key), is 100",
	} {
		_, err := Parse([]byte(strings.Replace(valid, "stronger_after: 1}", "stronger_after: 1, "+doc+"}", 1)))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: Parse = %v, want the refusal %q", doc, err, want)
		}
	}
}
