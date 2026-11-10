package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
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
	if len(sw) != 1 || sw[0].Kind != policy.SwitchOn || sw[0].Alias != "big" || sw[0].Twin != "big-alibaba" || sw[0].Median != 39 || sw[0].Deployment != "zai/glm-5.3" {
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
	if len(sw) != 2 || sw[1].Kind != policy.SwitchReturn || sw[1].Median != 30 {
		t.Fatalf("switch rows = %+v, want a switch then a return at 30", sw)
	}
	if open, _ := r.store.OpenAliasSwitches(t.Context()); len(open) != 0 {
		t.Fatalf("open switches = %+v, want none", open)
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
