package factoryd

import (
	"context"
	"reflect"
	"testing"
	"time"

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
	} {
		if got := GroupOf(tc.card); got != tc.want {
			t.Errorf("%s: GroupOf = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The open groups on main: p1 (three children, two merged, the merge
// events' earliest 09:00 although c3 finished at 09:05) and p5 (a merge
// commit and no event, so its child's finish) in sf/r, p4 (merged by a
// person: an event and no merge commit) in sf/q. p2 has no merged child,
// p3 is closed, d1 is a direct card.
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
			('c7', 1, 0, 'done', 'sf/r', 'agent', 'plan', 'p5', 'direct', 'one', 51, 'ghi', '2026-10-02T00:00:00Z')`,
		`INSERT INTO events (at, repository, task_id, issue_number, kind, actor, via) VALUES
			('2026-10-01T10:00:00Z', 'sf/r', 'c1', 11, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-10-01T09:00:00Z', 'sf/r', 'c3', 13, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-10-01T07:00:00Z', 'sf/r', 'c5', 31, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-10-02T08:00:00Z', 'sf/r', 'p3', 3, 'plan_closed', 'factoryd', 'factoryd'),
			('2026-10-03T12:00:00Z', 'sf/q', 'c6', 41, 'pull_request_merged', 'sebastian', 'webhook'),
			('2026-10-01T06:00:00Z', 'sf/r', 'd1', 9, 'pull_request_merged', 'factory-merge', 'factoryd')`,
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
			{Plan: "p1", Issue: 1, Repository: "sf/r", FirstMerge: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
				Children: map[string]int{"done": 1, "queued": 1, "merged": 1}},
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
}
