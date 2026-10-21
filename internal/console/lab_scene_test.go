package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// labGates are the gates a task row names.
var labGates = []string{factoryd.GateIntake, factoryd.GateSession, factoryd.GateChecks, factoryd.GatePolicy, factoryd.GateMigration,
	factoryd.GateReproduction, factoryd.GateCriteria, factoryd.GateReview, factoryd.GateApproval, factoryd.GateMerge, factoryd.GateHuman,
	factoryd.GateCommissioning, factoryd.GateScreening, factoryd.GateAudit, factoryd.GateProvider, factoryd.GateDependencies,
	factoryd.GateMergeQueue}

// labReply is one scripted answer of the model.
type labReply []labChunk

type labChunk struct {
	Say  string
	Call [][2]string
}

// labSceneJSON is labScene as a recording script posts it to /lab/scene.
type labSceneJSON struct {
	Project       labProject
	Remote        string
	Projects      []factoryd.Project
	Config        *policy.ProjectConfig
	MonthCap      float64
	PRFiles       map[string]string
	Tasks         []factoryd.BoardTask
	Deps          map[string][]string
	Plans         map[string][]factoryd.PlanRevision
	Questions     map[string][]factoryd.PlanQuestion
	Approvals     []factoryd.ApprovalRequest
	Objects       map[string]string
	Events        []factoryd.Event
	Chat          []factoryd.ChatMessage
	Proposals     []factoryd.ChatProposal
	UsedProposals []factoryd.ChatProposal
	Spend         float64
	ChatSpend     float64
	GitHub        string
	Alerts        []factoryd.Alert
	Drawer        string
	Autonomy      string
	Seen          map[int64]int64
	Working       []int64
	Inbox, Open   int64
	Archived      []int64
	Routines      []factoryd.RoutineView
	Runs          map[string][]factoryd.RoutineRun
	RoutinesAt    time.Time
	Metrics       factoryd.MetricsReport
	Costs         *policy.Costs
	Replies       []labReply
}

func (j labSceneJSON) scene() labScene {
	return labScene{project: j.Project, remote: j.Remote, projects: j.Projects, config: j.Config, monthCap: j.MonthCap,
		prFiles: j.PRFiles, tasks: j.Tasks, deps: j.Deps, plans: j.Plans, questions: j.Questions, approvals: j.Approvals,
		objects: j.Objects, events: j.Events, chat: j.Chat, proposals: j.Proposals, usedProposals: j.UsedProposals,
		spend: j.Spend, chatSpend: j.ChatSpend, github: j.GitHub, alerts: j.Alerts, drawer: j.Drawer, autonomy: j.Autonomy,
		seen: j.Seen, working: j.Working, inbox: j.Inbox, open: j.Open, archived: j.Archived, routines: j.Routines,
		runs: j.Runs, routinesAt: j.RoutinesAt, metrics: j.Metrics, costs: j.Costs, replies: j.Replies}
}

// check refuses a scene the console never holds: a status, gate or lane
// outside its list, a time after now, a reference to a task the scene
// lacks, a task without its timeline object.
func (j labSceneJSON) check(now time.Time) error {
	ids := map[string]bool{}
	for _, t := range j.Tasks {
		ids[t.ID] = true
	}
	for _, t := range j.Tasks {
		if !slices.Contains(factoryd.Statuses, t.Status) {
			return fmt.Errorf("task %s: status %q is none of %v", t.ID, t.Status, factoryd.Statuses)
		}
		if t.Gate != "" && !slices.Contains(labGates, t.Gate) {
			return fmt.Errorf("task %s: gate %q is none of %v", t.ID, t.Gate, labGates)
		}
		if !slices.Contains(policy.LaneNames, t.Lane) {
			return fmt.Errorf("task %s: lane %q is none of %v", t.ID, t.Lane, policy.LaneNames)
		}
	}
	late := func(what string, at time.Time) error {
		if at.After(now) {
			return fmt.Errorf("%s %s is after now", what, at.Format(time.RFC3339))
		}
		return nil
	}
	var errs []error
	for _, t := range j.Tasks {
		errs = append(errs, late("task "+t.ID+" CreatedAt", t.CreatedAt))
	}
	for _, e := range j.Events {
		errs = append(errs, late("event At", e.At))
	}
	for _, m := range j.Chat {
		errs = append(errs, late("chat message At", m.At))
	}
	for _, a := range j.Alerts {
		errs = append(errs, late("alert RaisedAt", a.RaisedAt))
	}
	for _, a := range j.Approvals {
		errs = append(errs, late("approval CreatedAt", a.CreatedAt))
	}
	known := func(what, id string) error {
		if id != "" && !ids[id] {
			return fmt.Errorf("%s %q names no task of the scene", what, id)
		}
		return nil
	}
	for _, t := range j.Tasks {
		errs = append(errs, known("parent", t.Parent), known("roll", t.RollID))
	}
	for k, vs := range j.Deps {
		errs = append(errs, known("dependency key", k))
		for _, v := range vs {
			errs = append(errs, known("dependency", v))
		}
	}
	for _, e := range j.Events {
		errs = append(errs, known("event task", e.TaskID))
	}
	for _, a := range j.Alerts {
		errs = append(errs, known("alert task", a.TaskID))
	}
	errs = append(errs, known("Drawer", j.Drawer))
	if c := j.Costs; c != nil {
		day := func(what, v string) error {
			if _, err := time.Parse(policy.DateLayout, v); err != nil {
				return fmt.Errorf("costs %s %q is no date in YYYY-MM-DD form", what, v)
			}
			return nil
		}
		errs = append(errs, day("from", c.From), day("to", c.To))
		for _, d := range c.Days {
			errs = append(errs, day("day", d.Day))
		}
		for _, tc := range c.Tasks {
			errs = append(errs, known("costs task", tc.TaskID))
		}
	}
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	for _, t := range j.Tasks {
		if _, ok := j.Objects["sessions/"+t.ID+"/timeline.jsonl"]; !ok {
			return fmt.Errorf("task %s lacks the object sessions/%s/timeline.jsonl", t.ID, t.ID)
		}
	}
	return nil
}

func TestLabSceneJSON(t *testing.T) {
	r := newRig(t)
	now := time.Now()
	body := fmt.Sprintf(`{"Project":{"Name":"nord-booking","Repository":"nord/booking","Login":"sebastian"},
	 "Tasks":[{"ID":"t-1","Kind":"agent","Status":"running","Lane":"ready","Repository":"nord/booking","IssueNumber":12,
	   "Objective":"Waitlist","CreatedAt":%q,"Gate":"session"}],
	 "Objects":{"sessions/t-1/timeline.jsonl":""}}`, now.Add(-time.Hour).Format(time.RFC3339))
	var j labSceneJSON
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		t.Fatal(err)
	}
	if err := j.check(now); err != nil {
		t.Fatal(err)
	}
	r.seedScene(j.scene())
	if got := r.tasks.tasks[0].Status; got != "running" {
		t.Fatalf("status %q", got)
	}
}

func TestLabSceneJSONWaitingDeps(t *testing.T) {
	var j labSceneJSON
	body := `{"Tasks":[{"ID":"t-1","Status":"waiting-deps","Lane":"ready","Gate":"dependencies","CreatedAt":"2026-01-01T00:00:00Z"}],"Objects":{"sessions/t-1/timeline.jsonl":""}}`
	if err := json.Unmarshal([]byte(body), &j); err != nil {
		t.Fatal(err)
	}
	if err := j.check(time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestLabSceneJSONRefuses(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct{ name, body, want string }{
		{"status", `{"Tasks":[{"ID":"t-1","Status":"flying","Lane":"ready","CreatedAt":"2026-01-01T00:00:00Z"}],"Objects":{"sessions/t-1/timeline.jsonl":""}}`, `status "flying"`},
		{"gate", `{"Tasks":[{"ID":"t-1","Status":"running","Lane":"ready","Gate":"flying","CreatedAt":"2026-01-01T00:00:00Z"}],"Objects":{"sessions/t-1/timeline.jsonl":""}}`, `gate "flying"`},
		{"timeline", `{"Tasks":[{"ID":"t-1","Status":"running","Lane":"ready","CreatedAt":"2026-01-01T00:00:00Z"}]}`, `sessions/t-1/timeline.jsonl`},
		{"future", fmt.Sprintf(`{"Tasks":[{"ID":"t-1","Status":"backlog","Lane":"backlog","CreatedAt":%q}],"Objects":{"sessions/t-1/timeline.jsonl":""}}`, now.Add(time.Hour).Format(time.RFC3339)), "CreatedAt"},
		{"parent", `{"Tasks":[{"ID":"t-1","Status":"backlog","Lane":"backlog","Parent":"t-9","CreatedAt":"2026-01-01T00:00:00Z"}],"Objects":{"sessions/t-1/timeline.jsonl":""}}`, `parent "t-9"`},
		{"roll", `{"Tasks":[{"ID":"t-1","Status":"merged","Lane":"ready","RollID":"t-9","CreatedAt":"2026-01-01T00:00:00Z"}],"Objects":{"sessions/t-1/timeline.jsonl":""}}`, `roll "t-9"`},
	} {
		var j labSceneJSON
		if err := json.Unmarshal([]byte(tc.body), &j); err != nil {
			t.Fatal(tc.name, err)
		}
		err := j.check(now)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q in it", tc.name, err, tc.want)
		}
	}
}

func TestLabSceneChatReplies(t *testing.T) {
	r, _, g := newChatRig(t)
	g.lenient = true
	g.script("nord-booking", []labReply{
		{{Say: "A waitlist, "}, {Say: "noted."}},
		{{Call: [][2]string{{"board", `{}`}}}, {Call: [][2]string{{"new_card", `{"title":"Waitlist"}`}}}},
		{},
	})
	ck, s := r.loggedIn(t)
	code, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"add a waitlist"}})
	equal(t, "stream", []any{code, events}, []any{200, []string{`text {"text":"A waitlist, "}`, `text {"text":"noted."}`, `done {}`}})
	rows, _ := r.chat.ChatMessages(t.Context(), "sf/code", "sebastian", 0, 20)
	if last := rows[len(rows)-1]; last.Role != "assistant" || last.Content != "A waitlist, noted." {
		t.Fatalf("last row %+v, want the scripted answer", last)
	}

	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"and a card?"}})
	rows, _ = r.chat.ChatMessages(t.Context(), "sf/code", "sebastian", 0, 20)
	var names, args []string
	for _, row := range rows {
		if row.Role != "assistant" {
			continue
		}
		for _, c := range row.ToolCalls {
			names, args = append(names, c.Name), append(args, c.Arguments)
		}
	}
	equal(t, "tool calls of the two chunks", []any{names[:2], args[:2]}, []any{[]string{"board", "new_card"}, []string{`{}`, `{"title":"Waitlist"}`}})

	before := len(g.calls())
	code, events, _ = chatPost(t, r, ck, s, chatPath, url.Values{"message": {"and now?"}})
	equal(t, "unscripted stream", []any{code, events, len(g.calls()) - before}, []any{200, []string{`error {"message":"The model could not be reached; try again."}`}, 1})
	rows, _ = r.chat.ChatMessages(t.Context(), "sf/code", "sebastian", 0, 20)
	if last := rows[len(rows)-1]; last.Role == "assistant" && last.Content != "" {
		t.Fatalf("the unscripted turn left an assistant row %+v", last)
	}
}

// nordScene is a scene of the demo project nord/booking holding tasks.
func nordScene(tasks ...factoryd.BoardTask) labScene {
	objects := map[string]string{}
	for _, t := range tasks {
		objects["sessions/"+t.ID+"/timeline.jsonl"] = ""
	}
	return labScene{project: labProject{"nord-booking", "nord/booking", "sebastian"},
		projects: []factoryd.Project{{Name: "nord-booking", Repository: "nord/booking", MergeRule: "human", Objects: "factory", Agents: "coder",
			GitToken: "minted", Process: "spec", Lanes: testProcesses()["spec"].Lanes, File: true}},
		tasks: tasks, objects: objects}
}

// nordCard is card #n of nord/booking in status and lane.
func nordCard(n int64, status, lane string) factoryd.BoardTask {
	return factoryd.BoardTask{Task: factoryd.Task{ID: labID(n), Kind: "agent", Status: status, Lane: lane, Process: "spec",
		Repository: "nord/booking", IssueNumber: n, Objective: fmt.Sprintf("Card %d", n), CreatedAt: time.Now().Add(-time.Hour)}}
}

// The New task form's File and a new_card proposal's Do it file issues on
// the scene's repository, numbered on from the scene's highest, and each
// lands on the board as its card.
func TestLabSceneFilesIssue(t *testing.T) {
	r, _, _ := newChatRig(t)
	token := strings.Repeat("p", 43)
	s := nordScene(nordCard(102, "running", policy.LaneReady), nordCard(101, "done", policy.LaneReady))
	s.proposals = []factoryd.ChatProposal{{Token: token, Kind: "proposal", Repository: "nord/booking", Login: "sebastian",
		Tool: "new_card", Args: json.RawMessage(`{"title":"Export","body":"Export the bookings.","lane":"backlog"}`),
		ExpiresAt: time.Now().Add(time.Hour), Conversation: 1}}
	r.seedScene(s)
	ck, sess := r.loggedIn(t)
	rec := r.do(post("/tasks", url.Values{"csrf": {sess.csrf}, "project": {"nord-booking"}, "title": {"Waitlist"},
		"body": {"Guests wait for a free slot.\n\nAcceptance:\n- A freed slot goes to the first guest"}, "lane": {"ready"}}), ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?filed=103&project=nord-booking" {
		t.Fatalf("POST /tasks = %d to %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	code, events, _ := chatPost(t, r, ck, sess, "/projects/nord-booking/chat/proposals/"+token, nil)
	if code != http.StatusOK || !slices.Contains(events, `applied {"result":"Filed nord/booking#104 in lane backlog.","tool":"new_card"}`) {
		t.Fatalf("Do it = %d %q, want #104 filed", code, events)
	}
	rec = r.do(post("/tasks", url.Values{"csrf": {sess.csrf}, "project": {"nord-booking"}, "title": {"Call the venue"}, "lane": {"human"}}), ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?filed=105&project=nord-booking" {
		t.Fatalf("POST /tasks of a human card = %d to %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	r.fg.mu.Lock()
	var titles []any
	for _, issue := range r.fg.issues {
		titles = append(titles, issue["title"])
	}
	r.fg.mu.Unlock()
	equal(t, "the issues filed", titles, []any{"Waitlist", "Export", "Call the venue"})
	r.tasks.mu.Lock()
	var cards []string
	for _, bt := range r.tasks.tasks {
		cards = append(cards, fmt.Sprint(bt.ID, " #", bt.IssueNumber, " ", bt.Kind, " ", bt.Status, " ", bt.Lane, " ", bt.Repository))
	}
	r.tasks.mu.Unlock()
	equal(t, "the cards", cards, []string{labID(102) + " #102 agent running ready nord/booking", labID(101) + " #101 agent done ready nord/booking",
		labID(103) + " #103 agent queued ready nord/booking", labID(104) + " #104 agent backlog backlog nord/booking",
		labID(105) + " #105 human waiting-human  nord/booking"})
	board := r.do(httptest.NewRequest(http.MethodGet, "/?project=nord-booking", nil), ck).Body.String()
	for _, n := range []int64{103, 104, 105} {
		if !strings.Contains(board, `href="#card=`+labID(n)+`"`) {
			t.Errorf("the board lacks card #%d", n)
		}
	}
}

// After a seed, the rows the chat stores during a turn carry the real
// clock, so the thread's head reads "now", not the rig's fixed date.
func TestLabSceneChatRowsNow(t *testing.T) {
	r, chat, g := newChatRig(t)
	g.lenient, r.gateway = true, g
	s := nordScene()
	s.replies = []labReply{{{Say: "Noted."}}}
	r.seedScene(s)
	ck, sess := r.loggedIn(t)
	if code, events, _ := chatPost(t, r, ck, sess, "/projects/nord-booking/chat", url.Values{"message": {"add a waitlist"}}); code != http.StatusOK {
		t.Fatalf("the turn = %d %q", code, events)
	}
	rows, _ := chat.ChatMessages(t.Context(), "nord/booking", "sebastian", 0, 20)
	if len(rows) != 2 {
		t.Fatalf("%d rows, want the ask and the answer: %+v", len(rows), rows)
	}
	for _, m := range rows {
		if d := time.Since(m.At); d < 0 || d > 5*time.Second {
			t.Errorf("row %d %q at %v, want within 5 s of now", m.ID, m.Content, m.At)
		}
	}
}

// A seed wakes the open streams: a board that showed no card redraws on
// the seed that adds one, as a board with cards does on its heartbeat.
// console.js redraws the board on event board and on an event row
// without a card.
func TestLabSceneWakesEmptyBoard(t *testing.T) {
	r := newRig(t)
	r.seedScene(nordScene())
	_, br := streamAt(t, r, "nord-booking", "", "")
	r.seedScene(nordScene(nordCard(101, "queued", policy.LaneReady)))
	seeded := time.Now()
	for {
		got := nextMessage(t, br)
		if time.Since(seeded) > 1500*time.Millisecond {
			t.Fatalf("no board redraw within 1.5 s of the seed; then %q", got)
		}
		var e struct {
			TaskID string `json:"task_id"`
			Kind   string `json:"kind"`
		}
		if slices.Contains(got, "event: board") ||
			slices.Contains(got, "event: event") && json.Unmarshal([]byte(strings.TrimPrefix(got[len(got)-1], "data: ")), &e) == nil &&
				e.TaskID == "" && e.Kind != factoryd.EventSessionSaid && e.Kind != factoryd.EventDocWritten {
			return
		}
	}
}

func TestLabSceneCosts(t *testing.T) {
	now := time.Now()
	body := `{"Tasks":[{"ID":"t-1","Status":"backlog","Lane":"backlog","CreatedAt":"2026-01-01T00:00:00Z"}],"Objects":{"sessions/t-1/timeline.jsonl":""},
		"Costs":{"from":"2026-10-01","to":"2026-10-02","days":[{"day":"2026-10-01","by_kind":{"model":1.5},"total":1.5},{"day":"2026-10-02","by_kind":{"model":2},"total":2}],
		"tasks":[{"task_id":"t-1","eur":3.5,"lines":[{"kind":"model","ref":"big","eur":3.5}]}]}}`
	var j labSceneJSON
	if err := json.Unmarshal([]byte(body), &j); err != nil {
		t.Fatal(err)
	}
	if err := j.check(now); err != nil {
		t.Fatal(err)
	}
	r := newRig(t)
	r.seedScene(j.scene())
	if !reflect.DeepEqual(r.ledger.costs, *j.Costs) {
		t.Fatalf("costs %+v, want the scene's %+v", r.ledger.costs, *j.Costs)
	}

	r.seedScene(labScene{spend: 40})
	if got := len(r.ledger.costs.Days); got != 14 {
		t.Fatalf("scene without costs: %d days, want 14", got)
	}

	for _, tc := range []struct{ name, costs, want string }{
		{"day", `{"from":"2026-10-01","to":"2026-10-02","days":[{"day":"1 Oct"}]}`, `"1 Oct"`},
		{"from", `{"from":"yesterday","to":"2026-10-02"}`, `"yesterday"`},
		{"to", `{"from":"2026-10-01","to":"2026-13-02"}`, `"2026-13-02"`},
		{"task", `{"from":"2026-10-01","to":"2026-10-02","tasks":[{"task_id":"t-9"}]}`, `"t-9"`},
	} {
		var j labSceneJSON
		if err := json.Unmarshal([]byte(`{"Costs":`+tc.costs+`}`), &j); err != nil {
			t.Fatal(tc.name, err)
		}
		if err := j.check(now); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %s in it", tc.name, err, tc.want)
		}
	}
}
