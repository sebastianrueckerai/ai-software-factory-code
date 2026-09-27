package policy

import (
	"strings"
	"testing"
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
		"zero day cap":      {strings.Replace(valid, "day_eur: 20", "day_eur: 0", 1), "must all be positive"},
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
