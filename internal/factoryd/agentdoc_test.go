package factoryd

import (
	"context"
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

// Every direct agent drives the real client against the fake gateway, and
// every span it registers appears in the request that crossed the wire:
// the capture is proven, not assumed.
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

// The purpose header rides every call; it is what the gateway bills by.
func TestAgentDocDirectSendsThePurpose(t *testing.T) {
	purposeByID := map[string]string{
		"checkin": policy.PurposeCheckin, "model-review": policy.PurposeReview,
		"security-audit": policy.PurposeAudit, "gate-supervisor": policy.PurposeGateSupervisor,
		"review-criteria": policy.PurposeReview, "review-behaviours": policy.PurposeReview,
		"delta-planner": policy.PurposeReview, "holdout-proposer": policy.PurposeReview,
		"holdout-redrafter": policy.PurposeReview, "answer-screener": policy.PurposeReview,
		"brief-writer": policy.PurposeBrief, "title": policy.PurposeTitle,
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
