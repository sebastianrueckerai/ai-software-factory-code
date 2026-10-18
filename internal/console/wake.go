package console

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// StartWake runs one turn the factory's Waker starts (the concept's
// wake): the member's turn without their request, on conversation, its
// user row carrying birth as the birth kind. The turn queues behind one
// already running on the thread, and StartWake ends when the turn does.
// Without a live login of the member the turn still runs; its
// token-needing tools refuse.
func (c *Console) StartWake(ctx context.Context, project, login string, conversation int64, birth, text string) error {
	if c.cfg.Chat == nil || c.cfg.Operator == nil {
		return errors.New("the console has no chat")
	}
	p, ok, err := c.projectOf(ctx, project)
	if err != nil {
		return fmt.Errorf("the projects: %w", err)
	}
	if !ok {
		return fmt.Errorf("there is no project %s", project)
	}
	th, ok, err := c.cfg.Chat.Conversation(ctx, p.Repository, login, conversation)
	if err != nil {
		return fmt.Errorf("read conversation %d: %w", conversation, err)
	}
	if !ok {
		return fmt.Errorf("there is no conversation %d of %s on %s: %w", conversation, login, p.Repository, factoryd.ErrWakeVenueGone)
	}
	if th.State == factoryd.ThreadArchived {
		return fmt.Errorf("conversation %d is archived: %w", conversation, factoryd.ErrWakeVenueGone)
	}
	v, _ := c.inForce(ctx)
	t := &turn{c: c, p: p, conv: conversation, owner: login, tree: v.Tree, cfg: v.Document.ChatOf(v.Projects[p.Name]),
		a: actor{s: c.wakeSession(ctx, login), repository: p.Repository, via: factoryd.ViaChat, chatTask: factoryd.ChatTaskID(p.Name, time.Now())}}
	if pc, ok := v.Projects[p.Name]; ok {
		t.a.level, t.a.mergeRule = pc.AutonomyState(), pc.MergeRule
	}
	t.a.thread = conversation
	t.r = wakeRequest(ctx, t.a.s)
	task, err := c.cfg.Chat.EnsureChatTask(ctx, p.Name, p.Repository, t.cfg.MonthlyEUR, time.Now())
	if err != nil {
		return fmt.Errorf("the chat's task: %w", err)
	}
	pos, wait := c.turns.enter(t.key())
	defer c.turns.leave(t.key())
	id, err := c.cfg.Chat.AddChatMessage(ctx, p.Repository, login, factoryd.ChatMessage{Role: "user", Content: text,
		Conversation: conversation, Queued: pos > 0, BirthKind: birth})
	if err != nil {
		return fmt.Errorf("store the wake's message: %w", err)
	}
	t.detach()
	t.stream(func(string) error { return nil }, nil)
	t.takeTurn(pos, wait)
	if pos > 0 {
		if err := c.cfg.Chat.StartQueued(ctx, p.Repository, login, id); err != nil {
			return fmt.Errorf("start the queued turn: %w", err)
		}
	}
	release := c.turns.slot(p.Repository, t.parallelTurns())
	defer release()
	t.run(task.ID, "")
	return nil
}

// wakeSession is the member's newest login the wake runs on, a bare
// session of just their name when none reads back or opens: the turn
// runs, its token-needing tools refuse.
func (c *Console) wakeSession(ctx context.Context, login string) *session {
	rows, err := c.cfg.Sessions.ConsoleSessionsForUser(ctx, login)
	if err != nil {
		log.Printf("console: the sessions of %s: %v", login, err)
		return &session{user: login}
	}
	for _, row := range rows {
		if s, err := c.openSession(row); err == nil {
			return s
		}
	}
	return &session{user: login}
}

// wakeRequest is the turn's request without a member's browser behind it
// (the pulse's pattern): the session travels in the context, the form
// fields stay empty.
func wakeRequest(ctx context.Context, s *session) *http.Request {
	r, err := http.NewRequestWithContext(context.WithValue(ctx, ctxKey{}, s), http.MethodPost, "/projects/wake/chat", nil)
	if err != nil {
		panic(err)
	}
	return r
}
