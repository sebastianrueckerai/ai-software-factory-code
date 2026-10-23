package gateway

import (
	"sync/atomic"

	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
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
	v.Document.Classes["coder"] = policy.Class{Models: []string{"small"}, VisionModel: "vision", Tools: map[string]policy.ToolCap{
		policy.ToolWebScrape: scrape,
		policy.ToolWebSearch: {MaxRequests: 30, MaxEUR: 0.2},
		policy.ToolReplicate: {MaxRequests: 5, MaxEUR: 0.5},
		policy.ToolFal:       {MaxRequests: 5, MaxEUR: 0.5},
	}}
	for _, tool := range policy.FreeTools {
		v.Document.Classes["coder"].Tools[tool] = policy.ToolCap{}
	}
	// the task's project runs in the cluster, so deployment_health is offered
	v.Projects = map[string]policy.ProjectConfig{"code": {Repository: "sf/code", Namespaces: []string{"factory"}}}
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
	mu      sync.Mutex
	calls   []string
	choices [][]string
	asks    []Escalation
	err     error
}

func (f *fakeEscalator) Escalate(_ context.Context, taskID string, e Escalation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join([]string{taskID, e.Kind, e.Question, e.Reason, e.Recommendation, e.Tried, e.Needs}, "|"))
	f.choices = append(f.choices, e.Choices)
	f.asks = append(f.asks, e)
	return f.err
}

type fakePlanFiler struct {
	mu    sync.Mutex
	calls []string
	res   PlanFiled
	err   error
}

func (f *fakePlanFiler) FilePlan(_ context.Context, taskID, plan string) (PlanFiled, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, taskID+"|"+plan)
	return f.res, f.err
}

type fakeVerdicter struct {
	mu    sync.Mutex
	calls []string
	res   VerdictFiled
	err   error
}

func (f *fakeVerdicter) FileVerdict(_ context.Context, taskID string, v HoldoutVerdictIn) (VerdictFiled, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s|%d|%s|%s|%s|%s", taskID, v.Idx, v.Verdict, v.Reason, v.Quote, v.TestCode))
	return f.res, f.err
}

type fakeDeploymentReader struct {
	mu    sync.Mutex
	calls []string
	text  string
	err   error
}

func (f *fakeDeploymentReader) DeploymentHealth(_ context.Context, taskID, deployment, namespace string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, taskID+"|"+deployment+"|"+namespace)
	return f.text, f.err
}

type fakeImageLooker struct {
	mu    sync.Mutex
	calls []string
	text  string
	err   error
}

func (f *fakeImageLooker) LookAtImage(_ context.Context, taskID string, in AnalyzeImageIn) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, taskID+"|"+in.Path+"|"+in.CardImage+"|"+in.Ask)
	return f.text, f.err
}

type toolEnv struct {
	deployments *fakeDeploymentReader
	looker      *fakeImageLooker
	store       *memStore
	providers   *fakeProviders
	escalator   *fakeEscalator
	filer       *fakePlanFiler
	verdicter   *fakeVerdicter
	delegator   *fakeDelegator
	session     *mcp.ClientSession
}

func newToolEnv(t *testing.T, task Task, pv policy.Version) *toolEnv {
	t.Helper()
	store := newMemStore(task)
	store.setPolicy(pv)
	providers := newFakeProviders(t)
	esc := &fakeEscalator{}
	filer := &fakePlanFiler{}
	verdicter := &fakeVerdicter{}
	delegator := &fakeDelegator{}
	deployments := &fakeDeploymentReader{text: "deployment web: 2/2 ready"}
	looker := &fakeImageLooker{text: "img1.png (Screenshot.png): a login form, the button grey"}
	s := NewServer(ServerOptions{
		Auth: fakeAuth{id: Identity{TaskID: task.ID, SAUID: "u", Session: "s-1"}}, Store: store, Minter: &fakeMinter{}, Prices: testPrices,
		Firecrawl: &Firecrawl{BaseURL: providers.URL, Key: "fc-key", HTTP: providers.Client()},
		Replicate: &Replicate{BaseURL: providers.URL, Key: "r8-key", HTTP: providers.Client(), CancelBackoff: time.Millisecond},
		Fal:       &Fal{RunURL: providers.URL, APIURL: providers.URL, Key: "fal-key", HTTP: providers.Client()},
		Escalator: esc, PlanFiler: filer, HoldoutVerdicter: verdicter, Delegator: delegator,
		DeploymentReader: deployments, ImageLooker: looker,
	})
	srv := httptest.NewServer(s.Routes("test"))
	t.Cleanup(srv.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return &toolEnv{store: store, providers: providers, escalator: esc, filer: filer, verdicter: verdicter, delegator: delegator, deployments: deployments, looker: looker, session: session}
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
	return Task{ID: "42", BudgetEUR: 1, Status: "running", Classes: classes, Repository: "sf/code"}
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
	want := "analyze_image,delegate,deployment_health,escalate_to_human,fal_run,file_plan,file_verdict,manual,replicate_predict,request_info,request_tool,web_scrape,web_search"
	if strings.Join(names, ",") != want {
		t.Fatalf("tools = %v, want %s", names, want)
	}
}

// escalate_to_human offers the agent choices: an optional list of strings
// with its one line, beside the three required fields.
func TestEscalateSchemaOffersChoices(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	res, err := e.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(res.Tools, func(tl *mcp.Tool) bool { return tl.Name == policy.ToolEscalate })
	if i < 0 {
		t.Fatal("no escalate_to_human")
	}
	b, _ := json.Marshal(res.Tools[i].InputSchema)
	var schema struct {
		Required   []string
		Properties map[string]struct {
			Type        any
			Description string
			Items       struct{ Type any }
		}
	}
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatal(err)
	}
	choices := schema.Properties["choices"]
	if fmt.Sprint(choices.Type) != "array" || fmt.Sprint(choices.Items.Type) != "string" ||
		choices.Description != "offer 2 to 5 short answers the human can pick with one click; always leave free text possible" {
		t.Fatalf("choices = %+v in %s", choices, b)
	}
	if !slices.Equal(schema.Required, []string{"question", "reason", "recommendation"}) {
		t.Fatalf("required = %q, want the three without choices", schema.Required)
	}
}

// Ruling 263, roadmap 201: when to call request_info and
// escalate_to_human is their descriptions' alone; factoryd's guides no
// longer say it.
func TestAskToolsSayWhenToCallThem(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	res, err := e.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for tool, want := range map[string]string{
		policy.ToolRequestInfo: "Ask the issue's author for information, instead of guessing, when you could not reproduce the problem and found no fault",
		policy.ToolEscalate:    "Ask a human when the task cannot be completed without a decision, access or information only a human has.",
	} {
		i := slices.IndexFunc(res.Tools, func(tl *mcp.Tool) bool { return tl.Name == tool })
		if i < 0 || !strings.HasPrefix(res.Tools[i].Description, want) {
			t.Errorf("%s: description does not open with %q", tool, want)
		}
	}
}

// tools/list shows only what the task's classes may call, from the same
// check that decides a call: a hidden tool called anyway is refused and
// audited like any tool the class lacks.
func TestMCPListsOnlyTheClassTools(t *testing.T) {
	pv := toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2})
	pv.Document.Classes["reader"] = policy.Class{Models: []string{"small"}, Tools: map[string]policy.ToolCap{
		policy.ToolWebSearch: {MaxRequests: 3, MaxEUR: 0.1}, policy.ToolRequestInfo: {}}}
	for _, tc := range []struct {
		classes []string
		want    string
	}{
		{[]string{"reader"}, "request_info,web_search"},
		{[]string{"odd"}, ""},
	} {
		e := newToolEnv(t, running(tc.classes...), pv)
		res, err := e.session.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tl := range res.Tools {
			names = append(names, tl.Name)
		}
		if strings.Join(names, ",") != tc.want {
			t.Fatalf("%v: tools = %v, want %s", tc.classes, names, tc.want)
		}
		text, isErr := e.call(t, policy.ToolWebScrape, map[string]any{"url": "https://example.com"})
		if !isErr || !strings.Contains(text, "refused_capability") {
			t.Fatalf("%v: hidden web_scrape = %q, want refused_capability", tc.classes, text)
		}
		if a := e.lastAudit(t); a.Decision != "refused_capability" || a.Tool != policy.ToolWebScrape {
			t.Fatalf("%v: audit = %+v", tc.classes, a)
		}
		if len(e.providers.calls) != 0 {
			t.Fatalf("%v: provider called", tc.classes)
		}
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

// A tool call's hold, booked onto its ledger row, carries the session of
// the call's token (roadmap 243).
func TestToolHoldCarriesTheSession(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	if text, isErr := e.call(t, policy.ToolWebScrape, map[string]any{"url": "https://example.com"}); isErr {
		t.Fatalf("scrape = %q", text)
	}
	if len(e.store.reqs) != 1 || e.store.reqs[0].Session != "s-1" {
		t.Fatalf("hold requests = %+v, want the scrape's under session s-1", e.store.reqs)
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

// The scrape's worst case is 0.005 EUR; linkshort's file caps the project
// at 0.002 a month.
func TestToolRefusedByProjectCap(t *testing.T) {
	task := running("coder")
	task.Repository = "linkshort/linkshort"
	pv := toolPolicy(policy.ToolCap{MaxRequests: 10, MaxEUR: 0.2})
	pv.Projects = map[string]policy.ProjectConfig{"linkshort": {Repository: "linkshort/linkshort", Budget: &policy.ProjectBudget{MonthEUR: 0.002}}}
	e := newToolEnv(t, task, pv)
	text, isErr := e.call(t, policy.ToolWebScrape, map[string]any{"url": "https://example.com"})
	if !isErr || !strings.Contains(text, policy.RefusedProjectCap) ||
		!strings.Contains(text, "the tool call's worst case exceeds the project's remaining month headroom (budget.month_eur in projects/linkshort.yaml)") {
		t.Fatalf("under linkshort's cap = %q", text)
	}
	if len(e.providers.calls) != 0 {
		t.Fatal("provider called")
	}
	if r := e.store.reqs[0]; r.Project != "linkshort" || r.ProjectEUR != 0.002 {
		t.Fatalf("hold request = %+v, want linkshort's cap 0.002", r)
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
	text, isErr := e.call(t, policy.ToolEscalate, map[string]any{"question": "Which license?", "reason": "the issue names none",
		"recommendation": "MIT, as the other repositories", "choices": []string{"MIT", "Apache-2.0"}})
	if isErr || !strings.HasPrefix(text, "escalated; this session will end") {
		t.Fatalf("result = %q", text)
	}
	// factoryd checks the choices where it stores them; the gateway hands them over
	if len(e.escalator.calls) != 1 || e.escalator.calls[0] != "42|escalation|Which license?|the issue names none|MIT, as the other repositories||" ||
		!slices.Equal(e.escalator.choices[0], []string{"MIT", "Apache-2.0"}) {
		t.Fatalf("escalations = %v with choices %q", e.escalator.calls, e.escalator.choices)
	}
	if a := e.lastAudit(t); a.Decision != "escalated" || a.Tool != policy.ToolEscalate || len(e.toolLedger()) != 0 {
		t.Fatalf("audit = %+v", a)
	}

	e.escalator.err = errors.New("factoryd answered 409")
	if text, isErr := e.call(t, policy.ToolEscalate, map[string]any{"question": "q", "reason": "r", "recommendation": "x"}); !isErr || !strings.Contains(text, "did not reach factoryd") {
		t.Fatalf("failed hand-over = %q", text)
	}

	other := newToolEnv(t, running("odd"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	if text, isErr := other.call(t, policy.ToolEscalate, map[string]any{"question": "q", "reason": "r", "recommendation": "x"}); !isErr || !strings.Contains(text, "refused_capability") {
		t.Fatalf("non-coder = %q", text)
	}
	if len(other.escalator.calls) != 0 {
		t.Fatal("non-coder escalated")
	}
	if text, isErr := e.call(t, policy.ToolEscalate, map[string]any{"question": " ", "reason": "r", "recommendation": "x"}); !isErr || !strings.Contains(text, "refused_arguments") {
		t.Fatalf("empty question = %q", text)
	}
}

// A choice list of more than 5 entries or over 1 KiB is dropped before
// factoryd, whose body cap would refuse the question with it: the question
// is delivered without choices, never refused. 1024 bytes pass.
func TestEscalateDropsAnOversizedChoiceList(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	many := make([]string, 300)
	for i := range many {
		many[i] = strings.Repeat("x", 230)
	}
	for _, tc := range []struct {
		name    string
		choices []string
		want    []string
	}{
		{"300 choices", many, nil},
		{"1025 bytes", []string{strings.Repeat("a", 600), strings.Repeat("b", 425)}, nil},
		{"1024 bytes", []string{strings.Repeat("a", 600), strings.Repeat("b", 424)}, []string{strings.Repeat("a", 600), strings.Repeat("b", 424)}},
	} {
		before := len(e.escalator.calls)
		text, isErr := e.call(t, policy.ToolEscalate, map[string]any{"question": "Which license?", "reason": "none named", "recommendation": "MIT", "choices": tc.choices})
		if isErr || !strings.HasPrefix(text, "escalated; this session will end") || len(e.escalator.calls) != before+1 ||
			e.escalator.calls[before] != "42|escalation|Which license?|none named|MIT||" || !slices.Equal(e.escalator.choices[before], tc.want) {
			t.Errorf("%s: %q, delivered %q with choices %q; want the question with %d", tc.name, text, e.escalator.calls[before:], e.escalator.choices[before:], len(tc.want))
		}
	}
}

// The recommendation is required, at most 2000 bytes (ruling 149); a
// refused call reaches no factoryd.
func TestEscalateNeedsARecommendation(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	for name, rec := range map[string]any{"missing": nil, "blank": "  ", "2001 bytes": strings.Repeat("x", 2001)} {
		args := map[string]any{"question": "q", "reason": "r"}
		if rec != nil {
			args["recommendation"] = rec
		}
		// a missing field fails the tool's input schema, a blank or long one the gateway's check
		want := "refused_arguments"
		if rec == nil {
			want = `missing properties: ["recommendation"]`
		}
		if text, isErr := e.call(t, policy.ToolEscalate, args); !isErr || !strings.Contains(text, want) || !strings.Contains(text, "recommendation") {
			t.Errorf("%s: %q, want a refusal naming the recommendation", name, text)
		}
	}
	if text, isErr := e.call(t, policy.ToolEscalate, map[string]any{"question": "q", "reason": "r", "recommendation": strings.Repeat("x", 2000)}); isErr {
		t.Fatalf("2000 bytes: %q, want accepted", text)
	}
	if len(e.escalator.calls) != 1 {
		t.Fatalf("escalations = %d, want only the valid one", len(e.escalator.calls))
	}
}

// request_info asks the issue's author (ruling 150): question, what the
// agent tried and what it needs, each required; through the class's tool
// list like every tool.
func TestRequestInfo(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	text, isErr := e.call(t, policy.ToolRequestInfo, map[string]any{"question": "Which browser?", "tried": "the steps on Go 1.26", "needs": "the browser and its version"})
	if isErr || !strings.HasPrefix(text, "asked the issue's author; this session will end") {
		t.Fatalf("result = %q", text)
	}
	if len(e.escalator.calls) != 1 || e.escalator.calls[0] != "42|info_request|Which browser?|||the steps on Go 1.26|the browser and its version" {
		t.Fatalf("calls = %v", e.escalator.calls)
	}
	if a := e.lastAudit(t); a.Decision != "info_requested" || a.Tool != policy.ToolRequestInfo {
		t.Fatalf("audit = %+v", a)
	}
	if text, isErr := e.call(t, policy.ToolRequestInfo, map[string]any{"question": "q", "tried": " ", "needs": "n"}); !isErr || !strings.Contains(text, "refused_arguments") {
		t.Fatalf("blank tried = %q", text)
	}
	v := toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2})
	delete(v.Document.Classes["coder"].Tools, policy.ToolRequestInfo)
	unlisted := newToolEnv(t, running("coder"), v)
	if text, isErr := unlisted.call(t, policy.ToolRequestInfo, map[string]any{"question": "q", "tried": "t", "needs": "n"}); !isErr || !strings.Contains(text, "refused_capability") {
		t.Fatalf("unlisted = %q", text)
	}
	if len(e.escalator.calls) != 1 || len(unlisted.escalator.calls) != 0 {
		t.Fatalf("calls = %v / %v", e.escalator.calls, unlisted.escalator.calls)
	}
}

// request_tool asks for a tool the sandbox lacks: the tool (1 to 80
// characters) and what for (1 to 600), both required; the session ends,
// and its description alone tells the agent what follows.
func TestRequestTool(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	res, err := e.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(res.Tools, func(tl *mcp.Tool) bool { return tl.Name == policy.ToolRequestTool })
	if i < 0 {
		t.Fatal("no request_tool")
	}
	if want := "Ask for a tool this sandbox lacks, such as a browser, ffmpeg or a language runtime, instead of trying to install it: the sandbox refuses installs. " +
		"Your work in progress is pushed, a card adds the tool to one of the project's images, and this card restarts on that image from its branch once the image is built. " +
		"Name the tool by its Debian package or binary name (chromium, ffmpeg, python3), and what you need it for."; res.Tools[i].Description != want {
		t.Fatalf("description = %q, want %q", res.Tools[i].Description, want)
	}
	b, _ := json.Marshal(res.Tools[i].InputSchema)
	var schema struct {
		Required   []string
		Properties map[string]struct{ Description string }
	}
	if err := json.Unmarshal(b, &schema); err != nil || !slices.Equal(schema.Required, []string{"tool", "need"}) {
		t.Fatalf("required = %q (%v), want tool and need", schema.Required, err)
	}
	if got := schema.Properties["tool"].Description; got != "the tool's Debian package or binary name, such as chromium, ffmpeg or python3" {
		t.Fatalf("tool's description = %q, want the Debian package or binary name", got)
	}

	text, isErr := e.call(t, policy.ToolRequestTool, map[string]any{"tool": " ffmpeg ", "need": " cut the demo video to 10 s "})
	if isErr || text != "asked for ffmpeg; this session will end. Your work in progress is pushed; the card restarts on an image with the tool once it is built, "+
		"or the next session is told why not. Stop working now." {
		t.Fatalf("result = %q", text)
	}
	if want := (Escalation{Kind: KindMissingTool, Tool: "ffmpeg", Need: "cut the demo video to 10 s"}); len(e.escalator.asks) != 1 || !reflect.DeepEqual(e.escalator.asks[0], want) {
		t.Fatalf("asks = %+v, want %+v", e.escalator.asks, want)
	}
	if a := e.lastAudit(t); a.Decision != "tool_requested" || a.Tool != policy.ToolRequestTool || a.Reason != "ffmpeg: cut the demo video to 10 s" {
		t.Fatalf("audit = %+v", a)
	}
	// 80 and 600 characters pass however many bytes they take
	if text, isErr := e.call(t, policy.ToolRequestTool, map[string]any{"tool": strings.Repeat("ä", 80), "need": strings.Repeat("ö", 600)}); isErr {
		t.Fatalf("80 and 600 characters = %q, want accepted", text)
	}
	for name, args := range map[string]map[string]any{
		"no tool":             {"tool": " ", "need": "n"},
		"no need":             {"tool": "ffmpeg", "need": ""},
		"long tool":           {"tool": strings.Repeat("a", 81), "need": "n"},
		"long need":           {"tool": "ffmpeg", "need": strings.Repeat("n", 601)},
		"tool on two lines":   {"tool": "ff\nmpeg", "need": "n"},
		"need with a control": {"tool": "ffmpeg", "need": "cut\u0007"},
	} {
		if text, isErr := e.call(t, policy.ToolRequestTool, args); !isErr || !strings.Contains(text, "refused_arguments") ||
			!strings.Contains(text, "tool (1 to 80 characters) and need (1 to 600 characters) are required, each on one line without control characters") {
			t.Errorf("%s = %q, want refused_arguments naming both bounds", name, text)
		}
	}
	if len(e.escalator.asks) != 2 {
		t.Fatalf("asks = %d, want the two valid ones", len(e.escalator.asks))
	}
	// factoryd refuses a report session's ask: the agent reads why, as a refusal
	e.escalator.err = &providerError{status: http.StatusUnprocessableEntity, body: "this session cannot wait for a new image: finish with what the sandbox has\n"}
	text, isErr = e.call(t, policy.ToolRequestTool, map[string]any{"tool": "ffmpeg", "need": "cut"})
	if want := "refused by the factory policy (refused_session): this session cannot wait for a new image: finish with what the sandbox has. " +
		"The tool did not run; do not retry this call."; !isErr || text != want {
		t.Fatalf("a refused ask = %q, want %q", text, want)
	}
	if a := e.lastAudit(t); a.Decision != "refused_session" || a.Reason != "this session cannot wait for a new image: finish with what the sandbox has" {
		t.Fatalf("audit = %+v", a)
	}
	e.escalator.err = nil
	v := toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2})
	delete(v.Document.Classes["coder"].Tools, policy.ToolRequestTool)
	unlisted := newToolEnv(t, running("coder"), v)
	if text, isErr := unlisted.call(t, policy.ToolRequestTool, map[string]any{"tool": "ffmpeg", "need": "n"}); !isErr || !strings.Contains(text, "refused_capability") {
		t.Fatalf("unlisted = %q", text)
	}
	// a tool card declares tools its own sandbox never gets: no request_tool
	card := running("coder")
	card.Source = policy.SourceToolRequest
	tc := newToolEnv(t, card, toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	if res, err := tc.session.ListTools(context.Background(), nil); err != nil || slices.ContainsFunc(res.Tools, func(tl *mcp.Tool) bool { return tl.Name == policy.ToolRequestTool }) {
		t.Fatalf("a tool card's tools list request_tool: %v", err)
	}
	if text, isErr := tc.call(t, policy.ToolRequestTool, map[string]any{"tool": "ffmpeg", "need": "n"}); !isErr || !strings.Contains(text, "refused_capability") ||
		!strings.Contains(text, "a tool card declares tools its own sandbox never gets") || len(tc.escalator.asks) != 0 {
		t.Fatalf("a tool card's request_tool = %q", text)
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
	if err := f.Escalate(context.Background(), "t-1", Escalation{Kind: "escalation", Question: "q?", Reason: "why", Recommendation: "do x"}); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer shared" || body != `{"task_id":"t-1","kind":"escalation","question":"q?","reason":"why","recommendation":"do x"}` {
		t.Fatalf("auth %q body %s", auth, body)
	}
}

// An escalation raised while factoryd restarts (unreachable, erring, or
// with no live session yet) is retried until the reattached session takes
// it; a refusal of the escalation itself is answered at once.
func TestFactorydEscalateClientRetriesThroughARestart(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusConflict)
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()
	f := &Factoryd{BaseURL: srv.URL, Token: "shared", HTTP: srv.Client(), RetryFor: time.Second, RetryEvery: time.Millisecond}
	if err := f.Escalate(context.Background(), "t-1", Escalation{Kind: "escalation", Question: "q?"}); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("calls = %d, want the two refusals retried, then the accept", n)
	}

	calls.Store(0)
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer refusing.Close()
	f = &Factoryd{BaseURL: refusing.URL, Token: "shared", HTTP: refusing.Client(), RetryFor: time.Second, RetryEvery: time.Millisecond}
	if err := f.Escalate(context.Background(), "t-1", Escalation{Kind: "escalation"}); err == nil {
		t.Fatal("a refused escalation returned no error")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("calls = %d, want the refusal answered at once", n)
	}

	calls.Store(0)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusConflict)
	}))
	defer down.Close()
	f = &Factoryd{BaseURL: down.URL, Token: "shared", HTTP: down.Client(), RetryFor: 250 * time.Millisecond, RetryEvery: 10 * time.Millisecond}
	if err := f.Escalate(context.Background(), "t-1", Escalation{Kind: "escalation"}); err == nil {
		t.Fatal("an escalation nobody took within the bound returned no error")
	}
	if n := calls.Load(); n < 2 {
		t.Fatalf("calls = %d, want retries until the bound", n)
	}
}

func TestFactorydFilePlanClient(t *testing.T) {
	var auth, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		auth, body = r.Header.Get("Authorization"), string(b)
		if r.URL.Path != "/internal/plans" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"revision":2,"sha256":"abc123","findings":["f"]}`)
	}))
	defer srv.Close()
	f := &Factoryd{BaseURL: srv.URL, Token: "shared", HTTP: srv.Client()}
	res, err := f.FilePlan(context.Background(), "t-1", `{"summary":"s"}`)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer shared" || body != `{"task_id":"t-1","plan":"{\"summary\":\"s\"}"}` {
		t.Fatalf("auth %q body %s", auth, body)
	}
	if res.Revision != 2 || res.SHA256 != "abc123" || !slices.Equal(res.Findings, []string{"f"}) {
		t.Fatalf("res = %+v", res)
	}
}

// file_plan is the planner's answer in the session it runs in: a plan the
// check refuses comes back as every finding, the session goes on and the
// corrected plan files in the same session; only a filed plan ends it.
func TestFilePlanTool(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	plan := `{"summary":"s","cards":[{"key":"links-api","role":"code","title":"t","brief":"b","size":"large"}]}`
	finding := "plan: card links-api: size huge: split it; a card is one package, one page or one set of endpoints"
	e.filer.res = PlanFiled{Findings: []string{finding}}
	text, isErr := e.call(t, policy.ToolFilePlan, map[string]any{"plan": plan})
	if !isErr || text != "the plan was refused:\n- "+finding+"\ncorrect the plan and file it again in this session" {
		t.Fatalf("refused plan = %q, want the finding in the session", text)
	}
	if a := e.lastAudit(t); a.Decision != "plan_refused" || a.Tool != policy.ToolFilePlan {
		t.Fatalf("audit = %+v", a)
	}
	e.filer.res = PlanFiled{Revision: 3, SHA256: "abc123"}
	text, isErr = e.call(t, policy.ToolFilePlan, map[string]any{"plan": plan})
	if isErr || text != "filed as revision 3; it is this session's answer. You may end the session." {
		t.Fatalf("filed plan = %q (%v)", text, isErr)
	}
	if a := e.lastAudit(t); a.Decision != "plan_filed" || a.Reason != "revision 3" {
		t.Fatalf("audit = %+v", a)
	}
	if calls := e.filer.calls; len(calls) != 2 || !strings.HasPrefix(calls[0], "42|") {
		t.Fatalf("filer calls = %v", calls)
	}
}

// file_plan reaches factoryd only for a running task of a class that lists
// it; every refusal names the reason and files nothing, and a hand-over
// that fails says so in the session.
func TestFilePlanRefused(t *testing.T) {
	pv := toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2})
	done := newToolEnv(t, Task{ID: "42", BudgetEUR: 1, Status: "waiting-human", Classes: []string{"coder"}}, pv)
	if text, isErr := done.call(t, policy.ToolFilePlan, map[string]any{"plan": "{}"}); !isErr ||
		!strings.Contains(text, "refused_task_status") {
		t.Fatalf("not running = %q, want refused_task_status", text)
	}
	e := newToolEnv(t, running("odd"), pv)
	if text, isErr := e.call(t, policy.ToolFilePlan, map[string]any{"plan": "{}"}); !isErr ||
		!strings.Contains(text, "refused_capability") {
		t.Fatalf("class without the tool = %q, want refused_capability", text)
	}
	e = newToolEnv(t, running("coder"), pv)
	if text, isErr := e.call(t, policy.ToolFilePlan, map[string]any{"plan": ""}); !isErr ||
		!strings.Contains(text, "refused_arguments") {
		t.Fatalf("empty plan = %q, want refused_arguments", text)
	}
	e.filer.err = errors.New("no route")
	if text, isErr := e.call(t, policy.ToolFilePlan, map[string]any{"plan": "{}"}); !isErr ||
		!strings.Contains(text, "the plan did not reach factoryd: no route") {
		t.Fatalf("filer error = %q", text)
	}
	if a := e.lastAudit(t); a.Decision != "error_plan" {
		t.Fatalf("audit = %+v", a)
	}
	if calls := e.filer.calls; len(calls) != 1 {
		t.Fatalf("filer calls = %v, want only the last one through", calls)
	}
	e.filer.err = nil
	big := strings.Repeat("x", 12<<10+1)
	if text, isErr := e.call(t, policy.ToolFilePlan, map[string]any{"plan": big}); isErr {
		t.Fatalf("plan over the old 12 KiB bound = %q, want it through", text)
	}
	if calls := e.filer.calls; len(calls) != 2 || !strings.HasSuffix(calls[1], big) {
		t.Fatalf("filer calls = %v, want the large plan through", calls)
	}
}

// file_verdict is the audit session's answer in the session it runs in: a
// verdict the check refuses comes back as every finding, the session goes
// on and the corrected verdict files in the same session; only a filed
// verdict ends it.
func TestFileVerdictTool(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	finding := "file_verdict: the verdict is one of wrong_test, real_gap, spec_question"
	e.verdicter.res = VerdictFiled{Findings: []string{finding}}
	text, isErr := e.call(t, policy.ToolFileVerdict, map[string]any{"idx": 2, "verdict": "no idea"})
	if !isErr || text != "the verdict was refused:\n- "+finding+"\ncorrect the verdict and file it again in this session" {
		t.Fatalf("refused verdict = %q, want the finding in the session", text)
	}
	if a := e.lastAudit(t); a.Decision != "verdict_refused" || a.Tool != policy.ToolFileVerdict {
		t.Fatalf("audit = %+v", a)
	}
	e.verdicter.res = VerdictFiled{Filed: true}
	text, isErr = e.call(t, policy.ToolFileVerdict, map[string]any{"idx": 2, "verdict": "wrong_test",
		"reason": "nothing drops the test", "quote": "The door opens only for a key that fits", "test_code": "package holdout"})
	if isErr || text != "filed, hold-out 2: wrong_test. It is this session's answer. You may end the session." {
		t.Fatalf("filed verdict = %q (%v)", text, isErr)
	}
	if a := e.lastAudit(t); a.Decision != "verdict_filed" || a.Reason != "hold-out 2: wrong_test" {
		t.Fatalf("audit = %+v", a)
	}
	if calls := e.verdicter.calls; len(calls) != 2 ||
		calls[1] != "42|2|wrong_test|nothing drops the test|The door opens only for a key that fits|package holdout" {
		t.Fatalf("verdicter calls = %v", calls)
	}
}

// file_verdict reaches factoryd only for a running task of a class that
// lists it, with a verdict and a rewrite within the bound; every refusal
// names the reason and files nothing, and a hand-over that fails says so
// in the session.
func TestFileVerdictRefused(t *testing.T) {
	pv := toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2})
	done := newToolEnv(t, Task{ID: "42", BudgetEUR: 1, Status: "waiting-human", Classes: []string{"coder"}}, pv)
	if text, isErr := done.call(t, policy.ToolFileVerdict, map[string]any{"idx": 2, "verdict": "real_gap"}); !isErr ||
		!strings.Contains(text, "refused_task_status") {
		t.Fatalf("not running = %q, want refused_task_status", text)
	}
	e := newToolEnv(t, running("odd"), pv)
	if text, isErr := e.call(t, policy.ToolFileVerdict, map[string]any{"idx": 2, "verdict": "real_gap"}); !isErr ||
		!strings.Contains(text, "refused_capability") {
		t.Fatalf("class without the tool = %q, want refused_capability", text)
	}
	e = newToolEnv(t, running("coder"), pv)
	for name, args := range map[string]map[string]any{
		"blank verdict":  {"idx": 2, "verdict": "  "},
		"negative idx":   {"idx": -1, "verdict": "real_gap"},
		"over the bound": {"idx": 2, "verdict": "wrong_test", "test_code": strings.Repeat("x", 4<<10+1)},
	} {
		if text, isErr := e.call(t, policy.ToolFileVerdict, args); !isErr || !strings.Contains(text, "refused_arguments") {
			t.Fatalf("%s = %q, want refused_arguments", name, text)
		}
	}
	e.verdicter.err = errors.New("no route")
	if text, isErr := e.call(t, policy.ToolFileVerdict, map[string]any{"idx": 2, "verdict": "real_gap"}); !isErr ||
		!strings.Contains(text, "the verdict did not reach factoryd: no route") {
		t.Fatalf("verdicter error = %q", text)
	}
	if a := e.lastAudit(t); a.Decision != "error_verdict" {
		t.Fatalf("audit = %+v", a)
	}
	if calls := e.verdicter.calls; len(calls) != 1 {
		t.Fatalf("verdicter calls = %v, want only the last one through", calls)
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

// manual reads the factory manual into the session: the loop, the card
// states, the gates. Free, for the classes that list it; a class without
// it is refused like any hidden tool.
func TestManualTool(t *testing.T) {
	v := toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2})
	v.Document.Classes["coder"].Tools["manual"] = policy.ToolCap{}
	e := newToolEnv(t, running("coder"), v)
	text, isErr := e.call(t, "manual", nil)
	if isErr || !strings.Contains(text, "Signals and members file cards") ||
		!strings.Contains(text, "backlog: waits in the backlog") ||
		!strings.Contains(text, "approval: a human approves the merge") {
		t.Fatalf("manual = %q", text)
	}
	delete(v.Document.Classes["coder"].Tools, "manual")
	off := newToolEnv(t, running("coder"), v)
	if text, isErr := off.call(t, "manual", nil); !isErr || !strings.Contains(text, "refused_capability") {
		t.Fatalf("unlisted manual = %q", text)
	}
}

type fakeDelegator struct {
	calls []Delegation
	err   error
}

func (f *fakeDelegator) Delegate(ctx context.Context, taskID string, d Delegation) (Delegated, error) {
	f.calls = append(f.calls, d)
	return Delegated{Issue: 77, TaskID: "t-child"}, f.err
}

// The delegate tool hands the child card to factoryd and tells the agent
// where it went; the session goes on. A brief or title missing is refused
// before the hand-over.
func TestDelegateTool(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	text, isErr := e.call(t, policy.ToolDelegate, map[string]any{"title": "Add the index", "brief": "In store.go add an index on tasks.rung; a test reads it."})
	if isErr || !strings.Contains(text, "issue #77 (card t-child) on the cheap rung") {
		t.Fatalf("delegate = %q (%v)", text, isErr)
	}
	d := e.delegator
	if len(d.calls) != 1 || d.calls[0].Title != "Add the index" || !strings.HasPrefix(d.calls[0].Brief, "In store.go") {
		t.Fatalf("hand-over = %+v", d.calls)
	}
	if a := e.lastAudit(t); a.Decision != "delegated" || a.Tool != policy.ToolDelegate {
		t.Fatalf("audit = %+v", a)
	}
	if text, isErr := e.call(t, policy.ToolDelegate, map[string]any{"title": "Add the index"}); !isErr || len(d.calls) != 1 {
		t.Fatalf("a delegate without a brief = %q (%v), calls %d; want refused", text, isErr, len(d.calls))
	}
}

func TestFactorydDelegateClient(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		if r.URL.Path != "/internal/delegations" || r.Header.Get("Authorization") != "Bearer shared" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"issue": 77, "task_id": "t-child"}`)
	}))
	defer srv.Close()
	f := &Factoryd{BaseURL: srv.URL, Token: "shared", HTTP: srv.Client()}
	res, err := f.Delegate(context.Background(), "t-1", Delegation{Title: "x", Brief: "y"})
	if err != nil || res.Issue != 77 || res.TaskID != "t-child" || body != `{"task_id":"t-1","title":"x","brief":"y"}` {
		t.Fatalf("res %+v err %v body %s", res, err, body)
	}
}

func TestDeploymentHealthTool(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	text, isErr := e.call(t, policy.ToolDeploymentHealth, map[string]any{"deployment": "web-api", "namespace": "factory"})
	if isErr || text != "deployment web: 2/2 ready" {
		t.Fatalf("result = %q, %v", text, isErr)
	}
	if text, isErr := e.call(t, policy.ToolDeploymentHealth, map[string]any{"deployment": "web"}); isErr || text == "" {
		t.Fatalf("no namespace = %q", text)
	}
	if !slices.Equal(e.deployments.calls, []string{"42|web-api|factory", "42|web|"}) {
		t.Fatalf("calls = %v", e.deployments.calls)
	}
	if a := e.lastAudit(t); a.Decision != "deployment_read" || a.Tool != policy.ToolDeploymentHealth || len(e.toolLedger()) != 0 {
		t.Fatalf("audit = %+v", a)
	}

	e.deployments.err = errors.New("namespace kube-system is not one of project p's namespaces: factory")
	if text, isErr := e.call(t, policy.ToolDeploymentHealth, map[string]any{"deployment": "web", "namespace": "kube-system"}); !isErr ||
		text != "namespace kube-system is not one of project p's namespaces: factory" {
		t.Fatalf("refusal = %q, %v", text, isErr)
	}
}

// A card whose project lists no namespaces runs nothing in the cluster: it
// is not offered deployment_health, and a call is refused as for a tool its
// classes do not list, with no read at factoryd.
func TestDeploymentHealthToolNeedsAProjectInTheCluster(t *testing.T) {
	pv := toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2})
	pv.Projects = map[string]policy.ProjectConfig{"code": {Repository: "sf/code"}}
	e := newToolEnv(t, running("coder"), pv)
	res, err := e.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(res.Tools, func(tl *mcp.Tool) bool { return tl.Name == policy.ToolDeploymentHealth }) {
		t.Fatalf("tools/list offers %s to a project without namespaces", policy.ToolDeploymentHealth)
	}
	if text, isErr := e.call(t, policy.ToolDeploymentHealth, map[string]any{"deployment": "web"}); !isErr || !strings.Contains(text, "refused_capability") {
		t.Fatalf("call = %q, %v; want the refusal", text, isErr)
	}
	if len(e.deployments.calls) != 0 {
		t.Fatalf("factoryd read %v for a project without namespaces", e.deployments.calls)
	}
}

func TestDeploymentHealthToolRefusesBadArguments(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	for _, args := range []map[string]any{
		{"deployment": ""},
		{"deployment": "  "},
		{"deployment": "Web"},
		{"deployment": "-web"},
		{"deployment": "web."},
		{"deployment": "we b"},
		{"deployment": "web/../x"},
		{"deployment": strings.Repeat("a", 254)},
		{"deployment": "web", "namespace": "Fac"},
		{"deployment": "web", "namespace": "a.b"},
		{"deployment": "web", "namespace": "-a"},
		{"deployment": "web", "namespace": strings.Repeat("a", 64)},
	} {
		if text, isErr := e.call(t, policy.ToolDeploymentHealth, args); !isErr || !strings.Contains(text, "refused_arguments") {
			t.Errorf("%v = %q", args, text)
		}
	}
	for _, ok := range []map[string]any{
		{"deployment": "a.b-c"},
		{"deployment": strings.Repeat("a", 253), "namespace": strings.Repeat("a", 63)},
	} {
		if text, isErr := e.call(t, policy.ToolDeploymentHealth, ok); isErr {
			t.Errorf("%v refused: %q", ok, text)
		}
	}
	if len(e.deployments.calls) != 2 {
		t.Fatalf("calls = %v, want only the two valid ones", e.deployments.calls)
	}
}

func TestDeploymentHealthToolNeedsTheClassToListIt(t *testing.T) {
	v := toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2})
	delete(v.Document.Classes["coder"].Tools, policy.ToolDeploymentHealth)
	e := newToolEnv(t, running("coder"), v)
	if text, isErr := e.call(t, policy.ToolDeploymentHealth, map[string]any{"deployment": "web"}); !isErr || !strings.Contains(text, "refused_capability") {
		t.Fatalf("unlisted = %q", text)
	}
	if len(e.deployments.calls) != 0 {
		t.Fatalf("calls = %v", e.deployments.calls)
	}
}

func TestFactorydDeploymentHealthClient(t *testing.T) {
	var method, path, auth, body string
	status, reply := http.StatusOK, `{"text":"deployment web: 2/2 ready\nevents: none"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, path, auth, body = r.Method, r.URL.Path, r.Header.Get("Authorization"), string(b)
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	defer srv.Close()
	f := &Factoryd{BaseURL: srv.URL, Token: "shared", HTTP: srv.Client()}
	text, err := f.DeploymentHealth(context.Background(), "t-1", "web", "factory")
	if err != nil || text != "deployment web: 2/2 ready\nevents: none" {
		t.Fatalf("text %q err %v", text, err)
	}
	if method != http.MethodPost || path != "/internal/deployment-health" || auth != "Bearer shared" ||
		body != `{"task_id":"t-1","deployment":"web","namespace":"factory"}` {
		t.Fatalf("%s %s auth %q body %s", method, path, auth, body)
	}
	if _, err := f.DeploymentHealth(context.Background(), "t-1", "web", ""); err != nil || body != `{"task_id":"t-1","deployment":"web"}` {
		t.Fatalf("body %s err %v", body, err)
	}

	for _, code := range []int{http.StatusForbidden, http.StatusNotFound} {
		status, reply = code, "namespace kube-system is not one of project p's namespaces: factory\n"
		if _, err := f.DeploymentHealth(context.Background(), "t-1", "web", "kube-system"); err == nil ||
			err.Error() != "namespace kube-system is not one of project p's namespaces: factory" {
			t.Fatalf("%d: err = %v", code, err)
		}
	}
	// factoryd's 502 is its own sentence (the cluster did not answer); a
	// 500 is not, and the agent gets the status alone
	status, reply = http.StatusBadGateway, "the cluster did not answer the read of Deployment web in namespace factory: try again shortly\n"
	if _, err := f.DeploymentHealth(context.Background(), "t-1", "web", ""); err == nil || err.Error() != "the cluster did not answer the read of Deployment web in namespace factory: try again shortly" {
		t.Fatalf("502: err = %v, want factoryd's sentence", err)
	}
	status, reply = http.StatusInternalServerError, "boom"
	if _, err := f.DeploymentHealth(context.Background(), "t-1", "web", ""); err == nil || err.Error() != "factoryd answered 500" {
		t.Fatalf("5xx err = %v", err)
	}
}

// analyze_image hands the image the agent names to factoryd, which looks
// at it on the card's account: the tool books nothing itself.
func TestAnalyzeImageTool(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	text, isErr := e.call(t, policy.ToolAnalyzeImage, map[string]any{"card_image": "img1.png", "ask": "is the button blue?"})
	if isErr || text != "img1.png (Screenshot.png): a login form, the button grey" {
		t.Fatalf("result = %q, %v", text, isErr)
	}
	if _, isErr := e.call(t, policy.ToolAnalyzeImage, map[string]any{"path": " shots/home.png "}); isErr {
		t.Fatal("a workspace path was refused")
	}
	if !slices.Equal(e.looker.calls, []string{"42||img1.png|is the button blue?", "42|shots/home.png||"}) {
		t.Fatalf("calls = %v", e.looker.calls)
	}
	if a := e.lastAudit(t); a.Decision != "image_looked" || a.Tool != policy.ToolAnalyzeImage || a.Reason != "shots/home.png" || len(e.toolLedger()) != 0 {
		t.Fatalf("audit = %+v, tool ledger %v", a, e.toolLedger())
	}

	e.looker.err = errors.New("no image img9 on card #12: its images are img1.png, img2.png")
	if text, isErr := e.call(t, policy.ToolAnalyzeImage, map[string]any{"card_image": "img9"}); !isErr ||
		text != "no image img9 on card #12: its images are img1.png, img2.png" {
		t.Fatalf("refusal = %q, %v", text, isErr)
	}
	if a := e.lastAudit(t); a.Decision != "error_image_look" {
		t.Fatalf("audit = %+v", a)
	}
}

func TestAnalyzeImageToolRefusesBadArguments(t *testing.T) {
	e := newToolEnv(t, running("coder"), toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	for _, args := range []map[string]any{
		{},
		{"path": "  ", "card_image": ""},
		{"path": "a.png", "card_image": "img1"},
		{"path": strings.Repeat("a", 4097)},
		{"card_image": strings.Repeat("a", 513)},
		{"card_image": "img1", "ask": strings.Repeat("a", 2001)},
	} {
		if text, isErr := e.call(t, policy.ToolAnalyzeImage, args); !isErr || !strings.Contains(text, "refused_arguments") {
			t.Errorf("%v = %q", args, text)
		}
	}
	for _, ok := range []map[string]any{
		{"path": strings.Repeat("a", 4096)},
		{"card_image": strings.Repeat("a", 512), "ask": strings.Repeat("a", 2000)},
	} {
		if text, isErr := e.call(t, policy.ToolAnalyzeImage, ok); isErr {
			t.Errorf("refused: %q", text)
		}
	}
	if len(e.looker.calls) != 2 {
		t.Fatalf("calls = %d, want only the two valid ones", len(e.looker.calls))
	}
}

func TestAnalyzeImageToolNeedsARunningTaskWhoseClassListsIt(t *testing.T) {
	v := toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2})
	delete(v.Document.Classes["coder"].Tools, policy.ToolAnalyzeImage)
	e := newToolEnv(t, running("coder"), v)
	if text, isErr := e.call(t, policy.ToolAnalyzeImage, map[string]any{"card_image": "img1"}); !isErr || !strings.Contains(text, "refused_capability") {
		t.Fatalf("unlisted = %q", text)
	}
	review := running("coder")
	review.Status = "review"
	e2 := newToolEnv(t, review, toolPolicy(policy.ToolCap{MaxRequests: 2, MaxEUR: 0.2}))
	if text, isErr := e2.call(t, policy.ToolAnalyzeImage, map[string]any{"card_image": "img1"}); !isErr || !strings.Contains(text, "refused_task_status") {
		t.Fatalf("in review = %q", text)
	}
	if len(e.looker.calls)+len(e2.looker.calls) != 0 {
		t.Fatalf("calls = %v %v", e.looker.calls, e2.looker.calls)
	}
}

func TestFactorydImageLookClient(t *testing.T) {
	var path, auth, body string
	status, reply := http.StatusOK, `{"text":"shots/home.png: a page with a blue header"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path, auth, body = r.URL.Path, r.Header.Get("Authorization"), string(b)
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	defer srv.Close()
	f := &Factoryd{BaseURL: srv.URL, Token: "shared", HTTP: srv.Client()}
	text, err := f.LookAtImage(context.Background(), "t-1", AnalyzeImageIn{Path: "shots/home.png", Ask: "the header's colour"})
	if err != nil || text != "shots/home.png: a page with a blue header" {
		t.Fatalf("text %q err %v", text, err)
	}
	if path != "/internal/image-look" || auth != "Bearer shared" || body != `{"task_id":"t-1","path":"shots/home.png","ask":"the header's colour"}` {
		t.Fatalf("%s auth %q body %s", path, auth, body)
	}
	status, reply = http.StatusNotFound, "no file shots/x.png in the workspace\n"
	if _, err := f.LookAtImage(context.Background(), "t-1", AnalyzeImageIn{Path: "shots/x.png"}); err == nil || err.Error() != "no file shots/x.png in the workspace" {
		t.Fatalf("404: err = %v", err)
	}
	status, reply = http.StatusInternalServerError, "boom"
	if _, err := f.LookAtImage(context.Background(), "t-1", AnalyzeImageIn{CardImage: "img1"}); err == nil || err.Error() != "factoryd answered 500" {
		t.Fatalf("5xx err = %v", err)
	}
}
