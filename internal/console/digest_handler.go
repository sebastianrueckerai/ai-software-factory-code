package console

import (
	"log"
	"net/http"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// cardLife answers GET /tasks/{id}/digest to a writer of the card's
// project: the drawer's fold of the card's life, its digest and then each
// open case under its kind with its steps, oldest first. The fold asks for
// it the first time the human opens it, so neither the board's render nor
// the drawer's reads the digest.
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
	type openCase struct {
		Kind  string
		Steps []factoryd.CaseStep
	}
	var open []openCase
	for _, k := range cases {
		steps, err := c.cfg.CaseSteps(ctx, k.ID)
		if err != nil {
			log.Printf("console: the steps of case %d: %v", k.ID, err)
			http.Error(w, "cannot read the card's cases", http.StatusInternalServerError)
			return
		}
		open = append(open, openCase{Kind: string(k.Kind), Steps: steps})
	}
	c.render(w, "card-life", map[string]any{"Life": d.Render(), "Cases": open})
}
