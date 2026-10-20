package factoryd

import (
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A merge is not done: the card moves to merged and its run waits,
// cost-free, for the chain to cover it. The poller's signal ends the
// wait — done names the run that covers it — and only then does the
// task end: one Finish, after the signal, so EventTaskEnded and the
// done poke fire at live, never at merge; no stall while the chain
// runs inside the bound.
func TestMergeWaitsForTheChain(t *testing.T) {
	g := newGateHarness(t, autoGates)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.onChainFacts(
		ChainFacts{Mode: policy.ChainModeRun, Stall: 2 * time.Hour},
		ChainFacts{Mode: policy.ChainModeRun, Stall: 2 * time.Hour, Live: true, RunID: 41})
	g.signalAfter(time.Hour, SignalChainCovered, TaskSignal{Author: FactoryDecider})

	if final := g.run(t); final != "done" || g.finals[0][1] != "live: covered by run 41" {
		t.Fatalf("final = %q, finals = %v; want done, live by run 41", final, g.finals)
	}
	if !slices.Contains(g.states, "merged") {
		t.Fatalf("states = %v; want the merged move", g.states)
	}
	if g.finishAt.Before(g.started.Add(time.Hour)) {
		t.Fatalf("finished at %s, before the covered signal at %s", g.finishAt, g.started.Add(time.Hour))
	}
	if g.stalls != 0 {
		t.Fatalf("stalls = %d; want none inside the stall bound", g.stalls)
	}
}

// A chain that concluded while factoryd was down decides on entry: the
// wait reads the recorded facts before it waits.
func TestAMergeTheChainAlreadyCoversIsDoneAtOnce(t *testing.T) {
	g := newGateHarness(t, autoGates)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	// the harness's default facts: the chain is live, run 41

	if final := g.run(t); final != "done" || g.finals[0][1] != "live: covered by run 41" {
		t.Fatalf("final = %q, finals = %v; want done, live by run 41", final, g.finals)
	}
	if !slices.Contains(g.states, "merged") {
		t.Fatalf("states = %v; want the merged move", g.states)
	}
}

// A project whose chain is none is done at its merge, exactly as a
// project without a deploy: no merged state, the merge's own reason.
func TestAMergeWithoutAChainIsDoneAtMerge(t *testing.T) {
	g := newGateHarness(t, autoGates)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.onChainFacts(ChainFacts{Mode: chainModeNone})

	if final := g.run(t); final != "done" || g.finals[0][1] != "pull request merged by factory-merge on the merge rule auto" {
		t.Fatalf("final = %q, finals = %v; want done at the merge", final, g.finals)
	}
	if slices.Contains(g.states, "merged") {
		t.Fatalf("states = %v; a chain of none never merges the card", g.states)
	}
}

// A merged card never turns terminal on a stall: the workflow's own
// timer wakes it past the bound, the stall is work for Task 5's repair,
// and the wait re-arms and reads the facts again — the chain green by
// then ends the card done.
func TestAMergedCardOutlivesAStalledChain(t *testing.T) {
	g := newGateHarness(t, autoGates)
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.onChainFacts(
		ChainFacts{Mode: policy.ChainModeRun, Stall: 30 * time.Minute},
		ChainFacts{Mode: policy.ChainModeRun, Stall: 30 * time.Minute, Live: true, RunID: 41})

	if final := g.run(t); final != "done" || g.finals[0][1] != "live: covered by run 41" {
		t.Fatalf("final = %q, finals = %v; want done once the chain covers it", final, g.finals)
	}
	if g.stalls != 1 {
		t.Fatalf("stalls = %d; want one stall inside the wait", g.stalls)
	}
}

// A run restarted mid-wait (factoryd came back, the history gone) enters
// the same wait through the heal path: reconcile finds the pull request
// merged, the entry facts are read again, and the covered signal ends it
// — one Finish in all, so the done poke fires once however often the
// run restarted.
func TestARestartedRunWaitsForTheChainAgain(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "merged", Merged: true, PR: PullRequest{Number: 9}}
	h.onChainFacts(
		ChainFacts{Mode: policy.ChainModeRun, Stall: 2 * time.Hour},
		ChainFacts{Mode: policy.ChainModeRun, Stall: 2 * time.Hour, Live: true, RunID: 41})
	h.signalAfter(time.Hour, SignalChainCovered, TaskSignal{Author: FactoryDecider})

	if final := h.run(t); final != "done" || h.finals[0][1] != "live: covered by run 41" {
		t.Fatalf("final = %q, finals = %v; want done, live by run 41", final, h.finals)
	}
	if len(h.sessions) != 0 || !slices.Contains(h.states, "merged") {
		t.Fatalf("sessions = %d, states = %v; want no session and the merged move", len(h.sessions), h.states)
	}
}
