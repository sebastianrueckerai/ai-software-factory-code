package factoryd

import (
	"context"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// BoundedWorkflows is Workflows under the work bound (roadmap 225), the
// one admission step every start passes: a queued card starts only while
// fewer than its project's work_in_progress cards hold a work slot, every
// card it depends on is done (or one ended closed, which its workflow takes
// to a person), and its project's month budget has headroom; else it stays
// queued, holding no slot, until a slot frees (a finished or parked card
// hands its slot to the oldest queued card of its project, the pull) or
// the heal pass's sweep re-derives the same admission every minute. A card
// the bound leaves out (Task.TakesNoSlot: a roll, a factory pull request, a
// screening, a red main's fix-forward card), a card past its start (every status but queued) and a queued
// card already admitted (its workflow died before running) start as
// before: no slot is taken, or the one it holds is its own. A card holds
// a slot while it runs or closes, while in review in any gate but the
// approval's, and while queued with its admission stamped (CountAdmitted,
// holdsSlot). A card parked on a person — waiting-human for an answer,
// waiting-poster for a fund after a cap, the approval gate for the merge —
// on a dependency (waiting-deps) or on its image (waiting-image) holds no
// slot (roadmap 233): the bound counts only what runs. Entering the
// approval gate or waiting-deps pulls the next queued card, and the card
// keeps its admission stamp, the time of its first admission. A card
// back from a wait takes its session start through the same count, with
// the project's surge on top of the bound (AdmitResume): past it the card
// waits queued, holding no slot, and the pull wakes the resumed cards, the
// oldest resumption first, before it starts any never-started card, which
// waits while a resumed card does.
type BoundedWorkflows struct {
	Workflows
	// Store reads the card, its dependencies and its project's spend, and
	// counts its repository's work slots.
	Store Store
	// Policy reads the project's work_in_progress and month budget.
	Policy func() policy.Tree
	// Now is the clock of the month the budget counts; nil is time.Now.
	Now func() time.Time
}

// WorkBoundReason starts the reason of a card the count refused, "work
// bound N of M" with N the slots held and M the bound: one kind of
// reason whatever its numbers, so its first refusal's time holds.
const WorkBoundReason = "work bound "

// admitMu holds the count-and-stamp of an admission together: with one
// factoryd, two admissions can never take one slot (allDoneMu's pattern).
var admitMu sync.Mutex

func (w BoundedWorkflows) Start(ctx context.Context, taskID string) error {
	admit, err := w.admit(ctx, taskID)
	if err != nil || !admit {
		return err
	}
	return w.Workflows.Start(ctx, taskID)
}

// Signal to a card over the bound is not lost: the card stays queued, and
// the workflow that starts once a slot frees reconciles from Forgejo,
// where the signal's words (an answer, a change request) sit. A close
// passes the bound, for a card its dependencies or its budget may keep
// queued for good: the workflow it starts finds the issue closed and ends
// the card before any session, holding no slot.
func (w BoundedWorkflows) Signal(ctx context.Context, taskID, name string, sig TaskSignal) error {
	if name == SignalSlotFree {
		room, err := w.surgeRoom(ctx, taskID)
		if err != nil || !room {
			return err
		}
	}
	if name != SignalIssueClosed {
		admit, err := w.admit(ctx, taskID)
		if err != nil || !admit {
			return err
		}
	}
	return w.Workflows.Signal(ctx, taskID, name, sig)
}

// admit reports whether task id's start takes the workflow as it is:
// true for every card out of the bound's reach, and for a queued card
// whose dependencies and project budget let it start under the bound,
// whose admission it stamps before the start, so the slot is held the
// moment it is taken. A refused card carries why on its row
// (SetQueueReason), which the admission's stamp clears.
func (w BoundedWorkflows) admit(ctx context.Context, taskID string) (bool, error) {
	t, ok, err := w.Store.Task(ctx, taskID)
	if err != nil || !ok {
		return true, err
	}
	if !waitsForSlot(t) {
		return true, nil
	}
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	at := now()
	why, err := admissionWait(ctx, w.Store, w.Policy(), at, t)
	if err != nil {
		return false, err
	}
	if why != "" {
		return false, w.Store.SetQueueReason(ctx, taskID, why, at)
	}
	admitMu.Lock()
	defer admitMu.Unlock()
	n, err := w.Store.CountAdmitted(ctx, t.Repository)
	if err != nil {
		return false, err
	}
	waiters, err := w.Store.ResumeWaiters(ctx, t.Repository)
	if err != nil {
		return false, err
	}
	if bound := w.Policy().WorkInProgressOf(t.Repository); n >= bound || len(waiters) > 0 {
		reason := fmt.Sprintf(WorkBoundReason+"%d of %d", n, bound)
		if len(waiters) > 0 {
			reason += fmt.Sprintf(", %d resumed first", len(waiters))
		}
		return false, w.Store.SetQueueReason(ctx, taskID, reason, at)
	}
	if err := w.Store.SetAdmitted(ctx, taskID); err != nil {
		return false, err
	}
	return true, nil
}

// surgeRoom reports whether card taskID's project has a session slot a
// resumed card may take: its live count is under work_in_progress plus surge.
func (w BoundedWorkflows) surgeRoom(ctx context.Context, taskID string) (bool, error) {
	t, ok, err := w.Store.Task(ctx, taskID)
	if err != nil || !ok {
		return true, err
	}
	n, err := w.Store.CountAdmitted(ctx, t.Repository)
	v := w.Policy()
	return n < v.WorkInProgressOf(t.Repository)+v.SurgeOf(t.Repository), err
}

// wakeResumed tells every card waiting queued to resume in repository that
// a slot may be free; each asks AdmitResume again, which keeps their order.
func wakeResumed(ctx context.Context, store Store, wf Workflows, repository string) {
	if wf == nil {
		return
	}
	waiters, err := store.ResumeWaiters(ctx, repository)
	if err != nil {
		log.Printf("wake %s: list resumed: %v", repository, err)
		return
	}
	for _, c := range waiters {
		if err := wf.Signal(ctx, c.ID, SignalSlotFree, TaskSignal{}); err != nil {
			log.Printf("wake %s: signal %s: %v", repository, c.ID, err)
		}
	}
}

// waitsForSlot reports whether card t is one admit judges: a queued agent
// card in the bound's reach (not TakesNoSlot), not admitted yet.
func waitsForSlot(t Task) bool {
	return t.Kind == "agent" && !t.TakesNoSlot() && t.Status == "queued" && t.AdmittedAt == nil && t.ResumedAt == nil
}

// admissionWait is what keeps card t, one waitsForSlot names, from a
// work slot before any count of slots, at now: "no issue", "waits on #N"
// (the first of its open dependencies, unless one ended closed, which its
// workflow takes to a person) or "project month cap"; "" when none does.
// admit runs it and records its answer on the card, which the board digest
// reads.
func admissionWait(ctx context.Context, store Store, tree policy.Tree, now time.Time, t Task) (string, error) {
	// a plan files its card row first and its issue last; the half-made
	// card holds no slot until its issue is filed, and the next sweep
	// starts it. A report card never reaches here: it is unbound.
	if t.IssueNumber == 0 {
		return "no issue", nil
	}
	deps, err := store.OpenDependencies(ctx, t.ID)
	if err != nil {
		return "", err
	}
	if len(deps) > 0 && ended(deps) == nil {
		return fmt.Sprintf("waits on #%d", deps[0].Issue), nil
	}
	if _, cap := tree.ProjectCap(t.Repository); !math.IsInf(cap, 1) {
		spent, err := store.ProjectMonthSpend(ctx, t.Repository, now)
		if err != nil {
			return "", err
		}
		if spent.EUR >= cap {
			return "project month cap", nil
		}
	}
	return "", nil
}

// pullQueued starts repository's queued cards in filing order, the oldest
// first: each start goes through the bound, so the pull stops at the
// project's work_in_progress of the moment.
func pullQueued(ctx context.Context, store Store, wf Workflows, repository string) {
	if wf == nil {
		return
	}
	wakeResumed(ctx, store, wf, repository)
	cards, err := store.ListQueuedByRepository(ctx, repository)
	if err != nil {
		log.Printf("pull %s: list queued: %s", repository, maskedErr(err))
		return
	}
	for _, c := range cards {
		if err := wf.Start(ctx, c.ID); err != nil {
			log.Printf("pull %s: start %s: %s", repository, c.ID, maskedErr(err))
			return
		}
	}
}
