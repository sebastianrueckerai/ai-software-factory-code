package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
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

// KeyMinter mints a per-task LiteLLM virtual key capped at maxBudget and
// limited to models, and blocks a superseded one. Implemented by LiteLLM;
// tests substitute a fake.
type KeyMinter interface {
	Mint(ctx context.Context, taskID string, maxBudget float64, models []string) (string, error)
	Block(ctx context.Context, key string) error
}

// Pricer is an alias's worst-case price. Implemented by policy.PriceBook.
type Pricer interface {
	Price(ctx context.Context, alias string) (policy.Price, error)
}

var (
	_ Authenticator = (*Auth)(nil)
	_ KeyMinter     = (*LiteLLM)(nil)
	_ Pricer        = (*policy.PriceBook)(nil)
)

// maxBodyBytes caps the proxied request body at the trust boundary. Large
// enough for 1M-token contexts (about 4 MB of text), var so tests can lower
// it.
var maxBodyBytes int64 = 16 << 20

type ServerOptions struct {
	Auth       Authenticator
	Store      Store
	Minter     KeyMinter
	Prices     Pricer
	LiteLLMURL string
	HTTP       *http.Client // proxy client; nil = no timeout, streaming-safe
	// the MCP tools' connectors and the escalation hand-over
	Firecrawl *Firecrawl
	Replicate *Replicate
	Fal       *Fal
	Escalator Escalator
	// Cloud is the commissioning connector (the agent Hetzner project).
	Cloud *Cloud
	// Telemetry receives the spans and metrics; nil = none.
	Telemetry *Telemetry
	// FactorydToken is the gateway-factoryd token: with it factoryd calls a
	// model on a task's account for one of policy.Purposes.
	FactorydToken string
}

// factoryd names the task and the purpose of its model calls in these
// headers; the gateway answers a refusal's decision in HeaderDecision.
const (
	HeaderFactoryTask    = "X-Factory-Task"
	HeaderFactoryPurpose = "X-Factory-Purpose"
	HeaderDecision       = "X-Gateway-Decision"
)

// factorydSA stands for the service account uid of factoryd's key slot.
const factorydSA = "factoryd"

// liveKey is a task's virtual key and what it was minted for: a new run,
// policy version or UTC day mints a new key (its models and max_budget
// follow from them) and retires this one. A retired key is blocked once
// the calls still using it are done, so none of them fails on the block.
type liveKey struct {
	key     string
	saUID   string
	version int64
	day     string
	// bound is the refusal the key's max_budget stands for: the smaller of
	// the task's and the day's headroom at the mint.
	bound    string
	inFlight int
	retired  bool
}

type Server struct {
	opts       ServerOptions
	tel        *Telemetry
	http       *http.Client
	mu         sync.Mutex
	keysByTask map[string]*liveKey
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
	tel := opts.Telemetry
	if tel == nil {
		tel = noTelemetry()
	}
	return &Server{opts: opts, tel: tel, http: opts.HTTP, keysByTask: map[string]*liveKey{}, now: time.Now}
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

// Routes is the gateway's full HTTP surface: the model route, the MCP
// tools and the pinned healthz shape from internal/health.
func (s *Server) Routes(version string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", http.HandlerFunc(s.handleChat))
	mux.Handle("/mcp", s.mcpHandler())
	mux.Handle("/healthz", health.Handler("factory-gateway", version))
	return s.tel.handler(mux)
}

// inTokenSlack covers the bytes the output limit adds to the body after the
// input bound is taken.
const inTokenSlack = 64

// handleChat admits a call only if its worst case fits the task's and the
// day's free headroom: the output limit is clamped to what the headroom can
// pay for at the alias's LiteLLM price, and that worst case is held until
// the call's cost is booked. GLM always reasons before answering (200+
// output tokens even for trivial prompts), so a call that cannot fit
// policy.MinCallTokens is refused.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	pv, polErr := s.currentPolicy(ctx)
	var pid *int64
	if pv != nil {
		pid = &pv.ID
	}

	id, purpose, err := s.identify(r)
	if err != nil {
		s.refuse(w, pid, id.TaskID, "", "refused_auth", err.Error(), "", http.StatusUnauthorized, "authentication_error")
		return
	}
	caller(ctx, id.TaskID)
	if polErr != nil {
		s.refuse(w, pid, id.TaskID, purpose, "error_policy", polErr.Error(), "", http.StatusServiceUnavailable, "server_error")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		s.refuse(w, pid, id.TaskID, purpose, "refused_body", "request body too large or unreadable", "", http.StatusRequestEntityTooLarge, "invalid_request_error")
		return
	}
	chatReq, err := parseChat(body)
	if err != nil {
		s.refuse(w, pid, id.TaskID, purpose, "refused_body", err.Error(), chatReq.Model, http.StatusBadRequest, "invalid_request_error")
		return
	}
	task, ok, err := s.opts.Store.Task(ctx, id.TaskID)
	if err != nil {
		s.refuse(w, pid, id.TaskID, purpose, "error_store", err.Error(), chatReq.Model, http.StatusInternalServerError, "server_error")
		return
	}
	if !ok {
		s.refuse(w, pid, id.TaskID, purpose, "refused_task_unknown", "no task row for "+id.TaskID, chatReq.Model, http.StatusNotFound, "invalid_request_error")
		return
	}
	// factoryd's calls run in review too: the hold admits a running or
	// in-review task
	if purpose == "" && task.Status != "running" {
		s.refuse(w, pid, id.TaskID, purpose, "refused_task_status", "task status is "+task.Status, chatReq.Model, http.StatusConflict, "invalid_request_error")
		return
	}

	if purpose == "" && !pv.Document.AllowsModel(task.Classes, chatReq.Model) {
		s.refuse(w, pid, id.TaskID, purpose, "refused_capability",
			fmt.Sprintf("model %q is not allowed for capability classes %q under policy version %d", chatReq.Model, task.Classes, pv.ID),
			chatReq.Model, http.StatusForbidden, "permission_error")
		return
	}
	if want := pv.Document.FactoryModel(task.Classes, purpose); purpose != "" && (want == "" || chatReq.Model != want) {
		s.refuse(w, pid, id.TaskID, purpose, "refused_capability",
			fmt.Sprintf("model %q is not the %s model %q of capability classes %q under policy version %d", chatReq.Model, purpose, want, task.Classes, pv.ID),
			chatReq.Model, http.StatusForbidden, "permission_error")
		return
	}
	price, err := s.opts.Prices.Price(ctx, chatReq.Model)
	if err != nil {
		log.Printf("PRICE MISSING: task %s, model %s: %v; every call of this alias is refused", task.ID, chatReq.Model, err)
		s.refuse(w, pid, id.TaskID, purpose, "error_price", err.Error(), chatReq.Model, http.StatusServiceUnavailable, "server_error")
		return
	}

	if chatReq.Stream {
		if err := withStreamUsage(chatReq.fields); err != nil {
			s.refuse(w, pid, id.TaskID, purpose, "refused_body", "stream_options is not a JSON object", chatReq.Model, http.StatusBadRequest, "invalid_request_error")
			return
		}
	}
	if body, err = json.Marshal(chatReq.fields); err != nil {
		s.refuse(w, pid, id.TaskID, purpose, "refused_body", err.Error(), chatReq.Model, http.StatusBadRequest, "invalid_request_error")
		return
	}

	ref, slot, models := chatReq.Model, task.ID, pv.Document.Models(task.Classes)
	if purpose != "" {
		ref, slot, models = purpose+"/"+chatReq.Model, task.ID+"/"+factorydSA, pv.Document.FactoryModels(task.Classes)
	}
	hold, err := s.opts.Store.TakeHold(ctx, policy.HoldRequest{
		TaskID: task.ID, Kind: policy.KindModel, Ref: ref,
		InTokens: len(body) + inTokenSlack, Price: price, WantOut: chatReq.WantOut,
		DayEUR: pv.Document.Budget.DayEUR,
	})
	var refusal *policy.Refusal
	if errors.As(err, &refusal) {
		s.refuseSpend(w, pid, task, purpose, refusal.Decision, chatReq.Model, refusal.Reason)
		return
	}
	if err != nil {
		s.refuse(w, pid, id.TaskID, purpose, "error_store", err.Error(), chatReq.Model, http.StatusInternalServerError, "server_error")
		return
	}
	call := &modelCall{pid: pid, task: task, hold: hold, alias: chatReq.Model, kind: purpose, slot: slot}

	if body, err = chatReq.withOutputLimit(hold.MaxOut); err != nil {
		s.finish(call, zeroCost, "")
		s.refuse(w, pid, id.TaskID, purpose, "refused_body", err.Error(), chatReq.Model, http.StatusBadRequest, "invalid_request_error")
		return
	}
	key, err := s.keyFor(ctx, slot, task, id.SAUID, models, pv, hold)
	if err != nil {
		s.finish(call, zeroCost, "")
		s.refuse(w, pid, id.TaskID, purpose, "error_mint", err.Error(), chatReq.Model, http.StatusBadGateway, "upstream_error")
		return
	}
	defer s.doneWith(slot, key)
	call.key = key

	s.proxy(w, r, call, key.key, body, chatReq.Stream)
}

// identify is the caller: factoryd by the gateway-factoryd token, with
// the task and one of policy.Purposes in its headers, or a sandbox by its
// ServiceAccount JWT (purpose ""), whose headers name nothing.
func (s *Server) identify(r *http.Request) (Identity, string, error) {
	raw, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if s.opts.FactorydToken == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(raw)), []byte(s.opts.FactorydToken)) != 1 {
		id, err := s.opts.Auth.Authenticate(r)
		return id, "", err
	}
	id := Identity{TaskID: r.Header.Get(HeaderFactoryTask), SAUID: factorydSA}
	purpose := r.Header.Get(HeaderFactoryPurpose)
	if id.TaskID == "" || !slices.Contains(policy.Purposes, purpose) {
		return id, "", fmt.Errorf("a factoryd call names its task in %s and one of %q in %s", HeaderFactoryTask, policy.Purposes, HeaderFactoryPurpose)
	}
	return id, purpose, nil
}

// forwardedFields are the request fields LiteLLM receives: the OpenAI chat
// parameters. Everything else is refused, above all LiteLLM's own routing
// overrides (fallbacks, context_window_fallbacks, content_policy_fallbacks,
// router_settings_override, mock_testing_*), which would reach aliases
// outside the task's classes.
var forwardedFields = map[string]bool{
	"model": true, "messages": true, "stream": true, "stream_options": true,
	"max_tokens": true, "max_completion_tokens": true, "n": true,
	"temperature": true, "top_p": true, "stop": true, "seed": true,
	"presence_penalty": true, "frequency_penalty": true, "logit_bias": true,
	"logprobs": true, "top_logprobs": true, "user": true,
	"tools": true, "tool_choice": true, "parallel_tool_calls": true,
	"response_format": true, "reasoning_effort": true,
}

type chatRequest struct {
	Model  string
	Stream bool
	// WantOut is the caller's output limit, the smaller of max_tokens and
	// max_completion_tokens; 0 = none.
	WantOut int
	fields  map[string]json.RawMessage
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
	var refused []string
	for k := range req.fields {
		if !forwardedFields[k] {
			refused = append(refused, k)
		}
	}
	if len(refused) > 0 {
		slices.Sort(refused)
		return req, fmt.Errorf("fields %q are not accepted", refused)
	}
	if raw, ok := req.fields["stream"]; ok {
		if err := json.Unmarshal(raw, &req.Stream); err != nil {
			return req, errors.New("stream is not a boolean")
		}
	}
	if err := checkTools(req.fields); err != nil {
		return req, err
	}
	if err := checkMessages(req.fields["messages"]); err != nil {
		return req, err
	}
	if raw, ok := req.fields["n"]; ok && string(raw) != "null" {
		var n int
		if err := json.Unmarshal(raw, &n); err != nil || n != 1 {
			return req, errors.New("n must be 1")
		}
	}
	for _, f := range []string{"max_tokens", "max_completion_tokens"} {
		raw, ok := req.fields[f]
		if !ok || string(raw) == "null" {
			continue
		}
		var n int
		if err := json.Unmarshal(raw, &n); err != nil || n <= 0 {
			return req, fmt.Errorf("%s is not a positive integer", f)
		}
		if req.WantOut == 0 || n < req.WantOut {
			req.WantOut = n
		}
	}
	return req, nil
}

// checkTools admits function tools only: a provider's built-in tools (web
// search, retrieval, code interpreter) are billed per use outside token
// prices and reach the internet past the sandbox's egress policy. Keys are
// read exactly, as the provider reads them.
func checkTools(fields map[string]json.RawMessage) error {
	if raw, ok := fields["tools"]; ok && string(raw) != "null" {
		var tools []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &tools); err != nil {
			return errors.New("tools is not an array of objects")
		}
		for i, t := range tools {
			if string(t["type"]) != `"function"` {
				return fmt.Errorf("tools[%d] has type %s; only function tools are accepted", i, t["type"])
			}
		}
	}
	raw, ok := fields["tool_choice"]
	if !ok || string(raw) == "null" {
		return nil
	}
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		if mode == "auto" || mode == "none" || mode == "required" {
			return nil
		}
		return fmt.Errorf("tool_choice %q is not accepted", mode)
	}
	var choice map[string]json.RawMessage
	if err := json.Unmarshal(raw, &choice); err != nil || string(choice["type"]) != `"function"` {
		return errors.New("tool_choice must be auto, none, required or a function")
	}
	return nil
}

// checkMessages admits text content only: a string, null, or text parts.
// An image, audio or file part is billed far above its bytes, so the hold
// could not bound it, and LiteLLM would fetch an image URL to size it.
func checkMessages(raw json.RawMessage) error {
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return errors.New("messages is not an array of objects")
	}
	for i, m := range msgs {
		c, ok := m["content"]
		if !ok || string(c) == "null" {
			continue
		}
		var text string
		if json.Unmarshal(c, &text) == nil {
			continue
		}
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(c, &parts); err != nil {
			return fmt.Errorf("messages[%d].content is neither text nor an array of parts", i)
		}
		for j, p := range parts {
			if string(p["type"]) != `"text"` {
				return fmt.Errorf("messages[%d].content[%d] has type %s; only text parts are accepted", i, j, p["type"])
			}
		}
	}
	return nil
}

// withOutputLimit writes the clamped output limit into the field(s) the
// caller used, max_tokens when it used neither, and re-encodes the body.
func (c chatRequest) withOutputLimit(limit int) ([]byte, error) {
	set := false
	for _, f := range []string{"max_tokens", "max_completion_tokens"} {
		if raw, ok := c.fields[f]; ok && string(raw) != "null" {
			c.fields[f] = json.RawMessage(strconv.Itoa(limit))
			set = true
		}
	}
	if !set {
		c.fields["max_tokens"] = json.RawMessage(strconv.Itoa(limit))
	}
	return json.Marshal(c.fields)
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

// keyFor returns the virtual key of the task's current run, policy version
// and UTC day, or mints one: limited to the aliases the task's classes
// allow (so LiteLLM refuses any other target) and capped at the hold's
// headroom, the smaller of the task's and the day's. The key counts the
// call as in flight until doneWith.
func (s *Server) keyFor(ctx context.Context, slot string, task Task, saUID string, models []string, pv *policy.Version, hold policy.Hold) (*liveKey, error) {
	want := liveKey{saUID: saUID, version: pv.ID, day: s.now().UTC().Format(policy.DateLayout), bound: hold.Bound}
	// a key minted here and superseded before this call takes it is rare
	// (a version or day change within the mint); three rounds cover it
	for range 3 {
		if k := s.acquire(slot, want); k != nil {
			return k, nil
		}
		// one mint per run at a time, and no task waits on another's mint
		_, err, _ := s.mints.Do(fmt.Sprintf("%s\x00%s\x00%d\x00%s", slot, saUID, pv.ID, want.day), func() (any, error) {
			s.mu.Lock()
			fresh := s.matches(s.keysByTask[slot], want)
			s.mu.Unlock()
			if fresh {
				return nil, nil
			}
			ctx := context.WithoutCancel(ctx)
			key, err := s.opts.Minter.Mint(ctx, task.ID, hold.Headroom, models)
			if err != nil {
				return nil, fmt.Errorf("mint virtual key: %w", err)
			}
			k := want
			k.key = key
			s.mu.Lock()
			old := s.keysByTask[slot]
			s.keysByTask[slot] = &k
			idle := old != nil && old.inFlight == 0
			if old != nil {
				old.retired = true
			}
			s.mu.Unlock()
			if idle {
				s.block(slot, old)
			}
			return nil, nil
		})
		if err != nil {
			return nil, err
		}
	}
	return nil, errors.New("mint virtual key: superseded while minting")
}

func (s *Server) matches(k *liveKey, want liveKey) bool {
	return k != nil && k.saUID == want.saUID && k.version == want.version && k.day == want.day
}

// acquire returns the slot's current key if it matches want, counting one
// more call in flight on it.
func (s *Server) acquire(slot string, want liveKey) *liveKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.keysByTask[slot]
	if !s.matches(k, want) {
		return nil
	}
	k.inFlight++
	return k
}

// doneWith ends a call on k; the last call on a retired key blocks it.
func (s *Server) doneWith(slot string, k *liveKey) {
	s.mu.Lock()
	k.inFlight--
	last := k.retired && k.inFlight == 0
	s.mu.Unlock()
	if last {
		s.block(slot, k)
	}
}

func (s *Server) block(slot string, k *liveKey) {
	if err := s.opts.Minter.Block(context.Background(), k.key); err != nil {
		log.Printf("key slot %s: block superseded key (version %d, day %s): %v", slot, k.version, k.day, err)
	}
}

// modelCall is one admitted call: its hold is released exactly once.
type modelCall struct {
	pid      *int64
	task     Task
	hold     policy.Hold
	alias    string
	kind     string // the factoryd purpose, "" for an agent's call
	slot     string // the key slot: the task, or the task's factoryd slot
	key      *liveKey
	overHold string        // the booking's flag, on every audit row after it
	up       *upstreamCall // nil until the call goes upstream
}

var zero = 0.0

// zeroCost is the cost of a call that never reached a provider.
var zeroCost = &zero

// finish releases the call's hold into the ledger, at cost or, when the
// cost is unknown (nil), at the hold's full amount, and audits the call
// unless it was refused on the way (decision ""). Booking runs on a
// background context: the billing happened, so it must be recorded even if
// the caller disconnected.
func (s *Server) finish(c *modelCall, cost *float64, decision string) {
	cost = policy.KnownCost(cost)
	reason := ""
	if cost == nil {
		reason = fmt.Sprintf("cost not reported, booked the hold %.6f EUR", c.hold.EUR)
	}
	b, err := s.opts.Store.ReleaseHold(context.Background(), c.hold, cost)
	if err != nil {
		reason = fmt.Sprintf("hold %d not released: %v", c.hold.ID, err)
		log.Printf("task %s: %s", c.task.ID, reason)
		b.Spent = c.task.SpentEUR
	}
	c.overHold = b.OverHold
	if c.up != nil {
		c.up.booked(b.Booked, b.OverHold)
	}
	if decision == "" {
		return
	}
	s.auditCall(c, decision, reason, &b.Spent)
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, c *modelCall, key string, body []byte, stream bool) {
	ctx, up := s.tel.startCall(r.Context(), c.task, "model", c.alias, c.hold.EUR)
	c.up = up
	defer up.end()
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.opts.LiteLLMURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		s.finish(c, zeroCost, "")
		s.refuse(w, c.pid, c.task.ID, c.kind, "error_upstream", err.Error(), c.alias, http.StatusBadGateway, "upstream_error")
		return
	}
	upReq.Header.Set("Content-Type", contentType(r))
	upReq.Header.Set("Authorization", "Bearer "+key)

	res, err := s.http.Do(upReq)
	if err != nil {
		// the provider may have run part of the call: book the hold
		s.finish(c, nil, "")
		s.refuse(w, c.pid, c.task.ID, c.kind, "error_upstream", err.Error(), c.alias, http.StatusBadGateway, "upstream_error")
		return
	}
	defer res.Body.Close()
	up.status = strconv.Itoa(res.StatusCode)

	if res.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		// a failure LiteLLM still priced (partial provider work) is booked;
		// an unpriced one is not, or every transient 503 would burn budget
		cost := zeroCost
		if v, err := strconv.ParseFloat(res.Header.Get("x-litellm-response-cost"), 64); err == nil {
			cost = &v
		}
		s.finish(c, cost, "")
		if res.StatusCode == http.StatusTooManyRequests && isBudgetExceeded(respBody) {
			var liteErr struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			json.Unmarshal(respBody, &liteErr) // #nosec G104 -- best-effort message extraction; an empty liteErr.Error.Message on failure still refuses the spend
			// the key's max_budget was the task's or the day's headroom
			reason := "litellm budget_exceeded: " + liteErr.Error.Message
			s.auditCall(c, c.key.bound, reason, &c.task.SpentEUR)
			w.Header().Set(HeaderDecision, c.key.bound)
			writeError(w, http.StatusPaymentRequired, "budget_exceeded", reason)
			return
		}
		s.auditCall(c, "error_upstream", fmt.Sprintf("upstream %d", res.StatusCode), &c.task.SpentEUR)
		w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
		w.WriteHeader(res.StatusCode)
		w.Write(respBody) // #nosec G104 -- best-effort response write, client disconnect is the only failure mode
		return
	}

	w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
	w.WriteHeader(res.StatusCode)

	// Each call's own cost is booked onto the task. x-litellm-key-spend is
	// not: it is a cached read that lags, and a stream's headers leave before
	// its cost exists. A stream's cost arrives in its final usage chunk; a
	// stream the client broke off never shows it, and books the hold.
	var cost *float64
	if stream {
		usage := &usageScanner{}
		s.stream(w, io.TeeReader(res.Body, usage))
		if usage.seen {
			cost = &usage.cost
		}
	} else {
		s.stream(w, res.Body)
		if v, err := strconv.ParseFloat(res.Header.Get("x-litellm-response-cost"), 64); err == nil {
			cost = &v
		}
	}
	s.finish(c, cost, "allowed")
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
func (s *Server) refuseSpend(w http.ResponseWriter, pid *int64, task Task, kind, decision, alias, reason string) {
	s.audit(pid, task.ID, kind, decision, reason, alias, &task.SpentEUR)
	w.Header().Set(HeaderDecision, decision)
	writeError(w, http.StatusPaymentRequired, "budget_exceeded", reason)
}

func (s *Server) refuse(w http.ResponseWriter, pid *int64, taskID, kind, decision, reason, alias string, status int, typ string) {
	s.audit(pid, taskID, kind, decision, reason, alias, nil)
	w.Header().Set(HeaderDecision, decision)
	writeError(w, status, typ, reason)
}

// auditCall audits an admitted call with its output limit and hold.
func (s *Server) auditCall(c *modelCall, decision, reason string, spend *float64) {
	if err := s.opts.Store.InsertAudit(context.Background(), AuditRow{
		TaskID: c.task.ID, Kind: c.kind, Decision: decision, Reason: reason, ModelAlias: c.alias, SpendEUR: spend,
		PolicyVersion: c.pid, OutputLimit: &c.hold.MaxOut, HoldEUR: &c.hold.EUR, OverHold: c.overHold,
	}); err != nil {
		log.Printf("insert audit row (task %s, decision %s): %v", c.task.ID, decision, err)
	}
}

// audit persists the decision on a background context: audit rows are the
// record of what the gateway did and must survive the caller disconnecting.
func (s *Server) audit(pid *int64, taskID, kind, decision, reason, alias string, spend *float64) {
	if err := s.opts.Store.InsertAudit(context.Background(), AuditRow{
		TaskID:        taskID,
		Kind:          kind,
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
