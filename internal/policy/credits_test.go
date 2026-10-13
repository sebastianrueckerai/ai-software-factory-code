package policy

import "testing"

// The plan's rates, by the deployment as LiteLLM names it after the
// provider; a name the plan holds no rate for answers none, so no credits
// are derived from its tokens.
func TestCreditRateOf(t *testing.T) {
	for deployment, want := range map[string]CreditRate{
		"zai/glm-5.3":       {Input: 6.9, Cached: 1.7, Output: 24},
		"zai/glm-5.3-flash": {Input: 2.3, Cached: 0.56, Output: 8},
		"glm-5.3":           {Input: 6.9, Cached: 1.7, Output: 24},
		"zai/GLM-5.3":       {Input: 6.9, Cached: 1.7, Output: 24},
	} {
		if r, ok := CreditRateOf(deployment); !ok || r != want {
			t.Errorf("CreditRateOf(%q) = (%+v, %v), want (%+v, true)", deployment, r, ok, want)
		}
	}
	for _, deployment := range []string{"", "zai/", "openai/gpt-5.3", "zai/glm-4.6"} {
		if r, ok := CreditRateOf(deployment); ok {
			t.Errorf("CreditRateOf(%q) = (%+v, true), want no rate", deployment, r)
		}
	}
}

// z.ai's own worked example: 1,000,000 prompt tokens of which 900,000
// served from the cache, and 20,000 output tokens are (100,000 × 6.9 +
// 900,000 × 1.7 + 20,000 × 24) / 10000 = 270 credits. A cached share
// beyond the prompt counts as the whole prompt cached, never a negative
// input.
func TestCredits(t *testing.T) {
	r, _ := CreditRateOf("zai/glm-5.3")
	if got := r.Credits(1000000, 900000, 20000); got != 270 {
		t.Errorf("glm-5.3 credits = %g, want 270", got)
	}
	if got := r.Credits(20000, 0, 0); got != 13.8 { // 20,000 fresh input
		t.Errorf("credits of fresh input = %g, want 13.8", got)
	}
	flash, _ := CreditRateOf("zai/glm-5.3-flash")
	if got := flash.Credits(2300, 10000, 3000); got != 2.5288 { // the whole prompt cached: (2300×0.56 + 3000×8) / 10000
		t.Errorf("flash credits = %g, want 2.5288", got)
	}
	if got := r.Credits(0, 0, 0); got != 0 {
		t.Errorf("credits of nothing = %g, want 0", got)
	}
}
