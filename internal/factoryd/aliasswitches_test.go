package factoryd

import (
	"context"
	"errors"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"strings"
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

// A floor refusal that carries a price read's error masks the URL's userinfo
// and the bearer value.
func TestFloorUnreadErrorIsScrubbed(t *testing.T) {
	const leak = "GET https://user:pass@litellm.example/model/info: Authorization: Bearer abcdefghijklmnopqrstuvwx"
	r := &Runner{store: newFakeStore(), cfg: RunnerConfig{Prices: failingPrices{errors.New(leak)}, Policy: testPolicyVersion}, Scrub: redact.New()}
	need, err := r.needOf(context.Background(), testPolicyVersion(), Task{ID: "t1"}, "factory/big", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(need.unread, "user:pass") || strings.Contains(need.unread, "abcdefghijklmnopqrstuvwx") || !strings.Contains(need.unread, "[redacted]") {
		t.Fatalf("the floor's refusal %q leaks, or masks nothing", need.unread)
	}
}

type failingPrices struct{ err error }

func (f failingPrices) Price(context.Context, string) (policy.Price, error) {
	return policy.Price{}, f.err
}
