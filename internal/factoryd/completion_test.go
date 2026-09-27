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
			tokens = `[{"id":77,"name":"task-t1"},{"id":78,"name":"other"},{"id":79,"name":"task-t1-base"}]`
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
			io.WriteString(w, `{"keys":[{"token":"h-t1","blocked":null,"metadata":{"task_id":"t1"}}],"total_count":1,"current_page":1,"total_pages":1}`)
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
	// Both task tokens are found by name, never by a remembered ID.
	if len(back.forgejoCalls) < 3 || back.forgejoCalls[1] != "DELETE /api/v1/users/factory-bot/tokens/77" || back.forgejoCalls[2] != "DELETE /api/v1/users/factory-bot/tokens/79" {
		t.Fatalf("forgejo calls = %v, want task-t1 (77) and task-t1-base (79) revoked by name", back.forgejoCalls)
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

// Every task finished within the settle window is raised to the spend
// LiteLLM reports for its keys, on every pass, never lowered: a spend write
// that lands late is still counted. One pass lists the keys once.
func TestCompletionSettlesSpendAgainstLiteLLM(t *testing.T) {
	var mu sync.Mutex
	lists := 0
	h1Spend := "0.25"
	ll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if req.URL.Path != "/key/list" {
			t.Errorf("unexpected litellm call %s", req.URL.Path)
			return
		}
		lists++
		io.WriteString(w, `{"keys":[
			{"token":"h1","spend":`+h1Spend+`,"metadata":{"task_id":"t1"}},
			{"token":"h2","spend":0.5,"blocked":true,"metadata":{"task_id":"t1"}},
			{"token":"h3","spend":9,"metadata":{"task_id":"old"}},
			{"token":"h4","spend":9,"metadata":{"task_id":"run"}}],"total_pages":1}`)
	}))
	defer ll.Close()

	store := newFakeStore()
	now := time.Now()
	for _, tk := range []Task{
		{ID: "t1", SpentEUR: 0.5, Status: "done"},
		{ID: "t2", SpentEUR: 1, Status: "failed"},
		{ID: "old", SpentEUR: 0.5, Status: "done"},
		{ID: "run", SpentEUR: 0.5, Status: "running"},
	} {
		store.CreateTask(context.Background(), tk)
	}
	store.finished["t1"] = now.Add(-10 * time.Second)
	store.finished["t2"] = now.Add(-30 * time.Minute)
	store.finished["old"] = now.Add(-61 * time.Minute) // past the 1h window
	c := &Completion{Store: store, LiteLLM: &LiteLLMAdmin{BaseURL: ll.URL, MasterKey: "mk"}}

	check := func(pass int, want map[string]float64) {
		t.Helper()
		n, err := c.SettleSpend(context.Background())
		if err != nil || n != 2 {
			t.Fatalf("pass %d: SettleSpend = (%d, %v), want (2, nil): t1 and t2", pass, n, err)
		}
		for id, w := range want {
			if got, _, _ := store.Task(context.Background(), id); got.SpentEUR != w {
				t.Fatalf("pass %d: %s spent = %v, want %v", pass, id, got.SpentEUR, w)
			}
		}
		if lists != pass {
			t.Fatalf("pass %d: key/list calls = %d, want one per pass", pass, lists)
		}
	}
	check(1, map[string]float64{
		"t1":  0.75, // 0.25 + 0.5 from its two keys, above the booked 0.5
		"t2":  1,    // no LiteLLM keys: never lowered
		"old": 0.5,  // outside the window
		"run": 0.5,  // not finished
	})
	mu.Lock()
	h1Spend = "0.5" // LiteLLM's late spend write lands: 0.5 + 0.5
	mu.Unlock()
	check(2, map[string]float64{"t1": 1, "t2": 1, "old": 0.5, "run": 0.5})

	// nothing finished in the window: no LiteLLM call at all
	delete(store.finished, "t1")
	delete(store.finished, "t2")
	if n, err := c.SettleSpend(context.Background()); n != 0 || err != nil || lists != 2 {
		t.Fatalf("empty pass = (%d, %v), key/list calls = %d; want (0, nil) and still 2", n, err, lists)
	}
}

// The settle loop runs passes on its own ticker until its context ends.
func TestCompletionSettleLoopRunsPasses(t *testing.T) {
	passes := make(chan struct{}, 10)
	ll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, `{"keys":[{"token":"h1","spend":0.25,"metadata":{"task_id":"t1"}}],"total_pages":1}`)
		select {
		case passes <- struct{}{}:
		default:
		}
	}))
	defer ll.Close()
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t1", Status: "done"})
	store.finished["t1"] = time.Now()
	c := &Completion{Store: store, LiteLLM: &LiteLLMAdmin{BaseURL: ll.URL, MasterKey: "mk"}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.SettleLoop(ctx, time.Millisecond); close(done) }()
	<-passes
	<-passes
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SettleLoop did not return after cancel")
	}
	if got, _, _ := store.Task(context.Background(), "t1"); got.SpentEUR != 0.25 {
		t.Fatalf("spent = %v, want 0.25", got.SpentEUR)
	}
}
