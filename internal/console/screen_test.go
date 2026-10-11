package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// columnItems are what column title holds, in order: "t-x" for a card's
// face, "approval 3" for an approval request.
func columnItems(t *testing.T, body, title string) []string {
	t.Helper()
	var out []string
	re := regexp.MustCompile(`data-card="([^"]+)"|<article class="card approval[^"]*" aria-labelledby="approval-(\d+)"`)
	for _, m := range re.FindAllStringSubmatch(col(t, body, title), -1) {
		if m[1] != "" {
			out = append(out, m[1])
		} else {
			out = append(out, "approval "+m[2])
		}
	}
	return out
}

// screenRig is the code project with a card in every status, two pending
// approval requests and a card of another project. Needs you holds, for
// sebastian, oldest waiting first: approval 4 (4 h), t-me waiting on him
// as its author (3 h), the escalated t-esc (2 h), approval 3 (1 h) and the
// human task t-human (20 min).
func screenRig(t *testing.T) (*rig, *http.Cookie) {
	t.Helper()
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	now := time.Now()
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	task := func(id, status, lane string) factoryd.Task {
		return factoryd.Task{ID: id, Kind: "agent", Status: status, Lane: lane, Repository: "sf/code", Objective: id, CreatedAt: now}
	}
	esc := factoryd.BoardTask{Task: task("t-esc", "waiting-human", "ready"), Escalation: factoryd.AgentAsks + "which region?",
		EscalatedAt: now.Add(-2 * time.Hour), EscalationAlert: 7}
	me := factoryd.BoardTask{Task: task("t-me", "waiting-poster", "ready")}
	me.Author, me.GateSince = "sebastian", ago(3*time.Hour)
	other := factoryd.BoardTask{Task: task("t-other", "waiting-poster", "ready")}
	other.Author, other.GateSince = "alice", ago(5*time.Hour)
	r.tasks.tasks = []factoryd.BoardTask{esc, me, other,
		{Task: factoryd.Task{ID: "t-human", Kind: "human", Status: "queued", Repository: "sf/code", Objective: "rotate the key", CreatedAt: now.Add(-20 * time.Minute)}},
		{Task: task("t-back", "backlog", "backlog")}, {Task: task("t-queue", "queued", "ready")}, {Task: task("t-deps", "waiting-deps", "ready"), WaitingOn: "#9"},
		{Task: task("t-run", "running", "ready")}, {Task: task("t-close", "closing", "ready")}, {Task: task("t-rev", "in-review", "ready")},
		{Task: task("t-done", "done", "ready")}, {Task: task("t-fail", "failed", "ready")}, {Task: task("t-closed", "closed", "ready")},
		{Task: factoryd.Task{ID: "t-elsewhere", Kind: "agent", Status: "running", Repository: "demo-app/demo-app", Objective: "elsewhere", CreatedAt: now}},
	}
	r.approvals.rows[0].CreatedAt = now.Add(-time.Hour)
	r.approvals.rows[1].CreatedAt = now.Add(-4 * time.Hour)
	return r, ck
}

// Ruling 255: the five columns are a view over the task states. Every
// status lands in exactly one column, waiting on the author in Needs you
// for the author and in Queued for anyone else; Needs you also holds the
// human lane (the approval requests, the escalation with its answer form,
// the human task), oldest waiting first; the headers count the column;
// another project's card is on no column.
func TestFiveColumnsPlaceEveryStatus(t *testing.T) {
	r, ck := screenRig(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	want := map[string][]string{
		"Needs you": {"approval 4", "t-me", "t-esc", "approval 3", "t-human"},
		"Queued":    {"t-other", "t-back", "t-queue", "t-deps"},
		"Working":   {"t-run", "t-close"},
		"Verifying": {"t-rev"},
		"Done":      {"t-done", "t-fail", "t-closed"},
	}
	counts := map[string]string{"Needs you": "5", "Queued": "4", "Working": "2", "Verifying": "1", "Done": "3"}
	for title, items := range want {
		if got := columnItems(t, body, title); !slices.Equal(got, items) {
			t.Errorf("%s holds %v, want %v", title, got, items)
		}
		if !strings.Contains(body, ">"+title+` <span class="count">`+counts[title]+"</span></h3>") {
			t.Errorf("the header of %s lacks the count %s", title, counts[title])
		}
		for _, id := range items {
			if id[0] == 't' && strings.Count(body, `data-card="`+id+`"`) != 1 {
				t.Errorf("%s shows %d times, want once", id, strings.Count(body, `data-card="`+id+`"`))
			}
		}
	}
	if strings.Contains(body, "t-elsewhere") || strings.Contains(body, `id="human-lane"`) || strings.Contains(body, `data-lane=`) {
		t.Error("the five columns show another project's card, the human lane or a lane column")
	}
	needs := col(t, body, "Needs you")
	for _, want := range []string{`<section class="column" aria-labelledby="col-0" id="needs-you">`, `action="/tasks/t-esc/answer"`,
		`<input type="hidden" name="back" value="/?project=ai-software-factory-code">`, `action="/approvals/4/approve"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the board lacks %s", want)
		}
	}
	if strings.Count(needs, `/answer"`) != 1 {
		t.Errorf("Needs you holds %d answer forms, want t-esc's", strings.Count(needs, `/answer"`))
	}
	// the place names the column, so a card that changes column for its
	// viewer swaps the board
	if !strings.Contains(body, `data-card="t-me" data-place="waiting-poster ready  false false 0 0"`) {
		t.Error("t-me's place does not name Needs you")
	}

	r.repoWriters["sf/code"] = append(r.repoWriters["sf/code"], "alice")
	ak, _ := r.as(t, "alice")
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), ak).Body.String()
	for title, items := range map[string][]string{
		"Needs you": {"t-other", "approval 4", "t-esc", "approval 3", "t-human"},
		"Queued":    {"t-me", "t-back", "t-queue", "t-deps"},
	} {
		if got := columnItems(t, body, title); !slices.Equal(got, items) {
			t.Errorf("alice: %s holds %v, want %v", title, got, items)
		}
	}
	if !strings.Contains(body, `data-card="t-me" data-place="waiting-poster ready  false false 0 1"`) {
		t.Error("alice: t-me's place does not name Queued")
	}
}

// The status line: the project, its state word, n working, n need you
// (the Needs you column, whatever the accent) and the spend of its tasks
// from the first UTC day of the month on against its month cap, the
// project file's budget.month_eur, else the policy's; a spend row of the
// month before and another project's do not count.
func TestStatusLineFigures(t *testing.T) {
	r, ck := screenRig(t)
	withLinkshort(r)
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	r.chat.spend = []spendRow{{"sf/code", month.AddDate(0, 0, -1), 7, false}, {"sf/code", month, 5, false}, {"sf/code", now, 0.5, true},
		{"demo-app/demo-app", month, 3, false}}
	r.ledger.monthCaps = map[string]float64{"demo-app": 12}
	line := func(query string) string {
		t.Helper()
		body := r.do(httptest.NewRequest(http.MethodGet, "/"+query, nil), ck).Body.String()
		start := strings.Index(body, `<p class="status-line"`)
		if start < 0 {
			t.Fatalf("board%s has no status line", query)
		}
		return body[start : start+strings.Index(body[start:], "</p>")+len("</p>")]
	}
	const title = `title="spend this UTC month against the project's month cap"`
	code := `<p class="status-line" aria-label="Status of the project">
<strong class="pname">ai-software-factory-code</strong>
<span class="state" data-state="waiting on you"><span aria-hidden="true">●</span> waiting on you</span>
<span class="fig">2 working</span>
<a href="#needs-you" class="fig need">5 need you</a>
<a href="/costs" class="fig" ` + title + `>€5.50 / €50.00</a>
</p>`
	for _, query := range []string{"", "?view=wide", "?accent=teal"} {
		if got := line(query); got != code {
			t.Errorf("board%s: status line\n%s\nwant\n%s", query, got, code)
		}
	}
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-ask", Kind: "agent", Status: "waiting-poster", Author: "sebastian", Repository: "demo-app/demo-app"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-old", Kind: "agent", Status: "done", Repository: "linkshort/linkshort"}})
	r.tasks.tasks = slices.DeleteFunc(r.tasks.tasks, func(b factoryd.BoardTask) bool { return b.ID == "t-elsewhere" })
	if got, want := line("?project=demo-app"), `<p class="status-line" aria-label="Status of the project">
<strong class="pname">demo-app</strong>
<span class="state" data-state="waiting on you"><span aria-hidden="true">●</span> waiting on you</span>
<span class="fig">0 working</span>
<a href="#needs-you" class="fig need">1 need you</a>
<a href="/costs" class="fig" `+title+`>€3.00 / €12.00</a>
</p>`; got != want {
		t.Errorf("demo-app: status line\n%s\nwant\n%s", got, want)
	}
	if got, want := line("?project=linkshort"), `<p class="status-line" aria-label="Status of the project">
<strong class="pname">linkshort</strong>
<span class="state" data-state="idle"><span aria-hidden="true">●</span> idle</span>
<span class="fig">0 working</span>
<a href="#needs-you" class="fig">0 need you</a>
<a href="/costs" class="fig" `+title+`>€0.00 / €50.00</a>
</p>`; got != want {
		t.Errorf("linkshort: status line\n%s\nwant\n%s", got, want)
	}
}

// One project at a time: the board shows the first project the viewer
// writes, the select offers the projects they write and the one shown,
// never every project; a viewer who writes none sees why.
func TestBoardShowsOneProjectTheViewerWrites(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	r.tasks.tasks = planTasks()
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{`<select id="project-filter" name="project">
<option value="ai-software-factory-code" selected>ai-software-factory-code</option>
<option value="demo-app">demo-app</option>
<option value="linkshort">linkshort</option>
</select>`, `data-project="ai-software-factory-code"`, `data-card="t-run"`} {
		if !strings.Contains(body, want) {
			t.Errorf("sebastian's board lacks %s", want)
		}
	}
	if strings.Contains(body, "every project") || strings.Contains(body, `data-card="t-c2"`) {
		t.Error("sebastian's board offers every project or shows linkshort's card")
	}
	r.repoWriters["linkshort/linkshort"] = []string{"sebastian", "alice"}
	ak, _ := r.as(t, "alice")
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), ak).Body.String()
	if !strings.Contains(body, `<select id="project-filter" name="project">
<option value="linkshort" selected>linkshort</option>
</select>`) || !strings.Contains(body, `data-card="t-c2"`) {
		t.Error("alice's board is not linkshort's alone")
	}
	body = r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ak).Body.String()
	if !strings.Contains(body, `<option value="linkshort">linkshort</option>
<option value="ai-software-factory-code" selected>ai-software-factory-code</option>
</select>`) || !strings.Contains(body, `data-card="t-run"`) || strings.Contains(body, `/tasks/t-run/summary"`) {
		t.Error("alice reading the code project: the select lacks it, or the board lacks its card or offers her its forms")
	}
	// her board follows no stream she may not read and opens no drawer
	if !strings.Contains(body, `<div id="board" data-project="" data-query="?project=ai-software-factory-code"`) ||
		!strings.Contains(body, `<strong class="title" title="fix the cache">fix the cache</strong>`) {
		t.Error("alice reading the code project follows its stream or links its cards to the drawer")
	}
	bk, _ := r.as(t, "bob")
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), bk).Body.String()
	if !strings.Contains(body, `<p class="problem" role="alert">You write none of the projects agents work on.</p>`) ||
		!strings.Contains(body, `data-project=""`) || strings.Contains(body, "status-line") || strings.Contains(body, "data-card") {
		t.Error("bob, who writes no project, does not see why his board is empty")
	}
}

// The lane is a chip on every card of the five columns whose status is
// another word, and the fine state a second chip in the pill's hues:
// waiting on the author, retrying (the newest session ended for a retry),
// waiting for a roll (its next session held while a roll waits for a
// session gap), stale (no event for half an hour); failed and closed are the pill's own
// words. The wide view's columns name the lane and the wait on the
// author, so there a card wears only what the column leaves unsaid.
func TestCardLaneAndFineChips(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	now := time.Now()
	task := func(id, status, lane string) factoryd.BoardTask {
		return factoryd.BoardTask{Task: factoryd.Task{ID: id, Kind: "agent", Status: status, Lane: lane, Repository: "sf/code", Objective: id, CreatedAt: now}}
	}
	other, stale := task("t-other", "waiting-poster", "ready"), task("t-stale", "running", "ready")
	other.Author, stale.CreatedAt = "alice", now.Add(-50*time.Minute)
	r.tasks.tasks = []factoryd.BoardTask{task("t-lane", "queued", "ready"), task("t-back", "backlog", "backlog"), other, task("t-fail", "failed", "ready"),
		task("t-closed", "closed", ""), task("t-done", "done", "ready"), stale, task("t-retry", "running", "ready"), task("t-again", "running", "ready"),
		task("t-held", "running", "ready"), task("t-unheld", "running", "ready")}
	r.objects["sessions/t-retry/timeline.jsonl"] = []byte(`{"step":"session_start","attempt":1}
{"step":"session_end","attempt":1,"outcome":"retry_stronger"}
`)
	r.objects["sessions/t-again/timeline.jsonl"] = []byte(`{"step":"session_start","attempt":1}
{"step":"session_end","attempt":1,"outcome":"retry_stronger"}
{"step":"session_start","attempt":2}
`)
	r.objects["sessions/t-held/timeline.jsonl"] = []byte(`{"step":"session_end","attempt":1,"outcome":"retry_stronger"}
{"step":"held","roll":"t-roll"}
`)
	r.objects["sessions/t-unheld/timeline.jsonl"] = []byte(`{"step":"held","roll":"t-roll"}
{"step":"session_start","attempt":1}
`)
	chips := func(body, id string) string {
		t.Helper()
		face := faceOf(t, body, id)
		start := strings.Index(face, `<p class="chips">`)
		if start < 0 {
			return ""
		}
		return face[start : start+strings.Index(face[start:], "</p>")+len("</p>")]
	}
	five := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	wide := r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String()
	const ready = `<span class="badge">ready</span> `
	for id, want := range map[string][2]string{
		"t-lane":   {`<p class="chips">` + ready + `</p>`, ""},
		"t-back":   {"", ""},
		"t-other":  {`<p class="chips">` + ready + `<span class="pill fine st-waiting hollow">waiting on the author</span> </p>`, `<p class="chips">` + ready + `</p>`},
		"t-fail":   {`<p class="chips">` + ready + `</p>`, `<p class="chips">` + ready + `</p>`},
		"t-closed": {"", ""},
		"t-done":   {`<p class="chips">` + ready + `</p>`, `<p class="chips">` + ready + `</p>`},
		"t-stale":  {`<p class="chips">` + ready + `<span class="pill fine st-stale">stale</span> </p>`, `<p class="chips">` + ready + `<span class="pill fine st-stale">stale</span> </p>`},
		"t-retry":  {`<p class="chips">` + ready + `<span class="pill fine st-running">retrying</span> </p>`, `<p class="chips">` + ready + `<span class="pill fine st-running">retrying</span> </p>`},
		"t-again":  {`<p class="chips">` + ready + `</p>`, `<p class="chips">` + ready + `</p>`},
		"t-held":   {`<p class="chips">` + ready + `<span class="pill fine st-waiting">waiting for a roll</span> </p>`, `<p class="chips">` + ready + `<span class="pill fine st-waiting">waiting for a roll</span> </p>`},
		"t-unheld": {`<p class="chips">` + ready + `</p>`, `<p class="chips">` + ready + `</p>`},
	} {
		if got := chips(five, id); got != want[0] {
			t.Errorf("five columns, %s: chips %q, want %q", id, got, want[0])
		}
		if got := chips(wide, id); got != want[1] {
			t.Errorf("wide view, %s: chips %q, want %q", id, got, want[1])
		}
	}
}

// The wide view keeps the board as it was, the lanes of the project's
// process first, under the human lane, with the status line; the view
// rides on the poll, the script's swaps and every form, and the header's
// switch names the other view.
func TestWideViewKeepsTheLanesAndSurvivesThePoll(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	r.tasks.tasks = planTasks()
	ck, s := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort&view=wide", nil), ck).Body.String()
	var titles []string
	for _, m := range regexp.MustCompile(`<h3 id="col-\d+">([^<]+) <span`).FindAllStringSubmatch(body, -1) {
		titles = append(titles, m[1])
	}
	if want := []string{"Backlog", "Plan", "Ready", "Running", "Waiting on a human", "Waiting on the author", "In review", "Done", "Failed", "Closed"}; !slices.Equal(titles, want) {
		t.Errorf("the wide view's columns are %v, want %v", titles, want)
	}
	for _, want := range []string{`<div id="board" data-project="linkshort" data-query="?project=linkshort&amp;view=wide" hx-get="/board?project=linkshort&amp;view=wide" hx-trigger="every 30s" hx-swap="outerHTML">`,
		`<section aria-labelledby="human-lane" id="needs-you">`, `class="status-line"`,
		`<input type="hidden" name="project" value="linkshort"><input type="hidden" name="accent" value=""><input type="hidden" name="view" value="wide">`,
		`<a href="/?project=linkshort" id="view-toggle" class="view-toggle" data-view="" aria-keyshortcuts="w">Five columns</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the wide view lacks %s", want)
		}
	}
	if poll := r.do(httptest.NewRequest(http.MethodGet, "/board?project=linkshort&view=wide", nil), ck).Body.String(); !strings.Contains(poll, `data-lane="backlog"`) ||
		strings.Contains(poll, `class="lane five"`) {
		t.Error("the wide view's poll answers the five columns")
	}
	if frag := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-c2?project=linkshort&view=wide", nil), ck).Body.String(); !strings.Contains(frag,
		`<input type="hidden" name="project" value="linkshort"><input type="hidden" name="accent" value=""><input type="hidden" name="view" value="wide">`) {
		t.Errorf("the card under the wide view's query keeps no view on its forms: %s", frag)
	}
	form := url.Values{"csrf": {s.csrf}, "summary": {"the links"}, "project": {"linkshort"}, "view": {"wide"}}
	req := post("/tasks/t-c2/summary", form)
	req.Header.Set("HX-Request", "true")
	if rec := r.do(req, ck); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `data-lane="backlog"`) {
		t.Errorf("a form of the wide view answers %d without the lanes", rec.Code)
	}
	if rec := r.do(post("/tasks/t-c2/summary", form), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?project=linkshort&view=wide" {
		t.Errorf("a browser's form of the wide view: %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	five := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	if !strings.Contains(five, `<a href="/?project=linkshort&amp;view=wide" id="view-toggle" class="view-toggle" data-view="wide" aria-keyshortcuts="w">Wide view</a>`) ||
		!strings.Contains(five, `<div class="lane five">`) {
		t.Error("the five columns lack the switch to the wide view")
	}
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/?view=tall", nil), ck); rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown view = %d, want 400", rec.Code)
	}
}

// The page holds the drawer, empty and hidden, every card's title opens
// it by #card=<id>, and the ? list names the keys w, Escape and ?; the
// script binds them on the board only, never with a modifier, and the
// head's script restores the remembered view with the card's hash.
func TestKeysAndDrawerOnThePage(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "Fix the cache.", IssueNumber: 11})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{`<aside id="drawer" class="drawer" aria-label="The card" hidden></aside>`,
		`<strong class="title" title="Fix the cache"><a href="#card=t-run" class="open">Fix the cache</a></strong>`,
		`<button type="button" id="keys-toggle" class="keys-toggle" aria-haspopup="dialog" aria-controls="keys" aria-keyshortcuts="Shift+?" title="Keys">?</button>`,
		`<div id="keys" class="overlay" role="dialog" aria-modal="true" aria-labelledby="keys-title" hidden>`,
		`<dt><kbd>w</kbd></dt><dd>the wide view, the lanes as columns, or back to the five columns</dd>`,
		`<dt><kbd>Escape</kbd></dt><dd>close the card's drawer, or this list</dd>`,
		`<dt><kbd>?</kbd></dt><dd>this list</dd>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the board page lacks %s", want)
		}
	}
	for path, wants := range map[string][]string{
		"/static/console.js": {`if (!document.getElementById("board") || e.ctrlKey || e.metaKey || e.altKey) return;`, `e.key === "Escape"`,
			`e.key === "w"`, `e.key === "?"`, `window.addEventListener("hashchange", showDrawer);`, `"/drawer"`,
			`if (drawer && !drawer.hidden) return true;`},
		"/static/theme.js": {`localStorage.getItem("view") !== "wide"`, `location.replace("/?" + q + location.hash);`},
	} {
		js := r.do(httptest.NewRequest(http.MethodGet, path, nil)).Body.String()
		for _, want := range wants {
			if !strings.Contains(js, want) {
				t.Errorf("%s lacks %s", path, want)
			}
		}
	}
}

// failingObjects is an object store that cannot be read.
type failingObjects struct{ fakeObjects }

func (failingObjects) Get(context.Context, string) ([]byte, error) { return nil, errors.New("s3: 503") }

// drawerRig is linkshort's plan with card #4 waiting on #3 (and on #5,
// done), a planned objective and the plan's revision; the code project's
// running t-run with a timeline, t-m whose merge request 4 waits, and
// t-pr with pull request 31.
func drawerRig(t *testing.T) (*rig, *http.Cookie) {
	t.Helper()
	r := newRig(t)
	withLinkshort(r)
	r.tasks.tasks = append(planTasks(),
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "Add the column", IssueNumber: 20, PRNumber: 9}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-pr", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "Draw the drawer", IssueNumber: 21, PRNumber: 31}})
	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].ID == "t-c4" {
			r.tasks.tasks[i].Objective = "Links API\n\nCard links-api of plan #1 (3 of 4), accepted by sebastian. Written by the planner.\n\nBuild the links API.\nDepends on: #3"
		}
	}
	r.tasks.deps = map[string][]string{"t-c4": {"t-c3", "t-c5"}}
	r.tasks.plans = map[string][]factoryd.PlanRevision{"t-plan": {planRevision("t-plan", 1, "sha-1")}}
	r.objects["sessions/t-run/timeline.jsonl"] = []byte(`{"time":"2026-10-11T10:05:00Z","step":"session_end","started":"2026-10-11T10:00:00Z","attempt":1,"outcome":"pushed"}
`)
	ck, _ := r.loggedIn(t)
	return r, ck
}

// drawer is card id's drawer on tab ("" the default).
func drawer(t *testing.T, r *rig, ck *http.Cookie, id, tab string) string {
	t.Helper()
	path := "/cards/" + id + "/drawer"
	if tab != "" {
		path += "?tab=" + tab
	}
	rec := r.do(httptest.NewRequest(http.MethodGet, path, nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// The drawer's Overview: the three tabs, the objective as its headline, a
// planned card's provenance line and its body, the related cards in their
// live state, each opening in the drawer (blocked by the dependencies not
// done, part of plan; a plan's children), the plan's table and a pending
// merge's reviews and audit.
func TestDrawerOverview(t *testing.T) {
	r, ck := drawerRig(t)
	open := func(id, label string) string {
		return `<a href="#card=` + id + `" hx-get="/cards/` + id + `/drawer" hx-target="#drawer" hx-swap="innerHTML">` + label + `</a></li>`
	}
	tab := func(id, name, title string, on bool) string {
		selected := "false"
		if on {
			selected = "true"
		}
		return `<button type="button" role="tab" aria-selected="` + selected + `" hx-get="/cards/` + id + `/drawer?tab=` + name + `" hx-target="#drawer" hx-swap="innerHTML">` + title + `</button>`
	}
	card := drawer(t, r, ck, "t-c4", "")
	for _, want := range []string{
		`<div class="drawer-content" data-drawer="t-c4">`,
		`<h2 tabindex="-1"><a href="https://git.example/linkshort/linkshort/issues/4">#4</a> k4</h2>`,
		`<nav class="tabs" role="tablist" aria-label="The card's tabs">` + tab("t-c4", "overview", "Overview", true) +
			tab("t-c4", "activity", "Activity", false) + tab("t-c4", "files", "Files", false) + `</nav>`,
		`<h3 class="headline">Links API</h3>`,
		`<p class="provenance">Card links-api of plan #1 (3 of 4), accepted by sebastian. Written by the planner.</p>`,
		"<p class=\"objective-body\">Build the links API.\nDepends on: #3</p>",
		`<li><span class="why">blocked by</span> <span class="pill st-backlog">backlog</span> ` + open("t-c3", "#3 k3"),
		`<li><span class="why">part of plan</span> <span class="pill st-done">done</span> ` + open("t-plan", "#1 Link shortener"),
		`<a href="/tasks/t-c4/timeline">the full timeline</a>`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("t-c4's overview lacks %s:\n%s", want, card)
		}
	}
	if strings.Contains(card, "t-c5") || strings.Count(card, "<li>") != 2 {
		t.Errorf("t-c4's related cards are not exactly #3 and the plan:\n%s", card)
	}

	plan := drawer(t, r, ck, "t-plan", "overview")
	for _, want := range []string{`<h3 class="headline">Link shortener</h3>`,
		"<p>Foundation first, then the API, then the acceptance tests.</p>",
		"<tr><td>foundation</td><td>foundation</td><td>Foundation</td><td>medium</td><td></td></tr>",
		"<tr><td>links-api</td><td>feature</td><td>Links API</td><td>medium</td><td>foundation</td></tr>",
		"<tr><td>integration</td><td>integration</td><td>Acceptance tests</td><td>small</td><td>foundation, links-api</td></tr>",
		`<li><span class="why">child</span> <span class="pill st-backlog">backlog</span> ` + open("t-c2", "#2 k2"),
		`<li><span class="why">child</span> <span class="pill st-backlog">backlog</span> ` + open("t-c3", "#3 k3"),
		`<li><span class="why">child</span> <span class="pill st-waiting">waiting-deps</span> ` + open("t-c4", "#4 k4"),
		`<li><span class="why">child</span> <span class="pill st-done">done</span> ` + open("t-c5", "#5 k5"),
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("the plan's overview lacks %s", want)
		}
	}
	if strings.Contains(plan, "provenance") || strings.Contains(plan, "objective-body") {
		t.Error("the plan's one-line objective shows a provenance or a body")
	}

	merge := drawer(t, r, ck, "t-m", "overview")
	for _, want := range []string{`<h4>Reviews and audit of <a href="https://git.example/sf/code/pulls/9">PR #9</a></h4>`,
		"<dd>approve: adds the column safely</dd>",
		"<dt>Earlier review 1</dt><dd>concerns: the column lacks a default<br>- 0018_x.sql: add a default</dd>",
		"<dt>Security audit</dt><dd>findings (coding-frontier, zai/glm-5.3)<br>- secret in the diff: private key: <code>internal/x/x_test.go:14</code>"} {
		if !strings.Contains(merge, want) {
			t.Errorf("t-m's overview lacks %s", want)
		}
	}
	if strings.Contains(drawer(t, r, ck, "t-pr", ""), "Reviews and audit") {
		t.Error("a card without a pending merge shows reviews")
	}
}

// The drawer's Activity is the timeline page's gates and steps; a
// timeline it cannot read says so.
func TestDrawerActivity(t *testing.T) {
	r, ck := drawerRig(t)
	activity := drawer(t, r, ck, "t-run", "activity")
	page := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-run/timeline", nil), ck).Body.String()
	tables := func(body string) string {
		start := strings.Index(body, `<table class="gates">`)
		if start < 0 {
			return ""
		}
		end := strings.LastIndex(body, "</table>") + len("</table>")
		return body[start:end]
	}
	if got := tables(activity); got == "" || got != tables(page) {
		t.Errorf("the activity's tables are not the timeline page's:\n%s\nwant\n%s", got, tables(page))
	}
	for _, want := range []string{
		`<tr><td>session</td><td>1</td><td>2026-10-11 10:00:00</td><td>5 min</td><td>pushed</td><td><meter min="0" max="300" value="300">5 min</meter></td></tr>`,
		`<tr><td>2026-10-11T10:05:00Z</td><td>session_end</td><td></td><td><code>{&#34;attempt&#34;:1,&#34;outcome&#34;:&#34;pushed&#34;,&#34;started&#34;:&#34;2026-10-11T10:00:00Z&#34;}</code></td></tr>`,
	} {
		if !strings.Contains(activity, want) {
			t.Errorf("the activity lacks %s", want)
		}
	}
	r.c.cfg.Timelines = failingObjects{}
	failed := drawer(t, r, ck, "t-run", "activity")
	if !strings.Contains(failed, `<p class="problem" role="alert">The timeline could not be read; see the factoryd log.</p>`) || strings.Contains(failed, "<table") {
		t.Errorf("an unreadable timeline:\n%s", failed)
	}
}

// The drawer's Files are the pull request's changed files, every page of
// Forgejo's list read with the human's token, or no pull request yet.
func TestDrawerFiles(t *testing.T) {
	r, ck := drawerRig(t)
	r.fg.calls = nil
	files := drawer(t, r, ck, "t-pr", "files")
	if want := `<tbody><tr><td><code>internal/console/board.go</code></td><td>modified</td><td>+120</td><td>−38</td></tr>
<tr><td><code>internal/console/drawer_test.go</code></td><td>added</td><td>+85</td><td>−0</td></tr>
<tr><td><code>internal/console/old.go</code></td><td>removed</td><td>+0</td><td>−12</td></tr>
</tbody>`; !strings.Contains(files, want) || !strings.Contains(files, "<caption>The changed files of PR #31</caption>") {
		t.Errorf("t-pr's files:\n%s\nwant\n%s", files, want)
	}
	want := []string{"GET /api/v1/repos/sf/code/pulls/31/files Bearer access-1", "GET /api/v1/repos/sf/code/pulls/31/files Bearer access-1",
		"GET /api/v1/repos/sf/code/pulls/31/files Bearer access-1"}
	if !slices.Equal(r.fg.calls, want) {
		t.Errorf("Forgejo calls %v, want the three pages as the human", r.fg.calls)
	}
	r.fg.calls = nil
	if none := drawer(t, r, ck, "t-run", "files"); !strings.Contains(none, `<p class="meta">no pull request yet</p>`) || len(r.fg.calls) != 0 {
		t.Errorf("a card without a pull request: calls %v\n%s", r.fg.calls, none)
	}
	// ten pages of fifty and no more: the first 500 files
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-big", Kind: "agent", Status: "in-review", Repository: "sf/code",
		Objective: "Vendor the generator", IssueNumber: 22, PRNumber: 32}})
	r.fg.calls = nil
	big := drawer(t, r, ck, "t-big", "files")
	if len(r.fg.calls) != 10 || strings.Count(big, "<tr><td><code>") != 500 || !strings.Contains(big, "<caption>The first 500 changed files of PR #32</caption>") ||
		!strings.Contains(big, "<code>gen/p10-49.go</code>") {
		t.Errorf("a pull request of more files: %d calls, %d rows", len(r.fg.calls), strings.Count(big, "<tr><td><code>"))
	}
}

// The drawer is its project's writers': 403 to anyone else, 404 for an
// unknown card, 400 for an unknown tab.
func TestDrawerRefusals(t *testing.T) {
	r, ck := drawerRig(t)
	for path, code := range map[string]int{"/cards/t-nope/drawer": http.StatusNotFound, "/cards/t-run/drawer?tab=chat": http.StatusBadRequest} {
		if rec := r.do(httptest.NewRequest(http.MethodGet, path, nil), ck); rec.Code != code {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, code)
		}
	}
	ak, _ := r.as(t, "alice")
	for _, tab := range []string{"overview", "activity", "files"} {
		if rec := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-run/drawer?tab="+tab, nil), ak); rec.Code != http.StatusForbidden {
			t.Errorf("alice, no writer of the code project, opens %s: %d, want 403", tab, rec.Code)
		}
	}
}

// A verdict goes back to the board it was given on, its filter kept, and
// never to another host.
func TestVerdictReturnsToTheFilteredBoard(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.alerts.rows = []factoryd.Alert{{ID: 3, Kind: factoryd.AlertEscalation, Source: "agent", TaskID: "t-e"},
		{ID: 6, Kind: factoryd.AlertEscalation, Source: "agent", TaskID: "t-e"}}
	for id, back := range map[string][2]string{"3": {"/?project=linkshort&view=wide", "/?project=linkshort&view=wide"}, "6": {"//evil.example/", "/alerts"}} {
		rec := r.do(post("/alerts/"+id+"/verdict", url.Values{"csrf": {s.csrf}, "verdict": {"real"}, "back": {back[0]}}), ck)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != back[1] {
			t.Errorf("verdict with back %q = %d to %q, want %q", back[0], rec.Code, rec.Header().Get("Location"), back[1])
		}
	}
}

// The approval cards' forms carry the board's filter and land on it: an
// approval of another project's merge goes back to that project's board.
func TestApprovalFormsKeepTheBoard(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	ck, s := r.loggedIn(t)
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 30, Kind: factoryd.ApprovalMerge, TaskID: "t-lm", Project: "linkshort/linkshort",
		PRNumber: 12, Status: "pending", Spec: json.RawMessage(`{"risk_class":"low","head_sha":"def456","review":{"verdict":"approve"}}`)})
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	start := strings.Index(body, `aria-labelledby="approval-30"`)
	card := body[start : start+strings.Index(body[start:], "</article>")]
	filter := `<input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value="linkshort"><input type="hidden" name="accent" value="">`
	if strings.Count(card, filter) != 2 {
		t.Errorf("the merge card's approve and deny forms do not both carry the filter:\n%s", card)
	}
	for _, tc := range []struct {
		path string
		form url.Values
		want string
	}{
		{"/approvals/30/approve", url.Values{"project": {"linkshort"}}, "/?project=linkshort"},
		{"/approvals/3/deny", url.Values{"project": {"ai-software-factory-code"}, "view": {"wide"}, "note": {"not now"}}, "/?project=ai-software-factory-code&view=wide"},
		{"/approvals/4/approve", url.Values{"project": {"ai-software-factory-code"}, "accent": {"teal"}}, "/?accent=teal&project=ai-software-factory-code"},
	} {
		tc.form.Set("csrf", s.csrf)
		if rec := r.do(post(tc.path, tc.form), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != tc.want {
			t.Errorf("POST %s = %d to %q, want 303 to %q: %s", tc.path, rec.Code, rec.Header().Get("Location"), tc.want, rec.Body)
		}
	}
}

// In Needs you a merge card shows its risk class, the model review's
// verdict, the agent's recommendation and its buttons, and folds the
// migration, the earlier reviews, the security audit and why a human
// under More; the wide view keeps them all in sight.
func TestNeedsYouFoldsTheMergeFacts(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	start := strings.Index(body, `aria-labelledby="approval-4"`)
	card := body[start : start+strings.Index(body[start:], "</article>")]
	shown, folded, ok := strings.Cut(card, `<details class="more"><summary>More</summary>`)
	if !ok {
		t.Fatalf("the merge card folds nothing:\n%s", card)
	}
	folded, after, _ := strings.Cut(folded, "</details>")
	shown += after
	for _, want := range []string{"<dt>Risk class</dt><dd>high</dd>", "<dt>Model review</dt><dd>approve: adds the column safely</dd>",
		`<dt>Agent's recommendation</dt><dd class="reason">Adds the column; merge it.</dd>`, `action="/approvals/4/approve"`, `action="/approvals/4/deny"`} {
		if !strings.Contains(shown, want) {
			t.Errorf("the merge card does not show %s:\n%s", want, shown)
		}
	}
	for _, want := range []string{"<dt>Migration</dt><dd>yes, human review required: <code>internal/factoryd/migrations/0018_x.sql</code></dd>",
		"<dt>Earlier review 1</dt><dd>concerns: the column lacks a default<br>- 0018_x.sql: add a default</dd>", "<dt>Security audit</dt>",
		"<dt>Why a human</dt><dd>the project&#39;s merge rule is human; risk class high is requires_approval</dd>"} {
		if !strings.Contains(folded, want) || strings.Contains(shown, want) {
			t.Errorf("the merge card does not fold %s", want)
		}
	}
}
