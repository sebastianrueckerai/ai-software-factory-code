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
// from, at the need's own suffix, nil on a need that holds no case.
func (n need) Decide() *decide {
	if n.Case == nil {
		return nil
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
