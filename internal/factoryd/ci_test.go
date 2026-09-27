package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
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
	spawner, err := NewCISpawner(testCIConfig(), forgejo, kube)
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
	if _, err := NewCISpawner(cfg, forgejo, kube); err == nil {
		t.Fatal("want an error for a missing Owner")
	}
	if _, err := NewCISpawner(testCIConfig(), &Forgejo{BaseURL: "http://x"}, kube); err == nil {
		t.Fatal("want an error for a missing CIRegistrarToken")
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
	if labels["factory.ai/task"] != "ci-42" || labels["pod-security.kubernetes.io/enforce"] != "restricted" {
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

	first, err := NewCISpawner(cfg, forgejo, kube)
	if err != nil {
		t.Fatal(err)
	}
	first.poll(context.Background())

	// A restarted factoryd builds a brand new CISpawner against the same
	// cluster and registrar state; the namespace label, not process memory,
	// is what must stop a second registration.
	restarted, err := NewCISpawner(cfg, forgejo, kube)
	if err != nil {
		t.Fatal(err)
	}
	restarted.poll(context.Background())

	if len(hub.registered) != 1 {
		t.Fatalf("registered = %v across a simulated restart, want exactly one", hub.registered)
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
		"registry-1.docker.io", "auth.docker.io", "production.cloudfront.docker.com",
	} {
		if !strings.Contains(cnpStr, host) {
			t.Fatalf("egress allowlist missing %s: %s", host, cnpStr)
		}
	}
	if !strings.Contains(cnpStr, "k8s:app.kubernetes.io/name:forgejo") || !strings.Contains(cnpStr, "port:3000") {
		t.Fatal("egress missing the in-cluster forgejo allow")
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

// A pr-untrusted-build job (ruling 91) gets the usual restricted runner pod
// plus a second, sandbox-root buildkitd pod in the same namespace, whose
// namespace is baseline (not restricted, which forbids runAsUser: 0
// unconditionally) and whose resource quota is doubled for the extra pod.
func TestCISpawnerBuildJobCreatesBuildkitdPodAndService(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 50, RunsOn: []string{"pr-untrusted-build"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)

	spawner.poll(context.Background())

	ns := "factory-agents-ci-50"
	labels, ok := kube.namespaces[ns]
	if !ok {
		t.Fatalf("namespace %s not created", ns)
	}
	if labels["pod-security.kubernetes.io/enforce"] != "baseline" {
		t.Fatalf("namespace enforce = %q, want baseline for a build job", labels["pod-security.kubernetes.io/enforce"])
	}

	quota := kube.objects["/api/v1/namespaces/"+ns+"/resourcequotas"]["spec"].(map[string]any)["hard"].(map[string]any)
	if quota["pods"] != "2" {
		t.Fatalf("resource quota pods = %v, want 2 (runner + buildkitd)", quota["pods"])
	}

	runnerPod, ok := kube.pods[ns+"/"+ciPodName]
	if !ok {
		t.Fatal("runner pod not created")
	}
	runnerArgs := fmt.Sprint(runnerPod.spec["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["args"])
	if !strings.Contains(runnerArgs, "pr-untrusted-build:host") {
		t.Fatalf("runner args = %s, want the build label", runnerArgs)
	}
	runnerEnv := fmt.Sprint(runnerPod.spec["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"])
	wantHost := "tcp://buildkitd." + ns + ".svc.cluster.local:1234"
	if !strings.Contains(runnerEnv, wantHost) {
		t.Fatalf("runner env = %s, missing BUILDKIT_HOST %s", runnerEnv, wantHost)
	}

	buildPod, ok := kube.pods[ns+"/"+buildPodName]
	if !ok {
		t.Fatal("buildkitd pod not created")
	}
	bspec := buildPod.spec["spec"].(map[string]any)
	if bspec["runtimeClassName"] != "gvisor" || bspec["automountServiceAccountToken"] != false {
		t.Fatalf("buildkitd pod hardening: runtimeClassName=%v automount=%v", bspec["runtimeClassName"], bspec["automountServiceAccountToken"])
	}
	podSC := bspec["securityContext"].(map[string]any)
	if podSC["runAsUser"] != int64(0) {
		t.Fatalf("buildkitd pod runAsUser = %v, want 0", podSC["runAsUser"])
	}
	bcontainer := bspec["containers"].([]any)[0].(map[string]any)
	csc := bcontainer["securityContext"].(map[string]any)
	if csc["allowPrivilegeEscalation"] != true {
		t.Fatalf("buildkitd container allowPrivilegeEscalation = %v, want true", csc["allowPrivilegeEscalation"])
	}
	// The container process starts long before buildkitd is actually
	// listening (nested unshare + uid/gid map writes first); without a
	// readiness gate the Service would route to it before it can accept a
	// connection, and the runner's first buildctl call would race it.
	probe, ok := bcontainer["readinessProbe"].(map[string]any)
	if !ok {
		t.Fatal("buildkitd container missing a readinessProbe")
	}
	tcp, ok := probe["tcpSocket"].(map[string]any)
	if !ok || tcp["port"] != int64(buildPodPort) {
		t.Fatalf("buildkitd readinessProbe tcpSocket = %v, want port %d", probe["tcpSocket"], buildPodPort)
	}
	if _, privileged := csc["privileged"]; privileged {
		t.Fatal("buildkitd container must not set privileged")
	}
	if _, caps := csc["capabilities"]; caps {
		t.Fatal("buildkitd container must not add capabilities")
	}
	volumes := fmt.Sprint(bspec["volumes"])
	if !strings.Contains(volumes, "20Gi") {
		t.Fatalf("buildkitd volumes = %s, want a 20Gi scratch emptyDir", volumes)
	}
	if buildPod.spec["metadata"].(map[string]any)["labels"].(map[string]string)[buildPodLabelKey] != "true" {
		t.Fatal("buildkitd pod missing the factory.ai/build-pod label the VAP carve-out keys on")
	}

	svc := kube.objects["/api/v1/namespaces/"+ns+"/services"]
	if svc == nil {
		t.Fatal("buildkitd Service not created")
	}
	svcSpec := svc["spec"].(map[string]any)
	if fmt.Sprint(svcSpec["selector"]) != "map[app.kubernetes.io/name:buildkitd]" {
		t.Fatalf("buildkitd service selector = %v", svcSpec["selector"])
	}

	buildCNP := kube.cnps[ns+"/buildkitd-policy"]
	if buildCNP == nil {
		t.Fatal("buildkitd egress CNP missing")
	}
	cnpStr := fmt.Sprint(buildCNP)
	for _, want := range []string{"169.254.169.254/32", "deb.debian.org", "registry-1.docker.io", "code.forgejo.org", "go.dev"} {
		if !strings.Contains(cnpStr, want) {
			t.Fatalf("buildkitd egress = %s, missing %q", cnpStr, want)
		}
	}
	if !strings.Contains(cnpStr, "ci-runner") {
		t.Fatal("buildkitd ingress must be scoped to the ci-runner pod")
	}

	runnerCNP := fmt.Sprint(kube.cnps[ns+"/ci-egress"])
	if !strings.Contains(runnerCNP, "buildkitd") {
		t.Fatal("runner's own egress policy must additionally reach buildkitd for a build job")
	}
}

// Live under gVisor, writing the nested child's uid_map/gid_map right after
// backgrounding it raced the child's own unshare(2)+exec and failed with
// "printf: I/O error" every time (reproduced on 5/5 live pods); waiting for
// an ack the child can only send after it is already running (and therefore
// already unshared, since unshare(1) syscalls before it execs) fixed it on
// 5/5. This pins that ordering in the generated script text so it can't
// regress back to the unsynchronized write.
func TestBuildkitdScriptWaitsForChildAckBeforeWritingMaps(t *testing.T) {
	script := buildkitdScript()

	childBlock := script[strings.Index(script, "unshare"):strings.Index(script, "' &")]
	ackIdx := strings.Index(childBlock, "echo ready > /tmp/ready.fifo")
	childGoReadIdx := strings.Index(childBlock, "read x < /tmp/go.fifo")
	if ackIdx < 0 || childGoReadIdx < 0 || ackIdx > childGoReadIdx {
		t.Fatalf("child must ack ready.fifo before blocking on go.fifo, got: %s", childBlock)
	}

	cpidIdx := strings.Index(script, "CPID=$!")
	parentAckReadIdx := strings.Index(script, "read x < /tmp/ready.fifo")
	uidMapIdx := strings.Index(script, "/proc/$CPID/uid_map")
	gidMapIdx := strings.Index(script, "/proc/$CPID/gid_map")
	releaseIdx := strings.Index(script, "echo go > /tmp/go.fifo")
	if cpidIdx < 0 || parentAckReadIdx < 0 || uidMapIdx < 0 || gidMapIdx < 0 || releaseIdx < 0 {
		t.Fatalf("script missing an expected step: %s", script)
	}
	if !(cpidIdx < parentAckReadIdx && parentAckReadIdx < uidMapIdx && uidMapIdx < gidMapIdx && gidMapIdx < releaseIdx) {
		t.Fatalf("parent must wait for the child's ack before writing uid_map/gid_map and only then release it, got order cpid=%d ack=%d uid=%d gid=%d release=%d",
			cpidIdx, parentAckReadIdx, uidMapIdx, gidMapIdx, releaseIdx)
	}
}

// An ordinary (non-build) job must never get a buildkitd pod, Service, or
// policy: those are ruling 91's build-label carve-out, not the default.
func TestCISpawnerOrdinaryJobGetsNoBuildkitdPod(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 51, RunsOn: []string{"pr-untrusted"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)

	spawner.poll(context.Background())

	ns := "factory-agents-ci-51"
	if _, ok := kube.pods[ns+"/"+buildPodName]; ok {
		t.Fatal("ordinary job must not get a buildkitd pod")
	}
	if kube.objects["/api/v1/namespaces/"+ns+"/services"] != nil {
		t.Fatal("ordinary job must not get a buildkitd Service")
	}
	if kube.cnps[ns+"/buildkitd-policy"] != nil {
		t.Fatal("ordinary job must not get a buildkitd egress policy")
	}
	if labels := kube.namespaces[ns]; labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Fatalf("enforce = %q, want restricted for an ordinary job", labels["pod-security.kubernetes.io/enforce"])
	}
	quota := kube.objects["/api/v1/namespaces/"+ns+"/resourcequotas"]["spec"].(map[string]any)["hard"].(map[string]any)
	if quota["pods"] != "1" {
		t.Fatalf("resource quota pods = %v, want 1 for a single-pod namespace", quota["pods"])
	}
}

// reconcile must reap a finished build namespace too: its
// pod-security.kubernetes.io/enforce is baseline, not restricted, and the
// listing that drives reconcile must query both.
func TestCISpawnerReconcileTearsDownFinishedBuildNamespace(t *testing.T) {
	kube := newFakeKube()
	hub := &ciHub{jobs: []CIJob{{ID: 52, RunsOn: []string{"pr-untrusted-build"}, Status: "waiting"}}}
	spawner := newTestCISpawner(t, kube, hub)
	spawner.poll(context.Background())
	ns := "factory-agents-ci-52"
	kube.pods[ns+"/"+ciPodName].phase = "Succeeded"

	spawner.reconcile(context.Background())

	if _, ok := kube.namespaces[ns]; ok {
		t.Fatal("finished build namespace survived reconcile")
	}
	if len(hub.deleted) != 1 {
		t.Fatalf("deleted registrations = %v, want the runner removed", hub.deleted)
	}
}
