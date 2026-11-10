package factoryd

import (
	"context"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// While the gateway serves big from big-alibaba, the floor prices the dearer
// of the two on each rate: big is 3e-6 in and 1.5e-5 out, its twin 5e-6 in
// and 1e-5 out, so 5e-6 in and 1.5e-5 out. A switch of another alias, or to
// another twin, changes nothing.
func TestFloorPricesTheAliasThatServes(t *testing.T) {
	big := policy.Price{In: 3e-6, CacheRead: 3e-7, Out: 1.5e-5, MaxOutput: 32768}
	twin := policy.Price{In: 5e-6, CacheRead: 5e-7, Out: 1e-5, MaxOutput: 32768}
	store := newFakeStore()
	r := &Runner{store: store, cfg: RunnerConfig{Prices: aliasPrices{"big": big, "big-alibaba": twin}, Policy: testPolicyVersion}}
	price := func() policy.Price {
		t.Helper()
		p, err := r.price(context.Background(), "big")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if got := price(); got.In != 3e-6 || got.Out != 1.5e-5 {
		t.Fatalf("price without a switch = %+v, want big's", got)
	}
	store.switches = []policy.AliasSwitch{{Alias: "small", Twin: "small-alibaba", Kind: policy.SwitchOn}, {Alias: "big", Twin: "big-backup", Kind: policy.SwitchOn}}
	if got := price(); got.In != 3e-6 {
		t.Fatalf("price with another switch open = %+v, want big's", got)
	}
	store.switches = []policy.AliasSwitch{{Alias: "big", Twin: "big-alibaba", Kind: policy.SwitchOn}}
	if got := price(); got.In != 5e-6 || got.CacheRead != 5e-7 || got.Out != 1.5e-5 || got.MaxOutput != 32768 {
		t.Fatalf("price with big switched = %+v, want 5e-6 in, 5e-7 cache, 1.5e-5 out", got)
	}
}
