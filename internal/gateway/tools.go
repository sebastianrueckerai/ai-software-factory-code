package gateway

import (
	"cmp"
	"context"
	"encoding/json"
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

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/manual"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// Escalator hands an agent's question to the running session's supervision.
// Implemented by Factoryd; tests substitute a fake.
type Escalator interface {
	Escalate(ctx context.Context, taskID string, e Escalation) error
}

// Delegator files the delegate tool's child card with factoryd (roadmap
// 222, item 6).
type Delegator interface {
	Delegate(ctx context.Context, taskID string, d Delegation) (Delegated, error)
}

// Delegation is a delegate call: the child card's title and brief.
type Delegation struct {
	Title string `json:"title"`
	Brief string `json:"brief"`
}

// Delegated is the child card factoryd filed.
type Delegated struct {
	Issue  int64  `json:"issue"`
	TaskID string `json:"task_id"`
}

// DelegateIn is the delegate tool's input.
type DelegateIn struct {
	Title string `json:"title" jsonschema:"the child card's title, one line"`
	Brief string `json:"brief" jsonschema:"the brief a weaker model acts on alone: the files, the change, and an 'Acceptance:' section listing how the work is checked (one '- item' per test); factoryd refuses a brief without one"`
}

// maxDelegateBrief bounds a delegate call's brief.
const maxDelegateBrief = 16 << 10

// PlanFiler files a planner session's plan (the file_plan tool):
// factoryd checks the whole plan json and stores it as the session's
// answer. Implemented by Factoryd; tests substitute a fake.
type PlanFiler interface {
	FilePlan(ctx context.Context, taskID, plan string) (PlanFiled, error)
}

// PlanFiled is a plan filing's result: a stored revision, or every
// finding the check raised, back into the same session.
type PlanFiled struct {
	Revision int      `json:"revision"`
	SHA256   string   `json:"sha256"`
	Findings []string `json:"findings"`
}

// DeploymentReader reads a Deployment of the task's project as the cluster
// reports it (the deployment_health tool). Implemented by Factoryd; tests
// substitute a fake.
type DeploymentReader interface {
	DeploymentHealth(ctx context.Context, taskID, deployment, namespace string) (string, error)
}

// ImageLooker has the task's vision model look at one image (the
// analyze_image tool). Implemented by Factoryd; tests substitute a fake.
type ImageLooker interface {
	LookAtImage(ctx context.Context, taskID string, in AnalyzeImageIn) (string, error)
}

// HoldoutVerdicter files an audit session's hold-out verdict (the
// file_verdict tool): factoryd checks it against the hold-out the audit
// runs on and stores it as the session's answer. Implemented by Factoryd;
// tests substitute a fake.
type HoldoutVerdicter interface {
	FileVerdict(ctx context.Context, taskID string, v HoldoutVerdictIn) (VerdictFiled, error)
}

// HoldoutVerdictIn is the filing's payload: the hold-out's idx on the
// task, the verdict, why, and the rewrite a wrong test carries.
type HoldoutVerdictIn struct {
	Idx      int    `json:"idx"`
	Verdict  string `json:"verdict"`
	Reason   string `json:"reason"`
	Quote    string `json:"quote"`
	TestCode string `json:"test_code"`
}

// VerdictFiled is a verdict filing's result: filed, or every finding the
// check raised, back into the same session.
type VerdictFiled struct {
	Filed    bool     `json:"filed"`
	Findings []string `json:"findings"`
}

// Escalation is what an agent asks: a human (escalate_to_human: the
// question, why, what the agent would do and the answers it offers), the
// issue's author (request_info: the question, what it tried, what it
// needs), the factory for a tool (request_tool: the tool, what for) or
// the check-in for a cut of its card (request_cut: why, and the parts).
type Escalation struct {
	Kind           string   `json:"kind"`
	Question       string   `json:"question"`
	Reason         string   `json:"reason,omitempty"`
	Recommendation string   `json:"recommendation,omitempty"`
	Choices        []string `json:"choices,omitempty"`
	Tried          string   `json:"tried,omitempty"`
	Needs          string   `json:"needs,omitempty"`
	Tool           string   `json:"tool,omitempty"`
	Need           string   `json:"need,omitempty"`
	Parts          []string `json:"parts,omitempty"`
}

// Escalation kinds.
const (
	KindEscalation  = "escalation"
	KindInfoRequest = "info_request"
	KindMissingTool = "missing_tool"
	KindCut         = "cut"
)

var (
	_ Escalator        = (*Factoryd)(nil)
	_ PlanFiler        = (*Factoryd)(nil)
	_ HoldoutVerdicter = (*Factoryd)(nil)
	_ DeploymentReader = (*Factoryd)(nil)
	_ ImageLooker      = (*Factoryd)(nil)
)

// Factoryd is the gateway's client to factoryd's internal endpoint.
type Factoryd struct {
	BaseURL string // http://factoryd.factory.svc.cluster.local:8080
	Token   string // the shared bearer (secret factory/gateway-factoryd)
	HTTP    *http.Client
	// RetryFor and RetryEvery bound Escalate's retries while factoryd
	// restarts (0: the defaults below)
	RetryFor, RetryEvery time.Duration
}

// A restarting factoryd (a roll) is unreachable or has no live session for
// a minute or two, within which the retried session reattaches (roadmap
// 222, item 2): an escalation raised meanwhile waits for it, inside the
// MCP tool's 150 s timeout.
const (
	escalateRetryFor   = 2 * time.Minute
	escalateRetryEvery = 5 * time.Second
)

func (f *Factoryd) Escalate(ctx context.Context, taskID string, e Escalation) error {
	deadline := time.Now().Add(cmp.Or(f.RetryFor, escalateRetryFor))
	for {
		_, err := call(ctx, f.HTTP, http.MethodPost, f.BaseURL+"/internal/escalations",
			http.Header{"Authorization": {"Bearer " + f.Token}},
			struct {
				TaskID string `json:"task_id"`
				Escalation
			}{taskID, e})
		if err == nil || !escalateRetries(err) || !time.Now().Before(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cmp.Or(f.RetryEvery, escalateRetryEvery)):
		}
	}
}

// escalateRetries: a factoryd unreachable, erring or without the live
// session (409) is retried; a refusal of the escalation itself is not.
func escalateRetries(err error) bool {
	var pe *providerError
	if !errors.As(err, &pe) {
		return true
	}
	return pe.status >= 500 || pe.status == http.StatusConflict
}

func (f *Factoryd) Delegate(ctx context.Context, taskID string, d Delegation) (Delegated, error) {
	var res Delegated
	b, err := call(ctx, f.HTTP, http.MethodPost, f.BaseURL+"/internal/delegations",
		http.Header{"Authorization": {"Bearer " + f.Token}},
		struct {
			TaskID string `json:"task_id"`
			Delegation
		}{taskID, d})
	if err != nil {
		return res, err
	}
	err = json.Unmarshal(b, &res)
	return res, err
}

func (f *Factoryd) FilePlan(ctx context.Context, taskID, plan string) (PlanFiled, error) {
	var res PlanFiled
	b, err := call(ctx, f.HTTP, http.MethodPost, f.BaseURL+"/internal/plans",
		http.Header{"Authorization": {"Bearer " + f.Token}},
		struct {
			TaskID string `json:"task_id"`
			Plan   string `json:"plan"`
		}{taskID, plan})
	if err != nil {
		return res, err
	}
	err = json.Unmarshal(b, &res)
	return res, err
}

// DeploymentHealth returns factoryd's plain-text report. A refusal comes
// back as an error that is factoryd's one sentence, for the agent to
// correct its call by.
func (f *Factoryd) DeploymentHealth(ctx context.Context, taskID, deployment, namespace string) (string, error) {
	b, err := call(ctx, f.HTTP, http.MethodPost, f.BaseURL+"/internal/deployment-health",
		http.Header{"Authorization": {"Bearer " + f.Token}},
		struct {
			TaskID     string `json:"task_id"`
			Deployment string `json:"deployment"`
			Namespace  string `json:"namespace,omitempty"`
		}{taskID, deployment, namespace})
	return factorydText(b, err)
}

// LookAtImage returns the vision model's answer on the image in names, as
// DeploymentHealth its report.
func (f *Factoryd) LookAtImage(ctx context.Context, taskID string, in AnalyzeImageIn) (string, error) {
	b, err := call(ctx, f.HTTP, http.MethodPost, f.BaseURL+"/internal/image-look",
		http.Header{"Authorization": {"Bearer " + f.Token}},
		struct {
			TaskID string `json:"task_id"`
			AnalyzeImageIn
		}{taskID, in})
	return factorydText(b, err)
}

// factorydText is the text of factoryd's {"text"} answer b; a refusal is
// factoryd's one sentence as the error.
func factorydText(b []byte, err error) (string, error) {
	var pe *providerError
	if errors.As(err, &pe) {
		// a refusal and factoryd's 502 (what it asked did not answer) are its
		// sentences; another 5xx says nothing an agent can act on
		if sentence := strings.TrimSpace(pe.body); sentence != "" && (pe.status < 500 || pe.status == http.StatusBadGateway) {
			return "", errors.New(sentence)
		}
		return "", fmt.Errorf("factoryd answered %d", pe.status)
	}
	if err != nil {
		return "", err
	}
	var res struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return "", err
	}
	return res.Text, nil
}

func (f *Factoryd) FileVerdict(ctx context.Context, taskID string, v HoldoutVerdictIn) (VerdictFiled, error) {
	var res VerdictFiled
	b, err := call(ctx, f.HTTP, http.MethodPost, f.BaseURL+"/internal/holdout-verdict",
		http.Header{"Authorization": {"Bearer " + f.Token}},
		struct {
			TaskID string `json:"task_id"`
			HoldoutVerdictIn
		}{taskID, v})
	if err != nil {
		return res, err
	}
	err = json.Unmarshal(b, &res)
	return res, err
}

// toolCaller is who an MCP request is for: the task, read per request,
// its session (Identity.Session) and the policy version it is decided
// under.
type toolCaller struct {
	pv      *policy.Version
	task    Task
	session string
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
		h.ServeHTTP(w, r.WithContext(context.WithValue(ctx, callerKey{}, &toolCaller{pv: pv, task: task, session: id.Session})))
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

type RequestToolIn struct {
	Tool string `json:"tool" jsonschema:"the tool's Debian package or binary name, such as chromium, ffmpeg or python3"`
	Need string `json:"need" jsonschema:"what you need it for in this card"`
}

type RequestCutIn struct {
	Reason string   `json:"reason" jsonschema:"why the card is bigger than one card should be"`
	Parts  []string `json:"parts" jsonschema:"the 2 to 5 parts you propose, each one card's work"`
}

type FilePlanIn struct {
	Plan string `json:"plan" jsonschema:"the complete plan json: one object with spec, summary, criteria and cards"`
}

type DeploymentHealthIn struct {
	Deployment string `json:"deployment" jsonschema:"the Deployment's name, a DNS-1123 subdomain such as web-api"`
	Namespace  string `json:"namespace,omitempty" jsonschema:"the namespace, a DNS-1123 label; leave it out to look in each of the project's namespaces"`
}

type AnalyzeImageIn struct {
	Path      string `json:"path,omitempty" jsonschema:"an image file in the workspace, such as a screenshot you took: absolute under /workspace or relative to the repository"`
	CardImage string `json:"card_image,omitempty" jsonschema:"an image on the card's issue or its comments: its name, such as img1.png, or its /attachments/ link"`
	Ask       string `json:"ask,omitempty" jsonschema:"what to look for, at most 2000 characters; leave it out to have the image described"`
}

// The bounds of an image look's arguments.
const (
	maxImagePath = 4096
	maxCardImage = 512
	maxImageAsk  = 2000
)

type FileVerdictIn struct {
	Idx      int    `json:"idx" jsonschema:"the hold-out's idx, from the task card"`
	Verdict  string `json:"verdict" jsonschema:"wrong_test, real_gap or spec_question"`
	Reason   string `json:"reason,omitempty" jsonschema:"why: what the test does wrong, the gap the spec names, or the question for the human"`
	Quote    string `json:"quote,omitempty" jsonschema:"with a rewrite: the spec sentence it tests, quoted word for word"`
	TestCode string `json:"test_code,omitempty" jsonschema:"with wrong_test and a rewrite: the rewrite's complete Go code, package holdout, its test's name unchanged"`
}

// maxVerdictCode bounds a rewrite the gateway passes on: factoryd's
// check refuses a larger one, so the session learns it here, without
// the hand-over.
const maxVerdictCode = 4 << 10

// plainArrays is T's input schema with each list of strings a plain
// array: the inferred type list ["null", "array"] is one some models' tool
// calls refuse.
func plainArrays[T any]() *jsonschema.Schema {
	s, err := jsonschema.For[T](&jsonschema.ForOptions{TypeSchemas: map[reflect.Type]*jsonschema.Schema{
		reflect.TypeFor[[]string](): {Type: "array", Items: &jsonschema.Schema{Type: "string"}}}})
	if err != nil {
		panic(err)
	}
	return s
}

var (
	escalateSchema = plainArrays[EscalateIn]()
	cutSchema      = plainArrays[RequestCutIn]()
)

// maxQuestion bounds an escalation's question and reason, an information
// request's fields and a cut's reason; maxRecommendation the
// recommendation.
const (
	maxQuestion       = 4000
	maxRecommendation = 2000
)

// maxCutParts bounds the parts a cut proposes, maxCutPart the bytes of one.
const (
	maxCutParts = 5
	maxCutPart  = 400
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
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolRequestInfo, Description: "Ask the issue's author for information, instead of guessing, when you could not reproduce the problem and found no fault: " +
		"what you tried and what you need. This session ends; the task resumes with the author's answer, or the issue closes as not reproducible after 14 days. Free."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in RequestInfoIn) (*mcp.CallToolResult, any, error) {
			return s.requestInfo(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolRequestTool, Description: "Ask for a tool this sandbox lacks, such as a browser, ffmpeg or a language runtime, instead of trying to install it: " +
		"the sandbox refuses installs. Your work in progress is pushed, a card adds the tool to one of the project's images, and this card restarts on that image " +
		"from its branch once the image is built. Name the tool by its Debian package or binary name (chromium, ffmpeg, python3), and what you need it for."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in RequestToolIn) (*mcp.CallToolResult, any, error) {
			return s.requestTool(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolRequestCut, Description: "Ask to cut this card when its work is bigger than one card should be: name why and the 2 to 5 parts you propose. " +
		"The check-in judges it. If it decides to cut, this session stops, your work in progress is pushed, and the planner plans the rest on top of it; " +
		"otherwise it answers you with a message. Go on with the work meanwhile. Free.", InputSchema: cutSchema},
		func(ctx context.Context, _ *mcp.CallToolRequest, in RequestCutIn) (*mcp.CallToolResult, any, error) {
			return s.requestCut(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolDelegate, Description: "File a small follow-up card on the cheap model rung that starts when this card has landed: " +
		"routine, well-specified work this card need not do itself. Give it a title and a brief a weaker model acts on alone: the files, the change, how to check it. " +
		"The card depends on this one and builds on its merge. Free."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in DelegateIn) (*mcp.CallToolResult, any, error) {
			return s.delegate(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolFilePlan, Description: "File the plan: hand the complete plan json over and it is checked at once. " +
		"A refusal names every finding; correct the plan and file it again in this session. A filed plan is this session's answer; end the session. Free."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in FilePlanIn) (*mcp.CallToolResult, any, error) {
			return s.filePlan(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolFileVerdict, Description: "File the audit's verdict on one hold-out: exactly one of wrong_test (no code drops the test; " +
		"test_code files the rewrite, with a quote of the spec sentence it tests), real_gap (the criterion becomes public and the builder works it) or " +
		"spec_question (the reason is the question for the human). A refusal names every finding; correct it and file again in this session. " +
		"A filed verdict is this session's answer; end the session. Free."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in FileVerdictIn) (*mcp.CallToolResult, any, error) {
			return s.fileVerdict(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolManual, Description: "Read the factory manual: what the factory is, the card states, the gates your pull request passes. Free."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return s.manualRead(c), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolDeploymentHealth, Description: "Read a Deployment of this card's project as the cluster reports it right now: the Deployment's replicas, " +
		"its pods with each container's restart count, state and last termination (reason, exit code, time), and the Kubernetes events of the last hour for the Deployment, its ReplicaSets and pods. " +
		"No logs and no Secrets. The namespace may be omitted: factoryd looks in each of the project's namespaces. Free."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in DeploymentHealthIn) (*mcp.CallToolResult, any, error) {
			return s.deploymentHealth(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolAnalyzeImage, Description: "Have a vision model look at one image and answer in words: a file in your workspace, " +
		"such as a screenshot of the page you built, or an image on the card's issue, such as a screenshot the card was filed with. " +
		"Name exactly one of path and card_image; ask says what to look for. PNG, JPEG, WebP or GIF of at most 8 MiB. Spends on the card's budget as a model call."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in AnalyzeImageIn) (*mcp.CallToolResult, any, error) {
			return s.analyzeImage(ctx, c, in), nil, nil
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
// every call: one of the task's classes lists it, it is no tool card's
// request_tool, and the task's project is offered it (policy.OffersTool:
// deployment_health only where the project lists namespaces).
func (c *toolCaller) allows(tool string) bool {
	_, p, _ := c.pv.ProjectOf(c.task.Repository)
	return c.pv.Document.AllowsSessionTool(c.task.Classes, c.task.Source, tool) && policy.OffersTool(tool, p.Namespaces)
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
		TaskID: c.task.ID, Kind: policy.KindTool, Ref: tool, Session: c.session, EUR: worst, Cap: toolCap,
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
	b, err := s.opts.Store.ReleaseHold(context.Background(), hold, cost, policy.Usage{})
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

// deploymentHealthTimeout bounds the read at factoryd.
const deploymentHealthTimeout = 20 * time.Second

// imageLookTimeout bounds an image look at factoryd: the image's read and
// the vision call (factoryd's operatorTimeout, 2 minutes), inside
// opencode's 150 s on an MCP call.
const imageLookTimeout = 145 * time.Second

// admitTool refuses a tool none of the task's classes lists in the policy.
func (s *Server) admitTool(c *toolCaller, tool string) *mcp.CallToolResult {
	switch {
	case c.allows(tool):
		return nil
	case c.pv.Document.AllowsTool(c.task.Classes, tool):
		return s.refuseTool(c, tool, "refused_capability", tool+" is not a tool card's: a tool card declares tools its own sandbox never gets")
	}
	return s.refuseTool(c, tool, "refused_capability", fmt.Sprintf(
		"%s is not allowed for capability classes %q under policy version %d", tool, c.task.Classes, c.pv.ID))
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
		log.Printf("task %s: escalate_to_human's choices are dropped: more than %d, or over %d bytes in all", c.task.ID, maxChoices, maxChoicesBytes) // #nosec G706 -- the task id is the stored row's, found by the verified JWT
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

// requestTool hands the agent's request for a tool to factoryd, which
// stops the session and parks the card on an image with the tool. Free,
// for the classes that list it.
func (s *Server) requestTool(ctx context.Context, c *toolCaller, in RequestToolIn) *mcp.CallToolResult {
	tool := policy.ToolRequestTool
	if c.task.Status != "running" {
		return s.refuseTool(c, tool, "refused_task_status", "the task is "+c.task.Status)
	}
	if r := s.admitTool(c, tool); r != nil {
		return r
	}
	name, need := strings.TrimSpace(in.Tool), strings.TrimSpace(in.Need)
	if refusal := policy.ToolAskRefusal(name, need); refusal != "" {
		return s.refuseTool(c, tool, "refused_arguments", refusal)
	}
	return s.handOver(ctx, c, tool, Escalation{Kind: KindMissingTool, Tool: name, Need: need}, "tool_requested",
		"asked for "+name+"; this session will end. Your work in progress is pushed; the card restarts on an image with the tool once it is built, "+
			"or the next session is told why not. Stop working now.")
}

// requestCut hands the agent's ask to cut its card to factoryd, whose
// check-in judges it while the session goes on. Free, for the classes that
// list it.
func (s *Server) requestCut(ctx context.Context, c *toolCaller, in RequestCutIn) *mcp.CallToolResult {
	tool := policy.ToolRequestCut
	if c.task.Status != "running" {
		return s.refuseTool(c, tool, "refused_task_status", "the task is "+c.task.Status)
	}
	if r := s.admitTool(c, tool); r != nil {
		return r
	}
	why, parts := strings.TrimSpace(in.Reason), make([]string, len(in.Parts))
	for i, p := range in.Parts {
		parts[i] = strings.TrimSpace(p)
	}
	if why == "" || len(why) > maxQuestion || len(parts) < 2 || len(parts) > maxCutParts ||
		slices.ContainsFunc(parts, func(p string) bool { return p == "" || len(p) > maxCutPart || strings.ContainsAny(p, "\r\n") }) {
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf(
			"reason (at most %d bytes) and 2 to %d parts (each one line of at most %d bytes) are required", maxQuestion, maxCutParts, maxCutPart))
	}
	return s.handOver(ctx, c, tool, Escalation{Kind: KindCut, Reason: why, Parts: parts}, "cut_requested",
		"asked to cut the card; the check-in judges it. Go on with the work until it answers.")
}

// manualRead answers the manual tool with the factory manual from
// internal/manual, the one source the project agent's manual reads too.
// Free, for the classes that list it.
func (s *Server) manualRead(c *toolCaller) *mcp.CallToolResult {
	tool := policy.ToolManual
	if c.task.Status != "running" {
		return s.refuseTool(c, tool, "refused_task_status", "the task is "+c.task.Status)
	}
	if r := s.admitTool(c, tool); r != nil {
		return r
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: manual.Text()}}}
}

var (
	dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	dnsLabel     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
)

// deploymentHealth has factoryd read one Deployment of the task's project.
// Free, for the classes that list it.
func (s *Server) deploymentHealth(ctx context.Context, c *toolCaller, in DeploymentHealthIn) *mcp.CallToolResult {
	tool := policy.ToolDeploymentHealth
	if c.task.Status != "running" {
		return s.refuseTool(c, tool, "refused_task_status", "the task is "+c.task.Status)
	}
	if r := s.admitTool(c, tool); r != nil {
		return r
	}
	dep, ns := strings.TrimSpace(in.Deployment), strings.TrimSpace(in.Namespace)
	if len(dep) > 253 || !dnsSubdomain.MatchString(dep) {
		return s.refuseTool(c, tool, "refused_arguments", "deployment is required: a DNS-1123 subdomain of at most 253 bytes (lowercase letters, digits, '-' and '.', starting and ending alphanumeric)")
	}
	if ns != "" && (len(ns) > 63 || !dnsLabel.MatchString(ns)) {
		return s.refuseTool(c, tool, "refused_arguments", "namespace must be left out or be a DNS-1123 label of at most 63 bytes (lowercase letters, digits and '-', starting and ending alphanumeric)")
	}
	ctx, cancel := context.WithTimeout(ctx, deploymentHealthTimeout)
	defer cancel()
	text, err := s.opts.DeploymentReader.DeploymentHealth(ctx, c.task.ID, dep, ns)
	if err != nil {
		s.auditTool(c, tool, "error_deployment_health", textutil.Truncate(err.Error(), 500), &c.task.SpentEUR, nil)
		return toolError(err.Error())
	}
	s.auditTool(c, tool, "deployment_read", dep, &c.task.SpentEUR, nil)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// analyzeImage has factoryd look at the image in names with the task's
// vision model. Free, for the classes that list it; the look spends on the
// task's account as a model call.
func (s *Server) analyzeImage(ctx context.Context, c *toolCaller, in AnalyzeImageIn) *mcp.CallToolResult {
	tool := policy.ToolAnalyzeImage
	if c.task.Status != "running" {
		return s.refuseTool(c, tool, "refused_task_status", "the task is "+c.task.Status)
	}
	if r := s.admitTool(c, tool); r != nil {
		return r
	}
	in.Path, in.CardImage, in.Ask = strings.TrimSpace(in.Path), strings.TrimSpace(in.CardImage), strings.TrimSpace(in.Ask)
	switch {
	case (in.Path == "") == (in.CardImage == ""):
		return s.refuseTool(c, tool, "refused_arguments", "name exactly one of path (a workspace file) and card_image (an image on the card's issue)")
	case len(in.Path) > maxImagePath:
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf("path is longer than %d bytes", maxImagePath))
	case len(in.CardImage) > maxCardImage:
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf("card_image is longer than %d bytes: name the image or paste its /attachments/ link", maxCardImage))
	case len(in.Ask) > maxImageAsk:
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf("ask is longer than %d characters", maxImageAsk))
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), imageLookTimeout)
	defer cancel()
	text, err := s.opts.ImageLooker.LookAtImage(ctx, c.task.ID, in)
	if err != nil {
		s.auditTool(c, tool, "error_image_look", textutil.Truncate(err.Error(), 500), &c.task.SpentEUR, nil)
		return toolError(err.Error())
	}
	s.auditTool(c, tool, "image_looked", cmp.Or(in.CardImage, in.Path), &c.task.SpentEUR, nil)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// filePlan hands the planner's plan to factoryd, which checks the whole
// json at once: a refusal carries every finding back into the session, a
// filed plan is the session's answer. Free, for the classes that list it.
func (s *Server) filePlan(ctx context.Context, c *toolCaller, in FilePlanIn) *mcp.CallToolResult {
	tool := policy.ToolFilePlan
	if c.task.Status != "running" {
		return s.refuseTool(c, tool, "refused_task_status", "the task is "+c.task.Status)
	}
	if r := s.admitTool(c, tool); r != nil {
		return r
	}
	if plan := strings.TrimSpace(in.Plan); plan == "" {
		return s.refuseTool(c, tool, "refused_arguments", "plan is required")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), escalationTimeout)
	defer cancel()
	res, err := s.opts.PlanFiler.FilePlan(ctx, c.task.ID, strings.TrimSpace(in.Plan))
	if err != nil {
		s.auditTool(c, tool, "error_plan", err.Error(), &c.task.SpentEUR, nil)
		return toolError("the plan did not reach factoryd: " + err.Error())
	}
	if len(res.Findings) > 0 {
		s.auditTool(c, tool, "plan_refused", textutil.Truncate(strings.Join(res.Findings, "; "), 500), &c.task.SpentEUR, nil)
		return toolError("the plan was refused:\n- " + strings.Join(res.Findings, "\n- ") +
			"\ncorrect the plan and file it again in this session")
	}
	s.auditTool(c, tool, "plan_filed", fmt.Sprintf("revision %d", res.Revision), &c.task.SpentEUR, nil)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
		Text: fmt.Sprintf("filed as revision %d; it is this session's answer. You may end the session.", res.Revision)}}}
}

// fileVerdict hands the audit session's verdict on one hold-out to
// factoryd, which checks it against the hold-out the audit runs on: a
// refusal carries every finding back into the session, a filed verdict
// is the session's answer. Free, for the classes that list it.
func (s *Server) fileVerdict(ctx context.Context, c *toolCaller, in FileVerdictIn) *mcp.CallToolResult {
	tool := policy.ToolFileVerdict
	if c.task.Status != "running" {
		return s.refuseTool(c, tool, "refused_task_status", "the task is "+c.task.Status)
	}
	if r := s.admitTool(c, tool); r != nil {
		return r
	}
	if in.Verdict = strings.TrimSpace(in.Verdict); in.Verdict == "" || in.Idx < 0 || len(in.TestCode) > maxVerdictCode {
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf("idx and verdict are required, the rewrite at most %d bytes", maxVerdictCode))
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), escalationTimeout)
	defer cancel()
	res, err := s.opts.HoldoutVerdicter.FileVerdict(ctx, c.task.ID, HoldoutVerdictIn(in))
	if err != nil {
		s.auditTool(c, tool, "error_verdict", err.Error(), &c.task.SpentEUR, nil)
		return toolError("the verdict did not reach factoryd: " + err.Error())
	}
	if !res.Filed {
		s.auditTool(c, tool, "verdict_refused", textutil.Truncate(strings.Join(res.Findings, "; "), 500), &c.task.SpentEUR, nil)
		return toolError("the verdict was refused:\n- " + strings.Join(res.Findings, "\n- ") +
			"\ncorrect the verdict and file it again in this session")
	}
	s.auditTool(c, tool, "verdict_filed", fmt.Sprintf("hold-out %d: %s", in.Idx, in.Verdict), &c.task.SpentEUR, nil)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
		Text: fmt.Sprintf("filed, hold-out %d: %s. It is this session's answer. You may end the session.", in.Idx, in.Verdict)}}}
}

// delegate files the child card with factoryd; the session goes on.
func (s *Server) delegate(ctx context.Context, c *toolCaller, in DelegateIn) *mcp.CallToolResult {
	tool := policy.ToolDelegate
	if c.task.Status != "running" {
		return s.refuseTool(c, tool, "refused_task_status", "the task is "+c.task.Status)
	}
	if r := s.admitTool(c, tool); r != nil {
		return r
	}
	title, brief := strings.TrimSpace(in.Title), strings.TrimSpace(in.Brief)
	if title == "" || len(title) > 255 || brief == "" || len(brief) > maxDelegateBrief {
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf("title (1 to 255 characters) and brief (at most %d bytes) are required", maxDelegateBrief))
	}
	if s.opts.Delegator == nil {
		return s.refuseTool(c, tool, "refused_capability", "no delegation hand-over is wired")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), escalationTimeout)
	defer cancel()
	res, err := s.opts.Delegator.Delegate(ctx, c.task.ID, Delegation{Title: title, Brief: brief})
	if err != nil {
		s.auditTool(c, tool, "error_delegate", err.Error(), &c.task.SpentEUR, nil)
		return toolError("the card did not reach factoryd: " + err.Error())
	}
	s.auditTool(c, tool, "delegated", fmt.Sprintf("issue #%d, task %s", res.Issue, res.TaskID), &c.task.SpentEUR, nil)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
		Text: fmt.Sprintf("filed as issue #%d (card %s) on the cheap rung; it starts when this card has landed. Carry on with this card.", res.Issue, res.TaskID)}}}
}

// handOver gives factoryd what the agent asks, which stops the session,
// or for a cut asks the check-in.
func (s *Server) handOver(ctx context.Context, c *toolCaller, tool string, e Escalation, decision, answer string) *mcp.CallToolResult {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), escalationTimeout)
	defer cancel()
	if err := s.opts.Escalator.Escalate(ctx, c.task.ID, e); err != nil {
		// factoryd refuses what this session cannot ask, as a report
		// session's request_tool
		var pe *providerError
		if errors.As(err, &pe) && pe.status == http.StatusUnprocessableEntity {
			return s.refuseTool(c, tool, "refused_session", strings.TrimSpace(pe.body))
		}
		s.auditTool(c, tool, "error_escalate", err.Error(), &c.task.SpentEUR, nil)
		return toolError("the escalation did not reach factoryd: " + err.Error())
	}
	asked := e.Question
	switch e.Kind {
	case KindMissingTool:
		asked = e.Tool + ": " + e.Need
	case KindCut:
		asked = e.Reason
	}
	s.auditTool(c, tool, decision, textutil.Truncate(asked, 500), &c.task.SpentEUR, nil)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: answer}}}
}
