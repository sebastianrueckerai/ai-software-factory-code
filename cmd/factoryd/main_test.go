package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/hcloud"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func TestRequireBearer(t *testing.T) {
	h := requireBearer("s3cret", func(w http.ResponseWriter, r *http.Request) {})
	for header, want := range map[string]int{
		"":               http.StatusUnauthorized,
		"s3cret":         http.StatusUnauthorized,
		"Bearer wrong":   http.StatusUnauthorized,
		"Bearer s3cret2": http.StatusUnauthorized,
		"Bearer s3cre":   http.StatusUnauthorized,
		"Basic s3cret":   http.StatusUnauthorized,
		"Bearer s3cret":  http.StatusOK,
	} {
		req := httptest.NewRequest(http.MethodPost, "/tasks", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Authorization %q: status %d, want %d", header, rec.Code, want)
		}
	}
}

type escalationStub struct{ delivered int }

func (e *escalationStub) Task(context.Context, string) (factoryd.Task, bool, error) {
	return factoryd.Task{ID: "t1", Status: "running"}, true, nil
}

func (e *escalationStub) InsertIntakeEvent(context.Context, factoryd.IntakeEvent) (int64, error) {
	return 1, nil
}

func (e *escalationStub) EscalateSession(context.Context, string, factoryd.AgentAsk) error {
	e.delivered++
	return nil
}

// Only the gateway's bearer reaches the escalation handler; the intake
// token does not.
func TestEscalationRouteNeedsTheGatewayToken(t *testing.T) {
	stub := &escalationStub{}
	h := escalationRoute("gw-token", stub, stub)
	for header, want := range map[string]int{
		"":                  http.StatusUnauthorized,
		"Bearer intake":     http.StatusUnauthorized,
		"Bearer gw-token":   http.StatusAccepted,
		"Bearer gw-token\n": http.StatusUnauthorized,
	} {
		req := httptest.NewRequest(http.MethodPost, "/internal/escalations", strings.NewReader(`{"task_id":"t1","kind":"escalation","question":"q","reason":"r","recommendation":"x"}`))
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Authorization %q: status %d, want %d", header, rec.Code, want)
		}
	}
	if stub.delivered != 1 {
		t.Fatalf("delivered %d escalations, want 1", stub.delivered)
	}
}

// createTaskForgejoStub answers CreateIssue and ReplaceIssueLabels, the two
// Forgejo calls createTaskHandler makes; it appends into the shared events
// log passed in so a test can assert call order against the fake store's
// own CreateTask.
func createTaskForgejoStub(t *testing.T, events *[]string, failIssueCreate bool) (forgejo *factoryd.Forgejo, issueBody *string, appliedLabels *[]string) {
	t.Helper()
	issueBody = new(string)
	appliedLabels = new([]string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			*events = append(*events, "create-issue")
			if failIssueCreate {
				http.Error(w, "forgejo unavailable", http.StatusInternalServerError)
				return
			}
			var payload struct{ Title, Body string }
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode issue create payload: %v", err)
			}
			*issueBody = payload.Body
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"number":42,"html_url":"https://git.example/sf/repo/issues/42"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/sf/ai-software-factory-code":
			io.WriteString(w, `{"default_branch":"main"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main"):
			io.WriteString(w, `{"commit":{"id":"abc123"}}`)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/labels"):
			*events = append(*events, "replace-labels")
			var payload struct {
				Labels []string `json:"labels"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode label payload: %v", err)
			}
			*appliedLabels = payload.Labels
			io.WriteString(w, `[]`)
		default:
			t.Fatalf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return &factoryd.Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}, issueBody, appliedLabels
}

// fakeTaskCreator is the taskCreator test double: it records every row
// created (and the call in the shared events log for ordering assertions),
// or, with err set, simulates a store failure without creating a row.
type fakeTaskCreator struct {
	events  *[]string
	err     error
	created []factoryd.Task
	// projects by repository; nil: every repository is a seed project
	// agents work on, following a process with a direct track
	projects map[string]factoryd.Project
}

func (s *fakeTaskCreator) ProjectByRepository(_ context.Context, repository string) (factoryd.Project, bool, error) {
	if s.projects == nil {
		return factoryd.Project{Name: "p", Repository: repository, Objects: "seed", Agents: "coder", File: true, Process: "maintenance",
			Tracks: policy.Tracks{Direct: policy.Track{Class: policy.ClassCoder}}}, true, nil
	}
	p, ok := s.projects[repository]
	return p, ok, nil
}

func (s *fakeTaskCreator) CreateTask(ctx context.Context, t factoryd.Task) error {
	*s.events = append(*s.events, "create-row")
	if s.err != nil {
		return s.err
	}
	s.created = append(s.created, t)
	return nil
}

// fakeStarter is the workflow seam: it logs starts into the shared events
// log.
type fakeStarter struct{ events *[]string }

func (w *fakeStarter) Start(ctx context.Context, taskID string) error {
	*w.events = append(*w.events, "start-workflow")
	return nil
}

func (w *fakeStarter) Signal(ctx context.Context, taskID, name string, sig factoryd.TaskSignal) error {
	return nil
}

func (w *fakeStarter) StartRoutineRun(ctx context.Context, in factoryd.RoutineRunInput) error {
	return nil
}

func (w *fakeStarter) Reopen(ctx context.Context, taskID, signal string, sig factoryd.TaskSignal) error {
	*w.events = append(*w.events, "reopen-workflow")
	return nil
}

func postCreateTask(h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCreateTaskHandlerFilesIssueBeforeRow(t *testing.T) {
	var events []string
	forgejo, _, _ := createTaskForgejoStub(t, &events, false)
	store := &fakeTaskCreator{events: &events}
	rec := postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, nil, testBudget), `{"objective":"Add X","repository":"sf/ai-software-factory-code"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	want := []string{"create-issue", "replace-labels", "create-row", "start-workflow"}
	if !slices.Equal(events, want) {
		t.Fatalf("call order = %v, want %v (issue filed, then labelled, then the row created, then its workflow started)", events, want)
	}
}

func TestCreateTaskHandlerResponseCarriesIssueRef(t *testing.T) {
	var events []string
	forgejo, _, _ := createTaskForgejoStub(t, &events, false)
	store := &fakeTaskCreator{events: &events}
	rec := postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, nil, testBudget), `{"objective":"Add X","repository":"sf/ai-software-factory-code"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var res struct {
		IssueNumber int64  `json:"issue_number"`
		IssueURL    string `json:"issue_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.IssueNumber != 42 || res.IssueURL != "https://git.example/sf/repo/issues/42" {
		t.Fatalf("response = %+v, want issue_number 42 and its URL", res)
	}
	if len(store.created) != 1 || store.created[0].IssueNumber != 42 || store.created[0].Source != "api" {
		t.Fatalf("created task = %+v, want IssueNumber 42", store.created)
	}
}

// The API intake stamps the base like the webhook path: the default
// branch's head, always. Neither the branch nor the sha is a caller's
// choice; a sha in the body is ignored.
func TestCreateTaskHandlerStampsBaseSHA(t *testing.T) {
	for body, want := range map[string]string{
		`{"objective":"Add X","repository":"sf/ai-software-factory-code"}`:                "abc123",
		`{"objective":"Add X","repository":"sf/ai-software-factory-code","sha":"given1"}`: "abc123",
	} {
		var events []string
		forgejo, _, _ := createTaskForgejoStub(t, &events, false)
		store := &fakeTaskCreator{events: &events}
		rec := postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, nil, testBudget), body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		if len(store.created) != 1 || store.created[0].SHA != want {
			t.Fatalf("created = %+v, want sha %q", store.created, want)
		}
		if strings.Contains(rec.Body.String(), `"branch"`) {
			t.Fatalf("response = %s, want no branch field", rec.Body)
		}
	}
}

func TestCreateTaskHandlerLabelsAgentAndQueued(t *testing.T) {
	var events []string
	forgejo, _, appliedLabels := createTaskForgejoStub(t, &events, false)
	store := &fakeTaskCreator{events: &events}
	rec := postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, nil, testBudget), `{"objective":"Add X","repository":"sf/ai-software-factory-code"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if !slices.Equal(*appliedLabels, []string{"agent", "state/queued"}) {
		t.Fatalf("labels = %v, want [agent state/queued]", *appliedLabels)
	}
}

func TestCreateTaskHandlerIssueBodyIsObjectivePlusFiledByFactoryd(t *testing.T) {
	var events []string
	forgejo, issueBody, _ := createTaskForgejoStub(t, &events, false)
	store := &fakeTaskCreator{events: &events}
	rec := postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, nil, testBudget), `{"objective":"Add X","repository":"sf/ai-software-factory-code"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	want := "Add X\n\nfiled by factoryd intake"
	if *issueBody != want {
		t.Fatalf("issue body = %q, want %q", *issueBody, want)
	}
}

func TestCreateTaskHandlerIssueCreationFailureCreatesNoRow(t *testing.T) {
	var events []string
	forgejo, _, _ := createTaskForgejoStub(t, &events, true)
	store := &fakeTaskCreator{events: &events}
	rec := postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, nil, testBudget), `{"objective":"Add X","repository":"sf/ai-software-factory-code"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if len(store.created) != 0 || slices.Contains(events, "create-row") {
		t.Fatalf("a row was created despite the issue-creation failure: events = %v, created = %v", events, store.created)
	}
}

func TestCreateTaskHandlerStoreFailureReturnsError(t *testing.T) {
	var events []string
	forgejo, _, _ := createTaskForgejoStub(t, &events, false)
	store := &fakeTaskCreator{events: &events, err: errors.New("db unavailable")}
	rec := postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, nil, testBudget), `{"objective":"Add X","repository":"sf/ai-software-factory-code"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("db unavailable")) {
		t.Fatalf("body = %s, want the store error surfaced", rec.Body)
	}
	if slices.Contains(events, "start-workflow") {
		t.Fatalf("events = %v: no workflow for a task row that does not exist", events)
	}
}

type fakeTaskReader map[string]factoryd.Task

func (s fakeTaskReader) Task(ctx context.Context, id string) (factoryd.Task, bool, error) {
	t, ok := s[id]
	return t, ok, nil
}

type fakeObjects map[string][]byte

func (o fakeObjects) Put(ctx context.Context, key string, body []byte) error {
	o[key] = body
	return nil
}

func (o fakeObjects) Get(ctx context.Context, key string) ([]byte, error) { return o[key], nil }
func (o fakeObjects) Delete(_ context.Context, key string) error          { delete(o, key); return nil }

func TestTimelineHandlerServesTheTaskTimeline(t *testing.T) {
	tasks := fakeTaskReader{"t-1": {ID: "t-1"}, "t-2": {ID: "t-2"}}
	objects := fakeObjects{"sessions/t-1/timeline.jsonl": []byte(`{"step":"intake"}` + "\n")}
	h := recordHandler(tasks, objects)
	for id, want := range map[string]struct {
		code int
		body string
	}{
		"t-1":   {http.StatusOK, `{"step":"intake"}` + "\n"},
		"t-2":   {http.StatusNotFound, ""},
		"nope":  {http.StatusNotFound, ""},
		"..%2F": {http.StatusNotFound, ""},
	} {
		req := httptest.NewRequest(http.MethodGet, "/tasks/"+id+"/timeline", nil)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != want.code || (want.code == http.StatusOK && rec.Body.String() != want.body) {
			t.Fatalf("%s: %d %q, want %d %q", id, rec.Code, rec.Body, want.code, want.body)
		}
	}
}

func TestRecordHandlerServesASessionRecord(t *testing.T) {
	tasks := fakeTaskReader{"t-1": {ID: "t-1"}}
	objects := fakeObjects{"sessions/t-1/s-0123456789ab.jsonl": []byte(`{"kind":"supervisor"}` + "\n")}
	h := recordHandler(tasks, objects)
	for session, want := range map[string]int{
		"s-0123456789ab": http.StatusOK,
		"s-ffffffffffff": http.StatusNotFound,
		"timeline":       http.StatusNotFound,
		"..%2Ftimeline":  http.StatusNotFound,
		"s-0123456789a":  http.StatusNotFound,
	} {
		req := httptest.NewRequest(http.MethodGet, "/tasks/t-1/sessions/x", nil)
		req.SetPathValue("id", "t-1")
		req.SetPathValue("session", session)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != want || (want == http.StatusOK && rec.Body.String() != `{"kind":"supervisor"}`+"\n") {
			t.Fatalf("%s: %d %q, want %d", session, rec.Code, rec.Body, want)
		}
	}
}

func testBudget() policy.Budget {
	return policy.Budget{TaskDefaultEUR: 1.5, TaskMaxEUR: 4, DayEUR: 20, MonthEUR: 50}
}

// The task budget comes from the policy in force: its default when none is
// asked for, never above its maximum; every API task holds the coder class.
func TestCreateTaskHandlerBudgetFromPolicy(t *testing.T) {
	for body, want := range map[string]float64{
		`{"objective":"Add X","repository":"sf/ai-software-factory-code"}`:                   1.5,
		`{"objective":"Add X","repository":"sf/ai-software-factory-code","budget_eur":0.01}`: 0.01,
		`{"objective":"Add X","repository":"sf/ai-software-factory-code","budget_eur":9}`:    4,
	} {
		var events []string
		forgejo, _, _ := createTaskForgejoStub(t, &events, false)
		store := &fakeTaskCreator{events: &events}
		rec := postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, nil, testBudget), body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		got := store.created[0]
		if got.BudgetEUR != want || !slices.Equal(got.RequiredCapabilities, []string{"coder"}) {
			t.Fatalf("%s: task budget %v, capabilities %v; want %v and [coder]", body, got.BudgetEUR, got.RequiredCapabilities, want)
		}
		var res struct {
			BudgetEUR float64 `json:"budget_eur"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.BudgetEUR != want {
			t.Fatalf("%s: response budget %v (%v), want %v", body, res.BudgetEUR, err, want)
		}
	}
}

// A project's card maximum bounds the API task's budget, asked or default.
func TestCreateTaskHandlerBudgetUnderAProjectMaximum(t *testing.T) {
	project := factoryd.Project{Name: "p", Repository: "sf/ai-software-factory-code", Objects: "seed", Agents: "coder", File: true, Process: "maintenance",
		Tracks: policy.Tracks{Direct: policy.Track{Class: policy.ClassCoder}}, Budget: &policy.ProjectBudget{MonthEUR: 50, TaskMaxEUR: 1}}
	for body, want := range map[string]float64{
		`{"objective":"Add X","repository":"sf/ai-software-factory-code"}`:                   1,
		`{"objective":"Add X","repository":"sf/ai-software-factory-code","budget_eur":0.01}`: 0.01,
		`{"objective":"Add X","repository":"sf/ai-software-factory-code","budget_eur":9}`:    1,
	} {
		var events []string
		forgejo, _, _ := createTaskForgejoStub(t, &events, false)
		store := &fakeTaskCreator{events: &events, projects: map[string]factoryd.Project{project.Repository: project}}
		rec := postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, nil, testBudget), body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		if got := store.created[0].BudgetEUR; got != want {
			t.Fatalf("%s: task budget %v, want %v", body, got, want)
		}
	}
}

// The API's repro_command lands on the task and, as a repro block, in the
// issue body, where the webhook path would read it too.
func TestCreateTaskHandlerReproCommand(t *testing.T) {
	var events []string
	forgejo, issueBody, _ := createTaskForgejoStub(t, &events, false)
	store := &fakeTaskCreator{events: &events}
	rec := postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, nil, testBudget),
		`{"objective":"Fix X","repository":"sf/ai-software-factory-code","repro_command":" go test ./x -run TestX "}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if store.created[0].ReproCommand != "go test ./x -run TestX" || factoryd.ReproCommand(*issueBody) != "go test ./x -run TestX" {
		t.Fatalf("task repro = %q, issue body = %q", store.created[0].ReproCommand, *issueBody)
	}
	events = nil
	rec = postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, nil, testBudget),
		"{\"objective\":\"Fix X\",\"repository\":\"sf/ai-software-factory-code\",\"repro_command\":\"a\\n```\\nb\"}")
	if rec.Code != http.StatusBadRequest || len(events) != 0 {
		t.Fatalf("a fence in the command = %d with calls %v, want 400 and none", rec.Code, events)
	}
}

// A terminal signal cancels the running session; the cancel reaches the
// activity in the answer to its next heartbeat. The SDK holds heartbeats
// back up to 80% of the 2-minute heartbeat timeout, capped at its 60 s
// default, unless the worker caps it lower: the cap and the activities'
// heartbeat interval bound the cancel at 10 s.
func TestACancelReachesARunningActivityWithinTenSeconds(t *testing.T) {
	if got := workerOptions(nil).MaxHeartbeatThrottleInterval; got != 10*time.Second {
		t.Fatalf("heartbeat throttle cap = %s, want 10s", got)
	}
	if factoryd.HeartbeatEvery > 10*time.Second {
		t.Fatalf("activities heartbeat every %s, want at most 10s", factoryd.HeartbeatEvery)
	}
}

// Ruling 179: POST /tasks on a project on agents none, or on a repository
// no active project names, answers 422 with the reason and files nothing.
func TestCreateTaskHandlerRefusesWhereNoAgentWorks(t *testing.T) {
	var events []string
	forgejo, _, _ := createTaskForgejoStub(t, &events, false)
	store := &fakeTaskCreator{events: &events, projects: map[string]factoryd.Project{
		"sf/ai-software-factory-infra": {Name: "ai-software-factory-infra", Repository: "sf/ai-software-factory-infra", Objects: "seed", Agents: "none", File: true},
		"demo-app/demo-app":            {Name: "demo-app", Repository: "demo-app/demo-app", Objects: "factory", Agents: "coder", File: true},
	}}
	h := createTaskHandler(store, forgejo, &fakeStarter{events: &events}, nil, testBudget)
	for repo, want := range map[string]string{
		"sf/ai-software-factory-infra": "project ai-software-factory-infra takes no agent tasks (agents: none in projects/ai-software-factory-infra.yaml): file an issue for a human",
		"sf/elsewhere":                 "no active project has the repository sf/elsewhere",
		"demo-app/demo-app":            "no active project has the repository demo-app/demo-app", // before register-project
	} {
		rec := postCreateTask(h, `{"objective":"Rotate the key","repository":"`+repo+`"}`)
		if rec.Code != http.StatusUnprocessableEntity || strings.TrimSpace(rec.Body.String()) != want {
			t.Errorf("%s: %d %q, want 422 %q", repo, rec.Code, rec.Body, want)
		}
	}
	if len(events) != 0 {
		t.Fatalf("a refused task reached Forgejo or the store: %v", events)
	}
}

type projectRows []factoryd.Project

func (p projectRows) Projects(context.Context) ([]factoryd.Project, error) { return p, nil }

// The console's writer lists (ruling 254): every project agents work on,
// and the factory's own code project, which lists factoryd's namespace. A
// project whose writers cannot be read keeps its last list for 5 minutes,
// then is left out, and one never read is left out at once; the own
// project's failure fails the lists.
func TestConsoleWriters(t *testing.T) {
	coder := func(name, repo string, ns ...string) factoryd.Project {
		return factoryd.Project{Name: name, Repository: repo, Objects: policy.ObjectsSeed, Agents: policy.AgentsCoder, File: true, Namespaces: ns}
	}
	rows := projectRows{coder("code", "sf/code", "factory"), coder("demo", "demo/demo"), coder("broken", "x/broken"),
		{Name: "infra", Repository: "sf/infra", Objects: policy.ObjectsSeed, Agents: policy.AgentsNone, File: true}}
	failing := map[string]bool{"x/broken": true}
	writersOf := func(_ context.Context, p factoryd.Project) ([]string, error) {
		if failing[p.Repository] {
			return nil, errors.New("forgejo: 502")
		}
		return []string{"w-" + p.Name}, nil
	}
	at := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return at }
	lists := consoleWriters(rows, writersOf, "factory", now)
	want := func(when string, wantLists map[string][]string) {
		t.Helper()
		own, got, err := lists(context.Background())
		if err != nil || own != "sf/code" || !reflect.DeepEqual(got, wantLists) {
			t.Fatalf("%s: lists = %q, %v, %v; want %v", when, own, got, err, wantLists)
		}
	}
	want("x/broken never read", map[string][]string{"sf/code": {"w-code"}, "demo/demo": {"w-demo"}})
	failing = map[string]bool{"demo/demo": true}
	at = at.Add(4 * time.Minute)
	want("demo failing 4 minutes after its last list", map[string][]string{"sf/code": {"w-code"}, "demo/demo": {"w-demo"}, "x/broken": {"w-broken"}})
	at = at.Add(2 * time.Minute)
	want("demo failing 6 minutes after its last list", map[string][]string{"sf/code": {"w-code"}, "x/broken": {"w-broken"}})
	failing = map[string]bool{"sf/code": true}
	if _, _, err := lists(context.Background()); err == nil {
		t.Fatal("the own project's writers failed, the lists did not")
	}
	if _, _, err := consoleWriters(rows, writersOf, "elsewhere", now)(context.Background()); err == nil {
		t.Fatal("no project lists the namespace, the lists did not fail")
	}
}

// shutdownRecorder is the mock CI sweep provider and worker of the
// shutdown test: it records every operation in call order, with the
// context each ran under.
type shutdownRecorder struct {
	mu          sync.Mutex
	ops         []string
	listCtx     context.Context
	listTimeout time.Duration
	deleteCtx   context.Context
	deleteErr   string
	sweepCalls  int
}

func (r *shutdownRecorder) record(op string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, op)
}

func (r *shutdownRecorder) Ops() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ops)
}

func (r *shutdownRecorder) Servers(ctx context.Context, labelSelector string) ([]hcloud.Server, error) {
	dl, ok := ctx.Deadline()
	var left time.Duration
	if ok {
		left = time.Until(dl)
	}
	r.mu.Lock()
	r.listCtx = ctx
	r.listTimeout = left
	r.sweepCalls++
	r.mu.Unlock()
	r.record("ciSweepList:" + labelSelector)
	return []hcloud.Server{{ID: 7}}, nil
}

func (r *shutdownRecorder) DeleteServer(ctx context.Context, id int64) error {
	r.mu.Lock()
	r.deleteCtx = ctx
	errStr := ""
	if ctx.Err() != nil {
		errStr = ctx.Err().Error()
	}
	r.mu.Unlock()
	r.record("ciSweepDelete:" + strconv.FormatInt(id, 10))
	if errStr != "" {
		return errors.New(errStr)
	}
	return nil
}

func (r *shutdownRecorder) Stop() { r.record("workerStop") }

// The shutdown's order (roadmap 189, Stage 18 item 3.15): when the process
// context ends (SIGTERM), the CI spawner's own cancellable context goes
// first, then the temporal worker stops, then SweepCIMachines deletes
// every labelled machine on a fresh 30-second context, and only then do
// the servers shut down.
func TestShutdownSweepsCIMachinesBeforeExit(t *testing.T) {
	rec := &shutdownRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	ciRunCtx, ciRunCancel := context.WithCancelCause(ctx)
	// the mock CI spawner: it notices its run context ended, returns from
	// its loop and closes spawnerDone; only then may the shutdown sweep.
	spawnerDone := make(chan struct{})
	go func() {
		<-ciRunCtx.Done()
		rec.record("ciSpawnerNoticed")
		rec.record("ciSpawnerStopped")
		close(spawnerDone)
	}()
	consoleServer := &http.Server{Addr: "127.0.0.1:0", Handler: http.NotFoundHandler()}
	server := &http.Server{Addr: "127.0.0.1:0", Handler: http.NotFoundHandler()}
	cons, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srvLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = consoleServer.Serve(cons) }()
	go func() { _ = server.Serve(srvLn) }()
	shutdownDone := make(chan struct{})
	go func() {
		runShutdown(ciRunCtx, ciRunCancel, spawnerDone, rec, rec, consoleServer, server)
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatal("runShutdown returned before the process context ended")
	default:
	}
	cancel() // the SIGTERM
	select {
	case <-shutdownDone:
	case <-time.After(10 * time.Second):
		t.Fatal("runShutdown did not return")
	}
	want := []string{"ciSpawnerNoticed", "ciSpawnerStopped", "workerStop",
		"ciSweepList:" + factoryd.CISweepLabelSelector, "ciSweepDelete:7"}
	if got := rec.Ops(); !slices.Equal(got, want) {
		t.Fatalf("ops = %v; want %v", got, want)
	}
	if rec.sweepCalls != 1 {
		t.Fatalf("sweep list calls = %d; want 1", rec.sweepCalls)
	}
	if rec.listTimeout <= 0 || rec.listTimeout > 30*time.Second {
		t.Fatalf("the sweep's context timeout = %s; want a fresh one of at most 30 seconds", rec.listTimeout)
	}
	if rec.deleteCtx == nil {
		t.Fatal("the sweep never deleted the machine it listed")
	}
	if rec.deleteErr != "" {
		t.Fatalf("the sweep deleted on a dead context: %s", rec.deleteErr)
	}
}
