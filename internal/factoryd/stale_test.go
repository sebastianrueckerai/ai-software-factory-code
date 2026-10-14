package factoryd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// staleForgejoStub is Forgejo as a stale escalation writes it: labels read
// empty and replaced, a comment posted and given id 55.
func staleForgejoStub(t *testing.T) (*Forgejo, *[]string, *[]string) {
	t.Helper()
	var requests, comments []string
	srv := httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/labels"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/labels"):
			io.WriteString(w, `{}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			comments = append(comments, string(body))
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":55}`)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw", Seeded: testSeeded}, &requests, &comments
}

// staleRig is one sweep's world: two quiet cards, a chat task and a
// queued one beside them, and the stores the sweep writes.
type staleRig struct {
	store    *fakeStore
	wf       *fakeWorkflows
	in       *Intake
	objects  *fakeObjectStore
	recorder *fakeEventRecorder
	requests *[]string
	comments *[]string
}

func newStaleRig(t *testing.T, ages map[string]time.Duration) *staleRig {
	t.Helper()
	store := newFakeStore()
	for id, age := range ages {
		status := map[string]string{"t-run": "running", "t-wait": "waiting-poster", "t-fresh": "running",
			"t-chat": "running", "t-queued": "queued"}[id]
		if status == "" {
			t.Fatalf("unknown stale rig card %s", id)
		}
		store.CreateTask(context.Background(), Task{ID: id, Status: status, Kind: map[string]string{"t-chat": KindChat}[id],
			Repository: "sf/code", IssueNumber: 42, CreatedAt: time.Now().Add(-age)})
	}
	forgejo, requests, comments := staleForgejoStub(t)
	wf := &fakeWorkflows{}
	rig := &staleRig{store: store, wf: wf, objects: &fakeObjectStore{}, recorder: &fakeEventRecorder{},
		requests: requests, comments: comments}
	rig.in = &Intake{Store: store, Forgejo: forgejo, Workflows: wf, BotUser: "factory-agent",
		Timeline: &Timeline{Objects: rig.objects, Events: rig.recorder, Store: store}}
	return rig
}

// A live-status card quiet for the window is nudged — the signal starts a
// workflow that died — once, not again on the next pass; the queue and
// the chat task owe no sweep, and a card whose session wrote a fresh
// timeline step is not quiet at all.
func TestStaleSweepNudgesAQuietCardOnce(t *testing.T) {
	rig := newStaleRig(t, map[string]time.Duration{"t-run": 45 * time.Minute, "t-fresh": 45 * time.Minute,
		"t-chat": 45 * time.Minute, "t-queued": 45 * time.Minute})
	// t-fresh's session wrote its start two minutes ago
	line, err := json.Marshal(map[string]any{"step": "session_start", "time": time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.objects.Put(context.Background(), "sessions/t-fresh/timeline.jsonl", append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	nudged := map[string]time.Time{}
	StaleSweep(context.Background(), rig.store, rig.wf, rig.in, nil, nudged)
	if want := []string{"t-run stale_nudge factoryd #0: quiet for 45 min while running"}; !slices.Equal(rig.wf.signals, want) {
		t.Fatalf("signals = %q, want the quiet running card nudged alone", rig.wf.signals)
	}
	if n, ok := nudged["t-run"]; !ok || time.Since(n) > time.Minute {
		t.Fatalf("nudged = %v, want t-run remembered at now", nudged)
	}
	StaleSweep(context.Background(), rig.store, rig.wf, rig.in, nil, nudged)
	if len(rig.wf.signals) != 1 {
		t.Fatalf("signals = %q, want no second nudge within the grace", rig.wf.signals)
	}
}

// A card still quiet a window after its nudge, with no activity of its
// workflow running in this process, is escalated like any escalation:
// waiting-human mirrored on the issue, the reason a comment whose first
// line is the question, an intake event, a question_asked row, an alert,
// and the timeline's escalated step carrying the sweep's own words. The
// waiting-poster card's words name its wait.
func TestStaleSweepEscalatesAStillQuietCard(t *testing.T) {
	rig := newStaleRig(t, map[string]time.Duration{"t-run": 70 * time.Minute, "t-wait": 70 * time.Minute})
	nudged := map[string]time.Time{"t-run": time.Now().Add(-StaleWindow - time.Minute),
		"t-wait": time.Now().Add(-StaleWindow - time.Minute)}
	StaleSweep(context.Background(), rig.store, rig.wf, rig.in, nil, nudged)
	ctx := context.Background()
	for id, want := range map[string]string{
		"t-run":  "The card was quiet for 1 h 10 min while running: is its run gone?",
		"t-wait": "The card was quiet for 1 h 10 min while waiting on the issue's author: is its run gone?",
	} {
		task, _, _ := rig.store.Task(ctx, id)
		if task.Status != "waiting-human" {
			t.Fatalf("%s = %s, want waiting-human", id, task.Status)
		}
		var anchor *IntakeEvent
		for i := range rig.store.intakeEvents {
			if rig.store.intakeEvents[i].TaskID == id {
				anchor = &rig.store.intakeEvents[i]
			}
		}
		if anchor == nil || anchor.Kind != IntakeEscalation || !strings.HasPrefix(anchor.Body, want) || anchor.CommentID != 55 {
			t.Fatalf("%s: intake events = %+v, want the escalation anchored with its reason and comment", id, rig.store.intakeEvents)
		}
		rig.recorder.mu.Lock()
		var question string
		for _, e := range rig.recorder.rows {
			if e.TaskID == id && e.Kind == EventQuestionAsked && e.Via == ViaFactoryd {
				question, _ = e.Detail["question"].(string)
			}
		}
		rig.recorder.mu.Unlock()
		if question != want {
			t.Fatalf("%s: the question_asked row's question = %q, want %q", id, question, want)
		}
		body, err := rig.objects.Get(ctx, "sessions/"+id+"/timeline.jsonl")
		if err != nil || !strings.Contains(string(body), `"step":"escalated"`) {
			t.Fatalf("%s: the timeline lacks its escalated step (%v):\n%s", id, err, body)
		}
		var step map[string]any
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			json.Unmarshal([]byte(line), &step)
			if step["step"] == "escalated" {
				break
			}
		}
		// the sweep's record in its own keys, its time lowercase like every
		// timeline line's
		state, _ := step["state"].(string)
		if state == "" || step["quiet"] != "1 h 10 min" || step["action"] == nil || step["reason"] == nil || step["comment"] != float64(55) {
			t.Fatalf("%s: the escalated step = %v, want the sweep's own words beside its reason", id, step)
		}
		if _, ok := step["time"]; !ok {
			t.Fatalf("%s: the escalated step lacks its time:\n%v", id, step)
		}
		if _, ok := nudged[id]; ok {
			t.Fatalf("%s: still in nudged after its escalation", id)
		}
	}
	if !slices.Contains(*rig.requests, "PUT /api/v1/repos/sf/code/issues/42/labels") {
		t.Fatalf("forgejo calls = %q, want the waiting-human label", *rig.requests)
	}
	if len(*rig.comments) != 2 {
		t.Fatalf("comments = %q, want one per escalated card", *rig.comments)
	}
	for _, raw := range *rig.comments {
		var posted struct{ Body string }
		if err := json.Unmarshal([]byte(raw), &posted); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(posted.Body, "factoryd: task ") || !strings.Contains(posted.Body, "```\nThe card was quiet for 1 h 10 min while") ||
			!strings.Contains(posted.Body, "Reply on this issue to start a new attempt") {
			t.Fatalf("the escalation comment = %q", posted.Body)
		}
	}
	if !slices.ContainsFunc(rig.store.alerts, func(a Alert) bool {
		reason, _ := a.Detail["reason"].(string)
		return a.Kind == AlertEscalation && a.Source == FactoryDecider && a.TaskID != "" &&
			strings.HasPrefix(reason, "The card was quiet for 1 h 10 min")
	}) {
		t.Fatalf("alerts = %+v, want the stale escalation's", rig.store.alerts)
	}
	// waiting-human is no longer swept: another pass records nothing new
	before := len(rig.store.intakeEvents)
	StaleSweep(context.Background(), rig.store, rig.wf, rig.in, nil, nudged)
	if len(rig.store.intakeEvents) != before || len(rig.wf.signals) != 0 {
		t.Fatalf("the second pass acted again: %d intake events, %q signals", len(rig.store.intakeEvents), rig.wf.signals)
	}
}

// A card whose workflow runs an activity in this process is left alone:
// its quiet is sparse steps, not a dead run — the liveness the gates
// read.
func TestStaleSweepLeavesALiveSessionAlone(t *testing.T) {
	rig := newStaleRig(t, map[string]time.Duration{"t-run": 70 * time.Minute})
	nudged := map[string]time.Time{"t-run": time.Now().Add(-StaleWindow - time.Minute)}
	gates := &GateWatch{}
	run := &watchedRun{activity: ActivityRunSession, attempt: 1, bound: time.Hour,
		timer: time.NewTimer(time.Hour), raise: func(time.Duration, bool) {}, cancel: func(error) {}}
	gates.mu.Lock()
	gates.runs = map[string]*watchedRun{runKey(WorkflowID("t-run"), "7"): run}
	gates.mu.Unlock()
	StaleSweep(context.Background(), rig.store, rig.wf, rig.in, gates, nudged)
	if task, _, _ := rig.store.Task(context.Background(), "t-run"); task.Status != "running" {
		t.Fatalf("the card = %s, want running: a live session is never escalated", task.Status)
	}
	if len(rig.store.intakeEvents) != 0 || len(rig.store.alerts) != 0 || len(*rig.comments) != 0 {
		t.Fatalf("the sweep escalated a live session: %+v %v", rig.store.intakeEvents, *rig.comments)
	}
}
