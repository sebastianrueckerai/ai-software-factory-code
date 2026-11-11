package factoryd

import (
	"context"
	"testing"
	"time"
)

// Window [Nov 1, Nov 8): prev [Oct 25, Nov 1), spark buckets b0..b6 from
// Sep 20, so the window is b6 and the prev window b5.
//
// Cards of sf/app: A (done Nov 3, 3 cases), B (done Nov 3, 1 case), C
// (running), D (done Nov 5, 2 overlapping cases), E (failed Nov 4, no
// case), F (closed Nov 4, two open cases, one a monitor flag). O is
// sf/other's.
func seedCaseMetrics(t *testing.T, s *PGStore) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{"events", "ledger", "tasks", "cases"} {
		if _, err := s.db.ExecContext(ctx, `TRUNCATE `+table+` CASCADE`); err != nil {
			t.Fatal(err)
		}
	}
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
		('D', 1, 0, 'done', 'sf/app', 'agent', 'webhook', '2026-10-30T08:00Z', '2026-11-05T00:00Z', 'direct'),
		('E', 1, 0, 'failed', 'sf/app', 'agent', 'webhook', '2026-10-30T08:00Z', '2026-11-04T12:00Z', 'direct'),
		('F', 1, 0, 'closed', 'sf/app', 'agent', 'webhook', '2026-10-30T08:00Z', '2026-11-04T12:00Z', 'direct'),
		('O', 1, 0, 'done', 'sf/other', 'agent', 'webhook', '2026-10-30T08:00Z', '2026-11-03T12:00Z', 'direct')`)
	type c struct {
		name, task, repo, kind, status string
		opened, resolved               string
		rung                           int
	}
	cases := []c{
		{"a1", "A", "sf/app", "gate_stop", "resolved", "2026-11-02T08:00Z", "2026-11-02T10:00Z", 0},
		{"a2", "A", "sf/app", "gate_stop", "resolved", "2026-11-02T14:00Z", "2026-11-02T16:00Z", 0},
		{"a3", "A", "sf/app", "gate_stop", "resolved", "2026-11-02T18:00Z", "2026-11-03T08:00Z", 3},
		{"b1", "B", "sf/app", "gate_stop", "resolved", "2026-11-02T08:00Z", "2026-11-02T09:00Z", 1},
		{"c5", "C", "sf/app", "gate_stop", "resolved", "2026-11-04T08:00Z", "2026-11-04T09:00Z", 2},
		{"c6", "C", "sf/app", "gate_stop", "resolved", "2026-11-05T08:00Z", "2026-11-05T09:00Z", 3},
		{"c7", "C", "sf/app", "gate_stop", "resolved", "2026-11-06T08:00Z", "2026-11-06T09:00Z", 3},
		{"c8", "C", "sf/app", "gate_stop", "withdrawn", "2026-11-04T10:00Z", "2026-11-04T11:00Z", 0},
		{"c9", "C", "sf/app", "gate_stop", "resolved", "2026-10-28T08:00Z", "2026-10-28T09:00Z", 0},
		{"cFrom", "C", "sf/app", "gate_stop", "resolved", "2026-10-31T20:00Z", "2026-11-01T00:00Z", 0},
		{"cTo", "C", "sf/app", "gate_stop", "resolved", "2026-11-07T20:00Z", "2026-11-08T00:00Z", 0},
		{"d1", "D", "sf/app", "gate_stop", "resolved", "2026-11-04T08:00Z", "2026-11-04T10:00Z", 1},
		{"d2", "D", "sf/app", "gate_stop", "resolved", "2026-11-04T09:00Z", "2026-11-04T12:00Z", 2},
		{"o1", "O", "sf/other", "gate_stop", "resolved", "2026-11-02T08:00Z", "2026-11-02T20:00Z", 3},
	}
	ids := map[string]int64{}
	for _, k := range cases {
		var id int64
		if err := s.db.QueryRowContext(ctx, `INSERT INTO cases (task_id, repository, kind, state_key, status, problem, opened_at, resolved_at, resolution)
			VALUES ($1, $2, $3, $4, $5, 'p', $6, $7, jsonb_build_object('by', 'x', 'rung', $8::int, 'action', 'a')) RETURNING id`,
			k.task, k.repo, k.kind, k.name, k.status, k.opened, k.resolved, k.rung).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[k.name] = id
	}
	for name, kind := range map[string]string{"f1": "monitor_flag", "f2": "gate_stop"} {
		var id int64
		if err := s.db.QueryRowContext(ctx, `INSERT INTO cases (task_id, repository, kind, state_key, status, problem, opened_at)
			VALUES ('F', 'sf/app', $1, $2, 'open', 'p', '2026-11-04T08:00Z') RETURNING id`, kind, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	step := func(name string, rung int, started string, cost float64) {
		t.Helper()
		must(`INSERT INTO case_steps (case_id, rung, actor, started_at, cost_eur, outcome) VALUES ($1, $2, 'x', $3, $4, 'resolved')`, ids[name], rung, started, cost)
	}
	// a3 reached the human twice: its first step counts
	step("a3", 3, "2026-11-03T06:00Z", 0)
	step("a3", 3, "2026-11-03T07:50Z", 0)
	step("c6", 3, "2026-11-05T08:50Z", 0)
	step("c7", 3, "2026-11-06T08:30Z", 0)
	step("o1", 3, "2026-11-02T08:00Z", 0)
	step("o1", 2, "2026-11-02T08:00Z", 9)
	step("a1", 1, "2026-11-02T08:10Z", 0.12)
	step("a1", 2, "2026-11-02T08:20Z", 0.30)
	step("c8", 2, "2026-11-04T10:30Z", 0.50)
	draw := func(kind, name string, eur float64) {
		t.Helper()
		must(`INSERT INTO events (at, repository, task_id, kind, actor, via, detail) VALUES ('2026-11-02T09:30Z', 'sf/app', 'A', $1, 'factoryd', 'factoryd',
			jsonb_build_object('case_id', $2::bigint, 'amount_eur', $3::float8))`, kind, ids[name], eur)
	}
	draw("card_funded", "a1", 1.5)
	draw("card_extended", "a2", 0.25)
	draw("card_funded", "o1", 7) // another project's case, a resolved one of sf/other
	must(`INSERT INTO events (at, repository, task_id, kind, actor, via, detail) VALUES ('2026-11-02T09:31Z', 'sf/app', 'A', 'card_funded', 'factoryd', 'factoryd', '{"amount_eur": 7}')`)
	ledger := func(task, kind string, eur float64, at string) {
		t.Helper()
		must(`INSERT INTO ledger (task_id, kind, ref, eur, created_at) VALUES ($1, $2, 'big', $3, $4)`, task, kind, eur, at)
	}
	ledger("A", "model", 5, "2026-11-02T09:00Z")
	ledger("A", "model", 0.84, "2026-11-02T11:00Z")
	ledger("A", "tool", 0.10, "2026-11-02T11:10Z")
	ledger("A", "resource", 0.20, "2026-11-02T11:20Z")
	ledger("A", "standing", 0.40, "2026-11-02T11:30Z")
	ledger("A", "model", 6, "2026-11-02T15:00Z")
	ledger("A", "model", 0.60, "2026-11-02T17:00Z")
	ledger("A", "model", 8, "2026-11-03T13:00Z")
	ledger("C", "model", 0.30, "2026-10-28T12:00Z")
	ledger("C", "model", 4, "2026-11-04T11:30Z")
	ledger("D", "model", 0.5, "2026-11-04T11:00Z")
	ledger("D", "model", 0.7, "2026-11-04T13:00Z")
	ledger("O", "model", 3, "2026-11-02T21:00Z")
	return ids
}

func TestMetricsCaseFigures(t *testing.T) {
	s := metricsStore(t)
	seedCaseMetrics(t, s)
	from, to := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	r, err := s.ProjectMetrics(context.Background(), "sf/app", from, to)
	if err != nil {
		t.Fatal(err)
	}
	// Cards that ended in the window: A (3 cases), B (1), D (2), E (none,
	// failed), F (2 open, one a monitor flag, closed) = 8 cases over 5
	// cards, all ended Nov 3 to 5 (b6). O's card and case are sf/other's.
	// No card ended in the prev window.
	if want := (Figure{Value: 8.0 / 5, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 8.0 / 5}}); r.CasesPerCard != want {
		t.Fatalf("cases per card = %+v, want %+v", r.CasesPerCard, want)
	}
	// Cases that ended in [Nov 1, Nov 8): a1 a2 cFrom at rung 0 (cFrom ended
	// exactly at From: inside), b1 d1 at 1, c5 d2 at 2, a3 c6 c7 at 3, c8
	// withdrawn = 11; cTo ended exactly at the window's end: outside, and
	// not in the prev window either. Prev [Oct 25, Nov 1): c9 at rung 0
	// alone. Below the human: 7 of 11 in b6, c9's 1 of 1 in b5.
	wantRung := CasesByRung{Rung: [4]float64{3.0 / 11, 2.0 / 11, 2.0 / 11, 3.0 / 11}, Withdrawn: 1.0 / 11,
		Prev: [5]float64{1, 0, 0, 0, 0}, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 1, 7.0 / 11}}
	if r.CasesByRung != wantRung {
		t.Fatalf("cases by rung = %+v, want %+v", r.CasesByRung, wantRung)
	}
	// Waits at the human: a3 from its first rung 3 step 06:00 to 08:00 =
	// 120 min (its second step at 07:50 does not start the wait), c6 10 min,
	// c7 30 min: median 30 min = 1 800 000 ms.
	if want := (Figure{Value: 1800000, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 1800000}}); r.HumanWait != want {
		t.Fatalf("human wait = %+v, want %+v", r.HumanWait, want)
	}
	// Resolved in the window: a1 a2 a3 b1 c5 c6 c7 cFrom d1 d2 (c8 was
	// withdrawn). Resolution cost: a1's steps 0.12 + 0.30, a1's fund 1.50,
	// a2's card_extended 0.25 = 2.17; the fund naming no case and o1's
	// draw and steps do not count.
	// Spend after: a row counts for the newest resolved case of its card
	// before it, until the card's next case opened or the card ended.
	// A: 0.84 model + 0.10 tool + 0.20 resource after a1, before a2 opened
	// at 14:00 = 1.14, and 0.60 at 17:00 after a2, before a3 opened at 18:00
	// = 1.74 (5 before a1, standing 0.40 is no session spend,
	// 6 at 15:00 lies inside a2, 8 at Nov 3 13:00 is after A ended).
	// C: 4 at Nov 4 11:30 is after c8 opened at 10:00, which cut c5's span.
	// D: d1 (10:00) and d2 (12:00) overlap; 0.5 at 11:00 belongs to d1,
	// 0.7 at 13:00 to d2 only = 1.2. Total 1.74 + 1.2 = 2.94.
	// c8 was withdrawn with a 0.50 step: it is no resolved case, so its
	// cost is not the resolution's. Prev: c9 alone, resolved Oct 28: the
	// 0.30 at Oct 28 12:00 on C has c9 as its newest case and lies before
	// cFrom opened, so Prev[0] = 0.30 (and no window total holds it, as c9
	// resolved before the window); c9's resolution cost 0.
	if want := (CaseMoneyFigure{AfterEUR: 2.94, ResolveEUR: 2.17, Prev: [2]float64{0.30, 0}, Has: true, PrevHas: true}); r.CaseMoney != want {
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
