package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
)

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
			delete(round, k)
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

// roundTasks are the tasks of cards that file a card of p's round.
func roundTasks(p Plan, cards []Task) []Task {
	keys := map[string]bool{}
	for _, c := range roundCards(p) {
		keys[c.Key] = true
	}
	var out []Task
	for _, k := range cards {
		if keys[k.PlanKey] {
			out = append(out, k)
		}
	}
	return out
}

// roundComplete tells whether p's round is over: each of its cards ended
// done or closed, one at least done, and no card outside p's keys, a
// delta card, is still open.
func roundComplete(p Plan, cards []Task) bool {
	round := roundTasks(p, cards)
	if !slices.ContainsFunc(round, func(k Task) bool { return k.Status == "done" }) ||
		slices.ContainsFunc(round, func(k Task) bool { return k.Status != "done" && k.Status != "closed" }) {
		return false
	}
	keys := map[string]bool{}
	for _, c := range p.Cards {
		keys[c.Key] = true
	}
	return !slices.ContainsFunc(cards, func(k Task) bool { return !keys[k.PlanKey] && !Final(k.Status) })
}

// cardsDone is "<n> card(s), <d> done" of tasks.
func cardsDone(tasks []Task) string {
	done := 0
	for _, k := range tasks {
		if k.Status == "done" {
			done++
		}
	}
	return fmt.Sprintf("%s, %d done", plural(len(tasks), "card"), done)
}

// RoundLine is the board's line of p's round, given the plan's cards: its
// cards and how many are done, then the parts still to plan. "" for a
// plan of one round, and for one filed before rounds.
func RoundLine(p Plan, cards []Task) string {
	if p.Round <= 1 && len(p.Outline) == 0 {
		return ""
	}
	line := fmt.Sprintf("round %d: %s.", p.Round, cardsDone(roundTasks(p, cards)))
	if len(p.Outline) > 0 {
		line += " Still to plan: " + strings.Join(p.Outline, ", ") + "."
	}
	return line
}

// RoundItem is one line of a plan's rounds on the board: a round's cards,
// Current for the plan's round, or a part still to plan.
type RoundItem struct {
	Text    string
	Current bool
}

// RoundItems are the board's lines of p's rounds, given the plan's cards:
// one per round with a card in p, then one per part still to plan. nil
// when RoundLine is "".
func RoundItems(p Plan, cards []Task) []RoundItem {
	if RoundLine(p, cards) == "" {
		return nil
	}
	rounds := map[string]int{}
	for _, c := range p.Cards {
		rounds[c.Key] = c.Round
	}
	var out []RoundItem
	for n := 1; n <= p.Round; n++ {
		if !slices.ContainsFunc(p.Cards, func(c PlanCard) bool { return c.Round == n }) {
			continue
		}
		var tasks []Task
		for _, k := range cards {
			if rounds[k.PlanKey] == n {
				tasks = append(tasks, k)
			}
		}
		item := RoundItem{Text: fmt.Sprintf("round %d: %s.", n, cardsDone(tasks))}
		if n == p.Round {
			item = RoundItem{Text: item.Text + " This round.", Current: true}
		}
		out = append(out, item)
	}
	for _, part := range p.Outline {
		out = append(out, RoundItem{Text: "still to plan: " + part})
	}
	return out
}

// roundCheck starts plan parent's next round: once the plan is done, its
// newest revision leaves parts to later rounds and its round is over, the
// round's replan step sends it back to its planner, its open issue left
// as it is. A round step whose reopen was lost is reopened. A plan issue
// closed at that point by a close that counts (closeCounts) is a
// person's close the webhook missed: the card ends closed and nothing
// starts. Any other close does not count, as on the webhook: the issue
// opens again and the round starts. A closed plan, one under another
// replan and one whose own group revert runs are left as they are.
func (r *Runner) roundCheck(ctx context.Context, parent string) {
	if err := r.nextRound(ctx, parent); err != nil {
		log.Printf("plan %s: the round check: %v", parent, err)
	}
}

// nextRound is roundCheck's work, its error the check's log line. It
// reads no timeline for a plan without an outline or whose round runs.
func (r *Runner) nextRound(ctx context.Context, parent string) error {
	t, repo, err := r.task(ctx, parent)
	if err != nil || t.Status != "done" {
		return err
	}
	p, revision, err := planOfStore(ctx, r.store, parent, "")
	if err != nil || len(p.Outline) == 0 || r.planClosed(ctx, parent) {
		return err
	}
	if pause, paused, err := r.store.QueuePause(ctx, t.Repository); err != nil || paused && pause.Plan == t.ID {
		return err
	}
	cards, err := r.store.PlanCards(ctx, parent)
	if err != nil || !roundComplete(p, cards) {
		return err
	}
	step, pending, err := r.pendingReplan(ctx, t)
	if err != nil || pending && step.Origin != ReplanRound {
		return err
	}
	closer, err := r.forgejo.IssueClosedBy(ctx, repo.Owner, repo.Name, t.IssueNumber)
	closed := closer != "" || errors.Is(err, errNoCloser)
	switch {
	case errors.Is(err, errNoCloser):
		log.Printf("plan %s: issue #%d closed, its timeline names no closer: not counted", t.ID, t.IssueNumber)
	case err != nil:
		return err
	case closed:
		switch counts, err := closeCounts(ctx, r.store, r.forgejo.BotUser, r.Writes, t, closer); {
		case err != nil:
			return err
		case counts:
			return r.store.TransitionTask(ctx, t.ID, []string{"done"}, "closed")
		}
	}
	if !pending {
		wrote, err := r.timeline.AppendUnlessPending(ctx, t.ID, stepReplan,
			map[string]any{"origin": ReplanRound, "revision": revision, "round": p.Round + 1}, replanClosers...)
		if err != nil || !wrote {
			return err
		}
	}
	return r.requeuePlan(ctx, t, closed)
}

// planRevisions reads plan revisions: the Store does, and the console's
// tasks.
type planRevisions interface {
	PlanRevision(ctx context.Context, id string, n int) (PlanRevision, bool, error)
}

// leavesRounds tells whether plan id's newest revision leaves parts to
// later rounds: its outline has parts. A plan without a revision leaves
// none.
func leavesRounds(ctx context.Context, store planRevisions, id string) (bool, error) {
	rev, ok, err := store.PlanRevision(ctx, id, 0)
	if err != nil || !ok {
		return false, err
	}
	var p Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return false, fmt.Errorf("plan %s revision %d: %w", id, rev.Revision, err)
	}
	return len(p.Outline) > 0, nil
}

// BetweenRounds tells whether plan card t is done while its newest
// revision leaves parts to later rounds: a person's close ends it closed,
// as a failed card's does.
func BetweenRounds(ctx context.Context, store planRevisions, t Task) (bool, error) {
	if t.Status != "done" {
		return false, nil
	}
	return leavesRounds(ctx, store, t.ID)
}
