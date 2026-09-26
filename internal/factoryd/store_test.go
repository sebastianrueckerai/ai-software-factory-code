package factoryd

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"
)

// fakeStore is the in-memory Store for tests of everything that persists.
type fakeStore struct {
	tasks    map[string]Task
	sessions map[string]Session
}

func newFakeStore() *fakeStore {
	return &fakeStore{tasks: map[string]Task{}, sessions: map[string]Session{}}
}

func (s *fakeStore) CreateTask(ctx context.Context, t Task) error {
	s.tasks[t.ID] = t
	return nil
}

func (s *fakeStore) Task(ctx context.Context, id string) (Task, bool, error) {
	t, ok := s.tasks[id]
	return t, ok, nil
}

func (s *fakeStore) SetTaskStatus(ctx context.Context, id, status string) error {
	t, ok := s.tasks[id]
	if !ok {
		return nil
	}
	t.Status = status
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

	if err := s.SetTaskStatus(ctx, "t1", "running"); err != nil {
		t.Fatalf("SetTaskStatus: %v", err)
	}
	got, _, _ = s.Task(ctx, "t1")
	if got.Status != "running" {
		t.Fatalf("status = %q", got.Status)
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
