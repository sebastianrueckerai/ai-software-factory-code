package console

import (
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A card's life as the Time section tells it (C10): one run per stage in
// order, a status row opening the run its status names, a session's
// start its attempt and model on the building run, the session's end,
// the checks and the review as notes on the run they fall in, a stall on
// the provider a note inside the building run, each building run with
// the cost of the model calls in its window, the last run open until now;
// and the at-a-glance line over them.
func TestRunsOfACardsLife(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 14, h, m, 0, 0, time.UTC) }
	stamp := func(h, m, s int) string { return time.Date(2026, 10, 14, h, m, s, 0, time.UTC).Format(time.RFC3339) }
	task := factoryd.Task{ID: "t-life", Kind: "agent", Status: "waiting-human", Track: "direct", Repository: "sf/code", CreatedAt: at(10, 0), SpentEUR: 0.70}
	status := func(h, m int, s string) factoryd.Event {
		return factoryd.Event{TaskID: "t-life", Kind: factoryd.EventTaskStatus, At: at(h, m), Detail: map[string]any{"status": s}}
	}
	events := []factoryd.Event{status(10, 5, "running"), status(10, 30, "in-review"), status(10, 36, "running"), status(10, 50, "in-review"),
		status(10, 57, "waiting-human"), {TaskID: "t-life", Kind: factoryd.EventQuestionAsked, At: at(10, 57), Detail: map[string]any{"question": "Merge PR #9?"}}}
	steps := []map[string]any{
		{"step": "session_start", "time": stamp(10, 5, 30), "session": "s-1", "model": "small", "attempt": 1.0},
		{"step": "session_end", "time": stamp(10, 30, 0), "session": "s-1", "attempt": 1.0, "outcome": "pushed"},
		{"step": "checks", "time": stamp(10, 35, 50), "started": stamp(10, 30, 10), "state": "failure",
			"jobs": []any{map[string]any{"name": "test", "status": "failure"}, map[string]any{"name": "vet", "status": "success"}}},
		{"step": "session_start", "time": stamp(10, 36, 10), "session": "s-2", "model": "big", "attempt": 2.0},
		{"step": factoryd.StepProviderRefused, "time": stamp(10, 40, 0), "status": 429.0},
		{"step": "session_end", "time": stamp(10, 50, 0), "session": "s-2", "attempt": 2.0, "outcome": "pushed"},
		{"step": "checks", "time": stamp(10, 55, 0), "started": stamp(10, 50, 10), "state": "success",
			"jobs": []any{map[string]any{"name": "test", "status": "success"}, map[string]any{"name": "vet", "status": "success"}}},
		{"step": "model_review", "time": stamp(10, 56, 0), "verdict": "approve"},
	}
	calls := []policy.ModelCall{{At: at(10, 10), EUR: 0.30}, {At: at(10, 40), EUR: 0.40}}
	runs := runsOf(task, events, steps, calls, at(11, 30))
	want := []struct {
		kind, at, dur, note string
		eur                 float64
	}{
		{"queued", "10:00", "5 min", "", 0},
		{"building", "10:05", "25 min", "attempt 1, small, pushed", 0.30},
		{"verifying", "10:30", "6 min", "checks 1/2, test failed", 0},
		{"building", "10:36", "14 min", "stalled on the provider, 429 · attempt 2, big, pushed", 0.40},
		{"verifying", "10:50", "7 min", "checks 2/2 · review by factoryd: approve", 0},
		{"waiting on you", "10:57", "33 min", "asked: Merge PR #9?", 0},
	}
	if len(runs) != len(want) {
		t.Fatalf("%d runs, want %d: %+v", len(runs), len(want), runs)
	}
	for i, w := range want {
		r := runs[i]
		if r.Kind != w.kind || r.At() != w.at || r.Duration() != w.dur || r.Note() != w.note || r.EUR != w.eur {
			t.Errorf("run %d: %s at %s for %s, %q, €%.2f; want %s at %s for %s, %q, €%.2f", i, r.Kind, r.At(), r.Duration(), r.Note(), r.EUR, w.kind, w.at, w.dur, w.note, w.eur)
		}
	}
	if runs[1].Attempt != 1 || runs[1].Model != "small" || runs[3].Attempt != 2 || runs[3].Model != "big" {
		t.Errorf("the building runs' attempts: %+v, %+v", runs[1], runs[3])
	}
	if got, want := lifeLine(runs, task.SpentEUR), "52 min factory in 2 attempts · 38 min waiting · €0.70"; got != want {
		t.Errorf("the line is %q, want %q", got, want)
	}
	// a card without a session or an event is one queued run from its filing
	fresh := factoryd.Task{ID: "t-new", Status: "queued", CreatedAt: at(11, 0)}
	if runs := runsOf(fresh, nil, nil, nil, at(11, 30)); len(runs) != 1 || runs[0].Kind != "queued" || runs[0].Minutes != 30 {
		t.Errorf("a fresh card's runs: %+v", runs)
	}
	if got := lifeLine(runsOf(fresh, nil, nil, nil, at(11, 30)), 0); got != "30 min waiting" {
		t.Errorf("a fresh card's line is %q", got)
	}
}

// A question withdrawn unanswered, a beside gate's whose head went back,
// is a note on the run it falls in: the withdrawal puts the card in
// review again at 10:20, after five minutes waiting on you, and the next
// session builds from 10:21.
func TestRunsOfACardsLifeNoteAWithdrawnQuestion(t *testing.T) {
	at := func(h, m, s int) time.Time { return time.Date(2026, 10, 15, h, m, s, 0, time.UTC) }
	task := factoryd.Task{ID: "t-w", Kind: "agent", Status: "running", Track: "direct", CreatedAt: at(9, 50, 0)}
	status := func(t time.Time, s string) factoryd.Event {
		return factoryd.Event{TaskID: "t-w", Kind: factoryd.EventTaskStatus, At: t, Detail: map[string]any{"status": s}}
	}
	const q = "The gate supervisor stopped ModelReview (attempt 1) past its bound of 10m0s: the model hangs"
	events := []factoryd.Event{status(at(10, 0, 0), "in-review"), status(at(10, 15, 0), "waiting-human"),
		{TaskID: "t-w", Kind: factoryd.EventQuestionAsked, At: at(10, 15, 0), Detail: map[string]any{"question": q, "comment": 601.0}},
		status(at(10, 20, 0), "in-review"),
		{TaskID: "t-w", Kind: factoryd.EventQuestionWithdrawn, At: at(10, 20, 1), Detail: map[string]any{"question": q, "comment": 601.0, "reason": "the checks failed"}},
		status(at(10, 21, 0), "running")}
	runs := runsOf(task, events, nil, nil, at(10, 30, 0))
	want := []struct{ kind, at, note string }{
		{"queued", "09:50", ""},
		{"verifying", "10:00", ""},
		{"waiting on you", "10:15", "asked: " + q},
		{"verifying", "10:20", "withdrawn because the checks failed: " + q},
		{"building", "10:21", ""},
	}
	if len(runs) != len(want) {
		t.Fatalf("%d runs, want %d: %+v", len(runs), len(want), runs)
	}
	for i, w := range want {
		if r := runs[i]; r.Kind != w.kind || r.At() != w.at || r.Note() != w.note {
			t.Errorf("run %d: %s at %s, %q; want %s at %s, %q", i, r.Kind, r.At(), r.Note(), w.kind, w.at, w.note)
		}
	}
}

// The sizing step is a note on the run it falls in: the size and why, or
// that the card runs as filed; a card that carries a pull request has none.
func TestRunsOfACardsLifeNotesTheSizing(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 29, h, m, 0, 0, time.UTC) }
	task := factoryd.Task{ID: "t-s", Kind: "agent", Status: "running", Track: "direct", CreatedAt: at(10, 0)}
	events := []factoryd.Event{{TaskID: "t-s", Kind: factoryd.EventTaskStatus, At: at(10, 4), Detail: map[string]any{"status": "running"}}}
	for _, tc := range []struct {
		step map[string]any
		want string
	}{
		{map[string]any{"size": "medium", "plan": true, "reason": "touches the scheduler and the store"},
			"sized medium, not small, so the planner takes it: touches the scheduler and the store"},
		{map[string]any{"size": "small", "sized": "small", "plan": false, "reason": "one handler and its test"},
			"sized small: one handler and its test"},
		{map[string]any{"size": "", "plan": false, "reason": "the sizing task t-s ended failed"},
			"not sized, it runs as filed: the sizing task t-s ended failed"},
		{map[string]any{"skipped": "the card carries pull request #9, whose work exists"}, ""},
	} {
		tc.step["step"] = factoryd.StepSizing
		tc.step["time"] = at(10, 3).Format(time.RFC3339)
		runs := runsOf(task, events, []map[string]any{tc.step}, nil, at(10, 30))
		if len(runs) == 0 || runs[0].Kind != "queued" || runs[0].At() != "10:00" || runs[0].Duration() != "4 min" || runs[0].Note() != tc.want {
			t.Errorf("step %v: runs %+v, want queued 10:00 for 4 min with note %q", tc.step, runs, tc.want)
		}
	}
}
