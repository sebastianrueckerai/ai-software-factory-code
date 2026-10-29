package factoryd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// labelledCalls are n calls whose input is their label, "part i of n".
func labelledCalls(n int) []reviewCall {
	calls := make([]reviewCall, n)
	for i := range calls {
		label := fmt.Sprintf("part %d of %d", i+1, n)
		calls[i] = reviewCall{Label: label, Prompt: "p", Input: label}
	}
	return calls
}

func echo(user string) string { return user }

func fanRunner(t *testing.T, ll *fakeLiteLLM) *Runner {
	t.Helper()
	return &Runner{supervisor: testSupervisorModel(ll.srv(t).URL)}
}

// toldWaves is a context whose gate watch records the waves callAll
// bounds its run by (boundByParts).
func toldWaves() (context.Context, *[]int) {
	var told []int
	return context.WithValue(context.Background(), wavesKey{}, func(n int) { told = append(told, n) }), &told
}

// Eight calls held 100 ms each: four at once, the answers by call index,
// the run bound by two waves.
func TestCallAllRunsFourAtOnce(t *testing.T) {
	ll := &fakeLiteLLM{delay: 100 * time.Millisecond, answer: echo}
	calls := labelledCalls(8)
	ctx, told := toldWaves()
	results, err := fanRunner(t, ll).callAll(ctx, "t1", "big", policy.PurposeReview, time.Minute, calls)
	if err != nil {
		t.Fatal(err)
	}
	if ll.peak != 4 || len(ll.chats) != 8 || len(results) != 8 || !slices.Equal(*told, []int{2}) {
		t.Fatalf("peak %d over %d calls, %d results, waves told %v: want 4 over 8, 8, [2]", ll.peak, len(ll.chats), len(results), *told)
	}
	for i, res := range results {
		if want := fmt.Sprintf("%q", calls[i].Label); res.Err != nil || res.Content != want {
			t.Fatalf("result %d = %+v, want %s", i, res, want)
		}
	}
}

// The gateway refuses every call that shares it with another, for the
// task's budget or for the day's, the month's or the project's cap (each
// policy.HeadroomBound decision): the refused and the untaken calls run
// again one at a time, and each is answered. Only a call alone is
// answered, so six answers mean the retry ran one call at a time.
func TestCallAllRetriesHeadroomRefusalsOneByOne(t *testing.T) {
	for _, decision := range []string{policy.RefusedBudget, policy.RefusedDayCap, policy.RefusedMonthCap, policy.RefusedProjectCap} {
		t.Run(decision, func(t *testing.T) {
			ll := &fakeLiteLLM{delay: 100 * time.Millisecond, answer: echo, status: http.StatusTooManyRequests, decision: decision, crowded: true}
			calls := labelledCalls(6)
			results, err := fanRunner(t, ll).callAll(context.Background(), "t1", "big", policy.PurposeReview, time.Minute, calls)
			if err != nil {
				t.Fatal(err)
			}
			for i, res := range results {
				if want := fmt.Sprintf("%q", calls[i].Label); res.Err != nil || res.Content != want {
					t.Fatalf("result %d = %+v, want %s: a call alone is answered", i, res, want)
				}
			}
			if ll.peak != 4 || len(ll.chats) <= 6 {
				t.Fatalf("peak %d over %d calls: want 4 in the first pass and a retry of the refused", ll.peak, len(ll.chats))
			}
		})
	}
}

// The gateway refuses every call: the retry stops at its first refusal
// and gives every call that refusal. The run is bound by two waves, then
// by the five calls of the retry beside them.
func TestCallAllStopsAtTheFirstRefusalOfTheRetry(t *testing.T) {
	ll := &fakeLiteLLM{status: http.StatusTooManyRequests, decision: policy.RefusedBudget}
	ctx, told := toldWaves()
	results, err := fanRunner(t, ll).callAll(ctx, "t1", "big", policy.PurposeReview, time.Minute, labelledCalls(5))
	if err != nil {
		t.Fatal(err)
	}
	for i, res := range results {
		var refusal *policy.Refusal
		if !errors.As(res.Err, &refusal) || refusal.Decision != policy.RefusedBudget {
			t.Fatalf("result %d = %+v, want the budget refusal", i, res)
		}
	}
	if len(ll.chats) > reviewParallel+1 {
		t.Fatalf("%d calls, want at most %d: the first pass and one call of the retry", len(ll.chats), reviewParallel+1)
	}
	if !slices.Equal(*told, []int{2, 7}) {
		t.Fatalf("waves told %v, want [2 7]", *told)
	}
}

// A failed call that no headroom refused is its result, not retried.
func TestCallAllKeepsOtherErrors(t *testing.T) {
	ll := &fakeLiteLLM{status: http.StatusBadGateway}
	results, err := fanRunner(t, ll).callAll(context.Background(), "t1", "big", policy.PurposeReview, time.Minute, labelledCalls(5))
	if err != nil {
		t.Fatal(err)
	}
	for i, res := range results {
		if res.Err == nil || !strings.Contains(res.Err.Error(), "status 502") {
			t.Fatalf("result %d = %+v, want the failed call", i, res)
		}
	}
	if len(ll.chats) != 5 {
		t.Fatalf("%d calls, want 5: no retry", len(ll.chats))
	}
}

// A cancel while four calls wait on the gateway ends them all: callAll
// returns the context's error at once and leaves no goroutine behind.
func TestCallAllCancelReturnsContextError(t *testing.T) {
	ll := &fakeLiteLLM{delay: time.Minute, answer: echo}
	r := fanRunner(t, ll)
	start := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		results []callResult
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		results, err := r.callAll(ctx, "t1", "big", policy.PurposeReview, time.Hour, labelledCalls(6))
		done <- outcome{results, err}
	}()
	for deadline := time.Now().Add(5 * time.Second); ll.running() < 4; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d calls in flight, want 4", ll.running())
		}
	}
	cancel()
	select {
	case o := <-done:
		if !errors.Is(o.err, context.Canceled) || o.results != nil {
			t.Fatalf("callAll = %v, %v: want no results and context.Canceled", o.results, o.err)
		}
	case <-time.After(time.Second):
		t.Fatal("callAll still runs 1 s after the cancel")
	}
	n := runtime.NumGoroutine()
	for deadline := time.Now().Add(time.Second); n > start && time.Now().Before(deadline); n = runtime.NumGoroutine() {
		time.Sleep(10 * time.Millisecond)
	}
	if n > start {
		t.Fatalf("%d goroutines after the cancel, %d before the calls", n, start)
	}
}
