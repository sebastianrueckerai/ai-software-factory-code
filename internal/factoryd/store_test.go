package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sort"
	"testing"
	"time"
)

// fromJSON must surface a decode failure rather than silently returning an
// empty slice, so corrupt required_capabilities/findings columns fail the
// read instead of quietly discarding data (fix round 1).
func TestFromJSONSurfacesDecodeErrors(t *testing.T) {
	if _, err := fromJSON([]byte(`not json`)); err == nil {
		t.Fatal("want an error for malformed JSON, got nil")
	}
	out, err := fromJSON(nil)
	if err != nil || out != nil {
		t.Fatalf("fromJSON(nil) = %v, %v, want nil, nil", out, err)
	}
	out, err = fromJSON([]byte(`["a","b"]`))
	if err != nil || !slices.Equal(out, []string{"a", "b"}) {
		t.Fatalf("fromJSON = %v, %v, want [a b], nil", out, err)
	}
}

// fakeStore is the in-memory Store for tests of everything that persists.
type fakeStore struct {
	tasks    map[string]Task
	sessions map[string]Session
	elapsed  map[string]time.Duration // what the store's clock reports at the next claim
	finished map[string]time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{tasks: map[string]Task{}, sessions: map[string]Session{},
		elapsed: map[string]time.Duration{}, finished: map[string]time.Time{}}
}

func (s *fakeStore) CreateTask(ctx context.Context, t Task) error {
	s.tasks[t.ID] = t
	return nil
}

func (s *fakeStore) Task(ctx context.Context, id string) (Task, bool, error) {
	t, ok := s.tasks[id]
	return t, ok, nil
}

func (s *fakeStore) TransitionTask(ctx context.Context, id string, from []string, to string) error {
	t, ok := s.tasks[id]
	if !ok || !slices.Contains(from, t.Status) {
		return fmt.Errorf("task %s: not found (not in %v, cannot move to %s)", id, from, to)
	}
	t.Status = to
	s.tasks[id] = t
	if to == "done" || to == "failed" {
		s.finished[id] = time.Now()
	}
	return nil
}

func (s *fakeStore) ClaimTask(ctx context.Context, id string) (time.Duration, bool, error) {
	t, ok := s.tasks[id]
	if !ok || t.Status != "queued" {
		return 0, false, nil
	}
	t.Status = "running"
	s.tasks[id] = t
	return s.elapsed[id], true, nil
}

func (s *fakeStore) RecentlyFinishedTasks(ctx context.Context, within time.Duration) ([]string, error) {
	var ids []string
	for id, at := range s.finished {
		if st := s.tasks[id].Status; (st == "done" || st == "failed") && time.Since(at) < within {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *fakeStore) SettleSpend(ctx context.Context, id string, reported float64) error {
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task %s: not found", id)
	}
	t.SpentEUR = max(t.SpentEUR, reported)
	s.tasks[id] = t
	return nil
}

func (s *fakeStore) ListTasksByStatus(ctx context.Context, status string) ([]Task, error) {
	var tasks []Task
	for _, t := range s.tasks {
		if t.Status == status {
			tasks = append(tasks, t)
		}
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Priority != tasks[j].Priority {
			return tasks[i].Priority > tasks[j].Priority
		}
		return tasks[i].ID < tasks[j].ID
	})
	return tasks, nil
}

func (s *fakeStore) InsertSession(ctx context.Context, sess Session) error {
	s.sessions[sess.ID] = sess
	return nil
}

func (s *fakeStore) Session(ctx context.Context, id string) (Session, bool, error) {
	sess, ok := s.sessions[id]
	return sess, ok, nil
}

func (s *fakeStore) UpdateSession(ctx context.Context, id string, findings []string, restartState []byte) error {
	sess, ok := s.sessions[id]
	if !ok {
		return nil
	}
	sess.Findings = findings
	sess.RestartState = restartState
	s.sessions[id] = sess
	return nil
}

var _ Store = (*fakeStore)(nil)

// Requires a disposable Postgres: FACTORYD_TEST_DATABASE_URL must point at a
// database this test may wipe. Skipped otherwise; CI stays hermetic.
func TestPGStore(t *testing.T) {
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
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// Booting twice must be safe: migrations are idempotent.
	s2, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("second OpenStore: %v", err)
	}
	defer s2.db.Close()

	in := Task{
		ID:                   "t1",
		Priority:             2,
		RequiredCapabilities: []string{"repository.write", "shell"},
		RiskClass:            "high",
		BudgetEUR:            10,
		Status:               "queued",
		Objective:            "Add the runner",
		Repository:           "sf/ai-software-factory-code",
		SHA:                  "abc123",
		Branch:               "factory/t1",
	}
	if err := s.CreateTask(ctx, in); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	got, ok, err := s.Task(ctx, "t1")
	if err != nil || !ok {
		t.Fatalf("Task = (%v, %v), want (true, nil)", ok, err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("task = %+v, want %+v", got, in)
	}

	elapsed, claimed, err := s.ClaimTask(ctx, "t1")
	if err != nil || !claimed || elapsed != 0 {
		t.Fatalf("first claim = (%v, %v, %v), want (0, true, nil): now() is the start", elapsed, claimed, err)
	}
	if _, claimed, err := s.ClaimTask(ctx, "t1"); err != nil || claimed {
		t.Fatalf("second claim = (%v, %v), want (false, nil): a running task must not be claimed twice", claimed, err)
	}
	got, _, _ = s.Task(ctx, "t1")
	if got.Status != "running" {
		t.Fatalf("status = %q", got.Status)
	}
	if err := s.TransitionTask(ctx, "t1", []string{"queued"}, "done"); err == nil {
		t.Fatal("a running task moved from queued")
	}
	if err := s.TransitionTask(ctx, "nope", []string{"running"}, "closing"); err == nil {
		t.Fatal("TransitionTask on a missing task must fail")
	}
	for _, step := range [][2]string{{"running", "closing"}, {"closing", "queued"}} {
		if err := s.TransitionTask(ctx, "t1", []string{step[0]}, step[1]); err != nil {
			t.Fatalf("TransitionTask %s -> %s: %v", step[0], step[1], err)
		}
	}
	// a requeued task keeps its first start: the wall-clock cap spans runs,
	// measured on the database's clock alone
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET started_at = now() - interval '90 minutes' WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	again, claimed, err := s.ClaimTask(ctx, "t1")
	// 90 min plus the gap between the two statements
	if err != nil || !claimed || again < 90*time.Minute || again > 91*time.Minute {
		t.Fatalf("reclaim = (%v, %v, %v), want (1h30m plus under a minute, true, nil)", again, claimed, err)
	}
	for _, step := range [][2]string{{"running", "closing"}, {"closing", "queued"}} {
		if err := s.TransitionTask(ctx, "t1", []string{step[0]}, step[1]); err != nil {
			t.Fatalf("TransitionTask %s -> %s: %v", step[0], step[1], err)
		}
	}

	// spend settlement: finished tasks inside the window, raised to at least
	// LiteLLM's figure on every call, never lowered
	for _, row := range []struct {
		id     string
		status string
	}{{"s-done", "done"}, {"s-failed", "failed"}, {"s-old", "done"}, {"s-run", "running"}} {
		if err := s.CreateTask(ctx, Task{ID: row.id, BudgetEUR: 1, SpentEUR: 0.5, Status: "closing"}); err != nil {
			t.Fatal(err)
		}
		if err := s.TransitionTask(ctx, row.id, []string{"closing"}, row.status); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET finished_at = now() - interval '2 hours' WHERE id = 's-old'`); err != nil {
		t.Fatal(err)
	}
	ids, err := s.RecentlyFinishedTasks(ctx, time.Hour)
	sort.Strings(ids)
	if err != nil || !reflect.DeepEqual(ids, []string{"s-done", "s-failed"}) {
		t.Fatalf("recently finished = (%v, %v), want [s-done s-failed]", ids, err)
	}
	for _, step := range []struct {
		id       string
		reported float64
	}{{"s-done", 0.75}, {"s-failed", 0.25}, {"s-done", 0.625}, {"s-done", 0.875}} {
		if err := s.SettleSpend(ctx, step.id, step.reported); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]float64{"s-done": 0.875, "s-failed": 0.5} {
		if got, _, _ := s.Task(ctx, id); got.SpentEUR != want {
			t.Fatalf("%s spent = %v, want %v (max of booked and every LiteLLM figure)", id, got.SpentEUR, want)
		}
	}

	if err := s.CreateTask(ctx, Task{ID: "t0", Priority: 1, BudgetEUR: 1, Status: "queued", Objective: "x"}); err != nil {
		t.Fatalf("CreateTask t0: %v", err)
	}
	queued, err := s.ListTasksByStatus(ctx, "queued")
	if err != nil {
		t.Fatalf("ListTasksByStatus: %v", err)
	}
	if len(queued) != 2 || queued[0].ID != "t1" || queued[0].Objective != "Add the runner" {
		t.Fatalf("queued = %+v, want t1 (priority 2, brief round-tripped) before t0", queued)
	}

	_, ok, err = s.Task(ctx, "nope")
	if err != nil || ok {
		t.Fatalf("unknown task = (%v, %v), want (false, nil)", ok, err)
	}

	restart, _ := json.Marshal(map[string]any{"last_step": 3})
	when := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	sessIn := Session{ID: "s1", TaskID: "t1", Findings: []string{"repo cloned"}, RestartState: restart, CreatedAt: when}
	if err := s.InsertSession(ctx, sessIn); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	sess, ok, err := s.Session(ctx, "s1")
	if err != nil || !ok {
		t.Fatalf("Session = (%v, %v), want (true, nil)", ok, err)
	}
	var rs map[string]any
	if err := json.Unmarshal(sess.RestartState, &rs); err != nil {
		t.Fatalf("restart_state is not JSON: %v", err)
	}
	if sess.TaskID != "t1" || !reflect.DeepEqual(sess.Findings, []string{"repo cloned"}) ||
		!reflect.DeepEqual(rs, map[string]any{"last_step": float64(3)}) {
		t.Fatalf("session = %+v restart=%v", sess, rs)
	}

	restart2, _ := json.Marshal(map[string]any{"last_step": 7})
	if err := s.UpdateSession(ctx, "s1", []string{"repo cloned", "tests green"}, restart2); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	sess, _, _ = s.Session(ctx, "s1")
	rs = nil
	json.Unmarshal(sess.RestartState, &rs)
	if len(sess.Findings) != 2 || !reflect.DeepEqual(rs, map[string]any{"last_step": float64(7)}) {
		t.Fatalf("updated session = %+v restart=%v", sess, rs)
	}

	// Order independence: a task row created the way the gateway seeds one
	// (only its columns) must survive factoryd's boot and gain the defaults.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, budget_eur, spent_eur, status) VALUES ('t-gw', 5, 0, 'running')`,
	); err != nil {
		t.Fatalf("gateway-style seed: %v", err)
	}
	if err := s2.migrate(ctx); err != nil {
		t.Fatalf("migrate over gateway-created row: %v", err)
	}
	gw, ok, err := s2.Task(ctx, "t-gw")
	if err != nil || !ok || gw.Priority != 0 || gw.RiskClass != "standard" || len(gw.RequiredCapabilities) != 0 {
		t.Fatalf("gateway-seeded task = %+v (%v, %v)", gw, ok, err)
	}
}
