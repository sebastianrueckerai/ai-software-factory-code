package factoryd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
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
		Forgejo: &Forgejo{BaseURL: fjURL, BotUser: "factory-bot", BotPassword: "bot-pw"},
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
	if len(back.forgejoCalls) < 2 || back.forgejoCalls[1] != "DELETE /api/v1/users/factory-bot/tokens/77" {
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

// Finished tasks are raised to the spend LiteLLM reports for their keys once
// the settle delay has passed, never lowered, and settled once.
func TestCompletionSettlesSpendAgainstLiteLLM(t *testing.T) {
	infos := map[string]string{
		"h1": `{"key":"h1","info":{"spend":0.3,"metadata":{"task_id":"t1"}}}`,
		"h2": `{"key":"h2","info":{"spend":0.45,"blocked":true,"metadata":{"task_id":"t1"}}}`,
		"h3": `{"key":"h3","info":{"spend":9,"metadata":{"task_id":"other"}}}`,
	}
	ll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/key/list":
			io.WriteString(w, `{"keys":["h1","h2","h3"],"total_pages":1}`)
		case "/key/info":
			io.WriteString(w, infos[req.URL.Query().Get("key")])
		default:
			t.Errorf("unexpected litellm call %s", req.URL.Path)
		}
	}))
	defer ll.Close()

	store := newFakeStore()
	now := time.Now()
	for id, spent := range map[string]float64{"t1": 0.5, "t2": 0.5, "t3": 1} {
		store.CreateTask(context.Background(), Task{ID: id, BudgetEUR: 2, SpentEUR: spent, Status: "done"})
	}
	store.finished["t1"] = now.Add(-4 * time.Minute)
	store.finished["t2"] = now.Add(-1 * time.Minute) // LiteLLM may not have written its spend yet
	store.finished["t3"] = now.Add(-4 * time.Minute)
	c := &Completion{Store: store, LiteLLM: &LiteLLMAdmin{BaseURL: ll.URL, MasterKey: "mk"}}

	if err := c.SettleSpend(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]float64{
		"t1": 0.75, // 0.3 + 0.45 from its two keys, above the booked 0.5
		"t2": 0.5,  // inside the settle delay
		"t3": 1,    // no LiteLLM spend: never lowered
	} {
		if got, _, _ := store.Task(context.Background(), id); got.SpentEUR != want {
			t.Fatalf("%s spent = %v, want %v", id, got.SpentEUR, want)
		}
	}
	if !store.settled["t1"] || store.settled["t2"] || !store.settled["t3"] {
		t.Fatalf("settled = %v, want t1 and t3", store.settled)
	}
}
