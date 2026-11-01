package console

import (
	"context"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// agentDocConsolePolicy is the fixture policy: its defaults name the
// chat's classes (the project agent on big, the vision looks on vision)
// and no project block overrides them.
func agentDocConsolePolicy() policy.Version {
	return policy.Version{Commit: "fixture", Tree: policy.Tree{Document: policy.Document{Version: 1}}}
}

// Every console agent drives the real code against the fake gateway —
// the chat POST handler for the project agent, the operator model's own
// calls for the rest — and every span it registers appears in the
// request that crossed the wire: the capture is proven, not assumed.
func TestAgentDocConsoleCapturesEveryConsoleAgent(t *testing.T) {
	for _, id := range AgentDocConsoleIDs() {
		t.Run(id, func(t *testing.T) {
			g := agentdoc.NewGateway("The capture runs no tools.")
			defer g.Close()
			spans, err := AgentDocConsole(context.Background(), id, g.URL, agentDocConsolePolicy(), nil)
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			recs := g.Bodies()
			if len(recs) != 1 {
				t.Fatalf("one call crosses the wire, got %d", len(recs))
			}
			b, err := agentdoc.ParseBody(recs[0].Raw)
			if err != nil {
				t.Fatal(err)
			}
			if len(spans) == 0 {
				t.Fatal("no spans registered")
			}
			var decoded strings.Builder
			for _, m := range b.Messages {
				decoded.WriteString(m.Content)
				for _, p := range m.Parts {
					decoded.WriteString(p.Text)
					decoded.WriteString(p.ImageURL)
				}
			}
			for _, sp := range spans {
				if !strings.Contains(decoded.String(), sp.Text) && !strings.Contains(string(recs[0].Raw), sp.Text) {
					t.Errorf("span %s (%s) is not in the captured request", sp.Name, sp.Symbol)
				}
			}
		})
	}
}

// The project agent streams with its offered tools on the chat's class;
// the compaction and vision calls ride the small or vision alias with
// no tools and their own answer limits.
func TestAgentDocConsoleEnvelope(t *testing.T) {
	toolsByID := map[string]bool{"project-agent": true}
	modelByID := map[string]string{"project-agent": policy.Big,
		"compact": policy.Small, "vision": policy.DefaultChat.VisionModel}
	tokensByID := map[string]int{"project-agent": factoryd.OperatorTokens,
		"compact": 2048, "vision": factoryd.OperatorTokens}
	for _, id := range AgentDocConsoleIDs() {
		t.Run(id, func(t *testing.T) {
			g := agentdoc.NewGateway("The capture runs no tools.")
			defer g.Close()
			if _, err := AgentDocConsole(context.Background(), id, g.URL, agentDocConsolePolicy(), nil); err != nil {
				t.Fatalf("capture: %v", err)
			}
			b, err := agentdoc.ParseBody(g.Bodies()[0].Raw)
			if err != nil {
				t.Fatal(err)
			}
			if !b.Stream || b.Model != modelByID[id] || b.MaxTokens != tokensByID[id] {
				t.Fatalf("%s streams on %s with max_tokens %d, got stream %t on %s with %d",
					id, modelByID[id], tokensByID[id], b.Stream, b.Model, b.MaxTokens)
			}
			if got := len(b.Tools) > 0; got != toolsByID[id] {
				t.Fatalf("tools on the wire: %v, want %v", got, toolsByID[id])
			}
			if id == "vision" {
				image := false
				for _, m := range b.Messages {
					for _, p := range m.Parts {
						image = image || p.ImageURL != ""
					}
				}
				if !image {
					t.Fatal("the vision call carries no image part")
				}
			}
		})
	}
}

// The purpose header rides every call; it is what the gateway bills by.
func TestAgentDocConsoleSendsThePurpose(t *testing.T) {
	purposeByID := map[string]string{
		"project-agent": policy.PurposeOperator,
		"compact":       policy.PurposeCompact,
		"vision":        policy.PurposeVision,
	}
	for _, id := range AgentDocConsoleIDs() {
		t.Run(id, func(t *testing.T) {
			g := agentdoc.NewGateway("The capture runs no tools.")
			defer g.Close()
			if _, err := AgentDocConsole(context.Background(), id, g.URL, agentDocConsolePolicy(), nil); err != nil {
				t.Fatalf("capture: %v", err)
			}
			if rec := g.Bodies()[0]; rec.Purpose != purposeByID[id] {
				t.Fatalf("purpose %q, want %q", rec.Purpose, purposeByID[id])
			}
		})
	}
}
