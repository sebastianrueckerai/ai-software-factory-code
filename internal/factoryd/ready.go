package factoryd

import (
	"context"
	"fmt"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// CardAcceptance is a card body's acceptance section: the "- item" lines
// under an "Acceptance:" heading, as cardFacts renders them; nil without
// one. A ready card carries acceptance criteria (roadmap 239).
func CardAcceptance(body string) []string {
	var out []string
	in := false
	for l := range strings.Lines(body) {
		l = strings.TrimRight(l, "\n")
		switch {
		case strings.TrimSpace(l) == "Acceptance:":
			in = true
		case in && strings.HasPrefix(l, "- "):
			out = append(out, strings.TrimPrefix(l, "- "))
		case in:
			in = false
		}
	}
	return out
}

// ReadyStore is what the ready check counts.
type ReadyStore interface {
	CountAdmitted(ctx context.Context, repository string) (int, error)
	CountBacklog(ctx context.Context, repository string) (int, error)
}

// ReadyFindings are the ready check's per-card findings for a card that
// would start or be filed (roadmap 239): its size one of the card sizes
// and acceptance criteria present, one line each naming the card and the
// failed line; none for a ready card. The checks are the plan card's
// own; the acceptance comes from the body when the card names none.
func ReadyFindings(key, body, size string, acceptance []string) []string {
	if acceptance == nil {
		acceptance = CardAcceptance(body)
	}
	out := sizeFindings(size)
	out = append(out, acceptanceFindings(acceptance)...)
	for i := range out {
		out[i] = "card " + key + ": " + out[i]
	}
	return out
}

// StartRefusal is the one-line refusal for card t about to move into a
// starting lane of its process: the ready check's findings, then the
// project's work slots; "" when it may start. A failed count leaves the
// refusal to the workflow's own bound.
func StartRefusal(ctx context.Context, store ReadyStore, tree policy.Tree, t Task) string {
	if f := ReadyFindings(t.PlanKey, t.Objective, t.Size, nil); len(f) > 0 {
		return strings.Join(f, "; ")
	}
	n, err := store.CountAdmitted(ctx, t.Repository)
	if err != nil {
		return ""
	}
	if wip := tree.WorkInProgressOf(t.Repository); n >= wip {
		return fmt.Sprintf("card %s: all %d work slots of the project are busy; it starts when one frees", t.ID, wip)
	}
	return ""
}

// FileRefusal is the one-line refusal for filing a card into a starting
// lane of repository: the ready check's findings, then the project's
// backlog cap; "" when it may file.
func FileRefusal(ctx context.Context, store ReadyStore, tree policy.Tree, repository, key, title, body, size string) string {
	if f := ReadyFindings(key, title+"\n\n"+body, size, nil); len(f) > 0 {
		return strings.Join(f, "; ")
	}
	return BacklogRefusal(ctx, store, tree, repository, key)
}

// BacklogRefusal is the one-line refusal for filing a card of repository
// at the backlog cap, whatever lane it asks for; "" when there is room.
func BacklogRefusal(ctx context.Context, store ReadyStore, tree policy.Tree, repository, key string) string {
	n, err := store.CountBacklog(ctx, repository)
	if err != nil {
		return ""
	}
	if cap := tree.BacklogCapOf(repository); n >= cap {
		return fmt.Sprintf("card %s: the backlog holds %d of its %d cards; file it when one starts", key, n, cap)
	}
	return ""
}
