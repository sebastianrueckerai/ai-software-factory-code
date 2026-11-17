package acceptance

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	aiharness "github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// accReproKube scripts the reproduction pods the review's settling runs
// ask for: one log body per pod in creation order, each served once,
// then the pod is gone; the rest of the cluster answers as accKube's.
type accReproKube struct {
	accKube
	mu     sync.Mutex
	bodies []string
	opens  map[string]int // pod name → its body's index, gone once served
	specs  []map[string]any
}

func (k *accReproKube) CreatePod(_ context.Context, _, name string, spec map[string]any) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.opens == nil {
		k.opens = map[string]int{}
	}
	k.opens[name] = len(k.specs)
	k.specs = append(k.specs, spec)
	return nil
}

func (k *accReproKube) PodDiagnosis(context.Context, string, string) (factoryd.PodDiagnosis, error) {
	return factoryd.PodDiagnosis{Phase: "Running"}, nil
}

func (k *accReproKube) PodStatus(context.Context, string, string) (string, int32, error) {
	return "Succeeded", 0, nil
}

func (k *accReproKube) PodLog(_ context.Context, _, name string) (io.ReadCloser, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	i, ok := k.opens[name]
	if !ok || i >= len(k.bodies) {
		return nil, factoryd.ErrNotFound
	}
	delete(k.opens, name)
	return io.NopCloser(strings.NewReader(k.bodies[i])), nil
}

// accReproEnvelope is one settling run's report from the worker in the
// pod: the run's exit and its output.
func accReproEnvelope(exit int, output string) string {
	b, _ := json.Marshal(aiharness.WorkerEnvelope{Type: "repro", Repro: &aiharness.Repro{Exit: exit, Output: output}})
	return string(b) + "\n"
}

// TestRunSettlesAFalseTestOutcomeClaim drives the review of a head whose
// answer holds concerns that bundle test-outcome claims the runs refute:
// each kept claim runs in its own sandbox pod this round, the run's
// evidence drops its finding, the verdict turns on the drops, and the
// turn reaches the PR comment and the timeline's model_review row —
// while the reviewer's input carries the head's own check runs beside
// the claims.
func TestRunSettlesAFalseTestOutcomeClaim(t *testing.T) {
	ctx := context.Background()
	s, db := divideStore(t)
	accCard(t, s, "t-run-1", "sf/code", "small", "c1", 91)
	fj, gw := &accForgejo{}, &accGateway{}
	fj.setHead("c1")
	ledger := &accLedger{PGStore: s}
	kube := &accReproKube{bodies: []string{
		accReproEnvelope(0, "ok  \tinternal/x\t0.01s"),
		accReproEnvelope(0, "ok  \tinternal/z\t0.01s"),
	}}
	r := newLedgerRunner(t, ledger, serve(t, fj), serve(t, gw), kube)
	gw.answer(accReply{content: `{"verdict":"concerns","summary":"the base is red",
		"findings":["internal/x: TestY fails at base","internal/z: TestZ fails at base"],
		"claims":[
			{"finding":0,"kind":"test","symbol":"TestY","package":"internal/x","ref":"base","outcome":"fail"},
			{"finding":1,"kind":"test","symbol":"TestZ","package":"internal/z","ref":"base","outcome":"fail"}]}`})
	rv, err := r.ModelReview(ctx, "t-run-1", factoryd.PullRequest{Number: 9}, "c1", factoryd.GateResults{}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if rv.Verdict != "approve" || rv.TurnedFrom != "concerns" || len(rv.Findings) != 0 || len(rv.Dropped) != 2 {
		t.Fatalf("review = %+v, want every finding dropped and the concerns turned approve", rv)
	}
	for i, want := range []string{
		"go test -run TestY internal/x at base (c1): exit 0, the test did not fail:\nok  \tinternal/x\t0.01s",
		"go test -run TestZ internal/z at base (c1): exit 0, the test did not fail:\nok  \tinternal/z\t0.01s",
	} {
		if rv.Dropped[i].Evidence != want {
			t.Errorf("drop %d evidence = %q, want %q", i, rv.Dropped[i].Evidence, want)
		}
	}
	if len(kube.specs) != 2 {
		t.Fatalf("pods = %d, want one settling run per kept claim", len(kube.specs))
	}
	if len(fj.comments) == 0 || !strings.Contains(fj.comments[0], "Verdict turned: the model's own verdict was **concerns**") {
		t.Fatalf("comments = %q, want the turn named on the pull request", fj.comments)
	}
	rows := accRows(t, db, factoryd.EventModelReview)
	if len(rows) != 1 || rows[0].Detail["verdict"] != "approve" || rows[0].Detail["turned_from"] != "concerns" || rows[0].Detail["reviewed"] != true {
		t.Fatalf("model_review rows %+v, want the turn beside the verdict", rows)
	}
	if input := gw.lastInput(t); !strings.Contains(input, `"checks":[{"context":"ci / test","state":"success"}]`) {
		t.Fatalf("the review input lacks the head's check runs: %s", input)
	}
}

func (g *accGateway) lastInput(t *testing.T) string {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.inputs) == 0 {
		t.Fatal("no review call recorded")
	}
	return g.inputs[len(g.inputs)-1]
}
