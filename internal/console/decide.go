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
	ApprovalID     int64
	CSRF, Suffix   string
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

// Decide is the need's decide view-model: the partial case-decide renders
// from, at the need's own suffix, nil on a need that holds no case.
func (n need) Decide() *decide {
	if n.Case == nil {
		return nil
	}
	f := n.Case.Face
	d := newDecide(n.Card.ID, n.CSRF, n.Suffix, n.Card.Filter, n.Waiting())
	d.Ask, d.Money, d.Why, d.Info, d.Recommendation, d.Source = f.Ask, f.Money, f.Why, f.Info, f.Recommendation, f.Source
	d.Premade, d.Offers, d.Refusals = n.Case.Premade, n.Case.Offers, n.Case.Refusals
	d.CaseID = n.Case.Case.ID
	d.AnswersPending, d.IssueNumber, d.IssueURL = n.Card.AnswersPending, n.Card.IssueNumber, n.Card.IssueURL
	return d
}
