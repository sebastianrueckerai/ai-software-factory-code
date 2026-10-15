package factoryd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// Routines starts the routines' runs (roadmap 267): a Temporal schedule
// for each cron trigger of the tree, the merge counters for the merge
// triggers, and the runs of the event triggers and of Run now.
type Routines struct {
	Store     RoutineStore
	Events    MergeEvents
	Current   func() policy.Version
	Schedules Schedules
	Workflows Workflows
	Now       func() time.Time
}

// MergeEvents reads the merges the merge pass counts.
type MergeEvents interface {
	// MergedEvents are the pull_request_merged events with an id above
	// after, oldest first, at most limit.
	MergedEvents(ctx context.Context, after int64, limit int) ([]Event, error)
}

// RoutineView is a routine as a project's board shows it. Project is the
// board's project and State.Project the routine's key in the store, ""
// for policy.yaml's template pass, which runs once for every project with
// a template.
type RoutineView struct {
	Project string
	Routine policy.Routine
	State   RoutineState
	// Next is the next cron run, zero for a routine without a cron
	// trigger, and Soon says it is under an hour away.
	Next time.Time
	Soon bool
	// NextWords is "Mon 06:00" within a week, else "Mon 20 Oct 06:00",
	// "after 6 more merges" or the event's words.
	NextWords string
	// Last is the newest run, zero before the first.
	Last RoutineRun
	// Open is the open card of the routine's runs, nil for none, and Held
	// says the routine holds its runs while it is open.
	Open *Task
	Held bool
	// Lane is "needs-you" for a routine that files human, else "queued".
	Lane string
}

const (
	// mergeBatch bounds the events one read of the merge pass takes.
	mergeBatch = 100
	// reconcileEvery sets back a schedule changed or deleted by hand in
	// Temporal.
	reconcileEvery = time.Hour
)

// Loop reconciles the schedules at its start, at each new version and
// every reconcileEvery, starts the configuration-version routines once
// per version, and passes the merges every poll until ctx ends. A failure
// logs once until it recovers.
func (r *Routines) Loop(ctx context.Context, poll time.Duration) {
	var reconciled, started int64
	var at time.Time
	var last string
	for {
		v := r.Current()
		var errs []error
		if v.ID != reconciled || r.Now().Sub(at) >= reconcileEvery {
			err := r.Reconcile(ctx)
			if err == nil {
				reconciled, at = v.ID, r.Now()
			}
			errs = append(errs, err)
		}
		if v.ID != started {
			err := r.ConfigurationVersion(ctx)
			if err == nil {
				started = v.ID
			}
			errs = append(errs, err)
		}
		errs = append(errs, r.MergePass(ctx))
		if err := errors.Join(errs...); err != nil && err.Error() != last {
			log.Printf("routines: %v", err)
			last = err.Error()
		} else if err == nil {
			last = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}

// Reconcile makes Temporal's routine schedules the tree's cron triggers:
// it creates the missing, updates the changed and deletes the rest. It
// creates the state row of every routine first, which the merge pass
// bumps.
func (r *Routines) Reconcile(ctx context.Context) error {
	tree := r.Current().Tree
	zone := tree.Document.TimeZone
	if zone == "" {
		zone = "UTC"
	}
	desired := map[string]ScheduleSpec{}
	for key, rs := range ownRoutines(tree) {
		for _, rt := range rs {
			if _, err := r.Store.RoutineState(ctx, key, rt.Name); err != nil {
				return err
			}
			// never 0, which reads back as the server's default
			window := 24 * time.Hour
			if rt.MissedRule() == policy.MissedSkip {
				window = 10 * time.Second
			}
			for i, t := range rt.Schedule {
				if t.Cron == "" {
					continue
				}
				in := RoutineRunInput{Project: key, Routine: rt.Name, Trigger: "cron"}
				id := routineRunPrefix(in) + "/" + strconv.Itoa(i)
				desired[id] = ScheduleSpec{ID: id, Cron: t.Cron, TimeZone: zone, CatchupWindow: window, Input: in}
			}
		}
	}
	have, err := r.Schedules.List(ctx)
	if err != nil {
		return fmt.Errorf("list the schedules: %w", err)
	}
	changed := 0
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(desired)) {
		want := desired[id]
		got, ok := have[id]
		var err error
		switch {
		case !ok:
			err = r.Schedules.Create(ctx, want)
		case got.Cron != want.Cron || got.TimeZone != want.TimeZone || got.CatchupWindow != want.CatchupWindow:
			err = r.Schedules.Update(ctx, want)
		default:
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("schedule %s: %w", id, err))
			continue
		}
		changed++
	}
	for _, id := range slices.Sorted(maps.Keys(have)) {
		if _, ok := desired[id]; ok {
			continue
		}
		if err := r.Schedules.Delete(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("delete schedule %s: %w", id, err))
			continue
		}
		changed++
	}
	if changed > 0 {
		log.Printf("routines: reconciled %d schedules", changed)
	}
	return errors.Join(errs...)
}

// ownRoutines are the tree's routines by their key in the store: "" for
// policy.yaml's, else the project's name.
func ownRoutines(tree policy.Tree) map[string][]policy.Routine {
	out := map[string][]policy.Routine{"": tree.Document.Routines}
	for name, p := range tree.Projects {
		out[name] = p.Routines
	}
	return out
}

// MergePass counts the merges after its cursor on their projects' state
// rows, then starts the run of each routine whose count is reached and
// keeps the count's remainder. A run that failed to start is tried again
// at the next pass.
func (r *Routines) MergePass(ctx context.Context) error {
	tree := r.Current().Tree
	if err := r.countMerges(ctx, tree); err != nil {
		return err
	}
	var errs []error
	for _, project := range slices.Sorted(maps.Keys(tree.Projects)) {
		rs := tree.Projects[project].Routines
		if !slices.ContainsFunc(rs, func(rt policy.Routine) bool { return rt.MergesEvery() > 0 }) {
			continue
		}
		states, err := r.Store.RoutineStates(ctx, project)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, st := range states {
			rt, _ := tree.Routine(project, st.Routine)
			every := rt.MergesEvery()
			if every == 0 || st.Merges < every {
				continue
			}
			in := RoutineRunInput{Project: project, Routine: rt.Name, Trigger: "merges", Due: r.Now()}
			if err := r.Workflows.StartRoutineRun(ctx, in); err != nil {
				errs = append(errs, fmt.Errorf("start %s/%s: %w", project, rt.Name, err))
				continue
			}
			// a crash before the count is set starts a second run at the
			// next pass, which a routine that holds while open holds
			if err := r.Store.SetRoutineMerges(ctx, project, rt.Name, st.Merges%every); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// countMerges adds the merges after the cursor to the state rows of their
// repositories' projects, a batch at a time, each batch with its cursor.
// A repository of no project counts for none.
func (r *Routines) countMerges(ctx context.Context, tree policy.Tree) error {
	after, err := r.Store.RoutineMergeCursor(ctx)
	if err != nil {
		return err
	}
	for {
		events, err := r.Events.MergedEvents(ctx, after, mergeBatch)
		if err != nil || len(events) == 0 {
			return err
		}
		merged := map[string]int{}
		for _, e := range events {
			if project, _, ok := tree.ProjectOf(e.Repository); ok {
				merged[project]++
			}
		}
		after = events[len(events)-1].ID
		if err := r.Store.CountRoutineMerges(ctx, after, merged); err != nil {
			return err
		}
		if len(events) < mergeBatch {
			return nil
		}
	}
}

// ConfigurationVersion starts the routines with on:
// configuration-version, policy.yaml's and each project's own, due when
// the version in force came into force: a second start for the same
// version, after a restart or a failed pass, is the same run.
func (r *Routines) ConfigurationVersion(ctx context.Context) error {
	v := r.Current()
	own := ownRoutines(v.Tree)
	var errs []error
	for _, key := range slices.Sorted(maps.Keys(own)) {
		errs = append(errs, r.startOn(ctx, key, own[key], policy.OnConfigurationVersion, v.ActivatedAt))
	}
	return errors.Join(errs...)
}

// MainPushed starts project's routines with on: push-main.
func (r *Routines) MainPushed(ctx context.Context, project string) error {
	return r.startOn(ctx, project, r.Current().Tree.Projects[project].Routines, policy.OnPushMain, r.Now())
}

// startOn starts each of key's routines rs that has the trigger on.
func (r *Routines) startOn(ctx context.Context, key string, rs []policy.Routine, on string, due time.Time) error {
	var errs []error
	for _, rt := range rs {
		if rt.Has(on) {
			errs = append(errs, r.Workflows.StartRoutineRun(ctx, RoutineRunInput{Project: key, Routine: rt.Name, Trigger: on, Due: due}))
		}
	}
	return errors.Join(errs...)
}

// RunNow starts a run of project's routine, due now. A project's template
// pass runs the factory's.
func (r *Routines) RunNow(ctx context.Context, project, routine string) error {
	key, ok := routineKey(r.Current().Tree, project, routine)
	if !ok {
		return fmt.Errorf("routine %s of project %q: %w", routine, project, ErrNotFound)
	}
	return r.Workflows.StartRoutineRun(ctx, RoutineRunInput{Project: key, Routine: routine, Trigger: "run-now", Due: r.Now()})
}

// routineKey is the store's key of the routine name that project's board
// shows: project for the project's own, "" for policy.yaml's. False when
// project runs no such routine.
func routineKey(tree policy.Tree, project, name string) (string, bool) {
	if _, ok := tree.Routine(project, name); ok {
		return project, true
	}
	return "", slices.ContainsFunc(tree.ProjectRoutines(project), func(rt policy.Routine) bool { return rt.Name == name })
}

// Views are project's routines as its board shows them: its own, then
// policy.yaml's template pass when it has a template.
func (r *Routines) Views(ctx context.Context, project string) ([]RoutineView, error) {
	v := r.Current()
	loc, err := v.Document.Location()
	if err != nil {
		return nil, err
	}
	states := map[[2]string]RoutineState{}
	for _, key := range []string{project, ""} {
		sts, err := r.Store.RoutineStates(ctx, key)
		if err != nil {
			return nil, err
		}
		for _, st := range sts {
			states[[2]string{key, st.Routine}] = st
		}
	}
	now := r.Now()
	var out []RoutineView
	for _, rt := range v.ProjectRoutines(project) {
		key, _ := routineKey(v.Tree, project, rt.Name)
		st, ok := states[[2]string{key, rt.Name}]
		if !ok {
			st = RoutineState{Project: key, Routine: rt.Name}
		}
		view := RoutineView{Project: project, Routine: rt, State: st, Lane: "queued"}
		if rt.Files == policy.FilesHuman {
			view.Lane = "needs-you"
		}
		if next, ok := rt.NextCron(now, loc); ok {
			view.Next, view.Soon = next, next.Sub(now) < time.Hour
			view.NextWords = next.Format("Mon 15:04")
			if next.Sub(now) >= 7*24*time.Hour {
				view.NextWords = next.Format("Mon 2 Jan 15:04")
			}
		} else if every := rt.MergesEvery(); every > 0 {
			view.NextWords = "after 1 more merge"
			if n := every - st.Merges; n > 1 {
				view.NextWords = fmt.Sprintf("after %d more merges", n)
			}
		} else {
			view.NextWords = rt.Words()
		}
		runs, err := r.Store.RoutineRuns(ctx, key, rt.Name, 1)
		if err != nil {
			return nil, err
		}
		if len(runs) > 0 {
			view.Last = runs[0]
		}
		open, ok, err := r.Store.OpenRoutineTask(ctx, key, rt.Name)
		if err != nil {
			return nil, err
		}
		if ok {
			view.Open = &open
			view.Held = rt.WhileOpenRule() == policy.WhileOpenHold
		}
		out = append(out, view)
	}
	return out, nil
}
