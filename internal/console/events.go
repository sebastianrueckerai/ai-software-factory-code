package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// eventLimitDefault is the project feed's page size when the query names
// none; eventLimitMax bounds it.
const (
	eventLimitDefault = 50
	eventLimitMax     = 500
)

// act records what the human just did as one event row, via button, with
// their login as actor. A failure is logged only: an action that took
// effect is never undone by its own event row, and the human already has
// their answer.
func (c *Console) act(r *http.Request, repository, taskID string, issueNumber int64, kind string, detail map[string]any) {
	c.event(r.Context(), factoryd.Event{Repository: repository, TaskID: taskID, IssueNumber: issueNumber,
		Kind: kind, Actor: current(r).user, Via: factoryd.ViaButton, Detail: detail})
}

// event writes e.
func (c *Console) event(ctx context.Context, e factoryd.Event) {
	if _, err := c.cfg.Events.Record(ctx, e); err != nil {
		log.Printf("console: event %s of %s: %v", e.Kind, e.Repository, err)
	}
}

// commentDetail is an answer event's detail: the id of the issue comment
// the button posted (0 = Forgejo named none).
func commentDetail(commentID int64) map[string]any {
	if commentID == 0 {
		return nil
	}
	return map[string]any{"comment": commentID}
}

// eventJSON is one row of the project feed.
type eventJSON struct {
	ID          int64          `json:"id"`
	At          time.Time      `json:"at"`
	Repository  string         `json:"repository"`
	TaskID      string         `json:"task_id"`
	IssueNumber int64          `json:"issue_number"`
	Kind        string         `json:"kind"`
	Actor       string         `json:"actor"`
	Via         string         `json:"via"`
	Detail      map[string]any `json:"detail"`
}

// projectOf is the project of that name among the configuration's; false
// when no project has it.
func (c *Console) projectOf(ctx context.Context, name string) (factoryd.Project, bool, error) {
	return c.projectWhere(ctx, func(p factoryd.Project) bool { return p.Name == name })
}

// projectAt is the project of repository.
func (c *Console) projectAt(ctx context.Context, repository string) (factoryd.Project, bool, error) {
	return c.projectWhere(ctx, func(p factoryd.Project) bool { return p.Repository == repository })
}

func (c *Console) projectWhere(ctx context.Context, match func(factoryd.Project) bool) (factoryd.Project, bool, error) {
	ps, err := c.cfg.Projects.Projects(ctx)
	if err != nil {
		return factoryd.Project{}, false, err
	}
	i := slices.IndexFunc(ps, match)
	if i < 0 {
		return factoryd.Project{}, false, nil
	}
	return ps[i], true, nil
}

// projectRepository is the repository (owner/name) of the project that
// name files or merges: its own one once a row lists it, else the
// repository its file will hold, name/name. Every event carries a project,
// so this is never the infra repository.
func (c *Console) projectRepository(r *http.Request, name string) string {
	p, ok, err := c.projectOf(r.Context(), name)
	switch {
	case err != nil:
		log.Printf("console: project %s: %v", name, err)
	case !ok || p.Repository == "":
		return name + "/" + name
	default:
		return p.Repository
	}
	return name + "/" + name
}

// writtenProject is the project the path names when the session's human
// writes it; false once the refusal is written (404 for no such project,
// 403 for one they do not write).
func (c *Console) writtenProject(w http.ResponseWriter, r *http.Request) (factoryd.Project, bool) {
	name := r.PathValue("name")
	p, ok, err := c.projectOf(r.Context(), name)
	switch {
	case err != nil:
		log.Printf("console: projects: %v", err)
		http.Error(w, "cannot read the projects", http.StatusInternalServerError)
		return p, false
	case !ok:
		http.NotFound(w, r)
		return p, false
	}
	may, err := c.mayMoveCached(r.Context(), p.Repository, current(r).user, policy.MovedByWriters)
	switch {
	case err != nil:
		log.Printf("console: may %s write %s: %v", current(r).user, p.Repository, err)
		http.Error(w, "cannot check the project's writers", http.StatusServiceUnavailable)
		return p, false
	case !may:
		http.Error(w, "you write no project "+name, http.StatusForbidden)
		return p, false
	}
	return p, true
}

// feedRow is e as the feed and the stream answer it: a missing detail is {}.
func feedRow(e factoryd.Event) eventJSON {
	detail := e.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	return eventJSON{e.ID, e.At.UTC(), e.Repository, e.TaskID, e.IssueNumber, e.Kind, e.Actor, e.Via, detail}
}

// projectEvents answers GET /projects/{name}/events: the project's events,
// newest first, as JSON, to a writer of it under the board's login rule
// (auth admitted the request only for a logged-in writer; here the project
// named must be one they write). limit and after page the feed; a bad
// one, or no such project, is answered.
func (c *Console) projectEvents(w http.ResponseWriter, r *http.Request) {
	p, ok := c.writtenProject(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := eventLimitDefault
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > eventLimitMax {
			http.Error(w, "limit is a whole number from 1 to "+strconv.Itoa(eventLimitMax), http.StatusBadRequest)
			return
		}
		limit = n
	}
	var after int64
	if raw := q.Get("after"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			http.Error(w, "after is an event id", http.StatusBadRequest)
			return
		}
		after = n
	}
	events, err := c.cfg.Events.ProjectEvents(r.Context(), p.Repository, after, limit)
	if err != nil {
		log.Printf("console: the events of %s: %v", p.Repository, err)
		http.Error(w, "cannot read the events", http.StatusInternalServerError)
		return
	}
	out := make([]eventJSON, 0, len(events))
	for _, e := range events {
		out = append(out, feedRow(e))
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil { // #nosec G104 -- best-effort response write
		log.Printf("console: the events of %s: %v", p.Repository, err)
	}
}

// heartbeat is how often the stream writes a comment line, which keeps an
// idle stream open through the Gateway; a test shortens it.
var heartbeat = 25 * time.Second

// streamBuffer is how many events a stream may fall behind before the bus
// drops it.
const streamBuffer = 16

// countersWait is how long a stream gathers events before it reads the
// board's counters again; a test shortens it.
var countersWait = 2 * time.Second

// countersJSON are the board's figures the stream keeps live: the status
// line's markup and each column's count in the page's order.
type countersJSON struct {
	Status string `json:"status"`
	Counts []int  `json:"counts"`
}

// counters are the counters of the board under f as the stream's member
// sees it, as JSON; "" when the board cannot be read.
func (c *Console) counters(r *http.Request, f boardFilter) string {
	d, err := c.boardOf(r, f)
	if err != nil {
		log.Printf("console: the counters of %s: %v", f.Project, err)
		return ""
	}
	var status strings.Builder
	if err := c.tmpl.ExecuteTemplate(&status, "status", d); err != nil {
		log.Printf("console: the counters of %s: %v", f.Project, err)
		return ""
	}
	b, err := json.Marshal(countersJSON{status.String(), d.counts()})
	if err != nil {
		return ""
	}
	return string(b)
}

// sseWriter writes s to w and flushes it, each write cut off after window:
// the server's WriteTimeout would end every stream after a minute.
func sseWriter(w http.ResponseWriter, window time.Duration) func(s string) error {
	rc := http.NewResponseController(w)
	return func(s string) error {
		if err := rc.SetWriteDeadline(time.Now().Add(window)); err != nil {
			return err
		}
		if _, err := io.WriteString(w, s); err != nil {
			return err
		}
		return rc.Flush()
	}
}

// projectEventsStream answers GET /projects/{name}/events/stream to a
// writer of the project: one SSE message per new events row, event
// "event", its id the row's, its data the feed's row; a live-only event
// (session_said) without an id. A reconnect's Last-Event-ID replays the
// rows after it from the table, oldest first and at most the newest
// eventLimitMax of them; a first connect replays nothing. countersWait
// after a live event other than the pulse's, event "counters" carries the
// board's counters under the query's filter when they changed. Each
// heartbeat checks the session and the write right again, and the stream
// ends once either is gone.
func (c *Console) projectEventsStream(w http.ResponseWriter, r *http.Request) {
	p, ok := c.writtenProject(w, r)
	if !ok {
		return
	}
	ctx, user := r.Context(), current(r).user
	ch := make(chan factoryd.Event, streamBuffer)
	cancel := c.cfg.Bus.Subscribe(p.Repository, ch)
	defer cancel()
	// each write may take two heartbeats, so a stalled client is cut off
	write := sseWriter(w, 2*heartbeat)
	send := func(e factoryd.Event) error {
		b, err := json.Marshal(feedRow(e))
		if err != nil {
			return err
		}
		if e.ID == 0 {
			return write(fmt.Sprintf("event: event\ndata: %s\n\n", b))
		}
		return write(fmt.Sprintf("id: %d\nevent: event\ndata: %s\n\n", e.ID, b))
	}
	f := urlFilter(r)
	f.Project = p.Name
	var due <-chan time.Time
	var last string
	// still writes the project: an error keeps the stream, a refusal ends it
	allowed := func() bool {
		if c.session(r) == nil {
			return false
		}
		login, _, err := c.writer(ctx, user)
		if err == nil && !login {
			return false
		}
		may, err := c.mayMoveCached(ctx, p.Repository, user, policy.MovedByWriters)
		return err != nil || may
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if err := write(""); err != nil {
		log.Printf("console: the stream of %s: %v", p.Repository, err)
		return
	}
	var replayed int64
	if after, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64); err == nil && after > 0 {
		replayed = after
		rows, err := c.cfg.Events.ProjectEvents(ctx, p.Repository, after, eventLimitMax)
		if err != nil {
			log.Printf("console: the stream of %s replays nothing: %v", p.Repository, err)
		}
		for i := len(rows) - 1; i >= 0; i-- {
			if err := send(rows[i]); err != nil {
				return
			}
			replayed = max(replayed, rows[i].ID)
		}
	}
	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			// live events may come out of id order: skip only the replayed ones
			if e.ID == 0 || e.ID > replayed {
				if err := send(e); err != nil {
					return
				}
			}
			if due == nil && e.Kind != factoryd.EventPulse && e.Kind != factoryd.EventPulseComputing {
				due = time.After(countersWait)
			}
		case <-due:
			due = nil
			if now := c.counters(r, f); now != "" && now != last {
				last = now
				if write("event: counters\ndata: "+now+"\n\n") != nil {
					return
				}
			}
		case <-tick.C:
			if !allowed() || write(": ping\n\n") != nil {
				return
			}
		}
	}
}
