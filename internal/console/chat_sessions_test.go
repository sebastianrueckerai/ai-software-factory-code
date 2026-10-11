package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// sessionsTimeline is t-r's timeline with two sessions: s1 on
// coding-cheap, nudged, tripped and messaged, which ended without a push,
// and s2 on coding-frontier, which pushed abc123.
const sessionsTimeline = `{"step":"session_start","time":"2026-10-11T10:00:00Z","session":"s1","parent":"","model":"coding-cheap","attempt":1,"start_ref":"0a1b2c3"}
{"step":"nudge","time":"2026-10-11T10:04:00Z","session":"s1","tool":"read","count":3}
{"step":"supervisor","time":"2026-10-11T10:05:00Z","session":"s1","detector":"repetition","trip":"read a.go 3 times","digest":"","decision":"guide","guidance":"change approach","reason":"","called":true}
{"step":"message","time":"2026-10-11T10:05:01Z","session":"s1"}
{"step":"message","time":"2026-10-11T10:06:00Z","session":"s1","from":"sebastian"}
{"step":"session_end","time":"2026-10-11T10:12:30Z","session":"s1","parent":"","model":"coding-cheap","attempt":1,"outcome":"no_push","summary":"stopped without a push","started":"2026-10-11T10:00:00Z"}
{"step":"session_start","time":"2026-10-11T10:13:00Z","session":"s2","parent":"s1","model":"coding-frontier","attempt":2,"start_ref":"0a1b2c3"}
{"step":"session_end","time":"2026-10-11T10:40:00Z","session":"s2","parent":"s1","model":"coding-frontier","attempt":2,"outcome":"pushed","summary":"added the p95","started":"2026-10-11T10:13:00Z","commit":"abc123"}
`

// s1Transcript and s2Transcript are the two sessions' transcripts as the
// session recorder writes them.
const (
	s1Transcript = `{"time":"2026-10-11T10:00:05Z","kind":"model_call","detail":{"reason":"tool-calls","tokens":{"total":1500,"input":1400,"output":100,"reasoning":0},"cost":0.01,"text":"reading"}}
{"time":"2026-10-11T10:00:10Z","kind":"tool_call","detail":{"tool":"read","status":"completed","args":{"path":"a.go"},"args_sha256":"aa","duration_ms":120}}
{"time":"2026-10-11T10:01:00Z","kind":"tool_call","detail":{"tool":"edit","status":"completed","args":{"path":"a.go"},"args_sha256":"bb","duration_ms":40}}
{"time":"2026-10-11T10:01:01Z","kind":"file_change","detail":{"path":"a.go","sha256":"cc","tool":"edit","bytes":300}}
{"time":"2026-10-11T10:02:00Z","kind":"command","detail":{"command":"go test ./...","status":"completed","exit":1,"output":"FAIL: want 3","duration_ms":2300,"bytes":12}}
{"time":"2026-10-11T10:03:00Z","kind":"checkpoint","detail":{"diff":{"files":1,"insertions":10,"deletions":2},"patch":"diff --git a/a.go b/a.go"}}
`
	s2Transcript = `{"time":"2026-10-11T10:13:30Z","kind":"model_call","detail":{"reason":"tool-calls","tokens":{"total":4200,"input":4000,"output":200,"reasoning":0},"cost":0.025,"text":"fixing the mean to the p95"}}
{"time":"2026-10-11T10:20:00Z","kind":"file_change","detail":{"path":"stats.go","sha256":"dd","tool":"write","bytes":900}}
{"time":"2026-10-11T10:30:00Z","kind":"command","detail":{"command":"go test ./...","status":"completed","exit":0,"output":"ok","duration_ms":800,"bytes":2}}
{"time":"2026-10-11T10:39:00Z","kind":"checkpoint","detail":{"diff":{"files":2,"insertions":40,"deletions":8},"patch":"diff --git a/stats.go b/stats.go"}}
`
)

// newSessionsRig is the chat rig with t-r #11's two sessions and their
// transcripts.
func newSessionsRig(t *testing.T) (*rig, *chatGateway) {
	r, _, g := newChatRig(t)
	r.objects["sessions/t-r/timeline.jsonl"] = []byte(sessionsTimeline)
	r.objects["sessions/t-r/s1.jsonl"] = []byte(s1Transcript)
	r.objects["sessions/t-r/s2.jsonl"] = []byte(s2Transcript)
	return r, g
}

const (
	s1Summary = "session 1 of 2 (s1), attempt 1, model coding-cheap: no_push\nstarted 2026-10-11 10:00 UTC, took 12 min\n" +
		"summary: stopped without a push\nsupervisor and messages:\n  4:00 nudged after 3 identical read calls\n" +
		"  5:00 repetition tripped: change approach\n  5:01 the supervisor's message\n  6:00 message from sebastian\n"
	s1Steps = "1 0:05 model call, 1500 tokens, €0.0100\n" +
		`2 0:10 read 120 ms: {"path":"a.go"}` + "\n" +
		`3 1:00 edit 40 ms: {"path":"a.go"}` + "\n" +
		"4 1:01 file_change: a.go, 300 bytes\n" +
		"5 2:00 command 2.3 s: go test ./..., exit 1\n" +
		"6 3:00 checkpoint: 1 file, +10 -2\n"
)

// sessionsOf numbers a card's sessions by their starts in order, pairs
// each start with its end and takes a pushed session's commit from its
// pushed step; a start with no end is running.
func TestSessionsOf(t *testing.T) {
	var steps []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(`{"step":"session_start","time":"2026-10-11T10:00:00Z","session":"a","model":"coding-cheap","attempt":1}
{"step":"session_end","time":"2026-10-11T10:30:00.5Z","session":"a","model":"coding-cheap","attempt":1,"outcome":"pushed","summary":"done","started":"2026-10-11T10:00:00Z"}
{"step":"pushed","time":"2026-10-11T10:30:01Z","session":"a","branch":"factory/t-r","commit":"c0ffee1"}
{"step":"session_end","time":"2026-10-11T10:31:00Z","session":"z","outcome":"aborted"}
{"step":"session_start","time":"2026-10-11T11:00:00Z","session":"b","model":"coding-frontier","attempt":2}`), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, m)
	}
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	equal(t, "the sessions", sessionsOf(steps), []sessionRef{
		{N: 1, ID: "a", Model: "coding-cheap", Attempt: 1, Started: at("2026-10-11T10:00:00Z"), Ended: at("2026-10-11T10:30:00.5Z"),
			Outcome: "pushed", Summary: "done", Commit: "c0ffee1"},
		{N: 2, ID: "b", Model: "coding-frontier", Attempt: 2, Started: at("2026-10-11T11:00:00Z"), Outcome: "running"},
	})
}

// The session read is one session's summary: its number of the card's,
// model, attempt and outcome, its duration, the agent's closing summary,
// the supervisor's trips and the messages the session got, the files it
// changed, its diff stat, its spend and its step count; the newest when n
// is unset. A running session runs since its start; one without a
// transcript says so; a card without sessions and a session past the
// card's are refused.
func TestChatSessionRead(t *testing.T) {
	r, g := newSessionsRig(t)
	equal(t, "the session reads", reads(t, r, g, "session", `{"task_id":"t-r"}`, "session", `{"task_id":"t-r","n":1}`,
		"session", `{"task_id":"t-r","n":3}`, "session", `{"task_id":"t-m"}`), []string{
		"session 2 of 2 (s2), attempt 2, model coding-frontier: pushed, commit abc123\nstarted 2026-10-11 10:13 UTC, took 27 min\n" +
			"summary: added the p95\nfiles changed: stats.go (1)\ndiff: 2 files, +40 -8\nspend: €0.0250\n4 steps",
		s1Summary + "files changed: a.go (1)\ndiff: 1 file, +10 -2\nspend: €0.0100\n6 steps",
		"Refused: Card #11 has no session 3; it has had 2 sessions.",
		"Refused: Card #5 has had no session yet.",
	})
	status, _ := refused(t, r, "session", toolInput{TaskID: "t-r", N: 3})
	equal(t, "a session past the card's", status, http.StatusNotFound)
	status, _ = refused(t, r, "session", toolInput{TaskID: "t-m"})
	equal(t, "a card without sessions", status, http.StatusConflict)

	r.objects["sessions/t-r/timeline.jsonl"] = []byte(sessionsTimeline +
		`{"step":"session_start","time":"2026-10-11T10:41:00Z","session":"s3","parent":"s2","model":"coding-frontier","attempt":3}` + "\n")
	equal(t, "a running session without a transcript", reads(t, r, g, "session", `{"task_id":"t-r"}`, "session_search", `{"task_id":"t-r","query":"zzz"}`), []string{
		"session 3 of 3 (s3), attempt 3, model coding-frontier: running\nrunning since 2026-10-11 10:41 UTC\nno transcript for session 3",
		"no hit for \"zzz\"\nno transcript for session 3",
	})
	status, msg := refused(t, r, "session_steps", toolInput{TaskID: "t-r"})
	equal(t, "the steps of a session without a transcript", []any{status, msg}, []any{http.StatusNotFound, "no transcript for session 3"})
}

// The steps read lists a session's transcript one line a step: its
// number, the time since the session's start, its kind or tool, its
// duration and a one-line summary, no content; the first 100 unless from
// and to name others, to clamped to the end.
func TestChatSessionSteps(t *testing.T) {
	r, g := newSessionsRig(t)
	equal(t, "the steps reads", reads(t, r, g, "session_steps", `{"task_id":"t-r","n":1}`, "session_steps", `{"task_id":"t-r","n":1,"from":2,"to":3}`,
		"session_steps", `{"task_id":"t-r"}`, "session_steps", `{"task_id":"t-r","n":1,"from":6,"to":40}`), []string{
		s1Steps + "steps 1 to 6 of 6",
		`2 0:10 read 120 ms: {"path":"a.go"}` + "\n" + `3 1:00 edit 40 ms: {"path":"a.go"}` + "\nsteps 2 to 3 of 6",
		"1 0:30 model call, 4200 tokens, €0.0250\n2 7:00 file_change: stats.go, 900 bytes\n3 17:00 command 800 ms: go test ./..., exit 0\n" +
			"4 26:00 checkpoint: 2 files, +40 -8\nsteps 1 to 4 of 4",
		"6 3:00 checkpoint: 1 file, +10 -2\nsteps 6 to 6 of 6",
	})
	for _, c := range []struct {
		in       toolInput
		status   int
		sentence string
	}{
		{toolInput{TaskID: "t-r", N: 1, From: 7}, http.StatusBadRequest, "Session 1 has 6 steps."},
		{toolInput{TaskID: "t-r", N: 1, From: 3, To: 2}, http.StatusBadRequest, "to 2 is before from 3."},
		{toolInput{TaskID: "t-r", N: 1, From: -1}, http.StatusBadRequest, "from and to are step numbers, from 1."},
	} {
		status, msg := refused(t, r, "session_steps", c.in)
		equal(t, fmt.Sprintf("session_steps %+v", c.in), []any{status, msg}, []any{c.status, c.sentence})
	}

	var many strings.Builder
	for i := range 150 {
		fmt.Fprintf(&many, `{"time":"2026-10-11T10:%02d:00Z","kind":"finished"}`+"\n", i%60)
	}
	r.objects["sessions/t-r/s1.jsonl"] = []byte(many.String())
	got := reads(t, r, g, "session_steps", `{"task_id":"t-r","n":1}`, "session_steps", `{"task_id":"t-r","n":1,"from":120}`,
		"session_step", `{"task_id":"t-r","n":1,"from":1,"to":30}`)
	lines := strings.Split(got[0], "\n")
	equal(t, "the default range's lines", []string{lines[0], lines[99], lines[100]}, []string{"1 0:00 finished", "100 39:00 finished", "steps 1 to 100 of 150"})
	equal(t, "a range from 120's end", got[1][strings.LastIndex(got[1], "\n")+1:], "steps 120 to 150 of 150")
	equal(t, "30 steps in full", got[2][strings.LastIndex(got[2], "\n")+1:], "steps 1 to 20 of 150")
}

// The step read is steps of a session in full: a tool call's arguments
// and error, a model call's text, a command's output, a checkpoint's
// stat without its patch; to defaults to from, clamps to the end and to
// 20 steps; each step is cut at 8 KiB and the answer at 32 KiB.
func TestChatSessionStep(t *testing.T) {
	r, g := newSessionsRig(t)
	equal(t, "the step reads", reads(t, r, g, "session_step", `{"task_id":"t-r","n":1,"from":5}`, "session_step", `{"task_id":"t-r","n":1,"from":1,"to":2}`,
		"session_step", `{"task_id":"t-r","n":1,"from":5,"to":99}`, "session_step", `{"task_id":"t-r","n":1}`), []string{
		"5 2:00 command 2.3 s: go test ./..., exit 1\n$ go test ./...\nFAIL: want 3\n\nsteps 5 to 5 of 6",
		"1 0:05 model call, 1500 tokens, €0.0100\nreading\n\n" + `2 0:10 read 120 ms: {"path":"a.go"}` + "\n" + `args: {"path":"a.go"}` + "\n\nsteps 1 to 2 of 6",
		"5 2:00 command 2.3 s: go test ./..., exit 1\n$ go test ./...\nFAIL: want 3\n\n6 3:00 checkpoint: 1 file, +10 -2\n\nsteps 5 to 6 of 6",
		"Refused: from and to are step numbers, from 1.",
	})
	status, _ := refused(t, r, "session_step", toolInput{TaskID: "t-r", N: 1})
	equal(t, "from 0", status, http.StatusBadRequest)

	r.objects["sessions/t-r/s2.jsonl"] = []byte(`{"time":"2026-10-11T10:14:00Z","kind":"tool_call","detail":{"tool":"bash","status":"error","args":{"command":"rm -rf /"},"error":"refused by the sandbox","duration_ms":3}}
{"time":"2026-10-11T10:15:00Z","kind":"supervisor","detail":{"detector":"stall","decision":"guide","guidance":"run the tests","reason":""}}
`)
	equal(t, "an error and a supervisor step", reads(t, r, g, "session_step", `{"task_id":"t-r","from":1,"to":2}`), []string{
		`1 1:00 bash 3 ms, error: {"command":"rm -rf /"}` + "\n" + `args: {"command":"rm -rf /"}` + "\nerror: refused by the sandbox\n\n" +
			"2 2:00 supervisor: stall\ndecision: guide\ndetector: stall\nguidance: run the tests\n\nsteps 1 to 2 of 2",
	})

	var big strings.Builder
	for range 30 {
		fmt.Fprintf(&big, `{"time":"2026-10-11T10:00:05Z","kind":"model_call","detail":{"tokens":{"total":1},"cost":0,"text":"%s"}}`+"\n", strings.Repeat("x", 10000))
	}
	r.objects["sessions/t-r/s1.jsonl"] = []byte(big.String())
	got := reads(t, r, g, "session_step", `{"task_id":"t-r","n":1,"from":1,"to":30}`)[0]
	// a block is "1 0:05 model call, 1 tokens, €0.0000\n" (39 bytes, € is
	// three) and the text: 10039 bytes, cut to 8192
	block := "1 0:05 model call, 1 tokens, €0.0000\n" + strings.Repeat("x", 8192-39) + "…\n(step cut at 8 KiB of 10039 bytes)"
	if !strings.HasPrefix(got, block+"\n\n2 0:05 model call") {
		t.Errorf("the first step reads\n%.300s", got)
	}
	const note = "…\n(answer cut; ask for a narrower range)"
	if !strings.HasSuffix(got, note) || len(got) != 32<<10+len(note) {
		t.Errorf("the answer is %d bytes ending %q, want %d ending %q", len(got), got[max(0, len(got)-60):], 32<<10+len(note), note)
	}
	if strings.Count(got, "model call") != 4 {
		t.Errorf("the answer holds %d steps, want 4 of the 20 asked (8 KiB each in 32 KiB)", strings.Count(got, "model call"))
	}
}

// The search reads the transcripts line by line for a text,
// case-insensitive, in the fields that hold text: each hit is its
// session, step and kind with the matching line and the line before and
// after it, of the same field or the neighbouring steps; 20 at most.
func TestChatSessionSearch(t *testing.T) {
	r, g := newSessionsRig(t)
	equal(t, "the searches", reads(t, r, g,
		"session_search", `{"task_id":"t-r","query":"p95"}`,
		"session_search", `{"task_id":"t-r","query":"a.go"}`,
		"session_search", `{"task_id":"t-r","query":"x"}`,
		"session_search", `{"task_id":"t-r","query":"nothing-here"}`,
		"session_search", `{"task_id":"t-r","query":"fail: WANT","n":1}`,
		"session_search", `{"task_id":"t-r","query":"a.go","n":2}`,
	), []string{
		"session 2 step 1 (model_call): fixing the mean to the p95\n  after: 2 7:00 file_change: stats.go, 900 bytes",
		`session 1 step 2 (tool_call): {"path":"a.go"}` + "\n  before: 1 0:05 model call, 1500 tokens, €0.0100\n" + `  after: 3 1:00 edit 40 ms: {"path":"a.go"}` + "\n" +
			`session 1 step 3 (tool_call): {"path":"a.go"}` + "\n" + `  before: 2 0:10 read 120 ms: {"path":"a.go"}` + "\n  after: 4 1:01 file_change: a.go, 300 bytes\n" +
			"session 1 step 4 (file_change): a.go\n" + `  before: 3 1:00 edit 40 ms: {"path":"a.go"}` + "\n  after: 5 2:00 command 2.3 s: go test ./..., exit 1",
		"Refused: The query is 2 to 200 characters.",
		`no hit for "nothing-here"`,
		"session 1 step 5 (command): FAIL: want 3\n  before: 4 1:01 file_change: a.go, 300 bytes\n  after: 6 3:00 checkpoint: 1 file, +10 -2",
		`no hit for "a.go"`,
	})
	for _, q := range []string{"x", strings.Repeat("é", 201), ""} {
		status, _ := refused(t, r, "session_search", toolInput{TaskID: "t-r", Query: q})
		equal(t, fmt.Sprintf("the query of %d characters", len([]rune(q))), status, http.StatusBadRequest)
	}

	lines := make([]string, 25)
	for i := range lines {
		lines[i] = fmt.Sprintf("Needle %d", i+1)
	}
	text, _ := json.Marshal(strings.Join(lines, "\n"))
	long := strings.Repeat("a", 500) + "NEEDLE" + strings.Repeat("b", 500)
	r.objects["sessions/t-r/s1.jsonl"] = []byte(`{"time":"2026-10-11T10:00:05Z","kind":"model_call","detail":{"tokens":{"total":1},"cost":0,"text":` + string(text) + "}}\n" +
		`{"time":"2026-10-11T10:00:06Z","kind":"command","detail":{"command":"echo","output":"` + long + `"}}` + "\n" +
		`{"time":"2026-10-11T10:00:07Z","kind":"command","detail":{"command":"make","output":"first\nlast line\n"}}` + "\n")
	var want []string
	for i := 1; i <= 20; i++ {
		hit := fmt.Sprintf("session 1 step 1 (model_call): Needle %d", i)
		if i > 1 {
			hit += fmt.Sprintf("\n  before: Needle %d", i-1)
		}
		want = append(want, hit+fmt.Sprintf("\n  after: Needle %d", i+1))
	}
	equal(t, "a search of more than 20 hits", reads(t, r, g, "session_search", `{"task_id":"t-r","query":"needle","n":1}`),
		[]string{strings.Join(want, "\n") + "\n... and 6 more"})
	equal(t, "a miss and a hit on a field's last line", reads(t, r, g, "session_search", `{"task_id":"t-r","query":"bneedle","n":1}`, "session_search", `{"task_id":"t-r","query":"needle 25","n":1}`,
		"session_search", `{"task_id":"t-r","query":"last line","n":1}`), []string{
		"no hit for \"bneedle\"",
		"session 1 step 1 (model_call): Needle 25\n  before: Needle 24",
		"session 1 step 3 (command): last line\n  before: first",
	})
	equal(t, "a hit in a long line", reads(t, r, g, "session_search", `{"task_id":"t-r","query":"aneedleb","n":1}`), []string{
		"session 1 step 2 (command): …" + strings.Repeat("a", 101) + "NEEDLE" + strings.Repeat("b", 101) + "…\n  before: 1 0:05 model call, 1 tokens, €0.0000" +
			"\n  after: 3 0:07 command: make",
	})
}

// A transcript over 8 MiB is not parsed: the session read and the steps
// say so, and the search still reads it line by line.
func TestChatSessionTooLarge(t *testing.T) {
	r, g := newSessionsRig(t)
	filler := `{"time":"2026-10-11T10:01:00Z","kind":"command","detail":{"command":"echo","exit":0,"output":"` + strings.Repeat("x", 4000) + `"}}` + "\n"
	var big strings.Builder
	for i := 1; big.Len() < 9<<20; i++ {
		if i == 1000 {
			big.WriteString(`{"time":"2026-10-11T10:01:00Z","kind":"command","detail":{"command":"grep needle-in-the-stack","exit":0}}` + "\n")
			continue
		}
		big.WriteString(filler)
	}
	r.objects["sessions/t-r/s1.jsonl"] = []byte(big.String())
	const tooLarge = "session 1's transcript is 9.0 MiB; too large to read"
	equal(t, "the reads of a large transcript", reads(t, r, g, "session", `{"task_id":"t-r","n":1}`, "session_steps", `{"task_id":"t-r","n":1}`,
		"session_search", `{"task_id":"t-r","query":"Needle-In"}`), []string{
		s1Summary + tooLarge,
		"Refused: " + tooLarge,
		"session 1 step 1000 (command): grep needle-in-the-stack\n  before: 999 1:00 command: echo, exit 0\n  after: 1001 1:00 command: echo, exit 0",
	})
	status, _ := refused(t, r, "session_step", toolInput{TaskID: "t-r", N: 1, From: 1})
	equal(t, "a step of a large transcript", status, http.StatusConflict)
}

// A path the agent wrote is cut at 200 bytes in a step's line and in the
// session's files, which count it once.
func TestChatSessionLongPath(t *testing.T) {
	r, g := newSessionsRig(t)
	long := strings.Repeat("p", 300)
	r.objects["sessions/t-r/s2.jsonl"] = []byte(`{"time":"2026-10-11T10:14:00Z","kind":"file_change","detail":{"path":"` + long + `","bytes":10}}` + "\n" +
		`{"time":"2026-10-11T10:15:00Z","kind":"file_change","detail":{"path":"` + long + `","bytes":12}}` + "\n")
	cut := strings.Repeat("p", 200) + "…"
	equal(t, "the reads of a long path", reads(t, r, g, "session_steps", `{"task_id":"t-r"}`, "session", `{"task_id":"t-r"}`), []string{
		"1 1:00 file_change: " + cut + ", 10 bytes\n2 2:00 file_change: " + cut + ", 12 bytes\nsteps 1 to 2 of 2",
		"session 2 of 2 (s2), attempt 2, model coding-frontier: pushed, commit abc123\nstarted 2026-10-11 10:13 UTC, took 27 min\n" +
			"summary: added the p95\nfiles changed: " + cut + " (1)\ndiff: none\nspend: €0.0000\n2 steps",
	})
}
