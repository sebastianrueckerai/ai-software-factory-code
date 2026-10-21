package console

import (
	"context"
	"log"
	"strconv"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The naming call (roadmap 304): a thread's first message names the
// thread once, through the cheap rung on the chat's account, while its
// title still is the head that message wrote. A member's rename in
// between, a card thread's "#task" and the inbox's title no longer match
// it, so they win and the model names no thread twice. A failed, refused
// or empty answer leaves today's head standing; nothing waits on the
// call.

// titleMax is a model title's longest, in characters: a few words, a
// name, not a sentence.
const titleMax = 60

// titleQuotes are the marks an answer may wear around its name.
const titleQuotes = "\"'`*“”„‚«»"

// nameThread writes the model's title for thread n of login on
// repository, whose first message's text is first, on chat task taskID's
// account.
func (c *Console) nameThread(ctx context.Context, repository, login string, n int64, taskID, first string) {
	if factoryd.ThreadTitle(first) == "" {
		return
	}
	answer, err := c.cfg.Operator.Title(ctx, taskID, repository+":"+login+":"+strconv.FormatInt(n, 10), first)
	if err != nil {
		log.Printf("console: the title of %s's thread %d on %s: %v", login, n, repository, err)
		return
	}
	if title := cleanTitle(answer, titleMax); title != "" {
		if _, err := c.cfg.Chat.ModelTitle(ctx, repository, login, n, title, factoryd.ThreadTitle(first)); err != nil {
			log.Printf("console: the title of %s's thread %d on %s: %v", login, n, repository, err)
		}
	}
}

// cleanTitle makes the model's answer a title: its first line's words on
// one line, the quote marks around it and a trailing sentence's dot off,
// cut at a word's end at max characters; "" for no answer left.
func cleanTitle(answer string, max int) string {
	s := strings.Join(strings.Fields(strings.Trim(strings.TrimSpace(strings.SplitN(answer, "\n", 2)[0]), titleQuotes)), " ")
	s = strings.TrimSuffix(s, ".")
	s = strings.Trim(s, titleQuotes)
	if runes := []rune(s); len(runes) > max {
		s = string(runes[:max])
		if i := strings.LastIndex(s, " "); i > max/2 {
			s = s[:i]
		}
		s = strings.TrimSpace(strings.Trim(s, titleQuotes))
	}
	return s
}
