package factoryd

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// [timeline_keeps_old_rows] Timeline objects keep their old rows as
// history: a sessions/<id>/timeline.jsonl written before the change, two of
// whose rows carry the decisions human and dropped_all, keeps every old row
// unrewritten when the same session answers a new session moment with
// continue, beside the one new row the answer adds.
func TestProofTimelineKeepsOldRows(t *testing.T) {
	requireStoredDecisionsOnTheRungsNames(t)
	objects := &fakeObjectStore{}
	old := []string{
		`{"time":"2026-11-12T09:00:00Z","step":"sizing","lines":800}`,
		`{"time":"2026-11-12T09:04:00Z","step":"checkin","round":2,"decision":"human","reason_kind":"goal_conflict","reason":"the objective contradicts the review"}`,
		`{"time":"2026-11-12T09:06:00Z","step":"handover","handover":"start from the store's New"}`,
		`{"time":"2026-11-12T09:30:00Z","step":"wip_pushed","head":"c0"}`,
		`{"time":"2026-11-12T09:40:00Z","step":"checkin","round":3,"decision":"dropped_all","reason":"every finding fell against the evidence"}`,
		`{"time":"2026-11-12T09:42:00Z","step":"handover","handover":"verify the head again"}`,
	}
	if err := objects.Put(context.Background(), "sessions/t2/timeline.jsonl", []byte(strings.Join(old, "\n")+"\n")); err != nil {
		t.Fatal(err)
	}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), kube, objects)
	runner.cfg.Supervisor.RepetitionEvents = 2
	chat := &scriptedChat{answers: []CheckinResponse{{Text: `{"decision":"continue","guidance":"use the store's New"}`}}}
	runner.Checkin = chat
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)
	logs := reads(0, 2) + reads(2, 1) +
		envLine(harness.Event{Kind: harness.EventFileChange, Detail: map[string]any{"path": "a.go"}}) + "\n" +
		pushedLine("c1") + "\n" + resultLine(true, "pushed")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed || res.Commit != "c1" {
		t.Fatalf("result = (%+v, %v), want pushed c1", res, err)
	}
	body, err := objects.Get(context.Background(), "sessions/t2/timeline.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) <= len(old) || !slices.Equal(lines[:len(old)], old) {
		t.Fatalf("the timeline holds %d rows with its first %d = %v, want every old row kept unrewritten", len(lines), len(old), lines[:min(len(lines), len(old))])
	}
	var answers int
	for _, line := range lines[len(old):] {
		if strings.Contains(line, `"step":"checkin"`) && strings.Contains(line, `"decision"`) {
			answers++
			if !strings.Contains(line, `"decision":"continue"`) {
				t.Fatalf("a new check-in row carries no continue: %s", line)
			}
		}
	}
	if answers != 1 {
		t.Fatalf("the moment's answer wrote %d check-in rows, want the one carrying continue", answers)
	}
}
