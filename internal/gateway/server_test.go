package gateway

import (
	"bytes"
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
	"time"
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

func (m *memStore) AddSpent(_ context.Context, id string, cost float64) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[id]
	t.SpentEUR += cost
	m.tasks[id] = t
	m.spends[id] = t.SpentEUR
	return t.SpentEUR, nil
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
	hold  map[string]chan struct{} // a task's mint waits until its channel closes
}

func (f *fakeMinter) Mint(_ context.Context, taskID string, maxBudget float64) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, mintCall{taskID, maxBudget})
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
	runningTask := Task{ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"}
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

		wantStatus      int
		wantDecision    string
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
			name:    "under budget proxies and books the call cost",
			task:    &runningTask,
			reqBody: chatBody("coding-cheap"),
			resp: stubResp{status: http.StatusOK, body: `{"choices":[{"message":{"role":"assistant"}}]}`, headers: map[string]string{
				"x-litellm-response-cost": "0.5",
				"x-litellm-key-spend":     "7", // stale cache read: never booked
			}},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantBody:        `{"choices":[{"message":{"role":"assistant"}}]}`,
			wantSpent:       ptr(2.5),
			wantMintBudget:  ptr(8.0),
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
			}},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantBody:        sseBody,
			wantUpstreamReq: `{"messages":[{"role":"user","content":"hi"}],"model":"coding-frontier","stream":true,"stream_options":{"include_usage":true}}`,
			wantSpent:       ptr(2.25),
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
			wantSpent:       ptr(2.25),
			wantUpstreamHit: 1,
		},
		{
			name:            "stream without a usage chunk leaves spent untouched",
			task:            &runningTask,
			reqBody:         `{"model":"coding-frontier","stream":true,"messages":[]}`,
			wantUpstreamReq: `{"messages":[],"model":"coding-frontier","stream":true,"stream_options":{"include_usage":true}}`,
			resp:            stubResp{status: http.StatusOK, body: "data: {\"choices\":[]}\n\ndata: [DONE]\n\n", headers: map[string]string{"Content-Type": "text/event-stream"}},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantUpstreamHit: 1,
		},
		{
			name:            "a case variant of stream does not turn a stream into a plain call",
			task:            &runningTask,
			reqBody:         `{"model":"coding-frontier","stream":true,"STREAM":false,"messages":[]}`,
			resp:            stubResp{status: http.StatusOK, body: sseBody, headers: map[string]string{"Content-Type": "text/event-stream", "x-litellm-response-cost": "0.0"}},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantUpstreamReq: `{"STREAM":false,"messages":[],"model":"coding-frontier","stream":true,"stream_options":{"include_usage":true}}`,
			wantSpent:       ptr(2.25),
			wantUpstreamHit: 1,
		},
		{
			name:            "a repeated stream key counts by its last value and is forwarded once",
			task:            &runningTask,
			reqBody:         `{"model":"coding-frontier","stream":false,"messages":[],"stream":true}`,
			resp:            stubResp{status: http.StatusOK, body: sseBody, headers: map[string]string{"Content-Type": "text/event-stream", "x-litellm-response-cost": "0.0"}},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantUpstreamReq: `{"messages":[],"model":"coding-frontier","stream":true,"stream_options":{"include_usage":true}}`,
			wantSpent:       ptr(2.25),
			wantUpstreamHit: 1,
		},
		{
			name:            "the audited model is the exact-case key LiteLLM calls",
			task:            &runningTask,
			reqBody:         `{"Model":"review-frontier","model":"coding-cheap","messages":[]}`,
			resp:            stubResp{status: http.StatusOK, body: `{"choices":[]}`, headers: map[string]string{"x-litellm-response-cost": "0.5"}},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantUpstreamReq: `{"Model":"review-frontier","messages":[],"model":"coding-cheap"}`,
			wantSpent:       ptr(2.5),
			wantUpstreamHit: 1,
			wantAlias:       "coding-cheap",
		},
		{
			name:            "a repeated model key counts by its last value and is forwarded once",
			task:            &runningTask,
			reqBody:         `{"model":"review-frontier","messages":[],"model":"coding-cheap"}`,
			resp:            stubResp{status: http.StatusOK, body: `{"choices":[]}`, headers: map[string]string{"x-litellm-response-cost": "0.5"}},
			wantStatus:      http.StatusOK,
			wantDecision:    "allowed",
			wantUpstreamReq: `{"messages":[],"model":"coding-cheap"}`,
			wantSpent:       ptr(2.5),
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
			name:            "missing cost header leaves spent untouched",
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
				if bodies[i] != want {
					t.Fatalf("upstream body[%d] = %q, want %q", i, bodies[i], want)
				}
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
	store := newMemStore(Task{ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"})
	minter := &fakeMinter{}
	s := NewServer(ServerOptions{
		Auth:       fakeAuth{id: Identity{Subject: "system:serviceaccount:factory:task-42", Namespace: "factory", TaskID: "42", SAUID: "sa-uid-1"}},
		Store:      store,
		Minter:     minter,
		LiteLLMURL: stub.URL,
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

// A requeued task runs under a recreated service account; its old key was
// blocked at requeue, so the gateway must mint a fresh one capped at what is
// left of the budget.
// A slow mint for one task must not hold up another task's calls, and
// concurrent calls of one run share a single mint.
func TestKeyMintsDoNotSerializeTasks(t *testing.T) {
	stub := newStubUpstream(t)
	stub.setResp(stubResp{status: http.StatusOK, body: `{}`, headers: map[string]string{"x-litellm-response-cost": "0.5"}})
	store := newMemStore(
		Task{ID: "slow", BudgetEUR: 10, Status: "running"},
		Task{ID: "fast", BudgetEUR: 10, Status: "running"},
	)
	release := make(chan struct{})
	minter := &fakeMinter{hold: map[string]chan struct{}{"slow": release}}
	auth := authByTask{}
	s := NewServer(ServerOptions{Auth: auth, Store: store, Minter: minter, LiteLLMURL: stub.URL})
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
	if !u.seen || u.cost != 0.25 {
		t.Fatalf("cost = %v seen = %v, want the usage after the long line: 0.25", u.cost, u.seen)
	}
}

func TestKeyRemintedForNewServiceAccount(t *testing.T) {
	stub := newStubUpstream(t)
	store := newMemStore(Task{ID: "42", BudgetEUR: 10, SpentEUR: 2, Status: "running"})
	minter := &fakeMinter{}
	id := Identity{Subject: "system:serviceaccount:factory:task-42", Namespace: "factory", TaskID: "42", SAUID: "sa-uid-1"}
	s := NewServer(ServerOptions{Auth: fakeAuth{id: id}, Store: store, Minter: minter, LiteLLMURL: stub.URL})
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

const sseBody = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{}}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"cost\":0.25}}\n\n" +
	"data: [DONE]\n\n"
