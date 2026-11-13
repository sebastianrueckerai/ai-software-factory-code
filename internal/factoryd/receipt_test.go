package factoryd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The receipt's proof (roadmap 66): every record below is seeded
// directly, and the test derives each receipt fact again from those
// records alone — the hash from the diff bytes, the policy version from
// the gate_head fact's row, the gates from the timeline's steps, the cost
// from the settled spend, the decisions from the approval and case rows —
// and asks the receipt the builder built to match.

// receiptDiff is the pull request's unified diff as the httptest Forgejo
// serves it: two files, whose texts in the diff's order are the bytes the
// hash is taken over.
const receiptDiff = `diff --git a/internal/shop/order.go b/internal/shop/order.go
index 1a2b3c4..5d6e7f8 100644
--- a/internal/shop/order.go
+++ b/internal/shop/order.go
@@ -4,3 +4,6 @@ func Total(lines []Line) int {
 	for _, l := range lines {
 		total += l.Cents
 	}
+	if total < 0 {
+		return 0
+	}
 	return total
 }
diff --git a/internal/shop/order_test.go b/internal/shop/order_test.go
index 9e8f7a6..4c5d6e7 100644
--- a/internal/shop/order_test.go
+++ b/internal/shop/order_test.go
@@ -9,3 +9,8 @@ func TestTotal(t *testing.T) {
 	if got := Total(nil); got != 0 {
 		t.Fatalf("Total(nil) = %d", got)
 	}
+	if got := Total([]Line{{Cents: -1}}); got != 0 {
+		t.Fatalf("a negative line clamps to zero, got %d", got)
+	}
 }
`

var (
	receiptApprovedAt = time.Date(2026, 11, 9, 18, 5, 0, 0, time.UTC)
	receiptAnsweredAt = time.Date(2026, 11, 9, 18, 40, 0, 0, time.UTC)
)

// seedReceiptRecords seeds t1's merged records directly: the task row
// (the settled spend, the risk class), the timeline's steps, the gate_head
// fact, the approval rows, the case rows and the criteria verdict rows.
func seedReceiptRecords(ctx context.Context, t *testing.T, store *fakeStore, objects *fakeObjectStore) {
	t.Helper()
	task := store.tasks["t1"]
	task.RiskClass, task.SpentEUR = "large", 3.5
	store.tasks["t1"] = task

	// the timeline's steps as the writer wrote them, one JSON object a
	// line: the policy gate's two runs, the reproduction's skipped first
	// run and its passing second, the review's and the audit's verdicts
	steps := []map[string]any{
		{"step": "gate_policy", "pr": 9, "commit": "c0", "class": "small", "outcome": "auto", "policy_version": 7, "merge_rule": "auto"},
		{"step": "gate_reproduction", "pr": 9, "commit": "c0", "skipped": "no reproduction command"},
		{"step": "gate_policy", "pr": 9, "commit": "c1", "class": "small", "outcome": "auto", "policy_version": 7, "merge_rule": "auto"},
		{"step": "gate_reproduction", "pr": 9, "commit": "c1", "command": "bash check", "exit": 0, "output": "ok\n", "base": "base", "base_exit": 1},
		{"step": "model_review", "pr": 9, "commit": "c1", "model": "factory/big", "verdict": "approved", "summary": "a strict improvement"},
		{"step": "audit", "pr": 9, "commit": "c1", "model": "factory/big", "deployment": true, "verdict": "clear"},
	}
	lines := make([]string, len(steps))
	for i, e := range steps {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		lines[i] = string(b)
	}
	if err := objects.Put(ctx, "sessions/t1/timeline.jsonl", []byte(strings.Join(lines, "\n")+"\n")); err != nil {
		t.Fatal(err)
	}

	// the gates' reading of the head, with the policy version it read
	fact := GateHeadFact{Task: "t1", Head: "c1", Gate: GateMerge, Decision: "approval", Size: "medium",
		Bound: 600, PolicyVersion: 7, Files: 3, Insertions: 120, Deletions: 20}
	if _, err := store.Record(ctx, fact.Event(gateTask.Repository, gateTask.IssueNumber)); err != nil {
		t.Fatal(err)
	}

	supersededAt := receiptApprovedAt.Add(-time.Hour)
	store.approvals = []ApprovalRequest{
		{ID: 1, Kind: ApprovalMerge, TaskID: "t1", Project: "sf/repo", PRNumber: 9, Status: "approved", // the human's
			DecidedBy: "sebastian", DecidedAt: &receiptApprovedAt, DecisionNote: "the strict improvement holds"},
		{ID: 2, Kind: ApprovalMerge, TaskID: "t1", Project: "sf/repo", PRNumber: 9, Status: "denied", // the factory's own
			DecidedBy: FactoryDecider, DecidedAt: &supersededAt, DecisionNote: "superseded by a newer request"},
		{ID: 3, Kind: ApprovalMerge, TaskID: "t1", Project: "sf/repo", PRNumber: 9, Status: "pending"}, // undecided
		{ID: 4, Kind: ApprovalMerge, TaskID: "t9", Project: "sf/repo", PRNumber: 8, Status: "approved", // another task's
			DecidedBy: "miriam", DecidedAt: &receiptApprovedAt, DecisionNote: "unrelated"},
	}

	opened, handedUp, chatAt := receiptApprovedAt.Add(-2*time.Hour), receiptApprovedAt.Add(-90*time.Minute), receiptApprovedAt.Add(-30*time.Minute)
	store.cases = []Case{
		{ID: 1, TaskID: "t1", Repository: "sf/repo", Kind: CaseGateStop, Status: CaseResolved, OpenedAt: opened, // rung 2: the chat's answer for the member
			ResolvedAt: &chatAt, Resolution: &CaseResolution{By: "sebastian", Rung: 2, Action: ActContinue, Reason: "hold the run until the deploy lands"}},
		{ID: 2, TaskID: "t1", Repository: "sf/repo", Kind: CaseMergeApproval, Status: CaseResolved, OpenedAt: handedUp, // the approval's own case
			ResolvedAt: &receiptApprovedAt, Resolution: &CaseResolution{By: "sebastian", Rung: 3, Action: "approve", Reason: "the strict improvement holds"}},
		{ID: 3, TaskID: "t1", Repository: "sf/repo", Kind: CaseSessionCap, Status: CaseResolved, OpenedAt: handedUp, // the human's answer
			ResolvedAt: &receiptAnsweredAt, Resolution: &CaseResolution{By: "sebastian", Rung: 3, Action: ActExtend,
				Args: map[string]any{"sessions": 2, "agent_time_s": 3600}, Reason: "the flaky checkout needs two more sessions"}},
		{ID: 4, TaskID: "t1", Repository: "sf/repo", Kind: CaseCheckinShare, Status: CaseAtHuman, OpenedAt: receiptAnsweredAt}, // still open
	}

	store.criteriaVerdicts = map[string][]CriterionVerdict{"t1": {
		{Criterion: "all_green", Verdict: "met", Evidence: "blind: criterion all_green: suite ./... passed", Kind: CriterionInvariant, Source: CriteriaSourceGate, DecidedBy: FactoryDecider},
		{Criterion: "reads_calm", Verdict: "met", Evidence: "the page keeps its calm with 200 rows on it", Kind: CriterionJudgement, Source: CriteriaSourceJudgement, DecidedBy: FactoryDecider},
		{Criterion: "pages_ok", Verdict: "met", Evidence: "opened both pages", Kind: CriterionHuman, Source: CriteriaSourceApprover, DecidedBy: "sebastian"},
	}}
}

func TestBuildMergeReceiptDerivesEveryFactFromTheRecords(t *testing.T) {
	ctx := context.Background()
	fj := &gateForgejo{diff: receiptDiff, merged: true, mergeCommit: "m1", head: "c1"}
	r, store, objects := newGateRunner(t, gateTask, fj)
	seedReceiptRecords(ctx, t, store, objects)

	got, err := r.BuildMergeReceipt(ctx, "t1", 9)
	if err != nil {
		t.Fatal(err)
	}

	// the hash, taken again over the served diff bytes alone
	sum := sha256.Sum256([]byte(receiptDiff))

	// the policy version, read again from the fact's row
	rows, err := store.TaskEvents(ctx, "t1", 8)
	if err != nil {
		t.Fatal(err)
	}
	var policyVersion int64
	for _, e := range rows {
		if e.Kind == EventGateHead {
			policyVersion, _ = e.Detail["policy_version"].(int64)
		}
	}

	// the reproduction's exit, a plain 0
	passed := 0

	want := MergeReceipt{
		Version: MergeReceiptVersion,
		Facts: ReceiptFacts{
			TaskID: "t1", Repository: "sf/repo", Issue: 4, PR: 9,
			Base: "base", Head: "c1", MergeCommit: "m1", // the task row's base, the merge's head and commit
			DiffSHA256:    hex.EncodeToString(sum[:]),
			PolicyVersion: policyVersion,
			Gates: []ReceiptGate{ // every gate run the timeline recorded, oldest first
				{Gate: GatePolicy, Verdict: "auto"},
				{Gate: GateReproduction, Verdict: "not run"},
				{Gate: GatePolicy, Verdict: "auto"},
				{Gate: GateReproduction, Command: "bash check", Exit: &passed, Verdict: "passed"},
			},
			CostEUR: 3.5, // the task row's settled spend
			Decisions: []ReceiptDecision{ // the human's, oldest first: the approval, the case it answered
				{Kind: ApprovalMerge, Decision: "approved", By: "sebastian", Note: "the strict improvement holds", At: &receiptApprovedAt},
				{Kind: "case", Decision: ActExtend, By: "sebastian", Draw: "2 sessions and 60 minutes of agent time",
					Note: "the flaky checkout needs two more sessions", At: &receiptAnsweredAt},
			},
		},
		Assessments: []ReceiptAssessment{ // the model's, labelled by source; the gate's and the approver's rows are none of them
			{Source: CriteriaSourceReview, Verdict: "approved"},
			{Source: "audit", Verdict: "clear"},
			{Source: CriteriaSourceJudgement, Criterion: "reads_calm", Verdict: "met", Evidence: "the page keeps its calm with 200 rows on it"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("receipt =\n%+v\nwant\n%+v", got, want)
	}

	// the lower rung's answer, whatever name it carries, is no decision
	for _, d := range got.Facts.Decisions {
		if d.Decision == ActContinue || d.Decision == "approve" {
			t.Fatalf("decision %+v: the chat's and the approval's own cases are no decisions of their own", d)
		}
	}
}

// A diff past the most factoryd reads (ErrDiffTooLong) leaves the
// receipt's diff hash empty; every other fact stands.
func TestBuildMergeReceiptPastTheDiffBoundLeavesTheHashEmpty(t *testing.T) {
	ctx := context.Background()
	fj := &gateForgejo{diff: strings.Repeat("a", maxDiffRead+1), merged: true, mergeCommit: "m1", head: "c1"}
	r, store, objects := newGateRunner(t, gateTask, fj)
	seedReceiptRecords(ctx, t, store, objects)

	got, err := r.BuildMergeReceipt(ctx, "t1", 9)
	if err != nil {
		t.Fatal(err)
	}
	if got.Facts.DiffSHA256 != "" {
		t.Fatalf("diff_sha256 = %q, want empty past the read bound", got.Facts.DiffSHA256)
	}
	if got.Facts.TaskID != "t1" || got.Facts.MergeCommit != "m1" || got.Facts.CostEUR != 3.5 || len(got.Facts.Gates) != 4 {
		t.Fatalf("receipt = %+v: want every other fact of the records", got.Facts)
	}
}

// The receipt is one document of declared fields, version 2 on every
// receipt, its facts a nested document that carries diff_sha256 — and
// the same records build the same bytes twice — no map-shaped output an
// export could not read back.
func TestMergeReceiptBuildsIdenticallyAndCarriesItsVersion(t *testing.T) {
	ctx := context.Background()
	fj := &gateForgejo{diff: receiptDiff, merged: true, mergeCommit: "m1", head: "c1"}
	r, store, objects := newGateRunner(t, gateTask, fj)
	seedReceiptRecords(ctx, t, store, objects)

	first, err := r.BuildMergeReceipt(ctx, "t1", 9)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.BuildMergeReceipt(ctx, "t1", 9)
	if err != nil {
		t.Fatal(err)
	}
	if MergeReceiptVersion != 2 {
		t.Fatalf("MergeReceiptVersion = %d, want 2: the shape changed, and no real merge may record a mislabelled document", MergeReceiptVersion)
	}
	if first.Version != MergeReceiptVersion {
		t.Fatalf("receipt version %d, want %d (MergeReceiptVersion) on every receipt", first.Version, MergeReceiptVersion)
	}

	a, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("the same records built\n%s\nand\n%s", a, b)
	}

	// the version-2 document's shape: the facts one nested document
	// carrying task and diff_sha256, no diff_hash anywhere
	var doc map[string]any
	if err := json.Unmarshal(a, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc) != 3 || doc["version"] != float64(2) {
		t.Fatalf("document's keys %v, want version, facts and assessments alone", doc)
	}
	facts, ok := doc["facts"].(map[string]any)
	if !ok || facts["task"] != "t1" || facts["diff_sha256"] == nil || facts["diff_sha256"] == "" {
		t.Fatalf("facts = %v, want the nested task and a diff_sha256", facts)
	}
	if strings.Contains(string(a), "diff_hash") {
		t.Fatalf("document %s carries the v1 diff_hash", a)
	}

	// and the document reads back to the receipt it came from
	var back MergeReceipt
	if err := json.Unmarshal(a, &back); err != nil {
		t.Fatal(err)
	}
	if again, err := json.Marshal(back); err != nil || string(again) != string(a) {
		t.Fatalf("the document reads back differently: %s (%v)", again, err)
	}
}

// receiptDigest reads the stored receipt of taskID and its bytes, for
// the merge paths' tests: the first write stands, so every later read
// must answer the same document.
func receiptDigest(t *testing.T, store *fakeStore, taskID string) (MergeReceipt, string) {
	t.Helper()
	got, ok, err := store.MergeReceipt(context.Background(), taskID)
	if err != nil || !ok {
		t.Fatalf("stored receipt = (%v, %v), want one", ok, err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	return got, string(b)
}

// The merge gate records the card's receipt on its merge (roadmap 66),
// and a gate that runs its merge again — a retry after a merge whose
// answer was lost, a human's merge found merged — writes nothing over
// the receipt the first run recorded.
func TestMergeGateRecordsTheReceiptOnce(t *testing.T) {
	ctx := context.Background()
	fj := &gateForgejo{diff: receiptDiff, mergeStatus: []int{200}, mergeCommit: "m-f1"}
	r, store, objects := newGateRunner(t, gateTask, fj)
	seedReceiptRecords(ctx, t, store, objects)

	val, err := execActivity(t, r, ActivityMerge, "t1", int64(9), "c1", "sebastian", "", []string(nil))
	if res := getInto[MergeResult](t, val, err); !res.Merged {
		t.Fatalf("result = %+v", res)
	}
	got, digest := receiptDigest(t, store, "t1")
	sum := sha256.Sum256([]byte(receiptDiff))
	if got.Version != MergeReceiptVersion || got.Facts.TaskID != "t1" || got.Facts.PR != 9 ||
		got.Facts.MergeCommit != "m-f1" || got.Facts.DiffSHA256 != hex.EncodeToString(sum[:]) || got.Facts.CostEUR != 3.5 {
		t.Fatalf("receipt =\n%+v\nwant the merge's facts, version %d", got, MergeReceiptVersion)
	}

	// the gate runs again and finds its own merge: the first write stands
	val, err = execActivity(t, r, ActivityMerge, "t1", int64(9), "c1", "sebastian", "", []string(nil))
	if res := getInto[MergeResult](t, val, err); !res.Merged {
		t.Fatalf("rerun result = %+v", res)
	}
	if again, d := receiptDigest(t, store, "t1"); d != digest || !reflect.DeepEqual(again, got) {
		t.Fatalf("receipt after the rerun =\n%+v\nwant the first receipt\n%+v", again, got)
	}
}

// The webhook's prDecided records a merged human card's receipt (the
// Intake.MergeReceipt callback), and a redelivered webhook records
// nothing over it; a close without a merge records nothing at all.
func TestPRDecidedRecordsTheReceiptOnce(t *testing.T) {
	ctx := context.Background()
	fj := &gateForgejo{diff: receiptDiff, merged: true, mergeCommit: "m1", head: "c1"}
	r, store, objects := newGateRunner(t, gateTask, fj)
	seedReceiptRecords(ctx, t, store, objects)
	in := &Intake{MergeReceipt: r.RecordMergeReceipt}

	if err := in.prDecided(ctx, gateTask, 9, false, "sebastian", ViaWebhook); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.MergeReceipt(ctx, "t1"); err != nil || ok {
		t.Fatalf("closed without a merge: stored receipt = (%v, %v), want none", ok, err)
	}

	if err := in.prDecided(ctx, gateTask, 9, true, "sebastian", ViaWebhook); err != nil {
		t.Fatal(err)
	}
	got, digest := receiptDigest(t, store, "t1")
	sum := sha256.Sum256([]byte(receiptDiff))
	if got.Version != MergeReceiptVersion || got.Facts.TaskID != "t1" || got.Facts.PR != 9 ||
		got.Facts.MergeCommit != "m1" || got.Facts.DiffSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("receipt =\n%+v\nwant the merge's facts, version %d", got, MergeReceiptVersion)
	}

	// the webhook redelivers its merge: the first write stands
	if err := in.prDecided(ctx, gateTask, 9, true, "sebastian", ViaWebhook); err != nil {
		t.Fatal(err)
	}
	if again, d := receiptDigest(t, store, "t1"); d != digest || !reflect.DeepEqual(again, got) {
		t.Fatalf("receipt after the redelivery =\n%+v\nwant the first receipt\n%+v", again, got)
	}
}

// The finish heal's cardMerged records a merged card's receipt when it
// discovers the merge itself, and a finish that runs again — the merge
// commit now recorded — writes nothing over it.
func TestCardMergedRecordsTheReceiptOnce(t *testing.T) {
	ctx := context.Background()
	fj := &gateForgejo{diff: receiptDiff, merged: true, mergeCommit: "m1", head: "c1"}
	r, store, objects := newGateRunner(t, gateTask, fj)
	seedReceiptRecords(ctx, t, store, objects)
	repo := Repo{Owner: "sf", Name: "repo"}

	merged, err := r.cardMerged(ctx, repo, gateTask)
	if err != nil || !merged {
		t.Fatalf("cardMerged = (%v, %v), want merged", merged, err)
	}
	got, digest := receiptDigest(t, store, "t1")
	sum := sha256.Sum256([]byte(receiptDiff))
	if got.Version != MergeReceiptVersion || got.Facts.TaskID != "t1" || got.Facts.PR != 9 ||
		got.Facts.MergeCommit != "m1" || got.Facts.DiffSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("receipt =\n%+v\nwant the merge's facts, version %d", got, MergeReceiptVersion)
	}

	// the heal runs again on the card whose merge commit it recorded:
	// the first write stands
	t1, _, err := store.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if merged, err = r.cardMerged(ctx, repo, t1); err != nil || !merged {
		t.Fatalf("cardMerged again = (%v, %v), want merged", merged, err)
	}
	if again, d := receiptDigest(t, store, "t1"); d != digest || !reflect.DeepEqual(again, got) {
		t.Fatalf("receipt after the rerun =\n%+v\nwant the first receipt\n%+v", again, got)
	}
}
