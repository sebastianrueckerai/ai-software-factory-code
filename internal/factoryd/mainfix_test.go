package factoryd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// A red push run of main pauses the project's merge queue once and files
// the fix-forward card, named after the failed jobs; the next poll files
// no second card.
func TestRollerPausesTheQueueOnARedMain(t *testing.T) {
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(58, commitC2, "failure"), pushRun(57, commitC1, "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		registry: registryOf(map[string]string{"sf/factory-gateway/manifests/c1c1c1c": gwOldIdx, "sf/opencode-worker/manifests/c1c1c1c": workerOldIdx, "sf/ci-runner/manifests/c1c1c1c": runnerIdx}, gwOldIdx, workerOldIdx, runnerIdx),
		statuses: map[string][]map[string]string{commitC2: {{"status": "failure", "context": "ci / test (push)"}, {"status": "success", "context": "ci / image (push)"}}},
	}
	roller, store, _ := newTestRoller(t, f)
	store.tasks["t-roll0"] = Task{ID: "t-roll0", Source: SourceRoll, Status: "done", Repository: codeRepo, PRNumber: 14}
	ctx := context.Background()
	for range 2 {
		if err := roller.poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	pause, ok := store.pauses[codeRepo]
	if !ok || pause.RunID != 58 || pause.SHA != commitC2 || pause.FixTask == "" || pause.RevertTask != "" {
		t.Fatalf("pause = %+v (%v), want run 58's with its fix card", pause, ok)
	}
	if len(f.issues) != 1 || !strings.HasPrefix(f.issues[0], "main is red at c2c2c2c: ci / test (push)\n\n") || !strings.Contains(f.issues[0], "run 0 of "+commitC2) {
		t.Fatalf("issues %q, want the one fix-forward card", f.issues)
	}
	fix := store.tasks[pause.FixTask]
	if fix.Source != SourceMainFix || fix.Lane != "ready" || fix.Status != "queued" || fix.SHA != commitC2 || fix.Kind != "agent" {
		t.Fatalf("fix card %+v, want a ready coder card on the red commit", fix)
	}
}

// A green push run of main ends the pause and wakes the queue's waiting
// head; a pause past the bound with no revert yet files the revert as a
// factory-PR card on a branch of the bot's fork.
func TestRollerEndsThePauseOnGreenAndRevertsAfterTheBound(t *testing.T) {
	f := &rollForgejo{
		runs:  []map[string]any{pushRun(59, commitC2, "success"), pushRun(58, commitC1, "failure")},
		files: map[string]map[string]string{codeRepo + "@main": mainPins()},
		registry: registryOf(map[string]string{"sf/factory-gateway/manifests/c2c2c2c": gwOldIdx, "sf/opencode-worker/manifests/c2c2c2c": workerOldIdx, "sf/ci-runner/manifests/c2c2c2c": runnerIdx},
			gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, wf := newTestRoller(t, f)
	store.tasks["t-roll0"] = Task{ID: "t-roll0", Source: SourceRoll, Status: "done", Repository: codeRepo, PRNumber: 14}
	store.tasks["w1"] = Task{ID: "w1", Status: "in-review", Repository: codeRepo, Gate: GateMergeQueue}
	store.queue = append(store.queue, queueEntry{project: codeRepo, task: "w1", at: time.Now()})
	store.pauses = map[string]QueuePause{codeRepo: {Project: codeRepo, RunID: 58, SHA: commitC1, Since: time.Now().Add(-time.Hour), FixTask: "fix1"}}
	ctx := context.Background()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.pauses[codeRepo]; ok {
		t.Fatal("the pause survived main's green run")
	}
	if !slices.Contains(wf.signals, "w1 queue_turn  #0: ") {
		t.Fatalf("signals = %q, want the waiting head woken", wf.signals)
	}

	// the bound: no fix landed in two hours
	f.runs = []map[string]any{pushRun(58, commitC1, "failure")}
	store.pauses = map[string]QueuePause{codeRepo: {Project: codeRepo, RunID: 58, SHA: commitC1, Since: time.Now().Add(-mainRedBound - time.Minute), FixTask: "fix1"}}
	var reverted []string
	revertBranch = func(ctx context.Context, fj *Forgejo, repo Repo, sha, branch string) error {
		reverted = append(reverted, sha+" "+branch)
		return nil
	}
	t.Cleanup(func() { revertBranch = nil })
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	pause := store.pauses[codeRepo]
	if pause.RevertTask == "" {
		t.Fatalf("pause = %+v, want its revert card", pause)
	}
	if want := commitC1 + " factory/" + pause.RevertTask; !slices.Equal(reverted, []string{want}) {
		t.Fatalf("reverted %q, want %q", reverted, want)
	}
	if len(f.pulls) != 1 || !strings.HasPrefix(f.pulls[0], "factory-agent:factory/"+pause.RevertTask+" Revert c1c1c1c: main red since ") {
		t.Fatalf("pull requests %q, want the revert's from the fork's branch", f.pulls)
	}
	rev := store.tasks[pause.RevertTask]
	if rev.Source != SourceRevert || rev.Lane != "" || rev.PRNumber != 71 || rev.Status != "queued" {
		t.Fatalf("revert card %+v, want a factory-PR card linked to pull request 71", rev)
	}
	// the pause holds: a second poll files no second revert
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.pulls) != 1 || len(reverted) != 1 {
		t.Fatalf("pull requests %q, reverts %q; want one of each", f.pulls, reverted)
	}
}
