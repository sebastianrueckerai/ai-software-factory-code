package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"golang.org/x/net/html"
)

// columnItems are the cards of the column titled title, in order: each
// small card's data-card, an approval request's its task's.
func columnItems(t *testing.T, body, title string) []string {
	t.Helper()
	var out []string
	for _, m := range regexp.MustCompile(`data-card="([^"]+)"`).FindAllStringSubmatch(col(t, body, title), -1) {
		out = append(out, m[1])
	}
	return out
}

// screenRig is the code project with a card in every status, two pending
// approval requests and a card of another project; the final cards
// finished one, two and three minutes ago. Needs you holds, for every
// viewer, oldest waiting first: t-other waiting on alice as its author
// (5 h), approval 4 (4 h), t-me waiting on sebastian (3 h), the escalated
// t-esc (2 h), approval 3 (1 h) and the human task t-human (20 min).
func screenRig(t *testing.T) (*rig, *http.Cookie) {
	t.Helper()
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	now := time.Now()
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	task := func(id, status, track string) factoryd.Task {
		return factoryd.Task{ID: id, Kind: "agent", Status: status, Track: track, Repository: "sf/code", Objective: id, CreatedAt: now}
	}
	esc := factoryd.BoardTask{Task: task("t-esc", "waiting-human", "direct"), Escalation: factoryd.AgentAsks + "which region?",
		EscalatedAt: now.Add(-2 * time.Hour), AskCase: 7}
	esc.IssueNumber = 12 // an answer goes on the issue
	me := factoryd.BoardTask{Task: task("t-me", "waiting-poster", "direct")}
	me.Author, me.GateSince = "sebastian", ago(3*time.Hour)
	other := factoryd.BoardTask{Task: task("t-other", "waiting-poster", "direct")}
	other.Author, other.GateSince = "alice", ago(5*time.Hour)
	finished := func(id, status string, d time.Duration) factoryd.BoardTask {
		k := factoryd.BoardTask{Task: task(id, status, "direct")}
		k.CreatedAt, k.FinishedAt = now.Add(-time.Hour), ago(d)
		return k
	}
	r.tasks.tasks = []factoryd.BoardTask{esc, me, other,
		{Task: factoryd.Task{ID: "t-human", Kind: "human", Status: "queued", Repository: "sf/code", Objective: "rotate the key", CreatedAt: now.Add(-20 * time.Minute)}},
		{Task: task("t-queue", "queued", "direct")}, {Task: task("t-deps", "waiting-deps", "direct"), WaitingOn: "#9"},
		{Task: task("t-run", "running", "direct")}, {Task: task("t-close", "closing", "direct")}, {Task: task("t-rev", "in-review", "direct")},
		finished("t-done", "done", time.Minute), finished("t-fail", "failed", 2*time.Minute), finished("t-closed", "closed", 3*time.Minute),
		{Task: factoryd.Task{ID: "t-elsewhere", Kind: "agent", Status: "running", Repository: "demo-app/demo-app", Objective: "elsewhere", CreatedAt: now}},
	}
	r.approvals.rows[0].CreatedAt = now.Add(-time.Hour)
	r.approvals.rows[1].CreatedAt = now.Add(-4 * time.Hour)
	return r, ck
}

// As C8 amends it: the four columns are a view over the task
// states. Every status lands in exactly one column, running, closing and
// in review together in In progress, waiting on the author in Needs you
// for every viewer, the author or not (roadmap 225: a person is the next
// actor); Needs you also holds the human lane (the approval requests, the escalation,
// the human task), oldest waiting first, their forms on their opened cards; the headers count the column;
// another project's card is on no column.
func TestFourColumnsPlaceEveryStatus(t *testing.T) {
	r, ck := screenRig(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	want := map[string][]string{
		"Needs you":   {"t-other", "t-m", "t-me", "t-esc", "t-res", "t-human"},
		"Queued":      {"t-queue", "t-deps"},
		"In progress": {"t-run", "t-close", "t-rev"},
		"Done":        {"t-done", "t-fail", "t-closed"},
	}
	counts := map[string]string{"Needs you": "6", "Queued": "2", "In progress": "3", "Done": "3"}
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
			// a card's face carries a place, which names one column; an
			// approval request's face, t-m's and t-res's, carries none
			if id != "t-m" && id != "t-res" && strings.Count(body, `data-card="`+id+`" data-place="`) != 1 {
				t.Errorf("%s shows %d times, want once", id, strings.Count(body, `data-card="`+id+`" data-place="`))
			}
		}
	}
	if strings.Contains(body, "t-elsewhere") || strings.Contains(body, `id="human-lane"`) || strings.Contains(body, `data-lane=`) {
		t.Error("the five columns show another project's card, the human lane or a lane column")
	}
	needs := col(t, body, "Needs you")
	if want := `<section class="column lit" aria-labelledby="col-0" id="needs-you" data-holds="waiting-human waiting-poster">`; !strings.Contains(body, want) {
		t.Errorf("the board lacks %s", want)
	}
	// the one ask here that suggests an answer is the merge its model
	// review approved: Needs you holds that press alone, the other answers
	// are the opened cards'
	if strings.Count(needs, "<form") != 1 || !strings.Contains(needs, `action="/approvals/4/approve"`) {
		t.Errorf("Needs you holds a form other than the merge's press:\n%s", needs)
	}
	if !strings.Contains(drawer(t, r, ck, "t-esc", "overview"), `action="/tasks/t-esc/resolve"`) {
		t.Error("t-esc's opened card lacks its answer form")
	}
	// the place names the column, so a card that changes column swaps the
	// board
	for _, id := range []string{"t-me", "t-other"} {
		if !strings.Contains(body, `data-card="`+id+`" data-place="waiting-poster direct  false false 0 0"`) {
			t.Errorf("%s's place does not name Needs you", id)
		}
	}

	r.repoWriters["sf/code"] = append(r.repoWriters["sf/code"], "alice")
	r.forgetWriters()
	ak, _ := r.as(t, "alice")
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), ak).Body.String()
	// alice is not t-me's author, and t-me still needs a person
	for title, items := range map[string][]string{
		"Needs you": {"t-other", "t-m", "t-me", "t-esc", "t-res", "t-human"},
		"Queued":    {"t-queue", "t-deps"},
	} {
		if got := columnItems(t, body, title); !slices.Equal(got, items) {
			t.Errorf("alice: %s holds %v, want %v", title, got, items)
		}
	}
	if !strings.Contains(body, `data-card="t-me" data-place="waiting-poster direct  false false 0 0"`) {
		t.Error("alice: t-me's place does not name Needs you")
	}
}

// Roadmap 244: a needs-you card shows the escalation's question and its
// state lines; the findings history the escalation carries for the issue
// is one pointer line, so the card stays a decision, not a transcript.
func TestNeedsYouCardShowsTheQuestionNotTheHistory(t *testing.T) {
	body := "the session cap is reached: every finding sent to the agent so far goes to a human\n" +
		"last: APIError: free headroom 0.00 EUR, below the minimum call (status 402)\n" +
		"- (15 earlier findings left out)\n" +
		"- session, attempt 15: ran a grep in workflow_stages.go (exit 0)\n" +
		"- session, attempt 15: ran a python splice on attempt2.diff (exit 0)"
	want := "the session cap is reached: every finding sent to the agent so far goes to a human\n" +
		"last: APIError: free headroom 0.00 EUR, below the minimum call (status 402)\n" +
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
		Track: "direct", Repository: "sf/code", Objective: "the card", CreatedAt: now, IssueNumber: 13},
		Escalation: body, EscalatedAt: now.Add(-2 * time.Hour)}}
	page := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-cap/drawer?tab=overview", nil), ck).Body.String()
	for _, want := range []string{"last: APIError: free headroom 0.00 EUR, below the minimum call (status 402)",
		"- the findings history is on the issue"} {
		if !strings.Contains(page, want) {
			t.Errorf("the drawer lacks %q", want)
		}
	}
	if strings.Contains(page, "attempt 15") {
		t.Error("the drawer shows the findings history")
	}
	if board := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); strings.Contains(board, "last: APIError") {
		t.Error("the case face repeats the raw reason: the ask shows once")
	}
}

// The status line: the project, its state word, n working, n need you
// (the Needs you column, whatever the accent) and the month spend against
// the month cap: the enforced month spend of the whole factory under the
// policy's cap, the project's own spend under its file's
// budget.month_eur, another project's rows left out there.
func TestStatusLineFigures(t *testing.T) {
	r, ck := screenRig(t)
	withLinkshort(r)
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	r.chat.spend = []spendRow{{"demo-app/demo-app", month, 3, false}}
	r.ledger.enforced = policy.Enforced{Month: 5.5}
	r.ledger.monthCaps = map[string]float64{"demo-app": 12}
	line := func(query string) string {
		t.Helper()
		body := r.do(httptest.NewRequest(http.MethodGet, "/"+query, nil), ck).Body.String()
		start := strings.Index(body, `<details id="status-line" class="status" aria-label="Status"><summary class="strip">`)
		if start < 0 {
			t.Fatalf("board%s has no status strip", query)
		}
		return body[start : start+strings.Index(body[start:], `</summary>
<div class="sp"`)]
	}
	const title = `title="spend this UTC month against the month cap: the project file's when it sets one, else the factory's, holds included"`
	gauge := func(pct int) string {
		return fmt.Sprintf(`<span class="gauge"><meter min="0" max="100" value="%d" aria-label="Spend of the month cap">%d percent</meter></span>`, pct, pct)
	}
	pool := `<span class="cell pool" title="the escalation pool every extend and fund draws from, set in the policy tree (escalation.pool), policy version 4 (commit c9d98bb), ` +
		now.Format("2006-01") + ` UTC: the same figures the chat's status line reads">pool: ` + noDraws + "</span>\n"
	code := `<details id="status-line" class="status" aria-label="Status"><summary class="strip">
<span class="cell state" data-state="waiting on you"><i class="dot" aria-hidden="true"></i>waiting on you</span>
<span class="cell"><span><b class="n">2</b> working</span></span>
<a href="#needs-you" class="cell needs on"><span><b class="n">6</b> need you</span></a>
<span class="cell spend" ` + title + `><a href="/costs"><span><b class="n">5.50 EUR</b> <span class="of">/ 50.00 EUR</span></span>` + gauge(11) + `</a><span class="pct">11%</span></span>
` + pool
	if got := line(""); got != code {
		t.Errorf("board: status line\n%s\nwant\n%s", got, code)
	}
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-ask", Kind: "agent", Status: "waiting-poster", Author: "sebastian", Repository: "demo-app/demo-app"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-old", Kind: "agent", Status: "done", Repository: "linkshort/linkshort"}})
	r.tasks.tasks = slices.DeleteFunc(r.tasks.tasks, func(b factoryd.BoardTask) bool { return b.ID == "t-elsewhere" })
	if got, want := line("?project=demo-app"), `<details id="status-line" class="status" aria-label="Status"><summary class="strip">
<span class="cell state" data-state="waiting on you"><i class="dot" aria-hidden="true"></i>waiting on you</span>
<span class="cell"><span><b class="n">0</b> working</span></span>
<a href="#needs-you" class="cell needs on"><span><b class="n">1</b> need you</span></a>
<span class="cell spend" `+title+`><a href="/costs"><span><b class="n">3.00 EUR</b> <span class="of">/ 12.00 EUR</span></span>`+gauge(25)+`</a><span class="pct">25%</span></span>
`+pool; got != want {
		t.Errorf("demo-app: status line\n%s\nwant\n%s", got, want)
	}
	if got, want := line("?project=linkshort"), `<details id="status-line" class="status" aria-label="Status"><summary class="strip">
<span class="cell state" data-state="idle"><i class="dot" aria-hidden="true"></i>idle</span>
<span class="cell"><span><b class="n">0</b> working</span></span>
<a href="#needs-you" class="cell needs"><span><b class="n">0</b> need you</span></a>
<span class="cell spend" `+title+`><a href="/costs"><span><b class="n">5.50 EUR</b> <span class="of">/ 50.00 EUR</span></span>`+gauge(11)+`</a><span class="pct">11%</span></span>
`+pool; got != want {
		t.Errorf("linkshort: status line\n%s\nwant\n%s", got, want)
	}
}

// The dependencies whisper (roadmap 299): no cell while every row is up,
// a red cell when one is down, an amber one when one is degraded, and
// the panel behind the strip always carries the list, "unknown" when
// the prober has written nothing.
func TestStatusLineDependencies(t *testing.T) {
	r, ck := screenRig(t)
	line := func() string {
		t.Helper()
		body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
		start := strings.Index(body, `<details id="status-line" class="status" aria-label="Status"><summary class="strip">`)
		if start < 0 {
			t.Fatal("board has no status strip")
		}
		return body[start : start+strings.Index(body[start:], `</summary>
<div class="sp"`)]
	}
	const down = `<span class="cell deps down" data-tip="a factory dependency is down; the status in detail names it"><i class="dot" aria-hidden="true"></i>dependencies</span>`
	const warn = `<span class="cell deps warn" data-tip="a factory dependency is slow; the status in detail names it"><i class="dot" aria-hidden="true"></i>dependencies</span>`
	const up = `<span class="cell deps up" data-tip="every factory dependency answers; the status in detail lists them"><i class="dot" aria-hidden="true"></i>dependencies</span>`
	if got := line(); strings.Contains(got, "deps") {
		t.Fatalf("no rows, strip holds a deps cell:\n%s", got)
	}
	r.deps.rows = []factoryd.DependencyState{
		{Name: "object storage", State: "up", CheckedAt: time.Now()},
		{Name: "forgejo", State: "down", Detail: "the api refused", CheckedAt: time.Now()},
	}
	if got := line(); !strings.Contains(got, down) || strings.Contains(got, warn) {
		t.Fatalf("one down row:\n%s\nwant the red cell only", got)
	}
	r.deps.rows = []factoryd.DependencyState{{Name: "postgres", State: "degraded", Detail: "answered in 3.4 s", CheckedAt: time.Now()}}
	if got := line(); !strings.Contains(got, warn) || strings.Contains(got, down) {
		t.Fatalf("one degraded row:\n%s\nwant the amber cell only", got)
	}
	r.deps.rows = []factoryd.DependencyState{{Name: "temporal", State: "up", CheckedAt: time.Now()}}
	if got := line(); !strings.Contains(got, up) || strings.Contains(got, down) || strings.Contains(got, warn) {
		t.Fatalf("every row up:\n%s\nwant the green cell only", got)
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{`<h3>Factory</h3>`, `data-tip="up · under a minute ago"><i class="dot" aria-hidden="true"></i>temporal</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the panel lacks %q", want)
		}
	}
	r.deps.rows = nil
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if !strings.Contains(body, `<p class="none">unknown</p>`) {
		t.Error("no rows, the panel does not say unknown")
	}
	r.deps.err = errors.New("the store is gone")
	if got := line(); strings.Contains(got, "deps") {
		t.Fatalf("a read error, strip holds a deps cell:\n%s", got)
	}
}

// The status line is one thin strip; the panel with the spend detail,
// the provider and the configuration sits behind the strip's click: a
// closed details whose summary is the strip. C20's "renders always,
// without an expand control" line misrecorded the standing want.
func TestStatusLineStripExpandsThePanel(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	i := strings.Index(body, `<details id="status-line" class="status" aria-label="Status"><summary class="strip">`)
	if i < 0 {
		t.Fatal("the status line is not a closed details with the strip as its summary")
	}
	if strings.Contains(body, `<section class="status"`) || strings.Contains(body, `<div class="strip"`) {
		t.Error("the old always-open status markup is still rendered")
	}
	rest := body[i:]
	summaryEnd := strings.Index(rest, `</summary>`)
	sp := strings.Index(rest, `<div class="sp" id="status-panel"`)
	if summaryEnd < 0 || sp < 0 || sp < summaryEnd {
		t.Fatal("the detail panel does not sit inside the details after the summary")
	}
	if j := strings.Index(rest[sp:], `</details>`); j < 0 {
		t.Error("the details does not close after the panel")
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
	// alice writes linkshort alone and reads the code project's repository
	r.repoWriters["linkshort/linkshort"] = []string{"sebastian", "alice"}
	r.c.cfg.HTTP = &http.Client{Transport: readerForgejo{http.DefaultTransport}}
	ak, _ := memberLogin(t, r, "alice")
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), ak).Body.String()
	if !strings.Contains(body, `<select id="project-filter" name="project">
<option value="linkshort" selected>linkshort</option>
</select>`) || !strings.Contains(body, `data-card="t-c2"`) {
		t.Error("alice's board is not linkshort's alone")
	}
	body = r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ak).Body.String()
	if !strings.Contains(body, `<option value="linkshort">linkshort</option>
<option value="ai-software-factory-code" selected>ai-software-factory-code</option>
</select>`) || !strings.Contains(body, `data-card="t-run"`) || strings.Contains(body, `/tasks/t-run/brief"`) {
		t.Error("alice reading the code project: the select lacks it, or the board lacks its card or offers her its forms")
	}
	// her board follows the stream read-only and opens no drawer
	if face := faceOf(t, body, "t-run"); !strings.Contains(body, `<div id="board" data-project="ai-software-factory-code" data-query="?project=ai-software-factory-code"`) ||
		!strings.Contains(face, `<strong class="title">fix the cache</strong>`) || strings.Contains(face, `class="open"`) {
		t.Errorf("alice reading the code project does not follow its stream, or a card offers her its drawer:\n%s", face)
	}
	// bob writes the infra project alone, which agents do not work on
	r.repoWriters["sf/infra"] = []string{"bob"}
	bk, _ := memberLogin(t, r, "bob")
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), bk).Body.String()
	if !strings.Contains(body, `<p class="problem" role="alert">You write none of the projects agents work on.</p>`) ||
		!strings.Contains(body, `data-project=""`) || strings.Contains(body, "status-line") || strings.Contains(body, "data-card") {
		t.Error("bob, who writes no project, does not see why his board is empty")
	}
}

// The fine state is a mark in the need's text: waiting on the author,
// retrying (the newest session ended for a retry), stale (no event for
// half an hour), in a small card's foot or a Needs-you card's band; failed
// and closed are the state's own words.
func TestCardFineChips(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	now := time.Now()
	task := func(id, status, track string) factoryd.BoardTask {
		return factoryd.BoardTask{Task: factoryd.Task{ID: id, Kind: "agent", Status: status, Track: track, Repository: "sf/code", Objective: id, CreatedAt: now}}
	}
	other, stale := task("t-other", "waiting-poster", "direct"), task("t-stale", "running", "direct")
	other.Author, stale.CreatedAt = "alice", now.Add(-50*time.Minute)
	r.tasks.tasks = []factoryd.BoardTask{task("t-lane", "queued", "direct"), other, task("t-fail", "failed", "direct"),
		task("t-closed", "closed", ""), task("t-done", "done", "direct"), stale, task("t-retry", "running", "direct"), task("t-again", "running", "direct")}
	r.objects["sessions/t-retry/timeline.jsonl"] = []byte(`{"step":"session_start","attempt":1}
{"step":"session_end","attempt":1,"outcome":"retry_stronger"}
`)
	r.objects["sessions/t-again/timeline.jsonl"] = []byte(`{"step":"session_start","attempt":1}
{"step":"session_end","attempt":1,"outcome":"retry_stronger"}
{"step":"session_start","attempt":2}
`)
	// the fine marks: a small card's foot items, a Needs-you card's band items
	marks := func(body, id string) string {
		t.Helper()
		return strings.Join(regexp.MustCompile(`<span class="(?:it )?flag">[^<]*</span>`).FindAllString(faceOf(t, body, id), -1), "")
	}
	five := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for id, want := range map[string]string{
		"t-lane":   "",
		"t-other":  `<span class="flag">waiting on the author</span>`,
		"t-fail":   "",
		"t-closed": "",
		"t-done":   "",
		"t-stale":  `<span class="it flag">stale</span>`,
		"t-retry":  `<span class="it flag">retrying</span>`,
		"t-again":  "",
	} {
		if got := marks(five, id); got != want {
			t.Errorf("%s: marks %q, want %q", id, got, want)
		}
	}
}

// A card whose newest session the model provider refused waits on it:
// its chip says so and the status line counts it as
// waiting, not working, so a project with no other card working idles;
// the next session_start makes it working again.
func TestCardWaitingOnTheProvider(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.approvals.rows = nil
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-wait", Kind: "agent", Status: "running", Track: "direct", Repository: "sf/code", Objective: "t-wait", CreatedAt: time.Now()}}}
	refused := `{"step":"session_start","attempt":1}
{"step":"session_end","attempt":1,"outcome":"provider_refused"}
{"step":"provider_refused","status":429,"reason":"budget exhausted","backoff_s":120}
`
	r.objects["sessions/t-wait/timeline.jsonl"] = []byte(refused)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{`<span class="cell state" data-state="idle"><i class="dot" aria-hidden="true"></i>idle</span>
<span class="cell"><span><b class="n">0</b> working</span></span>
<span class="cell warn"><span><b class="n">1</b> waiting on the provider</span></span>
<a href="#needs-you" class="cell needs"><span><b class="n">0</b> need you</span></a>`, `<span class="it flag">waiting on the provider</span>`} {
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
	// the sixth refusal asked a human: the card waits on them, not the provider
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-wait", Kind: "agent", Status: "waiting-human", Track: "direct", Repository: "sf/code", Objective: "t-wait", PRNumber: 5, CreatedAt: time.Now()}}}
	r.objects["sessions/t-wait/timeline.jsonl"] = []byte(refused)
	r.c.mu.Lock()
	r.c.checks = nil
	r.c.mu.Unlock()
	if body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); strings.Contains(body, "waiting on the provider") {
		t.Errorf("a card waiting on a human shows the provider's wait:\n%s", body)
	}
}

// The page holds the drawer, empty and hidden, every card's open link
// opens it by #card=<id>, and the ? list names the keys Escape and ?; the
// script binds them on the board only, never with a modifier, and the
// head's script paints the theme chosen and moves the page nowhere.
func TestKeysAndDrawerOnThePage(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "Fix the cache.", IssueNumber: 11})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{`<aside id="drawer" class="drawer" aria-label="The card" hidden></aside>`,
		`<a class="open" href="#card=t-run"><span class="visually-hidden">Open #11</span></a>` + "\n" + `<p class="para"><strong class="title">Fix the cache.</strong></p>`,
		`<button type="button" id="keys-toggle" class="keys-toggle" aria-haspopup="dialog" aria-controls="keys" aria-keyshortcuts="Shift+?" title="Keys" aria-label="Keys">?</button>`,
		`<div id="keys" class="overlay" role="dialog" aria-modal="true" aria-labelledby="keys-title" hidden>`,
		`<dt><kbd>Escape</kbd></dt><dd>close the card's drawer, the document, or this list</dd>`,
		`<dt><kbd>?</kbd></dt><dd>this list</dd>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the board page lacks %s", want)
		}
	}
	for path, wants := range map[string][]string{
		"/static/console.js": {`if (!document.getElementById("board") || e.ctrlKey || e.metaKey || e.altKey) return;`, `e.key === "Escape"`,
			`e.key === "?"`, `window.addEventListener("hashchange", showDrawer);`, `"/drawer"`,
			`if (el.querySelector("details[open]")) return true;`},
		"/static/theme.js": {`document.documentElement.setAttribute("data-theme", localStorage.getItem("theme") === "light" ? "light" : "dark");`},
	} {
		js := r.do(httptest.NewRequest(http.MethodGet, path, nil)).Body.String()
		for _, want := range wants {
			if !strings.Contains(js, want) {
				t.Errorf("%s lacks %s", path, want)
			}
		}
	}
	for path, gone := range map[string]string{"/static/console.js": `e.key === "w"`, "/static/theme.js": "location.replace"} {
		if strings.Contains(r.do(httptest.NewRequest(http.MethodGet, path, nil)).Body.String(), gone) {
			t.Errorf("%s still holds %s", path, gone)
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
			r.tasks.tasks[i].Objective = "Links API\n\nCard links-api of plan #1 (3 of 4), filed for sebastian. Written by the planner.\n\nBuild the links API.\nDepends on: #3"
		}
	}
	r.tasks.deps = map[string][]string{"t-c4": {"t-c3", "t-c5"}}
	r.tasks.plans = map[string][]factoryd.PlanRevision{"t-plan": {planRevision("t-plan", 1, "sha-1")}}
	r.objects["sessions/t-run/timeline.jsonl"] = []byte(`{"time":"2026-10-11T10:04:00Z","step":"checkin","session":"s-1","detector":"no_write","trip":"no write in 8 min","digest":"read console.css twice","decision":"redirect","guidance":"run the failing acceptance test","reason":"","called":true}
{"time":"2026-10-11T10:05:00Z","step":"session_end","started":"2026-10-11T10:00:00Z","attempt":1,"outcome":"pushed"}
{"time":"2026-10-11T10:06:00Z","step":"checkin","round":1,"decision":"reverify","reason":"","verdicts":{"a.go: x is missing":{"state":"dropped","evidence":"the test never ran","round":1}}}
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

// The drawer's Overview is the opened card: the tabs, the title, the
// brief's body, the cards it waits on that are not done in their live
// state, its plan with this card marked and the link to the plan's card,
// the full timeline, and a pending merge's reviews and audit.
func TestDrawerOverview(t *testing.T) {
	r, ck := drawerRig(t)
	// the plan's rows name the children by key, and the board's waiting
	// line names the dependencies not done, as the store reads them
	r.tasks.mu.Lock()
	for i, key := range map[string]string{"t-c2": "foundation", "t-c4": "links-api"} {
		for j := range r.tasks.tasks {
			if r.tasks.tasks[j].ID == i {
				r.tasks.tasks[j].PlanKey = key
			}
			if r.tasks.tasks[j].ID == "t-c4" {
				r.tasks.tasks[j].WaitingOn = "#3"
			}
		}
	}
	r.tasks.mu.Unlock()
	tab := drawerTabMarkup
	card := drawer(t, r, ck, "t-c4", "")
	for _, want := range []string{
		`<div class="drawer-content" data-drawer="t-c4">`,
		`<nav class="tabs" role="tablist" aria-label="The card's tabs">` + tab("t-c4", "overview", "Overview", true) +
			tab("t-c4", "chat", "Session", false) + tab("t-c4", "activity", "Activity", false) + tab("t-c4", "files", "Files", false) +
			tab("t-c4", "receipt", "Receipt", false) + `</nav>`,
		`<h2 class="o-title" tabindex="-1">Links API.</h2>`,
		`<span class="it place"><a class="ref-link" href="#card=t-plan">Link shortener</a> 2/3</span>`,
		`<summary>The full brief<span class="hint">waits on 1 card</span></summary>`,
		`<div class="o-body"><p>Card links-api of plan <a class="ref-link" href="#card=t-plan">#1</a> <span class="ref-of">(Link shortener, 0 of 3 cards done)</span> (3 of 4), ` +
			`filed for sebastian. Written by the planner.</p>` + "\n" +
			`<p>Build the links API.` + "\n" + `Depends on: <a class="ref-link" href="#card=t-c3">#3</a> <span class="ref-of">(card 3, queued)</span></p>`,
		`<h4>It waits on</h4><ul><li><a class="ref-link" href="#card=t-c3">#3</a> <span class="ref-of">(card 3, queued)</span></li></ul>`,
		`<ol class="plan-rows"><li><a class="ref-link" href="#card=t-c2">#2</a> <span class="ref-of">(card 2, queued)</span></li>` +
			`<li class="this" aria-current="true"><a class="ref-link" href="#card=t-c4">#4</a> <span class="ref-of">(Links API, queued)</span> <span class="meta">this card</span></li>` +
			`<li>Acceptance tests <span class="meta">not filed yet</span></li></ol>`,
		`<a href="#card=t-plan">open the plan's card</a>`,
		`<a href="/tasks/t-c4/timeline">the full timeline</a>`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("t-c4's overview lacks %s:\n%s", want, card)
		}
	}
	if strings.Contains(card, "#card=t-c5") {
		t.Errorf("t-c4 waits on its done #5:\n%s", card)
	}

	plan := drawer(t, r, ck, "t-plan", "overview")
	for _, want := range []string{`<h2 class="o-title" tabindex="-1">Link shortener.</h2>`, `<span class="it place">plan of 3 cards</span>`,
		`<summary>The plan<span class="hint">Link shortener, 3 cards, 0 ended</span></summary>`,
		`<li><a class="ref-link" href="#card=t-c2">#2</a> <span class="ref-of">(card 2, queued)</span></li>`,
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("the plan's overview lacks %s:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "open the plan's card") || strings.Contains(plan, `class="this"`) {
		t.Errorf("the plan's own card links itself or marks a card as this one:\n%s", plan)
	}

	merge := drawer(t, r, ck, "t-m", "overview")
	for _, want := range []string{
		`<dt>Model review</dt><dd>It approves the change. <span class="quote">adds the column safely</span><br>- a remark: <span class="quote">a follow-up files the docs</span><br>- dropped (grep finds the client closed in handler.go:40): <span class="quote">internal/api.go: the handler leaks the client</span></dd>`,
		`<dt>Earlier review 1</dt><dd>It has concerns. <span class="quote">the column lacks a default</span><br>- <span class="quote">0018_x.sql: add a default</span><br>- dropped (main already held the default): <span class="quote">0018_x.sql: add a default</span></dd>`,
		`<dt>Security audit</dt><dd>It found problems. The audit ran on <span class="mono">big</span>, <span class="mono">zai/glm-5.3</span>.<br>- <span class="quote">secret in the diff: private key</span>: <code>internal/x/x_test.go:14</code>`} {
		if !strings.Contains(merge, want) {
			t.Errorf("t-m's overview lacks %s:\n%s", want, merge)
		}
	}
	if strings.Contains(drawer(t, r, ck, "t-pr", ""), "<dt>Model review</dt>") {
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
		`<tr><td title="2026-10-11T10:04:00Z"><time>2026-10-11 10:04:00</time></td><td>checkin</td><td><strong>redirect</strong>: run the failing acceptance test</td><td><code>{&#34;called&#34;:true,&#34;decision&#34;:&#34;redirect&#34;,&#34;detector&#34;:&#34;no_write&#34;,&#34;digest&#34;:&#34;read console.css twice&#34;,&#34;guidance&#34;:&#34;run the failing acceptance test&#34;,&#34;reason&#34;:&#34;&#34;,&#34;session&#34;:&#34;s-1&#34;,&#34;trip&#34;:&#34;no write in 8 min&#34;}</code></td></tr>`,
		`<tr><td title="2026-10-11T10:06:00Z"><time>2026-10-11 10:06:00</time></td><td>checkin</td><td></td><td><code>{&#34;decision&#34;:&#34;reverify&#34;,&#34;reason&#34;:&#34;&#34;,&#34;round&#34;:1,&#34;verdicts&#34;:{&#34;a.go: x is missing&#34;:{&#34;evidence&#34;:&#34;the test never ran&#34;,&#34;round&#34;:1,&#34;state&#34;:&#34;dropped&#34;}}}</code></td></tr>`,
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
	if !strings.Contains(failed, `<p class="problem" role="alert">The timeline could not be read: s3: 503. Reload the board.</p>`) || strings.Contains(failed, "<table") {
		t.Errorf("an unreadable timeline:\n%s", failed)
	}
}

// The Supervisor column reads the runner's checkin step: the session
// supervisor's decision renders beside it, a review checkin (round,
// verdicts; no detector, no guidance) keeps its plain row and leaves the
// column empty, on the drawer's Activity and the timeline page alike.
func TestDrawerShowsTheCheckinDecision(t *testing.T) {
	r, ck := drawerRig(t)
	for _, body := range []string{
		drawer(t, r, ck, "t-run", "activity"),
		r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-run/timeline", nil), ck).Body.String(),
	} {
		for _, want := range []string{
			`<td>checkin</td><td><strong>redirect</strong>: run the failing acceptance test</td>`,
			`<td>checkin</td><td></td>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the steps lack %s:\n%s", want, body)
			}
		}
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
// order, each keyed as the stream's copy is, and no box of its own: a
// word to the session is an answer of the opened card; a card whose
// session ended opens on Overview and shows them read-only;
// a card without a session says so.
func TestDrawerChat(t *testing.T) {
	r, ck := drawerRig(t)
	r.c.cfg.Steer = func(context.Context, string, string, string) error { return nil }
	r.objects["sessions/t-run/timeline.jsonl"] = []byte(`{"time":"2026-10-11T09:00:00Z","step":"session_start","session":"s-1","model":"coding-standard","attempt":1}
{"time":"2026-10-11T09:20:00Z","step":"session_end","session":"s-1","outcome":"failed"}
{"time":"2026-10-11T10:00:00Z","step":"session_start","session":"s-2","model":"big","attempt":2}
`)
	r.objects["sessions/t-run/s-2.jsonl"] = []byte(`{"time":"2026-10-11T10:01:00.5Z","kind":"model_call","detail":{"text":"Reading the cache first.","cost":0.01}}
{"time":"2026-10-11T10:01:30Z","kind":"tool_call","detail":{"tool":"read"}}
{"time":"2026-10-11T10:02:00Z","kind":"model_call","detail":{"cost":0.01}}
{"time":"2026-10-11T10:04:00Z","kind":"model_call","detail":{"text":"Done <b>here</b>.\nNext: the tests."}}
`)
	r.objects["sessions/t-pr/timeline.jsonl"] = []byte(`{"time":"2026-10-11T08:00:00Z","step":"session_start","session":"s-9","model":"small","attempt":1}
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
		{TaskID: "t-run", At: at("10:03"), Kind: factoryd.EventCardRetried, Actor: "sebastian"},
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
			drawerTabMarkup("t-run", "activity", "Activity", false) + drawerTabMarkup("t-run", "files", "Files", false) +
			drawerTabMarkup("t-run", "receipt", "Receipt", false) + `</nav>`,
		`<div id="drawer-panel" class="drawer-body" role="tabpanel">
<h2 class="dr-title" tabindex="-1">fix the cache</h2>
<p class="meta">Session 2 · big · running</p>
<div id="session-log" class="session-log" role="log" aria-label="The messages of session 2" data-task="t-run" data-session="s-2">
<div class="msg agent" data-key="s-2@2026-10-11T10:01:00.5Z"><span class="av" aria-hidden="true"></span><p class="who"><span class="name">agent</span> <time>10-11 10:01Z</time></p><p class="said">Reading the cache first.</p></div>
<div class="msg you" data-key="e2"><span class="av" aria-hidden="true">S</span><p class="who"><span class="name">sebastian</span> <time>10-11 10:03Z</time></p><p class="said">Rebase first.</p></div>
<div class="msg agent" data-key="s-2@2026-10-11T10:04:00Z"><span class="av" aria-hidden="true"></span><p class="who"><span class="name">agent</span> <time>10-11 10:04Z</time></p><p class="said">Done &lt;b&gt;here&lt;/b&gt;.
Next: the tests.</p></div>
</div>
</div>`,
	} {
		if !strings.Contains(running, want) {
			t.Errorf("t-run's drawer lacks\n%s\nin\n%s", want, running)
		}
	}
	if ended := drawer(t, r, ck, "t-pr", ""); !strings.Contains(ended, drawerTabMarkup("t-pr", "overview", "Overview", true)) {
		t.Errorf("a card that runs no session does not open on Overview:\n%s", ended)
	}
	if got, want := drawer(t, r, ck, "t-pr", "chat"), `<p class="meta">Session 1 · small · pushed</p>
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

// A prover's Chat tab shows no transcript: the drawer names why, and none
// of the session's model text, which holds the plan's blind proofs.
func TestDrawerChatShowsNoProversTranscript(t *testing.T) {
	r, ck := drawerRig(t)
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-prove", Kind: "agent", Status: "running",
		Source: factoryd.SourceProver, Parent: "t-plan", Track: policy.TrackDirect, Repository: "sf/code"}})
	r.objects["sessions/t-prove/timeline.jsonl"] = []byte(`{"time":"2026-10-11T10:00:00Z","step":"session_start","session":"s-1","model":"big","attempt":1,"role":"prover"}
`)
	r.objects["sessions/t-prove/s-1.jsonl"] = []byte(`{"time":"2026-10-11T10:01:00Z","kind":"model_call","detail":{"text":"package key_log\n\nfunc TestKeyLog(t *testing.T) {}"}}
`)
	got := drawer(t, r, ck, "t-prove", "chat")
	if !strings.Contains(got, `<p class="problem" role="alert">Task t-prove is plan t-plan&#39;s prover: its sessions hold the plan&#39;s blind proofs, which no read outside the prover shows.</p>`) ||
		strings.Contains(got, "key_log") || strings.Contains(got, `class="msg agent"`) {
		t.Fatalf("the prover's chat tab:\n%s", got)
	}
}

// A word to the running session is "Tell the running session" among the
// opened card's other actions, the one way a human steers it: the steer
// function at once with the words trimmed, and its row with the text; a
// GitHub token in it reaches neither the session, the row nor the
// stream. A card that runs no session does not offer it and refuses it.
// The Session tab holds no box of its own, and the old door is gone.
func TestTellTheRunningSession(t *testing.T) {
	r, _ := drawerRig(t)
	var steered []string
	r.c.cfg.Steer = func(_ context.Context, taskID, user, text string) error {
		steered = append(steered, taskID+" "+user+": "+text)
		return nil
	}
	r.objects["sessions/t-run/timeline.jsonl"] = []byte(`{"time":"2020-01-01T00:00:00Z","step":"session_start","session":"s-1","model":"big","attempt":1}
`)
	ck, s := r.loggedIn(t)
	send := answerForm(t, drawer(t, r, ck, "t-run", "overview"), factoryd.AnswerSteer)
	if send.Label != "Tell the running session" || send.Action != "/tasks/t-run/resolve" {
		t.Fatalf("the steer answer is %q to %s", send.Label, send.Action)
	}
	tell := func(text string) *httptest.ResponseRecorder {
		v := maps.Clone(send.Values)
		v.Set(factoryd.AnswerSteer+".note", text)
		return r.do(post(send.Action, v), ck)
	}
	before := r.events.count()
	rec := tell(" Rebase on main first. ")
	rows := r.events.all()[before:]
	if rec.Code != http.StatusSeeOther || len(rows) != 1 {
		t.Fatalf("the steer = %d, %d rows: %s", rec.Code, len(rows), rec.Body)
	}
	row := rows[0]
	if want := (factoryd.Event{ID: row.ID, At: row.At, Repository: "sf/code", TaskID: "t-run", IssueNumber: 11, Kind: factoryd.EventCardSteered,
		Actor: "sebastian", Via: factoryd.ViaButton, Detail: map[string]any{"text": "Rebase on main first."}}); !reflect.DeepEqual(row, want) {
		t.Errorf("the steer's row = %+v, want %+v", row, want)
	}
	if !slices.Equal(steered, []string{"t-run sebastian: Rebase on main first."}) {
		t.Errorf("steered %q", steered)
	}
	if got := drawer(t, r, ck, "t-run", "chat"); !strings.Contains(got, `<div class="msg you" data-key="e`+strconv.FormatInt(row.ID, 10)+`">`) ||
		strings.Contains(got, "steer-form") || strings.Contains(got, "<textarea") {
		t.Errorf("the Session tab after the steer:\n%s", got)
	}

	const token = "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	_, br := streamOf(t, r, "")
	rec = tell("Push with " + token + " now.")
	all := r.events.all()
	row = all[len(all)-1]
	if rec.Code != http.StatusSeeOther || row.Detail["text"] != "Push with [token] now." || steered[1] != "t-run sebastian: Push with [token] now." {
		t.Errorf("a steer with a token = %d, row %v, steered %q", rec.Code, row.Detail, steered)
	}
	if got := strings.Join(nextMessage(t, br), "\n"); !strings.Contains(got, `"detail":{"text":"Push with [token] now."}`) || strings.Contains(got, token) {
		t.Errorf("the stream carries %s", got)
	}

	before = r.events.count()
	if body := drawer(t, r, ck, "t-pr", "overview"); strings.Contains(body, `data-answer="`+factoryd.AnswerSteer+`"`) {
		t.Errorf("a card in review offers a word to its session:\n%s", body)
	}
	v := maps.Clone(send.Values)
	v.Set(factoryd.AnswerSteer+".note", "hello")
	if rec := r.do(post("/tasks/t-pr/resolve", v), ck); rec.Code != http.StatusBadRequest || len(steered) != 2 || r.events.count() != before {
		t.Errorf("a steer to a card in review = %d, steered %q, %d rows: %s", rec.Code, steered, r.events.count()-before, rec.Body)
	}
	if rec := r.do(post("/tasks/t-run/steer", url.Values{"csrf": {s.csrf}, "text": {"hello"}}), ck); rec.Code != http.StatusNotFound || len(steered) != 2 {
		t.Errorf("the Session tab's old door = %d, steered %q", rec.Code, steered)
	}
	for err, want := range steerRefusals() {
		r.c.cfg.Steer = func(context.Context, string, string, string) error { return err }
		before = r.events.count()
		if rec := tell("hello"); rec.Code != http.StatusConflict ||
			strings.TrimSpace(rec.Body.String()) != want || r.events.count() != before {
			t.Errorf("a steer the session refuses with %v = %d, %d rows: %q, want 409 %q", err, rec.Code, r.events.count()-before, rec.Body, want)
		}
	}
}

// steerRefusals are the session states a steer to card #11 meets, each
// with the refusal that names it: a start check-in since 17:31 UTC, no
// session, a session that ended.
func steerRefusals() map[error]string {
	return map[error]string{
		&factoryd.SessionStartingError{Since: time.Date(2026, 11, 17, 17, 31, 24, 0, time.UTC)}: "Card #11 is in its start check-in since 17:31, which decides whether the agent starts. Send it once the agent starts, or comment on the card.",
		factoryd.ErrNoLiveSession: "Card #11 is between sessions. Comment on the card, and the next session reads it.",
		harness.ErrNotRunning:     "Card #11's session just ended. Comment on the card, and the next session reads it.",
	}
}

// The drawer and its word to the session are its project's writers': 403
// to anyone else and to a word without the form token, 404 for an unknown
// card, 400 for an unknown tab.
func TestDrawerRefusals(t *testing.T) {
	r, ck := drawerRig(t)
	steered := 0
	r.c.cfg.Steer = func(context.Context, string, string, string) error { steered++; return nil }
	for path, code := range map[string]int{"/cards/t-nope/drawer": http.StatusNotFound, "/cards/t-run/drawer?tab=steps": http.StatusBadRequest} {
		if rec := r.do(httptest.NewRequest(http.MethodGet, path, nil), ck); rec.Code != code {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, code)
		}
	}
	ak, _ := r.writerOf(t, "alice", "demo-app/demo-app")
	for _, tab := range []string{"overview", "activity", "files", "chat", "receipt"} {
		if rec := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-run/drawer?tab="+tab, nil), ak); rec.Code != http.StatusForbidden {
			t.Errorf("alice, no writer of the code project, opens %s: %d, want 403", tab, rec.Code)
		}
	}
	before := r.events.count()
	ak, as := memberLogin(t, r, "alice")
	word := url.Values{"action": {factoryd.AnswerSteer}, factoryd.AnswerSteer + ".note": {"hello"}}
	if rec := r.do(post("/tasks/t-run/resolve", url.Values{"csrf": {as.csrf}, "action": word["action"], factoryd.AnswerSteer + ".note": {"hello"}}), ak); rec.Code != http.StatusForbidden {
		t.Errorf("alice steers t-run: %d, want 403", rec.Code)
	}
	if rec := r.do(post("/tasks/t-run/resolve", word), ck); rec.Code != http.StatusForbidden {
		t.Errorf("a steer without the form token: %d, want 403", rec.Code)
	}
	if steered != 0 || r.events.count() != before {
		t.Errorf("refused steers reached the session %d times and left %d rows", steered, r.events.count()-before)
	}
}

// No human surface asks for an escalation verdict: the escalated card
// shows no question and no form to /alerts/, on the board and in the drawer.
func TestEscalatedCardAsksNoVerdict(t *testing.T) {
	r, ck := screenRig(t)
	for path, ask := range map[string]string{"/": "Which region?", "/cards/t-esc/drawer": "Which region?"} {
		body := r.do(httptest.NewRequest(http.MethodGet, path, nil), ck).Body.String()
		if !strings.Contains(body, ask) {
			t.Fatalf("GET %s: the escalation is not on the page", path)
		}
		for _, banned := range []string{"Was this escalation a real problem", "/alerts/", `name="verdict"`} {
			if strings.Contains(body, banned) {
				t.Errorf("GET %s offers %q", path, banned)
			}
		}
	}
	if rec := r.do(post("/alerts/7/verdict", url.Values{"verdict": {"real"}}), ck); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /alerts/7/verdict = %d, want no route", rec.Code)
	}
}

// An alert_verdict row from before the survey went still reads on its
// card's activity line.
func TestOldVerdictEventsStillRender(t *testing.T) {
	e := factoryd.Event{Kind: factoryd.EventAlertVerdict, Actor: "sebastian", Via: "board",
		At: time.Date(2026, 9, 28, 9, 30, 0, 0, time.UTC), Detail: map[string]any{"alert": 3, "verdict": "real"}}
	if got, want := activityLine(e), "alert verdict by sebastian via board at 09:30"; got != want {
		t.Errorf("activityLine = %q, want %q", got, want)
	}
}

// The approval cards' forms carry the board's filter and land on it: an
// approval of another project's merge goes back to that project's board.
// Each answer is its own send, and the decision lands on the card: its
// request decided by the sender and one row. An approve that names
// another answer decides nothing.
func TestApprovalFormsKeepTheBoard(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	ck, s := r.loggedIn(t)
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 30, Kind: factoryd.ApprovalMerge, TaskID: "t-lm", Project: "linkshort/linkshort",
		PRNumber: 12, Status: "pending", Spec: json.RawMessage(`{"risk_class":"low","head_sha":"def456","review":{"verdict":"approve"}}`)})
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-lm", Kind: "agent", Status: "in-review", Repository: "linkshort/linkshort", IssueNumber: 12}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-res", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 21}})
	ask := askBlockOf(t, r.do(httptest.NewRequest(http.MethodGet, "/cards/t-lm/drawer?project=linkshort", nil), ck).Body.String())
	filter := `<input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value="linkshort">`
	if forms, got := strings.Count(ask, "<form "), strings.Count(ask, filter); forms != 3 || got != forms {
		t.Errorf("the merge card's %d answers carry the filter %d times, want 3 each once:\n%s", forms, got, ask)
	}
	before := r.events.count()
	if rec := r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "action": {"deny"}}), ck); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "The answer deny is not one the card shows.") || r.approvals.get(4).Status != "pending" {
		t.Errorf("an approve naming deny = %d %q, request %q", rec.Code, rec.Body, r.approvals.get(4).Status)
	}
	for _, tc := range []struct {
		path string
		form url.Values
		want string
	}{
		{"/approvals/30/approve", url.Values{"project": {"linkshort"}, "action": {"approve"}}, "/?project=linkshort"},
		{"/approvals/3/approve", url.Values{"project": {"ai-software-factory-code"}, "q": {"load"}}, "/?project=ai-software-factory-code&q=load"},
		{"/approvals/4/deny", url.Values{"project": {"ai-software-factory-code"}, "action": {"request_changes"}, "note": {"not now"}}, "/?project=ai-software-factory-code"},
	} {
		tc.form.Set("csrf", s.csrf)
		if rec := r.do(post(tc.path, tc.form), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != tc.want {
			t.Errorf("POST %s = %d to %q, want 303 to %q: %s", tc.path, rec.Code, rec.Header().Get("Location"), tc.want, rec.Body)
		}
	}
	for id, want := range map[int64][2]string{
		30: {"approved", ""},
		3:  {"approved", "approved for the task"},
		4:  {"denied", "changes requested: not now"},
	} {
		if a := r.approvals.get(id); a.Status != want[0] || a.DecidedBy != "sebastian" || a.DecisionNote != want[1] {
			t.Errorf("approval %d = %q by %q, note %q, want %s by sebastian, note %q", id, a.Status, a.DecidedBy, a.DecisionNote, want[0], want[1])
		}
	}
	checkRows(t, r.events, before,
		wantRow{kind: factoryd.EventMergeApproved, repo: "linkshort/linkshort", taskID: "t-lm", detail: map[string]any{"pr": int64(12)}},
		wantRow{kind: factoryd.EventMergeApproved, repo: "sf/code", taskID: "t-res", detail: map[string]any{"approval": int64(3), "kind": "resource"}},
		wantRow{kind: factoryd.EventMergeDenied, repo: "sf/code", taskID: "t-m",
			detail: map[string]any{"pr": int64(9), "action": "request_changes"}})
}

// The merge item on the four-column board is a small card that asks its
// merge, with its model review's pick as its one press and no facts. Its
// opened card asks the merge with why a human decides it, the model
// review's pick and each answer's effect, one send per answer and no fund
// beside them, and its full brief holds the facts: the risk, the
// migration, the reviews, the recommendation and why a human (item 4).
func TestNeedsYouMergeItemIsBrief(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 9}})
	face := faceOf(t, r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(), "t-m")
	if !strings.Contains(face, `<strong class="title">Merge this card&#39;s pull request?</strong>`) {
		t.Errorf("the merge item does not ask its merge:\n%s", face)
	}
	for _, gone := range []string{`name="action" value="request_changes"`, "0018_x.sql", "Earlier review", "Security audit", "Why a human", "<details", "<dt>", "chip"} {
		if strings.Contains(face, gone) {
			t.Errorf("the merge item shows %s, which the opened card holds:\n%s", gone, face)
		}
	}
	if strings.Count(face, "<form") != 1 || !strings.Contains(face, `action="/approvals/4/approve"`) {
		t.Errorf("the merge item holds a form other than its pick's press:\n%s", face)
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-m/drawer?tab=overview", nil), ck).Body.String()
	ask := askBlockOf(t, body)
	for _, want := range []string{`<p class="o-question">Merge this card&#39;s pull request?</p>`,
		`<p class="o-askwhy">The change is high risk, and the factory&#39;s rules let only a human merge it.</p>`,
		`<p class="o-by">suggested by the model review: <span class="quote">It approved the change with no finding, and the security audit raised 2 findings.</span></p>`,
		`<span class="effect">Merges the pull request.</span>`, `<span class="effect">Returns the pull request to its agent with your note.</span>`,
		`<span class="effect">Closes the pull request. The card ends.</span>`} {
		if !strings.Contains(ask, want) {
			t.Errorf("the merge ask lacks %s:\n%s", want, ask)
		}
	}
	for what, want := range map[string]int{`<form `: 3, `action="/approvals/4/approve"`: 1, `<input type="hidden" name="action" value="request_changes">`: 1,
		`<input type="hidden" name="action" value="reject">`: 1, `name="fund_eur"`: 0, `<textarea`: 2} {
		if got := strings.Count(ask, what); got != want {
			t.Errorf("the merge ask carries %s %d times, want %d:\n%s", what, got, want, ask)
		}
	}
	for _, want := range []string{`<dt>Risk class</dt><dd>high</dd>`,
		`<dt>Migration</dt><dd>yes, human review required: <code>internal/factoryd/migrations/0018_x.sql</code></dd>`,
		`<dt>Model review</dt><dd>It approves the change. <span class="quote">adds the column safely</span><br>`,
		`<dt>Agent's recommendation</dt><dd class="reason">Adds the column; merge it.</dd>`,
		`<dt>Why a human</dt><dd><ul><li>the project&#39;s merge rule is human</li><li>risk class high is requires_approval</li></ul></dd>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the merge card's brief lacks %s:\n%s", want, body)
		}
	}
}

// The 2026-10-17 ruling: a needs-you card is compact on the board and
// opens its task's drawer by its title, exactly like a face card — a
// door only for the one who writes the project, whose drawer it is.
func TestNeedsYouCardOpensItsDrawer(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	lane := col(t, r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(), "Needs you")
	for _, id := range []string{"t-res", "t-m"} {
		if want := `<a class="open" href="#card=` + id + `">`; !strings.Contains(faceOf(t, lane, id), want) {
			t.Errorf("a writer's Needs you lacks %q:\n%s", want, lane)
		}
	}
	ak, _ := r.readerOfCode(t, "alice")
	lane = col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ak).Body.String(), "Needs you")
	if strings.Contains(lane, `class="open"`) || strings.Contains(lane, `href="#card=`) || !strings.Contains(lane, `data-card="t-res"`) || !strings.Contains(lane, `data-card="t-m"`) {
		t.Errorf("a reader's Needs you links to a drawer that would refuse:\n%s", lane)
	}
}

// The four columns hold their cards by arrival, the newest on top
// (Sebastian, 2026-10-14): Queued by filing, In progress by admission,
// Done by finishing, whatever order the store gave them; a card without
// the time keeps its filing time, and the store's order holds among
// equals.
func TestColumnsHoldTheNewestCardOnTop(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	now := time.Now()
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	task := func(id, status string, filed time.Duration) factoryd.Task {
		return factoryd.Task{ID: id, Kind: "agent", Status: status, Track: "direct", Repository: "sf/code", Objective: id, CreatedAt: now.Add(-filed)}
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
}

// The find (C13) narrows every column to its terms and the head reads
// "n of m": a word against what the face says (the ref, the title, the
// question), is:, status: and by: against the facts, every term matching;
// an emptied column says "none match" and the Needs you column keeps its
// amber (lit) from its whole count. The forms, the board's query and the
// clear link carry q, and the status panel keeps its two sections with
// the whole project's figures.
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
		{"t-run", "Queued", nil, "<b>0</b> of 2"},
		{"t-run", "Needs you", nil, "<b>0</b> of 6"},
		{"is:failed", "Done", []string{"t-fail"}, "<b>1</b> of 3"},
		{"is:blocked", "Queued", []string{"t-deps"}, "<b>1</b> of 2"},
		{"status:queued", "Queued", []string{"t-queue"}, "<b>1</b> of 2"},
		{"by:alice", "Needs you", []string{"t-other"}, "<b>1</b> of 6"},
		{"which region", "Needs you", []string{"t-esc"}, "<b>1</b> of 6"},
		{"rotate key", "Needs you", []string{"t-human"}, "<b>1</b> of 6"},
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
	for _, want := range []string{`<section class="column lit" aria-labelledby="col-0" id="needs-you" data-holds="waiting-human waiting-poster">`,
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
	// the panel keeps spend, provider, GitHub and the configuration
	// version (C9), under a find as without one
	for _, want := range []string{`<h3>Spend</h3>`, `<span class="fig">0.00 of 20.00 EUR day cap</span>`,
		`<h3>Factory</h3>`, `<p class="none">unknown</p>`,
		`<a class="lchip" href="https://git.example/sf/infra/src/commit/c9d98bb0aa11/factory" target="_blank" rel="noopener" data-tip="version 4 · c9d98bb0aa11">config</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("under the find the status panel lacks %s", want)
		}
	}
	asked := page("which region")
	for _, want := range []string{`<input type="hidden" name="q" value="which region">`} {
		if !strings.Contains(asked, want) {
			t.Errorf("the escalation's forms lack %s", want)
		}
	}
	// without a find the × is in place for the box's text to show it (CSS)
	plain := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{findForm, `<input id="q" name="q" type="text" value="" placeholder="Find: a word, #612, plan:558"`,
		`<a class="find-x" href="/?project=ai-software-factory-code" aria-label="Clear the find"`, `>Needs you <span class="count">6</span></h3>`} {
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

// htmx keeps a field marked hx-preserve by moving the live element into the
// answer it is about to swap in: a marked field inside a marked form (the
// find box inside the find's form) is carried out of the form into content
// the swap discards, and the field vanishes from the page. No preserved
// element may sit inside another.
func TestNoPreservedElementSitsInsideAnother(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	doc, err := html.Parse(r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body)
	if err != nil {
		t.Fatal(err)
	}
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, preserved bool) {
		marked := false
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				if a.Key != "hx-preserve" {
					continue
				}
				if preserved {
					id := ""
					for _, a := range n.Attr {
						if a.Key == "id" {
							id = a.Val
						}
					}
					t.Errorf("<%s id=%q> sits inside another preserved element; the swap carries it out and the field vanishes", n.Data, id)
				}
				marked = true
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, preserved || marked)
		}
	}
	walk(doc, false)
}

// The filter's query carries the find, trimmed, beside the project.
func TestFilterQueryCarriesTheFind(t *testing.T) {
	if got := (boardFilter{Project: "p", Q: "a b"}).query(); got != "?project=p&q=a+b" {
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
	task := func(id, status, track string, filed time.Duration) factoryd.Task {
		return factoryd.Task{ID: id, Kind: "agent", Status: status, Track: track, Repository: "sf/code", Objective: id, CreatedAt: now.Add(-filed), BudgetEUR: 2}
	}
	plan := task("t-plan", "done", "plan", 10*time.Hour)
	plan.IssueNumber, plan.FinishedAt = 7, ago(9*time.Hour)
	qb := task("t-q-b", "queued", "direct", 3*time.Hour) // from plan #7
	qb.Parent, qb.Source = "t-plan", factoryd.SourcePlan
	qc := task("t-q-c", "waiting-deps", "direct", 2*time.Hour)
	qa := task("t-q-a", "queued", "direct", 30*time.Minute)
	// build and planning filed inside the stale window, so with no event
	// they wear no ring; verify is stale and near its budget
	build := task("t-build", "running", "direct", 25*time.Minute)
	build.AdmittedAt, build.SpentEUR = ago(20*time.Minute), 0.5
	verify := task("t-verify", "in-review", "direct", 4*time.Hour)
	verify.AdmittedAt, verify.SpentEUR = ago(time.Hour), 1.9
	planning := task("t-planning", "running", "plan", 28*time.Minute)
	planning.AdmittedAt, planning.SpentEUR = ago(24*time.Minute), 1
	done := task("t-done", "done", "direct", 8*time.Hour)
	done.FinishedAt = ago(time.Hour)
	fail := task("t-fail", "failed", "direct", 7*time.Hour)
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
		`<form method="get" action="/" class="menu-pop auto-submit" hx-get="/" hx-select="#board" hx-target="#board" hx-swap="outerHTML"><input type="hidden" name="project" value="ai-software-factory-code"><input type="hidden" name="queued" value="oldest"><input type="hidden" name="queued-only" value="blocked"><fieldset><legend>Order</legend><label><input type="radio" name="done" value="" checked>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("under the Queued choices the board lacks %s", want)
		}
	}
	if n := strings.Count(body, `<p class="col-state">`); n != 1 {
		t.Errorf("%d state lines, want 1", n)
	}
	// a choice applies at once, read into the board in place: the menu's
	// form holds a button only for a browser that runs no script
	for _, menu := range regexp.MustCompile(`(?s)<form method="get" action="/" class="menu-pop auto-submit" hx-get="/" hx-select="#board" hx-target="#board" hx-swap="outerHTML">.*?</form>`).FindAllString(body, -1) {
		if strings.Count(menu, "<button") != 1 || !strings.Contains(menu, `<noscript><button type="submit" class="apply">Apply</button></noscript></form>`) {
			t.Errorf("a column menu holds a button outside its no-script Apply: %s", menu)
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
// says the board stops. A find or a show reads them all.
func TestDonePagesItsFinishedCards(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	now := time.Now()
	// t-d000 to t-d329 finish in threes a minute apart, t-d000 newest:
	// t-d048, t-d049 and t-d050 share a stamp across the first page's end
	fin := func(i int) *time.Time { at := now.Add(-time.Duration(i/3) * time.Minute); return &at }
	task := func(id, status string, finished *time.Time) factoryd.BoardTask {
		return factoryd.BoardTask{Task: factoryd.Task{ID: id, Kind: "agent", Status: status, Track: "direct", Repository: "sf/code", Objective: id,
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

	for _, target := range []string{
		"/board/finished?col=done",                                                  // after no card
		"/board/finished?col=done&after-id=t-d049",                                  // no arrival
		"/board/finished?col=done&after=yesterday&after-id=t-d049",                  // no time
		"/board/finished?col=done&after=2026-10-15T00:00:00Z&after-id=t-d049&q=t-f", // a find reads them all
		"/board/finished?col=failed&after=2026-10-15T00:00:00Z&after-id=t-d049",     // Done holds failed
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
			ID: fmt.Sprintf("t-d%02d", i), Kind: "agent", Status: "done", Track: "direct", Repository: "sf/code",
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

// A plan's name is a find word and a plan: value: plan:558 matches the
// cards of plan 558 and the plan's own card, by issue number exactly and never a name that merely contains 558, any
// other value is a case-insensitive part of the name; the opened card's
// head names the plan, linked to the plan's card.
func TestFindAndDrawerSpeakThePlanName(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = planTasks()
	r.tasks.tasks[0].PlanName = "Fix prod latency"
	r.tasks.tasks[5].PlanName = "batch 1 jobs"
	held := func(q string) []string {
		body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort&q="+url.QueryEscape(q), nil), ck).Body.String()
		got := append(columnItems(t, body, "Queued"), columnItems(t, body, "Done")...)
		slices.Sort(got)
		return got
	}
	for _, c := range []struct {
		q    string
		want []string
	}{
		{"plan:latency", []string{"t-c2", "t-c3", "t-c4", "t-c5", "t-plan"}},
		{"plan:LATENCY", []string{"t-c2", "t-c3", "t-c4", "t-c5", "t-plan"}},
		{"prod latency", []string{"t-c2", "t-c3", "t-c4", "t-c5", "t-plan"}},
		{"plan:1", []string{"t-c2", "t-c3", "t-c4", "t-c5", "t-plan"}},
		{"plan:#1", []string{"t-c2", "t-c3", "t-c4", "t-c5", "t-plan"}},
		{"plan:", nil},
		{"plan:#", nil},
		{"plan:0", nil},
		{"plan:batch", []string{"t-spec2"}},
	} {
		if got := held(c.q); !slices.Equal(got, c.want) {
			t.Errorf("q=%q holds %v, want %v", c.q, got, c.want)
		}
	}
	drawer := func() string {
		return r.do(httptest.NewRequest(http.MethodGet, "/cards/t-c5/drawer?tab=overview&project=linkshort", nil), ck).Body.String()
	}
	const link = `<span class="it place"><a class="ref-link" href="#card=t-plan">`
	if d := drawer(); !strings.Contains(d, link+`Fix prod latency</a></span>`) {
		t.Errorf("the drawer lacks its named plan:\n%s", d)
	}
	r.tasks.tasks[0].PlanName = ""
	if d := drawer(); !strings.Contains(d, link+`Link shortener</a></span>`) {
		t.Errorf("the drawer lacks its unnamed plan, named by the plan's title:\n%s", d)
	}
}

// A console without factoryd's steer offers no word to the running
// session, refuses one and records nothing, instead of calling a nil
// function.
func TestTellTheRunningSessionWithoutSteer(t *testing.T) {
	r, _ := drawerRig(t)
	r.c.cfg.Steer = nil
	ck, s := r.loggedIn(t)
	if body := drawer(t, r, ck, "t-run", "overview"); strings.Contains(body, `data-answer="`+factoryd.AnswerSteer+`"`) {
		t.Errorf("a console without steer offers a word to the session:\n%s", body)
	}
	before := r.events.count()
	rec := r.do(post("/tasks/t-run/resolve", url.Values{"csrf": {s.csrf}, "action": {factoryd.AnswerSteer}, factoryd.AnswerSteer + ".note": {"hello"}}), ck)
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "This console cannot message a running session") || r.events.count() != before {
		t.Fatalf("a word to the session without Steer = %d, %d new rows: %s", rec.Code, r.events.count()-before, rec.Body)
	}
}

// The note of a close by a user without write access says on the card's
// activity line that the close was ignored, why, and names the user.
func TestActivityLineOfAnIssueClosedWithoutWriteAccess(t *testing.T) {
	e := factoryd.Event{Kind: factoryd.EventIssueClosedWithoutWriteAccess, Actor: "mallory", Via: factoryd.ViaFactoryd, At: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	if got, want := activityLine(e), "Ignored the close by mallory: mallory cannot write the repository."; got != want {
		t.Errorf("activityLine = %q, want %q", got, want)
	}
}

// The four columns are the one board: an old link to the wide view reads
// the board as any other link does, with no lanes, no toggle and no view
// carried on.
func TestNoWideView(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	for _, path := range []string{"/?view=wide", "/board?view=wide"} {
		rec := getPage(r, ck, path)
		body := rec.Body.String()
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, rec.Code, body)
		}
		for _, title := range []string{"Needs you", "Queued", "In progress", "Done"} {
			col(t, body, title)
		}
		if n := strings.Count(body, `<section class="column`); n != 4 {
			t.Errorf("GET %s holds %d columns, want the four", path, n)
		}
		for _, gone := range []string{"Human lane", "Agent lane", `id="view-toggle"`, `name="view"`, "view=wide"} {
			if strings.Contains(body, gone) {
				t.Errorf("GET %s still holds %q", path, gone)
			}
		}
	}
}

// A running card frozen on a named provider limit splits its clocks: a
// ten-hour building stage with six hours and 48 minutes of it frozen
// reads "3 h 12 min, frozen 6 h 48 min", and its flag carries the
// provider's words and the reset it named.
func TestCardFrozenOnAProviderLimitSplitsItsClocks(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	// the stage's start lands on a minute, so the face's whole minutes
	// hold however long the render takes
	admitted := time.Now().Truncate(time.Minute).Add(-10 * time.Hour)
	r.tasks.set("", factoryd.Task{ID: "t-frozen", Kind: "agent", Status: "running", Track: "direct", Repository: "sf/code",
		Objective: "t-frozen", CreatedAt: admitted, AdmittedAt: &admitted})
	refused := admitted.Add(192 * time.Minute) // 3 h 12 m into the stage
	reset := refused.Add(7 * time.Hour)
	line, err := json.Marshal(map[string]any{"step": "provider_refused", "time": refused.UTC().Format(time.RFC3339Nano),
		"status": 429, "backoff_s": 25200, "words": "rate limit exceeded"})
	if err != nil {
		t.Fatal(err)
	}
	r.objects["sessions/t-frozen/timeline.jsonl"] = append([]byte(`{"step":"session_start","attempt":1}
{"step":"session_end","attempt":1,"outcome":"provider_refused"}
`), append(line, '\n')...)
	until := `<span class="it flag">frozen until ` + reset.UTC().Format("15:04 UTC") + `: rate limit exceeded</span>`
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if face, want := faceOf(t, body, "t-frozen"), `building</span> 3 h 12 min, frozen 6 h 48 min</span> `+until; !strings.Contains(face, want) {
		t.Errorf("the face lacks %s:\n%s", want, face)
	}
	// the opened card's clock splits the same way, and its life records
	// the frozen interval with the provider's words and the reset
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-frozen/drawer?tab=overview", nil), ck).Body.String()
	for _, want := range []string{
		`<span class="it">3 h 12 min, frozen 6 h 48 min</span> ` + until,
		`<span class="tnote">frozen on the provider limit until ` + reset.UTC().Format("15:04 UTC") + `: rate limit exceeded</span>`,
	} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the drawer lacks %s", want)
		}
	}
}
