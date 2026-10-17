package factoryd

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// WakeStarter starts one wake turn on the member's thread: the console's
// seam (Console.StartWake). The turn runs without the member's request
// and ends before StartWake returns.
type WakeStarter interface {
	StartWake(ctx context.Context, project, login string, conversation int64, birth, text string) error
}

// WakeStore is the Waker's rows: the chat task, the inbox, the events and
// the wake cursor and marks.
type WakeStore interface {
	Task(ctx context.Context, id string) (Task, bool, error)
	CreateTask(ctx context.Context, t Task) error
	EnsureChatTask(ctx context.Context, name, repository string, budget float64, at time.Time) (Task, error)
	EnsureInbox(ctx context.Context, repository, login string) (int64, error)
	WakeEvents(ctx context.Context, repository string, after int64, limit int) ([]Event, error)
	WakeCursor(ctx context.Context, repository string) (int64, error)
	SetWakeCursor(ctx context.Context, repository string, id int64) error
	WakeMark(ctx context.Context, repository, key string, at time.Time) error
	WakeMarkAt(ctx context.Context, repository, key string) (time.Time, bool, error)
}

// The Waker wakes the project agent's turn on the factory's own events
// (the concept's wake): an ask the agent did not file becomes one turn on
// the venue member's inbox thread that names every ask of the batch. Its
// cursor and marks are rows, so a restart or a repeat changes nothing,
// and no wake follows an event the agent itself caused.
type Waker struct {
	Store   WakeStore
	Current func() policy.Version
	Start   WakeStarter
	// Pulse reads the board's pulse for the heartbeat wake (the timers
	// concept); nil means none.
	Pulse func(ctx context.Context, repository string) (workOpen, waitingHuman bool, err error)
	// Forgejo and Timeline file the month-cap card when the chat task's
	// budget is dead.
	Forgejo  *Forgejo
	Timeline *Timeline
	Now      func() time.Time
}

const (
	// wakeTurnGap is how close two wakes may sit.
	wakeTurnGap = 10 * time.Second
	// wakeCooldown is how long a project whose wake failed waits.
	wakeCooldown = 60 * time.Second
	// wakeBatch bounds the events one read takes.
	wakeBatch = 100
)

// wakeKinds are the events the Waker reads: the asks, and the card and
// task moves the watches and the heartbeat judge.
var wakeKinds = []string{EventQuestionAsked, EventCardMoved, EventTaskStatus, EventTaskEnded}

// Loop passes every interval until ctx ends.
func (w *Waker) Loop(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := w.Pass(ctx); err != nil {
				log.Printf("wake: %v", err)
			}
		}
	}
}

// Pass reads every project's events past its cursor once and wakes what
// they ask.
func (w *Waker) Pass(ctx context.Context) error {
	v := w.Current()
	now := w.now()
	for name, p := range v.Tree.Projects {
		if err := w.passProject(ctx, v, name, p, now); err != nil {
			return err
		}
	}
	return nil
}

// pendingAsk is one unhandled ask of a batch, with its mark key.
type pendingAsk struct {
	e   Event
	key string
}

// passProject wakes one project on its new events. A batch nothing new
// asks from only moves the cursor. The turn mark, the cooldown and a
// dead chat budget each hold the cursor back, so their cause stays and a
// later pass takes it.
func (w *Waker) passProject(ctx context.Context, v policy.Version, name string, p policy.ProjectConfig, now time.Time) error {
	after, err := w.Store.WakeCursor(ctx, p.Repository)
	if err != nil {
		return err
	}
	events, err := w.Store.WakeEvents(ctx, p.Repository, after, wakeBatch)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	newest := events[len(events)-1].ID
	var pending []pendingAsk
	for _, e := range events {
		if e.Via == ViaChat {
			continue // the agent's own doing wakes nobody
		}
		if e.Kind != EventQuestionAsked || p.Autonomy != policy.AutonomyAutonomous {
			continue
		}
		sum := sha256.Sum256([]byte(askSentence(e)))
		key := "ask:" + e.TaskID + ":" + hex.EncodeToString(sum[:])
		if _, done, err := w.Store.WakeMarkAt(ctx, p.Repository, key); err != nil {
			return err
		} else if done {
			continue
		}
		pending = append(pending, pendingAsk{e, key})
	}
	if len(pending) == 0 {
		return w.Store.SetWakeCursor(ctx, p.Repository, newest)
	}
	for _, gap := range []struct {
		key    string
		within time.Duration
	}{{"turn", wakeTurnGap}, {"cooldown", wakeCooldown}} {
		if marked, err := w.markedWithin(ctx, p.Repository, gap.key, gap.within, now); err != nil {
			return err
		} else if marked {
			return nil
		}
	}
	chat := v.Document.ChatOf(p)
	task, err := w.Store.EnsureChatTask(ctx, name, p.Repository, chat.MonthlyEUR, now)
	if err != nil {
		return err
	}
	if task.SpentEUR >= task.BudgetEUR {
		return w.askBudget(ctx, p.Repository, task, now)
	}
	logins := pokeTo(v.Tree, p.Repository)
	if len(logins) == 0 {
		return w.Store.SetWakeCursor(ctx, p.Repository, newest)
	}
	conv, err := w.Store.EnsureInbox(ctx, p.Repository, logins[0])
	if err != nil {
		return err
	}
	if err := w.Start.StartWake(ctx, name, logins[0], conv, "wake_ask", w.askText(ctx, pending)); err != nil {
		log.Printf("wake %s: %v", name, err)
		return w.Store.WakeMark(ctx, p.Repository, "cooldown", now)
	}
	for _, a := range pending {
		if err := w.Store.WakeMark(ctx, p.Repository, a.key, now); err != nil {
			return err
		}
	}
	if err := w.Store.WakeMark(ctx, p.Repository, "turn", now); err != nil {
		return err
	}
	return w.Store.SetWakeCursor(ctx, p.Repository, newest)
}

// askBudget files the month-cap card once for the month of a dead chat
// budget and leaves the cursor behind: a raised cap lets the next pass
// wake.
func (w *Waker) askBudget(ctx context.Context, repository string, task Task, now time.Time) error {
	key := "budget:" + now.Format("2006-01")
	if _, done, err := w.Store.WakeMarkAt(ctx, repository, key); err != nil {
		return err
	} else if done {
		return nil
	}
	repo, err := SplitRepo(repository)
	if err != nil {
		return err
	}
	reason := monthCapReached + fmt.Sprintf("the chat task %s spent %.2f of its %.2f EUR", task.ID, task.SpentEUR, task.BudgetEUR)
	if _, err := EscalateToHuman(ctx, w.Store, w.Forgejo, w.Timeline, repo, *monthCapQuestion(reason), "the waker"); err != nil {
		return err
	}
	return w.Store.WakeMark(ctx, repository, key, now)
}

// askText is the wake's text: every ask of the batch with its card ref,
// then what the turn does with them.
func (w *Waker) askText(ctx context.Context, asks []pendingAsk) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Wake: %d asks wait for a human.", len(asks))
	for _, a := range asks {
		t, _, _ := w.Store.Task(ctx, a.e.TaskID)
		fmt.Fprintf(&b, "\n%s: %s", cardRef(a.e, t), askSentence(a.e))
	}
	b.WriteString("\n\nRead each card. Settle what your permissions allow. For what stays the human's, record what you found with attach_analysis. An ask already answered is done.")
	return b.String()
}

// askSentence is the ask event's own words: a shaped ask carries its full
// ask, the rest the question every real asker records.
func askSentence(e Event) string {
	if s, _ := e.Detail["ask"].(string); s != "" {
		return s
	}
	s, _ := e.Detail["question"].(string)
	return s
}

// markedWithin says a mark of repository sits within of now.
func (w *Waker) markedWithin(ctx context.Context, repository, key string, within time.Duration, now time.Time) (bool, error) {
	at, ok, err := w.Store.WakeMarkAt(ctx, repository, key)
	if err != nil || !ok {
		return false, err
	}
	return now.Sub(at) < within, nil
}

func (w *Waker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// WakeEvents lists repository's wake-kind events above after, oldest
// first, at most limit.
func (s *PGStore) WakeEvents(ctx context.Context, repository string, after int64, limit int) ([]Event, error) {
	return s.events(ctx, `SELECT `+eventColumns+` FROM events
		WHERE repository = $1 AND id > $2 AND kind = ANY($3) ORDER BY id LIMIT $4`,
		repository, after, wakeKinds, limit)
}

// WakeCursor is repository's last handled event. The first read plants it
// at the newest event, so no history wakes.
func (s *PGStore) WakeCursor(ctx context.Context, repository string) (int64, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO chat_wake_cursor (repository, event_id)
		SELECT $1, coalesce(max(id), 0) FROM events ON CONFLICT DO NOTHING`, repository); err != nil {
		return 0, err
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT event_id FROM chat_wake_cursor WHERE repository = $1`, repository).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// SetWakeCursor moves repository's cursor to id, never back.
func (s *PGStore) SetWakeCursor(ctx context.Context, repository string, id int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_wake_cursor (repository, event_id) VALUES ($1, $2)
		ON CONFLICT (repository) DO UPDATE SET event_id = greatest(chat_wake_cursor.event_id, excluded.event_id)`,
		repository, id)
	return err
}

// WakeMark upserts one of the Waker's marks of repository.
func (s *PGStore) WakeMark(ctx context.Context, repository, key string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_wake_state (repository, key, at) VALUES ($1, $2, $3)
		ON CONFLICT (repository, key) DO UPDATE SET at = excluded.at`, repository, key, at)
	return err
}

// WakeMarkAt reads one of the Waker's marks of repository.
func (s *PGStore) WakeMarkAt(ctx context.Context, repository, key string) (time.Time, bool, error) {
	var at time.Time
	err := s.db.QueryRowContext(ctx, `SELECT at FROM chat_wake_state WHERE repository = $1 AND key = $2`,
		repository, key).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return at, true, nil
}
