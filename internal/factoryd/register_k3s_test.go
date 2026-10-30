package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRegistrationClusterOnAnAPIServer runs registration's cluster part as
// factoryd's service account against a real API server carrying
// k8s/factoryd.yaml's RBAC and admission policy (and stand-in Flux and
// Cilium CRDs), then tries what the policy must refuse: FACTORYD_APISERVER
// is host:port, FACTORYD_APISERVER_SA a directory with factoryd's token
// and the CA, FACTORYD_APISERVER_ADMIN one with an admin's token (for the
// access reviews of the flux account).
func TestRegistrationClusterOnAnAPIServer(t *testing.T) {
	addr, dir, adminDir := os.Getenv("FACTORYD_APISERVER"), os.Getenv("FACTORYD_APISERVER_SA"), os.Getenv("FACTORYD_APISERVER_ADMIN")
	if addr == "" || dir == "" || adminDir == "" {
		t.Skip("FACTORYD_APISERVER, FACTORYD_APISERVER_SA and FACTORYD_APISERVER_ADMIN not set")
	}
	host, port, _ := net.SplitHostPort(addr)
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)
	oldDir := saDir
	saDir = dir
	t.Cleanup(func() { saDir = oldDir })
	kube, err := NewInClusterKube()
	if err != nil {
		t.Fatal(err)
	}
	reg := &Registrar{Kube: kube, FactoryNamespace: "factory", PullSecret: "forgejo-registry",
		GitURL: "https://git.aisoftwarefactory.world", RegistryHost: "git.aisoftwarefactory.world", ProjectsUser: "factory-projects"}
	p := demoProject()
	p.PullToken, p.GitToken = "pull-tok", "git-tok"
	logs := captureLog(t)
	ctx := context.Background()
	cluster := func() []error {
		run := &reconcileRun{r: reg, p: p}
		run.cluster(ctx)
		return run.errs
	}
	if errs := cluster(); len(errs) != 0 {
		t.Fatalf("first run: %v\n%s", errs, logs)
	}
	logs.Reset()
	if errs := cluster(); len(errs) != 0 {
		t.Fatalf("second run: %v", errs)
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if !strings.HasSuffix(line, ": no change") {
			t.Errorf("second run: %s", line)
		}
	}
	if _, err := kube.Deployments(ctx, "demo-staging"); err != nil {
		t.Fatalf("deployments: %v", err)
	}
	p.Quota.CPU = "3"
	logs.Reset()
	if errs := cluster(); len(errs) != 0 || !strings.Contains(logs.String(), "demo-staging resourcequota: updated") {
		t.Fatalf("quota change: %v\n%s", errs, logs)
	}

	// what factoryd must never do (review-task65 m4, m10)
	refused := func(name, method, path string, obj map[string]any) {
		t.Helper()
		_, err := kube.call(ctx, method, path, obj, nil)
		if err == nil || !(errors.Is(err, ErrForbidden) || strings.Contains(err.Error(), "ValidatingAdmissionPolicy 'factoryd-task-namespaces'")) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	apply := func(path string) string { return path + "?fieldManager=factoryd&force=true" }
	ns := func(name string, labels map[string]string) map[string]any {
		return map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": name, "labels": labels}}
	}
	refused("label kube-system a project", http.MethodPatch, apply("/api/v1/namespaces/kube-system"),
		ns("kube-system", map[string]string{ProjectLabel: "x", "pod-security.kubernetes.io/enforce": "restricted"}))
	refused("label factory a project", http.MethodPatch, apply("/api/v1/namespaces/factory"),
		ns("factory", map[string]string{ProjectLabel: "x", "pod-security.kubernetes.io/enforce": "restricted"}))
	refused("project namespace without restricted", http.MethodPost, "/api/v1/namespaces", ns("p1", map[string]string{ProjectLabel: "p1"}))
	refused("move a namespace to another project", http.MethodPatch, apply("/api/v1/namespaces/demo-prod"),
		ns("demo-prod", map[string]string{ProjectLabel: "other", "pod-security.kubernetes.io/enforce": "restricted"}))
	refused("delete a project namespace", http.MethodDelete, "/api/v1/namespaces/demo-prod", nil)
	rb := func(role, sa, saNS string) map[string]any {
		return roleBinding("demo-staging", "evil", role, nil, map[string]any{"kind": "ServiceAccount", "name": sa, "namespace": saNS})
	}
	rbs := "/apis/rbac.authorization.k8s.io/v1/namespaces/demo-staging/rolebindings"
	refused("bind the deployer to another account", http.MethodPost, rbs, rb(DeployerRole, "default", "demo-staging"))
	refused("bind admin to flux", http.MethodPost, rbs, rb("admin", "flux", "demo-staging"))
	refused("bind factoryd-project to flux", http.MethodPost, rbs, rb(ProjectReaderRole, "flux", "demo-staging"))
	gr := func(url string, extra map[string]any) map[string]any {
		spec := map[string]any{"interval": "1m", "url": url, "ref": map[string]any{"branch": "main"}, "secretRef": map[string]any{"name": "demo-app-git"}}
		for k, v := range extra {
			spec[k] = v
		}
		return map[string]any{"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "GitRepository",
			"metadata": map[string]any{"name": "demo-app", "namespace": "flux-system", "labels": map[string]string{ProjectLabel: "demo-app"}}, "spec": spec}
	}
	grPath := apply("/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories/demo-app")
	refused("a Flux source at another host", http.MethodPatch, grPath, gr("https://evil.example/x", nil))
	refused("a Flux source with another secret", http.MethodPatch, grPath, gr("https://git.aisoftwarefactory.world/demo-app/demo-app",
		map[string]any{"secretRef": map[string]any{"name": "forgejo-pat"}}))
	refused("a Flux source including another", http.MethodPatch, grPath, gr("https://git.aisoftwarefactory.world/demo-app/demo-app",
		map[string]any{"include": []any{map[string]any{"repository": map[string]any{"name": "flux-system"}}}}))
	refused("a Flux source with a proxy", http.MethodPatch, grPath, gr("https://git.aisoftwarefactory.world/demo-app/demo-app",
		map[string]any{"proxySecretRef": map[string]any{"name": "x"}}))
	refused("another project's git secret", http.MethodPost, "/api/v1/namespaces/flux-system/secrets", map[string]any{"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": "forgejo-pat2", "labels": map[string]string{ProjectLabel: "demo-app"}}})
	ks := func(ns string, spec map[string]any) map[string]any {
		return map[string]any{"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
			"metadata": map[string]any{"name": "other", "namespace": "demo-staging"}, "spec": spec}
	}
	kss := "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/demo-staging/kustomizations"
	src := map[string]any{"kind": "GitRepository", "name": "demo-app", "namespace": "flux-system"}
	refused("a Kustomization without the flux account", http.MethodPost, kss, ks("demo-staging",
		map[string]any{"path": "./x", "sourceRef": src, "targetNamespace": "demo-staging"}))
	refused("a Kustomization into another namespace", http.MethodPost, kss, ks("demo-staging",
		map[string]any{"path": "./x", "sourceRef": src, "targetNamespace": "demo-prod", "serviceAccountName": "flux"}))
	refused("a pod in a project namespace", http.MethodPost, "/api/v1/namespaces/demo-staging/pods", map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "x"}, "spec": map[string]any{"containers": []any{map[string]any{"name": "x", "image": "busybox"}}}})
	refused("a configmap in a project namespace", http.MethodPost, "/api/v1/namespaces/demo-staging/configmaps",
		map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x"}})
	refused("a token for the flux account", http.MethodPost, "/api/v1/namespaces/demo-staging/secrets", map[string]any{"apiVersion": "v1", "kind": "Secret",
		"type": "kubernetes.io/service-account-token", "metadata": map[string]any{"name": "flux-token",
			"annotations": map[string]string{"kubernetes.io/service-account.name": "flux"}}})
	refused("a network policy of another name", http.MethodPost, "/apis/cilium.io/v2/namespaces/demo-staging/ciliumnetworkpolicies",
		map[string]any{"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy", "metadata": map[string]any{"name": "open"}, "spec": map[string]any{}})
	refused("a secret in kube-system", http.MethodPost, "/api/v1/namespaces/kube-system/secrets",
		map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "x"}})

	// what the project's flux account may do, asked as an admin
	token, err := os.ReadFile(filepath.Join(adminDir, "token"))
	if err != nil {
		t.Fatal(err)
	}

	// a seed project's namespace (ruling 180): the infra repository labels
	// it factory/project and factory/objects=seed; factoryd acts in it as
	// in any namespace not its own, not at all, even were it restricted
	seedLabels := map[string]string{ProjectLabel: "ai-software-factory-infra", ObjectsLabel: "seed", "pod-security.kubernetes.io/enforce": "restricted"}
	{
		b, _ := json.Marshal(ns("seed-ns", seedLabels))
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, kube.Host+"/api/v1/namespaces", strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		req.Header.Set("Content-Type", "application/json")
		res, err := kube.HTTP.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusConflict {
			t.Fatalf("admin creates seed-ns: %d", res.StatusCode)
		}
	}
	refused("bind factoryd-project in a seed namespace", http.MethodPost, "/apis/rbac.authorization.k8s.io/v1/namespaces/seed-ns/rolebindings",
		roleBinding("seed-ns", "factoryd", ProjectReaderRole, nil, map[string]any{"kind": "ServiceAccount", "name": "factoryd", "namespace": "factory"}))
	refused("a secret in a seed namespace", http.MethodPost, "/api/v1/namespaces/seed-ns/secrets",
		map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "x"}})
	refused("take the seed mark off", http.MethodPatch, apply("/api/v1/namespaces/seed-ns"),
		ns("seed-ns", map[string]string{ProjectLabel: "ai-software-factory-infra", "pod-security.kubernetes.io/enforce": "restricted"}))
	refused("create a namespace with the seed mark", http.MethodPost, "/api/v1/namespaces", ns("seed-2", seedLabels))
	can := func(ns, group, resource, verb string) bool {
		t.Helper()
		body := map[string]any{"apiVersion": "authorization.k8s.io/v1", "kind": "SubjectAccessReview", "spec": map[string]any{
			"user": "system:serviceaccount:demo-staging:flux", "groups": []string{"system:serviceaccounts", "system:serviceaccounts:demo-staging"},
			"resourceAttributes": map[string]any{"namespace": ns, "group": group, "resource": resource, "verb": verb}}}
		b, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, kube.Host+"/apis/authorization.k8s.io/v1/subjectaccessreviews", strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		req.Header.Set("Content-Type", "application/json")
		res, err := kube.HTTP.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out struct {
			Status struct {
				Allowed bool `json:"allowed"`
			} `json:"status"`
		}
		json.NewDecoder(res.Body).Decode(&out)
		return out.Status.Allowed
	}
	for _, c := range []struct {
		ns, group, resource, verb string
		want                      bool
	}{
		{"demo-staging", "apps", "deployments", "create", true},
		{"demo-staging", "postgresql.cnpg.io", "clusters", "create", true},
		{"demo-staging", "gateway.networking.k8s.io", "httproutes", "create", true},
		{"demo-prod", "apps", "deployments", "create", false},
		{"demo-staging", "kustomize.toolkit.fluxcd.io", "kustomizations", "create", false},
		{"demo-staging", "kustomize.toolkit.fluxcd.io", "kustomizations", "patch", false},
		{"demo-staging", "rbac.authorization.k8s.io", "rolebindings", "create", false},
		{"demo-staging", "cilium.io", "ciliumnetworkpolicies", "patch", false},
		{"demo-staging", "networking.k8s.io", "networkpolicies", "create", false},
		{"demo-staging", "", "resourcequotas", "update", false},
		{"", "rbac.authorization.k8s.io", "clusterrolebindings", "create", false},
	} {
		if got := can(c.ns, c.group, c.resource, c.verb); got != c.want {
			t.Errorf("flux %s %s.%s in %q = %v, want %v", c.verb, c.resource, c.group, c.ns, got, c.want)
		}
	}

	// the factory's own Kustomization: factoryd flips its spec.suspend and
	// changes nothing else of it, and no other Kustomization of flux-system
	asAdmin := func(method, path string, body, into any) int {
		t.Helper()
		var r io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		}
		req, _ := http.NewRequestWithContext(ctx, method, kube.Host+path, r)
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		req.Header.Set("Content-Type", "application/json")
		res, err := kube.HTTP.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if into != nil {
			json.NewDecoder(res.Body).Decode(into)
		}
		return res.StatusCode
	}
	fks := "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/flux-system/kustomizations"
	fk := func(name string, labels map[string]string, spec map[string]any) map[string]any {
		meta := map[string]any{"name": name, "namespace": "flux-system"}
		if labels != nil {
			meta["labels"] = labels
		}
		return map[string]any{"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization", "metadata": meta, "spec": spec}
	}
	for name, path := range map[string]string{"factory": "./k8s", "platform": "./platform"} {
		spec := map[string]any{"interval": "10m", "path": path, "prune": true,
			"sourceRef": map[string]any{"kind": "GitRepository", "name": "factory-code"}, "serviceAccountName": "kustomize-controller"}
		if code := asAdmin(http.MethodPost, fks, fk(name, nil, spec), nil); code != http.StatusCreated && code != http.StatusConflict {
			t.Fatalf("admin creates flux-system/%s: %d", name, code)
		}
	}
	for _, suspend := range []bool{true, false} {
		if err := kube.SuspendKustomization(ctx, "flux-system", "factory", suspend); err != nil {
			t.Fatalf("factoryd sets flux-system/factory's suspend %v: %v", suspend, err)
		}
		var got struct{ Spec struct{ Suspend *bool } }
		if code := asAdmin(http.MethodGet, fks+"/factory", nil, &got); code != http.StatusOK || got.Spec.Suspend == nil || *got.Spec.Suspend != suspend {
			t.Fatalf("admin reads flux-system/factory: %d, suspend %v; want %v", code, got.Spec.Suspend, suspend)
		}
	}
	refused("factory's path", http.MethodPatch, apply(fks+"/factory"), fk("factory", nil, map[string]any{"path": "./x"}))
	refused("a label on factory", http.MethodPatch, apply(fks+"/factory"), fk("factory", map[string]string{"x": "y"}, map[string]any{}))
	if err := kube.SuspendKustomization(ctx, "flux-system", "platform", true); err == nil ||
		!(errors.Is(err, ErrForbidden) || strings.Contains(err.Error(), "ValidatingAdmissionPolicy 'factoryd-task-namespaces'")) {
		t.Errorf("factoryd suspends flux-system/platform: %v, want refused", err)
	}
}
