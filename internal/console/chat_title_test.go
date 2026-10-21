package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The first message names the thread once (roadmap 304): the naming call
// goes out on the chat's task with the cheap alias and the first
// message's text, its cleaned answer lands as the thread's title in the
// switcher, no later message names again, and an empty answer leaves
// today's head standing.
func TestChatNamesTheThread(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.titleNames = []string{`"The cache plans."`}
	g.replies = [][]string{{say("Two cards wait.")}}
	code, _, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"Can you find the cache bug?"}})
	equal(t, "POST", code, 200)
	if !waitFor(t, 2*time.Second, func() bool { return len(g.titles()) == 1 }) {
		t.Fatal("no naming call went out")
	}
	equal(t, "naming calls", len(g.titles()), 1)
	msgs := g.titles()[0]["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("the naming call's first message = %v, want the naming prompt", msgs[0])
	}
	equal(t, "the naming call's message", msgs[1],
		map[string]any{"role": "user", "content": "Can you find the cache bug?"})
	if !waitFor(t, 2*time.Second, func() bool {
		rec := r.do(httptest.NewRequest(http.MethodGet, chatPath, nil), ck)
		return strings.Contains(rec.Body.String(), `"title":"The cache plans"`)
	}) {
		t.Fatal("the model's title did not land")
	}
	convs, err := chat.Conversations(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "the switcher's title", convs[0].Title, "The cache plans")
	g.replies = [][]string{{say("One card waits.")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"and now?"}})
	time.Sleep(100 * time.Millisecond)
	equal(t, "naming calls after the second message", len(g.titles()), 1)

	r2, _, g2 := newChatRig(t)
	ck2, s2 := r2.loggedIn(t)
	g2.replies = [][]string{{say("Two cards wait.")}}
	chatPost(t, r2, ck2, s2, chatPath, url.Values{"message": {"Can you find the cache bug?"}})
	if !waitFor(t, 2*time.Second, func() bool { return len(g2.titles()) == 1 }) {
		t.Fatal("no naming call went out")
	}
	rec := r2.do(httptest.NewRequest(http.MethodGet, chatPath, nil), ck2)
	if !strings.Contains(rec.Body.String(), `"title":"Can you find the cache bug?"`) {
		t.Fatalf("the head did not stand: %s", rec.Body.String())
	}
}

// The inbox's and a card thread's first messages name nothing (roadmap
// 304): the inbox's title and a card thread's "#task" stay.
func TestChatNamesNoInboxOrCardThread(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	inbox, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	card, err := chat.EnsureCardThread(t.Context(), "sf/code", "sebastian", "t-m")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int64{inbox, card} {
		g.replies = [][]string{{say("ok")}}
		code, _, _ := chatPost(t, r, ck, s, chatPath,
			url.Values{"message": {"what is open?"}, "conversation": {strconv.FormatInt(n, 10)}})
		equal(t, "POST", code, 200)
	}
	time.Sleep(100 * time.Millisecond)
	equal(t, "naming calls", len(g.titles()), 0)
}

// cleanTitle makes the model's answer a name: one line's words on one
// line, quotes and a trailing dot off, cut at a word's end at max.
func TestCleanTitle(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"  \n ", ""},
		{"The cache plans.", "The cache plans"},
		{`"The cache plans."`, "The cache plans"},
		{"«Der Cache-Plan.»", "Der Cache-Plan"},
		{"*Cache-Plan*", "Cache-Plan"},
		{"First line\nsecond line", "First line"},
		{"words   in   pieces", "words in pieces"},
		{strings.TrimSpace(strings.Repeat("word ", 12)), strings.TrimSpace(strings.Repeat("word ", 12))},
		{strings.Repeat("word ", 30) + "end", strings.TrimSpace(strings.Repeat("word ", 12))},
	} {
		if got := cleanTitle(tc.in, titleMax); got != tc.want {
			t.Errorf("cleanTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
