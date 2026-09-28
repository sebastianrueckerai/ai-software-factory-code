package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// testWorkflowID is the workflow id of every activity the test activity
// environment runs.
const testWorkflowID = "default-test-workflow-id"

// liveRun puts attempt 1 of activity run id into g as if it ran there;
// the returned slice receives what cancelled it.
func liveRun(g *GateWatch, id string, started time.Time, bound time.Duration) *[]error {
	return liveAttempt(g, id, 1, started, bound)
}

func liveAttempt(g *GateWatch, id string, attempt int32, started time.Time, bound time.Duration) *[]error {
	var causes []error
	run := &watchedRun{attempt: attempt, started: started, bound: bound, timer: time.NewTimer(time.Hour), raise: func(time.Duration, bool) {},
		cancel: func(err error) { causes = append(causes, err) }}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.runs == nil {
		g.runs = map[string]*watchedRun{}
	}
	g.runs[runKey(testWorkflowID, id)] = run
	return &causes
}

// gateCase is a runner whose supervisor answers answer, for task t1 with
// pull request 9, one open gate_bound alert and forgejo serving routes.
type gateCase struct {
	runner  *Runner
	store   *fakeStore
	kube    *fakeKube
	objects *fakeObjectStore
	model   *fakeLiteLLM
	alert   int64
	failure string // LastFailure's answer
}

func newGateCase(t *testing.T, activity, answer string, routes map[string]string) *gateCase {
	t.Helper()
	c := &gateCase{kube: newFakeKube(), objects: &fakeObjectStore{}, model: &fakeLiteLLM{content: answer}}
	c.runner, _, c.store = newTestRunner(t, Task{ID: "t1", Status: "in-review", Objective: "fix the short link", Repository: "sf/r",
		IssueNumber: 3, PRNumber: 9}, c.kube, c.objects)
	c.runner.supervisor = testSupervisorModel(c.model.srv(t).URL)
	c.runner.cfg.LastFailure = func(_ context.Context, workflowID, activityID string) (string, error) {
		if workflowID != testWorkflowID || activityID != "7" {
			t.Errorf("last failure of %s/%s, want %s/7", workflowID, activityID, testWorkflowID)
		}
		return c.failure, nil
	}
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(forge.Close)
	c.runner.forgejo = &Forgejo{BaseURL: forge.URL, BotUser: "factory-agent", BotPassword: "pw"}
	c.objects.Put(context.Background(), "sessions/t1/timeline.jsonl", []byte(`{"step":"intake","time":"2026-09-28T10:00:00Z"}`+"\n"))
	var err error
	c.alert, err = c.store.InsertAlert(context.Background(), Alert{Kind: AlertGateBound, Source: activity, TaskID: "t1", BoundS: 30})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// supervise runs SuperviseGate for the case's alert and returns its
// decision and the bundle the model read.
func (c *gateCase) supervise(t *testing.T, activity string, input []any, canWait, canRetry bool) (GateDecision, map[string]any) {
	t.Helper()
	val, err := execActivity(t, c.runner, ActivitySuperviseGate, "t1", GateSupervision{
		Trip:  GateTrip{Activity: activity, ActivityID: "7", Attempt: 1, AlertID: c.alert, Started: time.Now().Add(-45 * time.Second), Bound: 30 * time.Second},
		Input: input, CanWait: canWait, CanRetry: canRetry})
	if err != nil {
		t.Fatal(err)
	}
	var d GateDecision
	if err := val.Get(&d); err != nil {
		t.Fatal(err)
	}
	if len(c.model.chats) == 0 {
		return d, nil
	}
	msgs := c.model.chats[0]["messages"].([]any)
	var bundle map[string]any
	if err := json.Unmarshal([]byte(msgs[1].(map[string]any)["content"].(string)), &bundle); err != nil {
		t.Fatal(err)
	}
	return d, bundle
}

func (c *gateCase) alertRow(t *testing.T) Alert {
	t.Helper()
	for _, a := range c.store.alertRows() {
		if a.ID == c.alert {
			return a
		}
	}
	t.Fatalf("alert %d gone", c.alert)
	return Alert{}
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A check wait past its bound: the model reads the check runs, the runner
// queue, the timeline and the times; its wait extends the run's bound,
// judges the alert noise and goes on the timeline; the call is the task's.
func TestSuperviseGateWaitChecks(t *testing.T) {
	c := newGateCase(t, ActivityWaitChecks, `{"action":"wait","diagnosis":"lint passed, test runs; one job waits for a runner"}`, map[string]string{
		"GET /api/v1/repos/sf/r/commits/c1/status": `{"state":"pending","statuses":[{"status":"success","context":"ci / lint","description":"ok","target_url":"https://git.example/sf/r/actions/runs/4/jobs/0"},{"status":"pending","context":"ci / test","description":"running"}]}`,
	})
	c.runner.cfg.CIQueue.Observe(context.Background(), "ubuntu", []CIJob{{ID: 5, Name: "build", Status: "waiting"}})
	live := liveRun(c.runner.cfg.Gates, "7", time.Now().Add(-45*time.Second), 30*time.Second)

	d, bundle := c.supervise(t, ActivityWaitChecks, []any{"t1", "c1"}, true, true)
	if d != (GateDecision{Action: ActionWait, Diagnosis: "lint passed, test runs; one job waits for a runner"}) {
		t.Fatalf("decision = %+v", d)
	}
	elapsed, _ := time.ParseDuration(fmt.Sprint(bundle["elapsed"]))
	if bundle["activity"] != ActivityWaitChecks || bundle["bound"] != "30s" || elapsed < 45*time.Second || elapsed > 50*time.Second ||
		bundle["objective"] != "fix the short link" || bundle["input"] != `["t1","c1"]` ||
		!strings.Contains(fmt.Sprint(bundle["timeline"]), `"step":"intake"`) ||
		jsonOf(bundle["allowed_actions"]) != `["wait","retry","cancel","escalate"]` {
		t.Fatalf("bundle = %s", jsonOf(bundle))
	}
	// the scrubber's 32-character rule takes the long URL path (review I7's known cost)
	want := `{"checks":{"commit":"c1","runner_queue":[{"job":"build","label":"ubuntu","waiting_s":0}],"state":"pending",` +
		`"statuses":[{"context":"ci / lint","description":"ok","status":"success","target_url":"https://git.[redacted]"},` +
		`{"context":"ci / test","description":"running","status":"pending","target_url":""}]}}`
	if got := jsonOf(bundle["state"]); got != want {
		t.Fatalf("state = %s\nwant    %s", got, want)
	}
	meter := c.runner.supervisor.Meter.(*memPolicy)
	if len(meter.holds) != 1 || meter.holds[0].TaskID != "t1" || meter.holds[0].Ref != "supervisor" {
		t.Fatalf("holds = %+v, want the call held on the task", meter.holds)
	}
	if !c.runner.cfg.Gates.runs[runKey(testWorkflowID, "7")].extended || len(*live) != 0 {
		t.Fatal("the run's bound was not extended, or it was cancelled")
	}
	a := c.alertRow(t)
	if a.Verdict != VerdictNoise || a.ResolvedBy != "supervisor" || a.ResolvedAt == nil || a.Detail["action"] != "wait" ||
		a.Detail["diagnosis"] != d.Diagnosis {
		t.Fatalf("alert = %+v", a)
	}
	timeline := string(objGet(c.objects, "sessions/t1/timeline.jsonl"))
	if !strings.Contains(timeline, `"step":"gate_supervisor"`) || !strings.Contains(timeline, `"action":"wait"`) ||
		!strings.Contains(timeline, `"activity":"WaitChecks"`) {
		t.Fatalf("timeline = %s", timeline)
	}
}

// A session past its bound: the model reads the sandbox pod's phase and
// conditions and its last 100 log lines with the task's tokens scrubbed;
// its retry stops the running attempt.
func TestSuperviseGateRunSessionRetries(t *testing.T) {
	c := newGateCase(t, ActivityRunSession, `{"action":"retry","diagnosis":"the worker hangs on a dead model stream"}`, nil)
	ns := "factory-agents-t1"
	c.kube.CreatePod(context.Background(), ns, "worker-s-1", nil)
	var body strings.Builder
	for i := 1; i <= 150; i++ {
		fmt.Fprintf(&body, "line %d\n", i)
	}
	body.WriteString("push with tok-push-secret and eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ0In0.c2lnbmF0dXJl\n")
	c.kube.scriptPods(0, body.String())
	c.kube.podAt(0).conditions = []PodCondition{{Type: "Ready", Status: "True"}}
	c.kube.secrets[ns+"/task-git"] = map[string]string{"REPO_TOKEN": "tok-read-secret", "PUSH_TOKEN": "tok-push-secret"}
	untrack := c.runner.trackPod("t1", ns, "worker-s-1")
	defer untrack()
	live := liveRun(c.runner.cfg.Gates, "7", time.Now(), 30*time.Second)

	d, bundle := c.supervise(t, ActivityRunSession, []any{map[string]any{"TaskID": "t1"}}, true, true)
	if d.Action != ActionRetry || len(*live) != 1 || (*live)[0] != errGateRetry {
		t.Fatalf("decision = %+v, cancels = %v; want retry stopping the attempt", d, *live)
	}
	pod := bundle["state"].(map[string]any)["pod"].(map[string]any)
	logTail := fmt.Sprint(pod["log"])
	lines := strings.Split(strings.TrimSpace(logTail), "\n")
	if pod["name"] != "worker-s-1" || pod["phase"] != "Running" || jsonOf(pod["conditions"]) != `[{"status":"True","type":"Ready"}]` ||
		len(lines) != 100 || lines[0] != "line 52" || lines[99] != "push with [redacted] and [redacted]" || strings.Contains(logTail, "secret") {
		t.Fatalf("pod = %s", jsonOf(pod))
	}
	if a := c.alertRow(t); a.Verdict != VerdictReal || a.Detail["action"] != "retry" {
		t.Fatalf("alert = %+v", a)
	}
}

// A merge past its bound: the model reads the last error and the pull
// request's mergeability; its cancel is returned for the workflow to fail
// the task with the diagnosis.
func TestSuperviseGateMergeCancels(t *testing.T) {
	c := newGateCase(t, ActivityMerge, `{"action":"cancel","diagnosis":"the branch conflicts with main; no retry merges it"}`, map[string]string{
		"GET /api/v1/repos/sf/r/pulls/9": `{"number":9,"state":"open","merged":false,"mergeable":false,"head":{"sha":"c1"}}`,
	})
	c.failure = "merge: forgejo: refused: status 405: not mergeable"
	live := liveRun(c.runner.cfg.Gates, "7", time.Now(), 30*time.Second)

	d, bundle := c.supervise(t, ActivityMerge, []any{"t1", 9, "c1", "sebastian"}, true, true)
	if d.Action != ActionCancel || d.Diagnosis != "the branch conflicts with main; no retry merges it" || len(*live) != 0 {
		t.Fatalf("decision = %+v, cancels %v", d, *live)
	}
	if bundle["last_error"] != c.failure ||
		jsonOf(bundle["state"]) != `{"pull_request":{"head":"c1","mergeable":false,"merged":false,"number":9,"state":"open"}}` {
		t.Fatalf("bundle = %s", jsonOf(bundle))
	}
	if a := c.alertRow(t); a.Verdict != VerdictReal || a.Detail["action"] != "cancel" {
		t.Fatalf("alert = %+v", a)
	}
}

// A model review past its bound: the model reads the review's last result
// from the timeline and its last error; its escalation carries the
// recommendation.
func TestSuperviseGateModelReviewEscalates(t *testing.T) {
	c := newGateCase(t, ActivityModelReview, `{"action":"escalate","diagnosis":"the review alias answers in minutes","recommendation":"skip the review for this head"}`, nil)
	c.failure = "model review: context deadline exceeded"
	c.objects.Put(context.Background(), "sessions/t1/timeline.jsonl", []byte(`{"step":"model_review","time":"2026-09-28T10:00:00Z","commit":"c0","verdict":"approve"}`+"\n"+
		`{"step":"checks","time":"2026-09-28T10:30:00Z","state":"success"}`+"\n"))
	liveRun(c.runner.cfg.Gates, "7", time.Now(), 30*time.Second)

	d, bundle := c.supervise(t, ActivityModelReview, nil, true, true)
	if d != (GateDecision{Action: ActionEscalate, Diagnosis: "the review alias answers in minutes", Recommendation: "skip the review for this head"}) {
		t.Fatalf("decision = %+v", d)
	}
	want := `{"review":{"last_result":{"commit":"c0","step":"model_review","time":"2026-09-28T10:00:00Z","verdict":"approve"},"model":"review-frontier"}}`
	if got := jsonOf(bundle["state"]); got != want || bundle["last_error"] != c.failure {
		t.Fatalf("state = %s, last error %v\nwant    %s", got, bundle["last_error"], want)
	}
	if a := c.alertRow(t); a.Verdict != VerdictReal || a.Detail["recommendation"] != d.Recommendation {
		t.Fatalf("alert = %+v", a)
	}
}

// A malformed answer is an escalation that says so.
func TestSuperviseGateMalformedAnswerEscalates(t *testing.T) {
	c := newGateCase(t, ActivityMerge, `sure, let it wait`, map[string]string{
		"GET /api/v1/repos/sf/r/pulls/9": `{"number":9,"state":"open","mergeable":true,"head":{"sha":"c1"}}`,
	})
	c.failure = "merge: forgejo POST /pulls/9/merge: status 502"
	liveRun(c.runner.cfg.Gates, "7", time.Now(), 30*time.Second)

	d, bundle := c.supervise(t, ActivityMerge, []any{"t1", 9, "c1", "sebastian"}, true, true)
	if d.Action != ActionEscalate || !strings.HasPrefix(d.Diagnosis, "supervisor answer invalid") {
		t.Fatalf("decision = %+v", d)
	}
	if bundle["last_error"] != c.failure {
		t.Fatalf("bundle = %s", jsonOf(bundle))
	}
	if a := c.alertRow(t); a.Verdict != VerdictReal || a.Detail["action"] != "escalate" {
		t.Fatalf("alert = %+v", a)
	}
}

// A run that waited once and retried once offers only cancel and escalate;
// the model's wait then is an escalation.
func TestSuperviseGateOneWaitAndOneRetryPerRun(t *testing.T) {
	c := newGateCase(t, ActivityWaitChecks, `{"action":"wait","diagnosis":"still running"}`, map[string]string{
		"GET /api/v1/repos/sf/r/commits/c1/status": `{"state":"pending","statuses":[]}`,
	})
	live := liveRun(c.runner.cfg.Gates, "7", time.Now(), 30*time.Second)

	d, bundle := c.supervise(t, ActivityWaitChecks, []any{"t1", "c1"}, false, false)
	if jsonOf(bundle["allowed_actions"]) != `["cancel","escalate"]` {
		t.Fatalf("allowed = %v", bundle["allowed_actions"])
	}
	if d.Action != ActionEscalate || !strings.Contains(d.Diagnosis, `"wait" is not allowed`) || !strings.Contains(d.Diagnosis, "still running") ||
		c.runner.cfg.Gates.runs[runKey(testWorkflowID, "7")].extended || len(*live) != 0 {
		t.Fatalf("decision = %+v", d)
	}
}

// An attempt that ended before the supervisor looked needs no model call:
// the alert is noise.
func TestSuperviseGateEndedRunIsNoise(t *testing.T) {
	c := newGateCase(t, ActivityWaitChecks, `{"action":"wait","diagnosis":"x"}`, nil)
	d, _ := c.supervise(t, ActivityWaitChecks, []any{"t1", "c1"}, true, true)
	if d.Action != "" || len(c.model.chats) != 0 {
		t.Fatalf("decision = %+v after %d calls, want none and no call", d, len(c.model.chats))
	}
	if a := c.alertRow(t); a.Verdict != VerdictNoise || a.ResolvedBy != "supervisor" || a.Detail["diagnosis"] != "the attempt ended before the supervisor looked" {
		t.Fatalf("alert = %+v", a)
	}
}

func TestGateActionsAllowed(t *testing.T) {
	for _, tc := range []struct {
		wait, retry bool
		want        []SupervisorAction
	}{
		{true, true, []SupervisorAction{ActionWait, ActionRetry, ActionCancel, ActionEscalate}},
		{false, true, []SupervisorAction{ActionRetry, ActionCancel, ActionEscalate}},
		{true, false, []SupervisorAction{ActionWait, ActionCancel, ActionEscalate}},
	} {
		if got := (GateSupervision{CanWait: tc.wait, CanRetry: tc.retry}).allowed(); !slices.Equal(got, tc.want) {
			t.Errorf("wait %v retry %v: %v, want %v", tc.wait, tc.retry, got, tc.want)
		}
	}
}

// The trip's attempt ended and the run's next attempt runs here: that one
// is not the supervisor's, so no call, no extension, no cancel.
func TestSuperviseGateOnlyActsOnTheAttemptThatTripped(t *testing.T) {
	c := newGateCase(t, ActivityRunSession, `{"action":"retry","diagnosis":"x"}`, nil)
	live := liveAttempt(c.runner.cfg.Gates, "7", 2, time.Now(), 30*time.Second)
	d, _ := c.supervise(t, ActivityRunSession, nil, true, true)
	if d.Action != "" || len(c.model.chats) != 0 || len(*live) != 0 || c.runner.cfg.Gates.runs[runKey(testWorkflowID, "7")].extended {
		t.Fatalf("decision = %+v, %d calls, cancels %v", d, len(c.model.chats), *live)
	}
	if a := c.alertRow(t); a.Verdict != VerdictNoise {
		t.Fatalf("alert = %+v", a)
	}
}

// A call the task's headroom cannot pay for is not made: escalate.
func TestSuperviseGateRefusedCallEscalates(t *testing.T) {
	c := newGateCase(t, ActivityWaitChecks, `{"action":"wait","diagnosis":"x"}`, map[string]string{
		"GET /api/v1/repos/sf/r/commits/c1/status": `{"state":"pending","statuses":[]}`,
	})
	c.runner.supervisor.Meter.(*memPolicy).taskFree = 0
	liveRun(c.runner.cfg.Gates, "7", time.Now(), 30*time.Second)
	d, _ := c.supervise(t, ActivityWaitChecks, []any{"t1", "c1"}, true, true)
	if d.Action != ActionEscalate || !strings.Contains(d.Diagnosis, "could not be asked") || len(c.model.chats) != 0 {
		t.Fatalf("decision = %+v after %d calls", d, len(c.model.chats))
	}
	if a := c.alertRow(t); a.Verdict != VerdictReal || a.Detail["action"] != "escalate" {
		t.Fatalf("alert = %+v", a)
	}
}

// The whole bundle is scrubbed: while the sandbox runs, the task's git
// tokens and a JWT in the timeline, the last error and the input are gone.
func TestSuperviseGateScrubsTheWholeBundle(t *testing.T) {
	c := newGateCase(t, ActivityRunSession, `{"action":"retry","diagnosis":"x"}`, nil)
	c.kube.secrets["factory-agents-t1/task-git"] = map[string]string{"REPO_TOKEN": "tok-read-secret", "PUSH_TOKEN": "tok-push-secret"}
	c.kube.CreatePod(context.Background(), "factory-agents-t1", "worker-s-1", nil)
	c.kube.scriptPods(0, "working")
	defer c.runner.trackPod("t1", "factory-agents-t1", "worker-s-1")()
	c.objects.Put(context.Background(), "sessions/t1/timeline.jsonl", []byte(`{"step":"x","error":"push tok-push-secret refused"}`+"\n"))
	c.failure = "merge as eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ0In0.c2lnbmF0dXJl refused"
	liveRun(c.runner.cfg.Gates, "7", time.Now(), 30*time.Second)
	_, bundle := c.supervise(t, ActivityRunSession, []any{"t1", 9, "c1", "tok-read-secret"}, true, true)
	all := jsonOf(bundle)
	for _, gone := range []string{"tok-read-secret", "tok-push-secret", "eyJhbGci"} {
		if strings.Contains(all, gone) {
			t.Errorf("bundle keeps %q: %s", gone, all)
		}
	}
	if !strings.Contains(fmt.Sprint(bundle["timeline"]), "push [redacted] refused") || bundle["input"] != `["t1",9,"c1","[redacted]"]` {
		t.Fatalf("bundle = %s", all)
	}
}

// After the session the task's namespace is gone, and a secret read there
// is refused (403, no role binding left), not "not found": a check wait's
// bundle has no tokens to scrub of and is not withheld. With a sandbox
// running, a refused read still withholds the bundle.
func TestSuperviseGateReadsTokensOnlyWhileASandboxRuns(t *testing.T) {
	c := newGateCase(t, ActivityWaitChecks, `{"action":"wait","diagnosis":"checks run"}`, map[string]string{
		"GET /api/v1/repos/sf/r/commits/c1/status": `{"state":"pending","statuses":[]}`,
	})
	c.kube.getErr = map[string]error{"factory-agents-t1/task-git": fmt.Errorf("%w: GET secret", ErrForbidden)}
	liveRun(c.runner.cfg.Gates, "7", time.Now(), 30*time.Second)
	d, bundle := c.supervise(t, ActivityWaitChecks, []any{"t1", "c1"}, true, true)
	if d.Action != ActionWait || bundle["input"] != `["t1","c1"]` || jsonOf(bundle["state"]) != `{"checks":{"commit":"c1","runner_queue":[],"state":"pending","statuses":[]}}` {
		t.Fatalf("decision %+v, bundle %s", d, jsonOf(bundle))
	}

	c = newGateCase(t, ActivityRunSession, `{"action":"wait","diagnosis":"x"}`, nil)
	c.kube.getErr = map[string]error{"factory-agents-t1/task-git": fmt.Errorf("%w: GET secret", ErrForbidden)}
	c.kube.CreatePod(context.Background(), "factory-agents-t1", "worker-s-1", nil)
	defer c.runner.trackPod("t1", "factory-agents-t1", "worker-s-1")()
	liveRun(c.runner.cfg.Gates, "7", time.Now(), 30*time.Second)
	_, bundle = c.supervise(t, ActivityRunSession, nil, true, true)
	if bundle["input"] != "" || !strings.Contains(jsonOf(bundle["state"]), "withheld") {
		t.Fatalf("bundle %s, want it withheld", jsonOf(bundle))
	}
}
