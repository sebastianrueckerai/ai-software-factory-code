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
	tasks        map[string]Task
	sessions     map[string]Session
	started      map[string]bool
	finished     map[string]time.Time
	intakeEvents []IntakeEvent
	deliveries   map[string]bool
	pruned       []time.Duration
}

func newFakeStore() *fakeStore {
	return &fakeStore{tasks: map[string]Task{}, sessions: map[string]Session{},
		started: map[string]bool{}, finished: map[string]time.Time{}}
}

func (s *fakeStore) CreateTask(ctx context.Context, t Task) error {
	if t.Kind == "" {
		t.Kind = "agent"
	}
	s.tasks[t.ID] = t
	return nil
}

func (s *fakeStore) Task(ctx context.Context, id string) (Task, bool, error) {
	t, ok := s.tasks[id]
	return t, ok, nil
}

func (s *fakeStore) TaskByIssue(ctx context.Context, repository string, issueNumber int64) (Task, bool, error) {
	for _, t := range s.tasks {
		if t.Repository == repository && t.IssueNumber == issueNumber && issueNumber != 0 {
			return t, true, nil
		}
	}
	return Task{}, false, nil
}

func (s *fakeStore) InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error) {
	for _, have := range s.intakeEvents {
		if (e.DeliveryID != "" && have.DeliveryID == e.DeliveryID) ||
			(e.CommentID != 0 && have.TaskID == e.TaskID && have.Kind == e.Kind && have.CommentID == e.CommentID) {
			return 0, nil
		}
	}
	e.ID = int64(len(s.intakeEvents) + 1)
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	s.intakeEvents = append(s.intakeEvents, e)
	return e.ID, nil
}

func (s *fakeStore) IntakeEvents(ctx context.Context, taskID string) ([]IntakeEvent, error) {
	var out []IntakeEvent
	for _, e := range s.intakeEvents {
		if e.TaskID == taskID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *fakeStore) MarkSignalled(ctx context.Context, id int64) error {
	for i := range s.intakeEvents {
		if s.intakeEvents[i].ID == id {
			s.intakeEvents[i].Signalled = true
			return nil
		}
	}
	return fmt.Errorf("intake event %d: no such row", id)
}

func (s *fakeStore) UnaddressedChangeRequests(ctx context.Context, taskID string) ([]IntakeEvent, error) {
	var latest time.Time
	for _, sess := range s.sessions {
		if sess.TaskID == taskID && sess.CreatedAt.After(latest) {
			latest = sess.CreatedAt
		}
	}
	var out []IntakeEvent
	for _, e := range s.intakeEvents {
		if e.TaskID == taskID && e.Signalled && slices.Contains(changeRequestKinds, e.Kind) && e.CreatedAt.After(latest) {
			out = append(out, e)
		}
	}
	return out, nil
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

func (s *fakeStore) MarkStarted(ctx context.Context, id string) error {
	t, ok := s.tasks[id]
	if !ok || !slices.Contains([]string{"queued", "running", "in-review", "waiting-human"}, t.Status) {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	t.Status = "running"
	s.tasks[id] = t
	s.started[id] = true
	return nil
}

func (s *fakeStore) SetTaskPR(ctx context.Context, id string, number int64) error {
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	t.PRNumber = number
	s.tasks[id] = t
	return nil
}

func (s *fakeStore) MarkHalfWay(ctx context.Context, id string) error {
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	t.HalfWayDone = true
	s.tasks[id] = t
	return nil
}

func (s *fakeStore) DeliverySeen(ctx context.Context, id string) (bool, error) {
	return s.deliveries[id], nil
}

func (s *fakeStore) RecordDelivery(ctx context.Context, id string) error {
	if s.deliveries == nil {
		s.deliveries = map[string]bool{}
	}
	s.deliveries[id] = true
	return nil
}

func (s *fakeStore) PruneDeliveries(ctx context.Context, age time.Duration) error {
	s.pruned = append(s.pruned, age)
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
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events`); err != nil {
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
		Kind:                 "agent",
		IssueNumber:          42,
		Source:               "api",
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
	byIssue, ok, err := s.TaskByIssue(ctx, "sf/ai-software-factory-code", 42)
	if err != nil || !ok || byIssue.ID != "t1" {
		t.Fatalf("TaskByIssue = (%+v, %v, %v), want (t1, true, nil)", byIssue, ok, err)
	}
	if _, ok, err := s.TaskByIssue(ctx, "sf/ai-software-factory-code", 999); err != nil || ok {
		t.Fatalf("TaskByIssue unknown issue = (%v, %v), want (false, nil)", ok, err)
	}
	if id, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t1", Kind: "issue_comment", Author: "sebastian", Body: "looks good"}); err != nil || id == 0 {
		t.Fatalf("InsertIntakeEvent = (%d, %v)", id, err)
	}
	// a comment is recorded once per task and kind, whichever path saw it first
	commentEvent, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t1", Kind: "pull_request_comment", Author: "sebastian", Body: "nit", CommentID: 80, Trusted: true})
	if err != nil || commentEvent == 0 {
		t.Fatalf("InsertIntakeEvent comment = (%d, %v)", commentEvent, err)
	}
	if again, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t1", Kind: "pull_request_comment", Author: "sebastian", Body: "nit", CommentID: 80, DeliveryID: "d-80"}); err != nil || again != 0 {
		t.Fatalf("InsertIntakeEvent same comment = (%d, %v), want 0", again, err)
	}
	if err := s.MarkSignalled(ctx, commentEvent); err != nil {
		t.Fatal(err)
	}
	events, err := s.IntakeEvents(ctx, "t1")
	if err != nil || len(events) != 2 || events[1].CommentID != 80 || !events[1].Trusted || !events[1].Signalled || events[0].Signalled {
		t.Fatalf("IntakeEvents = %+v, %v", events, err)
	}
	// unaddressed: signalled change requests newer than the latest session
	if got, err := s.UnaddressedChangeRequests(ctx, "t1"); err != nil || len(got) != 1 || got[0].ID != commentEvent {
		t.Fatalf("UnaddressedChangeRequests before any session = %+v, %v", got, err)
	}
	if err := s.InsertSession(ctx, Session{ID: "s-after", TaskID: "t1", CreatedAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.UnaddressedChangeRequests(ctx, "t1"); err != nil || len(got) != 0 {
		t.Fatalf("UnaddressedChangeRequests after a session = %+v, %v, want none", got, err)
	}
	if seen, err := s.DeliverySeen(ctx, "d-1"); err != nil || seen {
		t.Fatalf("unseen delivery = (%v, %v)", seen, err)
	}
	for range 2 {
		if err := s.RecordDelivery(ctx, "d-1"); err != nil {
			t.Fatalf("RecordDelivery: %v", err)
		}
	}
	if seen, err := s.DeliverySeen(ctx, "d-1"); err != nil || !seen {
		t.Fatalf("recorded delivery = (%v, %v), want seen", seen, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE webhook_deliveries SET received_at = now() - interval '31 days' WHERE id = 'd-1'`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDelivery(ctx, "d-2"); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneDeliveries(ctx, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"d-1": false, "d-2": true} {
		if seen, _ := s.DeliverySeen(ctx, id); seen != want {
			t.Fatalf("after prune %s seen = %v, want %v", id, seen, want)
		}
	}
	for i := range 2 {
		id, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t1", Kind: "issue_comment", Author: "sebastian", Body: "again", DeliveryID: "d-9"})
		if err != nil || (id == 0) != (i == 1) {
			t.Fatalf("InsertIntakeEvent with delivery, round %d = (%d, %v)", i, id, err)
		}
	}
	if err := s.SetTaskPR(ctx, "t1", 12); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "t1"); got.PRNumber != 12 || got.HalfWayDone {
		t.Fatalf("pr_number = %d, half_way_done = %v, want 12 and false", got.PRNumber, got.HalfWayDone)
	}
	if err := s.MarkHalfWay(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "t1"); !got.HalfWayDone {
		t.Fatal("half_way_done not kept")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET pr_number = 0 WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	var intakeCount int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM intake_events WHERE task_id = 't1'`).Scan(&intakeCount); err != nil {
		t.Fatal(err)
	}
	if intakeCount != 3 {
		t.Fatalf("intake_events for t1 = %d, want 3 (the redelivered d-9 and the re-recorded comment 80 count once each)", intakeCount)
	}

	if err := s.MarkStarted(ctx, "t1"); err != nil {
		t.Fatalf("MarkStarted: %v", err)
	}
	var first time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT started_at FROM tasks WHERE id = 't1'`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{{"running", "in-review"}} {
		if err := s.TransitionTask(ctx, "t1", []string{step[0]}, step[1]); err != nil {
			t.Fatalf("TransitionTask %s -> %s: %v", step[0], step[1], err)
		}
	}
	if err := s.MarkStarted(ctx, "t1"); err != nil {
		t.Fatalf("MarkStarted from in-review: %v", err)
	}
	if err := s.TransitionTask(ctx, "t1", []string{"running"}, "waiting-human"); err != nil {
		t.Fatalf("TransitionTask running -> waiting-human: %v", err)
	}
	if err := s.MarkStarted(ctx, "t1"); err != nil {
		t.Fatalf("MarkStarted from waiting-human (a human answered): %v", err)
	}
	var again time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT started_at FROM tasks WHERE id = 't1'`).Scan(&again); err != nil {
		t.Fatal(err)
	}
	if !again.Equal(first) {
		t.Fatalf("started_at moved from %v to %v: the first start is kept", first, again)
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
	if err := s.CreateTask(ctx, Task{ID: "t0", Priority: 1, BudgetEUR: 1, Status: "running", Objective: "x"}); err != nil {
		t.Fatalf("CreateTask t0: %v", err)
	}
	running, err := s.ListTasksByStatus(ctx, "running")
	if err != nil || len(running) != 2 || running[0].ID != "t1" || running[0].Source != "api" {
		t.Fatalf("running = (%+v, %v), want t1 (priority 2, source round-tripped) before t0", running, err)
	}
	if err := s.TransitionTask(ctx, "t1", []string{"running"}, "closing"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkStarted(ctx, "t1"); err == nil {
		t.Fatal("MarkStarted revived a closing task")
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
	if err != nil || !ok || gw.Priority != 0 || gw.RiskClass != "standard" || len(gw.RequiredCapabilities) != 0 ||
		gw.Kind != "agent" || gw.IssueNumber != 0 {
		t.Fatalf("gateway-seeded task = %+v (%v, %v)", gw, ok, err)
	}
}
