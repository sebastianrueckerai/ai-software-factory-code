package factoryd

import (
	"context"
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
// the bound leaves out (Task.Unbound: a roll, a factory pull request, a
// screening), a card past its start (every status but queued) and a queued
// card already admitted (its workflow died before running) start as
// before: no slot is taken, or the one it holds is its own. A card parked
// on a person — waiting-human for an answer, waiting-poster for a fund
// after a cap — or on its image (waiting-image) holds no slot (roadmap
// 233): the bound counts only what runs, and a person's answer or the
// image resumes the card as running even at the bound, an overshoot of one
// gate pass that touches only the card just unblocked.
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
// moment it is taken.
func (w BoundedWorkflows) admit(ctx context.Context, taskID string) (bool, error) {
	t, ok, err := w.Store.Task(ctx, taskID)
	if err != nil || !ok {
		return true, err
	}
	if t.Kind != "agent" || t.Unbound() || t.Status != "queued" || t.AdmittedAt != nil {
		return true, nil
	}
	deps, err := w.Store.OpenDependencies(ctx, taskID)
	if err != nil {
		return false, err
	}
	if len(deps) > 0 && ended(deps) == nil {
		return false, nil
	}
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	if _, cap := w.Policy().ProjectCap(t.Repository); !math.IsInf(cap, 1) {
		spent, err := w.Store.ProjectMonthSpend(ctx, t.Repository, now())
		if err != nil {
			return false, err
		}
		if spent.EUR >= cap {
			return false, nil
		}
	}
	admitMu.Lock()
	defer admitMu.Unlock()
	n, err := w.Store.CountAdmitted(ctx, t.Repository)
	if err != nil {
		return false, err
	}
	if n >= w.Policy().WorkInProgressOf(t.Repository) {
		return false, nil
	}
	if err := w.Store.SetAdmitted(ctx, taskID); err != nil {
		return false, err
	}
	return true, nil
}

// pullQueued starts repository's queued cards in filing order, the oldest
// first: each start goes through the bound, so the pull stops at the
// project's work_in_progress of the moment.
func pullQueued(ctx context.Context, store Store, wf Workflows, repository string) {
	if wf == nil {
		return
	}
	cards, err := store.ListQueuedByRepository(ctx, repository)
	if err != nil {
		log.Printf("pull %s: list queued: %v", repository, err)
		return
	}
	for _, c := range cards {
		if err := wf.Start(ctx, c.ID); err != nil {
			log.Printf("pull %s: start %s: %v", repository, c.ID, err)
			return
		}
	}
}
