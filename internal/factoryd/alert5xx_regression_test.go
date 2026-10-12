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

// Regression test for the SigNoz alert "5xx rate above 5 % on service
// factoryd in namespace factory", firing since 2026-10-12T18:58:38Z.
//
// The rule that fired is restartWindowAlert, the one whose firings in the
// factory's own namespace judgeRestartWindow measures against a roll's
// restart window (item 127). Judging reads the roll targets' Deployments
// through AlertIntake.Kube. cmd/factoryd/main.go's literal first shipped
// without naming Kube or Namespace, so this very rule reaching the deployed
// intake ran on the nil DeploymentReader: the handler goroutine panicked,
// net http answered an empty 500 ("Post http://...: EOF" for SigNoz), every
// delivery was retried, and those retries are what pushed factoryd's own
// 5xx share past this rule's threshold — the alert reporting itself.
//
// The fix is twofold and lives on main already: main.go's literal wires
// Kube and Namespace, and judgeRestartWindow files instead of panicking
// when no reader is wired. Both halves stay under test here; the without-
// wiring condition is constructed inside the test, not checked out from a
// pristine tree, so it runs in place.

// serveSigNoz posts one webhook notification of the alert to intake over a
// real listener and returns the status the client saw. A panic in the
// handler shows up as a 500 with an empty body — the "Post ...: EOF" SigNoz
// logged — exactly as it does in the cluster.
func serveSigNoz(t *testing.T, in *AlertIntake, msg map[string]any) int {
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

// restartWindowFiring is the rule as SigNoz sends it for the factory's own
// namespace, about factoryd: the alert this issue was filed from.
func restartWindowFiring(fingerprint string, startsAt time.Time) map[string]any {
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

// factoryOwnedRig is the alert rig owning namespace "factory" through the
// code repository's project, with deployments standing for the roll targets'
// Deployments in it.
func factoryOwnedRig(t *testing.T, deployments DeploymentReader) *alertRig {
	t.Helper()
	r := newAlertRig(t)
	r.ns.labels["factory"] = "ai-software-factory-code"
	r.in.Projects.(*memProjects).m["ai-software-factory-code"] = Project{
		Name: "ai-software-factory-code", Repository: "sf/ai-software-factory-code", Objects: "seed", Agents: "coder",
		File: true, Namespaces: []string{"factory"}, Process: "maintenance", Lanes: maintenanceLanes}
	r.in.Kube, r.in.Namespace = deployments, "factory"
	return r
}

// panickingOnNilReader is the deployed intake's fault shape: a reader whose
// every call goes through a nil DeploymentReader, so it raises exactly the
// runtime error signozwebhook.go panicked with.
type panickingOnNilReader struct{}

func (panickingOnNilReader) Deployment(ctx context.Context, _ string, _ string) (Deployment, error) {
	var reader DeploymentReader
	return reader.Deployment(ctx, "factory", "factory-gateway") // #nosec G601 -- deliberate nil-interface call
}

// rollingGateway is a factory-gateway Deployment Flux applied 90 s ago and
// not live yet: its restart window is open-ended, so every recent start of
// this rule lies inside it — the shape production served at 18:58.
func rollingGateway(now time.Time) *fakeDeployments {
	stamp := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	return &fakeDeployments{json: map[string]string{
		"factoryd":        deploymentJSON(stamp(-time.Hour), stamp(-time.Hour), registryHost+"factory-gateway@"+digestOld),
		"factory-gateway": deploymentJSON(stamp(-90*time.Second), "", registryHost+"factory-gateway@"+digestNew),
	}, reads: map[string]int{}}
}

// TestReproAlert5xxOnFactorydWithoutTheWiring is the alert itself, with the
// without-wiring condition built inside the test rather than in a separate
// checkout. Three deliveries of this rule firing in factory reach
// judgeRestartWindow on an intake with no Deployment reader — as the
// deployed factoryd had it. Before the guard the first delivery panicked on
// the nil interface and answered an empty 500, and SigNoz's retries kept
// factoryd's own 5xx share above this rule's threshold.
//
// The test asserts both sides: the panic the unwired path raises is caught
// and confined — nothing else escapes but the request that hit it — and the
// shipped intake answers 200 to every delivery and files each firing
// instead of losing it.
func TestReproAlert5xxOnFactorydWithoutTheWiring(t *testing.T) {
	// Side one: the without-wiring condition, constructed in the test. The
	// deployed intake was built without naming Kube, so this rule firing in
	// factory reached judgeRestartWindow on the nil DeploymentReader and
	// panicked there — signozwebhook.go's crash, recovered only by net
	// http's conn.serve, which is why SigNoz saw a 500 with nothing written
	// ("Post ...: EOF") and retried. A reader that forwards to a nil
	// DeploymentReader replays that call; the shipped judging must never
	// reach it again.
	r := factoryOwnedRig(t, panickingOnNilReader{})
	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		_, _ = r.in.judgeRestartWindow(t.Context(), SigNozAlert{}, restartWindowAlert, "factory")
	}()
	if !panicked {
		t.Fatal("the path through a nil DeploymentReader no longer panics: this half of the test would catch nothing")
	}

	// With Kube left nil as main.go's literal first had it, the guard catches
	// the condition before the nil is ever called: the panic stays confined
	// to the unwired shape above, no empty 500 escapes, every delivery
	// answers 200 and each firing files instead of being lost.
	r.in.Kube = nil
	starts := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		if code := serveSigNoz(t, r.in, restartWindowFiring(fmt.Sprintf("fp-unwired-%d", i), starts)); code != http.StatusOK {
			t.Fatalf("delivery %d: status %d, want 200 — a 5xx here is what tripped the alert", i+1, code)
		}
	}
	if len(r.fj.issues) != 3 || len(r.store.alertRows()) != 3 {
		t.Fatalf("%d issues, %d rows; want the firings filed instead of lost to the panic", len(r.fj.issues), len(r.store.alertRows()))
	}

	// Side two: the wired literal passes — with the reader wired, a firing
	// inside a roll's restart window is recorded as noise, not filed,
	// answered 200 (item 127 holds across the guard).
	w := factoryOwnedRig(t, rollingGateway(time.Now()))
	if code := serveSigNoz(t, w.in, restartWindowFiring("fp-window", time.Now().Add(-60*time.Second))); code != http.StatusOK {
		t.Fatalf("wired: status %d, want 200", code)
	}
	rows := w.store.alertRows()
	if len(rows) != 1 || rows[0].Verdict != VerdictNoise || rows[0].TaskID != "" {
		t.Fatalf("wired: rows %+v, want one noise row with no task", rows)
	}
	if len(w.fj.issues) != 0 {
		t.Fatalf("wired: %d issues, want none filed inside the window", len(w.fj.issues))
	}
}

// TestAlertIntakeWiresEveryInterfaceField is the wiring itself: every field
// of AlertIntake whose type is an interface of this package must be named in
// cmd/factoryd/main.go's literal, or the first webhook that reaches it runs
// on a nil one — the unwired Kube is what panicked POST /webhooks/signoz
// into this 5xx alert on factoryd.
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
		if !isPackageInterface(t, string(hookGo), ty) {
			continue // a string, a func, a map, a pointer to a concrete struct
		}
		checked++
		if !strings.Contains(literal, name+":") {
			t.Errorf("AlertIntake declares %s %s but main.go's literal wires no %s: the webhook reaching it runs on the nil", name, ty, name)
		}
	}
	if checked == 0 {
		t.Fatal("AlertIntake declares no interface-typed field: the check names nothing")
	}
}

// isPackageInterface reports whether ty names an interface declared by a
// non-test file of this package.
func isPackageInterface(t *testing.T, source, ty string) bool {
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
