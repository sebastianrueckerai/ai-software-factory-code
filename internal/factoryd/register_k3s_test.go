package factoryd

import (
	"context"
	"encoding/json"
	"errors"
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
}
