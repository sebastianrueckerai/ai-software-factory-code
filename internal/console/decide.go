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
	// ApprovalID names an approval request: the form's send goes to the
	// approve endpoint with the chosen action, 0 on a case's decide, whose
	// send resolves the case.
	ApprovalID   int64
	CSRF, Suffix string
	// Plan is the plan ask the slot renders: the form posts to
	// /tasks/{id}/plan/answer with the slot's revision and index, one
	// decision at a time — the confirm/revise ask or the answer ask, never
	// both. nil on a case face.
	Plan           *planSlot
	Action, DataTo string // the form posts to Action, with data-to DataTo
	Filter         boardFilter
	AnswersPending int
	IssueNumber    int64
	IssueURL       string
	Waiting        string
}

// newDecide is the decide frame every face builds on: the card it answers
// on, its csrf and suffix, the board's filter and the wait, and the form
// a case's decide sends, which an approval's own builder retargets.
func newDecide(taskID, csrf, suffix string, filter boardFilter, waiting string) *decide {
	return &decide{TaskID: taskID, CSRF: csrf, Suffix: suffix, Filter: filter, Waiting: waiting,
		Action: "/tasks/" + taskID + "/resolve", DataTo: "running"}
}

// PlanDecide is the plan ask's decide view-model: the shared case-decide
// partial renders it as the slot's one decide block, at the need's own
// suffix, nil on a need that shows no plan slot.
func (n need) PlanDecide() *decide {
	if n.Case == nil || n.Case.Plan == nil {
		return nil
	}
	f := n.Case.Face
	d := newDecide(n.Card.ID, n.CSRF, n.Suffix, n.Card.Filter, n.Waiting())
	d.Ask, d.Money, d.Why, d.Info, d.Recommendation, d.Source = f.Ask, f.Money, f.Why, f.Info, f.Recommendation, f.Source
	d.Plan = n.Case.Plan
	return d
}

// Decide is the need's decide view-model: the partial case-decide renders
// from, at the need's own suffix, nil on a need no face of its own decides.
func (n need) Decide() *decide {
	if n.Case == nil {
		return n.escalationDecide()
	}
	f := n.Case.Face
	d := newDecide(n.Card.ID, n.CSRF, n.Suffix, n.Card.Filter, n.Waiting())
	d.Ask, d.Money, d.Why, d.Info, d.Recommendation, d.Source = f.Ask, f.Money, f.Why, f.Info, f.Recommendation, f.Source
	d.Premade, d.Offers, d.Refusals = n.Case.Premade, n.Case.Offers, n.Case.Refusals
	d.CaseID = n.Case.Case.ID
	d.AnswersPending, d.IssueNumber, d.IssueURL = n.Card.AnswersPending, n.Card.IssueNumber, n.Card.IssueURL
	return d
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
	d := newDecide(k.ID, n.CSRF, n.Suffix, k.Filter, n.Waiting())
	d.AnswersPending, d.IssueNumber, d.IssueURL = k.AnswersPending, k.IssueNumber, k.IssueURL
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
