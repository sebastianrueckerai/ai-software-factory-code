package console

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// Pulses reads a project's newest pulse.
type Pulses interface {
	NewestPulse(ctx context.Context, repository string) (factoryd.Pulse, bool, error)
}

// pulseOf is project p's newest pulse for the request's member, under
// GET /pulse's rule; nil for none, one unreadable or a member who neither
// writes nor reads p.
func (c *Console) pulseOf(r *http.Request, p factoryd.Project) *factoryd.Pulse {
	if c.cfg.Pulses == nil {
		return nil
	}
	reads, err := c.reads(r.Context(), current(r), p)
	if err != nil {
		log.Printf("console: may %s read %s: %v", current(r).user, p.Repository, err)
	}
	if !reads {
		return nil
	}
	row, ok, err := c.cfg.Pulses.NewestPulse(r.Context(), p.Repository)
	if err != nil {
		log.Printf("console: the pulse of %s: %v", p.Repository, err)
	}
	if !ok {
		return nil
	}
	return &row
}

// pulseGet answers GET /projects/{name}/pulse: the project's newest pulse
// as JSON, to a member who writes or reads it (403 otherwise); 404 for no
// such project or no pulse yet.
func (c *Console) pulseGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, ok, err := c.projectOf(ctx, r.PathValue("name"))
	if err != nil {
		log.Printf("console: projects: %v", err)
		http.Error(w, "cannot read the projects", http.StatusInternalServerError)
		return
	}
	if !ok || c.cfg.Pulses == nil {
		http.NotFound(w, r)
		return
	}
	reads, err := c.reads(ctx, current(r), p)
	switch {
	case err != nil:
		log.Printf("console: may %s read %s: %v", current(r).user, p.Repository, err)
		http.Error(w, "cannot check who reads the project", http.StatusServiceUnavailable)
		return
	case !reads:
		http.Error(w, "you read no project "+p.Name, http.StatusForbidden)
		return
	}
	row, ok, err := c.cfg.Pulses.NewestPulse(ctx, p.Repository)
	switch {
	case err != nil:
		log.Printf("console: the pulse of %s: %v", p.Repository, err)
		http.Error(w, "cannot read the pulse", http.StatusInternalServerError)
		return
	case !ok:
		http.Error(w, "the project has no pulse yet", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(row); err != nil {
		log.Printf("console: the pulse of %s: %v", p.Repository, err)
	}
}

// reads reports whether member s reads project p: they write it, or
// Forgejo shows them its repository.
func (c *Console) reads(ctx context.Context, s *session, p factoryd.Project) (bool, error) {
	if s.user == "" {
		return false, nil
	}
	if writes, err := c.mayMoveCached(ctx, p.Repository, s.user, policy.MovedByWriters); err == nil && writes {
		return true, nil
	}
	repo := p.Repo()
	err := c.as(ctx, s, http.MethodGet, "/api/v1/repos/"+url.PathEscape(repo.Owner)+"/"+url.PathEscape(repo.Name), nil, http.StatusOK, nil)
	var api *apiError
	if errors.As(err, &api) && (api.status == http.StatusNotFound || api.status == http.StatusForbidden) {
		return false, nil
	}
	return err == nil, err
}

// pulseTurn reads project p as the project agent reads it, seen by no
// member: its snapshot with status line and its last
// factoryd.PulseEvents events.
func (c *Console) pulseTurn(ctx context.Context, p factoryd.Project) (string, statusLine, error) {
	r, err := http.NewRequestWithContext(context.WithValue(ctx, ctxKey{}, &session{}), http.MethodGet, "/", nil)
	if err != nil {
		return "", statusLine{}, err
	}
	return (&turn{c: c, r: r, p: p}).project(factoryd.PulseEvents)
}

// PulseContext is the pulse's prompt (factoryd.Pulser): pulseTurn's
// snapshot and whether a card on p waits on a human.
func (c *Console) PulseContext(ctx context.Context, p factoryd.Project) (string, bool, error) {
	text, s, err := c.pulseTurn(ctx, p)
	return text, s.NeedYou > 0, err
}

// PulseState answers the heartbeat's two questions from the same
// snapshot: whether work sits open on p's board and whether a card
// waits on a human.
func (c *Console) PulseState(ctx context.Context, p factoryd.Project) (workOpen, waitingHuman bool, err error) {
	_, s, err := c.pulseTurn(ctx, p)
	return s.Working > 0 || s.Waiting > 0, s.NeedYou > 0, err
}
