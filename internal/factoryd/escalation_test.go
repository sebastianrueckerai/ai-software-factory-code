package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	runner, hub, store := newTestRunner(t, supervisedTask("t2"), kube, objects)
	f := &fakeLiteLLM{content: `{"action":"continue"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)
	runner.stopWait = 200 * time.Millisecond

	delivered := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := runner.EscalateSession(t.Context(), "t2", licenseAsk)
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
	if res.Outcome != OutcomeEscalate || !strings.HasPrefix(res.Reason, "The agent asks: Which license applies?\nWhy: the issue names none\nRecommendation: MIT, as the other repositories") ||
		!strings.Contains(res.Reason, "Detector: agent_escalation") {
		t.Fatalf("result = %+v", res)
	}
	if len(f.chats) != 0 {
		t.Fatalf("supervisor called %d times, want none", len(f.chats))
	}
	// the agent's question is the workflow's escalation alert, no trip
	if alerts := store.alertRows(); len(alerts) != 0 {
		t.Fatalf("alerts = %+v, want none from the session", alerts)
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
	if err := runner.EscalateSession(t.Context(), "t2", licenseAsk); !errors.Is(err, ErrNoLiveSession) {
		t.Fatalf("late escalation = %v", err)
	}
}

// An escalate_to_human that arrives while a supervisor call is in flight
// wins: the agent was told "escalated", so the supervisor's redirect never
// takes effect, and the call is cut.
func TestAgentEscalationWinsOverAnInFlightSupervisorCall(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, _ := newTestRunner(t, supervisedTask("t2"), kube, objects)
	runner.stopWait = 5 * time.Second
	inFlight, cut := make(chan struct{}), make(chan struct{})
	var once sync.Once
	sup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key/list":
			io.WriteString(w, `{"keys":[],"total_pages":1}`)
		case "/key/generate":
			io.WriteString(w, `{"key":"sk-super-1","token":"hash1"}`)
		case "/v1/chat/completions":
			io.ReadAll(r.Body)
			once.Do(func() { close(inFlight) })
			select {
			case <-r.Context().Done():
				close(cut)
				return
			case <-time.After(20 * time.Second):
			}
			io.WriteString(w, `{"choices":[{"message":{"content":"{\"action\":\"redirect\",\"guidance\":\"try another way\"}"}}]}`)
		}
	}))
	t.Cleanup(sup.Close)
	runner.supervisor = testSupervisorModel(sup.URL)

	delivered := make(chan error, 1)
	go func() {
		<-inFlight
		delivered <- runner.EscalateSession(t.Context(), "t2", licenseAsk)
	}()
	logs := loopFailure(1) + loopFailure(2) + loopFailure(3) + cutLogs +
		pushedLine("c-wip") + "\n" + resultLine(false, "stopped; work in progress pushed at c-wip") + "\n"
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-delivered; err != nil {
		t.Fatalf("EscalateSession = %v", err)
	}
	select {
	case <-cut:
	case <-time.After(5 * time.Second):
		t.Fatal("the escalation did not cut the supervisor call")
	}
	if res.Outcome != OutcomeEscalate || !strings.HasPrefix(res.Reason, "The agent asks: Which license applies?") || res.Commit != "c-wip" {
		t.Fatalf("result = %+v, want the agent's escalation with the work in progress", res)
	}
	if strings.Contains(strings.Join(res.Findings, "\n"), "try another way") {
		t.Fatalf("findings = %v carry the superseded redirect", res.Findings)
	}
	if len(hub.comments) != 0 {
		t.Fatalf("comments = %q, want the question left to the workflow's Escalate", hub.comments)
	}
	record := string(objGet(objects, "sessions/t2/"+res.SessionID+".jsonl"))
	if !strings.Contains(record, `"detector":"agent_escalation"`) || strings.Contains(record, `"decision":"redirect"`) {
		t.Fatalf("record = %s", record)
	}
	// the stopped session takes no second escalation
	if err := runner.EscalateSession(t.Context(), "t2", licenseAsk); !errors.Is(err, ErrNoLiveSession) {
		t.Fatalf("escalation after the stop = %v", err)
	}
}

type fakeSessions struct {
	err   error
	calls []string
}

func (f *fakeSessions) EscalateSession(_ context.Context, taskID string, a AgentAsk) error {
	f.calls = append(f.calls, strings.Join([]string{taskID, a.Kind, a.Question, a.Reason, a.Recommendation, a.Tried, a.Needs}, "|"))
	return f.err
}

// licenseAsk is an agent's escalate_to_human.
var licenseAsk = AgentAsk{Kind: AskEscalation, Question: "Which license applies?", Reason: "the issue names none", Recommendation: "MIT, as the other repositories"}

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
		return fmt.Sprintf(`{"task_id":%q,"kind":"escalation","question":"Which license?","reason":"none named","recommendation":" MIT "}`, task)
	}

	s := &fakeSessions{}
	if rec := post(s, body("t-run")); rec.Code != http.StatusAccepted {
		t.Fatalf("running task = %d %s", rec.Code, rec.Body)
	}
	info := `{"task_id":"t-run","kind":"info_request","question":"Which browser?","tried":"the steps","needs":"the version"}`
	if rec := post(s, info); rec.Code != http.StatusAccepted {
		t.Fatalf("info request = %d %s", rec.Code, rec.Body)
	}
	if len(s.calls) != 2 || s.calls[0] != "t-run|escalation|Which license?|none named|MIT||" || s.calls[1] != "t-run|info_request|Which browser?|||the steps|the version" {
		t.Fatalf("delivered = %v", s.calls)
	}
	if e := store.intakeEvents; len(e) != 2 || e[0].Kind != IntakeAgentEscalation || e[0].Author != "agent" || e[0].Trusted ||
		e[0].Body != "Which license?\n\nWhy: none named\n\nRecommendation: MIT" ||
		e[1].Kind != IntakeAgentInfoRequest || e[1].Body != "Which browser?\n\nTried: the steps\n\nNeeds: the version" {
		t.Fatalf("intake events = %+v", e)
	}
	for name, tc := range map[string]struct {
		sessions *fakeSessions
		body     string
		want     int
	}{
		"unknown task":        {&fakeSessions{}, body("t-none"), http.StatusNotFound},
		"not running":         {&fakeSessions{}, body("t-wait"), http.StatusConflict},
		"no live session":     {&fakeSessions{err: ErrNoLiveSession}, body("t-run"), http.StatusConflict},
		"no question":         {&fakeSessions{}, `{"task_id":"t-run","kind":"escalation","question":" ","reason":"r","recommendation":"x"}`, http.StatusBadRequest},
		"too long":            {&fakeSessions{}, fmt.Sprintf(`{"task_id":"t-run","kind":"escalation","question":%q,"reason":"r","recommendation":"x"}`, strings.Repeat("q", 4001)), http.StatusBadRequest},
		"no recommendation":   {&fakeSessions{}, `{"task_id":"t-run","kind":"escalation","question":"q","reason":"r"}`, http.StatusBadRequest},
		"long recommendation": {&fakeSessions{}, fmt.Sprintf(`{"task_id":"t-run","kind":"escalation","question":"q","reason":"r","recommendation":%q}`, strings.Repeat("x", 2001)), http.StatusBadRequest},
		"info without needs":  {&fakeSessions{}, `{"task_id":"t-run","kind":"info_request","question":"q","tried":"t"}`, http.StatusBadRequest},
		"unknown kind":        {&fakeSessions{}, `{"task_id":"t-run","kind":"chat","question":"q","reason":"r","recommendation":"x"}`, http.StatusBadRequest},
		"no kind":             {&fakeSessions{}, `{"task_id":"t-run","question":"q","reason":"r","recommendation":"x"}`, http.StatusBadRequest},
		"not json":            {&fakeSessions{}, `nope`, http.StatusBadRequest},
	} {
		if rec := post(tc.sessions, tc.body); rec.Code != tc.want {
			t.Errorf("%s = %d %s, want %d", name, rec.Code, rec.Body, tc.want)
		}
	}
	// only the accepted asks are recorded
	if n := len(store.intakeEvents); n != 2 {
		t.Fatalf("intake events = %d, want 1: %+v", n, store.intakeEvents)
	}
}

// An escalation counts as delivered only once the session took it: one the
// session never reads before it ends is refused, never accepted and lost.
func TestEscalateSessionNeedsTheSessionToTakeIt(t *testing.T) {
	runner := &Runner{}
	esc, unregister := runner.liveSession("t1")
	go func() {
		time.Sleep(50 * time.Millisecond)
		unregister()
	}()
	if err := runner.EscalateSession(t.Context(), "t1", licenseAsk); !errors.Is(err, ErrNoLiveSession) {
		t.Fatalf("escalation to a session that ended without reading = %v, want ErrNoLiveSession", err)
	}
	select {
	case e := <-esc:
		t.Fatalf("escalation %+v left behind", e)
	default:
	}

	esc, unregister = runner.liveSession("t1")
	defer unregister()
	took := make(chan AgentAsk, 1)
	go func() { took <- <-esc }()
	if err := runner.EscalateSession(t.Context(), "t1", licenseAsk); err != nil {
		t.Fatalf("escalation to a reading session = %v", err)
	}
	if e := <-took; e != licenseAsk {
		t.Fatalf("took %+v", e)
	}
}

// The agent's request_info ends the session like an escalation, with the
// outcome needs_info and the request as its reason, no supervisor call,
// no trip alert (the workflow's RequestInfo records info_request).
func TestRunSessionAgentInfoRequest(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, store := newTestRunner(t, supervisedTask("t2"), kube, objects)
	f := &fakeLiteLLM{content: `{"action":"continue"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)
	runner.stopWait = 200 * time.Millisecond
	ask := AgentAsk{Kind: AskInfo, Question: "Which browser?", Tried: "the steps on Go 1.26", Needs: "the browser and its version"}
	delivered := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := runner.EscalateSession(t.Context(), "t2", ask)
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
	want := "The agent asks the issue's author: Which browser?\nTried: the steps on Go 1.26\nNeeds: the browser and its version"
	if res.Outcome != OutcomeNeedsInfo || res.Reason != want {
		t.Fatalf("result = %+v, want needs_info with %q", res, want)
	}
	if len(f.chats) != 0 || len(store.alertRows()) != 0 || len(hub.comments) != 0 {
		t.Fatalf("supervisor calls %d, alerts %+v, comments %q: want none from the session", len(f.chats), store.alertRows(), hub.comments)
	}
	if !strings.Contains(string(objGet(objects, "sessions/t2/"+res.SessionID+".jsonl")), `"detector":"agent_info_request"`) {
		t.Fatal("the session record lacks the info request")
	}
}

// A pushed session leaves the agent's last message on the task row: its
// recommendation and a not_reproduced line for the gates.
func TestRunSessionKeepsTheAgentsLastWord(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix the slug", Repository: "sf/ai-software-factory-code", SHA: "abc123", BudgetEUR: 2}
	runner, _, store := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
	last := "Fixed the slug check.\nnot_reproduced: the report's URL is gone; the check refused every dash"
	logs := envLine(harness.Event{Kind: harness.EventFinished, Time: time.Now(), Detail: map[string]any{"success": true, "summary": last}}) + "\n" +
		pushedLine("c0ffee") + "\n" + resultLine(true, "pushed branch factory/t1 at c0ffee")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/coding-cheap"}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed {
		t.Fatalf("RunSession = %+v, %v", res, err)
	}
	if got := store.tasks["t1"].AgentSummary; got != last {
		t.Fatalf("agent summary = %q, want %q", got, last)
	}
}
