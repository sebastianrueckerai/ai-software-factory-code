package factoryd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// The agent's escalate_to_human ends the session like a supervisor's
// escalate, without a supervisor call: the worker is stopped, the outcome
// carries the question for the workflow's Escalate.
func TestRunSessionAgentEscalation(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, _ := newTestRunner(t, supervisedTask("t2"), kube, objects)
	f := &fakeLiteLLM{content: `{"action":"continue"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)
	runner.stopWait = 200 * time.Millisecond

	delivered := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := runner.EscalateSession("t2", "Which license applies?", "the issue names none")
			if !errors.Is(err, ErrNoLiveSession) || time.Now().After(deadline) {
				delivered <- err
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, hangLogs)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-delivered; err != nil {
		t.Fatalf("EscalateSession = %v", err)
	}
	if res.Outcome != OutcomeEscalate || !strings.HasPrefix(res.Reason, "The agent asks: Which license applies?\nWhy: the issue names none") ||
		!strings.Contains(res.Reason, "Detector: agent_escalation") {
		t.Fatalf("result = %+v", res)
	}
	if len(f.chats) != 0 {
		t.Fatalf("supervisor called %d times, want none", len(f.chats))
	}
	if len(kube.deletedPods) == 0 {
		t.Fatal("the worker was not stopped")
	}
	// the question reaches the issue through the workflow's Escalate, once
	if len(hub.comments) != 0 {
		t.Fatalf("comments = %q", hub.comments)
	}
	record := string(objGet(objects, "sessions/t2/"+res.SessionID+".jsonl"))
	var ev harness.Event
	json.Unmarshal([]byte(strings.TrimSpace(record)), &ev)
	if ev.Kind != harness.EventSupervisor || ev.Detail["detector"] != "agent_escalation" || ev.Detail["decision"] != "escalate" || ev.Detail["called"] != false {
		t.Fatalf("record = %s", record)
	}
	// the session is gone (it was on the first escalation): a late one finds none
	if err := runner.EscalateSession("t2", "q", "r"); !errors.Is(err, ErrNoLiveSession) {
		t.Fatalf("late escalation = %v", err)
	}
}

type fakeSessions struct {
	err   error
	calls []string
}

func (f *fakeSessions) EscalateSession(taskID, question, reason string) error {
	f.calls = append(f.calls, taskID+"|"+question+"|"+reason)
	return f.err
}

func TestEscalationHandler(t *testing.T) {
	store := newFakeStore()
	store.CreateTask(t.Context(), Task{ID: "t-run", Status: "running"})
	store.CreateTask(t.Context(), Task{ID: "t-wait", Status: "waiting-human"})
	post := func(sessions *fakeSessions, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		EscalationHandler(store, sessions).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/escalations", strings.NewReader(body)))
		return rec
	}
	body := func(task string) string {
		return fmt.Sprintf(`{"task_id":%q,"question":"Which license?","reason":"none named"}`, task)
	}

	s := &fakeSessions{}
	if rec := post(s, body("t-run")); rec.Code != http.StatusAccepted {
		t.Fatalf("running task = %d %s", rec.Code, rec.Body)
	}
	if len(s.calls) != 1 || s.calls[0] != "t-run|Which license?|none named" {
		t.Fatalf("delivered = %v", s.calls)
	}
	if e := store.intakeEvents; len(e) != 1 || e[0].Kind != IntakeAgentEscalation || e[0].Author != "agent" || e[0].Trusted ||
		e[0].Body != "Which license?\n\nWhy: none named" {
		t.Fatalf("intake events = %+v", e)
	}
	for name, tc := range map[string]struct {
		sessions *fakeSessions
		body     string
		want     int
	}{
		"unknown task":    {&fakeSessions{}, body("t-none"), http.StatusNotFound},
		"not running":     {&fakeSessions{}, body("t-wait"), http.StatusConflict},
		"no live session": {&fakeSessions{err: ErrNoLiveSession}, body("t-run"), http.StatusConflict},
		"no question":     {&fakeSessions{}, `{"task_id":"t-run","question":" ","reason":"r"}`, http.StatusBadRequest},
		"too long":        {&fakeSessions{}, fmt.Sprintf(`{"task_id":"t-run","question":%q,"reason":"r"}`, strings.Repeat("q", 4001)), http.StatusBadRequest},
		"not json":        {&fakeSessions{}, `nope`, http.StatusBadRequest},
	} {
		if rec := post(tc.sessions, tc.body); rec.Code != tc.want {
			t.Errorf("%s = %d %s, want %d", name, rec.Code, rec.Body, tc.want)
		}
	}
}
