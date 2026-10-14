package gateway

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

var coder = []string{"coder"}

// testPolicy is the shape of the deployed v1 policy.
func testPolicy(id int64, dayEUR float64) policy.Version {
	return policy.Version{ID: id, Commit: fmt.Sprintf("c-%d", id), Tree: policy.Tree{Document: policy.Document{
		Version: 1,
		Budget:  policy.Budget{TaskDefaultEUR: 2, TaskMaxEUR: 5, DayEUR: dayEUR, MonthEUR: 50},
		Limits: policy.Limits{Sessions: 3, AgentTime: 20 * time.Minute, SupervisorCalls: 3, SupervisorTimeout: 6 * time.Minute,
			StrongerAfter: 1},
		Risk:            policy.Risk{Classes: []policy.RiskClass{{Name: "normal", Outcome: policy.OutcomeRequiresApproval}}, Default: "normal"},
		EnvelopeDefault: policy.Envelope{MonthlyEUR: 10, ServerTypes: []string{"cx23"}, MaxCount: 2, MaxLeaseHours: 4},
		Classes: map[string]policy.Class{
			"coder": {Models: []string{"coding-cheap", "coding-frontier"}},
			"odd":   {Models: []string{"unpriced"}},
		},
	}}}
}

type ledgerRow struct {
	taskID, ref string
	eur         float64
	kind        string
}

// testPrice prices every test alias: 1e-6 in, 4e-6 out per token, at most
// 32768 output tokens.
var testPrice = policy.Price{In: 1e-6, Out: 4e-6, MaxOutput: 32768}

type fakePrices map[string]policy.Price

func (f fakePrices) Price(_ context.Context, alias string) (policy.Price, error) {
	if p, ok := f[alias]; ok {
		return p, nil
	}
	return policy.Price{}, fmt.Errorf("LiteLLM has no model %q", alias)
}

var testPrices = fakePrices{"coding-cheap": testPrice, "coding-frontier": testPrice, "review-frontier": testPrice}

// memStore is the in-memory Store fake for tests only: tasks, holds and the
// ledger, with policy.Fit doing the admission as policy.TakeHold does.
type memStore struct {
	mu     sync.Mutex
	tasks  map[string]Task
	audits []AuditRow
	spends map[string]float64
	ledger []ledgerRow
	usages []policy.Usage // each ledger row's
	holds  map[int64]policy.Hold
	nextID int64
	// reqs are the hold requests in order
	reqs []policy.HoldRequest
	// day is today's ledger total before this test's calls, month the
	// month's before today
	day, month float64
	pol        *policy.Version // nil = none loaded
	polReads   int
}

func newMemStore(tasks ...Task) *memStore {
	pol := testPolicy(1, 20)
	m := &memStore{tasks: map[string]Task{}, spends: map[string]float64{}, holds: map[int64]policy.Hold{}, pol: &pol}
	for _, t := range tasks {
		m.tasks[t.ID] = t
	}
	return m
}

func (m *memStore) Task(_ context.Context, id string) (Task, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	return t, ok, nil
}

func (m *memStore) TakeHold(_ context.Context, r policy.HoldRequest) (policy.Hold, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[r.TaskID]
	if !ok {
		return policy.Hold{}, fmt.Errorf("hold: no task row for %s", r.TaskID)
	}
	if t.Status != "running" && t.Status != "in-review" {
		return policy.Hold{}, &policy.Refusal{Decision: "refused_task_status", Reason: "task " + t.ID + " is " + t.Status}
	}
	for i, l := range m.ledger {
		if u := m.usages[i]; l.taskID == r.TaskID && l.kind == r.Kind && l.ref == r.Ref && u.PromptTokens > 0 && u.BodyBytes > 0 {
			r.Earlier = u
		}
	}
	m.reqs = append(m.reqs, r)
	dayTotal, projectTotal := m.day, 0.0
	for _, l := range m.ledger {
		dayTotal += l.eur
		if m.tasks[l.taskID].Repository == t.Repository {
			projectTotal += l.eur
		}
	}
	taskHeld, dayHeld, projectHeld := 0.0, 0.0, 0.0
	for _, h := range m.holds {
		dayHeld += h.EUR
		if h.TaskID == r.TaskID {
			taskHeld += h.EUR
		}
		if m.tasks[h.TaskID].Repository == t.Repository {
			projectHeld += h.EUR
		}
	}
	taskHeadroom, dayHeadroom, monthHeadroom, projectHeadroom := t.BudgetEUR-t.SpentEUR, r.DayEUR-dayTotal, r.MonthEUR-m.month-dayTotal, r.ProjectEUR-projectTotal
	free := policy.Free{Task: taskHeadroom - taskHeld, Day: dayHeadroom - dayHeld, Month: monthHeadroom - dayHeld, Project: projectHeadroom - projectHeld}
	var h policy.Hold
	var err error
	if r.Kind == policy.KindTool {
		requests, used := 0, 0.0
		for _, l := range m.ledger {
			if l.taskID == r.TaskID && l.kind == policy.KindTool && l.ref == r.Ref {
				requests, used = requests+1, used+l.eur
			}
		}
		for _, o := range m.holds {
			if o.TaskID == r.TaskID && o.Kind == policy.KindTool && o.Ref == r.Ref {
				requests, used = requests+1, used+o.EUR
			}
		}
		h, err = policy.FitTool(free, requests, used, r)
	} else {
		h, err = policy.Fit(free, r)
	}
	if err != nil {
		return h, err
	}
	m.nextID++
	h.ID = m.nextID
	h.Headroom, h.Bound = policy.HeadroomBound(taskHeadroom, dayHeadroom, monthHeadroom, projectHeadroom)
	m.holds[h.ID] = h
	return h, nil
}

func (m *memStore) ReleaseHold(_ context.Context, h policy.Hold, cost *float64, u policy.Usage) (policy.Booking, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cost = policy.KnownCost(cost)
	held, ok := m.holds[h.ID]
	delete(m.holds, h.ID)
	amount := held.EUR
	if cost != nil {
		amount = *cost
	}
	if !ok {
		amount = 0
		if cost != nil {
			amount = max(0, *cost-h.EUR)
		}
	}
	t := m.tasks[h.TaskID]
	if amount > 0 || ok && h.Kind == policy.KindTool {
		t.SpentEUR += amount
		m.tasks[h.TaskID] = t
		m.spends[h.TaskID] = t.SpentEUR
		m.ledger = append(m.ledger, ledgerRow{h.TaskID, h.Ref, amount, h.Kind})
		m.usages = append(m.usages, u)
	}
	b := policy.Booking{Spent: t.SpentEUR, Booked: amount}
	if cost != nil {
		b.OverHold = policy.OverHold(h.TaskID, h.Kind+" "+h.Ref, *cost, h.EUR)
	}
	return b, nil
}

func (m *memStore) openHolds() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.holds)
}

func (m *memStore) CurrentPolicy(context.Context) (policy.Version, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.polReads++
	if m.pol == nil {
		return policy.Version{}, false, nil
	}
	return *m.pol, true, nil
}

func (m *memStore) setPolicy(v policy.Version) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pol = &v
}

func (m *memStore) InsertAudit(_ context.Context, row AuditRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audits = append(m.audits, row)
	return nil
}

func (m *memStore) auditRows() []AuditRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]AuditRow(nil), m.audits...)
}

func (m *memStore) spentOf(id string) (float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.spends[id]
	return v, ok
}

var _ Store = (*memStore)(nil)

type fakeAuth struct {
	id  Identity
	err error
}

func (f fakeAuth) Authenticate(*http.Request) (Identity, error) { return f.id, f.err }

type mintCall struct {
	taskID    string
	maxBudget float64
	models    string // comma-joined
}

type fakeMinter struct {
	mu      sync.Mutex
	calls   []mintCall
	blocked []string
	err     error
	hold    map[string]chan struct{} // a task's mint waits until its channel closes
}

func (f *fakeMinter) Block(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked = append(f.blocked, key)
	return nil
}

func (f *fakeMinter) blockedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.blocked...)
}

func (f *fakeMinter) Mint(_ context.Context, slot, taskID string, maxBudget float64, models []string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, mintCall{taskID, maxBudget, strings.Join(models, ",")})
	hold := f.hold[taskID]
	f.mu.Unlock()
	if hold != nil {
		<-hold
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	return fmt.Sprintf("sk-task-key-%d", len(f.calls)), nil
}

func (f *fakeMinter) mintCalls() []mintCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mintCall(nil), f.calls...)
}

type stubResp struct {
	status  int
	body    string
	headers map[string]string
}

type stubUpstream struct {
	*httptest.Server
	mu     sync.Mutex
	auths  []string
	bodies []string
	paths  []string
	resp   stubResp
}

func newStubUpstream(t *testing.T) *stubUpstream {
	s := &stubUpstream{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.paths = append(s.paths, r.URL.Path)
		s.bodies = append(s.bodies, string(body))
		resp := s.resp
		s.mu.Unlock()
		for k, v := range resp.headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(resp.status)
		w.Write([]byte(resp.body))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stubUpstream) setResp(resp stubResp) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resp = resp
}

func (s *stubUpstream) seen() (auths, bodies, paths []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auths...),
		append([]string(nil), s.bodies...),
		append([]string(nil), s.paths...)
}

func chatBody(model string) string {
	// keys in encoding/json order, so the forwarded re-encoding is byte-identical
	return fmt.Sprintf(`{"messages":[{"role":"user","content":"hi"}],"model":%q}`, model)
}

func doChat(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer forged-but-fakeAuth-ignores")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

func TestChatCompletions(t *testing.T) {
	runningTask := Task{Classes: coder, ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"}
	testIdentity := Identity{
		Subject:   "system:serviceaccount:factory:task-42",
		Namespace: "factory",
		TaskID:    "42",
		SAUID:     "sa-uid-1",
	}

	tests := []struct {
		name string

		authErr     error
		task        *Task
		reqBody     string
		mintErr     error
		upstreamURL string // "" = stub URL; "closed" = unreachable port
		resp        stubResp
		noPolicy    bool    // no policy version loaded yet
		oldPolicy   bool    // the version in force is a row from before the tree
		dayTotal    float64 // today's ledger total before the call (policy day cap 20)
		monthTotal  float64 // the month's ledger total before today (policy month cap 50)

		wantStatus      int
		wantDecision    string
		wantReason      string // "" = don't check
		wantOverHold    string // the last audit row's over-hold flag
		wantDeployment  string // the last audit row's deployment
		wantErrType     string // "" = no error body expected
		wantBody        string // "" = don't check
		wantUpstreamReq string // "" = the client body forwarded verbatim
		wantSpent       *float64
		wantMintBudget  *float64
		wantNoMint      bool
		wantUpstreamHit int
		wantAlias       string // "" = don't check
	}{
		{
			name:         "a model outside the task's classes refuses 403",
			wantNoMint:   true,
			task:         &runningTask,
			reqBody:      chatBody("review-frontier"),
			wantStatus:   http.StatusForbidden,
			wantDecision: "refused_capability",
			wantErrType:  "permission_error",
			wantAlias:    "review-frontier",
		},
		{
			name:         "a task without capability classes refuses 403",
			wantNoMint:   true,
			task:         &Task{ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"},
			reqBody:      chatBody("coding-cheap"),
			wantStatus:   http.StatusForbidden,
			wantDecision: "refused_capability",
			wantErrType:  "permission_error",
		},
		{
			name:         "a class the policy does not define allows nothing",
			wantNoMint:   true,
			task:         &Task{Classes: []string{"reviewer"}, ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"},
			reqBody:      chatBody("coding-cheap"),
			wantStatus:   http.StatusForbidden,
			wantDecision: "refused_capability",
			wantErrType:  "permission_error",
		},
		{
			name:         "no policy version loaded refuses 503",
			wantNoMint:   true,
			noPolicy:     true,
			task:         &runningTask,
			reqBody:      chatBody("coding-cheap"),
			wantStatus:   http.StatusServiceUnavailable,
			wantDecision: "error_policy",
			wantErrType:  "server_error",
		},
		{
			// the stored row of a policy file decodes to a zero tree; it is
			// no policy, not an empty one
			name:         "a version in force that does not validate refuses 503",
			wantNoMint:   true,
			oldPolicy:    true,
			task:         &runningTask,
			reqBody:      chatBody("coding-cheap"),
			wantStatus:   http.StatusServiceUnavailable,
			wantDecision: "error_policy",
			wantErrType:  "server_error",
		},
		{
			name:         "the day cap reached refuses 402",
			wantNoMint:   true,
			dayTotal:     20,
			task:         &runningTask,
			reqBody:      chatBody("coding-cheap"),
			wantStatus:   http.StatusPaymentRequired,
			wantDecision: "refused_day_cap",
			wantErrType:  "budget_exceeded",
		},
		{
			name:         "the month cap reached refuses 402",
			wantNoMint:   true,
			monthTotal:   49,
			dayTotal:     1,
			task:         &runningTask,
			reqBody:      chatBody("coding-cheap"),
			wantStatus:   http.StatusPaymentRequired,
			wantDecision: "refused_month_cap",
			wantErrType:  "budget_exceeded",
		},
		{
			name:            "the key is capped at the month headroom when it is the smallest",
			monthTotal:      47,
			dayTotal:        1,
			task:            &runningTask,
			reqBody:         chatBody("coding-cheap"),
			resp:            stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-response-cost": "0.1"}},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantMintBudget:  ptr(2.0),
			wantSpent:       ptr(2.1),
			wantUpstreamHit: 1,
		},
		{
			name:            "the key is capped at the day headroom when it is the smaller",
			dayTotal:        17,
			task:            &runningTask,
			reqBody:         chatBody("coding-cheap"),
			resp:            stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-response-cost": "0.1"}},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantMintBudget:  ptr(3.0),
			wantSpent:       ptr(2.1),
			wantUpstreamHit: 1,
		},
		{
			name:         "no or invalid jwt refuses 401",
			wantNoMint:   true,
			authErr:      errors.New("missing bearer token"),
			task:         &runningTask,
			reqBody:      chatBody("coding-cheap"),
			wantStatus:   http.StatusUnauthorized,
			wantDecision: "refused_auth",
			wantErrType:  "authentication_error",
		},
		{
			name:         "malformed body refuses 400",
			wantNoMint:   true,
			task:         &runningTask,
			reqBody:      "not json",
			wantStatus:   http.StatusBadRequest,
			wantDecision: "refused_body",
			wantErrType:  "invalid_request_error",
		},
		{
			name:         "unknown task refuses 404",
			wantNoMint:   true,
			task:         nil,
			reqBody:      chatBody("coding-cheap"),
			wantStatus:   http.StatusNotFound,
			wantDecision: "refused_task_unknown",
			wantErrType:  "invalid_request_error",
		},
		{
			name:         "non-running task refuses 409",
			wantNoMint:   true,
			task:         &Task{Classes: coder, ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "completed"},
			reqBody:      chatBody("coding-cheap"),
			wantStatus:   http.StatusConflict,
			wantDecision: "refused_task_status",
			wantErrType:  "invalid_request_error",
		},
		{
			name:            "spent equals budget refuses 402",
			wantNoMint:      true,
			task:            &Task{Classes: coder, ID: "42", BudgetEUR: 10, SpentEUR: 10, Status: "running"},
			reqBody:         chatBody("coding-cheap"),
			wantStatus:      http.StatusPaymentRequired,
			wantDecision:    "refused_budget",
			wantErrType:     "budget_exceeded",
			wantUpstreamHit: 0,
		},
		{
			name:            "spent over budget refuses 402",
			wantNoMint:      true,
			task:            &Task{Classes: coder, ID: "42", BudgetEUR: 10, SpentEUR: 11, Status: "running"},
			reqBody:         chatBody("coding-cheap"),
			wantStatus:      http.StatusPaymentRequired,
			wantDecision:    "refused_budget",
			wantErrType:     "budget_exceeded",
			wantUpstreamHit: 0,
		},
		{
			name:         "key mint failure refuses 502",
			task:         &runningTask,
			reqBody:      chatBody("coding-cheap"),
			mintErr:      errors.New("litellm down"),
			wantStatus:   http.StatusBadGateway,
			wantDecision: "error_mint",
			wantErrType:  "upstream_error",
		},
		{
			name:    "litellm 429 budget_exceeded maps to 402",
			task:    &runningTask,
			reqBody: chatBody("coding-cheap"),
			resp: stubResp{
				status: http.StatusTooManyRequests,
				body:   `{"error":{"message":"Budget has been exceeded! Key=42 (sk-...W8TA) Current cost: 8.0, Max budget: 8.0","type":"budget_exceeded","param":null,"code":"429"}}`,
			},
			wantStatus:      http.StatusPaymentRequired,
			wantDecision:    "refused_budget",
			wantErrType:     "budget_exceeded",
			wantUpstreamHit: 1,
		},
		{
			name:     "litellm 429 budget_exceeded on a key the day capped is a day-cap refusal",
			task:     &runningTask,
			dayTotal: 15, // day headroom 5 < task headroom 8
			reqBody:  chatBody("coding-cheap"),
			resp: stubResp{
				status: http.StatusTooManyRequests,
				body:   `{"error":{"message":"Budget has been exceeded! Key=42 (sk-...W8TA) Current cost: 5.0, Max budget: 5.0","type":"budget_exceeded","param":null,"code":"429"}}`,
			},
			wantStatus:      http.StatusPaymentRequired,
			wantDecision:    "refused_day_cap",
			wantErrType:     "budget_exceeded",
			wantUpstreamHit: 1,
		},
		{
			// LiteLLM 1.102.2 auth_checks.py _global_proxy_budget_check raises
			// BudgetExceededError without a message; exceptions.py's default
			// names no entity, auth_exception_handler.py passes it through
			name:    "litellm 429 on the proxy-wide budget is a month-cap refusal",
			task:    &runningTask,
			reqBody: chatBody("coding-cheap"),
			resp: stubResp{
				status: http.StatusTooManyRequests,
				body:   `{"error":{"message":"Budget has been exceeded! Current cost: 55.0123, Max budget: 55.0","type":"budget_exceeded","param":null,"code":"429"}}`,
			},
			wantStatus:      http.StatusPaymentRequired,
			wantDecision:    "refused_month_cap",
			wantReason:      "litellm proxy budget exceeded (litellm_settings.max_budget, the proxy-wide monthly stop behind budget.month_eur): Budget has been exceeded! Current cost: 55.0123, Max budget: 55.0",
			wantErrType:     "budget_exceeded",
			wantUpstreamHit: 1,
		},
		{
			// the key's window budget (auth_checks.py) names the key
			name:     "litellm 429 on a key's window budget stays the key's bound",
			task:     &runningTask,
			dayTotal: 15,
			reqBody:  chatBody("coding-cheap"),
			resp: stubResp{
				status: http.StatusTooManyRequests,
				body:   `{"error":{"message":"ExceededBudget: Key over 1d budget. Spend=$5.0100, Limit=$5.00","type":"budget_exceeded","param":null,"code":"429"}}`,
			},
			wantStatus:      http.StatusPaymentRequired,
			wantDecision:    "refused_day_cap",
			wantErrType:     "budget_exceeded",
			wantUpstreamHit: 1,
		},
		{
			name:    "a failure LiteLLM priced is booked",
			task:    &runningTask,
			reqBody: chatBody("coding-cheap"),
			resp: stubResp{status: http.StatusInternalServerError, body: `{"error":{"message":"provider broke mid-way"}}`,
				headers: map[string]string{"x-litellm-response-cost": "0.25"}},
			wantStatus:      http.StatusInternalServerError,
			wantDecision:    "error_upstream",
			wantOverHold:    "COST OVER HOLD: model coding-cheap cost 0.250000 EUR against a hold of 0.131204 EUR; booked in full",
			wantSpent:       ptr(2.25),
			wantUpstreamHit: 1,
		},
		{
			name:    "a non-finite cost books the hold and audits the missing-cost reason",
			task:    &runningTask,
			reqBody: chatBody("coding-cheap"),
			resp:    stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-response-cost": "NaN"}},
			// 132 input tokens at 1e-6 and 32768 output tokens at 4e-6
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantReason:      "cost not reported, booked the hold 0.131204 EUR",
			wantSpent:       ptr(2.131204),
			wantUpstreamHit: 1,
		},
		{
			name:    "litellm 429 rate limit passes through",
			task:    &runningTask,
			reqBody: chatBody("coding-cheap"),
			resp: stubResp{
				status: http.StatusTooManyRequests,
				body:   `{"error":{"message":"No deployments available","type":"rate_limit_error","param":null,"code":"429"}}`,
			},
			wantStatus:      http.StatusTooManyRequests,
			wantDecision:    "error_upstream",
			wantBody:        `{"error":{"message":"No deployments available","type":"rate_limit_error","param":null,"code":"429"}}`,
			wantUpstreamHit: 1,
		},
		{
			name:            "litellm 500 passes through",
			task:            &runningTask,
			reqBody:         chatBody("coding-cheap"),
			resp:            stubResp{status: http.StatusInternalServerError, body: `{"error":"boom"}`},
			wantStatus:      http.StatusInternalServerError,
			wantDecision:    "error_upstream",
			wantBody:        `{"error":"boom"}`,
			wantUpstreamHit: 1,
		},
		{
			name:    "a failed call names the deployment LiteLLM tried",
			task:    &runningTask,
			reqBody: chatBody("coding-cheap"),
			resp: stubResp{status: http.StatusServiceUnavailable, body: `{"error":"down"}`,
				headers: map[string]string{"x-litellm-model-name": "dashscope/qwen3.8-flash"}},
			wantStatus:      http.StatusServiceUnavailable,
			wantDecision:    "error_upstream",
			wantDeployment:  "dashscope/qwen3.8-flash",
			wantBody:        `{"error":"down"}`,
			wantUpstreamHit: 1,
		},
		{
			name:         "litellm unreachable refuses 502",
			task:         &runningTask,
			reqBody:      chatBody("coding-cheap"),
			upstreamURL:  "closed",
			wantStatus:   http.StatusBadGateway,
			wantDecision: "error_upstream",
			wantErrType:  "upstream_error",
			// the provider may have run part of it: the hold is booked,
			// 132 input tokens at 1e-6 and 32768 output tokens at 4e-6
			wantSpent:       ptr(2.131204),
			wantUpstreamHit: 0,
		},
		{
			name:    "under budget proxies and books the call cost",
			task:    &runningTask,
			reqBody: chatBody("coding-cheap"),
			resp: stubResp{status: http.StatusOK, body: `{"choices":[{"message":{"role":"assistant"}}]}`, headers: map[string]string{
				"x-litellm-response-cost": "0.1",
				"x-litellm-key-spend":     "7", // stale cache read: never booked
				"x-litellm-model-name":    "deepseek/deepseek-flash",
			}},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantDeployment:  "deepseek/deepseek-flash",
			wantBody:        `{"choices":[{"message":{"role":"assistant"}}]}`,
			wantSpent:       ptr(2.1),
			wantMintBudget:  ptr(8.0),
			wantUpstreamHit: 1,
		},
		{
			// 132 input bytes and 32768 output tokens at 1e-6 and 4e-6 EUR
			// hold 0.131204 EUR
			name:            "a cost above the hold is booked in full and flagged",
			task:            &runningTask,
			reqBody:         chatBody("coding-cheap"),
			resp:            stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-response-cost": "0.5"}},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantOverHold:    "COST OVER HOLD: model coding-cheap cost 0.500000 EUR against a hold of 0.131204 EUR; booked in full",
			wantSpent:       ptr(2.5),
			wantUpstreamHit: 1,
		},
		{
			name:    "stream books the usage cost of the final chunk",
			task:    &runningTask,
			reqBody: `{"model":"coding-frontier","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			resp: stubResp{status: http.StatusOK, body: sseBody, headers: map[string]string{
				"Content-Type":            "text/event-stream",
				"x-litellm-response-cost": "0.0",
				"x-litellm-key-spend":     "0.0",
				"x-litellm-model-name":    "zai/glm-5.3",
			}},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantDeployment:  "zai/glm-5.3",
			wantBody:        sseBody,
			wantUpstreamReq: `{"messages":[{"role":"user","content":"hi"}],"model":"coding-frontier","stream":true,"stream_options":{"include_usage":true}}`,
			wantSpent:       ptr(2.125),
			wantUpstreamHit: 1,
		},
		{
			name:            "stream forces include_usage over the client's choice",
			task:            &runningTask,
			reqBody:         `{"model":"coding-frontier","stream":true,"stream_options":{"include_usage":false,"x":1},"messages":[]}`,
			resp:            stubResp{status: http.StatusOK, body: sseBody, headers: map[string]string{"Content-Type": "text/event-stream"}},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantUpstreamReq: `{"messages":[],"model":"coding-frontier","stream":true,"stream_options":{"include_usage":true,"x":1}}`,
			wantSpent:       ptr(2.125),
			wantUpstreamHit: 1,
		},
		{
			name:            "stream without a usage chunk books the hold",
			task:            &runningTask,
			reqBody:         `{"model":"coding-frontier","stream":true,"messages":[]}`,
			wantUpstreamReq: `{"messages":[],"model":"coding-frontier","stream":true,"stream_options":{"include_usage":true}}`,
			resp:            stubResp{status: http.StatusOK, body: "data: {\"choices\":[]}\n\ndata: [DONE]\n\n", headers: map[string]string{"Content-Type": "text/event-stream"}},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantSpent:       ptr(2.131231), // 159 input and 32768 output tokens
			wantUpstreamHit: 1,
		},
		{
			name:         "a case variant of a field is refused",
			wantNoMint:   true,
			task:         &runningTask,
			reqBody:      `{"model":"coding-frontier","stream":true,"STREAM":false,"messages":[]}`,
			wantStatus:   http.StatusBadRequest,
			wantDecision: "refused_body",
			wantErrType:  "invalid_request_error",
		},
		{
			name:            "a repeated stream key counts by its last value and is forwarded once",
			task:            &runningTask,
			reqBody:         `{"model":"coding-frontier","stream":false,"messages":[],"stream":true}`,
			resp:            stubResp{status: http.StatusOK, body: sseBody, headers: map[string]string{"Content-Type": "text/event-stream", "x-litellm-response-cost": "0.0"}},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantUpstreamReq: `{"messages":[],"model":"coding-frontier","stream":true,"stream_options":{"include_usage":true}}`,
			wantSpent:       ptr(2.125),
			wantUpstreamHit: 1,
		},
		{
			name:         "a case variant of model is refused",
			wantNoMint:   true,
			task:         &runningTask,
			reqBody:      `{"Model":"review-frontier","model":"coding-cheap","messages":[]}`,
			wantStatus:   http.StatusBadRequest,
			wantDecision: "refused_body",
			wantErrType:  "invalid_request_error",
		},
		{
			name:         "LiteLLM fallbacks are refused",
			wantNoMint:   true,
			task:         &runningTask,
			reqBody:      `{"model":"coding-cheap","fallbacks":["review-frontier"],"mock_testing_fallbacks":true,"messages":[]}`,
			wantStatus:   http.StatusBadRequest,
			wantDecision: "refused_body",
			wantErrType:  "invalid_request_error",
			wantBody:     `{"error":{"code":"400","message":"fields [\"fallbacks\" \"mock_testing_fallbacks\"] are not accepted","param":null,"type":"invalid_request_error"}}` + "\n",
		},
		{
			name:         "a router settings override is refused",
			wantNoMint:   true,
			task:         &runningTask,
			reqBody:      `{"model":"coding-cheap","router_settings_override":{"fallbacks":[{"coding-cheap":["supervisor"]}]},"messages":[]}`,
			wantStatus:   http.StatusBadRequest,
			wantDecision: "refused_body",
			wantErrType:  "invalid_request_error",
		},
		{
			name:         "a provider built-in tool is refused",
			wantNoMint:   true,
			task:         &runningTask,
			reqBody:      `{"model":"coding-cheap","tools":[{"type":"function","function":{"name":"bash"}},{"type":"web_search","web_search":{"enable":true}}],"messages":[]}`,
			wantStatus:   http.StatusBadRequest,
			wantDecision: "refused_body",
			wantErrType:  "invalid_request_error",
		},
		{
			name:         "a tool whose exact type key is not function is refused",
			wantNoMint:   true,
			task:         &runningTask,
			reqBody:      `{"model":"coding-cheap","tools":[{"Type":"function","type":"retrieval"}],"messages":[]}`,
			wantStatus:   http.StatusBadRequest,
			wantDecision: "refused_body",
			wantErrType:  "invalid_request_error",
		},
		{
			name:         "a tool_choice naming a built-in tool is refused",
			wantNoMint:   true,
			task:         &runningTask,
			reqBody:      `{"model":"coding-cheap","tool_choice":{"type":"web_search"},"messages":[]}`,
			wantStatus:   http.StatusBadRequest,
			wantDecision: "refused_body",
			wantErrType:  "invalid_request_error",
		},
		{
			name:         "an image part is refused",
			wantNoMint:   true,
			task:         &runningTask,
			reqBody:      `{"model":"coding-cheap","messages":[{"role":"user","content":[{"type":"text","text":"see"},{"type":"image_url","image_url":{"url":"http://10.0.0.1/x.png","detail":"high"}}]}]}`,
			wantStatus:   http.StatusBadRequest,
			wantDecision: "refused_body",
			wantErrType:  "invalid_request_error",
		},
		{
			name:         "an audio part is refused",
			wantNoMint:   true,
			task:         &runningTask,
			reqBody:      `{"model":"coding-cheap","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`,
			wantStatus:   http.StatusBadRequest,
			wantDecision: "refused_body",
			wantErrType:  "invalid_request_error",
		},
		{
			name:         "thinking is refused",
			wantNoMint:   true,
			task:         &runningTask,
			reqBody:      `{"model":"coding-cheap","thinking":{"type":"enabled","budget_tokens":100000},"messages":[]}`,
			wantStatus:   http.StatusBadRequest,
			wantDecision: "refused_body",
			wantErrType:  "invalid_request_error",
		},
		{
			name:            "text parts, function tools, tool calls and tool results pass",
			task:            &runningTask,
			reqBody:         `{"max_tokens":100,"messages":[{"content":"sys","role":"system"},{"content":[{"text":"hi","type":"text"}],"role":"user"},{"content":null,"role":"assistant","tool_calls":[{"function":{"arguments":"{}","name":"bash"},"id":"c1","type":"function"}]},{"content":"ok","role":"tool","tool_call_id":"c1"}],"model":"coding-cheap","tool_choice":"auto","tools":[{"function":{"name":"bash"},"type":"function"}]}`,
			resp:            stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-response-cost": "0.0005"}},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantSpent:       ptr(2.0005),
			wantUpstreamHit: 1,
		},
		{
			name:         "n above 1 is refused",
			wantNoMint:   true,
			task:         &runningTask,
			reqBody:      `{"model":"coding-cheap","n":2,"messages":[]}`,
			wantStatus:   http.StatusBadRequest,
			wantDecision: "refused_body",
			wantErrType:  "invalid_request_error",
		},
		{
			name:         "the caller's max_completion_tokens is clamped to the headroom",
			task:         &Task{Classes: coder, ID: "42", BudgetEUR: 0.01, Status: "running"},
			reqBody:      `{"model":"coding-cheap","max_completion_tokens":50000,"messages":[]}`,
			resp:         stubResp{status: http.StatusOK, body: `{"choices":[]}`, headers: map[string]string{"x-litellm-response-cost": "0.001"}},
			wantStatus:   http.StatusOK,
			wantDecision: "allowed",
			// 0.01 free: 68 body bytes + 64 = 132 input tokens at 1e-6 leave
			// 0.009868, 2467 output tokens at 4e-6
			wantUpstreamReq: `{"max_completion_tokens":2467,"messages":[],"model":"coding-cheap"}`,
			wantSpent:       ptr(0.001),
			wantUpstreamHit: 1,
		},
		{
			name:       "a headroom below the minimum call refuses 402",
			wantNoMint: true,
			// 0.004 free: 132 input tokens leave room for 967 output tokens
			task:         &Task{Classes: coder, ID: "42", BudgetEUR: 0.004, Status: "running"},
			reqBody:      chatBody("coding-cheap"),
			wantStatus:   http.StatusPaymentRequired,
			wantDecision: "refused_budget",
			wantErrType:  "budget_exceeded",
		},
		{
			name:         "an alias LiteLLM cannot price refuses 503",
			wantNoMint:   true,
			task:         &Task{Classes: []string{"odd"}, ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"},
			reqBody:      chatBody("unpriced"),
			wantStatus:   http.StatusServiceUnavailable,
			wantDecision: "error_price",
			wantErrType:  "server_error",
		},
		{
			name:            "a repeated model key counts by its last value and is forwarded once",
			task:            &runningTask,
			reqBody:         `{"model":"review-frontier","messages":[],"model":"coding-cheap"}`,
			resp:            stubResp{status: http.StatusOK, body: `{"choices":[]}`, headers: map[string]string{"x-litellm-response-cost": "0.1"}},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantUpstreamReq: `{"messages":[],"model":"coding-cheap"}`,
			wantSpent:       ptr(2.1),
			wantUpstreamHit: 1,
			wantAlias:       "coding-cheap",
		},
		{
			name:         "a non-boolean stream refuses 400",
			wantNoMint:   true,
			task:         &runningTask,
			reqBody:      `{"model":"coding-cheap","stream":"yes","messages":[]}`,
			wantStatus:   http.StatusBadRequest,
			wantDecision: "refused_body",
			wantErrType:  "invalid_request_error",
		},
		{
			name:         "a non-string model refuses 400",
			wantNoMint:   true,
			task:         &runningTask,
			reqBody:      `{"model":7,"messages":[]}`,
			wantStatus:   http.StatusBadRequest,
			wantDecision: "refused_body",
			wantErrType:  "invalid_request_error",
		},
		{
			name:         "a JSON array body refuses 400",
			wantNoMint:   true,
			task:         &runningTask,
			reqBody:      `[{"model":"coding-cheap"}]`,
			wantStatus:   http.StatusBadRequest,
			wantDecision: "refused_body",
			wantErrType:  "invalid_request_error",
		},
		{
			name:            "missing cost header books the hold",
			task:            &runningTask,
			reqBody:         chatBody("coding-frontier"),
			resp:            stubResp{status: http.StatusOK, body: `{"choices":[]}`},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantSpent:       ptr(2.131207), // 135 input and 32768 output tokens
			wantBody:        `{"choices":[]}`,
			wantUpstreamHit: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := newStubUpstream(t)
			stub.setResp(tt.resp)

			task := Task{Classes: coder, ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"}
			if tt.task != nil {
				task = *tt.task
			}
			store := newMemStore()
			if tt.task != nil {
				// the task's session runs on the alias the case calls;
				// TestAgentCallsOnlyItsSessionAlias refuses any other
				var req struct{ Model string }
				json.Unmarshal([]byte(tt.reqBody), &req)
				task.ModelAlias = req.Model
				store.tasks[task.ID] = task
			}
			if tt.noPolicy {
				store.pol = nil
			}
			if tt.oldPolicy {
				store.pol = &policy.Version{ID: 1, Commit: "e3b0c442"}
			}
			store.day, store.month = tt.dayTotal, tt.monthTotal
			minter := &fakeMinter{err: tt.mintErr}

			url := stub.URL
			if tt.upstreamURL == "closed" {
				url = "http://127.0.0.1:1"
			}
			s := NewServer(ServerOptions{
				Auth:       fakeAuth{id: testIdentity, err: tt.authErr},
				Store:      store,
				Minter:     minter,
				LiteLLMURL: url,
				Prices:     testPrices,
			})

			rr := doChat(t, s.Routes("test"), tt.reqBody)
			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rr.Code, tt.wantStatus, rr.Body.String())
			}

			audits := store.auditRows()
			if len(audits) == 0 {
				t.Fatal("no audit row written")
			}
			for _, a := range audits {
				unset := tt.noPolicy || tt.oldPolicy
				if unset && a.PolicyVersion != nil || !unset && (a.PolicyVersion == nil || *a.PolicyVersion != 1) {
					t.Fatalf("audit row %+v names policy version %v, want 1 (nil without a policy)", a, a.PolicyVersion)
				}
			}
			last := audits[len(audits)-1]
			if last.Decision != tt.wantDecision {
				t.Fatalf("decision = %q, want %q (all rows %+v)", last.Decision, tt.wantDecision, audits)
			}
			if tt.wantReason != "" && last.Reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", last.Reason, tt.wantReason)
			}
			if last.OverHold != tt.wantOverHold {
				t.Fatalf("over hold = %q, want %q", last.OverHold, tt.wantOverHold)
			}
			if last.Deployment != tt.wantDeployment {
				t.Fatalf("deployment = %q, want %q", last.Deployment, tt.wantDeployment)
			}
			// an agent never learns the deployment behind its alias
			if d := rr.Header().Get(HeaderDeployment); d != "" {
				t.Fatalf("an agent's answer names deployment %q", d)
			}

			if tt.wantErrType != "" {
				var eb struct {
					Error struct {
						Message string `json:"message"`
						Type    string `json:"type"`
						Code    string `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal(rr.Body.Bytes(), &eb); err != nil {
					t.Fatalf("error body not json: %v (%s)", err, rr.Body.String())
				}
				if eb.Error.Type != tt.wantErrType {
					t.Fatalf("error.type = %q, want %q", eb.Error.Type, tt.wantErrType)
				}
				wantCode := fmt.Sprint(rr.Code)
				if eb.Error.Code != wantCode {
					t.Fatalf("error.code = %q, want %q", eb.Error.Code, wantCode)
				}
			}
			if tt.wantBody != "" && rr.Body.String() != tt.wantBody {
				t.Fatalf("body = %q, want %q", rr.Body.String(), tt.wantBody)
			}

			if tt.wantSpent != nil {
				got, ok := store.spentOf("42")
				if !ok {
					t.Fatal("spent never updated")
				}
				if got != *tt.wantSpent {
					t.Fatalf("spent = %v, want %v", got, *tt.wantSpent)
				}
			} else if _, ok := store.spentOf("42"); ok {
				t.Fatal("spent updated, want untouched")
			}

			calls := minter.mintCalls()
			if tt.wantMintBudget != nil {
				if len(calls) != 1 {
					t.Fatalf("mint calls = %d, want 1", len(calls))
				}
				if calls[0].maxBudget != *tt.wantMintBudget || calls[0].taskID != "42" {
					t.Fatalf("mint call = %+v, want task 42 budget %v", calls[0], *tt.wantMintBudget)
				}
			} else if tt.wantNoMint && len(calls) != 0 {
				t.Fatalf("mint calls = %+v, want none", calls)
			}

			auths, bodies, paths := stub.seen()
			if len(auths) != tt.wantUpstreamHit {
				t.Fatalf("upstream hits = %d, want %d", len(auths), tt.wantUpstreamHit)
			}
			for i, a := range auths {
				if a != "Bearer sk-task-key-1" {
					t.Fatalf("upstream auth[%d] = %q, want the minted virtual key", i, a)
				}
				if paths[i] != "/v1/chat/completions" {
					t.Fatalf("upstream path[%d] = %q, want /v1/chat/completions", i, paths[i])
				}
				want := tt.wantUpstreamReq
				if want == "" {
					want = tt.reqBody
				}
				if !strings.Contains(want, `"max_`) {
					want = withMaxTokens(t, want, 32768)
				}
				if bodies[i] != want {
					t.Fatalf("upstream body[%d] = %q, want %q", i, bodies[i], want)
				}
			}

			if n := store.openHolds(); n != 0 {
				t.Fatalf("%d holds left open, want every hold released", n)
			}
			if last.Decision == "allowed" && (last.OutputLimit == nil || last.HoldEUR == nil) {
				t.Fatalf("allowed audit row lacks its output limit and hold: %+v", last)
			}
			if tt.wantAlias != "" && last.ModelAlias != tt.wantAlias {
				t.Fatalf("audited model = %q, want %q", last.ModelAlias, tt.wantAlias)
			}
			if tt.wantUpstreamHit > 0 && last.Decision == "allowed" {
				if last.ModelAlias == "" {
					t.Fatalf("allowed audit row missing model alias: %+v", last)
				}
			}
		})
	}
}

func TestKeyCachedPerTask(t *testing.T) {
	stub := newStubUpstream(t)
	store := newMemStore(Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"})
	minter := &fakeMinter{}
	s := NewServer(ServerOptions{
		Auth:       fakeAuth{id: Identity{Subject: "system:serviceaccount:factory:task-42", Namespace: "factory", TaskID: "42", SAUID: "sa-uid-1"}},
		Store:      store,
		Minter:     minter,
		LiteLLMURL: stub.URL,
		Prices:     testPrices,
	})

	stub.setResp(stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-response-cost": "0.5"}})
	if rr := doChat(t, s.Routes("test"), chatBody("coding-cheap")); rr.Code != http.StatusOK {
		t.Fatalf("first call status = %d", rr.Code)
	}
	stub.setResp(stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-response-cost": "0.25"}})
	if rr := doChat(t, s.Routes("test"), chatBody("coding-cheap")); rr.Code != http.StatusOK {
		t.Fatalf("second call status = %d", rr.Code)
	}

	if calls := minter.mintCalls(); len(calls) != 1 {
		t.Fatalf("mint calls = %d, want 1 (key cached per task)", len(calls))
	}
	if spent, ok := store.spentOf("42"); !ok || spent != 2.75 {
		t.Fatalf("spent = (%v, %v), want (2.75, true): 2 + 0.5 + 0.25", spent, ok)
	}
	auths, _, _ := stub.seen()
	if len(auths) != 2 || auths[0] != "Bearer sk-task-key-1" || auths[1] != "Bearer sk-task-key-1" {
		t.Fatalf("upstream auths = %v, want the same cached key twice", auths)
	}
	audits := store.auditRows()
	if n := len(audits); n != 2 || *audits[1].SpendEUR != 2.75 {
		t.Fatalf("audits = %+v, want 2 rows, the last at spend 2.75", audits)
	}
}

// factoryd blocks the task's keys at every reconcile, so a key the gateway
// cached can die between two calls on the same run, policy version and UTC
// day. LiteLLM says so ("Key is blocked"): the gateway drops the slot's
// cached key, mints a new one and retries the call once.
func TestBlockedKeyIsReMinted(t *testing.T) {
	const blockedBody = `{"error":{"message":"Authentication Error, Key is blocked. Update via ` + "`/key/unblock`" + ` if you are an admin.","code":"401"}}`
	var mu sync.Mutex
	hits := map[string]int{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		hits[auth]++
		n := hits[auth]
		mu.Unlock()
		// the first call succeeds on key 1; the reconcile that follows
		// blocks it, so the second attempt with it answers 401
		if auth == "sk-task-key-1" && n > 1 {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, blockedBody)
			return
		}
		w.Header().Set("x-litellm-response-cost", "0.1")
		io.WriteString(w, `{}`)
	}))
	defer up.Close()

	store := newMemStore(Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, Status: "running"})
	minter := &fakeMinter{}
	s := NewServer(ServerOptions{
		Auth:       fakeAuth{id: Identity{Subject: "system:serviceaccount:factory:task-42", Namespace: "factory", TaskID: "42", SAUID: "sa-uid-1"}},
		Store:      store,
		Minter:     minter,
		LiteLLMURL: up.URL,
		Prices:     testPrices,
	})
	h := s.Routes("test")

	if rr := doChat(t, h, chatBody("coding-cheap")); rr.Code != http.StatusOK {
		t.Fatalf("first call = %d: %s", rr.Code, rr.Body)
	}
	if rr := doChat(t, h, chatBody("coding-cheap")); rr.Code != http.StatusOK {
		t.Fatalf("call after the block = %d: %s, want the gateway to re-mint and retry", rr.Code, rr.Body)
	}

	if calls := minter.mintCalls(); len(calls) != 2 {
		t.Fatalf("mint calls = %+v, want a second key after the block", calls)
	}
	mu.Lock()
	got := map[string]int{}
	for k, v := range hits {
		got[k] = v
	}
	mu.Unlock()
	if got["sk-task-key-1"] != 2 || got["sk-task-key-2"] != 1 {
		t.Fatalf("upstream hits = %v, want key 1 twice (the blocked answer) then the re-minted key 2 once", got)
	}
	if spent, ok := store.spentOf("42"); !ok || spent != 0.2 {
		t.Fatalf("spent = (%v, %v), want 0.2: the blocked answer booked nothing", spent, ok)
	}
	audits := store.auditRows()
	if len(audits) != 2 || audits[0].Decision != "allowed" || audits[1].Decision != "allowed" {
		t.Fatalf("audits = %+v, want two allowed rows", audits)
	}
}

// A requeued task runs under a recreated service account; its old key was
// blocked at requeue, so the gateway must mint a fresh one capped at what is
// left of the budget.
// A slow mint for one task must not hold up another task's calls, and
// concurrent calls of one run share a single mint.
func TestKeyMintsDoNotSerializeTasks(t *testing.T) {
	stub := newStubUpstream(t)
	stub.setResp(stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-response-cost": "0.5"}})
	store := newMemStore(
		Task{Classes: coder, ModelAlias: "coding-cheap", ID: "slow", BudgetEUR: 10, Status: "running"},
		Task{Classes: coder, ModelAlias: "coding-cheap", ID: "fast", BudgetEUR: 10, Status: "running"},
	)
	release := make(chan struct{})
	minter := &fakeMinter{hold: map[string]chan struct{}{"slow": release}}
	auth := authByTask{}
	s := NewServer(ServerOptions{Auth: auth, Store: store, Minter: minter, LiteLLMURL: stub.URL, Prices: testPrices})
	h := s.Routes("test")

	slowDone := make(chan int, 3)
	for range 3 {
		go func() {
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatBody("coding-cheap")))
			r.Header.Set("X-Task", "slow")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
			slowDone <- rr.Code
		}()
	}
	for len(minter.mintCalls()) == 0 {
		time.Sleep(time.Millisecond)
	}

	fast := make(chan int, 1)
	go func() {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatBody("coding-cheap")))
		r.Header.Set("X-Task", "fast")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		fast <- rr.Code
	}()
	select {
	case code := <-fast:
		if code != http.StatusOK {
			t.Fatalf("fast task status = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fast task waited on the slow task's mint")
	}

	close(release)
	for range 3 {
		if code := <-slowDone; code != http.StatusOK {
			t.Fatalf("slow task status = %d", code)
		}
	}
	slowMints := 0
	for _, c := range minter.mintCalls() {
		if c.taskID == "slow" {
			slowMints++
		}
	}
	if slowMints != 1 {
		t.Fatalf("slow task mints = %d, want 1 shared by its 3 calls", slowMints)
	}
}

// authByTask maps the X-Task header to a task identity with a fixed SA UID.
type authByTask struct{}

func (authByTask) Authenticate(r *http.Request) (Identity, error) {
	id := r.Header.Get("X-Task")
	return Identity{Subject: "system:serviceaccount:factory:task-" + id, Namespace: "factory", TaskID: id, SAUID: "uid-" + id}, nil
}

func TestUsageScannerBoundsItsBuffer(t *testing.T) {
	u := &usageScanner{}
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for range 40 { // 2.5 MiB of one line
		u.Write(chunk)
		if len(u.pending) > maxSSELine {
			t.Fatalf("pending = %d bytes, want at most %d", len(u.pending), maxSSELine)
		}
	}
	u.Write([]byte("\ndata: {\"usage\":{\"cost\":0.25}}\n"))
	if u.usage.Cost == nil || *u.usage.Cost != 0.25 {
		t.Fatalf("cost = %v, want the usage after the long line: 0.25", u.usage.Cost)
	}
}

func TestKeyRemintedForNewServiceAccount(t *testing.T) {
	stub := newStubUpstream(t)
	store := newMemStore(Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"})
	minter := &fakeMinter{}
	id := Identity{Subject: "system:serviceaccount:factory:task-42", Namespace: "factory", TaskID: "42", SAUID: "sa-uid-1"}
	s := NewServer(ServerOptions{Auth: fakeAuth{id: id}, Store: store, Minter: minter, LiteLLMURL: stub.URL, Prices: testPrices})
	stub.setResp(stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-response-cost": "0.5"}})

	if rr := doChat(t, s.Routes("test"), chatBody("coding-cheap")); rr.Code != http.StatusOK {
		t.Fatalf("first run status = %d", rr.Code)
	}
	id.SAUID = "sa-uid-2"
	s.opts.Auth = fakeAuth{id: id}
	if rr := doChat(t, s.Routes("test"), chatBody("coding-cheap")); rr.Code != http.StatusOK {
		t.Fatalf("second run status = %d", rr.Code)
	}

	calls := minter.mintCalls()
	if len(calls) != 2 || calls[0].maxBudget != 8 || calls[1].maxBudget != 7.5 {
		t.Fatalf("mint calls = %+v, want budgets 8 (10-2) then 7.5 (10-2.5)", calls)
	}
	auths, _, _ := stub.seen()
	if len(auths) != 2 || auths[0] != "Bearer sk-task-key-1" || auths[1] != "Bearer sk-task-key-2" {
		t.Fatalf("upstream auths = %v, want the first key then the re-minted one", auths)
	}
}

func TestBodyLimit(t *testing.T) {
	old := maxBodyBytes
	maxBodyBytes = 16
	t.Cleanup(func() { maxBodyBytes = old })

	stub := newStubUpstream(t)
	store := newMemStore(Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, Status: "running"})
	s := NewServer(ServerOptions{
		Auth:       fakeAuth{id: Identity{TaskID: "42"}},
		Store:      store,
		Minter:     &fakeMinter{},
		LiteLLMURL: stub.URL,
		Prices:     testPrices,
	})

	rr := doChat(t, s.Routes("test"), strings.Repeat("x", 32))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rr.Code)
	}
	audits := store.auditRows()
	if len(audits) != 1 || audits[0].Decision != "refused_body" {
		t.Fatalf("audits = %+v, want one refused_body", audits)
	}
	if auths, _, _ := stub.seen(); len(auths) != 0 {
		t.Fatalf("upstream hit %d times, want 0", len(auths))
	}
}

func TestRoutes(t *testing.T) {
	s := NewServer(ServerOptions{
		Auth:       fakeAuth{id: Identity{TaskID: "42"}},
		Store:      newMemStore(),
		Minter:     &fakeMinter{},
		LiteLLMURL: "http://127.0.0.1:1",
		Prices:     testPrices,
	})
	h := s.Routes("test")

	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest("GET", "/healthz", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200 (reuses internal/health)", res.Code)
	}
	var body struct {
		Service string `json:"service"`
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Service != "factory-gateway" || body.Status != "ok" || body.Version != "test" {
		t.Fatalf("healthz body = %+v", body)
	}

	res = httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest("GET", "/v1/chat/completions", nil))
	if res.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", res.Code)
	}

	res = httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest("POST", "/healthz", nil))
	if res.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST healthz status = %d, want 405", res.Code)
	}

	res = httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest("GET", "/nope", nil))
	if res.Code != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", res.Code)
	}
}

func ptr(f float64) *float64 { return &f }

// An agent calls only its session's alias (tasks.model_alias): on a task
// whose session runs on coding-cheap, coding-frontier, which its class
// lists, is refused 403 naming both aliases before any hold or upstream
// call, and coding-cheap goes through. A factoryd call keeps its purpose's
// alias (TestFactorydReviewCall).
func TestAgentCallsOnlyItsSessionAlias(t *testing.T) {
	stub := newStubUpstream(t)
	stub.setResp(stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-response-cost": "0.125"}})
	store := newMemStore(Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, Status: "running"})
	s := NewServer(ServerOptions{Auth: fakeAuth{id: Identity{TaskID: "42", SAUID: "sa"}}, Store: store, Minter: &fakeMinter{}, LiteLLMURL: stub.URL, Prices: testPrices})
	rr := doChat(t, s.Routes("test"), chatBody("coding-frontier"))
	_, bodies, _ := stub.seen()
	want := `model "coding-frontier" is not the model "coding-cheap" of the task's session (tasks.model_alias); a stronger model comes only by the climb`
	if a := store.auditRows(); rr.Code != http.StatusForbidden || len(a) != 1 || a[0].Decision != "refused_capability" || a[0].Reason != want ||
		len(store.reqs) != 0 || len(bodies) != 0 {
		t.Fatalf("coding-frontier = %d %s, audit %+v, %d holds, %d upstream calls; want 403 refused_capability: %s", rr.Code, rr.Body.String(), a, len(store.reqs), len(bodies), want)
	}
	if rr := doChat(t, s.Routes("test"), chatBody("coding-cheap")); rr.Code != http.StatusOK {
		t.Fatalf("coding-cheap = %d %s, want 200 through LiteLLM", rr.Code, rr.Body.String())
	}
}

// A settled call books LiteLLM's cost with the provider's usage and the
// bytes of the body sent upstream: a stream's usage from its final chunk,
// any other call's from its body, its cost from the header.
func TestSettledCallRecordsItsUsage(t *testing.T) {
	stub := newStubUpstream(t)
	store := newMemStore(Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, Status: "running"})
	s := NewServer(ServerOptions{Auth: fakeAuth{id: Identity{TaskID: "42", SAUID: "sa"}}, Store: store, Minter: &fakeMinter{}, LiteLLMURL: stub.URL, Prices: testPrices})
	stub.setResp(stubResp{status: http.StatusOK, body: sseBody, headers: map[string]string{"Content-Type": "text/event-stream"}})
	doChat(t, s.Routes("test"), `{"model":"coding-cheap","stream":true,"messages":[]}`)
	stub.setResp(stubResp{status: http.StatusOK, headers: map[string]string{"x-litellm-response-cost": "0.25"},
		body: `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}}`})
	doChat(t, s.Routes("test"), chatBody("coding-cheap"))
	_, bodies, _ := stub.seen()
	if len(bodies) != 2 || !reflect.DeepEqual(store.ledger, []ledgerRow{{"42", "coding-cheap", 0.125, "model"}, {"42", "coding-cheap", 0.25, "model"}}) ||
		!reflect.DeepEqual(store.usages, []policy.Usage{{PromptTokens: 3, OutputTokens: 1, BodyBytes: len(bodies[0])}, {PromptTokens: 10, CachedTokens: 4, OutputTokens: 2, BodyBytes: len(bodies[1])}}) {
		t.Fatalf("ledger = %+v with usages %+v", store.ledger, store.usages)
	}
}

const sseBody = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{}}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"cost\":0.125}}\n\n" +
	"data: [DONE]\n\n"

// Two tasks, each within its own budget, together exhaust the day: the day
// cap binds the key of the second and refuses the next call of either.
func TestDayCapAcrossTasks(t *testing.T) {
	stub := newStubUpstream(t)
	store := newMemStore(
		Task{Classes: coder, ModelAlias: "coding-cheap", ID: "a", BudgetEUR: 5, Status: "running"},
		Task{Classes: coder, ModelAlias: "coding-cheap", ID: "b", BudgetEUR: 5, Status: "running"},
	)
	store.setPolicy(testPolicy(3, 1))
	minter := &fakeMinter{}
	s := NewServer(ServerOptions{Auth: authByTask{}, Store: store, Minter: minter, LiteLLMURL: stub.URL, Prices: testPrices})
	call := func(task, cost string) *httptest.ResponseRecorder {
		stub.setResp(stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-response-cost": cost}})
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatBody("coding-cheap")))
		r.Header.Set("X-Task", task)
		rr := httptest.NewRecorder()
		s.Routes("test").ServeHTTP(rr, r)
		return rr
	}

	if rr := call("a", "0.625"); rr.Code != http.StatusOK {
		t.Fatalf("a's first call = %d, want 200", rr.Code)
	}
	if rr := call("b", "0.5"); rr.Code != http.StatusOK {
		t.Fatalf("b's first call = %d, want 200", rr.Code)
	}
	// a minted at min(5, 1), b at min(5, 1 - 0.625)
	if calls := minter.mintCalls(); len(calls) != 2 || calls[0] != (mintCall{"a", 1, "coding-cheap,coding-frontier"}) || calls[1] != (mintCall{"b", 0.375, "coding-cheap,coding-frontier"}) {
		t.Fatalf("mint calls = %+v, want a at 1, b at 0.375", calls)
	}
	for _, task := range []string{"a", "b"} {
		rr := call(task, "0.1")
		if rr.Code != http.StatusPaymentRequired || !strings.Contains(rr.Body.String(), "day -0.125000") {
			t.Fatalf("%s after the day's 1.125 = %d %s, want 402 naming the day's headroom", task, rr.Code, rr.Body.String())
		}
	}
	// both calls report more than their 0.131204 EUR holds
	audits := store.auditRows()
	for i, want := range []string{"allowed", "allowed", "refused_day_cap", "refused_day_cap"} {
		if audits[i].Decision != want || (i < 2) != (audits[i].OverHold != "") || audits[i].PolicyVersion == nil || *audits[i].PolicyVersion != 3 {
			t.Fatalf("audit %d = %+v, want %s under policy version 3", i, audits[i], want)
		}
	}
	if len(store.ledger) != 2 || store.ledger[0] != (ledgerRow{"a", "coding-cheap", 0.625, "model"}) || store.ledger[1] != (ledgerRow{"b", "coding-cheap", 0.5, "model"}) {
		t.Fatalf("ledger = %+v, want one model row per allowed call", store.ledger)
	}
}

// Two tasks of linkshort, whose file caps the project at 1 EUR a month,
// together spend it: the cap binds both keys and refuses the next call of
// either with the file to raise it in; a task of a project without a cap
// keeps its own headroom.
func TestProjectCapAcrossTasks(t *testing.T) {
	stub := newStubUpstream(t)
	store := newMemStore(
		Task{Classes: coder, ModelAlias: "coding-cheap", ID: "a", BudgetEUR: 5, Status: "running", Repository: "linkshort/linkshort"},
		Task{Classes: coder, ModelAlias: "coding-cheap", ID: "b", BudgetEUR: 5, Status: "running", Repository: "linkshort/linkshort"},
		Task{Classes: coder, ModelAlias: "coding-cheap", ID: "c", BudgetEUR: 5, Status: "running", Repository: "notes/notes"},
	)
	pv := testPolicy(3, 20)
	pv.Projects = map[string]policy.ProjectConfig{
		"linkshort": {Repository: "linkshort/linkshort", Budget: &policy.ProjectBudget{MonthEUR: 1}},
		"notes":     {Repository: "notes/notes"},
	}
	store.setPolicy(pv)
	minter := &fakeMinter{}
	s := NewServer(ServerOptions{Auth: authByTask{}, Store: store, Minter: minter, LiteLLMURL: stub.URL, Prices: testPrices})
	call := func(task, cost string) *httptest.ResponseRecorder {
		stub.setResp(stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-response-cost": cost}})
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatBody("coding-cheap")))
		r.Header.Set("X-Task", task)
		rr := httptest.NewRecorder()
		s.Routes("test").ServeHTTP(rr, r)
		return rr
	}
	if rr := call("a", "0.625"); rr.Code != http.StatusOK {
		t.Fatalf("a's first call = %d, want 200", rr.Code)
	}
	if rr := call("b", "0.5"); rr.Code != http.StatusOK {
		t.Fatalf("b's first call = %d, want 200", rr.Code)
	}
	for _, task := range []string{"a", "b"} {
		rr := call(task, "0.1")
		if rr.Code != http.StatusPaymentRequired || rr.Header().Get(HeaderDecision) != policy.RefusedProjectCap ||
			!strings.Contains(rr.Body.String(), "project -0.125000) fits 0 output tokens") ||
			!strings.Contains(rr.Body.String(), "; the project cap budget.month_eur in projects/linkshort.yaml is spent, and only a human raises it, by a commit") {
			t.Fatalf("%s after the project's 1.125 = %d %s %s, want 402 refused_project_cap naming projects/linkshort.yaml", task, rr.Code, rr.Header().Get(HeaderDecision), rr.Body.String())
		}
	}
	if rr := call("c", "0.1"); rr.Code != http.StatusOK {
		t.Fatalf("c's call = %d %s, want 200: notes has no cap", rr.Code, rr.Body.String())
	}
	// a minted at min(5, 20, 50, 1), b at the project's 1 - 0.625, c at its
	// task's 5
	if calls := minter.mintCalls(); len(calls) != 3 || calls[0] != (mintCall{"a", 1, "coding-cheap,coding-frontier"}) ||
		calls[1] != (mintCall{"b", 0.375, "coding-cheap,coding-frontier"}) || calls[2] != (mintCall{"c", 5, "coding-cheap,coding-frontier"}) {
		t.Fatalf("mint calls = %+v, want a at 1, b at 0.375, c at 5", calls)
	}
	audits := store.auditRows()
	for i, want := range []string{"allowed", "allowed", policy.RefusedProjectCap, policy.RefusedProjectCap, "allowed"} {
		if audits[i].Decision != want {
			t.Fatalf("audit %d = %+v, want %s", i, audits[i], want)
		}
	}
	if r := store.reqs[0]; r.Project != "linkshort" || r.ProjectEUR != 1 {
		t.Fatalf("a's hold request = %+v, want linkshort's cap 1", r)
	}
	if r := store.reqs[len(store.reqs)-1]; r.TaskID != "c" || r.Project != "notes" || !math.IsInf(r.ProjectEUR, 1) {
		t.Fatalf("c's hold request = %+v, want notes without a cap", r)
	}
}

// A new policy version is in force within policyTTL, and the store is read
// at most once per TTL.
func TestPolicyCachedForAtMost30s(t *testing.T) {
	stub := newStubUpstream(t)
	stub.setResp(stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-response-cost": "0"}})
	store := newMemStore(Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, Status: "running"})
	s := NewServer(ServerOptions{Auth: fakeAuth{id: Identity{TaskID: "42", SAUID: "u"}}, Store: store, Minter: &fakeMinter{}, LiteLLMURL: stub.URL, Prices: testPrices})
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	doChat(t, s.Routes("test"), chatBody("coding-cheap"))
	next := testPolicy(2, 20)
	next.Document.Classes = map[string]policy.Class{"coder": {Models: []string{"coding-frontier"}}}
	store.setPolicy(next)
	now = now.Add(29 * time.Second)
	if rr := doChat(t, s.Routes("test"), chatBody("coding-cheap")); rr.Code != http.StatusOK {
		t.Fatalf("within the TTL = %d, want 200 under version 1", rr.Code)
	}
	now = now.Add(time.Second)
	if rr := doChat(t, s.Routes("test"), chatBody("coding-cheap")); rr.Code != http.StatusForbidden {
		t.Fatalf("after the TTL = %d, want 403 under version 2", rr.Code)
	}
	var versions []int64
	for _, a := range store.auditRows() {
		versions = append(versions, *a.PolicyVersion)
	}
	if fmt.Sprint(versions) != "[1 1 2]" || store.polReads != 2 {
		t.Fatalf("audited versions %v with %d store reads, want [1 1 2] with 2", versions, store.polReads)
	}
}

// withMaxTokens is body as the gateway forwards it when the caller set no
// output limit and the headroom allows limit tokens.
func withMaxTokens(t *testing.T, body string, limit int) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	m["max_tokens"] = json.RawMessage(fmt.Sprint(limit))
	b, _ := json.Marshal(m)
	return string(b)
}

// A key is minted for one run, policy version and UTC day, limited to the
// classes' aliases; a new version or day mints a new key and blocks the old.
func TestKeyRemintedOnPolicyVersionAndDay(t *testing.T) {
	stub := newStubUpstream(t)
	stub.setResp(stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-response-cost": "0.5"}})
	store := newMemStore(Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"})
	minter := &fakeMinter{}
	s := NewServer(ServerOptions{Auth: fakeAuth{id: Identity{TaskID: "42", SAUID: "u"}}, Store: store, Minter: minter, LiteLLMURL: stub.URL, Prices: testPrices})
	now := time.Date(2026, 9, 27, 23, 59, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	call := func() {
		t.Helper()
		if rr := doChat(t, s.Routes("test"), chatBody("coding-cheap")); rr.Code != http.StatusOK {
			t.Fatalf("status = %d %s", rr.Code, rr.Body.String())
		}
	}

	call()                         // version 1, 2026-09-27: headroom 8
	call()                         // cached
	now = now.Add(2 * time.Minute) // UTC midnight passed
	call()                         // headroom 7
	next := testPolicy(2, 20)
	next.Document.Classes["coder"] = policy.Class{Models: []string{"coding-cheap"}}
	store.setPolicy(next)
	now = now.Add(policyTTL)
	call() // version 2: headroom 6.5, only coding-cheap

	want := []mintCall{
		{"42", 8, "coding-cheap,coding-frontier"},
		{"42", 7, "coding-cheap,coding-frontier"},
		{"42", 6.5, "coding-cheap"},
	}
	if calls := minter.mintCalls(); !reflect.DeepEqual(calls, want) {
		t.Fatalf("mints = %+v, want %+v", calls, want)
	}
	if blocked := minter.blockedKeys(); !reflect.DeepEqual(blocked, []string{"sk-task-key-1", "sk-task-key-2"}) {
		t.Fatalf("blocked = %v, want the two superseded keys", blocked)
	}
}

// Two concurrent calls, headroom for one: one gets through, the other is
// refused while the first holds the headroom.
func TestConcurrentCallsCannotShareTheLastHeadroom(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.Header().Set("x-litellm-response-cost", "0.1")
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	// 0.135 free: the first call holds 132e-6 + 32768 * 4e-6 = 0.131204,
	// leaving 0.003796, below 1024 output tokens
	store := newMemStore(Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 0.135, Status: "running"})
	s := NewServer(ServerOptions{Auth: fakeAuth{id: Identity{TaskID: "42", SAUID: "u"}}, Store: store, Minter: &fakeMinter{}, LiteLLMURL: up.URL, Prices: testPrices})

	codes := make(chan int, 2)
	go func() { codes <- doChat(t, s.Routes("test"), chatBody("coding-cheap")).Code }()
	<-started
	codes <- doChat(t, s.Routes("test"), chatBody("coding-cheap")).Code
	close(release)
	got := []int{<-codes, <-codes}
	if got[0] != http.StatusPaymentRequired || got[1] != http.StatusOK {
		t.Fatalf("codes = %v, want the second call refused 402 while the first holds, then the first 200", got)
	}
	if spent, _ := store.spentOf("42"); spent != 0.1 {
		t.Fatalf("spent = %v, want the first call's 0.1", spent)
	}
	audits := store.auditRows()
	last := audits[len(audits)-1]
	if last.Decision != "allowed" || *last.OutputLimit != 32768 || math.Abs(*last.HoldEUR-0.131204) > 1e-12 {
		t.Fatalf("allowed audit = %+v, want output limit 32768 and hold 0.131204", last)
	}
}

// A call still using a superseded key finishes on it: the key is blocked
// only when its last call is done.
func TestSupersededKeyBlockedAfterItsCalls(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var mu sync.Mutex
	first := true
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		wait := first
		first = false
		mu.Unlock()
		if wait {
			started <- struct{}{}
			<-release
		}
		w.Header().Set("x-litellm-response-cost", "0.01")
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	store := newMemStore(Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, Status: "running"})
	minter := &fakeMinter{}
	s := NewServer(ServerOptions{Auth: fakeAuth{id: Identity{TaskID: "42", SAUID: "u"}}, Store: store, Minter: minter, LiteLLMURL: up.URL, Prices: testPrices})
	now := time.Date(2026, 9, 27, 23, 59, 0, 0, time.UTC)
	var clock sync.Mutex
	s.now = func() time.Time { clock.Lock(); defer clock.Unlock(); return now }

	done := make(chan int, 1)
	go func() { done <- doChat(t, s.Routes("test"), chatBody("coding-cheap")).Code }()
	<-started // the first call runs on key 1
	clock.Lock()
	now = now.Add(2 * time.Minute) // UTC midnight: key 2
	clock.Unlock()
	if rr := doChat(t, s.Routes("test"), chatBody("coding-cheap")); rr.Code != http.StatusOK {
		t.Fatalf("call on the new day = %d", rr.Code)
	}
	if b := minter.blockedKeys(); len(b) != 0 {
		t.Fatalf("blocked %v while a call still runs on key 1, want none", b)
	}
	close(release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("the call on the superseded key = %d, want 200", code)
	}
	if b := minter.blockedKeys(); !reflect.DeepEqual(b, []string{"sk-task-key-1"}) {
		t.Fatalf("blocked = %v, want key 1 once its call is done", b)
	}
}

const testFactorydToken = "factoryd-shared-token"

func doFactorydChat(t *testing.T, h http.Handler, token, task, purpose, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set(HeaderFactoryTask, task)
	r.Header.Set(HeaderFactoryPurpose, purpose)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

func factoryPolicy() policy.Version {
	v := testPolicy(1, 20)
	v.Document.Classes["coder"] = policy.Class{Models: []string{"coding-cheap", "coding-frontier"},
		ReviewModel: "review-frontier", SupervisorModel: "supervisor", AuditModel: "coding-frontier"}
	return v
}

// factoryd's review call on a task in review: the gateway-factoryd token
// and the task, the policy's review alias, the hold and the cost booked
// to the task under ref review/review-frontier, an audit row of kind
// review; the agent's JWT path never reaches the alias.
func TestFactorydReviewCall(t *testing.T) {
	prices := fakePrices{"coding-cheap": testPrice, "coding-frontier": testPrice, "review-frontier": testPrice, "supervisor": testPrice}
	type want struct {
		status   int
		decision string
		header   string // X-Gateway-Decision
		upstream int
		ledger   []ledgerRow
		spent    float64
		// deployment is the answer's deployment header
		deployment string
	}
	for name, tc := range map[string]struct {
		task    Task
		token   string
		purpose string
		model   string
		resp    stubResp
		want    want
		// authKind is a refused_auth row's kind; noToken runs the gateway
		// without a FactorydToken
		authKind string
		noToken  bool
	}{
		"authorised": {
			task: Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "in-review"}, token: testFactorydToken,
			purpose: "review", model: "review-frontier",
			resp: stubResp{status: 200, body: `{"choices":[]}`, headers: map[string]string{"x-litellm-response-cost": "0.125"}},
			want: want{status: 200, decision: "allowed", upstream: 1,
				ledger: []ledgerRow{{"42", "review/review-frontier", 0.125, policy.KindModel}}, spent: 2.125},
		},
		// the audit (ruling 188) learns the deployment that answered
		"audit": {
			task: Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "in-review"}, token: testFactorydToken,
			purpose: "audit", model: "coding-frontier",
			resp: stubResp{status: 200, body: `{"choices":[]}`, headers: map[string]string{"x-litellm-response-cost": "0.125", "x-litellm-model-name": "zai/glm-5.3"}},
			want: want{status: 200, decision: "allowed", upstream: 1, deployment: "zai/glm-5.3",
				ledger: []ledgerRow{{"42", "audit/coding-frontier", 0.125, policy.KindModel}}, spent: 2.125},
		},
		"the audit model for the review": {
			task: Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "in-review"}, token: testFactorydToken,
			purpose: "review", model: "coding-frontier",
			want: want{status: 403, decision: "refused_capability", header: "refused_capability", spent: 2},
		},
		"at the budget cap": {
			task: Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 2, SpentEUR: 2, Status: "in-review"}, token: testFactorydToken,
			purpose: "review", model: "review-frontier",
			want: want{status: 402, decision: "refused_budget", header: "refused_budget", spent: 2},
		},
		"upstream error": {
			task: Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "in-review"}, token: testFactorydToken,
			purpose: "review", model: "review-frontier",
			resp: stubResp{status: 500, body: `{"error":"provider down"}`},
			want: want{status: 500, decision: "error_upstream", upstream: 1, spent: 2},
		},
		"not the review alias": {
			task: Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "in-review"}, token: testFactorydToken,
			purpose: "review", model: "supervisor",
			want: want{status: 403, decision: "refused_capability", header: "refused_capability", spent: 2},
		},
		"a finished task": {
			task: Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "done"}, token: testFactorydToken,
			purpose: "review", model: "review-frontier",
			want: want{status: 402, decision: "refused_task_status", header: "refused_task_status", spent: 2},
		},
		"unknown purpose": {
			task: Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "in-review"}, token: testFactorydToken,
			purpose: "session", model: "review-frontier", authKind: "factoryd",
			want: want{status: 401, decision: "refused_auth", header: "refused_auth", spent: 2},
		},
		// the headers without the token are an agent's call: its JWT is
		// checked, and fails here
		"a wrong token with the factoryd headers": {
			task: Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "in-review"}, token: "not-the-token",
			purpose: "review", model: "review-frontier",
			want: want{status: 401, decision: "refused_auth", header: "refused_auth", spent: 2},
		},
		"no factoryd token configured": {
			task: Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "in-review"}, token: testFactorydToken,
			purpose: "review", model: "review-frontier", noToken: true,
			want: want{status: 401, decision: "refused_auth", header: "refused_auth", spent: 2},
		},
	} {
		t.Run(name, func(t *testing.T) {
			stub := newStubUpstream(t)
			stub.setResp(tc.resp)
			store := newMemStore(tc.task)
			store.setPolicy(factoryPolicy())
			minter := &fakeMinter{}
			token := testFactorydToken
			if tc.noToken {
				token = ""
			}
			s := NewServer(ServerOptions{Auth: fakeAuth{err: errors.New("not a JWT")}, Store: store, Minter: minter,
				LiteLLMURL: stub.URL, Prices: prices, FactorydToken: token})
			rr := doFactorydChat(t, s.Routes("test"), tc.token, "42", tc.purpose, chatBody(tc.model))
			if rr.Header().Get(HeaderDeployment) != tc.want.deployment {
				t.Fatalf("deployment header %q, want %q", rr.Header().Get(HeaderDeployment), tc.want.deployment)
			}
			if rr.Code != tc.want.status || rr.Header().Get(HeaderDecision) != tc.want.header {
				t.Fatalf("status %d decision header %q, want %d %q: %s", rr.Code, rr.Header().Get(HeaderDecision), tc.want.status, tc.want.header, rr.Body)
			}
			// a task header behind no token names no task
			wantTask := "42"
			if tc.want.decision == "refused_auth" && tc.authKind == "" {
				wantTask = ""
			}
			audits := store.auditRows()
			if len(audits) != 1 || audits[0].Decision != tc.want.decision || audits[0].TaskID != wantTask {
				t.Fatalf("audits = %+v, want one %s row on task %q", audits, tc.want.decision, wantTask)
			}
			wantKind := tc.purpose
			if tc.want.decision == "refused_auth" {
				wantKind = tc.authKind
			}
			if audits[0].Kind != wantKind {
				t.Fatalf("audit kind = %q, want %q", audits[0].Kind, wantKind)
			}
			if tc.want.decision != "refused_auth" && audits[0].ModelAlias != tc.model {
				t.Fatalf("audit alias = %q, want %q", audits[0].ModelAlias, tc.model)
			}
			if tc.want.decision == "allowed" && (audits[0].SpendEUR == nil || *audits[0].SpendEUR != tc.want.spent) {
				t.Fatalf("audit spend = %v, want %v", audits[0].SpendEUR, tc.want.spent)
			}
			auths, _, _ := stub.seen()
			if len(auths) != tc.want.upstream {
				t.Fatalf("upstream calls = %d, want %d", len(auths), tc.want.upstream)
			}
			if tc.want.upstream > 0 {
				if calls := minter.mintCalls(); len(calls) != 1 || calls[0].models != "coding-frontier,review-frontier,supervisor" || calls[0].taskID != "42" {
					t.Fatalf("mints = %+v, want one key for task 42 on the factory aliases only", calls)
				}
			}
			if !reflect.DeepEqual(store.ledger, tc.want.ledger) {
				t.Fatalf("ledger = %+v, want %+v", store.ledger, tc.want.ledger)
			}
			if got := store.tasks["42"].SpentEUR; got != tc.want.spent {
				t.Fatalf("spent = %v, want %v", got, tc.want.spent)
			}
			if store.openHolds() != 0 {
				t.Fatal("a hold is left open")
			}
		})
	}
}

// The project agent's call (purpose operator) and the pulse's (purpose
// pulse) are a chat task's: each runs on its alias, books under the task,
// and stops at its budget; an agent task's account takes neither.
func TestOperatorCallOnTheChatTask(t *testing.T) {
	prices := fakePrices{"operator": testPrice, "pulse": testPrice, "coding-frontier": testPrice, "review-frontier": testPrice, "supervisor": testPrice}
	chat := []string{policy.ClassChat}
	for name, tc := range map[string]struct {
		task   Task
		use    string // the purpose, "" for operator
		status int
		header string
		ledger []ledgerRow
	}{
		"the chat task": {task: Task{Classes: chat, ID: "t-chat-x-2026-10", BudgetEUR: 5, Status: "running"}, status: 200,
			ledger: []ledgerRow{{"t-chat-x-2026-10", "operator/operator", 0.125, policy.KindModel}}},
		"the pulse": {task: Task{Classes: chat, ID: "t-chat-x-2026-10", BudgetEUR: 5, Status: "running"}, use: policy.PurposePulse, status: 200,
			ledger: []ledgerRow{{"t-chat-x-2026-10", "pulse/pulse", 0.125, policy.KindModel}}},
		"the pulse on an agent task": {task: Task{Classes: coder, ModelAlias: "coding-cheap", ID: "t-chat-x-2026-10", BudgetEUR: 5, Status: "running"}, use: policy.PurposePulse,
			status: 403, header: "refused_capability"},
		"its budget spent": {task: Task{Classes: chat, ID: "t-chat-x-2026-10", BudgetEUR: 5, SpentEUR: 5, Status: "running"},
			status: 402, header: "refused_budget"},
		"an agent task": {task: Task{Classes: coder, ModelAlias: "coding-cheap", ID: "t-chat-x-2026-10", BudgetEUR: 5, Status: "running"},
			status: 403, header: "refused_capability"},
	} {
		t.Run(name, func(t *testing.T) {
			stub := newStubUpstream(t)
			stub.setResp(stubResp{status: 200, body: `{"choices":[]}`, headers: map[string]string{"x-litellm-response-cost": "0.125"}})
			store := newMemStore(tc.task)
			store.setPolicy(factoryPolicy())
			minter := &fakeMinter{}
			s := NewServer(ServerOptions{Auth: fakeAuth{err: errors.New("not a JWT")}, Store: store, Minter: minter,
				LiteLLMURL: stub.URL, Prices: prices, FactorydToken: testFactorydToken})
			use := cmp.Or(tc.use, policy.PurposeOperator)
			rr := doFactorydChat(t, s.Routes("test"), testFactorydToken, tc.task.ID, use, chatBody(use))
			if rr.Code != tc.status || rr.Header().Get(HeaderDecision) != tc.header {
				t.Fatalf("status %d decision %q, want %d %q: %s", rr.Code, rr.Header().Get(HeaderDecision), tc.status, tc.header, rr.Body)
			}
			if !reflect.DeepEqual(store.ledger, tc.ledger) {
				t.Fatalf("ledger = %+v, want %+v", store.ledger, tc.ledger)
			}
			if calls := minter.mintCalls(); tc.status == 200 && (len(calls) != 1 || calls[0].models != "operator,pulse") {
				t.Fatalf("mints = %+v, want one key on the chat task's two aliases", calls)
			}
		})
	}
}

// The factoryd token is no agent's: a sandbox's JWT never reaches the
// review alias, and factoryd's key is its own, so a review or supervisor
// call during a session neither retires nor shares the session's key.
func TestFactorydKeyIsNotTheSessionKey(t *testing.T) {
	stub := newStubUpstream(t)
	stub.setResp(stubResp{status: 200, body: `{}`, headers: map[string]string{"x-litellm-response-cost": "0.01"}})
	store := newMemStore(Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"})
	store.setPolicy(factoryPolicy())
	minter := &fakeMinter{}
	s := NewServer(ServerOptions{
		Auth:  fakeAuth{id: Identity{Subject: "system:serviceaccount:factory:task-42", Namespace: "factory", TaskID: "42", SAUID: "sa-uid-1"}},
		Store: store, Minter: minter, LiteLLMURL: stub.URL, FactorydToken: testFactorydToken,
		Prices: fakePrices{"coding-cheap": testPrice, "review-frontier": testPrice, "supervisor": testPrice},
	})
	h := s.Routes("test")
	if rr := doChat(t, h, chatBody("review-frontier")); rr.Code != http.StatusForbidden {
		t.Fatalf("agent call of the review alias = %d, want 403", rr.Code)
	}
	// the agent's credential with factoryd's headers is still the agent's call
	if rr := doFactorydChat(t, h, "the-agents-jwt", "42", "review", chatBody("review-frontier")); rr.Code != http.StatusForbidden {
		t.Fatalf("agent call with the factoryd headers = %d, want 403", rr.Code)
	}
	if rr := doChat(t, h, chatBody("coding-cheap")); rr.Code != http.StatusOK {
		t.Fatalf("agent call = %d", rr.Code)
	}
	if rr := doFactorydChat(t, h, testFactorydToken, "42", "supervisor", chatBody("supervisor")); rr.Code != http.StatusOK {
		t.Fatalf("supervisor call = %d: %s", rr.Code, rr.Body)
	}
	if rr := doChat(t, h, chatBody("coding-cheap")); rr.Code != http.StatusOK {
		t.Fatalf("agent call = %d", rr.Code)
	}
	calls := minter.mintCalls()
	if len(calls) != 2 || calls[0].models != "coding-cheap,coding-frontier" || calls[1].models != "coding-frontier,review-frontier,supervisor" {
		t.Fatalf("mints = %+v, want the session's key and factoryd's", calls)
	}
	if b := minter.blockedKeys(); len(b) != 0 {
		t.Fatalf("blocked = %v, want none", b)
	}
	auths, _, _ := stub.seen()
	if !reflect.DeepEqual(auths, []string{"Bearer sk-task-key-1", "Bearer sk-task-key-2", "Bearer sk-task-key-1"}) {
		t.Fatalf("upstream auths = %v", auths)
	}
	var kinds []string
	for _, a := range store.auditRows() {
		kinds = append(kinds, a.Kind+" "+a.Decision)
	}
	if !reflect.DeepEqual(kinds, []string{" refused_capability", " refused_capability", " allowed", "supervisor allowed", " allowed"}) {
		t.Fatalf("audit kinds = %q", kinds)
	}
}

// A caller that disconnects mid-call is not an upstream fault: the gateway
// must answer 499 (not 502) and still book whatever hold the provider may
// have earned, so an alert on 5xx never counts a client's own cancellation
// (debug-gateway-5xx.md).

func TestChatClientCanceledDuringUpstreamAnswers499(t *testing.T) {
	started := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body) // #nosec G104 -- a real upstream reads the body too; net/http only notices a dropped connection once it does
		close(started)
		<-r.Context().Done() // the gateway drops the connection once its caller cancels
	}))
	defer up.Close()

	store := newMemStore(Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"})
	s := NewServer(ServerOptions{
		Auth:       fakeAuth{id: Identity{Subject: "system:serviceaccount:factory:task-42", Namespace: "factory", TaskID: "42", SAUID: "sa-uid-1"}},
		Store:      store,
		Minter:     &fakeMinter{},
		LiteLLMURL: up.URL,
		Prices:     testPrices,
	})

	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatBody("coding-cheap"))).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer forged-but-fakeAuth-ignores")
	rr := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		s.Routes("test").ServeHTTP(rr, r)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the fake litellm never received the call")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never returned after the client cancelled")
	}

	if rr.Code != 499 {
		t.Fatalf("status = %d, want 499", rr.Code)
	}
	if got := rr.Header().Get(HeaderDecision); got != "client_canceled" {
		t.Fatalf("decision header = %q, want client_canceled", got)
	}
	audits := store.auditRows()
	if last := audits[len(audits)-1]; last.Decision != "client_canceled" {
		t.Fatalf("audit decision = %q, want client_canceled (all rows %+v)", last.Decision, audits)
	}
	// the provider may have run part of the call: the hold is booked exactly
	// as a real upstream failure would, 132 input tokens at 1e-6 and 32768
	// output tokens at 4e-6
	if got, ok := store.spentOf("42"); !ok || got != 2.131204 {
		t.Fatalf("spent = (%v, %v), want (2.131204, true)", got, ok)
	}
	if n := store.openHolds(); n != 0 {
		t.Fatalf("%d holds left open, want every hold released", n)
	}
}

// The existing "litellm unreachable refuses 502" case in TestChatCompletions
// is the regression for this same site with the client still connected: a
// real transport failure must keep answering 502 error_upstream.

func TestKeyMintClientCanceledAnswers499(t *testing.T) {
	stub := newStubUpstream(t)
	hold := make(chan struct{})
	store := newMemStore(Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"})
	minter := &fakeMinter{hold: map[string]chan struct{}{"42": hold}, err: errors.New("litellm down")}
	s := NewServer(ServerOptions{
		Auth:       fakeAuth{id: Identity{Subject: "system:serviceaccount:factory:task-42", Namespace: "factory", TaskID: "42", SAUID: "sa-uid-1"}},
		Store:      store,
		Minter:     minter,
		LiteLLMURL: stub.URL,
		Prices:     testPrices,
	})

	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatBody("coding-cheap"))).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer forged-but-fakeAuth-ignores")
	rr := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		s.Routes("test").ServeHTTP(rr, r)
		close(done)
	}()

	for len(minter.mintCalls()) == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	close(hold)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never returned after the client cancelled")
	}

	if rr.Code != 499 {
		t.Fatalf("status = %d, want 499", rr.Code)
	}
	if got := rr.Header().Get(HeaderDecision); got != "client_canceled" {
		t.Fatalf("decision header = %q, want client_canceled", got)
	}
	if _, ok := store.spentOf("42"); ok {
		t.Fatal("spent updated, want untouched: no provider call was ever made")
	}
	if n := store.openHolds(); n != 0 {
		t.Fatalf("%d holds left open, want every hold released", n)
	}
	if auths, _, _ := stub.seen(); len(auths) != 0 {
		t.Fatalf("upstream hit %d times, want 0: the mint never finished", len(auths))
	}
}

// The existing "key mint failure refuses 502" case in TestChatCompletions is
// the regression for this site with the client still connected.

func TestRekeyClientCanceledAnswers499(t *testing.T) {
	const blockedBody = `{"error":{"message":"Authentication Error, Key is blocked. Update via ` + "`/key/unblock`" + ` if you are an admin.","code":"401"}}`
	var mu sync.Mutex
	blocked := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		b := blocked
		mu.Unlock()
		if b {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, blockedBody)
			return
		}
		w.Header().Set("x-litellm-response-cost", "0.1")
		io.WriteString(w, `{}`)
	}))
	defer up.Close()

	store := newMemStore(Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"})
	minter := &fakeMinter{}
	s := NewServer(ServerOptions{
		Auth:       fakeAuth{id: Identity{Subject: "system:serviceaccount:factory:task-42", Namespace: "factory", TaskID: "42", SAUID: "sa-uid-1"}},
		Store:      store,
		Minter:     minter,
		LiteLLMURL: up.URL,
		Prices:     testPrices,
	})
	h := s.Routes("test")

	// establish a cached key so the second call hits the blocked-key retry
	if rr := doChat(t, h, chatBody("coding-cheap")); rr.Code != http.StatusOK {
		t.Fatalf("first call = %d: %s", rr.Code, rr.Body)
	}

	mu.Lock()
	blocked = true
	mu.Unlock()
	hold := make(chan struct{})
	minter.hold = map[string]chan struct{}{"42": hold}
	minter.err = errors.New("litellm down")

	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatBody("coding-cheap"))).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer forged-but-fakeAuth-ignores")
	rr := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rr, r)
		close(done)
	}()

	for len(minter.mintCalls()) < 2 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	close(hold)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never returned after the client cancelled")
	}

	if rr.Code != 499 {
		t.Fatalf("status = %d, want 499", rr.Code)
	}
	if got := rr.Header().Get(HeaderDecision); got != "client_canceled" {
		t.Fatalf("decision header = %q, want client_canceled", got)
	}
	// the first call's cost booked; the blocked retry's hold booked nothing
	if spent, ok := store.spentOf("42"); !ok || spent != 2.1 {
		t.Fatalf("spent = (%v, %v), want (2.1, true)", spent, ok)
	}
	if n := store.openHolds(); n != 0 {
		t.Fatalf("%d holds left open, want every hold released", n)
	}
}

func TestRekeyFailureStays502WhenConnected(t *testing.T) {
	const blockedBody = `{"error":{"message":"Authentication Error, Key is blocked. Update via ` + "`/key/unblock`" + ` if you are an admin.","code":"401"}}`
	var mu sync.Mutex
	blocked := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		b := blocked
		mu.Unlock()
		if b {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, blockedBody)
			return
		}
		w.Header().Set("x-litellm-response-cost", "0.1")
		io.WriteString(w, `{}`)
	}))
	defer up.Close()

	store := newMemStore(Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"})
	minter := &fakeMinter{}
	s := NewServer(ServerOptions{
		Auth:       fakeAuth{id: Identity{Subject: "system:serviceaccount:factory:task-42", Namespace: "factory", TaskID: "42", SAUID: "sa-uid-1"}},
		Store:      store,
		Minter:     minter,
		LiteLLMURL: up.URL,
		Prices:     testPrices,
	})
	h := s.Routes("test")

	if rr := doChat(t, h, chatBody("coding-cheap")); rr.Code != http.StatusOK {
		t.Fatalf("first call = %d: %s", rr.Code, rr.Body)
	}

	mu.Lock()
	blocked = true
	mu.Unlock()
	minter.err = errors.New("litellm down")

	rr := doChat(t, h, chatBody("coding-cheap"))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: a real rekey failure with the client still connected must still alert", rr.Code)
	}
	if got := rr.Header().Get(HeaderDecision); got != "error_mint" {
		t.Fatalf("decision header = %q, want error_mint", got)
	}
	if spent, ok := store.spentOf("42"); !ok || spent != 2.1 {
		t.Fatalf("spent = (%v, %v), want (2.1, true) unchanged by the failed rekey", spent, ok)
	}
	if n := store.openHolds(); n != 0 {
		t.Fatalf("%d holds left open, want every hold released", n)
	}
}

// A cheaper alias of the class runs on the session's key (a subagent's
// calls, roadmap 222, item 6): its calls cost at most the session's, so
// the session's hold bounds them. A dearer alias stays refused.
func TestAgentCallsACheaperAliasOfTheClass(t *testing.T) {
	stub := newStubUpstream(t)
	stub.setResp(stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-response-cost": "0.125"}})
	store := newMemStore(Task{Classes: coder, ModelAlias: "coding-frontier", ID: "42", BudgetEUR: 10, Status: "running"})
	dear := testPrice
	dear.In, dear.Out = testPrice.In*10, testPrice.Out*10
	prices := fakePrices{"coding-cheap": testPrice, "coding-frontier": dear}
	s := NewServer(ServerOptions{Auth: fakeAuth{id: Identity{TaskID: "42", SAUID: "sa"}}, Store: store, Minter: &fakeMinter{}, LiteLLMURL: stub.URL, Prices: prices})
	if rr := doChat(t, s.Routes("test"), chatBody("coding-cheap")); rr.Code != http.StatusOK {
		t.Fatalf("coding-cheap under a frontier session = %d %s, want 200 through LiteLLM", rr.Code, rr.Body.String())
	}
	if rows := store.ledger; len(rows) != 1 || rows[0].taskID != "42" || rows[0].ref != "coding-cheap" {
		t.Fatalf("ledger = %+v, want the cheap call booked on the task", rows)
	}

	store = newMemStore(Task{Classes: coder, ModelAlias: "coding-cheap", ID: "42", BudgetEUR: 10, Status: "running"})
	s = NewServer(ServerOptions{Auth: fakeAuth{id: Identity{TaskID: "42", SAUID: "sa"}}, Store: store, Minter: &fakeMinter{}, LiteLLMURL: stub.URL, Prices: prices})
	if rr := doChat(t, s.Routes("test"), chatBody("coding-frontier")); rr.Code != http.StatusForbidden {
		t.Fatalf("coding-frontier under a cheap session = %d, want 403", rr.Code)
	}
}
