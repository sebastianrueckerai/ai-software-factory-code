package console

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// inboxRows stores rows on login's inbox of the code repository, made
// first, and answers their ids in order.
func inboxRows(t *testing.T, chat *fakeChat, login string, rows ...factoryd.ChatMessage) []int64 {
	t.Helper()
	inbox, err := chat.EnsureInbox(t.Context(), "sf/code", login)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, m := range rows {
		m.Conversation = inbox
		id, err := chat.AddChatMessage(t.Context(), "sf/code", login, m)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func poke(content, task string) factoryd.ChatMessage {
	return factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessagePoke, Content: content, TaskID: task}
}

func escalation(content, task string) factoryd.ChatMessage {
	return factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageEscalation, Content: content, TaskID: task, Choices: []string{"Yes", "No"}}
}

// The inbox read is the reader's own inbox when the project pokes them,
// else the first poked human's, the one the Waker wakes; with no notify
// list the approvers are the poked humans, and with neither nobody is.
func TestChatInboxReadsTheVenueMembersInbox(t *testing.T) {
	for _, c := range []struct {
		notify     []string
		who, whose string
	}{
		{[]string{"alice", "bob"}, "sebastian", "alice"},
		{[]string{"alice", "bob"}, "bob", "bob"},
		{nil, "alice", "sebastian"},
	} {
		r, chat, g := newChatRig(t)
		r.repoWriters["sf/code"] = []string{"sebastian", "alice", "bob"}
		r.forgetWriters()
		r.projects.rows[0].Notify = c.notify
		ids := map[string]int64{}
		for _, login := range []string{"sebastian", "alice", "bob"} {
			ids[login] = inboxRows(t, chat, login, poke("for "+login, ""))[0]
		}
		memberLogin(t, r, c.who)
		out := reads(t, r, g, "inbox", `{}`)
		equal(t, c.who+"'s read", out[0], fmt.Sprintf("The inbox of %s on ai-software-factory-code:\n"+
			"%d 10-11 09:30Z poke: for %s (open)\nThat is the whole window.", c.whose, ids[c.whose], c.whose))
	}

	r, _, g := newChatRig(t)
	r.ledger.approvers = []string{}
	equal(t, "a project that pokes nobody", reads(t, r, g, "inbox", `{}`)[0],
		"No inbox serves ai-software-factory-code: its policy names no notify list and no approvers for the factory to poke.")
}

// A human nothing has poked has no inbox, and the read makes none.
func TestChatInboxNamesNoInboxYet(t *testing.T) {
	r, chat, g := newChatRig(t)
	out := reads(t, r, g, "inbox", `{}`)
	equal(t, "the read", out[0], "No inbox exists yet for sebastian on ai-software-factory-code: nothing has poked it.")
	list, err := chat.Conversations(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	for _, th := range list {
		if th.Inbox {
			t.Fatalf("the read made an inbox: %+v", list)
		}
	}
}

// An open ask leads the page whatever its age, then the rest newest
// first: #13 waits on a human, the two pokes about #11 nobody has read.
func TestChatInboxListsOpenAsksFirst(t *testing.T) {
	r, chat, g := newChatRig(t)
	ids := inboxRows(t, chat, "sebastian",
		escalation("#13 needs you: Use a replica?", "t-e"),
		poke("#11 started.", "t-r"),
		poke("#11 merged.", "t-r"))
	out := reads(t, r, g, "inbox", `{}`)
	equal(t, "the read", out[0], fmt.Sprintf("The inbox of sebastian on ai-software-factory-code:\n"+
		"%d 10-11 09:30Z escalation #13: #13 needs you: Use a replica? (open)\n"+
		"%d 10-11 09:30Z poke #11: #11 merged. (open)\n"+
		"%d 10-11 09:30Z poke #11: #11 started. (open)\n"+
		"That is the whole window.", ids[0], ids[2], ids[1]))
}

// query keeps the rows that hold it in any case, a query that holds in
// no row says so, and one out of 2 to 200 characters is refused.
func TestChatInboxFiltersByQuery(t *testing.T) {
	r, chat, g := newChatRig(t)
	ids := inboxRows(t, chat, "sebastian",
		poke("#11 started.", "t-r"),
		poke("#11 MERGED into main.", "t-r"),
		poke("#5 queued.", "t-m"),
		poke("#5 merged at last.", "t-m"))
	out := reads(t, r, g, "inbox", `{"query":"Merged"}`, "inbox", `{"query":"replica"}`)
	equal(t, "the hits", out[0], fmt.Sprintf("The inbox of sebastian on ai-software-factory-code:\n"+
		"%d 10-11 09:30Z poke #5: #5 merged at last. (open)\n"+
		"%d 10-11 09:30Z poke #11: #11 MERGED into main. (open)\n"+
		"That is the whole window.", ids[3], ids[1]))
	equal(t, "no hit", out[1], "The inbox of sebastian on ai-software-factory-code:\n"+
		`no row holds "replica"`+"\nThat is the whole window.")
	for _, q := range []string{"m", strings.Repeat("m", 201)} {
		code, msg := refused(t, r, "inbox", toolInput{Query: q})
		equal(t, fmt.Sprintf("a query of %d characters", len(q)), []any{code, msg}, []any{http.StatusBadRequest, "The query is 2 to 200 characters."})
	}
}

// A page holds 20 rows and names the id the next one reads below; the
// last page says the window ends, and a page below the oldest row is
// empty.
func TestChatInboxPagesWithBefore(t *testing.T) {
	r, chat, g := newChatRig(t)
	var rows []factoryd.ChatMessage
	for i := range 25 {
		rows = append(rows, poke(fmt.Sprintf("poke %d", i), ""))
	}
	ids := inboxRows(t, chat, "sebastian", rows...)
	line := func(i int) string { return fmt.Sprintf("%d 10-11 09:30Z poke: poke %d (open)", ids[i], i) }
	var first, second []string
	for i := 24; i >= 5; i-- {
		first = append(first, line(i))
	}
	for i := 4; i >= 0; i-- {
		second = append(second, line(i))
	}
	head := "The inbox of sebastian on ai-software-factory-code:\n"
	out := reads(t, r, g, "inbox", `{}`)
	equal(t, "page one", out[0], head+strings.Join(first, "\n")+fmt.Sprintf("\nbefore=%d reads on.", ids[5]))
	out = reads(t, r, g, "inbox", fmt.Sprintf(`{"before":%d}`, ids[5]), "inbox", fmt.Sprintf(`{"before":%d}`, ids[0]))
	equal(t, "page two", out[0], head+strings.Join(second, "\n")+"\nThat is the whole window.")
	equal(t, "below the oldest row", out[1], head+fmt.Sprintf("no row older than row %d\nThat is the whole window.", ids[0]))
}

// A settled ask reads its reason line as the page shows it: #13's answer
// an hour after the row, #11's card no longer waiting on a human with no
// event since the row.
func TestChatInboxNamesSettledReasons(t *testing.T) {
	r, chat, g := newChatRig(t)
	ids := inboxRows(t, chat, "sebastian",
		escalation("#13 needs you: Use a replica?", "t-e"),
		escalation("#11 needs you: rebase?", "t-r"))
	if _, err := r.events.Record(t.Context(), factoryd.Event{Repository: "sf/code", TaskID: "t-e", Kind: factoryd.EventQuestionAnswered,
		Actor: "sebastian", Via: factoryd.ViaButton, At: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	out := reads(t, r, g, "inbox", `{}`)
	equal(t, "the read", out[0], fmt.Sprintf("The inbox of sebastian on ai-software-factory-code:\n"+
		"%d 10-11 09:30Z escalation #11: #11 needs you: rebase? (moved on · 09:30)\n"+
		"%d 10-11 09:30Z escalation #13: #13 needs you: Use a replica? (answered by sebastian · 10:30)\n"+
		"That is the whole window.", ids[1], ids[0]))
}

// A row names what wrote it: the Waker's note a wake turn, the turn's
// rows their role, a round of calls without words the tools it called.
// Its words run on one line, cut at 100 bytes.
func TestChatInboxNamesEachRowsKind(t *testing.T) {
	r, chat, g := newChatRig(t)
	ids := inboxRows(t, chat, "sebastian",
		factoryd.ChatMessage{Role: "user", BirthKind: "wake_ask", Content: "Cards moved:\n#13 waits on you."},
		factoryd.ChatMessage{Role: "assistant", ToolCalls: []factoryd.ChatToolCall{{ID: "c1", Name: "card"}, {ID: "c2", Name: "board"}}},
		factoryd.ChatMessage{Role: "assistant", Content: "I read #13."},
		poke(strings.Repeat("x", 300), ""))
	out := reads(t, r, g, "inbox", `{}`)
	equal(t, "the read", out[0], fmt.Sprintf("The inbox of sebastian on ai-software-factory-code:\n"+
		"%d 10-11 09:30Z poke: %s… (open)\n"+
		"%d 10-11 09:30Z assistant: I read #13.\n"+
		"%d 10-11 09:30Z assistant: called card, board\n"+
		"%d 10-11 09:30Z wake turn: Cards moved: #13 waits on you.\n"+
		"That is the whole window.", ids[3], strings.Repeat("x", 100), ids[2], ids[1], ids[0]))
}
