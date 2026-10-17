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

// pathProject is the project the path names; false once the refusal is
// written. The route's access-table rule judges who may act on it; this
// only resolves the path.
func (c *Console) pathProject(w http.ResponseWriter, r *http.Request) (factoryd.Project, bool) {
	p, ok, err := c.projectOf(r.Context(), r.PathValue("name"))
	switch {
	case err != nil:
		log.Printf("console: projects: %v", err)
		http.Error(w, "cannot read the projects", http.StatusInternalServerError)
		return p, false
	case !ok:
		http.NotFound(w, r)
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
	p, ok := c.pathProject(w, r)
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
// board again; a test shortens it.
var countersWait = 2 * time.Second

// countersJSON are the board's figures the stream keeps live: the status
// line's markup and each column's count in the page's order.
type countersJSON struct {
	Status string   `json:"status"`
	Counts []string `json:"counts"`
}

// counters are d's counters as JSON; "" when they cannot be rendered.
func (c *Console) counters(d boardData) string {
	var status strings.Builder
	if err := c.tmpl.ExecuteTemplate(&status, "status", d); err != nil {
		log.Printf("console: the counters of %s: %v", d.Project, err)
		return ""
	}
	counts, err := c.counts(d)
	if err != nil {
		log.Printf("console: the counters of %s: %v", d.Project, err)
		return ""
	}
	b, err := json.Marshal(countersJSON{status.String(), counts})
	if err != nil {
		return ""
	}
	return string(b)
}

// follows reports whether the request's member sees project p's board,
// which its stream follows, and whether they write it: they write it,
// write the factory's own code, or read its repository (boardOf's rule).
func (c *Console) follows(r *http.Request, p factoryd.Project) (sees, writes bool, err error) {
	s := current(r)
	if writes, err = c.mayMoveCached(r.Context(), p.Repository, s.user, policy.MovedByWriters); err != nil || writes {
		return writes, writes, err
	}
	if _, factory, err := c.writer(r.Context(), s.user); err != nil || factory {
		return factory, false, err
	}
	sees, err = c.reads(r.Context(), s, p)
	return sees, false, err
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

// sightWait bounds a stream connect's retries of a sight check that
// errors; a test shortens it.
var sightWait = 4 * time.Second

// sight is follows with the connect's patience: a check that errors —
// a canceled ask, a Forgejo blip — is asked again until sightWait is
// out, keeping the distinction the stream's own heartbeat makes (its
// allowed, below: an error is waited out, a refusal ends the stream).
// So one errored check does not answer 503, which closes the browser's
// EventSource for good, while an outage cannot hold connects open
// either; a refusal is never retried, and the answer is whatever the
// last ask said.
func (c *Console) sight(r *http.Request, p factoryd.Project) (sees, writes bool, err error) {
	deadline := time.Now().Add(sightWait)
	for {
		sees, writes, err = c.follows(r, p)
		if err == nil {
			return sees, writes, nil
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return sees, writes, err
		}
		wait = min(wait, moveFailTTL) // re-ask once the failure cache has breathed out
		select {
		case <-r.Context().Done():
			return false, false, err
		case <-time.After(wait):
		}
	}
}

// projectEventsStream answers GET /projects/{name}/events/stream to whoever
// sees the project's board. A writer gets one SSE message per new events
// row, event "event", its id the row's, its data the feed's row, and a
// live-only event (session_said) without an id; a reconnect's
// Last-Event-ID replays the rows after it from the table, oldest first
// and at most the newest eventLimitMax of them; a first connect replays
// nothing. A member who only reads it gets no row: they carry steers and
// the agent's text, and the pulse's is the board's to show. countersWait
// after a live event other than the agent's text (and, to a writer, the
// pulse's), and at every heartbeat (ruling 288), the stream reads the
// board under the query's filter: event "counters" carries its counters
// when they changed, and a data-less event "board" says, at a heartbeat,
// that a card's face changed with time alone, and, to a member who only
// reads, that events came. Each heartbeat checks
// the session and the member's sight of the project again, and the stream
// ends once either is gone.
func (c *Console) projectEventsStream(w http.ResponseWriter, r *http.Request) {
	ctx, user := r.Context(), current(r).user
	p, ok, err := c.projectOf(ctx, r.PathValue("name"))
	switch {
	case err != nil:
		log.Printf("console: projects: %v", err)
		http.Error(w, "cannot read the projects", http.StatusInternalServerError)
		return
	case !ok:
		http.NotFound(w, r)
		return
	}
	sees, writes, err := c.sight(r, p)
	switch {
	case err != nil:
		log.Printf("console: may %s see %s: %v", user, p.Repository, err)
		http.Error(w, "cannot check who sees the project", http.StatusServiceUnavailable)
		return
	case !sees:
		http.Error(w, "you read no project "+p.Name, http.StatusForbidden)
		return
	}
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
	// look reads the board once; its first read learns the faces
	var counters, faces string
	look := func(events bool) error {
		d, err := c.boardOf(r, f, button(r))
		if err != nil {
			log.Printf("console: the stream of %s reads no board: %v", p.Repository, err)
			return nil
		}
		if now := c.counters(d); now != "" && now != counters {
			counters = now
			if err := write("event: counters\ndata: " + now + "\n\n"); err != nil {
				return err
			}
		}
		was := faces
		faces = d.faces()
		if was != "" && was != faces && !events || events && !writes {
			return write("event: board\ndata:\n\n")
		}
		return nil
	}
	// still sees the project: an error keeps the stream, a refusal ends it
	ck, _ := r.Cookie(sessionCookie)
	allowed := func() bool {
		s, err := c.sessionByCookie(ctx, ck.Value)
		if err != nil {
			log.Printf("console: the stream's session read: %v", err)
			return true
		}
		if s == nil {
			return false
		}
		login, _, err := c.writer(ctx, user)
		if err == nil && !login {
			return false
		}
		// a failed check leaves the member read-only, as now holds only a yes
		sees, now, err := c.follows(r, p)
		writes = now
		return err != nil || sees
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if err := write(""); err != nil {
		log.Printf("console: the stream of %s: %v", p.Repository, err)
		return
	}
	var replayed int64
	if after, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64); err == nil && after > 0 && writes {
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
	var due <-chan time.Time
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
			pulse := e.Kind == factoryd.EventPulse || e.Kind == factoryd.EventPulseComputing
			// live events may come out of id order: skip only the replayed ones
			if writes && (e.ID == 0 || e.ID > replayed) {
				if err := send(e); err != nil {
					return
				}
			}
			if due == nil && (!pulse || !writes) && e.Kind != factoryd.EventSessionSaid {
				due = time.After(countersWait)
			}
		case <-due:
			due = nil
			if look(true) != nil {
				return
			}
		case <-tick.C:
			if !allowed() || write(": ping\n\n") != nil || look(false) != nil {
				return
			}
		}
	}
}
