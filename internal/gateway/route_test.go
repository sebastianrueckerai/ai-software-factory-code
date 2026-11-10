package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// seedSlow seeds n successful calls of alias served by deployment, each
// 1000 output tokens in sec seconds: sec seconds per 1000 tokens.
func seedSlow(m *memStore, alias, deployment string, n, sec int) {
	for range n {
		m.addAudit(AuditRow{TaskID: "42", Decision: "allowed", ModelAlias: alias, Deployment: deployment,
			OutputTokens: 1000, DurationMS: sec * 1000, Status: "200"})
	}
}

type routeRig struct {
	store  *memStore
	stub   *stubUpstream
	minter *fakeMinter
	srv    *Server
	h      http.Handler
	clock  time.Time
}

func newRouteRig(t *testing.T) *routeRig {
	t.Helper()
	stub := newStubUpstream(t)
	stub.setResp(stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-response-cost": "0.001"}})
	store := newMemStore(Task{Classes: coder, ModelAlias: "big", ID: "42", BudgetEUR: 100, Status: "running"})
	prices := fakePrices{"small": testPrice, "big": testPrice, "big-alibaba": testPrice}
	minter := &fakeMinter{}
	s := NewServer(ServerOptions{Auth: fakeAuth{id: Identity{TaskID: "42", SAUID: "sa-1"}}, Store: store, Minter: minter, LiteLLMURL: stub.URL, Prices: prices})
	r := &routeRig{store: store, stub: stub, minter: minter, srv: s, h: s.Routes("test"), clock: time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)}
	s.now = func() time.Time { return r.clock }
	return r
}

// advance moves the server's clock, past the policy and routing reads' TTL.
func (r *routeRig) advance(d time.Duration) { r.clock = r.clock.Add(d) }

// modelsSent are the models the upstream received, in order.
func (r *routeRig) modelsSent(t *testing.T) []string {
	t.Helper()
	_, bodies, _ := r.stub.seen()
	var out []string
	for _, b := range bodies {
		var req struct{ Model string }
		if err := json.Unmarshal([]byte(b), &req); err != nil {
			t.Fatal(err)
		}
		out = append(out, req.Model)
	}
	return out
}

func (r *routeRig) call(t *testing.T, n int) { r.callOn(t, "big", n) }

func (r *routeRig) callOn(t *testing.T, alias string, n int) {
	t.Helper()
	for range n {
		if rr := doChat(t, r.h, chatBody(alias)); rr.Code != http.StatusOK {
			t.Fatalf("call = %d: %s", rr.Code, rr.Body)
		}
	}
}

// The median of 20 calls at 39 s per 1000 tokens is 39, above the bound of
// 30: the next call runs on the twin, priced and audited as the twin, and
// the switch is stored with that median.
func TestSlowAliasSwitchesToItsTwin(t *testing.T) {
	r := newRouteRig(t)
	seedSlow(r.store, "big", "zai/glm-5.3", 20, 39)
	r.call(t, 1)
	if got := r.modelsSent(t); len(got) != 1 || got[0] != "big-alibaba" {
		t.Fatalf("upstream models = %v, want [big-alibaba]", got)
	}
	rows := r.store.auditRows()
	if last := rows[len(rows)-1]; last.ModelAlias != "big-alibaba" || last.Decision != "allowed" {
		t.Fatalf("audit row = %+v, want allowed on big-alibaba", last)
	}
	sw := r.store.switchRows()
	if len(sw) != 1 || sw[0].Kind != policy.SwitchOn || sw[0].Alias != "big" || sw[0].Twin != "big-alibaba" || sw[0].Rate != 39 || sw[0].Deployment != "zai/glm-5.3" {
		t.Fatalf("switch rows = %+v, want one switch of big to big-alibaba at 39 on zai/glm-5.3", sw)
	}
	if open, _ := r.store.OpenAliasSwitches(t.Context()); len(open) != 1 {
		t.Fatalf("open switches = %+v, want one", open)
	}
}

// 19 calls are under the 20 a median needs: no switch.
func TestSlowAliasNeedsTwentyCalls(t *testing.T) {
	r := newRouteRig(t)
	seedSlow(r.store, "big", "zai/glm-5.3", 19, 39)
	r.call(t, 1)
	if got := r.modelsSent(t); got[0] != "big" || len(r.store.switchRows()) != 0 {
		t.Fatalf("upstream models %v, switches %+v, want big and none", got, r.store.switchRows())
	}
}

// Calls under 100 output tokens and calls of another deployment do not
// count: 10 slow calls of 99 tokens and 10 of the twin's deployment leave
// 10 counted calls.
func TestSlowAliasCountsOnlyPrimaryCallsOfEnoughTokens(t *testing.T) {
	r := newRouteRig(t)
	seedSlow(r.store, "big", "zai/glm-5.3", 10, 39)
	for range 10 {
		r.store.addAudit(AuditRow{Decision: "allowed", ModelAlias: "big", Deployment: "zai/glm-5.3", OutputTokens: 99, DurationMS: 90000, Status: "200"})
	}
	seedSlow(r.store, "big", "dashscope/qwen3.8-max", 10, 39)
	seedSlow(r.store, "big-alibaba", "dashscope/qwen3.8-max", 5, 5)
	r.call(t, 1)
	if got := r.modelsSent(t); got[0] != "big" {
		t.Fatalf("upstream models = %v, want big: 10 counted calls are under 20", got)
	}
}

// A switched alias sends every 10th call to the primary: calls 1 to 9 go
// to the twin, call 10 to big.
func TestSwitchedAliasSendsEveryTenthCallToThePrimary(t *testing.T) {
	r := newRouteRig(t)
	seedSlow(r.store, "big", "zai/glm-5.3", 20, 39)
	r.call(t, 10)
	got := r.modelsSent(t)
	want := append(strings.Split(strings.Repeat("big-alibaba,", 9), ",")[:9], "big")
	if !slicesEqual(got, want) {
		t.Fatalf("upstream models = %v, want %v", got, want)
	}
	r.call(t, 1)
	if got := r.modelsSent(t); got[10] != "big-alibaba" {
		t.Fatalf("call 11 went to %s, want big-alibaba", got[10])
	}
}

// A median back at 30, the bound itself, returns the alias to its primary
// and stores the return.
func TestSlowAliasReturnsWhenTheMedianIsBackAtTheBound(t *testing.T) {
	r := newRouteRig(t)
	seedSlow(r.store, "big", "zai/glm-5.3", 20, 39)
	r.call(t, 1)
	r.store.mu.Lock()
	r.store.audits = nil
	r.store.mu.Unlock()
	seedSlow(r.store, "big", "zai/glm-5.3", 20, 30)
	r.advance(2 * time.Minute)
	r.call(t, 1)
	if got := r.modelsSent(t); got[1] != "big" {
		t.Fatalf("upstream models = %v, want the second call on big", got)
	}
	sw := r.store.switchRows()
	if len(sw) != 2 || sw[1].Kind != policy.SwitchReturn || sw[1].Rate != 30 {
		t.Fatalf("switch rows = %+v, want a switch then a return at 30", sw)
	}
	if open, _ := r.store.OpenAliasSwitches(t.Context()); len(open) != 0 {
		t.Fatalf("open switches = %+v, want none", open)
	}
}

// switchedThenAged switches big on 20 slow calls, then ages them out of the
// window: the window is empty and two minutes have passed.
func switchedThenAged(t *testing.T) *routeRig {
	r := newRouteRig(t)
	seedSlow(r.store, "big", "zai/glm-5.3", 20, 39)
	r.call(t, 1)
	r.store.mu.Lock()
	r.store.audits = nil
	r.store.mu.Unlock()
	r.advance(2 * time.Minute)
	return r
}

// A switched alias returns on 3 probes at 25 s per 1000 tokens: the median
// is 25, at or under 30. Two probes are too few: it stays on its twin.
func TestSwitchedAliasReturnsOnThreeProbes(t *testing.T) {
	r := switchedThenAged(t)
	seedSlow(r.store, "big", "zai/glm-5.3", 3, 25)
	r.call(t, 1)
	if got := r.modelsSent(t); got[1] != "big" {
		t.Fatalf("upstream models = %v, want the second call on big", got)
	}
	if sw := r.store.switchRows(); len(sw) != 2 || sw[1].Kind != policy.SwitchReturn || sw[1].Rate != 25 {
		t.Fatalf("switch rows = %+v, want a switch then a return at 25", sw)
	}
}

// Two probes are under the 3 a return needs, so the alias asks for more: the
// next call goes to the primary again (call 2), the one after to the twin.
func TestSwitchedAliasProbesAgainOnTwoProbes(t *testing.T) {
	r := switchedThenAged(t)
	seedSlow(r.store, "big", "zai/glm-5.3", 2, 25)
	r.call(t, 2)
	got := r.modelsSent(t)
	if got[1] != "big" || got[2] != "big-alibaba" || len(r.store.switchRows()) != 1 {
		t.Fatalf("upstream models %v, switches %+v, want big then big-alibaba and the one switch", got, r.store.switchRows())
	}
}

// With no primary call in the window, the next call goes to the primary and
// the one after to the twin again; no return is stored.
func TestSwitchedAliasProbesAfterAQuietHour(t *testing.T) {
	r := switchedThenAged(t)
	r.call(t, 2)
	if got := r.modelsSent(t); got[1] != "big" || got[2] != "big-alibaba" || len(r.store.switchRows()) != 1 {
		t.Fatalf("upstream models %v, switches %+v, want big then big-alibaba and the one switch", got, r.store.switchRows())
	}
}

// An alias without a twin never switches, however slow.
func TestAliasWithoutTwinNeverSwitches(t *testing.T) {
	r := newRouteRig(t)
	pv := testPolicy(1, 20)
	pv.Tree.Document.Classes["coder"] = policy.Class{Models: []string{"small", "big", "premium"}}
	pv.Tree.Document.Prices.MaxOutputTokens["premium"] = 32768
	r.store.setPolicy(pv)
	r.store.tasks["42"] = Task{Classes: coder, ModelAlias: "premium", ID: "42", BudgetEUR: 100, Status: "running"}
	r.srv.opts.Prices = fakePrices{"premium": testPrice}
	seedSlow(r.store, "premium", "openai/gpt-6.1-sol", 20, 90)
	r.callOn(t, "premium", 1)
	if got := r.modelsSent(t); got[0] != "premium" || len(r.store.switchRows()) != 0 {
		t.Fatalf("upstream models %v, switches %+v, want premium and none", got, r.store.switchRows())
	}
}

// The task's key allows each alias's twin, beside the aliases of its classes.
func TestKeyAllowsTheTwins(t *testing.T) {
	r := newRouteRig(t)
	r.call(t, 1)
	calls := r.minter.mintCalls()
	if len(calls) != 1 || calls[0].models != "big,big-alibaba,small,small-alibaba" {
		t.Fatalf("mint calls = %+v, want the models big,big-alibaba,small,small-alibaba", calls)
	}
}

func slicesEqual(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

// LiteLLM's failure fallback serves a call of big on Qwen before any switch,
// and no call of big-alibaba is in the window. 10 z.ai calls at 39 and 10
// fallback calls at 5: only the 10 z.ai calls count, under the 20 a switch
// needs.
func TestFallbackCallsAreNoPrimarySample(t *testing.T) {
	r := newRouteRig(t)
	seedSlow(r.store, "big", "zai/glm-5.3", 10, 39)
	seedSlow(r.store, "big", "dashscope/qwen3.8-max", 10, 5)
	r.call(t, 1)
	if got := r.modelsSent(t); got[0] != "big" || len(r.store.switchRows()) != 0 {
		t.Fatalf("upstream models %v, switches %+v, want big and none: 10 counted calls", got, r.store.switchRows())
	}
}

// 19 z.ai calls at 10 and 20 fallback calls at 120 (their retry waits): 19
// counted calls, no switch, though the whole alias's window is slow.
func TestSlowFallbackCallsDoNotSwitch(t *testing.T) {
	r := newRouteRig(t)
	seedSlow(r.store, "big", "zai/glm-5.3", 19, 10)
	seedSlow(r.store, "big", "dashscope/qwen3.8-max", 20, 120)
	r.call(t, 1)
	if got := r.modelsSent(t); got[0] != "big" || len(r.store.switchRows()) != 0 {
		t.Fatalf("upstream models %v, switches %+v, want big and none", got, r.store.switchRows())
	}
}

// A twin call that LiteLLM falls back to the primary counts as a primary
// sample by its deployment: 2 probes at 25 on big and one twin-alias call
// that z.ai answered at 25 are 3 counted calls at 25, so the alias returns.
func TestTwinCallFallenBackToThePrimaryCounts(t *testing.T) {
	r := switchedThenAged(t)
	seedSlow(r.store, "big", "zai/glm-5.3", 2, 25)
	seedSlow(r.store, "big-alibaba", "zai/glm-5.3", 1, 25)
	r.call(t, 1)
	if sw := r.store.switchRows(); len(sw) != 2 || sw[1].Kind != policy.SwitchReturn || sw[1].Rate != 25 {
		t.Fatalf("switch rows = %+v, want a switch then a return at 25", sw)
	}
}

// The rate is the window's sum of duration over its sum of output tokens: 10
// calls of 1000 tokens at 20 s and 10 of 100 tokens at 6 s (60 s per 1000)
// make 260000 ms over 11000 tokens, 23.6 s per 1000, under the bound. Their
// median would be (20 + 60) / 2 = 40.
func TestSlowAliasRateIsTheRatioOfSums(t *testing.T) {
	r := newRouteRig(t)
	seedSlow(r.store, "big", "zai/glm-5.3", 10, 20)
	for range 10 {
		r.store.addAudit(AuditRow{Decision: "allowed", ModelAlias: "big", Deployment: "zai/glm-5.3", OutputTokens: 100, DurationMS: 6000, Status: "200"})
	}
	r.call(t, 1)
	if got := r.modelsSent(t); got[0] != "big" || len(r.store.switchRows()) != 0 {
		t.Fatalf("upstream models %v, switches %+v, want big and none", got, r.store.switchRows())
	}
	r.store.mu.Lock()
	r.store.audits = nil
	r.store.mu.Unlock()
	seedSlow(r.store, "big", "zai/glm-5.3", 10, 20)
	for range 10 {
		r.store.addAudit(AuditRow{Decision: "allowed", ModelAlias: "big", Deployment: "zai/glm-5.3", OutputTokens: 100, DurationMS: 20000, Status: "200"})
	}
	r.advance(2 * time.Minute)
	// 200000 + 200000 ms over 11000 tokens: 36.4 s per 1000
	r.call(t, 1)
	if sw := r.store.switchRows(); len(sw) != 1 || sw[0].Rate < 36.3 || sw[0].Rate > 36.4 {
		t.Fatalf("switch rows = %+v, want a switch at 400000/11000 = 36.36", sw)
	}
}

// A twin LiteLLM cannot price is no switch.
func TestSlowAliasStaysWhenItsTwinCannotBePriced(t *testing.T) {
	r := newRouteRig(t)
	r.srv.opts.Prices = fakePrices{"small": testPrice, "big": testPrice}
	seedSlow(r.store, "big", "zai/glm-5.3", 20, 39)
	r.call(t, 1)
	if got := r.modelsSent(t); got[0] != "big" || len(r.store.switchRows()) != 0 {
		t.Fatalf("upstream models %v, switches %+v, want big and none", got, r.store.switchRows())
	}
}

// A call the hold refuses does not use up the probe: calls 1 to 8 go to the
// twin, a refused call is no call 9, so the next served call is the 9th (the
// twin) and the one after it the 10th, the primary.
func TestRefusedCallKeepsTheProbe(t *testing.T) {
	r := newRouteRig(t)
	seedSlow(r.store, "big", "zai/glm-5.3", 20, 39)
	r.call(t, 8)
	r.store.mu.Lock()
	r.store.tasks["42"] = Task{Classes: coder, ModelAlias: "big", ID: "42", BudgetEUR: 0, Status: "running"}
	r.store.mu.Unlock()
	if rr := doChat(t, r.h, chatBody("big")); rr.Code == http.StatusOK {
		t.Fatal("a call on a task without budget was served")
	}
	r.store.mu.Lock()
	r.store.tasks["42"] = Task{Classes: coder, ModelAlias: "big", ID: "42", BudgetEUR: 100, Status: "running"}
	r.store.mu.Unlock()
	r.call(t, 2)
	got := r.modelsSent(t)
	if len(got) != 10 || got[8] != "big-alibaba" || got[9] != "big" {
		t.Fatalf("upstream models = %v, want call 9 on big-alibaba and call 10 on big", got)
	}
}

// When the policy names another twin, or none, the open switch closes: a
// return is stored and the call is served on the primary.
func TestPolicyChangeClosesTheSwitch(t *testing.T) {
	for name, twins := range map[string]map[string]string{"another twin": {"big": "big-backup"}, "no twin": {"small": "small-alibaba"}} {
		t.Run(name, func(t *testing.T) {
			r := newRouteRig(t)
			seedSlow(r.store, "big", "zai/glm-5.3", 20, 39)
			r.call(t, 1)
			pv := testPolicy(2, 20)
			pv.Tree.Document.Routing.Twins = twins
			r.store.setPolicy(pv)
			r.advance(2 * time.Minute)
			r.call(t, 1)
			got, sw := r.modelsSent(t), r.store.switchRows()
			if got[1] != "big" || len(sw) != 2 || sw[1].Kind != policy.SwitchReturn || sw[1].Twin != "big-alibaba" {
				t.Fatalf("upstream models %v, switches %+v, want the second call on big and a return of big-alibaba", got, sw)
			}
			if open, _ := r.store.OpenAliasSwitches(t.Context()); len(open) != 0 {
				t.Fatalf("open switches = %+v, want none", open)
			}
		})
	}
}

// The window door answers the smaller window of the alias and its twin.
func TestWindowIsTheSmallerOfAliasAndTwin(t *testing.T) {
	r := newRouteRig(t)
	r.srv.opts.Prices = fakePrices{"big": {Window: 200000}, "big-alibaba": {Window: 128000}, "premium": {Window: 400000}}
	for alias, want := range map[string]int{"big": 128000, "premium": 400000} {
		rr := httptest.NewRecorder()
		r.h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/window/"+alias, nil))
		var got struct{ Window int }
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || got.Window != want {
			t.Fatalf("window of %s = %s, want %d", alias, rr.Body, want)
		}
	}
}

type timedSample struct {
	at time.Time
	s  policy.LatencySample
}

// agingStore is a memStore whose latency window ages its rows out.
type agingStore struct {
	*memStore
	rows []timedSample
}

func (a *agingStore) LatencySamples(_ context.Context, aliases []string, since time.Time) ([]policy.LatencySample, error) {
	var out []policy.LatencySample
	for _, r := range a.rows {
		if !r.at.Before(since) && slices.Contains(aliases, r.s.Alias) {
			out = append(out, r.s)
		}
	}
	return out, nil
}

// A switched alias returns at every call rate once the primary has
// recovered to 20 s per 1000 tokens: 20 slow calls switch it, then 6 hours
// of calls with their rows aging out of the window. At 6 calls an hour the
// probes alone (one a minute until 3 counted calls) bring it back.
func TestSwitchedAliasReturnsAtLowTraffic(t *testing.T) {
	for _, perHour := range []int{6, 15, 25, 40} {
		clock := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
		st := &agingStore{memStore: newMemStore(Task{Classes: coder, ModelAlias: "big", ID: "42", BudgetEUR: 100, Status: "running"})}
		for range 20 {
			st.rows = append(st.rows, timedSample{clock, policy.LatencySample{Alias: "big", Deployment: "zai/glm-5.3", OutputTokens: 1000, DurationMS: 39000}})
		}
		s := NewServer(ServerOptions{Store: st, Prices: fakePrices{"big": testPrice, "big-alibaba": testPrice}})
		s.now = func() time.Time { return clock }
		pv := testPolicy(1, 20)
		step := time.Hour / time.Duration(perHour)
		returnedAt, primary := time.Duration(-1), 0
		for i := range perHour * 6 {
			rt := s.route(t.Context(), &pv, "big")
			if rt.alias == "big" {
				primary++
				st.rows = append(st.rows, timedSample{clock, policy.LatencySample{Alias: "big", Deployment: "zai/glm-5.3", OutputTokens: 1000, DurationMS: 20000}})
			} else {
				st.rows = append(st.rows, timedSample{clock, policy.LatencySample{Alias: "big-alibaba", Deployment: "dashscope/qwen3.8-max", OutputTokens: 1000, DurationMS: 10000}})
			}
			if returnedAt < 0 && len(st.switchRows()) == 2 {
				returnedAt = time.Duration(i) * step
			}
			clock = clock.Add(step)
		}
		t.Logf("%d calls/hour over 6 h: %d primary calls, returned after %v", perHour, primary, returnedAt)
		if returnedAt < 0 {
			t.Errorf("%d calls an hour: the alias never returned", perHour)
		}
	}
}

// 100 concurrent calls of a switched alias: exactly 1 in 10 reaches the
// primary.
func TestConcurrentCallsKeepTheProbeSchedule(t *testing.T) {
	r := newRouteRig(t)
	seedSlow(r.store, "big", "zai/glm-5.3", 20, 39)
	r.call(t, 1)
	var wg sync.WaitGroup
	for range 99 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			doChat(t, r.h, chatBody("big"))
		}()
	}
	wg.Wait()
	n := map[string]int{}
	for _, m := range r.modelsSent(t) {
		n[m]++
	}
	if n["big"] != 10 || n["big-alibaba"] != 90 {
		t.Fatalf("models sent = %v, want 10 on big and 90 on big-alibaba", n)
	}
}
