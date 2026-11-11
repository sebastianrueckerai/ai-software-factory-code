package factoryd

import "context"

// ActivityEscalate names the tests' own door into the human face of a stop:
// the case opens as OpenTrip opens it, open until its face is written, or
// for a restart the card's newest live ask takes the ask, and no rung runs.
const ActivityEscalate = "Escalate"

func (r *Runner) escalateDouble(ctx context.Context, taskID, reason string, choices []string) (int64, error) {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return 0, err
	}
	var c Case
	if reason == healedReason {
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
	id, err := r.askHuman(ctx, t, c, reason, replyNext(reason), choices, nil)
	if err != nil || c.ID == 0 {
		return id, err
	}
	return id, r.store.SetCaseStatus(ctx, c.ID, CaseAtHuman)
}

// teeEvents records every event in rows and in the store, as the real events
// table is both the recorder and the case's source.
type teeEvents struct {
	*fakeEventRecorder
	store *fakeStore
}

func (t teeEvents) Record(ctx context.Context, e Event) (int64, error) {
	if _, err := t.store.Record(ctx, e); err != nil {
		return 0, err
	}
	return t.fakeEventRecorder.Record(ctx, e)
}

// askedRows are the asks recorded for card id, oldest first, as their
// question_asked rows hold them: the stop's words (its reason, else the ask),
// the five-part question and the choices' labels, in the shape the ask's
// escalation row had.
func (s *fakeStore) askedRows(id string) []IntakeEvent {
	var out []IntakeEvent
	for _, e := range s.events {
		if e.TaskID != id || e.Kind != EventQuestionAsked {
			continue
		}
		comment, _ := toInt(e.Detail["comment"])
		body, _ := e.Detail["reason"].(string)
		if body == "" {
			body, _ = e.Detail["ask"].(string)
		}
		out = append(out, IntakeEvent{TaskID: id, Kind: IntakeEscalation, Author: "factoryd", Body: body, CommentID: int64(comment),
			Question: questionOfAsk(e.Detail), Choices: caseChoices(Case{}, e.Detail)})
	}
	return out
}
