package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// Escalator hands an agent's question to the running session's supervision.
// Implemented by Factoryd; tests substitute a fake.
type Escalator interface {
	Escalate(ctx context.Context, taskID string, e Escalation) error
}

// Escalation is what an agent asks: a human (escalate_to_human: the
// question, why, what the agent would do and the answers it offers) or the
// issue's author (request_info: the question, what it tried, what it
// needs).
type Escalation struct {
	Kind           string   `json:"kind"`
	Question       string   `json:"question"`
	Reason         string   `json:"reason,omitempty"`
	Recommendation string   `json:"recommendation,omitempty"`
	Choices        []string `json:"choices,omitempty"`
	Tried          string   `json:"tried,omitempty"`
	Needs          string   `json:"needs,omitempty"`
}

// Escalation kinds.
const (
	KindEscalation  = "escalation"
	KindInfoRequest = "info_request"
)

var _ Escalator = (*Factoryd)(nil)

// Factoryd is the gateway's client to factoryd's internal endpoint.
type Factoryd struct {
	BaseURL string // http://factoryd.factory.svc.cluster.local:8080
	Token   string // the shared bearer (secret factory/gateway-factoryd)
	HTTP    *http.Client
}

func (f *Factoryd) Escalate(ctx context.Context, taskID string, e Escalation) error {
	_, err := call(ctx, f.HTTP, http.MethodPost, f.BaseURL+"/internal/escalations",
		http.Header{"Authorization": {"Bearer " + f.Token}},
		struct {
			TaskID string `json:"task_id"`
			Escalation
		}{taskID, e})
	return err
}

// toolCaller is who an MCP request is for: the task, read per request, and
// the policy version it is decided under.
type toolCaller struct {
	pv   *policy.Version
	task Task
}

type callerKey struct{}

// mcpHandler serves the MCP tools (streamable HTTP, stateless: every
// request stands alone, so any gateway replica serves it) behind the same
// task-JWT check and task lookup as the model path.
func (s *Server) mcpHandler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		c, _ := r.Context().Value(callerKey{}).(*toolCaller)
		return s.toolServer(c)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		pv, polErr := s.currentPolicy(ctx)
		var pid *int64
		if pv != nil {
			pid = &pv.ID
		}
		id, err := s.opts.Auth.Authenticate(r)
		if err != nil {
			s.refuse(w, pid, id.TaskID, "", "refused_auth", err.Error(), "", http.StatusUnauthorized, "authentication_error")
			return
		}
		caller(ctx, id.TaskID)
		if polErr != nil {
			s.refuse(w, pid, id.TaskID, "", "error_policy", polErr.Error(), "", http.StatusServiceUnavailable, "server_error")
			return
		}
		task, ok, err := s.opts.Store.Task(ctx, id.TaskID)
		if err != nil {
			s.refuse(w, pid, id.TaskID, "", "error_store", err.Error(), "", http.StatusInternalServerError, "server_error")
			return
		}
		if !ok {
			s.refuse(w, pid, id.TaskID, "", "refused_task_unknown", "no task row for "+id.TaskID, "", http.StatusNotFound, "invalid_request_error")
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(ctx, callerKey{}, &toolCaller{pv: pv, task: task})))
	})
}

type ScrapeIn struct {
	URL string `json:"url" jsonschema:"the http(s) page to fetch"`
}

type SearchIn struct {
	Query string `json:"query" jsonschema:"the web search query"`
	Limit int    `json:"limit,omitempty" jsonschema:"results to return, 1 to 10 (default 5)"`
}

type ReplicateIn struct {
	Model   string         `json:"model" jsonschema:"the model as owner/name"`
	Version string         `json:"version,omitempty" jsonschema:"the 64-hex version id; the model's latest when empty"`
	Input   map[string]any `json:"input" jsonschema:"the model's input object"`
}

type FalIn struct {
	Endpoint string         `json:"endpoint" jsonschema:"the fal endpoint id, e.g. fal-ai/any-llm"`
	Input    map[string]any `json:"input" jsonschema:"the endpoint's input object"`
}

type EscalateIn struct {
	Question       string `json:"question" jsonschema:"what the human must answer or decide"`
	Reason         string `json:"reason" jsonschema:"why the task cannot go on without it"`
	Recommendation string `json:"recommendation" jsonschema:"what you would do and why, at most 2000 bytes"`
	// factoryd checks the choices where it stores them: a list it cannot
	// offer is dropped, never refused
	Choices []string `json:"choices,omitempty" jsonschema:"offer 2 to 5 short answers the human can pick with one click; always leave free text possible"`
}

type RequestInfoIn struct {
	Question string `json:"question" jsonschema:"what you ask the issue's author"`
	Tried    string `json:"tried" jsonschema:"what you tried to reproduce or find the fault"`
	Needs    string `json:"needs" jsonschema:"what information would let you go on"`
}

// escalateSchema is EscalateIn's input schema with choices a plain array:
// the inferred type list ["null", "array"] is one some models' tool calls
// refuse.
var escalateSchema = func() *jsonschema.Schema {
	s, err := jsonschema.For[EscalateIn](&jsonschema.ForOptions{TypeSchemas: map[reflect.Type]*jsonschema.Schema{
		reflect.TypeFor[[]string](): {Type: "array", Items: &jsonschema.Schema{Type: "string"}}}})
	if err != nil {
		panic(err)
	}
	return s
}()

// maxQuestion bounds an escalation's question and reason and an
// information request's fields; maxRecommendation the recommendation.
const (
	maxQuestion       = 4000
	maxRecommendation = 2000
)

// maxChoices and maxChoicesBytes bound the choices passed on to factoryd,
// which checks each one where it stores them.
const (
	maxChoices      = 5
	maxChoicesBytes = 1 << 10
)

// toolServer is the MCP server of one request. Every tool is listed; a
// call the task's classes, caps or headroom do not allow is refused with
// the reason, as a tool error the agent reads.
func (s *Server) toolServer(c *toolCaller) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "factory-tools", Version: "1"}, nil)
	caps := " Paid and capped per task; a refusal names the cap."
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolWebScrape, Description: "Fetch one web page as markdown (Firecrawl)." + caps},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ScrapeIn) (*mcp.CallToolResult, any, error) {
			return s.paid(ctx, c, policy.ToolWebScrape, s.scrape(c, in)), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolWebSearch, Description: "Search the web: url, title and description per result (Firecrawl)." + caps},
		func(ctx context.Context, _ *mcp.CallToolRequest, in SearchIn) (*mcp.CallToolResult, any, error) {
			return s.paid(ctx, c, policy.ToolWebSearch, s.search(c, in)), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolReplicate, Description: "Run a Replicate model version on an input and wait for its output (at most about a minute)." + caps},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ReplicateIn) (*mcp.CallToolResult, any, error) {
			return s.paid(ctx, c, policy.ToolReplicate, s.replicate(c, in)), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolFal, Description: "Run a fal.ai endpoint on an input and return its output." + caps},
		func(ctx context.Context, _ *mcp.CallToolRequest, in FalIn) (*mcp.CallToolResult, any, error) {
			return s.paid(ctx, c, policy.ToolFal, s.fal(c, in)), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolEscalate, Description: "Ask a human when the task cannot be completed without a decision, access or information only a human has. " +
		"This session ends; the task resumes with the human's answer. Free.", InputSchema: escalateSchema},
		func(ctx context.Context, _ *mcp.CallToolRequest, in EscalateIn) (*mcp.CallToolResult, any, error) {
			return s.escalate(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolRequestInfo, Description: "Ask the issue's author for information when you could not reproduce the problem and found no fault: " +
		"what you tried and what you need. This session ends; the task resumes with the author's answer, or the issue closes as not reproducible after 14 days. Free."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in RequestInfoIn) (*mcp.CallToolResult, any, error) {
			return s.requestInfo(ctx, c, in), nil, nil
		})
	if s.opts.Cloud != nil {
		s.addCloudTools(srv, c)
	}
	srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if list, ok := res.(*mcp.ListToolsResult); ok {
				list.Tools = slices.DeleteFunc(list.Tools, func(t *mcp.Tool) bool { return !c.allows(t.Name) })
			}
			return res, err
		}
	})
	return srv
}

// allows is the one admission check of a tool, for tools/list and for
// every call: one of the task's classes lists it.
func (c *toolCaller) allows(tool string) bool {
	return c.pv.Document.AllowsTool(c.task.Classes, tool)
}

// paidCall is one paid tool call. prepare validates the arguments and
// prices the call's worst case, before any hold: an error refuses the
// call (refused_arguments or refused_price). run calls the provider and
// returns the agent's text and the cost, nil when unknown.
type paidCall struct {
	prepare func(ctx context.Context) (worst float64, refusal, reason string)
	run     func(ctx context.Context) (text string, cost *float64, err error)
}

// paid runs a paid tool call: the task is running, one of its classes
// allows the tool, its worst case is priced, held against the tool's caps
// and the task and day headroom, then the provider is called and the real
// cost (the hold when unknown) booked with the task's spend, audited
// under the policy version.
func (s *Server) paid(ctx context.Context, c *toolCaller, tool string, p paidCall) *mcp.CallToolResult {
	if c.task.Status != "running" {
		return s.refuseTool(c, tool, "refused_task_status", "the task is "+c.task.Status)
	}
	if r := s.admitTool(c, tool); r != nil {
		return r
	}
	toolCap, _ := c.pv.Document.ToolCap(c.task.Classes, tool)
	worst, decision, reason := p.prepare(ctx)
	if decision != "" {
		return s.refuseTool(c, tool, decision, reason)
	}
	project, projectEUR := c.pv.ProjectCap(c.task.Repository)
	hold, err := s.opts.Store.TakeHold(ctx, policy.HoldRequest{
		TaskID: c.task.ID, Kind: policy.KindTool, Ref: tool, EUR: worst, Cap: toolCap,
		DayEUR:   c.pv.Document.Budget.DayEUR,
		MonthEUR: c.pv.Document.Budget.MonthEUR,
		Project:  project, ProjectEUR: projectEUR,
	})
	var refusal *policy.Refusal
	if errors.As(err, &refusal) {
		return s.refuseTool(c, tool, refusal.Decision, refusal.Reason)
	}
	if err != nil {
		s.auditTool(c, tool, "error_store", err.Error(), nil, nil)
		return toolError("the factory could not reserve the call's cost: " + err.Error())
	}
	ctx, up := s.tel.startCall(ctx, c.task, "tool", tool, hold.EUR)
	defer up.end()
	// a disconnecting agent must not cut a call the provider bills; the
	// connectors' client timeouts bound it
	text, cost, callErr := p.run(context.WithoutCancel(ctx))
	up.status = toolStatus(callErr)
	decision, note := "allowed", ""
	if cost == nil {
		note = fmt.Sprintf("; cost not reported, booked the hold %.6f EUR", hold.EUR)
	}
	// the provider billed whether or not the agent is still listening; a
	// cost past the hold (the arguments were bounded to it) is booked in
	// full, so the caps refuse the next call from the true headroom
	b, err := s.opts.Store.ReleaseHold(context.Background(), hold, cost)
	if err != nil {
		note += fmt.Sprintf("; hold %d not released: %v", hold.ID, err)
		log.Printf("task %s: tool %s: hold %d not released: %v", c.task.ID, tool, hold.ID, err) // #nosec G706 -- the task id is the stored row's, found by the verified JWT; tool is one of the policy constants
		b.Spent = c.task.SpentEUR
	}
	up.booked(b.Booked, b.OverHold)
	spend := b.Spent
	if callErr != nil {
		s.auditBooked(c, tool, "error_tool", callErr.Error()+note, &spend, &hold.EUR, b.OverHold)
		return toolError(tool + " failed: " + callErr.Error())
	}
	s.auditBooked(c, tool, decision, strings.TrimPrefix(note, "; "), &spend, &hold.EUR, b.OverHold)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

// refuseTool audits a refusal and tells the agent it was the policy, not
// the tool.
func (s *Server) refuseTool(c *toolCaller, tool, decision, reason string) *mcp.CallToolResult {
	s.auditTool(c, tool, decision, reason, &c.task.SpentEUR, nil)
	return toolError("refused by the factory policy (" + decision + "): " + reason +
		". The tool did not run; do not retry this call.")
}

func (s *Server) auditTool(c *toolCaller, tool, decision, reason string, spend, hold *float64) {
	s.auditBooked(c, tool, decision, reason, spend, hold, "")
}

// auditBooked audits a tool call after its booking, with the booking's
// over-hold flag.
func (s *Server) auditBooked(c *toolCaller, tool, decision, reason string, spend, hold *float64, overHold string) {
	if err := s.opts.Store.InsertAudit(context.Background(), AuditRow{
		TaskID: c.task.ID, Decision: decision, Reason: reason, Tool: tool, SpendEUR: spend,
		PolicyVersion: &c.pv.ID, HoldEUR: hold, OverHold: overHold,
	}); err != nil {
		log.Printf("insert audit row (task %s, tool %s, decision %s): %v", c.task.ID, tool, decision, err) // #nosec G706 -- the task id is the stored row's, found by the verified JWT; tool and decision are constants
	}
}

func refusedArgs(reason string) (float64, string, string) { return 0, "refused_arguments", reason }
func refusedPrice(err error) (float64, string, string)    { return 0, "refused_price", err.Error() }

// perCredit books Firecrawl credits at the policy's price; nil credits
// (not reported) leave the cost unknown, a 4xx with none cost nothing.
func perCredit(eur float64, credits *int, err error) *float64 {
	if credits != nil {
		v := float64(*credits) * eur
		return &v
	}
	if billedNothing(err) {
		return &zero
	}
	return nil
}

// maxURL and maxQuery bound web_scrape's url and web_search's query.
const (
	maxURL   = 2048
	maxQuery = 1000
)

// publicTLD is a top-level label of the public DNS: letters, or an IDN.
var publicTLD = regexp.MustCompile(`^([a-z]{2,63}|xn--[a-z0-9-]{1,59})$`)

// localTLDs never resolve on the public internet.
var localTLDs = []string{"localhost", "local", "internal", "lan", "home", "corp", "arpa", "test", "invalid", "example"}

// publicURL refuses what Firecrawl can only fail on or must never be
// pointed at: IP literals (loopback, RFC 1918, link-local, metadata and
// their numeric spellings), single-label and cluster or private names.
func publicURL(raw string) error {
	if len(raw) > maxURL {
		return fmt.Errorf("url is longer than %d bytes", maxURL)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("url must be an absolute http(s) URL")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	labels := strings.Split(host, ".")
	tld := labels[len(labels)-1]
	if len(labels) < 2 || !publicTLD.MatchString(tld) || slices.Contains(localTLDs, tld) {
		return fmt.Errorf("url host %q is not a public DNS name (IP addresses, local and cluster names are refused)", u.Hostname())
	}
	return nil
}

func (s *Server) scrape(c *toolCaller, in ScrapeIn) paidCall {
	fc := c.pv.Document.Prices.Firecrawl
	return paidCall{
		prepare: func(context.Context) (float64, string, string) {
			if err := publicURL(in.URL); err != nil {
				return refusedArgs(err.Error())
			}
			return float64(fc.ScrapeCredits) * fc.EURPerCredit, "", ""
		},
		run: func(ctx context.Context) (string, *float64, error) {
			text, credits, err := s.opts.Firecrawl.Scrape(ctx, in.URL)
			return text, perCredit(fc.EURPerCredit, credits, err), err
		},
	}
}

// maxSearchResults bounds a search; Firecrawl bills 2 credits per 10.
const maxSearchResults = 10

func (s *Server) search(c *toolCaller, in SearchIn) paidCall {
	fc := c.pv.Document.Prices.Firecrawl
	limit := in.Limit
	if limit == 0 {
		limit = 5
	}
	return paidCall{
		prepare: func(context.Context) (float64, string, string) {
			if strings.TrimSpace(in.Query) == "" || len(in.Query) > maxQuery {
				return refusedArgs(fmt.Sprintf("query must be 1 to %d bytes", maxQuery))
			}
			if limit < 1 || limit > maxSearchResults {
				return refusedArgs(fmt.Sprintf("limit must be 1 to %d", maxSearchResults))
			}
			return float64(fc.SearchCredits) * fc.EURPerCredit, "", ""
		},
		run: func(ctx context.Context) (string, *float64, error) {
			text, credits, err := s.opts.Firecrawl.Search(ctx, in.Query, limit)
			return text, perCredit(fc.EURPerCredit, credits, err), err
		},
	}
}

func (s *Server) replicate(c *toolCaller, in ReplicateIn) paidCall {
	rp := c.pv.Document.Prices.Replicate
	var rate float64
	return paidCall{
		prepare: func(context.Context) (float64, string, string) {
			if !replicateModel.MatchString(in.Model) || (in.Version != "" && !replicateVersion.MatchString(in.Version)) {
				return refusedArgs("model must be owner/name and version, when given, a 64-hex version id")
			}
			if in.Input == nil {
				return refusedArgs("input must be a JSON object")
			}
			sku, ok := rp.Models[in.Model]
			if !ok {
				return 0, "refused_price", fmt.Sprintf("replicate model %s has no hardware in the policy (prices.replicate.models), so its cost cannot be bounded", in.Model)
			}
			if rate = rp.EURPerSecond[sku]; rate <= 0 {
				return 0, "refused_price", fmt.Sprintf("replicate hardware %s has no rate in the policy", sku)
			}
			return rate * rp.MaxSeconds, "", ""
		},
		// the version lookup runs under the hold: the shared key does no
		// work the task's request cap does not count
		run: func(ctx context.Context) (string, *float64, error) {
			version, err := s.opts.Replicate.Version(ctx, in.Model, in.Version)
			if err != nil {
				return "", &zero, err
			}
			text, seconds, err := s.opts.Replicate.Predict(ctx, version, in.Input, rp.MaxSeconds)
			var cost *float64
			switch {
			case seconds != nil:
				v := *seconds * rate
				cost = &v
			case billedNothing(err):
				cost = &zero
			}
			return text, cost, err
		},
	}
}

func (s *Server) fal(c *toolCaller, in FalIn) paidCall {
	var price falPrice
	var input map[string]any
	return paidCall{
		prepare: func(ctx context.Context) (float64, string, string) {
			if !falEndpoint.MatchString(in.Endpoint) || strings.Contains(in.Endpoint, "..") {
				return refusedArgs("endpoint must be a fal endpoint id such as fal-ai/any-llm")
			}
			if in.Input == nil {
				return refusedArgs("input must be a JSON object")
			}
			var err error
			if price, err = s.opts.Fal.Price(ctx, in.Endpoint); err != nil {
				return refusedPrice(err)
			}
			worst, err := falWorst(price, c.pv.Document.Prices.Fal)
			if err != nil {
				return refusedPrice(err)
			}
			input = in.Input
			if price.unit == falUnitMegapixels {
				if input, err = falBoundImages(in.Input, c.pv.Document.Prices.Fal.MaxMegapixels); err != nil {
					return refusedArgs(err.Error())
				}
			}
			return worst, "", ""
		},
		run: func(ctx context.Context) (string, *float64, error) {
			text, units, err := s.opts.Fal.Run(ctx, in.Endpoint, input, price.unit)
			var cost *float64
			switch {
			case err == nil && units != nil:
				v := *units * price.unitPrice
				cost = &v
			case billedNothing(err):
				cost = &zero
			}
			return text, cost, err
		},
	}
}

// escalationTimeout bounds the hand-over to factoryd.
const escalationTimeout = 30 * time.Second

// admitTool refuses a tool none of the task's classes lists in the policy.
func (s *Server) admitTool(c *toolCaller, tool string) *mcp.CallToolResult {
	if !c.allows(tool) {
		return s.refuseTool(c, tool, "refused_capability", fmt.Sprintf(
			"%s is not allowed for capability classes %q under policy version %d", tool, c.task.Classes, c.pv.ID))
	}
	return nil
}

// escalate hands the agent's question to factoryd, which stops the session
// the way a supervisor's escalate does. Free, for the classes that list it.
func (s *Server) escalate(ctx context.Context, c *toolCaller, in EscalateIn) *mcp.CallToolResult {
	tool := policy.ToolEscalate
	if c.task.Status != "running" {
		return s.refuseTool(c, tool, "refused_task_status", "the task is "+c.task.Status)
	}
	if r := s.admitTool(c, tool); r != nil {
		return r
	}
	q, why, rec := strings.TrimSpace(in.Question), strings.TrimSpace(in.Reason), strings.TrimSpace(in.Recommendation)
	if q == "" || why == "" || rec == "" || len(q) > maxQuestion || len(why) > maxQuestion || len(rec) > maxRecommendation {
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf(
			"question, reason and recommendation are required, question and reason at most %d bytes, the recommendation at most %d", maxQuestion, maxRecommendation))
	}
	choices, size := in.Choices, 0
	for _, ch := range choices {
		size += len(ch)
	}
	if len(choices) > maxChoices || size > maxChoicesBytes {
		// factoryd's body cap would refuse the question with them
		log.Printf("task %s: escalate_to_human's choices are dropped: more than %d, or over %d bytes in all", c.task.ID, maxChoices, maxChoicesBytes)
		choices = nil
	}
	return s.handOver(ctx, c, tool, Escalation{Kind: KindEscalation, Question: q, Reason: why, Recommendation: rec, Choices: choices}, "escalated",
		"escalated; this session will end. A human answers on the issue and the task resumes with the answer. Stop working now.")
}

// requestInfo hands the agent's information request to factoryd, which
// stops the session and asks the issue's author on the issue. Free, for
// the classes that list it.
func (s *Server) requestInfo(ctx context.Context, c *toolCaller, in RequestInfoIn) *mcp.CallToolResult {
	tool := policy.ToolRequestInfo
	if c.task.Status != "running" {
		return s.refuseTool(c, tool, "refused_task_status", "the task is "+c.task.Status)
	}
	if r := s.admitTool(c, tool); r != nil {
		return r
	}
	q, tried, needs := strings.TrimSpace(in.Question), strings.TrimSpace(in.Tried), strings.TrimSpace(in.Needs)
	if q == "" || tried == "" || needs == "" || len(q) > maxQuestion || len(tried) > maxQuestion || len(needs) > maxQuestion {
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf("question, tried and needs are required, each at most %d bytes", maxQuestion))
	}
	return s.handOver(ctx, c, tool, Escalation{Kind: KindInfoRequest, Question: q, Tried: tried, Needs: needs}, "info_requested",
		"asked the issue's author; this session will end. The task resumes with the author's answer. Stop working now.")
}

// handOver gives factoryd what the agent asks, which stops the session.
func (s *Server) handOver(ctx context.Context, c *toolCaller, tool string, e Escalation, decision, answer string) *mcp.CallToolResult {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), escalationTimeout)
	defer cancel()
	if err := s.opts.Escalator.Escalate(ctx, c.task.ID, e); err != nil {
		s.auditTool(c, tool, "error_escalate", err.Error(), &c.task.SpentEUR, nil)
		return toolError("the escalation did not reach factoryd: " + err.Error())
	}
	s.auditTool(c, tool, decision, textutil.Truncate(e.Question, 500), &c.task.SpentEUR, nil)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: answer}}}
}
