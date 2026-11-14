package console

import "github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"

// decide is the view-model the case-decide partial renders from: the five
// parts of the ask it decides — the ask with its money line, why, what the
// call needs, the recommendation with its source — and the answers it
// offers, with the form that sends them. Every face that asks includes the
// partial with one of these, so the partial reads one value whatever face
// includes it.
type decide struct {
	Ask, Money, Why, Info, Recommendation, Source string
	Premade                                       []string
	Offers                                        []offer
	Refusals                                      []factoryd.ActionRefusal
	TaskID                                        string
	CaseID                                        int64
	CSRF, Suffix                                  string
	Filter                                        boardFilter
	AnswersPending                                int
	IssueNumber                                   int64
	IssueURL, Waiting                             string
}

// Decide is the need's decide view-model: the partial case-decide renders
// from, at the need's own suffix, nil on a need no face of its own decides.
func (n need) Decide() *decide {
	if n.Case == nil {
		return n.escalationDecide()
	}
	f := n.Case.Face
	return &decide{
		Ask: f.Ask, Money: f.Money, Why: f.Why, Info: f.Info, Recommendation: f.Recommendation, Source: f.Source,
		Premade: n.Case.Premade, Offers: n.Case.Offers, Refusals: n.Case.Refusals,
		TaskID: n.Card.ID, CaseID: n.Case.Case.ID, CSRF: n.CSRF, Suffix: n.Suffix, Filter: n.Card.Filter,
		AnswersPending: n.Card.AnswersPending, IssueNumber: n.Card.IssueNumber, IssueURL: n.Card.IssueURL,
		Waiting: n.Waiting(),
	}
}

// escalationDecide is the escalation face's decide view-model: the card's
// own question in the partial's five parts, its choices the premade
// answers, and the one form that sends the choice with its fund and note.
// nil on a face the shared partial cannot answer: a human card's, whose
// choices keep their own forms, and an ask without premade answers, whose
// own words alone answer on its old form.
func (n need) escalationDecide() *decide {
	k := n.Card
	if k.Kind == "human" {
		return nil
	}
	d := &decide{TaskID: k.ID, CSRF: n.CSRF, Suffix: n.Suffix, Filter: k.Filter,
		AnswersPending: k.AnswersPending, IssueNumber: k.IssueNumber, IssueURL: k.IssueURL, Waiting: n.Waiting()}
	if q := k.Question; q != nil {
		d.Ask, d.Why, d.Info, d.Recommendation, d.Source = q.Ask, q.Why, q.Info, q.Recommendation, q.Source
		for _, c := range q.Choices {
			d.Premade = append(d.Premade, c.Label)
		}
		if len(d.Premade) == 0 {
			return nil
		}
		return d
	}
	if len(k.Choices) == 0 {
		return nil
	}
	d.Ask = k.Reason
	d.Premade = k.Choices
	return d
}
