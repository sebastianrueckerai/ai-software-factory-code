package console

import (
	"encoding/json"
	"log"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// costDays is how far back /costs looks, today included.
const costDays = 14

var ledgerKinds = []string{policy.KindModel, policy.KindTool, policy.KindResource, policy.KindSettlement}

type costRow struct {
	Day   string
	Kinds []float64 // in ledgerKinds order
	Total float64
}

// costs shows the ledger per day and per task over a window of UTC days,
// the last 14 by default, today included, and the window's last day's
// total against the policy's day cap. Query params from and to are
// YYYY-MM-DD dates, both included; each defaults as before.
func (c *Console) costs(w http.ResponseWriter, r *http.Request) {
	to := time.Now().UTC()
	from := to.AddDate(0, 0, -(costDays - 1))
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
	ledger, err := c.cfg.Costs.Costs(r.Context(), from, to)
	if err != nil {
		log.Printf("console: costs: %v", err)
		http.Error(w, "cannot read the ledger", http.StatusInternalServerError)
		return
	}
	v, ok, err := c.cfg.Policies.CurrentPolicy(r.Context())
	if err != nil || !ok {
		log.Printf("console: current policy: loaded %v, %v", ok, err)
		http.Error(w, "no current policy version", http.StatusServiceUnavailable)
		return
	}
	today := to.Format(policy.DateLayout)
	var rows []costRow
	var todayTotal float64
	for i := len(ledger.Days) - 1; i >= 0; i-- {
		d := ledger.Days[i]
		row := costRow{Day: d.Day, Total: d.Total}
		for _, k := range ledgerKinds {
			row.Kinds = append(row.Kinds, d.ByKind[k])
		}
		rows = append(rows, row)
		if d.Day == today {
			todayTotal = d.Total
		}
	}
	s := current(r)
	c.render(w, "costs.html", map[string]any{"User": s.user, "CSRF": s.csrf, "Factory": true,
		"From": ledger.From, "To": ledger.To, "Kinds": ledgerKinds, "Days": rows, "Tasks": ledger.Tasks,
		"Today": todayTotal, "DayCap": v.Document.Budget.DayEUR, "PolicyID": v.ID})
}

// policy shows the enforced version; edits happen in Git.
func (c *Console) policy(w http.ResponseWriter, r *http.Request) {
	v, ok, err := c.cfg.Policies.CurrentPolicy(r.Context())
	if err != nil {
		log.Printf("console: current policy: %v", err)
		http.Error(w, "cannot read the policy", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "no policy version loaded", http.StatusServiceUnavailable)
		return
	}
	doc, err := json.MarshalIndent(v.Document, "", "  ")
	if err != nil {
		http.Error(w, "cannot render the policy", http.StatusInternalServerError)
		return
	}
	type project struct {
		Name string
		policy.ProjectConfig
	}
	var projects []project
	for _, name := range slices.Sorted(maps.Keys(v.Projects)) {
		projects = append(projects, project{name, v.Projects[name]})
	}
	s := current(r)
	c.render(w, "policy.html", map[string]any{"User": s.user, "CSRF": s.csrf, "Factory": true, "Version": v, "Projects": projects,
		"LoadedAt":    v.LoadedAt.UTC().Format(time.RFC3339),
		"ActivatedAt": v.ActivatedAt.UTC().Format(time.RFC3339), "Document": string(doc), "Dir": c.cfg.ConfigDir,
		"At":   c.configTree(v.Commit),
		"Main": c.cfg.ConfigRepo + "/src/branch/main/" + c.cfg.ConfigDir})
}
