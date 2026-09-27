package factoryd

import (
	"fmt"
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

func TestNormaliseOutputStripsVolatileParts(t *testing.T) {
	// two runs of the same failing go test, minutes apart, in different temp dirs
	run1 := `=== RUN   TestParse
    parse_test.go:42: got "a", want "b"
--- FAIL: TestParse (0.01s)
panic: runtime error: invalid memory address or nil pointer dereference [recovered]
[signal SIGSEGV: segmentation violation code=0x1 addr=0x18 pc=0x4a2f1c]
goroutine 7 [running]:
main.parse(0xc000012345)
	/tmp/go-build3312/b001/parse.go:17 +0x1d
FAIL	example.com/p	0.214s
2026-09-27T10:15:02.123Z ERROR build failed after 1m2.5s`
	run2 := `=== RUN   TestParse
    parse_test.go:44: got "a", want "b"
--- FAIL: TestParse (0.03s)
panic: runtime error: invalid memory address or nil pointer dereference [recovered]
[signal SIGSEGV: segmentation violation code=0x1 addr=0x18 pc=0x4a3b00]
goroutine 7 [running]:
main.parse(0xc0000a0000)
	/tmp/go-build9123/b001/parse.go:19 +0x2f
FAIL	example.com/p	1.02s
2026-09-27 10:19:44,881 ERROR build failed after 58ms`
	if a, b := normaliseOutput(run1), normaliseOutput(run2); a != b {
		t.Fatalf("same failure normalised differently:\n%s\n---\n%s", a, b)
	}
	// the message itself survives
	n := normaliseOutput(run1)
	for _, want := range []string{`got "a", want "b"`, "--- FAIL: TestParse", "nil pointer dereference", "parse_test.go", "ERROR build failed"} {
		if !strings.Contains(n, want) {
			t.Fatalf("normalised output lost %q:\n%s", want, n)
		}
	}
	for _, gone := range []string{"0xc000012345", "go-build3312", "0.214s", "10:15:02", ":42"} {
		if strings.Contains(n, gone) {
			t.Fatalf("normalised output kept %q:\n%s", gone, n)
		}
	}
	// a different failure stays different
	other := strings.Replace(run1, `want "b"`, `want "c"`, 1)
	if normaliseOutput(other) == n {
		t.Fatal("different failures normalised to the same text")
	}
	// shell and python shapes
	sh1 := "loopcheck: FAIL at Sat Sep 27 10:15:02 UTC 2026 in /tmp/tmp.Xa81Kd (pid 311)\nline 7: expected token"
	sh2 := "loopcheck: FAIL at Sat Sep 27 10:21:40 UTC 2026 in /tmp/tmp.0PqL2z (pid 498)\nline 9: expected token"
	if normaliseOutput(sh1) != normaliseOutput(sh2) {
		t.Fatalf("shell failure normalised differently:\n%s\n---\n%s", normaliseOutput(sh1), normaliseOutput(sh2))
	}
	py1 := "Traceback (most recent call last):\n  File \"/var/folders/x1/abc/T/pytest-7/test_a.py\", line 12, in test_a\nAssertionError: 1 != 2\n1 failed in 0.12 seconds"
	py2 := "Traceback (most recent call last):\n  File \"/var/folders/x1/abc/T/pytest-9/test_a.py\", line 15, in test_a\nAssertionError: 1 != 2\n1 failed in 3.40 seconds"
	if normaliseOutput(py1) != normaliseOutput(py2) {
		t.Fatalf("python failure normalised differently:\n%s\n---\n%s", normaliseOutput(py1), normaliseOutput(py2))
	}
}

func cmdEvent(command string, exit int, output string) harness.Event {
	return harness.Event{Kind: harness.EventCommand, Detail: map[string]any{"command": command, "exit": float64(exit), "output": output}}
}

func toolEvent(tool string, args map[string]any) harness.Event {
	return harness.Event{Kind: harness.EventToolCall, Detail: map[string]any{"tool": tool, "status": "completed", "args": args}}
}

func fileEvent(path, sha string) harness.Event {
	return harness.Event{Kind: harness.EventFileChange, Detail: map[string]any{"path": path, "sha256": sha}}
}

func diffEvent(ins, del int) harness.Event {
	return harness.Event{Kind: harness.EventCheckpoint, Detail: map[string]any{"diff": map[string]any{"files": float64(1), "insertions": float64(ins), "deletions": float64(del)}}}
}

func newTraces() *traceDetectors {
	return newTraceDetectors(SupervisorConfig{}.withDefaults())
}

func TestSameFailureTripsOnTheNthIdenticalFailureInARow(t *testing.T) {
	d := newTraces()
	fail := func(at string) harness.Event {
		return cmdEvent("sh scripts/loopcheck.sh", 1, "loopcheck: FAIL at "+at+" (took 12ms)\nexpected the impossible")
	}
	if trip := d.observe(fail("10:00:01")); trip != nil {
		t.Fatalf("first failure tripped: %+v", trip)
	}
	d.observe(fileEvent("a.go", "h1")) // edits in between do not break the run
	if trip := d.observe(fail("10:00:09")); trip != nil {
		t.Fatalf("second failure tripped: %+v", trip)
	}
	trip := d.observe(fail("10:01:30"))
	if trip == nil || trip.Kind != TripSameFailure {
		t.Fatalf("third identical failure: trip = %+v, want same_failure", trip)
	}
	if !strings.Contains(trip.Detail, "sh scripts/loopcheck.sh") || !strings.Contains(trip.Detail, "3") {
		t.Fatalf("detail = %q, want the command and the count", trip.Detail)
	}
}

func TestSameFailureNonTrips(t *testing.T) {
	d := newTraces()
	// a different signature restarts the run
	d.observe(cmdEvent("go test ./...", 1, "want 1"))
	d.observe(cmdEvent("go test ./...", 1, "want 1"))
	d.observe(cmdEvent("go test ./...", 1, "want 2"))
	if trip := d.observe(cmdEvent("go test ./...", 1, "want 2")); trip != nil {
		t.Fatalf("different signatures tripped: %+v", trip)
	}
	// interleaved success of the same command restarts the run
	d = newTraces()
	d.observe(cmdEvent("go test ./...", 1, "want 1"))
	d.observe(cmdEvent("go test ./...", 1, "want 1"))
	d.observe(cmdEvent("go test ./...", 0, "ok"))
	if trip := d.observe(cmdEvent("go test ./...", 1, "want 1")); trip != nil {
		t.Fatalf("failure after an interleaved success tripped: %+v", trip)
	}
	// the same output from different commands is different
	d = newTraces()
	d.observe(cmdEvent("go test ./a", 1, "boom"))
	d.observe(cmdEvent("go test ./b", 1, "boom"))
	if trip := d.observe(cmdEvent("go test ./c", 1, "boom")); trip != nil {
		t.Fatalf("different commands tripped: %+v", trip)
	}
	// a command without an exit code is no evidence either way
	d = newTraces()
	for i := 0; i < 5; i++ {
		if trip := d.observe(harness.Event{Kind: harness.EventCommand, Detail: map[string]any{"command": "x"}}); trip != nil {
			t.Fatalf("exit-less command tripped: %+v", trip)
		}
	}
}

func TestSameCallTripsOnTheNthIdenticalCall(t *testing.T) {
	d := newTraces()
	args := func() map[string]any {
		return map[string]any{"filePath": "/w/a.go", "limit": float64(200), "offset": float64(0)}
	}
	for i := 1; i <= 3; i++ {
		if trip := d.observe(toolEvent("read", args())); trip != nil {
			t.Fatalf("call %d tripped: %+v", i, trip)
		}
		// other calls in between do not reset the per-session count
		d.observe(toolEvent("read", map[string]any{"filePath": "/w/b.go"}))
		d.observe(toolEvent("grep", args()))
	}
	// same arguments in a different key order are the same call
	trip := d.observe(toolEvent("read", map[string]any{"offset": float64(0), "limit": float64(200), "filePath": "/w/a.go"}))
	if trip == nil || trip.Kind != TripSameCall || !strings.Contains(trip.Detail, "read") {
		t.Fatalf("fourth identical call: trip = %+v, want same_call naming the tool", trip)
	}
}

func TestSameCallUsesTheAdapterHashForCappedArguments(t *testing.T) {
	d := newTraces()
	ev := func(h string) harness.Event {
		e := toolEvent("webfetch", map[string]any{"url": "…capped"})
		e.Detail["args_sha256"] = h
		return e
	}
	for i := 0; i < 3; i++ {
		d.observe(ev("h1"))
	}
	if trip := d.observe(ev("h2")); trip != nil {
		t.Fatalf("different full arguments tripped: %+v", trip)
	}
	if trip := d.observe(ev("h1")); trip == nil || trip.Kind != TripSameCall {
		t.Fatalf("trip = %+v, want same_call on the fourth h1", trip)
	}
}

func TestFlipFlopTripsWhenAFileReturnsToAnEarlierContent(t *testing.T) {
	d := newTraces()
	for _, h := range []string{"A", "A", "B"} { // rewriting the same content is no flip
		if trip := d.observe(fileEvent("x.go", h)); trip != nil {
			t.Fatalf("%s tripped: %+v", h, trip)
		}
	}
	if trip := d.observe(fileEvent("y.go", "A")); trip != nil {
		t.Fatalf("another file with the same content tripped: %+v", trip)
	}
	trip := d.observe(fileEvent("x.go", "A"))
	if trip == nil || trip.Kind != TripFlipFlop || !strings.Contains(trip.Detail, "x.go") {
		t.Fatalf("A→B→A: trip = %+v, want flip_flop naming x.go", trip)
	}
	// a change without a hash (deleted, unreadable) is no evidence
	d = newTraces()
	for _, h := range []string{"A", "", "A"} {
		if trip := d.observe(fileEvent("z.go", h)); trip != nil {
			t.Fatalf("hashless change tripped: %+v", trip)
		}
	}
}

func TestChurnTripsAfterNChangesWithoutDiffGrowth(t *testing.T) {
	d := newTraces()
	d.observe(diffEvent(10, 0))
	for i := 1; i < 15; i++ {
		if trip := d.observe(fileEvent("x.go", fmt.Sprint("h", i))); trip != nil {
			t.Fatalf("change %d tripped: %+v", i, trip)
		}
		if i%5 == 0 {
			d.observe(diffEvent(6, 4)) // same size, rewritten: no growth
		}
	}
	trip := d.observe(fileEvent("x.go", "h15"))
	if trip == nil || trip.Kind != TripChurn {
		t.Fatalf("15th change without growth: trip = %+v, want churn", trip)
	}
}

func TestChurnResetsOnDiffGrowthOrNewlyPassingCommand(t *testing.T) {
	d := newTraces()
	for i := 1; i <= 14; i++ {
		d.observe(fileEvent("x.go", fmt.Sprint("h", i)))
	}
	d.observe(diffEvent(3, 0)) // grew
	for i := 1; i <= 14; i++ {
		if trip := d.observe(fileEvent("y.go", fmt.Sprint("g", i))); trip != nil {
			t.Fatalf("tripped after growth at %d: %+v", i, trip)
		}
	}
	d.observe(cmdEvent("go test", 1, "fail"))
	d.observe(cmdEvent("go test", 0, "ok")) // newly passing
	for i := 1; i <= 14; i++ {
		if trip := d.observe(fileEvent("z.go", fmt.Sprint("k", i))); trip != nil {
			t.Fatalf("tripped after a newly passing command at %d: %+v", i, trip)
		}
	}
	// a command that always passed is not newly passing
	d.observe(cmdEvent("ls", 0, ""))
	if trip := d.observe(fileEvent("z.go", "k15")); trip == nil || trip.Kind != TripChurn {
		t.Fatalf("trip = %+v, want churn: ls never failed", trip)
	}
}

func TestResetClearsOnlyTheTrippedDetector(t *testing.T) {
	d := newTraces()
	for i := 0; i < 2; i++ {
		d.observe(cmdEvent("go test", 1, "same"))
	}
	trip := d.observe(cmdEvent("go test", 1, "same"))
	if trip == nil {
		t.Fatal("no trip")
	}
	d.reset(trip)
	if trip := d.observe(cmdEvent("go test", 1, "same")); trip != nil {
		t.Fatalf("tripped right after the reset: %+v", trip)
	}
	d.observe(cmdEvent("go test", 1, "same"))
	if trip := d.observe(cmdEvent("go test", 1, "same")); trip == nil || trip.Kind != TripSameFailure {
		t.Fatalf("trip = %+v, want same_failure again after 3 more", trip)
	}
}
