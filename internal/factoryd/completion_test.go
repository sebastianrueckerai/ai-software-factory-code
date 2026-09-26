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
}

func (r *recordingBackend) srv(t *testing.T) (forgejo, litellm string) {
	t.Helper()
	fj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.forgejoCalls = append(r.forgejoCalls, req.Method+" "+req.URL.Path)
		status := r.forgejoStatus
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	ll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.litellmCalls = append(r.litellmCalls, req.Method+" "+req.URL.Path)
		status := r.litellmStatus
		r.mu.Unlock()
		if req.URL.Path == "/key/list" {
			io.WriteString(w, `{"keys":[{"key":"sk-t1","metadata":{"task_id":"t1"},"blocked":false}]}`)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(fj.Close)
	t.Cleanup(ll.Close)
	return fj.URL, ll.URL
}

func TestCompletionClosesSpendThenRevokesThenDones(t *testing.T) {
	back := &recordingBackend{forgejoStatus: 204, litellmStatus: 200}
	fjURL, llURL := back.srv(t)
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t1", BudgetEUR: 5, Status: "running"})

	c := &Completion{
		Store:   store,
		Forgejo: &Forgejo{BaseURL: fjURL, AdminToken: "admin"},
		BotUser: "factory-bot",
		LiteLLM: &LiteLLMAdmin{BaseURL: llURL, MasterKey: "mk"},
	}
	if err := c.Complete(context.Background(), "t1", 77); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	task, _, _ := store.Task(context.Background(), "t1")
	if task.Status != "done" {
		t.Fatalf("status = %q, want done", task.Status)
	}
	joined := back.forgejoCalls[0]
	if joined != "DELETE /api/v1/admin/users/factory-bot/tokens/77" {
		t.Fatalf("forgejo call = %v", back.forgejoCalls)
	}
	if len(back.litellmCalls) == 0 || back.litellmCalls[len(back.litellmCalls)-1] != "POST /key/block" {
		t.Fatalf("litellm calls = %v", back.litellmCalls)
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

		c := &Completion{
			Store:   store,
			Forgejo: &Forgejo{BaseURL: fjURL, AdminToken: "admin"},
			BotUser: "factory-bot",
			LiteLLM: &LiteLLMAdmin{BaseURL: llURL, MasterKey: "mk"},
		}
		if err := c.Complete(context.Background(), "t1", 1); err == nil {
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
