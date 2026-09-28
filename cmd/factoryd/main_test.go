package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
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

func (e *escalationStub) EscalateSession(context.Context, string, string, string) error {
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
		req := httptest.NewRequest(http.MethodPost, "/internal/escalations", strings.NewReader(`{"task_id":"t1","question":"q","reason":"r"}`))
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
	rec := postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, testBudget), `{"objective":"Add X","repository":"sf/ai-software-factory-code"}`)
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
	rec := postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, testBudget), `{"objective":"Add X","repository":"sf/ai-software-factory-code"}`)
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
		rec := postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, testBudget), body)
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
	rec := postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, testBudget), `{"objective":"Add X","repository":"sf/ai-software-factory-code"}`)
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
	rec := postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, testBudget), `{"objective":"Add X","repository":"sf/ai-software-factory-code"}`)
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
	rec := postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, testBudget), `{"objective":"Add X","repository":"sf/ai-software-factory-code"}`)
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
	rec := postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, testBudget), `{"objective":"Add X","repository":"sf/ai-software-factory-code"}`)
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

func testBudget() policy.Budget { return policy.Budget{TaskDefaultEUR: 1.5, TaskMaxEUR: 4, DayEUR: 20} }

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
		rec := postCreateTask(createTaskHandler(store, forgejo, &fakeStarter{events: &events}, testBudget), body)
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
