package factoryd

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// fakeWorkerEndpoint is a worker's message endpoint answering status.
type fakeWorkerEndpoint struct {
	mu     sync.Mutex
	status int
	auth   []string
	texts  []string
}

func (f *fakeWorkerEndpoint) start(t *testing.T, r *Runner) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Text string `json:"text"`
		}
		b, _ := io.ReadAll(req.Body)
		json.Unmarshal(b, &in)
		f.mu.Lock()
		f.auth = append(f.auth, req.Header.Get("Authorization"))
		f.texts = append(f.texts, req.Method+" "+req.URL.Path+" "+in.Text)
		status := f.status
		f.mu.Unlock()
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
	if len(worker.texts) != 1 || worker.texts[0] != "POST /message "+readNudge {
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
	if len(worker.texts) != 1 || worker.texts[0] != "POST /message use the store's New" {
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
