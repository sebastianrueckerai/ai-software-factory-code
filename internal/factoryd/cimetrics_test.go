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

// ciAPIHub serves actions/tasks and actions/runs page 1 from testdata
// files, later pages empty, and checks the registrar token.
type ciAPIHub struct {
	mu                  sync.Mutex
	tasksFile, runsFile string
	pages               []string // "tasks 1", "runs 1", ...
}

func (h *ciAPIHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token registrar-tok" {
			t.Errorf("auth %q, want the registrar token", got)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		var file, kind string
		switch r.URL.Path {
		case "/api/v1/repos/sf/ai-software-factory-code/actions/tasks":
			file, kind = h.tasksFile, "tasks"
		case "/api/v1/repos/sf/ai-software-factory-code/actions/runs":
			file, kind = h.runsFile, "runs"
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		page := r.URL.Query().Get("page")
		h.pages = append(h.pages, kind+" "+page)
		if page != "1" || file == "" {
			w.Write([]byte(`{"workflow_runs":[],"total_count":0}`))
			return
		}
		body, err := os.ReadFile("testdata/" + file)
		if err != nil {
			t.Error(err)
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (h *ciAPIHub) set(tasks, runs string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tasksFile, h.runsFile, h.pages = tasks, runs, nil
}

func TestCIRunsEmitsEachFinishedJobAndRunOnce(t *testing.T) {
	m, reader := testMetrics(t)
	hub := &ciAPIHub{tasksFile: "actions-tasks-boot.json", runsFile: "actions-runs-boot.json"}
	srv := hub.serve(t)
	c := &CIRuns{Forgejo: &Forgejo{BaseURL: srv.URL, CIRegistrarToken: "registrar-tok"}, Owner: "sf", Repo: "ai-software-factory-code", Metrics: m}
	ctx := context.Background()

	// boot: what already finished (tasks 100, 102, run 50) is history
	c.poll(ctx)
	if got := histograms(t, reader, "factory.ci.job.duration"); len(got) != 0 {
		t.Fatalf("boot emitted jobs %v", got)
	}
	if got := histograms(t, reader, "factory.ci.run.duration"); len(got) != 0 {
		t.Fatalf("boot emitted runs %v", got)
	}
	if !reflect.DeepEqual(hub.pages, []string{"tasks 1", "runs 1"}) {
		t.Fatalf("boot read %v, want only the newest pages", hub.pages)
	}

	hub.set("actions-tasks-2.json", "actions-runs-2.json")
	c.poll(ctx)
	hub.set("actions-tasks-3.json", "actions-runs-3.json")
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
	// run 51: created 09:59:58, stopped 10:05:13; run 52: created
	// 10:05:39, stopped 10:09:40; run 50 was history at boot
	runs := histograms(t, reader, "factory.ci.run.duration")
	if !reflect.DeepEqual(runs, map[string]histPoint{
		"event=pull_request,status=success": {1, 315},
		"event=push,status=failure":         {1, 241},
	}) {
		t.Fatalf("run durations %v", runs)
	}
}

// A run that finishes after a newer one is still emitted once.
func TestFreshEmitsOutOfOrderFinishesOnce(t *testing.T) {
	type item struct {
		id   int64
		done bool
	}
	id := func(i item) int64 { return i.id }
	done := func(i item) bool { return i.done }
	var f idFloor
	if got := fresh(&f, []item{{3, false}, {2, true}}, id, done); len(got) != 0 {
		t.Fatalf("boot gave %v", got)
	}
	if got := fresh(&f, []item{{4, true}, {3, false}, {2, true}}, id, done); !reflect.DeepEqual(got, []item{{4, true}}) {
		t.Fatalf("second read gave %v", got)
	}
	if f.floor != 2 {
		t.Fatalf("floor %d, want 2: below the open 3", f.floor)
	}
	if got := fresh(&f, []item{{4, true}, {3, true}}, id, done); !reflect.DeepEqual(got, []item{{3, true}}) {
		t.Fatalf("third read gave %v", got)
	}
	if f.floor != 4 || len(f.emitted) != 0 {
		t.Fatalf("floor %d emitted %v, want 4 and none", f.floor, f.emitted)
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
	if got := sums(t, reader, "factory.ci.job.queue.unmeasured"); len(got) != 0 {
		t.Fatalf("measured jobs counted unmeasured: %v", got)
	}
}

// A job already taken when first seen counts as unmeasured, once; at a
// label's first poll nothing counts.
func TestCIQueueCountsJobsTakenBeforeTheyWereSeen(t *testing.T) {
	m, reader := testMetrics(t)
	q := NewCIQueue(m)
	ctx := context.Background()
	q.Observe(ctx, "ubuntu-latest", []CIJob{{ID: 1, Name: "test", Status: "running", TaskID: 11}})
	for range 2 {
		q.Observe(ctx, "ubuntu-latest", []CIJob{{ID: 1, Name: "test", Status: "running", TaskID: 11},
			{ID: 2, Name: "image", Status: "running", TaskID: 12}})
	}
	if got := sums(t, reader, "factory.ci.job.queue.unmeasured"); !reflect.DeepEqual(got, map[string]float64{"job=image,label=ubuntu-latest": 1}) {
		t.Fatalf("unmeasured %v", got)
	}
	if got := histograms(t, reader, "factory.ci.job.queue"); len(got) != 0 {
		t.Fatalf("queue %v", got)
	}
}

func TestCIJobsOfAHeadListsNameQueueAndRun(t *testing.T) {
	hub := &ciAPIHub{tasksFile: "actions-tasks-3.json"}
	srv := hub.serve(t)
	m, _ := testMetrics(t)
	q := NewCIQueue(m)
	q.byTask[101] = queued{seconds: 30}
	jobs, err := CIJobsOf(context.Background(), &Forgejo{BaseURL: srv.URL, CIRegistrarToken: "registrar-tok"}, "sf", "ai-software-factory-code",
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
