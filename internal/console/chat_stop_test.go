package console

import (
	"bufio"
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// chatStream posts form, the session's CSRF token added, to path and
// answers its stream live: each event as "event data" on the channel,
// which closes when the response does. A test that stops a turn needs the
// events as they arrive, not once the stream ended.
func chatStream(t *testing.T, r *rig, ck *http.Cookie, s *session, path string, form url.Values) <-chan string {
	t.Helper()
	events := make(chan string, 16)
	go func() {
		defer close(events)
		srv := httptest.NewServer(r.c)
		defer srv.Close()
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		for k, vs := range form {
			for _, v := range vs {
				if err := w.WriteField(k, v); err != nil {
					return
				}
			}
		}
		if err := w.WriteField("csrf", s.csrf); err != nil {
			return
		}
		if err := w.Close(); err != nil {
			return
		}
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, &body)
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", w.FormDataContentType())
		req.AddCookie(ck)
		resp, err := srv.Client().Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		event := ""
		for sc.Scan() {
			if e, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
				event = e
			}
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				events <- event + " " + d
			}
		}
	}()
	return events
}

// waitEvent is the stream's next event, ending the test when the stream
// ends first or nothing comes.
func waitEvent(t *testing.T, events <-chan string) string {
	t.Helper()
	select {
	case e, ok := <-events:
		if !ok {
			t.Fatal("the stream ended before its next event")
		}
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("the stream's next event did not come")
		return ""
	}
}

// A stop ends the turn in flight (the chat-stop plan): a press on a
// thread with no turn, and a second press after one, change nothing; one
// mid-answer stops the model call at once, the stream answers stopped and
// ends, the thread keeps the answer so far as a row marked cut and the
// stop row that names the member, a message that joined the stopped turn
// stays unread, and one chat_stopped event lands on the project's feed.
func TestChatStopEndsTheTurn(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{say("Partial "), say("tail")}}
	g.midHold, g.midHoldLine = make(chan struct{}), 1

	before := r.events.count()
	if code, body, _ := chatPost(t, r, ck, s, chatPath+"/stop", url.Values{"conversation": {"1"}}); code != http.StatusNoContent {
		t.Fatalf("a stop with no turn in flight = %d %q", code, body)
	}
	if rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 1, 10); len(rows) != 0 {
		t.Fatalf("%d rows after a stop with no turn, want 0", len(rows))
	}
	equal(t, "event rows", r.events.count(), before)

	stream := chatStream(t, r, ck, s, chatPath, url.Values{"message": {"what is open?"}})
	if e := waitEvent(t, stream); e != `text {"text":"Partial "}` {
		t.Fatalf("the stream's first event = %q", e)
	}
	if code, joined, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"still there?"}, "conversation": {"1"}}); code != http.StatusOK ||
		len(joined) != 2 || !strings.HasPrefix(joined[0], "joined ") || joined[1] != "done {}" {
		t.Fatalf("the message joining the running turn = %d %v", code, joined)
	}
	if code, body, _ := chatPost(t, r, ck, s, chatPath+"/stop", url.Values{"conversation": {"1"}}); code != http.StatusNoContent {
		t.Fatalf("the stop = %d %q", code, body)
	}
	close(g.midHold)
	if e := waitEvent(t, stream); !strings.HasPrefix(e, `stopped {"message":{"id":`) ||
		!strings.Contains(e, `"content":"Stopped by sebastian."`) || !strings.Contains(e, `"kind":"stop"`) {
		t.Fatalf("the stream's stopped event = %q", e)
	}
	if e := waitEvent(t, stream); e != "done {}" {
		t.Fatalf("the stream's last event = %q", e)
	}
	for range stream { // the stream ends with done
	}

	rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 1, 10)
	if len(rows) != 4 {
		t.Fatalf("%d rows after the stop, want the message, the unread one, the cut answer and the stop row: %+v", len(rows), rows)
	}
	if rows[1].Content != "still there?" || !rows[1].Queued {
		t.Errorf("the joined message = %+v, want it unread", rows[1])
	}
	if rows[2].Role != "assistant" || !rows[2].Cut || !strings.HasPrefix(rows[2].Content, "Partial ") {
		t.Errorf("the cut answer = %+v", rows[2])
	}
	if rows[3].Kind != factoryd.MessageStop || rows[3].Content != "Stopped by sebastian." {
		t.Errorf("the stop row = %+v", rows[3])
	}
	after := r.events.all()[before:]
	if len(after) != 1 || after[0].Repository != "sf/code" || after[0].Kind != factoryd.EventChatStopped ||
		after[0].Actor != "sebastian" || after[0].Via != factoryd.ViaButton || after[0].Detail["thread"] != int64(1) {
		t.Fatalf("the stop's event rows = %+v", after)
	}

	if code, body, _ := chatPost(t, r, ck, s, chatPath+"/stop", url.Values{"conversation": {"1"}}); code != http.StatusNoContent {
		t.Fatalf("a second stop after the turn ended = %d %q", code, body)
	}
	equal(t, "event rows after the second press", r.events.count(), before+1)
}

// The rows a stop leaves read as the next turn's context does (the
// chat-stop plan): the stop row, like a budget row's, never replays; the
// cut answer stays; and a message the stop left unread starts with the
// next turn of any kind, so its words reach the model first.
func TestChatStopRowsLeaveTheContext(t *testing.T) {
	r, _, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{say("Partial "), say("tail")}, {say("on it")}}
	g.midHold, g.midHoldLine = make(chan struct{}), 1

	stream := chatStream(t, r, ck, s, chatPath, url.Values{"message": {"what is open?"}})
	if e := waitEvent(t, stream); e != `text {"text":"Partial "}` {
		t.Fatalf("the stream's first event = %q", e)
	}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"still there?"}, "conversation": {"1"}})
	chatPost(t, r, ck, s, chatPath+"/stop", url.Values{"conversation": {"1"}})
	close(g.midHold)
	waitEvent(t, stream) // stopped
	waitEvent(t, stream) // done
	for range stream {
	}

	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"go on"}, "conversation": {"1"}})
	calls := g.calls()
	if len(calls) != 2 {
		t.Fatalf("%d model calls, want 2", len(calls))
	}
	said := fmt.Sprint(messages(t, calls[1]))
	for _, want := range []string{"Partial ", "still there?"} {
		if !strings.Contains(said, want) {
			t.Errorf("the next turn's prompt lacks %q:\n%s", want, said)
		}
	}
	if strings.Contains(said, "Stopped by sebastian.") {
		t.Errorf("the next turn's prompt carries the stop row:\n%s", said)
	}
}
