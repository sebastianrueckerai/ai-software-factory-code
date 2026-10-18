package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// columnItems are what column title holds, in order: "t-x" for a card's
// face, "approval 3" for an approval request, which shares its task's
// data-card with the face.
func columnItems(t *testing.T, body, title string) []string {
	t.Helper()
	var out []string
	// an escalation article shares its face's data-card, which the
	// face's place marks: drop the article's copy before the walk
	esc := regexp.MustCompile(`(<article class="card escalation" id="need-[^"]+") data-card="[^"]*"`)
	re := regexp.MustCompile(`<article class="card approval[^"]*" data-card="[^"]*" aria-labelledby="approval-(\d+)"|data-card="([^"]+)"`)
	for _, m := range re.FindAllStringSubmatch(esc.ReplaceAllString(col(t, body, title), "$1"), -1) {
		if m[1] != "" {
			out = append(out, "approval "+m[1])
		} else {
			out = append(out, m[2])
		}
	}
	return out
}

// screenRig is the code project with a card in every status, two pending
// approval requests and a card of another project; the final cards
// finished one, two and three minutes ago. Needs you holds, for
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
	esc.IssueNumber = 12 // an answer goes on the issue
	me := factoryd.BoardTask{Task: task("t-me", "waiting-poster", "ready")}
	me.Author, me.GateSince = "sebastian", ago(3*time.Hour)
	other := factoryd.BoardTask{Task: task("t-other", "waiting-poster", "ready")}
	other.Author, other.GateSince = "alice", ago(5*time.Hour)
	finished := func(id, status string, d time.Duration) factoryd.BoardTask {
		k := factoryd.BoardTask{Task: task(id, status, "ready")}
		k.CreatedAt, k.FinishedAt = now.Add(-time.Hour), ago(d)
		return k
	}
	r.tasks.tasks = []factoryd.BoardTask{esc, me, other,
		{Task: factoryd.Task{ID: "t-human", Kind: "human", Status: "queued", Repository: "sf/code", Objective: "rotate the key", CreatedAt: now.Add(-20 * time.Minute)}},
		{Task: task("t-back", "backlog", "backlog")}, {Task: task("t-queue", "queued", "ready")}, {Task: task("t-deps", "waiting-deps", "ready"), WaitingOn: "#9"},
		{Task: task("t-run", "running", "ready")}, {Task: task("t-close", "closing", "ready")}, {Task: task("t-rev", "in-review", "ready")},
		finished("t-done", "done", time.Minute), finished("t-fail", "failed", 2*time.Minute), finished("t-closed", "closed", 3*time.Minute),
		{Task: factoryd.Task{ID: "t-elsewhere", Kind: "agent", Status: "running", Repository: "demo-app/demo-app", Objective: "elsewhere", CreatedAt: now}},
	}
	r.approvals.rows[0].CreatedAt = now.Add(-time.Hour)
	r.approvals.rows[1].CreatedAt = now.Add(-4 * time.Hour)
	return r, ck
}

// Ruling 255 as C8 amends it: the four columns are a view over the task
// states. Every status lands in exactly one column, running, closing and
// in review together in In progress, waiting on the author in Needs you
// for the author and in Queued for anyone else; Needs you also holds the
// human lane (the approval requests, the escalation with its answer form,
// the human task), oldest waiting first; the headers count the column;
// another project's card is on no column.
func TestFourColumnsPlaceEveryStatus(t *testing.T) {
	r, ck := screenRig(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	want := map[string][]string{
		"Needs you":   {"approval 4", "t-me", "t-esc", "approval 3", "t-human"},
		"Queued":      {"t-other", "t-back", "t-queue", "t-deps"},
		"In progress": {"t-run", "t-close", "t-rev"},
		"Done":        {"t-done", "t-fail", "t-closed"},
	}
	counts := map[string]string{"Needs you": "5", "Queued": "4", "In progress": "3", "Done": "3"}
	for title, items := range want {
		if got := columnItems(t, body, title); !slices.Equal(got, items) {
			t.Errorf("%s holds %v, want %v", title, got, items)
		}
		// the head, then the menu on the three columns that have one (C13),
		// then the body that alone scrolls (never sideways)
		next := "<div class=\"cards\">"
		if title != "Needs you" {
			next = `<details class="col-menu">`
		}
		if !strings.Contains(body, ">"+title+` <span class="count">`+counts[title]+"</span></h3>\n"+next) || !strings.Contains(col(t, body, title), `<div class="cards">`) {
			t.Errorf("the header of %s lacks the count %s, its menu or its body", title, counts[title])
		}
		for _, id := range items {
			// an escalation article shares its face's data-card; the
			// face alone carries it with a place, which names one column
			if id[0] == 't' && strings.Count(body, `data-card="`+id+`" data-place="`) != 1 {
				t.Errorf("%s shows %d times, want once", id, strings.Count(body, `data-card="`+id+`" data-place="`))
			}
		}
	}
	if strings.Contains(body, "t-elsewhere") || strings.Contains(body, `id="human-lane"`) || strings.Contains(body, `data-lane=`) {
		t.Error("the five columns show another project's card, the human lane or a lane column")
	}
	needs := col(t, body, "Needs you")
	for _, want := range []string{`<section class="column lit" aria-labelledby="col-0" id="needs-you">`, `action="/tasks/t-esc/answer"`,
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

// Roadmap 244: a needs-you card shows the escalation's question and its
// state lines; the findings history the escalation carries for the issue
// is one pointer line, so the card stays a decision, not a transcript.
func TestNeedsYouCardShowsTheQuestionNotTheHistory(t *testing.T) {
	body := "the session cap is reached: every finding sent to the agent so far goes to a human\n" +
		"last: APIError: free headroom 0.000003 EUR, below the minimum call (status 402)\n" +
		"- (15 earlier findings left out)\n" +
		"- session, attempt 15: ran a grep in workflow_stages.go (exit 0)\n" +
		"- session, attempt 15: ran a python splice on attempt2.diff (exit 0)"
	want := "the session cap is reached: every finding sent to the agent so far goes to a human\n" +
		"last: APIError: free headroom 0.000003 EUR, below the minimum call (status 402)\n" +
		"- the findings history is on the issue"
	if got := cardReason(body); got != want {
		t.Errorf("cardReason = %q, want the question and state lines with the pointer", got)
	}
	// a body without bullets (an agent's ask) passes through whole, and a
	// bulletless body past the bound carries the rest pointer
	ask := factoryd.AgentAsks + "which region?\nWhy: the replica serves one.\nRecommendation: fsn1"
	if got := cardReason(ask); got != ask {
		t.Errorf("cardReason of an ask = %q, want it unchanged", got)
	}
	long := strings.Repeat("a", maxCardReason+500)
	if got := cardReason(long); !strings.HasPrefix(got, strings.Repeat("a", maxCardReason)) || !strings.HasSuffix(got, "- the rest is on the issue") {
		t.Error("cardReason of a long bulletless body lacks its cut at the bound and the rest pointer")
	}
	// the rendered card carries the compact reason, the lane card and the
	// drawer alike, and none of the history
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	now := time.Now()
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-cap", Kind: "agent", Status: "waiting-human",
		Lane: "ready", Repository: "sf/code", Objective: "the card", CreatedAt: now, IssueNumber: 13},
		Escalation: body, EscalatedAt: now.Add(-2 * time.Hour)}}
	page := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{"last: APIError: free headroom 0.000003 EUR, below the minimum call (status 402)",
		"- the findings history is on the issue"} {
		if !strings.Contains(page, want) {
			t.Errorf("the needs-you card lacks %q", want)
		}
	}
	if strings.Contains(page, "attempt 15") {
		t.Error("the needs-you card shows the findings history")
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
		start := strings.Index(body, `<div id="status-line"><div class="strip" role="group" aria-label="Status of the project">`)
		if start < 0 {
			t.Fatalf("board%s has no status strip", query)
		}
		return body[start : start+strings.Index(body[start:], `</div>
<div class="sp"`)]
	}
	const title = `title="spend this UTC month against the project's month cap"`
	gauge := func(pct int) string {
		return fmt.Sprintf(`<span class="gauge"><meter min="0" max="100" value="%d" aria-label="Spend of the month cap">%d percent</meter></span><span class="pct">%d%%</span>`, pct, pct, pct)
	}
	code := `<div id="status-line"><div class="strip" role="group" aria-label="Status of the project">
<span class="cell state" data-state="waiting on you"><i class="dot" aria-hidden="true"></i>waiting on you</span>
<span class="cell"><span><b class="n">2</b> working</span></span>
<a href="#needs-you" class="cell needs on"><span><b class="n">5</b> need you</span></a>
<a href="/costs" class="cell spend" ` + title + `><span><b class="n">€5.50</b> <span class="of">/ €50.00</span></span>` + gauge(11) + `</a>
`
	for _, query := range []string{"", "?view=wide"} {
		if got := line(query); got != code {
			t.Errorf("board%s: status line\n%s\nwant\n%s", query, got, code)
		}
	}
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-ask", Kind: "agent", Status: "waiting-poster", Author: "sebastian", Repository: "demo-app/demo-app"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-old", Kind: "agent", Status: "done", Repository: "linkshort/linkshort"}})
	r.tasks.tasks = slices.DeleteFunc(r.tasks.tasks, func(b factoryd.BoardTask) bool { return b.ID == "t-elsewhere" })
	if got, want := line("?project=demo-app"), `<div id="status-line"><div class="strip" role="group" aria-label="Status of the project">
<span class="cell state" data-state="waiting on you"><i class="dot" aria-hidden="true"></i>waiting on you</span>
<span class="cell"><span><b class="n">0</b> working</span></span>
<a href="#needs-you" class="cell needs on"><span><b class="n">1</b> need you</span></a>
<a href="/costs" class="cell spend" `+title+`><span><b class="n">€3.00</b> <span class="of">/ €12.00</span></span>`+gauge(25)+`</a>
`; got != want {
		t.Errorf("demo-app: status line\n%s\nwant\n%s", got, want)
	}
	if got, want := line("?project=linkshort"), `<div id="status-line"><div class="strip" role="group" aria-label="Status of the project">
<span class="cell state" data-state="idle"><i class="dot" aria-hidden="true"></i>idle</span>
<span class="cell"><span><b class="n">0</b> working</span></span>
<a href="#needs-you" class="cell needs"><span><b class="n">0</b> need you</span></a>
<a href="/costs" class="cell spend" `+title+`><span><b class="n">€0.00</b> <span class="of">/ €50.00</span></span>`+gauge(0)+`</a>
`; got != want {
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
	// her board follows the stream read-only and opens no drawer
	if !strings.Contains(body, `<div id="board" data-project="ai-software-factory-code" data-query="?project=ai-software-factory-code"`) ||
		!strings.Contains(body, `<strong class="title" title="fix the cache">fix the cache</strong>`) {
		t.Error("alice reading the code project does not follow its stream or links its cards to the drawer")
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
	// the chips line and the fine pills of the state row, per view
	lines := func(body, id string) (chips, fine string) {
		t.Helper()
		face := faceOf(t, body, id)
		if start := strings.Index(face, `<p class="chips">`); start >= 0 {
			chips = face[start : start+strings.Index(face[start:], "</p>")+len("</p>")]
		}
		for _, m := range regexp.MustCompile(`<span class="pill fine [^"]*">[^<]*</span>`).FindAllString(face, -1) {
			fine += m
		}
		return chips, fine
	}
	five := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	wide := r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String()
	// the default lane, ready, is no badge on the four-column board; the
	// wide view's state columns name it
	const ready = `<p class="chips"><span class="badge">ready</span> </p>`
	for id, want := range map[string][4]string{
		"t-lane":   {"", "", "", ""},
		"t-back":   {"", "", "", ""},
		"t-other":  {"", ready, `<span class="pill fine st-waiting hollow">waiting on the author</span>`, ""},
		"t-fail":   {"", "", "", ""},
		"t-closed": {"", "", "", ""},
		"t-done":   {"", "", "", ""},
		"t-stale":  {"", ready, `<span class="pill fine st-stale">stale</span>`, `<span class="pill fine st-stale">stale</span>`},
		"t-retry":  {"", ready, `<span class="pill fine st-running">retrying</span>`, `<span class="pill fine st-running">retrying</span>`},
		"t-again":  {"", ready, "", ""},
		"t-held":   {"", ready, `<span class="pill fine st-waiting">waiting for a roll</span>`, `<span class="pill fine st-waiting">waiting for a roll</span>`},
		"t-unheld": {"", ready, "", ""},
	} {
		if chips, fine := lines(five, id); chips != want[0] || fine != want[2] {
			t.Errorf("four columns, %s: chips %q, fine %q, want %q and %q", id, chips, fine, want[0], want[2])
		}
		if chips, fine := lines(wide, id); chips != want[1] || fine != want[3] {
			t.Errorf("wide view, %s: chips %q, fine %q, want %q and %q", id, chips, fine, want[1], want[3])
		}
	}
}

// A card whose newest session the model provider refused waits on it
// (ruling 267): its chip says so and the status line counts it as
// waiting, not working, so a project with no other card working idles;
// the next session_start makes it working again.
func TestCardWaitingOnTheProvider(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.approvals.rows = nil
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-wait", Kind: "agent", Status: "running", Lane: "ready", Repository: "sf/code", Objective: "t-wait", CreatedAt: time.Now()}}}
	refused := `{"step":"session_start","attempt":1}
{"step":"session_end","attempt":1,"outcome":"provider_refused"}
{"step":"provider_refused","status":429,"reason":"budget exhausted","backoff_s":120}
`
	r.objects["sessions/t-wait/timeline.jsonl"] = []byte(refused)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{`<span class="cell state" data-state="idle"><i class="dot" aria-hidden="true"></i>idle</span>
<span class="cell"><span><b class="n">0</b> working</span></span>
<span class="cell warn"><span><b class="n">1</b> waiting on the provider</span></span>
<a href="#needs-you" class="cell needs"><span><b class="n">0</b> need you</span></a>`, `<span class="pill fine st-waiting">waiting on the provider</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the board lacks %s", want)
		}
	}
	r.objects["sessions/t-wait/timeline.jsonl"] = []byte(refused + `{"step":"session_start","attempt":1}` + "\n")
	r.c.mu.Lock()
	r.c.checks = nil
	r.c.mu.Unlock()
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if want := `<span class="cell state" data-state="working"><i class="dot" aria-hidden="true"></i>working</span>
<span class="cell"><span><b class="n">1</b> working</span></span>
<a href="#needs-you"`; !strings.Contains(body, want) || strings.Contains(body, "waiting on the provider") {
		t.Errorf("after the session_start the board does not show the card working:\n%s", body)
	}
	// past its backoff the next session waits for a roll, not the provider
	r.objects["sessions/t-wait/timeline.jsonl"] = []byte(refused + `{"step":"held","roll":"t-roll"}` + "\n")
	r.c.mu.Lock()
	r.c.checks = nil
	r.c.mu.Unlock()
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if !strings.Contains(body, `<span class="pill fine st-waiting">waiting for a roll</span>`) || !strings.Contains(body, `<span class="cell"><span><b class="n">1</b> working</span></span>`) ||
		strings.Contains(body, "waiting on the provider") {
		t.Errorf("a held card after a refusal does not wait for the roll:\n%s", body)
	}
	// the sixth refusal asked a human: the card waits on them, not the provider
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-wait", Kind: "agent", Status: "waiting-human", Lane: "ready", Repository: "sf/code", Objective: "t-wait", PRNumber: 5, CreatedAt: time.Now()}}}
	r.objects["sessions/t-wait/timeline.jsonl"] = []byte(refused)
	r.c.mu.Lock()
	r.c.checks = nil
	r.c.mu.Unlock()
	if body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); strings.Contains(body, "waiting on the provider") {
		t.Errorf("a card waiting on a human shows the provider's wait:\n%s", body)
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
	for _, want := range []string{`<div id="board" data-project="linkshort" data-query="?project=linkshort&amp;view=wide">`,
		`<section aria-labelledby="human-lane" id="needs-you">`, `id="status-line"`,
		`<input type="hidden" name="project" value="linkshort"><input type="hidden" name="view" value="wide">`,
		`<a href="/?project=linkshort" id="view-toggle" class="view-toggle" data-view="" aria-keyshortcuts="w">Four columns</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the wide view lacks %s", want)
		}
	}
	if poll := r.do(httptest.NewRequest(http.MethodGet, "/board?project=linkshort&view=wide", nil), ck).Body.String(); !strings.Contains(poll, `data-lane="backlog"`) ||
		strings.Contains(poll, `class="lane five"`) {
		t.Error("the wide view's poll answers the five columns")
	}
	if frag := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-c2?project=linkshort&view=wide", nil), ck).Body.String(); !strings.Contains(frag,
		`<input type="hidden" name="project" value="linkshort"><input type="hidden" name="view" value="wide">`) {
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
		`<strong class="title" title="Fix the cache."><a href="#card=t-run" class="open">Fix the cache.</a></strong>`,
		`<button type="button" id="keys-toggle" class="keys-toggle" aria-haspopup="dialog" aria-controls="keys" aria-keyshortcuts="Shift+?" title="Keys" aria-label="Keys">?</button>`,
		`<div id="keys" class="overlay" role="dialog" aria-modal="true" aria-labelledby="keys-title" hidden>`,
		`<dt><kbd>w</kbd></dt><dd>the wide view, the board across the full width with the chat hidden, or back</dd>`,
		`<dt><kbd>Escape</kbd></dt><dd>close the card's drawer, the document, or this list</dd>`,
		`<dt><kbd>?</kbd></dt><dd>this list</dd>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the board page lacks %s", want)
		}
	}
	for path, wants := range map[string][]string{
		"/static/console.js": {`if (!document.getElementById("board") || e.ctrlKey || e.metaKey || e.altKey) return;`, `e.key === "Escape"`,
			`e.key === "w"`, `e.key === "?"`, `window.addEventListener("hashchange", showDrawer);`, `"/drawer"`,
			`if (el.querySelector("details[open]")) return true;`},
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

// drawerTabMarkup is the drawer's tab name of card id, selected when on.
func drawerTabMarkup(id, name, title string, on bool) string {
	return `<button type="button" role="tab" aria-selected="` + strconv.FormatBool(on) + `" aria-controls="drawer-panel" hx-get="/cards/` + id +
		`/drawer?tab=` + name + `" hx-target="#drawer" hx-swap="innerHTML">` + title + `</button>`
}

// The drawer's Overview: the four tabs, the objective as its headline, a
// planned card's provenance line and its body, the related cards in their
// live state, each opening in the drawer (blocked by the dependencies not
// done, part of plan; a plan's children), the plan's table and a pending
// merge's reviews and audit.
func TestDrawerOverview(t *testing.T) {
	r, ck := drawerRig(t)
	open := func(id, label string) string {
		return `<a href="#card=` + id + `" hx-get="/cards/` + id + `/drawer" hx-target="#drawer" hx-swap="innerHTML">` + label + `</a></li>`
	}
	tab := drawerTabMarkup
	card := drawer(t, r, ck, "t-c4", "")
	for _, want := range []string{
		`<div class="drawer-content" data-drawer="t-c4">`,
		`<div class="dr-title"><a class="dr-ref" href="https://git.example/linkshort/linkshort/issues/4" title="the issue">#4</a><h2 tabindex="-1">k4</h2></div>`,
		`<nav class="tabs" role="tablist" aria-label="The card's tabs">` + tab("t-c4", "overview", "Overview", true) +
			tab("t-c4", "chat", "Session", false) + tab("t-c4", "activity", "Activity", false) + tab("t-c4", "files", "Files", false) + `</nav>`,
		`<dt>Task</dt><dd>Links API</dd>`, `<details class="sec" data-sec="time"><summary>Time<span class="hint">`,
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
	for _, want := range []string{`<dt>Task</dt><dd>Link shortener</dd>`,
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
		"<dd>approve: adds the column safely<br>- remark: a follow-up files the docs<br>- dropped (grep finds the client closed in handler.go:40): internal/api.go: the handler leaks the client</dd>",
		"<dt>Earlier review 1</dt><dd>concerns: the column lacks a default<br>- 0018_x.sql: add a default<br>- dropped (main already held the default): 0018_x.sql: add a default</dd>",
		"<dt>Security audit</dt><dd>findings (coding-frontier, zai/glm-5.3)<br>- secret in the diff: private key: <code>internal/x/x_test.go:14</code>"} {
		if !strings.Contains(merge, want) {
			t.Errorf("t-m's overview lacks %s", want)
		}
	}
	if strings.Contains(drawer(t, r, ck, "t-pr", ""), `data-sec="reviews"`) {
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
		`<tr><td>session</td><td>1</td><td><time>2026-10-11 10:00:00</time></td><td>5 min</td><td>pushed</td><td><meter min="0" max="300" value="300">5 min</meter></td></tr>`,
		`<tr><td title="2026-10-11T10:05:00Z"><time>2026-10-11 10:05:00</time></td><td>session_end</td><td></td><td><code>{&#34;attempt&#34;:1,&#34;outcome&#34;:&#34;pushed&#34;,&#34;started&#34;:&#34;2026-10-11T10:00:00Z&#34;}</code></td></tr>`,
	} {
		if !strings.Contains(activity, want) {
			t.Errorf("the activity lacks %s", want)
		}
	}
	// the <time> cells keep to one line only through the stylesheet's
	// nowrap rule; the wrapper and the rule stand together
	css := r.do(httptest.NewRequest(http.MethodGet, "/static/console.css", nil)).Body.String()
	if !strings.Contains(css, "td time { white-space: nowrap; }") {
		t.Error("console.css lacks td time { white-space: nowrap; }")
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

// The drawer's Chat: a running card opens on it, with its newest
// session's agent text and the steers sent within that session in time
// order, each keyed as the stream's copy is, and the input that steers it;
// a card whose session ended opens on Overview and shows them read-only;
// a card without a session says so.
func TestDrawerChat(t *testing.T) {
	r, ck := drawerRig(t)
	r.c.cfg.Steer = func(context.Context, string, string, string) error { return nil }
	r.objects["sessions/t-run/timeline.jsonl"] = []byte(`{"time":"2026-10-11T09:00:00Z","step":"session_start","session":"s-1","model":"coding-standard","attempt":1}
{"time":"2026-10-11T09:20:00Z","step":"session_end","session":"s-1","outcome":"failed"}
{"time":"2026-10-11T10:00:00Z","step":"session_start","session":"s-2","model":"coding-frontier","attempt":2}
`)
	r.objects["sessions/t-run/s-2.jsonl"] = []byte(`{"time":"2026-10-11T10:01:00.5Z","kind":"model_call","detail":{"text":"Reading the cache first.","cost":0.01}}
{"time":"2026-10-11T10:01:30Z","kind":"tool_call","detail":{"tool":"read"}}
{"time":"2026-10-11T10:02:00Z","kind":"model_call","detail":{"cost":0.01}}
{"time":"2026-10-11T10:04:00Z","kind":"model_call","detail":{"text":"Done <b>here</b>.\nNext: the tests."}}
`)
	r.objects["sessions/t-pr/timeline.jsonl"] = []byte(`{"time":"2026-10-11T08:00:00Z","step":"session_start","session":"s-9","model":"coding-cheap","attempt":1}
{"time":"2026-10-11T08:30:00Z","step":"session_end","session":"s-9","outcome":"pushed"}
`)
	r.objects["sessions/t-pr/s-9.jsonl"] = []byte(`{"time":"2026-10-11T08:10:00Z","kind":"model_call","detail":{"text":"Drawing it."}}
`)
	at := func(hhmm string) time.Time {
		v, _ := time.Parse(time.RFC3339, "2026-10-11T"+hhmm+":00Z")
		return v
	}
	for _, e := range []factoryd.Event{
		{TaskID: "t-run", At: at("09:10"), Kind: factoryd.EventCardSteered, Actor: "alice", Detail: map[string]any{"text": "to the first session"}},
		{TaskID: "t-run", At: at("10:03"), Kind: factoryd.EventCardSteered, Actor: "sebastian", Detail: map[string]any{"text": "Rebase first."}},
		{TaskID: "t-run", At: at("10:03"), Kind: factoryd.EventCardMoved, Actor: "sebastian"},
		{TaskID: "t-pr", At: at("08:20"), Kind: factoryd.EventCardSteered, Actor: "sebastian", Detail: map[string]any{"text": "Use the grid."}},
		{TaskID: "t-pr", At: at("08:31"), Kind: factoryd.EventCardSteered, Actor: "sebastian", Detail: map[string]any{"text": "too late"}},
	} {
		e.Repository, e.Via = "sf/code", factoryd.ViaButton
		if _, err := r.events.Record(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	running := drawer(t, r, ck, "t-run", "")
	for _, want := range []string{
		drawerTabMarkup("t-run", "overview", "Overview", false) + drawerTabMarkup("t-run", "chat", "Session", true) +
			drawerTabMarkup("t-run", "activity", "Activity", false) + drawerTabMarkup("t-run", "files", "Files", false) + `</nav>`,
		`<div id="drawer-panel" class="drawer-body" role="tabpanel">
<p class="meta">Session 2 · coding-frontier · running</p>
<div id="session-log" class="session-log" role="log" aria-label="The messages of session 2" data-task="t-run" data-session="s-2">
<div class="msg agent" data-key="s-2@2026-10-11T10:01:00.5Z"><span class="av" aria-hidden="true"></span><p class="who"><span class="name">agent</span> <time>10-11 10:01Z</time></p><p class="said">Reading the cache first.</p></div>
<div class="msg you" data-key="e2"><span class="av" aria-hidden="true">S</span><p class="who"><span class="name">sebastian</span> <time>10-11 10:03Z</time></p><p class="said">Rebase first.</p></div>
<div class="msg agent" data-key="s-2@2026-10-11T10:04:00Z"><span class="av" aria-hidden="true"></span><p class="who"><span class="name">agent</span> <time>10-11 10:04Z</time></p><p class="said">Done &lt;b&gt;here&lt;/b&gt;.
Next: the tests.</p></div>
</div>
<form id="steer-form" class="chat-form composer" hx-post="/tasks/t-run/steer" hx-target="#drawer" hx-swap="innerHTML">
<div class="prompt"><span class="ps" aria-hidden="true">›</span><label for="steer-input" class="visually-hidden">Your message to the running session</label>
<textarea id="steer-input" name="text" rows="2" maxlength="8192" required placeholder="Message the running session; it reads it at its next step"></textarea>
<button type="submit">Send</button></div>
</form>
</div>`,
	} {
		if !strings.Contains(running, want) {
			t.Errorf("t-run's drawer lacks\n%s\nin\n%s", want, running)
		}
	}
	if ended := drawer(t, r, ck, "t-pr", ""); !strings.Contains(ended, drawerTabMarkup("t-pr", "overview", "Overview", true)) {
		t.Errorf("a card that runs no session does not open on Overview:\n%s", ended)
	}
	if got, want := drawer(t, r, ck, "t-pr", "chat"), `<p class="meta">Session 1 · coding-cheap · pushed</p>
<div id="session-log" class="session-log" role="log" aria-label="The messages of session 1" data-task="t-pr" data-session="s-9">
<div class="msg agent" data-key="s-9@2026-10-11T08:10:00Z"><span class="av" aria-hidden="true"></span><p class="who"><span class="name">agent</span> <time>10-11 08:10Z</time></p><p class="said">Drawing it.</p></div>
<div class="msg you" data-key="e4"><span class="av" aria-hidden="true">S</span><p class="who"><span class="name">sebastian</span> <time>10-11 08:20Z</time></p><p class="said">Use the grid.</p></div>
</div>
</div>`; !strings.Contains(got, want) {
		t.Errorf("the ended session's chat lacks\n%s\nin\n%s", want, got)
	}
	if got := drawer(t, r, ck, "t-c4", "chat"); !strings.Contains(got, `<p class="meta">No session yet</p>`) || strings.Contains(got, "session-log") {
		t.Errorf("a card without a session:\n%s", got)
	}
}

// A steer from the drawer is the member's own act, so no proposal: the
// steer function at once, its row with the text, and the Chat tab again
// with the message in it and the steer's note; a GitHub token in it
// reaches neither the session, the row, the stream nor the answer; a
// card that runs no session refuses it and records nothing.
func TestDrawerSteer(t *testing.T) {
	r, _ := drawerRig(t)
	var steered []string
	r.c.cfg.Steer = func(_ context.Context, taskID, user, text string) error {
		steered = append(steered, taskID+" "+user+": "+text)
		return nil
	}
	r.objects["sessions/t-run/timeline.jsonl"] = []byte(`{"time":"2020-01-01T00:00:00Z","step":"session_start","session":"s-1","model":"coding-frontier","attempt":1}
`)
	ck, s := r.loggedIn(t)
	before := r.events.count()
	rec := r.do(post("/tasks/t-run/steer", url.Values{"csrf": {s.csrf}, "text": {" Rebase on main first. "}}), ck)
	rows := r.events.all()[before:]
	if rec.Code != http.StatusOK || len(rows) != 1 {
		t.Fatalf("POST steer = %d, %d rows: %s", rec.Code, len(rows), rec.Body)
	}
	row := rows[0]
	if want := (factoryd.Event{ID: row.ID, At: row.At, Repository: "sf/code", TaskID: "t-run", IssueNumber: 11, Kind: factoryd.EventCardSteered,
		Actor: "sebastian", Via: factoryd.ViaButton, Detail: map[string]any{"text": "Rebase on main first."}}); !reflect.DeepEqual(row, want) {
		t.Errorf("the steer's row = %+v, want %+v", row, want)
	}
	if !slices.Equal(steered, []string{"t-run sebastian: Rebase on main first."}) {
		t.Errorf("steered %q", steered)
	}
	for _, want := range []string{drawerTabMarkup("t-run", "chat", "Session", true),
		`<div id="drawer-panel" class="drawer-body" role="tabpanel">
<p class="note" role="status">The session of #11 has your message.</p>`,
		`<div class="msg you" data-key="e` + strconv.FormatInt(row.ID, 10) + `"><span class="av" aria-hidden="true">S</span><p class="who"><span class="name">sebastian</span> <time>` + row.At.UTC().Format("01-02 15:04") +
			`Z</time></p><p class="said">Rebase on main first.</p></div>`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the steer's answer lacks %s:\n%s", want, rec.Body)
		}
	}

	const token = "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	_, br := streamOf(t, r, "")
	rec = r.do(post("/tasks/t-run/steer", url.Values{"csrf": {s.csrf}, "text": {"Push with " + token + " now."}}), ck)
	all := r.events.all()
	row = all[len(all)-1]
	if rec.Code != http.StatusOK || row.Detail["text"] != "Push with [token] now." || steered[1] != "t-run sebastian: Push with [token] now." {
		t.Errorf("a steer with a token = %d, row %v, steered %q", rec.Code, row.Detail, steered)
	}
	if got := strings.Join(nextMessage(t, br), "\n"); !strings.Contains(got, `"detail":{"text":"Push with [token] now."}`) || strings.Contains(got, token) {
		t.Errorf("the stream carries %s", got)
	}
	if body := rec.Body.String(); strings.Contains(body, token) || !strings.Contains(body, `<p class="said">Push with [token] now.</p>`) ||
		!strings.Contains(body, `<p class="note" role="status">The session of #11 has your message. I removed a GitHub token from it; it was not stored or sent.</p>`) {
		t.Errorf("the answer to a steer with a token:\n%s", body)
	}
	before = r.events.count()
	if rec := r.do(post("/tasks/t-pr/steer", url.Values{"csrf": {s.csrf}, "text": {"hello"}}), ck); rec.Code != http.StatusConflict ||
		len(steered) != 2 || r.events.count() != before {
		t.Errorf("a steer to a card in review = %d, steered %q, %d rows: %s", rec.Code, steered, r.events.count()-before, rec.Body)
	}
}

// The drawer and its steer are its project's writers': 403 to anyone
// else and to a steer without the form token, 404 for an unknown card,
// 400 for an unknown tab.
func TestDrawerRefusals(t *testing.T) {
	r, ck := drawerRig(t)
	steered := 0
	r.c.cfg.Steer = func(context.Context, string, string, string) error { steered++; return nil }
	for path, code := range map[string]int{"/cards/t-nope/drawer": http.StatusNotFound, "/cards/t-run/drawer?tab=steps": http.StatusBadRequest} {
		if rec := r.do(httptest.NewRequest(http.MethodGet, path, nil), ck); rec.Code != code {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, code)
		}
	}
	ak, _ := r.as(t, "alice")
	for _, tab := range []string{"overview", "activity", "files", "chat"} {
		if rec := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-run/drawer?tab="+tab, nil), ak); rec.Code != http.StatusForbidden {
			t.Errorf("alice, no writer of the code project, opens %s: %d, want 403", tab, rec.Code)
		}
	}
	before := r.events.count()
	ak, as := r.as(t, "alice")
	if rec := r.do(post("/tasks/t-run/steer", url.Values{"csrf": {as.csrf}, "text": {"hello"}}), ak); rec.Code != http.StatusForbidden {
		t.Errorf("alice steers t-run: %d, want 403", rec.Code)
	}
	if rec := r.do(post("/tasks/t-run/steer", url.Values{"text": {"hello"}}), ck); rec.Code != http.StatusForbidden {
		t.Errorf("a steer without the form token: %d, want 403", rec.Code)
	}
	if steered != 0 || r.events.count() != before {
		t.Errorf("refused steers reached the session %d times and left %d rows", steered, r.events.count()-before)
	}
}

// A verdict goes back to the board it was given on, its filter kept, and
// never to another host.
func TestVerdictReturnsToTheFilteredBoard(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.alerts.rows = []factoryd.Alert{{ID: 3, Kind: factoryd.AlertEscalation, Source: "agent", TaskID: "t-e"},
		{ID: 6, Kind: factoryd.AlertEscalation, Source: "agent", TaskID: "t-e"}}
	for id, back := range map[string][2]string{"3": {"/?project=linkshort&view=wide", "/?project=linkshort&view=wide"}, "6": {"//evil.example/", "/"}} {
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
	filter := `<input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value="linkshort">`
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
		{"/approvals/4/approve", url.Values{"project": {"ai-software-factory-code"}}, "/?project=ai-software-factory-code"},
	} {
		tc.form.Set("csrf", s.csrf)
		if rec := r.do(post(tc.path, tc.form), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != tc.want {
			t.Errorf("POST %s = %d to %q, want 303 to %q: %s", tc.path, rec.Code, rec.Header().Get("Location"), tc.want, rec.Body)
		}
	}
}

// The merge item on the four-column board stays small, like a
// question's face: the headline with the risk and migration chips, the
// Approve and Deny controls and the foot links. The five-part ask —
// why a human, the model review's verdict and summary, the
// recommendation — and the facts wall (the migration files, the
// earlier reviews, the audit) live in the drawer, which the item's
// Details link opens (item 4).
func TestNeedsYouMergeItemIsBrief(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 9}})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	start := strings.Index(body, `aria-labelledby="approval-4"`)
	card := body[start : start+strings.Index(body[start:], "</article>")]
	for _, want := range []string{`<span class="chip risk">high risk</span> <span class="chip migration">migration</span>`,
		`action="/approvals/4/approve"`, `<button type="button" class="deny" aria-expanded="false" aria-controls="deny-4">Deny</button>`, `<form method="post" action="/approvals/4/deny" class="deny-box" id="deny-4" hidden>`, `<a href="#card=t-m:reviews" class="details">Details</a>`} {
		if !strings.Contains(card, want) {
			t.Errorf("the merge item lacks %s:\n%s", want, card)
		}
	}
	for _, gone := range []string{`<p class="why">the project&#39;s merge rule is human; risk class high is requires_approval</p>`, `<p class="info"><b>Model review</b> approve: adds the column safely</p>`, `<p class="recommendation">Recommendation: Adds the column; merge it.</p>`, "0018_x.sql", "Earlier review", "Security audit", "Why a human", "<details", "<dt>"} {
		if strings.Contains(card, gone) {
			t.Errorf("the merge item shows %s, which the drawer holds:\n%s", gone, card)
		}
	}
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-m/drawer?tab=overview", nil), ck).Body.String()
	for _, want := range []string{`<p class="why">the project&#39;s merge rule is human; risk class high is requires_approval</p>`, `<p class="info"><b>Model review</b> approve: adds the column safely</p>`, `<p class="recommendation">Recommendation: Adds the column; merge it.</p>`} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the merge drawer lacks %s", want)
		}
	}
}

// The 2026-10-17 ruling: a needs-you card is compact on the board and
// opens its task's drawer by its title, exactly like a face card — a
// door only for the one who writes the project, whose drawer it is.
func TestNeedsYouCardOpensItsDrawer(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	lane := col(t, r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String(), "Approvals")
	for _, want := range []string{`data-card="t-res"`, `<a href="#card=t-res" class="open">Resource request</a>`,
		`data-card="t-m"`, `<a href="#card=t-m" class="open">Merge</a>`} {
		if !strings.Contains(lane, want) {
			t.Errorf("a writer's approval column lacks %q:\n%s", want, lane)
		}
	}
	ak, _ := r.as(t, "alice") // alice reads every board but writes none
	lane = col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ak).Body.String(), "Approvals")
	if strings.Contains(lane, `class="open"`) || !strings.Contains(lane, ">Resource request</h4>") || !strings.Contains(lane, "Merge <a href=") {
		t.Errorf("a reader's approval column links to a drawer that would refuse:\n%s", lane)
	}
}

// The four columns hold their cards by arrival, the newest on top
// (Sebastian, 2026-10-14): Queued by filing, In progress by admission,
// Done by finishing, whatever order the store gave them; a card without
// the time keeps its filing time, and the store's order holds among
// equals. The wide view keeps the store's order, where the fake, as
// the store since #762, ranks the running cards by rank and id.
func TestColumnsHoldTheNewestCardOnTop(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	now := time.Now()
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	task := func(id, status string, filed time.Duration) factoryd.Task {
		return factoryd.Task{ID: id, Kind: "agent", Status: status, Lane: "ready", Repository: "sf/code", Objective: id, CreatedAt: now.Add(-filed)}
	}
	run := func(id string, filed, admitted time.Duration) factoryd.Task {
		k := task(id, "running", filed)
		k.AdmittedAt = ago(admitted)
		return k
	}
	done := func(id, status string, filed, finished time.Duration) factoryd.Task {
		k := task(id, status, filed)
		if finished > 0 {
			k.FinishedAt = ago(finished)
		}
		return k
	}
	// the store's order: Queued oldest first, In progress and Done as ranked
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: task("t-q-old", "queued", 3*time.Hour)}, {Task: task("t-q-new", "queued", time.Hour)}, {Task: task("t-q-mid", "waiting-deps", 2*time.Hour), WaitingOn: "#9"},
		{Task: run("t-r-first", 6*time.Hour, 5*time.Hour)}, {Task: run("t-r-last", 4*time.Hour, 10*time.Minute)}, {Task: run("t-r-mid", 5*time.Hour, time.Hour)},
		{Task: done("t-d-old", "done", 9*time.Hour, 2*time.Hour)}, {Task: done("t-d-new", "done", 8*time.Hour, 10*time.Minute)},
		{Task: done("t-f-none", "failed", 90*time.Minute, 0)}, {Task: done("t-d-mid", "closed", 7*time.Hour, time.Hour)},
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	want := map[string][]string{
		"Queued":      {"t-q-new", "t-q-mid", "t-q-old"},
		"In progress": {"t-r-last", "t-r-mid", "t-r-first"},
		"Done":        {"t-d-new", "t-d-mid", "t-f-none", "t-d-old"},
	}
	for title, items := range want {
		if got := columnItems(t, body, title); !slices.Equal(got, items) {
			t.Errorf("%s holds %v, want %v", title, got, items)
		}
	}
	// the wide view keeps the store's order, which since #762 puts the
	// newest finished first (the rig's fake sorts as the store does)
	wide := r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String()
	if got := columnItems(t, wide, "Running"); !slices.Equal(got, []string{"t-r-first", "t-r-last", "t-r-mid"}) {
		t.Errorf("the wide view's Running holds %v, want the store's order", got)
	}
}

// The find (C13) narrows every column to its terms and the head reads
// "n of m": a word against what the face says (the ref, the title, the
// question), is:, status: and by: against the facts, every term matching;
// an emptied column says "none match" and the Needs you column keeps its
// amber (lit) from its whole count. The forms, the board's query and the
// clear link carry q, and the status panel lists the whole project.
func TestFindNarrowsTheColumns(t *testing.T) {
	r, ck := screenRig(t)
	page := func(q string) string {
		return r.do(httptest.NewRequest(http.MethodGet, "/?q="+url.QueryEscape(q), nil), ck).Body.String()
	}
	for _, c := range []struct {
		q, title string
		items    []string
		count    string
	}{
		{"t-run", "In progress", []string{"t-run"}, "<b>1</b> of 3"},
		{"t-run", "Queued", nil, "<b>0</b> of 4"},
		{"t-run", "Needs you", nil, "<b>0</b> of 5"},
		{"is:failed", "Done", []string{"t-fail"}, "<b>1</b> of 3"},
		{"is:blocked", "Queued", []string{"t-deps"}, "<b>1</b> of 4"},
		{"status:queued", "Queued", []string{"t-queue"}, "<b>1</b> of 4"},
		{"by:alice", "Queued", []string{"t-other"}, "<b>1</b> of 4"},
		{"which region", "Needs you", []string{"t-esc"}, "<b>1</b> of 5"},
		{"rotate key", "Needs you", []string{"t-human"}, "<b>1</b> of 5"},
		{"nothing-here", "Done", nil, "<b>0</b> of 3"},
	} {
		body := page(c.q)
		if got := columnItems(t, body, c.title); !slices.Equal(got, c.items) {
			t.Errorf("q=%q: %s holds %v, want %v", c.q, c.title, got, c.items)
		}
		if !strings.Contains(body, ">"+c.title+` <span class="count">`+c.count+"</span></h3>") {
			t.Errorf("q=%q: the header of %s does not read %s", c.q, c.title, c.count)
		}
	}
	body := page("t-run")
	for _, want := range []string{`<section class="column lit" aria-labelledby="col-0" id="needs-you">`,
		`data-query="?project=ai-software-factory-code&amp;q=t-run"`, findForm,
		`<input id="q" name="q" type="text" value="t-run" placeholder="Find: a word, #612, plan:558"`,
		`<a class="find-x" href="/?project=ai-software-factory-code" aria-label="Clear the find"`} {
		if !strings.Contains(body, want) {
			t.Errorf("under the find the board lacks %s", want)
		}
	}
	if n := strings.Count(body, `<p class="none-match">none match</p>`); n != 3 {
		t.Errorf("%d columns say none match, want 3", n)
	}
	panel := body[strings.Index(body, "<section><h3>Needs you</h3>"):]
	panel = panel[:strings.Index(panel, "</section>")]
	if n := strings.Count(panel, `class="sp-link"`); n != 5 {
		t.Errorf("the status panel lists %d Needs you items under the find, want the whole 5", n)
	}
	asked := page("which region")
	for _, want := range []string{`<input type="hidden" name="q" value="which region">`, `name="back" value="/?project=ai-software-factory-code&amp;q=which&#43;region"`} {
		if !strings.Contains(asked, want) {
			t.Errorf("the escalation's forms lack %s", want)
		}
	}
	// without a find the × is in place for the box's text to show it (CSS)
	plain := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{findForm, `<input id="q" name="q" type="text" value="" placeholder="Find: a word, #612, plan:558"`,
		`<a class="find-x" href="/?project=ai-software-factory-code" aria-label="Clear the find"`, `>Needs you <span class="count">5</span></h3>`} {
		if !strings.Contains(plain, want) {
			t.Errorf("without a find the board lacks %s", want)
		}
	}
	for _, none := range []string{"none match", " of "} {
		if strings.Contains(col(t, plain, "Done"), none) {
			t.Errorf("without a find the board shows %q", none)
		}
	}
}

// findForm is the find's form: as you type it swaps the board around
// itself; console.js writes its query into the URL.
const findForm = `<form class="find" id="find" role="search" method="get" action="/" hx-get="/" hx-trigger="input changed delay:250ms" hx-target="#board" hx-select="#board" hx-swap="outerHTML" hx-preserve>`

// The filter's query carries the find, trimmed, beside the project and
// the view.
func TestFilterQueryCarriesTheFind(t *testing.T) {
	if got := (boardFilter{Project: "p", View: "wide", Q: "a b"}).query(); got != "?project=p&q=a+b&view=wide" {
		t.Errorf("query %q", got)
	}
	if got := urlFilter(httptest.NewRequest(http.MethodGet, "/?q=%20x%20", nil)); got.Q != "x" {
		t.Errorf("urlFilter trims to %q", got.Q)
	}
	if got := (boardFilter{Project: "p", Queued: colFilter{"oldest", "blocked"}, Done: colFilter{Only: "failed"}}).query(); got != "?done-only=failed&project=p&queued=oldest&queued-only=blocked" {
		t.Errorf("query with the menus %q", got)
	}
	if got := urlFilter(httptest.NewRequest(http.MethodGet, "/?queued=sideways&queued-only=plan:x&done-only=failed", nil)); got.Queued != (colFilter{}) || got.Done != (colFilter{Only: "failed"}) {
		t.Errorf("urlFilter reads unknown choices as %+v %+v", got.Queued, got.Done)
	}
}

// The column menus (C13): Queued, In progress and Done take their order
// and their show from the query, arrival newest first and every card
// otherwise; the head says the non-default choices under it with a way
// back and marks its ▾; the forms keep the choices; a value a menu does
// not offer reads as the default; the show narrows under the find's
// whole count.
func TestColumnMenusOrderAndShow(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	now := time.Now()
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	task := func(id, status, lane string, filed time.Duration) factoryd.Task {
		return factoryd.Task{ID: id, Kind: "agent", Status: status, Lane: lane, Repository: "sf/code", Objective: id, CreatedAt: now.Add(-filed), BudgetEUR: 2}
	}
	plan := task("t-plan", "done", "plan", 10*time.Hour)
	plan.IssueNumber, plan.FinishedAt = 7, ago(9*time.Hour)
	qb := task("t-q-b", "queued", "ready", 3*time.Hour) // from plan #7
	qb.Parent, qb.Source = "t-plan", factoryd.SourcePlan
	qc := task("t-q-c", "waiting-deps", "ready", 2*time.Hour)
	qa := task("t-q-a", "queued", "ready", 30*time.Minute)
	// build and planning filed inside the stale window, so with no event
	// they wear no ring; verify is stale and near its budget
	build := task("t-build", "running", "ready", 25*time.Minute)
	build.AdmittedAt, build.SpentEUR = ago(20*time.Minute), 0.5
	verify := task("t-verify", "in-review", "ready", 4*time.Hour)
	verify.AdmittedAt, verify.SpentEUR = ago(time.Hour), 1.9
	planning := task("t-planning", "running", "plan", 28*time.Minute)
	planning.AdmittedAt, planning.SpentEUR = ago(24*time.Minute), 1
	done := task("t-done", "done", "ready", 8*time.Hour)
	done.FinishedAt = ago(time.Hour)
	fail := task("t-fail", "failed", "ready", 7*time.Hour)
	fail.FinishedAt = ago(30 * time.Minute)
	r.tasks.tasks = []factoryd.BoardTask{{Task: qa}, {Task: qb}, {Task: qc, WaitingOn: "#9"}, {Task: build}, {Task: verify}, {Task: planning}, {Task: plan}, {Task: done}, {Task: fail}}
	page := func(query string) string {
		return r.do(httptest.NewRequest(http.MethodGet, "/"+query, nil), ck).Body.String()
	}
	for _, c := range []struct {
		query, title string
		items        []string
		count        string
	}{
		{"", "Queued", []string{"t-q-a", "t-q-c", "t-q-b"}, "3"},
		{"?queued=oldest", "Queued", []string{"t-q-b", "t-q-c", "t-q-a"}, "3"},
		{"?queued=next", "Queued", []string{"t-q-a", "t-q-b", "t-q-c"}, "3"},
		{"?queued-only=blocked", "Queued", []string{"t-q-c"}, "<b>1</b> of 3"},
		{"?queued-only=plan:7", "Queued", []string{"t-q-b"}, "<b>1</b> of 3"},
		{"", "In progress", []string{"t-build", "t-planning", "t-verify"}, "3"},
		{"?progress=oldest", "In progress", []string{"t-verify", "t-planning", "t-build"}, "3"},
		{"?progress=stage", "In progress", []string{"t-planning", "t-build", "t-verify"}, "3"},
		{"?progress=spend", "In progress", []string{"t-verify", "t-planning", "t-build"}, "3"},
		{"?progress-only=exceptional", "In progress", []string{"t-verify"}, "<b>1</b> of 3"},
		{"", "Done", []string{"t-fail", "t-done", "t-plan"}, "3"},
		{"?done=oldest", "Done", []string{"t-plan", "t-done", "t-fail"}, "3"},
		{"?done-only=failed", "Done", []string{"t-fail"}, "<b>1</b> of 3"},
		{"?done=sideways&done-only=nothing", "Done", []string{"t-fail", "t-done", "t-plan"}, "3"},
		{"?q=t-q&queued-only=blocked", "Queued", []string{"t-q-c"}, "<b>1</b> of 3"},
	} {
		body := page(c.query)
		if got := columnItems(t, body, c.title); !slices.Equal(got, c.items) {
			t.Errorf("%s: %s holds %v, want %v", c.query, c.title, got, c.items)
		}
		if !strings.Contains(body, ">"+c.title+` <span class="count">`+c.count+"</span></h3>") {
			t.Errorf("%s: the header of %s does not read %s", c.query, c.title, c.count)
		}
	}
	body := page("?queued=oldest&queued-only=blocked")
	for _, want := range []string{
		`<p class="col-state"><span>oldest first · blocked only</span><a class="col-x" href="/?project=ai-software-factory-code" aria-label="Back to newest first and all"`,
		`<summary class="on" aria-label="Order and show, Queued: oldest first · blocked only"`,
		`<input type="radio" name="queued" value="oldest" checked><span>oldest first</span>`,
		`<input type="radio" name="queued-only" value="blocked" checked><span>blocked only</span>`,
		`<input type="radio" name="queued-only" value="plan:7"><span>from plan #7</span>`,
		`data-query="?project=ai-software-factory-code&amp;queued=oldest&amp;queued-only=blocked"`,
		// the find's form keeps the menus' choices, a menu's form the rest of the filter
		findForm + `<input type="hidden" name="project" value="ai-software-factory-code"><input type="hidden" name="queued" value="oldest"><input type="hidden" name="queued-only" value="blocked"><label`,
		`<form method="get" action="/" class="menu-pop auto-submit"><input type="hidden" name="project" value="ai-software-factory-code"><input type="hidden" name="queued" value="oldest"><input type="hidden" name="queued-only" value="blocked"><fieldset><legend>Order</legend><label><input type="radio" name="done" value="" checked>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("under the Queued choices the board lacks %s", want)
		}
	}
	if n := strings.Count(body, `<p class="col-state">`); n != 1 {
		t.Errorf("%d state lines, want 1", n)
	}
	// a choice applies at once: the menu's form has no button to press
	for _, menu := range regexp.MustCompile(`(?s)<form method="get" action="/" class="menu-pop auto-submit">.*?</form>`).FindAllString(body, -1) {
		if strings.Contains(menu, "<button") {
			t.Errorf("a column menu holds a button: %s", menu)
		}
	}
	plain := page("")
	if strings.Contains(plain, "col-state") || strings.Contains(plain, `<summary class="on"`) || strings.Count(plain, `<details class="col-menu">`) != 3 || strings.Contains(col(t, plain, "Needs you"), "col-menu") {
		t.Error("by default the board shows a state line, a marked menu, not three menus, or one on Needs you")
	}
}

// The Done column reads its finished cards a page at a time, past the
// 300 one read once bounded the board to: the newest 50 of the week,
// then the form of the next page, which htmx asks for
// once the viewer scrolls to it and its button asks for by keyboard, or
// without script asks for the board showing 50 more. A page starts after
// the card the one before ended with, ties on the stamp by id, so a card
// finishing meanwhile repeats none; the head counts them all and no line
// says the board stops. A find or a show reads them all; the wide view
// pages each final status.
func TestDonePagesItsFinishedCards(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	now := time.Now()
	// t-d000 to t-d329 finish in threes a minute apart, t-d000 newest:
	// t-d048, t-d049 and t-d050 share a stamp across the first page's end
	fin := func(i int) *time.Time { at := now.Add(-time.Duration(i/3) * time.Minute); return &at }
	task := func(id, status string, finished *time.Time) factoryd.BoardTask {
		return factoryd.BoardTask{Task: factoryd.Task{ID: id, Kind: "agent", Status: status, Lane: "ready", Repository: "sf/code", Objective: id,
			CreatedAt: now.Add(-3 * time.Hour), FinishedAt: finished}}
	}
	ids := func(n, m int) []string {
		var out []string
		for i := n; i < m; i++ {
			out = append(out, fmt.Sprintf("t-d%03d", i))
		}
		return out
	}
	for i := range 330 {
		r.tasks.tasks = append(r.tasks.tasks, task(fmt.Sprintf("t-d%03d", i), "done", fin(i)))
	}
	old := now.AddDate(0, 0, -8)
	elsewhere := task("t-elsewhere", "done", fin(0))
	elsewhere.Repository = "demo-app/demo-app"
	r.tasks.tasks = append(r.tasks.tasks, task("t-f", "failed", fin(600)), task("t-old", "done", &old), elsewhere, task("t-run", "running", nil))
	get := func(target string) (int, string) {
		rec := r.do(httptest.NewRequest(http.MethodGet, target, nil), ck)
		return rec.Code, rec.Body.String()
	}
	form := func(i, shown int) string {
		return `<form class="more" method="get" action="/" hx-get="/board/finished" hx-trigger="submit, intersect once" hx-swap="outerHTML">` +
			`<input type="hidden" name="project" value="ai-software-factory-code"><input type="hidden" name="col" value="done">` +
			`<input type="hidden" name="after" value="` + fin(i).UTC().Format(time.RFC3339Nano) + `"><input type="hidden" name="after-id" value="` + fmt.Sprintf("t-d%03d", i) + `">` +
			`<input type="hidden" name="done-shown" value="` + strconv.Itoa(shown) + `"><button type="submit" id="more-done" aria-label="Load more of Done">Load more</button></form>`
	}
	page := func(after, shown int) string {
		return fmt.Sprintf("/board/finished?project=ai-software-factory-code&col=done&after=%s&after-id=t-d%03d&done-shown=%d",
			url.QueryEscape(fin(after).UTC().Format(time.RFC3339Nano)), after, shown)
	}
	cards := func(body string) []string {
		var out []string
		for _, m := range regexp.MustCompile(`data-card="([^"]+)"`).FindAllStringSubmatch(body, -1) {
			out = append(out, m[1])
		}
		return out
	}

	_, body := get("/")
	if got := columnItems(t, body, "Done"); !slices.Equal(got, ids(0, 50)) {
		t.Fatalf("Done holds %v, want t-d000 to t-d049", got)
	}
	if p := regexp.MustCompile(`<p class="problem"[^>]*>[^<]*`).FindAllString(body, -1); len(p) > 0 {
		t.Errorf("the board says %q", p)
	}
	for _, want := range []string{`data-finished="done"><h3 id="col-3">Done <span class="count">331</span></h3>`, form(49, 100) + "</div>"} {
		if !strings.Contains(body, want) {
			t.Errorf("the board lacks %s", want)
		}
	}

	// t-new finishes while the viewer reads; the next page goes on after t-d049
	r.tasks.mu.Lock()
	later := now.Add(time.Second)
	r.tasks.tasks = append(r.tasks.tasks, task("t-new", "done", &later))
	r.tasks.mu.Unlock()
	code, body := get(page(49, 100))
	if code != http.StatusOK || !slices.Equal(cards(body), ids(50, 100)) || !strings.HasPrefix(body, `<article class="card face final d-done" data-card="t-d050"`) ||
		!strings.HasSuffix(body, form(99, 150)) {
		t.Fatalf("the page after t-d049 = %d %v, want the cards t-d050 to t-d099 and the form after t-d099:\n%s", code, cards(body), body)
	}
	if _, body = get(page(299, 350)); !slices.Equal(cards(body), append(ids(300, 330), "t-f")) || strings.Contains(body, `class="more"`) {
		t.Fatalf("the last page = %v, want t-d300 to t-d329 and t-f without a form", cards(body))
	}

	// the board asked to show 100 (a reload, a swap, the button without script)
	_, body = get("/?done-shown=100")
	if got := columnItems(t, body, "Done"); !slices.Equal(got, append([]string{"t-new"}, ids(0, 99)...)) || !strings.Contains(body, form(98, 150)) ||
		!strings.Contains(body, `Done <span class="count">332</span>`) {
		t.Errorf("Done showing 100 holds %v, want t-new and t-d000 to t-d098, the form after t-d098 and the count 332", got)
	}
	for _, c := range []struct{ query, count string }{{"?q=t-f", "<b>1</b> of 332"}, {"?done-only=failed", "<b>1</b> of 332"}} {
		_, body = get("/" + c.query)
		if got := columnItems(t, body, "Done"); !slices.Equal(got, []string{"t-f"}) || strings.Contains(body, `class="more"`) ||
			!strings.Contains(body, `Done <span class="count">`+c.count+`</span>`) {
			t.Errorf("%s: Done holds %v, want t-f of all 332 and no form", c.query, got)
		}
	}
	if _, body = get("/?done=oldest"); !slices.Equal(columnItems(t, body, "Done")[:5], []string{"t-f", "t-d327", "t-d328", "t-d329", "t-d324"}) ||
		len(columnItems(t, body, "Done")) != 50 {
		t.Errorf("Done oldest first holds %v, want t-f, t-d327 to t-d329, then t-d324, 50 in all", columnItems(t, body, "Done"))
	}

	// the wide view: Done pages its own, Failed and Closed hold theirs
	_, body = get("/?view=wide")
	if got := columnItems(t, body, "Done"); !slices.Equal(got, append([]string{"t-new"}, ids(0, 49)...)) ||
		!strings.Contains(body, `data-finished="failed"><h3 id="col-`) || !slices.Equal(columnItems(t, body, "Failed"), []string{"t-f"}) ||
		strings.Contains(col(t, body, "Failed"), `class="more"`) || !strings.Contains(col(t, body, "Done"), `<input type="hidden" name="view" value="wide">`) {
		t.Errorf("the wide view's Done holds %v and Failed %v, want 50 with the form and t-f alone", got, columnItems(t, body, "Failed"))
	}

	for _, target := range []string{
		"/board/finished?col=done",                                                        // after no card
		"/board/finished?col=done&after-id=t-d049",                                        // no arrival
		"/board/finished?col=done&after=yesterday&after-id=t-d049",                        // no time
		"/board/finished?col=done&after=2026-10-15T00:00:00Z&after-id=t-d049&q=t-f",       // a find reads them all
		"/board/finished?col=failed&after=2026-10-15T00:00:00Z&after-id=t-d049",           // the four columns' Done holds failed
		"/board/finished?col=done&after=2026-10-15T00:00:00Z&after-id=t-d049&view=narrow", // no such view
	} {
		if code, body := get(target); code != http.StatusBadRequest {
			t.Errorf("%s = %d %q, want 400", target, code, body)
		}
	}
}

// countingTasks wraps the task store and counts the whole board's reads.
type countingTasks struct {
	*fakeTasks
	mu    sync.Mutex
	reads map[string]int
}

func (s *countingTasks) note(what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reads == nil {
		s.reads = map[string]int{}
	}
	s.reads[what]++
}

func (s *countingTasks) BoardTasks(ctx context.Context, repository string) ([]factoryd.BoardTask, error) {
	s.note("BoardTasks")
	return s.fakeTasks.BoardTasks(ctx, repository)
}

func (s *countingTasks) FinishedTasks(ctx context.Context, q factoryd.FinishedQuery) ([]factoryd.BoardTask, int, error) {
	s.note("FinishedTasks")
	return s.fakeTasks.FinishedTasks(ctx, q)
}

// countingApprovals wraps the approval requests.
type countingApprovals struct {
	*fakeApprovals
	mu    sync.Mutex
	reads int
}

func (a *countingApprovals) ApprovalRequests(ctx context.Context, status string) ([]factoryd.ApprovalRequest, error) {
	a.mu.Lock()
	a.reads++
	a.mu.Unlock()
	return a.fakeApprovals.ApprovalRequests(ctx, status)
}

// A page of finished cards reads only its column: the column's page after
// the cursor, not the open cards or the approval requests a whole board
// read takes (roadmap 270).
func TestFinishedPageReadsItsColumnAlone(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	now, at := time.Now(), time.Now().Add(-time.Hour)
	r.tasks.mu.Lock()
	for i := range 60 {
		r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{
			ID: fmt.Sprintf("t-d%02d", i), Kind: "agent", Status: "done", Lane: "ready", Repository: "sf/code",
			Objective: "a done card", CreatedAt: now.Add(-2 * time.Hour), FinishedAt: &at}})
	}
	r.tasks.mu.Unlock()
	tasks, approvals := &countingTasks{fakeTasks: r.tasks}, &countingApprovals{fakeApprovals: r.approvals}
	cfg := r.c.cfg
	cfg.Tasks, cfg.Approvals = tasks, approvals
	page, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	get := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.AddCookie(ck)
		rec := httptest.NewRecorder()
		page.ServeHTTP(rec, req)
		return rec
	}
	rec := get("/board/finished?project=ai-software-factory-code&col=done&after=" +
		url.QueryEscape(at.UTC().Format(time.RFC3339Nano)) + "&after-id=t-d00")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `data-card="t-d50"`) {
		t.Fatalf("the page = %d %s", rec.Code, rec.Body.String())
	}
	if n := tasks.reads["BoardTasks"]; n != 0 {
		t.Errorf("the page read the open cards %d time(s)", n)
	}
	if approvals.reads != 0 {
		t.Errorf("the page read the approval requests %d time(s)", approvals.reads)
	}
	if n := tasks.reads["FinishedTasks"]; n != 1 {
		t.Errorf("the page read its column %d time(s), want 1", n)
	}
	// the whole board still reads all of it
	if rec := get("/?project=ai-software-factory-code"); rec.Code != http.StatusOK {
		t.Fatalf("the board = %d", rec.Code)
	}
	if tasks.reads["BoardTasks"] != 1 || approvals.reads != 1 {
		t.Errorf("the board read the open cards %d and the approvals %d time(s), want 1 each",
			tasks.reads["BoardTasks"], approvals.reads)
	}
}
