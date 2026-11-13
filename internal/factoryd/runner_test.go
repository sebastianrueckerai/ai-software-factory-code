package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
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
	nodes        []NodeStorage                // the cluster's nodes; nil is one node nothing fills
	unplaced     []int64                      // requests of pods no node holds
	nodesErr     error                        // injected NodeStorage failure
	nodeReads    int                          // NodeStorage calls
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
	events      map[string]string         // ns/pod → its newest event
	cronJobsErr map[string]error          // "<kind> <namespace>" → injected failure of its read
	tokenTaken  chan struct{}             // closed when a worker endpoint took a push token
	takeToken   sync.Once
	nextLogs    []string // the log bodies the next created pods take at their creation, in order
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
	ns, name string
	phase    string
	// logEnded is a phase Succeeded set by the scripted log's end: that
	// log ends at once, before the runner has handled its lines, while a
	// live pod's container runs on as they are read, so PodIP still
	// answers for it
	logEnded        bool
	reason, message string // status.reason, status.message (Evicted's)
	logs            string
	logsReady       chan struct{} // closed when the body is assigned
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
	for key, p := range k.pods {
		if strings.HasPrefix(key, name+"/") {
			delete(k.pods, key)
			if !p.deleted {
				k.deletedPods = append(k.deletedPods, key)
			}
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

func (k *fakeKube) NodeStorage(context.Context) ([]NodeStorage, []int64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.nodeReads++
	if k.nodes == nil && k.nodesErr == nil {
		return []NodeStorage{{Name: "n1", Allocatable: 1 << 50}}, nil, nil
	}
	return slices.Clone(k.nodes), slices.Clone(k.unplaced), k.nodesErr
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
	if len(k.nextLogs) > 0 {
		p.script(k.nextLogs[0])
		k.nextLogs = k.nextLogs[1:]
	}
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
	// a worker whose log is cut, goes on after its SIGTERM or is not written
	// yet is still inside its grace period: the pod stays until the rest of
	// its log has been read
	if (!logsWritten(p) || strings.Contains(p.logs, cutLogs) || strings.Contains(p.logs, sigtermLogs)) && p.phase == "Running" {
		p.deleted = true
		return nil
	}
	delete(k.pods, namespace+"/"+name)
	return nil
}

// logsWritten says p's scripted log is written.
func logsWritten(p *fakePod) bool {
	select {
	case <-p.logsReady:
		return true
	default:
		return false
	}
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

// PodIP answers loopback for every existing pod that has not ended (as
// logEnded says): a test's worker endpoint listens there.
func (k *fakeKube) PodIP(ctx context.Context, namespace, name string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	p, ok := k.pods[namespace+"/"+name]
	if !ok {
		return "", ErrNotFound
	}
	if err := podEnded(namespace, name, p.phase); err != nil && !p.logEnded {
		return "", err
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

func (k *fakeKube) NewestPodEvent(ctx context.Context, namespace, name string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.events[namespace+"/"+name], nil
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
		p.phase, p.logEnded = "Succeeded", true
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
		k.podOrder[idx].script(body)
	}
}

// script assigns p's log body and unblocks its log streams.
func (p *fakePod) script(body string) {
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
	branchHead  string            // the task branch's head; empty = no branch yet
	committers  map[string]string // commit → its committer email; unlisted commits are a worker's
	parents     map[string]string // commit → its first parent; unlisted commits are root commits
	mainHead    string            // the task repository's main head
	comments    []string
	failWrites  int    // write-token mints to answer 502 before succeeding
	prFiles     string // a pull request's files, as Forgejo lists them
	// pulls are GET /pulls/{n} bodies, unlisted: 404; pullStatus answers
	// a number with its status instead
	pulls      map[int64]string
	pullStatus map[int64]int
	// branches are the heads of the fork's branches by name, which answer
	// before branchHead
	branches map[string]string
	// files are the raw files by "<ref>:<path>", unlisted: 404
	files map[string]string
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
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/raw/"):
			_, file, _ := strings.Cut(r.URL.Path, "/raw/")
			body, ok := h.files[r.URL.Query().Get("ref")+":"+file]
			if !ok {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, body)
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
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/git/commits/"):
			_, sha, _ := strings.Cut(r.URL.Path, "/git/commits/")
			parents := []map[string]string{}
			if p := h.parents[sha]; p != "" {
				parents = append(parents, map[string]string{"sha": p})
			}
			json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"committer": map[string]string{"email": cmp.Or(h.committers[sha], "factory-agent@factory.local")}}, "parents": parents})
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/branches/"):
			if strings.HasPrefix(r.URL.Path, "/api/v1/repos/sf/") && strings.HasSuffix(r.URL.Path, "/branches/main") {
				fmt.Fprintf(w, `{"commit":{"id":%q}}`, h.mainHead)
				return
			}
			// task branches live on the fork only
			_, name, _ := strings.Cut(r.URL.Path, "/branches/")
			if head, ok := h.branches[name]; ok && strings.HasPrefix(r.URL.Path, "/api/v1/repos/factory-agent/") {
				fmt.Fprintf(w, `{"commit":{"id":%q}}`, head)
				return
			}
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
		Supervisor:           SupervisorConfig{RepetitionEvents: 24},
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
// setDetectors sets the supervisor detectors, in seconds, of the version r
// runs.
func setDetectors(r *Runner, seconds map[string]int) {
	v := r.cfg.Policy()
	v.Document.Supervisor = seconds
	r.cfg.Policy = func() policy.Version { return v }
}

func setLimits(r *Runner, change func(*policy.Limits)) {
	v := r.cfg.Policy()
	change(&v.Document.Limits)
	r.cfg.Policy = func() policy.Version { return v }
}

// testPolicyVersion is version 7 of testPolicyYAML.
func testPolicyVersion() policy.Version {
	files := testFiles(testPolicyYAML)
	files["projects/repo.yaml"] = []byte(policy.Header("projects/repo.yaml") +
		"\nrepository: sf/repo\nmerge_rule: human\nobjects: seed\nagents: none\ntokens: {read: forgejo-config}\nchain: {mode: off}\n")
	tree, err := policy.ParseTree(files)
	if err != nil {
		panic(err)
	}
	return policy.Version{ID: 7, Commit: "c7", Tree: tree}
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

// unexpectedSupervisor is a supervisor model whose gateway reports every
// call, with the source that names a stray: the request line, the
// User-Agent, the remote address and the first 12 characters of the
// bearer token.
func unexpectedSupervisor(t *testing.T, report func(format string, args ...any)) *SupervisorModel {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		report("unexpected supervisor call %s %s from %s, User-Agent %q, token %q",
			r.Method, r.URL.Path, r.RemoteAddr, r.UserAgent(), token[:min(12, len(token))])
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return testSupervisorModel(srv.URL)
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
	sup := unexpectedSupervisor(t, t.Errorf)
	runner, err := NewRunner(testRunnerConfig(), store, kube, completion.Forgejo, completion, objects, sup)
	if err != nil {
		t.Fatal(err)
	}
	runner.stopWait = 500 * time.Millisecond
	runner.endpointWait = 50 * time.Millisecond
	runner.newOwner = func() string { return "own" }
	return runner, hub, store
}

// runSession executes the RunSession activity in a Temporal test activity
// environment rooted at ctx (nil: background), with heartbeat details from
// a previous attempt when dead is set; the pods the session creates take
// bodies as their logs at their creation, in order.
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
	kube := r.kube.(*fakeKube)
	kube.mu.Lock()
	kube.nextLogs = bodies
	kube.mu.Unlock()
	defer func() {
		kube.mu.Lock()
		kube.nextLogs = nil
		kube.mu.Unlock()
	}()
	var res SessionResult
	val, err := env.ExecuteActivity(ActivityRunSession, in)
	if err == nil {
		err = val.Get(&res)
	}
	return res, err
}

// A session's pod holds its scripted log from its creation: no scheduling
// of the test's goroutine can leave the session on an unwritten log until
// its wall-clock cap.
func TestRunSessionPodHoldsItsScriptedLogFromItsCreation(t *testing.T) {
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), kube, &fakeObjectStore{})
	(&fakeWorkerEndpoint{status: http.StatusNoContent}).start(t, runner)
	written := false
	kube.onCreatePod = func() { written = logsWritten(kube.podOrder[len(kube.podOrder)-1]) }
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if !written {
		t.Fatal("the session's pod was created without its scripted log")
	}
	if err != nil || res.Outcome != OutcomePushed || res.Commit != "c1" {
		t.Fatalf("result = (%+v, %v), want the scripted push", res, err)
	}
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
		!slices.Contains(hub.calls, "mint task-t1-push-own write:repository factory-agent/ai-software-factory-code") || len(hub.minted()) != 2 {
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
			t.Fatalf("DNS rule lacks %s (the sandbox carries the Go toolchain): %v", name, dnsRule)
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
		"proofs": {r.proofsPodSpec(task, Repo{Owner: "linkshort", Name: "linkshort"}, pullRef(1), "c1", "", "bearer-x", proofsBudget),
			"reg/worker@sha256:abc", []string{"harness", "fetch"}},
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

// A session's compaction (the worker's own threshold, roadmap 239) leaves
// one timeline row naming the session and one event in the session's
// history: the bus carries no token figures, so the row is the audit
// fact alone.
func TestRunSessionRecordsTheCompaction(t *testing.T) {
	objects := &fakeObjectStore{}
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), objects)
	(&fakeWorkerEndpoint{status: http.StatusNoContent}).start(t, runner)
	at := time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)
	logs := envLine(harness.Event{Kind: harness.EventCompacted, Time: at}) + "\n" + resultLine(true, "pushed")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	tl, _ := objects.Get(t.Context(), "sessions/t2/timeline.jsonl")
	for _, want := range []string{`"event":"compacted"`, `"session":"` + res.SessionID + `"`} {
		if !strings.Contains(string(tl), want) {
			t.Errorf("timeline lacks %s:\n%s", want, tl)
		}
	}
	hist, err := objects.Get(t.Context(), "sessions/t2/"+res.SessionID+".jsonl")
	if err != nil || !strings.Contains(string(hist), `"kind":"compacted"`) {
		t.Errorf("session history = %s (%v), want the compacted event", hist, err)
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
// scheduled, image pull stuck) left no record and is not the parent: its
// replacement keeps the input's; only a session that ran adds the
// interruption finding.
func TestRunSessionRetryKeepsTheParentPastADeadSessionThatNeverRan(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "long", Repository: "sf/repo", SHA: "base", Track: "direct"}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, task, kube, &fakeObjectStore{})
	dead := &sessionProgress{SessionID: "s-dead"}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small", Findings: []string{"earlier note"},
		ParentSessionID: "s-0"}, dead, pushedLine("c2")+"\n"+resultLine(true, "pushed"))
	if err != nil {
		t.Fatal(err)
	}
	env := podEnv(kube.podAt(0))
	if env["PARENT_SESSION_ID"] != "s-0" {
		t.Fatalf("PARENT_SESSION_ID = %q, want the input's s-0", env["PARENT_SESSION_ID"])
	}
	var findings []string
	if err := json.Unmarshal([]byte(env["FINDINGS"]), &findings); err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0] != "earlier note" {
		t.Fatalf("findings = %q, want the input only: nothing ran, nothing was interrupted", findings)
	}
	if sess := store.sessions[res.SessionID]; sess.ParentSessionID != "s-0" {
		t.Fatalf("session row = %+v, want the input's parent s-0", sess)
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
	if texts := withoutWipAsks(worker.texts); !slices.Equal(texts, []string{"POST /brief "}) {
		t.Fatalf("worker got %q, want only the start brief: a handover pushes nothing to the pod", texts)
	}
	// the workflow writes the check-in's step for a riding outcome; the
	// runner writes none
	if tl := objGet(objects, "sessions/t2/timeline.jsonl"); strings.Contains(string(tl), `"step":"checkin"`) {
		t.Fatalf("timeline = %s, want no runner-side check-in step", tl)
	}
}

// The in-session check-in's transcript tools read the live session's own
// record: a search over the trip's events answers the line it found in
// the next round of the conversation. Its card_facts reads the session's
// own card.
func TestRunSessionCheckinToolsReadTheLiveRecord(t *testing.T) {
	runner, _, _, _, chat, _, _ := liveRig(t, CheckinResponse{})
	chat.answers = []CheckinResponse{
		{ToolName: "transcript_search", ToolArgs: map[string]any{"query": "loopcheck: FAIL at 10:00:01"}},
		{ToolName: "card_facts"},
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
	if chat.calls != 3 {
		t.Fatalf("chat calls = %d, want the two tool rounds and the decision", chat.calls)
	}
	// the tool's answer is the conversation's last appended piece: on an
	// empty parent the search returns nothing and the round ends at the
	// marker
	_, toolOut, _ := strings.Cut(chat.gots[1].Input, "answered:\n")
	if !strings.Contains(toolOut, "loopcheck: FAIL at 10:00:01") {
		t.Fatalf("tool round ends with %q, want the live record's line the search found", toolOut)
	}
	if _, facts, _ := strings.Cut(chat.gots[2].Input, "tool card_facts null answered:\n"); !strings.HasPrefix(facts, "#7 · ") {
		t.Errorf("card_facts answered %q, want card #7's digest first", facts)
	}
}

// The check-in's human ask inside the live session ends it the way the
// gate's does: the record rides the result, and no conversation-time text
// is invented for the human.
func TestRunSessionCheckinHumanEndsTheSession(t *testing.T) {
	runner, hub, _, _, chat, _, _ := liveRig(t, CheckinResponse{Text: `{"decision":"hand_up","reason_kind":"goal_conflict","reason":"only a human can pick the license"}`})

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
	if chat.calls != 2 || !strings.Contains(chat.gots[1].Input, "run is not a tool of this conversation; its tools are transcript_search, transcript_read, card_facts, card_step, gates, read_file, grep, git_log and answer") {
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

// A fresh session the card's own money cannot open is refused before
// anything opens: no token, no session row, no pod. The card gets the
// floor's refusal as its money ask, and no session burns the rest of its
// budget on calls the gateway must refuse.
// 0.10 EUR of 2 is free, -0.30 under the ceiling with the 0.40 gate
// reserve held out, of which 0.10 is what the gates keep; the cheap rung's
// floor asks 0.30.
func TestRunSessionRefusesAnOpeningTheBudgetCannotPay(t *testing.T) {
	task := Task{ID: "t4k", Status: "running", Objective: "fix vet", Repository: "sf/repo", SHA: "base", Track: "direct", BudgetEUR: 2, SpentEUR: 1.9}
	kube := newFakeKube()
	runner, hub, store := newTestRunner(t, task, kube, &fakeObjectStore{})
	v := testPolicyVersion()
	v.Tree.Document.Budget.SessionFloorCheapEUR = 0.3
	runner.cfg.Policy = func() policy.Version { return v }
	runner.cfg.Prices = stubPrices{price: policy.Price{In: 4e-6, CacheRead: 4e-7, Out: 4e-6, MaxOutput: 32768}}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t4k", Attempt: 1, Model: "factory/small"}, nil, pushedLine("c2")+"\n"+resultLine(true, "pushed"))
	why := "the remaining budget cannot fund a call: Raise the budget (Fund on the board) and answer, or close the card.\n" +
		"The card spent 1.90 EUR of its 2.00 EUR budget; 0.10 EUR is held for the review and the audit, which need 0.40 EUR" +
		", and a session on factory/small needs 0.300000 EUR " +
		"(the floor of its rung, budget.session_floor_cheap_eur in policy.yaml), so a fund of 0.60 EUR opens it; a session started now would run out of money before its work is done."
	if err != nil || res.Outcome != OutcomeEscalate || !res.StartFailed || res.Summary != why || res.Reason != why || res.Model != "factory/small" {
		t.Fatalf("result = (%+v, %v), want the money ask:\n%s", res, err, why)
	}
	if !strings.Contains(res.Reason, "(Fund on the board) and answer") {
		t.Fatalf("reason = %q, want the Fund action", res.Reason)
	}
	if got := escalationSource(res.Reason); got != "budget" {
		t.Fatalf("escalation source = %q, want budget", got)
	}
	if len(store.sessions) != 0 || len(kube.podOrder) != 0 || slices.ContainsFunc(hub.calls, func(c string) bool { return strings.HasPrefix(c, "mint ") }) {
		t.Fatalf("sessions %v, pods %d, forgejo calls %v; want nothing opened", store.sessions, len(kube.podOrder), hub.calls)
	}
}

// A factory with no price source opens every session, a card whose
// budget is spent included: no floor can be read.
func TestRunSessionOpensWhenNoPricesAreConfigured(t *testing.T) {
	task := Task{ID: "t4l", Status: "running", Objective: "fix vet", Repository: "sf/repo", SHA: "base", Track: "direct", BudgetEUR: 2, SpentEUR: 2}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, task, kube, &fakeObjectStore{})
	runner.cfg.Prices = nil
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t4l", Attempt: 1, Model: "factory/small"}, nil, pushedLine("c2")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.Outcome != OutcomePushed || res.StartFailed || len(kube.podOrder) != 1 || len(store.sessions) != 1 {
		t.Fatalf("result = (%+v, %v), pods %d, sessions %d; want the session opened and pushed", res, err, len(kube.podOrder), len(store.sessions))
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

// A session whose pod never left Pending ends not_started,
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
		setDetectors(runner, map[string]int{policy.DetectorNoProgress: 3})
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
			pinned, current := testPolicyVersion(), testPolicyVersion()
			pinned.Document.Supervisor = map[string]int{policy.DetectorNoProgress: 3}
			current.Document.Supervisor = pinned.Document.Supervisor
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
	setDetectors(runner, map[string]int{policy.DetectorNoProgress: 3})
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

// takeoverStore plays the next attempt. Once the session's row is written
// its context ends (the stale attempt). Where the newer attempt acts: at the
// write ("put": it took the row over and minted a push token), or in the
// window of the stale attempt's cleanup, at its first read or release of the
// row ("race": the takeover lands after the read, before the delete) or
// after the newer attempt finished ("gone": it deleted its own row).
type takeoverStore struct {
	Store
	hub    *forgejoHub
	cancel func()
	when   string
	// ended: the stale attempt's context has ended
	ended, done bool
}

func (s *takeoverStore) newer(l LiveSession, deleteRow bool) {
	s.done = true
	s.hub.mu.Lock()
	s.hub.tokens = append(s.hub.tokens, TokenRef{ID: 9, Name: PushTokenName(l.TaskID, "newer")})
	s.hub.mu.Unlock()
	if deleteRow {
		s.Store.DeleteLiveSession(context.Background(), l.TaskID)
		return
	}
	l.Owner = "newer"
	s.Store.PutLiveSession(context.Background(), l)
}

func (s *takeoverStore) PutLiveSession(ctx context.Context, l LiveSession) error {
	if l.Owner == "newer" || s.done {
		return s.Store.PutLiveSession(ctx, l)
	}
	if err := s.Store.PutLiveSession(ctx, l); err != nil {
		return err
	}
	if s.when == "put" || s.when == "supervise" {
		s.newer(l, false)
	}
	s.ended = true
	if s.when != "supervise" {
		s.cancel()
	}
	return nil
}

func (s *takeoverStore) LiveSession(ctx context.Context, taskID string) (LiveSession, bool, error) {
	if s.ended && !s.done && s.when == "gone" {
		l, _, _ := s.Store.LiveSession(ctx, taskID)
		s.newer(l, true)
	}
	l, ok, err := s.Store.LiveSession(ctx, taskID)
	if s.ended && !s.done && s.when == "race" {
		s.newer(l, false)
	}
	return l, ok, err
}

func (s *takeoverStore) ReleaseLiveSession(ctx context.Context, taskID, owner string) (bool, error) {
	if s.ended && !s.done && s.when != "put" {
		l, _, _ := s.Store.LiveSession(ctx, taskID)
		s.newer(l, s.when == "gone")
	}
	return s.Store.ReleaseLiveSession(ctx, taskID, owner)
}

// The stale attempt is not cancelled for a while after the takeover: it
// stops supervising on its own at its next tick and leaves the session to
// the newer attempt, with its pod, namespace and tokens, and answers no
// push request.
func TestStaleAttemptStopsSupervisingAfterATakeover(t *testing.T) {
	task := Task{ID: "t6", Status: "running", Objective: "long", Repository: "sf/repo", Track: "direct"}
	kube := newFakeKube()
	runner, hub, store := newTestRunner(t, task, kube, &fakeObjectStore{})
	runner.store = &takeoverStore{Store: store, hub: hub, cancel: func() {}, when: "supervise"}
	runner.tick = 20 * time.Millisecond
	// the pod asks for its push token after the takeover: the stale attempt
	// mints none
	_, err := runSession(t, nil, runner, SessionInput{TaskID: "t6", Attempt: 1, Model: "m"}, nil, pushRequestLine("c1")+"\n"+hangLogs)
	var app *temporal.ApplicationError
	if !errors.As(err, &app) || app.Message() != errSuperseded.Error() {
		t.Fatalf("err = %v, want the session left to the newer attempt", err)
	}
	if len(kube.deletedPods) != 0 || len(hub.revoked) != 0 || len(writeMints(hub)) != 0 {
		t.Fatalf("deleted pods %v, revoked %v, write mints %v; want the newer attempt's session untouched", kube.deletedPods, hub.revoked, writeMints(hub))
	}
	if l, ok, _ := store.LiveSession(context.Background(), "t6"); !ok || l.Owner != "newer" {
		t.Fatalf("live session = %+v, %v; want the newer attempt's row", l, ok)
	}
}

// An attempt Temporal timed out keeps running while the next attempt
// reattaches to the same pod: when the stale attempt's context ends, its
// cleanup must leave the newer attempt's pod, namespace and push token
// alone, whenever the newer attempt acts: before the cleanup, between its
// read and its delete, or after the newer attempt ended.
func TestStaleAttemptCleanupLeavesTheNewerAttemptsSession(t *testing.T) {
	for _, when := range []string{"put", "race", "gone"} {
		t.Run(when, func(t *testing.T) {
			task := Task{ID: "t6", Status: "running", Objective: "long", Repository: "sf/repo", Track: "direct"}
			kube := newFakeKube()
			runner, hub, store := newTestRunner(t, task, kube, &fakeObjectStore{})
			ctx, cancel := context.WithCancel(context.Background())
			runner.store = &takeoverStore{Store: store, hub: hub, cancel: cancel, when: when}
			if _, err := runSession(t, ctx, runner, SessionInput{TaskID: "t6", Attempt: 1, Model: "m"}, nil, hangLogs); err == nil {
				t.Fatal("the stale attempt ended without its cancel")
			}
			hub.mu.Lock()
			revoked := slices.Clone(hub.revoked)
			hub.mu.Unlock()
			if len(revoked) != 0 {
				t.Fatalf("revoked = %v, want the newer attempt's tokens alone", revoked)
			}
			if len(kube.deletedPods) != 0 {
				t.Fatalf("deleted pods = %v, want the newer attempt's pod alone", kube.deletedPods)
			}
			if _, ok := kube.namespaces["factory-agents-t6"]; !ok {
				t.Fatal("the stale attempt deleted the newer attempt's namespace")
			}
			l, ok, _ := store.LiveSession(context.Background(), "t6")
			if (when == "gone") == ok || ok && l.Owner != "newer" {
				t.Fatalf("live session = %+v, %v; want the newer attempt's row (none once it ended)", l, ok)
			}
		})
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

// A pod that finished while factoryd was stopping still holds the result
// the old factoryd had not read: the retry reattaches to it, reads the log
// to its end and takes the result, and starts no new pod.
func TestRunSessionReattachesToAFinishedPod(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, _, store := newTestRunner(t, supervisedTask("t2"), kube, objects)
	stop := make(chan struct{})
	kube.onCreatePod = func() { go func() { time.Sleep(20 * time.Millisecond); close(stop) }() }
	logs := loopFailure(1) + cutLogs + pushedLine("c-wip") + "\n" + resultLine(true, "pushed") + "\n"
	if _, err := runSessionIn(t, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, []string{logs}, func(env *testsuite.TestActivityEnvironment) {
		env.SetWorkerStopChannel(stop)
	}); err == nil || !strings.Contains(err.Error(), "factoryd stopping") {
		t.Fatalf("err = %v, want the detach", err)
	}
	first := store.liveSessions["t2"]
	kube.mu.Lock()
	kube.pods[first.Namespace+"/"+first.Pod].phase = "Succeeded"
	kube.mu.Unlock()

	res, err := runSessionIn(t, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, func(env *testsuite.TestActivityEnvironment) {
		env.SetHeartbeatDetails(sessionProgress{SessionID: first.SessionID, Running: true, Events: 1})
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomePushed || res.Commit != "c-wip" || res.SessionID != first.SessionID {
		t.Fatalf("result = %+v, want the finished session's push as its own", res)
	}
	if len(kube.podOrder) != 1 {
		t.Fatalf("pods created %d, want only the first factoryd's", len(kube.podOrder))
	}
	if _, ok := store.liveSessions["t2"]; ok {
		t.Fatal("the live row survived the session's end")
	}
	if timeline := string(objGet(objects, "sessions/t2/timeline.jsonl")); !strings.Contains(timeline, `"step":"session_reattached"`) || strings.Count(timeline, `"step":"session_start"`) != 1 {
		t.Fatalf("timeline = %s, want one start and a reattach", timeline)
	}
}

// A reattached session is already open and paid into: the floor is not
// read for it, though the card's spent budget could not open a new one.
func TestRunSessionDoesNotFloorAReattachedSession(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, _, store := newTestRunner(t, supervisedTask("t2"), kube, objects)
	stop := make(chan struct{})
	kube.onCreatePod = func() { go func() { time.Sleep(20 * time.Millisecond); close(stop) }() }
	logs := loopFailure(1) + cutLogs + pushedLine("c-wip") + "\n" + resultLine(true, "pushed") + "\n"
	if _, err := runSessionIn(t, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, []string{logs}, func(env *testsuite.TestActivityEnvironment) {
		env.SetWorkerStopChannel(stop)
	}); err == nil || !strings.Contains(err.Error(), "factoryd stopping") {
		t.Fatalf("err = %v, want the detach", err)
	}
	first := store.liveSessions["t2"]
	spent := store.tasks["t2"]
	spent.SpentEUR = spent.BudgetEUR
	store.tasks["t2"] = spent
	v := testPolicyVersion()
	v.Tree.Document.Prices.MaxOutputTokens["m"] = 32768
	runner.cfg.Policy = func() policy.Version { return v }
	asked := &askedPrices{price: policy.Price{In: 4e-6, CacheRead: 4e-7, Out: 4e-6, MaxOutput: 32768}}
	runner.cfg.Prices = asked
	if why, err := runner.SessionFunded(context.Background(), "t2", "m"); err != nil || !BudgetRefusal(why) {
		t.Fatalf("a new opening's floor = (%q, %v), want the budget's refusal", why, err)
	}
	asked.aliases = nil

	res, err := runSessionIn(t, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, func(env *testsuite.TestActivityEnvironment) {
		env.SetHeartbeatDetails(sessionProgress{SessionID: first.SessionID, Running: true, Events: 1})
	})
	if err != nil || res.Outcome != OutcomePushed || res.SessionID != first.SessionID || res.StartFailed {
		t.Fatalf("result = (%+v, %v), want the live session's push as its own", res, err)
	}
	if len(asked.aliases) != 0 || len(kube.podOrder) != 1 {
		t.Fatalf("prices read %q, pods %d; want no floor read and only the first pod", asked.aliases, len(kube.podOrder))
	}
}

func spendTripSeen(hub *forgejoHub) bool {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return slices.ContainsFunc(hub.comments, func(c string) bool { return strings.Contains(c, "spend_no_push") })
}

// A session reattached after a restart weighs its spend from its real
// start: 1 EUR spent of 5 when it started leaves 4, and 2.5 EUR spent by
// the reattach is 62.5 percent of 4, past the line of 60. Weighed from the
// reattach (3.5 spent, 1.5 left) nothing would have been spent.
func TestRunSessionReattachWeighsSpendFromTheSessionsStart(t *testing.T) {
	runner, hub, kube, _, chat, store, _ := liveRig(t, CheckinResponse{Text: `{"decision":"continue","reason":"it is making progress"}`})
	tk := store.tasks["t2"]
	tk.BudgetEUR, tk.SpentEUR = 5, 1
	store.tasks["t2"] = tk
	setDetectors(runner, map[string]int{policy.DetectorNoProgress: 3600})
	runner.tick = 50 * time.Millisecond
	stop := make(chan struct{})
	kube.onCreatePod = func() { go func() { time.Sleep(20 * time.Millisecond); close(stop) }() }
	if _, err := runSessionIn(t, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/big"}, []string{hangLogs}, func(env *testsuite.TestActivityEnvironment) {
		env.SetWorkerStopChannel(stop)
	}); err == nil || !strings.Contains(err.Error(), "factoryd stopping") {
		t.Fatalf("err = %v, want the detach", err)
	}
	first := store.liveSessions["t2"]
	if first.SpentAtStart == nil || *first.SpentAtStart != 1 {
		t.Fatalf("live row baseline = %v, want 1", first.SpentAtStart)
	}
	tk = store.tasks["t2"]
	tk.SpentEUR = 3.5
	store.tasks["t2"] = tk
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for end := time.Now().Add(10 * time.Second); time.Now().Before(end) && !spendTripSeen(hub); {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	runSessionIn(t, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/big"}, nil, func(env *testsuite.TestActivityEnvironment) {
		env.SetWorkerOptions(worker.Options{BackgroundActivityContext: ctx})
		env.SetHeartbeatDetails(sessionProgress{SessionID: first.SessionID, Running: true, Events: 1})
	})
	if chat.calls != 1 || !strings.Contains(chat.gots[0].Input, "spend_no_push") {
		t.Fatalf("chat = %d calls, want the spend trip's conversation", chat.calls)
	}
}

// A baseline the store would not give leaves spend_no_push off for the
// session: 4 EUR of 5 spent in all would trip the old lifetime rule. Only
// the baseline read (the third Task call) fails; the session runs in its
// pod and the spend arrives after the pod is created.
func TestRunSessionUnreadBaselineKeepsSpendNoPushOff(t *testing.T) {
	runner, hub, kube, _, chat, store, _ := liveRig(t, CheckinResponse{Text: `{"decision":"continue"}`})
	tk := store.tasks["t2"]
	tk.BudgetEUR, tk.SpentEUR = 5, 1
	store.tasks["t2"] = tk
	spending := &spendingStore{Store: runner.store, failCall: 3}
	runner.store = spending
	kube.onCreatePod = func() { spending.added.Store(math.Float64bits(3)) }
	setDetectors(runner, map[string]int{policy.DetectorNoProgress: 3600})
	runner.tick = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runSession(t, ctx, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/big"}, nil, hangLogs)
	if len(kube.podOrder) != 1 || spending.calls.Load() < 4 {
		t.Fatalf("pods = %d, task reads = %d; want the session to run with its baseline read failing", len(kube.podOrder), spending.calls.Load())
	}
	if row, ok := store.liveSessions["t2"]; ok && row.SpentAtStart != nil {
		t.Fatalf("live row baseline = %v, want none", *row.SpentAtStart)
	}
	if chat.calls != 0 || spendTripSeen(hub) {
		t.Fatalf("chat = %d calls, trip seen = %v, want spend_no_push off", chat.calls, spendTripSeen(hub))
	}
}

// A session reattached from a row written before the baseline column (null)
// runs with spend_no_push off: 4 of 5 spent in all would trip the old rule.
func TestRunSessionReattachWithNullBaselineKeepsSpendNoPushOff(t *testing.T) {
	runner, hub, kube, _, chat, store, objects := liveRig(t, CheckinResponse{Text: `{"decision":"continue"}`})
	tk := store.tasks["t2"]
	tk.BudgetEUR, tk.SpentEUR = 5, 1
	store.tasks["t2"] = tk
	setDetectors(runner, map[string]int{policy.DetectorNoProgress: 3600})
	runner.tick = 50 * time.Millisecond
	stop := make(chan struct{})
	kube.onCreatePod = func() { go func() { time.Sleep(20 * time.Millisecond); close(stop) }() }
	if _, err := runSessionIn(t, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/big"}, []string{hangLogs}, func(env *testsuite.TestActivityEnvironment) {
		env.SetWorkerStopChannel(stop)
	}); err == nil || !strings.Contains(err.Error(), "factoryd stopping") {
		t.Fatalf("err = %v, want the detach", err)
	}
	first := store.liveSessions["t2"]
	first.SpentAtStart = nil
	store.liveSessions["t2"] = first
	tk = store.tasks["t2"]
	tk.SpentEUR = 4
	store.tasks["t2"] = tk
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runSessionIn(t, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/big"}, nil, func(env *testsuite.TestActivityEnvironment) {
		env.SetWorkerOptions(worker.Options{BackgroundActivityContext: ctx})
		env.SetHeartbeatDetails(sessionProgress{SessionID: first.SessionID, Running: true, Events: 1})
	})
	if timeline := string(objGet(objects, "sessions/t2/timeline.jsonl")); !strings.Contains(timeline, `"step":"session_reattached"`) {
		t.Fatalf("timeline = %s, want the reattach", timeline)
	}
	if chat.calls != 0 || spendTripSeen(hub) {
		t.Fatalf("chat = %d calls, trip seen = %v, want spend_no_push off", chat.calls, spendTripSeen(hub))
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
	// resumed from an interim commit and changed nothing: the interim push is no finished work
	res = sessionResult(sessionSpec{id: "s1", startRef: "wip", base: "main", startWip: true}, sessionOutcome{result: done})
	if res.Outcome != OutcomeNoPush || res.Commit != "" {
		t.Fatalf("session resumed from an interim commit without new changes: %+v", res)
	}
	// resumed from an interim commit, the worker landed a finished one on it: the gates judge that
	res = sessionResult(sessionSpec{id: "s1", startRef: "wip", base: "main", startWip: true}, sessionOutcome{result: done, commit: "fin"})
	if res.Outcome != OutcomePushed || res.Commit != "fin" {
		t.Fatalf("session that landed a finished commit on an interim start: %+v", res)
	}
	// started at the base and changed nothing: no push
	res = sessionResult(sessionSpec{id: "s1", startRef: "main", base: "main"}, sessionOutcome{result: done})
	if res.Outcome != OutcomeNoPush || res.Commit != "" {
		t.Fatalf("first session without changes: %+v", res)
	}
}

// zaiFilterSummary is z.ai's refusal of filtered content as LiteLLM passes
// it on: business code 1301 inside the provider's error body, HTTP 400.
const zaiFilterSummary = `AI_APICallError: litellm.BadRequestError: ZaiException - {"error":{"code":"1301","message":"System detected potentially unsafe or sensitive content in input or generation. Please avoid prompts that may produce sensitive content."}} (status 400)`

func TestContentFilterRefusalGoesToAHuman(t *testing.T) {
	const prefix = "the model provider's content filter refused this session's model call (z.ai error 1301): "
	const suffix = ". Close this card and file it again with the wording changed, or answer to try once more."
	res := sessionResult(sessionSpec{id: "s1"}, sessionOutcome{result: &harness.Result{Success: false, Summary: zaiFilterSummary}})
	if res.Outcome != OutcomeEscalate || res.Reason != prefix+zaiFilterSummary+suffix {
		t.Fatalf("content-filter refusal: %+v", res)
	}
	if got := escalationSource(res.Reason); got != "content_filter" {
		t.Fatalf("escalation source = %q, want content_filter", got)
	}
	for name, summary := range map[string]string{
		"Alibaba's old wording": "AI_APICallError: InternalError.Algo.DataInspectionFailed: input check failed (status 400)",
		"its snake-case form":   "provider error: data_inspection_failed (status 400)",
		"a rate limit":          "AI_APICallError: rate limited (status 429)",
		"z.ai's rate limit":     `ZaiException - {"error":{"code":"1302","message":"rate limit"}} (status 429)`,
	} {
		res = sessionResult(sessionSpec{id: "s1"}, sessionOutcome{result: &harness.Result{Success: false, Summary: summary}})
		if res.Outcome != OutcomeNoPush || strings.HasPrefix(res.Reason, contentFilterRefusal) {
			t.Errorf("%s opens a content-filter case: %+v", name, res)
		}
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
	setDetectors(runner, map[string]int{policy.DetectorNoProgress: 3}) // the tick every second
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
	if !regexp.MustCompile(`^factoryd's facts for this session: 2\.80 of the 5\.00 EUR budget left for its calls \(the model review and the audit hold 0\.40 EUR of it\); \d+ minutes of agent time left; this is session 1 of the 4 `).MatchString(got) {
		t.Fatalf("FACTS = %q", got)
	}
}

// The session's facts and the check-in's card facts state the spend the
// gateway counts: 4.00 settled and 0.50 held of 5.00 is 4.50 spent and
// 0.50 left, 0.10 of it past the 0.40 gate reserve.
func TestSessionFactsSpendIncludesHolds(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", SHA: "base", BudgetEUR: 5, SpentEUR: 4, HeldEUR: 0.5, Track: "direct"}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Sessions: 4, Model: "factory/small"}, nil,
		pushedLine("c0ffee")+"\n"+resultLine(true, "done")); err != nil {
		t.Fatal(err)
	}
	if got := podEnv(kube.podAt(0))["FACTS"]; !strings.HasPrefix(got, "factoryd's facts for this session: 0.10 of the 5.00 EUR budget left for its calls (the model review and the audit hold 0.40 EUR of it); ") {
		t.Fatalf("FACTS = %q, want 0.10 of 5.00 EUR left for its calls", got)
	}
	if got, want := checkinFacts(task, sessionSpec{attempt: 1, sessions: 4}, wipOutcome{}), `{"attempt":1,"budget_eur":5,"sessions":4,"spent_eur":4.5}`; got != want {
		t.Fatalf("card facts = %s, want %s", got, want)
	}
	if got, want := checkinFacts(task, sessionSpec{attempt: 1, sessions: 4}, wipOutcome{head: "abc123"}),
		`{"attempt":1,"budget_eur":5,"sessions":4,"spent_eur":4.5,"wip_head":"abc123"}`; got != want {
		t.Fatalf("card facts = %s, want %s naming the interim push's head", got, want)
	}
}

// A running session's pace and spend watch read the spend the gateway
// counts from the card's row: 4.50 settled and 0.40 held of 5.00 is 4.90
// spent, so the pace reads 4.90, the budget warning says 98 percent with
// 0.10 left, and the no-push trip names 4.90 of the 5.00 the card had.
func TestPaceAndSpendWatchNetHolds(t *testing.T) {
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", SHA: "base",
		BudgetEUR: 5, SpentEUR: 4.5, HeldEUR: 0.4, Track: "direct"}, kube, &fakeObjectStore{})
	ctx := context.Background()
	s := sessionSpec{id: "s1", attempt: 1, sessions: 3, messageToken: "tok"}
	if p := runner.pace(ctx, Task{ID: "t1"}, s, 0, 0, time.Now(), time.Now().Add(time.Hour)); p == nil || p.SpentEUR != 4.9 {
		t.Fatalf("pace = %+v, want 4.90 spent", p)
	}
	kube.pods["ns/pod"] = &fakePod{ns: "ns", name: "pod"}
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)
	trip := runner.watchLimits(ctx, Task{ID: "t1"}, s, "ns", "pod", newLimitWatch(policy.Limits{}, false, time.Now(), time.Hour), spendSupervisor(t, nil))
	worker.mu.Lock()
	texts := slices.Clone(worker.texts)
	worker.mu.Unlock()
	if len(texts) != 1 || !strings.HasPrefix(texts[0], "POST /message factoryd: 98 percent of this card's budget is spent, 0.00 of 5.00 EUR are left for its calls (the model review and the audit hold 0.10 EUR of it). ") {
		t.Fatalf("worker got %q, want the budget warning at 4.90 of 5.00: 0.10 left, under the 0.40 reserve", texts)
	}
	if trip == nil || trip.Kind != TripSpendNoPush || trip.Detail != "this session spent 4.90 EUR of the 5.00 EUR the card had left at its start (98 percent, the line is 60) and no work is pushed" {
		t.Fatalf("trip = %+v, want the no-push trip at 4.90 of 5.00", trip)
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
	setDetectors(runner, map[string]int{policy.DetectorNoProgress: 60})
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
	if len(texts) != 2 || texts[0] != "POST /brief " || !strings.HasPrefix(texts[1], "POST /message factoryd: 90 percent of this card's budget is spent, 0.10 of 5.00 EUR are left for its calls (the model review and the audit hold 0.40 EUR of it).") {
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
	setDetectors(runner, map[string]int{policy.DetectorNoProgress: 60})
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

// spendingStore reports the spend a session adds once the test sets it.
type spendingStore struct {
	Store
	added atomic.Uint64
	// failCall is the 1-based Task call that fails; 0 = none
	failCall int32
	calls    atomic.Int32
}

func (s *spendingStore) Task(ctx context.Context, id string) (Task, bool, error) {
	if s.calls.Add(1) == s.failCall {
		return Task{}, false, errors.New("store down")
	}
	t, ok, err := s.Store.Task(ctx, id)
	t.SpentEUR += math.Float64frombits(s.added.Load())
	return t, ok, err
}

// startsFrom runs a session that starts on the branch head w2, spends 2.5
// of the 4 EUR the card had left and pushes nothing. committer is who wrote
// w2 and given the start the workflow's input names; set says who wrote the
// commits below w2. It reports whether spend_no_push tripped and the worker's
// START_WIP.
func startsFrom(t *testing.T, committer, given string, set func(*forgejoHub)) (tripped bool, startWip string) {
	t.Helper()
	runner, hub, kube, _, chat, store, _ := liveRig(t, CheckinResponse{Text: `{"decision":"continue","reason":"it is making progress"}`})
	hub.branchHead = "w2"
	hub.committers, hub.parents = map[string]string{"w2": committer}, map[string]string{"w2": "b0"}
	if set != nil {
		set(hub)
	}
	tk := store.tasks["t2"]
	tk.BudgetEUR, tk.SpentEUR, tk.SHA = 5, 1, "b0"
	store.tasks["t2"] = tk
	spending := &spendingStore{Store: runner.store}
	runner.store = spending
	kube.onCreatePod = func() { spending.added.Store(math.Float64bits(2.5)) }
	setDetectors(runner, map[string]int{policy.DetectorNoProgress: 3600})
	runner.tick = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for end := time.Now().Add(3 * time.Second); time.Now().Before(end) && !spendTripSeen(hub); {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	runSession(t, ctx, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/big", StartRef: given}, nil, hangLogs)
	return chat.calls == 1 && spendTripSeen(hub), podEnv(kube.podAt(0))["START_WIP"]
}

// A card whose earlier sessions spent 1 of its 5 EUR has 4 left when its next
// session starts, so the line is 0.6 x 4 = 2.4 EUR of the session's own spend.
// At 2.1 EUR spent by the session (3.1 of 5 in all, past the 3.0 a lifetime
// weighing would use) nothing trips; at 2.5 it does.
func TestRunSessionWeighsSpendFromTheCardsSpendAtItsStart(t *testing.T) {
	runner, hub, kube, _, chat, store, _ := liveRig(t, CheckinResponse{Text: `{"decision":"continue","reason":"it is making progress"}`})
	tk := store.tasks["t2"]
	tk.BudgetEUR, tk.SpentEUR = 5, 1
	store.tasks["t2"] = tk
	spending := &spendingStore{Store: runner.store}
	runner.store = spending
	started := make(chan struct{})
	kube.onCreatePod = func() {
		spending.added.Store(math.Float64bits(2.1))
		close(started)
	}
	setDetectors(runner, map[string]int{policy.DetectorNoProgress: 3600})
	runner.tick = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	var early atomic.Bool
	go func() {
		defer cancel()
		<-started
		for seen := spending.calls.Load(); spending.calls.Load() < seen+8; {
			time.Sleep(10 * time.Millisecond)
		}
		early.Store(spendTripSeen(hub))
		spending.added.Store(math.Float64bits(2.5))
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && !spendTripSeen(hub); {
			time.Sleep(10 * time.Millisecond)
		}
	}()
	runSession(t, ctx, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/big"}, nil, hangLogs)
	if early.Load() {
		t.Fatal("2.1 EUR spent by the session of the 4 the card had left tripped spend_no_push")
	}
	if chat.calls != 1 || !spendTripSeen(hub) {
		t.Fatalf("chat = %d calls, trip seen = %v; want the trip at 2.5 EUR spent", chat.calls, spendTripSeen(hub))
	}
}

// interimChain makes w2 the top of n interim commits above the base b0.
func interimChain(h *forgejoHub, n int) {
	below := "w2"
	for i := 1; i < n; i++ {
		next := fmt.Sprintf("x%d", i)
		h.parents[below], h.committers[next] = next, wipEmail
		below = next
	}
	h.parents[below] = "b0"
}

// An interim push grants no push credit, whatever the input names as the
// start: a session that starts on an interim head, named or not (a retry of
// a dead attempt, a run from before the gate), and spends past the line with
// nothing pushed trips spend_no_push. One that starts on a session's push
// does not.
func TestRunSessionOnAnInterimHeadKeepsSpendNoPush(t *testing.T) {
	for _, tc := range []struct {
		name, committer, given string
		set                    func(*forgejoHub)
		trips                  bool
	}{
		{"an interim head the input names", wipEmail, "w2", nil, true},
		{"an interim head the input leaves out", wipEmail, "", nil, true},
		{"a session's push the input names", "factory-agent@factory.local", "w2", nil, false},
		{"a session's push the input leaves out", "factory-agent@factory.local", "", nil, false},
		// an interim commit sits on its session's start: here c2, a session's
		// push, which a session of the card pushed before
		{"an interim head on a session's push", wipEmail, "w2", func(h *forgejoHub) { h.parents = map[string]string{"w2": "c2"} }, false},
		{"stacked interim heads on the base", wipEmail, "w2", func(h *forgejoHub) {
			h.parents, h.committers["w1"] = map[string]string{"w2": "w1", "w1": "b0"}, wipEmail
		}, true},
		{"32 interim heads on the base", wipEmail, "w2", func(h *forgejoHub) { interimChain(h, 32) }, true},
		{"33 interim heads on the base count as a push", wipEmail, "w2", func(h *forgejoHub) { interimChain(h, 33) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, startWip := startsFrom(t, tc.committer, tc.given, tc.set)
			if got != tc.trips {
				t.Fatalf("spend_no_push tripped = %v, want %v", got, tc.trips)
			}
			if want := map[bool]string{true: "1"}[tc.committer == wipEmail]; startWip != want {
				t.Fatalf("worker START_WIP = %q, want %q: the worker lands a finished commit on an interim start", startWip, want)
			}
		})
	}
}

// A session that spends its share of what the card had left, with nothing
// pushed, asks the check-in: 1 EUR before the session leaves 4, and 2.5 EUR
// spent in it is 62.5 percent of 4, past the default 60.
func TestRunSessionSpendWithoutPushRunsCheckin(t *testing.T) {
	runner, hub, kube, _, chat, store, _ := liveRig(t, CheckinResponse{Text: `{"decision":"continue","reason":"it is making progress"}`})
	tk := store.tasks["t2"]
	tk.BudgetEUR, tk.SpentEUR, tk.SHA = 5, 1, "b0"
	store.tasks["t2"] = tk
	spending := &spendingStore{Store: runner.store}
	runner.store = spending
	kube.onCreatePod = func() { spending.added.Store(math.Float64bits(2.5)) }
	setDetectors(runner, map[string]int{policy.DetectorNoProgress: 3600})
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

// A small card past its bound, in a project whose process has a plan
// track: the check-in's replan stops the session, its work in progress
// pushed and kept, and the result carries the text the planner cuts the
// rest from.
func TestRunSessionCheckinReplanStopsTheSession(t *testing.T) {
	runner, hub, kube, _, chat, store, objects := liveRig(t, CheckinResponse{Text: `{"decision":"replan","reason":"the rest is two packages"}`})
	runner.stopWait = 10 * time.Second
	project := seedProject("sf/repo")
	project.Process, project.Tracks = "spec", specTracks
	store.projects = []Project{project}
	tk := store.tasks["t2"]
	tk.Size = "small"
	store.tasks["t2"] = tk
	version := testPolicyVersion()
	version.Document.Risk.LargeLines = 1000
	runner.cfg.Policy = func() policy.Version { return version }
	runner.cfg.PolicyVersion = func(context.Context, int64) (policy.Version, bool, error) { return version, true, nil }
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/big"}, nil,
		cardDiffLine(250, 100)+"\n"+stoppedWorker("t2"))
	if err != nil {
		t.Fatal(err)
	}
	if chat.calls != 1 || !strings.Contains(chat.gots[0].System, "|replan") || !strings.Contains(chat.gots[0].Input, "- replan:") {
		t.Fatalf("chat = %d calls %+v, want one conversation offered replan", chat.calls, chat.gots)
	}
	want := "The check-in decided replan after the size trip (the card's diff is 350 changed lines (added plus deleted), over the 300 its size plans): the rest is two packages"
	if res.Outcome != OutcomeReplan || res.Reason != "the rest is two packages" || res.Trip != string(TripSize) || res.Replan != want || res.Commit != "c-wip" {
		t.Fatalf("result = %+v, want the replan with its text and the work in progress c-wip", res)
	}
	if len(kube.deletedPods) == 0 {
		t.Fatal("the worker was not stopped")
	}
	if tl := string(objGet(objects, "sessions/t2/timeline.jsonl")); !strings.Contains(tl, `"step":"checkin"`) || !strings.Contains(tl, `"decision":"replan"`) {
		t.Fatalf("timeline = %s, want the check-in's replan step", tl)
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "**replan** after the size detector tripped") {
		t.Fatalf("comments = %q, want the check-in's replan on the issue", hub.comments)
	}
}

// A person's replan stops the running session on their word: no check-in
// is asked, no detector's alert is raised, the step is recorded and the
// work in progress kept.
func TestRunSessionOfAPersonsReplanSkipsTheCheckin(t *testing.T) {
	runner, hub, kube, _, chat, store, objects := liveRig(t)
	runner.stopWait = 10 * time.Second
	// the replan reaches the session before the worker printed a line: its
	// log is written only once the pod's deletion has begun
	kube.onDeletePod = func() { go kube.scriptPods(0, stoppedWorker("t2")) }
	delivered := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := runner.EscalateSession(t.Context(), "t2", AgentAsk{Kind: AskReplan, Reason: "replanned by ana: x"})
			if !errors.Is(err, ErrNoLiveSession) || time.Now().After(deadline) {
				delivered <- err
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/small"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-delivered; err != nil {
		t.Fatalf("EscalateSession = %v", err)
	}
	if res.Outcome != OutcomeReplan || res.Reason != "replanned by ana: x" || res.Trip != string(TripReplan) || res.Replan != "replanned by ana: x" || res.Commit != "c-wip" {
		t.Fatalf("result = %+v, want the person's replan with the work in progress c-wip", res)
	}
	if chat.calls != 0 {
		t.Fatalf("check-in conversations = %d, want none", chat.calls)
	}
	if len(kube.deletedPods) == 0 {
		t.Fatal("the worker was not stopped")
	}
	if tl := string(objGet(objects, "sessions/t2/timeline.jsonl")); !strings.Contains(tl, `"step":"checkin"`) || !strings.Contains(tl, `"detector":"replan"`) {
		t.Fatalf("timeline = %s, want the person's replan step", tl)
	}
	if alerts := store.alertRows(); len(alerts) != 0 {
		t.Fatalf("alerts = %+v, want none: a person's replan trips no detector", alerts)
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.comments) != 0 {
		t.Fatalf("comments = %q, want none: a person's decision is recorded, not commented", hub.comments)
	}
}

// cutAsk is an agent's request_cut with two parts.
var cutAsk = AgentAsk{Kind: AskCut, Reason: "a store and a UI", Parts: []string{"the store", "the UI"}}

// cutDetail is cutAsk's trip detail.
const cutDetail = "the agent asks to cut the card: a store and a UI. Its parts: 1. the store; 2. the UI"

// withPlanTrack gives the rig's project a process with a plan track, so
// its direct card can be cut.
func withPlanTrack(store *fakeStore) {
	project := seedProject("sf/repo")
	project.Process, project.Tracks = "spec", specTracks
	store.projects = []Project{project}
}

// The agent's cut goes to the check-in, which reads the ask and is offered
// replan: its replan stops the session, the work in progress kept, with
// the agent's parts in the planner's text, and no detector's alert.
func TestRunSessionCutGoesToTheCheckin(t *testing.T) {
	runner, hub, kube, _, chat, store, objects := liveRig(t, CheckinResponse{Text: `{"decision":"replan","reason":"the two parts hold"}`})
	runner.stopWait = 10 * time.Second
	withPlanTrack(store)
	delivered := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := runner.EscalateSession(t.Context(), "t2", cutAsk)
			if !errors.Is(err, ErrNoLiveSession) || time.Now().After(deadline) {
				delivered <- err
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/big"}, nil, stoppedWorker("t2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := <-delivered; err != nil {
		t.Fatalf("EscalateSession = %v", err)
	}
	if chat.calls != 1 {
		t.Fatalf("chat = %d calls, want one conversation on the cut", chat.calls)
	}
	in := chat.gots[0].Input
	for _, want := range []string{"the detector agent_cut tripped (" + cutDetail + ")", "- replan:",
		"The agent asks to cut the card. Answer replan if the cut holds. Answer continue, with its guidance, if the card should finish as one card."} {
		if !strings.Contains(in, want) {
			t.Fatalf("check-in statement misses %q:\n%s", want, in)
		}
	}
	want := "The check-in decided replan after the agent_cut trip (" + cutDetail + "): the two parts hold"
	if res.Outcome != OutcomeReplan || res.Reason != "the two parts hold" || res.Trip != string(TripAgentCut) || res.Replan != want || res.Commit != "c-wip" {
		t.Fatalf("result = %+v, want the replan naming the agent's parts with the work in progress c-wip", res)
	}
	if len(kube.deletedPods) == 0 {
		t.Fatal("the worker was not stopped")
	}
	if step := timelineStep(t, objects, "t2", "checkin"); step["detector"] != "agent_cut" || step["decision"] != "replan" {
		t.Fatalf("check-in step = %v, want the cut's replan", step)
	}
	if alerts := store.alertRows(); len(alerts) != 0 {
		t.Fatalf("alerts = %+v, want none: the agent's cut trips no detector", alerts)
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "**replan** after the agent_cut detector tripped") {
		t.Fatalf("comments = %q, want the check-in's replan on the issue", hub.comments)
	}
}

// Review Focus 4: the agent's cut that arrives while the check-in on
// another trip runs does not cut that conversation. Its answer stands, and
// the cut is the next trip, judged in a conversation of its own.
func TestCutDuringACheckinWaitsForTheAnswer(t *testing.T) {
	runner, _, _, _, chat, store, objects := liveRig(t,
		CheckinResponse{Text: `{"decision":"continue","reason":"the reads differ each time"}`},
		CheckinResponse{Text: `{"decision":"continue","reason":"the card is one card"}`})
	withPlanTrack(store)
	// the repetition's conversation answers only once the session took the cut
	delivered := make(chan error, 1)
	chat.onCall = func() {
		if chat.calls == 1 {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			delivered <- runner.EscalateSession(ctx, "t2", cutAsk)
		}
	}
	logs := commands(0, 24) + pushedLine("c1") + "\n" + resultLine(true, "pushed")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-delivered; err != nil {
		t.Fatalf("the cut during the conversation = %v, want taken", err)
	}
	if res.Outcome != OutcomePushed || res.Commit != "c1" {
		t.Fatalf("result = %+v, want the session run on to its push", res)
	}
	if chat.calls != 2 || !strings.Contains(chat.gots[0].Input, "the detector repetition tripped") ||
		!strings.Contains(chat.gots[1].Input, "the detector agent_cut tripped ("+cutDetail+")") {
		t.Fatalf("chat = %d calls %+v, want the repetition's and then the cut's", chat.calls, chat.gots)
	}
	var steps []string
	tl := string(objGet(objects, "sessions/t2/timeline.jsonl"))
	for _, l := range strings.Split(strings.TrimSpace(tl), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["step"] == "checkin" {
			steps = append(steps, fmt.Sprint(m["detector"], " ", m["decision"], ": ", m["reason"]))
		}
	}
	if want := []string{"repetition continue: the reads differ each time", "agent_cut continue: the card is one card"}; !slices.Equal(steps, want) {
		t.Fatalf("check-in steps = %q, want %q", steps, want)
	}
	if alerts := store.alertRows(); len(alerts) != 1 || alerts[0].Source != "repetition" {
		t.Fatalf("alerts = %+v, want the repetition's alone", alerts)
	}
}

// A cut held during a check-in whose answer stops the session is dropped
// and logged: the session judges nothing after its stop.
func TestCutDuringACheckinThatStopsTheSessionIsDropped(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	runner, _, _, _, chat, store, _ := liveRig(t, CheckinResponse{Text: `{"decision":"handover","handover":"read less, write the store"}`})
	withPlanTrack(store)
	delivered := make(chan error, 1)
	chat.onCall = func() {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		delivered <- runner.EscalateSession(ctx, "t2", cutAsk)
	}
	stopped := envLine(harness.Event{Kind: harness.EventFinished, Time: time.Now(), Detail: map[string]any{"success": false}}) + "\n" +
		pushedLine("c-wip") + "\n" + resultLine(false, "stopped: work in progress pushed at c-wip") + "\n"
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, commands(0, 24)+stopped)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-delivered; err != nil {
		t.Fatalf("the cut during the conversation = %v, want taken", err)
	}
	if res.Outcome != OutcomeNoPush || res.Trip != string(TripRepetition) || res.CheckIn == nil || res.CheckIn.Handover != "read less, write the store" || chat.calls != 1 {
		t.Fatalf("result = %+v after %d calls, want the repetition's handover alone", res, chat.calls)
	}
	if got := logs.String(); !strings.Contains(got, "task t2: the agent's cut during the check-in for repetition is dropped: the session stops") {
		t.Fatalf("log = %q, want the dropped cut", got)
	}
}

// An escalation after a held cut, in the same conversation, cuts the
// conversation and wins as any escalation does; the held cut is dropped.
func TestAnEscalationAfterAHeldCutWins(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	runner, _, _, _, chat, store, _ := liveRig(t, CheckinResponse{Text: `{"decision":"continue","guidance":"try another way"}`})
	runner.stopWait = 5 * time.Second
	withPlanTrack(store)
	chat.delay = 20 * time.Second
	asked := make(chan error, 2)
	chat.onCall = func() {
		if chat.calls == 1 {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			asked <- runner.EscalateSession(ctx, "t2", cutAsk)
			asked <- runner.EscalateSession(ctx, "t2", licenseAsk)
		}
	}
	stopped := envLine(harness.Event{Kind: harness.EventFinished, Time: time.Now(), Detail: map[string]any{"success": false}}) + "\n" +
		pushedLine("c-wip") + "\n" + resultLine(false, "stopped: work in progress pushed at c-wip") + "\n"
	began := time.Now()
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, commands(0, 24)+stopped)
	if err != nil {
		t.Fatal(err)
	}
	if cut, esc := <-asked, <-asked; cut != nil || esc != nil {
		t.Fatalf("the cut = %v, the escalation = %v; want both taken", cut, esc)
	}
	if took := time.Since(began); took > 10*time.Second || chat.calls != 1 {
		t.Fatalf("took %s over %d calls: the escalation did not cut the conversation", took, chat.calls)
	}
	if res.Outcome != OutcomeEscalate || !strings.HasPrefix(res.Reason, "The agent asks: Which license applies?") || res.Commit != "c-wip" {
		t.Fatalf("result = %+v, want the agent's escalation with the work in progress", res)
	}
	if got := logs.String(); !strings.Contains(got, "task t2: the agent's cut during the check-in for repetition is dropped: the session stops") {
		t.Fatalf("log = %q, want the dropped cut", got)
	}
}

// A second cut during the hold replaces the first: the check-in judges
// the newer ask alone.
func TestASecondCutDuringACheckinReplacesTheFirst(t *testing.T) {
	runner, _, _, _, chat, store, _ := liveRig(t,
		CheckinResponse{Text: `{"decision":"continue","reason":"the reads differ each time"}`},
		CheckinResponse{Text: `{"decision":"continue","reason":"the card is one card"}`})
	withPlanTrack(store)
	second := AgentAsk{Kind: AskCut, Reason: "a store, a UI and a CLI", Parts: []string{"the store", "the UI", "the CLI"}}
	asked := make(chan error, 2)
	chat.onCall = func() {
		if chat.calls == 1 {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			asked <- runner.EscalateSession(ctx, "t2", cutAsk)
			asked <- runner.EscalateSession(ctx, "t2", second)
		}
	}
	logs := commands(0, 24) + pushedLine("c1") + "\n" + resultLine(true, "pushed")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if first, again := <-asked, <-asked; first != nil || again != nil {
		t.Fatalf("the cuts = %v and %v, want both taken", first, again)
	}
	if res.Outcome != OutcomePushed || res.Commit != "c1" {
		t.Fatalf("result = %+v, want the session run on to its push", res)
	}
	if chat.calls != 2 || !strings.Contains(chat.gots[1].Input, "the detector agent_cut tripped (the agent asks to cut the card: a store, a UI and a CLI. Its parts: 1. the store; 2. the UI; 3. the CLI)") ||
		strings.Contains(chat.gots[1].Input, cutDetail) {
		t.Fatalf("chat = %d calls %+v, want the repetition's and then the second cut's alone", chat.calls, chat.gots)
	}
}

// The check-in's continue on the agent's cut answers the agent: the
// check-in's reason reaches it as a message, and the session runs on.
func TestRunSessionCutContinueAnswersTheAgent(t *testing.T) {
	runner, _, _, worker, chat, store, objects := liveRig(t, CheckinResponse{Text: `{"decision":"continue","reason":"the rest is one store."}`})
	withPlanTrack(store)
	const want = "POST /message The check-in keeps this card whole: the rest is one store. Finish it as one card."
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	delivered := make(chan error, 1)
	go func() {
		defer cancel()
		deadline := time.Now().Add(10 * time.Second)
		err := ErrNoLiveSession
		for errors.Is(err, ErrNoLiveSession) && time.Now().Before(deadline) {
			if err = runner.EscalateSession(t.Context(), "t2", cutAsk); errors.Is(err, ErrNoLiveSession) {
				time.Sleep(5 * time.Millisecond)
			}
		}
		delivered <- err
		for time.Now().Before(deadline) {
			worker.mu.Lock()
			found := slices.Contains(worker.texts, want)
			worker.mu.Unlock()
			if found {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	runSession(t, ctx, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, hangLogs)
	if err := <-delivered; err != nil {
		t.Fatalf("EscalateSession = %v", err)
	}
	worker.mu.Lock()
	texts := slices.Clone(worker.texts)
	worker.mu.Unlock()
	if !slices.Contains(texts, want) || chat.calls != 1 {
		t.Fatalf("worker got %q after %d calls, want the check-in's reason as a message", texts, chat.calls)
	}
	if step := timelineStep(t, objects, "t2", "checkin"); step["detector"] != "agent_cut" || step["decision"] != "continue" {
		t.Fatalf("check-in step = %v, want the cut's continue", step)
	}
}

// A reattached session reads its project again: a card with a plan track
// takes the agent's cut after the reattach too.
func TestRunSessionReattachedSessionTakesACut(t *testing.T) {
	runner, _, kube, _, chat, store, _ := liveRig(t, CheckinResponse{Text: `{"decision":"replan","reason":"the two parts hold"}`})
	runner.stopWait = 10 * time.Second
	withPlanTrack(store)
	stop := make(chan struct{})
	kube.onCreatePod = func() { go func() { time.Sleep(20 * time.Millisecond); close(stop) }() }
	logs := loopFailure(1) + sigtermLogs + pushedLine("c-wip") + "\n" + resultLine(false, "stopped; work in progress pushed at c-wip") + "\n"
	if _, err := runSessionIn(t, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, []string{logs}, func(env *testsuite.TestActivityEnvironment) {
		env.SetWorkerStopChannel(stop)
	}); err == nil || !strings.Contains(err.Error(), "factoryd stopping") {
		t.Fatalf("err = %v, want the detach", err)
	}
	first := store.liveSessions["t2"]
	delivered := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := runner.EscalateSession(t.Context(), "t2", cutAsk)
			if !errors.Is(err, ErrNoLiveSession) || time.Now().After(deadline) {
				delivered <- err
				if err != nil {
					// a refused cut leaves the session held at its stop: end it
					kube.DeletePod(context.Background(), first.Namespace, first.Pod)
				}
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	res, err := runSessionIn(t, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, func(env *testsuite.TestActivityEnvironment) {
		env.SetHeartbeatDetails(sessionProgress{SessionID: first.SessionID, Running: true, Events: 1})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-delivered; err != nil {
		t.Fatalf("the cut of the reattached session = %v, want taken", err)
	}
	if res.SessionID != first.SessionID || res.Outcome != OutcomeReplan || res.Trip != string(TripAgentCut) || res.Commit != "c-wip" || chat.calls != 1 {
		t.Fatalf("result = %+v after %d calls, want the reattached session's replan on the cut", res, chat.calls)
	}
}

// request_cut is offered only where the card can be cut: a coding
// session of a direct card whose project has a plan track, never a card
// without one or a report.
func TestWorkerOffersRequestCutOnlyWhereTheCardCanBeCut(t *testing.T) {
	r := &Runner{cfg: testRunnerConfig(), forgejo: &Forgejo{BotUser: "factory-agent"}}
	doc := testPolicyVersion().Document
	coder := doc.Classes["coder"]
	coder.Tools = map[string]policy.ToolCap{policy.ToolRequestCut: {}, policy.ToolRequestInfo: {}}
	doc.Classes["coder"] = coder
	task := Task{ID: "t1", Kind: "agent", Repository: "sf/repo", RequiredCapabilities: []string{"coder"}, Track: "direct"}
	for name, tc := range map[string]struct {
		s    sessionSpec
		want string
	}{
		"a plan track":  {sessionSpec{id: "s-1", policy: doc, planTrack: true}, `["request_cut","request_info"]`},
		"no plan track": {sessionSpec{id: "s-1", policy: doc}, `["request_info"]`},
		"a report":      {sessionSpec{id: "s-1", policy: doc, planTrack: true, report: "the guide"}, `["request_info"]`},
	} {
		pod := r.workerPodSpec(task, tc.s)
		if got := containerEnv(pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any))["TOOLS"]; got != tc.want {
			t.Errorf("%s: TOOLS = %s, want %s", name, got, tc.want)
		}
	}
}

// A direct card whose project's process has no plan track cannot be cut:
// its size trip still asks the check-in, which is offered no replan, and a
// replan answer is refused and asked again.
func TestRunSessionOfACardWithoutAPlanTrackOffersNoReplan(t *testing.T) {
	runner, _, _, _, chat, store, _ := liveRig(t,
		CheckinResponse{Text: `{"decision":"replan","reason":"the rest is two packages"}`},
		CheckinResponse{Text: `{"decision":"continue","reason":"the rest is tests"}`})
	tk := store.tasks["t2"]
	tk.Size = "small"
	store.tasks["t2"] = tk
	version := testPolicyVersion()
	version.Document.Risk.LargeLines = 1000
	runner.cfg.Policy = func() policy.Version { return version }
	runner.cfg.PolicyVersion = func(context.Context, int64) (policy.Version, bool, error) { return version, true, nil }
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/big"}, nil,
		cardDiffLine(250, 100)+"\n"+pushedLine("c0ffee")+"\n"+resultLine(true, "done"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomePushed || res.Commit != "c0ffee" {
		t.Fatalf("result = %+v, want the session run on to its push", res)
	}
	if chat.calls != 2 {
		t.Fatalf("chat = %d calls %+v, want the refused replan and the answer after it", chat.calls, chat.gots)
	}
	first := chat.gots[0]
	if !strings.Contains(first.Input, "over the 300 its size plans") || strings.Contains(first.System, "|replan") ||
		strings.Contains(first.Input, "- replan:") || strings.Contains(first.Input, "Answer replan") {
		t.Fatalf("the size trip's check-in offers replan:\n%s\n%s", first.System, first.Input)
	}
	if !strings.Contains(chat.gots[1].Input, `Your last answer was unreadable: unknown decision "replan"; allowed: continue, handover, hand_up.`) {
		t.Fatalf("second call = %s, want the replan refused", chat.gots[1].Input)
	}
}

// The project read only decides whether replan is offered: when it fails,
// the session runs, and its check-in is offered no replan.
func TestRunSessionRunsWhenTheProjectReadFails(t *testing.T) {
	runner, _, _, _, chat, store, _ := liveRig(t, CheckinResponse{Text: `{"decision":"continue","reason":"the rest is tests"}`})
	project := seedProject("sf/repo")
	project.Process, project.Tracks = "spec", specTracks
	store.projects = []Project{project}
	store.projectReadFails = 1 // the session start's read, the first
	tk := store.tasks["t2"]
	tk.Size = "small"
	store.tasks["t2"] = tk
	version := testPolicyVersion()
	version.Document.Risk.LargeLines = 1000
	runner.cfg.Policy = func() policy.Version { return version }
	runner.cfg.PolicyVersion = func(context.Context, int64) (policy.Version, bool, error) { return version, true, nil }
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/big"}, nil,
		cardDiffLine(250, 100)+"\n"+pushedLine("c0ffee")+"\n"+resultLine(true, "done"))
	if err != nil || res.Outcome != OutcomePushed || res.Commit != "c0ffee" {
		t.Fatalf("result = (%+v, %v), want the session run to its push", res, err)
	}
	if chat.calls != 1 {
		t.Fatalf("chat = %d calls %+v, want the size trip's check-in", chat.calls, chat.gots)
	}
	if got := chat.gots[0]; strings.Contains(got.System, "|replan") || strings.Contains(got.Input, "- replan:") || strings.Contains(got.Input, "Answer replan") {
		t.Fatalf("the check-in is offered replan without the project:\n%s\n%s", got.System, got.Input)
	}
}

// Only a bound agent card's coding session can be cut, and only where a
// planner can take its rest: a plan's child, or a direct card whose
// project's process has a plan track.
func TestCuttable(t *testing.T) {
	child, direct := Task{Kind: "agent", Source: SourcePlan}, Task{Kind: "agent"}
	for name, tc := range map[string]struct {
		t    Task
		s    sessionSpec
		want bool
	}{
		"a plan's child":                     {child, sessionSpec{}, true},
		"a direct card with a plan track":    {direct, sessionSpec{planTrack: true}, true},
		"a direct card without a plan track": {direct, sessionSpec{}, false},
		"a report session":                   {child, sessionSpec{report: "the guide"}, false},
		"a planner session":                  {child, sessionSpec{planner: true}, false},
		"a human card":                       {Task{Kind: "human"}, sessionSpec{planTrack: true}, false},
		"an unbound sizing card":             {Task{Kind: "agent", Source: SourceSizing}, sessionSpec{planTrack: true}, false},
	} {
		if got := cuttable(tc.t, tc.s); got != tc.want {
			t.Errorf("%s: cuttable = %v, want %v", name, got, tc.want)
		}
	}
}

// The session record is scrubbed with the runner's scrubber: a boot
// secret no generic rule knows is masked by its value.
func TestRunSessionRecordMasksTheBootSecrets(t *testing.T) {
	const secret = "boot-object-storage-secret"
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", SHA: "base", Track: "direct"}
	objects := &fakeObjectStore{}
	runner, _, _ := newTestRunner(t, task, newFakeKube(), objects)
	runner.Scrub = redact.New(secret)
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small"}, nil,
		envLine(harness.Event{Kind: harness.EventCommand, Time: time.Now(), Detail: map[string]any{"command": "env", "exit": 0, "output": "the key " + secret + " is in use"}})+"\n"+
			pushedLine("c0ffee")+"\n"+resultLine(true, "done"))
	if err != nil {
		t.Fatal(err)
	}
	rec := string(objGet(objects, "sessions/t1/"+res.SessionID+".jsonl"))
	if strings.Contains(rec, secret) || !strings.Contains(rec, `"output":"the key [redacted] is in use"`) {
		t.Fatalf("record = %s, want the boot secret masked", rec)
	}
}

// A report session has no card bound and no session count to pace: its
// check-in statement states no pace.
func TestRunSessionOfAReportStatesNoPace(t *testing.T) {
	report := Task{ID: "t1", Status: "running", Objective: "o", Repository: "sf/repo", SHA: "abc123", Process: "maintenance", Parent: "t-parent",
		Source: SourceScreening, Track: policy.TrackDirect, RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 2}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, report, kube, &fakeObjectStore{})
	if err := store.CreateTask(context.Background(), parentTask()); err != nil {
		t.Fatal(err)
	}
	chat := &scriptedChat{answers: []CheckinResponse{{Text: `{"decision":"continue","reason":"one failing command"}`}}}
	runner.checkin = chat
	done := make(chan error)
	go func() {
		_, err := execActivity(t, runner, ActivityRunReport, "t1", "factory/small")
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, loopFailure(1)+loopFailure(2)+loopFailure(3)+resultLine(true, `{"verdict": "clear"}`))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if chat.calls != 1 {
		t.Fatalf("chat = %d calls %+v, want the check-in on same_failure", chat.calls, chat.gots)
	}
	if in := chat.gots[0].Input; strings.Contains(in, "Pace of the card") {
		t.Fatalf("statement states a pace:\n%s", in)
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
		`{"findings":[{"text":"`+finding+`","verdict":"confirmed"}],"decision":"hand_up","reason_kind":"goal_conflict","reason":"the objective contradicts the review"}`)
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m",
		CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: finding, Source: "review"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeCheckinHuman || res.Commit != "" || res.Reason != "the objective contradicts the review" {
		t.Fatalf("result = (%+v, %v), want the human ask before any spend", res, err)
	}
	if res.CheckIn == nil || res.CheckIn.Decision != RungHandUp || res.CheckIn.ReasonKind != CheckGoalConflict ||
		res.CheckIn.Verdicts[finding].State != CheckConfirmed {
		t.Fatalf("check-in = %+v, want the human ask under goal_conflict with the verdict", res.CheckIn)
	}
	if rec := res.CheckIn.Record; rec == nil || rec.Model != "big" || rec.ParentSession != "" ||
		rec.Decision != RungHandUp || rec.ReasonKind != CheckGoalConflict {
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
	dropped := `{"findings":[{"text":"TasksByPR is undefined","verdict":"dropped","evidence":"` + good + `"}],"decision":"reverify"}`

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
			`{"findings":[{"text":"TasksByPR is undefined","verdict":"dropped","evidence":"`+good+`"},{"text":"no test covers the handler","verdict":"confirmed"}],"decision":"reverify"}`)
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
// advanced past the partial line the cut lands in. The check-in's
// card_facts reads the session's own card.
func TestRunSessionCheckinParentRecord(t *testing.T) {
	record := "HEAD-OF-THE-FIRST-LINE " + strings.Repeat("a", 30_000) + "\nTHE-SECOND-LINE\nTHE-THIRD-LINE\n"
	finding := "the objective contradicts the review"
	runner, _, _, worker, chat, _, _ := liveRig(t, CheckinResponse{ToolName: "card_facts"}, CheckinResponse{Text: `{"findings":[{"text":"` + finding +
		`","verdict":"confirmed"}],"decision":"hand_up","reason_kind":"goal_conflict","reason":"the objective contradicts the review"}`})
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
	if _, facts, _ := strings.Cut(chat.got.Input, "tool card_facts null answered:\n"); !strings.HasPrefix(facts, "#7 · ") {
		t.Errorf("card_facts answered %q, want card #7's digest first", facts)
	}
	statement := chat.gots[0].Input
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

// A start check-in never outlives its session: with 2 seconds of agent
// time and the default 30-minute cap, the slow conversation's evidence
// call is cut where the answer's reserve begins and its answer call at the
// session's deadline, the time cap's human ask, not a refused call's
// handover past the RunSession activity's own deadline.
func TestRunSessionStartCheckinStopsAtTheSessionsDeadline(t *testing.T) {
	runner, _, _, _, chat, _, _ := checkinRig(t,
		`{"findings":[{"text":"TasksByPR is undefined","verdict":"confirmed"}],"decision":"handover","handover":"fix the caller first"}`)
	setLimits(runner, func(l *policy.Limits) { l.AgentTime = 2 * time.Second })
	chat.delay = 15 * time.Second

	began := time.Now()
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m",
		CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "TasksByPR is undefined", Source: "review"}}}}, nil)
	if err != nil || res.Outcome != OutcomeCheckinHuman || res.CheckIn == nil || !strings.HasPrefix(res.CheckIn.Reason, checkinTimeCapStop+" after ") {
		t.Fatalf("result = (%+v, %v), want the time cap's human ask, naming the time the check-in had", res, err)
	}
	if had, err := time.ParseDuration(strings.TrimPrefix(res.CheckIn.Reason, checkinTimeCapStop+" after ")); err != nil || had > 2*time.Second {
		t.Fatalf("the stop names %q, want at most the session's 2 seconds", res.CheckIn.Reason)
	}
	if took := time.Since(began); took > 10*time.Second || chat.calls != 2 {
		t.Fatalf("took %s with %d calls: want the evidence call and the answer call, both cut by the session's deadline", took, chat.calls)
	}
}

// A start check-in's time cap is the policy's, cut at the session's
// deadline, and 0 once that deadline passed, never a negative time.
func TestStartCheckinTimeIsCutAtTheDeadline(t *testing.T) {
	for name, tc := range map[string]struct {
		left time.Duration // the time to the session's deadline
		want time.Duration
	}{
		"the deadline is far":   {2 * time.Hour, 30 * time.Minute},
		"the deadline is close": {5 * time.Minute, 5 * time.Minute},
		"the deadline passed":   {-12 * time.Second, 0},
	} {
		if got := startCheckinTime(policy.Limits{}, time.Now().Add(tc.left)); got != tc.want {
			t.Errorf("%s: startCheckinTime = %s, want %s", name, got, tc.want)
		}
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

// A session stopped by its check-in is no session: the timeline holds the
// pod's start but no session_started, and SessionAnswered says no model
// answered for it.
func TestRunSessionCheckinHumanIsNoSession(t *testing.T) {
	finding := "the objective contradicts the review"
	runner, _, _, _, _, _, objects := checkinRig(t,
		`{"findings":[{"text":"`+finding+`","verdict":"confirmed"}],"decision":"hand_up","reason_kind":"goal_conflict","reason":"the objective contradicts the review"}`)
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

// The gates tool shows what the review's claim runs settled: the findings
// a run dropped with its evidence, and the claims no run settled.
func TestRunnerToolsGatesShowTheClaimRuns(t *testing.T) {
	r := &Runner{}
	tools := &runnerTools{r: r, ask: &CheckInAsk{Gates: &GateResults{Pin: 7},
		Review: &Review{
			Dropped:   []Dropped{{Finding: "TestA fails", Evidence: "go test -run TestA ./p at main: exit 0"}},
			Unsettled: []string{"TestB in ./p at head was not run"},
			Upheld:    []string{"TestC fails: go test -run TestC ./p at head (abc): exit 1, the test did fail as claimed:\nFAIL"},
		}}}
	out, _, err := tools.Tool(t.Context(), "gates", nil)
	for _, want := range []string{`"Pin":7`, `"review_dropped":[{`, "TestA fails", "exit 0", `"review_unsettled":["TestB in ./p at head was not run"]`, `"review_upheld":["TestC fails: go test -run TestC ./p at head (abc): exit 1, the test did fail as claimed:\nFAIL"]`} {
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("gates = (%q, %v), want %q", out, err, want)
		}
	}
}

// A replan's planner session reads the old cards' branches: its read
// token covers the bot's fork beside the repository and the document
// repository, its clone container alone gets the fork's URL and the
// branches to fetch, and its brief carries the record's files. A planner
// session with no branch to fetch reads the repository and the document
// repository only.
func TestPlannerSessionClonesTheOldBranches(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		step                  map[string]any
		reads, branches, push string
		files                 []string
	}{
		{"a replan", personReplan, "mint task-t-plan-base read:repository linkshort/linkshort,factory-agent/linkshort,sf/ai-software-factory",
			"factory/t-ui:card/31", "http://forgejo-http.forgejo.svc.cluster.local:3000/factory-agent/linkshort.git",
			[]string{"card-30.md", "card-33.md", "card-32.md", "card-31.md"}},
		{"no replan", nil, "mint task-t-plan-base read:repository linkshort/linkshort,sf/ai-software-factory", "", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, _, kube, hub, _ := oldCardsRig(t, tc.step)
			v := fixtureVersion(t)
			project := v.Projects["linkshort"]
			project.Chat = &policy.ChatConfig{DocRepo: "sf/ai-software-factory"}
			v.Projects["linkshort"] = project
			runner.cfg.Policy = func() policy.Version { return v }
			runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return v, id == 7, nil }
			worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
			worker.start(t, runner)
			done := make(chan error)
			go func() {
				_, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string(nil))
				done <- err
			}()
			awaitPod(t, kube, done)
			kube.scriptPods(0, resultLine(true, "no plan"))
			if err := <-done; err != nil {
				t.Fatalf("RunPlan: %v", err)
			}
			var mints []string
			for _, c := range hub.calls {
				if strings.HasPrefix(c, "mint ") {
					mints = append(mints, c)
				}
			}
			if !slices.Equal(mints, []string{tc.reads}) {
				t.Fatalf("mints %q, want %q", mints, tc.reads)
			}
			clone := containerEnv(initContainer(t, kube.podAt(0).spec["spec"].(map[string]any), "clone"))
			if clone["PUSH_URL"] != tc.push || clone["BRANCHES"] != tc.branches {
				t.Fatalf("clone env PUSH_URL %q, BRANCHES %q; want %q and %q", clone["PUSH_URL"], clone["BRANCHES"], tc.push, tc.branches)
			}
			if env := podEnv(kube.podAt(0)); env["PUSH_URL"] != "" || env["BRANCHES"] != "" {
				t.Fatalf("the agent's container env has PUSH_URL %q, BRANCHES %q; want neither", env["PUSH_URL"], env["BRANCHES"])
			}
			b := onlyBrief(t, worker)
			var names []string
			for _, f := range b.Files {
				names = append(names, f.Name)
			}
			if tc.files == nil && b.Prior != nil || tc.files != nil && (b.Prior == nil || b.Prior.Session != "replan") || !slices.Equal(names, tc.files) {
				t.Fatalf("brief prior %+v, files %q; want files %q", b.Prior, names, tc.files)
			}
		})
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

// The plan's integration card's session pushes the plan's opened proofs
// with its brief, each file the row's code, a hold-out's among them, and
// no stored proof; a feature card of the same plan pushes none.
func TestRunSessionPushesThePlansOpenedProofs(t *testing.T) {
	ctx := context.Background()
	opened := []harness.ProofFile{{Path: "proofs/size_bound/size_bound_test.go", Content: sizeBoundCode},
		{Path: "proofs/key_log/key_log_test.go", Content: "package key_log"}}
	for _, tc := range []struct {
		key  string
		want []harness.ProofFile
	}{{"integration", opened}, {"links-api", nil}} {
		t.Run(tc.key, func(t *testing.T) {
			card := Task{ID: "t1", Kind: "agent", Status: "queued", Track: policy.TrackDirect, Process: "spec", Repository: "sf/r", SHA: "abc123",
				IssueNumber: 4, Source: SourcePlan, Parent: "plan1", PlanKey: tc.key, Author: "sebastian", AuthorWriter: true,
				RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 2, Objective: "Integration: acceptance tests"}
			runner, _, store := newTestRunner(t, card, newFakeKube(), &fakeObjectStore{})
			worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
			worker.start(t, runner)
			store.CreateTask(ctx, Task{ID: "plan1", Kind: "agent", Status: "done", Repository: "sf/r", IssueNumber: 1, Objective: "Link shortener", Track: "direct"})
			raw, sha, _ := planSHA(testPlan())
			store.AddPlanRevision(ctx, "plan1", raw, sha)
			store.proofs = map[string][]PlanProof{"plan1": {
				{Idx: 1, Origin: ProofOriginCriterion, Criterion: "size_bound", ID: "size_bound", Suite: "proofs/size_bound", Name: "TestSizeBound",
					Path: "proofs/size_bound/size_bound_test.go", Code: sizeBoundCode, State: ProofOpened},
				{Idx: 2, Origin: ProofOriginCriterion, Criterion: "door", ID: "door", Suite: "proofs/door", Name: "TestDoor",
					Path: "proofs/door/door_test.go", Code: "package door", State: ProofStored},
				{Idx: 3, Origin: ProofOriginHoldout, ID: "key_log", Suite: "proofs/key_log", Name: "TestKeyLog",
					Path: "proofs/key_log/key_log_test.go", Code: "package key_log", State: ProofOpened},
			}}
			if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small"}, nil,
				resultLine(true, "built")); err != nil {
				t.Fatal(err)
			}
			b := onlyBrief(t, worker)
			if !slices.Equal(b.Proofs, tc.want) {
				t.Fatalf("pushed proofs = %+v, want %+v", b.Proofs, tc.want)
			}
			if strings.Contains(worker.briefs[0], "package door") || tc.want == nil && strings.Contains(worker.briefs[0], `"proofs"`) {
				t.Fatalf("brief push = %s", worker.briefs[0])
			}
		})
	}
}

// The opened proofs a session's clone gets are new files of the card's
// pull request, so their lines raise the card's line bound: a small
// integration card, bound 300 at large_lines 1000, with three opened
// proofs of 100 lines each, is bound at 600. A second session sets the
// same raise again and does not add it twice.
func TestRunSessionRaisesTheBoundByTheOpenedProofsLines(t *testing.T) {
	ctx := context.Background()
	card := Task{ID: "t1", Kind: "agent", Status: "queued", Track: policy.TrackDirect, Process: "spec", Repository: "sf/r", SHA: "abc123",
		IssueNumber: 4, Source: SourcePlan, Parent: "plan1", PlanKey: "integration", Author: "sebastian", AuthorWriter: true, Size: "small",
		RequiredCapabilities: []string{policy.ClassCoder}, BudgetEUR: 2, Objective: "Integration: acceptance tests"}
	kube := newFakeKube()
	runner, _, store := newTestRunner(t, card, kube, &fakeObjectStore{})
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)
	store.CreateTask(ctx, Task{ID: "plan1", Kind: "agent", Status: "done", Repository: "sf/r", IssueNumber: 1, Objective: "Link shortener", Track: "direct"})
	raw, sha, _ := planSHA(testPlan())
	store.AddPlanRevision(ctx, "plan1", raw, sha)
	hundred := strings.Repeat("// a line\n", 100)
	store.proofs = map[string][]PlanProof{"plan1": {
		{Idx: 1, Origin: ProofOriginCriterion, Criterion: "links_crud", ID: "links_crud", Path: "acceptance/links_test.go", Code: hundred, State: ProofOpened},
		{Idx: 2, Origin: ProofOriginCriterion, Criterion: "redirect_302", ID: "redirect_302", Path: "acceptance/redirect_test.go", Code: hundred, State: ProofOpened},
		{Idx: 3, Origin: ProofOriginHoldout, ID: "key_log", Path: "proofs/key_log/key_log_test.go", Code: hundred, State: ProofOpened},
		{Idx: 4, Origin: ProofOriginCriterion, Criterion: "door", ID: "door", Path: "proofs/door/door_test.go", Code: hundred, State: ProofStored},
	}}
	for i := range 2 {
		if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: i + 1, Model: "factory/small"}, nil,
			resultLine(true, "built")); err != nil {
			t.Fatal(err)
		}
		if got := store.tasks["t1"].Usage.ProofLines; got != 300 {
			t.Fatalf("session %d: proof lines = %d, want 300", i+1, got)
		}
		if got := podEnv(kube.podAt(i))["LINE_BOUND"]; got != "600" {
			t.Fatalf("session %d: LINE_BOUND = %q, want 600: 300 for size small and 300 for the proofs", i+1, got)
		}
	}
}

// prevRig is a session rig whose object store holds the record of the
// stopped session s-prev: a todo list, one changed file, its last event at
// 23:03:32 UTC.
func prevRig(t *testing.T, answer string) (*Runner, *fakeWorkerEndpoint) {
	t.Helper()
	runner, _, _, worker, _, _, objects := checkinRig(t, answer)
	rec := carryRecord(t, todoEv(false, [2]string{"in_progress", "fix the gate"}), fileEv("/workspace/repo/g.go"))
	if err := objects.Put(t.Context(), "sessions/t2/s-prev.jsonl", rec); err != nil {
		t.Fatal(err)
	}
	return runner, worker
}

// onlyBrief decodes the one brief the worker got.
func onlyBrief(t *testing.T, w *fakeWorkerEndpoint) harness.BriefPush {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.briefs) != 1 {
		t.Fatalf("briefs = %q, want one", w.briefs)
	}
	var b harness.BriefPush
	if err := json.Unmarshal([]byte(w.briefs[0]), &b); err != nil {
		t.Fatalf("brief %s: %v", w.briefs[0], err)
	}
	return b
}

// A session continuing a stopped one gets the stopped one's working state
// with its brief, dated and with the commit the branch stood at.
func TestRunSessionDeliversThePriorStateWithTheBrief(t *testing.T) {
	runner, worker := prevRig(t, "")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 2, Model: "m", ParentSessionID: "s-prev",
		ParentStop: "supervision trip: wall_clock", ParentOutcome: OutcomeWallClock, ParentTrip: "wall_clock", StartRef: "0123456789abcdef"}, nil,
		pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.Outcome != OutcomePushed {
		t.Fatalf("result = (%+v, %v), want the plain pushed session", res, err)
	}
	b := onlyBrief(t, worker)
	if b.Prior == nil {
		t.Fatal("the brief has no prior state")
	}
	if want := "From session s-prev, stopped at 23:03 UTC with outcome wall_clock after the wall_clock trip, branch at commit 01234567. Written before your session started. Where they differ from the code on the branch, the code is right."; b.Prior.Heading != want {
		t.Errorf("heading = %q, want %q", b.Prior.Heading, want)
	}
	if want := "Stop reason: supervision trip: wall_clock\n\nTodo list at the stop:\n- [in_progress] fix the gate\n\nFiles the session changed:\n- g.go"; b.Prior.Body != want {
		t.Errorf("body = %q, want %q", b.Prior.Body, want)
	}
}

// A retry continues the dead session when it left a record, one event or
// more: its state comes with the brief, its stop named by factoryd, and
// the input's pushed parent gives none. A dead session that recorded no
// event left no record: the input's parent's state comes instead.
func TestRunSessionDeliversTheDeadSessionsState(t *testing.T) {
	cases := map[string]struct {
		in         SessionInput
		dead       sessionProgress
		want, stop string
	}{
		"ran 12 events": {
			SessionInput{TaskID: "t2", Attempt: 2, Model: "m", ParentSessionID: "s-0",
				ParentStop: "pushed branch factory/t2 at c0", ParentOutcome: OutcomePushed, StartRef: "0123456789abcdef"},
			sessionProgress{SessionID: "s-prev", Running: true, Events: 12},
			"From session s-prev, stopped at 23:03 UTC, branch at commit 01234567. Written before your session started. Where they differ from the code on the branch, the code is right.",
			"Stop reason: factoryd's run of it ended without an outcome after at least 12 events\n\n",
		},
		"ran no event": {
			SessionInput{TaskID: "t2", Attempt: 2, Model: "m", ParentSessionID: "s-prev",
				ParentStop: "supervision trip: wall_clock", ParentOutcome: OutcomeNoPush, ParentTrip: "wall_clock", StartRef: "0123456789abcdef"},
			sessionProgress{SessionID: "s-dead", Running: true},
			"From session s-prev, stopped at 23:03 UTC with outcome no_push after the wall_clock trip, branch at commit 01234567. Written before your session started. Where they differ from the code on the branch, the code is right.",
			"Stop reason: supervision trip: wall_clock\n\n",
		},
		"never ran": {
			SessionInput{TaskID: "t2", Attempt: 2, Model: "m", ParentSessionID: "s-prev",
				ParentStop: "supervision trip: wall_clock", ParentOutcome: OutcomeNoPush, ParentTrip: "wall_clock", StartRef: "0123456789abcdef"},
			sessionProgress{SessionID: "s-dead"},
			"From session s-prev, stopped at 23:03 UTC with outcome no_push after the wall_clock trip, branch at commit 01234567. Written before your session started. Where they differ from the code on the branch, the code is right.",
			"Stop reason: supervision trip: wall_clock\n\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			runner, worker := prevRig(t, "")
			res, err := runSession(t, nil, runner, tc.in, &tc.dead, pushedLine("c1")+"\n"+resultLine(true, "pushed"))
			if err != nil || res.Outcome != OutcomePushed {
				t.Fatalf("result = (%+v, %v), want the plain pushed session", res, err)
			}
			b := onlyBrief(t, worker)
			if b.Prior == nil {
				t.Fatal("the brief has no prior state")
			}
			if b.Prior.Heading != tc.want {
				t.Errorf("heading = %q, want %q", b.Prior.Heading, tc.want)
			}
			if !strings.HasPrefix(b.Prior.Body, tc.stop) {
				t.Errorf("body = %q, want it to start %q", b.Prior.Body, tc.stop)
			}
		})
	}
}

// A parent whose approach a check-in model judged wrong followed a dead
// end: its todo list comes labelled as the dead end's.
func TestRunSessionLabelsADeadEndsState(t *testing.T) {
	for _, outcome := range []SessionOutcome{OutcomeRedirect, OutcomeRetryStronger} {
		t.Run(string(outcome), func(t *testing.T) {
			runner, worker := prevRig(t, "")
			res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 2, Model: "m", ParentSessionID: "s-prev",
				ParentStop: "check-in: wrong approach", ParentOutcome: outcome, ParentDeadEnd: true, StartRef: "0123456789abcdef"}, nil,
				pushedLine("c1")+"\n"+resultLine(true, "pushed"))
			if err != nil || res.Outcome != OutcomePushed {
				t.Fatalf("result = (%+v, %v), want the plain pushed session", res, err)
			}
			b := onlyBrief(t, worker)
			if b.Prior == nil {
				t.Fatal("the brief has no prior state")
			}
			if want := "The check-in judged this approach wrong; the todo list below is the dead end's, not a plan to follow."; !strings.HasSuffix(b.Prior.Heading, want) {
				t.Errorf("heading = %q, want it to end %q", b.Prior.Heading, want)
			}
			if want := "Stop reason: check-in: wrong approach\n\nTodo list at the stop (the dead end's):\n- [in_progress] fix the gate"; !strings.HasPrefix(b.Prior.Body, want) {
				t.Errorf("body = %q, want it to start %q", b.Prior.Body, want)
			}
		})
	}
}

// A redirect or a climb no check-in model judged (a cheap-cap stop, a
// failed delivery, the loop's fixed redirect, the unjudged stuck climb)
// leaves a todo list that is still a plan: no dead-end label.
func TestRunSessionLabelsNoUnjudgedStopADeadEnd(t *testing.T) {
	for _, outcome := range []SessionOutcome{OutcomeRedirect, OutcomeRetryStronger} {
		t.Run(string(outcome), func(t *testing.T) {
			runner, worker := prevRig(t, "")
			res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 2, Model: "m", ParentSessionID: "s-prev",
				ParentStop: "check-in: retry_stronger after cheap_cap", ParentOutcome: outcome, StartRef: "0123456789abcdef"}, nil,
				pushedLine("c1")+"\n"+resultLine(true, "pushed"))
			if err != nil || res.Outcome != OutcomePushed {
				t.Fatalf("result = (%+v, %v), want the plain pushed session", res, err)
			}
			b := onlyBrief(t, worker)
			if b.Prior == nil {
				t.Fatal("the brief has no prior state")
			}
			if strings.Contains(b.Prior.Heading, "dead end") || strings.Contains(b.Prior.Body, "dead end") {
				t.Errorf("prior = %+v, want no dead-end label", b.Prior)
			}
		})
	}
}

// Only a decision a check-in model judged marks the stop a dead end: the
// stuck trip's handover. The cheap cap's climb, a failed delivery's
// redirect, the loop's fixed redirect and the unjudged stuck climb do not.
func TestSessionResultMarksOnlyAJudgedStopADeadEnd(t *testing.T) {
	stuck := "stuck after the supervisor's message: repetition: d"
	for name, tc := range map[string]struct {
		trip    *Trip
		d       *Decision
		outcome SessionOutcome
		deadEnd bool
	}{
		"judged stuck handover": {&Trip{Kind: TripRepetition, Detail: "d", Stuck: stuck}, &Decision{Action: ActionRetryStronger, Reason: "n", Guidance: "n", Judged: true}, OutcomeRetryStronger, true},
		"unjudged stuck climb":  {&Trip{Kind: TripRepetition, Detail: "d", Stuck: stuck}, &Decision{Action: ActionRetryStronger, Reason: stuck}, OutcomeRetryStronger, false},
		"cheap cap":             {&Trip{Kind: TripCheapCap, Detail: "d"}, &Decision{Action: ActionRetryStronger, Reason: "d"}, OutcomeRetryStronger, false},
		"failed delivery":       {&Trip{Kind: TripRepetition, Detail: "d"}, &Decision{Action: ActionRedirect, Guidance: "g"}, OutcomeRedirect, false},
		"loop":                  {&Trip{Kind: TripLoop, Detail: "d"}, &Decision{Action: ActionRedirect}, OutcomeRedirect, false},
	} {
		res := sessionResult(sessionSpec{id: "s1"}, sessionOutcome{trip: tc.trip, decision: tc.d, noResult: "stopped"})
		if res.Outcome != tc.outcome || res.DeadEnd != tc.deadEnd {
			t.Errorf("%s: outcome %s, dead end %v; want %s, %v", name, res.Outcome, res.DeadEnd, tc.outcome, tc.deadEnd)
		}
	}
}

// A parent that pushed finished its work: the next session gets no copy
// of its state. A parent without a record gives none either, and the
// session runs on.
func TestRunSessionSendsNoPriorStateAfterAPushedParent(t *testing.T) {
	t.Run("pushed parent", func(t *testing.T) {
		runner, worker := prevRig(t, "")
		res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 2, Model: "m", ParentSessionID: "s-prev",
			ParentStop: "pushed branch factory/t2 at c0", ParentOutcome: OutcomePushed, StartRef: "0123456789abcdef"}, nil,
			pushedLine("c1")+"\n"+resultLine(true, "pushed"))
		if err != nil || res.Outcome != OutcomePushed {
			t.Fatalf("result = (%+v, %v), want the plain pushed session", res, err)
		}
		if b := onlyBrief(t, worker); b.Prior != nil {
			t.Fatalf("prior = %+v, want none after a pushed parent", b.Prior)
		}
	})
	t.Run("no record", func(t *testing.T) {
		runner, _, _, worker, _, _, _ := checkinRig(t, "")
		res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 2, Model: "m", ParentSessionID: "s-prev",
			ParentStop: "supervision trip: wall_clock", ParentOutcome: OutcomeWallClock, StartRef: "0123456789abcdef"}, nil,
			pushedLine("c1")+"\n"+resultLine(true, "pushed"))
		if err != nil || res.Outcome != OutcomePushed {
			t.Fatalf("result = (%+v, %v), want the session to run and push", res, err)
		}
		if b := onlyBrief(t, worker); b.Prior != nil {
			t.Fatalf("prior = %+v, want none without a record", b.Prior)
		}
	})
}

// A handover, the input's or the start check-in's, rides the brief beside
// the prior state.
func TestRunSessionKeepsTheHandoverBesideThePriorState(t *testing.T) {
	t.Run("mid-session handover", func(t *testing.T) {
		runner, worker := prevRig(t, "")
		res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 2, Model: "m", ParentSessionID: "s-prev",
			ParentStop: "supervision trip: wall_clock", ParentOutcome: OutcomeNoPush, StartRef: "0123456789abcdef", Handover: "start with g.go"}, nil,
			pushedLine("c1")+"\n"+resultLine(true, "pushed"))
		if err != nil || res.Outcome != OutcomePushed {
			t.Fatalf("result = (%+v, %v), want the plain pushed session", res, err)
		}
		if b := onlyBrief(t, worker); b.Handover != "start with g.go" || b.Prior == nil || b.Prior.Session != "s-prev" {
			t.Fatalf("brief = %+v, want the handover and s-prev's state", b)
		}
	})
	t.Run("check-in handover", func(t *testing.T) {
		finding := "no test covers the gate"
		runner, worker := prevRig(t, `{"findings":[{"text":"`+finding+`","verdict":"confirmed"}],"decision":"handover","handover":"start with g.go"}`)
		res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 2, Model: "m", ParentSessionID: "s-prev",
			ParentStop: "supervision trip: wall_clock", ParentOutcome: OutcomeNoPush, StartRef: "0123456789abcdef",
			CheckInAsk: &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: finding, Source: "review"}}}}, nil,
			pushedLine("c1")+"\n"+resultLine(true, "pushed"))
		if err != nil || res.Outcome != OutcomePushed || res.CheckIn == nil || res.CheckIn.Decision != CheckHandover {
			t.Fatalf("result = (%+v, %v), want the check-in's handover and the pushed session", res, err)
		}
		if b := onlyBrief(t, worker); b.Handover != "start with g.go" || b.Prior == nil || b.Prior.Session != "s-prev" {
			t.Fatalf("brief = %+v, want the check-in's handover and s-prev's state", b)
		}
	})
}

// Without a model under the check-in's purpose the asked check-in does not
// run, and the empty brief still carries the prior state.
func TestRunSessionCarriesThePriorStateWithoutACheckinModel(t *testing.T) {
	runner, worker := prevRig(t, "")
	noAlias := testPolicyVersion()
	cl := noAlias.Document.Classes["coder"]
	cl.ReviewModel = ""
	noAlias.Document.Classes["coder"] = cl
	runner.cfg.Policy = func() policy.Version { return noAlias }
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 2, Model: "m", ParentSessionID: "s-prev",
		ParentStop: "supervision trip: wall_clock", ParentOutcome: OutcomeNoPush, StartRef: "0123456789abcdef",
		CheckInAsk: &CheckInAsk{Moment: CheckInDead}}, nil,
		pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	if err != nil || res.Outcome != OutcomePushed || res.CheckIn != nil {
		t.Fatalf("result = (%+v, %v), want the plain pushed session without a check-in", res, err)
	}
	b := onlyBrief(t, worker)
	if b.Handover != "" || b.Prior == nil {
		t.Fatalf("brief = %+v, want no handover and s-prev's state", b)
	}
	if want := "From session s-prev, stopped at 23:03 UTC with outcome no_push, branch at commit 01234567. Written before your session started. Where they differ from the code on the branch, the code is right."; b.Prior.Heading != want {
		t.Errorf("heading = %q, want %q", b.Prior.Heading, want)
	}
}

// The statement of every check-in states the card's pace: 350 changed lines
// against the small card's 300, 1.80 of 5 EUR spent, 5 - 1.80 - 0.40 = 2.80 left for a session
// with the gate reserve held out (0 for a 0.30 card with 0.05 spent: 0.30 - 0.05 - 0.40 < 0), session 1 of 4.
func TestRunSessionCheckinStatementsCarryThePace(t *testing.T) {
	t.Run("mid-session", func(t *testing.T) {
		runner, _, _, _, chat, store, _ := liveRig(t, CheckinResponse{Text: `{"decision":"continue","reason":"the rest is tests"}`})
		tk := store.tasks["t2"]
		tk.Size, tk.BudgetEUR, tk.SpentEUR = "small", 5, 1.8
		store.tasks["t2"] = tk
		store.callTotals["t2"] = TaskCallTotals{Calls: 6, PromptTokens: 2500, CachedTokens: 500, OutputTokens: 1000}
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
		for _, want := range []string{"350 changed lines so far against its bound of 300; 1.80 EUR spent, 2.80 left;", "left; session 1 of 4.",
			`Cost and state: {"budget_eur":5,"spent_eur":1.8,"remaining_eur":2.8,"reserve_eur":0.4,"calls":6,"prompt_tokens":2500,"cached_tokens":500,"output_tokens":1000,"state":"running"}`} {
			if !strings.Contains(in, want) {
				t.Fatalf("statement lacks %q:\n%s", want, in)
			}
		}
	})
	t.Run("mid-session before a checkpoint", func(t *testing.T) {
		runner, _, _, _, chat, _, _ := liveRig(t, CheckinResponse{Text: `{"decision":"continue","reason":"one failing command"}`})
		logs := loopFailure(1) + loopFailure(2) + loopFailure(3) + pushedLine("c1") + "\n" + resultLine(true, "done")
		if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Sessions: 4, Model: "factory/small"}, nil, logs); err != nil {
			t.Fatal(err)
		}
		if chat.calls != 1 || !strings.Contains(chat.gots[0].Input, "Pace of the card: changed lines are not measured at this moment;") {
			t.Fatalf("chat = %d calls %+v, want the unmeasured pace", chat.calls, chat.gots)
		}
	})
	t.Run("start", func(t *testing.T) {
		runner, _, _, _, chat, store, _ := checkinRig(t, `{"decision":"handover","handover":"start from the tests"}`)
		store.callTotals["t2"] = TaskCallTotals{Calls: 5, PromptTokens: 2000, CachedTokens: 400, OutputTokens: 800}
		if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Sessions: 4, Model: "m",
			CheckInAsk: &CheckInAsk{Moment: CheckInDead, Stop: "wall clock"}}, nil,
			pushedLine("c1")+"\n"+resultLine(true, "pushed")); err != nil {
			t.Fatal(err)
		}
		if chat.calls != 1 || !strings.Contains(chat.gots[0].Input, "Pace of the card: changed lines are not measured at this moment;") {
			t.Fatalf("chat = %d calls %+v, want the unmeasured pace", chat.calls, chat.gots)
		}
		if !strings.Contains(chat.gots[0].Input, `Cost and state: {"budget_eur":0.3,"spent_eur":0.05,"remaining_eur":0,"reserve_eur":0.4,"calls":5,"prompt_tokens":2000,"cached_tokens":400,"output_tokens":800,"state":"running"}`) {
			t.Fatalf("statement lacks the cost and state block:\n%s", chat.gots[0].Input)
		}
	})
}

// The worker pod carries the card's line bound as LINE_BOUND: the size's
// plan plus the raises its extend answers added — the same effective
// number the gate and the supervisor read. An unsized card is bound by
// the policy's large_lines, like every line-bound check.
func TestWorkerPodSpecCarriesTheCardLineBound(t *testing.T) {
	spec := sessionSpec{id: "s-1", policy: policy.Document{Risk: policy.Risk{LargeLines: 400}}}
	pod := func(t Task) map[string]string {
		p := (&Runner{cfg: testRunnerConfig(), forgejo: &Forgejo{BotUser: "factory-agent"}}).workerPodSpec(t, spec)
		vars := map[string]string{}
		for _, e := range p["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"].([]map[string]any) {
			vars[e["name"].(string)] = e["value"].(string)
		}
		return vars
	}
	if got := pod(Task{ID: "t1", Size: "small"})["LINE_BOUND"]; got != "120" {
		t.Fatalf("LINE_BOUND = %q for a small card under large_lines 400, want 120", got)
	}
	if got := pod(Task{ID: "t1", Size: "small", Usage: TaskUsage{ExtraLines: 30}})["LINE_BOUND"]; got != "150" {
		t.Fatalf("LINE_BOUND = %q for a small card raised by 30, want 150", got)
	}
	if got := pod(Task{ID: "t1"})["LINE_BOUND"]; got != "400" {
		t.Fatalf("LINE_BOUND = %q for a card with no size under large_lines 400, want 400", got)
	}
}

// takingChat is a check-in during whose conversation the newer attempt takes
// the session over; the judge answers a handover, which would stop the pod.
type takingChat struct {
	store Store
	calls atomic.Int32
}

func (c *takingChat) Call(ctx context.Context, _, _ string, _ CheckinRequest) (CheckinResponse, error) {
	c.calls.Add(1)
	c.store.TakeLiveSession(ctx, "t2", "own", "newer")
	return CheckinResponse{Text: `{"decision":"handover","handover":"stop here"}`}, nil
}

// An attempt the session is taken from during its check-in does not act on
// the answer: the pod is the newer attempt's, so it is not stopped, and the
// stale attempt records no end for the session.
func TestStaleAttemptTakenOverDuringACheckinStopsNothing(t *testing.T) {
	runner, hub, kube, _, _, store, objects := liveRig(t)
	chat := &takingChat{store: store}
	runner.checkin = chat
	_, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/small"}, nil, commands(0, 24)+hangLogs)
	var app *temporal.ApplicationError
	if chat.calls.Load() != 1 || !errors.As(err, &app) || app.Message() != errSuperseded.Error() {
		t.Fatalf("check-in calls %d, err %v; want one conversation and the session left to the newer attempt", chat.calls.Load(), err)
	}
	if len(kube.deletedPods) != 0 || len(hub.revoked) != 0 {
		t.Fatalf("deleted pods %v, revoked %v; want the newer attempt's session untouched", kube.deletedPods, hub.revoked)
	}
	if tl := string(objGet(objects, "sessions/t2/timeline.jsonl")); strings.Contains(tl, `"step":"session_end"`) || strings.Contains(tl, `"step":"checkin"`) {
		t.Fatalf("timeline = %s, want no end and no decision from the stale attempt", tl)
	}
}

// takeAtStore hands the session to the newer attempt right before the
// attempt's n-th owner check (holds), so that check is the first to find
// the row another attempt's.
type takeAtStore struct {
	Store
	n     int32
	calls atomic.Int32
}

func (s *takeAtStore) TakeLiveSession(ctx context.Context, taskID, from, to string) (bool, error) {
	if from == to && s.calls.Add(1) == s.n {
		s.Store.TakeLiveSession(ctx, taskID, from, "newer")
	}
	return s.Store.TakeLiveSession(ctx, taskID, from, to)
}

// A stale attempt's trip opens no check-in, and its wall-clock stop deletes
// no pod, the pod started: the newer attempt takes the session at the first
// owner check, onTrip's, for the trip, and at the second, stop()'s after
// onTrip's, for the wall clock. Without onTrip's check the trip opens the
// check-in; without either the wall clock deletes the pod.
func TestStaleAttemptTripsAndStopsDoNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		logs      string
		agentTime time.Duration
		tick      time.Duration
		n         int32
	}{
		"repetition": {commands(0, 24) + hangLogs, 0, time.Hour, 1},
		"wall clock": {envLine(harness.Event{Kind: harness.EventModelCall, Time: time.Now()}) + "\n" + hangLogs, 300 * time.Millisecond, 20 * time.Millisecond, 2},
	} {
		t.Run(name, func(t *testing.T) {
			runner, hub, kube, worker, chat, store, _ := liveRig(t, CheckinResponse{Text: `{"decision":"continue"}`})
			runner.store = &takeAtStore{Store: store, n: tc.n}
			runner.tick = tc.tick
			if tc.agentTime > 0 {
				setLimits(runner, func(l *policy.Limits) { l.AgentTime = tc.agentTime })
			}
			runner.stopWait = 100 * time.Millisecond
			_, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/small"}, nil, tc.logs)
			var app *temporal.ApplicationError
			if !errors.As(err, &app) || app.Message() != errSuperseded.Error() {
				t.Fatalf("err = %v, want the session left to the newer attempt", err)
			}
			worker.mu.Lock()
			asked := slices.ContainsFunc(worker.texts, func(s string) bool { return strings.HasPrefix(s, "POST /wip") })
			worker.mu.Unlock()
			if chat.calls != 0 || asked || len(kube.deletedPods) != 0 || len(hub.revoked) != 0 {
				t.Fatalf("check-in calls %d, /wip asked %v, deleted pods %v, revoked %v; want none", chat.calls, asked, kube.deletedPods, hub.revoked)
			}
		})
	}
}

// The newer attempt takes the session while the stale attempt's message to
// the pod fails: the stop that follows asks first, deletes no pod and says
// nothing on the issue, as the newer attempt supervises on. The
// owner checks before it are onTrip's and the one after the conversation,
// so stop()'s is the third.
func TestStaleAttemptTakenOverBeforeItsStopDeletesNoPod(t *testing.T) {
	runner, hub, kube, worker, chat, store, _ := liveRig(t, CheckinResponse{Text: `{"decision":"continue","guidance":"write the test first"}`})
	worker.status = http.StatusInternalServerError
	runner.store = &takeAtStore{Store: store, n: 3}
	runner.tick = time.Hour
	runner.stopWait = 100 * time.Millisecond
	_, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/small"}, nil, commands(0, 24)+hangLogs)
	var app *temporal.ApplicationError
	if !errors.As(err, &app) || app.Message() != errSuperseded.Error() {
		t.Fatalf("err = %v, want the session left to the newer attempt", err)
	}
	worker.mu.Lock()
	messaged := slices.Contains(worker.texts, "POST /message write the test first")
	worker.mu.Unlock()
	if chat.calls != 1 || !messaged {
		t.Fatalf("check-in calls %d, message sent %v; want the conversation and its failed message before the stop", chat.calls, messaged)
	}
	hub.mu.Lock()
	comments := slices.Clone(hub.comments)
	hub.mu.Unlock()
	if len(kube.deletedPods) != 0 || len(hub.revoked) != 0 || len(comments) != 0 {
		t.Fatalf("deleted pods %v, revoked %v, comments %q; want the newer attempt's session untouched and nothing said", kube.deletedPods, hub.revoked, comments)
	}
}

// The newer attempt takes the session during the check-in's conversation,
// which answers a message: the stale attempt delivers none, records no
// decision and says nothing on the issue.
func TestStaleAttemptTakenOverDuringACheckinSendsNoMessage(t *testing.T) {
	runner, hub, _, worker, chat, store, objects := liveRig(t, CheckinResponse{Text: `{"decision":"continue","guidance":"write the test first"}`})
	chat.onCall = func() { store.TakeLiveSession(context.Background(), "t2", "own", "newer") }
	runner.tick = 200 * time.Millisecond
	_, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "factory/small"}, nil, commands(0, 24)+hangLogs)
	var app *temporal.ApplicationError
	if chat.calls != 1 || !errors.As(err, &app) || app.Message() != errSuperseded.Error() {
		t.Fatalf("check-in calls %d, err %v; want one conversation and the session left to the newer attempt", chat.calls, err)
	}
	worker.mu.Lock()
	messaged := slices.ContainsFunc(worker.texts, func(s string) bool { return strings.HasPrefix(s, "POST /message") })
	worker.mu.Unlock()
	hub.mu.Lock()
	comments := slices.Clone(hub.comments)
	hub.mu.Unlock()
	if messaged || len(comments) != 0 {
		t.Fatalf("message sent %v, comments %q; want neither from the stale attempt", messaged, comments)
	}
	if tl := string(objGet(objects, "sessions/t2/timeline.jsonl")); strings.Contains(tl, `"step":"checkin"`) {
		t.Fatalf("timeline = %s, want no decision from the stale attempt", tl)
	}
}

// A session that ended while its row's release failed leaves the row and
// the pod: the activity fails for Temporal to run it again, and the retry,
// the same activity, reattaches and reads the session's own result, its
// push on the branch.
func TestSessionEndedWithAFailedReleaseIsReadByItsRetry(t *testing.T) {
	task := Task{ID: "t6", Status: "running", Objective: "long", Repository: "sf/repo", Track: "direct"}
	kube := newFakeKube()
	runner, hub, store := newTestRunner(t, task, kube, &fakeObjectStore{})
	runner.store = &failingRelease{Store: store}
	_, err := runSession(t, nil, runner, SessionInput{TaskID: "t6", Attempt: 1, Model: "m"}, nil, pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	var app *temporal.ApplicationError
	if !errors.As(err, &app) || app.Message() != errSuperseded.Error() || len(kube.deletedPods) != 0 {
		t.Fatalf("err = %v, deleted pods %v; want the session left for the retry", err, kube.deletedPods)
	}
	first := store.liveSessions["t6"]
	runner.store = store
	hub.mu.Lock()
	hub.branches = map[string]string{"factory/t6": "c1"}
	hub.mu.Unlock()
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t6", Attempt: 1, Model: "m"}, nil)
	if err != nil || res.Outcome != OutcomePushed || res.Commit != "c1" || res.SessionID != first.SessionID {
		t.Fatalf("retry = (%+v, %v), want the session's own push", res, err)
	}
	if len(kube.podOrder) != 1 || !slices.Contains(kube.deletedPods, first.Namespace+"/"+first.Pod) {
		t.Fatalf("pods created %d, deleted %v; want the one pod, gone at the end", len(kube.podOrder), kube.deletedPods)
	}
	if _, ok := store.liveSessions["t6"]; ok {
		t.Fatal("the live row survived the retry's end")
	}
}

// A row another activity left — a session that ended while its release
// failed, its pod finished with an old result or its delete pending — is no
// session of this activity: the row goes, a fresh pod runs, and the old
// result is never read.
func TestARowOfAnotherActivityIsNeverReattached(t *testing.T) {
	for _, phase := range []string{"Succeeded", "Running"} {
		t.Run(phase, func(t *testing.T) {
			task := Task{ID: "t6", Status: "running", Objective: "long", Repository: "sf/repo", Track: "direct"}
			kube := newFakeKube()
			runner, _, store := newTestRunner(t, task, kube, &fakeObjectStore{})
			runner.store = &failingRelease{Store: store}
			if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t6", Attempt: 1, Model: "m"}, nil, pushedLine("c1")+"\n"+resultLine(true, "pushed")); err == nil {
				t.Fatal("the first session ended without its release failing")
			}
			old := store.liveSessions["t6"]
			old.Activity = "run-0/9"
			store.liveSessions["t6"] = old
			kube.mu.Lock()
			kube.pods[old.Namespace+"/"+old.Pod].phase = phase
			kube.mu.Unlock()
			runner.store = store
			res, err := runSession(t, nil, runner, SessionInput{TaskID: "t6", Attempt: 2, Model: "m"}, nil, pushedLine("c2")+"\n"+resultLine(true, "pushed"))
			if err != nil || res.Commit != "c2" || res.SessionID == old.SessionID {
				t.Fatalf("result = (%+v, %v), want the new session's own push", res, err)
			}
			if len(kube.podOrder) != 2 || !slices.Contains(kube.deletedPods, old.Namespace+"/"+old.Pod) {
				t.Fatalf("pods created %d, deleted %v; want a fresh pod and the old one gone", len(kube.podOrder), kube.deletedPods)
			}
		})
	}
}

// A pod in the phase Unknown, or with no phase, may still run on a node the
// cluster lost: the retry deletes and revokes nothing, and fails for
// Temporal to run it again.
func TestUnknownPodPhaseDeletesAndRevokesNothing(t *testing.T) {
	for _, phase := range []string{"Unknown", ""} {
		t.Run(cmp.Or(phase, "none"), func(t *testing.T) {
			task := Task{ID: "t6", Status: "running", Objective: "long", Repository: "sf/repo", Track: "direct"}
			kube := newFakeKube()
			runner, hub, store := newTestRunner(t, task, kube, &fakeObjectStore{})
			runner.store = &failingRelease{Store: store}
			if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t6", Attempt: 1, Model: "m"}, nil, pushedLine("c1")+"\n"+resultLine(true, "pushed")); err == nil {
				t.Fatal("the session ended without its release failing")
			}
			l := store.liveSessions["t6"]
			kube.mu.Lock()
			kube.pods[l.Namespace+"/"+l.Pod].phase = phase
			deleted := len(kube.deletedPods)
			kube.mu.Unlock()
			hub.mu.Lock()
			revoked := len(hub.revoked)
			hub.mu.Unlock()
			runner.store = store
			// a fresh pod, which the retry must not start, would end at once
			if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t6", Attempt: 1, Model: "m"}, nil, resultLine(true, "fresh")); err == nil || !strings.Contains(err.Error(), "phase") {
				t.Fatalf("err = %v, want the unknown phase retried", err)
			}
			if _, ok := store.liveSessions["t6"]; !ok || len(kube.deletedPods) != deleted || len(hub.revoked) != revoked || len(kube.podOrder) != 1 {
				t.Fatalf("row kept %v, deleted pods %v, revoked %v, pods %d; want nothing deleted, revoked or started", ok, kube.deletedPods, hub.revoked, len(kube.podOrder))
			}
		})
	}
}

// staleRelease is a release that fails after the newer attempt took the
// session over.
type staleRelease struct{ Store }

func (s staleRelease) ReleaseLiveSession(ctx context.Context, taskID, owner string) (bool, error) {
	s.Store.TakeLiveSession(ctx, taskID, owner, "newer")
	return false, errors.New("the store is away")
}

// A stale attempt whose session ended and whose release fails leaves the
// newer attempt's session whole: no pod deleted, no token revoked, no end
// recorded.
func TestStaleAttemptWhoseReleaseFailsLeavesTheNewerAttemptsSession(t *testing.T) {
	task := Task{ID: "t6", Status: "running", Objective: "long", Repository: "sf/repo", Track: "direct"}
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, store := newTestRunner(t, task, kube, objects)
	runner.store = staleRelease{Store: store}
	_, err := runSession(t, nil, runner, SessionInput{TaskID: "t6", Attempt: 1, Model: "m"}, nil, pushedLine("c1")+"\n"+resultLine(true, "pushed"))
	var app *temporal.ApplicationError
	if !errors.As(err, &app) || app.Message() != errSuperseded.Error() {
		t.Fatalf("err = %v, want the session left to the newer attempt", err)
	}
	hub.mu.Lock()
	revoked := slices.Clone(hub.revoked)
	hub.mu.Unlock()
	if len(kube.deletedPods) != 0 || len(revoked) != 0 {
		t.Fatalf("deleted pods %v, revoked %v; want the newer attempt's session untouched", kube.deletedPods, revoked)
	}
	if _, ok := kube.namespaces["factory-agents-t6"]; !ok {
		t.Fatal("the stale attempt deleted the newer attempt's namespace")
	}
	if tl := string(objGet(objects, "sessions/t6/timeline.jsonl")); strings.Contains(tl, `"step":"session_end"`) {
		t.Fatalf("timeline = %s, want no end from the stale attempt", tl)
	}
}

type failingRelease struct{ Store }

func (failingRelease) ReleaseLiveSession(context.Context, string, string) (bool, error) {
	return false, errors.New("the store is away")
}

// An unknown pod status is no dead pod: nothing is deleted or revoked, and
// the activity fails for Temporal to run again.
func TestUnknownPodStatusDeletesAndRevokesNothing(t *testing.T) {
	task := Task{ID: "t6", Status: "running", Objective: "long", Repository: "sf/repo", Track: "direct"}
	kube := newFakeKube()
	runner, hub, store := newTestRunner(t, task, kube, &fakeObjectStore{})
	store.PutLiveSession(context.Background(), LiveSession{TaskID: "t6", SessionID: "s-old", Namespace: "ns", Pod: "pod", MessageToken: "tok", Owner: "old"})
	kube.statusErr = map[string]error{"ns/pod": errors.New("the API server is away")}
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t6", Attempt: 1, Model: "m"}, nil); err == nil {
		t.Fatal("the activity went on past an unknown pod status")
	}
	if _, ok, _ := store.LiveSession(context.Background(), "t6"); !ok {
		t.Fatal("the live session row was deleted on an unknown pod status")
	}
	if len(kube.deletedPods) != 0 || len(hub.revoked) != 0 {
		t.Fatalf("deleted pods %v, revoked %v; want nothing", kube.deletedPods, hub.revoked)
	}
}

// The guard of a runner that expects no supervisor call reports each
// call with its source, and no more of the token than 12 characters.
func TestUnexpectedSupervisorNamesTheStray(t *testing.T) {
	var reports []string
	m := unexpectedSupervisor(t, func(format string, args ...any) { reports = append(reports, fmt.Sprintf(format, args...)) })
	req, _ := http.NewRequest(http.MethodPost, m.GatewayURL+"/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer 0123456789abcdefSECRET")
	req.Header.Set("User-Agent", "stray/1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(reports) != 1 || !strings.HasPrefix(reports[0], "unexpected supervisor call POST /v1/chat/completions from 127.0.0.1:") ||
		!strings.HasSuffix(reports[0], `, User-Agent "stray/1", token "0123456789ab"`) || strings.Contains(reports[0], "SECRET") {
		t.Fatalf("reports = %q", reports)
	}
}

// stepsOf are task id's timeline steps, each its name and the detail keys
// in keys.
func stepsOf(t *testing.T, r *Runner, id string, keys ...string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range timelineLines(t, r, id) {
		m := map[string]any{"step": line["step"]}
		for _, k := range keys {
			if v, ok := line[k]; ok {
				m[k] = v
			}
		}
		out = append(out, m)
	}
	return out
}
