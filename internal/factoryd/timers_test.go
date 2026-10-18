package factoryd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The timers and watches (the concept's timers section): a member's
// thread stores its own subscriptions, the Waker fires them, and they die
// with their card. These tests run on the store beside the wake tests'
// rig.

// wakeMove records a card move of task on repository, its status change
// when status is true, via chat when the agent itself did it, and
// returns the event's id.
func wakeMove(t *testing.T, s *PGStore, repository, task, via string, status bool) int64 {
	t.Helper()
	kind := EventCardMoved
	if status {
		kind = EventTaskStatus
	}
	id, err := s.Record(t.Context(), Event{Repository: repository, TaskID: task, Kind: kind, Via: via})
	if err != nil {
		t.Fatalf("record the move: %v", err)
	}
	return id
}

// A watched card's move wakes one turn on the watching thread at
// assisted: the wake carries the card, the same card's second move
// inside a minute wakes nobody, a move after a minute wakes again, and a
// manual project's watched card wakes nobody while its cursor still
// moves.
func TestWatchedCardWakesAtAssistedOnItsThread(t *testing.T) {
	w, s, starter, clock := newWakeRig(t, map[string]policy.ProjectConfig{
		"wake-proj": {Repository: "sf/wake", Autonomy: policy.AutonomyAssisted, Notify: []string{"sebastian"}},
		"manual":    {Repository: "sf/manual", Autonomy: policy.AutonomyManual, Notify: []string{"sebastian"}},
	})
	conv, err := s.NewConversation(t.Context(), "sf/wake", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	manualConv, err := s.NewConversation(t.Context(), "sf/manual", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddChatWatch(t.Context(), "sf/wake", "sebastian", conv, "t-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddChatWatch(t.Context(), "sf/manual", "sebastian", manualConv, "t-9"); err != nil {
		t.Fatal(err)
	}
	idManual := wakeMove(t, s, "sf/manual", "t-9", ViaFactoryd, false)
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 0 {
		t.Fatalf("%d wakes on the manual project, want 0: %+v", len(calls), calls)
	}
	if got := wakeCursorOf(t, s, "sf/manual"); got < idManual {
		t.Fatalf("the manual cursor %d stayed behind its event %d", got, idManual)
	}
	id1 := wakeMove(t, s, "sf/wake", "t-1", ViaFactoryd, false)
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	calls := starter.recorded()
	if len(calls) != 1 {
		t.Fatalf("%d wakes, want 1: %+v", len(calls), calls)
	}
	c := calls[0]
	if c.project != "wake-proj" || c.login != "sebastian" || c.conversation != conv || c.birth != "wake_card" ||
		!strings.Contains(c.text, "t-1") {
		t.Fatalf("the wake = %+v, want wake-proj sebastian on %d about t-1", c, conv)
	}
	if got := wakeCursorOf(t, s, "sf/wake"); got < id1 {
		t.Fatalf("the cursor %d stayed behind the event %d", got, id1)
	}
	id2 := wakeMove(t, s, "sf/wake", "t-1", ViaFactoryd, false)
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls = starter.recorded(); len(calls) != 1 {
		t.Fatalf("%d wakes after a second move inside a minute, want 1: %+v", len(calls), calls)
	}
	if got := wakeCursorOf(t, s, "sf/wake"); got < id2 {
		t.Fatalf("the cursor %d stayed behind the second event %d", got, id2)
	}
	*clock = clock.Add(61 * time.Second)
	wakeMove(t, s, "sf/wake", "t-1", ViaFactoryd, true)
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls = starter.recorded(); len(calls) != 2 {
		t.Fatalf("%d wakes after a minute, want 2: %+v", len(calls), calls)
	}
	if calls[1].birth != "wake_card" || calls[1].conversation != conv || !strings.Contains(calls[1].text, "t-1") {
		t.Fatalf("the second wake = %+v, want wake_card on %d about t-1", calls[1], conv)
	}
}

// A due timer wakes one turn with its note on its own thread and the row
// is gone: the same pass wakes nobody again, a timer not yet due wakes
// nobody, and a row added after one fired wakes when it comes due, which
// is the rearm a woken turn does with set_timer.
func TestTimerFiresOnceOnItsThread(t *testing.T) {
	w, s, starter, clock := newWakeRig(t, nil)
	conv, err := s.NewConversation(t.Context(), "sf/wake", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddChatTimer(t.Context(), "sf/wake", "sebastian", conv, clock.Add(-time.Minute), "Check the build.", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AddChatTimer(t.Context(), "sf/wake", "sebastian", conv, clock.Add(30*time.Minute), "Too early.", ""); err != nil {
		t.Fatal(err)
	}
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	calls := starter.recorded()
	if len(calls) != 1 {
		t.Fatalf("%d wakes, want 1: %+v", len(calls), calls)
	}
	c := calls[0]
	if c.project != "wake-proj" || c.login != "sebastian" || c.conversation != conv || c.birth != "wake_timer" ||
		!strings.Contains(c.text, "Check the build.") {
		t.Fatalf("the wake = %+v, want wake-proj sebastian on %d carrying the note", c, conv)
	}
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls = starter.recorded(); len(calls) != 1 {
		t.Fatalf("%d wakes after the row is gone, want 1: %+v", len(calls), calls)
	}
	*clock = clock.Add(30 * time.Minute)
	wakeMove(t, s, "sf/wake", "t-1", ViaFactoryd, false) // a move is not the timer's thread
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	calls = starter.recorded()
	if len(calls) != 2 {
		t.Fatalf("%d wakes at the second timer, want 2: %+v", len(calls), calls)
	}
	if calls[1].conversation != conv || calls[1].birth != "wake_timer" || !strings.Contains(calls[1].text, "Too early.") {
		t.Fatalf("the second wake = %+v, want wake_timer on %d carrying the note", calls[1], conv)
	}
}

// A card's end deletes its watches and the timers bound to it, however
// the end was filed, and leaves the unbound timers alone; the card's
// later moves wake nobody.
func TestWatchesDieWithTheCard(t *testing.T) {
	w, s, starter, clock := newWakeRig(t, nil)
	conv, err := s.NewConversation(t.Context(), "sf/wake", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddChatWatch(t.Context(), "sf/wake", "sebastian", conv, "t-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddChatTimer(t.Context(), "sf/wake", "sebastian", conv, clock.Add(30*time.Minute), "bound", "t-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddChatTimer(t.Context(), "sf/wake", "sebastian", conv, clock.Add(time.Hour), "free", ""); err != nil {
		t.Fatal(err)
	}
	idEnd, err := s.Record(t.Context(), Event{Repository: "sf/wake", TaskID: "t-1", Kind: EventTaskEnded, Via: ViaChat})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 0 {
		t.Fatalf("%d wakes, want 0: %+v", len(calls), calls)
	}
	if got := wakeCursorOf(t, s, "sf/wake"); got < idEnd {
		t.Fatalf("the cursor %d stayed behind the end %d", got, idEnd)
	}
	if ws, err := s.ChatWatchesFor(t.Context(), "sf/wake", "t-1"); err != nil || len(ws) != 0 {
		t.Fatalf("the watch lived on: %v, %d rows", err, len(ws))
	}
	due, err := s.DueChatTimers(t.Context(), "sf/wake", clock.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Note != "free" {
		t.Fatalf("the timers after the end = %+v, want only the unbound one", due)
	}
	wakeMove(t, s, "sf/wake", "t-1", ViaFactoryd, false)
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls := starter.recorded(); len(calls) != 0 {
		t.Fatalf("%d wakes on a dead watch, want 0: %+v", len(calls), calls)
	}
}

// The heartbeat wakes the inbox hourly while work sits open and daily
// otherwise, on projects at assisted and above only, and a pulse that
// says a card waits on a human holds it: the needs-you card already has
// the member's eye.
func TestHeartbeatOpenHourlyIdleDaily(t *testing.T) {
	w, s, starter, clock := newWakeRig(t, map[string]policy.ProjectConfig{
		"wake-proj": {Repository: "sf/wake", Autonomy: policy.AutonomyAssisted, Notify: []string{"sebastian"}},
		"manual":    {Repository: "sf/manual", Autonomy: policy.AutonomyManual, Notify: []string{"sebastian"}},
	})
	open, waiting := true, false
	w.Pulse = func(context.Context, string) (bool, bool, error) { return open, waiting, nil }
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	calls := starter.recorded()
	conv, err := s.EnsureInbox(t.Context(), "sf/wake", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("%d wakes, want 1: %+v", len(calls), calls)
	}
	if c := calls[0]; c.project != "wake-proj" || c.login != "sebastian" || c.conversation != conv ||
		c.birth != "wake_heartbeat" || !strings.Contains(c.text, "hourly") {
		t.Fatalf("the wake = %+v, want wake_heartbeat on the inbox about the hour", c)
	}
	*clock = clock.Add(30 * time.Minute)
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls = starter.recorded(); len(calls) != 1 {
		t.Fatalf("%d wakes at half an hour, want 1: %+v", len(calls), calls)
	}
	*clock = clock.Add(31 * time.Minute)
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls = starter.recorded(); len(calls) != 2 {
		t.Fatalf("%d wakes at the hour, want 2: %+v", len(calls), calls)
	}
	open = false
	*clock = clock.Add(23 * time.Hour) // the next day
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls = starter.recorded(); len(calls) != 3 {
		t.Fatalf("%d wakes on the idle day, want 3: %+v", len(calls), calls)
	}
	if calls[2].birth != "wake_heartbeat" || !strings.Contains(calls[2].text, "idle") {
		t.Fatalf("the idle wake = %+v, want wake_heartbeat about the idle board", calls[2])
	}
	*clock = clock.Add(2 * time.Hour)
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls = starter.recorded(); len(calls) != 3 {
		t.Fatalf("%d wakes two hours in, want 3: %+v", len(calls), calls)
	}
	*clock = clock.Add(25 * time.Hour)
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls = starter.recorded(); len(calls) != 4 {
		t.Fatalf("%d wakes at the day, want 4: %+v", len(calls), calls)
	}
	waiting = true
	*clock = clock.Add(25 * time.Hour)
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if calls = starter.recorded(); len(calls) != 4 {
		t.Fatalf("%d wakes while a card waits on a human, want 4: %+v", len(calls), calls)
	}
	for _, c := range starter.recorded() {
		if c.project == "manual" {
			t.Fatalf("a heartbeat woke the manual project: %+v", c)
		}
	}
}

// Sixteen watches and timers are a project's cap: the seventeenth add is
// refused naming the cap, an upsert of a watched card stays inside it,
// and a removed watch frees its slot.
func TestTimerCapRefuses(t *testing.T) {
	_, s, _, clock := newWakeRig(t, nil)
	for i := range 8 {
		if err := s.AddChatWatch(t.Context(), "sf/wake", "sebastian", 1, fmt.Sprintf("t-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 8 {
		if err := s.AddChatTimer(t.Context(), "sf/wake", "sebastian", 1, clock.Add(time.Duration(i)*time.Minute), "n", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddChatWatch(t.Context(), "sf/wake", "sebastian", 1, "t-0"); err != nil {
		t.Fatalf("the upsert of a watched card is inside the cap: %v", err)
	}
	err := s.AddChatTimer(t.Context(), "sf/wake", "sebastian", 1, clock.Add(time.Minute), "one too many", "")
	if !errors.Is(err, ErrChatWakeCap) {
		t.Fatalf("the seventeenth add = %v, want the cap", err)
	}
	if !strings.Contains(err.Error(), "16 of 16") {
		t.Fatalf("the cap's refusal does not name the count: %v", err)
	}
	if ok, err := s.RemoveChatWatch(t.Context(), "sf/wake", "sebastian", "t-0"); err != nil || !ok {
		t.Fatalf("the removal = %v, %v, want gone", ok, err)
	}
	if err := s.AddChatTimer(t.Context(), "sf/wake", "sebastian", 1, clock.Add(2*time.Minute), "fits again", ""); err != nil {
		t.Fatalf("the freed slot refused a timer: %v", err)
	}
}

// One project's failing pass holds only that project back: the sweep
// logs its error and the other projects' timers still fire.
func TestWakerPassIsolatesProjects(t *testing.T) {
	w, s, starter, _ := newWakeRig(t, map[string]policy.ProjectConfig{
		"one": {Repository: "sf/one", Autonomy: policy.AutonomyAssisted, Notify: []string{"sebastian"}},
		"two": {Repository: "sf/two", Autonomy: policy.AutonomyAssisted, Notify: []string{"sebastian"}},
	})
	w.Pulse = func(context.Context, string) (bool, bool, error) {
		return false, false, fmt.Errorf("the pulse is down")
	}
	for _, repository := range []string{"sf/one", "sf/two"} {
		if _, err := s.NewConversation(t.Context(), repository, "sebastian"); err != nil {
			t.Fatal(err)
		}
		if err := s.AddChatTimer(t.Context(), repository, "sebastian", 1, time.Now().Add(-time.Minute), "Check.", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Pass(t.Context()); err != nil {
		t.Fatalf("Pass returned %v, want nil: one project's error is not the sweep's", err)
	}
	if calls := starter.recorded(); len(calls) != 2 {
		t.Fatalf("%d timer wakes, want 2: %+v", len(calls), calls)
	}
}

// One watch per card: a second member's watch of a taken card refuses
// and names the watcher, while the watcher's own row still moves to a
// new thread of theirs.
func TestWatchOnePerCard(t *testing.T) {
	_, s, _, _ := newWakeRig(t, nil)
	seb, err := s.NewConversation(t.Context(), "sf/wake", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.NewConversation(t.Context(), "sf/wake", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddChatWatch(t.Context(), "sf/wake", "sebastian", seb, "t-1"); err != nil {
		t.Fatal(err)
	}
	err = s.AddChatWatch(t.Context(), "sf/wake", "bob", bob, "t-1")
	var taken *WatchTakenError
	if !errors.As(err, &taken) || taken.Login != "sebastian" {
		t.Fatalf("bob's watch of a taken card returned %v, want WatchTakenError naming sebastian", err)
	}
	seb2, err := s.NewConversation(t.Context(), "sf/wake", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddChatWatch(t.Context(), "sf/wake", "sebastian", seb2, "t-1"); err != nil {
		t.Fatalf("the watcher's own re-watch: %v", err)
	}
	if w, err := s.ChatWatchesFor(t.Context(), "sf/wake", "t-1"); err != nil || len(w) != 1 || w[0].Conversation != seb2 {
		t.Fatalf("the watch after the re-watch: %v %+v", err, w)
	}
}
