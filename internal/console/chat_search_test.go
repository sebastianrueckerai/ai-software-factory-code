package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// seedThread puts login's rows on the code repository's thread n, n of
// their current conversation for 0.
func seedThread(t *testing.T, chat *fakeChat, login string, n int64, rows ...factoryd.ChatMessage) {
	t.Helper()
	for _, m := range rows {
		if n > 0 {
			m.Conversation = n
		}
		if _, err := chat.AddChatMessage(context.Background(), "sf/code", login, m); err != nil {
			t.Fatal(err)
		}
	}
}

// chat_search reads every thread of the project, every member's: a hit
// names its thread's owner and title, its excerpt runs to 100 bytes, and
// a 21st hit is counted but not shown.
func TestChatSearchFindsAcrossMembers(t *testing.T) {
	r, chat, g := newChatRig(t)
	other, err := chat.NewConversation(context.Background(), "sf/code", "controller")
	if err != nil {
		t.Fatal(err)
	}
	seedThread(t, chat, "sebastian", 0, factoryd.ChatMessage{Role: "user", Content: "needle once"})
	seedThread(t, chat, "controller", other,
		factoryd.ChatMessage{Role: "user", Content: "the shard plan"},
		factoryd.ChatMessage{Role: "assistant", Content: "needle " + strings.Repeat("x", 300)})
	for i := range 19 {
		seedThread(t, chat, "controller", other, factoryd.ChatMessage{Role: "assistant", Content: fmt.Sprintf("needle again %d", i)})
	}
	out := reads(t, r, g, "chat_search", `{"query":"needle"}`)
	if len(out) != 1 {
		t.Fatalf("%d tool results, want one", len(out))
	}
	hits := 0
	for _, line := range strings.Split(out[0], "\n") {
		if strings.HasPrefix(line, "thread ") {
			hits++
		}
	}
	// controller's thread is the project's newest, so its 20 hits fill
	// the cap and sebastian's own is the one counted but not shown
	equal(t, "the hits shown", hits, 20)
	if !strings.Contains(out[0], "thread "+strconv.FormatInt(other, 10)+" (controller's, the shard plan): row ") {
		t.Errorf("the hit names the thread's owner and title:\n%s", out[0])
	}
	if !strings.Contains(out[0], ": assistant: needle ") {
		t.Errorf("the hit carries its row's role and excerpt:\n%s", out[0])
	}
	if strings.Contains(out[0], strings.Repeat("x", 94)) {
		t.Errorf("the excerpt runs past 100 bytes:\n%s", out[0])
	}
	if !strings.Contains(out[0], "... and 1 more") {
		t.Errorf("the 21st hit is counted, not shown:\n%s", out[0])
	}
}

// active_threads lists every member's threads on the project: another
// member's thread appears with its owner named, even one whose number
// the reader's own thread shares.
func TestActiveThreadsListsProjectWide(t *testing.T) {
	r, chat, g := newChatRig(t)
	seedThread(t, chat, "controller", 0, factoryd.ChatMessage{Role: "user", Content: "watch the shard"})
	out := reads(t, r, g, "active_threads", `{}`)
	if len(out) != 1 {
		t.Fatalf("%d tool results, want one", len(out))
	}
	if !strings.Contains(out[0], "The project's active threads on ai-software-factory-code") ||
		!strings.Contains(out[0], `thread 1 "watch the shard" (controller's)`) {
		t.Fatalf("controller's thread is missing from the digest:\n%s", out[0])
	}
}

// Any project conversation opens read/write with its owner named: the
// page renders another member's rows, a message posted there lands in
// their thread while its actor is the writer, an ask in it settles from
// wherever its answer lands, and its controls stay its owner's.
func TestConversationReadAcrossMembers(t *testing.T) {
	r, chat, g := newChatRig(t)
	ctx := t.Context()
	seedThread(t, chat, "controller", 0, factoryd.ChatMessage{Role: "user", Content: "the shard costs more"})
	ck, s := r.loggedIn(t)

	rec := r.do(httptest.NewRequest(http.MethodGet, chatPath+"?conversation=1&owner=controller", nil), ck)
	var read chatGetAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &read); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || read.Conversation != 1 || read.Thread.Login != "controller" ||
		len(read.Messages) != 1 || read.Messages[0].Content != "the shard costs more" {
		t.Fatalf("controller's thread as sebastian = %d, conversation %d of %q with %d rows:\n%s",
			rec.Code, read.Conversation, read.Thread.Login, len(read.Messages), rec.Body)
	}

	g.replies = [][]string{{say("Read and done.")}}
	before := r.events.count()
	code, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{
		"message": {"what would a shard cost?"}, "conversation": {"1"}, "owner": {"controller"}})
	if code != http.StatusOK || events[len(events)-1] != "done {}" {
		t.Fatalf("the turn in controller's thread = %d %v", code, events)
	}
	chatRows(t, r.events, before, "sebastian") // a read records no event
	said := ""
	for _, m := range func() []factoryd.ChatMessage {
		rows, err := chat.ChatMessages(ctx, "sf/code", "controller", 1, 50)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}() {
		said += m.Role + ": " + m.Content + "\n"
	}
	for _, want := range []string{"user: what would a shard cost?\n", "assistant: Read and done.\n"} {
		if !strings.Contains(said, want) {
			t.Errorf("controller's thread holds no %q:\n%s", want, said)
		}
	}

	seedThread(t, chat, "controller", 1, factoryd.ChatMessage{Role: "user", Kind: factoryd.MessageEscalation,
		Content: "#13 needs you: Use a replica?", Choices: []string{"a replica", "a shard"}, TaskID: "t-e"})
	code, body, _ := chatPost(t, r, ck, s, chatPath+"/answer", url.Values{
		"conversation": {"1"}, "owner": {"controller"}, "message": {strconv.FormatInt(chat.next, 10)}, "choice": {"a replica"}})
	if code != http.StatusNoContent {
		t.Fatalf("the answer in controller's thread = %d %v", code, body)
	}
	equal(t, "the comment as the writer, not the owner", r.fg.commentBodies("/api/v1/repos/sf/code/issues/13/comments"), []string{"a replica"})

	if renamed := r.do(post(chatPath+"/rename", url.Values{"csrf": {s.csrf}, "conversation": {"1"},
		"owner": {"controller"}, "title": {"mine now"}}), ck); renamed.Code != http.StatusForbidden {
		t.Fatalf("renaming controller's thread as sebastian = %d, want the refusal:\n%s", renamed.Code, renamed.Body)
	}
}
