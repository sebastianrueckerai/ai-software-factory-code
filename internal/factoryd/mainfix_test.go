package factoryd

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// A red push run of main pauses the project's merge queue once and files
// the fix-forward card, named after the failed jobs, when any of them
// failed a step, though another lost its runner; the next poll files no
// second card.
func TestRollerPausesTheQueueOnARedMain(t *testing.T) {
	test, image := "/"+codeRepo+"/actions/runs/0/jobs/0", "/"+codeRepo+"/actions/runs/0/jobs/1"
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(58, commitC2, "failure"), pushRun(57, commitC1, "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		registry: registryOf(map[string]string{"sf/factory-gateway/manifests/c1c1c1c": gwOldIdx, "sf/opencode-worker/manifests/c1c1c1c": workerOldIdx, "sf/ci-runner/manifests/c1c1c1c": runnerIdx}, gwOldIdx, workerOldIdx, runnerIdx),
		statuses: map[string][]map[string]string{commitC2: {{"status": "failure", "context": "ci / test (push)", "target_url": test}, {"status": "failure", "context": "ci / image (push)", "target_url": image}}},
		logs:     map[string]string{test: readTestdata(t, "joblog-run1208-test.log"), image: readTestdata(t, "joblog-run1198-image.log")},
	}
	roller, store, _ := newTestRoller(t, f)
	roller.Forgejo.PublicURL = "https://git.example"
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
	if len(f.issues) != 1 || !strings.HasPrefix(f.issues[0], "main is red at c2c2c2c: ci / test (push), ci / image (push)\n\n") || !strings.Contains(f.issues[0], "run 0 of "+commitC2) ||
		!strings.Contains(f.issues[0], "The run: https://git.example/"+codeRepo+"/actions/runs/0.") {
		t.Fatalf("issues %q, want the one fix-forward card", f.issues)
	}
	fix := store.tasks[pause.FixTask]
	if fix.Source != SourceMainFix || fix.Lane != "ready" || fix.Status != "queued" || fix.SHA != commitC2 || fix.Kind != "agent" {
		t.Fatalf("fix card %+v, want a ready coder card on the red commit", fix)
	}
}

// Main's nightly run reads as a push run to Forgejo: red at 03:00 on a
// roll merge whose push run was green, it filed a fix card and two hours
// later the revert of the roll (cards #804 and #806). A scheduled run says
// nothing of main's push CI: no pause, no card.
func TestRollerTakesNoScheduledRunForMainsCI(t *testing.T) {
	nightly := pushRun(59, commitC2, "failure")
	nightly["trigger_event"] = "schedule"
	f := &rollForgejo{
		runs:     []map[string]any{nightly, pushRun(58, commitC2, "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		registry: registryOf(map[string]string{"sf/factory-gateway/manifests/c2c2c2c": gwOldIdx, "sf/opencode-worker/manifests/c2c2c2c": workerOldIdx, "sf/ci-runner/manifests/c2c2c2c": runnerIdx}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, _ := newTestRoller(t, f)
	store.tasks["t-roll0"] = Task{ID: "t-roll0", Source: SourceRoll, Status: "done", Repository: codeRepo, PRNumber: 14}
	for range 2 {
		if err := roller.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if pause, ok := store.pauses[codeRepo]; ok || len(f.issues) != 0 {
		t.Fatalf("pause %+v (%v), issues %q; want none: the red run is the nightly schedule", pause, ok, f.issues)
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
	store.tasks["fix1"] = Task{ID: "fix1", Source: SourceMainFix, Kind: "agent", Status: "in-review", Repository: codeRepo, IssueNumber: 40}
	ctx := context.Background()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.pauses[codeRepo]; ok {
		t.Fatal("the pause survived main's green run")
	}
	if len(f.comments) != 0 || len(f.closed) != 0 {
		t.Fatalf("comments %q, closed issues %q; the fix-forward card ends with its merge", f.comments, f.closed)
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

// A red push run whose every failed job lost its runner pauses the queue
// as any red run does, but files a human card, no agent's: it names the
// run's job, the runner's line and the re-run. The job's log runs past
// the 1 MiB factoryd reads: its tail tells. Past the bound the pause
// reverts nothing, the commit not being at fault; while the re-run runs
// it holds; the re-run that turns the run green ends the pause and closes
// the card.
func TestRollerFilesARerunCardWhenMainLostItsRunner(t *testing.T) {
	job := "/" + codeRepo + "/actions/runs/1208/jobs/0"
	run := func(status string) map[string]any {
		r := pushRun(58, commitC2, status)
		r["index_in_repo"] = 1208
		return r
	}
	f := &rollForgejo{
		runs:  []map[string]any{run("failure"), pushRun(57, commitC1, "success")},
		files: map[string]map[string]string{codeRepo + "@main": mainPins()},
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwOldIdx, "sf/opencode-worker/manifests/c1c1c1c": workerOldIdx, "sf/ci-runner/manifests/c1c1c1c": runnerIdx,
			"sf/factory-gateway/manifests/c2c2c2c": gwOldIdx, "sf/opencode-worker/manifests/c2c2c2c": workerOldIdx, "sf/ci-runner/manifests/c2c2c2c": runnerIdx,
		}, gwOldIdx, workerOldIdx, runnerIdx),
		statuses: map[string][]map[string]string{commitC2: {
			{"status": "failure", "context": "ci / test (push)", "target_url": job},
			{"status": "success", "context": "ci / image (push)", "target_url": "/" + codeRepo + "/actions/runs/1208/jobs/1"},
		}},
		// 30000 lines of 40 bytes ahead of run 1208's tail
		logs: map[string]string{job: strings.Repeat("2026-10-14T20:28:00.0000000Z #31 filler\n", 30000) + readTestdata(t, "joblog-run1208-test.log")},
	}
	roller, store, wf := newTestRoller(t, f)
	roller.Forgejo.PublicURL = "https://git.example"
	store.tasks["t-roll0"] = Task{ID: "t-roll0", Source: SourceRoll, Status: "done", Repository: codeRepo, PRNumber: 14}
	store.tasks["w1"] = Task{ID: "w1", Status: "in-review", Repository: codeRepo, Gate: GateMergeQueue}
	store.queue = append(store.queue, queueEntry{project: codeRepo, task: "w1", at: time.Now()})
	ctx := context.Background()
	for range 2 {
		if err := roller.poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	pause, ok := store.pauses[codeRepo]
	if !ok || pause.RunID != 58 || pause.SHA != commitC2 || pause.FixTask == "" || pause.RevertTask != "" {
		t.Fatalf("pause = %+v (%v), want run 58's with its card", pause, ok)
	}
	if len(f.issues) != 1 || !strings.HasPrefix(f.issues[0], "main is red at c2c2c2c: re-run ci / test (push), its runner was lost.\n\n```\nmain is red at c2c2c2c: re-run ci / test (push), its runner was lost.\n```\n\nWhy: Main CI's push run 1208 of "+commitC2+" failed, but none of its steps did") ||
		!strings.Contains(f.issues[0], "ci / test (push), https://git.example"+job+":\n\n```\n\u2699\ufe0f [runner]: context canceled\\nGet \"https://127.0.0.1:2376/v1.51/containers/6e2dfaae97bc") ||
		!strings.Contains(f.issues[0], "Re-run each job from its page.") {
		t.Fatalf("issues %q, want the one re-run card", f.issues)
	}
	fix := store.tasks[pause.FixTask]
	if fix.Source != SourceRerun || fix.Kind != "human" || fix.Status != "waiting-human" || fix.Lane != "" || fix.SHA != commitC2 {
		t.Fatalf("card %+v, want a human card on the red commit", fix)
	}
	if len(wf.started) != 0 {
		t.Fatalf("workflows started %q, want none", wf.started)
	}

	// the bound passes with no re-run: the pause holds, nothing reverts
	pause.Since = time.Now().Add(-mainRedBound - time.Minute)
	store.pauses[codeRepo] = pause
	revertBranch = func(ctx context.Context, fj *Forgejo, repo Repo, sha, branch string) error {
		t.Errorf("reverted %s on %s", sha, branch)
		return nil
	}
	t.Cleanup(func() { revertBranch = nil })
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if p, ok := store.pauses[codeRepo]; !ok || p.RevertTask != "" || len(f.pulls) != 0 || len(f.issues) != 1 {
		t.Fatalf("pause = %+v (%v), pull requests %q, issues %d; want the pause alone", p, ok, f.pulls, len(f.issues))
	}

	// the re-run runs: run 57's green says nothing of main's head
	f.runs = []map[string]any{run("running"), pushRun(57, commitC1, "success")}
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.pauses[codeRepo]; !ok || len(wf.signals) != 0 || len(f.comments) != 0 {
		t.Fatalf("pause held: %v, signals %q, comments %q; want the pause alone", ok, wf.signals, f.comments)
	}

	// the re-run turns run 1208 green
	f.runs = []map[string]any{run("success"), pushRun(57, commitC1, "success")}
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.pauses[codeRepo]; ok {
		t.Fatal("the pause survived the re-run's green")
	}
	if !slices.Contains(wf.signals, "w1 queue_turn  #0: ") {
		t.Fatalf("signals = %q, want the waiting head woken", wf.signals)
	}
	want := "31 factoryd: main's push run 1208 at " + commitC2 + " is green (https://git.example/" + codeRepo + "/actions/runs/1208): the merge queue's pause has ended. Closing this issue."
	if !slices.Equal(f.comments, []string{want}) || !slices.Equal(f.closed, []string{"31"}) {
		t.Fatalf("comments %q, closed issues %q; want the card's issue closed with %q", f.comments, f.closed, want)
	}
	if !slices.ContainsFunc(store.intakeEvents, func(e IntakeEvent) bool {
		return e.TaskID == fix.ID && e.Kind == IntakeFactoryClosed && e.CommentID == 31
	}) {
		t.Fatalf("intake events %+v, want factoryd's close of issue 31 recorded", store.intakeEvents)
	}
}

// A runner killed outright writes no line of its own: its job's log stops
// mid-step, and Forgejo fails the job once the runner has been silent for
// its zombie timeout, 10 minutes in production (the test job's record is
// a local Forgejo 15.0.9 and runner 12.7.3 whose runner took SIGKILL; it
// ran with a 1-minute timeout, so the record's time here is production's).
// A dind that restarts under a living runner fails the step it ran, and
// the runner's line names the daemon lost (the image job's, the same
// reproduction). Neither is the commit's fault: the red run files the
// re-run card naming both records, and no agent starts.
func TestRollerFilesARerunCardWhenARunnerWasKilledOrItsDockerRestarted(t *testing.T) {
	test, image := "/"+codeRepo+"/actions/runs/0/jobs/0", "/"+codeRepo+"/actions/runs/0/jobs/1"
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(58, commitC2, "failure"), pushRun(57, commitC1, "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		registry: registryOf(map[string]string{"sf/factory-gateway/manifests/c1c1c1c": gwOldIdx, "sf/opencode-worker/manifests/c1c1c1c": workerOldIdx, "sf/ci-runner/manifests/c1c1c1c": runnerIdx}, gwOldIdx, workerOldIdx, runnerIdx),
		statuses: map[string][]map[string]string{commitC2: {
			{"status": "failure", "context": "ci / test (push)", "target_url": test, "created_at": "2026-10-15T07:13:45Z"},
			{"status": "failure", "context": "ci / image (push)", "target_url": image, "created_at": "2026-10-15T07:04:49Z"},
		}},
		logs: map[string]string{test: readTestdata(t, "joblog-runner-killed.log"), image: readTestdata(t, "joblog-dind-restart.log")},
	}
	roller, store, wf := newTestRoller(t, f)
	roller.Forgejo.PublicURL = "https://git.example"
	store.tasks["t-roll0"] = Task{ID: "t-roll0", Source: SourceRoll, Status: "done", Repository: codeRepo, PRNumber: 14}
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	pause, ok := store.pauses[codeRepo]
	if !ok || pause.RunID != 58 || pause.FixTask == "" {
		t.Fatalf("pause = %+v (%v), want run 58's with its card", pause, ok)
	}
	// 07:13:45 less the last tick's 07:02:43.4584859 is 11m1.54s
	if len(f.issues) != 1 || !strings.HasPrefix(f.issues[0], "main is red at c2c2c2c: re-run ci / test (push), ci / image (push), its runner was lost.\n\n") ||
		!strings.Contains(f.issues[0], "ci / test (push), https://git.example"+test+": Forgejo failed it at 2026-10-15T07:13:45Z, 11m2s after its log's last line, its runner silent since:\n\n```\ntick 38\n```") ||
		!strings.Contains(f.issues[0], "ci / image (push), https://git.example"+image+":\n\n```\n\u2699\ufe0f [runner]: failed to inspect exec: Cannot connect to the Docker daemon at tcp://127.0.0.1:2376.") {
		t.Fatalf("issues %q, want the one re-run card naming both records", f.issues)
	}
	if fix := store.tasks[pause.FixTask]; fix.Source != SourceRerun || fix.Kind != "human" || fix.Status != "waiting-human" {
		t.Fatalf("card %+v, want a human re-run card", fix)
	}
	if len(wf.started) != 0 {
		t.Fatalf("workflows started %q, want none", wf.started)
	}
}

// A runner killed before it uploaded a line leaves its job no log: an
// empty one, or one Forgejo cannot open (actions.Logs answers 500 for a
// log file never written). No line ran, so no code failed: the red run
// files the re-run card, and no agent starts.
func TestRollerFilesARerunCardWhenAJobLeftNoLog(t *testing.T) {
	test, image := "/"+codeRepo+"/actions/runs/0/jobs/0", "/"+codeRepo+"/actions/runs/0/jobs/1"
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(58, commitC2, "failure"), pushRun(57, commitC1, "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		registry: registryOf(map[string]string{"sf/factory-gateway/manifests/c1c1c1c": gwOldIdx, "sf/opencode-worker/manifests/c1c1c1c": workerOldIdx, "sf/ci-runner/manifests/c1c1c1c": runnerIdx}, gwOldIdx, workerOldIdx, runnerIdx),
		statuses: map[string][]map[string]string{commitC2: {
			{"status": "failure", "context": "ci / test (push)", "target_url": test, "created_at": "2026-10-15T07:13:45Z"},
			{"status": "failure", "context": "ci / image (push)", "target_url": image, "created_at": "2026-10-15T07:13:45Z"},
		}},
		logs:       map[string]string{test: ""},
		unreadable: map[string]bool{image: true},
	}
	roller, store, wf := newTestRoller(t, f)
	roller.Forgejo.PublicURL = "https://git.example"
	store.tasks["t-roll0"] = Task{ID: "t-roll0", Source: SourceRoll, Status: "done", Repository: codeRepo, PRNumber: 14}
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	pause, ok := store.pauses[codeRepo]
	if !ok || pause.RunID != 58 || pause.FixTask == "" {
		t.Fatalf("pause = %+v (%v), want run 58's with its card", pause, ok)
	}
	if len(f.issues) != 1 || !strings.HasPrefix(f.issues[0], "main is red at c2c2c2c: re-run ci / test (push), ci / image (push), its runner was lost.\n\n") ||
		!strings.Contains(f.issues[0], "ci / test (push), https://git.example"+test+": its log is empty, its runner lost before the job's first line.") ||
		!strings.Contains(f.issues[0], "ci / image (push), https://git.example"+image+": factoryd cannot read its log (forgejo job log "+image+": status 500); a runner lost before the job's first line leaves none.") {
		t.Fatalf("issues %q, want the one re-run card naming both jobs", f.issues)
	}
	if fix := store.tasks[pause.FixTask]; fix.Source != SourceRerun || fix.Kind != "human" || fix.Status != "waiting-human" {
		t.Fatalf("card %+v, want a human re-run card", fix)
	}
	if len(wf.started) != 0 {
		t.Fatalf("workflows started %q, want none", wf.started)
	}
}

// While main's newest push run waits, an older red one pauses nothing:
// it says nothing of main's head.
func TestRollerWaitsOnMainsNewestRun(t *testing.T) {
	f := &rollForgejo{runs: []map[string]any{pushRun(59, commitC2, "waiting"), pushRun(58, commitC1, "failure")}}
	roller, store, _ := newTestRoller(t, f)
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p, ok := store.pauses[codeRepo]; ok || len(f.issues) != 0 {
		t.Fatalf("pause = %+v (%v), issues %q; want none", p, ok, f.issues)
	}
}

// runnerLost tells a job that lost its runner from one that failed a
// step, on the last lines of real job logs and the time Forgejo recorded
// each failure: run 1208's test job died with its runner's evicted pod,
// run 1199's failed go vet in an image build, run 1198's image job a
// Dockerfile's RUN step; a local reproduction killed one job's runner
// outright and restarted another's dind.
func TestRunnerLost(t *testing.T) {
	for _, c := range []struct {
		file, recorded string
		want           bool
	}{
		{"joblog-run1208-test.log", "2026-10-14T20:42:56Z", true},
		{"joblog-run1199-test.log", "2026-10-14T19:06:36Z", false},
		{"joblog-run1198-image.log", "2026-10-14T19:07:54Z", false},
		// a red step stays the cause though its runner fell silent after
		{"joblog-run1199-test.log", "2026-10-14T19:21:36Z", false},
		{"joblog-dind-restart.log", "2026-10-15T07:04:49Z", true},
		{"joblog-runner-killed.log", "2026-10-15T07:13:45Z", true},
		// the reproduction's own record: its zombie timeout was 1 minute
		{"joblog-runner-killed.log", "2026-10-15T07:03:45Z", false},
	} {
		line, _, lost := runnerLost([]byte(readTestdata(t, c.file)), mustTime(t, c.recorded))
		if lost != c.want || lost != (line != "") {
			t.Errorf("%s recorded %s: runnerLost = %q, %v; want %v", c.file, c.recorded, line, lost, c.want)
		}
	}
	line, silent, _ := runnerLost([]byte(readTestdata(t, "joblog-run1208-test.log")), mustTime(t, "2026-10-14T20:42:56Z"))
	if !strings.HasPrefix(line, "\u2699\ufe0f [runner]: context canceled\\nGet \"https://127.0.0.1:2376/v1.51/containers/6e2dfaae97bc") ||
		!strings.HasSuffix(line, "path=%2Fvar%2Frun%2Fact%2Fworkflow%2Fpathcmd.txt\": context canceled") || silent != 0 {
		t.Errorf("run 1208's line = %q, silent %s; want its runner's, timestamp stripped", line, silent)
	}
	line, silent, _ = runnerLost([]byte(readTestdata(t, "joblog-dind-restart.log")), mustTime(t, "2026-10-15T07:04:49Z"))
	if !strings.HasPrefix(line, "\u2699\ufe0f [runner]: failed to inspect exec: Cannot connect to the Docker daemon at tcp://127.0.0.1:2376. Is the docker daemon running?\\n") || silent != 0 {
		t.Errorf("the dind restart's line = %q, silent %s; want its runner's", line, silent)
	}
	// 07:13:45 less the last tick's 07:02:43.4584859
	line, silent, _ = runnerLost([]byte(readTestdata(t, "joblog-runner-killed.log")), mustTime(t, "2026-10-15T07:13:45Z"))
	if want := 11*time.Minute + 1541514100*time.Nanosecond; line != "tick 38" || silent != want {
		t.Errorf("the killed runner's job: line %q, silent %s; want the last tick, %s", line, silent, want)
	}
	for log, want := range map[string]bool{
		// runner 13's host executor (job 5 of run 1148) words the cancelled
		// step its own way
		"2026-10-14T17:07:21.1244570Z \u2699\ufe0f [runner]: this step has been cancelled: ctx: context canceled, exec: RUN signal: killed\n2026-10-14T17:07:22.2178401Z \U0001F3C1  Job failed\n": true,
		// a step past its timeout-minutes failed on its own
		"2026-10-14T17:07:21.1244570Z \u2699\ufe0f [runner]: this step has been cancelled: ctx: context deadline exceeded, exec: RUN signal: killed\n": false,
		// a red step, then an always() step whose runner was lost
		readTestdata(t, "joblog-run1199-test.log") + "2026-10-14T19:06:36.0000000Z \u2699\ufe0f [runner]: context canceled\n": false,
		// the same step output, no runner line
		"2026-10-14T20:29:01.7686760Z context canceled\n": false,
		// a runner lost before its first line: no line ran
		"":   true,
		"\n": true,
		// dind back before the runner's next call: the daemon's answers
		// for the job's stopped container and its lost exec
		"2026-10-15T07:04:50.0000000Z \u2699\ufe0f [runner]: failed to create exec: Error response from daemon: container 3f2a is not running\n": true,
		"2026-10-15T07:04:50.0000000Z \u2699\ufe0f [runner]: failed to inspect exec: Error response from daemon: No such exec instance: 9b1c\n":  true,
	} {
		if _, _, lost := runnerLost([]byte(log), time.Time{}); lost != want {
			t.Errorf("runnerLost(%q) = %v, want %v", log, lost, want)
		}
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
