package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	digestOld       = "sha256:" + strings.Repeat("0", 64)
	digestNew       = "sha256:" + strings.Repeat("1", 64)
	digestRunner    = "sha256:" + strings.Repeat("2", 64)
	digestWorkerOld = "sha256:" + strings.Repeat("3", 64)
	digestWorkerNew = "sha256:" + strings.Repeat("4", 64)
)

const registryHost = "git.aisoftwarefactory.world/sf/"

// k8sManifests renders the code repository's two manifests pinning the
// given digests, shaped like k8s/factoryd.yaml and k8s/deployment.yaml.
func k8sManifests(factoryd, gateway, worker, runner string) map[string]string {
	return map[string]string{
		"k8s/factoryd.yaml": `apiVersion: v1
kind: ServiceAccount
metadata:
  name: factoryd
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: factoryd
spec:
  template:
    spec:
      containers:
      - name: factoryd
        image: ` + registryHost + `factory-gateway@` + factoryd + `
        command: [factoryd]
        env:
        - name: CI_RUNNER_IMAGE
          value: ` + registryHost + `ci-runner@` + runner + `
        - name: WORKER_IMAGE
          value: ` + registryHost + `opencode-worker@` + worker + `
`,
		"k8s/deployment.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: factory-gateway
spec:
  template:
    spec:
      containers:
      - name: gateway
        image: ` + registryHost + `factory-gateway@` + gateway + `
`,
	}
}

// rollHub serves the Forgejo reads of the roll watcher: the newest k8s
// commit, the manifests at each commit and each commit's push run.
type rollHub struct {
	mu        sync.Mutex
	head      [2]string                    // newest k8s commit and its parent
	manifests map[string]map[string]string // commit -> path -> content
	pushed    map[string]string            // commit -> its push run's created time
	pulls     []map[string]any             // the closed pull requests
	parents   map[string][]string          // commit -> its parents
	byNumber  map[string]map[string]any    // "/pulls/<n>" -> the pull request
}

func (h *rollHub) setHead(sha, parent string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.head = [2]string{sha, parent}
}

func (h *rollHub) forgejo(t *testing.T) *Forgejo {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		const base = "/api/v1/repos/sf/ai-software-factory-code"
		q := r.URL.Query()
		switch {
		case r.URL.Path == base+"/commits" && q.Get("sha") == "main" && q.Get("path") == "k8s" && q.Get("limit") == "1":
			fmt.Fprintf(w, `[{"sha":%q,"parents":[{"sha":%q}]}]`, h.head[0], h.head[1])
		case strings.HasPrefix(r.URL.Path, base+"/raw/"):
			content, ok := h.manifests[q.Get("ref")][strings.TrimPrefix(r.URL.Path, base+"/raw/")]
			if !ok {
				t.Errorf("unexpected read %s", r.URL)
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(content))
		case h.byNumber[strings.TrimPrefix(r.URL.Path, base)] != nil:
			json.NewEncoder(w).Encode(h.byNumber[strings.TrimPrefix(r.URL.Path, base)])
		case r.URL.Path == base+"/pulls" && q.Get("state") == "closed":
			json.NewEncoder(w).Encode(append([]map[string]any{}, h.pulls...))
		case strings.HasPrefix(r.URL.Path, base+"/git/commits/"):
			var parents []map[string]string
			for _, p := range h.parents[strings.TrimPrefix(r.URL.Path, base+"/git/commits/")] {
				parents = append(parents, map[string]string{"sha": p})
			}
			json.NewEncoder(w).Encode(map[string]any{"parents": parents})
		case r.URL.Path == base+"/actions/runs" && q.Get("event") == "push" && q.Get("head_sha") != "":
			if r.Header.Get("Authorization") != "token registrar-tok" {
				t.Errorf("actions/runs auth %q", r.Header.Get("Authorization"))
			}
			created, ok := h.pushed[q.Get("head_sha")]
			if !ok {
				w.Write([]byte(`{"workflow_runs":[],"total_count":0}`))
				return
			}
			fmt.Fprintf(w, `{"workflow_runs":[{"id":52,"commit_sha":%q,"event":"push","status":"running","prettyref":"main","created":%q}],"total_count":1}`,
				q.Get("head_sha"), created)
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "pw"}
}

// deploymentJSON is a Deployment Flux applied at applied, carrying image
// and env values; ready is when its new ReplicaSet became available, ""
// while it still rolls.
func deploymentJSON(applied, ready, image string, env ...string) string {
	var envs []map[string]string
	for _, v := range env {
		envs = append(envs, map[string]string{"name": "X", "value": v})
	}
	progressing := map[string]string{"type": "Progressing", "status": "True", "reason": "ReplicaSetUpdated", "lastUpdateTime": applied}
	if ready != "" {
		progressing["reason"], progressing["lastUpdateTime"] = "NewReplicaSetAvailable", ready
	}
	b, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"managedFields": []map[string]string{
			{"manager": "kustomize-controller", "operation": "Apply", "time": applied},
			{"manager": "kube-controller-manager", "operation": "Update", "time": applied, "subresource": "status"}}},
		"spec":   map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []map[string]any{{"name": "c", "image": image, "env": envs}}}}},
		"status": map[string]any{"conditions": []map[string]string{progressing}},
	})
	return string(b)
}

type fakeDeployments struct {
	mu    sync.Mutex
	json  map[string]string // name -> Deployment
	reads map[string]int
}

func (f *fakeDeployments) Deployment(ctx context.Context, namespace, name string) (Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if namespace != "factory" {
		return Deployment{}, fmt.Errorf("namespace %q", namespace)
	}
	f.reads[name]++
	var d Deployment
	return d, json.Unmarshal([]byte(f.json[name]), &d)
}

func (f *fakeDeployments) set(name, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.json[name] = body
}

type fakeRolls struct {
	mu   sync.Mutex
	rows map[string]Roll // sha/image
	prs  []int64         // the roll tasks' pull requests
}

func (s *fakeRolls) RollTaskPRs(ctx context.Context, repository string) ([]int64, error) {
	if repository != "sf/ai-software-factory-code" {
		return nil, fmt.Errorf("repository %q", repository)
	}
	return s.prs, nil
}

func (s *fakeRolls) RollRecorded(ctx context.Context, sha, image string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.rows[sha+"/"+image]
	return ok, nil
}

func (s *fakeRolls) RecordRoll(ctx context.Context, r Roll) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[r.SHA+"/"+r.Image]; ok {
		return false, nil
	}
	s.rows[r.SHA+"/"+r.Image] = r
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
	return &RollWatcher{Forgejo: hub.forgejo(t), Owner: "sf", Repo: "ai-software-factory-code", Token: "registrar-tok", Branch: "main", Namespace: "factory",
		Kube: deps, Store: rolls, Metrics: m, Poll: time.Millisecond, Timeout: time.Hour}
}

func steps(t *testing.T, w *RollWatcher, n int) {
	t.Helper()
	for range n {
		if err := w.step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	rollSHA    = "9f1e0c3a5b7d2e4f6a8c0b1d3e5f7a9c1b3d5e7f"
	rollParent = "8e2d4c6a0b1f3e5d7c9a1b3d5f7e9c1a3b5d7f9e"
	rollNext   = "7d3c5b7a9e1f3d5c7b9a1e3d5c7b9f1e3d5c7b9a"
)

// A commit that changed the control-plane image in both manifests rolls
// both Deployments: each gets its row, its times and its metrics as soon
// as it is live on the new image, the one still rolling is waited for.
func TestRollRecordsEachDeploymentWhenLive(t *testing.T) {
	logs := captureLog(t)
	hub := &rollHub{head: [2]string{rollSHA, rollParent},
		manifests: map[string]map[string]string{
			rollParent: k8sManifests(digestOld, digestOld, digestWorkerOld, digestRunner),
			rollSHA:    k8sManifests(digestNew, digestNew, digestWorkerOld, digestRunner),
		},
		pushed: map[string]string{rollSHA: "2026-09-28T10:05:40Z"}}
	gw := registryHost + "factory-gateway@" + digestNew
	deps := &fakeDeployments{reads: map[string]int{}, json: map[string]string{
		"factoryd":        deploymentJSON("2026-09-28T08:00:00Z", "2026-09-28T08:02:00Z", registryHost+"factory-gateway@"+digestOld),
		"factory-gateway": deploymentJSON("2026-09-28T10:07:11Z", "2026-09-28T10:08:02Z", gw),
	}}
	rolls := &fakeRolls{rows: map[string]Roll{}}
	w := newTestRollWatcher(t, hub, deps, rolls)
	m, reader := testMetrics(t)
	w.Metrics = m

	steps(t, w, 1)
	gateway := Roll{SHA: rollSHA, Image: "gateway", Deployment: "factory-gateway",
		PushedAt: at("2026-09-28T10:05:40Z"), AppliedAt: at("2026-09-28T10:07:11Z"), LiveAt: at("2026-09-28T10:08:02Z")}
	if !reflect.DeepEqual(rolls.rows, map[string]Roll{rollSHA + "/gateway": gateway}) {
		t.Fatalf("rows after the gateway went live %+v", rolls.rows)
	}
	deps.set("factoryd", deploymentJSON("2026-09-28T10:07:10Z", "", gw))
	steps(t, w, 1)
	if len(rolls.rows) != 1 {
		t.Fatalf("factoryd recorded while still rolling: %+v", rolls.rows)
	}
	deps.set("factoryd", deploymentJSON("2026-09-28T10:07:10Z", "2026-09-28T10:09:50Z", gw))
	steps(t, w, 2)

	factoryd := Roll{SHA: rollSHA, Image: "factoryd", Deployment: "factoryd",
		PushedAt: at("2026-09-28T10:05:40Z"), AppliedAt: at("2026-09-28T10:07:10Z"), LiveAt: at("2026-09-28T10:09:50Z")}
	if want := map[string]Roll{rollSHA + "/gateway": gateway, rollSHA + "/factoryd": factoryd}; !reflect.DeepEqual(rolls.rows, want) {
		t.Fatalf("rows %+v, want %+v", rolls.rows, want)
	}
	for _, line := range []string{
		" roll " + rollSHA + " factory-gateway live (gateway): push to apply 91 s, apply to ready 51 s\n",
		" roll " + rollSHA + " factoryd live (factoryd): push to apply 90 s, apply to ready 160 s\n",
	} {
		if !strings.Contains(logs.String(), line) {
			t.Fatalf("log %q lacks %q", logs.String(), line)
		}
	}
	for name, want := range map[string]map[string]histPoint{
		"factory.roll.duration": {"deployment=factory-gateway": {1, 142}, "deployment=factoryd": {1, 250}},
		"factory.roll.apply":    {"deployment=factory-gateway": {1, 91}, "deployment=factoryd": {1, 90}},
		"factory.roll.ready":    {"deployment=factory-gateway": {1, 51}, "deployment=factoryd": {1, 160}},
	} {
		if got := histograms(t, reader, name); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s %v, want %v", name, got, want)
		}
	}
	reads := deps.reads["factoryd"] + deps.reads["factory-gateway"]
	steps(t, w, 2)

	// a factoryd started by this roll finds the rows: no second emission
	again := newTestRollWatcher(t, hub, deps, rolls)
	again.Metrics = m
	steps(t, again, 2)
	if got := deps.reads["factoryd"] + deps.reads["factory-gateway"]; got != reads {
		t.Fatalf("a recorded roll read the Deployments again: %d reads, had %d", got, reads)
	}
	if got := histograms(t, reader, "factory.roll.duration"); got["deployment=factoryd"].Count != 1 {
		t.Fatalf("roll duration emitted again: %v", got)
	}
}

// The watcher runs on after boot: a later commit that changes only the
// worker image records the worker's row, measured on factoryd, which
// carries it as an env value (backlog 7: rolls without factoryd's own
// image had no row). A commit that changed no image records nothing.
func TestRollWatchesLaterCommitsForEveryImage(t *testing.T) {
	logs := captureLog(t)
	before := k8sManifests(digestOld, digestOld, digestWorkerOld, digestRunner)
	hub := &rollHub{head: [2]string{rollSHA, rollParent},
		manifests: map[string]map[string]string{
			rollParent: before, rollSHA: before,
			rollNext: k8sManifests(digestOld, digestOld, digestWorkerNew, digestRunner),
		},
		pushed: map[string]string{rollSHA: "2026-09-28T10:05:40Z", rollNext: "2026-09-28T11:00:00Z"}}
	deps := &fakeDeployments{reads: map[string]int{}, json: map[string]string{
		"factoryd": deploymentJSON("2026-09-28T11:00:05Z", "2026-09-28T11:01:00Z", registryHost+"factory-gateway@"+digestOld,
			registryHost+"ci-runner@"+digestRunner, registryHost+"opencode-worker@"+digestWorkerNew),
	}}
	rolls := &fakeRolls{rows: map[string]Roll{}}
	w := newTestRollWatcher(t, hub, deps, rolls)
	m, reader := testMetrics(t)
	w.Metrics = m

	steps(t, w, 2)
	if len(rolls.rows) != 0 || len(deps.reads) != 0 || !strings.Contains(logs.String(), "commit "+rollSHA+" changed no image") {
		t.Fatalf("a commit that changed no image: rows %v, reads %v, log %q", rolls.rows, deps.reads, logs.String())
	}
	hub.setHead(rollNext, rollSHA)
	steps(t, w, 2)
	want := map[string]Roll{rollNext + "/worker": {SHA: rollNext, Image: "worker", Deployment: "factoryd",
		PushedAt: at("2026-09-28T11:00:00Z"), AppliedAt: at("2026-09-28T11:00:05Z"), LiveAt: at("2026-09-28T11:01:00Z")}}
	if !reflect.DeepEqual(rolls.rows, want) {
		t.Fatalf("rows %+v, want %+v", rolls.rows, want)
	}
	if deps.reads["factory-gateway"] != 0 {
		t.Fatal("the gateway, which the worker's roll does not restart, was read")
	}
	if got := histograms(t, reader, "factory.roll.duration"); !reflect.DeepEqual(got, map[string]histPoint{"deployment=factoryd": {1, 60}}) {
		t.Fatalf("roll duration %v, want one factoryd sample of 60 s", got)
	}
}

// Without a push run there is no push time: nothing is recorded or emitted.
func TestRollWithoutAPushRunRecordsNothing(t *testing.T) {
	hub := &rollHub{head: [2]string{rollSHA, rollParent},
		manifests: map[string]map[string]string{
			rollParent: k8sManifests(digestOld, digestOld, digestWorkerOld, digestRunner),
			rollSHA:    k8sManifests(digestOld, digestNew, digestWorkerOld, digestRunner),
		}}
	deps := &fakeDeployments{reads: map[string]int{}, json: map[string]string{
		"factory-gateway": deploymentJSON("2026-09-28T10:07:11Z", "2026-09-28T10:08:02Z", registryHost+"factory-gateway@"+digestNew),
	}}
	rolls := &fakeRolls{rows: map[string]Roll{}}
	w := newTestRollWatcher(t, hub, deps, rolls)
	m, reader := testMetrics(t)
	w.Metrics = m
	steps(t, w, 3)
	if len(rolls.rows) != 0 {
		t.Fatalf("recorded %v", rolls.rows)
	}
	for _, name := range []string{"factory.roll.duration", "factory.roll.apply", "factory.roll.ready"} {
		if got := histograms(t, reader, name); len(got) != 0 {
			t.Fatalf("%s emitted %v", name, got)
		}
	}
}

// pinnedRef reads exactly one digest-pinned reference per image.
func TestPinnedRef(t *testing.T) {
	m := k8sManifests(digestNew, digestOld, digestWorkerOld, digestRunner)
	ref, digest, err := pinnedRef([]byte(m["k8s/factoryd.yaml"]), "opencode-worker")
	if err != nil || ref != registryHost+"opencode-worker@"+digestWorkerOld || digest != digestWorkerOld {
		t.Fatalf("worker ref %q digest %q err %v", ref, digest, err)
	}
	if _, _, err := pinnedRef([]byte(m["k8s/deployment.yaml"]), "ci-runner"); err == nil {
		t.Fatal("no reference is no error")
	}
	twice := m["k8s/deployment.yaml"] + m["k8s/deployment.yaml"]
	if _, _, err := pinnedRef([]byte(twice), "factory-gateway"); err == nil {
		t.Fatal("two references are no error")
	}
	if _, _, err := pinnedRef([]byte("image: "+registryHost+"factory-gateway:abc1234\n"), "factory-gateway"); err == nil {
		t.Fatal("a tag without a digest is no pinned reference")
	}
}

// factory-merge lands a roll as a merge commit, and the path query answers
// the roll branch's tip, the fold, which only the fork has: the watcher
// records the merge, compared with its first parent (main before the
// merge), for every target of both branch commits; only the merge has a
// push run.
func TestRollRecordsTheMergeOfARollPullRequest(t *testing.T) {
	const (
		mainBefore = "5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a"
		first      = "6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b"
		fold       = "7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c"
		merge      = "8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d"
	)
	rolled := k8sManifests(digestNew, digestNew, digestWorkerNew, digestRunner)
	hub := &rollHub{head: [2]string{fold, first},
		manifests: map[string]map[string]string{
			mainBefore: k8sManifests(digestOld, digestOld, digestWorkerOld, digestRunner),
			first:      k8sManifests(digestNew, digestNew, digestWorkerOld, digestRunner),
			fold:       rolled, merge: rolled,
		},
		pushed: map[string]string{merge: "2026-09-29T12:00:00Z"},
		pulls: []map[string]any{
			{"number": 20, "merged": true, "merge_commit_sha": "0f0f", "head": map[string]any{"sha": "1e1e"}},
			{"number": 21, "merged": true, "merge_commit_sha": merge, "head": map[string]any{"sha": fold}},
		},
		parents: map[string][]string{merge: {mainBefore, fold}}}
	gw := registryHost + "factory-gateway@" + digestNew
	deps := &fakeDeployments{reads: map[string]int{}, json: map[string]string{
		"factoryd": deploymentJSON("2026-09-29T12:01:00Z", "2026-09-29T12:02:00Z", gw,
			registryHost+"ci-runner@"+digestRunner, registryHost+"opencode-worker@"+digestWorkerNew),
		"factory-gateway": deploymentJSON("2026-09-29T12:01:01Z", "2026-09-29T12:01:30Z", gw),
	}}
	rolls := &fakeRolls{rows: map[string]Roll{}}
	w := newTestRollWatcher(t, hub, deps, rolls)
	steps(t, w, 3)
	row := func(image, dep, applied, live string) Roll {
		return Roll{SHA: merge, Image: image, Deployment: dep, PushedAt: at("2026-09-29T12:00:00Z"), AppliedAt: at(applied), LiveAt: at(live)}
	}
	want := map[string]Roll{
		merge + "/factoryd": row("factoryd", "factoryd", "2026-09-29T12:01:00Z", "2026-09-29T12:02:00Z"),
		merge + "/worker":   row("worker", "factoryd", "2026-09-29T12:01:00Z", "2026-09-29T12:02:00Z"),
		merge + "/gateway":  row("gateway", "factory-gateway", "2026-09-29T12:01:01Z", "2026-09-29T12:01:30Z"),
	}
	if !reflect.DeepEqual(rolls.rows, want) {
		t.Fatalf("rows %+v, want %+v", rolls.rows, want)
	}
}

// A roll's merge older than the closed-pull-request page the list reads
// is still found: the newest roll task's recorded pull request is read
// first, by its number.
func TestRollFindsTheRollsMergeByItsRecordedPullRequest(t *testing.T) {
	const (
		mainBefore = "5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a"
		tip        = "7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c"
		merge      = "8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d"
	)
	rolled := k8sManifests(digestOld, digestNew, digestWorkerOld, digestRunner)
	hub := &rollHub{head: [2]string{tip, mainBefore},
		manifests: map[string]map[string]string{mainBefore: k8sManifests(digestOld, digestOld, digestWorkerOld, digestRunner), tip: rolled, merge: rolled},
		pushed:    map[string]string{merge: "2026-09-29T12:00:00Z"},
		byNumber: map[string]map[string]any{"/pulls/21": {"number": 21, "state": "closed", "merged": true, "merge_commit_sha": merge,
			"head": map[string]any{"sha": tip}}},
		parents: map[string][]string{merge: {mainBefore, tip}}}
	deps := &fakeDeployments{reads: map[string]int{}, json: map[string]string{
		"factory-gateway": deploymentJSON("2026-09-29T12:01:01Z", "2026-09-29T12:01:30Z", registryHost+"factory-gateway@"+digestNew),
	}}
	rolls := &fakeRolls{rows: map[string]Roll{}, prs: []int64{7, 21}}
	w := newTestRollWatcher(t, hub, deps, rolls)
	steps(t, w, 2)
	want := map[string]Roll{merge + "/gateway": {SHA: merge, Image: "gateway", Deployment: "factory-gateway",
		PushedAt: at("2026-09-29T12:00:00Z"), AppliedAt: at("2026-09-29T12:01:01Z"), LiveAt: at("2026-09-29T12:01:30Z")}}
	if !reflect.DeepEqual(rolls.rows, want) {
		t.Fatalf("rows %+v, want %+v", rolls.rows, want)
	}
}
