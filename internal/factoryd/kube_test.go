package factoryd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
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
	phase, exit, _, err := k.PodStatus(ctx, "factory-agents-t1", "worker-s1")
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
// A pod's address is its running one alone: none before it has one, and
// none once it ended, since the cluster may give that address to another
// pod.
func TestInClusterKubePodIPOfARunningPodAlone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		phase, ip, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/ns/pods/"), "-")
		fmt.Fprintf(w, `{"status":{"phase":%q,"podIP":%q}}`, phase, ip)
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
	if ip, err := k.PodIP(context.Background(), "ns", "Running-10.0.0.7"); err != nil || ip != "10.0.0.7" {
		t.Fatalf("running: %q, %v", ip, err)
	}
	if _, err := k.PodIP(context.Background(), "ns", "Pending-"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending: %v, want ErrNotFound", err)
	}
	for _, phase := range []string{"Succeeded", "Failed"} {
		if ip, err := k.PodIP(context.Background(), "ns", phase+"-10.0.0.7"); !errors.Is(err, errPodEnded) || ip != "" {
			t.Fatalf("%s: %q, %v; want errPodEnded", phase, ip, err)
		}
	}
}

// A pod's env is read back from its spec, the named container's alone, and
// a pod without that container answers ErrNotFound.
func TestInClusterKubePodEnv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/ns/pods/worker-s-1" {
			t.Errorf("GET %s", r.URL.Path)
		}
		io.WriteString(w, `{"spec":{"containers":[{"name":"clone","env":[{"name":"LINE_BOUND","value":"1"}]},`+
			`{"name":"worker","env":[{"name":"MODEL","value":"m"},{"name":"LINE_BOUND","value":"600"}]}]}}`)
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
	env, err := k.PodEnv(context.Background(), "ns", "worker-s-1", "worker")
	if err != nil || !maps.Equal(env, map[string]string{"MODEL": "m", "LINE_BOUND": "600"}) {
		t.Fatalf("env = %v, %v; want the worker container's MODEL and LINE_BOUND 600", env, err)
	}
	if _, err := k.PodEnv(context.Background(), "ns", "worker-s-1", "agent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a container the pod lacks: %v, want ErrNotFound", err)
	}
}

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

// Jobs reads a namespace's Jobs with the CronJob that owns each (name and
// uid), the start (the creation time when status has none) and the
// completion.
func TestInClusterKubeJobs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/batch/v1/namespaces/forgejo/jobs" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"items":[`+
			`{"metadata":{"name":"forgejo-dump-29887350","creationTimestamp":"2026-10-29T02:30:00Z","ownerReferences":[{"kind":"CronJob","name":"forgejo-dump","uid":"u1"}]},`+
			`"status":{"startTime":"2026-10-29T02:30:05Z","completionTime":"2026-10-29T02:31:10Z"}},`+
			`{"metadata":{"name":"forgejo-dump-29888790","creationTimestamp":"2026-10-30T02:30:00Z","ownerReferences":[{"kind":"CronJob","name":"forgejo-dump","uid":"u1"}]},"status":{}},`+
			`{"metadata":{"name":"manual","creationTimestamp":"2026-10-29T03:00:00Z"},"status":{}}]}`)
	}))
	defer srv.Close()
	got, err := kubeAt(t, srv).Jobs(context.Background(), "forgejo")
	want := []JobState{
		{Name: "forgejo-dump-29887350", Namespace: "forgejo", Owner: JobOwner{Name: "forgejo-dump", UID: "u1"},
			Started: time.Date(2026, 10, 29, 2, 30, 5, 0, time.UTC), Completed: time.Date(2026, 10, 29, 2, 31, 10, 0, time.UTC)},
		{Name: "forgejo-dump-29888790", Namespace: "forgejo", Owner: JobOwner{Name: "forgejo-dump", UID: "u1"}, Started: time.Date(2026, 10, 30, 2, 30, 0, 0, time.UTC)},
		{Name: "manual", Namespace: "forgejo", Started: time.Date(2026, 10, 29, 3, 0, 0, 0, time.UTC)},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Jobs = %+v, %v\nwant %+v", got, err, want)
	}
}

// CronJobUID reads one CronJob's uid, and ErrNotFound for one that is gone.
func TestInClusterKubeCronJobUID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/batch/v1/namespaces/forgejo/cronjobs/forgejo-dump" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"metadata":{"name":"forgejo-dump","uid":"u1"}}`)
	}))
	defer srv.Close()
	k := kubeAt(t, srv)
	if uid, err := k.CronJobUID(context.Background(), "forgejo", "forgejo-dump"); err != nil || uid != "u1" {
		t.Fatalf("CronJobUID = %q, %v; want u1", uid, err)
	}
	if _, err := k.CronJobUID(context.Background(), "forgejo", "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CronJobUID of a gone CronJob = %v, want ErrNotFound", err)
	}
}

// kube429Client points an InClusterKube at srv with a stub token and swaps
// retryWait for a recorder, so no test sleeps.
func kube429Client(t *testing.T, srv *httptest.Server, wait func(context.Context, time.Duration) error) (*InClusterKube, *[]time.Duration) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldDir, oldWait := saDir, retryWait
	saDir = dir
	waits := &[]time.Duration{}
	retryWait = func(ctx context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		if wait != nil {
			return wait(ctx, d)
		}
		return nil
	}
	t.Cleanup(func() { saDir, retryWait = oldDir, oldWait })
	return &InClusterKube{Host: srv.URL, HTTP: srv.Client()}, waits
}

func TestKubeCallRetriesThrottle(t *testing.T) {
	throttle := func(w http.ResponseWriter, after string) {
		if after != "" {
			w.Header().Set("Retry-After", after)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"reason":"TooManyRequests"}`)
	}
	t.Run("two throttles then success", func(t *testing.T) {
		var n int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n++
			if n <= 2 {
				throttle(w, "1")
				return
			}
			io.WriteString(w, `{"ok":true}`)
		}))
		defer srv.Close()
		k, waits := kube429Client(t, srv, nil)
		raw, err := k.call(context.Background(), http.MethodGet, "/x", nil, nil)
		if err != nil || string(raw) != `{"ok":true}` {
			t.Fatalf("raw = %q, err = %v", raw, err)
		}
		if n != 3 || !slices.Equal(*waits, []time.Duration{time.Second, time.Second}) {
			t.Fatalf("requests = %d, waits = %v", n, *waits)
		}
	})
	t.Run("always throttled stops after four requests", func(t *testing.T) {
		var n int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n++
			throttle(w, "1")
		}))
		defer srv.Close()
		k, waits := kube429Client(t, srv, nil)
		_, err := k.call(context.Background(), http.MethodGet, "/x", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "status 429") || !strings.Contains(err.Error(), "TooManyRequests") {
			t.Fatalf("err = %v", err)
		}
		if n != 4 || len(*waits) != 3 {
			t.Fatalf("requests = %d, waits = %v", n, *waits)
		}
	})
	t.Run("no Retry-After is not retried", func(t *testing.T) {
		var n int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n++
			throttle(w, "")
		}))
		defer srv.Close()
		k, waits := kube429Client(t, srv, nil)
		_, err := k.call(context.Background(), http.MethodGet, "/x", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "status 429") {
			t.Fatalf("err = %v", err)
		}
		if n != 1 || len(*waits) != 0 {
			t.Fatalf("requests = %d, waits = %v", n, *waits)
		}
	})
	// 99999999999 passes 2^32: a long wait asked is still the cap.
	for _, after := range []string{"30", "99999999999"} {
		t.Run("wait is capped at five seconds: Retry-After "+after, func(t *testing.T) {
			var n int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n++
				if n == 1 {
					throttle(w, after)
					return
				}
				io.WriteString(w, `{}`)
			}))
			defer srv.Close()
			k, waits := kube429Client(t, srv, nil)
			if _, err := k.call(context.Background(), http.MethodGet, "/x", nil, nil); err != nil {
				t.Fatal(err)
			}
			if n != 2 || !slices.Equal(*waits, []time.Duration{5 * time.Second}) {
				t.Fatalf("requests = %d, waits = %v", n, *waits)
			}
		})
	}
	t.Run("a wait error ends the call", func(t *testing.T) {
		var n int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n++
			if n == 1 {
				throttle(w, "1")
				return
			}
			io.WriteString(w, `{}`)
		}))
		defer srv.Close()
		k, _ := kube429Client(t, srv, func(context.Context, time.Duration) error { return context.Canceled })
		_, err := k.call(context.Background(), http.MethodGet, "/x", nil, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if n != 1 {
			t.Fatalf("requests = %d", n)
		}
	})
	t.Run("Retry-After 0 waits the one second floor", func(t *testing.T) {
		var n int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n++
			if n == 1 {
				throttle(w, "0")
				return
			}
			io.WriteString(w, `{}`)
		}))
		defer srv.Close()
		k, waits := kube429Client(t, srv, nil)
		if _, err := k.call(context.Background(), http.MethodGet, "/x", nil, nil); err != nil {
			t.Fatal(err)
		}
		if n != 2 || !slices.Equal(*waits, []time.Duration{time.Second}) {
			t.Fatalf("requests = %d, waits = %v", n, *waits)
		}
	})
	t.Run("a 503 with Retry-After is not retried", func(t *testing.T) {
		var n int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n++
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		k, waits := kube429Client(t, srv, nil)
		_, err := k.call(context.Background(), http.MethodGet, "/x", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "status 503") {
			t.Fatalf("err = %v", err)
		}
		if n != 1 || len(*waits) != 0 {
			t.Fatalf("requests = %d, waits = %v", n, *waits)
		}
	})
	t.Run("a signed Retry-After is not retried", func(t *testing.T) {
		for _, v := range []string{"-3", "+3"} {
			var n int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n++
				throttle(w, v)
			}))
			k, waits := kube429Client(t, srv, nil)
			_, err := k.call(context.Background(), http.MethodGet, "/x", nil, nil)
			srv.Close()
			if err == nil || !strings.Contains(err.Error(), "status 429") {
				t.Fatalf("%s: err = %v", v, err)
			}
			if n != 1 || len(*waits) != 0 {
				t.Fatalf("%s: requests = %d, waits = %v", v, n, *waits)
			}
		}
	})
	t.Run("every answer's body is closed", func(t *testing.T) {
		var n int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n++
			if n <= 2 {
				throttle(w, "1")
				return
			}
			io.WriteString(w, `{}`)
		}))
		defer srv.Close()
		k, _ := kube429Client(t, srv, nil)
		ct := &closeCounter{rt: srv.Client().Transport}
		k.HTTP = &http.Client{Transport: ct}
		if _, err := k.call(context.Background(), http.MethodGet, "/x", nil, nil); err != nil {
			t.Fatal(err)
		}
		if ct.opened != 3 || ct.closed != 3 {
			t.Fatalf("bodies opened = %d, closed = %d", ct.opened, ct.closed)
		}
	})
	t.Run("a retried POST sends the same body", func(t *testing.T) {
		var bodies []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(b))
			if len(bodies) == 1 {
				throttle(w, "1")
				return
			}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		}))
		defer srv.Close()
		k, _ := kube429Client(t, srv, nil)
		if _, err := k.call(context.Background(), http.MethodPost, "/x", map[string]string{"a": "b"}, nil); err != nil {
			t.Fatal(err)
		}
		want := `{"a":"b"}`
		if !slices.Equal(bodies, []string{want, want}) {
			t.Fatalf("bodies = %q", bodies)
		}
	})
}

// closeCounter counts the response bodies its transport hands out and how
// many of them are closed.
type closeCounter struct {
	rt     http.RoundTripper
	opened int
	closed int
}

func (c *closeCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := c.rt.RoundTrip(r)
	if err == nil {
		c.opened++
		res.Body = &countedBody{ReadCloser: res.Body, c: c}
	}
	return res, err
}

type countedBody struct {
	io.ReadCloser
	c *closeCounter
}

func (b *countedBody) Close() error {
	b.c.closed++
	return b.ReadCloser.Close()
}

// NewestPodEvent reads the events of the pod alone, by field selector, and
// takes the newest by whichever time an event carries: lastTimestamp, an
// eventTime of the newer API, or a series' lastObservedTime. The answers are
// shaped as the API server's EventList.
func TestInClusterKubeNewestPodEvent(t *testing.T) {
	const ns = "/api/v1/namespaces/factory-agents-upstream-1-aa/events"
	scheduled := `{"kind":"Event","reason":"Scheduled","message":"Successfully assigned to sf-1","lastTimestamp":"2026-11-12T10:00:00Z","eventTime":null,` +
		`"involvedObject":{"kind":"Pod","name":"%s"}}`
	sandbox := `{"kind":"Event","reason":"FailedCreatePodSandBox","message":"gvisor: no runsc","lastTimestamp":null,"eventTime":"2026-11-12T10:05:00.123456Z",` +
		`"involvedObject":{"kind":"Pod","name":"%s"}}`
	pulling := `{"kind":"Event","reason":"Pulling","message":"Pulling image","lastTimestamp":"2026-11-12T10:01:00Z","eventTime":null,` +
		`"series":{"count":4,"lastObservedTime":"2026-11-12T10:09:00.000000Z"},"involvedObject":{"kind":"Pod","name":"%s"}}`
	otherPods := `{"kind":"Event","reason":"Killing","message":"another pod's","lastTimestamp":"2026-11-12T11:00:00Z","involvedObject":{"kind":"Pod","name":"worker"}}`
	var selectors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ns {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		sel := r.URL.Query().Get("fieldSelector")
		selectors = append(selectors, sel)
		items := []string{otherPods}
		switch {
		case strings.Contains(sel, "involvedObject.name=p1"):
			items = []string{fmt.Sprintf(scheduled, "p1"), fmt.Sprintf(sandbox, "p1"), fmt.Sprintf(pulling, "p1")}
		case strings.Contains(sel, "involvedObject.name=p2"):
			items = []string{fmt.Sprintf(sandbox, "p2"), fmt.Sprintf(scheduled, "p2")}
		case strings.Contains(sel, "involvedObject.name=p3"):
			items = nil
		}
		io.WriteString(w, `{"kind":"EventList","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[`+strings.Join(items, ",")+`]}`)
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
	for pod, want := range map[string]string{
		"p1": "Pulling: Pulling image",
		"p2": "FailedCreatePodSandBox: gvisor: no runsc",
		"p3": "",
	} {
		got, err := k.NewestPodEvent(context.Background(), "factory-agents-upstream-1-aa", pod)
		if err != nil || got != want {
			t.Errorf("NewestPodEvent(%s) = %q, %v; want %q", pod, got, err, want)
		}
	}
	for _, sel := range selectors {
		if !strings.Contains(sel, "involvedObject.kind=Pod") || !strings.Contains(sel, "involvedObject.name=p") {
			t.Errorf("fieldSelector = %q, want the pod's kind and name", sel)
		}
	}
}

// PodStatus reads why a container waits, an init container's first, with
// the pull's message.
func TestPodStatusReadsAWaitingContainer(t *testing.T) {
	pods := map[string]string{
		"init": `{"status":{"phase":"Pending",
			"initContainerStatuses":[{"state":{"waiting":{"reason":"ErrImagePull","message":"unauthorized: authentication required"}}}],
			"containerStatuses":[{"state":{"waiting":{"reason":"PodInitializing"}}}]}}`,
		"main": `{"status":{"phase":"Pending",
			"initContainerStatuses":[{"state":{"terminated":{"exitCode":0}}}],
			"containerStatuses":[{"state":{"waiting":{"reason":"ImagePullBackOff","message":"Back-off pulling image"}}}]}}`,
		"runs": `{"status":{"phase":"Running","containerStatuses":[{"state":{"running":{}}}]}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, pods[path.Base(r.URL.Path)])
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := SaDirForTest(dir)
	t.Cleanup(func() { SaDirForTest(old) })
	k := &InClusterKube{Host: srv.URL, HTTP: srv.Client()}
	for name, want := range map[string]PodWait{
		"init": {Reason: "ErrImagePull", Message: "unauthorized: authentication required"},
		"main": {Reason: "ImagePullBackOff", Message: "Back-off pulling image"},
		"runs": {},
	} {
		phase, _, wait, err := k.PodStatus(context.Background(), "ns", name)
		if err != nil || wait != want {
			t.Errorf("%s: PodStatus = %q %+v %v, want the wait %+v", name, phase, wait, err, want)
		}
	}
}

func TestPodDiagnosisReadsThePodsPlace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"metadata":{"uid":"u-1","deletionTimestamp":"2026-11-20T10:07:00Z"},`+
			`"spec":{"nodeName":"sf-2","containers":[{"name":"worker","resources":{"limits":{"memory":"4Gi"}}},{"name":"side"}],`+
			`"initContainers":[{"name":"clone","resources":{"limits":{"memory":"1Gi"}}}]},`+
			`"status":{"phase":"Running","startTime":"2026-11-20T10:00:00Z","conditions":[`+
			`{"type":"DisruptionTarget","status":"True","reason":"DeletionByTaintManager","lastTransitionTime":"2026-11-20T10:06:00Z"}],`+
			`"containerStatuses":[{"name":"worker","state":{"running":{}}},{"name":"side","state":{"running":{}}}],`+
			`"initContainerStatuses":[{"name":"clone","state":{"terminated":{"reason":"Completed"}}}]}}`)
	}))
	defer srv.Close()
	d, err := kubeAt(t, srv).PodDiagnosis(context.Background(), "ns", "p")
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	if d.UID != "u-1" || d.Node != "sf-2" || !d.StartedAt.Equal(at("2026-11-20T10:00:00Z")) || !d.Deleting {
		t.Errorf("place = %+v", d)
	}
	if len(d.Conditions) != 1 || d.Conditions[0].Type != "DisruptionTarget" || !d.Conditions[0].Since.Equal(at("2026-11-20T10:06:00Z")) {
		t.Errorf("conditions = %+v", d.Conditions)
	}
	var limits []string
	for _, c := range d.Containers {
		limits = append(limits, c.Name+"="+c.MemoryLimit)
	}
	if got := strings.Join(limits, ","); got != "clone=1Gi,worker=4Gi,side=" {
		t.Errorf("limits = %s", got)
	}
}

func TestNodeStateReadsTheBootID(t *testing.T) {
	var sel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/nodes" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		sel = r.URL.Query().Get("fieldSelector")
		if r.URL.Query().Get("fieldSelector") == "metadata.name=sf-1" {
			io.WriteString(w, `{"items":[{"metadata":{"name":"sf-1"},"status":{"nodeInfo":{"bootID":"b1"},"conditions":[`+
				`{"type":"MemoryPressure","status":"False","lastTransitionTime":"2026-11-20T09:00:00Z"},`+
				`{"type":"Ready","status":"False","lastTransitionTime":"2026-11-20T10:00:00Z"}]}}]}`)
			return
		}
		io.WriteString(w, `{"items":[]}`)
	}))
	defer srv.Close()
	k := kubeAt(t, srv)
	n, err := k.NodeState(context.Background(), "sf-1")
	if err != nil || sel != "metadata.name=sf-1" {
		t.Fatalf("NodeState = %+v, %v, selector %q", n, err, sel)
	}
	if n.Name != "sf-1" || n.BootID != "b1" || n.Ready || !n.Since.Equal(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("node = %+v", n)
	}
	if _, err := k.NodeState(context.Background(), "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing node err = %v, want ErrNotFound", err)
	}
}

func TestNodesReadsEveryNode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"items":[{"metadata":{"name":"a"},"status":{"nodeInfo":{"bootID":"ba"},"conditions":[{"type":"Ready","status":"True","lastTransitionTime":"2026-11-20T10:00:00Z"}]}},`+
			`{"metadata":{"name":"b"},"status":{"nodeInfo":{"bootID":"bb"}}}]}`)
	}))
	defer srv.Close()
	ns, err := kubeAt(t, srv).Nodes(context.Background())
	if err != nil || len(ns) != 2 || !ns[0].Ready || ns[0].BootID != "ba" || ns[1].Ready || !ns[1].Since.IsZero() {
		t.Fatalf("Nodes = %+v, %v", ns, err)
	}
}

func TestPodEventsOldestFirst(t *testing.T) {
	long := strings.Repeat("x", 500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"items":[`+
			`{"reason":"Late","type":"Warning","message":"`+long+`","lastTimestamp":"2026-11-12T10:09:00Z","count":3},`+
			`{"reason":"First","type":"Normal","message":"m","firstTimestamp":"2026-11-12T10:00:00Z"},`+
			`{"reason":"Mid","type":"Normal","message":"m","eventTime":"2026-11-12T10:05:00Z","series":{"count":4,"lastObservedTime":"2026-11-12T10:06:00Z"}}]}`)
	}))
	defer srv.Close()
	evs, err := kubeAt(t, srv).PodEvents(context.Background(), "ns", "p")
	if err != nil || len(evs) != 3 {
		t.Fatalf("PodEvents = %+v, %v", evs, err)
	}
	if evs[0].Reason != "First" || evs[1].Reason != "Mid" || evs[2].Reason != "Late" {
		t.Errorf("order = %s %s %s", evs[0].Reason, evs[1].Reason, evs[2].Reason)
	}
	if evs[2].Message != long || evs[2].Type != "Warning" || evs[2].Count != 3 || evs[1].Count != 4 {
		t.Errorf("late = %+v, mid count %d", evs[2], evs[1].Count)
	}
	if !evs[1].At.Equal(time.Date(2026, 11, 12, 10, 6, 0, 0, time.UTC)) {
		t.Errorf("mid at = %v, want the series' last observed time", evs[1].At)
	}
}

// VolumeClaims lists a namespace's volume claims with their phase, and reads the
// events of each claim that is not Bound, by its kind and name, for the
// newest: where a provisioner says why it refused the claim.
func TestInClusterKubeVolumeClaims(t *testing.T) {
	var selectors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces/factory-agents-ci-7/persistentvolumeclaims":
			io.WriteString(w, `{"items":[{"metadata":{"name":"cache-ci-demo"},"status":{"phase":"Pending"}},`+
				`{"metadata":{"name":"data"},"status":{"phase":"Bound"}}]}`)
		case "/api/v1/namespaces/factory-agents-ci-7/events":
			selectors = append(selectors, r.URL.Query().Get("fieldSelector"))
			io.WriteString(w, `{"items":[`+
				`{"reason":"ProvisioningFailed","message":"the shared path needs its switch","lastTimestamp":"2026-11-26T04:15:00Z"},`+
				`{"reason":"ExternalProvisioning","message":"waiting for a volume to be created","lastTimestamp":"2026-11-26T04:14:00Z"}]}`)
		default:
			t.Errorf("%s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	got, err := kubeAt(t, srv).VolumeClaims(context.Background(), "factory-agents-ci-7")
	want := []VolumeClaim{{Name: "cache-ci-demo", Phase: "Pending", Event: "ProvisioningFailed: the shared path needs its switch"}, {Name: "data", Phase: "Bound"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Claims = %+v, %v; want %+v", got, err, want)
	}
	if !slices.Equal(selectors, []string{"involvedObject.kind=PersistentVolumeClaim,involvedObject.name=cache-ci-demo"}) {
		t.Errorf("event reads %q, want the unbound claim's alone", selectors)
	}
}

func TestPodEventsKeepTheNewestTwenty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var items []string
		for i := 25; i > 0; i-- {
			items = append(items, fmt.Sprintf(`{"reason":"R%d","lastTimestamp":"2026-11-12T10:%02d:00Z"}`, i, i))
		}
		io.WriteString(w, `{"items":[`+strings.Join(items, ",")+`]}`)
	}))
	defer srv.Close()
	evs, err := kubeAt(t, srv).PodEvents(context.Background(), "ns", "p")
	if err != nil || len(evs) != 20 || evs[0].Reason != "R6" || evs[19].Reason != "R25" {
		t.Fatalf("PodEvents = %d events, %v", len(evs), err)
	}
}

func TestServicePodsReadTheLastTermination(t *testing.T) {
	var q url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/proj/pods" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		q = r.URL.Query()
		io.WriteString(w, `{"items":[{"metadata":{"namespace":"proj","name":"web-1","uid":"u9","ownerReferences":[{"kind":"Foo","controller":false},{"kind":"Job","controller":true}]},"spec":{"nodeName":"sf-3"},`+
			`"status":{"phase":"Running","containerStatuses":[{"name":"web","restartCount":2,`+
			`"state":{"running":{"startedAt":"2026-11-20T10:10:00Z"}},`+
			`"lastState":{"terminated":{"reason":"OOMKilled","exitCode":137,"startedAt":"2026-11-20T10:00:00Z","finishedAt":"2026-11-20T10:09:00Z"}}}]}}]}`)
	}))
	defer srv.Close()
	ps, err := kubeAt(t, srv).ServicePods(context.Background(), "proj", "app=web")
	if err != nil || q.Get("labelSelector") != "app=web" || len(ps) != 1 {
		t.Fatalf("ServicePods = %+v, %v, query %v", ps, err, q)
	}
	p := ps[0]
	if p.Namespace != "proj" || p.Name != "web-1" || p.UID != "u9" || p.Node != "sf-3" || p.Phase != "Running" || p.Owner != "Job" || len(p.Containers) != 1 {
		t.Fatalf("pod = %+v", p)
	}
	c := p.Containers
	at := func(m int) time.Time { return time.Date(2026, 11, 20, 10, m, 0, 0, time.UTC) }
	if c[0].Name != "web" || c[0].Restarts != 2 || c[0].LastReason != "OOMKilled" || c[0].LastExitCode != 137 ||
		!c[0].LastStarted.Equal(at(0)) || !c[0].LastFinished.Equal(at(9)) || !c[0].RunningSince.Equal(at(10)) {
		t.Errorf("container = %+v", c[0])
	}
}

func TestNewestPodEventStillReadsTheNewest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"items":[{"reason":"New","message":"n","lastTimestamp":"2026-11-12T10:09:00Z"},{"reason":"Old","message":"o","lastTimestamp":"2026-11-12T10:01:00Z"}]}`)
	}))
	defer srv.Close()
	got, err := kubeAt(t, srv).NewestPodEvent(context.Background(), "ns", "p")
	if err != nil || got != "New: n" {
		t.Fatalf("NewestPodEvent = %q, %v", got, err)
	}
}

func TestNewestPodEventTakesTheFirstOfATieWhole(t *testing.T) {
	long := strings.Repeat("x", 500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"items":[{"reason":"Old","message":"o","lastTimestamp":"2026-11-12T10:01:00Z"},`+
			`{"reason":"A","message":"`+long+`","lastTimestamp":"2026-11-12T10:09:00Z"},`+
			`{"reason":"B","message":"b","lastTimestamp":"2026-11-12T10:09:00Z"}]}`)
	}))
	defer srv.Close()
	got, err := kubeAt(t, srv).NewestPodEvent(context.Background(), "ns", "p")
	if err != nil || got != "A: "+long {
		t.Fatalf("NewestPodEvent = %q, %v; want the first of the tie, whole", got, err)
	}
}
