package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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

// Every routine stands as a ghost at the foot of the lane its card lands
// in, after the lane's real cards, which alone make the count; the strip
// names the run under an hour away; a card a routine filed wears the chip.
func TestBoardPlacesGhostsAtTheFootOfTheirLane(t *testing.T) {
	r := newRig(t)
	r.routines.views = []factoryd.RoutineView{audit(), soonBump()}
	done := time.Now().Add(-time.Hour)
	r.tasks.set("which store?",
		factoryd.Task{ID: "t-603", Status: "waiting-human", Repository: "sf/code", IssueNumber: 603, Lane: policy.LaneReady},
		factoryd.Task{ID: "t-455", Status: "queued", Repository: "sf/code", IssueNumber: 455, Lane: policy.LaneReady},
		factoryd.Task{ID: "t-792", Status: "queued", Repository: "sf/code", IssueNumber: 792, Lane: policy.LaneReady, Source: factoryd.SourceRoutine},
		factoryd.Task{ID: "t-781", Status: "done", Repository: "sf/code", IssueNumber: 781, Source: factoryd.SourceRoutine, FinishedAt: &done})
	ck, s := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, codeBoard, nil), ck).Body.String()

	needs, queued := col(t, body, "Needs you"), col(t, body, "Queued")
	bump := ghostIn(t, needs, "weekly-bump")
	if strings.Index(needs, "data-ghost=") < strings.LastIndex(needs, "data-card=") || strings.Contains(needs, "audit") {
		t.Fatalf("Needs you does not end in the bump's ghost alone:\n%s", needs)
	}
	for _, want := range []string{
		`<article class="card ghost"`,
		`<button type="button" class="open" data-routine="ai-software-factory-code/weekly-bump"><span class="ref">↻</span> <span class="ttl">Weekly bump</span></button>`,
		`<div class="line2 mono">every Monday 06:00</div>`,
		`<div class="line2 mono soon">Mon 06:00 · in 20 min · human card</div>`,
		`<div class="line2 mono">last #781 done in 2 d</div>`,
		`<form method="post" action="/routines/ai-software-factory-code/weekly-bump/skip" hx-post="/routines/ai-software-factory-code/weekly-bump/skip" hx-target="#board" hx-swap="outerHTML"><input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value="ai-software-factory-code"><button type="submit" class="xs" aria-label="Skip Weekly bump once">Skip once</button></form>`,
		`<form method="post" action="/routines/ai-software-factory-code/weekly-bump/run" hx-post="/routines/ai-software-factory-code/weekly-bump/run" hx-target="#board" hx-swap="outerHTML"><input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value="ai-software-factory-code"><button type="submit" class="xs" aria-label="Run Weekly bump now">Run now</button></form>`,
	} {
		if !strings.Contains(bump, want) {
			t.Fatalf("the bump's ghost lacks %s:\n%s", want, bump)
		}
	}
	if strings.Contains(bump, "data-card") || strings.Contains(bump, `class="count"`) {
		t.Fatalf("a ghost reads as a card:\n%s", bump)
	}

	a := ghostIn(t, queued, "audit")
	if strings.Index(queued, "data-ghost=") < strings.LastIndex(queued, "data-card=") || strings.Contains(queued, "weekly-bump") {
		t.Fatalf("Queued does not end in the audit's ghost alone:\n%s", queued)
	}
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

	if !strings.Contains(body, `<span class="cell routine">↻ weekly in 20 min</span>`) {
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

// The control shows every ghost, those within 7 days, or none; a lane
// with hidden ghosts says how many, linked to the board with all of them.
// The choice travels with the poll and the acts' forms.
func TestBoardRoutinesFilter(t *testing.T) {
	r := newRig(t)
	deps := factoryd.RoutineView{Project: "ai-software-factory-code",
		Routine: policy.Routine{Name: "dependency-update", Title: "Dependency update", Files: policy.FilesAgent, Skill: "deps", Schedule: []policy.Trigger{{Cron: "0 6 * * 4"}}},
		Next:    time.Now().Add(10 * 24 * time.Hour), NextWords: "Sun 25 Oct 06:00", Lane: "queued"}
	r.routines.views = []factoryd.RoutineView{deps, audit(), soonBump()}
	r.tasks.set("", factoryd.Task{ID: "t-455", Status: "queued", Repository: "sf/code", IssueNumber: 455, Lane: policy.LaneReady})
	ck, s := r.loggedIn(t)

	body := r.do(httptest.NewRequest(http.MethodGet, codeBoard, nil), ck).Body.String()
	queued := col(t, body, "Queued")
	if deps := strings.Index(queued, `data-ghost="ai-software-factory-code/dependency-update"`); deps < 0 || strings.Index(queued, `data-ghost="ai-software-factory-code/audit"`) < deps {
		t.Fatalf("a merge-counted ghost sorts before a dated one:\n%s", queued)
	}

	body = r.do(httptest.NewRequest(http.MethodGet, codeBoard+"&routines=week", nil), ck).Body.String()
	needs, queued := col(t, body, "Needs you"), col(t, body, "Queued")
	ghostIn(t, needs, "weekly-bump")
	ghostIn(t, queued, "audit")
	if strings.Contains(queued, "dependency-update") {
		t.Fatalf("within 7 days shows a run 10 days out:\n%s", queued)
	}
	if !strings.Contains(queued, `<p class="ghosts-hidden"><a href="/?project=ai-software-factory-code&amp;routines=all">1 routine hidden</a></p>`) {
		t.Fatalf("Queued does not say what it hides:\n%s", queued)
	}
	if strings.Contains(needs, "ghosts-hidden") || !strings.Contains(queued, `Queued <span class="count">1</span>`) {
		t.Fatalf("the hidden line or the count is wrong:\n%s\n%s", needs, queued)
	}
	if !strings.Contains(body, `data-query="?project=ai-software-factory-code&amp;routines=week"`) ||
		!strings.Contains(body, `<summary class="on" aria-label="Routines on the board: within 7 days"`) ||
		!strings.Contains(body, `<input type="radio" name="routines" value="week" checked>`) {
		t.Fatal("the board does not keep within 7 days")
	}
	if !strings.Contains(ghostIn(t, queued, "audit"), `<input type="hidden" name="routines" value="week">`) {
		t.Fatal("a ghost's form drops the choice")
	}

	body = r.do(httptest.NewRequest(http.MethodGet, codeBoard+"&routines=none", nil), ck).Body.String()
	needs, queued = col(t, body, "Needs you"), col(t, body, "Queued")
	if strings.Contains(body, "data-ghost=") {
		t.Fatal("none shows a ghost")
	}
	if !strings.Contains(needs, `>1 routine hidden</a></p>`) || !strings.Contains(queued, `>2 routines hidden</a></p>`) ||
		!strings.Contains(queued, `Queued <span class="count">1</span>`) {
		t.Fatalf("none: the lanes say wrong:\n%s\n%s", needs, queued)
	}
	if !strings.Contains(body, `<span class="cell routine">↻ weekly in 20 min</span>`) {
		t.Fatal("hiding the ghosts hides the strip's cell")
	}

	// an act's answer keeps the choice it carried
	rec := r.do(htmxPost("/routines/ai-software-factory-code/audit/skip", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "routines": {"week"}}), ck)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `data-query="?project=ai-software-factory-code&amp;routines=week"`) {
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
	run.Open, run.Held = &factoryd.Task{ID: "t-792", Status: "running", IssueNumber: 792}, true
	run.Last = factoryd.RoutineRun{Project: run.Project, Routine: "audit", Run: 4, Started: time.Now(), Trigger: "merges", TaskID: "t-792", Issue: 792, Outcome: "filed"}
	r.routines.views = []factoryd.RoutineView{bump, run}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, codeBoard, nil), ck).Body.String()

	g := ghostIn(t, col(t, body, "Needs you"), "weekly-bump")
	if !strings.Contains(g, `<article class="card ghost held"`) || !strings.Contains(g, `<span class="held">held: #781 is still open</span>`) {
		t.Fatalf("the held ghost is quiet:\n%s", g)
	}
	if !strings.Contains(body, `<span class="cell routine warn">↻ weekly held</span>`) {
		t.Fatal("the strip does not say held")
	}
	if !strings.Contains(g, `<div class="line2 mono">last held</div>`) {
		t.Fatalf("the held ghost does not name its last run:\n%s", g)
	}
	a := ghostIn(t, col(t, body, "Queued"), "audit")
	if !strings.Contains(a, `<span class="run"><i class="dot" aria-hidden="true"></i>#792 running</span>`) || strings.Contains(a, "held") {
		t.Fatalf("the running ghost does not breathe, or says held:\n%s", a)
	}

	// the bump's card filed and open, no run held: the ghost and the strip whisper
	bump.Last = factoryd.RoutineRun{Project: bump.Project, Routine: "weekly-bump", Run: 15, Started: time.Now(), Trigger: "cron", TaskID: "t-781", Issue: 781, Outcome: "filed"}
	r.routines.views = []factoryd.RoutineView{bump, run}
	body = r.do(httptest.NewRequest(http.MethodGet, codeBoard, nil), ck).Body.String()
	if g := ghostIn(t, col(t, body, "Needs you"), "weekly-bump"); strings.Contains(g, "held") || !strings.Contains(g, "last #781 filed") {
		t.Fatalf("an open card reads as a held run:\n%s", g)
	}
	if !strings.Contains(body, `<span class="cell routine">↻ weekly in 20 min</span>`) {
		t.Fatal("the strip does not name the run under an hour away")
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
