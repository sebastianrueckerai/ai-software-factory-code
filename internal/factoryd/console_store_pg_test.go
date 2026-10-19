package factoryd

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// A card's change is live when a green run covers it (roadmap 297): the
// run that carried its pull request exists and the newest green main run —
// any row, the head row's status the run's own AND every required context —
// sits at or after it, so a green run after its own red carry run deploys
// it. The finished page holds a done card until then, and done cards are
// final on the board. A card the chain names at no run is live only while
// it finished before the chain began recording; a card without a pull
// request has nothing to deploy. Requires a disposable Postgres
// (FACTORYD_TEST_DATABASE_URL).
func TestChainCoverageHoldsTheCard(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events, chain_runs CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	for _, tk := range []Task{
		{ID: "t-hold", Status: "done", PRNumber: 7},
		{ID: "t-era", Status: "done", PRNumber: 8},
		{ID: "t-nopr", Status: "done", PRNumber: 0},
	} {
		tk.BudgetEUR, tk.Repository, tk.Kind = 1, "sf/r", "agent"
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask %s: %v", tk.ID, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET finished_at = now() - interval '20 minutes' WHERE id = 't-hold';
		UPDATE tasks SET finished_at = now() - interval '25 minutes' WHERE id = 't-nopr';
		UPDATE tasks SET finished_at = now() - interval '90 minutes' WHERE id = 't-era'`); err != nil {
		t.Fatalf("stamp finished_at: %v", err)
	}
	// the rows as the poller records them, in run order: a green head, then
	// run 10 carries pr 7 and fails, head row and all
	for _, row := range []struct {
		pr     int64
		sha    string
		run    int64
		status string
	}{
		{0, "e9", 9, "success"},
		{7, "e10", 10, "failure"},
		{0, "e10", 10, "failure"},
	} {
		if err := s.RecordChainRun(ctx, "sf/r", row.pr, row.sha, row.run, row.status); err != nil {
			t.Fatalf("RecordChainRun pr %d: %v", row.pr, err)
		}
	}
	live := func(id string) bool {
		t.Helper()
		b, ok, err := s.BoardTask(ctx, id)
		if err != nil || !ok {
			t.Fatalf("BoardTask(%s) = %v, %v", id, ok, err)
		}
		return b.ChainLive
	}
	board := func(want ...string) {
		t.Helper()
		got, err := s.BoardTasks(ctx, "sf/r")
		if err != nil {
			t.Fatalf("BoardTasks: %v", err)
		}
		var ids []string
		for _, tk := range got {
			ids = append(ids, tk.ID)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("BoardTasks = %v, want %v", ids, want)
		}
	}
	finished := func(want ...string) {
		t.Helper()
		got, _, err := s.FinishedTasks(ctx, FinishedQuery{Repository: "sf/r", Statuses: FinalStatuses, Since: time.Now().Add(-7 * 24 * time.Hour)})
		if err != nil {
			t.Fatalf("FinishedTasks: %v", err)
		}
		var ids []string
		for _, tk := range got {
			ids = append(ids, tk.ID)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("FinishedTasks = %v, want %v", ids, want)
		}
	}
	// no green run at or after pr 7's carry run: the finished page holds
	// the card; t-era finished before the chain began recording, t-nopr
	// never opened a pull request
	board()
	finished("t-nopr", "t-era")
	if live("t-hold") {
		t.Fatal("the carried card reads live before a green run covers it")
	}
	if !live("t-era") || !live("t-nopr") {
		t.Fatal("the grandfathered and the pull-request-less card read not live")
	}
	// a green head run after the carry run covers the change
	if err := s.RecordChainRun(ctx, "sf/r", 0, "e12", 12, "success"); err != nil {
		t.Fatalf("RecordChainRun run 12: %v", err)
	}
	board()
	finished("t-hold", "t-nopr", "t-era")
	if !live("t-hold") {
		t.Fatal("the carried card reads not live once a green run covers it")
	}
}
