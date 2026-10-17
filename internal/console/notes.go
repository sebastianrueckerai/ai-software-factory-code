package console

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

const (
	// notesMax bounds a project's scratchpad.
	notesMax = 16 << 10
	// notesTail is the scratchpad's tail active_threads shows.
	notesTail = 1 << 10
	// notesTries bounds a scratchpad write's retries when another write
	// came between its read and its write.
	notesTries = 3
	// stampLayout is a time as the notes tools write it.
	stampLayout = "2006-01-02 15:04Z"
)

// notes is the turn's project's scratchpad.
func (t *turn) notes() (factoryd.Notes, error) {
	if t.c.cfg.Notes == nil {
		return factoryd.Notes{}, refuse(http.StatusConflict, "this factory keeps no scratchpad")
	}
	n, err := t.c.cfg.Notes.Notes(t.r.Context(), t.p.Repository)
	if err != nil {
		return n, refuse(http.StatusInternalServerError, "cannot read the scratchpad")
	}
	return n, nil
}

// notesText is notes_read's answer.
func (t *turn) notesText() (string, error) {
	n, err := t.notes()
	if err != nil {
		return "", err
	}
	if n.Content == "" {
		return "The scratchpad of " + t.p.Name + " is empty; notes_write with text starts it.", nil
	}
	return fmt.Sprintf("The scratchpad of %s: %d of %d bytes, last written by %s at %s.\n\n%s", t.p.Name, len(n.Content), notesMax,
		n.By, n.At.UTC().Format(stampLayout), n.Content), nil
}

// writeNotes is notes_write: text appended as its own paragraph, or old
// replaced by new, on the scratchpad as it is at the write; a write that
// another came between reads it again and applies once more.
func (t *turn) writeNotes(text, old, new string) (string, error) {
	if (text == "") == (old == "") {
		return "", refuse(http.StatusBadRequest, "notes_write takes text to append, or old and new to replace a passage")
	}
	for range notesTries {
		n, err := t.notes()
		if err != nil {
			return "", err
		}
		next := n.Content
		switch {
		case text != "" && next == "":
			next = strings.TrimSpace(text)
		case text != "":
			next = strings.TrimRight(next, "\n") + "\n\n" + strings.TrimSpace(text)
		case strings.Count(next, old) != 1:
			return "", refuse(http.StatusUnprocessableEntity, fmt.Sprintf("the passage old is in the scratchpad %d times: read it with notes_read and copy one passage exactly", strings.Count(next, old)))
		default:
			next = strings.Replace(next, old, new, 1)
		}
		if len(next) > notesMax {
			return "", refuse(http.StatusRequestEntityTooLarge, fmt.Sprintf("the scratchpad would be %d bytes, over its cap of %d: remove what graduated into the roadmap first (old with new empty)", len(next), notesMax))
		}
		ok, err := t.c.cfg.Notes.PutNotes(t.r.Context(), t.p.Repository, t.a.s.user, next, n.Hash())
		if err != nil {
			return "", refuse(http.StatusInternalServerError, "cannot write the scratchpad")
		}
		if ok {
			return fmt.Sprintf("The scratchpad holds %d of %d bytes now.", len(next), notesMax), nil
		}
	}
	return "", refuse(http.StatusConflict, "the scratchpad changed under every try; read it with notes_read and write again")
}

// activeThreads is active_threads: the project's active threads, every
// member's with its owner named, each with its newest message, and the
// scratchpad's tail. The inbox threads stay out; they are the members'
// own faces to the factory.
func (t *turn) activeThreads() (string, error) {
	ctx := t.r.Context()
	list, err := t.c.cfg.Chat.ProjectConversations(ctx, t.p.Repository)
	if err != nil {
		return "", refuse(http.StatusInternalServerError, "cannot read the threads")
	}
	var b strings.Builder
	for _, th := range list {
		if th.Inbox || th.State != factoryd.ThreadActive || th.Messages == 0 {
			continue
		}
		if th.Login == t.a.s.user && th.Number == t.conv {
			continue
		}
		rows, err := t.c.cfg.Chat.ChatMessages(ctx, t.p.Repository, th.Login, th.Number, 40)
		if err != nil {
			return "", refuse(http.StatusInternalServerError, "cannot read the threads")
		}
		said := ""
		for _, m := range rows {
			if m.Role == "user" {
				said = m.Content
			}
		}
		fmt.Fprintf(&b, "- thread %d %q", th.Number, th.Title)
		if th.Login != t.a.s.user {
			fmt.Fprintf(&b, " (%s's)", th.Login)
		}
		fmt.Fprintf(&b, ", last turn %s", th.LastAt.UTC().Format(stampLayout))
		if t.c.turns.working(threadKey(t.p.Repository, th.Login, th.Number)) {
			b.WriteString(", a turn running")
		}
		if said != "" {
			fmt.Fprintf(&b, ": %s's newest message %q", th.Login, textutil.Truncate(strings.Join(strings.Fields(said), " "), 300))
		}
		b.WriteString("\n")
	}
	out := "No active thread on " + t.p.Name + " besides this one."
	if b.Len() > 0 {
		out = "The project's active threads on " + t.p.Name + ":\n" + b.String()
	}
	if t.c.cfg.Notes != nil {
		n, err := t.notes()
		if err != nil {
			return "", err
		}
		if n.Content != "" {
			tail := n.Content
			if len(tail) > notesTail {
				tail = "…" + strings.ToValidUTF8(tail[len(tail)-notesTail:], "")
			}
			out += fmt.Sprintf("\n\nThe scratchpad's tail (written by %s at %s):\n%s", n.By, n.At.UTC().Format(stampLayout), tail)
		}
	}
	return out, nil
}
