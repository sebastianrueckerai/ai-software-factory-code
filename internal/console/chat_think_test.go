package console

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// thinks is a reasoning delta of the chat's model (the thinking-fold
// plan): the reasoning_content premium and the GLM classes send beside
// their text.
func thinks(s string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"reasoning_content": s}}}})
	return string(b)
}

// The turn streams the model's reasoning as it arrives (the thinking-fold
// plan): one think event per reasoning delta, in order, before and
// between the text it explains, and the stream still ends with done.
func TestChatStreamsThinking(t *testing.T) {
	r, _, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{thinks("The member asks what is open. "), say("Two cards"),
		thinks("Keep it short. "), say(" wait on you.")}}

	stream := chatStream(t, r, ck, s, chatPath, url.Values{"message": {"what is open?"}})
	for _, want := range []string{
		`think {"text":"The member asks what is open. "}`,
		`text {"text":"Two cards"}`,
		`think {"text":"Keep it short. "}`,
		`text {"text":" wait on you."}`,
		"done {}",
	} {
		if e := waitEvent(t, stream); e != want {
			t.Fatalf("the stream's next event = %q, want %q", e, want)
		}
	}
	for range stream { // the stream ends with done
	}
}

// The model's reasoning never lands (the thinking-fold plan): the thread
// holds the answer and only the answer, so the replay and the model's
// context never see the thinking.
func TestChatThinkingIsNotStored(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{thinks("The member asks what is open. "), say("Two cards wait on you.")}}

	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"what is open?"}})

	rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 1, 10)
	if len(rows) != 2 {
		t.Fatalf("%d rows after the turn, want the message and the answer: %+v", len(rows), rows)
	}
	if rows[1].Role != "assistant" || rows[1].Content != "Two cards wait on you." || rows[1].Kind != "" {
		t.Errorf("the answer row = %+v", rows[1])
	}
	for _, row := range rows {
		if strings.Contains(row.Content, "asks what is open") {
			t.Errorf("the row %+v carries the model's thinking", row)
		}
	}
}

// A scripted lab reply carries its reasoning (the thinking-fold plan):
// each Think chunk of a labReply is one reasoning delta on the stream, so
// the lab can photograph a turn that reasons while the member watches.
func TestChatGatewayScriptThinks(t *testing.T) {
	r, _, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.script("ai-software-factory-code", []labReply{{{Think: "The board first. "}, {Say: "Two cards"},
		{Think: "Then stop. "}, {Say: " wait on you."}}})

	stream := chatStream(t, r, ck, s, chatPath, url.Values{"message": {"what is open?"}})
	for _, want := range []string{
		`think {"text":"The board first. "}`,
		`text {"text":"Two cards"}`,
		`think {"text":"Then stop. "}`,
		`text {"text":" wait on you."}`,
		"done {}",
	} {
		if e := waitEvent(t, stream); e != want {
			t.Fatalf("the scripted stream's next event = %q, want %q", e, want)
		}
	}
	for range stream { // the stream ends with done
	}
}
