package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// memStore is the in-memory Store fake for tests only.
type memStore struct {
	mu     sync.Mutex
	tasks  map[string]Task
	audits []AuditRow
	spends map[string]float64
}

func newMemStore(tasks ...Task) *memStore {
	m := &memStore{tasks: map[string]Task{}, spends: map[string]float64{}}
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

func (m *memStore) UpdateSpent(_ context.Context, id string, spent float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spends[id] = spent
	return nil
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
}

type fakeMinter struct {
	mu    sync.Mutex
	calls []mintCall
	err   error
}

func (f *fakeMinter) Mint(_ context.Context, taskID string, maxBudget float64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, mintCall{taskID, maxBudget})
	if f.err != nil {
		return "", f.err
	}
	return "sk-task-key", nil
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
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
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
	runningTask := Task{ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"}
	testIdentity := Identity{
		Subject:   "system:serviceaccount:factory:task-42",
		Namespace: "factory",
		TaskID:    "42",
	}

	tests := []struct {
		name string

		authErr     error
		task        *Task
		reqBody     string
		mintErr     error
		upstreamURL string // "" = stub URL; "closed" = unreachable port
		resp        stubResp

		wantStatus      int
		wantDecision    string
		wantErrType     string // "" = no error body expected
		wantBody        string // "" = don't check
		wantSpent       *float64
		wantMintBudget  *float64
		wantNoMint      bool
		wantUpstreamHit int
	}{
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
			task:         &Task{ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "completed"},
			reqBody:      chatBody("coding-cheap"),
			wantStatus:   http.StatusConflict,
			wantDecision: "refused_task_status",
			wantErrType:  "invalid_request_error",
		},
		{
			name:            "spent equals budget refuses 402",
			wantNoMint:      true,
			task:            &Task{ID: "42", BudgetEUR: 10, SpentEUR: 10, Status: "running"},
			reqBody:         chatBody("coding-cheap"),
			wantStatus:      http.StatusPaymentRequired,
			wantDecision:    "refused_budget",
			wantErrType:     "budget_exceeded",
			wantUpstreamHit: 0,
		},
		{
			name:            "spent over budget refuses 402",
			wantNoMint:      true,
			task:            &Task{ID: "42", BudgetEUR: 10, SpentEUR: 11, Status: "running"},
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
			name:            "litellm unreachable refuses 502",
			task:            &runningTask,
			reqBody:         chatBody("coding-cheap"),
			upstreamURL:     "closed",
			wantStatus:      http.StatusBadGateway,
			wantDecision:    "error_upstream",
			wantErrType:     "upstream_error",
			wantUpstreamHit: 0,
		},
		{
			name:            "under budget proxies and harvests spend",
			task:            &runningTask,
			reqBody:         chatBody("coding-cheap"),
			resp:            stubResp{status: http.StatusOK, body: `{"choices":[{"message":{"role":"assistant"}}]}`, headers: map[string]string{"x-litellm-key-spend": "0.5"}},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantBody:        `{"choices":[{"message":{"role":"assistant"}}]}`,
			wantSpent:       ptr(2.5),
			wantMintBudget:  ptr(8.0),
			wantUpstreamHit: 1,
		},
		{
			name:            "missing spend header leaves spent untouched",
			task:            &runningTask,
			reqBody:         chatBody("coding-frontier"),
			resp:            stubResp{status: http.StatusOK, body: `{"choices":[]}`},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantBody:        `{"choices":[]}`,
			wantUpstreamHit: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := newStubUpstream(t)
			stub.setResp(tt.resp)

			task := Task{ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"}
			if tt.task != nil {
				task = *tt.task
			}
			store := newMemStore()
			if tt.task != nil {
				store.tasks[task.ID] = task
			}
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
			})

			rr := doChat(t, s.Routes("test"), tt.reqBody)
			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rr.Code, tt.wantStatus, rr.Body.String())
			}

			audits := store.auditRows()
			if len(audits) == 0 {
				t.Fatal("no audit row written")
			}
			last := audits[len(audits)-1]
			if last.Decision != tt.wantDecision {
				t.Fatalf("decision = %q, want %q (all rows %+v)", last.Decision, tt.wantDecision, audits)
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
				if a != "Bearer sk-task-key" {
					t.Fatalf("upstream auth[%d] = %q, want the minted virtual key", i, a)
				}
				if paths[i] != "/v1/chat/completions" {
					t.Fatalf("upstream path[%d] = %q, want /v1/chat/completions", i, paths[i])
				}
				if bodies[i] != tt.reqBody {
					t.Fatalf("upstream body[%d] = %q, want the client body forwarded verbatim", i, bodies[i])
				}
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
	store := newMemStore(Task{ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"})
	minter := &fakeMinter{}
	s := NewServer(ServerOptions{
		Auth:       fakeAuth{id: Identity{Subject: "system:serviceaccount:factory:task-42", Namespace: "factory", TaskID: "42"}},
		Store:      store,
		Minter:     minter,
		LiteLLMURL: stub.URL,
	})

	stub.setResp(stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-key-spend": "0.5"}})
	if rr := doChat(t, s.Routes("test"), chatBody("coding-cheap")); rr.Code != http.StatusOK {
		t.Fatalf("first call status = %d", rr.Code)
	}
	stub.setResp(stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-key-spend": "0.7"}})
	if rr := doChat(t, s.Routes("test"), chatBody("coding-cheap")); rr.Code != http.StatusOK {
		t.Fatalf("second call status = %d", rr.Code)
	}

	if calls := minter.mintCalls(); len(calls) != 1 {
		t.Fatalf("mint calls = %d, want 1 (key cached per task)", len(calls))
	}
	if spent, ok := store.spentOf("42"); !ok || spent != 2.7 {
		t.Fatalf("spent = (%v, %v), want (2.7, true): base 2 + key spend 0.7", spent, ok)
	}
	auths, _, _ := stub.seen()
	if len(auths) != 2 || auths[0] != "Bearer sk-task-key" || auths[1] != "Bearer sk-task-key" {
		t.Fatalf("upstream auths = %v, want the same cached key twice", auths)
	}
}

func TestBodyLimit(t *testing.T) {
	old := maxBodyBytes
	maxBodyBytes = 16
	t.Cleanup(func() { maxBodyBytes = old })

	stub := newStubUpstream(t)
	store := newMemStore(Task{ID: "42", BudgetEUR: 10, Status: "running"})
	s := NewServer(ServerOptions{
		Auth:       fakeAuth{id: Identity{TaskID: "42"}},
		Store:      store,
		Minter:     &fakeMinter{},
		LiteLLMURL: stub.URL,
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
