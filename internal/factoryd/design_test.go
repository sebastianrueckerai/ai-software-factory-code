package factoryd

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// designDecisionsC20 is the document repository's design decisions as
// the C20 story holds it (design/design-decisions.md): the pulse band
// ruling with three clauses in its screen after, an entry before it and
// one after it without a screen after.
const designDecisionsC20 = `# Design decisions

## C19. An earlier ruling (2026-10-16, Sebastian)

Body of the earlier ruling.

## C20. The pulse band goes: the strip carries the status line (2026-10-17, Sebastian)

Decided in conversation, 2026-10-17 evening, no image round.

The screen after: the strip stays as it is; the SPEND / provider / configuration panel renders always, without an expand control; the band's markup, the Pulser loop and the pulse's model calls are deleted. The chat task stays for the conversation.

What goes: the pulse sentence.

## C21. A later ruling (2026-10-17, Sebastian)

Body of the later ruling, no screen after.
`

// An entry's screen after is its clause list: the first sentence after
// "The screen after:" split on its semicolons; an entry without one is
// a single clause, its title without the heading's date-and-who tail.
func TestFindRuling(t *testing.T) {
	e := findRuling([]byte(designDecisionsC20), "c20")
	if e == nil {
		t.Fatal("no entry C20")
	}
	want := []string{
		"the strip stays as it is",
		"the SPEND / provider / configuration panel renders always, without an expand control",
		"the band's markup, the Pulser loop and the pulse's model calls are deleted",
	}
	if strings.Join(e.Clauses, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("clauses = %q, want %q", e.Clauses, want)
	}
	if e.Title != "The pulse band goes: the strip carries the status line" {
		t.Fatalf("title = %q", e.Title)
	}
	later := findRuling([]byte(designDecisionsC20), "C21")
	if later == nil || len(later.Clauses) != 1 || later.Clauses[0] != "A later ruling" {
		t.Fatalf("entry without a screen after = %+v, want one clause of its title", later)
	}
	if findRuling([]byte(designDecisionsC20), "C99") != nil {
		t.Fatal("entry C99 found")
	}
}

// The Ships line: the ruling a card cites and the clauses of the
// screen after it ships, 1-based; a bare citation states none.
func TestAskedShips(t *testing.T) {
	for name, tc := range map[string]struct {
		body   string
		ruling string
		want   []int
	}{
		"clauses":        {"Ship it\n\nShips: C20 clauses 1, 3\n", "C20", []int{1, 3}},
		"one clause":     {"Ships: C20 clause 2", "C20", []int{2}},
		"any case":       {"ships: c20 clauses 3,1", "C20", []int{1, 3}},
		"no spaces":      {"Ships: C20 clauses 1,3", "C20", []int{1, 3}},
		"bare citation":  {"Ships: C20", "C20", nil},
		"last wins":      {"Ships: C20 clause 1\nShips: C20 clause 2", "C20", []int{2}},
		"no line":        {"No citation.", "", nil},
		"not a ruling":   {"Ships: D20 clauses 1", "", nil},
		"no clause kept": {"Ships: C20 clauses zero, x", "C20", nil},
	} {
		t.Run(name, func(t *testing.T) {
			ruling, clauses := askedShips(tc.body)
			if ruling != tc.ruling || fmt.Sprint(clauses) != fmt.Sprint(tc.want) {
				t.Fatalf("askedShips = %q, %v, want %q, %v", ruling, clauses, tc.ruling, tc.want)
			}
		})
	}
}

// The Carries line: the pull request whose head a carry card's clone
// supplies, as branch pull/<n>; 0 without one.
func TestAskedCarry(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want int64
	}{
		"hash":         {"Carry the branch\n\nCarries: #9\n", 9},
		"bare number":  {"Carries: 9", 9},
		"any case":     {"carries: #41", 41},
		"last wins":    {"Carries: #9\nCarries: #41", 41},
		"no line":      {"No carry.", 0},
		"not a number": {"Carries: nine", 0},
		"zero":         {"Carries: #0", 0},
		"negative":     {"Carries: #-1", 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got := askedCarry(tc.body); got != tc.want {
				t.Fatalf("askedCarry = %d, want %d", got, tc.want)
			}
		})
	}
}

// The Ships line is card data, not brief prose: every model brief drops
// it with the other intake lines.
func TestBriefObjectiveDropsTheShipsLine(t *testing.T) {
	got := briefObjective("The band goes\n\nThe screen.\n\nShips: C20 clauses 1, 3\nSize: small", false)
	if got != "The band goes\n\nThe screen." {
		t.Fatalf("brief = %q, want it without the Ships and Size lines", got)
	}
}

// The Carries line is card data too: the brief drops it with the others,
// and the carry sentence says what the clone holds.
func TestBriefObjectiveDropsTheCarriesLine(t *testing.T) {
	got := briefObjective("The band goes\n\nThe screen.\n\nCarries: #9", false)
	if got != "The band goes\n\nThe screen." {
		t.Fatalf("brief = %q, want it without the Carries line", got)
	}
}

// reviewShips runs the model review of task over doc as the design
// decisions of sf/docs, the document repository of its project, with
// the citing sibling cards in the store, at the model's approve.
func reviewShips(t *testing.T, task Task, siblings []Task, doc string) Review {
	t.Helper()
	fj := &gateForgejo{diff: "diff --git a/Makefile b/Makefile\n+check:\n"}
	if doc != "" {
		fj.raw = map[string]string{"sf/docs/raw/" + designDecisionsPath: doc}
	}
	r, store, _ := newGateRunner(t, task, fj)
	for _, s := range siblings {
		if err := store.CreateTask(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	v := r.cfg.Policy()
	if v.Projects == nil {
		v.Projects = map[string]policy.ProjectConfig{}
	}
	v.Projects["repo"] = policy.ProjectConfig{Repository: task.Repository}
	if v.Document.Chat == nil {
		v.Document.Chat = &policy.ChatConfig{}
	}
	v.Document.Chat.DocRepo = "sf/docs"
	r.cfg.Policy = func() policy.Version { return v }
	ll := &fakeLiteLLM{content: `{"verdict":"approve","summary":"ok","findings":[]}`}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	val, err := execActivity(t, r, ActivityModelReview, task.ID, PullRequest{Number: 9}, "c1", GateResults{})
	return getInto[Review](t, val, err)
}

// The t-e98f769e872d story: the card of PR 1032 shipped only the band's
// clause while the ruling's screen after counts three, and no card
// carried the panel clause. The review gate refuses such a card,
// naming the omitted clause however the model judged the diff.
func TestModelReviewRefusesARulingClauseLeftToNoCard(t *testing.T) {
	task := gateTask
	task.Objective = "the pulse band leaves the screen: the strip carries the status line\n\nShips: C20 clauses 1, 3"
	task.ShipsRuling, task.ShipsClauses = "C20", []int{1, 3}
	rv := reviewShips(t, task, nil, designDecisionsC20)
	if rv.Verdict != "concerns" {
		t.Fatalf("verdict = %q, want concerns", rv.Verdict)
	}
	joined := strings.Join(rv.Findings, "\n")
	if !strings.Contains(joined, "clause 2 of C20") ||
		!strings.Contains(joined, `the SPEND / provider / configuration panel renders always, without an expand control`) {
		t.Fatalf("findings name no omitted clause: %q", joined)
	}
}

// The finishing card t-e98f769e872d ships clause 2 alone: the union
// with the earlier card's clauses 1 and 3 covers the ruling's whole
// screen after and the review stands at the model's verdict. Alone, it
// leaves clauses 1 and 3 to no card and the review refuses it naming
// them.
func TestModelReviewPassesARulingWhoseClausesAreAllListed(t *testing.T) {
	task := gateTask
	task.ShipsRuling, task.ShipsClauses = "C20", []int{2}
	earlier := Task{ID: "t2", Repository: "sf/repo", Status: "done", ShipsRuling: "C20", ShipsClauses: []int{1, 3}, Track: "direct"}
	if rv := reviewShips(t, task, []Task{earlier}, designDecisionsC20); rv.Verdict != "approve" || len(rv.Findings) != 0 {
		t.Fatalf("review = %+v, want the model's approve with no finding", rv)
	}
	rv := reviewShips(t, task, nil, designDecisionsC20)
	joined := strings.Join(rv.Findings, "\n")
	if rv.Verdict != "concerns" || !strings.Contains(joined, "clause 1 of C20") || !strings.Contains(joined, "clause 3 of C20") {
		t.Fatalf("review = %+v, want the omitted clauses named: %q", rv, joined)
	}
}

// A broken Ships statement refuses the card: clause numbers the entry
// does not have, a citation naming no clause.
func TestModelReviewRefusesABrokenShipsStatement(t *testing.T) {
	task := gateTask
	task.ShipsRuling, task.ShipsClauses = "C20", []int{1, 7}
	rv := reviewShips(t, task, []Task{{ID: "t2", Repository: "sf/repo", Status: "done", ShipsRuling: "C20", ShipsClauses: []int{2}, Track: "direct"}},
		designDecisionsC20)
	joined := strings.Join(rv.Findings, "\n")
	if rv.Verdict != "concerns" || !strings.Contains(joined, "clause 7 of C20, which the entry does not have") ||
		!strings.Contains(joined, "clause 3 of C20") {
		t.Fatalf("review = %+v, want the unknown clause and the omitted one named: %q", rv, joined)
	}

	task.ShipsClauses = nil
	rv = reviewShips(t, task, nil, designDecisionsC20)
	joined = strings.Join(rv.Findings, "\n")
	if rv.Verdict != "concerns" || !strings.Contains(joined, "without stating clauses") ||
		!strings.Contains(joined, "clause 2 of C20") {
		t.Fatalf("review = %+v, want the bare citation and every clause named: %q", rv, joined)
	}
}

// A citation the document repository cannot back refuses the card too:
// no such entry, and no design decisions file at all.
func TestModelReviewRefusesARulingTheDocumentRepositoryDoesNotHold(t *testing.T) {
	task := gateTask
	task.ShipsRuling, task.ShipsClauses = "C99", []int{1}
	rv := reviewShips(t, task, nil, designDecisionsC20)
	if rv.Verdict != "concerns" || !strings.Contains(strings.Join(rv.Findings, "\n"), "names no entry C99") {
		t.Fatalf("review = %+v, want the missing entry named", rv)
	}
	rv = reviewShips(t, task, nil, "")
	if rv.Verdict != "concerns" || !strings.Contains(strings.Join(rv.Findings, "\n"), "is unreadable") {
		t.Fatalf("review = %+v, want the unreadable file named", rv)
	}
}
