package factoryd

import (
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// routineCardStore is a routine store that also files a routine's card and
// ends it.
type routineCardStore interface {
	RoutineStore
	CreateTask(ctx context.Context, t Task) error
	TransitionTask(ctx context.Context, id string, from []string, to string) error
}

func TestPGStoreRoutineRuns(t *testing.T) { checkRoutineStore(t, routinePGStore(t)) }

// Two runs of a routine with a state row that begin at the same time: the
// same due begins once, and two dues take the numbers 1 and 2.
func TestPGStoreRoutineRunsBeginConcurrently(t *testing.T) {
	s := routinePGStore(t)
	due := time.Date(2026, 10, 13, 4, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		routine string
		dues    [2]time.Time
	}{
		{"same-due", [2]time.Time{due, due}},
		{"two-dues", [2]time.Time{due, due.Add(time.Hour)}},
	} {
		if _, err := s.RoutineState(t.Context(), "linkshort", tc.routine); err != nil {
			t.Fatal(err)
		}
		var runs [2]RoutineRun
		var began [2]bool
		var errs [2]error
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Go(func() {
				<-start
				runs[i], began[i], errs[i] = s.BeginRoutineRun(t.Context(), RoutineRun{Project: "linkshort", Routine: tc.routine, Due: tc.dues[i], Trigger: "cron"}, true)
			})
		}
		close(start)
		wg.Wait()
		if err := errors.Join(errs[:]...); err != nil {
			t.Fatalf("%s: %v", tc.routine, err)
		}
		numbers := map[int]bool{runs[0].Run: true, runs[1].Run: true}
		switch tc.routine {
		case "same-due":
			if began[0] == began[1] || len(numbers) != 1 || !numbers[1] {
				t.Fatalf("the same due: began %v, runs %+v", began, runs)
			}
		case "two-dues":
			if !began[0] || !began[1] || len(numbers) != 2 || !numbers[1] || !numbers[2] {
				t.Fatalf("two dues: began %v, runs %+v", began, runs)
			}
		}
	}
}

// The merge pass reads the merges after its cursor, oldest first, a batch
// at a time.
func TestPGStoreMergedEvents(t *testing.T) {
	ctx := t.Context()
	s := routinePGStore(t)
	var ids []int64
	for _, e := range []Event{{Repository: "sf/code", Kind: EventPullRequestMerged}, {Repository: "sf/code", Kind: EventTaskEnded},
		{Repository: "demo/app", Kind: EventPullRequestMerged}} {
		id, err := s.Record(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	got := func(after int64, limit int) []int64 {
		es, err := s.MergedEvents(ctx, after, limit)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, e := range es {
			out = append(out, e.ID)
		}
		return out
	}
	if all := got(ids[0]-1, 10); !slices.Equal(all, []int64{ids[0], ids[2]}) {
		t.Fatalf("after %d: %v, recorded %v", ids[0]-1, all, ids)
	}
	if rest := got(ids[0], 10); !slices.Equal(rest, []int64{ids[2]}) {
		t.Fatalf("after %d: %v", ids[0], rest)
	}
	if first := got(ids[0]-1, 1); !slices.Equal(first, []int64{ids[0]}) {
		t.Fatalf("limit 1: %v", first)
	}
}

// A count that fails part-way counts none of the batch and leaves the
// cursor, so the next pass reads the batch again.
func TestPGStoreCountRoutineMergesIsOneTransaction(t *testing.T) {
	ctx := t.Context()
	s := routinePGStore(t)
	for _, project := range []string{"demo-app", "linkshort"} {
		if _, err := s.RoutineState(ctx, project, "audit"); err != nil {
			t.Fatal(err)
		}
	}
	// linkshort, counted after demo-app, overflows its column
	if err := s.SetRoutineMerges(ctx, "linkshort", "audit", math.MaxInt32); err != nil {
		t.Fatal(err)
	}
	if err := s.CountRoutineMerges(ctx, 9, map[string]int{"demo-app": 1, "linkshort": 1}); err == nil {
		t.Fatal("an overflowing count succeeded")
	}
	if st, err := s.RoutineState(ctx, "demo-app", "audit"); err != nil || st.Merges != 0 {
		t.Fatalf("demo-app counted %+v %v", st, err)
	}
	if c, err := s.RoutineMergeCursor(ctx); err != nil || c != 0 {
		t.Fatalf("the cursor moved to %d %v", c, err)
	}
}

// routinePGStore is the Postgres store on its own database, its routine
// rows and tasks wiped and its merge cursor at 0.
func routinePGStore(t *testing.T) *PGStore {
	t.Helper()
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_routines"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.db.Exec(`TRUNCATE routine_state, routine_runs, tasks CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE routine_merge_cursor SET last_event_id = 0`); err != nil {
		t.Fatal(err)
	}
	return s
}

// The fake answers as the Postgres store does, so the tests that run on it
// hold for the factory.
func TestFakeStoreRoutineRuns(t *testing.T) { checkRoutineStore(t, newFakeStore()) }

// A routine's runs are numbered from 1, begin once per due time, consume a
// pending Skip once (a Run now leaves it), end with their card and are
// found by it. The merge
// counters count per project, and the merge pass's cursor holds its place.
func checkRoutineStore(t *testing.T, s routineCardStore) {
	ctx := t.Context()
	due := time.Date(2026, 10, 13, 4, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	begin := func(due time.Time) (RoutineRun, bool) {
		t.Helper()
		r, ok, err := s.BeginRoutineRun(ctx, RoutineRun{Project: "linkshort", Routine: "weekly-bump", Due: due, Trigger: "cron"}, true)
		if err != nil {
			t.Fatalf("BeginRoutineRun %s: %v", due, err)
		}
		return r, ok
	}
	state := func(project, routine string) RoutineState {
		t.Helper()
		st, err := s.RoutineState(ctx, project, routine)
		if err != nil {
			t.Fatalf("RoutineState %s/%s: %v", project, routine, err)
		}
		return st
	}

	r1, ok := begin(due)
	if !ok || r1.Run != 1 || r1.Outcome != "" || !r1.Due.Equal(due) || r1.Trigger != "cron" || !r1.Ended.IsZero() {
		t.Fatalf("first: %+v %v", r1, ok)
	}
	if again, ok := begin(due); ok || again.Run != 1 {
		t.Fatalf("the same due began twice: %+v %v", again, ok)
	}
	if err := s.SetRoutineSkip(ctx, "linkshort", "weekly-bump", true, "sebastian"); err != nil {
		t.Fatal(err)
	}
	r2, ok := begin(due.Add(week))
	if !ok || r2.Run != 2 || r2.Outcome != "skipped" || r2.Note != "skipped once by sebastian" {
		t.Fatalf("skip: %+v %v", r2, ok)
	}
	if replay, ok := begin(due.Add(week)); ok || replay.Run != 2 || replay.Outcome != "skipped" || replay.Note != "skipped once by sebastian" {
		t.Fatalf("the skipped run's replay: %+v %v", replay, ok)
	}
	if st := state("linkshort", "weekly-bump"); st.SkipNext || st.SkipBy != "" || st.Runs != 2 {
		t.Fatalf("state after the skip: %+v", st)
	}

	if err := s.RoutineRunFiled(ctx, "linkshort", "weekly-bump", 1, "t-1", 790); err != nil {
		t.Fatal(err)
	}
	runs, err := s.RoutineRuns(ctx, "linkshort", "weekly-bump", 10)
	if err != nil || len(runs) != 2 || runs[0].Run != 2 || !runs[0].Ended.IsZero() {
		t.Fatalf("runs %+v %v", runs, err)
	}
	if r := runs[1]; r.Run != 1 || r.TaskID != "t-1" || r.Issue != 790 || r.Outcome != "filed" || !r.Ended.IsZero() || r.Started.IsZero() {
		t.Fatalf("the filed run %+v", r)
	}
	if last, err := s.RoutineRuns(ctx, "linkshort", "weekly-bump", 1); err != nil || len(last) != 1 || last[0].Run != 2 {
		t.Fatalf("the last run %+v %v", last, err)
	}
	if err := s.RoutineRunFiled(ctx, "linkshort", "weekly-bump", 9, "t-9", 799); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a run that never began filed: %v", err)
	}
	if err := s.EndRoutineRun(ctx, "linkshort", "weekly-bump", 9, "done", "", "", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a run that never began ended: %v", err)
	}

	// a later run held by the open card names it too, the run that filed it stays its run
	if r3, ok := begin(due.Add(2 * week)); !ok || r3.Run != 3 || r3.Outcome != "" {
		t.Fatalf("third: %+v %v", r3, ok)
	}
	if err := s.EndRoutineRun(ctx, "linkshort", "weekly-bump", 3, "held", "held: #790 is still open", "t-1", 790, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.RoutineRunFiled(ctx, "linkshort", "weekly-bump", 3, "t-3", 791); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an ended run filed: %v", err)
	}
	if got, ok, err := s.RoutineRunOfTask(ctx, "t-1"); err != nil || !ok || got.Run != 1 || got.Routine != "weekly-bump" || got.Project != "linkshort" || !got.Ended.IsZero() {
		t.Fatalf("the run of t-1: %+v %v %v", got, ok, err)
	}
	if _, ok, err := s.RoutineRunOfTask(ctx, "t-none"); err != nil || ok {
		t.Fatalf("the run of a card no routine filed: %v %v", ok, err)
	}

	if _, ok, err := s.OpenRoutineTask(ctx, "linkshort", "weekly-bump"); err != nil || ok {
		t.Fatalf("an open task before its row exists: %v %v", ok, err)
	}
	if err := s.CreateTask(ctx, Task{ID: "t-1", Kind: "human", Status: "waiting-human", BudgetEUR: 1, Repository: "sf/linkshort", Source: SourceRoutine}); err != nil {
		t.Fatal(err)
	}
	if open, ok, err := s.OpenRoutineTask(ctx, "linkshort", "weekly-bump"); err != nil || !ok || open.ID != "t-1" {
		t.Fatalf("the open task: %+v %v %v", open, ok, err)
	}
	if _, ok, err := s.OpenRoutineTask(ctx, "linkshort", "audit"); err != nil || ok {
		t.Fatalf("another routine's open task: %v %v", ok, err)
	}
	if err := s.TransitionTask(ctx, "t-1", []string{"waiting-human"}, "done"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.OpenRoutineTask(ctx, "linkshort", "weekly-bump"); err != nil || ok {
		t.Fatalf("a done task is open: %v %v", ok, err)
	}
	if err := s.EndRoutineRun(ctx, "linkshort", "weekly-bump", 1, "merged", "", "t-1", 790, 1.25); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.RoutineRunOfTask(ctx, "t-1"); err != nil || !ok || got.Run != 1 || got.Outcome != "merged" || got.Ended.IsZero() || got.CostEUR != 1.25 || got.TaskID != "t-1" || got.Issue != 790 {
		t.Fatalf("the ended run of t-1: %+v %v %v", got, ok, err)
	}

	if st := state("linkshort", "audit"); st != (RoutineState{Project: "linkshort", Routine: "audit"}) {
		t.Fatalf("a missing state reads %+v, want zero", st)
	}
	state("demo-app", "audit")
	if c, err := s.RoutineMergeCursor(ctx); err != nil || c != 0 {
		t.Fatalf("the first cursor %d %v", c, err)
	}
	// a project without state rows counts nowhere
	if err := s.CountRoutineMerges(ctx, 7, map[string]int{"linkshort": 3, "sf-none": 2}); err != nil {
		t.Fatal(err)
	}
	states, err := s.RoutineStates(ctx, "linkshort")
	if err != nil || len(states) != 2 || states[0].Routine != "audit" || states[0].Merges != 3 || states[1].Routine != "weekly-bump" || states[1].Merges != 3 {
		t.Fatalf("counted %+v %v", states, err)
	}
	if st := state("demo-app", "audit"); st.Merges != 0 {
		t.Fatalf("another project's state was counted: %+v", st)
	}
	if c, err := s.RoutineMergeCursor(ctx); err != nil || c != 7 {
		t.Fatalf("the cursor after a count %d %v", c, err)
	}
	if err := s.SetRoutineMerges(ctx, "linkshort", "weekly-bump", 0); err != nil {
		t.Fatal(err)
	}
	if all, err := s.RoutineStates(ctx, "linkshort"); err != nil || len(all) != 2 || all[0].Merges != 3 || all[1].Merges != 0 || all[1].Runs != 3 {
		t.Fatalf("states %+v %v", all, err)
	}
	if none, err := s.RoutineStates(ctx, "sf-none"); err != nil || len(none) != 0 {
		t.Fatalf("a project with no routines: %+v %v", none, err)
	}

	if err := s.SetRoutineSkip(ctx, "", "drift", true, "anna"); err != nil {
		t.Fatal(err)
	}
	if st := state("", "drift"); !st.SkipNext || st.SkipBy != "anna" {
		t.Fatalf("a skip on a routine without a row: %+v", st)
	}
	if err := s.SetRoutineSkip(ctx, "", "drift", false, "anna"); err != nil {
		t.Fatal(err)
	}
	if st := state("", "drift"); st.SkipNext || st.SkipBy != "" {
		t.Fatalf("an undone skip: %+v", st)
	}

	// a run that takes no skip begins unskipped and leaves the mark to the
	// next run that does
	if err := s.SetRoutineSkip(ctx, "", "drift", true, "anna"); err != nil {
		t.Fatal(err)
	}
	now, ok, err := s.BeginRoutineRun(ctx, RoutineRun{Routine: "drift", Due: due, Trigger: TriggerRunNow}, false)
	if err != nil || !ok || now.Run != 1 || now.Outcome != "" || now.Note != "" {
		t.Fatalf("the Run now: %+v %v %v", now, ok, err)
	}
	if st := state("", "drift"); !st.SkipNext || st.SkipBy != "anna" || st.Runs != 1 {
		t.Fatalf("the mark after a Run now: %+v", st)
	}
	next, ok, err := s.BeginRoutineRun(ctx, RoutineRun{Routine: "drift", Due: due.Add(time.Hour), Trigger: "cron"}, true)
	if err != nil || !ok || next.Run != 2 || next.Outcome != "skipped" || next.Note != "skipped once by anna" {
		t.Fatalf("the cron run after the Run now: %+v %v %v", next, ok, err)
	}
	if st := state("", "drift"); st.SkipNext || st.SkipBy != "" || st.Runs != 2 {
		t.Fatalf("the mark after the skipped run: %+v", st)
	}
}
