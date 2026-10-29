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
}

type wakeCall struct {
	project, login string
	conversation   int64
	birth, text    string
}

func (f *fakeWakeStarter) StartWake(ctx context.Context, project, login string, conversation int64, birth, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, wakeCall{project, login, conversation, birth, text})
	if f.gone[conversation] {
		return fmt.Errorf("conversation %d is archived: %w", conversation, ErrWakeVenueGone)
	}
	return f.err
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
		"chat_wake_cursor", "chat_wake_state"} {
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
	if err := w.Pass(t.Context()); err != nil {
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
	if err := w.Pass(t.Context()); err != nil {
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
	if err := w.Pass(t.Context()); err != nil {
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
	if err := w.Pass(t.Context()); err != nil {
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

// The same ask text of the same card again is the same ask: no second
// wake.
func TestWakerNoSecondAttemptOnRepeatedAsk(t *testing.T) {
	w, s, starter, _ := newWakeRig(t, nil)
	wakeAsk(t, s, "sf/wake", "t-1", "Raise the month cap?", ViaFactoryd)
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	wakeAsk(t, s, "sf/wake", "t-1", "Raise the month cap?", ViaFactoryd)
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 1 {
		t.Fatalf("%d wakes, want 1: %+v", len(calls), calls)
	}
}

// A second wake inside ten seconds of the last one does not start and the
// cursor waits; eleven seconds later it starts.
func TestWakerTurnRateCaps(t *testing.T) {
	w, s, starter, clock := newWakeRig(t, nil)
	wakeAsk(t, s, "sf/wake", "t-1", "Raise the month cap?", ViaFactoryd)
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	capped := wakeCursorOf(t, s, "sf/wake")
	id := wakeAsk(t, s, "sf/wake", "t-2", "Fund the build?", ViaFactoryd)
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 1 {
		t.Fatalf("%d wakes inside the turn mark, want 1: %+v", len(calls), calls)
	}
	if got := wakeCursorOf(t, s, "sf/wake"); got != capped {
		t.Fatalf("the cursor moved to %d while capped at %d", got, capped)
	}
	*clock = clock.Add(11 * time.Second)
	if err := w.Pass(t.Context()); err != nil {
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
				if err := w.Pass(t.Context()); err != nil {
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
			if err := w.Pass(t.Context()); err != nil {
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
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 0 || wakeCursorOf(t, s, "sf/wake") < id {
		t.Fatalf("wakes %+v, cursor %d; want none and the cursor past %d", calls, wakeCursorOf(t, s, "sf/wake"), id)
	}
	ask("t-roll", "3 rolls failed since the last one merged. Retry the roll?", SourceRollFailed)
	if err := w.Pass(t.Context()); err != nil {
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
	if err := w.Pass(t.Context()); err != nil {
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
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 1 {
		t.Fatalf("%d starts inside the cooldown, want 1", len(calls))
	}
	*clock = clock.Add(61 * time.Second)
	starter.err = nil
	if err := w.Pass(t.Context()); err != nil {
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
	if err := w.Pass(t.Context()); err != nil {
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

// The marks and the cursor are rows, so a restart changes nothing: what
// one Waker handled stays handled, and what it left blocked still starts.
func TestWakerMarksSurviveRestart(t *testing.T) {
	w1, s, starter1, clock := newWakeRig(t, nil)
	wakeAsk(t, s, "sf/wake", "t-1", "Raise the month cap?", ViaFactoryd)
	if err := w1.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	wakeAsk(t, s, "sf/wake", "t-2", "Fund the build?", ViaFactoryd)
	if err := w1.Pass(t.Context()); err != nil {
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
	if err := w2.Pass(t.Context()); err != nil {
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
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass returned %v, want nil", err)
	}
	if ws, err := s.ChatWatchesFor(t.Context(), "sf/wake", "t-1"); err != nil {
		t.Fatal(err)
	} else if len(ws) != 0 {
		t.Fatalf("the watch outlived its venue: %+v", ws)
	}
	wakeAsk(t, s, "sf/wake", "t-1", "Where does the cache live?", "member")
	if err := w.Pass(t.Context()); err != nil {
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
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass returned %v, want nil", err)
	}
	*clock = clock.Add(2 * time.Minute)
	if err := w.Pass(t.Context()); err != nil {
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
		if err := w.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		calls := starter.recorded()
		if len(calls) != 1 || strings.Contains(calls[0].text, "takes fund from your pool.") != tc.want {
			t.Errorf("approvers %v at %s: wake %+v, want fund named %v", tc.approvers, tc.autonomy, calls, tc.want)
		}
	}
}
