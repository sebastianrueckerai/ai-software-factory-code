package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"gopkg.in/yaml.v3"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// fakeKube scripts the Kubernetes seam: pods are created with a phase and a
// log body; everything else is recorded for assertions.
type fakeKube struct {
	mu           sync.Mutex
	namespaces   map[string]map[string]string // name → labels
	pods         map[string]*fakePod
	podOrder     []*fakePod // creation order; entries survive deletion
	deletedPods  []string
	deletedNs    []string
	secrets      map[string]map[string]string // ns/name → data
	secretTypes  map[string]string            // ns/name → type
	sourceData   map[string]map[string]string // read-only source secrets
	cnps         map[string]map[string]any    // ns/name → spec
	createdNs    map[string]map[string]string // survives deletion
	objects      map[string]map[string]any    // Create path → object
	saErr        error                        // injected CreateServiceAccount failure
	getErr       map[string]error             // ns/name → injected GetSecretData failure
	statusErr    map[string]error             // ns/name → injected PodStatus failure
	initFinished map[string]time.Time         // ns/name → the first init container's successful finish
	onDeletePod  func()                       // runs on every DeletePod
	ops          []string                     // namespace deletions and pod creations, in order
	applies      []string                     // ns/name of every applied CNP, in order
	onCreatePod  func()                       // runs on every CreatePod
	nsDelay      time.Duration                // every CreateNamespace takes this long
	pvs          []PersistentVolume
	deletedPVs   []string
	tokenTaken   chan struct{} // closed when a worker endpoint took a push token
	takeToken    sync.Once
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
	ns, name        string
	phase           string
	reason, message string // status.reason, status.message (Evicted's)
	logs            string
	logsReady       chan struct{} // closed when scriptPods has assigned the body
	spec            map[string]any
	opens           int           // log streams opened
	deleted         bool          // deletion requested, grace period running
	term            chan struct{} // closed at the deletion: the worker's SIGTERM
	conditions      []PodCondition
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
		tokenTaken:  make(chan struct{}),
	}
}

func (k *fakeKube) CreateNamespace(ctx context.Context, name string, labels map[string]string) error {
	time.Sleep(k.nsDelay)
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
	slices.Sort(names) // the API server lists by name
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

func (k *fakeKube) ApplyCiliumNetworkPolicy(ctx context.Context, namespace, name string, spec map[string]any) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.namespaces[namespace]; !ok {
		return ErrNotFound
	}
	k.cnps[namespace+"/"+name] = spec
	k.applies = append(k.applies, namespace+"/"+name)
	return nil
}

func (k *fakeKube) CiliumNetworkPolicySpec(ctx context.Context, namespace, name string) (map[string]any, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	spec, ok := k.cnps[namespace+"/"+name]
	if !ok {
		return nil, ErrNotFound
	}
	return spec, nil
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
	p := &fakePod{ns: namespace, name: name, phase: "Running", logsReady: make(chan struct{}), spec: pod, term: make(chan struct{})}
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
	p, ok := k.pods[namespace+"/"+name]
	if !ok || p.deleted {
		return ErrNotFound
	}
	k.deletedPods = append(k.deletedPods, namespace+"/"+name)
	if k.onDeletePod != nil {
		k.onDeletePod()
	}
	close(p.term)
	// a worker whose log is cut or goes on after its SIGTERM is still
	// inside its grace period: the pod stays until the rest of its log has
	// been read
	if (strings.Contains(p.logs, cutLogs) || strings.Contains(p.logs, sigtermLogs)) && p.phase == "Running" {
		p.deleted = true
		return nil
	}
	delete(k.pods, namespace+"/"+name)
	return nil
}

func (k *fakeKube) PodInitFinished(ctx context.Context, namespace, name string) (time.Time, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	at, ok := k.initFinished[namespace+"/"+name]
	return at, ok, nil
}

func (k *fakeKube) PodStatus(ctx context.Context, namespace, name string) (string, int32, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err, ok := k.statusErr[namespace+"/"+name]; ok {
		return "", -1, err
	}
	p, ok := k.pods[namespace+"/"+name]
	if !ok {
		return "", -1, ErrNotFound
	}
	return p.phase, 0, nil
}

// PodIP answers loopback for every existing pod: a test's worker
// endpoint listens there.
func (k *fakeKube) PodIP(ctx context.Context, namespace, name string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.pods[namespace+"/"+name]; !ok {
		return "", ErrNotFound
	}
	return "127.0.0.1", nil
}

func (k *fakeKube) PodDiagnosis(ctx context.Context, namespace, name string) (PodDiagnosis, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	p, ok := k.pods[namespace+"/"+name]
	if !ok {
		return PodDiagnosis{}, ErrNotFound
	}
	return PodDiagnosis{Phase: p.phase, Reason: p.reason, Message: p.message, Conditions: p.conditions}, nil
}

// PodLogTail serves the scripted body's last lines.
func (k *fakeKube) PodLogTail(ctx context.Context, namespace, name string, lines int) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	p, ok := k.pods[namespace+"/"+name]
	if !ok {
		return "", ErrNotFound
	}
	all := strings.SplitAfter(p.logs, "\n")
	if all[len(all)-1] == "" {
		all = all[:len(all)-1]
	}
	return strings.Join(all[max(0, len(all)-lines):], ""), nil
}

// PodLog serves the scripted body; reading it to the end means the
// container exited (phase Succeeded, and a deleted pod goes).
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
	body := p.logs
	if before, after, ok := strings.Cut(body, cutLogs); ok {
		k.mu.Lock()
		p.opens++
		first := p.opens == 1
		k.mu.Unlock()
		if first {
			// the stream breaks here while the container runs on
			return io.NopCloser(strings.NewReader(before)), nil
		}
		body = before + after
	}
	r := heldLog(ctx, body, map[string]<-chan struct{}{sigtermLogs: p.term, tokenLogs: k.tokenTaken})
	if rest, ok := strings.CutPrefix(body, trickleLogs); ok {
		r = iotest.HalfReader(strings.NewReader(rest))
	}
	return io.NopCloser(&exitReader{r: r, exited: func() {
		k.mu.Lock()
		defer k.mu.Unlock()
		p.phase = "Succeeded"
		if p.deleted {
			delete(k.pods, namespace+"/"+name)
		}
	}}), nil
}

type exitReader struct {
	r      io.Reader
	exited func()
	done   bool
}

func (e *exitReader) Read(b []byte) (int, error) {
	n, err := e.r.Read(b)
	if err == io.EOF && !e.done {
		e.done = true
		e.exited()
	}
	return n, err
}

// trickleLogs before a log body serves it in small reads, as a live pod log
// arrives: the reader's buffer fills and is reused across lines.
const trickleLogs = "<trickle>"

// cutLogs in a log body breaks the first stream there while the container
// runs on; a reopened stream serves the whole body.
const cutLogs = "<cut>"

// sigtermLogs in a log body holds the stream there until the pod's
// deletion, then serves the rest: what the worker prints after its SIGTERM.
const sigtermLogs = "<sigterm>"

// tokenLogs in a log body holds the stream there until a worker endpoint
// took a push token: the worker waits for it before it pushes.
const tokenLogs = "<token>"

// heldLog serves body, holding the stream at each marker of holds until
// the marker's channel closes.
func heldLog(ctx context.Context, body string, holds map[string]<-chan struct{}) io.Reader {
	var parts []io.Reader
	for {
		at, marker := len(body), ""
		for m := range holds {
			if i := strings.Index(body, m); i >= 0 && i < at {
				at, marker = i, m
			}
		}
		parts = append(parts, strings.NewReader(body[:at]))
		if marker == "" {
			return io.MultiReader(parts...)
		}
		parts = append(parts, holdReader{ctx, holds[marker]})
		body = body[at+len(marker):]
	}
}

type holdReader struct {
	ctx  context.Context
	open <-chan struct{}
}

func (r holdReader) Read([]byte) (int, error) {
	select {
	case <-r.open:
		return 0, io.EOF
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	}
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
		// the worker ends every line it prints
		if body != hangLogs && body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
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

func (k *fakeKube) PersistentVolumes(ctx context.Context) ([]PersistentVolume, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.pvs), nil
}

func (k *fakeKube) DeletePersistentVolume(ctx context.Context, name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.deletedPVs = append(k.deletedPVs, name)
	return nil
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
	mainHead    string // the task repository's main head
	comments    []string
}

func (h *forgejoHub) minted() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.mintedNames...)
}

func (h *forgejoHub) srv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/tokens"):
			var body struct {
				Name         string
				Scopes       []string
				Repositories []Repo
			}
			json.NewDecoder(r.Body).Decode(&body)
			h.nextID++
			h.tokens = append(h.tokens, TokenRef{ID: h.nextID, Name: body.Name})
			h.mintedNames = append(h.mintedNames, body.Name)
			repos := []string{}
			for _, repo := range body.Repositories {
				repos = append(repos, repo.String())
			}
			h.calls = append(h.calls, fmt.Sprintf("mint %s %s %s", body.Name, strings.Join(body.Scopes, ","), strings.Join(repos, ",")))
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d,"sha1":"tok-%s"}`, h.nextID, body.Name)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/repos/factory-agent/") && strings.Count(r.URL.Path, "/") == 5:
			h.calls = append(h.calls, "fork")
			name := path.Base(r.URL.Path)
			fmt.Fprintf(w, `{"full_name":"factory-agent/%s","fork":true,"private":true,"parent":{"full_name":"sf/%s"}}`, name, name)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/tokens"):
			h.calls = append(h.calls, "list")
			list, _ := json.Marshal(h.tokens)
			w.Write(list)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/comments"):
			var list []map[string]any
			for i, c := range h.comments {
				list = append(list, map[string]any{"id": 501 + i, "body": c, "user": map[string]string{"login": "factory-agent"}})
			}
			b, _ := json.Marshal(list)
			w.Write(b)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments"):
			var body struct{ Body string }
			json.NewDecoder(r.Body).Decode(&body)
			h.comments = append(h.comments, body.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d}`, 500+len(h.comments))
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/branches/"):
			if strings.HasPrefix(r.URL.Path, "/api/v1/repos/sf/") && strings.HasSuffix(r.URL.Path, "/branches/main") {
				fmt.Fprintf(w, `{"commit":{"id":%q}}`, h.mainHead)
				return
			}
			// task branches live on the fork only
			if h.branchHead == "" || !strings.HasPrefix(r.URL.Path, "/api/v1/repos/factory-agent/") {
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
		ChecksPoll:           time.Millisecond,
		Supervisor:           SupervisorConfig{NoProgressAfter: time.Minute, RepetitionEvents: 24},
		Policy:               testPolicyVersion,
		PolicyVersion: func(_ context.Context, id int64) (policy.Version, bool, error) {
			return testPolicyVersion(), id == testPolicyVersion().ID, nil
		},
		Leases:      &fakeLeases{},
		Gates:       &GateWatch{},
		LastFailure: func(context.Context, string, string) (string, error) { return "", nil },
		Workflows:   &fakeWorkflows{},
	}
}

// setLimits changes the limits of the runner's policy version.
func setLimits(r *Runner, change func(*policy.Limits)) {
	v := r.cfg.Policy()
	change(&v.Document.Limits)
	r.cfg.Policy = func() policy.Version { return v }
}

// testPolicyVersion is version 7 of testPolicyYAML.
func testPolicyVersion() policy.Version {
	return policy.Version{ID: 7, Commit: "c7", Tree: testTree(testPolicyYAML)}
}

// fakeLeases is the registry's live leased addresses by task.
type fakeLeases struct {
	mu           sync.Mutex
	byID         map[string][]string
	commissioned []cloudres.Commissioned // what the next claim returns
}

func (f *fakeLeases) ClaimCommissioned(context.Context) ([]cloudres.Commissioned, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.commissioned
	f.commissioned = nil
	return out, nil
}

func (f *fakeLeases) LiveAddresses(context.Context) (map[string][]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]string{}
	for k, v := range f.byID {
		out[k] = v
	}
	return out, nil
}

// newTestRunner wires a runner against scripted backends and a queued task.
func newTestRunner(t *testing.T, task Task, kube *fakeKube, objects ObjectStore) (*Runner, *forgejoHub, *fakeStore) {
	t.Helper()
	// main is where the task was filed unless a test moves it
	hub := &forgejoHub{nextID: 100, mainHead: cmp.Or(task.SHA, "m0")}
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
	return runSessionIn(t, r, in, bodies, func(env *testsuite.TestActivityEnvironment) {
		if ctx != nil {
			env.SetWorkerOptions(worker.Options{BackgroundActivityContext: ctx})
		}
		if dead != nil {
			env.SetHeartbeatDetails(*dead)
		}
	})
}

func runSessionIn(t *testing.T, r *Runner, in SessionInput, bodies []string, setup func(*testsuite.TestActivityEnvironment)) (SessionResult, error) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := suite.NewTestActivityEnvironment()
	r.RegisterActivities(env)
	setup(env)
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
	(&fakeWorkerEndpoint{status: http.StatusNoContent}).start(t, runner)

	logs := envLine(harness.Event{Kind: harness.EventFileChange, Time: time.Now(), Detail: map[string]any{"path": "README.md"}}) + "\n" +
		pushRequestLine("c0ffee") + "\n" + tokenLogs + pushedLine("c0ffee") + "\n" + resultLine(true, "pushed branch factory/t1 at c0ffee")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/coding-cheap"}, nil, logs)
	if err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	if res.Outcome != OutcomePushed || res.Commit != "c0ffee" || !strings.HasPrefix(res.SessionID, "s-") {
		t.Fatalf("result = %+v, want pushed c0ffee", res)
	}
	// read on the base repository and the fork to clone, write on the fork at the push: nothing else
	if !slices.Contains(hub.calls, "mint task-t1-base read:repository sf/ai-software-factory-code,factory-agent/ai-software-factory-code") ||
		!slices.Contains(hub.calls, "mint task-t1 write:repository factory-agent/ai-software-factory-code") || len(hub.minted()) != 2 {
		t.Fatalf("forgejo calls = %v, want a read token on sf and a write token on the fork", hub.calls)
	}
	if !slices.Equal(hub.revoked, []int64{101, 102}) {
		t.Fatalf("revoked = %v, want both session tokens revoked at its end", hub.revoked)
	}
	env := podEnv(kube.podAt(0))
	if env["REPO_SHA"] != "abc123" || env["MODEL"] != "factory/coding-cheap" {
		t.Fatalf("pod env = %v, want the base SHA and the cheap model", env)
	}
	if env["PUSH_URL"] != "http://forgejo-http.forgejo.svc.cluster.local:3000/factory-agent/ai-software-factory-code.git" {
		t.Fatalf("pod env = %v, want the push to the fork", env)
	}
	if _, ok := env["GIT_TOKEN"]; ok {
		t.Fatal("GIT_TOKEN is still set: one way only")
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
	if sec := kube.secrets[ns+"/task-git"]; sec["REPO_TOKEN"] != "tok-task-t1-base" || len(sec) != 1 {
		t.Fatalf("git token secret = %v", sec)
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
	var fqdn, dnsRule map[string]any
	for _, rule := range cnp["egress"].([]any) {
		r := rule.(map[string]any)
		switch {
		case r["toFQDNs"] != nil:
			fqdn = r
		case strings.Contains(fmt.Sprint(r["toEndpoints"]), "kube-dns"):
			dnsRule = r
		}
	}
	for _, name := range []string{"matchName:proxy.golang.org", "matchName:sum.golang.org", "matchName:storage.googleapis.com", "matchName:registry.npmjs.org"} {
		if dnsRule == nil || !strings.Contains(fmt.Sprint(dnsRule), name) {
			t.Fatalf("DNS rule lacks %s (ruling 99: the sandbox carries the Go toolchain): %v", name, dnsRule)
		}
	}
	if fqdn == nil || fmt.Sprint(fqdn["toFQDNs"]) != "[map[matchName:proxy.golang.org] map[matchName:sum.golang.org] map[matchName:storage.googleapis.com] map[matchName:registry.npmjs.org]]" || fmt.Sprint(fqdn["toPorts"]) != "[map[ports:[map[port:443 protocol:TCP]]]]" {
		t.Fatalf("toFQDNs rule = %v, want exactly the Go module proxy, its checksum DB and the npm registry on 443", fqdn)
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
	if hard := quota["spec"].(map[string]any)["hard"].(map[string]any); hard["pods"] != "2" || hard["limits.memory"] != "8Gi" {
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
	if mounts := fmt.Sprint(container["volumeMounts"]); !strings.Contains(mounts, "mountPath:/tmp") {
		t.Fatalf("mounts = %s, want a writable /tmp under the read-only root (go build needs one)", mounts)
	}
	sc := container["securityContext"].(map[string]any)
	if sc["readOnlyRootFilesystem"] != true || sc["runAsNonRoot"] != true || sc["allowPrivilegeEscalation"] != false {
		t.Fatalf("container securityContext = %v", sc)
	}
	if res := fmt.Sprint(container["resources"]); res != "map[limits:map[cpu:2 memory:4Gi] requests:map[cpu:250m memory:512Mi]]" {
		t.Fatalf("container resources = %s", res)
	}
	if dns := fmt.Sprint(spec["dnsConfig"]); dns != "map[options:[map[name:ndots value:1]]]" {
		t.Fatalf("dnsConfig = %s", dns)
	}
	volumes := spec["volumes"].([]any)
	for i, want := range []string{"4Gi", "1Gi", "1Gi"} {
		if got := volumes[i].(map[string]any)["emptyDir"].(map[string]any)["sizeLimit"]; got != want {
			t.Fatalf("volume %d sizeLimit = %v, want %s", i, got, want)
		}
	}
	projected := volumes[3].(map[string]any)["projected"].(map[string]any)
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
	// the attempt runs from its session_start to its session_end
	start, end := timelineStep(t, objects, "t1", "session_start"), timelineStep(t, objects, "t1", "session_end")
	began, _ := time.Parse(time.RFC3339, fmt.Sprint(start["time"]))
	started, err := time.Parse(time.RFC3339, fmt.Sprint(end["started"]))
	if err != nil || started.After(began) || began.Sub(started) > time.Second || end["attempt"] != float64(1) {
		t.Fatalf("session_end started %v, attempt %v; want the session_start's %s, attempt 1", end["started"], end["attempt"], start["time"])
	}
	if _, err := objects.Get(context.Background(), "sessions/t1/"+res.SessionID+".jsonl"); err != nil {
		t.Fatal(err)
	}
}

// The clone runs in an init container, the read token's only holder
// (item 40): read on the task's repository and the bot's fork, where a
// resumed session's branch head is. The worker container, where the agent
// runs, gets neither the read token nor the repository URL to use it on.
func TestSessionPodClonesInAnInitContainer(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/x", SHA: "abc123", BudgetEUR: 2}
	kube := newFakeKube()
	runner, hub, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/coding-cheap"}, nil,
		pushedLine("c0ffee")+"\n"+resultLine(true, "pushed branch factory/t1 at c0ffee")); err != nil {
		t.Fatal(err)
	}
	var reads []string
	for _, call := range hub.calls {
		if strings.Contains(call, " read:repository ") {
			reads = append(reads, call)
		}
	}
	if !slices.Equal(reads, []string{"mint task-t1-base read:repository sf/x,factory-agent/x"}) {
		t.Fatalf("read mints = %v, want one read token on sf/x and factory-agent/x", reads)
	}
	spec := kube.podAt(0).spec["spec"].(map[string]any)
	inits, _ := spec["initContainers"].([]any)
	if len(inits) != 1 || inits[0].(map[string]any)["name"] != "clone" {
		t.Fatalf("initContainers = %v, want [clone]", inits)
	}
	clone, worker := inits[0].(map[string]any), spec["containers"].([]any)[0].(map[string]any)
	want := map[string]string{
		"MODE":       "clone",
		"REPO_URL":   "http://forgejo-http.forgejo.svc.cluster.local:3000/sf/x.git",
		"PUSH_URL":   "http://forgejo-http.forgejo.svc.cluster.local:3000/factory-agent/x.git",
		"REPO_SHA":   "abc123",
		"BRANCH":     "factory/t1",
		"REPO_TOKEN": "secret:task-git/REPO_TOKEN",
	}
	if got := containerEnv(clone); !reflect.DeepEqual(got, want) {
		t.Fatalf("clone env = %v, want %v", got, want)
	}
	for _, key := range []string{"image", "command", "resources", "securityContext"} {
		if !reflect.DeepEqual(clone[key], worker[key]) {
			t.Fatalf("clone %s = %v, want the worker's %v", key, clone[key], worker[key])
		}
	}
	// the workspace only: no gateway token, no project cache
	if mounts := fmt.Sprint(clone["volumeMounts"]); mounts != "[map[mountPath:/workspace name:workspace]]" {
		t.Fatalf("clone mounts = %s, want the workspace only", mounts)
	}
	env := containerEnv(worker)
	for _, name := range []string{"REPO_TOKEN", "REPO_URL"} {
		if _, ok := env[name]; ok {
			t.Fatalf("worker container env has %s: %v", name, env)
		}
	}
	if env["REPO_SHA"] != "abc123" || env["BRANCH"] != "factory/t1" || env["PUSH_URL"] != "http://forgejo-http.forgejo.svc.cluster.local:3000/factory-agent/x.git" {
		t.Fatalf("worker container env = %v, want the start, the branch and the fork", env)
	}
}

// podEnv flattens a pod's worker container env into name → value.
func podEnv(p *fakePod) map[string]string {
	return containerEnv(p.spec["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any))
}

// containerEnv flattens a container's env into name → value, a secret
// reference into secret:<name>/<key>.
func containerEnv(container map[string]any) map[string]string {
	env := map[string]string{}
	for _, e := range container["env"].([]map[string]any) {
		if v, ok := e["value"].(string); ok {
			env[e["name"].(string)] = v
		} else if ref, ok := e["valueFrom"].(map[string]any)["secretKeyRef"].(map[string]any); ok {
			env[e["name"].(string)] = fmt.Sprintf("secret:%s/%s", ref["name"], ref["key"])
		}
	}
	return env
}

// The worker gets the tools the task's classes may call under its pinned
// version, for the brief to name.
func TestRunSessionPassesTheOpenTools(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", SHA: "base", RequiredCapabilities: []string{"coder"}}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	v := runner.cfg.Policy()
	coder := v.Document.Classes["coder"]
	coder.Tools = map[string]policy.ToolCap{policy.ToolWebSearch: {MaxRequests: 1, MaxEUR: 0.1}, policy.ToolRequestInfo: {}}
	v.Document.Classes["coder"] = coder
	runner.cfg.Policy = func() policy.Version { return v }
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/coding-cheap"}, nil,
		pushedLine("c0ffee")+"\n"+resultLine(true, "done")); err != nil {
		t.Fatal(err)
	}
	if got := podEnv(kube.podAt(0))["TOOLS"]; got != `["request_info","web_search"]` {
		t.Fatalf("TOOLS = %s, want request_info and web_search", got)
	}
}

func TestRunSessionStartsFromTheGivenRef(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", SHA: "base"}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, task, kube, &fakeObjectStore{})

	_, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 2, Model: "factory/coding-frontier",
		StartRef: "c1", ParentSessionID: "s-prev", Findings: []string{"check \"pr / test\": failure"},
		Answers: []string{"sebastian: b", "sebastian: a"}}, nil, resultLine(false, "gave up"))
	if err != nil {
		t.Fatal(err)
	}
	env := podEnv(kube.podAt(0))
	if env["REPO_SHA"] != "c1" || env["MODEL"] != "factory/coding-frontier" || env["PARENT_SESSION_ID"] != "s-prev" ||
		!strings.Contains(env["FINDINGS"], "pr / test") || env["ANSWERS"] != `["sebastian: b","sebastian: a"]` {
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
// A session whose worker was lost before it ever ran (pod never
// scheduled, image pull stuck) still gets linked as the parent of its
// replacement; only a session that ran adds the interruption finding.
func TestRunSessionRetryLinksDeadSessionThatNeverRan(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "long", Repository: "sf/repo", SHA: "base"}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, task, kube, &fakeObjectStore{})
	dead := &sessionProgress{SessionID: "s-dead"}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/coding-cheap", Findings: []string{"earlier note"}},
		dead, pushedLine("c2")+"\n"+resultLine(true, "pushed"))
	if err != nil {
		t.Fatal(err)
	}
	env := podEnv(kube.podAt(0))
	if env["PARENT_SESSION_ID"] != "s-dead" {
		t.Fatalf("PARENT_SESSION_ID = %q, want s-dead", env["PARENT_SESSION_ID"])
	}
	var findings []string
	if err := json.Unmarshal([]byte(env["FINDINGS"]), &findings); err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0] != "earlier note" {
		t.Fatalf("findings = %q, want the input only: nothing ran, nothing was interrupted", findings)
	}
	if sess := store.sessions[res.SessionID]; sess.ParentSessionID != "s-dead" {
		t.Fatalf("session row = %+v, want parent s-dead", sess)
	}
}

func TestRunSessionRetryReplacesDeadSession(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "long", Repository: "sf/repo", SHA: "base"}
	kube := newFakeKube()
	runner, hub, store := newTestRunner(t, task, kube, &fakeObjectStore{})
	kube.namespaces["factory-agents-t1"] = map[string]string{"factory.ai/task": "t1"}
	kube.pods["factory-agents-t1/worker-s-dead"] = &fakePod{ns: "factory-agents-t1", name: "worker-s-dead", phase: "Running", term: make(chan struct{})}
	hub.tokens = []TokenRef{{ID: 50, Name: "task-t1"}, {ID: 51, Name: "task-t1-base"}}

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
	mint := slices.IndexFunc(hub.calls, func(c string) bool { return strings.HasPrefix(c, "mint ") })
	if r50, r51 := slices.Index(hub.calls, "revoke 50"), slices.Index(hub.calls, "revoke 51"); mint < 0 || r50 < 0 || r51 < 0 || r50 > mint || r51 > mint {
		t.Fatalf("forgejo calls = %v, want both leftover tokens revoked before the new mints", hub.calls)
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

// A first session (no branch, no given ref) starts at main as it is now
// and records it as the task's base; a given ref leaves the base alone.
func TestFirstSessionStartsAtMainHead(t *testing.T) {
	for name, tc := range map[string]struct{ ref, wantStart, wantSHA string }{
		"no ref": {"", "main2", "main2"},
		"ref c3": {"c3", "c3", "seed1"},
	} {
		task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", SHA: "seed1"}
		kube := newFakeKube()
		runner, hub, store := newTestRunner(t, task, kube, &fakeObjectStore{})
		hub.mainHead = "main2"

		if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "m", StartRef: tc.ref}, nil, resultLine(false, "x")); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if env := podEnv(kube.podAt(0)); env["REPO_SHA"] != tc.wantStart {
			t.Fatalf("%s: REPO_SHA = %q, want %s", name, env["REPO_SHA"], tc.wantStart)
		}
		if got, _, _ := store.Task(context.Background(), "t1"); got.SHA != tc.wantSHA {
			t.Fatalf("%s: task sha = %q, want %s", name, got.SHA, tc.wantSHA)
		}
	}
}

// loopFailure is one turn of "edit, test, same failure".
func loopFailure(i int) string {
	return envLine(harness.Event{Kind: harness.EventFileChange, Time: time.Now(), Detail: map[string]any{"path": "notes.md", "sha256": fmt.Sprint("h", i)}}) + "\n" +
		envLine(harness.Event{Kind: harness.EventCommand, Time: time.Now(), Detail: map[string]any{
			"command": "sh scripts/loopcheck.sh", "exit": 1, "output": fmt.Sprintf("loopcheck: FAIL at 10:00:%02d", i)}}) + "\n"
}

func supervisedTask(id string) Task {
	return Task{ID: id, Status: "running", Objective: "make loopcheck pass", Repository: "sf/repo", BudgetEUR: 0.3, SpentEUR: 0.05, IssueNumber: 7,
		RequiredCapabilities: []string{policy.ClassCoder}}
}

// The third identical failure trips same_failure; the supervisor (through
// the gateway on the task's account) says escalate; the worker is stopped, pushes
// its work in progress, and the decision lands in the session record, the
// timeline and on the issue.
func TestRunSessionTripAsksTheSupervisorAndStopsGracefully(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, store := newTestRunner(t, supervisedTask("t2"), kube, objects)
	f := &fakeLiteLLM{content: `{"action":"escalate","reason":"the check needs a file the agent never created"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)

	logs := loopFailure(1) + loopFailure(2) + loopFailure(3) +
		// what the worker prints after its SIGTERM
		envLine(harness.Event{Kind: harness.EventFinished, Time: time.Now(), Detail: map[string]any{"success": false}}) + "\n" +
		pushedLine("c-wip") + "\n" + resultLine(false, "stopped: work in progress pushed at c-wip") + "\n"
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/coding-cheap"}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeEscalate || res.Commit != "c-wip" || !strings.HasPrefix(res.Reason, "the check needs a file the agent never created") {
		t.Fatalf("result = %+v, want escalate with the reason and the work in progress", res)
	}
	if joined := strings.Join(res.Findings, "\n"); !strings.Contains(joined, "c-wip") {
		t.Fatalf("findings = %v, want the pushed work", res.Findings)
	}
	if len(f.chats) != 1 || f.chats[0]["model"] != "supervisor" || f.tasks[0] != "t2" || f.purposes[0] != "supervisor" {
		t.Fatalf("calls = %d, tasks %v, purposes %v; want one supervisor call through the gateway on t2", len(f.chats), f.tasks, f.purposes)
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
		if ev.Kind == harness.EventSupervisor && (ev.Detail["detector"] != "same_failure" || ev.Detail["decision"] != "escalate" ||
			!strings.Contains(fmt.Sprint(ev.Detail["digest"]), "3 commands (3 failed)")) {
			t.Fatalf("supervisor event = %v", ev.Detail)
		}
	}
	if strings.Join(kinds, ",") != "file_change,command,file_change,command,file_change,command,supervisor,finished,pushed" {
		t.Fatalf("record kinds = %v", kinds)
	}
	timeline := string(objGet(objects, "sessions/t2/timeline.jsonl"))
	if !strings.Contains(timeline, `"step":"supervisor"`) || !strings.Contains(timeline, `"decision":"escalate"`) {
		t.Fatalf("timeline = %s", timeline)
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "**escalate**") || !strings.Contains(hub.comments[0], "same_failure") {
		t.Fatalf("comments = %v", hub.comments)
	}
	if snap := store.sessions[res.SessionID]; len(snap.Findings) == 0 {
		t.Fatal("tripped session left no findings snapshot")
	}
}

func TestRunSessionContinueLetsTheSessionRunOn(t *testing.T) {
	kube := newFakeKube()
	runner, hub, store := newTestRunner(t, supervisedTask("t2"), kube, &fakeObjectStore{})
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
	// the trip is an alert the supervisor judged noise by continuing
	alerts := store.alertRows()
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want the one trip", alerts)
	}
	if a := alerts[0]; a.Kind != AlertSupervisorTrip || a.Source != string(TripSameFailure) || a.TaskID != "t2" ||
		a.Verdict != VerdictNoise || a.ResolvedBy != "supervisor" || a.Detail["action"] != "continue" || a.Detail["session"] != res.SessionID ||
		a.Detail["diagnosis"] != "each run fails later" {
		t.Fatalf("alert = %+v", a)
	}
}

// Three continues use up the session's calls; the fourth trip escalates
// without a call, and the escalation is left to the workflow's comment.
func TestRunSessionFourthTripEscalatesWithoutACall(t *testing.T) {
	runner, hub, store := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
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
	if len(hub.comments) != 4 || !strings.Contains(hub.comments[3], "**escalate**") {
		t.Fatalf("comments = %q, want one per decision, the escalation too", hub.comments)
	}
	var verdicts []string
	for _, a := range store.alertRows() {
		verdicts = append(verdicts, a.Verdict+" "+fmt.Sprint(a.Detail["action"]))
	}
	if want := []string{"noise continue", "noise continue", "noise continue", "real escalate"}; !slices.Equal(verdicts, want) {
		t.Fatalf("trip verdicts = %q, want %q", verdicts, want)
	}
}

// A supervisor still thinking when the wall-clock cap arrives is cut off:
// the cap ends the session.
func TestRunSessionCapCutsOffASlowSupervisor(t *testing.T) {
	runner, hub, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
	setLimits(runner, func(l *policy.Limits) { l.AgentTime = time.Second })
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
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "Superseded: the task's wall-clock cap was reached during the supervisor call") {
		t.Fatalf("comments = %q, want the decision commented with the superseded note", hub.comments)
	}
}

// What a session that ended without a push did reaches its result.
func TestRunSessionResultCarriesFindings(t *testing.T) {
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
	logs := envLine(harness.Event{Kind: harness.EventCommand, Time: time.Now(), Detail: map[string]any{"command": "go test ./...", "exit": 1}}) + "\n" + resultLine(false, "gave up")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Findings, "ran `go test ./...` (exit 1)") {
		t.Fatalf("result = %+v, want the command in the findings", res)
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
	if !slices.Equal(hub.revoked, []int64{101}) {
		t.Fatalf("revoked = %v: a failed session must still revoke its read token, the one it minted", hub.revoked)
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

// The pod's own diagnosis, read before its deletion, tells an eviction
// apart from any other death: the next session's findings name the cause
// instead of the generic "pod exited without a result".
func TestEvictionRecorded(t *testing.T) {
	task := Task{ID: "t4e", Status: "running", Objective: "flaky", Repository: "sf/repo"}
	kube := newFakeKube()
	kube.onCreatePod = func() {
		p := kube.podOrder[len(kube.podOrder)-1]
		p.reason, p.message = "Evicted", "The node was low on resource: ephemeral-storage."
	}
	runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t4e", Attempt: 1, Model: "m"}, nil,
		envLine(harness.Event{Kind: harness.EventModelCall, Time: time.Now()})+"\n")
	if err != nil || res.Outcome != OutcomeNoPush || res.Summary != "evicted: The node was low on resource: ephemeral-storage." {
		t.Fatalf("result = (%+v, %v)", res, err)
	}
}

// A session reads its limits from the version its workflow started under,
// whatever is current now; a workflow from before the pin (version 0)
// reads the current one.
func TestRunSessionTakesThePinnedVersion(t *testing.T) {
	for name, tc := range map[string]struct {
		pin             int64
		pinned, current time.Duration
	}{
		"pinned":         {pin: 7, pinned: 300 * time.Millisecond, current: time.Hour},
		"before the pin": {pin: 0, pinned: time.Hour, current: 300 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			task := Task{ID: "t5", Status: "running", Objective: "slow", Repository: "sf/repo"}
			runner, _, _ := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
			runner.cfg.Supervisor = SupervisorConfig{NoProgressAfter: 3 * time.Second, RepetitionEvents: 24}
			pinned, current := testPolicyVersion(), testPolicyVersion()
			pinned.Document.Limits.AgentTime = tc.pinned
			current.ID, current.Document.Limits.AgentTime = 8, tc.current
			runner.cfg.Policy = func() policy.Version { return current }
			runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
			began := time.Now()
			res, err := runSession(t, nil, runner, SessionInput{TaskID: "t5", Attempt: 1, Model: "m", PolicyVersion: tc.pin}, nil, hangLogs)
			if err != nil || res.Outcome != OutcomeWallClock || time.Since(began) > 10*time.Second {
				t.Fatalf("result = (%+v, %v) after %s, want wall_clock at 300ms", res, err, time.Since(began))
			}
		})
	}
}

// The worker's step cap is the pinned version's limits.steps, and
// DefaultSteps for a version pinned before the key existed.
func TestRunSessionPassesThePinnedStepCap(t *testing.T) {
	for name, tc := range map[string]struct {
		pinned int
		want   string
	}{
		"pinned":           {pinned: 40, want: "40"},
		"pinned before it": {pinned: 0, want: "200"},
	} {
		t.Run(name, func(t *testing.T) {
			task := Task{ID: "t5", Status: "running", Objective: "steps", Repository: "sf/repo"}
			kube := newFakeKube()
			runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
			pinned, current := testPolicyVersion(), testPolicyVersion()
			pinned.Document.Limits.Steps = tc.pinned
			current.ID, current.Document.Limits.Steps = 8, 90
			runner.cfg.Policy = func() policy.Version { return current }
			runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
			if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t5", Attempt: 1, Model: "m", PolicyVersion: 7}, nil,
				pushedLine("c0ffee")+"\n"+resultLine(true, "pushed")); err != nil {
				t.Fatal(err)
			}
			if got := podEnv(kube.podAt(0))["STEPS"]; got != tc.want {
				t.Fatalf("STEPS = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunSessionWallClockTrip(t *testing.T) {
	task := Task{ID: "t5", Status: "running", Objective: "slow", Repository: "sf/repo"}
	kube := newFakeKube()
	runner, hub, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	runner.cfg.Supervisor = SupervisorConfig{NoProgressAfter: 3 * time.Second, RepetitionEvents: 24}
	setLimits(runner, func(l *policy.Limits) { l.AgentTime = 300 * time.Millisecond })

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t5", Attempt: 1, Model: "m"}, nil, hangLogs)
	if err != nil || res.Outcome != OutcomeWallClock {
		t.Fatalf("result = (%+v, %v), want wall_clock", res, err)
	}
	if !slices.Equal(hub.revoked, []int64{101}) {
		t.Fatalf("revoked = %v, want the read token, the one a session that never asked to push minted", hub.revoked)
	}
}

// A cancelled attempt (the workflow decided while the session ran) ends
// it: its pod, namespace and credentials go, the session's end is on the
// timeline, and the cancel is the activity's error.
func TestRunSessionCancelCleansUpAndFails(t *testing.T) {
	task := Task{ID: "t6", Status: "running", Objective: "long", Repository: "sf/repo"}
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, _ := newTestRunner(t, task, kube, objects)
	ctx, cancel := context.WithCancel(context.Background())
	kube.onCreatePod = func() { go func() { time.Sleep(20 * time.Millisecond); cancel() }() }

	_, err := runSession(t, ctx, runner, SessionInput{TaskID: "t6", Attempt: 1, Model: "m"}, nil, hangLogs)
	var app *temporal.ApplicationError
	if !errors.As(err, &app) || app.Message() != context.Canceled.Error() {
		t.Fatalf("err = %v, want the cancel to reach the activity", err)
	}
	if !slices.Equal(hub.revoked, []int64{101}) {
		t.Fatalf("revoked = %v: the session's read token, the one it minted, must not survive", hub.revoked)
	}
	if _, ok := kube.namespaces["factory-agents-t6"]; ok {
		t.Fatal("namespace survived the cancel")
	}
	if timeline := string(objGet(objects, "sessions/t6/timeline.jsonl")); !strings.Contains(timeline, `"step":"session_end"`) || !strings.Contains(timeline, `"outcome":"aborted"`) {
		t.Fatalf("timeline = %s, want the aborted session's end", timeline)
	}
}

// factoryd stopping (the worker stop channel) is not the session's end:
// the worker is stopped gracefully and pushes its work in progress, the
// heartbeat carries its report, and the retry on the next factoryd
// continues from it.
func TestRunSessionWorkerStopPushesWorkInProgress(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, _ := newTestRunner(t, supervisedTask("t2"), kube, objects)
	runner.stopWait = 10 * time.Second
	stop := make(chan struct{})
	kube.onCreatePod = func() { go func() { time.Sleep(20 * time.Millisecond); close(stop) }() }
	var last sessionProgress
	var mu sync.Mutex

	logs := loopFailure(1) + cutLogs + pushedLine("c-wip") + "\n" + resultLine(false, "stopped; work in progress pushed to factory/t2 at c-wip") + "\n"
	_, err := runSessionIn(t, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, []string{logs}, func(env *testsuite.TestActivityEnvironment) {
		env.SetWorkerStopChannel(stop)
		env.SetOnActivityHeartbeatListener(func(_ *activity.Info, details converter.EncodedValues) {
			mu.Lock()
			defer mu.Unlock()
			_ = details.Get(&last)
		})
	})
	if err == nil || !strings.Contains(err.Error(), "factoryd stopping") || errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a retryable stop", err)
	}
	if !slices.Contains(kube.deletedPods, "factory-agents-t2/worker-"+last.SessionID) {
		t.Fatalf("deleted pods = %v, want the worker stopped", kube.deletedPods)
	}
	mu.Lock()
	defer mu.Unlock()
	if !last.Running || !slices.Contains(last.Findings, "the stopped worker reported: stopped; work in progress pushed to factory/t2 at c-wip") ||
		!slices.Contains(last.Findings, "the stopped session's work in progress is on the branch at c-wip") {
		t.Fatalf("last heartbeat = %+v, want the stopped worker's report for the retry", last)
	}
	if timeline := string(objGet(objects, "sessions/t2/timeline.jsonl")); !strings.Contains(timeline, `"outcome":"stopped"`) || !strings.Contains(timeline, "c-wip") {
		t.Fatalf("timeline = %s, want the stopped session's end with its push", timeline)
	}
	if !slices.Equal(hub.revoked, []int64{101}) {
		t.Fatalf("revoked = %v: the session's read token, the one it minted, must not survive", hub.revoked)
	}
	if _, ok := kube.namespaces["factory-agents-t2"]; ok {
		t.Fatal("namespace survived the stop")
	}
}

// factoryd stopping while the supervisor is still thinking: the call is
// cut, its decision is not acted on, and the worker is stopped within the
// grace period, not after the call's own timeout.
func TestRunSessionWorkerStopCutsAnInFlightSupervisorCall(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, _ := newTestRunner(t, supervisedTask("t2"), kube, objects)
	runner.stopWait = 10 * time.Second
	stop := make(chan struct{})
	var once sync.Once
	sup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key/list":
			io.WriteString(w, `{"keys":[],"total_pages":1}`)
		case "/v1/chat/completions":
			// factoryd starts stopping while this call is in flight
			io.ReadAll(r.Body)
			once.Do(func() { close(stop) })
			<-r.Context().Done()
		}
	}))
	t.Cleanup(sup.Close)
	runner.supervisor = testSupervisorModel(sup.URL)
	runner.supervisor.HTTP = &http.Client{Timeout: 3 * time.Second}
	var last sessionProgress
	var mu sync.Mutex

	logs := loopFailure(1) + loopFailure(2) + loopFailure(3) + cutLogs +
		pushedLine("c-wip") + "\n" + resultLine(false, "stopped; work in progress pushed to factory/t2 at c-wip") + "\n"
	began := time.Now()
	_, err := runSessionIn(t, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, []string{logs}, func(env *testsuite.TestActivityEnvironment) {
		env.SetWorkerStopChannel(stop)
		env.SetOnActivityHeartbeatListener(func(_ *activity.Info, details converter.EncodedValues) {
			mu.Lock()
			defer mu.Unlock()
			_ = details.Get(&last)
		})
	})
	if err == nil || !strings.Contains(err.Error(), "factoryd stopping") {
		t.Fatalf("err = %v, want the retryable stop, not the cut call's decision", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("took %s: the stop waited for the supervisor call", took)
	}
	if len(hub.comments) != 0 {
		t.Fatalf("comments = %q, want no decision from the cut call", hub.comments)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(last.Findings, "the stopped session's work in progress is on the branch at c-wip") {
		t.Fatalf("last heartbeat = %+v, want the stopped worker's push for the retry", last)
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
	if !slices.Equal(hub.revoked, []int64{101}) {
		t.Fatalf("revoked = %v, want the read token revoked", hub.revoked)
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

func TestSessionResultTakesTheBranchAsPushedWhenASessionAddsNothing(t *testing.T) {
	done := &harness.Result{Success: true, Summary: "no changes to push"}
	// resumed from the work in progress a stop pushed: that work is the result
	res := sessionResult(sessionSpec{id: "s1", startRef: "wip", base: "main"}, sessionOutcome{result: done})
	if res.Outcome != OutcomePushed || res.Commit != "wip" || res.Summary != "no changes to push; the branch already held wip" {
		t.Fatalf("resumed session without new changes: %+v", res)
	}
	// started at the base and changed nothing: no push
	res = sessionResult(sessionSpec{id: "s1", startRef: "main", base: "main"}, sessionOutcome{result: done})
	if res.Outcome != OutcomeNoPush || res.Commit != "" {
		t.Fatalf("first session without changes: %+v", res)
	}
}

func TestContentFilterRefusalGoesToAHuman(t *testing.T) {
	const prefix = "the model provider's content filter refused this session's model call (DataInspectionFailed): "
	const suffix = ". Close this card and file it again with the wording changed, or answer to try once more."
	summary := "AI_APICallError: InternalError.Algo.DataInspectionFailed: input check failed (status 400)"
	res := sessionResult(sessionSpec{id: "s1"}, sessionOutcome{result: &harness.Result{Success: false, Summary: summary}})
	if res.Outcome != OutcomeEscalate || res.Reason != prefix+summary+suffix {
		t.Fatalf("content-filter refusal: %+v", res)
	}
	summary = "provider error: data_inspection_failed (status 400)"
	res = sessionResult(sessionSpec{id: "s1"}, sessionOutcome{result: &harness.Result{Success: false, Summary: summary}})
	if res.Outcome != OutcomeEscalate || !strings.HasPrefix(res.Reason, prefix+summary) {
		t.Fatalf("snake-case refusal: %+v", res)
	}
	res = sessionResult(sessionSpec{id: "s1"}, sessionOutcome{result: &harness.Result{Success: false, Summary: "AI_APICallError: rate limited (status 429)"}})
	if res.Outcome != OutcomeNoPush {
		t.Fatalf("rate limit: %+v", res)
	}
}

func TestSessionResultKeepsWhatAStoppedWorkerReported(t *testing.T) {
	s := sessionSpec{id: "s1"}
	trip := &Trip{Kind: TripSameFailure, Detail: "x", Findings: []string{"ran y"}}
	redirect := &Decision{Action: ActionRedirect, Guidance: "g"}

	res := sessionResult(s, sessionOutcome{trip: trip, decision: redirect,
		result: &harness.Result{Summary: "push failed: refusing to push: .forgejo/x changes .forgejo/"}})
	if res.Outcome != OutcomeRedirect || !slices.Contains(res.Findings, "the stopped worker reported: push failed: refusing to push: .forgejo/x changes .forgejo/") {
		t.Fatalf("refused push: %+v", res)
	}
	res = sessionResult(s, sessionOutcome{trip: trip, decision: redirect, noResult: "the stopped worker did not report within 1m5s"})
	if !slices.Contains(res.Findings, "work in progress not pushed: the stopped worker did not report within 1m5s") {
		t.Fatalf("timed-out stop: %+v", res)
	}
	res = sessionResult(s, sessionOutcome{trip: &Trip{Kind: TripWallClock}, noResult: "the stopped worker exited without a result"})
	if res.Outcome != OutcomeWallClock || !slices.Contains(res.Findings, "work in progress not pushed: the stopped worker exited without a result") {
		t.Fatalf("wall-clock stop: %+v", res)
	}
	// the worker finished and pushed while the supervisor was asked
	res = sessionResult(s, sessionOutcome{trip: trip, decision: redirect, commit: "c1", result: &harness.Result{Success: true, Summary: "pushed branch b at c1"}})
	if res.Outcome != OutcomePushed || res.Commit != "c1" {
		t.Fatalf("pushed during the call: %+v, want verification of c1", res)
	}
}

// The log stream breaks while the worker still runs (here during the
// stop's grace period); the log is reopened and the worker's report and
// pushed work in progress are not lost, nor recorded twice.
func TestRunSessionStopSurvivesABrokenLogStream(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), kube, objects)
	f := &fakeLiteLLM{content: `{"action":"escalate","reason":"stop"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)
	runner.stopWait = 10 * time.Second

	logs := loopFailure(1) + loopFailure(2) + loopFailure(3) + cutLogs +
		pushedLine("c-wip") + "\n" + resultLine(false, "stopped; work in progress pushed to factory/t2 at c-wip") + "\n"
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeEscalate || res.Commit != "c-wip" ||
		!slices.Contains(res.Findings, "the stopped worker reported: stopped; work in progress pushed to factory/t2 at c-wip") {
		t.Fatalf("result = %+v, want the escalation with the pushed work in progress", res)
	}
	record := strings.TrimSpace(string(objGet(objects, "sessions/t2/"+res.SessionID+".jsonl")))
	if n := strings.Count(record, `"kind":"command"`); n != 3 || strings.Count(record, `"kind":"pushed"`) != 1 {
		t.Fatalf("record has %d commands, want each event once:\n%s", n, record)
	}
}

// A break outside a stop: the session reads on after the reopen.
func TestRunSessionReadsOnAfterABrokenLogStream(t *testing.T) {
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
	logs := envLine(harness.Event{Kind: harness.EventModelCall, Time: time.Now()}) + "\n" + cutLogs + pushedLine("c1") + "\n" + resultLine(true, "pushed")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed || res.Commit != "c1" {
		t.Fatalf("result = (%+v, %v), want pushed c1", res, err)
	}
}

func TestReadLogLinesSkipsDropsLongLinesAndHoldsBrokenTails(t *testing.T) {
	long := strings.Repeat("x", maxLogLine+10)
	body := "a\nb\n" + long + "\nc\nunterminated"
	var got []string
	n, err := readLogLines(strings.NewReader(body), 1, func(l []byte) bool { got = append(got, string(l)); return true })
	if n != 4 || err != io.EOF || !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("n = %d, err = %v, lines = %q; want 4 lines seen (the long one dropped), b and c delivered, the broken tail held", n, err, got)
	}
}

// The follower reads the whole log ahead while nobody consumes (a
// supervisor call blocks the session loop), so a pod deleted meanwhile
// loses nothing; every line is delivered afterwards, in order.
func TestFollowPodLogReadsAheadWithoutLimit(t *testing.T) {
	kube := newFakeKube()
	kube.CreatePod(context.Background(), "ns", "p", nil)
	var body strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&body, "line %d\n", i)
	}
	kube.scriptPods(0, body.String())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pl := followPodLog(ctx, kube, "ns", "p", "t1", nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if phase, _, _ := kube.PodStatus(ctx, "ns", "p"); phase == "Succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the follower stopped reading while nobody consumed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	kube.DeletePod(ctx, "ns", "p")
	n := 0
	for l := range pl.lines {
		if string(l) != fmt.Sprintf("line %d", n) {
			t.Fatalf("line %d = %q", n, l)
		}
		n++
	}
	if n != 5000 || pl.err() != nil {
		t.Fatalf("delivered %d lines, err %v; want 5000, none", n, pl.err())
	}
}

// Past the read-ahead cap the follower stops and says so: the session
// fails loudly instead of losing lines.
func TestFollowPodLogFailsLoudlyPastItsBuffer(t *testing.T) {
	kube := newFakeKube()
	kube.CreatePod(context.Background(), "ns", "p", nil)
	kube.scriptPods(0, strings.Repeat("0123456789\n", 1000))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pl := followPodLogCap(ctx, kube, "ns", "p", "t1", 5000, nil)
	time.Sleep(200 * time.Millisecond)
	n := 0
	for range pl.lines {
		n++
	}
	if pl.err() == nil || n == 0 || n >= 1000 {
		t.Fatalf("delivered %d lines, err %v; want what fit, then an error", n, pl.err())
	}
}

// An escalation overtaken by the session's own push is commented as
// superseded; the task goes to verification, not to a human.
func TestRunSessionEscalationSupersededByAPush(t *testing.T) {
	runner, hub, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
	f := &fakeLiteLLM{content: `{"action":"escalate","reason":"impossible"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)
	logs := loopFailure(1) + loopFailure(2) + loopFailure(3) + pushedLine("c1") + "\n" + resultLine(true, "pushed branch factory/t2 at c1")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed {
		t.Fatalf("result = (%+v, %v), want pushed", res, err)
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "**escalate**") ||
		!strings.Contains(hub.comments[0], "Superseded: the session finished and pushed c1 during the supervisor call; the task goes to verification.") {
		t.Fatalf("comments = %q", hub.comments)
	}
}

func TestWorkerEgressAdmitsLeasedServersOnSSH(t *testing.T) {
	r := &Runner{cfg: testRunnerConfig()}
	plain := r.workerEgressSpec(nil)["egress"].([]any)
	spec := r.workerEgressSpec([]string{"203.0.113.7", "203.0.113.8"})
	egress := spec["egress"].([]any)
	if len(egress) != len(plain)+2 {
		t.Fatalf("egress rules = %d, want %d", len(egress), len(plain)+2)
	}
	for i, ip := range []string{"203.0.113.7", "203.0.113.8"} {
		want := map[string]any{
			"toCIDR":  []string{ip + "/32"},
			"toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": "22", "protocol": "TCP"}}}},
		}
		if got := egress[len(plain)+i]; !reflect.DeepEqual(got, want) {
			t.Fatalf("leased rule %d = %v, want %v", i, got, want)
		}
	}
	for _, rule := range plain {
		if _, ok := rule.(map[string]any)["toCIDR"]; ok {
			t.Fatalf("a CIDR allow without a lease: %v", rule)
		}
	}
}

// A commissioned server's commissioning lands on its task's timeline, once,
// running from the request to the server's live moment.
func TestRecordCommissioned(t *testing.T) {
	objects := &fakeObjectStore{}
	requested := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	leases := &fakeLeases{commissioned: []cloudres.Commissioned{{ID: 11, TaskID: "t1", ServerType: "cx23", Requested: requested, Live: requested.Add(95 * time.Second)}}}
	cfg := testRunnerConfig()
	cfg.Leases = leases
	r := &Runner{cfg: cfg, timeline: &Timeline{Objects: objects}}
	for range 2 {
		if err := r.RecordCommissioned(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(string(objGet(objects, "sessions/t1/timeline.jsonl")), `"step":"commissioned"`); n != 1 {
		t.Fatalf("%d commissioned steps, want 1", n)
	}
	step := timelineStep(t, objects, "t1", "commissioned")
	if step["started"] != "2026-09-28T10:00:00Z" || step["live"] != "2026-09-28T10:01:35Z" || step["resource"] != float64(11) || step["server_type"] != "cx23" {
		t.Fatalf("commissioned step = %v", step)
	}
}

func TestAdmitLeasesFollowsTheRegistry(t *testing.T) {
	kube := newFakeKube()
	for _, ns := range []string{"factory-agents-t1", "factory-agents-t2", "factory-agents-ci-5"} {
		kube.namespaces[ns] = map[string]string{"factory.ai/task": strings.TrimPrefix(ns, "factory-agents-")}
	}
	leases := &fakeLeases{byID: map[string][]string{"t1": {"203.0.113.7"}, "gone": {"203.0.113.9"}}}
	cfg := testRunnerConfig()
	cfg.Leases = leases
	r := &Runner{cfg: cfg, kube: kube}
	ctx := context.Background()
	rules := func(ns string) string {
		kube.mu.Lock()
		defer kube.mu.Unlock()
		return fmt.Sprint(kube.cnps[ns+"/worker-egress"]["egress"])
	}
	if err := r.AdmitLeases(ctx); err != nil {
		t.Fatal(err)
	}
	// agent namespaces without the desired policy get it; CI ones never
	if got := strings.Join(kube.applies, ","); got != "factory-agents-t1/worker-egress,factory-agents-t2/worker-egress" {
		t.Fatalf("applies = %s", got)
	}
	if !strings.Contains(rules("factory-agents-t1"), "203.0.113.7/32") || strings.Contains(rules("factory-agents-t2"), "/32") {
		t.Fatalf("t1 %s\nt2 %s", rules("factory-agents-t1"), rules("factory-agents-t2"))
	}
	if err := r.AdmitLeases(ctx); err != nil || len(kube.applies) != 2 {
		t.Fatalf("unchanged pass applied again: %v, %v", kube.applies, err)
	}
	// the lease ends: its address leaves the policy
	leases.mu.Lock()
	delete(leases.byID, "t1")
	leases.mu.Unlock()
	if err := r.AdmitLeases(ctx); err != nil {
		t.Fatal(err)
	}
	if len(kube.applies) != 3 || strings.Contains(rules("factory-agents-t1"), "203.0.113.7") {
		t.Fatalf("applies %v, t1 %s", kube.applies, rules("factory-agents-t1"))
	}
	// an edit out of band is repaired on the next pass
	kube.mu.Lock()
	kube.cnps["factory-agents-t2/worker-egress"] = map[string]any{"endpointSelector": map[string]any{}}
	kube.mu.Unlock()
	if err := r.AdmitLeases(ctx); err != nil || len(kube.applies) != 4 || kube.applies[3] != "factory-agents-t2/worker-egress" {
		t.Fatalf("drift not repaired: %v, %v", kube.applies, err)
	}
	if !strings.Contains(rules("factory-agents-t2"), "kube-dns") {
		t.Fatalf("t2 %s", rules("factory-agents-t2"))
	}
}

// forbiddenPolicy answers a GET of the egress policy in ns as the API does
// once the namespace's RoleBinding is gone (teardown) or not yet made.
type forbiddenPolicy struct {
	*fakeKube
	ns string
}

func (k forbiddenPolicy) CiliumNetworkPolicySpec(ctx context.Context, namespace, name string) (map[string]any, error) {
	if namespace == k.ns {
		return nil, fmt.Errorf("%w: GET /apis/cilium.io/v2/namespaces/%s/ciliumnetworkpolicies/%s", ErrForbidden, namespace, name)
	}
	return k.fakeKube.CiliumNetworkPolicySpec(ctx, namespace, name)
}

// A namespace without factoryd's RoleBinding is being set up or torn
// down: the pass skips it without an error and admits the others.
func TestAdmitLeasesSkipsANamespaceWithoutItsBinding(t *testing.T) {
	kube := newFakeKube()
	for _, id := range []string{"t1", "t2"} {
		kube.namespaces["factory-agents-"+id] = map[string]string{"factory.ai/task": id}
	}
	cfg := testRunnerConfig()
	cfg.Leases = &fakeLeases{byID: map[string][]string{}}
	r := &Runner{cfg: cfg, kube: forbiddenPolicy{fakeKube: kube, ns: "factory-agents-t1"}}
	if err := r.AdmitLeases(context.Background()); err != nil {
		t.Fatalf("AdmitLeases = %v, want nil", err)
	}
	if got := strings.Join(kube.applies, ","); got != "factory-agents-t2/worker-egress" {
		t.Fatalf("applies = %s, want t2's only", got)
	}
}

func TestSetupNamespaceAdmitsTheTasksLeases(t *testing.T) {
	kube := newFakeKube()
	kube.sourceData["factory/forgejo-registry"] = map[string]string{".dockerconfigjson": "{}"}
	cfg := testRunnerConfig()
	cfg.Leases = &fakeLeases{byID: map[string][]string{"t1": {"203.0.113.7"}, "t2": {"203.0.113.8"}}}
	r := &Runner{cfg: cfg, kube: kube, store: newFakeStore()}
	if _, err := r.setupNamespace(context.Background(), Task{ID: "t1"}, ForgejoToken{}); err != nil {
		t.Fatal(err)
	}
	egress := fmt.Sprint(kube.cnps["factory-agents-t1/worker-egress"]["egress"])
	if !strings.Contains(egress, "203.0.113.7/32") || strings.Contains(egress, "203.0.113.8") {
		t.Fatalf("egress = %s", egress)
	}
	// the loop sees it applied already
	if err := r.AdmitLeases(context.Background()); err != nil || len(kube.applies) != 1 {
		t.Fatalf("applies = %v, %v", kube.applies, err)
	}
}

// The check wait polls, and so heartbeats, every HeartbeatEvery: a
// terminal signal's cancel reaches it as fast as a running session.
func TestChecksPollDefaultsToTheHeartbeatInterval(t *testing.T) {
	cfg := testRunnerConfig()
	cfg.ChecksPoll = 0
	r, err := NewRunner(cfg, newFakeStore(), newFakeKube(), &Forgejo{}, &Completion{}, &fakeObjectStore{}, testSupervisorModel("http://unused"))
	if err != nil {
		t.Fatal(err)
	}
	if r.cfg.ChecksPoll != HeartbeatEvery {
		t.Fatalf("checks poll = %s, want %s", r.cfg.ChecksPoll, HeartbeatEvery)
	}
}

// The gateway's ingress admits only the node's probes, factoryd and the
// sandboxes (opencode-worker pods in a factory.ai/task namespace). Every
// other in-cluster pod, a project's or a CI runner's, is refused even
// though its token would not authenticate.
func TestGatewayIngressAdmitsFactorydAndTheSandboxes(t *testing.T) {
	data, err := os.ReadFile("../../k8s/deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var spec any
	for {
		var doc struct {
			Kind     string
			Metadata struct{ Name, Namespace string }
			Spec     any
		}
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if doc.Kind == "CiliumNetworkPolicy" && doc.Metadata.Name == "factory-gateway-ingress" && doc.Metadata.Namespace == "factory" {
			spec = doc.Spec
		}
	}
	want := `{"endpointSelector":{"matchLabels":{"app":"factory-gateway"}},"ingress":[{"fromEntities":["host"],"toPorts":[{"ports":[{"port":"8081","protocol":"TCP"}]}]},{"fromEndpoints":[{"matchLabels":{"app":"factoryd"}}],"toPorts":[{"ports":[{"port":"8081","protocol":"TCP"}]}]},{"fromEndpoints":[{"matchExpressions":[{"key":"k8s:io.cilium.k8s.namespace.labels.factory.ai/task","operator":"Exists"}],"matchLabels":{"app.kubernetes.io/name":"opencode-worker"}}],"toPorts":[{"ports":[{"port":"8081","protocol":"TCP"}]}]}]}`
	if got := jsonOf(spec); got != want {
		t.Fatalf("factory-gateway-ingress spec = %s, want %s", got, want)
	}
}
