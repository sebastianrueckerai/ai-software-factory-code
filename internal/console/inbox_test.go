package console

import (
	"html"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// Every birth kind the factory writes has its inbox rule, every rule
// names a birth kind, and every note that wakes the agent is not for the
// human.
func TestEveryBirthKindHasAnInboxRule(t *testing.T) {
	for _, birth := range factoryd.BirthKinds() {
		k, ok := inboxKinds[birth]
		if !ok || k.Render == nil {
			t.Errorf("the inbox has no rule for the birth kind %q", birth)
		}
	}
	for birth := range inboxKinds {
		if !slices.Contains(factoryd.BirthKinds(), birth) {
			t.Errorf("the inbox has a rule for %q, which BirthKinds lacks", birth)
		}
	}
	for _, wake := range []string{factoryd.WakeAsk, factoryd.EventCaseClimbed, factoryd.WakeCard, factoryd.WakeTimer, factoryd.WakeRefill} {
		if inboxKinds[wake].ForHuman {
			t.Errorf("the note %q shows to the human in full", wake)
		}
	}
}

// A case_climbed note of 11,000 characters and the agent's reply of 500
// render as one line of at most 160 characters, which leads with what the
// human must do, here nothing, and names the card by its issue and title;
// the whole turn stays behind the line. A reply that asks the human, or
// files a card for them, says so. A turn that failed says so and that
// nothing was looked at, its error behind the line, as does one that ended
// without a word; a turn in flight is under way, whatever it said so far.
func TestWakeNotesFoldToOneLine(t *testing.T) {
	note := "Wake: 1 case waits for the factory agent: #2084.\n#2084:\n" + strings.Repeat("The criteria gate ran twice. ", 400)
	note = note[:11000]
	reply := strings.Repeat("Nothing here needs a human. ", 18)[:500]
	escalated := factoryd.ChatMessage{Role: "assistant", ToolCalls: []factoryd.ChatToolCall{{Name: "escalate", Result: "Filed the escalation as #2190 in Needs you."}}}
	for _, c := range []struct {
		name  string
		reply []factoryd.ChatMessage
		live  bool
		want  string
	}{
		{"a reply in words", []factoryd.ChatMessage{{Role: "assistant", Content: reply}}, false,
			"Nothing for you. The agent looked at #2084 (Build the criteria gate)."},
		{"an escalation", []factoryd.ChatMessage{escalated, {Role: "assistant", Content: reply}}, false,
			"The agent asks you on a card. It looked at #2084 (Build the criteria gate)."},
		{"a refused escalation", []factoryd.ChatMessage{{Role: "assistant", ToolCalls: []factoryd.ChatToolCall{{Name: "escalate", Status: 400, Result: "Refused: ask: 1 to 255 characters"}}}}, false,
			"Nothing for you. The agent looked at #2084 (Build the criteria gate)."},
		{"an ask of its own", []factoryd.ChatMessage{{Role: "assistant", Kind: factoryd.MessageAsk, Content: "Raise the cap?", Choices: []string{"Yes", "No"}}}, false,
			"The agent asks you below. It looked at #2084 (Build the criteria gate)."},
		{"a failed turn", []factoryd.ChatMessage{{Role: "assistant", Content: turnFailed + "the model gateway answered 502"}}, false,
			"The agent's turn on #2084 (Build the criteria gate) failed: nothing was looked at."},
		{"a failed turn after an escalation", []factoryd.ChatMessage{escalated, {Role: "assistant", Content: turnFailed + "the model gateway answered 502"}}, false,
			"The agent asks you on a card. It looked at #2084 (Build the criteria gate)."},
		{"a turn under way", nil, true, "The agent is looking at #2084 (Build the criteria gate)."},
		{"a turn under way after its first round", []factoryd.ChatMessage{{Role: "assistant", ToolCalls: []factoryd.ChatToolCall{{Name: "card", Result: "#2084 waits for a human."}}}}, true,
			"The agent is looking at #2084 (Build the criteria gate)."},
		{"a turn that ended without a word", nil, false, "The agent's turn on #2084 (Build the criteria gate) ended without a word: nothing was looked at."},
	} {
		r, chat, _ := newChatRig(t)
		ck, _ := r.loggedIn(t)
		r.tasks.set("", factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 2084,
			Objective: "Build the criteria gate"})
		rows := append([]factoryd.ChatMessage{{Role: "user", BirthKind: factoryd.EventCaseClimbed, Content: note}}, c.reply...)
		inboxRows(t, chat, "sebastian", rows...)
		inbox, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
		if err != nil {
			t.Fatal(err)
		}
		if c.live {
			r.c.turns.enter(threadKey("sf/code", "sebastian", inbox))
		}
		msgs := chatRead(t, r, ck, chatPath+"?conversation="+strconv.FormatInt(inbox, 10)).Messages
		if len(msgs) != len(rows) {
			t.Fatalf("%s: %d rows, want %d", c.name, len(msgs), len(rows))
		}
		if msgs[0].Line != c.want || utf8.RuneCountInString(msgs[0].Line) > 160 || msgs[0].Content != note {
			t.Errorf("%s: the line = %q (%d characters), want %q over the whole note", c.name, msgs[0].Line, utf8.RuneCountInString(msgs[0].Line), c.want)
		}
	}
}

// A wake on two cards whose names do not fit its line of 160 characters
// counts them.
func TestAWakeOnCardsPastItsLineCountsThem(t *testing.T) {
	r, chat, _ := newChatRig(t)
	ck, _ := r.loggedIn(t)
	long := strings.Repeat("Build the criteria gate and its ledger ", 3) // 117 characters
	r.tasks.set("", factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 2084, Objective: long},
		factoryd.Task{ID: "t-h", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 2085, Objective: long})
	inboxRows(t, chat, "sebastian", factoryd.ChatMessage{Role: "user", BirthKind: factoryd.WakeAsk, Content: "Wake: 2 asks wait for a human: #2084, #2085."},
		factoryd.ChatMessage{Role: "assistant", Content: "Both wait for a human's call."})
	inbox, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	msgs := chatRead(t, r, ck, chatPath+"?conversation="+strconv.FormatInt(inbox, 10)).Messages
	if want := "Nothing for you. The agent looked at 2 cards."; len(msgs) != 2 || msgs[0].Line != want {
		t.Fatalf("the line = %q, want %q", msgs[0].Line, want)
	}
}

// A message of the agent's past 600 characters shows its first 280, cut
// at a space, above a "more" fold, unless the human's last words asked for
// a report; a shorter one shows whole. A routine's report shows its first
// line above the fold.
func TestLongMessagesFold(t *testing.T) {
	long := strings.Repeat("word ", 130) // 650 characters
	r, chat, _ := newChatRig(t)
	ck, _ := r.loggedIn(t)
	inboxRows(t, chat, "sebastian",
		factoryd.ChatMessage{Role: "user", Content: "What changed?"},
		factoryd.ChatMessage{Role: "assistant", Content: long},
		factoryd.ChatMessage{Role: "assistant", Content: long[:550]},
		factoryd.ChatMessage{Role: "user", Content: "Give me the full report."},
		factoryd.ChatMessage{Role: "assistant", Content: long},
		factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessagePoke, BirthKind: factoryd.EventRoutineRan, Content: "Long waits\n\n" + long})
	inbox, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	msgs := chatRead(t, r, ck, chatPath+"?conversation="+strconv.FormatInt(inbox, 10)).Messages
	var got []string
	for _, m := range msgs {
		got = append(got, m.Short)
	}
	// 280 runes end on the space after the 56th word: the cut keeps 55
	// words and the 56th, 279 runes
	opening := strings.Repeat("word ", 55) + "word"
	if want := []string{"", opening, "", "", "", "Long waits"}; !slices.Equal(got, want) {
		t.Errorf("the openings = %q,\nwant %q", got, want)
	}
}

// The inbox's message of every request the gallery's cards wait on says
// the question its opened card asks: the card's ask is the message's one
// source, red checks included. The row is the request case's facts and
// problem, as factoryd writes them; a plan's request pokes nobody, as its
// questions' rows ask, and a criteria request's case pokes with its face's
// ask row.
func TestTheInboxSaysWhatTheCardAsks(t *testing.T) {
	question := regexp.MustCompile(`<p class="o-question">(.*?)</p>`)
	tags := regexp.MustCompile(`<[^>]*>`)
	asks := 0
	for _, fx := range facefixtures.All() {
		if fx.Approval == nil {
			continue
		}
		c, err := factoryd.RequestCase(*fx.Approval)
		if err != nil {
			t.Fatal(err)
		}
		c.Facts["kind"], c.Facts["problem"] = fx.Approval.Kind, c.Problem
		e := factoryd.Event{Kind: factoryd.EventApprovalRequested, TaskID: fx.Task.ID, Repository: fx.Task.Repository, Detail: c.Facts}
		if fx.Approval.Kind == factoryd.ApprovalCriteria || fx.Approval.Kind == factoryd.ApprovalCriteriaRerun {
			// a criteria request pokes nobody: its case's face does, with
			// the ask row it writes as the case reaches the human
			if m, ok := factoryd.PokeOf(e, fx.Task, factoryd.Task{}); ok {
				t.Errorf("%s/%s: the criteria request's own row pokes with %q", fx.Group, fx.Kind, m.Content)
			}
			if fx.Ask == nil {
				t.Errorf("%s/%s: the criteria request's case holds no ask", fx.Group, fx.Kind)
				continue
			}
			e = factoryd.Event{Kind: factoryd.EventQuestionAsked, TaskID: fx.Task.ID, Repository: fx.Task.Repository, Detail: fx.Ask.Detail}
		}
		m, ok := factoryd.PokeOf(e, fx.Task, factoryd.Task{})
		if !ok {
			continue
		}
		_, _, _, _, page := openedEntry(t, fx.Group, fx.Kind, "")
		got := question.FindStringSubmatch(page)
		if got == nil {
			t.Errorf("%s/%s: the opened card asks nothing", fx.Group, fx.Kind)
			continue
		}
		asks++
		if q := html.UnescapeString(tags.ReplaceAllString(got[1], "")); !strings.Contains(m.Content, q) {
			t.Errorf("%s/%s: the inbox says %q, not the card's ask %q", fx.Group, fx.Kind, m.Content, q)
		}
	}
	if asks < 10 {
		t.Errorf("%d requests compared, want every one the gallery holds", asks)
	}
}

// Every message the inbox writes on the gallery's cards holds at most 280
// characters, a request's and a card's ask alike.
func TestEveryInboxMessageFitsItsBound(t *testing.T) {
	n := 0
	for _, fx := range facefixtures.All() {
		var events []factoryd.Event
		if fx.Approval != nil {
			row, err := factoryd.RequestRow(*fx.Approval)
			if err != nil {
				t.Fatal(err)
			}
			events = append(events, factoryd.Event{Kind: factoryd.EventApprovalRequested, Detail: row})
		}
		if fx.Ask != nil {
			events = append(events, factoryd.Event{Kind: factoryd.EventQuestionAsked, Detail: fx.Ask.Detail})
		}
		var parent factoryd.Task
		if fx.Parent != nil {
			parent = *fx.Parent
		}
		for _, e := range events {
			e.TaskID, e.Repository = fx.Task.ID, fx.Task.Repository
			m, ok := factoryd.PokeOf(e, fx.Task, parent)
			if !ok {
				continue
			}
			n++
			if got := utf8.RuneCountInString(m.Content); got > 280 {
				t.Errorf("%s/%s: the %s message holds %d characters: %q", fx.Group, fx.Kind, e.Kind, got, m.Content)
			}
		}
	}
	if n < 40 {
		t.Errorf("%d messages measured, want every one the gallery holds", n)
	}
}
