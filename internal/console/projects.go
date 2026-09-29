package console

import (
	"log"
	"net/http"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

type projectRow struct {
	factoryd.Project
	// NotifyList is who gets pinged, NotifyDefault when that is the
	// policy's approvers.
	NotifyList    []string
	NotifyDefault bool
	Envelope      envelopeView
	GatewayYAML   string
	// File links projects/<name>.yaml at the version in force.
	File string
}

// projects shows every project read-only (ruling 183): its file's
// settings with a link to the file, where every change is made, its
// registration's state and its envelope's use.
func (c *Console) projects(w http.ResponseWriter, r *http.Request) {
	ps, err := c.cfg.Projects.Projects(r.Context())
	if err != nil {
		log.Printf("console: projects: %v", err)
		http.Error(w, "cannot read the projects", http.StatusInternalServerError)
		return
	}
	v, ok, err := c.cfg.Policies.CurrentPolicy(r.Context())
	if err != nil || !ok {
		log.Printf("console: current policy: loaded %v, %v", ok, err)
		http.Error(w, "cannot read the policy", http.StatusInternalServerError)
		return
	}
	var rows []projectRow
	for _, p := range ps {
		row := projectRow{Project: p, NotifyList: p.NotifyOrApprovers(v.Document.Approvers), NotifyDefault: len(p.Notify) == 0,
			GatewayYAML: factoryd.ListenerYAML(p)}
		if p.File {
			row.File = c.fileLink(v, p.Name)
			if row.Envelope, err = c.envelopeOf(r.Context(), v, p.Repository); err != nil {
				log.Printf("console: envelope use of %s: %v", p.Repository, err)
				http.Error(w, "cannot read the envelopes' use", http.StatusInternalServerError)
				return
			}
		}
		rows = append(rows, row)
	}
	s := current(r)
	c.render(w, "projects.html", map[string]any{"User": s.user, "CSRF": s.csrf, "Projects": rows,
		"New": c.cfg.ConfigRepo + "/src/branch/main/" + c.cfg.ConfigDir + "/projects"})
}
