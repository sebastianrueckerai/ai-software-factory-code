package factoryd

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
)

// TestRegistrationClusterOnAnAPIServer runs registration's cluster part as
// factoryd's service account against a real API server carrying
// k8s/factoryd.yaml's RBAC and admission policy (and stand-in Flux CRDs):
// FACTORYD_APISERVER is host:port, FACTORYD_APISERVER_SA a directory with
// the account's token and the CA.
func TestRegistrationClusterOnAnAPIServer(t *testing.T) {
	addr, dir := os.Getenv("FACTORYD_APISERVER"), os.Getenv("FACTORYD_APISERVER_SA")
	if addr == "" || dir == "" {
		t.Skip("FACTORYD_APISERVER and FACTORYD_APISERVER_SA not set")
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
		GitURL: "https://git.aisoftwarefactory.world", RegistryHost: "git.aisoftwarefactory.world", CIUser: "factory-ci"}
	p := demoProject()
	p.PullToken = "pull-tok"
	logs := captureLog(t)
	ctx := context.Background()
	run := &reconcileRun{r: reg, p: p}
	run.cluster(ctx)
	if len(run.errs) != 0 {
		t.Fatalf("first run: %v\n%s", run.errs, logs)
	}
	logs.Reset()
	run = &reconcileRun{r: reg, p: p}
	run.cluster(ctx)
	if len(run.errs) != 0 {
		t.Fatalf("second run: %v", run.errs)
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if !strings.HasSuffix(line, ": no change") {
			t.Errorf("second run: %s", line)
		}
	}
	if _, err := kube.Deployments(ctx, "demo-staging"); err != nil {
		t.Fatalf("deployments: %v", err)
	}
	// a quota change reaches the ResourceQuota
	p.Quota.CPU = "3"
	logs.Reset()
	run = &reconcileRun{r: reg, p: p}
	run.cluster(ctx)
	if len(run.errs) != 0 || !strings.Contains(logs.String(), "demo-staging resourcequota: updated") {
		t.Fatalf("quota change: %v\n%s", run.errs, logs)
	}
}
