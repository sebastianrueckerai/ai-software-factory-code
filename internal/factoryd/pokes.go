package factoryd

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// Pokes (roadmap 239): factoryd's own turns in the inbox thread of each
// member a project tells, its notify list else its approvers, on 145's
// event classes: a card that needs a human (and one that needs them no
// more, its question withdrawn), a card stopped by its budget, a terminal
// failure, a card or plan done, the agent's own starts and filings at
// assisted and autonomous, a change of the level, and a routine's run
// that tells the inbox (roadmap 267). A poke
// is a row written through the message path: no model turn, one per state
// change, behind a turn in flight in the inbox as every writer's rows
// (roadmap 263). A repeat of the newest poke about the same card counts on
// that row (74's rule).

const (
	// MessagePoke is a poke's kind; MessageEscalation a poke about a card
	// that waits for a person, which the chat marks "needs you".
	MessagePoke       = "poke"
	MessageEscalation = "escalation"
)

// ChoiceRerun is the inbox's answer that runs a factory pull request's red
// checks again (roadmap 264).
const ChoiceRerun = "Re-run the checks"

// pokeKinds are the events a poke may answer.
var pokeKinds = []string{EventQuestionAsked, EventQuestionWithdrawn, EventApprovalRequested, EventTaskEnded, EventAllChildrenDone, EventAutonomyChanged,
	EventCardFiled, EventCardMoved, EventPlanStarted, EventPlanAccepted, EventCardRetried, EventSessionGranted, EventRoutineRan}

// budgetSources are the escalation sources of a card stopped by money.
var budgetSources = []string{"budget", "cap", SourceMonthCap, "project_cap", "day_cap", "split_cap"}

// agentVerbs are the agent's own actions a poke tells, by event kind.
var agentVerbs = map[string]string{EventCardFiled: "filed", EventCardMoved: "moved", EventPlanStarted: "started",
	EventPlanAccepted: "accepted", EventCardRetried: "retried", EventSessionGranted: "granted"}

// PokeDelivery is a poke's messenger half (roadmap 145): the member level
// its row asks (empty for a row that never leaves the factory), the
// members whose derived default is needs-you (the resolved notify list),
// and whether the project's file allows delivery.
type PokeDelivery struct {
	RowLevel string
	Derived  []string
	On       bool
}

// PokeStore is what the poke writer reads and writes.
type PokeStore interface {
	Task(ctx context.Context, id string) (Task, bool, error)
	// PokeEvents are the poke events after the poke cursor, oldest first,
	// at most limit.
	PokeEvents(ctx context.Context, limit int) ([]Event, error)
	EnsureInbox(ctx context.Context, repository, login string) (int64, error)
	// PokeRecipients are a poke's recipients on repository: base plus every
	// member whose explicit level row (needs-you or all) joins them.
	PokeRecipients(ctx context.Context, repository string, base []string) ([]string, error)
	// Poke writes m into the inbox thread of each of logins on repository,
	// queued where queued says so, one pending delivery row per connection
	// whose member's level passes plan's row level, and moves the cursor
	// past event id, in one transaction; it answers each login's new row,
	// 0 for a repeat.
	Poke(ctx context.Context, id int64, repository string, logins []string, queued []bool, m ChatMessage, plan PokeDelivery) ([]int64, error)
}

// Line is the chat's one line of writers on each thread (roadmap 263), the
// console's: Behind takes a place behind thread n's turn in flight and
// those waiting before it. queued says one is ahead, so the writer stores
// its rows queued; land, called once with their ids, starts them in their
// order when the place comes and leaves it.
type Line interface {
	Behind(repository, login string, n int64) (queued bool, land func(ids ...int64))
}

// Poker writes the pokes.
type Poker struct {
	Store   PokeStore
	Current func() policy.Version
	Line    Line
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
				to = pokeTo(tree, e.Repository)
			}
			plan := PokeDelivery{RowLevel: deliveryLevel(m), Derived: to, On: true}
			if _, c, found := tree.ProjectOf(e.Repository); found {
				plan.On = c.MessengersOn()
			}
			if err := p.poke(ctx, e, to, m, plan); err != nil {
				return err
			}
		}
		if len(events) < pokeBatch {
			return nil
		}
	}
}

// poke writes m into the inbox of each of logins behind a turn in flight
// there and moves the cursor past event e.
func (p *Poker) poke(ctx context.Context, e Event, logins []string, m ChatMessage, plan PokeDelivery) error {
	if plan.On && len(logins) > 0 {
		joined, err := p.Store.PokeRecipients(ctx, e.Repository, logins)
		if err != nil {
			return err
		}
		logins = joined
	}
	queued, ids := make([]bool, len(logins)), make([]int64, len(logins))
	var lands []func(...int64)
	defer func() {
		for i, land := range lands {
			land(ids[i])
		}
	}()
	for i, login := range logins {
		n, err := p.Store.EnsureInbox(ctx, e.Repository, login)
		if err != nil {
			return err
		}
		var land func(...int64)
		queued[i], land = p.Line.Behind(e.Repository, login, n)
		lands = append(lands, land)
	}
	got, err := p.Store.Poke(ctx, e.ID, e.Repository, logins, queued, m, plan)
	if err != nil {
		return err
	}
	copy(ids, got)
	return nil
}

// deliveryLevel is the member level a poke's row asks for its messenger
// copy: every ask that needs a person, and the informs of a finished or
// failed card or plan; the rest stays in the inbox.
func deliveryLevel(m ChatMessage) string {
	if m.Kind == MessageEscalation {
		return LevelNeedsYou
	}
	switch m.BirthKind {
	case EventTaskEnded, EventAllChildrenDone:
		return LevelAll
	}
	return ""
}

// pokeTo are the members a poke on repository tells: its project's
// notify list, else its approvers.
func pokeTo(tree policy.Tree, repository string) []string {
	if _, c, ok := tree.ProjectOf(repository); ok && len(c.Notify) > 0 {
		return c.Notify
	}
	return tree.ApproversOf(repository)
}

// pokeOf is event e's poke, t the card it names, false for an event that
// pokes nobody.
func pokeOf(e Event, t Task) (ChatMessage, bool) {
	ref := cardRef(e, t)
	str := func(k string) string { s, _ := e.Detail[k].(string); return s }
	num := func(k string) int64 { n, _ := e.Detail[k].(float64); return int64(n) }
	m := ChatMessage{Role: "assistant", Kind: MessagePoke, TaskID: e.TaskID, BirthKind: e.Kind}
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
			if rerun, _ := e.Detail["rerun"].(bool); rerun {
				m.Content = fmt.Sprintf("PR #%d of %s waits for you: its checks are red, so it cannot merge; run them again, or close it with your words.", num("pr"), ref)
				m.Choices = []string{ChoiceRerun}
			}
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
			m.Content = fmt.Sprintf("%s failed: %s.", ref, strings.TrimSuffix(FirstLine(str("reason")), "."))
			m.Choices = []string{"Retry", "Leave it"}
		case str("state") == "done" && t.Parent == "":
			m.Content = fmt.Sprintf("%s is done: %s. €%.2f in all.", ref, CardTitle(t), t.SpentEUR)
		default:
			return m, false
		}
	case EventAllChildrenDone:
		m.Content = fmt.Sprintf("Plan %s is done: all %d cards.", ref, num("cards"))
	case EventRoutineRan:
		if str("poke") == "" {
			return m, false
		}
		m.Content = str("poke")
		m.Decider = fmt.Sprintf("routine %s · run %d · %s", str("routine"), num("run"), str("schedule"))
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

func (s *PGStore) Poke(ctx context.Context, id int64, repository string, logins []string, queued []bool, m ChatMessage, plan PokeDelivery) ([]int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after the commit
	ids := make([]int64, len(logins))
	for i, login := range logins {
		n, err := ensureInbox(ctx, tx, repository, login)
		if err != nil {
			return nil, err
		}
		m.Conversation, m.Queued = n, queued[i]
		if m.TaskID != "" {
			// a repeat: the newest poke about the card says the same
			res, err := tx.ExecContext(ctx, `UPDATE chat_messages SET repeats = repeats + 1, at = now()
				WHERE id = (SELECT id FROM chat_messages WHERE repository = $1 AND login = $2 AND conversation = $3
					AND task_id = $4 AND kind IN ($5, $6) ORDER BY id DESC LIMIT 1)
				AND kind = $7 AND content = $8`, repository, login, n, m.TaskID, MessagePoke, MessageEscalation, m.Kind, m.Content)
			if err != nil {
				return nil, err
			}
			if repeated, err := res.RowsAffected(); err != nil || repeated > 0 {
				if err != nil {
					return nil, err
				}
				continue
			}
		}
		if ids[i], err = insertChatMessage(ctx, tx, repository, login, m); err != nil {
			return nil, err
		}
	}
	if err := queueDeliveries(ctx, tx, repository, logins, ids, plan); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_poke_cursor SET event_id = greatest(event_id, $1)`, id); err != nil {
		return nil, err
	}
	return ids, tx.Commit()
}
