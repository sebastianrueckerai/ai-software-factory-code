package factoryd

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Reproduction of the SigNoz alert "5xx rate above 5 % on service factoryd in
// namespace factory".
//
// The rule that fired is k8s/alerts/5xx-rate.json, the one restartWindowAlert
// names. Its firings in the factory's own namespace are the only ones factoryd
// judges against a roll's restart window (item 127), and judging reads the roll
// targets' Deployments through AlertIntake.Kube. At c4c41b1 cmd/factoryd/main.go
// built the AlertIntake without naming Kube or Namespace, so this very rule
// firing in factory reached judgeRestartWindow with no Deployment reader: the
// handler goroutine panicked on the nil interface, net http answered an empty
// 500 ("Post http://...: EOF" for SigNoz), every delivery was retried, and
// those retries are what pushed factoryd's own 5xx share past this rule's
// threshold — the alert reporting itself.
//
// TestReproAlert5xxOnFactorydWithoutTheWiring reproduces that: the intake as
// main.go's literal left it, over a real listener. TestReproAlert5xxOnFactoryd-
// ProductionShape is the same alert read the way production reads it, through
// the client main.go builds (factoryd.NewInClusterKube) against a stub API
// server, which is how the fix must behave once the wiring is in place.

// apiStub stands in for the Kubernetes API server: it answers the namespace's
// project label, each roll target Deployment with deploymentJSON[name] or 404
// when it has none, and counts the Deployment reads so a test can see whether
// the intake judged at all.
type apiStub struct {
	mu        sync.Mutex
	deploymap map[string]string // Deployment name -> its JSON
	reads     map[string]int
}

func newAPIStub(t *testing.T, deployments map[string]string) (*apiStub, *InClusterKube) {
	t.Helper()
	api := &apiStub{deploymap: deployments, reads: map[string]int{}}
	srv := httptest.NewTLSServer(http.HandlerFunc(api.serve))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	oldDir := saDir
	saDir = dir
	t.Cleanup(func() { saDir = oldDir })

	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)
	kube, err := NewInClusterKube()
	if err != nil {
		t.Fatal(err)
	}
	return api, kube
}

func (a *apiStub) serve(w http.ResponseWriter, r *http.Request) {
	if ns, ok := strings.CutPrefix(r.URL.Path, "/api/v1/namespaces/"); ok && !strings.Contains(ns, "/") {
		fmt.Fprint(w, `{"metadata":{"labels":{"`+ProjectLabel+`":"ai-software-factory-code"}}}`)
		return
	}
	name := ""
	if rest, ok := strings.CutPrefix(r.URL.Path, "/apis/apps/v1/namespaces/"); ok {
		parts := strings.Split(rest, "/")
		if len(parts) == 3 && parts[1] == "deployments" && parts[2] != "" {
			name = parts[2]
		}
	}
	a.mu.Lock()
	body, seen := "", false
	if name != "" {
		a.reads[name]++
		body, seen = a.deploymap[name]
	}
	a.mu.Unlock()
	if !seen {
		http.Error(w, `{"kind":"Status","code":404}`, http.StatusNotFound)
		return
	}
	fmt.Fprint(w, body)
}

func (a *apiStub) readCount(name string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reads[name]
}

// dumpReads is the read counts, for a failure message.
func (a *apiStub) dumpReads() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	names := make([]string, 0, len(a.reads))
	for n := range a.reads {
		names = append(names, n)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s:%d ", n, a.reads[n])
	}
	return b.String()
}

// newAlertProdRig is the alert intake as cmd/factoryd/main.go wires it: the
// same in-cluster client reading the namespaces' project labels and, once the
// literal names them, the roll targets' Deployments. Without wired it is
// main.go's literal at c4c41b1, where neither Kube nor Namespace was named.
func newAlertProdRig(t *testing.T, kube *InClusterKube, wired bool) *alertRig {
	t.Helper()
	forgejo, fj := newAlertForgejo(t)
	store := newFakeStore()
	projects := &memProjects{m: map[string]Project{"ai-software-factory-code": {
		Name: "ai-software-factory-code", Repository: "sf/ai-software-factory-code", Objects: "seed", Agents: "coder",
		File: true, Namespaces: []string{"factory"}, Process: "maintenance", Lanes: maintenanceLanes}}}
	flows := &fakeWorkflows{}
	in := &AlertIntake{User: "signoz", Password: "hook-pw",
		Namespaces: kube, Projects: projects, Store: store, Forgejo: forgejo, Workflows: flows, Budget: testBudget,
		LinkHosts: func() []string { return nil }}
	if wired {
		in.Kube, in.Namespace = kube, kube.Namespace()
		if in.Namespace == "" {
			in.Namespace = "factory" // the pod's own namespace, read from the API server
		}
	}
	return &alertRig{in: in, store: store, fj: fj, ns: &fakeNamespaces{}, flows: flows}
}

// postSigNozDelivery posts one webhook notification through the intake over a
// real listener and returns the status the client saw. A panic in the handler
// shows up as a 500 with an empty body, exactly as it does in the cluster.
func postSigNozDelivery(t *testing.T, in *AlertIntake, msg map[string]any) int {
	t.Helper()
	srv := httptest.NewServer(in)
	t.Cleanup(srv.Close)
	body, err := json.Marshal(map[string]any{"version": "4", "status": "firing", "receiver": "factoryd", "alerts": []any{msg}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("signoz", "hook-pw")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("the delivery never got an answer: %v", err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

// firingOf is the restart-window rule as SigNoz sends it for the factory's own
// namespace, about factoryd: the alert this issue was filed from.
func firingOf(fingerprint string, startsAt time.Time) map[string]any {
	return map[string]any{
		"status": "firing",
		"labels": map[string]string{"alertname": restartWindowAlert, "namespace": "factory", "severity": "critical",
			"service.name": "factoryd", "ruleId": "r-1", "threshold.name": "critical"},
		"annotations":  map[string]string{"value": "6.382978723404255", "summary": "share of the service's requests that answered 5xx, in %"},
		"startsAt":     startsAt.UTC().Format(time.RFC3339),
		"endsAt":       "0001-01-01T00:00:00Z",
		"generatorURL": "https://signoz.example/alerts/overview?ruleId=r-1",
		"fingerprint":  fingerprint,
	}
}

// TestReproAlert5xxOnFactorydWithoutTheWiring is the alert itself: three
// deliveries of this rule firing in factory against the intake exactly as
// main.go's literal left it at c4c41b1 — no reader named, no namespace named.
// Before the fix the first panicked on the nil DeploymentReader and answered
// an empty 500, and SigNoz's retries kept factoryd's own 5xx share above this
// rule's threshold. Every delivery must answer 200 and file.
func TestReproAlert5xxOnFactorydWithoutTheWiring(t *testing.T) {
	_, kube := newAPIStub(t, nil)
	r := newAlertProdRig(t, kube, false)
	starts := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		if code := postSigNozDelivery(t, r.in, firingOf(fmt.Sprintf("fp-unwired-%d", i), starts)); code != http.StatusOK {
			t.Fatalf("delivery %d: status %d, want 200 — a 5xx here is what tripped the alert", i+1, code)
		}
	}
	if len(r.fj.issues) != 3 || len(r.store.alertRows()) != 3 {
		t.Fatalf("%d issues, %d rows; want the firings filed instead of lost to the panic", len(r.fj.issues), len(r.store.alertRows()))
	}
}

// TestReproAlert5xxOnFactorydProductionShape is the same alert read the way
// production reads it: the client main.go builds, pointed at an API server
// whose roll target Deployments exist but sit outside any restart window. The
// firing files, answered 200, and the intake really did judge both roll
// targets through the client rather than skip the check.
func TestReproAlert5xxOnFactorydProductionShape(t *testing.T) {
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	api, kube := newAPIStub(t, map[string]string{
		"factoryd":        deploymentJSON(at(-48*time.Hour), at(-48*time.Hour), registryHost+"factory-gateway@"+digestOld),
		"factory-gateway": deploymentJSON(at(-48*time.Hour), at(-48*time.Hour), registryHost+"factory-gateway@"+digestNew),
	})
	r := newAlertProdRig(t, kube, true)
	if code := postSigNozDelivery(t, r.in, firingOf("fp-shape", now)); code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	if len(r.fj.issues) != 1 {
		t.Fatalf("%d issues, want the firing filed", len(r.fj.issues))
	}
	if api.readCount("factoryd") == 0 || api.readCount("factory-gateway") == 0 {
		t.Fatalf("the wired intake read [%s]: it must check both roll targets' Deployments", api.dumpReads())
	}
}

// TestReproAlert5xxInsideARollsWindowStillNoise holds item 127 across the
// nil-reader guard: with the wiring, a firing inside a roll's restart window
// is recorded as noise and not filed, answered 200.
func TestReproAlert5xxInsideARollsWindowStillNoise(t *testing.T) {
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	// Flux applied factory-gateway's spec 90 s ago and it is not live yet:
	// an open-ended window, so this start lies inside it.
	api, kube := newAPIStub(t, map[string]string{
		"factoryd":        deploymentJSON(at(-time.Hour), at(-time.Hour), registryHost+"factory-gateway@"+digestOld),
		"factory-gateway": deploymentJSON(at(-90*time.Second), "", registryHost+"factory-gateway@"+digestNew),
	})
	r := newAlertProdRig(t, kube, true)
	if code := postSigNozDelivery(t, r.in, firingOf("fp-window", now.Add(-60*time.Second))); code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	rows := r.store.alertRows()
	if len(rows) != 1 || rows[0].Verdict != VerdictNoise || rows[0].TaskID != "" {
		t.Fatalf("rows %+v, want one noise row with no task", rows)
	}
	if len(r.fj.issues) != 0 {
		t.Fatalf("%d issues, want none filed inside the window", len(r.fj.issues))
	}
	if api.readCount("factory-gateway") == 0 {
		t.Fatalf("factory-gateway read %d times: the window was never read from the API server", api.readCount("factory-gateway"))
	}
}

// TestAlertIntakeWiringIsComplete names the fault directly: main.go's
// AlertIntake literal must wire every field of the intake whose type is an
// interface of this package, or the first webhook that reaches it runs on a
// nil one — the unwired Kube is what panicked POST /webhooks/signoz into the
// 5xx alert on factoryd.
func TestAlertIntakeWiringIsComplete(t *testing.T) {
	mainGo, err := os.ReadFile("../../cmd/factoryd/main.go")
	if err != nil {
		t.Fatal(err)
	}
	hookGo, err := os.ReadFile("signozwebhook.go")
	if err != nil {
		t.Fatal(err)
	}
	structBlock := regexp.MustCompile(`(?s)type AlertIntake struct \{.*?\n\}`).FindString(string(hookGo))
	if structBlock == "" {
		t.Fatal("signozwebhook.go declares no AlertIntake struct the pattern finds")
	}
	i := strings.Index(string(mainGo), "&factoryd.AlertIntake{")
	if i < 0 {
		t.Fatal("main.go builds no factoryd.AlertIntake")
	}
	literal := string(mainGo)[i:]
	if j := strings.Index(literal, "})"); j >= 0 {
		literal = literal[:j]
	}
	checked := 0
	for _, line := range strings.Split(structBlock, "\n") {
		f := regexp.MustCompile(`^\t([A-Za-z][A-Za-z0-9]*)\s+(\S+)`).FindStringSubmatch(line)
		if f == nil {
			continue
		}
		name, ty := f[1], f[2]
		if !isInterfaceOfType(t, ty) {
			continue // a string, a func, a map, a pointer to a concrete struct
		}
		checked++
		if !strings.Contains(literal, name+":") {
			t.Errorf("AlertIntake declares %s %s but main.go's literal wires no %s: the webhook reaching it panics on the nil", name, ty, name)
		}
	}
	if checked == 0 {
		t.Fatal("AlertIntake declares no interface-typed field: the check names nothing")
	}
}

// isInterfaceOfType reports whether ty names an interface declared by a
// non-test file of this package.
func isInterfaceOfType(t *testing.T, ty string) bool {
	t.Helper()
	if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`).MatchString(ty) {
		return false
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	pat := regexp.MustCompile(`(?m)^type ` + regexp.QuoteMeta(ty) + `( interface| [^\n]*interface)`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if pat.Match(b) {
			return true
		}
	}
	return false
}
