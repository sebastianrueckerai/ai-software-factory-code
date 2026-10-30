package factoryd

import "fmt"

// filingRound is the planning round the next revision of a plan files:
// the round of its pending replan step (zero: none) when the step names
// one, else the round of prev, its newest revision; a plan's first is
// round 1.
func filingRound(step ReplanStep, prev *Plan) int {
	switch {
	case step.Round > 0:
		return step.Round
	case prev != nil:
		return prev.Round
	}
	return 1
}

// settledCards are the entries of prev that a revision of round keeps as
// they are: those of earlier rounds whose card in cards is done and still
// on main, the integration card's aside, in prev's order.
func settledCards(prev Plan, cards []Task, round int) []PlanCard {
	done := map[string]bool{}
	for _, k := range cards {
		if k.PlanKey != "" && k.Status == "done" && k.RevertedAt == nil {
			done[k.PlanKey] = true
		}
	}
	var out []PlanCard
	for _, c := range prev.Cards {
		if c.Round < round && c.Role != RoleIntegration && done[c.Key] {
			out = append(out, c)
		}
	}
	return out
}

// settledFindings refuses an answer p that names a settled card: as a
// card of its own, or in take_off_main.
func settledFindings(p Plan, settled []PlanCard) []string {
	round := map[string]int{}
	for _, c := range settled {
		round[c.Key] = c.Round
	}
	var out []string
	for _, c := range p.Cards {
		if n, ok := round[c.Key]; ok {
			out = append(out, fmt.Sprintf("plan: card %s: the key of a merged card of round %d; the plan keeps that card as it is, so leave it out and give a new card a new key", c.Key, n))
		}
	}
	for _, k := range p.TakeOffMain {
		if n, ok := round[k]; ok {
			out = append(out, fmt.Sprintf("plan: take_off_main names card %s, settled in round %d: it stays on main as it is; a person's off-main replan can take it off main", k, n))
		}
	}
	return out
}

// planView is p as its planner reads it again: without the settled cards
// and without factoryd's round stamps.
func planView(p Plan, settled []PlanCard) Plan {
	keys := map[string]bool{}
	for _, c := range settled {
		keys[c.Key] = true
	}
	cards := []PlanCard{}
	for _, c := range p.Cards {
		if !keys[c.Key] {
			c.Round = 0
			cards = append(cards, c)
		}
	}
	p.Round, p.Cards = 0, cards
	return p
}
