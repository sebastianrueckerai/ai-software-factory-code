package factoryd

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// sessionNoPushBound is how many sessions in a row may end without a push
// before the digest flags them.
const sessionNoPushBound = 2

// expectedBudget is the budget card t holds when nothing moved it but its
// events: its filing's budget plus every card_funded and card_extended
// amount. The filing's budget is the one its card_created row records,
// else, for a card filed before the rows recorded it, the policy's
// (policy.CardBudget, as intake and the planner file a card).
func (r *Runner) expectedBudget(ctx context.Context, t Task, events []Event) (float64, error) {
	var eur float64
	filed := false
	if i := slices.IndexFunc(events, func(e Event) bool { return e.Kind == EventCardCreated }); i >= 0 {
		eur, filed = events[i].Detail["budget_eur"].(float64)
	}
	if !filed {
		p, _, err := r.store.ProjectByRepository(ctx, t.Repository)
		if err != nil {
			return 0, err
		}
		_, asked := askedBudget(t.Objective)
		eur = policy.CardBudget(r.cfg.Policy().Document.Budget, p.Budget, t.Size, t.Rung == RungFrontier, asked)
	}
	for _, e := range events {
		if e.Kind == EventCardFunded || e.Kind == EventCardExtended {
			amount, _ := e.Detail["amount_eur"].(float64)
			eur += amount
		}
	}
	return eur, nil
}

// copiesBudget reports whether card t took its budget from another card:
// a successor, or a retry's replacement, whose funding rows stay with the
// card it replaces.
func copiesBudget(t Task, events []Event) bool {
	return t.Source == SourceSuccessor || slices.ContainsFunc(events, func(e Event) bool {
		replaces, _ := e.Detail["replaces"].(string)
		return e.Kind == EventCardCreated && replaces != ""
	})
}

// digestFlags are the patterns the card's life shows, by name: one pass
// over the steps, then the cases, the row and the dependencies.
func digestFlags(t Task, steps []DigestStep, cases []Case, deps []Dependency, budget float64) []DigestFlag {
	var flags []DigestFlag
	add := func(name, detail string) { flags = append(flags, DigestFlag{name, detail}) }
	confirmed := map[string]int{}
	review := ""
	// a grant is open until a session pushes; afterNoPush names the first
	// session after it that ended without a push
	granted, afterNoPush := false, ""
	run, runFirst := 0, ""
	for _, s := range steps {
		switch s.Kind {
		case "answer", "granted", "funded", "extended":
			if granted && afterNoPush != "" {
				add("grant_bought_nothing", afterNoPush)
			}
			granted, afterNoPush = true, ""
		case "session":
			if s.state == "" {
				continue
			}
			attempt, _, _ := strings.Cut(s.Actor, " ")
			name := "session " + attempt
			if s.state == string(OutcomePushed) {
				granted = false
			} else if granted && afterNoPush == "" {
				afterNoPush = name
			}
			if s.state != string(OutcomeNoPush) {
				run = 0
				continue
			}
			if run++; run == 1 {
				runFirst = name
			}
			if run == sessionNoPushBound {
				add("session_no_push", runFirst)
			}
		case "review":
			if s.state != "" {
				review = s.state
			}
		case "checkin":
			if s.state == "" {
				add("checkin_cut", offsetText(s.Offset))
			}
			for _, id := range s.Findings {
				if confirmed[id]++; confirmed[id] == 2 {
					add("finding_confirmed_twice", id)
				}
			}
		}
	}
	if granted && afterNoPush != "" {
		add("grant_bought_nothing", afterNoPush)
	}
	for _, c := range cases {
		if c.Status != CaseOpen && c.Status != CaseAtHuman {
			continue
		}
		if c.Money != nil && review == "concerns" {
			add("money_ask_hides_findings", "")
		}
		if slices.ContainsFunc(cases, func(o Case) bool { return o.ID < c.ID && o.Status == CaseResolved && o.StateKey == c.StateKey }) {
			add("answer_changed_nothing", c.StateKey)
		}
		if Final(t.Status) {
			add("issue_open_after_end", string(c.Kind))
		}
	}
	switch {
	case !holdsSlot(t):
	case t.Status == "waiting-deps" && t.AdmittedAt != nil:
		add("slot_held_by_wait", t.Status)
	case t.Status == "in-review" && t.Gate == GateApproval:
		add("slot_held_by_wait", t.Gate)
	}
	for _, d := range deps {
		if d.Status == "failed" {
			add("dependency_failed", fmt.Sprintf("#%d", d.Issue))
		}
	}
	if defaultKind(t.Kind) == "agent" && Cents(t.BudgetEUR) != Cents(budget) {
		add("budget_moved_no_event", fmt.Sprintf("%.2f EUR not %.2f EUR", t.BudgetEUR, budget))
	}
	slices.SortFunc(flags, func(a, b DigestFlag) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Detail, b.Detail))
	})
	return slices.Compact(flags)
}
