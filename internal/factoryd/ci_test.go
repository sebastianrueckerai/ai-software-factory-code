package factoryd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/hcloud"
)

// ciHub scripts the Forgejo Actions runner-registrar API (task 41): the
// repo-scoped jobs search, ephemeral registration, listing, and deletion
// endpoints, authenticated by the registrar's own bearer-style token
// header, never the bot user's basic auth.
type ciHub struct {
	mu         sync.Mutex
	jobs       []CIJob
	runners    map[int64]string // id → name, currently registered
	registered []string         // registration name, in call order
	deleted    []int64
	nextID     int64
	wantToken  string
}

func (h *ciHub) srv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.wantToken != "" && r.Header.Get("Authorization") != "token "+h.wantToken {
			t.Errorf("ci call %s %s: auth = %q, want the registrar token", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/actions/runners/jobs"):
			label := r.URL.Query().Get("labels")
			matched := make([]CIJob, 0, len(h.jobs))
			for _, j := range h.jobs {
				if label == "" || slices.Contains(j.RunsOn, label) {
					matched = append(matched, j)
				}
			}
			body, _ := json.Marshal(matched)
			w.Write(body)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/actions/runners"):
			refs := make([]CIRunnerRef, 0, len(h.runners))
			for id, name := range h.runners {
				refs = append(refs, CIRunnerRef{ID: id, Name: name})
			}
			sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
			body, _ := json.Marshal(refs)
			w.Write(body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions/runners"):
			var in struct {
				Name      string `json:"name"`
				Ephemeral bool   `json:"ephemeral"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			if !in.Ephemeral {
				t.Errorf("runner %q registered without ephemeral: true", in.Name)
			}
			h.nextID++
			h.registered = append(h.registered, in.Name)
			if h.runners == nil {
				h.runners = map[int64]string{}
			}
			h.runners[h.nextID] = in.Name
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d,"uuid":"uuid-%d","token":"tok-%d"}`, h.nextID, h.nextID, h.nextID)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/actions/runners/"):
			var id int64
			fmt.Sscanf(r.URL.Path, "/api/v1/repos/sf/ai-software-factory-code/actions/runners/%d", &id)
			if _, ok := h.runners[id]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			delete(h.runners, id)
			h.deleted = append(h.deleted, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected ci call %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testCIConfig() CIConfig {
	return CIConfig{
		FactoryNamespace: "factory",
		Owner:            "sf",
		Repo:             "ai-software-factory-code",
		Label:            "pr-untrusted",
		RunnerImage:      "reg/runner@sha256:abc",
		PullSecret:       "forgejo-registry",
		GitBaseURL:       "http://forgejo-http.forgejo.svc.cluster.local:3000",
		PollInterval:     time.Millisecond,
		JobTimeout:       45 * time.Minute,
	}
}

// newTestCISpawner wires a spawner against a scripted registrar and a fake
// cluster carrying the same source pull secret the agent-sandbox tests seed.
func newTestCISpawner(t *testing.T, kube *fakeKube, hub *ciHub) *CISpawner {
	t.Helper()
	hub.wantToken = "registrar-tok"
	srv := hub.srv(t)
	forgejo := &Forgejo{BaseURL: srv.URL, CIRegistrarToken: "registrar-tok"}
	kube.sourceData["factory/forgejo-registry"] = map[string]string{".dockerconfigjson": "{}"}
	builds, _, _ := newTestBuildVMs(t, 2)
	builds.collector.OwnerDone = CIOwnerDone(kube)
	builds.collector.ReleaseAttempts = 1 // as cmd/factoryd wires it
	builds.collector.RetryWait = time.Hour
	spawner, err := NewCISpawner(testCIConfig(), forgejo, kube, builds)
	if err != nil {
		t.Fatal(err)
	}
	return spawner
}

func TestNewCISpawnerRequiresConfig(t *testing.T) {
	kube := newFakeKube()
	forgejo := &Forgejo{BaseURL: "http://x", CIRegistrarToken: "t"}
	cfg := testCIConfig()
	cfg.Owner = ""
	builds, _, _ := newTestBuildVMs(t, 2)
	if _, err := NewCISpawner(cfg, forgejo, kube, builds); err == nil {
		t.Fatal("want an error for a missing Owner")
	}
	if _, err := NewCISpawner(testCIConfig(), &Forgejo{BaseURL: "http://x"}, kube, builds); err == nil {
		t.Fatal("want an error for a missing CIRegistrarToken")
	}
	if _, err := NewCISpawner(testCIConfig(), forgejo, kube, nil); err == nil {
		t.Fatal("want an error for missing build VMs")
	}
}

func TestCISpawnerSpawnsRunnerForWaitingJob(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 42, RunsOn: []string{"pr-untrusted"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)

	spawner.poll(context.Background())

	ns := "factory-agents-ci-42"
	labels, ok := kube.namespaces[ns]
	if !ok {
		t.Fatalf("namespace %s not created", ns)
	}
	// factory.ai/ci is what the build cache's network policy admits for reads
	if labels["factory.ai/task"] != "ci-42" || labels["pod-security.kubernetes.io/enforce"] != "restricted" || labels["factory.ai/ci"] != "true" {
		t.Fatalf("namespace labels = %v", labels)
	}
	if len(hub.registered) != 1 {
		t.Fatalf("registered = %v, want one runner", hub.registered)
	}
	pod, ok := kube.pods[ns+"/"+ciPodName]
	if !ok {
		t.Fatal("runner pod not created")
	}
	spec := pod.spec["spec"].(map[string]any)
	if spec["runtimeClassName"] != "gvisor" || spec["automountServiceAccountToken"] != false {
		t.Fatalf("pod hardening: runtimeClassName=%v automount=%v", spec["runtimeClassName"], spec["automountServiceAccountToken"])
	}
	if spec["restartPolicy"] != "Never" {
		t.Fatalf("restartPolicy = %v, want Never", spec["restartPolicy"])
	}
	if secs, ok := spec["activeDeadlineSeconds"].(int64); !ok || secs != 45*60 {
		t.Fatalf("activeDeadlineSeconds = %v, want 2700", spec["activeDeadlineSeconds"])
	}
	container := spec["containers"].([]any)[0].(map[string]any)
	args := fmt.Sprint(container["args"])
	for _, want := range []string{"one-job", "--url", "http://forgejo-http.forgejo.svc.cluster.local:3000", "$(RUNNER_UUID)", "file:/var/run/secrets/ci/token", "pr-untrusted:host", "--wait"} {
		if !strings.Contains(args, want) {
			t.Fatalf("runner args = %s, missing %q", args, want)
		}
	}
	sc := container["securityContext"].(map[string]any)
	if sc["readOnlyRootFilesystem"] != true || sc["runAsNonRoot"] != true || sc["allowPrivilegeEscalation"] != false {
		t.Fatalf("container securityContext = %v", sc)
	}
	if caps := fmt.Sprint(sc["capabilities"]); caps != "map[drop:[ALL]]" {
		t.Fatalf("capabilities = %s, want drop ALL", caps)
	}
	mounts := fmt.Sprint(container["volumeMounts"])
	for _, path := range []string{"/workspace", "/home/runner", "/tmp", "/var/run/secrets/ci"} {
		if !strings.Contains(mounts, path) {
			t.Fatalf("volumeMounts = %s, missing %s", mounts, path)
		}
	}
	reg := kube.secrets[ns+"/"+ciSecretName]
	if reg["token"] == "" || reg["uuid"] == "" || reg["runner_id"] == "" {
		t.Fatalf("registration secret = %v", reg)
	}
	if typ := kube.secretTypes[ns+"/"+ciSecretName]; typ != "Opaque" {
		t.Fatalf("registration secret type = %q, want Opaque", typ)
	}
}

func TestCISpawnerIgnoresNonWaitingJobs(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 1, Status: "running"}, {ID: 2, Status: "success"}}}
	spawner := newTestCISpawner(t, kube, hub)

	spawner.poll(context.Background())

	if len(hub.registered) != 0 {
		t.Fatalf("registered = %v, want none: no job was waiting", hub.registered)
	}
}

func TestCISpawnerIdempotentAcrossPolls(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 7, RunsOn: []string{"pr-untrusted"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)

	spawner.poll(context.Background())
	spawner.poll(context.Background())

	if len(hub.registered) != 1 {
		t.Fatalf("registered = %v, want exactly one runner across two polls", hub.registered)
	}
	if n := len(kube.namespaces); n != 1 {
		t.Fatalf("namespaces = %d, want 1", n)
	}
}

func TestCISpawnerIdempotentAcrossRestart(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 9, RunsOn: []string{"pr-untrusted"}, Status: "waiting"}}}
	hub.wantToken = "registrar-tok"
	srv := hub.srv(t)
	forgejo := &Forgejo{BaseURL: srv.URL, CIRegistrarToken: "registrar-tok"}
	kube.sourceData["factory/forgejo-registry"] = map[string]string{".dockerconfigjson": "{}"}
	cfg := testCIConfig()
	builds, _, _ := newTestBuildVMs(t, 2)

	first, err := NewCISpawner(cfg, forgejo, kube, builds)
	if err != nil {
		t.Fatal(err)
	}
	first.poll(context.Background())

	// A restarted factoryd builds a brand new CISpawner against the same
	// cluster and registrar state; the namespace label, not process memory,
	// is what must stop a second registration.
	restarted, err := NewCISpawner(cfg, forgejo, kube, builds)
	if err != nil {
		t.Fatal(err)
	}
	restarted.poll(context.Background())

	if len(hub.registered) != 1 {
		t.Fatalf("registered = %v across a simulated restart, want exactly one", hub.registered)
	}
}

// At most MaxConcurrent runner namespaces live at once; further waiting
// jobs stay queued in Forgejo until a slot frees.
func TestCISpawnerCapsConcurrentRunners(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{
		{ID: 1, RunsOn: []string{"pr-untrusted"}, Status: "waiting"},
		{ID: 2, RunsOn: []string{"pr-untrusted"}, Status: "waiting"},
		{ID: 3, RunsOn: []string{"pr-untrusted"}, Status: "waiting"},
	}}
	spawner := newTestCISpawner(t, kube, hub)
	spawner.cfg.MaxConcurrent = 2
	ctx := context.Background()
	spawner.poll(ctx)
	spawner.poll(ctx)
	for id, want := range map[string]bool{"factory-agents-ci-1": true, "factory-agents-ci-2": true, "factory-agents-ci-3": false} {
		if _, ok := kube.namespaces[id]; ok != want {
			t.Fatalf("namespace %s present = %v, want %v with a cap of 2", id, ok, want)
		}
	}
	kube.pods["factory-agents-ci-1/"+ciPodName].phase = "Succeeded"
	hub.jobs[0].Status = "success"
	spawner.poll(ctx)
	if _, ok := kube.namespaces["factory-agents-ci-1"]; ok {
		t.Fatal("finished namespace survived the poll")
	}
	if _, ok := kube.namespaces["factory-agents-ci-3"]; !ok {
		t.Fatal("job 3 not spawned once a slot freed")
	}
}

// A namespace whose pod status cannot be read counts as live: the cap
// fails closed, not open.
func TestCISpawnerCapCountsUnreadableNamespacesAsLive(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{
		{ID: 1, RunsOn: []string{"pr-untrusted"}, Status: "waiting"},
		{ID: 2, RunsOn: []string{"pr-untrusted"}, Status: "waiting"},
	}}
	spawner := newTestCISpawner(t, kube, hub)
	spawner.cfg.MaxConcurrent = 1
	ctx := context.Background()
	spawner.poll(ctx)
	kube.statusErr = map[string]error{"factory-agents-ci-1/" + ciPodName: fmt.Errorf("apiserver says no")}
	spawner.poll(ctx)
	if _, ok := kube.namespaces["factory-agents-ci-2"]; ok {
		t.Fatal("job 2 spawned past the cap while namespace 1's status was unreadable")
	}
	if _, ok := kube.namespaces["factory-agents-ci-1"]; !ok {
		t.Fatal("namespace 1 torn down on an unreadable status")
	}
}

func TestCISpawnerReconcileTearsDownSucceededPod(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 5, RunsOn: []string{"pr-untrusted"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)
	spawner.poll(context.Background())
	ns := "factory-agents-ci-5"
	kube.pods[ns+"/"+ciPodName].phase = "Succeeded"

	spawner.reconcile(context.Background())

	if _, ok := kube.namespaces[ns]; ok {
		t.Fatal("finished namespace survived reconcile")
	}
	if len(hub.deleted) != 1 {
		t.Fatalf("deleted registrations = %v, want the runner removed", hub.deleted)
	}
}

// A pod that hit activeDeadlineSeconds reads as Failed with no special
// casing: the same teardown path as a normal failure applies.
func TestCISpawnerReconcileTearsDownFailedPod(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 15, RunsOn: []string{"pr-untrusted"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)
	spawner.poll(context.Background())
	ns := "factory-agents-ci-15"
	kube.pods[ns+"/"+ciPodName].phase = "Failed"

	spawner.reconcile(context.Background())

	if _, ok := kube.namespaces[ns]; ok {
		t.Fatal("deadline-exceeded namespace survived reconcile")
	}
	if len(hub.deleted) != 1 {
		t.Fatalf("deleted registrations = %v, want the runner removed", hub.deleted)
	}
}

func TestCISpawnerReconcileLeavesRunningPodAlone(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 6, RunsOn: []string{"pr-untrusted"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)
	spawner.poll(context.Background())
	ns := "factory-agents-ci-6" // fakeKube.CreatePod defaults phase to "Running"

	spawner.reconcile(context.Background())

	if _, ok := kube.namespaces[ns]; !ok {
		t.Fatal("running namespace was torn down")
	}
	if len(hub.deleted) != 0 {
		t.Fatalf("deleted = %v, want none while the pod runs", hub.deleted)
	}
}

// reconcile must not touch agent-task namespaces: they carry the same
// pod-security label but a different name prefix and no ci-runner pod.
func TestCISpawnerReconcileIgnoresAgentNamespaces(t *testing.T) {
	kube := newFakeKube()
	kube.namespaces["factory-agents-t1"] = map[string]string{
		"factory.ai/task": "t1", "pod-security.kubernetes.io/enforce": "restricted",
	}
	hub := &ciHub{}
	spawner := newTestCISpawner(t, kube, hub)

	spawner.reconcile(context.Background())

	if _, ok := kube.namespaces["factory-agents-t1"]; !ok {
		t.Fatal("agent task namespace was swept by the CI reconciler")
	}
}

// A setup failure after the namespace is created must still give back the
// runner registration; the half-built namespace itself is left for the next
// reconcile tick, which sees no pod and tears it down.
func TestCISpawnerRollsBackRunnerOnSetupFailure(t *testing.T) {
	kube := newFakeKube()
	kube.saErr = fmt.Errorf("apiserver says no")
	hub := &ciHub{jobs: []CIJob{{ID: 11, RunsOn: []string{"pr-untrusted"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)

	spawner.poll(context.Background())

	if len(hub.registered) != 1 || len(hub.deleted) != 1 {
		t.Fatalf("registered = %v, deleted = %v: a setup failure must roll back the registration", hub.registered, hub.deleted)
	}
	ns := "factory-agents-ci-11"
	if _, ok := kube.namespaces[ns]; !ok {
		t.Fatal("half-built namespace should still exist until the next reconcile")
	}

	spawner.reconcile(context.Background())

	if _, ok := kube.namespaces[ns]; ok {
		t.Fatal("half-built namespace survived a reconcile tick")
	}
	if len(hub.deleted) != 1 {
		t.Fatalf("deleted = %v, want no second delete for a registration secret that was never written", hub.deleted)
	}
}

func TestCIEgressSpecAllowlist(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 21, RunsOn: []string{"pr-untrusted"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)

	spawner.poll(context.Background())

	ns := "factory-agents-ci-21"
	cnp := kube.cnps[ns+"/ci-egress"]
	if cnp == nil {
		t.Fatal("ci egress CNP missing")
	}
	cnpStr := fmt.Sprint(cnp)
	if !strings.Contains(cnpStr, "169.254.169.254/32") {
		t.Fatal("egress missing the metadata-address deny")
	}
	for _, host := range []string{
		"proxy.golang.org", "sum.golang.org", "data.forgejo.org",
		"storage.googleapis.com",
	} {
		if !strings.Contains(cnpStr, host) {
			t.Fatalf("egress allowlist missing %s: %s", host, cnpStr)
		}
	}

	var fqdnRule, dnsRule map[string]any
	for _, rule := range cnp["egress"].([]any) {
		r := rule.(map[string]any)
		switch {
		case r["toFQDNs"] != nil:
			fqdnRule = r
		case strings.Contains(fmt.Sprint(r["toEndpoints"]), "kube-dns"):
			dnsRule = r
		}
	}
	if fqdnRule == nil || !strings.Contains(fmt.Sprint(fqdnRule["toFQDNs"]), "storage.googleapis.com") {
		t.Fatalf("443 toFQDNs rule missing storage.googleapis.com (proxy.golang.org redirects zip downloads there): %v", fqdnRule)
	}
	if dnsRule == nil || !strings.Contains(fmt.Sprint(dnsRule), "storage.googleapis.com") {
		t.Fatalf("DNS rule missing storage.googleapis.com: %v", dnsRule)
	}
	// an ordinary job drives no build: no Docker Hub host at all
	if strings.Contains(cnpStr, "docker.io") || strings.Contains(cnpStr, "docker.com") {
		t.Fatalf("ordinary runner egress reaches Docker Hub: %s", cnpStr)
	}
	if !strings.Contains(cnpStr, "k8s:app.kubernetes.io/name:forgejo") || !strings.Contains(cnpStr, "port:3000") {
		t.Fatal("egress missing the in-cluster forgejo allow")
	}
	if dnsRule == nil || !strings.Contains(fmt.Sprint(dnsRule), "build-cache.forgejo.svc.cluster.local") {
		t.Fatalf("DNS rule missing the build cache: %v", dnsRule)
	}
	var cacheRule map[string]any
	for _, rule := range cnp["egress"].([]any) {
		r := rule.(map[string]any)
		if strings.Contains(fmt.Sprint(r["toEndpoints"]), "build-cache") {
			cacheRule = r
		}
	}
	wantCache := map[string]any{
		"toEndpoints": []any{map[string]any{
			"matchLabels": map[string]string{
				"k8s:io.kubernetes.pod.namespace": "forgejo",
				"k8s:app.kubernetes.io/name":      "build-cache",
			},
		}},
		"toPorts": []any{map[string]any{
			"ports": []any{map[string]any{"port": "5000", "protocol": "TCP"}},
		}},
	}
	if !reflect.DeepEqual(cacheRule, wantCache) {
		t.Fatalf("build cache egress rule = %v, want %v", cacheRule, wantCache)
	}
	if strings.Contains(cnpStr, "kubernetes.default") || strings.Contains(cnpStr, "443 protocol:TCP] map[port:6443") {
		t.Fatal("egress must not reach the Kubernetes API")
	}
}

func TestCISpawnerPullSecretCopiedNoOtherCredentials(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 30, RunsOn: []string{"pr-untrusted"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)

	spawner.poll(context.Background())

	ns := "factory-agents-ci-30"
	if typ := kube.secretTypes[ns+"/forgejo-registry"]; typ != "kubernetes.io/dockerconfigjson" {
		t.Fatalf("pull secret type = %q", typ)
	}
	wantSecrets := map[string]bool{ns + "/" + ciSecretName: true, ns + "/forgejo-registry": true}
	for key := range kube.secrets {
		if strings.HasPrefix(key, ns+"/") && !wantSecrets[key] {
			t.Fatalf("unexpected secret %s in the CI namespace: only the runner registration and the pull secret belong there", key)
		}
	}
}

// The factoryd-task ClusterRole grants only create, not get, on secrets
// (finding 1, fix round 1): a Forbidden read of the registration secret
// must not be mistaken for "no secret was ever written" — the namespace
// must survive so the next reconcile tick retries the read, instead of
// silently losing the deregistration forever.
func TestCISpawnerReconcileRetriesForbiddenRegistrationRead(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 8, RunsOn: []string{"pr-untrusted"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)
	spawner.poll(context.Background())
	ns := "factory-agents-ci-8"
	kube.pods[ns+"/"+ciPodName].phase = "Succeeded"
	kube.getErr = map[string]error{ns + "/" + ciSecretName: ErrForbidden}

	spawner.reconcile(context.Background())

	if _, ok := kube.namespaces[ns]; !ok {
		t.Fatal("namespace was torn down despite a Forbidden registration read")
	}
	if len(hub.deleted) != 0 {
		t.Fatalf("deleted = %v, want no deregistration while the read is Forbidden", hub.deleted)
	}

	delete(kube.getErr, ns+"/"+ciSecretName)
	spawner.reconcile(context.Background())

	if _, ok := kube.namespaces[ns]; ok {
		t.Fatal("namespace survived once the registration read succeeded")
	}
	if len(hub.deleted) != 1 {
		t.Fatalf("deleted = %v, want the runner deregistered once the read succeeds", hub.deleted)
	}
}

// A crash between RegisterCIRunner succeeding and any local write (the
// namespace, the registration secret) leaves an orphaned Forgejo
// registration with nothing local to recover it from (finding 2, fix round
// 1). Forgejo's own runner list is the only record that survives that
// crash: reconcile must find and deregister a runner named for a job that
// has no namespace.
func TestCISpawnerReconcileSweepsOrphanRunnerRegistration(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{}
	spawner := newTestCISpawner(t, kube, hub)
	// a runner registered under spawn's naming convention, but the process
	// crashed before CreateNamespace ever ran for job 99
	hub.runners = map[int64]string{99: "ci-job-99"}

	spawner.reconcile(context.Background())

	if len(hub.deleted) != 1 || hub.deleted[0] != 99 {
		t.Fatalf("deleted = %v, want the orphan runner 99 deregistered", hub.deleted)
	}
}

// A runner whose job's namespace exists — spawning or already running —
// must survive sweep: only a registration with no namespace to show for it
// is an orphan.
func TestCISpawnerReconcileLeavesActiveRunnerRegistrationAlone(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 6, RunsOn: []string{"pr-untrusted"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)
	spawner.poll(context.Background()) // registers ci-job-6 and creates its namespace

	spawner.reconcile(context.Background())

	if len(hub.deleted) != 0 {
		t.Fatalf("deleted = %v, want the active job's runner registration left alone", hub.deleted)
	}
}

// A registration whose name doesn't match spawn's ci-job-<id> convention —
// one a human or another process registered — must never be swept.
func TestCISpawnerReconcileIgnoresForeignRunnerNames(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{}
	spawner := newTestCISpawner(t, kube, hub)
	hub.runners = map[int64]string{5: "some-other-runner"}

	spawner.reconcile(context.Background())

	if len(hub.deleted) != 0 {
		t.Fatalf("deleted = %v, want a non-spawner runner name left alone", hub.deleted)
	}
}

func ciBuildFakes(s *CISpawner) (*fakeHetzner, *cloudres.Memory) {
	return s.builds.provider.(*fakeHetzner), s.builds.registry.(*cloudres.Memory)
}

func podContainer(t *testing.T, kube *fakeKube, ns string) (spec, container map[string]any) {
	t.Helper()
	pod, ok := kube.pods[ns+"/"+ciPodName]
	if !ok {
		t.Fatalf("runner pod in %s not created", ns)
	}
	spec = pod.spec["spec"].(map[string]any)
	return spec, spec["containers"].([]any)[0].(map[string]any)
}

// A pr-untrusted-build job (ruling 110) gets a throwaway VM: the runner
// pod, in a restricted namespace like every CI job, carries the VM's
// address and a mounted client pair, and its egress admits exactly that
// VM on the buildkitd port.
func TestCISpawnerBuildJobGetsBuildVM(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 50, RunsOn: []string{"pr-untrusted-build"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)
	hz, reg := ciBuildFakes(spawner)

	spawner.poll(context.Background())

	ns := "factory-agents-ci-50"
	if enforce := kube.namespaces[ns]["pod-security.kubernetes.io/enforce"]; enforce != "restricted" {
		t.Fatalf("enforce = %q, want restricted", enforce)
	}
	if strings.Join(hz.calls, ",") != "create firewall/101,create server/102" {
		t.Fatalf("hetzner calls = %v", hz.calls)
	}
	rows, _ := reg.OpenByOwner(context.Background(), "ci", "50")
	if len(rows) != 2 {
		t.Fatalf("registry rows for job 50 = %+v", rows)
	}
	const ip = "203.0.113.102"

	spec, container := podContainer(t, kube, ns)
	if len(spec["containers"].([]any)) != 1 || len(kube.pods) != 1 {
		t.Fatalf("pods = %v, want the runner pod only", kube.pods)
	}
	env := map[string]string{}
	for _, e := range container["env"].([]any) {
		m := e.(map[string]any)
		if v, ok := m["value"].(string); ok {
			env[m["name"].(string)] = v
		}
	}
	if env["BUILDKIT_HOST"] != "tcp://"+ip+":1234" || env["BUILDKIT_TLS_DIR"] != "/var/run/secrets/buildkit" || env["BUILDKIT_TLS_SERVERNAME"] != "buildkitd" {
		t.Fatalf("runner env = %v", env)
	}
	mounted := false
	for _, m := range container["volumeMounts"].([]any) {
		mm := m.(map[string]any)
		if mm["mountPath"] == "/var/run/secrets/buildkit" {
			mounted = mm["readOnly"] == true && mm["name"] == "buildkit-client"
		}
	}
	if !mounted {
		t.Fatalf("volumeMounts = %v, want buildkit-client read-only at /var/run/secrets/buildkit", container["volumeMounts"])
	}
	if !strings.Contains(fmt.Sprint(spec["volumes"]), "map[name:buildkit-client secret:map[secretName:buildkit-client]]") {
		t.Fatalf("volumes = %v", spec["volumes"])
	}

	tlsData := kube.secrets[ns+"/buildkit-client"]
	if len(tlsData) != 3 || tlsData["ca.pem"] == "" || tlsData["cert.pem"] == "" || tlsData["key.pem"] == "" {
		t.Fatalf("buildkit-client secret keys = %v", slices.Sorted(maps.Keys(tlsData)))
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(tlsData["ca.pem"]))
	pair, err := tls.X509KeyPair([]byte(tlsData["cert.pem"]), []byte(tlsData["key.pem"]))
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: buildVMNow, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("client cert does not chain to the secret's CA: %v", err)
	}
	if !strings.Contains(hz.serverReqs[0].UserData, strings.TrimSpace(tlsData["ca.pem"])) {
		t.Fatal("the VM does not trust the CA the runner's client cert chains to")
	}

	var vmRule map[string]any
	for _, r := range kube.cnps[ns+"/ci-egress"]["egress"].([]any) {
		if m := r.(map[string]any); m["toCIDR"] != nil {
			if vmRule != nil {
				t.Fatal("more than one toCIDR egress rule")
			}
			vmRule = m
		}
	}
	want := map[string]any{
		"toCIDR":  []string{ip + "/32"},
		"toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": "1234", "protocol": "TCP"}}}},
	}
	if !reflect.DeepEqual(vmRule, want) {
		t.Fatalf("VM egress rule = %v, want %v (CIDR and port in one item)", vmRule, want)
	}
	// buildctl's session answers the VM's registry token requests, so a
	// build runner reaches Docker Hub's token service; the layers are the VM's
	var fqdnRule, dnsRule map[string]any
	for _, r := range kube.cnps[ns+"/ci-egress"]["egress"].([]any) {
		m := r.(map[string]any)
		switch {
		case m["toFQDNs"] != nil:
			fqdnRule = m
		case strings.Contains(fmt.Sprint(m["toEndpoints"]), "kube-dns"):
			dnsRule = m
		}
	}
	if !strings.Contains(fmt.Sprint(fqdnRule["toFQDNs"]), "auth.docker.io") || !strings.Contains(fmt.Sprint(dnsRule), "auth.docker.io") {
		t.Fatalf("build runner egress lacks auth.docker.io in the 443 or DNS rule: %v %v", fqdnRule, dnsRule)
	}
	if s := fmt.Sprint(kube.cnps[ns+"/ci-egress"]); strings.Contains(s, "registry-1.docker.io") || strings.Contains(s, "docker.com") {
		t.Fatalf("build runner egress reaches Docker Hub's registry or CDN: %s", s)
	}
	if quota := kube.objects["/api/v1/namespaces/"+ns+"/resourcequotas"]["spec"].(map[string]any)["hard"].(map[string]any); quota["pods"] != "1" {
		t.Fatalf("resource quota pods = %v, want 1", quota["pods"])
	}
	if kube.objects["/api/v1/namespaces/"+ns+"/services"] != nil {
		t.Fatal("a build job must not create a Service")
	}
}

// An ordinary job gets no VM, no client pair and no CIDR egress.
func TestCISpawnerOrdinaryJobGetsNoBuildVM(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 51, RunsOn: []string{"pr-untrusted"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)
	hz, _ := ciBuildFakes(spawner)

	spawner.poll(context.Background())

	ns := "factory-agents-ci-51"
	if len(hz.calls) != 0 || hz.pricingCalled != 0 {
		t.Fatalf("hetzner calls = %v", hz.calls)
	}
	if _, ok := kube.secrets[ns+"/buildkit-client"]; ok {
		t.Fatal("ordinary job got a buildkit client secret")
	}
	_, container := podContainer(t, kube, ns)
	if s := fmt.Sprint(container["env"], container["volumeMounts"]); strings.Contains(s, "BUILDKIT") || strings.Contains(s, "buildkit") {
		t.Fatalf("ordinary runner carries build wiring: %s", s)
	}
	if strings.Contains(fmt.Sprint(kube.cnps[ns+"/ci-egress"]["egress"]), "toCIDR:") {
		t.Fatal("ordinary job egress has a CIDR rule")
	}
}

// At most CI_BUILD_VMS_MAX VMs live at once: a further build job is not
// claimed (it stays queued in Forgejo) while ordinary jobs still spawn,
// and it is claimed once a finished job's VM is released.
func TestCISpawnerBuildJobsWaitForAVMSlot(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{
		{ID: 1, RunsOn: []string{"pr-untrusted-build"}, Status: "waiting"},
		{ID: 2, RunsOn: []string{"pr-untrusted-build"}, Status: "waiting"},
		{ID: 3, RunsOn: []string{"pr-untrusted"}, Status: "waiting"},
	}}
	spawner := newTestCISpawner(t, kube, hub)
	spawner.builds.cfg.Max = 1
	hz, _ := ciBuildFakes(spawner)
	ctx := context.Background()

	spawner.poll(ctx)
	spawner.poll(ctx)

	for ns, want := range map[string]bool{"factory-agents-ci-1": true, "factory-agents-ci-2": false, "factory-agents-ci-3": true} {
		if _, ok := kube.namespaces[ns]; ok != want {
			t.Fatalf("namespace %s present = %v, want %v with one VM slot", ns, ok, want)
		}
	}
	if len(hub.registered) != 2 || len(hz.servers) != 1 {
		t.Fatalf("registered = %v, servers = %d: the queued build job must not be claimed", hub.registered, len(hz.servers))
	}

	kube.pods["factory-agents-ci-1/"+ciPodName].phase = "Succeeded"
	hub.jobs[0].Status = "success"
	spawner.poll(ctx)

	if _, ok := kube.namespaces["factory-agents-ci-2"]; !ok {
		t.Fatal("build job 2 not claimed once job 1's VM was released")
	}
	if len(hz.servers) != 1 {
		t.Fatalf("servers = %d, want job 2's only", len(hz.servers))
	}
}

// Teardown releases the job's VM and firewall and closes their rows before
// it deletes the namespace, whatever ended the job.
func TestCISpawnerTeardownReleasesBuildVM(t *testing.T) {
	for _, phase := range []string{"Succeeded", "Failed"} {
		t.Run(phase, func(t *testing.T) {
			kube := newFakeKube()
			hub := &ciHub{jobs: []CIJob{{ID: 52, RunsOn: []string{"pr-untrusted-build"}, Status: "waiting"}}}
			spawner := newTestCISpawner(t, kube, hub)
			hz, reg := ciBuildFakes(spawner)
			ctx := context.Background()
			spawner.poll(ctx)
			ns := "factory-agents-ci-52"
			kube.pods[ns+"/"+ciPodName].phase = phase
			hz.calls = nil

			spawner.reconcile(ctx)

			if _, ok := kube.namespaces[ns]; ok {
				t.Fatal("finished build namespace survived reconcile")
			}
			if strings.Join(hz.calls, ",") != "delete server/102,delete firewall/101" || len(hz.servers)+len(hz.firewalls) != 0 {
				t.Fatalf("hetzner calls = %v", hz.calls)
			}
			for id := int64(1); id <= 2; id++ {
				if r, _ := reg.Get(id); r.State != cloudres.StateDeleted {
					t.Fatalf("row %d = %s, want deleted", id, r.State)
				}
			}
			if len(hub.deleted) != 1 {
				t.Fatalf("deleted registrations = %v", hub.deleted)
			}
		})
	}
}

// A VM that cannot be created fails the setup: the registration is rolled
// back and the next reconcile removes the half-built namespace, so the job
// is claimed again later.
func TestCISpawnerBuildVMFailureRollsBack(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 53, RunsOn: []string{"pr-untrusted-build"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)
	hz, reg := ciBuildFakes(spawner)
	hz.serverErr = &hcloud.APIError{Status: 403, Code: "resource_limit_exceeded"}
	ctx := context.Background()

	spawner.poll(ctx)

	ns := "factory-agents-ci-53"
	if _, ok := kube.pods[ns+"/"+ciPodName]; ok {
		t.Fatal("runner pod created without a VM")
	}
	if len(hub.deleted) != 1 {
		t.Fatalf("deleted registrations = %v, want the rollback", hub.deleted)
	}
	if rows, _ := reg.Open(ctx, "ci"); len(rows) != 0 || len(hz.firewalls) != 0 {
		t.Fatalf("open rows = %+v, firewalls = %v", rows, hz.firewalls)
	}
	// the provider failed: further ticks claim nothing, no registration churn
	spawner.poll(ctx)
	spawner.poll(ctx)
	if len(hub.registered) != 1 {
		t.Fatalf("registered = %v while backing off, want the first attempt only", hub.registered)
	}
	hz.serverErr = nil
	spawner.builds.now = func() time.Time { return buildVMNow.Add(time.Minute) }
	spawner.poll(ctx)
	spawner.poll(ctx)
	if _, ok := kube.pods[ns+"/"+ciPodName]; !ok {
		t.Fatal("job not claimed again after the back-off")
	}
}

// A firewall its deleting server still holds must not stall the poll loop:
// teardown tries it once and leaves the row deleting; the collector's next
// pass deletes it.
func TestCISpawnerTeardownLeavesAFirewallInUseToTheCollector(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 54, RunsOn: []string{"pr-untrusted-build"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)
	hz, reg := ciBuildFakes(spawner)
	ctx := context.Background()
	spawner.poll(ctx)
	kube.pods["factory-agents-ci-54/"+ciPodName].phase = "Succeeded"
	hz.calls, hz.fwInUseTimes = nil, 1

	start := time.Now()
	spawner.reconcile(ctx)

	if time.Since(start) > time.Second {
		t.Fatalf("teardown waited %s on the firewall", time.Since(start))
	}
	if strings.Join(hz.calls, ",") != "delete server/102,in use firewall/101" {
		t.Fatalf("hetzner calls = %v", hz.calls)
	}
	if _, ok := kube.namespaces["factory-agents-ci-54"]; ok {
		t.Fatal("namespace survived teardown")
	}
	if r, _ := reg.Get(1); r.State != cloudres.StateDeleting {
		t.Fatalf("firewall row = %s, want deleting", r.State)
	}
	if err := spawner.builds.collector.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := reg.Get(1); r.State != cloudres.StateDeleted || len(hz.firewalls) != 0 {
		t.Fatalf("firewall row = %s, firewalls = %v", r.State, hz.firewalls)
	}
}

// A build job Forgejo still lists as waiting after its namespace was torn
// down gets no second VM within the window; after it, it is claimed again.
func TestCISpawnerDoesNotReclaimATornDownBuildJob(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 60, RunsOn: []string{"pr-untrusted-build"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)
	hz, _ := ciBuildFakes(spawner)
	now := buildVMNow
	spawner.now = func() time.Time { return now }
	ctx := context.Background()
	spawner.poll(ctx)
	kube.pods["factory-agents-ci-60/"+ciPodName].phase = "Succeeded"

	spawner.poll(ctx) // tears 60 down; Forgejo still lists it waiting
	spawner.poll(ctx)

	if len(hub.registered) != 1 || len(hz.serverReqs) != 1 {
		t.Fatalf("registered = %v, servers created = %d: a torn-down job was claimed again", hub.registered, len(hz.serverReqs))
	}
	now = now.Add(ciReclaimWindow)
	spawner.poll(ctx)
	if len(hub.registered) != 2 || len(hz.serverReqs) != 2 {
		t.Fatalf("registered = %v, servers created = %d: want the job claimed again after the window", hub.registered, len(hz.serverReqs))
	}
}
