package factoryd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func stubKube(t *testing.T, h http.HandlerFunc) *InClusterKube {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldDir := saDir
	saDir = dir
	t.Cleanup(func() { saDir = oldDir })
	return &InClusterKube{Host: srv.URL, HTTP: srv.Client()}
}

// Apply tells created, updated and unchanged apart by the resourceVersion
// before and after a server-side apply.
func TestKubeApply(t *testing.T) {
	const path = "/api/v1/namespaces/demo/resourcequotas/project"
	rv := map[string]string{"new": "", "same": "7", "changed": "7"}
	after := map[string]string{"new": "1", "same": "7", "changed": "8"}
	var ct, query string
	var name string
	k := stubKube(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			t.Errorf("path %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			if rv[name] == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			io.WriteString(w, `{"metadata":{"resourceVersion":"`+rv[name]+`"}}`)
		case http.MethodPatch:
			ct, query = r.Header.Get("Content-Type"), r.URL.RawQuery
			io.WriteString(w, `{"metadata":{"resourceVersion":"`+after[name]+`"}}`)
		}
	})
	for n, want := range map[string]Change{"new": Created, "same": Unchanged, "changed": Updated} {
		name = n
		got, err := k.Apply(context.Background(), path, map[string]any{"apiVersion": "v1", "kind": "ResourceQuota"})
		if err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", n, got, err, want)
		}
	}
	if ct != "application/apply-patch+yaml" || query != "fieldManager=factoryd&force=true" {
		t.Fatalf("apply sent %q ?%s", ct, query)
	}
}

func TestDeploymentRolledOut(t *testing.T) {
	for name, tc := range map[string]struct {
		d   DeploymentState
		ok  bool
		why string
	}{
		"done":         {DeploymentState{Generation: 3, ObservedGeneration: 3, Replicas: 1, StatusReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}, true, ""},
		"not observed": {DeploymentState{Generation: 4, ObservedGeneration: 3, Replicas: 1, StatusReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}, false, "observed generation 3 of 4"},
		// review-task64 I3: the new pod crashloops, the old one serves
		"new pod not ready": {DeploymentState{Generation: 4, ObservedGeneration: 4, Replicas: 1, StatusReplicas: 2, UpdatedReplicas: 1, AvailableReplicas: 1}, false, "1 old replicas pending termination"},
		"not updated":       {DeploymentState{Generation: 4, ObservedGeneration: 4, Replicas: 2, StatusReplicas: 2, UpdatedReplicas: 1, AvailableReplicas: 2}, false, "1 of 2 replicas updated"},
		"unavailable":       {DeploymentState{Generation: 4, ObservedGeneration: 4, Replicas: 1, StatusReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 0}, false, "0 of 1 updated replicas available"},
		"scaled to zero":    {DeploymentState{Generation: 2, ObservedGeneration: 2}, true, ""},
	} {
		if ok, why := tc.d.RolledOut(); ok != tc.ok || why != tc.why {
			t.Errorf("%s: %v %q, want %v %q", name, ok, why, tc.ok, tc.why)
		}
	}
}

func TestKubeDeployments(t *testing.T) {
	k := stubKube(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/apps/v1/namespaces/demo-staging/deployments" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"items":[{"metadata":{"name":"demo-app","generation":5},
			"spec":{"replicas":2,"template":{"spec":{"containers":[
				{"image":"git.aisoftwarefactory.world/sf/demo-app@sha256:`+strings.Repeat("a", 64)+`"}]}}},
			"status":{"observedGeneration":5,"replicas":3,"updatedReplicas":2,"availableReplicas":2}},
			{"metadata":{"name":"one","generation":1},"spec":{},"status":{}}]}`)
	})
	got, err := k.Deployments(context.Background(), "demo-staging")
	want := []DeploymentState{{Name: "demo-app", Generation: 5, ObservedGeneration: 5, Replicas: 2, StatusReplicas: 3, UpdatedReplicas: 2, AvailableReplicas: 2,
		Images: []string{"git.aisoftwarefactory.world/sf/demo-app@sha256:" + strings.Repeat("a", 64)}},
		{Name: "one", Generation: 1, Replicas: 1}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Deployments = %+v, %v; want %+v", got, err, want)
	}
}

// KustomizationState reads the Ready condition's status and message and
// the last applied revision: a Kustomization that applied once keeps its
// revision while it fails, one that never applied has none, one without
// a Ready condition reads "", and a missing Kustomization is ErrNotFound.
func TestKubeKustomizationState(t *testing.T) {
	bodies := map[string]string{
		"ready": `{"status":{"conditions":[{"type":"Ready","status":"True","reason":"ReconciliationSucceeded",
			"message":"Applied revision: deploy@sha1:5f6d735b077a3bd077e556109d5651d4abfb8456"}],
			"lastAppliedRevision":"deploy@sha1:5f6d735b077a3bd077e556109d5651d4abfb8456"}}`,
		"broken": `{"status":{"conditions":[{"type":"Ready","status":"False","reason":"ReconciliationFailed",
			"message":"Secret/broken/db dry-run failed: secrets \"db\" not found"}],
			"lastAppliedRevision":"deploy@sha1:5f6d735b077a3bd077e556109d5651d4abfb8456",
			"lastAttemptedRevision":"deploy@sha1:0d805847b1c58eed2c5524a91e5c009f895ffdc0"}}`,
		"missing": `{"status":{"conditions":[{"type":"Reconciling","status":"True","message":"Fetching manifests"},
			{"type":"Ready","status":"False","reason":"ArtifactFailed",
			"message":"kustomization path not found: stat /tmp/kustomization-103304189/deploy/staging: no such file or directory"}]}}`,
		"progressing": `{"status":{"conditions":[{"type":"Ready","status":"Unknown","message":"Reconciliation in progress"}]}}`,
		"new":         `{"metadata":{"name":"new"}}`,
	}
	k := stubKube(t, func(w http.ResponseWriter, r *http.Request) {
		ns, ok := strings.CutPrefix(r.URL.Path, "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/")
		ns, name, _ := strings.Cut(ns, "/kustomizations/")
		body, known := bodies[ns]
		if r.Method != http.MethodGet || !ok || name != "app" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if !known {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		io.WriteString(w, body)
	})
	for ns, want := range map[string]FluxState{
		"ready": {"True", "Applied revision: deploy@sha1:5f6d735b077a3bd077e556109d5651d4abfb8456",
			"deploy@sha1:5f6d735b077a3bd077e556109d5651d4abfb8456"},
		"broken": {"False", `Secret/broken/db dry-run failed: secrets "db" not found`,
			"deploy@sha1:5f6d735b077a3bd077e556109d5651d4abfb8456"},
		"missing":     {"False", "kustomization path not found: stat /tmp/kustomization-103304189/deploy/staging: no such file or directory", ""},
		"progressing": {"Unknown", "Reconciliation in progress", ""},
		"new":         {},
	} {
		got, err := k.KustomizationState(context.Background(), ns, "app")
		if err != nil || got != want {
			t.Errorf("%s: %+v, %v; want %+v", ns, got, err, want)
		}
	}
	if _, err := k.KustomizationState(context.Background(), "gone", "app"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing Kustomization: %v, want ErrNotFound", err)
	}
}
