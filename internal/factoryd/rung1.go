package factoryd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// humanOnlyKinds are the case kinds only a human decides: the rungs above
// code analyse them but do not decide.
var humanOnlyKinds = map[CaseKind]string{
	CasePlanQuestion:     "a plan question is the human's to answer",
	CasePlanConfirmation: "a plan confirmation is the human's to give",
	CaseMonthCap:         "only a commit raises the month cap",
	CaseProjectCap:       "only a commit raises the project cap",
	CaseDayCap:           "only a commit raises the day cap",
}

// humanOnlyReasons are the reason kinds of a check-in's hand to the human
// that no rung decides.
var humanOnlyReasons = map[string]string{
	CheckGoalConflict: "the check-in names a goal conflict",
	CheckPolicyClass:  "the merge gate marks the policy class human",
	CheckAccess:       "the card needs access the factory lacks",
}

// humanOnly says whether case c skips the rungs' decision, and why in
// words.
func humanOnly(c Case, f CaseFacts) (bool, string) {
	if why, ok := humanOnlyKinds[c.Kind]; ok {
		return true, why
	}
	switch c.Kind {
	case CaseCheckinHuman:
		kind, _ := c.Facts["reason_kind"].(string)
		why, ok := humanOnlyReasons[kind]
		return ok, why
	case CaseBudgetFloor:
		if c.Money != nil && slices.Contains(f.Partial, "pool") {
			return true, "the pool could not be read, so the card's need of money cannot be weighed against it"
		}
		if c.Money != nil && Cents(c.Money.NeedEUR) > Cents(f.PoolLeft.EUR) {
			return true, fmt.Sprintf("the card needs %.2f EUR and the pool holds %.2f EUR", c.Money.NeedEUR, f.PoolLeft.EUR)
		}
	}
	return false, ""
}

const stepCut = 300

// rungStatement is what a rung receives for case c, built by code: the
// problem, the money, the digest, the plan, the earlier steps and the
// actions of rung with the refusals of those it may not take.
func (r *Runner) rungStatement(c Case, f CaseFacts, digest string, steps []CaseStep, rung int) string {
	f.atCase(c)
	cut := func(s string) string { return textutil.Truncate(r.Scrub.String(s), stepCut) }
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	line("%s", r.Scrub.String(c.Problem))
	line("Money: budget %.2f, spent %.2f, left %.2f EUR, ceiling %.2f EUR; card draw left %s; pool left %s.",
		f.BudgetEUR, f.SpentEUR, f.LeftEUR, f.CeilingEUR, drawText(f.CardDrawLeft), drawText(f.PoolLeft))
	line("%s", r.Scrub.String(digest))
	switch {
	case f.Plan == "":
		line("This card is in no plan.")
	case slices.Contains(f.Partial, "siblings"):
		line("The card's plan and its other cards could not be read.")
	default:
		line("Plan #%d %s, %s", f.PlanIssue, f.PlanName, f.PlanStatus)
		for _, s := range f.Siblings {
			line("#%d %s: %s", s.Issue, s.Key, s.Status)
		}
	}
	for _, s := range steps {
		line("Rung %d, %s: outcome %s, decision %s, grounds %s, found %s", s.Rung, cut(s.Actor), cut(s.Outcome), cut(s.Decision), cut(s.Grounds), cut(s.Found))
	}
	for _, a := range Actions {
		if !a.Rungs[rung] {
			continue
		}
		p := probe(a, f)
		if why := a.refusal(f, rung, Resolution{Action: a.Name, Args: p.Args, Note: p.Note}); why != "" {
			line("- %s: not offered: %s", a.Name, why)
		} else {
			line("- %s", a.Name)
		}
	}
	return b.String()
}
