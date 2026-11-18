package acceptance

// The provider limit's acceptance proofs at the compiled gateway: a real
// gateway.NewServer stood on the pool-gateway rig with a policy version
// that names provider_limits, and a stub upstream that serves the
// provider's answers. The proofs walk the plan's first five gateway
// criteria: today's zai body freezes the class with the time zone read
// right; only a dated error freezes, the undated ladder keeps today's
// error_upstream path; a provider's own pattern set replaces the default
// alone; the hold answers every work call locally before the reset, no
// wire call, and a call after the lapse probes once and re-arms; the
// class_frozen and class_resumed events land with the class, the reset
// and the words.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	factorygateway "github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// plZaiBody is today's zai refusal: the naive stamp the incident carried,
// which policy reads as Asia/Singapore.
const plZaiBody = `{"error":{"message":"Usage limit reached for 5 hour. Your limit will reset at 2026-11-15 01:04:50","type":"rate_limit_error","param":null,"code":"429"}}`

// plWords answers the words a refusal body carries: the upstream message
// the gateway echoes back.
func plWords(t *testing.T, body string) string {
	t.Helper()
	var wire struct {
		Error struct{ Message string }
	}
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		t.Fatalf("the refusal body %s: %v", body, err)
	}
	return wire.Error.Message
}

// plPolicy is the policy version the provider-limit proofs hold: the
// rig's classes each pointed at their own provider, zai's entry with
// Asia/Singapore, utc's with no zone (UTC by the default), custom's with
// a pattern set of its own. Waits left at the defaults unless a proof
// overrides them.
func plPolicy() policy.Version {
	return policy.Version{ID: 7, Commit: "pl-7", Tree: policy.Tree{
		Document: policy.Document{
			Version: 1,
			Budget:  policy.Budget{TaskDefaultEUR: 2, TaskMaxEUR: 5, DayEUR: gwDayEUR, MonthEUR: gwMonthEUR},
			Limits: policy.Limits{Sessions: 3, AgentTime: 20 * time.Minute, SupervisorCalls: 3,
				SupervisorTimeout: 6 * time.Minute, StrongerAfter: 1},
			Risk:            policy.Risk{Classes: []policy.RiskClass{{Name: "normal", Outcome: policy.OutcomeRequiresApproval}}, Default: "normal"},
			EnvelopeDefault: policy.Envelope{MonthlyEUR: 10, ServerTypes: []string{"cx23"}, MaxCount: 2, MaxLeaseHours: 4},
			Classes: map[string]policy.Class{
				"zai-class":    {Models: []string{"small"}, Provider: "zai"},
				"utc-class":    {Models: []string{"small"}, Provider: "utcprov"},
				"custom-class": {Models: []string{"small"}, Provider: "customprov"},
			},
			ProviderLimits: map[string]policy.ProviderLimit{
				"zai":        {TZ: "Asia/Singapore"},
				"utcprov":    {},
				"customprov": {Patterns: []string{`(?i)window opens[:\s]+(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})`}},
			},
			Prices: policy.ToolPrices{MaxOutputTokens: map[string]int{"small": 32768, "big": 32768}},
		},
	}}
}

// plTask is the card the proofs call as: a running coder of the code
// project on the class named, allocated EUR 100 of its own and none
// spent.
func plTask(class string) factorygateway.Task {
	return factorygateway.Task{ID: "t-pl", BudgetEUR: 100, Status: "running",
		Classes: []string{class}, Repository: "sf/code", Source: "card", ModelAlias: "small"}
}

// plServer stands the compiled gateway on the rig for task under pv,
// with the stub upstream and a recorder minter; it answers the handler
// and the store, the store holding the policy version in force.
func plServer(t *testing.T, task factorygateway.Task, pv policy.Version, stub *accStubLiteLLM) (http.Handler, *accGatewayStore, *accGatewayMinter) {
	t.Helper()
	store := newAccGatewayStore(task)
	store.setPolicy(pv)
	minter := &accGatewayMinter{}
	h := factorygateway.NewServer(factorygateway.ServerOptions{
		Auth:       accGatewayAuth{task: task.ID},
		Store:      store,
		Minter:     minter,
		Prices:     accGatewayPrices{price: gwPrice(gwBodyBytes(t)), also: map[string]policy.Price{"big": gwPrice(gwBodyBytes(t))}},
		LiteLLMURL: stub.URL,
	}).Routes("pl-proof")
	return h, store, minter
}

// plPost posts one chat call at the handler as the proof's card.
func plPost(t *testing.T, h http.Handler) *http.Response {
	t.Helper()
	rec := gwPost(h, gwChatBody(t))
	return rec.Result()
}

// plFrozen asserts one call answered the named freeze: status 429, the
// decision header, the body carrying the class, the reset and the
// provider's own words, and the audit row of the decision.
func plFrozen(t *testing.T, what string, rec *http.Response, store *accGatewayStore, class, reset, words string) {
	t.Helper()
	if rec.StatusCode != http.StatusTooManyRequests {
		raw := make([]byte, 4096)
		n, _ := rec.Body.Read(raw)
		t.Fatalf("%s = %d, want 429:\n%s", what, rec.StatusCode, raw[:n])
	}
	if got := rec.Header.Get(factorygateway.HeaderDecision); got != factorygateway.RefusedProviderLimit {
		t.Fatalf("%s's decision = %q, want %q", what, got, factorygateway.RefusedProviderLimit)
	}
	var body struct {
		Error struct{ Message string }
		Class string
		Reset string
		Words string
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("%s's body: %v", what, err)
	}
	if body.Class != class || body.Reset != reset || body.Words != words || body.Error.Message != words {
		t.Fatalf("%s's body = {class %q reset %q words %q message %q}, want class %q reset %q words %q",
			what, body.Class, body.Reset, body.Words, body.Error.Message, class, reset, words)
	}
	rows := store.auditRows()
	if len(rows) == 0 || rows[len(rows)-1].Decision != factorygateway.RefusedProviderLimit {
		t.Fatalf("%s wrote %+v, want a %s row", what, rows, factorygateway.RefusedProviderLimit)
	}
	if want := "class " + class + ": reset " + reset + ": " + words; !strings.Contains(rows[len(rows)-1].Reason, want) {
		t.Fatalf("%s's reason = %q, want the class, the reset and the words %q", what, rows[len(rows)-1].Reason, want)
	}
}

// TestGatewayZaiBodyFreeze: the compiled gateway maps today's zai body to
// the named class freeze with the time zone read right — the body
// datetime parses in the provider's tz (Singapore read, parsed plus one
// minute), a naive stamp with no provider zone reads UTC, and a stamp
// with an offset reads as is. [parse_reads_zai_body]
func TestGatewayZaiBodyFreeze(t *testing.T) {
	pv := plPolicy()
	stub := newAccStubLiteLLM(t)
	words := plWords(t, plZaiBody)

	// the zai case: Asia/Singapore reads 01:04:50 as 17:04:50 UTC, the
	// answer carrying its minute's margin
	h, store, _ := plServer(t, plTask("zai-class"), pv, stub)
	stub.serve(http.StatusTooManyRequests, plZaiBody, nil)
	plFrozen(t, "the zai body", plPost(t, h), store, "zai-class", "2026-11-14T17:05:50Z", words)

	// the no-entry zone: the entry stands but names no tz, so the naive
	// stamp reads UTC
	h, store, _ = plServer(t, plTask("utc-class"), pv, stub)
	stub.serve(http.StatusTooManyRequests, plZaiBody, nil)
	plFrozen(t, "the naive stamp without a zone", plPost(t, h), store, "utc-class", "2026-11-15T01:05:50Z", words)

	// the offset header: the stamp carries its offset and reads as is,
	// the Singapore offset again naming 17:04:50 UTC
	h, store, _ = plServer(t, plTask("zai-class"), pv, stub)
	stub.serve(http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`,
		map[string]string{"x-ratelimit-reset": "2026-11-15T01:04:50+08:00"})
	plFrozen(t, "the offset reset header", plPost(t, h), store, "zai-class", "2026-11-14T17:05:50Z", "rate limited")
}

// TestGatewayUndatedKeepsLadder: every undated error — the bare 429, the
// relative hint in words, the 402 with no readable datetime, the
// seconds-form Retry-After, OpenAI's and Google's relative hints — keeps
// today's error_upstream path: no decision header, no hold armed, and a
// later call reaches the upstream again. [only_dated_errors_freeze]
func TestGatewayUndatedKeepsLadder(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		headers    map[string]string
		wantStatus int
		wantReason string
	}{
		{"a bare 429", http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`, nil, http.StatusTooManyRequests, "upstream 429"},
		{"a relative hint in words", http.StatusTooManyRequests, `{"error":{"message":"rate limited, try again in 1 second"}}`, nil, http.StatusTooManyRequests, "upstream 429"},
		{"a 402 with no readable datetime", http.StatusPaymentRequired, `{"error":{"message":"payment required"}}`, nil, http.StatusPaymentRequired, "upstream 402"},
		{"a seconds-form Retry-After", http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`,
			map[string]string{"Retry-After": "30"}, http.StatusTooManyRequests, "upstream 429"},
		{"an x-ratelimit-reset-requests relative hint", http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`,
			map[string]string{"x-ratelimit-reset-requests": "1s", "x-ratelimit-reset-tokens": "6m0s"}, http.StatusTooManyRequests, "upstream 429"},
		{"a retryDelay relative hint", http.StatusTooManyRequests, `{"error":{"message":"Resource has been exhausted, retryDelay: 32s"}}`, nil, http.StatusTooManyRequests, "upstream 429"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			stub := newAccStubLiteLLM(t)
			h, store, _ := plServer(t, plTask("zai-class"), plPolicy(), stub)
			stub.serve(tt.status, tt.body, tt.headers)
			rec := plPost(t, h)
			if rec.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.StatusCode, tt.wantStatus)
			}
			if got := rec.Header.Get(factorygateway.HeaderDecision); got != "" {
				t.Fatalf("decision header = %q, want none", got)
			}
			rows := store.auditRows()
			if len(rows) == 0 || rows[len(rows)-1].Decision != "error_upstream" || rows[len(rows)-1].Reason != tt.wantReason {
				t.Fatalf("audit rows %+v, want error_upstream %q", rows, tt.wantReason)
			}
			if events := store.eventRows(); len(events) != 0 {
				t.Fatalf("events %+v, want none: an undated error never arms a hold", events)
			}
			// the ladder keeps serving: a later call reaches the upstream
			stub.serve(http.StatusOK, "{}", nil)
			rec2 := plPost(t, h)
			if rec2.StatusCode != http.StatusOK {
				t.Fatalf("the later call = %d, want the upstream's 200", rec2.StatusCode)
			}
			if calls := stub.calls(); len(calls) != 2 {
				t.Fatalf("upstream calls %v, want both on the wire", calls)
			}
		})
	}
}

// TestProviderLimitPatternSets: a provider's own patterns replace the
// default set alone — its new wording freezes, a default-only wording
// does not — and an entry with none freezes on the default set's shapes.
// [pattern_sets_replace]
func TestProviderLimitPatternSets(t *testing.T) {
	pv := plPolicy()
	stub := newAccStubLiteLLM(t)
	custom := `{"error":{"message":"next window opens 2026-11-15 01:04:50"}}`

	// the custom set alone: its own wording freezes
	h, store, _ := plServer(t, plTask("custom-class"), pv, stub)
	stub.serve(http.StatusTooManyRequests, custom, nil)
	plFrozen(t, "the custom wording", plPost(t, h), store, "custom-class", "2026-11-15T01:05:50Z", plWords(t, custom))

	// and a default-set wording does not freeze under the custom set
	h, store, _ = plServer(t, plTask("custom-class"), pv, stub)
	stub.serve(http.StatusTooManyRequests, plZaiBody, nil)
	rec := plPost(t, h)
	if rec.StatusCode != http.StatusTooManyRequests || rec.Header.Get(factorygateway.HeaderDecision) != "" {
		t.Fatalf("the default wording under the custom set = %d %q, want the plain passthrough",
			rec.StatusCode, rec.Header.Get(factorygateway.HeaderDecision))
	}
	if rows := store.auditRows(); len(rows) == 0 || rows[len(rows)-1].Reason != "upstream 429" {
		t.Fatalf("audit rows %+v, want upstream 429", rows)
	}

	// an entry with no patterns freezes on the default set's shapes
	h, store, _ = plServer(t, plTask("utc-class"), pv, stub)
	stub.serve(http.StatusTooManyRequests, plZaiBody, nil)
	plFrozen(t, "the entry without patterns", plPost(t, h), store, "utc-class", "2026-11-15T01:05:50Z", plWords(t, plZaiBody))
}

// TestGatewayHoldLocalRefusal: the hold arms on the first dated refusal,
// every work call inside the freeze is answered locally with the same
// named refusal and no wire call, a call after the provider_recheck lapse
// probes the upstream exactly once and re-arms the hold at the new reset,
// and the class_frozen and class_resumed events land with the class, the
// reset and the words. The proof's policy names provider_recheck at its
// floor of one minute, so the lapse costs a minute of wall clock.
// [class_hold_local_refusal]
func TestGatewayHoldLocalRefusal(t *testing.T) {
	pv := plPolicy()
	pv.Tree.Document.Waits = policy.Waits{ProviderRecheck: time.Minute}
	stub := newAccStubLiteLLM(t)
	task := plTask("zai-class")
	h, store, _ := plServer(t, task, pv, stub)

	// the arming call: the provider's dated refusal freezes the class at
	// the Singapore read plus its minute's margin
	first := `{"error":{"message":"Usage limit reached. Your limit will reset at 2027-01-05 05:00:00","type":"rate_limit_error","param":null,"code":"429"}}`
	words := plWords(t, first)
	stub.serve(http.StatusTooManyRequests, first, nil)
	plFrozen(t, "the arming call", plPost(t, h), store, "zai-class", "2027-01-04T21:01:00Z", words)
	if calls := stub.calls(); len(calls) != 1 {
		t.Fatalf("upstream calls after the arm %v, want the one", calls)
	}

	// a second work call inside the freeze is answered locally: the same
	// until, no upstream request on the wire
	plFrozen(t, "the held call", plPost(t, h), store, "zai-class", "2027-01-04T21:01:00Z", words)
	if calls := stub.calls(); len(calls) != 1 {
		t.Fatalf("upstream calls after the held call %v, want one: the freeze answers locally", calls)
	}
	rows := store.auditRows()
	if len(rows) != 2 || !strings.Contains(rows[1].Reason, "provider limit hold for class zai-class") {
		t.Fatalf("audit rows %+v, want a local refused_provider_limit row", rows)
	}

	// a call after the lapse probes the upstream exactly once; the stub
	// serves a new dated refusal and the hold re-arms at the new reset
	renewed := `{"error":{"message":"Usage limit reached. Your limit will reset at 2027-01-05 09:00:00","type":"rate_limit_error","param":null,"code":"429"}}`
	time.Sleep(65 * time.Second) // past the one-minute recheck lapse
	stub.serve(http.StatusTooManyRequests, renewed, nil)
	plFrozen(t, "the post-lapse call", plPost(t, h), store, "zai-class", "2027-01-05T01:01:00Z", plWords(t, renewed))
	if calls := stub.calls(); len(calls) != 2 {
		t.Fatalf("upstream calls after the post-lapse call %v, want two: one probe", calls)
	}

	// and the freeze is back: a further call is answered locally again
	plFrozen(t, "the re-frozen call", plPost(t, h), store, "zai-class", "2027-01-05T01:01:00Z", plWords(t, renewed))
	if calls := stub.calls(); len(calls) != 2 {
		t.Fatalf("upstream calls after the re-frozen call %v, want two", calls)
	}

	// the events land with the class, the reset and the words: frozen on
	// the arm, resumed on the lapse, frozen again on the re-arm
	events := store.eventRows()
	kinds := []string{"class_frozen", "class_resumed", "class_frozen"}
	resets := []string{"2027-01-04T21:01:00Z", "2027-01-04T21:01:00Z", "2027-01-05T01:01:00Z"}
	if len(events) != len(kinds) {
		t.Fatalf("events %+v, want %d rows", events, len(kinds))
	}
	for i, e := range events {
		if e.Kind != kinds[i] || e.TaskID != task.ID || e.Repository != task.Repository {
			t.Fatalf("event %d = %+v, want kind %s on %s (%s)", i, e, kinds[i], task.ID, task.Repository)
		}
		if e.Detail["class"] != "zai-class" || e.Detail["reset"] != resets[i] {
			t.Fatalf("event %d detail = %+v, want class zai-class and reset %s", i, e.Detail, resets[i])
		}
		if w, _ := e.Detail["words"].(string); !strings.Contains(w, "reset at") {
			t.Fatalf("event %d detail = %+v, want the provider's words", i, e.Detail)
		}
	}
}
