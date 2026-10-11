package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// faceOf is the face of card id: the first card article in section that
// links its timeline.
func faceOf(t *testing.T, section, id string) string {
	t.Helper()
	i := strings.Index(section, `href="/tasks/`+id+`/timeline"`)
	if i < 0 {
		t.Fatalf("card %s missing:\n%s", id, section)
	}
	start := strings.LastIndex(section[:i], "<article")
	return section[start : i+strings.Index(section[i:], "</article>")]
}

// titleOf is the title line's words on the face of card id.
func titleOf(t *testing.T, body, id string) string {
	t.Helper()
	face := faceOf(t, body, id)
	_, title, _ := strings.Cut(face, `<strong class="title">`)
	title, _, _ = strings.Cut(title, "</strong>")
	return title
}

// The face's title: a writer's summary, else a planned card's key, else
// the objective's first clause on one line, up to a full stop followed by
// whitespace or ending the text, and at most 80 characters.
func TestCardFaceTitle(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	for _, tc := range []struct {
		name string
		task factoryd.Task
		want string
	}{
		{"plan key", factoryd.Task{PlanKey: "web-pages", Objective: "Build the pages. All of them."}, "web-pages"},
		{"first clause", factoryd.Task{Objective: "Fix the cache. Then update the tests."}, "Fix the cache"},
		{"full stop before a newline", factoryd.Task{Objective: "Fix the cache.\nThen update the tests."}, "Fix the cache"},
		{"full stop before a tab", factoryd.Task{Objective: "Fix the cache.\tThen update the tests."}, "Fix the cache"},
		{"final full stop", factoryd.Task{Objective: "Fix the cache."}, "Fix the cache"},
		{"a path keeps its dot", factoryd.Task{Objective: "Fix internal/console/board.go so it renders"}, "Fix internal/console/board.go so it renders"},
		{"a version keeps its dot", factoryd.Task{Objective: "Bump to v1.2 and release. Then tag it."}, "Bump to v1.2 and release"},
		{"lines joined", factoryd.Task{Objective: "Fix the\ncache"}, "Fix the cache"},
		{"80 characters", factoryd.Task{Objective: strings.Repeat("abcdefghij", 10)}, strings.Repeat("abcdefghij", 8)},
		{"a writer's summary", factoryd.Task{PlanKey: "web-pages", Objective: "Fix the cache.", HumanSummary: "the cache, at last"}, "the cache, at last"},
	} {
		tc.task.ID, tc.task.Kind, tc.task.Status, tc.task.Repository = "t-1", "agent", "running", "sf/code"
		r.tasks.set("", tc.task)
		body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
		if got := titleOf(t, body, "t-1"); got != tc.want {
			t.Errorf("%s: title %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The pill: one class per state and the status's own words, so the three
// waits stay apart; the author's wait wears the hollow pill.
func TestCardFacePillPerStatus(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	for status, class := range map[string]string{
		"running": "st-running", "closing": "st-running", "queued": "st-queued", "waiting-deps": "st-waiting",
		"waiting-human": "st-waiting", "waiting-poster": "st-waiting hollow", "in-review": "st-review",
		"done": "st-done", "failed": "st-failed", "closed": "st-closed", "backlog": "st-backlog",
	} {
		r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: status, Lane: "ready", Repository: "sf/code", Objective: "work"})
		body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
		if want := `<span class="pill ` + class + `">` + status + `</span>`; !strings.Contains(faceOf(t, body, "t-1"), want) {
			t.Errorf("%s: the face lacks %s:\n%s", status, want, faceOf(t, body, "t-1"))
		}
	}
}

// Spend against budget to the cent: muted below 80 percent of the budget,
// from 80 percent on prominent with the warning sign and the words; a card
// without a budget shows none.
func TestCardFaceSpend(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("",
		factoryd.Task{ID: "t-low", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "low", BudgetEUR: 10, SpentEUR: 7.99},
		factoryd.Task{ID: "t-at", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "at 80", BudgetEUR: 10, SpentEUR: 8},
		factoryd.Task{ID: "t-none", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "no budget"})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for id, want := range map[string]string{
		"t-low": `<span class="spend">€7.99 / €10.00</span>`,
		"t-at":  `<span class="spend near">€8.00 / €10.00 ⚠ near its budget</span>`,
	} {
		if face := faceOf(t, body, id); !strings.Contains(face, want) {
			t.Errorf("%s lacks %s:\n%s", id, want, face)
		}
	}
	if face := faceOf(t, body, "t-none"); strings.Contains(face, `class="spend`) {
		t.Errorf("a card without a budget shows spend:\n%s", face)
	}
	css := r.do(httptest.NewRequest(http.MethodGet, "/static/console.css", nil)).Body.String()
	for _, rule := range []string{".spend { color: var(--text-muted); }", ".spend.near { color: var(--warn); font-weight: 600; }"} {
		if !strings.Contains(css, rule) {
			t.Errorf("console.css lacks %s", rule)
		}
	}
}

// The chips line holds only what a card has: its lane badge, its model,
// "blocked by n" with the issues in its title while dependencies are not
// done, "waiting on" in waiting-deps (never both); a card with none has
// no chips line and no transitions line.
func TestCardFaceChips(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-bare", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "bare"}},
		{Task: factoryd.Task{ID: "t-full", Kind: "agent", Status: "running", Lane: "ready", Repository: "sf/code", Objective: "full",
			ModelAlias: "coding-cheap"}, Dependencies: "#3, #5"},
		{Task: factoryd.Task{ID: "t-free", Kind: "agent", Status: "queued", Lane: "ready", Repository: "sf/code", Objective: "deps done"}},
		{Task: factoryd.Task{ID: "t-wait", Kind: "agent", Status: "waiting-deps", Lane: "ready", Repository: "sf/code", Objective: "waits"},
			WaitingOn: "#7", Dependencies: "#7"},
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	full := faceOf(t, body, "t-full")
	for _, want := range []string{`<span class="badge">ready</span>`, `<span class="chip model">coding-cheap</span>`,
		`<span class="chip blocked" title="#3, #5">blocked by 2</span>`} {
		if !strings.Contains(full, want) {
			t.Errorf("t-full lacks %s:\n%s", want, full)
		}
	}
	if bare := faceOf(t, body, "t-bare"); strings.Contains(bare, `class="chips"`) || strings.Contains(bare, `class="transitions"`) {
		t.Errorf("a card with nothing to show has a chips or transitions line:\n%s", bare)
	}
	if free := faceOf(t, body, "t-free"); strings.Contains(free, "blocked by") {
		t.Errorf("a card whose dependencies are done is blocked:\n%s", free)
	}
	wait := faceOf(t, body, "t-wait")
	if !strings.Contains(wait, `<span class="chip waiting">waiting on #7</span>`) || strings.Contains(wait, "blocked by") {
		t.Errorf("a card in waiting-deps shows not exactly its waiting chip:\n%s", wait)
	}
}

// An accepted plan's card rolls its cards up as n running · n done ·
// n waiting, only the parts with cards; with only backlog cards, none.
func TestCardFacePlanChildren(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = append(planTasks(), factoryd.BoardTask{Task: factoryd.Task{ID: "t-c9", Kind: "agent", Status: "running", Lane: "ready",
		Process: "spec", Objective: "card 9", Repository: "linkshort/linkshort", IssueNumber: 9, Source: factoryd.SourcePlan, Parent: "t-plan", PlanKey: "k9"}})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if face := faceOf(t, body, "t-plan"); !strings.Contains(face, `<span class="chip children">1 running · 1 done · 1 waiting</span>`) {
		t.Errorf("the plan's face lacks its rollup:\n%s", face)
	}
	r.tasks.tasks = planTasks()[:3]
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if face := faceOf(t, body, "t-plan"); strings.Contains(face, "chip children") {
		t.Errorf("a plan with only backlog cards shows a rollup:\n%s", face)
	}
}

// Checks n/n from the newest checks step of the card's timeline, read for
// a card with a pull request that is not final; a final card and one
// without a pull request show none.
func TestCardFaceChecks(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	timeline := []byte(`{"step":"session_start","attempt":1}
{"step":"checks","state":"failure","jobs":[{"name":"test","status":"failure"},{"name":"vet","status":"success"}]}
{"step":"checks","state":"failure","jobs":[{"name":"test","status":"success"},{"name":"vet","status":"success"},{"name":"lint","status":"failure"}]}
{"step":"model_review","verdict":"approve"}
`)
	for _, id := range []string{"t-pr", "t-done", "t-nopr"} {
		r.objects["sessions/"+id+"/timeline.jsonl"] = timeline
	}
	r.tasks.set("",
		factoryd.Task{ID: "t-pr", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "a pr", PRNumber: 4},
		factoryd.Task{ID: "t-done", Kind: "agent", Status: "done", Repository: "sf/code", Objective: "merged", PRNumber: 5},
		factoryd.Task{ID: "t-nopr", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "no pr yet"})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if face := faceOf(t, body, "t-pr"); !strings.Contains(face, `<span class="checks">2/3 checks</span>`) {
		t.Errorf("the card in review lacks 2/3 checks:\n%s", face)
	}
	for _, id := range []string{"t-done", "t-nopr"} {
		if face := faceOf(t, body, id); strings.Contains(face, `class="checks"`) {
			t.Errorf("%s shows checks:\n%s", id, face)
		}
	}
}

// An accent is a chip with its name and the card's left edge and changes
// no state rendering; a card without one has neither.
func TestCardFaceAccent(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("",
		factoryd.Task{ID: "t-teal", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "teal work", Accent: "teal", CreatedAt: time.Now()},
		factoryd.Task{ID: "t-none", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "plain work", CreatedAt: time.Now()})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	teal := faceOf(t, body, "t-teal")
	if !strings.HasPrefix(teal, `<article class="card face edge-teal">`) || !strings.Contains(teal, `<span class="chip accent-teal">teal</span>`) ||
		!strings.Contains(teal, `<span class="pill st-running">running</span>`) {
		t.Errorf("the teal card lacks its edge, its chip or its plain running pill:\n%s", teal)
	}
	if none := faceOf(t, body, "t-none"); !strings.HasPrefix(none, `<article class="card face">`) || strings.Contains(none, "chip accent-") {
		t.Errorf("a card without an accent shows one:\n%s", none)
	}
}

// The amber ring and its words: a live card whose newest event is half an
// hour old or more, or that has had none in its half hour on the board;
// never a final card, never one in the backlog.
func TestCardFaceStaleRing(t *testing.T) {
	r, ev := newRigWithEvents(t)
	ck, _ := r.loggedIn(t)
	old, recent := time.Now().Add(-45*time.Minute), time.Now().Add(-5*time.Minute)
	r.tasks.set("",
		factoryd.Task{ID: "t-quiet", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "quiet", CreatedAt: old},
		factoryd.Task{ID: "t-busy", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "busy", CreatedAt: old},
		factoryd.Task{ID: "t-new", Kind: "agent", Status: "queued", Lane: "ready", Repository: "sf/code", Objective: "never moved", CreatedAt: old},
		factoryd.Task{ID: "t-done", Kind: "agent", Status: "done", Repository: "sf/code", Objective: "done", CreatedAt: old},
		factoryd.Task{ID: "t-idea", Kind: "agent", Status: "backlog", Lane: "backlog", Repository: "sf/code", Objective: "an idea", CreatedAt: old})
	for id, at := range map[string]time.Time{"t-quiet": old, "t-busy": recent, "t-done": old, "t-idea": old} {
		if _, err := ev.Record(context.Background(), factoryd.Event{Repository: "sf/code", TaskID: id, Kind: factoryd.EventCardMoved,
			Actor: "sebastian", Via: factoryd.ViaButton, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	quiet := `card_moved by sebastian via button at ` + old.UTC().Format("15:04") + ` — nothing for over 45 min</p>`
	for id, words := range map[string]string{"t-quiet": quiet, "t-new": `<p class="activity">no event yet, 45 min on the card</p>`} {
		if face := faceOf(t, body, id); !strings.HasPrefix(face, `<article class="card face stale">`) || !strings.Contains(face, words) {
			t.Errorf("%s lacks the ring or %s:\n%s", id, words, face)
		}
	}
	for _, id := range []string{"t-busy", "t-done", "t-idea"} {
		if face := faceOf(t, body, id); strings.Contains(face, "stale") || strings.Contains(face, "nothing for over") {
			t.Errorf("%s wears the ring:\n%s", id, face)
		}
	}
}

// A waiting card's activity line is the question it waits on, not its
// newest event, in the escalations and in its column alike.
func TestCardFaceWaitingReason(t *testing.T) {
	r, ev := newRigWithEvents(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set(factoryd.AgentAsks+"use a replica or a shard?\nWhy: the reads grow",
		factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code", Objective: "add the feed", IssueNumber: 12})
	if _, err := ev.Record(context.Background(), factoryd.Event{Repository: "sf/code", TaskID: "t-e", Kind: factoryd.EventCardMoved,
		Actor: "sebastian", Via: factoryd.ViaButton, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	want := `<p class="activity"><a href="https://git.example/sf/code/issues/12">answer on the issue</a> — use a replica or a shard?</p>`
	for _, title := range []string{"Escalations", "Waiting on a human"} {
		if face := faceOf(t, col(t, body, title), "t-e"); !strings.Contains(face, want) || strings.Contains(face, "card_moved") {
			t.Errorf("%s: the waiting card's activity is not its question:\n%s", title, face)
		}
	}
}

// <html> carries the dark theme, the header the toggle, and the toggle's
// script and the light theme's tokens are served.
func TestThemeOnHTMLAndToggleServed(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{`<html lang="en" data-theme="dark">`, `<button type="button" id="theme-toggle"`,
		`<script src="/static/console.js" defer></script>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	js := r.do(httptest.NewRequest(http.MethodGet, "/static/console.js", nil))
	for _, want := range []string{`localStorage.getItem("theme")`, `localStorage.setItem("theme", theme)`,
		`document.documentElement.setAttribute("data-theme", theme)`} {
		if js.Code != http.StatusOK || !strings.Contains(js.Body.String(), want) {
			t.Errorf("console.js = %d, lacks %s", js.Code, want)
		}
	}
	if css := r.do(httptest.NewRequest(http.MethodGet, "/static/console.css", nil)).Body.String(); !strings.Contains(css, "[data-theme=light] {") {
		t.Error("console.css lacks the light theme")
	}
}

// The summary: a writer's POST answers 303 and the face shows it trimmed,
// up to 255 characters (not bytes), 400 beyond with nothing stored; an
// empty or blank one restores the generated title; a non-writer gets 403.
// One row per stored summary.
func TestSummaryPost(t *testing.T) {
	r, ev := newRigWithEvents(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "Fix the cache. Then the tests.", IssueNumber: 1})
	for _, step := range []struct {
		summary string
		code    int
		title   string
		rows    int
	}{
		{"a short summary", http.StatusSeeOther, "a short summary", 1},
		{strings.Repeat("é", 255), http.StatusSeeOther, strings.Repeat("é", 255), 1},
		{strings.Repeat("x", 256), http.StatusBadRequest, strings.Repeat("é", 255), 0},
		{"  the cache  ", http.StatusSeeOther, "the cache", 1},
		{"   ", http.StatusSeeOther, "Fix the cache", 1},
		{"", http.StatusSeeOther, "Fix the cache", 1},
	} {
		before := ev.count()
		rec := r.do(post("/tasks/t-1/summary", url.Values{"csrf": {s.csrf}, "summary": {step.summary}}), ck)
		if rec.Code != step.code || ev.count()-before != step.rows {
			t.Fatalf("summary %.20q = %d with %d rows, want %d with %d: %s", step.summary, rec.Code, ev.count()-before, step.code, step.rows, rec.Body)
		}
		if got := titleOf(t, r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(), "t-1"); got != step.title {
			t.Fatalf("after summary %.20q the title is %.20q, want %.20q", step.summary, got, step.title)
		}
	}
	r.repoWriters["sf/code"] = nil
	r.c.mu.Lock()
	r.c.moves = nil
	r.c.mu.Unlock()
	before := ev.count()
	if rec := r.do(post("/tasks/t-1/summary", url.Values{"csrf": {s.csrf}, "summary": {"mine"}}), ck); rec.Code != http.StatusForbidden || ev.count() != before {
		t.Fatalf("a non-writer's summary = %d with %d rows, want 403 and none", rec.Code, ev.count()-before)
	}
	if got := titleOf(t, r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(), "t-1"); got != "Fix the cache" {
		t.Fatalf("a refused summary changed the title to %q", got)
	}
}

// The accent: each of the eight names answers 303 and lands on the card
// with one row; another name answers 400 and leaves it; none (the empty
// value) clears it; a non-writer gets 403.
func TestAccentPost(t *testing.T) {
	r, ev := newRigWithEvents(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "work", IssueNumber: 1})
	stored := func() string {
		r.tasks.mu.Lock()
		defer r.tasks.mu.Unlock()
		return r.tasks.tasks[0].Accent
	}
	accent := func(value string) int {
		return r.do(post("/tasks/t-1/accent", url.Values{"csrf": {s.csrf}, "value": {value}}), ck).Code
	}
	for _, name := range []string{"red", "orange", "amber", "green", "teal", "blue", "violet", "pink"} {
		before := ev.count()
		if code := accent(name); code != http.StatusSeeOther || stored() != name || ev.count()-before != 1 {
			t.Fatalf("accent %s = %d, stored %q, %d rows; want 303, stored, one row", name, code, stored(), ev.count()-before)
		}
	}
	if code := accent("purple"); code != http.StatusBadRequest || stored() != "pink" {
		t.Fatalf("accent purple = %d, stored %q; want 400 and pink kept", code, stored())
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if face := faceOf(t, body, "t-1"); !strings.Contains(face, `<option value="">none</option>`) || !strings.Contains(face, `<option value="pink" selected>pink</option>`) {
		t.Fatalf("the accent select lacks none or the chosen pink:\n%s", face)
	}
	if code := accent(""); code != http.StatusSeeOther || stored() != "" {
		t.Fatalf("accent none = %d, stored %q; want 303 and cleared", code, stored())
	}
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if face := faceOf(t, body, "t-1"); strings.Contains(face, "chip accent-") || strings.Contains(face, "edge-") ||
		!strings.Contains(face, `<option value="" selected>none</option>`) {
		t.Fatalf("a cleared accent still shows:\n%s", face)
	}
	r.repoWriters["sf/code"] = nil
	r.c.mu.Lock()
	r.c.moves = nil
	r.c.mu.Unlock()
	if code := accent("blue"); code != http.StatusForbidden || stored() != "" {
		t.Fatalf("a non-writer's accent = %d, stored %q; want 403 and nothing", code, stored())
	}
}

// ?accent= narrows the board to the cards of one accent; another name is
// refused.
func TestAccentFilterNarrowsBoard(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("",
		factoryd.Task{ID: "t-blue", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "blue work", Accent: "blue"},
		factoryd.Task{ID: "t-red", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "red work", Accent: "red"},
		factoryd.Task{ID: "t-none", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "plain work"})
	body := r.do(httptest.NewRequest(http.MethodGet, "/?accent=blue", nil), ck).Body.String()
	if !strings.Contains(body, "blue work") || strings.Contains(body, "red work") || strings.Contains(body, "plain work") {
		t.Errorf("the blue board shows another card or not its own:\n%s", col(t, body, "Running"))
	}
	if !strings.Contains(body, `<option value="blue" selected>blue</option>`) {
		t.Error("the filter's select does not keep blue")
	}
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/?accent=purple", nil), ck); rec.Code != http.StatusBadRequest {
		t.Errorf("accent purple = %d, want 400", rec.Code)
	}
}

// On a filtered board every form carries the filter back, never the card's
// own accent: move, retry, Start plan, the plan's accept, changes and
// reject, summary and accent; the poll asks for the same board, and a
// browser's POST is sent back to it.
func TestFilteredBoardFormsCarryTheFilter(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	tasks := append(planTasks(), failedCard())
	for i := range tasks {
		tasks[i].Accent = "teal"
	}
	r.tasks.tasks = tasks
	r.tasks.plans = map[string][]factoryd.PlanRevision{"t-spec2": {planRevision("t-spec2", 1, "sha-1")}}
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 20, Kind: factoryd.ApprovalPlan, TaskID: "t-spec2",
		Project: "linkshort/linkshort", Status: "pending", Spec: json.RawMessage(`{"revision":1,"sha256":"sha-1"}`)})
	ck, s := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort&accent=teal", nil), ck).Body.String()
	filter := `<input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value="linkshort"><input type="hidden" name="accent" value="teal">`
	htmx := func(action string) string {
		return `action="` + action + `" hx-post="` + action + `" hx-target="#board" hx-swap="outerHTML"`
	}
	for _, want := range []string{
		`hx-get="/board?accent=teal&amp;project=linkshort"`,
		htmx("/cards/t-c2/lane") + ` class="inline move" data-move="ready">` + filter,
		htmx("/tasks/t-c7/retry") + ` class="inline">` + filter,
		htmx("/tasks/t-plan/start-plan") + ` class="inline">` + filter,
		htmx("/tasks/t-spec2/plan/accept") + ">\n" + filter,
		htmx("/tasks/t-spec2/plan/changes") + ">\n" + filter,
		htmx("/tasks/t-spec2/plan/reject") + ">\n" + filter,
		htmx("/tasks/t-c2/summary") + ` class="edit-form summary-edit">` + filter,
		htmx("/tasks/t-c2/accent") + ` class="edit-form accent-edit">` + filter,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the filtered board lacks %s", want)
		}
	}
	if plain := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); strings.Contains(plain, `name="accent" value="teal"`) {
		t.Error("on the unfiltered board a form carries the card's accent as the board's filter")
	}
	for path, form := range map[string]url.Values{
		"/tasks/t-c2/summary": {"summary": {"links"}},
		"/tasks/t-c2/accent":  {"value": {"blue"}},
	} {
		form.Set("csrf", s.csrf)
		form.Set("project", "linkshort")
		form.Set("accent", "teal")
		if rec := r.do(post(path, form), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?accent=teal&project=linkshort" {
			t.Errorf("%s = %d to %q, want 303 to the filtered board", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	r.tasks.mu.Lock()
	defer r.tasks.mu.Unlock()
	if got := r.tasks.tasks[1]; got.ID != "t-c2" || got.HumanSummary != "links" || got.Accent != "blue" {
		t.Errorf("t-c2 = %q, %q; want the summary links and the accent blue, not the filter's teal", got.HumanSummary, got.Accent)
	}
}
