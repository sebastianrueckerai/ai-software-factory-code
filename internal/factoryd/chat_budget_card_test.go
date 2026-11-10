package factoryd

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"
)

// Hand-derived for the chat task t-chat-linkshort-2026-11 at 50 EUR: the
// stop files one human card on the project's repository, its ask naming
// the budget "50.00 EUR" and the month, its why saying rung 2 answers no
// stopped card until a raise or the next month, its info the action. A
// second stop at the same budget files none. A raise to 60 closes the card
// with the budget named; so does the turn of the UTC month to 2026-12
// under 50; a card of the unraised budget in its own month stays.
func TestChatBudgetCards(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	forgejo, fj := newAlertForgejo(t)
	cards := &ChatBudgetCards{Store: store, Forgejo: forgejo}
	p := Project{Name: "linkshort", Repository: "sf/linkshort"}
	chat := Task{ID: "t-chat-linkshort-2026-11", Kind: KindChat, Status: "running", BudgetEUR: 50}
	if err := store.CreateTask(ctx, chat); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := cards.File(ctx, p, chat); err != nil {
			t.Fatal(err)
		}
	}
	if len(fj.issues) != 1 || fj.repos[0] != "sf/linkshort" {
		t.Fatalf("issues %q on %q, want one on sf/linkshort", fj.issues, fj.repos)
	}
	open, _ := store.ListTasksByStatus(ctx, "waiting-human")
	if len(open) != 1 || open[0].Source != SourceChatBudget || open[0].Kind != "human" || open[0].Parent != chat.ID || open[0].BudgetEUR != 50 {
		t.Fatalf("cards = %+v, want one human card of the chat task at 50", open)
	}
	events, _ := store.IntakeEvents(ctx, open[0].ID)
	want := HumanQuestion{
		Ask:     "The chat budget of 50.00 EUR for 2026-11 is spent: raise it?",
		Why:     "Rung 2 of the ladder answers no stopped card through the chat until the chat budget is raised or the UTC month turns, so every stop reaches Needs you.",
		Info:    "The action: raise the chat budget, with Fund and go on at the stop in the chat. The raise ends this card.",
		Choices: []QuestionChoice{{AnswerClose, "Decided, close"}},
	}
	if len(events) != 1 || events[0].Question == nil || !reflect.DeepEqual(*events[0].Question, want) {
		t.Fatalf("the ask = %+v, want %+v", events, want)
	}

	if err := cards.Check(ctx, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.Task(ctx, open[0].ID); got.Status != "waiting-human" {
		t.Fatalf("under the same budget in its month the card is %s, want waiting-human", got.Status)
	}
	chat.BudgetEUR = 60
	store.tasks[chat.ID] = chat
	if err := cards.Check(ctx, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.Task(ctx, open[0].ID); got.Status != "done" {
		t.Fatalf("after the raise the card is %s, want done", got.Status)
	}
	if got := closeComments(t, store, open[0].ID); !slices.Equal(got, []string{"The chat budget is 60.00 EUR for 2026-11: the card closes."}) {
		t.Fatalf("close comments = %q", got)
	}

	chat.BudgetEUR = 60
	if err := cards.File(ctx, p, chat); err != nil {
		t.Fatal(err)
	}
	second, _ := store.ListTasksByStatus(ctx, "waiting-human")
	if len(second) != 1 || second[0].ID == open[0].ID || second[0].BudgetEUR != 60 {
		t.Fatalf("after the stop at 60 the cards = %+v, want a new card at 60", second)
	}
	if err := cards.Check(ctx, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if got := closeComments(t, store, second[0].ID); !slices.Equal(got, []string{"The UTC month 2026-11 ended: the card closes."}) {
		t.Fatalf("close comments = %q", got)
	}
}
