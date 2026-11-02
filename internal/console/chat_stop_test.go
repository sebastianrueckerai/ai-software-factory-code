package console

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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

// The stop reaches a turn that has not run yet

// The stop reaches a turn that has not run yet (the chat-stop plan): the
// browser counts a turn busy from its POST, so a press while the turn
// still waits — here for one of the project's parallel slots, three turns
// holding them in slow model calls — stops it: the press lands its event,
// the waiting turn's stream answers stopped and ends without a model
// call, its message stays unanswered, and the thread holds its row and
// the stop row that names the member.
func TestChatStopEndsATurnThatStillWaits(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.slow = 8 * time.Second
	g.arrived = make(chan struct{}, 8)
	g.replies = [][]string{{say("one")}, {say("two")}, {say("three")}, {say("late answer")}}
	for _, message := range []string{"first", "second", "third"} {
		if code, _, _ := chatPost(t, r, ck, s, chatPath+"/new", nil); code != http.StatusNoContent {
			t.Fatalf("POST /chat/new before %q = %d", message, code)
		}
		chatStream(t, r, ck, s, chatPath, url.Values{"message": {message}})
		select {
		case <-g.arrived:
		case <-time.After(10 * time.Second):
			t.Fatalf("the turn of %q never reached its model call", message)
		}
	}
	if code, _, _ := chatPost(t, r, ck, s, chatPath+"/new", nil); code != http.StatusNoContent {
		t.Fatalf("POST /chat/new = %d", code)
	}
	victim := chatStream(t, r, ck, s, chatPath, url.Values{"message": {"stop me"}, "conversation": {"5"}})
	before := r.events.count()
	pressed := false
	for range 200 {
		if code, _, _ := chatPost(t, r, ck, s, chatPath+"/stop", url.Values{"conversation": {"5"}}); code != http.StatusNoContent {
			t.Fatalf("the stop = %d", code)
		}
		for _, e := range r.events.all()[before:] {
			if e.Kind == factoryd.EventChatStopped {
				pressed = true
			}
		}
		if pressed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !pressed {
		t.Fatal("the press while the turn waited for a slot stopped nothing")
	}
	if e := waitEvent(t, victim); !strings.HasPrefix(e, `stopped {"message":`) || !strings.Contains(e, `"content":"Stopped by sebastian."`) {
		t.Fatalf("the waiting turn's stream = %q, want it stopped", e)
	}
	if e := waitEvent(t, victim); e != "done {}" {
		t.Fatalf("the stream's last event = %q", e)
	}
	for range victim {
	}
	rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 5, 10)
	if len(rows) != 2 || rows[0].Content != "stop me" || rows[1].Kind != factoryd.MessageStop || rows[1].Content != "Stopped by sebastian." {
		t.Fatalf("thread 5's rows = %+v, want the message and the stop row", rows)
	}
	if calls := g.calls(); len(calls) != 0 {
		t.Fatalf("%d model calls finished, want none: the press ends the waiting turn before any answer runs", len(calls))
	}
	after := r.events.all()[before:]
	if len(after) != 1 || after[0].Kind != factoryd.EventChatStopped || after[0].Via != factoryd.ViaButton || after[0].Detail["thread"] != int64(5) {
		t.Fatalf("the stop's event rows = %+v", after)
	}
}

// waitStored waits until the thread holds the row of content: a POST
// owns the thread's turn only once its message row is in.
func waitStored(t *testing.T, chat *fakeChat, thread int64, content string) {
	t.Helper()
	for range 2000 {
		rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", thread, 10)
		for _, row := range rows {
			if row.Content == content {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the message %q never stored", content)
}

// A second press stops the thread's next waiting turn (the chat-stop
// plan): with the thread's own turn waiting for one of the project's
// model slots — three turns holding them in slow calls — a successor
// message queues behind it, and two presses end both at once, each with
// its stop row, the successor's message left for the thread's next turn,
// and no model call for either.
func TestChatStopSecondPressStopsTheNextWaitingTurn(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.slow = 8 * time.Second
	g.arrived = make(chan struct{}, 8)
	g.replies = [][]string{{say("one")}, {say("two")}, {say("three")}, {say("never said")}}
	for _, message := range []string{"first", "second", "third"} {
		if code, _, _ := chatPost(t, r, ck, s, chatPath+"/new", nil); code != http.StatusNoContent {
			t.Fatalf("POST /chat/new before %q = %d", message, code)
		}
		chatStream(t, r, ck, s, chatPath, url.Values{"message": {message}})
		select {
		case <-g.arrived:
		case <-time.After(10 * time.Second):
			t.Fatalf("the turn of %q never reached its model call", message)
		}
	}
	if code, _, _ := chatPost(t, r, ck, s, chatPath+"/new", nil); code != http.StatusNoContent {
		t.Fatalf("POST /chat/new = %d", code)
	}
	first := chatStream(t, r, ck, s, chatPath, url.Values{"message": {"held back"}})
	waitStored(t, chat, 5, "held back")
	next := chatStream(t, r, ck, s, chatPath, url.Values{"message": {"behind it"}, "conversation": {"5"}})
	waitStored(t, chat, 5, "behind it") // queued behind the thread's turn before either press
	before := r.events.count()
	pressed := 0
	for range 200 {
		if code, _, _ := chatPost(t, r, ck, s, chatPath+"/stop", url.Values{"conversation": {"5"}}); code != http.StatusNoContent {
			t.Fatalf("the stop = %d", code)
		}
		pressed = 0
		for _, e := range r.events.all()[before:] {
			if e.Kind == factoryd.EventChatStopped {
				pressed++
			}
		}
		if pressed >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pressed < 2 {
		t.Fatal("the two presses stopped fewer than the thread's two waiting turns")
	}
	if e := waitEvent(t, first); !strings.HasPrefix(e, `stopped {"message":`) || !strings.Contains(e, `"content":"Stopped by sebastian."`) {
		t.Fatalf("the waiting turn's stream = %q, want it stopped", e)
	}
	if e := waitEvent(t, first); e != "done {}" {
		t.Fatalf("the first stream's last event = %q, want the stream ended", e)
	}
	if e := waitEvent(t, next); e != `queued {"position":1}` {
		t.Fatalf("the successor's first event = %q, want its place in line", e)
	}
	if e := waitEvent(t, next); !strings.HasPrefix(e, `stopped {"message":`) || !strings.Contains(e, `"content":"Stopped by sebastian."`) {
		t.Fatalf("the successor's stream = %q, want it stopped too", e)
	}
	if e := waitEvent(t, next); e != "done {}" {
		t.Fatalf("the successor's last event = %q, want the stream ended", e)
	}
	for range first {
	}
	for range next {
	}
	rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 5, 10)
	if len(rows) != 4 || rows[0].Content != "held back" {
		t.Fatalf("thread 5's rows = %+v, want both messages and both stop rows", rows)
	}
	stops, said := 0, 0
	for _, row := range rows[1:] {
		switch {
		case row.Kind == factoryd.MessageStop && row.Content == "Stopped by sebastian.":
			stops++
		case row.Role == "user" && row.Content == "behind it":
			said++
		}
	}
	if stops != 2 || said != 1 {
		t.Fatalf("thread 5's rows = %+v, want the successor's message and both stop rows", rows)
	}
	if calls := g.calls(); len(calls) != 0 {
		t.Fatalf("%d model calls finished, want none: both stopped turns ended before a model call", len(calls))
	}
	after := r.events.all()[before:]
	if len(after) != 2 || after[0].Kind != factoryd.EventChatStopped || after[1].Kind != factoryd.EventChatStopped ||
		after[0].Via != factoryd.ViaButton || after[1].Via != factoryd.ViaButton ||
		after[0].Detail["thread"] != int64(5) || after[1].Detail["thread"] != int64(5) {
		t.Fatalf("the presses' event rows = %+v", after)
	}
}

// A stop ends the follow-up turn of a pressed proposal too: the tool's
// refusal row stays, the stream answers stopped and ends, and the stop row
// lands after it.
func TestChatStopEndsAPressedProposalsFollowUp(t *testing.T) {
	r, chat, g := newChatRig(t)
	annasRefusedProposal(t, r)
	ck, s := memberLogin(t, r, "anna")
	g.replies = [][]string{{say("never said")}}
	g.hold, g.arrived = make(chan struct{}), make(chan struct{}, 1)
	defer close(g.hold)

	stream := chatStream(t, r, ck, s, chatPath+"/proposals/p-anna-proposal", nil)
	<-g.arrived
	if code, body, _ := chatPost(t, r, ck, s, chatPath+"/stop", url.Values{"conversation": {"1"}}); code != http.StatusNoContent {
		t.Fatalf("the stop = %d %q", code, body)
	}
	var events []string
	for e := range stream {
		events = append(events, e)
	}
	if n := len(events); n < 2 || !strings.HasPrefix(events[n-2], `stopped {"message":{"id":`) ||
		!strings.Contains(events[n-2], `"content":"Stopped by anna."`) || events[n-1] != "done {}" {
		t.Fatalf("the stream = %v, want it to end with stopped, then done", events)
	}
	rows, _ := chat.ChatMessages(t.Context(), "sf/code", "anna", 1, 10)
	if len(rows) != 2 || rows[0].ToolCalls[0].Name != "approve_merge" || rows[1].Kind != factoryd.MessageStop || rows[1].Content != "Stopped by anna." {
		t.Fatalf("rows = %+v, want the refused tool's row, then the stop row", rows)
	}
}

// A stop reaches a pressed proposal before a message that came while its
// tool ran: the press's follow-up ends stopped, its tool's row standing,
// and the message, which waited behind the press, gets its own answer.
func TestChatStopReachesThePressBeforeAMessageThatWaits(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	ran, release := make(chan struct{}), make(chan struct{})
	r.c.cfg.Escalate = func(context.Context, string, factoryd.HumanQuestion, string) (int64, error) {
		close(ran)
		<-release
		return 0, errors.New("forgejo is down")
	}
	if err := chat.PutProposal(t.Context(), factoryd.ChatProposal{Token: "p-escalate-press", Kind: "proposal", Repository: "sf/code", Login: "sebastian",
		Tool: "escalate", Args: json.RawMessage(`{"ask":"Use a replica?","why":"The card cannot go on.","info":"The replica adds 12 ms of lag.",` +
			`"recommendation":"Keep one replica","choices":["Yes","No"]}`), ExpiresAt: time.Now().Add(time.Hour), Conversation: 1}); err != nil {
		t.Fatal(err)
	}
	g.replies = [][]string{{say("The message's answer.")}}

	press := chatStream(t, r, ck, s, chatPath+"/proposals/p-escalate-press", nil)
	<-ran
	message := chatStream(t, r, ck, s, chatPath, url.Values{"message": {"still there?"}, "conversation": {"1"}})
	if e := waitEvent(t, message); e != `queued {"position":1}` {
		t.Fatalf("the message's first event = %q, want it queued behind the press", e)
	}
	if code, body, _ := chatPost(t, r, ck, s, chatPath+"/stop", url.Values{"conversation": {"1"}}); code != http.StatusNoContent {
		t.Fatalf("the stop = %d %q", code, body)
	}
	close(release)

	var pressed, said []string
	for e := range press {
		pressed = append(pressed, e)
	}
	for e := range message {
		said = append(said, e)
	}
	if n := len(pressed); n < 2 || !strings.HasPrefix(pressed[n-2], `stopped {"message":`) ||
		!strings.Contains(pressed[n-2], `"content":"Stopped by sebastian."`) || pressed[n-1] != "done {}" {
		t.Fatalf("the press's stream = %v, want it to end stopped, then done", pressed)
	}
	if !slices.Contains(said, `text {"text":"The message's answer."}`) || said[len(said)-1] != "done {}" ||
		slices.ContainsFunc(said, func(e string) bool { return strings.HasPrefix(e, "stopped ") }) {
		t.Fatalf("the message's stream = %v, want its answer and no stop", said)
	}
	// the queued message lands, after the press's rows, when its turn starts
	rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 1, 10)
	if len(rows) != 4 || rows[0].ToolCalls[0].Name != "escalate" || !strings.HasPrefix(rows[0].ToolCalls[0].Result, "Refused: ") ||
		rows[1].Kind != factoryd.MessageStop || rows[1].Content != "Stopped by sebastian." ||
		rows[2].Content != "still there?" || rows[2].Queued || rows[3].Content != "The message's answer." {
		t.Fatalf("rows = %+v, want the refused tool's row, the stop row, the landed message and its answer", rows)
	}
	if n := len(g.calls()); n != 1 {
		t.Fatalf("%d model calls, want the message's alone", n)
	}
}
