package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

type logCapture struct {
	mu   sync.Mutex
	recs []sdklog.Record
}

func (c *logCapture) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
func (c *logCapture) OnEmit(_ context.Context, r *sdklog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r.Clone())
	return nil
}
func (c *logCapture) Shutdown(context.Context) error   { return nil }
func (c *logCapture) ForceFlush(context.Context) error { return nil }

// attributes are each record's attributes as strings.
func (c *logCapture) attributes() []map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]string
	for _, r := range c.recs {
		m := map[string]string{"body": r.Body().String()}
		r.WalkAttributes(func(kv otellog.KeyValue) bool {
			m[kv.Key] = kv.Value.String()
			return true
		})
		out = append(out, m)
	}
	return out
}

type telemetryRig struct {
	tel     *Telemetry
	spans   *tracetest.SpanRecorder
	metrics *sdkmetric.ManualReader
	logs    *logCapture
}

func newTelemetryRig(t *testing.T, secrets ...string) telemetryRig {
	t.Helper()
	r := telemetryRig{spans: tracetest.NewSpanRecorder(), metrics: sdkmetric.NewManualReader(), logs: &logCapture{}}
	var err error
	r.tel, err = NewTelemetry(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(r.spans)),
		sdkmetric.NewMeterProvider(sdkmetric.WithReader(r.metrics)),
		sdklog.NewLoggerProvider(sdklog.WithProcessor(r.logs)), secrets...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// metric values by name and encoded attribute set: a counter's sum, a
// histogram's sample count.
func (r telemetryRig) collect(t *testing.T) map[string]map[string]float64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.metrics.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			vals := map[string]float64{}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					vals[dp.Attributes.Encoded(attribute.DefaultEncoder())] = float64(dp.Value)
				}
			case metricdata.Sum[float64]:
				for _, dp := range d.DataPoints {
					vals[dp.Attributes.Encoded(attribute.DefaultEncoder())] = dp.Value
				}
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					vals[dp.Attributes.Encoded(attribute.DefaultEncoder())] = float64(dp.Count)
				}
			}
			out[m.Name] = vals
		}
	}
	return out
}

func spanAttrs(s sdktrace.ReadOnlySpan) map[string]string {
	out := map[string]string{}
	for _, kv := range s.Attributes() {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

func TestScrubRemovesKeysAndBodies(t *testing.T) {
	s := newScrubber("hunter2-master")
	for in, want := range map[string]string{
		`provider answered 402: {"error":"Insufficient credits","key":"fc-123"}; cost not reported, booked the hold 0.005000 EUR`:  "provider answered 402: [body dropped]; cost not reported, booked the hold 0.005000 EUR",
		`provider answered 500: {"error":"stub failure"}`:                                                                          "provider answered 500: [body dropped]",
		`key generate: status 500: {"error":{"message":"db down"}}`:                                                                "key generate: status 500: [body dropped]",
		`key block: status 404: {"detail":"key sk-x not found"}`:                                                                   "key block: status 404: [body dropped]",
		"replicate: unreadable prediction: <html><body>Bad gateway</body></html>; cost not reported, booked the hold 0.009000 EUR": "replicate: unreadable prediction: [body dropped]; cost not reported, booked the hold 0.009000 EUR",
		"Authorization: Bearer abc.def":                                                                                            "Authorization: Bearer [redacted]",
		"Authorization: Basic dXNlcjpwYXNzd29yZA==":                                                                                "Authorization: Basic [redacted]",
		"clone https://x-access-token:ghp_abc123shortpass@git.example/sf/repo.git failed":                                          "clone https://[redacted]@git.example/sf/repo.git failed",
		"litellm refused key sk-task-key-1: blocked":                                                                               "litellm refused key [redacted]: blocked",
		"token eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln refused":                                                                  "token [redacted] refused",
		"hetzner token 0123456789abcdefghijABCDEFGHIJ0123456789 refused":                                                           "hetzner token [redacted] refused",
		"hetzner token AAAAAAAAAAAAAAAAAAAAbbbbbbbbbbbbbbbbbbbb01234567890123456789wxyz refused":                                   "hetzner token [redacted] refused",
		"fal key 0b7e3f1a-2c4d-4e5f-8a9b-1c2d3e4f5a6b:0123456789abcdef0123456789abcdef refused":                                    "fal key [redacted]:[redacted] refused",
		"replicate token r8_0123456789abcdefghijABCDEFGHIJ01234 refused":                                                           "replicate token [redacted] refused",
		"firecrawl key fc-0123456789abcdef0123456789abcdef refused":                                                                "firecrawl key [redacted] refused",
		"openbao token hvs.CAESIJ0123456789abcdefghijklmnopqrstuvwxyz expired":                                                     "openbao token hvs.[redacted] expired",
		"key -----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\nQyNTUxOQAAACD=\n-----END OPENSSH PRIVATE KEY----- rejected": "key [pem redacted] rejected",
		"the master key hunter2-master is in use":                                                     "the master key [redacted] is in use",
		`model "coding-cheap" is not allowed for capability classes ["coder"] under policy version 3`: `model "coding-cheap" is not allowed for capability classes ["coder"] under policy version 3`,
	} {
		if got := s.scrub(in); got != want {
			t.Errorf("scrub(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every audit row stored is also one log record: the row's columns as
// attributes, every string scrubbed, nil and empty columns left out. The
// store keeps the row as it was; a row the store refused is not emitted.
func TestAuditRowsGoToOTelScrubbed(t *testing.T) {
	rig := newTelemetryRig(t)
	store := newMemStore()
	audited := rig.tel.Audited(store)
	three, spend, hold, limit := int64(3), 0.25, 0.5, 2250
	rows := []AuditRow{
		{TaskID: "t1", Decision: "error_tool", Tool: policy.ToolReplicate, PolicyVersion: &three, SpendEUR: &spend, HoldEUR: &hold,
			Reason: `provider answered 401: {"detail":"Invalid token r8_0123456789abcdefghijABCDEFGHIJ"}`},
		{TaskID: "t1", Decision: "allowed", ModelAlias: "sk-sneaky-alias-0123", PolicyVersion: &three, SpendEUR: &spend,
			OutputLimit: &limit, HoldEUR: &hold, OverHold: "COST OVER HOLD: model x cost 0.6 EUR against a hold of 0.5 EUR; booked in full"},
		{TaskID: "t1", Decision: "refused_budget", Reason: "task headroom 0.000000 EUR", ModelAlias: "coding-cheap", PolicyVersion: &three},
		// a task id as long as a key stays whole: it is the join key
		{TaskID: "t-0123456789abcdef0123456789abcdef01", Decision: "escalated", Tool: policy.ToolEscalate},
	}
	for _, row := range rows {
		if err := audited.InsertAudit(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
	if got := store.auditRows(); !reflect.DeepEqual(got, rows) {
		t.Fatalf("stored rows %+v, want them unchanged", got)
	}
	want := []map[string]string{
		{"body": "audit error_tool", "task.id": "t1", "audit.decision": "error_tool", "audit.tool": policy.ToolReplicate,
			"audit.reason": "provider answered 401: [body dropped]", "audit.policy_version": "3", "audit.spend_eur": "0.25", "audit.hold_eur": "0.5"},
		{"body": "audit allowed", "task.id": "t1", "audit.decision": "allowed", "audit.model_alias": "[redacted]",
			"audit.policy_version": "3", "audit.spend_eur": "0.25", "audit.output_limit": "2250", "audit.hold_eur": "0.5",
			"audit.over_hold": "COST OVER HOLD: model x cost 0.6 EUR against a hold of 0.5 EUR; booked in full"},
		{"body": "audit refused_budget", "task.id": "t1", "audit.decision": "refused_budget", "audit.reason": "task headroom 0.000000 EUR",
			"audit.model_alias": "coding-cheap", "audit.policy_version": "3"},
		{"body": "audit escalated", "task.id": "t-0123456789abcdef0123456789abcdef01", "audit.decision": "escalated", "audit.tool": policy.ToolEscalate},
	}
	got := rig.logs.attributes()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("log records\n%v\nwant\n%v", got, want)
	}
	for _, rec := range got {
		for k, v := range rec {
			if strings.Contains(v, "r8_") || strings.Contains(v, "sk-") || strings.Contains(v, "Invalid token") {
				t.Fatalf("attribute %s = %q carries a key or a body", k, v)
			}
		}
	}
	if m := rig.collect(t)["gateway.refusals"]; !reflect.DeepEqual(m, map[string]float64{"reason=refused_budget": 1}) {
		t.Fatalf("gateway.refusals = %v", m)
	}

	failing := rig.tel.Audited(failingAudit{memStore: newMemStore()})
	if err := failing.InsertAudit(context.Background(), rows[0]); err == nil {
		t.Fatal("the store's error was swallowed")
	}
	if n := len(rig.logs.attributes()); n != 4 {
		t.Fatalf("%d log records after a refused insert, want 4", n)
	}
}

type failingAudit struct{ *memStore }

func (failingAudit) InsertAudit(context.Context, AuditRow) error { return errors.New("database down") }

// An admitted model call: the server span carries the task, its child the
// upstream call with task, project, kind, alias, status, hold, cost and the
// over-hold flag; the metrics count the call, its time and the spend.
func TestGatewaySpansAndMetricsOfAModelCall(t *testing.T) {
	rig := newTelemetryRig(t)
	stub := newStubUpstream(t)
	store := newMemStore(Task{Classes: coder, ID: "t1", BudgetEUR: 5, Status: "running", Repository: "sf/demo"})
	store.setPolicy(testPolicy(3, 1))
	s := NewServer(ServerOptions{Auth: fakeAuth{id: Identity{TaskID: "t1", SAUID: "u"}}, Store: rig.tel.Audited(store),
		Minter: &fakeMinter{}, LiteLLMURL: stub.URL, Prices: testPrices, Telemetry: rig.tel})
	stub.setResp(stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-response-cost": "0.0125"}})
	if rr := doChat(t, s.Routes("test"), chatBody("coding-cheap")); rr.Code != http.StatusOK {
		t.Fatalf("call = %d %s", rr.Code, rr.Body.String())
	}
	stub.setResp(stubResp{status: http.StatusInternalServerError, body: `{"error":"boom"}`})
	if rr := doChat(t, s.Routes("test"), chatBody("coding-cheap")); rr.Code != http.StatusInternalServerError {
		t.Fatalf("failing call = %d", rr.Code)
	}

	ended := rig.spans.Ended()
	if len(ended) != 4 {
		t.Fatalf("%d spans, want 2 server spans and 2 upstream calls", len(ended))
	}
	for i, want := range []map[string]string{
		{"task.id": "t1", "project": "sf/demo", "kind": "model", "name": "coding-cheap", "upstream.status": "200",
			"hold_eur": "0.131204", "cost_eur": "0.0125", "over_hold": "false"},
		{"task.id": "t1", "project": "sf/demo", "kind": "model", "name": "coding-cheap", "upstream.status": "500",
			"hold_eur": "0.131204", "cost_eur": "0", "over_hold": "false"},
	} {
		call, server := ended[2*i], ended[2*i+1]
		if call.Name() != "model coding-cheap" || server.Name() != "POST /v1/chat/completions" {
			t.Fatalf("spans %q, %q", call.Name(), server.Name())
		}
		if call.Parent().SpanID() != server.SpanContext().SpanID() {
			t.Fatal("the upstream call is not the request span's child")
		}
		if spanAttrs(server)["task.id"] != "t1" {
			t.Fatalf("server span attributes %v", spanAttrs(server))
		}
		if got := spanAttrs(call); !reflect.DeepEqual(got, want) {
			t.Fatalf("call %d attributes\n%v\nwant\n%v", i, got, want)
		}
	}
	if ended[2].Status().Code != codes.Error {
		t.Fatalf("the 500's span status %v", ended[2].Status())
	}
	m := rig.collect(t)
	if got := m["gateway.calls"]; !reflect.DeepEqual(got, map[string]float64{
		"kind=model,name=coding-cheap,status=200": 1, "kind=model,name=coding-cheap,status=500": 1}) {
		t.Fatalf("gateway.calls = %v", got)
	}
	if got := m["gateway.call.duration"]; !reflect.DeepEqual(got, map[string]float64{"kind=model,name=coding-cheap": 2}) {
		t.Fatalf("gateway.call.duration samples = %v", got)
	}
	if got := m["gateway.spend.eur"]; !reflect.DeepEqual(got, map[string]float64{"project=sf/demo": 0.0125}) {
		t.Fatalf("gateway.spend.eur = %v", got)
	}
	if spent, _ := store.spentOf("t1"); spent != 0.0125 {
		t.Fatalf("ledger spent %v, want the metric's 0.0125", spent)
	}
	logs := rig.logs.attributes()
	if len(logs) != 2 || logs[0]["audit.decision"] != "allowed" || logs[1]["audit.decision"] != "error_upstream" || logs[1]["task.id"] != "t1" {
		t.Fatalf("audit log records %v", logs)
	}
}

// A paid tool call: its span under the MCP request, the provider's status,
// the hold and the booked cost; a refused provider call books nothing.
func TestGatewaySpansAndMetricsOfAToolCall(t *testing.T) {
	rig := newTelemetryRig(t)
	task := Task{ID: "42", BudgetEUR: 1, Status: "running", Classes: []string{"coder"}, Repository: "sf/demo"}
	store := newMemStore(task)
	store.setPolicy(toolPolicy(policy.ToolCap{MaxRequests: 10, MaxEUR: 0.2}))
	providers := newFakeProviders(t)
	s := NewServer(ServerOptions{
		Auth: fakeAuth{id: Identity{TaskID: task.ID, SAUID: "u"}}, Store: rig.tel.Audited(store), Minter: &fakeMinter{}, Prices: testPrices,
		Firecrawl: &Firecrawl{BaseURL: providers.URL, Key: "fc-key", HTTP: providers.Client()},
		Telemetry: rig.tel,
	})
	srv := httptest.NewServer(s.Routes("test"))
	t.Cleanup(srv.Close)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	scrape := func() {
		if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: policy.ToolWebScrape,
			Arguments: map[string]any{"url": "https://example.com"}}); err != nil {
			t.Fatal(err)
		}
	}
	scrape()
	providers.mu.Lock()
	providers.status["POST /v2/scrape"] = http.StatusPaymentRequired
	providers.mu.Unlock()
	scrape()

	var calls []sdktrace.ReadOnlySpan
	servers := map[[8]byte]bool{}
	for _, s := range rig.spans.Ended() {
		if s.Name() == "tool web_scrape" {
			calls = append(calls, s)
		}
		if s.Name() == "POST /mcp" && spanAttrs(s)["task.id"] == "42" {
			servers[s.SpanContext().SpanID()] = true
		}
	}
	if len(calls) != 2 {
		t.Fatalf("%d tool spans, want 2", len(calls))
	}
	for i, want := range []map[string]string{
		{"task.id": "42", "project": "sf/demo", "kind": "tool", "name": "web_scrape", "upstream.status": "ok",
			"hold_eur": "0.005", "cost_eur": "0.005", "over_hold": "false"},
		{"task.id": "42", "project": "sf/demo", "kind": "tool", "name": "web_scrape", "upstream.status": "402",
			"hold_eur": "0.005", "cost_eur": "0", "over_hold": "false"},
	} {
		if !servers[calls[i].Parent().SpanID()] {
			t.Fatalf("tool span %d is not under a task's MCP request span", i)
		}
		if got := spanAttrs(calls[i]); !reflect.DeepEqual(got, want) {
			t.Fatalf("tool span %d\n%v\nwant\n%v", i, got, want)
		}
	}
	m := rig.collect(t)
	if got := m["gateway.calls"]; !reflect.DeepEqual(got, map[string]float64{
		"kind=tool,name=web_scrape,status=ok": 1, "kind=tool,name=web_scrape,status=402": 1}) {
		t.Fatalf("gateway.calls = %v", got)
	}
	if got := m["gateway.spend.eur"]; !reflect.DeepEqual(got, map[string]float64{"project=sf/demo": 0.005}) {
		t.Fatalf("gateway.spend.eur = %v", got)
	}
	logs := rig.logs.attributes()
	if len(logs) != 2 || logs[1]["audit.reason"] != "provider answered 402: [body dropped]" {
		t.Fatalf("audit log records %v", logs)
	}
}
