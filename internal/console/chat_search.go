package console

import (
	"cmp"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// chat_search (the main-agent concept): the project's threads are one
// space, so a member asks for a text and gets every thread's rows that
// hold it, the owner's name on each hit. The scan walks the listing's
// order, each conversation's live rows read up to chatSearchRead, and
// stops at searchHits hits with the rest counted; a thread argument
// narrows it to one of the member's own threads.

// chatSearchRead bounds the rows the search reads of one thread, so a
// huge thread cannot hold the turn.
const chatSearchRead = 500

// chatSearch is the chat_search tool.
func (t *turn) chatSearch(in toolInput) (string, error) {
	if n := utf8.RuneCountInString(in.Query); n < 2 || n > 200 {
		return "", refuse(http.StatusBadRequest, "The query is 2 to 200 characters.")
	}
	ctx := t.r.Context()
	list, err := t.c.cfg.Chat.ProjectConversations(ctx, t.p.Repository)
	if err != nil {
		return "", refuse(http.StatusInternalServerError, "cannot read the threads")
	}
	if in.Thread > 0 {
		var kept []factoryd.ChatConversation
		for _, th := range list {
			if th.Login == t.a.s.user && th.Number == in.Thread {
				kept = append(kept, th)
			}
		}
		if len(kept) == 0 {
			return "", refuse(http.StatusNotFound, fmt.Sprintf("There is no conversation %d.", in.Thread))
		}
		list = kept
	}
	re := regexp.MustCompile("(?i)" + regexp.QuoteMeta(in.Query))
	var hits []string
	total := 0
	for _, th := range list {
		rows, err := t.c.cfg.Chat.ChatMessages(ctx, t.p.Repository, th.Login, th.Number, chatSearchRead)
		if err != nil {
			return "", refuse(http.StatusInternalServerError, "cannot read the threads")
		}
		for _, m := range rows {
			for _, line := range strings.Split(m.Content, "\n") {
				at := re.FindStringIndex(line)
				if at == nil {
					continue
				}
				total++
				if len(hits) < searchHits {
					hits = append(hits, fmt.Sprintf("thread %d (%s's, %s): row %d: %s: %s",
						th.Number, th.Login, cmp.Or(th.Title, "untitled"), m.ID, m.Role,
						textutil.Truncate(line[at[0]:], excerptSide)))
				}
			}
		}
	}
	answer := fmt.Sprintf("no hit for %q", in.Query)
	if len(hits) > 0 {
		answer = strings.Join(hits, "\n")
	}
	if total > len(hits) {
		answer += fmt.Sprintf("\n... and %d more", total-len(hits))
	}
	return answer, nil
}
