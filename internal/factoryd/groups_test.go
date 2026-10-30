package factoryd

import (
	"context"
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
	} {
		if got := GroupOf(tc.card); got != tc.want {
			t.Errorf("%s: GroupOf = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A plan card and a plan's child are in a plan; a direct card, a
// delegated one and a person's card are not.
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
// child c9 was taken off main again.
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
			('c9', 1, 0, 'done', 'sf/r', 'agent', 'plan', 'p7', 'direct', 'one', 71, 'stu', '2026-10-01T05:00:00Z')`,
		`UPDATE tasks SET reverted_at = '2026-10-04T09:00:00Z' WHERE id = 'c9'`,
		`INSERT INTO events (at, repository, task_id, issue_number, kind, actor, via) VALUES
			('2026-10-01T10:00:00Z', 'sf/r', 'c1', 11, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-10-01T09:00:00Z', 'sf/r', 'c3', 13, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-10-01T07:00:00Z', 'sf/r', 'c5', 31, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-10-02T08:00:00Z', 'sf/r', 'p3', 3, 'plan_closed', 'factoryd', 'factoryd'),
			('2026-10-03T12:00:00Z', 'sf/q', 'c6', 41, 'pull_request_merged', 'sebastian', 'webhook'),
			('2026-10-01T06:00:00Z', 'sf/r', 'd1', 9, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-09-30T18:00:00Z', 'sf/r', 'c8', 61, 'pull_request_merged', 'factory-merge', 'factoryd'),
			('2026-10-01T04:55:00Z', 'sf/r', 'c9', 71, 'pull_request_merged', 'factory-merge', 'factoryd')`,
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
			('t-1304', 2, '{"summary": "x", "criteria": [{"id": "c1", "kind": "test", "test": "go test ./..."}]}', 's3'),
			('t-1305', 1, '{"summary": "y", "criteria": [{"id": "c1", "kind": "test", "test": "go test ./..."}]}', 's4'),
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
// is in the queue but no plan's.
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
			('d1', 1, 0, 'in-review', 'sf/r', 'agent', 'webhook', NULL, 'direct', NULL, 9, '', NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, e := range [][2]string{{"sf/r", "c1"}, {"sf/q", "c4"}, {"sf/r", "d1"}} {
		if _, err := s.EnterQueue(ctx, e[0], e[1]); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.QueuedPlans(ctx, "sf/r"); err != nil || !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("QueuedPlans(sf/r) = %v, %v; want plans #1 and #2", got, err)
	}
}
