package console

import (
	"cmp"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// decide is the view-model the case-decide partial renders from: the parts
// of the ask it decides — the ask with its money line, why, the story, the
// agent's view, the recommendation with its source — and the answers it
// offers, with the form that sends them. Every face that asks includes the
// partial with one of these, so the partial reads one value whatever face
// includes it.
type decide struct {
	Ask, Money, Why, Info, Recommendation, Source string
	Story                                         factoryd.Story
	Offers                                        []offer
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
// from, at the need's own suffix, nil on a need that holds no case.
func (n need) Decide() *decide {
	if n.Case == nil {
		return nil
	}
	f := n.Case.Face
	if ap := n.Case.Approval; ap != nil && ap.Hosted && ap.Decide != nil {
		// the case's parts show the approval's own ask with its numbers
		// and the why of the ask, keeping what the case itself wrote: its
		// brief as the info, its recommendation with its writer
		f.Ask, f.Why = ap.Decide.Ask, ap.Decide.Why
		f.Money, f.Info = cmp.Or(f.Money, ap.Decide.Money), cmp.Or(f.Info, ap.Decide.Info)
		if f.Recommendation == "" {
			f.Recommendation, f.Source = ap.Decide.Recommendation, ap.Decide.Source
		}
	}
	d := newDecide(n.Card.ID, n.CSRF, n.Suffix, n.Card.Filter, n.Waiting())
	d.Ask, d.Money, d.Why, d.Info, d.Recommendation, d.Source = f.Ask, f.Money, f.Why, f.Info, f.Recommendation, f.Source
	d.Story = f.Story
	d.Offers = n.Case.Offers
	d.CaseID = n.Case.Case.ID
	d.AnswersPending, d.IssueNumber, d.IssueURL = n.Card.AnswersPending, n.Card.IssueNumber, n.Card.IssueURL
	return d
}
