package factoryd

import (
	"context"
	"fmt"

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

// ResumeSlot is AdmitResume's answer: Granted when the card runs (SetState
// moved it), else Ahead resumed cards come before it.
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
// wakes it (SignalSlotFree). Granting moves it to running inside the same
// lock that counts, so two grants never take one slot.
func (r *Runner) AdmitResume(ctx context.Context, taskID string) (ResumeSlot, error) {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return ResumeSlot{}, err
	}
	if !resumesIntoSlot(t) {
		return ResumeSlot{Granted: true}, nil
	}
	slot, err := r.admitResume(ctx, t)
	if err == nil && slot.Granted {
		wakeResumed(ctx, r.store, r.cfg.Workflows, t.Repository)
	}
	return slot, err
}

func (r *Runner) admitResume(ctx context.Context, t Task) (ResumeSlot, error) {
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
	ahead := len(waiters)
	for i, w := range waiters {
		if w.ID == t.ID {
			ahead = i
			break
		}
	}
	if ahead == 0 && live < limit {
		return ResumeSlot{Granted: true}, r.SetState(ctx, t.ID, "running")
	}
	reason := fmt.Sprintf(ResumedWaitReason+"%d of %d live, %d ahead", live, limit, ahead)
	if err := r.store.SetResumeWait(ctx, t.ID, reason, r.clock()); err != nil {
		return ResumeSlot{}, err
	}
	if t.ResumedAt == nil {
		mirrorTaskState(ctx, r.forgejo, t, "queued")
	}
	return ResumeSlot{Ahead: ahead}, nil
}

// awaitSlot is the surge bound's wait before a resumed card's session:
// AdmitResume until it grants, between asks the wait for a freed slot or a
// terminal signal. final is set when that signal decided the card.
func (t *taskRun) awaitSlot() (final, why string) {
	for {
		var s ResumeSlot
		if err := workflow.ExecuteActivity(t.short, ActivityAdmitResume, t.taskID).Get(t.ctx, &s); err != nil {
			return "failed", "admit the resumed session: " + errMessage(err)
		}
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
