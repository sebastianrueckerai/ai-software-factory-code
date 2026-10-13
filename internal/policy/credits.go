package policy

import "strings"

// Under the GLM Coding Plan no call is billed money: tokens are the
// stored fact, and the plan's own unit of use is the credit, derived from
// them (z.ai, 2026-10-13): credits = (input × input + cached × cached +
// output × output) / 10000, input the prompt less its cached share.
// Credits are never stored (roadmap 206): whoever shows them computes
// them from the tokens it reads.

// CreditRate is a model's credit multipliers: the credits its 10,000
// tokens of each kind cost.
type CreditRate struct {
	Input, Cached, Output float64
}

// creditRates are the plan's models by the deployment's model, as
// LiteLLM names it after the provider (z.ai's names, 2026-10-13).
var creditRates = map[string]CreditRate{
	"glm-5.3":       {Input: 6.9, Cached: 1.7, Output: 24},
	"glm-5.3-flash": {Input: 2.3, Cached: 0.56, Output: 8},
}

// CreditRateOf is deployment's rate — "provider/model" as LiteLLM names a
// deployment, or a bare model name; false when the plan holds no rate for
// its model, so no credits can be derived from it.
func CreditRateOf(deployment string) (CreditRate, bool) {
	_, model, _ := strings.Cut(deployment, "/")
	r, ok := creditRates[strings.ToLower(model)]
	return r, ok
}

// Credits is the plan's credits of one call's tokens: (input × input +
// cached × cached + output × output) / 10000, input the prompt less its
// cached share (a cached share beyond the prompt counts as the whole
// prompt cached).
func (r CreditRate) Credits(prompt, cached, output int) float64 {
	cached = min(cached, prompt)
	return (float64(prompt-cached)*r.Input + float64(cached)*r.Cached + float64(output)*r.Output) / 10000
}
