package console

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The threads (roadmap 239): a member's many conversations per project,
// one turn at a time on each. A second message while a turn runs is
// stored at once as queued and answers queued with its place; when the
// turn ends the next starts, in the server, so one tab, two tabs and a
// reload behave alike. Every other writer of a thread's rows, an answer's
// and factoryd's pokes, takes the same line (Behind, roadmap 263), so no
// row lands inside a turn. Threads run in parallel, under the project's
// bound on model turns.

// turnGate serialises the turns of each thread and bounds the model turns
// of each project; what it holds is also the threads' working marks.
type turnGate struct {
	mu      sync.Mutex
	cond    *sync.Cond
	threads map[string]*threadLine
	running map[string]int
}

// threadLine is one thread's turn in flight and the turns waiting behind
// it, each woken by its channel's close.
type threadLine struct {
	waiting []chan struct{}
}

func newTurnGate() *turnGate {
	g := &turnGate{threads: map[string]*threadLine{}, running: map[string]int{}}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func threadKey(repository, login string, n int64) string {
	return repository + "\x00" + login + "\x00" + strconv.FormatInt(n, 10)
}

// enter takes the thread's turn: 0 and a closed channel when none runs,
// else its place behind the turn in flight and the channel its turn
// opens; every enter is followed by one leave.
func (g *turnGate) enter(key string) (int, <-chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	line, ok := g.threads[key]
	if !ok {
		g.threads[key] = &threadLine{}
		open := make(chan struct{})
		close(open)
		return 0, open
	}
	ch := make(chan struct{})
	line.waiting = append(line.waiting, ch)
	return len(line.waiting), ch
}

// leave ends the thread's turn: the next waiting one starts.
func (g *turnGate) leave(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	line := g.threads[key]
	if line == nil {
		return
	}
	if len(line.waiting) == 0 {
		delete(g.threads, key)
		return
	}
	next := line.waiting[0]
	line.waiting = line.waiting[1:]
	close(next)
}

// working reports whether a turn runs or waits on the thread.
func (g *turnGate) working(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.threads[key] != nil
}

// slot takes one of repository's limit model turns, waiting for one to
// free, and answers its release.
func (g *turnGate) slot(repository string, limit int) func() {
	g.mu.Lock()
	for g.running[repository] >= max(limit, 1) {
		g.cond.Wait()
	}
	g.running[repository]++
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		g.running[repository]--
		g.cond.Broadcast()
		g.mu.Unlock()
	}
}

// Behind takes a writer's place in thread n's line behind its turn in
// flight and those waiting before it (factoryd.Line): queued says one is
// ahead, so the writer stores its rows queued; land, called once with
// their ids, starts them in their order when the place comes and leaves
// it.
func (c *Console) Behind(repository, login string, n int64) (queued bool, land func(ids ...int64)) {
	key := threadKey(repository, login, n)
	pos, wait := c.turns.enter(key)
	if pos == 0 {
		return false, func(...int64) { c.turns.leave(key) }
	}
	return true, func(ids ...int64) {
		go func() {
			defer c.turns.leave(key)
			<-wait
			for _, id := range ids {
				if err := c.cfg.Chat.StartQueued(context.Background(), repository, login, id); err != nil {
					log.Printf("console: land queued row %d of %s's thread %d on %s: %v", id, login, n, repository, err)
				}
			}
		}()
	}
}

// key is the turn's thread's key in the gate.
func (t *turn) key() string { return threadKey(t.p.Repository, t.owner, t.conv) }

// takeTurn waits for the thread's turn: the stream says queued with its
// place while one runs. The returned leave ends it.
func (t *turn) takeTurn(pos int, wait <-chan struct{}) {
	if pos > 0 {
		t.send("queued", map[string]int{"position": pos})
	}
	<-wait
}

// detach lets the turn run to its end when the member leaves: the page
// may close or switch threads, the turn finishes and marks the thread.
func (t *turn) detach() {
	t.r = t.r.WithContext(context.WithoutCancel(t.r.Context()))
}

// thread resolves the thread the request names in field conversation,
// the member's current one when it names none, and in field owner the
// member whose thread it is: another member's thread, which everyone who
// writes the project may read and talk in, resolves by their name. 400
// for no number, 404 for no thread of theirs. writable refuses an
// archived thread.
func (t *turn) thread(r *http.Request, writable bool) (factoryd.ChatConversation, error) {
	ctx := r.Context()
	var n int64
	if s := cmp.Or(r.FormValue("conversation"), r.URL.Query().Get("conversation")); s != "" {
		var err error
		if n, err = strconv.ParseInt(s, 10, 64); err != nil || n < 1 {
			return factoryd.ChatConversation{}, refuse(http.StatusBadRequest, "conversation must be a positive integer")
		}
	} else {
		cur, err := t.c.cfg.Chat.CurrentConversation(ctx, t.p.Repository, t.a.s.user)
		if err != nil {
			log.Printf("console: the chat of %s on %s: %v", t.a.s.user, t.p.Repository, err)
			return factoryd.ChatConversation{}, refuse(http.StatusInternalServerError, "cannot read the conversation")
		}
		n = cur
	}
	owner := t.a.s.user
	if s := cmp.Or(r.FormValue("owner"), r.URL.Query().Get("owner")); s != "" {
		owner = s
	}
	th, ok, err := t.c.cfg.Chat.Conversation(ctx, t.p.Repository, owner, n)
	switch {
	case err != nil:
		log.Printf("console: the chat of %s on %s: %v", owner, t.p.Repository, err) // #nosec G706 -- the owner's stored login and the project's repository name, a log line alone
		return th, refuse(http.StatusInternalServerError, "cannot read the conversation")
	case !ok:
		if owner != t.a.s.user {
			return th, refuse(http.StatusNotFound, fmt.Sprintf("There is no conversation %d of %s's.", n, owner))
		}
		// the current thread before its first message has no row yet
		th = factoryd.ChatConversation{Number: n, Login: owner, State: factoryd.ThreadActive}
		if r.FormValue("conversation") != "" || r.URL.Query().Get("conversation") != "" {
			if cur, _ := t.c.cfg.Chat.CurrentConversation(ctx, t.p.Repository, t.a.s.user); cur != n {
				return th, refuse(http.StatusNotFound, fmt.Sprintf("There is no conversation %d.", n))
			}
		}
	case writable && th.State == factoryd.ThreadArchived:
		return th, refuse(http.StatusConflict, fmt.Sprintf("Conversation %d is archived: restore it to write in it.", n))
	}
	t.conv, t.a.thread, t.owner = n, n, owner
	return th, nil
}

// mine refuses another member's thread's controls: its rows are
// everyone's to read and talk in, its markers and its title, archive and
// delete its owner's.
func (t *turn) mine() error {
	if t.owner != t.a.s.user {
		return refuse(http.StatusForbidden, fmt.Sprintf("%s's thread: read it and talk in it; its controls stay %s's.", t.owner, t.owner))
	}
	return nil
}

// chatRename answers POST /projects/{name}/chat/rename: the thread's new
// title, 1 to 80 characters, then 204.
func (c *Console) chatRename(w http.ResponseWriter, r *http.Request) {
	t, ok := c.chatTurn(w, r)
	if !ok {
		return
	}
	title := strings.Join(strings.Fields(r.PostFormValue("title")), " ")
	if title == "" || utf8.RuneCountInString(title) > 80 {
		http.Error(w, "a title of 1 to 80 characters is required", http.StatusBadRequest)
		return
	}
	if _, err := t.thread(r, false); err != nil {
		c.fail(w, r, err)
		return
	}
	if err := t.mine(); err != nil {
		c.fail(w, r, err)
		return
	}
	switch ok, err := c.cfg.Chat.RenameConversation(r.Context(), t.p.Repository, t.owner, t.conv, title); {
	case err != nil:
		log.Printf("console: rename the chat of %s on %s: %v", t.a.s.user, t.p.Repository, err)
		http.Error(w, "cannot rename the conversation", http.StatusInternalServerError)
	case !ok:
		http.Error(w, "the conversation has no message yet: it takes its title from the first", http.StatusConflict)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// chatSeen answers POST /projects/{name}/chat/seen: the thread reads as
// read up to its newest row, then 204.
func (c *Console) chatSeen(w http.ResponseWriter, r *http.Request) {
	t, ok := c.chatTurn(w, r)
	if !ok {
		return
	}
	if _, err := t.thread(r, false); err != nil {
		c.fail(w, r, err)
		return
	}
	if err := t.mine(); err != nil {
		c.fail(w, r, err)
		return
	}
	if err := c.cfg.Chat.MarkSeen(r.Context(), t.p.Repository, t.owner, t.conv); err != nil {
		log.Printf("console: the chat of %s on %s: %v", t.owner, t.p.Repository, err)
		http.Error(w, "cannot mark the conversation read", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// chatArchive answers POST /projects/{name}/chat/archive: the thread
// archived, or restored with restore=1, then 204. The inbox stays, and a
// thread with a turn in flight waits for it to end.
func (c *Console) chatArchive(w http.ResponseWriter, r *http.Request) {
	t, ok := c.chatTurn(w, r)
	if !ok {
		return
	}
	th, err := t.thread(r, false)
	if err == nil {
		if err = t.mine(); err == nil {
			err = t.idle(th)
		}
	}
	if err != nil {
		c.fail(w, r, err)
		return
	}
	restore := r.PostFormValue("restore") == "1"
	done, err := c.cfg.Chat.ArchiveConversation(r.Context(), t.p.Repository, t.owner, t.conv, !restore)
	switch {
	case err != nil:
		log.Printf("console: archive the chat of %s on %s: %v", t.a.s.user, t.p.Repository, err)
		http.Error(w, "cannot archive the conversation", http.StatusInternalServerError)
	case !done:
		c.fail(w, r, refuse(http.StatusNotFound, "404 page not found"))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// chatDelete answers POST /projects/{name}/chat/delete: the member's
// thread and every row of it gone for good, one event naming the member,
// the thread and the rows' count, then 204. What the chat did stays in
// the events, the audit and git. The inbox stays, and a thread with a
// turn in flight waits for it to end.
func (c *Console) chatDelete(w http.ResponseWriter, r *http.Request) {
	t, ok := c.chatTurn(w, r)
	if !ok {
		return
	}
	th, err := t.thread(r, false)
	if err == nil {
		if err = t.mine(); err == nil {
			err = t.idle(th)
		}
	}
	if err != nil {
		c.fail(w, r, err)
		return
	}
	gone, err := c.cfg.Chat.DeleteConversation(r.Context(), t.p.Repository, t.owner, t.conv)
	switch {
	case errors.Is(err, factoryd.ErrNotFound):
		c.fail(w, r, refuse(http.StatusNotFound, "404 page not found"))
		return
	case err != nil:
		log.Printf("console: delete the chat of %s on %s: %v", t.a.s.user, t.p.Repository, err)
		http.Error(w, "cannot delete the conversation", http.StatusInternalServerError)
		return
	}
	c.event(context.WithoutCancel(r.Context()), factoryd.Event{Repository: t.p.Repository, Kind: factoryd.EventChatThreadDeleted,
		Actor: t.a.s.user, Via: factoryd.ViaButton, Detail: map[string]any{"thread": t.conv, "messages": gone}})
	log.Printf("console: %s deleted thread %d on %s (%d messages)", t.a.s.user, t.conv, t.p.Repository, gone)
	w.WriteHeader(http.StatusNoContent)
}

// idle refuses a thread with a turn in flight: its rows still land.
func (t *turn) idle(th factoryd.ChatConversation) error {
	if t.c.turns.working(t.key()) {
		return refuse(http.StatusConflict, fmt.Sprintf("A turn runs in conversation %d: wait for it to end.", th.Number))
	}
	return nil
}

// parallelTurns is the project's bound on model turns at once.
func (t *turn) parallelTurns() int {
	return cmp.Or(t.cfg.ParallelTurns, policy.DefaultParallelTurns)
}

// inThread binds a Do it's or an undo's turn to the thread that raised
// it, the member's current one for a row stored before threads.
func (t *turn) inThread(n int64) error {
	if n > 0 {
		t.conv, t.a.thread = n, n
		return nil
	}
	cur, err := t.c.cfg.Chat.CurrentConversation(t.r.Context(), t.p.Repository, t.a.s.user)
	if err != nil {
		log.Printf("console: the chat of %s on %s: %v", t.a.s.user, t.p.Repository, err)
		return refuse(http.StatusInternalServerError, "cannot read the conversation")
	}
	t.conv, t.a.thread = cur, cur
	return nil
}
