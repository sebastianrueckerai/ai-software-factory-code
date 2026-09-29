package factoryd

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
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
// trip; the supervisor's redirect stops the session.
func TestRunSessionUndeliveredNudgeAsksTheSupervisor(t *testing.T) {
	runner, _, _ := newTestRunner(t, supervisedTask("t2"), newFakeKube(), &fakeObjectStore{})
	f := &fakeLiteLLM{content: `{"action":"redirect","guidance":"read b.go instead"}`}
	runner.supervisor = testSupervisorModel(f.srv(t).URL)
	worker := &fakeWorkerEndpoint{status: http.StatusBadGateway}
	worker.start(t, runner)

	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil, threeReads()+resultLine(false, "stopped"))
	if err != nil || res.Outcome != OutcomeRedirect || res.Trip != string(TripSameCall) {
		t.Fatalf("result = (%+v, %v), want the supervisor's redirect on same_call", res, err)
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
