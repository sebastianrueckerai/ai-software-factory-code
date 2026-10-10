package console

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"slices"
	"strconv"
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

// event writes e; a nil port records nothing (a test without events).
func (c *Console) event(ctx context.Context, e factoryd.Event) {
	if c.cfg.Events == nil {
		return
	}
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
	ps, err := c.cfg.Projects.Projects(ctx)
	if err != nil {
		return factoryd.Project{}, false, err
	}
	i := slices.IndexFunc(ps, func(p factoryd.Project) bool { return p.Name == name })
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

// projectEvents answers GET /projects/{name}/events: the project's events,
// newest first, as JSON, to a writer of it under the board's login rule
// (auth admitted the request only for a logged-in writer; here the project
// named must be one they write). limit and after page the feed; a bad
// one, or no such project, is answered.
func (c *Console) projectEvents(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, ok, err := c.projectOf(r.Context(), name)
	switch {
	case err != nil:
		log.Printf("console: projects: %v", err)
		http.Error(w, "cannot read the projects", http.StatusInternalServerError)
		return
	case !ok:
		http.NotFound(w, r)
		return
	}
	may, err := c.mayMoveCached(r.Context(), p.Repository, current(r).user, policy.MovedByWriters)
	switch {
	case err != nil:
		log.Printf("console: may %s write %s: %v", current(r).user, p.Repository, err)
		http.Error(w, "cannot check the project's writers", http.StatusServiceUnavailable)
		return
	case !may:
		http.Error(w, "you write no project "+name, http.StatusForbidden)
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
		detail := e.Detail
		if detail == nil {
			detail = map[string]any{}
		}
		out = append(out, eventJSON{e.ID, e.At.UTC(), e.Repository, e.TaskID, e.IssueNumber, e.Kind, e.Actor, e.Via, detail})
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil { // #nosec G104 -- best-effort response write
		log.Printf("console: the events of %s: %v", p.Repository, err)
	}
}
