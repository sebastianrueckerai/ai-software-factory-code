package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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
	onDeletePod func()                       // runs on every DeletePod
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
	return io.NopCloser(strings.NewReader(p.logs)), nil
}

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
		Model:                "factory/coding-frontier",
		RetryModel:           "factory/review-frontier",
		AnswerTokens:         8192,
		HeadroomTokens:       16384,
		MaxSessions:          3,
		PollInterval:         time.Millisecond,
		Supervisor:           SupervisorConfig{NoProgressAfter: time.Minute, WallClockCap: time.Hour, RepetitionEvents: 24},
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
	runner, err := NewRunner(testRunnerConfig(), store, kube, completion.Forgejo, completion, objects)
	if err != nil {
		t.Fatal(err)
	}
	return runner, hub, store
}

func TestRunnerHappyPathRunsSandboxAndCompletes(t *testing.T) {
	task := Task{ID: "t1", Status: "queued", Objective: "add a README line",
		Repository: "sf/ai-software-factory-code", SHA: "abc123", BudgetEUR: 2}
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, store := newTestRunner(t, task, kube, objects)

	logs := envLine(harness.Event{Kind: harness.EventFileChange, Time: time.Now(), Detail: map[string]any{"path": "README.md"}}) + "\n" +
		resultLine(true, "done")
	runWithPodLogs(t, runner, task, []string{logs})

	got, _, _ := store.Task(context.Background(), "t1")
	if got.Status != "done" {
		t.Fatalf("status = %q, want done", got.Status)
	}
	if minted := hub.minted(); len(minted) != 1 || minted[0] != "task-t1" {
		t.Fatalf("minted = %v, want one token named task-t1", minted)
	}
	if len(hub.revoked) != 1 || hub.revoked[0] != 101 {
		t.Fatalf("revoked = %v, want the minted token", hub.revoked)
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
	if len(objects.puts) == 0 || !strings.HasPrefix(objects.puts[0].key, "sessions/t1/") {
		t.Fatalf("session record puts = %v", objects.puts)
	}
}

// runWithPodLogs drives execute with per-attempt log bodies assigned as pods
// get created (fakeKube.scriptPods needs the pods to exist first).
func runWithPodLogs(t *testing.T, r *Runner, task Task, bodies []string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.execute(context.Background(), task)
	}()
	assigned := 0
	for assigned < len(bodies) {
		kube := r.kube.(*fakeKube)
		kube.mu.Lock()
		created := len(kube.podOrder)
		kube.mu.Unlock()
		if created > assigned {
			kube.scriptPods(assigned, bodies[assigned:created]...)
			assigned = created
		}
		select {
		case <-done:
			// execute may finish before consuming every scripted body
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
	<-done
}

func TestRunnerResumesAfterSupervisionTrip(t *testing.T) {
	task := Task{ID: "t2", Status: "queued", Objective: "do it", Repository: "sf/repo"}
	kube := newFakeKube()
	cfg := testRunnerConfig()
	cfg.Supervisor.RepetitionEvents = 2
	kube.sourceData["factory/forgejo-registry"] = map[string]string{".dockerconfigjson": "{}"}
	objects := &fakeObjectStore{}
	hub := &forgejoHub{nextID: 200}
	fjSrv := hub.srv(t)
	ll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"keys":[],"total_pages":0}`)
	}))
	t.Cleanup(ll.Close)
	store := newFakeStore()
	store.CreateTask(context.Background(), task)
	completion := &Completion{Store: store, Forgejo: &Forgejo{BaseURL: fjSrv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"},
		LiteLLM: &LiteLLMAdmin{BaseURL: ll.URL}}
	runner, err := NewRunner(cfg, store, kube, completion.Forgejo, completion, objects)
	if err != nil {
		t.Fatal(err)
	}

	// First session: three commands, no file change → repetition trip.
	var stalling string
	for i := 0; i < 3; i++ {
		stalling += envLine(harness.Event{Kind: harness.EventCommand, Time: time.Now(), Detail: map[string]any{"command": fmt.Sprintf("cmd %d", i), "exit": 0}}) + "\n"
	}
	runWithPodLogs(t, runner, task, []string{stalling, resultLine(true, "recovered")})

	got, _, _ := store.Task(context.Background(), "t2")
	if got.Status != "done" {
		t.Fatalf("status = %q, want done after resume", got.Status)
	}
	if len(kube.podOrder) != 2 {
		t.Fatalf("pods created = %d, want 2 (stalled + resumed)", len(kube.podOrder))
	}
	sessions := 0
	for _, s := range store.sessions {
		if s.TaskID == "t2" {
			sessions++
		}
	}
	if sessions != 2 {
		t.Fatalf("sessions = %d, want 2", sessions)
	}
	var resumed Session
	for _, s := range store.sessions {
		if s.ParentSessionID != "" {
			resumed = s
		}
	}
	if resumed.ID == "" {
		t.Fatal("no session chained to a parent")
	}
	if _, ok := store.sessions[resumed.ParentSessionID]; !ok {
		t.Fatalf("parent %s not recorded", resumed.ParentSessionID)
	}
	// The snapshot carries the trip's findings into the resumed session row.
	parent := store.sessions[resumed.ParentSessionID]
	if len(parent.Findings) == 0 {
		t.Fatal("tripped session left no findings snapshot")
	}

	second := runner.workerPodSpec(task, "s2", "s1", nil, cfg.RetryModel)
	secondEnv := second["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"].([]map[string]any)
	var model, parentEnv, repository string
	for _, e := range secondEnv {
		if e["name"] == "REPOSITORY" {
			repository = e["value"].(string)
		}
		if e["name"] == "MODEL" {
			model = e["value"].(string)
		}
		if e["name"] == "PARENT_SESSION_ID" {
			parentEnv = e["value"].(string)
		}
	}
	if model != cfg.RetryModel || parentEnv != "s1" || repository != task.Repository {
		t.Fatalf("resumed pod env: model=%q parent=%q repository=%q", model, parentEnv, repository)
	}
}

func TestRunnerFailsAndRevokesWhenAdapterFails(t *testing.T) {
	task := Task{ID: "t3", Status: "queued", Objective: "break", Repository: "sf/repo"}
	kube := newFakeKube()
	runner, hub, store := newTestRunner(t, task, kube, &fakeObjectStore{})

	runWithPodLogs(t, runner, task, []string{resultLine(false, "model refused")})

	got, _, _ := store.Task(context.Background(), "t3")
	if got.Status != "failed" {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if len(hub.revoked) != 1 {
		t.Fatalf("revoked = %v: a failed run must still revoke", hub.revoked)
	}
}

func TestRunnerRetriesWhenPodDiesWithoutResult(t *testing.T) {
	task := Task{ID: "t4", Status: "queued", Objective: "flaky", Repository: "sf/repo"}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, task, kube, &fakeObjectStore{})

	runWithPodLogs(t, runner, task, []string{
		envLine(harness.Event{Kind: harness.EventModelCall, Time: time.Now()}) + "\n",
		resultLine(true, "second attempt worked"),
	})

	got, _, _ := store.Task(context.Background(), "t4")
	if got.Status != "done" {
		t.Fatalf("status = %q, want done after retrying the dead pod", got.Status)
	}
	if len(kube.podOrder) != 2 {
		t.Fatalf("pods = %d, want 2", len(kube.podOrder))
	}
}

func TestRunnerBootSweep(t *testing.T) {
	kube := newFakeKube()
	kube.namespaces["factory-agents-t9"] = map[string]string{"factory.ai/task": "t9"}
	kube.namespaces["factory-agents-tqueued"] = map[string]string{"factory.ai/task": "tqueued"}
	kube.namespaces["factory-agents-ghost"] = map[string]string{"factory.ai/task": "ghost"}
	objects := &fakeObjectStore{}
	hub := &forgejoHub{nextID: 300}
	fjSrv := hub.srv(t)
	ll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"keys":[],"total_pages":0}`)
	}))
	t.Cleanup(ll.Close)
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t9", Status: "running", Repository: "sf/repo", Objective: "x"})
	store.CreateTask(context.Background(), Task{ID: "tclosing", Status: "closing", Repository: "sf/repo", Objective: "z"})
	store.CreateTask(context.Background(), Task{ID: "tqueued", Status: "queued", Repository: "sf/repo", Objective: "y"})
	completion := &Completion{Store: store, Forgejo: &Forgejo{BaseURL: fjSrv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"},
		LiteLLM: &LiteLLMAdmin{BaseURL: ll.URL}}
	runner, err := NewRunner(testRunnerConfig(), store, kube, completion.Forgejo, completion, objects)
	if err != nil {
		t.Fatal(err)
	}

	runner.BootSweep(context.Background())

	t9, _, _ := store.Task(context.Background(), "t9")
	if t9.Status != "failed" {
		t.Fatalf("interrupted task status = %q, want failed", t9.Status)
	}
	tc, _, _ := store.Task(context.Background(), "tclosing")
	if tc.Status != "failed" {
		t.Fatalf("stuck-closing task status = %q, want failed (sweep finishes the cleanup)", tc.Status)
	}
	if _, ok := kube.namespaces["factory-agents-t9"]; ok {
		t.Fatal("interrupted task's namespace survived the sweep")
	}
	if _, ok := kube.namespaces["factory-agents-ghost"]; ok {
		t.Fatal("orphan namespace survived the sweep")
	}
	if _, ok := kube.namespaces["factory-agents-tqueued"]; !ok {
		t.Fatal("queued task's namespace must NOT be swept")
	}
}

func TestRunnerWallClockCapIsPerTask(t *testing.T) {
	task := Task{ID: "t5", Status: "queued", Objective: "slow", Repository: "sf/repo"}
	kube := newFakeKube()
	runner, hub, store := newTestRunner(t, task, kube, &fakeObjectStore{})
	runner.cfg.Supervisor = SupervisorConfig{NoProgressAfter: 3 * time.Second, WallClockCap: 300 * time.Millisecond, RepetitionEvents: 24}

	runWithPodLogs(t, runner, task, []string{hangLogs, hangLogs, hangLogs})

	got, _, _ := store.Task(context.Background(), "t5")
	if got.Status != "failed" {
		t.Fatalf("status = %q, want failed at the task cap", got.Status)
	}
	if len(kube.podOrder) != 1 {
		t.Fatalf("pods = %d, want 1: a wall-clock trip ends the task, it does not resume", len(kube.podOrder))
	}
	if len(hub.revoked) != 1 {
		t.Fatalf("revoked = %v", hub.revoked)
	}
}

func TestRunnerShutdownRequeuesInFlightTask(t *testing.T) {
	task := Task{ID: "t6", Status: "queued", Objective: "long", Repository: "sf/repo"}
	kube := newFakeKube()
	runner, hub, store := newTestRunner(t, task, kube, &fakeObjectStore{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.execute(ctx, task)
	}()
	for {
		kube.mu.Lock()
		n := len(kube.podOrder)
		kube.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	kube.scriptPods(0, hangLogs)
	cancel()
	<-done

	got, _, _ := store.Task(context.Background(), "t6")
	if got.Status != "queued" {
		t.Fatalf("status = %q, want queued (handed back on shutdown)", got.Status)
	}
	if len(kube.podOrder) != 1 {
		t.Fatalf("pods = %d, want 1: no new session after shutdown", len(kube.podOrder))
	}
	if len(hub.revoked) != 1 {
		t.Fatalf("revoked = %v: a requeued task's token must not survive", hub.revoked)
	}
	if _, ok := kube.namespaces["factory-agents-t6"]; ok {
		t.Fatal("namespace survived the shutdown")
	}
}

// A shutdown that lands after the worker delivered its verdict must not
// hand the finished task back to the queue.
func TestRunnerShutdownAfterVerdictCompletes(t *testing.T) {
	task := Task{ID: "t8", Status: "queued", Objective: "quick", Repository: "sf/repo"}
	kube := newFakeKube()
	runner, hub, store := newTestRunner(t, task, kube, &fakeObjectStore{})
	ctx, cancel := context.WithCancel(context.Background())
	kube.onDeletePod = cancel // the session's pod teardown runs after the result arrived

	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.execute(ctx, task)
	}()
	for {
		kube.mu.Lock()
		n := len(kube.podOrder)
		kube.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	kube.scriptPods(0, resultLine(true, "pushed branch factory/t8 at abc")+"\n")
	<-done

	if got, _, _ := store.Task(context.Background(), "t8"); got.Status != "done" {
		t.Fatalf("status = %q, want done: the verdict came before the shutdown", got.Status)
	}
	if len(hub.revoked) != 1 {
		t.Fatalf("revoked = %v", hub.revoked)
	}
}

// A requeued task keeps its first start, so the wall-clock cap counts the
// runs before the requeue too.
func TestRunnerWallClockAnchorsToFirstStart(t *testing.T) {
	task := Task{ID: "t9", Status: "queued", Objective: "requeued", Repository: "sf/repo"}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, task, kube, &fakeObjectStore{})
	store.started["t9"] = time.Now().Add(-61 * time.Minute) // cap is 1h

	runner.execute(context.Background(), task)

	if got, _, _ := store.Task(context.Background(), "t9"); got.Status != "failed" {
		t.Fatalf("status = %q, want failed: 61 min since the first start is past the 1h cap", got.Status)
	}
	if len(kube.podOrder) != 0 {
		t.Fatalf("pods = %d, want 0: no session starts past the cap", len(kube.podOrder))
	}
}

func TestRunnerSkipsTaskItCannotClaim(t *testing.T) {
	task := Task{ID: "t7", Status: "running", Objective: "taken", Repository: "sf/repo"}
	kube := newFakeKube()
	runner, hub, store := newTestRunner(t, task, kube, &fakeObjectStore{})

	runner.execute(context.Background(), task)

	if len(kube.createdNs) != 0 || len(hub.minted()) != 0 {
		t.Fatalf("unclaimed task was run: namespaces %v, tokens %v", kube.createdNs, hub.minted())
	}
	if got, _, _ := store.Task(context.Background(), "t7"); got.Status != "running" {
		t.Fatalf("status = %q, want untouched running", got.Status)
	}
}

func TestRunnerSetupFailureDeletesNamespace(t *testing.T) {
	task := Task{ID: "t8", Status: "queued", Objective: "x", Repository: "sf/repo"}
	kube := newFakeKube()
	kube.saErr = fmt.Errorf("apiserver says no")
	runner, hub, store := newTestRunner(t, task, kube, &fakeObjectStore{})

	runner.execute(context.Background(), task)

	if got, _, _ := store.Task(context.Background(), "t8"); got.Status != "failed" {
		t.Fatalf("status = %q, want failed", got.Status)
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
