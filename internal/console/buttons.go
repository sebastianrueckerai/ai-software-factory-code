package console

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// retryCard files a failed card again (doRetry).
func (c *Console) retryCard(w http.ResponseWriter, r *http.Request) {
	c.press(w, r, c.doRetry)
}

// closeCard is the face's Close and Close with note (doClose, on the form's note).
func (c *Console) closeCard(w http.ResponseWriter, r *http.Request) {
	c.press(w, r, func(ctx context.Context, a actor, id string) (string, error) {
		return c.doClose(ctx, a, id, r.PostFormValue("note"))
	})
}

// replanCard is the drawer's Replan (doReplan, on the form's note and its
// off_main box).
func (c *Console) replanCard(w http.ResponseWriter, r *http.Request) {
	c.press(w, r, func(ctx context.Context, a actor, id string) (string, error) {
		return c.doReplan(ctx, a, id, r.PostFormValue("note"), r.PostFormValue("off_main") == "on")
	})
}

// planAnswer posts a writer's answer to a question of the plan
// (doAnswerPlanQuestion): the words are the free text, else the chosen
// answer, which the form names by its place among the question's choices,
// with the note after them.
func (c *Console) planAnswer(w http.ResponseWriter, r *http.Request) {
	revision, _ := strconv.Atoi(r.PostFormValue("revision"))
	idx, _ := strconv.Atoi(r.PostFormValue("index"))
	main := strings.TrimSpace(r.PostFormValue("own"))
	if pick, _ := strconv.Atoi(r.PostFormValue("choice")); main == "" && pick != 0 {
		var err error
		if main, err = c.planChoiceOf(r.Context(), button(r), r.PathValue("id"), revision, idx, pick); err != nil {
			c.pressed(w, r, err)
			return
		}
	}
	main = cmp.Or(main, strings.TrimSpace(r.PostFormValue("answer")))
	note := strings.TrimSpace(r.PostFormValue("note"))
	if main == "" && note != "" && r.PostFormValue("decision") == "" {
		c.pressed(w, r, refuse(http.StatusBadRequest, "A note is not an answer: pick one of the choices or write your own answer, and the note goes with it."))
		return
	}
	words := main
	if note != "" {
		words += " — " + note
	}
	_, err := c.doAnswerPlanQuestion(r.Context(), button(r), r.PathValue("id"), revision, idx, r.PostFormValue("decision"), strings.TrimSpace(words))
	c.pressed(w, r, err)
}

// planChoiceOf is choice pick (from 1) of question idx of revision revision
// of plan id, as the planner stored it.
func (c *Console) planChoiceOf(ctx context.Context, a actor, id string, revision, idx, pick int) (string, error) {
	t, err := c.planIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	qs, err := c.cfg.Tasks.PlanQuestions(ctx, t.ID, revision)
	if err != nil {
		return "", err
	}
	if idx < 1 || idx > len(qs) {
		return "", refuse(http.StatusConflict, fmt.Sprintf("Plan #%d revision %d has no question %d.", t.IssueNumber, revision, idx))
	}
	if n := len(qs[idx-1].Choices); pick < 1 || pick > n {
		return "", refuse(http.StatusBadRequest, fmt.Sprintf("Question %d of plan #%d has %d choices; there is no choice %d.", idx, t.IssueNumber, n, pick))
	}
	return qs[idx-1].Choices[pick-1], nil
}

// planHoldout posts a writer's okay or drop of one hold-out of the plan
// (doDecideHoldout).
func (c *Console) planHoldout(w http.ResponseWriter, r *http.Request) {
	revision, _ := strconv.Atoi(r.PostFormValue("revision"))
	idx, _ := strconv.Atoi(r.PostFormValue("index"))
	c.pressed(w, r, c.doDecideHoldout(r.Context(), button(r), r.PathValue("id"), revision, idx, r.PostFormValue("verb")))
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
