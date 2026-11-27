package factoryd

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// agentDocPolicy is the fixture policy: the coder class names every
// factoryd alias, the supervisor timeout is short. It stands for the
// infra policy.yaml the tool pins; aliases on pages come from it.
func agentDocPolicy() policy.Version {
	return policy.Version{
		Commit: "fixture",
		Tree: policy.Tree{Document: policy.Document{
			Version: 1,
			Classes: map[string]policy.Class{"coder": {
				ReviewModel: "big", SupervisorModel: "big", AuditModel: "big", CheckinModel: "big", VisionModel: "vision",
			}},
			Limits: policy.Limits{SupervisorTimeout: 10 * time.Second},
		}},
	}
}

// agentDocSections are a body's messages as the page locates them: each
// message's text, its parts' texts and images in order when it sent parts.
func agentDocSections(b agentdoc.Body) []string {
	var out []string
	for _, m := range b.Messages {
		text := m.Content
		for _, p := range m.Parts {
			text += p.Text + p.ImageURL
		}
		out = append(out, text)
	}
	return out
}

// Every direct agent drives the real client against the fake gateway,
// every span it registers appears in the request that crossed the wire,
// and its messages tile with no unlabelled byte: the capture is proven,
// not assumed.
func TestAgentDocDirectCapturesEveryDirectAgent(t *testing.T) {
	toolsByID := map[string]bool{"checkin": true, "case-checkin": true}
	for _, id := range AgentDocDirectIDs() {
		t.Run(id, func(t *testing.T) {
			g := agentdoc.NewGateway(AgentDocDirectAnswer(id))
			defer g.Close()
			spans, err := AgentDocDirect(context.Background(), id, g.URL, agentDocPolicy(), nil)
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
			if got := len(b.Tools) > 0; got != toolsByID[id] {
				t.Fatalf("tools on the wire: %v, want %v", got, toolsByID[id])
			}
			if len(spans) == 0 {
				t.Fatal("no spans registered")
			}
			if toolsByID[id] {
				for _, tool := range b.Tools {
					found := false
					for _, sp := range spans {
						found = found || sp.Name == "tool "+tool.Name
					}
					if !found {
						t.Errorf("the request carries tool %q with no span", tool.Name)
					}
				}
			}
			sections := agentDocSections(b)
			decoded := strings.Join(sections, "")
			for _, sp := range spans {
				if !strings.Contains(decoded, sp.Text) && !strings.Contains(string(recs[0].Raw), sp.Text) {
					t.Errorf("span %s (%s) is not in the captured request", sp.Name, sp.Symbol)
				}
			}
			for i, m := range b.Messages {
				place := agentdoc.PlaceUser
				if m.Role == "system" {
					place = agentdoc.PlaceSystem
				}
				var placeSpans []agentdoc.Span
				for _, sp := range spans {
					if sp.Place == place {
						placeSpans = append(placeSpans, sp)
					}
				}
				claims, err := agentdoc.Locate(sections[i], placeSpans)
				if err != nil {
					t.Fatalf("%s message: %v", m.Role, err)
				}
				for _, c := range claims {
					if c.Unlabelled {
						t.Errorf("%s message: %d unlabelled bytes %q", m.Role, c.End-c.Start, c.Text)
					}
				}
			}
		})
	}
}

// The supervisor-family agents call in JSON mode on the task's account,
// the plan namer and the follow's liveness call on the small alias. The
// check-ins call unstreamed with tools and no response format. The digest
// streams on the small alias with a short answer, the look on the class's
// vision alias with the operator's.
func TestAgentDocDirectEnvelope(t *testing.T) {
	jsonModel := map[string]string{"plan-namer": policy.Small, "follow-liveness": policy.Small}
	for _, id := range AgentDocDirectIDs() {
		t.Run(id, func(t *testing.T) {
			g := agentdoc.NewGateway(AgentDocDirectAnswer(id))
			defer g.Close()
			if _, err := AgentDocDirect(context.Background(), id, g.URL, agentDocPolicy(), nil); err != nil {
				t.Fatalf("capture: %v", err)
			}
			b, err := agentdoc.ParseBody(g.Bodies()[0].Raw)
			if err != nil {
				t.Fatal(err)
			}
			switch id {
			case "checkin", "case-checkin":
				if b.Stream || b.ResponseFormat != "" || b.MaxTokens != 0 || b.Model != "big" {
					t.Fatalf("the check-in calls big unstreamed with no format and no limit, got %+v", b)
				}
				return
			case "digest":
				if !b.Stream || b.ResponseFormat != "" || b.MaxTokens != 2048 || b.Model != policy.Small {
					t.Fatalf("the digest streams on the small alias with a 2048-token answer, got %+v", b)
				}
				return
			case "image-look":
				if !b.Stream || b.ResponseFormat != "" || b.MaxTokens != OperatorTokens || b.Model != "vision" {
					t.Fatalf("the look streams on the class's vision alias with a %d-token answer, got %+v", OperatorTokens, b)
				}
				return
			}
			if b.ResponseFormat != "json_object" || b.Stream || b.MaxTokens <= 0 || b.Model != cmp.Or(jsonModel[id], "big") {
				t.Fatalf("the supervisor family calls in JSON mode on the class's alias, got %+v", b)
			}
		})
	}
}

// Each capture sends what its real assembly sends: rung
// 1's statement on the case opens with its problem and closes with the
// calls left and the answer's due time beside the case's evidence tools,
// the namer asks on the plan's facts, the session's digest and look carry
// their doors' default asks and no system prompt, and the follow's
// liveness call asks for one word.
func TestAgentDocDirectMessages(t *testing.T) {
	type message struct{ role, prefix, suffix string }
	for id, want := range map[string]struct {
		messages []message
		tools    []string
	}{
		"case-checkin": {messages: []message{{role: "system", prefix: testPrompt(t, "case-checkin", rungPrompt).Text(), suffix: rungPrompt},
			{role: "user", prefix: "The review keeps raising findings on this card. How should it go on?\n",
				suffix: "\nCalls left 20; your answer is due within 28m30s."}},
			tools: []string{"transcript_search", "transcript_read", "card_facts", "card_step", "gates", "read_file", "grep", "cluster_facts", "answer"}},
		"plan-namer": {messages: []message{{role: "system", prefix: testPrompt(t, "plan-namer", namerPrompt).Text(), suffix: namerPrompt},
			{role: "user", prefix: "The input, as JSON:\n" + `"Issue title: Count the skipped rows\nSpec outcome: The counter counts every row the query returns, skipped ones included.\nSummary: One card fixes the counter; one criterion proves it.\nAlready taken by other open plans: Nightly report"`}}},
		"digest": {messages: []message{{role: "user",
			prefix: "Summarise this file.\n\n# The nightly job\n\nThe job counts the day's rows and writes the report at 02:00 UTC.\nA batch that fails is requeued once, and a second failure pages the human on call.\n"}}},
		"image-look": {messages: []message{{role: "user", prefix: "Describe this image.data:image/png;base64,iVBORw0KGgo"}}},
		"follow-liveness": {messages: []message{{role: "system", prefix: `Answer with a JSON object {"ok": true}.`},
			{role: "user", prefix: "The input, as JSON:\n" + `{"say":"ok"}`}}},
	} {
		t.Run(id, func(t *testing.T) {
			g := agentdoc.NewGateway(AgentDocDirectAnswer(id))
			defer g.Close()
			if _, err := AgentDocDirect(context.Background(), id, g.URL, agentDocPolicy(), nil); err != nil {
				t.Fatalf("capture: %v", err)
			}
			b, err := agentdoc.ParseBody(g.Bodies()[0].Raw)
			if err != nil {
				t.Fatal(err)
			}
			sections := agentDocSections(b)
			if len(b.Messages) != len(want.messages) {
				t.Fatalf("%d messages, want %d: %q", len(b.Messages), len(want.messages), sections)
			}
			for i, w := range want.messages {
				m, text := b.Messages[i], sections[i]
				if m.Role != w.role || !strings.HasPrefix(text, w.prefix) || !strings.HasSuffix(text, w.suffix) {
					t.Errorf("message %d is %s %q, want %s from %q to %q", i, m.Role, text, w.role, w.prefix, w.suffix)
				}
			}
			var tools []string
			for _, tl := range b.Tools {
				tools = append(tools, tl.Name)
			}
			if !slices.Equal(tools, want.tools) {
				t.Errorf("tools = %q, want %q", tools, want.tools)
			}
		})
	}
}

// The judgement capture drives judge's own call: its system prompt is the
// criterion's pinned prompt with the answer's form, its input the
// artifact alone. The review's criterion call is gone from the map.
func TestAgentDocJudgementDrivesTheJudgeCall(t *testing.T) {
	ids := AgentDocDirectIDs()
	if !slices.Contains(ids, "judgement") || slices.Contains(ids, "review-criteria") {
		t.Fatalf("ids = %q, want judgement and no review-criteria", ids)
	}
	g := agentdoc.NewGateway(`{"decision":"continue"}`)
	defer g.Close()
	spans, err := AgentDocDirect(context.Background(), "judgement", g.URL, agentDocPolicy(), nil)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	symbols := map[string]string{}
	for _, s := range spans {
		symbols[s.Name] = s.Symbol
	}
	for name, symbol := range map[string]string{"the criterion's prompt": "factoryd.PlanCriterion.Prompt", "the answer's form": "factoryd.judgementAnswer",
		"the artifact": "factoryd.GateArtifact"} {
		if symbols[name] != symbol {
			t.Errorf("span %q has symbol %q, want %q", name, symbols[name], symbol)
		}
	}
	b, err := agentdoc.ParseBody(g.Bodies()[0].Raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Messages) != 2 || !strings.HasSuffix(b.Messages[0].Content, "\n\n"+judgementAnswer) || !strings.Contains(b.Messages[1].Content, `"artifact":`) {
		t.Fatalf("messages = %+v, want the pinned prompt with the answer's form and the artifact", b.Messages)
	}
}

// The purpose header rides every call; it is what the gateway bills by.
func TestAgentDocDirectSendsThePurpose(t *testing.T) {
	purposeByID := map[string]string{
		"checkin": policy.PurposeCheckin, "model-review": policy.PurposeReview,
		"security-audit": policy.PurposeAudit, "gate-supervisor": policy.PurposeGateSupervisor,
		"judgement": policy.PurposeReview, "review-behaviours": policy.PurposeReview,
		"delta-planner": policy.PurposeReview, "holdout-proposer": policy.PurposeReview,
		"answer-screener": policy.PurposeReview, "brief-writer": policy.PurposeBrief, "case-brief-writer": policy.PurposeBrief,
		"case-checkin": policy.PurposeCheckin, "plan-namer": policy.PurposeTitle,
		"digest": policy.PurposeDigest, "image-look": policy.PurposeVision, "follow-liveness": policy.PurposeDigest,
	}
	for _, id := range AgentDocDirectIDs() {
		t.Run(id, func(t *testing.T) {
			g := agentdoc.NewGateway(AgentDocDirectAnswer(id))
			defer g.Close()
			if _, err := AgentDocDirect(context.Background(), id, g.URL, agentDocPolicy(), nil); err != nil {
				t.Fatalf("capture: %v", err)
			}
			rec := g.Bodies()[0]
			if rec.Purpose != purposeByID[id] {
				t.Fatalf("purpose %q, want %q", rec.Purpose, purposeByID[id])
			}
		})
	}
}

// The map regenerates byte for byte: every direct driver draws its
// markers from a pinned constant, so two captures of the same agent
// agree span for span.
func TestAgentDocDirectIsDeterministic(t *testing.T) {
	for _, id := range AgentDocDirectIDs() {
		g := agentdoc.NewGateway(AgentDocDirectAnswer(id))
		first, err := AgentDocDirect(context.Background(), id, g.URL, agentDocPolicy(), nil)
		g.Close()
		if err != nil {
			t.Fatalf("%s: capture: %v", id, err)
		}
		g = agentdoc.NewGateway(AgentDocDirectAnswer(id))
		second, err := AgentDocDirect(context.Background(), id, g.URL, agentDocPolicy(), nil)
		g.Close()
		if err != nil {
			t.Fatalf("%s: second capture: %v", id, err)
		}
		if len(first) != len(second) {
			t.Fatalf("%s: %d spans then %d", id, len(first), len(second))
		}
		for i := range first {
			a, b := first[i], second[i]
			if a.Name != b.Name || a.Symbol != b.Symbol || a.Place != b.Place || a.Text != b.Text || a.Kind != b.Kind {
				t.Errorf("%s: span %s differs between captures:\n%+v\n%+v", id, a.Name, a, b)
			}
		}
	}
}
