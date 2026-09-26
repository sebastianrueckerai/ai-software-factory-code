package factoryd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type recordingBackend struct {
	mu            sync.Mutex
	forgejoCalls  []string
	litellmCalls  []string
	forgejoStatus int
	litellmStatus int
	forgejoTokens string // GET /users/<bot>/tokens body
}

func (r *recordingBackend) srv(t *testing.T) (forgejo, litellm string) {
	t.Helper()
	fj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.forgejoCalls = append(r.forgejoCalls, req.Method+" "+req.URL.Path)
		status := r.forgejoStatus
		tokens := r.forgejoTokens
		r.mu.Unlock()
		if tokens == "" {
			tokens = `[{"id":77,"name":"task-t1"},{"id":78,"name":"other"}]`
		}
		if req.Method == "GET" {
			io.WriteString(w, tokens)
			return
		}
		w.WriteHeader(status)
	}))
	ll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.litellmCalls = append(r.litellmCalls, req.Method+" "+req.URL.Path)
		status := r.litellmStatus
		r.mu.Unlock()
		if req.URL.Path == "/key/list" {
			io.WriteString(w, `{"keys":["h-t1"],"total_count":1,"current_page":1,"total_pages":1}`)
			return
		}
		if req.URL.Path == "/key/info" {
			io.WriteString(w, `{"key":"h-t1","info":{"blocked":null,"metadata":{"task_id":"t1"}}}`)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(fj.Close)
	t.Cleanup(ll.Close)
	return fj.URL, ll.URL
}

func newCompletion(store Store, fjURL, llURL string) *Completion {
	return &Completion{
		Store:   store,
		Forgejo: &Forgejo{BaseURL: fjURL, AdminToken: "admin"},
		BotUser: "factory-bot",
		LiteLLM: &LiteLLMAdmin{BaseURL: llURL, MasterKey: "mk"},
	}
}

func TestCompletionClosesSpendThenRevokesThenDones(t *testing.T) {
	back := &recordingBackend{forgejoStatus: 204, litellmStatus: 200}
	fjURL, llURL := back.srv(t)
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t1", BudgetEUR: 5, Status: "running"})

	if err := newCompletion(store, fjURL, llURL).Complete(context.Background(), "t1", "done"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	task, _, _ := store.Task(context.Background(), "t1")
	if task.Status != "done" {
		t.Fatalf("status = %q, want done", task.Status)
	}
	// The token is found by name, never by a remembered ID.
	if len(back.forgejoCalls) < 2 || back.forgejoCalls[1] != "DELETE /api/v1/admin/users/factory-bot/tokens/77" {
		t.Fatalf("forgejo calls = %v, want task-t1 (id 77) revoked by name", back.forgejoCalls)
	}
	if len(back.litellmCalls) == 0 || back.litellmCalls[len(back.litellmCalls)-1] != "POST /key/block" {
		t.Fatalf("litellm calls = %v", back.litellmCalls)
	}
}

func TestCompletionMarksFailedRunAsFailed(t *testing.T) {
	back := &recordingBackend{forgejoStatus: 204, litellmStatus: 200}
	fjURL, llURL := back.srv(t)
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t1", BudgetEUR: 5, Status: "running"})

	if err := newCompletion(store, fjURL, llURL).Complete(context.Background(), "t1", "failed"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	task, _, _ := store.Task(context.Background(), "t1")
	// A failed run still revokes everything; only the resting status differs.
	if task.Status != "failed" {
		t.Fatalf("status = %q, want failed", task.Status)
	}
	if len(back.forgejoCalls) == 0 {
		t.Fatal("forgejo token was not revoked for a failed run")
	}
}

func TestCompletionSkipsOtherTasksTokens(t *testing.T) {
	back := &recordingBackend{forgejoStatus: 204, litellmStatus: 200, forgejoTokens: `[{"id":78,"name":"task-t2"}]`}
	fjURL, llURL := back.srv(t)
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t1", BudgetEUR: 5, Status: "running"})

	if err := newCompletion(store, fjURL, llURL).Complete(context.Background(), "t1", "done"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(back.forgejoCalls) != 1 {
		t.Fatalf("forgejo calls = %v, want only the list, no revoke of another task's token", back.forgejoCalls)
	}
}

func TestCompletionLeavesClosingOnRevocationFailure(t *testing.T) {
	for name, breakIt := range map[string]func(*recordingBackend){
		"forgejo refuses": func(b *recordingBackend) { b.forgejoStatus = 403 },
		"litellm refuses": func(b *recordingBackend) { b.litellmStatus = 500 },
	} {
		back := &recordingBackend{forgejoStatus: 204, litellmStatus: 200}
		breakIt(back)
		fjURL, llURL := back.srv(t)
		store := newFakeStore()
		store.CreateTask(context.Background(), Task{ID: "t1", BudgetEUR: 5, Status: "running"})

		if err := newCompletion(store, fjURL, llURL).Complete(context.Background(), "t1", "done"); err == nil {
			t.Fatalf("%s: Complete succeeded against a broken backend", name)
		}
		task, _, _ := store.Task(context.Background(), "t1")
		// "closing" is the safe resting state: the gateway already refuses
		// model spend for it, so a stuck completion cannot leak budget.
		if task.Status != "closing" {
			t.Fatalf("%s: status = %q, want closing", name, task.Status)
		}
	}
}
