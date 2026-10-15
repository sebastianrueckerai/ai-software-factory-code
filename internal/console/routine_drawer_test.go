package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const bumpKey = "ai-software-factory-code/weekly-bump"

// bumpRuns are the weekly bump's newest six runs, newest first: run 15
// filed #790, still open; #781 merged 2 days and 2 hours after run 14; a
// Skip once; a run held while #770 was open; #770 failed after 8 days
// and 4 hours; #762 done in 3 hours.
func bumpRuns() []factoryd.RoutineRun {
	const p, name = "ai-software-factory-code", "weekly-bump"
	mon := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 6, 0, 0, 0, time.UTC) }
	return []factoryd.RoutineRun{
		{Project: p, Routine: name, Run: 15, Due: mon(10, 12), Started: mon(10, 12), Trigger: "cron", TaskID: "t-790", Issue: 790, Outcome: "filed"},
		{Project: p, Routine: name, Run: 14, Due: mon(10, 5), Started: mon(10, 5), Ended: mon(10, 5).Add(50 * time.Hour), Trigger: "cron",
			TaskID: "t-781", Issue: 781, Outcome: "merged", CostEUR: 0.42},
		{Project: p, Routine: name, Run: 13, Due: mon(9, 28), Started: mon(9, 28), Ended: mon(9, 28), Trigger: "cron", Outcome: "skipped", Note: "skipped once by sebastian"},
		{Project: p, Routine: name, Run: 12, Due: mon(9, 21), Started: mon(9, 21), Ended: mon(9, 21), Trigger: "cron", Outcome: "held", Note: "held: #770 is still open"},
		{Project: p, Routine: name, Run: 11, Due: mon(9, 14), Started: mon(9, 14), Ended: mon(9, 14).Add(196 * time.Hour), Trigger: "cron",
			TaskID: "t-770", Issue: 770, Outcome: "failed", CostEUR: 1.1},
		{Project: p, Routine: name, Run: 10, Due: mon(9, 7), Started: mon(9, 7), Ended: mon(9, 7).Add(3 * time.Hour), Trigger: "cron",
			TaskID: "t-762", Issue: 762, Outcome: "done", CostEUR: 0.3},
	}
}

// bumpRows are bumpRuns as the drawer's run rows, in their order.
var bumpRows = []string{
	`<span class="mono">Mon 12 Oct 06:00</span><span class="mono"><a href="#card=t-790">#790</a></span><span class="k st-running">filed</span><span class="mono num">open</span><span class="mono num"></span><span class="tnote"></span>`,
	`<span class="mono">Mon 5 Oct 06:00</span><span class="mono"><a href="#card=t-781">#781</a></span><span class="k st-done">merged</span><span class="mono num">2 d 2 h</span><span class="mono num">€0.42</span><span class="tnote"></span>`,
	`<span class="mono">Mon 28 Sep 06:00</span><span class="mono"></span><span class="k st-closed">skipped</span><span class="mono num">under a minute</span><span class="mono num"></span><span class="tnote">skipped once by sebastian</span>`,
	`<span class="mono">Mon 21 Sep 06:00</span><span class="mono"></span><span class="k st-waiting">held</span><span class="mono num">under a minute</span><span class="mono num"></span><span class="tnote">held: #770 is still open</span>`,
	`<span class="mono">Mon 14 Sep 06:00</span><span class="mono"><a href="#card=t-770">#770</a></span><span class="k st-failed">failed</span><span class="mono num">8 d 4 h</span><span class="mono num">€1.10</span><span class="tnote"></span>`,
	`<span class="mono">Mon 7 Sep 06:00</span><span class="mono"><a href="#card=t-762">#762</a></span><span class="k st-done">done</span><span class="mono num">3 h 0 min</span><span class="mono num">€0.30</span><span class="tnote"></span>`,
}

func getPage(r *rig, ck *http.Cookie, path string) *httptest.ResponseRecorder {
	return r.do(httptest.NewRequest(http.MethodGet, path, nil), ck)
}

// A card a routine filed names the routine in its drawer, under the top
// block: its title opens the routine's drawer, the run that filed the
// card, and the routine's newest other run. A routine since removed
// keeps its name without the door; another card says nothing.
func TestCardDrawerCarriesTheProvenanceLine(t *testing.T) {
	r := newRig(t)
	r.routines.views = []factoryd.RoutineView{weeklyBump()}
	r.routines.runs = map[string][]factoryd.RoutineRun{bumpKey: bumpRuns()}
	r.tasks.set("which pins?",
		factoryd.Task{ID: "t-790", Kind: "human", Status: "waiting-human", Repository: "sf/code", IssueNumber: 790, Source: factoryd.SourceRoutine,
			Objective: "Weekly bump\n\nBump the pins.\n\nfiled by the factory · routine weekly-bump · run 15"},
		factoryd.Task{ID: "t-455", Status: "queued", Repository: "sf/code", IssueNumber: 455, Lane: policy.LaneReady})
	ck, _ := r.loggedIn(t)
	line := func(want string) {
		t.Helper()
		d := getPage(r, ck, "/cards/t-790/drawer?tab=overview").Body.String()
		at := strings.Index(d, `<p class="provenance">filed by the factory`)
		if !strings.Contains(d, want) || at < strings.Index(d, "</dl>") || at > strings.Index(d, `data-sec="time"`) {
			t.Fatalf("the drawer lacks, under its top block,\n%s\n%s", want, d)
		}
	}
	const head = `<p class="provenance">filed by the factory · routine <button type="button" class="linkbtn" data-open-routine="` + bumpKey + `">Weekly bump</button> · run 15`
	line(head + ` · last run #781 merged in 2 d</p>`)
	if d := getPage(r, ck, "/cards/t-790/drawer?tab=overview").Body.String(); !strings.Contains(d, `<p class="objective-body">Bump the pins.</p>`) {
		t.Fatalf("the objective repeats the routine's line:\n%s", d)
	}

	// a run since: the routine's newest run other than the card's
	r.routines.runs[bumpKey] = append([]factoryd.RoutineRun{{Project: "ai-software-factory-code", Routine: "weekly-bump", Run: 16, Outcome: "held",
		Note: "held: #790 is still open"}}, bumpRuns()...)
	line(head + ` · last run held</p>`)

	// the routine's first run has none before it
	r.routines.runs[bumpKey] = bumpRuns()[:1]
	line(head + `</p>`)

	r.routines.views = nil
	line(`<p class="provenance">filed by the factory · routine weekly-bump · run 15</p>`)

	if d := getPage(r, ck, "/cards/t-455/drawer?tab=overview").Body.String(); strings.Contains(d, "filed by the factory") {
		t.Fatalf("a card no routine filed names one:\n%s", d)
	}
}

// The routine's drawer: Overview with its head, its top block, its acts,
// its brief, its newest three runs and its configuration; Runs with every
// run in the Time section's shape; Config with the routine as the project
// file holds it.
func TestRoutineDrawerTabs(t *testing.T) {
	r := newRig(t)
	bump := soonBump()
	bump.Routine.Brief, bump.State.Runs = "routines/weekly-bump.md", 15
	bump.Last, bump.Open = bumpRuns()[0], &factoryd.Task{ID: "t-790", Status: "waiting-human", IssueNumber: 790}
	r.routines.views = []factoryd.RoutineView{bump, audit()}
	r.routines.runs = map[string][]factoryd.RoutineRun{bumpKey: bumpRuns()}
	r.ledger.briefs = map[string]string{"routines/weekly-bump.md": "Bump the pinned inputs to the newest release of each line.\n"}
	ck, s := r.loggedIn(t)
	const path = "/routines/" + bumpKey + "/drawer"

	rec := getPage(r, ck, path)
	d := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("the overview = %d:\n%s", rec.Code, d)
	}
	runs := `<details class="sec" data-sec="runs"><summary>Runs (15)</summary><div class="sec-body"><div class="life runs">` + strings.Join(bumpRows[:3], "") + `</div></div></details>`
	config := "<pre>- name: weekly-bump\n  title: Weekly bump\n  schedule:\n    - cron: 0 6 * * 1\n  files: human\n  brief: routines/weekly-bump.md</pre>" +
		`<p class="quiet">A change of schedule is a configuration change. Ask the project agent to change this.</p>`
	for _, want := range []string{
		`<div class="drawer-content" data-routine-drawer="` + bumpKey + `:overview">`,
		`<h2 tabindex="-1"><span class="glyph" aria-hidden="true">↻</span> Weekly bump</h2>`,
		`<p class="drawer-facts">routine · every Monday 06:00 · human card · next Mon 06:00, in 20 min</p>`,
		`<button type="button" role="tab" aria-selected="true" aria-controls="drawer-panel" hx-get="/routines/` + bumpKey + `/drawer?tab=overview" hx-target="#drawer" hx-swap="innerHTML">Overview</button>`,
		`<button type="button" role="tab" aria-selected="false" aria-controls="drawer-panel" hx-get="/routines/` + bumpKey + `/drawer?tab=runs" hx-target="#drawer" hx-swap="innerHTML">Runs</button>`,
		`<button type="button" role="tab" aria-selected="false" aria-controls="drawer-panel" hx-get="/routines/` + bumpKey + `/drawer?tab=config" hx-target="#drawer" hx-swap="innerHTML">Config</button>`,
		"<dl class=\"top-block\">\n" +
			"<dt>Schedule</dt><dd>every Monday 06:00 <span class=\"why\">· UTC</span></dd>\n" +
			"<dt>Next</dt><dd>Mon 06:00 · in 20 min</dd>\n" +
			"<dt>Last</dt><dd>#790 filed <span class=\"why\">· Mon 12 Oct 06:00</span></dd>\n" +
			"<dt>While open</dt><dd>hold <span class=\"why\">· a run skips while the last card is open, and says so</span></dd>\n" +
			"<dt>Tells</dt><dd>inbox <span class=\"why\">· a filing pokes the inbox</span></dd>\n</dl>",
		`<form method="post" action="/routines/` + bumpKey + `/skip" hx-post="/routines/` + bumpKey + `/skip" hx-target="#board" hx-swap="outerHTML"><input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value="ai-software-factory-code"><button type="submit" class="xs" aria-label="Skip Weekly bump once">Skip once</button></form>`,
		`<form method="post" action="/routines/` + bumpKey + `/run" hx-post="/routines/` + bumpKey + `/run" hx-target="#board" hx-swap="outerHTML"><input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value="ai-software-factory-code"><button type="submit" class="xs" aria-label="Run Weekly bump now">Run now</button></form>`,
		`<details class="sec" data-sec="brief"><summary>The brief<span class="hint">routines/weekly-bump.md</span></summary><div class="sec-body"><p class="objective-body">Bump the pinned inputs to the newest release of each line.</p></div></details>`,
		runs,
		`<details class="sec" data-sec="config"><summary>Config</summary><div class="sec-body">` + config + `</div></details>`,
	} {
		if !strings.Contains(d, want) {
			t.Fatalf("the overview lacks\n%s\n%s", want, d)
		}
	}
	if strings.Contains(d, "Missed") || strings.Contains(d, `class="warn"`) {
		t.Fatalf("the overview names a missed rule the file leaves out, or speaks amber:\n%s", d)
	}

	d = getPage(r, ck, path+"?tab=runs").Body.String()
	if !strings.Contains(d, `data-routine-drawer="`+bumpKey+`:runs">`) || !strings.Contains(d, `<div class="life runs">`+strings.Join(bumpRows, "")+`</div>`) {
		t.Fatalf("the runs tab lacks the six runs newest first:\n%s", d)
	}
	d = getPage(r, ck, path+"?tab=config").Body.String()
	if !strings.Contains(d, `<div id="drawer-panel" class="drawer-body" role="tabpanel">`+"\n"+config) || strings.Contains(d, "top-block") {
		t.Fatalf("the config tab:\n%s", d)
	}
	if rec := getPage(r, ck, path+"?tab=chat"); rec.Code != http.StatusBadRequest {
		t.Fatalf("a tab the drawer lacks = %d", rec.Code)
	}

	// a held run speaks in the Last row alone; the missed rule shows once set
	bump.Held, bump.Routine.Missed = true, policy.MissedSkip
	bump.Last = factoryd.RoutineRun{Project: bump.Project, Routine: "weekly-bump", Run: 16, Due: time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC), Outcome: "held",
		Note: "held: #790 is still open"}
	r.routines.views = []factoryd.RoutineView{bump, audit()}
	d = getPage(r, ck, path).Body.String()
	for _, want := range []string{
		"<dt>Last</dt><dd class=\"warn\">held: #790 is still open <span class=\"why\">· Mon 19 Oct 06:00</span></dd>\n",
		"<dt>Missed</dt><dd>skip <span class=\"why\">· a missed run is skipped</span></dd>\n",
	} {
		if !strings.Contains(d, want) {
			t.Fatalf("the held overview lacks\n%s\n%s", want, d)
		}
	}

	// 131 runs: the list holds the newest 100 and counts the rest
	bump.State.Runs = 131
	r.routines.views, r.routines.runs[bumpKey] = []factoryd.RoutineView{bump, audit()}, nil
	for n := 131; n > 0; n-- {
		r.routines.runs[bumpKey] = append(r.routines.runs[bumpKey], factoryd.RoutineRun{Project: bump.Project, Routine: "weekly-bump", Run: n, Outcome: "no change"})
	}
	d = getPage(r, ck, path+"?tab=runs").Body.String()
	if strings.Count(d, `<span class="k st-closed">no change</span>`) != 100 || !strings.Contains(d, "</div>\n<p class=\"quiet\">and 31 older runs</p>") {
		t.Fatalf("the runs past the list:\n%s", d)
	}

	// a merge-counted routine that never ran, without a brief
	d = getPage(r, ck, "/routines/ai-software-factory-code/audit/drawer").Body.String()
	for _, want := range []string{
		`<p class="drawer-facts">routine · every 20 merges · agent card · next after 6 more merges</p>`,
		"<dt>Schedule</dt><dd>every 20 merges</dd>\n<dt>Next</dt><dd>after 6 more merges</dd>\n<dt>Last</dt><dd class=\"muted\">no run yet</dd>\n",
		`<div class="sec empty"><p class="summary" aria-disabled="true">The brief<span class="hint">no brief</span></p></div>`,
		`<div class="sec empty"><p class="summary" aria-disabled="true">Runs<span class="hint">no run yet</span></p></div>`,
	} {
		if !strings.Contains(d, want) {
			t.Fatalf("the audit's overview lacks\n%s\n%s", want, d)
		}
	}
}

// The routine's drawer is its project's writers', as a card's drawer
// is; a reader's ghost has no door to it. A routine or a project there
// is none of is 404.
func TestRoutineDrawerNeedsAMember(t *testing.T) {
	r := newRig(t)
	r.routines.views = []factoryd.RoutineView{weeklyBump()}
	const path = "/routines/" + bumpKey + "/drawer"

	ak, _ := r.as(t, "anna")
	if rec := getPage(r, ak, path); rec.Code != http.StatusForbidden {
		t.Fatalf("anna, who does not write the project, = %d:\n%s", rec.Code, rec.Body)
	}
	if g := ghostIn(t, col(t, getPage(r, ak, codeBoard).Body.String(), "Needs you"), "weekly-bump"); strings.Contains(g, "data-routine") || strings.Contains(g, "<button") {
		t.Fatalf("a reader's ghost opens a drawer:\n%s", g)
	}

	ck, _ := r.as(t, "sebastian")
	if rec := getPage(r, ck, path); rec.Code != http.StatusOK {
		t.Fatalf("a writer = %d:\n%s", rec.Code, rec.Body)
	}
	for _, p := range []string{"/routines/ai-software-factory-code/nightly/drawer", "/routines/nowhere/weekly-bump/drawer"} {
		if rec := getPage(r, ck, p); rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d", p, rec.Code)
		}
	}
}
