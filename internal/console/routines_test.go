package console

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// fakeRoutines is the Routines port over views and runs set by a test.
// runs are keyed by project+"/"+routine, newest first; skips holds the
// login of a pending Skip once, "" once undone; ran lists each Run now as
// project/routine. runNow, when set, is the run a Run now leaves, which
// the fake numbers after the newest and stores; without it a Run now
// leaves no run. A Run now leaves a pending skip, as the store does.
type fakeRoutines struct {
	mu     sync.Mutex
	views  []factoryd.RoutineView
	runs   map[string][]factoryd.RoutineRun
	skips  map[string]string
	ran    []string
	runNow func(project, routine string) (factoryd.RoutineRun, error)
	// limits are the limits Runs was asked for, runsErr its failure, and
	// onSkip changes the views when a Skip once is set or undone, under mu
	limits  []int
	runsErr error
	onSkip  func(project, routine string, skip bool)
}

// known reports whether project's board shows routine, as the service's
// routine key does; the caller holds mu.
func (f *fakeRoutines) known(project, routine string) error {
	if slices.ContainsFunc(f.views, func(v factoryd.RoutineView) bool { return v.Project == project && v.Routine.Name == routine }) {
		return nil
	}
	return fmt.Errorf("routine %s of project %q: %w", routine, project, factoryd.ErrNotFound)
}

func (f *fakeRoutines) Views(_ context.Context, project string) ([]factoryd.RoutineView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []factoryd.RoutineView
	for _, v := range f.views {
		if v.Project == project {
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *fakeRoutines) Runs(_ context.Context, project, routine string, limit int) ([]factoryd.RoutineRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.known(project, routine); err != nil {
		return nil, err
	}
	f.limits = append(f.limits, limit)
	if f.runsErr != nil {
		return nil, f.runsErr
	}
	runs := f.runs[project+"/"+routine]
	return slices.Clone(runs[:min(limit, len(runs))]), nil
}

func (f *fakeRoutines) RunOfTask(_ context.Context, taskID string) (factoryd.RoutineRun, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var first factoryd.RoutineRun
	for _, runs := range f.runs {
		for _, run := range runs {
			if run.TaskID == taskID && (first.Run == 0 || run.Run < first.Run) {
				first = run
			}
		}
	}
	return first, first.Run > 0, nil
}

func (f *fakeRoutines) Skip(_ context.Context, project, routine string, skip bool, by string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.known(project, routine); err != nil {
		return err
	}
	if f.skips == nil {
		f.skips = map[string]string{}
	}
	if !skip {
		by = ""
	}
	f.skips[project+"/"+routine] = by
	if f.onSkip != nil {
		f.onSkip(project, routine, skip)
	}
	return nil
}

func (f *fakeRoutines) RunNow(_ context.Context, project, routine string) error {
	key := project + "/" + routine
	f.mu.Lock()
	err := f.known(project, routine)
	if err == nil {
		f.ran = append(f.ran, key)
	}
	hook := f.runNow
	f.mu.Unlock()
	if err != nil || hook == nil {
		return err
	}
	run, err := hook(project, routine)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runs == nil {
		f.runs = map[string][]factoryd.RoutineRun{}
	}
	run.Project, run.Routine, run.Run, run.Trigger = project, routine, 1, cmp.Or(run.Trigger, factoryd.TriggerRunNow)
	if runs := f.runs[key]; len(runs) > 0 {
		run.Run = runs[0].Run + 1
	}
	f.runs[key] = append([]factoryd.RoutineRun{run}, f.runs[key]...)
	return nil
}

// weeklyBump is the code project's weekly bump as its board shows it
// with a Skip once pending: the next run after the skipped one.
func weeklyBump() factoryd.RoutineView {
	return factoryd.RoutineView{Project: "ai-software-factory-code",
		Routine: policy.Routine{Name: "weekly-bump", Title: "Weekly bump", Files: policy.FilesHuman, Schedule: []policy.Trigger{{Cron: "0 6 * * 1"}}},
		State:   factoryd.RoutineState{Project: "ai-software-factory-code", Routine: "weekly-bump"},
		Next:    time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC), NextWords: "Mon 20 Oct 06:00", Lane: "needs-you"}
}

// htmxPost is post as htmx sends it: the answer is the board's fragment.
func htmxPost(path string, form url.Values) *http.Request {
	req := post(path, form)
	req.Header.Set("HX-Request", "true")
	return req
}

// Skip once marks the routine's next run for the member and answers with
// the run after it; the undo clears the mark. Each is one event with its
// via on the project's repository.
func TestRoutineSkipAnswersWithTheNextRun(t *testing.T) {
	r := newRig(t)
	r.routines.views = []factoryd.RoutineView{weeklyBump()}
	ck, s := r.loggedIn(t)
	const path = "/routines/ai-software-factory-code/weekly-bump/skip"

	rec := r.do(htmxPost(path, url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<p class="note" role="status">Skipped once · next Mon 20 Oct 06:00</p>`) {
		t.Fatalf("skip = %d:\n%s", rec.Code, rec.Body)
	}
	if by := r.routines.skips["ai-software-factory-code/weekly-bump"]; by != "sebastian" {
		t.Fatalf("the skip reached the port as %q", by)
	}
	rec = r.do(htmxPost(path, url.Values{"csrf": {s.csrf}, "undo": {"1"}}), ck)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<p class="note" role="status">Skip once undone</p>`) {
		t.Fatalf("undo = %d:\n%s", rec.Code, rec.Body)
	}
	if by, ok := r.routines.skips["ai-software-factory-code/weekly-bump"]; !ok || by != "" {
		t.Fatalf("the undo left %q", by)
	}
	var got []string
	for _, e := range r.events.all() {
		got = append(got, fmt.Sprintf("%s %s %s %s %v", e.Kind, e.Repository, e.Actor, e.Via, e.Detail))
	}
	equal(t, "the events", got, []string{
		"routine_skipped sf/code sebastian button map[project:ai-software-factory-code routine:weekly-bump undo:false]",
		"routine_skipped sf/code sebastian button map[project:ai-software-factory-code routine:weekly-bump undo:true]",
	})

	// a browser's form goes back to the board; a routine the project
	// does not run is 404 and leaves no event
	rec = r.do(post(path, url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the form's skip = %d", rec.Code)
	}
	before := r.events.count()
	if rec = r.do(htmxPost("/routines/ai-software-factory-code/nightly/skip", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown routine = %d %s", rec.Code, rec.Body)
	}
	if r.events.count() != before {
		t.Fatal("an unknown routine left an event")
	}
}

// Run now starts a run and answers with what it did once its outcome is
// in: the card it filed and the column it waits in, the hold that kept it,
// or "Filing…" when no outcome came within the wait.
func TestRoutineRunNowAnswersHeld(t *testing.T) {
	wait, poll := runNowWait, runNowPoll
	runNowWait, runNowPoll = 50*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { runNowWait, runNowPoll = wait, poll })

	r := newRig(t)
	r.routines.views = []factoryd.RoutineView{weeklyBump()}
	r.tasks.set("", factoryd.Task{ID: "t-790", Kind: "human", Status: "waiting-human", Repository: "sf/code", IssueNumber: 790, Source: factoryd.SourceRoutine})
	ck, s := r.loggedIn(t)
	const path = "/routines/ai-software-factory-code/weekly-bump/run"
	leaves := func(run factoryd.RoutineRun) func(string, string) (factoryd.RoutineRun, error) {
		return func(string, string) (factoryd.RoutineRun, error) { return run, nil }
	}
	note := func(hook func(string, string) (factoryd.RoutineRun, error)) string {
		t.Helper()
		r.routines.runNow = hook
		rec := r.do(htmxPost(path, url.Values{"csrf": {s.csrf}}), ck)
		if rec.Code != http.StatusOK {
			t.Fatalf("run now = %d %s", rec.Code, rec.Body)
		}
		body := rec.Body.String()
		i := strings.Index(body, `<p class="note" role="status">`)
		if i < 0 {
			t.Fatalf("no note:\n%s", body)
		}
		body = body[i+len(`<p class="note" role="status">`):]
		return body[:strings.Index(body, "</p>")]
	}
	// laterCron is a Run now whose held run a cron run lands after
	laterCron := func(project, routine string) (factoryd.RoutineRun, error) {
		r.routines.mu.Lock()
		defer r.routines.mu.Unlock()
		key := project + "/" + routine
		held := factoryd.RoutineRun{Project: project, Routine: routine, Run: r.routines.runs[key][0].Run + 1, Trigger: factoryd.TriggerRunNow,
			Outcome: "held", Note: "held: #781 is still open"}
		r.routines.runs[key] = append([]factoryd.RoutineRun{held}, r.routines.runs[key]...)
		return factoryd.RoutineRun{Trigger: "cron", Outcome: "no change"}, nil
	}

	equal(t, "held", note(leaves(factoryd.RoutineRun{Outcome: "held", Note: "held: #781 is still open"})), "Held: #781 is still open")
	equal(t, "filed", note(leaves(factoryd.RoutineRun{Outcome: "filed", TaskID: "t-790", Issue: 790})), "Filed #790 in Needs you")
	equal(t, "no change", note(leaves(factoryd.RoutineRun{Outcome: "no change"})), "No change")
	equal(t, "behind a later cron run", note(laterCron), "Held: #781 is still open")
	equal(t, "an earlier run's outcome", note(nil), "Filing…")
	equal(t, "the run of another trigger", note(leaves(factoryd.RoutineRun{Trigger: "cron", Outcome: "filed", TaskID: "t-790", Issue: 790})), "Filing…")
	equal(t, "a run without its outcome", note(leaves(factoryd.RoutineRun{})), "Filing…")

	if len(r.routines.ran) != 7 {
		t.Fatalf("ran %v", r.routines.ran)
	}
	var got []string
	for _, e := range r.events.all() {
		got = append(got, fmt.Sprintf("%s %s %s %s %d %s %v", e.Kind, e.Repository, e.Actor, e.Via, e.IssueNumber, e.TaskID, e.Detail))
	}
	const detail = "map[outcome:%s project:ai-software-factory-code routine:weekly-bump]"
	equal(t, "the events", got, []string{
		"routine_run_now sf/code sebastian button 0  " + fmt.Sprintf(detail, "held"),
		"routine_run_now sf/code sebastian button 790 t-790 " + fmt.Sprintf(detail, "filed"),
		"routine_run_now sf/code sebastian button 0  " + fmt.Sprintf(detail, "no change"),
		"routine_run_now sf/code sebastian button 0  " + fmt.Sprintf(detail, "held"),
		"routine_run_now sf/code sebastian button 0  " + fmt.Sprintf(detail, "pending"),
		"routine_run_now sf/code sebastian button 0  " + fmt.Sprintf(detail, "pending"),
		"routine_run_now sf/code sebastian button 0  " + fmt.Sprintf(detail, "pending"),
	})

	// a pending Skip once stays for the run it was set for
	if rec := r.do(htmxPost("/routines/ai-software-factory-code/weekly-bump/skip", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusOK {
		t.Fatalf("skip = %d %s", rec.Code, rec.Body)
	}
	equal(t, "a Run now beside a skip", note(leaves(factoryd.RoutineRun{Outcome: "no change"})), "No change")
	if by := r.routines.skips["ai-software-factory-code/weekly-bump"]; by != "sebastian" {
		t.Fatalf("the Run now took the skip: %q", by)
	}

	before := r.events.count()
	if rec := r.do(htmxPost("/routines/ai-software-factory-code/nightly/run", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown routine = %d %s", rec.Code, rec.Body)
	}
	if r.events.count() != before || len(r.routines.ran) != 8 {
		t.Fatal("an unknown routine ran or left an event")
	}
}

// Both acts take a writer of the routine's project: a writer of another
// project gets 403 in the writer's words, and nothing reaches the port.
func TestRoutineRoutesNeedTheProjectsWriter(t *testing.T) {
	r := newRig(t)
	members(r)
	r.routines.views = []factoryd.RoutineView{weeklyBump()}
	bk, bs := memberLogin(t, r, "bob")
	for _, act := range []string{"skip", "run"} {
		rec := r.do(htmxPost("/routines/ai-software-factory-code/weekly-bump/"+act, url.Values{"csrf": {bs.csrf}}), bk)
		if rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != "you write no project of ai-software-factory-code" {
			t.Errorf("%s as bob = %d %q", act, rec.Code, rec.Body)
		}
	}
	if len(r.routines.skips) != 0 || len(r.routines.ran) != 0 || r.events.count() != 0 {
		t.Fatalf("a refused act acted: skips %v, ran %v, %d events", r.routines.skips, r.routines.ran, r.events.count())
	}
	if rec := r.do(htmxPost("/routines/nowhere/weekly-bump/skip", url.Values{"csrf": {bs.csrf}}), bk); rec.Code != http.StatusNotFound {
		t.Fatalf("a missing project = %d", rec.Code)
	}
}
