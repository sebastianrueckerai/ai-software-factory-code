package factoryd

import (
	"context"
	"errors"
	"fmt"
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

// fakeFluxSwitch is the factory's Kustomization as its suspend leaves it.
// Every call to SuspendKustomization counts in sets; a refused one changes
// nothing, and a level that differs from suspended counts in changes.
type fakeFluxSwitch struct {
	suspended                       bool
	applied                         string
	sets, changes                   int
	suspendErr, resumeErr, stateErr error
	onChange                        func()
}

func (f *fakeFluxSwitch) KustomizationState(ctx context.Context, namespace, name string) (FluxState, error) {
	if namespace != "flux-system" || name != "factory" {
		return FluxState{}, fmt.Errorf("read of %s/%s, want flux-system/factory", namespace, name)
	}
	if f.stateErr != nil {
		return FluxState{}, f.stateErr
	}
	return FluxState{Applied: f.applied}, nil
}

func (f *fakeFluxSwitch) SuspendKustomization(ctx context.Context, namespace, name string, suspend bool) error {
	f.sets++
	if namespace != "flux-system" || name != "factory" {
		return fmt.Errorf("suspend of %s/%s, want flux-system/factory", namespace, name)
	}
	if suspend && f.suspendErr != nil {
		return f.suspendErr
	}
	if !suspend && f.resumeErr != nil {
		return f.resumeErr
	}
	if suspend != f.suspended {
		f.suspended = suspend
		f.changes++
		if f.onChange != nil {
			f.onChange()
		}
	}
	return nil
}

const (
	fluxSuspendedLog = "roller: plan #40 is open on main: the factory's Kustomization is suspended"
	fluxResumedLog   = "roller: the factory's Kustomization is resumed"
)

// While a plan is open on main the factory's Kustomization is suspended,
// its level sent every poll and said once. The plan's close resumes it
// before the roll is filed in the same poll.
func TestRollerSuspendsTheFactoryKustomizationWhileHeld(t *testing.T) {
	logs := captureLog(t)
	roller, f, store, _ := newHeldRoller(t)
	flux := roller.Flux.(*fakeFluxSwitch)
	ctx := context.Background()
	for poll := 1; poll <= 2; poll++ {
		if err := roller.poll(ctx); err != nil {
			t.Fatal(err)
		}
		if !flux.suspended || flux.changes != 1 || flux.sets != poll {
			t.Fatalf("poll %d: suspended %v, changes %d, sets %d; want suspended once, set every poll", poll, flux.suspended, flux.changes, flux.sets)
		}
		if n := strings.Count(logs.String(), fluxSuspendedLog); n != 1 {
			t.Fatalf("poll %d: the suspend logged %d times, want once: %q", poll, n, logs.String())
		}
	}

	store.closed["t-plan"] = time.Now()
	issues := -1
	flux.onChange = func() { issues = len(f.issues) }
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if flux.suspended || issues != 0 {
		t.Fatalf("suspended %v, issues at the resume %d; want resumed before the roll is filed", flux.suspended, issues)
	}
	rolledC1(t, f, store)
	if n := strings.Count(logs.String(), fluxResumedLog); n != 1 {
		t.Fatalf("the resume logged %d times, want once: %q", n, logs.String())
	}
}

// A resume Flux refuses ends the poll before a roll is filed: the roll
// needs Flux to apply it.
func TestRollerFilesNoRollWhenTheResumeFails(t *testing.T) {
	roller, f, store, _ := newHeldRoller(t)
	flux := roller.Flux.(*fakeFluxSwitch)
	flux.suspended = true
	flux.resumeErr = fmt.Errorf("%w: PATCH x: refused", ErrForbidden)
	store.closed["t-plan"] = time.Now()
	if err := roller.poll(context.Background()); !errors.Is(err, flux.resumeErr) {
		t.Fatalf("poll: %v, want the resume's error", err)
	}
	if len(f.issues) != 0 || len(store.rolls) != 0 || !flux.suspended {
		t.Fatalf("issues %q, rolls %+v, suspended %v; want no roll and Flux still suspended", f.issues, store.rolls, flux.suspended)
	}
}

// A suspend Flux refuses does not end the hold: rolls stay held, the error
// is logged once, and once the keeper filed the hold's ask it is noted
// there once. Each suspend that lands after a refused one is said.
func TestRollerHoldsOnWhenTheSuspendFails(t *testing.T) {
	logs := captureLog(t)
	roller, f, store, _ := newHeldRoller(t)
	flux := roller.Flux.(*fakeFluxSwitch)
	flux.suspendErr = fmt.Errorf("%w: PATCH x: refused", ErrForbidden)
	ctx := context.Background()
	for poll := 1; poll <= 2; poll++ {
		if err := roller.poll(ctx); err != nil {
			t.Fatalf("poll %d: %v, want the hold to go on", poll, err)
		}
		if len(f.issues) != 0 || !slices.Equal(f.reported, []string{commitC2 + " success " + rollStatusContext}) {
			t.Fatalf("poll %d: issues %q, reported %q; want no roll and c2 marked held", poll, f.issues, f.reported)
		}
		if n := strings.Count(logs.String(), "roller: plan #40 is open on main: the factory's Kustomization could not be suspended ("); n != 1 {
			t.Fatalf("poll %d: the failed suspend logged %d times, want once: %q", poll, n, logs.String())
		}
		if len(f.comments) != 0 {
			t.Fatalf("poll %d: comments %q, want none before the ask", poll, f.comments)
		}
	}

	heldAsk(store, "waiting-human")
	for poll := 3; poll <= 4; poll++ {
		if err := roller.poll(ctx); err != nil {
			t.Fatalf("poll %d: %v", poll, err)
		}
	}
	if !slices.Equal(f.comments, []string{fluxRefusedComment}) {
		t.Fatalf("comments %q, want %q once", f.comments, fluxRefusedComment)
	}

	// each suspend that lands after a refused one is said
	for want := 1; want <= 2; want++ {
		flux.suspendErr = nil
		if err := roller.poll(ctx); err != nil {
			t.Fatal(err)
		}
		if !flux.suspended || strings.Count(logs.String(), fluxSuspendedLog) != want {
			t.Fatalf("suspended %v, log %q; want the suspend said %d times", flux.suspended, logs.String(), want)
		}
		flux.suspendErr = fmt.Errorf("%w: PATCH x: refused", ErrForbidden)
		if err := roller.poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// An open roll keeps Flux resumed though a plan is open: the roll goes
// live through it. Once the roll's task ends, the next poll suspends.
func TestRollerKeepsFluxResumedForAnOpenRoll(t *testing.T) {
	roller, _, store, _ := newHeldRoller(t)
	flux := roller.Flux.(*fakeFluxSwitch)
	store.tasks["t-roll1"] = Task{ID: "t-roll1", Source: SourceRoll, Status: "running", Repository: codeRepo}
	store.rolls = []RollRequest{{SHA: "5050505050505050505050505050505050505050", RunID: 50, Repository: codeRepo, State: RollOpened, TaskID: "t-roll1"}}
	ctx := context.Background()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if flux.suspended || flux.sets != 1 {
		t.Fatalf("suspended %v, sets %d; want resumed while the roll is open", flux.suspended, flux.sets)
	}
	task := store.tasks["t-roll1"]
	task.Status = "done"
	store.tasks["t-roll1"] = task
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if !flux.suspended {
		t.Fatal("still resumed after the roll's task ended")
	}
}

// Deploy now resumes Flux before its commit's roll is filed and keeps it
// resumed while the roll is open and until Flux applied main's newest
// commit; a state Flux will not tell counts as not yet. Then Deploy now is
// spent and the hold suspends again.
func TestRollerResumesFluxForDeployNow(t *testing.T) {
	logs := captureLog(t)
	roller, f, store, _ := newHeldRoller(t)
	flux := roller.Flux.(*fakeFluxSwitch)
	flux.suspended = true
	store.releases = map[string]Release{codeRepo: {Repository: codeRepo, HeldSince: time.Now(), DeployNow: commitC1}}
	issues := -1
	flux.onChange = func() { issues = len(f.issues) }
	ctx := context.Background()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if flux.suspended || issues != 0 {
		t.Fatalf("suspended %v, issues at the resume %d; want resumed before the roll is filed", flux.suspended, issues)
	}
	rolledC1(t, f, store)

	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if flux.suspended {
		t.Fatal("poll 2: suspended while the roll is open")
	}

	task := store.tasks[store.rolls[0].TaskID]
	task.Status = "done"
	store.tasks[task.ID] = task
	store.rolls[0].State = RollMerged
	flux.applied = "main@sha1:" + commitC1
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if flux.suspended || store.releases[codeRepo].DeployNow != commitC1 {
		t.Fatalf("poll 3: suspended %v, Deploy now %q; want resumed and c1's Deploy now kept: Flux has not applied main's newest commit", flux.suspended, store.releases[codeRepo].DeployNow)
	}

	flux.stateErr = errors.New("kube GET x: status 500")
	for range 2 {
		if err := roller.poll(ctx); err != nil {
			t.Fatalf("poll 4: %v, want the poll to go on", err)
		}
		if flux.suspended || store.releases[codeRepo].DeployNow != commitC1 {
			t.Fatalf("poll 4: suspended %v, Deploy now %q; want resumed and c1's Deploy now kept", flux.suspended, store.releases[codeRepo].DeployNow)
		}
	}
	if n := strings.Count(logs.String(), "kube GET x: status 500"); n != 1 {
		t.Fatalf("the failed read logged %d times over two polls, want once: %q", n, logs.String())
	}

	flux.stateErr = nil
	flux.applied = "main@sha1:" + commitC2
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if !flux.suspended || store.releases[codeRepo].DeployNow != "" {
		t.Fatalf("poll 5: suspended %v, Deploy now %q; want suspended and Deploy now spent", flux.suspended, store.releases[codeRepo].DeployNow)
	}
}

// A plan card in the merge queue suspends Flux before the plan's first
// merge, though no plan is open on main and rolls are not held: a card
// holding a queue entry, and one held back in gate merge-queue. Once the
// card leaves the queue without merging, the next poll resumes. A card
// merged by hand keeps its gate merge-queue but is out of the queue: once
// its plan closed, Flux stays resumed.
func TestRollerSuspendsFluxForAQueuedPlanCard(t *testing.T) {
	logs := captureLog(t)
	f := &rollForgejo{runs: []map[string]any{pushRun(58, commitC2, "running")}}
	roller, store, _ := newTestRoller(t, f)
	flux := roller.Flux.(*fakeFluxSwitch)
	store.tasks["t-plan"] = Task{ID: "t-plan", IssueNumber: 40, Repository: codeRepo, Track: "plan"}
	store.tasks["t-c1"] = Task{ID: "t-c1", Source: SourcePlan, Parent: "t-plan", PlanKey: "a", Repository: codeRepo, Status: "in-review"}
	ctx := context.Background()
	poll := func(want bool) {
		t.Helper()
		if err := roller.poll(ctx); err != nil {
			t.Fatal(err)
		}
		if flux.suspended != want {
			t.Fatalf("suspended %v, want %v", flux.suspended, want)
		}
	}
	poll(false)

	store.queue = []queueEntry{{project: codeRepo, task: "t-c1", at: time.Now()}}
	poll(true)
	if n := strings.Count(logs.String(), "roller: plan #40 has a card in the merge queue: the factory's Kustomization is suspended"); n != 1 {
		t.Fatalf("the suspend logged %d times, want once: %q", n, logs.String())
	}
	if len(f.reported) != 0 || strings.Contains(logs.String(), "no roll until") {
		t.Fatalf("reported %q, log %q; want no roll hold: no plan is open on main", f.reported, logs.String())
	}

	store.queue = nil
	c1 := store.tasks["t-c1"]
	c1.Gate = GateMergeQueue
	store.tasks["t-c1"] = c1
	poll(true)

	c1.Gate = ""
	store.tasks["t-c1"] = c1
	poll(false)

	c1.Status, c1.Gate, c1.MergeCommit, c1.FinishedAt = "merged", GateMergeQueue, mergePR12, justNow()
	store.tasks["t-c1"] = c1
	store.closed["t-plan"] = time.Now()
	poll(false)
}

// With no Deploy now a poll sets Flux's level before it reads the runs: a
// poll that fails on main's state, or on the run read, still suspends Flux
// for a plan card that entered the merge queue, and still ends with the
// failure.
func TestRollerSuspendsFluxThoughThePollFails(t *testing.T) {
	for name, fail := range map[string]func(*Roller, *rollForgejo, *fakeStore){
		"main's state": func(r *Roller, _ *rollForgejo, s *fakeStore) { r.Store = &pauseReadFails{s} },
		"the run read": func(_ *Roller, f *rollForgejo, _ *fakeStore) { f.runsDown = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := &rollForgejo{runs: []map[string]any{pushRun(58, commitC2, "success")}}
			roller, store, _ := newTestRoller(t, f)
			flux := roller.Flux.(*fakeFluxSwitch)
			store.tasks["t-plan"] = Task{ID: "t-plan", IssueNumber: 40, Repository: codeRepo, Track: "plan"}
			store.tasks["t-c1"] = Task{ID: "t-c1", Source: SourcePlan, Parent: "t-plan", PlanKey: "a", Repository: codeRepo, Status: "in-review"}
			store.queue = []queueEntry{{project: codeRepo, task: "t-c1", at: time.Now()}}
			fail(roller, f, store)
			if err := roller.poll(context.Background()); err == nil {
				t.Fatal("poll: nil, want the failure")
			}
			if !flux.suspended {
				t.Fatal("resumed: the failed poll left Flux at its last level")
			}
		})
	}
}

// A resume Flux refuses still ends the poll, after main's state: a green
// main ends its queue pause though Flux cannot be resumed.
func TestRollerReadsMainsStateThoughTheResumeFails(t *testing.T) {
	f := &rollForgejo{runs: []map[string]any{pushRun(58, commitC2, "success")}}
	roller, store, _ := newTestRoller(t, f)
	flux := roller.Flux.(*fakeFluxSwitch)
	flux.suspended = true
	flux.resumeErr = fmt.Errorf("%w: PATCH x: refused", ErrForbidden)
	store.pauses = map[string]QueuePause{codeRepo: {Project: codeRepo, RunID: 57, SHA: commitC1, Since: time.Now()}}
	if err := roller.poll(context.Background()); !errors.Is(err, flux.resumeErr) {
		t.Fatalf("poll: %v, want the resume's error", err)
	}
	if p, ok := store.pauses[codeRepo]; ok || !flux.suspended {
		t.Fatalf("pause %+v (%v), suspended %v; want the pause ended by main's green run, Flux as it was", p, ok, flux.suspended)
	}
}

// pauseReadFails is a store whose queue pause cannot be read, so main's
// state fails.
type pauseReadFails struct{ *fakeStore }

func (s *pauseReadFails) QueuePause(context.Context, string) (QueuePause, bool, error) {
	return QueuePause{}, false, errors.New("the database is gone")
}

// queuedPlanRoller is newHeldRoller's rig with plan #40 not yet on main:
// its card t-c1 holds a merge queue entry, so no roll is held, c1's roll
// is filed, and Flux is suspended for the queued card.
func queuedPlanRoller(t *testing.T) (*Roller, *rollForgejo, *fakeStore, *fakeFluxSwitch) {
	t.Helper()
	roller, f, store, _ := newHeldRoller(t)
	c1 := store.tasks["t-c1"]
	c1.Status, c1.MergeCommit, c1.FinishedAt = "in-review", "", nil
	store.tasks["t-c1"] = c1
	store.queue = []queueEntry{{project: codeRepo, task: "t-c1", at: time.Now()}}
	return roller, f, store, roller.Flux.(*fakeFluxSwitch)
}

// A roll filed while a queued plan card holds Flux suspended resumes Flux
// before the roll is filed: the roll goes live through it. The open roll
// keeps it resumed.
func TestRollerResumesFluxToFileARoll(t *testing.T) {
	roller, f, store, flux := queuedPlanRoller(t)
	issues := -1
	flux.onChange = func() {
		if !flux.suspended {
			issues = len(f.issues)
		}
	}
	ctx := context.Background()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if flux.suspended || flux.changes != 2 || issues != 0 {
		t.Fatalf("suspended %v, changes %d, issues at the resume %d; want suspended, then resumed before the roll is filed", flux.suspended, flux.changes, issues)
	}
	rolledC1(t, f, store)
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if flux.suspended || flux.changes != 2 {
		t.Fatalf("poll 2: suspended %v, changes %d; want resumed while the roll is open", flux.suspended, flux.changes)
	}
}

// A resume refused before the filing files no roll.
func TestRollerFilesNoRollWhenItsResumeFails(t *testing.T) {
	roller, f, store, flux := queuedPlanRoller(t)
	flux.resumeErr = fmt.Errorf("%w: PATCH x: refused", ErrForbidden)
	if err := roller.poll(context.Background()); !errors.Is(err, flux.resumeErr) {
		t.Fatalf("poll: %v, want the resume's error", err)
	}
	if len(f.issues) != 0 || len(store.rolls) != 0 || !flux.suspended {
		t.Fatalf("issues %q, rolls %+v, suspended %v; want no roll and Flux still suspended", f.issues, store.rolls, flux.suspended)
	}
}

const fluxRefusedComment = "77 factoryd: the factory's Kustomization flux-system/factory could not be suspended, so a change under k8s/ goes live at its merge while production is held. The roller's log names the error."

// heldAsk files the hold's ask as the keeper does, issue #77, in status.
func heldAsk(store *fakeStore, status string) {
	store.tasks["t-ask"] = Task{ID: "t-ask", Kind: "human", Source: SourceProductionHeld, Status: status, IssueNumber: 77, Repository: codeRepo}
}

// A note on the ask Forgejo refuses ends the poll with its error, and the
// next poll notes it.
func TestRollerNotesARefusedSuspendOnceForgejoTakesIt(t *testing.T) {
	roller, f, store, _ := newHeldRoller(t)
	roller.Flux.(*fakeFluxSwitch).suspendErr = fmt.Errorf("%w: PATCH x: refused", ErrForbidden)
	heldAsk(store, "waiting-human")
	f.commentsOff = true
	ctx := context.Background()
	if err := roller.poll(ctx); err == nil || !strings.Contains(err.Error(), "note the refused suspend on the hold's ask #77: ") {
		t.Fatalf("poll: %v, want the refused note's error", err)
	}
	if len(f.comments) != 0 || len(f.issues) != 0 {
		t.Fatalf("comments %q, issues %q; want none", f.comments, f.issues)
	}
	f.commentsOff = false
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.comments, []string{fluxRefusedComment}) {
		t.Fatalf("comments %q, want %q", f.comments, fluxRefusedComment)
	}
}

// A refused suspend notes nothing on an ask that has ended.
func TestRollerNotesNoRefusedSuspendOnAnEndedAsk(t *testing.T) {
	roller, f, store, _ := newHeldRoller(t)
	roller.Flux.(*fakeFluxSwitch).suspendErr = fmt.Errorf("%w: PATCH x: refused", ErrForbidden)
	heldAsk(store, "done")
	for poll := 1; poll <= 2; poll++ {
		if err := roller.poll(context.Background()); err != nil {
			t.Fatalf("poll %d: %v", poll, err)
		}
	}
	if len(f.comments) != 0 {
		t.Fatalf("comments %q, want none on the ended ask", f.comments)
	}
}

// A plan card that merges and leaves the merge queue between the roller's
// reads of the queue and of the open groups counts in one of them, either
// way round: Flux stays suspended.
func TestRollerSuspendsFluxForACardThatMergesBetweenItsReads(t *testing.T) {
	for _, after := range []string{"OpenGroups", "QueuedPlans"} {
		t.Run("after "+after, func(t *testing.T) {
			f := &rollForgejo{runs: []map[string]any{pushRun(58, commitC2, "running")}}
			roller, store, _ := newTestRoller(t, f)
			flux := roller.Flux.(*fakeFluxSwitch)
			store.tasks["t-plan"] = Task{ID: "t-plan", IssueNumber: 40, Repository: codeRepo, Track: "plan"}
			store.tasks["t-c1"] = Task{ID: "t-c1", Source: SourcePlan, Parent: "t-plan", PlanKey: "a", Repository: codeRepo, Status: "in-review"}
			store.queue = []queueEntry{{project: codeRepo, task: "t-c1", at: time.Now()}}
			store.afterRead = func(name string) {
				if name != after || store.queue == nil {
					return
				}
				c1 := store.tasks["t-c1"]
				c1.Status, c1.MergeCommit, c1.FinishedAt = "merged", mergePR12, justNow()
				store.tasks["t-c1"] = c1
				store.queue = nil
			}
			if err := roller.poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !flux.suspended {
				t.Fatal("resumed: the card counted in neither read")
			}
		})
	}
}

// A queued plan card blocks no other work: a commit that goes live through
// Flux alone (only k8s/ changed, or no image) while the card holds Flux
// suspended resumes Flux before it is judged live, as a roll's filing
// does. The next poll suspends again for the queued card and judges
// nothing twice. A resume Flux refuses ends the poll, the commit unjudged.
func TestRollerResumesFluxForACommitLiveThroughFluxAlone(t *testing.T) {
	for name, f := range map[string]func() *rollForgejo{
		"only k8s": func() *rollForgejo {
			return &rollForgejo{runs: []map[string]any{pushRun(62, commitC2, "success")}, statuses: imageGreen(commitC2),
				touched: map[string][]string{commitC2: {"k8s/factoryd.yaml"}}}
		},
		"no image": func() *rollForgejo {
			return &rollForgejo{runs: []map[string]any{pushRun(62, commitC2, "success")}, statuses: imageGreen(commitC2),
				files: map[string]map[string]string{codeRepo + "@main": mainPins()},
				registry: registryOf(map[string]string{
					"sf/factory-gateway/manifests/c2c2c2c": imageIndex(gwOld, "sha256:b1"),
					"sf/opencode-worker/manifests/c2c2c2c": workerRebuilt,
					"sf/ci-runner/manifests/c2c2c2c":       runnerRebuilt2,
				}, gwOldIdx, workerOldIdx, runnerIdx)}
		},
	} {
		queued := func(t *testing.T) (*Roller, *rollForgejo, *fakeStore, *fakeFluxSwitch) {
			fg := f()
			roller, store, _ := newTestRoller(t, fg)
			store.tasks["t-plan"] = Task{ID: "t-plan", IssueNumber: 40, Repository: codeRepo, Track: "plan"}
			store.tasks["t-c1"] = Task{ID: "t-c1", Source: SourcePlan, Parent: "t-plan", PlanKey: "a", Repository: codeRepo, Status: "in-review"}
			store.queue = []queueEntry{{project: codeRepo, task: "t-c1", at: time.Now()}}
			return roller, fg, store, roller.Flux.(*fakeFluxSwitch)
		}
		t.Run(name, func(t *testing.T) {
			roller, f, store, flux := queued(t)
			judged := -1
			flux.onChange = func() {
				if !flux.suspended {
					judged = len(store.rolls)
				}
			}
			ctx := context.Background()
			if err := roller.poll(ctx); err != nil {
				t.Fatal(err)
			}
			if flux.suspended || flux.changes != 2 || judged != 0 {
				t.Fatalf("suspended %v, changes %d, judged at the resume %d; want suspended, then resumed before c2 is judged", flux.suspended, flux.changes, judged)
			}
			if len(store.rolls) != 1 || store.rolls[0].SHA != commitC2 || store.rolls[0].State != RollSkipped ||
				!slices.Equal(f.reported, []string{commitC2 + " success " + rollStatusContext}) {
				t.Fatalf("rolls %+v, reported %q; want c2 judged live", store.rolls, f.reported)
			}
			if err := roller.poll(ctx); err != nil {
				t.Fatal(err)
			}
			if !flux.suspended || flux.changes != 3 || len(store.rolls) != 1 || len(f.reported) != 1 {
				t.Fatalf("poll 2: suspended %v, changes %d, rolls %+v, reported %q; want suspended again, c2 judged once",
					flux.suspended, flux.changes, store.rolls, f.reported)
			}
		})
		t.Run(name+", its resume refused", func(t *testing.T) {
			roller, f, store, flux := queued(t)
			flux.resumeErr = fmt.Errorf("%w: PATCH x: refused", ErrForbidden)
			if err := roller.poll(context.Background()); !errors.Is(err, flux.resumeErr) {
				t.Fatalf("poll: %v, want the resume's error", err)
			}
			if !flux.suspended || len(store.rolls) != 0 || len(f.reported) != 0 {
				t.Fatalf("suspended %v, rolls %+v, reported %q; want c2 unjudged while Flux stays suspended", flux.suspended, store.rolls, f.reported)
			}
		})
	}
}
