package acceptance

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// TestModelReviewInputCarriesTheCostState proves the review's cost and
// state block over a real store: a task row of budget 3.00 EUR, spent 0.75
// EUR and status in-review, and four model-call audit rows of prompt 1500,
// cached 300 and output 600 tokens in all, drive the Runner's ModelReview
// against the fake model gateway; the input the model read carries the
// block with remaining 2.25, a tool call's audit row not counted as a call.
func TestModelReviewInputCarriesTheCostState(t *testing.T) {
	s, db := divideStore(t)
	ctx := context.Background()
	task := factoryd.Task{ID: "t-cost", Status: "in-review", Objective: "the review's cost block", Repository: "sf/code",
		Kind: "agent", Source: "api", Track: "direct", BudgetEUR: 3, SpentEUR: 0.75, IssueNumber: 91, SHA: "c1",
		RequiredCapabilities: []string{"coder"}}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO audit (task_id, decision, reason, kind, tool, prompt_tokens, cached_tokens, output_tokens) VALUES
		('t-cost', 'allowed', '', 'agent', '', 500, 100, 200),
		('t-cost', 'allowed', '', 'agent', '', 500, 100, 200),
		('t-cost', 'allowed', '', 'agent', '', 300, 100, 100),
		('t-cost', 'allowed', '', 'agent', '', 200, 0, 100),
		('t-cost', 'allowed', '', 'agent', 'web_fetch', 999, 999, 999)`); err != nil {
		t.Fatal(err)
	}
	fj, gw := &accForgejo{}, &accGateway{}
	fj.setHead("c1")
	gw.answer(accReply{content: `{"verdict":"approve","summary":"fine"}`})
	r := newLedgerRunner(t, &accLedger{PGStore: s}, serve(t, fj), serve(t, gw))
	if _, err := r.ModelReview(ctx, "t-cost", factoryd.PullRequest{Number: 9}, "c1", factoryd.GateResults{}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if len(gw.inputs) != 1 {
		t.Fatalf("the model read %d inputs, want 1", len(gw.inputs))
	}
	var in struct {
		Cost map[string]any `json:"cost"`
	}
	if err := json.Unmarshal([]byte(gw.inputs[0][strings.Index(gw.inputs[0], "{"):]), &in); err != nil {
		t.Fatalf("decode the input: %v\n%s", err, gw.inputs[0])
	}
	want := map[string]any{"budget_eur": float64(3), "spent_eur": 0.75, "remaining_eur": 2.25,
		"calls": float64(4), "prompt_tokens": float64(1500), "cached_tokens": float64(300), "output_tokens": float64(600),
		"state": "in-review"}
	if !reflect.DeepEqual(in.Cost, want) {
		t.Fatalf("cost = %v\nwant  %v", in.Cost, want)
	}
}
