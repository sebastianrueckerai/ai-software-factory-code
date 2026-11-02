package console

import (
	"context"
	"net/http"
	"strconv"
)

// retryCard files a failed card of a plan again (doRetry).
func (c *Console) retryCard(w http.ResponseWriter, r *http.Request) {
	c.press(w, r, c.doRetry)
}

// closeCard is the face's Close and Close with note (doClose, on the form's note).
func (c *Console) closeCard(w http.ResponseWriter, r *http.Request) {
	c.press(w, r, func(ctx context.Context, a actor, id string) (string, error) {
		return c.doClose(ctx, a, id, r.PostFormValue("note"))
	})
}

// acceptanceCloseCard is the face's Close: acceptance green
// (doCloseAcceptance).
func (c *Console) acceptanceCloseCard(w http.ResponseWriter, r *http.Request) {
	c.press(w, r, c.doCloseAcceptance)
}

// replanCard is the drawer's Replan (doReplan, on the form's note and its
// off_main box).
func (c *Console) replanCard(w http.ResponseWriter, r *http.Request) {
	c.press(w, r, func(ctx context.Context, a actor, id string) (string, error) {
		return c.doReplan(ctx, a, id, r.PostFormValue("note"), r.PostFormValue("off_main") == "on")
	})
}

// planAnswer posts a writer's answer to a question of the plan
// (doAnswerPlanQuestion).
func (c *Console) planAnswer(w http.ResponseWriter, r *http.Request) {
	revision, _ := strconv.Atoi(r.PostFormValue("revision"))
	idx, _ := strconv.Atoi(r.PostFormValue("index"))
	_, err := c.doAnswerPlanQuestion(r.Context(), button(r), r.PathValue("id"), revision, idx, r.PostFormValue("decision"), r.PostFormValue("answer"))
	c.pressed(w, r, err)
}

// planHoldout posts a writer's okay or drop of one hold-out of the plan
// (doDecideHoldout).
func (c *Console) planHoldout(w http.ResponseWriter, r *http.Request) {
	revision, _ := strconv.Atoi(r.PostFormValue("revision"))
	idx, _ := strconv.Atoi(r.PostFormValue("index"))
	_, err := c.doDecideHoldout(r.Context(), button(r), r.PathValue("id"), revision, idx, r.PostFormValue("verb"))
	c.pressed(w, r, err)
}

// planChanges posts a writer's change request on the plan
// (doRequestChanges).
func (c *Console) planChanges(w http.ResponseWriter, r *http.Request) {
	c.press(w, r, func(ctx context.Context, a actor, id string) (string, error) {
		return c.doRequestChanges(ctx, a, id, r.PostFormValue("text"))
	})
}

// press runs a button's action on the card the path names and answers
// the press.
func (c *Console) press(w http.ResponseWriter, r *http.Request, action func(ctx context.Context, a actor, id string) (string, error)) {
	_, err := action(r.Context(), button(r), r.PathValue("id"))
	c.pressed(w, r, err)
}

// deployNow is the panel's Deploy now (doDeployNow).
func (c *Console) deployNow(w http.ResponseWriter, r *http.Request) {
	_, err := c.doDeployNow(r.Context(), button(r), r.PathValue("name"))
	c.pressed(w, r, err)
}
