package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"go.temporal.io/sdk/converter"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// fakeKube scripts the Kubernetes seam: pods are created with a phase and a
// log body; everything else is recorded for assertions.
type fakeKube struct {
	mu          sync.Mutex
	namespaces  map[string]map[string]string // name → labels
	pods        map[string]*fakePod
	podOrder    []*fakePod // creation order; entries survive deletion
	deletedPods []string
	deletedNs   []string
	secrets     map[string]map[string]string // ns/name → data
	secretTypes map[string]string            // ns/name → type
	sourceData  map[string]map[string]string // read-only source secrets
	cnps        map[string]map[string]any    // ns/name → spec
	createdNs   map[string]map[string]string // survives deletion
	objects     map[string]map[string]any    // Create path → object
	saErr       error                        // injected CreateServiceAccount failure
	getErr      map[string]error             // ns/name → injected GetSecretData failure
	onDeletePod func()                       // runs on every DeletePod
	ops         []string                     // namespace deletions and pod creations, in order
	onCreatePod func()                       // runs on every CreatePod
}

// hangLogs as a pod's log body keeps the stream open until the reader's
// context ends: a worker that runs on without output.
const hangLogs = "<hang>"

type ctxReader struct{ ctx context.Context }

func (r ctxReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, io.EOF
}

type fakePod struct {
	ns, name  string
	phase     string
	logs      string
	logsReady chan struct{} // closed when scriptPods has assigned the body
	spec      map[string]any
}

func newFakeKube() *fakeKube {
	return &fakeKube{
		namespaces:  map[string]map[string]string{},
		pods:        map[string]*fakePod{},
		secrets:     map[string]map[string]string{},
		secretTypes: map[string]string{},
		sourceData:  map[string]map[string]string{},
		cnps:        map[string]map[string]any{},
		createdNs:   map[string]map[string]string{},
		objects:     map[string]map[string]any{},
	}
}

func (k *fakeKube) CreateNamespace(ctx context.Context, name string, labels map[string]string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.namespaces[name]; ok {
		return ErrAlreadyExists
	}
	k.namespaces[name] = labels
	k.createdNs[name] = labels
	return nil
}

func (k *fakeKube) DeleteNamespace(ctx context.Context, name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.namespaces[name]; !ok {
		return ErrNotFound
	}
	delete(k.namespaces, name)
	k.deletedNs = append(k.deletedNs, name)
	k.ops = append(k.ops, "delete namespace "+name)
	for key := range k.pods {
		if strings.HasPrefix(key, name+"/") {
			delete(k.pods, key)
			k.deletedPods = append(k.deletedPods, key)
		}
	}
	return nil
}

func (k *fakeKube) NamespacesByLabel(ctx context.Context, key, value string) ([]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var names []string
	for name, labels := range k.namespaces {
		v, ok := labels[key]
		if value == "" && ok || v == value {
			names = append(names, name)
		}
	}
	return names, nil
}

func (k *fakeKube) CreateServiceAccount(ctx context.Context, namespace, name string) error {
	return k.saErr
}

func (k *fakeKube) CreateSecret(ctx context.Context, namespace, name, secretType string, stringData map[string]string) error {
	if secretType == "" {
		secretType = "Opaque"
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.secrets[namespace+"/"+name] = stringData
	k.secretTypes[namespace+"/"+name] = secretType
	return nil
}

func (k *fakeKube) GetSecretData(ctx context.Context, namespace, name string) (map[string]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err, ok := k.getErr[namespace+"/"+name]; ok {
		return nil, err
	}
	if data, ok := k.sourceData[namespace+"/"+name]; ok {
		return data, nil
	}
	if data, ok := k.secrets[namespace+"/"+name]; ok {
		return data, nil
	}
	return nil, ErrNotFound
}

func (k *fakeKube) CreateCiliumNetworkPolicy(ctx context.Context, namespace, name string, spec map[string]any) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.cnps[namespace+"/"+name] = spec
	return nil
}

func (k *fakeKube) Create(ctx context.Context, path string, obj map[string]any) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.objects[path] = obj
	return nil
}

func (k *fakeKube) CreatePod(ctx context.Context, namespace, name string, pod map[string]any) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.pods[namespace+"/"+name]; ok {
		return ErrAlreadyExists
	}
	p := &fakePod{ns: namespace, name: name, phase: "Running", logsReady: make(chan struct{}), spec: pod}
	k.pods[namespace+"/"+name] = p
	k.podOrder = append(k.podOrder, p)
	k.ops = append(k.ops, "create pod "+namespace+"/"+name)
	if k.onCreatePod != nil {
		k.onCreatePod()
	}
	return nil
}

func (k *fakeKube) DeletePod(ctx context.Context, namespace, name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.pods[namespace+"/"+name]; !ok {
		return ErrNotFound
	}
	delete(k.pods, namespace+"/"+name)
	k.deletedPods = append(k.deletedPods, namespace+"/"+name)
	if k.onDeletePod != nil {
		k.onDeletePod()
	}
	return nil
}

func (k *fakeKube) PodStatus(ctx context.Context, namespace, name string) (string, int32, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	p, ok := k.pods[namespace+"/"+name]
	if !ok {
		return "", -1, ErrNotFound
	}
	return p.phase, 0, nil
}

func (k *fakeKube) PodLog(ctx context.Context, namespace, name string) (io.ReadCloser, error) {
	k.mu.Lock()
	p := k.pods[namespace+"/"+name]
	k.mu.Unlock()
	if p == nil {
		return nil, ErrNotFound
	}
	select {
	case <-p.logsReady:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if p.logs == hangLogs {
		return io.NopCloser(ctxReader{ctx}), nil
	}
	if body, ok := strings.CutPrefix(p.logs, trickleLogs); ok {
		return io.NopCloser(iotest.HalfReader(strings.NewReader(body))), nil
	}
	return io.NopCloser(strings.NewReader(p.logs)), nil
}

// trickleLogs before a log body serves it in small reads, as a live pod log
// arrives: the reader's buffer fills and is reused across lines.
const trickleLogs = "<trickle>"

// scriptPods assigns log bodies to the pods created from index start on,
// in creation order, and unblocks their log streams.
func (k *fakeKube) scriptPods(start int, bodies ...string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for i, body := range bodies {
		idx := start + i
		if idx >= len(k.podOrder) {
			break
		}
		p := k.podOrder[idx]
		p.logs = body
		select {
		case <-p.logsReady:
		default:
			close(p.logsReady)
		}
	}
}

func (k *fakeKube) podAt(i int) *fakePod {
	return k.podOrder[i]
}

var _ Kube = (*fakeKube)(nil)

// forgejoHub mints, lists and revokes like the live admin API: minted
// tokens show up in the list until they are revoked.
type forgejoHub struct {
	mu          sync.Mutex
	tokens      []TokenRef // live list, emptied by revocation
	mintedNames []string   // everything ever minted
	revoked     []int64
	nextID      int64
	calls       []string
	branchHead  string // the task branch's head; empty = no branch yet
	comments    []string
}

func (h *forgejoHub) minted() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.mintedNames...)
}

func (h *forgejoHub) srv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/tokens"):
			var body struct{ Name string }
			json.NewDecoder(r.Body).Decode(&body)
			h.nextID++
			h.tokens = append(h.tokens, TokenRef{ID: h.nextID, Name: body.Name})
			h.mintedNames = append(h.mintedNames, body.Name)
			h.calls = append(h.calls, "mint "+body.Name)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d,"sha1":"tok-%s"}`, h.nextID, body.Name)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/tokens"):
			h.calls = append(h.calls, "list")
			list, _ := json.Marshal(h.tokens)
			w.Write(list)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments"):
			var body struct{ Body string }
			json.NewDecoder(r.Body).Decode(&body)
			h.comments = append(h.comments, body.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d}`, 500+len(h.comments))
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/branches/"):
			if h.branchHead == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprintf(w, `{"commit":{"id":%q}}`, h.branchHead)
		case r.Method == "DELETE":
			var id int64
			fmt.Sscanf(r.URL.Path, "/api/v1/users/factory-agent/tokens/%d", &id)
			kept := h.tokens[:0]
			for _, tok := range h.tokens {
				if tok.ID != id {
					kept = append(kept, tok)
				}
			}
			h.tokens = kept
			h.revoked = append(h.revoked, id)
			h.calls = append(h.calls, fmt.Sprintf("revoke %d", id))
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func envLine(ev harness.Event) string {
	b, _ := json.Marshal(harness.WorkerEnvelope{Type: "event", Event: &ev})
	return string(b)
}

func resultLine(success bool, summary string) string {
	b, _ := json.Marshal(harness.WorkerEnvelope{Type: "result", Result: &harness.Result{Success: success, Summary: summary}})
	return string(b)
}

func testRunnerConfig() RunnerConfig {
	return RunnerConfig{
		AgentNamespacePrefix: "factory-agents-",
		WorkerImage:          "reg/worker@sha256:abc",
		PullSecret:           "forgejo-registry",
		FactoryNamespace:     "factory",
		GatewayURL:           "http://factory-gateway.factory.svc.cluster.local:8081",
		GitBaseURL:           "http://forgejo-http.forgejo.svc.cluster.local:3000",
		Models:               ModelLadder{Start: "factory/coding-cheap", Stronger: "factory/coding-frontier"},
		AnswerTokens:         8192,
		HeadroomTokens:       16384,
		MaxSessions:          3,
		ChecksPoll:           time.Millisecond,
		// the test activity environment's deadline is 10 minutes out, so a
		// 10 minute cap puts the half-way point 3 minutes into each session
		Supervisor: SupervisorConfig{NoProgressAfter: time.Minute, WallClockCap: 10 * time.Minute, RepetitionEvents: 24},
	}
}

// newTestRunner wires a runner against scripted backends and a queued task.
func newTestRunner(t *testing.T, task Task, kube *fakeKube, objects ObjectStore) (*Runner, *forgejoHub, *fakeStore) {
	t.Helper()
	hub := &forgejoHub{nextID: 100}
	fjSrv := hub.srv(t)
	ll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/key/list" {
			io.WriteString(w, `{"keys":[],"total_pages":0}`)
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(ll.Close)
	store := newFakeStore()
	if err := store.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	kube.sourceData["factory/forgejo-registry"] = map[string]string{".dockerconfigjson": "{}"}
	completion := &Completion{
		Store:   store,
		Forgejo: &Forgejo{BaseURL: fjSrv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"},
		LiteLLM: &LiteLLMAdmin{BaseURL: ll.URL, MasterKey: "mk"},
	}
	// a test that expects a supervisor call replaces this with a fakeLiteLLM
	unexpected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected supervisor call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(unexpected.Close)
	runner, err := NewRunner(testRunnerConfig(), store, kube, completion.Forgejo, completion, objects, testSupervisorModel(unexpected.URL))
	if err != nil {
		t.Fatal(err)
	}
	runner.stopWait = 500 * time.Millisecond
	return runner, hub, store
}

// runSession executes the RunSession activity in a Temporal test activity
// environment rooted at ctx (nil: background), with heartbeat details from
// a previous attempt when dead is set, assigning per-pod log bodies as the
// pods appear (fakeKube.scriptPods needs the pods to exist first).
func runSession(t *testing.T, ctx context.Context, r *Runner, in SessionInput, dead *sessionProgress, bodies ...string) (SessionResult, error) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := suite.NewTestActivityEnvironment()
	r.RegisterActivities(env)
	if ctx != nil {
		env.SetWorkerOptions(worker.Options{BackgroundActivityContext: ctx})
	}
	if dead != nil {
		env.SetHeartbeatDetails(*dead)
	}
	var res SessionResult
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		var val converter.EncodedValue
		if val, err = env.ExecuteActivity(ActivityRunSession, in); err == nil {
			err = val.Get(&res)
		}
	}()
	kube := r.kube.(*fakeKube)
	assigned := 0
	for assigned < len(bodies) {
		kube.mu.Lock()
		created := len(kube.podOrder)
		kube.mu.Unlock()
		if created > assigned {
			kube.scriptPods(assigned, bodies[assigned:created]...)
			assigned = created
		}
		select {
		case <-done:
			return res, err
		case <-time.After(5 * time.Millisecond):
		}
	}
	<-done
	return res, err
}

func pushedLine(commit string) string {
	return envLine(harness.Event{Kind: harness.EventPushed, Time: time.Now(), Detail: map[string]any{"branch": "factory/t1", "commit": commit}})
}

func TestRunSessionHappyPathPushesInOwnSandbox(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "add a README line",
		Repository: "sf/ai-software-factory-code", SHA: "abc123", BudgetEUR: 2}
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, _ := newTestRunner(t, task, kube, objects)

	logs := envLine(harness.Event{Kind: harness.EventFileChange, Time: time.Now(), Detail: map[string]any{"path": "README.md"}}) + "\n" +
		pushedLine("c0ffee") + "\n" + resultLine(true, "pushed branch factory/t1 at c0ffee")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/coding-cheap"}, nil, logs)
	if err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	if res.Outcome != OutcomePushed || res.Commit != "c0ffee" || !strings.HasPrefix(res.SessionID, "s-") {
		t.Fatalf("result = %+v, want pushed c0ffee", res)
	}
	if minted := hub.minted(); len(minted) != 1 || minted[0] != "task-t1" {
		t.Fatalf("minted = %v, want one token named task-t1", minted)
	}
	if len(hub.revoked) != 1 || hub.revoked[0] != 101 {
		t.Fatalf("revoked = %v, want the session's token revoked at its end", hub.revoked)
	}
	if env := podEnv(kube.podAt(0)); env["REPO_SHA"] != "abc123" || env["MODEL"] != "factory/coding-cheap" {
		t.Fatalf("pod env = %v, want the base SHA and the cheap model", env)
	}
	ns := "factory-agents-t1"
	if _, ok := kube.namespaces[ns]; ok {
		t.Fatal("task namespace still exists after completion")
	}
	if len(kube.deletedNs) == 0 || kube.deletedNs[0] != ns {
		t.Fatalf("deleted namespaces = %v, want %s", kube.deletedNs, ns)
	}
	if l := kube.createdNs[ns]; l["factory.ai/task"] != "t1" || l["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Fatalf("task namespace labels = %v", l)
	}
	if kube.secrets[ns+"/task-git"]["GIT_TOKEN"] != "tok-task-t1" {
		t.Fatalf("git token secret = %v", kube.secrets[ns+"/task-git"])
	}
	if typ := kube.secretTypes[ns+"/forgejo-registry"]; typ != "kubernetes.io/dockerconfigjson" {
		t.Fatalf("copied pull secret type = %q, want kubernetes.io/dockerconfigjson (kubelet ignores Opaque)", typ)
	}
	if kube.secrets[ns+"/forgejo-registry"][".dockerconfigjson"] != "{}" {
		t.Fatal("pull secret was not copied into the task namespace")
	}
	cnp := kube.cnps[ns+"/worker-egress"]
	if cnp == nil {
		t.Fatal("worker egress CNP missing")
	}
	deny := fmt.Sprint(cnp["egressDeny"])
	if !strings.Contains(deny, "169.254.169.254/32") {
		t.Fatalf("egressDeny = %v, want the metadata address", deny)
	}
	cnpStr := fmt.Sprint(cnp)
	if strings.Contains(cnpStr, "matchPattern") {
		t.Fatal("DNS rule with a pattern: only the allowlisted names may resolve")
	}
	for _, name := range []string{"matchName:factory-gateway.factory.svc.cluster.local", "matchName:forgejo-http.forgejo.svc.cluster.local"} {
		if !strings.Contains(cnpStr, name) {
			t.Fatalf("DNS allowlist lacks %s: %s", name, cnpStr)
		}
	}
	if strings.Contains(cnpStr, "golang.org") || strings.Contains(cnpStr, "toFQDNs") {
		t.Fatal("no toolchain in the sandbox image, so no package-proxy egress")
	}
	gw := cnp["egress"].([]any)[0].(map[string]any)
	if !strings.Contains(fmt.Sprint(gw["toEndpoints"]), "app:factory-gateway") || fmt.Sprint(gw["toPorts"]) != "[map[ports:[map[port:8081 protocol:TCP]]]]" {
		t.Fatalf("gateway allow not restricted to port 8081: %v", gw)
	}
	rb := kube.objects["/apis/rbac.authorization.k8s.io/v1/namespaces/"+ns+"/rolebindings"]
	if fmt.Sprint(rb["roleRef"]) != "map[apiGroup:rbac.authorization.k8s.io kind:ClusterRole name:factoryd-task]" {
		t.Fatalf("task role binding = %v", rb)
	}
	quota := kube.objects["/api/v1/namespaces/"+ns+"/resourcequotas"]
	if hard := quota["spec"].(map[string]any)["hard"].(map[string]any); hard["pods"] != "2" || hard["limits.memory"] != "4Gi" {
		t.Fatalf("resource quota = %v", quota)
	}
	if kube.objects["/api/v1/namespaces/"+ns+"/limitranges"] == nil {
		t.Fatal("limit range missing")
	}
	if strings.Contains(cnpStr, "matchName") && strings.Contains(cnpStr, "git.aisoftwarefactory.world") {
		t.Fatal("git host allowed by FQDN: the in-cluster DNS answer is the gateway ClusterIP, which toFQDNs cannot authorize")
	}
	if !strings.Contains(cnpStr, `k8s:app.kubernetes.io/name:forgejo`) || !strings.Contains(cnpStr, `port:3000`) {
		t.Fatal("CNP without the in-cluster forgejo service allow")
	}

	pod := kube.podAt(0)
	spec := pod.spec["spec"].(map[string]any)
	if spec["runtimeClassName"] != "gvisor" || spec["automountServiceAccountToken"] != false {
		t.Fatalf("pod spec hardening: %v %v", spec["runtimeClassName"], spec["automountServiceAccountToken"])
	}
	if sp, _ := spec["securityContext"].(map[string]any)["seccompProfile"].(map[string]any); sp["type"] != "RuntimeDefault" {
		t.Fatalf("pod seccompProfile = %v", sp)
	}
	if spec["serviceAccountName"] != "task-t1" || spec["restartPolicy"] != "Never" {
		t.Fatalf("pod identity: %v %v", spec["serviceAccountName"], spec["restartPolicy"])
	}
	container := spec["containers"].([]any)[0].(map[string]any)
	sc := container["securityContext"].(map[string]any)
	if sc["readOnlyRootFilesystem"] != true || sc["runAsNonRoot"] != true || sc["allowPrivilegeEscalation"] != false {
		t.Fatalf("container securityContext = %v", sc)
	}
	if res := fmt.Sprint(container["resources"]); res != "map[limits:map[cpu:2 memory:2Gi] requests:map[cpu:250m memory:512Mi]]" {
		t.Fatalf("container resources = %s", res)
	}
	if dns := fmt.Sprint(spec["dnsConfig"]); dns != "map[options:[map[name:ndots value:1]]]" {
		t.Fatalf("dnsConfig = %s", dns)
	}
	volumes := spec["volumes"].([]any)
	for i, want := range []string{"4Gi", "1Gi"} {
		if got := volumes[i].(map[string]any)["emptyDir"].(map[string]any)["sizeLimit"]; got != want {
			t.Fatalf("volume %d sizeLimit = %v, want %s", i, got, want)
		}
	}
	projected := volumes[2].(map[string]any)["projected"].(map[string]any)
	sat := projected["sources"].([]any)[0].(map[string]any)["serviceAccountToken"].(map[string]any)
	if sat["audience"] != "factory-gateway" {
		t.Fatalf("projected token audience = %v", sat["audience"])
	}
	timeline, _ := objects.Get(context.Background(), "sessions/t1/timeline.jsonl")
	for _, step := range []string{`"step":"session_start"`, `"step":"session_end"`, `"step":"pushed"`, `"commit":"c0ffee"`} {
		if !strings.Contains(string(timeline), step) {
			t.Fatalf("timeline lacks %s:\n%s", step, timeline)
		}
	}
	if _, err := objects.Get(context.Background(), "sessions/t1/"+res.SessionID+".jsonl"); err != nil {
		t.Fatal(err)
	}
}

// podEnv flattens a pod's worker container env into name → value.
func podEnv(p *fakePod) map[string]string {
	env := map[string]string{}
	container := p.spec["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	for _, e := range container["env"].([]map[string]any) {
		if v, ok := e["value"].(string); ok {
			env[e["name"].(string)] = v
		}
	}
	return env
}

func TestRunSessionStartsFromTheGivenRef(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", SHA: "base"}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, task, kube, &fakeObjectStore{})

	_, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 2, Model: "factory/coding-frontier",
		StartRef: "c1", ParentSessionID: "s-prev", Findings: []string{"check \"pr / test\": failure"}}, nil, resultLine(false, "gave up"))
	if err != nil {
		t.Fatal(err)
	}
	env := podEnv(kube.podAt(0))
	if env["REPO_SHA"] != "c1" || env["MODEL"] != "factory/coding-frontier" || env["PARENT_SESSION_ID"] != "s-prev" ||
		!strings.Contains(env["FINDINGS"], "pr / test") {
		t.Fatalf("pod env = %v, want the branch head, the stronger model, the parent and the findings", env)
	}
	for _, s := range store.sessions {
		if s.ParentSessionID != "s-prev" {
			t.Fatalf("session %+v, want parent s-prev", s)
		}
	}
}

// A crashed factoryd's attempt is retried by Temporal on the next worker:
// the dead session's namespace (and pod) and token go first, then a new
// session carries the dead one's findings and id as parent.
func TestRunSessionRetryReplacesDeadSession(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "long", Repository: "sf/repo", SHA: "base"}
	kube := newFakeKube()
	runner, hub, store := newTestRunner(t, task, kube, &fakeObjectStore{})
	kube.namespaces["factory-agents-t1"] = map[string]string{"factory.ai/task": "t1"}
	kube.pods["factory-agents-t1/worker-s-dead"] = &fakePod{ns: "factory-agents-t1", name: "worker-s-dead", phase: "Running"}
	hub.tokens = []TokenRef{{ID: 50, Name: "task-t1"}}

	dead := &sessionProgress{SessionID: "s-dead", Running: true, Events: 7, Findings: []string{"changed a.go"}}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/coding-cheap", Findings: []string{"earlier note"}},
		dead, pushedLine("c2")+"\n"+resultLine(true, "pushed"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomePushed {
		t.Fatalf("result = %+v", res)
	}
	if len(kube.ops) < 2 || kube.ops[0] != "delete namespace factory-agents-t1" || !strings.HasPrefix(kube.ops[1], "create pod factory-agents-t1/worker-s-") {
		t.Fatalf("kube ops = %v, want the dead session's namespace deleted before the new pod", kube.ops)
	}
	if !slices.Contains(kube.deletedPods, "factory-agents-t1/worker-s-dead") {
		t.Fatalf("deleted pods = %v, want the dead session's pod gone", kube.deletedPods)
	}
	if len(hub.calls) < 3 || hub.calls[1] != "revoke 50" || hub.calls[2] != "mint task-t1" {
		t.Fatalf("forgejo calls = %v, want the leftover token revoked before the new mint", hub.calls)
	}
	env := podEnv(kube.podAt(0))
	if env["PARENT_SESSION_ID"] != "s-dead" {
		t.Fatalf("PARENT_SESSION_ID = %q, want s-dead", env["PARENT_SESSION_ID"])
	}
	var findings []string
	if err := json.Unmarshal([]byte(env["FINDINGS"]), &findings); err != nil {
		t.Fatal(err)
	}
	if len(findings) != 3 || findings[0] != "earlier note" || findings[1] != "changed a.go" || !strings.Contains(findings[2], "s-dead stopped after 7 events") {
		t.Fatalf("findings = %q, want the input, the dead session's findings, the interruption", findings)
	}
	if sess := store.sessions[res.SessionID]; sess.ParentSessionID != "s-dead" {
		t.Fatalf("session row = %+v, want parent s-dead", sess)
	}
	timeline, _ := runner.objects.Get(context.Background(), "sessions/t1/timeline.jsonl")
	if !strings.Contains(string(timeline), `"parent":"s-dead"`) || !strings.Contains(string(timeline), `"findings":["earlier note","changed a.go","session s-dead stopped after 7 events`) {
		t.Fatalf("timeline = %s, want the session start with its parent and findings", timeline)
	}
}

// A dead session may have pushed before its worker was lost; the retry
// builds on the branch head, not on the ref the attempt was scheduled with,
// or its non-force push would be refused.
func TestRunSessionStartsFromTheBranchHead(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", SHA: "base"}
	kube := newFakeKube()
	runner, hub, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	hub.branchHead = "c9"

	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "m", StartRef: "c1"}, nil, resultLine(false, "x")); err != nil {
		t.Fatal(err)
	}
	if env := podEnv(kube.podAt(0)); env["REPO_SHA"] != "c9" {
		t.Fatalf("REPO_SHA = %q, want the branch head c9", env["REPO_SHA"])
	}
}

// A retry after a setup failure (the pod never ran) has no dead session:
// no parent, no "stopped" finding.
func TestRunSessionRetryAfterSetupFailureStartsClean(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "x", Repository: "sf/repo", SHA: "base"}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})

	dead := &sessionProgress{SessionID: "s-never-ran"}
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "m", Findings: []string{"note"}}, dead, resultLine(false, "x")); err != nil {
		t.Fatal(err)
	}
	env := podEnv(kube.podAt(0))
	if env["PARENT_SESSION_ID"] != "" || env["FINDINGS"] != `["note"]` {
		t.Fatalf("pod env = %v, want no parent and only the input findings", env)
	}
}

func loopFailure(i int) string {
	return envLine(harness.Event{Kind: harness.EventCommand, Time: time.Now(), Detail: map[string]any{
		"command": "sh scripts/loopcheck.sh", "exit": 1, "output": fmt.Sprintf("loopcheck: FAIL at 10:00:%02d", i)}}) + "\n"
}

func supervisedTask(id string) Task {
	return Task{ID: id, Status: "running", Objective: "make loopcheck pass", Repository: "sf/repo", BudgetEUR: 0.3, SpentEUR: 0.05, IssueNumber: 7}
}

// The third identical failure trips same_failure; the supervisor (under
// the task's own key) says retry_stronger; the worker is stopped, pushes
// its work in progress, and the decision lands in the session record, the
// timeline and on the issue.
func TestRunSessionTripAsksTheSupervisorAndStopsGracefully(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, store := newTestRunner(t, supervisedTask("t2"), kube, objects)
	f := &fakeLiteLLM{content: `{"action":"retry_stronger","guidance":"the check needs a file the agent never created"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)

	logs := loopFailure(1) + loopFailure(2) + loopFailure(3) +
		// what the worker prints after its SIGTERM
		envLine(harness.Event{Kind: harness.EventFinished, Time: time.Now(), Detail: map[string]any{"success": false}}) + "\n" +
		pushedLine("c-wip") + "\n" + resultLine(false, "stopped: work in progress pushed at c-wip") + "\n"
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/coding-cheap"}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeRetryStronger || res.Commit != "c-wip" {
		t.Fatalf("result = %+v, want retry_stronger with the work in progress", res)
	}
	joined := strings.Join(res.Findings, "\n")
	if !strings.Contains(joined, "supervisor guidance: the check needs a file") || !strings.Contains(joined, "c-wip") {
		t.Fatalf("findings = %v, want the guidance and the pushed work", res.Findings)
	}
	if len(f.mints) != 1 || f.mints[0]["max_budget"] != 0.25 {
		t.Fatalf("mints = %v, want one key capped at the remaining 0.25", f.mints)
	}
	if len(kube.deletedPods) == 0 {
		t.Fatal("the worker was not stopped")
	}
	// the record: three commands, the decision, then what the stopped worker sent
	record := string(objGet(objects, "sessions/t2/"+res.SessionID+".jsonl"))
	var kinds []string
	for _, l := range strings.Split(strings.TrimSpace(record), "\n") {
		var ev harness.Event
		json.Unmarshal([]byte(l), &ev)
		kinds = append(kinds, string(ev.Kind))
		if ev.Kind == harness.EventSupervisor && (ev.Detail["detector"] != "same_failure" || ev.Detail["decision"] != "retry_stronger" ||
			!strings.Contains(fmt.Sprint(ev.Detail["digest"]), "3 commands (3 failed)")) {
			t.Fatalf("supervisor event = %v", ev.Detail)
		}
	}
	if strings.Join(kinds, ",") != "command,command,command,supervisor,finished,pushed" {
		t.Fatalf("record kinds = %v", kinds)
	}
	timeline := string(objGet(objects, "sessions/t2/timeline.jsonl"))
	if !strings.Contains(timeline, `"step":"supervisor"`) || !strings.Contains(timeline, `"decision":"retry_stronger"`) {
		t.Fatalf("timeline = %s", timeline)
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "**retry_stronger**") || !strings.Contains(hub.comments[0], "same_failure") {
		t.Fatalf("comments = %v", hub.comments)
	}
	if snap := store.sessions[res.SessionID]; len(snap.Findings) == 0 {
		t.Fatal("tripped session left no findings snapshot")
	}
}

func TestRunSessionContinueLetsTheSessionRunOn(t *testing.T) {
	kube := newFakeKube()
	runner, hub, _ := newTestRunner(t, supervisedTask("t2"), kube, &fakeObjectStore{})
	f := &fakeLiteLLM{content: `{"action":"continue","reason":"each run fails later"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)

	logs := loopFailure(1) + loopFailure(2) + loopFailure(3) + loopFailure(4) + pushedLine("c1") + "\n" + resultLine(true, "pushed")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomePushed || res.Commit != "c1" || len(f.chats) != 1 {
		t.Fatalf("result = %+v after %d supervisor calls, want pushed after one", res, len(f.chats))
	}
	if len(kube.deletedPods) != 1 {
		t.Fatalf("deleted pods = %v, want only the cleanup delete", kube.deletedPods)
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "**continue**") {
		t.Fatalf("comments = %v", hub.comments)
	}
}

// Three continues use up the session's calls; the fourth trip escalates
// without a call, and the escalation is left to the workflow's comment.
func TestRunSessionFourthTripEscalatesWithoutACall(t *testing.T) {
	runner, hub, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
	f := &fakeLiteLLM{content: `{"action":"continue"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)

	var logs string
	for i := 0; i < 12; i++ {
		logs += loopFailure(i)
	}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs+resultLine(false, "stopped"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.chats) != 3 || res.Outcome != OutcomeEscalate || !strings.Contains(res.Reason, "after 3 supervisor calls") {
		t.Fatalf("calls = %d, result = %+v; want 3 calls, then escalate", len(f.chats), res)
	}
	if !strings.Contains(res.Reason, "Progress: 12 commands (12 failed)") {
		t.Fatalf("reason = %q, want the digest summary", res.Reason)
	}
	if len(hub.comments) != 3 {
		t.Fatalf("comments = %d, want one per continue and none for the escalation", len(hub.comments))
	}
}

// A supervisor still thinking when the wall-clock cap arrives is cut off:
// the cap ends the session.
func TestRunSessionCapCutsOffASlowSupervisor(t *testing.T) {
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
	runner.cfg.Supervisor.WallClockCap = time.Second
	f := &fakeLiteLLM{content: `{"action":"continue"}`, delay: time.Minute}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)

	began := time.Now()
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, loopFailure(1)+loopFailure(2)+loopFailure(3))
	if err != nil || res.Outcome != OutcomeWallClock {
		t.Fatalf("result = (%+v, %v), want wall_clock", res, err)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Fatalf("took %s: the supervisor call outlived the session", took)
	}
}

// What the session did reaches its result: the findings of a session that
// ended without a push, and the half-way flag the workflow carries on.
func TestRunSessionResultCarriesFindingsAndHalfWay(t *testing.T) {
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
	logs := envLine(harness.Event{Kind: harness.EventCommand, Time: time.Now(), Detail: map[string]any{"command": "go test ./...", "exit": 1}}) + "\n" + resultLine(false, "gave up")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m", HalfWayDone: true}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Findings, "ran `go test ./...` (exit 1)") || !res.HalfWayDone {
		t.Fatalf("result = %+v, want the command in the findings and the half-way flag kept", res)
	}
}

// Every line of a log that arrives in pieces is recorded intact.
func TestRunSessionRecordsEveryLineOfATricklingLog(t *testing.T) {
	objects := &fakeObjectStore{}
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), objects)
	logs := trickleLogs
	for i := 0; i < 400; i++ {
		logs += envLine(harness.Event{Kind: harness.EventModelCall, Time: time.Now(), Detail: map[string]any{"n": i, "text": strings.Repeat("x", 300)}}) + "\n"
	}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs+resultLine(false, "x"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(objGet(objects, "sessions/t2/"+res.SessionID+".jsonl"))), "\n")
	if len(lines) != 400 {
		t.Fatalf("recorded %d events, want 400", len(lines))
	}
	for i, l := range lines {
		var ev harness.Event
		if err := json.Unmarshal([]byte(l), &ev); err != nil || ev.Detail["n"] != float64(i) {
			t.Fatalf("event %d = %.80s (%v)", i, l, err)
		}
	}
}

func TestRunSessionRedirectCarriesGuidance(t *testing.T) {
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
	runner.cfg.Supervisor.RepetitionEvents = 2
	f := &fakeLiteLLM{content: `{"action":"redirect","guidance":"write the file instead of reading"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)

	logs := envLine(harness.Event{Kind: harness.EventToolCall, Time: time.Now(), Detail: map[string]any{"tool": "read", "args": map[string]any{"filePath": "a"}}}) + "\n" +
		envLine(harness.Event{Kind: harness.EventToolCall, Time: time.Now(), Detail: map[string]any{"tool": "read", "args": map[string]any{"filePath": "b"}}}) + "\n"
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeRedirect || !slices.Contains(res.Findings, "supervisor guidance: write the file instead of reading") {
		t.Fatalf("result = %+v", res)
	}
}

// Only factoryd writes supervisor events: one from the worker is dropped.
func TestRunSessionDropsWorkerSupervisorEvents(t *testing.T) {
	objects := &fakeObjectStore{}
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), objects)
	forged := envLine(harness.Event{Kind: harness.EventSupervisor, Time: time.Now(), Detail: map[string]any{"decision": "continue"}})
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, forged+"\n"+resultLine(false, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if rec := objGet(objects, "sessions/t2/"+res.SessionID+".jsonl"); len(rec) != 0 {
		t.Fatalf("record = %s, want the forged event dropped", rec)
	}
}

func TestRunSessionAdapterFailureIsNoPush(t *testing.T) {
	task := Task{ID: "t3", Status: "running", Objective: "break", Repository: "sf/repo"}
	kube := newFakeKube()
	runner, hub, _ := newTestRunner(t, task, kube, &fakeObjectStore{})

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t3", Attempt: 1, Model: "m"}, nil, resultLine(false, "model refused"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeNoPush || res.Summary != "model refused" || !strings.Contains(strings.Join(res.Findings, "\n"), "model refused") {
		t.Fatalf("result = %+v", res)
	}
	if len(hub.revoked) != 1 {
		t.Fatalf("revoked = %v: a failed session must still revoke", hub.revoked)
	}
}

func TestRunSessionSuccessWithoutPushIsNoPush(t *testing.T) {
	task := Task{ID: "t3", Status: "running", Objective: "noop", Repository: "sf/repo"}
	runner, _, _ := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t3", Attempt: 1, Model: "m"}, nil, resultLine(true, "no changes to push"))
	if err != nil || res.Outcome != OutcomeNoPush || res.Summary != "no changes to push" {
		t.Fatalf("result = (%+v, %v), want no_push", res, err)
	}
}

func TestRunSessionPodDeathIsNoPush(t *testing.T) {
	task := Task{ID: "t4", Status: "running", Objective: "flaky", Repository: "sf/repo"}
	runner, _, _ := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t4", Attempt: 1, Model: "m"}, nil,
		envLine(harness.Event{Kind: harness.EventModelCall, Time: time.Now()})+"\n")
	if err != nil || res.Outcome != OutcomeNoPush || res.Summary != "pod exited without a result" {
		t.Fatalf("result = (%+v, %v)", res, err)
	}
}

func TestRunSessionWallClockTrip(t *testing.T) {
	task := Task{ID: "t5", Status: "running", Objective: "slow", Repository: "sf/repo"}
	kube := newFakeKube()
	runner, hub, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	runner.cfg.Supervisor = SupervisorConfig{NoProgressAfter: 3 * time.Second, WallClockCap: 300 * time.Millisecond, RepetitionEvents: 24}

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t5", Attempt: 1, Model: "m"}, nil, hangLogs)
	if err != nil || res.Outcome != OutcomeWallClock {
		t.Fatalf("result = (%+v, %v), want wall_clock", res, err)
	}
	if len(hub.revoked) != 1 {
		t.Fatalf("revoked = %v", hub.revoked)
	}
}

// A worker shutdown cancels the attempt: its pod, namespace and credentials
// go, and the error hands the attempt back to Temporal for a retry.
func TestRunSessionShutdownCleansUpAndFails(t *testing.T) {
	task := Task{ID: "t6", Status: "running", Objective: "long", Repository: "sf/repo"}
	kube := newFakeKube()
	runner, hub, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	ctx, cancel := context.WithCancel(context.Background())
	kube.onCreatePod = func() { go func() { time.Sleep(20 * time.Millisecond); cancel() }() }

	_, err := runSession(t, ctx, runner, SessionInput{TaskID: "t6", Attempt: 1, Model: "m"}, nil, hangLogs)
	if err == nil {
		t.Fatal("RunSession succeeded through a shutdown")
	}
	if len(hub.revoked) != 1 {
		t.Fatalf("revoked = %v: the session's token must not survive", hub.revoked)
	}
	if _, ok := kube.namespaces["factory-agents-t6"]; ok {
		t.Fatal("namespace survived the shutdown")
	}
}

func TestRunSessionSetupFailureDeletesNamespace(t *testing.T) {
	task := Task{ID: "t8", Status: "running", Objective: "x", Repository: "sf/repo"}
	kube := newFakeKube()
	kube.saErr = fmt.Errorf("apiserver says no")
	runner, hub, _ := newTestRunner(t, task, kube, &fakeObjectStore{})

	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t8", Attempt: 1, Model: "m"}, nil); err == nil {
		t.Fatal("RunSession succeeded without a sandbox")
	}
	if _, ok := kube.createdNs["factory-agents-t8"]; !ok {
		t.Fatal("setup never created the namespace")
	}
	if _, ok := kube.namespaces["factory-agents-t8"]; ok {
		t.Fatal("half-built namespace survived a setup failure")
	}
	if len(hub.revoked) != 1 {
		t.Fatalf("revoked = %v", hub.revoked)
	}
}

func TestBootSweepKeepsOnlyLiveTaskNamespaces(t *testing.T) {
	kube := newFakeKube()
	for _, id := range []string{"trunning", "tqueued", "treview", "tdone", "ghost", "ci-87"} {
		kube.namespaces["factory-agents-"+id] = map[string]string{"factory.ai/task": id}
	}
	runner, _, store := newTestRunner(t, Task{ID: "trunning", Status: "running", Repository: "sf/repo"}, kube, &fakeObjectStore{})
	store.CreateTask(context.Background(), Task{ID: "tqueued", Status: "queued", Repository: "sf/repo"})
	store.CreateTask(context.Background(), Task{ID: "treview", Status: "in-review", Repository: "sf/repo"})
	store.CreateTask(context.Background(), Task{ID: "tdone", Status: "done", Repository: "sf/repo"})

	runner.BootSweep(context.Background())

	// factory-agents-ci-* belongs to the CI spawner, which runs PR checks
	// in it across factoryd restarts
	for id, kept := range map[string]bool{"trunning": true, "tqueued": true, "treview": false, "tdone": false, "ghost": false, "ci-87": true} {
		if _, ok := kube.namespaces["factory-agents-"+id]; ok != kept {
			t.Fatalf("namespace of %s kept = %v, want %v", id, ok, kept)
		}
	}
	if got, _, _ := store.Task(context.Background(), "trunning"); got.Status != "running" {
		t.Fatalf("running task = %q: the sweep must leave it to its workflow", got.Status)
	}
}

func TestFreshNamespaceReplacesLeftover(t *testing.T) {
	kube := newFakeKube()
	kube.namespaces["factory-agents-t10"] = map[string]string{"stale": "yes"}
	runner, _, _ := newTestRunner(t, Task{ID: "t10", Status: "queued"}, kube, &fakeObjectStore{})

	if err := runner.freshNamespace(context.Background(), "factory-agents-t10", map[string]string{"factory.ai/task": "t10"}); err != nil {
		t.Fatal(err)
	}
	if l := kube.namespaces["factory-agents-t10"]; l["stale"] != "" || l["factory.ai/task"] != "t10" {
		t.Fatalf("namespace labels = %v, want the fresh namespace", l)
	}
}

func objGet(s *fakeObjectStore, key string) []byte {
	b, _ := s.Get(context.Background(), key)
	return b
}
