package factoryd

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// reviewParallel is the most review or audit calls of one activity that
// wait on the gateway at once, each under its own hold.
const reviewParallel = 4

// reviewCall is one model call of a review or an audit: Label names it in
// a note ("part 2 of 5"). Input builds the call's input when the call
// starts, so the diff text of the calls not running is held once, in its
// parts.
type reviewCall struct {
	Label  string
	Prompt string
	Input  func() any
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
		content, deployment, err := r.supervisor.Chat(ctx, taskID, model, purpose, timeout, calls[i].Prompt, calls[i].Input())
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

// unfunded says why the card's free headroom cannot pay for calls, before
// the first is made; "" when it can, or when their model has no price,
// whose calls the gateway refuses itself. The need is what the calls are
// expected to spend, priced by Fit alone: each call's request at the
// ref's settled calls (fanOutPrice), at the ref's mean output but for the
// last call, at the whole limit. A premium call the premium month's
// headroom cannot hold is priced on big, where the gateway serves it. The
// note names what the approval card offers a person instead.
func (r *Runner) unfunded(ctx context.Context, taskID, model, purpose string, calls []reviewCall) (string, error) {
	if r.cfg.Prices == nil || model == "" || len(calls) == 0 {
		return "", nil
	}
	limit, err := r.supervisor.Budget.OutputLimit()
	if err != nil {
		return "", err
	}
	alias := aliasOf(model)
	at, ok, err := r.fanOutPrice(ctx, taskID, purpose, alias)
	if !ok || err != nil {
		return "", err
	}
	req := policy.HoldRequest{TaskID: taskID, Kind: policy.KindModel, ProjectEUR: math.Inf(1)}
	if alias == policy.Premium {
		req.PremiumEUR = r.cfg.Policy().Document.Budget.PremiumMonthEUR
	}
	own, free, err := r.store.Free(ctx, req)
	if err != nil {
		return "", err
	}
	var big *callPrice
	need := 0.0
	for i, c := range calls {
		body, err := chatBody(model, c.Prompt, c.Input(), limit)
		if err != nil {
			return "", err
		}
		p, last := at, i == len(calls)-1
		onBig := free.Premium != nil && at.cost(len(body), limit, true) > *free.Premium
		if onBig {
			if big == nil {
				fallback, ok, err := r.fanOutPrice(ctx, taskID, purpose, policy.Big)
				if !ok || err != nil {
					return "", err
				}
				big = &fallback
			}
			p = *big
		}
		cost := p.cost(len(body), limit, last)
		if free.Premium != nil && !onBig {
			*free.Premium -= cost
		}
		need += cost
	}
	if free := own.Free(); need > free {
		return fmt.Sprintf("its %s would cost about %.2f EUR, and the card has %.2f EUR free. Read the diff, then Approve the merge or Deny it (Close on a pull request factoryd opened).",
			textutil.Plural(len(calls), "call", "calls"), need, max(free, 0)), nil
	}
	return "", nil
}

// callPrice is what an estimate prices one alias's calls at: the request,
// calibrated on the ref's settled calls (Store.CalibrateEstimate), and
// the ref's mean output.
type callPrice struct {
	req  policy.HoldRequest
	mean int
}

// fanOutPrice is alias's callPrice for purpose; false when alias has no
// price.
func (r *Runner) fanOutPrice(ctx context.Context, taskID, purpose, alias string) (callPrice, bool, error) {
	price, err := r.price(ctx, alias)
	if err != nil {
		return callPrice{}, false, nil
	}
	req, mean, err := r.store.CalibrateEstimate(ctx, policy.HoldRequest{TaskID: taskID, Kind: policy.KindModel, Ref: policy.LedgerRef(purpose, alias), Price: price})
	return callPrice{req, mean}, err == nil, err
}

// cost is Fit on a call of bytes at p, the headroom unbounded: its output
// the ref's mean, or limit for the last call or a ref without one.
func (p callPrice) cost(bytes, limit int, last bool) float64 {
	r := p.req
	r.Bytes, r.WantOut = bytes, limit
	if p.mean > 0 && !last {
		r.WantOut = min(p.mean, limit)
	}
	inf := math.Inf(1)
	h, _ := policy.Fit(policy.Free{Task: inf, Day: inf, Month: inf, Project: inf}, r)
	return h.EUR
}

// partsNamed names the parts at, 0-based and ascending, of n: "part 3 of
// 9", "parts 3 to 5 of 9", "parts 1, 3 to 5 and 9 of 9".
func partsNamed(at []int, n int) string {
	var runs []string
	for i := 0; i < len(at); {
		j := i
		for j+1 < len(at) && at[j+1] == at[j]+1 {
			j++
		}
		run := strconv.Itoa(at[i] + 1)
		if j > i {
			run += " to " + strconv.Itoa(at[j]+1)
		}
		runs = append(runs, run)
		i = j + 1
	}
	list, word := runs[len(runs)-1], "parts"
	if len(runs) > 1 {
		list = strings.Join(runs[:len(runs)-1], ", ") + " and " + list
	}
	if len(at) == 1 {
		word = "part"
	}
	return fmt.Sprintf("%s %s of %d", word, list, n)
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
