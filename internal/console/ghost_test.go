package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const codeBoard = "/?project=ai-software-factory-code"

// ghostIn is the ghost article of routine name on the code project's board.
func ghostIn(t *testing.T, section, name string) string {
	t.Helper()
	i := strings.Index(section, `data-ghost="ai-software-factory-code/`+name+`"`)
	if i < 0 {
		t.Fatalf("ghost %s missing:\n%s", name, section)
	}
	start := strings.LastIndex(section[:i], "<article")
	return section[start : i+strings.Index(section[i:], "</article>")]
}

// soonBump is the weekly bump 20 minutes before its run, its last card
// #781 done two days after it was filed.
func soonBump() factoryd.RoutineView {
	v := weeklyBump()
	v.Next, v.Soon, v.NextWords = time.Now().Add(20*time.Minute+30*time.Second), true, "Mon 06:00"
	filed := time.Now().Add(-7 * 24 * time.Hour)
	v.Last = factoryd.RoutineRun{Project: v.Project, Routine: v.Routine.Name, Run: 14, Started: filed, Ended: filed.Add(50 * time.Hour),
		Trigger: "cron", TaskID: "t-781", Issue: 781, Outcome: "done"}
	return v
}

// audit is an agent routine every 20 merges at 14 of them.
func audit() factoryd.RoutineView {
	return factoryd.RoutineView{Project: "ai-software-factory-code",
		Routine: policy.Routine{Name: "audit", Title: "Audit", Files: policy.FilesAgent, Skill: "audit", Schedule: []policy.Trigger{{Merges: 20}}},
		State:   factoryd.RoutineState{Project: "ai-software-factory-code", Routine: "audit", Merges: 14}, MergesLeft: 6,
		NextWords: "after 6 more merges", Lane: "queued"}
}

// Every routine stands as a ghost at the foot of Queued, after the
// lane's real cards, which alone make the count; needs-you holds only
// the filed card of a run that waits on the human. The strip names the
// run under an hour away; a card a routine filed wears the chip.
func TestBoardPlacesGhostsAtTheFootOfTheirLane(t *testing.T) {
	r := newRig(t)
	// the bump's last run filed #781, which still waits on the human
	bump := soonBump()
	bump.Last.Outcome, bump.Last.Ended = "filed", time.Time{}
	r.routines.views = []factoryd.RoutineView{audit(), bump}
	r.tasks.set("which store?",
		factoryd.Task{ID: "t-603", Status: "waiting-human", Repository: "sf/code", IssueNumber: 603, Lane: policy.LaneReady},
		factoryd.Task{ID: "t-455", Status: "queued", Repository: "sf/code", IssueNumber: 455, Lane: policy.LaneReady},
		factoryd.Task{ID: "t-792", Status: "queued", Repository: "sf/code", IssueNumber: 792, Lane: policy.LaneReady, Source: factoryd.SourceRoutine},
		factoryd.Task{ID: "t-781", Status: "waiting-human", Repository: "sf/code", IssueNumber: 781, Source: factoryd.SourceRoutine})
	ck, s := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, codeBoard, nil), ck).Body.String()

	needs, queued := col(t, body, "Needs you"), col(t, body, "Queued")
	bumpG := ghostIn(t, queued, "weekly-bump")
	// needs-you holds the bump's filed, waiting card and no ghost; the
	// preview queues behind the lane's cards, the dated bump first
	if strings.Contains(needs, "data-ghost=") || !strings.Contains(needs, `data-card="t-781"`) {
		t.Fatalf("Needs you holds more or less than the bump's filed card:\n%s", needs)
	}
	if bumpAt, auditAt := strings.Index(queued, `data-ghost="ai-software-factory-code/weekly-bump"`), strings.Index(queued, `data-ghost="ai-software-factory-code/audit"`); bumpAt < 0 || auditAt < bumpAt || bumpAt < strings.LastIndex(queued, "data-card=") {
		t.Fatalf("Queued does not end in the bump's ghost, then the audit's:\n%s", queued)
	}
	for _, want := range []string{
		`<article class="card ghost"`,
		`<button type="button" class="open" data-routine="ai-software-factory-code/weekly-bump"><span class="ref">↻</span> <span class="ttl">Weekly bump</span></button>`,
		`<div class="line2 mono">every Monday 06:00</div>`,
		`<div class="line2 mono soon">Mon 06:00 · in 20 min · human card</div>`,
		`<div class="line2 mono">last #781 filed</div>`,
		`<form method="post" action="/routines/ai-software-factory-code/weekly-bump/skip" hx-post="/routines/ai-software-factory-code/weekly-bump/skip" hx-target="#board" hx-swap="outerHTML"><input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value="ai-software-factory-code"><button type="submit" class="xs" aria-label="Skip Weekly bump once">Skip once</button></form>`,
		`<form method="post" action="/routines/ai-software-factory-code/weekly-bump/run" hx-post="/routines/ai-software-factory-code/weekly-bump/run" hx-target="#board" hx-swap="outerHTML"><input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value="ai-software-factory-code"><button type="submit" class="xs" aria-label="Run Weekly bump now">Run now</button></form>`,
	} {
		if !strings.Contains(bumpG, want) {
			t.Fatalf("the bump's ghost lacks %s:\n%s", want, bumpG)
		}
	}
	if strings.Contains(bumpG, "data-card") || strings.Contains(bumpG, `class="count"`) {
		t.Fatalf("a ghost reads as a card:\n%s", bumpG)
	}

	a := ghostIn(t, queued, "audit")
	for _, want := range []string{`<div class="line2 mono">every 20 merges</div>`, `<div class="line2 mono">after 6 more merges</div>`} {
		if !strings.Contains(a, want) {
			t.Fatalf("the audit's ghost lacks %s:\n%s", want, a)
		}
	}
	if strings.Contains(a, "last ") {
		t.Fatalf("a routine that never ran names a last run:\n%s", a)
	}
	if !strings.Contains(queued, `Queued <span class="count">2</span>`) {
		t.Fatalf("Queued's count is not its two cards:\n%s", queued)
	}

	if !strings.Contains(body, `<span class="cell routine">↻ bump in 20 min</span>`) {
		t.Fatal("the strip names no run under an hour away")
	}
	if !strings.Contains(body, `<details class="col-menu routines-ctl"><summary aria-label="Routines on the board" title="Routines on the board">↻ routines</summary>`) {
		t.Fatal("the board's head has no routines control")
	}
	for _, id := range []string{"t-792", "t-781"} {
		if !strings.Contains(faceOf(t, body, id), `<span class="chip routine">↻ routine</span>`) {
			t.Fatalf("%s wears no routine chip:\n%s", id, faceOf(t, body, id))
		}
	}
	if strings.Contains(faceOf(t, body, "t-455"), "chip routine") {
		t.Fatal("a card no routine filed wears the chip")
	}
	// the find matches the chip's word
	found := col(t, r.do(httptest.NewRequest(http.MethodGet, codeBoard+"&q=routine", nil), ck).Body.String(), "Queued")
	if !strings.Contains(found, `data-card="t-792"`) || strings.Contains(found, `data-card="t-455"`) {
		t.Fatalf("the find of routine:\n%s", found)
	}
}

// The control shows every ghost, those whose next run is within its
// period, or none; a routine with no next time, counted in merges or run
// on an event, stays within any period. A lane with hidden ghosts says
// how many, linked to the board with all of them. The choice travels
// with the poll, the find and the acts' forms; the control's own form
// carries none of it.
func TestBoardRoutinesFilter(t *testing.T) {
	r := newRig(t)
	queuedAt := func(name string, next time.Duration) factoryd.RoutineView {
		return factoryd.RoutineView{Project: "ai-software-factory-code",
			Routine: policy.Routine{Name: name, Title: name, Files: policy.FilesAgent, Skill: "deps", Schedule: []policy.Trigger{{Cron: "0 6 * * 4"}}},
			Next:    time.Now().Add(next), NextWords: "Thu 06:00", Lane: "queued"}
	}
	deps, pass := queuedAt("dependency-update", 10*24*time.Hour), queuedAt("template-pass", 5*time.Hour)
	onConfig := factoryd.RoutineView{Project: "ai-software-factory-code",
		Routine: policy.Routine{Name: "on-config", Title: "On config", Files: policy.FilesAgent, Skill: "audit", Schedule: []policy.Trigger{{On: policy.OnConfigurationVersion}}},
		Lane:    "queued"}
	r.routines.views = []factoryd.RoutineView{deps, pass, onConfig, audit(), soonBump()}
	r.tasks.set("", factoryd.Task{ID: "t-455", Status: "queued", Repository: "sf/code", IssueNumber: 455, Lane: policy.LaneReady})
	ck, s := r.loggedIn(t)
	board := func(query string) (body, needs, queued string) {
		t.Helper()
		body = r.do(httptest.NewRequest(http.MethodGet, codeBoard+query, nil), ck).Body.String()
		return body, col(t, body, "Needs you"), col(t, body, "Queued")
	}
	// shown checks the queued ghosts the board shows, and that it hides the rest
	shown := func(query, queued string, want ...string) {
		t.Helper()
		for _, name := range []string{"dependency-update", "template-pass", "on-config", "audit"} {
			if has := strings.Contains(queued, `data-ghost="ai-software-factory-code/`+name+`"`); has != slices.Contains(want, name) {
				t.Errorf("%s: Queued shows %s: %v, want %v", query, name, has, !has)
			}
		}
	}

	body, needs, queued := board("")
	if deps := strings.Index(queued, `data-ghost="ai-software-factory-code/dependency-update"`); deps < 0 || strings.Index(queued, `data-ghost="ai-software-factory-code/audit"`) < deps {
		t.Fatalf("a merge-counted ghost sorts before a dated one:\n%s", queued)
	}
	shown("", queued, "dependency-update", "template-pass", "on-config", "audit")
	if strings.Contains(body, "ghosts-hidden") || !strings.Contains(body, `<input type="radio" name="routines" value="all" checked>`) {
		t.Fatal("all hides a ghost or does not show as chosen")
	}

	body, needs, queued = board("&routines=within&within=2&unit=hours")
	ghostIn(t, queued, "weekly-bump")
	shown("2 hours", queued, "on-config", "audit")
	if !strings.Contains(queued, `<p class="ghosts-hidden"><a href="/?project=ai-software-factory-code&amp;routines=all">2 routines hidden</a></p>`) {
		t.Fatalf("Queued does not say what it hides:\n%s", queued)
	}
	if strings.Contains(needs, "ghosts-hidden") || !strings.Contains(queued, `Queued <span class="count">1</span>`) {
		t.Fatalf("the hidden line or the count is wrong:\n%s\n%s", needs, queued)
	}
	for _, want := range []string{
		`data-query="?project=ai-software-factory-code&amp;routines=within&amp;unit=hours&amp;within=2"`,
		`<summary class="on" aria-label="Routines on the board: within 2 hours" title="Routines on the board">↻ routines</summary>`,
		`<form method="get" action="/" class="menu-pop auto-submit" hx-get="/" hx-select="#board" hx-target="#board" hx-swap="outerHTML"><input type="hidden" name="project" value="ai-software-factory-code"><fieldset><legend>Show</legend>`,
		`<label><input type="radio" name="routines" value="all"><span>all</span></label><div class="within"><label><input type="radio" name="routines" value="within" checked><span>within</span></label>` +
			`<input type="number" name="within" min="1" max="999" value="2" aria-label="The period's length"><select name="unit" aria-label="The period's unit"><option value="days">days</option><option value="hours" selected>hours</option></select></div>`,
		`<input type="hidden" name="project" value="ai-software-factory-code"><input type="hidden" name="routines" value="within"><input type="hidden" name="within" value="2"><input type="hidden" name="unit" value="hours"><label class="visually-hidden" for="q">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("within 2 hours: the board lacks %s", want)
		}
	}
	if !strings.Contains(ghostIn(t, queued, "audit"), `<input type="hidden" name="routines" value="within"><input type="hidden" name="within" value="2"><input type="hidden" name="unit" value="hours">`) {
		t.Fatal("a ghost's form drops the choice")
	}

	body, _, queued = board("&routines=within&within=3&unit=days")
	shown("3 days", queued, "template-pass", "on-config", "audit")
	if !strings.Contains(queued, `>1 routine hidden</a></p>`) || !strings.Contains(body, `aria-label="Routines on the board: within 3 days"`) ||
		!strings.Contains(body, `value="3" aria-label="The period's length"><select name="unit" aria-label="The period's unit"><option value="days">days</option><option value="hours">hours</option></select>`) {
		t.Fatalf("within 3 days: the lane or the control is wrong:\n%s", queued)
	}

	// a period the control does not take reads as 1 day
	body, _, queued = board("&routines=within&within=0&unit=weeks")
	shown("within=0&unit=weeks", queued, "template-pass", "on-config", "audit")
	if !strings.Contains(body, `data-query="?project=ai-software-factory-code&amp;routines=within&amp;unit=days&amp;within=1"`) ||
		!strings.Contains(body, `aria-label="Routines on the board: within 1 day"`) || !strings.Contains(body, `value="1" aria-label="The period's length">`) {
		t.Fatal("a refused period does not read as 1 day")
	}
	// the old week is no choice: it reads as all
	body, _, queued = board("&routines=week")
	shown("week", queued, "dependency-update", "template-pass", "on-config", "audit")
	if !strings.Contains(body, `<summary aria-label="Routines on the board" title="Routines on the board">`) || strings.Contains(body, "routines=week") {
		t.Fatal("routines=week does not read as all")
	}

	body, needs, queued = board("&routines=none")
	if strings.Contains(body, "data-ghost=") {
		t.Fatal("none shows a ghost")
	}
	if strings.Contains(needs, "ghosts-hidden") || !strings.Contains(queued, `>5 routines hidden</a></p>`) ||
		!strings.Contains(queued, `Queued <span class="count">1</span>`) {
		t.Fatalf("none: the lanes say wrong:\n%s\n%s", needs, queued)
	}
	if !strings.Contains(body, `<span class="cell routine">↻ bump in 20 min</span>`) {
		t.Fatal("hiding the ghosts hides the strip's cell")
	}

	// the hidden line's link keeps the find and the column's order, and
	// the find leaves the ghosts standing
	_, needs, queued = board("&q=455&queued=oldest&routines=within&within=2&unit=hours")
	ghostIn(t, queued, "weekly-bump")
	ghostIn(t, queued, "audit")
	if !strings.Contains(queued, `<p class="ghosts-hidden"><a href="/?project=ai-software-factory-code&amp;q=455&amp;queued=oldest&amp;routines=all">2 routines hidden</a></p>`) {
		t.Fatalf("the hidden line drops the board's state:\n%s", queued)
	}

	// an act's answer keeps the choice it carried
	rec := r.do(htmxPost("/routines/ai-software-factory-code/audit/skip", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"routines": {"within"}, "within": {"2"}, "unit": {"hours"}}), ck)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `data-query="?project=ai-software-factory-code&amp;routines=within&amp;unit=hours&amp;within=2"`) {
		t.Fatalf("skip = %d:\n%s", rec.Code, rec.Body)
	}
}

// A routine the tree no longer has leaves the board: no ghost, no cell,
// no control.
func TestBoardShowsNoGhostForARemovedRoutine(t *testing.T) {
	r := newRig(t)
	r.routines.views = []factoryd.RoutineView{soonBump()}
	ck, _ := r.loggedIn(t)
	if body := r.do(httptest.NewRequest(http.MethodGet, codeBoard, nil), ck).Body.String(); !strings.Contains(body, "data-ghost=") {
		t.Fatal("no ghost before the removal")
	}
	r.routines.views = nil
	body := r.do(httptest.NewRequest(http.MethodGet, codeBoard, nil), ck).Body.String()
	for _, gone := range []string{"data-ghost=", "cell routine", "routines-ctl", "ghosts-hidden"} {
		if strings.Contains(body, gone) {
			t.Fatalf("the removed routine leaves %s", gone)
		}
	}
}

// The wide view has no four columns to stand the ghosts in: the strip
// keeps its cell, the board has no ghost and no control. A member who
// reads the project but does not write it sees the ghosts without acts.
func TestGhostsInTheWideViewAndForAReader(t *testing.T) {
	r := newRig(t)
	r.routines.views = []factoryd.RoutineView{audit(), soonBump()}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, codeBoard+"&view=wide", nil), ck).Body.String()
	if !strings.Contains(body, `<span class="cell routine">↻ bump in 20 min</span>`) || strings.Contains(body, "data-ghost=") ||
		strings.Contains(body, "routines-ctl") || strings.Contains(body, "ghosts-hidden") {
		t.Fatal("the wide view: want the cell, no ghost, no control")
	}

	ak, _ := r.as(t, "anna")
	rec := r.do(httptest.NewRequest(http.MethodGet, codeBoard, nil), ak)
	if rec.Code != http.StatusOK {
		t.Fatalf("anna's board = %d", rec.Code)
	}
	g := ghostIn(t, col(t, rec.Body.String(), "Queued"), "weekly-bump")
	if strings.Contains(g, `class="acts"`) || strings.Contains(g, "/skip") || strings.Contains(g, "/run") {
		t.Fatalf("a reader is offered the acts:\n%s", g)
	}
}

// A held run says so on its ghost and in the strip, in the amber text
// colour; a card a run filed that is still open is no held run, and one
// that runs breathes on its ghost.
func TestHeldGhostSpeaks(t *testing.T) {
	r := newRig(t)
	bump := soonBump()
	bump.Open, bump.Held = &factoryd.Task{ID: "t-781", Status: "waiting-human", IssueNumber: 781}, true
	bump.Last = factoryd.RoutineRun{Project: bump.Project, Routine: "weekly-bump", Run: 15, Started: time.Now(), Ended: time.Now(),
		Trigger: "cron", Outcome: "held", Note: "held: #781 is still open"}
	run := audit()
	run.Open = &factoryd.Task{ID: "t-792", Status: "running", IssueNumber: 792}
	run.Last = factoryd.RoutineRun{Project: run.Project, Routine: "audit", Run: 4, Started: time.Now(), Trigger: "merges", TaskID: "t-792", Issue: 792, Outcome: "filed"}
	r.routines.views = []factoryd.RoutineView{bump, run}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, codeBoard, nil), ck).Body.String()

	g := ghostIn(t, col(t, body, "Queued"), "weekly-bump")
	if !strings.Contains(g, `<article class="card ghost held"`) || !strings.Contains(g, `<span class="held">held: #781 is still open</span>`) {
		t.Fatalf("the held ghost is quiet:\n%s", g)
	}
	if !strings.Contains(body, `<span class="cell routine warn">↻ bump held</span>`) {
		t.Fatal("the strip does not say held")
	}
	// the held run's words stand in for its last line, under the next run
	if !strings.Contains(g, `<div class="line2 mono soon">Mon 06:00 · in 20 min · human card</div>`+"\n"+
		`<div class="line2 mono"><span class="held">held: #781 is still open</span></div>`) || strings.Contains(g, "last held") {
		t.Fatalf("the held ghost does not name its next run, or repeats the held run as its last:\n%s", g)
	}
	a := ghostIn(t, col(t, body, "Queued"), "audit")
	if !strings.Contains(a, `<span class="run"><i class="dot" aria-hidden="true"></i>#792 running</span>`) || strings.Contains(a, "held") {
		t.Fatalf("the running ghost does not breathe, or says held:\n%s", a)
	}

	// the bump's card filed and open, no run held: the ghost and the strip whisper
	bump.Held, bump.Last = false, factoryd.RoutineRun{Project: bump.Project, Routine: "weekly-bump", Run: 15, Started: time.Now(), Trigger: "cron", TaskID: "t-781", Issue: 781, Outcome: "filed"}
	r.routines.views = []factoryd.RoutineView{bump, run}
	body = r.do(httptest.NewRequest(http.MethodGet, codeBoard, nil), ck).Body.String()
	if g := ghostIn(t, col(t, body, "Queued"), "weekly-bump"); strings.Contains(g, "held") || !strings.Contains(g, "last #781 filed") {
		t.Fatalf("an open card reads as a held run:\n%s", g)
	}
	if !strings.Contains(body, `<span class="cell routine">↻ bump in 20 min</span>`) {
		t.Fatal("the strip does not name the run under an hour away")
	}
}

// The strip's cell names a routine by the last segment of its name,
// whatever its title: the weekly bump's real title ends in "inputs", the
// audit's in "review".
func TestStripCellWordIsTheNamesLastSegment(t *testing.T) {
	r := newRig(t)
	bump := soonBump()
	bump.Routine.Title = "Weekly bump of the pinned outside inputs"
	r.routines.views = []factoryd.RoutineView{bump}
	ck, _ := r.loggedIn(t)
	if body := r.do(httptest.NewRequest(http.MethodGet, codeBoard, nil), ck).Body.String(); !strings.Contains(body, `<span class="cell routine">↻ bump in 20 min</span>`) {
		t.Fatalf("the cell does not name the bump by its name:\n%s", body)
	}
	a := audit()
	a.Routine.Title = "Audit: bug hunt and architecture review"
	a.Open, a.Held = &factoryd.Task{ID: "t-792", Status: "queued", IssueNumber: 792}, true
	a.Last = factoryd.RoutineRun{Project: a.Project, Routine: "audit", Run: 5, Started: time.Now(), Ended: time.Now(), Trigger: "merges",
		Outcome: "held", Note: "held: #792 is still open"}
	r.routines.views = []factoryd.RoutineView{a}
	if body := r.do(httptest.NewRequest(http.MethodGet, codeBoard, nil), ck).Body.String(); !strings.Contains(body, `<span class="cell routine warn">↻ audit held</span>`) {
		t.Fatalf("the cell does not name the held audit by its name:\n%s", body)
	}
}

// The time-driven faces carry each ghost's next words, so the stream
// re-renders "in 20 min" as it counts down.
func TestFacesCarryTheGhostsNextWords(t *testing.T) {
	d := boardData{Columns: []column{{Ghosts: []ghost{{Name: "weekly-bump", NextWords: "Mon 06:00 · in 20 min · human card"}}}}}
	if !strings.Contains(d.faces(), "Mon 06:00 · in 20 min · human card") {
		t.Fatalf("faces = %q", d.faces())
	}
}

// A pending Skip once leads the ghost's next line, which names the run
// after the skipped one, and its act becomes the undo, posting undo=1;
// the undo clears the skip and the ghost reads as before.
func TestGhostShowsAPendingSkip(t *testing.T) {
	r := newRig(t)
	bump := weeklyBump()
	bump.State.SkipNext, bump.State.SkipBy = true, "sebastian"
	r.routines.views = []factoryd.RoutineView{bump}
	r.routines.onSkip = func(_, _ string, skip bool) { r.routines.views[0].State.SkipNext = skip }
	ck, s := r.loggedIn(t)
	g := ghostIn(t, col(t, r.do(httptest.NewRequest(http.MethodGet, codeBoard, nil), ck).Body.String(), "Queued"), "weekly-bump")
	for _, want := range []string{
		`<div class="line2 mono">skipped once · next Mon 20 Oct 06:00 · in `,
		`<form method="post" action="/routines/ai-software-factory-code/weekly-bump/skip" hx-post="/routines/ai-software-factory-code/weekly-bump/skip" hx-target="#board" hx-swap="outerHTML">` +
			`<input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="undo" value="1"><input type="hidden" name="project" value="ai-software-factory-code">` +
			`<button type="submit" class="xs" aria-label="Undo the skip of Weekly bump">Undo skip</button></form>`,
	} {
		if !strings.Contains(g, want) {
			t.Fatalf("the skipped ghost lacks %s:\n%s", want, g)
		}
	}
	if strings.Contains(g, "Skip once") {
		t.Fatalf("a skipped ghost offers Skip once:\n%s", g)
	}
	rec := r.do(htmxPost("/routines/ai-software-factory-code/weekly-bump/skip", url.Values{"csrf": {s.csrf}, "undo": {"1"}, "project": {"ai-software-factory-code"}}), ck)
	g = ghostIn(t, col(t, rec.Body.String(), "Queued"), "weekly-bump")
	if rec.Code != http.StatusOK || strings.Contains(g, "skipped once") || !strings.Contains(g, ">Skip once</button>") || r.routines.skips[bumpKey] != "" {
		t.Fatalf("the undo = %d, skips %v:\n%s", rec.Code, r.routines.skips, g)
	}
}

// An event's routine has no next time, so its pending skip names only
// what the next run files.
func TestGhostShowsAnEventRoutinesPendingSkip(t *testing.T) {
	rv := factoryd.RoutineView{Project: "ai-software-factory-code",
		Routine: policy.Routine{Name: "on-push", Title: "On push", Files: policy.FilesHuman, Schedule: []policy.Trigger{{On: "push-main"}}},
		State:   factoryd.RoutineState{Project: "ai-software-factory-code", Routine: "on-push", SkipNext: true}}
	if g := ghostOf(rv, time.Now(), boardFilter{}, ""); g.NextWords != "skipped once · human card" {
		t.Fatalf("next words = %q", g.NextWords)
	}
}
