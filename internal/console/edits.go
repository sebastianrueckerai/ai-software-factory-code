package console

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// maxSummaryLen bounds a human's card summary: what its face's title line
// can hold, one clause of a heading.
const maxSummaryLen = 255

// mayWriteTask is whether the session's human writes task t's project:
// the writers moved_by of its process's lanes, asked of Forgejo through
// the board's cache. A refusal answers 403; an unreadable answer, 503.
func (c *Console) mayWriteTask(w http.ResponseWriter, r *http.Request, t factoryd.Task) bool {
	ok, err := c.mayMoveCached(r.Context(), t.Repository, current(r).user, policy.MovedByWriters)
	switch {
	case err != nil:
		log.Printf("console: may %s write %s: %v", current(r).user, t.Repository, err)
		http.Error(w, "cannot check the project's writers", http.StatusServiceUnavailable)
		return false
	case !ok:
		http.Error(w, "you write no project of task "+t.ID, http.StatusForbidden)
		return false
	}
	return true
}

// setSummary is POST /tasks/{id}/summary: a writer's one-line summary of
// the card, shown on its face instead of the generated title. An empty
// value restores the generated one. One action, one event row.
func (c *Console) setSummary(w http.ResponseWriter, r *http.Request) {
	t, ok := c.taskOf(w, r)
	if !ok {
		return
	}
	if !c.mayWriteTask(w, r, t) {
		return
	}
	summary := r.PostFormValue("summary")
	if utf8.RuneCountInString(summary) > maxSummaryLen {
		http.Error(w, "a summary of up to 255 characters", http.StatusBadRequest)
		return
	}
	// a value of nothing but spaces restores the generated title.
	if strings.TrimSpace(summary) == "" {
		summary = ""
	}
	if err := c.cfg.Tasks.SetHumanSummary(r.Context(), t.ID, summary); err != nil {
		if errors.Is(err, factoryd.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		log.Printf("console: %s summarized %s: %v", current(r).user, t.ID, err)
		http.Error(w, "the summary could not be stored", http.StatusInternalServerError)
		return
	}
	log.Printf("console: %s set the summary of %s (%q)", current(r).user, t.ID, summary)
	c.act(r, t.Repository, t.ID, t.IssueNumber, factoryd.EventSummarySet, map[string]any{"summary": summary})
	c.showBoard(w, r, http.StatusOK, "The card's summary is set.", nil)
}

// setAccent is POST /tasks/{id}/accent: a writer's colour label on the
// card, one of factoryd.Accents or "" to remove it. The accent shows as a
// chip and the card's left edge; it never changes a state rendering and
// never enters an agent's brief. One action, one event row.
func (c *Console) setAccent(w http.ResponseWriter, r *http.Request) {
	t, ok := c.taskOf(w, r)
	if !ok {
		return
	}
	if !c.mayWriteTask(w, r, t) {
		return
	}
	accent := r.PostFormValue("value")
	if accent != "" && !factoryd.IsAccent(accent) {
		http.Error(w, "accent must be one of "+strings.Join(factoryd.Accents, ", "), http.StatusBadRequest)
		return
	}
	if err := c.cfg.Tasks.SetTaskAccent(r.Context(), t.ID, accent); err != nil {
		if errors.Is(err, factoryd.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		log.Printf("console: %s accented %s: %v", current(r).user, t.ID, err)
		http.Error(w, "the accent could not be stored", http.StatusInternalServerError)
		return
	}
	log.Printf("console: %s set the accent of %s (%q)", current(r).user, t.ID, accent)
	c.act(r, t.Repository, t.ID, t.IssueNumber, factoryd.EventAccentSet, map[string]any{"accent": accent})
	c.showBoard(w, r, http.StatusOK, "The card's accent is set.", nil)
}
