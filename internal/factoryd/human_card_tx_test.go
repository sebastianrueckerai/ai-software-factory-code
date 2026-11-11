package factoryd

import (
	"context"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A human card never exists without its ask row: its row and its
// question_asked row land in one store transaction, so a failing one leaves
// neither, the issue of a person is left as it is, and the site's own retry
// files the card whole. The store's failing transaction is a failing ask
// write (fakeStore.recordFails), which the fake undoes as Postgres rolls back.

func noHumanCard(t *testing.T, store *fakeStore, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "the store is down") {
		t.Fatalf("err = %v, want the failed transaction's error", err)
	}
	for id, tk := range store.tasks {
		t.Fatalf("task %s (%s) stands after a failed transaction, want no card row", id, tk.Status)
	}
	for _, e := range store.events {
		if e.Kind == EventQuestionAsked {
			t.Fatalf("ask row %+v stands after a failed transaction", e)
		}
	}
}

func oneHumanCardWithItsAsk(t *testing.T, store *fakeStore) Task {
	t.Helper()
	var cards []Task
	for _, tk := range store.tasks {
		cards = append(cards, tk)
	}
	asks := 0
	for _, e := range store.events {
		if e.Kind == EventQuestionAsked {
			asks++
		}
	}
	if len(cards) != 1 || cards[0].Status != "waiting-human" || asks != 1 {
		t.Fatalf("cards %+v and %d ask rows, want one waiting-human card with its one ask", cards, asks)
	}
	return cards[0]
}

// EscalateToHuman: the filing activity's retry runs it again. The issue the
// failed attempt filed is the factory's own, and FileTask closes it.
func TestEscalateToHumanFilesNoCardWhenItsTransactionFails(t *testing.T) {
	f := &chainForgejo{}
	store := newFakeStore()
	store.recordFails = map[string]int{EventQuestionAsked: 1}
	q := HumanQuestion{Ask: "Handle it?", Why: "w", Info: "i"}
	_, err := EscalateToHuman(context.Background(), store, f.srv(t), Repo{Owner: "sf", Name: "r"}, q, "factoryd", Task{Source: SourceAlert})
	noHumanCard(t, store, err)
	if len(f.issues) != 1 || len(f.closed) != 1 {
		t.Fatalf("issues %d, closed %q; want the factory's own issue closed", len(f.issues), f.closed)
	}
	if _, err := EscalateToHuman(context.Background(), store, f.srv(t), Repo{Owner: "sf", Name: "r"}, q, "factoryd", Task{Source: SourceAlert}); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	oneHumanCardWithItsAsk(t, store)
}

// fileHumanTask, the month cap's and the routine's card: the check that
// files it runs again on its next pass. The issue the failed attempt filed
// stays open here (its close failed too), and the retry takes it on.
func TestFileHumanTaskFilesNoCardWhenItsTransactionFails(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	alarm, fj := monthAlarm(t, store, 50)
	store.recordFails = map[string]int{EventQuestionAsked: 1}
	fj.failClose = 1
	q := HumanQuestion{Ask: "Raise the cap?", Why: "the cap is reached", Info: "a raise is a commit"}
	file := func() error {
		_, _, err := fileHumanTask(ctx, store, alarm.Forgejo, nil, alarm.Repo, q, nil, Task{ID: "t-1", Kind: "human", Source: SourceMonthCap}, "")
		return err
	}
	noHumanCard(t, store, file())
	if err := file(); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if card := oneHumanCardWithItsAsk(t, store); len(fj.issues) != 1 || card.IssueNumber == 0 {
		t.Fatalf("issues %d, card %+v; want the stranded issue taken on, not filed again", len(fj.issues), card)
	}
}

// The webhook intake of a human card: the failed delivery answers an error,
// so Forgejo delivers it again, and the redelivery files the card whole. The
// issue is the person's: nothing writes to it before the card exists.
func TestIntakeFilesNoHumanCardWhenItsTransactionFails(t *testing.T) {
	in, store, requests := newTestIntake(t)
	none := seedProject("sf/ai-software-factory-code")
	none.Agents = "none"
	store.projects = []Project{none}
	store.recordFails = map[string]int{EventQuestionAsked: 1}
	body := issueOpenedBody(7, "sebastian", []string{"human"}, "Decide the colour", "red or blue")
	if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code == 200 {
		t.Fatalf("the failed delivery answered %d, want an error so Forgejo delivers it again", rr.Code)
	}
	for _, e := range store.events {
		if e.Kind == EventQuestionAsked {
			t.Fatalf("ask row %+v stands", e)
		}
	}
	if len(store.tasks) != 0 {
		t.Fatalf("tasks %+v, want no card row", store.tasks)
	}
	for _, p := range *requests {
		if strings.HasSuffix(p, "/issues/7") {
			t.Fatalf("the person's issue was touched: %q", *requests)
		}
	}
	if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != 200 {
		t.Fatalf("the redelivery answered %d", rr.Code)
	}
	if card := oneHumanCardWithItsAsk(t, store); card.IssueNumber != 7 {
		t.Fatalf("card %+v, want it on issue 7", card)
	}
}

// humanLaneWithoutAgents: a queued agent card of a project on agents none
// moves to the human lane with its ask. A failed transaction leaves the card
// queued and askless; the next redelivery or heal pass moves it.
func TestHumanLaneMoveLeavesTheQueuedCardWhenItsTransactionFails(t *testing.T) {
	in, store, _ := newTestIntake(t)
	none := seedProject("sf/ai-software-factory-code")
	none.Agents = "none"
	store.projects = []Project{none}
	store.tasks["t-q"] = Task{ID: "t-q", Repository: "sf/ai-software-factory-code", Status: "queued", Kind: "agent", IssueNumber: 7,
		Objective: "Add the runner\n\nobjective body", Author: "sebastian", Source: "webhook", Track: "direct"}
	store.recordFails = map[string]int{EventQuestionAsked: 1}
	moved, err := in.humanLaneWithoutAgents(context.Background(), store.tasks["t-q"])
	if err == nil || moved {
		t.Fatalf("moved %v, err %v; want the failed transaction's error", moved, err)
	}
	if got := store.tasks["t-q"]; got.Kind != "agent" || got.Status != "queued" || len(store.askedRows("t-q")) != 0 {
		t.Fatalf("card %+v, asks %+v; want it queued and askless", got, store.askedRows("t-q"))
	}
	if moved, err := in.humanLaneWithoutAgents(context.Background(), store.tasks["t-q"]); err != nil || !moved {
		t.Fatalf("the retry: moved %v, err %v", moved, err)
	}
	if got := store.tasks["t-q"]; got.Kind != "human" || got.Status != "waiting-human" || len(store.askedRows("t-q")) != 1 {
		t.Fatalf("card %+v, asks %+v; want it moved with its ask", got, store.askedRows("t-q"))
	}
}

// A plan's human card (fileChildren): the plan task's filing activity runs
// again, and the card, the issue and the ask come in that retry. The failed
// attempt filed no issue.
func TestPlanHumanCardFilesNothingWhenItsTransactionFails(t *testing.T) {
	ctx := context.Background()
	r, store, _, fj := newPlanRunner(t, testPlan())
	pinned := fixtureVersion(t)
	r.cfg.Policy = func() policy.Version { return pinned }
	p := testPlan()
	p.Cards = p.Cards[:1]
	p.Cards[0].Directories = []string{".github/workflows"}
	plan, repo, err := r.task(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	store.recordFails = map[string]int{EventQuestionAsked: 1}
	base := len(fj.issues)
	if _, err := r.fileChildren(ctx, plan, repo, pinned, "spec", "plan #1", p, FactoryDecider, true, 1, "", nil); err == nil {
		t.Fatal("fileChildren succeeded, want the failed transaction's error")
	}
	if cards := cardsByKey(t, store); len(cards) != 0 || len(fj.issues) != base {
		t.Fatalf("cards %+v, issues %d; want no card row and no new issue", cards, len(fj.issues)-base)
	}
	if _, err := r.fileChildren(ctx, plan, repo, pinned, "spec", "plan #1", p, FactoryDecider, true, 1, "", nil); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	card := cardsByKey(t, store)[p.Cards[0].Key]
	asks := 0
	for _, e := range store.events {
		if e.TaskID == card.ID && e.Kind == EventQuestionAsked {
			asks++
		}
	}
	if card.Kind != "human" || card.Status != "waiting-human" || card.IssueNumber == 0 || asks != 1 || len(fj.issues) != base+1 {
		t.Fatalf("card %+v, %d asks, %d issues; want one human card with its ask on its one issue", card, asks, len(fj.issues)-base)
	}
}
