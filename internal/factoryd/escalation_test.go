package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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
		!strings.Contains(res.Reason, "Detector: agent_escalation") || !slices.Equal(res.Choices, []string{"MIT", "Apache-2.0", "GPL-3.0"}) {
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
	asks  []AgentAsk
}

func (f *fakeSessions) EscalateSession(_ context.Context, taskID string, a AgentAsk) error {
	f.calls = append(f.calls, strings.Join([]string{taskID, a.Kind, a.Question, a.Reason, a.Recommendation, a.Tried, a.Needs}, "|"))
	f.asks = append(f.asks, a)
	return f.err
}

// licenseAsk is an agent's escalate_to_human with its choices.
var licenseAsk = AgentAsk{Kind: AskEscalation, Question: "Which license applies?", Reason: "the issue names none", Recommendation: "MIT, as the other repositories",
	Choices: []string{"MIT", "Apache-2.0", "GPL-3.0"}}

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

// An escalation's choices are 2 to 5, none empty or over 60 characters,
// each trimmed: they reach the session and the agent's intake event. Any
// other list is dropped and logged, and the question still delivered.
func TestEscalationHandlerChoices(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	store := newFakeStore()
	store.CreateTask(t.Context(), Task{ID: "t-run", Status: "running"})
	wide := strings.Repeat("ä", 60) // 60 characters in 120 bytes
	for _, tc := range []struct {
		name, choices, logged string
		want                  []string
	}{
		{"three", `[" MIT ","Apache-2.0","GPL-3.0"]`, "", []string{"MIT", "Apache-2.0", "GPL-3.0"}},
		{"60 characters", `["` + wide + `","no"]`, "", []string{wide, "no"}},
		{"none", `[]`, "", nil},
		{"six", `["a","b","c","d","e","f"]`, "task t-run: the agent's 6 choices are dropped", nil},
		{"61 characters", `["` + strings.Repeat("a", 61) + `","no"]`, "task t-run: the agent's 2 choices are dropped", nil},
		{"an empty one", `["yes"," "]`, "task t-run: the agent's 2 choices are dropped", nil},
		{"one", `["yes"]`, "task t-run: the agent's 1 choices are dropped", nil},
	} {
		logs.Reset()
		store.intakeEvents = nil
		s := &fakeSessions{}
		rec := httptest.NewRecorder()
		EscalationHandler(store, s).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/escalations", strings.NewReader(
			`{"task_id":"t-run","kind":"escalation","question":"Which license?","reason":"none named","recommendation":"MIT","choices":`+tc.choices+`}`)))
		if rec.Code != http.StatusAccepted || len(s.asks) != 1 || !slices.Equal(s.asks[0].Choices, tc.want) {
			t.Errorf("%s: %d, delivered %+v; want 202 with choices %q", tc.name, rec.Code, s.asks, tc.want)
		}
		if e := store.intakeEvents; len(e) != 1 || !slices.Equal(e[0].Choices, tc.want) {
			t.Errorf("%s: intake events %+v, want one with choices %q", tc.name, e, tc.want)
		}
		if got := logs.String(); tc.logged == "" && strings.Contains(got, "dropped") || !strings.Contains(got, tc.logged) {
			t.Errorf("%s: log %q, want %q", tc.name, got, tc.logged)
		}
	}
	// the issue's author answers an information request on Forgejo: it keeps no choices
	store.intakeEvents = nil
	s := &fakeSessions{}
	rec := httptest.NewRecorder()
	EscalationHandler(store, s).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/escalations", strings.NewReader(
		`{"task_id":"t-run","kind":"info_request","question":"Which browser?","tried":"the steps","needs":"the version","choices":["Firefox","Chrome"]}`)))
	if rec.Code != http.StatusAccepted || len(s.asks) != 1 || s.asks[0].Choices != nil || len(store.intakeEvents) != 1 || store.intakeEvents[0].Choices != nil {
		t.Fatalf("info request = %d, delivered %+v, stored %+v; want no choices", rec.Code, s.asks, store.intakeEvents)
	}
}

// An escalation counts as delivered only once the session took it: one the
// session never reads before it ends is refused, never accepted and lost.
func TestEscalateSessionNeedsTheSessionToTakeIt(t *testing.T) {
	runner := &Runner{}
	esc, unregister := runner.liveSession("t1", sessionWorker{})
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

	esc, unregister = runner.liveSession("t1", sessionWorker{})
	defer unregister()
	took := make(chan AgentAsk, 1)
	go func() { took <- <-esc }()
	if err := runner.EscalateSession(t.Context(), "t1", licenseAsk); err != nil {
		t.Fatalf("escalation to a reading session = %v", err)
	}
	if e := <-took; !reflect.DeepEqual(e, licenseAsk) {
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
	// a pushed session without a last message keeps it (review M5)
	task.AgentSummary = last
	runner, _, store = newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
	logs = pushedLine("c0ffee2") + "\n" + resultLine(true, "pushed branch factory/t1 at c0ffee2")
	if res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 2, Model: "factory/coding-cheap"}, nil, logs); err != nil || res.Outcome != OutcomePushed {
		t.Fatalf("RunSession = %+v, %v", res, err)
	}
	if got := store.tasks["t1"].AgentSummary; got != last {
		t.Fatalf("agent summary after a silent session = %q, want %q", got, last)
	}
}

// A session the gateway refused at the month cap waits on a human with the
// refusal as its reason: no later session could spend, whatever it tried.
func TestRunSessionParksAtTheMonthCap(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix the slug", Repository: "sf/ai-software-factory-code", SHA: "abc123", BudgetEUR: 2}
	runner, _, store := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
	refusal := "free headroom 0.000100 EUR (task 1.900000, day 19.000000, month 0.000100) fits 25 output tokens after at most 4000 input tokens, below the minimum call of 1024; the month cap budget.month_eur in policy.yaml is spent, and only a human raises it, by a commit"
	store.capRefusals = map[string]CapRefusal{"t1": {Refusal: policy.Refusal{Decision: policy.RefusedMonthCap, Reason: refusal}}}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/coding-cheap"}, nil,
		resultLine(false, "the model call failed: 402 budget_exceeded"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeEscalate || res.Reason != monthCapReached+refusal {
		t.Fatalf("result = %+v, want escalate with the month cap's refusal", res)
	}
	if got := escalationSource(res.Reason); got != "month_cap" {
		t.Fatalf("escalation source = %q, want month_cap", got)
	}
	// a session the month cap did not refuse ends as before
	runner, _, _ = newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
	res, err = runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/coding-cheap"}, nil,
		resultLine(false, "the model call failed: 402 budget_exceeded"))
	if err != nil || res.Outcome != OutcomeNoPush {
		t.Fatalf("result without a month cap refusal = %+v, %v, want no_push", res, err)
	}
}

// A session the gateway refused at the day cap (card #784, 2026-10-14
// 20:43Z) ends day_cap with the refusal and whether the gateway allowed a
// call of it before: its card names the refusal and resumes on its own,
// once the day's headroom funds a session or at the next 00:00 UTC.
func TestRunSessionWaitsForTheDayCap(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix main", Repository: "sf/ai-software-factory-code", SHA: "abc123", BudgetEUR: 2}
	runner, _, store := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
	refusal := "free headroom -23.439417 EUR (task 2.000000, day -23.439417, month 54.546612) fits 0 output tokens after a request of 61234 bytes (at most 61234 prompt tokens), below the minimum call of 1024"
	capped := CapRefusal{Refusal: policy.Refusal{Decision: policy.RefusedDayCap, Reason: refusal}, At: time.Date(2026, 10, 14, 20, 43, 31, 0, time.UTC), Allowed: true}
	store.capRefusals = map[string]CapRefusal{"t1": capped}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/coding-cheap"}, nil,
		resultLine(false, "the model call failed: 402 refused_day_cap"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeDayCap || res.Capped == nil || res.Capped.Refusal != capped.Refusal || !res.Capped.At.Equal(capped.At) || !res.Capped.Allowed {
		t.Fatalf("result = %+v, want day_cap with the refusal", res)
	}
	question := "the day cap is reached: the card resumes on its own once the day's free headroom " +
		"funds a session, at the latest when the UTC day turns at 2026-10-15T00:00:00Z. Raise budget.day_eur in policy.yaml to resume it sooner, or close the card."
	reason := res.Capped.reason()
	if want := question + "\nthe gateway refused this task's spend (refused_day_cap): " + refusal; reason != want || !res.Capped.until().Equal(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("reason %q until %v, want %q until 2026-10-15 00:00 UTC", reason, res.Capped.until(), want)
	}
	if got := escalationQuestion(reason); got != question {
		t.Fatalf("question row = %q, want %q", got, question)
	}
	if got := escalationSource(reason); got != "day_cap" {
		t.Fatalf("escalation source = %q, want day_cap", got)
	}
}

// A session whose first model call the provider refused, with no call
// allowed after, ends provider_refused with the provider's status and the
// worker's message (ruling 267); a refusal after work, a refusal the
// provider's status names no outage of, and a session the audit holds no
// call of end no_push as before.
func TestRunSessionNamesAProviderRefusal(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix the slug", Repository: "sf/ai-software-factory-code", SHA: "abc123", BudgetEUR: 2}
	const said = "DashscopeException: budget exhausted (status 429)"
	for name, tc := range map[string]struct {
		audit   string // ProviderRefusal's reason; "" for none
		outcome SessionOutcome
		status  int
	}{
		"refused at the first call": {"upstream 429", OutcomeProviderRefused, 429},
		"payment required":          {"upstream 402", OutcomeProviderRefused, 402},
		"provider down":             {"upstream 503", OutcomeProviderRefused, 503},
		"litellm unreachable":       {"dial tcp 10.0.0.9:4000: connect: connection refused", OutcomeProviderRefused, 502},
		"a bad request":             {"upstream 400", OutcomeNoPush, 0},
		"after work or no call":     {"", OutcomeNoPush, 0},
	} {
		t.Run(name, func(t *testing.T) {
			runner, _, store := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
			if tc.audit != "" {
				store.providerRefusals = map[string]string{"t1": tc.audit}
			}
			res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/coding-cheap"}, nil, resultLine(false, said))
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != tc.outcome || res.Status != tc.status || tc.status != 0 && res.Reason != said {
				t.Fatalf("result = %+v, want %s, status %d", res, tc.outcome, tc.status)
			}
		})
	}
	// the month cap is the factory's own refusal: a human raises it
	runner, _, store := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
	store.providerRefusals = map[string]string{"t1": "upstream 429"}
	store.capRefusals = map[string]CapRefusal{"t1": {Refusal: policy.Refusal{Decision: policy.RefusedMonthCap, Reason: "spent"}}}
	if res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/coding-cheap"}, nil, resultLine(false, said)); err != nil || res.Outcome != OutcomeEscalate {
		t.Fatalf("month cap and a provider refusal = %+v, %v, want escalate", res, err)
	}
}

// The provider's refusal of repeated calls, opencode's error event after
// many allowed calls, ends the session as the loop rule does: a redirect
// on loop with the pushed work in progress, the provider's text in the
// timeline's session_end and in the next session's brief. No outage: the
// first-call rule (the audit holds allowed calls) does not take it.
func TestRunSessionRepetitionRefusalIsALoop(t *testing.T) {
	p := &opencodeParser{}
	p.line([]byte(`{"type":"error","sessionID":"ses_x","error":{"name":"APIError","data":{"message":"litellm.BadRequestError: DashscopeException - <400> `+
		`InternalError.Algo.InvalidParameter: Repetitive tool calls detected in the conversation history","statusCode":400}}}`), func(harness.Event) {})
	said := p.errMessage() + "; work in progress pushed to factory/t2 at c-wip"
	objects := &fakeObjectStore{}
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), objects)
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)
	logs := reads(0, 3) + pushRequestLine("c-wip") + "\n" + tokenLogs + pushedLine("c-wip") + "\n" + resultLine(false, said)
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomeRedirect || res.Trip != string(TripLoop) || res.Commit != "c-wip" || res.Summary != said {
		t.Fatalf("result = (%+v, %v), want a redirect on loop with c-wip", res, err)
	}
	line := "the previous session ended in a loop after the model provider refused its repeated tool calls: APIError: litellm.BadRequestError: " +
		"DashscopeException - <400> InternalError.Algo.InvalidParameter: Repetitive tool calls detected in the conversation history (status 400); " +
		"work in progress pushed to factory/t2 at c-wip"
	if !slices.Contains(res.Findings, line) {
		t.Fatalf("findings = %q, want %q", res.Findings, line)
	}
	tl, _ := objects.Get(t.Context(), "sessions/t2/timeline.jsonl")
	var end map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(tl)), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(l), &e) == nil && e["step"] == "session_end" {
			end = e
		}
	}
	if end == nil || end["outcome"] != "redirect" || end["summary"] != said {
		t.Fatalf("timeline session_end = %v", end)
	}
	if !slices.Equal(worker.texts, []string{"POST /push tok-task-t2"}) {
		t.Fatalf("worker got %q, want the push token", worker.texts)
	}
}

// A failed session that pushed its work is unfinished, not without a
// commit, in the next brief; one that pushed nothing keeps the old line.
// The model's own text quoting the refusal is no provider error event (no
// "(status 400)"), so it is no loop.
func TestRunSessionFailedSessionsLine(t *testing.T) {
	const quoted = "the log says Repetitive tool calls detected, so I stop here"
	for name, tc := range map[string]struct{ said, push, line string }{
		"a 400 after work, pushed": {"APIError: context window exceeded (status 400)", "c-wip",
			"previous session ended unfinished: APIError: context window exceeded (status 400); work in progress pushed to factory/t2 at c-wip"},
		"the model quotes the refusal, pushed": {quoted, "c-wip",
			"previous session ended unfinished: " + quoted + "; work in progress pushed to factory/t2 at c-wip"},
		"nothing pushed": {"APIError: context window exceeded (status 400)", "",
			"previous session ended without a pushed commit: APIError: context window exceeded (status 400)"},
	} {
		t.Run(name, func(t *testing.T) {
			runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
			worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
			worker.start(t, runner)
			said, logs := tc.said, reads(0, 2)
			if tc.push != "" {
				said += "; work in progress pushed to factory/t2 at " + tc.push
				logs += pushRequestLine(tc.push) + "\n" + tokenLogs + pushedLine(tc.push) + "\n"
			}
			res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs+resultLine(false, said))
			if err != nil || res.Outcome != OutcomeNoPush || res.Trip != "" {
				t.Fatalf("result = (%+v, %v), want no_push without a trip", res, err)
			}
			if got := res.Findings[len(res.Findings)-1]; got != tc.line {
				t.Fatalf("line = %q, want %q", got, tc.line)
			}
		})
	}
}

// A session whose model call crossed its cost hold (card #756, audit row
// 46726) ends naming the call's state: the worker's line says how the last
// step finished, and the gateway's booking adds the prompt, cached and
// output tokens, the cost, the hold and the fitting action, an output at
// its limit a shorter output, one below it a smaller context. The line
// reaches the next brief and the card, its last line and its history.
func TestRunSessionNamesACostHoldCrossing(t *testing.T) {
	p := &opencodeParser{}
	p.line([]byte(`{"type":"step_finish","sessionID":"ses_x","part":{"reason":"length","tokens":{"input":13784,"output":24576,"reasoning":0,"cache":{"read":8896,"write":0}},"cost":0}}`), func(harness.Event) {})
	said := p.summary()
	if want := `the agent's last model step finished on "length" after 24576 output and 0 reasoning tokens, not on "stop"`; said != want {
		t.Fatalf("worker summary = %q, want %q", said, want)
	}
	for name, tc := range map[string]struct {
		call *OverHoldCall
		want string
	}{
		"the output at its limit": {&OverHoldCall{Alias: "coding-frontier", PromptTokens: 22680, CachedTokens: 8896, OutputTokens: 24576, OutputLimit: 24576, HoldEUR: 0.122078, CostEUR: 0.129745},
			said + "; a model call on coding-frontier crossed its cost hold: 22680 prompt tokens (8896 cached) and 24576 output tokens cost 0.129745 EUR against a hold of 0.122078 EUR; " +
				"its output ran to the limit of 24576 tokens, so a shorter output fits: write the change in smaller pieces"},
		"the prompt over its price": {&OverHoldCall{Alias: "coding-frontier", PromptTokens: 90000, OutputTokens: 3000, OutputLimit: 24576, HoldEUR: 0.131, CostEUR: 0.1392},
			said + "; a model call on coding-frontier crossed its cost hold: 90000 prompt tokens (0 cached) and 3000 output tokens cost 0.139200 EUR against a hold of 0.131000 EUR; " +
				"its prompt cost more than the hold priced, so a smaller context fits: read fewer and smaller files"},
		"no call crossed its hold": {nil, said},
	} {
		t.Run(name, func(t *testing.T) {
			runner, _, store := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
			if tc.call != nil {
				store.overHolds = map[string]OverHoldCall{"t2": *tc.call}
			}
			res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 13, Model: "m"}, nil, reads(0, 2)+resultLine(false, said))
			if err != nil || res.Outcome != OutcomeNoPush || res.Summary != tc.want {
				t.Fatalf("result = (%+v, %v), want no_push with summary %q", res, err, tc.want)
			}
			line := "previous session ended without a pushed commit: " + tc.want
			if got := res.Findings[len(res.Findings)-1]; got != line {
				t.Fatalf("line = %q, want %q", got, line)
			}
			var run taskRun
			run.remember("session", 13, res.Findings)
			card := run.sessionCapCard(res.Summary, nil)
			if !strings.Contains(card, "\nlast: "+tc.want+"\n") || !strings.HasSuffix(card, "\n- session, attempt 13: "+line) {
				t.Fatalf("card = %q, want the state as its last line and its newest finding", card)
			}
		})
	}
}

// A session the gateway refused at the project's month cap waits on a
// human with the refusal, which names the file a human raises the cap in.
func TestProjectCapRefusalEscalates(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "add the stats page", Repository: "sf/ai-software-factory-code", SHA: "abc123", BudgetEUR: 2}
	runner, _, store := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
	refusal := "free headroom -0.000100 EUR (task 1.900000, day 19.000000, month 49.000000, project -0.000100) fits 0 output tokens after at most 4000 input tokens, below the minimum call of 1024; " +
		"the project cap budget.month_eur in projects/linkshort.yaml is spent, and only a human raises it, by a commit"
	store.capRefusals = map[string]CapRefusal{"t1": {Refusal: policy.Refusal{Decision: policy.RefusedProjectCap, Reason: refusal}}}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/coding-cheap"}, nil,
		resultLine(false, "the model call failed: 402 budget_exceeded"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "the project's month cap is reached: the gateway refused this task's spend (refused_project_cap): " + refusal; res.Outcome != OutcomeEscalate || res.Reason != want {
		t.Fatalf("result = %+v, want escalate with %q", res, want)
	}
	if got := escalationSource(res.Reason); got != "project_cap" {
		t.Fatalf("escalation source = %q, want project_cap", got)
	}
}
