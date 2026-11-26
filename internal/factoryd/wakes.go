package factoryd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// wakeCaseIDs are the cases handed to rung 2 that a wake was woken for, each
// once, in order: the cases whose rung 2 step the turn's cost lands on.
func wakeCaseIDs(asks []pendingAsk) []int64 {
	var out []int64
	for _, a := range asks {
		if id, _ := toInt(a.e.Detail["case_id"]); id != 0 && !slices.Contains(out, int64(id)) {
			out = append(out, int64(id))
		}
	}
	return out
}

// The wakes' birth kinds: WakeCard a watched card's news, WakeTimer a timer
// the agent set, WakeRefill a queue whose waiting cards fell below
// refill_below. A wake for cases handed to rung 2 is born case_climbed.
// WakeAsk is the kind of the stored rows of a wake that named asks: no
// wake is born so any more, and the inbox still reads those rows.
const (
	WakeAsk    = "wake_ask"
	WakeCard   = "wake_card"
	WakeTimer  = "wake_timer"
	WakeRefill = "queue_refill"
)

// WakeStarter starts one wake turn on the member's thread: the console's
// seam (Console.StartWake). The turn runs without the member's request
// and ends before StartWake returns.
type WakeStarter interface {
	StartWake(ctx context.Context, project, login string, conversation int64, birth, text string, cases ...int64) error
}

// ErrWakeVenueGone marks a wake refusal that no retry can settle: the
// thread it names is deleted or archived. The Waker drops the wake's
// row instead of holding a cooldown.
var ErrWakeVenueGone = errors.New("the wake's thread is gone")

// ErrChatUnfunded marks a wake refused because the project's chat stands
// at its budget: the chat_budget_spent event recorded the stop, and no
// fund or raise of the policy lifted it since. The Waker holds the wake as
// it holds any refused one, the cooldown marked and the cursor or the
// timer kept, so its cause wakes the turn once the chat is funded.
var ErrChatUnfunded = errors.New("the judgment budget is used up")

// WakeStore is the Waker's rows: the cards, the inbox, the events and the
// wake cursor and marks.
type WakeStore interface {
	Task(ctx context.Context, id string) (Task, bool, error)
	EnsureInbox(ctx context.Context, repository, login string) (int64, error)
	DueChatTimers(ctx context.Context, repository string, now time.Time) ([]ChatTimer, error)
	DeleteChatTimer(ctx context.Context, repository string, id int64) error
	ChatWatchesFor(ctx context.Context, repository, taskID string) ([]ChatWatch, error)
	RemoveChatWatch(ctx context.Context, repository, login, taskID string) (bool, error)
	DeleteChatWatchesOfTask(ctx context.Context, repository, taskID string) error
	CaseSteps(ctx context.Context, caseID int64) ([]CaseStep, error)
	WakeEvents(ctx context.Context, repository string, after int64, limit int) ([]Event, error)
	WakeCursor(ctx context.Context, repository string) (int64, error)
	SetWakeCursor(ctx context.Context, repository string, id int64) error
	WakeMark(ctx context.Context, repository, key string, at time.Time) error
	WakeMarkAt(ctx context.Context, repository, key string) (time.Time, bool, error)
	CountQueue(ctx context.Context, repository string) (int, error)
	ChatIntakeClosed(ctx context.Context, repository string) (bool, error)
	RoadmapPullStamp(ctx context.Context, repository string) (string, bool, error)
	SetRoadmapPullStamp(ctx context.Context, repository, head string, at time.Time) error
}

// The Waker wakes the project agent's turn on the factory's own events
// (the concept's wake): a case handed to rung 2 becomes one turn on the
// venue member's inbox thread that names every case of the batch. Its
// cursor and marks are rows, so a restart or a repeat changes nothing,
// and no wake follows an event the agent itself caused. Wakes fire on
// cases handed to rung 2, watched cards and timers, never on the clock
// alone.
type Waker struct {
	Store   WakeStore
	Current func() policy.Version
	Start   WakeStarter
	Now     func() time.Time
	// RoadmapHead reads the doc repository's head of docs/roadmap.md, the
	// identity the pull checks and stamps. Nil: the pull never fires.
	RoadmapHead func(ctx context.Context, docRepo string) (string, error)
	// Statement reads case id's statement for rung 2 and Board the
	// project's board digest, both masked: the text of a case's wake. Nil:
	// the wake names the ask alone.
	Statement func(ctx context.Context, caseID int64) (string, error)
	Board     func(ctx context.Context, repository string) (string, error)

	// flights are the repositories whose wake turn runs; a pass leaves
	// such a project to its turn instead of waiting on it
	mu      sync.Mutex
	flights map[string]bool
	wg      sync.WaitGroup
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

// wakeKinds are the events the Waker reads: the cases handed to rung 2, and
// the task moves the watches judge. An ask wakes nobody: its case climbs to
// rung 2 on case_climbed, or reaches the human with no rung able to run.
var wakeKinds = []string{EventCaseClimbed, EventTaskStatus, EventTaskEnded}

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

// launch takes repository's wake turn for this Waker: false when one
// already runs, so the caller leaves the project to it.
func (w *Waker) launch(repository string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.flights == nil {
		w.flights = map[string]bool{}
	}
	if w.flights[repository] {
		return false
	}
	w.flights[repository] = true
	w.wg.Add(1)
	return true
}

// land gives repository's wake turn back.
func (w *Waker) land(repository string) {
	w.mu.Lock()
	delete(w.flights, repository)
	w.mu.Unlock()
	w.wg.Done()
}

// flying says repository's wake turn runs.
func (w *Waker) flying(repository string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flights[repository]
}

// Drain waits for every wake turn a pass started.
func (w *Waker) Drain() { w.wg.Wait() }

// goWake runs one wake turn on its own goroutine, one at a time per
// project: turn starts it and keeps its own bookkeeping — the marks, the
// row deletes, the cursor — and its error lands in the log, for a turn
// minutes long must hold no pass and no other project back.
func (w *Waker) goWake(ctx context.Context, repository string, turn func(ctx context.Context) error) {
	if !w.launch(repository) {
		return
	}
	go func() {
		defer w.land(repository)
		if err := turn(ctx); err != nil {
			log.Printf("wake %s: %v", repository, err)
		}
	}()
}

// pendingAsk is one unhandled ask of a batch, with its mark key.
type pendingAsk struct {
	e   Event
	key string
}

// passProject wakes one project: a due timer fires on its own thread,
// then the new events wake the watched cards and the asks. A batch
// nothing new wakes from only moves the cursor. The turn mark and the
// cooldown each hold a wake and the cursor back, and a refused wake (a
// stopped chat's ErrChatUnfunded among them) marks the cooldown, so its
// cause stays and a later pass takes it. A wake turn still running holds
// the project's passes too: its bookkeeping lands when it ends.
func (w *Waker) passProject(ctx context.Context, v policy.Version, name string, p policy.ProjectConfig, now time.Time) error {
	if w.flying(p.Repository) {
		return nil
	}
	if err := w.fireTimer(ctx, name, p, now); err != nil {
		return err
	}
	if err := w.fireRefill(ctx, v, name, p, now); err != nil {
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
		if e.Kind == EventCaseClimbed {
			if worked, err := w.staleCaseEvent(ctx, e); err != nil {
				return err
			} else if worked {
				continue
			}
			key := askKey(e)
			if _, done, err := w.Store.WakeMarkAt(ctx, p.Repository, key); err != nil {
				return err
			} else if done {
				continue
			}
			pending = append(pending, pendingAsk{e, key})
			continue
		}
		if e.Kind == EventTaskStatus && p.Autonomy != policy.AutonomyManual && !moved[e.TaskID] {
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
	if len(watched) > 0 {
		c := watched[0]
		w.goWake(ctx, p.Repository, func(ctx context.Context) error {
			if err := w.Start.StartWake(ctx, name, c.watch.Login, c.watch.Conversation, WakeCard, w.cardText(ctx, c.e)); err != nil {
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
		})
		return nil
	}
	logins := pokeTo(v.Tree, p.Repository)
	if len(logins) == 0 {
		return w.Store.SetWakeCursor(ctx, p.Repository, newest)
	}
	conv, err := w.Store.EnsureInbox(ctx, p.Repository, logins[0])
	if err != nil {
		return err
	}
	asks, text := pending, w.askText(ctx, pending, p.Repository)
	w.goWake(ctx, p.Repository, func(ctx context.Context) error {
		if err := w.Start.StartWake(ctx, name, logins[0], conv, EventCaseClimbed, text, wakeCaseIDs(pending)...); err != nil {
			log.Printf("wake %s: %v", name, err)
			return w.Store.WakeMark(ctx, p.Repository, "cooldown", now)
		}
		for _, a := range asks {
			if err := w.Store.WakeMark(ctx, p.Repository, a.key, now); err != nil {
				return err
			}
		}
		if err := w.Store.WakeMark(ctx, p.Repository, "turn", now); err != nil {
			return err
		}
		return w.Store.SetWakeCursor(ctx, p.Repository, newest)
	})
	return nil
}

// pendingCard is one watched card's move of a batch, with the watch its
// wake turns on.
type pendingCard struct {
	e     Event
	watch ChatWatch
}

// fireTimer wakes the oldest due timer's own thread with its note, then
// the row is gone: one timer at a time, never combined with anything. The
// wake turn runs on its own goroutine and keeps its own bookkeeping. A
// held turn or cooldown, and a refused wake (a stopped chat's among them),
// each leave the row for a later pass.
func (w *Waker) fireTimer(ctx context.Context, name string, p policy.ProjectConfig, now time.Time) error {
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
	w.goWake(ctx, p.Repository, func(ctx context.Context) error {
		if err := w.Start.StartWake(ctx, name, t.Login, t.Conversation, WakeTimer, timerText(t)); err != nil {
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
	})
	return nil
}

// fireRefill wakes the roadmap pull (2026-11-01): when the project's
// intake is open, its waiting cards fall below the tree's refill_below
// and the doc repository's head of docs/roadmap.md is one no pull turn
// of the project read, one turn reads the roadmap and files its ripe
// items. The wake stamps the head it read, so a turn that files nothing
// never loops and a failed start moves no stamp; the turn gaps and the
// cooldown hold the pull like any wake.
func (w *Waker) fireRefill(ctx context.Context, v policy.Version, name string, p policy.ProjectConfig, now time.Time) error {
	below := v.Tree.RefillBelowOf(p.Repository)
	if below <= 0 || w.RoadmapHead == nil {
		return nil
	}
	closed, err := w.Store.ChatIntakeClosed(ctx, p.Repository)
	if err != nil || closed {
		return err
	}
	docRepo := v.Tree.Document.ChatOf(p).DocRepo
	if docRepo == "" {
		return nil
	}
	waiting, err := w.Store.CountQueue(ctx, p.Repository)
	if err != nil {
		return err
	}
	if waiting >= below {
		return nil
	}
	if held, err := w.held(ctx, p.Repository, now); err != nil {
		return err
	} else if held {
		return nil
	}
	head, err := w.RoadmapHead(ctx, docRepo)
	if err != nil {
		// one unread head skips this pass — the whole loop of projects
		// must not wait on one Forgejo answer
		log.Printf("waker: the roadmap head of %s: %v", docRepo, err)
		return nil
	}
	if stamped, ok, err := w.Store.RoadmapPullStamp(ctx, p.Repository); err != nil {
		return err
	} else if ok && stamped == head {
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
	w.goWake(ctx, p.Repository, func(ctx context.Context) error {
		if err := w.Start.StartWake(ctx, name, logins[0], conv, WakeRefill, refillText(name, below)); err != nil {
			log.Printf("wake %s: %v", name, err)
			return w.Store.WakeMark(ctx, p.Repository, "cooldown", now)
		}
		if err := w.Store.SetRoadmapPullStamp(ctx, p.Repository, head, now); err != nil {
			return err
		}
		return w.Store.WakeMark(ctx, p.Repository, "turn", now)
	})
	return nil
}

// refillText is the roadmap pull's wake: what the turn reads and files.
func refillText(project string, below int) string {
	return fmt.Sprintf("The queue of %s is below %d: read docs/roadmap.md of the project's doc repository, "+
		"file every item whose Priority is filled and whose number no card cites, then end.", project, below)
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

// timerText is the timer's wake: its note, then what the turn does with
// it.
func timerText(t ChatTimer) string {
	return "Timer: " + t.Note + ". Run the check it stood for, then set a new timer if the thing still needs watching."
}

// cardText is the watched card's wake: the card, then the turn's charge.
func (w *Waker) cardText(ctx context.Context, e Event) string {
	t, _, _ := w.Store.Task(ctx, e.TaskID)
	return fmt.Sprintf("The watched card %s moved: read the card and act on where it stands.", cardRef(e, t))
}

// WakeCards are the cards a wake's text names in its first line, by
// their refs (cardRef): the cards a wake turn looked at.
func WakeCards(text string) []string {
	first, _, _ := strings.Cut(text, "\n")
	return wakeRef.FindAllString(first, -1)
}

var wakeRef = regexp.MustCompile(`#\d+\b|\bt-[0-9a-z][0-9a-z-]*`)

// askText is the wake's text: its first line the cases handed to rung 2 of
// the batch counted, with the refs of their cards (WakeCards), then each
// with its card ref, its statement for rung 2 and the two ends of the turn,
// then the project's board digest once.
func (w *Waker) askText(ctx context.Context, asks []pendingAsk, repository string) string {
	verb := "wait"
	if len(asks) == 1 {
		verb = "waits"
	}
	cards := make([]Task, len(asks))
	var refs []string
	for i, a := range asks {
		cards[i], _, _ = w.Store.Task(ctx, a.e.TaskID)
		if ref := cardRef(a.e, cards[i]); !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Wake: %s %s for the factory agent: %s.", plural(len(asks), "case"), verb, strings.Join(refs, ", "))
	for i, a := range asks {
		t := cards[i]
		line, _ := a.e.Detail["problem"].(string)
		id, _ := toInt(a.e.Detail["case_id"])
		withStatement := id != 0 && w.Statement != nil
		if withStatement {
			// the statement's first line is the case's problem
			fmt.Fprintf(&b, "\n%s:", cardRef(a.e, t))
		} else {
			fmt.Fprintf(&b, "\n%s: %s", cardRef(a.e, t), line)
			continue
		}
		statement, err := w.Statement(ctx, int64(id))
		if err != nil {
			log.Printf("wake %s: the statement of case %d: %v", repository, id, err)
			fmt.Fprintf(&b, " %s\nThe statement of case %d could not be built: %s", line, id, err)
			continue
		}
		end := fmt.Sprintf("End case %d with answer and its case_id, or with attach_analysis, its case_id and the recommended action.", id)
		kind, _ := a.e.Detail["kind"].(string)
		switch {
		case statementOffers(statement, ActExtend):
			end = fmt.Sprintf("End case %d with give_room for more room, with answer and its case_id for its other actions, or with attach_analysis, its case_id and the recommended action.", id)
		case kind == string(CasePlanQuestion) || kind == string(CasePlanConfirmation):
			end = fmt.Sprintf("End case %d with answer on the plan's revision and question, with no case_id, or with attach_analysis, case_id %d and the recommended answer.", id, id)
		case ChoiceKind(CaseKind(kind)):
			end = fmt.Sprintf("End case %d with answer, its case_id and one of the card's choices as text, or with attach_analysis, case_id %d and a choice's value as the recommended answer.", id, id)
		}
		fmt.Fprintf(&b, "\n%s\n%s", statement, end)
	}
	if w.Board != nil {
		if board, err := w.Board(ctx, repository); err != nil {
			log.Printf("wake %s: the board digest: %v", repository, err)
			fmt.Fprintf(&b, "\n\nThe board digest could not be built: %s", err)
		} else {
			b.WriteString("\n\n" + board)
		}
	}
	return b.String()
}

// staleCaseEvent says case_climbed e no longer asks rung 2: its step ended
// since.
func (w *Waker) staleCaseEvent(ctx context.Context, e Event) (bool, error) {
	id, _ := toInt(e.Detail["case_id"])
	if id == 0 {
		return false, nil
	}
	steps, err := w.Store.CaseSteps(ctx, int64(id))
	if err != nil {
		return false, err
	}
	_, open := inProgress(steps, 2)
	return !open, nil
}

// askKey is the identity of row e, which hands a case to rung 2, on its
// mark: the case and the row, so a reprocessed batch wakes once and a case
// handed to rung 2 again (judgeAgain) wakes again.
func askKey(e Event) string {
	id, _ := toInt(e.Detail["case_id"])
	return fmt.Sprintf("case:%d@%d", id, e.ID)
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

// RoadmapPullStamp is the head of docs/roadmap.md repository's last pull
// turn read, and when it read it; false before the first pull.
func (s *PGStore) RoadmapPullStamp(ctx context.Context, repository string) (string, bool, error) {
	var head string
	var at time.Time
	err := s.db.QueryRowContext(ctx, `SELECT head, at FROM roadmap_pull WHERE repository = $1`, repository).
		Scan(&head, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return head, true, nil
}

// SetRoadmapPullStamp stamps repository's pull with the head its wake
// read, so the same roadmap never wakes the project twice.
func (s *PGStore) SetRoadmapPullStamp(ctx context.Context, repository, head string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO roadmap_pull (repository, head, at) VALUES ($1, $2, $3)
		ON CONFLICT (repository) DO UPDATE SET head = excluded.head, at = excluded.at`, repository, head, at)
	return err
}
