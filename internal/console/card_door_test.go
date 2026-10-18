package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// seedAsk records t-e's open ask as the worker files it: the five parts
// and the choices.
func seedAsk(t *testing.T, r *rig) {
	t.Helper()
	if _, err := r.events.Record(t.Context(), factoryd.Event{Repository: "sf/code", TaskID: "t-e",
		Kind: factoryd.EventQuestionAsked, Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd,
		Detail: map[string]any{"question": "use a replica?", "why": "the shard costs more every month",
			"info": "the plan names one store", "recommendation": "a replica",
			"choices": []any{"a replica", "a shard"}}}); err != nil {
		t.Fatal(err)
	}
}

// The card's door: POST makes the card's thread, its first row the ask's
// five parts with the choices as its buttons, and an answer through the
// thread settles the card as the inbox's buttons do, with no model call.
func TestCardDoorOpensBoundThread(t *testing.T) {
	r, _, g := newChatRig(t)
	seedAsk(t, r)
	ck, s := r.loggedIn(t)
	rec := r.do(post("/cards/t-e/thread", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /cards/t-e/thread = %d %s", rec.Code, rec.Body.String())
	}
	var door struct {
		Conversation int64 `json:"conversation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &door); err != nil || door.Conversation == 0 {
		t.Fatalf("the door's answer = %s (%v), want a conversation number", rec.Body.String(), err)
	}
	conv := strconv.FormatInt(door.Conversation, 10)
	get := r.do(httptest.NewRequest(http.MethodGet, chatPath+"?conversation="+conv, nil), ck)
	var answer chatGetAnswer
	if err := json.Unmarshal(get.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Thread.Title != "#t-e" {
		t.Fatalf("the thread's title = %q, want #t-e", answer.Thread.Title)
	}
	if len(answer.Messages) != 1 {
		t.Fatalf("the thread's rows = %+v, want the ask alone", answer.Messages)
	}
	m := answer.Messages[0]
	for _, want := range []string{"#13 needs you: use a replica?", "\nWhy: the shard costs more every month",
		"\nInfo: the plan names one store", "\nRecommendation: a replica"} {
		if !strings.Contains(m.Content, want) {
			t.Fatalf("the ask's row =\n%s\nwant it to hold %q", m.Content, want)
		}
	}
	if m.Role != "user" || m.Kind != factoryd.MessageEscalation || m.BirthKind != factoryd.EventQuestionAsked ||
		m.TaskID != "t-e" || len(m.Choices) != 2 || m.Choices[0] != "a replica" || m.Choices[1] != "a shard" {
		t.Fatalf("the ask's row = %+v, want the seeded ask with its choices", m)
	}
	// the thread answers the card as the inbox's buttons do
	code, _, _ := chatPost(t, r, ck, s, chatPath+"/answer",
		url.Values{"conversation": {conv}, "message": {strconv.FormatInt(m.ID, 10)}, "choice": {"a replica"}})
	if code != http.StatusNoContent {
		t.Fatalf("the thread's answer = %d, want 204", code)
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; got != "a replica" {
		t.Fatalf("the settle comment = %q, want the choice", got)
	}
	if len(g.calls()) != 0 {
		t.Fatalf("model calls through the door = %d, want none", len(g.calls()))
	}
}

// The drawer of a needs-you card names the door and the worker's tab is
// Session, not Chat.
func TestDrawerShowsDoorAndSessionTab(t *testing.T) {
	r, _, _ := newChatRig(t)
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-e/drawer", nil), ck).Body.String()
	if !strings.Contains(body, `data-card-thread="t-e"`) {
		t.Fatal("the drawer holds no door control")
	}
	if !strings.Contains(body, ">Session<") {
		t.Fatal("the drawer names no Session tab")
	}
	if strings.Contains(body, ">Chat<") {
		t.Fatal("the drawer still names a Chat tab")
	}
}

// The ask's sentence rides its ask field, the field the wake path
// writes: the thread's first row carries it, not an empty ask.
func TestCardDoorSeedsTheAskSentence(t *testing.T) {
	r, _, _ := newChatRig(t)
	if _, err := r.events.Record(t.Context(), factoryd.Event{Repository: "sf/code", TaskID: "t-e",
		Kind: factoryd.EventQuestionAsked, Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd,
		Detail: map[string]any{"ask": "use a replica?"}}); err != nil {
		t.Fatal(err)
	}
	ck, s := r.loggedIn(t)
	rec := r.do(post("/cards/t-e/thread", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /cards/t-e/thread = %d %s", rec.Code, rec.Body.String())
	}
	var door struct {
		Conversation int64 `json:"conversation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &door); err != nil || door.Conversation == 0 {
		t.Fatalf("the door's answer = %s (%v), want a conversation number", rec.Body.String(), err)
	}
	get := r.do(httptest.NewRequest(http.MethodGet,
		chatPath+"?conversation="+strconv.FormatInt(door.Conversation, 10), nil), ck)
	var answer chatGetAnswer
	if err := json.Unmarshal(get.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if len(answer.Messages) != 1 {
		t.Fatalf("the thread's rows = %+v, want the ask alone", answer.Messages)
	}
	if want := "#13 needs you: use a replica?"; !strings.Contains(answer.Messages[0].Content, want) {
		t.Fatalf("the ask's row = %q, want it to hold %q", answer.Messages[0].Content, want)
	}
}
