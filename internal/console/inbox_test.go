package console

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
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
// files a card for them, says so.
func TestWakeNotesFoldToOneLine(t *testing.T) {
	note := "Wake: 1 case waits for the factory agent: #2084.\n#2084:\n" + strings.Repeat("The criteria gate ran twice. ", 400)
	note = note[:11000]
	reply := strings.Repeat("Nothing here needs a human. ", 18)[:500]
	escalated := factoryd.ChatMessage{Role: "assistant", ToolCalls: []factoryd.ChatToolCall{{Name: "escalate", Result: "Filed the escalation as #2190 in Needs you."}}}
	for _, c := range []struct {
		name  string
		reply []factoryd.ChatMessage
		want  string
	}{
		{"a reply in words", []factoryd.ChatMessage{{Role: "assistant", Content: reply}},
			"Nothing for you. The agent looked at #2084 (Build the criteria gate)."},
		{"an escalation", []factoryd.ChatMessage{escalated, {Role: "assistant", Content: reply}},
			"The agent asks you on a card. It looked at #2084 (Build the criteria gate)."},
		{"a refused escalation", []factoryd.ChatMessage{{Role: "assistant", ToolCalls: []factoryd.ChatToolCall{{Name: "escalate", Status: 400, Result: "Refused: ask: 1 to 255 characters"}}}},
			"Nothing for you. The agent looked at #2084 (Build the criteria gate)."},
		{"an ask of its own", []factoryd.ChatMessage{{Role: "assistant", Kind: factoryd.MessageAsk, Content: "Raise the cap?", Choices: []string{"Yes", "No"}}},
			"The agent asks you below. It looked at #2084 (Build the criteria gate)."},
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
		msgs := chatRead(t, r, ck, chatPath+"?conversation="+strconv.FormatInt(inbox, 10)).Messages
		if len(msgs) != len(rows) {
			t.Fatalf("%s: %d rows, want %d", c.name, len(msgs), len(rows))
		}
		if msgs[0].Line != c.want || utf8.RuneCountInString(msgs[0].Line) > 160 || msgs[0].Content != note {
			t.Errorf("%s: the line = %q (%d characters), want %q over the whole note", c.name, msgs[0].Line, utf8.RuneCountInString(msgs[0].Line), c.want)
		}
		if faults := writingguide.Lint(msgs[0].Line); len(faults) > 0 {
			t.Errorf("%s: the line %q: %v", c.name, msgs[0].Line, faults)
		}
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
