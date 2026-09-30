package console

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
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
	c.render(w, "alerts.html", map[string]any{"User": s.user, "CSRF": s.csrf, "Alerts": rows, "Approver": c.approver(r),
		"Limit": alertsLimit})
}

// verdict is an approver's verdict on an open escalation alert.
func (c *Console) verdict(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	user := current(r).user
	if !c.approver(r) {
		log.Printf("console: %s is no approver: verdict on alert %d refused", user, id)
		http.Error(w, "Only the policy's approvers may judge an alert.", http.StatusForbidden)
		return
	}
	v := r.PostFormValue("verdict")
	if v != factoryd.VerdictReal && v != factoryd.VerdictNoise {
		http.Error(w, "the verdict is real or noise", http.StatusBadRequest)
		return
	}
	switch err := c.cfg.Alerts.SetAlertVerdict(r.Context(), id, v, user); {
	case errors.Is(err, factoryd.ErrNotFound):
		http.Error(w, fmt.Sprintf("alert %d is no open human alert (any more); reload the page", id), http.StatusConflict)
		return
	case err != nil:
		log.Printf("console: verdict on alert %d: %v", id, err)
		http.Error(w, "cannot record the verdict", http.StatusInternalServerError)
		return
	}
	log.Printf("console: %s judged alert %d %s", user, id, v)
	c.cfg.Metrics.Intervened(r.Context(), factoryd.InterventionAlertVerdict, "none", "none")
	back := "/alerts"
	if r.PostFormValue("back") == "/" {
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
