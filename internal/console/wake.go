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
// user row carrying birth as the birth kind, its Forgejo calls the project
// agent's own (wakeSession). The turn queues behind one already running on
// the thread, and StartWake ends when the turn does.
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
	s, err := c.wakeSession(p, login)
	if err != nil {
		return err
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
		a: actor{s: s, repository: p.Repository, via: factoryd.ViaChat, wake: true, chatTask: factoryd.ChatTaskID(p.Name, time.Now())}}
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

// answerInChat runs the member's card answer, whose words matched none of
// the question's choices, as their chat turn. It is a variable set in
// init, because the chat's tool table reaches doAnswerValue, whose chat
// route reaches the turn, whose tool calls read the table: a direct call
// would close that circle at package init.
var answerInChat func(c *Console, ctx context.Context, a actor, t factoryd.Task, text string) (string, error)

func init() { answerInChat = (*Console).answerInChatTurn }

// answerInChatTurn stores the member's words as a user row on their
// current thread before it returns, runs the turn on without the request,
// and answers the note the answer route reads to land the member on the
// chat page, where the agent reads the card and answers with its own
// judgement.
func (c *Console) answerInChatTurn(ctx context.Context, a actor, t factoryd.Task, text string) (string, error) {
	p, ok, err := c.projectAt(ctx, t.Repository)
	switch {
	case err != nil:
		log.Printf("console: the projects: %v", err)
		return "", refuse(http.StatusInternalServerError, "cannot read the projects")
	case !ok:
		return "", refuse(http.StatusNotFound, "There is no project "+t.Repository+".")
	}
	conv, err := c.cfg.Chat.CurrentConversation(ctx, p.Repository, a.s.user)
	if err != nil {
		log.Printf("console: the chat of %s on %s: %v", a.s.user, p.Repository, err)
		return "", refuse(http.StatusInternalServerError, "cannot read the conversation")
	}
	v, _ := c.inForce(ctx)
	cfg := v.Document.ChatOf(v.Projects[p.Name])
	text, scrubbed := scrubTokens(text)
	notice := ""
	if scrubbed {
		notice = "I removed a GitHub token from your answer; it was not stored or sent."
	}
	tu := &turn{c: c, p: p, conv: conv, owner: a.s.user, tree: v.Tree, cfg: cfg,
		a: actor{s: a.s, thread: conv, repository: p.Repository, via: factoryd.ViaChat, chatTask: factoryd.ChatTaskID(p.Name, time.Now())}}
	if pc, ok := v.Projects[p.Name]; ok {
		tu.a.level, tu.a.mergeRule = pc.AutonomyState(), pc.MergeRule
	}
	tu.r = wakeRequest(context.WithoutCancel(ctx), tu.a.s)
	task, err := c.cfg.Chat.EnsureChatTask(ctx, p.Name, p.Repository, cfg.MonthlyEUR, time.Now())
	if err != nil {
		log.Printf("console: the chat task of %s: %v", p.Name, err)
		return "", refuse(http.StatusInternalServerError, "cannot file the chat's task")
	}
	pos, wait := c.turns.enter(tu.key())
	id, err := c.cfg.Chat.AddChatMessage(ctx, p.Repository, a.s.user, factoryd.ChatMessage{Role: "user",
		Content: fmt.Sprintf("About card #%d (%s): %s", t.IssueNumber, t.ID, text), Conversation: conv, Queued: pos > 0})
	if err != nil {
		c.turns.leave(tu.key())
		log.Printf("console: the chat of %s on %s: %v", a.s.user, p.Repository, err)
		return "", refuse(http.StatusInternalServerError, "cannot store the message")
	}
	go func() {
		defer c.turns.leave(tu.key())
		tu.detach()
		tu.stream(func(string) error { return nil }, nil)
		tu.takeTurn(pos, wait)
		if pos > 0 {
			if err := c.cfg.Chat.StartQueued(tu.r.Context(), p.Repository, a.s.user, id); err != nil {
				log.Printf("console: start the queued turn of %s on %s: %v", a.s.user, p.Repository, err)
				return
			}
		}
		release := c.turns.slot(p.Repository, tu.parallelTurns())
		defer release()
		tu.run(task.ID, notice)
	}()
	return answerInChatNote, nil
}

// wakeSession is the session a wake on p runs on: the woken member's
// name, so the turn's rows and events stay theirs, on p's chat token, so
// every Forgejo call goes out as factoryd.ChatUser whether or not the
// member is logged in. Without a chat token the wake is refused.
func (c *Console) wakeSession(p factoryd.Project, login string) (*session, error) {
	var token string
	if c.cfg.ChatToken != nil {
		token = c.cfg.ChatToken(p)
	}
	if token == "" {
		return nil, fmt.Errorf("project %s has no chat token, which a wake acts as %s with: a seed project names it in tokens.chat, "+
			"a factory project's registration mints it", p.Name, factoryd.ChatUser)
	}
	return &session{user: login, access: token, fixed: true}, nil
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
