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
	// Answer is the fake gateway's scripted answer: JSON for the
	// JSON-mode clients, plain text that ends the loop for the chat.
	Answer  string
	Capture func(ctx context.Context, id, gwURL string, v policy.Version, hc *http.Client) ([]agentdoc.Span, error)
}

// registry lists every agent the map documents, in page order. The
// families are the code map's: B, the direct factoryd and console
// clients; the opencode loop modes (A) join in the next phase.
var registry = []Agent{
	{ID: "checkin", Pkg: "internal/factoryd", Family: "B", Job: "Decides continue, message, handover, human or replan on a session trip", Runs: "factoryd, in-process loop, tools executed in the sandbox", Answer: `{"decision":"continue"}`, Capture: factoryd.AgentDocDirect},
	{ID: "model-review", Pkg: "internal/factoryd", Family: "B", Job: "Reviews a diff for one criterion or one behaviour", Runs: "factoryd direct", Answer: `{"decision":"continue"}`, Capture: factoryd.AgentDocDirect},
	{ID: "security-audit", Pkg: "internal/factoryd", Family: "B", Job: "Audits changes against the security checklist", Runs: "factoryd direct", Answer: `{"decision":"continue"}`, Capture: factoryd.AgentDocDirect},
	{ID: "gate-supervisor", Pkg: "internal/factoryd", Family: "B", Job: "Decides approve or escalate at the gates", Runs: "factoryd direct", Answer: `{"decision":"continue"}`, Capture: factoryd.AgentDocDirect},
	{ID: "review-criteria", Pkg: "internal/factoryd", Family: "B", Job: "Drafts the review criteria text", Runs: "factoryd direct", Answer: `{"decision":"continue"}`, Capture: factoryd.AgentDocDirect},
	{ID: "review-behaviours", Pkg: "internal/factoryd", Family: "B", Job: "Drafts the review behaviours text", Runs: "factoryd direct", Answer: `{"decision":"continue"}`, Capture: factoryd.AgentDocDirect},
	{ID: "delta-planner", Pkg: "internal/factoryd", Family: "B", Job: "Plans delta work between reviews", Runs: "factoryd direct", Answer: `{"decision":"continue"}`, Capture: factoryd.AgentDocDirect},
	{ID: "holdout-proposer", Pkg: "internal/factoryd", Family: "B", Job: "Proposes hold-out criteria", Runs: "factoryd direct", Answer: `{"decision":"continue"}`, Capture: factoryd.AgentDocDirect},
	{ID: "holdout-redrafter", Pkg: "internal/factoryd", Family: "B", Job: "Rewrites a dropped hold-out criterion", Runs: "factoryd direct", Answer: `{"decision":"continue"}`, Capture: factoryd.AgentDocDirect},
	{ID: "answer-screener", Pkg: "internal/factoryd", Family: "B", Job: "Screens a late answer of the issue author", Runs: "factoryd direct", Answer: `{"decision":"continue"}`, Capture: factoryd.AgentDocDirect},
	{ID: "brief-writer", Pkg: "internal/factoryd", Family: "B", Job: "Writes the human approval brief", Runs: "factoryd direct", Answer: `{"decision":"continue"}`, Capture: factoryd.AgentDocDirect},
	{ID: "title", Pkg: "internal/factoryd", Family: "B", Job: "Names a chat thread from its first message", Runs: "the console's naming call, a factoryd direct client", Answer: `{"decision":"continue"}`, Capture: factoryd.AgentDocDirect},
	{ID: "project-agent", Pkg: "internal/console", Family: "B", Job: "The always-on repo chat agent; also answers escalations", Runs: "console, own tool loop", Answer: "The capture runs no tools.", Capture: console.AgentDocConsole},
	{ID: "compact", Pkg: "internal/console", Family: "B", Job: "Summarises a chat thread's older turns", Runs: "console", Answer: "The capture runs no tools.", Capture: console.AgentDocConsole},
	{ID: "vision", Pkg: "internal/console", Family: "B", Job: "Describes an image the member attached", Runs: "console", Answer: "The capture runs no tools.", Capture: console.AgentDocConsole},
}

func agentByID(id string) *Agent {
	for i := range registry {
		if registry[i].ID == id {
			return &registry[i]
		}
	}
	return nil
}
