package factoryd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// factoryCloses are the words of factoryd's closes of card taskID that had
// no issue.
func factoryCloses(s *fakeStore, taskID string) []string {
	var out []string
	for _, e := range s.intakeEvents {
		if e.TaskID == taskID && e.Kind == IntakeFactoryClosed && e.CommentID == 0 {
			out = append(out, e.Body)
		}
	}
	return out
}

// heldLabels are the labels the held ask of card taskID files its issue
// with at its face, nil when the card holds no held ask.
func heldLabels(s *fakeStore, taskID string) []string {
	for _, e := range s.events {
		if e.TaskID == taskID && e.Kind == EventQuestionAsked && e.Detail["held"] == true {
			return stringsOf(e.Detail["labels"])
		}
	}
	return nil
}

// silentForge is a Forgejo that fails the test on any call.
func silentForge(t *testing.T) *Forgejo {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("forgejo call %s %s, want none", r.Method, r.URL.Path)
		http.Error(w, "no call", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent"}
}

// factoryd's own human card waits out of sight for its face: it is written
// in waiting-agent with no issue and no forge call, and its ask row is
// marked held and carries the filing's labels, so its case opens on the
// rungs, a human card's, where the chat's escalation opens at the human. A
// second filing of its id is refused by name, with no forge call.
func TestFactorydsHumanCardWaitsForItsFace(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	got, err := EscalateToHuman(ctx, store, silentForge(t), Repo{Owner: "sf", Name: "r"}, HumanQuestion{
		Ask: "The chain is pending. Re-run it?", Why: "pending past the bound", Info: "a re-run moves it",
		Choices: []QuestionChoice{{Value: AnswerRerunAgain, Label: "Re-run again"}, {Value: AnswerDismiss, Label: "Dismiss"}},
	}, "the chain poller", Task{ID: "t-held", Source: SourceChainStall, SHA: "c2c2"}, "bug")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "waiting-agent" || got.IssueNumber != 0 || got.Repository != "sf/r" || store.tasks["t-held"].Status != "waiting-agent" {
		t.Fatalf("the card = %+v, stored %+v; want waiting-agent on sf/r with no issue", got, store.tasks["t-held"])
	}
	if len(store.events) != 1 || store.events[0].Detail["held"] != true || !reflect.DeepEqual(store.events[0].Detail["labels"], []string{"human", "bug"}) {
		t.Fatalf("rows = %+v, want one held ask with the labels human and bug", store.events)
	}
	c, err := CaseFromAsk(got, store.events[0], "")
	if err != nil || c.Status != CaseOpen || c.Kind != CaseHumanCard || asksAtOnce(store.events[0], c.Kind) {
		t.Fatalf("its case = %+v, %v; want a human card's case open, which climbs", c, err)
	}
	_, err = EscalateToHuman(ctx, store, silentForge(t), Repo{Owner: "sf", Name: "r"}, HumanQuestion{Ask: "The chain is pending. Re-run it?", Why: "w", Info: "i"},
		"the chain poller", Task{ID: "t-held", Source: SourceChainStall, SHA: "c2c2"})
	if want := "card t-held stands already (waiting-agent), so this second filing of it is refused: its case asks"; err == nil || err.Error() != want || len(store.events) != 1 {
		t.Fatalf("a second filing = %v with %d rows, want %q and no second ask", err, len(store.events), want)
	}
	chatAsk := Event{Kind: EventQuestionAsked, Via: ViaChat, Detail: map[string]any{"ask": "Write the criteria?", "source": "agent"}}
	chat, err := CaseFromAsk(Task{ID: "t-chat", Kind: "human", Status: "waiting-human"}, chatAsk, "")
	if err != nil || chat.Status != CaseOpen || chat.Kind != CaseHumanCard || !asksAtOnce(chatAsk, chat.Kind) {
		t.Fatalf("the chat's case = %+v, %v; want a human card's case open, which reaches the human at its opening", chat, err)
	}
}

// The chat's escalation, the month cap's card, the judgment budget's card,
// the provider freeze's card and the platform deaths' card ask a human at
// their filing; every other source's card waits for its face.
func TestOnlyTheChatAndTheCapsAskAtOnce(t *testing.T) {
	for source, want := range map[string]string{SourceChat: "waiting-human", SourceMonthCap: "waiting-human", SourceChatBudget: "waiting-human", SourceFreezeCard: "waiting-human",
		SourcePlatformDeaths: "waiting-human", SourceChainStall: "waiting-agent", SourceStillRed: "waiting-agent", SourceRoutine: "waiting-agent", SourcePlan: "waiting-agent", "": "waiting-agent"} {
		if got := HumanCardStatus(source); got != want {
			t.Errorf("HumanCardStatus(%q) = %s, want %s", source, got, want)
		}
	}
}

// Rung 2 ends a card that waits for its face only once factoryd reads the
// card's condition gone, from the fact its source's closer ends it on, and
// never parks it out of sight. By hand, for card t-h on sf/r at commit
// c2c2c2c2: each fact that stands refuses close, check_again, hold_wait
// and dismiss with what stands; each that is gone lets them through; a
// source with no check refuses them; a choice that works (rerun_again)
// passes; go, a hand-over to an agent of a card its filer routed to a
// human, is refused; a card at its face and the human's answer meet no
// such gate.
func TestAHeldCardsChoiceEndsItOnlyOnceItsConditionIsGone(t *testing.T) {
	ctx := context.Background()
	up := func(choice string) string {
		return ": hand the case up (`hand_up`) with `" + choice + "` as the recommendation"
	}
	const stands = "card t-h's condition stands: "
	for _, tc := range []struct {
		name   string
		source string
		status string
		rung   int
		choice string
		seed   func(s *fakeStore)
		policy int64
		want   string
	}{
		{"a park", SourceChainStall, "waiting-agent", 2, AnswerPark, nil, 0,
			"card t-h waits out of sight until a human is asked, so `park` would leave it there" + up(AnswerPark)},
		{"a keep waiting", SourceAlert, "waiting-agent", 2, AnswerKeepWaiting, nil, 0,
			"card t-h waits out of sight until a human is asked, so `keep_waiting` would leave it there" + up(AnswerKeepWaiting)},
		{"a stall that stands", SourceChainStall, "waiting-agent", 2, AnswerCheckAgain, func(s *fakeStore) { s.heads = map[string][2]string{"sf/r": {"c2c2c2c2", "stalled"}} }, 0,
			stands + "the chain of sf/r is stalled at c2c2c2c" + up(AnswerCheckAgain)},
		{"a dismiss of a stall that stands", SourceChainStall, "waiting-agent", 2, AnswerDismiss, func(s *fakeStore) { s.heads = map[string][2]string{"sf/r": {"c2c2c2c2", "pending"}} }, 0,
			stands + "the chain of sf/r is pending at c2c2c2c" + up(AnswerDismiss)},
		{"a stall whose head moved on", SourceChainStall, "waiting-agent", 2, AnswerClose, func(s *fakeStore) { s.heads = map[string][2]string{"sf/r": {"c3c3c3c3", "pending"}} }, 0, ""},
		{"a stall whose head concluded", SourceChainStall, "waiting-agent", 2, AnswerClose, func(s *fakeStore) { s.heads = map[string][2]string{"sf/r": {"c2c2c2c2", "success"}} }, 0, ""},
		{"a stall with no head row", SourceChainStall, "waiting-agent", 2, AnswerClose, nil, 0, ""},
		{"a repair that stands", SourceChainRepair, "waiting-agent", 2, AnswerClose, func(s *fakeStore) { s.heads = map[string][2]string{"sf/r": {"c2c2c2c2", "failure"}} }, 0,
			stands + "the chain of sf/r is failure at c2c2c2c" + up(AnswerClose)},
		{"a repair the head made green", SourceChainRepair, "waiting-agent", 2, AnswerClose, func(s *fakeStore) { s.heads = map[string][2]string{"sf/r": {"c2c2c2c2", "success"}} }, 0, ""},
		{"main still red", SourceStillRed, "waiting-agent", 2, AnswerClose, func(s *fakeStore) { s.pauses = map[string]QueuePause{"sf/r": {Project: "sf/r", FixTask: "t-h"}} }, 0,
			stands + "main of sf/r is red and its merge queue paused on it" + up(AnswerClose)},
		{"a re-run card whose pause moved on", SourceRerun, "waiting-agent", 2, AnswerClose, func(s *fakeStore) { s.pauses = map[string]QueuePause{"sf/r": {Project: "sf/r", FixTask: "t-other"}} }, 0, ""},
		{"main green", SourceStillRed, "waiting-agent", 2, AnswerClose, nil, 0, ""},
		{"a refusal no commit fixed", SourceConfig, "waiting-agent", 2, AnswerClose, func(s *fakeStore) {
			s.alerts = []Alert{{Kind: AlertConfigRejected, Fingerprint: "c2c2c2c2", Detail: map[string]any{"current": float64(5)}}}
		}, 5, stands + "no valid commit loaded after c2c2c2c" + up(AnswerClose)},
		{"a refusal a newer version fixed", SourceConfig, "waiting-agent", 2, AnswerClose, func(s *fakeStore) {
			s.alerts = []Alert{{Kind: AlertConfigRejected, Fingerprint: "c2c2c2c2", Detail: map[string]any{"current": float64(5)}}}
		}, 6, ""},
		{"an alert that fires", SourceAlert, "waiting-agent", 2, AnswerClose, func(s *fakeStore) {
			s.alerts = []Alert{{Kind: AlertSigNoz, Fingerprint: "fp", Detail: map[string]any{"starts_at": "2026-11-18T10:00:00Z"}}}
		}, 0, stands + "its alert fires, or is an event no resolve clears" + up(AnswerClose)},
		{"an alert that cleared", SourceAlert, "waiting-agent", 2, AnswerClose, func(s *fakeStore) {
			s.alerts = []Alert{{Kind: AlertSigNoz, Fingerprint: "fp", Detail: map[string]any{"starts_at": "2026-11-18T10:00:00Z", "resolved_at": "2026-11-18T11:00:00Z"}}}
		}, 0, ""},
		{"an event's alert", SourceAlert, "waiting-agent", 2, AnswerClose, func(s *fakeStore) {
			s.alerts = []Alert{{Kind: AlertSigNoz, Fingerprint: "fp", Detail: map[string]any{"starts_at": "2026-11-18T10:00:00Z", "resolved_at": "2026-11-18T11:00:00Z", "event": true}}}
		}, 0, stands + "its alert fires, or is an event no resolve clears" + up(AnswerClose)},
		{"a routine's card", SourceRoutine, "waiting-agent", 2, AnswerClose, nil, 0, "no code checks card t-h's condition" + up(AnswerClose)},
		{"a re-run of a stall that stands", SourceChainStall, "waiting-agent", 2, AnswerRerunAgain, func(s *fakeStore) { s.heads = map[string][2]string{"sf/r": {"c2c2c2c2", "stalled"}} }, 0, ""},
		{"a routine's hand-over", SourceRoutine, "waiting-agent", 2, AnswerGo, nil, 0,
			"card t-h's filer routed it to a human, so only a human hands it to an agent" + up(AnswerGo)},
		{"a person's issue's hand-over", "webhook", "waiting-agent", 2, AnswerGo, nil, 0,
			"card t-h's filer routed it to a human, so only a human hands it to an agent" + up(AnswerGo)},
		{"the human's hand-over after the face", SourceRoutine, "waiting-human", 3, AnswerGo, nil, 0, ""},
		{"a card at its face", SourceRoutine, "waiting-human", 2, AnswerClose, nil, 0, ""},
		{"the human's answer", SourceRoutine, "waiting-agent", 3, AnswerClose, nil, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			if tc.seed != nil {
				tc.seed(store)
			}
			v := policy.Version{ID: tc.policy}
			r := &Runner{store: store, cfg: RunnerConfig{Policy: func() policy.Version { return v }}}
			card := Task{ID: "t-h", Kind: "human", Status: tc.status, Repository: "sf/r", SHA: "c2c2c2c2", Source: tc.source, AlertFingerprint: "fp"}
			if got := r.heldRefusal(ctx, card, ActInput{Rung: tc.rung, Res: Resolution{Action: tc.choice}}); got != tc.want {
				t.Errorf("heldRefusal = %q,\nwant %q", got, tc.want)
			}
		})
	}
}

// factoryd's close of a card with no issue is its record alone: one
// factory_closed row with no issue number, which the heal pass ends the
// card on, and no forge call; a second close records nothing more.
func TestCloseIssueAsFactoryRecordsACardWithNoIssueAlone(t *testing.T) {
	store := newFakeStore()
	for range 2 {
		if err := closeIssueAsFactory(context.Background(), store, silentForge(t), Task{ID: "t-h", Kind: "human", Repository: "sf/r"}, "factoryd: the alert cleared."); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.intakeEvents) != 1 {
		t.Fatalf("intake events = %+v, want one", store.intakeEvents)
	}
	if e := store.intakeEvents[0]; e.TaskID != "t-h" || e.Kind != IntakeFactoryClosed || e.Author != "factoryd" || e.CommentID != 0 || e.Body != "factoryd: the alert cleared." {
		t.Fatalf("the record = %+v, want t-h's factory close with no issue and its words", e)
	}
}

// A face that filed its issue and failed before the card held it left the
// bot's open issue of the card's title: the next face takes it on, labels
// it for the wait on a human and files no second issue.
func TestAFaceTakesOnTheIssueAFailedFaceLeft(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	f := &chainForgejo{open: map[string][]map[string]any{"sf/r": {
		{"number": 44, "title": "Main of sf/r is red: revert or fix?", "user": map[string]string{"login": "factory-agent"}}}}}
	forge := f.srv(t)
	repo := Repo{Owner: "sf", Name: "r"}
	if _, _, err := FileHumanCard(ctx, store, forge, repo, BriefText{Title: "Main of sf/r is red: revert or fix?", Body: "body"}, []string{"human"}, Task{ID: "t-h", Source: SourceStillRed},
		HumanAsk{Q: HumanQuestion{Ask: "Main of sf/r is red: revert or fix?", Why: "w", Info: "i"}, By: "factoryd", Asker: SourceStillRed, Via: ViaFactoryd}); err != nil {
		t.Fatal(err)
	}
	var held map[string]any
	for _, e := range store.events {
		if e.TaskID == "t-h" && e.Kind == EventQuestionAsked && e.Detail["held"] == true {
			held = e.Detail
		}
	}
	if held == nil {
		t.Fatalf("events %+v, want the card's held ask", store.events)
	}
	got, err := (&Runner{store: store, forgejo: forge}).fileFace(ctx, store.tasks["t-h"], repo, held, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.IssueNumber != 44 || store.tasks["t-h"].IssueNumber != 44 || len(f.issues) != 0 || !reflect.DeepEqual(f.labelled, []string{"sf/r#44 human state/waiting-human"}) {
		t.Fatalf("card on #%d (stored #%d), issues %q, labels %q; want #44 taken on, labelled, no issue filed", got.IssueNumber, store.tasks["t-h"].IssueNumber, f.issues, f.labelled)
	}
}

// The issue a face files lists the face's answers by the labels the card
// shows, and its recommendation names one of them: the held question's own
// label of its close, "Fixed, close", gives way to the face's "End the
// card" in the list, where the recommendation finds it.
func TestAFacesIssueListsTheLabelsItRecommendsBy(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	f := &chainForgejo{}
	forge := f.srv(t)
	repo := Repo{Owner: "sf", Name: "r"}
	q := HumanQuestion{Ask: "Main of sf/r is red: revert or fix?", Why: "w", Info: "i",
		Choices: []QuestionChoice{{Value: ActClose, Label: "Fixed, close"}, {Value: AnswerPark, Label: "Not now"}}}
	if _, _, err := FileHumanCard(ctx, store, forge, repo, BriefText{Title: q.Ask, Body: "body"}, []string{"human"}, Task{ID: "t-h", Source: SourceStillRed},
		HumanAsk{Q: q, By: "factoryd", Asker: SourceStillRed, Via: ViaFactoryd}); err != nil {
		t.Fatal(err)
	}
	held := store.events[0].Detail
	if held["held"] != true {
		t.Fatalf("events %+v, want the card's held ask", store.events)
	}
	face := map[string]any{"recommendation": "main is green again", "preselected": map[string]any{"action": ActClose},
		"offers": []AskOffer{{Value: ActClose, Label: "End the card"}, {Value: AnswerPark, Label: "Not now"}}}
	if _, err := (&Runner{store: store, forgejo: forge}).fileFace(ctx, store.tasks["t-h"], repo, held, face); err != nil {
		t.Fatal(err)
	}
	if len(f.issues) != 1 || !strings.Contains(f.issues[0], "\n\nRecommendation: \u201cEnd the card\u201d: main is green again\n\nChoices:\n- End the card\n- Not now\n") ||
		strings.Contains(f.issues[0], "Fixed, close") {
		t.Fatalf("the filed issue %q; want its list and its recommendation to say “End the card”", f.issues)
	}
}

// A freeze card's close record off its issue (comment_id 0) is the card's
// earlier end, not the close of a card that had no issue. The watcher's
// reopen confirms such a record (ReopenFreezeCard); this one is not
// confirmed, as a record a reopen left before that change. The heal pass
// lists no row of a freeze card, so it leaves the card's issue open and
// the card waiting, and calls Forgejo for neither issue.
func TestTheHealLeavesAFreezeCardsForgottenCloseOffItsIssue(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.CreateTask(ctx, Task{ID: "fz", Kind: "human", Status: "waiting-human", Source: SourceFreezeCard, Repository: "linkshort/linkshort", IssueNumber: 5, Track: "direct"})
	store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "fz", Kind: IntakeFactoryClosed, Author: "factoryd", CommentID: 5, Body: "factoryd: the press closed this card.", CreatedAt: time.Now().Add(-time.Minute)})
	for i := range store.intakeEvents {
		store.intakeEvents[i].CommentID = 0
	}
	fj := &planForgejo{issues: map[int64]*planIssue{5: {state: "open"}}, failCall: "GET issues/0", failStatus: http.StatusNotFound}
	in := &Intake{Store: store, Forgejo: &Forgejo{BaseURL: fj.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw"}}
	if err := in.HealFactoryCloses(ctx); err != nil {
		t.Fatal(err)
	}
	if fj.issues[5].state != "open" || slices.ContainsFunc(fj.calls, func(c string) bool { return strings.Contains(c, "issues/") }) || store.tasks["fz"].Status != "waiting-human" {
		t.Fatalf("issue #5 %s, calls %q, card %s; want no issue called and the card waiting-human", fj.issues[5].state, fj.calls, store.tasks["fz"].Status)
	}
	if left, err := store.UnconfirmedFactoryCloses(ctx, 0, 10); err != nil || len(left) != 0 {
		t.Fatalf("the heal's unconfirmed closes = %+v, %v; want no row of a freeze card", left, err)
	}
}
