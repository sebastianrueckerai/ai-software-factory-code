package factoryd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
)

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL), like TestPGStore.
func TestPGStoreBoardTasks(t *testing.T) {
	dsn := os.Getenv("FACTORYD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("FACTORYD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	for _, tk := range []Task{
		{ID: "low", Priority: 0, Status: "queued"},
		{ID: "high", Priority: 3, Status: "running"},
		{ID: "recent", Status: "done"},
		{ID: "old", Status: "failed"},
	} {
		tk.BudgetEUR, tk.Repository, tk.Kind = 1, "sf/r", "agent"
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask %s: %v", tk.ID, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET finished_at = now() - interval '1 day' WHERE id = 'recent';
		UPDATE tasks SET finished_at = now() - interval '8 days' WHERE id = 'old'`); err != nil {
		t.Fatalf("stamp finished_at: %v", err)
	}
	// the board reads an escalation's reason from the row the escalation
	// wrote, the latest one, not from the timeline object
	for _, body := range []string{"old reason", "cannot reach the database"} {
		if _, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "high", Kind: IntakeEscalation, Author: "factoryd", Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "low", Kind: "comment", Author: "sebastian", Body: "not an escalation"}); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.BoardTasks(ctx, time.Now().Add(-7*24*time.Hour), 10)
	if err != nil {
		t.Fatalf("BoardTasks: %v", err)
	}
	var ids, reasons []string
	for _, tk := range tasks {
		ids = append(ids, tk.ID)
		reasons = append(reasons, tk.Escalation)
	}
	if want := []string{"high", "low", "recent"}; !slices.Equal(ids, want) {
		t.Fatalf("BoardTasks = %v, want %v", ids, want)
	}
	if want := []string{"cannot reach the database", "", ""}; !slices.Equal(reasons, want) {
		t.Fatalf("reasons = %q, want %q", reasons, want)
	}
	// bounded, open tasks first
	tasks, err = s.BoardTasks(ctx, time.Now().Add(-7*24*time.Hour), 2)
	if err != nil || len(tasks) != 2 || tasks[0].ID != "high" || tasks[1].ID != "low" {
		t.Fatalf("BoardTasks limit 2 = %+v, %v", tasks, err)
	}
}

// A task row carries when the work was asked for and the gate it is in;
// alerts are raised by the machine and a human alert takes one verdict.
func TestPGStoreGatesAndAlerts(t *testing.T) {
	dsn := os.Getenv("FACTORYD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("FACTORYD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events, alerts`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	asked := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	for _, tk := range []Task{
		{ID: "issue", Status: "queued", Kind: "agent", CreatedAt: asked},
		{ID: "api", Status: "queued", Kind: "agent"},
		{ID: "hand", Status: "waiting-human", Kind: "human", CreatedAt: asked},
	} {
		tk.BudgetEUR, tk.Repository = 1, "sf/r"
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask %s: %v", tk.ID, err)
		}
	}
	got, _, err := s.Task(ctx, "issue")
	if err != nil || !got.CreatedAt.Equal(asked) || got.Gate != GateIntake || got.GateSince == nil || !got.GateSince.Equal(asked) || got.FinishedAt != nil {
		t.Fatalf("issue task = created %s, gate %q since %v, finished %v, %v; want the issue's time, intake since then", got.CreatedAt, got.Gate, got.GateSince, got.FinishedAt, err)
	}
	if got, _, _ := s.Task(ctx, "api"); time.Since(got.CreatedAt) > time.Minute || got.Gate != GateIntake || !got.GateSince.Equal(got.CreatedAt) {
		t.Fatalf("api task = created %s, gate %q since %v; want now, intake since then", got.CreatedAt, got.Gate, got.GateSince)
	}
	if got, _, _ := s.Task(ctx, "hand"); got.Gate != "" || got.GateSince != nil {
		t.Fatalf("human task gate = %q since %v, want none", got.Gate, got.GateSince)
	}
	checks := asked.Add(25 * time.Minute)
	if err := s.SetGate(ctx, "issue", GateChecks, checks); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "issue"); got.Gate != GateChecks || !got.GateSince.Equal(checks) {
		t.Fatalf("after SetGate: gate %q since %v", got.Gate, got.GateSince)
	}
	if err := s.TransitionTask(ctx, "issue", []string{"queued"}, "done"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "issue"); got.Gate != "" || got.GateSince != nil || got.FinishedAt == nil {
		t.Fatalf("finished task: gate %q since %v, finished %v; want no gate, a finish time", got.Gate, got.GateSince, got.FinishedAt)
	}
	// a finished task enters no gate again
	if err := s.SetGate(ctx, "issue", GateMerge, checks); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "issue"); got.Gate != "" {
		t.Fatalf("finished task re-entered gate %q", got.Gate)
	}

	bound, err := s.InsertAlert(ctx, Alert{Kind: AlertGateBound, Source: "WaitChecks", TaskID: "api", BoundS: 600,
		Detail: map[string]any{"attempt": 1}})
	if err != nil {
		t.Fatal(err)
	}
	trip, err := s.InsertAlert(ctx, Alert{Kind: AlertSupervisorTrip, Source: "repetition", TaskID: "api", Verdict: VerdictNoise,
		ResolvedBy: "supervisor", Detail: map[string]any{"action": "continue"}})
	if err != nil {
		t.Fatal(err)
	}
	esc, err := s.InsertAlert(ctx, Alert{Kind: AlertEscalation, Source: "agent", TaskID: "api", Detail: map[string]any{"comment": 12}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertAlert(ctx, Alert{Kind: "bogus", Source: "x"}); err == nil {
		t.Fatal("an unknown alert kind was stored")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'waiting-human' WHERE id = 'api'`); err != nil {
		t.Fatal(err)
	}
	board, err := s.BoardTasks(ctx, time.Now().Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range board {
		if b.ID == "api" && b.EscalationAlert != esc {
			t.Fatalf("board escalation alert = %d, want %d", b.EscalationAlert, esc)
		}
	}
	alerts, err := s.Alerts(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, a := range alerts {
		ids = append(ids, a.ID)
	}
	if !slices.Equal(ids, []int64{esc, trip, bound}) {
		t.Fatalf("alerts = %v, want newest first %v", ids, []int64{esc, trip, bound})
	}
	b, tr := alerts[2], alerts[1]
	if b.BoundS != 600 || b.Verdict != "" || b.ResolvedAt != nil || b.Detail["attempt"] != float64(1) || b.Source != "WaitChecks" {
		t.Fatalf("gate_bound alert = %+v", b)
	}
	if tr.Verdict != VerdictNoise || tr.ResolvedBy != "supervisor" || tr.ResolvedAt == nil || tr.BoundS != 0 {
		t.Fatalf("supervisor trip = %+v; want resolved at once as noise by the supervisor", tr)
	}
	if err := s.SetAlertVerdict(ctx, esc, VerdictReal, "sebastian"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAlertVerdict(ctx, esc, VerdictNoise, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second verdict = %v, want ErrNotFound", err)
	}
	if err := s.SetAlertVerdict(ctx, bound, VerdictNoise, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a human verdict on a machine alert = %v, want ErrNotFound", err)
	}
	alerts, _ = s.Alerts(ctx, 1)
	if a := alerts[0]; len(alerts) != 1 || a.Verdict != VerdictReal || a.ResolvedBy != "sebastian" || a.ResolvedAt == nil {
		t.Fatalf("escalation after the verdict = %+v", alerts)
	}
	board, _ = s.BoardTasks(ctx, time.Now().Add(-time.Hour), 10)
	for _, b := range board {
		if b.ID == "api" && b.EscalationAlert != 0 {
			t.Fatalf("board still offers the judged alert %d", b.EscalationAlert)
		}
	}
}

// The gate supervisor judges an open gate_bound alert once, its action
// merged into the detail, and reads the task's newest gateway audit rows.
func TestPGStoreResolveAlertAndAuditRows(t *testing.T) {
	dsn := os.Getenv("FACTORYD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("FACTORYD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE alerts, audit`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	bound, err := s.InsertAlert(ctx, Alert{Kind: AlertGateBound, Source: "WaitChecks", TaskID: "t1", BoundS: 30, Detail: map[string]any{"attempt": 1}})
	if err != nil {
		t.Fatal(err)
	}
	esc, err := s.InsertAlert(ctx, Alert{Kind: AlertEscalation, Source: "agent", TaskID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveAlert(ctx, bound, VerdictNoise, "supervisor", map[string]any{"action": "wait", "diagnosis": "checks run"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveAlert(ctx, bound, VerdictReal, "supervisor", map[string]any{"action": "cancel"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second verdict err = %v, want ErrNotFound", err)
	}
	if err := s.ResolveAlert(ctx, esc, VerdictReal, "supervisor", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("verdict on an escalation err = %v, want ErrNotFound: a human judges it", err)
	}
	alerts, err := s.Alerts(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	a := alerts[1]
	if a.ID != bound || a.Verdict != VerdictNoise || a.ResolvedBy != "supervisor" || a.ResolvedAt == nil ||
		fmt.Sprint(a.Detail) != "map[action:wait attempt:1 diagnosis:checks run]" {
		t.Fatalf("alert = %+v", a)
	}

	for _, row := range []string{
		`('t1', '2026-09-28T10:00:00Z', 'allow', 'ok', 'coder', '', 0.012)`,
		`('t2', '2026-09-28T10:01:00Z', 'allow', 'ok', 'coder', '', 0.5)`,
		`('t1', '2026-09-28T10:02:00Z', 'deny', 'budget exhausted', 'review-frontier', '', NULL)`,
	} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO audit (task_id, ts, decision, reason, model_alias, tool, spend_eur) VALUES `+row); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.AuditRows(ctx, "t1", 20)
	if err != nil {
		t.Fatal(err)
	}
	spend := 0.012
	want := []AuditRow{{Time: time.Date(2026, 9, 28, 10, 2, 0, 0, time.UTC), Decision: "deny", Reason: "budget exhausted", Model: "review-frontier"},
		{Time: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC), Decision: "allow", Reason: "ok", Model: "coder", SpendEUR: &spend}}
	if jsonOf(rows) != jsonOf(want) {
		t.Fatalf("audit rows = %s, want %s", jsonOf(rows), jsonOf(want))
	}
}
