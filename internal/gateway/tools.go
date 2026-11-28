package gateway

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/answers"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/manual"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
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

// Delegation is a delegate call: the child card's title, why line,
// summary and brief.
type Delegation struct {
	Title   string `json:"title"`
	Why     string `json:"why"`
	Summary string `json:"summary"`
	Brief   string `json:"brief"`
}

// Delegated is the child card factoryd filed.
type Delegated struct {
	Issue  int64  `json:"issue"`
	TaskID string `json:"task_id"`
}

// DelegateIn is the delegate tool's input; its title, why and summary
// carry the brief's field lines (withBriefLines).
type DelegateIn struct {
	Title   string `json:"title"`
	Why     string `json:"why"`
	Summary string `json:"summary"`
	Brief   string `json:"brief" jsonschema:"the brief a weaker model acts on alone: the files, the change, and an 'Acceptance:' section listing how the work is checked (one '- item' per test); factoryd refuses a brief without one"`
}

// maxDelegateBrief bounds a delegate call's brief.
const maxDelegateBrief = 16 << 10

// ProofsFiler files a prover's tests (the file_proofs tool): factoryd
// checks the filing's shape against the proofs the plan owes and keeps an
// accepted one for its dry run on main after the session. Implemented by
// Factoryd; tests substitute a fake.
type ProofsFiler interface {
	FileProofs(ctx context.Context, taskID string, proofs []ProofIn) (ProofsFiled, error)
}

// ProofsFiled is a filing's answer: its number, the filings left, and
// every finding that refused it.
type ProofsFiled struct {
	Filing   int      `json:"filing"`
	Left     int      `json:"left"`
	Findings []string `json:"findings"`
}

// AnswerFiler files a session's answer object (the file_screening,
// file_size, file_map and the planner's file_spec, file_criteria,
// file_cards, file_replan and file_revert tools):
// factoryd checks it and stores it as the session's answer. Implemented by
// Factoryd; tests substitute a fake.
type AnswerFiler interface {
	FileAnswer(ctx context.Context, taskID, tool string, answer json.RawMessage) (AnswerFiled, error)
}

// AnswerFiled is an answer filing's result: the findings that refused it,
// none when it is stored.
type AnswerFiled struct {
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

// Digester has the Small model answer an ask over one workspace file (the
// digest tool). Implemented by Factoryd; tests substitute a fake.
type Digester interface {
	Digest(ctx context.Context, taskID string, in DigestIn) (string, error)
}

// Briefer reads a session's card brief at factoryd (GET /worker/brief)
// and stores there the text session session's first model call got (POST
// /worker/session-brief). Implemented by Factoryd; tests substitute a fake.
type Briefer interface {
	Brief(ctx context.Context, taskID string) (string, error)
	StoreSessionBrief(ctx context.Context, taskID, session, text string) error
}

// Escalation is what an agent asks: a human (escalate: the
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
	// Choice is the number from 1 of the choice the agent recommends, 0
	// for none.
	Choice int      `json:"choice,omitempty"`
	Tried  string   `json:"tried,omitempty"`
	Needs  string   `json:"needs,omitempty"`
	Tool   string   `json:"tool,omitempty"`
	Need   string   `json:"need,omitempty"`
	Parts  []string `json:"parts,omitempty"`
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
	_ ProofsFiler      = (*Factoryd)(nil)
	_ AnswerFiler      = (*Factoryd)(nil)
	_ DeploymentReader = (*Factoryd)(nil)
	_ ImageLooker      = (*Factoryd)(nil)
	_ Digester         = (*Factoryd)(nil)
	_ Briefer          = (*Factoryd)(nil)
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

func (f *Factoryd) FileProofs(ctx context.Context, taskID string, proofs []ProofIn) (ProofsFiled, error) {
	var res ProofsFiled
	b, err := call(ctx, f.HTTP, http.MethodPost, f.BaseURL+"/internal/proofs",
		http.Header{"Authorization": {"Bearer " + f.Token}},
		struct {
			TaskID string    `json:"task_id"`
			Proofs []ProofIn `json:"proofs"`
		}{taskID, proofs})
	if err != nil {
		return res, err
	}
	err = json.Unmarshal(b, &res)
	return res, err
}

func (f *Factoryd) FileAnswer(ctx context.Context, taskID, tool string, answer json.RawMessage) (AnswerFiled, error) {
	var res AnswerFiled
	b, err := call(ctx, f.HTTP, http.MethodPost, f.BaseURL+"/internal/answers",
		http.Header{"Authorization": {"Bearer " + f.Token}},
		struct {
			TaskID string          `json:"task_id"`
			Tool   string          `json:"tool"`
			Answer json.RawMessage `json:"answer"`
		}{taskID, tool, answer})
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

// Digest returns the Small model's answer over the workspace file in in,
// as LookAtImage its look.
func (f *Factoryd) Digest(ctx context.Context, taskID string, in DigestIn) (string, error) {
	b, err := call(ctx, f.HTTP, http.MethodPost, f.BaseURL+"/internal/digest",
		http.Header{"Authorization": {"Bearer " + f.Token}},
		struct {
			TaskID string `json:"task_id"`
			DigestIn
		}{taskID, in})
	return factorydText(b, err)
}

// Brief returns factoryd's text of task taskID's card brief, which its
// session's worker reads.
func (f *Factoryd) Brief(ctx context.Context, taskID string) (string, error) {
	b, err := call(ctx, f.HTTP, http.MethodPost, f.BaseURL+"/internal/brief",
		http.Header{"Authorization": {"Bearer " + f.Token}},
		struct {
			TaskID string `json:"task_id"`
		}{taskID})
	return factorydText(b, err)
}

// StoreSessionBrief stores at factoryd text, the first message of task
// taskID's session session, which its worker built.
func (f *Factoryd) StoreSessionBrief(ctx context.Context, taskID, session, text string) error {
	_, err := call(ctx, f.HTTP, http.MethodPost, f.BaseURL+"/internal/session-brief",
		http.Header{"Authorization": {"Bearer " + f.Token}},
		struct {
			TaskID  string `json:"task_id"`
			Session string `json:"session"`
			Text    string `json:"text"`
		}{taskID, session, text})
	return factorydErr(err)
}

// factorydText is the text of factoryd's {"text"} answer b; a refusal is
// factoryd's one sentence as the error (factorydErr).
func factorydText(b []byte, err error) (string, error) {
	if err != nil {
		return "", factorydErr(err)
	}
	var res struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return "", err
	}
	return res.Text, nil
}

// factorydErr is err of a call to factoryd: a refusal and factoryd's 502
// (what it asked did not answer) are its one sentence; another 5xx says
// nothing an agent can act on.
func factorydErr(err error) error {
	var pe *providerError
	if !errors.As(err, &pe) {
		return err
	}
	if sentence := strings.TrimSpace(pe.body); sentence != "" && (pe.status < 500 || pe.status == http.StatusBadGateway) {
		return errors.New(sentence)
	}
	return fmt.Errorf("factoryd answered %d", pe.status)
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
	Reason         string `json:"reason" jsonschema:"why the question comes up now and what waiting for the answer costs"`
	Recommendation string `json:"recommendation" jsonschema:"what you would do and why, at most 2000 bytes"`
	// a choice a button cannot carry is refused; a list factoryd cannot offer is
	// dropped there
	Choices []string `json:"choices,omitempty" jsonschema:"offer 2 to 5 short answers the human can pick with one click, each at most 60 characters in plain words; always leave free text possible"`
	Choice  string   `json:"choice,omitempty" jsonschema:"the choice you recommend, by its label or its number from 1"`
}

type RequestInfoIn struct {
	Question string `json:"question" jsonschema:"one question for the issue's author"`
	Tried    string `json:"tried" jsonschema:"what you tried to reproduce or find the fault"`
	Needs    string `json:"needs" jsonschema:"one sentence: what information would let you go on"`
}

type RequestToolIn struct {
	Tool string `json:"tool" jsonschema:"the tool's Debian package or binary name, such as chromium, ffmpeg or python3"`
	Need string `json:"need" jsonschema:"what you need it for in this card"`
}

type RequestCutIn struct {
	Reason string   `json:"reason" jsonschema:"why the card is bigger than one card should be"`
	Parts  []string `json:"parts" jsonschema:"the 2 to 5 parts you propose, each one card's work"`
}

type ProofsIn struct {
	Proofs []ProofIn `json:"proofs" jsonschema:"one entry per criterion and hold-out your brief lists"`
}

type ProofIn struct {
	Criterion string `json:"criterion" jsonschema:"the criterion's or the hold-out's id"`
	Path      string `json:"path" jsonschema:"the test file, relative to the repository's root"`
	Suite     string `json:"suite" jsonschema:"the package bash check test runs, such as ./proofs/size_bound"`
	Name      string `json:"name" jsonschema:"the test's name, such as TestSizeBound"`
	Code      string `json:"code" jsonschema:"the test file's whole content"`
	Outcome   string `json:"outcome" jsonschema:"what your own run of bash check test on main showed: red, green or unrunnable"`
	Artifact  string `json:"artifact,omitempty" jsonschema:"for a judgement or human criterion: the file the test writes under CHECK_ARTIFACTS"`
}

// maxProofsCall is the code one file_proofs call may carry, summed.
const maxProofsCall = 512 << 10

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

type DigestIn struct {
	Path string `json:"path" jsonschema:"a text file in the workspace: absolute under /workspace or relative to the repository"`
	Ask  string `json:"ask,omitempty" jsonschema:"what you need from the file, at most 2000 characters; leave it out for a summary"`
}

// TaskCostsIn is the task_costs tool's input: without task_id the
// caller's own figures, with it those of that task of the caller's
// project, a read of the classes that supervise.
type TaskCostsIn struct {
	TaskID string `json:"task_id,omitempty" jsonschema:"another task of the caller's project, a read of the classes that supervise (a plan session); leave it out for the caller's own figures"`
}

// taskCostsAnswer is the task_costs tool's answer: the figures of the
// task it read.
type taskCostsAnswer struct {
	TaskID       string  `json:"task_id"`
	BudgetEUR    float64 `json:"budget_eur"`
	SpentEUR     float64 `json:"spent_eur"`
	RemainingEUR float64 `json:"remaining_eur"`
	Calls        int     `json:"calls"`
	PromptTokens int     `json:"prompt_tokens"`
	CachedTokens int     `json:"cached_tokens"`
	OutputTokens int     `json:"output_tokens"`
}

// The bounds of a digest's arguments; the file's is policy.MaxDigestBytes.
const (
	maxDigestPath = 4096
	maxDigestAsk  = 2000
)

// plainArrays is T's input schema with each list of strings or proofs a
// plain array: the inferred type list ["null", "array"] is one some
// models' tool calls refuse.
func plainArrays[T any]() *jsonschema.Schema {
	proof, err := jsonschema.For[ProofIn](nil)
	if err != nil {
		panic(err)
	}
	s, err := jsonschema.For[T](&jsonschema.ForOptions{TypeSchemas: map[reflect.Type]*jsonschema.Schema{
		reflect.TypeFor[[]string]():  {Type: "array", Items: &jsonschema.Schema{Type: "string"}},
		reflect.TypeFor[[]ProofIn](): {Type: "array", Items: proof}}})
	if err != nil {
		panic(err)
	}
	return s
}

// withBriefLines is s with the schema text of a brief's fields, the same
// three lines at every filing door.
func withBriefLines(s *jsonschema.Schema) *jsonschema.Schema {
	for name, line := range map[string]string{"title": writingguide.TitleLine, "why": writingguide.WhyLine, "summary": writingguide.SummaryLine} {
		s.Properties[name].Description = line
	}
	return s
}

var (
	delegateSchema = withBriefLines(plainArrays[DelegateIn]())
	escalateSchema = plainArrays[EscalateIn]()
	cutSchema      = plainArrays[RequestCutIn]()
	proofsSchema   = plainArrays[ProofsIn]()
)

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
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolEscalate, Description: "Escalate when the task cannot be completed without a decision, access or information you lack. " +
		"Who answers first is policy, the project agent or a human; an ask nothing settles always reaches a human. " +
		"This session ends; the task resumes with the answer. Free.", InputSchema: escalateSchema},
		func(ctx context.Context, _ *mcp.CallToolRequest, in EscalateIn) (*mcp.CallToolResult, any, error) {
			return s.escalate(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolRequestInfo, Description: "Ask the issue's author for information instead of guessing, when you could not reproduce the problem: what you tried and what you need. This session ends. The task resumes with the answer or closes after 14 days. Free."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in RequestInfoIn) (*mcp.CallToolResult, any, error) {
			return s.requestInfo(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolRequestTool, Description: "Ask for a tool the sandbox lacks, such as chromium, ffmpeg or python3, instead of installing it. Your work is pushed, a card adds the tool to a project image, and this card restarts on it. Name the Debian package or binary and why."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in RequestToolIn) (*mcp.CallToolResult, any, error) {
			return s.requestTool(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolRequestCut, Description: "Ask to cut this card when its work is bigger than one card should be: give why and 2 to 5 parts. The check-in judges it. If it cuts, this session stops, your work is pushed and the planner plans the rest. Free.", InputSchema: cutSchema},
		func(ctx context.Context, _ *mcp.CallToolRequest, in RequestCutIn) (*mcp.CallToolResult, any, error) {
			return s.requestCut(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolDelegate, Description: "File a follow-up card for routine work this card need not do. It runs on the cheap rung after this card lands and builds on its merge. Give a title, why, summary and a brief a weaker model acts on alone. Free.", InputSchema: delegateSchema},
		func(ctx context.Context, _ *mcp.CallToolRequest, in DelegateIn) (*mcp.CallToolResult, any, error) {
			return s.delegate(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolFileProofs, Description: "File one test per criterion and hold-out, each with path, suite, name, whole code and what your run of bash check test on main showed. A refusal names every finding: fix it and file again, three filings at most. The last passing one stands.", InputSchema: proofsSchema},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ProofsIn) (*mcp.CallToolResult, any, error) {
			return s.fileProofs(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, filingTool[answers.ScreeningAnswer](policy.ToolFileScreening, "File your verdict on the issue. A refusal names what to fix: file again in this session. A stored verdict is your answer; end the session. Free."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in answers.ScreeningAnswer) (*mcp.CallToolResult, any, error) {
			return s.fileAnswer(ctx, c, policy.ToolFileScreening, in), nil, nil
		})
	mcp.AddTool(srv, filingTool[answers.SizeAnswer](policy.ToolFileSize, "File your size for the card. A refusal names what to fix: file again in this session. A stored size is your answer; end the session. Free."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in answers.SizeAnswer) (*mcp.CallToolResult, any, error) {
			return s.fileAnswer(ctx, c, policy.ToolFileSize, in), nil, nil
		})
	mcp.AddTool(srv, filingTool[answers.MapAnswer](policy.ToolFileMap, "File your map of the repository. A refusal names what to fix: file again in this session. A stored map is your answer; end the session. Free."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in answers.MapAnswer) (*mcp.CallToolResult, any, error) {
			return s.fileAnswer(ctx, c, policy.ToolFileMap, in), nil, nil
		})
	mcp.AddTool(srv, filingTool[answers.SpecAnswer](policy.ToolFileSpec, "File your spec, summary and questions. A refusal names what to fix: file again in this session. A stored spec is your answer; end the session. Free."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in answers.SpecAnswer) (*mcp.CallToolResult, any, error) {
			return s.fileAnswer(ctx, c, policy.ToolFileSpec, in), nil, nil
		})
	mcp.AddTool(srv, filingTool[answers.CriteriaAnswer](policy.ToolFileCriteria, "File the criteria of the spec. A refusal names what to fix: file again in this session. Stored criteria are your answer; end the session. Free."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in answers.CriteriaAnswer) (*mcp.CallToolResult, any, error) {
			return s.fileAnswer(ctx, c, policy.ToolFileCriteria, in), nil, nil
		})
	mcp.AddTool(srv, filingTool[answers.CardsAnswer](policy.ToolFileCards, "File the cards of this round. A refusal names what to fix: file again in this session. A stored round is your answer; end the session. Free."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in answers.CardsAnswer) (*mcp.CallToolResult, any, error) {
			return s.fileAnswer(ctx, c, policy.ToolFileCards, in), nil, nil
		})
	mcp.AddTool(srv, filingTool[answers.ReplanAnswer](policy.ToolFileReplan, "File the plan again, changed. A refusal names what to fix: file again in this session. A stored plan is your answer; end the session. Free."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in answers.ReplanAnswer) (*mcp.CallToolResult, any, error) {
			return s.fileAnswer(ctx, c, policy.ToolFileReplan, in), nil, nil
		})
	mcp.AddTool(srv, filingTool[answers.RevertAnswer](policy.ToolFileRevert, "File the merged cards whose work comes off main. A refusal names what to fix: file again in this session. A stored list is your answer; end the session. Free."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in answers.RevertAnswer) (*mcp.CallToolResult, any, error) {
			return s.fileAnswer(ctx, c, policy.ToolFileRevert, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolManual, Description: "Read the factory manual: what the factory is, the card states, the gates your pull request passes. Free."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return s.manualRead(c), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolTaskCosts, Description: "Read the task's cost figures: budget, spent and remaining EUR, the count of its model calls and their prompt, cached and output tokens; " +
		"task_id names another task of the caller's project, a read of the classes that supervise. Free."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in TaskCostsIn) (*mcp.CallToolResult, any, error) {
			return s.taskCosts(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolDeploymentHealth, Description: "Read a Deployment of this card's project as the cluster reports it now: replicas, pods with each container's restarts, state and last termination, and the last hour's events. No logs, no Secrets. Omit namespace to search the project's namespaces."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in DeploymentHealthIn) (*mcp.CallToolResult, any, error) {
			return s.deploymentHealth(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolAnalyzeImage, Description: "Have a vision model describe one image: a workspace file or an image of the card's issue. Name exactly one of path and card_image. ask says what to look for. PNG, JPEG, WebP or GIF, at most 8 MiB. Books as a model call."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in AnalyzeImageIn) (*mcp.CallToolResult, any, error) {
			return s.analyzeImage(ctx, c, in), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: policy.ToolDigest, Description: "Have a small model read one UTF-8 text file of your workspace, at most 128 KiB, and answer your ask. Only the answer enters your context. Use it on a big file with a pointed ask. Books as a cheap-model call."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in DigestIn) (*mcp.CallToolResult, any, error) {
			return s.digest(ctx, c, in), nil, nil
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
	return c.pv.Document.AllowsSessionTool(c.task.Classes, policy.ToolCard(c.task.Source, c.task.ToolImage), tool) && policy.OffersTool(tool, p.Namespaces)
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
	text, err := s.runPaid(ctx, c, tool, toolCap, p)
	var refused *toolRefusal
	switch {
	case err == nil:
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
	case errors.As(err, &refused):
		return s.refuseTool(c, tool, refused.decision, refused.reason)
	default:
		return toolError(err.Error())
	}
}

// runPaid holds, runs and books one paid tool call for either door: the
// call's worst case is held against the caps the caller read, the
// provider runs under the hold, and the real cost (the hold when unknown)
// is booked with the task's spend and audited under the policy version. A
// refusal comes back as a *toolRefusal; a failure is already audited.
func (s *Server) runPaid(ctx context.Context, c *toolCaller, tool string, toolCap policy.ToolCap, p paidCall) (string, error) {
	if c.task.Status != "running" {
		return "", &toolRefusal{"refused_task_status", "the task is " + c.task.Status}
	}
	worst, decision, reason := p.prepare(ctx)
	if decision != "" {
		return "", &toolRefusal{decision, reason}
	}
	project, projectEUR := c.pv.ProjectCap(c.task.Repository)
	req := policy.HoldRequest{
		TaskID: c.task.ID, Kind: policy.KindTool, Ref: tool, Session: c.session, EUR: worst, Cap: toolCap,
		DayEUR:   c.pv.Document.Budget.DayEUR,
		MonthEUR: c.pv.Document.Budget.MonthEUR,
		Project:  project, ProjectEUR: projectEUR,
	}
	if c.session != "" {
		// a session's paid tool call holds the card's gate reserve out
		// as any of its calls does, the planner's among them; the
		// callers with no session (the checkin) keep the full headroom
		req.ReserveEUR = c.pv.Document.Limits.GateReserve()
	}
	hold, err := s.opts.Store.TakeHold(ctx, req)
	var refusal *policy.Refusal
	if errors.As(err, &refusal) {
		return "", &toolRefusal{refusal.Decision, refusal.Reason}
	}
	if err != nil {
		s.auditTool(c, tool, "error_store", err.Error(), nil, nil)
		return "", fmt.Errorf("the factory could not reserve the call's cost: %w", err)
	}
	ctx, up := s.tel.startCall(ctx, c.task, "tool", tool, hold.EUR)
	defer up.end()
	// a disconnecting agent must not cut a call the provider bills; the
	// connectors' client timeouts bound it
	text, cost, callErr := p.run(context.WithoutCancel(ctx))
	up.status = toolStatus(callErr)
	decision, note := "allowed", ""
	if cost == nil {
		note = fmt.Sprintf("; cost not reported, booked the hold %s EUR", textutil.EUR(hold.EUR))
	}
	// the provider billed whether or not the agent is still listening; a
	// cost past the hold (the arguments were bounded to it) is booked in
	// full, so the caps refuse the next call from the true headroom
	b, err := s.opts.Store.ReleaseHold(context.Background(), hold, cost, policy.Usage{})
	if err != nil {
		note += fmt.Sprintf("; hold %d not released: %v", hold.ID, err)
		log.Printf("task %s: tool %s: hold %d not released: %v", c.task.ID, tool, hold.ID, err) // #nosec G706 -- the task id is the stored row's, found by the verified caller; tool is one of the policy constants
		b.Spent = c.task.SpentEUR
	}
	up.booked(b.Booked, b.OverHold)
	spend := b.Spent
	if callErr != nil {
		s.auditBooked(c, tool, "error_tool", callErr.Error()+note, &spend, &hold.EUR, b.OverHold)
		return "", fmt.Errorf("%s failed: %w", tool, callErr)
	}
	s.auditBooked(c, tool, decision, strings.TrimPrefix(note, "; "), &spend, &hold.EUR, b.OverHold)
	return text, nil
}

// toolRefusal is a paid tool call the policy refused before the provider
// ran; the doors word it for their callers.
type toolRefusal struct{ decision, reason string }

func (e *toolRefusal) Error() string { return e.decision + ": " + e.reason }

// maxDoorBody bounds a chat tool door request body: a query or a URL.
const maxDoorBody = maxURL + 1024

// handleChatTool is the chat's tool door: the factoryd shared token on
// the factory-task header, as the completions door's identity check, runs
// one of the chat block's web tools on the chat task's account under its
// chat.tools caps. The agent door at /mcp admits sandbox JWTs only; no
// sandbox opens this one.
func (s *Server) handleChatTool(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pv, polErr := s.currentPolicy(ctx)
	var pid *int64
	if pv != nil {
		pid = &pv.ID
	}
	if !s.factorydBearer(r) {
		s.refuse(w, pid, r.Header.Get(HeaderFactoryTask), auditFactoryd, "refused_auth",
			"the chat's tool door takes the factoryd shared token, not a sandbox token", "", http.StatusUnauthorized, "authentication_error")
		return
	}
	taskID := r.Header.Get(HeaderFactoryTask)
	if taskID == "" {
		s.refuse(w, pid, "", auditFactoryd, "refused_auth",
			"a factoryd call names its task in "+HeaderFactoryTask, "", http.StatusBadRequest, "invalid_request_error")
		return
	}
	if polErr != nil {
		s.refuse(w, pid, taskID, auditFactoryd, "error_policy", polErr.Error(), "", http.StatusServiceUnavailable, "server_error")
		return
	}
	task, ok, err := s.opts.Store.Task(ctx, taskID)
	switch {
	case err != nil:
		s.refuse(w, pid, taskID, auditFactoryd, "error_store", err.Error(), "", http.StatusInternalServerError, "server_error")
		return
	case !ok:
		s.refuse(w, pid, taskID, auditFactoryd, "refused_task_unknown", "no task row for "+taskID, "", http.StatusNotFound, "invalid_request_error")
		return
	}
	tool := r.PathValue("tool")
	name, project, _ := pv.ProjectOf(task.Repository)
	c := &toolCaller{pv: pv, task: task}
	toolCap, listed := pv.Document.ChatOf(project).Tools[tool]
	reason := ""
	switch {
	case !slices.Contains(task.Classes, policy.ClassChat):
		reason = "the web tools serve the chat task, and " + task.ID + " is not one"
	case !listed:
		reason = tool + " is not in the chat block's tools (chat.tools)"
	}
	if reason != "" {
		s.auditTool(c, tool, "refused_capability",
			fmt.Sprintf("%s on %s: %s (chat.tools of %s under policy version %d)", tool, task.ID, reason, cmp.Or(name, "the policy"), pv.ID),
			&task.SpentEUR, nil)
		w.Header().Set(HeaderDecision, "refused_capability")
		writeError(w, http.StatusForbidden, "permission_error", reason)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDoorBody))
	if err != nil {
		s.refuse(w, pid, taskID, auditFactoryd, "refused_arguments", err.Error(), "", http.StatusBadRequest, "invalid_request_error")
		return
	}
	refuseArgs := func(reason string) {
		s.auditTool(c, tool, "refused_arguments", reason, &task.SpentEUR, nil)
		w.Header().Set(HeaderDecision, "refused_arguments")
		writeError(w, http.StatusBadRequest, "invalid_request_error", reason)
	}
	var p paidCall
	switch tool {
	case policy.ToolWebSearch:
		var in SearchIn
		if json.Unmarshal(body, &in) != nil {
			refuseArgs("the body is not a `web_search` call: `{\"query\": string, \"limit\": 1 to 10}`")
			return
		}
		p = s.search(c, in)
	case policy.ToolWebScrape:
		var in ScrapeIn
		if json.Unmarshal(body, &in) != nil {
			refuseArgs("the body is not a `web_scrape` call: `{\"url\": string}`")
			return
		}
		p = s.scrape(c, in)
	}
	text, err := s.runPaid(ctx, c, tool, toolCap, p)
	var refused *toolRefusal
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, text) // #nosec G104,G705 -- best-effort response write; served as text/plain, a browser renders the answer as text, not HTML
	case errors.As(err, &refused):
		s.auditTool(c, tool, refused.decision, refused.reason, &task.SpentEUR, nil)
		w.Header().Set(HeaderDecision, refused.decision)
		status, typ := http.StatusPaymentRequired, "budget_exceeded"
		switch refused.decision {
		case "refused_arguments":
			status, typ = http.StatusBadRequest, "invalid_request_error"
		case "refused_task_status":
			status, typ = http.StatusConflict, "invalid_request_error"
		}
		writeError(w, status, typ, refused.reason)
	default:
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
	}
}

func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

// refuseTool audits a refusal and tells the agent it was the policy, not
// the tool.
func (s *Server) refuseTool(c *toolCaller, tool, decision, reason string) *mcp.CallToolResult {
	s.auditTool(c, tool, decision, reason, &c.task.SpentEUR, nil)
	return toolError("refused by the factory policy (" + decision + "): " + reason +
		". The tool did not run. Do not retry this call.")
}

// refuseBrief audits factoryd's refusal of a brief and hands its lines to
// the agent whole: each names a fault and its fix, and the corrected call
// files.
func (s *Server) refuseBrief(c *toolCaller, tool, lines string) *mcp.CallToolResult {
	s.auditTool(c, tool, RefusedBrief, lines, &c.task.SpentEUR, nil)
	return toolError(lines)
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

// digestTimeout bounds a digest at factoryd as imageLookTimeout a look:
// the file's read and the Small call.
const digestTimeout = 145 * time.Second

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
		// a choice is a button a human reads: the agent writes it again
		if faults := writingguide.ChoiceFaults(ch); len(faults) > 0 {
			return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf(
				"choice %q: %s Each choice is a short answer in plain words, at most %d characters.", ch, strings.Join(faults, " "), writingguide.MaxChoice))
		}
	}
	pick, refusal := pickOf(strings.TrimSpace(in.Choice), choices)
	if refusal != "" {
		return s.refuseTool(c, tool, "refused_arguments", refusal)
	}
	if len(choices) > maxChoices || size > maxChoicesBytes {
		// factoryd's body cap would refuse the question with them
		log.Printf("task %s: escalate's choices are dropped: more than %d, or over %d bytes in all", c.task.ID, maxChoices, maxChoicesBytes) // #nosec G706 -- the task id is the stored row's, found by the verified JWT
		choices, pick = nil, 0
	}
	return s.handOver(ctx, c, tool, Escalation{Kind: KindEscalation, Question: q, Reason: why, Recommendation: rec, Choices: choices, Choice: pick}, "escalated",
		"escalated; this session will end. Policy decides who answers; an ask nothing settles always reaches a human, and the task resumes with the answer. Stop working now.")
}

// pickOf is the number from 1 of the one of choices pick names, by its
// label or its number; 0 for no pick, and a refusal naming the choices
// for a pick that names none of them.
func pickOf(pick string, choices []string) (int, string) {
	if pick == "" {
		return 0, ""
	}
	if n, err := strconv.Atoi(pick); err == nil && n >= 1 && n <= len(choices) {
		return n, ""
	}
	if i := slices.IndexFunc(choices, func(c string) bool { return strings.TrimSpace(c) == pick }); i >= 0 {
		return i + 1, ""
	}
	if len(choices) == 0 {
		return 0, fmt.Sprintf("choice %q names none of your choices: you offered none.", pick)
	}
	named := make([]string, len(choices))
	for i, c := range choices {
		named[i] = fmt.Sprintf("%d %q", i+1, strings.TrimSpace(c))
	}
	list := named[len(named)-1]
	if len(named) > 1 {
		list = strings.Join(named[:len(named)-1], ", ") + " or " + list
	}
	return 0, fmt.Sprintf("choice %q names none of your choices: name one by its label or its number, %s.", pick, list)
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
	if refusal := policy.CutAskRefusal(why, parts); refusal != "" {
		return s.refuseTool(c, tool, "refused_arguments", refusal)
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

// taskCosts answers the caller's own figures, or those of a named task
// of the caller's project for the classes that supervise (a plan
// session). Free, for the classes that list it.
func (s *Server) taskCosts(ctx context.Context, c *toolCaller, in TaskCostsIn) *mcp.CallToolResult {
	tool := policy.ToolTaskCosts
	if c.task.Status != "running" {
		return s.refuseTool(c, tool, "refused_task_status", "the task is "+c.task.Status)
	}
	if r := s.admitTool(c, tool); r != nil {
		return r
	}
	task := c.task
	if id := strings.TrimSpace(in.TaskID); id != "" {
		if !slices.ContainsFunc(c.task.Classes, func(name string) bool {
			class, ok := c.pv.Document.Classes[name]
			return ok && class.Session == policy.SessionPlan
		}) {
			return s.refuseTool(c, tool, "refused_capability",
				"`task_id` is a read of the classes that supervise (a plan session): leave it out for the caller's own figures")
		}
		named, ok, err := s.opts.Store.Task(ctx, id)
		switch {
		case err != nil:
			s.auditTool(c, tool, "error_store", textutil.Truncate(err.Error(), 500), &c.task.SpentEUR, nil)
			return toolError(err.Error())
		case !ok:
			return s.refuseTool(c, tool, "refused_arguments", "no such task "+id)
		}
		// the same scoping the chat's card read makes: the named task's
		// repository must resolve to the caller's own project
		project, _, _ := c.pv.ProjectOf(c.task.Repository)
		if namedProject, _, ok := c.pv.ProjectOf(named.Repository); !ok || namedProject != project {
			return s.refuseTool(c, tool, "refused_capability", fmt.Sprintf("task %s is not in the caller's project", id))
		}
		task = named
	}
	totals, ok, err := s.opts.Store.TaskCosts(ctx, task.ID)
	if err != nil {
		s.auditTool(c, tool, "error_store", textutil.Truncate(err.Error(), 500), &c.task.SpentEUR, nil)
		return toolError(err.Error())
	}
	if !ok {
		return s.refuseTool(c, tool, "refused_arguments", "no such task "+task.ID)
	}
	s.auditTool(c, tool, "costs_read", task.ID, &c.task.SpentEUR, nil)
	b, _ := json.Marshal(taskCostsAnswer{
		TaskID: task.ID, BudgetEUR: task.BudgetEUR, SpentEUR: task.SpentEUR, RemainingEUR: task.BudgetEUR - task.SpentEUR,
		Calls: totals.Calls, PromptTokens: totals.PromptTokens, CachedTokens: totals.CachedTokens, OutputTokens: totals.OutputTokens,
	})
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
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
		return s.refuseTool(c, tool, "refused_arguments", "name exactly one of `path` (a workspace file) and `card_image` (an image on the card's issue)")
	case len(in.Path) > maxImagePath:
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf("path is longer than %d bytes", maxImagePath))
	case len(in.CardImage) > maxCardImage:
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf("`card_image` is longer than %d bytes: name the image or paste its /attachments/ link", maxCardImage))
	case utf8.RuneCountInString(in.Ask) > maxImageAsk:
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

// digest has factoryd read one workspace file and answer the ask over it
// with the Small model. Free, for the classes that list it; the call
// books on the task's account as a model call.
func (s *Server) digest(ctx context.Context, c *toolCaller, in DigestIn) *mcp.CallToolResult {
	tool := policy.ToolDigest
	if c.task.Status != "running" {
		return s.refuseTool(c, tool, "refused_task_status", "the task is "+c.task.Status)
	}
	if r := s.admitTool(c, tool); r != nil {
		return r
	}
	in.Path, in.Ask = strings.TrimSpace(in.Path), strings.TrimSpace(in.Ask)
	switch {
	case in.Path == "":
		return s.refuseTool(c, tool, "refused_arguments", "path is required: a text file of the workspace, absolute under /workspace or relative to the repository")
	case len(in.Path) > maxDigestPath:
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf("path is longer than %d bytes", maxDigestPath))
	case utf8.RuneCountInString(in.Ask) > maxDigestAsk:
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf("ask is longer than %d characters: ask for the one thing you need from the file", maxDigestAsk))
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), digestTimeout)
	defer cancel()
	text, err := s.opts.Digester.Digest(ctx, c.task.ID, in)
	if err != nil {
		s.auditTool(c, tool, "error_digest", textutil.Truncate(err.Error(), 500), &c.task.SpentEUR, nil)
		return toolError(err.Error())
	}
	s.auditTool(c, tool, "digested", in.Path, &c.task.SpentEUR, nil)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// fileProofs hands a prover's filing to factoryd, which checks its shape
// against the proofs the plan owes: a refusal carries every finding back
// into the session with the filings left, an accepted filing stands until
// a later one passes. The audit names the filing and counts its findings,
// never their words, which may name a hold-out. Free, for the classes
// that list it.
func (s *Server) fileProofs(ctx context.Context, c *toolCaller, in ProofsIn) *mcp.CallToolResult {
	tool := policy.ToolFileProofs
	if c.task.Status != "running" {
		return s.refuseTool(c, tool, "refused_task_status", "the task is "+c.task.Status)
	}
	if r := s.admitTool(c, tool); r != nil {
		return r
	}
	code := 0
	for _, p := range in.Proofs {
		code += len(p.Code)
	}
	switch {
	case len(in.Proofs) == 0:
		return s.refuseTool(c, tool, "refused_arguments", "proofs is required: one entry per criterion and hold-out your brief lists")
	case code > maxProofsCall:
		return s.refuseTool(c, tool, "refused_arguments", fmt.Sprintf("the proofs carry %d bytes of code, and one call carries at most %d", code, maxProofsCall))
	case s.opts.ProofsFiler == nil:
		return s.refuseTool(c, tool, "refused_capability", "no proofs hand-over is wired")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), escalationTimeout)
	defer cancel()
	res, err := s.opts.ProofsFiler.FileProofs(ctx, c.task.ID, in.Proofs)
	if err != nil {
		s.auditTool(c, tool, "error_proofs", textutil.Truncate(err.Error(), 500), &c.task.SpentEUR, nil)
		return toolError("the filing did not reach factoryd: " + err.Error())
	}
	if len(res.Findings) > 0 {
		s.auditTool(c, tool, "proofs_refused", fmt.Sprintf("filing %d, %d findings", res.Filing, len(res.Findings)), &c.task.SpentEUR, nil)
		next := fmt.Sprintf("correct it and file again in this session. Filings left: %d", res.Left)
		if res.Left == 0 {
			next = "no filing is left: end the session"
		}
		return toolError("the filing was refused:\n- " + strings.Join(res.Findings, "\n- ") + "\n" + next)
	}
	s.auditTool(c, tool, "proofs_filed", fmt.Sprintf("filing %d, %d proofs", res.Filing, len(in.Proofs)), &c.task.SpentEUR, nil)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
		Text: fmt.Sprintf("filing %d stands; factoryd runs every test on main after this session. Filings left: %d. You may end the session.", res.Filing, res.Left)}}}
}

// filingTool is the tool name files a T answer through: its schema is the
// answer tool's of T.
func filingTool[T any](name, description string) *mcp.Tool {
	b, err := json.Marshal(prompts.AnswerTool(reflect.TypeFor[T]()).Parameters)
	if err != nil {
		panic(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(b, &schema); err != nil {
		panic(err)
	}
	return &mcp.Tool{Name: name, Description: description, InputSchema: &schema}
}

// fileAnswer hands a session's answer to factoryd, which checks it against
// the task it answers for: a refusal carries every finding back into the
// session, a stored answer is the session's answer. Free, for the classes
// that list the tool.
func (s *Server) fileAnswer(ctx context.Context, c *toolCaller, tool string, answer any) *mcp.CallToolResult {
	if c.task.Status != "running" {
		return s.refuseTool(c, tool, "refused_task_status", "the task is "+c.task.Status)
	}
	if r := s.admitTool(c, tool); r != nil {
		return r
	}
	if s.opts.AnswerFiler == nil {
		return s.refuseTool(c, tool, "refused_capability", "no answer hand-over is wired")
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		return s.refuseTool(c, tool, "refused_arguments", err.Error())
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), escalationTimeout)
	defer cancel()
	res, err := s.opts.AnswerFiler.FileAnswer(ctx, c.task.ID, tool, raw)
	if err != nil {
		s.auditTool(c, tool, "error_answer", textutil.Truncate(err.Error(), 500), &c.task.SpentEUR, nil)
		return toolError("the answer did not reach factoryd: " + err.Error())
	}
	if len(res.Findings) > 0 {
		s.auditTool(c, tool, "answer_refused", fmt.Sprintf("%d findings", len(res.Findings)), &c.task.SpentEUR, nil)
		return toolError("the answer was refused:\n- " + strings.Join(res.Findings, "\n- ") + "\ncorrect it and file it again in this session")
	}
	s.auditTool(c, tool, "answer_stored", "", &c.task.SpentEUR, nil)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "stored; it is this session's answer. You may end the session."}}}
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
	res, err := s.opts.Delegator.Delegate(ctx, c.task.ID, Delegation{Title: in.Title, Why: in.Why, Summary: in.Summary, Brief: brief})
	var pe *providerError
	if errors.As(err, &pe) && pe.decision == RefusedBrief {
		return s.refuseBrief(c, tool, strings.TrimSpace(pe.body))
	}
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
		switch {
		case errors.As(err, &pe) && pe.status == http.StatusUnprocessableEntity:
			return s.refuseTool(c, tool, "refused_session", strings.TrimSpace(pe.body))
		case errors.As(err, &pe) && pe.decision == RefusedBrief:
			return s.refuseBrief(c, tool, strings.TrimSpace(pe.body))
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
