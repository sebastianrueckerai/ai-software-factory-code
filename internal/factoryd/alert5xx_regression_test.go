package factoryd

import (
	"encoding/json"
	"io"
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
// AlertIntake literal shipped without naming Kube, so this very rule reaching
// the deployed intake ran on the nil DeploymentReader: the handler goroutine
// panicked there, net/http answered an empty 500 — the "Post ...: EOF" SigNoz
// logged — every delivery was retried, and those retries pushed factoryd's own
// 5xx share past this rule's threshold: the alert reporting itself.
//
// The fix is twofold: main.go names Kube and Namespace on the intake, and the
// delivery path itself answers a plain 500 with an error body when that reader
// is missing, so the missing-wiring class cannot ship as a panic again. This
// test delivers the real webhook notification to the real handler over a real
// listener with Kube left nil, exactly what the deployed factoryd received at
// 18:58. Without the guard the panic escapes ServeHTTP, net/http tears the
// connection down mid-response and the client sees no answer at all; with it
// the delivery answers 500 with a non-empty body and nothing panics.
//
// Reproduce in place:
//
//	cd /workspace/repo && go test ./internal/factoryd/ -run 'TestReproAlert5xxOnFactorydWithoutTheWiring|TestAlertIntakeWiresEveryInterfaceField' -count=1 -v

// TestReproAlert5xxOnFactorydWithoutTheWiring replays the firing that set off
// the alert against the unwired intake: one valid notification of the
// restart-window rule posted to POST /webhooks/signoz served by an
// AlertIntake whose literal names everything main.go's first one did except
// Kube. It asserts the delivery answers 500 with a non-empty body and that no
// panic escapes the handler.
func TestReproAlert5xxOnFactorydWithoutTheWiring(t *testing.T) {
	forgejo, _ := newAlertForgejo(t)
	in := &AlertIntake{User: "signoz", Password: "hook-pw",
		Namespaces: &fakeNamespaces{labels: map[string]string{"factory": "ai-software-factory-code"}},
		Projects: &memProjects{m: map[string]Project{"ai-software-factory-code": {Name: "ai-software-factory-code",
			Repository: "sf/ai-software-factory-code", Objects: "seed", Agents: "coder", File: true,
			Namespaces: []string{"factory"}, Process: "maintenance", Lanes: maintenanceLanes}}},
		Store: newFakeStore(), Forgejo: forgejo, Workflows: &fakeWorkflows{}, Budget: testBudget,
		Namespace: "factory"} // Kube left nil, as main.go first had it

	srv := httptest.NewServer(in)
	t.Cleanup(srv.Close)

	starts := time.Now().UTC().Truncate(time.Second)
	msg := map[string]any{
		"status": "firing",
		"labels": map[string]string{"alertname": restartWindowAlert, "namespace": "factory", "project": "ai-software-factory-code",
			"severity": "critical", "service.name": "factoryd", "ruleId": "01a1307c-40ee-7152-ad0c-17e84133caba", "threshold.name": "critical"},
		"annotations":  map[string]string{"value": "6.382978723404255", "summary": "share of the service's requests that answered 5xx, in %"},
		"startsAt":     starts.Format(time.RFC3339),
		"endsAt":       neverEnds,
		"generatorURL": "https://signoz.example/alerts/overview?ruleId=01a1307c-40ee-7152-ad0c-17e84133caba",
		"fingerprint":  "fp-repro-5xx",
	}
	body, err := json.Marshal(map[string]any{"version": "4", "status": "firing", "receiver": "factoryd", "alerts": []any{msg}})
	if err != nil {
		t.Fatal(err)
	}

	// A panic raised inside ServeHTTP escapes to net/http, which tears the
	// connection down and writes no response: the client errors out instead
	// of getting an answer. Recovering here keeps the test honest about
	// which of the two happened — base fails on the escape, head on the body.
	var status int
	var payload string
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("a panic escaped the delivery path: %v — the request answering it gets the empty 500 that fired this alert", p)
			}
		}()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/webhooks/signoz", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth("signoz", "hook-pw")
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("the delivery never got an answer: %v — the handler panicked and net/http dropped the connection, the empty 500 SigNoz retried into this alert", err)
		}
		defer res.Body.Close()
		status = res.StatusCode
		got, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		payload = string(got)
	}()

	if status != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: the unwired intake must not file an alert it cannot judge, and must not panic", status)
	}
	if strings.TrimSpace(payload) == "" {
		t.Fatal("the 500 has an empty body: that is the response SigNoz could not read and retried until factoryd's own 5xx share crossed this rule's threshold")
	}
}
