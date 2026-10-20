// Package acceptance holds the plan's acceptance tests, driven through
// factoryd's production seams: roadmap 188 (Stage 18 item 3.14), the lease
// admission waiting out a terminating namespace.
//
// The scenario is Stage 17's at 11:13:54Z: task t-old finished and teardown
// deleted its namespace; the API server still lists it Terminating while its
// contents are garbage-collected away — its RoleBinding gone, so even a
// policy read answers forbidden — and its commissioned server is still live.
// Task t-new, the next session, starts with its own lease. The scripted
// apiserver answers as the real one does, including Stage 17's line on a
// write, "unable to create new content in namespace ... because it is being
// terminated", and the runner talks to it through the production REST
// client. What the spec names for this round is what the test verifies: no
// 'forbidden' error logged, the active namespaces processed correctly, and
// the wait observable in factoryd's log.
package acceptance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const (
	nsPrefix        = "factory-agents-"
	factoryNS       = "factory"
	pullSecret      = "forgejo-registry"
	workerEgress    = "worker-egress"
	oldLease        = "203.0.113.7" // t-old's commissioned server, live while its namespace terminates
	newLease        = "203.0.113.8" // t-new's commissioned server, the next session's
	cnpCollection   = "/apis/cilium.io/v2/namespaces/"
	rbacCollections = "/apis/rbac.authorization.k8s.io/v1/namespaces/"
)

// policyYAML and processYAML are the minimal configuration tree of a valid
// policy version.
const policyYAML = `version: 1
budget:
  task_default_eur: 2
  task_max_eur: 5
  day_eur: 20
  month_eur: 50
limits: {sessions: 3, agent_time: 10m, supervisor_calls: 3, supervisor_timeout: 6m, stronger_after: 1}
classes:
  coder:
    session: code
    models: [small, big]
    tools: {}
    review_model: big
    supervisor_model: supervisor
prices:
  max_output_tokens: {small: 24576, big: 24576, supervisor: 18432, operator: 4096, pulse: 2048}
risk:
  classes:
    - {name: forbidden, outcome: forbidden}
    - {name: migration, outcome: requires_approval, migration: true}
    - {name: high, outcome: requires_approval}
    - {name: normal, outcome: requires_approval}
  rules:
    - {class: forbidden, paths: [".forgejo/**", ".gitea/**", ".github/**"]}
    - {class: migration, paths: ["internal/*/migrations/**", "internal/policy/schema.sql"]}
    - {class: high, paths: ["k8s/**", "**/Dockerfile*", "go.mod", "go.sum"]}
  default: normal
envelope_default: {monthly_eur: 10, server_types: [cx23], max_count: 2, max_lease_hours: 4, volume_max_gb: 20}
`

const processYAML = `lanes:
  - {name: backlog}
  - {name: ready, class: coder, moved_by: writers}
merge: {queue: none}
stages:
  - {name: checks, on_findings: agent, enabled: true}
  - {name: policy, on_findings: fail, enabled: true}
  - {name: review, on_findings: agent, enabled: true}
  - {name: approval, on_findings: human, enabled: true}
  - {name: merge, on_findings: human, enabled: true}
`

func testVersion() policy.Version {
	files := map[string][]byte{
		"policy.yaml":                []byte(policy.Header("policy.yaml") + "\n" + policyYAML),
		"processes/maintenance.yaml": []byte(policy.Header("processes/maintenance.yaml") + "\n" + processYAML),
	}
	t, err := policy.ParseTree(files)
	if err != nil {
		panic(err)
	}
	return policy.Version{ID: 7, Commit: "c7", Tree: t}
}

// registry is the LeaseReader seam: the commissioned servers each task
// holds, by task id.
type registry struct {
	mu   sync.Mutex
	byID map[string][]string
}

func (r *registry) LiveAddresses(context.Context) (map[string][]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string][]string{}
	for k, v := range r.byID {
		out[k] = append([]string(nil), v...)
	}
	return out, nil
}

func (r *registry) ClaimCommissioned(context.Context) ([]cloudres.Commissioned, error) {
	return nil, nil
}

var _ factoryd.LeaseReader = (*registry)(nil)

// apiNamespace mirrors one namespace as the scripted API server holds it.
type apiNamespace struct {
	labels            map[string]string
	deletionTimestamp string // metadata.deletionTimestamp once DELETE ran
	bound             bool   // factoryd's RoleBinding is in place
	pods              map[string]map[string]any
	cnps              map[string]map[string]any // policy name → live spec
	secrets           map[string]map[string]string
}

// apiserver scripts the Kubernetes API over HTTP as the real server answers
// during a namespace teardown: a Terminating namespace still matches its
// label selector and still lists with metadata.deletionTimestamp; every
// ciliumnetworkpolicies access there answers 403 forbidden — a read because
// the namespace's RoleBinding is already garbage-collected away, a write
// with Stage 17's message, "unable to create new content in namespace ...
// because it is being terminated".
type apiserver struct {
	mu         sync.Mutex
	namespaces map[string]*apiNamespace
	writes     []string // ns/name of every CNP apply that reached the store
	patches    []string // ns/name of every CNP PATCH received, in order
	deletes    []string // namespaces whose DELETE was answered
	pvs        []factoryd.PersistentVolume
	pvDeletes  []string
}

func newAPIServer() *apiserver {
	return &apiserver{namespaces: map[string]*apiNamespace{}}
}

func (a *apiserver) addNamespace(name string, labels map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.namespaces[name] = &apiNamespace{labels: labels, cnps: map[string]map[string]any{},
		pods: map[string]map[string]any{}, secrets: map[string]map[string]string{}}
}

// bind puts factoryd's RoleBinding into the namespace, as setupNamespace
// does before it writes anything there.
func (a *apiserver) bind(ns string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.namespaces[ns].bound = true
}

// putSecret seeds one namespace's secret, as platform applies the pull
// secret to the factory namespace.
func (a *apiserver) putSecret(ns, name string, data map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.namespaces[ns].secrets[name] = data
}

// terminate answers DELETE /api/v1/namespaces/<ns> with 200 and lets the
// garbage collector drain the namespace: it stays listed with
// metadata.deletionTimestamp, its RoleBinding is gone, and its contents go
// one by one.
func (a *apiserver) terminate(t *testing.T, ns string) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	live, ok := a.namespaces[ns]
	if !ok {
		t.Fatalf("namespace %s does not exist", ns)
	}
	live.deletionTimestamp = "2026-10-13T11:13:54Z"
	live.bound = false
	delete(live.pods, "worker-s1") // the session's pod went with the binding
	a.deletes = append(a.deletes, ns)
}

func (a *apiserver) setPVs(pvs []factoryd.PersistentVolume) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pvs = pvs
}

func (a *apiserver) egressSpec(t *testing.T, ns string) map[string]any {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	live, ok := a.namespaces[ns]
	if !ok {
		t.Fatalf("namespace %s does not exist", ns)
	}
	return live.cnps[workerEgress]
}

func (a *apiserver) count(list []string, prefix string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, item := range list {
		if strings.HasPrefix(item, prefix) {
			n++
		}
	}
	return n
}

func (a *apiserver) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		path := strings.TrimSuffix(r.URL.Path, "/")
		switch {
		case path == "/api/v1/namespaces" && r.Method == http.MethodGet:
			var items []any
			for name, ns := range a.namespaces {
				meta := map[string]any{"name": name, "labels": ns.labels}
				if ns.deletionTimestamp != "" {
					meta["deletionTimestamp"] = ns.deletionTimestamp
				}
				items = append(items, map[string]any{"metadata": meta})
			}
			writeJSON(w, map[string]any{"items": items})
		case path == "/api/v1/persistentvolumes" && r.Method == http.MethodGet:
			var items []any
			for _, pv := range a.pvs {
				items = append(items, map[string]any{
					"metadata": map[string]any{"name": pv.Name},
					"spec":     map[string]any{"storageClassName": pv.StorageClass},
					"status":   map[string]any{"phase": pv.Phase},
				})
			}
			writeJSON(w, map[string]any{"items": items})
		case strings.HasPrefix(path, "/api/v1/persistentvolumes/") && r.Method == http.MethodDelete:
			a.pvDeletes = append(a.pvDeletes, strings.TrimPrefix(path, "/api/v1/persistentvolumes/"))
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(path, cnpCollection):
			a.cnpPath(t, w, r, strings.TrimPrefix(path, cnpCollection))
		case strings.HasPrefix(path, rbacCollections):
			a.rbacPath(w, r, strings.TrimPrefix(path, rbacCollections))
		case strings.HasPrefix(path, "/api/v1/namespaces/"):
			a.namespacePath(t, w, r, strings.TrimPrefix(path, "/api/v1/namespaces/"))
		default:
			t.Errorf("unexpected request %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// forbid answers 403 as the API server does, with the Status body whose
// message the client reports.
func forbid(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "status": "Failure",
		"reason": "Forbidden", "code": 403, "message": message})
}

// cnpPath serves /apis/cilium.io/v2/namespaces/<ns>/ciliumnetworkpolicies
// [/<name>].
func (a *apiserver) cnpPath(t *testing.T, w http.ResponseWriter, r *http.Request, rest string) {
	nsName, sub, _ := strings.Cut(rest, "/")
	_, name, hasName := strings.Cut(sub, "ciliumnetworkpolicies/")
	ns, known := a.namespaces[nsName]
	if !known || !hasName {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if ns.deletionTimestamp != "" {
		// the RoleBinding went with the teardown's first GC pass: a read is
		// refused, and new content even more so — Stage 17's exact message
		a.patches = append(a.patches, nsName+"/"+name)
		forbid(w, fmt.Sprintf(`ciliumnetworkpolicies.cilium.io %q is forbidden: unable to create new content in namespace %s because it is being terminated`, name, nsName))
		return
	}
	if !ns.bound {
		forbid(w, fmt.Sprintf(`ciliumnetworkpolicies.cilium.io %q is forbidden`, name))
		return
	}
	switch r.Method {
	case http.MethodGet:
		spec, exists := ns.cnps[name]
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"spec": spec})
	case http.MethodPatch:
		var body struct {
			Spec map[string]any `json:"spec"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode apply body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ns.cnps[name] = body.Spec
		a.writes = append(a.writes, nsName+"/"+name)
		writeJSON(w, map[string]any{"spec": body.Spec})
	default:
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// rbacPath serves the rolebindings collection: factoryd's binding is what
// admits its own reads and writes inside a task namespace, and the garbage
// collector takes it away first on a teardown.
func (a *apiserver) rbacPath(w http.ResponseWriter, r *http.Request, rest string) {
	nsName, sub, _ := strings.Cut(rest, "/")
	col, name, hasName := strings.Cut(sub, "/")
	if col != "rolebindings" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	ns, known := a.namespaces[nsName]
	if !known {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !hasName {
		if r.Method == http.MethodPost {
			var body struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Metadata.Name == "factoryd" {
				ns.bound = true
			}
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if name != "factoryd" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch {
	case r.Method == http.MethodGet && ns.bound:
		writeJSON(w, map[string]any{"metadata": map[string]any{"name": "factoryd"}})
	case r.Method == http.MethodGet:
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodPost:
		io.Copy(io.Discard, r.Body)
		ns.bound = true
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// namespacePath serves /api/v1/namespaces/<ns>/... : pods, secrets and the
// plain collections setupNamespace posts.
func (a *apiserver) namespacePath(t *testing.T, w http.ResponseWriter, r *http.Request, rest string) {
	nsName, sub, _ := strings.Cut(rest, "/")
	ns, known := a.namespaces[nsName]
	if !known {
		if r.Method == http.MethodPost && sub == "" {
			var body struct {
				Metadata struct {
					Name   string            `json:"name"`
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
			}
			io.Copy(io.Discard, r.Body)
			if body.Metadata.Name == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if _, exists := a.namespaces[body.Metadata.Name]; exists {
				w.WriteHeader(http.StatusConflict)
				return
			}
			a.namespaces[body.Metadata.Name] = &apiNamespace{labels: body.Metadata.Labels,
				cnps: map[string]map[string]any{}, pods: map[string]map[string]any{},
				secrets: map[string]map[string]string{}}
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method == http.MethodDelete && sub == "" {
		ns.deletionTimestamp = "2026-10-13T11:13:54Z"
		ns.bound = false
		delete(ns.pods, "worker-s1")
		a.deletes = append(a.deletes, nsName)
		w.WriteHeader(http.StatusOK)
		return
	}
	col, item, _ := strings.Cut(sub, "/")
	switch col {
	case "pods":
		a.podPath(t, w, r, ns, item)
	case "secrets":
		a.secretPath(t, w, r, ns, item)
	case "serviceaccounts", "resourcequotas", "limitranges", "persistentvolumeclaims":
		if r.Method == http.MethodPost {
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusCreated)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	default:
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (a *apiserver) secretPath(t *testing.T, w http.ResponseWriter, r *http.Request, ns *apiNamespace, item string) {
	name, _, _ := strings.Cut(item, "/")
	switch r.Method {
	case http.MethodGet:
		data, ok := ns.secrets[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		encoded := map[string]string{}
		for k, v := range data {
			encoded[k] = base64.StdEncoding.EncodeToString([]byte(v))
		}
		writeJSON(w, map[string]any{"data": encoded})
	case http.MethodPost:
		var body struct {
			StringData map[string]string `json:"stringData"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode secret: %v", err)
		}
		ns.secrets[name] = body.StringData
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// podPath serves one namespace's pod collection and items.
func (a *apiserver) podPath(t *testing.T, w http.ResponseWriter, r *http.Request, ns *apiNamespace, item string) {
	name, action, _ := strings.Cut(item, "/")
	if name == "" {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected request %s pods", r.Method)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode pod: %v", err)
		}
		meta, _ := body["metadata"].(map[string]any)
		podName, _ := meta["name"].(string)
		if podName == "" {
			t.Error("pod created without a name")
		}
		ns.pods[podName] = body
		w.WriteHeader(http.StatusCreated)
		return
	}
	if action == "log" {
		w.Write([]byte("worker\n"))
		return
	}
	_, ok := ns.pods[name]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		delete(ns.pods, name)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		writeJSON(w, runningPodBody())
	default:
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// runningPodBody is a Running pod: enough for PodStatus, PodIP,
// PodInitFinished and PodDiagnosis to answer without an error.
func runningPodBody() map[string]any {
	return map[string]any{
		"status": map[string]any{
			"phase": "Running",
			"podIP": "10.42.0.9",
			"initContainerStatuses": []any{map[string]any{
				"state": map[string]any{"terminated": map[string]any{
					"exitCode": 0, "message": "ready", "finishedAt": "2026-10-13T11:13:54Z",
				}},
			}},
			"containerStatuses": []any{map[string]any{"name": "worker",
				"state": map[string]any{"running": map[string]any{}}}},
		},
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// memObjects is the ObjectStore seam behind the task timeline.
type memObjects struct {
	mu   sync.Mutex
	puts map[string][]byte
}

func (o *memObjects) Put(_ context.Context, key string, body []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.puts == nil {
		o.puts = map[string][]byte{}
	}
	o.puts[key] = append([]byte(nil), body...)
	return nil
}

func (o *memObjects) Get(_ context.Context, key string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.puts[key], nil
}

func (o *memObjects) Delete(_ context.Context, key string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.puts, key)
	return nil
}

var _ factoryd.ObjectStore = (*memObjects)(nil)

// noopWorkflows satisfies the workflow seam without a Temporal server.
type noopWorkflows struct{}

func (noopWorkflows) Start(context.Context, string) error { return nil }

func (noopWorkflows) StartRoutineRun(context.Context, factoryd.RoutineRunInput) error { return nil }

func (noopWorkflows) Signal(context.Context, string, string, factoryd.TaskSignal) error { return nil }

// syncBuffer collects factoryd's log while the test runs.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harness wires factoryd's Runner against the scripted apiserver through
// the production REST client, with the log captured, so the test observes
// what factoryd's log shows this round.
type harness struct {
	runner  *factoryd.Runner
	api     *apiserver
	logs    *syncBuffer
	objects *memObjects
}

func newHarness(t *testing.T, reg *registry) *harness {
	t.Helper()
	api := newAPIServer()
	srv := httptest.NewTLSServer(api.handler(t))
	t.Cleanup(srv.Close)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)
	t.Setenv("FACTORYD_KUBE_INSECURE", "1")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	var pemBuf strings.Builder
	for _, c := range srv.TLS.Certificates {
		for _, raw := range c.Certificate {
			pem.Encode(&pemBuf, &pem.Block{Type: "CERTIFICATE", Bytes: raw})
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte(pemBuf.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	// the production client re-reads the pod's service-account token from
	// this directory per request; the acceptance run points it at a stub
	old := factoryd.SaDirForTest(dir)
	t.Cleanup(func() { factoryd.SaDirForTest(old) })
	kube, err := factoryd.NewInClusterKube()
	if err != nil {
		t.Fatal(err)
	}
	v := testVersion()
	cfg := factoryd.RunnerConfig{
		AgentNamespacePrefix: nsPrefix,
		WorkerImage:          "reg/worker@sha256:abc",
		PullSecret:           pullSecret,
		FactoryNamespace:     factoryNS,
		GatewayURL:           "http://factory-gateway.factory.svc.cluster.local:8081",
		GitBaseURL:           "http://forgejo-http.forgejo.svc.cluster.local:3000",
		Models:               factoryd.ModelLadder{Start: "factory/small", Stronger: "factory/big"},
		AnswerTokens:         8192,
		HeadroomTokens:       16384,
		ChecksPoll:           time.Millisecond,
		EgressCanary:         "example.com:443",
		EgressDirectIP:       "192.0.2.7:443",
		Supervisor:           factoryd.SupervisorConfig{NoProgressAfter: time.Minute, RepetitionEvents: 24},
		Policy:               func() policy.Version { return v },
		PolicyVersion: func(_ context.Context, id int64) (policy.Version, bool, error) {
			return v, id == v.ID, nil
		},
		Leases:      reg,
		Gates:       &factoryd.GateWatch{},
		LastFailure: func(context.Context, string, string) (string, error) { return "", nil },
		Workflows:   noopWorkflows{},
	}
	objects := &memObjects{}
	logs := &syncBuffer{}
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(logs)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })
	r, err := factoryd.NewRunner(cfg, nil, kube, nil, nil, objects, &factoryd.SupervisorModel{})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return &harness{runner: r, api: api, logs: logs, objects: objects}
}

// TestLeaseAdmissionWaitsOutTerminatingNamespace is plan #511's acceptance
// item (card 3 of 3): a previous session's namespace is being deleted while
// a new session starts. AdmitLeases logs no 'forbidden' error, the active
// namespace's egress policy is applied, and the wait out of the terminating
// namespace is observable in factoryd's log.
func TestLeaseAdmissionWaitsOutTerminatingNamespace(t *testing.T) {
	reg := &registry{byID: map[string][]string{
		"t-old": {oldLease}, // its server has not expired yet
		"t-new": {newLease}, // the next session's commissioned server
	}}
	h := newHarness(t, reg)
	ctx := context.Background()

	// the previous session's world: its namespace, bound, holding its
	// session's pod and its egress policy with its leased server
	h.api.addNamespace(factoryNS, map[string]string{"app.kubernetes.io/name": "factoryd"})
	h.api.putSecret(factoryNS, pullSecret, map[string]string{".dockerconfigjson": "{}"})
	h.api.addNamespace(nsPrefix+"t-old", map[string]string{"factory.ai/task": "t-old"})
	h.runner.BindTaskNamespace(ctx, nsPrefix+"t-old")
	if err := h.runner.AdmitLeases(ctx); err != nil {
		t.Fatalf("first AdmitLeases = %v, want nil", err)
	}
	if got := h.api.egressSpec(t, nsPrefix+"t-old"); got == nil {
		t.Fatalf("t-old's egress policy was never applied: %s", h.logs.String())
	}

	// teardown: task t-old finished, DeleteNamespace returned, and the
	// garbage collector drains the namespace — its RoleBinding is gone, so
	// even a policy read there answers forbidden
	h.api.terminate(t, nsPrefix+"t-old")

	// the next session starts: task t-new's namespace is set up as
	// setupNamespace would, and its lease joins the registry
	h.api.addNamespace(nsPrefix+"t-new", map[string]string{"factory.ai/task": "t-new"})
	h.runner.BindTaskNamespace(ctx, nsPrefix+"t-new")

	// 1. AdmitLeases carries no error out of the pass and logs no
	// 'forbidden' error for the terminating namespace — Stage 17's line
	// "lease admission: ... kubernetes: forbidden: PATCH ..." must not
	// appear this round.
	if err := h.runner.AdmitLeases(ctx); err != nil {
		t.Fatalf("AdmitLeases = %v, want nil: the pass must not carry an error for the terminating namespace", err)
	}
	if got := h.logs.String(); strings.Contains(got, "forbidden") {
		t.Fatalf("factoryd's log carries a forbidden error:\n%s", got)
	}

	// 2. the active namespaces are still processed correctly: t-new's
	// policy is applied and admits only its own lease.
	if got := h.api.egressSpec(t, nsPrefix+"t-new"); got == nil {
		t.Fatalf("%s/%s was never applied: %s", nsPrefix+"t-new", workerEgress, h.logs.String())
	} else if s := fmt.Sprint(got["egress"]); !strings.Contains(s, newLease+"/32") || strings.Contains(s, oldLease) {
		t.Fatalf("t-new's egress = %s, want only %s/32", s, newLease)
	}

	// 3. the terminating namespace needs no policy any more: the pass waits
	// it out instead of writing into it — no PATCH reached it at all.
	if n := h.api.count(h.api.patches, nsPrefix+"t-old/"); n != 0 {
		t.Fatalf("%d policy accesses attempted in the terminating namespace, want 0", n)
	}

	// A second pass repairs nothing, still attempts nothing in the
	// terminating namespace and logs no forbidden line.
	if err := h.runner.AdmitLeases(ctx); err != nil {
		t.Fatalf("second AdmitLeases = %v, want nil", err)
	}
	if n := h.api.count(h.api.patches, nsPrefix+"t-old/"); n != 0 {
		t.Fatalf("the second pass touched the terminating namespace %d times, want 0", n)
	}
	if got := h.logs.String(); strings.Contains(got, "forbidden") {
		t.Fatalf("the second pass logged a forbidden error:\n%s", got)
	}

	// The wait is observable in factoryd's log: the lease admission records
	// it on the terminating task's timeline, naming the namespace it waited
	// out. AdmitLeases above already appended the namespace_wait step for
	// every round that found t-old terminating — two rounds so far.
	body, err := h.objects.Get(ctx, "sessions/t-old/timeline.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) < 2 {
		t.Fatalf("t-old's timeline carries %d entries, want at least one per waiting round:\n%s", len(lines), body)
	}
	for _, line := range lines {
		for _, want := range []string{`"step":"namespace_wait"`, `"namespace":"` + nsPrefix + `t-old"`, `"waited":"terminating"`} {
			if !strings.Contains(line, want) {
				t.Fatalf("t-old's timeline does not show the waited-out namespace (%s): %s", want, line)
			}
		}
	}
	if n := h.api.count(h.api.patches, nsPrefix+"t-old/"); n != 0 {
		t.Fatalf("the wait wrote into the terminating namespace %d times, want 0", n)
	}
}
