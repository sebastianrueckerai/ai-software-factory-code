package factoryd

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
	"reflect"
	"slices"
	"testing"
	"time"
)

// The go action's hand-over claim: one row per card, so of two racing
// hands InsertIntakeEvent returns 0 to the second, and the failed
// filing's release lets the next attempt claim again. Requires a
// disposable Postgres (FACTORYD_TEST_DATABASE_URL), like TestPGStore.
func TestPGStoreHandoverClaim(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, intake_events CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := s.CreateTask(ctx, Task{ID: "t-go", Kind: "human", Status: "waiting-human", Repository: "sf/r", BudgetEUR: 1}); err != nil {
		t.Fatal(err)
	}
	first, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t-go", Kind: IntakeHandover, Author: "sebastian", Body: "handed to the factory"})
	if err != nil || first == 0 {
		t.Fatalf("the first claim = %d, %v; want its row", first, err)
	}
	if second, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t-go", Kind: IntakeHandover, Author: "sebastian", Body: "handed to the factory"}); err != nil || second != 0 {
		t.Fatalf("the racing claim = %d, %v; want the refusal", second, err)
	}
	if err := s.ReleaseHandover(ctx, "t-go"); err != nil {
		t.Fatal(err)
	}
	if again, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t-go", Kind: IntakeHandover, Author: "sebastian", Body: "handed to the factory"}); err != nil || again == 0 {
		t.Fatalf("the claim after the release = %d, %v; want its row", again, err)
	}
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL), like TestPGStore.
func TestPGStoreBoardTasks(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	for _, tk := range []Task{
		{ID: "low", Priority: 0, Status: "queued", Track: "direct"},
		{ID: "high", Priority: 3, Status: "running", Track: "direct"},
		{ID: "recent", Status: "done", Track: "direct"},
		{ID: "old", Status: "failed", Track: "direct"},
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
	// the board reads an escalation's reason and choices from the row the
	// escalation wrote, the latest one, not from the timeline object
	for _, e := range []IntakeEvent{{Body: "old reason", Choices: []string{"old", "older"}},
		{Body: "cannot reach the database", Choices: []string{"use the replica", "wait for it", "give up"}}} {
		e.TaskID, e.Kind, e.Author = "high", IntakeEscalation, "factoryd"
		if _, err := s.InsertIntakeEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "low", Kind: "comment", Author: "sebastian", Body: "not an escalation"}); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.BoardTasks(ctx, "sf/r")
	if err != nil {
		t.Fatalf("BoardTasks: %v", err)
	}
	// the finished ones of the week, "old" finished before it
	done, rest, err := s.FinishedTasks(ctx, FinishedQuery{Repository: "sf/r", Statuses: FinalStatuses, Since: time.Now().Add(-7 * 24 * time.Hour)})
	if err != nil || rest != 1 {
		t.Fatalf("FinishedTasks = %+v, %d, %v", done, rest, err)
	}
	tasks = append(tasks, done...)
	var ids, reasons []string
	for _, tk := range tasks {
		ids = append(ids, tk.ID)
		reasons = append(reasons, tk.Escalation)
		if escalated := tk.ID == "high"; escalated == tk.EscalatedAt.IsZero() || escalated && time.Since(tk.EscalatedAt) > time.Minute {
			t.Errorf("%s escalated at %v", tk.ID, tk.EscalatedAt)
		}
	}
	if want := []string{"high", "low", "recent"}; !slices.Equal(ids, want) {
		t.Fatalf("BoardTasks = %v, want %v", ids, want)
	}
	if want := []string{"cannot reach the database", "", ""}; !slices.Equal(reasons, want) {
		t.Fatalf("reasons = %q, want %q", reasons, want)
	}
	if want := []string{"use the replica", "wait for it", "give up"}; !slices.Equal(tasks[0].Choices, want) || tasks[1].Choices != nil {
		t.Fatalf("choices = %q and %q, want %q and none", tasks[0].Choices, tasks[1].Choices, want)
	}
	// the intake events read them back; a row without choices has none
	events, err := s.IntakeEvents(ctx, "high")
	if err != nil || len(events) != 2 || !slices.Equal(events[0].Choices, []string{"old", "older"}) || !slices.Equal(events[1].Choices, []string{"use the replica", "wait for it", "give up"}) {
		t.Fatalf("IntakeEvents(high) = %+v, %v", events, err)
	}
	if events, err := s.IntakeEvents(ctx, "low"); err != nil || len(events) != 1 || events[0].Choices != nil {
		t.Fatalf("IntakeEvents(low) = %+v, %v; want one row without choices", events, err)
	}
	// one card reads as the board reads it, a card off the board too
	if one, ok, err := s.BoardTask(ctx, "high"); err != nil || !ok || !reflect.DeepEqual(one, tasks[0]) {
		t.Fatalf("BoardTask(high) = %+v, %v, %v; want the board's %+v", one, ok, err, tasks[0])
	}
	if one, ok, err := s.BoardTask(ctx, "old"); err != nil || !ok || one.Status != "failed" {
		t.Fatalf("BoardTask(old) = %+v, %v, %v; want the failed task", one, ok, err)
	}
	if _, ok, err := s.BoardTask(ctx, "t-gone"); err != nil || ok {
		t.Fatalf("BoardTask(t-gone) = %v, %v; want no task", ok, err)
	}
}

// A card's liveness is the chain's fact (roadmap 297): a done card is
// live when the run that carried its pull request went green, a card no
// run names only while it finished before the chain began recording;
// the wait itself is the merged card's, which the board holds with its
// carry row's status as its chain word until the chain poller ends it.
// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL).
func TestPGStoreChainLiveness(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events, chain_runs CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	for _, tk := range []Task{
		{ID: "t-era", Status: "done", PRNumber: 11, Track: "direct"},
		{ID: "t-green", Status: "done", PRNumber: 12, Track: "direct"},
		{ID: "t-run", Status: "merged", PRNumber: 13, Track: "direct"},
		{ID: "t-red", Status: "merged", PRNumber: 14, Track: "direct"},
	} {
		tk.BudgetEUR, tk.Repository, tk.Kind = 1, "sf/r", "agent"
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask %s: %v", tk.ID, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET finished_at = now() - interval '90 minutes' WHERE id = 't-era';
		UPDATE tasks SET finished_at = now() - interval '20 minutes' WHERE id = 't-green'`); err != nil {
		t.Fatalf("stamp finished_at: %v", err)
	}
	// before any chain run is recorded the merged cards wait on the board
	// with no chain word; every done card is live (none recorded yet:
	// every card, while none is)
	if board, err := s.BoardTasks(ctx, "sf/r"); err != nil || len(board) != 2 {
		t.Fatalf("BoardTasks before the chain = %d cards, %v; want the two merged", len(board), err)
	}
	for _, row := range []struct {
		pr     int64
		sha    string
		run    int64
		status string
	}{
		{0, "e0", 50, "success"}, // a run no pull request rode: only the era starts
		{12, "e1", 51, "success"},
		{13, "e2", 52, "running"},
		{14, "e3", 53, "failure"},
	} {
		if err := s.RecordChainRun(ctx, "sf/r", row.pr, row.sha, row.run, row.status); err != nil {
			t.Fatalf("RecordChainRun pr %d: %v", row.pr, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE chain_runs SET updated_at = now() - interval '80 minutes' WHERE run_id = 50`); err != nil {
		t.Fatalf("stamp chain era: %v", err)
	}
	board, err := s.BoardTasks(ctx, "sf/r")
	if err != nil {
		t.Fatalf("BoardTasks: %v", err)
	}
	var ids, stages []string
	for _, tk := range board {
		ids = append(ids, tk.ID)
		stages = append(stages, tk.ChainState)
	}
	// the merged cards wait on their run's verdict on the board (t-red
	// first: its id sorts before t-run's at the same priority); t-era
	// finished before the first recorded run and t-green rode a green run,
	// so both are live and done
	if want := []string{"t-red", "t-run"}; !slices.Equal(ids, want) {
		t.Fatalf("BoardTasks = %v, want %v", ids, want)
	}
	if want := []string{"failure", "running"}; !slices.Equal(stages, want) {
		t.Fatalf("chain states = %v, want %v", stages, want)
	}
	done, rest, err := s.FinishedTasks(ctx, FinishedQuery{Repository: "sf/r", Statuses: FinalStatuses, Since: time.Now().Add(-7 * 24 * time.Hour)})
	if err != nil || rest != len(done) {
		t.Fatalf("FinishedTasks = %d cards, rest %d, %v", len(done), rest, err)
	}
	var doneIDs []string
	for _, tk := range done {
		doneIDs = append(doneIDs, tk.ID)
	}
	if want := []string{"t-green", "t-era"}; !slices.Equal(doneIDs, want) {
		t.Fatalf("FinishedTasks = %v, want %v", doneIDs, want)
	}
	// the running run settles green: the store moves the card's own row, its
	// chain word with it — the card itself the chain poller's to end
	if err := s.RefreshChainStatus(ctx, "sf/r", "e2", "success"); err != nil {
		t.Fatalf("RefreshChainStatus: %v", err)
	}
	if board, err := s.BoardTasks(ctx, "sf/r"); err != nil || len(board) != 2 || board[1].ID != "t-run" || board[1].ChainState != "success" {
		t.Fatalf("BoardTasks after the run went green = %+v, %v; want t-run's word green, the card still waiting", board, err)
	}
}

// BoardTasks reads only the repository it is asked for: none of another
// repository's open cards, however urgent, all of its own — and a card
// that waits on another repository's card still names its issue, its
// chips reading the dependency itself, not the other board. Requires a
// disposable Postgres (FACTORYD_TEST_DATABASE_URL).
func TestPGStoreBoardTasksOfRepository(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	for _, tk := range []Task{
		{ID: "mine-low", Priority: 0, Status: "queued", Track: "direct"},
		{ID: "mine-waits", Priority: 1, Status: "waiting-deps", IssueNumber: 4, Track: "direct"},
		{ID: "mine-high", Priority: 3, Status: "running", Track: "direct"},
		{ID: "mine-done", Priority: 5, Status: "done", Track: "direct"},
		{ID: "mine-chat", Priority: 5, Status: "running", Kind: KindChat},
		{ID: "theirs", Priority: 5, Status: "queued", IssueNumber: 9, Repository: "sf/other", Track: "direct"},
	} {
		tk.BudgetEUR, tk.Repository, tk.Kind = 1, cmp.Or(tk.Repository, "sf/r"), cmp.Or(tk.Kind, "agent")
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask %s: %v", tk.ID, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO task_deps (task_id, depends_on) VALUES ('mine-waits', 'theirs')`); err != nil {
		t.Fatalf("cross-repository dependency: %v", err)
	}
	ids := func(tasks []BoardTask) []string {
		var out []string
		for _, tk := range tasks {
			out = append(out, tk.ID)
		}
		return out
	}
	// the other repository's open card never shows, this one's own open
	// cards all do, most urgent first; its finished and chat tasks do not
	if got, err := s.BoardTasks(ctx, "sf/r"); err != nil || !slices.Equal(ids(got), []string{"mine-high", "mine-waits", "mine-low"}) {
		t.Fatalf("BoardTasks(sf/r) = %v, %v; want mine-high, mine-waits, mine-low", ids(got), err)
	} else if got[1].WaitingOn != "#9" || got[1].Dependencies != "#9" {
		t.Fatalf("the waiting card reads %q, %q; want the other repository's #9 on both chips", got[1].WaitingOn, got[1].Dependencies)
	}
	if got, err := s.BoardTasks(ctx, "sf/other"); err != nil || !slices.Equal(ids(got), []string{"theirs"}) {
		t.Fatalf("BoardTasks(sf/other) = %v, %v; want theirs", ids(got), err)
	}
}

// The Done column orders finished cards by date, not by rank (roadmap
// 234): the newest first however low its priority; the open cards stay
// most urgent first.
func TestPGStoreBoardDoneOrder(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	for _, tk := range []Task{
		{ID: "q0", Priority: 0, Status: "queued", Track: "direct"},
		{ID: "q2", Priority: 2, Status: "queued", Track: "direct"},
		{ID: "older", Priority: 2, Status: "done", Track: "direct"},
		{ID: "roll", Priority: 0, Status: "done", Track: "direct"},
		{ID: "unstamped", Priority: 1, Status: "failed", PlanKey: "k", Track: "direct"},
		{ID: "waiter", Priority: 1, Status: "waiting-deps", Track: "direct"},
	} {
		tk.BudgetEUR, tk.Repository, tk.Kind = 1, "sf/r", "agent"
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask %s: %v", tk.ID, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET finished_at = now() - interval '3 days' WHERE id = 'older';
		UPDATE tasks SET finished_at = now() WHERE id = 'roll';
		UPDATE tasks SET created_at = now() - interval '1 day', finished_at = now() - interval '1 day' WHERE id = 'unstamped';
		INSERT INTO task_deps (task_id, depends_on) VALUES ('waiter', 'unstamped')`); err != nil {
		t.Fatalf("stamp finished_at: %v", err)
	}
	open, err := s.BoardTasks(ctx, "sf/r")
	if err != nil {
		t.Fatalf("BoardTasks: %v", err)
	}
	q := FinishedQuery{Repository: "sf/r", Statuses: FinalStatuses, Since: time.Now().Add(-7 * 24 * time.Hour)}
	done, _, err := s.FinishedTasks(ctx, q)
	if err != nil {
		t.Fatalf("FinishedTasks: %v", err)
	}
	ids := func(tasks []BoardTask) []string {
		var out []string
		for _, tk := range tasks {
			out = append(out, tk.ID)
		}
		return out
	}
	// the open cards stay most urgent first (q2, waiter, q0); the roll
	// finished newest heads Done, the card finished a day ago follows,
	// and the older one last however higher its rank
	if want := []string{"q2", "waiter", "q0"}; !slices.Equal(ids(open), want) {
		t.Fatalf("BoardTasks = %v, want %v", ids(open), want)
	}
	if want := []string{"roll", "unstamped", "older"}; !slices.Equal(ids(done), want) {
		t.Fatalf("FinishedTasks = %v, want %v", ids(done), want)
	}
	// the in-memory fakes sort and page with the same orders
	all := append(slices.Clone(done), open...)
	SortBoardTasks(open)
	if want := []string{"q2", "waiter", "q0"}; !slices.Equal(ids(open), want) {
		t.Fatalf("SortBoardTasks = %v, want %v", ids(open), want)
	}
	if page, _ := FinishedPage(all, q); !slices.Equal(ids(page), ids(done)) {
		t.Fatalf("FinishedPage = %v, want %v", ids(page), ids(done))
	}
}

// The board pages a project's finished cards by arrival and id: ties on
// the arrival go by id, each page starts after the card the one before
// ended with, so cards finishing between two pages neither repeat nor
// push one off; a page says how many cards are left from it on.
// Requires FACTORYD_TEST_DATABASE_URL.
func TestPGStoreFinishedPages(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	minute := func(n int) *time.Time { at := base.Add(time.Duration(-n) * time.Minute); return &at }
	week := base.AddDate(0, 0, -8)
	at90 := base.Add(-90 * time.Second)
	for _, tk := range []struct {
		Task
		at *time.Time
	}{
		{Task{ID: "a", Status: "done", Track: "direct"}, minute(0)},
		{Task{ID: "b", Status: "failed", Track: "direct"}, minute(1)},
		{Task{ID: "c", Status: "done", Track: "direct"}, minute(1)},
		{Task{ID: "d", Status: "closed", Track: "direct"}, minute(1)},
		{Task{ID: "e", Status: "done", Track: "direct"}, minute(2)},
		{Task{ID: "f", Status: "failed", PlanKey: "k", CreatedAt: at90, Track: "direct"}, &at90},
		{Task{ID: "g", Status: "done", Track: "direct"}, &week},
		{Task{ID: "h", Status: "done", Kind: "human"}, minute(0)},
		{Task{ID: "i", Status: "closed", Kind: KindChat}, minute(0)},
		{Task{ID: "j", Status: "done", Repository: "sf/other", Track: "direct"}, minute(0)},
		{Task{ID: "k", Status: "running", Track: "direct"}, nil},
		{Task{ID: "w", Status: "waiting-deps", Track: "direct"}, nil},
	} {
		tk.BudgetEUR, tk.Repository, tk.Kind = 1, cmp.Or(tk.Repository, "sf/r"), cmp.Or(tk.Kind, "agent")
		if err := s.CreateTask(ctx, tk.Task); err != nil {
			t.Fatalf("CreateTask %s: %v", tk.ID, err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET finished_at = $2 WHERE id = $1`, tk.ID, tk.at); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddTaskDeps(ctx, "w", []string{"f"}); err != nil {
		t.Fatal(err)
	}
	page := func(q FinishedQuery, want []string, wantRest int) []BoardTask {
		t.Helper()
		q.Repository, q.Since = "sf/r", base.AddDate(0, 0, -7)
		if q.Statuses == nil {
			q.Statuses = FinalStatuses
		}
		got, rest, err := s.FinishedTasks(ctx, q)
		var ids []string
		for _, b := range got {
			ids = append(ids, b.ID)
		}
		if err != nil || !slices.Equal(ids, want) || rest != wantRest {
			t.Fatalf("FinishedTasks(%+v) = %v, %d, %v; want %v, %d", q, ids, rest, err, want, wantRest)
		}
		return got
	}
	page(FinishedQuery{}, []string{"a", "b", "c", "d", "f", "e"}, 6)
	page(FinishedQuery{Oldest: true}, []string{"e", "f", "b", "c", "d", "a"}, 6)
	page(FinishedQuery{Statuses: []string{"failed"}}, []string{"b", "f"}, 2)
	page(FinishedQuery{After: Cursor{At: *minute(1), ID: "c"}}, []string{"d", "f", "e"}, 3)
	page(FinishedQuery{After: Cursor{At: *minute(1), ID: "c"}, Oldest: true}, []string{"d", "a"}, 2)
	page(FinishedQuery{After: Cursor{At: *minute(2), ID: "e"}}, nil, 0)
	first := page(FinishedQuery{Limit: 2}, []string{"a", "b"}, 6)
	second := page(FinishedQuery{Limit: 2, After: CursorOf(first[1].Task)}, []string{"c", "d"}, 4)
	// a card finishes now, newer than every page: the next page goes on
	// where the last ended, and only a first page shows it
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'done', finished_at = now() WHERE id = 'k'`); err != nil {
		t.Fatal(err)
	}
	page(FinishedQuery{Limit: 2, After: CursorOf(second[1].Task)}, []string{"f", "e"}, 2)
	page(FinishedQuery{Limit: 2}, []string{"k", "a"}, 7)
}

// A writer's summary and accent land on the task row and read back on the
// task and the board; "" stores NULL, which reads back as "".
func TestPGStoreHumanSummaryAndAccent(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := s.CreateTask(ctx, Task{ID: "t-s", Status: "running", BudgetEUR: 1, Repository: "sf/r", Kind: "agent", Track: "direct"}); err != nil {
		t.Fatal(err)
	}
	read := func() (Task, BoardTask) {
		t.Helper()
		tk, ok, err := s.Task(ctx, "t-s")
		if err != nil || !ok {
			t.Fatalf("Task = %v, %v", ok, err)
		}
		board, err := s.BoardTasks(ctx, "sf/r")
		if err != nil || len(board) != 1 {
			t.Fatalf("BoardTasks = %+v, %v", board, err)
		}
		return tk, board[0]
	}
	if tk, b := read(); tk.HumanSummary != "" || tk.Accent != "" || b.HumanSummary != "" || b.Accent != "" {
		t.Fatalf("a new task has summary %q, accent %q", tk.HumanSummary, tk.Accent)
	}
	if err := s.SetHumanSummary(ctx, "t-s", "the cache, at last"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskAccent(ctx, "t-s", "teal"); err != nil {
		t.Fatal(err)
	}
	if tk, b := read(); tk.HumanSummary != "the cache, at last" || tk.Accent != "teal" || b.HumanSummary != "the cache, at last" || b.Accent != "teal" {
		t.Fatalf("task %q %q, board %q %q; want the cache, at last and teal", tk.HumanSummary, tk.Accent, b.HumanSummary, b.Accent)
	}
	if err := s.SetHumanSummary(ctx, "t-s", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskAccent(ctx, "t-s", ""); err != nil {
		t.Fatal(err)
	}
	var cleared bool
	if err := s.db.QueryRowContext(ctx, `SELECT human_summary IS NULL AND accent IS NULL FROM tasks WHERE id = 't-s'`).Scan(&cleared); err != nil || !cleared {
		t.Fatalf("cleared = %v, %v; want both columns NULL", cleared, err)
	}
	if tk, _ := read(); tk.HumanSummary != "" || tk.Accent != "" {
		t.Fatalf("after clearing: summary %q, accent %q", tk.HumanSummary, tk.Accent)
	}
	if err := s.SetObjective(ctx, "t-s", "Fix the cache eviction\n\nIt drops entries."); err != nil {
		t.Fatal(err)
	}
	if tk, _ := read(); tk.Objective != "Fix the cache eviction\n\nIt drops entries." {
		t.Fatalf("objective = %q, want the edited one", tk.Objective)
	}
	if err := s.SetObjective(ctx, "t-gone", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetObjective of no task = %v, want ErrNotFound", err)
	}
	if err := s.SetHumanSummary(ctx, "t-gone", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetHumanSummary of no task = %v, want ErrNotFound", err)
	}
	if err := s.SetTaskAccent(ctx, "t-gone", "red"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetTaskAccent of no task = %v, want ErrNotFound", err)
	}
}

// A task row carries when the work was asked for and the gate it is in;
// alerts are raised by the machine and a human alert takes one verdict.
func TestPGStoreGatesAndAlerts(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events, alerts CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	asked := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	for _, tk := range []Task{
		{ID: "issue", Status: "queued", Kind: "agent", CreatedAt: asked, Track: "direct"},
		{ID: "api", Status: "queued", Kind: "agent", Track: "direct"},
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
	board, err := s.BoardTasks(ctx, "sf/r")
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
	board, _ = s.BoardTasks(ctx, "sf/r")
	for _, b := range board {
		if b.ID == "api" && b.EscalationAlert != 0 {
			t.Fatalf("board still offers the judged alert %d", b.EscalationAlert)
		}
	}
}

// The gate supervisor judges an open gate_bound alert once, its action
// merged into the detail.
func TestPGStoreResolveAlert(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE alerts`); err != nil {
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
	if err := s.ResolveAlert(ctx, bound, AlertGateBound, VerdictNoise, "supervisor", map[string]any{"action": "wait", "diagnosis": "checks run"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveAlert(ctx, bound, AlertGateBound, VerdictReal, "supervisor", map[string]any{"action": "cancel"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second verdict err = %v, want ErrNotFound", err)
	}
	if err := s.ResolveAlert(ctx, esc, AlertGateBound, VerdictReal, "supervisor", nil); !errors.Is(err, ErrNotFound) {
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
}

// JudgeEscalations judges the task's open escalation alerts its filter
// picks: by source the day cap's, by comment one question's, never
// another task's nor one judged already.
func TestPGStoreJudgeEscalations(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE alerts`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	for _, a := range []Alert{{TaskID: "t1", Source: "supervisor", Detail: map[string]any{"comment": 650}},
		{TaskID: "t1", Source: "supervisor", Detail: map[string]any{"comment": 701}}, {TaskID: "t1", Source: "day_cap", Detail: map[string]any{"comment": 702}},
		{TaskID: "t2", Source: "supervisor", Detail: map[string]any{"comment": 701}}} {
		a.Kind = AlertEscalation
		if _, err := s.InsertAlert(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := s.JudgeEscalations(ctx, "t1", EscalationFilter{Comment: 701}, VerdictNoise, FactoryDecider); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.JudgeEscalations(ctx, "t1", EscalationFilter{Source: "day_cap"}, VerdictReal, FactoryDecider); err != nil {
		t.Fatal(err)
	}
	alerts, err := s.Alerts(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range alerts {
		got = append(got, fmt.Sprintf("%s %s %v %q by %q", a.TaskID, a.Source, a.Detail["comment"], a.Verdict, a.ResolvedBy))
	}
	slices.Sort(got)
	want := []string{`t1 day_cap 702 "real" by "factoryd"`, `t1 supervisor 650 "" by ""`, `t1 supervisor 701 "noise" by "factoryd"`, `t2 supervisor 701 "" by ""`}
	if !slices.Equal(got, want) {
		t.Fatalf("alerts = %q, want %q", got, want)
	}
}

// CloseClearedCard writes the alert's verdict and the card's end together
// or not at all.
func TestCloseClearedCardIsOneStep(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, alerts, events CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	state := func(id string, alert int64) (status string, verdict string) {
		t.Helper()
		if err := s.db.QueryRowContext(ctx, `SELECT t.status, coalesce(a.verdict, '') FROM tasks t, alerts a WHERE t.id = $1 AND a.id = $2`, id, alert).Scan(&status, &verdict); err != nil {
			t.Fatal(err)
		}
		return status, verdict
	}
	issue := int64(0)
	add := func(id, status string) int64 {
		t.Helper()
		issue++
		if err := s.CreateTask(ctx, Task{ID: id, Kind: "human", Status: status, Repository: "sf/r", IssueNumber: issue}); err != nil {
			t.Fatal(err)
		}
		a, err := s.InsertAlert(ctx, Alert{Kind: AlertSigNoz, Source: "signoz", Fingerprint: id, TaskID: id})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	finished := add("t-done", "done")
	if err := s.CloseClearedCard(ctx, finished, "t-done"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a finished card: err = %v, want ErrNotFound", err)
	}
	if status, verdict := state("t-done", finished); status != "done" || verdict != "" {
		t.Fatalf("a finished card: task %s, verdict %q; want done and no verdict", status, verdict)
	}

	judged := add("t-judged", "waiting-human")
	if err := s.ResolveAlert(ctx, judged, AlertSigNoz, VerdictReal, "sebastian", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseClearedCard(ctx, judged, "t-judged"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a judged alert: err = %v, want ErrNotFound", err)
	}
	if status, verdict := state("t-judged", judged); status != "waiting-human" || verdict != VerdictReal {
		t.Fatalf("a judged alert: task %s, verdict %q; want waiting-human and real", status, verdict)
	}

	fresh := add("t-fresh", "waiting-human")
	if err := s.CloseClearedCard(ctx, fresh, "t-fresh"); err != nil {
		t.Fatal(err)
	}
	if status, verdict := state("t-fresh", fresh); status != "closed" || verdict != VerdictUnjudged {
		t.Fatalf("a fresh pair: task %s, verdict %q; want closed and unjudged", status, verdict)
	}
	var by, closed string
	var finishedAt sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT a.resolved_by, a.detail->>'closed', t.finished_at FROM alerts a, tasks t WHERE a.id = $1 AND t.id = 't-fresh'`, fresh).
		Scan(&by, &closed, &finishedAt); err != nil {
		t.Fatal(err)
	}
	if by != "factoryd" || closed != "cleared" || !finishedAt.Valid {
		t.Fatalf("resolved_by %q, detail closed %q, finished_at %v; want factoryd, cleared, set", by, closed, finishedAt)
	}
	evs, err := s.TaskEvents(ctx, "t-fresh", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != EventTaskStatus || evs[0].Detail["status"] != "closed" {
		t.Fatalf("events %+v, want one task_status to closed", evs)
	}
	for _, id := range []string{"t-done", "t-judged"} {
		if evs, _ := s.TaskEvents(ctx, id, 10); len(evs) != 0 {
			t.Fatalf("%s recorded %+v after a rolled-back close", id, evs)
		}
	}
}
