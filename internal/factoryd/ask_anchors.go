package factoryd

import (
	"cmp"
	"context"
	"slices"
	"time"
)

// AskAnchor is one face of an ask case, a comment on the card's issue that
// the human's later comments answer, by the comments' ids. Each face of a case
// is an anchor, so a comment after the first face still answers the ask. It
// carries what the readers need of the ask: the question it put (the first
// line of its reason, as the live run tags the answers), its five parts, the stop's words, the answers the agent offered, when the face was
// put, and Withdrawn, the comment of the case's withdrawal, 0 while the ask
// stands.
type AskAnchor struct {
	CaseID    int64
	Comment   int64
	Question  string
	Shaped    *HumanQuestion // the five-part question of the ask row, nil on an ask no template shaped
	Body      string
	Choices   []string
	Asked     time.Time
	Withdrawn int64
}

// askAnchorsOf is the one function that builds the anchors of a card from
// its record: its cases, the face steps (a rung-3 step the factory took,
// carrying its comment), each case's newest question_asked row and its
// newest withdrawal with a comment. Cases an answer on the card does not
// end (ownAnswerKinds) take no anchor. Anchors come oldest comment first.
func askAnchorsOf(cases []Case, faces []CaseStep, asks map[int64]CaseAsk, withdrawals map[int64]int64) []AskAnchor {
	var out []AskAnchor
	for _, c := range cases {
		if slices.Contains(ownAnswerKinds, c.Kind) {
			continue
		}
		ask := asks[c.ID]
		for _, f := range faces {
			if f.CaseID != c.ID {
				continue
			}
			body := askBody(c)
			out = append(out, AskAnchor{CaseID: c.ID, Comment: f.CommentID, Question: escalationQuestion(body), Body: body,
				Shaped: questionOfAsk(ask.Detail), Choices: askChoices(c, ask.Detail), Asked: f.StartedAt, Withdrawn: withdrawals[c.ID]})
		}
	}
	slices.SortStableFunc(out, func(a, b AskAnchor) int { return cmp.Or(cmp.Compare(a.Comment, b.Comment), a.Asked.Compare(b.Asked)) })
	return out
}

// caseAskOf is the newest question_asked row of a case as the readers take it.
func caseAskOf(e Event) CaseAsk { return CaseAsk{At: e.At, Detail: e.Detail} }

// withdrawalComment is the comment a question_withdrawn row left on the
// issue, 0 for none.
func withdrawalComment(e Event) int64 {
	id, _ := toInt(e.Detail["withdrawal"])
	return int64(id)
}

// askBody is the stop's words a case's face asked: the face, else the
// reason, else the case's problem (faceOf).
func askBody(c Case) string {
	reason, _ := faceOf(c)
	return reason
}

// askChoices are the answers the agent offered with the stop: the case's
// own facts, else the labels its newest ask row carries.
func askChoices(c Case, ask map[string]any) []string {
	if _, choices := faceOf(c); len(choices) > 0 {
		return choices
	}
	switch raw := ask["choices"].(type) {
	case []string:
		return raw
	case []any:
		var out []string
		for _, x := range raw {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// AskAnchors are card taskID's ask anchors (askAnchorsOf), read from its
// record.
func (s *PGStore) AskAnchors(ctx context.Context, taskID string) ([]AskAnchor, error) {
	cases, err := s.CasesOf(ctx, taskID)
	if err != nil {
		return nil, err
	}
	faces, err := s.caseSteps(ctx, `SELECT `+caseStepColumns+` FROM case_steps WHERE rung = 3 AND actor = $2
		AND case_id IN (SELECT id FROM cases WHERE task_id = $1) ORDER BY id`, taskID, FactoryDecider)
	if err != nil {
		return nil, err
	}
	newest := func(kind, where string) (map[int64]Event, error) {
		evs, err := s.events(ctx, `SELECT DISTINCT ON (detail->>'case_id') `+eventColumns+` FROM events
			WHERE task_id = $1 AND kind = $2 AND detail->>'case_id' IS NOT NULL `+where+` ORDER BY detail->>'case_id', id DESC`, taskID, kind)
		if err != nil {
			return nil, err
		}
		return byCase(evs), nil
	}
	asks, err := newest(EventQuestionAsked, "")
	if err != nil {
		return nil, err
	}
	withdrawn, err := newest(EventQuestionWithdrawn, "AND detail->>'withdrawal' IS NOT NULL")
	if err != nil {
		return nil, err
	}
	return askAnchorsOf(cases, faces, asksOf(asks), withdrawalsOf(withdrawn)), nil
}

// byCase keys events by the case they name.
func byCase(evs []Event) map[int64]Event {
	out := map[int64]Event{}
	for _, e := range evs {
		id, _ := toInt(e.Detail["case_id"])
		if id != 0 {
			out[int64(id)] = e
		}
	}
	return out
}

func asksOf(evs map[int64]Event) map[int64]CaseAsk {
	out := map[int64]CaseAsk{}
	for id, e := range evs {
		out[id] = caseAskOf(e)
	}
	return out
}

func withdrawalsOf(evs map[int64]Event) map[int64]int64 {
	out := map[int64]int64{}
	for id, e := range evs {
		out[id] = withdrawalComment(e)
	}
	return out
}
