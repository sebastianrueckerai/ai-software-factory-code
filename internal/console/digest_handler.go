package console

import (
	"context"
	"log"
	"net/http"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// cardLife answers GET /tasks/{id}/digest to whoever may read the card's
// project's board, 404 to anyone else: the fold of the card's life, its
// digest as a human reads it (CardDigest.Life), then each open case under
// its ask with who weighed it and what came of it, oldest first. The fold asks for
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
		http.Error(w, "The card's life could not be read. Open the fold again.", http.StatusInternalServerError)
		return
	}
	cases, err := c.cfg.OpenCasesOf(ctx, id)
	if err != nil {
		log.Printf("console: the open cases of %s: %v", id, err)
		http.Error(w, "The card's open asks could not be read. Open the fold again.", http.StatusInternalServerError)
		return
	}
	t, _, err := c.cfg.Tasks.Task(ctx, id)
	if err != nil {
		log.Printf("console: the card %s: %v", id, err)
		http.Error(w, "The card could not be read. Open the fold again.", http.StatusInternalServerError)
		return
	}
	type step struct{ Lead, Found string }
	type openAsk struct {
		Ask   string
		Steps []step
	}
	var open []openAsk
	for _, k := range cases {
		steps, err := c.cfg.CaseSteps(ctx, k.ID)
		if err != nil {
			log.Printf("console: the steps of case %d: %v", k.ID, err)
			http.Error(w, "The card's open asks could not be read. Open the fold again.", http.StatusInternalServerError)
			return
		}
		a := openAsk{Ask: factoryd.FaceOf(k, factoryd.Facts{CaseFacts: factoryd.FactsOf(k), Card: t, Case: k}).Ask}
		for _, s := range steps {
			lead, found := factoryd.StepWords(s)
			a.Steps = append(a.Steps, step{lead, found})
		}
		open = append(open, a)
	}
	c.render(w, "card-life", map[string]any{"Life": d.Life(), "Asks": open})
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
