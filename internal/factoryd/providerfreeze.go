package factoryd

import (
	"context"
	"errors"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// freezeWakeMargin is how long past a freeze's reset a consumer waits
// before it retries: the gateway's hold lapses at the reset it named (its
// parse plus one minute), and the margin keeps the retry, and a brief
// case's wake, clear of it.
const freezeWakeMargin = 2 * time.Minute

// providerLimitUntil is the reset the named refusal in err's chain holds
// the class to (gateway.RefusedProviderLimit), ok false for any other
// error or a refusal whose body named no reset.
func providerLimitUntil(err error) (time.Time, bool) {
	var refusal *policy.Refusal
	if errors.As(err, &refusal) && refusal.Decision == gateway.RefusedProviderLimit && !refusal.Until.IsZero() {
		return refusal.Until, true
	}
	return time.Time{}, false
}

// waitUntil sleeps until at, or answers ctx's end.
func waitUntil(ctx context.Context, at time.Time) error {
	d := time.Until(at)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
