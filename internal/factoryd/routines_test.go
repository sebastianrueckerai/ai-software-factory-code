package factoryd

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// routineTree is the policy package's fixture tree: the code project's
// weekly bump (Mondays 06:00) and audit (every 20 merges), and
// policy.yaml's template pass (hourly and on each configuration version)
// for linkshort, the one project with a template, in Europe/Berlin.
func routineTree(t *testing.T) policy.Tree {
	t.Helper()
	files, err := policy.ReadDir("../policy/testdata/tree")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := policy.ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// withRoutines is tree with project's own routines replaced by rs.
func withRoutines(tree policy.Tree, project string, rs ...policy.Routine) policy.Tree {
	tree.Projects = maps.Clone(tree.Projects)
	p := tree.Projects[project]
	p.Routines = rs
	tree.Projects[project] = p
	return tree
}

// fakeSchedules is Temporal's schedules in memory; List leaves out those
// unlisted names, as a list that lags behind a create does.
type fakeSchedules struct {
	have             map[string]ScheduleSpec
	unlisted         map[string]bool
	updates, deletes int
}

func (f *fakeSchedules) List(context.Context) (map[string]ScheduleSpec, error) {
	out := maps.Clone(f.have)
	maps.DeleteFunc(out, func(id string, _ ScheduleSpec) bool { return f.unlisted[id] })
	return out, nil
}

func (f *fakeSchedules) Create(_ context.Context, s ScheduleSpec) error {
	if _, ok := f.have[s.ID]; ok {
		return temporal.ErrScheduleAlreadyRunning
	}
	if f.have == nil {
		f.have = map[string]ScheduleSpec{}
	}
	f.have[s.ID] = s
	return nil
}

func (f *fakeSchedules) Update(_ context.Context, s ScheduleSpec) error {
	if _, ok := f.have[s.ID]; !ok {
		return fmt.Errorf("schedule %s: %w", s.ID, ErrNotFound)
	}
	f.have[s.ID] = s
	f.updates++
	return nil
}

func (f *fakeSchedules) Delete(_ context.Context, id string) error {
	if _, ok := f.have[id]; !ok {
		return fmt.Errorf("schedule %s: %w", id, ErrNotFound)
	}
	delete(f.have, id)
	f.deletes++
	return nil
}

// fakeEvents answers the merge pass's reads from rows, oldest first, or
// fails with err.
type fakeEvents struct {
	rows  []Event
	err   error
	reads int
	// onRead runs after each read
	onRead func(reads int)
}

func (f *fakeEvents) MergedEvents(_ context.Context, after int64, limit int) ([]Event, error) {
	var out []Event
	for _, e := range f.rows {
		if e.ID > after && e.Kind == EventPullRequestMerged && len(out) < limit {
			out = append(out, e)
		}
	}
	err := f.err
	f.reads++
	if f.onRead != nil {
		f.onRead(f.reads)
	}
	return out, err
}

// mon0540 is Monday 12 October 2026, 05:40 in Berlin.
var mon0540 = time.Date(2026, 10, 12, 5, 40, 0, 0, berlin)

var berlin = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		panic(err)
	}
	return loc
}()

// routinesRig is the service over the fixture tree, an empty store, no
// schedules and no events, at mon0540.
func routinesRig(t *testing.T) (*Routines, *fakeStore, *fakeSchedules, *recordingWorkflows, *policy.Tree) {
	t.Helper()
	tree := routineTree(t)
	store, sched, wf := newFakeStore(), &fakeSchedules{}, &recordingWorkflows{}
	r := &Routines{Store: store, Events: &fakeEvents{}, Schedules: sched, Workflows: wf,
		Current: func() policy.Version { return policy.Version{ID: 7, Tree: tree, ActivatedAt: mon0540.Add(-time.Hour)} },
		Now:     func() time.Time { return mon0540 }}
	return r, store, sched, wf, &tree
}

func TestReconcileCreatesUpdatesAndDeletesSchedules(t *testing.T) {
	ctx := t.Context()
	r, store, sched, _, tree := routinesRig(t)
	sched.have = map[string]ScheduleSpec{"routine/sf/old/0": {ID: "routine/sf/old/0", Cron: "0 0 * * *"}}
	logs := captureLog(t)
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := sched.have["routine/sf/old/0"]; ok {
		t.Fatal("a schedule no longer in the tree stayed")
	}
	bump := sched.have["routine/ai-software-factory-code/weekly-bump/0"]
	want := RoutineRunInput{Project: "ai-software-factory-code", Routine: "weekly-bump", Trigger: "cron"}
	if bump.Cron != "0 6 * * 1" || bump.TimeZone != "Europe/Berlin" || bump.CatchupWindow != 24*time.Hour || bump.Input != want {
		t.Fatalf("bump %+v", bump)
	}
	pass := sched.have["routine//template-pass/0"]
	if pass.Cron != "3 * * * *" || pass.Input != (RoutineRunInput{Routine: "template-pass", Trigger: "cron"}) {
		t.Fatalf("the factory's template pass: %+v", pass)
	}
	// the audit counts merges and the template pass's second trigger is an
	// event: neither is a clock
	if len(sched.have) != 2 {
		t.Fatalf("schedules %v", slices.Sorted(maps.Keys(sched.have)))
	}
	if got := logs.String(); !strings.Contains(got, "routines: reconciled 3 schedules\n") {
		t.Fatalf("log %q", got)
	}
	// every routine of the tree has its state row, for the merge pass to bump
	for _, k := range []string{"ai-software-factory-code/weekly-bump", "ai-software-factory-code/audit", "/template-pass"} {
		if _, ok := store.routineStates[k]; !ok {
			t.Errorf("no state row %s", k)
		}
	}

	// a tree as it was changes nothing
	logs.Reset()
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if sched.updates != 0 || sched.deletes != 1 || logs.Len() != 0 {
		t.Fatalf("an unchanged tree: %d updates, %d deletes, log %q", sched.updates, sched.deletes, logs)
	}

	// an edit updates in place
	tree.Projects["ai-software-factory-code"].Routines[0].Schedule[0].Cron = "0 7 * * 1"
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if sched.have["routine/ai-software-factory-code/weekly-bump/0"].Cron != "0 7 * * 1" || sched.updates != 1 {
		t.Fatalf("update: %+v %d", sched.have, sched.updates)
	}
	// missed: skip catches up 10 s, Temporal's least
	tree.Projects["ai-software-factory-code"].Routines[0].Missed = policy.MissedSkip
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := sched.have["routine/ai-software-factory-code/weekly-bump/0"].CatchupWindow; got != 10*time.Second || sched.updates != 2 {
		t.Fatalf("missed skip: catch-up %v, %d updates", got, sched.updates)
	}
	// no time_zone is UTC
	tree.Document.TimeZone = ""
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for id, s := range sched.have {
		if s.TimeZone != "UTC" {
			t.Errorf("%s in %q", id, s.TimeZone)
		}
	}
	if sched.updates != 4 {
		t.Fatalf("a new zone updated %d", sched.updates-2)
	}
}

// A schedule the list does not show yet is there: Temporal refuses its
// create as already registered, and the pass updates it instead.
func TestReconcileUpdatesAScheduleTheListMissed(t *testing.T) {
	r, _, sched, _, _ := routinesRig(t)
	id := "routine/ai-software-factory-code/weekly-bump/0"
	sched.have = map[string]ScheduleSpec{id: {ID: id, Cron: "0 5 * * 1"}}
	sched.unlisted = map[string]bool{id: true}
	logs := captureLog(t)
	if err := r.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := sched.have[id]; got.Cron != "0 6 * * 1" || got.TimeZone != "Europe/Berlin" || sched.updates != 1 {
		t.Fatalf("schedule %+v, %d updates: want the tree's spec through one update", got, sched.updates)
	}
	if got := logs.String(); !strings.Contains(got, "routines: reconciled 2 schedules\n") {
		t.Fatalf("log %q", got)
	}
}

func TestReconcileDeletesSchedulesNoLongerInTheTree(t *testing.T) {
	r, _, sched, _, tree := routinesRig(t)
	if err := r.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	*tree = policy.Tree{}
	if err := r.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(sched.have) != 0 || sched.deletes != 2 {
		t.Fatalf("left %v, deleted %d", slices.Sorted(maps.Keys(sched.have)), sched.deletes)
	}
}

func TestMergePassFiresOnceAndKeepsTheRemainder(t *testing.T) {
	ctx := t.Context()
	r, store, _, wf, _ := routinesRig(t)
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRoutineMerges(ctx, "ai-software-factory-code", "audit", 19); err != nil {
		t.Fatal(err)
	}
	code, merged := "sf/ai-software-factory-code", EventPullRequestMerged
	r.Events = &fakeEvents{rows: []Event{{ID: 1, Repository: code, Kind: merged}, {ID: 2, Repository: "other/other", Kind: merged},
		{ID: 3, Repository: code, Kind: EventTaskEnded}, {ID: 4, Repository: code, Kind: merged}}}
	if err := r.MergePass(ctx); err != nil {
		t.Fatal(err)
	}
	want := RoutineRunInput{Project: "ai-software-factory-code", Routine: "audit", Trigger: "merges", Due: mon0540}
	if len(wf.routineRuns) != 1 || wf.routineRuns[0] != want {
		t.Fatalf("runs %+v", wf.routineRuns)
	}
	if st, _ := store.RoutineState(ctx, "ai-software-factory-code", "audit"); st.Merges != 1 {
		t.Fatalf("remainder %d", st.Merges)
	}
	if c, _ := store.RoutineMergeCursor(ctx); c != 4 {
		t.Fatalf("cursor %d", c)
	}
	// nothing new: no run
	if err := r.MergePass(ctx); err != nil {
		t.Fatal(err)
	}
	if len(wf.routineRuns) != 1 {
		t.Fatalf("a pass without merges ran %+v", wf.routineRuns[1:])
	}
}

// Merges beyond one batch and beyond the count: one run, the count's
// remainder kept.
func TestMergePassReadsEveryBatch(t *testing.T) {
	ctx := t.Context()
	r, store, _, wf, _ := routinesRig(t)
	if err := r.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	events := &fakeEvents{}
	for id := range int64(205) {
		events.rows = append(events.rows, Event{ID: id + 1, Repository: "sf/ai-software-factory-code", Kind: EventPullRequestMerged})
	}
	r.Events = events
	if err := r.MergePass(ctx); err != nil {
		t.Fatal(err)
	}
	if len(wf.routineRuns) != 1 || events.reads != 3 {
		t.Fatalf("runs %+v after %d reads", wf.routineRuns, events.reads)
	}
	if st, _ := store.RoutineState(ctx, "ai-software-factory-code", "audit"); st.Merges != 5 {
		t.Fatalf("remainder %d", st.Merges)
	}
	if c, _ := store.RoutineMergeCursor(ctx); c != 205 {
		t.Fatalf("cursor %d", c)
	}
}

func TestConfigurationVersionStartsTheOnRoutines(t *testing.T) {
	r, _, _, wf, tree := routinesRig(t)
	if err := r.ConfigurationVersion(t.Context()); err != nil {
		t.Fatal(err)
	}
	// once, as the factory's, though linkshort runs it, and due when the
	// version came into force, so a second start is the same run
	activated := mon0540.Add(-time.Hour)
	want := RoutineRunInput{Routine: "template-pass", Trigger: "configuration-version", Due: activated}
	if len(wf.routineRuns) != 1 || wf.routineRuns[0] != want {
		t.Fatalf("runs %+v", wf.routineRuns)
	}
	*tree = withRoutines(*tree, "demo-app", policy.Routine{Name: "notes", Title: "Notes", Files: policy.FilesReport,
		Schedule: []policy.Trigger{{On: policy.OnConfigurationVersion}}})
	wf.routineRuns = nil
	if err := r.ConfigurationVersion(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(wf.routineRuns) != 2 || wf.routineRuns[1] != (RoutineRunInput{Project: "demo-app", Routine: "notes", Trigger: "configuration-version", Due: activated}) {
		t.Fatalf("with a project's own: %+v", wf.routineRuns)
	}
}

func TestMainPushedStartsThePushRoutinesOfTheRepository(t *testing.T) {
	r, _, _, wf, tree := routinesRig(t)
	*tree = withRoutines(*tree, "linkshort", policy.Routine{Name: "smoke", Title: "Smoke", Files: policy.FilesReport,
		Schedule: []policy.Trigger{{On: policy.OnPushMain}}})
	for _, project := range []string{"linkshort", "ai-software-factory-code", "no-such-project"} {
		if err := r.MainPushed(t.Context(), project); err != nil {
			t.Fatalf("%s: %v", project, err)
		}
	}
	want := RoutineRunInput{Project: "linkshort", Routine: "smoke", Trigger: "push-main", Due: mon0540}
	if len(wf.routineRuns) != 1 || wf.routineRuns[0] != want {
		t.Fatalf("runs %+v", wf.routineRuns)
	}
}

func TestRunNowStartsARunDueNow(t *testing.T) {
	ctx := t.Context()
	r, _, _, wf, _ := routinesRig(t)
	for _, c := range [][2]string{{"ai-software-factory-code", "weekly-bump"}, {"linkshort", "template-pass"}, {"", "template-pass"}} {
		if err := r.RunNow(ctx, c[0], c[1]); err != nil {
			t.Fatalf("%v: %v", c, err)
		}
	}
	// a project's view of the factory's template pass runs the factory's
	want := []RoutineRunInput{{Project: "ai-software-factory-code", Routine: "weekly-bump", Trigger: "run-now", Due: mon0540},
		{Routine: "template-pass", Trigger: "run-now", Due: mon0540}, {Routine: "template-pass", Trigger: "run-now", Due: mon0540}}
	if fmt.Sprint(wf.routineRuns) != fmt.Sprint(want) {
		t.Fatalf("runs %+v", wf.routineRuns)
	}
	for _, c := range [][2]string{{"demo-app", "template-pass"}, {"ai-software-factory-code", "nightly"}, {"", "weekly-bump"}} {
		if err := r.RunNow(ctx, c[0], c[1]); !errors.Is(err, ErrNotFound) {
			t.Errorf("%v: %v", c, err)
		}
	}
}

// Skip and Runs reach the store under the routine's key, the project for
// its own and "" for the factory's template pass a project's board
// shows, and refuse a routine the project does not run without writing a
// row; RunOfTask reads the store.
func TestRoutineActsReachTheStoreByKey(t *testing.T) {
	ctx := t.Context()
	r, store, _, _, _ := routinesRig(t)
	if err := r.Skip(ctx, "ai-software-factory-code", "weekly-bump", true, "sebastian"); err != nil {
		t.Fatal(err)
	}
	if err := r.Skip(ctx, "linkshort", "template-pass", true, "alice"); err != nil {
		t.Fatal(err)
	}
	if st := store.routineStates["ai-software-factory-code/weekly-bump"]; !st.SkipNext || st.SkipBy != "sebastian" {
		t.Fatalf("the bump's state %+v", st)
	}
	if st := store.routineStates["/template-pass"]; !st.SkipNext || st.SkipBy != "alice" {
		t.Fatalf("the template pass's state %+v", st)
	}
	if err := r.Skip(ctx, "linkshort", "template-pass", false, "alice"); err != nil {
		t.Fatal(err)
	}
	if st := store.routineStates["/template-pass"]; st.SkipNext || st.SkipBy != "" {
		t.Fatalf("the undone skip %+v", st)
	}

	store.routineRuns["/template-pass"] = []RoutineRun{{Routine: "template-pass", Run: 1, TaskID: "t-1"}, {Routine: "template-pass", Run: 2}}
	runs, err := r.Runs(ctx, "linkshort", "template-pass", 1)
	if err != nil || len(runs) != 1 || runs[0].Run != 2 {
		t.Fatalf("runs %+v %v", runs, err)
	}
	if run, ok, err := r.RunOfTask(ctx, "t-1"); err != nil || !ok || run.Run != 1 {
		t.Fatalf("the run of t-1: %+v %v %v", run, ok, err)
	}

	for _, c := range [][2]string{{"demo-app", "template-pass"}, {"ai-software-factory-code", "nightly"}} {
		if err := r.Skip(ctx, c[0], c[1], true, "sebastian"); !errors.Is(err, ErrNotFound) {
			t.Errorf("skip %v: %v", c, err)
		}
		if _, err := r.Runs(ctx, c[0], c[1], 1); !errors.Is(err, ErrNotFound) {
			t.Errorf("runs %v: %v", c, err)
		}
	}
	if len(store.routineStates) != 2 {
		t.Fatalf("states %v", store.routineStates)
	}
}

func TestViews(t *testing.T) {
	ctx := t.Context()
	r, store, _, _, tree := routinesRig(t)
	if err := store.SetRoutineMerges(ctx, "ai-software-factory-code", "audit", 14); err != nil {
		t.Fatal(err)
	}
	store.tasks["t-1"] = Task{ID: "t-1", Status: "waiting-human", IssueNumber: 781, Source: SourceRoutine}
	store.routineRuns["ai-software-factory-code/weekly-bump"] = []RoutineRun{{Project: "ai-software-factory-code", Routine: "weekly-bump", Run: 1,
		Trigger: "cron", TaskID: "t-1", Issue: 781, Outcome: "filed"}}

	vs, err := r.Views(ctx, "ai-software-factory-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 {
		t.Fatalf("views %+v", vs)
	}
	bump, audit := vs[0], vs[1]
	if bump.Project != "ai-software-factory-code" || bump.Routine.Name != "weekly-bump" || !bump.Next.Equal(mon0540.Add(20*time.Minute)) ||
		bump.NextWords != "Mon 06:00" || bump.MergesLeft != 0 || !bump.Soon || bump.Lane != "needs-you" {
		t.Fatalf("the weekly bump: %+v", bump)
	}
	if bump.Last.Run != 1 || bump.Open == nil || bump.Open.ID != "t-1" || bump.Held {
		t.Fatalf("the weekly bump's card: last %+v, open %v, held %v", bump.Last, bump.Open, bump.Held)
	}
	if audit.Routine.Name != "audit" || audit.State.Merges != 14 || audit.NextWords != "after 6 more merges" || audit.MergesLeft != 6 || !audit.Next.IsZero() ||
		audit.Soon || audit.Lane != "queued" || audit.Open != nil || audit.Held || audit.Last.Run != 0 {
		t.Fatalf("the audit: %+v", audit)
	}

	// the next run held, as #781 is still open; once #781 closes the
	// held run no longer holds anything
	store.routineRuns["ai-software-factory-code/weekly-bump"] = append(store.routineRuns["ai-software-factory-code/weekly-bump"],
		RoutineRun{Project: "ai-software-factory-code", Routine: "weekly-bump", Run: 2, Trigger: "cron", Outcome: "held", Note: "held: #781 is still open"})
	if vs, err := r.Views(ctx, "ai-software-factory-code"); err != nil || !vs[0].Held || vs[0].Open == nil || vs[0].Open.ID != "t-1" ||
		vs[0].Last.Note != "held: #781 is still open" {
		t.Fatalf("held: %+v %v", vs, err)
	}
	store.tasks["t-1"] = Task{ID: "t-1", Status: "done", IssueNumber: 781, Source: SourceRoutine, FinishedAt: &mon0540}
	if vs, err := r.Views(ctx, "ai-software-factory-code"); err != nil || vs[0].Held || vs[0].Open != nil {
		t.Fatalf("closed: %+v %v", vs, err)
	}
	// one merge short, and past the count while a run did not start
	for merges, want := range map[int]string{19: "after 1 more merge", 25: "after 1 more merge"} {
		if err := store.SetRoutineMerges(ctx, "ai-software-factory-code", "audit", merges); err != nil {
			t.Fatal(err)
		}
		if vs, err := r.Views(ctx, "ai-software-factory-code"); err != nil || vs[1].NextWords != want {
			t.Fatalf("%d merges: %q %v", merges, vs[1].NextWords, err)
		}
	}

	// a pending Skip once moves the next run past the one it skips: the
	// bump's to the Monday after, the audit's by another 20 merges
	for _, rt := range []string{"weekly-bump", "audit"} {
		if err := store.SetRoutineSkip(ctx, "ai-software-factory-code", rt, true, "sebastian"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetRoutineMerges(ctx, "ai-software-factory-code", "audit", 14); err != nil {
		t.Fatal(err)
	}
	vs, err = r.Views(ctx, "ai-software-factory-code")
	if err != nil {
		t.Fatal(err)
	}
	if !vs[0].Next.Equal(time.Date(2026, 10, 19, 6, 0, 0, 0, berlin)) || vs[0].NextWords != "Mon 19 Oct 06:00" || vs[0].Soon ||
		vs[1].NextWords != "after 26 more merges" || vs[1].MergesLeft != 26 {
		t.Fatalf("skipped: %+v", vs)
	}
	// past the count the pass's run takes the skip and keeps 25 % 20 = 5
	if err := store.SetRoutineMerges(ctx, "ai-software-factory-code", "audit", 25); err != nil {
		t.Fatal(err)
	}
	if vs, err := r.Views(ctx, "ai-software-factory-code"); err != nil || vs[1].NextWords != "after 15 more merges" || vs[1].MergesLeft != 15 {
		t.Fatalf("skipped past the count: %q %d %v", vs[1].NextWords, vs[1].MergesLeft, err)
	}

	// linkshort's board shows the factory's template pass with its state,
	// the skipped 06:03 run passed over
	if err := store.SetRoutineSkip(ctx, "", "template-pass", true, "sebastian"); err != nil {
		t.Fatal(err)
	}
	vs, err = r.Views(ctx, "linkshort")
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 || vs[0].Project != "linkshort" || vs[0].State.Project != "" || !vs[0].State.SkipNext ||
		vs[0].NextWords != "Mon 07:03" || vs[0].Soon || vs[0].Lane != "queued" {
		t.Fatalf("linkshort: %+v", vs)
	}
	if _, ok := store.routineStates["linkshort/template-pass"]; ok {
		t.Fatal("a view wrote a state row")
	}
	// without the skip the hourly pass is 23 minutes away and not soon:
	// the run after it comes within the day
	if err := store.SetRoutineSkip(ctx, "", "template-pass", false, ""); err != nil {
		t.Fatal(err)
	}
	if vs, err := r.Views(ctx, "linkshort"); err != nil || vs[0].NextWords != "Mon 06:03" || !vs[0].Next.Equal(mon0540.Add(23*time.Minute)) || vs[0].Soon {
		t.Fatalf("the hourly pass: %+v %v, want 06:03 and not soon", vs, err)
	}

	// past a week the date is named, and an event says its words
	*tree = withRoutines(*tree, "demo-app",
		policy.Routine{Name: "far", Title: "Far", Files: policy.FilesHuman, Schedule: []policy.Trigger{{Cron: "0 6 20 10 *"}}},
		policy.Routine{Name: "smoke", Title: "Smoke", Files: policy.FilesReport, Schedule: []policy.Trigger{{On: policy.OnPushMain}}})
	vs, err = r.Views(ctx, "demo-app")
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 || vs[0].NextWords != "Tue 20 Oct 06:00" || vs[0].Soon || vs[1].NextWords != "on each push to main" || vs[1].Soon {
		t.Fatalf("demo-app: %+v", vs)
	}

	// a daily run 20 minutes away is soon on the eve of the spring's
	// clock change, the run after it 23 h later
	*tree = withRoutines(*tree, "demo-app",
		policy.Routine{Name: "daily", Title: "Daily", Files: policy.FilesHuman, Schedule: []policy.Trigger{{Cron: "0 6 * * *"}}})
	r.Now = func() time.Time { return time.Date(2026, 3, 28, 5, 40, 0, 0, berlin) }
	if vs, err := r.Views(ctx, "demo-app"); err != nil || len(vs) != 1 || vs[0].NextWords != "Sat 06:00" || !vs[0].Soon {
		t.Fatalf("the daily run before the clock change: %+v %v, want Sat 06:00 and soon", vs, err)
	}
}

// The loop reconciles and starts the configuration-version routines at
// its start, passes the merges every poll, and logs a failure once until
// it recovers.
func TestRoutinesLoop(t *testing.T) {
	r, store, sched, wf, _ := routinesRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	events := &fakeEvents{rows: []Event{{ID: 9, Repository: "sf/ai-software-factory-code", Kind: EventPullRequestMerged}},
		onRead: func(reads int) {
			if reads == 3 {
				cancel()
			}
		}}
	r.Events = events
	r.Loop(ctx, time.Millisecond)
	if len(sched.have) != 2 || len(wf.routineRuns) != 1 || wf.routineRuns[0].Trigger != "configuration-version" {
		t.Fatalf("schedules %v, runs %+v", slices.Sorted(maps.Keys(sched.have)), wf.routineRuns)
	}
	if c, _ := store.RoutineMergeCursor(t.Context()); c != 9 || events.reads != 3 {
		t.Fatalf("cursor %d after %d reads", c, events.reads)
	}

}

// The loop logs a failure once while it lasts, and again when it comes
// back after a recovery.
func TestRoutinesLoopLogsAFailureOnceUntilItRecovers(t *testing.T) {
	r, _, _, _, _ := routinesRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	down := errors.New("events down")
	events := &fakeEvents{err: down}
	events.onRead = func(reads int) {
		switch reads {
		case 2:
			events.err = nil
		case 3:
			events.err = down
		case 4:
			cancel()
		}
	}
	r.Events = events
	logs := captureLog(t)
	r.Loop(ctx, time.Millisecond)
	if got := strings.Count(logs.String(), "events down"); got != 2 {
		t.Fatalf("logged %d times:\n%s", got, logs)
	}
}

// The loop reconciles again every hour, which sets back a schedule deleted
// by hand, and at a new version, which also starts the
// configuration-version routines again.
func TestRoutinesLoopReconcilesHourlyAndAtANewVersion(t *testing.T) {
	r, _, sched, wf, tree := routinesRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	v, now := policy.Version{ID: 7, Tree: *tree, ActivatedAt: mon0540}, mon0540
	r.Current = func() policy.Version { return v }
	r.Now = func() time.Time { return now }
	const hourly = "routine//template-pass/0"
	events := &fakeEvents{}
	events.onRead = func(reads int) {
		switch reads {
		case 1:
			delete(sched.have, hourly)
			now = now.Add(30 * time.Minute)
		case 2:
			if _, ok := sched.have[hourly]; ok {
				t.Error("reconciled within the hour")
			}
			now = now.Add(31 * time.Minute)
		case 3:
			if _, ok := sched.have[hourly]; !ok {
				t.Error("an hour on, the deleted schedule is still missing")
			}
			v.ID, v.ActivatedAt = 8, now
			v.Tree = withRoutines(v.Tree, "ai-software-factory-code", policy.Routine{Name: "weekly-bump", Title: "Weekly bump", Files: policy.FilesHuman,
				Schedule: []policy.Trigger{{Cron: "0 7 * * 1"}}})
		case 4:
			cancel()
		}
	}
	r.Events = events
	r.Loop(ctx, time.Millisecond)
	if got := sched.have["routine/ai-software-factory-code/weekly-bump/0"].Cron; got != "0 7 * * 1" {
		t.Fatalf("the new version's cron %q", got)
	}
	if len(wf.routineRuns) != 2 || wf.routineRuns[0].Due != mon0540 || wf.routineRuns[1].Due != v.ActivatedAt {
		t.Fatalf("configuration-version runs %+v", wf.routineRuns)
	}
}
