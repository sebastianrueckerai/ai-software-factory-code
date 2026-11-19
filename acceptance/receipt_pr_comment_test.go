package acceptance

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
	"go.temporal.io/sdk/activity"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// receiptMarker marks the receipt's comment on the pull request, as
// factoryd's receiptMarker renders it.
const receiptMarker = "<!-- factoryd receipt t-wip -->"

// The receipt as one comment on the pull request: a merged card's receipt
// is posted exactly once on its pull request, every fact equal to the
// stored record's value and the assessments apart in their labelled
// section, and a rerun of the merge path posts nothing (the first write
// stands).
func TestReceiptCommentOnTheMergedPullRequest(t *testing.T) {
	ctx := context.Background()
	store, err := factoryd.OpenStore(ctx, accDatabase(t, "acceptance_receipt_comment"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	git := &gitForgejo{base: wipBase}
	srv := git.server(t)
	litellm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/key/list"):
			io.WriteString(w, `{"keys":[],"total_pages":1}`)
		case r.Method == http.MethodPost && r.URL.Path == "/key/block":
		default:
			t.Errorf("unexpected litellm call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(litellm.Close)
	objects := &stubObjects{data: map[string][]byte{}}
	forgejo := &factoryd.Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw",
		Seeded: map[string]string{policy.SecretMerge: "9a8b7c6d5e4f30211203f4e5d6c7b8a998877665"}}
	cfg := factoryd.RunnerConfig{
		AgentNamespacePrefix: "factory-agents-", WorkerImage: "reg/worker@sha256:abc", SandboxBaseImage: "reg/sandbox-base@sha256:bcd", RegistryHost: "store.test",
		PullSecret: "forgejo-registry", FactoryNamespace: "factory",
		GatewayURL: "http://factory-gateway.factory.svc.cluster.local:8081", GitBaseURL: "http://forgejo-http.forgejo.svc.cluster.local:3000",
		Models:       factoryd.ModelLadder{Start: "factory/small", Stronger: "factory/big"},
		AnswerTokens: 8192, HeadroomTokens: 16384, ChecksPoll: time.Millisecond,
		EgressCanary: "example.com:443", EgressDirectIP: "192.0.2.7:443",
		Supervisor: factoryd.SupervisorConfig{RepetitionEvents: 24},
		Policy:     wipPolicy,
		PolicyVersion: func(_ context.Context, id int64) (policy.Version, bool, error) {
			v := wipPolicy()
			return v, id == v.ID, nil
		},
		Leases: &acceptanceLeases{}, Gates: &factoryd.GateWatch{},
		LastFailure: func(context.Context, string, string) (string, error) { return "", nil },
		Workflows:   &acceptanceWorkflows{},
	}
	completion := &factoryd.Completion{Store: store, Forgejo: forgejo, LiteLLM: &factoryd.LiteLLMAdmin{BaseURL: litellm.URL, MasterKey: "mk"}}
	runner, err := factoryd.NewRunner(cfg, mergeProject{store}, &acceptanceKube{}, forgejo, completion, objects,
		&factoryd.SupervisorModel{GatewayURL: litellm.URL, Token: "gw-token", Budget: factoryd.TokenBudget{AnswerTokens: 1000, HeadroomTokens: 8000}})
	if err != nil {
		t.Fatal(err)
	}
	// the seeded records: a task row (the base SHA, the booked cost) and the timeline's gate runs
	if err := store.CreateTask(ctx, factoryd.Task{ID: "t-wip", Status: "queued", Objective: "the receipt comment's card", Repository: "demo-app/demo-app",
		Kind: "agent", Source: "api", Track: "direct", BudgetEUR: 5, IssueNumber: 4, SHA: wipBase, RequiredCapabilities: []string{policy.ClassCoder}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SettleSpend(ctx, "t-wip", 2.25); err != nil {
		t.Fatal(err)
	}
	tl := &factoryd.Timeline{Objects: objects}
	for _, st := range []struct {
		step   string
		detail map[string]any
	}{
		{"gate_policy", map[string]any{"pr": 12, "commit": "c3", "outcome": "approval", "policy_version": 7, "merge_rule": "human"}},
		{"gate_reproduction", map[string]any{"pr": 12, "commit": "c3", "command": "bash check test ./acceptance", "exit": 0}},
		{"model_review", map[string]any{"pr": 12, "commit": "c3", "verdict": "approve", "note": "the change is small and calm"}},
	} {
		if err := tl.Append(ctx, "t-wip", st.step, st.detail); err != nil {
			t.Fatal(err)
		}
	}

	session := func(_ context.Context, _ factoryd.SessionInput) (factoryd.SessionResult, error) {
		git.end("c3")
		return factoryd.SessionResult{Outcome: factoryd.OutcomePushed, SessionID: "s-1", Commit: "c3", Summary: "pushed branch factory/t-wip at c3"}, nil
	}

	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := suite.NewTestWorkflowEnvironment()
	runner.RegisterActivities(skipping{env: env, skip: factoryd.ActivityRunSession})
	env.RegisterActivityWithOptions(session, activity.RegisterOptions{Name: factoryd.ActivityRunSession})
	env.RegisterWorkflowWithOptions(factoryd.TaskWorkflow{Config: runner.WorkflowConfig}.Run, workflow.RegisterOptions{Name: "TaskWorkflow"})
	env.RegisterDelayedCallback(func() {
		// the console's approve press: the request the ask opened a person
		// decides, then the signal carries the merge
		as, err := store.ApprovalRequests(ctx, "pending")
		if err != nil {
			t.Errorf("ApprovalRequests: %v", err)
		}
		for _, a := range as {
			if a.TaskID == "t-wip" && a.Kind == factoryd.ApprovalMerge {
				if err := store.DecideApproval(ctx, a.ID, factoryd.ApprovalMerge, "approved", "sebastian", "the change holds"); err != nil {
					t.Errorf("DecideApproval: %v", err)
				}
			}
		}
		env.SignalWorkflow(factoryd.SignalApproved, factoryd.TaskSignal{Author: "sebastian", PR: 12})
	}, 3*time.Hour)

	env.ExecuteWorkflow("TaskWorkflow", "t-wip")
	if !env.IsWorkflowCompleted() {
		t.Fatal("the card's workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("the card's workflow: %v", err)
	}

	// the stored receipt the comment must carry
	rec, ok, err := store.MergeReceipt(ctx, "t-wip")
	if err != nil || !ok {
		t.Fatalf("MergeReceipt = (%v, %v), want the stored receipt", ok, err)
	}
	sum := sha256.Sum256([]byte(accWipDiff))
	if rec.Facts.Base != wipBase || rec.Facts.Head != "c3" || rec.Facts.MergeCommit != "m-12" ||
		rec.Facts.DiffSHA256 != hex.EncodeToString(sum[:]) || rec.Facts.PolicyVersion != 7 || rec.Facts.CostEUR != 2.25 ||
		len(rec.Facts.Gates) != 2 || len(rec.Facts.Decisions) != 1 {
		t.Fatalf("the stored receipt's facts = %+v, want the seeded merge's", rec.Facts)
	}

	// exactly one receipt comment on the pull request, every fact equal to
	// the record's value, the assessments apart and labelled
	receiptBodies := func() []string {
		var out []string
		for _, c := range git.postedComments() {
			if strings.Contains(c.body, receiptMarker) {
				out = append(out, c.body)
			}
		}
		return out
	}
	if bodies := receiptBodies(); len(bodies) != 1 {
		t.Fatalf("%d receipt comments stand on the pull request, want exactly 1", len(bodies))
	}
	body := receiptBodies()[0]
	f := rec.Facts
	facts := []string{"Base: " + f.Base, "Head: " + f.Head, "Merge commit: " + f.MergeCommit,
		"Diff sha256: " + f.DiffSHA256, fmt.Sprintf("Policy version: %d", f.PolicyVersion)}
	for _, g := range f.Gates {
		parts := []string{"Gate " + g.Gate}
		if g.Command != "" {
			parts = append(parts, g.Command)
		}
		if g.Exit != nil {
			parts[len(parts)-1] += fmt.Sprintf(" (exit %d)", *g.Exit)
		}
		facts = append(facts, strings.Join(append(parts, g.Verdict), ": "))
	}
	facts = append(facts, "Cost: "+textutil.EUR(f.CostEUR)+" EUR")
	for _, d := range f.Decisions {
		facts = append(facts, fmt.Sprintf("Decision: %s %s by %s", d.Kind, d.Decision, d.By))
	}
	const section = "Assessments, the model's own, apart from the facts above:"
	at := strings.Index(body, section)
	if at < 0 {
		t.Fatalf("the receipt comment holds no assessments section:\n%s", body)
	}
	for _, fact := range facts {
		if i := strings.Index(body, fact); i < 0 || i > at {
			t.Fatalf("the receipt comment lacks the fact %q among its records' facts:\n%s", fact, body)
		}
	}
	for _, a := range rec.Assessments {
		line := a.Source + ": " + a.Verdict
		if !strings.Contains(body[at:], line) || strings.Contains(body[:at], a.Source+":") {
			t.Fatalf("the assessment %q is not apart and labelled by source:\n%s", line, body)
		}
	}

	// the same merge path again lands on merged-already: the first write
	// stands, no second comment is posted
	aenv := suite.NewTestActivityEnvironment()
	runner.RegisterActivities(aenv)
	var res factoryd.MergeResult
	val, err := aenv.ExecuteActivity(factoryd.ActivityMerge, "t-wip", int64(12), "c3", "sebastian", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := val.Get(&res); err != nil || !res.Merged {
		t.Fatalf("the rerun of the merge path = (%+v, %v), want merged", res, err)
	}
	if bodies := receiptBodies(); len(bodies) != 1 || bodies[0] != body {
		t.Fatalf("after the rerun %d receipt comments stand, want still the one:\n%v", len(bodies), bodies)
	}
}

// The receipt as one comment on the human-merged pull request: a person
// merges the card's pull request in Forgejo, so the decided-merge webhook
// path itself — Intake's prDecided, its MergeReceipt hook — records the
// receipt and posts its comment on the pull request, exactly once, every
// fact equal to the stored record's value and the assessments apart in
// their labelled section. The webhook delivered again — the path run a
// second time — posts nothing: the first write stands.
func TestReceiptCommentOnTheWebhookMergedPullRequest(t *testing.T) {
	ctx := context.Background()
	store, err := factoryd.OpenStore(ctx, accDatabase(t, "acceptance_receipt_webhook"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	git := &gitForgejo{base: wipBase}
	srv := git.server(t)
	litellm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/key/list"):
			io.WriteString(w, `{"keys":[],"total_pages":1}`)
		case r.Method == http.MethodPost && r.URL.Path == "/key/block":
		default:
			t.Errorf("unexpected litellm call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(litellm.Close)
	objects := &stubObjects{data: map[string][]byte{}}
	forgejo := &factoryd.Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw",
		Seeded: map[string]string{policy.SecretMerge: "9a8b7c6d5e4f30211203f4e5d6c7b8a998877665"}}
	cfg := factoryd.RunnerConfig{
		AgentNamespacePrefix: "factory-agents-", WorkerImage: "reg/worker@sha256:abc", SandboxBaseImage: "reg/sandbox-base@sha256:bcd", RegistryHost: "store.test",
		PullSecret: "forgejo-registry", FactoryNamespace: "factory",
		GatewayURL: "http://factory-gateway.factory.svc.cluster.local:8081", GitBaseURL: "http://forgejo-http.forgejo.svc.cluster.local:3000",
		Models:       factoryd.ModelLadder{Start: "factory/small", Stronger: "factory/big"},
		AnswerTokens: 8192, HeadroomTokens: 16384, ChecksPoll: time.Millisecond,
		EgressCanary: "example.com:443", EgressDirectIP: "192.0.2.7:443",
		Supervisor: factoryd.SupervisorConfig{RepetitionEvents: 24},
		Policy:     wipPolicy,
		PolicyVersion: func(_ context.Context, id int64) (policy.Version, bool, error) {
			v := wipPolicy()
			return v, id == v.ID, nil
		},
		Leases: &acceptanceLeases{}, Gates: &factoryd.GateWatch{},
		LastFailure: func(context.Context, string, string) (string, error) { return "", nil },
		Workflows:   &acceptanceWorkflows{},
	}
	completion := &factoryd.Completion{Store: store, Forgejo: forgejo, LiteLLM: &factoryd.LiteLLMAdmin{BaseURL: litellm.URL, MasterKey: "mk"}}
	runner, err := factoryd.NewRunner(cfg, mergeProject{store}, &acceptanceKube{}, forgejo, completion, objects,
		&factoryd.SupervisorModel{GatewayURL: litellm.URL, Token: "gw-token", Budget: factoryd.TokenBudget{AnswerTokens: 1000, HeadroomTokens: 8000}})
	if err != nil {
		t.Fatal(err)
	}
	// the seeded records: a human card waiting on its pull request (the
	// base SHA, the booked cost), the timeline's gate runs and the merge
	// approval a person decided
	if err := store.CreateTask(ctx, factoryd.Task{ID: "t-wip", Status: "waiting-human", Objective: "the webhook receipt comment's card",
		Repository: "demo-app/demo-app", Kind: "human", Source: "api", Track: "direct", BudgetEUR: 5,
		IssueNumber: 4, PRNumber: 12, SHA: wipBase}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SettleSpend(ctx, "t-wip", 2.25); err != nil {
		t.Fatal(err)
	}
	tl := runner.Timeline()
	for _, st := range []struct {
		step   string
		detail map[string]any
	}{
		{"gate_policy", map[string]any{"pr": 12, "commit": "c3", "outcome": "approval", "policy_version": 7, "merge_rule": "human"}},
		{"gate_reproduction", map[string]any{"pr": 12, "commit": "c3", "command": "bash check test ./acceptance", "exit": 0}},
		{"model_review", map[string]any{"pr": 12, "commit": "c3", "verdict": "approve", "note": "the change is small and calm"}},
	} {
		if err := tl.Append(ctx, "t-wip", st.step, st.detail); err != nil {
			t.Fatal(err)
		}
	}
	ask, err := store.OpenApproval(ctx, factoryd.ApprovalRequest{Kind: factoryd.ApprovalMerge, TaskID: "t-wip",
		Project: "demo-app/demo-app", PRNumber: 12, Reason: "the merge waits on a person"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DecideApproval(ctx, ask, factoryd.ApprovalMerge, "approved", "sebastian", "the change holds"); err != nil {
		t.Fatal(err)
	}
	// the person merges the pull request in Forgejo: the branch holds the
	// head they merged, the pull request stands closed by their merge
	git.end("c3")
	git.merged = "c3"

	// the webhook path as main wires it: prDecided over the posting
	// MergeReceipt hook
	const hookSecret = "receipt-webhook-secret"
	in := &factoryd.Intake{Store: store, Forgejo: forgejo, Workflows: &acceptanceWorkflows{}, BotUser: forgejo.BotUser,
		Secret: hookSecret, Policy: func() policy.Tree { return wipPolicy().Tree }, Projects: store, Timeline: tl,
		MergeReceipt: runner.RecordMergeReceiptAndComment}
	deliver := func() {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"action": "closed",
			"pull_request": map[string]any{"number": 12, "merged": true, "merge_commit_sha": "m-12",
				"head": map[string]any{"ref": "factory/t-wip", "sha": "c3", "repo": map[string]any{"full_name": "factory-agent/demo-app"}}},
			"repository": map[string]any{"full_name": "demo-app/demo-app", "default_branch": "main"},
			"sender":     map[string]any{"login": "sebastian"},
		})
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, []byte(hookSecret))
		mac.Write(body)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/forgejo", bytes.NewReader(body))
		req.Header.Set("X-Forgejo-Event-Type", "pull_request")
		req.Header.Set("X-Forgejo-Signature", hex.EncodeToString(mac.Sum(nil)))
		rr := httptest.NewRecorder()
		in.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("the merge webhook: status %d: %s", rr.Code, rr.Body)
		}
	}
	receiptBodies := func() []string {
		var out []string
		for _, c := range git.postedComments() {
			if strings.Contains(c.body, receiptMarker) {
				out = append(out, c.body)
			}
		}
		return out
	}

	deliver()

	// the card ended done with its person's merge
	task, ok, err := store.Task(ctx, "t-wip")
	if err != nil || !ok || task.Status != "done" {
		t.Fatalf("the card after the merge webhook = (%+v, %v, %v), want done", task, ok, err)
	}

	// the stored receipt the comment must carry
	rec, ok, err := store.MergeReceipt(ctx, "t-wip")
	if err != nil || !ok {
		t.Fatalf("MergeReceipt = (%v, %v), want the stored receipt", ok, err)
	}
	sum := sha256.Sum256([]byte(accWipDiff))
	if rec.Facts.Base != wipBase || rec.Facts.Head != "c3" || rec.Facts.MergeCommit != "m-12" ||
		rec.Facts.DiffSHA256 != hex.EncodeToString(sum[:]) || rec.Facts.PolicyVersion != 7 || rec.Facts.CostEUR != 2.25 ||
		len(rec.Facts.Gates) != 2 || len(rec.Facts.Decisions) != 1 {
		t.Fatalf("the stored receipt's facts = %+v, want the seeded human merge's", rec.Facts)
	}

	// exactly one receipt comment on the pull request, every fact equal to
	// the record's value, the assessments apart and labelled
	bodies := receiptBodies()
	if len(bodies) != 1 {
		t.Fatalf("%d receipt comments stand on the pull request, want exactly 1", len(bodies))
	}
	body := bodies[0]
	f := rec.Facts
	facts := []string{"Base: " + f.Base, "Head: " + f.Head, "Merge commit: " + f.MergeCommit,
		"Diff sha256: " + f.DiffSHA256, fmt.Sprintf("Policy version: %d", f.PolicyVersion)}
	for _, g := range f.Gates {
		parts := []string{"Gate " + g.Gate}
		if g.Command != "" {
			parts = append(parts, g.Command)
		}
		if g.Exit != nil {
			parts[len(parts)-1] += fmt.Sprintf(" (exit %d)", *g.Exit)
		}
		facts = append(facts, strings.Join(append(parts, g.Verdict), ": "))
	}
	facts = append(facts, "Cost: "+textutil.EUR(f.CostEUR)+" EUR")
	for _, d := range f.Decisions {
		facts = append(facts, fmt.Sprintf("Decision: %s %s by %s", d.Kind, d.Decision, d.By))
	}
	const section = "Assessments, the model's own, apart from the facts above:"
	at := strings.Index(body, section)
	if at < 0 {
		t.Fatalf("the receipt comment holds no assessments section:\n%s", body)
	}
	for _, fact := range facts {
		if i := strings.Index(body, fact); i < 0 || i > at {
			t.Fatalf("the receipt comment lacks the fact %q among its records' facts:\n%s", fact, body)
		}
	}
	for _, a := range rec.Assessments {
		line := a.Source + ": " + a.Verdict
		if !strings.Contains(body[at:], line) || strings.Contains(body[:at], a.Source+":") {
			t.Fatalf("the assessment %q is not apart and labelled by source:\n%s", line, body)
		}
	}

	// the webhook delivered again runs the same path a second time: the
	// first write stands, no second comment is posted, the receipt and
	// its comment stand untouched
	deliver()
	if again, ok, err := store.MergeReceipt(ctx, "t-wip"); err != nil || !ok || !reflect.DeepEqual(again, rec) {
		t.Fatalf("the receipt after the redelivery = (%+v, %v, %v), want the first receipt\n%+v", again, ok, err, rec)
	}
	if after := receiptBodies(); len(after) != 1 || after[0] != body {
		t.Fatalf("after the redelivery %d receipt comments stand, want still the one:\n%v", len(after), after)
	}
}
