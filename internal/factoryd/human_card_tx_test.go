package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
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
// failed attempt filed stays open for the retry to take on.
func TestEscalateToHumanFilesNoCardWhenItsTransactionFails(t *testing.T) {
	f := &chainForgejo{}
	store := newFakeStore()
	store.recordFails = map[string]int{EventQuestionAsked: 1}
	q := HumanQuestion{Ask: "Handle it?", Why: "w", Info: "i"}
	_, err := EscalateToHuman(context.Background(), store, f.srv(t), Repo{Owner: "sf", Name: "r"}, q, "factoryd", Task{Source: SourceAlert})
	noHumanCard(t, store, err)
	if len(f.issues) != 1 || len(f.closed) != 0 {
		t.Fatalf("issues %d, closed %q; want the one issue left open", len(f.issues), f.closed)
	}
	title, _, _ := strings.Cut(f.issues[0], "\n\n")
	f.open = map[string][]map[string]any{"sf/r": {{"number": 31, "title": title, "user": map[string]any{"login": "factory-agent"}}}}
	if _, err := EscalateToHuman(context.Background(), store, f.srv(t), Repo{Owner: "sf", Name: "r"}, q, "factoryd", Task{Source: SourceAlert}); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if card := oneHumanCardWithItsAsk(t, store); len(f.issues) != 1 || card.IssueNumber != 31 {
		t.Fatalf("issues %d, card %+v; want the retry on the one open issue #31", len(f.issues), card)
	}
}

// fileHumanTask, the month cap's and the routine's card: the check that
// files it runs again on its next pass. The issue the failed attempt filed
// stays open, and the retry takes it on.
func TestFileHumanTaskFilesNoCardWhenItsTransactionFails(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	alarm, fj := monthAlarm(t, store, 50)
	store.recordFails = map[string]int{EventQuestionAsked: 1}
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

// A failed card write leaves the human card's issue open, and the retry,
// which has a new task id, takes it on: one open issue, held by the new card.
func TestHumanCardRetryAfterStoreFailureKeepsOneOpenIssue(t *testing.T) {
	q := HumanQuestion{Ask: "Raise the cap?", Why: "the cap is reached", Info: "a raise is a commit"}
	callers := map[string]func(ctx context.Context, store *fakeStore, alarm *MonthAlarm, id string) error{
		"fileHumanTask": func(ctx context.Context, store *fakeStore, alarm *MonthAlarm, id string) error {
			_, _, err := fileHumanTask(ctx, store, alarm.Forgejo, nil, alarm.Repo, q, nil, Task{ID: id, Kind: "human", Source: SourceMonthCap}, "")
			return err
		},
		"EscalateToHumanWith": func(ctx context.Context, store *fakeStore, alarm *MonthAlarm, id string) error {
			_, err := EscalateToHumanWith(ctx, store, alarm.Forgejo, alarm.Repo, q, "factoryd", Task{ID: id, Source: SourceMonthCap}, nil)
			return err
		},
	}
	for name, file := range callers {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := newFakeStore()
			alarm, fj := monthAlarm(t, store, 50)
			store.recordFails = map[string]int{EventQuestionAsked: 1}
			noHumanCard(t, store, file(ctx, store, alarm, "t-1"))
			if len(fj.issues) != 1 || len(fj.closed) != 0 {
				t.Fatalf("after the failure: issues %d, closed %v; want one issue, open", len(fj.issues), fj.closed)
			}
			if err := file(ctx, store, alarm, "t-2"); err != nil {
				t.Fatalf("the retry: %v", err)
			}
			card := oneHumanCardWithItsAsk(t, store)
			if card.ID != "t-2" || card.IssueNumber != 41 || len(fj.issues) != 1 || len(fj.closed) != 0 {
				t.Fatalf("card %+v, issues %d, closed %v; want t-2 on the one open issue #41", card, len(fj.issues), fj.closed)
			}
		})
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
	if touched := issueRequests(*requests, 7); len(touched) != 0 {
		t.Fatalf("the person's issue was touched: %q", touched)
	}
	if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != 200 {
		t.Fatalf("the redelivery answered %d", rr.Code)
	}
	if card := oneHumanCardWithItsAsk(t, store); card.IssueNumber != 7 {
		t.Fatalf("card %+v, want it on issue 7", card)
	}
	// the card's state goes onto the issue once the card exists
	if touched := issueRequests(*requests, 7); !slices.Contains(touched, "/api/v1/repos/sf/ai-software-factory-code/issues/7/labels") {
		t.Fatalf("requests to the issue after the redelivery %q, want its state labels", touched)
	}
}

// issueRequests are the requests to issue n's path or any path below it:
// its labels, comments, state and the rest.
func issueRequests(requests []string, n int64) []string {
	var out []string
	for _, p := range requests {
		if strings.Contains(p+"/", fmt.Sprintf("/issues/%d/", n)) {
			out = append(out, p)
		}
	}
	return out
}

// humanLaneWithoutAgents: a queued agent card of a project on agents none
// moves to the human lane with its ask. A failed transaction leaves the card
// queued and askless; the next redelivery or heal pass moves it.
func TestHumanLaneMoveLeavesTheQueuedCardWhenItsTransactionFails(t *testing.T) {
	in, store, requests := newTestIntake(t)
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
	if touched := issueRequests(*requests, 7); len(touched) != 0 {
		t.Fatalf("the person's issue was touched: %q", touched)
	}
	if moved, err := in.humanLaneWithoutAgents(context.Background(), store.tasks["t-q"]); err != nil || !moved {
		t.Fatalf("the retry: moved %v, err %v", moved, err)
	}
	if got := store.tasks["t-q"]; got.Kind != "human" || got.Status != "waiting-human" || len(store.askedRows("t-q")) != 1 {
		t.Fatalf("card %+v, asks %+v; want it moved with its ask", got, store.askedRows("t-q"))
	}
	if touched := issueRequests(*requests, 7); !slices.Contains(touched, "/api/v1/repos/sf/ai-software-factory-code/issues/7/labels") {
		t.Fatalf("requests to the issue after the move %q, want its state labels", touched)
	}
}

// A plan's human card (fileChildren): its issue is filed first, and the card
// and its ask land with the issue's number. The plan task's filing activity
// runs again after a failed transaction, and its retry takes on the issue
// the failed attempt filed, by the card's marker.
func TestPlanHumanCardFilesNoCardWhenItsTransactionFails(t *testing.T) {
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
	if cards := cardsByKey(t, store); len(cards) != 0 || len(fj.issues) != base+1 {
		t.Fatalf("cards %+v, issues %d; want no card row and the card's issue filed", cards, len(fj.issues)-base)
	}
	if _, err := r.fileChildren(ctx, plan, repo, pinned, "spec", "plan #1", p, FactoryDecider, true, 1, "", nil); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	card := cardsByKey(t, store)[p.Cards[0].Key]
	var asks []Event
	for _, e := range store.events {
		if e.TaskID == card.ID && e.Kind == EventQuestionAsked {
			asks = append(asks, e)
		}
	}
	if card.Kind != "human" || card.Status != "waiting-human" || card.IssueNumber == 0 || len(asks) != 1 || len(fj.issues) != base+1 {
		t.Fatalf("card %+v, asks %+v, %d issues; want one human card with its ask on its one issue", card, asks, len(fj.issues)-base)
	}
	if asks[0].IssueNumber != card.IssueNumber {
		t.Fatalf("the ask row is on #%d, want the card's #%d", asks[0].IssueNumber, card.IssueNumber)
	}
}

// Two overlapping attempts of one card id: the second takes on the issue the
// first is filing, the first stores its card on it, and the second's
// refusal leaves that issue, which the live card holds, open.
func TestFileHumanCardLeavesTheIssueOfTheLiveCardOpen(t *testing.T) {
	f := &chainForgejo{open: map[string][]map[string]any{"sf/r": {{"number": 31, "title": "config", "user": map[string]any{"login": "factory-agent"}}}}}
	store := newFakeStore()
	store.tasks["t-h"] = Task{ID: "t-h", Kind: "human", Status: "waiting-human", Repository: "sf/r", IssueNumber: 31}
	_, _, err := FileHumanCard(context.Background(), notHeldStore{store}, f.srv(t), Repo{Owner: "sf", Name: "r"}, "config", "body", []string{"human"},
		Task{ID: "t-h", Source: SourceConfig}, HumanAsk{Q: HumanQuestion{Ask: "Config?", Why: "w", Info: "i"}, By: "factoryd", Asker: SourceConfig, Via: ViaFactoryd})
	const why = "card t-h stands already (#31, waiting-human), so this second filing of it is refused: the card's ask is on #31"
	if err == nil || err.Error() != why {
		t.Fatalf("FileHumanCard = %v, want %q", err, why)
	}
	if len(f.issues) != 0 || len(f.closed) != 0 || len(f.comments) != 0 {
		t.Fatalf("issues %d, closed %q, comments %q; want the live card's #31 untouched", len(f.issues), f.closed, f.comments)
	}
}

// The reuse finds the bot's open issue by a search on its title, and a failed
// search ends the filing with an error that names the search and the action.
func TestFileHumanCardSearchesForTheStrandedIssueAndNamesAFailedSearch(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/sf/r/issues" {
			query = r.URL.Query().Get("state") + " " + r.URL.Query().Get("created_by") + " " + r.URL.Query().Get("q")
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		t.Errorf("unexpected call %s %s", r.Method, r.URL)
	}))
	t.Cleanup(srv.Close)
	f := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	_, _, err := FileHumanCard(context.Background(), newFakeStore(), f, Repo{Owner: "sf", Name: "r"}, "config", "body", []string{"human"},
		Task{ID: "t-h", Source: SourceConfig}, HumanAsk{Q: HumanQuestion{Ask: "Config?", Why: "w", Info: "i"}, By: "factoryd", Asker: SourceConfig, Via: ViaFactoryd})
	if query != "open factory-agent config" {
		t.Errorf("the search asked %q, want open issues of factory-agent matching config", query)
	}
	const want = `search sf/r's open issues for a stranded one titled "config": `
	if err == nil || !strings.HasPrefix(err.Error(), want) || !strings.HasSuffix(err.Error(), "the card is not filed, so file it again") {
		t.Fatalf("FileHumanCard = %v, want an error naming the search and the action", err)
	}
}

// searchStub serves the search's pages from pages, an empty page past them,
// and counts the pages asked.
func searchStub(t *testing.T, pages [][]map[string]any) (*Forgejo, *[]string) {
	t.Helper()
	asked := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		*asked = append(*asked, page)
		n, _ := strconv.Atoi(page)
		out := []map[string]any{}
		if n >= 1 && n <= len(pages) {
			out = pages[n-1]
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}, asked
}

func issueOf(n int, title, poster string) map[string]any {
	return map[string]any{"number": n, "title": title, "user": map[string]any{"login": poster}}
}

// Forgejo's search matches words, so only the bot's issue of exactly the
// title is taken on: a longer or a shorter title and another poster's
// issue of the title are not.
func TestStrandedIssueTakesOnlyTheBotsIssueOfTheExactTitle(t *testing.T) {
	repo := Repo{Owner: "sf", Name: "r"}
	for name, tc := range map[string]struct {
		pages [][]map[string]any
		want  int64
	}{
		"the exact title by the bot among the others": {[][]map[string]any{{
			issueOf(5, "config rejected", "factory-agent"), issueOf(6, "config", "someone"), issueOf(7, "config", "factory-agent"), issueOf(8, "con", "factory-agent")}}, 7},
		"a longer title":  {[][]map[string]any{{issueOf(5, "config rejected", "factory-agent")}}, 0},
		"a shorter title": {[][]map[string]any{{issueOf(5, "con", "factory-agent")}}, 0},
		"another poster":  {[][]map[string]any{{issueOf(6, "config", "someone")}}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := searchStub(t, tc.pages)
			got, err := strandedIssue(context.Background(), newFakeStore(), f, repo, "config")
			if err != nil || got != tc.want {
				t.Fatalf("strandedIssue = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

// The search reads at most 2 pages of 50; past them with no exact match the
// filing files a new issue and one line names the title, the bound and the
// count read.
func TestStrandedIssueReadsTwoPagesAtMost(t *testing.T) {
	var pages [][]map[string]any
	for p := range 3 {
		var page []map[string]any
		for i := range 50 {
			page = append(page, issueOf(100+p*50+i, fmt.Sprintf("config %d", p*50+i), "factory-agent"))
		}
		pages = append(pages, page)
	}
	f, asked := searchStub(t, pages)
	logs := captureLog(t)
	got, err := strandedIssue(context.Background(), newFakeStore(), f, Repo{Owner: "sf", Name: "r"}, "config")
	if err != nil || got != 0 {
		t.Fatalf("strandedIssue = %d, %v; want none", got, err)
	}
	if !slices.Equal(*asked, []string{"1", "2"}) {
		t.Fatalf("pages asked %q, want 1 and 2", *asked)
	}
	const want = `the search of sf/r for "config" read 100 issues in its first 2 pages without the exact title; a new issue is filed`
	if !strings.Contains(logs.String(), want) || strings.Count(logs.String(), "the search of") != 1 {
		t.Fatalf("log %q, want one line %q", logs.String(), want)
	}
}

// notHeldStore answers TaskByIssue as the read did before the first attempt
// stored its card.
type notHeldStore struct{ *fakeStore }

func (notHeldStore) TaskByIssue(context.Context, string, int64) (Task, bool, error) {
	return Task{}, false, nil
}

// FileHumanCard for an id whose card stands already: the filing is refused
// by that card's name, its issue is closed with a comment naming the card,
// and the card that stands keeps its issue and gets no second ask.
func TestFileHumanCardRefusesTheIdOfACardThatStands(t *testing.T) {
	f := &chainForgejo{}
	store := newFakeStore()
	store.tasks["t-h"] = Task{ID: "t-h", Kind: "human", Status: "waiting-human", Repository: "sf/r", IssueNumber: 7}
	_, _, err := FileHumanCard(context.Background(), store, f.srv(t), Repo{Owner: "sf", Name: "r"}, "config", "body", []string{"human"},
		Task{ID: "t-h", Source: SourceConfig}, HumanAsk{Q: HumanQuestion{Ask: "Config?", Why: "w", Info: "i"}, By: "factoryd", Asker: SourceConfig, Via: ViaFactoryd})
	const why = "card t-h stands already (#7, waiting-human), so this second filing of it is refused and its issue #31 closed: the card's ask is on #7"
	if err == nil || err.Error() != why {
		t.Fatalf("FileHumanCard = %v, want %q", err, why)
	}
	if len(f.issues) != 1 || !slices.Equal(f.closed, []string{"sf/r#31"}) || !slices.Equal(f.comments, []string{"sf/r#31 factoryd: " + why}) {
		t.Fatalf("issues %d, closed %q, comments %q; want #31 closed with the comment naming t-h", len(f.issues), f.closed, f.comments)
	}
	if got := store.tasks["t-h"]; got.IssueNumber != 7 || len(store.askedRows("t-h")) != 0 {
		t.Fatalf("the card that stands %+v, asks %+v; want it on #7 with no second ask", got, store.askedRows("t-h"))
	}
}

// fileHumanTask takes on its stranded issue for an id whose card stands:
// the filing is refused by that card's name, the issue it took on is closed
// with a comment naming the card, no second card_created is recorded, and
// the card that stands keeps its issue and gets no second ask.
func TestFileHumanTaskRefusesTheIdOfACardThatStandsOnTheIssueItTakesOn(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	alarm, fj := monthAlarm(t, store, 50)
	q := HumanQuestion{Ask: "Raise the cap?", Why: "the cap is reached", Info: "a raise is a commit"}
	// a failed transaction leaves issue #41 open
	store.recordFails = map[string]int{EventQuestionAsked: 1}
	if _, _, err := fileHumanTask(ctx, store, alarm.Forgejo, nil, alarm.Repo, q, nil, Task{ID: "t-1", Kind: "human", Source: SourceMonthCap}, ""); err == nil {
		t.Fatal("the failed transaction filed a card")
	}
	store.tasks["t-1"] = Task{ID: "t-1", Kind: "human", Status: "waiting-human", Repository: "sf/ai-software-factory-infra", IssueNumber: 7}
	rows := &fakeEventRecorder{}
	tl := &Timeline{Objects: &fakeObjectStore{}, Store: store, Events: rows}
	_, _, err := fileHumanTask(ctx, store, alarm.Forgejo, tl, alarm.Repo, q, nil, Task{ID: "t-1", Kind: "human", Source: SourceMonthCap}, "")
	const why = "card t-1 stands already (#7, waiting-human), so this second filing of it is refused and its issue #41 closed: the card's ask is on #7"
	if err == nil || err.Error() != why {
		t.Fatalf("fileHumanTask = %v, want %q", err, why)
	}
	if len(fj.issues) != 1 || !slices.Equal(fj.closed, []int64{41}) || !slices.Equal(fj.comments, []string{"#41 factoryd: " + why}) {
		t.Fatalf("issues %d, closed %v, comments %q; want #41 taken on, then closed with the comment naming t-1", len(fj.issues), fj.closed, fj.comments)
	}
	for _, e := range rows.rows {
		if e.Kind == EventCardCreated {
			t.Fatalf("card_created %+v recorded for the refused filing", e)
		}
	}
	if got := store.tasks["t-1"]; got.IssueNumber != 7 || len(store.askedRows("t-1")) != 0 {
		t.Fatalf("the card that stands %+v, asks %+v; want it on #7 with no second ask", got, store.askedRows("t-1"))
	}
}

// FileTask files no human card: one asks for its ask through FileHumanCard,
// and nothing is filed.
func TestFileTaskRefusesAHumanCard(t *testing.T) {
	f := &chainForgejo{}
	store := newFakeStore()
	_, _, err := FileTask(context.Background(), store, f.srv(t), Repo{Owner: "sf", Name: "r"}, "config", "body", nil, Task{ID: "t-h", Kind: "human"})
	if err == nil || err.Error() != "task t-h is a human card: FileHumanCard files it with its ask" {
		t.Fatalf("FileTask = %v, want the refusal naming FileHumanCard", err)
	}
	if len(f.issues) != 0 || len(store.tasks) != 0 {
		t.Fatalf("issues %d, tasks %+v; want nothing filed", len(f.issues), store.tasks)
	}
}
