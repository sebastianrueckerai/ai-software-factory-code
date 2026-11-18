package factoryd_test

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
)

// fixtureOf is the fixture of group and kind.
func fixtureOf(t *testing.T, group, kind string) facefixtures.Fixture {
	t.Helper()
	all := facefixtures.All()
	i := slices.IndexFunc(all, func(fx facefixtures.Fixture) bool { return fx.Group == group && fx.Kind == kind })
	if i < 0 {
		t.Fatalf("no fixture %s/%s", group, kind)
	}
	return all[i]
}

// The inbox's message about a session cap's ask is its card's face: the
// card's issue, which links it, and title, the why line and the suggested
// answer's label. It names the sessions, never the budget.
func TestCardMessageComesFromTheFace(t *testing.T) {
	fx := fixtureOf(t, facefixtures.GroupCase, string(factoryd.CaseSessionCap))
	m, ok := factoryd.PokeOf(factoryd.Event{Kind: factoryd.EventQuestionAsked, TaskID: fx.Task.ID, Detail: fx.Ask.Detail}, fx.Task, factoryd.Task{})
	want := "#2291 (Board export: import a snapshot back) needs you. A card runs only the sessions it was given, and this one has used all 3."
	if !ok || m.Content != want {
		t.Fatalf("the message = %q, %v\nwant %q", m.Content, ok, want)
	}
	if strings.Contains(m.Content, "budget") || !strings.Contains(m.Content, "sessions") {
		t.Errorf("the message names no sessions or the budget: %q", m.Content)
	}
	suggested := fx.Ask.Detail
	suggested["preselected"], suggested["recommended_by"] = map[string]any{"action": factoryd.ActExtend}, "the factory agent"
	m, _ = factoryd.PokeOf(factoryd.Event{Kind: factoryd.EventQuestionAsked, TaskID: fx.Task.ID, Detail: suggested}, fx.Task, factoryd.Task{})
	if want += " The factory agent suggests “Allow more sessions”."; m.Content != want {
		t.Errorf("the suggested message = %q\nwant %q", m.Content, want)
	}
}

// Every template of the inbox's pokes, written with the gallery's facts,
// passes the writing guide's check: an ask's message is at most 280
// characters, a notice one line of at most 160, and a button the factory
// offers takes the vocabulary's label.
func TestEveryPokePassesLint(t *testing.T) {
	var events []struct {
		e    factoryd.Event
		card factoryd.Task
	}
	add := func(e factoryd.Event, card factoryd.Task) {
		e.TaskID = card.ID
		events = append(events, struct {
			e    factoryd.Event
			card factoryd.Task
		}{e, card})
	}
	for _, fx := range facefixtures.All() {
		card := fx.Task
		if fx.Ask != nil {
			add(factoryd.Event{Kind: factoryd.EventQuestionAsked, Detail: fx.Ask.Detail}, card)
		}
		if a := fx.Approval; a != nil {
			c, err := factoryd.RequestCase(*a)
			if err != nil {
				t.Fatal(err)
			}
			c.Facts["kind"] = a.Kind
			add(factoryd.Event{Kind: factoryd.EventApprovalRequested, Detail: c.Facts}, card)
		}
		if card.Kind != "agent" {
			continue
		}
		add(factoryd.Event{Kind: factoryd.EventQuestionWithdrawn, Detail: map[string]any{"reason": "the card ended"}}, card)
		add(factoryd.Event{Kind: factoryd.EventQuestionWithdrawn, Detail: map[string]any{"reason": "the wait ended", "case_kind": string(factoryd.CaseMonitorFlag)}}, card)
		add(factoryd.Event{Kind: factoryd.EventTaskEnded, Detail: map[string]any{"state": "failed", "reason": "checks: the checks failed on c1."}}, card)
		done := card
		done.Parent = ""
		add(factoryd.Event{Kind: factoryd.EventTaskEnded, Detail: map[string]any{"state": "done"}}, done)
		done.Status = "merged"
		add(factoryd.Event{Kind: factoryd.EventPullRequestMerged, Detail: map[string]any{"chain": "running"}}, done)
		add(factoryd.Event{Kind: factoryd.EventPullRequestMerged, Detail: map[string]any{"chain": "failure", "repair": float64(2190)}}, done)
		add(factoryd.Event{Kind: factoryd.EventAllChildrenDone, Detail: map[string]any{"cards": float64(5)}}, card)
		for _, kind := range []string{factoryd.EventCardFiled, factoryd.EventCardRetried, factoryd.EventSessionGranted} {
			add(factoryd.Event{Kind: kind, Detail: map[string]any{"autonomy": "assisted", "decider": "agent"}}, card)
		}
	}
	add(factoryd.Event{Kind: factoryd.EventChatBudgetSpent, Detail: map[string]any{"budget_eur": 5.0, "spent_eur": 5.0}}, factoryd.Task{ID: "t-chat-code-2026-11", Kind: factoryd.KindChat})
	add(factoryd.Event{Kind: factoryd.EventAutonomyChanged, Actor: "sebastian", Detail: map[string]any{"from": "manual", "to": "assisted"}}, factoryd.Task{})
	add(factoryd.Event{Kind: factoryd.EventAutonomyChanged, Actor: "sebastian", Detail: map[string]any{"from": "assisted", "to": "assisted", "queue_cap": 5.0}}, factoryd.Task{})
	add(factoryd.Event{Kind: factoryd.EventRoutineRan, Detail: map[string]any{"poke": "Weekly bump: 1 card."}}, factoryd.Task{})
	seen := map[string]bool{}
	for _, x := range events {
		m, ok := factoryd.PokeOf(x.e, x.card, factoryd.Task{})
		if !ok {
			continue
		}
		seen[x.e.Kind] = true
		// the card's title and the why its writer filed pass their own doors
		own := m.Content
		for _, writer := range []any{factoryd.CardTitle(x.card), x.e.Detail["why"]} {
			if w, _ := writer.(string); w != "" {
				own = strings.ReplaceAll(own, w, "")
			}
		}
		if faults := writingguide.Lint(own); len(faults) > 0 {
			t.Errorf("%s of %s: %q: %v", x.e.Kind, x.card.ID, m.Content, faults)
		}
		bound := 160
		if m.Kind == factoryd.MessageEscalation || x.e.Kind == factoryd.EventChatBudgetSpent {
			bound = 280
		}
		if n := utf8.RuneCountInString(m.Content); n > bound || strings.Contains(m.Content, "\n") {
			t.Errorf("%s of %s: %d characters over %d lines, want at most %d on one line: %q", x.e.Kind, x.card.ID, n, strings.Count(m.Content, "\n")+1, bound, m.Content)
		}
		// the factory's own buttons in the inbox take the vocabulary's labels;
		// an ask's choices are its agent's or its card's words
		for _, c := range m.Choices {
			if x.e.Kind != factoryd.EventQuestionAsked && !slices.ContainsFunc(factoryd.Vocabulary, func(a factoryd.Answer) bool { return a.Label == c }) {
				t.Errorf("%s of %s offers %q, no label of the vocabulary", x.e.Kind, x.card.ID, c)
			}
		}
	}
	for _, kind := range factoryd.PokeKinds {
		if !seen[kind] {
			t.Errorf("no gallery fact writes a %s poke", kind)
		}
	}
}
