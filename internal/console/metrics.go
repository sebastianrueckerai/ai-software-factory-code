package console

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// metricsDays is the metrics page's default window of UTC days, today
// included.
const metricsDays = 28

// projectMetrics answers GET /projects/{name}/metrics: a project's
// figures over the window and the one before it, as the page, and as
// JSON for the seam (format=json). Query params from and to are
// YYYY-MM-DD dates; each defaults as before.
func (c *Console) projectMetrics(w http.ResponseWriter, r *http.Request) {
	p, ok := c.pathProject(w, r)
	if !ok {
		return
	}
	now := time.Now().UTC()
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	from := to.AddDate(0, 0, -(metricsDays - 1))
	q := r.URL.Query()
	if v := q.Get("from"); v != "" {
		d, _, ok := parseCostsDay(v)
		if !ok {
			http.Error(w, "from: want YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		from = d
	}
	if v := q.Get("to"); v != "" {
		d, _, ok := parseCostsDay(v)
		if !ok {
			http.Error(w, "to: want YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		to = d
	}
	if from.After(to) {
		http.Error(w, "from is after to", http.StatusBadRequest)
		return
	}
	report, err := c.cfg.Figures.ProjectMetrics(r.Context(), p.Repository, from, to)
	if err != nil {
		log.Printf("console: the figures of %s: %v", p.Repository, err)
		http.Error(w, "cannot read the figures", http.StatusInternalServerError)
		return
	}
	if q.Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(report); err != nil { // #nosec G104 -- best-effort response write
			log.Printf("console: the figures of %s: %v", p.Repository, err)
		}
		return
	}
	s := current(r)
	c.render(w, "metrics.html", map[string]any{"User": s.user, "CSRF": s.csrf,
		"Project": p, "Report": report, "From": from, "To": to})
}
