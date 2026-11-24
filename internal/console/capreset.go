package console

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// factoryCapReset is the status panel's Reset of the day's, the month's
// or the premium's cap: a human's press, in no chat or MCP tool.
func (c *Console) factoryCapReset(w http.ResponseWriter, r *http.Request) {
	cap := r.PathValue("cap")
	if cap != policy.CapDay && cap != policy.CapMonth && cap != policy.CapPremium {
		http.NotFound(w, r)
		return
	}
	c.resetCap(w, r, cap, "")
}

// projectCapReset is the panel's Reset of a project's month cap, refused
// for a project without one of its own.
func (c *Console) projectCapReset(w http.ResponseWriter, r *http.Request) {
	p, ok, err := c.projectOf(r.Context(), r.PathValue("name"))
	if err != nil || !ok {
		http.NotFound(w, r)
		return
	}
	v, ok, err := c.cfg.Policies.CurrentPolicy(r.Context())
	if err != nil || !ok {
		log.Printf("console: current policy: loaded %v, %v", ok, err)
		http.Error(w, "The policy in force cannot be read. Press Reset again in a moment.", http.StatusServiceUnavailable)
		return
	}
	if _, eur := v.ProjectCap(p.Repository); math.IsInf(eur, 1) {
		c.stalePanel(w, r, "Project "+p.Name+" has no month cap of its own.")
		return
	}
	c.resetCap(w, r, policy.CapProject, p.Repository)
}

// stalePanel refuses a press made on a panel that no longer stands with a
// 409 of line, and under htmx refreshes the board, so the panel shows
// what stands now.
func (c *Console) stalePanel(w http.ResponseWriter, r *http.Request, line string) {
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Trigger", "board-refresh")
	}
	c.fail(w, r, refuse(http.StatusConflict, line))
}

// capCounts name each cap's count as its press does.
var capCounts = map[string]string{policy.CapDay: "today's count", policy.CapMonth: "this month's count", policy.CapProject: "the project's month count",
	policy.CapPremium: "this month's premium count"}

// resetCap restarts cap's count for the session's human and wakes the
// cards that wait on a cap, when the form's seen is the newest reset of
// the cap its panel showed (0 for none). A newer one refuses the press
// with a 409 that names it and refreshes the board, so the panel shows
// it. A failed wake is logged: the waits check on their own.
func (c *Console) resetCap(w http.ResponseWriter, r *http.Request, cap, repository string) {
	if c.cfg.Caps == nil {
		http.Error(w, "The caps' counts cannot be reset here.", http.StatusServiceUnavailable)
		return
	}
	seen, err := strconv.ParseInt(r.FormValue("seen"), 10, 64)
	if err != nil || seen < 0 {
		http.Error(w, "The form names no reset it saw. Reload the board and press Reset again.", http.StatusBadRequest)
		return
	}
	reset, err := c.cfg.Caps.ResetCap(r.Context(), cap, repository, current(r).user, seen)
	var stale *policy.StaleResetError
	if errors.As(err, &stale) {
		n := stale.Newest
		c.stalePanel(w, r, n.Login+" reset "+capCounts[cap]+"; the panel now shows it. Press Reset again to reset it once more.")
		return
	}
	if err != nil {
		log.Printf("console: reset the %s cap: %v", cap, err)
		c.fail(w, r, refuse(http.StatusInternalServerError, "The "+cap+" cap's count could not be reset: "+errText(err)+". Press Reset again."))
		return
	}
	if c.cfg.WakeCapWaits != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
		defer cancel()
		why := "console: " + reset.Login + " reset the " + cap + " cap's count"
		if repository != "" {
			why += " of " + repository
		}
		if err := c.cfg.WakeCapWaits(ctx, fmt.Sprintf("%s (reset %d)", why, reset.ID)); err != nil {
			log.Printf("console: wake the cap waits after reset %d: %v", reset.ID, err)
		}
	}
	c.pressed(w, r, nil)
}
