package factoryd

import (
	"context"
	"testing"
	"time"
)

// Window [Nov 1, Nov 8): prev [Oct 25, Nov 1), spark buckets b0..b6 from
// Sep 20, so the window is b6 and the prev window b5.
func seedCaseMetrics(t *testing.T, s *PGStore) {
	t.Helper()
	ctx := context.Background()
	clear := func() {
		for _, table := range []string{"events", "ledger", "tasks", "cases"} {
			if _, err := s.db.ExecContext(ctx, `TRUNCATE `+table+` CASCADE`); err != nil {
				t.Fatal(err)
			}
		}
	}
	clear()
	t.Cleanup(clear)
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	must(`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, created_at, finished_at, track) VALUES
		('A', 1, 0, 'done', 'sf/app', 'agent', 'webhook', '2026-10-30T08:00Z', '2026-11-03T12:00Z', 'direct'),
		('B', 1, 0, 'done', 'sf/app', 'agent', 'webhook', '2026-10-30T08:00Z', '2026-11-03T12:00Z', 'direct'),
		('C', 1, 0, 'running', 'sf/app', 'agent', 'webhook', '2026-10-30T08:00Z', NULL, 'direct'),
		('O', 1, 0, 'done', 'sf/other', 'agent', 'webhook', '2026-10-30T08:00Z', '2026-11-03T12:00Z', 'direct')`)
	// id, task, status, opened, resolved, resolution rung (-1 none)
	type c struct {
		id               int
		task, repo       string
		status           string
		opened, resolved string
		rung             int
	}
	cases := []c{
		{1, "A", "sf/app", "resolved", "2026-11-02T08:00Z", "2026-11-02T10:00Z", 0},
		{2, "A", "sf/app", "resolved", "2026-11-02T14:00Z", "2026-11-02T16:00Z", 0},
		{3, "A", "sf/app", "resolved", "2026-11-02T18:00Z", "2026-11-03T08:00Z", 3},
		{4, "B", "sf/app", "resolved", "2026-11-02T08:00Z", "2026-11-02T09:00Z", 1},
		{5, "C", "sf/app", "resolved", "2026-11-04T08:00Z", "2026-11-04T09:00Z", 2},
		{6, "C", "sf/app", "resolved", "2026-11-05T08:00Z", "2026-11-05T09:00Z", 3},
		{7, "C", "sf/app", "resolved", "2026-11-06T08:00Z", "2026-11-06T09:00Z", 3},
		{8, "C", "sf/app", "withdrawn", "2026-11-04T10:00Z", "2026-11-04T11:00Z", 0},
		{9, "C", "sf/app", "resolved", "2026-10-28T08:00Z", "2026-10-28T09:00Z", 0},
		{10, "O", "sf/other", "resolved", "2026-11-02T08:00Z", "2026-11-02T20:00Z", 3},
	}
	for _, k := range cases {
		must(`INSERT INTO cases (id, task_id, repository, kind, state_key, status, problem, opened_at, resolved_at, resolution)
			VALUES ($1::int, $2, $3, 'gate_stop', 'k' || $1::int, $4, 'p', $5, $6, jsonb_build_object('by', 'x', 'rung', $7::int, 'action', 'a'))`,
			k.id, k.task, k.repo, k.status, k.opened, k.resolved, k.rung)
	}
	// the human waits: case 6 10 min, 7 30 min, 3 120 min, 10 (other) 12 h
	for id, at := range map[int]string{3: "2026-11-03T06:00Z", 6: "2026-11-05T08:50Z", 7: "2026-11-06T08:30Z", 10: "2026-11-02T08:00Z"} {
		must(`INSERT INTO case_steps (case_id, rung, actor, started_at, outcome) VALUES ($1, 3, 'human', $2, 'resolved')`, id, at)
	}
	must(`INSERT INTO case_steps (case_id, rung, actor, cost_eur, outcome) VALUES (1, 1, 'agent', 0.12, 'handed_up'), (1, 2, 'agent', 0.30, 'resolved'), (10, 2, 'agent', 9, 'handed_up')`)
	must(`INSERT INTO events (at, repository, task_id, kind, actor, via, detail) VALUES
		('2026-11-02T09:30Z', 'sf/app', 'A', 'card_funded', 'factoryd', 'factoryd', '{"case_id": 1, "amount_eur": 1.5}'),
		('2026-11-02T09:31Z', 'sf/app', 'A', 'card_funded', 'factoryd', 'factoryd', '{"case_id": 99, "amount_eur": 7}')`)
	// A's spend: before case 1 resolved, after it and before case 2 opened
	// (the one that counts), and inside case 2; sf/other's card spends too
	must(`INSERT INTO ledger (task_id, kind, ref, eur, created_at) VALUES
		('A', 'model', 'big', 5, '2026-11-02T09:00Z'),
		('A', 'model', 'big', 0.84, '2026-11-02T11:00Z'),
		('A', 'model', 'big', 6, '2026-11-02T15:00Z'),
		('O', 'model', 'big', 3, '2026-11-02T21:00Z')`)
}

func TestMetricsCaseFigures(t *testing.T) {
	s := metricsStore(t)
	seedCaseMetrics(t, s)
	from, to := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	r, err := s.ProjectMetrics(context.Background(), "sf/app", from, to)
	if err != nil {
		t.Fatal(err)
	}
	// A has 3 cases, B 1, over 2 cards ended Nov 3 (b6); no card ended in prev
	if want := (Figure{Value: 2, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 2}}); r.CasesPerCard != want {
		t.Fatalf("cases per card = %+v, want %+v", r.CasesPerCard, want)
	}
	// ended in the window: cases 1 2 at rung 0, 4 at 1, 5 at 2, 3 6 7 at 3,
	// 8 withdrawn: 8 cases. Prev: case 9 at rung 0. Weeks: resolved below
	// the human 4 of 8 in b6, 1 of 1 in b5
	wantRung := CasesByRung{Rung: [4]float64{0.25, 0.125, 0.125, 0.375}, Withdrawn: 0.125,
		Prev: [5]float64{1, 0, 0, 0, 0}, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 1, 0.5}}
	if r.CasesByRung != wantRung {
		t.Fatalf("cases by rung = %+v, want %+v", r.CasesByRung, wantRung)
	}
	// waits of cases 6, 7 and 3: 10, 30 and 120 min, median 30 min
	if want := (Figure{Value: 1800000, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 1800000}}); r.HumanWait != want {
		t.Fatalf("human wait = %+v, want %+v", r.HumanWait, want)
	}
	// case 1: steps 0.12 + 0.30 and the 1.50 fund naming it; A's 0.84
	// between its resolution and case 2's opening; the prev case costs nothing
	if want := (CaseMoneyFigure{AfterEUR: 0.84, ResolveEUR: 1.92, Has: true}); r.CaseMoney != want {
		t.Fatalf("case money = %+v, want %+v", r.CaseMoney, want)
	}

	empty, err := s.ProjectMetrics(context.Background(), "sf/app", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 7, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if empty.CasesPerCard.Has || empty.CasesByRung.Has || empty.HumanWait.Has || empty.CaseMoney.Has {
		t.Fatalf("empty window = %+v %+v %+v %+v", empty.CasesPerCard, empty.CasesByRung, empty.HumanWait, empty.CaseMoney)
	}
}
