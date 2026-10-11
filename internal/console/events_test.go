package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// fakeEvents is factoryd.Events: it keeps every row and answers a project's
// feed and the newest event of each card. Like PGStore it refuses an event
// with no repository or no kind, so a handler that forgets the project of
// its event fails here instead of passing silently.
type fakeEvents struct {
	mu   sync.Mutex
	rows []factoryd.Event
	next int64
}

func (f *fakeEvents) Record(_ context.Context, e factoryd.Event) (int64, error) {
	if e.Repository == "" || e.Kind == "" {
		return 0, fmt.Errorf("event: repository and kind are required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	e.ID = f.next
	if e.At.IsZero() {
		e.At = time.Now()
	}
	f.rows = append(f.rows, e)
	return e.ID, nil
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

func (f *fakeEvents) ButtonCommentIDs(_ context.Context, taskID string) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int64
	for i := len(f.rows) - 1; i >= 0; i-- {
		e := f.rows[i]
		if e.TaskID == taskID && e.Kind == factoryd.EventQuestionAnswered && e.Via == factoryd.ViaButton {
			if id, ok := e.Detail["comment"].(int64); ok {
				out = append(out, id)
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
	ev := &fakeEvents{}
	r.c.cfg.Events = ev
	return r, ev
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
	setup   func(*rig)
	request func(*rig, *session) *http.Request
}

// boardAction posts form to path, as the board does: htmx off, so a
// handler that shows the board answers 303.
func boardAction(name, path, kind, repo, taskID string, detail map[string]any, setup func(*rig), form url.Values) action {
	return action{name: name, kind: kind, repo: repo, taskID: taskID, detail: detail, setup: setup,
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

// actions are the fifteen POST handlers, one per event kind of the spine.
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
	boardAction("setAccent", "/tasks/t-m/accent", factoryd.EventAccentSet, "sf/code", "t-m",
		map[string]any{"accent": "blue"}, backlogCard, url.Values{"value": {"blue"}}),
	boardAction("promote", "/resources/11/promote", factoryd.EventResourcePromoted, "sf/code", "t-res",
		map[string]any{"resource": 11, "kind": "server"}, standingResource, url.Values{}),
	boardAction("release", "/resources/11/release", factoryd.EventResourceReleased, "sf/code", "t-res",
		map[string]any{"resource": 11, "kind": "server"}, standingResource, url.Values{}),
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
			if a.setup != nil {
				a.setup(r)
			}
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

// The reject path of deny: an action neither request_changes nor reject is
// refused with 400 and leaves no row.
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

// A handler that records an event with no project is refused by the store
// (PGStore and this fake alike), not silently dropped: the feed of the
// infra repository stays empty and the row never exists.
func TestFakeEventsRejectAnEmptyRepository(t *testing.T) {
	ev := &fakeEvents{}
	if _, err := ev.Record(context.Background(), factoryd.Event{Kind: factoryd.EventCardFiled, Actor: "a", Via: factoryd.ViaButton}); err == nil {
		t.Fatal("an event with no repository was accepted")
	}
	if _, err := ev.Record(context.Background(), factoryd.Event{Repository: "sf/code", Actor: "a", Via: factoryd.ViaButton}); err == nil {
		t.Fatal("an event with no kind was accepted")
	}
	if ev.count() != 0 {
		t.Fatalf("rows = %d, want none", ev.count())
	}
}

// The board's activity line: one line of the card's newest event where one
// exists, the escalation's question where it has none but waits on an
// answer, and nothing at all where it has neither.
func TestBoardActivityLine(t *testing.T) {
	r, ev := newRigWithEvents(t)
	r.projects.rows = append(r.projects.rows, factoryd.Project{Name: "code", Repository: "sf/code", MergeRule: "human",
		Objects: "factory", Agents: "coder", File: true, GitToken: "t", Process: "maintenance", Lanes: testProcesses()["maintenance"].Lanes})
	r.tasks.set("The agent asks: use a replica or a shard?",
		factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Lane: policy.LaneReady, Process: "maintenance",
			Objective: "fix the cache", Repository: "sf/code", IssueNumber: 11},
		factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Process: "maintenance",
			Objective: "add the feed", Repository: "sf/code", IssueNumber: 12},
		factoryd.Task{ID: "t-b", Kind: "agent", Status: "backlog", Lane: policy.LaneBacklog, Process: "maintenance",
			Objective: "an improvement", Repository: "sf/code", IssueNumber: 13})
	at := time.Now().Add(-5 * time.Minute)
	if _, err := ev.Record(context.Background(), factoryd.Event{Repository: "sf/code", TaskID: "t-run", Kind: factoryd.EventCardMoved,
		Actor: "sebastian", Via: factoryd.ViaButton, At: at}); err != nil {
		t.Fatalf("Record: %v", err)
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
	if got := card("t-run"); !strings.Contains(got, `class="activity">card_moved by sebastian via button at `+at.UTC().Format("15:04")+`<`) {
		t.Errorf("the card with an event: %.400s", got)
	}
	if got := card("t-e"); !strings.Contains(got, `class="activity">use a replica or a shard?<`) {
		t.Errorf("the card without an event falls back to no escalation text: %.400s", got)
	}
	if got := card("t-b"); strings.Contains(got, `activity`) {
		t.Errorf("a card with neither claims an activity line: %.400s", got)
	}
}
