package policy

import (
	"slices"
	"testing"
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
	if got, want := none.WithTwins([]string{"small", "big", "small"}), []string{"big", "big-alibaba", "small", "small-alibaba"}; !slices.Equal(got, want) {
		t.Errorf("WithTwins = %v, want %v", got, want)
	}
	if got, want := none.WithTwins([]string{"premium"}), []string{"premium"}; !slices.Equal(got, want) {
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
