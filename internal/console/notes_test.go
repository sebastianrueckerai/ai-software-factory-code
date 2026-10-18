package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// fakeNotes keeps the scratchpads as factoryd's table does: a write
// lands only on the base it read.
type fakeNotes struct {
	mu    sync.Mutex
	notes map[string]factoryd.Notes
	// race, when set, is written before the next write lands: another
	// thread's write between a read and a write
	race string
}

func (f *fakeNotes) Notes(_ context.Context, repository string) (factoryd.Notes, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.notes[repository], nil
}

func (f *fakeNotes) PutNotes(_ context.Context, repository, login, content, base string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notes == nil {
		f.notes = map[string]factoryd.Notes{}
	}
	if f.race != "" {
		f.notes[repository] = factoryd.Notes{Content: f.race, By: "anna", At: at}
		f.race = ""
	}
	if f.notes[repository].Hash() != base {
		return false, nil
	}
	f.notes[repository] = factoryd.Notes{Content: content, By: login, At: at}
	return true, nil
}

// The scratchpad: empty, a note appended, a second as its own paragraph,
// a passage replaced; another thread's write between a read and a write
// is read again and kept; past the cap the write refuses and names the
// way out (roadmap 239).
func TestChatNotes(t *testing.T) {
	r, _, _ := newChatRig(t)
	notes := &fakeNotes{}
	r.c.cfg.Notes = notes
	_, s := r.loggedIn(t)
	var events []string
	trn := r.chatTurnOf(t, s, &events)
	got, _ := trn.notesText()
	equal(t, "the empty scratchpad", got, "The scratchpad of ai-software-factory-code is empty; notes_write with text starts it.")
	steps := []struct{ text, old, new, want string }{
		{"round 12: the cache plan", "", "", "The scratchpad holds 24 of 16384 bytes now."},
		{"open: the window", "", "", "The scratchpad holds 42 of 16384 bytes now."},
		{"", "the window", "the store", "The scratchpad holds 41 of 16384 bytes now."},
	}
	for _, st := range steps {
		got, err := trn.writeNotes(st.text, st.old, st.new)
		equal(t, "a write", []any{got, err}, []any{st.want, error(nil)})
	}
	got, _ = trn.notesText()
	equal(t, "the scratchpad", got, "The scratchpad of ai-software-factory-code: 41 of 16384 bytes, last written by sebastian at 2026-10-11 09:30Z.\n\n"+
		"round 12: the cache plan\n\nopen: the store")
	notes.race = "anna's note"
	got, _ = trn.writeNotes("mine", "", "")
	equal(t, "the write after a race", []any{got, notes.notes["sf/code"].Content}, []any{"The scratchpad holds 17 of 16384 bytes now.", "anna's note\n\nmine"})
	_, err := trn.writeNotes(strings.Repeat("x", notesMax), "", "")
	equal(t, "past the cap", err.Error(), "the scratchpad would be 16403 bytes, over its cap of 16384: remove what graduated into the roadmap first (old with new empty)")
	_, err = trn.writeNotes("a", "b", "c")
	equal(t, "text and old", err.Error(), "notes_write takes text to append, or old and new to replace a passage")
}

// active_threads: the project's every member's active threads with their
// newest message and a running turn's mark, then the scratchpad's tail.
func TestChatActiveThreads(t *testing.T) {
	r, chat, g := newChatRig(t)
	r.c.cfg.Notes = &fakeNotes{notes: map[string]factoryd.Notes{"sf/code": {Content: "round 12", By: "anna", At: at}}}
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{say("one")}, {say("two")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"Plan the members page"}})
	chatPost(t, r, ck, s, chatPath+"/new", nil)
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"Fix the costs page"}})
	r.c.turns.enter(threadKey("sf/code", "sebastian", 1))
	defer r.c.turns.leave(threadKey("sf/code", "sebastian", 1))
	var events []string
	trn := r.chatTurnOf(t, s, &events)
	trn.conv = 2
	got, err := trn.activeThreads()
	th, _, _ := chat.Conversation(t.Context(), "sf/code", "sebastian", 1)
	equal(t, "the read", []any{got, err}, []any{"The project's active threads on ai-software-factory-code:\n" +
		`- thread 1 "Plan the members page", last turn ` + th.LastAt.UTC().Format(stampLayout) + `, a turn running: sebastian's newest message "Plan the members page"` + "\n" +
		"\n\nThe scratchpad's tail (written by anna at 2026-10-11 09:30Z):\nround 12", error(nil)})
}

// A card a chat thread filed or steered names that thread in its drawer,
// read as the store answers the event (JSON numbers): the member's own
// thread as a button that opens it, another member's by name (roadmap
// 239's card link).
func TestDrawerNamesTheChatThread(t *testing.T) {
	r, _, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{say("ok")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"Have #11 rebase"}})
	steered := func(who string) {
		t.Helper()
		if _, err := r.events.Record(t.Context(), factoryd.Event{Repository: "sf/code", TaskID: "t-r", IssueNumber: 11, Kind: factoryd.EventCardSteered,
			Actor: who, Via: factoryd.ViaChat, Detail: map[string]any{"text": "Rebase.", "thread": float64(1)}}); err != nil {
			t.Fatal(err)
		}
	}
	drawer := func() string {
		t.Helper()
		rec := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-r/drawer?tab=overview", nil), ck)
		if rec.Code != http.StatusOK {
			t.Fatalf("the drawer = %d", rec.Code)
		}
		return rec.Body.String()
	}
	if strings.Contains(drawer(), "thread-link") {
		t.Fatal("a card no thread touched names one")
	}
	steered("sebastian")
	if want := `<p class="thread-link">From your chat thread <button type="button" class="linkbtn" data-open-thread="1">Have #11 rebase</button></p>`; !strings.Contains(drawer(), want) {
		t.Fatalf("the drawer lacks %s", want)
	}
	r.events.mu.Lock()
	r.events.rows = nil
	r.events.mu.Unlock()
	steered("anna")
	if want := `<p class="thread-link">From anna's chat thread 1</p>`; !strings.Contains(drawer(), want) {
		t.Fatalf("the drawer lacks %s", want)
	}
}
