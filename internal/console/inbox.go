package console

import (
	"cmp"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// inboxPage bounds the other rows one inbox read answers.
const inboxPage = 20

// inboxText is the inbox tool: the reader's own inbox when the project
// pokes them, else the inbox of the first human it pokes, whom the Waker
// wakes. It reads the same window chat_search reads, settled as the page
// settles it, and keeps the rows that hold the query. The first page
// answers every open ask, then the newest inboxPage other rows; a page
// below before answers only other rows, so no row shows twice.
func (t *turn) inboxText(in toolInput) (string, error) {
	var re *regexp.Regexp
	if in.Query != "" {
		var err error
		if re, err = searchQuery(in.Query); err != nil {
			return "", err
		}
	}
	logins := t.p.NotifyOrApprovers(t.tree.ApproversOf(t.p.Repository))
	if len(logins) == 0 {
		return "No inbox serves " + t.p.Name + ": its policy names no notify list and no approvers for the factory to poke.", nil
	}
	login := logins[0]
	if slices.Contains(logins, t.a.s.user) {
		login = t.a.s.user
	}
	ctx := t.r.Context()
	list, err := t.c.cfg.Chat.Conversations(ctx, t.p.Repository, login)
	if err != nil {
		return "", refuse(http.StatusInternalServerError, "cannot read the threads")
	}
	i := slices.IndexFunc(list, func(th factoryd.ChatConversation) bool { return th.Inbox })
	if i < 0 {
		return fmt.Sprintf("No inbox exists yet for %s on %s: nothing has poked it.", login, t.p.Name), nil
	}
	th := list[i]
	msgs, err := t.c.cfg.Chat.ChatMessages(ctx, t.p.Repository, login, th.Number, chatSearchRead)
	if err != nil {
		return "", refuse(http.StatusInternalServerError, "cannot read the inbox")
	}
	stop, err := t.c.chatStopNow(ctx, t.p)
	if err != nil {
		log.Printf("console: the chat stop of %s: %v", t.p.Repository, err)
	}
	t.settleRows(ctx, msgs, login, th.Number, th.SeenUpto, stop)
	paging := in.Before > 0
	var asks, rest []factoryd.ChatMessage
	for _, m := range slices.Backward(msgs) {
		if re != nil && !re.MatchString(m.Content) {
			continue
		}
		if factoryd.AskFamily(m) != "" && !m.Settled {
			if !paging {
				asks = append(asks, m)
			}
		} else if !paging || m.ID < in.Before {
			rest = append(rest, m)
		}
	}
	shown := rest[:min(len(rest), inboxPage)]
	page := append(asks, shown...)
	var b strings.Builder
	fmt.Fprintf(&b, "The inbox of %s on %s:\n", login, t.p.Name)
	if len(page) == 0 {
		b.WriteString("no row")
		if re != nil {
			fmt.Fprintf(&b, " holds %q", in.Query)
		}
		if paging {
			fmt.Fprintf(&b, " older than row %d", in.Before)
		}
		b.WriteString("\n")
	}
	issues := map[string]int64{}
	for _, m := range page {
		card := ""
		if m.TaskID != "" {
			n, ok := issues[m.TaskID]
			if !ok {
				n = t.c.issueOf(ctx, m.TaskID)
				issues[m.TaskID] = n
			}
			card = " " + issueRef(m.TaskID, n)
		}
		fmt.Fprintf(&b, "%d %s %s%s: %s%s\n", m.ID, m.At.UTC().Format("01-02 15:04Z"), inboxKind(m), card,
			textutil.Truncate(inboxSaid(m), excerptSide), inboxState(m))
	}
	if len(rest) > len(shown) {
		fmt.Fprintf(&b, "before=%d reads on.", shown[len(shown)-1].ID)
	} else {
		b.WriteString("That is the whole window.")
	}
	return b.String(), nil
}

// inboxKind is what wrote row m: the factory's kind of row, a wake
// turn's note, else its role.
func inboxKind(m factoryd.ChatMessage) string {
	switch {
	case m.Kind != "":
		return m.Kind
	case m.BirthKind != "":
		return "wake turn"
	}
	return m.Role
}

// inboxSaid is row m's words on one line; a round of tool calls without
// words names the tools it called.
func inboxSaid(m factoryd.ChatMessage) string {
	said := strings.Join(strings.Fields(m.Content), " ")
	if said != "" || len(m.ToolCalls) == 0 {
		return said
	}
	names := make([]string, len(m.ToolCalls))
	for i, c := range m.ToolCalls {
		names[i] = c.Name
	}
	return "called " + strings.Join(names, ", ")
}

// inboxState is the settle state of a row settleRows stamps, as the page
// shows it: open, or its reason line; other rows carry none.
func inboxState(m factoryd.ChatMessage) string {
	switch {
	case m.Kind != factoryd.MessagePoke && m.Kind != factoryd.MessageEscalation && m.Kind != factoryd.MessageAsk && !fundRow(m):
		return ""
	case !m.Settled:
		return " (open)"
	}
	return " (" + cmp.Or(m.Reason, "settled") + ")"
}
