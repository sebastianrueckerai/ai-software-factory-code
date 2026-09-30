package policy

import "testing"

func TestZZReproGroqSearchBranch(t *testing.T) {
	body := `{"data":[
	 {"model_name":"web-search","model_info":{"litellm_provider":"groq","input_cost_per_token":7.5e-8,"output_cost_per_token":3e-7,
	   "search_context_cost_per_query":{"search_context_size_high":0.005}}}]}`
	prices, unpriced, err := parseModelInfo([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !unpriced["web-search"] {
		t.Fatalf("groq deployment carrying search_context_cost_per_query was priced %+v; want unpriced like every other provider", prices["web-search"])
	}
}
