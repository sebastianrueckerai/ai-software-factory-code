package factoryd

import (
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

func TestSupervisorConfigMustBeSane(t *testing.T) {
	base := SupervisorConfig{NoProgressAfter: 5 * time.Minute, WallClockCap: time.Hour, RepetitionEvents: 10}
	if _, err := NewSupervisor(base, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for name, mutate := range map[string]func(*SupervisorConfig){
		"no-progress zero":  func(c *SupervisorConfig) { c.NoProgressAfter = 0 },
		"no-progress spent": func(c *SupervisorConfig) { c.NoProgressAfter = -time.Minute },
		"wall clock zero":   func(c *SupervisorConfig) { c.WallClockCap = 0 },
		"wall clock spent":  func(c *SupervisorConfig) { c.WallClockCap = -time.Hour },
		"repetition 1":      func(c *SupervisorConfig) { c.RepetitionEvents = 1 },
		"repetition 0":      func(c *SupervisorConfig) { c.RepetitionEvents = 0 },
	} {
		cfg := base
		mutate(&cfg)
		if _, err := NewSupervisor(cfg, time.Unix(0, 0), time.Unix(0, 0)); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestSupervisorNoProgressBoundary(t *testing.T) {
	s := mustSupervisor(t, 5*time.Minute, time.Hour, 10)
	t0 := time.Unix(0, 0)
	if trip := s.Event(harness.Event{Kind: harness.EventCheckpoint}, t0); trip != nil {
		t.Fatalf("first event tripped: %+v", trip)
	}
	if trip := s.Tick(t0.Add(5*time.Minute - 1)); trip != nil {
		t.Fatalf("tripped one nanosecond early: %+v", trip)
	}
	trip := s.Tick(t0.Add(5 * time.Minute))
	if trip == nil || trip.Kind != TripNoProgress || trip.RetryStrongerModel {
		t.Fatalf("trip = %+v, want no_progress resume", trip)
	}
	// Fresh progress pushes the boundary out.
	s2 := mustSupervisor(t, 5*time.Minute, time.Hour, 10)
	s2.Event(harness.Event{Kind: harness.EventCommand}, t0.Add(4*time.Minute))
	if trip := s2.Tick(t0.Add(5 * time.Minute)); trip != nil {
		t.Fatalf("tripped despite recent event: %+v", trip)
	}
	if trip := s2.Tick(t0.Add(9*time.Minute - 1)); trip != nil {
		t.Fatalf("tripped before the new boundary: %+v", trip)
	}
	if trip := s2.Tick(t0.Add(9 * time.Minute)); trip == nil {
		t.Fatal("should trip at the new boundary")
	}
}

func TestSupervisorWallClockCapWinsOverProgress(t *testing.T) {
	s := mustSupervisor(t, 5*time.Minute, 30*time.Minute, 1000)
	t0 := time.Unix(0, 0)
	// Events keep flowing: no-progress never fires, but the cap does.
	for m := 0; m <= 30; m++ {
		if trip := s.Event(harness.Event{Kind: harness.EventModelCall}, t0.Add(time.Duration(m)*time.Minute)); trip != nil {
			if m != 30 {
				t.Fatalf("tripped at minute %d: %+v", m, trip)
			}
			if trip.Kind != TripWallClock || trip.RetryStrongerModel {
				t.Fatalf("trip = %+v, want wall_clock resume", trip)
			}
			return
		}
	}
	t.Fatal("wall clock cap never tripped")
}

func TestSupervisorRepetitionDetector(t *testing.T) {
	s := mustSupervisor(t, time.Hour, time.Hour, 3)
	t0 := time.Unix(0, 0)
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Second) }
	// Model calls and checkpoints never count: a long reasoning streak is not a loop.
	for i := 0; i < 10; i++ {
		if trip := s.Event(harness.Event{Kind: harness.EventModelCall}, at(i)); trip != nil {
			t.Fatalf("model call %d tripped: %+v", i, trip)
		}
		if trip := s.Event(harness.Event{Kind: harness.EventCheckpoint}, at(i)); trip != nil {
			t.Fatalf("checkpoint %d tripped: %+v", i, trip)
		}
	}
	// count: command 1, file change resets to 0, command 1, tool call 2,
	// model call stays 2, command 3 trips.
	steps := []struct {
		ev   harness.Event
		trip bool
	}{
		{harness.Event{Kind: harness.EventCommand, Detail: map[string]any{"command": "go test", "exit": 1}}, false},
		{harness.Event{Kind: harness.EventFileChange, Detail: map[string]any{"path": "main.go"}}, false},
		{harness.Event{Kind: harness.EventCommand, Detail: map[string]any{"command": "go build", "exit": 0}}, false},
		{harness.Event{Kind: harness.EventToolCall, Detail: map[string]any{"tool": "grep", "status": "completed"}}, false},
		{harness.Event{Kind: harness.EventModelCall}, false},
		{harness.Event{Kind: harness.EventCommand, Detail: map[string]any{"command": "go vet", "exit": 0}}, true},
	}
	var trip *Trip
	for i, step := range steps {
		trip = s.Event(step.ev, at(20+i))
		if (trip != nil) != step.trip {
			t.Fatalf("step %d (%s): trip = %+v, want trip=%v", i, step.ev.Kind, trip, step.trip)
		}
	}
	if trip.Kind != TripRepetition || !trip.RetryStrongerModel {
		t.Fatalf("trip = %+v, want repetition retry-stronger-model", trip)
	}
	// The snapshot findings carry what the session actually did.
	joined := strings.Join(trip.Findings, "\n")
	for _, want := range []string{"go test", "exit 1", "main.go", "go build"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("findings missing %q: %v", want, trip.Findings)
		}
	}
}

func TestSupervisorWallClockSpansSessions(t *testing.T) {
	cfg := SupervisorConfig{NoProgressAfter: time.Hour, WallClockCap: 30 * time.Minute, RepetitionEvents: 100}
	taskStart := time.Unix(0, 0)
	// A resumed session starting 25 minutes into the task has 5 minutes left.
	s, err := NewSupervisor(cfg, taskStart, taskStart.Add(25*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if trip := s.Tick(taskStart.Add(29*time.Minute + 59*time.Second)); trip != nil {
		t.Fatalf("tripped before the task cap: %+v", trip)
	}
	if trip := s.Tick(taskStart.Add(30 * time.Minute)); trip == nil || trip.Kind != TripWallClock {
		t.Fatalf("trip = %+v, want wall_clock at the task cap", trip)
	}
}

func TestSupervisorTripIsSticky(t *testing.T) {
	s := mustSupervisor(t, time.Minute, time.Hour, 5)
	t0 := time.Unix(0, 0)
	first := s.Tick(t0.Add(time.Minute))
	if first == nil {
		t.Fatal("expected a trip")
	}
	again := s.Tick(t0.Add(2 * time.Minute))
	if again != first {
		t.Fatalf("trip not sticky: %+v then %+v", first, again)
	}
	if more := s.Event(harness.Event{Kind: harness.EventModelCall}, t0.Add(3*time.Minute)); more != first {
		t.Fatalf("post-trip event changed the verdict: %+v", more)
	}
}

func mustSupervisor(t *testing.T, noProgress, wallClock time.Duration, repetition int) *Supervisor {
	t.Helper()
	s, err := NewSupervisor(SupervisorConfig{
		NoProgressAfter:  noProgress,
		WallClockCap:     wallClock,
		RepetitionEvents: repetition,
	}, time.Unix(0, 0), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
