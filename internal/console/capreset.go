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

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
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

// poolReset is the panel's Reset of a project's escalation pool.
func (c *Console) poolReset(w http.ResponseWriter, r *http.Request) {
	p, ok, err := c.projectOf(r.Context(), r.PathValue("name"))
	if err != nil || !ok {
		http.NotFound(w, r)
		return
	}
	c.resetCap(w, r, policy.CapPool, p.Repository)
}

// chatReset is the panel's Reset of a project's judgment budget: the
// month's chat task's budget rises until the policy's monthly figure is
// free again (chatResetEUR), a fund as the stop's own (doChatFund). A
// budget with that much free already is refused with a 409.
func (c *Console) chatReset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, ok, err := c.projectOf(r.Context(), name)
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
	stop, err := c.chatStopNow(r.Context(), p)
	if err != nil {
		log.Printf("console: the chat task of %s: %v", p.Repository, err)
		c.fail(w, r, refuse(http.StatusInternalServerError, "The judgment budget could not be read. Press Reset again."))
		return
	}
	monthly := v.Document.ChatOf(v.Projects[p.Name]).MonthlyEUR
	eur := chatResetEUR(stop.task, monthly)
	if eur <= 0 {
		c.stalePanel(w, r, "The judgment budget has "+textutil.EUR(monthly)+" EUR free: there is nothing to reset.")
		return
	}
	if _, _, err := c.doChatFund(r.Context(), button(r), name, eur, chatResetNote, ""); err != nil {
		c.fail(w, r, err)
		return
	}
	c.pressed(w, r, nil)
}

// chatResetNote is the note of a fund the panel's Reset made.
const chatResetNote = "reset in the status panel"

// chatResetEUR is what a Reset adds to chat task t's budget so that
// monthly, the policy's monthly figure, is free again: 0 when that much
// is free already.
func chatResetEUR(t factoryd.Task, monthly float64) float64 {
	return max(math.Round((monthly-(t.BudgetEUR-t.EnforcedSpent()))*100)/100, 0)
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
	policy.CapPremium: "this month's premium count", policy.CapPool: "the project's pool count"}

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
