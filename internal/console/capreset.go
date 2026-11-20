package console

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// factoryCapReset is the status panel's Reset of the day's or the
// month's cap: a human's press, in no chat or MCP tool.
func (c *Console) factoryCapReset(w http.ResponseWriter, r *http.Request) {
	cap := r.PathValue("cap")
	if cap != policy.CapDay && cap != policy.CapMonth {
		http.NotFound(w, r)
		return
	}
	c.resetCap(w, r, cap, "")
}

// projectCapReset is the panel's Reset of a project's month cap.
func (c *Console) projectCapReset(w http.ResponseWriter, r *http.Request) {
	p, ok, err := c.projectOf(r.Context(), r.PathValue("name"))
	if err != nil || !ok {
		http.NotFound(w, r)
		return
	}
	c.resetCap(w, r, policy.CapProject, p.Repository)
}

// resetCap restarts cap's count for the session's human and wakes the
// cards that wait on a cap. A failed wake is logged: the waits check on
// their own.
func (c *Console) resetCap(w http.ResponseWriter, r *http.Request, cap, repository string) {
	if c.cfg.Caps == nil {
		http.Error(w, "The caps' counts cannot be reset here.", http.StatusServiceUnavailable)
		return
	}
	reset, err := c.cfg.Caps.ResetCap(r.Context(), cap, repository, current(r).user)
	if err != nil {
		log.Printf("console: reset the %s cap: %v", cap, err)
		c.fail(w, r, refuse(http.StatusInternalServerError, "The "+cap+" cap's count could not be reset: "+errText(err)+". Press Reset again."))
		return
	}
	if c.cfg.WakeCapWaits != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
		defer cancel()
		if err := c.cfg.WakeCapWaits(ctx); err != nil {
			log.Printf("console: wake the cap waits after reset %d: %v", reset.ID, err)
		}
	}
	c.pressed(w, r, nil)
}
