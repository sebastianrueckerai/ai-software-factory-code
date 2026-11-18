package factoryd

import "context"

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

// askedRow is a question_asked row as the tests read it: the stop's words,
// the comment of its face, its five-part question and the agent's choices.
type askedRow struct {
	Body      string
	CommentID int64
	Question  *HumanQuestion
	Choices   []string
}

// askedRows are the asks recorded for card id, oldest first, as their
// question_asked rows hold them.
func (s *fakeStore) askedRows(id string) []askedRow {
	var out []askedRow
	for _, e := range s.events {
		if e.TaskID != id || e.Kind != EventQuestionAsked {
			continue
		}
		comment, _ := toInt(e.Detail["comment"])
		body, _ := e.Detail["reason"].(string)
		if body == "" {
			body, _ = e.Detail["ask"].(string)
		}
		out = append(out, askedRow{Body: body, CommentID: int64(comment), Question: questionOfAsk(e.Detail), Choices: askChoices(Case{}, e.Detail)})
	}
	return out
}
