package acceptance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// TestRunSettlesAFalseTestOutcomeClaim proves the settle-by-run loop over
// the real store: the model's concerns bundles three failing-test claims,
// the sandbox pods the runner creates run them and exit 0 — the tests did
// not fail — so the finding drops on the runs' own evidence and the
// verdict turns approve with the model's own answer kept beside it, the
// recorded input carrying the head's check runs and the model_review row
// naming the turn.
func TestRunSettlesAFalseTestOutcomeClaim(t *testing.T) {
	s, db := divideStore(t)
	ctx := context.Background()
	task := factoryd.Task{ID: "t-settle", Status: "in-review", Objective: "settle the claims by run", Repository: "sf/code",
		Kind: "agent", Source: "api", Track: "direct", IssueNumber: 92, SHA: "c1", RequiredCapabilities: []string{"coder"}}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	fj, gw := &accForgejo{}, &accGateway{}
	fj.setHead("c1")
	gw.answer(accReply{content: `{"verdict":"concerns","summary":"the tests fail","findings":["internal/x: TestA, TestB and TestC fail at the head"],` +
		`"claims":[` +
		`{"finding":0,"kind":"test","symbol":"TestA","package":"internal/x","ref":"head","outcome":"fail"},` +
		`{"finding":0,"kind":"test","symbol":"TestB","package":"internal/x","ref":"head","outcome":"fail"},` +
		`{"finding":0,"kind":"test","symbol":"TestC","package":"internal/x","ref":"head","outcome":"fail"}]}`})
	kube := &accKube{bodies: []string{
		`{"type":"repro","repro":{"exit":0,"output":"ok"}}`,
		`{"type":"repro","repro":{"exit":0,"output":"ok"}}`,
		`{"type":"repro","repro":{"exit":0,"output":"ok"}}`,
	}}
	r := newLedgerRunner(t, &accLedger{PGStore: s}, serve(t, fj), serve(t, gw), kube)
	rv, err := r.ModelReview(ctx, "t-settle", factoryd.PullRequest{Number: 9}, "c1", factoryd.GateResults{}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if rv.Verdict != "approve" || rv.Turned != "concerns" || len(rv.Findings) != 0 || len(rv.Pending) != 0 {
		t.Fatalf("review = %+v, want the verdict turned approve with every finding dropped", rv)
	}
	if len(rv.Dropped) != 1 || !strings.Contains(rv.Dropped[0].Evidence, "go test -run Test") ||
		!strings.Contains(rv.Dropped[0].Evidence, "exit 0, the test did not fail") {
		t.Fatalf("drops = %+v, want the finding dropped on the go test runs' evidence", rv.Dropped)
	}
	if len(gw.inputs) != 1 {
		t.Fatalf("the model read %d inputs, want 1", len(gw.inputs))
	}
	var in struct {
		CheckRuns []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal([]byte(gw.inputs[0][strings.Index(gw.inputs[0], "{"):]), &in); err != nil {
		t.Fatalf("decode the input: %v\n%s", err, gw.inputs[0])
	}
	if len(in.CheckRuns) != 2 || in.CheckRuns[0].Context != "ci / build" || in.CheckRuns[0].State != "success" ||
		in.CheckRuns[1].Context != "ci / test" || in.CheckRuns[1].State != "success" {
		t.Fatalf("check_runs = %+v, want the head's two green checks", in.CheckRuns)
	}
	rows := accRows(t, db, "model_review")
	if len(rows) != 1 || rows[0].Detail["verdict"] != "approve" || rows[0].Detail["turned_from"] != "concerns" {
		t.Fatalf("model_review rows = %+v, want the turn named beside the model's own concerns", rows)
	}
}
