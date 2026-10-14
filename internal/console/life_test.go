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
	task := factoryd.Task{ID: "t-life", Kind: "agent", Status: "waiting-human", Lane: "ready", Repository: "sf/code", CreatedAt: at(10, 0), SpentEUR: 0.70}
	status := func(h, m int, s string) factoryd.Event {
		return factoryd.Event{TaskID: "t-life", Kind: factoryd.EventTaskStatus, At: at(h, m), Detail: map[string]any{"status": s}}
	}
	events := []factoryd.Event{status(10, 5, "running"), status(10, 30, "in-review"), status(10, 36, "running"), status(10, 50, "in-review"),
		status(10, 57, "waiting-human"), {TaskID: "t-life", Kind: factoryd.EventQuestionAsked, At: at(10, 57), Detail: map[string]any{"question": "Merge PR #9?"}}}
	steps := []map[string]any{
		{"step": "session_start", "time": stamp(10, 5, 30), "session": "s-1", "model": "coding-cheap", "attempt": 1.0},
		{"step": "session_end", "time": stamp(10, 30, 0), "session": "s-1", "attempt": 1.0, "outcome": "pushed"},
		{"step": "checks", "time": stamp(10, 35, 50), "started": stamp(10, 30, 10), "state": "failure",
			"jobs": []any{map[string]any{"name": "test", "status": "failure"}, map[string]any{"name": "vet", "status": "success"}}},
		{"step": "session_start", "time": stamp(10, 36, 10), "session": "s-2", "model": "coding-frontier", "attempt": 2.0},
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
		{"building", "10:05", "25 min", "attempt 1, coding-cheap, pushed", 0.30},
		{"verifying", "10:30", "6 min", "checks 1/2, test failed", 0},
		{"building", "10:36", "14 min", "stalled on the provider, 429 · attempt 2, coding-frontier, pushed", 0.40},
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
	if runs[1].Attempt != 1 || runs[1].Model != "coding-cheap" || runs[3].Attempt != 2 || runs[3].Model != "coding-frontier" {
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
