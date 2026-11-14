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
	// Plan is the plan ask the slot renders: the form posts to
	// /tasks/{id}/plan/answer with the slot's revision and index, one
	// decision at a time — the confirm/revise ask or the answer ask, never
	// both. nil on a case face.
	Plan *planSlot
}

// PlanDecide is the plan ask's decide view-model: the shared case-decide
// partial renders it as the slot's one decide block, at the need's own
// suffix, nil on a need that shows no plan slot.
func (n need) PlanDecide() *decide {
	if n.Case == nil || n.Case.Plan == nil {
		return nil
	}
	f := n.Case.Face
	return &decide{
		Ask: f.Ask, Money: f.Money, Why: f.Why, Info: f.Info, Recommendation: f.Recommendation, Source: f.Source,
		TaskID: n.Card.ID, CSRF: n.CSRF, Suffix: n.Suffix, Filter: n.Card.Filter,
		AnswersPending: n.Card.AnswersPending, IssueNumber: n.Card.IssueNumber, IssueURL: n.Card.IssueURL,
		Waiting: n.Waiting(), Plan: n.Case.Plan,
	}
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
