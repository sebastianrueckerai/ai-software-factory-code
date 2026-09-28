package factoryd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	rollSHA    = "9f1e0c3a5b7d2e4f6a8c0b1d3e5f7a9c1b3d5e7f"
	rollParent = "8e2d4c6a0b1f3e5d7c9a1b3d5f7e9c1a3b5d7f9e"
)

// rollHub scripts the Forgejo reads of a roll: the newest k8s commit, the
// manifests at it and at its parent, and its push run.
type rollHub struct {
	parentPrefix string // "parent-", or "" when the parent's images are the commit's
	pushRun      bool
}

func (h *rollHub) forgejo(t *testing.T) *Forgejo {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const base = "/api/v1/repos/sf/ai-software-factory-code"
		q := r.URL.Query()
		var file string
		switch {
		case r.URL.Path == base+"/commits" && q.Get("sha") == "main" && q.Get("path") == "k8s" && q.Get("limit") == "1":
			file = "roll/commits.json"
		case strings.HasPrefix(r.URL.Path, base+"/raw/k8s/") && q.Get("ref") == rollSHA:
			file = "roll/" + strings.TrimPrefix(r.URL.Path, base+"/raw/k8s/")
		case strings.HasPrefix(r.URL.Path, base+"/raw/k8s/") && q.Get("ref") == rollParent:
			file = "roll/" + h.parentPrefix + strings.TrimPrefix(r.URL.Path, base+"/raw/k8s/")
		case r.URL.Path == base+"/actions/runs" && q.Get("event") == "push" && q.Get("head_sha") == rollSHA:
			if r.Header.Get("Authorization") != "token registrar-tok" {
				t.Errorf("actions/runs auth %q", r.Header.Get("Authorization"))
			}
			if !h.pushRun {
				w.Write([]byte(`{"workflow_runs":[],"total_count":0}`))
				return
			}
			file = "roll/push-run.json"
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		body, err := os.ReadFile("testdata/" + file)
		if err != nil {
			t.Error(err)
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "pw", CIRegistrarToken: "registrar-tok"}
}

// fakeDeployments serves each Deployment from a testdata file the test can
// swap.
type fakeDeployments struct {
	mu    sync.Mutex
	files map[string]string
	reads int
}

func (f *fakeDeployments) Deployment(ctx context.Context, namespace, name string) (Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	var d Deployment
	body, err := os.ReadFile("testdata/roll/" + f.files[namespace+"/"+name])
	if err != nil {
		return d, err
	}
	return d, json.Unmarshal(body, &d)
}

func (f *fakeDeployments) set(name, file string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files["factory/"+name] = file
}

type fakeRolls struct {
	mu   sync.Mutex
	rows map[string]Roll
}

func (s *fakeRolls) RollRecorded(ctx context.Context, sha string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.rows[sha]
	return ok, nil
}

func (s *fakeRolls) RecordRoll(ctx context.Context, r Roll) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[r.SHA]; ok {
		return false, nil
	}
	s.rows[r.SHA] = r
	return true, nil
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func newTestRollWatcher(t *testing.T, hub *rollHub, deps *fakeDeployments, rolls *fakeRolls) *RollWatcher {
	t.Helper()
	m, _ := testMetrics(t)
	return &RollWatcher{Forgejo: hub.forgejo(t), Owner: "sf", Repo: "ai-software-factory-code", Branch: "main", Namespace: "factory",
		Deployments: map[string]string{"factoryd": "k8s/factoryd.yaml", "factory-gateway": "k8s/deployment.yaml"},
		Kube:        deps, Store: rolls, Metrics: m, Poll: time.Millisecond, Timeout: 5 * time.Second}
}

// A roll waits until Flux applied the commit's images to both
// Deployments and both are available on them, then records once.
func TestRollWaitsForApplyAndLiveThenRecordsOnce(t *testing.T) {
	logs := captureLog(t)
	deps := &fakeDeployments{files: map[string]string{"factory/factoryd": "factoryd-before.json", "factory/factory-gateway": "gateway-live.json"}}
	rolls := &fakeRolls{rows: map[string]Roll{}}
	w := newTestRollWatcher(t, &rollHub{parentPrefix: "parent-", pushRun: true}, deps, rolls)
	m, reader := testMetrics(t)
	w.Metrics = m

	ctx := context.Background()
	if done, err := w.step(ctx); err != nil || done {
		t.Fatalf("old image: done %v err %v, want waiting", done, err)
	}
	deps.set("factoryd", "factoryd-rolling.json")
	if done, err := w.step(ctx); err != nil || done {
		t.Fatalf("rolling: done %v err %v, want waiting", done, err)
	}
	deps.set("factoryd", "factoryd-live.json")
	w.Run(ctx)

	// apply: the later kustomize-controller time, 10:07:11; live: the later
	// NewReplicaSetAvailable, 10:09:50; push 10:05:40
	want := Roll{SHA: rollSHA, PushedAt: at("2026-09-28T10:05:40Z"), AppliedAt: at("2026-09-28T10:07:11Z"), LiveAt: at("2026-09-28T10:09:50Z")}
	if got := rolls.rows[rollSHA]; !reflect.DeepEqual(got, want) {
		t.Fatalf("row %+v, want %+v", got, want)
	}
	if !strings.HasSuffix(logs.String(), " roll "+rollSHA+" live: push to apply 91 s, apply to ready 159 s\n") {
		t.Fatalf("log %q", logs.String())
	}
	for name, v := range map[string]float64{"factory.roll.duration": 250, "factory.roll.apply": 91, "factory.roll.ready": 159} {
		if got := histograms(t, reader, name); !reflect.DeepEqual(got, map[string]histPoint{"": {1, v}}) {
			t.Fatalf("%s %v, want one sample of %v", name, got, v)
		}
	}

	// a second boot during the same roll finds the row: no second emission
	again := newTestRollWatcher(t, &rollHub{parentPrefix: "parent-", pushRun: true}, deps, rolls)
	again.Metrics = m
	reads := deps.reads
	again.Run(ctx)
	if deps.reads != reads {
		t.Fatal("a recorded roll read the Deployments again")
	}
	if got := histograms(t, reader, "factory.roll.duration"); got[""].Count != 1 {
		t.Fatalf("roll duration emitted again: %v", got)
	}
}

// A newest k8s commit that changed no Deployment image is no roll, even
// with both Deployments live on its images and a push run.
func TestRollIgnoresACommitThatChangedNoImage(t *testing.T) {
	logs := captureLog(t)
	deps := &fakeDeployments{files: map[string]string{"factory/factoryd": "factoryd-live.json", "factory/factory-gateway": "gateway-live.json"}}
	rolls := &fakeRolls{rows: map[string]Roll{}}
	newTestRollWatcher(t, &rollHub{pushRun: true}, deps, rolls).Run(context.Background())
	if len(rolls.rows) != 0 || deps.reads != 0 {
		t.Fatalf("recorded %v after %d Deployment reads", rolls.rows, deps.reads)
	}
	if !strings.Contains(logs.String(), "changed no Deployment image") {
		t.Fatalf("log %q", logs.String())
	}
}

// Without a push run there is no push time: nothing is recorded or emitted.
func TestRollWithoutAPushRunRecordsNothing(t *testing.T) {
	deps := &fakeDeployments{files: map[string]string{"factory/factoryd": "factoryd-live.json", "factory/factory-gateway": "gateway-live.json"}}
	rolls := &fakeRolls{rows: map[string]Roll{}}
	w := newTestRollWatcher(t, &rollHub{parentPrefix: "parent-"}, deps, rolls)
	m, reader := testMetrics(t)
	w.Metrics = m
	w.Run(context.Background())
	if len(rolls.rows) != 0 {
		t.Fatalf("recorded %v", rolls.rows)
	}
	for _, name := range []string{"factory.roll.duration", "factory.roll.apply", "factory.roll.ready"} {
		if got := histograms(t, reader, name); len(got) != 0 {
			t.Fatalf("%s emitted %v", name, got)
		}
	}
}
