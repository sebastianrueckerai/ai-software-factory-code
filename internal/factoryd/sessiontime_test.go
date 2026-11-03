package factoryd

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A session's time by what ran, as its transcript reads back: bash calls
// by the project's classes, a failed bash call by its arguments' command,
// the other tools by name. A subagent's calls sit inside the parent's task
// call, which counts them once. A call's start adds nothing: its end holds
// the duration.
func TestSessionTimeOf(t *testing.T) {
	ms := func(n float64) float64 { return n }
	events := []harness.Event{
		{Kind: harness.EventToolRunning, Detail: map[string]any{"call_id": "p1", "tool": "bash", "started": ms(1)}},
		{Kind: harness.EventCommand, Detail: map[string]any{"call_id": "p1", "command": "bash check 2>&1 | tail -25", "exit": ms(0), "duration_ms": ms(360000)}},
		{Kind: harness.EventCommand, Detail: map[string]any{"command": "cd /workspace/repo && bash check changed", "exit": ms(1), "duration_ms": ms(90000)}},
		{Kind: harness.EventToolCall, Detail: map[string]any{"tool": "bash", "status": "error", "args": map[string]any{"command": "bash check"}, "duration_ms": ms(600000)}},
		{Kind: harness.EventCommand, Detail: map[string]any{"command": "git status", "exit": ms(0), "duration_ms": ms(200)}},
		{Kind: harness.EventFileChange, Detail: map[string]any{"tool": "edit", "path": "a.go", "duration_ms": ms(40)}},
		{Kind: harness.EventToolCall, Detail: map[string]any{"tool": "read", "status": "completed", "duration_ms": ms(10)}},
		{Kind: harness.EventToolCall, Detail: map[string]any{"tool": "task", "status": "completed", "duration_ms": ms(120000)}},
		{Kind: harness.EventCommand, Detail: map[string]any{"command": "bash check", "exit": ms(0), "duration_ms": ms(50000), "subagent": true}},
		{Kind: harness.EventModelCall, Detail: map[string]any{"reason": "stop"}},
	}
	started := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	f := SessionTimeOf(events, policy.ProjectConfig{}.CommandClassOf())
	f.Session, f.Attempt, f.Started = "s-1", 2, started
	ev := f.Event("t-1", "sf/code", 7, started.Add(30*time.Minute))
	want := map[string]any{
		"session": "s-1", "attempt": 2, "wall_ms": int64(1800000), "commands_ms": int64(1050200),
		"classes": map[string]any{
			"check":         map[string]any{"ms": int64(960000), "count": 2, "runs": []int64{360000, 600000}},
			"check_changed": map[string]any{"ms": int64(90000), "count": 1, "runs": []int64{90000}},
			"other":         map[string]any{"ms": int64(200), "count": 1},
		},
		"tools":   map[string]any{"edit": int64(40), "read": int64(10), "task": int64(120000)},
		"longest": map[string]any{"class": "check", "ms": int64(600000), "command": "bash check"},
	}
	if !reflect.DeepEqual(ev.Detail, want) {
		t.Fatalf("detail =\n%#v\nwant\n%#v", ev.Detail, want)
	}
	if ev.Kind != EventSessionTime || ev.Repository != "sf/code" || ev.TaskID != "t-1" || ev.IssueNumber != 7 ||
		!ev.At.Equal(started.Add(30*time.Minute)) || ev.IdentityKey != identityKey(EventSessionTime, "s-1", "2") {
		t.Fatalf("row = %+v, want the session's time on t-1's card, one per session and attempt", ev)
	}
}

// A session that ran no command has a wall time and nothing else, and the
// longest command of a long text is its first 120 bytes.
func TestSessionTimeOfQuietAndLongCommands(t *testing.T) {
	started := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	quiet := SessionTimeOf([]harness.Event{{Kind: harness.EventModelCall}}, policy.ProjectConfig{}.CommandClassOf())
	quiet.Started = started
	if d := quiet.Event("t", "r", 1, started.Add(time.Minute)).Detail; d["commands_ms"] != int64(0) ||
		!reflect.DeepEqual(d["classes"], map[string]any{}) || !reflect.DeepEqual(d["tools"], map[string]any{}) || d["longest"] != nil {
		t.Fatalf("a quiet session's detail = %#v", d)
	}
	long := "for i in $(seq 1 14); do printf \"%s: \" \"$(date +%H:%M:%S)\"; go list -test ./acceptance/ >/dev/null; done; echo end of a very long loop"
	f := SessionTimeOf([]harness.Event{{Kind: harness.EventCommand, Detail: map[string]any{"command": long, "duration_ms": float64(5)}}},
		policy.ProjectConfig{}.CommandClassOf())
	if got := f.Event("t", "r", 1, started).Detail["longest"]; !reflect.DeepEqual(got, map[string]any{"class": "other", "ms": int64(5), "command": long[:120] + "…"}) {
		t.Fatalf("longest = %#v", got)
	}
}

// A session's end writes its time row beside its end row, from the
// transcript the session recorded: the check run by its class, the read
// by its tool, the wall from the session's start to its end.
func TestSessionEndWritesTheSessionsTime(t *testing.T) {
	runner, _, store := newTestRunner(t, Task{ID: "t6", Status: "running", Objective: "check",
		Repository: "sf/repo", Track: "direct"}, newFakeKube(), &fakeObjectStore{})
	logs := envLine(harness.Event{Time: time.Now(), Kind: harness.EventCommand, Detail: map[string]any{
		"command": "bash check", "exit": 0, "duration_ms": 360000}}) + "\n" +
		envLine(harness.Event{Time: time.Now(), Kind: harness.EventToolCall, Detail: map[string]any{
			"tool": "read", "status": "completed", "duration_ms": 25}}) + "\n" +
		pushedLine("c1") + "\n" + resultLine(true, "pushed")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t6", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed {
		t.Fatalf("session = (%+v, %v), want pushed", res, err)
	}
	rows := eventsOfKind(store, EventSessionTime)
	if len(rows) != 1 {
		t.Fatalf("session_time rows = %d, want one", len(rows))
	}
	d := rows[0].Detail
	end := eventsOfKind(store, EventSessionEnd)[0]
	if d["session"] != res.SessionID || d["attempt"] != 1 || d["commands_ms"] != int64(360000) ||
		!reflect.DeepEqual(d["classes"], map[string]any{"check": map[string]any{"ms": int64(360000), "count": 1, "runs": []int64{360000}}}) ||
		!reflect.DeepEqual(d["tools"], map[string]any{"read": int64(25)}) || !rows[0].At.Equal(end.At) {
		t.Fatalf("row = %+v, want the session's check and read, at its end's time %s", rows[0], end.At)
	}
	if rows[0].IdentityKey != identityKey(EventSessionTime, res.SessionID, "1") {
		t.Fatalf("identity = %q", rows[0].IdentityKey)
	}
}

// timeRow records one session_time row of repository at at: a wall of
// wall, the commands and their classes and tools as given, in ms.
func timeRow(t *testing.T, s *PGStore, repository, session string, at time.Time, wall time.Duration, classes map[string]*ClassTime, tools map[string]time.Duration, longest *LongestCommand) {
	t.Helper()
	f := SessionTimeFact{Session: session, Attempt: 1, Started: at.Add(-wall), Classes: classes, Tools: tools, Longest: longest}
	for _, c := range classes {
		f.Commands += c.Total
	}
	dcRecord(t, s, f.Event("t-"+session, repository, 9, at), at)
}

// The session-time panel over the window: the check share (check and
// check_changed over the wall) and the command share against the window
// before and by week, the hours by class and by tool, the full check's
// median run, and the longest command; another repository's rows count
// for none of it.
func TestSessionTimeFigures(t *testing.T) {
	s := metricsStore(t)
	from, to := metricsTestWindow()
	const repo = "sf/sessiontime"
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	run := func(total int, runs ...int) *ClassTime {
		c := &ClassTime{Total: ms(total), Count: len(runs)}
		for _, r := range runs {
			c.Runs = append(c.Runs, ms(r))
		}
		return c
	}
	other := func(total, count int) *ClassTime { return &ClassTime{Total: ms(total), Count: count} }
	oct1, oct5, sep20 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC), time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	timeRow(t, s, repo, "s1", oct1, time.Hour,
		map[string]*ClassTime{"check": run(1200000, 600000, 600000), "other": other(600000, 3)},
		map[string]time.Duration{"read": ms(36000)}, &LongestCommand{Class: "check", Took: ms(600000), Command: "bash check"})
	timeRow(t, s, repo, "s2", oct5, 30*time.Minute,
		map[string]*ClassTime{"check_changed": run(300000, 300000), "check": run(300000, 300000), "other": other(300000, 2)},
		map[string]time.Duration{"read": ms(18000), "edit": ms(9000)}, &LongestCommand{Class: "check", Took: ms(300000), Command: "bash check"})
	timeRow(t, s, repo, "s0", sep20, time.Hour, map[string]*ClassTime{"check": run(360000, 360000)}, nil, nil)
	timeRow(t, s, "sf/elsewhere", "s9", oct1, time.Hour, map[string]*ClassTime{"check": run(3600000, 3600000)}, nil, nil)

	r, err := s.ProjectMetrics(context.Background(), repo, from, to)
	if err != nil {
		t.Fatal(err)
	}
	got := r.SessionTime
	var checkWeeks, commandWeeks [7]float64
	checkWeeks[3], checkWeeks[5], checkWeeks[6] = 360000.0/3600000, 1200000.0/3600000, 600000.0/1800000
	commandWeeks[3], commandWeeks[5], commandWeeks[6] = 360000.0/3600000, 1800000.0/3600000, 900000.0/1800000
	if want := (Figure{Value: 1800000.0 / 5400000, Prev: 0.1, Has: true, PrevHas: true, Weeks: checkWeeks}); got.CheckShare != want {
		t.Errorf("check share = %+v, want %+v", got.CheckShare, want)
	}
	if want := (Figure{Value: 0.5, Prev: 0.1, Has: true, PrevHas: true, Weeks: commandWeeks}); got.CommandShare != want {
		t.Errorf("command share = %+v, want %+v", got.CommandShare, want)
	}
	if got.Sessions != 2 || got.WallHours != 1.5 || got.CheckRuns != 3 || got.CheckMedianS != 600 {
		t.Errorf("sessions %d, wall %v h, check runs %d, median %v s; want 2, 1.5, 3, 600", got.Sessions, got.WallHours, got.CheckRuns, got.CheckMedianS)
	}
	if want := map[string]float64{"check": 1500000.0 / 3600000, "check_changed": 300000.0 / 3600000, "other": 900000.0 / 3600000}; !reflect.DeepEqual(got.ClassHours, want) {
		t.Errorf("class hours = %v, want %v", got.ClassHours, want)
	}
	if want := map[string]float64{"read": 54000.0 / 3600000, "edit": 9000.0 / 3600000}; !reflect.DeepEqual(got.ToolHours, want) {
		t.Errorf("tool hours = %v, want %v", got.ToolHours, want)
	}
	if want := (LongestRun{Class: "check", Seconds: 600, Command: "bash check"}); got.Longest != want {
		t.Errorf("longest = %+v, want %+v", got.Longest, want)
	}

	empty, err := s.ProjectMetrics(context.Background(), "sf/none", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if e := empty.SessionTime; e.CheckShare.Has || e.CommandShare.Has || e.Sessions != 0 || len(e.ClassHours) != 0 || e.Longest != (LongestRun{}) {
		t.Errorf("a project without rows = %+v, want nothing", e)
	}
}
