package factoryd

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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
		{ID: "t-merged", Status: "merged", PRNumber: 9},
	} {
		tk.BudgetEUR, tk.Repository, tk.Kind = 1, "sf/r", "agent"
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask %s: %v", tk.ID, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET finished_at = now() - interval '20 minutes' WHERE id = 't-hold';
		UPDATE tasks SET finished_at = now() - interval '25 minutes' WHERE id = 't-nopr';
		UPDATE tasks SET finished_at = now() - interval '90 minutes' WHERE id = 't-era';
		UPDATE tasks SET finished_at = now() - interval '30 minutes' WHERE id = 't-merged'`); err != nil {
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
	board("t-merged")
	finished("t-nopr", "t-era")
	if live("t-hold") {
		t.Fatal("the carried card reads live before a green run covers it")
	}
	if !live("t-era") || !live("t-nopr") {
		t.Fatal("the grandfathered and the pull-request-less card read not live")
	}
	if live("t-merged") {
		t.Fatal("the uncarried merged card reads live: every row of its repository was written after its merge, but only a done card is grandfathered")
	}
	// a green head run after the carry run covers the change
	if err := s.RecordChainRun(ctx, "sf/r", 0, "e12", 12, "success"); err != nil {
		t.Fatalf("RecordChainRun run 12: %v", err)
	}
	board("t-merged")
	finished("t-hold", "t-nopr", "t-era")
	if !live("t-hold") {
		t.Fatal("the carried card reads not live once a green run covers it")
	}
}

// The reconciler over the real store, ending through the runner's EndCovered:
// a merged card a green run covers ends done, one the chain has not
// covered yet stays merged, the covered card's task_ended event and its
// done poke land once, and a second tick changes nothing. Requires a
// disposable Postgres (FACTORYD_TEST_DATABASE_URL).
func TestTheReconcilerEndsACoveredMergedCardOnce(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events, chain_runs, alerts, events,
		chat_messages, chat_conversations, chat_seen, chat_poke_cursor CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	for _, tk := range []Task{
		{ID: "t-live", PRNumber: 7},
		{ID: "t-wait", PRNumber: 8},
	} {
		tk.Status, tk.BudgetEUR, tk.Repository, tk.Kind, tk.Objective = "merged", 1, "sf/r", "agent", "Feed page"
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask %s: %v", tk.ID, err)
		}
	}
	// pr 7 rode the green run 10; pr 8 rides run 12, still running
	for _, row := range []struct {
		pr     int64
		sha    string
		run    int64
		status string
	}{
		{7, "e10", 10, "success"},
		{0, "e10", 10, "success"},
		{8, "e12", 12, "pending"},
		{0, "e12", 12, "pending"},
	} {
		if err := s.RecordChainRun(ctx, "sf/r", row.pr, row.sha, row.run, row.status); err != nil {
			t.Fatalf("RecordChainRun pr %d: %v", row.pr, err)
		}
	}
	hub := &forgejoHub{nextID: 100, mainHead: "m0"}
	fj := &Forgejo{BaseURL: hub.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	ll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"keys":[],"total_pages":0}`)
	}))
	t.Cleanup(ll.Close)
	r, err := NewRunner(testRunnerConfig(), s, newFakeKube(), fj, &Completion{Store: s, Forgejo: fj, LiteLLM: &LiteLLMAdmin{BaseURL: ll.URL, MasterKey: "mk"}},
		&fakeObjectStore{}, testSupervisorModel("http://127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	tree := policy.Tree{Projects: map[string]policy.ProjectConfig{"r": {Repository: "sf/r", Notify: []string{"bob"}}}}
	poker := &Poker{Store: s, Current: func() policy.Version { return policy.Version{Tree: tree} }, Line: &busyLine{busy: map[string]bool{}, landed: map[string][]int64{}}}
	if err := poker.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	p := &ChainPoller{Store: s, End: r.EndCovered}
	p.once()
	for range 2 {
		p.reconcile(ctx, "sf/r")
	}
	for id, want := range map[string]string{"t-live": "done", "t-wait": "merged"} {
		if got, _, err := s.Task(ctx, id); err != nil || got.Status != want {
			t.Fatalf("%s = %q, %v; want %s", id, got.Status, err, want)
		}
	}
	var ended []string
	rows, err := s.db.QueryContext(ctx, `SELECT task_id || ' ' || (detail->>'reason') FROM events WHERE kind = $1 ORDER BY id`, EventTaskEnded)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		ended = append(ended, e)
	}
	if !slices.Equal(ended, []string{"t-live live: covered by run 10"}) {
		t.Fatalf("task_ended rows = %q, want one for t-live naming run 10", ended)
	}
	if err := poker.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	inbox, err := s.EnsureInbox(ctx, "sf/r", "bob")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := s.ChatMessages(ctx, "sf/r", "bob", inbox, 10)
	if err != nil || len(msgs) != 1 || msgs[0].Content != "t-live is done and live: Feed page. €0.00 in all." || msgs[0].Repeats != 0 {
		t.Fatalf("bob's inbox = %+v, %v; want the one done poke of t-live, unrepeated", msgs, err)
	}
}

// A stalled head surfaces on the cards it holds (Task 6's ruling): the
// poller's stall writes the carried rows of the stalled commit too, so a
// merged card reads ChainState stalled — the board's amber — until the
// run concludes and its refresh clears it. Requires a disposable
// Postgres (FACTORYD_TEST_DATABASE_URL).
func TestStalledHeadSurfacesOnTheCard(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events, chain_runs CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	tk := Task{ID: "t-hold", Status: "merged", PRNumber: 7, BudgetEUR: 1, Repository: "sf/r", Kind: "agent"}
	if err := s.CreateTask(ctx, tk); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// run 15 carries pr 7 and sits pending as the head
	for _, row := range []struct {
		pr     int64
		sha    string
		run    int64
		status string
	}{
		{7, "e15", 15, "pending"},
		{0, "e15", 15, "pending"},
	} {
		if err := s.RecordChainRun(ctx, "sf/r", row.pr, row.sha, row.run, row.status); err != nil {
			t.Fatalf("RecordChainRun pr %d: %v", row.pr, err)
		}
	}
	p := &ChainPoller{Store: s, Policy: func() policy.Version { return policy.Version{} }, Own: Repo{Owner: "sf", Name: "r"}}
	p.stalled(ctx, "sf/r", "e15")
	state := func() string {
		t.Helper()
		b, ok, err := s.BoardTask(ctx, "t-hold")
		if err != nil || !ok {
			t.Fatalf("BoardTask = %v, %v", ok, err)
		}
		return b.ChainState
	}
	if got := state(); got != "stalled" {
		t.Fatalf("ChainState = %q, want stalled", got)
	}
	// the run concludes: the refresh the conclusion writes clears the card
	if err := s.RefreshChainStatus(ctx, "sf/r", "e15", "success"); err != nil {
		t.Fatalf("RefreshChainStatus: %v", err)
	}
	if got := state(); got != "success" {
		t.Fatalf("ChainState = %q, want the concluded run's status", got)
	}
}

// seedMergedCards truncates the store and files the cards the merge-commit
// reads are tested over.
func seedMergedCards(t *testing.T, ctx context.Context) *PGStore {
	t.Helper()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.db.Close() })
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events, chain_runs, alerts, events CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	for _, c := range []struct {
		id, repo, status, commit string
		pr                       int64
	}{
		{"t-a", "sf/r", "merged", "m-a", 7},
		{"t-b", "sf/r", "done", "m-b", 8},
		{"t-c", "sf/r", "in-review", "m-c", 9},
		{"t-nocommit", "sf/r", "merged", "", 10},
		{"t-nopr", "sf/r", "merged", "m-d", 0},
		{"t-other", "sf/other", "merged", "m-e", 7},
	} {
		tk := Task{ID: c.id, PRNumber: c.pr, Status: c.status, BudgetEUR: 1, Repository: c.repo, Kind: "agent", Objective: "x"}
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask %s: %v", c.id, err)
		}
		if c.commit != "" {
			if err := s.SetTaskMergeCommit(ctx, c.id, c.commit); err != nil {
				t.Fatal(err)
			}
		}
	}
	return s
}

// MergedCardCommits maps each of the given merge commits to its card's
// pull request: a card without a pull request or of another repository
// is left out, and a commit not asked for stays out. Requires a disposable
// Postgres (FACTORYD_TEST_DATABASE_URL).
func TestMergedCardCommitsListsMergedCards(t *testing.T) {
	ctx := context.Background()
	s := seedMergedCards(t, ctx)
	got, err := s.MergedCardCommits(ctx, "sf/r", []string{"m-a", "m-b", "m-c", "m-d", "m-e", "m-x"})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int64{"m-a": 7, "m-b": 8, "m-c": 9}; !maps.Equal(got, want) {
		t.Fatalf("MergedCardCommits = %v, want %v", got, want)
	}
	if got, err = s.MergedCardCommits(ctx, "sf/r", []string{"m-b"}); err != nil || !maps.Equal(got, map[string]int64{"m-b": 8}) {
		t.Fatalf("MergedCardCommits of m-b = %v, %v", got, err)
	}
}

// UncarriedMerged lists the merged cards with a pull request and no
// carry row, the merge commit recorded or not. Requires a disposable
// Postgres (FACTORYD_TEST_DATABASE_URL).
func TestUncarriedMergedListsOnlyRowlessCards(t *testing.T) {
	ctx := context.Background()
	s := seedMergedCards(t, ctx)
	ids := func(repository string) []string {
		t.Helper()
		cards, err := s.UncarriedMerged(ctx, repository)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range cards {
			out = append(out, c.ID)
		}
		return out
	}
	if got := ids("sf/r"); !slices.Equal(got, []string{"t-a", "t-nocommit"}) {
		t.Fatalf("UncarriedMerged = %v, want t-a and t-nocommit: merged with a pull request and no row, the merge commit or none", got)
	}
	if err := s.RecordChainRun(ctx, "sf/r", 7, "e1", 3, "success"); err != nil {
		t.Fatal(err)
	}
	if got := ids("sf/r"); !slices.Equal(got, []string{"t-nocommit"}) {
		t.Fatalf("UncarriedMerged = %v, want t-nocommit alone once t-a has a row", got)
	}
	if got := ids("sf/other"); !slices.Equal(got, []string{"t-other"}) {
		t.Fatalf("UncarriedMerged of sf/other = %v, want t-other: the row of pr 7 in sf/r is not its", got)
	}
}

// A card moved to merged carries the merge's stamp, the clock the
// uncarried alert reads; chain_uncarried rows stand once per card and
// resolve when their fingerprint no longer stands. Requires a disposable
// Postgres (FACTORYD_TEST_DATABASE_URL).
func TestUncarriedAlertRowsStandOncePerCard(t *testing.T) {
	ctx := context.Background()
	s := seedMergedCards(t, ctx)
	if err := s.TransitionTask(ctx, "t-c", []string{"in-review"}, "merged"); err != nil {
		t.Fatal(err)
	}
	cards, err := s.UncarriedMerged(ctx, "sf/r")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cards {
		if c.ID == "t-c" && (c.FinishedAt == nil || time.Since(*c.FinishedAt) > time.Minute) {
			t.Fatalf("t-c merged with finished_at %v, want the merge's stamp", c.FinishedAt)
		}
	}
	for _, id := range []string{"t-a", "t-a", "t-nocommit"} {
		if _, err := s.InsertAlert(ctx, Alert{Kind: AlertChainUncarried, Source: "sf/r", Fingerprint: "sf/r " + id, Detail: map[string]any{"card": id}}); err != nil {
			t.Fatal(err)
		}
	}
	open := func() int {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM alerts WHERE kind = $1 AND verdict IS NULL`, AlertChainUncarried).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := open(); n != 2 {
		t.Fatalf("%d open rows after t-a twice and t-nocommit, want 2", n)
	}
	if err := s.ResolveChainUncarried(ctx, "sf/r", []string{"sf/r t-nocommit"}); err != nil {
		t.Fatal(err)
	}
	if n := open(); n != 1 {
		t.Fatalf("%d open rows after t-a stood no more, want t-nocommit's alone", n)
	}
	if err := s.ResolveChainUncarried(ctx, "sf/other", nil); err != nil {
		t.Fatal(err)
	}
	if n := open(); n != 1 {
		t.Fatalf("%d open rows after another repository's resolve, want it untouched", n)
	}
	if err := s.ResolveChainUncarried(ctx, "sf/r", nil); err != nil {
		t.Fatal(err)
	}
	if n := open(); n != 0 {
		t.Fatalf("%d open rows after nothing stands, want none", n)
	}
}

// A merged card reads the issue of the newest roll that carries its pull
// request and is opened, or merged while the card is merged; a failed,
// refused or skipped request, another repository's, and a done card read
// 0. The roll task's carried cards are the cards its requests list.
// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL).
func TestRollIssueOfAMergedCard(t *testing.T) {
	ctx := context.Background()
	s := seedMergedCards(t, ctx)
	for _, r := range []struct {
		id, repo, status string
		issue            int64
	}{{"roll-old", "sf/r", "done", 1100}, {"roll-new", "sf/r", "running", 1162}} {
		if err := s.CreateTask(ctx, Task{ID: r.id, Status: r.status, BudgetEUR: 1, Repository: r.repo, Kind: "agent", Source: SourceRoll, IssueNumber: r.issue, Objective: "roll"}); err != nil {
			t.Fatal(err)
		}
	}
	req := func(sha, state, task string, prs ...int64) {
		t.Helper()
		if err := s.InsertRollRequest(ctx, RollRequest{SHA: sha, Repository: "sf/r", RunID: 1, State: state, TaskID: task, PRs: prs}); err != nil {
			t.Fatal(err)
		}
	}
	issue := func(id string) int64 {
		t.Helper()
		b, ok, err := s.BoardTask(ctx, id)
		if err != nil || !ok {
			t.Fatalf("BoardTask %s = %v, %v", id, ok, err)
		}
		if (b.RollID != "") != (b.RollIssue > 0) {
			t.Fatalf("RollID %q and RollIssue %d disagree", b.RollID, b.RollIssue)
		}
		return b.RollIssue
	}
	req("s1", "merged", "roll-old", 7)
	if got := issue("t-a"); got != 1100 {
		t.Fatalf("a merged card under a merged roll: RollIssue = %d, want 1100", got)
	}
	if b, _, _ := s.BoardTask(ctx, "t-a"); b.RollID != "roll-old" {
		t.Fatalf("RollID = %q, want roll-old", b.RollID)
	}
	req("s2", "opened", "roll-new", 7, 9)
	if got := issue("t-a"); got != 1162 {
		t.Fatalf("under an opened roll too: RollIssue = %d, want the newest, 1162", got)
	}
	if got := issue("t-b"); got != 0 {
		t.Fatalf("a request not carrying pr 8 gave t-b RollIssue %d, want 0", got)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE roll_requests SET state = 'failed' WHERE sha = 's2'`); err != nil {
		t.Fatal(err)
	}
	if got := issue("t-a"); got != 1100 {
		t.Fatalf("a failed request must not count: RollIssue = %d, want 1100", got)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'done' WHERE id = 't-a'`); err != nil {
		t.Fatal(err)
	}
	if got := issue("t-a"); got != 0 {
		t.Fatalf("a done card under a merged roll: RollIssue = %d, want 0", got)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE roll_requests SET state = 'opened' WHERE sha = 's2'`); err != nil {
		t.Fatal(err)
	}
	if got := issue("t-a"); got != 0 {
		t.Fatalf("an opened roll must not name a done card: RollIssue = %d, want 0", got)
	}
	if b, _, _ := s.BoardTask(ctx, "t-c"); b.RollID != "" {
		t.Fatalf("t-c is in review, not merged: RollID = %q, want none", b.RollID)
	}
	carried, err := s.RollCarries(ctx, "roll-new")
	if err != nil || len(carried) != 2 || carried[0].ID != "t-a" || carried[1].ID != "t-c" {
		t.Fatalf("RollCarries = %+v, %v; want t-a and t-c", carried, err)
	}
}

// A failed card whose pull request never merged stays in the finished
// page once the chain records runs, where deployLive alone would hide it.
// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL).
func TestFailedCardsStayInFinished(t *testing.T) {
	ctx := context.Background()
	s := seedMergedCards(t, ctx)
	now := time.Now()
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'failed', finished_at = $1 WHERE id = 't-c'`, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChainRun(ctx, "sf/r", 7, "e1", 1, "success"); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.FinishedTasks(ctx, FinishedQuery{Repository: "sf/r", Statuses: FinalStatuses, Since: now.Add(-time.Hour)})
	if err != nil || !slices.ContainsFunc(got, func(b BoardTask) bool { return b.ID == "t-c" }) {
		t.Fatalf("FinishedTasks = %+v, %v; want the failed t-c among them", got, err)
	}
}
