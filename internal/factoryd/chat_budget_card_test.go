package factoryd

import (
	"context"
	"reflect"
	"slices"
	"sync"
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
	cards := &ChatBudgetCards{Store: store, Forgejo: forgejo, Timeline: &Timeline{Objects: &fakeObjectStore{}, Store: store, Events: store}}
	p := Project{Name: "linkshort", Repository: "sf/linkshort"}
	chat := Task{ID: "t-chat-linkshort-2026-11", Kind: KindChat, Status: "running", BudgetEUR: 50, SpentEUR: 49.96}
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
	asked := askedQuestion(t, store, open[0].ID)
	want := HumanQuestion{
		Ask:     "The judgment budget of 50.00 EUR for 2026-11 is spent: raise it?",
		Why:     "The project spent 49.96 EUR of its judgment budget of 50.00 EUR in 2026-11. The judgment budget pays the chat, rung 1, rung 2 and the check-in's judgment. Until it is raised or the UTC month turns, no rung judges a stopped card, so every stop reaches Needs you unjudged.",
		Info:    "The action: raise the judgment budget on the project screen, or press Fund and go on in the chat thread that hit the stop. Only a human who approves the project can do either. The raise ends this card.",
		Choices: []QuestionChoice{{AnswerClose, "Decided, close"}},
	}
	if !reflect.DeepEqual(asked, want) {
		t.Fatalf("the ask = %+v, want %+v", asked, want)
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
	if got := closeComments(t, store, open[0].ID); !slices.Equal(got, []string{"The judgment budget is 60.00 EUR for 2026-11: the card closes."}) {
		t.Fatalf("close comments = %q", got)
	}

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

// Two stops at the same moment file one card.
func TestChatBudgetCardsFileOnceUnderRace(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	forgejo, fj := newAlertForgejo(t)
	cards := &ChatBudgetCards{Store: store, Forgejo: forgejo}
	p := Project{Name: "linkshort", Repository: "sf/linkshort"}
	chat := Task{ID: "t-chat-linkshort-2026-11", Kind: KindChat, Status: "running", BudgetEUR: 50}
	if err := store.CreateTask(ctx, chat); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := cards.File(ctx, p, chat); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	open, _ := store.ListTasksByStatus(ctx, "waiting-human")
	if len(open) != 1 || len(fj.issues) != 1 {
		t.Fatalf("8 stops at once filed %d cards and %d issues, want 1 and 1", len(open), len(fj.issues))
	}
}

// A Forgejo outage at the stop leaves its card to the loop: Check files it
// once Forgejo answers, once, and files none for a card a human closed by
// hand, nor for a chat task whose raise lifted it above the recorded stop.
func TestChatBudgetCardsLoopFilesAStopWithoutCard(t *testing.T) {
	ctx := context.Background()
	captureLog(t)
	store := newFakeStore()
	forgejo, fj := newAlertForgejo(t)
	cards := &ChatBudgetCards{Store: store, Forgejo: forgejo}
	chat := Task{ID: "t-chat-linkshort-2026-11", Kind: KindChat, Status: "running", Repository: "sf/linkshort", BudgetEUR: 50}
	if err := store.CreateTask(ctx, chat); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if err := cards.Check(ctx, now); err != nil || len(fj.issues) != 0 {
		t.Fatalf("with no stop recorded Check = %v, issues %d, want nil and 0", err, len(fj.issues))
	}
	store.events = append(store.events, Event{TaskID: chat.ID, Kind: EventChatBudgetSpent, Detail: map[string]any{"budget_eur": 50.0}})
	for range 60 {
		store.events = append(store.events, Event{TaskID: chat.ID, Kind: EventChatFunded})
	}
	fj.failNext = 1
	if err := cards.Check(ctx, now); err == nil || len(fj.issues) != 0 {
		t.Fatalf("during the outage Check = %v, issues %d, want an error and 0", err, len(fj.issues))
	}
	for range 2 {
		if err := cards.Check(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	open, _ := store.ListTasksByStatus(ctx, "waiting-human")
	if len(open) != 1 || len(fj.issues) != 1 || open[0].BudgetEUR != 50 || open[0].Parent != chat.ID {
		t.Fatalf("after the outage: cards %+v, issues %d; want one card at 50 of the chat task", open, len(fj.issues))
	}
	if err := store.TransitionTask(ctx, open[0].ID, []string{"waiting-human"}, "done"); err != nil {
		t.Fatal(err)
	}
	if err := cards.Check(ctx, now); err != nil || len(fj.issues) != 1 {
		t.Fatalf("after a human closed it Check = %v, issues %d, want nil and 1", err, len(fj.issues))
	}
}

// A raise that lands between the loop's read of the chat task and File's
// filing leaves no card at the old budget: File reads the task again.
func TestChatBudgetCardsFileNothingAfterARaise(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	forgejo, fj := newAlertForgejo(t)
	cards := &ChatBudgetCards{Store: store, Forgejo: forgejo}
	p := Project{Name: "linkshort", Repository: "sf/linkshort"}
	read := Task{ID: "t-chat-linkshort-2026-11", Kind: KindChat, Status: "running", BudgetEUR: 50}
	raised := read
	raised.BudgetEUR = 60
	if err := store.CreateTask(ctx, raised); err != nil {
		t.Fatal(err)
	}
	if err := cards.File(ctx, p, read); err != nil {
		t.Fatal(err)
	}
	open, _ := store.ListTasksByStatus(ctx, "waiting-human")
	if len(open) != 0 || len(fj.issues) != 0 {
		t.Fatalf("a snapshot at 50 of a chat raised to 60 filed %d cards and %d issues, want none", len(open), len(fj.issues))
	}
}

// The fund a stop suggests carries the chat's daily rate over the last 7
// days, or the month's days so far when fewer, a day at least, to the
// next UTC month, in whole euros and at least 1.
func TestSuggestFund(t *testing.T) {
	for _, tc := range []struct {
		name   string
		spent7 float64
		now    time.Time
		want   float64
	}{
		{"1 a day, 11 days left", 7, time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC), 11},
		{"under 1 euro", 0.5, time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC), 1},
		{"nothing spent", 0, time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC), 1},
		{"the live stop: 5.47 over 7 days, 3 days 1h20 left", 5.47, time.Date(2026, 10, 28, 22, 40, 0, 0, time.UTC), 3},
		{"day 3: 3 over 2 days, 29 days left", 3, time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC), 44},
		{"6 hours in: 2 over a day, 29.75 days left", 2, time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC), 60},
	} {
		if got := SuggestFund(tc.spent7, tc.now); got != tc.want {
			t.Errorf("%s: SuggestFund(%v, %s) = %v, want %v", tc.name, tc.spent7, tc.now, got, tc.want)
		}
	}
}
