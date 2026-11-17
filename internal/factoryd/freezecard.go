package factoryd

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// SourceFreezeCard is the source of the human-lane card the freeze watcher
// files for a frozen model class.
const SourceFreezeCard = "provider_freeze"

// freezeResumeHold is how long a card whose class resumed on its own stays
// open, the held time on its face, before it closes itself.
const freezeResumeHold = 5 * time.Minute

// freezeClassMarker opens the line of a freeze card's objective that names
// its class, the line that matches a later event of the class to the
// class's open card.
func freezeClassMarker(class string) string { return "Class: " + class + "\n" }

// freezeCardTitle is the stable title of a class's freeze card.
func freezeCardTitle(class string) string {
	return "The provider limit freezes the " + class + " class"
}

// FreezeEvent is one class_frozen or class_resumed row the gateway wrote
// on the shared events table (gateway.EventRow, actor gateway).
type FreezeEvent struct {
	ID         int64
	Repository string
	TaskID     string
	At         time.Time
	Kind       string // "class_frozen" on an arm, "class_resumed" on an end
	Class      string
	Reset      string // the answered reset, RFC 3339
	Words      string // the provider's own words
}

// FreezeCardStore is the store the freeze watcher reads and writes.
type FreezeCardStore interface {
	humanCardStore
	// FreezeEvents reads the gateway's class_frozen and class_resumed
	// rows after the stored cursor, oldest first, and returns the
	// newest row's id as the cursor to persist.
	FreezeEvents(ctx context.Context) ([]FreezeEvent, int64, error)
	// SetFreezeCursor persists the watcher's cursor (the migration's
	// freeze_cursor table), so a restart reads forward only.
	SetFreezeCursor(ctx context.Context, cursor int64) error
	// FrozenCards counts the class's waiting cards for the face.
	FrozenCards(ctx context.Context, repository, class string) (int, error)
	CreateTask(ctx context.Context, t Task) error
	SetObjective(ctx context.Context, id, objective string) error
	ListTasksByStatus(ctx context.Context, status string) ([]Task, error)
	TransitionTask(ctx context.Context, id string, from []string, to string) error
	InsertIntakeEvent(context.Context, IntakeEvent) (int64, error)
	IntakeEvents(context.Context, string) ([]IntakeEvent, error)
	ConfirmFactoryClose(context.Context, string, int64) error
}

// FreezeCard is the watcher in the month alarm's cap-card shape: the
// first class_frozen for a class files ONE card naming the provider's
// words, the class, the reset with its countdown and how many cards wait;
// it holds no slot and asks nothing. A re-arm (a second class_frozen with
// a later reset) updates the same card, no second card. A self-resume
// (class_resumed) leaves it five minutes, the held time on its face, then
// it closes itself. Past waits.provider_limit it alone speaks once: add a
// fallback, switch the model, keep waiting; the work cards ask nothing.
type FreezeCard struct {
	Store   FreezeCardStore
	Forgejo *Forgejo
	// Current hands the policy version in force, whose waits.provider_limit
	// bounds the silence; nil reads the defaults.
	Current func() policy.Version
	// Timeline records the card's card_created; nil records none.
	Timeline *Timeline

	now     func() time.Time
	cursor  int64
	classes map[string]*freezeClass
}

// freezeClass is the watcher's memory of one class's card across passes.
type freezeClass struct {
	repo      string
	class     string
	taskID    string
	issue     int64
	reset     string
	frozenAt  time.Time
	resumedAt time.Time
	spoke     bool
}

// Loop checks every interval until ctx ends.
func (w *FreezeCard) Loop(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := w.Check(ctx); err != nil {
				log.Printf("freeze card: %v", err)
			}
		}
	}
}

// Check reads the gateway's freeze events past the cursor and runs the
// classes' clocks: the five-minute resume hold and the provider-limit
// bound.
func (w *FreezeCard) Check(ctx context.Context) error {
	if w.classes == nil {
		w.classes = map[string]*freezeClass{}
	}
	if w.now == nil {
		w.now = time.Now
	}
	if err := w.rehydrate(ctx); err != nil {
		return err
	}
	events, cursor, err := w.Store.FreezeEvents(ctx)
	if err != nil {
		return err
	}
	for _, ev := range events {
		if err := w.event(ctx, ev); err != nil {
			return err
		}
	}
	if cursor != w.cursor {
		if err := w.Store.SetFreezeCursor(ctx, cursor); err != nil {
			return err
		}
		w.cursor = cursor
	}
	return w.tick(ctx)
}

// rehydrate adopts the waiting freeze cards a previous watcher filed, so
// a restart between a persisted cursor and a clock does not leave the
// card open for ever or mute its one-time ask: the class from the card's
// face, frozenAt from the row, the resume hold from the face's stamp
// ('The card closes itself at <stamp>'), and the ask from its face.
func (w *FreezeCard) rehydrate(ctx context.Context) error {
	open, err := w.Store.ListTasksByStatus(ctx, "waiting-human")
	if err != nil {
		return err
	}
	for _, t := range open {
		if t.Source != SourceFreezeCard {
			continue
		}
		class := freezeCardClass(t.Objective)
		if class == "" {
			continue
		}
		key := t.Repository + "/" + class
		if _, ok := w.classes[key]; ok {
			continue
		}
		st := &freezeClass{repo: t.Repository, class: class, taskID: t.ID, issue: t.IssueNumber, frozenAt: t.CreatedAt}
		if at, ok := freezeResumeStamp(t.Objective); ok {
			st.resumedAt = at.Add(-freezeResumeHold)
		}
		st.spoke = strings.Contains(t.Objective, "waits.provider_limit")
		w.classes[key] = st
	}
	return nil
}

// freezeCardClass reads the class line of a freeze card's face: the whole
// "Class: <class>" line, never a prefix of a longer class's.
func freezeCardClass(objective string) string {
	for _, line := range strings.Split(objective, "\n") {
		if class, ok := strings.CutPrefix(line, "Class: "); ok {
			return class
		}
	}
	return ""
}

// freezeResumeStamp reads the closes-itself stamp of a resumed card's
// face; ok is false while the freeze card still holds.
func freezeResumeStamp(objective string) (time.Time, bool) {
	_, after, ok := strings.Cut(objective, "The card closes itself at ")
	if !ok {
		return time.Time{}, false
	}
	stamp, _, _ := strings.Cut(after, ".")
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// event applies one gateway row to its class's state.
func (w *FreezeCard) event(ctx context.Context, ev FreezeEvent) error {
	key := ev.Repository + "/" + ev.Class
	st := w.classes[key]
	if st == nil {
		st = &freezeClass{repo: ev.Repository, class: ev.Class}
		w.classes[key] = st
	}
	switch ev.Kind {
	case "class_frozen":
		return w.frozen(ctx, st, ev)
	case "class_resumed":
		return w.resumed(ctx, st, ev)
	}
	return nil
}

// frozen files the class's card on a first arm and updates it on a re-arm
// with a later reset; an unchanged arm stays quiet.
func (w *FreezeCard) frozen(ctx context.Context, st *freezeClass, ev FreezeEvent) error {
	if st.taskID == "" {
		if t, issue, ok, err := w.findCard(ctx, st); err != nil {
			return err
		} else if ok {
			st.taskID, st.issue, st.frozenAt = t.ID, issue, t.CreatedAt
		}
	}
	if st.reset == ev.Reset && st.taskID != "" {
		return nil
	}
	count, err := w.Store.FrozenCards(ctx, st.repo, st.class)
	if err != nil {
		return err
	}
	body := freezeCardBody(ev, count, w.now())
	if st.taskID == "" {
		t, err := w.file(ctx, st, ev, body)
		if err != nil {
			return err
		}
		st.taskID, st.issue, st.frozenAt, st.reset = t.ID, t.IssueNumber, w.now(), ev.Reset
		st.resumedAt, st.spoke = time.Time{}, false
		log.Printf("freeze card: %s filed #%d for the frozen %s class (%d cards wait)", st.repo, t.IssueNumber, st.class, count)
		return nil
	}
	// a re-arm: the same card, the new reset and count on its face
	if err := w.Store.SetObjective(ctx, st.taskID, freezeCardTitle(st.class)+"\n\n"+body); err != nil {
		return err
	}
	repo, err := SplitRepo(st.repo)
	if err != nil {
		return err
	}
	if _, err := w.Forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, st.issue,
		fmt.Sprintf("factoryd: the hold re-armed with a later reset (%s). %d cards of the class wait.", ev.Reset, count)); err != nil {
		log.Printf("freeze card: comment the re-arm on issue #%d: %v", st.issue, err)
	}
	st.reset = ev.Reset
	st.resumedAt, st.spoke = time.Time{}, false
	if st.frozenAt.IsZero() {
		st.frozenAt = w.now()
	}
	return nil
}

// resumed starts the card's five-minute close hold after a self-resume,
// the held time on its face. The retry press's at-once close is the
// freeze-close-on-retry card's item.
func (w *FreezeCard) resumed(ctx context.Context, st *freezeClass, ev FreezeEvent) error {
	if st.taskID == "" {
		t, issue, ok, err := w.findCard(ctx, st)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		st.taskID, st.issue, st.frozenAt = t.ID, issue, t.CreatedAt
	}
	if !st.resumedAt.IsZero() {
		return nil
	}
	st.resumedAt = w.now()
	held := freezeHeld(st.frozenAt, st.resumedAt)
	body := freezeCardTitle(st.class) + "\n\n" + fmt.Sprintf(
		"Class: %s\n\nThe class resumed on its own at %s: the freeze held %s. The card closes itself at %s.",
		st.class, stamp(st.resumedAt), held, stamp(st.resumedAt.Add(freezeResumeHold)))
	if err := w.Store.SetObjective(ctx, st.taskID, body); err != nil {
		return err
	}
	log.Printf("freeze card: the %s class resumed, its card #%d closes at %s", st.class, st.issue, stamp(st.resumedAt.Add(freezeResumeHold)))
	return nil
}

// tick runs the classes' clocks: a card past its resume hold closes
// itself; a card whose freeze outgrew waits.provider_limit speaks once.
func (w *FreezeCard) tick(ctx context.Context) error {
	now := w.now()
	for key, st := range w.classes {
		if st.taskID == "" {
			continue
		}
		switch {
		case !st.resumedAt.IsZero() && !now.Before(st.resumedAt.Add(freezeResumeHold)):
			if err := w.close(ctx, st); err != nil {
				return err
			}
			delete(w.classes, key)
		case st.resumedAt.IsZero() && !st.spoke && !st.frozenAt.IsZero():
			limit := w.providerLimit(st.repo)
			if now.Before(st.frozenAt.Add(limit)) {
				continue
			}
			if err := w.speak(ctx, st, limit); err != nil {
				return err
			}
			st.spoke = true
		}
	}
	return nil
}

// providerLimit is repository's waits.provider_limit, the default's when
// no version stands.
func (w *FreezeCard) providerLimit(repository string) time.Duration {
	if w.Current == nil {
		return policy.DefaultWaits.ProviderLimit
	}
	if d := w.Current().Tree.WaitsOf(repository).ProviderLimit; d > 0 {
		return d
	}
	return policy.DefaultWaits.ProviderLimit
}

// speak makes the freeze card ask once: add a fallback, switch the model,
// keep waiting. The work cards ask nothing.
func (w *FreezeCard) speak(ctx context.Context, st *freezeClass, limit time.Duration) error {
	ask := fmt.Sprintf("The freeze of the %s class has held past waits.provider_limit (%s). "+
		"Add a fallback for the class, switch the class's model in policy, or keep waiting. The work cards ask nothing.",
		st.class, limit)
	t, ok, err := w.waiting(ctx, st.taskID)
	if err != nil {
		return err
	}
	if ok {
		if err := w.Store.SetObjective(ctx, st.taskID, t.Objective+"\n\n"+ask); err != nil {
			return err
		}
	}
	repo, err := SplitRepo(st.repo)
	if err != nil {
		return err
	}
	if _, err := w.Forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, st.issue, "factoryd: "+ask); err != nil {
		return fmt.Errorf("comment the provider-limit ask on issue #%d: %w", st.issue, err)
	}
	log.Printf("freeze card: the %s class's freeze outgrew %s, its card #%d asked once", st.class, limit, st.issue)
	return nil
}

// close closes the card's issue and its row once its resume hold ran out.
func (w *FreezeCard) close(ctx context.Context, st *freezeClass) error {
	t, ok, err := w.waiting(ctx, st.taskID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	msg := fmt.Sprintf("The freeze of the %s class ended: the freeze held %s, and the card closes.", st.class, freezeHeld(st.frozenAt, st.resumedAt))
	if err := closeIssueAsFactory(ctx, w.Store, w.Forgejo, t, "factoryd: "+msg); err != nil {
		return err
	}
	if err := w.Store.TransitionTask(ctx, t.ID, []string{"waiting-human"}, "done"); err != nil {
		return err
	}
	log.Printf("freeze card: %s (#%d)", msg, t.IssueNumber)
	return nil
}

// waiting reads task id's row while it still waits on a human; ok is
// false once it moved.
func (w *FreezeCard) waiting(ctx context.Context, id string) (Task, bool, error) {
	open, err := w.Store.ListTasksByStatus(ctx, "waiting-human")
	if err != nil {
		return Task{}, false, err
	}
	for _, t := range open {
		if t.ID == id {
			return t, true, nil
		}
	}
	return Task{}, false, nil
}

// findCard matches the class's open freeze card, by source and the class
// marker line on its face, so a restart adopts the card it filed before.
func (w *FreezeCard) findCard(ctx context.Context, st *freezeClass) (Task, int64, bool, error) {
	open, err := w.Store.ListTasksByStatus(ctx, "waiting-human")
	if err != nil {
		return Task{}, 0, false, err
	}
	for _, t := range open {
		if t.Source == SourceFreezeCard && t.Repository == st.repo && strings.Contains(t.Objective, freezeClassMarker(st.class)) {
			return t, t.IssueNumber, true, nil
		}
	}
	return Task{}, 0, false, nil
}

// freezeCardBody is a freeze card's face: the provider's words, the
// class, the reset with its countdown, the waiting count, and what the
// card does and does not do.
func freezeCardBody(ev FreezeEvent, count int, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Class: %s\n\n", ev.Class)
	b.WriteString("The provider limit freezes the class: every call of the class is answered with the named refusal until the reset.\n\n")
	b.WriteString("The provider said:\n\n" + fenced(ev.Words) + "\n\n")
	if reset, err := time.Parse(time.RFC3339, ev.Reset); err == nil {
		fmt.Fprintf(&b, "The class resets at %s (%s). ", stamp(reset), freezeCountdown(reset, now))
	} else {
		fmt.Fprintf(&b, "The class resets at %s. ", ev.Reset)
	}
	fmt.Fprintf(&b, "%d cards of the class wait.\n\n", count)
	b.WriteString("This card holds no work slot and asks nothing. It updates on a re-arm, and closes itself five minutes after the class resumes.")
	return b.String()
}

// freezeCountdown is reset's distance from now, "in 4h 59m".
func freezeCountdown(reset, now time.Time) string {
	d := reset.Sub(now)
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("in %dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}

// freezeHeld is how long a freeze held, "2h 03m".
func freezeHeld(from, to time.Time) string {
	d := to.Sub(from)
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}

// file files the class's freeze card on the events' repository: the issue
// first, then its row, no ask row — the card asks nothing. An open issue
// of the title that no row holds is taken on, so a filing whose row the
// store refused leaves no stranded issue behind.
func (w *FreezeCard) file(ctx context.Context, st *freezeClass, ev FreezeEvent, body string) (Task, error) {
	repo, err := SplitRepo(ev.Repository)
	if err != nil {
		return Task{}, err
	}
	title := freezeCardTitle(st.class)
	reuse, err := strandedIssue(ctx, w.Store, w.Forgejo, repo, title)
	if err != nil {
		return Task{}, err
	}
	t := Task{ID: NewTaskID(), Kind: "human", Status: "waiting-human", RiskClass: "standard", Repository: ev.Repository,
		Source: SourceFreezeCard, Author: w.Forgejo.BotUser, Objective: title + "\n\n" + body}
	got, _, err := fileOnIssue(ctx, w.Forgejo, repo, title, body, []string{"human"}, t, reuse, func(t Task) (Task, error) {
		if err := w.Store.CreateTask(ctx, t); err != nil {
			return Task{}, err
		}
		return t, nil
	})
	if err != nil {
		return Task{}, err
	}
	w.Timeline.recordEvent(ctx, got.ID, EventCardCreated, map[string]any{"by": cmp.Or(got.Author, w.Forgejo.BotUser), "issue": got.IssueNumber})
	return got, nil
}

// The store reads the watcher needs, on the shared events table.

// FreezeEvents reads the gateway's class_frozen and class_resumed rows
// after the stored cursor, oldest first, and returns the newest row's id
// as the cursor to persist.
func (s *PGStore) FreezeEvents(ctx context.Context) ([]FreezeEvent, int64, error) {
	var stored int64
	if err := s.db.QueryRowContext(ctx, `SELECT cursor FROM freeze_cursor WHERE id = 1`).Scan(&stored); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, repository, COALESCE(task_id, ''), kind,
		COALESCE(detail->>'class', ''), COALESCE(detail->>'reset', ''), COALESCE(detail->>'words', '')
		FROM events WHERE id > $1 AND kind IN ('class_frozen', 'class_resumed') ORDER BY id`, stored)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []FreezeEvent
	var newest int64
	for rows.Next() {
		ev := FreezeEvent{}
		if err := rows.Scan(&ev.ID, &ev.At, &ev.Repository, &ev.TaskID, &ev.Kind, &ev.Class, &ev.Reset, &ev.Words); err != nil {
			return nil, 0, err
		}
		out = append(out, ev)
		newest = ev.ID
	}
	return out, max(newest, stored), rows.Err()
}

// SetFreezeCursor persists the watcher's cursor: the id of the last
// events row it read.
func (s *PGStore) SetFreezeCursor(ctx context.Context, cursor int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE freeze_cursor SET cursor = $1 WHERE id = 1`, cursor)
	return err
}

// FrozenCards counts the class's waiting cards for the face: the cards of
// the repository whose model alias is the class and that hold a work slot
// or wait for one, the cards the freeze covers.
func (s *PGStore) FrozenCards(ctx context.Context, repository, class string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM tasks
		WHERE repository = $1 AND model_alias = $2 AND status IN ('running', 'waiting', 'queued')`, repository, class).Scan(&n)
	return n, err
}
