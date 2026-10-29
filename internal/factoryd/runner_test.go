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
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"reflect"
	"regexp"
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
	terminating  map[string]bool              // name → metadata.deletionTimestamp is set
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
	applyErr     error                        // injected ApplyCiliumNetworkPolicy failure
	getErr       map[string]error             // ns/name → injected GetSecretData failure
	statusErr    map[string]error             // ns/name → injected PodStatus failure
	initFinished map[string]time.Time         // ns/name → the first init container's successful finish
	onDeletePod  func()                       // runs on every DeletePod
	ops          []string                     // namespace deletions and pod creations, in order
	applies      []string                     // ns/name of every applied CNP, in order
	onCreatePod  func()                       // runs on every CreatePod
	onDiagnosis  func()                       // runs on every PodDiagnosis, under mu
	// keepDeleted keeps a deleted pod readable after its exit, as one
	// whose deletion the API server has not completed yet
	keepDeleted bool
	nsDelay     time.Duration // every CreateNamespace takes this long
	pvs         []PersistentVolume
	deletedPVs  []string
	cronJobs    map[string][]CronJobState // namespace → its CronJobs and ScheduledBackups
	deployments map[string]string         // ns/name → Deployment JSON
	cronJobsErr map[string]error          // "<kind> <namespace>" → injected failure of its read
	tokenTaken  chan struct{}             // closed when a worker endpoint took a push token
	takeToken   sync.Once
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
	containers      []ContainerState // init containers first, as PodDiagnosis reads them
}

func newFakeKube() *fakeKube {
	return &fakeKube{
		namespaces:  map[string]map[string]string{},
		terminating: map[string]bool{},
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
	delete(k.terminating, name) // a recreated namespace has no deletionTimestamp
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
	nsList, err := k.ListNamespacesByLabel(ctx, key, value)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(nsList))
	for _, ns := range nsList {
		names = append(names, ns.Name)
	}
	return names, nil
}

// ListNamespacesByLabel reports the deletion state fakeKube's namespaces
// carry: InClusterKube decodes metadata.deletionTimestamp, and reconcile is
// the caller that needs it.
func (k *fakeKube) ListNamespacesByLabel(ctx context.Context, key, value string) ([]NamespaceInfo, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []NamespaceInfo
	for name, labels := range k.namespaces {
		v, ok := labels[key]
		if value == "" && ok || v == value {
			out = append(out, NamespaceInfo{Name: name, Terminating: k.terminating[name], Labels: maps.Clone(labels)})
		}
	}
	slices.SortFunc(out, func(a, b NamespaceInfo) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
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
	if k.applyErr != nil {
		return k.applyErr
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

// CreatePod refuses a pod in a namespace that does not exist or is being
// deleted, as the API server does.
func (k *fakeKube) CreatePod(ctx context.Context, namespace, name string, pod map[string]any) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.namespaces[namespace]; !ok {
		return fmt.Errorf("%w: namespace %s", ErrNotFound, namespace)
	}
	if k.terminating[namespace] {
		return fmt.Errorf("%w: namespace %s is being terminated", ErrForbidden, namespace)
	}
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
	if k.onDiagnosis != nil {
		k.onDiagnosis()
	}
	p, ok := k.pods[namespace+"/"+name]
	if !ok {
		return PodDiagnosis{}, ErrNotFound
	}
	return PodDiagnosis{Phase: p.phase, Reason: p.reason, Message: p.message, Conditions: p.conditions, Containers: p.containers}, nil
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
		if p.deleted && !k.keepDeleted {
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

func (k *fakeKube) CronJobs(ctx context.Context, namespace string) ([]CronJobState, error) {
	return k.jobsOf(KindCronJob, namespace)
}

func (k *fakeKube) ScheduledBackups(ctx context.Context, namespace string) ([]CronJobState, error) {
	return k.jobsOf(KindScheduledBackup, namespace)
}

func (k *fakeKube) Deployment(ctx context.Context, namespace, name string) (Deployment, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	body, ok := k.deployments[namespace+"/"+name]
	if !ok {
		return Deployment{}, fmt.Errorf("deployment %s/%s: %w", namespace, name, ErrNotFound)
	}
	var d Deployment
	return d, json.Unmarshal([]byte(body), &d)
}

func (k *fakeKube) jobsOf(kind, namespace string) ([]CronJobState, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.cronJobsErr[kind+" "+namespace]; err != nil {
		return nil, err
	}
	var out []CronJobState
	for _, j := range k.cronJobs[namespace] {
		if j.Kind == kind {
			out = append(out, j)
		}
	}
	return out, nil
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
	failWrites  int    // write-token mints to answer 502 before succeeding
	prFiles     string // a pull request's files, as Forgejo lists them
	// pulls are GET /pulls/{n} bodies, unlisted: 404; pullStatus answers
	// a number with its status instead
	pulls      map[int64]string
	pullStatus map[int64]int
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
			if slices.Contains(body.Scopes, "write:repository") && h.failWrites > 0 {
				h.failWrites--
				h.calls = append(h.calls, fmt.Sprintf("mint %s %s failed", body.Name, strings.Join(body.Scopes, ",")))
				w.WriteHeader(http.StatusBadGateway)
				return
			}
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
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/repos/sf/") && strings.Count(r.URL.Path, "/") == 5:
			fmt.Fprint(w, `{"default_branch":"main"}`)
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
		case r.Method == "GET" && path.Base(path.Dir(r.URL.Path)) == "pulls":
			h.calls = append(h.calls, "pull "+path.Base(r.URL.Path))
			var n int64
			fmt.Sscanf(path.Base(r.URL.Path), "%d", &n)
			body, ok := h.pulls[n]
			switch {
			case h.pullStatus[n] != 0:
				w.WriteHeader(h.pullStatus[n])
			case !ok:
				http.NotFound(w, r)
			default:
				io.WriteString(w, body)
			}
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/files") && h.prFiles != "":
			io.WriteString(w, h.prFiles)
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
		SandboxBaseImage:     "reg/sandbox-base@sha256:bcd",
		RegistryHost:         "store.test",
		PullSecret:           "forgejo-registry",
		FactoryNamespace:     "factory",
		GatewayURL:           "http://factory-gateway.factory.svc.cluster.local:8081",
		GitBaseURL:           "http://forgejo-http.forgejo.svc.cluster.local:3000",
		Models:               ModelLadder{Start: "factory/small", Stronger: "factory/big"},
		AnswerTokens:         8192,
		HeadroomTokens:       16384,
		ChecksPoll:           time.Millisecond,
		EgressCanary:         "example.com:443",
		EgressDirectIP:       "192.0.2.7:443",
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
	runner.endpointWait = 50 * time.Millisecond
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
	// pods of sessions this runner already ran are not this call's: a
	// second session on the same runner scripts its own pods only
	kube.mu.Lock()
	base := len(kube.podOrder)
	kube.mu.Unlock()
	assigned := 0
	for assigned < len(bodies) {
		kube.mu.Lock()
		created := len(kube.podOrder) - base
		kube.mu.Unlock()
		if created > assigned {
			kube.scriptPods(base+assigned, bodies[assigned:created]...)
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
		Repository: "sf/ai-software-factory-code", SHA: "abc123", BudgetEUR: 2, Track: "direct"}
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, _ := newTestRunner(t, task, kube, objects)
	(&fakeWorkerEndpoint{status: http.StatusNoContent}).start(t, runner)

	logs := envLine(harness.Event{Kind: harness.EventFileChange, Time: time.Now(), Detail: map[string]any{"path": "README.md"}}) + "\n" +
		pushRequestLine("c0ffee") + "\n" + tokenLogs + pushedLine("c0ffee") + "\n" + resultLine(true, "pushed branch factory/t1 at c0ffee")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small"}, nil, logs)
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
	if env["REPO_SHA"] != "abc123" || env["MODEL"] != "factory/small" {
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
	// ephemeral-storage: workspace 4, home 1, tmp 4, the harness 1 and the module cache 8
	if res := fmt.Sprint(container["resources"]); res != "map[limits:map[cpu:2 memory:4Gi] requests:map[cpu:250m ephemeral-storage:18Gi memory:512Mi]]" {
		t.Fatalf("container resources = %s", res)
	}
	if dns := fmt.Sprint(spec["dnsConfig"]); dns != "map[options:[map[name:ndots value:1]]]" {
		t.Fatalf("dnsConfig = %s", dns)
	}
	volumes := spec["volumes"].([]any)
	for i, want := range []string{"4Gi", "1Gi", "4Gi", "1Gi"} {
		if got := volumes[i].(map[string]any)["emptyDir"].(map[string]any)["sizeLimit"]; got != want {
			t.Fatalf("volume %d sizeLimit = %v, want %s", i, got, want)
		}
	}
	projected := volumes[4].(map[string]any)["projected"].(map[string]any)
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
	// session_started is SessionAnswered's to write, never the pod's start's
	if strings.Contains(string(timeline), `"event":"session_started"`) {
		t.Fatalf("timeline holds a session_started no answered call wrote:\n%s", timeline)
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
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/x", SHA: "abc123", BudgetEUR: 2, Track: "direct"}
	kube := newFakeKube()
	runner, hub, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small"}, nil,
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
	clone, worker := initContainer(t, spec, "clone"), spec["containers"].([]any)[0].(map[string]any)
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
	// the workspace and the harness only: no gateway token, no project cache
	if mounts := fmt.Sprint(clone["volumeMounts"]); mounts != "[map[mountPath:/workspace name:workspace] map[mountPath:/factory name:harness readOnly:true]]" {
		t.Fatalf("clone mounts = %s, want the workspace and the harness only", mounts)
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

// Every sandbox pod runs the harness from the factory's image, whatever
// image its worker runs: an init container, first, copies agent-worker,
// opencode and the opencode bake into the pod's harness volume, as the
// worker's uid under the worker's confinement and resources; the worker
// runs /factory/bin/agent-worker off the volume mounted read-only. The
// clone or fetch init container, which holds the read token, runs the
// factory's image whatever image the worker runs.
func TestSandboxPodsTakeTheHarnessFromAnInitContainer(t *testing.T) {
	r := &Runner{cfg: testRunnerConfig(), forgejo: &Forgejo{BotUser: "factory-agent"}}
	task := Task{ID: "t1", Repository: "linkshort/linkshort", Track: "direct"}
	project := Task{ID: "t1", Repository: "linkshort/linkshort", ImageDigest: "reg/project@sha256:def", Track: "direct"}
	for name, c := range map[string]struct {
		pod   map[string]any
		image string
		inits []string
	}{
		"sandbox":                    {r.sandboxPodSpec(task, "reg/project@sha256:def", "", nil, false), "reg/project@sha256:def", []string{"harness"}},
		"session":                    {r.workerPodSpec(task, sessionSpec{id: "s-1", cache: "cache-agent-linkshort"}), "reg/worker@sha256:abc", []string{"harness", "clone"}},
		"session on a project image": {r.workerPodSpec(project, sessionSpec{id: "s-1", cache: "cache-agent-linkshort"}), "reg/project@sha256:def", []string{"harness", "clone"}},
		"repro": {r.reproPodSpec(task, Repo{Owner: "linkshort", Name: "linkshort"}, pullRef(1), "c1", ""),
			"reg/worker@sha256:abc", []string{"harness", "fetch"}},
		"repro on a project image": {r.reproPodSpec(project, Repo{Owner: "linkshort", Name: "linkshort"}, pullRef(1), "c1", ""),
			"reg/project@sha256:def", []string{"harness", "fetch"}},
	} {
		spec := c.pod["spec"].(map[string]any)
		inits, _ := spec["initContainers"].([]any)
		var names []string
		for _, ic := range inits {
			names = append(names, ic.(map[string]any)["name"].(string))
		}
		if !slices.Equal(names, c.inits) {
			t.Fatalf("%s: init containers %v, want %v", name, names, c.inits)
		}
		first, worker := inits[0].(map[string]any), spec["containers"].([]any)[0].(map[string]any)
		if first["image"] != "reg/worker@sha256:abc" || !reflect.DeepEqual(first["command"], []any{"/bin/sh", "-c",
			"mkdir -p /factory/bin && cp /usr/local/bin/agent-worker /usr/local/bin/opencode /factory/bin/ && cp -a /opt/opencode-home /factory/"}) {
			t.Errorf("%s: harness runs %v %q, want the factory's image copying the harness", name, first["image"], first["command"])
		}
		if got := string(mustJSON(first["volumeMounts"])); got != `[{"mountPath":"/factory","name":"harness"}]` {
			t.Errorf("%s: harness mounts %s, want the harness volume alone, writable", name, got)
		}
		if _, ok := first["env"]; ok {
			t.Errorf("%s: harness carries env %v, want none", name, first["env"])
		}
		// a failed copy names its cause in the pod's diagnosis
		if first["terminationMessagePolicy"] != "FallbackToLogsOnError" {
			t.Errorf("%s: harness terminationMessagePolicy %v, want FallbackToLogsOnError", name, first["terminationMessagePolicy"])
		}
		for _, key := range []string{"securityContext", "resources"} {
			if !reflect.DeepEqual(first[key], worker[key]) {
				t.Errorf("%s: harness %s = %v, want the worker's %v", name, key, first[key], worker[key])
			}
		}
		if worker["image"] != c.image || !reflect.DeepEqual(worker["command"], []any{"/bin/sh", "-c", cacheUmaskSh, "/factory/bin/agent-worker"}) {
			t.Errorf("%s: worker runs %v %q, want %s running the harness's agent-worker", name, worker["image"], worker["command"], c.image)
		}
		for _, ic := range slices.Concat(inits[1:], []any{worker}) {
			ic := ic.(map[string]any)
			want := "reg/worker@sha256:abc"
			if ic["name"] == "worker" {
				want = c.image
			}
			if ic["image"] != want || !strings.Contains(string(mustJSON(ic["volumeMounts"])), `{"mountPath":"/factory","name":"harness","readOnly":true}`) {
				t.Errorf("%s: %s runs %v with mounts %s, want %s with the harness read-only", name, ic["name"], ic["image"], mustJSON(ic["volumeMounts"]), want)
			}
		}
		var harnessVolume any
		for _, v := range spec["volumes"].([]any) {
			if v.(map[string]any)["name"] == "harness" {
				harnessVolume = v
			}
		}
		if got := string(mustJSON(harnessVolume)); got != `{"emptyDir":{"sizeLimit":"1Gi"},"name":"harness"}` {
			t.Errorf("%s: harness volume %s, want an emptyDir of 1Gi", name, got)
		}
	}
}

// initContainer is the pod spec's init container name.
func initContainer(t *testing.T, spec map[string]any, name string) map[string]any {
	t.Helper()
	for _, ic := range spec["initContainers"].([]any) {
		if ic.(map[string]any)["name"] == name {
			return ic.(map[string]any)
		}
	}
	t.Fatalf("no init container %s in %v", name, spec["initContainers"])
	return nil
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

// A model call's text reaches the bus as session_said of the session and
// the card, at the step's time; a call without text and a tool call
// publish nothing.
func TestRunSessionPublishesTheAgentsText(t *testing.T) {
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
	(&fakeWorkerEndpoint{status: http.StatusNoContent}).start(t, runner)
	runner.cfg.Bus = NewEventBus()
	ch := make(chan Event, 8)
	defer runner.cfg.Bus.Subscribe("sf/repo", ch)()
	at := time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)
	logs := envLine(harness.Event{Kind: harness.EventModelCall, Time: at, Detail: map[string]any{"text": "Reading the store first.", "cost": 0.01}}) + "\n" +
		envLine(harness.Event{Kind: harness.EventModelCall, Time: at.Add(time.Second), Detail: map[string]any{"cost": 0.01}}) + "\n" +
		envLine(harness.Event{Kind: harness.EventToolCall, Time: at.Add(2 * time.Second), Detail: map[string]any{"tool": "read"}}) + "\n" +
		pushedLine("c1") + "\n" + resultLine(true, "pushed")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	want := Event{Repository: "sf/repo", TaskID: "t2", IssueNumber: 7, Kind: EventSessionSaid, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"session": res.SessionID, "text": "Reading the store first."}}
	select {
	case got := <-ch:
		gotAt := got.At
		got.At = time.Time{}
		if !gotAt.Equal(at) || !reflect.DeepEqual(got, want) {
			t.Fatalf("published %+v at %s, want %+v at %s", got, gotAt, want, at)
		}
	default:
		t.Fatal("no session_said published")
	}
	select {
	case got := <-ch:
		t.Fatalf("published another %+v", got)
	default:
	}
}

// The worker gets the tools the task's classes may call under its pinned
// version, for the brief to name.
func TestRunSessionPassesTheOpenTools(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", SHA: "base", RequiredCapabilities: []string{"coder"}, Track: "direct"}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	v := runner.cfg.Policy()
	coder := v.Document.Classes["coder"]
	coder.Tools = map[string]policy.ToolCap{policy.ToolWebSearch: {MaxRequests: 1, MaxEUR: 0.1}, policy.ToolRequestInfo: {}}
	v.Document.Classes["coder"] = coder
	runner.cfg.Policy = func() policy.Version { return v }
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small"}, nil,
		pushedLine("c0ffee")+"\n"+resultLine(true, "done")); err != nil {
		t.Fatal(err)
	}
	if got := podEnv(kube.podAt(0))["TOOLS"]; got != `["request_info","web_search"]` {
		t.Fatalf("TOOLS = %s, want request_info and web_search", got)
	}
}

// A tool card's session gets no request_tool: it declares tools its own
// sandbox never gets.
func TestRunSessionGivesAToolCardNoRequestTool(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", SHA: "base", RequiredCapabilities: []string{"coder"}, Source: SourceToolRequest, Track: "direct"}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	v := runner.cfg.Policy()
	coder := v.Document.Classes["coder"]
	coder.Tools = map[string]policy.ToolCap{policy.ToolRequestTool: {}, policy.ToolRequestInfo: {}}
	v.Document.Classes["coder"] = coder
	runner.cfg.Policy = func() policy.Version { return v }
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small"}, nil,
		pushedLine("c0ffee")+"\n"+resultLine(true, "done")); err != nil {
		t.Fatal(err)
	}
	if got := podEnv(kube.podAt(0))["TOOLS"]; got != `["request_info"]` {
		t.Fatalf("TOOLS = %s, want request_info alone", got)
	}
}

// A report session's card restarts on no image: its tools leave out
// request_tool, which a coding session of the same classes gets.
func TestReportSessionGetsNoRequestTool(t *testing.T) {
	r := &Runner{cfg: testRunnerConfig(), forgejo: &Forgejo{BotUser: "factory-agent"}}
	doc := testPolicyVersion().Document
	coder := doc.Classes["coder"]
	coder.Tools = map[string]policy.ToolCap{policy.ToolRequestTool: {}, policy.ToolRequestInfo: {}}
	doc.Classes["coder"] = coder
	task := Task{ID: "t1", Repository: "sf/repo", RequiredCapabilities: []string{"coder"}, Track: "direct"}
	for report, want := range map[string]string{"": `["request_info","request_tool"]`, "the guide": `["request_info"]`} {
		pod := r.workerPodSpec(task, sessionSpec{id: "s-1", policy: doc, report: report})
		if got := containerEnv(pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any))["TOOLS"]; got != want {
			t.Errorf("report %q: TOOLS = %s, want %s", report, got, want)
		}
	}
}

// A writer's hand-filed card: the last line "Directories: <dir>, <dir>" of
// its issue body, key in any case, names the directories of its brief's
// map (the pod's DIRECTORIES), and the map lists them, so the brief's
// objective (OBJECTIVE) holds no Directories line (roadmap 201); the
// title is no line of the body. An outside account's or an alert's line,
// or a card without one, names none, and its objective keeps every line.
// Every objective leaves out the intake's Size, Budget and Rung lines.
func TestRunSessionPassesTheCardsDirectories(t *testing.T) {
	line := "Fix the drawer\n\nIt breaks.\nDirectories: internal/console, internal/policy\n"
	kept := "Fix the drawer\n\nIt breaks.\nDirectories: internal/console, internal/policy"
	for _, tc := range []struct {
		objective, source string
		writer            bool
		want, brief       string
	}{
		{line, "webhook", true, `["internal/console","internal/policy"]`, "Fix the drawer\n\nIt breaks."},
		{"Fix the drawer\n\nDirectories: a\nDIRECTORIES:  internal/console ,, internal/gone", "webhook", true, `["internal/console","internal/gone"]`, "Fix the drawer"},
		{"Fix the drawer\n\nThe directories: internal/x are no line of their own", "webhook", true, "", "Fix the drawer\n\nThe directories: internal/x are no line of their own"},
		{"Directories: internal/console", "webhook", true, "", "Directories: internal/console"},
		{"Directories: internal/console\n\nIt breaks.", "webhook", true, "", "Directories: internal/console\n\nIt breaks."},
		{line, "webhook", false, "", kept},
		{line, SourceAlert, true, "", kept},
		{"fix it", "webhook", true, "", "fix it"},
		{"Fix the drawer\n\nSize: medium\nDirectories: internal/console\n\nIt breaks.\nBudget: 3 EUR\n\nSize: small\nRung: cheap, a one-line fix", "webhook", true,
			`["internal/console"]`, "Fix the drawer\n\nIt breaks."},
		{"Fix the drawer\n\nIt breaks.\nSize: huge\nRung: soon", "webhook", false, "", "Fix the drawer\n\nIt breaks.\nSize: huge\nRung: soon"},
	} {
		task := Task{ID: "t1", Status: "running", Objective: tc.objective, Repository: "sf/repo", SHA: "base", Source: tc.source,
			Author: "sebastian", AuthorWriter: tc.writer, RequiredCapabilities: []string{"coder"}, Track: "direct"}
		kube := newFakeKube()
		runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
		if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small"}, nil,
			pushedLine("c0ffee")+"\n"+resultLine(true, "done")); err != nil {
			t.Fatal(err)
		}
		env := podEnv(kube.podAt(0))
		if got, ok := env["DIRECTORIES"]; got != tc.want || ok != (tc.want != "") {
			t.Errorf("%q from %s (writer %v): DIRECTORIES = %q (set %v), want %q", tc.objective, tc.source, tc.writer, got, ok, tc.want)
		}
		if env["OBJECTIVE"] != tc.brief {
			t.Errorf("%q from %s (writer %v): OBJECTIVE = %q, want %q", tc.objective, tc.source, tc.writer, env["OBJECTIVE"], tc.brief)
		}
	}
}

func TestRunSessionStartsFromTheGivenRef(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", SHA: "base", Track: "direct"}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, task, kube, &fakeObjectStore{})

	_, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 2, Model: "factory/big",
		StartRef: "c1", ParentSessionID: "s-prev", Findings: []string{"check \"pr / test\": failure"},
		Answers: []string{"sebastian: b", "sebastian: a"}}, nil, resultLine(false, "gave up"))
	if err != nil {
		t.Fatal(err)
	}
	env := podEnv(kube.podAt(0))
	if env["REPO_SHA"] != "c1" || env["MODEL"] != "factory/big" || env["PARENT_SESSION_ID"] != "s-prev" ||
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
	task := Task{ID: "t1", Status: "running", Objective: "long", Repository: "sf/repo", SHA: "base", Track: "direct"}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, task, kube, &fakeObjectStore{})
	dead := &sessionProgress{SessionID: "s-dead"}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small", Findings: []string{"earlier note"}},
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
	task := Task{ID: "t1", Status: "running", Objective: "long", Repository: "sf/repo", SHA: "base", Track: "direct"}
	kube := newFakeKube()
	runner, hub, store := newTestRunner(t, task, kube, &fakeObjectStore{})
	kube.namespaces["factory-agents-t1"] = map[string]string{"factory.ai/task": "t1"}
	kube.pods["factory-agents-t1/worker-s-dead"] = &fakePod{ns: "factory-agents-t1", name: "worker-s-dead", phase: "Running", term: make(chan struct{})}
	hub.tokens = []TokenRef{{ID: 50, Name: "task-t1"}, {ID: 51, Name: "task-t1-base"}}

	dead := &sessionProgress{SessionID: "s-dead", Running: true, Events: 7}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small", Findings: []string{"earlier note"}},
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
	if len(findings) != 2 || findings[0] != "earlier note" || !strings.Contains(findings[1], "s-dead stopped after 7 events") {
		t.Fatalf("findings = %q, want the input and the interruption, no command line of the dead session", findings)
	}
	if sess := store.sessions[res.SessionID]; sess.ParentSessionID != "s-dead" {
		t.Fatalf("session row = %+v, want parent s-dead", sess)
	}
	timeline, _ := runner.objects.Get(context.Background(), "sessions/t1/timeline.jsonl")
	if !strings.Contains(string(timeline), `"parent":"s-dead"`) || !strings.Contains(string(timeline), `"findings":["earlier note","session s-dead stopped after 7 events`) {
		t.Fatalf("timeline = %s, want the session start with its parent and findings", timeline)
	}
}

// A dead session may have pushed before its worker was lost; the retry
// builds on the branch head, not on the ref the attempt was scheduled with,
// or its non-force push would be refused.
func TestRunSessionStartsFromTheBranchHead(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", SHA: "base", Track: "direct"}
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

// A first session on a row with no stamp (no branch, no given ref) starts
// at main as it is now and records it as the task's base; a given ref, and
// a row already stamped (by intake or an earlier start), leave the base
// alone — the start ref reads the stamp, the fork point it names.
func TestFirstSessionStartsAtMainHead(t *testing.T) {
	for name, tc := range map[string]struct{ sha, ref, wantStart, wantSHA string }{
		"no stamp": {"", "", "main2", "main2"},
		"stamped":  {"seed1", "", "seed1", "seed1"},
		"ref c3":   {"seed1", "c3", "c3", "seed1"},
	} {
		task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", SHA: tc.sha, Track: "direct"}
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

// The base stamp is written once: the first session start stamps main's
// head, and a later start, or a session-start retry while the branch does
// not exist yet (the sandbox cut it but has not pushed it), reads that
// stamp — the fork point the branch was cut from — instead of re-reading
// main's head, which may have moved in between. A re-stamp made the
// pull-request gate refuse a clean branch against a base it was not cut
// from (task t-0c9745c86d12).
func TestSessionStartStampsTheBaseOnce(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", Track: "direct"}
	kube := newFakeKube()
	runner, hub, store := newTestRunner(t, task, kube, &fakeObjectStore{})
	ctx := context.Background()

	// the first session start: no branch, no stamp: main's head is the
	// start ref and becomes the task's base
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "m"}, nil, resultLine(false, "x")); err != nil {
		t.Fatal(err)
	}
	if env := podEnv(kube.podAt(0)); env["REPO_SHA"] != "m0" {
		t.Fatalf("first session's REPO_SHA = %q, want main's head m0", env["REPO_SHA"])
	}
	if got, _, _ := store.Task(ctx, "t1"); got.SHA != "m0" {
		t.Fatalf("task sha = %q, want the first stamp m0", got.SHA)
	}

	// main moves before the retry; the branch, still unpushed, does not
	// exist on the fork: the first stamp stays the start ref and the base
	hub.mainHead = "main2"
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 2, Model: "m"}, nil, resultLine(false, "x")); err != nil {
		t.Fatal(err)
	}
	if env := podEnv(kube.podAt(1)); env["REPO_SHA"] != "m0" {
		t.Fatalf("retry's REPO_SHA = %q, want the first stamp m0, not main's moved head main2", env["REPO_SHA"])
	}
	if got, _, _ := store.Task(ctx, "t1"); got.SHA != "m0" {
		t.Fatalf("task sha = %q, want the first stamp m0 kept", got.SHA)
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
		RequiredCapabilities: []string{policy.ClassCoder}, Track: "direct"}
}

// The third identical failure trips same_failure; the check-in inside the
// live session hands over with a note: the pod is stopped gracefully (its
// work in progress pushed), the outcome is no_push with the note and the
// trip riding it, and the workflow writes the check-in's timeline step.
func TestRunSessionCheckinHandoverStopsGracefully(t *testing.T) {
	note := "stop here; the loop needs a different shape"
	runner, _, kube, worker, chat, _, objects := liveRig(t, CheckinResponse{Text: `{"decision":"handover","handover":"` + note + `"}`})

	logs := loopFailure(1) + loopFailure(2) + loopFailure(3) +
		// what the worker prints after its SIGTERM
		envLine(harness.Event{Kind: harness.EventFinished, Time: time.Now(), Detail: map[string]any{"success": false}}) + "\n" +
		pushedLine("c-wip") + "\n" + resultLine(false, "stopped: work in progress pushed at c-wip") + "\n"
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/small"}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeNoPush || res.Commit != "c-wip" || res.Trip != string(TripSameFailure) || len(res.Findings) != 0 {
		t.Fatalf("result = %+v, want no_push with the pushed work in progress and the trip named", res)
	}
	if res.CheckIn == nil || res.CheckIn.Decision != CheckHandover || res.CheckIn.Handover != note {
		t.Fatalf("check-in = %+v, want the handover with its note", res.CheckIn)
	}
	if rec := res.CheckIn.Record; rec == nil || rec.Decision != CheckHandover || rec.Moment != CheckInSession ||
		rec.Session != res.SessionID || rec.ParentSession != res.SessionID {
		t.Fatalf("record = %+v, want the session-moment handover record of the session's own parentage", res.CheckIn.Record)
	}
	if chat.calls != 1 || chat.aliases[0] != "big" {
		t.Fatalf("chat = %d calls on %v, want one conversation on the check-in's alias", chat.calls, chat.aliases)
	}
	if len(kube.deletedPods) == 0 {
		t.Fatal("the worker was not stopped")
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if !slices.Equal(worker.texts, []string{"POST /brief "}) {
		t.Fatalf("worker got %q, want only the start brief: a handover pushes nothing to the pod", worker.texts)
	}
	// the workflow writes the check-in's step for a riding outcome; the
	// runner writes none
	if tl := objGet(objects, "sessions/t2/timeline.jsonl"); strings.Contains(string(tl), `"step":"checkin"`) {
		t.Fatalf("timeline = %s, want no runner-side check-in step", tl)
	}
}

// The in-session check-in's transcript tools read the live session's own
// record: a search over the trip's events answers the line it found in
// the next round of the conversation.
func TestRunSessionCheckinToolsReadTheLiveRecord(t *testing.T) {
	runner, _, _, _, chat, _, _ := liveRig(t, CheckinResponse{})
	chat.answers = []CheckinResponse{
		{ToolName: "transcript_search", ToolArgs: map[string]any{"query": "loopcheck: FAIL at 10:00:01"}},
		{Text: `{"decision":"continue","reason":"one failing command, the session carries on"}`},
	}
	logs := loopFailure(1) + loopFailure(2) + loopFailure(3) + pushedLine("c1") + "\n" + resultLine(true, "done")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/small"}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomePushed || res.Commit != "c1" || res.CheckIn != nil {
		t.Fatalf("result = (%+v, %v), want the session run on to its push after a continue", res, err)
	}
	if chat.calls != 2 {
		t.Fatalf("chat calls = %d, want the tool round and the decision", chat.calls)
	}
	// the tool's answer is the conversation's last appended piece: on an
	// empty parent the search returns nothing and the round ends at the
	// marker
	_, toolOut, _ := strings.Cut(chat.gots[1].Input, "answered:\n")
	if !strings.Contains(toolOut, "loopcheck: FAIL at 10:00:01") {
		t.Fatalf("tool round ends with %q, want the live record's line the search found", toolOut)
	}
}

// The check-in's human ask inside the live session ends it the way the
// gate's does: the record rides the result, and no conversation-time text
// is invented for the human.
func TestRunSessionCheckinHumanEndsTheSession(t *testing.T) {
	runner, hub, _, _, chat, _, _ := liveRig(t, CheckinResponse{Text: `{"decision":"human","reason_kind":"goal_conflict","reason":"only a human can pick the license"}`})

	logs := commands(0, 24) + pushedLine("c-wip") + "\n" + resultLine(false, "stopped")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeCheckinHuman || res.Reason != "only a human can pick the license" || res.Commit != "c-wip" {
		t.Fatalf("result = %+v, want the human ask in the check-in's own words with the work in progress", res)
	}
	if rec := res.CheckIn.Record; rec == nil || rec.ReasonKind != CheckGoalConflict || rec.Moment != CheckInSession ||
		rec.Session != res.SessionID || rec.ParentSession != res.SessionID {
		t.Fatalf("record = %+v, want the session-moment record riding the result", res.CheckIn.Record)
	}
	if chat.calls != 1 {
		t.Fatalf("chat calls = %d, want the one conversation", chat.calls)
	}
	if len(hub.comments) != 0 {
		t.Fatalf("comments = %q, want nothing invented for the human", hub.comments)
	}
}

// The live check-in opens with the policy's 20 calls, and its continue
// carries the session on to its push.
func TestRunSessionLiveCheckinOpensWithThePolicysCalls(t *testing.T) {
	runner, _, _, _, chat, _, _ := liveRig(t, CheckinResponse{Text: `{"decision":"continue"}`})

	logs := commands(0, 24) + pushedLine("c2") + "\n" + resultLine(true, "pushed")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed || res.Commit != "c2" {
		t.Fatalf("result = (%+v, %v), want the session run on to its push", res, err)
	}
	if chat.calls != 1 {
		t.Fatalf("chat calls = %d, want the trip's one conversation", chat.calls)
	}
	if !strings.Contains(chat.gots[0].Input, "calls left 20,") {
		t.Fatalf("statement = %q, want the policy's 20 calls left", chat.gots[0].Input)
	}
}

// The check-in's provider refusing every call is not a stop: the session
// continues on the refusal's deterministic continue, the timeline and the
// record say why, and the next trip asks again.
func TestRunSessionCheckinRefusedContinues(t *testing.T) {
	runner, _, _, _, chat, _, objects := liveRig(t, CheckinResponse{Text: `{"decision":"continue","reason":"the loop is real"}`})
	chat.fails = 1

	logs := commands(0, 24) + commands(24, 24) + pushedLine("c2") + "\n" + resultLine(true, "pushed")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed || res.Commit != "c2" {
		t.Fatalf("result = (%+v, %v), want the session carried through the refusal to the push", res, err)
	}
	if chat.calls != 2 {
		t.Fatalf("chat calls = %d, want the refused conversation and the retried one", chat.calls)
	}
	tl := string(objGet(objects, "sessions/t2/timeline.jsonl"))
	if n := strings.Count(tl, `"step":"checkin"`); n != 2 || !strings.Contains(tl, `"reason":"check-in calls refused`) {
		t.Fatalf("timeline has %d check-in steps, want the refusal's and the retry's:\n%s", n, tl)
	}
	var refused map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(objGet(objects, "sessions/t2/"+res.SessionID+".jsonl"))), "\n") {
		var ev harness.Event
		if json.Unmarshal([]byte(l), &ev) != nil || ev.Kind != harness.EventSupervisor {
			continue
		}
		if rec, _ := ev.Detail["record"].(map[string]any); rec != nil {
			if calls, ok := rec["calls"].([]any); ok && len(calls) > 0 && calls[0].(map[string]any)["result_bytes"].(float64) == 0 {
				refused = ev.Detail
			}
		}
	}
	if refused == nil {
		t.Fatal("no event carries the refused conversation's record with its zero answer")
	}
}

// No model under the check-in's purpose: the trip is judged in code as a
// continue — logged, alerted, but without a conversation, a record or a
// step.
func TestRunSessionCheckinNoAliasContinues(t *testing.T) {
	runner, _, _, _, chat, store, objects := liveRig(t, CheckinResponse{Text: `{"decision":"continue"}`})
	noAlias := testPolicyVersion()
	cl := noAlias.Document.Classes["coder"]
	cl.ReviewModel = ""
	noAlias.Document.Classes["coder"] = cl
	runner.cfg.Policy = func() policy.Version { return noAlias }

	logs := commands(0, 24) + pushedLine("c2") + "\n" + resultLine(true, "pushed")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed || res.Commit != "c2" {
		t.Fatalf("result = (%+v, %v), want the session carried on without a check-in", res, err)
	}
	if chat.calls != 0 {
		t.Fatalf("chat calls = %d, want none", chat.calls)
	}
	if tl := objGet(objects, "sessions/t2/timeline.jsonl"); strings.Contains(string(tl), `"step":"checkin"`) {
		t.Fatalf("timeline = %s, want no check-in step without a conversation", tl)
	}
	var sup map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(objGet(objects, "sessions/t2/"+res.SessionID+".jsonl"))), "\n") {
		var ev harness.Event
		if json.Unmarshal([]byte(l), &ev) == nil && ev.Kind == harness.EventSupervisor {
			sup = ev.Detail
		}
	}
	if sup == nil || sup["decision"] != "continue" || sup["reason"] != "no model under the check-in's purpose" ||
		sup["called"] != false || sup["record"] != nil {
		t.Fatalf("supervisor event = %v, want the deterministic continue without a record", sup)
	}
	a := store.alertRows()[0]
	if a.ResolvedBy != "checkin" || a.Detail["diagnosis"] != "no model under the check-in's purpose" {
		t.Fatalf("alert = %+v, want the trip kept with the reason", a)
	}
}

// The alias a factoryd model call uses comes from the version in force,
// never the task's pinned one: the gateway's purpose check enforces the
// version in force, and a task pinned before an alias landed would have
// every call refused for its life (the #937 probe's defect).
func TestCheckinAliasFollowsTheVersionInForce(t *testing.T) {
	runner, _, _, _, chat, _, _ := liveRig(t, CheckinResponse{Text: `{"decision":"continue","reason":"on track"}`})
	pinned := testPolicyVersion() // coder: no checkin_model, so the pin's fallback is big
	runner.cfg.PolicyVersion = func(context.Context, int64) (policy.Version, bool, error) { return pinned, true, nil }
	inForce := testPolicyVersion()
	cl := inForce.Document.Classes["coder"]
	cl.CheckinModel = "checkin"
	inForce.Document.Classes["coder"] = cl
	runner.cfg.Policy = func() policy.Version { return inForce }

	logs := commands(0, 24) + pushedLine("c1") + "\n" + resultLine(true, "pushed")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m", PolicyVersion: 7}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed || res.Commit != "c1" {
		t.Fatalf("result = (%+v, %v), want the push through the conversation", res, err)
	}
	if chat.calls != 1 || chat.aliases[0] != "checkin" {
		t.Fatalf("chat = %d calls on %v, want one on the version in force's checkin alias", chat.calls, chat.aliases)
	}
}

// The inverse skew: the version in force names no model under the purpose
// while the pin still does — no call leaves factoryd, because the gateway
// would refuse any alias the pin names.
func TestCheckinAliasNoneInForceMeansNoCall(t *testing.T) {
	runner, _, _, _, chat, _, objects := liveRig(t, CheckinResponse{Text: `{"decision":"continue"}`})
	pinned := testPolicyVersion()
	cl := pinned.Document.Classes["coder"]
	cl.CheckinModel = "checkin"
	pinned.Document.Classes["coder"] = cl
	runner.cfg.PolicyVersion = func(context.Context, int64) (policy.Version, bool, error) { return pinned, true, nil }
	inForce := testPolicyVersion()
	cl = inForce.Document.Classes["coder"]
	cl.ReviewModel = ""
	inForce.Document.Classes["coder"] = cl
	runner.cfg.Policy = func() policy.Version { return inForce }

	logs := commands(0, 24) + pushedLine("c2") + "\n" + resultLine(true, "pushed")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m", PolicyVersion: 7}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed || res.Commit != "c2" {
		t.Fatalf("result = (%+v, %v), want the session carried on without a call", res, err)
	}
	if chat.calls != 0 {
		t.Fatalf("chat calls = %d, want none: the version in force names no alias", chat.calls)
	}
	if tl := objGet(objects, "sessions/t2/timeline.jsonl"); strings.Contains(string(tl), `"step":"checkin"`) {
		t.Fatalf("timeline = %s, want no check-in step without a call", tl)
	}
}

// A check-in still answering when the wall-clock cap arrives loses to the
// cap: the session ends wall_clock, the cut conversation's record still
// landed, and the comment says the cap superseded the check-in.
func TestRunSessionCheckinAnswerSupersededByCap(t *testing.T) {
	runner, hub, _, _, chat, _, objects := liveRig(t, CheckinResponse{Text: `{"decision":"continue","reason":"never read"}`})
	setLimits(runner, func(l *policy.Limits) { l.AgentTime = time.Second })
	chat.delay = time.Minute

	began := time.Now()
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, commands(0, 24))
	if err != nil || res.Outcome != OutcomeWallClock {
		t.Fatalf("result = (%+v, %v), want wall_clock", res, err)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Fatalf("took %s: the check-in outlived the session", took)
	}
	tl := string(objGet(objects, "sessions/t2/timeline.jsonl"))
	if !strings.Contains(tl, `"step":"checkin"`) || !strings.Contains(tl, `"reason":"check-in calls refused`) {
		t.Fatalf("timeline = %s, want the cut conversation's record kept", tl)
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "**continue**") ||
		!strings.Contains(hub.comments[0], "Superseded: the task's wall-clock cap was reached during the check-in.") {
		t.Fatalf("comments = %q, want the answer named and superseded", hub.comments)
	}
	if res.Trip != string(TripWallClock) {
		t.Fatalf("trip = %q, want the cap named", res.Trip)
	}
}

// The check-in's run tool is refused inside a live session: the refusal is
// the conversation's tool error, and no /tool post leaves factoryd.
func TestRunSessionLiveToolsRefuseRunInConversation(t *testing.T) {
	runner, _, _, worker, chat, _, _ := liveRig(t,
		CheckinResponse{ToolName: "run", ToolArgs: map[string]any{"command": "go test ./..."}},
		CheckinResponse{Text: `{"decision":"continue","reason":"read only"}`})

	logs := commands(0, 24) + pushedLine("c3") + "\n" + resultLine(true, "pushed")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed {
		t.Fatalf("result = (%+v, %v), want the conversation carried past the refusal", res, err)
	}
	if chat.calls != 2 || !strings.Contains(chat.gots[1].Input, "run is not available to the check-in inside a live session; read instead") {
		t.Fatalf("chat = %d calls, second statement %q, want the refusal fed back as the tool error", chat.calls, chat.gots[1].Input)
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if len(worker.tools) != 0 {
		t.Fatalf("worker tools = %q, want none: the check-in never posts to the pod", worker.tools)
	}
}

// A repetition trip inside the live session asks the check-in, which says
// continue: the session runs on, and the decision lands in the session
// record, the timeline, the issue comment and the alert.
func TestRunSessionTripRunsCheckinAndContinues(t *testing.T) {
	runner, hub, kube, _, chat, store, objects := liveRig(t, CheckinResponse{Text: `{"decision":"continue","reason":"the reads differ each time"}`})

	logs := commands(0, 24) + pushedLine("c1") + "\n" + resultLine(true, "pushed")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomePushed || res.Commit != "c1" || res.SessionID == "" {
		t.Fatalf("result = %+v, want the push through", res)
	}
	if chat.calls != 1 || !slices.Equal(chat.aliases, []string{"big"}) {
		t.Fatalf("chat = %d calls on %v, want one on the check-in's alias", chat.calls, chat.aliases)
	}
	in := chat.gots[0].Input
	for _, want := range []string{"the detector repetition tripped", "24 tool calls and commands without a file change", "<<<SESSION-TAIL>>>"} {
		if !strings.Contains(in, want) {
			t.Fatalf("check-in statement misses %q:\n%s", want, in)
		}
	}
	step := timelineStep(t, objects, "t2", "checkin")
	if step["decision"] != "continue" || step["moment"] != "session" || step["parent_session"] != res.SessionID {
		t.Fatalf("check-in step = %v, want the decision with the session moment and the session's own parentage", step)
	}
	var sup map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(objGet(objects, "sessions/t2/"+res.SessionID+".jsonl"))), "\n") {
		var ev harness.Event
		if json.Unmarshal([]byte(l), &ev) == nil && ev.Kind == harness.EventSupervisor {
			sup = ev.Detail
		}
	}
	if sup == nil || sup["detector"] != "repetition" || sup["decision"] != "continue" ||
		!strings.Contains(fmt.Sprint(sup["digest"]), "24 commands (0 failed)") || sup["record"] == nil {
		t.Fatalf("supervisor event = %v, want the detector, the decision, the digest and the record", sup)
	}
	if len(kube.deletedPods) != 1 {
		t.Fatalf("deleted pods = %v, want the final cleanup only", kube.deletedPods)
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "**continue**") {
		t.Fatalf("comments = %q, want the decision commented", hub.comments)
	}
	a := store.alertRows()[0]
	if a.Kind != AlertSupervisorTrip || a.Source != "repetition" || a.Verdict != VerdictNoise || a.ResolvedBy != "checkin" ||
		a.Detail["action"] != "continue" || a.Detail["session"] != res.SessionID || a.Detail["diagnosis"] != "the reads differ each time" {
		t.Fatalf("alert = %+v", a)
	}
}

// A check-in still thinking when the wall-clock cap arrives is cut off:
// the cap ends the session, and its answer is commented with the note.
func TestRunSessionCapCutsOffASlowCheckin(t *testing.T) {
	runner, hub, _, _, chat, _, _ := liveRig(t, CheckinResponse{Text: `{"decision":"continue"}`})
	setLimits(runner, func(l *policy.Limits) { l.AgentTime = time.Second })
	chat.delay = time.Minute

	began := time.Now()
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, loopFailure(1)+loopFailure(2)+loopFailure(3))
	if err != nil || res.Outcome != OutcomeWallClock {
		t.Fatalf("result = (%+v, %v), want wall_clock", res, err)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Fatalf("took %s: the check-in outlived the session", took)
	}
	if chat.calls != 1 {
		t.Fatalf("chat calls = %d, want the cut conversation", chat.calls)
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "Superseded: the task's wall-clock cap was reached during the check-in") {
		t.Fatalf("comments = %q, want the decision commented with the superseded note", hub.comments)
	}
}

// SessionAnswered writes the session_started record of a session that got a
// model answer, and none for one that did not.
func TestSessionAnsweredRecordsTheStartedSession(t *testing.T) {
	objects := &fakeObjectStore{}
	runner, _, store := newTestRunner(t, supervisedTask("t2"), newFakeKube(), objects)
	in := SessionAnswerInput{TaskID: "t2", SessionID: "s1", Model: "factory/small", Attempt: 2, Since: time.Now().Add(-time.Minute)}

	runner.record(t.Context(), "t2", "session_start", map[string]any{"session": "s1", "attempt": 2})
	if ok, err := runner.SessionAnswered(t.Context(), in); err != nil || ok {
		t.Fatalf("starved session = (%v, %v), want false", ok, err)
	}
	tl, _ := objects.Get(t.Context(), "sessions/t2/timeline.jsonl")
	if !strings.Contains(string(tl), `"step":"session_start"`) || strings.Contains(string(tl), `"event":"session_started"`) {
		t.Fatalf("timeline = %s, want the pod's session_start and no session_started for a starved session", tl)
	}
	store.answered = map[string]bool{"t2": true}
	if ok, err := runner.SessionAnswered(t.Context(), in); err != nil || !ok {
		t.Fatalf("answered session = (%v, %v), want true", ok, err)
	}
	tl, _ = objects.Get(t.Context(), "sessions/t2/timeline.jsonl")
	for _, want := range []string{`"event":"session_started"`, `"session":"s1"`, `"model":"small"`, `"attempt":2`} {
		if !strings.Contains(string(tl), want) {
			t.Errorf("timeline lacks %s:\n%s", want, tl)
		}
	}
	// a session whose activity failed has no id: it counts, and no record
	// names a session ""
	in.SessionID = ""
	if ok, err := runner.SessionAnswered(t.Context(), in); err != nil || !ok {
		t.Fatalf("answered session without an id = (%v, %v), want true", ok, err)
	}
	if tl, _ = objects.Get(t.Context(), "sessions/t2/timeline.jsonl"); strings.Count(string(tl), `"event":"session_started"`) != 1 {
		t.Fatalf("timeline = %s, want the one session_started of s1", tl)
	}
}

// What a session that ended without a push did does not reach its result as
// lines: its commands are in its record, and the next session gets the stop
// reason, the branch and the agent's last message instead.
func TestRunSessionResultCarriesNoCommandLines(t *testing.T) {
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
	logs := envLine(harness.Event{Kind: harness.EventCommand, Time: time.Now(), Detail: map[string]any{"command": "go test ./...", "exit": 1}}) + "\n" + resultLine(false, "gave up")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Findings, []string{"previous session ended without a pushed commit: gave up"}) {
		t.Fatalf("findings = %q, want the stop line alone", res.Findings)
	}
}

func TestSessionResultCarriesTheBranchAndTheAgentsLastMessage(t *testing.T) {
	res := sessionResult(sessionSpec{id: "s1"}, sessionOutcome{noResult: "the worker exited without a result", commit: "c-wip", agentText: "I ran out of budget"})
	if res.Commit != "c-wip" || res.LastMessage != "I ran out of budget" ||
		!slices.Equal(res.Findings, []string{"previous session ended unfinished: the worker exited without a result"}) {
		t.Fatalf("result = %+v, want the branch's commit, the last message and the stop line", res)
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
	task := Task{ID: "t3", Status: "running", Objective: "break", Repository: "sf/repo", Track: "direct"}
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
	task := Task{ID: "t3", Status: "running", Objective: "noop", Repository: "sf/repo", Track: "direct"}
	runner, _, _ := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t3", Attempt: 1, Model: "m"}, nil, resultLine(true, "no changes to push"))
	if err != nil || res.Outcome != OutcomeNoPush || res.Summary != "no changes to push" {
		t.Fatalf("result = (%+v, %v), want no_push", res, err)
	}
}

func TestRunSessionPodDeathIsNoPush(t *testing.T) {
	task := Task{ID: "t4", Status: "running", Objective: "flaky", Repository: "sf/repo", Track: "direct"}
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
	task := Task{ID: "t4e", Status: "running", Objective: "flaky", Repository: "sf/repo", Track: "direct"}
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

// imageNotFound is containerd's answer, through the kubelet, for a
// worker image whose manifest the registry does not hold.
const imageNotFound = `rpc error: code = NotFound desc = failed to pull and unpack image "reg/worker@sha256:abc": failed to resolve reference "reg/worker@sha256:abc": reg/worker@sha256:abc: not found`

// startEscalation is a start failure's escalation reason, by hand: the
// step on the question row with its action, the failure below.
func startEscalation(step, act, summary string) string {
	return "the sandbox cannot start at " + step + ", and no session can fix it: " + act +
		", then answer to start a session again, or close the card.\n" + summary
}

// A session that cannot start goes to a human at once, with the step on
// the card's question row and the failure below it, whether its pod's
// clone init container or worker container waits on an image the
// registry does not hold (not ten minutes later) or the worker's own
// start fails at a step and says so in its result. No agent ran: the
// session hands on no findings of its own, and the timeline's
// session_end says what failed.
func TestRunSessionThatCannotStartEscalatesAtOnce(t *testing.T) {
	const pushTheImage = "push the worker image or correct its reference"
	const openCanary = "start failed: egress probe: canary example.com:443 answered: connected to 192.0.2.7:443"
	cloneErr := "start failed: clone: git clone -q http://forgejo/sf/repo.git /workspace/repo: exit status 128: fatal: repository 'http://forgejo/sf/repo.git/' not found"
	for _, tc := range []struct {
		name          string
		containers    []ContainerState
		log           string
		summary, step string
		act           string
	}{
		{name: "clone container", containers: []ContainerState{{Name: "clone", State: "waiting", Reason: "ErrImagePull", Message: imageNotFound},
			{Name: "worker", State: "waiting", Reason: "PodInitializing"}},
			summary: "start failed: image pull: container clone waits with ErrImagePull: " + imageNotFound, step: "image pull", act: pushTheImage},
		{name: "worker container", containers: []ContainerState{{Name: "clone", State: "terminated", Reason: "Completed"},
			{Name: "worker", State: "waiting", Reason: "ErrImagePull", Message: imageNotFound}},
			summary: "start failed: image pull: container worker waits with ErrImagePull: " + imageNotFound, step: "image pull", act: pushTheImage},
		{name: "worker start", log: workerStartFailed("clone", cloneErr),
			summary: cloneErr, step: "clone", act: "fix the cause below"},
		{name: "egress probe", log: workerStartFailed("egress probe", openCanary),
			summary: openCanary, step: "egress probe", act: "check the task namespace's worker-egress policy, or the pod's uid, against the probe named below"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := Task{ID: "t4i", Status: "running", Objective: "fix vet", Repository: "sf/repo", Track: "direct"}
			kube := newFakeKube()
			if tc.containers != nil {
				kube.onCreatePod = func() {
					p := kube.podOrder[len(kube.podOrder)-1]
					p.phase, p.containers = "Pending", tc.containers
				}
			}
			objects := &fakeObjectStore{}
			runner, _, _ := newTestRunner(t, task, kube, objects)
			// a start that waits ends in the deadline's error, not ten minutes on
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var bodies []string
			if tc.log != "" {
				bodies = []string{tc.log}
			}
			res, err := runSession(t, ctx, runner, SessionInput{TaskID: "t4i", Attempt: 1, Model: "factory/small", Findings: []string{"go vet: unreachable code"}}, nil, bodies...)
			reason := startEscalation(tc.step, tc.act, tc.summary)
			if err != nil || res.Outcome != OutcomeEscalate || res.Summary != tc.summary || res.Reason != reason || !res.StartFailed {
				t.Fatalf("result = (%+v, %v), want an escalation:\n%s", res, err, reason)
			}
			if len(res.Findings) != 0 || res.Model != "factory/small" || res.Trip != "" {
				t.Fatalf("findings %q, trip %q on %s, want none of its own on its model", res.Findings, res.Trip, res.Model)
			}
			if got := escalationSource(res.Reason); got != "start" {
				t.Fatalf("escalation source = %q, want start", got)
			}
			if end := timelineStep(t, objects, "t4i", "session_end"); end["outcome"] != string(OutcomeEscalate) || end["summary"] != tc.summary {
				t.Fatalf("session_end = %v, want outcome escalate with the summary", end)
			}
		})
	}
}

// workerStartFailed is the worker's result line for a start that failed
// at step with summary.
func workerStartFailed(step, summary string) string {
	b, _ := json.Marshal(harness.WorkerEnvelope{Type: "result", Result: &harness.Result{Summary: summary, Step: step}})
	return string(b)
}

// A worker start that failed on a fault a later start may not meet, the
// gateway restarting under its egress probe or Forgejo answering the clone
// with a 5xx, is no escalation: the session ends aborted with the worker's
// summary, and the activity's error is retryable, so Temporal starts the
// session again.
func TestRunSessionRetriesATransientStartFailure(t *testing.T) {
	for step, summary := range map[string]string{
		StepEgressProbe: "start failed: egress probe: gateway: http://factory-gateway.factory.svc.cluster.local:8081/healthz answered 503 Service Unavailable",
		"clone": "start failed: clone: git clone -q http://forgejo/sf/repo.git /workspace/repo: exit status 128: " +
			"fatal: unable to access 'http://forgejo/sf/repo.git/': The requested URL returned error: 502",
	} {
		t.Run(step, func(t *testing.T) {
			task := Task{ID: "t4j", Status: "running", Objective: "fix vet", Repository: "sf/repo", Track: "direct"}
			objects := &fakeObjectStore{}
			runner, _, _ := newTestRunner(t, task, newFakeKube(), objects)
			line, _ := json.Marshal(harness.WorkerEnvelope{Type: "result", Result: &harness.Result{Summary: summary, Step: step, Transient: true}})
			res, err := runSession(t, nil, runner, SessionInput{TaskID: "t4j", Attempt: 1, Model: "factory/small"}, nil, string(line))
			if err == nil || !strings.Contains(err.Error(), summary) || isTerminal(err) || res.StartFailed || res.Reason != "" {
				t.Fatalf("result = (%+v, %v), want a retryable error with the worker's summary", res, err)
			}
			if end := timelineStep(t, objects, "t4j", "session_end"); end["outcome"] != "aborted" || end["summary"] != summary {
				t.Fatalf("session_end = %v, want outcome aborted with the summary", end)
			}
		})
	}
}

// startRunner is a runner on kube that reads a pod's start every 10 ms
// and lets a container wait with a configuration error for grace.
func startRunner(kube *fakeKube, grace time.Duration) *Runner {
	return &Runner{kube: kube, startPoll: 10 * time.Millisecond, configGrace: grace}
}

// awaitPodStart ends a pod's start at the first wait no wait gets past,
// in its init or its main container, and waits on through every other:
// a registry that answers 503, a back-off that names no error (a kubelet
// before 1.30), a container still being created, a configuration error
// first seen, a pod no node takes for cpu, memory or disk. A closed stop
// ends the first wait at once, so errWorkerStop says the start waited.
func TestAwaitPodStartEndsOnlyAtAWaitNoWaitPasses(t *testing.T) {
	const unavailable = `rpc error: code = Unknown desc = failed to pull and unpack image "reg/worker@sha256:abc": failed to resolve reference "reg/worker@sha256:abc": ` +
		`unexpected status from HEAD request to https://reg/v2/worker/manifests/sha256:abc: 503 Service Unavailable`
	for _, tc := range []struct {
		name       string
		containers []ContainerState
		conditions []PodCondition
		want       string // the start failure; "" waits on
	}{
		{name: "init ErrImagePull, manifest not found",
			containers: []ContainerState{{Name: "clone", State: "waiting", Reason: "ErrImagePull", Message: imageNotFound}, {Name: "worker", State: "waiting", Reason: "PodInitializing"}},
			want:       "start failed: image pull: container clone waits with ErrImagePull: " + imageNotFound},
		{name: "main ImagePullBackOff after a pull not found",
			containers: []ContainerState{{Name: "clone", State: "terminated", Reason: "Completed"},
				{Name: "worker", State: "waiting", Reason: "ImagePullBackOff", Message: `Back-off pulling image "reg/worker@sha256:abc": ErrImagePull: ` + imageNotFound}},
			want: `start failed: image pull: container worker waits with ImagePullBackOff: Back-off pulling image "reg/worker@sha256:abc": ErrImagePull: ` + imageNotFound},
		{name: "manifest unknown",
			containers: []ContainerState{{Name: "clone", State: "waiting", Reason: "ErrImagePull", Message: "reading manifest abc in reg/worker: manifest unknown"}},
			want:       "start failed: image pull: container clone waits with ErrImagePull: reading manifest abc in reg/worker: manifest unknown"},
		{name: "invalid image name",
			containers: []ContainerState{{Name: "clone", State: "waiting", Reason: "InvalidImageName", Message: `Failed to apply default image tag "reg/Worker": couldn't parse image name "reg/Worker": invalid reference format: repository name must be lowercase`}},
			want:       `start failed: image pull: container clone waits with InvalidImageName: Failed to apply default image tag "reg/Worker": couldn't parse image name "reg/Worker": invalid reference format: repository name must be lowercase`},
		{name: "container config, first seen",
			containers: []ContainerState{{Name: "clone", State: "terminated", Reason: "Completed"}, {Name: "worker", State: "waiting", Reason: "CreateContainerConfigError", Message: `secret "task-git" not found`}}},
		{name: "registry 503",
			containers: []ContainerState{{Name: "clone", State: "waiting", Reason: "ImagePullBackOff", Message: `Back-off pulling image "reg/worker@sha256:abc": ErrImagePull: ` + unavailable}}},
		{name: "back-off without the pull's error",
			containers: []ContainerState{{Name: "clone", State: "waiting", Reason: "ImagePullBackOff", Message: `Back-off pulling image "reg/worker@sha256:abc"`}}},
		{name: "container creating",
			containers: []ContainerState{{Name: "clone", State: "terminated", Reason: "Completed"}, {Name: "worker", State: "waiting", Reason: "ContainerCreating"}}},
		{name: "unschedulable",
			conditions: []PodCondition{{Type: "PodScheduled", Status: "False", Reason: "Unschedulable", Message: "0/3 nodes are available: 3 Insufficient memory."}}},
		// the ephemeral-storage its emptyDirs request (requestEmptyDirs)
		{name: "unschedulable for disk",
			conditions: []PodCondition{{Type: "PodScheduled", Status: "False", Reason: "Unschedulable", Message: "0/3 nodes are available: 1 Insufficient cpu, 3 Insufficient ephemeral-storage."}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kube := newFakeKube()
			kube.pods["ns/worker-s1"] = &fakePod{ns: "ns", name: "worker-s1", phase: "Pending", containers: tc.containers, conditions: tc.conditions}
			stop := make(chan struct{})
			close(stop)
			err := startRunner(kube, time.Hour).awaitPodStart(context.Background(), "ns", "worker-s1", time.Minute, stop)
			var f *startFailure
			switch {
			case tc.want == "" && !errors.Is(err, errWorkerStop):
				t.Fatalf("awaitPodStart = %v, want it to wait on", err)
			case tc.want != "" && (!errors.As(err, &f) || err.Error() != tc.want):
				t.Fatalf("awaitPodStart = %v, want the start failure %q", err, tc.want)
			}
		})
	}
}

// A container that waits with a configuration error ends the start only
// once it has waited with it for the grace: the kubelet's view of a
// secret written just before the pod can lag, and one that clears within
// the grace starts the pod. A wait without the error starts the count
// again. The worker's state follows the time since the start, read every
// 10 ms against a grace of 600 ms.
func TestAwaitPodStartGivesAConfigErrorItsGrace(t *testing.T) {
	configErr := ContainerState{Name: "worker", State: "waiting", Reason: "CreateContainerConfigError", Message: `secret "task-git" not found`}
	creating := ContainerState{Name: "worker", State: "waiting", Reason: "ContainerCreating"}
	running := ContainerState{Name: "worker", State: "running"}
	type span struct {
		until time.Duration // the state holds until then, the last one for good
		state ContainerState
	}
	for _, tc := range []struct {
		name  string
		spans []span
		want  string // "" starts
	}{
		{name: "clears within the grace", spans: []span{{300 * time.Millisecond, configErr}, {0, running}}},
		// 700 ms of the error in all, never 600 ms in a row
		{name: "cleared, then again", spans: []span{{300 * time.Millisecond, configErr}, {500 * time.Millisecond, creating},
			{900 * time.Millisecond, configErr}, {0, running}}},
		{name: "persists", spans: []span{{0, configErr}},
			want: `start failed: container config: container worker waits with CreateContainerConfigError: secret "task-git" not found`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kube := newFakeKube()
			pod := &fakePod{ns: "ns", name: "worker-s1", phase: "Pending"}
			kube.pods["ns/worker-s1"] = pod
			began := time.Now()
			kube.onDiagnosis = func() {
				i := 0
				for i < len(tc.spans)-1 && time.Since(began) >= tc.spans[i].until {
					i++
				}
				pod.containers = []ContainerState{{Name: "clone", State: "terminated", Reason: "Completed"}, tc.spans[i].state}
				if tc.spans[i].state.State == "running" {
					pod.phase = "Running"
				}
			}
			err := startRunner(kube, 600*time.Millisecond).awaitPodStart(context.Background(), "ns", "worker-s1", time.Minute, nil)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("awaitPodStart = %v after %v, want the pod started", err, time.Since(began))
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Fatalf("awaitPodStart = %v, want %q", err, tc.want)
			}
		})
	}
}

// A start that times out says what the pod waits on: its first container
// not done, init containers first, with the kubelet's reason and message,
// or the scheduler's refusal.
func TestAwaitPodStartTimeoutSaysWhatThePodWaitsOn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		containers []ContainerState
		conditions []PodCondition
		want       string
	}{
		{name: "pull back-off", containers: []ContainerState{{Name: "clone", State: "waiting", Reason: "ImagePullBackOff", Message: `Back-off pulling image "reg/worker@sha256:abc"`},
			{Name: "worker", State: "waiting", Reason: "PodInitializing"}},
			want: `pod worker-s1 still pending after 30ms: container clone waits with ImagePullBackOff: Back-off pulling image "reg/worker@sha256:abc"`},
		{name: "clone runs", containers: []ContainerState{{Name: "clone", State: "running"}, {Name: "worker", State: "waiting", Reason: "PodInitializing"}},
			want: "pod worker-s1 still pending after 30ms: container clone runs"},
		{name: "unschedulable", conditions: []PodCondition{{Type: "PodScheduled", Status: "False", Reason: "Unschedulable", Message: "0/3 nodes are available: 3 Insufficient memory."}},
			want: "pod worker-s1 still pending after 30ms: Unschedulable: 0/3 nodes are available: 3 Insufficient memory."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kube := newFakeKube()
			kube.pods["ns/worker-s1"] = &fakePod{ns: "ns", name: "worker-s1", phase: "Pending", containers: tc.containers, conditions: tc.conditions}
			err := startRunner(kube, time.Hour).awaitPodStart(context.Background(), "ns", "worker-s1", 30*time.Millisecond, nil)
			var p *podPending
			if !errors.As(err, &p) || err.Error() != tc.want {
				t.Fatalf("awaitPodStart = %v, want the pending pod %q", err, tc.want)
			}
		})
	}
}

// A session whose pod no node takes ends not_started when its start's
// wait runs out, here the 300 ms of agent time the pinned version leaves
// it, naming the scheduler's refusal; so does a report session, whose
// report task then reads NotStarted instead of failing.
func TestSessionsWhosePodNoNodeTakesEndNotStarted(t *testing.T) {
	const refusal = "Unschedulable: 0/3 nodes are available: 3 Insufficient ephemeral-storage."
	unschedulable := func(kube *fakeKube) {
		kube.onCreatePod = func() {
			p := kube.podOrder[len(kube.podOrder)-1]
			p.phase, p.conditions = "Pending", []PodCondition{{Type: "PodScheduled", Status: "False", Reason: "Unschedulable", Message: "0/3 nodes are available: 3 Insufficient ephemeral-storage."}}
		}
	}
	short := func(r *Runner) {
		v := testPolicyVersion()
		v.Document.Limits.AgentTime = 300 * time.Millisecond
		r.cfg.Policy = func() policy.Version { return v }
		r.startPoll = 20 * time.Millisecond
	}

	kube := newFakeKube()
	unschedulable(kube)
	runner, _, _ := newTestRunner(t, Task{ID: "t6", Status: "running", Objective: "o", Repository: "sf/repo", Track: "direct"}, kube, &fakeObjectStore{})
	short(runner)
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t6", Attempt: 1, Model: "m"}, nil)
	if err != nil || res.Outcome != OutcomeNotStarted || !strings.HasPrefix(res.Summary, "pod worker-") || !strings.HasSuffix(res.Summary, ": "+refusal) {
		t.Fatalf("result = (%+v, %v), want not_started naming %q", res, err, refusal)
	}

	report := Task{ID: "t1", Status: "running", Objective: "o", Repository: "sf/r", SHA: "abc123", Process: "maintenance",
		Parent: "t-parent", Source: SourceScreening, Track: policy.TrackDirect, RequiredCapabilities: []string{policy.ClassAnalyzer}, BudgetEUR: 2}
	kube = newFakeKube()
	unschedulable(kube)
	runner, hub, store := newTestRunner(t, report, kube, &fakeObjectStore{})
	store.CreateTask(context.Background(), parentTask())
	short(runner)
	val, err := execActivity(t, runner, ActivityRunReport, "t1", "factory/small")
	var rep ReportResult
	if err == nil {
		err = val.Get(&rep)
	}
	if err != nil || !strings.HasSuffix(rep.NotStarted, ": "+refusal) || len(hub.comments) != 0 {
		t.Fatalf("RunReport = %+v, %v; want NotStarted naming %q, no comment", rep, err, refusal)
	}
}

// A session whose pod never left Pending ends not_started (versionPodWait),
// with the line and the finding a no-push session's would carry; a start
// failure stays an escalation.
func TestSessionResultOfAPodThatNeverStarted(t *testing.T) {
	pending := &podPending{pod: "worker-s1", after: 10 * time.Minute, on: "Unschedulable: 0/3 nodes are available: 3 Insufficient ephemeral-storage."}
	res := sessionResult(sessionSpec{id: "s1"}, sessionOutcome{noResult: pending.Error(), pending: pending})
	why := "pod worker-s1 still pending after 10m0s: Unschedulable: 0/3 nodes are available: 3 Insufficient ephemeral-storage."
	if res.Outcome != OutcomeNotStarted || res.Summary != why || !slices.Equal(res.Findings, []string{"previous session ended without a pushed commit: " + why}) || res.StartFailed {
		t.Fatalf("result %+v, want not_started with %q", res, why)
	}
	failed := &startFailure{step: stepImagePull, text: "start failed: image pull: not found"}
	if res := sessionResult(sessionSpec{id: "s1"}, sessionOutcome{noResult: failed.Error(), start: failed}); res.Outcome != OutcomeEscalate || !res.StartFailed {
		t.Fatalf("result %+v, want the start failure's escalation", res)
	}
}

// A pod that ends without a result says how, from what the kubelet
// reports: the first container that ended in failure, init containers
// first, with its reason and exit code, instead of the generic line.
func TestRunSessionNamesHowThePodEndedWithoutAResult(t *testing.T) {
	for _, tc := range []struct {
		name       string
		containers []ContainerState
		want       string
	}{
		{name: "worker OOM", containers: []ContainerState{{Name: "clone", State: "terminated", Reason: "Completed"}, {Name: "worker", State: "terminated", Reason: "OOMKilled", ExitCode: 137}},
			want: "pod exited without a result: container worker terminated with OOMKilled (exit code 137)"},
		{name: "clone error", containers: []ContainerState{{Name: "clone", State: "terminated", Reason: "Error", ExitCode: 1, Message: "agent-worker: prctl PR_SET_DUMPABLE: operation not permitted"},
			{Name: "worker", State: "waiting", Reason: "PodInitializing"}},
			want: "pod exited without a result: container clone terminated with Error (exit code 1): agent-worker: prctl PR_SET_DUMPABLE: operation not permitted"},
		{name: "worker exited 0", containers: []ContainerState{{Name: "clone", State: "terminated", Reason: "Completed"}, {Name: "worker", State: "terminated", Reason: "Completed"}},
			want: "pod exited without a result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := Task{ID: "t4o", Status: "running", Objective: "flaky", Repository: "sf/repo", Track: "direct"}
			kube := newFakeKube()
			kube.onCreatePod = func() { kube.podOrder[len(kube.podOrder)-1].containers = tc.containers }
			runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
			res, err := runSession(t, nil, runner, SessionInput{TaskID: "t4o", Attempt: 1, Model: "m"}, nil,
				envLine(harness.Event{Kind: harness.EventModelCall, Time: time.Now()})+"\n")
			if err != nil || res.Outcome != OutcomeNoPush || res.Summary != tc.want {
				t.Fatalf("result = (%+v, %v), want summary %q", res, err, tc.want)
			}
		})
	}
}

// A stopped worker that exits without a result says how, through the
// same reading of the pod as a worker that dies unstopped: the wall-clock
// cap stops the session, and the worker, killed for its memory, reports
// nothing. One that pushed its work in progress first states the stop
// alone: the branch holds the commit.
func TestStoppedWorkerNamesHowItEndedWithoutAResult(t *testing.T) {
	const ended = "the stopped worker exited without a result: container worker terminated with OOMKilled (exit code 137)"
	for name, tc := range map[string]struct {
		after, commit string
		findings      []string
	}{
		"nothing pushed":         {"", "", []string{"work in progress not pushed: " + ended}},
		"pushed, then no report": {pushedLine("c-wip") + "\n", "c-wip", []string{ended}},
	} {
		task := Task{ID: "t5o", Status: "running", Objective: "slow", Repository: "sf/repo", Track: "direct"}
		kube := newFakeKube()
		kube.keepDeleted = true
		kube.onCreatePod = func() {
			kube.podOrder[len(kube.podOrder)-1].containers = []ContainerState{{Name: "clone", State: "terminated", Reason: "Completed"},
				{Name: "worker", State: "terminated", Reason: "OOMKilled", ExitCode: 137}}
		}
		runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
		// the log's follower reads a pod that exited once more, a second on
		runner.stopWait = 5 * time.Second
		runner.cfg.Supervisor = SupervisorConfig{NoProgressAfter: 3 * time.Second, RepetitionEvents: 24}
		setLimits(runner, func(l *policy.Limits) { l.AgentTime = 300 * time.Millisecond })
		res, err := runSession(t, nil, runner, SessionInput{TaskID: "t5o", Attempt: 1, Model: "m"}, nil,
			envLine(harness.Event{Kind: harness.EventModelCall, Time: time.Now()})+"\n"+sigtermLogs+tc.after)
		if err != nil || res.Outcome != OutcomeWallClock || res.Commit != tc.commit || !slices.Equal(res.Findings, tc.findings) {
			t.Fatalf("%s: result = (%+v, %v), want wall_clock at %q with %q", name, res, err, tc.commit, tc.findings)
		}
	}
}

// The supervisor's clocks start when the pod has started: a pull that
// takes longer than the cheap rung's cap leaves the session on the cheap
// rung, with no cheap_cap trip at its first event.
func TestSlowPodStartIsNoTimeOfTheCheapRung(t *testing.T) {
	task := Task{ID: "t4s", Status: "running", Objective: "fix vet", Repository: "sf/repo", Track: "direct"}
	kube := newFakeKube()
	kube.onCreatePod = func() {
		p := kube.podOrder[len(kube.podOrder)-1]
		p.phase = "Pending"
		time.AfterFunc(1500*time.Millisecond, func() {
			kube.mu.Lock()
			defer kube.mu.Unlock()
			p.phase = "Running"
		})
	}
	runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	runner.startPoll, runner.cheapCap = 50*time.Millisecond, time.Second
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t4s", Attempt: 1, Model: "factory/small"}, nil,
		envLine(harness.Event{Kind: harness.EventModelCall, Time: time.Now()})+"\n"+resultLine(true, "no changes to push"))
	if err != nil || res.Outcome != OutcomeNoPush || res.Trip != "" || res.Model != "factory/small" || res.Summary != "no changes to push" {
		t.Fatalf("result = (%+v, %v), want no trip on the cheap rung", res, err)
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
			task := Task{ID: "t5", Status: "running", Objective: "slow", Repository: "sf/repo", Track: "direct"}
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
			task := Task{ID: "t5", Status: "running", Objective: "steps", Repository: "sf/repo", Track: "direct"}
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
	task := Task{ID: "t5", Status: "running", Objective: "slow", Repository: "sf/repo", Track: "direct"}
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
	task := Task{ID: "t6", Status: "running", Objective: "long", Repository: "sf/repo", Track: "direct"}
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

// factoryd stopping detaches from the session (roadmap 222, item 2): the
// worker runs on in its pod, nothing is deleted or revoked, the row keeps
// how much of the log was consumed, and the activity's retryable stop
// carries the session for the next factoryd.
func TestRunSessionWorkerStopDetachesAndKeepsThePod(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, store := newTestRunner(t, supervisedTask("t2"), kube, objects)
	stop := make(chan struct{})
	// the stop lands once the session's pre-conversation work (its brief
	// delivery) is through and the loop has taken the scripted lines
	kube.onCreatePod = func() { go func() { time.Sleep(200 * time.Millisecond); close(stop) }() }
	var last sessionProgress
	var mu sync.Mutex

	logs := loopFailure(1) + cutLogs + pushedLine("c-wip") + "\n" + resultLine(true, "pushed") + "\n"
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
	if len(kube.deletedPods) != 0 || len(kube.deletedNs) != 0 {
		t.Fatalf("deleted pods %v, namespaces %v; want the worker running on", kube.deletedPods, kube.deletedNs)
	}
	if len(hub.revoked) != 0 {
		t.Fatalf("revoked = %v: the session's tokens must outlive the detach", hub.revoked)
	}
	mu.Lock()
	defer mu.Unlock()
	live, ok := store.liveSessions["t2"]
	if !ok || live.SessionID != last.SessionID || live.Pod != "worker-"+last.SessionID || live.Lines != strings.Count(loopFailure(1), "\n") {
		t.Fatalf("live row = %+v (%v), heartbeat %+v; want the session's pod and the lines consumed before the stop", live, ok, last)
	}
	if timeline := string(objGet(objects, "sessions/t2/timeline.jsonl")); !strings.Contains(timeline, `"step":"session_detached"`) || strings.Contains(timeline, `"step":"session_end"`) {
		t.Fatalf("timeline = %s, want the detach and no end", timeline)
	}
}

// The next factoryd's retry reattaches to the live pod: no new pod, no
// namespace or token work, the log read on from the lines consumed, the
// same session record, and the session's push as its result.
func TestRunSessionReattachesToTheLivePod(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, store := newTestRunner(t, supervisedTask("t2"), kube, objects)
	stop := make(chan struct{})
	kube.onCreatePod = func() { go func() { time.Sleep(20 * time.Millisecond); close(stop) }() }
	logs := loopFailure(1) + cutLogs + pushedLine("c-wip") + "\n" + resultLine(true, "pushed") + "\n"
	if _, err := runSessionIn(t, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, []string{logs}, func(env *testsuite.TestActivityEnvironment) {
		env.SetWorkerStopChannel(stop)
	}); err == nil || !strings.Contains(err.Error(), "factoryd stopping") {
		t.Fatalf("err = %v, want the detach", err)
	}
	first := store.liveSessions["t2"]
	calls := len(hub.calls)

	res, err := runSessionIn(t, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, func(env *testsuite.TestActivityEnvironment) {
		env.SetHeartbeatDetails(sessionProgress{SessionID: first.SessionID, Running: true, Events: 1})
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomePushed || res.Commit != "c-wip" || res.SessionID != first.SessionID || !res.Started.Equal(first.StartedAt) {
		t.Fatalf("result = %+v, want the live session's push as its own, started when its pod did (%v)", res, first.StartedAt)
	}
	if len(kube.podOrder) != 1 {
		t.Fatalf("pods created %d, want the one the first factoryd made", len(kube.podOrder))
	}
	if later := hub.calls[calls:]; slices.ContainsFunc(later, func(c string) bool { return strings.HasPrefix(c, "mint ") }) {
		t.Fatalf("forgejo calls after the detach %v, want no new token for a reattach", later)
	}
	if _, ok := store.liveSessions["t2"]; ok {
		t.Fatal("the live row survived the session's end")
	}
	if !slices.Contains(kube.deletedPods, "factory-agents-t2/worker-"+first.SessionID) {
		t.Fatalf("deleted pods = %v, want the pod gone at the end", kube.deletedPods)
	}
	timeline := string(objGet(objects, "sessions/t2/timeline.jsonl"))
	if !strings.Contains(timeline, `"step":"session_reattached"`) || strings.Count(timeline, `"step":"session_start"`) != 1 {
		t.Fatalf("timeline = %s, want one start and a reattach", timeline)
	}
	history := string(objGet(objects, "sessions/t2/"+first.SessionID+".jsonl"))
	if strings.Count(history, "\n") != strings.Count(loopFailure(1), "\n")+1 {
		t.Fatalf("session history:\n%s\nwant the events before the stop and the push, each once", history)
	}
}

// A live row whose pod is gone (a crash took it) starts the attempt over
// as before: a new pod, and the row replaced by the new session's.
func TestRunSessionReattachFindsNoPodAndStartsOver(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "x", Repository: "sf/repo", SHA: "base", Track: "direct"}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, task, kube, &fakeObjectStore{})
	store.liveSessions = map[string]LiveSession{"t1": {TaskID: "t1", SessionID: "s-gone", Namespace: "factory-agents-t1", Pod: "worker-s-gone", MessageToken: "tok", Lines: 4,
		StartedAt: time.Now().Add(-time.Hour)}}
	before := time.Now()
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "m"}, nil, pushedLine("c2")+"\n"+resultLine(true, "pushed"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomePushed || res.SessionID == "s-gone" || len(kube.podOrder) != 1 || res.Started.Before(before) {
		t.Fatalf("result = %+v, pods %d; want a fresh session in a new pod, started after %v", res, len(kube.podOrder), before)
	}
	if _, ok := store.liveSessions["t1"]; ok {
		t.Fatal("a live row survived the fresh session's end")
	}
}

// factoryd stopping while the check-in is still thinking: the conversation
// is cut, its answer is not acted on, and the worker is stopped within the
// grace period, not after the conversation's own timeout.
func TestRunSessionWorkerStopCutsAnInFlightCheckin(t *testing.T) {
	runner, hub, kube, _, chat, _, _ := liveRig(t, CheckinResponse{Text: `{"decision":"continue"}`})
	runner.stopWait = 10 * time.Second
	stop := make(chan struct{})
	var once sync.Once
	// factoryd starts stopping while the conversation is in flight
	chat.onCall = func() { once.Do(func() { close(stop) }) }
	chat.delay = time.Minute
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
		t.Fatalf("err = %v, want the retryable stop, not the cut conversation's answer", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("took %s: the stop waited for the check-in", took)
	}
	if len(hub.comments) != 0 {
		t.Fatalf("comments = %q, want no decision from the cut conversation", hub.comments)
	}
	if len(kube.deletedPods) != 0 {
		t.Fatalf("deleted pods = %v, want the worker running on for the reattach", kube.deletedPods)
	}
	mu.Lock()
	defer mu.Unlock()
	if !last.Running {
		t.Fatalf("last heartbeat = %+v, want the running session for the retry", last)
	}
}

func TestRunSessionSetupFailureDeletesNamespace(t *testing.T) {
	task := Task{ID: "t8", Status: "running", Objective: "x", Repository: "sf/repo", Track: "direct"}
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
	runner, _, store := newTestRunner(t, Task{ID: "trunning", Status: "running", Repository: "sf/repo", Track: "direct"}, kube, &fakeObjectStore{})
	store.CreateTask(context.Background(), Task{ID: "tqueued", Status: "queued", Repository: "sf/repo", Track: "direct"})
	store.CreateTask(context.Background(), Task{ID: "treview", Status: "in-review", Repository: "sf/repo", Track: "direct"})
	store.CreateTask(context.Background(), Task{ID: "tdone", Status: "done", Repository: "sf/repo", Track: "direct"})

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
	runner, _, _ := newTestRunner(t, Task{ID: "t10", Status: "queued", Track: "direct"}, kube, &fakeObjectStore{})

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
	if got := escalationSource(res.Reason); got != "content_filter" {
		t.Fatalf("escalation source = %q, want content_filter", got)
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
	// work in progress on the branch is the result's commit, which
	// handoverLines states with the last message: no report repeats it
	res = sessionResult(s, sessionOutcome{trip: trip, decision: redirect, commit: "c-wip", agentText: "tests still fail",
		result: &harness.Result{Summary: "stopped; work in progress pushed to factory/t1 at c-wip"}})
	if res.Outcome != OutcomeRedirect || res.Commit != "c-wip" || res.LastMessage != "tests still fail" ||
		!slices.Equal(res.Findings, []string{"supervisor guidance: g"}) {
		t.Fatalf("stop with work in progress: %+v, want the commit, the last message and the guidance alone", res)
	}
}

// The stuck rule's line is the first finding, before the stopped worker's
// report.
func TestSessionResultPutsTheStuckLineFirst(t *testing.T) {
	stuck := "stuck after the supervisor's message: same_failure: x"
	trip := &Trip{Kind: TripSameFailure, Detail: "x", Findings: []string{"ran y", stuck}, Stuck: stuck}
	res := sessionResult(sessionSpec{id: "s1"}, sessionOutcome{trip: trip, decision: &Decision{Action: ActionRetryStronger, Reason: stuck},
		result: &harness.Result{Summary: "push failed: refusing to push: .forgejo/x changes .forgejo/"}})
	want := []string{stuck, "the stopped worker reported: push failed: refusing to push: .forgejo/x changes .forgejo/"}
	if res.Outcome != OutcomeRetryStronger || !slices.Equal(res.Findings, want) {
		t.Fatalf("result = %+v, want findings %q", res, want)
	}
}

// The agent's last message reaches the next session cut at
// maxHandoverMessage bytes, marked.
func TestSessionResultCutsTheLastMessage(t *testing.T) {
	res := sessionResult(sessionSpec{id: "s1"}, sessionOutcome{noResult: "x", agentText: strings.Repeat("a", maxHandoverMessage) + "bc"})
	if want := strings.Repeat("a", maxHandoverMessage) + "…"; res.LastMessage != want {
		t.Fatalf("last message of %d bytes, want %d a's and the mark", len(res.LastMessage), maxHandoverMessage)
	}
}

// The log stream breaks while the worker still runs (here during the
// stop's grace period); the log is reopened and the worker's report and
// pushed work in progress are not lost, nor recorded twice.
func TestRunSessionStopSurvivesABrokenLogStream(t *testing.T) {
	runner, _, _, _, _, _, objects := liveRig(t, CheckinResponse{Text: `{"decision":"handover","handover":"stop"}`})
	runner.stopWait = 10 * time.Second

	logs := loopFailure(1) + loopFailure(2) + loopFailure(3) + cutLogs +
		pushedLine("c-wip") + "\n" + resultLine(false, "stopped; work in progress pushed to factory/t2 at c-wip") + "\n"
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeNoPush || res.Commit != "c-wip" || res.CheckIn == nil || res.CheckIn.Decision != CheckHandover {
		t.Fatalf("result = %+v, want the handover with the pushed work in progress as its commit alone", res)
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
	kube.CreateNamespace(context.Background(), "ns", nil)
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
	kube.CreateNamespace(context.Background(), "ns", nil)
	kube.CreatePod(context.Background(), "ns", "p", nil)
	kube.scriptPods(0, strings.Repeat("0123456789\n", 1000))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pl := followPodLogCap(ctx, kube, "ns", "p", "t1", 0, 5000, nil)
	time.Sleep(200 * time.Millisecond)
	n := 0
	for range pl.lines {
		n++
	}
	if pl.err() == nil || n == 0 || n >= 1000 {
		t.Fatalf("delivered %d lines, err %v; want what fit, then an error", n, pl.err())
	}
}

// A check-in answer overtaken by the session's own push is commented as
// superseded; the task goes to verification on what the session pushed,
// and the conversation's record still landed.
func TestRunSessionCheckinAnswerSupersededByAPush(t *testing.T) {
	runner, hub, _, _, chat, _, objects := liveRig(t, CheckinResponse{Text: `{"decision":"handover","handover":"stop here; the loop needs a different shape"}`})
	logs := loopFailure(1) + loopFailure(2) + loopFailure(3) + pushedLine("c1") + "\n" + resultLine(true, "pushed branch factory/t2 at c1")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed || res.Commit != "c1" {
		t.Fatalf("result = (%+v, %v), want pushed with c1: the session's own push won", res, err)
	}
	if res.CheckIn != nil {
		t.Fatalf("check-in = %+v, want the outcome cleared by the supersede", res.CheckIn)
	}
	if chat.calls != 1 {
		t.Fatalf("chat calls = %d, want the superseded conversation", chat.calls)
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "Superseded: the session finished and pushed c1 during the check-in; the task goes to verification.") {
		t.Fatalf("comments = %q", hub.comments)
	}
	found := false
	for _, l := range strings.Split(strings.TrimSpace(string(objGet(objects, "sessions/t2/"+res.SessionID+".jsonl"))), "\n") {
		var ev harness.Event
		if json.Unmarshal([]byte(l), &ev) != nil || ev.Kind != harness.EventSupervisor {
			continue
		}
		if rec, _ := ev.Detail["record"].(map[string]any); rec != nil && rec["decision"] == CheckHandover {
			found = true
		}
	}
	if !found {
		t.Fatal("no event carries the superseded conversation's record")
	}
}

func TestWorkerEgressAdmitsLeasedServersOnSSH(t *testing.T) {
	r := &Runner{cfg: testRunnerConfig()}
	plain := r.workerEgressSpec(sandboxRegistries, nil)["egress"].([]any)
	spec := r.workerEgressSpec(sandboxRegistries, []string{"203.0.113.7", "203.0.113.8"})
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
	r := &Runner{cfg: cfg, kube: kube, store: newFakeStore()}
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
	r := &Runner{cfg: cfg, kube: forbiddenPolicy{fakeKube: kube, ns: "factory-agents-t1"}, store: newFakeStore()}
	if err := r.AdmitLeases(context.Background()); err != nil {
		t.Fatalf("AdmitLeases = %v, want nil", err)
	}
	if got := strings.Join(kube.applies, ","); got != "factory-agents-t2/worker-egress" {
		t.Fatalf("applies = %s, want t2's only", got)
	}
}

// Roadmap 188: teardown deletes the task's namespace asynchronously, so it
// stays Terminating while its contents are garbage-collected away. A policy
// write there fails ("unable to create new content in namespace ... because
// it is being terminated"), and the namespace needs none any more: the lease
// admission waits it out and admits the live namespaces without an error.
func TestAdmitLeasesSkipsTerminatingNamespaces(t *testing.T) {
	kube := newFakeKube()
	for _, id := range []string{"t1", "t2"} {
		kube.namespaces["factory-agents-"+id] = map[string]string{"factory.ai/task": id}
	}
	kube.terminating["factory-agents-t1"] = true // DeleteNamespace returned; the GC is running
	cfg := testRunnerConfig()
	cfg.Leases = &fakeLeases{byID: map[string][]string{"t1": {"203.0.113.7"}, "t2": {"203.0.113.8"}}}
	r := &Runner{cfg: cfg, kube: kube, store: newFakeStore()}
	if err := r.AdmitLeases(context.Background()); err != nil {
		t.Fatalf("AdmitLeases = %v, want nil", err)
	}
	if got := strings.Join(kube.applies, ","); got != "factory-agents-t2/worker-egress" {
		t.Fatalf("applies = %s, want t2's only", got)
	}
}

// The wait out of a terminating namespace is observed only where it holds
// a live lease back: an ordinary teardown, whose task holds none, records
// no namespace_wait step (the step on every teardown read as sessions
// running in a dying namespace, roadmap 240).
func TestAdmitLeasesObservesOnlyAWaitThatHoldsALeaseBack(t *testing.T) {
	kube := newFakeKube()
	r, _, _ := newTestRunner(t, Task{ID: "t1", Track: "direct"}, kube, &fakeObjectStore{})
	for _, id := range []string{"t1", "t3"} {
		kube.namespaces["factory-agents-"+id] = map[string]string{"factory.ai/task": id}
		kube.terminating["factory-agents-"+id] = true
	}
	r.cfg.Leases = &fakeLeases{byID: map[string][]string{"t1": {"203.0.113.7"}}}
	if err := r.AdmitLeases(context.Background()); err != nil {
		t.Fatalf("AdmitLeases = %v, want nil", err)
	}
	if got := fmt.Sprint(stepsOf(t, r, "t1", "namespace", "leases")); got != "[map[leases:[203.0.113.7] namespace:factory-agents-t1 step:namespace_wait]]" {
		t.Errorf("t1's timeline = %s, want one namespace_wait naming its lease", got)
	}
	if b, _ := r.timeline.Objects.Get(context.Background(), "sessions/t3/timeline.jsonl"); strings.TrimSpace(string(b)) != "" {
		t.Errorf("t3's timeline = %s, want nothing for a teardown that holds no lease back", b)
	}
}

func TestSetupNamespaceAdmitsTheTasksLeases(t *testing.T) {
	kube := newFakeKube()
	kube.sourceData["factory/forgejo-registry"] = map[string]string{".dockerconfigjson": "{}"}
	cfg := testRunnerConfig()
	cfg.Leases = &fakeLeases{byID: map[string][]string{"t1": {"203.0.113.7"}, "t2": {"203.0.113.8"}}}
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t1", Track: "direct"})
	r := &Runner{cfg: cfg, kube: kube, store: store}
	if _, _, err := r.setupNamespace(context.Background(), &Task{ID: "t1"}, ForgejoToken{}); err != nil {
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

// The worker pod carries the policy's migration globs as MIGRATION_GLOBS
// (roadmap 186): the worker's push guard refuses a migration added under
// them with a name off the timestamped pattern, where the guide's
// sentence used to say it. A policy with no migration rule sets none.
func TestWorkerPodSpecCarriesMigrationGlobs(t *testing.T) {
	spec := sessionSpec{id: "s-1", policy: policy.Document{Risk: policy.Risk{
		Classes: []policy.RiskClass{{Name: "migration", Outcome: policy.OutcomeRequiresApproval, Migration: true}},
		Rules:   []policy.RiskRule{{Class: "migration", Paths: []string{"internal/policy/migrations/**", "internal/policy/schema.sql"}}},
	}}}
	env := func(s sessionSpec) map[string]string {
		pod := (&Runner{cfg: testRunnerConfig(), forgejo: &Forgejo{BotUser: "factory-agent"}}).workerPodSpec(Task{ID: "t1"}, s)
		vars := map[string]string{}
		for _, e := range pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"].([]map[string]any) {
			vars[e["name"].(string)] = e["value"].(string)
		}
		return vars
	}
	if got := env(spec)["MIGRATION_GLOBS"]; got != `["internal/policy/migrations/**","internal/policy/schema.sql"]` {
		t.Fatalf("MIGRATION_GLOBS = %q, want the migration rule's globs", got)
	}
	spec.policy.Risk = policy.Risk{}
	if got, ok := env(spec)["MIGRATION_GLOBS"]; ok {
		t.Fatalf("MIGRATION_GLOBS = %q without a migration rule, want none", got)
	}
}

// The worker pod carries the egress probe's targets (roadmap 42) as
// factoryd is configured with them.
func TestWorkerPodSpecCarriesTheEgressProbeTargets(t *testing.T) {
	cfg := testRunnerConfig()
	cfg.EgressCanary, cfg.EgressDirectIP = "canary.example:443", "192.0.2.7:443"
	pod := (&Runner{cfg: cfg, forgejo: &Forgejo{BotUser: "factory-agent"}}).workerPodSpec(Task{ID: "t1"}, sessionSpec{id: "s-1"})
	env := containerEnv(pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any))
	if env["EGRESS_CANARY"] != "canary.example:443" || env["EGRESS_DIRECT_IP"] != "192.0.2.7:443" {
		t.Fatalf("EGRESS_CANARY = %q, EGRESS_DIRECT_IP = %q; want canary.example:443 and 192.0.2.7:443", env["EGRESS_CANARY"], env["EGRESS_DIRECT_IP"])
	}
}

// factoryd refuses at boot an egress probe target the probe cannot use:
// a canary that is no name and a port, or one on the sandbox's DNS
// allowlist (its probe would fail every start), and a direct IP that is
// no IP and a port.
func TestNewRunnerRefusesEgressProbeTargetsTheProbeCannotUse(t *testing.T) {
	for _, tc := range []struct{ canary, directIP, want string }{
		{"example.com", "192.0.2.7:443", `runner: the egress canary must be a name and a port, is "example.com"`},
		{"192.0.2.8:443", "192.0.2.7:443", `runner: the egress canary must be a name and a port, is "192.0.2.8:443"`},
		{"example.com:0", "192.0.2.7:443", `runner: the egress canary must be a name and a port, is "example.com:0"`},
		{"", "192.0.2.7:443", `runner: the egress canary must be a name and a port, is ""`},
		{"example.com:443", "one.one.one.one:443", `runner: the egress direct IP must be an IP and a port, is "one.one.one.one:443"`},
		{"example.com:443", "192.0.2.7", `runner: the egress direct IP must be an IP and a port, is "192.0.2.7"`},
		{"proxy.golang.org:443", "192.0.2.7:443", "runner: the egress canary proxy.golang.org is on the sandbox's DNS allowlist, so its probe would fail every session's start"},
		{"Factory-Gateway.factory.svc.cluster.local.:8081", "192.0.2.7:443", "runner: the egress canary Factory-Gateway.factory.svc.cluster.local. is on the sandbox's DNS allowlist, so its probe would fail every session's start"},
	} {
		cfg := testRunnerConfig()
		cfg.EgressCanary, cfg.EgressDirectIP = tc.canary, tc.directIP
		if _, err := NewRunner(cfg, newFakeStore(), newFakeKube(), &Forgejo{}, &Completion{}, &fakeObjectStore{}, testSupervisorModel("http://unused")); err == nil || err.Error() != tc.want {
			t.Errorf("canary %q, direct IP %q: err = %v, want %s", tc.canary, tc.directIP, err, tc.want)
		}
	}
	if _, err := NewRunner(testRunnerConfig(), newFakeStore(), newFakeKube(), &Forgejo{}, &Completion{}, &fakeObjectStore{}, testSupervisorModel("http://unused")); err != nil {
		t.Fatalf("example.com:443 and 192.0.2.7:443: %v", err)
	}
}

func TestNewRunnerRequiresTheSandboxBaseAndTheStore(t *testing.T) {
	const want = "runner: AgentNamespacePrefix, WorkerImage, SandboxBaseImage, RegistryHost, GatewayURL, GitBaseURL and Leases are required"
	for _, unset := range []func(*RunnerConfig){
		func(c *RunnerConfig) { c.SandboxBaseImage = "" },
		func(c *RunnerConfig) { c.RegistryHost = "" },
	} {
		cfg := testRunnerConfig()
		unset(&cfg)
		if _, err := NewRunner(cfg, newFakeStore(), newFakeKube(), &Forgejo{}, &Completion{}, &fakeObjectStore{}, testSupervisorModel("http://unused")); err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %s", err, want)
		}
	}
}

// One uid: the sandbox pod runs as SandboxUID, which the worker's egress
// probe holds itself to, and Dockerfile.sandbox adds and runs as its user.
func TestSandboxUIDIsThePodsAndTheImages(t *testing.T) {
	pod := (&Runner{cfg: testRunnerConfig(), forgejo: &Forgejo{BotUser: "factory-agent"}}).workerPodSpec(Task{ID: "t1"}, sessionSpec{id: "s-1"})
	sc := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["securityContext"].(map[string]any)
	if sc["runAsUser"] != SandboxUID || sc["runAsGroup"] != SandboxUID {
		t.Fatalf("runAsUser %v, runAsGroup %v; want %d", sc["runAsUser"], sc["runAsGroup"], SandboxUID)
	}
	raw, err := os.ReadFile("../../Dockerfile.sandbox")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{fmt.Sprintf("useradd -m -u %d ", SandboxUID), fmt.Sprintf("\nUSER %d:%d\n", SandboxUID, SandboxUID)} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("Dockerfile.sandbox lacks %q", want)
		}
	}
}

// Roadmap 237: every worker env value that grows with its input stays
// within the 128 KiB Linux allows one "NAME=value" string with its NUL
// (MAX_ARG_STRLEN), so a session's container always starts, and LEFT_OUT
// carries factoryd's note on each it shortened, none inside a list. A
// list of 32 items of 4000 bytes and one of m is m + 128100 bytes of JSON
// (the bodies, 3 bytes per item for its quotes and comma, 1 for the
// brackets less one comma). With its "=" and NUL, FINDINGS (8 bytes) holds
// m = 2962 and DIRECTORIES (11) 2959; one byte more leaves out the item at
// the list's oldest end (the last directory). Roadmap 201: FINDINGS keeps
// at most 60 items, ANSWERS and POSTER_INFO 20, the oldest left out
// first; 19 items of 4000 bytes and one of m are m + 76061 bytes, so
// ANSWERS (7) holds m = 55002 and POSTER_INFO (11) 54998. 21 answers, the
// newest of 55003 bytes before 19 of 4000 and an old one, leave out the
// old one by the cap and the oldest of 4000 by the bytes, in one note. A
// text over its room is cut on a character with "…" (3
// bytes) to fill the room: OBJECTIVE's is 131061 bytes, so "a" and 65531
// two-byte "ä" (131063) keep 131057; REPORT_GUIDE's 131058, SPEC's 131066.
func TestWorkerPodSpecFitsTheEnv(t *testing.T) {
	env := func(task Task, s sessionSpec) map[string]string {
		pod := (&Runner{cfg: testRunnerConfig(), forgejo: &Forgejo{BotUser: "factory-agent"}}).workerPodSpec(task, s)
		vars := map[string]string{}
		for _, e := range pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"].([]map[string]any) {
			vars[e["name"].(string)] = e["value"].(string)
		}
		return vars
	}
	z := func(n int) []string { return []string{strings.Repeat("z", n)} }
	full := slices.Repeat([]string{strings.Repeat("a", 4000)}, 32)
	a := strings.Repeat
	short := func(n int) []string {
		var l []string
		for i := range n {
			l = append(l, fmt.Sprint("s", i))
		}
		return l
	}
	for _, tc := range []struct {
		name    string
		task    Task
		s       sessionSpec
		v, want string
		leftOut []string
	}{
		{"FINDINGS whole", Task{}, sessionSpec{findings: slices.Concat(z(2962), full)}, "FINDINGS", jsonOf(slices.Concat(z(2962), full)), nil},
		{"FINDINGS one byte over", Task{}, sessionSpec{findings: slices.Concat(z(2963), full)}, "FINDINGS", jsonOf(full), []string{"1 of the 33 findings, the oldest"}},
		{"FINDINGS past the cap", Task{}, sessionSpec{findings: short(70)}, "FINDINGS", jsonOf(short(70)[10:]), []string{"10 of the 70 findings, the oldest"}},
		{"ANSWERS whole", Task{}, sessionSpec{answers: slices.Concat(full[:19], z(55002))}, "ANSWERS", jsonOf(slices.Concat(full[:19], z(55002))), nil},
		{"ANSWERS one byte over", Task{}, sessionSpec{answers: slices.Concat(full[:19], z(55003))}, "ANSWERS", jsonOf(full[:19]),
			[]string{"1 of the 20 answers to the escalations, the oldest"}},
		{"ANSWERS past the cap", Task{}, sessionSpec{answers: short(25)}, "ANSWERS", jsonOf(short(25)[:20]), []string{"5 of the 25 answers to the escalations, the oldest"}},
		{"ANSWERS past the cap and the bytes", Task{}, sessionSpec{answers: slices.Concat(z(55003), full[:19], []string{"old"})}, "ANSWERS",
			jsonOf(slices.Concat(z(55003), full[:18])), []string{"2 of the 21 answers to the escalations, the oldest"}},
		{"POSTER_INFO whole", Task{}, sessionSpec{posterInfo: slices.Concat(z(54998), full[:19])}, "POSTER_INFO", jsonOf(slices.Concat(z(54998), full[:19])), nil},
		{"POSTER_INFO one byte over", Task{}, sessionSpec{posterInfo: slices.Concat(z(54999), full[:19])}, "POSTER_INFO", jsonOf(full[:19]),
			[]string{"1 of the 20 answers of the issue's author, the oldest"}},
		{"POSTER_INFO past the cap", Task{}, sessionSpec{posterInfo: short(25)}, "POSTER_INFO", jsonOf(short(25)[5:]), []string{"5 of the 25 answers of the issue's author, the oldest"}},
		{"DIRECTORIES whole", Task{}, sessionSpec{directories: slices.Concat(full, z(2959))}, "DIRECTORIES", jsonOf(slices.Concat(full, z(2959))), nil},
		{"DIRECTORIES one byte over", Task{}, sessionSpec{directories: slices.Concat(full, z(2960))}, "DIRECTORIES", jsonOf(full),
			[]string{"1 of the 33 directories, the last"}},
		{"OBJECTIVE whole", Task{Objective: a("a", 131061)}, sessionSpec{}, "OBJECTIVE", a("a", 131061), nil},
		{"OBJECTIVE cut on a character", Task{Objective: "a" + a("ä", 65531)}, sessionSpec{}, "OBJECTIVE", "a" + a("ä", 65528) + "…",
			[]string{"the issue's text past its first 131057 of 131063 bytes"}},
		{"REPORT_GUIDE cut", Task{}, sessionSpec{report: a("a", 131059)}, "REPORT_GUIDE", a("a", 131055) + "…",
			[]string{"the report's guide past its first 131055 of 131059 bytes"}},
		{"SPEC cut", Task{}, sessionSpec{spec: a("a", 131067), cardRole: "web"}, "SPEC", a("a", 131063) + "…",
			[]string{"the spec and the plan past its first 131063 of 131067 bytes"}},
	} {
		tc.task.ID, tc.s.id = "t1", "s-1"
		vars := env(tc.task, tc.s)
		if got := vars[tc.v]; got != tc.want {
			t.Errorf("%s: %s = %d bytes, want %d", tc.name, tc.v, len(got), len(tc.want))
		}
		for name, v := range vars {
			if n := len(name) + len("=") + len(v) + len("\x00"); n > 128<<10 {
				t.Errorf("%s: %s's string is %d bytes, over 128 KiB", tc.name, name, n)
			}
		}
		if got, ok := vars["LEFT_OUT"]; ok != (tc.leftOut != nil) || ok && got != jsonOf(tc.leftOut) {
			t.Errorf("%s: LEFT_OUT = %q (set %v), want %q", tc.name, got, ok, tc.leftOut)
		}
	}
}

// The climb in place (roadmap 222, item 5): on the start rung's first
// attempt the cheap cap trips without a push, and the session goes on
// instead of ending: the task's alias moves to the stronger one, the
// worker gets the model with the message, the switch is recorded, and the
// result names the model the session ended on.
func TestRunSessionClimbsInPlace(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, store := newTestRunner(t, supervisedTask("t2"), kube, objects)
	runner.cheapCap = 50 * time.Millisecond
	runner.cfg.Supervisor.NoProgressAfter = 3 * time.Second // the tick every second
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)
	ctx, cancel := context.WithCancel(context.Background())
	// the cancel waits for the climb's comment, the switch's last side
	// effect: canceling at the worker's message alone races the session's
	// bookkeeping after the switch and loses the comment, the timeline
	// step or the cancellation's own answer
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			hub.mu.Lock()
			n := len(hub.comments)
			hub.mu.Unlock()
			if n > 0 {
				// the climb's comment on the issue follows the message
				time.Sleep(500 * time.Millisecond)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	// the cancel ends the session by its context or by the closed log
	// stream, whichever the loop takes first
	if _, err := runSession(t, ctx, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/small"}, nil, hangLogs); err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("err = %v, want the test's cancel after the climb", err)
	}
	worker.mu.Lock()
	texts := slices.Clone(worker.texts)
	worker.mu.Unlock()
	if len(texts) != 2 || texts[0] != "POST /brief " || !strings.HasPrefix(texts[1], "POST /message factoryd: this session continues on the stronger model, big: the cheap rung's 50ms passed without a push.") {
		t.Fatalf("worker got %q, want the brief and the climb's message", texts)
	}
	if store.tasks["t2"].ModelAlias != "big" {
		t.Fatalf("model_alias = %q, want the stronger alias for the gateway", store.tasks["t2"].ModelAlias)
	}
	step := timelineStep(t, objects, "t2", "model_switched")
	if step["from"] != "small" || step["to"] != "big" || step["trip"] != "cheap_cap" {
		t.Fatalf("model_switched step = %v", step)
	}
	if !slices.ContainsFunc(hub.comments, func(c string) bool { return strings.Contains(c, "continues on the stronger model from here") }) {
		t.Fatalf("comments = %q, want the climb explained on the issue", hub.comments)
	}
}

// A session that climbed and then pushes reports the model it ended on,
// so the next session starts there.
func TestRunSessionResultNamesTheModelItEndedOn(t *testing.T) {
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), kube, &fakeObjectStore{})
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 2, Model: "factory/big"}, nil, pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "factory/big" || res.Outcome != OutcomePushed {
		t.Fatalf("result = %+v, want the model it ended on", res)
	}
}

func TestRunSessionStatesTheFactsInThePodEnv(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", SHA: "base", BudgetEUR: 5, SpentEUR: 1.8, Track: "direct"}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Sessions: 4, Model: "factory/small"}, nil,
		pushedLine("c0ffee")+"\n"+resultLine(true, "done")); err != nil {
		t.Fatal(err)
	}
	got := podEnv(kube.podAt(0))["FACTS"]
	if !regexp.MustCompile(`^factoryd's facts for this session: budget 3\.20 of 5\.00 EUR left; \d+ minutes of agent time left; this is session 1 of the 4 `).MatchString(got) {
		t.Fatalf("FACTS = %q", got)
	}
}

func TestRunSessionStatesTheCardsSizeInTheFacts(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", SHA: "base", BudgetEUR: 5, Track: "direct", Size: "medium"}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	version := testPolicyVersion()
	version.Document.Risk.LargeLines = 1000
	runner.cfg.Policy = func() policy.Version { return version }
	runner.cfg.PolicyVersion = func(context.Context, int64) (policy.Version, bool, error) { return version, true, nil }
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Sessions: 3, Model: "factory/small"}, nil,
		pushedLine("c0ffee")+"\n"+resultLine(true, "done")); err != nil {
		t.Fatal(err)
	}
	if got := podEnv(kube.podAt(0))["FACTS"]; !strings.Contains(got, "This card is planned at most 600 changed lines (size medium).") {
		t.Fatalf("FACTS = %q", got)
	}
}

func TestRunSessionWithoutASessionsCapStatesThePolicyLimit(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", SHA: "base", BudgetEUR: 5, SpentEUR: 1.8, Track: "direct"}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	// testPolicyYAML sets limits.sessions to 3
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 3, Model: "factory/small"}, nil,
		pushedLine("c0ffee")+"\n"+resultLine(true, "done")); err != nil {
		t.Fatal(err)
	}
	got := podEnv(kube.podAt(0))["FACTS"]
	if !strings.Contains(got, "this is session 3 of the 3 the policy allows before a human is asked. This card has no size, so it is planned at most 1000 changed lines, the policy's large_lines. It is the last one") {
		t.Fatalf("FACTS = %q", got)
	}
}

// Near the budget the running agent is told to wrap up and push: the warning
// reaches the worker's message endpoint once.
func TestRunSessionWarnsNearTheBudget(t *testing.T) {
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, supervisedTask("t2"), kube, &fakeObjectStore{})
	tk := store.tasks["t2"]
	tk.BudgetEUR, tk.SpentEUR = 5, 4.5
	store.tasks["t2"] = tk
	v := runner.cfg.Policy()
	v.Document.Limits.NoPushSpend = 99 // the spend detector is task 4's
	runner.cfg.Policy = func() policy.Version { return v }
	runner.cfg.Supervisor.NoProgressAfter = time.Minute
	runner.tick = 50 * time.Millisecond
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			worker.mu.Lock()
			n := len(worker.texts)
			worker.mu.Unlock()
			if n > 0 {
				time.Sleep(500 * time.Millisecond) // ten further ticks: no second warning
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	runSession(t, ctx, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/big"}, nil, hangLogs)
	worker.mu.Lock()
	texts := slices.Clone(worker.texts)
	worker.mu.Unlock()
	if len(texts) != 2 || texts[0] != "POST /brief " || !strings.HasPrefix(texts[1], "POST /message factoryd: 90 percent of this card's budget is spent, 0.50 of 5.00 EUR are left.") {
		t.Fatalf("worker got %q, want the brief and one budget warning", texts)
	}
}

// A warning the worker did not take comes again on the next tick.
func TestRunSessionRepeatsAWarningTheWorkerRefused(t *testing.T) {
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, supervisedTask("t2"), kube, &fakeObjectStore{})
	tk := store.tasks["t2"]
	tk.BudgetEUR, tk.SpentEUR = 5, 4.5
	store.tasks["t2"] = tk
	v := runner.cfg.Policy()
	v.Document.Limits.NoPushSpend = 99
	runner.cfg.Policy = func() policy.Version { return v }
	runner.cfg.Supervisor.NoProgressAfter = time.Minute
	runner.tick = 50 * time.Millisecond
	worker := &fakeWorkerEndpoint{status: http.StatusServiceUnavailable}
	worker.start(t, runner)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			worker.mu.Lock()
			n := len(worker.texts)
			worker.mu.Unlock()
			if n >= 4 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	runSession(t, ctx, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/big"}, nil, hangLogs)
	worker.mu.Lock()
	n := len(worker.texts)
	worker.mu.Unlock()
	if n < 4 {
		t.Fatalf("worker got %d warnings, want the refused one again each tick", n)
	}
	timeline, _ := runner.objects.Get(context.Background(), "sessions/t2/timeline.jsonl")
	if steps := strings.Count(string(timeline), `"step":"message"`); steps != 1 {
		t.Fatalf("%d message steps recorded, want only the first failure:\n%s", steps, timeline)
	}
}

// A card past its budget share with nothing pushed asks the check-in.
func TestRunSessionSpendWithoutPushRunsCheckin(t *testing.T) {
	runner, hub, _, _, chat, store, _ := liveRig(t, CheckinResponse{Text: `{"decision":"continue","reason":"it is making progress"}`})
	tk := store.tasks["t2"]
	tk.BudgetEUR, tk.SpentEUR = 5, 3.5 // 70 percent, past the default 60
	store.tasks["t2"] = tk
	runner.cfg.Supervisor.NoProgressAfter = time.Hour
	runner.tick = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			hub.mu.Lock()
			found := slices.ContainsFunc(hub.comments, func(c string) bool { return strings.Contains(c, "spend_no_push") })
			hub.mu.Unlock()
			if found {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	runSession(t, ctx, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/big"}, nil, hangLogs)
	if chat.calls != 1 || !strings.Contains(chat.gots[0].Input, "spend_no_push") {
		t.Fatalf("chat = %d calls, want the spend trip's conversation", chat.calls)
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if !slices.ContainsFunc(hub.comments, func(c string) bool {
		return strings.Contains(c, "spend_no_push") && strings.Contains(c, "**continue**")
	}) {
		t.Fatalf("comments = %q, want the check-in's continue on spend_no_push", hub.comments)
	}
}

// cardDiffLine is the pod's checkpoint line with the card's diff against
// main.
func cardDiffLine(ins, del int) string {
	return envLine(harness.Event{Kind: harness.EventCheckpoint, Time: time.Now(), Detail: map[string]any{
		"card_diff": map[string]any{"files": 4, "insertions": ins, "deletions": del}}})
}

// A small card plans 300 lines under large_lines 1000: its diff of 350
// asks the check-in with the size fact.
func TestRunSessionAsksTheCheckinWhenTheCardPassesItsBound(t *testing.T) {
	runner, _, _, _, chat, store, _ := liveRig(t, CheckinResponse{Text: `{"decision":"continue","reason":"the rest is tests"}`})
	tk := store.tasks["t2"]
	tk.Size = "small"
	store.tasks["t2"] = tk
	version := testPolicyVersion()
	version.Document.Risk.LargeLines = 1000
	runner.cfg.Policy = func() policy.Version { return version }
	runner.cfg.PolicyVersion = func(context.Context, int64) (policy.Version, bool, error) { return version, true, nil }
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/big"}, nil,
		cardDiffLine(250, 100)+"\n"+pushedLine("c0ffee")+"\n"+resultLine(true, "done")); err != nil {
		t.Fatal(err)
	}
	want := "the card's diff is 350 changed lines (added plus deleted), over the 300 its size plans"
	if chat.calls != 1 || !strings.Contains(chat.gots[0].Input, want) {
		t.Fatalf("chat = %d calls %+v, want one with %q", chat.calls, chat.gots, want)
	}
}

// A report session changes no code: its card diff asks no check-in.
func TestRunSessionOfAReportNeverTripsSize(t *testing.T) {
	report := Task{ID: "t1", Status: "running", Objective: "o", Repository: "sf/r", SHA: "abc123", Process: "maintenance", Parent: "t-parent",
		Source: SourceScreening, Track: policy.TrackDirect, RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 2}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, report, kube, &fakeObjectStore{})
	if err := store.CreateTask(context.Background(), parentTask()); err != nil {
		t.Fatal(err)
	}
	chat := &scriptedChat{answers: []CheckinResponse{{Text: `{"decision":"continue","reason":"r"}`}}}
	runner.checkin = chat
	done := make(chan error)
	go func() {
		_, err := execActivity(t, runner, ActivityRunReport, "t1", "factory/small")
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, cardDiffLine(4000, 1000)+"\n"+resultLine(true, `{"verdict": "clear"}`))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if chat.calls != 0 {
		t.Fatalf("chat = %d calls %+v, want none", chat.calls, chat.gots)
	}
}

func TestRunSessionPassesTheContextCap(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", SHA: "base", Track: "direct"}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small"}, nil,
		pushedLine("c0ffee")+"\n"+resultLine(true, "done")); err != nil {
		t.Fatal(err)
	}
	if got := podEnv(kube.podAt(0))["CONTEXT_TOKENS"]; got != "100000" {
		t.Fatalf("CONTEXT_TOKENS = %q, want the default 100000", got)
	}
}

// The gateway refused a call for the card's budget and the worker pushed
// the work in progress: the session is pushed, with that commit.
func TestRunSessionBudgetRefusalAfterAPushIsPushed(t *testing.T) {
	task := Task{ID: "t3", Status: "running", Objective: "x", Repository: "sf/repo", Track: "direct"}
	runner, _, store := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
	store.budgetRefused = map[string]bool{"t3": true}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t3", Attempt: 1, Model: "m"}, nil,
		pushedLine("c-wip")+"\n"+resultLine(false, "free headroom 0.07 EUR fits 0 output tokens (status 402); work in progress pushed to factory/t3 at c-wip"))
	if err != nil || res.Outcome != OutcomePushed || res.Commit != "c-wip" {
		t.Fatalf("result = (%+v, %v), want pushed at c-wip", res, err)
	}
}

// A failure that pushed but met no budget refusal stays no_push, and a
// refusal with nothing pushed has no commit to call pushed.
func TestRunSessionOnlyABudgetRefusalAfterAPushIsPushed(t *testing.T) {
	task := Task{ID: "t3", Status: "running", Objective: "x", Repository: "sf/repo", Track: "direct"}
	runner, _, store := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t3", Attempt: 1, Model: "m"}, nil,
		pushedLine("c-wip")+"\n"+resultLine(false, "model refused"))
	if err != nil || res.Outcome != OutcomeNoPush {
		t.Fatalf("no refusal: (%+v, %v), want no_push", res, err)
	}
	store.budgetRefused = map[string]bool{"t3": true}
	res, err = runSession(t, nil, runner, SessionInput{TaskID: "t3", Attempt: 1, Model: "m"}, nil, resultLine(false, "free headroom 0 EUR"))
	if err != nil || res.Outcome != OutcomeNoPush {
		t.Fatalf("refusal without a push: (%+v, %v), want no_push", res, err)
	}
}

// liveRig is a running session whose in-session check-in answers the
// scripted responses in order, over a worker endpoint that takes every
// push.
func liveRig(t *testing.T, answers ...CheckinResponse) (*Runner, *forgejoHub, *fakeKube, *fakeWorkerEndpoint, *scriptedChat, *fakeStore, *fakeObjectStore) {
	t.Helper()
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, store := newTestRunner(t, supervisedTask("t2"), kube, objects)
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)
	chat := &scriptedChat{answers: answers}
	runner.checkin = chat
	return runner, hub, kube, worker, chat, store, objects
}

// checkinRig is a session whose check-in runs against one scripted final
// answer, over a worker endpoint that takes every push.
func checkinRig(t *testing.T, answer string) (*Runner, *forgejoHub, *fakeKube, *fakeWorkerEndpoint, *scriptedChat, *fakeStore, *fakeObjectStore) {
	return liveRig(t, CheckinResponse{Text: answer})
}

// A check-in that asks a human stops the pod before the coding
// conversation starts: nothing of the session is spent, the pushed result
// the pod's log was scripted to hold is never read, and the outcome
// carries the ask's reason and the record of the conversation that ran
// under the class's review model.
func TestRunSessionCheckinHumanStopsBeforeTheConversation(t *testing.T) {
	finding := "the objective contradicts the review"
	runner, _, kube, worker, chat, _, objects := checkinRig(t,
		`{"findings":[{"text":"`+finding+`","verdict":"confirmed"}],"decision":"human","reason_kind":"goal_conflict","reason":"the objective contradicts the review"}`)
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m",
		CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: finding, Source: "review"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeCheckinHuman || res.Commit != "" || res.Reason != "the objective contradicts the review" {
		t.Fatalf("result = (%+v, %v), want the human ask before any spend", res, err)
	}
	if res.CheckIn == nil || res.CheckIn.Decision != CheckHuman || res.CheckIn.ReasonKind != CheckGoalConflict ||
		res.CheckIn.Verdicts[finding].State != CheckConfirmed {
		t.Fatalf("check-in = %+v, want the human ask under goal_conflict with the verdict", res.CheckIn)
	}
	if rec := res.CheckIn.Record; rec == nil || rec.Model != "big" || rec.ParentSession != "" ||
		rec.Decision != CheckHuman || rec.ReasonKind != CheckGoalConflict {
		t.Fatalf("record = %+v, want the coder class's review model and the ask carried to Task 6", res.CheckIn.Record)
	}
	if chat.calls != 1 {
		t.Fatalf("chat calls = %d, want the one deciding call", chat.calls)
	}
	if len(kube.deletedPods) != 1 || !strings.HasPrefix(kube.deletedPods[0], "factory-agents-t2/worker-s-") {
		t.Fatalf("deleted pods = %q, want the session's pod stopped before the conversation", kube.deletedPods)
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if len(worker.texts) != 0 {
		t.Fatalf("worker got %q, want nothing: no brief and no conversation for a stopped session", worker.texts)
	}
	tl, _ := objects.Get(t.Context(), "sessions/t2/timeline.jsonl")
	if !strings.Contains(string(tl), `"step":"session_start"`) || !strings.Contains(string(tl), `"outcome":"checkin_human"`) {
		t.Fatalf("timeline = %s, want the pod's start and the check-in's end", tl)
	}
}

// A check-in that hands a note over pushes it as the brief and the coding
// conversation runs: the session's own result stands with the check-in
// riding it.
func TestRunSessionCheckinHandoverRunsTheConversation(t *testing.T) {
	finding := "no test covers the handler"
	runner, _, _, worker, _, _, _ := checkinRig(t,
		`{"findings":[{"text":"`+finding+`","verdict":"confirmed"}],"decision":"handover","handover":"fix the handler test first"}`)
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m",
		CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: finding, Source: "review"}}}}, nil,
		pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.Outcome != OutcomePushed || res.Commit != "c1" {
		t.Fatalf("result = (%+v, %v), want the conversation's own push", res, err)
	}
	if res.CheckIn == nil || res.CheckIn.Decision != CheckHandover || res.CheckIn.Handover != "fix the handler test first" {
		t.Fatalf("check-in = %+v, want the handover riding the result", res.CheckIn)
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if !slices.Equal(worker.texts, []string{"POST /brief fix the handler test first"}) {
		t.Fatalf("worker got %q, want the note pushed as the brief before the conversation", worker.texts)
	}
}

// A check-in that drops every finding against evidence stops the session
// the way a human ask does. A mixed set answered dropped_all is the
// guards' handover: the conversation runs.
func TestRunSessionCheckinDropped(t *testing.T) {
	good := "internal/factoryd/store.go:672 at main: func (s *PGStore) TasksByPR"
	dropped := `{"findings":[{"text":"TasksByPR is undefined","verdict":"dropped","evidence":"` + good + `"}],"decision":"dropped_all"}`

	t.Run("every finding dropped", func(t *testing.T) {
		runner, _, kube, worker, _, _, _ := checkinRig(t, dropped)
		res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m",
			CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "TasksByPR is undefined", Source: "review"}}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeCheckinDropped || res.Commit != "" || res.CheckIn == nil ||
			res.CheckIn.Verdicts["TasksByPR is undefined"].State != CheckDropped ||
			res.CheckIn.Verdicts["TasksByPR is undefined"].Evidence != good {
			t.Fatalf("result = (%+v, %v), want the dropped outcome with the verdict and its evidence", res, err)
		}
		if len(kube.deletedPods) != 1 {
			t.Fatalf("deleted pods = %q, want the pod stopped", kube.deletedPods)
		}
		worker.mu.Lock()
		defer worker.mu.Unlock()
		if len(worker.texts) != 0 {
			t.Fatalf("worker got %q, want no conversation", worker.texts)
		}
	})

	t.Run("a mixed dropped_all is the guards' handover", func(t *testing.T) {
		runner, _, _, worker, _, _, _ := checkinRig(t,
			`{"findings":[{"text":"TasksByPR is undefined","verdict":"dropped","evidence":"`+good+`"},{"text":"no test covers the handler","verdict":"confirmed"}],"decision":"dropped_all"}`)
		res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m",
			CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "TasksByPR is undefined"}, {Text: "no test covers the handler"}}}}, nil,
			pushedLine("c1")+"\n"+resultLine(true, "pushed"))
		if err != nil || res.Outcome != OutcomePushed || res.Commit != "c1" {
			t.Fatalf("result = (%+v, %v), want the conversation to run", res, err)
		}
		if res.CheckIn == nil || res.CheckIn.Decision != CheckHandover {
			t.Fatalf("check-in = %+v, want the guards' handover for a set with a finding left standing", res.CheckIn)
		}
		worker.mu.Lock()
		defer worker.mu.Unlock()
		if len(worker.texts) != 1 || worker.texts[0] != "POST /brief "+res.CheckIn.Handover {
			t.Fatalf("worker got %q, want the handover pushed as the brief", worker.texts)
		}
	})
}

// The parent session's record is delivered whole to the pod for its copy,
// while the check-in's statement sees only the tail: the policy's cut,
// advanced past the partial line the cut lands in.
func TestRunSessionCheckinParentRecord(t *testing.T) {
	record := "HEAD-OF-THE-FIRST-LINE " + strings.Repeat("a", 30_000) + "\nTHE-SECOND-LINE\nTHE-THIRD-LINE\n"
	finding := "the objective contradicts the review"
	runner, _, _, worker, chat, _, _ := checkinRig(t,
		`{"findings":[{"text":"`+finding+`","verdict":"confirmed"}],"decision":"human","reason_kind":"goal_conflict","reason":"the objective contradicts the review"}`)
	if err := runner.objects.Put(t.Context(), "sessions/t2/s-parent-1.jsonl", []byte(record)); err != nil {
		t.Fatal(err)
	}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m", ParentSessionID: "s-parent-1",
		CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: finding, Source: "review"}}}}, nil)
	if err != nil || res.Outcome != OutcomeCheckinHuman {
		t.Fatalf("result = (%+v, %v), want the human ask", res, err)
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if !slices.Equal(worker.texts, []string{"POST /records s-parent-1" + record}) {
		t.Fatalf("worker got %d texts, want the whole record delivered for the pod's copy", len(worker.texts))
	}
	statement := chat.got.Input
	for _, want := range []string{"/workspace/.factory/sessions/s-parent-1.jsonl", "<<<SESSION-TAIL>>>", "THE-SECOND-LINE", "THE-THIRD-LINE"} {
		if !strings.Contains(statement, want) {
			t.Errorf("the statement lacks %s", want)
		}
	}
	if strings.Contains(statement, "HEAD-OF-THE-FIRST-LINE") {
		t.Error("the statement holds the head of a record the tail cut past")
	}
	if rec := res.CheckIn.Record; rec.ParentSession != "s-parent-1" || rec.TailBytes != len("THE-SECOND-LINE\nTHE-THIRD-LINE\n") {
		t.Fatalf("record = %+v, want the parent named and the tail's own size", res.CheckIn.Record)
	}
}

// The limits are read before anything is bought: with the budget share
// already spent, the session stops on the human ask under spend and no
// conversation runs.
func TestRunSessionCheckinLimitsExhausted(t *testing.T) {
	runner, _, kube, worker, chat, _, _ := checkinRig(t, "")
	store := runner.store.(*fakeStore)
	// the coder card's 0.3 EUR budget at the default 10 percent share
	store.checkinSpent = map[string]float64{"t2": 0.03}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m",
		CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "TasksByPR is undefined", Source: "review"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeCheckinHuman || res.CheckIn == nil || res.CheckIn.ReasonKind != CheckSpend || res.CheckIn.Reason != "the check-in's share of the budget is spent" {
		t.Fatalf("result = (%+v, %v), want the human ask under spend on the share", res, err)
	}
	if chat.calls != 0 || res.Commit != "" || len(kube.deletedPods) != 1 {
		t.Fatalf("chat calls %d, commit %q, deleted pods %q: nothing of the session ran", chat.calls, res.Commit, kube.deletedPods)
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if len(worker.texts) != 0 {
		t.Fatalf("worker got %q, want nothing pushed", worker.texts)
	}
}

// The gate moment's check-in opens with the policy's 20 calls too.
func TestRunSessionGateCheckinOpensWithThePolicysCalls(t *testing.T) {
	runner, _, _, _, chat, _, _ := checkinRig(t,
		`{"findings":[{"text":"TasksByPR is undefined","verdict":"confirmed"}],"decision":"handover","handover":"fix the caller first"}`)
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m",
		CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "TasksByPR is undefined", Source: "review"}}}}, nil,
		pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.CheckIn == nil || res.CheckIn.Decision != CheckHandover {
		t.Fatalf("result = (%+v, %v), want the gate check-in's handover", res, err)
	}
	if chat.calls != 1 {
		t.Fatalf("chat calls = %d, want the one conversation", chat.calls)
	}
	if !strings.Contains(chat.gots[0].Input, "calls left 20,") {
		t.Fatalf("statement = %q, want the policy's 20 calls left", chat.gots[0].Input)
	}
}

// The share a human's answer granted widens the budget the check-in counts
// (issue 959): the card's check-in share is spent, but TaskUsage says the
// answer granted one more of its widths, so the check-in's conversation
// runs where the policy's share alone would have stopped the session
// before any model call.
func TestRunSessionCheckinShareGrantedByAnAnswerRuns(t *testing.T) {
	runner, _, _, _, chat, _, _ := checkinRig(t,
		`{"findings":[{"text":"TasksByPR is undefined","verdict":"confirmed"}],"decision":"handover","handover":"fix the caller first"}`)
	store := runner.store.(*fakeStore)
	// the coder card's 0.3 EUR budget at the default 10 percent share,
	// spent to its last cent
	store.checkinSpent = map[string]float64{"t2": 0.03}
	tk := store.tasks["t2"]
	tk.Usage.ExtraCheckinShare = 1
	store.tasks["t2"] = tk
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m",
		CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "TasksByPR is undefined", Source: "review"}}}}, nil,
		pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomePushed || res.Commit != "c1" || res.CheckIn == nil || res.CheckIn.Decision != CheckHandover {
		t.Fatalf("result = %+v, want the pushed session on the check-in's handover", res)
	}
	if chat.calls != 1 {
		t.Fatalf("check-in calls = %d, want the one conversation the grant paid for", chat.calls)
	}
}

// A session stopped by its check-in is no session: the timeline holds the
// pod's start but no session_started, and SessionAnswered says no model
// answered for it.
func TestRunSessionCheckinHumanIsNoSession(t *testing.T) {
	finding := "the objective contradicts the review"
	runner, _, _, _, _, _, objects := checkinRig(t,
		`{"findings":[{"text":"`+finding+`","verdict":"confirmed"}],"decision":"human","reason_kind":"goal_conflict","reason":"the objective contradicts the review"}`)
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m",
		CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: finding, Source: "review"}}}}, nil)
	if err != nil || res.Outcome != OutcomeCheckinHuman {
		t.Fatalf("result = (%+v, %v), want the human ask", res, err)
	}
	tl, _ := objects.Get(t.Context(), "sessions/t2/timeline.jsonl")
	if !strings.Contains(string(tl), `"step":"session_start"`) || strings.Contains(string(tl), `"event":"session_started"`) {
		t.Fatalf("timeline = %s, want the pod's start and no session_started", tl)
	}
	if ok, err := runner.SessionAnswered(t.Context(), SessionAnswerInput{TaskID: "t2", SessionID: res.SessionID, Model: "m",
		Attempt: 1, Since: time.Now().Add(-time.Minute)}); err != nil || ok {
		t.Fatalf("SessionAnswered = (%v, %v), want false: no model answered for the session", ok, err)
	}
}

// Every session pushes its brief, empty when no check-in decided one: the
// worker holds the conversation's start for it, and a push that never
// lands would stall the session for the whole wait.
func TestRunSessionPushesAnEmptyBriefWithoutACheckin(t *testing.T) {
	runner, _, _, worker, _, _, _ := checkinRig(t, "")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.Outcome != OutcomePushed || res.CheckIn != nil {
		t.Fatalf("result = (%+v, %v), want the plain pushed session", res, err)
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if !slices.Equal(worker.texts, []string{"POST /brief "}) {
		t.Fatalf("worker got %q, want the empty brief before the conversation", worker.texts)
	}
}

// The check-in inside a live session reads but never edits: run is
// refused factoryd-side, before any post leaves, and the read tools
// still post to the worker's /tool.
func TestRunnerToolsLiveRefuseRun(t *testing.T) {
	kube := newFakeKube()
	kube.pods["ns1/p1"] = &fakePod{ns: "ns1", name: "p1"}
	r := &Runner{kube: kube}
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, r)
	tools := &runnerTools{r: r, ns: "ns1", pod: "p1", token: "bearer-7", live: true,
		parent: []byte("alpha\nbeta\n"), ask: &CheckInAsk{Gates: &GateResults{Pin: 7}}}

	if _, _, err := tools.Tool(t.Context(), "run", map[string]any{"command": "rm -rf ."}); err == nil ||
		!strings.Contains(err.Error(), "run is not available to the check-in inside a live session") {
		t.Fatalf("run on a live pod = %v, want the refusal named", err)
	}
	worker.mu.Lock()
	posted := slices.Clone(worker.tools)
	worker.mu.Unlock()
	if len(posted) != 0 {
		t.Fatalf("worker got %q, want no post for a refused run", posted)
	}
	worker.mu.Lock()
	worker.onTool = func(name string, args map[string]any) (int, string) {
		b, _ := json.Marshal(map[string]any{"output": name + " " + args["path"].(string), "cut": false})
		return http.StatusOK, string(b)
	}
	worker.mu.Unlock()
	if out, _, err := tools.Tool(t.Context(), "read_file", map[string]any{"path": "x.go"}); out != "read_file x.go" || err != nil {
		t.Fatalf("read_file = (%q, %v), want the post answered", out, err)
	}
}

// The no-ask branch pushes the input's handover as the brief: the
// mid-session note of the session moment rides SessionInput.Handover.
func TestRunSessionPushesTheInputHandoverAsTheBrief(t *testing.T) {
	runner, _, _, worker, _, _, _ := checkinRig(t, "")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m",
		Handover: "stop polishing the linter, commit the handler fix"}, nil,
		pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.Outcome != OutcomePushed || res.CheckIn != nil {
		t.Fatalf("result = (%+v, %v), want the plain pushed session", res, err)
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if !slices.Equal(worker.texts, []string{"POST /brief stop polishing the linter, commit the handler fix"}) {
		t.Fatalf("worker got %q, want the input's handover as the brief", worker.texts)
	}
}

// The statement of every check-in states the card's pace: 350 changed lines
// against the small card's 300, 1.80 of 5 EUR spent, session 1 of 4.
func TestRunSessionCheckinStatementsCarryThePace(t *testing.T) {
	t.Run("mid-session", func(t *testing.T) {
		runner, _, _, _, chat, store, _ := liveRig(t, CheckinResponse{Text: `{"decision":"continue","reason":"the rest is tests"}`})
		tk := store.tasks["t2"]
		tk.Size, tk.BudgetEUR, tk.SpentEUR = "small", 5, 1.8
		store.tasks["t2"] = tk
		version := testPolicyVersion()
		version.Document.Risk.LargeLines = 1000
		runner.cfg.Policy = func() policy.Version { return version }
		runner.cfg.PolicyVersion = func(context.Context, int64) (policy.Version, bool, error) { return version, true, nil }
		if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Sessions: 4, Model: "factory/big"}, nil,
			cardDiffLine(250, 100)+"\n"+pushedLine("c0ffee")+"\n"+resultLine(true, "done")); err != nil {
			t.Fatal(err)
		}
		if chat.calls != 1 {
			t.Fatalf("chat = %d calls, want one", chat.calls)
		}
		in := chat.gots[0].Input
		for _, want := range []string{"350 changed lines so far against its bound of 300; 1.80 EUR spent, 3.20 left;", "left; session 1 of 4."} {
			if !strings.Contains(in, want) {
				t.Fatalf("statement lacks %q:\n%s", want, in)
			}
		}
	})
	t.Run("start", func(t *testing.T) {
		runner, _, _, _, chat, _, _ := checkinRig(t, `{"decision":"handover","handover":"start from the tests"}`)
		if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Sessions: 4, Model: "m",
			CheckInAsk: &CheckInAsk{Moment: CheckInDead, Stop: "wall clock"}}, nil,
			pushedLine("c1")+"\n"+resultLine(true, "pushed")); err != nil {
			t.Fatal(err)
		}
		if chat.calls != 1 || !strings.Contains(chat.gots[0].Input, "Pace of the card: changed lines are not measured at this moment;") {
			t.Fatalf("chat = %d calls %+v, want the unmeasured pace", chat.calls, chat.gots)
		}
	})
}
