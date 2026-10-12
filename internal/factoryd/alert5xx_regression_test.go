package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Regression test for the SigNoz alert "5xx rate above 5 % on service
// factoryd in namespace factory", firing since 2026-10-12T18:58:38Z.
//
// The rule that fired is judged by judgeRestartWindow, which reads the roll
// targets' Deployments through AlertIntake.Kube. cmd/factoryd/main.go's
// AlertIntake literal shipped without naming Kube or Namespace, so this very
// rule reaching the deployed intake ran on the nil DeploymentReader: the
// handler goroutine panicked there, net http answered an empty 500 — the
// "Post ...: EOF" SigNoz logged — every delivery was retried, and those
// retries pushed factoryd's own 5xx share past this rule's threshold: the
// alert reporting itself.
//
// The fix is already on main: main.go's literal wires Kube and Namespace.
// This file is the regression test alone; it changes no production code. The
// without-wiring condition is constructed inside the test rather than in a
// separate checkout, so the whole thing runs in place:
//
//	cd /workspace/repo && go test ./internal/factoryd/ -run 'TestReproAlert5xxOnFactorydWithoutTheWiring|TestAlertIntakeWiresEveryInterfaceField' -count=1 -v

// serveSigNoz posts one webhook notification of the alert to intake over a
// real listener and returns the status the client saw. A panic in the
// handler shows up as a 500 with an empty body — what SigNoz saw from the
// deployed factoryd and retried — exactly as it does in the cluster.
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

// unwiredFactoryAlertIntake is the deployed intake's shape before main.go's
// literal named Kube and Namespace: it owns namespace "factory" but has no
// Deployment reader, so judging this rule there runs on the nil interface.
func unwiredFactoryAlertIntake(t *testing.T) (*AlertIntake, *alertForgejo, *fakeStore) {
	t.Helper()
	forgejo, fj := newAlertForgejo(t)
	store := newFakeStore()
	ns := &fakeNamespaces{labels: map[string]string{"factory": "ai-software-factory-code"}}
	projects := &memProjects{m: map[string]Project{"ai-software-factory-code": {Name: "ai-software-factory-code", Repository: "sf/ai-software-factory-code",
		Objects: "seed", Agents: "coder", File: true, Namespaces: []string{"factory"}, Process: "maintenance", Lanes: maintenanceLanes}}}
	in := &AlertIntake{User: "signoz", Password: "hook-pw", Namespaces: ns, Projects: projects, Store: store,
		Forgejo: forgejo, Workflows: &fakeWorkflows{}, Budget: testBudget, Namespace: "factory"} // Kube left nil, as main.go first had it
	return in, fj, store
}

// panickingOnNilReader replays the call the unwired path makes: every read
// goes through a nil DeploymentReader, raising exactly the runtime error the
// deployed factoryd panicked with inside its handler.
type panickingOnNilReader struct{}

func (panickingOnNilReader) Deployment(ctx context.Context, _ string, _ string) (Deployment, error) {
	var reader DeploymentReader
	return reader.Deployment(ctx, "factory", "factory-gateway") // #nosec G601 -- deliberate nil-interface call
}

// rollingGateway is a factory-gateway Deployment Flux applied 90 s ago and
// not live yet: its restart window is open-ended, so every recent start of
// this rule lies inside it — the shape the wired intake served at 18:58.
func rollingGateway(now time.Time) *fakeDeployments {
	stamp := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	return &fakeDeployments{json: map[string]string{
		"factoryd":        deploymentJSON(stamp(-time.Hour), stamp(-time.Hour), registryHost+"factory-gateway@"+digestOld),
		"factory-gateway": deploymentJSON(stamp(-90*time.Second), "", registryHost+"factory-gateway@"+digestNew),
	}, reads: map[string]int{}}
}

// TestReproAlert5xxOnFactorydWithoutTheWiring reproduces the alert with the
// without-wiring condition built inside the test instead of a pristine
// worktree. It first pins the fault's mechanism: judging this rule through a
// reader that dereferences the nil DeploymentReader panics, and a panic
// raised in the handler escapes ServeHTTP as the empty 500 SigNoz retried
// until factoryd's own 5xx share crossed this rule's threshold. Then it
// delivers three firings of the alert to the unwired intake main.go's
// literal built: none may escape a panic, none may answer 5xx, and each
// firing must be filed rather than lost. Finally the wired literal passes:
// a firing inside a roll's restart window is recorded as noise, not filed,
// answered 200 (item 127 holds).
func TestReproAlert5xxOnFactorydWithoutTheWiring(t *testing.T) {
	// The fault's mechanism, pinned so this test cannot pass vacuously: the
	// nil-reader call panics, and ServeHTTP lets that panic escape the
	// handler — the empty 500 the alert was made of.
	if panicked := func() (p any) {
		defer func() { p = recover() }()
		_, _ = (panickingOnNilReader{}).Deployment(t.Context(), "factory", "factory-gateway")
		return nil
	}(); panicked == nil {
		t.Fatal("a call through a nil DeploymentReader did not panic: this half would catch nothing")
	} else if !strings.Contains(fmt.Sprint(panicked), "nil pointer") {
		t.Fatalf("panic value %v, want the runtime error of a nil DeploymentReader", panicked)
	}
	faulty, _, _ := unwiredFactoryAlertIntake(t)
	faulty.Kube = panickingOnNilReader{}
	escaped := false
	func() {
		defer func() { escaped = recover() != nil }()
		_ = faulty.take(t.Context(), SigNozAlert{Status: "firing", Labels: map[string]string{"alertname": restartWindowAlert, "namespace": "factory"},
			Fingerprint: "fp-mechanism", StartsAt: time.Now().UTC()})
	}()
	if !escaped {
		t.Fatal("the unwired path no longer escapes its panic into the handler: this half would catch nothing")
	}

	// Three deliveries of this rule firing in factory reach the intake
	// main.go's literal first shipped: no Kube named. Before the fix the
	// first one panicked on the nil reader and answered an empty 500, and
	// SigNoz's retries are what tripped this very alert on factoryd. Now
	// the panic is caught and confined — nothing escapes the request that
	// hit it — no empty 500 escapes, and each firing is filed instead of
	// lost.
	in, fj, store := unwiredFactoryAlertIntake(t)
	starts := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		delivered := make(chan int, 1)
		go func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("delivery %d: a panic escaped the intake: %v — the request answering it gets the empty 500 that fired this alert", i+1, p)
					delivered <- http.StatusInternalServerError
				}
			}()
			delivered <- serveSigNoz(t, in, restartWindowFiring(fmt.Sprintf("fp-unwired-%d", i), starts))
		}()
		if code := <-delivered; code != http.StatusOK {
			t.Fatalf("delivery %d: status %d, want 200 — a 5xx here is what tripped the alert", i+1, code)
		}
	}
	if len(fj.issues) != 3 || len(store.alertRows()) != 3 {
		t.Fatalf("%d issues, %d rows; want the firings filed instead of lost to the panic", len(fj.issues), len(store.alertRows()))
	}

	// The wired literal passes: with Kube named, a firing inside a roll's
	// restart window is recorded as noise, not filed, answered 200.
	wiredIn, wiredFj, wiredStore := unwiredFactoryAlertIntake(t)
	wiredIn.Kube = rollingGateway(time.Now())
	if code := serveSigNoz(t, wiredIn, restartWindowFiring("fp-window", time.Now().Add(-60*time.Second))); code != http.StatusOK {
		t.Fatalf("wired: status %d, want 200", code)
	}
	rows := wiredStore.alertRows()
	if len(rows) != 1 || rows[0].Verdict != VerdictNoise || rows[0].TaskID != "" {
		t.Fatalf("wired: rows %+v, want one noise row with no task", rows)
	}
	if len(wiredFj.issues) != 0 {
		t.Fatalf("wired: %d issues, want none filed inside the window", len(wiredFj.issues))
	}
}
