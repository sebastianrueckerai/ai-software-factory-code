package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// Escalator hands an agent's question to the running session's supervision.
// Implemented by Factoryd; tests substitute a fake.
type Escalator interface {
	Escalate(ctx context.Context, taskID, question, reason string) error
}

var _ Escalator = (*Factoryd)(nil)

// Factoryd is the gateway's client to factoryd's internal endpoint.
type Factoryd struct {
	BaseURL string // http://factoryd.factory.svc.cluster.local:8080
	Token   string // the shared bearer (secret factory/gateway-factoryd)
	HTTP    *http.Client
}

func (f *Factoryd) Escalate(ctx context.Context, taskID, question, reason string) error {
	_, err := call(ctx, f.HTTP, http.MethodPost, f.BaseURL+"/internal/escalations",
		http.Header{"Authorization": {"Bearer " + f.Token}},
		map[string]string{"task_id": taskID, "question": question, "reason": reason})
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
			s.refuse(w, pid, id.TaskID, "refused_auth", err.Error(), "", http.StatusUnauthorized, "authentication_error")
			return
		}
		if polErr != nil {
			s.refuse(w, pid, id.TaskID, "error_policy", polErr.Error(), "", http.StatusServiceUnavailable, "server_error")
			return
		}
		task, ok, err := s.opts.Store.Task(ctx, id.TaskID)
		if err != nil {
			s.refuse(w, pid, id.TaskID, "error_store", err.Error(), "", http.StatusInternalServerError, "server_error")
			return
		}
		if !ok {
			s.refuse(w, pid, id.TaskID, "refused_task_unknown", "no task row for "+id.TaskID, "", http.StatusNotFound, "invalid_request_error")
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
	Question string `json:"question" jsonschema:"what the human must answer or decide"`
	Reason   string `json:"reason" jsonschema:"why the task cannot go on without it"`
}

// maxQuestion bounds an escalation's question and reason.
const maxQuestion = 4000

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
		"This session ends; the task resumes with the human's answer. Free."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in EscalateIn) (*mcp.CallToolResult, any, error) {
			return s.escalate(ctx, c, in), nil, nil
		})
	if s.opts.Cloud != nil {
		s.addCloudTools(srv, c)
	}
	return srv
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
	toolCap, ok := c.pv.Document.ToolCap(c.task.Classes, tool)
	if !ok {
		return s.refuseTool(c, tool, "refused_capability", fmt.Sprintf(
			"%s is not allowed for capability classes %q under policy version %d", tool, c.task.Classes, c.pv.ID))
	}
	worst, decision, reason := p.prepare(ctx)
	if decision != "" {
		return s.refuseTool(c, tool, decision, reason)
	}
	hold, err := s.opts.Store.TakeHold(ctx, policy.HoldRequest{
		TaskID: c.task.ID, Kind: policy.KindTool, Ref: tool, EUR: worst, Cap: toolCap,
		DayEUR: c.pv.Document.Budget.DayEUR,
	})
	var refusal *policy.Refusal
	if errors.As(err, &refusal) {
		return s.refuseTool(c, tool, refusal.Decision, refusal.Reason)
	}
	if err != nil {
		s.auditTool(c, tool, "error_store", err.Error(), nil, nil)
		return toolError("the factory could not reserve the call's cost: " + err.Error())
	}
	// a disconnecting agent must not cut a call the provider bills; the
	// connectors' client timeouts bound it
	text, cost, callErr := p.run(context.WithoutCancel(ctx))
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
	} else if b.OverHold != "" {
		decision, note = "allowed_over_hold", "; "+b.OverHold
	}
	spend := b.Spent
	if callErr != nil {
		s.auditTool(c, tool, "error_tool", callErr.Error()+note, &spend, &hold.EUR)
		return toolError(tool + " failed: " + callErr.Error())
	}
	s.auditTool(c, tool, decision, strings.TrimPrefix(note, "; "), &spend, &hold.EUR)
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
	if err := s.opts.Store.InsertAudit(context.Background(), AuditRow{
		TaskID: c.task.ID, Decision: decision, Reason: reason, Tool: tool, SpendEUR: spend,
		PolicyVersion: &c.pv.ID, HoldEUR: hold,
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

// escalate hands the agent's question to factoryd, which stops the session
// the way a supervisor's escalate does. Free, for the coder class.
func (s *Server) escalate(ctx context.Context, c *toolCaller, in EscalateIn) *mcp.CallToolResult {
	tool := policy.ToolEscalate
	if c.task.Status != "running" {
		return s.refuseTool(c, tool, "refused_task_status", "the task is "+c.task.Status)
	}
	if !slices.Contains(c.task.Classes, policy.ClassCoder) {
		return s.refuseTool(c, tool, "refused_capability", fmt.Sprintf("%s is for the %s class; the task holds %q", tool, policy.ClassCoder, c.task.Classes))
	}
	q, why := strings.TrimSpace(in.Question), strings.TrimSpace(in.Reason)
	if q == "" || why == "" || len(q) > maxQuestion || len(why) > maxQuestion {
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf("question and reason are required, each at most %d bytes", maxQuestion))
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), escalationTimeout)
	defer cancel()
	if err := s.opts.Escalator.Escalate(ctx, c.task.ID, q, why); err != nil {
		s.auditTool(c, tool, "error_escalate", err.Error(), &c.task.SpentEUR, nil)
		return toolError("the escalation did not reach factoryd: " + err.Error())
	}
	s.auditTool(c, tool, "escalated", textutil.Truncate(q, 500), &c.task.SpentEUR, nil)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
		Text: "escalated; this session will end. A human answers on the issue and the task resumes with the answer. Stop working now."}}}
}
