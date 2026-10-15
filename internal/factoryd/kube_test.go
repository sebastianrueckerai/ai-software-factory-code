package factoryd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestInClusterKubeWire verifies the raw-REST client against a scripted
// apiserver: paths, status-code mapping, and secret decoding.
func TestInClusterKubeWire(t *testing.T) {
	var mu sync.Mutex
	var selectors []string
	respond := map[string]func(w http.ResponseWriter, r *http.Request){
		"POST /api/v1/namespaces": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			meta := body["metadata"].(map[string]any)
			if meta["name"] != "factory-agents-t1" {
				t.Errorf("namespace name = %v", meta["name"])
			}
			if meta["labels"].(map[string]any)["factory.ai/task"] != "t1" {
				t.Errorf("labels = %v", meta["labels"])
			}
			w.WriteHeader(http.StatusCreated)
		},
		"GET /api/v1/namespaces": func(w http.ResponseWriter, r *http.Request) {
			selectors = append(selectors, r.URL.Query().Get("labelSelector"))
			io.WriteString(w, `{"items":[{"metadata":{"name":"factory-agents-t1"}},`+
				`{"metadata":{"name":"factory-agents-t2","deletionTimestamp":"2026-09-28T10:00:00Z"}}]}`)
		},
		"POST /api/v1/namespaces/factory-agents-t1/resourcequotas": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
		},
		"DELETE /api/v1/namespaces/factory-agents-t1": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
		"POST /api/v1/namespaces/factory-agents-t1/serviceaccounts": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
		},
		"POST /api/v1/namespaces/factory-agents-t1/secrets": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["stringData"].(map[string]any)["PUSH_TOKEN"] != "s3cret" {
				t.Errorf("stringData = %v", body["stringData"])
			}
			if body["type"] != "Opaque" {
				t.Errorf("type = %v", body["type"])
			}
			w.WriteHeader(http.StatusCreated)
		},
		"GET /api/v1/namespaces/factory/secrets/forgejo-registry": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"data":{".dockerconfigjson":"`+base64.StdEncoding.EncodeToString([]byte("{}"))+`"}}`)
		},
		"POST /apis/cilium.io/v2/namespaces/factory-agents-t1/ciliumnetworkpolicies": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["apiVersion"] != "cilium.io/v2" || body["kind"] != "CiliumNetworkPolicy" {
				t.Errorf("cnp identity = %v", body["apiVersion"])
			}
			if body["spec"] == nil {
				t.Error("cnp without spec")
			}
			w.WriteHeader(http.StatusCreated)
		},
		"GET /apis/cilium.io/v2/namespaces/factory-agents-t1/ciliumnetworkpolicies/worker-egress": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"kind":"CiliumNetworkPolicy","spec":{"egress":[{"toCIDR":["203.0.113.7/32"]}]}}`)
		},
		"PATCH /apis/cilium.io/v2/namespaces/factory-agents-t1/ciliumnetworkpolicies/worker-egress": func(w http.ResponseWriter, r *http.Request) {
			if ct := r.Header.Get("Content-Type"); ct != "application/apply-patch+yaml" {
				t.Errorf("apply content type = %q", ct)
			}
			if q := r.URL.Query(); q.Get("fieldManager") != "factoryd" || q.Get("force") != "true" {
				t.Errorf("apply query = %v", q)
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["kind"] != "CiliumNetworkPolicy" || body["metadata"].(map[string]any)["name"] != "worker-egress" || body["spec"] == nil {
				t.Errorf("apply body = %v", body)
			}
		},
		"POST /api/v1/namespaces/factory-agents-t1/pods": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["metadata"].(map[string]any)["name"] != "worker-s1" {
				t.Errorf("pod name overwritten: %v", body["metadata"])
			}
			w.WriteHeader(http.StatusCreated)
		},
		"DELETE /api/v1/namespaces/factory-agents-t1/pods/worker-s1": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
		"GET /api/v1/namespaces/factory-agents-t1/pods/worker-s1": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"status":{"phase":"Succeeded","containerStatuses":[{"state":{"terminated":{"exitCode":0}}}]}}`)
		},
		"GET /api/v1/namespaces/factory-agents-t1/pods/worker-s1/log": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("follow") != "true" {
				t.Errorf("log without follow")
			}
			io.WriteString(w, "line\n")
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		handler := respond[r.Method+" "+r.URL.Path]
		mu.Unlock()
		if handler == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		handler(w, r)
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldDir := saDir
	saDir = dir
	t.Cleanup(func() { saDir = oldDir })
	k := &InClusterKube{Host: srv.URL, HTTP: srv.Client()}
	ctx := context.Background()

	if err := k.CreateNamespace(ctx, "factory-agents-t1", map[string]string{"factory.ai/task": "t1"}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	names, err := k.NamespacesByLabel(ctx, "factory.ai/task", "t1")
	if err != nil || len(names) != 2 {
		t.Fatalf("NamespacesByLabel = %v, %v", names, err)
	}
	for _, want := range []string{"factory-agents-t1", "factory-agents-t2"} {
		if !slices.Contains(names, want) {
			t.Errorf("NamespacesByLabel = %v, missing %s: a Terminating namespace still exists", names, want)
		}
	}
	listed, err := k.ListNamespacesByLabel(ctx, "factory.ai/task", "t1")
	if err != nil || len(listed) != 2 {
		t.Fatalf("ListNamespacesByLabel = %v, %v", listed, err)
	}
	wantListed := []NamespaceInfo{{Name: "factory-agents-t1"}, {Name: "factory-agents-t2", Terminating: true}}
	if !reflect.DeepEqual(listed, wantListed) {
		t.Fatalf("ListNamespacesByLabel = %+v, want %+v", listed, wantListed)
	}
	if _, err := k.NamespacesByLabel(ctx, "factory.ai/task", ""); err != nil {
		t.Fatalf("NamespacesByLabel key-only: %v", err)
	}
	// "factory.ai/task=" would select only namespaces whose label is empty
	if fmt.Sprint(selectors) != "[factory.ai/task=t1 factory.ai/task=t1 factory.ai/task]" {
		t.Fatalf("labelSelectors = %v", selectors)
	}
	if err := k.CreateServiceAccount(ctx, "factory-agents-t1", "task-t1"); err != nil {
		t.Fatalf("CreateServiceAccount: %v", err)
	}
	if err := k.CreateSecret(ctx, "factory-agents-t1", "task-git", "", map[string]string{"PUSH_TOKEN": "s3cret"}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	data, err := k.GetSecretData(ctx, "factory", "forgejo-registry")
	if err != nil || data[".dockerconfigjson"] != "{}" {
		t.Fatalf("GetSecretData = %v, %v", data, err)
	}
	if err := k.CreateCiliumNetworkPolicy(ctx, "factory-agents-t1", "worker-egress", map[string]any{"endpointSelector": map[string]any{}}); err != nil {
		t.Fatalf("CreateCiliumNetworkPolicy: %v", err)
	}
	if err := k.ApplyCiliumNetworkPolicy(ctx, "factory-agents-t1", "worker-egress", map[string]any{"endpointSelector": map[string]any{}}); err != nil {
		t.Fatalf("ApplyCiliumNetworkPolicy: %v", err)
	}
	if spec, err := k.CiliumNetworkPolicySpec(ctx, "factory-agents-t1", "worker-egress"); err != nil || fmt.Sprint(spec) != "map[egress:[map[toCIDR:[203.0.113.7/32]]]]" {
		t.Fatalf("CiliumNetworkPolicySpec = %v, %v", spec, err)
	}
	if _, err := k.CiliumNetworkPolicySpec(ctx, "factory-agents-t1", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing policy err = %v", err)
	}
	if err := k.Create(ctx, "/api/v1/namespaces/factory-agents-t1/resourcequotas", map[string]any{"kind": "ResourceQuota"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := k.CreatePod(ctx, "factory-agents-t1", "worker-s1", map[string]any{"spec": map[string]any{}}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	phase, exit, err := k.PodStatus(ctx, "factory-agents-t1", "worker-s1")
	if err != nil || phase != "Succeeded" || exit != 0 {
		t.Fatalf("PodStatus = %q %d %v", phase, exit, err)
	}
	stream, err := k.PodLog(ctx, "factory-agents-t1", "worker-s1")
	if err != nil {
		t.Fatalf("PodLog: %v", err)
	}
	body, _ := io.ReadAll(stream)
	stream.Close()
	if string(body) != "line\n" {
		t.Fatalf("log body = %q", body)
	}
	if err := k.DeletePod(ctx, "factory-agents-t1", "worker-s1"); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	if err := k.DeleteNamespace(ctx, "factory-agents-t1"); err != nil {
		t.Fatalf("DeleteNamespace: %v", err)
	}

	// Status mapping: gone → NotFound, conflict → AlreadyExists.
	if err := k.DeletePod(ctx, "factory-agents-t1", "gone"); err != ErrNotFound {
		t.Fatalf("delete missing pod = %v, want ErrNotFound", err)
	}
	conflict := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer conflict.Close()
	ck := &InClusterKube{Host: conflict.URL, HTTP: conflict.Client()}
	if err := ck.CreateNamespace(ctx, "x", nil); err != ErrAlreadyExists {
		t.Fatalf("conflict = %v, want ErrAlreadyExists", err)
	}
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer forbidden.Close()
	fk := &InClusterKube{Host: forbidden.URL, HTTP: forbidden.Client()}
	if err := fk.CreateNamespace(ctx, "x", nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("forbidden = %v, want ErrForbidden", err)
	}
}

func TestInClusterKubeNeedsTheServiceAddress(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	if _, err := NewInClusterKube(); err == nil || !strings.Contains(err.Error(), "KUBERNETES_SERVICE_HOST") {
		t.Fatalf("err = %v, want the missing service address named", err)
	}
}

// The ready time is the first init container's finish, taken only when it
// exited 0 with the termination message "ready".
func TestPodInitFinishedTakesOnlyAReadyFinish(t *testing.T) {
	pods := map[string]string{
		"ready": `{"status":{"phase":"Succeeded","initContainerStatuses":[{"name":"buildkitd-ready","state":{"terminated":
			{"exitCode":0,"reason":"Completed","message":"ready\n","startedAt":"2026-09-28T10:00:03Z","finishedAt":"2026-09-28T10:00:47Z"}}}]}}`,
		"silent": `{"status":{"phase":"Succeeded","initContainerStatuses":[{"name":"buildkitd-ready","state":{"terminated":
			{"exitCode":0,"reason":"Completed","startedAt":"2026-09-28T10:00:03Z","finishedAt":"2026-09-28T10:03:03Z"}}}]}}`,
		"failed": `{"status":{"phase":"Failed","initContainerStatuses":[{"name":"buildkitd-ready","state":{"terminated":
			{"exitCode":124,"reason":"Error","message":"ready","startedAt":"2026-09-28T10:00:03Z","finishedAt":"2026-09-28T10:03:03Z"}}}]}}`,
		"running": `{"status":{"phase":"Pending","initContainerStatuses":[{"name":"buildkitd-ready","state":{"running":{"startedAt":"2026-09-28T10:00:03Z"}}}]}}`,
		"none":    `{"status":{"phase":"Succeeded","containerStatuses":[{"name":"runner","state":{"terminated":{"exitCode":0,"message":"ready"}}}]}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ns, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/"), "/")
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/namespaces/"+ns+"/pods/ci-runner" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, pods[ns])
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldDir := saDir
	saDir = dir
	t.Cleanup(func() { saDir = oldDir })
	k := &InClusterKube{Host: srv.URL, HTTP: srv.Client()}
	for ns, want := range map[string]time.Time{
		"ready": time.Date(2026, 9, 28, 10, 0, 47, 0, time.UTC), "silent": {}, "failed": {}, "running": {}, "none": {},
	} {
		at, ok, err := k.PodInitFinished(context.Background(), ns, "ci-runner")
		if err != nil || ok != !want.IsZero() || !at.Equal(want) {
			t.Errorf("%s: (%v, %v, %v), want %v", ns, at, ok, err, want)
		}
	}
}

// The gate supervisor's pod reads: phase, conditions and container states,
// the init containers' first, and the log's last lines without following.
func TestInClusterKubePodDiagnosisAndLogTail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces/factory-agents-t1/pods/worker-s1":
			io.WriteString(w, `{"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"True"},`+
				`{"type":"Ready","status":"False","reason":"ContainersNotReady","message":"containers with unready status: [worker]"}],`+
				`"containerStatuses":[{"name":"worker","restartCount":2,"state":{"waiting":{"reason":"ImagePullBackOff","message":"Back-off pulling image"}}}],`+
				`"initContainerStatuses":[{"name":"clone","state":{"terminated":{"reason":"Error","message":"no space","exitCode":1}}}]}}`)
		case "/api/v1/namespaces/factory-agents-t1/pods/worker-s1/log":
			if q := r.URL.Query(); q.Get("tailLines") != "100" || q.Get("limitBytes") != "65536" || q.Has("follow") {
				t.Errorf("log query = %v, want tailLines=100, limitBytes=65536 and no follow", q)
			}
			io.WriteString(w, "a\nb\n")
		case "/api/v1/namespaces/factory-agents-t1/pods/worker-evicted":
			io.WriteString(w, `{"status":{"phase":"Failed","reason":"Evicted","message":"The node was low on resource: ephemeral-storage."}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldDir := saDir
	saDir = dir
	t.Cleanup(func() { saDir = oldDir })
	k := &InClusterKube{Host: srv.URL, HTTP: srv.Client()}
	d, err := k.PodDiagnosis(context.Background(), "factory-agents-t1", "worker-s1")
	want := PodDiagnosis{Phase: "Pending", Conditions: []PodCondition{{Type: "PodScheduled", Status: "True"},
		{Type: "Ready", Status: "False", Reason: "ContainersNotReady", Message: "containers with unready status: [worker]"}},
		Containers: []ContainerState{{Name: "clone", State: "terminated", Reason: "Error", Message: "no space", ExitCode: 1},
			{Name: "worker", State: "waiting", Reason: "ImagePullBackOff", Message: "Back-off pulling image", Restarts: 2}}}
	if err != nil || fmt.Sprint(d) != fmt.Sprint(want) {
		t.Fatalf("PodDiagnosis = %+v, %v", d, err)
	}
	evicted, err := k.PodDiagnosis(context.Background(), "factory-agents-t1", "worker-evicted")
	if err != nil || evicted.Phase != "Failed" || evicted.Reason != "Evicted" ||
		evicted.Message != "The node was low on resource: ephemeral-storage." {
		t.Fatalf("evicted PodDiagnosis = %+v, %v", evicted, err)
	}
	if tail, err := k.PodLogTail(context.Background(), "factory-agents-t1", "worker-s1", 100); err != nil || tail != "a\nb\n" {
		t.Fatalf("PodLogTail = %q, %v", tail, err)
	}
	if _, err := k.PodLogTail(context.Background(), "factory-agents-t1", "gone", 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("gone pod's tail err = %v", err)
	}
}

// The namespace state contract: ListNamespacesByLabel reports Terminating=true
// for a namespace the apiserver stamped metadata.deletionTimestamp on — such a
// namespace cannot accept new writes — and Labels carries its labels. Tests
// simulate it with fakeKube.terminating[name] = true.
func TestListNamespacesByLabelReportsTerminatingState(t *testing.T) {
	ctx := context.Background()

	fk := newFakeKube()
	if err := fk.CreateNamespace(ctx, "factory-agents-t9", map[string]string{"factory.ai/task": "t9"}); err != nil {
		t.Fatalf("fake CreateNamespace: %v", err)
	}
	if err := fk.CreateNamespace(ctx, "factory-agents-other", map[string]string{"factory.ai/task": "other"}); err != nil {
		t.Fatalf("fake CreateNamespace: %v", err)
	}
	fk.terminating["factory-agents-t9"] = true
	fakeList, err := fk.ListNamespacesByLabel(ctx, "factory.ai/task", "t9")
	wantFake := []NamespaceInfo{{Name: "factory-agents-t9", Terminating: true, Labels: map[string]string{"factory.ai/task": "t9"}}}
	if err != nil || !reflect.DeepEqual(fakeList, wantFake) {
		t.Fatalf("fakeKube ListNamespacesByLabel = %+v, %v, want %+v", fakeList, err, wantFake)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces" || r.URL.Query().Get("labelSelector") != "factory.ai/task=t9" {
			t.Errorf("GET %s?labelSelector=%s", r.URL.Path, r.URL.Query().Get("labelSelector"))
		}
		io.WriteString(w, `{"items":[{"metadata":{"name":"factory-agents-t9","labels":{"factory.ai/task":"t9"},"deletionTimestamp":"2026-10-13T11:13:54Z"}},`+
			`{"metadata":{"name":"factory-agents-t10","labels":{"factory.ai/task":"t9"}}}]}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldDir := saDir
	saDir = dir
	t.Cleanup(func() { saDir = oldDir })
	k := &InClusterKube{Host: srv.URL, HTTP: srv.Client()}
	got, err := k.ListNamespacesByLabel(ctx, "factory.ai/task", "t9")
	want := []NamespaceInfo{
		{Name: "factory-agents-t9", Terminating: true, Labels: map[string]string{"factory.ai/task": "t9"}},
		{Name: "factory-agents-t10", Labels: map[string]string{"factory.ai/task": "t9"}},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ListNamespacesByLabel = %+v, %v, want %+v", got, err, want)
	}
}

// SecretsByLabels selects with every label, sorted, and answers each
// Secret's name, type and decoded data.
func TestInClusterKubeSecretsByLabels(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sel := r.URL.Query().Get("labelSelector")
		if r.URL.Path != "/api/v1/namespaces/demo-staging/secrets" || sel != "kustomize.toolkit.fluxcd.io/name=demo-app,kustomize.toolkit.fluxcd.io/namespace=demo-staging" {
			t.Errorf("GET %s with labelSelector %q", r.URL.Path, sel)
		}
		io.WriteString(w, `{"items":[{"metadata":{"name":"admin-token"},"type":"Opaque","data":{"ADMIN_TOKEN":"`+b64("x")+`"}},`+
			`{"metadata":{"name":"registry"},"type":"kubernetes.io/dockerconfigjson","data":{".dockerconfigjson":"`+b64("{}")+`"}}]}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldDir := saDir
	saDir = dir
	t.Cleanup(func() { saDir = oldDir })
	k := &InClusterKube{Host: srv.URL, HTTP: srv.Client()}
	got, err := k.SecretsByLabels(context.Background(), "demo-staging", map[string]string{
		"kustomize.toolkit.fluxcd.io/namespace": "demo-staging", "kustomize.toolkit.fluxcd.io/name": "demo-app"})
	want := []KubeSecret{{Name: "admin-token", Type: "Opaque", Data: map[string]string{"ADMIN_TOKEN": "x"}},
		{Name: "registry", Type: "kubernetes.io/dockerconfigjson", Data: map[string]string{".dockerconfigjson": "{}"}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("SecretsByLabels = %+v, %v\nwant %+v", got, err, want)
	}
}

// kubeAt is the production client against srv, its token a stub.
func kubeAt(t *testing.T, srv *httptest.Server) *InClusterKube {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldDir := saDir
	saDir = dir
	t.Cleanup(func() { saDir = oldDir })
	return &InClusterKube{Host: srv.URL, HTTP: srv.Client()}
}

// CronJobs reads a namespace's CronJobs with their schedule, suspension
// and last runs; a job never run has zero times.
func TestInClusterKubeCronJobs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/batch/v1/namespaces/forgejo/cronjobs" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"items":[`+
			`{"metadata":{"name":"forgejo-dump"},"spec":{"schedule":"30 2 * * *","suspend":false},`+
			`"status":{"lastScheduleTime":"2026-10-15T02:30:00Z","lastSuccessfulTime":"2026-10-15T02:31:10Z"}},`+
			`{"metadata":{"name":"push-mirrors"},"spec":{"schedule":"*/10 * * * *","suspend":true},"status":{}}]}`)
	}))
	defer srv.Close()
	got, err := kubeAt(t, srv).CronJobs(context.Background(), "forgejo")
	want := []CronJobState{
		{Kind: KindCronJob, Namespace: "forgejo", Name: "forgejo-dump", Schedule: "30 2 * * *",
			LastScheduled: time.Date(2026, 10, 15, 2, 30, 0, 0, time.UTC), LastSuccessful: time.Date(2026, 10, 15, 2, 31, 10, 0, time.UTC)},
		{Kind: KindCronJob, Namespace: "forgejo", Name: "push-mirrors", Schedule: "*/10 * * * *", Suspended: true},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("CronJobs = %+v, %v\nwant %+v", got, err, want)
	}
}

// ScheduledBackups reads a namespace's CNPG ScheduledBackups: their
// six-field schedule as written, suspension and last schedule time. The
// resource records no success, so LastSuccessful stays zero.
func TestInClusterKubeScheduledBackups(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/postgresql.cnpg.io/v1/namespaces/platform/scheduledbackups" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"items":[`+
			`{"metadata":{"name":"sf-cluster-daily"},"spec":{"schedule":"0 0 2 * * *","cluster":{"name":"sf-cluster"}},`+
			`"status":{"lastCheckTime":"2026-10-15T09:12:00Z","lastScheduleTime":"2026-10-15T02:00:00Z","nextScheduleTime":"2026-10-16T02:00:00Z"}},`+
			`{"metadata":{"name":"sf-cluster-weekly"},"spec":{"schedule":"0 0 3 * * 0","suspend":true}}]}`)
	}))
	defer srv.Close()
	got, err := kubeAt(t, srv).ScheduledBackups(context.Background(), "platform")
	want := []CronJobState{
		{Kind: KindScheduledBackup, Namespace: "platform", Name: "sf-cluster-daily", Schedule: "0 0 2 * * *",
			LastScheduled: time.Date(2026, 10, 15, 2, 0, 0, 0, time.UTC)},
		{Kind: KindScheduledBackup, Namespace: "platform", Name: "sf-cluster-weekly", Schedule: "0 0 3 * * 0", Suspended: true},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ScheduledBackups = %+v, %v\nwant %+v", got, err, want)
	}
}

// ClusterJobs reads platform, factory and forgejo unless told other
// namespaces, in their order, each namespace's CronJobs before its
// ScheduledBackups. A read the cluster refuses fails the whole read and
// names its namespace.
func TestClusterJobsReadTheirNamespaces(t *testing.T) {
	backup := CronJobState{Kind: KindScheduledBackup, Namespace: "platform", Name: "sf-cluster-daily", Schedule: "0 0 2 * * *"}
	bao := CronJobState{Kind: KindCronJob, Namespace: "platform", Name: "openbao-config", Schedule: "*/10 * * * *"}
	facts := CronJobState{Kind: KindCronJob, Namespace: "factory", Name: "cluster-facts", Schedule: "0 * * * *"}
	dump := CronJobState{Kind: KindCronJob, Namespace: "forgejo", Name: "forgejo-dump", Schedule: "30 2 * * *"}
	other := CronJobState{Kind: KindCronJob, Namespace: "signoz", Name: "retention", Schedule: "0 4 * * *"}
	k := &fakeKube{cronJobs: map[string][]CronJobState{"platform": {backup, bao}, "factory": {facts}, "forgejo": {dump}, "signoz": {other}}}
	got, err := ClusterJobs{Kube: k}.CronJobs(context.Background())
	if err != nil || !reflect.DeepEqual(got, []CronJobState{bao, backup, facts, dump}) {
		t.Fatalf("by default = %+v, %v", got, err)
	}
	got, err = ClusterJobs{Kube: k, Namespaces: []string{"signoz", "forgejo"}}.CronJobs(context.Background())
	if err != nil || !reflect.DeepEqual(got, []CronJobState{other, dump}) {
		t.Fatalf("signoz and forgejo = %+v, %v", got, err)
	}
	for key, want := range map[string]string{
		KindCronJob + " factory":          "cronjobs of factory: " + ErrForbidden.Error(),
		KindScheduledBackup + " platform": "scheduled backups of platform: " + ErrForbidden.Error(),
	} {
		k.cronJobsErr = map[string]error{key: ErrForbidden}
		if got, err := (ClusterJobs{Kube: k}).CronJobs(context.Background()); !errors.Is(err, ErrForbidden) || err.Error() != want || got != nil {
			t.Errorf("%s forbidden = %+v, %v", key, got, err)
		}
	}
}
