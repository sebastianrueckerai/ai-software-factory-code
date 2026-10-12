package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The SigNoz alert "5xx rate above 5 % on service factoryd in namespace
// factory": the restart-window rule firing in the factory's own namespace.
// cmd/factoryd/main.go built the AlertIntake without Kube and Namespace, so
// judgeRestartWindow called Deployment on the nil reader: the request
// goroutine panicked, net http answered 500, SigNoz retried the delivery,
// and the retries are what pushed factoryd's own 5xx share past the
// threshold. These tests reproduce that shape with a real listener.

// emptyKube is a wired reader that finds no Deployment: every read is
// ErrNotFound, as it is outside the cluster.
type emptyKube struct{}

func (emptyKube) Deployment(context.Context, string, string) (Deployment, error) {
	return Deployment{}, ErrNotFound
}

// serveAlert posts one SigNoz webhook delivery of the restart-window rule to
// intake over a real listener and returns the status the client saw. A panic
// in the handler shows up as a 500 with an empty body, exactly as it does in
// the cluster.
func serveAlert(t *testing.T, intake *AlertIntake, fingerprint, ns string, startsAt time.Time) int {
	t.Helper()
	srv := httptest.NewServer(intake)
	t.Cleanup(srv.Close)
	alert := map[string]any{
		"status": "firing",
		"labels": map[string]string{"alertname": restartWindowAlert, "namespace": ns, "severity": "critical",
			"service.name": "factoryd", "ruleId": "r-1", "threshold.name": "critical"},
		"annotations":  map[string]string{"value": "6.382978723404255", "summary": "share of the service's requests that answered 5xx, in %"},
		"startsAt":     startsAt.UTC().Format(time.RFC3339),
		"endsAt":       "0001-01-01T00:00:00Z",
		"generatorURL": "https://signoz.example/alerts/overview?ruleId=r-1",
		"fingerprint":  fingerprint,
	}
	body, err := json.Marshal(map[string]any{"version": "4", "status": "firing", "receiver": "factoryd", "alerts": []any{alert}})
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

// factoryProject is the code repository's project row owning namespace
// "factory", the factory's own.
func factoryProject() Project {
	return Project{Name: "ai-software-factory-code", Repository: "sf/ai-software-factory-code", Objects: "seed", Agents: "coder",
		File: true, Namespaces: []string{"factory"}, Process: "maintenance", Lanes: maintenanceLanes}
}

// newFactoryRig is the alert rig with the factory's own namespace owned by
// the code repository's project; deployments stands for the roll targets'
// Deployments in it.
func newFactoryRig(t *testing.T, deployments DeploymentReader) *alertRig {
	t.Helper()
	r := newAlertRig(t)
	r.ns.labels["factory"] = "ai-software-factory-code"
	r.in.Projects.(*memProjects).m["ai-software-factory-code"] = factoryProject()
	r.in.Kube, r.in.Namespace = deployments, "factory"
	return r
}

// rollingGateway is a factory-gateway Deployment Flux applied 90 s ago and
// not live yet: its restart window is open-ended, so every recent start of
// this rule lies inside it.
func rollingGateway(now time.Time) *fakeDeployments {
	stamp := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	return &fakeDeployments{json: map[string]string{
		"factoryd":        deploymentJSON(stamp(-time.Hour), stamp(-time.Hour), registryHost+"factory-gateway@"+digestOld),
		"factory-gateway": deploymentJSON(stamp(-90*time.Second), "", registryHost+"factory-gateway@"+digestNew),
	}, reads: map[string]int{}}
}

// Repro: the intake exactly as cmd/factoryd/main.go wired it — no Kube, no
// Namespace — answering three deliveries in a row of this rule firing in the
// factory's own namespace. Before the fix the first one panicked on the nil
// DeploymentReader and net http answered 500 with nothing written; SigNoz
// retried, and those retries are what pushed factoryd's own 5xx share past
// this alert's threshold. Every delivery must answer 200 and file.
func TestReproAlertIntakeWithoutTheWiringAnswersNo5xx(t *testing.T) {
	r := newFactoryRig(t, nil)
	starts := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		if code := serveAlert(t, r.in, fmt.Sprintf("fp-repro-%d", i), "factory", starts); code != http.StatusOK {
			t.Fatalf("delivery %d: status %d, want 200 (a 5xx here is what tripped the alert)", i+1, code)
		}
	}
	if len(r.fj.issues) != 3 || len(r.store.alertRows()) != 3 {
		t.Fatalf("%d issues, %d rows; want the firings filed instead of lost", len(r.fj.issues), len(r.store.alertRows()))
	}
}

// A wired reader that answers "no such Deployment" (ErrNotFound, as outside
// the cluster) judges nothing and files: no panic, no lost alert.
func TestReproAlertIntakeReaderWithoutDeploymentsFiles(t *testing.T) {
	r := newFactoryRig(t, emptyKube{})
	if code := serveAlert(t, r.in, "fp-empty", "factory", time.Now()); code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	if len(r.fj.issues) != 1 {
		t.Fatalf("%d issues, want the firing filed", len(r.fj.issues))
	}
}

// With the reader wired, a firing inside a roll's restart window is noise:
// recorded, not filed, answered 200 — item 127 holds.
func TestReproAlertIntakeWiredReaderStillRecordsNoise(t *testing.T) {
	r := newFactoryRig(t, rollingGateway(time.Now()))
	if code := serveAlert(t, r.in, "fp-window", "factory", time.Now().Add(-60*time.Second)); code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	rows := r.store.alertRows()
	if len(rows) != 1 || rows[0].Verdict != VerdictNoise || rows[0].TaskID != "" {
		t.Fatalf("rows %+v, want one noise row with no task", rows)
	}
	if len(r.fj.issues) != 0 || len(r.store.tasks) != 0 {
		t.Fatalf("%d issues, %d tasks; want none filed inside the window", len(r.fj.issues), len(r.store.tasks))
	}
}

// The wiring itself: every field of AlertIntake whose type is an interface of
// this package must be set in cmd/factoryd/main.go's literal, or the first
// webhook that reaches it runs on a nil interface — the unwired Kube is what
// panicked POST /webhooks/signoz into the 5xx alert on factoryd.
func TestAlertIntakeWiresEveryInterfaceField(t *testing.T) {
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
		if !implementsInterface(t, string(hookGo), ty) {
			continue // a pointer to a concrete struct, a func type, a map, a string
		}
		checked++
		if !strings.Contains(literal, name+":") {
			t.Errorf("AlertIntake declares %s %s but main.go's literal wires no %s", name, ty, name)
		}
	}
	if checked == 0 {
		t.Fatal("AlertIntake declares no interface-typed field: the test checks nothing")
	}
}

// implementsInterface reports whether ty names an interface of this package:
// `type X interface` here, or a defined type whose declaration names one,
// here or in another file of the package. A plain string, a func type, a map
// or a pointer has nothing to wire at risk of a nil panic.
func implementsInterface(t *testing.T, source, ty string) bool {
	t.Helper()
	if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*(\.[A-Za-z][A-Za-z0-9]*)?$`).MatchString(ty) {
		return false
	}
	name := ty
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	if regexp.MustCompile(`(?m)^type ` + regexp.QuoteMeta(name) + ` interface`).MatchString(source) {
		return true
	}
	d := regexp.MustCompile(`(?m)^type ` + regexp.QuoteMeta(name) + ` ([^\n]*)`).FindStringSubmatch(source)
	if d != nil && strings.Contains(d[1], "interface") {
		return true
	}
	if d != nil {
		return false // a foreign named type (a plain string alias): not ours
	}
	return declaresInterfaceInPackage(t, name)
}

// declaresInterfaceInPackage is whether any non-test file of this package
// declares name as an interface.
func declaresInterfaceInPackage(t *testing.T, name string) bool {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	pat := regexp.MustCompile(`(?m)^type ` + regexp.QuoteMeta(name) + `( interface| [^\n]*interface)`)
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
