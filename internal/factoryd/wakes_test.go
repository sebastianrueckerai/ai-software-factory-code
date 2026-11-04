package factoryd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The Waker wakes the project agent's turn on the events the agent itself
// did not cause. These tests run on the store: the cursor, the marks and
// the timers are all rows.

// fakeWakeStarter records the wake turns the Waker starts.
type fakeWakeStarter struct {
	mu    sync.Mutex
	calls []wakeCall
	err   error
	gone  map[int64]bool // conversations whose venue is gone: the wake refuses for good
	// block holds the wakes of one project on release, a turn in flight
	// the Waker must not wait for
	block   string
	release chan struct{}
}

type wakeCall struct {
	project, login string
	conversation   int64
	birth, text    string
}

func (f *fakeWakeStarter) StartWake(ctx context.Context, project, login string, conversation int64, birth, text string) error {
	f.mu.Lock()
	f.calls = append(f.calls, wakeCall{project, login, conversation, birth, text})
	gone := f.gone[conversation]
	wait := f.block == project && f.release != nil
	err := f.err
	f.mu.Unlock()
	if wait {
		<-f.release
	}
	if gone {
		return fmt.Errorf("conversation %d is archived: %w", conversation, ErrWakeVenueGone)
	}
	return err
}

func (f *fakeWakeStarter) recorded() []wakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// newWakeRig opens the store and one Waker over projects on a movable
// clock, then runs a first pass over no events, so every project's cursor
// is seeded before any event lands: no history wakes. A nil map is the one
// autonomous project wake-proj of sf/wake with member sebastian.
func newWakeRig(t *testing.T, projects map[string]policy.ProjectConfig) (*Waker, *PGStore, *fakeWakeStarter, *time.Time) {
	t.Helper()
	s, err := OpenStore(t.Context(), schematest.DSN(t, "waker"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.db.Close() })
	for _, table := range []string{"events", "tasks", "chat_messages", "chat_conversations", "chat_watches", "chat_timers",
		"chat_wake_cursor", "chat_wake_state", "chat_intake", "roadmap_pull"} {
		if _, err := s.db.ExecContext(t.Context(), `TRUNCATE `+table+` CASCADE`); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	if projects == nil {
		projects = map[string]policy.ProjectConfig{
			"wake-proj": {Repository: "sf/wake", Autonomy: policy.AutonomyAutonomous, Notify: []string{"sebastian"}},
		}
	}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	clock := &now
	starter := &fakeWakeStarter{}
	w := &Waker{
		Store: s, Start: starter,
		Now:     func() time.Time { return *clock },
		Current: func() policy.Version { return policy.Version{Tree: policy.Tree{Projects: projects}} },
	}
	if err := drainPass(t, w); err != nil {
		t.Fatalf("the seeding pass: %v", err)
	}
	return w, s, starter, clock
}

// wakeAsk records one ask of task on repository, via chat when the agent
// itself filed it, and returns the event's id.
func wakeAsk(t *testing.T, s *PGStore, repository, task, ask, via string) int64 {
	t.Helper()
	id, err := s.Record(t.Context(), Event{Repository: repository, TaskID: task, Kind: EventQuestionAsked, Via: via,
		Detail: map[string]any{"ask": ask}})
	if err != nil {
		t.Fatalf("record the ask: %v", err)
	}
	return id
}

// wakeAskComment records one ask of task on repository that filed the
// issue comment id, and returns the event's id.
func wakeAskComment(t *testing.T, s *PGStore, repository, task, ask string, comment int64, via string) int64 {
	t.Helper()
	id, err := s.Record(t.Context(), Event{Repository: repository, TaskID: task, Kind: EventQuestionAsked, Via: via,
		Detail: map[string]any{"ask": ask, "comment": comment}})
	if err != nil {
		t.Fatalf("record the ask: %v", err)
	}
	return id
}

// wakeCursorOf reads repository's wake cursor, 0 with no row.
func wakeCursorOf(t *testing.T, s *PGStore, repository string) int64 {
	t.Helper()
	var c int64
	err := s.db.QueryRowContext(t.Context(), `SELECT event_id FROM chat_wake_cursor WHERE repository = $1`, repository).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatalf("read the wake cursor: %v", err)
	}
	return c
}

// wakeStateKeys reads repository's mark keys, sorted.
func wakeStateKeys(t *testing.T, s *PGStore, repository string) []string {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), `SELECT key FROM chat_wake_state WHERE repository = $1 ORDER BY key`, repository)
	if err != nil {
		t.Fatalf("read the wake marks: %v", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan a wake mark: %v", err)
		}
		keys = append(keys, k)
	}
	return keys
}

// wakeAskMarked says a mark of task's ask sits in the keys.
func wakeAskMarked(keys []string, task string) bool {
	return slices.ContainsFunc(keys, func(k string) bool { return strings.HasPrefix(k, "ask:"+task+":") })
}

// An ask the agent did not file wakes one turn on the venue member's
// inbox, its birth kind wake_ask, and the cursor moves past the event.
func TestWakerWakesOnAskAtAutonomous(t *testing.T) {
	w, s, starter, _ := newWakeRig(t, nil)
	id := wakeAsk(t, s, "sf/wake", "t-1", "Raise the month cap?", ViaFactoryd)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	calls := starter.recorded()
	if len(calls) != 1 {
		t.Fatalf("%d wakes, want 1: %+v", len(calls), calls)
	}
	conv, err := s.EnsureInbox(t.Context(), "sf/wake", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	c := calls[0]
	if c.project != "wake-proj" || c.login != "sebastian" || c.conversation != conv || c.birth != "wake_ask" ||
		!strings.Contains(c.text, "Raise the month cap?") {
		t.Fatalf("the wake = %+v, want wake-proj sebastian on %d about the ask", c, conv)
	}
	if got := wakeCursorOf(t, s, "sf/wake"); got < id {
		t.Fatalf("the cursor %d stayed behind the event %d", got, id)
	}
}

// An ask the chat agent itself filed wakes nobody, and a project below
// autonomous wakes nobody however the ask was filed.
func TestWakerIgnoresChatFiledAsksAndManualProjects(t *testing.T) {
	w, s, starter, _ := newWakeRig(t, map[string]policy.ProjectConfig{
		"auto":     {Repository: "sf/auto", Autonomy: policy.AutonomyAutonomous, Notify: []string{"sebastian"}},
		"assisted": {Repository: "sf/assisted", Autonomy: policy.AutonomyAssisted, Notify: []string{"sebastian"}},
		"manual":   {Repository: "sf/manual", Autonomy: policy.AutonomyManual, Notify: []string{"sebastian"}},
	})
	idAuto := wakeAsk(t, s, "sf/auto", "t-1", "Raise the month cap?", ViaChat)
	idAssisted := wakeAsk(t, s, "sf/assisted", "t-2", "Fund the build?", ViaFactoryd)
	idManual := wakeAsk(t, s, "sf/manual", "t-3", "Merge the branch?", ViaFactoryd)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 0 {
		t.Fatalf("%d wakes, want 0: %+v", len(calls), calls)
	}
	for repo, id := range map[string]int64{"sf/auto": idAuto, "sf/assisted": idAssisted, "sf/manual": idManual} {
		if got := wakeCursorOf(t, s, repo); got < id {
			t.Fatalf("%s's cursor %d stayed behind its event %d", repo, got, id)
		}
	}
}

// Several asks of one batch ride one wake whose text names them all, and
// every ask carries its mark.
func TestWakerBatchesAsks(t *testing.T) {
	w, s, starter, _ := newWakeRig(t, nil)
	wakeAsk(t, s, "sf/wake", "t-1", "Raise the month cap?", ViaFactoryd)
	wakeAsk(t, s, "sf/wake", "t-2", "Fund the build?", ViaFactoryd)
	wakeAsk(t, s, "sf/wake", "t-3", "Merge the branch?", ViaFactoryd)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	calls := starter.recorded()
	if len(calls) != 1 {
		t.Fatalf("%d wakes, want 1: %+v", len(calls), calls)
	}
	text := calls[0].text
	for _, want := range []string{"Raise the month cap?", "Fund the build?", "Merge the branch?", "t-1", "t-2", "t-3"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the wake text %q misses %q", text, want)
		}
	}
	keys := wakeStateKeys(t, s, "sf/wake")
	for _, task := range []string{"t-1", "t-2", "t-3"} {
		if !wakeAskMarked(keys, task) {
			t.Fatalf("no ask mark for %s among %v", task, keys)
		}
	}
}

// An ask that carries no comment of its own falls back to its sentence:
// the same text of the same card again wakes no second time.
func TestWakerNoSecondAttemptOnRepeatedAsk(t *testing.T) {
	w, s, starter, _ := newWakeRig(t, nil)
	wakeAsk(t, s, "sf/wake", "t-1", "Raise the month cap?", ViaFactoryd)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	wakeAsk(t, s, "sf/wake", "t-1", "Raise the month cap?", ViaFactoryd)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 1 {
		t.Fatalf("%d wakes, want 1: %+v", len(calls), calls)
	}
}

// A new ask that repeats a sentence wakes again: the mark keys the ask's
// own comment, so two asks of one sentence are two asks.
func TestWakerANewAskThatRepeatsASentenceWakesAgain(t *testing.T) {
	w, s, starter, clock := newWakeRig(t, nil)
	wakeAskComment(t, s, "sf/wake", "t-1", "Raise the month cap?", 27247, ViaFactoryd)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	wakeAskComment(t, s, "sf/wake", "t-1", "Raise the month cap?", 27258, ViaFactoryd)
	*clock = clock.Add(11 * time.Second)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 2 {
		t.Fatalf("%d wakes, want 2: %+v", len(calls), calls)
	}
}

// The same ask again, the same comment however it reappears, is the one
// the mark holds: no second wake.
func TestWakerTheSameAskWakesOnce(t *testing.T) {
	w, s, starter, clock := newWakeRig(t, nil)
	wakeAskComment(t, s, "sf/wake", "t-1", "Raise the month cap?", 27247, ViaFactoryd)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	wakeAskComment(t, s, "sf/wake", "t-1", "Raise the month cap?", 27247, ViaFactoryd)
	*clock = clock.Add(11 * time.Second)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 1 {
		t.Fatalf("%d wakes, want 1: %+v", len(calls), calls)
	}
}

// wakeApproval records one approval row of task #issue on repository,
// detail its kind and what the kind carries, and returns the event's id.
func wakeApproval(t *testing.T, s *PGStore, repository, task string, issue int64, detail map[string]any) int64 {
	t.Helper()
	id, err := s.Record(t.Context(), Event{Repository: repository, TaskID: task, IssueNumber: issue,
		Kind: EventApprovalRequested, Via: ViaFactoryd, Detail: detail})
	if err != nil {
		t.Fatalf("record the approval: %v", err)
	}
	return id
}

// mergeRow is a merge approval row's detail as the merge gate files it.
func mergeRow(approval, pr int64, class string, red bool) map[string]any {
	return map[string]any{"approval": approval, "kind": ApprovalMerge, "pr": pr, "class": class, "factory": false, "red_checks": red}
}

// A merge approval row at autonomous wakes one turn on the venue inbox
// like an ask: its line names the pull request and the risk, and at merge
// rule human the turn records what it found, the merge staying a human's
// even for an approver.
func TestWakeMergeRowWakesAtAutonomous(t *testing.T) {
	w, s, starter, _ := newWakeRig(t, map[string]policy.ProjectConfig{
		"wake-proj": {Repository: "sf/wake", Autonomy: policy.AutonomyAutonomous, MergeRule: policy.MergeHuman,
			Notify: []string{"sebastian"}, Approvers: []string{"sebastian"}},
	})
	id := wakeApproval(t, s, "sf/wake", "t-1", 40, mergeRow(3, 12, "medium", false))
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	conv, err := s.EnsureInbox(t.Context(), "sf/wake", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	want := "Wake: 1 merge waits for a human.\n#40: merge of PR #12, medium risk\n\n" +
		"Read each card. Settle what your permissions allow. A card waiting on its budget takes fund from your pool. " +
		"For what stays the human's, record what you found with attach_analysis. An ask already answered is done. " +
		"A human approves merges here."
	calls := starter.recorded()
	if len(calls) != 1 {
		t.Fatalf("%d wakes, want 1: %+v", len(calls), calls)
	}
	if c := calls[0]; c.project != "wake-proj" || c.login != "sebastian" || c.conversation != conv || c.birth != "wake_ask" || c.text != want {
		t.Fatalf("the wake = %+v, want wake-proj sebastian on %d, wake_ask, text %q", c, conv, want)
	}
	if keys := wakeStateKeys(t, s, "sf/wake"); !slices.Equal(keys, []string{"approval:t-1:3", "turn"}) {
		t.Fatalf("the marks are %v, want the approval's and the turn's", keys)
	}
	if got := wakeCursorOf(t, s, "sf/wake"); got < id {
		t.Fatalf("the cursor %d stayed behind the event %d", got, id)
	}
}

// At full auto, autonomous with merge rule auto, the woken approver may
// settle the merge itself: the turn names approve_merge and deny_merge. A
// woken login that approves nothing still leaves the merge to a human.
// Red checks ride the merge line, and so does a GitHub sync's human
// approval, which no agent gives; another pull request factoryd opened
// takes the agent's.
func TestWakeMergeRowWakesAtFullAuto(t *testing.T) {
	for _, tc := range []struct {
		approvers []string
		source    string
		want      string
	}{
		{[]string{"sebastian"}, "", "Wake: 1 merge waits for a human.\n#40: merge of PR #12, medium risk, checks red\n\n" +
			"Read each card. Settle what your permissions allow. A card waiting on its budget takes fund from your pool. " +
			"For what stays the human's, record what you found with attach_analysis. An ask already answered is done. " +
			"A merge you find safe takes approve_merge, a wrong one deny_merge, and the rest goes to a human with attach_analysis."},
		{[]string{"morgan"}, "", "Wake: 1 merge waits for a human.\n#40: merge of PR #12, medium risk, checks red\n\n" +
			"Read each card. Settle what your permissions allow. " +
			"For what stays the human's, record what you found with attach_analysis. An ask already answered is done. " +
			"A human approves merges here."},
		{[]string{"sebastian"}, SourceGitHubSync, "Wake: 1 merge waits for a human.\n#40: merge of PR #12, medium risk, checks red, a human approves\n\n" +
			"Read each card. Settle what your permissions allow. A card waiting on its budget takes fund from your pool. " +
			"For what stays the human's, record what you found with attach_analysis. An ask already answered is done. " +
			"A merge you find safe takes approve_merge, a wrong one deny_merge, and the rest goes to a human with attach_analysis."},
		{[]string{"sebastian"}, SourceTemplateUpdate, "Wake: 1 merge waits for a human.\n#40: merge of PR #12, medium risk, checks red\n\n" +
			"Read each card. Settle what your permissions allow. A card waiting on its budget takes fund from your pool. " +
			"For what stays the human's, record what you found with attach_analysis. An ask already answered is done. " +
			"A merge you find safe takes approve_merge, a wrong one deny_merge, and the rest goes to a human with attach_analysis."},
	} {
		w, s, starter, _ := newWakeRig(t, map[string]policy.ProjectConfig{
			"wake-proj": {Repository: "sf/wake", Autonomy: policy.AutonomyAutonomous, MergeRule: policy.MergeAuto,
				Notify: []string{"sebastian"}, Approvers: tc.approvers},
		})
		row := mergeRow(3, 12, "medium", true)
		if tc.source != "" {
			if err := s.CreateTask(t.Context(), Task{ID: "t-1", Kind: "agent", Status: "in-review", Repository: "sf/wake",
				IssueNumber: 40, Objective: "Sync from GitHub", Track: "direct", Source: tc.source}); err != nil {
				t.Fatal(err)
			}
			row["factory"] = true
		}
		id := wakeApproval(t, s, "sf/wake", "t-1", 40, row)
		if err := drainPass(t, w); err != nil {
			t.Fatalf("Pass: %v", err)
		}
		calls := starter.recorded()
		if len(calls) != 1 || calls[0].birth != "wake_ask" || calls[0].text != tc.want {
			t.Errorf("approvers %v, source %q: the wakes are %+v, want one wake_ask with text %q", tc.approvers, tc.source, calls, tc.want)
		}
		if keys := wakeStateKeys(t, s, "sf/wake"); !slices.Equal(keys, []string{"approval:t-1:3", "turn"}) {
			t.Errorf("approvers %v, source %q: the marks are %v, want the approval's and the turn's", tc.approvers, tc.source, keys)
		}
		if got := wakeCursorOf(t, s, "sf/wake"); got < id {
			t.Errorf("approvers %v, source %q: the cursor %d stayed behind the event %d", tc.approvers, tc.source, got, id)
		}
	}
}

// Below autonomous a merge row wakes nobody, whatever the merge rule: the
// event only moves the cursor.
func TestWakeMergeRowSleepsBelowAutonomous(t *testing.T) {
	w, s, starter, _ := newWakeRig(t, map[string]policy.ProjectConfig{
		"assisted": {Repository: "sf/assisted", Autonomy: policy.AutonomyAssisted, MergeRule: policy.MergeAuto,
			Notify: []string{"sebastian"}, Approvers: []string{"sebastian"}},
		"manual": {Repository: "sf/manual", Autonomy: policy.AutonomyManual, MergeRule: policy.MergeHuman,
			Notify: []string{"sebastian"}, Approvers: []string{"sebastian"}},
	})
	ids := map[string]int64{
		"sf/assisted": wakeApproval(t, s, "sf/assisted", "t-1", 40, mergeRow(3, 12, "medium", false)),
		"sf/manual":   wakeApproval(t, s, "sf/manual", "t-2", 41, mergeRow(4, 13, "low", false)),
	}
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 0 {
		t.Fatalf("%d wakes, want 0: %+v", len(calls), calls)
	}
	for repo, id := range ids {
		if got := wakeCursorOf(t, s, repo); got < id {
			t.Fatalf("%s's cursor %d stayed behind its event %d", repo, got, id)
		}
		if keys := wakeStateKeys(t, s, repo); len(keys) != 0 {
			t.Fatalf("%s carries marks %v, want none", repo, keys)
		}
	}
}

// Only merge rows wake: a plan's, a criteria gap's and a criteria rerun's
// approval rows start no turn, the cursor passes them, and an ask beside
// them wakes alone.
func TestWakePlanAndCriteriaRowsOnlyMoveTheCursor(t *testing.T) {
	w, s, starter, _ := newWakeRig(t, nil)
	var last int64
	for i, kind := range []string{ApprovalPlan, ApprovalCriteria, ApprovalCriteriaRerun} {
		last = wakeApproval(t, s, "sf/wake", "t-plan", 50, map[string]any{"approval": int64(i + 1), "kind": kind})
	}
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 0 {
		t.Fatalf("%d wakes, want 0: %+v", len(calls), calls)
	}
	if got := wakeCursorOf(t, s, "sf/wake"); got < last {
		t.Fatalf("the cursor %d stayed behind the event %d", got, last)
	}
	wakeApproval(t, s, "sf/wake", "t-plan", 50, map[string]any{"approval": int64(4), "kind": ApprovalPlan})
	wakeAsk(t, s, "sf/wake", "t-1", "Use a replica?", ViaFactoryd)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	want := "Wake: 1 ask waits for a human.\nt-1: Use a replica?\n\n" +
		"Read each card. Settle what your permissions allow. " +
		"For what stays the human's, record what you found with attach_analysis. An ask already answered is done."
	if calls := starter.recorded(); len(calls) != 1 || calls[0].text != want {
		t.Fatalf("the wakes are %+v, want one with text %q", calls, want)
	}
}

// Two merge rows of one card are two approvals: each keys its own mark,
// so each wakes its own turn, and the same approval again wakes no more.
func TestWakeTwoMergeRowsOfOneCardWakeOnceEach(t *testing.T) {
	w, s, starter, clock := newWakeRig(t, nil)
	wakeApproval(t, s, "sf/wake", "t-1", 40, mergeRow(3, 12, "medium", false))
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	wakeApproval(t, s, "sf/wake", "t-1", 40, mergeRow(4, 12, "medium", false))
	*clock = clock.Add(11 * time.Second)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	wakeApproval(t, s, "sf/wake", "t-1", 40, mergeRow(4, 12, "medium", false))
	*clock = clock.Add(11 * time.Second)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	calls := starter.recorded()
	if len(calls) != 2 {
		t.Fatalf("%d wakes, want 2: %+v", len(calls), calls)
	}
	for i, c := range calls {
		if !strings.Contains(c.text, "\n#40: merge of PR #12, medium risk\n") {
			t.Fatalf("wake %d's text %q misses the merge line", i, c.text)
		}
	}
	if keys := wakeStateKeys(t, s, "sf/wake"); !slices.Equal(keys, []string{"approval:t-1:3", "approval:t-1:4", "turn"}) {
		t.Fatalf("the marks are %v, want both approvals' and the turn's", keys)
	}
}

// An ask and a merge row of one pass ride one turn that names both, each
// marked under its own key.
func TestWakeMergeAndAskBatchInOneTurn(t *testing.T) {
	w, s, starter, _ := newWakeRig(t, map[string]policy.ProjectConfig{
		"wake-proj": {Repository: "sf/wake", Autonomy: policy.AutonomyAutonomous, MergeRule: policy.MergeHuman,
			Notify: []string{"sebastian"}},
	})
	wakeAskComment(t, s, "sf/wake", "t-1", "Use a replica?", 27247, ViaFactoryd)
	wakeApproval(t, s, "sf/wake", "t-2", 41, mergeRow(5, 13, "high", false))
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	want := "Wake: 1 ask and 1 merge wait for a human.\nt-1: Use a replica?\n#41: merge of PR #13, high risk\n\n" +
		"Read each card. Settle what your permissions allow. " +
		"For what stays the human's, record what you found with attach_analysis. An ask already answered is done. " +
		"A human approves merges here."
	calls := starter.recorded()
	if len(calls) != 1 || calls[0].birth != "wake_ask" || calls[0].text != want {
		t.Fatalf("the wakes are %+v, want one wake_ask with text %q", calls, want)
	}
	if keys := wakeStateKeys(t, s, "sf/wake"); !slices.Equal(keys, []string{"approval:t-2:5", "ask:t-1:27247", "turn"}) {
		t.Fatalf("the marks are %v, want the ask's, the approval's and the turn's", keys)
	}
}

// drainPass runs one pass and waits out the wake turns it started, so a
// test reads the store only after their bookkeeping landed.
func drainPass(t *testing.T, w *Waker) error {
	t.Helper()
	err := w.Pass(t.Context())
	w.drain()
	return err
}

// A wake turn runs on its own goroutine: Pass returns while it runs,
// another project's timer fires in the same pass, the running wake's own
// timer row stands until its turn ends, and no second wake of that
// project starts beside it.
func TestWakerPassesWhileAWakeTurnRuns(t *testing.T) {
	w, s, starter, clock := newWakeRig(t, map[string]policy.ProjectConfig{
		"a-proj": {Repository: "sf/a", Autonomy: policy.AutonomyAutonomous, Notify: []string{"sebastian"}},
		"b-proj": {Repository: "sf/b", Autonomy: policy.AutonomyAutonomous, Notify: []string{"sebastian"}},
	})
	convA, err := s.NewConversation(t.Context(), "sf/a", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	convB, err := s.NewConversation(t.Context(), "sf/b", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	for _, t2 := range []struct {
		repo string
		conv int64
		note string
	}{
		{"sf/a", convA, "Check A."},
		{"sf/b", convB, "Check B."},
	} {
		if err := s.AddChatTimer(t.Context(), t2.repo, "sebastian", t2.conv, clock.Add(-time.Minute), t2.note, ""); err != nil {
			t.Fatal(err)
		}
	}
	starter.block, starter.release = "a-proj", make(chan struct{})
	pass := make(chan error, 1)
	go func() { pass <- w.Pass(t.Context()) }()
	select {
	case err := <-pass:
		if err != nil {
			t.Fatalf("Pass: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Pass waited for the wake turn to run")
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if calls := starter.recorded(); len(calls) == 2 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("%d wakes, want both projects': %+v", len(calls), calls)
		}
	}
	if due, err := s.DueChatTimers(t.Context(), "sf/a", *clock); err != nil || len(due) != 1 {
		t.Fatalf("a's due timers = %v, %v, want its row while its wake runs", due, err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		due, err := s.DueChatTimers(t.Context(), "sf/b", *clock)
		if err != nil {
			t.Fatal(err)
		}
		if len(due) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("b's timer row outlived its wake turn")
		}
	}
	close(starter.release)
	w.drain()
	if due, err := s.DueChatTimers(t.Context(), "sf/a", *clock); err != nil || len(due) != 0 {
		t.Fatalf("a's due timers = %v, %v, want none once its wake ran", due, err)
	}
}

// A second wake inside ten seconds of the last one does not start and the
// cursor waits; eleven seconds later it starts.
func TestWakerTurnRateCaps(t *testing.T) {
	w, s, starter, clock := newWakeRig(t, nil)
	wakeAsk(t, s, "sf/wake", "t-1", "Raise the month cap?", ViaFactoryd)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	capped := wakeCursorOf(t, s, "sf/wake")
	id := wakeAsk(t, s, "sf/wake", "t-2", "Fund the build?", ViaFactoryd)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 1 {
		t.Fatalf("%d wakes inside the turn mark, want 1: %+v", len(calls), calls)
	}
	if got := wakeCursorOf(t, s, "sf/wake"); got != capped {
		t.Fatalf("the cursor moved to %d while capped at %d", got, capped)
	}
	*clock = clock.Add(11 * time.Second)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	calls := starter.recorded()
	if len(calls) != 2 || !strings.Contains(calls[1].text, "Fund the build?") {
		t.Fatalf("the second wake after the mark = %+v", calls)
	}
	if got := wakeCursorOf(t, s, "sf/wake"); got < id {
		t.Fatalf("the cursor %d stayed behind the event %d", got, id)
	}
}

// A wake the console refuses because the chat stands at its budget
// (ErrChatUnfunded) is held as a refused one, whatever woke it: no card is
// filed, the cooldown is marked, and the cause stays (the cursor behind
// the ask or the move, the timer's row), so once the chat is funded the
// pass after the cooldown wakes the same turn.
func TestWakerHoldsWhatAStoppedChatRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, birth, text string
		cause             func(t *testing.T, s *PGStore, at time.Time) int64
	}{
		{"an ask", "wake_ask", "Use a replica?", func(t *testing.T, s *PGStore, _ time.Time) int64 {
			return wakeAsk(t, s, "sf/wake", "t-1", "Use a replica?", ViaFactoryd)
		}},
		{"a watched card", "wake_card", "The watched card t-1 moved", func(t *testing.T, s *PGStore, _ time.Time) int64 {
			conv, err := s.NewConversation(t.Context(), "sf/wake", "sebastian")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.AddChatWatch(t.Context(), "sf/wake", "sebastian", conv, "t-1"); err != nil {
				t.Fatal(err)
			}
			return wakeMove(t, s, "sf/wake", "t-1", ViaFactoryd)
		}},
		{"a timer", "wake_timer", "Timer: Check the build.", func(t *testing.T, s *PGStore, at time.Time) int64 {
			conv, err := s.NewConversation(t.Context(), "sf/wake", "sebastian")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.AddChatTimer(t.Context(), "sf/wake", "sebastian", conv, at.Add(-time.Minute), "Check the build.", ""); err != nil {
				t.Fatal(err)
			}
			return 0
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, s, starter, clock := newWakeRig(t, map[string]policy.ProjectConfig{
				"wake-proj": {Repository: "sf/wake", Autonomy: policy.AutonomyAutonomous, Notify: []string{"sebastian"}},
			})
			starter.err = fmt.Errorf("the chat of wake-proj stands at its budget of 15.00 EUR: %w", ErrChatUnfunded)
			id := tc.cause(t, s, *clock)
			due := func() int {
				timers, err := s.DueChatTimers(t.Context(), "sf/wake", *clock)
				if err != nil {
					t.Fatal(err)
				}
				return len(timers)
			}
			timers := due()
			for i := range 2 {
				if err := drainPass(t, w); err != nil {
					t.Fatalf("pass %d: %v", i+1, err)
				}
				var cards int
				if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM tasks`).Scan(&cards); err != nil {
					t.Fatal(err)
				}
				if calls := starter.recorded(); len(calls) != 1 || cards != 0 || due() != timers ||
					!slices.Contains(wakeStateKeys(t, s, "sf/wake"), "cooldown") || id > 0 && wakeCursorOf(t, s, "sf/wake") >= id {
					t.Fatalf("pass %d: wakes %+v, %d cards, %d due timers of %d, marks %v, cursor %d of the cause %d; want the one refused wake, "+
						"no card, the timer kept, the cooldown and the cursor behind", i+1, calls, cards, due(), timers,
						wakeStateKeys(t, s, "sf/wake"), wakeCursorOf(t, s, "sf/wake"), id)
				}
			}
			*clock = clock.Add(61 * time.Second)
			starter.err = nil
			if err := drainPass(t, w); err != nil {
				t.Fatal(err)
			}
			if calls := starter.recorded(); len(calls) != 2 || calls[1].birth != tc.birth || !strings.Contains(calls[1].text, tc.text) ||
				due() != 0 || wakeCursorOf(t, s, "sf/wake") < id {
				t.Fatalf("the funded chat's pass: wakes %+v, %d due timers, cursor %d; want the %s wake, no timer, the cursor past %d",
					calls, due(), wakeCursorOf(t, s, "sf/wake"), tc.birth, id)
			}
		})
	}
}

// A factory ask wakes the agent, the roll ladder's for the analysis it
// attaches; a month cap's ask wakes nobody, since the cap refuses the
// agent's own calls, and the cursor moves past it.
func TestWakerWakesOnFactoryAsksButNotTheMonthCap(t *testing.T) {
	w, s, starter, _ := newWakeRig(t, nil)
	ask := func(task, ask, source string) int64 {
		t.Helper()
		id, err := s.Record(t.Context(), Event{Repository: "sf/wake", TaskID: task, Kind: EventQuestionAsked, Via: ViaFactoryd,
			Detail: map[string]any{"ask": ask, "source": source}})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := ask("t-cap", "The month cap is reached. Raise it?", SourceMonthCap)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 0 || wakeCursorOf(t, s, "sf/wake") < id {
		t.Fatalf("wakes %+v, cursor %d; want none and the cursor past %d", calls, wakeCursorOf(t, s, "sf/wake"), id)
	}
	ask("t-roll", "3 rolls failed since the last one merged. Retry the roll?", SourceRollFailed)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 1 || !strings.Contains(calls[0].text, "rolls failed since the last one merged") {
		t.Fatalf("wakes %+v, want one on the roll's ask", calls)
	}
}

// A wake that fails leaves the cursor behind and cools the project down a
// minute; inside the cooldown no new start, after it the same cause
// starts.
func TestWakerFailedTurnRetriesColdly(t *testing.T) {
	w, s, starter, clock := newWakeRig(t, nil)
	starter.err = errors.New("the wake did not run")
	id := wakeAsk(t, s, "sf/wake", "t-1", "Raise the month cap?", ViaFactoryd)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 1 {
		t.Fatalf("%d starts, want the one failed start", len(calls))
	}
	if got := wakeCursorOf(t, s, "sf/wake"); got >= id {
		t.Fatalf("the cursor moved to %d past the unhandled event %d", got, id)
	}
	if !slices.Contains(wakeStateKeys(t, s, "sf/wake"), "cooldown") {
		t.Fatalf("no cooldown mark among %v", wakeStateKeys(t, s, "sf/wake"))
	}
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 1 {
		t.Fatalf("%d starts inside the cooldown, want 1", len(calls))
	}
	*clock = clock.Add(61 * time.Second)
	starter.err = nil
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	calls := starter.recorded()
	if len(calls) != 2 || !strings.Contains(calls[1].text, "Raise the month cap?") {
		t.Fatalf("the retry after the cooldown = %+v", calls)
	}
	if got := wakeCursorOf(t, s, "sf/wake"); got < id {
		t.Fatalf("the cursor %d stayed behind the event %d", got, id)
	}
}

// The wake text lists each ask with its card ref, reads the ask the event
// carries (the question key the real askers write) and tells the turn
// what to do, attach_analysis named as the way to record what it found.
func TestWakeAskTextNamesCardsAndTools(t *testing.T) {
	w, s, starter, _ := newWakeRig(t, nil)
	if err := s.CreateTask(t.Context(), Task{ID: "t-1", Kind: "agent", Status: "waiting-human",
		Repository: "sf/wake", IssueNumber: 40, Objective: "Use a replica?", Track: "direct"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(t.Context(), Event{Repository: "sf/wake", TaskID: "t-1", IssueNumber: 40,
		Kind: EventQuestionAsked, Via: ViaFactoryd, Detail: map[string]any{"question": "Use a replica?"}}); err != nil {
		t.Fatal(err)
	}
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	calls := starter.recorded()
	if len(calls) != 1 {
		t.Fatalf("%d wakes, want 1: %+v", len(calls), calls)
	}
	text := calls[0].text
	for _, want := range []string{
		"Wake: 1 ask waits for a human.",
		"#40: Use a replica?",
		"\n\nRead each card. Settle what your permissions allow. For what stays the human's, record what you found with attach_analysis. An ask already answered is done.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the wake text %q misses %q", text, want)
		}
	}
}

// The wake line counts asks and merge rows apart, each in the singular
// and the plural: one ask waits, two asks wait, two merges wait, and a
// batch of both names each.
func TestWakeAskTextCountsAsks(t *testing.T) {
	w, s, starter, clock := newWakeRig(t, nil)
	wakeAsk(t, s, "sf/wake", "t-1", "Raise the month cap?", ViaFactoryd)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	for _, batch := range [][]func(){
		{func() { wakeAsk(t, s, "sf/wake", "t-2", "Fund the build?", ViaFactoryd) },
			func() { wakeAsk(t, s, "sf/wake", "t-3", "Merge the branch?", ViaFactoryd) }},
		{func() { wakeApproval(t, s, "sf/wake", "t-4", 44, mergeRow(4, 14, "low", false)) },
			func() { wakeApproval(t, s, "sf/wake", "t-5", 45, mergeRow(5, 15, "low", false)) }},
		{func() { wakeAsk(t, s, "sf/wake", "t-6", "Keep the cache?", ViaFactoryd) },
			func() { wakeApproval(t, s, "sf/wake", "t-7", 47, mergeRow(7, 17, "low", false)) },
			func() { wakeApproval(t, s, "sf/wake", "t-8", 48, mergeRow(8, 18, "low", false)) }},
		{func() { wakeAsk(t, s, "sf/wake", "t-9", "Drop the index?", ViaFactoryd) },
			func() { wakeAsk(t, s, "sf/wake", "t-10", "Rename the table?", ViaFactoryd) },
			func() { wakeApproval(t, s, "sf/wake", "t-11", 51, mergeRow(11, 21, "low", false)) }},
	} {
		for _, record := range batch {
			record()
		}
		*clock = clock.Add(11 * time.Second)
		if err := drainPass(t, w); err != nil {
			t.Fatalf("Pass: %v", err)
		}
	}
	calls := starter.recorded()
	if len(calls) != 5 {
		t.Fatalf("%d wakes, want 5: %+v", len(calls), calls)
	}
	for i, want := range []string{
		"Wake: 1 ask waits for a human.",
		"Wake: 2 asks wait for a human.",
		"Wake: 2 merges wait for a human.",
		"Wake: 1 ask and 2 merges wait for a human.",
		"Wake: 2 asks and 1 merge wait for a human.",
	} {
		if !strings.HasPrefix(calls[i].text, want) {
			t.Fatalf("wake %d's text %q does not open %q", i, calls[i].text, want)
		}
	}
}

// The marks and the cursor are rows, so a restart changes nothing: what
// one Waker handled stays handled, and what it left blocked still starts.
func TestWakerMarksSurviveRestart(t *testing.T) {
	w1, s, starter1, clock := newWakeRig(t, nil)
	wakeAsk(t, s, "sf/wake", "t-1", "Raise the month cap?", ViaFactoryd)
	if err := drainPass(t, w1); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	wakeAsk(t, s, "sf/wake", "t-2", "Fund the build?", ViaFactoryd)
	if err := drainPass(t, w1); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter1.recorded(); len(calls) != 1 {
		t.Fatalf("%d wakes, want the one before the restart", len(calls))
	}
	starter2 := &fakeWakeStarter{}
	w2 := &Waker{Store: s, Start: starter2,
		Now:     func() time.Time { return *clock },
		Current: w1.Current}
	*clock = clock.Add(11 * time.Second)
	if err := drainPass(t, w2); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	calls := starter2.recorded()
	if len(calls) != 1 || !strings.Contains(calls[0].text, "Fund the build?") ||
		strings.Contains(calls[0].text, "Raise the month cap?") {
		t.Fatalf("the restart's wake = %+v, want only the blocked ask", calls)
	}
}

// A wake whose venue is gone is not a failure to retry: the watch row
// goes with the thread, the cursor moves past the move, and a later ask
// on the same project still wakes.
func TestVenueGoneDropsItsWatch(t *testing.T) {
	w, s, starter, _ := newWakeRig(t, nil)
	conv, err := s.NewConversation(t.Context(), "sf/wake", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddChatWatch(t.Context(), "sf/wake", "sebastian", conv, "t-1"); err != nil {
		t.Fatal(err)
	}
	starter.gone = map[int64]bool{conv: true}
	wakeMove(t, s, "sf/wake", "t-1", "board")
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass returned %v, want nil", err)
	}
	if ws, err := s.ChatWatchesFor(t.Context(), "sf/wake", "t-1"); err != nil {
		t.Fatal(err)
	} else if len(ws) != 0 {
		t.Fatalf("the watch outlived its venue: %+v", ws)
	}
	wakeAsk(t, s, "sf/wake", "t-1", "Where does the cache live?", "member")
	if err := drainPass(t, w); err != nil {
		t.Fatalf("second Pass returned %v, want nil", err)
	}
	calls := starter.recorded()
	if len(calls) != 2 || calls[1].birth != "wake_ask" {
		t.Fatalf("the wakes are %+v, want the refused card wake then wake_ask", calls)
	}
}

// A due timer on a gone venue drops its row instead of holding the
// cooldown: the sweep does not retry it.
func TestVenueGoneDropsItsTimer(t *testing.T) {
	w, s, starter, clock := newWakeRig(t, map[string]policy.ProjectConfig{
		"wake-proj": {Repository: "sf/wake", Autonomy: policy.AutonomyAssisted, Notify: []string{"sebastian"}},
	})
	conv, err := s.NewConversation(t.Context(), "sf/wake", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddChatTimer(t.Context(), "sf/wake", "sebastian", conv, clock.Add(-time.Minute), "Check the mirror.", ""); err != nil {
		t.Fatal(err)
	}
	starter.gone = map[int64]bool{conv: true}
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass returned %v, want nil", err)
	}
	*clock = clock.Add(2 * time.Minute)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("second Pass returned %v, want nil", err)
	}
	if calls := starter.recorded(); len(calls) != 1 {
		t.Fatalf("%d wakes, want 1 (the refused timer wake, never retried): %+v", len(calls), calls)
	}
}

// fund is named in the wake only where the woken login is offered it: an
// approver of an autonomous project.
func TestWakeAskTextNamesFundOnlyForAnApprover(t *testing.T) {
	for _, tc := range []struct {
		approvers []string
		autonomy  string
		want      bool
	}{
		{nil, policy.AutonomyAutonomous, false},
		{[]string{"sebastian"}, policy.AutonomyAutonomous, true},
		{[]string{"morgan"}, policy.AutonomyAutonomous, false},
	} {
		w, s, starter, _ := newWakeRig(t, map[string]policy.ProjectConfig{
			"wake-proj": {Repository: "sf/wake", Autonomy: tc.autonomy, Notify: []string{"sebastian"}, Approvers: tc.approvers}})
		wakeAsk(t, s, "sf/wake", "t-1", "Use a replica?", ViaFactoryd)
		if err := drainPass(t, w); err != nil {
			t.Fatal(err)
		}
		calls := starter.recorded()
		if len(calls) != 1 || strings.Contains(calls[0].text, "takes fund from your pool.") != tc.want {
			t.Errorf("approvers %v at %s: wake %+v, want fund named %v", tc.approvers, tc.autonomy, calls, tc.want)
		}
	}
}

// fakeRoadmapHead is the doc repository read the pull checks: the head
// of docs/roadmap.md, movable by a test, and how often it was read.
type fakeRoadmapHead struct {
	mu    sync.Mutex
	head  string
	reads int
}

func (f *fakeRoadmapHead) read(_ context.Context, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.head, nil
}

func (f *fakeRoadmapHead) set(head string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head = head
}

func (f *fakeRoadmapHead) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// refillRig is one Waker over an autonomous project with the pull
// threshold `below`, an empty queue and the roadmap head head-1: every
// condition of the pull holds, so a case breaks exactly one.
func refillRig(t *testing.T, below int) (*Waker, *PGStore, *fakeWakeStarter, *time.Time, *fakeRoadmapHead) {
	t.Helper()
	projects := map[string]policy.ProjectConfig{
		"wake-proj": {Repository: "sf/wake", Autonomy: policy.AutonomyAutonomous, Notify: []string{"sebastian"}, RefillBelow: below},
	}
	w, s, starter, clock := newWakeRig(t, projects)
	roadmap := &fakeRoadmapHead{head: "head-1"}
	w.RoadmapHead = roadmap.read
	w.Current = func() policy.Version {
		return policy.Version{Tree: policy.Tree{Projects: projects,
			Document: policy.Document{Chat: &policy.ChatConfig{DocRepo: "sf/docs"}}}}
	}
	return w, s, starter, clock, roadmap
}

// refillWaiting puts one waiting card on sf/wake.
func refillWaiting(t *testing.T, s *PGStore) {
	t.Helper()
	if err := s.insertTask(t.Context(), Task{ID: "t-wait", Repository: "sf/wake", Kind: "agent", Track: "direct",
		Source: "webhook", Status: "queued", Objective: "b"}, ""); err != nil {
		t.Fatal(err)
	}
}

// The roadmap pull: an open intake, a queue below the threshold and a
// roadmap head the project never pulled wake one turn on the member's
// inbox, birth queue_refill, and the wake stamps the head it read, so a
// turn that files nothing never refires on the same roadmap and a changed
// roadmap pulls once more.
func TestWakerWakesOnTheRoadmapPull(t *testing.T) {
	w, s, starter, clock, roadmap := refillRig(t, 2)
	refillWaiting(t, s)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	calls := starter.recorded()
	if len(calls) != 1 {
		t.Fatalf("%d wakes, want 1: %+v", len(calls), calls)
	}
	conv, err := s.EnsureInbox(t.Context(), "sf/wake", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	want := "The queue of wake-proj is below 2: read docs/roadmap.md of the project's doc repository, " +
		"file every item whose Priority is filled and whose number no card cites, then end."
	c := calls[0]
	if c.project != "wake-proj" || c.login != "sebastian" || c.conversation != conv || c.birth != "queue_refill" || c.text != want {
		t.Fatalf("the wake = %+v, want wake-proj sebastian on %d, birth queue_refill, text\n%s", c, conv, want)
	}
	if head, ok, err := s.RoadmapPullStamp(t.Context(), "sf/wake"); err != nil || !ok || head != "head-1" {
		t.Fatalf("the stamp = %q, %v, %v; want head-1: the wake stamps the head it read", head, ok, err)
	}
	// the fake starter runs no turn, so nothing was filed: the stamped head
	// holds the pull past every gap and cooldown
	*clock = clock.Add(2 * time.Minute)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("the second Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 1 {
		t.Fatalf("%d wakes after the first, want the stamp to hold the pull: %+v", len(calls), calls)
	}
	// a changed roadmap is a new pull, once the cooldown allows it
	roadmap.set("head-2")
	if err := drainPass(t, w); err != nil {
		t.Fatalf("the third Pass: %v", err)
	}
	calls = starter.recorded()
	if len(calls) != 2 || calls[1].birth != "queue_refill" {
		t.Fatalf("%d wakes after the head change, want a second queue_refill: %+v", len(calls), calls)
	}
	if head, ok, err := s.RoadmapPullStamp(t.Context(), "sf/wake"); err != nil || !ok || head != "head-2" {
		t.Fatalf("the stamp = %q, %v, %v; want head-2", head, ok, err)
	}
}

// The pull's refusals, one pin each: a closed intake stops the pull
// alone, the queue at the threshold holds it, the stamped head holds it,
// and a project with no threshold — or no document repository — never
// reads the roadmap at all.
func TestWakerRefillRefusals(t *testing.T) {
	cases := []struct {
		name      string
		prepare   func(t *testing.T) (*Waker, *PGStore, *fakeWakeStarter, *fakeRoadmapHead)
		wantStamp string // the head a refused pull leaves, "" for no stamp
	}{
		{"a closed intake stops the pull alone", func(t *testing.T) (*Waker, *PGStore, *fakeWakeStarter, *fakeRoadmapHead) {
			w, s, starter, _, roadmap := refillRig(t, 1)
			if err := s.SetChatIntake(t.Context(), "sf/wake", true, "sebastian", time.Now()); err != nil {
				t.Fatal(err)
			}
			return w, s, starter, roadmap
		}, ""},
		{"the queue at the threshold holds the pull", func(t *testing.T) (*Waker, *PGStore, *fakeWakeStarter, *fakeRoadmapHead) {
			w, s, starter, _, roadmap := refillRig(t, 1)
			refillWaiting(t, s)
			return w, s, starter, roadmap
		}, ""},
		{"the stamped head holds the pull", func(t *testing.T) (*Waker, *PGStore, *fakeWakeStarter, *fakeRoadmapHead) {
			w, s, starter, _, roadmap := refillRig(t, 1)
			if err := s.SetRoadmapPullStamp(t.Context(), "sf/wake", "head-1", time.Now()); err != nil {
				t.Fatal(err)
			}
			return w, s, starter, roadmap
		}, "head-1"},
		{"no threshold never reads the roadmap", func(t *testing.T) (*Waker, *PGStore, *fakeWakeStarter, *fakeRoadmapHead) {
			w, s, starter, _, roadmap := refillRig(t, 0)
			return w, s, starter, roadmap
		}, ""},
		{"no document repository never reads the roadmap", func(t *testing.T) (*Waker, *PGStore, *fakeWakeStarter, *fakeRoadmapHead) {
			w, s, starter, _, roadmap := refillRig(t, 1)
			w.Current = func() policy.Version {
				return policy.Version{Tree: policy.Tree{Projects: map[string]policy.ProjectConfig{
					"wake-proj": {Repository: "sf/wake", Autonomy: policy.AutonomyAutonomous, Notify: []string{"sebastian"}, RefillBelow: 1}}}}
			}
			return w, s, starter, roadmap
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, s, starter, roadmap := tc.prepare(t)
			if err := drainPass(t, w); err != nil {
				t.Fatalf("Pass: %v", err)
			}
			if calls := starter.recorded(); len(calls) != 0 {
				t.Fatalf("%d wakes, want none: %+v", len(calls), calls)
			}
			if head, ok, err := s.RoadmapPullStamp(t.Context(), "sf/wake"); err != nil || ok != (tc.wantStamp != "") || head != tc.wantStamp {
				t.Fatalf("the stamp = %q, %v, %v; want %q: a refused pull moves no stamp", head, ok, err, tc.wantStamp)
			}
			if strings.Contains(tc.name, "never reads") && roadmap.readCount() != 0 {
				t.Fatalf("the roadmap was read %d times, want none", roadmap.readCount())
			}
		})
	}
}

// A pull wake that fails to start moves no stamp: a lost turn loses
// nothing, and the next pass takes the pull again.
func TestWakerRefillAFailedStartMovesNoStamp(t *testing.T) {
	w, s, starter, clock, _ := refillRig(t, 1)
	starter.err = errors.New("the chat is unfunded")
	if err := drainPass(t, w); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 1 {
		t.Fatalf("%d wake starts, want the failed one: %+v", len(calls), calls)
	}
	if _, ok, err := s.RoadmapPullStamp(t.Context(), "sf/wake"); err != nil || ok {
		t.Fatalf("the stamp = %v, %v; a failed start moves no stamp", ok, err)
	}
	starter.err = nil
	*clock = clock.Add(61 * time.Second)
	if err := drainPass(t, w); err != nil {
		t.Fatalf("the retry Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 2 {
		t.Fatalf("%d wake starts, want the retry: %+v", len(calls), calls)
	}
	if head, ok, err := s.RoadmapPullStamp(t.Context(), "sf/wake"); err != nil || !ok || head != "head-1" {
		t.Fatalf("the stamp = %q, %v, %v; want head-1 after the retry", head, ok, err)
	}
}
