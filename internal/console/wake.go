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
// agent's own (wakeTurn). A chat its budget stopped refuses the wake
// before any row, factoryd.ErrChatUnfunded. The turn queues behind one
// already running on the thread, and StartWake ends when the turn does;
// a turn the budget stops on its way is delivered, its thread ending on
// the stop row a fund resumes.
func (c *Console) StartWake(ctx context.Context, project, login string, conversation int64, birth, text string, cases ...int64) error {
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
	t, err := c.wakeTurn(ctx, p, login, conversation)
	if err != nil {
		return err
	}
	t.a.birth = birth
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
	task, err := c.cfg.Chat.EnsureChatTask(ctx, p.Name, p.Repository, t.cfg.MonthlyEUR, time.Now())
	if err != nil {
		return fmt.Errorf("the chat's task: %w", err)
	}
	if stop, err := c.chatStopOf(ctx, task); err != nil {
		return fmt.Errorf("the chat's stop: %w", err)
	} else if stop.stopped {
		return fmt.Errorf("the chat of %s stands at its budget of €%.2f: %w", p.Name, task.BudgetEUR, factoryd.ErrChatUnfunded)
	}
	pos, wait := c.turns.enter(t.key())
	defer c.turns.leave(t.key())
	id, err := c.cfg.Chat.AddChatMessage(ctx, p.Repository, login, factoryd.ChatMessage{Role: "user", Content: text,
		Conversation: conversation, Queued: pos > 0, BirthKind: birth})
	if err != nil {
		return fmt.Errorf("store the wake's message: %w", err)
	}
	t.detach()
	t.quiet()
	t.takeTurn(pos, wait)
	if pos > 0 {
		landed, err := c.cfg.Chat.StartQueued(ctx, p.Repository, login, id)
		if err != nil {
			return fmt.Errorf("start the queued turn: %w", err)
		}
		if landed == 0 {
			return nil // a turn of the thread already read the wake's note
		}
	}
	release := c.turns.slot(context.Background(), p.Repository, t.parallelTurns())
	defer release()
	_, before, err := c.cfg.Chat.ThreadTotals(ctx, t.threadRef())
	if err != nil {
		log.Printf("console: the totals of %s: %v", t.threadRef(), err)
	}
	t.run(task.ID, "")
	if err == nil {
		t.bookCost(ctx, before, cases)
	}
	return nil
}

// bookCost writes the thread's own spend over the turn, from its audit
// totals, on the rung 2 step of each case the wake named, shared evenly: the
// chat task's total is no measure, other turns and rung 1 book on it too.
func (t *turn) bookCost(ctx context.Context, before float64, cases []int64) {
	if t.c.cfg.AddStepCost == nil || len(cases) == 0 {
		return
	}
	_, after, err := t.c.cfg.Chat.ThreadTotals(ctx, t.threadRef())
	if err != nil {
		log.Printf("console: the totals of %s: %v", t.threadRef(), err)
		return
	}
	spent := after - before
	if spent <= 0 {
		return
	}
	for _, id := range cases {
		if err := t.c.cfg.AddStepCost(ctx, id, 2, spent/float64(len(cases))); err != nil {
			log.Printf("console: the cost of rung 2's step on case %d: %v", id, err)
		}
	}
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
	tu := c.sessionTurn(context.WithoutCancel(ctx), p, a.s, conv)
	text, github, chat := scrubTokens(text)
	notice := ""
	if github || chat {
		notice = "I removed " + removedTokens(github, chat) + " from your answer; it was not stored or sent."
	}
	task, err := c.cfg.Chat.EnsureChatTask(ctx, p.Name, p.Repository, tu.cfg.MonthlyEUR, time.Now())
	if err != nil {
		log.Printf("console: the chat task of %s: %v", p.Name, err)
		return "", refuse(http.StatusInternalServerError, "cannot file the chat's task")
	}
	pos, wait := c.turns.enter(tu.key())
	id, err := c.cfg.Chat.AddChatMessage(ctx, p.Repository, a.s.user, factoryd.ChatMessage{Role: "user",
		Content: fmt.Sprintf("About card #%d (%s): %s", t.IssueNumber, t.ID, text), Conversation: conv, Queued: pos > 0})
	if err != nil {
		// its channel leaves the line waking no one; a first message's
		// turn owns the line and ends it
		if pos > 0 {
			c.turns.unline(tu.key(), wait)
		} else {
			c.turns.leave(tu.key())
		}
		log.Printf("console: the chat of %s on %s: %v", a.s.user, p.Repository, err)
		return "", refuse(http.StatusInternalServerError, "cannot store the message")
	}
	go func() {
		defer c.turns.leave(tu.key())
		tu.detach()
		tu.quiet()
		tu.takeTurn(pos, wait)
		if pos > 0 {
			landed, err := c.cfg.Chat.StartQueued(tu.r.Context(), p.Repository, a.s.user, id)
			if err != nil {
				log.Printf("console: start the queued turn of %s on %s: %v", a.s.user, p.Repository, err)
				return
			}
			if landed == 0 {
				return // a turn of the thread already read the answer
			}
		}
		release := c.turns.slot(tu.r.Context(), p.Repository, tu.parallelTurns())
		defer release()
		tu.run(task.ID, notice)
	}()
	return answerInChatNote, nil
}

// wakeTurn is a turn on login's thread conversation of project p without
// the member's request, a wake's or a fund's resume: its rows the
// member's, its Forgejo calls the project agent's own on p's chat token
// (wakeSession), the member never having asked.
func (c *Console) wakeTurn(ctx context.Context, p factoryd.Project, login string, conversation int64) (*turn, error) {
	s, err := c.wakeSession(p, login)
	if err != nil {
		return nil, err
	}
	t := c.sessionTurn(ctx, p, s, conversation)
	t.a.wake = true
	return t, nil
}

// sessionTurn is s's turn on their thread conversation of project p with
// no browser behind it: a wake's, a card answer's or a chat token's.
func (c *Console) sessionTurn(ctx context.Context, p factoryd.Project, s *session, conversation int64) *turn {
	v, _ := c.inForce(ctx)
	t := &turn{c: c, p: p, conv: conversation, owner: s.user, tree: v.Tree, cfg: v.Document.ChatOf(v.Projects[p.Name]),
		a: actor{s: s, repository: p.Repository, via: factoryd.ViaChat, chatTask: factoryd.ChatTaskID(p.Name, time.Now()), thread: conversation}}
	if pc, ok := v.Projects[p.Name]; ok {
		t.a.level, t.a.mergeRule = pc.AutonomyState(), pc.MergeRule
	}
	t.r = sessionRequest(ctx, s)
	t.a.reads = t.readList
	return t
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

// sessionRequest is the turn's request without a browser behind it (the
// pulse's pattern): the session travels in the context, the form fields
// stay empty.
func sessionRequest(ctx context.Context, s *session) *http.Request {
	r, err := http.NewRequestWithContext(context.WithValue(ctx, ctxKey{}, s), http.MethodPost, "/projects/wake/chat", nil)
	if err != nil {
		panic(err)
	}
	return r
}
