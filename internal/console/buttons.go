package console

import (
	"context"
	"net/http"
	"strconv"
)

// retryCard files a failed card of a plan again (doRetry).
func (c *Console) retryCard(w http.ResponseWriter, r *http.Request) {
	c.showNote(w, r, c.doRetry)
}

// closeCard is the face's Close and Close with note (doClose, on the form's note).
func (c *Console) closeCard(w http.ResponseWriter, r *http.Request) {
	c.showNote(w, r, func(ctx context.Context, a actor, id string) (string, error) {
		return c.doClose(ctx, a, id, r.PostFormValue("note"))
	})
}

// acceptanceCloseCard is the face's Close: acceptance green
// (doCloseAcceptance).
func (c *Console) acceptanceCloseCard(w http.ResponseWriter, r *http.Request) {
	c.showNote(w, r, c.doCloseAcceptance)
}

// planAnswer posts a writer's answer to a question of the plan
// (doAnswerPlanQuestion).
func (c *Console) planAnswer(w http.ResponseWriter, r *http.Request) {
	revision, _ := strconv.Atoi(r.PostFormValue("revision"))
	idx, _ := strconv.Atoi(r.PostFormValue("index"))
	note, err := c.doAnswerPlanQuestion(r.Context(), button(r), r.PathValue("id"), revision, idx, r.PostFormValue("answer"))
	if err != nil {
		c.fail(w, r, err)
		return
	}
	c.showBoard(w, r, http.StatusOK, note, nil)
}

// planHoldout posts a writer's okay or drop of one hold-out of the plan
// (doDecideHoldout).
func (c *Console) planHoldout(w http.ResponseWriter, r *http.Request) {
	revision, _ := strconv.Atoi(r.PostFormValue("revision"))
	idx, _ := strconv.Atoi(r.PostFormValue("index"))
	note, err := c.doDecideHoldout(r.Context(), button(r), r.PathValue("id"), revision, idx, r.PostFormValue("verb"))
	if err != nil {
		c.fail(w, r, err)
		return
	}
	c.showBoard(w, r, http.StatusOK, note, nil)
}

// planChanges posts a writer's change request on the plan
// (doRequestChanges).
func (c *Console) planChanges(w http.ResponseWriter, r *http.Request) {
	c.showNote(w, r, func(ctx context.Context, a actor, id string) (string, error) {
		return c.doRequestChanges(ctx, a, id, r.PostFormValue("text"))
	})
}

// showNote runs a button's action on the card the path names and shows
// the board with its note.
func (c *Console) showNote(w http.ResponseWriter, r *http.Request, action func(ctx context.Context, a actor, id string) (string, error)) {
	note, err := action(r.Context(), button(r), r.PathValue("id"))
	if err != nil {
		c.fail(w, r, err)
		return
	}
	c.showBoard(w, r, http.StatusOK, note, nil)
}
