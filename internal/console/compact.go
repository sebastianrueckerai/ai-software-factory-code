package console

import (
	"cmp"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// Compaction (roadmap 239): the raw rows stay, the thread carries a
// running summary of its rows before its kept tail. A turn's context is
// the status, the summary and the rows after it, so the ceiling holds by
// construction. Three triggers run the one step: the preset, before a
// turn projected past 80 percent of the window; the forced, on the
// provider's refusal of a prompt past its window, at most twice with the
// second keeping half the tail; and /compact by hand.

const (
	// keepTurns is the kept tail: the thread's last two turns (C14).
	keepTurns = 2
	// chatContextMax bounds the rows after the summary a context holds;
	// a thread past it compacts first.
	chatContextMax = 200
	// compactRead bounds the rows one compaction reads.
	compactRead = 2000
	// compactChunk bounds the text of one summarising call, in
	// characters: a source past it is folded chunk by chunk.
	compactChunk = 48 << 10
	// compactSlot bounds one tool result as the summariser reads it.
	compactSlot = 600
	// chatCompactCommand is the hand trigger.
	chatCompactCommand = "/compact"
)

// threadRef names the turn's thread on the audit rows of its calls.
func (t *turn) threadRef() string {
	return t.p.Repository + ":" + t.owner + ":" + strconv.FormatInt(t.conv, 10)
}

// window is the context window of the alias the chat calls.
func (t *turn) window() int { return cmp.Or(t.cfg.Window, policy.DefaultChatWindow) }

// estimate is text's tokens as a compaction counts them: four characters
// a token.
func estimate(text string) int { return (len(text) + 3) / 4 }

// weight is the estimated tokens of summary and rows as the turn's
// context carries them (replay): this turn's reads whole, an earlier
// turn's as its line.
func (t *turn) weight(summary string, rows []factoryd.ChatMessage) int {
	var b strings.Builder
	b.WriteString(summary)
	for _, m := range t.replay(rows) {
		b.WriteString(m.Content)
		for _, c := range m.ToolCalls {
			b.WriteString(c.Name + c.Arguments)
		}
	}
	return estimate(b.String())
}

// liveRows are thread th's rows after its summary, the queued left out.
func liveRows(th factoryd.ChatConversation, rows []factoryd.ChatMessage) []factoryd.ChatMessage {
	var out []factoryd.ChatMessage
	for _, m := range rows {
		if m.ID > th.SummaryUpto && !m.Queued {
			out = append(out, m)
		}
	}
	return out
}

// tailStart is where rows' last keep turns start, a turn starting at a
// member's message; 0 when rows hold no more turns than that.
func tailStart(rows []factoryd.ChatMessage, keep int) int {
	seen := 0
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Role == "user" {
			seen++
			if seen == max(keep, 1) {
				return i
			}
		}
	}
	return 0
}

// transcript is rows as the summariser reads them: who said what, each
// tool call with its result cut short, a read's left out, so the summary
// carries no old read as the project now.
func transcript(rows []factoryd.ChatMessage) string {
	var b strings.Builder
	for _, m := range rows {
		who := "the member"
		switch {
		case m.Kind != "":
			who = "the factory"
		case m.Role == "assistant":
			who = "the agent"
		}
		if m.Content != "" {
			fmt.Fprintf(&b, "%s: %s\n", who, m.Content)
		}
		for _, c := range m.ToolCalls {
			if isRead(c.Name) {
				fmt.Fprintf(&b, "the agent read %s %s\n", c.Name, c.Arguments)
				continue
			}
			fmt.Fprintf(&b, "the agent called %s %s: %s\n", c.Name, c.Arguments, textutil.Truncate(strings.Join(strings.Fields(c.Result), " "), compactSlot))
		}
	}
	return b.String()
}

// chunks are text cut at line ends into pieces of at most n characters;
// a longer line is a piece of its own.
func chunks(text string, n int) []string {
	var out []string
	var b strings.Builder
	for l := range strings.Lines(text) {
		if b.Len() > 0 && b.Len()+len(l) > n {
			out = append(out, b.String())
			b.Reset()
		}
		b.WriteString(l)
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

const compactPrompt = `You keep the running summary of a member's thread with the project agent of a software factory.
Write the new summary from the summary so far and the turns after it: what the member asked for and decided, what the agent did (cards filed, moved, answered, steered, approved, with their issue numbers and task ids), what is open, and the facts a later turn needs.
Keep issue numbers (#12), task ids (t-...), names, numbers and quoted decisions exactly. Leave out greetings, tool output nobody needs, and anything the turns settled and left behind.
Answer with the summary alone, as short paragraphs, at most 400 words.`

// compact folds thread n's rows before its last keep turns, with its
// summary so far, into a new summary on chat task taskID's account: one
// cheap call, chunk by chunk past the summariser's window. False when the
// thread holds nothing before its tail.
func (t *turn) compact(taskID string, keep int) (bool, error) {
	ctx := t.r.Context()
	th, _, err := t.c.cfg.Chat.Conversation(ctx, t.p.Repository, t.owner, t.conv)
	if err != nil {
		return false, err
	}
	rows, err := t.c.cfg.Chat.ChatMessages(ctx, t.p.Repository, t.owner, t.conv, compactRead)
	if err != nil {
		return false, err
	}
	rows = liveRows(th, rows)
	start := tailStart(rows, keep)
	if start == 0 {
		return false, nil
	}
	summary := th.Summary
	for _, chunk := range chunks(transcript(rows[:start]), compactChunk) {
		so := cmp.Or(summary, "(none yet)")
		msgs := []factoryd.OperatorMessage{{Role: "system", Content: compactPrompt},
			{Role: "user", Content: "The summary so far:\n" + so + "\n\nThe turns after it:\n" + chunk}}
		if summary, err = t.c.cfg.Operator.Compact(ctx, taskID, t.threadRef(), msgs); err != nil {
			return false, err
		}
		if summary = strings.TrimSpace(summary); summary == "" {
			return false, fmt.Errorf("the summariser answered nothing")
		}
	}
	before := t.weight(th.Summary, rows)
	after := t.weight(summary, rows[start:])
	if err := t.c.cfg.Chat.SaveSummary(ctx, t.p.Repository, t.owner, t.conv,
		factoryd.ThreadSummary{Text: summary, Model: policy.Small, Upto: rows[start-1].ID, Before: before, After: after}); err != nil {
		return false, err
	}
	t.send("compacted", map[string]int{"messages": start, "before": before, "after": after})
	return true, nil
}

// presetCompact compacts the thread before its turn when the turn is
// projected past 80 percent of the window, or holds more rows after its
// summary than a context may: the projection is the thread's newest call
// since its last compaction, its prompt and its answer as the ledger
// booked them, plus the new message; a thread with no call since reads
// its summary and rows. A failed compaction is logged; the forced
// trigger still holds the bound.
func (t *turn) presetCompact(taskID string) {
	ctx := t.r.Context()
	th, _, err := t.c.cfg.Chat.Conversation(ctx, t.p.Repository, t.owner, t.conv)
	if err != nil {
		log.Printf("console: the chat of %s on %s: %v", t.owner, t.p.Repository, err)
		return
	}
	rows, err := t.c.cfg.Chat.ChatMessages(ctx, t.p.Repository, t.owner, t.conv, compactRead)
	if err != nil {
		log.Printf("console: the chat of %s on %s: %v", t.owner, t.p.Repository, err)
		return
	}
	rows = liveRows(th, rows)
	projected, err := t.c.cfg.Chat.ThreadTokens(ctx, t.threadRef(), th.SummaryAt)
	if err != nil {
		log.Printf("console: the tokens of %s: %v", t.threadRef(), err)
	}
	if len(rows) > 0 {
		newest := rows[len(rows)-1]
		if projected > 0 {
			projected += estimate(newest.Content)
		} else {
			projected = t.weight(th.Summary, rows)
		}
	}
	if projected*10 <= t.window()*8 && len(rows) <= chatContextMax {
		return
	}
	if _, err := t.compact(taskID, keepTurns); err != nil {
		log.Printf("console: compact %s before its turn: %v", t.threadRef(), err)
	}
}

// compactByHand is /compact: the thread's turn compacts it and answers
// what it folded.
func (t *turn) compactByHand(taskID string) {
	done, err := false, t.begin()
	if err == nil {
		done, err = t.compact(taskID, keepTurns)
	}
	switch {
	case err != nil:
		log.Printf("console: /compact %s: %v", t.threadRef(), err)
		t.fail("The thread could not be compacted: " + t.sentence(err))
		return
	case !done:
		t.send("text", map[string]string{"text": "Nothing to compact: the thread holds no turns before its last two."})
	}
	t.send("done", struct{}{})
}
