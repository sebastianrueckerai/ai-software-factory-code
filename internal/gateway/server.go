package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/health"
)

// Authenticator is implemented by Auth; tests substitute a fake.
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

// KeyMinter mints a per-task LiteLLM virtual key with a spend cap of
// maxBudget. Implemented by LiteLLM; tests substitute a fake.
type KeyMinter interface {
	Mint(ctx context.Context, taskID string, maxBudget float64) (string, error)
}

var (
	_ Authenticator = (*Auth)(nil)
	_ KeyMinter     = (*LiteLLM)(nil)
)

// maxBodyBytes caps the proxied request body at the trust boundary. Large
// enough for 1M-token contexts (about 4 MB of text), var so tests can lower
// it.
var maxBodyBytes int64 = 16 << 20

type ServerOptions struct {
	Auth       Authenticator
	Store      Store
	Minter     KeyMinter
	LiteLLMURL string
	HTTP       *http.Client // proxy client; nil = no timeout, streaming-safe
}

type cachedKey struct {
	key       string
	baseSpend float64 // task spent at mint time
}

type Server struct {
	opts       ServerOptions
	http       *http.Client
	mu         sync.Mutex
	keysByTask map[string]cachedKey
}

func NewServer(opts ServerOptions) *Server {
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{}
	}
	return &Server{opts: opts, http: opts.HTTP, keysByTask: map[string]cachedKey{}}
}

// Routes is the gateway's full HTTP surface: the single model route plus the
// pinned healthz shape from internal/health.
func (s *Server) Routes(version string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", http.HandlerFunc(s.handleChat))
	mux.Handle("/healthz", health.Handler("factory-gateway", version))
	return mux
}

// Budget consumer contract: the GLM upstream behind coding-frontier and
// review-frontier always reasons before answering, 200+ tokens billed at the
// output rate even for trivial prompts. Task budgets set by factoryd and the
// remaining-budget caps minted here must leave headroom for that reasoning
// overhead, and audit reasons carry the upstream refusal messages verbatim
// so an exhausted-by-reasoning task stays explainable after the fact.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id, err := s.opts.Auth.Authenticate(r)
	if err != nil {
		s.refuse(w, id.TaskID, "refused_auth", err.Error(), "", http.StatusUnauthorized, "authentication_error")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		s.refuse(w, id.TaskID, "refused_body", "request body too large or unreadable", "", http.StatusRequestEntityTooLarge, "invalid_request_error")
		return
	}
	var chatReq struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &chatReq); err != nil {
		s.refuse(w, id.TaskID, "refused_body", "request body is not JSON", "", http.StatusBadRequest, "invalid_request_error")
		return
	}

	task, ok, err := s.opts.Store.Task(ctx, id.TaskID)
	if err != nil {
		s.refuse(w, id.TaskID, "error_store", err.Error(), chatReq.Model, http.StatusInternalServerError, "server_error")
		return
	}
	if !ok {
		s.refuse(w, id.TaskID, "refused_task_unknown", "no task row for "+id.TaskID, chatReq.Model, http.StatusNotFound, "invalid_request_error")
		return
	}
	if task.Status != "running" {
		s.refuse(w, id.TaskID, "refused_task_status", "task status is "+task.Status, chatReq.Model, http.StatusConflict, "invalid_request_error")
		return
	}

	if task.SpentEUR >= task.BudgetEUR {
		s.refuseSpend(w, task, chatReq.Model, fmt.Sprintf("spent %.4f >= budget %.4f EUR", task.SpentEUR, task.BudgetEUR))
		return
	}

	key, err := s.keyFor(ctx, task)
	if err != nil {
		s.refuse(w, id.TaskID, "error_mint", err.Error(), chatReq.Model, http.StatusBadGateway, "upstream_error")
		return
	}

	s.proxy(w, r, task, key, body, chatReq.Model)
}

// keyFor returns the task's cached virtual key or mints one capped at the
// remaining budget, so a task can never spend past its budget_eur even if
// the gateway's own check lags.
func (s *Server) keyFor(ctx context.Context, task Task) (cachedKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.keysByTask[task.ID]; ok {
		return c, nil
	}
	key, err := s.opts.Minter.Mint(ctx, task.ID, task.BudgetEUR-task.SpentEUR)
	if err != nil {
		return cachedKey{}, fmt.Errorf("mint virtual key: %w", err)
	}
	c := cachedKey{key: key, baseSpend: task.SpentEUR}
	s.keysByTask[task.ID] = c
	return c, nil
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, task Task, key cachedKey, body []byte, alias string) {
	ctx := r.Context()
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.opts.LiteLLMURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		s.refuse(w, task.ID, "error_upstream", err.Error(), alias, http.StatusBadGateway, "upstream_error")
		return
	}
	upReq.Header.Set("Content-Type", contentType(r))
	upReq.Header.Set("Authorization", "Bearer "+key.key)

	res, err := s.http.Do(upReq)
	if err != nil {
		s.refuse(w, task.ID, "error_upstream", err.Error(), alias, http.StatusBadGateway, "upstream_error")
		return
	}
	defer res.Body.Close()

	if res.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		if res.StatusCode == http.StatusTooManyRequests && isBudgetExceeded(respBody) {
			var liteErr struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			json.Unmarshal(respBody, &liteErr)
			s.refuseSpend(w, task, alias, "litellm budget_exceeded: "+liteErr.Error.Message)
			return
		}
		s.audit(task.ID, "error_upstream", fmt.Sprintf("upstream %d", res.StatusCode), alias, &task.SpentEUR)
		w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
		w.WriteHeader(res.StatusCode)
		w.Write(respBody)
		return
	}

	// Spend is the key's cumulative total; the task's is that plus whatever
	// the task had already spent when the key was minted. Written on a
	// background context: the billing happened, so it must be recorded even
	// if the caller disconnects mid-stream.
	spend := task.SpentEUR
	if v, err := strconv.ParseFloat(res.Header.Get("x-litellm-key-spend"), 64); err == nil {
		spend = key.baseSpend + v
		if err := s.opts.Store.UpdateSpent(context.Background(), task.ID, spend); err != nil {
			log.Printf("update spent for task %s: %v", task.ID, err)
		}
	}
	s.audit(task.ID, "allowed", "", alias, &spend)

	w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
	w.WriteHeader(res.StatusCode)
	s.stream(w, res.Body)
}

// stream copies the upstream body with per-write flushes so SSE streaming
// responses pass through unbuffered.
func (s *Server) stream(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func isBudgetExceeded(body []byte) bool {
	var e struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	return json.Unmarshal(body, &e) == nil && e.Error.Type == "budget_exceeded"
}

func contentType(r *http.Request) string {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		return ct
	}
	return "application/json"
}

// refuseSpend writes the 402 budget refusal. The gateway refuses with 402;
// LiteLLM's 429 + budget_exceeded is only the backstop mapped onto it.
func (s *Server) refuseSpend(w http.ResponseWriter, task Task, alias, reason string) {
	s.audit(task.ID, "refused_budget", reason, alias, &task.SpentEUR)
	writeError(w, http.StatusPaymentRequired, "budget_exceeded", reason)
}

func (s *Server) refuse(w http.ResponseWriter, taskID, decision, reason, alias string, status int, typ string) {
	s.audit(taskID, decision, reason, alias, nil)
	writeError(w, status, typ, reason)
}

// audit persists the decision on a background context: audit rows are the
// record of what the gateway did and must survive the caller disconnecting.
func (s *Server) audit(taskID, decision, reason, alias string, spend *float64) {
	if err := s.opts.Store.InsertAudit(context.Background(), AuditRow{
		TaskID:     taskID,
		Decision:   decision,
		Reason:     reason,
		ModelAlias: alias,
		SpendEUR:   spend,
	}); err != nil {
		log.Printf("insert audit row (task %s, decision %s): %v", taskID, decision, err)
	}
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    typ,
			"param":   nil,
			"code":    strconv.Itoa(status),
		},
	})
}
