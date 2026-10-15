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
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// Authenticator is implemented by Auth; tests substitute a fake.
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

// KeyMinter mints a per-task LiteLLM virtual key for a slot (the task, or
// the task's factoryd slot) limited to models, and blocks a superseded
// one. Implemented by LiteLLM; tests substitute a fake.
type KeyMinter interface {
	Mint(ctx context.Context, slot, taskID string, models []string) (string, error)
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

// cheaperThan: alias costs less per token than session, the session's
// alias, on some rate and more on none, so a call on it is bounded by the
// session's hold. An alias without a price, or priced the same, is not.
func (s *Server) cheaperThan(ctx context.Context, alias, session string) bool {
	a, err := s.opts.Prices.Price(ctx, alias)
	if err != nil {
		return false
	}
	b, err := s.opts.Prices.Price(ctx, session)
	if err != nil {
		return false
	}
	return a.In <= b.In && a.Out <= b.Out && a.CacheRead <= b.CacheRead && (a.In < b.In || a.Out < b.Out)
}

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
	// PlanFiler is the file_plan tool's hand-over to factoryd.
	PlanFiler PlanFiler
	// Delegator is the delegate tool's hand-over to factoryd.
	Delegator Delegator
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
	// HeaderFactoryThread names the chat thread a factoryd call serves
	// (roadmap 239): its audit row carries it, the thread's token read.
	HeaderFactoryThread = "X-Factory-Thread"
)

// RefusedContext is the decision of a call the provider refused for a
// prompt past its context window: typed, so the caller compacts and
// retries (roadmap 239).
const RefusedContext = "refused_context"

// maxThread bounds the thread a call names.
const maxThread = 200

// factorydSA stands for the service account uid of factoryd's key slot.
const factorydSA = "factoryd"

// statusClientCanceled is nginx's "client closed request"; net/http defines
// no constant for it. It answers a call whose caller is already gone
// instead of 5xx, so an alert on 5xx-rate never counts a caller's own
// disconnect as an upstream fault.
const statusClientCanceled = 499

// auditFactoryd is the audit kind of a factoryd call refused before its
// purpose is known; it is no purpose.
const auditFactoryd = "factoryd"

// liveKey is a task's virtual key and what it was minted for: a new run
// or policy version mints a new key (its models follow from the version)
// and retires this one. A retired key is blocked once the calls still
// using it are done, so none of them fails on the block. A key carries no
// budget: the gateway's holds are the budget, and LiteLLM's proxy-wide
// max_budget the backstop.
type liveKey struct {
	key      string
	saUID    string
	version  int64
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
	// a row this binary cannot read as a policy (one from before the
	// configuration tree) is no policy, not an empty one
	if err := v.Document.Validate(); err != nil {
		return nil, fmt.Errorf("policy version %d: %w", v.ID, err)
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
	return telemetry.Handler(s.tel.tp, "factory-gateway", mux)
}

// bodySlack covers the bytes the output limit adds to the body after the
// hold is taken.
const bodySlack = 64

// handleChat admits a call only if its worst case fits the task's and the
// day's free headroom: the output limit is clamped to what the headroom can
// pay for at the alias's LiteLLM price, and that worst case is held until
// the call's cost is booked. A call must leave room for its output: a
// reasoning model spends tokens thinking before it answers, so a call that
// cannot fit policy.MinCallTokens is refused.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	pv, polErr := s.currentPolicy(ctx)
	var pid *int64
	if pv != nil {
		pid = &pv.ID
	}

	id, purpose, err := s.identify(r)
	if err != nil {
		s.refuse(w, pid, id.TaskID, purpose, "refused_auth", err.Error(), "", http.StatusUnauthorized, "authentication_error")
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
	// a stronger model comes only by the climb; a cheaper alias of the
	// class (a subagent's, roadmap 222, item 6) runs on the session's key
	if purpose == "" && chatReq.Model != task.ModelAlias && !s.cheaperThan(ctx, chatReq.Model, task.ModelAlias) {
		s.refuse(w, pid, id.TaskID, purpose, "refused_capability",
			fmt.Sprintf("model %q is not the model %q of the task's session (tasks.model_alias); a stronger model comes only by the climb", chatReq.Model, task.ModelAlias),
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
	if err == nil {
		price, err = pv.Document.Limited(chatReq.Model, price)
	}
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
	project, projectEUR := pv.ProjectCap(task.Repository)
	hold, err := s.opts.Store.TakeHold(ctx, policy.HoldRequest{
		TaskID: task.ID, Kind: policy.KindModel, Ref: ref, Session: id.Session,
		Bytes: len(body) + bodySlack, Price: price, WantOut: chatReq.WantOut,
		DayEUR:   pv.Document.Budget.DayEUR,
		MonthEUR: pv.Document.Budget.MonthEUR,
		Project:  project, ProjectEUR: projectEUR,
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
	call := &modelCall{pid: pid, task: task, hold: hold, alias: chatReq.Model, kind: purpose,
		slot: slot, saUID: id.SAUID, models: models, pv: pv, started: s.now()}
	if purpose != "" && purpose != auditFactoryd {
		// read only behind factoryd's token: a sandbox names no thread
		if th := r.Header.Get(HeaderFactoryThread); len(th) <= maxThread {
			call.thread = th
		}
	}

	if body, err = chatReq.withOutputLimit(hold.MaxOut); err != nil {
		s.finish(call, zeroCost, "")
		s.refuse(w, pid, id.TaskID, purpose, "refused_body", err.Error(), chatReq.Model, http.StatusBadRequest, "invalid_request_error")
		return
	}
	key, err := s.keyFor(ctx, slot, task, id.SAUID, models, pv)
	if err != nil {
		s.finish(call, zeroCost, "")
		if r.Context().Err() != nil {
			s.refuse(w, pid, id.TaskID, purpose, "client_canceled", err.Error(), chatReq.Model, statusClientCanceled, "client_canceled")
			return
		}
		s.refuse(w, pid, id.TaskID, purpose, "error_mint", err.Error(), chatReq.Model, http.StatusBadGateway, "upstream_error")
		return
	}
	call.key = key
	// the key can be re-minted mid-call (a blocked key); end the call on
	// whichever key it finished with
	defer func() {
		if call.key != nil {
			s.doneWith(slot, call.key)
		}
	}()

	s.proxy(w, r, call, body, chatReq.Stream)
}

// identify is the caller: factoryd by the gateway-factoryd token, with
// the task and one of policy.Purposes in its headers, or a sandbox by its
// ServiceAccount JWT (purpose ""), whose headers name nothing. A factoryd
// call without a known purpose is refused as kind auditFactoryd.
func (s *Server) identify(r *http.Request) (Identity, string, error) {
	raw, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if s.opts.FactorydToken == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(raw)), []byte(s.opts.FactorydToken)) != 1 {
		id, err := s.opts.Auth.Authenticate(r)
		return id, "", err
	}
	id := Identity{TaskID: r.Header.Get(HeaderFactoryTask), SAUID: factorydSA}
	purpose := r.Header.Get(HeaderFactoryPurpose)
	if id.TaskID == "" || !slices.Contains(policy.Purposes, purpose) {
		return id, auditFactoryd, fmt.Errorf("a factoryd call names its task in %s and one of %q in %s", HeaderFactoryTask, policy.Purposes, HeaderFactoryPurpose)
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

// keyFor returns the virtual key of the task's current run and policy
// version, or mints one, limited to the aliases the task's classes allow
// (so LiteLLM refuses any other target). The key counts the call as in
// flight until doneWith.
func (s *Server) keyFor(ctx context.Context, slot string, task Task, saUID string, models []string, pv *policy.Version) (*liveKey, error) {
	want := liveKey{saUID: saUID, version: pv.ID}
	// a key minted here and superseded before this call takes it is rare
	// (a version change within the mint); three rounds cover it
	for range 3 {
		if k := s.acquire(slot, want); k != nil {
			return k, nil
		}
		// one mint per run at a time, and no task waits on another's mint
		_, err, _ := s.mints.Do(fmt.Sprintf("%s\x00%s\x00%d", slot, saUID, pv.ID), func() (any, error) {
			s.mu.Lock()
			fresh := s.matches(s.keysByTask[slot], want)
			s.mu.Unlock()
			if fresh {
				return nil, nil
			}
			ctx := context.WithoutCancel(ctx)
			key, err := s.opts.Minter.Mint(ctx, slot, task.ID, models)
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
	return k != nil && k.saUID == want.saUID && k.version == want.version
}

// dropKey removes the slot's cached key after LiteLLM answered that it is
// blocked, so the next keyFor mints. A key another call already replaced is
// left in place.
func (s *Server) dropKey(slot string, k *liveKey) {
	s.mu.Lock()
	if s.keysByTask[slot] == k {
		delete(s.keysByTask, slot)
	}
	s.mu.Unlock()
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
		log.Printf("key slot %s: block superseded key (version %d): %v", slot, k.version, err)
	}
}

// modelCall is one admitted call: its hold is released exactly once.
type modelCall struct {
	pid        *int64
	task       Task
	hold       policy.Hold
	alias      string
	kind       string // the factoryd purpose, "" for an agent's call
	thread     string // the chat thread a factoryd call serves, "" for none
	slot       string // the key slot: the task, or the task's factoryd slot
	saUID      string // what the slot's key was minted for, to re-mint it
	models     []string
	pv         *policy.Version
	key        *liveKey
	overHold   string        // the booking's flag, on every audit row after it
	deployment string        // the LiteLLM deployment that answered
	up         *upstreamCall // nil until the call goes upstream
	usage      policy.Usage  // the provider's, booked with the cost
	// the call row's own truth (roadmap 198 item 1): when it was admitted
	// and what it ended with — the upstream status once it answered, the
	// modeled cost finish booked (nil until then, and when none was
	// reported) and how long it ran.
	started  time.Time
	status   string
	callCost *float64
	duration time.Duration
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
	c.callCost, c.duration = cost, s.now().Sub(c.started)
	reason := ""
	if cost == nil {
		reason = fmt.Sprintf("cost not reported, booked the hold %.6f EUR", c.hold.EUR)
	}
	b, err := s.opts.Store.ReleaseHold(context.Background(), c.hold, cost, c.usage)
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

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, c *modelCall, body []byte, stream bool) {
	ctx, up := s.tel.startCall(r.Context(), c.task, "model", c.alias, c.hold.EUR)
	c.up = up
	defer up.end()
	c.usage.BodyBytes = len(body)

	// a cached key can have been blocked under this call (factoryd's
	// reconcile blocks the task's keys): drop it, mint a fresh one and
	// retry once, so a blocked key never turns a call into a refusal
	for attempt := 0; ; attempt++ {
		upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.opts.LiteLLMURL+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			s.finish(c, zeroCost, "")
			s.refuse(w, c.pid, c.task.ID, c.kind, "error_upstream", err.Error(), c.alias, http.StatusBadGateway, "upstream_error")
			return
		}
		upReq.Header.Set("Content-Type", contentType(r))
		upReq.Header.Set("Authorization", "Bearer "+c.key.key)

		res, err := s.http.Do(upReq)
		if err != nil {
			// the provider may have run part of the call: book the hold
			s.finish(c, nil, "")
			if r.Context().Err() != nil {
				s.refuse(w, c.pid, c.task.ID, c.kind, "client_canceled", err.Error(), c.alias, statusClientCanceled, "client_canceled")
				return
			}
			s.refuse(w, c.pid, c.task.ID, c.kind, "error_upstream", err.Error(), c.alias, http.StatusBadGateway, "upstream_error")
			return
		}
		up.status = strconv.Itoa(res.StatusCode)
		c.status = up.status
		c.deployment = res.Header.Get(HeaderDeployment)
		up.deployment = c.deployment

		if res.StatusCode/100 != 2 {
			respBody, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
			_ = res.Body.Close()
			if res.StatusCode == http.StatusUnauthorized && attempt == 0 && isKeyBlocked(respBody) {
				if err := s.rekey(ctx, c); err != nil {
					s.finish(c, zeroCost, "")
					if r.Context().Err() != nil {
						s.refuse(w, c.pid, c.task.ID, c.kind, "client_canceled", err.Error(), c.alias, statusClientCanceled, "client_canceled")
						return
					}
					s.refuse(w, c.pid, c.task.ID, c.kind, "error_mint", err.Error(), c.alias, http.StatusBadGateway, "upstream_error")
					return
				}
				continue
			}
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
				// no key has a budget: LiteLLM's one is the proxy-wide stop
				decision, reason := policy.RefusedMonthCap, "litellm proxy budget exceeded (litellm_settings.max_budget, the proxy-wide monthly stop behind budget.month_eur): "+liteErr.Error.Message
				s.auditCall(c, decision, reason, &c.task.SpentEUR)
				w.Header().Set(HeaderDecision, decision)
				writeError(w, http.StatusPaymentRequired, "budget_exceeded", reason)
				return
			}
			if isContextOverflow(res.StatusCode, respBody) {
				reason := "the prompt is past the model's context window: " + upstreamMessage(respBody)
				s.auditCall(c, RefusedContext, reason, &c.task.SpentEUR)
				w.Header().Set(HeaderDecision, RefusedContext)
				writeError(w, http.StatusBadRequest, "context_length_exceeded", reason)
				return
			}
			s.auditCall(c, "error_upstream", fmt.Sprintf("upstream %d", res.StatusCode), &c.task.SpentEUR)
			w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
			w.WriteHeader(res.StatusCode)
			w.Write(respBody) // #nosec G104 -- best-effort response write, client disconnect is the only failure mode
			return
		}

		defer res.Body.Close()

		w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
		if c.kind != "" && c.deployment != "" {
			w.Header().Set(HeaderDeployment, c.deployment)
		}
		w.WriteHeader(res.StatusCode)

		// Each call's own cost is booked onto the task. x-litellm-key-spend is
		// not: it is a cached read that lags, and a stream's headers leave before
		// its cost exists. A stream's cost arrives in its final usage chunk; a
		// stream the client broke off never shows it, and books the hold.
		// Any other answer is one JSON document with its usage.
		scan := &usageScanner{doc: !stream}
		s.stream(w, io.TeeReader(res.Body, scan))
		var cost *float64
		if stream {
			cost = scan.usage.Cost
		} else {
			scan.parse(scan.pending)
			if v, err := strconv.ParseFloat(res.Header.Get("x-litellm-response-cost"), 64); err == nil {
				cost = &v
			}
		}
		c.usage.PromptTokens, c.usage.CachedTokens, c.usage.OutputTokens = scan.usage.Prompt, scan.usage.Details.Cached, scan.usage.Output
		s.finish(c, cost, "allowed")
		return
	}
}

// isKeyBlocked reports whether LiteLLM refused the key because it is blocked
// (a reconcile or an admin blocked it between two calls).
func isKeyBlocked(body []byte) bool {
	return bytes.Contains(body, []byte("Key is blocked"))
}

// rekey retires the slot's blocked cached key and mints a fresh one for the
// same call, holding the same hold. It is the retry path of a 401 "Key is
// blocked": the old key is dropped from the cache and its in-flight count
// released, then keyFor mints through the regular path.
func (s *Server) rekey(ctx context.Context, c *modelCall) error {
	old := c.key
	s.dropKey(c.slot, old)
	s.doneWith(c.slot, old)
	c.key = nil
	key, err := s.keyFor(ctx, c.slot, c.task, c.saUID, c.models, c.pv)
	if err != nil {
		return err
	}
	c.key = key
	return nil
}

// HeaderDeployment names the deployment that answered (its litellm_params
// model, e.g. qwen3.8-max). LiteLLM rewrites the body's model to the alias
// the client asked for, so the header is the only place it shows; the
// gateway passes it on to factoryd's calls.
const HeaderDeployment = "x-litellm-model-name"

// maxSSELine bounds what usageScanner buffers of one line. The usage chunk
// is small; a longer line is content, passed through but not inspected.
const maxSSELine = 1 << 20

// usage is a completion's usage as LiteLLM reports it: the provider's
// tokens and, in a stream, LiteLLM's cost.
type usage struct {
	Cost    *float64 `json:"cost"`
	Prompt  int      `json:"prompt_tokens"`
	Output  int      `json:"completion_tokens"`
	Details struct {
		Cached int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// usageScanner watches an SSE stream for the usage chunk, or with doc
// keeps a JSON answer, up to maxSSELine, to parse once it ends.
type usageScanner struct {
	doc      bool
	pending  []byte
	skipping bool // the current line outgrew maxSSELine
	usage    usage
}

func (u *usageScanner) Write(p []byte) (int, error) {
	n := len(p)
	if u.doc {
		if !u.skipping {
			u.pending = append(u.pending, p...)
			if len(u.pending) > maxSSELine {
				u.pending, u.skipping = nil, true
			}
		}
		return n, nil
	}
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
	if data, ok := bytes.CutPrefix(bytes.TrimSpace(l), []byte("data:")); ok {
		u.parse(data)
	}
}

func (u *usageScanner) parse(data []byte) {
	if !bytes.Contains(data, []byte(`"usage"`)) {
		return
	}
	var ev struct {
		Usage *usage `json:"usage"`
	}
	if json.Unmarshal(data, &ev) == nil && ev.Usage != nil {
		u.usage = *ev.Usage
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

// contextOverflow are the words a provider's or LiteLLM's refusal of a
// prompt past the model's window carries: LiteLLM's mapped
// ContextWindowExceededError, OpenAI's and DashScope's code and sentence,
// Anthropic's, and the GLM family's.
var contextOverflow = []string{"contextwindowexceedederror", "context_length_exceeded", "maximum context length",
	"prompt is too long", "input is too long", "exceeds the model's maximum context", "context window", "range of input length"}

// isContextOverflow reports whether an upstream refusal is one of a
// prompt past the model's window: a 400, 413 or 422 that names it.
func isContextOverflow(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusRequestEntityTooLarge && status != http.StatusUnprocessableEntity {
		return false
	}
	lower := strings.ToLower(string(body))
	return slices.ContainsFunc(contextOverflow, func(w string) bool { return strings.Contains(lower, w) })
}

// upstreamMessage is an upstream error body's message, the body itself
// cut when it is no error object.
func upstreamMessage(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return strings.TrimSpace(string(body[:min(len(body), 500)]))
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

// refuseSpend writes a 402 refusal: the task's budget, the day or the
// month cap. The
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

// auditCall audits an admitted call with its output limit and hold, and
// the call row's own truth: the usage the provider reported, the modeled
// cost it was booked at (nil when none was reported), how long it ran and
// the status the upstream answered.
func (s *Server) auditCall(c *modelCall, decision, reason string, spend *float64) {
	if err := s.opts.Store.InsertAudit(context.Background(), AuditRow{
		TaskID: c.task.ID, Kind: c.kind, Thread: c.thread, Decision: decision, Reason: reason, ModelAlias: c.alias, SpendEUR: spend,
		PolicyVersion: c.pid, OutputLimit: &c.hold.MaxOut, HoldEUR: &c.hold.EUR, OverHold: c.overHold, Deployment: c.deployment,
		PromptTokens: c.usage.PromptTokens, CachedTokens: c.usage.CachedTokens, OutputTokens: c.usage.OutputTokens,
		CallCostEUR: c.callCost, DurationMS: int(c.duration.Milliseconds()), Status: c.status,
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
