package console

import (
	"context"
	"slices"
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
			g := agentdoc.NewGateway(AgentDocConsoleAnswer(id))
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

// The project agent streams with its offered tools on the chat's class,
// and the research loop with its reads on big. The compaction, the
// naming, the digests and the look ride the small or vision alias with no
// tools and their own answer limits.
func TestAgentDocConsoleEnvelope(t *testing.T) {
	toolsByID := map[string]bool{"project-agent": true, "research": true}
	modelByID := map[string]string{"project-agent": policy.Big, "compact": policy.Small, "vision": policy.DefaultChat.VisionModel,
		"research": policy.Big, "title": policy.Small, "attachment-digest": policy.Small, "read-digest": policy.Small}
	tokensByID := map[string]int{"project-agent": factoryd.OperatorTokens, "compact": 2048, "vision": factoryd.OperatorTokens,
		"research": 2048, "title": 200, "attachment-digest": 2048, "read-digest": 2048}
	for _, id := range AgentDocConsoleIDs() {
		t.Run(id, func(t *testing.T) {
			g := agentdoc.NewGateway(AgentDocConsoleAnswer(id))
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
		"project-agent":     policy.PurposeOperator,
		"compact":           policy.PurposeCompact,
		"vision":            policy.PurposeVision,
		"research":          policy.PurposeResearch,
		"title":             policy.PurposeTitle,
		"attachment-digest": policy.PurposeDigest,
		"read-digest":       policy.PurposeDigest,
	}
	for _, id := range AgentDocConsoleIDs() {
		t.Run(id, func(t *testing.T) {
			g := agentdoc.NewGateway(AgentDocConsoleAnswer(id))
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

// The captures offer the tools production offers the fixture's writer:
// the chat's config gates request_image, the web tools and the
// routine tools on doors factoryd always wires, so the fixture console
// wires them too, and the research loop reads the web as the chat does.
func TestAgentDocConsoleOffersTheProductionTools(t *testing.T) {
	for id, want := range map[string][]string{
		"project-agent": {"request_image", "web_search", "web_scrape", "routines", "routine_run", "routine_skip"},
		"research":      {"tree", "grep", "file", "read_doc", "web_search", "web_scrape"},
	} {
		t.Run(id, func(t *testing.T) {
			g := agentdoc.NewGateway(AgentDocConsoleAnswer(id))
			defer g.Close()
			if _, err := AgentDocConsole(context.Background(), id, g.URL, agentDocConsolePolicy(), nil); err != nil {
				t.Fatalf("capture: %v", err)
			}
			b, err := agentdoc.ParseBody(g.Bodies()[0].Raw)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, tl := range b.Tools {
				names = append(names, tl.Name)
			}
			for _, name := range want {
				if !slices.Contains(names, name) {
					t.Errorf("the wire offers no %s: %q", name, names)
				}
			}
		})
	}
}

// The naming, the digests and the look send what their tools send: the
// title form and the naming rules over the thread's first message, each digest's default ask
// over what it reads with no system prompt, the agent's ask beside the
// attached image.
func TestAgentDocConsoleMessages(t *testing.T) {
	for id, want := range map[string][]string{
		"title":             {"system", "# Title\n\nA title names the work", "user", "Set the staging budget to 40 euros."},
		"attachment-digest": {"user", "Summarise this document.\n\nStaging budget: 40 EUR a month, raised from 30 on 2026-11-02.\nThe deploy waits on the budget's OK.\n"},
		"read-digest":       {"user", "Summarise this page.\n\nRelease notes 4.2\n\nThe staging deploy now waits for its budget's OK before it starts.\n"},
		"vision":            {"user", "What blocks the staging deploy?data:image/png;base64,iVBORw0KGgo"},
	} {
		t.Run(id, func(t *testing.T) {
			g := agentdoc.NewGateway(AgentDocConsoleAnswer(id))
			defer g.Close()
			if _, err := AgentDocConsole(context.Background(), id, g.URL, agentDocConsolePolicy(), nil); err != nil {
				t.Fatalf("capture: %v", err)
			}
			b, err := agentdoc.ParseBody(g.Bodies()[0].Raw)
			if err != nil {
				t.Fatal(err)
			}
			if len(b.Messages)*2 != len(want) {
				t.Fatalf("%d messages, want %d", len(b.Messages), len(want)/2)
			}
			for i, m := range b.Messages {
				text := m.Content
				for _, p := range m.Parts {
					text += p.Text + p.ImageURL
				}
				if m.Role != want[2*i] || !strings.HasPrefix(text, want[2*i+1]) {
					t.Errorf("message %d is %s %q, want %s from %q", i, m.Role, text, want[2*i], want[2*i+1])
				}
			}
		})
	}
}
