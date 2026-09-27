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
	rec := postCreateTask(createTaskHandler(store, forgejo, 5), `{"objective":"Add X","repository":"sf/ai-software-factory-code"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	want := []string{"create-issue", "replace-labels", "create-row"}
	if !slices.Equal(events, want) {
		t.Fatalf("call order = %v, want %v (issue filed, then labelled, then the row created)", events, want)
	}
}

func TestCreateTaskHandlerResponseCarriesIssueRef(t *testing.T) {
	var events []string
	forgejo, _, _ := createTaskForgejoStub(t, &events, false)
	store := &fakeTaskCreator{events: &events}
	rec := postCreateTask(createTaskHandler(store, forgejo, 5), `{"objective":"Add X","repository":"sf/ai-software-factory-code"}`)
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

func TestCreateTaskHandlerLabelsAgentAndQueued(t *testing.T) {
	var events []string
	forgejo, _, appliedLabels := createTaskForgejoStub(t, &events, false)
	store := &fakeTaskCreator{events: &events}
	rec := postCreateTask(createTaskHandler(store, forgejo, 5), `{"objective":"Add X","repository":"sf/ai-software-factory-code"}`)
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
	rec := postCreateTask(createTaskHandler(store, forgejo, 5), `{"objective":"Add X","repository":"sf/ai-software-factory-code"}`)
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
	rec := postCreateTask(createTaskHandler(store, forgejo, 5), `{"objective":"Add X","repository":"sf/ai-software-factory-code"}`)
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
	rec := postCreateTask(createTaskHandler(store, forgejo, 5), `{"objective":"Add X","repository":"sf/ai-software-factory-code"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("db unavailable")) {
		t.Fatalf("body = %s, want the store error surfaced", rec.Body)
	}
}
