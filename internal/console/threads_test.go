package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// waitUntil polls ok for up to five seconds.
func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("waited 5 s for %s", what)
}

// A second message while a turn runs is stored at once as queued, answers
// queued with its place, and starts its turn when the first ends: the
// first turn's answer lands before it, and the second turn reads it
// (roadmap 239, decision 4).
func TestChatTurnsSerialise(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.hold, g.arrived = make(chan struct{}), make(chan struct{}, 4)
	g.replies = [][]string{{say("first answer")}, {say("second answer")}}
	type result struct {
		code   int
		events []string
		err    error
	}
	post := func(text string, into chan<- result) {
		code, events, _, err := postStream(r, ck, s, chatPath, url.Values{"message": {text}})
		into <- result{code, events, err}
	}
	first, second := make(chan result, 1), make(chan result, 1)
	go post("first", first)
	<-g.arrived
	go post("second", second)
	queued := func() int {
		chat.mu.Lock()
		defer chat.mu.Unlock()
		n := 0
		for _, m := range chat.msgs["sf/code sebastian"] {
			if m.Kind == factoryd.MessageQueued {
				n++
			}
		}
		return n
	}
	waitUntil(t, "the second message stored as queued", func() bool { return queued() == 1 })
	if !r.c.turns.working(threadKey("sf/code", "sebastian", 1)) {
		t.Fatal("the thread does not show a turn in flight")
	}
	close(g.hold)
	a, b := <-first, <-second
	if a.err != nil || b.err != nil {
		t.Fatal(a.err, b.err)
	}
	equal(t, "the first stream", []any{a.code, a.events}, []any{200, []string{`text {"text":"first answer"}`, `done {}`}})
	equal(t, "the second stream", []any{b.code, b.events},
		[]any{200, []string{`queued {"position":1}`, `text {"text":"second answer"}`, `done {}`}})
	msgs, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, m := range msgs {
		order = append(order, m.Role+" "+m.Kind+":"+m.Content)
	}
	equal(t, "the thread", order, []string{"user :first", "assistant :first answer", "user :second", "assistant :second answer"})
	equal(t, "the second turn's messages", messages(t, g.calls()[1])[1:], []map[string]any{
		{"role": "user", "content": "first"}, {"role": "assistant", "content": earlier("first answer")}, {"role": "user", "content": "second"}})
	if r.c.turns.working(threadKey("sf/code", "sebastian", 1)) {
		t.Fatal("the thread still shows a turn in flight after both ended")
	}
}

// A queued turn reads the project when it starts, after the turn ahead of
// it ends, not when its message came: its read is stamped then.
func TestQueuedTurnReadsWhenItStarts(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.hold, g.arrived = make(chan struct{}), make(chan struct{}, 4)
	g.replies = [][]string{{say("first answer")}, {call(1, "board", `{}`)}, {say("second answer")}}
	done := make(chan error, 2)
	post := func(text string) {
		_, _, _, err := postStream(r, ck, s, chatPath, url.Values{"message": {text}})
		done <- err
	}
	go post("first")
	<-g.arrived
	go post("second")
	waitUntil(t, "the second message stored as queued", func() bool {
		chat.mu.Lock()
		defer chat.mu.Unlock()
		return slices.ContainsFunc(chat.msgs["sf/code sebastian"], func(m factoryd.ChatMessage) bool { return m.Kind == factoryd.MessageQueued })
	})
	released := time.Now()
	close(g.hold)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 5 || len(msgs[3].ToolCalls) != 1 {
		t.Fatalf("the thread holds %d rows, want the second turn's board read as its fourth: %+v", len(msgs), msgs)
	}
	if read := msgs[3].ToolCalls[0].ReadAt; read.Before(released) {
		t.Fatalf("the queued turn's board read is stamped %v, before the turn ahead ended after %v", read, released)
	}
}

// The gate: one turn per thread, the waiting ones in order, and at most
// the project's bound of model turns at once, another project's apart.
func TestTurnGate(t *testing.T) {
	g := newTurnGate()
	key := threadKey("o/r", "anna", 1)
	pos, open := g.enter(key)
	if pos != 0 {
		t.Fatalf("the first turn's place = %d, want 0", pos)
	}
	<-open
	p2, w2 := g.enter(key)
	p3, w3 := g.enter(key)
	equal(t, "the waiting places", []int{p2, p3}, []int{1, 2})
	g.leave(key)
	select {
	case <-w2:
	default:
		t.Fatal("the second turn did not start when the first left")
	}
	select {
	case <-w3:
		t.Fatal("the third turn started while the second runs")
	default:
	}
	g.leave(key)
	<-w3
	g.leave(key)
	if g.working(key) {
		t.Fatal("the thread works after every turn left")
	}

	r1, r2 := g.slot("o/r", 2), g.slot("o/r", 2)
	got := make(chan func(), 1)
	go func() { got <- g.slot("o/r", 2) }()
	select {
	case <-got:
		t.Fatal("a third model turn ran over the bound of 2")
	case <-time.After(50 * time.Millisecond):
	}
	other := g.slot("o/other", 2)
	r1()
	select {
	case r3 := <-got:
		r3()
	case <-time.After(5 * time.Second):
		t.Fatal("the third model turn did not start when one freed")
	}
	r2()
	other()
}

// A message names its thread: the turn reads that thread and its rows
// land there, an unknown thread is 404 and no number 400; the title is the
// member's to change; the seen marker clears a thread's unread turns.
func TestChatThreadsByNumber(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{say("first reply")}, {say("second reply")}, {say("again reply")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"hello"}})
	chatPost(t, r, ck, s, chatPath+"/new", nil)
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"world"}})
	code, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"again"}, "conversation": {"1"}})
	equal(t, "the post to thread 1", []any{code, events}, []any{200, []string{`text {"text":"again reply"}`, `done {}`}})
	equal(t, "the third turn's messages", messages(t, g.calls()[2])[1:], []map[string]any{
		{"role": "user", "content": "hello"}, {"role": "assistant", "content": earlier("first reply")}, {"role": "user", "content": "again"}})
	one, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 1, 10)
	if err != nil || len(one) != 4 {
		t.Fatalf("thread 1 = %v, %v, want its four rows", one, err)
	}
	unread := func() map[int64]int64 {
		t.Helper()
		list, err := chat.Conversations(t.Context(), "sf/code", "sebastian")
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]int64{}
		for _, c := range list {
			out[c.Number] = c.Unread
		}
		return out
	}
	equal(t, "the unread turns", unread(), map[int64]int64{1: 2, 2: 1})
	if code, body, _ := chatPost(t, r, ck, s, chatPath+"/seen", url.Values{"conversation": {"1"}}); code != http.StatusNoContent {
		t.Fatalf("POST seen = %d %q", code, body)
	}
	equal(t, "the unread turns after thread 1 was read", unread(), map[int64]int64{1: 0, 2: 1})

	for _, tt := range []struct {
		form url.Values
		code int
		body string
	}{
		{url.Values{"message": {"x"}, "conversation": {"9"}}, http.StatusNotFound, "404 page not found"},
		{url.Values{"message": {"x"}, "conversation": {"0"}}, http.StatusBadRequest, "conversation must be a positive integer"},
	} {
		if code, body, _ := chatPost(t, r, ck, s, chatPath, tt.form); code != tt.code || body[0] != tt.body {
			t.Errorf("POST %v = %d %q, want %d %q", tt.form, code, body, tt.code, tt.body)
		}
	}

	if code, body, _ := chatPost(t, r, ck, s, chatPath+"/rename", url.Values{"conversation": {"2"}, "title": {"  The   second  "}}); code != http.StatusNoContent {
		t.Fatalf("POST rename = %d %q", code, body)
	}
	th, ok, _ := chat.Conversation(t.Context(), "sf/code", "sebastian", 2)
	if !ok || th.Title != "The second" {
		t.Fatalf("thread 2 = %+v, want the title The second", th)
	}
	if code, body, _ := chatPost(t, r, ck, s, chatPath+"/rename", url.Values{"conversation": {"2"}, "title": {strings.Repeat("x", 81)}}); code != http.StatusBadRequest {
		t.Fatalf("an 81-character title = %d %q, want 400", code, body)
	}
}

// The inbox (roadmap 239): the list makes it and pins it first; a poke in
// it counts unread; reading another thread leaves its badge, and opening
// it clears it.
func TestChatInboxBadge(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{say("first reply")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"hello"}})
	list := func() []factoryd.ChatConversation {
		t.Helper()
		rec := r.do(httptest.NewRequest(http.MethodGet, chatPath+"/conversations", nil), ck)
		var out []factoryd.ChatConversation
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(rec.Code, err)
		}
		return out
	}
	unread := func(l []factoryd.ChatConversation) map[int64]int64 {
		out := map[int64]int64{}
		for _, c := range l {
			out[c.Number] = c.Unread
		}
		return out
	}
	first := list()
	if len(first) != 2 || !first[0].Inbox || first[0].Number != 2 {
		t.Fatalf("the list = %+v, want the inbox 2 first, then thread 1", first)
	}
	poke := factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessagePoke, Content: "#7 failed: Feed page.", Conversation: 2}
	if err := chat.AddChatMessage(t.Context(), "sf/code", "sebastian", poke); err != nil {
		t.Fatal(err)
	}
	equal(t, "the unread turns after the poke", unread(list()), map[int64]int64{1: 1, 2: 1})
	r.do(httptest.NewRequest(http.MethodGet, chatPath+"?conversation=1", nil), ck)
	equal(t, "the unread turns after thread 1 was read", unread(list()), map[int64]int64{1: 0, 2: 1})
	r.do(httptest.NewRequest(http.MethodGet, chatPath+"?conversation=2", nil), ck)
	equal(t, "the unread turns after the inbox was read", unread(list()), map[int64]int64{1: 0, 2: 0})
}

// Archive hides a thread and keeps its rows, and restore brings it back;
// delete takes every row of it for good and writes one event naming the
// member, the thread and the count, while the card's events stay; the
// inbox stays, and a thread with a turn in flight waits (roadmap 239).
func TestChatArchiveAndDelete(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{say("first reply")}, {say("second reply")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"hello"}})
	chatPost(t, r, ck, s, chatPath+"/new", nil)
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"world"}})
	state := func(n int64) string {
		th, ok, _ := chat.Conversation(t.Context(), "sf/code", "sebastian", n)
		if !ok {
			return "gone"
		}
		return th.State
	}
	post := func(path string, form url.Values) (int, string) {
		t.Helper()
		code, body, _ := chatPost(t, r, ck, s, chatPath+path, form)
		return code, strings.Join(body, "")
	}
	if code, body := post("/archive", url.Values{"conversation": {"1"}}); code != http.StatusNoContent || state(1) != "archived" {
		t.Fatalf("archive = %d %q, thread 1 %s", code, body, state(1))
	}
	if code, body := post("", url.Values{"message": {"again"}, "conversation": {"1"}}); code != http.StatusConflict || body != "Conversation 1 is archived: restore it to write in it." {
		t.Fatalf("a message to an archived thread = %d %q", code, body)
	}
	if code, _ := post("/archive", url.Values{"conversation": {"1"}, "restore": {"1"}}); code != http.StatusNoContent || state(1) != "active" {
		t.Fatalf("restore = %d, thread 1 %s", code, state(1))
	}
	inbox, _ := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	in := url.Values{"conversation": {strconv.FormatInt(inbox, 10)}}
	if code, _ := post("/archive", in); code != http.StatusNotFound {
		t.Errorf("archiving the inbox = %d, want 404", code)
	}
	if code, _ := post("/delete", in); code != http.StatusNotFound {
		t.Errorf("deleting the inbox = %d, want 404", code)
	}

	key := threadKey("sf/code", "sebastian", 2)
	r.c.turns.enter(key)
	code, body := post("/delete", url.Values{"conversation": {"2"}})
	r.c.turns.leave(key)
	equal(t, "a delete while a turn runs", []any{code, body}, []any{http.StatusConflict, "A turn runs in conversation 2: wait for it to end."})

	filed := factoryd.Event{Repository: "sf/code", TaskID: "t-m", IssueNumber: 5, Kind: factoryd.EventCardFiled, Actor: "sebastian", Via: factoryd.ViaChat,
		Detail: map[string]any{"thread": float64(2)}}
	if _, err := r.events.Record(t.Context(), filed); err != nil {
		t.Fatal(err)
	}
	before := r.events.count()
	if code, body := post("/delete", url.Values{"conversation": {"2"}}); code != http.StatusNoContent {
		t.Fatalf("delete = %d %q", code, body)
	}
	rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 2, 10)
	equal(t, "thread 2 after the delete", []any{len(rows), state(2)}, []any{0, "gone"})
	after := r.events.all()[before:]
	if len(after) != 1 {
		t.Fatalf("%d event rows of the delete, want 1: %+v", len(after), after)
	}
	equal(t, "the delete's event", after[0], factoryd.Event{ID: after[0].ID, At: after[0].At, Repository: "sf/code", Kind: factoryd.EventChatThreadDeleted,
		Actor: "sebastian", Via: factoryd.ViaButton, Detail: map[string]any{"thread": int64(2), "messages": int64(2)}})
	r.events.mu.Lock()
	kept := slices.ContainsFunc(r.events.rows, func(e factoryd.Event) bool { return e.Kind == factoryd.EventCardFiled && e.TaskID == "t-m" })
	r.events.mu.Unlock()
	if !kept {
		t.Error("the card's filing event went with the thread")
	}
	if code, _ := post("/delete", url.Values{"conversation": {"2"}}); code != http.StatusNotFound {
		t.Errorf("a second delete = %d, want 404", code)
	}
}
