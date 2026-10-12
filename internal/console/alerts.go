package console

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// alertsLimit bounds the alerts one page shows.
const alertsLimit = 200

func (c *Console) alerts(w http.ResponseWriter, r *http.Request) {
	rows, err := c.cfg.Alerts.Alerts(r.Context(), alertsLimit)
	if err != nil {
		log.Printf("console: alerts: %v", err)
		http.Error(w, "cannot read the alerts", http.StatusInternalServerError)
		return
	}
	s := current(r)
	c.render(w, "alerts.html", map[string]any{"User": s.user, "CSRF": s.csrf, "Factory": true, "Alerts": rows, "Approver": c.approver(r),
		"Limit": alertsLimit})
}

// verdict is a factory approver's verdict on an open escalation alert.
func (c *Console) verdict(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := c.doAlertVerdict(r.Context(), button(r), id, r.PostFormValue("verdict")); err != nil {
		c.fail(w, r, err)
		return
	}
	// back to the alerts, or to the board under its filter: a path on
	// this host, never another
	back := "/alerts"
	if b := r.PostFormValue("back"); b == "/" || strings.HasPrefix(b, "/?") {
		back = b
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// alertRepository is the repository of alert id's task and the task's id,
// both "" when the alert, its task or their repository cannot be read.
func (c *Console) alertRepository(ctx context.Context, id int64) (repo, taskID string) {
	a, ok, err := c.cfg.Alerts.AlertByID(ctx, id)
	if err != nil {
		log.Printf("console: alert %d: %v", id, err)
		return "", ""
	}
	if !ok || a.TaskID == "" {
		return "", ""
	}
	t, ok, err := c.cfg.Tasks.Task(ctx, a.TaskID)
	if err != nil {
		log.Printf("console: alert %d: task %s: %v", id, a.TaskID, err)
		return "", ""
	}
	if !ok {
		return "", ""
	}
	return t.Repository, a.TaskID
}
