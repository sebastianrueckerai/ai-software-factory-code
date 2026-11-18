package factoryd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// SourceFreezeCard is the source of the human-lane card the freeze watcher
// files for a frozen model class.
const SourceFreezeCard = "provider_freeze"

// freezeResumeHold is how long a card whose class resumed on its own stays
// open, the held time on its face, before it closes itself.
const freezeResumeHold = 5 * time.Minute

// freezeResumedSentence is the resumed face's tell: the sentence a done
// freeze card carries only when its episode ended on a self-resume.
const freezeResumedSentence = "The class resumed on its own"

// freezeResumedFace is the card's face once its class resumed on its
// own: the held time and the close the face schedules.
const freezeResumedFace = "Class: %s\n\n" + freezeResumedSentence + " at %s: the freeze held %s. The card closes itself at %s."

// freezePressedSentence is the press-closed face's tell: the sentence a
// done freeze card carries when the retry press closed it.
const freezePressedSentence = "The retry press closed this card"

// freezePressedBody is the face of class's card the retry press by closed
// at at: the reset the hold named bounds the press's episode, so a dated
// refusal before it opens the card again and a limit after it files a new
// card; a face with no reset bounds nothing, and a new limit files anew.
func freezePressedBody(class, by string, at time.Time, reset string) string {
	body := fmt.Sprintf("Class: %s\n\n%s at %s: %s cleared the class's hold.", class, freezePressedSentence, stamp(at), by)
	if _, err := time.Parse(time.RFC3339, reset); err == nil {
		body += fmt.Sprintf("\n\nThe class resets at %s (the hold's reset): a dated refusal before it opens this card again, a limit after it files a new card.", reset)
	}
	return freezeCardTitle(class) + "\n\n" + body
}

// beforeReset reports whether at lies before reset, an RFC 3339 stamp: a
// moment inside the episode a press closed. No reset bounds no episode.
func beforeReset(at time.Time, reset string) bool {
	r, err := time.Parse(time.RFC3339, reset)
	return err == nil && at.Before(r)
}

// FreezeCardHolds reports whether t is a freeze card whose class's hold
// stands: open, and its face not the self-resume's, whose close is pending.
// Only such a card takes the retry press.
func FreezeCardHolds(t Task) bool {
	return t.Source == SourceFreezeCard && t.Status == "waiting-human" && !strings.Contains(t.Objective, freezeResumedSentence)
}

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
	// ListFrozenCards lists the running and queued cards whose classes
	// hold class, in every repository.
	ListFrozenCards(ctx context.Context, class string) ([]Task, error)
	CreateTask(ctx context.Context, t Task) error
	SetObjective(ctx context.Context, id, objective string) error
	// FreezeCards lists the freeze cards in status, every repository's.
	FreezeCards(ctx context.Context, status string) ([]Task, error)
	TransitionTask(ctx context.Context, id string, from []string, to string) error
	InsertIntakeEvent(context.Context, IntakeEvent) (int64, error)
	IntakeEvents(context.Context, string) ([]IntakeEvent, error)
	ConfirmFactoryClose(context.Context, string, int64) error
	// ForgetFactoryClose deletes the record of factoryd's close of task
	// taskID's issue: a reopened freeze card's next close records, and
	// says, its own.
	ForgetFactoryClose(ctx context.Context, taskID string, issue int64) error
	// Task reads a card: the one a retry press names, and the watcher's
	// own while it waits.
	Task(ctx context.Context, id string) (Task, bool, error)
}

// errNoPolicyReader refuses a freeze watcher wired without its policy
// reader, which would count no frozen card and wake none.
var errNoPolicyReader = errors.New("the freeze card watcher has no policy reader (FreezeCard.Current): it cannot tell which cards a class's hold freezes")

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
	// Current hands the policy version in force: its classes say which
	// cards a class's hold freezes, its waits.provider_limit bounds the
	// silence. Required: Check and the frozen cards' read refuse a watcher
	// without it (errNoPolicyReader).
	Current func() policy.Version
	// Timeline records the card's card_created; nil records none.
	Timeline *Timeline

	// mu serializes Check, on the watcher's loop, and CloseOnRetry, on a
	// press's goroutine.
	mu      sync.Mutex
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
	closed    bool // the retry press closed the card; an arm before reset reopens it
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
	if w.Current == nil {
		return errNoPolicyReader
	}
	w.mu.Lock()
	defer w.mu.Unlock()
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
// card open for ever, mute its one-time ask or re-arm it on the reset it
// holds: the class and the reset from the card's face, frozenAt from the
// row, the resume hold from the face's stamp ('The card closes itself at
// <stamp>'), and the ask from its face.
func (w *FreezeCard) rehydrate(ctx context.Context) error {
	open, err := w.Store.FreezeCards(ctx, "waiting-human")
	if err != nil {
		return err
	}
	for _, t := range open {
		class := freezeCardClass(t.Objective)
		if class == "" {
			continue
		}
		key := t.Repository + "/" + class
		if _, ok := w.classes[key]; ok {
			continue
		}
		st := &freezeClass{repo: t.Repository, class: class, taskID: t.ID, issue: t.IssueNumber, frozenAt: t.CreatedAt, reset: freezeFaceReset(t.Objective)}
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

// freezeFaceReset reads the reset a holding card's face names on its line
// 'The class resets at <stamp> (': the last such line, as the provider's
// words come before it on the face; "" on a face that names none.
func freezeFaceReset(objective string) string {
	reset := ""
	for _, line := range strings.Split(objective, "\n") {
		if rest, ok := strings.CutPrefix(line, "The class resets at "); ok {
			reset, _, _ = strings.Cut(rest, " ")
		}
	}
	if _, err := time.Parse(time.RFC3339, reset); err != nil {
		return ""
	}
	return reset
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

// event applies one gateway row to every repository's open card of the
// class, as the gateway's hold is the class's in all of them: an arm
// re-arms each, which cancels a pending close, reopens each card the
// press closed before its reset, and files the card of the arming
// repository when it has none; an end starts each open card's close.
func (w *FreezeCard) event(ctx context.Context, ev FreezeEvent) error {
	repos, err := w.openCardRepositories(ctx, ev.Class)
	if err != nil {
		return err
	}
	switch ev.Kind {
	case "class_frozen":
		pressed, err := w.pressClosedCards(ctx, ev.Class, ev.At)
		if err != nil {
			return err
		}
		for _, t := range pressed {
			if !slices.Contains(repos, t.Repository) {
				repos = append(repos, t.Repository)
			}
		}
		if !slices.Contains(repos, ev.Repository) {
			repos = append(repos, ev.Repository)
		}
		for _, repo := range repos {
			if err := w.frozen(ctx, w.class(repo, ev.Class), ev); err != nil {
				return err
			}
		}
	case "class_resumed":
		for _, repo := range repos {
			if err := w.resumed(ctx, w.class(repo, ev.Class), ev); err != nil {
				return err
			}
		}
	}
	return nil
}

// class is the watcher's memory of repository's card of class.
func (w *FreezeCard) class(repository, class string) *freezeClass {
	key := repository + "/" + class
	st := w.classes[key]
	if st == nil {
		st = &freezeClass{repo: repository, class: class}
		w.classes[key] = st
	}
	return st
}

// frozen files the class's card on a first arm and re-arms it on any
// other: a new reset, a card the press closed before the reset it held,
// or a resume's pending close, which an arm with the reset unchanged
// cancels unsaid, the resume being a recheck's lapse. An arm at or past
// the reset of the card the press closed opens a new episode and files a
// new card. Only a true duplicate, an arm with the reset an open, frozen
// card already holds, stays quiet.
func (w *FreezeCard) frozen(ctx context.Context, st *freezeClass, ev FreezeEvent) error {
	if st.closed && !beforeReset(ev.At, st.reset) {
		*st = freezeClass{repo: st.repo, class: st.class}
	}
	if st.taskID == "" {
		if t, ok, err := w.findCard(ctx, st); err != nil {
			return err
		} else if ok {
			st.taskID, st.issue, st.frozenAt, st.reset = t.ID, t.IssueNumber, t.CreatedAt, freezeFaceReset(t.Objective)
		} else if t, ok, err := w.findClosedCard(ctx, st, ev.At); err != nil {
			return err
		} else if ok {
			// the press closed the card and a restart lost the memory:
			// adopt it as closed, so the re-arm below opens the same card
			st.taskID, st.issue, st.frozenAt, st.reset = t.ID, t.IssueNumber, t.CreatedAt, freezeFaceReset(t.Objective)
			st.closed = true
		}
	}
	if st.reset == ev.Reset && st.taskID != "" && !st.closed && st.resumedAt.IsZero() {
		return nil
	}
	cards, err := w.classCards(ctx, st.class)
	if err != nil {
		return err
	}
	count := 0
	for _, c := range cards {
		if c.Repository == st.repo {
			count++
		}
	}
	body := freezeCardBody(ev, count, w.now())
	if st.closed {
		// the press closed the card; a first dated refusal re-arms the
		// hold and updates the same card — no second card, no ask — whose
		// next close is its own
		if err := w.Store.ForgetFactoryClose(ctx, st.taskID, st.issue); err != nil {
			return err
		}
		if err := w.Store.SetObjective(ctx, st.taskID, freezeCardTitle(st.class)+"\n\n"+body); err != nil {
			return err
		}
		if err := w.Store.TransitionTask(ctx, st.taskID, []string{"done"}, "waiting-human"); err != nil {
			return err
		}
		repo, err := SplitRepo(st.repo)
		if err != nil {
			return err
		}
		if err := w.Forgejo.ReopenIssue(ctx, repo.Owner, repo.Name, st.issue); err != nil {
			return err
		}
		if _, err := w.Forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, st.issue,
			fmt.Sprintf("factoryd: the hold re-armed after the retry, to the reset %s. %d cards of the class wait.", ev.Reset, count)); err != nil {
			log.Printf("freeze card: comment the re-arm on issue #%d: %v", st.issue, err)
		}
		st.closed = false
		st.reset = ev.Reset
		st.frozenAt = w.now()
		st.resumedAt, st.spoke = time.Time{}, false
		log.Printf("freeze card: the hold re-armed after the retry, %s's card #%d holds again", st.class, st.issue)
		return nil
	}
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
	if st.reset == ev.Reset {
		// a recheck's lapse resumed the class and its probe met the reset
		// the card holds: the frozen face comes back, the close is off
		if st.spoke {
			body += "\n\n" + freezeAsk(st.class, w.providerLimit(st.repo))
		}
		if err := w.Store.SetObjective(ctx, st.taskID, freezeCardTitle(st.class)+"\n\n"+body); err != nil {
			return err
		}
		st.resumedAt = time.Time{}
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
		fmt.Sprintf("factoryd: the hold re-armed, to the reset %s. %d cards of the class wait.", ev.Reset, count)); err != nil {
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
// the held time on its face. The hold runs from the row's own time, so
// every repository's card and a retry after a failed write read the same
// moment, and it starts only once the face is written.
func (w *FreezeCard) resumed(ctx context.Context, st *freezeClass, ev FreezeEvent) error {
	if st.closed {
		return nil // the press closed it; success keeps it closed
	}
	if st.taskID == "" {
		t, ok, err := w.findCard(ctx, st)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		st.taskID, st.issue, st.frozenAt = t.ID, t.IssueNumber, t.CreatedAt
	}
	if !st.resumedAt.IsZero() {
		return nil
	}
	body := freezeCardTitle(st.class) + "\n\n" + fmt.Sprintf(
		freezeResumedFace, st.class, stamp(ev.At), freezeHeld(st.frozenAt, ev.At), stamp(ev.At.Add(freezeResumeHold)))
	if err := w.Store.SetObjective(ctx, st.taskID, body); err != nil {
		return err
	}
	st.resumedAt = ev.At
	log.Printf("freeze card: the %s class resumed, its card #%d closes at %s", st.class, st.issue, stamp(st.resumedAt.Add(freezeResumeHold)))
	return nil
}

// CloseOnRetry closes the class's open freeze card at once, no
// five-minute grace, its issue naming by, who pressed: the retry press —
// the chat action and the card button — calls it after it clears the
// hold. It matches the class's own card only, by the whole-line Class
// marker, and is idempotent when no open card stands. The issue closes
// first, as close does, then the row takes the press-closed face, with
// the reset the hold named, and closes: a close that fails part way
// leaves the card open, to close five minutes after its class resumed.
// The class's memory keeps the closed card, so a first dated refusal
// before that reset re-arms the hold and updates the same card, while a
// first success keeps it closed on class_resumed; a restart that loses
// the memory adopts the done card by its face (findClosedCard).
func (w *FreezeCard) CloseOnRetry(ctx context.Context, repository, class, by string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.now == nil {
		w.now = time.Now
	}
	open, err := openFreezeCards(ctx, w.Store, class)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(open, func(t Task) bool { return t.Repository == repository })
	if i < 0 {
		return nil
	}
	t, st := open[i], w.classes[repository+"/"+class]
	reset := freezeFaceReset(t.Objective)
	if reset == "" && st != nil {
		reset = st.reset // a resume rewrote the face before the press
	}
	msg := fmt.Sprintf("%s's retry press cleared the %s class's hold: the freeze card closes at once.", by, class)
	if err := closeIssueAsFactory(ctx, w.Store, w.Forgejo, t, "factoryd: "+msg); err != nil {
		return err
	}
	if err := w.Store.SetObjective(ctx, t.ID, freezePressedBody(class, by, w.now(), reset)); err != nil {
		return err
	}
	if err := w.Store.TransitionTask(ctx, t.ID, []string{"waiting-human"}, "done"); err != nil {
		return err
	}
	if st != nil {
		st.taskID, st.issue, st.reset, st.closed = t.ID, t.IssueNumber, reset, true
	}
	log.Printf("freeze card: %s's retry press closed %s's card #%d at once", by, class, t.IssueNumber)
	return nil
}

// openCardRepositories lists the repositories that hold an open freeze
// card of class, each once.
func (w *FreezeCard) openCardRepositories(ctx context.Context, class string) ([]string, error) {
	open, err := openFreezeCards(ctx, w.Store, class)
	if err != nil {
		return nil, err
	}
	var repos []string
	for _, t := range open {
		if !slices.Contains(repos, t.Repository) {
			repos = append(repos, t.Repository)
		}
	}
	return repos, nil
}

// openFreezeCards lists the open freeze cards of class, every
// repository's.
func openFreezeCards(ctx context.Context, s interface {
	FreezeCards(ctx context.Context, status string) ([]Task, error)
}, class string) ([]Task, error) {
	open, err := s.FreezeCards(ctx, "waiting-human")
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(open, func(t Task) bool { return !objectiveHasClassLine(t.Objective, class) }), nil
}

// pressClosedCards lists the freeze cards of class the retry press closed
// whose reset lies after at, every repository's: an arm at at reopens
// them.
func (w *FreezeCard) pressClosedCards(ctx context.Context, class string, at time.Time) ([]Task, error) {
	done, err := w.Store.FreezeCards(ctx, "done")
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(done, func(t Task) bool {
		return !objectiveHasClassLine(t.Objective, class) || !strings.Contains(t.Objective, freezePressedSentence) ||
			!beforeReset(at, freezeFaceReset(t.Objective))
	}), nil
}

// classCards lists the cards the class's hold freezes, in every
// repository: the running and queued cards the policy in force holds
// under class, by the gateway's own rule (policy.Document.ProviderLimitOf).
func (w *FreezeCard) classCards(ctx context.Context, class string) ([]Task, error) {
	if w.Current == nil {
		return nil, errNoPolicyReader
	}
	cards, err := w.Store.ListFrozenCards(ctx, class)
	if err != nil {
		return nil, err
	}
	doc := w.Current().Document
	return slices.DeleteFunc(cards, func(t Task) bool {
		held, _ := doc.ProviderLimitOf(t.RequiredCapabilities)
		return held != class
	}), nil
}

// objectiveHasClassLine answers whether objective carries the class's
// marker as a whole line, so a close matches the class's own card only —
// the big class's press does not close bigv2's card.
func objectiveHasClassLine(objective, class string) bool {
	want := strings.TrimSuffix(freezeClassMarker(class), "\n")
	for _, line := range strings.Split(objective, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// tick runs the classes' clocks: a card past its resume hold closes
// itself; a card whose freeze outgrew waits.provider_limit speaks once.
func (w *FreezeCard) tick(ctx context.Context) error {
	now := w.now()
	for key, st := range w.classes {
		if st.taskID == "" || st.closed {
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
// the version in force names none.
func (w *FreezeCard) providerLimit(repository string) time.Duration {
	if d := w.Current().Tree.WaitsOf(repository).ProviderLimit; d > 0 {
		return d
	}
	return policy.DefaultWaits.ProviderLimit
}

// freezeAsk is the freeze card's one ask, once the freeze of class held
// past limit, waits.provider_limit.
func freezeAsk(class string, limit time.Duration) string {
	return fmt.Sprintf("The freeze of the %s class has held past waits.provider_limit (%s). "+
		"Add a fallback for the class, switch the class's model in policy, or keep waiting. The work cards ask nothing.",
		class, limit)
}

// speak makes the freeze card ask once: add a fallback, switch the model,
// keep waiting. The work cards ask nothing.
func (w *FreezeCard) speak(ctx context.Context, st *freezeClass, limit time.Duration) error {
	ask := freezeAsk(st.class, limit)
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
	t, ok, err := w.Store.Task(ctx, id)
	return t, ok && t.Status == "waiting-human", err
}

// findCard matches the class's open freeze card of the repository, by the
// whole-line Class marker on its face, so a restart adopts the card it
// filed before.
func (w *FreezeCard) findCard(ctx context.Context, st *freezeClass) (Task, bool, error) {
	open, err := openFreezeCards(ctx, w.Store, st.class)
	if err != nil {
		return Task{}, false, err
	}
	i := slices.IndexFunc(open, func(t Task) bool { return t.Repository == st.repo })
	if i < 0 {
		return Task{}, false, nil
	}
	return open[i], true, nil
}

// findClosedCard matches the class's done freeze card of the repository
// the retry press closed, whose reset lies after at (pressClosedCards), so
// a restart between the press and a first dated refusal adopts it as
// closed and the refusal re-arms the same card, no second card. A card
// that ended on a self-resume, or whose reset passed, ended its episode: a
// new hold files its own card.
func (w *FreezeCard) findClosedCard(ctx context.Context, st *freezeClass, at time.Time) (Task, bool, error) {
	pressed, err := w.pressClosedCards(ctx, st.class, at)
	if err != nil {
		return Task{}, false, err
	}
	i := slices.IndexFunc(pressed, func(t Task) bool { return t.Repository == st.repo })
	if i < 0 {
		return Task{}, false, nil
	}
	return pressed[i], true, nil
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

// ListFrozenCards lists the running and queued cards whose classes hold
// class, in every repository, in id order: the cards that hold a work slot
// or wait for one.
func (s *PGStore) ListFrozenCards(ctx context.Context, class string) ([]Task, error) {
	return s.tasks(ctx, `SELECT `+taskColumns+` FROM tasks
		WHERE status IN ('running', 'queued') AND required_capabilities @> jsonb_build_array($1::text) ORDER BY id`, class)
}

// FreezeCards lists the freeze cards (source provider_freeze) in status,
// every repository's, in id order.
func (s *PGStore) FreezeCards(ctx context.Context, status string) ([]Task, error) {
	return s.tasks(ctx, `SELECT `+taskColumns+` FROM tasks WHERE source = $1 AND status = $2 ORDER BY id`, SourceFreezeCard, status)
}

// ForgetFactoryClose deletes the record of factoryd's close of task
// taskID's issue, confirmed or not: the freeze watcher's reopen of a card
// the press closed, whose next close records and posts its own.
func (s *PGStore) ForgetFactoryClose(ctx context.Context, taskID string, issue int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM intake_events WHERE task_id = $1 AND kind = $2 AND comment_id = $3`, taskID, IntakeFactoryClosed, issue)
	return err
}
