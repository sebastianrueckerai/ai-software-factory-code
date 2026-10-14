package console

import (
	"cmp"
	"context"
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
// reload behave alike. Threads run in parallel, under the project's bound
// on model turns.

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

// key is the turn's thread's key in the gate.
func (t *turn) key() string { return threadKey(t.p.Repository, t.a.s.user, t.conv) }

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
// the member's current one when it names none: 400 for no number, 404
// for no thread of theirs. writable refuses an archived thread.
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
	th, ok, err := t.c.cfg.Chat.Conversation(ctx, t.p.Repository, t.a.s.user, n)
	switch {
	case err != nil:
		log.Printf("console: the chat of %s on %s: %v", t.a.s.user, t.p.Repository, err)
		return th, refuse(http.StatusInternalServerError, "cannot read the conversation")
	case !ok:
		// the current thread before its first message has no row yet
		th = factoryd.ChatConversation{Number: n, State: factoryd.ThreadActive}
		if r.FormValue("conversation") != "" || r.URL.Query().Get("conversation") != "" {
			if cur, _ := t.c.cfg.Chat.CurrentConversation(ctx, t.p.Repository, t.a.s.user); cur != n {
				return th, refuse(http.StatusNotFound, fmt.Sprintf("There is no conversation %d.", n))
			}
		}
	case writable && th.State == factoryd.ThreadArchived:
		return th, refuse(http.StatusConflict, fmt.Sprintf("Conversation %d is archived: restore it to write in it.", n))
	}
	t.conv, t.a.thread = n, n
	return th, nil
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
	switch ok, err := c.cfg.Chat.RenameConversation(r.Context(), t.p.Repository, t.a.s.user, t.conv, title); {
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
	if err := c.cfg.Chat.MarkSeen(r.Context(), t.p.Repository, t.a.s.user, t.conv); err != nil {
		log.Printf("console: the chat of %s on %s: %v", t.a.s.user, t.p.Repository, err)
		http.Error(w, "cannot mark the conversation read", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
