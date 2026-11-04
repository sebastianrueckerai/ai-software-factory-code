package factoryd

import (
	"context"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// queueReasonStores are the stores whose queue reason must agree: Postgres,
// and the fake the admission and board tests read.
var queueReasonStores = map[string]func(*testing.T) Store{
	"postgres": func(t *testing.T) Store {
		s, err := OpenStore(context.Background(), schematest.DSN(t, "queue_reason"))
		if err != nil {
			t.Fatalf("OpenStore: %v", err)
		}
		t.Cleanup(func() { s.db.Close() })
		if _, err := s.db.ExecContext(context.Background(), `TRUNCATE tasks CASCADE`); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		return s
	},
	"fake": func(*testing.T) Store { return newFakeStore() },
}

// A refusal that repeats the stored reason keeps the time of the first, as
// does the count's at new numbers, a new reason restarts it, and "" clears
// both. The admission's stamp clears both too, and an admitted card takes
// no reason, though a refusal read before its admission writes one. The
// board's read carries what the card's read does.
func TestSetQueueReasonKeepsTheFirstRefusalTime(t *testing.T) {
	for name, open := range queueReasonStores {
		t.Run(name, func(t *testing.T) {
			ctx, s := context.Background(), open(t)
			if err := s.CreateTask(ctx, Task{ID: "t-q", Repository: "sf/r", Kind: "agent", Track: "direct", Status: "queued", Objective: "b", IssueNumber: 3}); err != nil {
				t.Fatal(err)
			}
			t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
			text := func(at *time.Time) string {
				if at == nil {
					return "none"
				}
				return at.UTC().Format(time.RFC3339)
			}
			read := func(step string, reason string, since *time.Time) {
				t.Helper()
				q, _, err := s.Task(ctx, "t-q")
				if err != nil || q.QueueReason != reason || text(q.QueueSince) != text(since) {
					t.Fatalf("%s: Task = %q since %s (err %v), want %q since %s", step, q.QueueReason, text(q.QueueSince), err, reason, text(since))
				}
				board, err := s.BoardTasks(ctx, "sf/r")
				if err != nil || len(board) != 1 || board[0].QueueReason != reason || text(board[0].QueueSince) != text(since) {
					t.Fatalf("%s: BoardTasks = %+v (err %v), want %q since %s", step, board, err, reason, text(since))
				}
			}
			set := func(reason string, after time.Duration) {
				t.Helper()
				if err := s.SetQueueReason(ctx, "t-q", reason, t0.Add(after)); err != nil {
					t.Fatalf("SetQueueReason(%q): %v", reason, err)
				}
			}
			at := func(d time.Duration) *time.Time { v := t0.Add(d); return &v }

			read("a new card", "", nil)
			set("waits on #7", 0)
			read("the first refusal", "waits on #7", at(0))
			set("waits on #7", time.Hour)
			read("the same reason again", "waits on #7", at(0))
			set("work bound 2 of 2", 2*time.Hour)
			read("a new reason", "work bound 2 of 2", at(2*time.Hour))
			set("work bound 3 of 2", 2*time.Hour+30*time.Minute)
			read("the count's reason at new numbers", "work bound 3 of 2", at(2*time.Hour))
			set("", 3*time.Hour)
			read("the admission", "", nil)
			set("no issue", 4*time.Hour)
			read("a refusal after the clear", "no issue", at(4*time.Hour))
			if err := s.SetAdmitted(ctx, "t-q"); err != nil {
				t.Fatal(err)
			}
			read("the admission's stamp", "", nil)
			set("work bound 2 of 2", 6*time.Hour)
			read("a refusal read before the card's admission", "", nil)
		})
	}
}
