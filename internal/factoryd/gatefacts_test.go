package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The gate facts' runner: a gate runner whose event rows land in a real
// store's events table, the card's row inserted — the fact identities
// meet the store's one-row-per-identity rule, not a fake's.
func gateFactsRunner(t *testing.T, s *PGStore, task Task, fj *gateForgejo) (*Runner, *fakeStore, *fakeObjectStore) {
	t.Helper()
	r, store, objects := newGateRunner(t, task, fj)
	if err := s.insertTask(context.Background(), task, `ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	r.timeline.Events = s
	return r, store, objects
}

// gateHeadFacts reads the gate_head rows of tasks' cards, in id order,
// their details.
func gateHeadFacts(t *testing.T, s *PGStore, tasks ...string) []map[string]any {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT detail FROM events WHERE kind = $1 AND task_id = ANY($2) ORDER BY id`, EventGateHead, tasks)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var facts []map[string]any
	for rows.Next() {
		var b []byte
		var f map[string]any
		if err := rows.Scan(&b); err != nil || json.Unmarshal(b, &f) != nil {
			t.Fatalf("a gate_head row: %v", err)
		}
		facts = append(facts, f)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return facts
}

func factInt(t *testing.T, f map[string]any, key string) int {
	t.Helper()
	n, ok := f[key].(float64)
	if !ok {
		t.Fatalf("fact %v: %q is not a number", f, key)
	}
	return int(n)
}

// The merge decision's snapshot (roadmap 336): the card's size and the
// bound that size planned under the policy the gates read — the small
// bound, never the policy's large_lines standing in for it — with the
// policy version and the lines the gates counted, one row per card and
// head whichever path the decision took: the automatic merge's and the
// human approval's readings are the same fact. A card counts above its
// bound once, however many heads and retries reached the decision.
func TestDivideConquerGateSizeFacts(t *testing.T) {
	s := metricsStore(t)

	over := gateTask
	over.ID, over.Size, over.IssueNumber = "t-over", "small", 41
	ro, _, _ := gateFactsRunner(t, s, over, &gateForgejo{})
	out := GateResults{Policy: PolicyGate{LargeLines: 1000, PolicyVersion: 7, Files: 4, Insertions: 281, Deletions: 20}}
	// a retried head records its reading once; the decision's fall from
	// the automatic merge to a human's approval keeps it too
	for i := 0; i < 2; i++ {
		if _, err := execActivity(t, ro, ActivityAutoMergeNote, "t-over", int64(9), "h1", out, Review{}); err != nil {
			t.Fatal(err)
		}
	}
	in := GateResults{Policy: PolicyGate{LargeLines: 1000, PolicyVersion: 7, Files: 2, Insertions: 150, Deletions: 150}}
	if _, err := execActivity(t, ro, ActivityRequestApproval, "t-over",
		ApprovalInput{PR: PullRequest{Number: 9}, Commit: "h2", Gates: in}); err != nil {
		t.Fatal(err)
	}

	within := gateTask
	within.ID, within.Size, within.IssueNumber = "t-within", "small", 42
	rw, _, _ := gateFactsRunner(t, s, within, &gateForgejo{})
	if _, err := execActivity(t, rw, ActivityAutoMergeNote, "t-within", int64(9), "h3", in, Review{}); err != nil {
		t.Fatal(err)
	}

	facts := gateHeadFacts(t, s, "t-over", "t-within")
	if len(facts) != 3 {
		t.Fatalf("facts = %d rows, want 3: one per card and head, the retried head once", len(facts))
	}
	h1 := facts[0]
	if h1["head"] != "h1" || h1["gate"] != GateMerge || h1["decision"] != policy.OutcomeAuto {
		t.Fatalf("h1 = %v, want the automatic merge's reading at the merge gate", h1)
	}
	if h1["size"] != "small" || factInt(t, h1, "bound") != 300 {
		t.Fatalf("h1 = %v, want size small and bound 300: the small bound, not large_lines 1000", h1)
	}
	if factInt(t, h1, "policy_version") != 7 || factInt(t, h1, "files") != 4 || factInt(t, h1, "insertions") != 281 || factInt(t, h1, "deletions") != 20 {
		t.Fatalf("h1 = %v, want policy 7 and the gate's counts: 4 files, 281 added, 20 deleted", h1)
	}
	if facts[1]["head"] != "h2" || facts[1]["decision"] != "approval" {
		t.Fatalf("h2 = %v, want the human approval's reading", facts[1])
	}
	// the panel's question — how many cards crossed their bound — counts
	// a card once: t-over by its 301-line head, t-within never
	var crossed int
	if err := s.db.QueryRowContext(context.Background(), `SELECT count(DISTINCT task_id) FROM events WHERE kind = $1 AND task_id = ANY($2)
		AND (detail->>'insertions')::int + (detail->>'deletions')::int > (detail->>'bound')::int`, EventGateHead, []string{"t-over", "t-within"}).Scan(&crossed); err != nil {
		t.Fatal(err)
	}
	if crossed != 1 {
		t.Fatalf("cards above their bound = %d, want 1", crossed)
	}
}

// The fact a card records at the merge boundary ties to the head that
// merged: the abandoned head's reading and the merged head's reading are
// both facts, the merged one once however often the boundary runs; the
// merged step names the same head. A merge factoryd never decided — a
// human's in Forgejo, seen again as an already-merged pull request —
// leaves the head unknown: no fact is reconstructed for it.
func TestDivideConquerMergedLineFacts(t *testing.T) {
	s := metricsStore(t)

	// one card: a head abandoned for a newer one, which merges
	card := gateTask
	card.ID, card.Size, card.IssueNumber = "t-heads", "small", 43
	r, store, objects := gateFactsRunner(t, s, card, &gateForgejo{merged: true, mergedBy: MergeUser, head: "h2", mergeCommit: "m2"})
	g := GateResults{Policy: PolicyGate{PolicyVersion: 7, Files: 3, Insertions: 40, Deletions: 10}}
	if _, err := execActivity(t, r, ActivityRequestApproval, "t-heads",
		ApprovalInput{PR: PullRequest{Number: 9}, Commit: "h1", Gates: g}); err != nil {
		t.Fatal(err)
	}
	if _, err := execActivity(t, r, ActivityAutoMergeNote, "t-heads", int64(9), "h2", g, Review{}); err != nil {
		t.Fatal(err)
	}
	// the merge lands — its answer lost, the retry finds it merged
	if _, err := execActivity(t, r, ActivityMerge, "t-heads", int64(9), "h2", AutoApprover, "h2", []string{}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.Task(context.Background(), "t-heads"); got.MergeCommit != "m2" {
		t.Fatalf("merge commit = %q, want m2", got.MergeCommit)
	}

	// another card: a human merged it in Forgejo without factoryd's gate
	external := gateTask
	external.ID, external.Size, external.IssueNumber = "t-human", "small", 44
	rh, _, _ := gateFactsRunner(t, s, external, &gateForgejo{merged: true, mergedBy: "alice", head: "h9", mergeCommit: "m9"})
	if _, err := execActivity(t, rh, ActivityMerge, "t-human", int64(9), "h9", "alice", "h9", []string{}); err != nil {
		t.Fatal(err)
	}

	facts := gateHeadFacts(t, s, "t-heads", "t-human")
	if len(facts) != 2 {
		t.Fatalf("facts = %d rows, want 2: one per head of t-heads, none for t-human", len(facts))
	}
	if facts[0]["head"] != "h1" || facts[1]["head"] != "h2" {
		t.Fatalf("facts = %v, want the abandoned head h1 then the merged head h2", facts)
	}
	if step := timelineStep(t, objects, "t-heads", "merged"); step["commit"] != "h2" {
		t.Fatalf("merged step = %v, want it naming h2, the head the fact ties to", step)
	}
	var merged int
	if err := s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM events WHERE kind = $1 AND task_id = $2`, EventPullRequestMerged, "t-heads").Scan(&merged); err != nil {
		t.Fatal(err)
	}
	if merged != 1 {
		t.Fatalf("pull_request_merged rows = %d, want 1: the landing of h2", merged)
	}
}

// A recorded snapshot never moves: the card's metadata and the policy
// change after the reading — a new pinned version with another
// large_lines, the card re-sized — and the row keeps what the gates read;
// a retry of the head meets its identity, a new head reads anew. And a
// fact never fails the decision it records: a store that refuses the row
// is logged, the decision stands.
func TestDivideConquerGateSnapshotRetries(t *testing.T) {
	s := metricsStore(t)
	card := gateTask
	card.ID, card.Size, card.IssueNumber = "t-snap", "small", 45
	r, store, _ := gateFactsRunner(t, s, card, &gateForgejo{})
	first := GateResults{Policy: PolicyGate{PolicyVersion: 7, Files: 4, Insertions: 120, Deletions: 30}}
	if _, err := execActivity(t, r, ActivityAutoMergeNote, "t-snap", int64(9), "h1", first, Review{}); err != nil {
		t.Fatal(err)
	}

	// the card is re-sized and a new policy is pinned: 2000 large_lines
	task := store.tasks["t-snap"]
	task.Size = "large"
	store.tasks["t-snap"] = task
	newer := GateResults{Policy: PolicyGate{LargeLines: 2000, PolicyVersion: 8, Files: 6, Insertions: 900, Deletions: 900}}
	for _, head := range []string{"h1", "h2"} {
		if _, err := execActivity(t, r, ActivityAutoMergeNote, "t-snap", int64(9), head, newer, Review{}); err != nil {
			t.Fatal(err)
		}
	}

	facts := gateHeadFacts(t, s, "t-snap")
	if len(facts) != 2 {
		t.Fatalf("facts = %d rows, want 2: the retried head once, the new head anew", len(facts))
	}
	if facts[0]["head"] != "h1" || facts[0]["size"] != "small" || factInt(t, facts[0], "bound") != 300 ||
		factInt(t, facts[0], "policy_version") != 7 || factInt(t, facts[0], "insertions") != 120 {
		t.Fatalf("h1 = %v, want its first reading: small, bound 300, policy 7, 120 added", facts[0])
	}
	if facts[1]["head"] != "h2" || facts[1]["size"] != "large" || factInt(t, facts[1], "bound") != 2000 ||
		factInt(t, facts[1], "policy_version") != 8 || factInt(t, facts[1], "insertions") != 900 {
		t.Fatalf("h2 = %v, want the new reading: large, bound 2000, policy 8, 900 added", facts[1])
	}

	// a store that refuses the fact: logged, the decision stands
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	r.timeline.Events = failingEvents{}
	in := ApprovalInput{PR: PullRequest{Number: 9}, Commit: "h3", Gates: newer}
	if _, err := execActivity(t, r, ActivityRequestApproval, "t-snap", in); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "gate_head") {
		t.Fatalf("logs = %q, want the failed fact row logged", logs.String())
	}
	if len(gateHeadFacts(t, s, "t-snap")) != 2 {
		t.Fatal("the failed write left no fact")
	}
}

// failingEvents refuses every row: a fact's write that cannot land.
type failingEvents struct{}

func (failingEvents) Record(context.Context, Event) (int64, error) {
	return 0, errors.New("events table down")
}
