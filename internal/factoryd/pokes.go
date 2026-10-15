package factoryd

import (
	"context"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// Pokes (roadmap 239): factoryd's own turns in the inbox thread of each
// member a project tells, its notify list else its approvers, on 145's
// event classes: a card that needs a human (and one that needs them no
// more, its question withdrawn), a card stopped by its budget, a terminal
// failure, a card or plan done, the agent's own starts and filings at
// assisted and autonomous, and a change of the level. A poke
// is a row written through the message path: no model turn and no queue,
// one per state change. A repeat of the newest poke about the same card
// counts on that row (74's rule).

const (
	// MessagePoke is a poke's kind; MessageEscalation a poke about a card
	// that waits for a person, which the chat marks "needs you".
	MessagePoke       = "poke"
	MessageEscalation = "escalation"
)

// pokeKinds are the events a poke may answer.
var pokeKinds = []string{EventQuestionAsked, EventQuestionWithdrawn, EventApprovalRequested, EventTaskEnded, EventAllChildrenDone, EventAutonomyChanged,
	EventCardFiled, EventCardMoved, EventPlanStarted, EventPlanAccepted, EventCardRetried, EventSessionGranted}

// budgetSources are the escalation sources of a card stopped by money.
var budgetSources = []string{"budget", "cap", SourceMonthCap, "project_cap", "day_cap", "split_cap"}

// agentVerbs are the agent's own actions a poke tells, by event kind.
var agentVerbs = map[string]string{EventCardFiled: "filed", EventCardMoved: "moved", EventPlanStarted: "started",
	EventPlanAccepted: "accepted", EventCardRetried: "retried", EventSessionGranted: "granted"}

// PokeStore is what the poke writer reads and writes.
type PokeStore interface {
	Task(ctx context.Context, id string) (Task, bool, error)
	// PokeEvents are the poke events after the poke cursor, oldest first,
	// at most limit.
	PokeEvents(ctx context.Context, limit int) ([]Event, error)
	// Poke writes m into the inbox thread of each of logins on repository
	// and moves the cursor past event id, in one transaction.
	Poke(ctx context.Context, id int64, repository string, logins []string, m ChatMessage) error
}

// Poker writes the pokes.
type Poker struct {
	Store   PokeStore
	Current func() policy.Version
}

// pokeBatch bounds the events one read takes.
const pokeBatch = 100

// Loop pokes every interval until ctx ends.
func (p *Poker) Loop(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := p.Pass(ctx); err != nil {
				log.Printf("pokes: %v", err)
			}
		}
	}
}

// Pass pokes for every poke event after the cursor, oldest first; an
// event that pokes nobody only moves the cursor.
func (p *Poker) Pass(ctx context.Context) error {
	for {
		events, err := p.Store.PokeEvents(ctx, pokeBatch)
		if err != nil {
			return err
		}
		tree := p.Current().Tree
		for _, e := range events {
			var t Task
			if e.TaskID != "" {
				if t, _, err = p.Store.Task(ctx, e.TaskID); err != nil {
					return err
				}
			}
			m, ok := pokeOf(e, t)
			var to []string
			if ok {
				to = tree.ApproversOf(e.Repository)
				if _, c, found := tree.ProjectOf(e.Repository); found && len(c.Notify) > 0 {
					to = c.Notify
				}
			}
			if err := p.Store.Poke(ctx, e.ID, e.Repository, to, m); err != nil {
				return err
			}
		}
		if len(events) < pokeBatch {
			return nil
		}
	}
}

// pokeOf is event e's poke, t the card it names, false for an event that
// pokes nobody.
func pokeOf(e Event, t Task) (ChatMessage, bool) {
	ref := cardRef(e, t)
	str := func(k string) string { s, _ := e.Detail[k].(string); return s }
	num := func(k string) int64 { n, _ := e.Detail[k].(float64); return int64(n) }
	m := ChatMessage{Role: "assistant", Kind: MessagePoke, TaskID: e.TaskID}
	switch e.Kind {
	case EventQuestionAsked:
		m.Kind = MessageEscalation
		m.Content = ref + " needs you: " + str("question")
		if slices.Contains(budgetSources, str("source")) {
			m.Content = ref + " is stopped by its budget: " + str("question")
		}
		if cs, ok := e.Detail["choices"].([]any); ok {
			for _, c := range cs {
				if s, ok := c.(string); ok {
					m.Choices = append(m.Choices, s)
				}
			}
		}
	case EventQuestionWithdrawn:
		// a row after the card's escalation: the inbox offers its answer
		// row no more
		m.Content = ref + " needs you no more: its question is withdrawn because " + str("reason") + "."
	case EventApprovalRequested:
		m.Kind = MessageEscalation
		switch str("kind") {
		case ApprovalMerge:
			m.Content = fmt.Sprintf("Merge PR #%d of %s waits for you: risk class %s.", num("pr"), ref, str("class"))
			m.Choices = []string{"Approve"}
		case ApprovalPlan:
			m.Content = fmt.Sprintf("Plan %s %q waits for your acceptance: revision %d, %d cards.", ref, CardTitle(t), num("revision"), num("cards"))
			m.Choices = []string{"Accept"}
		default:
			return m, false
		}
	case EventTaskEnded:
		switch {
		case t.Kind != "agent":
			return m, false
		case str("state") == "failed":
			m.Content = fmt.Sprintf("%s failed: %s.", ref, CardTitle(t))
			m.Choices = []string{"Retry", "Leave it"}
		case str("state") == "done" && t.Parent == "":
			m.Content = fmt.Sprintf("%s is done: %s. €%.2f in all.", ref, CardTitle(t), t.SpentEUR)
		default:
			return m, false
		}
	case EventAllChildrenDone:
		m.Content = fmt.Sprintf("Plan %s is done: all %d cards.", ref, num("cards"))
	case EventAutonomyChanged:
		m.TaskID = ""
		from, to := str("from"), str("to")
		switch {
		case from != to:
			m.Content = fmt.Sprintf("%s set the autonomy from %s to %s: %s.", e.Actor, from, to, policy.AutonomyMeaning[to])
		case num("backlog_cap") > 0:
			m.Content = fmt.Sprintf("%s set the backlog cap to %d.", e.Actor, num("backlog_cap"))
		default:
			return m, false
		}
	default:
		verb, level := agentVerbs[e.Kind], str("autonomy")
		if verb == "" || str("decider") != "agent" || level == "" || level == policy.AutonomyManual {
			return m, false
		}
		if level == policy.AutonomyAutonomous && str("merge_rule") == policy.MergeAuto {
			level = policy.AutonomyFullAuto
		}
		title := fmt.Sprintf("%s %q", ref, CardTitle(t))
		switch e.Kind {
		case EventCardFiled:
			m.Content = "Filed " + title + "."
		case EventCardMoved:
			m.Content = fmt.Sprintf("Moved %s to lane %s.", title, str("lane"))
			if str("lane") == policy.LaneReady {
				verb, m.Content = "started", "Started "+title+"."
			}
		case EventPlanStarted:
			m.Content = "Started plan " + title + "."
		case EventPlanAccepted:
			m.Content = "Accepted plan " + title + "."
		case EventCardRetried:
			m.Content = "Retried " + title + "."
		case EventSessionGranted:
			m.Content = "Granted " + title + " a session."
		}
		m.Decider = level + " level · " + verb + " by the agent"
	}
	return m, true
}

// cardRef is how a poke names e's card: its issue, else its task id.
func cardRef(e Event, t Task) string {
	switch {
	case t.IssueNumber != 0:
		return fmt.Sprintf("#%d", t.IssueNumber)
	case e.IssueNumber != 0:
		return fmt.Sprintf("#%d", e.IssueNumber)
	}
	return e.TaskID
}

func (s *PGStore) PokeEvents(ctx context.Context, limit int) ([]Event, error) {
	// the first read starts at the newest event: no history pokes
	if _, err := s.db.ExecContext(ctx, `INSERT INTO chat_poke_cursor (event_id) SELECT coalesce(max(id), 0) FROM events
		ON CONFLICT DO NOTHING`); err != nil {
		return nil, err
	}
	return s.events(ctx, `SELECT `+eventColumns+` FROM events WHERE id > (SELECT event_id FROM chat_poke_cursor)
		AND kind = ANY($1) ORDER BY id LIMIT $2`, pokeKinds, limit)
}

func (s *PGStore) Poke(ctx context.Context, id int64, repository string, logins []string, m ChatMessage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after the commit
	for _, login := range logins {
		n, err := ensureInbox(ctx, tx, repository, login)
		if err != nil {
			return err
		}
		m.Conversation = n
		if m.TaskID != "" {
			// a repeat: the newest poke about the card says the same
			res, err := tx.ExecContext(ctx, `UPDATE chat_messages SET repeats = repeats + 1, at = now()
				WHERE id = (SELECT id FROM chat_messages WHERE repository = $1 AND login = $2 AND conversation = $3
					AND task_id = $4 AND kind IN ($5, $6) ORDER BY id DESC LIMIT 1)
				AND kind = $7 AND content = $8`, repository, login, n, m.TaskID, MessagePoke, MessageEscalation, m.Kind, m.Content)
			if err != nil {
				return err
			}
			if repeated, err := res.RowsAffected(); err != nil || repeated > 0 {
				if err != nil {
					return err
				}
				continue
			}
		}
		if _, err := insertChatMessage(ctx, tx, repository, login, m); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_poke_cursor SET event_id = greatest(event_id, $1)`, id); err != nil {
		return err
	}
	return tx.Commit()
}
