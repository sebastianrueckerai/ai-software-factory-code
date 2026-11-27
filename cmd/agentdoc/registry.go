package main

// The registry is the map's hand-maintained list: which agents exist,
// where they run, and which capture drives them. Everything else on a
// page is regenerated from the wire.

import (
	"context"
	"net/http"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/console"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// Agent is one documented agent of the map.
type Agent struct {
	ID     string // the page name
	Pkg    string // the package its builders live in, for the permalinks
	Family string // the assembly family the code map gives it
	Job    string // the inventory table's one line
	Runs   string // where it runs
	// Calls are the functions that make the model call the page shows, as
	// the model-call walk names them (agentdoc.ModelCalls).
	Calls []string
	// Answer is the fake gateway's scripted answer for the page, the
	// capture's package's own: the one answer that ends its call.
	Answer  func(id string) string
	Capture func(ctx context.Context, id, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error)
	// Loop marks a page the pinned binary captures through
	// factoryd.AgentDocLoop, not an in-process client: an opencode loop
	// mode.
	Loop bool
	// Config marks a page with no capture at all: what the config says
	// about an agent whose prompts are not ours.
	Config bool
	// Steps are the profile names the agent's request is built from.
	Steps []string
	// NoPrompt marks an agent whose request carries no prompt a profile
	// builds: a digest's or a look's ask over what it reads.
	NoPrompt bool
}

// registry lists every agent the map documents, in page order. The
// families are the code map's: A, the opencode loop modes and the
// harness's own calls the pinned binary captures, then B, the direct
// factoryd and console clients.
var registry = []Agent{
	{ID: "coding", Steps: []string{"coding"}, Pkg: "internal/factoryd", Family: "A", Job: "Implements the card: reads repo, edits, commits in sandbox", Runs: "agent-worker, the opencode loop in a gVisor pod", Loop: true, Calls: []string{"factoryd.(*Runner).RunSession"}},
	{ID: "screening", Steps: []string{"screening"}, Pkg: "internal/factoryd", Family: "A", Job: "Reads an issue pre-code, answers clear/suspect JSON", Runs: "agent-worker, report mode", Loop: true, Calls: []string{"factoryd.(*Runner).RunReport"}},
	{ID: "sizing", Steps: []string{"sizing"}, Pkg: "internal/factoryd", Family: "A", Job: "Sizes a card small/medium/large before first session", Runs: "agent-worker, report mode", Loop: true, Calls: []string{"factoryd.(*Runner).RunReport"}},
	{ID: "planner", Steps: []string{"planner"}, Pkg: "internal/factoryd", Family: "A", Job: "Reads repo, files a plan via the file_plan MCP tool", Runs: "agent-worker, report mode", Loop: true, Calls: []string{"factoryd.(*Runner).RunPlan"}},
	{ID: "prover", Steps: []string{"prover"}, Pkg: "internal/factoryd", Family: "A", Job: "Writes one blind test per criterion on main, files them via file_proofs", Runs: "agent-worker, report mode", Loop: true, Calls: []string{"factoryd.(*Runner).RunReport"}},
	{ID: "message-responder", Steps: []string{"coding", "message-responder"}, Pkg: "internal/factoryd", Family: "A", Job: "Answers POST /message pushes mid-session", Runs: "agent-worker, the session's next turn in the sandbox", Loop: true, Calls: []string{"factoryd.(*Runner).MessageSession"}},
	{ID: "opencode-subagents", Pkg: "internal/factoryd", Family: "A", Job: "opencode-internal task and explore delegation", Runs: "inside opencode, under the session config's agent block", Config: true},
	{ID: "checkin", Steps: []string{"checkin-session"}, Pkg: "internal/factoryd", Family: "B", Job: "Decides continue, message, handover, human or replan on a session trip", Runs: "factoryd, in-process loop, tools executed in the sandbox", Answer: factoryd.AgentDocDirectAnswer, Capture: factoryd.AgentDocDirect, Calls: []string{"factoryd.(*Runner).runCheckin"}},
	{ID: "case-checkin", Steps: []string{"case-checkin"}, Pkg: "internal/factoryd", Family: "B", Job: "Judges a case at rung 1: acts on it, or hands it up with a recommendation", Runs: "factoryd, in-process loop on the project's chat task, tools read by factoryd", Answer: factoryd.AgentDocDirectAnswer, Capture: factoryd.AgentDocDirect, Calls: []string{"factoryd.(*Runner).rung1Converse"}},
	{ID: "model-review", Steps: []string{"model-review"}, Pkg: "internal/factoryd", Family: "B", Job: "Reviews a diff part against the card's objective", Runs: "factoryd direct", Answer: factoryd.AgentDocDirectAnswer, Capture: factoryd.AgentDocDirect, Calls: []string{"factoryd.(*Runner).ModelReview"}},
	{ID: "security-audit", Steps: []string{"security-audit"}, Pkg: "internal/factoryd", Family: "B", Job: "Audits changes against the security checklist", Runs: "factoryd direct", Answer: factoryd.AgentDocDirectAnswer, Capture: factoryd.AgentDocDirect, Calls: []string{"factoryd.(*Runner).GateAudit"}},
	{ID: "gate-supervisor", Steps: []string{"gate-supervisor"}, Pkg: "internal/factoryd", Family: "B", Job: "Decides approve or escalate at the gates", Runs: "factoryd direct", Answer: factoryd.AgentDocDirectAnswer, Capture: factoryd.AgentDocDirect, Calls: []string{"factoryd.(*Runner).SuperviseGate"}},
	{ID: "judgement", Steps: []string{"judgement"}, Pkg: "internal/factoryd", Family: "B", Job: "Judges a judgement criterion's artifact with its pinned prompt", Runs: "factoryd direct", Answer: factoryd.AgentDocDirectAnswer, Capture: factoryd.AgentDocDirect, Calls: []string{"factoryd.(*Runner).judgeArtifacts"}},
	{ID: "review-behaviours", Steps: []string{"review-behaviours"}, Pkg: "internal/factoryd", Family: "B", Job: "Drafts the review behaviours text", Runs: "factoryd direct", Answer: factoryd.AgentDocDirectAnswer, Capture: factoryd.AgentDocDirect, Calls: []string{"factoryd.(*Runner).ModelReview"}},
	{ID: "delta-planner", Steps: []string{"delta-planner"}, Pkg: "internal/factoryd", Family: "B", Job: "Plans delta work between reviews", Runs: "factoryd direct", Answer: factoryd.AgentDocDirectAnswer, Capture: factoryd.AgentDocDirect, Calls: []string{"factoryd.(*Runner).draftDelta"}},
	{ID: "holdout-proposer", Steps: []string{"holdout-proposer"}, Pkg: "internal/factoryd", Family: "B", Job: "Proposes hold-out criteria", Runs: "factoryd direct", Answer: factoryd.AgentDocDirectAnswer, Capture: factoryd.AgentDocDirect, Calls: []string{"factoryd.(*Runner).proposeHoldouts"}},
	{ID: "answer-screener", Steps: []string{"answer-screener"}, Pkg: "internal/factoryd", Family: "B", Job: "Screens a late answer of the issue author", Runs: "factoryd direct", Answer: factoryd.AgentDocDirectAnswer, Capture: factoryd.AgentDocDirect, Calls: []string{"factoryd.(*Runner).ScreenAnswer"}},
	{ID: "brief-writer", Steps: []string{"brief-writer"}, Pkg: "internal/factoryd", Family: "B", Job: "Writes the brief of a resource request or a plan approval", Runs: "factoryd direct", Answer: factoryd.AgentDocDirectAnswer, Capture: factoryd.AgentDocDirect, Calls: []string{"factoryd.(*BriefWriter).ask"}},
	{ID: "case-brief-writer", Steps: []string{"case-brief-writer"}, Pkg: "internal/factoryd", Family: "B", Job: "Writes parts 3 and 4 of a Needs-you case", Runs: "factoryd direct", Answer: factoryd.AgentDocDirectAnswer, Capture: factoryd.AgentDocDirect, Calls: []string{"factoryd.(*BriefWriter).askCase"}},
	{ID: "plan-namer", Steps: []string{"plan-namer"}, Pkg: "internal/factoryd", Family: "B", Job: "Names a plan from its first revision", Runs: "factoryd direct", Answer: factoryd.AgentDocDirectAnswer, Capture: factoryd.AgentDocDirect, Calls: []string{"factoryd.(*Namer).Name"}},
	{ID: "digest", NoPrompt: true, Pkg: "internal/factoryd", Family: "B", Job: "Summarises one big read under an ask", Runs: "factoryd direct, behind a session's digest tool", Answer: factoryd.AgentDocDirectAnswer, Capture: factoryd.AgentDocDirect, Calls: []string{"factoryd.(*Runner).DigestHandler"}},
	{ID: "image-look", NoPrompt: true, Pkg: "internal/factoryd", Family: "B", Job: "Looks at one image a session names", Runs: "factoryd direct, behind a session's analyze_image tool", Answer: factoryd.AgentDocDirectAnswer, Capture: factoryd.AgentDocDirect, Calls: []string{"factoryd.(*Runner).ImageLookHandler"}},
	{ID: "follow-liveness", Steps: []string{"follow-liveness"}, Pkg: "internal/factoryd", Family: "B", Job: "Asks the small model for one word after a move rolls out", Runs: "factoryd direct, in the follow judgment", Answer: factoryd.AgentDocDirectAnswer, Capture: factoryd.AgentDocDirect, Calls: []string{"factoryd.(*Runner).followModelAnswers"}},
	{ID: "project-agent", Steps: []string{"project-agent"}, Pkg: "internal/console", Family: "B", Job: "The always-on repo chat agent; also answers escalations", Runs: "console, own tool loop", Answer: console.AgentDocConsoleAnswer, Capture: console.AgentDocConsole, Calls: []string{"console.(*turn).run"}},
	{ID: "research", Steps: []string{"research"}, Pkg: "internal/console", Family: "B", Job: "Researches one question for the project agent", Runs: "console, own tool loop behind the chat's research tool", Answer: console.AgentDocConsoleAnswer, Capture: console.AgentDocConsole, Calls: []string{"console.(*turn).research"}},
	{ID: "compact", Steps: []string{"compact"}, Pkg: "internal/console", Family: "B", Job: "Summarises a chat thread's older turns", Runs: "console", Answer: console.AgentDocConsoleAnswer, Capture: console.AgentDocConsole, Calls: []string{"console.(*turn).compact"}},
	{ID: "title", Steps: []string{"title"}, Pkg: "internal/console", Family: "B", Job: "Names a chat thread from its first message", Runs: "console, the naming call", Answer: console.AgentDocConsoleAnswer, Capture: console.AgentDocConsole, Calls: []string{"console.(*Console).nameThread"}},
	{ID: "attachment-digest", NoPrompt: true, Pkg: "internal/console", Family: "B", Job: "Summarises a document the human attached", Runs: "console, behind the chat's summarize_attachment tool", Answer: console.AgentDocConsoleAnswer, Capture: console.AgentDocConsole, Calls: []string{"console.(*turn).summarizeAttachment"}},
	{ID: "read-digest", NoPrompt: true, Pkg: "internal/console", Family: "B", Job: "Summarises one big read of the chat under an ask", Runs: "console, behind the chat's reads that take an ask", Answer: console.AgentDocConsoleAnswer, Capture: console.AgentDocConsole, Calls: []string{"console.(*turn).withDigest"}},
	{ID: "vision", NoPrompt: true, Pkg: "internal/console", Family: "B", Job: "Describes an image the human attached", Runs: "console", Answer: console.AgentDocConsoleAnswer, Capture: console.AgentDocConsole, Calls: []string{"console.(*turn).lookImage"}},
}

func agentByID(id string) *Agent {
	for i := range registry {
		if registry[i].ID == id {
			return &registry[i]
		}
	}
	return nil
}
