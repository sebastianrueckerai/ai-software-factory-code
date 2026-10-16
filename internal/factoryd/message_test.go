package factoryd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// fakeWorkerEndpoint is a worker's message endpoint answering status. It
// records POST /message's text and POST /push's token; onPush runs at
// each POST /push, and a token taken (204) lets a log held at tokenLogs
// go on.
type fakeWorkerEndpoint struct {
	mu     sync.Mutex
	status int
	auth   []string
	texts  []string
	onPush func()
	// onTool answers POST /tool with its status and body; tools records
	// every ask as the tool's name with its args
	onTool func(name string, args map[string]any) (int, string)
	tools  []string
}

func (f *fakeWorkerEndpoint) start(t *testing.T, r *Runner) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Text     string         `json:"text"`
			Token    string         `json:"token"`
			Session  string         `json:"session"`
			Record   string         `json:"record"`
			Handover string         `json:"handover"`
			Name     string         `json:"name"`
			Args     map[string]any `json:"args"`
		}
		b, _ := io.ReadAll(req.Body)
		json.Unmarshal(b, &in)
		f.mu.Lock()
		f.auth = append(f.auth, req.Header.Get("Authorization"))
		f.texts = append(f.texts, req.Method+" "+req.URL.Path+" "+in.Text+in.Token+in.Session+in.Record+in.Handover+in.Name)
		status := f.status
		f.mu.Unlock()
		if req.URL.Path == "/tool" {
			if args, err := json.Marshal(in.Args); err == nil {
				f.mu.Lock()
				f.tools = append(f.tools, in.Name+" "+string(args))
				f.mu.Unlock()
			}
			code, body := http.StatusOK, `{"output":"","cut":false}`
			if f.onTool != nil {
				f.mu.Lock()
				code, body = f.onTool(in.Name, in.Args)
				f.mu.Unlock()
			}
			w.WriteHeader(code)
			io.WriteString(w, body)
			return
		}
		if req.URL.Path == "/push" {
			if f.onPush != nil {
				f.onPush()
			}
			if kube, ok := r.kube.(*fakeKube); ok && status == http.StatusNoContent {
				kube.takeToken.Do(func() { close(kube.tokenTaken) })
			}
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	r.workerPort, _ = strconv.Atoi(port)
}

func threeReads() string {
	var lines []string
	for i := 0; i < 3; i++ {
		lines = append(lines, envLine(harness.Event{Kind: harness.EventToolCall, Time: time.Now(),
			Detail: map[string]any{"tool": "read", "status": "completed", "args": map[string]any{"filePath": "/w/a.go"}, "args_sha256": "h1"}}))
	}
	return strings.Join(lines, "\n") + "\n"
}

const readNudge = "You repeated the same call three times (read). Change approach, then continue."

// Three identical calls in a row: the running session gets the nudge
// through its worker's endpoint with the session's bearer, the timeline a
// nudge step, and the session runs on to its push.
func TestRunSessionNudgesTheRunningSession(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), kube, objects)
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)

	logs := threeReads() + envLine(harness.Event{Kind: harness.EventFileChange, Time: time.Now(), Detail: map[string]any{"path": "a.go"}}) + "\n" +
		pushedLine("c0ffee") + "\n" + resultLine(true, "pushed branch factory/t2 at c0ffee")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed {
		t.Fatalf("result = (%+v, %v), want pushed after the nudge", res, err)
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if len(worker.texts) != 2 || worker.texts[0] != "POST /brief " || worker.texts[1] != "POST /message "+readNudge {
		t.Fatalf("worker got %q", worker.texts)
	}
	// the pod carries the bearer's hash, never the bearer
	var hash string
	for _, e := range kube.podOrder[0].spec["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"].([]map[string]any) {
		if e["name"] == "MESSAGE_TOKEN_SHA256" {
			hash, _ = e["value"].(string)
		}
		if v, _ := e["value"].(string); v != "" && "Bearer "+v == worker.auth[0] {
			t.Fatalf("the pod's %s carries the bearer", e["name"])
		}
	}
	token, ok := strings.CutPrefix(worker.auth[0], "Bearer ")
	if !ok || len(token) < 26 || messageTokenHash(token) != hash {
		t.Fatalf("bearer %q does not match the pod's hash %q", worker.auth[0], hash)
	}
	tl, _ := objects.Get(t.Context(), "sessions/t2/timeline.jsonl")
	var step map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(tl)), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) == nil && e["step"] == "nudge" {
			step = e
		}
	}
	if step == nil || step["tool"] != "read" || step["count"] != float64(3) || step["session"] != res.SessionID || step["error"] != nil {
		t.Fatalf("timeline nudge step = %v", step)
	}
}

// Three delivered nudges with no file change in between end the session
// as a redirect on loop, no supervisor call: the stopped worker pushes its
// work in progress, the timeline holds the loop, and the next session's
// brief the line naming what the session repeated.
func TestRunSessionLoopEndsAfterThreeNudges(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), kube, objects)
	runner.stopWait = 10 * time.Second
	worker, order := watchStop(t, runner, kube)
	var logs string
	for _, p := range []string{"/w/a.go", "/w/b.go", "/w/c.go"} {
		for range 3 {
			logs += envLine(harness.Event{Kind: harness.EventToolCall, Time: time.Now(),
				Detail: map[string]any{"tool": "read", "status": "completed", "args": map[string]any{"filePath": p}}}) + "\n"
		}
	}
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs+stoppedWorker("t2"))
	if err != nil || res.Outcome != OutcomeRedirect || res.Trip != string(TripLoop) || res.Commit != "c-wip" {
		t.Fatalf("result = (%+v, %v), want a redirect on loop with the work in progress c-wip", res, err)
	}
	if got := order(); !slices.Equal(got, []string{"delete pod", "POST /push"}) {
		t.Fatalf("order = %v, want the push after the stop", got)
	}
	want := []string{"POST /brief ", "POST /message " + readNudge, "POST /message " + readNudge, "POST /message " + readNudge, "POST /push tok-task-t2"}
	if !slices.Equal(worker.texts, want) {
		t.Fatalf("worker got %q, want three nudges and the push token", worker.texts)
	}
	detail := "three nudges without new file content in between: read `{\"filePath\":\"/w/a.go\"}` 3 times; " +
		"read `{\"filePath\":\"/w/b.go\"}` 3 times; read `{\"filePath\":\"/w/c.go\"}` 3 times"
	line := "the previous session ended in a loop after " + detail
	if !slices.Contains(res.Findings, line) || !strings.Contains(objectiveText(harness.TaskBrief{Findings: res.Findings}), "\n- "+line+"\n") {
		t.Fatalf("findings = %q, want the loop line %q", res.Findings, line)
	}
	tl, _ := objects.Get(t.Context(), "sessions/t2/timeline.jsonl")
	var step map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(tl)), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(l), &e) == nil && e["step"] == "supervisor" {
			step = e
		}
	}
	if step == nil || step["detector"] != "loop" || step["trip"] != detail || step["decision"] != "redirect" || step["called"] != false {
		t.Fatalf("timeline supervisor step = %v", step)
	}
}

// A harness without messages (the worker's 501) gets today's redirect: the
// session stops, and the next one starts with the nudge as its guidance;
// no supervisor call.
func TestRunSessionNudgeFallsBackToTheRedirect(t *testing.T) {
	objects := &fakeObjectStore{}
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), objects)
	worker := &fakeWorkerEndpoint{status: http.StatusNotImplemented}
	worker.start(t, runner)

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, threeReads()+resultLine(false, "stopped"))
	if err != nil || res.Outcome != OutcomeRedirect || res.Trip != string(TripSameCall) {
		t.Fatalf("result = (%+v, %v), want a redirect on same_call", res, err)
	}
	found := false
	for _, f := range res.Findings {
		found = found || strings.Contains(f, readNudge)
	}
	if !found {
		t.Fatalf("findings = %q, want the nudge as the guidance", res.Findings)
	}
	tl, _ := objects.Get(t.Context(), "sessions/t2/timeline.jsonl")
	if !strings.Contains(string(tl), `"fallback":"redirect"`) || !strings.Contains(string(tl), `"step":"nudge"`) {
		t.Fatalf("timeline = %s", tl)
	}
}

// A nudge that cannot be delivered asks the supervisor, as a same_call
// trip; the supervisor's message cannot be delivered either, so the
// session stops with it as the guidance.
func TestRunSessionUndeliveredNudgeAsksTheSupervisor(t *testing.T) {
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
	f := &fakeLiteLLM{content: `{"action":"message","guidance":"read b.go instead"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)
	worker := &fakeWorkerEndpoint{status: http.StatusBadGateway}
	worker.start(t, runner)

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, threeReads()+resultLine(false, "stopped"))
	if err != nil || res.Outcome != OutcomeRedirect || res.Trip != string(TripSameCall) {
		t.Fatalf("result = (%+v, %v), want the stop on same_call", res, err)
	}
	if !slices.Contains(res.Findings, "supervisor guidance: read b.go instead") {
		t.Fatalf("findings = %q, want the message as the guidance", res.Findings)
	}
	if len(f.chats) != 1 {
		t.Fatalf("supervisor calls = %d, want 1", len(f.chats))
	}
}

// The sandbox policy admits one ingress: factoryd's pod, on the worker's
// message port. Cilium denies every other ingress to a pod once a policy
// selecting it has an ingress section.
func TestWorkerPolicyAdmitsOnlyFactorydOnTheMessagePort(t *testing.T) {
	spec := (&Runner{cfg: testRunnerConfig()}).workerEgressSpec(nil)
	want := []any{map[string]any{
		"fromEndpoints": []any{map[string]any{"matchLabels": map[string]string{
			"k8s:io.kubernetes.pod.namespace": "factory",
			"app":                             "factoryd",
		}}},
		"toPorts": []any{map[string]any{
			"ports": []any{map[string]any{"port": "8091", "protocol": "TCP"}},
		}},
	}}
	if !reflect.DeepEqual(spec["ingress"], want) {
		t.Fatalf("ingress = %#v, want factoryd on 8091 only", spec["ingress"])
	}
	if sel := spec["endpointSelector"].(map[string]any)["matchLabels"]; !reflect.DeepEqual(sel, map[string]string{"app.kubernetes.io/name": "opencode-worker"}) {
		t.Fatalf("selector = %v", sel)
	}
}

// A session that ended between its last call and the nudge (the worker's
// 409) needs none: no trip, the session's own end stands.
func TestRunSessionNudgeAfterTheSessionEndedIsANoOp(t *testing.T) {
	objects := &fakeObjectStore{}
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), objects)
	worker := &fakeWorkerEndpoint{status: http.StatusConflict}
	worker.start(t, runner)

	logs := threeReads() + pushedLine("c0ffee") + "\n" + resultLine(true, "pushed branch factory/t2 at c0ffee")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed {
		t.Fatalf("result = (%+v, %v), want pushed", res, err)
	}
	tl, _ := objects.Get(t.Context(), "sessions/t2/timeline.jsonl")
	if !strings.Contains(string(tl), `"error":"no session is running"`) {
		t.Fatalf("timeline = %s", tl)
	}
}

func reads(from, n int) string {
	var s string
	for i := from; i < from+n; i++ {
		s += envLine(harness.Event{Kind: harness.EventToolCall, Time: time.Now(),
			Detail: map[string]any{"tool": "read", "status": "completed", "args": map[string]any{"filePath": fmt.Sprint("/w/", i, ".go")}}}) + "\n"
	}
	return s
}

func commands(from, n int) string {
	var s string
	for i := from; i < from+n; i++ {
		s += envLine(harness.Event{Kind: harness.EventCommand, Time: time.Now(), Detail: map[string]any{"command": fmt.Sprint("ls ", i), "exit": 0}}) + "\n"
	}
	return s
}

// The supervisor's message reaches the running worker through the
// nudge's path; the pod runs on and the tripped detector is reset, so a
// third read makes no second trip.
func TestMessageKeepsSessionRunning(t *testing.T) {
	kube := newFakeKube()
	runner, hub, _ := newTestRunner(t, supervisedTask("t2"), kube, &fakeObjectStore{})
	runner.cfg.Supervisor.RepetitionEvents = 2
	f := &fakeLiteLLM{content: `{"action":"message","guidance":"use the store's New"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)

	logs := reads(0, 2) + reads(2, 1) +
		envLine(harness.Event{Kind: harness.EventFileChange, Time: time.Now(), Detail: map[string]any{"path": "a.go"}}) + "\n" +
		pushedLine("c1") + "\n" + resultLine(true, "pushed")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed || res.Commit != "c1" {
		t.Fatalf("result = (%+v, %v), want pushed c1", res, err)
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if len(worker.texts) != 2 || worker.texts[0] != "POST /brief " || worker.texts[1] != "POST /message use the store's New" {
		t.Fatalf("worker got %q", worker.texts)
	}
	if len(f.chats) != 1 {
		t.Fatalf("supervisor calls = %d, want 1", len(f.chats))
	}
	if len(kube.deletedPods) != 1 {
		t.Fatalf("deleted pods = %v, want only the cleanup delete", kube.deletedPods)
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "**message**") || !strings.Contains(hub.comments[0], "use the store's New") {
		t.Fatalf("comments = %q", hub.comments)
	}
}

// After a delivered message, 24 commands without a write trip repetition:
// the session ends as retry_stronger without a second supervisor call.
func TestTripAfterMessageWithoutWriteIsStuck(t *testing.T) {
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
	f := &fakeLiteLLM{content: `{"action":"message","guidance":"use the store's New"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)

	logs := commands(0, 24) + commands(24, 24) + pushedLine("c-wip") + "\n" + resultLine(false, "stopped")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomeRetryStronger || res.Trip != string(TripRepetition) || res.Commit != "c-wip" {
		t.Fatalf("result = (%+v, %v), want retry_stronger on repetition with c-wip", res, err)
	}
	if len(f.chats) != 1 {
		t.Fatalf("supervisor calls = %d, want still 1", len(f.chats))
	}
	found := false
	for _, l := range res.Findings {
		found = found || strings.HasPrefix(l, "stuck after the supervisor's message: repetition: 24 tool calls and commands without a file change")
	}
	if !found {
		t.Fatalf("findings = %q", res.Findings)
	}
}

// A write after the message: the next trip asks the supervisor again.
func TestTripAfterMessageWithWriteAsks(t *testing.T) {
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
	f := &fakeLiteLLM{content: `{"action":"message","guidance":"use the store's New"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)

	logs := commands(0, 24) +
		envLine(harness.Event{Kind: harness.EventFileChange, Time: time.Now(), Detail: map[string]any{"path": "a.go"}}) + "\n" +
		commands(24, 24) + pushedLine("c1") + "\n" + resultLine(true, "pushed")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed {
		t.Fatalf("result = (%+v, %v), want pushed", res, err)
	}
	if len(f.chats) != 2 {
		t.Fatalf("supervisor calls = %d, want 2", len(f.chats))
	}
}

// A message the worker cannot take (501) falls back to the stop, with the
// guidance as the next session's finding.
func TestUndeliveredMessageStops(t *testing.T) {
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
	runner.cfg.Supervisor.RepetitionEvents = 2
	f := &fakeLiteLLM{content: `{"action":"message","guidance":"use the store's New"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)
	worker := &fakeWorkerEndpoint{status: http.StatusNotImplemented}
	worker.start(t, runner)

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, reads(0, 2)+resultLine(false, "stopped"))
	if err != nil || res.Outcome != OutcomeRedirect || res.Trip != string(TripRepetition) {
		t.Fatalf("result = (%+v, %v), want the stop on repetition", res, err)
	}
	if !slices.Contains(res.Findings, "supervisor guidance: use the store's New") {
		t.Fatalf("findings = %q, want the guidance", res.Findings)
	}
}

// No half-way check-in: a session at 60% of its agent time and 60% of its
// budget, with no check-in configured, asks no supervisor.
func TestNoHalfWay(t *testing.T) {
	task := supervisedTask("t2")
	task.SpentEUR = 0.18 // of 0.3
	runner, _, _ := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
	runner.cfg.Supervisor.RepetitionEvents = 1000
	logs := envLine(harness.Event{Kind: harness.EventModelCall, Time: time.Now()}) + "\n" + resultLine(false, "x")
	// the activity deadline is 10 minutes out: 15 of 25 minutes are used
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m", AgentTime: 25 * time.Minute}, nil, logs)
	if err != nil || res.Trip != "" {
		t.Fatalf("result = (%+v, %v), want no trip", res, err)
	}
}

// A stuck end is commented on the issue with the stuck finding (review
// minor 2); a second call sees the session's earlier message (minor 4).
func TestStuckEndCommentedAndEarlierMessagesShown(t *testing.T) {
	runner, hub, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
	f := &fakeLiteLLM{content: `{"action":"message","guidance":"use the store's New"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)

	logs := commands(0, 24) +
		envLine(harness.Event{Kind: harness.EventFileChange, Time: time.Now(), Detail: map[string]any{"path": "a.go"}}) + "\n" +
		commands(24, 24) + commands(48, 24) + resultLine(false, "stopped")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomeRetryStronger || len(f.chats) != 2 {
		t.Fatalf("result = (%+v, %v) after %d calls, want retry_stronger after 2", res, err, len(f.chats))
	}
	var in SupervisorInput
	msgs := f.chats[1]["messages"].([]any)
	json.Unmarshal([]byte(msgs[1].(map[string]any)["content"].(string)), &in)
	if !slices.Equal(in.Messages, []string{"use the store's New"}) {
		t.Fatalf("second call's messages = %q, want the first message", in.Messages)
	}
	if first := f.chats[0]["messages"].([]any)[1].(map[string]any)["content"].(string); strings.Contains(first, "use the store's New") {
		t.Fatal("the first call already saw the message")
	}
	if n := len(hub.comments); n != 3 || !strings.Contains(hub.comments[2], "**retry_stronger**") ||
		!strings.Contains(hub.comments[2], "stuck after the supervisor's message: repetition") {
		t.Fatalf("comments = %q, want two messages and the stuck end", hub.comments)
	}
}

// Roadmap 201: a session's first supervisor call reads the whole brief,
// its carried findings the newest 60; a later call the issue's text and
// the answers that amend it, without the findings, and the guidance it
// gave in messages only, not again as a supervisor event of the record.
// The objective comes without the intake's Size line.
func TestSupervisorReadsTheBriefOncePerSession(t *testing.T) {
	task := supervisedTask("t2")
	task.Objective = "make loopcheck pass\n\nIt loops.\n\nSize: small"
	runner, _, _ := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
	f := &fakeLiteLLM{content: `{"action":"message","guidance":"use the store's New"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)
	(&fakeWorkerEndpoint{status: http.StatusNoContent}).start(t, runner)
	var findings []string
	for i := range 70 {
		findings = append(findings, fmt.Sprint("ran x", i))
	}
	answers := []string{`sebastian, to "Which store?": the default one`}
	logs := commands(0, 24) +
		envLine(harness.Event{Kind: harness.EventFileChange, Time: time.Now(), Detail: map[string]any{"path": "a.go"}}) + "\n" +
		commands(24, 24) + commands(48, 24) + resultLine(false, "stopped")
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m", Findings: findings, Answers: answers}, nil, logs); err != nil || len(f.chats) != 2 {
		t.Fatalf("session: %v after %d calls, want 2", err, len(f.chats))
	}
	in := make([]SupervisorInput, 2)
	for i := range in {
		if err := json.Unmarshal([]byte(f.chats[i]["messages"].([]any)[1].(map[string]any)["content"].(string)), &in[i]); err != nil {
			t.Fatal(err)
		}
		if in[i].Brief.Objective != "make loopcheck pass\n\nIt loops." || !slices.Equal(in[i].Brief.Answers, answers) {
			t.Errorf("call %d: brief = %+v, want the objective without its Size line and the answer", i+1, in[i].Brief)
		}
		if strings.Contains(string(in[i].Record), `"kind":"supervisor"`) {
			t.Errorf("call %d: record holds a supervisor event: %s", i+1, in[i].Record)
		}
	}
	if !slices.Equal(in[0].Brief.Findings, findings[10:]) || in[1].Brief.Findings != nil {
		t.Fatalf("findings: first call %d (want the newest 60), second %d (want none)", len(in[0].Brief.Findings), len(in[1].Brief.Findings))
	}
	if in[0].Messages != nil || !slices.Equal(in[1].Messages, []string{"use the store's New"}) {
		t.Fatalf("messages = %q, then %q; want none, then the first call's guidance", in[0].Messages, in[1].Messages)
	}
}

func pushRequestLine(commit string) string {
	b, _ := json.Marshal(harness.WorkerEnvelope{Type: "push", Push: &harness.PushRequest{Commit: commit}})
	return string(b)
}

// writeMints are the write tokens minted so far.
func writeMints(h *forgejoHub) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, call := range h.calls {
		if strings.Contains(call, " write:repository ") {
			out = append(out, call)
		}
	}
	return out
}

// The push token is minted when the worker asks for it, once every agent
// process in the pod is dead, not at the session's start: write on the
// bot's fork, posted to the worker's POST /push with the session's bearer.
func TestPushTokenMintedAtThePushRequest(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/x", SHA: "abc123", BudgetEUR: 2}
	kube := newFakeKube()
	runner, hub, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	var atStart, atPost []string
	kube.onCreatePod = func() { atStart = writeMints(hub) }
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent, onPush: func() { atPost = writeMints(hub) }}
	worker.start(t, runner)

	logs := envLine(harness.Event{Kind: harness.EventFileChange, Time: time.Now(), Detail: map[string]any{"path": "a.go"}}) + "\n" +
		envLine(harness.Event{Kind: harness.EventCommand, Time: time.Now(), Detail: map[string]any{"command": "go test ./...", "exit": 0}}) + "\n" +
		pushRequestLine("c1") + "\n" + tokenLogs + pushedLine("c1") + "\n" + resultLine(true, "pushed c1")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed || res.Commit != "c1" {
		t.Fatalf("result = (%+v, %v), want pushed c1", res, err)
	}
	want := []string{"mint task-t1 write:repository factory-agent/x"}
	if len(atStart) != 0 || !slices.Equal(atPost, want) || !slices.Equal(writeMints(hub), want) {
		t.Fatalf("write mints at the pod's start %v, at the post %v, in all %v; want none, then %v", atStart, atPost, writeMints(hub), want)
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if !slices.Equal(worker.texts, []string{"POST /brief ", "POST /push tok-task-t1"}) {
		t.Fatalf("worker got %q, want the minted token on POST /push", worker.texts)
	}
	// the session's message bearer, the one whose hash the pod holds
	token, ok := strings.CutPrefix(worker.auth[0], "Bearer ")
	if hash := podEnv(kube.podAt(0))["MESSAGE_TOKEN_SHA256"]; !ok || len(token) < 26 || messageTokenHash(token) != hash {
		t.Fatalf("POST /push carried %q, want the bearer of the pod's hash %q", worker.auth[0], hash)
	}
}

// One failed answer — a Forgejo mint blip on the first ask — costs the
// worker a re-ask, not the push: the tap answers every ask, and only the
// answered one posts a token.
func TestPushTokenOneFailedAnswerThenASuccess(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/x", SHA: "abc123", BudgetEUR: 2}
	kube := newFakeKube()
	runner, hub, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	hub.failWrites = 1
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "m"}, nil,
		pushRequestLine("c1")+"\n"+pushRequestLine("c1")+"\n"+tokenLogs+pushedLine("c1")+"\n"+resultLine(true, "pushed c1"))
	if err != nil || res.Outcome != OutcomePushed || res.Commit != "c1" {
		t.Fatalf("result = (%+v, %v), want pushed c1 after the re-ask", res, err)
	}
	if asks := len(writeMints(hub)); asks != 2 {
		t.Fatalf("write mints = %v, want two: one failed, one answered", writeMints(hub))
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if !slices.Equal(worker.texts, []string{"POST /brief ", "POST /push tok-task-t1"}) {
		t.Fatalf("worker got %q, want one token, posted on the answered ask", worker.texts)
	}
}

// Neither the worker container nor its namespace's secret holds a push
// token: the container's env takes no secret, and task-git holds the clone
// init container's read token only.
func TestWorkerContainerCarriesNoForgejoToken(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/x", SHA: "abc123", BudgetEUR: 2}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "m"}, nil,
		pushedLine("c1")+"\n"+resultLine(true, "pushed c1")); err != nil {
		t.Fatal(err)
	}
	worker := kube.podAt(0).spec["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	for _, e := range worker["env"].([]map[string]any) {
		if name := e["name"].(string); e["valueFrom"] != nil || strings.HasSuffix(name, "_TOKEN") {
			t.Errorf("worker container env %s = %v", name, e)
		}
	}
	if keys := slices.Sorted(maps.Keys(kube.secrets["factory-agents-t1/task-git"])); !slices.Equal(keys, []string{"REPO_TOKEN"}) {
		t.Fatalf("task-git keys = %v, want [REPO_TOKEN]", keys)
	}
}

// stoppedWorker is what a worker prints after its SIGTERM: it commits its
// work in progress, asks for the push token, pushes and reports.
func stoppedWorker(task string) string {
	return sigtermLogs + pushRequestLine("c-wip") + "\n" + tokenLogs + pushedLine("c-wip") + "\n" +
		resultLine(false, "stopped; work in progress pushed to factory/"+task+" at c-wip") + "\n"
}

// watchStop serves a worker endpoint that takes the token and records, in
// order, the pod's deletion (the worker's SIGTERM) and each POST /push.
func watchStop(t *testing.T, runner *Runner, kube *fakeKube) (*fakeWorkerEndpoint, func() []string) {
	var mu sync.Mutex
	var order []string
	note := func(what string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, what)
	}
	kube.onDeletePod = func() { note("delete pod") }
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent, onPush: func() { note("POST /push") }}
	worker.start(t, runner)
	return worker, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(order)
	}
}

// A supervisor's stop deletes the pod; the stopped worker's push request
// is answered within its grace period, and its work in progress reaches
// the next session.
func TestSupervisorStopAnswersThePushRequest(t *testing.T) {
	kube := newFakeKube()
	runner, hub, _ := newTestRunner(t, supervisedTask("t2"), kube, &fakeObjectStore{})
	f := &fakeLiteLLM{content: `{"action":"escalate","reason":"the check needs a file the agent never created"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)
	runner.stopWait = 10 * time.Second
	worker, order := watchStop(t, runner, kube)

	logs := loopFailure(1) + loopFailure(2) + loopFailure(3) + stoppedWorker("t2")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomeEscalate || res.Commit != "c-wip" {
		t.Fatalf("result = (%+v, %v), want escalate with the work in progress c-wip", res, err)
	}
	if got := order(); !slices.Equal(got, []string{"delete pod", "POST /push"}) {
		t.Fatalf("order = %v, want the push token posted after the stop", got)
	}
	if !slices.Equal(worker.texts, []string{"POST /brief ", "POST /push tok-task-t2"}) || !slices.Equal(writeMints(hub), []string{"mint task-t2 write:repository factory-agent/repo"}) {
		t.Fatalf("worker got %q, write mints %v", worker.texts, writeMints(hub))
	}
	if len(hub.revoked) != 2 {
		t.Fatalf("revoked = %v, want the read and the push token revoked", hub.revoked)
	}
}

// The wall clock stops the session the same way.
func TestWallClockStopAnswersThePushRequest(t *testing.T) {
	task := Task{ID: "t5", Status: "running", Objective: "slow", Repository: "sf/repo"}
	kube := newFakeKube()
	runner, hub, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	runner.cfg.Supervisor = SupervisorConfig{NoProgressAfter: 3 * time.Second, RepetitionEvents: 24}
	setLimits(runner, func(l *policy.Limits) { l.AgentTime = 300 * time.Millisecond })
	runner.stopWait = 10 * time.Second
	worker, order := watchStop(t, runner, kube)

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t5", Attempt: 1, Model: "m"}, nil, stoppedWorker("t5"))
	if err != nil || res.Outcome != OutcomeWallClock || res.Commit != "c-wip" {
		t.Fatalf("result = (%+v, %v), want wall_clock with the work in progress c-wip", res, err)
	}
	if got := order(); !slices.Equal(got, []string{"delete pod", "POST /push"}) {
		t.Fatalf("order = %v, want the push token posted after the stop", got)
	}
	if !slices.Equal(worker.texts, []string{"POST /brief ", "POST /push tok-task-t5"}) || len(hub.revoked) != 2 {
		t.Fatalf("worker got %q, revoked %v", worker.texts, hub.revoked)
	}
}

// factoryd's own graceful shutdown detaches (roadmap 222, item 2): the
// worker is not stopped and asks for no push token; the activity returns
// for the retry with the pod running on.
func TestShutdownDetachesWithoutAStop(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, hub, store := newTestRunner(t, supervisedTask("t2"), kube, objects)
	worker, order := watchStop(t, runner, kube)
	stop := make(chan struct{})
	kube.onCreatePod = func() { go func() { time.Sleep(20 * time.Millisecond); close(stop) }() }

	logs := loopFailure(1) + stoppedWorker("t2")
	_, err := runSessionIn(t, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, []string{logs}, func(env *testsuite.TestActivityEnvironment) {
		env.SetWorkerStopChannel(stop)
	})
	if err == nil || !strings.Contains(err.Error(), "factoryd stopping") {
		t.Fatalf("err = %v, want the retryable stop", err)
	}
	// the brief may have landed before the stop; nothing else may have
	briefed := len(worker.texts) == 0 || (len(worker.texts) == 1 && worker.texts[0] == "POST /brief ")
	if got := order(); len(got) != 0 || !briefed || len(hub.revoked) != 0 {
		t.Fatalf("order = %v, worker got %q, revoked %v; want the worker left running", got, worker.texts, hub.revoked)
	}
	if _, ok := store.liveSessions["t2"]; !ok {
		t.Fatal("no live row for the retry to reattach to")
	}
}

// A worker that finishes while the supervisor is asked gets its push token
// at once: the log follower answers the request, not the session loop,
// which waits for the supervisor's answer.
func TestPushAnsweredDuringASupervisorCall(t *testing.T) {
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), kube, &fakeObjectStore{})
	pushed := make(chan struct{})
	var once sync.Once
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent, onPush: func() { once.Do(func() { close(pushed) }) }}
	worker.start(t, runner)
	var sawPush atomic.Bool
	sup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/key/list":
			io.WriteString(w, `{"keys":[],"total_pages":1}`)
		case "/v1/chat/completions":
			select {
			case <-pushed:
				sawPush.Store(true)
			case <-time.After(3 * time.Second):
			}
			out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": `{"action":"continue"}`}}}})
			w.Write(out)
		}
	}))
	t.Cleanup(sup.Close)
	runner.supervisor = testSupervisorModel(sup.URL)

	logs := loopFailure(1) + loopFailure(2) + loopFailure(3) +
		pushRequestLine("c1") + "\n" + tokenLogs + pushedLine("c1") + "\n" + resultLine(true, "pushed branch factory/t2 at c1")
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil || res.Outcome != OutcomePushed || res.Commit != "c1" {
		t.Fatalf("result = (%+v, %v), want pushed c1", res, err)
	}
	if !sawPush.Load() {
		t.Fatal("the push request waited for the supervisor's answer")
	}
}

// A writer's steer reaches the task's running session on the message path:
// the worker gets the text, marked as the writer's, with the session's
// bearer, and the timeline a message step naming the writer. A task with
// no session running here is refused.
func TestMessageSessionReachesTheRunningSession(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), kube, objects)
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)
	kube.pods["agent-t2/worker-t2"] = &fakePod{ns: "agent-t2", name: "worker-t2"}
	_, unregister := runner.liveSession("t2", sessionWorker{session: "s1", ns: "agent-t2", pod: "worker-t2", token: "bearer-1"})
	defer unregister()
	if err := runner.MessageSession(t.Context(), "t2", "anna", "rebase on main first"); err != nil {
		t.Fatalf("MessageSession = %v", err)
	}
	worker.mu.Lock()
	texts, auth := worker.texts, worker.auth
	worker.mu.Unlock()
	if len(texts) != 1 || !strings.HasPrefix(texts[0], "POST /message You are the agent session of card t2") ||
		!strings.HasSuffix(texts[0], "A message from anna, a writer of this project: rebase on main first") ||
		!slices.Equal(auth, []string{"Bearer bearer-1"}) {
		t.Fatalf("worker got %q with %q", texts, auth)
	}
	b, err := objects.Get(t.Context(), "sessions/t2/timeline.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var step map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(b), &step); err != nil {
		t.Fatalf("timeline %q: %v", b, err)
	}
	if step["step"] != "message" || step["session"] != "s1" || step["from"] != "anna" || step["error"] != nil {
		t.Fatalf("timeline step = %v, want message of session s1 from anna", step)
	}
	if err := runner.MessageSession(t.Context(), "t3", "anna", "hello"); !errors.Is(err, ErrNoLiveSession) {
		t.Fatalf("MessageSession to a task without a session = %v, want ErrNoLiveSession", err)
	}
}

// The first message a writer sends through the card's chat carries the
// factory basics, once: the session reads them before the writer's text.
// Later messages go in bare, and so does every supervisor message.
func TestFirstHumanMessageCarriesTheFactoryBasics(t *testing.T) {
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), kube, &fakeObjectStore{})
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)
	kube.pods["agent-t2/worker-t2"] = &fakePod{ns: "agent-t2", name: "worker-t2"}
	_, unregister := runner.liveSession("t2", sessionWorker{session: "s1", ns: "agent-t2", pod: "worker-t2", token: "bearer-1"})
	defer unregister()
	if err := runner.MessageSession(t.Context(), "t2", "anna", "rebase on main first"); err != nil {
		t.Fatalf("MessageSession = %v", err)
	}
	if err := runner.MessageSession(t.Context(), "t2", "anna", "and push"); err != nil {
		t.Fatalf("second MessageSession = %v", err)
	}
	worker.mu.Lock()
	texts := slices.Clone(worker.texts)
	worker.mu.Unlock()
	if len(texts) != 2 {
		t.Fatalf("worker got %d messages, want 2", len(texts))
	}
	if !strings.HasPrefix(texts[0], "POST /message You are the agent session of card t2") ||
		!strings.Contains(texts[0], "Signals and members file cards") ||
		!strings.HasSuffix(texts[0], "A message from anna, a writer of this project: rebase on main first") {
		t.Fatalf("first message = %q", texts[0])
	}
	if texts[1] != "POST /message A message from anna, a writer of this project: and push" {
		t.Fatalf("second message = %q", texts[1])
	}
	if err := runner.message(t.Context(), supervisedTask("t2"), sessionSpec{id: "s1", messageToken: "bearer-1"}, "agent-t2", "worker-t2", "periodic check-in"); err != nil {
		t.Fatalf("supervisor message = %v", err)
	}
	worker.mu.Lock()
	texts = slices.Clone(worker.texts)
	worker.mu.Unlock()
	if len(texts) != 3 || texts[2] != "POST /message periodic check-in" {
		t.Fatalf("supervisor messages = %q, want the check-in bare", texts[2:])
	}
}

// The check-in's two deliveries to the pod: the parent session's record
// on POST /records and the handover on POST /brief, both with the
// session's bearer as /message's.
func TestRecordsAndBriefWorker(t *testing.T) {
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, Task{ID: "t1", Status: "running", Objective: "o", Repository: "sf/x", SHA: "abc123", BudgetEUR: 2}, kube, &fakeObjectStore{})
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, runner)
	if err := kube.CreatePod(t.Context(), "ns-t1", "worker-t1", nil); err != nil {
		t.Fatal(err)
	}
	record := "{\"kind\":\"model_call\"}\n{\"kind\":\"tool_call\"}\n"
	if err := runner.recordsWorker(t.Context(), "ns-t1", "worker-t1", "bearer-1", "s-6", record); err != nil {
		t.Fatalf("recordsWorker = %v", err)
	}
	if err := runner.briefWorker(t.Context(), "ns-t1", "worker-t1", "bearer-1", "the finding is false; build and test as usual"); err != nil {
		t.Fatalf("briefWorker = %v", err)
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	want := []string{"POST /records s-6" + record, "POST /brief the finding is false; build and test as usual"}
	if !slices.Equal(worker.texts, want) {
		t.Fatalf("worker got %q, want %q", worker.texts, want)
	}
	for _, auth := range worker.auth {
		if auth != "Bearer bearer-1" {
			t.Fatalf("authorization %q, want the session's bearer", auth)
		}
	}
}
