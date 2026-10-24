package console

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// fakeEvents is factoryd.Events: it keeps every row, publishes it on bus
// as PGStore does, and answers a project's feed and the newest event of
// each card. Like PGStore it refuses an event with no repository or no
// kind, so a handler that forgets the project of its event fails here
// instead of passing silently.
type fakeEvents struct {
	mu   sync.Mutex
	rows []factoryd.Event
	next int64
	bus  *factoryd.EventBus
	// fail, when set, is every Record's answer
	fail error
}

func (f *fakeEvents) Record(_ context.Context, e factoryd.Event) (int64, error) {
	if e.Repository == "" || e.Kind == "" {
		return 0, fmt.Errorf("event: repository and kind are required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return 0, f.fail
	}
	answer := func(kind string) bool {
		return kind == factoryd.EventQuestionAnswered || kind == factoryd.EventSessionGranted
	}
	if answer(e.Kind) && e.Detail != nil && e.Detail["comment"] != nil {
		comment := e.Detail["comment"]
		for _, r := range f.rows {
			if r.TaskID == e.TaskID && answer(r.Kind) && r.Detail != nil && r.Detail["comment"] == comment {
				return 0, nil
			}
		}
	}
	f.next++
	e.ID = f.next
	if e.At.IsZero() {
		e.At = time.Now()
	}
	f.rows = append(f.rows, e)
	f.bus.Publish(e)
	return e.ID, nil
}

func (f *fakeEvents) FiledPR(_ context.Context, name string) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.rows) - 1; i >= 0; i-- {
		if e := f.rows[i]; e.Repository == name+"/"+name && e.Kind == factoryd.EventProjectFiled {
			pr, _ := e.Detail["pr"].(int64)
			return pr, pr > 0, nil
		}
	}
	return 0, false, nil
}

func (f *fakeEvents) ProjectEvents(_ context.Context, repository string, after int64, limit int) ([]factoryd.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []factoryd.Event
	for i := len(f.rows) - 1; i >= 0; i-- {
		e := f.rows[i]
		if e.Repository == repository && e.ID > after {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeEvents) TaskEvents(_ context.Context, id string, limit int) ([]factoryd.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []factoryd.Event
	for i := len(f.rows) - 1; i >= 0 && len(out) < limit; i-- {
		if f.rows[i].TaskID == id {
			out = append(out, f.rows[i])
		}
	}
	return out, nil
}

func (f *fakeEvents) TaskEventsFor(ctx context.Context, ids []string, limit int) (map[string][]factoryd.Event, error) {
	out := map[string][]factoryd.Event{}
	for _, id := range ids {
		evs, err := f.TaskEvents(ctx, id, limit)
		if err != nil {
			return nil, err
		}
		out[id] = evs
	}
	return out, nil
}

func (f *fakeEvents) NewestTaskEvents(_ context.Context, tasks []string) (map[string]factoryd.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	set := map[string]bool{}
	for _, t := range tasks {
		set[t] = true
	}
	out := map[string]factoryd.Event{}
	for i := len(f.rows) - 1; i >= 0; i-- {
		e := f.rows[i]
		if e.TaskID != "" && set[e.TaskID] {
			if _, ok := out[e.TaskID]; !ok {
				out[e.TaskID] = e
			}
		}
	}
	return out, nil
}

func (f *fakeEvents) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func (f *fakeEvents) all() []factoryd.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]factoryd.Event(nil), f.rows...)
}

func newRigWithEvents(t *testing.T) (*rig, *fakeEvents) {
	r := newRig(t)
	return r, r.events
}

// postedDetail is the detail an answer event carries: the comment Forgejo
// gave the button's own comment.
func postedDetail(id int64) map[string]any { return map[string]any{"comment": id} }

// action is one POST handler: the request to send, what it needs on the
// rig, and the event exactly one row must carry afterwards.
type action struct {
	name    string
	kind    string
	repo    string
	taskID  string
	detail  map[string]any
	setup   func(*testing.T, *rig)
	request func(*rig, *session) *http.Request
}

// boardAction posts form to path, as the board does: htmx off, so a
// handler that shows the board answers 303.
func boardAction(name, path, kind, repo, taskID string, detail map[string]any, setup func(*rig), form url.Values) action {
	return action{name: name, kind: kind, repo: repo, taskID: taskID, detail: detail,
		setup: func(_ *testing.T, r *rig) {
			if setup != nil {
				setup(r)
			}
		},
		request: func(r *rig, s *session) *http.Request {
			form.Set("csrf", s.csrf)
			return post(path, form)
		}}
}

// planRig is the board rig of TestAcceptPlanApproversOnly: plan t-plan of
// linkshort at revision 2, waiting for its approver.
func planRig(r *rig) {
	withLinkshort(r)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "waiting-human", Lane: policy.LanePlan,
		Process: "spec", Objective: "Link shortener", Repository: "linkshort/linkshort", IssueNumber: 1}}}
	r.tasks.plans = map[string][]factoryd.PlanRevision{"t-plan": {planRevision("t-plan", 1, "sha-1"), planRevision("t-plan", 2, "sha-2")}}
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 20, Kind: factoryd.ApprovalPlan, TaskID: "t-plan",
		Project: "linkshort/linkshort", Status: "pending", Spec: json.RawMessage(`{"revision":2,"sha256":"sha-2"}`)})
}

// acceptedPlanRig is the same plan once its approver accepted revision 2
// and factoryd filed its cards: Start plan labels the backlog children.
func acceptedPlanRig(r *rig) {
	planRig(r)
	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].ID == "t-plan" {
			r.tasks.tasks[i].Status, r.tasks.tasks[i].Escalation = "done", ""
		}
	}
	for _, c := range planTasks() {
		if c.ID != "t-plan" && c.Repository == "linkshort/linkshort" && c.Status == "backlog" {
			r.tasks.tasks = append(r.tasks.tasks, c)
		}
	}
}

func backlogCard(r *rig) {
	r.tasks.set("", factoryd.Task{ID: "t-m", Kind: "agent", Status: "backlog", Lane: policy.LaneBacklog, Process: "maintenance",
		Repository: "sf/code", IssueNumber: 5})
}

func escalatedCard(r *rig) {
	r.tasks.set("the agent asks: use a replica?", factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human",
		Repository: "sf/code", IssueNumber: 13})
}

func failedPlanCard(r *rig) {
	withLinkshort(r)
	child := factoryd.Task{ID: "t-c2", Kind: "agent", Status: "failed", Lane: policy.LaneReady, Process: "spec",
		Repository: "linkshort/linkshort", IssueNumber: 2, Source: factoryd.SourcePlan, Parent: "t-plan", PlanKey: "k2"}
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "done", Lane: policy.LanePlan, Process: "spec",
			Objective: "Link shortener", Repository: "linkshort/linkshort", IssueNumber: 1}},
		{Task: child},
	}
}

func verdictAlert(r *rig) {
	r.alerts.rows = []factoryd.Alert{{ID: 3, Kind: factoryd.AlertEscalation, Source: "agent", TaskID: "t-e"}}
	r.tasks.set("", factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13})
}

func standingResource(r *rig) {
	r.resources.rows = []cloudres.Resource{{ID: 11, Kind: "server", Repo: "sf/code", OwnerRef: "t-res", HoldID: 7, State: "live",
		Size: "cx33", LeaseExpires: time.Now().Add(time.Hour)}}
}

// actions are the POST handlers, one per kind a button leaves;
// factoryd's own kinds, task_status among them, come from its store and
// its Timeline (internal/factoryd).
var actions = []action{
	boardAction("createTask", "/tasks", factoryd.EventCardFiled, "sf/code", "",
		map[string]any{"lane": "ready", "title": "Fix the cache"}, nil,
		url.Values{"project": {"ai-software-factory-code"}, "title": {"Fix the cache"}, "body": {"details"}, "lane": {"ready"}}),
	boardAction("moveCard", "/cards/t-m/lane", factoryd.EventCardMoved, "sf/code", "t-m",
		map[string]any{"lane": "ready"}, backlogCard,
		url.Values{"project": {"ai-software-factory-code"}, "lane": {"ready"}}),
	boardAction("answer", "/tasks/t-e/answer", factoryd.EventQuestionAnswered, "sf/code", "t-e",
		postedDetail(1), escalatedCard,
		url.Values{"answer": {"use the replica"}}),
	boardAction("startPlan", "/tasks/t-plan/start-plan", factoryd.EventPlanStarted, "linkshort/linkshort", "t-plan",
		map[string]any{"cards": 2}, acceptedPlanRig, url.Values{}),
	boardAction("acceptPlan", "/tasks/t-plan/plan/accept", factoryd.EventPlanAccepted, "linkshort/linkshort", "t-plan",
		map[string]any{"revision": 2}, planRig, url.Values{"revision": {"2"}, "sha256": {"sha-2"}}),
	boardAction("planAnswer", "/tasks/t-plan/plan/answer", factoryd.EventPlanQuestionAnswered, "linkshort/linkshort", "t-plan",
		map[string]any{"answer": "60 seconds", "index": 1, "revision": 2}, questionsRig,
		url.Values{"revision": {"2"}, "index": {"1"}, "answer": {"60 seconds"}}),
	boardAction("planChanges", "/tasks/t-plan/plan/changes", factoryd.EventPlanChangesRequested, "linkshort/linkshort", "t-plan",
		nil, planRig, url.Values{"text": {"Split the web pages into two cards."}}),
	boardAction("rejectPlan", "/tasks/t-plan/plan/reject", factoryd.EventPlanRejected, "linkshort/linkshort", "t-plan",
		nil, planRig, url.Values{"note": {"not this quarter"}}),
	boardAction("retryCard", "/tasks/t-c2/retry", factoryd.EventCardRetried, "linkshort/linkshort", "t-c2",
		map[string]any{"replacement": 9}, failedPlanCard, url.Values{}),
	boardAction("approve resource", "/approvals/3/approve", factoryd.EventMergeApproved, "sf/code", "t-res",
		map[string]any{"approval": 3, "kind": "resource"}, nil, url.Values{}),
	boardAction("approve merge", "/approvals/4/approve", factoryd.EventMergeApproved, "sf/code", "t-m",
		map[string]any{"pr": 9}, nil, url.Values{}),
	boardAction("deny resource", "/approvals/3/deny", factoryd.EventMergeDenied, "sf/code", "t-res",
		map[string]any{"approval": 3, "kind": "resource"}, nil, url.Values{"note": {"wait for the envelope"}}),
	boardAction("deny request changes", "/approvals/4/deny", factoryd.EventMergeDenied, "sf/code", "t-m",
		map[string]any{"pr": 9, "action": "request_changes"}, nil, url.Values{"note": {"split it"}, "action": {"request_changes"}}),
	boardAction("deny reject", "/approvals/4/deny", factoryd.EventMergeDenied, "sf/code", "t-m",
		map[string]any{"pr": 9, "action": "reject"}, nil, url.Values{"note": {"not wanted"}, "action": {"reject"}}),
	boardAction("verdict", "/alerts/3/verdict", factoryd.EventAlertVerdict, "sf/code", "t-e",
		map[string]any{"alert": 3, "verdict": "real"}, verdictAlert, url.Values{"verdict": {"real"}}),
	boardAction("setSummary", "/tasks/t-m/summary", factoryd.EventSummarySet, "sf/code", "t-m",
		map[string]any{"summary": "a short summary"}, backlogCard, url.Values{"summary": {"a short summary"}}),
	boardAction("closeCard", "/tasks/t-m/close", factoryd.EventCardClosed, "sf/code", "t-m",
		map[string]any{"note": ""}, backlogCard, url.Values{"project": {"ai-software-factory-code"}}),
	boardAction("setAccent", "/tasks/t-m/accent", factoryd.EventAccentSet, "sf/code", "t-m",
		map[string]any{"accent": "blue"}, backlogCard, url.Values{"value": {"blue"}}),
	boardAction("promote", "/resources/11/promote", factoryd.EventResourcePromoted, "sf/code", "t-res",
		map[string]any{"resource": 11, "kind": "server"}, standingResource, url.Values{}),
	boardAction("release", "/resources/11/release", factoryd.EventResourceReleased, "sf/code", "t-res",
		map[string]any{"resource": 11, "kind": "server"}, standingResource, url.Values{}),
	{name: "createProject", kind: factoryd.EventProjectFiled, repo: "linkshort/linkshort",
		detail: map[string]any{"name": "linkshort", "pr": int64(12)},
		setup: func(t *testing.T, r *rig) {
			withInfra(t, r)
			r.c.cfg.CheckProject = func(context.Context, string, []byte) ([]string, error) { return nil, nil }
		},
		request: func(r *rig, s *session) *http.Request {
			return post("/projects", url.Values{"csrf": {s.csrf}, "name": {"linkshort"}, "process": {"spec"}, "template": {"ts-go"}, "merge_rule": {"human"}})
		}},
	{name: "mergeProject", kind: factoryd.EventProjectMerged, repo: "linkshort/linkshort",
		detail: map[string]any{"name": "linkshort", "pr": int64(12)},
		setup: func(t *testing.T, r *rig) {
			f := withInfra(t, r)
			r.filed(t, 12)
			f.set(map[string]any{"number": 12, "state": "open", "merged": false})
		},
		request: func(r *rig, s *session) *http.Request {
			return post("/projects/linkshort/merge", url.Values{"csrf": {s.csrf}})
		}},
}

// wantRow is what the row of one action must say: exactly one new event,
// via button, by the acting human, of a named project and card.
type wantRow struct {
	kind   string
	repo   string
	taskID string
	detail map[string]any
}

func checkRows(t *testing.T, ev *fakeEvents, before int, wants ...wantRow) {
	t.Helper()
	rows := ev.all()[before:]
	if len(rows) != len(wants) {
		t.Fatalf("%d event rows, want %d: %+v", len(rows), len(wants), rows)
	}
	for i, w := range wants {
		e := rows[i]
		switch {
		case e.Kind != w.kind:
			t.Errorf("row %d: kind %s, want %s", i, e.Kind, w.kind)
		case e.Via != factoryd.ViaButton:
			t.Errorf("row %d: via %q, want button", i, e.Via)
		case e.Actor != "sebastian":
			t.Errorf("row %d: actor %q, want sebastian", i, e.Actor)
		case e.Repository != w.repo:
			t.Errorf("row %d: repository %q, want %q", i, e.Repository, w.repo)
		case w.taskID != "" && e.TaskID != w.taskID:
			t.Errorf("row %d: task %q, want %q", i, e.TaskID, w.taskID)
		}
		if w.detail != nil && fmt.Sprint(e.Detail) != fmt.Sprint(w.detail) {
			t.Errorf("row %d: detail %v, want %v", i, e.Detail, w.detail)
		}
	}
}

// Every POST handler leaves exactly one row: via button, the acting
// human's login, a non-empty repository (the project the event belongs to)
// and the kind named after the action.
func TestEveryPostHandlerLeavesOneButtonRow(t *testing.T) {
	for _, a := range actions {
		t.Run(a.name, func(t *testing.T) {
			r, ev := newRigWithEvents(t)
			a.setup(t, r)
			ck, s := r.loggedIn(t)
			before := ev.count()
			rec := r.do(a.request(r, s), ck)
			if rec.Code != http.StatusSeeOther && rec.Code != http.StatusOK {
				t.Fatalf("%s = %d: %.300s", a.name, rec.Code, rec.Body)
			}
			checkRows(t, ev, before, wantRow{kind: a.kind, repo: a.repo, taskID: a.taskID, detail: a.detail})
		})
	}
}

// A deny whose action is neither request_changes nor reject is refused
// with 400 and leaves no row.
func TestDenyBadActionLeavesNoRow(t *testing.T) {
	r, ev := newRigWithEvents(t)
	ck, s := r.loggedIn(t)
	before := ev.count()
	rec := r.do(post("/approvals/4/deny", url.Values{"csrf": {s.csrf}, "note": {"no"}, "action": {"maybe"}}), ck)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "choose request changes or reject") {
		t.Fatalf("deny with no action = %d: %s", rec.Code, rec.Body)
	}
	if ev.count() != before {
		t.Fatalf("a refused deny left %d rows", ev.count()-before)
	}
}

// One action, one row: the Answer button posts its own issue comment and
// names its id in the event's detail, so intake can stay silent for that
// comment when the webhook delivers it.
func TestAnswerEventNamesTheCommentItPosted(t *testing.T) {
	r, ev := newRigWithEvents(t)
	escalatedCard(r)
	ck, s := r.loggedIn(t)
	before := ev.count()
	rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "answer": {"use the replica"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("answer = %d: %s", rec.Code, rec.Body)
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; got != "use the replica" {
		t.Fatalf("the comment = %q", got)
	}
	rows := ev.all()[before:]
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	if got := rows[0].Detail["comment"]; got != int64(1) {
		t.Fatalf("detail %v, want the comment id Forgejo answered", rows[0].Detail)
	}
}

// A project's feed answers JSON, newest first, to a writer of it under the
// board's login rule; limit and after page it, a bad one is 400.
func TestProjectFeedAnswers200JSONForWriter(t *testing.T) {
	r, ev := newRigWithEvents(t)
	ck, _ := r.loggedIn(t)
	ev.Record(context.Background(), factoryd.Event{Repository: "sf/code", Kind: factoryd.EventCardFiled, Actor: "alice", Via: factoryd.ViaButton})
	ev.Record(context.Background(), factoryd.Event{Repository: "sf/code", Kind: factoryd.EventCardMoved, Actor: "bob", Via: factoryd.ViaButton})
	rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/events?limit=50", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /projects/.../events = %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var events []eventJSON
	if err := json.NewDecoder(rec.Body).Decode(&events); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].Kind != factoryd.EventCardMoved || events[1].Kind != factoryd.EventCardFiled {
		t.Fatalf("order: %s, %s; want newest first", events[0].Kind, events[1].Kind)
	}
	if events[0].Repository != "sf/code" || events[0].Via != factoryd.ViaButton || events[0].Actor != "bob" {
		t.Fatalf("row: %+v", events[0])
	}
}

func TestProjectFeedAnswers403ForNonWriter(t *testing.T) {
	r, _ := newRigWithEvents(t)
	ck, _ := r.loggedIn(t)
	r.repoWriters["sf/code"] = []string{"someone-else"}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/events", nil), ck)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-writer = %d, want 403", rec.Code)
	}
}

func TestProjectFeedLimitAndAfter(t *testing.T) {
	r, ev := newRigWithEvents(t)
	ck, _ := r.loggedIn(t)
	var ids []int64
	for i := range 5 {
		id, err := ev.Record(context.Background(), factoryd.Event{Repository: "sf/code", Kind: factoryd.EventCardFiled, Actor: "alice",
			Via: factoryd.ViaButton, Detail: map[string]any{"i": i}})
		if err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	decode := func(rawURL string) []eventJSON {
		t.Helper()
		rec := r.do(httptest.NewRequest(http.MethodGet, rawURL, nil), ck)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", rawURL, rec.Code, rec.Body)
		}
		var events []eventJSON
		if err := json.NewDecoder(rec.Body).Decode(&events); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return events
	}
	got := decode("/projects/ai-software-factory-code/events?limit=2")
	if len(got) != 2 || got[0].ID != ids[4] || got[1].ID != ids[3] {
		t.Fatalf("limit 2: %v, want ids %d, %d", idsOf(got), ids[4], ids[3])
	}
	got = decode(fmt.Sprintf("/projects/ai-software-factory-code/events?after=%d&limit=50", ids[2]))
	if len(got) != 2 || got[0].ID != ids[4] || got[1].ID != ids[3] {
		t.Fatalf("after %d: %v, want ids %d, %d", ids[2], idsOf(got), ids[4], ids[3])
	}
	for _, raw := range []string{"limit=0", "limit=501", "limit=x", "after=-1", "after=x"} {
		rec := r.do(httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/events?"+raw, nil), ck)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", raw, rec.Code)
		}
	}
}

func idsOf(events []eventJSON) []int64 {
	out := make([]int64, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}

// streamOf opens the code project's stream on a real server as the
// logged-in human, sending lastID as Last-Event-ID unless it is empty.
// The stream, the request and the server end with the test.
func streamOf(t *testing.T, r *rig, lastID string) (*http.Response, *bufio.Reader) {
	t.Helper()
	return streamAt(t, r, "ai-software-factory-code", "", lastID)
}

// streamAt is streamOf of project name under the board's query.
func streamAt(t *testing.T, r *rig, name, query, lastID string) (*http.Response, *bufio.Reader) {
	t.Helper()
	ck, _ := r.loggedIn(t)
	srv := httptest.NewServer(r.c)
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/projects/"+name+"/events/stream"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(ck)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		resp.Body.Close()
		srv.Close()
	})
	br := bufio.NewReader(resp.Body)
	if resp.StatusCode == http.StatusOK {
		if got := nextMessage(t, br); !slices.Equal(got, pinged) {
			t.Fatalf("the stream opened with %q, want %q", got, pinged)
		}
	}
	return resp, br
}

// pinged is the stream's heartbeat as nextMessage reads it: event ping,
// the console's build its data.
var pinged = []string{"event: ping", "data: " + build}

// nextMessage is the stream's next message, or comment, as its lines up
// to the blank line that ends it; the test fails after two seconds.
func nextMessage(t *testing.T, br *bufio.Reader) []string {
	t.Helper()
	got := make(chan []string, 1)
	go func() {
		var lines []string
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				got <- append(lines, "read: "+err.Error())
				return
			}
			if line = strings.TrimSuffix(line, "\n"); line == "" {
				got <- lines
				return
			}
			lines = append(lines, line)
		}
	}()
	select {
	case lines := <-got:
		return lines
	case <-time.After(2 * time.Second):
		t.Fatal("no message on the stream within 2 s")
		return nil
	}
}

// at is a fixed event time, so a message's JSON is known to the byte.
var at = time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)

// The stream answers a writer with an event stream that is not cached.
func TestStreamAnswersEventStreamForWriter(t *testing.T) {
	r := newRig(t)
	resp, _ := streamOf(t, r, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream = %d, want 200", resp.StatusCode)
	}
	if ct, cc := resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"); ct != "text/event-stream" || cc != "no-cache" {
		t.Fatalf("Content-Type %q, Cache-Control %q; want text/event-stream, no-cache", ct, cc)
	}
}

// The stream answers 403 to a member who neither writes the project, nor
// the factory's code, nor reads its repository.
func TestStreamAnswers403ForNonMember(t *testing.T) {
	r := newRig(t)
	members(r)
	ck, _ := memberLogin(t, r, "bob")
	req := httptest.NewRequest(http.MethodGet, "/projects/ai-software-factory-code/events/stream", nil)
	if rec := r.do(req, ck); rec.Code != http.StatusForbidden {
		t.Fatalf("bob, who sees no code project = %d, want 403", rec.Code)
	}
}

// readingForgejo shows the member repository sf/code while reads holds,
// as Forgejo does a repository they read.
type readingForgejo struct {
	reads atomic.Bool
	base  http.RoundTripper
}

func (f *readingForgejo) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/sf/code" && f.reads.Load() {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"full_name":"sf/code"}`)), Request: r}, nil
	}
	return f.base.RoundTrip(r)
}

// skipPings is the stream's next message other than the heartbeat; the
// test fails after 40 heartbeats in a row.
func skipPings(t *testing.T, br *bufio.Reader) []string {
	t.Helper()
	for range 40 {
		if got := nextMessage(t, br); !slices.Equal(got, pinged) {
			return got
		}
	}
	t.Fatal("40 heartbeats and nothing else")
	return nil
}

// A member who only reads the project follows its stream read-only: no
// replay and no rows, as they carry steers and the agent's text, but a
// live event other than the agent's text swaps their board; each
// heartbeat checks that they still read it, and the stream ends once
// they do not.
func TestStreamFollowsReadOnly(t *testing.T) {
	wait, beat := countersWait, heartbeat
	countersWait, heartbeat = 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { countersWait, heartbeat = wait, beat })
	r := newRig(t)
	members(r)
	forgejo := &readingForgejo{base: http.DefaultTransport}
	forgejo.reads.Store(true)
	r.c.cfg.HTTP = &http.Client{Transport: forgejo}
	ctx := context.Background()
	row := func(kind string, detail map[string]any) {
		r.events.Record(ctx, factoryd.Event{Repository: "sf/code", TaskID: "t-c", IssueNumber: 5, Kind: kind, Actor: "alice", Via: factoryd.ViaButton, At: at, Detail: detail})
	}
	row(factoryd.EventCardMoved, nil)
	row(factoryd.EventCardMoved, nil)
	memberLogin(t, r, "bob")
	resp, br := streamOf(t, r, "1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bob, who reads the code project = %d, want 200", resp.StatusCode)
	}
	r.events.bus.Publish(factoryd.Event{Repository: "sf/code", TaskID: "t-c", Kind: factoryd.EventSessionSaid, Actor: factoryd.FactoryDecider,
		Via: factoryd.ViaFactoryd, At: at, Detail: map[string]any{"session": "s-1", "text": "the agent's text"}})
	row(factoryd.EventCardSteered, map[string]any{"text": "a writer's steer"})
	for got := skipPings(t, br); !slices.Equal(got, []string{"event: board", "data:"}); got = skipPings(t, br) {
		if len(got) == 0 || got[0] != "event: counters" {
			t.Fatalf("bob's stream sent %q before event board", got)
		}
	}
	forgejo.reads.Store(false)
	for range 10 {
		switch got := nextMessage(t, br); {
		case slices.Equal(got, []string{"read: EOF"}):
			return
		case slices.Equal(got, pinged), len(got) > 0 && (got[0] == "event: counters" || got[0] == "event: board"):
		default:
			t.Fatalf("after bob lost the project the stream sent %q", got)
		}
	}
	t.Fatal("the stream outlived bob's read right")
}

// failingRepo answers 500 to a read of repository demo/demo.
type failingRepo struct{ base http.RoundTripper }

func (f failingRepo) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/api/v1/repos/demo/demo" {
		return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	}
	return f.base.RoundTrip(r)
}

// A writer who loses the project's write right gets no row from the next
// heartbeat on: sebastian, who still sees the board as a writer of the
// factory's code, and bob, whose read of the repository then fails, so
// the stream stays, read-only, and reads no board.
func TestStreamStopsRowsToAFormerWriter(t *testing.T) {
	wait, beat := countersWait, heartbeat
	countersWait, heartbeat = time.Hour, 30*time.Millisecond
	t.Cleanup(func() { countersWait, heartbeat = wait, beat })
	for _, tc := range []struct{ user, project, repo string }{{"sebastian", "ai-software-factory-code", "sf/code"}, {"bob", "demo", "demo/demo"}} {
		r := newRig(t)
		members(r)
		r.repoWriters["sf/code"] = append(r.repoWriters["sf/code"], "sebastian")
		r.writers.users = append(r.writers.users, "sebastian")
		r.c.cfg.HTTP = &http.Client{Transport: failingRepo{http.DefaultTransport}}
		memberLogin(t, r, tc.user)
		_, br := streamAt(t, r, tc.project, "", "")
		r.repoWriters[tc.repo] = nil
		r.c.mu.Lock()
		clear(r.c.moves)
		r.c.mu.Unlock()
		// the second heartbeat after it checked the member again
		for pings := 0; pings < 2; {
			if got := nextMessage(t, br); slices.Equal(got, pinged) {
				pings++
			}
		}
		r.events.Record(context.Background(), factoryd.Event{Repository: tc.repo, TaskID: "t-x", Kind: factoryd.EventCardSteered, Actor: "alice",
			Via: factoryd.ViaButton, At: at, Detail: map[string]any{"text": "a writer's steer"}})
		for pings := 0; pings < 2; {
			switch got := nextMessage(t, br); {
			case slices.Equal(got, pinged):
				pings++
			case len(got) > 0 && got[0] == "event: board":
			default:
				t.Fatalf("%s, a former writer: the stream sent %q", tc.user, got)
			}
		}
	}
}

// A writer's card events swap their cards, so the read after them sends
// no event board even when a face changed: that waits for the heartbeat.
func TestStreamSendsAWriterNoBoardAfterEvents(t *testing.T) {
	wait, beat := countersWait, heartbeat
	countersWait, heartbeat = 10*time.Millisecond, time.Hour
	t.Cleanup(func() { countersWait, heartbeat = wait, beat })
	r := newRig(t)
	running := factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 11, Objective: "fix the cache",
		CreatedAt: time.Now().Add(-5 * time.Minute)}
	r.tasks.set("", running)
	_, br := streamOf(t, r, "")
	moved := func() {
		r.events.Record(context.Background(), factoryd.Event{Repository: "sf/code", TaskID: "t-run", Kind: factoryd.EventCardMoved, Actor: "alice", Via: factoryd.ViaButton, At: at})
	}
	moved()
	for _, want := range []string{"id: 1", "event: counters"} {
		if got := nextMessage(t, br); len(got) == 0 || got[0] != want {
			t.Fatalf("got %q, want %s", got, want)
		}
	}
	running.CreatedAt = running.CreatedAt.Add(-time.Hour)
	r.tasks.set("", running)
	moved()
	time.Sleep(200 * time.Millisecond)
	r.events.bus.Publish(factoryd.Event{Repository: "sf/code", TaskID: "t-run", Kind: factoryd.EventSessionSaid, Actor: factoryd.FactoryDecider,
		Via: factoryd.ViaFactoryd, At: at, Detail: map[string]any{"session": "s-1", "text": "the sentinel"}})
	for range 5 {
		got := nextMessage(t, br)
		if len(got) == 2 && strings.Contains(got[1], "the sentinel") {
			return
		}
		if len(got) == 0 || got[0] == "event: board" {
			t.Fatalf("after a card event the writer's stream sent %q", got)
		}
	}
	t.Fatal("no sentinel")
}

// Ruling 288: each heartbeat reads the board too, so what changes with
// time alone reaches the page without a request of its own: the counters
// when they changed, and event board when a card's face did (its minute
// age here); the agent's text reads nothing.
func TestStreamHeartbeatReadsTheBoard(t *testing.T) {
	wait, beat := countersWait, heartbeat
	countersWait, heartbeat = 10*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { countersWait, heartbeat = wait, beat })
	r := newRig(t)
	running := factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 11, Objective: "fix the cache",
		CreatedAt: time.Now().Add(-5 * time.Minute)}
	r.tasks.set("", running)
	_, br := streamOf(t, r, "")
	r.events.bus.Publish(factoryd.Event{Repository: "sf/code", TaskID: "t-run", Kind: factoryd.EventSessionSaid, Actor: factoryd.FactoryDecider,
		Via: factoryd.ViaFactoryd, At: at, Detail: map[string]any{"session": "s-1", "text": "Reading."}})
	if got := nextMessage(t, br); len(got) != 2 || got[0] != "event: event" {
		t.Fatalf("first %q, want the agent's text", got)
	}
	for _, want := range []string{"event: ping", "event: counters"} {
		if got := nextMessage(t, br); len(got) == 0 || got[0] != want {
			t.Fatalf("got %q, want %s", got, want)
		}
	}
	running.CreatedAt = running.CreatedAt.Add(-time.Hour)
	r.tasks.set("", running)
	// the panel carries no stage ages (C20), so the card's age alone
	// changes no counter: the heartbeat's ping, then the board event
	for _, want := range []string{"event: ping", "event: board"} {
		if got := nextMessage(t, br); len(got) == 0 || got[0] != want {
			t.Fatalf("after the card's age changed: %q, want %s", got, want)
		}
	}
}

// A Record on the project reaches an open stream as one message: event
// "event", the row's id, the feed's JSON of the row. A first connect
// replays nothing (row 1), and another project's row (2) is not sent.
func TestStreamCarriesARecordedEvent(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.events.Record(ctx, factoryd.Event{Repository: "sf/code", TaskID: "t-old", Kind: factoryd.EventCardFiled, Actor: "alice", Via: factoryd.ViaButton, At: at})
	_, br := streamOf(t, r, "")
	r.events.Record(ctx, factoryd.Event{Repository: "demo-app/demo-app", TaskID: "t-d", Kind: factoryd.EventCardMoved, Actor: "alice", Via: factoryd.ViaButton, At: at})
	r.events.Record(ctx, factoryd.Event{Repository: "sf/code", TaskID: "t-x", IssueNumber: 4, Kind: factoryd.EventCardMoved, Actor: "alice",
		Via: factoryd.ViaButton, At: at, Detail: map[string]any{"lane": "ready"}})
	want := []string{"id: 3", "event: event",
		`data: {"id":3,"at":"2026-10-11T09:30:00Z","repository":"sf/code","task_id":"t-x","issue_number":4,"kind":"card_moved","actor":"alice","via":"button","detail":{"lane":"ready"}}`}
	if got := nextMessage(t, br); !slices.Equal(got, want) {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

// Last-Event-ID replays the project's rows after it, oldest first, a
// missing detail as {}; then the stream goes on live, skipping a live
// event at or below the last one it sent (row 3, published again as a
// row recorded between the subscription and the replay would be).
func TestStreamReplaysAfterLastEventID(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	for _, task := range []string{"t-1", "t-2", "t-3"} {
		r.events.Record(ctx, factoryd.Event{Repository: "sf/code", TaskID: task, Kind: factoryd.EventCardMoved, Actor: "alice", Via: factoryd.ViaButton, At: at})
	}
	_, br := streamOf(t, r, "1")
	row := func(id int, task string) []string {
		return []string{fmt.Sprintf("id: %d", id), "event: event", fmt.Sprintf(
			`data: {"id":%d,"at":"2026-10-11T09:30:00Z","repository":"sf/code","task_id":"%s","issue_number":0,"kind":"card_moved","actor":"alice","via":"button","detail":{}}`, id, task)}
	}
	for _, want := range [][]string{row(2, "t-2"), row(3, "t-3")} {
		if got := nextMessage(t, br); !slices.Equal(got, want) {
			t.Fatalf("replayed %q, want %q", got, want)
		}
	}
	r.events.bus.Publish(r.events.all()[2])
	r.events.Record(ctx, factoryd.Event{Repository: "sf/code", TaskID: "t-4", Kind: factoryd.EventCardMoved, Actor: "alice", Via: factoryd.ViaButton, At: at})
	if got, want := nextMessage(t, br), row(4, "t-4"); !slices.Equal(got, want) {
		t.Fatalf("live after the replay %q, want %q", got, want)
	}
}

// Live events may arrive out of id order (two Records publish after their
// inserts in either order): after a replay up to 10, live 12 and then 11
// both arrive, and only a replayed id (10, again) is skipped.
func TestStreamKeepsOutOfOrderLiveEvents(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	for range 10 {
		r.events.Record(ctx, factoryd.Event{Repository: "sf/code", TaskID: "t-x", Kind: factoryd.EventCardMoved, Actor: "alice", Via: factoryd.ViaButton, At: at})
	}
	_, br := streamOf(t, r, "9")
	if got := nextMessage(t, br); len(got) == 0 || got[0] != "id: 10" {
		t.Fatalf("replayed %q, want id 10", got)
	}
	for _, id := range []int64{12, 11, 10, 13} {
		r.events.bus.Publish(factoryd.Event{ID: id, Repository: "sf/code", TaskID: "t-x", Kind: factoryd.EventCardMoved, Actor: "alice", Via: factoryd.ViaButton, At: at})
	}
	for _, want := range []string{"id: 12", "id: 11", "id: 13"} {
		if got := nextMessage(t, br); len(got) == 0 || got[0] != want {
			t.Fatalf("live message %q, want %s", got, want)
		}
	}
}

// A live-only event (the agent's text, no row) goes out without an id, so
// the browser's Last-Event-ID stays the last row's, also after a replay.
func TestStreamSendsALiveOnlyEvent(t *testing.T) {
	r := newRig(t)
	r.events.Record(context.Background(), factoryd.Event{Repository: "sf/code", TaskID: "t-x", Kind: factoryd.EventCardMoved, Actor: "alice", Via: factoryd.ViaButton, At: at})
	_, br := streamOf(t, r, "1")
	r.events.bus.Publish(factoryd.Event{Repository: "sf/code", TaskID: "t-x", IssueNumber: 4, Kind: factoryd.EventSessionSaid, Actor: factoryd.FactoryDecider,
		Via: factoryd.ViaFactoryd, At: at, Detail: map[string]any{"session": "s-1", "text": "Reading the store."}})
	want := []string{"event: event", `data: {"id":0,"at":"2026-10-11T09:30:00Z","repository":"sf/code","task_id":"t-x","issue_number":4,` +
		`"kind":"session_said","actor":"factoryd","via":"factoryd","detail":{"session":"s-1","text":"Reading the store."}}`}
	if got := nextMessage(t, br); !slices.Equal(got, want) {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

// The stream sends the board's counters, the status line's markup and
// each column's count in the page's order, a moment after a live event
// and only when they changed, under the board's filter its query names.
func TestStreamSendsTheCounters(t *testing.T) {
	wait, beat := countersWait, heartbeat
	countersWait, heartbeat = 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { countersWait, heartbeat = wait, beat })
	r := newRig(t)
	running := factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 11, Objective: "fix the cache"}
	r.tasks.set("", running)
	// the member's write rights are read once, before two streams read the board
	ck, _ := r.loggedIn(t)
	r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck)
	_, br := streamOf(t, r, "")
	moved := func() {
		r.events.Record(context.Background(), factoryd.Event{Repository: "sf/code", TaskID: "t-run", Kind: factoryd.EventCardMoved, Actor: "alice", Via: factoryd.ViaButton, At: at})
	}
	// next is the next counters message, past the events and the heartbeats
	next := func(br *bufio.Reader) countersJSON {
		t.Helper()
		for {
			got := nextMessage(t, br)
			if len(got) == 2 && got[0] == "event: counters" {
				var c countersJSON
				if err := json.Unmarshal([]byte(strings.TrimPrefix(got[1], "data: ")), &c); err != nil {
					t.Fatalf("counters %q: %v", got[1], err)
				}
				return c
			}
		}
	}
	// the strip's cells, as the status template renders them
	status := func(s string) bool {
		for _, cell := range []string{`<span class="cell state" data-state="waiting on you"><i class="dot" aria-hidden="true"></i>waiting on you</span>`,
			`<span class="cell"><span><b class="n">1</b> working</span></span>`, `<a href="#needs-you" class="cell needs on"><span><b class="n">2</b> need you</span></a>`,
			`<span class="cell spend" title="spend this UTC month against the project's month cap"><a href="/costs"><span><b class="n">€0.00</b> <span class="of">/ €50.00</span></span>`} {
			if !strings.Contains(s, cell) {
				return false
			}
		}
		return true
	}
	moved()
	if got := next(br); !status(got.Status) || !slices.Equal(got.Counts, []string{"2", "0", "1", "0"}) {
		t.Errorf("counters %+v, want the strip and 2 0 1 0", got)
	}
	// unchanged: two heartbeats pass after the event without counters
	moved()
	for pings := 0; pings < 2; {
		switch got := nextMessage(t, br); {
		case len(got) > 0 && got[0] == "event: counters":
			t.Fatalf("unchanged counters sent again: %q", got)
		case slices.Equal(got, pinged):
			pings++
		}
	}
	queued := factoryd.Task{ID: "t-q", Kind: "agent", Status: "queued", Repository: "sf/code", IssueNumber: 12, Objective: "queue it"}
	r.tasks.set("", running, queued)
	moved()
	if got := next(br); !status(got.Status) || !slices.Equal(got.Counts, []string{"2", "1", "1", "0"}) {
		t.Errorf("counters after a queued card %+v, want 2 1 1 0", got)
	}
}

// A stream ends by the next heartbeat once its session is gone: expired,
// or dropped by a logout in another tab.
func TestStreamEndsWithItsSession(t *testing.T) {
	was := heartbeat
	heartbeat = 20 * time.Millisecond
	t.Cleanup(func() { heartbeat = was })
	r := newRig(t)
	_, br := streamOf(t, r, "")
	if got := nextMessage(t, br); !slices.Equal(got, pinged) {
		t.Fatalf("a live session's stream sent %q, want the heartbeat", got)
	}
	r.sessions.mu.Lock()
	clear(r.sessions.rows)
	r.sessions.mu.Unlock()
	// a heartbeat already under way when the session went may still arrive
	for pings := 0; pings < 2; {
		switch got := nextMessage(t, br); {
		case slices.Equal(got, []string{"read: EOF"}):
			return
		case slices.Equal(got, pinged):
			pings++
		case len(got) > 0 && (got[0] == "event: counters" || got[0] == "event: board"):
		default:
			t.Fatalf("after the session ended the stream sent %q", got)
		}
	}
	t.Fatal("the stream outlived its session by two heartbeats")
}

// The console's server shuts down without waiting on its open streams:
// EndStreams, registered on its shutdown, ends them, so Shutdown returns
// before its timeout and the stream reads its end.
func TestStreamEndsAtTheServersShutdown(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: r.c, ReadHeaderTimeout: time.Second}
	srv.RegisterOnShutdown(r.c.EndStreams)
	go srv.Serve(ln)
	req, err := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/projects/ai-software-factory-code/events/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(ck)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	if got := nextMessage(t, br); !slices.Equal(got, pinged) {
		t.Fatalf("the stream opened with %q, want %q", got, pinged)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown with an open stream: %v, want it to return once the stream ended", err)
	}
	if got := nextMessage(t, br); !slices.Equal(got, []string{"read: EOF"}) {
		t.Fatalf("after the shutdown the stream read %q, want its end", got)
	}
}

// Every card face the board renders is what its fragment answers, byte
// for byte, under the board's filter: the escalated card in its column and
// in the escalations, backlog cards with one move form per lane, a plan's
// cards with Start plan, Retry and their plan's issue, the badge, the
// blocked, waiting and merge queue lines, a human task, an accent and an
// activity line.
func TestCardFragmentIsTheBoardsCard(t *testing.T) {
	r, ev := newRigWithEvents(t)
	withLinkshort(r)
	since := time.Now().Add(-90 * time.Minute)
	r.tasks.tasks = append(planTasks(),
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-c7", Kind: "agent", Status: "failed", Lane: "ready", Process: "spec", Objective: "card 7",
			Repository: "linkshort/linkshort", IssueNumber: 7, Source: factoryd.SourcePlan, Parent: "t-plan", PlanKey: "k7"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13,
			Objective: "add the feed", Accent: "teal", CreatedAt: since}, Escalation: "The agent asks: a replica or a shard?\nmore", EscalationAlert: 3},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-q", Kind: "agent", Status: "in-review", Repository: "linkshort/linkshort", IssueNumber: 14,
			PRNumber: 21, Objective: "queue it", CreatedAt: since, Gate: factoryd.GateMergeQueue, GateSince: &since}, QueueAhead: 2},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-blk", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 15,
			Objective: "blocked work", HumanSummary: "the blocked one", CreatedAt: since}, Dependencies: "#3, #5"},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-h", Kind: "human", Status: "queued", Repository: "sf/code", IssueNumber: 16, Objective: "rotate the key"}})
	if _, err := ev.Record(context.Background(), factoryd.Event{Repository: "sf/code", TaskID: "t-run", Kind: factoryd.EventCardMoved,
		Actor: "sebastian", Via: factoryd.ViaButton, At: since}); err != nil {
		t.Fatal(err)
	}
	ck, _ := r.loggedIn(t)
	for _, page := range []string{"", "?view=wide", "?project=linkshort", "?project=linkshort&view=wide"} {
		body := r.do(httptest.NewRequest(http.MethodGet, "/"+page, nil), ck).Body.String()
		// the script asks for a card under the board's own query, the
		// project it shows included
		_, query, _ := strings.Cut(body, `data-query="`)
		query, _, _ = strings.Cut(query, `"`)
		query = html.UnescapeString(query)
		faces := 0
		for rest := body; ; faces++ {
			i := strings.Index(rest, `<article class="card face`)
			if i < 0 {
				break
			}
			rest = rest[i:]
			face := rest[:strings.Index(rest, "</article>")+len("</article>")]
			rest = rest[len(face):]
			id, _, _ := strings.Cut(face[strings.Index(face, `data-card="`)+len(`data-card="`):], `"`)
			rec := r.do(httptest.NewRequest(http.MethodGet, "/cards/"+id+query, nil), ck)
			if rec.Code != http.StatusOK || rec.Body.String() != face {
				t.Errorf("board%s: the fragment of %s = %d\n%s\nwant the board's face\n%s", page, id, rec.Code, rec.Body, face)
			}
		}
		// the code project: t-run, t-b, t-blk, the human task t-h and the
		// escalated t-e, which the wide view shows twice (its escalation
		// and its column); linkshort: the plan, its cards 2 to 5 and 7,
		// the second spec and t-q; teal: t-e alone, of the code project
		if want := map[string]int{"": 5, "?view=wide": 6, "?project=linkshort": 8, "?project=linkshort&view=wide": 8}[page]; faces != want {
			t.Errorf("board%s shows %d faces, want %d", page, faces, want)
		}
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String() +
		r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	for id, lanes := range map[string][]string{"t-c2": {"ready"}, "t-b": {"ready"}} {
		face := faceOf(t, body, id)
		for _, lane := range lanes {
			if n := strings.Count(face, `data-move="`+lane+`"`); n != 1 {
				t.Errorf("%s has %d forms into %s, want 1", id, n, lane)
			}
		}
	}
}

func TestCardFragmentAnswers404ForNonWriter(t *testing.T) {
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-x", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 99})
	ck, _ := r.loggedIn(t)
	r.repoWriters["sf/code"] = []string{"someone-else"}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-x", nil), ck)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-writer = %d, want 404", rec.Code)
	}
}

// The board polls nothing: no polling hx-trigger, the footer that says
// when the stream is down, and a board fragment that carries the status
// line out of band. console.js under node, which the test needs: it
// follows the stream under the board's query; a first open asks for
// nothing, a reconnect for the board once and the drawer's Chat it shows,
// and no timer for anything; counters set the status line and the
// columns' counts; the agent's text and a steer reach the drawer's Chat
// once each, and another session's text and the card's new status load
// the tab again, a steer being typed kept; event board swaps the board,
// and a swap the open drawer holds back runs once it closes, or once the
// focus lands elsewhere, not while it moves on to another field. The
// find's box, focused and holding text, holds no swap; a find in flight
// holds the board's and the cards' swaps until its answer is in; a board
// under a new query moves the stream to it, whose first open reads the
// board and the Chat; a board swap that took the box's focus gives it back
// with its caret, and a card's swap after the focus moved on does not. A
// find whose answer failed in its handling releases the hold too; two
// reads of the board asked for during one in flight send one more after
// it, and so does a read whose handling failed, once htmx let go of the
// board, so a reconnect during its rerun still goes out; an answer for a
// board that left the page is not swapped, another's through it is;
// the find's good answer writes its URL in place, with the open
// document, the drawer's hash and the entry's state, a failed one none;
// and Back and Forward put the entry's q in the box without a run of the
// find, the board reading under the entry's query when it differs from
// the one it stands on.
func TestBoardFollowsTheStreamAlone(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()
	for _, want := range []string{`<div id="board" data-project="ai-software-factory-code" data-query="?project=ai-software-factory-code">`,
		`<footer id="live" role="status"></footer>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the board page lacks %s", want)
		}
	}
	if regexp.MustCompile(`hx-trigger="[^"]*\bevery\b`).MatchString(body) {
		t.Error("the board page still polls")
	}
	frag := r.do(httptest.NewRequest(http.MethodGet, "/board?project=ai-software-factory-code", nil), ck).Body.String()
	if !strings.HasPrefix(frag, `<div id="board"`) || !strings.Contains(frag, "</div>\n<div id=\"status-line\" hx-swap-oob=\"innerHTML\"><summary class=\"strip\">") {
		t.Errorf("the board fragment carries no status line out of band:\n%s", frag)
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var listeners = {}, winListeners = {}, requests = [], pending = [], replaced = [], pushed = [], timers = 0, sources = [], locked = false, queued = [];
function swapped() { locked = false; queued = []; }
function el(props) {
  var e = {dataset: {}, hidden: false, textContent: "", innerHTML: "", children: [],
    querySelector: function () { return null; }, querySelectorAll: function () { return []; }, contains: function () { return false; },
    getAttribute: function () { return null; }, setAttribute: function () {}, addEventListener: function () {},
    classList: {contains: function () { return false; }}, appendChild: function (c) { this.children.push(c); return c; }};
  return Object.assign(e, props);
}
var counts = [el(), el(), el()], log = el({dataset: {task: "t-run", session: "s-2"}});
log.querySelector = function (sel) {
  return log.children.some(function (c) { return sel === "[data-key=\"" + c.dataset.key + "\"]"; }) ? {} : null;
};
var themeLight = el({dataset: {themeSet: "light"}});
var byID = {board: el({id: "board", dataset: {project: "code", query: "?project=code"}}), "status-line": el(), live: el(),
  drawer: el({hidden: true}), "session-log": log, "steer-input": el({value: "half a steer"})};
function EventSource(url) { this.url = url; this.on = {}; sources.push(this); }
EventSource.prototype.addEventListener = function (type, fn) { this.on[type] = fn; };
EventSource.prototype.close = function () { this.closed = true; };
EventSource.CLOSED = 2;
var sandbox = {EventSource: EventSource, URL: URL, URLSearchParams: URLSearchParams, CSS: {escape: function (s) { return s; }}, location: {hash: ""},
  history: {state: null, replaceState: function (state, title, path) { replaced.push([state, path]); },
    pushState: function (state, title, path) { pushed.push(path); }},
  localStorage: {getItem: function () { return null; }, setItem: function () {}},
  setTimeout: function () { timers++; }, setInterval: function () { timers++; }, clearTimeout: function () {},
  Event: function (type) { this.type = type; },
  CustomEvent: function (type, init) { this.type = type; this.detail = init && init.detail; },
  fetch: function (url) { requests.push("fetch " + url); return new Promise(function () {}); },
  // a request is in flight until settle answers it; as htmx does, a read of
  // the board asked for while one is in flight queues behind it, its
  // promise settled at once, and is dropped when the first's swap replaces
  // the board
  htmx: {ajax: function (verb, url) {
    if (url.indexOf("/board") === 0 && locked) {
      queued.push(verb + " " + url);
      return Promise.resolve();
    }
    requests.push(verb + " " + url);
    if (url.indexOf("/drawer") > 0) byID["steer-input"] = el({value: ""});
    if (url.indexOf("/board") !== 0) return new Promise(function (ok) { pending.push(ok); });
    locked = true;
    return new Promise(function (ok) { pending.push(function () { swapped(); ok(); }); });
  }, swap: function () {}, process: function () {}},
  window: {addEventListener: function (type, fn) { (winListeners[type] = winListeners[type] || []).push(fn); }},
  document: {body: {classList: {toggle: function () {}}}, documentElement: {getAttribute: function () { return "dark"; }},
    getElementById: function (id) { return byID[id] || null; },
    querySelectorAll: function (sel) { return sel === "#board .count" ? counts : []; },
    querySelector: function (sel) { return sel === 'button[data-theme-set="light"]' ? themeLight : null; },
    removeEventListener: function (type, fn) { listeners[type] = (listeners[type] || []).filter(function (f) { return f !== fn; }); },
    createElement: function () { return el(); },
    addEventListener: function (type, fn) { (listeners[type] = listeners[type] || []).push(fn); },
    dispatchEvent: function (e) { (listeners[e.type] || []).forEach(function (fn) { fn(e); }); }}};
require("vm").runInNewContext(require("fs").readFileSync(process.argv[1], "utf8"), sandbox);
listeners.DOMContentLoaded.forEach(function (fn) { fn(); });
// settle answers every read in flight, and what their answers start runs
function settle() {
  pending.splice(0).forEach(function (ok) { ok(); });
  return new Promise(function (done) { setImmediate(done); });
}
// fire hands the listeners an htmx event, whose request's config names
// the element that asked: the one given, else the swapped target
function fire(type, detail) {
  if (detail && !detail.requestConfig) detail.requestConfig = {elt: el(detail.elt || detail.target)};
  (listeners[type] || []).forEach(function (fn) { fn({type: type, detail: detail}); });
}
// fail is htmx failing in its handling of the board's read in flight: it
// fires onLoadError while it holds the board, never settles that read's
// promise, then lets go of the board and sends the read queued behind it
function fail() {
  pending.splice(0);
  fire("htmx:onLoadError", {elt: {id: "board"}});
  locked = false;
  queued.splice(0, 1).forEach(function (r) {
    locked = true;
    requests.push(r);
    pending.push(swapped);
  });
}
// xhr is a request's, whose end the test calls
function xhr() {
  var ends = [];
  return {addEventListener: function (type, fn) { if (type === "loadend") ends.push(fn); }, end: function () { ends.forEach(function (fn) { fn(); }); }};
}
(async function () {
var es = sources[0], out = {url: es.url};
function event(e) { es.on.event({data: JSON.stringify(e)}); }
es.onopen();
out.opened = requests.slice();
es.onopen();
out.reconnected = requests.slice();
await settle();
out.timers = timers;
es.on.counters({data: JSON.stringify({status: "\n<strong class=\"pname\">code</strong>\n", counts: ["2", "0", "1"]})});
out.status = byID["status-line"].innerHTML;
out.counts = counts.map(function (c) { return c.innerHTML; });
var text = {id: 0, at: "2026-10-11T10:05:00.25Z", task_id: "t-run", kind: "session_said", actor: "factoryd", detail: {session: "s-2", text: "Tests pass."}};
event(text);
event(text);
requests = [];
event({id: 7, at: "2026-10-11T10:06:00Z", task_id: "t-run", kind: "card_steered", actor: "sebastian", detail: {text: "Merge it."}});
await settle();
event({id: 0, at: "2026-10-11T10:07:00Z", task_id: "t-run", kind: "session_said", actor: "factoryd", detail: {session: "s-3", text: "Again."}});
await settle();
event({id: 8, at: "2026-10-11T10:08:00Z", task_id: "t-run", kind: "task_status", actor: "factoryd", detail: {status: "in-review"}});
await settle();
event({id: 9, at: "2026-10-11T10:08:00Z", task_id: "t-other", kind: "task_status", actor: "factoryd", detail: {status: "done"}});
await settle();
out.after = requests;
requests = [];
byID.drawer.hidden = false;
event({id: 10, at: "2026-10-11T10:09:00Z", task_id: "", kind: "card_filed", actor: "sebastian", detail: {}});
await settle();
es.on.board({data: ""});
await settle();
out.held = requests.slice();
byID.drawer.hidden = true;
listeners.focusout.forEach(function (fn) { fn({relatedTarget: el()}); });
out.moved = requests.slice();
listeners.focusin.forEach(function (fn) { fn({}); });
out.focused = requests;
requests = [];
byID.drawer.hidden = false;
es.on.board({data: ""});
await settle();
byID.drawer.hidden = true;
listeners.drawerclosed.forEach(function (fn) { fn(); });
out.released = requests;
// a turn: the avatar, the who line (the name and the time), the text
out.said = log.children.map(function (m) { var who = m.children[1]; return [m.className, m.dataset.key, who.children[0].textContent + " · " + who.children[1].textContent, m.children[2].textContent].join(" | "); });
out.timers = timers;
await settle();
out.steer = byID["steer-input"].value;
// the find's box, focused and holding text, holds no swap of the board
var q = el({id: "q", type: "text", value: "exp", defaultValue: "", selectionStart: 2, selectionEnd: 3,
  matches: function () { return true; }, focus: function () { sandbox.document.activeElement = q; },
  setSelectionRange: function (a, b) { out.caret = [a, b]; }});
byID.q = q;
byID.board.contains = function (x) { return x === q; };
byID.board.querySelectorAll = function (sel) { return sel === "input, textarea" ? [q] : []; };
sandbox.document.activeElement = q;
requests = [];
event({id: 11, at: "2026-10-11T10:10:00Z", task_id: "", kind: "card_filed", actor: "sebastian", detail: {}});
out.typing = requests.slice();
await settle();
// a find in flight holds the board's and the cards' swaps until its answer
// is in; the two held run one after the other, as reads do
requests = [];
fire("htmx:beforeRequest", {elt: {id: "find"}});
event({id: 12, at: "2026-10-11T10:11:00Z", task_id: "", kind: "card_filed", actor: "sebastian", detail: {}});
event({id: 13, at: "2026-10-11T10:11:00Z", task_id: "t-other", kind: "card_moved", actor: "sebastian", detail: {}});
out.finding = requests.slice();
fire("htmx:afterRequest", {elt: {id: "find"}});
await settle();
out.found = requests.slice();
await settle();
// a failed handling of the find's answer releases the hold too
requests = [];
fire("htmx:beforeRequest", {elt: {id: "find"}});
event({id: 14, at: "2026-10-11T10:12:00Z", task_id: "", kind: "card_filed", actor: "sebastian", detail: {}});
out.failing = requests.slice();
fire("htmx:onLoadError", {elt: {id: "find"}});
out.failed = requests.slice();
await settle();
// two reads of the board asked for during one in flight send exactly one more after it
requests = [];
event({id: 15, at: "2026-10-11T10:13:00Z", task_id: "", kind: "card_filed", actor: "sebastian", detail: {}});
event({id: 16, at: "2026-10-11T10:13:00Z", task_id: "", kind: "card_filed", actor: "sebastian", detail: {}});
es.on.board({data: ""});
out.reads = [requests.length];
await settle();
out.reads.push(requests.length);
await settle();
out.reads.push(requests.length);
// a read whose handling failed (htmx never settles its promise) releases
// the next read too, once htmx has let go of the board
requests = [];
event({id: 17, at: "2026-10-11T10:14:00Z", task_id: "", kind: "card_filed", actor: "sebastian", detail: {}});
event({id: 18, at: "2026-10-11T10:14:00Z", task_id: "", kind: "card_filed", actor: "sebastian", detail: {}});
fail();
await Promise.resolve();
out.unsettled = requests.slice();
await settle();
// a reconnect asked for while that rerun is in flight still goes out after it
requests = [];
event({id: 21, at: "2026-10-11T10:16:00Z", task_id: "", kind: "card_filed", actor: "sebastian", detail: {}});
event({id: 22, at: "2026-10-11T10:16:00Z", task_id: "", kind: "card_filed", actor: "sebastian", detail: {}});
fail();
await Promise.resolve();
es.onopen();
await settle();
out.rerun = requests.slice();
await settle();
// an answer for a board that has left the page is not swapped; another
// request's answer told to the same board is; a board still in the page
// takes its answer; the guard goes when its xhr ends; the find and other
// targets get none
function asker(id) {
  var on = [];
  return Object.assign(el({id: id, isConnected: true}), {
    addEventListener: function (type, fn) { if (type === "htmx:beforeSwap") on.push(fn); },
    removeEventListener: function (type, fn) { on = on.filter(function (f) { return f !== fn; }); },
    answer: function (x) {
      var ev = {type: "htmx:beforeSwap", detail: {xhr: x, shouldSwap: true}};
      on.slice().forEach(function (fn) { fn(ev); });
      return !ev.detail.shouldSwap;
    },
    guards: function () { return on.length; }});
}
var stale = asker("board"), late = xhr(), live = asker("board"), kept = xhr(), findTarget = asker("board"), drawerTarget = asker("drawer");
fire("htmx:beforeRequest", {elt: stale, target: stale, xhr: late});
out.guard = [stale.guards(), stale.answer(xhr())];
stale.isConnected = false;
out.guard.push(stale.answer(late));
late.end();
out.guard.push(stale.guards());
fire("htmx:beforeRequest", {elt: asker("t-1-summary"), target: live, xhr: kept});
out.guard.push(live.answer(kept));
kept.end();
out.guard.push(live.guards());
fire("htmx:beforeRequest", {elt: {id: "find"}, target: findTarget, xhr: xhr()});
fire("htmx:afterRequest", {elt: {id: "find"}});
fire("htmx:beforeRequest", {elt: asker("x"), target: drawerTarget, xhr: xhr()});
out.guard.push(findTarget.guards(), drawerTarget.guards());
// the stream's read whose answer the guard does not swap still settles,
// and the next read goes out after it
requests = [];
event({id: 19, at: "2026-10-11T10:15:00Z", task_id: "", kind: "card_filed", actor: "sebastian", detail: {}});
var read = asker("board"), asked = xhr();
fire("htmx:beforeRequest", {elt: read, target: read, xhr: asked});
event({id: 20, at: "2026-10-11T10:15:00Z", task_id: "", kind: "card_filed", actor: "sebastian", detail: {}});
read.isConnected = false;
out.dropped = [read.answer(asked)];
asked.end();
await settle();
out.dropped.push(requests);
await settle();
// a swap that brings the board under a new query moves the stream to it,
// once; the new stream's first open reads the board and the Chat again
requests = [];
fire("htmx:afterSwap", {target: byID.board});
byID.board = el({id: "board", dataset: {project: "code", query: "?project=code&q=exp"}});
fire("htmx:afterSwap", {target: byID.board});
fire("htmx:afterSwap", {target: byID.board});
out.streams = sources.map(function (s) { return s.url + (s.closed ? " closed" : ""); });
if (sources[1]) sources[1].onopen();
out.followed = requests.slice();
await settle();
// a board swap that took the box's focus gives it back with the caret;
// a card's swap (no beforeSwap) after the focus moved on leaves it there
sandbox.document.activeElement = q;
fire("htmx:beforeSwap", {target: byID.board, xhr: {status: 200, responseText: ""}});
sandbox.document.activeElement = sandbox.document.body;
fire("htmx:afterSwap", {target: byID.board});
out.refocused = sandbox.document.activeElement === q;
var composer = el({id: "chat-input"});
sandbox.document.activeElement = composer;
fire("htmx:afterSwap", {});
out.kept = sandbox.document.activeElement === composer;
// the find's good answer writes its query into the URL in place, with the
// open document's doc=, the drawer's hash and the entry's state; a failed
// answer writes none
replaced = [];
Object.assign(sandbox.location, {href: "http://lab/?project=code&doc=docs/a.md#card=t-1", search: "?project=code&doc=docs/a.md", hash: "#card=t-1"});
sandbox.history.state = {doc: "docs/a.md"};
fire("htmx:afterRequest", {elt: {id: "find"}, successful: true, pathInfo: {finalRequestPath: "/?project=code&q=exp"}});
fire("htmx:afterRequest", {elt: {id: "find"}, successful: false, pathInfo: {finalRequestPath: "/?project=code&q=bad"}});
Object.assign(sandbox.location, {href: "http://lab/?project=code", search: "?project=code", hash: ""});
sandbox.history.state = null;
fire("htmx:afterRequest", {elt: {id: "find"}, successful: true, pathInfo: {finalRequestPath: "/?project=code&q=exp"}});
out.replaced = replaced;
// Back and Forward follow the URL's query: the find's box takes the
// entry's q without a run of the find, and the board reads under the
// entry's query when it differs from the one it stands on; an entry with
// the board's own query reads nothing
var typed = [];
q.dispatchEvent = function (ev) { typed.push("[" + q.value + "] " + ev.type); };
requests = [];
Object.assign(sandbox.location, {search: "?project=code&q=exp"});
q.value = "other";
winListeners.popstate.forEach(function (fn) { fn({}); });
out.popped = [typed.slice(), q.value, requests.slice()];
Object.assign(sandbox.location, {search: "?project=code"});
q.value = "exp";
winListeners.popstate.forEach(function (fn) { fn({}); });
out.popped.push(typed.slice(), q.value, requests.slice());
await settle();
console.log(JSON.stringify(out));
})();`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver, "static/console.js")
	got, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	want := `{"url":"/projects/code/events/stream?project=code","opened":[],"reconnected":["GET /board?project=code","GET /cards/t-run/drawer?tab=chat"],"timers":0,` +
		`"status":"\n<strong class=\"pname\">code</strong>\n","counts":["2","0","1"],` +
		`"after":["GET /board?project=code","GET /cards/t-run/drawer?tab=chat","GET /cards/t-run/drawer?tab=chat","GET /board?project=code","GET /board?project=code"],` +
		`"held":["GET /board?project=code","GET /board?project=code"],"moved":["GET /board?project=code","GET /board?project=code"],"focused":["GET /board?project=code","GET /board?project=code"],"released":["GET /board?project=code"],` +
		`"said":["msg agent | s-2@2026-10-11T10:05:00.25Z | agent · 10-11 10:05Z | Tests pass.","msg you | e7 | sebastian · 10-11 10:06Z | Merge it."],"steer":"half a steer",` +
		`"typing":["GET /board?project=code"],"finding":[],"found":["GET /board?project=code","GET /board?project=code"],` +
		`"failing":[],"failed":["GET /board?project=code"],"reads":[1,2,2],"unsettled":["GET /board?project=code","GET /board?project=code"],` +
		`"rerun":["GET /board?project=code","GET /board?project=code","GET /cards/t-run/drawer?tab=chat","GET /board?project=code"],` +
		`"guard":[1,false,true,0,false,0,0,0],"dropped":[true,["GET /board?project=code","GET /board?project=code"]],` +
		`"streams":["/projects/code/events/stream?project=code closed","/projects/code/events/stream?project=code&q=exp"],` +
		`"followed":["GET /board?project=code&q=exp","GET /cards/t-run/drawer?tab=chat"],"caret":[2,3],"refocused":true,"kept":true,` +
		`"replaced":[[{"doc":"docs/a.md"},"/?project=code&q=exp&doc=docs%2Fa.md#card=t-1"],[null,"/?project=code&q=exp"]],"popped":[[],"exp",[],[],"",["GET /board?project=code"]]}`
	if strings.TrimSpace(string(got)) != want {
		t.Errorf("console.js under node:\n%s\nwant\n%s", got, want)
	}
}

// The board stays live across a roll and a tab left behind, console.js
// under node: a failed read of the board's own flashes nothing, a member's
// action the router answers in a roll's gap (502 to 504 without the
// Content-Security-Policy, or no answer) flashes that the factory
// restarts, the console's own refusal and the router's other answers
// their text; a stream a refused connect closed reads the board in 5 s (an
// ended session answers it with the login) and opens anew, whose open
// reads the board again and tells the chat, as the browser's own
// reconnect after a drop does, while a stream moved to a new query reads
// the board and tells the chat nothing; a tab back in front reopens a
// stream closed or silent for over a minute, not a talking one, and a
// hidden tab or the network back checks too; a ping naming another build
// reloads the page once it is hidden, nothing typed and nothing streaming.
func TestBoardStaysLive(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var listeners = {}, winListeners = {}, requests = [], pending = [], timers = [], sources = [], reloads = 0, clock = 1000000, opened = 0, typed = [], streaming = null;
function el(props) {
  return Object.assign({dataset: {}, hidden: false, textContent: "", innerHTML: "",
    querySelector: function () { return null; }, querySelectorAll: function () { return []; }, contains: function () { return false; },
    getAttribute: function () { return null; }, setAttribute: function () {}, addEventListener: function () {},
    classList: {contains: function () { return false; }}}, props);
}
var byID = {board: el({id: "board", dataset: {project: "code", query: "?project=code"}}), live: el(), flash: el({hidden: true})};
function EventSource(url) { this.url = url; this.on = {}; this.readyState = 0; sources.push(this); }
EventSource.prototype.addEventListener = function (type, fn) { this.on[type] = fn; };
EventSource.prototype.close = function () { this.closed = true; this.readyState = 2; };
EventSource.CONNECTING = 0; EventSource.OPEN = 1; EventSource.CLOSED = 2;
var document = {hidden: false, activeElement: null,
  body: el({querySelectorAll: function (sel) { return sel === "input, textarea" ? typed : []; }}),
  documentElement: {getAttribute: function () { return "dark"; }},
  getElementById: function (id) { return byID[id] || null; },
  querySelector: function (sel) { return sel === 'meta[name="build"]' ? {content: "b1"} : sel === '[aria-busy="true"]' ? streaming : null; },
  querySelectorAll: function () { return []; },
  addEventListener: function (type, fn) { (listeners[type] = listeners[type] || []).push(fn); },
  removeEventListener: function () {},
  dispatchEvent: function (e) { if (e.type === "streamopen") opened++; (listeners[e.type] || []).forEach(function (fn) { fn(e); }); }};
var sandbox = {EventSource: EventSource, URL: URL, URLSearchParams: URLSearchParams, CSS: {escape: function (s) { return s; }}, document: document,
  location: {hash: "", reload: function () { reloads++; }}, history: {state: null, replaceState: function () {}, pushState: function () {}},
  localStorage: {getItem: function () { return null; }, setItem: function () {}},
  Date: {now: function () { return clock; }},
  setTimeout: function (fn, ms) { timers.push([fn, ms]); }, setInterval: function () {}, clearTimeout: function () {},
  CustomEvent: function (type, init) { this.type = type; this.detail = init && init.detail; },
  fetch: function (url) { requests.push("fetch " + url); return new Promise(function () {}); },
  htmx: {ajax: function (verb, url) { requests.push(verb + " " + url); return new Promise(function (ok) { pending.push(ok); }); }, swap: function () {}, process: function () {}},
  window: {addEventListener: function (type, fn) { (winListeners[type] = winListeners[type] || []).push(fn); }}};
require("vm").runInNewContext(require("fs").readFileSync(process.argv[1], "utf8"), sandbox);
function fire(type, e) { (listeners[type] || []).forEach(function (fn) { fn(e || {}); }); }
function settle() {
  pending.splice(0).forEach(function (ok) { ok(); });
  return new Promise(function (done) { setImmediate(done); });
}
function xhr(status, csp, text) { return {status: status, responseText: text, getResponseHeader: function (h) { return h === "Content-Security-Policy" && csp ? "default-src 'self'" : null; }}; }
function flashed() { var f = byID.flash.hidden ? "" : byID.flash.textContent; byID.flash.hidden = true; return f; }
(async function () {
var out = {};
fire("DOMContentLoaded");
var es = sources[0];
es.readyState = 1;
es.onopen();
fire("htmx:responseError", {detail: {elt: byID.board, xhr: xhr(503, false, "upstream connect error or disconnect/reset before headers")}});
out.flash = [flashed()];
fire("htmx:responseError", {detail: {elt: el({id: "move"}), xhr: xhr(503, false, "upstream connect error or disconnect/reset before headers")}});
out.flash.push(flashed());
fire("htmx:responseError", {detail: {elt: el({id: "move"}), xhr: xhr(409, true, "The lane takes no card from you.\n")}});
out.flash.push(flashed());
fire("htmx:sendError", {detail: {elt: el({id: "move"}), xhr: xhr(0, false, "")}});
out.flash.push(flashed());
fire("htmx:responseError", {detail: {elt: el({id: "move"}), xhr: xhr(503, true, "cannot read the session")}});
out.flash.push(flashed());
fire("htmx:responseError", {detail: {elt: el({id: "move"}), xhr: xhr(413, false, "Payload Too Large")}});
out.flash.push(flashed());
// a roll's refused connect closes the stream
es.readyState = 2;
es.onerror();
out.timers = timers.map(function (t) { return t[1]; });
out.down = byID.live.textContent;
timers[0][0]();
out.closed = [requests.slice(), sources.length, !!es.closed];
es = sources[1];
es.readyState = 1;
es.onopen();
await settle();
out.reopened = [requests.slice(), opened, byID.live.textContent];
await settle();
// the tab coming back: a talking stream stays, a silent or closed one is reopened
requests = [];
clock += 59000;
fire("visibilitychange");
out.talking = [sources.length, requests.slice()];
clock += 2000;
fire("visibilitychange");
out.silent = [sources.length, requests.slice(), !!es.closed];
await settle();
es = sources[2];
es.readyState = 1;
es.onopen();
await settle();
es.readyState = 2;
document.hidden = true;
fire("visibilitychange");
out.hidden = sources.length;
document.hidden = false;
winListeners.online.forEach(function (fn) { fn({}); });
out.online = sources.length;
await settle();
es = sources[3];
es.readyState = 1;
es.onopen();
clock += 61000;
winListeners.pageshow.forEach(function (fn) { fn({persisted: true}); });
out.restored = sources.length;
await settle();
es = sources[4];
es.readyState = 1;
es.onopen();
await settle();
// the ping: its own build reloads nothing, another reloads once hidden,
// nothing typed and nothing streaming
es.on.ping({data: "b1"});
document.hidden = true;
fire("visibilitychange");
out.reloads = [reloads];
document.hidden = false;
es.on.ping({data: "b2"});
out.reloads.push(reloads);
var field = el({type: "text", value: "half a task", defaultValue: ""});
typed = [field];
document.hidden = true;
fire("visibilitychange");
out.reloads.push(reloads);
typed = [];
streaming = el();
fire("visibilitychange");
out.reloads.push(reloads);
streaming = null;
fire("visibilitychange");
out.reloads.push(reloads);
// the browser's own reconnect after a drop tells the chat; a stream moved
// to a new query reads the board and tells it nothing
var was = opened;
es.readyState = 0;
es.onerror();
es.readyState = 1;
es.onopen();
out.dropped = opened - was;
await settle();
await settle();
requests = [];
byID.board = el({id: "board", dataset: {project: "code", query: "?project=code&q=x"}});
fire("htmx:afterSwap", {detail: {target: byID.board}});
sources[sources.length - 1].onopen();
out.moved = [opened - was, requests.slice(), sources.length];
console.log(JSON.stringify(out));
})();`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver, "static/console.js")
	got, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, got)
	}
	const restarts = "The factory did not answer: it restarts after a deploy. Try again in a moment."
	want := `{"flash":["","` + restarts + `","The lane takes no card from you.","` + restarts + `","cannot read the session","Payload Too Large"],` +
		`"timers":[5000],"down":"The live stream is down; it reconnects by itself.",` +
		`"closed":[["GET /board?project=code"],2,true],` +
		`"reopened":[["GET /board?project=code","GET /board?project=code"],1,""],` +
		`"talking":[2,[]],"silent":[3,["GET /board?project=code"],true],` +
		`"hidden":3,"online":4,"restored":5,"reloads":[0,0,0,0,1],"dropped":1,"moved":[1,["GET /board?project=code&q=x"],6]}`
	if strings.TrimSpace(string(got)) != want {
		t.Errorf("console.js under node:\n%s\nwant\n%s", got, want)
	}
}

// TestConsoleViewEvent: the agent's view change (282) holds for the page
// as a click does. The event's query pushes the URL beside the open
// doc= and the drawer's hash and reads the board under it; with no board
// on the page it touches nothing. The card opens the drawer as its link
// does and the tab is clicked once the drawer's settle is in, a settle
// that never comes leaving the listener armed for the next one, a second
// event before it replacing the shot. Card ""
// closes the drawer. The theme works the theme's
// own button. Back reads the board under the entry's query and puts the
// entry's q in the box without running the find; an entry that differs
// from the board's query only by doc= or the fields' order reads nothing.
// The find's box takes the event's q when the read goes out, held or not,
// without an input event, and a query without q clears it; one Back
// supersedes, for the box waits for the query it names. An event whose
// query the board already stands on — a card, a tab or the theme alone —
// pushes no entry, the drawer's hash a card click leaves.
func TestConsoleViewEvent(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var listeners = {}, winListeners = {}, requests = [], pending = [], replaced = [], pushed = [], sources = [], locked = false, queued = [];
function swapped() { locked = false; queued = []; }
function el(props) {
  var e = {dataset: {}, hidden: false, textContent: "", innerHTML: "", children: [],
    querySelector: function () { return null; }, querySelectorAll: function () { return []; }, contains: function () { return false; },
    getAttribute: function () { return null; }, setAttribute: function () {}, addEventListener: function () {},
    appendChild: function (c) { this.children.push(c); return c; }};
  return Object.assign(e, props);
}
var tabBtn = el({clicks: 0, click: function () { this.clicks++; }});
var drawer = el({id: "drawer", hidden: true});
drawer.querySelector = function (sel) { return sel === 'button[hx-get*="tab=chat"]' ? tabBtn : null; };
var themeLight = el({dataset: {themeSet: "light"}, click: function () { this.clicks = (this.clicks || 0) + 1; }});
var themeSwitch = el({id: "theme-switch"});
themeSwitch.querySelectorAll = function () { return [themeLight]; };
var box = el({id: "q", value: ""}), typed = [];
box.dispatchEvent = function (ev) { typed.push(ev.type); };
var byID = {board: el({id: "board", dataset: {project: "code", query: "?project=code"}}), drawer: drawer,
  "theme-switch": themeSwitch, live: el(), q: box};
function EventSource(url) { this.url = url; this.on = {}; sources.push(this); }
EventSource.prototype.addEventListener = function (type, fn) { this.on[type] = fn; };
EventSource.prototype.close = function () { this.closed = true; };
EventSource.CLOSED = 2;
// place moves the page's URL as the history calls do in a browser
function place(path) {
  var u = new URL(path, sandbox.location.href);
  sandbox.location.href = u.href;
  sandbox.location.pathname = u.pathname;
  sandbox.location.search = u.search;
  sandbox.location.hash = u.hash;
}
var sandbox = {EventSource: EventSource, URL: URL, URLSearchParams: URLSearchParams, CSS: {escape: function (s) { return s; }},
  location: {href: "http://lab/?project=code&doc=docs/a.md", pathname: "/", search: "?project=code&doc=docs/a.md", hash: ""},
  history: {state: null,
    replaceState: function (state, title, path) { replaced.push([state, path]); place(path); },
    pushState: function (state, title, path) { pushed.push(path); place(path); }},
  localStorage: {getItem: function () { return null; }, setItem: function () {}},
  setTimeout: function () {}, setInterval: function () {}, clearTimeout: function () {},
  Event: function (type) { this.type = type; },
  CustomEvent: function (type, init) { this.type = type; this.detail = init && init.detail; },
  fetch: function (url) { requests.push("fetch " + url); return new Promise(function () {}); },
  htmx: {ajax: function (verb, url) {
    if (url.indexOf("/board") === 0 && locked) {
      queued.push(verb + " " + url);
      return Promise.resolve();
    }
    requests.push(verb + " " + url);
    if (url.indexOf("/drawer") > 0) byID["steer-input"] = el({value: ""});
    if (url.indexOf("/board") !== 0) return new Promise(function (ok) { pending.push(ok); });
    locked = true;
    return new Promise(function (ok) { pending.push(function () { swapped(); ok(); }); });
  }, swap: function () {}, process: function () {}},
  window: {addEventListener: function (type, fn) { (winListeners[type] = winListeners[type] || []).push(fn); }},
  document: {body: {classList: {toggle: function () {}}}, documentElement: {getAttribute: function () { return "dark"; }},
    getElementById: function (id) { return byID[id] || null; },
    querySelectorAll: function () { return []; },
    querySelector: function (sel) { return sel === 'button[data-theme-set="light"]' ? themeLight : null; },
    removeEventListener: function (type, fn) { listeners[type] = (listeners[type] || []).filter(function (f) { return f !== fn; }); },
    createElement: function () { return el(); },
    addEventListener: function (type, fn) { (listeners[type] = listeners[type] || []).push(fn); },
    dispatchEvent: function (e) { (listeners[e.type] || []).forEach(function (fn) { fn(e); }); }}};
require("vm").runInNewContext(require("fs").readFileSync(process.argv[1], "utf8"), sandbox);
listeners.DOMContentLoaded.forEach(function (fn) { fn(); });
function settle() {
  pending.splice(0).forEach(function (ok) { ok(); });
  return new Promise(function (done) { setImmediate(done); });
}
// fire reaches the document's listeners as htmx does: the element a swap
// landed in is the event's target and detail.target both
function fire(type, detail) { (listeners[type] || []).forEach(function (fn) { fn({type: type, target: detail && detail.target, detail: detail}); }); }
(async function () {
var out = {};
box.value = "old";
fire("view", {query: "?project=code&q=exp"});
out.pushed = pushed.slice();
out.box = box.value;
out.query = [byID.board.dataset.query, requests.slice()];
// a read in flight holds the next: it goes out once the first settles
fire("view", {query: "?project=code"});
await settle();
await settle();
out.queued = requests.slice();
out.queuedBox = box.value;
// the card opens the drawer as its link does, the tab once its swap is in
requests = [];
fire("view", {card: "t-9", tab: "chat"});
out.card = [sandbox.location.hash, byID.drawer.hidden];
winListeners.hashchange.forEach(function (fn) { fn({}); });
out.opened = [byID.drawer.hidden, requests.slice()];
// the tab waits for the drawer's settle, not its swap: a click inside the
// swap reaches no htmx event, so nothing is clicked yet
fire("htmx:afterSwap", {target: byID.drawer});
out.tabBeforeSettle = tabBtn.clicks;
fire("htmx:afterSettle", {target: byID.drawer});
fire("htmx:afterSettle", {target: byID.drawer});
out.tab = tabBtn.clicks;
fire("view", {card: ""});
out.closed = [byID.drawer.hidden, sandbox.location.hash, replaced.slice()];
fire("view", {theme: "light"});
out.theme = themeLight.clicks;
// a card or the theme alone, the event's query the board already stands
// on, pushes no entry: the drawer's hash is the one a card click leaves,
// and the theme pushes none at all
var entries = pushed.length, tabsBefore = tabBtn.clicks;
requests = [];
fire("view", {query: "?project=code", theme: "light"});
out.flatTheme = [pushed.length - entries, themeLight.clicks];
fire("view", {query: "?project=code", card: "t-9", tab: "chat"});
winListeners.hashchange.forEach(function (fn) { fn({}); });
out.flatCard = [pushed.length - entries, byID.drawer.hidden];
await settle();
await settle();
fire("htmx:afterSettle", {target: byID.drawer});
out.flatTab = tabBtn.clicks - tabsBefore;
// a page without a board: nothing pushed, nothing read, no throw
requests = [];
delete byID.board;
fire("view", {query: "?project=code"});
out.noboard = [pushed.length, requests.length];
// Back reads the board under the entry's query and puts the entry's q in
// the box without an input event
byID.board = el({id: "board", dataset: {project: "code", query: "?project=code&q=exp"}});
Object.assign(sandbox.location, {search: "?project=code"});
box.value = "exp";
requests = [];
winListeners.popstate.forEach(function (fn) { fn({}); });
out.popped = [typed.slice(), box.value, requests.slice()];
await settle();
await settle();
// an entry that differs from the board's query only by doc= or the
// fields' order names the same board: Back reads nothing, the box still
// takes the entry's q
Object.assign(sandbox.location, {search: "?doc=docs%2Fa.md&project=code"});
box.value = "left";
requests = [];
winListeners.popstate.forEach(function (fn) { fn({}); });
out.popDoc = [box.value, requests.length, byID.board.dataset.query];
await settle();
await settle();
byID.board.dataset.query = "?project=code&q=exp&view=wide";
Object.assign(sandbox.location, {search: "?project=code&view=wide&q=exp"});
requests = [];
winListeners.popstate.forEach(function (fn) { fn({}); });
out.popOrder = [byID.board.dataset.query, requests.length];
// a held view read whose q Back has replaced leaves the box the entry's:
// the sync waits for the query it names
requests = [];
fire("view", {query: "?project=code&q=lock"});
fire("view", {query: "?project=code&q=new"});
Object.assign(sandbox.location, {search: "?project=code&q=old"});
winListeners.popstate.forEach(function (fn) { fn({}); });
await settle();
await settle();
out.backBox = [box.value, requests.slice()];
// two card+tab events before any settle arm one shot, not two: the
// latest tab alone is clicked, once
var chatTab = el({clicks: 0, click: function () { this.clicks++; }}), actTab = el({clicks: 0, click: function () { this.clicks++; }});
drawer.querySelector = function (sel) {
  if (sel === 'button[hx-get*="tab=chat"]') return chatTab;
  if (sel === 'button[hx-get*="tab=activity"]') return actTab;
  return null;
};
fire("view", {query: "?project=code", card: "t-9", tab: "chat"});
fire("view", {query: "?project=code", card: "t-9", tab: "activity"});
fire("htmx:afterSettle", {target: byID.drawer});
fire("htmx:afterSettle", {target: byID.drawer});
out.twoShots = [chatTab.clicks, actTab.clicks];
out.streams = sources.length;
await settle();
console.log(JSON.stringify(out));
})();`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver, "static/console.js")
	got, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	want := `{"pushed":["/?project=code&q=exp&doc=docs%2Fa.md"],"box":"exp","query":["?project=code&q=exp",["GET /board?project=code&q=exp"]],` +
		`"queued":["GET /board?project=code&q=exp","GET /board?project=code"],"queuedBox":"","card":["#card=t-9",true],` +
		`"opened":[false,["GET /cards/t-9/drawer"]],"tabBeforeSettle":0,"tab":1,"closed":[true,"",[[null,"/?project=code&doc=docs%2Fa.md"]]],` +
		`"theme":1,` +
		`"flatTheme":[0,2],"flatCard":[0,false],"flatTab":1,"noboard":[2,0],"popped":[[],"",["GET /board?project=code"]],"popDoc":["",0,"?project=code"],` +
		`"popOrder":["?project=code&q=exp&view=wide",0],"backBox":["old",["GET /board?project=code&q=lock","GET /board?project=code&q=old"]],` +
		`"twoShots":[0,1],"streams":1}`
	if strings.TrimSpace(string(got)) != want {
		t.Errorf("console.js under node:\n%s\nwant\n%s", got, want)
	}
}

// The board's activity line: one line of the card's newest event where one
// exists (a task_status row in words), the escalation's question where it
// has none but waits on an answer, and nothing at all where it has neither.
func TestBoardActivityLine(t *testing.T) {
	r, ev := newRigWithEvents(t)
	r.projects.rows = append(r.projects.rows, factoryd.Project{Name: "code", Repository: "sf/code", MergeRule: "human",
		Objects: "factory", Agents: "coder", File: true, GitToken: "t", Process: "maintenance", Lanes: testProcesses()["maintenance"].Lanes})
	r.tasks.set("The agent asks: use a replica or a shard?",
		factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Lane: policy.LaneReady, Process: "maintenance",
			Objective: "fix the cache", Repository: "sf/code", IssueNumber: 11},
		factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Process: "maintenance",
			Objective: "add the feed", Repository: "sf/code", IssueNumber: 12, CreatedAt: time.Now()},
		factoryd.Task{ID: "t-b", Kind: "agent", Status: "backlog", Lane: policy.LaneBacklog, Process: "maintenance",
			Objective: "an improvement", Repository: "sf/code", IssueNumber: 13},
		factoryd.Task{ID: "t-rev", Kind: "agent", Status: "in-review", Process: "maintenance",
			Objective: "review it", Repository: "sf/code", IssueNumber: 14, CreatedAt: time.Now()},
		factoryd.Task{ID: "t-fin", Kind: "agent", Status: "done", Process: "maintenance",
			Objective: "finished", Repository: "sf/code", IssueNumber: 15})
	at := time.Now().Add(-5 * time.Minute)
	for _, e := range []factoryd.Event{
		{TaskID: "t-run", Kind: factoryd.EventCardMoved, Actor: "sebastian", Via: factoryd.ViaButton},
		{TaskID: "t-rev", Kind: factoryd.EventTaskStatus, Detail: map[string]any{"status": "in-review", "gate": "checks"}},
		{TaskID: "t-fin", Kind: factoryd.EventTaskStatus, Detail: map[string]any{"status": "done", "gate": ""}},
	} {
		if e.Actor == "" {
			e.Actor, e.Via = factoryd.FactoryDecider, factoryd.ViaFactoryd
		}
		e.Repository, e.At = "sf/code", at
		if _, err := ev.Record(context.Background(), e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	card := func(id string) string {
		i := strings.Index(body, `data-card="`+id+`"`)
		if i < 0 {
			i = strings.Index(body, `/tasks/`+id+`/timeline`)
		}
		if i < 0 {
			t.Fatalf("card %s missing from the board", id)
		}
		start := strings.LastIndex(body[:i], "<article")
		end := strings.Index(body[i:], "</article>")
		return body[start : i+end]
	}
	if got := card("t-run"); !strings.Contains(got, `class="activity">card moved by sebastian via button at `+at.UTC().Format("15:04")+`<`) {
		t.Errorf("the card with an event: %.400s", got)
	}
	if got := card("t-e"); !strings.Contains(got, `class="activity">use a replica or a shard?<`) {
		t.Errorf("the escalated card without an event lacks its question: %.400s", got)
	}
	hhmm := at.UTC().Format("15:04")
	for id, want := range map[string]string{"t-rev": "status in-review, gate checks, at " + hhmm} {
		if got := card(id); !strings.Contains(got, `class="activity">`+want+`<`) {
			t.Errorf("the card whose newest event is its status lacks %q: %.400s", want, got)
		}
	}
	if got := card("t-fin"); strings.Contains(got, `activity`) {
		t.Errorf("a finished card claims an activity line: %.400s", got)
	}
	if got := card("t-b"); strings.Contains(got, `activity`) {
		t.Errorf("a card with neither claims an activity line: %.400s", got)
	}
}
