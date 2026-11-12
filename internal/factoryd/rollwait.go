package factoryd

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/sdk/activity"
)

// rollPoll is how often a roll's rollout wait reads its state.
const rollPoll = 15 * time.Second

// rollWaitSlack is how long a roll's rollout wait may run past its own
// bound before Temporal ends it.
const rollWaitSlack = 5 * time.Minute

var errStopping = errors.New("factoryd stopping; the wait goes on in the next")

func (r *Runner) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// pause sleeps d; it ends early with an error when ctx ends or factoryd
// stops, and the activity's retry waits on in the next factoryd.
func (r *Runner) pause(ctx context.Context, d time.Duration) error {
	if r.sleep != nil {
		return r.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-activity.GetWorkerStopChannel(ctx):
		return errStopping
	case <-t.C:
		return nil
	}
}
