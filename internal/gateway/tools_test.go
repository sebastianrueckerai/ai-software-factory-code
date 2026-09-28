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

// fakeProviders answers every provider API from providerResponses, or with
// a status set per request line.
type fakeProviders struct {
	*httptest.Server
	mu     sync.Mutex
	status map[string]int
	calls  []string
	auths  map[string]string
}

func newFakeProviders(t *testing.T) *fakeProviders {
	f := &fakeProviders{status: map[string]int{}, auths: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		if q, _ := url.QueryUnescape(r.URL.RawQuery); q != "" {
			key += "?" + q
		}
		f.mu.Lock()
		f.calls = append(f.calls, key)
		f.auths[key] = r.Header.Get("Authorization")
		status := f.status[key]
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			io.WriteString(w, `{"error":"stub failure"}`)
			return
		}
		body, ok := providerResponses[key]
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
		Replicate: &Replicate{BaseURL: providers.URL, Key: "r8-key", HTTP: providers.Client()},
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
