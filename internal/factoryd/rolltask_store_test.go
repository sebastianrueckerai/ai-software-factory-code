package factoryd

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

func TestPGStoreRollRequests(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE roll_requests, tasks CASCADE`); err != nil {
		t.Fatal(err)
	}
	const repo = "sf/ai-software-factory-code"
	task := func(id, status string) {
		t.Helper()
		if err := s.CreateTask(ctx, Task{ID: id, Status: status, Repository: repo, Kind: "agent", Source: SourceRoll}); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(r RollRequest) {
		t.Helper()
		if err := s.InsertRollRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	seen := func(sha string) bool {
		t.Helper()
		ok, err := s.RollRequestSeen(ctx, sha)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	open := func() string {
		t.Helper()
		got, ok, err := s.OpenRollTask(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return ""
		}
		return got.ID
	}

	if seen("a") || open() != "" {
		t.Fatal("an empty table sees a commit or an open roll")
	}
	if first, carried, err := s.RollRuns(ctx, repo); err != nil || first != 0 || carried != 0 {
		t.Fatalf("runs %d %d %v, want none", first, carried, err)
	}
	insert(RollRequest{SHA: "a", Repository: repo, RunID: 10, State: RollSkipped})
	if !seen("a") {
		t.Fatal("the skipped commit is not seen")
	}

	task("rt1", "in-review")
	insert(RollRequest{SHA: "b", Repository: repo, RunID: 11, State: RollOpened, TaskID: "rt1",
		Changes: map[string]string{"worker": "sha256:w1"}, Summary: "b summary"})
	insert(RollRequest{SHA: "c", Repository: repo, RunID: 12, State: RollOpened, TaskID: "rt1",
		Changes: map[string]string{"gateway": "sha256:g2", "factoryd": "sha256:g2"}, Summary: "c summary"})
	if open() != "rt1" {
		t.Fatalf("open roll %q, want rt1", open())
	}
	insert(RollRequest{SHA: "s13", Repository: repo, RunID: 13, State: RollSkipped})
	if first, carried, err := s.RollRuns(ctx, repo); err != nil || first != 10 || carried != 12 {
		t.Fatalf("runs %d %d %v, want the first judged 10 and the newest carried 12", first, carried, err)
	}
	if err := s.DeleteRollRequest(ctx, "s13"); err != nil {
		t.Fatal(err)
	}
	pending, err := s.UncommittedRolls(ctx, "rt1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].SHA != "b" || pending[1].SHA != "c" ||
		!reflect.DeepEqual(pending[1].Changes, map[string]string{"gateway": "sha256:g2", "factoryd": "sha256:g2"}) || pending[1].Summary != "c summary" {
		t.Fatalf("uncommitted %+v, want b then c with their changes", pending)
	}
	if head, err := s.RollHead(ctx, "rt1"); err != nil || head != "" {
		t.Fatalf("roll head before a commit %q %v", head, err)
	}
	if err := s.MarkRollsCommitted(ctx, "rt1", []string{"b"}, "commit-1"); err != nil {
		t.Fatal(err)
	}
	if head, err := s.RollHead(ctx, "rt1"); err != nil || head != "commit-1" {
		t.Fatalf("roll head %q %v, want commit-1", head, err)
	}
	if err := s.SetTaskPR(ctx, "rt1", 21); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, Task{ID: "agent1", Status: "in-review", Repository: repo, Kind: "agent", Source: "webhook"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskPR(ctx, "agent1", 22); err != nil {
		t.Fatal(err)
	}
	if prs, err := s.RollTaskPRs(ctx, repo); err != nil || !reflect.DeepEqual(prs, []int64{21}) {
		t.Fatalf("roll pull requests %v %v, want [21]", prs, err)
	}
	if err := s.SetTaskSHA(ctx, "rt1", "cut-from"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "rt1"); got.SHA != "cut-from" {
		t.Fatalf("task base %q", got.SHA)
	}
	if pending, _ = s.UncommittedRolls(ctx, "rt1"); len(pending) != 1 || pending[0].SHA != "c" {
		t.Fatalf("uncommitted after b %+v, want c", pending)
	}
	if _, ok, err := s.LastMergedRoll(ctx, repo); err != nil || ok {
		t.Fatalf("a merged roll before any merge: %v %v", ok, err)
	}

	// rt1 merges with c folded in too late (never committed); rt2's row
	// lost its task (a crash between the row and the issue); rt3 failed
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'done' WHERE id = 'rt1'`); err != nil {
		t.Fatal(err)
	}
	insert(RollRequest{SHA: "d", Repository: repo, RunID: 13, State: RollOpened, TaskID: "rt2"})
	task("rt3", "failed")
	insert(RollRequest{SHA: "e", Repository: repo, RunID: 14, State: RollOpened, TaskID: "rt3"})
	if err := s.SettleRollRequests(ctx); err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	rows, err := s.db.QueryContext(ctx, `SELECT sha, state FROM roll_requests`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var sha, state string
		if err := rows.Scan(&sha, &state); err != nil {
			t.Fatal(err)
		}
		states[sha] = state
	}
	rows.Close()
	if want := map[string]string{"a": RollSkipped, "b": RollMerged, "e": RollFailed}; !reflect.DeepEqual(states, want) {
		t.Fatalf("settled %v, want %v", states, want)
	}
	if open() != "" {
		t.Fatalf("open roll %q after settling, want none", open())
	}
	if sha, ok, err := s.LastMergedRoll(ctx, repo); err != nil || !ok || sha != "b" {
		t.Fatalf("last merged roll %q %v %v, want b", sha, ok, err)
	}
	if err := s.DeleteRollRequest(ctx, "a"); err != nil || seen("a") {
		t.Fatalf("delete a: %v, seen %v", err, seen("a"))
	}
}

// RebaseRolls frees the markings past the head the branch sits at, only
// when the branch sits at a marking of the task's at all.
func TestPGStoreRebaseRolls(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE roll_requests, tasks CASCADE`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, Task{ID: "rt1", Status: "in-review", Repository: "sf/ai-software-factory-code", Kind: "agent", Source: SourceRoll}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []RollRequest{
		{SHA: "b", Repository: "sf/ai-software-factory-code", RunID: 11, State: RollOpened, TaskID: "rt1"},
		{SHA: "c", Repository: "sf/ai-software-factory-code", RunID: 12, State: RollOpened, TaskID: "rt1"},
		{SHA: "d", Repository: "sf/ai-software-factory-code", RunID: 13, State: RollOpened, TaskID: "rt1"},
	} {
		if err := s.InsertRollRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkRollsCommitted(ctx, "rt1", []string{"b"}, "commit-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRollsCommitted(ctx, "rt1", []string{"c"}, "commit-2"); err != nil {
		t.Fatal(err)
	}
	recorded, err := s.RebaseRolls(ctx, "rt1", "commit-1")
	if err != nil || !recorded {
		t.Fatalf("rebase onto commit-1: %t %v", recorded, err)
	}
	if head, err := s.RollHead(ctx, "rt1"); err != nil || head != "commit-1" {
		t.Fatalf("roll head %q %v, want commit-1", head, err)
	}
	pending, err := s.UncommittedRolls(ctx, "rt1")
	if err != nil || len(pending) != 2 || pending[0].SHA != "c" || pending[1].SHA != "d" {
		t.Fatalf("uncommitted %+v %v, want c then d", pending, err)
	}
	var committed string
	if err := s.db.QueryRowContext(ctx, `SELECT string_agg(sha, ',' ORDER BY sha) FROM roll_requests WHERE task_id = 'rt1' AND committed`).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if committed != "b" {
		t.Fatalf("committed %q, want b", committed)
	}
	if recorded, err := s.RebaseRolls(ctx, "rt1", "pushed-by-an-agent"); err != nil || recorded {
		t.Fatalf("rebase onto a foreign head: %t %v", recorded, err)
	}
	if pending, _ := s.UncommittedRolls(ctx, "rt1"); len(pending) != 2 {
		t.Fatalf("a foreign head moved the markings: %+v", pending)
	}
}

// A roll's hold is in force until its until; RollHold answers the newest
// of those in force, none past their until or cleared.
func TestPGStoreRollHolds(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE roll_holds`); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	hold := func(now time.Time) (RollHold, bool) {
		t.Helper()
		h, ok, err := s.RollHold(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		return h, ok
	}
	if h, ok := hold(at); ok {
		t.Fatalf("hold before any = %+v", h)
	}
	if err := s.SetRollHold(ctx, "r1", at, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if h, ok := hold(at.Add(time.Minute)); !ok || h.TaskID != "r1" || !h.Since.Equal(at) || !h.Until.Equal(at.Add(time.Hour)) {
		t.Fatalf("hold = %+v %v, want r1 from 12:00 to 13:00", h, ok)
	}
	if h, ok := hold(at.Add(time.Hour + time.Second)); ok {
		t.Fatalf("hold a second past its until = %+v", h)
	}
	if err := s.SetRollHold(ctx, "r2", at.Add(10*time.Minute), at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if h, ok := hold(at.Add(20 * time.Minute)); !ok || h.TaskID != "r2" {
		t.Fatalf("hold of two = %+v %v, want the newer r2", h, ok)
	}
	if err := s.ClearRollHold(ctx, "r2"); err != nil {
		t.Fatal(err)
	}
	if h, ok := hold(at.Add(20 * time.Minute)); !ok || h.TaskID != "r1" {
		t.Fatalf("hold after clearing r2 = %+v %v, want r1", h, ok)
	}
	// a hold set again (the wait's retry) moves its since and until
	if err := s.SetRollHold(ctx, "r1", at.Add(30*time.Minute), at.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if h, ok := hold(at.Add(2 * time.Hour)); !ok || h.TaskID != "r1" || !h.Since.Equal(at.Add(30*time.Minute)) {
		t.Fatalf("hold set again = %+v %v, want r1 from 12:30", h, ok)
	}
	if err := s.ClearRollHold(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if h, ok := hold(at.Add(20 * time.Minute)); ok {
		t.Fatalf("hold after clearing both = %+v", h)
	}
}
