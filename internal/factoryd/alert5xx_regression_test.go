package factoryd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// Regression test for the SigNoz alert "5xx rate above 5 % on service
// factoryd in namespace factory", firing since 2026-10-12T18:58:38Z.
//
// The rule that fired is judged by rolloutAt, which reads the roll
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
			Namespaces: []string{"factory"}, Process: "maintenance", Tracks: maintenanceTracks}}},
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

// The same rule fired again on 2026-10-16T16:04:45Z, at 5.673758865248227:
// exactly 8 of 141 requests on factoryd answered 5xx in its 5-minute
// window. The eight were this webhook's own answers: a filing that fails
// after its alerts row was written answered 500, and SigNoz redelivers a
// 5xx notification — every redelivery re-failed the filing and answered
// 500 again, each one another failed request on factoryd, until the burst
// of retries alone crossed the rule's 5 % share: the alert pipeline
// feeding the rule that watches it. The row, the dedupe key, is written
// first with its task's id, so nothing is lost by accepting the delivery:
// the sweep finishes the filing each minute, as a redelivery would. The
// 5xx stays for a failure before the row (the project read, the insert),
// whose only repair is SigNoz's redelivery.
//
// Reproduce in place:
//
//	cd /workspace/repo && go test ./internal/factoryd/ -run 'TestAlertIntakeHoldsAFailedFilingToNo5xxStorm' -count=1 -v
func TestAlertIntakeHoldsAFailedFilingToNo5xxStorm(t *testing.T) {
	r := newAlertRig(t)
	flaky := &flakyBook{fakeStore: r.store, creates: 8}
	r.in.Store = flaky

	// SigNoz redelivered the notification while it read 5xx; the filing
	// keeps failing through all eight retries, and not one redelivery may
	// answer 5xx again: on the base commit every one of these posts is
	// another 500 span on factoryd — the burst that fired this rule.
	for i := 1; i <= 8; i++ {
		if rr := r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp1", "demo-staging", firedAt, neverEnds)); rr.Code != http.StatusOK {
			t.Fatalf("redelivery %d: status %d, want 200: %s", i, rr.Code, rr.Body)
		}
	}
	rows := r.store.alertRows()
	if len(rows) != 1 || rows[0].TaskID == "" || rows[0].Detail["issue"] != nil {
		t.Fatalf("after the failing redeliveries: %d rows, first %+v; want one row holding its task id, unfiled", len(rows), rows)
	}
	if len(r.store.tasks) != 0 {
		t.Fatalf("after the failing redeliveries: %d tasks, want 0", len(r.store.tasks))
	}

	// the transient failure heals: the sweep, not a 5xx-retry storm,
	// finishes the filing under the row's task id
	flaky.creates = 0
	live := firing("active", map[string]string{"alertname": "5xx rate above 5 %", "namespace": "demo-staging", "project": "demo-app",
		"severity": "critical", "ruleId": "r-1", "threshold.name": "critical"}, "5xx share of the requests over 5 minutes")
	live.Fingerprint, live.StartsAt, live.GeneratorURL = "fp1", at(firedAt), "https://signoz.example/alerts/overview?ruleId=r-1"
	r.in.SigNoz = &fakeAlerts{list: []FiringAlert{live}}
	if err := r.in.SweepSigNoz(context.Background(), at("2026-10-16T16:06:00Z")); err != nil {
		t.Fatal(err)
	}
	rows = r.store.alertRows()
	if len(rows) != 1 {
		t.Fatalf("after the sweep: %d rows, want 1", len(rows))
	}
	id := rows[0].TaskID
	if len(r.store.tasks) != 1 {
		t.Fatalf("after the sweep: %d tasks, want 1", len(r.store.tasks))
	}
	if got := r.store.tasks[id].IssueNumber; got != 9 {
		t.Fatalf("task's issue %d, want 9 (issues 1-8 are the failed deliveries' orphans)", got)
	}
	if !slices.Equal(r.flows.started, []string{id}) {
		t.Fatalf("started %v, want [%s]", r.flows.started, id)
	}
	if got := rows[0].Detail["issue"]; got != int64(9) {
		t.Fatalf("row's issue %v, want 9", got)
	}
}

// The same burst, one step further along the delivery: the failing call
// sat after the row was written AND after its issue was filed — the
// "fired again" comment on the running task's issue, or the resolve's
// comment — where the accept above covered only a row without its issue.
// SigNoz redelivered such a notification just the same: every redelivery
// retried the comment, failed on the same blip, and answered 500 again,
// another burst of 8 in 141 on this rule's share. The row is the
// delivery's dedupe key on these paths too — the next delivery of the
// firing posts the comment again, and the sweep finishes a resolve SigNoz
// no longer lists — so the delivery is accepted and the 5xx stays only
// for a failure before the row was written, whose only repair is SigNoz's
// redelivery.
//
// Reproduce in place:
//
//	cd /workspace/repo && go test ./internal/factoryd/ -run TestAlertIntakeHoldsAFailingCommentToNo5xxStorm -count=1 -v
func TestAlertIntakeHoldsAFailingCommentToNo5xxStorm(t *testing.T) {
	// the fired-again comment: fp1's task runs, the alert fires anew and
	// its comment blips
	r := newAlertRig(t)
	r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp1", "demo-staging", firedAt, neverEnds))
	r.fj.failComments = 8
	for i := 1; i <= 8; i++ {
		if rr := r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp1", "demo-staging", "2026-09-28T21:00:00Z", neverEnds)); rr.Code != http.StatusOK {
			t.Fatalf("redelivery %d: status %d, want 200: %s", i, rr.Code, rr.Body)
		}
	}
	if len(r.fj.issues) != 1 || len(r.store.tasks) != 1 || len(r.fj.comments) != 0 {
		t.Fatalf("through the blip: %d issues, %d tasks, %d comments; want 1, 1, 0", len(r.fj.issues), len(r.store.tasks), len(r.fj.comments))
	}
	// the blip heals: the next delivery of the firing posts the comment
	// once and records its start on the row
	r.fj.failComments = 0
	if rr := r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp1", "demo-staging", "2026-09-28T21:00:00Z", neverEnds)); rr.Code != http.StatusOK {
		t.Fatalf("the delivery after the blip: status %d: %s", rr.Code, rr.Body)
	}
	if want := []string{"#1 factoryd: the alert fired again at 2026-09-28T21:00:00Z.\n\n```\nvalue=14.29\n```"}; !slices.Equal(r.fj.comments, want) {
		t.Fatalf("comments %q, want %q once", r.fj.comments, want)
	}
	if got := r.store.alertRows()[0].Detail["starts_at"]; got != "2026-09-28T21:00:00Z" {
		t.Fatalf("row's starts_at %v, want the new firing's", got)
	}

	// the resolve's comment: its burst is accepted the same way, and the
	// sweep, not a 5xx-retry storm, finishes it
	r2 := newAlertRig(t)
	r2.post(t, "signoz", "hook-pw", sigAlert("firing", "fp2", "demo-staging", firedAt, neverEnds))
	r2.fj.failComments = 8
	for i := 1; i <= 8; i++ {
		if rr := r2.post(t, "signoz", "hook-pw", sigAlert("resolved", "fp2", "demo-staging", firedAt, resolvedAt)); rr.Code != http.StatusOK {
			t.Fatalf("resolve redelivery %d: status %d, want 200: %s", i, rr.Code, rr.Body)
		}
	}
	if len(r2.fj.comments) != 0 {
		t.Fatalf("through the blip: %d comments, want 0", len(r2.fj.comments))
	}
	r2.fj.failComments = 0
	r2.in.SigNoz = &fakeAlerts{}
	if err := r2.in.SweepSigNoz(context.Background(), at(resolvedAt)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"#1 factoryd: resolved at " + resolvedAt}; !slices.Equal(r2.fj.comments, want) {
		t.Fatalf("comments %q, want %q once", r2.fj.comments, want)
	}
	if _, firing := firingSince(r2.store.alertRows()[0]); firing {
		t.Fatal("the swept row still fires")
	}
}
