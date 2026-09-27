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
	"strings"
	"sync"
	"testing"
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
			io.WriteString(w, `{"items":[{"metadata":{"name":"factory-agents-t1"}}]}`)
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
	if err != nil || len(names) != 1 || names[0] != "factory-agents-t1" {
		t.Fatalf("NamespacesByLabel = %v, %v", names, err)
	}
	if _, err := k.NamespacesByLabel(ctx, "factory.ai/task", ""); err != nil {
		t.Fatalf("NamespacesByLabel key-only: %v", err)
	}
	// "factory.ai/task=" would select only namespaces whose label is empty
	if fmt.Sprint(selectors) != "[factory.ai/task=t1 factory.ai/task]" {
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
