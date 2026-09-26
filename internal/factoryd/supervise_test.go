package factoryd

import (
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

func TestSupervisorConfigMustBeSane(t *testing.T) {
	base := SupervisorConfig{NoProgressAfter: 5 * time.Minute, WallClockCap: time.Hour, RepetitionEvents: 10}
	if _, err := NewSupervisor(base, time.Unix(0, 0)); err != nil {
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
		if _, err := NewSupervisor(cfg, time.Unix(0, 0)); err == nil {
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
	// Three events without a file change trips on the third; a file change
	// in between resets the count.
	if trip := s.Event(harness.Event{Kind: harness.EventModelCall}, t0); trip != nil {
		t.Fatalf("trip after 1: %+v", trip)
	}
	if trip := s.Event(harness.Event{Kind: harness.EventCommand, Detail: map[string]any{"command": "go test", "exit": 1}}, t0.Add(time.Second)); trip != nil {
		t.Fatalf("trip after 2: %+v", trip)
	}
	if trip := s.Event(harness.Event{Kind: harness.EventFileChange, Detail: map[string]any{"path": "main.go"}}, t0.Add(2*time.Second)); trip != nil {
		t.Fatalf("file change must reset the count: %+v", trip)
	}
	if trip := s.Event(harness.Event{Kind: harness.EventModelCall}, t0.Add(3*time.Second)); trip != nil {
		t.Fatalf("trip after reset+1: %+v", trip)
	}
	trip := s.Event(harness.Event{Kind: harness.EventCommand, Detail: map[string]any{"command": "go build", "exit": 0}}, t0.Add(4*time.Second))
	if trip != nil {
		t.Fatalf("trip after reset+2: %+v", trip)
	}
	trip = s.Event(harness.Event{Kind: harness.EventModelCall}, t0.Add(5*time.Second))
	if trip == nil || trip.Kind != TripRepetition || !trip.RetryStrongerModel {
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
	}, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
