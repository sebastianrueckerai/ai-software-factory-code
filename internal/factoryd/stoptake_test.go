package factoryd

// The stop's taking of the session's work (takeStopWork, wippush.go): a
// wall-clock stop whose worker's end push does not land takes the work
// itself through the WIP machinery within the pod's remaining grace, on
// the same seams the over-bound refusal's tests use (wippush_test.go's
// TestEndPushRefusedOverTheBound tests).

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A stopped worker whose end push never lands is not the end of its
// work: the stop asks the pod for the work it holds and ships it, and
// the outcome carries the pushed head and names where the work sits.
func TestStopTakesTheWorkWhenNoEndPushLands(t *testing.T) {
	runner, objects, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
	})
	// the log's follower reads the stopped pod once more, as the take does
	runner.kube.(*fakeKube).keepDeleted = true
	runner.stopWait = 3 * time.Second
	setDetectors(runner, map[string]int{policy.DetectorNoProgress: 3})
	setLimits(runner, func(l *policy.Limits) { l.AgentTime = 300 * time.Millisecond })
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		envLine(harness.Event{Kind: harness.EventModelCall, Time: time.Now()})+"\n"+sigtermLogs)
	if err != nil || res.Outcome != OutcomeWallClock {
		t.Fatalf("result = (%+v, %v), want a wall-clock stop", res, err)
	}
	tip := wipRun(t, "-C", serve.fork, "rev-parse", "refs/heads/factory/t2")
	if tip == serve.base || res.Commit != tip {
		t.Fatalf("branch %s, result commit %q; want the stop's take pushed and its head in the result", tip, res.Commit)
	}
	if want := stopTakenNote + " factory/t2 at " + tip; len(res.Findings) == 0 || res.Findings[0] != want {
		t.Fatalf("findings = %q, want %q first, for the successor", res.Findings, want)
	}
	if n := wipSteps(t, objects, "t2", "wip_pushed"); n != 1 {
		t.Fatalf("wip_pushed steps = %d, want the stop's one take", n)
	}
	serve.mu.Lock()
	defer serve.mu.Unlock()
	if len(serve.commits) != 1 {
		t.Fatalf("the pod shipped %d WIPs, want the stop's single ask", len(serve.commits))
	}
}

// Where no push can land the stop's work is no silent loss: the
// outcome's first finding says the work was not pushed and why, beside
// the stopped worker's own words, and no commit is claimed.
func TestStopThatCannotTakeTheWorkNamesItOnTheOutcome(t *testing.T) {
	runner, _, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
	})
	serve.mu.Lock()
	serve.wipStatus = http.StatusInternalServerError
	serve.mu.Unlock()
	// the log's follower reads the stopped pod once more, as the take does
	runner.kube.(*fakeKube).keepDeleted = true
	runner.stopWait = 3 * time.Second
	setDetectors(runner, map[string]int{policy.DetectorNoProgress: 3})
	setLimits(runner, func(l *policy.Limits) { l.AgentTime = 300 * time.Millisecond })
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t2", Attempt: 1, Model: "m"}, nil,
		envLine(harness.Event{Kind: harness.EventModelCall, Time: time.Now()})+"\n"+sigtermLogs)
	if err != nil || res.Outcome != OutcomeWallClock {
		t.Fatalf("result = (%+v, %v), want a wall-clock stop", res, err)
	}
	if res.Commit != "" {
		t.Fatalf("result commit = %q, want none: no push landed", res.Commit)
	}
	if len(res.Findings) < 2 || !strings.HasPrefix(res.Findings[0], stopUnpushed) || !strings.Contains(res.Findings[0], "the ask of the pod failed") {
		t.Fatalf("findings = %q, want %q first, naming the failed ask", res.Findings, stopUnpushed)
	}
}

// A stop of a session that holds nothing to take — an empty startRef,
// nothing asked — asks nothing and waits no ask-bound: the empty-bundle
// early return keeps a stop of such a session as short as the stop
// itself.
func TestStopOfASessionWithNothingToTakeAsksNothing(t *testing.T) {
	runner, _, serve := wipRig(t, nil, func(dir string) {
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\n"), 0o644)
	})
	runner.kube.(*fakeKube).pods["ns/pod"] = &fakePod{ns: "ns", name: "pod", phase: "Running"}
	spec := sessionSpec{id: "s-1", startRef: "", repo: Repo{Owner: "sf", Name: "repo"}, messageToken: "tok"}
	began := time.Now()
	out := runner.takeStopWork(context.Background(), supervisedTask("t2"), spec, "ns", "pod", nil, time.Now())
	if out.head != "" || out.unpushed != "" || out.refusal != "" || out.askFailed {
		t.Fatalf("outcome = %+v, want nothing taken", out)
	}
	if time.Since(began) > time.Second {
		t.Fatalf("the take of nothing took %s, want the early return without the ask-bound", time.Since(began))
	}
	if order := serve.order.all(); len(order) != 0 {
		t.Fatalf("the pod was asked %q, want no ask of a session that holds nothing", order)
	}
}
