package factoryd

import (
	"cmp"
	"context"
	"log"
	"slices"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// CIRuns turns the code repository's actions/tasks into
// factory.ci.job.duration once per finished job and factory.ci.run.duration
// once per finished run, and remembers each push run's start for the roll
// record. It keeps what it emitted in memory: after a restart it starts
// from the newest page, and jobs that finished while it was down are lost.
type CIRuns struct {
	Forgejo     *Forgejo
	Owner, Repo string
	Metrics     *telemetry.Metrics
	now         func() time.Time
	lastErr     string // logged once until the next success

	mu      sync.Mutex
	started bool
	// floor: every task at or below it is handled; emitted holds the
	// finished ones above it
	floor   int64
	emitted map[int64]bool
	runs    map[int64]*ciRun
	pushes  map[string]time.Time // head sha of a push run → its first job's created_at
}

type ciRun struct {
	event       string
	first, last time.Time
	status      string
	open        map[int64]bool // tasks not yet finished
	done        bool
}

const (
	ciRunsInterval = time.Minute
	// ciRunSettle: a run whose known jobs all finished this long ago is
	// finished. actions/tasks lists a job only once a runner took it, so a
	// job still blocked on its needs is invisible until then.
	ciRunSettle = 5 * time.Minute
	// ciRunsPages bounds one poll's read; older tasks count as handled.
	ciRunsPages = 10
	ciMemory    = 24 * time.Hour
)

func (c *CIRuns) clock() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
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
	c.mu.Lock()
	floor, boot := c.floor, !c.started
	c.mu.Unlock()
	var tasks []ActionTask
	for page := 1; page <= ciRunsPages; page++ {
		items, err := c.Forgejo.ActionTasks(ctx, c.Owner, c.Repo, page)
		if err != nil {
			if err.Error() != c.lastErr {
				log.Printf("ci metrics: actions tasks: %v", err)
				c.lastErr = err.Error()
			}
			return
		}
		c.lastErr = ""
		tasks = append(tasks, items...)
		if boot || len(items) == 0 || items[len(items)-1].ID <= floor {
			break
		}
	}
	c.observe(ctx, tasks)
}

// observe handles one read of tasks, newest first as Forgejo lists them.
func (c *CIRuns) observe(ctx context.Context, tasks []ActionTask) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.emitted == nil {
		c.emitted, c.runs, c.pushes = map[int64]bool{}, map[int64]*ciRun{}, map[string]time.Time{}
	}
	now := c.clock()
	tasks = slices.DeleteFunc(slices.Clone(tasks), func(t ActionTask) bool { return t.ID <= c.floor })
	slices.SortFunc(tasks, func(a, b ActionTask) int { return cmp.Compare(a.ID, b.ID) })
	boot := !c.started
	c.started = true
	for _, t := range tasks {
		if t.Event == "push" {
			if at, ok := c.pushes[t.HeadSHA]; !ok || t.CreatedAt.Before(at) {
				c.pushes[t.HeadSHA] = t.CreatedAt
			}
		}
		c.observeRun(t)
		if !t.Finished() || c.emitted[t.ID] {
			continue
		}
		c.emitted[t.ID] = true
		if boot {
			continue
		}
		c.Metrics.CIJobDuration.Record(ctx, t.RunSeconds(), metric.WithAttributes(
			attribute.String("job", t.Name), attribute.String("event", t.Event), attribute.String("status", t.Status)))
	}
	for number, r := range c.runs {
		if r.done || len(r.open) > 0 {
			continue
		}
		if boot {
			r.done = true
			continue
		}
		if now.Sub(r.last) < ciRunSettle {
			continue
		}
		r.done = true
		c.Metrics.CIRunDuration.Record(ctx, r.last.Sub(r.first).Seconds(), metric.WithAttributes(
			attribute.String("event", r.event), attribute.String("status", r.status)))
		log.Printf("ci metrics: run %d (%s) %s in %.0f s", number, r.event, r.status, r.last.Sub(r.first).Seconds())
	}
	c.advance(tasks, now)
}

func (c *CIRuns) observeRun(t ActionTask) {
	r := c.runs[t.RunNumber]
	if r == nil {
		r = &ciRun{event: t.Event, first: t.CreatedAt, status: "success", open: map[int64]bool{}}
		c.runs[t.RunNumber] = r
	}
	if r.done {
		return
	}
	if t.CreatedAt.Before(r.first) {
		r.first = t.CreatedAt
	}
	if !t.Finished() {
		r.open[t.ID] = true
		return
	}
	delete(r.open, t.ID)
	if t.UpdatedAt.After(r.last) {
		r.last = t.UpdatedAt
	}
	switch {
	case t.Status == "failure":
		r.status = "failure"
	case t.Status == "cancelled" && r.status != "failure":
		r.status = "cancelled"
	}
}

// advance moves the floor below the oldest unfinished task read, or to
// the newest read, and forgets what lies under it.
func (c *CIRuns) advance(tasks []ActionTask, now time.Time) {
	floor := c.floor
	for _, t := range tasks {
		if !t.Finished() {
			floor = t.ID - 1
			break
		}
		floor = t.ID
	}
	c.floor = floor
	for id := range c.emitted {
		if id <= floor {
			delete(c.emitted, id)
		}
	}
	for n, r := range c.runs {
		if r.done && now.Sub(r.first) > ciMemory {
			delete(c.runs, n)
		}
	}
	for sha, at := range c.pushes {
		if now.Sub(at) > ciMemory {
			delete(c.pushes, sha)
		}
	}
}

// PushedAt is the created_at of sha's push run: its first job.
func (c *CIRuns) PushedAt(sha string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.pushes[sha]
	return at, ok
}

// CIQueue measures how long each Actions job waits for a runner: from the
// first poll that sees it waiting to the first that sees it picked up
// (the spawner's 10 s poll is the resolution). It emits
// factory.ci.job.queue and remembers the result by task id for the checks
// step. In memory only: a restart loses the jobs waiting across it.
type CIQueue struct {
	metrics *telemetry.Metrics
	now     func() time.Time
	mu      sync.Mutex
	waiting map[int64]waitingJob // job id
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
	return &CIQueue{metrics: m, now: time.Now, waiting: map[int64]waitingJob{}, byTask: map[int64]queued{}}
}

// Observe takes one poll's jobs for label. A job that vanishes while
// waiting (cancelled, or taken and finished between two polls) is dropped
// unmeasured.
func (q *CIQueue) Observe(ctx context.Context, label string, jobs []CIJob) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
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
		if !tracked {
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
	for id, e := range q.byTask {
		if now.Sub(e.at) > ciMemory {
			delete(q.byTask, id)
		}
	}
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
