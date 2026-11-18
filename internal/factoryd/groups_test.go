package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

func TestGroupOf(t *testing.T) {
	for _, tc := range []struct {
		name string
		card Task
		want string
	}{
		{"a plan's child", Task{ID: "c1", Source: SourcePlan, Parent: "p1"}, "p1"},
		{"a direct card", Task{ID: "d1", Source: "webhook"}, "d1"},
		{"a plan card", Task{ID: "p1", Source: "webhook", Track: "plan"}, "p1"},
		{"a delegated card", Task{ID: "g1", Source: SourceDelegate, Parent: "d1"}, "g1"},
		{"a plan card's successor", Task{ID: "s1", Source: SourceSuccessor, Parent: "p1", PlanKey: "links"}, "p1"},
		{"a direct card's successor", Task{ID: "s2", Source: SourceSuccessor}, "s2"},
	} {
		if got := GroupOf(tc.card); got != tc.want {
			t.Errorf("%s: GroupOf = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A plan card, a plan's child and the successor that carries its key are
// in a plan; a direct card, its successor, a delegated one and a person's
// card are not.
func TestInPlan(t *testing.T) {
	for _, tc := range []struct {
		name string
		card Task
		want bool
	}{
		{"a plan's child", Task{ID: "c1", Source: SourcePlan, Parent: "p1", Track: "direct"}, true},
		{"a plan card", Task{ID: "p1", Source: "webhook", Track: "plan"}, true},
		{"a direct card", Task{ID: "d1", Source: "webhook", Track: "direct"}, false},
		{"a delegated card", Task{ID: "g1", Source: SourceDelegate, Parent: "d1", Track: "direct"}, false},
		{"a person's card", Task{ID: "h1", Kind: "human", Source: "webhook"}, false},
		{"a plan card's successor", Task{ID: "s1", Source: SourceSuccessor, Parent: "p1", PlanKey: "links", Track: "direct"}, true},
		{"a direct card's successor", Task{ID: "s2", Source: SourceSuccessor, Track: "direct"}, false},
	} {
		if got := InPlan(tc.card); got != tc.want {
			t.Errorf("%s: InPlan = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// The open groups on main: p6 (its merge the day before p1's, so the
// plan ids' order is not the groups'), p1 (three children, two merged,
// the merge events' earliest 09:00 although c3 finished at 09:05) and p5
// (a merge commit and no event, so its child's finish) in sf/r, p4
// (merged by a person: an event and no merge commit) in sf/q. p2 has no
// merged child, p3 is closed, d1 is a direct card, and p7's only merged
// child c9 was taken off main again. p8's key one was cut: its card c10
// failed, and its successor b10, filed after it, merged and opened the
// group; each key counts once, as its newest card, so p8 holds one merged
// child and one queued.
func TestOpenGroups(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_groups"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, q := range []string{
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, parent_task, track, plan_key, issue_number, merge_commit, finished_at) VALUES
			('p1', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 1, '', '2026-09-30T00:00:00Z'),
			('c1', 1, 0, 'done', 'sf/r', 'agent', 'plan', 'p1', 'direct', 'one', 11, 'abc', '2026-10-01T10:05:00Z'),
			('c2', 1, 0, 'queued', 'sf/r', 'agent', 'plan', 'p1', 'direct', 'two', 12, '', NULL),
			('c3', 1, 0, 'merged', 'sf/r', 'agent', 'plan', 'p1', 'direct', 'three', 13, 'def', '2026-10-01T09:05:00Z'),
			('p2', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 2, '', '2026-09-30T00:00:00Z'),
			('c4', 1, 0, 'queued', 'sf/r', 'agent', 'plan', 'p2', 'direct', 'one', 21, '', NULL),
			('p3', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 3, '', '2026-09-30T00:00:00Z'),
			('c5', 1, 0, 'done', 'sf/r', 'agent', 'plan', 'p3', 'direct', 'one', 31, 'jkl', '2026-10-01T07:00:00Z'),
			('p4', 1, 0, 'done', 'sf/q', 'agent', 'webhook', NULL, 'plan', NULL, 4, '', '2026-09-30T00:00:00Z'),
			('c6', 1, 0, 'merged', 'sf/q', 'agent', 'plan', 'p4', 'direct', 'one', 41, '', '2026-10-03T12:05:00Z'),
			('d1', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'direct', NULL, 9, 'mno', '2026-10-01T06:00:00Z'),
			('p5', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 5, '', '2026-09-30T00:00:00Z'),
			('c7', 1, 0, 'done', 'sf/r', 'agent', 'plan', 'p5', 'direct', 'one', 51, 'ghi', '2026-10-02T00:00:00Z'),
			('p6', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 6, '', '2026-09-29T00:00:00Z'),
			('c8', 1, 0, 'merged', 'sf/r', 'agent', 'plan', 'p6', 'direct', 'one', 61, 'pqr', '2026-09-30T18:05:00Z'),
			('p7', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 7, '', '2026-09-30T00:00:00Z'),
			('c9', 1, 0, 'done', 'sf/r', 'agent', 'plan', 'p7', 'direct', 'one', 71, 'stu', '2026-10-01T05:00:00Z'),
			('p8', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 8, '', '2026-09-30T00:00:00Z'),
			('c10', 1, 0, 'failed', 'sf/r', 'agent', 'plan', 'p8', 'direct', 'one', 81, '', '2026-10-01T08:00:00Z'),
			('b10', 1, 0, 'merged', 'sf/r', 'agent', 'successor', 'p8', 'direct', 'one', 82, 'vwx', '2026-10-01T11:05:00Z'),
			('c11', 1, 0, 'queued', 'sf/r', 'agent', 'plan', 'p8', 'direct', 'two', 83, '', NULL)`,
		`UPDATE tasks SET reverted_at = '2026-10-04T09:00:00Z' WHERE id = 'c9'`,
		`UPDATE tasks SET created_at = '2026-10-01T00:00:00Z' WHERE id = 'c10'`,
		`UPDATE tasks SET created_at = '2026-10-01T08:00:00Z' WHERE id = 'b10'`,
		`INSERT INTO events (at, repository, task_id, issue_number, kind, actor, via) VALUES
			('2026-10-01T10:00:00Z', 'sf/r', 'c1', 11, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-10-01T09:00:00Z', 'sf/r', 'c3', 13, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-10-01T07:00:00Z', 'sf/r', 'c5', 31, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-10-02T08:00:00Z', 'sf/r', 'p3', 3, 'plan_closed', 'factoryd', 'factoryd'),
			('2026-10-03T12:00:00Z', 'sf/q', 'c6', 41, 'pull_request_merged', 'sebastian', 'webhook'),
			('2026-10-01T06:00:00Z', 'sf/r', 'd1', 9, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-09-30T18:00:00Z', 'sf/r', 'c8', 61, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-10-01T04:55:00Z', 'sf/r', 'c9', 71, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-10-01T11:00:00Z', 'sf/r', 'b10', 82, 'pull_request_merged', 'factory-merge', 'factoryd')`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	got, err := s.OpenGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, groups := range got {
		for i := range groups {
			groups[i].FirstMerge = groups[i].FirstMerge.UTC()
		}
	}
	want := map[string][]Group{
		"sf/r": {
			{Plan: "p6", Issue: 6, Repository: "sf/r", FirstMerge: time.Date(2026, 9, 30, 18, 0, 0, 0, time.UTC),
				Children: map[string]int{"merged": 1}},
			{Plan: "p1", Issue: 1, Repository: "sf/r", FirstMerge: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
				Children: map[string]int{"done": 1, "queued": 1, "merged": 1}},
			{Plan: "p8", Issue: 8, Repository: "sf/r", FirstMerge: time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC),
				Children: map[string]int{"merged": 1, "queued": 1}},
			{Plan: "p5", Issue: 5, Repository: "sf/r", FirstMerge: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
				Children: map[string]int{"done": 1}},
		},
		"sf/q": {
			{Plan: "p4", Issue: 4, Repository: "sf/q", FirstMerge: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
				Children: map[string]int{"merged": 1}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OpenGroups =\n%+v\nwant\n%+v", got, want)
	}
	for plan, want := range map[string]time.Time{"p3": time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC), "p1": {}} {
		if at, closed, err := s.PlanClosed(ctx, plan); err != nil || closed != !want.IsZero() || !at.Equal(want) {
			t.Errorf("PlanClosed(%s) = %v, %v, %v; want %v, %v", plan, at, closed, err, want, !want.IsZero())
		}
	}
	for plan, want := range map[string]bool{"p8": true, "p2": false, "p7": false} {
		if merged, err := s.PlanMerged(ctx, plan); err != nil || merged != want {
			t.Errorf("PlanMerged(%s) = %v, %v; want %v", plan, merged, err, want)
		}
	}
}

// The plans between rounds are the done or closed plan cards without a
// plan_closed row whose newest revision holds an outline: p1, p7, whose
// card a person closed, and p8, whose newest revision added one. p2's
// newest revision dropped its outline, p3 runs, p4 and the closed p9 are
// closed by their rows, p5's outline is empty and p6's null.
func TestPlansBetweenRounds(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_rounds"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, track, issue_number, finished_at) VALUES
		('p1', 1, 0, 'done', 'sf/r', 'agent', 'webhook', 'plan', 1, '2026-10-01T09:00:00Z'),
		('p2', 1, 0, 'done', 'sf/r', 'agent', 'webhook', 'plan', 2, '2026-10-01T09:00:00Z'),
		('p3', 1, 0, 'running', 'sf/r', 'agent', 'webhook', 'plan', 3, NULL),
		('p4', 1, 0, 'done', 'sf/r', 'agent', 'webhook', 'plan', 4, '2026-10-01T09:00:00Z'),
		('p5', 1, 0, 'done', 'sf/r', 'agent', 'webhook', 'plan', 5, '2026-10-01T09:00:00Z'),
		('p6', 1, 0, 'done', 'sf/r', 'agent', 'webhook', 'plan', 6, '2026-10-01T09:00:00Z'),
		('p7', 1, 0, 'closed', 'sf/r', 'agent', 'webhook', 'plan', 7, '2026-10-01T09:00:00Z'),
		('p8', 1, 0, 'done', 'sf/q', 'agent', 'webhook', 'plan', 8, '2026-10-01T09:00:00Z'),
		('p9', 1, 0, 'closed', 'sf/r', 'agent', 'webhook', 'plan', 9, '2026-10-01T09:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	outline, last := `{"round": 1, "outline": ["the console"]}`, `{"round": 2}`
	for _, r := range []struct{ id, plan string }{
		{"p1", outline}, {"p2", outline}, {"p2", last}, {"p3", outline}, {"p4", outline},
		{"p5", `{"round": 1, "outline": []}`}, {"p6", `{"round": 1, "outline": null}`}, {"p7", outline}, {"p8", last}, {"p8", outline}, {"p9", outline},
	} {
		if _, err := s.AddPlanRevision(ctx, r.id, json.RawMessage(r.plan), r.id+r.plan); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO events (at, repository, task_id, issue_number, kind, actor, via) VALUES
		('2026-10-02T08:00:00Z', 'sf/r', 'p4', 4, 'plan_closed', 'factoryd', 'factoryd'),
		('2026-10-02T08:00:00Z', 'sf/r', 'p9', 9, 'plan_closed', 'factoryd', 'factoryd')`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PlansBetweenRounds(ctx); err != nil || !slices.Equal(got, []string{"p1", "p7", "p8"}) {
		t.Fatalf("PlansBetweenRounds = %v, %v; want [p1 p7 p8]", got, err)
	}
}

// The plans awaiting their criteria are the plan cards done or failed
// without a plan_closed row whose newest revision names criteria and no
// later round, with that revision's integration card filed and every card
// final or merged: the done a1, whose cards ended closed and merged, and
// the failed a2 whose outline is null, and a12, which a human closed
// after a run of the gate on main's head. a3's row closed it, a4 has
// rounds to come, a5 runs, a6 names no criteria, a7's newest revision
// dropped them, a8 a human closed with no run of the gate, a9's
// integration card still runs, a10 filed no integration card, a11's card
// under the integration card's key is another revision's feature card,
// and a13, closed, had its run on the integration card's pull request.
// a14's and a15's integration cards were cut: a14's successor still runs,
// a15's merged.
func TestPlansAwaitingCriteria(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_awaiting_criteria"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const fin = "'2026-10-01T09:00:00Z'"
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, track, issue_number, finished_at, parent_task, plan_key) VALUES
		('a1', 1, 0, 'done', 'sf/r', 'agent', 'webhook', 'plan', 1, `+fin+`, NULL, NULL), ('a2', 1, 0, 'failed', 'sf/r', 'agent', 'webhook', 'plan', 2, `+fin+`, NULL, NULL),
		('a3', 1, 0, 'done', 'sf/r', 'agent', 'webhook', 'plan', 3, `+fin+`, NULL, NULL), ('a4', 1, 0, 'done', 'sf/r', 'agent', 'webhook', 'plan', 4, `+fin+`, NULL, NULL),
		('a5', 1, 0, 'running', 'sf/r', 'agent', 'webhook', 'plan', 5, NULL, NULL, NULL), ('a6', 1, 0, 'done', 'sf/r', 'agent', 'webhook', 'plan', 6, `+fin+`, NULL, NULL),
		('a7', 1, 0, 'done', 'sf/r', 'agent', 'webhook', 'plan', 7, `+fin+`, NULL, NULL), ('a8', 1, 0, 'closed', 'sf/r', 'agent', 'webhook', 'plan', 8, `+fin+`, NULL, NULL),
		('a9', 1, 0, 'done', 'sf/r', 'agent', 'webhook', 'plan', 9, `+fin+`, NULL, NULL), ('a10', 1, 0, 'done', 'sf/r', 'agent', 'webhook', 'plan', 10, `+fin+`, NULL, NULL),
		('a11', 1, 0, 'done', 'sf/r', 'agent', 'webhook', 'plan', 11, `+fin+`, NULL, NULL),
		('a12', 1, 0, 'closed', 'sf/r', 'agent', 'webhook', 'plan', 12, `+fin+`, NULL, NULL),
		('a13', 1, 0, 'closed', 'sf/r', 'agent', 'webhook', 'plan', 13, `+fin+`, NULL, NULL),
		('a14', 1, 0, 'done', 'sf/r', 'agent', 'webhook', 'plan', 14, `+fin+`, NULL, NULL),
		('a15', 1, 0, 'done', 'sf/r', 'agent', 'webhook', 'plan', 15, `+fin+`, NULL, NULL),
		('a14-int', 1, 0, 'failed', 'sf/r', 'agent', 'plan', 'direct', 35, `+fin+`, 'a14', 'integ'),
		('a14-s', 1, 0, 'running', 'sf/r', 'agent', 'successor', 'direct', 36, NULL, 'a14', 'integ'),
		('a15-int', 1, 0, 'failed', 'sf/r', 'agent', 'plan', 'direct', 37, `+fin+`, 'a15', 'integ'),
		('a15-s', 1, 0, 'merged', 'sf/r', 'agent', 'successor', 'direct', 38, `+fin+`, 'a15', 'integ'),
		('a12-int', 1, 0, 'closed', 'sf/r', 'agent', 'plan', 'direct', 33, `+fin+`, 'a12', 'integ'),
		('a13-int', 1, 0, 'done', 'sf/r', 'agent', 'plan', 'direct', 34, `+fin+`, 'a13', 'integ'),
		('a1-int', 1, 0, 'closed', 'sf/r', 'agent', 'plan', 'direct', 21, `+fin+`, 'a1', 'integ'),
		('a1-f', 1, 0, 'merged', 'sf/r', 'agent', 'plan', 'direct', 22, `+fin+`, 'a1', 'feat'),
		('a2-int', 1, 0, 'failed', 'sf/r', 'agent', 'plan', 'direct', 23, `+fin+`, 'a2', 'integ'),
		('a3-int', 1, 0, 'done', 'sf/r', 'agent', 'plan', 'direct', 24, `+fin+`, 'a3', 'integ'),
		('a4-int', 1, 0, 'done', 'sf/r', 'agent', 'plan', 'direct', 25, `+fin+`, 'a4', 'integ'),
		('a5-int', 1, 0, 'done', 'sf/r', 'agent', 'plan', 'direct', 26, `+fin+`, 'a5', 'integ'),
		('a6-int', 1, 0, 'done', 'sf/r', 'agent', 'plan', 'direct', 27, `+fin+`, 'a6', 'integ'),
		('a7-int', 1, 0, 'done', 'sf/r', 'agent', 'plan', 'direct', 28, `+fin+`, 'a7', 'integ'),
		('a8-int', 1, 0, 'closed', 'sf/r', 'agent', 'plan', 'direct', 29, `+fin+`, 'a8', 'integ'),
		('a9-int', 1, 0, 'running', 'sf/r', 'agent', 'plan', 'direct', 30, NULL, 'a9', 'integ'),
		('a10-f', 1, 0, 'done', 'sf/r', 'agent', 'plan', 'direct', 31, `+fin+`, 'a10', 'feat'),
		('a11-f', 1, 0, 'done', 'sf/r', 'agent', 'plan', 'direct', 32, `+fin+`, 'a11', 'feat')`); err != nil {
		t.Fatal(err)
	}
	const cards = `"cards": [{"key": "integ", "role": "integration"}, {"key": "feat", "role": "feature"}]`
	criteria := `{"round": 1, "criteria": [{"id": "c1", "kind": "test"}], ` + cards + `}`
	for _, r := range []struct{ id, plan string }{
		{"a1", criteria}, {"a2", `{"round": 1, "outline": null, "criteria": [{"id": "c1", "kind": "test"}], ` + cards + `}`}, {"a3", criteria},
		{"a4", `{"round": 1, "outline": ["the rest"], "criteria": [{"id": "c1", "kind": "test"}], ` + cards + `}`}, {"a5", criteria},
		{"a6", `{"round": 1, "criteria": [], ` + cards + `}`}, {"a7", criteria}, {"a7", `{"round": 1, ` + cards + `}`}, {"a8", criteria}, {"a9", criteria},
		{"a10", criteria}, {"a12", criteria}, {"a13", criteria}, {"a14", criteria}, {"a15", criteria},
		{"a11", `{"round": 1, "criteria": [{"id": "c1", "kind": "test"}], "cards": [{"key": "integ2", "role": "integration"}, {"key": "feat", "role": "feature"}]}`},
	} {
		if _, err := s.AddPlanRevision(ctx, r.id, json.RawMessage(r.plan), r.id+r.plan); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO events (at, repository, task_id, issue_number, kind, actor, via) VALUES
		('2026-10-02T08:00:00Z', 'sf/r', 'a3', 3, 'plan_closed', 'factoryd', 'factoryd')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO events (at, repository, task_id, issue_number, kind, actor, via, detail) VALUES
		('2026-10-02T08:00:00Z', 'sf/r', 'a12', 12, 'criteria_gate_run', 'factoryd', 'factoryd', '{"task": "a12-int", "pr": 0, "outcome": "started"}'),
		('2026-10-02T08:00:00Z', 'sf/r', 'a13', 13, 'criteria_gate_run', 'factoryd', 'factoryd', '{"task": "a13-int", "pr": 9, "outcome": "ran"}')`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PlansAwaitingCriteria(ctx); err != nil || !slices.Equal(got, []string{"a1", "a12", "a15", "a2"}) {
		t.Fatalf("PlansAwaitingCriteria = %v, %v; want [a1 a12 a15 a2]", got, err)
	}
}

// A gate run's rows land on the integration card unless the card whose
// pull request it judged ended undelivered: closed, failed or done with
// nothing merged refuse; open, done with its merge, merged and a run with
// no pull request write. The rows of drop go with the write, the
// approver's stay. A card's end that waits on a write's lock is seen by
// the write: it lands no row after the end.
func TestPutGateVerdictsRefusesACardThatEndedUndelivered(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_gate_verdicts"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const fin = "'2026-10-01T09:00:00Z'"
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, track, issue_number, finished_at, merge_commit) VALUES
		('g-int', 1, 0, 'closed', 'sf/r', 'agent', 'plan', 'direct', 1, `+fin+`, ''), ('g-open', 1, 0, 'in-review', 'sf/r', 'agent', 'plan', 'direct', 2, NULL, ''),
		('g-closed', 1, 0, 'closed', 'sf/r', 'agent', 'plan', 'direct', 3, `+fin+`, ''), ('g-failed', 1, 0, 'failed', 'sf/r', 'agent', 'plan', 'direct', 4, `+fin+`, ''),
		('g-done', 1, 0, 'done', 'sf/r', 'agent', 'plan', 'direct', 5, `+fin+`, ''), ('g-landed', 1, 0, 'done', 'sf/r', 'agent', 'plan', 'direct', 6, `+fin+`, 'm6'),
		('g-merged', 1, 0, 'merged', 'sf/r', 'agent', 'plan', 'direct', 7, `+fin+`, 'm7'), ('g-ending', 1, 0, 'in-review', 'sf/r', 'agent', 'plan', 'direct', 8, NULL, '')`); err != nil {
		t.Fatal(err)
	}
	for writer, want := range map[string]error{"g-open": nil, "g-closed": ErrCardEnded, "g-failed": ErrCardEnded, "g-done": ErrCardEnded,
		"g-landed": nil, "g-merged": nil, "": nil} {
		row := []CriterionVerdict{{Criterion: "c-" + writer, Verdict: "met", Kind: CriterionTest}}
		if err := s.PutGateVerdicts(ctx, writer, "g-int", CriteriaSourceGate, row, nil); !errors.Is(err, want) || (err == nil) != (want == nil) {
			t.Errorf("writer %q: err %v, want %v", writer, err, want)
		}
	}
	rows, err := s.CriteriaVerdicts(ctx, "g-int")
	var got []string
	for _, v := range rows {
		got = append(got, v.Criterion)
	}
	if err != nil || !slices.Equal(got, []string{"c-", "c-g-landed", "c-g-merged", "c-g-open"}) {
		t.Fatalf("rows %v, %v; want the open, landed and merged writers' and the one with no pull request", got, err)
	}
	if err := s.PutCriteriaVerdicts(ctx, "g-int", CriteriaSourceApprover, "sebastian", []CriterionVerdict{{Criterion: "c-g-open", Verdict: "met", Kind: CriterionTest}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutGateVerdicts(ctx, "g-open", "g-int", CriteriaSourceJudgement, []CriterionVerdict{{Criterion: "j1", Verdict: "met", Kind: CriterionJudgement}},
		[]string{"c-g-open", "c-g-landed"}); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.CriteriaVerdicts(ctx, "g-int")
	got = nil
	for _, v := range rows {
		got = append(got, v.Criterion+"/"+v.Source)
	}
	if want := []string{"c-/gate", "c-g-merged/gate", "c-g-open/approver", "j1/judgement"}; !slices.Equal(got, want) {
		t.Fatalf("rows after the drop %v, want %v", got, want)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'closed', finished_at = now() WHERE id = 'g-ending'`); err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	go func() {
		wrote <- s.PutGateVerdicts(ctx, "g-ending", "g-int", CriteriaSourceGate, []CriterionVerdict{{Criterion: "late", Verdict: "met", Kind: CriterionTest}}, nil)
	}()
	select {
	case err := <-wrote:
		t.Fatalf("the write went past the card's open end: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-wrote; !errors.Is(err, ErrCardEnded) {
		t.Fatalf("the write after the end: %v, want ErrCardEnded", err)
	}
}

const planClosedBackfill = "factoryd/20261104100000_plan_closed_backfill.sql"

// The backfill closes, quietly, each plan without criteria whose cards
// are all done or closed: #509, #558, #852 (its failed card was retried
// and let go of its key) and #1305 (its newest revision dropped the
// criteria). #1293 keeps its own close. Open stay #1300 (a queued card),
// #1301 (a failed card), #1302 (a merged card waiting for its chain),
// #1303 (no card yet), #1304 (its newest revision holds criteria) and the
// direct card d-1 with its delegated card done. A second run adds nothing.
// A row says it closed when the plan's newest card finished.
func TestPlanClosedBackfill(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_plan_closed_backfill"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, q := range []string{
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, parent_task, track, plan_key, issue_number, finished_at) VALUES
			('t-509', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 509, now()),
			('c-509a', 1, 0, 'done', 'sf/r', 'agent', 'plan', 't-509', 'direct', 'a', 600, '2026-09-01 10:00+00'),
			('c-509b', 1, 0, 'done', 'sf/r', 'agent', 'plan', 't-509', 'direct', 'b', 601, '2026-09-03 12:00+00'),
			('t-558', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 558, now()),
			('c-558a', 1, 0, 'done', 'sf/r', 'agent', 'plan', 't-558', 'direct', 'a', 602, '2026-09-02 08:00+00'),
			('c-558b', 1, 0, 'closed', 'sf/r', 'agent', 'plan', 't-558', 'direct', 'b', 603, '2026-09-05 08:00+00'),
			('t-852', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 852, now()),
			('c-852old', 1, 0, 'failed', 'sf/r', 'agent', 'plan', 't-852', 'direct', NULL, 604, '2026-08-01 09:00+00'),
			('c-852a', 1, 0, 'done', 'sf/r', 'agent', 'plan', 't-852', 'direct', 'a', 605, '2026-08-02 09:00+00'),
			('t-1293', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 1293, now()),
			('c-1293a', 1, 0, 'done', 'sf/r', 'agent', 'plan', 't-1293', 'direct', 'a', 1294, now()),
			('t-1300', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 1300, now()),
			('c-1300a', 1, 0, 'done', 'sf/r', 'agent', 'plan', 't-1300', 'direct', 'a', 1310, now()),
			('c-1300b', 1, 0, 'queued', 'sf/r', 'agent', 'plan', 't-1300', 'direct', 'b', 1311, NULL),
			('t-1301', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 1301, now()),
			('c-1301a', 1, 0, 'done', 'sf/r', 'agent', 'plan', 't-1301', 'direct', 'a', 1312, now()),
			('c-1301b', 1, 0, 'failed', 'sf/r', 'agent', 'plan', 't-1301', 'direct', 'b', 1313, now()),
			('t-1302', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 1302, now()),
			('c-1302a', 1, 0, 'done', 'sf/r', 'agent', 'plan', 't-1302', 'direct', 'a', 1314, now()),
			('c-1302b', 1, 0, 'merged', 'sf/r', 'agent', 'plan', 't-1302', 'direct', 'b', 1315, now()),
			('t-1303', 1, 0, 'waiting-human', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 1303, NULL),
			('t-1304', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 1304, now()),
			('c-1304a', 1, 0, 'done', 'sf/r', 'agent', 'plan', 't-1304', 'direct', 'a', 1316, now()),
			('t-1305', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 1305, now()),
			('c-1305a', 1, 0, 'done', 'sf/r', 'agent', 'plan', 't-1305', 'direct', 'a', 1317, now()),
			('d-1', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'direct', NULL, 1320, now()),
			('g-1', 1, 0, 'done', 'sf/r', 'agent', 'delegate', 'd-1', 'direct', NULL, 1321, now())`,
		`INSERT INTO plan_revisions (task_id, revision, plan, sha256) VALUES
			('t-509', 1, '{"summary": "links"}', 's1'),
			('t-1304', 1, '{"summary": "x"}', 's2'),
			('t-1304', 2, '{"summary": "x", "criteria": [{"id": "c1", "kind": "invariant", "suite": "./..."}]}', 's3'),
			('t-1305', 1, '{"summary": "y", "criteria": [{"id": "c1", "kind": "invariant", "suite": "./..."}]}', 's4'),
			('t-1305', 2, '{"summary": "y"}', 's5')`,
		`INSERT INTO events (repository, task_id, issue_number, kind, actor, via, detail) VALUES
			('sf/r', 't-1293', 1293, 'plan_closed', 'factoryd', 'factoryd', '{"criteria": 3}')`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	want := []string{
		"t-1293 1293 factoryd factoryd {\"criteria\": 3}",
		"t-1305 1305 factoryd factoryd {\"backfill\": true, \"criteria\": 0}",
		"t-509 509 factoryd factoryd {\"backfill\": true, \"criteria\": 0}",
		"t-558 558 factoryd factoryd {\"backfill\": true, \"criteria\": 0}",
		"t-852 852 factoryd factoryd {\"backfill\": true, \"criteria\": 0}",
	}
	for run := 1; run <= 2; run++ {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name = $1`, planClosedBackfill); err != nil {
			t.Fatal(err)
		}
		if err := schema.Migrate(ctx, s.db); err != nil {
			t.Fatal(err)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT task_id || ' ' || issue_number || ' ' || actor || ' ' || via || ' ' || detail::text
			FROM events WHERE kind = 'plan_closed' ORDER BY task_id`)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				t.Fatal(err)
			}
			got = append(got, r)
		}
		rows.Close()
		if !slices.Equal(got, want) {
			t.Fatalf("run %d: plan_closed rows\n%s\nwant\n%s", run, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		for plan, at := range map[string]string{"t-509": "2026-09-03 12:00:00", "t-558": "2026-09-05 08:00:00", "t-852": "2026-08-02 09:00:00"} {
			var got string
			if err := s.db.QueryRowContext(ctx, `SELECT to_char(at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS') FROM events WHERE task_id = $1 AND kind = 'plan_closed'`, plan).Scan(&got); err != nil || got != at {
				t.Errorf("run %d: %s closed at %q, %v; want its newest card's finish %q", run, plan, got, err, at)
			}
		}
	}
}

// The plans of sf/r with a card in the merge queue: p1's c1 holds a queue
// entry, p2's c2 waits in gate merge-queue outside it (held back for
// another group). p3's c3 is in review out of the queue, p4's c4 is in
// sf/q's queue, p5's c5 failed in gate merge-queue, and d1, a direct card,
// is in the queue but no plan's. p7's c7 waits for its approval out of the
// queue, which its turn re-enters before the merge: it counts too. p8's
// c8 was cut, and its successor s8, which carries its key, waits in gate
// merge-queue.
func TestQueuedPlans(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_queuedplans"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, parent_task, track, plan_key, issue_number, gate, finished_at) VALUES
			('p1', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 1, '', '2026-10-01T00:00:00Z'),
			('c1', 1, 0, 'in-review', 'sf/r', 'agent', 'plan', 'p1', 'direct', 'one', 11, '', NULL),
			('p2', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 2, '', '2026-10-01T00:00:00Z'),
			('c2', 1, 0, 'in-review', 'sf/r', 'agent', 'plan', 'p2', 'direct', 'one', 21, 'merge-queue', NULL),
			('p3', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 3, '', '2026-10-01T00:00:00Z'),
			('c3', 1, 0, 'in-review', 'sf/r', 'agent', 'plan', 'p3', 'direct', 'one', 31, '', NULL),
			('p4', 1, 0, 'done', 'sf/q', 'agent', 'webhook', NULL, 'plan', NULL, 4, '', '2026-10-01T00:00:00Z'),
			('c4', 1, 0, 'in-review', 'sf/q', 'agent', 'plan', 'p4', 'direct', 'one', 41, '', NULL),
			('p5', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 5, '', '2026-10-01T00:00:00Z'),
			('c5', 1, 0, 'failed', 'sf/r', 'agent', 'plan', 'p5', 'direct', 'one', 51, 'merge-queue', '2026-10-01T00:00:00Z'),
			('p6', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 6, '', '2026-10-01T00:00:00Z'),
			('c6', 1, 0, 'merged', 'sf/r', 'agent', 'plan', 'p6', 'direct', 'one', 61, 'merge-queue', '2026-10-01T00:00:00Z'),
			('p7', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 7, '', '2026-10-01T00:00:00Z'),
			('c7', 1, 0, 'in-review', 'sf/r', 'agent', 'plan', 'p7', 'direct', 'one', 71, 'approval', NULL),
			('p8', 1, 0, 'done', 'sf/r', 'agent', 'webhook', NULL, 'plan', NULL, 8, '', '2026-10-01T00:00:00Z'),
			('c8', 1, 0, 'failed', 'sf/r', 'agent', 'plan', 'p8', 'direct', 'one', 81, '', '2026-10-01T00:00:00Z'),
			('s8', 1, 0, 'in-review', 'sf/r', 'agent', 'successor', 'p8', 'direct', 'one', 82, 'merge-queue', NULL),
			('d1', 1, 0, 'in-review', 'sf/r', 'agent', 'webhook', NULL, 'direct', NULL, 9, '', NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, e := range [][2]string{{"sf/r", "c1"}, {"sf/q", "c4"}, {"sf/r", "d1"}} {
		if _, err := s.EnterQueue(ctx, e[0], e[1]); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.QueuedPlans(ctx, "sf/r"); err != nil || !slices.Equal(got, []int64{1, 2, 7, 8}) {
		t.Fatalf("QueuedPlans(sf/r) = %v, %v; want plans #1, #2, #7 and #8", got, err)
	}
}
