package factoryd

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// BotRefs resolves the references of the briefs of repository's cards as
// the filing door does: as f's bot, with the project's document
// repository under tree.
func BotRefs(ctx context.Context, f *Forgejo, tree policy.Tree, repository string) (RefResolver, error) {
	refs, err := botRefsOf(ctx, f, tree, repository)
	if err != nil {
		return nil, err
	}
	return refs, nil
}

// BoardFinding is one break of the card contract on a card of the board:
// the card's issue, the field (title, why, summary, text or answers) and
// the refusal in plain words. HumanIssue marks a finding on the text of a
// human's own issue, which stays as the human wrote it.
type BoardFinding struct {
	Issue          int
	Field, Refusal string
	HumanIssue     bool
}

// AuditBoard is every finding on repository's board, by issue: the filing
// door's checks over the brief of each open card and of each card that
// asks, field by field, with refs resolving the references; a card of a
// plan without a why line; and an ask at the human whose face offers other
// than 2 to 4 answers. A reference refs could not read fails the audit.
func AuditBoard(ctx context.Context, s *PGStore, repository string, refs RefResolver) ([]BoardFinding, error) {
	board, err := s.BoardTasks(ctx, repository)
	if err != nil {
		return nil, err
	}
	desk, err := s.CaseDesk(ctx, repository)
	if err != nil {
		return nil, err
	}
	var cards []Task
	for _, b := range board {
		cards = append(cards, b.Task)
	}
	var out []BoardFinding
	for _, c := range desk.Cases {
		if c.Status != CaseAtHuman {
			continue
		}
		// a helper's case asks on its parent's card, on the helper's facts
		own, card, err := askingCards(ctx, s, c.TaskID)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(cards, func(t Task) bool { return t.ID == card.ID }) {
			cards = append(cards, card)
		}
		face := CaseFace(c, own, desk.Asks[c.ID])
		if n := len(face.Offers); n < 2 || n > mostAnswers {
			refusal := fmt.Sprintf("An ask offers 2 to 4 answers, and %q offers %d", face.Ask, n)
			var labels []string
			for _, o := range face.Offers {
				labels = append(labels, o.Label)
			}
			if n > 0 {
				refusal += ": " + strings.Join(labels, ", ")
			}
			out = append(out, BoardFinding{Issue: int(card.IssueNumber), Field: "answers", Refusal: refusal + "."})
		}
	}
	var briefs []BoardFinding
	for _, t := range cards {
		found, err := auditBrief(ctx, s, t, refs)
		if err != nil {
			return nil, err
		}
		briefs = append(briefs, found...)
	}
	out = append(briefs, out...)
	slices.SortStableFunc(out, func(a, b BoardFinding) int { return cmp.Compare(a.Issue, b.Issue) })
	return out, nil
}

// askingCards are the task of a case, id, and the card it asks on: its
// own, or a helper's parent.
func askingCards(ctx context.Context, s *PGStore, id string) (own, card Task, err error) {
	own, ok, err := s.Task(ctx, id)
	if err != nil || !ok {
		return Task{}, Task{}, cmp.Or(err, fmt.Errorf("the case's card %s: %w", id, ErrNotFound))
	}
	if !own.Report() || own.Parent == "" {
		return own, own, nil
	}
	card, ok, err = s.Task(ctx, own.Parent)
	if err != nil || !ok {
		return Task{}, Task{}, cmp.Or(err, fmt.Errorf("the parent %s of %s: %w", own.Parent, id, ErrNotFound))
	}
	return own, card, nil
}

// auditBrief is the findings on card t's brief: CheckBrief's refusals by
// the field each names, and a plan's card without a why line.
func auditBrief(ctx context.Context, s *PGStore, t Task, refs RefResolver) ([]BoardFinding, error) {
	b, job := storedBrief(t), BriefJobOf(t)
	human := job == BriefHumanIssue
	var out []BoardFinding
	for i, field := range [4]string{"title", "why", "summary", "text"} {
		var on [4]bool
		on[i] = true
		for _, l := range checkBrief(b, job, refs, on) {
			out = append(out, BoardFinding{Issue: int(t.IssueNumber), Field: field, Refusal: l, HumanIssue: human})
		}
	}
	if err := refs.Err(); err != nil {
		return nil, fmt.Errorf("the references of #%d: %w", t.IssueNumber, err)
	}
	// the door asks no why line of a human's issue, but a plan's card
	// needs one
	if human && b.Why == "" {
		plan, _, _, err := cardPlan(ctx, s, t)
		if err != nil {
			return nil, err
		}
		if plan.ID != "" {
			out = append(out, BoardFinding{Issue: int(t.IssueNumber), Field: "why",
				Refusal: "A card of a plan has no why line. Write in one sentence why the plan needs it.", HumanIssue: true})
		}
	}
	return out, nil
}
