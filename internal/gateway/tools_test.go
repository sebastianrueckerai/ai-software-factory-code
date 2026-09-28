package gateway

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// toolPolicy prices the tools: Firecrawl 0.005 EUR per credit (a scrape
// held at 1 credit, a search at 2), Replicate's CPU at 0.0001 EUR/s held for
// 90 s, fal megapixel endpoints held at 4 megapixels.
func toolPolicy(scrape policy.ToolCap) policy.Version {
	v := testPolicy(7, 20)
	v.Document.Classes["coder"] = policy.Class{Models: []string{"coding-cheap"}, Tools: map[string]policy.ToolCap{
		policy.ToolWebScrape: scrape,
		policy.ToolWebSearch: {MaxRequests: 30, MaxEUR: 0.2},
		policy.ToolReplicate: {MaxRequests: 5, MaxEUR: 0.5},
		policy.ToolFal:       {MaxRequests: 5, MaxEUR: 0.5},
	}}
	for _, tool := range policy.FreeTools {
		v.Document.Classes["coder"].Tools[tool] = policy.ToolCap{}
	}
	v.Document.Prices = policy.ToolPrices{
		Firecrawl: policy.FirecrawlPrices{EURPerCredit: 0.005, ScrapeCredits: 1, SearchCredits: 2},
		Replicate: policy.ReplicatePrices{EURPerSecond: map[string]float64{"cpu": 0.0001}, MaxSeconds: 90,
			Models: map[string]string{"replicate/hello-world": "cpu"}},
		Fal: policy.FalPrices{MaxMegapixels: 4},
	}
	return v
}

const helloVersion = "9dcd6d78e7c6560c340d916fe32e9f24aabfa331e5cce95fe31f77fb03121426"

// Provider responses recorded from the live APIs (research, 2026-09-27),
// shortened.
var providerResponses = map[string]string{
	"POST /v2/scrape":                                        `{"success":true,"data":{"markdown":"# Example Domain\n\nThis domain is for use in documentation examples.","metadata":{"title":"Example Domain","sourceURL":"https://example.com","url":"https://example.com/","statusCode":200,"contentType":"text/html","proxyUsed":"basic","creditsUsed":1}}}`,
	"POST /v2/search":                                        `{"success":true,"data":{"web":[{"url":"https://www.iana.org/help/example-domains","title":"Example Domains","description":"# Example Domains","position":1}]},"creditsUsed":2,"id":"01a0e545"}`,
	"GET /v1/models/replicate/hello-world":                   `{"name":"hello-world","owner":"replicate","is_official":false,"latest_version":{"id":"` + helloVersion + `","cog_version":"0.13.7"}}`,
	"POST /v1/predictions":                                   `{"id":"y7rgvntm9xrmw0d0wkdb1dkeww","model":"replicate/hello-world","version":"` + helloVersion + `","input":{"text":"Stage7"},"output":"hello Stage7","error":null,"status":"succeeded","metrics":{"predict_time":0.5,"total_time":19.49}}`,
	"GET /v1/models/pricing?endpoint_id=fal-ai/any-llm":      `{"prices":[{"endpoint_id":"fal-ai/any-llm","unit_price":0.001,"unit":"requests","currency":"USD"}],"next_cursor":null,"has_more":false}`,
	"GET /v1/models/pricing?endpoint_id=fal-ai/flux/schnell": `{"prices":[{"endpoint_id":"fal-ai/flux/schnell","unit_price":0.003,"unit":"megapixels","currency":"USD"}],"next_cursor":null,"has_more":false}`,
	"GET /v1/models/pricing?endpoint_id=fal-ai/fast-sdxl":    `{"prices":[{"endpoint_id":"fal-ai/fast-sdxl","unit_price":0.00125,"unit":"compute seconds","currency":"USD"}],"next_cursor":null,"has_more":false}`,
	"POST /fal-ai/any-llm":                                   `{"output":"pong","reasoning":null,"partial":false,"error":null}`,
	"POST /fal-ai/flux/schnell":                              `{"images":[{"url":"https://v3.fal.media/a.jpg","width":1024,"height":768},{"url":"https://v3.fal.media/b.jpg","width":1024,"height":768}],"seed":1}`,
}

// fakeProviders answers every provider API from providerResponses (or a
// per-test override in bodies), or with a status set per request line, or
// with a 500 for the first fails[key] requests.
type fakeProviders struct {
	*httptest.Server
	mu      sync.Mutex
	status  map[string]int
	fails   map[string]int
	bodies  map[string]string
	calls   []string
	auths   map[string]string
	sent    map[string]string
	headers map[string]http.Header
}

func newFakeProviders(t *testing.T) *fakeProviders {
	f := &fakeProviders{status: map[string]int{}, fails: map[string]int{}, bodies: map[string]string{},
		auths: map[string]string{}, sent: map[string]string{}, headers: map[string]http.Header{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		if q, _ := url.QueryUnescape(r.URL.RawQuery); q != "" {
			key += "?" + q
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, key)
		f.auths[key] = r.Header.Get("Authorization")
		f.sent[key] = string(b)
		f.headers[key] = r.Header.Clone()
		status := f.status[key]
		if f.fails[key] > 0 {
			f.fails[key]--
			status = http.StatusInternalServerError
		}
		body, ok := f.bodies[key]
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			io.WriteString(w, `{"error":"stub failure"}`)
			return
		}
		if !ok {
			body, ok = providerResponses[key]
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeProviders) called(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == key {
			n++
		}
	}
	return n
}

type fakeEscalator struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeEscalator) Escalate(_ context.Context, taskID, question, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, taskID+"|"+question+"|"+reason)
	return f.err
}

type toolEnv struct {
	store     *memStore
	providers *fakeProviders
	escalator *fakeEscalator
	session   *mcp.ClientSession
}

func newToolEnv(t *testing.T, task Task, pv policy.Version) *toolEnv {
	t.Helper()
	store := newMemStore(task)
	store.setPolicy(pv)
	providers := newFakeProviders(t)
	esc := &fakeEscalator{}
	s := NewServer(ServerOptions{
		Auth: fakeAuth{id: Identity{TaskID: task.ID, SAUID: "u"}}, Store: store, Minter: &fakeMinter{}, Prices: testPrices,
		Firecrawl: &Firecrawl{BaseURL: providers.URL, Key: "fc-key", HTTP: providers.Client()},
		Replicate: &Replicate{BaseURL: providers.URL, Key: "r8-key", HTTP: providers.Client(), CancelBackoff: time.Millisecond},
		Fal:       &Fal{RunURL: providers.URL, APIURL: providers.URL, Key: "fal-key", HTTP: providers.Client()},
		Escalator: esc,
	})
	srv := httptest.NewServer(s.Routes("test"))
	t.Cleanup(srv.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return &toolEnv{store: store, providers: providers, escalator: esc, session: session}
}

func (e *toolEnv) call(t *testing.T, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := e.session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return text, res.IsError
}

// lastAudit is the last tool audit row.
func (e *toolEnv) lastAudit(t *testing.T) AuditRow {
	t.Helper()
	rows := e.store.auditRows()
	if len(rows) == 0 {
		t.Fatal("no audit row")
	}
	return rows[len(rows)-1]
}

func (e *toolEnv) toolLedger() []ledgerRow {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	var out []ledgerRow
	for _, l := range e.store.ledger {
		if l.kind == policy.KindTool {
			out = append(out, l)
		}
	}
	return out
}

func running(classes ...string) Task {
	return Task{ID: "42", BudgetEUR: 1, Status: "running", Classes: classes}
}

func near6(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestMCPListsTheTools(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	res, err := e.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	want := "escalate_to_human,fal_run,replicate_predict,web_scrape,web_search"
	if strings.Join(names, ",") != want {
		t.Fatalf("tools = %v, want %s", names, want)
	}
}

func TestMCPRefusesWithoutIdentity(t *testing.T) {
	store := newMemStore(running("coder"))
	s := NewServer(ServerOptions{Auth: fakeAuth{err: errors.New("no token")}, Store: store, Minter: &fakeMinter{}, Prices: testPrices})
	srv := httptest.NewServer(s.Routes("test"))
	defer srv.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	if _, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", MaxRetries: -1}, nil); err == nil {
		t.Fatal("connected without a task identity")
	}
	if rows := store.auditRows(); len(rows) == 0 || rows[0].Decision != "refused_auth" {
		t.Fatalf("audit = %+v, want refused_auth", rows)
	}
}

// A scrape: held at 1 credit (0.005 EUR), booked at the creditsUsed its
// response reports, audited under the policy version.
func TestWebScrapeBooksCredits(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	text, isErr := e.call(t, policy.ToolWebScrape, map[string]any{"url": "https://example.com"})
	if isErr || !strings.Contains(text, "Title: Example Domain") || !strings.Contains(text, "# Example Domain") {
		t.Fatalf("scrape = %q (error %v)", text, isErr)
	}
	if got := e.providers.auths["POST /v2/scrape"]; got != "Bearer fc-key" {
		t.Fatalf("firecrawl auth = %q", got)
	}
	if l := e.toolLedger(); len(l) != 1 || l[0].ref != policy.ToolWebScrape || !near6(l[0].eur, 0.005) {
		t.Fatalf("ledger = %+v, want one web_scrape row of 0.005", l)
	}
	a := e.lastAudit(t)
	if a.Decision != "allowed" || a.Tool != policy.ToolWebScrape || *a.PolicyVersion != 7 || !near6(*a.HoldEUR, 0.005) || !near6(*a.SpendEUR, 0.005) {
		t.Fatalf("audit = %+v", a)
	}
	if e.store.openHolds() != 0 {
		t.Fatal("hold left open")
	}
}

func TestWebSearchBooksCredits(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	text, isErr := e.call(t, policy.ToolWebSearch, map[string]any{"query": "example domain iana", "limit": 3})
	if isErr || !strings.Contains(text, "iana.org/help/example-domains") {
		t.Fatalf("search = %q (error %v)", text, isErr)
	}
	if l := e.toolLedger(); len(l) != 1 || !near6(l[0].eur, 0.01) {
		t.Fatalf("ledger = %+v, want 2 credits = 0.01", l)
	}
	if _, isErr := e.call(t, policy.ToolWebSearch, map[string]any{"query": "x", "limit": 11}); !isErr {
		t.Fatal("limit 11 accepted")
	}
}

// Replicate: held at 90 s of CPU (0.009), booked at predict_time 0.5 s x
// 0.0001 = 0.00005.
func TestReplicateBooksPredictTime(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	text, isErr := e.call(t, policy.ToolReplicate, map[string]any{"model": "replicate/hello-world", "input": map[string]any{"text": "Stage7"}})
	if isErr || text != `"hello Stage7"` {
		t.Fatalf("predict = %q (error %v)", text, isErr)
	}
	if l := e.toolLedger(); len(l) != 1 || !near6(l[0].eur, 0.00005) {
		t.Fatalf("ledger = %+v, want 0.00005", l)
	}
	if a := e.lastAudit(t); !near6(*a.HoldEUR, 0.009) {
		t.Fatalf("hold = %v, want 0.009", *a.HoldEUR)
	}
}

// A model the policy names no hardware for is refused before any call.
func TestReplicateUnknownHardwareRefusedBeforeTheCall(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	text, isErr := e.call(t, policy.ToolReplicate, map[string]any{"model": "stability-ai/sdxl", "input": map[string]any{}})
	if !isErr || !strings.Contains(text, "refused by the factory policy (refused_price)") || !strings.Contains(text, "no hardware in the policy") {
		t.Fatalf("result = %q", text)
	}
	if n := len(e.providers.calls); n != 0 {
		t.Fatalf("provider called %d times: %v", n, e.providers.calls)
	}
	if e.lastAudit(t).Decision != "refused_price" || len(e.toolLedger()) != 0 {
		t.Fatal("refusal not audited, or booked")
	}
}

// fal: a flat price per request, and a price per megapixel rounded up per
// image (two 1024x768 images = 2 MP x 0.003 = 0.006, held at 4 MP = 0.012).
func TestFalBooksPricedUnits(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	if text, isErr := e.call(t, policy.ToolFal, map[string]any{"endpoint": "fal-ai/any-llm", "input": map[string]any{"prompt": "ping"}}); isErr || !strings.Contains(text, "pong") {
		t.Fatalf("any-llm = %q (error %v)", text, isErr)
	}
	if got := e.providers.auths["POST /fal-ai/any-llm"]; got != "Key fal-key" {
		t.Fatalf("fal auth = %q", got)
	}
	if _, isErr := e.call(t, policy.ToolFal, map[string]any{"endpoint": "fal-ai/flux/schnell", "input": map[string]any{"prompt": "a cat"}}); isErr {
		t.Fatal("flux refused")
	}
	l := e.toolLedger()
	if len(l) != 2 || !near6(l[0].eur, 0.001) || !near6(l[1].eur, 0.006) {
		t.Fatalf("ledger = %+v, want 0.001 and 0.006", l)
	}
	if a := e.lastAudit(t); !near6(*a.HoldEUR, 0.012) {
		t.Fatalf("flux hold = %v, want 0.012", *a.HoldEUR)
	}
	// the price is cached: one pricing call per endpoint
	e.call(t, policy.ToolFal, map[string]any{"endpoint": "fal-ai/any-llm", "input": map[string]any{"prompt": "ping"}})
	if n := e.providers.called("GET /v1/models/pricing?endpoint_id=fal-ai/any-llm"); n != 1 {
		t.Fatalf("pricing fetched %d times, want 1", n)
	}
}

func TestFalUnboundedUnitRefusedBeforeTheCall(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	text, isErr := e.call(t, policy.ToolFal, map[string]any{"endpoint": "fal-ai/fast-sdxl", "input": map[string]any{}})
	if !isErr || !strings.Contains(text, `per "compute seconds"`) {
		t.Fatalf("result = %q", text)
	}
	if e.providers.called("POST /fal-ai/fast-sdxl") != 0 {
		t.Fatal("endpoint called")
	}
	// no price at all is refused alike
	if text, isErr := e.call(t, policy.ToolFal, map[string]any{"endpoint": "fal-ai/unknown", "input": map[string]any{}}); !isErr || !strings.Contains(text, "refused_price") {
		t.Fatalf("unpriced = %q", text)
	}
}

func TestToolRefusedByClass(t *testing.T) {
	e := newToolEnv(t, running("odd"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	text, isErr := e.call(t, policy.ToolWebScrape, map[string]any{"url": "https://example.com"})
	if !isErr || !strings.Contains(text, "refused_capability") || !strings.Contains(text, "do not retry") {
		t.Fatalf("result = %q", text)
	}
	if a := e.lastAudit(t); a.Decision != "refused_capability" || a.Tool != policy.ToolWebScrape || *a.PolicyVersion != 7 {
		t.Fatalf("audit = %+v", a)
	}
	if len(e.providers.calls) != 0 {
		t.Fatal("provider called")
	}
}

func TestToolRefusedByRequestCount(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 1, MaxEUR: 0.2}))
	if _, isErr := e.call(t, policy.ToolWebScrape, map[string]any{"url": "https://example.com"}); isErr {
		t.Fatal("first call refused")
	}
	text, isErr := e.call(t, policy.ToolWebScrape, map[string]any{"url": "https://example.com"})
	if !isErr || !strings.Contains(text, "refused_tool_requests") || !strings.Contains(text, "1 of the task's 1 requests used") {
		t.Fatalf("second call = %q", text)
	}
	if n := e.providers.called("POST /v2/scrape"); n != 1 {
		t.Fatalf("firecrawl called %d times, want 1", n)
	}
}

// 0.012 EUR on the tool: two calls use 0.01, a third's 0.005 does not fit.
func TestToolRefusedByEuros(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 10, MaxEUR: 0.012}))
	for range 2 {
		if _, isErr := e.call(t, policy.ToolWebScrape, map[string]any{"url": "https://example.com"}); isErr {
			t.Fatal("call refused")
		}
	}
	if text, isErr := e.call(t, policy.ToolWebScrape, map[string]any{"url": "https://example.com"}); !isErr || !strings.Contains(text, "refused_tool_eur") {
		t.Fatalf("third call = %q", text)
	}
}

func TestToolRefusedByDayAndTask(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 10, MaxEUR: 0.2}))
	e.store.mu.Lock()
	e.store.day = 19.998 // 0.002 left today, the scrape's worst case is 0.005
	e.store.mu.Unlock()
	if text, isErr := e.call(t, policy.ToolWebScrape, map[string]any{"url": "https://example.com"}); !isErr || !strings.Contains(text, "refused_day_cap") {
		t.Fatalf("under a full day = %q", text)
	}
	e2 := newToolEnv(t, Task{ID: "42", BudgetEUR: 0.004, Status: "running", Classes: []string{"coder"}}, toolPolicy(policy.ToolCap{MaxRequests: 10, MaxEUR: 0.2}))
	if text, isErr := e2.call(t, policy.ToolWebScrape, map[string]any{"url": "https://example.com"}); !isErr || !strings.Contains(text, "refused_budget") {
		t.Fatalf("over the task budget = %q", text)
	}
	if len(e.providers.calls)+len(e2.providers.calls) != 0 {
		t.Fatal("provider called")
	}
}

func TestToolRefusedWhenTaskNotRunning(t *testing.T) {
	e := newToolEnv(t, Task{ID: "42", BudgetEUR: 1, Status: "waiting-human", Classes: []string{"coder"}}, toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	if text, isErr := e.call(t, policy.ToolWebScrape, map[string]any{"url": "https://example.com"}); !isErr || !strings.Contains(text, "refused_task_status") {
		t.Fatalf("result = %q", text)
	}
}

// A failed provider call releases its hold: a 5xx may have worked and books
// the hold, a 4xx was refused before any work and books 0 (a row still:
// it was a request).
func TestToolFailureReleasesTheHold(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 10, MaxEUR: 0.2}))
	e.providers.status["POST /v2/scrape"] = http.StatusBadGateway
	if text, isErr := e.call(t, policy.ToolWebScrape, map[string]any{"url": "https://example.com"}); !isErr || !strings.Contains(text, "web_scrape failed") {
		t.Fatalf("5xx = %q", text)
	}
	e.providers.status["POST /v2/scrape"] = http.StatusPaymentRequired
	e.call(t, policy.ToolWebScrape, map[string]any{"url": "https://example.com"})
	l := e.toolLedger()
	if len(l) != 2 || !near6(l[0].eur, 0.005) || l[1].eur != 0 {
		t.Fatalf("ledger = %+v, want the hold 0.005 then 0", l)
	}
	if e.store.openHolds() != 0 {
		t.Fatal("hold left open")
	}
	if a := e.lastAudit(t); a.Decision != "error_tool" {
		t.Fatalf("audit = %+v", a)
	}
}

func TestToolRefusesBadArguments(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 10, MaxEUR: 0.2}))
	for tool, args := range map[string]map[string]any{
		policy.ToolWebScrape: {"url": "file:///etc/passwd"},
		policy.ToolReplicate: {"model": "../x", "input": map[string]any{}},
		policy.ToolFal:       {"endpoint": "fal-ai/../../v1/models", "input": map[string]any{}},
	} {
		if text, isErr := e.call(t, tool, args); !isErr || !strings.Contains(text, "refused_arguments") {
			t.Errorf("%s = %q", tool, text)
		}
	}
	if len(e.providers.calls) != 0 {
		t.Fatalf("provider called: %v", e.providers.calls)
	}
}

func TestEscalateToHuman(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	text, isErr := e.call(t, policy.ToolEscalate, map[string]any{"question": "Which license?", "reason": "the issue names none"})
	if isErr || !strings.HasPrefix(text, "escalated; this session will end") {
		t.Fatalf("result = %q", text)
	}
	if len(e.escalator.calls) != 1 || e.escalator.calls[0] != "42|Which license?|the issue names none" {
		t.Fatalf("escalations = %v", e.escalator.calls)
	}
	if a := e.lastAudit(t); a.Decision != "escalated" || a.Tool != policy.ToolEscalate || len(e.toolLedger()) != 0 {
		t.Fatalf("audit = %+v", a)
	}

	e.escalator.err = errors.New("factoryd answered 409")
	if text, isErr := e.call(t, policy.ToolEscalate, map[string]any{"question": "q", "reason": "r"}); !isErr || !strings.Contains(text, "did not reach factoryd") {
		t.Fatalf("failed hand-over = %q", text)
	}

	other := newToolEnv(t, running("odd"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	if text, isErr := other.call(t, policy.ToolEscalate, map[string]any{"question": "q", "reason": "r"}); !isErr || !strings.Contains(text, "refused_capability") {
		t.Fatalf("non-coder = %q", text)
	}
	if len(other.escalator.calls) != 0 {
		t.Fatal("non-coder escalated")
	}
	if text, isErr := e.call(t, policy.ToolEscalate, map[string]any{"question": " ", "reason": "r"}); !isErr || !strings.Contains(text, "refused_arguments") {
		t.Fatalf("empty question = %q", text)
	}
}

func TestFactorydEscalateClient(t *testing.T) {
	var auth, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		auth, body = r.Header.Get("Authorization"), string(b)
		if r.URL.Path != "/internal/escalations" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	f := &Factoryd{BaseURL: srv.URL, Token: "shared", HTTP: srv.Client()}
	if err := f.Escalate(context.Background(), "t-1", "q?", "why"); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer shared" || body != `{"question":"q?","reason":"why","task_id":"t-1"}` {
		t.Fatalf("auth %q body %s", auth, body)
	}
}

// A megapixel endpoint's input is bounded before the call: num_images x
// ceil(megapixels per image, each side rounded up to a multiple of 64) must
// fit max_megapixels (4). 4 x 2048x2048 = 4 x ceil(4.19) = 20 MP; 3 x
// square_hd = 3 x ceil(1.05) = 6 MP; 4 x 1000x1000 is bounded as 4 x
// ceil(1024x1024/1e6) = 8 MP; a key outside the allowlist (a source image,
// a count alias, another size name) is refused.
func TestFalMegapixelInputBounded(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 20, MaxEUR: 0.5}))
	for name, input := range map[string]map[string]any{
		"20 MP":          {"prompt": "x", "num_images": 4, "image_size": map[string]any{"width": 2048, "height": 2048}},
		"6 MP":           {"prompt": "x", "num_images": 3, "image_size": "square_hd"},
		"aspect ratio":   {"prompt": "x", "aspect_ratio": "16:9"},
		"width":          {"prompt": "x", "width": 1024},
		"unknown preset": {"prompt": "x", "image_size": "huge"},
		"fractional":     {"prompt": "x", "num_images": 1.5},
		"zero images":    {"prompt": "x", "num_images": 0},
		"bad size":       {"prompt": "x", "image_size": map[string]any{"width": 1024}},
		"8 MP rounded":   {"prompt": "x", "num_images": 4, "image_size": map[string]any{"width": 1000, "height": 1000}},
		"image_url":      {"prompt": "x", "image_url": "https://example.com/a.png"},
		"num_outputs":    {"prompt": "x", "num_outputs": 4},
		"target_res":     {"prompt": "x", "target_resolution": "4k"},
	} {
		if text, isErr := e.call(t, policy.ToolFal, map[string]any{"endpoint": "fal-ai/flux/schnell", "input": input}); !isErr || !strings.Contains(text, "refused_arguments") {
			t.Errorf("%s = %q, want refused_arguments", name, text)
		} else if name == "8 MP rounded" && !strings.Contains(text, "4 images of 1000x1000 are 8 megapixels") {
			t.Errorf("%s names the rounded sides: %q", name, text)
		}
	}
	if n := e.providers.called("POST /fal-ai/flux/schnell"); n != 0 || len(e.toolLedger()) != 0 {
		t.Fatalf("endpoint called %d times, ledger %+v", n, e.toolLedger())
	}
	// 2 x square_hd = 4 MP fits, with every allowlisted key set
	if text, isErr := e.call(t, policy.ToolFal, map[string]any{"endpoint": "fal-ai/flux/schnell", "input": map[string]any{
		"prompt": "x", "negative_prompt": "y", "seed": 7, "num_images": 2, "image_size": "square_hd",
		"num_inference_steps": 4, "guidance_scale": 3.5, "enable_safety_checker": true, "output_format": "png"}}); isErr {
		t.Fatalf("allowlisted input refused: %s", text)
	}
	// unset sizes are sent explicitly: one image at landscape_4_3 (1 MP)
	if _, isErr := e.call(t, policy.ToolFal, map[string]any{"endpoint": "fal-ai/flux/schnell", "input": map[string]any{"prompt": "a cat"}}); isErr {
		t.Fatal("defaults refused")
	}
	if got := e.providers.sent["POST /fal-ai/flux/schnell"]; got != `{"image_size":"landscape_4_3","num_images":1,"prompt":"a cat"}` {
		t.Fatalf("sent %s", got)
	}
}

// An endpoint that returns more than the bound anyway (2 x 2048x2048 = 10 MP
// x 0.025 = 0.25) books that real cost against a hold of 4 MP x 0.025 =
// 0.10, loudly audited; a fal cap of 0.3 then refuses the next call
// (0.25 used + 0.10 held > 0.3), where booking the hold would have left
// 0.10 + 0.10 inside it.
func TestFalOverBoundResponseBooksTheRealCost(t *testing.T) {
	pv := toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2})
	pv.Document.Classes["coder"].Tools[policy.ToolFal] = policy.ToolCap{MaxRequests: 5, MaxEUR: 0.3}
	e := newToolEnv(t, running("coder"), pv)
	e.providers.bodies["GET /v1/models/pricing?endpoint_id=fal-ai/flux/dev"] = `{"prices":[{"endpoint_id":"fal-ai/flux/dev","unit_price":0.025,"unit":"megapixels","currency":"USD"}]}`
	e.providers.bodies["POST /fal-ai/flux/dev"] = `{"images":[{"url":"https://v3.fal.media/a.jpg","width":2048,"height":2048},{"url":"https://v3.fal.media/b.jpg","width":2048,"height":2048}]}`
	if _, isErr := e.call(t, policy.ToolFal, map[string]any{"endpoint": "fal-ai/flux/dev", "input": map[string]any{"prompt": "x", "image_size": "square"}}); isErr {
		t.Fatal("call failed")
	}
	if l := e.toolLedger(); len(l) != 1 || !near6(l[0].eur, 0.25) {
		t.Fatalf("ledger = %+v, want the real 0.25", l)
	}
	a := e.lastAudit(t)
	if a.Decision != "allowed" || a.OverHold != "COST OVER HOLD: tool fal_run cost 0.250000 EUR against a hold of 0.100000 EUR; booked in full" ||
		!near6(*a.HoldEUR, 0.1) || !near6(*a.SpendEUR, 0.25) {
		t.Fatalf("audit = %+v", a)
	}
	if text, isErr := e.call(t, policy.ToolFal, map[string]any{"endpoint": "fal-ai/flux/dev", "input": map[string]any{"prompt": "x", "image_size": "square"}}); !isErr || !strings.Contains(text, "refused_tool_eur") {
		t.Fatalf("next call = %q, want refused_tool_eur (0.25 used + 0.10 > 0.3)", text)
	}
}

// Firecrawl reporting more credits than held (5 against 1) books the 5:
// 5 x 0.005 = 0.025 against a hold of 0.005.
func TestFirecrawlCreditsOverHoldBookTheRealCost(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	e.providers.bodies["POST /v2/scrape"] = `{"success":true,"data":{"markdown":"x","metadata":{"title":"T","statusCode":200,"creditsUsed":5}}}`
	if _, isErr := e.call(t, policy.ToolWebScrape, map[string]any{"url": "https://example.com"}); isErr {
		t.Fatal("scrape failed")
	}
	if l := e.toolLedger(); len(l) != 1 || !near6(l[0].eur, 0.025) {
		t.Fatalf("ledger = %+v, want 0.025", l)
	}
	if a := e.lastAudit(t); a.Decision != "allowed" || a.OverHold == "" || !near6(*a.HoldEUR, 0.005) || !near6(*a.SpendEUR, 0.025) {
		t.Fatalf("audit = %+v", a)
	}
}

// A prediction still running after the wait is cancelled, the cancel
// retried: two 500s, then it lands; predict_time 61.5 s x 0.0001 = 0.00615.
// Replicate itself cancels 5 s before max_seconds (Cancel-After 85 s), and
// the wait never outlasts that.
func TestReplicateCancelRetriedAndBounded(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	e.providers.bodies["POST /v1/predictions"] = `{"id":"p1","status":"processing","output":null,"error":null,"metrics":{}}`
	e.providers.bodies["GET /v1/predictions/p1"] = `{"id":"p1","status":"canceled","output":null,"error":null,"metrics":{"predict_time":61.5}}`
	e.providers.bodies["POST /v1/predictions/p1/cancel"] = `{"id":"p1","status":"canceled"}`
	e.providers.fails["POST /v1/predictions/p1/cancel"] = 2
	if text, isErr := e.call(t, policy.ToolReplicate, map[string]any{"model": "replicate/hello-world", "input": map[string]any{"text": "x"}}); !isErr || !strings.Contains(text, "p1 canceled") {
		t.Fatalf("result = %q", text)
	}
	if n := e.providers.called("POST /v1/predictions/p1/cancel"); n != 3 {
		t.Fatalf("cancel sent %d times, want 3", n)
	}
	if l := e.toolLedger(); len(l) != 1 || !near6(l[0].eur, 0.00615) {
		t.Fatalf("ledger = %+v, want 0.00615", l)
	}
	h := e.providers.headers["POST /v1/predictions"]
	if h.Get("Prefer") != "wait=60" || h.Get("Cancel-After") != "85s" {
		t.Fatalf("Prefer %q, Cancel-After %q", h.Get("Prefer"), h.Get("Cancel-After"))
	}

	// max_seconds 30: waited 25 s, cancelled by Replicate at 25 s, held 0.003
	pv := toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2})
	pv.Document.Prices.Replicate.MaxSeconds = 30
	e2 := newToolEnv(t, running("coder"), pv)
	e2.call(t, policy.ToolReplicate, map[string]any{"model": "replicate/hello-world", "input": map[string]any{"text": "x"}})
	h = e2.providers.headers["POST /v1/predictions"]
	if h.Get("Prefer") != "wait=25" || h.Get("Cancel-After") != "25s" || !near6(*e2.lastAudit(t).HoldEUR, 0.003) {
		t.Fatalf("Prefer %q, Cancel-After %q, audit %+v", h.Get("Prefer"), h.Get("Cancel-After"), e2.lastAudit(t))
	}
}

// A cancel that never lands books the hold (90 s x 0.0001 = 0.009) and says
// so loudly, with the prediction id.
func TestReplicateCancelFailureIsLoud(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	e.providers.bodies["POST /v1/predictions"] = `{"id":"p1","status":"processing","output":null,"error":null,"metrics":{}}`
	e.providers.fails["POST /v1/predictions/p1/cancel"] = 100
	if text, isErr := e.call(t, policy.ToolReplicate, map[string]any{"model": "replicate/hello-world", "input": map[string]any{"text": "x"}}); !isErr || !strings.Contains(text, "REPLICATE CANCEL FAILED") {
		t.Fatalf("result = %q", text)
	}
	if n := e.providers.called("POST /v1/predictions/p1/cancel"); n != 4 {
		t.Fatalf("cancel sent %d times, want 4", n)
	}
	if l := e.toolLedger(); len(l) != 1 || !near6(l[0].eur, 0.009) {
		t.Fatalf("ledger = %+v, want the hold 0.009", l)
	}
	if a := e.lastAudit(t); a.Decision != "error_tool" || !strings.Contains(a.Reason, "REPLICATE CANCEL FAILED: prediction p1") {
		t.Fatalf("audit = %+v", a)
	}
}

// The version lookup runs under the hold: it counts as a request.
func TestReplicateVersionLookupIsARequest(t *testing.T) {
	pv := toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2})
	pv.Document.Classes["coder"].Tools[policy.ToolReplicate] = policy.ToolCap{MaxRequests: 1, MaxEUR: 0.5}
	e := newToolEnv(t, running("coder"), pv)
	args := map[string]any{"model": "replicate/hello-world", "version": strings.Repeat("a", 64), "input": map[string]any{}}
	if text, isErr := e.call(t, policy.ToolReplicate, args); !isErr || !strings.Contains(text, "replicate_predict failed") {
		t.Fatalf("unknown version = %q", text)
	}
	if l := e.toolLedger(); len(l) != 1 || l[0].eur != 0 {
		t.Fatalf("ledger = %+v, want one 0 EUR row", l)
	}
	if text, isErr := e.call(t, policy.ToolReplicate, args); !isErr || !strings.Contains(text, "refused_tool_requests") {
		t.Fatalf("second lookup = %q", text)
	}
	if n := e.providers.called("GET /v1/models/replicate/hello-world/versions/" + strings.Repeat("a", 64)); n != 1 {
		t.Fatalf("version looked up %d times, want 1", n)
	}
}

// Targets Firecrawl could only fail on, or should never fetch, are refused
// before any hold.
func TestWebToolsRefuseLocalTargets(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 50, MaxEUR: 0.2}))
	for _, u := range []string{
		"http://localhost/", "http://app.localhost/", "http://127.0.0.1/", "http://10.0.0.5/x", "http://192.168.1.1/",
		"http://169.254.169.254/latest/meta-data", "http://[::1]/", "http://[fe80::1]/",
		"http://factoryd.factory.svc.cluster.local:8080/", "http://metadata.google.internal/", "http://factoryd/",
		"http://2130706433/", "http://0x7f.1/", "http://127.1/", "https://example.com/" + strings.Repeat("a", 2048),
	} {
		if text, isErr := e.call(t, policy.ToolWebScrape, map[string]any{"url": u}); !isErr || !strings.Contains(text, "refused_arguments") {
			t.Errorf("%s = %q", u, text)
		}
	}
	if text, isErr := e.call(t, policy.ToolWebSearch, map[string]any{"query": strings.Repeat("q", 1001)}); !isErr || !strings.Contains(text, "refused_arguments") {
		t.Errorf("long query = %q", text)
	}
	if len(e.providers.calls) != 0 || len(e.toolLedger()) != 0 {
		t.Fatalf("provider called: %v", e.providers.calls)
	}
}
