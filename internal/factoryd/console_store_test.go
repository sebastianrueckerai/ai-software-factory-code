package factoryd

import (
	"context"
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
	tasks, err := s.BoardTasks(ctx, time.Now().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("BoardTasks: %v", err)
	}
	var ids []string
	for _, tk := range tasks {
		ids = append(ids, tk.ID)
	}
	if want := []string{"high", "low", "recent"}; !slices.Equal(ids, want) {
		t.Fatalf("BoardTasks = %v, want %v", ids, want)
	}
}
