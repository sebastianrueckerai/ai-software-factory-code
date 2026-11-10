package policy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

func TestRouting(t *testing.T) {
	var none Routing
	if got := none.Bound(); got != 30 {
		t.Fatalf("absent bound = %g, want 30", got)
	}
	if got := (Routing{SlowSeconds: 45}).Bound(); got != 45 {
		t.Fatalf("bound = %g, want 45", got)
	}
	for alias, want := range map[string]string{"big": "big-alibaba", "small": "small-alibaba", "premium": "", "vision": ""} {
		if got := none.Twin(alias); got != want {
			t.Errorf("default twin of %s = %q, want %q", alias, got, want)
		}
	}
	own := Routing{Twins: map[string]string{"big": "big-backup"}}
	if own.Twin("big") != "big-backup" || own.Twin("small") != "" {
		t.Errorf("a file's twins replace the defaults: big %q, small %q, want big-backup and none", own.Twin("big"), own.Twin("small"))
	}
	if got, want := none.WithTwins([]string{"small", "big", "small"}, func(string) bool { return true }), []string{"big", "big-alibaba", "small", "small-alibaba"}; !slices.Equal(got, want) {
		t.Errorf("WithTwins = %v, want %v", got, want)
	}
	if got, want := none.WithTwins([]string{"small", "big"}, func(t string) bool { return t == "big-alibaba" }), []string{"big", "big-alibaba", "small"}; !slices.Equal(got, want) {
		t.Errorf("WithTwins with a twin LiteLLM does not serve = %v, want %v", got, want)
	}
	if got, want := none.WithTwins([]string{"premium"}, func(string) bool { return true }), []string{"premium"}; !slices.Equal(got, want) {
		t.Errorf("WithTwins(premium) = %v, want %v: premium has no twin", got, want)
	}
}

func TestLimitedTwinTakesItsAliasLimit(t *testing.T) {
	d := Document{Prices: ToolPrices{MaxOutputTokens: map[string]int{"big": 24000}}}
	if p, err := d.Limited("big-alibaba", Price{}); err != nil || p.MaxOutput != 24000 {
		t.Fatalf("Limited(big-alibaba) = %+v, %v, want big's 24000", p, err)
	}
	if _, err := d.Limited("small-alibaba", Price{}); err == nil {
		t.Fatal("Limited(small-alibaba) is priced, want the refusal: small has no limit to lend")
	}
	d.Prices.MaxOutputTokens["big-alibaba"] = 8000
	if p, _ := d.Limited("big-alibaba", Price{}); p.MaxOutput != 8000 {
		t.Fatalf("a twin's own limit = %d, want 8000", p.MaxOutput)
	}
}

// Deployments are the litellm_params.model of each model_name, each once, and
// a failed refresh keeps the last good answer.
func TestPriceBookDeployments(t *testing.T) {
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		io.WriteString(w, `{"data":[
		 {"model_name":"big","litellm_params":{"model":"zai/glm-5.3"},"model_info":{"input_cost_per_token":1e-6,"output_cost_per_token":4e-6}},
		 {"model_name":"big","litellm_params":{"model":"zai/glm-5.3"},"model_info":{"input_cost_per_token":1e-6,"output_cost_per_token":4e-6}},
		 {"model_name":"big-alibaba","litellm_params":{"model":"dashscope/qwen3.8-max"},"model_info":{"input_cost_per_token":1e-6,"output_cost_per_token":4e-6}}]}`)
	}))
	defer srv.Close()
	b := &PriceBook{BaseURL: srv.URL, MasterKey: "mk", TTL: time.Hour}
	ctx := context.Background()
	if got, err := b.Deployments(ctx, "big"); err != nil || !slices.Equal(got, []string{"zai/glm-5.3"}) {
		t.Fatalf("Deployments(big) = %v, %v, want [zai/glm-5.3]", got, err)
	}
	if _, err := b.Deployments(ctx, "nope"); err == nil {
		t.Fatal("Deployments(nope) answered")
	}
	fail = true
	b.at = time.Now().Add(-2 * time.Hour)
	if got, err := b.Deployments(ctx, "big-alibaba"); err != nil || !slices.Equal(got, []string{"dashscope/qwen3.8-max"}) {
		t.Fatalf("Deployments(big-alibaba) after a failed refresh = %v, %v, want the last good answer", got, err)
	}
}
