package console

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
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

// A human nothing has poked has no inbox, and the read makes none: the
// reader's own, or the venue human's when the project pokes another.
func TestChatInboxNamesNoInboxYet(t *testing.T) {
	for _, c := range []struct {
		notify []string
		whose  string
	}{
		{nil, "sebastian"},
		{[]string{"alice"}, "alice"},
	} {
		r, chat, g := newChatRig(t)
		r.projects.rows[0].Notify = c.notify
		out := reads(t, r, g, "inbox", `{}`)
		equal(t, "the read of "+c.whose+"'s inbox", out[0], "No inbox exists yet for "+c.whose+" on ai-software-factory-code: nothing has poked it.")
		for _, login := range []string{"sebastian", "alice"} {
			list, err := chat.Conversations(t.Context(), "sf/code", login)
			if err != nil {
				t.Fatal(err)
			}
			if slices.ContainsFunc(list, func(th factoryd.ChatConversation) bool { return th.Inbox }) {
				t.Fatalf("the read of %s's inbox made one for %s: %+v", c.whose, login, list)
			}
		}
	}
}

// A wake's ask in the inbox stands past the rows that answer nothing, a
// poke here: the inbox shows it open with its choices and the inbox read
// lists it open. A press answers it.
func TestChatInboxAskStandsPastAPoke(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	inbox, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	g.replies = [][]string{{call(1, "ask", `{"ask":"Snapshots nightly?","choices":["Yes","No"]}`)}}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", inbox, "wake_ask", "#13 asks about snapshots."); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	pokeID := inboxRows(t, chat, "sebastian", poke("#11 merged.", "t-r"))[0]
	path := chatPath + "?conversation=" + strconv.FormatInt(inbox, 10)
	ask := func() factoryd.ChatMessage {
		t.Helper()
		msgs := chatRead(t, r, ck, path).Messages
		i := slices.IndexFunc(msgs, func(m factoryd.ChatMessage) bool { return m.Kind == factoryd.MessageAsk })
		if i < 0 {
			t.Fatalf("the inbox holds no ask: %+v", msgs)
		}
		return msgs[i]
	}
	row := ask()
	equal(t, "the ask past the poke", []any{row.Settled, row.Choices}, []any{false, []string{"Yes", "No"}})
	out := reads(t, r, g, "inbox", `{}`)
	equal(t, "the inbox read", out[0], fmt.Sprintf("The inbox of sebastian on ai-software-factory-code:\n"+
		"%d 10-11 09:30Z ask: Snapshots nightly? (open)\n"+
		"%d 10-11 09:30Z poke #11: #11 merged. (seen)\n"+
		"%d 10-11 09:30Z wake turn: #13 asks about snapshots.\n"+
		"That is the whole window.", row.ID, pokeID, row.ID-1))

	g.replies = [][]string{{say("Nightly, then.")}}
	if code, events, _ := chatPost(t, r, ck, s, chatPath+"/answer", url.Values{"conversation": {strconv.FormatInt(inbox, 10)},
		"message": {strconv.FormatInt(row.ID, 10)}, "choice": {"Yes"}}); code != http.StatusOK {
		t.Fatalf("the press = %d %v", code, events)
	}
	equal(t, "the ask after the press", ask().Settled, true)
}

// An open ask leads the first page whatever its age, then the rest newest
// first: #13 waits on a human, the two pokes about #11 nobody has read. A
// page below before leaves the ask out, though it is older.
func TestChatInboxListsOpenAsksFirst(t *testing.T) {
	r, chat, g := newChatRig(t)
	ids := inboxRows(t, chat, "sebastian",
		escalation("#13 needs you: Use a replica?", "t-e"),
		poke("#11 started.", "t-r"),
		poke("#11 merged.", "t-r"))
	out := reads(t, r, g, "inbox", `{}`, "inbox", fmt.Sprintf(`{"before":%d}`, ids[2]))
	equal(t, "the first page", out[0], fmt.Sprintf("The inbox of sebastian on ai-software-factory-code:\n"+
		"%d 10-11 09:30Z escalation #13: #13 needs you: Use a replica? (open)\n"+
		"%d 10-11 09:30Z poke #11: #11 merged. (open)\n"+
		"%d 10-11 09:30Z poke #11: #11 started. (open)\n"+
		"That is the whole window.", ids[0], ids[2], ids[1]))
	equal(t, "below the newest poke", out[1], fmt.Sprintf("The inbox of sebastian on ai-software-factory-code:\n"+
		"%d 10-11 09:30Z poke #11: #11 started. (open)\n"+
		"That is the whole window.", ids[1]))
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

// A page holds 20 rows besides the open asks and names the id the next
// one reads below; the last page says the window ends, and a page below
// the oldest row is empty. The open asks, 25 of them between 23 pokes,
// fill the first page whole and never show again.
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

	r, chat, g = newChatRig(t)
	rows = nil
	var askAt, pokeAt []int
	for i := range 25 {
		askAt = append(askAt, len(rows))
		rows = append(rows, escalation(fmt.Sprintf("ask %d", i), "t-e"))
		if i < 23 {
			pokeAt = append(pokeAt, len(rows))
			rows = append(rows, poke(fmt.Sprintf("poke %d", i), ""))
		}
	}
	ids = inboxRows(t, chat, "sebastian", rows...)
	ask := func(i int) string {
		return fmt.Sprintf("%d 10-11 09:30Z escalation #13: ask %d (open)", ids[askAt[i]], i)
	}
	pokeLine := func(i int) string { return fmt.Sprintf("%d 10-11 09:30Z poke: poke %d (open)", ids[pokeAt[i]], i) }
	first, second = nil, nil
	for i := 24; i >= 0; i-- {
		first = append(first, ask(i))
	}
	for i := 22; i >= 3; i-- {
		first = append(first, pokeLine(i))
	}
	for i := 2; i >= 0; i-- {
		second = append(second, pokeLine(i))
	}
	out = reads(t, r, g, "inbox", `{}`, "inbox", fmt.Sprintf(`{"before":%d}`, ids[pokeAt[3]]))
	equal(t, "the asks' first page", out[0], head+strings.Join(first, "\n")+fmt.Sprintf("\nbefore=%d reads on.", ids[pokeAt[3]]))
	equal(t, "the pokes past it", out[1], head+strings.Join(second, "\n")+"\nThat is the whole window.")
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

// An ended card's newest rows are read too: the monitor's ask that asks
// its issue closed stays open while no row names its case, and another
// monitor's ask on that card moves on.
func TestChatAskFactsReadAnEndedCardsNewestRows(t *testing.T) {
	r, _, _ := newChatRig(t)
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-d", Kind: "agent", Status: "done", Repository: "sf/code", IssueNumber: 17}})
	var rows []factoryd.ChatMessage
	for i, flag := range []string{"issue_open_after_end", "checkin_cut"} {
		question := fmt.Sprintf("The monitor flagged this card: what should happen? Answer case %d in the project chat.", 21+i)
		if _, err := r.events.Record(t.Context(), factoryd.Event{Repository: "sf/code", TaskID: "t-d", Kind: factoryd.EventQuestionAsked, Actor: factoryd.FactoryDecider,
			Via: factoryd.ViaFactoryd, At: at, Detail: map[string]any{"question": question, "case_id": float64(21 + i), "case_kind": string(factoryd.CaseMonitorFlag), "flag": flag}}); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, factoryd.ChatMessage{Kind: factoryd.MessageEscalation, BirthKind: factoryd.EventQuestionAsked, TaskID: "t-d", At: at.Add(time.Second),
			Content: "#17 needs you: " + question})
	}
	statuses, events := askFacts(t.Context(), r.c, []string{"t-d"})
	var words []string
	for _, m := range rows {
		words = append(words, factoryd.SettleOf(m, events["t-d"], statuses["t-d"], false).Word)
	}
	if !slices.Equal(words, []string{"", "moved on"}) {
		t.Fatalf("the ended card's monitor asks settle as %q, want the issue's open and the other moved on", words)
	}
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
