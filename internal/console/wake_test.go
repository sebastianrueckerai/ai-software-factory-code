package console

import (
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// A wake turn runs without a request: the Waker's call alone writes the
// user row with its birth kind and answers as the model. The member never
// logged in here, so the turn runs on a bare session.
func TestStartWakeRunsATurnWithoutHTTP(t *testing.T) {
	r, chat, g := newChatRig(t)
	g.replies = [][]string{{say("I looked.")}}
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "wake_ask", "Wake: 2 asks wait."); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	if calls := g.calls(); len(calls) != 1 {
		t.Fatalf("%d model calls, want 1", len(calls))
	}
	rows, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", conv, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Role != "user" || rows[0].BirthKind != "wake_ask" || rows[0].Content != "Wake: 2 asks wait." ||
		rows[1].Role != "assistant" || rows[1].Content != "I looked." {
		t.Fatalf("the wake's rows = %+v", rows)
	}
}

// A wake on a thread whose turn runs waits for it: its user row lands
// queued while the member's turn holds the thread, and its turn starts
// only after theirs ends.
func TestStartWakeQueuesBehindALiveTurn(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{say("the member's answer")}, {say("the wake's answer")}}
	g.hold, g.arrived = make(chan struct{}), make(chan struct{}, 2)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		code, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"what is open?"}})
		if code != 200 || len(events) == 0 {
			t.Errorf("the member's turn = %d %v", code, events)
		}
	}()
	<-g.arrived // the member's model call is in flight and held
	wakeDone := make(chan error, 1)
	go func() {
		wakeDone <- r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", 1, "wake_ask", "Wake: 1 ask waits.")
	}()
	queued := waitFor(t, time.Second, func() bool {
		rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 1, 10)
		return len(rows) == 2 && rows[1].Queued && rows[1].BirthKind == "wake_ask"
	})
	if !queued {
		rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 1, 10)
		t.Fatalf("the wake's row did not land queued while the member's turn ran: %+v", rows)
	}
	close(g.hold)
	if err := <-wakeDone; err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	wg.Wait()
	rows, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	var user, assistant int
	for _, m := range rows {
		if m.BirthKind == "wake_ask" && m.Role == "user" {
			user++
			if m.Queued {
				t.Errorf("the wake's user row stayed queued: %+v", m)
			}
		}
		if m.Role == "assistant" && m.Content == "the wake's answer" {
			assistant++
		}
	}
	if user != 1 || assistant != 1 {
		t.Fatalf("the wake's rows after both turns = %+v", rows)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, within time.Duration, cond func() bool) bool {
	t.Helper()
	for start := time.Now(); time.Since(start) < within; {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// The two refusals a wake cannot outlive name the sentinel the Waker
// drops its row on: a thread that is not there any more and an archived
// one are both gone for good, never a cooldown.
func TestStartWakeRefusalNamesAVenueThatIsGone(t *testing.T) {
	r, chat, _ := newChatRig(t)
	conv, err := chat.NewConversation(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chat.ArchiveConversation(t.Context(), "sf/code", "sebastian", conv, true); err != nil {
		t.Fatal(err)
	}
	for name, n := range map[string]int64{"archived": conv, "missing": conv + 400} {
		err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", n, "wake_ask", "Wake: 1 ask waits.")
		if !errors.Is(err, factoryd.ErrWakeVenueGone) {
			t.Fatalf("%s venue: StartWake returned %v, want ErrWakeVenueGone", name, err)
		}
	}
}
