package console

import (
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A wake turn runs without a request: the Waker's call alone writes the
// user row with its birth kind and answers as the model. The member never
// logged in here; the turn needs no login of theirs.
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

// A wake is the project agent's own turn: every Forgejo call of it goes
// out as factory-chat with the project's chat token. The woken member's
// login, whose tokens here expire within the refresh margin, is neither
// read nor refreshed.
func TestAWakeActsAsTheChatBot(t *testing.T) {
	r, chat, g := newChatRig(t)
	r.fg.expiresIn = 30
	r.loggedIn(t)
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	r.fg.mu.Lock()
	calls, grants := len(r.fg.calls), len(r.fg.grants)
	r.fg.mu.Unlock()
	r.sessions.mu.Lock()
	reads, updates := r.sessions.reads, r.sessions.tokens
	r.sessions.mu.Unlock()
	g.replies = [][]string{{call(1, "comment", `{"task_id":"t-m","text":"Looks right."}`)}, {say("Commented on #5.")}}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "wake_ask", "Wake: 1 ask waits."); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	r.fg.mu.Lock()
	wake, granted := slices.Clone(r.fg.calls[calls:]), len(r.fg.grants)-grants
	r.fg.mu.Unlock()
	if !slices.Contains(wake, "POST /api/v1/repos/sf/code/issues/5/comments Bearer chat-token") {
		t.Fatalf("the wake's Forgejo calls = %q, want the comment posted with the chat token", wake)
	}
	for _, c := range wake {
		if !strings.HasSuffix(c, " Bearer chat-token") {
			t.Errorf("the wake called %q, not as the chat bot", c)
		}
	}
	if got := r.fg.commentBodies("/api/v1/repos/sf/code/issues/5/comments"); !slices.Equal(got, []string{"Looks right."}) {
		t.Errorf("comments on #5 = %q", got)
	}
	r.sessions.mu.Lock()
	defer r.sessions.mu.Unlock()
	if granted != 0 || r.sessions.reads != reads || r.sessions.tokens != updates {
		t.Errorf("the wake made %d token grants, %d session reads and %d token updates; want none", granted, r.sessions.reads-reads, r.sessions.tokens-updates)
	}
}

// A project without a chat token has no identity a wake can act as: the
// wake is refused before the model is called or anything is stored, and
// the refusal names tokens.chat.
func TestAWakeOnAProjectWithoutAChatTokenIsRefused(t *testing.T) {
	r, chat, g := newChatRig(t)
	r.chatToken = ""
	r.loggedIn(t)
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	g.replies = [][]string{{call(1, "comment", `{"task_id":"t-m","text":"Looks right."}`)}, {say("Commented on #5.")}}
	err = r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "wake_ask", "Wake: 1 ask waits.")
	if err == nil || !strings.Contains(err.Error(), "ai-software-factory-code") || !strings.Contains(err.Error(), "tokens.chat") {
		t.Fatalf("StartWake = %v, want a refusal naming the project and tokens.chat", err)
	}
	rows, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", conv, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || len(g.calls()) != 0 || len(r.fg.commentBodies("/api/v1/repos/sf/code/issues/5/comments")) != 0 {
		t.Fatalf("the refused wake stored %+v, made %d model calls and commented", rows, len(g.calls()))
	}
}

// A wake's approval is the factory's own: the merge request's approval
// goes to the workflow as factory-chat's and no review is posted, as
// factory-chat's would count for nothing; the merge gate's review carries
// it at the merge. A red-checks request reads only the head's checks, with
// the chat token. A resource request is decided as factory-chat. The
// events stay the woken member's, decided by the agent.
func TestAWakesApprovalIsTheFactorysOwn(t *testing.T) {
	approve := func(t *testing.T, r *rig, id int64) string {
		t.Helper()
		s, err := r.c.wakeSession(factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}, "sebastian")
		if err != nil {
			t.Fatal(err)
		}
		note, err := r.c.doApprove(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaChat, wake: true, decider: "agent",
			level: policy.AutonomyAutonomous, mergeRule: policy.MergeAuto}, id)
		if err != nil {
			t.Fatalf("approve %d: %v", id, err)
		}
		return note
	}
	approved := func(t *testing.T, r *rig) factoryd.Event {
		t.Helper()
		rows := r.events.all()
		if len(rows) != 1 || rows[0].Kind != factoryd.EventMergeApproved || rows[0].Actor != "sebastian" || rows[0].Detail["decider"] != "agent" {
			t.Fatalf("events = %+v, want one merge_approved of sebastian decided by the agent", rows)
		}
		return rows[0]
	}
	t.Run("merge", func(t *testing.T) {
		r := newRig(t)
		if note := approve(t, r, 4); note != "Approved pull request #9; factoryd merges it." {
			t.Fatalf("note = %q", note)
		}
		if !slices.Equal(r.fg.calls, []string{"SIGNAL approved t-m #9 by factory-chat"}) || len(r.fg.reviews) != 0 {
			t.Fatalf("calls %q, reviews %v: want the approval signalled as factory-chat's and no review", r.fg.calls, r.fg.reviews)
		}
		approved(t, r)
	})
	t.Run("merge with red checks", func(t *testing.T) {
		r := newRig(t)
		r.approvals.rows[1].Spec = json.RawMessage(`{"risk_class":"standard","head_sha":"abc123","red_checks":true}`)
		r.fg.statuses = map[string]string{"abc123": `{"state":"success","statuses":[{"context":"ci / test","status":"success"}]}`}
		approve(t, r, 4)
		if !slices.Equal(r.fg.calls, []string{"GET /api/v1/repos/sf/code/commits/abc123/status Bearer chat-token", "SIGNAL approved t-m #9 by factory-chat"}) ||
			len(r.fg.reviews) != 0 {
			t.Fatalf("calls %q, reviews %v: want the head's checks read with the chat token, the approval signalled, no review", r.fg.calls, r.fg.reviews)
		}
	})
	t.Run("resource", func(t *testing.T) {
		r := newRig(t)
		if note := approve(t, r, 3); note != "Approved the resource for its task." {
			t.Fatalf("note = %q", note)
		}
		if q := r.approvals.get(3); q.Status != "approved" || q.DecidedBy != "factory-chat" || len(r.fg.calls) != 0 {
			t.Fatalf("request 3 %s by %s, calls %q: want it approved by factory-chat with no Forgejo call", q.Status, q.DecidedBy, r.fg.calls)
		}
		approved(t, r)
	})
}
