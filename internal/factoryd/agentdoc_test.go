package factoryd

import (
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
				ReviewModel: "big", SupervisorModel: "big", AuditModel: "big", CheckinModel: "big",
			}},
			Limits: policy.Limits{SupervisorTimeout: 10 * time.Second},
		}},
	}
}

// Every direct agent drives the real client against the fake gateway,
// every span it registers appears in the request that crossed the wire,
// and its messages tile with no unlabelled byte: the capture is proven,
// not assumed.
func TestAgentDocDirectCapturesEveryDirectAgent(t *testing.T) {
	toolsByID := map[string]bool{"checkin": true}
	for _, id := range AgentDocDirectIDs() {
		t.Run(id, func(t *testing.T) {
			g := agentdoc.NewGateway(`{"decision":"continue"}`)
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
			if id == "checkin" {
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
			var decoded strings.Builder
			for _, m := range b.Messages {
				decoded.WriteString(m.Content)
				for _, p := range m.Parts {
					decoded.WriteString(p.Text)
				}
			}
			for _, sp := range spans {
				if !strings.Contains(decoded.String(), sp.Text) && !strings.Contains(string(recs[0].Raw), sp.Text) {
					t.Errorf("span %s (%s) is not in the captured request", sp.Name, sp.Symbol)
				}
			}
			for _, m := range b.Messages {
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
				claims, err := agentdoc.Locate(m.Content, placeSpans)
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

// The supervisor-family agents call in JSON mode on the task's account;
// the check-in calls unstreamed with tools and no response format; the
// naming call streams on the small alias with a short answer.
func TestAgentDocDirectEnvelope(t *testing.T) {
	for _, id := range AgentDocDirectIDs() {
		t.Run(id, func(t *testing.T) {
			g := agentdoc.NewGateway(`{"decision":"continue"}`)
			defer g.Close()
			if _, err := AgentDocDirect(context.Background(), id, g.URL, agentDocPolicy(), nil); err != nil {
				t.Fatalf("capture: %v", err)
			}
			b, err := agentdoc.ParseBody(g.Bodies()[0].Raw)
			if err != nil {
				t.Fatal(err)
			}
			switch id {
			case "checkin":
				if b.Stream || b.ResponseFormat != "" || b.MaxTokens != 0 {
					t.Fatalf("the check-in calls unstreamed with no format and no limit, got %+v", b)
				}
				return
			case "title":
				if !b.Stream || b.ResponseFormat != "" || b.MaxTokens != titleTokens || b.Model != policy.Small {
					t.Fatalf("the naming call streams on the small alias with a %d-token answer, got %+v", titleTokens, b)
				}
				return
			}
			if b.ResponseFormat != "json_object" || b.Stream || b.MaxTokens <= 0 || b.Model != "big" {
				t.Fatalf("the supervisor family calls in JSON mode on the class's alias, got %+v", b)
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
		"title": policy.PurposeTitle,
	}
	for _, id := range AgentDocDirectIDs() {
		t.Run(id, func(t *testing.T) {
			g := agentdoc.NewGateway(`{"decision":"continue"}`)
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
		g := agentdoc.NewGateway(`{"decision":"continue"}`)
		first, err := AgentDocDirect(context.Background(), id, g.URL, agentDocPolicy(), nil)
		g.Close()
		if err != nil {
			t.Fatalf("%s: capture: %v", id, err)
		}
		g = agentdoc.NewGateway(`{"decision":"continue"}`)
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
