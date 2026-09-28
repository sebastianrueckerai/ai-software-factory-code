package factoryd

import (
	"cmp"
	"context"
	"log"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// CIRuns turns the code repository's actions/tasks into
// factory.ci.job.duration once per finished job, and its actions/runs into
// factory.ci.run.duration once per finished run. It keeps what it emitted
// in memory: after a restart it starts from the newest page, and what
// finished while it was down is lost.
type CIRuns struct {
	Forgejo     *Forgejo
	Owner, Repo string
	Metrics     *telemetry.Metrics
	lastErr     string // logged once until the next success

	jobs, runs idFloor
}

const (
	ciRunsInterval = time.Minute
	// ciRunsPages bounds one poll's read; older entries count as handled.
	ciRunsPages = 10
	ciMemory    = 24 * time.Hour
)

// idFloor remembers which entries of a newest-first list were handled:
// every id at or below floor, and the finished ones above it in emitted.
// Entries finish out of order, so the floor stops below the oldest one
// still open.
type idFloor struct {
	started bool
	floor   int64
	emitted map[int64]bool
}

// readPages reads a newest-first list down to the floor; the first read
// takes the newest page only.
func readPages[T any](ctx context.Context, f *idFloor, fetch func(ctx context.Context, page int) ([]T, error), id func(T) int64) ([]T, error) {
	var all []T
	for page := 1; page <= ciRunsPages; page++ {
		items, err := fetch(ctx, page)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if !f.started || len(items) == 0 || id(items[len(items)-1]) <= f.floor {
			break
		}
	}
	return all, nil
}

// fresh returns the entries of one read that finished since the last,
// oldest first; at the first read what already finished is history.
func fresh[T any](f *idFloor, items []T, id func(T) int64, finished func(T) bool) []T {
	if f.emitted == nil {
		f.emitted = map[int64]bool{}
	}
	items = slices.DeleteFunc(slices.Clone(items), func(t T) bool { return id(t) <= f.floor })
	slices.SortFunc(items, func(a, b T) int { return cmp.Compare(id(a), id(b)) })
	boot := !f.started
	f.started = true
	var out []T
	floor, open := f.floor, false
	for _, t := range items {
		if !finished(t) {
			if !open {
				floor, open = id(t)-1, true
			}
			continue
		}
		if !open {
			floor = id(t)
		}
		if f.emitted[id(t)] {
			continue
		}
		f.emitted[id(t)] = true
		if !boot {
			out = append(out, t)
		}
	}
	f.floor = floor
	for i := range f.emitted {
		if i <= floor {
			delete(f.emitted, i)
		}
	}
	return out
}

// Run polls until ctx ends; a failed poll logs and waits for the next.
func (c *CIRuns) Run(ctx context.Context) {
	for {
		c.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(ciRunsInterval):
		}
	}
}

func (c *CIRuns) poll(ctx context.Context) {
	err := c.pollJobs(ctx)
	if err == nil {
		err = c.pollRuns(ctx)
	}
	switch {
	case err == nil:
		c.lastErr = ""
	case err.Error() != c.lastErr:
		log.Printf("ci metrics: %v", err)
		c.lastErr = err.Error()
	}
}

func (c *CIRuns) pollJobs(ctx context.Context) error {
	taskID := func(t ActionTask) int64 { return t.ID }
	tasks, err := readPages(ctx, &c.jobs, func(ctx context.Context, page int) ([]ActionTask, error) {
		return c.Forgejo.ActionTasks(ctx, c.Owner, c.Repo, page)
	}, taskID)
	if err != nil {
		return err
	}
	for _, t := range fresh(&c.jobs, tasks, taskID, ActionTask.Finished) {
		c.Metrics.CIJobDuration.Record(ctx, t.RunSeconds(), metric.WithAttributes(
			attribute.String("job", t.Name), attribute.String("event", t.Event), attribute.String("status", t.Status)))
	}
	return nil
}

// pollRuns emits each finished run's wall time, from the event that
// created it (the push) to its last job's finish: queue time included.
func (c *CIRuns) pollRuns(ctx context.Context) error {
	runID := func(r ActionRun) int64 { return r.ID }
	runs, err := readPages(ctx, &c.runs, func(ctx context.Context, page int) ([]ActionRun, error) {
		return c.Forgejo.ActionRuns(ctx, c.Owner, c.Repo, url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(actionTasksPage)}})
	}, runID)
	if err != nil {
		return err
	}
	for _, r := range fresh(&c.runs, runs, runID, ActionRun.Finished) {
		c.Metrics.CIRunDuration.Record(ctx, r.Stopped.Sub(r.Created).Seconds(), metric.WithAttributes(
			attribute.String("event", r.Event), attribute.String("status", r.Status)))
	}
	return nil
}

// CIQueue measures how long each Actions job waits for a runner: from the
// first poll that sees it waiting to the first that sees it taken (the
// spawner's 10 s poll is the resolution). It emits factory.ci.job.queue
// and remembers the result by task id for the checks step. A job already
// taken when first seen has no measurable queue (the list carries no
// creation time): it counts in factory.ci.job.queue.unmeasured instead, so
// the share of short queues the histogram misses stays visible. In memory
// only: a restart loses the jobs waiting across it.
type CIQueue struct {
	metrics *telemetry.Metrics
	now     func() time.Time
	mu      sync.Mutex
	waiting map[int64]waitingJob // job id
	// taken: job id → label of jobs seen taken and still listed
	taken   map[int64]string
	started map[string]bool // labels observed at least once
	byTask  map[int64]queued
}

type waitingJob struct {
	label, name string
	since       time.Time
}

type queued struct {
	seconds float64
	at      time.Time
}

func NewCIQueue(m *telemetry.Metrics) *CIQueue {
	return &CIQueue{metrics: m, now: time.Now, waiting: map[int64]waitingJob{}, taken: map[int64]string{},
		started: map[string]bool{}, byTask: map[int64]queued{}}
}

// Observe takes one poll's jobs for label. A job that vanishes while
// waiting (cancelled, or taken and finished between two polls) is dropped
// unmeasured. At a label's first poll the jobs already taken were queued
// before factoryd looked: neither measured nor counted.
func (q *CIQueue) Observe(ctx context.Context, label string, jobs []CIJob) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	boot := !q.started[label]
	q.started[label] = true
	listed := map[int64]bool{}
	for _, j := range jobs {
		listed[j.ID] = true
		w, tracked := q.waiting[j.ID]
		if j.Status == "waiting" {
			if !tracked {
				q.waiting[j.ID] = waitingJob{label: label, name: j.Name, since: now}
			}
			continue
		}
		if _, seen := q.taken[j.ID]; seen {
			continue
		}
		q.taken[j.ID] = label
		if !tracked {
			if !boot {
				q.metrics.CIJobQueueUnmeasured.Add(ctx, 1, metric.WithAttributes(attribute.String("label", label), attribute.String("job", j.Name)))
			}
			continue
		}
		delete(q.waiting, j.ID)
		s := now.Sub(w.since).Seconds()
		q.metrics.CIJobQueue.Record(ctx, s, metric.WithAttributes(attribute.String("label", label), attribute.String("job", w.name)))
		if j.TaskID != 0 {
			q.byTask[j.TaskID] = queued{seconds: s, at: now}
		}
	}
	for id, w := range q.waiting {
		if w.label == label && !listed[id] {
			delete(q.waiting, id)
		}
	}
	for id, l := range q.taken {
		if l == label && !listed[id] {
			delete(q.taken, id)
		}
	}
	for id, e := range q.byTask {
		if now.Sub(e.at) > ciMemory {
			delete(q.byTask, id)
		}
	}
}

// QueuedJob is a job waiting for a runner, as the spawner last saw it.
type QueuedJob struct {
	Label    string `json:"label"`
	Job      string `json:"job"`
	WaitingS int    `json:"waiting_s"`
}

// Waiting lists the jobs waiting for a runner now, longest waiting first.
func (q *CIQueue) Waiting() []QueuedJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	out := []QueuedJob{}
	for _, w := range q.waiting {
		out = append(out, QueuedJob{Label: w.label, Job: w.name, WaitingS: int(now.Sub(w.since).Seconds())})
	}
	slices.SortFunc(out, func(a, b QueuedJob) int {
		return cmp.Or(cmp.Compare(b.WaitingS, a.WaitingS), cmp.Compare(a.Job, b.Job))
	})
	return out
}

// Seconds is the measured queue time of the job actions/tasks lists as taskID.
func (q *CIQueue) Seconds(taskID int64) (float64, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.byTask[taskID]
	return e.seconds, ok
}

// ciJobsPages bounds the search for a head's jobs, newest first.
const ciJobsPages = 3

// CIJobsOf lists the finished jobs of the runs on sha, oldest first: name,
// status, queue seconds where the queue measured them and run seconds.
func CIJobsOf(ctx context.Context, f *Forgejo, owner, repo, sha string, queue *CIQueue) ([]map[string]any, error) {
	var matched []ActionTask
	for page := 1; page <= ciJobsPages; page++ {
		items, err := f.ActionTasks(ctx, owner, repo, page)
		if err != nil {
			return nil, err
		}
		before := len(matched)
		for _, t := range items {
			if t.HeadSHA == sha && t.Finished() {
				matched = append(matched, t)
			}
		}
		if len(items) == 0 || (before > 0 && len(matched) == before) {
			break
		}
	}
	slices.SortFunc(matched, func(a, b ActionTask) int { return cmp.Compare(a.ID, b.ID) })
	jobs := make([]map[string]any, 0, len(matched))
	for _, t := range matched {
		j := map[string]any{"name": t.Name, "status": t.Status, "run_s": t.RunSeconds()}
		if s, ok := queue.Seconds(t.ID); ok {
			j["queue_s"] = s
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}
