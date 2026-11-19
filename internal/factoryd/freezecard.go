package factoryd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
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
const freezeResumedFace = "Class: %s\n\n" + freezeResumedSentence + " at %s: the freeze held %s. The card closes itself at %s.\n\n" + freezeStarted + "%s.\n" + freezeResetWas + "%s."

// freezeStarted opens the line of a face that names when the card's
// freeze started: the clock of the card's ask, which each pass reads back.
const freezeStarted = "The freeze started at "

// freezeResetWas opens the line of a resumed face that names the reset
// the card's hold had, which an arm inside the close's five minutes is
// measured against.
const freezeResetWas = "The hold's reset was "

// freezeRefusedReset opens the part of a given-up face that keeps the
// reset and the start of a freeze that still stands.
const freezeRefusedReset = "\n\nThe class resets at "

// freezeRefusedSentence is the given-up face's tell.
const freezeRefusedSentence = "Forgejo refused this card's issue"

// freezeCardStart is when the freeze of card t started: the stamp its face
// names, on the last such line, as the provider's words come before it;
// the card's filing on a face that names none.
func freezeCardStart(t Task) time.Time {
	start := t.CreatedAt
	for _, line := range strings.Split(t.Objective, "\n") {
		if rest, ok := strings.CutPrefix(line, freezeStarted); ok {
			if at, err := time.Parse(time.RFC3339, strings.TrimSuffix(rest, ".")); err == nil {
				start = at
			}
		}
	}
	return start
}

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
	// StandingFreezes lists the freezes that stand at the stored cursor:
	// one event for each repository that armed a class since the class's
	// last class_resumed, with that repository and with the id, time,
	// reset and words of the class's newest class_frozen.
	StandingFreezes(ctx context.Context) ([]FreezeEvent, error)
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
	// EndFreezeCard ends waiting card t in one transaction: it records
	// the close of t's issue with comment as its body (IntakeFactoryClosed)
	// unless comment is "", writes face unless it is "", moves the row to
	// done and deletes an owed reopen of the card. A row that no longer
	// waits, or a close record that stands on the issue already (a close
	// in flight that is not the watcher's: a human's through the console),
	// writes nothing and answers moved false.
	EndFreezeCard(ctx context.Context, t Task, face, comment string) (moved bool, err error)
	// ReopenFreezeCard opens done card t again in one transaction: it
	// moves the row to waiting-human, writes face, takes the close's
	// record off the issue, confirmed, where it stays as the card's
	// earlier end (countHumanEnd), and records the owed reopen of the
	// issue with comment as its body (IntakeFactoryReopened), which it
	// answers. A close Forgejo did not confirm puts its own comment first
	// in that body (freezeOwedSplit). A row that is not done changes
	// nothing and is an error.
	ReopenFreezeCard(ctx context.Context, t Task, face, comment string) (owed string, err error)
	// OwedFreezeReopens lists the owed reopens, oldest first, and
	// SettleFreezeReopen deletes the one of task taskID's issue.
	OwedFreezeReopens(ctx context.Context) ([]IntakeEvent, error)
	SettleFreezeReopen(ctx context.Context, taskID string, issue int64) error
	// UnconfirmedFreezeCloses lists the closes of freeze cards that stand
	// on their issues unconfirmed for olderThan, oldest first: the closes
	// Forgejo did not take yet.
	UnconfirmedFreezeCloses(ctx context.Context, olderThan time.Duration) ([]IntakeEvent, error)
	// HeldResumes lists the class_resumed rows that are the newest row of
	// their class at the stored cursor and whose wake is not recorded,
	// oldest first; RecordFreezeWake records the wake of one.
	HeldResumes(ctx context.Context) ([]FreezeEvent, error)
	RecordFreezeWake(ctx context.Context, resumed FreezeEvent) error
	InsertIntakeEvent(context.Context, IntakeEvent) (int64, error)
	IntakeEvents(context.Context, string) ([]IntakeEvent, error)
	ConfirmFactoryClose(context.Context, string, int64) error
	// Task reads a card: the one a retry press names, and the watcher's
	// own while it waits.
	Task(ctx context.Context, id string) (Task, bool, error)
	// projectLookup labels a card's counted end.
	projectLookup
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
//
// A card's close and its reopen are each one store transaction
// (EndFreezeCard, ReopenFreezeCard), so a failed write changes nothing,
// and Forgejo follows the store: the close's record and the owed reopen
// say what Forgejo still has to take, and each pass makes those calls
// again until Forgejo took them (closeOwed, reopenOwed). The wake of a
// class's frozen cards is due by the gateway's rows and made until it is
// recorded (wakeHeld). A human's close of a card wins over the watcher's
// reopen and its close. Each pass builds its view of the cards from their
// rows, so a watcher built anew passes as the old one would.
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
	// Signal delivers the provider_retry signal to a task's workflow: the
	// retry press's wake, and a self-resume's that held. Nil wakes none.
	Signal func(ctx context.Context, taskID string) error
	// Metrics counts a freeze card's end (factory.task.outcome) when the
	// watcher moves its row; nil counts none.
	Metrics *telemetry.Metrics

	// mu serializes Check, on the watcher's loop, and CloseOnRetry, on a
	// press's goroutine.
	mu  sync.Mutex
	now func() time.Time
	// classes is one pass's view of the cards, built from the rows at its
	// start: nothing in it outlives the pass.
	classes map[string]*freezeClass
	// standing and held are the store's StandingFreezes and HeldResumes at
	// the stored cursor, read again whenever the cursor moves; stood tells
	// that they were read. A recorded wake leaves held.
	standing, held []FreezeEvent
	stood          bool
	// refused is, by repository and class, the standing freeze whose
	// filing Forgejo refused for good: only a newer arm files again.
	refused map[string]int64
}

// IntakeFactoryReopened records that factoryd owes Forgejo the reopen of a
// freeze card's issue, the issue's number as its comment id and the
// re-arm's comment as its body (ReopenFreezeCard). When Forgejo did not
// confirm the close before it, the body holds that close's comment first,
// then freezeOwedSplit, then the re-arm's. The row is deleted once Forgejo
// took all of it (SettleFreezeReopen).
const IntakeFactoryReopened = "factory_reopened"

// freezeOwedSplit parts the two comments of an owed reopen's body.
const freezeOwedSplit = "\n\x1f\n"

// EventClassWoken is the watcher's record, on the events table, that it
// woke a class's frozen cards for the class_resumed row its detail names
// (resumed, the row's id).
const EventClassWoken = "class_woken"

// freezeClass is a pass's view of one class's card in one repository.
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

// Check is one pass. It builds its view of the cards from their rows,
// makes the owed reopens and the unconfirmed closes again, applies the
// gateway's freeze events past the cursor, applies the standing freezes
// again, wakes the frozen cards of each class whose resume held, and runs
// the classes' clocks: the five-minute resume hold and the provider-limit
// bound. A watcher built anew passes the same way: what a pass needs is in
// the rows.
//
// The cursor moves past a row whose effect is written, and past an arm
// that did not finish, whatever stopped it: each pass makes that arm
// again while its freeze stands (stand), and once more when the pass
// comes to the class's resume, before it applies the resume. An arm that
// fails then too is over with its freeze, and the wake of its frozen
// cards comes from the rows (wakeHeld). So no repository and no row holds
// another class's rows. The rest of the pass runs whatever a row did.
func (w *FreezeCard) Check(ctx context.Context) error {
	if w.Current == nil {
		return errNoPolicyReader
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.now == nil {
		w.now = time.Now
	}
	if w.refused == nil {
		w.refused = map[string]int64{}
	}
	w.classes = map[string]*freezeClass{}
	if err := w.rehydrate(ctx); err != nil {
		return err
	}
	if err := w.reopenOwed(ctx); err != nil {
		return err
	}
	if err := w.closeOwed(ctx); err != nil {
		return err
	}
	if err := w.readStanding(ctx, !w.stood); err != nil {
		return err
	}
	events, _, err := w.Store.FreezeEvents(ctx)
	if err != nil {
		return err
	}
	var stopped error
	applied := 0
	for i, ev := range events {
		if ev.Kind == "class_resumed" {
			// the freeze ends: what its arms did not finish is made once
			// more, so the resume meets the cards a run with no failure has
			stopped = w.stand(ctx, w.armsBefore(events[:i], ev.Class))
		}
		if stopped == nil {
			stopped = w.event(ctx, ev)
		}
		if stopped != nil {
			break
		}
		applied++
	}
	if applied > 0 {
		if err := w.Store.SetFreezeCursor(ctx, events[applied-1].ID); err != nil {
			return err
		}
	}
	if err := w.readStanding(ctx, applied > 0); err != nil {
		return err
	}
	return errors.Join(stopped, w.stand(ctx, w.standing), w.wakeHeld(ctx), w.tick(ctx))
}

// readStanding reads the standing freezes and the held resumes at the
// stored cursor when due: on a watcher's first pass and after the cursor
// moved.
func (w *FreezeCard) readStanding(ctx context.Context, due bool) (err error) {
	if !due {
		return nil
	}
	if w.standing, err = w.Store.StandingFreezes(ctx); err != nil {
		return err
	}
	if w.held, err = w.Store.HeldResumes(ctx); err != nil {
		return err
	}
	w.stood = true
	return nil
}

// armsBefore is what StandingFreezes would answer for class right before
// the row after batch, the rows of this pass read so far: one arm for each
// repository that armed the class since its last resume, with the newest
// arm's id, time, reset and words.
func (w *FreezeCard) armsBefore(batch []FreezeEvent, class string) []FreezeEvent {
	var arms []FreezeEvent
	for _, ev := range w.standing {
		if ev.Class == class {
			arms = append(arms, ev)
		}
	}
	for _, ev := range batch {
		switch {
		case ev.Class != class:
		case ev.Kind == "class_resumed":
			arms = nil
		case ev.Kind == "class_frozen":
			arms = append(arms, ev)
		}
	}
	var out []FreezeEvent
	for _, arm := range arms {
		if !slices.ContainsFunc(out, func(have FreezeEvent) bool { return have.Repository == arm.Repository }) {
			newest := arms[len(arms)-1]
			newest.Repository, newest.TaskID = arm.Repository, ""
			out = append(out, newest)
		}
	}
	return out
}

// stand applies again each of arms whose reset is ahead: the standing
// freezes in every pass, and a class's arms once more when its resume
// comes (Check). What an arm did not finish, this does. An open card that
// holds the freeze's reset stays quiet (frozen); a filing, a re-arm or a
// reopen that Forgejo or the store stopped is made again. A freeze the
// retry press answered since, by the time of its close's record after the
// arm's, is over, whether or not the gateway wrote its class_resumed: the
// press cleared the hold. A freeze whose card a human closed gets no card
// again before a new reset (endedOutside), and one whose filing Forgejo
// refused for good waits for a newer arm.
func (w *FreezeCard) stand(ctx context.Context, arms []FreezeEvent) error {
	answered := map[string]bool{}
	var errs []error
	for _, ev := range arms {
		if id, ok := w.refused[ev.Repository+"/"+ev.Class]; !beforeReset(w.now(), ev.Reset) || ok && id == ev.ID {
			continue
		}
		if _, read := answered[ev.Class]; !read {
			done, err := w.Store.FreezeCards(ctx, "done")
			if err != nil {
				return err
			}
			for _, t := range done {
				// a press that answered the arm ended its card after it
				if answered[ev.Class] || !objectiveHasClassLine(t.Objective, ev.Class) || t.FinishedAt == nil || !t.FinishedAt.After(ev.At) {
					continue
				}
				press, _, at, err := w.closedBy(ctx, t)
				if err != nil {
					return err
				}
				answered[ev.Class] = press && at.After(ev.At)
			}
		}
		if answered[ev.Class] {
			continue
		}
		if err := w.event(ctx, ev); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// wakeHeld wakes the frozen cards of each class whose resume held five
// minutes with no new class_frozen, in every repository: the probe that
// met the lapse was served, so the class works, while the cards would
// sleep to their resets. The wake is due by the gateway's rows alone,
// whoever closed the class's freeze card and whether one was filed, and
// it is recorded after it is made: a pass that failed before the record,
// or a watcher built anew, makes it again. A press's resume wakes too,
// five minutes after the press woke the cards, which costs a woken card
// nothing. A wake that fails for one card is logged; that card starts at
// its reset.
func (w *FreezeCard) wakeHeld(ctx context.Context) error {
	var errs []error
	var waits []FreezeEvent
	for _, ev := range w.held {
		if w.now().Before(ev.At.Add(freezeResumeHold)) {
			waits = append(waits, ev)
			continue
		}
		cards, err := w.classCards(ctx, ev.Class)
		if err == nil {
			woken, frozen, failed := w.wake(ctx, cards)
			log.Printf("freeze card: the %s class's resume held, %d of %d frozen cards woken", ev.Class, woken, frozen)
			if len(failed) > 0 {
				log.Printf("freeze card: wake the %s class's frozen cards: %v", ev.Class, errors.Join(failed...))
			}
			err = w.Store.RecordFreezeWake(ctx, ev)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("wake the %s class's frozen cards: %w", ev.Class, err))
			waits = append(waits, ev)
		}
	}
	w.held = waits
	return errors.Join(errs...)
}

// reopenOwed makes every owed reopen again: a card that still waits gets
// its issue opened and its comments (reopenIssue), a card that no longer
// waits is settled without a call. A card whose issue is gone for good is
// given up, and this pass files its replacement while the freeze stands
// (stand). Any other refusal leaves the reopen owed.
func (w *FreezeCard) reopenOwed(ctx context.Context) error {
	owed, err := w.Store.OwedFreezeReopens(ctx)
	if err != nil {
		return err
	}
	for _, e := range owed {
		t, ok, err := w.waiting(ctx, e.TaskID)
		if err != nil {
			return err
		}
		if !ok {
			if err := w.Store.SettleFreezeReopen(ctx, e.TaskID, e.CommentID); err != nil {
				return err
			}
			continue
		}
		t.IssueNumber = e.CommentID
		err = w.reopenIssue(ctx, t, e.Body)
		st := w.classes[t.Repository+"/"+freezeCardClass(t.Objective)]
		switch {
		case err == nil:
		case forgejoGone(err) && st != nil && st.taskID == t.ID:
			if _, err := w.giveUp(ctx, st, err); err != nil {
				return err
			}
		case errors.Is(err, ErrRefused) && st != nil:
			w.keep(st, err)
		default:
			log.Printf("freeze card: open issue #%d of %s again: %v: each pass tries it again", t.IssueNumber, t.Repository, maskedErr(err))
		}
	}
	return nil
}

// reopenIssue is the Forgejo side of a reopen the store holds as owed,
// body: the close before it gets its comment where Forgejo did not
// confirm that close, card t's issue opens again and takes the re-arm's
// comment, and the owed reopen is settled. Each comment posts where the
// issue lacks it, so each step is safe to make again. A human's close
// wins: when a close record stands on the issue, which since the reopen's
// own write only a human's close through the console leaves there, the
// reopen is settled with no call.
func (w *FreezeCard) reopenIssue(ctx context.Context, t Task, body string) error {
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return err
	}
	events, err := w.Store.IntakeEvents(ctx, t.ID)
	if err != nil {
		return err
	}
	if botCloseRecorded(events, t) {
		return w.Store.SettleFreezeReopen(ctx, t.ID, t.IssueNumber)
	}
	comment := body
	if closed, rearmed, ok := strings.Cut(body, freezeOwedSplit); ok {
		comment = rearmed
		said := 0
		for _, e := range events {
			if e.Kind == IntakeFactoryClosed && e.Body == closed {
				said++
			}
		}
		if err := w.Forgejo.CommentTimes(ctx, repo.Owner, repo.Name, t.IssueNumber, closed, said); err != nil {
			return err
		}
	}
	if err := w.Forgejo.ReopenIssue(ctx, repo.Owner, repo.Name, t.IssueNumber); err != nil {
		return err
	}
	if err := w.Forgejo.CommentOnce(ctx, repo.Owner, repo.Name, t.IssueNumber, comment); err != nil {
		return err
	}
	return w.Store.SettleFreezeReopen(ctx, t.ID, t.IssueNumber)
}

// closeOwed finishes the closes of freeze cards that Forgejo did not take:
// each close that stands on its issue unconfirmed for two minutes, the
// watcher's own and a human's through the console alike
// (healFactoryClose). It runs in the watcher's pass and not in the heal
// pass, so no reopen of the card runs beside it. Only an issue that is
// gone for good settles a close; a 401 or a 403 keeps it owed until a
// human fixes the bot's credential or its rights, and so does a Forgejo
// that does not answer.
func (w *FreezeCard) closeOwed(ctx context.Context) error {
	rows, err := w.Store.UnconfirmedFreezeCloses(ctx, closeHealAge)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := healFactoryClose(ctx, w.Store, w.Forgejo, row, forgejoGone); err != nil {
			log.Printf("freeze card: close issue #%d of task %s: %v: each pass tries it again", row.CommentID, row.TaskID, maskedErr(err))
		}
	}
	return nil
}

// rehydrate builds the pass's view of the waiting freeze cards from their
// faces: the class, the reset and the freeze's start (freezeCardStart),
// the resume hold from the face's stamp ('The card closes itself at
// <stamp>'), and the ask. A waiting card that carries a close record on
// its issue for two minutes is no card of the view: a human closed it and
// no webhook ended the row (closedByHuman).
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
		events, err := w.Store.IntakeEvents(ctx, t.ID)
		if err != nil {
			return err
		}
		if i := slices.IndexFunc(events, func(e IntakeEvent) bool { return e.Kind == IntakeFactoryClosed && e.CommentID == t.IssueNumber }); i >= 0 &&
			!w.now().Before(events[i].CreatedAt.Add(closeHealAge)) {
			if err := w.closedByHuman(ctx, t, events[i].Body); err != nil {
				return err
			}
			continue
		}
		st := &freezeClass{repo: t.Repository, class: class, taskID: t.ID, issue: t.IssueNumber, frozenAt: freezeCardStart(t), reset: freezeFaceReset(t.Objective)}
		if at, ok := freezeResumeStamp(t.Objective); ok {
			st.resumedAt = at.Add(-freezeResumeHold)
		}
		st.spoke = strings.Contains(t.Objective, "waits.provider_limit")
		w.classes[t.Repository+"/"+class] = st
	}
	return nil
}

// closedByHuman ends waiting card t, on whose issue a close record
// stands. The watcher's own close moves the row in the write that records
// it, so this record is a human's through the console, and the human's
// close wins. Its webhook ends the row; when none did for two minutes, as
// Forgejo sends none for an issue that was closed already, the row ends
// here as it is. The end is counted, and the human's close is finished
// where it was cut short or a reopen opened the issue meanwhile: the
// issue takes the record's comment, said, where it lacks it, and closes
// (closeIssueAsFactory, which meets the record). A Forgejo that fails
// there leaves the close to closeOwed.
func (w *FreezeCard) closedByHuman(ctx context.Context, t Task, said string) error {
	moved, err := w.Store.EndFreezeCard(ctx, t, "", "")
	if err != nil || !moved {
		return err
	}
	countHumanEnd(ctx, w.Metrics, w.Store, t, "done")
	log.Printf("freeze card: a human closed %s's card #%d, the card ends", t.Repository, t.IssueNumber)
	if err := closeIssueAsFactory(ctx, w.Store, w.Forgejo, t, said); err != nil {
		log.Printf("freeze card: close issue #%d of %s, which a human closed: %v: a later pass closes it", t.IssueNumber, t.Repository, maskedErr(err))
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

// freezeFaceReset reads the reset a card's face names, on a holding
// face's line 'The class resets at <stamp> (' or a resumed face's 'The
// hold's reset was <stamp>.': the last such line, as the provider's words
// come before it on the face; "" on a face that names none.
func freezeFaceReset(objective string) string {
	reset := ""
	for _, line := range strings.Split(objective, "\n") {
		for _, prefix := range []string{"The class resets at ", freezeResetWas} {
			if rest, ok := strings.CutPrefix(line, prefix); ok {
				reset, _, _ = strings.Cut(rest, " ")
				reset = strings.TrimSuffix(reset, ".")
			}
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
// repository when it has none; an end starts each open card's close and
// drops the clock a given-up card kept for its replacement.
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
			if err := w.arm(ctx, w.class(repo, ev.Class), ev); err != nil {
				return err
			}
		}
	case "class_resumed":
		if err := w.endGivenUp(ctx, ev.Class); err != nil {
			return err
		}
		for _, repo := range repos {
			if err := w.resumed(ctx, w.class(repo, ev.Class), ev); err != nil {
				return err
			}
		}
	}
	return nil
}

// forgejoGone reports whether err is Forgejo's word that an issue is gone
// for good, whoever asks: a 404, a 410 or a 422.
func forgejoGone(err error) bool {
	return errors.Is(err, errForgejoNotFound) || errors.Is(err, errForgejoGone) || errors.Is(err, errForgejoUnprocessable)
}

// arm applies class_frozen ev to st's card (frozen) and decides what a
// failure does to it, so that one card does not stop the pass for every
// class. A card whose issue is gone for good is given up, and the arm in
// hand files its replacement at once. A filing Forgejo refuses for good
// waits for the class's next arm. Any other failure keeps the card and
// stops nothing: a 4xx (keep), a Forgejo that does not answer, a store's
// failed write or a row that names no repository. The arm is made again
// while its freeze stands (stand) and when its class resumes (Check).
// Only the store's failure to give a card up is returned.
func (w *FreezeCard) arm(ctx context.Context, st *freezeClass, ev FreezeEvent) error {
	err := w.frozen(ctx, st, ev)
	switch {
	case err == nil:
		return nil
	case forgejoGone(err) && st.taskID == "":
		w.refused[st.repo+"/"+st.class] = ev.ID
		action := "a human restores the repository or the bot's access to it, and the class's next arm files the card"
		if errors.Is(err, errForgejoUnprocessable) {
			action = "Forgejo cannot process the filing as factoryd sends it, which the answer above explains, and the class's next arm tries it again"
		}
		log.Printf("freeze card: Forgejo refuses for good to file the %s class's freeze card on %s (%s): %s", st.class, st.repo, textutil.Truncate(maskedErr(err), 300), action)
		return nil
	case forgejoGone(err):
		if _, err := w.giveUp(ctx, st, err); err != nil {
			return err
		}
		return w.arm(ctx, w.class(st.repo, st.class), ev)
	case errors.Is(err, ErrRefused):
		w.keep(st, err)
		return nil
	}
	log.Printf("freeze card: %s's %s card%s: %v: the arm is made again while the freeze stands", st.repo, st.class, issueRef(st.issue), maskedErr(err))
	return nil
}

// issueRef is " #n" for an issue, "" for none.
func issueRef(issue int64) string {
	if issue == 0 {
		return ""
	}
	return fmt.Sprintf(" #%d", issue)
}

// giveUp ends st's card, whose issue Forgejo refuses for good: the
// refusal is logged and recorded on the card, its face says so and no
// longer names the press, so no arm adopts it again, and the pass forgets
// it. The card ends in one write, with no comment, as its issue is gone.
// It answers the provider's words of the face it replaced, which a
// replacement carries on.
func (w *FreezeCard) giveUp(ctx context.Context, st *freezeClass, cause error) (string, error) {
	why := textutil.Truncate(maskedErr(cause), 300)
	log.Printf("freeze card: Forgejo refuses %s's %s card%s for good (%s): the card ends, and a new one speaks for the class while it is frozen", st.repo, st.class, issueRef(st.issue), why)
	t, ok, err := w.Store.Task(ctx, st.taskID)
	if err != nil {
		return "", err
	}
	words := freezeFaceWords(t.Objective)
	if ok {
		moved, err := w.end(ctx, t, freezeRefusedBody(st, why), "")
		if err != nil {
			return "", err
		}
		if moved {
			w.Timeline.RecordRow(ctx, t.ID, EventFreezeIssueRefused, FactoryDecider, ViaFactoryd, map[string]any{"class": st.class, "issue": st.issue, "error": why})
		}
	}
	delete(w.classes, st.repo+"/"+st.class)
	return words, nil
}

// end ends waiting freeze card t in the store's one write (EndFreezeCard),
// under face unless it is "", and reports whether this call moved the
// row. Then it counts the end, and Forgejo follows: the issue takes
// comment, closes, and the close's record is confirmed
// (closeIssueAsFactory, which meets the record). A Forgejo that fails
// there is logged, and a later pass, which meets the record too, posts
// its body where the issue lacks this close's comment and closes the
// issue (closeOwed). No comment closes no issue: the issue is gone.
func (w *FreezeCard) end(ctx context.Context, t Task, face, comment string) (bool, error) {
	moved, err := w.Store.EndFreezeCard(ctx, t, face, comment)
	if err != nil || !moved {
		return false, err
	}
	countHumanEnd(ctx, w.Metrics, w.Store, t, "done")
	if comment == "" {
		return true, nil
	}
	if err := closeIssueAsFactory(ctx, w.Store, w.Forgejo, t, comment); err != nil {
		log.Printf("freeze card: close issue #%d of %s: %v: a later pass closes it", t.IssueNumber, t.Repository, maskedErr(err))
	}
	return true, nil
}

// countHumanEnd counts human card t's end (countOutcome), each card once:
// a freeze card a re-arm opened again counted its end when the press
// closed it, which the close's record off its issue tells
// (ReopenFreezeCard), so its later end counts nothing.
func countHumanEnd(ctx context.Context, m *telemetry.Metrics, store interface {
	projectLookup
	taskReader
	IntakeEvents(context.Context, string) ([]IntakeEvent, error)
}, t Task, final string) {
	if t.Source == SourceFreezeCard {
		events, err := store.IntakeEvents(ctx, t.ID)
		if err != nil {
			log.Printf("metrics: the earlier ends of freeze card %s: %v", t.ID, err)
		}
		if slices.ContainsFunc(events, func(e IntakeEvent) bool { return e.Kind == IntakeFactoryClosed && e.CommentID == 0 }) {
			return
		}
	}
	countOutcome(ctx, m, store, t, final)
}

// freezeRefusedBody is the face of st's card whose issue Forgejo refused
// for good, why. While the class is frozen it keeps the reset and the
// start of its freeze, which the card filed in its place takes on.
func freezeRefusedBody(st *freezeClass, why string) string {
	body := fmt.Sprintf("Class: %s\n\n%s #%d for good (%s), so the card ended: a new card speaks for the class while it is frozen.", st.class, freezeRefusedSentence, st.issue, why)
	if reset, err := time.Parse(time.RFC3339, st.reset); err == nil && st.resumedAt.IsZero() {
		body += fmt.Sprintf(freezeRefusedReset+"%s (the reset its freeze held).\n%s%s.", stamp(reset), freezeStarted, stamp(st.frozenAt))
	}
	return freezeCardTitle(st.class) + "\n\n" + body
}

// freezeFaceWords reads the provider's words off a freeze card's face, the
// fenced block after 'The provider said:'; "" on a face that has none. The
// fence is longer than any backtick run in the words, so its closing line
// is the first line that is the fence alone.
func freezeFaceWords(objective string) string {
	_, rest, ok := strings.Cut(objective, "The provider said:\n\n")
	if !ok {
		return ""
	}
	fence, rest, ok := strings.Cut(rest, "\n")
	if !ok || fence == "" || strings.Trim(fence, "`") != "" {
		return ""
	}
	words, _, ok := strings.Cut(rest, "\n"+fence+"\n")
	if !ok {
		return ""
	}
	return words
}

// class is the pass's view of repository's card of class.
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
// new card (pressClosedCards). Two arms stay quiet: a duplicate, with the
// reset an open, frozen card already holds, and one to the reset of a card
// a human closed (endedOutside).
func (w *FreezeCard) frozen(ctx context.Context, st *freezeClass, ev FreezeEvent) error {
	if quiet, err := w.endedOutside(ctx, st, ev.Reset); err != nil || quiet {
		return err
	}
	if st.taskID == "" {
		if t, ok, err := w.findCard(ctx, st); err != nil {
			return err
		} else if ok {
			st.taskID, st.issue, st.frozenAt, st.reset = t.ID, t.IssueNumber, freezeCardStart(t), freezeFaceReset(t.Objective)
		} else if t, ok, err := w.findClosedCard(ctx, st, ev.At); err != nil {
			return err
		} else if ok {
			// the press closed the card: the re-arm below opens the same
			// card
			st.taskID, st.issue, st.frozenAt, st.reset = t.ID, t.IssueNumber, freezeCardStart(t), freezeFaceReset(t.Objective)
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
	since := st.frozenAt
	switch {
	case st.closed:
		since = ev.At
	case st.taskID == "" && since.IsZero():
		if since, err = w.givenUpStart(ctx, st, ev); err != nil {
			return err
		}
	case since.IsZero():
		since = w.now()
	}
	body := freezeCardBody(ev, count, w.now(), since)
	if st.closed {
		// the press closed the card; a first dated refusal re-arms the
		// hold and updates the same card — no second card, no ask — whose
		// next close is its own. The store opens the card in one write and
		// holds the issue's reopen as owed; then Forgejo follows.
		t := Task{ID: st.taskID, Repository: st.repo, IssueNumber: st.issue}
		comment := fmt.Sprintf("factoryd: the hold re-armed after the retry at %s, to the reset %s. %s.", stamp(ev.At), ev.Reset, cardsWait(count))
		owed, err := w.Store.ReopenFreezeCard(ctx, t, freezeCardTitle(st.class)+"\n\n"+body, comment)
		if err != nil {
			return err
		}
		st.closed = false
		st.reset = ev.Reset
		st.frozenAt = since
		st.resumedAt, st.spoke = time.Time{}, false
		log.Printf("freeze card: the hold re-armed after the retry, %s's card #%d holds again", st.class, st.issue)
		// the card is open in the store: a Forgejo that does not answer
		// leaves the issue's reopen owed to the next pass (reopenOwed)
		if err := w.reopenIssue(ctx, t, owed); forgejoGone(err) || errors.Is(err, ErrRefused) {
			return err
		} else if err != nil {
			log.Printf("freeze card: open issue #%d of %s again: %v: each pass tries it again", st.issue, st.repo, maskedErr(err))
		}
		return nil
	}
	if st.taskID == "" {
		t, err := w.file(ctx, st, ev, body)
		if err != nil {
			return err
		}
		// a replacement takes on its episode's clock and ask
		st.taskID, st.issue, st.reset, st.frozenAt, st.resumedAt = t.ID, t.IssueNumber, ev.Reset, since, time.Time{}
		log.Printf("freeze card: %s filed #%d for the frozen %s class (%s)", st.repo, t.IssueNumber, st.class, cardsWait(count))
		return nil
	}
	// the open card of one episode: its face keeps the ask it spoke once
	if st.spoke {
		body += "\n\n" + freezeAsk(st.class, w.providerLimit(st.repo), since)
	}
	if st.reset == ev.Reset {
		// a recheck's lapse resumed the class and its probe met the reset
		// the card holds: the frozen face comes back, the close is off
		if err := w.Store.SetObjective(ctx, st.taskID, freezeCardTitle(st.class)+"\n\n"+body); err != nil {
			return err
		}
		st.frozenAt, st.resumedAt = since, time.Time{}
		return nil
	}
	// a re-arm: the same card, the new reset and count on its face. The
	// comment names the arm's time, so no other arm says the same words.
	// It posts first, once however often a pass tries, and the face
	// takes the reset only after it, as the ask does (speak): a re-arm
	// Forgejo did not take is on no face, so the next pass makes it again.
	repo, err := SplitRepo(st.repo)
	if err != nil {
		return err
	}
	if err := w.Forgejo.CommentOnce(ctx, repo.Owner, repo.Name, st.issue,
		fmt.Sprintf("factoryd: the hold re-armed at %s, to the reset %s. %s.", stamp(ev.At), ev.Reset, cardsWait(count))); err != nil {
		return fmt.Errorf("comment the re-arm on issue #%d: %w", st.issue, err)
	}
	if err := w.Store.SetObjective(ctx, st.taskID, freezeCardTitle(st.class)+"\n\n"+body); err != nil {
		return err
	}
	st.reset, st.frozenAt, st.resumedAt = ev.Reset, since, time.Time{}
	return nil
}

// givenUpStart is the clock of a card filed for st's class on arm ev: the
// start of the freeze a given-up card of that reset held, whose place the
// new card takes; the arm's own time for a freeze no card held, however
// late Forgejo takes the filing. A given-up face names
// its reset only while its freeze stands (endGivenUp), so every given-up
// card this matches held the same freeze and names the same start.
func (w *FreezeCard) givenUpStart(ctx context.Context, st *freezeClass, ev FreezeEvent) (time.Time, error) {
	done, err := w.Store.FreezeCards(ctx, "done")
	if err != nil {
		return time.Time{}, err
	}
	for _, t := range done {
		if t.Repository == st.repo && objectiveHasClassLine(t.Objective, st.class) && strings.Contains(t.Objective, freezeRefusedSentence) && freezeFaceReset(t.Objective) == ev.Reset {
			return freezeCardStart(t), nil
		}
	}
	return ev.At, nil
}

// endGivenUp takes the reset and the start off the faces of class's
// given-up cards once the class resumed: their freeze ended, and a later
// freeze's card starts its own clock.
func (w *FreezeCard) endGivenUp(ctx context.Context, class string) error {
	done, err := w.Store.FreezeCards(ctx, "done")
	if err != nil {
		return err
	}
	for _, t := range done {
		head, _, kept := strings.Cut(t.Objective, freezeRefusedReset)
		if kept && objectiveHasClassLine(t.Objective, class) && strings.Contains(t.Objective, freezeRefusedSentence) {
			if err := w.Store.SetObjective(ctx, t.ID, head); err != nil {
				return err
			}
		}
	}
	return nil
}

// endedOutside is the one rule on a card of st's class that ended outside
// the watcher, a human having closed its issue: frozen applies it to every
// arm, a standing freeze's among them (stand), and the tick to every card
// it holds. A card in the pass's view whose row no longer waits is forgotten, so
// nothing writes its face or comments on its issue again. An arm to reset
// then stays quiet when a done card of that reset stands which factoryd
// did not close, and files a new card for any other reset. The watcher's
// own closes carry their record (closedBy), whatever the face says.
func (w *FreezeCard) endedOutside(ctx context.Context, st *freezeClass, reset string) (quiet bool, err error) {
	if st.taskID != "" && !st.closed {
		if _, ok, err := w.waiting(ctx, st.taskID); err != nil || ok {
			return false, err
		}
		*st = freezeClass{repo: st.repo, class: st.class}
	}
	if st.taskID != "" {
		return false, nil
	}
	done, err := w.Store.FreezeCards(ctx, "done")
	if err != nil {
		return false, err
	}
	for _, t := range done {
		if t.Repository != st.repo || !objectiveHasClassLine(t.Objective, st.class) || freezeFaceReset(t.Objective) != reset {
			continue
		}
		if _, own, _, err := w.closedBy(ctx, t); err != nil {
			return false, err
		} else if !own {
			return true, nil
		}
	}
	return false, nil
}

// freezePressSaid and freezeEndedSaid are the tells of the watcher's two
// closes in the close's record (closeIssueAsFactory): the retry press's
// and the self-resume's, on the record's first line. A human's close
// through the console is recorded too, with neither there.
const (
	freezePressSaid = "'s retry press cleared the "
	freezeEndedSaid = " class ended: the freeze held "
)

// closedBy reports how done freeze card t ended and when: own when the
// watcher ended it, by the record of its close or by giving the card up,
// press when that close was the retry press's, at the record's time.
// Neither means a human closed it.
func (w *FreezeCard) closedBy(ctx context.Context, t Task) (press, own bool, at time.Time, err error) {
	if strings.Contains(t.Objective, freezeRefusedSentence) {
		return false, true, time.Time{}, nil
	}
	events, err := w.Store.IntakeEvents(ctx, t.ID)
	for _, e := range events {
		if e.Kind == IntakeFactoryClosed && e.CommentID == t.IssueNumber {
			said, _, _ := strings.Cut(e.Body, "\n")
			press = strings.Contains(said, freezePressSaid)
			own = press || strings.Contains(said, freezeEndedSaid)
			at = e.CreatedAt
		}
	}
	return press, own, at, err
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
		st.taskID, st.issue, st.frozenAt, st.reset = t.ID, t.IssueNumber, freezeCardStart(t), freezeFaceReset(t.Objective)
	}
	if !st.resumedAt.IsZero() {
		return nil
	}
	body := freezeCardTitle(st.class) + "\n\n" + fmt.Sprintf(
		freezeResumedFace, st.class, stamp(ev.At), freezeHeld(st.frozenAt, ev.At), stamp(ev.At.Add(freezeResumeHold)), stamp(st.frozenAt), st.reset)
	if err := w.Store.SetObjective(ctx, st.taskID, body); err != nil {
		return err
	}
	st.resumedAt = ev.At
	log.Printf("freeze card: the %s class resumed, its card #%d closes at %s", st.class, st.issue, stamp(st.resumedAt.Add(freezeResumeHold)))
	return nil
}

// CloseOnRetry closes the class's open freeze card at once, no
// five-minute grace, its issue naming by, who pressed: the retry
// press — the chat action and the card button — calls it after it clears
// the hold. It matches the class's own card only, by the whole-line Class
// marker, and is idempotent when no open card stands. The card ends in
// one write under the press-closed face, with the reset the hold named
// (end): a write that fails leaves the card as it was, to close five
// minutes after its class resumed or on a press made again. A card a
// human's close is in flight on is left to that close. A first dated
// refusal before the face's reset re-arms the hold and opens the same
// card, by the record of this close (findClosedCard), while a first
// success keeps it closed on class_resumed.
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
	t := open[i]
	msg := fmt.Sprintf("%s"+freezePressSaid+"%s class's hold: the freeze card closes at once.", by, class)
	moved, err := w.end(ctx, t, freezePressedBody(class, by, w.now(), freezeFaceReset(t.Objective)), "factoryd: "+msg)
	if err != nil {
		return err
	}
	if moved {
		log.Printf("freeze card: %s's retry press closed %s's card #%d at once", by, class, t.IssueNumber)
	}
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

// pressClosedCards lists the done freeze cards of class the retry press
// closed whose reset lies after at, every repository's: an arm at at
// reopens them. The close's record tells the press (closedBy), not the
// face. A card that ended on a self-resume or was given up, or whose reset
// passed, ended its episode: a new hold files its own card.
func (w *FreezeCard) pressClosedCards(ctx context.Context, class string, at time.Time) ([]Task, error) {
	done, err := w.Store.FreezeCards(ctx, "done")
	if err != nil {
		return nil, err
	}
	var pressed []Task
	for _, t := range done {
		if !objectiveHasClassLine(t.Objective, class) || !beforeReset(at, freezeFaceReset(t.Objective)) {
			continue
		}
		if press, _, _, err := w.closedBy(ctx, t); err != nil {
			return nil, err
		} else if press {
			pressed = append(pressed, t)
		}
	}
	return pressed, nil
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
// itself, and a card whose freeze outgrew waits.provider_limit speaks
// once.
func (w *FreezeCard) tick(ctx context.Context) error {
	now := w.now()
	for key, st := range w.classes {
		if st.taskID == "" || st.closed {
			continue
		}
		if _, err := w.endedOutside(ctx, st, st.reset); err != nil {
			return err
		} else if st.taskID == "" {
			delete(w.classes, key)
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
			err := w.speak(ctx, st, limit)
			switch {
			case forgejoGone(err):
				// the class is still frozen: the replacement files at once,
				// on the episode's clock, and makes the ask that was due
				words, err := w.giveUp(ctx, st, err)
				if err != nil {
					return err
				}
				next := w.class(st.repo, st.class)
				next.frozenAt = st.frozenAt
				if err := w.arm(ctx, next, FreezeEvent{Repository: st.repo, At: now, Kind: "class_frozen", Class: st.class, Reset: st.reset, Words: words}); err != nil {
					return err
				}
				continue
			case errors.Is(err, ErrRefused):
				w.keep(st, err)
				continue
			case err != nil:
				return err
			}
			st.spoke = true
		}
	}
	return nil
}

// keep logs a refusal of st's card that does not say its issue is gone:
// the card stays, and the next pass tries it again. A 401 or a 403 names
// the bot's credential or its rights; any other status names itself.
func (w *FreezeCard) keep(st *freezeClass, err error) {
	_, after, _ := strings.Cut(err.Error(), ": status ")
	status, _, _ := strings.Cut(after, ":")
	action := "Forgejo will not take the request with status " + status + ": a human reads its answer above and restores what it names"
	if status == "401" || status == "403" {
		action = "a human fixes the bot's credential or its rights"
	}
	log.Printf("freeze card: Forgejo refuses %s's %s card%s (%s): %s, and each pass tries the card again",
		st.repo, st.class, issueRef(st.issue), textutil.Truncate(maskedErr(err), 300), action)
}

// wake signals provider_retry to every running card of cards, the ones a
// class's hold froze: the frozen cards it woke, of how many, and the wakes
// that failed. A watcher with no Signal wakes none.
func (w *FreezeCard) wake(ctx context.Context, cards []Task) (woken, frozen int, errs []error) {
	for _, c := range cards {
		if c.Status != "running" {
			continue
		}
		frozen++
		if w.Signal == nil {
			continue
		}
		if err := w.Signal(ctx, c.ID); err != nil {
			errs = append(errs, fmt.Errorf("wake %s: %w", c.ID, err))
			continue
		}
		woken++
	}
	return woken, frozen, errs
}

// providerLimit is repository's waits.provider_limit, the default's when
// the version in force names none.
func (w *FreezeCard) providerLimit(repository string) time.Duration {
	if d := w.Current().Tree.WaitsOf(repository).ProviderLimit; d > 0 {
		return d
	}
	return policy.DefaultWaits.ProviderLimit
}

// freezeAsk is the freeze card's one ask, once the freeze of class that
// started at since held past limit, waits.provider_limit. It names since,
// so each freeze of a card the press closed and a re-arm opened again
// makes its own ask.
func freezeAsk(class string, limit time.Duration, since time.Time) string {
	return fmt.Sprintf("The freeze of the %s class has held since %s, past waits.provider_limit (%s). "+
		"Add a fallback for the class, switch the class's model in policy, or keep waiting. The work cards ask nothing.",
		class, stamp(since), limit)
}

// speak makes the freeze card ask once: add a fallback, switch the model,
// keep waiting. The work cards ask nothing. The comment posts first, once
// however often a pass tries (CommentOnce), and the face takes the ask
// only after it: an ask Forgejo refused is on no face, so the next pass
// does not read it as made (rehydrate).
func (w *FreezeCard) speak(ctx context.Context, st *freezeClass, limit time.Duration) error {
	ask := freezeAsk(st.class, limit, st.frozenAt)
	repo, err := SplitRepo(st.repo)
	if err != nil {
		return err
	}
	if err := w.Forgejo.CommentOnce(ctx, repo.Owner, repo.Name, st.issue, "factoryd: "+ask); err != nil {
		return fmt.Errorf("comment the provider-limit ask on issue #%d: %w", st.issue, err)
	}
	t, ok, err := w.waiting(ctx, st.taskID)
	if err != nil {
		return err
	}
	if ok {
		if err := w.Store.SetObjective(ctx, st.taskID, t.Objective+"\n\n"+ask); err != nil {
			return err
		}
	}
	log.Printf("freeze card: the %s class's freeze outgrew %s, its card #%d asked once", st.class, limit, st.issue)
	return nil
}

// close ends the card once its resume hold ran out (end).
func (w *FreezeCard) close(ctx context.Context, st *freezeClass) error {
	t, ok, err := w.waiting(ctx, st.taskID)
	if err != nil || !ok {
		return err
	}
	msg := fmt.Sprintf("The freeze of the %s"+freezeEndedSaid+"%s, and the card closes.", st.class, freezeHeld(st.frozenAt, st.resumedAt))
	moved, err := w.end(ctx, t, "", "factoryd: "+msg)
	if moved {
		log.Printf("freeze card: %s (#%d)", msg, t.IssueNumber)
	}
	return err
}

// waiting reads task id's row while it still waits on a human; ok is
// false once it moved.
func (w *FreezeCard) waiting(ctx context.Context, id string) (Task, bool, error) {
	t, ok, err := w.Store.Task(ctx, id)
	return t, ok && t.Status == "waiting-human", err
}

// findCard matches the class's open freeze card of the repository, by the
// whole-line Class marker on its face.
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

// findClosedCard matches the repository's card among pressClosedCards, so
// a first dated refusal after the press re-arms the same card, no second
// card.
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
func freezeCardBody(ev FreezeEvent, count int, now, since time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Class: %s\n\n", ev.Class)
	b.WriteString("The provider limit freezes the class: every call of the class is answered with the named refusal until the reset.\n\n")
	b.WriteString("The provider said:\n\n" + fenced(ev.Words) + "\n\n")
	if reset, err := time.Parse(time.RFC3339, ev.Reset); err == nil {
		fmt.Fprintf(&b, "The class resets at %s (%s). ", stamp(reset), freezeCountdown(reset, now))
	} else {
		fmt.Fprintf(&b, "The class resets at %s. ", ev.Reset)
	}
	fmt.Fprintf(&b, "%s.\n%s%s.\n\n", cardsWait(count), freezeStarted, stamp(since))
	b.WriteString("This card holds no work slot and asks nothing. It updates on a re-arm, and closes itself five minutes after the class resumes.")
	return b.String()
}

// cardsWait words how many cards a class's hold freezes: "1 card of the
// class waits", "9 cards of the class wait".
func cardsWait(count int) string {
	return textutil.Plural(count, "card of the class waits", "cards of the class wait")
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
	out, err := s.freezeEvents(ctx, `SELECT `+freezeEventColumns+` FROM events
		WHERE id > $1 AND kind IN ('class_frozen', 'class_resumed') ORDER BY id`, stored)
	if err != nil {
		return nil, 0, err
	}
	if len(out) > 0 {
		stored = out[len(out)-1].ID
	}
	return out, stored, nil
}

// StandingFreezes lists the freezes that stand at the stored cursor: one
// event for each repository that armed a class since the class's last
// class_resumed, with that repository and with the id, time, reset and
// words of the class's newest class_frozen, in that id's order.
func (s *PGStore) StandingFreezes(ctx context.Context) ([]FreezeEvent, error) {
	return s.freezeEvents(ctx, `WITH armed AS (
			SELECT e.id, e.at, e.repository, e.detail, e.detail->>'class' AS class FROM events e
			WHERE e.id <= (SELECT cursor FROM freeze_cursor WHERE id = 1) AND e.kind = 'class_frozen'
			  AND NOT EXISTS (SELECT 1 FROM events r WHERE r.kind = 'class_resumed' AND r.id > e.id
				AND r.id <= (SELECT cursor FROM freeze_cursor WHERE id = 1) AND r.detail->>'class' = e.detail->>'class')),
		newest AS (SELECT DISTINCT ON (class) * FROM armed ORDER BY class, id DESC)
		SELECT n.id, n.at, a.repository, '', 'class_frozen',
			COALESCE(n.class, ''), COALESCE(n.detail->>'reset', ''), COALESCE(n.detail->>'words', '')
		FROM (SELECT DISTINCT class, repository FROM armed) a JOIN newest n ON n.class IS NOT DISTINCT FROM a.class
		ORDER BY n.id, a.repository`)
}

const freezeEventColumns = `id, at, repository, COALESCE(task_id, ''), kind,
	COALESCE(detail->>'class', ''), COALESCE(detail->>'reset', ''), COALESCE(detail->>'words', '')`

func (s *PGStore) freezeEvents(ctx context.Context, query string, args ...any) ([]FreezeEvent, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FreezeEvent
	for rows.Next() {
		ev := FreezeEvent{}
		if err := rows.Scan(&ev.ID, &ev.At, &ev.Repository, &ev.TaskID, &ev.Kind, &ev.Class, &ev.Reset, &ev.Words); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
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

// EndFreezeCard ends waiting freeze card t in one transaction: the row
// moves to done under face (kept when face is ""), the close of t's issue
// is recorded with comment as its body unless comment is "", and an owed
// reopen of the card is deleted. A row that no longer waits, or a close
// record that stands on the issue already, rolls all of it back and
// answers false.
func (s *PGStore) EndFreezeCard(ctx context.Context, t Task, face, comment string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	events, moved, err := changeStatusOn(ctx, tx, transitionSet+`, objective = CASE WHEN $4::text = '' THEN objective ELSE $4::text END`,
		transitionWhere+` AND source = $5`, t.ID, "done", []string{"waiting-human"}, face, SourceFreezeCard)
	if err != nil || !moved {
		return false, err
	}
	if comment != "" {
		res, err := tx.ExecContext(ctx, `INSERT INTO intake_events (task_id, kind, author, body, comment_id)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`, t.ID, IntakeFactoryClosed, FactoryDecider, comment, t.IssueNumber)
		if err != nil {
			return false, err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM intake_events WHERE task_id = $1 AND kind = $2`, t.ID, IntakeFactoryReopened); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	s.recordStatus(ctx, events)
	return true, nil
}

// ReopenFreezeCard opens done freeze card t again in one transaction: the
// row moves to waiting-human under face, the close's record goes off the
// issue (comment_id 0) and counts as confirmed, so no pass closes the
// issue again, and the owed reopen of the issue is recorded with comment
// as its body, after the comment of that close when Forgejo did not
// confirm it (freezeOwedSplit). A row that is not done is ErrNotFound, and
// nothing is written.
func (s *PGStore) ReopenFreezeCard(ctx context.Context, t Task, face, comment string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	events, moved, err := changeStatusOn(ctx, tx, transitionSet+`, objective = $4`,
		transitionWhere+` AND source = $5`, t.ID, "waiting-human", []string{"done"}, face, SourceFreezeCard)
	if err != nil {
		return "", err
	}
	if !moved {
		return "", fmt.Errorf("freeze card %s: %w (not done, cannot open again)", t.ID, ErrNotFound)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM intake_events WHERE task_id = $1 AND kind = $2`, t.ID, IntakeFactoryReopened); err != nil {
		return "", err
	}
	var owed string
	if err := tx.QueryRowContext(ctx, `INSERT INTO intake_events (task_id, kind, author, body, comment_id)
		VALUES ($1, $2, $3, COALESCE((SELECT c.body || $7::text FROM intake_events c
			WHERE c.task_id = $1 AND c.kind = $6 AND c.comment_id = $5 AND c.confirmed_at IS NULL AND c.body <> ''), '') || $4::text, $5)
		RETURNING body`,
		t.ID, IntakeFactoryReopened, FactoryDecider, comment, t.IssueNumber, IntakeFactoryClosed, freezeOwedSplit).Scan(&owed); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE intake_events SET comment_id = 0, confirmed_at = COALESCE(confirmed_at, now())
		WHERE task_id = $1 AND kind = $2 AND comment_id = $3`, t.ID, IntakeFactoryClosed, t.IssueNumber); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	s.recordStatus(ctx, events)
	return owed, nil
}

// OwedFreezeReopens lists the reopens factoryd owes Forgejo, oldest first.
func (s *PGStore) OwedFreezeReopens(ctx context.Context) ([]IntakeEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+intakeColumns+` FROM intake_events WHERE kind = 'factory_reopened' ORDER BY id`)
	return scanIntakeEvents(rows, err)
}

// UnconfirmedFreezeCloses lists the closes of freeze cards that stand on
// their issues unconfirmed for olderThan (on the store's clock), oldest
// first.
func (s *PGStore) UnconfirmedFreezeCloses(ctx context.Context, olderThan time.Duration) ([]IntakeEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+intakeColumns+` FROM intake_events
		WHERE kind = $1 AND comment_id <> 0 AND confirmed_at IS NULL AND created_at < now() - $2 * interval '1 second'
		  AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = intake_events.task_id AND t.source = $3)
		ORDER BY id`, IntakeFactoryClosed, olderThan.Seconds(), SourceFreezeCard)
	return scanIntakeEvents(rows, err)
}

// HeldResumes lists the class_resumed rows that are the newest row of
// their class at the stored cursor and that no class_woken row names,
// oldest first.
func (s *PGStore) HeldResumes(ctx context.Context) ([]FreezeEvent, error) {
	return s.freezeEvents(ctx, `SELECT `+freezeEventColumns+` FROM (
		SELECT DISTINCT ON (detail->>'class') * FROM events
		WHERE id <= (SELECT cursor FROM freeze_cursor WHERE id = 1) AND kind IN ('class_frozen', 'class_resumed')
		ORDER BY detail->>'class', id DESC) newest
		WHERE kind = 'class_resumed' AND NOT EXISTS (SELECT 1 FROM events w
			WHERE w.id > newest.id AND w.kind = 'class_woken' AND w.detail->>'resumed' = newest.id::text) ORDER BY id`)
}

// RecordFreezeWake records, on the events table, that the frozen cards of
// resumed's class were woken for that class_resumed row.
func (s *PGStore) RecordFreezeWake(ctx context.Context, resumed FreezeEvent) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO events (repository, kind, actor, via, detail)
		VALUES ($1, $2, $3, $4, jsonb_build_object('class', $5::text, 'resumed', $6::text))`,
		resumed.Repository, EventClassWoken, FactoryDecider, ViaFactoryd, resumed.Class, strconv.FormatInt(resumed.ID, 10))
	return err
}

// SettleFreezeReopen deletes the owed reopen of task taskID's issue, which
// Forgejo took.
func (s *PGStore) SettleFreezeReopen(ctx context.Context, taskID string, issue int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM intake_events WHERE task_id = $1 AND kind = $2 AND comment_id = $3`, taskID, IntakeFactoryReopened, issue)
	return err
}
