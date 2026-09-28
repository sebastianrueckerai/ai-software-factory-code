package factoryd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

// tasksHub serves actions/tasks page 1 from a testdata file, later pages
// empty, and checks the tasks token.
type tasksHub struct {
	mu    sync.Mutex
	file  string
	pages []int
}

func (h *tasksHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/sf/ai-software-factory-code/actions/tasks" {
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "token tasks-tok" {
			t.Errorf("auth %q, want the tasks token", got)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		page := r.URL.Query().Get("page")
		h.pages = append(h.pages, map[string]int{"1": 1, "2": 2, "3": 3}[page])
		if page != "1" {
			w.Write([]byte(`{"workflow_runs":[],"total_count":0}`))
			return
		}
		body, err := os.ReadFile("testdata/" + h.file)
		if err != nil {
			t.Error(err)
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (h *tasksHub) set(file string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.file, h.pages = file, nil
}

func TestCIRunsEmitsEachFinishedJobAndRunOnce(t *testing.T) {
	m, reader := testMetrics(t)
	hub := &tasksHub{file: "actions-tasks-boot.json"}
	srv := hub.serve(t)
	now := time.Date(2026, 9, 28, 10, 1, 0, 0, time.UTC)
	c := &CIRuns{Forgejo: &Forgejo{BaseURL: srv.URL, CITasksToken: "tasks-tok"}, Owner: "sf", Repo: "ai-software-factory-code",
		Metrics: m, now: func() time.Time { return now }}
	ctx := context.Background()

	// boot: what already finished (100, 102, run 50) is history
	c.poll(ctx)
	if got := histograms(t, reader, "factory.ci.job.duration"); len(got) != 0 {
		t.Fatalf("boot emitted jobs %v", got)
	}
	if !reflect.DeepEqual(hub.pages, []int{1}) {
		t.Fatalf("boot read pages %v, want only the newest", hub.pages)
	}

	hub.set("actions-tasks-2.json")
	now = now.Add(4 * time.Minute) // 10:05
	c.poll(ctx)
	hub.set("actions-tasks-3.json")
	now = now.Add(time.Minute) // 10:06, run 51 finished 10:05:13, not settled
	c.poll(ctx)
	if got := histograms(t, reader, "factory.ci.run.duration"); len(got) != 0 {
		t.Fatalf("run emitted before it settled: %v", got)
	}
	now = now.Add(5 * time.Minute) // 10:11
	c.poll(ctx)
	c.poll(ctx)

	jobs := histograms(t, reader, "factory.ci.job.duration")
	want := map[string]histPoint{
		// 101: 10:00:00 to 10:04:18
		"event=pull_request,job=test,status=success": {1, 258},
		// 103: 10:04:20 to 10:05:13
		"event=pull_request,job=image-build,status=success": {1, 53},
	}
	if !reflect.DeepEqual(jobs, want) {
		t.Fatalf("job durations %v, want %v", jobs, want)
	}
	// run 51: first created 10:00:00 to last updated 10:05:13; run 50 was
	// history at boot, run 52 still runs
	runs := histograms(t, reader, "factory.ci.run.duration")
	if !reflect.DeepEqual(runs, map[string]histPoint{"event=pull_request,status=success": {1, 313}}) {
		t.Fatalf("run durations %v", runs)
	}
	if at, ok := c.PushedAt("9f1e0c3a5b7d2e4f6a8c0b1d3e5f7a9c1b3d5e7f"); !ok || !at.Equal(time.Date(2026, 9, 28, 10, 5, 40, 0, time.UTC)) {
		t.Fatalf("pushed at %v %v", at, ok)
	}
	if _, ok := c.PushedAt("c62cd2d1ad2dd06b8e73354a0ff07fde47321184"); ok {
		t.Fatal("a pull_request run is no push")
	}
}

func TestCIRunsFailedJobFailsTheRun(t *testing.T) {
	m, reader := testMetrics(t)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	c := &CIRuns{Metrics: m, now: func() time.Time { return now }}
	c.started = true
	c.observe(context.Background(), []ActionTask{
		{ID: 3, Name: "security", RunNumber: 9, Event: "push", Status: "failure",
			CreatedAt: now.Add(-9 * time.Minute), RunStartedAt: now.Add(-9 * time.Minute), UpdatedAt: now.Add(-7 * time.Minute)},
		{ID: 2, Name: "test", RunNumber: 9, Event: "push", Status: "success",
			CreatedAt: now.Add(-10 * time.Minute), RunStartedAt: now.Add(-10 * time.Minute), UpdatedAt: now.Add(-6 * time.Minute)},
	})
	// 10:50 to 10:54 = 240 s
	runs := histograms(t, reader, "factory.ci.run.duration")
	if !reflect.DeepEqual(runs, map[string]histPoint{"event=push,status=failure": {1, 240}}) {
		t.Fatalf("run durations %v", runs)
	}
}

func readRunJobs(t *testing.T, file string) []CIJob {
	t.Helper()
	body, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var jobs []CIJob
	if err := json.Unmarshal(body, &jobs); err != nil {
		t.Fatal(err)
	}
	return jobs
}

func TestCIQueueMeasuresWaitingToPickedUp(t *testing.T) {
	m, reader := testMetrics(t)
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	q := NewCIQueue(m)
	q.now = func() time.Time { return now }
	for i, file := range []string{"runner-jobs-1.json", "runner-jobs-2.json", "runner-jobs-3.json", "runner-jobs-4.json"} {
		now = time.Date(2026, 9, 28, 10, 0, 10*i, 0, time.UTC)
		q.Observe(context.Background(), "pr-untrusted", readRunJobs(t, file))
	}
	// 8 left waiting at 10 s, 7 at 20 s; 9 vanished while waiting (cancelled)
	got := histograms(t, reader, "factory.ci.job.queue")
	want := map[string]histPoint{
		"job=vet-and-format,label=pr-untrusted": {1, 10},
		"job=test,label=pr-untrusted":           {1, 20},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("queue %v, want %v", got, want)
	}
	if s, ok := q.Seconds(207); !ok || s != 20 {
		t.Fatalf("task 207 queue %v %v", s, ok)
	}
	if s, ok := q.Seconds(208); !ok || s != 10 {
		t.Fatalf("task 208 queue %v %v", s, ok)
	}
	if _, ok := q.Seconds(209); ok {
		t.Fatal("an unknown task has no queue time")
	}
}

func TestCIJobsOfAHeadListsNameQueueAndRun(t *testing.T) {
	hub := &tasksHub{file: "actions-tasks-3.json"}
	srv := hub.serve(t)
	m, _ := testMetrics(t)
	q := NewCIQueue(m)
	q.byTask[101] = queued{seconds: 30}
	jobs, err := CIJobsOf(context.Background(), &Forgejo{BaseURL: srv.URL, CITasksToken: "tasks-tok"}, "sf", "ai-software-factory-code",
		"c62cd2d1ad2dd06b8e73354a0ff07fde47321184", q)
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"name": "test", "status": "success", "queue_s": 30.0, "run_s": 258.0},
		{"name": "vet-and-format", "status": "success", "run_s": 146.0},
		{"name": "image-build", "status": "success", "run_s": 53.0},
	}
	if !reflect.DeepEqual(jobs, want) {
		t.Fatalf("jobs %v, want %v", jobs, want)
	}
}
