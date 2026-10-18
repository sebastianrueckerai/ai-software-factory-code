package factoryd

import (
	"context"
	"fmt"
	"strings"
)

// designDecisionsPath is the design rulings' file in the project's
// document repository (chat.doc_repo): its C-entries are the factory's
// design decisions, each entry one ruling.
const designDecisionsPath = "design/design-decisions.md"

// rulingEntry is one C-entry of the design decisions: its title and the
// clauses of its screen after, in order.
type rulingEntry struct {
	Title   string
	Clauses []string
}

// findRuling is entry id ("C20", any case) of the design decisions'
// markdown, nil when the file names no such entry. An entry's clauses
// are its "The screen after:" sentence split on the semicolons; an
// entry without one is a single clause, its title.
func findRuling(doc []byte, id string) *rulingEntry {
	head := "## " + strings.ToUpper(id) + ". "
	var title, body string
	found := false
	for _, line := range strings.Split(string(doc), "\n") {
		if strings.HasPrefix(line, "## ") {
			if found {
				break // the next entry ends this one
			}
			if strings.HasPrefix(line, head) {
				found = true
				title = strings.TrimSpace(strings.TrimPrefix(line, head))
			}
			continue
		}
		if found {
			body += line + "\n"
		}
	}
	if !found {
		return nil
	}
	entry := &rulingEntry{Title: title}
	if k := strings.LastIndex(entry.Title, " ("); k >= 0 && strings.HasSuffix(entry.Title, ")") {
		entry.Title = entry.Title[:k] // the heading's "(date, who)" tail
	}
	text := body
	i := strings.Index(text, "The screen after:")
	if i < 0 {
		entry.Clauses = []string{entry.Title}
		return entry
	}
	sent := text[i+len("The screen after:"):]
	if j := strings.Index(sent, ". "); j >= 0 {
		sent = sent[:j]
	}
	if j := strings.IndexByte(sent, '\n'); j >= 0 {
		sent = sent[:j]
	}
	for c := range strings.SplitSeq(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sent), ".")), ";") {
		if c = strings.TrimSpace(c); c != "" {
			entry.Clauses = append(entry.Clauses, strings.TrimSuffix(c, "."))
		}
	}
	return entry
}

// designScope is the review gate's ruling coverage over a card's Ships
// statement (a design decision's C-entry): the findings that fail the
// card when a clause of the ruling's screen after is left to no card
// citing the ruling — the card's own stated clauses unioned with every
// other card's of the same repository, whatever their status — or the
// statement itself is broken: a clause the entry does not have, a
// citation naming no clause, an entry or file the document repository
// does not hold. nil findings: the ruling's whole screen after is
// shipped. A card citing no ruling checks nothing.
func (r *Runner) designScope(ctx context.Context, t Task, pin int64) ([]string, error) {
	if t.ShipsRuling == "" {
		return nil, nil
	}
	id := t.ShipsRuling
	doc, err := r.docRepo(ctx, t, pin)
	if err != nil {
		return nil, err
	}
	if doc.Owner == "" {
		return []string{fmt.Sprintf("design scope: the card cites %s but its project has no document repository to read the ruling from", id)}, nil
	}
	content, err := r.forgejo.FileAt(ctx, doc.Owner, doc.Name, "master", designDecisionsPath)
	if err != nil {
		if forgejoTerminal(err) {
			return []string{fmt.Sprintf("design scope: %s in %s/%s is unreadable: %s", designDecisionsPath, doc.Owner, doc.Name, errMessage(err))}, nil
		}
		return nil, err
	}
	entry := findRuling(content, id)
	if entry == nil {
		return []string{fmt.Sprintf("design scope: %s in %s/%s names no entry %s", designDecisionsPath, doc.Owner, doc.Name, id)}, nil
	}
	var findings []string
	if len(t.ShipsClauses) == 0 {
		findings = append(findings, fmt.Sprintf("design scope: the card cites %s without stating clauses: its Ships line must name the clauses of the screen after it ships (Ships: %s clauses 1-%d)", id, id, len(entry.Clauses)))
	}
	covered := make([]bool, len(entry.Clauses))
	claim := func(clauses []int) {
		for _, c := range clauses {
			if c >= 1 && c <= len(entry.Clauses) {
				covered[c-1] = true
			}
		}
	}
	claim(t.ShipsClauses)
	for _, c := range t.ShipsClauses {
		if c < 1 || c > len(entry.Clauses) {
			findings = append(findings, fmt.Sprintf("design scope: the card states clause %d of %s, which the entry does not have: it counts %d clauses", c, id, len(entry.Clauses)))
		}
	}
	cards, err := r.store.TasksByRuling(ctx, t.Repository, id)
	if err != nil {
		return nil, err
	}
	for _, card := range cards {
		claim(card.ShipsClauses)
	}
	for i, ok := range covered {
		if !ok {
			findings = append(findings, fmt.Sprintf("design scope: clause %d of %s — %q — is left to no card: no card citing %s on %s ships it on its Ships line", i+1, id, entry.Clauses[i], id, t.Repository))
		}
	}
	return findings, nil
}
