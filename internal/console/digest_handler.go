package console

import (
	"log"
	"net/http"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// cardLife answers GET /tasks/{id}/digest to a writer of the card's
// project: the drawer's fold of the card's life, its digest and then the
// steps of its open cases, oldest first. The fold asks for it once it
// shows, so neither the board nor the drawer reads the digest.
func (c *Console) cardLife(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	d, err := c.cfg.CardDigest(ctx, id)
	if err != nil {
		log.Printf("console: the card digest of %s: %v", id, err)
		http.Error(w, "cannot read the card's life", http.StatusInternalServerError)
		return
	}
	cases, err := c.cfg.OpenCasesOf(ctx, id)
	if err != nil {
		log.Printf("console: the open cases of %s: %v", id, err)
		http.Error(w, "cannot read the card's cases", http.StatusInternalServerError)
		return
	}
	var steps []factoryd.CaseStep
	for _, k := range cases {
		s, err := c.cfg.CaseSteps(ctx, k.ID)
		if err != nil {
			log.Printf("console: the steps of case %d: %v", k.ID, err)
			http.Error(w, "cannot read the card's cases", http.StatusInternalServerError)
			return
		}
		steps = append(steps, s...)
	}
	c.render(w, "card-life", map[string]any{"Life": d.Render(), "Steps": steps})
}
