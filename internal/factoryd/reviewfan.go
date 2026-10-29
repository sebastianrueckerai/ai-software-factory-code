package factoryd

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// reviewParallel is the most review or audit calls of one activity that
// wait on the gateway at once, each under its own hold.
const reviewParallel = 4

// reviewCall is one model call of a review or an audit: Label names it in
// a note ("part 2 of 5").
type reviewCall struct {
	Label  string
	Prompt string
	Input  any
}

// callResult is one call's answer, or why it has none.
type callResult struct {
	Content    string
	Deployment string
	Err        error
}

// callAll makes calls in order of index, at most reviewParallel at once,
// each through r.supervisor.Chat. A call the gateway refused for the
// budget (policy.RefusedBudget) is retried once after the others end,
// one at a time, with the calls not yet made, which no worker starts
// after a budget refusal; the first refusal there gives the rest the same
// refusal without a call. Returns ctx.Err() when ctx ended.
func (r *Runner) callAll(ctx context.Context, taskID, model, purpose string, timeout time.Duration, calls []reviewCall) ([]callResult, error) {
	results := make([]callResult, len(calls))
	ran := make([]bool, len(calls))
	call := func(i int) {
		ran[i] = true
		content, deployment, err := r.supervisor.Chat(ctx, taskID, model, purpose, timeout, calls[i].Prompt, calls[i].Input)
		results[i] = callResult{content, deployment, err}
	}
	next := make(chan int, len(calls))
	for i := range calls {
		next <- i
	}
	close(next)
	var refused atomic.Bool
	var wg sync.WaitGroup
	for range min(reviewParallel, len(calls)) {
		wg.Go(func() {
			for i := range next {
				if refused.Load() || ctx.Err() != nil {
					return
				}
				call(i)
				if refusedForBudget(results[i].Err) {
					refused.Store(true)
				}
			}
		})
	}
	wg.Wait()
	var again error
	for i := range calls {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		switch {
		case ran[i] && !refusedForBudget(results[i].Err):
		case again != nil:
			results[i] = callResult{Err: again}
		default:
			call(i)
			if refusedForBudget(results[i].Err) {
				again = results[i].Err
			}
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return results, nil
}

func refusedForBudget(err error) bool {
	var refusal *policy.Refusal
	return errors.As(err, &refusal) && refusal.Decision == policy.RefusedBudget
}
