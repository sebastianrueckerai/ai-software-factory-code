package console

import (
	"cmp"
	"context"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// inboxRow is a row of a thread as the inbox renders it: the row, the
// cards a note to the agent names, and the agent's rows that answer it.
type inboxRow struct {
	m     factoryd.ChatMessage
	cards []factoryd.Task
	reply []factoryd.ChatMessage
}

// inboxView is what the panel shows of a row in place of its words: the
// one quiet line of a note to the agent with its reply, or the opening of
// a long message above its fold; "" for the row as written.
type inboxView struct{ Line, Short string }

// inboxKind is the inbox's rule for the rows of one birth kind: a row for
// the human shows its words, a note to the agent folds with its reply.
type inboxKind struct {
	ForHuman bool
	Render   func(inboxRow) inboxView
}

// inboxKinds hold a rule for every birth kind the factory writes
// (factoryd.BirthKinds).
var inboxKinds = func() map[string]inboxKind {
	k := map[string]inboxKind{
		factoryd.WakeAsk:          {Render: lookedAt},
		factoryd.EventCaseClimbed: {Render: lookedAt},
		factoryd.WakeCard:         {Render: lookedAt},
		factoryd.WakeTimer:        {Render: lookedAt},
		factoryd.WakeRefill: {Render: func(r inboxRow) inboxView {
			return inboxView{Line: agentDid(r.reply, "read the roadmap for new cards")}
		}},
	}
	for _, birth := range []string{factoryd.EventQuestionAsked, factoryd.EventQuestionWithdrawn, factoryd.EventApprovalRequested,
		factoryd.EventTaskEnded, factoryd.EventAllChildrenDone, factoryd.EventAutonomyChanged, factoryd.EventCardFiled,
		factoryd.EventCardRetried, factoryd.EventSessionGranted, factoryd.EventChatBudgetSpent, factoryd.EventPullRequestMerged} {
		k[birth] = inboxKind{ForHuman: true, Render: func(r inboxRow) inboxView { return inboxView{Short: opening(r.m.Content, inboxMax)} }}
	}
	// a routine's report is a notice: its first line, the list below a fold
	k[factoryd.EventRoutineRan] = inboxKind{ForHuman: true, Render: func(r inboxRow) inboxView {
		if first, rest, ok := strings.Cut(r.m.Content, "\n"); ok && strings.TrimSpace(rest) != "" {
			return inboxView{Short: cmp.Or(opening(first, lineMax), strings.TrimSpace(first))}
		}
		return inboxView{Short: opening(r.m.Content, lineMax)}
	}}
	return k
}()

const (
	// inboxMax bounds a message above its fold, lineMax the one line of a
	// note to the agent, in runes.
	inboxMax = 280
	lineMax  = 160
	// agentFold is the length past which the agent's own message folds.
	agentFold = 600
)

// lookedAt is the line of a wake on the cards it names (agentDid).
func lookedAt(r inboxRow) inboxView {
	var names []string
	for _, t := range r.cards {
		names = append(names, cardWords(t))
	}
	what := "the factory's news"
	switch n := len(names); {
	case n == 1:
		what = names[0]
	case n == 2:
		what = names[0] + " and " + names[1]
	case n > 2:
		what = names[0] + " and " + strconv.Itoa(n-1) + " more cards"
	}
	line := agentDid(r.reply, "looked at "+what)
	if utf8.RuneCountInString(line) > lineMax && len(r.cards) > 0 {
		line = agentDid(r.reply, "looked at "+strconv.Itoa(len(r.cards))+" cards")
	}
	return inboxView{Line: line}
}

// cardWords name card t for a line: its issue, which links it, and its
// title.
func cardWords(t factoryd.Task) string {
	title := factoryd.CardTitle(t)
	switch {
	case t.IssueNumber == 0:
		return "“" + title + "”"
	case title == "":
		return "#" + strconv.FormatInt(t.IssueNumber, 10)
	}
	return "#" + strconv.FormatInt(t.IssueNumber, 10) + " (" + title + ")"
}

// agentDid is a wake's line: what the agent's reply means for the human
// first, as the chat's form leads, then what the agent did.
func agentDid(reply []factoryd.ChatMessage, did string) string {
	lead, who := outcome(reply), "The agent"
	if strings.HasPrefix(lead, who) {
		who = "It"
	}
	return lead + " " + who + " " + did + "."
}

// outcome is what the agent's reply means for the human: an ask of its
// own comes below, a card it filed or handed to the human comes as its own
// message; else nothing is for them.
func outcome(reply []factoryd.ChatMessage) string {
	for _, m := range reply {
		if m.Kind == factoryd.MessageAsk {
			return "The agent asks you below."
		}
		for _, c := range m.ToolCalls {
			if c.Status == 0 && (c.Name == "escalate" || c.Name == "attach_analysis" && strings.HasPrefix(c.Result, handedUp)) {
				return "The agent asks you on a card."
			}
		}
	}
	return "Nothing for you."
}

// opening is the start of text a fold shows, at most max runes cut at a
// space, "" for a text that fits.
func opening(text string, max int) string {
	if utf8.RuneCountInString(text) <= max {
		return ""
	}
	cut := []rune(text)[:max]
	if i := strings.LastIndexAny(string(cut), " \n"); i > 0 {
		return strings.TrimRight(string(cut)[:i], " \n")
	}
	return string(cut)
}

// askedForReport says the human's words ask for a report, which their
// answer gives whole.
var askedForReport = regexp.MustCompile(`(?i)\breports?\b`)

// inboxViews sets on msgs, a thread oldest first, what the panel shows of
// each row: a row not for the human folds with the agent's reply to it
// into one line, a factory row past inboxMax and an answer of the agent
// past agentFold show their opening above a fold, unless the human's last
// words asked for a report.
func (t *turn) inboxViews(ctx context.Context, msgs []factoryd.ChatMessage) {
	cards := map[string]factoryd.Task{}
	card := func(ref string) (factoryd.Task, bool) {
		if c, ok := cards[ref]; ok {
			return c, c.ID != ""
		}
		var c factoryd.Task
		var err error
		if n, isIssue := strings.CutPrefix(ref, "#"); isIssue {
			issue, _ := strconv.ParseInt(n, 10, 64)
			c, _, err = t.c.cfg.Tasks.TaskByIssue(ctx, t.p.Repository, issue)
		} else {
			c, _, err = t.c.cfg.Tasks.Task(ctx, ref)
		}
		if err != nil {
			c = factoryd.Task{}
		}
		cards[ref] = c
		return c, c.ID != ""
	}
	report := false
	for i, m := range msgs {
		if humanSaid(m) {
			report = askedForReport.MatchString(m.Content)
			continue
		}
		rule, ok := inboxKinds[m.BirthKind]
		switch {
		case ok && !rule.ForHuman:
			r := inboxRow{m: m}
			for _, ref := range factoryd.WakeCards(m.Content) {
				if c, ok := card(ref); ok {
					r.cards = append(r.cards, c)
				}
			}
			for _, n := range msgs[i+1:] {
				if n.Role == "user" || factoryRow(n) {
					break
				}
				r.reply = append(r.reply, n)
			}
			msgs[i].Line = rule.Render(r).Line
		case ok:
			msgs[i].Short = rule.Render(inboxRow{m: m}).Short
		case m.Role == "assistant" && m.Kind == "" && !report && utf8.RuneCountInString(m.Content) > agentFold:
			msgs[i].Short = opening(m.Content, inboxMax)
		}
	}
}

// factoryRow says m is a row the factory wrote itself, which the panel
// renders as its own whatever its role.
func factoryRow(m factoryd.ChatMessage) bool {
	switch m.Kind {
	case factoryd.MessagePoke, factoryd.MessageEscalation, factoryd.MessageBudget, factoryd.MessageStop:
		return true
	}
	return false
}
