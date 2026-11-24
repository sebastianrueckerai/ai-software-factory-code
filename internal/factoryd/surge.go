package factoryd

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/workflow"
)

const (
	// ActivityAdmitResume is the one admission of a session start by a card
	// back from a wait.
	ActivityAdmitResume = "AdmitResume"
	// ResumedWaitReason starts the reason of a resumed card the surge bound
	// holds back, "resumed: work bound N of M live, K ahead", so the
	// monitor's queued_over_bound names a resumed wait as such.
	ResumedWaitReason = "resumed: work bound "
	// SignalSlotFree wakes a card waiting queued to resume: a slot may be
	// free for it.
	SignalSlotFree = "slot_free"
)

// ResumeSlot is AdmitResume's answer: Granted when the card holds a slot (a
// grant has moved it to running), else Ahead resumed cards come before it.
type ResumeSlot struct {
	Granted bool
	Ahead   int
}

// resumesIntoSlot reports whether card t is parked, holding no slot, so
// that a session start must pass the surge bound: waiting on a person, a
// cap, an image or a dependency, in the approval gate, or already waiting
// queued to resume.
func resumesIntoSlot(t Task) bool {
	if t.Kind != "agent" || t.Unbound() {
		return false
	}
	switch t.Status {
	case "waiting-human", "waiting-poster", "waiting-image", "waiting-deps":
		return true
	case "in-review":
		return t.Gate == GateApproval
	case "queued":
		return t.ResumedAt != nil
	}
	return false
}

// AdmitResume starts card taskID's session slot: a card holding one, or
// out of the bound's reach, passes. A parked card runs while its project's
// live count is under work_in_progress plus surge and no resumed card is
// ahead of it; else it waits queued, holding no slot, and a freed slot
// wakes it (SignalSlotFree). since is when the wait it resumes from ended,
// the order the queue keeps. Granting moves the card to running inside the
// lock that counts, so two grants never take one slot; the approvals and the
// issue's label follow outside it, in the workflow's SetState.
func (r *Runner) AdmitResume(ctx context.Context, taskID string, since time.Time) (ResumeSlot, error) {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return ResumeSlot{}, err
	}
	line, err := cardHoldWait(ctx, r.store, t)
	if err != nil {
		return ResumeSlot{}, err
	}
	if line != "" {
		parked, err := r.parkHeld(ctx, t, line, since)
		if err != nil || parked {
			return ResumeSlot{}, err
		}
		// a human released the card between the hold's read and the park:
		// it goes on as a card that holds none
		if t, _, err = r.task(ctx, taskID); err != nil {
			return ResumeSlot{}, err
		}
	}
	if !resumesIntoSlot(t) {
		return ResumeSlot{Granted: true}, nil
	}
	slot, err := r.admitResume(ctx, t, since)
	if err != nil {
		return ResumeSlot{}, err
	}
	if slot.Granted {
		wakeResumed(ctx, r.store, r.cfg.Workflows, t.Repository)
	} else if t.ResumedAt == nil {
		mirrorTaskState(ctx, r.forgejo, t, "queued")
	}
	return slot, nil
}

// parkHeld parks held card t queued under its hold's line; false when the
// card was in no state to park. A card that held a slot gives it up: the
// queue is pulled. A card that moved to queued has its issue mirrored. A
// re-check of a parked card wakes no one, or each wake would send it back
// here.
func (r *Runner) parkHeld(ctx context.Context, t Task, line string, since time.Time) (bool, error) {
	parked, err := r.store.SetResumeWait(ctx, t.ID, line, since)
	if err != nil || !parked {
		return parked, err
	}
	if HoldsSlot(t) || t.Status == "running" {
		pullQueued(ctx, r.store, r.cfg.Workflows, t.Repository)
	}
	if t.Status != "queued" {
		mirrorTaskState(ctx, r.forgejo, t, "queued")
	}
	return true, nil
}

func (r *Runner) admitResume(ctx context.Context, t Task, since time.Time) (ResumeSlot, error) {
	admitMu.Lock()
	defer admitMu.Unlock()
	v := r.cfg.Policy()
	limit := v.WorkInProgressOf(t.Repository) + v.SurgeOf(t.Repository)
	live, err := r.store.CountAdmitted(ctx, t.Repository)
	if err != nil {
		return ResumeSlot{}, err
	}
	waiters, err := r.store.ResumeWaiters(ctx, t.Repository)
	if err != nil {
		return ResumeSlot{}, err
	}
	waiters = unheld(waiters)
	ahead := len(waiters)
	for i, w := range waiters {
		if w.ID == t.ID {
			ahead = i
			break
		}
	}
	if ahead == 0 && live < limit {
		return ResumeSlot{Granted: true}, r.store.MarkStarted(ctx, t.ID)
	}
	reason := fmt.Sprintf(ResumedWaitReason+"%d of %d live, %d ahead", live, limit, ahead)
	if _, err := r.store.SetResumeWait(ctx, t.ID, reason, since); err != nil {
		return ResumeSlot{}, err
	}
	return ResumeSlot{Ahead: ahead}, nil
}

// awaitSlot is the surge bound's wait before a resumed card's session:
// AdmitResume until it grants, between asks the wait for a freed slot or a
// terminal signal. final is set when that signal decided the card.
func (t *taskRun) awaitSlot() (final, why string) {
	since := t.waitEnded
	if since.IsZero() {
		since = workflow.Now(t.ctx)
	}
	t.waitEnded = time.Time{}
	for {
		var s ResumeSlot
		if err := workflow.ExecuteActivity(t.short, ActivityAdmitResume, t.taskID, since).Get(t.ctx, &s); err != nil {
			return "failed", "admit the resumed session: " + errMessage(err)
		}
		// a press's hold is in the store, which AdmitResume or the next
		// session's registration reads: no later wait takes the press
		t.dropHolds()
		if s.Granted {
			return "", ""
		}
		sel := workflow.NewSelector(t.ctx)
		for _, n := range []string{SignalSlotFree, SignalPRMerged, SignalPRClosed, SignalIssueClosed} {
			sel.AddReceive(workflow.GetSignalChannel(t.ctx, n), func(workflow.ReceiveChannel, bool) {})
		}
		sel.Select(t.ctx)
		for workflow.GetSignalChannel(t.ctx, SignalSlotFree).ReceiveAsync(nil) {
		}
		if final, why, ok := t.decided(); ok {
			return final, why
		}
	}
}
