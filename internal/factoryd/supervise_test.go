package factoryd

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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
	if trip == nil || trip.Kind != TripNoProgress {
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
			if trip.Kind != TripWallClock {
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
	if trip.Kind != TripRepetition {
		t.Fatalf("trip = %+v, want repetition", trip)
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

// mustSupervisor keeps the progress detectors out of reach: the tests
// using it are about the others.
func mustSupervisor(t *testing.T, noProgress, wallClock time.Duration, repetition int) *Supervisor {
	t.Helper()
	s, err := NewSupervisor(SupervisorConfig{
		NoProgressAfter:  noProgress,
		WallClockCap:     wallClock,
		RepetitionEvents: repetition,
		NoWriteAfter:     100 * time.Hour,
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
	d.observe(diffEvent(0, 0)) // the baseline checkpoint
	if trip := d.observe(fail("10:00:01")); trip != nil {
		t.Fatalf("first failure tripped: %+v", trip)
	}
	d.observe(fileEvent("a.go", "h1"))
	if trip := d.observe(fail("10:00:09")); trip != nil {
		t.Fatalf("second failure tripped: %+v", trip)
	}
	// an edit made through the shell shows as a changed diff
	d.observe(diffEvent(1, 0))
	trip := d.observe(fail("10:01:30"))
	if trip == nil || trip.Kind != TripSameFailure {
		t.Fatalf("third identical failure: trip = %+v, want same_failure", trip)
	}
	if !strings.Contains(trip.Detail, "sh scripts/loopcheck.sh") || !strings.Contains(trip.Detail, "3 times") {
		t.Fatalf("detail = %q, want the command and the count", trip.Detail)
	}
}

// `cmd; echo "EXIT: $?"` exits 0 whatever cmd did: the output decides.
func TestSameFailureSeesThroughAnEchoedExitCode(t *testing.T) {
	d := newTraces()
	var trip *Trip
	for i := 0; i < 3; i++ {
		d.observe(fileEvent("notes.md", fmt.Sprint("h", i)))
		trip = d.observe(cmdEvent(`sh scripts/loopcheck.sh; echo "EXIT: $?"`, 0,
			fmt.Sprintf("loopcheck: FAIL at 2026-09-27T12:0%d:00Z in /tmp/tmp.X%d\nloopcheck: checksum prefix b9a18334, want 00000000\nEXIT: 1", i, i)))
	}
	if trip == nil || trip.Kind != TripSameFailure || !strings.Contains(trip.Detail, "exit 0") {
		t.Fatalf("trip = %+v, want same_failure on the echoed exit code", trip)
	}
}

// A git command the sandbox's identity cannot authenticate: no edit can
// fix it, so the refusals share one run whatever command produced them
// (a supply-blocked session varies its probes) and trip without file
// changes in between — a blocked fetch reads as blocked, not as slow work.
func TestSameFailureGitAuthRefusalsShareOneRun(t *testing.T) {
	d := newTraces()
	refuse := func(url string) harness.Event {
		return cmdEvent("git fetch "+url, 128, "fatal: Authentication failed for '"+url+"'\n")
	}
	if trip := d.observe(refuse("http://forgejo/sf/r.git")); trip != nil {
		t.Fatalf("first refusal tripped: %+v", trip)
	}
	if trip := d.observe(refuse("http://github.com/x/r.git")); trip != nil {
		t.Fatalf("second refusal tripped: %+v", trip)
	}
	trip := d.observe(cmdEvent("git fetch http://forgejo/fork.git", 128,
		"remote: Forbidden\nfatal: unable to access 'http://forgejo/fork.git': The requested URL returned error: 403\n"))
	if trip == nil || trip.Kind != TripSameFailure {
		t.Fatalf("third refusal: trip = %+v, want same_failure", trip)
	}
	if !strings.Contains(trip.Detail, "refused authentication") || !strings.Contains(trip.Detail, "git credential") {
		t.Fatalf("detail = %q, want the supply refusal named", trip.Detail)
	}
}

// A test's own log can embed git's refusal words, indented: that is code
// under test failing, not the sandbox refused, and never trips the shared
// refusal run.
func TestSameFailureIndentedAuthTextIsNoRefusal(t *testing.T) {
	d := newTraces()
	fail := cmdEvent("go test ./...", 1, "    auth_test.go:20: fatal: Authentication failed for 'http://x'\nFAIL")
	for i := 0; i < 4; i++ {
		if trip := d.observe(fail); trip != nil {
			t.Fatalf("indented auth text tripped: %+v", trip)
		}
	}
}

func TestSameFailureNonTrips(t *testing.T) {
	edit := 0
	step := func(d *traceDetectors, ev harness.Event) *Trip {
		edit++
		d.observe(fileEvent("a.go", fmt.Sprint("h", edit)))
		return d.observe(ev)
	}
	// a different signature restarts the run
	d := newTraces()
	step(d, cmdEvent("go test ./...", 1, "want 1"))
	step(d, cmdEvent("go test ./...", 1, "want 1"))
	step(d, cmdEvent("go test ./...", 1, "want 2"))
	if trip := step(d, cmdEvent("go test ./...", 1, "want 2")); trip != nil {
		t.Fatalf("different signatures tripped: %+v", trip)
	}
	// a different exit code with the same output is a different result
	d = newTraces()
	step(d, cmdEvent("go test ./...", 1, "same"))
	step(d, cmdEvent("go test ./...", 1, "same"))
	if trip := step(d, cmdEvent("go test ./...", 2, "same")); trip != nil {
		t.Fatalf("a different exit code tripped: %+v", trip)
	}
	// interleaved success of the same command restarts the run
	d = newTraces()
	step(d, cmdEvent("go test ./...", 1, "want 1"))
	step(d, cmdEvent("go test ./...", 1, "want 1"))
	step(d, cmdEvent("go test ./...", 0, "ok"))
	if trip := step(d, cmdEvent("go test ./...", 1, "want 1")); trip != nil {
		t.Fatalf("failure after an interleaved success tripped: %+v", trip)
	}
	// the same output from different commands is different
	d = newTraces()
	step(d, cmdEvent("go test ./a", 1, "boom"))
	step(d, cmdEvent("go test ./b", 1, "boom"))
	if trip := step(d, cmdEvent("go test ./c", 1, "boom")); trip != nil {
		t.Fatalf("different commands tripped: %+v", trip)
	}
	// reruns with nothing changed in between are no new evidence
	d = newTraces()
	for i := 0; i < 5; i++ {
		if trip := d.observe(cmdEvent("go test ./...", 1, "want 1")); trip != nil {
			t.Fatalf("rerun %d without a change tripped: %+v", i, trip)
		}
	}
	// a command without output is no evidence either
	d = newTraces()
	for i := 0; i < 5; i++ {
		if trip := step(d, cmdEvent("true", 0, "")); trip != nil {
			t.Fatalf("silent command tripped: %+v", trip)
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
	// same arguments in a different key order are the same call; the
	// fourth earns the repetition its nudge first (limits.nudges)
	if trip := d.observe(toolEvent("read", map[string]any{"offset": float64(0), "limit": float64(200), "filePath": "/w/a.go"})); trip != nil {
		t.Fatalf("fourth identical call tripped before its nudge: %+v", trip)
	}
	if d.nudge == nil || d.nudge.Tool != "read" || d.nudge.Count != 4 || d.nudge.Text() != "You repeated the same call four times (read). Change approach, then continue." {
		t.Fatalf("nudge = %+v, want read after 4", d.nudge)
	}
	d.nudged[d.nudge.key]++ // the session took it
	var trip *Trip
	for i := 1; i <= 4; i++ {
		trip = d.observe(toolEvent("read", args()))
		d.observe(toolEvent("grep", args()))
	}
	if trip == nil || trip.Kind != TripSameCall || !strings.HasPrefix(trip.Detail, "tool read called 4 times with the same arguments") || !strings.HasSuffix(trip.Detail, "after 1 nudge") {
		t.Fatalf("four more after the nudge: trip = %+v, want same_call naming the tool", trip)
	}
}

func TestSameCallUsesTheAdapterHashForCappedArguments(t *testing.T) {
	d := newTraces()
	ev := func(h string) harness.Event {
		e := toolEvent("webfetch", map[string]any{"url": "…capped"})
		e.Detail["args_sha256"] = h
		return e
	}
	// not in a row, or the loop nudge would come first
	for i := 0; i < 3; i++ {
		d.observe(ev("h1"))
		if trip := d.observe(ev("h2")); trip != nil {
			t.Fatalf("different full arguments tripped: %+v", trip)
		}
	}
	if trip := d.observe(ev("h1")); trip != nil || d.nudge == nil {
		t.Fatalf("trip = %+v, nudge %+v; want the nudge on the fourth h1", trip, d.nudge)
	}
}

// The flip-flop detector's two halves (roadmap 184): a file's return to a
// content it had before is only pending evidence; the trip comes when a
// command that was failing when the file returned fails again after the
// return. A return while nothing fails — a trial edit undone — never
// trips, and a check that failed before the return and passes after it
// disarms the pending flip.
func TestFlipFlopTripsWhenAFailingCommandFailsAgainAfterAReturn(t *testing.T) {
	const check = "go test ./..."
	d := newTraces()
	for _, h := range []string{"A", "A", "B"} { // rewriting the same content is no flip
		if trip := d.observe(fileEvent("x.go", h)); trip != nil {
			t.Fatalf("%s tripped: %+v", h, trip)
		}
	}
	if trip := d.observe(fileEvent("y.go", "A")); trip != nil {
		t.Fatalf("another file with the same content tripped: %+v", trip)
	}
	if trip := d.observe(fileEvent("x.go", "A")); trip != nil {
		t.Fatalf("the return itself tripped: %+v", trip)
	}
	// nothing failed when x.go returned, so no refail can complete it
	if trip := d.observe(cmdEvent(check, 1, "FAIL: TestLinks")); trip != nil {
		t.Fatalf("a first failure tripped: %+v", trip)
	}
	if trip := d.observe(cmdEvent(check, 1, "FAIL: TestLinks")); trip != nil {
		t.Fatalf("a refail after a return that caught nothing failing tripped: %+v", trip)
	}
	// fail, return, fail again: the trip names the path and the command
	d = newTraces()
	if trip := d.observe(cmdEvent(check, 1, "FAIL: TestLinks")); trip != nil {
		t.Fatalf("first failure tripped: %+v", trip)
	}
	d.observe(fileEvent("x.go", "A"))
	d.observe(fileEvent("x.go", "B"))
	if trip := d.observe(fileEvent("x.go", "A")); trip != nil {
		t.Fatalf("the return tripped: %+v", trip)
	}
	trip := d.observe(cmdEvent(check, 1, "FAIL: TestLinks"))
	if trip == nil || trip.Kind != TripFlipFlop || !strings.Contains(trip.Detail, "x.go") || !strings.Contains(trip.Detail, check) {
		t.Fatalf("refail after the return = %+v, want flip_flop naming x.go and the command", trip)
	}
	// a check that failed before the return and passes after it disarms it
	d = newTraces()
	d.observe(cmdEvent(check, 1, "FAIL: TestLinks"))
	d.observe(fileEvent("x.go", "A"))
	d.observe(fileEvent("x.go", "B"))
	d.observe(fileEvent("x.go", "A"))
	if trip := d.observe(cmdEvent(check, 0, "ok")); trip != nil {
		t.Fatalf("the pass tripped: %+v", trip)
	}
	for range 3 {
		if trip := d.observe(cmdEvent(check, 1, "FAIL: TestLinks")); trip != nil {
			t.Fatalf("a failure after a passing check tripped: %+v", trip)
		}
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
	run := func(i int) *Trip {
		d.observe(fileEvent("a.go", fmt.Sprint("h", i)))
		return d.observe(cmdEvent("go test", 1, "same"))
	}
	run(1)
	run(2)
	trip := run(3)
	if trip == nil {
		t.Fatal("no trip")
	}
	d.reset(trip)
	if trip := run(4); trip != nil {
		t.Fatalf("tripped right after the reset: %+v", trip)
	}
	run(5)
	if trip := run(6); trip == nil || trip.Kind != TripSameFailure {
		t.Fatalf("trip = %+v, want same_failure again after 3 more", trip)
	}
}

func TestSupervisorRoutesEventsThroughTheTraceDetectors(t *testing.T) {
	s := mustSupervisor(t, 30*time.Minute, 10*time.Hour, 100)
	t0 := time.Unix(0, 0)
	var trip *Trip
	for i := 0; i < 3; i++ {
		s.Event(fileEvent("a.go", fmt.Sprint("h", i)), t0.Add(time.Duration(i)*time.Second))
		trip = s.Event(cmdEvent("sh scripts/loopcheck.sh", 1, "loopcheck: impossible"), t0.Add(time.Duration(i)*time.Second))
	}
	if trip == nil || trip.Kind != TripSameFailure || len(trip.Findings) != 6 {
		t.Fatalf("trip = %+v, want same_failure with the three edits and runs as findings", trip)
	}
	// continue: the detector starts over, the session runs on
	s.Resume(t0.Add(time.Minute))
	s.Event(fileEvent("a.go", "h9"), t0.Add(61*time.Second))
	if trip := s.Event(cmdEvent("sh scripts/loopcheck.sh", 1, "loopcheck: impossible"), t0.Add(61*time.Second)); trip != nil {
		t.Fatalf("tripped right after continue: %+v", trip)
	}
	// resume also restarts the silence clock
	if trip := s.Tick(t0.Add(61*time.Second + 30*time.Minute - time.Second)); trip != nil {
		t.Fatalf("stall counted the supervisor's time: %+v", trip)
	}
}

func TestSupervisorResumeResetsReadingWithoutWriting(t *testing.T) {
	s := mustSupervisor(t, time.Hour, time.Hour, 3)
	t0 := time.Unix(0, 0)
	for i := 0; i < 3; i++ {
		s.Event(toolEvent("read", map[string]any{"filePath": fmt.Sprint(i)}), t0)
	}
	if s.tripped == nil || s.tripped.Kind != TripRepetition {
		t.Fatalf("tripped = %+v, want repetition", s.tripped)
	}
	s.Resume(t0)
	for i := 3; i < 5; i++ {
		if trip := s.Event(toolEvent("read", map[string]any{"filePath": fmt.Sprint(i)}), t0); trip != nil {
			t.Fatalf("read %d tripped after the reset: %+v", i, trip)
		}
	}
}

// The event that trips reading-without-writing still reaches the trace
// detectors.
func TestSupervisorTracesSeeTheEventThatTripsRepetition(t *testing.T) {
	s := mustSupervisor(t, time.Hour, 10*time.Hour, 2)
	t0 := time.Unix(0, 0)
	s.Event(cmdEvent("go test", 1, "boom"), t0)
	if trip := s.Event(cmdEvent("go vet", 1, "vet says"), t0); trip == nil || trip.Kind != TripRepetition {
		t.Fatalf("trip = %+v, want repetition", trip)
	}
	if !s.traces.failed["go vet"] {
		t.Fatal("the tripping event never reached the trace detectors")
	}
}

// A healthy edit → test (green, same output) loop is progress, not a
// circle; only a command that masks its exit status is judged by output.
func TestSameFailureAPassEndsTheRunUnlessTheExitIsMasked(t *testing.T) {
	d := newTraces()
	for i := 0; i < 6; i++ {
		d.observe(fileEvent("a.go", fmt.Sprint("h", i)))
		if trip := d.observe(cmdEvent("go test ./...", 0, "ok  \texample.com/p\t0.0"+fmt.Sprint(i)+"1s")); trip != nil {
			t.Fatalf("passing run %d tripped: %+v", i, trip)
		}
	}
	for _, masked := range []string{`sh scripts/loopcheck.sh; echo "EXIT: $?"`, "go test ./... | tail -5", "make check || true", "go test ./... &", "go test ./...\necho done"} {
		d := newTraces()
		var trip *Trip
		for i := 0; i < 3; i++ {
			d.observe(fileEvent("a.go", fmt.Sprint("h", i)))
			trip = d.observe(cmdEvent(masked, 0, "FAIL: want 1\nEXIT: 1"))
		}
		if trip == nil || trip.Kind != TripSameFailure {
			t.Fatalf("%s: trip = %+v, want same_failure", masked, trip)
		}
	}
}

// The first checkpoint only sets the baseline; it is no change.
func TestSameFailureFirstCheckpointIsNoChange(t *testing.T) {
	d := newTraces()
	d.observe(cmdEvent("go test", 1, "boom"))
	d.observe(diffEvent(5, 0)) // baseline
	d.observe(cmdEvent("go test", 1, "boom"))
	d.observe(diffEvent(6, 0)) // a change
	if trip := d.observe(cmdEvent("go test", 1, "boom")); trip != nil {
		t.Fatalf("tripped after one real change: %+v", trip)
	}
	d.observe(diffEvent(7, 0))
	if trip := d.observe(cmdEvent("go test", 1, "boom")); trip == nil {
		t.Fatal("no trip after the second real change")
	}
}

func minute(m float64) time.Time { return time.Unix(0, 0).Add(time.Duration(m * float64(time.Minute))) }

// no_write with the policy's default wait (5 min): only an agent's event
// trips it, the wait after the last write; a command, an unchanged
// checkpoint or silence never do (a changed diff is a write:
// TestShellWriteCountsAsWrite).
func TestNoWriteTripsOnAnAgentEventFiveMinutesAfterTheLastWrite(t *testing.T) {
	s, err := NewSupervisor(SupervisorConfig{NoProgressAfter: 10 * time.Hour, WallClockCap: 100 * time.Hour, RepetitionEvents: 1000},
		time.Unix(0, 0), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		at float64
		ev *harness.Event
	}{
		{2, &harness.Event{Kind: harness.EventFileChange, Detail: map[string]any{"path": "main.go"}}},
		{5, ptr(cmdEvent("sed -i s/a/b/ main.go", 0, ""))},
		{6, ptr(diffEvent(3, 1))},
		{7 - 1.0/60, &harness.Event{Kind: harness.EventModelCall}},
		{7.5, nil},
		{7 + 40.0/60, ptr(diffEvent(3, 1))},
	} {
		var trip *Trip
		if step.ev == nil {
			trip = s.Tick(minute(step.at))
		} else {
			trip = s.Event(*step.ev, minute(step.at))
		}
		if trip != nil {
			t.Fatalf("minute %.2f tripped: %+v", step.at, trip)
		}
	}
	trip := s.Event(toolEvent("grep", map[string]any{"pattern": "x"}), minute(7.75))
	if trip == nil || trip.Kind != TripNoWrite || trip.Detail != "agent events for 5m45s without a file write or edit" {
		t.Fatalf("trip = %+v, want no_write at 7:45", trip)
	}
	s.Resume(minute(8))
	if trip := s.Event(harness.Event{Kind: harness.EventModelCall}, minute(13).Add(-time.Second)); trip != nil {
		t.Fatalf("tripped again before 5 min after the continue: %+v", trip)
	}
	if trip := s.Event(harness.Event{Kind: harness.EventModelCall}, minute(13)); trip == nil || trip.Kind != TripNoWrite {
		t.Fatalf("trip = %+v, want no_write again at 13:00", trip)
	}
}

// A command's own running time is the machine's, not the agent's: a
// compile of 3 min 30 s does not count towards no_write (Stage 11's gate
// lane task tripped it on a cold `go test` compile in the sandbox).
func TestNoWriteExcludesACommandsRunningTime(t *testing.T) {
	s, err := NewSupervisor(SupervisorConfig{NoProgressAfter: 10 * time.Hour, WallClockCap: 100 * time.Hour, RepetitionEvents: 1000},
		time.Unix(0, 0), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if trip := s.Event(fileEvent("a_test.go", "1"), minute(2)); trip != nil {
		t.Fatalf("write tripped: %+v", trip)
	}
	build := cmdEvent("go test ./internal/factoryd/ -run TestX", 0, "ok")
	build.Detail["duration_ms"] = float64(210000)
	if trip := s.Event(build, minute(6)); trip != nil {
		t.Fatalf("the command tripped: %+v", trip)
	}
	if trip := s.Event(toolEvent("grep", map[string]any{"pattern": "x"}), minute(7.25)); trip != nil {
		t.Fatalf("tripped at 7:15 after 1m45s of agent time: %+v", trip)
	}
	trip := s.Event(toolEvent("grep", map[string]any{"pattern": "y"}), minute(10.75))
	if trip == nil || trip.Kind != TripNoWrite || trip.Detail != "agent events for 5m15s without a file write or edit" {
		t.Fatalf("trip = %+v, want no_write at 10:45 with 5m15s", trip)
	}
}

func ptr[T any](v T) *T { return &v }

func TestNoWriteRejectsANegativeWait(t *testing.T) {
	if _, err := NewSupervisor(SupervisorConfig{NoProgressAfter: time.Minute, WallClockCap: time.Hour, RepetitionEvents: 10, NoWriteAfter: -time.Minute},
		time.Unix(0, 0), time.Unix(0, 0)); err == nil {
		t.Fatal("a negative wait was accepted")
	}
}

// A session's progress detectors, agent time and calls are its policy's.
func TestSessionSupervisorTakesThePolicysProgressDetectors(t *testing.T) {
	r := &Runner{cfg: RunnerConfig{Supervisor: SupervisorConfig{NoProgressAfter: time.Minute, RepetitionEvents: 24}}}
	d := policy.Document{Supervisor: map[string]int{policy.DetectorNoWrite: 60},
		Limits: policy.Limits{AgentTime: 45 * time.Minute, SupervisorCalls: 2}}
	c := r.supervisorConfig(d, d.Limits.AgentTime, d.Limits.CheckIn)
	if c.NoWriteAfter != time.Minute || c.RepetitionEvents != 24 || c.WallClockCap != 45*time.Minute {
		t.Fatalf("config = %+v, want no_write 1m, agent time 45m and 2 calls from the policy, the rest as configured", c)
	}
}

// The limits of the version in force when a task's workflow starts are
// the task's: its session cap, agent time and stronger rung, and the
// version it pinned. The stages are its process's, which TaskConfig reads.
func TestWorkflowConfigTakesTheLimits(t *testing.T) {
	v := policy.Version{ID: 9, Tree: policy.Tree{Document: policy.Document{
		Limits: policy.Limits{Sessions: 5, AgentTime: 30 * time.Minute, SupervisorCalls: 2, SupervisorTimeout: 6 * time.Minute, StrongerAfter: 2}},
		Processes: map[string]policy.Process{"maintenance": {Stages: testStages()}}}}
	r := &Runner{cfg: RunnerConfig{Models: ModelLadder{Start: "a", Stronger: "b"}, ChecksTimeout: time.Hour,
		Policy: func() policy.Version { return v }}}
	want := WorkflowConfig{Models: ModelLadder{Start: "a", Stronger: "b"}, MaxSessions: 5, WallClockCap: 30 * time.Minute,
		ChecksTimeout: time.Hour, StrongerAfter: 2, PolicyVersion: 9}
	if got := r.WorkflowConfig(); !reflect.DeepEqual(got, want) {
		t.Fatalf("WorkflowConfig = %+v, want %+v", got, want)
	}
}

// A report session writes no file by design: neither repetition nor
// no_write trips it; the wall-clock cap still does.
func TestReadOnlySessionTakesNoWriteDetectors(t *testing.T) {
	start := time.Unix(0, 0)
	s, err := NewSupervisor(SupervisorConfig{NoProgressAfter: time.Hour, WallClockCap: 20 * time.Minute, RepetitionEvents: 3,
		NoWriteAfter: time.Minute, ReadOnly: true}, start, start)
	if err != nil {
		t.Fatal(err)
	}
	// nine reads over eight minutes: three times the repetition count and
	// eight times the no_write wait
	for i := range 9 {
		ev := toolEvent("read", map[string]any{"path": fmt.Sprintf("f%d.go", i)})
		if trip := s.Event(ev, start.Add(time.Duration(i)*time.Minute)); trip != nil {
			t.Fatalf("event %d tripped: %+v", i, trip)
		}
	}
	if trip := s.Event(harness.Event{Kind: harness.EventModelCall}, start.Add(20*time.Minute)); trip == nil || trip.Kind != TripWallClock {
		t.Fatalf("trip = %+v, want the wall-clock cap", trip)
	}
}

// The loop nudge (queue item C): three identical calls in a row earn one
// nudge and start the count over; a different call in between breaks the
// run; three again after the nudge trip the supervisor.
func TestNudgeComesOnceBeforeTheTrip(t *testing.T) {
	s := mustSupervisor(t, 30*time.Minute, 10*time.Hour, 100)
	t0 := time.Unix(0, 0)
	read := func() harness.Event { return toolEvent("read", map[string]any{"filePath": "/w/a.go"}) }
	step := harness.Event{Kind: harness.EventModelCall}
	glob := func() harness.Event { return toolEvent("glob", map[string]any{"pattern": "*.go"}) }
	// the per-session same_call count (4) sees these three globs too
	for i, ev := range []harness.Event{glob(), step, glob(), toolEvent("grep", map[string]any{"pattern": "x"}), glob(), step, read(), read()} {
		if trip := s.Event(ev, t0); trip != nil {
			t.Fatalf("event %d tripped: %+v", i, trip)
		}
		if n := s.TakeNudge(); n != nil {
			t.Fatalf("event %d nudged: %+v", i, n)
		}
	}
	// the third read in a row; model steps in between do not break it
	if trip := s.Event(read(), t0); trip != nil {
		t.Fatalf("third in a row tripped: %+v", trip)
	}
	n := s.TakeNudge()
	if n == nil || n.Tool != "read" || n.Count != 3 {
		t.Fatalf("nudge = %+v, want read after 3", n)
	}
	if n.Text() != "You repeated the same call three times (read). Change approach, then continue." {
		t.Fatalf("text = %q", n.Text())
	}
	if s.TakeNudge() != nil {
		t.Fatal("a nudge taken twice")
	}
	s.NudgeSent(*n)
	// the count starts over: the same_call detector's per-session count too
	for i := 1; i <= 2; i++ {
		if trip := s.Event(read(), t0); trip != nil || s.TakeNudge() != nil {
			t.Fatalf("call %d after the nudge: trip %+v", i, trip)
		}
	}
	trip := s.Event(read(), t0)
	if trip == nil || trip.Kind != TripSameCall || trip.Detail != "read: the same call 3 times in a row after 1 nudge" {
		t.Fatalf("three again after the nudge: trip = %+v, want same_call", trip)
	}
	if s.TakeNudge() != nil {
		t.Fatal("the trip came with a nudge")
	}
	// after a continue the repetition trips again, it earns no second nudge
	s.Resume(t0)
	for i := 1; i <= 2; i++ {
		s.Event(read(), t0)
	}
	if trip := s.Event(read(), t0); trip == nil || trip.Kind != TripSameCall || s.TakeNudge() != nil {
		t.Fatalf("after continue: trip = %+v", trip)
	}
}

// Commands and file writes are calls too: the same command line three times
// in a row is nudged, under the bash tool.
func TestNudgeCountsCommandsAndWrites(t *testing.T) {
	d := newTraces()
	for i := 0; i < 3; i++ {
		d.observe(cmdEvent("go build ./...", 0, "ok"))
	}
	if d.nudge == nil || d.nudge.Tool != "bash" {
		t.Fatalf("nudge = %+v, want bash", d.nudge)
	}
	d.nudge = nil
	write := harness.Event{Kind: harness.EventFileChange, Detail: map[string]any{"path": "a.go", "tool": "write", "sha256": "h1"}}
	d.observe(write)
	d.observe(write)
	d.observe(harness.Event{Kind: harness.EventFileChange, Detail: map[string]any{"path": "a.go", "tool": "write", "sha256": "h2"}})
	d.observe(write)
	if d.nudge != nil {
		t.Fatalf("different content in between nudged: %+v", d.nudge)
	}
	d.observe(write)
	d.observe(write)
	if d.nudge == nil || d.nudge.Tool != "write" {
		t.Fatalf("nudge = %+v, want write", d.nudge)
	}
}

func TestNudgeLimitsFromThePinnedPolicy(t *testing.T) {
	var d policy.Document
	d.Limits.NudgeAfter, d.Limits.Nudges = 5, 2
	r := &Runner{}
	if c := r.supervisorConfig(d, d.Limits.AgentTime, d.Limits.CheckIn); c.NudgeAfter != 5 || c.Nudges != 2 {
		t.Fatalf("config = %d after, %d nudges; want 5, 2", c.NudgeAfter, c.Nudges)
	}
	if c := r.supervisorConfig(policy.Document{}, 0, 0); c.NudgeAfter != 3 || c.Nudges != 1 {
		t.Fatalf("defaults = %d after, %d nudges; want 3, 1", c.NudgeAfter, c.Nudges)
	}
}

// Scattered earlier identical calls count toward the nudge, never past it:
// h1 h2 h1 h1 h1 is nudged at the fourth h1, where same_call's per-session
// count would have tripped.
func TestNudgePrecedesSameCallOnScatteredCalls(t *testing.T) {
	d := newTraces()
	h := func(v string) harness.Event {
		e := toolEvent("webfetch", map[string]any{"url": v})
		e.Detail["args_sha256"] = v
		return e
	}
	for i, v := range []string{"h1", "h2", "h1", "h1"} {
		if trip := d.observe(h(v)); trip != nil || d.nudge != nil {
			t.Fatalf("call %d: trip %+v, nudge %+v", i, trip, d.nudge)
		}
	}
	if trip := d.observe(h("h1")); trip != nil || d.nudge == nil || d.nudge.Count != 3 {
		t.Fatalf("fourth h1: trip %+v, nudge %+v; want the nudge after 3 in a row", trip, d.nudge)
	}
}

// nudgeOn feeds ev three times in a row and sends the nudge it earns,
// returning NudgeSent's trip.
func nudgeOn(t *testing.T, s *Supervisor, ev harness.Event) *Trip {
	t.Helper()
	for i := 0; i < 3; i++ {
		if trip := s.Event(ev, time.Unix(0, 0)); trip != nil {
			t.Fatalf("call %d of %v tripped: %+v", i, ev.Detail, trip)
		}
	}
	n := s.TakeNudge()
	if n == nil {
		t.Fatalf("no nudge after three of %v", ev.Detail)
	}
	return s.NudgeSent(*n)
}

func readEvent(path string) harness.Event { return toolEvent("read", map[string]any{"filePath": path}) }

// The loop rule: the third nudge delivered with no new file content since
// the last one ends the session as a redirect, decided without the
// supervisor model. nudge 1, a call, nudge 2, a file change, nudges 3, 4
// and 5: the trip comes at nudge 5, the third since the change.
func TestLoopEndsAtTheThirdNudgeWithoutNewContent(t *testing.T) {
	t0 := time.Unix(0, 0)
	read, nudge := readEvent, func(s *Supervisor, ev harness.Event) *Trip { return nudgeOn(t, s, ev) }
	s := mustSupervisor(t, 30*time.Minute, 10*time.Hour, 100)
	steps := []func() *Trip{
		func() *Trip { return nudge(s, read("/w/a.go")) },
		func() *Trip { return s.Event(toolEvent("glob", map[string]any{"pattern": "*.go"}), t0) },
		func() *Trip { return nudge(s, cmdEvent("go test ./...", 1, "FAIL")) },
		func() *Trip { return s.Event(fileEvent("a_test.go", "h1"), t0) },
		func() *Trip { return nudge(s, read("/w/b.go")) },
		func() *Trip { return nudge(s, cmdEvent("go vet ./...", 1, "vet: x")) },
	}
	for i, step := range steps {
		if trip := step(); trip != nil {
			t.Fatalf("step %d tripped: %+v", i, trip)
		}
	}
	trip := nudge(s, read("/w/c.go"))
	want := "three nudges without new file content in between: read `{\"filePath\":\"/w/b.go\"}` 3 times; bash `go vet ./...` 3 times; read `{\"filePath\":\"/w/c.go\"}` 3 times"
	if trip == nil || trip.Kind != TripLoop || trip.Detail != want {
		t.Fatalf("nudge 5: trip = %+v, want loop with %q", trip, want)
	}
	if trip.decided == nil || trip.decided.Action != ActionRedirect {
		t.Fatalf("decision = %+v, want a redirect decided without the model", trip.decided)
	}
	if again := s.Event(read("/w/d.go"), t0); again != trip {
		t.Fatalf("after the trip: %+v, want the sticky loop trip", again)
	}

	// a report session writes no file: its nudges are no loop
	ro, err := NewSupervisor(SupervisorConfig{NoProgressAfter: time.Hour, WallClockCap: 10 * time.Hour, RepetitionEvents: 100, ReadOnly: true}, t0, t0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/w/a.go", "/w/b.go", "/w/c.go"} {
		if trip := nudge(ro, read(p)); trip != nil {
			t.Fatalf("report session: nudge of %s tripped %+v", p, trip)
		}
	}
}

// Only content the session has not written before resets the loop count
// (ruling 280): a rewrite of the same content, or a shell diff back at an
// earlier state, is no progress. h0, nudge, h1, then nudge and h1 twice
// more, and a nudge: the trip comes at the third nudge after the first h1.
// Through the shell: p2 and p3 are new, p2 and p3 again are not, so the
// third nudge after the first p3 trips.
func TestLoopResetsOnlyOnNewContent(t *testing.T) {
	t0 := time.Unix(0, 0)
	s := mustSupervisor(t, 30*time.Minute, 10*time.Hour, 100)
	steps := []func() *Trip{
		func() *Trip { return s.Event(fileEvent("a.go", "h0"), t0) },
		func() *Trip { return nudgeOn(t, s, readEvent("/w/1.go")) },
		func() *Trip { return s.Event(fileEvent("a.go", "h1"), t0) },
		func() *Trip { return nudgeOn(t, s, readEvent("/w/2.go")) },
		func() *Trip { return s.Event(fileEvent("a.go", "h1"), t0) },
		func() *Trip { return nudgeOn(t, s, readEvent("/w/3.go")) },
		func() *Trip { return s.Event(fileEvent("a.go", "h1"), t0) },
	}
	for i, step := range steps {
		if trip := step(); trip != nil {
			t.Fatalf("step %d tripped: %+v", i, trip)
		}
	}
	if trip := nudgeOn(t, s, readEvent("/w/4.go")); trip == nil || trip.Kind != TripLoop || !strings.Contains(trip.Detail, "/w/2.go") || strings.Contains(trip.Detail, "/w/1.go") {
		t.Fatalf("third nudge after h1: trip = %+v, want loop over 2, 3 and 4", trip)
	}

	checkpoint := func(ins int, patch string) harness.Event {
		return harness.Event{Kind: harness.EventCheckpoint, Detail: map[string]any{
			"diff": map[string]any{"files": float64(1), "insertions": float64(ins), "deletions": float64(0)}, "patch": patch}}
	}
	s = mustSupervisor(t, 30*time.Minute, 10*time.Hour, 100)
	steps = []func() *Trip{
		func() *Trip { return nudgeOn(t, s, readEvent("/w/1.go")) },
		func() *Trip { return s.Event(checkpoint(1, "p1"), t0) }, // the baseline
		func() *Trip { return s.Event(checkpoint(2, "p2"), t0) },
		func() *Trip { return nudgeOn(t, s, readEvent("/w/2.go")) },
		func() *Trip { return s.Event(checkpoint(3, "p3"), t0) },
		func() *Trip { return nudgeOn(t, s, readEvent("/w/3.go")) },
		func() *Trip { return s.Event(checkpoint(2, "p2"), t0) },
		func() *Trip { return nudgeOn(t, s, readEvent("/w/4.go")) },
		func() *Trip { return s.Event(checkpoint(3, "p3"), t0) },
	}
	for i, step := range steps {
		if trip := step(); trip != nil {
			t.Fatalf("shell step %d tripped: %+v", i, trip)
		}
	}
	if trip := nudgeOn(t, s, readEvent("/w/5.go")); trip == nil || trip.Kind != TripLoop || !strings.Contains(trip.Detail, "/w/3.go") || strings.Contains(trip.Detail, "/w/2.go") {
		t.Fatalf("third nudge after p3: trip = %+v, want loop over 3, 4 and 5", trip)
	}
}

// Only a nudge the session took is spent: one taken but not sent, and one
// dropped because the same event tripped, leave the next run its nudge.
func TestNudgeNotSentIsNotSpent(t *testing.T) {
	s := mustSupervisor(t, 30*time.Minute, 10*time.Hour, 100)
	t0 := time.Unix(0, 0)
	read := func() harness.Event { return toolEvent("read", map[string]any{"filePath": "/w/a.go"}) }
	for i := 0; i < 3; i++ {
		s.Event(read(), t0)
	}
	if s.TakeNudge() == nil {
		t.Fatal("no nudge after three")
	}
	// not sent: the next run is nudged again, not tripped
	for i := 0; i < 2; i++ {
		s.Event(read(), t0)
	}
	if trip := s.Event(read(), t0); trip != nil || s.TakeNudge() == nil {
		t.Fatalf("second run after an unsent nudge: trip %+v", trip)
	}
	// earned on the event the wall clock trips: dropped with it
	s2 := mustSupervisor(t, 30*time.Minute, time.Hour, 100)
	s2.Event(read(), t0)
	s2.Event(read(), t0)
	if trip := s2.Event(read(), t0.Add(time.Hour)); trip == nil || trip.Kind != TripWallClock {
		t.Fatalf("trip = %+v, want wall_clock", trip)
	}
	s2.Resume(t0)
	s2.deadline = t0.Add(10 * time.Hour)
	if s2.TakeNudge() != nil {
		t.Fatal("the dropped nudge survived the trip")
	}
	for i := 0; i < 2; i++ {
		s2.Event(read(), t0)
	}
	if trip := s2.Event(read(), t0); trip != nil || s2.TakeNudge() == nil {
		t.Fatalf("the next run after a dropped nudge: trip %+v, want its nudge", trip)
	}
}

// The check-in trips at every CheckIn interval counted from the task's
// anchor, across sessions: anchor t0, 45m, a session from t0+50m trips at
// t0+90m and t0+135m and not between.
func TestCheckInEveryInterval(t *testing.T) {
	cfg := SupervisorConfig{NoProgressAfter: 10 * time.Hour, WallClockCap: 4 * time.Hour, RepetitionEvents: 1000,
		NoWriteAfter: 100 * time.Hour, CheckIn: 45 * time.Minute}
	s, err := NewSupervisor(cfg, minute(0), minute(50))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []float64{50, 89.99, 90, 100, 134.99, 135, 179.99} {
		trip := s.Tick(minute(m))
		want := m == 90 || m == 135
		if (trip != nil) != want {
			t.Fatalf("minute %.2f: trip = %+v, want a check-in: %v", m, trip, want)
		}
		if trip == nil {
			continue
		}
		if trip.Kind != TripCheckIn || trip.Detail != "scheduled check-in, no detector tripped" {
			t.Fatalf("minute %.2f: trip = %+v", m, trip)
		}
		s.Resume(minute(m))
	}
	// an event on the interval trips it too
	s2, _ := NewSupervisor(cfg, minute(0), minute(50))
	if trip := s2.Event(harness.Event{Kind: harness.EventModelCall}, minute(90)); trip == nil || trip.Kind != TripCheckIn {
		t.Fatalf("event at t0+90m: trip = %+v, want check_in", trip)
	}
	// no interval, no check-in
	cfg.CheckIn = 0
	s3, _ := NewSupervisor(cfg, minute(0), minute(50))
	if trip := s3.Tick(minute(135)); trip != nil {
		t.Fatalf("CheckIn 0 tripped: %+v", trip)
	}
}

// A file written through a shell shows only as a changed checkpoint diff:
// it counts as a write for repetition and no_write. The first checkpoint
// is the baseline.
func TestShellWriteCountsAsWrite(t *testing.T) {
	s, err := NewSupervisor(SupervisorConfig{NoProgressAfter: 10 * time.Hour, WallClockCap: 100 * time.Hour, RepetitionEvents: 100},
		minute(0), minute(0))
	if err != nil {
		t.Fatal(err)
	}
	s.Event(cmdEvent("ls", 0, ""), minute(1))
	s.Event(diffEvent(3, 0), minute(2)) // 1 file +3: the baseline
	s.Event(cmdEvent("cat <<EOF > a.go", 0, ""), minute(3))
	s.Event(cmdEvent("go build", 0, ""), minute(4))
	if s.sinceChange != 3 || !s.noWriteFrom.Equal(minute(0)) {
		t.Fatalf("before the change: sinceChange %d, no_write anchor %s; want 3 and minute 0", s.sinceChange, s.noWriteFrom)
	}
	s.Event(diffEvent(9, 0), minute(5)) // 1 file +9
	if s.sinceChange != 0 || !s.noWriteFrom.Equal(minute(5)) {
		t.Fatalf("after the change: sinceChange %d, no_write anchor %s; want 0 and minute 5", s.sinceChange, s.noWriteFrom)
	}
}

// A delivered message arms the stuck rule: the next detector trip without
// a write since ends the session as retry_stronger, decided already; a
// write disarms it; a check-in never ends it.
func TestSupervisorStuckAfterMessage(t *testing.T) {
	cfg := SupervisorConfig{NoProgressAfter: 10 * time.Hour, WallClockCap: 100 * time.Hour, RepetitionEvents: 2,
		NoWriteAfter: 100 * time.Hour, CheckIn: 45 * time.Minute}
	s, _ := NewSupervisor(cfg, minute(0), minute(0))
	s.Event(toolEvent("read", map[string]any{"filePath": "a"}), minute(1))
	if trip := s.Event(toolEvent("read", map[string]any{"filePath": "b"}), minute(1)); trip == nil || trip.decided != nil {
		t.Fatalf("first trip = %+v, want repetition for the supervisor", trip)
	}
	s.Messaged(minute(2))
	if trip := s.Tick(minute(45)); trip == nil || trip.Kind != TripCheckIn || trip.decided != nil {
		t.Fatalf("check-in after the message = %+v, want a check-in for the supervisor", trip)
	}
	s.Resume(minute(45))
	s.Event(toolEvent("read", map[string]any{"filePath": "c"}), minute(46))
	trip := s.Event(toolEvent("read", map[string]any{"filePath": "d"}), minute(46))
	if trip == nil || trip.decided == nil || trip.decided.Action != ActionRetryStronger {
		t.Fatalf("trip = %+v, want repetition decided retry_stronger", trip)
	}
	if last := trip.Findings[len(trip.Findings)-1]; last != "stuck after the supervisor's message: repetition: 2 tool calls and commands without a file change" {
		t.Fatalf("finding = %q", last)
	}

	s2, _ := NewSupervisor(cfg, minute(0), minute(0))
	s2.Event(toolEvent("read", map[string]any{"filePath": "a"}), minute(1))
	s2.Event(toolEvent("read", map[string]any{"filePath": "b"}), minute(1))
	s2.Messaged(minute(1))
	s2.Event(fileEvent("a.go", "1"), minute(2))
	s2.Event(toolEvent("read", map[string]any{"filePath": "c"}), minute(3))
	if trip := s2.Event(toolEvent("read", map[string]any{"filePath": "d"}), minute(3)); trip == nil || trip.decided != nil {
		t.Fatalf("trip after a write = %+v, want one for the supervisor", trip)
	}
}

// A message starts every progress detector afresh: a detector that did
// not trip keeps no count from before the message (review I1).
func TestMessageStartsTheDetectorsAfresh(t *testing.T) {
	cfg := SupervisorConfig{NoProgressAfter: 10 * time.Hour, WallClockCap: 100 * time.Hour, RepetitionEvents: 24,
		NoWriteAfter: 5 * time.Minute, CheckIn: 45 * time.Minute}
	// (a) 24 reads in 4 minutes trip repetition; after the message at 4:06
	// the first model call at 5:00 is no no_write trip
	s, _ := NewSupervisor(cfg, minute(0), minute(0))
	var trip *Trip
	for i := range 24 {
		trip = s.Event(toolEvent("read", map[string]any{"filePath": fmt.Sprint(i)}), minute(float64(i)/6))
	}
	if trip == nil || trip.Kind != TripRepetition {
		t.Fatalf("trip = %+v, want repetition", trip)
	}
	s.Messaged(minute(4.1))
	if trip := s.Event(harness.Event{Kind: harness.EventModelCall}, minute(5)); trip != nil {
		t.Fatalf("model call at 5:00 after the message tripped: %+v", trip)
	}
	// (b) a check-in at 45 min with 20 reads since the last write, a
	// message: 23 reads give no trip, the 24th is stuck
	s2, _ := NewSupervisor(cfg, minute(0), minute(0))
	s2.Event(fileEvent("a.go", "1"), minute(42))
	for i := range 20 {
		s2.Event(toolEvent("read", map[string]any{"filePath": fmt.Sprint(i)}), minute(42+float64(i)/10))
	}
	if trip := s2.Tick(minute(45)); trip == nil || trip.Kind != TripCheckIn {
		t.Fatalf("trip = %+v, want the check-in", trip)
	}
	s2.Messaged(minute(45))
	for i := range 23 {
		if trip := s2.Event(toolEvent("read", map[string]any{"filePath": fmt.Sprint("m", i)}), minute(45.5)); trip != nil {
			t.Fatalf("read %d after the message tripped: %+v", i+1, trip)
		}
	}
	trip = s2.Event(toolEvent("read", map[string]any{"filePath": "m23"}), minute(45.5))
	if trip == nil || trip.Kind != TripRepetition || trip.decided == nil || trip.decided.Action != ActionRetryStronger {
		t.Fatalf("24th read: %+v, want repetition decided retry_stronger", trip)
	}
	// the trace counters start afresh too: a repeated call counted before
	// the message does not trip same_call after it
	s3, _ := NewSupervisor(cfg, minute(0), minute(0))
	s3.Event(diffEvent(3, 0), minute(1))
	for i := range 3 {
		s3.Event(cmdEvent("go test ./...", 1, "FAIL"), minute(1))
		s3.Event(toolEvent("read", map[string]any{"filePath": "x"}), minute(1))
		s3.Event(fileEvent("a.go", fmt.Sprint(i)), minute(1))
	}
	if d := s3.traces; len(d.calls) == 0 || len(d.runs) == 0 || len(d.failed) == 0 || len(d.contents) == 0 || d.churn == 0 || d.streak == 0 {
		t.Fatalf("trace counters before the message are empty: %+v", d)
	}
	s3.traces.nudged = map[string]int{"kept": 1}
	s3.tripped = cmp.Or(s3.tripped, &Trip{Kind: TripCheckIn})
	s3.Messaged(minute(2))
	// the command detectors clear; the alternation memory (contents, churn)
	// stays, so a circle of writes spanning messages trips (ruling 224,
	// TestSupervisorFlipFlopSpansTheMessage)
	if d := s3.traces; len(d.calls) != 0 || len(d.runs) != 0 || len(d.failed) != 0 || len(d.contents) == 0 || d.churn == 0 || d.streak != 0 ||
		d.lastDiff == "" || d.maxDiff != 3 || d.nudged["kept"] != 1 {
		t.Fatalf("trace counters after the message: %+v", d)
	}
}

func subagentEvent(tool string, args map[string]any) harness.Event {
	ev := toolEvent(tool, args)
	ev.Detail["subagent"] = true
	return ev
}

// A subagent's run counts once for repetition, as the session's task call:
// its reads add nothing, its writes still reset the count.
func TestSubagentCallsCountOnce(t *testing.T) {
	cfg := SupervisorConfig{NoProgressAfter: 10 * time.Hour, WallClockCap: 100 * time.Hour, RepetitionEvents: 3, NoWriteAfter: time.Hour}
	read := func(path string) harness.Event { return toolEvent("read", map[string]any{"filePath": path}) }
	s, _ := NewSupervisor(cfg, minute(0), minute(0))
	for i := range 5 {
		if trip := s.Event(subagentEvent("read", map[string]any{"filePath": fmt.Sprintf("s%d.go", i)}), minute(1)); trip != nil {
			t.Fatalf("subagent read %d tripped: %+v", i, trip)
		}
	}
	if trip := s.Event(read("a.go"), minute(2)); trip != nil {
		t.Fatalf("the first own read tripped: %+v", trip)
	}
	s.Event(read("b.go"), minute(2))
	trip := s.Event(read("c.go"), minute(2))
	if trip == nil || trip.Kind != TripRepetition || trip.Detail != "3 tool calls and commands without a file change" {
		t.Fatalf("trip = %+v, want repetition at the third own call", trip)
	}

	s2, _ := NewSupervisor(cfg, minute(0), minute(0))
	s2.Event(read("a.go"), minute(1))
	s2.Event(read("b.go"), minute(1))
	write := fileEvent("a.go", "h1")
	write.Detail["subagent"] = true
	s2.Event(write, minute(2))
	for _, path := range []string{"c.go", "d.go"} {
		if trip := s2.Event(read(path), minute(3)); trip != nil {
			t.Fatalf("a read after the subagent's write tripped: %+v", trip)
		}
	}
}

// A nudge or trip about a subagent's call would reach the main agent, which
// did not make it: six identical subagent reads, past the nudge (3 in a row)
// and same_call (4), leave no trace; six identical own reads after them get
// the nudge at the third and the trip at the sixth.
func TestSubagentCallsFeedNoTraceDetector(t *testing.T) {
	s := mustSupervisor(t, 30*time.Minute, 10*time.Hour, 100)
	t0 := time.Unix(0, 0)
	args := map[string]any{"filePath": "/w/a.go"}
	for i := range 6 {
		if trip := s.Event(subagentEvent("read", args), t0); trip != nil {
			t.Fatalf("subagent read %d tripped: %+v", i, trip)
		}
		if n := s.TakeNudge(); n != nil {
			t.Fatalf("subagent read %d nudged: %+v", i, n)
		}
	}
	for i := 1; i <= 5; i++ {
		if trip := s.Event(toolEvent("read", args), t0); trip != nil {
			t.Fatalf("own read %d tripped: %+v", i, trip)
		}
		n := s.TakeNudge()
		if i != 3 {
			if n != nil {
				t.Fatalf("own read %d nudged: %+v", i, n)
			}
			continue
		}
		if n == nil || n.Tool != "read" || n.Count != 3 {
			t.Fatalf("nudge = %+v, want read after 3", n)
		}
		s.NudgeSent(*n)
	}
	trip := s.Event(toolEvent("read", args), t0)
	if trip == nil || trip.Kind != TripSameCall || trip.Detail != "read: the same call 3 times in a row after 1 nudge" {
		t.Fatalf("the sixth own read: trip = %+v, want same_call", trip)
	}
}

// Silence after a message is no circle: a command running over 5 minutes
// trips no_progress, which the supervisor is asked about (review I2).
func TestNoProgressAfterMessageAsks(t *testing.T) {
	cfg := SupervisorConfig{NoProgressAfter: 5 * time.Minute, WallClockCap: 100 * time.Hour, RepetitionEvents: 2, NoWriteAfter: time.Hour}
	s, _ := NewSupervisor(cfg, minute(0), minute(0))
	s.Event(toolEvent("read", map[string]any{"filePath": "a"}), minute(1))
	s.Event(toolEvent("read", map[string]any{"filePath": "b"}), minute(1))
	s.Messaged(minute(1))
	trip := s.Tick(minute(6.5))
	if trip == nil || trip.Kind != TripNoProgress || trip.decided != nil {
		t.Fatalf("trip = %+v, want no_progress for the supervisor", trip)
	}
}

// Ruling 224: flip_flop and churn after a delivered message are circles,
// not progress, although they write: they end the session too.
func TestFlipFlopAfterMessageIsStuck(t *testing.T) {
	const check = "go test ./..."
	cfg := SupervisorConfig{NoProgressAfter: 10 * time.Hour, WallClockCap: 100 * time.Hour, RepetitionEvents: 2, NoWriteAfter: time.Hour}
	s, _ := NewSupervisor(cfg, minute(0), minute(0))
	s.Event(toolEvent("read", map[string]any{"filePath": "a"}), minute(1))
	s.Event(toolEvent("read", map[string]any{"filePath": "b"}), minute(1))
	s.Messaged(minute(1))
	s.Event(fileEvent("a.go", "h1"), minute(2))
	s.Event(fileEvent("a.go", "h2"), minute(3))
	s.Event(cmdEvent(check, 1, "FAIL: TestLinks"), minute(3))
	if trip := s.Event(fileEvent("a.go", "h1"), minute(4)); trip != nil {
		t.Fatalf("the return itself tripped: %+v", trip)
	}
	trip := s.Event(cmdEvent(check, 1, "FAIL: TestLinks"), minute(5))
	if trip == nil || trip.Kind != TripFlipFlop || trip.decided == nil || trip.decided.Action != ActionRetryStronger {
		t.Fatalf("trip = %+v, want flip_flop decided retry_stronger", trip)
	}
	// without a message the supervisor is asked
	s2, _ := NewSupervisor(cfg, minute(0), minute(0))
	s2.Event(fileEvent("a.go", "h1"), minute(2))
	s2.Event(fileEvent("a.go", "h2"), minute(3))
	s2.Event(cmdEvent(check, 1, "FAIL: TestLinks"), minute(3))
	s2.Event(fileEvent("a.go", "h1"), minute(4))
	if trip := s2.Event(cmdEvent(check, 1, "FAIL: TestLinks"), minute(5)); trip == nil || trip.decided != nil {
		t.Fatalf("trip = %+v, want flip_flop for the supervisor", trip)
	}
}

// Roadmap 184: the return alone is not a circle. After a message, an edit
// undone — the file back at a content it had before, the check that was
// failing when it returned never failing again — leaves the card where it
// is and the session runs on.
func TestUndoAfterAMessageLeavesTheCardWhereItIs(t *testing.T) {
	const check = "go test ./..."
	cfg := SupervisorConfig{NoProgressAfter: 10 * time.Hour, WallClockCap: 100 * time.Hour, RepetitionEvents: 2, NoWriteAfter: time.Hour, Churn: 1000}
	s, _ := NewSupervisor(cfg, minute(0), minute(0))
	s.Event(toolEvent("read", map[string]any{"filePath": "a"}), minute(1))
	s.Event(toolEvent("read", map[string]any{"filePath": "b"}), minute(1))
	s.Messaged(minute(2))
	s.Event(fileEvent("main.go", "v1"), minute(3))
	s.Event(fileEvent("main.go", "v2"), minute(4))
	s.Event(cmdEvent(check, 1, "FAIL: TestMain"), minute(5))
	// main.go returns to v1 with the check failing: the return is pending
	// evidence, but no refail ever comes — the undo holds
	if trip := s.Event(fileEvent("main.go", "v1"), minute(6)); trip != nil {
		t.Fatalf("the return itself tripped: %+v", trip)
	}
	s.Event(fileEvent("other.go", "n1"), minute(7))
	if trip := s.Event(cmdEvent("gofmt -w other.go", 0, ""), minute(8)); trip != nil {
		t.Fatalf("work after the undo tripped: %+v", trip)
	}
	if trip := s.Tick(minute(30)); trip != nil {
		t.Fatalf("the session cannot run on after an undo: %+v", trip)
	}
}

// Roadmap 184: the circle's other half. The same return as the undo
// above, but now the check that was failing when the file returned fails
// again after it, unchanged: the flip-flop trips, and the stuck rule —
// a message was delivered — moves the card to the frontier tier
// (retry_stronger), not to the supervisor.
func TestARefailAfterAReturnMovesTheCardToTheFrontierTier(t *testing.T) {
	const check = "go test ./..."
	cfg := SupervisorConfig{NoProgressAfter: 10 * time.Hour, WallClockCap: 100 * time.Hour, RepetitionEvents: 2, NoWriteAfter: time.Hour, Churn: 1000}
	s, _ := NewSupervisor(cfg, minute(0), minute(0))
	s.Event(toolEvent("read", map[string]any{"filePath": "a"}), minute(1))
	s.Event(toolEvent("read", map[string]any{"filePath": "b"}), minute(1))
	s.Messaged(minute(2))
	s.Event(fileEvent("main.go", "v1"), minute(3))
	s.Event(fileEvent("main.go", "v2"), minute(4))
	s.Event(cmdEvent(check, 1, "FAIL: TestMain"), minute(5))
	if trip := s.Event(fileEvent("main.go", "v1"), minute(6)); trip != nil {
		t.Fatalf("the return itself tripped: %+v", trip)
	}
	trip := s.Event(cmdEvent(check, 1, "FAIL: TestMain"), minute(7))
	if trip == nil || trip.Kind != TripFlipFlop || trip.decided == nil || trip.decided.Action != ActionRetryStronger {
		t.Fatalf("trip = %+v decided=%+v, want flip_flop decided retry_stronger (the frontier tier)", trip, trip.decided)
	}
	if !strings.Contains(trip.Detail, "main.go") || !strings.Contains(trip.Detail, check) {
		t.Fatalf("detail = %q, want the path and the command", trip.Detail)
	}
}

// Roadmap 184: a check that failed not before the return but only after
// it is new work, not the circle's second half: the refail finds nothing
// pending and the card does not move.
func TestAFailureOnlyAfterAReturnIsNewWork(t *testing.T) {
	const check = "go vet ./..."
	cfg := SupervisorConfig{NoProgressAfter: 10 * time.Hour, WallClockCap: 100 * time.Hour, RepetitionEvents: 2, NoWriteAfter: time.Hour, Churn: 1000}
	s, _ := NewSupervisor(cfg, minute(0), minute(0))
	s.Event(toolEvent("read", map[string]any{"filePath": "a"}), minute(1))
	s.Event(toolEvent("read", map[string]any{"filePath": "b"}), minute(1))
	s.Messaged(minute(2))
	s.Event(fileEvent("main.go", "v1"), minute(3))
	s.Event(fileEvent("main.go", "v2"), minute(4))
	s.Event(cmdEvent(check, 0, "ok"), minute(5)) // nothing fails yet
	if trip := s.Event(fileEvent("main.go", "v1"), minute(6)); trip != nil {
		t.Fatalf("the return itself tripped: %+v", trip)
	}
	if trip := s.Event(cmdEvent(check, 1, "FAIL: TestVet"), minute(7)); trip != nil {
		t.Fatalf("the first failure tripped: %+v", trip)
	}
	s.Event(fileEvent("other.go", "n1"), minute(8))
	if trip := s.Event(cmdEvent(check, 1, "FAIL: TestVet"), minute(9)); trip != nil {
		t.Fatalf("refail of a check that was passing at the return = %+v decided=%+v, want no circle", trip, trip.decided)
	}
}

// No check-in in a session's first 10 minutes: anchor t0, 45m, a session
// from t0+40m skips t0+45m and checks in at t0+90m.
func TestNoCheckInInTheSessionsFirstTenMinutes(t *testing.T) {
	cfg := SupervisorConfig{NoProgressAfter: 10 * time.Hour, WallClockCap: 4 * time.Hour, RepetitionEvents: 1000,
		NoWriteAfter: 100 * time.Hour, CheckIn: 45 * time.Minute}
	s, _ := NewSupervisor(cfg, minute(0), minute(40))
	if trip := s.Tick(minute(89.99)); trip != nil {
		t.Fatalf("tripped at 89.99: %+v", trip)
	}
	if trip := s.Tick(minute(90)); trip == nil || trip.Kind != TripCheckIn {
		t.Fatalf("trip = %+v, want the check-in at t0+90m", trip)
	}
	// a point exactly 10 minutes after the start is kept
	s2, _ := NewSupervisor(cfg, minute(0), minute(35))
	if trip := s2.Tick(minute(45)); trip == nil || trip.Kind != TripCheckIn {
		t.Fatalf("trip = %+v, want the check-in at t0+45m", trip)
	}
}

// Ruling 224: a circle of writes is not progress. A message must not clear
// the alternation memory, so an agent that flips a file back and forth
// across messages and refails its check trips flip_flop and the stuck rule
// ends the session (round 2's drill 4: 38 minutes of message loops on one
// alternating file).
func TestSupervisorFlipFlopSpansTheMessage(t *testing.T) {
	const check = "go test ./..."
	cfg := SupervisorConfig{NoProgressAfter: 10 * time.Hour, WallClockCap: 100 * time.Hour, RepetitionEvents: 2,
		NoWriteAfter: 100 * time.Hour, CheckIn: 45 * time.Minute, Churn: 1000}
	s, _ := NewSupervisor(cfg, minute(0), minute(0))
	s.Event(fileEvent("stats_test.go", "versionA"), minute(1))
	s.Event(toolEvent("read", map[string]any{"filePath": "a"}), minute(1))
	if trip := s.Event(toolEvent("read", map[string]any{"filePath": "b"}), minute(1)); trip == nil {
		t.Fatal("no repetition trip before the message")
	}
	s.Messaged(minute(2))
	s.Event(fileEvent("stats_test.go", "versionB"), minute(3))
	s.Event(cmdEvent(check, 1, "FAIL: TestStats"), minute(3))
	if trip := s.Event(fileEvent("stats_test.go", "versionA"), minute(4)); trip != nil {
		t.Fatalf("the return itself tripped: %+v", trip)
	}
	trip := s.Event(cmdEvent(check, 1, "FAIL: TestStats"), minute(5))
	if trip == nil || trip.Kind != TripFlipFlop || trip.decided == nil || trip.decided.Action != ActionRetryStronger {
		t.Fatalf("flip back across the message = %+v, want flip_flop decided retry_stronger", trip)
	}
	if last := trip.Findings[len(trip.Findings)-1]; last != "stuck after the supervisor's message: flip_flop: after stats_test.go returned to a content it had before, `"+check+"` failed again, unchanged" {
		t.Fatalf("finding = %q", last)
	}
}

// The start rung's first attempt has the cheap cap to push: at the cap
// without a push the supervisor trips cheap_cap with the climb decided, no
// model call; a push before it, or the climb in place, ends the cap.
func TestCheapCapTripsAtTheCapWithoutAPush(t *testing.T) {
	cfg := SupervisorConfig{NoProgressAfter: 10 * time.Hour, WallClockCap: 100 * time.Hour, RepetitionEvents: 1000, CheapCap: 8 * time.Minute, NoWriteAfter: 10 * time.Hour}
	s, err := NewSupervisor(cfg, time.Unix(0, 0), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if trip := s.Tick(minute(7.9)); trip != nil {
		t.Fatalf("tripped before the cap: %+v", trip)
	}
	trip := s.Tick(minute(8))
	if trip == nil || trip.Kind != TripCheapCap || trip.decided == nil || trip.decided.Action != ActionRetryStronger {
		t.Fatalf("trip = %+v, want cheap_cap with the climb decided", trip)
	}

	s, _ = NewSupervisor(cfg, time.Unix(0, 0), time.Unix(0, 0))
	s.Event(harness.Event{Kind: harness.EventPushed, Detail: map[string]any{"commit": "c1"}}, minute(3))
	if trip := s.Tick(minute(9)); trip != nil {
		t.Fatalf("tripped after a push: %+v", trip)
	}

	s, _ = NewSupervisor(cfg, time.Unix(0, 0), time.Unix(0, 0))
	s.Climbed()
	if trip := s.Event(harness.Event{Kind: harness.EventModelCall}, minute(9)); trip != nil {
		t.Fatalf("tripped after the climb: %+v", trip)
	}

	s, _ = NewSupervisor(SupervisorConfig{NoProgressAfter: 10 * time.Hour, WallClockCap: 100 * time.Hour, RepetitionEvents: 1000}, time.Unix(0, 0), time.Unix(0, 0))
	if trip := s.Tick(minute(30)); trip != nil {
		t.Fatalf("a session without the cap tripped: %+v", trip)
	}
}

func spendSupervisor(t *testing.T, mutate func(*SupervisorConfig)) *Supervisor {
	t.Helper()
	cfg := SupervisorConfig{NoProgressAfter: 5 * time.Minute, WallClockCap: time.Hour, RepetitionEvents: 100,
		NoWriteAfter: 100 * time.Hour, NoPushSpend: 0.6}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := NewSupervisor(cfg, time.Unix(0, 0), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// 60 percent of 5 EUR is 3.00: 2.90 is under the line, 3.10 over it.
func TestSpendWithoutPushTripsOncePastTheShare(t *testing.T) {
	s := spendSupervisor(t, nil)
	if trip := s.Spent(2.9, 5); trip != nil {
		t.Fatalf("tripped at 58 percent: %+v", trip)
	}
	trip := s.Spent(3.1, 5)
	if trip == nil || trip.Kind != TripSpendNoPush || trip.Detail != "3.10 of the card's 5.00 EUR are spent (62 percent, the line is 60) and no work is pushed" {
		t.Fatalf("trip = %+v", trip)
	}
	s.Resume(time.Unix(10, 0))
	if again := s.Spent(4.9, 5); again != nil {
		t.Fatalf("tripped twice in a session: %+v", again)
	}
}

func TestSpendWithoutPushIsOffWhereItDoesNotApply(t *testing.T) {
	for name, mutate := range map[string]func(*SupervisorConfig){
		"unset":            func(c *SupervisorConfig) { c.NoPushSpend = 0 },
		"a report session": func(c *SupervisorConfig) { c.ReadOnly = true },
		"work pushed":      func(c *SupervisorConfig) { c.PriorPush = true },
	} {
		if trip := spendSupervisor(t, mutate).Spent(4.9, 5); trip != nil {
			t.Errorf("%s: tripped: %+v", name, trip)
		}
	}
	if trip := spendSupervisor(t, nil).Spent(1, 0); trip != nil {
		t.Errorf("a zero budget tripped: %+v", trip)
	}
	s := spendSupervisor(t, nil)
	s.Event(harness.Event{Kind: harness.EventPushed}, time.Unix(1, 0))
	if trip := s.Spent(4.9, 5); trip != nil {
		t.Errorf("a pushed session tripped: %+v", trip)
	}
}

// A spend trip after the supervisor's message is no evidence of being stuck.
func TestSpendTripAfterAMessageIsNotTheStuckRule(t *testing.T) {
	s := spendSupervisor(t, nil)
	if trip := s.Tick(time.Unix(400, 0)); trip == nil || trip.Kind != TripNoProgress {
		t.Fatalf("setup trip = %+v, want no_progress", trip)
	}
	s.Messaged(time.Unix(401, 0))
	trip := s.Spent(3.5, 5)
	if trip == nil || trip.decided != nil {
		t.Fatalf("trip = %+v, want the supervisor asked, not the stuck climb", trip)
	}
}

func TestSpendNoPushShareMustBeBelowOne(t *testing.T) {
	cfg := SupervisorConfig{NoProgressAfter: time.Minute, WallClockCap: time.Hour, RepetitionEvents: 10, NoPushSpend: 1}
	if _, err := NewSupervisor(cfg, time.Unix(0, 0), time.Unix(0, 0)); err == nil {
		t.Fatal("a share of 1 accepted")
	}
}

func TestReportSessionRemembersWhatItRead(t *testing.T) {
	s := spendSupervisor(t, func(c *SupervisorConfig) { c.ReadOnly = true })
	for _, p := range []string{"/w/a.go", "/w/b.go", "/w/a.go"} {
		s.Event(readEvent(p), time.Unix(1, 0))
	}
	s.Event(harness.Event{Kind: harness.EventToolCall, Detail: map[string]any{"tool": "grep", "args": map[string]any{"pattern": "x"}}}, time.Unix(2, 0))
	s.Event(harness.Event{Kind: harness.EventToolCall, Detail: map[string]any{"tool": "read", "status": "error", "args": map[string]any{"filePath": "/w/gone.go"}}}, time.Unix(3, 0))
	if got := s.Reads(); !slices.Equal(got, []string{"/w/a.go", "/w/b.go"}) {
		t.Fatalf("Reads = %v, want the two distinct paths in order", got)
	}
}

func TestCodingSessionKeepsNoReads(t *testing.T) {
	s := spendSupervisor(t, nil)
	s.Event(readEvent("/w/a.go"), time.Unix(1, 0))
	if got := s.Reads(); len(got) != 0 {
		t.Fatalf("Reads = %v, want none for a coding session", got)
	}
}
