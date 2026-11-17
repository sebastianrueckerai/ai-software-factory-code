package factoryd

import "context"

// FaceStop is the tests' door into the human face of a stop: the case opens as
// OpenTrip opens it, open until its face is written, or for a restart the
// card's newest live ask takes the ask, and no rung runs. The face is
// production's (Face): the ask row, the status, the case's wake.
func (r *Runner) FaceStop(ctx context.Context, taskID, reason string, choices []string) (int64, error) {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return 0, err
	}
	var c Case
	if reason == HealedReason {
		c, err = r.store.NewestAskCase(ctx, t.ID)
	} else {
		var f CaseFacts
		if f, err = r.tripFacts(ctx, t, "", ""); err == nil {
			if c, _, err = r.tripCase(ctx, t, f, TripInput{TaskID: taskID, Reason: reason, Choices: choices}); err == nil {
				c, _, err = r.store.OpenCase(ctx, c)
			}
		}
	}
	if err != nil {
		return 0, err
	}
	out, err := r.Face(ctx, t, c, reason, replyNext(reason), choices, nil)
	return out.Comment, err
}
