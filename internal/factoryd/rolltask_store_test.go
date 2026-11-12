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
		if err := s.CreateTask(ctx, Task{ID: id, Status: status, Repository: repo, Kind: "agent", Source: SourceRoll, Track: "direct"}); err != nil {
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
	insert(RollRequest{SHA: "b", Repository: repo, RunID: 11, State: RollOpened, TaskID: "rt1", PRs: []int64{12},
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
	if err := s.CreateTask(ctx, Task{ID: "agent1", Status: "in-review", Repository: repo, Kind: "agent", Source: "webhook", Track: "direct"}); err != nil {
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

	// rt1 merges with c folded in too late (never committed); rt2's row
	// lost its task (a crash between the row and the issue); rt3 failed;
	// f's refusal has its card, closed; g's refusal lost its card
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'done', finished_at = now() WHERE id = 'rt1'`); err != nil {
		t.Fatal(err)
	}
	insert(RollRequest{SHA: "d", Repository: repo, RunID: 13, State: RollOpened, TaskID: "rt2"})
	task("rt3", "failed")
	insert(RollRequest{SHA: "e", Repository: repo, RunID: 14, State: RollOpened, TaskID: "rt3"})
	if err := s.CreateTask(ctx, Task{ID: "rc1", Status: "done", Repository: repo, Kind: "human", Source: SourceRollRefused}); err != nil {
		t.Fatal(err)
	}
	insert(RollRequest{SHA: "f", Repository: repo, RunID: 15, State: RollRefused, TaskID: "rc1",
		Changes: map[string]string{"worker": "sha256:w3"}, Summary: "roll f refused"})
	insert(RollRequest{SHA: "g", Repository: repo, RunID: 16, State: RollRefused, TaskID: "rc2"})
	settled, err := s.SettleRollRequests(ctx)
	if err != nil {
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
	if want := map[string]string{"a": RollSkipped, "b": RollMerged, "e": RollFailed, "f": RollRefused}; !reflect.DeepEqual(states, want) {
		t.Fatalf("settled %v, want %v", states, want)
	}
	// settle returned the two it settled, for their verdicts to ride a
	// commit status; the gone (c, d) and the refused (f) are not among them
	got := map[string]string{}
	for _, r := range settled {
		got[r.SHA] = r.State
	}
	if want := map[string]string{"b": RollMerged, "e": RollFailed}; !reflect.DeepEqual(got, want) {
		t.Fatalf("settled rows %v, want %v", got, want)
	}
	if open() != "" {
		t.Fatalf("open roll %q after settling, want none", open())
	}
	// settle deleted the never-committed c (the states above say so):
	// the merged b alone remains, carrying its pull request
	if rolls, err := s.TaskRolls(ctx, "rt1"); err != nil || len(rolls) != 1 ||
		!reflect.DeepEqual(rolls[0].PRs, []int64{12}) {
		t.Fatalf("task rolls %+v, want the settled b carrying pull request 12", rolls)
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
	if err := s.CreateTask(ctx, Task{ID: "rt1", Status: "in-review", Repository: "sf/ai-software-factory-code", Kind: "agent", Source: SourceRoll, Track: "direct"}); err != nil {
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

func TestARollRequestSettlesOnAMergedRollTask(t *testing.T) {
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
	for i, c := range []struct{ task, status, sha string }{
		{"rt1", "merged", "m"}, {"rt2", "failed", "f"}, {"rt3", "running", "r"},
	} {
		if err := s.CreateTask(ctx, Task{ID: c.task, Status: c.status, Repository: repo, Kind: "agent", Source: SourceRoll, Track: "direct"}); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertRollRequest(ctx, RollRequest{SHA: c.sha, Repository: repo, RunID: int64(10 + i), State: RollOpened, TaskID: c.task}); err != nil {
			t.Fatal(err)
		}
		if c.status != "running" {
			if err := s.MarkRollsCommitted(ctx, c.task, []string{c.sha}, "commit-"+c.sha); err != nil {
				t.Fatal(err)
			}
		}
	}
	// folded in after rt1's last commit: forgotten, not merged
	if err := s.InsertRollRequest(ctx, RollRequest{SHA: "late", Repository: repo, RunID: 13, State: RollOpened, TaskID: "rt1"}); err != nil {
		t.Fatal(err)
	}
	settled, err := s.SettleRollRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range settled {
		got[r.SHA] = r.State
	}
	if want := map[string]string{"m": RollMerged, "f": RollFailed}; !reflect.DeepEqual(got, want) {
		t.Fatalf("settled %v, want %v", got, want)
	}
	states := map[string]string{}
	rows, err := s.db.QueryContext(ctx, `SELECT sha, state FROM roll_requests`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var sha, state string
		if err := rows.Scan(&sha, &state); err != nil {
			t.Fatal(err)
		}
		states[sha] = state
	}
	if want := map[string]string{"m": RollMerged, "f": RollFailed, "r": RollOpened}; !reflect.DeepEqual(states, want) {
		t.Fatalf("rows %v, want %v", states, want)
	}
}

// A roll that settles failed ends the Deploy now window of its commit in
// the same statement that settles it: a poll that dies right after the
// settle cannot leave the window standing, for no later poll sees the
// settled request again. Another commit's window stands.
func TestASettledFailedRollEndsItsReleaseWindow(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE roll_requests, tasks, releases CASCADE`); err != nil {
		t.Fatal(err)
	}
	const repo = "sf/ai-software-factory-code"
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	if err := s.CreateTask(ctx, Task{ID: "rt1", Status: "failed", Repository: repo, Kind: "agent", Source: SourceRoll, Track: "direct"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRollRequest(ctx, RollRequest{SHA: "f", Repository: repo, RunID: 10, State: RollOpened, TaskID: "rt1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRollsCommitted(ctx, "rt1", []string{"f"}, "commit-f"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeployNow(ctx, repo, "f", at, Event{}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeployNow(ctx, "sf/linkshort", "g", at, Event{}); err != nil {
		t.Fatal(err)
	}
	settled, err := s.SettleRollRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(settled) != 1 || settled[0].SHA != "f" || settled[0].State != RollFailed {
		t.Fatalf("settled %+v, want the one failed roll of f", settled)
	}
	rel, err := s.Release(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if rel.DeployNow != "" {
		t.Fatalf("deploy_now after the failed settle = %q, want the window ended", rel.DeployNow)
	}
	if rel, err = s.Release(ctx, "sf/linkshort"); err != nil {
		t.Fatal(err)
	}
	if rel.DeployNow != "g" {
		t.Fatalf("linkshort deploy_now = %q, want g standing", rel.DeployNow)
	}
}
