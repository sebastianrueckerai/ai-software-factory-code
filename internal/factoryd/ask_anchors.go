package factoryd

import (
	"context"
	"slices"
)

// askSource is what the escalation events of a card are read from: its cases
// and the question_asked and question_withdrawn rows that name them.
type askSource interface {
	CasesOf(ctx context.Context, taskID string) ([]Case, error)
	TaskEventsOfKind(ctx context.Context, id, kind string, limit int) ([]Event, error)
}

// askRowLimit bounds the rows of one kind a card's anchors read.
const askRowLimit = 1000

// askAnchors are the escalation events of card taskID read from its ask
// cases: one IntakeEscalation per case that was faced, carrying the face's
// comment, the stop's words, the agent's choices and the five-part question
// of the case's newest ask row, and one IntakeEscalationWithdrawn per case
// whose face was withdrawn with a comment of its own. The readers that anchor
// a human's comments (Reconcile's answers, the cap wait, the screening wait)
// read them as they read intake rows, which no code writes any more.
func askAnchors(ctx context.Context, src askSource, taskID string) ([]IntakeEvent, error) {
	cases, err := src.CasesOf(ctx, taskID)
	if err != nil {
		return nil, err
	}
	asks, err := src.TaskEventsOfKind(ctx, taskID, EventQuestionAsked, askRowLimit)
	if err != nil {
		return nil, err
	}
	withdrawals, err := src.TaskEventsOfKind(ctx, taskID, EventQuestionWithdrawn, askRowLimit)
	if err != nil {
		return nil, err
	}
	newest := func(rows []Event, c Case) (Event, bool) {
		i := slices.IndexFunc(rows, func(e Event) bool {
			id, _ := toInt(e.Detail["case_id"])
			return int64(id) == c.ID
		})
		if i < 0 {
			return Event{}, false
		}
		return rows[i], true
	}
	var out []IntakeEvent
	for _, c := range cases {
		if slices.Contains(ownAnswerKinds, c.Kind) {
			continue
		}
		ask, faced := newest(asks, c)
		if !faced {
			continue
		}
		comment := c.CommentID
		if n, _ := toInt(ask.Detail["comment"]); comment == 0 {
			comment = int64(n)
		}
		e := IntakeEvent{TaskID: taskID, Kind: IntakeEscalation, Author: "factoryd", Body: caseBody(c), CommentID: comment, CreatedAt: c.OpenedAt,
			Question: questionOfAsk(ask.Detail), Choices: caseChoices(c, ask.Detail)}
		out = append(out, e)
		if w, ok := newest(withdrawals, c); ok {
			if id, _ := toInt(w.Detail["withdrawal"]); id != 0 {
				why, _ := w.Detail["reason"].(string)
				out = append(out, IntakeEvent{TaskID: taskID, Kind: IntakeEscalationWithdrawn, Author: FactoryDecider, Body: why, CommentID: int64(id), Withdraws: comment, CreatedAt: w.At})
			}
		}
	}
	return out, nil
}

// caseBody is the stop's words a case's face asked: the whole reason, else
// its first 300 characters, else the case's problem.
func caseBody(c Case) string {
	for _, k := range []string{"face", "reason"} {
		if s, _ := c.Facts[k].(string); s != "" {
			return s
		}
	}
	return c.Problem
}

// caseChoices are the answers the agent offered with the stop: the case's own
// facts, else the labels its newest ask row carries.
func caseChoices(c Case, ask map[string]any) []string {
	for _, raw := range []any{c.Facts["choices"], ask["choices"]} {
		switch v := raw.(type) {
		case []string:
			return v
		case []any:
			var out []string
			for _, x := range v {
				if s, ok := x.(string); ok {
					out = append(out, s)
				}
			}
			return out
		}
	}
	return nil
}

// mergeIntake puts derived events among events, oldest first.
func mergeIntake(events, derived []IntakeEvent) []IntakeEvent {
	if len(derived) == 0 {
		return events
	}
	all := slices.Concat(events, derived)
	slices.SortStableFunc(all, func(a, b IntakeEvent) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return all
}
