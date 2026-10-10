package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeEventRecorder is the events table of a Timeline in tests: it keeps
// every row and answers ButtonCommentIDs from them, as PGStore does.
type fakeEventRecorder struct {
	mu   sync.Mutex
	rows []Event
}

func (f *fakeEventRecorder) Record(_ context.Context, e Event) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, e)
	return int64(len(f.rows)), nil
}

func (f *fakeEventRecorder) ButtonCommentIDs(_ context.Context, taskID string) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int64
	for i := len(f.rows) - 1; i >= 0; i-- {
		e := f.rows[i]
		if e.TaskID == taskID && e.Kind == EventQuestionAnswered && e.Via == ViaButton {
			if c, ok := e.Detail["comment"].(int64); ok {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// answerEvents is recorder's question_answered rows of task id, as
// "actor via comment".
func answerEvents(rec *fakeEventRecorder, id string) []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []string
	for _, e := range rec.rows {
		if e.TaskID == id && e.Kind == EventQuestionAnswered {
			c, _ := e.Detail["comment"].(int64)
			out = append(out, fmt.Sprintf("%s %s %d", e.Actor, e.Via, c))
		}
	}
	return out
}

// An answer that arrives as an issue comment leaves one question_answered
// row via webhook (ruling 252): a writer's on a task waiting for a human,
// the poster's on one waiting for the author. A comment the console's
// Answer button posted itself names stays silent: the button recorded its
// own row naming that comment, so one question leaves one event whatever
// the input.
func TestIntakeAnswerLeavesOneWebhookRow(t *testing.T) {
	comment := func(in *Intake, number int64, user string, id int64) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{
			"action":     "created",
			"issue":      map[string]any{"number": number},
			"comment":    map[string]any{"id": id, "user": map[string]string{"login": user}, "body": "use a replica"},
			"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		})
		if rr := postWebhook(in, "issue_comment", b, sign(webhookSecret, b)); rr.Code != http.StatusOK {
			t.Fatalf("%s %d: status %d", user, id, rr.Code)
		}
	}

	// a writer's comment on a waiting-human task records it by webhook
	in, store, _ := newTestIntake(t)
	store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", IssueNumber: 12, Status: "waiting-human", Kind: "agent"}
	rec := &fakeEventRecorder{}
	objects := &fakeObjectStore{}
	in.Timeline = &Timeline{Objects: objects, Events: rec, Store: store}
	comment(in, 12, "sebastian", 71)
	if got := answerEvents(rec, "t1"); !slices.Equal(got, []string{"sebastian webhook 71"}) {
		t.Fatalf("a writer's answer: %v", got)
	}

	// the same comment through intake again (a redelivery) adds nothing
	comment(in, 12, "sebastian", 71)
	if got := answerEvents(rec, "t1"); len(got) != 1 {
		t.Fatalf("a redelivered answer: %v, want one row", got)
	}

	// a comment the button already recorded stays silent: the button's
	// own row named it, so intake must not double-count it
	in2, store2, _ := newTestIntake(t)
	store2.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", IssueNumber: 13, Status: "waiting-human", Kind: "agent"}
	rec2 := &fakeEventRecorder{}
	rec2.rows = append(rec2.rows, Event{Repository: "sf/ai-software-factory-code", TaskID: "t1", Kind: EventQuestionAnswered,
		Actor: "sebastian", Via: ViaButton, Detail: map[string]any{"comment": int64(72)}})
	in2.Timeline = &Timeline{Objects: &fakeObjectStore{}, Events: rec2, Store: store2}
	comment(in2, 13, "sebastian", 72)
	if got := answerEvents(rec2, "t1"); len(got) != 1 || got[0] != "sebastian button 72" {
		t.Fatalf("the button's own comment was counted twice: %v", got)
	}

	// a later comment still counts, even though an earlier button answer
	// sits in the list (sebastian is this repository's writer)
	comment(in2, 13, "sebastian", 73)
	if got := answerEvents(rec2, "t1"); len(got) != 2 || !strings.Contains(got[1], "webhook") {
		t.Fatalf("a later answer after a button's: %v", got)
	}

	// the poster's comment on a waiting-poster task records it too
	in3, store3, _ := newTestIntake(t)
	store3.tasks["t-p"] = Task{ID: "t-p", Repository: "sf/ai-software-factory-code", IssueNumber: 30, Status: "waiting-poster", Kind: "agent", Author: "visitor"}
	rec3 := &fakeEventRecorder{}
	in3.Timeline = &Timeline{Objects: &fakeObjectStore{}, Events: rec3, Store: store3}
	comment(in3, 30, "stranger", 81) // neither the poster nor a writer: no row
	comment(in3, 30, "visitor", 82)  // the poster: one row
	if got := answerEvents(rec3, "t-p"); !slices.Equal(got, []string{"visitor webhook 82"}) {
		t.Fatalf("the poster's answer: %v", got)
	}
}
