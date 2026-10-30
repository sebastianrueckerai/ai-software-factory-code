package factoryd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const (
	planHeldLog    = "roller: plan #40 is open on main: no roll until no plan is open on main"
	heldStatusDesc = "held: plan #40 is open on main; the roll follows once no plan is open on main"
)

// openPlan opens plan #40 on the code repository's main: its child t-c1
// merged.
func openPlan(store *fakeStore) {
	store.tasks["t-plan"] = Task{ID: "t-plan", IssueNumber: 40, Repository: codeRepo, Track: "plan"}
	store.tasks["t-c1"] = Task{ID: "t-c1", Source: SourcePlan, Parent: "t-plan", PlanKey: "a", Repository: codeRepo, Status: "merged", MergeCommit: mergePR12, FinishedAt: justNow()}
}

// newHeldRoller is TestRollerFilesOneRollPerCommit's rig with plan #40 open
// on main: main's head c2 (run 58) runs with its tests green, c1 (run 57)
// has its images green and changed.
func newHeldRoller(t *testing.T) (*Roller, *rollForgejo, *fakeStore, *fakeWorkflows) {
	t.Helper()
	statuses := imageGreen(commitC1)
	statuses[commitC2] = []map[string]string{{"status": "success", "context": testContext, "target_url": "https://git.example"}}
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(58, commitC2, "running"), pushRun(57, commitC1, "success"), pushRun(56, "c0c0c0c0", "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		statuses: statuses,
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwNewIdx,
			"sf/opencode-worker/manifests/c1c1c1c": workerRebuilt,
			"sf/ci-runner/manifests/c1c1c1c":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx),
		titles: map[int64]string{12: "Keys are no longer blocked at every session (closes #4, @bob)"},
	}
	roller, store, wf := newTestRoller(t, f)
	store.tasks["t-roll0"] = Task{ID: "t-roll0", Source: SourceRoll, Status: "done", Repository: codeRepo, PRNumber: 14, MergeCommit: mergePR14}
	store.tasks["t-12"] = Task{ID: "t-12", Status: "merged", FinishedAt: justNow(), Repository: codeRepo, PRNumber: 12, MergeCommit: mergePR12}
	store.tasks["t-11"] = Task{ID: "t-11", Status: "merged", FinishedAt: justNow(), Repository: codeRepo, PRNumber: 11, MergeCommit: "0b0b"}
	openPlan(store)
	return roller, f, store, wf
}

// rolledC1 fails t unless the store holds exactly one roll, of c1 at run 57
// carrying #12, and its issue is filed.
func rolledC1(t *testing.T, f *rollForgejo, store *fakeStore) {
	t.Helper()
	if len(store.rolls) != 1 || len(f.issues) != 1 {
		t.Fatalf("roll requests %+v, issues %d; want one roll", store.rolls, len(f.issues))
	}
	if req := store.rolls[0]; req.SHA != commitC1 || req.RunID != 57 || req.State != RollOpened || !slices.Equal(req.PRs, []int64{12}) {
		t.Fatalf("roll request %+v, want c1's of run 57, open, carrying #12", req)
	}
}

// While a plan is open on main the roller opens no roll: main's newest
// commit, which it has not judged, gets factory/roll success with a held
// description once, so a merged card ends done, and the log says why once.
// The plan's close lets the next poll roll as ever. A plan that opens next,
// while the same head is still unjudged, is a new hold: marked and said again.
func TestRollerHoldsWhileAPlanIsOpenOnMain(t *testing.T) {
	logs := captureLog(t)
	roller, f, store, wf := newHeldRoller(t)
	ctx := context.Background()
	for poll := 1; poll <= 2; poll++ {
		if err := roller.poll(ctx); err != nil {
			t.Fatal(err)
		}
		if len(f.issues) != 0 || len(store.rolls) != 0 || len(wf.started) != 0 {
			t.Fatalf("poll %d: issues %q, rolls %+v, started %v; want no roll while plan #40 is open", poll, f.issues, store.rolls, wf.started)
		}
		if !slices.Equal(f.reported, []string{commitC2 + " success " + rollStatusContext}) {
			t.Fatalf("poll %d: reported %q, want c2 marked held once", poll, f.reported)
		}
		if n := strings.Count(logs.String(), planHeldLog); n != 1 {
			t.Fatalf("poll %d: the hold logged %d times, want once: %q", poll, n, logs.String())
		}
	}
	posted := f.statuses[commitC2][len(f.statuses[commitC2])-1]
	if posted["context"] != rollStatusContext || posted["description"] != heldStatusDesc {
		t.Fatalf("the held status %v, want %q", posted, heldStatusDesc)
	}

	store.closed["t-plan"] = time.Now()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	rolledC1(t, f, store)

	// plan #41 opens while c2, still unjudged, is main's head: a new hold
	store.tasks["t-plan2"] = Task{ID: "t-plan2", IssueNumber: 41, Repository: codeRepo, Track: "plan"}
	store.tasks["t-d1"] = Task{ID: "t-d1", Source: SourcePlan, Parent: "t-plan2", PlanKey: "a", Repository: codeRepo, Status: "merged", MergeCommit: "d1d1", FinishedAt: justNow()}
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if want := []string{commitC2 + " success " + rollStatusContext, commitC1 + " pending " + rollStatusContext, commitC2 + " success " + rollStatusContext}; !slices.Equal(f.reported, want) {
		t.Fatalf("reported %q, want %q: c2 marked held again", f.reported, want)
	}
	if n := strings.Count(logs.String(), "roller: plan #41 is open on main: no roll until no plan is open on main"); n != 1 {
		t.Fatalf("plan #41's hold logged %d times, want once: %q", n, logs.String())
	}
}

// A free poll forgets the held mark before anything can fail it: after a
// free poll whose push runs Forgejo would not list, plan #41 opening on the
// same unjudged head marks it again.
func TestRollerFreePollThatFailsForgetsTheHeldMark(t *testing.T) {
	roller, f, store, _ := newHeldRoller(t)
	ctx := context.Background()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	store.closed["t-plan"] = time.Now()
	f.runsDown = true
	if err := roller.poll(ctx); err == nil {
		t.Fatal("the free poll read the push runs Forgejo refused")
	}
	f.runsDown = false
	store.tasks["t-plan2"] = Task{ID: "t-plan2", IssueNumber: 41, Repository: codeRepo, Track: "plan"}
	store.tasks["t-d1"] = Task{ID: "t-d1", Source: SourcePlan, Parent: "t-plan2", PlanKey: "a", Repository: codeRepo, Status: "merged", MergeCommit: "d1d1", FinishedAt: justNow()}
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if want := []string{commitC2 + " success " + rollStatusContext, commitC2 + " success " + rollStatusContext}; !slices.Equal(f.reported, want) {
		t.Fatalf("reported %q, want %q: c2 marked again for plan #41", f.reported, want)
	}
	if len(store.rolls) != 0 || len(f.issues) != 0 {
		t.Fatalf("rolls %+v, issues %q; want none", store.rolls, f.issues)
	}
}

// The held status spares a commit the roller has judged: a failed roll's
// failure and an open roll's pending stand. No roll is filed, and the log
// says why once.
func TestRollerHeldStatusSparesAJudgedCommit(t *testing.T) {
	for name, judged := range map[string]func(*fakeStore){
		"failed": func(s *fakeStore) {
			s.rolls = []RollRequest{{SHA: commitC2, RunID: 58, Repository: codeRepo, State: RollFailed}}
		},
		"open": func(s *fakeStore) {
			s.tasks["t-roll2"] = Task{ID: "t-roll2", Source: SourceRoll, Status: "running", Repository: codeRepo}
			s.rolls = []RollRequest{{SHA: commitC2, RunID: 58, Repository: codeRepo, State: RollOpened, TaskID: "t-roll2"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureLog(t)
			roller, f, store, wf := newHeldRoller(t)
			judged(store)
			if err := roller.poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.reported) != 0 {
				t.Fatalf("reported %q, want c2's own verdict to stand", f.reported)
			}
			if len(f.issues) != 0 || len(store.rolls) != 1 || len(wf.started) != 0 {
				t.Fatalf("issues %q, rolls %+v, started %v; want no roll", f.issues, store.rolls, wf.started)
			}
			if n := strings.Count(logs.String(), planHeldLog); n != 1 {
				t.Fatalf("the hold logged %d times, want once: %q", n, logs.String())
			}
		})
	}
}

// Deploy now's commit rolls while held, and main's newest commit is marked
// held. Deploy now on a commit whose images are not green rolls nothing.
// With no plan open the release row narrows nothing: the newest green run
// rolls, whatever commit a stale Deploy now names.
func TestRollerRollsTheDeployNowCommitWhileHeld(t *testing.T) {
	t.Run("held", func(t *testing.T) {
		roller, f, store, _ := newHeldRoller(t)
		store.releases = map[string]Release{codeRepo: {Repository: codeRepo, DeployNow: commitC1}}
		if err := roller.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		rolledC1(t, f, store)
		if !slices.Equal(f.reported, []string{commitC2 + " success " + rollStatusContext, commitC1 + " pending " + rollStatusContext}) {
			t.Fatalf("reported %q, want c2 held and c1's roll pending", f.reported)
		}
	})
	t.Run("not green", func(t *testing.T) {
		roller, f, store, _ := newHeldRoller(t)
		store.releases = map[string]Release{codeRepo: {Repository: codeRepo, DeployNow: commitC2}}
		if err := roller.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(f.issues) != 0 || len(store.rolls) != 0 || len(f.reported) != 0 {
			t.Fatalf("issues %q, rolls %+v, reported %q; want nothing: c2's images are not green and it is Deploy now's", f.issues, store.rolls, f.reported)
		}
	})
	for _, deploy := range []string{commitC1, commitC2} {
		t.Run("closed "+deploy[:2], func(t *testing.T) {
			roller, f, store, _ := newHeldRoller(t)
			store.releases = map[string]Release{codeRepo: {Repository: codeRepo, DeployNow: deploy}}
			store.closed["t-plan"] = time.Now()
			if err := roller.poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			rolledC1(t, f, store)
			if !slices.Equal(f.reported, []string{commitC1 + " pending " + rollStatusContext}) {
				t.Fatalf("reported %q, want c1's roll pending alone", f.reported)
			}
		})
	}
}

// A held status Forgejo refuses is posted again by the next poll, its error
// logged once per text; the poll goes on, so Deploy now's commit rolls in
// the poll whose mark failed. The hold is said once the mark lands.
func TestRollerPostsARefusedHeldStatusAgain(t *testing.T) {
	logs := captureLog(t)
	roller, f, store, _ := newHeldRoller(t)
	store.releases = map[string]Release{codeRepo: {Repository: codeRepo, DeployNow: commitC1}}
	f.statusFails = map[string]int{commitC2: 2}
	refused := "roller: plan #40 is open on main: mark " + commitC2 + " held: "
	ctx := context.Background()
	for poll := 1; poll <= 2; poll++ {
		if err := roller.poll(ctx); err != nil {
			t.Fatalf("poll %d: %v, want the poll to go on", poll, err)
		}
		rolledC1(t, f, store)
		if !slices.Equal(f.reported, []string{commitC1 + " pending " + rollStatusContext}) {
			t.Fatalf("poll %d: reported %q, want c1's roll pending alone: c2's mark refused", poll, f.reported)
		}
		if n := strings.Count(logs.String(), refused); n != 1 || strings.Contains(logs.String(), planHeldLog) {
			t.Fatalf("poll %d: the refusal logged %d times, want once, and the hold not yet: %q", poll, n, logs.String())
		}
	}
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.reported, []string{commitC1 + " pending " + rollStatusContext, commitC2 + " success " + rollStatusContext}) {
		t.Fatalf("reported %q, want c2 marked held at the third poll", f.reported)
	}
	if n := strings.Count(logs.String(), planHeldLog); n != 1 {
		t.Fatalf("the hold logged %d times, want once: %q", n, logs.String())
	}
}

// A retry answered while held resets the ladder and says the fresh roll
// waits for the hold; no roll is filed. The retried commit, unjudged again,
// is marked held.
func TestRollerRetryWhileHeldSaysWhen(t *testing.T) {
	f := &rollForgejo{
		runs:     []map[string]any{pushRun(52, commitC1, "success")},
		files:    map[string]map[string]string{codeRepo + "@main": mainPins()},
		statuses: imageGreen(commitC1),
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwNewIdx,
			"sf/opencode-worker/manifests/c1c1c1c": workerRebuilt,
			"sf/ci-runner/manifests/c1c1c1c":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, wf := newTestRoller(t, f)
	openPlan(store)
	ctx := context.Background()
	seed := func(id, sha string, run int64, request string) {
		store.tasks[id] = Task{ID: id, Source: SourceRoll, Status: "failed", Repository: codeRepo, IssueNumber: run - 20}
		store.rolls = append(store.rolls, RollRequest{SHA: sha, Repository: codeRepo, RunID: run, State: request, TaskID: id})
	}
	seed("t-r1", "1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f", 50, RollFailed)
	seed("t-r2", "2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f", 51, RollFailed)
	seed("t-r3", commitC1, 52, RollOpened) // ends this poll
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.reported, []string{commitC1 + " failure " + rollStatusContext}) {
		t.Fatalf("reported %q, want c1's failed roll alone", f.reported)
	}
	var ask Task
	for _, t := range store.tasks {
		if t.Kind == "human" && strings.Contains(t.Objective, rollAskMarker) {
			ask = t
		}
	}
	if ask.IssueNumber != 31 {
		t.Fatalf("the question's card %+v, want issue #31", ask)
	}
	store.intakeEvents = append(store.intakeEvents,
		IntakeEvent{TaskID: ask.ID, Kind: "issue_comment", Author: "sebastian", Body: "Retry the roll", Trusted: true})
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	want := "31 factoryd: retry ordered: the failed attempts' bound is reset and one roll task of main's newest green images starts once no plan is open on main. Closing this issue."
	if !slices.Equal(f.closed, []string{"31"}) || !slices.Equal(f.comments, []string{want}) {
		t.Fatalf("closed %v, comments %q; want #31 closed with %q", f.closed, f.comments, want)
	}
	if len(store.rolls) != 0 || len(wf.started) != 0 || len(f.issues) != 1 {
		t.Fatalf("roll requests %+v, started %v, issues %d; want the ladder reset and no roll", store.rolls, wf.started, len(f.issues))
	}
	if !slices.Equal(f.reported, []string{commitC1 + " failure " + rollStatusContext, commitC1 + " success " + rollStatusContext}) {
		t.Fatalf("reported %q, want c1 marked held once the retry unjudged it", f.reported)
	}
}

// A red main during a hold: mainState pauses the queue and re-runs the
// failed job once, as with no plan open, and files no card; the red head,
// unjudged, is marked held, since the chain reads its runs beside the mark.
// The re-run's green ends the pause and wakes the queue; the head is not
// marked again, and c1's changed images, which roll with no plan open (the
// head's tests are green), do not roll.
func TestRollerHeldMainRedPausesAndReRunsAsEver(t *testing.T) {
	logs := captureLog(t)
	statuses := imageGreen(commitC1)
	statuses[commitC2] = []map[string]string{{"status": "success", "context": testContext, "target_url": "https://git.example"}}
	run3176 := func(status string) []map[string]any {
		r := pushRun(3090, commitC2, status)
		r["index_in_repo"] = 3176
		return []map[string]any{r, pushRun(3089, commitC1, "success")}
	}
	f := &rollForgejo{
		runs:  run3176("failure"),
		files: map[string]map[string]string{codeRepo + "@main": mainPins()},
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwNewIdx, "sf/opencode-worker/manifests/c1c1c1c": workerRebuilt, "sf/ci-runner/manifests/c1c1c1c": runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx),
		statuses: statuses,
		jobs: map[int64][]map[string]any{3090: {
			actionJob(9410, "upstream", "success", 1), actionJob(9411, "test", "success", 1), actionJob(9412, "image", "success", 1),
			actionJob(9413, "image-sandbox", "success", 1), actionJob(9414, "image-runner", "failure", 1),
		}},
	}
	roller, store, wf := newTestRoller(t, f)
	roller.Forgejo.Seeded = map[string]string{policy.SecretCIRegistrar: "actions-tok"}
	store.tasks["t-roll0"] = Task{ID: "t-roll0", Source: SourceRoll, Status: "done", Repository: codeRepo, PRNumber: 14}
	store.tasks["w1"] = Task{ID: "w1", Status: "in-review", Repository: codeRepo, Gate: GateMergeQueue}
	store.queue = append(store.queue, queueEntry{project: codeRepo, task: "w1", at: time.Now()})
	openPlan(store)
	ctx := context.Background()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	pause, ok := store.pauses[codeRepo]
	if !ok || pause.RunID != 3090 || pause.SHA != commitC2 || pause.FixTask != "" || pause.Plan != "" {
		t.Fatalf("pause = %+v (%v), want run 3090's with no card", pause, ok)
	}
	if !slices.Equal(f.reruns, []string{"actions-tok 9414"}) || len(f.issues) != 0 || len(store.rolls) != 0 || len(wf.started) != 0 {
		t.Fatalf("re-runs %q, issues %q, rolls %+v, started %v; want image-runner re-run, no card, no roll", f.reruns, f.issues, store.rolls, wf.started)
	}
	if !slices.Equal(f.reported, []string{commitC2 + " success " + rollStatusContext}) || strings.Count(logs.String(), planHeldLog) != 1 {
		t.Fatalf("reported %q, log %q; want the red head marked held and the hold logged once", f.reported, logs.String())
	}

	f.jobs[3090][4]["status"] = "success"
	f.runs = run3176("success")
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if p, ok := store.pauses[codeRepo]; ok {
		t.Fatalf("pause %+v survived the re-run's green", p)
	}
	if !slices.Contains(wf.signals, "w1 queue_turn  #0: ") {
		t.Fatalf("signals = %q, want the waiting head woken", wf.signals)
	}
	if !slices.Equal(f.reported, []string{commitC2 + " success " + rollStatusContext}) || len(f.reruns) != 1 || len(f.issues) != 0 || len(store.rolls) != 0 {
		t.Fatalf("reported %q, re-runs %q, issues %q, rolls %+v; want no second mark, no card, no roll", f.reported, f.reruns, f.issues, store.rolls)
	}
}
