package acceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"

	_ "github.com/jackc/pgx/v5/stdlib" // the store's driver, to read the seeded records back
)

// The receipt's proof from the records (roadmap 66): a full task's records
// seeded in a real store, every receipt fact re-derived from them.

const accReceiptDiff = `diff --git a/order.go b/order.go
@@ -1 +1,2 @@
+calm
+green
diff --git a/order_test.go b/order_test.go
@@ -1 +1,2 @@
+again
`

// accReceiptForgejo serves one merged pull request's state and diff.
type accReceiptForgejo struct{ head, mergeCommit, diff string }

func (f *accReceiptForgejo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/9.diff"):
		_, _ = w.Write([]byte(f.diff))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/9"):
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 9, "state": "closed", "merged": true,
			"merge_commit_sha": f.mergeCommit, "head": map[string]any{"sha": f.head}, "base": map[string]any{"ref": "main"}})
	default:
		http.NotFound(w, r)
	}
}

// accReceiptStore opens the real store on the suite's throwaway Postgres.
func accReceiptStore(t *testing.T) (*factoryd.PGStore, *sql.DB) {
	t.Helper()
	dsn := schematest.DSN(t, "acceptance_receipt")
	s, err := factoryd.OpenStore(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return s, db
}

// accReceiptRunner wires the real Runner over the real store, its
// timeline reading the seeded objects, beside the scripted Forgejo.
func accReceiptRunner(t *testing.T, s *factoryd.PGStore, fj string, objects *accObjects) *factoryd.Runner {
	t.Helper()
	version := policy.Version{ID: 9, Commit: "c9", Tree: accPolicyTree(t)}
	cfg := factoryd.RunnerConfig{
		AgentNamespacePrefix: "factory-agents-", WorkerImage: "reg/worker@sha256:abc",
		SandboxBaseImage: "reg/sandbox-base@sha256:bcd", RegistryHost: "store.test", FactoryNamespace: "factory",
		GatewayURL: "http://gateway.factory.svc.cluster.local:8081", GitBaseURL: "http://forgejo.forgejo.svc.cluster.local:3000",
		Models:       factoryd.ModelLadder{Start: "factory/small", Stronger: "factory/big"},
		AnswerTokens: 8192, HeadroomTokens: 16384, ChecksPoll: time.Millisecond,
		EgressCanary: "example.com:443", EgressDirectIP: "192.0.2.7:443",
		Supervisor:    factoryd.SupervisorConfig{RepetitionEvents: 24},
		Policy:        func() policy.Version { return version },
		PolicyVersion: func(context.Context, int64) (policy.Version, bool, error) { return version, true, nil },
		Leases:        accLeases{}, Gates: &factoryd.GateWatch{}, Workflows: accWorkflows{},
		LastFailure: func(context.Context, string, string) (string, error) { return "", nil },
	}
	r, err := factoryd.NewRunner(cfg, s, accKube{}, &factoryd.Forgejo{BaseURL: fj, BotUser: "factory-agent", BotPassword: "bot-pw"},
		nil, objects, &factoryd.SupervisorModel{GatewayURL: "http://127.0.0.1:1", Token: "gw-token",
			Budget: factoryd.TokenBudget{AnswerTokens: 1000, HeadroomTokens: 8000}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// accSeedReceipt seeds id's full merged records through the production
// writers: task row, settled spend, timeline gates, a decided approval,
// a rung-3 case resolution, judgement criteria verdict rows.
func accSeedReceipt(ctx context.Context, t *testing.T, s *factoryd.PGStore, objects *accObjects, id string, issue int64) {
	t.Helper()
	task := factoryd.Task{ID: id, Status: "merged", Objective: "the receipt proof's card", Repository: "sf/code",
		Kind: "agent", Source: "api", Track: "direct", BudgetEUR: 5, IssueNumber: issue, SHA: "c0-base", PRNumber: 9,
		RequiredCapabilities: []string{"coder"}}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, eur := range []float64{0.75, 1.25} { // the settled sum follows the ledger
		if _, err := s.SettleSpend(ctx, id, eur); err != nil {
			t.Fatal(err)
		}
	}
	tl := &factoryd.Timeline{Objects: objects}
	for _, st := range []struct {
		step   string
		detail map[string]any
	}{
		{"gate_policy", map[string]any{"pr": 9, "commit": "c0", "outcome": "auto", "policy_version": 7, "merge_rule": "auto"}},
		{"gate_reproduction", map[string]any{"pr": 9, "commit": "c0", "skipped": "no reproduction command"}},
		{"gate_policy", map[string]any{"pr": 9, "commit": "c1", "outcome": "approval", "policy_version": 9, "merge_rule": "human"}},
		{"gate_reproduction", map[string]any{"pr": 9, "commit": "c1", "command": "bash check test ./acceptance", "exit": 2}},
	} {
		if err := tl.Append(ctx, id, st.step, st.detail); err != nil {
			t.Fatal(err)
		}
	}
	aid, err := s.OpenApproval(ctx, factoryd.ApprovalRequest{Kind: factoryd.ApprovalMerge, TaskID: id, Project: "sf/code",
		PRNumber: 9, Reason: "a person's word on the merge"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DecideApproval(ctx, aid, factoryd.ApprovalMerge, "approved", "sebastian", "the strict improvement holds"); err != nil {
		t.Fatal(err)
	}
	c, _, err := s.OpenCase(ctx, factoryd.Case{TaskID: id, Repository: "sf/code", Kind: factoryd.CaseSessionCap,
		StateKey: "cap", Problem: "the card spent its sessions"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveCase(ctx, c.ID, factoryd.CaseResolution{By: "sebastian", Rung: 3, Action: factoryd.ActExtend,
		Args: map[string]any{"sessions": 2, "agent_time_s": 3600}, Reason: "the flaky checkout needs two more sessions"},
		nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCriteriaVerdicts(ctx, id, factoryd.CriteriaSourceJudgement, factoryd.FactoryDecider, []factoryd.CriterionVerdict{
		{Criterion: "reads_calm", Verdict: "met", Evidence: "the page keeps its calm with 200 rows on it", Kind: factoryd.CriterionJudgement}}); err != nil {
		t.Fatal(err)
	}
}

// TestReceiptFactsFromRecords asks every receipt fact to equal its
// record's value; the first stored receipt stands, byte for byte.
func TestReceiptFactsFromRecords(t *testing.T) {
	ctx := context.Background()
	s, db := accReceiptStore(t)
	objects := &accObjects{files: map[string][]byte{}}
	r := accReceiptRunner(t, s, serve(t, &accReceiptForgejo{head: "c1-head", mergeCommit: "m9", diff: accReceiptDiff}), objects)
	const id = "rt1"
	accSeedReceipt(ctx, t, s, objects, id, 84)

	rec, err := r.BuildMergeReceipt(ctx, id, 9)
	if err != nil {
		t.Fatal(err)
	}

	// the base SHA and the settled sum, read again from the task's own tables
	base, settled := "", 0.0
	if err := db.QueryRowContext(ctx, `SELECT (SELECT sha FROM tasks WHERE id = $1),
		(SELECT COALESCE(sum(eur), 0)::float8 FROM ledger WHERE task_id = $1)`, id).Scan(&base, &settled); err != nil {
		t.Fatal(err)
	}
	// the newest policy version and the gates, read again from the timeline's own bytes, in their order
	var policyVersion int64
	var gates []factoryd.ReceiptGate
	body, err := objects.Get(ctx, "sessions/"+id+"/timeline.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for line := range bytes.SplitSeq(body, []byte("\n")) {
		var e struct {
			Step          string  `json:"step"`
			Outcome       string  `json:"outcome"`
			Command       string  `json:"command"`
			Exit          *int    `json:"exit"`
			PolicyVersion float64 `json:"policy_version"`
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch e.Step {
		case "gate_policy":
			if e.PolicyVersion > 0 {
				policyVersion = int64(e.PolicyVersion)
			}
			gates = append(gates, factoryd.ReceiptGate{Gate: factoryd.GatePolicy, Verdict: e.Outcome})
		case "gate_reproduction":
			g := factoryd.ReceiptGate{Gate: factoryd.GateReproduction, Command: e.Command, Verdict: "not run"}
			if e.Exit != nil {
				g.Exit, g.Verdict = e.Exit, "failed"
				if *e.Exit == 0 {
					g.Verdict = "passed"
				}
			}
			gates = append(gates, g)
		}
	}
	sum := sha256.Sum256([]byte(accReceiptDiff)) // the diff texts, in the diff's order

	f := rec.Facts
	if f.TaskID != id || f.Repository != "sf/code" || f.Issue != 84 || f.PR != 9 || f.Base != base ||
		f.Head != "c1-head" || f.MergeCommit != "m9" || f.DiffSHA256 != hex.EncodeToString(sum[:]) ||
		f.PolicyVersion != policyVersion || f.CostEUR != settled {
		t.Fatalf("facts = %+v\nwant base %s, head c1-head, merge m9, hash %s, policy %d, cost %v",
			f, base, hex.EncodeToString(sum[:]), policyVersion, settled)
	}
	same(t, "gates", f.Gates, gates)

	if len(f.Decisions) != 2 {
		t.Fatalf("decisions = %+v, want the approval and the case's answer, oldest first", f.Decisions)
	}
	approval, answer := f.Decisions[0], f.Decisions[1]
	if approval.Kind != factoryd.ApprovalMerge || approval.Decision != "approved" || approval.By != "sebastian" ||
		approval.Note != "the strict improvement holds" || approval.At == nil {
		t.Errorf("the approval's decision = %+v", approval)
	}
	if answer.Kind != "case" || answer.Decision != factoryd.ActExtend || answer.By != "sebastian" ||
		answer.Draw != "2 sessions and 60 minutes of agent time" ||
		answer.Note != "the flaky checkout needs two more sessions" || answer.At == nil || answer.At.Before(*approval.At) {
		t.Errorf("the case's decision = %+v", answer)
	}
	if len(rec.Assessments) != 1 || rec.Assessments[0] != (factoryd.ReceiptAssessment{Source: factoryd.CriteriaSourceJudgement,
		Criterion: "reads_calm", Verdict: "met", Evidence: "the page keeps its calm with 200 rows on it"}) {
		t.Errorf("assessments = %+v, want the judgement criterion's row alone", rec.Assessments)
	}

	// the first write stands: a second PutMergeReceipt changes no stored byte
	if err := s.PutMergeReceipt(ctx, id, rec); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := db.QueryRowContext(ctx, `SELECT document FROM merge_receipts WHERE task_id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var built factoryd.MergeReceipt
	a, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(stored), &built); err != nil {
		t.Fatal(err)
	}
	if b, err := json.Marshal(built); err != nil || string(b) != string(a) {
		t.Fatalf("the stored document = %s, want the built receipt it came from (%v)", stored, err)
	}
	overwritten := rec
	overwritten.Facts.CostEUR = 99
	if err := s.PutMergeReceipt(ctx, id, overwritten); err != nil {
		t.Fatal(err)
	}
	var again string
	if err := db.QueryRowContext(ctx, `SELECT document FROM merge_receipts WHERE task_id = $1`, id).Scan(&again); err != nil {
		t.Fatal(err)
	}
	if again != stored {
		t.Fatalf("the stored receipt changed on the second put:\n%s\nbecame\n%s", stored, again)
	}
}

// TestReceiptFactsFromRecordsPastTheDiffBound proves the variant whose
// diff is past ErrDiffTooLong: the hash stays empty, every other fact stands.
func TestReceiptFactsFromRecordsPastTheDiffBound(t *testing.T) {
	ctx := context.Background()
	s, db := accReceiptStore(t)
	objects := &accObjects{files: map[string][]byte{}}
	r := accReceiptRunner(t, s, serve(t, &accReceiptForgejo{head: "c1-head", mergeCommit: "m9",
		diff: strings.Repeat("a", 8<<20+1)}), objects)
	const id = "rt2"
	accSeedReceipt(ctx, t, s, objects, id, 85)

	rec, err := r.BuildMergeReceipt(ctx, id, 9)
	if err != nil {
		t.Fatal(err)
	}
	base, settled := "", 0.0
	if err := db.QueryRowContext(ctx, `SELECT (SELECT sha FROM tasks WHERE id = $1),
		(SELECT COALESCE(sum(eur), 0)::float8 FROM ledger WHERE task_id = $1)`, id).Scan(&base, &settled); err != nil {
		t.Fatal(err)
	}
	f := rec.Facts
	if f.DiffSHA256 != "" {
		t.Errorf("diff_sha256 = %q, want empty past the read bound", f.DiffSHA256)
	}
	if f.TaskID != id || f.Base != base || f.Head != "c1-head" || f.MergeCommit != "m9" ||
		f.PolicyVersion != 9 || f.CostEUR != settled || len(f.Gates) != 4 || len(f.Decisions) != 2 {
		t.Fatalf("facts = %+v, want every other fact of the records", f)
	}
}
