package factoryd

import (
	"context"
	"encoding/json"
	"slices"
	"time"
)

// askAnchors are the escalation events of card taskID read from its ask
// cases: one IntakeEscalation per case that was faced, carrying the face's
// comment, the stop's words, the agent's choices and the five-part question
// the case's newest ask row holds, and one IntakeEscalationWithdrawn per
// case whose face was withdrawn with a comment. Readers that anchor the
// human's comments (Reconcile's answers, the cap wait, the screening wait)
// read them as they read the intake rows, which no code writes any more.
func (s *PGStore) askAnchors(ctx context.Context, taskID string) ([]IntakeEvent, error) {
	type row struct {
		id, comment        int64
		opened             time.Time
		body               string
		choices, ask, drew []byte
		drewAt             *time.Time
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.comment_id, c.opened_at,
		COALESCE(NULLIF(c.facts->>'face', ''), c.facts->>'reason', c.problem), c.facts->'choices',
		a.detail, w.detail, w.at
		FROM cases c
		JOIN LATERAL (SELECT ev.detail FROM events ev WHERE ev.task_id = c.task_id AND ev.kind = $2
		    AND ev.detail->>'case_id' = c.id::text ORDER BY ev.id DESC LIMIT 1) a ON true
		LEFT JOIN LATERAL (SELECT ev.detail, ev.at FROM events ev WHERE ev.task_id = c.task_id AND ev.kind = $3
		    AND ev.detail->>'case_id' = c.id::text ORDER BY ev.id DESC LIMIT 1) w ON true
		WHERE c.task_id = $1 AND c.kind NOT IN `+ownAnswerKindsSQL+` ORDER BY c.id`, taskID, EventQuestionAsked, EventQuestionWithdrawn)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IntakeEvent
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.comment, &r.opened, &r.body, &r.choices, &r.ask, &r.drew, &r.drewAt); err != nil {
			return nil, err
		}
		var asked, withdrawn map[string]any
		if err := json.Unmarshal(r.ask, &asked); err != nil {
			return nil, err
		}
		e := IntakeEvent{TaskID: taskID, Kind: IntakeEscalation, Author: "factoryd", Body: r.body, CommentID: r.comment, CreatedAt: r.opened, Question: questionOfAsk(asked)}
		if err := unmarshalChoices(r.choices, &e.Choices); err != nil {
			return nil, err
		}
		if e.Choices == nil {
			if labels, ok := asked["choices"].([]any); ok {
				for _, l := range labels {
					if s, ok := l.(string); ok {
						e.Choices = append(e.Choices, s)
					}
				}
			}
		}
		out = append(out, e)
		if len(r.drew) == 0 || json.Unmarshal(r.drew, &withdrawn) != nil {
			continue
		}
		if id, _ := toInt(withdrawn["withdrawal"]); id != 0 {
			why, _ := withdrawn["reason"].(string)
			out = append(out, IntakeEvent{TaskID: taskID, Kind: IntakeEscalationWithdrawn, Author: FactoryDecider, Body: why, CommentID: int64(id), Withdraws: r.comment, CreatedAt: *r.drewAt})
		}
	}
	return out, rows.Err()
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

