package console

import (
	"context"
	"log"
	"net/http"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// cardLife answers GET /tasks/{id}/digest to whoever may read the card's
// project's board, 404 to anyone else: the fold of the card's life, its digest and then each
// open case under its kind with its steps, oldest first. The fold asks for
// it the first time the human opens it, so neither the board's render nor
// the drawer's reads the digest.
func (c *Console) cardLife(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	if !c.mayReadCard(ctx, r, id) {
		http.NotFound(w, r)
		return
	}
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

// mayReadCard says whether the viewer may read card id's project's board:
// a writer of it, a factory writer, or a reader of its repository.
func (c *Console) mayReadCard(ctx context.Context, r *http.Request, id string) bool {
	t, ok, err := c.cfg.Tasks.Task(ctx, id)
	if err != nil || !ok {
		return false
	}
	projects, err := c.coderProjects(ctx)
	if err != nil {
		return false
	}
	for _, p := range projects {
		if p.Repository != t.Repository {
			continue
		}
		if c.factoryWriter(r) {
			return true
		}
		reads, err := c.reads(ctx, current(r), p)
		return err == nil && reads
	}
	return false
}
