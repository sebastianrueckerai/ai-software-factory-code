package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/health"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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
	key   string
	saUID string // the run the key was minted for
}

type Server struct {
	opts       ServerOptions
	http       *http.Client
	mu         sync.Mutex
	keysByTask map[string]cachedKey
	mints      singleflight.Group

	now   func() time.Time
	polMu sync.Mutex
	pol   policy.Version
	polAt time.Time // when pol was read; zero = never
}

// policyTTL bounds how long the gateway enforces a policy version after
// reading it: a new version is in force within this time.
const policyTTL = 30 * time.Second

func NewServer(opts ServerOptions) *Server {
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{}
	}
	return &Server{opts: opts, http: opts.HTTP, keysByTask: map[string]cachedKey{}, now: time.Now}
}

// currentPolicy is the policy version in force, read from Postgres at most
// policyTTL ago. The gateway never parses the policy file: factoryd loads
// it, and an unreadable current version refuses every call.
func (s *Server) currentPolicy(ctx context.Context) (*policy.Version, error) {
	s.polMu.Lock()
	defer s.polMu.Unlock()
	if !s.polAt.IsZero() && s.now().Sub(s.polAt) < policyTTL {
		v := s.pol
		return &v, nil
	}
	v, ok, err := s.opts.Store.CurrentPolicy(ctx)
	if err != nil {
		return nil, fmt.Errorf("read current policy: %w", err)
	}
	if !ok {
		return nil, errors.New("no policy version loaded")
	}
	s.pol, s.polAt = v, s.now()
	return &v, nil
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

	pv, polErr := s.currentPolicy(ctx)
	var pid *int64
	if pv != nil {
		pid = &pv.ID
	}

	id, err := s.opts.Auth.Authenticate(r)
	if err != nil {
		s.refuse(w, pid, id.TaskID, "refused_auth", err.Error(), "", http.StatusUnauthorized, "authentication_error")
		return
	}
	if polErr != nil {
		s.refuse(w, pid, id.TaskID, "error_policy", polErr.Error(), "", http.StatusServiceUnavailable, "server_error")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		s.refuse(w, pid, id.TaskID, "refused_body", "request body too large or unreadable", "", http.StatusRequestEntityTooLarge, "invalid_request_error")
		return
	}
	chatReq, err := parseChat(body)
	if err != nil {
		s.refuse(w, pid, id.TaskID, "refused_body", err.Error(), "", http.StatusBadRequest, "invalid_request_error")
		return
	}
	task, ok, err := s.opts.Store.Task(ctx, id.TaskID)
	if err != nil {
		s.refuse(w, pid, id.TaskID, "error_store", err.Error(), chatReq.Model, http.StatusInternalServerError, "server_error")
		return
	}
	if !ok {
		s.refuse(w, pid, id.TaskID, "refused_task_unknown", "no task row for "+id.TaskID, chatReq.Model, http.StatusNotFound, "invalid_request_error")
		return
	}
	if task.Status != "running" {
		s.refuse(w, pid, id.TaskID, "refused_task_status", "task status is "+task.Status, chatReq.Model, http.StatusConflict, "invalid_request_error")
		return
	}

	if !pv.Document.AllowsModel(task.Classes, chatReq.Model) {
		s.refuse(w, pid, id.TaskID, "refused_capability",
			fmt.Sprintf("model %q is not allowed for capability classes %q under policy version %d", chatReq.Model, task.Classes, pv.ID),
			chatReq.Model, http.StatusForbidden, "permission_error")
		return
	}
	taskHeadroom := task.BudgetEUR - task.SpentEUR
	if taskHeadroom <= 0 {
		s.refuseSpend(w, pid, task, "refused_budget", chatReq.Model, fmt.Sprintf("spent %.4f >= budget %.4f EUR", task.SpentEUR, task.BudgetEUR))
		return
	}
	dayTotal, err := s.opts.Store.DayTotal(ctx)
	if err != nil {
		s.refuse(w, pid, id.TaskID, "error_store", err.Error(), chatReq.Model, http.StatusInternalServerError, "server_error")
		return
	}
	dayHeadroom := pv.Document.Budget.DayEUR - dayTotal
	if dayHeadroom <= 0 {
		s.refuseSpend(w, pid, task, "refused_day_cap", chatReq.Model, fmt.Sprintf("today's spend %.4f >= day cap %.4f EUR (UTC day, all tasks)", dayTotal, pv.Document.Budget.DayEUR))
		return
	}

	if chatReq.Stream {
		if err := withStreamUsage(chatReq.fields); err != nil {
			s.refuse(w, pid, id.TaskID, "refused_body", "stream_options is not a JSON object", chatReq.Model, http.StatusBadRequest, "invalid_request_error")
			return
		}
	}
	if body, err = json.Marshal(chatReq.fields); err != nil {
		s.refuse(w, pid, id.TaskID, "refused_body", err.Error(), chatReq.Model, http.StatusBadRequest, "invalid_request_error")
		return
	}

	key, err := s.keyFor(ctx, task, id.SAUID, min(taskHeadroom, dayHeadroom))
	if err != nil {
		s.refuse(w, pid, id.TaskID, "error_mint", err.Error(), chatReq.Model, http.StatusBadGateway, "upstream_error")
		return
	}

	s.proxy(w, r, pid, task, key, body, chatReq.Model, chatReq.Stream)
}

type chatRequest struct {
	Model  string
	Stream bool
	fields map[string]json.RawMessage
}

// parseChat reads model and stream by exact key, as LiteLLM does (Go's
// struct decoding would also match "MODEL" or "Stream"). A repeated key keeps
// its last value here, and the gateway forwards the re-encoded fields, so
// the body LiteLLM parses holds each key once, with the value checked here.
func parseChat(body []byte) (chatRequest, error) {
	req := chatRequest{}
	if err := json.Unmarshal(body, &req.fields); err != nil || req.fields == nil {
		return req, errors.New("request body is not a JSON object")
	}
	if raw, ok := req.fields["model"]; ok {
		if err := json.Unmarshal(raw, &req.Model); err != nil {
			return req, errors.New("model is not a string")
		}
	}
	if raw, ok := req.fields["stream"]; ok {
		if err := json.Unmarshal(raw, &req.Stream); err != nil {
			return req, errors.New("stream is not a boolean")
		}
	}
	return req, nil
}

// withStreamUsage makes a streaming request end with a usage chunk, which
// LiteLLM stamps with the call's cost: the only place a stream's cost shows.
func withStreamUsage(req map[string]json.RawMessage) error {
	opts := map[string]json.RawMessage{}
	if raw, ok := req["stream_options"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &opts); err != nil {
			return err
		}
	}
	opts["include_usage"] = json.RawMessage("true")
	var err error
	req["stream_options"], err = json.Marshal(opts)
	return err
}

// keyFor returns the virtual key of the task's current run or mints one
// capped at maxBudget, the smaller of the task's and the day's headroom, so
// a task can never spend past either even if the gateway's own check lags.
// A requeue blocks the old key and recreates the service account, so a new
// SA UID gets a new key.
func (s *Server) keyFor(ctx context.Context, task Task, saUID string, maxBudget float64) (string, error) {
	if key, ok := s.cachedKey(task.ID, saUID); ok {
		return key, nil
	}
	// one mint per run at a time, and no task waits on another's mint
	v, err, _ := s.mints.Do(task.ID+"\x00"+saUID, func() (any, error) {
		if key, ok := s.cachedKey(task.ID, saUID); ok {
			return key, nil
		}
		key, err := s.opts.Minter.Mint(context.WithoutCancel(ctx), task.ID, maxBudget)
		if err != nil {
			return "", fmt.Errorf("mint virtual key: %w", err)
		}
		s.mu.Lock()
		s.keysByTask[task.ID] = cachedKey{key: key, saUID: saUID}
		s.mu.Unlock()
		return key, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

func (s *Server) cachedKey(taskID, saUID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.keysByTask[taskID]
	return c.key, ok && c.saUID == saUID
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, pid *int64, task Task, key string, body []byte, alias string, stream bool) {
	ctx := r.Context()
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.opts.LiteLLMURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		s.refuse(w, pid, task.ID, "error_upstream", err.Error(), alias, http.StatusBadGateway, "upstream_error")
		return
	}
	upReq.Header.Set("Content-Type", contentType(r))
	upReq.Header.Set("Authorization", "Bearer "+key)

	res, err := s.http.Do(upReq)
	if err != nil {
		s.refuse(w, pid, task.ID, "error_upstream", err.Error(), alias, http.StatusBadGateway, "upstream_error")
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
			json.Unmarshal(respBody, &liteErr) // #nosec G104 -- best-effort message extraction; an empty liteErr.Error.Message on failure still refuses the spend
			s.refuseSpend(w, pid, task, "refused_budget", alias, "litellm budget_exceeded: "+liteErr.Error.Message)
			return
		}
		s.audit(pid, task.ID, "error_upstream", fmt.Sprintf("upstream %d", res.StatusCode), alias, &task.SpentEUR)
		w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
		w.WriteHeader(res.StatusCode)
		w.Write(respBody) // #nosec G104 -- best-effort response write, client disconnect is the only failure mode
		return
	}

	w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
	w.WriteHeader(res.StatusCode)

	// Each call's own cost is booked onto the task. x-litellm-key-spend is
	// not: it is a cached read that lags, and a stream's headers leave before
	// its cost exists. A stream's cost arrives in its final usage chunk. Costs
	// are LiteLLM's USD prices booked 1:1 as EUR, the unit of budget_eur and
	// of the key's max_budget.
	var cost float64
	costErr := error(nil)
	if stream {
		usage := &usageScanner{}
		s.stream(w, io.TeeReader(res.Body, usage))
		cost, costErr = usage.cost, usage.err()
	} else {
		cost, costErr = strconv.ParseFloat(res.Header.Get("x-litellm-response-cost"), 64)
		s.stream(w, res.Body)
	}

	// Booked on a background context: the billing happened, so it must be
	// recorded even if the caller disconnects.
	spend, reason := task.SpentEUR, ""
	if costErr != nil {
		reason = "cost not reported, not booked: " + costErr.Error()
	} else if total, err := s.opts.Store.AddSpent(context.Background(), task.ID, alias, cost); err != nil {
		reason = "cost " + strconv.FormatFloat(cost, 'g', -1, 64) + " not booked: " + err.Error()
	} else {
		spend = total
	}
	if reason != "" {
		log.Printf("task %s: %s", task.ID, reason)
	}
	s.audit(pid, task.ID, "allowed", reason, alias, &spend)
}

// maxSSELine bounds what usageScanner buffers of one line. The usage chunk
// is small; a longer line is content, passed through but not inspected.
const maxSSELine = 1 << 20

// usageScanner watches an SSE stream for the usage chunk's cost.
type usageScanner struct {
	pending  []byte
	skipping bool // the current line outgrew maxSSELine
	cost     float64
	seen     bool
}

func (u *usageScanner) Write(p []byte) (int, error) {
	n := len(p)
	for {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if !u.skipping {
				u.pending = append(u.pending, p...)
				if len(u.pending) > maxSSELine {
					u.pending, u.skipping = nil, true
				}
			}
			return n, nil
		}
		if !u.skipping {
			u.line(append(u.pending, p[:i]...))
		}
		u.pending, u.skipping = u.pending[:0], false
		p = p[i+1:]
	}
}

func (u *usageScanner) line(l []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimSpace(l), []byte("data:"))
	if !ok || !bytes.Contains(data, []byte(`"usage"`)) {
		return
	}
	var ev struct {
		Usage *struct {
			Cost *float64 `json:"cost"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &ev) == nil && ev.Usage != nil && ev.Usage.Cost != nil {
		u.cost, u.seen = *ev.Usage.Cost, true
	}
}

func (u *usageScanner) err() error {
	if !u.seen {
		return errors.New("stream ended without a usage cost")
	}
	return nil
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

// refuseSpend writes a 402 refusal: the task's budget or the day cap. The
// gateway refuses with 402; LiteLLM's 429 + budget_exceeded is only the
// backstop mapped onto it.
func (s *Server) refuseSpend(w http.ResponseWriter, pid *int64, task Task, decision, alias, reason string) {
	s.audit(pid, task.ID, decision, reason, alias, &task.SpentEUR)
	writeError(w, http.StatusPaymentRequired, "budget_exceeded", reason)
}

func (s *Server) refuse(w http.ResponseWriter, pid *int64, taskID, decision, reason, alias string, status int, typ string) {
	s.audit(pid, taskID, decision, reason, alias, nil)
	writeError(w, status, typ, reason)
}

// audit persists the decision on a background context: audit rows are the
// record of what the gateway did and must survive the caller disconnecting.
func (s *Server) audit(pid *int64, taskID, decision, reason, alias string, spend *float64) {
	if err := s.opts.Store.InsertAudit(context.Background(), AuditRow{
		TaskID:        taskID,
		Decision:      decision,
		Reason:        reason,
		ModelAlias:    alias,
		SpendEUR:      spend,
		PolicyVersion: pid,
	}); err != nil {
		log.Printf("insert audit row (task %s, decision %s): %v", taskID, decision, err)
	}
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{ // #nosec G104 -- best-effort response write
		"error": map[string]any{
			"message": msg,
			"type":    typ,
			"param":   nil,
			"code":    strconv.Itoa(status),
		},
	})
}
