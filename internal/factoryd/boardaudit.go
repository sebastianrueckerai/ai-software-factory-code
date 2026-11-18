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

// BotChecks reads a commit's checks as f's bot, green as the gates read
// them (ChecksGreen).
func BotChecks(f *Forgejo) ChecksRead {
	return func(ctx context.Context, project, sha string) (bool, error) {
		repo, err := SplitRepo(project)
		if err != nil {
			return false, err
		}
		cs, err := f.CombinedStatus(ctx, repo.Owner, repo.Name, sha)
		return err == nil && ChecksGreen(cs), err
	}
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

// AuditBoard is every finding on repository's board outside Done, by
// issue. Its cards are the open ones, each finished card that shows a case
// (AskingCards) and each card a pending request names. Over each card's
// brief it runs the filing door's checks field by field, with refs
// resolving the references, and asks a plan's card for a why line; over
// each page of the asks the board shows (CardPages) it counts 2 to 4
// answers. checks reads a red merge head's checks. A read that fails
// fails the audit.
func AuditBoard(ctx context.Context, s *PGStore, repository string, refs RefResolver, checks ChecksRead) ([]BoardFinding, error) {
	cards, err := s.BoardTasks(ctx, repository)
	if err != nil {
		return nil, err
	}
	desk, err := s.CaseDesk(ctx, repository)
	if err != nil {
		return nil, err
	}
	asking, err := AskingCards(ctx, s, desk.Cases, cards)
	if err != nil {
		return nil, err
	}
	cards = append(cards, asking...)
	all, err := s.ApprovalRequests(ctx, "pending")
	if err != nil {
		return nil, err
	}
	pending := slices.DeleteFunc(all, func(a ApprovalRequest) bool { return a.Project != repository })
	var readErr error
	task := func(id string) Task {
		for _, b := range cards {
			if b.ID == id {
				return b.Task
			}
			if i := slices.IndexFunc(b.Helpers, func(h BoardTask) bool { return h.ID == id }); i >= 0 {
				return b.Helpers[i].Task
			}
		}
		t, ok, err := s.Task(ctx, id)
		if err == nil && !ok {
			err = fmt.Errorf("the card %s: %w", id, ErrNotFound)
		}
		readErr = cmp.Or(readErr, err)
		return t
	}
	read := func(a ApprovalRequest) Request {
		r, err := ReadRequest(ctx, s, a, task(a.TaskID), checks)
		if err != nil {
			readErr = cmp.Or(readErr, fmt.Errorf("approval request %d: %w", a.ID, err))
		}
		return r
	}
	var out []BoardFinding
	answers := func(card Task, pages []CardPage) {
		for _, p := range pages {
			if n := len(p.Face.Offers); n < 2 || n > mostAnswers {
				refusal := fmt.Sprintf("An ask offers 2 to 4 answers, and %q offers %d", p.Face.Ask, n)
				var labels []string
				for _, o := range p.Face.Offers {
					labels = append(labels, o.Label)
				}
				if n > 0 {
					refusal += ": " + strings.Join(labels, ", ")
				}
				out = append(out, BoardFinding{Issue: int(card.IssueNumber), Field: "answers", Refusal: refusal + "."})
			}
		}
	}
	shows := map[string]bool{}
	for _, b := range cards {
		if cases := CardCases(b, desk.Cases); len(cases) > 0 {
			shows[b.ID] = true
			answers(b.Task, CardPages(b, cases, desk.Asks, pending, read))
		}
	}
	// a request its card holds no case for asks on a card of its own
	for _, a := range pending {
		if shows[a.TaskID] {
			continue
		}
		t := task(a.TaskID)
		if !slices.ContainsFunc(cards, func(b BoardTask) bool { return b.ID == t.ID }) {
			cards = append(cards, BoardTask{Task: t})
		}
		answers(t, CardPages(BoardTask{Task: t}, nil, nil, []ApprovalRequest{a}, read))
	}
	if readErr != nil {
		return nil, readErr
	}
	var briefs []BoardFinding
	for _, b := range cards {
		found, err := auditBrief(ctx, s, b.Task, refs)
		if err != nil {
			return nil, err
		}
		briefs = append(briefs, found...)
	}
	out = append(briefs, out...)
	slices.SortStableFunc(out, func(a, b BoardFinding) int { return cmp.Compare(a.Issue, b.Issue) })
	return out, nil
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
