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
// each through r.supervisor.Chat. A call the gateway refused for its
// headroom (refusedForHeadroom) is retried once after the others end, one
// at a time, with the calls not yet made, which no worker starts after
// such a refusal; the first refusal there gives the rest the same refusal
// without a call. It bounds the running activity by its waves of calls
// (boundByParts), and again before the retries with one wave for each of
// them. Returns ctx.Err() when ctx ended.
func (r *Runner) callAll(ctx context.Context, taskID, model, purpose string, timeout time.Duration, calls []reviewCall) ([]callResult, error) {
	waves := (len(calls) + reviewParallel - 1) / reviewParallel
	boundByParts(ctx, waves)
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
				if refusedForHeadroom(results[i].Err) {
					refused.Store(true)
				}
			}
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var retries []int
	for i := range calls {
		if !ran[i] || refusedForHeadroom(results[i].Err) {
			retries = append(retries, i)
		}
	}
	if len(retries) > 0 {
		boundByParts(ctx, waves+len(retries))
	}
	var again error
	for _, i := range retries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if again != nil {
			results[i] = callResult{Err: again}
			continue
		}
		call(i)
		if refusedForHeadroom(results[i].Err) {
			again = results[i].Err
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return results, nil
}

// refusedForHeadroom: the gateway refused the call for the smallest of
// the task's, the day's, the month's and the project's headroom
// (policy.HeadroomBound), which the other calls' holds may have taken.
func refusedForHeadroom(err error) bool {
	var refusal *policy.Refusal
	if !errors.As(err, &refusal) {
		return false
	}
	switch refusal.Decision {
	case policy.RefusedBudget, policy.RefusedDayCap, policy.RefusedMonthCap, policy.RefusedProjectCap:
		return true
	}
	return false
}
