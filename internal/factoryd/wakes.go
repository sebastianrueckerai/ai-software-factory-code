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

// ErrWakeVenueGone marks a wake refusal that no retry can settle: the
// thread it names is deleted or archived. The Waker drops the wake's
// row instead of holding a cooldown.
var ErrWakeVenueGone = errors.New("the wake's thread is gone")

// WakeStore is the Waker's rows: the chat task, the inbox, the events and
// the wake cursor and marks.
type WakeStore interface {
	Task(ctx context.Context, id string) (Task, bool, error)
	CreateTask(ctx context.Context, t Task) error
	EnsureChatTask(ctx context.Context, name, repository string, budget float64, at time.Time) (Task, error)
	EnsureInbox(ctx context.Context, repository, login string) (int64, error)
	DueChatTimers(ctx context.Context, repository string, now time.Time) ([]ChatTimer, error)
	DeleteChatTimer(ctx context.Context, repository string, id int64) error
	ChatWatchesFor(ctx context.Context, repository, taskID string) ([]ChatWatch, error)
	RemoveChatWatch(ctx context.Context, repository, login, taskID string) (bool, error)
	DeleteChatWatchesOfTask(ctx context.Context, repository, taskID string) error
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
	// WakeTimersMax is a project's cap across watches and timers, the
	// runaway self-scheduling guard: an add past it is refused with the
	// count (ErrChatWakeCap).
	WakeTimersMax = 16
)

// ErrChatWakeCap is a project's watches-and-timers cap: the adding store
// call returns it wrapped with the count.
var ErrChatWakeCap = errors.New("this project's watches and timers are full")

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
			// one project's failure holds only that project back
			log.Printf("wake: %s: %v", name, err)
		}
	}
	return nil
}

// pendingAsk is one unhandled ask of a batch, with its mark key.
type pendingAsk struct {
	e   Event
	key string
}

// passProject wakes one project: a due timer fires on its own thread,
// the heartbeat wakes the inbox when nothing else moved, then the new
// events wake the watched cards and the asks. A batch nothing new wakes
// from only moves the cursor. The turn mark, the cooldown and a dead
// chat budget each hold a wake and the cursor back, so their cause stays
// and a later pass takes it.
func (w *Waker) passProject(ctx context.Context, v policy.Version, name string, p policy.ProjectConfig, now time.Time) error {
	if err := w.fireTimer(ctx, v, name, p, now); err != nil {
		return err
	}
	if err := w.heartbeat(ctx, v, name, p, now); err != nil {
		return err
	}
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
	var watched []pendingCard
	moved := map[string]bool{}
	for _, e := range events {
		if e.Kind == EventTaskEnded {
			// the card is gone however its end was filed: its watches and
			// bound timers go with it
			if err := w.Store.DeleteChatWatchesOfTask(ctx, p.Repository, e.TaskID); err != nil {
				return err
			}
		}
		if e.Via == ViaChat {
			continue // the agent's own doing wakes nobody
		}
		if e.Kind == EventQuestionAsked && p.Autonomy == policy.AutonomyAutonomous {
			sum := sha256.Sum256([]byte(askSentence(e)))
			key := "ask:" + e.TaskID + ":" + hex.EncodeToString(sum[:])
			if _, done, err := w.Store.WakeMarkAt(ctx, p.Repository, key); err != nil {
				return err
			} else if done {
				continue
			}
			pending = append(pending, pendingAsk{e, key})
			continue
		}
		if (e.Kind == EventCardMoved || e.Kind == EventTaskStatus) && p.Autonomy != policy.AutonomyManual && !moved[e.TaskID] {
			moved[e.TaskID] = true // one wake judges a card per batch
			watches, err := w.Store.ChatWatchesFor(ctx, p.Repository, e.TaskID)
			if err != nil {
				return err
			}
			if len(watches) == 0 {
				continue
			}
			if marked, err := w.markedWithin(ctx, p.Repository, "card:"+e.TaskID, time.Minute, now); err != nil {
				return err
			} else if marked {
				continue // the card woke its watchers within the minute
			}
			watched = append(watched, pendingCard{e, watches[0]})
		}
	}
	if len(pending) == 0 && len(watched) == 0 {
		return w.Store.SetWakeCursor(ctx, p.Repository, newest)
	}
	if held, err := w.held(ctx, p.Repository, now); err != nil {
		return err
	} else if held {
		return nil
	}
	task, live, err := w.chatTask(ctx, v, name, p, now)
	if err != nil {
		return err
	}
	if !live {
		if len(pending) > 0 {
			return w.askBudget(ctx, p.Repository, task, now) // the ask path owns the month-cap card
		}
		return nil // a raised cap lets the next pass wake
	}
	if len(watched) > 0 {
		c := watched[0]
		if err := w.Start.StartWake(ctx, name, c.watch.Login, c.watch.Conversation, "wake_card", w.cardText(ctx, c.e)); err != nil {
			if errors.Is(err, ErrWakeVenueGone) {
				log.Printf("wake %s: %v", name, err)
				// the watch's thread is gone for good: the row goes with it,
				// and this card no longer holds the batch back
				if _, err := w.Store.RemoveChatWatch(ctx, p.Repository, c.watch.Login, c.watch.TaskID); err != nil {
					return err
				}
				if len(pending) == 0 && len(watched) == 1 {
					return w.Store.SetWakeCursor(ctx, p.Repository, newest)
				}
				return nil
			}
			log.Printf("wake %s: %v", name, err)
			return w.Store.WakeMark(ctx, p.Repository, "cooldown", now)
		}
		for _, key := range []string{"card:" + c.e.TaskID, "turn"} {
			if err := w.Store.WakeMark(ctx, p.Repository, key, now); err != nil {
				return err
			}
		}
		if len(pending) == 0 && len(watched) == 1 {
			return w.Store.SetWakeCursor(ctx, p.Repository, newest)
		}
		return nil // the rest of the batch stays for the next pass
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

// pendingCard is one watched card's move of a batch, with the watch its
// wake turns on.
type pendingCard struct {
	e     Event
	watch ChatWatch
}

// fireTimer wakes the oldest due timer's own thread with its note, then
// the row is gone: one timer at a time, never combined with anything. A
// held turn or cooldown, and a dead chat budget, each leave the row for a
// later pass.
func (w *Waker) fireTimer(ctx context.Context, v policy.Version, name string, p policy.ProjectConfig, now time.Time) error {
	due, err := w.Store.DueChatTimers(ctx, p.Repository, now)
	if err != nil || len(due) == 0 {
		return err
	}
	t := due[0]
	if held, err := w.held(ctx, p.Repository, now); err != nil {
		return err
	} else if held {
		return nil
	}
	if _, live, err := w.chatTask(ctx, v, name, p, now); err != nil {
		return err
	} else if !live {
		return nil // the ask path owns the month-cap card; the row waits
	}
	if err := w.Start.StartWake(ctx, name, t.Login, t.Conversation, "wake_timer", timerText(t)); err != nil {
		if errors.Is(err, ErrWakeVenueGone) {
			log.Printf("wake %s: %v", name, err)
			// the timer's thread is gone for good: the row goes with it
			return w.Store.DeleteChatTimer(ctx, p.Repository, t.ID)
		}
		log.Printf("wake %s: %v", name, err)
		return w.Store.WakeMark(ctx, p.Repository, "cooldown", now)
	}
	if err := w.Store.DeleteChatTimer(ctx, p.Repository, t.ID); err != nil {
		return err
	}
	return w.Store.WakeMark(ctx, p.Repository, "turn", now)
}

// heartbeat wakes the project agent's inbox thread when nothing else
// woke it: every hour while work sits open, every day otherwise, on
// projects at assisted and above. A pulse that says a card waits on a
// human holds it — the needs-you card already has the member's eye — and
// so do a held turn, a dead chat budget and a missing member.
func (w *Waker) heartbeat(ctx context.Context, v policy.Version, name string, p policy.ProjectConfig, now time.Time) error {
	if w.Pulse == nil || p.Autonomy == policy.AutonomyManual {
		return nil
	}
	workOpen, waitingHuman, err := w.Pulse(ctx, p.Repository)
	if err != nil || waitingHuman {
		return err
	}
	key, within := "hb:idle", 24*time.Hour
	if workOpen {
		key, within = "hb:open", time.Hour
	}
	if marked, err := w.markedWithin(ctx, p.Repository, key, within, now); err != nil {
		return err
	} else if marked {
		return nil
	}
	if held, err := w.held(ctx, p.Repository, now); err != nil {
		return err
	} else if held {
		return nil
	}
	if _, live, err := w.chatTask(ctx, v, name, p, now); err != nil {
		return err
	} else if !live {
		return nil
	}
	logins := pokeTo(v.Tree, p.Repository)
	if len(logins) == 0 {
		return nil
	}
	conv, err := w.Store.EnsureInbox(ctx, p.Repository, logins[0])
	if err != nil {
		return err
	}
	if err := w.Start.StartWake(ctx, name, logins[0], conv, "wake_heartbeat", heartbeatText(name, workOpen)); err != nil {
		log.Printf("wake %s: %v", name, err)
		return w.Store.WakeMark(ctx, p.Repository, "cooldown", now)
	}
	for _, k := range []string{key, "turn"} {
		if err := w.Store.WakeMark(ctx, p.Repository, k, now); err != nil {
			return err
		}
	}
	return nil
}

// held says a turn or the cooldown holds repository's wakes back.
func (w *Waker) held(ctx context.Context, repository string, now time.Time) (bool, error) {
	for _, gap := range []struct {
		key    string
		within time.Duration
	}{{"turn", wakeTurnGap}, {"cooldown", wakeCooldown}} {
		if marked, err := w.markedWithin(ctx, repository, gap.key, gap.within, now); err != nil {
			return false, err
		} else if marked {
			return true, nil
		}
	}
	return false, nil
}

// chatTask is the project's chat task, live when its budget is not
// spent.
func (w *Waker) chatTask(ctx context.Context, v policy.Version, name string, p policy.ProjectConfig, now time.Time) (Task, bool, error) {
	chat := v.Document.ChatOf(p)
	task, err := w.Store.EnsureChatTask(ctx, name, p.Repository, chat.MonthlyEUR, now)
	if err != nil {
		return task, false, err
	}
	return task, task.SpentEUR < task.BudgetEUR, nil
}

// timerText is the timer's wake: its note, then what the turn does with
// it.
func timerText(t ChatTimer) string {
	return "Timer: " + t.Note + ". Run the check it stood for, then set a new timer if the thing still needs watching."
}

// heartbeatText is the heartbeat's wake: the cadence it woke on, then
// the turn's charge, which the agent's own read of the board carries.
func heartbeatText(name string, workOpen bool) string {
	if workOpen {
		return "The hourly heartbeat on " + name + ": work sits open and no event woke you. Read the board and keep it moving."
	}
	return "The daily heartbeat on " + name + ": the board is idle. Look for what to start or improve, then act."
}

// cardText is the watched card's wake: the card, then the turn's charge.
func (w *Waker) cardText(ctx context.Context, e Event) string {
	t, _, _ := w.Store.Task(ctx, e.TaskID)
	return fmt.Sprintf("The watched card %s moved: read the card and act on where it stands.", cardRef(e, t))
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
