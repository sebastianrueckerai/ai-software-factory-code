package factoryd_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// requestStates are the states of the rows of fixture fx's request that a
// face reads, each named: a merge with and without a human promise
// waiting for its verdict and with its red checks healed; a plan with its
// confirmation alone, with an extra promise waiting, with an open question
// and answered; a criteria request with its promises unjudged, one not
// met, a human's waiting, all settled, before and after delivery, and
// every promise met by the gate; and a rerun request in each state of the
// run of its tests.
func requestStates(fx facefixtures.Fixture) map[string]factoryd.Request {
	a := *fx.Approval
	base := factoryd.Request{ApprovalRequest: a}
	out := map[string]factoryd.Request{"as filed": base}
	var plan factoryd.Plan
	if fx.Plan != nil {
		plan = *fx.Plan
	}
	rows := func(verdict func(c factoryd.PlanCriterion) string) []factoryd.PromiseRow {
		var out []factoryd.PromiseRow
		for _, c := range plan.Criteria {
			out = append(out, factoryd.PromiseRow{PlanCriterion: c, Verdict: verdict(c)})
		}
		return out
	}
	switch a.Kind {
	case factoryd.ApprovalMerge:
		if err := json.Unmarshal(a.Spec, &base.Merge); err != nil {
			panic(err)
		}
		human := factoryd.PlanCriterion{ID: "panels_read_on_a_phone", Kind: factoryd.CriterionHuman, Claim: "the panels read well on a phone",
			Promise: "The panels read well on a phone.", Evidence: "Open /metrics at 390 px wide"}
		waiting := base
		waiting.Promises, waiting.Unsettled, waiting.Integration = []factoryd.PromiseRow{{PlanCriterion: human}}, []string{human.ID}, "t-integration"
		healed := base
		healed.Healed = base.Merge.RedChecks
		out["as filed"], out["a human promise waits"], out["healed"] = base, waiting, healed
	case factoryd.ApprovalPlan:
		base.Plan, base.PlanSpec = plan, factoryd.PlanSpec{Revision: 1}
		confirm := factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation, Text: factoryd.ConfirmationQuestion(1, plan)}
		question := factoryd.PlanQuestion{Text: "How far back does the feed page by default?", Choices: []string{"One day", "The 500 newest rows"}}
		alone := base
		alone.PlanQuestions = []factoryd.PlanQuestion{confirm}
		extra := alone
		extra.HoldoutRows = []factoryd.HoldoutRow{{PlanProof: factoryd.PlanProof{Idx: 1, Round: 1, Origin: factoryd.ProofOriginHoldout,
			Claim: "The feed shows a deleted card's events.", State: factoryd.ProofProposed}, Index: 1, Decides: true}}
		asking := base
		asking.PlanQuestions = []factoryd.PlanQuestion{question, confirm}
		out["the confirmation alone"], out["an extra promise waits"], out["an open question"] = alone, extra, asking
	case factoryd.ApprovalCriteria, factoryd.ApprovalCriteriaRerun:
		var spec factoryd.CriteriaSpec
		if err := json.Unmarshal(a.Spec, &spec); err != nil {
			panic(err)
		}
		base.CriteriaSpec, base.Plan = spec, plan
		state := func(delivered bool, verdict func(c factoryd.PlanCriterion) string, run *factoryd.RunFact) factoryd.Request {
			r := base
			r.Delivered, r.Promises, r.Run = delivered, rows(verdict), run
			return r
		}
		none := func(factoryd.PlanCriterion) string { return "" }
		gate := base
		gate.Delivered = true
		for _, c := range plan.Criteria {
			c.Kind = factoryd.CriterionTest
			gate.Promises = append(gate.Promises, factoryd.PromiseRow{PlanCriterion: c, Verdict: "met", Evidence: "blind: criterion " + c.ID + ": its test passed"})
		}
		out["every promise met by the gate"] = gate
		for _, delivered := range []bool{false, true} {
			at := fmt.Sprintf(" (delivered %v)", delivered)
			out["unjudged"+at] = state(delivered, none, nil)
			out["one not met"+at] = state(delivered, func(c factoryd.PlanCriterion) string {
				if c.ID == plan.Criteria[0].ID {
					return "not_met"
				}
				return "met"
			}, nil)
			settled := state(delivered, func(factoryd.PlanCriterion) string { return "waived" }, nil)
			settled.ClosesItself = true
			out["all settled"+at] = settled
			if a.Kind == factoryd.ApprovalCriteriaRerun {
				for _, outcome := range []string{"asked", "started", "ran", "cut", "no_room"} {
					out["a run "+outcome+at] = state(delivered, none, &factoryd.RunFact{Outcome: outcome, By: "sebastian", At: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)})
				}
			}
		}
	}
	return out
}

// caseOf is fixture fx's case, the zero case for none.
func caseOf(fx facefixtures.Fixture) factoryd.Case {
	if fx.Case == nil {
		return factoryd.Case{}
	}
	return *fx.Case
}

// Every page a card can show offers 2 to 4 answers: the pages of every
// fixture's case, as its card's own, on a human card that waits for
// another card, and as a helper's under its parent's card, and of every
// fixture's request in each state of its rows, on the card's case and
// with no case to hold it. A page no case holds offers no Not now that
// keeps a note, for nothing keeps one there.
func TestEveryPageOffersTwoToFourAnswers(t *testing.T) {
	type holder struct {
		name  string
		card  factoryd.BoardTask
		cases []factoryd.Case
	}
	for _, fx := range facefixtures.All() {
		if fx.Case == nil && fx.Approval == nil {
			continue
		}
		var holders []holder
		if fx.Case != nil {
			holders = append(holders, holder{"its card's", factoryd.BoardTask{Task: fx.Task}, []factoryd.Case{*fx.Case}})
			if fx.Task.Kind == "human" {
				waits := factoryd.BoardTask{Task: fx.Task, Dependencies: []factoryd.DepFact{{TaskID: "t-dep", Issue: 1999, Status: "queued"}}}
				holders = append(holders, holder{"on a card that waits for #1999", waits, []factoryd.Case{*fx.Case}})
			}
			if fx.Task.Kind == "agent" && fx.Approval == nil {
				helper := fx.Task
				helper.Source, helper.Parent = factoryd.SourceSizing, "t-parent"
				parent := factoryd.BoardTask{Task: factoryd.Task{ID: "t-parent", Kind: "agent", Status: "running", Repository: fx.Task.Repository, IssueNumber: 1998,
					Objective: "Board export: import a snapshot back"}, Helpers: []factoryd.BoardTask{{Task: helper}}}
				holders = append(holders, holder{"a helper's", parent, []factoryd.Case{*fx.Case}})
			}
		}
		if fx.Approval != nil {
			holders = append(holders, holder{"no case's", factoryd.BoardTask{Task: fx.Task}, nil})
		}
		states := map[string]factoryd.Request{"no request": {}}
		_, promise := factoryd.PromiseOn(caseOf(fx))
		_, holdout := factoryd.HoldoutOn(caseOf(fx))
		switch {
		case fx.Approval != nil && (promise || holdout):
			// a page the fixture made already: its request's rows as the
			// page holds them
			states = map[string]factoryd.Request{"as paged": {ApprovalRequest: *fx.Approval}}
		case fx.Approval != nil:
			states = requestStates(fx)
		}
		for state, r := range states {
			var pending []factoryd.ApprovalRequest
			if fx.Approval != nil {
				pending = []factoryd.ApprovalRequest{*fx.Approval}
			}
			read := func(factoryd.ApprovalRequest) factoryd.Request { return r }
			for _, h := range holders {
				asks := map[int64]factoryd.CaseAsk{}
				if fx.Ask != nil && fx.Case != nil {
					asks[fx.Case.ID] = *fx.Ask
				}
				for _, p := range factoryd.CardPages(h.card, h.cases, asks, pending, read) {
					if n := len(p.Face.Offers); n < 2 || n > 4 {
						var labels []string
						for _, o := range p.Face.Offers {
							labels = append(labels, o.Label)
						}
						t.Errorf("%s/%s, %s, %s case, page %s (%s): %q offers %d answers %q, want 2 to 4",
							fx.Group, fx.Kind, state, h.name, p.Key(), p.Case.Kind, p.Face.Ask, n, labels)
					}
					if p.Case.ID <= 0 && slices.ContainsFunc(p.Face.Offers, func(o factoryd.AskOffer) bool { return o.Value == factoryd.ActNote }) {
						t.Errorf("%s/%s, %s, %s case, page %s (%s): %q offers a Not now that keeps a note, and no case holds the page",
							fx.Group, fx.Kind, state, h.name, p.Key(), p.Case.Kind, p.Face.Ask)
					}
				}
			}
		}
	}
}

// A criteria request offers what its rows take: with every promise met or
// dropped, the plan's close now or the wait, which ends in the factory's
// close only where the close check closes the plan by itself, and no card
// to build; The work is enough only once the plan's cards are delivered.
func TestACriteriaRequestOffersWhatItsRowsTake(t *testing.T) {
	i := slices.IndexFunc(facefixtures.All(), func(fx facefixtures.Fixture) bool {
		return fx.Group == facefixtures.GroupApproval && fx.Kind == factoryd.ApprovalCriteria
	})
	fx := facefixtures.All()[i]
	states := requestStates(fx)
	pages := func(state string) []factoryd.CardPage {
		return factoryd.CardPages(factoryd.BoardTask{Task: fx.Task}, []factoryd.Case{*fx.Case}, nil, []factoryd.ApprovalRequest{*fx.Approval},
			func(factoryd.ApprovalRequest) factoryd.Request { return states[state] })
	}
	offered := func(p factoryd.CardPage) (answers, more []string) {
		face := factoryd.FaceOf(p.Case, factoryd.Facts{CaseFacts: factoryd.FactsOf(p.Case), Card: fx.Task, Case: p.Case})
		for _, o := range p.Face.Offers {
			answers = append(answers, o.Label+": "+o.Effect)
		}
		return answers, labels(face.More)
	}
	for state, wait := range map[string]string{"all settled (delivered true)": "The plan closes by itself within minutes.",
		"every promise met by the gate": "The plan stays open until someone closes it."} {
		settled := pages(state)
		answers, more := offered(settled[0])
		if want := []string{"Close the plan now: Closes the plan now, so production deploys go on.", "Not now: " + wait}; len(settled) != 1 || !slices.Equal(answers, want) ||
			slices.Contains(more, "Build the missing part") || settled[0].Face.Ask != "Every promise of plan “divide and conquer” is met or dropped. Close it now?" {
			t.Errorf("%s: %d pages, ask %q, answers %q, more %q; want one page that asks to close now, %q, and no card to build", state, len(settled), settled[0].Face.Ask, answers, more, want)
		}
	}
	for _, state := range []string{"unjudged (delivered false)", "unjudged (delivered true)"} {
		for _, p := range pages(state) {
			answers, more := offered(p)
			if enough := slices.Contains(more, "The work is enough") || slices.ContainsFunc(answers, func(a string) bool { return strings.HasPrefix(a, "The work is enough:") }); enough != strings.HasSuffix(state, "(delivered true)") {
				t.Errorf("%s, page %s: The work is enough offered %v, answers %q, more %q", state, p.Key(), enough, answers, more)
			}
		}
	}
}
