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
	"strconv"
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

// SuspendKustomization is one server-side apply of spec.suspend, with no
// read first; a refused patch is ErrForbidden.
func TestKubeSuspendKustomization(t *testing.T) {
	var seen []string
	forbid := false
	k := stubKube(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+r.Header.Get("Content-Type")+" "+string(body))
		if forbid {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"kind":"Status","reason":"Forbidden"}`)
			return
		}
		io.WriteString(w, `{"metadata":{"resourceVersion":"8"}}`)
	})
	const patch = "PATCH /apis/kustomize.toolkit.fluxcd.io/v1/namespaces/flux-system/kustomizations/factory?fieldManager=factoryd&force=true application/apply-patch+yaml "
	for _, suspend := range []bool{true, false} {
		seen = nil
		if err := k.SuspendKustomization(context.Background(), "flux-system", "factory", suspend); err != nil {
			t.Fatalf("suspend %v: %v", suspend, err)
		}
		want := patch + `{"apiVersion":"kustomize.toolkit.fluxcd.io/v1","kind":"Kustomization","metadata":{"name":"factory","namespace":"flux-system"},"spec":{"suspend":` + strconv.FormatBool(suspend) + `}}`
		if !reflect.DeepEqual(seen, []string{want}) {
			t.Fatalf("suspend %v sent %q, want one request %q", suspend, seen, want)
		}
	}
	forbid = true
	if err := k.SuspendKustomization(context.Background(), "flux-system", "factory", true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a refused patch: %v, want ErrForbidden", err)
	}
}

// FluxRollouts lists every Kustomization but the factory's own (it applies
// a roll target's Deployment in the namespace asked), a project
// environment's scoped to its namespace, and every HelmRelease with Flux's
// Ready message. A Kustomization's rollout starts and settles at its
// newest content's first reconcile; a reverted content keeps its old first
// reconcile, so its rollout starts when the content before it last
// reconciled and settles one interval later. A HelmRelease's runs from its
// newest release's deploy until Ready reads True or False for its
// generation, never before the deploy; Ready Unknown, or a generation not
// observed yet, still runs. An object Flux never applied has none.
func TestKubeFluxRollouts(t *testing.T) {
	k := stubKube(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kustomize.toolkit.fluxcd.io/v1/kustomizations" && r.URL.RawQuery == "":
			io.WriteString(w, `{"items":[
				{"metadata":{"namespace":"flux-system","name":"factory"},"spec":{"interval":"10m"},
				 "status":{"history":[{"firstReconciled":"2026-10-15T11:00:37Z","lastReconciled":"2026-10-15T11:00:37Z"}],
				  "inventory":{"entries":[{"id":"factory_factoryd_apps_Deployment","v":"v1"}]}}},
				{"metadata":{"namespace":"flux-system","name":"platform"},"spec":{"interval":"10m"},
				 "status":{"history":[{"firstReconciled":"2026-10-15T10:49:30Z","lastReconciled":"2026-10-15T11:00:39Z"},
				  {"firstReconciled":"2026-10-15T10:35:53Z","lastReconciled":"2026-10-15T10:45:25Z"}],
				  "inventory":{"entries":[{"id":"platform_sf-cluster_postgresql.cnpg.io_Cluster","v":"v1"},{"id":"other_factoryd_apps_Deployment","v":"v1"}]}}},
				{"metadata":{"namespace":"flux-system","name":"reverted"},"spec":{"interval":"10m0s"},
				 "status":{"history":[{"firstReconciled":"2026-10-14T08:00:00Z","lastReconciled":"2026-10-15T11:00:00Z"},
				  {"firstReconciled":"2026-10-15T10:20:00Z","lastReconciled":"2026-10-15T10:41:00Z"}]}},
				{"metadata":{"namespace":"flux-system","name":"fresh"}},
				{"metadata":{"namespace":"demo-staging","name":"demo-app","labels":{"factory/project":"demo-app"}},"spec":{"interval":"1m"},
				 "status":{"history":[{"firstReconciled":"2026-10-15T10:58:00Z","lastReconciled":"2026-10-15T11:02:00Z"}]}}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/helm.toolkit.fluxcd.io/v2/helmreleases":
			io.WriteString(w, `{"items":[
				{"metadata":{"namespace":"platform","name":"signoz","generation":3},
				 "status":{"conditions":[{"type":"Released","status":"True","observedGeneration":3,"lastTransitionTime":"2026-10-15T10:50:01Z"},
				  {"type":"Ready","status":"True","observedGeneration":3,"lastTransitionTime":"2026-10-15T10:50:04Z"}],
				  "history":[{"lastDeployed":"2026-10-15T10:49:31Z"},{"lastDeployed":"2026-10-15T10:36:13Z"}]}},
				{"metadata":{"namespace":"platform","name":"upgrading","generation":5},
				 "status":{"conditions":[{"type":"Ready","status":"Unknown","observedGeneration":5,"lastTransitionTime":"2026-10-15T11:01:00Z",
				  "message":"Running 'upgrade' action with timeout of 5m0s"}],
				  "history":[{"lastDeployed":"2026-10-15T11:00:58Z"}]}},
				{"metadata":{"namespace":"platform","name":"unobserved","generation":6},
				 "status":{"conditions":[{"type":"Ready","status":"True","observedGeneration":5,"lastTransitionTime":"2026-10-15T10:20:00Z"}],
				  "history":[{"lastDeployed":"2026-10-15T10:19:00Z"}]}},
				{"metadata":{"namespace":"forgejo","name":"failed","generation":2},
				 "status":{"conditions":[{"type":"Ready","status":"False","observedGeneration":2,"lastTransitionTime":"2026-10-15T10:47:10Z"}],
				  "history":[{"lastDeployed":"2026-10-15T10:42:10Z"}]}},
				{"metadata":{"namespace":"forgejo","name":"quick","generation":1},
				 "status":{"conditions":[{"type":"Ready","status":"True","observedGeneration":1,"lastTransitionTime":"2026-10-15T10:59:59Z"}],
				  "history":[{"lastDeployed":"2026-10-15T11:00:01Z"}]}},
				{"metadata":{"namespace":"forgejo","name":"new","generation":1}}]}`)
		default:
			t.Errorf("%s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	got, err := k.FluxRollouts(context.Background(), "factory")
	want := []Rollout{
		{Kind: "Kustomization", Namespace: "flux-system", Name: "platform", Applied: at("2026-10-15T10:49:30Z"), Settled: at("2026-10-15T10:49:30Z")},
		{Kind: "Kustomization", Namespace: "flux-system", Name: "reverted", Applied: at("2026-10-15T10:41:00Z"), Settled: at("2026-10-15T10:51:00Z")},
		{Kind: "Kustomization", Namespace: "flux-system", Name: "fresh"},
		{Kind: "Kustomization", Namespace: "demo-staging", Name: "demo-app", Scope: "demo-staging",
			Applied: at("2026-10-15T10:58:00Z"), Settled: at("2026-10-15T10:58:00Z")},
		{Kind: "HelmRelease", Namespace: "platform", Name: "signoz", Applied: at("2026-10-15T10:49:31Z"), Settled: at("2026-10-15T10:50:04Z")},
		{Kind: "HelmRelease", Namespace: "platform", Name: "upgrading", Applied: at("2026-10-15T11:00:58Z"),
			Message: "Running 'upgrade' action with timeout of 5m0s"},
		{Kind: "HelmRelease", Namespace: "platform", Name: "unobserved", Applied: at("2026-10-15T10:19:00Z")},
		{Kind: "HelmRelease", Namespace: "forgejo", Name: "failed", Applied: at("2026-10-15T10:42:10Z"), Settled: at("2026-10-15T10:47:10Z")},
		{Kind: "HelmRelease", Namespace: "forgejo", Name: "quick", Applied: at("2026-10-15T11:00:01Z"), Settled: at("2026-10-15T11:00:01Z")},
		{Kind: "HelmRelease", Namespace: "forgejo", Name: "new"},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("FluxRollouts = %+v, %v\nwant %+v", got, err, want)
	}
}
