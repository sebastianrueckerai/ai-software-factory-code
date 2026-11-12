package factoryd

import (
	"context"
	"time"
)

// retryWait waits d between a failed attempt and its retry and returns the
// context's error if the context ends first; a var so a test can record the
// waits instead of sleeping them.
var retryWait = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
