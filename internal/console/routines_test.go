package console

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	// needsMe holds the side a live toggle set, "human by <login>" or
	// "agent by <login>", "" once undone
	needsMe map[string]string
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

func (f *fakeRoutines) NeedsMe(_ context.Context, project, routine string, needsMe *bool, by string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.known(project, routine); err != nil {
		return err
	}
	if f.needsMe == nil {
		f.needsMe = map[string]string{}
	}
	side := ""
	if needsMe != nil {
		side = map[bool]string{true: "human", false: "agent"}[*needsMe] + " by " + by
	}
	f.needsMe[project+"/"+routine] = side
	return nil
}

// weeklyBump is the code project's weekly bump as its board shows it
// with a Skip once pending: the next run after the skipped one.
func weeklyBump() factoryd.RoutineView {
	return factoryd.RoutineView{Project: "ai-software-factory-code",
		Routine: policy.Routine{Name: "weekly-bump", Title: "Weekly bump", Files: policy.FilesHuman, Schedule: []policy.Trigger{{Cron: "0 6 * * 1"}}},
		State:   factoryd.RoutineState{Project: "ai-software-factory-code", Routine: "weekly-bump"},
		Next:    time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC), NextWords: "Mon 20 Oct 06:00", Lane: "queued"}
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
	if rec = r.do(htmxPost("/routines/nowhere/weekly-bump/skip", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusNotFound {
		t.Fatalf("a missing project = %d %s", rec.Code, rec.Body)
	}
	if r.events.count() != before {
		t.Fatal("a missing project left an event")
	}
}

// The live toggle: a human or agent routine's ghost names the side in
// force and offers its flip, an override offers the undo, and a
// pull-request routine offers none. Each flip is one event with its via,
// the undo follows the config again, and a routine the project does not
// run is 404 without an event.
func TestRoutineToggle(t *testing.T) {
	r := newRig(t)
	nightly := factoryd.RoutineView{Project: "ai-software-factory-code",
		Routine: policy.Routine{Name: "nightly", Title: "Nightly", Files: policy.FilesHuman, Schedule: []policy.Trigger{{Cron: "0 3 * * *"}}},
		State:   factoryd.RoutineState{Project: "ai-software-factory-code", Routine: "nightly"}}
	roll := factoryd.RoutineView{Project: "ai-software-factory-code",
		Routine: policy.Routine{Name: "roll-prs", Title: "Roll PRs", Files: policy.FilesPullRequest, Schedule: []policy.Trigger{{On: "push-main"}}},
		State:   factoryd.RoutineState{Project: "ai-software-factory-code", Routine: "roll-prs"}}
	no := false
	overridden := nightly
	overridden.Routine.Name, overridden.Routine.Title = "nightly-hold", "Nightly hold"
	overridden.State.Routine = "nightly-hold"
	overridden.State.NeedsMe, overridden.State.NeedsMeBy = &no, "sebastian"
	r.routines.views = []factoryd.RoutineView{weeklyBump(), audit(), overridden, roll}
	ck, s := r.loggedIn(t)
	queued := col(t, r.do(httptest.NewRequest(http.MethodGet, codeBoard, nil), ck).Body.String(), "Queued")
	form := `<form method="post" action="/routines/ai-software-factory-code/%s/needs-me" hx-post="/routines/ai-software-factory-code/%s/needs-me" hx-target="#board" hx-swap="outerHTML">` +
		`<input type="hidden" name="csrf" value="` + s.csrf + `">%s<input type="hidden" name="project" value="ai-software-factory-code">` +
		`<button type="submit" class="xs" aria-label="%s">%s</button></form>`
	bumpG := ghostIn(t, queued, "weekly-bump")
	if want := fmt.Sprintf(form, "weekly-bump", "weekly-bump", `<input type="hidden" name="needs_me" value="0">`, "Weekly bump runs on its own", "Runs on its own"); !strings.Contains(bumpG, want) {
		t.Fatalf("the bump's ghost lacks its flip to the agent side:\n%s", bumpG)
	}
	if !strings.Contains(bumpG, "human card") {
		t.Fatalf("the bump's ghost names no side:\n%s", bumpG)
	}
	auditG := ghostIn(t, queued, "audit")
	if want := fmt.Sprintf(form, "audit", "audit", `<input type="hidden" name="needs_me" value="1">`, "Audit needs you", "Needs me"); !strings.Contains(auditG, want) {
		t.Fatalf("the audit's ghost lacks its flip to the human side:\n%s", auditG)
	}
	overG := ghostIn(t, queued, "nightly-hold")
	if want := fmt.Sprintf(form, "nightly-hold", "nightly-hold", `<input type="hidden" name="undo" value="1">`, "Nightly hold follows its config", "Follow the config"); !strings.Contains(overG, want) {
		t.Fatalf("an overridden ghost lacks the undo:\n%s", overG)
	}
	if !strings.Contains(overG, "agent card") {
		t.Fatalf("the override's ghost names no side:\n%s", overG)
	}
	if g := ghostIn(t, queued, "roll-prs"); strings.Contains(g, "needs-me") {
		t.Fatalf("a pull-request routine carries the toggle:\n%s", g)
	}

	for _, tt := range []struct {
		routine, field, note, side string
	}{
		{"weekly-bump", "needs_me=0", "Weekly bump runs on its own.", "agent by sebastian"},
		{"audit", "needs_me=1", "Audit needs you.", "human by sebastian"},
		{"nightly-hold", "undo=1", "Nightly hold follows its config.", ""},
	} {
		name, value, _ := strings.Cut(tt.field, "=")
		rec := r.do(htmxPost("/routines/ai-software-factory-code/"+tt.routine+"/needs-me", url.Values{"csrf": {s.csrf}, name: {value}}), ck)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<p class="note" role="status">`+tt.note+`</p>`) {
			t.Fatalf("%s = %d:\n%s", tt.field, rec.Code, rec.Body)
		}
		if got := r.routines.needsMe["ai-software-factory-code/"+tt.routine]; got != tt.side {
			t.Fatalf("%s reached the port as %q", tt.field, got)
		}
	}
	var got []string
	for _, e := range r.events.all() {
		got = append(got, fmt.Sprintf("%s %s %s %s %s %v", e.Kind, e.Repository, e.Actor, e.Via, e.TaskID, e.Detail))
	}
	equal(t, "the events", got, []string{
		"routine_needs_me sf/code sebastian button  map[needs_me:false project:ai-software-factory-code routine:weekly-bump undo:false]",
		"routine_needs_me sf/code sebastian button  map[needs_me:true project:ai-software-factory-code routine:audit undo:false]",
		"routine_needs_me sf/code sebastian button  map[needs_me:false project:ai-software-factory-code routine:nightly-hold undo:true]",
	})

	before := r.events.count()
	if rec := r.do(htmxPost("/routines/ai-software-factory-code/gone/needs-me", url.Values{"csrf": {s.csrf}, "needs_me": {"1"}}), ck); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown routine = %d %s", rec.Code, rec.Body)
	}
	if r.events.count() != before || len(r.routines.needsMe) != 3 {
		t.Fatal("an unknown routine toggled or left an event")
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
