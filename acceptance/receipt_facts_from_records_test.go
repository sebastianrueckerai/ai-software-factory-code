package acceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"

	_ "github.com/jackc/pgx/v5/stdlib" // the store's driver, to read the seeded records back
)

// The receipt's proof from the records (roadmap 66): a full task's
// records seeded in a real store beside the suite's throwaway Postgres —
// the task row, the merged pull request's state and diff records, a
// timeline of gate steps, an approval request a person decided, a rung-3
// case resolution and the judgement criteria's verdict rows — and every
// receipt fact derived again, straight from the record each one came
// from, so anyone reading the records alone rebuilds each fact's value.

// accReceiptDiff is the pull request's diff record: three files' texts,
// in the order the diff carries them.
const accReceiptDiff = `diff --git a/order.go b/order.go
@@ -1 +1,2 @@
+calm
+green
diff --git a/order_test.go b/order_test.go
@@ -1 +1,2 @@
+again
diff --git a/readme.md b/readme.md
@@ -1 +1 @@
+the receipt's own words
`

// accReceiptForgejo serves one merged pull request's records: its state
// (the head, the merge commit) and its whole diff.
type accReceiptForgejo struct{ head, mergeCommit, diff string }

func (f *accReceiptForgejo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/9.diff"):
		_, _ = io.WriteString(w, f.diff)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/9"):
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 9, "state": "closed", "merged": true,
			"merge_commit_sha": f.mergeCommit, "head": map[string]any{"sha": f.head}, "base": map[string]any{"ref": "main"}})
	default:
		http.NotFound(w, r)
	}
}

// accReceiptStore opens the real store on the suite's throwaway Postgres,
// and a plain database handle on the same DSN, to read the seeded
// records back as anyone can.
func accReceiptStore(t *testing.T, name string) (*factoryd.PGStore, *sql.DB) {
	t.Helper()
	dsn := schematest.DSN(t, name)
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
// writers: the task row (the base SHA, the repository, the issue, the
// settled cost), the timeline's gate steps, a decided approval, a rung-3
// case resolution and the judgement criteria's verdict rows.
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

// accReceiptRecord reads the pull request's record at path as anyone
// can: its own bytes over the forge's own API.
func accReceiptRecord(t *testing.T, fj, path string) []byte {
	t.Helper()
	res, err := http.Get(fj + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", path, res.Status)
	}
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// accReceiptState reads the pull request's state record as anyone can
// and answers the head and the merge commit it names.
func accReceiptState(t *testing.T, fj string) (head, mergeCommit string) {
	t.Helper()
	var pr struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		MergeCommit string `json:"merge_commit_sha"`
	}
	if err := json.Unmarshal(accReceiptRecord(t, fj, "/api/v1/repos/sf/code/pulls/9"), &pr); err != nil {
		t.Fatal(err)
	}
	return pr.Head.SHA, pr.MergeCommit
}

// accReceiptDiffSHA256 reads the pull request's diff record as anyone
// can and answers sha256 over its file texts, whole, in the order the
// diff carries them: byte for byte the diff the gate read.
func accReceiptDiffSHA256(t *testing.T, fj string) string {
	t.Helper()
	diff := string(accReceiptRecord(t, fj, "/api/v1/repos/sf/code/pulls/9.diff"))
	h := sha256.New()
	for rest := diff; rest != ""; { // a file's text ends where the next "diff --git" line starts
		end := len(rest)
		if i := strings.Index(rest[1:], "\ndiff --git "); i >= 0 {
			end = i + 2
		}
		h.Write([]byte(rest[:end]))
		rest = rest[end:]
	}
	return hex.EncodeToString(h.Sum(nil))
}

// accReceiptTaskRow reads the task's own row as anyone can and answers
// the base SHA, the repository, the issue and the settled ledger sum.
func accReceiptTaskRow(ctx context.Context, t *testing.T, db *sql.DB, id string) (base, repository string, issue int64, settled float64) {
	t.Helper()
	if err := db.QueryRowContext(ctx, `SELECT sha, repository, issue_number,
		(SELECT COALESCE(sum(eur), 0)::float8 FROM ledger WHERE task_id = $1) FROM tasks WHERE id = $1`, id).
		Scan(&base, &repository, &issue, &settled); err != nil {
		t.Fatal(err)
	}
	return base, repository, issue, settled
}

// accReceiptTimeline reads the task's timeline record as anyone can and
// answers the newest policy version its gate_policy steps name and every
// gate in the order the record carries them, each with its command, its
// exit where it ran one, and its verdict.
func accReceiptTimeline(t *testing.T, objects *accObjects, id string) (policyVersion int64, gates []factoryd.ReceiptGate) {
	t.Helper()
	body, err := objects.Get(context.Background(), "sessions/"+id+"/timeline.jsonl")
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
	return policyVersion, gates
}

// accExit answers a pointer at n, an exit code's shape in a gate.
func accExit(n int) *int { return &n }

// accReceiptJSON answers v as its document bytes, a receipt's own
// canonical form: the level at which a stored receipt is one receipt.
func accReceiptJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// accReceiptDecisions reads the task's decided approval requests and its
// case resolutions as anyone can and answers the human decisions they
// record — the approval a person decided, the case the person answered
// on rung 3 — each with its kind, its decision and its by, the approval
// in the records' own order, the case's draw said in the factory's own
// words from the resolution's own args.
func accReceiptDecisions(ctx context.Context, t *testing.T, s *factoryd.PGStore, id string) []factoryd.ReceiptDecision {
	t.Helper()
	var out []factoryd.ReceiptDecision
	for _, status := range []string{"approved", "denied"} {
		as, err := s.ApprovalRequests(ctx, status)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range as {
			if a.TaskID != id {
				continue
			}
			out = append(out, factoryd.ReceiptDecision{Kind: a.Kind, Decision: a.Status, By: a.DecidedBy,
				Note: a.DecisionNote, At: a.DecidedAt})
		}
	}
	cs, err := s.CasesOf(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		res := c.Resolution
		if res == nil || res.Rung != 3 {
			continue
		}
		sessions, _ := res.Args["sessions"].(float64)
		seconds, _ := res.Args["agent_time_s"].(float64)
		out = append(out, factoryd.ReceiptDecision{Kind: "case", Decision: res.Action, By: res.By,
			Draw: fmt.Sprintf("%d sessions and %d minutes of agent time", int(sessions), int(seconds)/60),
			Note: res.Reason, At: c.ResolvedAt})
	}
	return out
}

// accReceiptAssessments reads the task's verdict rows as anyone can and
// answers the judgement criteria's assessments, the model's own, that
// the receipt carries beside its facts.
func accReceiptAssessments(ctx context.Context, t *testing.T, s *factoryd.PGStore, id string) []factoryd.ReceiptAssessment {
	t.Helper()
	rows, err := s.CriteriaVerdicts(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var out []factoryd.ReceiptAssessment
	for _, v := range rows {
		if v.Source != factoryd.CriteriaSourceJudgement {
			continue
		}
		out = append(out, factoryd.ReceiptAssessment{Source: v.Source, Criterion: v.Criterion,
			Verdict: v.Verdict, Evidence: v.Evidence})
	}
	return out
}

// TestReceiptFactsFromRecords asks every receipt fact to equal the value
// read straight from its record, and the first stored receipt to stand,
// byte for byte.
func TestReceiptFactsFromRecords(t *testing.T) {
	ctx := context.Background()
	fj := serve(t, &accReceiptForgejo{head: "c1-head", mergeCommit: "m9", diff: accReceiptDiff})
	s, db := accReceiptStore(t, "acceptance_receipt")
	objects := &accObjects{files: map[string][]byte{}}
	r := accReceiptRunner(t, s, fj, objects)
	const id = "rt1"
	accSeedReceipt(ctx, t, s, objects, id, 84)

	rec, err := r.BuildMergeReceipt(ctx, id, 9)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Version != factoryd.MergeReceiptVersion {
		t.Errorf("version = %d, want %d", rec.Version, factoryd.MergeReceiptVersion)
	}
	f := rec.Facts

	// the task's own row: the task, the repository, the issue, the pull
	// request, the base SHA, and the cost as the settled ledger sum
	base, repository, issue, settled := accReceiptTaskRow(ctx, t, db, id)
	if f.TaskID != id || f.Repository != repository || f.Issue != issue || f.PR != 9 {
		t.Errorf("the task's facts = %s in %s, issue %d, pr %d; want the row's %s in %s, issue %d, pr 9",
			f.TaskID, f.Repository, f.Issue, f.PR, id, repository, issue)
	}
	if f.Base != base {
		t.Errorf("base = %q, want the task row's sha %q", f.Base, base)
	}
	if f.CostEUR != settled {
		t.Errorf("cost_eur = %v, want the settled ledger sum %v", f.CostEUR, settled)
	}

	// the pull request's records: the head and the merge commit its state
	// names, and sha256 over its diff's file texts in the diff's order
	head, mergeCommit := accReceiptState(t, fj)
	if f.Head != head {
		t.Errorf("head = %q, want the pull request record's %q", f.Head, head)
	}
	if f.MergeCommit != mergeCommit {
		t.Errorf("merge_commit = %q, want the pull request record's %q", f.MergeCommit, mergeCommit)
	}
	if want := accReceiptDiffSHA256(t, fj); f.DiffSHA256 != want {
		t.Errorf("diff_sha256 = %q, want sha256 over the diff record's file texts %q", f.DiffSHA256, want)
	}

	// the timeline's own bytes: the newest policy version, every gate in
	// timeline order with its command and its exit
	policyVersion, gates := accReceiptTimeline(t, objects, id)
	if f.PolicyVersion != policyVersion {
		t.Errorf("policy_version = %d, want the timeline's newest %d", f.PolicyVersion, policyVersion)
	}
	// name the gates the record carries, so the equality below is against
	// known values, not the builder against itself
	same(t, "the timeline's gates", gates, []factoryd.ReceiptGate{
		{Gate: factoryd.GatePolicy, Verdict: "auto"},
		{Gate: factoryd.GateReproduction, Verdict: "not run"},
		{Gate: factoryd.GatePolicy, Verdict: "approval"},
		{Gate: factoryd.GateReproduction, Command: "bash check test ./acceptance", Exit: accExit(2), Verdict: "failed"},
	})
	same(t, "gates", f.Gates, gates)

	// the human decisions, oldest first, with their kind, decision and by
	decisions := accReceiptDecisions(ctx, t, s, id)
	if len(decisions) != 2 {
		t.Fatalf("the records carry %d decisions, want the approval and the case's answer", len(decisions))
	}
	if decisions[0].Kind != factoryd.ApprovalMerge || decisions[0].Decision != "approved" || decisions[0].By != "sebastian" {
		t.Errorf("the first decision = %+v, want the merge approval sebastian decided", decisions[0])
	}
	if decisions[1].Kind != "case" || decisions[1].Decision != factoryd.ActExtend || decisions[1].By != "sebastian" {
		t.Errorf("the second decision = %+v, want the rung-3 extend sebastian answered", decisions[1])
	}
	same(t, "decisions", f.Decisions, decisions)
	for i := 1; i < len(f.Decisions); i++ { // oldest first
		if f.Decisions[i].At == nil || f.Decisions[i-1].At == nil || f.Decisions[i].At.Before(*f.Decisions[i-1].At) {
			t.Errorf("decision %d (%+v) does not follow its elder (%+v)", i, f.Decisions[i], f.Decisions[i-1])
		}
	}

	// the assessments, beside the facts, labelled by their source
	same(t, "assessments", rec.Assessments, accReceiptAssessments(ctx, t, s, id))

	// the first write stands: the foundation's first-write signal answers
	// stored, the stored document is the built receipt whole — the jsonb
	// column renders its own bytes — and a second PutMergeReceipt, of a
	// receipt whose facts moved, changes no stored byte
	stored, err := s.PutMergeReceipt(ctx, id, rec)
	if err != nil {
		t.Fatal(err)
	}
	if !stored {
		t.Error("the first PutMergeReceipt answered not stored, want the first write's signal")
	}
	var document string
	if err := db.QueryRowContext(ctx, `SELECT document FROM merge_receipts WHERE task_id = $1`, id).Scan(&document); err != nil {
		t.Fatal(err)
	}
	var fromDocument factoryd.MergeReceipt
	if err := json.Unmarshal([]byte(document), &fromDocument); err != nil {
		t.Fatalf("the stored document = %s: %v", document, err)
	}
	if got, want := accReceiptJSON(t, fromDocument), accReceiptJSON(t, rec); got != want {
		t.Fatalf("the stored document = %s, want the built receipt's own form %s", got, want)
	}
	held, ok, err := s.MergeReceipt(ctx, id)
	if err != nil || !ok {
		t.Fatalf("MergeReceipt = (%v, %v), want the stored receipt back", ok, err)
	}
	if got, want := accReceiptJSON(t, held), accReceiptJSON(t, rec); got != want {
		t.Fatalf("the receipt read back = %s, want the built receipt's own form %s", got, want)
	}

	moved := rec
	moved.Facts.CostEUR = 99
	moved.Facts.Head = "c9-moved"
	again, err := s.PutMergeReceipt(ctx, id, moved)
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Error("the second PutMergeReceipt answered stored, want the first write to stand")
	}
	var reread string
	if err := db.QueryRowContext(ctx, `SELECT document FROM merge_receipts WHERE task_id = $1`, id).Scan(&reread); err != nil {
		t.Fatal(err)
	}
	if reread != document {
		t.Fatalf("the stored receipt changed on the second put:\n%s\nbecame\n%s", document, reread)
	}
}

// TestReceiptFactsFromRecordsPastTheDiffBound proves the variant whose
// diff record is past ErrDiffTooLong: no gate reads it, diff_sha256
// stays empty, and every other fact still equals its record's value.
func TestReceiptFactsFromRecordsPastTheDiffBound(t *testing.T) {
	ctx := context.Background()
	fj := serve(t, &accReceiptForgejo{head: "c2-head", mergeCommit: "m10", diff: strings.Repeat("a", 8<<20+1)})
	s, db := accReceiptStore(t, "acceptance_receipt_bound")
	objects := &accObjects{files: map[string][]byte{}}
	r := accReceiptRunner(t, s, fj, objects)
	const id = "rt2"
	accSeedReceipt(ctx, t, s, objects, id, 85)

	// the diff record itself: one byte past the most factoryd reads
	if body := accReceiptRecord(t, fj, "/api/v1/repos/sf/code/pulls/9.diff"); len(body) <= 8<<20 {
		t.Fatalf("the diff record holds %d bytes, want past the 8 MiB read bound", len(body))
	}

	rec, err := r.BuildMergeReceipt(ctx, id, 9)
	if err != nil {
		t.Fatal(err)
	}
	f := rec.Facts
	if f.DiffSHA256 != "" {
		t.Errorf("diff_sha256 = %q, want empty for a diff past ErrDiffTooLong", f.DiffSHA256)
	}
	// every other fact equals its record's value
	base, repository, issue, settled := accReceiptTaskRow(ctx, t, db, id)
	head, mergeCommit := accReceiptState(t, fj)
	policyVersion, gates := accReceiptTimeline(t, objects, id)
	if f.TaskID != id || f.Repository != repository || f.Issue != issue || f.PR != 9 || f.Base != base ||
		f.Head != head || f.MergeCommit != mergeCommit || f.PolicyVersion != policyVersion || f.CostEUR != settled {
		t.Errorf("facts = %+v; want the row's base %s and cost %v, the pull request's head %s and merge %s, the timeline's policy %d",
			f, base, settled, head, mergeCommit, policyVersion)
	}
	same(t, "gates", f.Gates, gates)
	same(t, "decisions", f.Decisions, accReceiptDecisions(ctx, t, s, id))
	same(t, "assessments", rec.Assessments, accReceiptAssessments(ctx, t, s, id))
}
