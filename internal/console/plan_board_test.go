package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// withLinkshort adds the project linkshort, which follows process spec, to
// the rig's projects.
func withLinkshort(r *rig) {
	r.projects.rows = append(r.projects.rows, factoryd.Project{Name: "linkshort", Repository: "linkshort/linkshort", MergeRule: "human",
		Objects: "factory", Agents: "coder", File: true, GitToken: "t", Process: "spec", Tracks: testProcesses()["spec"].Tracks})
}

// col is the section of the board's column title: from its heading to the
// column's end.
func col(t *testing.T, body, title string) string {
	t.Helper()
	start := strings.Index(body, ">"+title+" <span")
	if start < 0 {
		t.Fatalf("column %q missing", title)
	}
	return body[start : start+strings.Index(body[start:], "</section>")]
}

// as logs user, a writer of the factory's project, in as a further
// session of the rig.
func (r *rig) as(t *testing.T, user string) (*http.Cookie, *session) {
	t.Helper()
	r.fg.mu.Lock()
	r.fg.user = user
	r.fg.mu.Unlock()
	if !slices.Contains(r.repoWriters["sf/code"], user) {
		r.repoWriters["sf/code"] = append(r.repoWriters["sf/code"], user)
		r.forgetWriters()
	}
	return r.loggedIn(t)
}

// planTasks are a spec's plan on linkshort, done (#1), with its cards: #2
// and #3 queued, #4 waiting on #3 and #5, #5 done; a second spec (#6)
// queued on the plan track; and two cards of the code project, #11
// running and #12 queued.
func planTasks() []factoryd.BoardTask {
	child := func(id string, n int64, status string) factoryd.Task {
		return factoryd.Task{ID: id, Kind: "agent", Status: status, Track: "direct", Process: "spec", Objective: fmt.Sprintf("card %d", n),
			Repository: "linkshort/linkshort", IssueNumber: n, Source: factoryd.SourcePlan, Parent: "t-plan", PlanKey: fmt.Sprintf("k%d", n)}
	}
	return []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "done", Track: "plan", Process: "spec", Objective: "Link shortener",
			Repository: "linkshort/linkshort", IssueNumber: 1}},
		{Task: child("t-c2", 2, "queued")},
		{Task: child("t-c3", 3, "queued")},
		{Task: child("t-c4", 4, "waiting-deps"), WaitingOn: "#3, #5"},
		{Task: child("t-c5", 5, "done")},
		{Task: factoryd.Task{ID: "t-spec2", Kind: "agent", Status: "queued", Track: "plan", Process: "spec", Objective: "Another spec",
			Repository: "linkshort/linkshort", IssueNumber: 6}},
		{Task: factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Track: "direct", Process: "maintenance", Objective: "fix the cache",
			Repository: "sf/code", IssueNumber: 11}},
		{Task: factoryd.Task{ID: "t-b", Kind: "agent", Status: "queued", Track: "direct", Process: "maintenance", Objective: "an improvement",
			Repository: "sf/code", IssueNumber: 12}},
	}
}

// Roadmap 225: the wide view shows a column per state and no lane column,
// queued and waiting-deps in Queued, closing in Running; no card wears a
// lane badge; a plan's card reads its plan; the board shows one project.
func TestWideViewHasNoLaneColumn(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = planTasks()
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort&view=wide", nil), ck).Body.String()
	titles := []string{"Queued", "Running", "Waiting on a human", "Waiting for image", "Waiting on the author", "In review", "Merged", "Done", "Failed", "Closed"}
	last := -1
	for _, title := range titles {
		i := strings.Index(body, ">"+title+" <span")
		if i < last {
			t.Fatalf("column %q at %d, before the column ahead of it (%d)", title, i, last)
		}
		last = i
	}
	for _, gone := range []string{">Backlog <span", ">Plan <span", ">Ready <span", "data-lane=", `class="badge"`, "/lane\"", "start-plan"} {
		if strings.Contains(body, gone) {
			t.Errorf("the wide view still has %s", gone)
		}
	}
	if !strings.Contains(body, `data-holds="queued waiting-deps"`) || !strings.Contains(body, `data-holds="running closing"`) {
		t.Errorf("Queued does not hold waiting-deps, or Running closing:\n%s", body)
	}
	code := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ck).Body.String()
	for title, want := range map[string][]string{
		"Queued":       {`title="card 2"`, `title="card 3"`, `title="card 4"`, `<span class="chip waiting">waiting on #3, #5</span>`, "Another spec"},
		"Done":         {"Link shortener", `title="card 5"`},
		"code Queued":  {"an improvement"},
		"code Running": {"fix the cache"},
	} {
		board := body
		if name, ok := strings.CutPrefix(title, "code "); ok {
			title, board = name, code
		}
		c := col(t, board, title)
		for _, w := range want {
			if !strings.Contains(c, w) {
				t.Errorf("column %s lacks %q:\n%s", title, w, c)
			}
		}
	}
	// the plan's three cards still in play wear the chip with their keys;
	// its done card's row has none, and its drawer names the plan
	if got := strings.Count(body, `<span class="chip plan">from plan #1 · k`); got != 3 {
		t.Errorf("%d cards read from plan #1 with their key, want its 3 cards in play", got)
	}
	if d := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-c5/drawer?project=linkshort", nil), ck).Body.String(); !strings.Contains(d, `part of plan <span class="ref-plain">#1</span> · k5`) {
		t.Errorf("the done card's drawer does not name its plan and key:\n%s", d)
	}
	if !strings.Contains(body, `<option value="linkshort" selected>linkshort</option>`) || strings.Contains(body, "every project") {
		t.Errorf("board lacks the project select, or offers every project")
	}
	if strings.Contains(code, "card 2") || strings.Contains(code, "Link shortener") || !strings.Contains(code, "fix the cache") {
		t.Errorf("the code project's board shows another project's cards, or not its own")
	}
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/board?project=nope", nil), ck); rec.Code != http.StatusNotFound {
		t.Errorf("unknown project: %d, want 404", rec.Code)
	}
}

// A plan's card can name a plan the board's own read does not list: one
// of another project, open (before roadmap 269 the board read it among
// every project's cards), or a finished one. The card's chip still names
// the plan's issue — planIssue reads the parent itself — while the board
// shows none of the other project's cards.
func TestBoardPlanParentOutsideItsCards(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "queued", Track: "plan", Process: "spec", Objective: "Link shortener",
			Repository: "sf/code", IssueNumber: 1}},
		{Task: factoryd.Task{ID: "t-c", Kind: "agent", Status: "running", Track: "direct", Process: "spec", Objective: "build the API",
			Repository: "linkshort/linkshort", IssueNumber: 2, Source: factoryd.SourcePlan, Parent: "t-plan", PlanKey: "k2"}},
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	if !strings.Contains(body, `<span class="chip plan">from plan #1 · k2</span>`) {
		t.Errorf("the card's chip does not name its plan and key, a card of another project:\n%s", body)
	}
	if strings.Contains(body, "Link shortener") {
		t.Errorf("the board shows the other project's plan card:\n%s", body)
	}
	if d := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-c/drawer?tab=overview&project=linkshort", nil), ck).Body.String(); !strings.Contains(d, `part of plan <span class="ref-plain">#1</span> · k2`) {
		t.Errorf("the drawer does not name the plan and key either:\n%s", d)
	}
	code := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()
	if !strings.Contains(code, "Link shortener") || strings.Contains(code, "build the API") {
		t.Errorf("the code project's board does not show its own plan card, or shows linkshort's card:\n%s", code)
	}
}

// planRevision is a stored plan of three cards; factoryd's edges make the
// links API depend on the foundation and the integration on both.
func planRevision(task string, n int, sha string) factoryd.PlanRevision {
	plan, _ := json.Marshal(factoryd.Plan{Summary: "Foundation first, then the API, then the acceptance tests.", Cards: []factoryd.PlanCard{
		{Key: "integration", Role: "integration", Title: "Acceptance tests", Size: "small"},
		{Key: "foundation", Role: "foundation", Title: "Foundation", Size: "medium"},
		{Key: "links-api", Role: "feature", Title: "Links API", Size: "medium"},
	}})
	return factoryd.PlanRevision{TaskID: task, Revision: n, Plan: plan, SHA256: sha}
}

// Roadmap 225, the plan card: while a revision waits for the answers to
// its open questions, Needs you shows the summary, the questions and
// Request changes, and says what happens next; it offers no Accept and
// no Reject, to an approver or a writer, and their routes are gone. A
// plan at its session cap reads the cap line on the card alone.
func TestPlanCardOffersNoAcceptOrReject(t *testing.T) {
	r := newRig(t)
	planRig(r)
	r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-plan 2": {{Text: "How long are the results kept?", Choices: []string{"7 days", "30 days"}}}}
	ck, s := r.loggedIn(t)
	board := func(ck *http.Cookie, view string) string {
		return r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort"+view, nil), ck).Body.String()
	}
	card := col(t, board(ck, ""), "Needs you")
	for _, want := range []string{
		`<article class="card approval plan" data-card="t-plan"`,
		`<a href="#card=t-plan" class="open">Plan</a> <a class="ref" href="https://git.example/linkshort/linkshort/issues/1" target="_blank" rel="noopener">#1</a>, revision 2`,
		"Foundation first, then the API, then the acceptance tests.",
		"<legend>1. How long are the results kept?</legend>",
		`<p class="meta">1 question open. The answers go to the planner; its next revision with no open question files the cards.</p>`,
		`hx-post="/tasks/t-plan/plan/changes" hx-target="#board"`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("plan card lacks %q:\n%s", want, card)
		}
	}
	for _, gone := range []string{"/plan/accept", "/plan/reject", "Accept plan", ">Reject<", "may accept a plan", "sha256"} {
		if strings.Contains(card, gone) {
			t.Errorf("the plan card offers %q:\n%s", gone, card)
		}
	}
	// a plan at the session cap reads the cap line on its card, which
	// Request changes answers: no second card in Escalations
	requested := time.Now().Add(-time.Hour)
	r.approvals.rows[len(r.approvals.rows)-1].CreatedAt = requested
	r.tasks.tasks[0].Escalation, r.tasks.tasks[0].EscalatedAt = "limit reached: 3 sessions", requested.Add(time.Minute)
	wide := board(ck, "&view=wide")
	if !strings.Contains(col(t, wide, "Approvals"), `<p class="reason">limit reached: 3 sessions</p>`) ||
		!strings.Contains(wide, `Escalations <span class="count">0</span>`) {
		t.Errorf("the plan's escalation is not on its plan card alone:\n%s", wide)
	}
	// the two choices, the own answer and Request changes keep the filter
	if filtered := col(t, wide, "Approvals"); strings.Count(filtered,
		`<input type="hidden" name="project" value="linkshort"><input type="hidden" name="view" value="wide">`) != 4 {
		t.Errorf("the plan card's four forms do not keep the filter:\n%s", filtered)
	}
	r.tasks.tasks[0].Escalation, r.tasks.tasks[0].EscalatedAt = "", time.Time{}

	r.repoWriters["linkshort/linkshort"] = append(r.repoWriters["linkshort/linkshort"], "alice")
	r.forgetWriters()
	ak, as := r.as(t, "alice")
	if alice := col(t, board(ak, ""), "Needs you"); strings.Contains(alice, "/plan/accept") || strings.Contains(alice, "/plan/reject") ||
		!strings.Contains(alice, "/plan/changes") {
		t.Errorf("alice's plan card: want Request changes alone:\n%s", alice)
	}
	r.fg.calls = nil
	for _, path := range []string{"/tasks/t-plan/plan/accept", "/tasks/t-plan/plan/reject", "/tasks/t-plan/start-plan", "/cards/t-plan/lane"} {
		if rec := r.do(post(path, url.Values{"csrf": {s.csrf}, "revision": {"2"}, "sha256": {"sha-2"}, "lane": {"ready"}}), ck); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404: the route is gone", path, rec.Code)
		}
	}
	if len(r.fg.calls) != 0 {
		t.Errorf("a gone route acted: %v", r.fg.calls)
	}
	rec := r.do(post("/tasks/t-plan/plan/changes", url.Values{"csrf": {as.csrf}, "text": {"Split the web pages into two cards."}}), ak)
	if rec.Code != http.StatusSeeOther || r.fg.comments["/api/v1/repos/linkshort/linkshort/issues/1/comments"] != "Split the web pages into two cards." ||
		!slices.Equal(r.fg.calls, []string{"POST /api/v1/repos/linkshort/linkshort/issues/1/comments Bearer access-2"}) {
		t.Errorf("request changes: %d, calls %v, comments %v", rec.Code, r.fg.calls, r.fg.comments)
	}
	if q := r.approvals.get(20); q.Status != "denied" || q.DecidedBy != "alice" || q.DecisionNote != "changes requested: Split the web pages into two cards." {
		t.Errorf("the plan's request after the change request = %+v, want denied by alice", q)
	}
	if rec := r.do(post("/tasks/t-plan/plan/changes", url.Values{"csrf": {as.csrf}, "text": {" "}}), ak); rec.Code != http.StatusBadRequest {
		t.Errorf("changes without text: %d, want 400", rec.Code)
	}
}

// Roadmap 225: a done plan stays in Done with its children's rollup, its
// queued children counted, and lists them by issue and state in its
// drawer; it offers no Start plan.
func TestDonePlanRollsUpItsChildren(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	r.tasks.tasks = planTasks()
	ck, _ := r.loggedIn(t)
	done := col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String(), "Done")
	if !strings.Contains(done, "<span class=\"chip rollup\">1\u00a0done · 1\u00a0waiting · 2\u00a0queued</span>") || strings.Contains(done, "Start plan") || strings.Contains(done, "start-plan") {
		t.Errorf("the plan in Done: want its rollup and no Start plan:\n%s", done)
	}
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-plan/drawer?project=linkshort", nil), ck).Body.String()
	for _, want := range []string{
		`<li><a href="https://git.example/linkshort/linkshort/issues/2" target="_blank" rel="noopener">#2</a> queued</li>`,
		`<li><a href="https://git.example/linkshort/linkshort/issues/4" target="_blank" rel="noopener">#4</a> waiting-deps</li>`,
		`<li><a href="https://git.example/linkshort/linkshort/issues/5" target="_blank" rel="noopener">#5</a> done</li>`,
	} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the plan's drawer lacks %q:\n%s", want, drawer)
		}
	}
}

// failedCard is card #7 of plan t-plan, failed.
func failedCard() factoryd.BoardTask {
	return factoryd.BoardTask{Task: factoryd.Task{ID: "t-c7", Kind: "agent", Status: "failed", Track: "direct", Process: "spec", Objective: "card 7",
		Repository: "linkshort/linkshort", IssueNumber: 7, Source: factoryd.SourcePlan, Parent: "t-plan", PlanKey: "k7"}}
}

// Item 119: a failed card of a plan that holds its key shows Retry to an
// approver, and so does one whose retry stopped before its replacement
// got an issue; a done card, a failed card retried (its replacement on an
// issue) and a failed task of no plan show none, nor does any card to a
// writer who is no approver.
func TestFailedPlanCardShowsRetry(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	retried, stopped := failedCard(), failedCard()
	retried.ID, retried.IssueNumber, retried.PlanKey = "t-c8", 8, ""
	stopped.ID, stopped.IssueNumber, stopped.PlanKey = "t-c9", 9, ""
	replacement := func(id string, n int64) factoryd.BoardTask {
		return factoryd.BoardTask{Task: factoryd.Task{ID: id, Kind: "agent", Status: "queued", Track: "direct", Process: "spec",
			Objective: "again", Repository: "linkshort/linkshort", IssueNumber: n, Source: factoryd.SourcePlan, Parent: "t-plan", PlanKey: "k"}}
	}
	r.tasks.tasks = append(planTasks(), failedCard(), retried, stopped, replacement("t-fcbf45070fe4", 14), replacement("t-806433097bea", 0),
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-fix", Kind: "agent",
			Status: "failed", Track: "direct", Process: "maintenance", Objective: "a fix", Repository: "sf/code", IssueNumber: 13}})
	ck, s := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String() +
		r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()
	failed := col(t, body, "Done")
	form := `<form method="post" action="/tasks/t-c7/retry" hx-post="/tasks/t-c7/retry" hx-target="#board" hx-swap="outerHTML" class="inline">` +
		`<input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value="linkshort">` +
		`<button type="submit" aria-label="Retry #7">Retry</button></form>`
	if !strings.Contains(failed, form) || !strings.Contains(failed, `action="/tasks/t-c9/retry"`) {
		t.Errorf("the failed card, or the one whose retry stopped, lacks Retry:\n%s", failed)
	}
	for _, id := range []string{"t-c5", "t-c8", "t-fix"} {
		if strings.Contains(body, "/tasks/"+id+"/retry") {
			t.Errorf("%s shows Retry", id)
		}
	}
	ak, _ := r.as(t, "alice")
	if strings.Contains(r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ak).Body.String(), "/retry") {
		t.Error("alice, no approver, sees Retry")
	}
}

// Item 119: Retry, by an approver, files the replacement (factoryd's
// RetryPlanCard), which starts once a work slot is free: the console
// labels nothing. A refusal answers 409 with its sentence; a writer who
// is no approver gets 403 and nothing happens.
func TestRetryFilesTheReplacement(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	r.tasks.tasks = append(planTasks(), failedCard())
	ck, s := r.loggedIn(t)
	ak, as := r.as(t, "alice")
	retry := func(ck *http.Cookie, csrf string) *httptest.ResponseRecorder {
		req := post("/tasks/t-c7/retry", url.Values{"csrf": {csrf}})
		req.Header.Set("HX-Request", "true")
		return r.do(req, ck)
	}
	r.fg.calls = nil
	if rec := retry(ak, as.csrf); rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != "Only the approvers of project linkshort may retry a card." ||
		len(r.retried) != 0 || len(r.fg.calls) != 0 {
		t.Fatalf("alice retries: %d %q, retried %v, forgejo %v", rec.Code, rec.Body, r.retried, r.fg.calls)
	}
	rec := retry(ck, s.csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Retried #7 as #9; it starts once a work slot is free.") {
		t.Fatalf("retry: %d %.400s", rec.Code, rec.Body)
	}
	if !slices.Equal(r.retried, []string{"t-c7 by sebastian"}) || len(r.fg.labelled) != 0 {
		t.Errorf("RetryPlanCard calls %v, labelled %v; want one call and no label", r.retried, r.fg.labelled)
	}
	r.retryErr = factoryd.ErrNoRetry("Card #7 was already retried.")
	if rec := retry(ck, s.csrf); rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != "Card #7 was already retried." {
		t.Errorf("a refused retry: %d %q", rec.Code, rec.Body)
	}
	r.retryErr = errors.New("the retry of t-c7 as t-x stopped at filing its issue: forgejo 502")
	if rec := retry(ck, s.csrf); rec.Code != http.StatusInternalServerError ||
		strings.TrimSpace(rec.Body.String()) != "The card could not be retried; see the factoryd log." {
		t.Errorf("a failed retry: %d %q", rec.Code, rec.Body)
	}
}

// Item 1131: the go choice of a card that waits on a person hands its
// work to the factory (factoryd's Go), notes the new issue on the card's
// issue and closes it; a card whose question does not offer it, and a card
// already closed, refuse and file nothing.
func TestGoHandsTheWaitingCardOver(t *testing.T) {
	r := newRig(t)
	waiting := humanAsk("t-wait", 15, factoryd.QuestionChoice{Value: "go", Label: "Have the factory do it"}, factoryd.QuestionChoice{Value: "close", Label: "Done"})
	waiting.Objective, waiting.Source = "Bump the weekly numbers.", factoryd.SourceRoutine
	nogo := humanAsk("t-nogo", 17, factoryd.QuestionChoice{Value: "close", Label: "Done"})
	r.tasks.tasks = append(planTasks(), waiting, nogo)
	ck, s := r.loggedIn(t)
	hand := func(id, csrf string) *httptest.ResponseRecorder {
		req := post("/tasks/"+id+"/answer", url.Values{"csrf": {csrf}, "value": {"go"}})
		req.Header.Set("HX-Request", "true")
		return r.do(req, ck)
	}
	if rec := hand("t-nogo", s.csrf); rec.Code != http.StatusBadRequest ||
		strings.TrimSpace(rec.Body.String()) != "Card #17 takes one of its choices: Done." || len(r.handed) != 0 {
		t.Fatalf("the card that offers no go: %d %q, handed %v", rec.Code, rec.Body, r.handed)
	}
	rec := hand("t-wait", s.csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "The factory runs it as #16; card #15 closes.") {
		t.Fatalf("the go: %d %.400s", rec.Code, rec.Body)
	}
	if !slices.Equal(r.handed, []string{"t-wait by sebastian"}) {
		t.Errorf("GoCard calls %v", r.handed)
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/15/comments"]; got != "The factory runs it as #16." {
		t.Errorf("the close note = %q", got)
	}
	if !slices.Equal(r.fg.patched, []string{`/api/v1/repos/sf/code/issues/15 {"state":"closed"}`}) {
		t.Errorf("the issue stays open: %v", r.fg.patched)
	}
	// factoryd ended the card on the close, so a second go refuses
	i := slices.IndexFunc(r.tasks.tasks, func(b factoryd.BoardTask) bool { return b.ID == "t-wait" })
	r.tasks.tasks[i].Status = "closed"
	if rec := hand("t-wait", s.csrf); rec.Code != http.StatusConflict || len(r.handed) != 1 {
		t.Errorf("the second go: %d %q, handed %v", rec.Code, rec.Body, r.handed)
	}
	r.tasks.tasks[i].Status = "waiting-human"
	r.goErr = factoryd.ErrNoGo("Card #15 was already handed over.")
	if rec := hand("t-wait", s.csrf); rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != "Card #15 was already handed over." {
		t.Errorf("a refused go: %d %q", rec.Code, rec.Body)
	}
	r.goErr = errors.New("the go of t-wait stopped at filing its issue: forgejo 502")
	if rec := hand("t-wait", s.csrf); rec.Code != http.StatusInternalServerError ||
		strings.TrimSpace(rec.Body.String()) != "The card could not be handed to the factory; see the factoryd log." {
		t.Errorf("a failed go: %d %q", rec.Code, rec.Body)
	}
}

// A revision that takes merged cards off main says which, in each view
// that lists it: the plan's approval card, its drawer and the chat's plan
// read.
func TestPlanViewsShowTakeOffMain(t *testing.T) {
	r := newRig(t)
	planRig(r)
	plan, _ := json.Marshal(factoryd.Plan{Summary: "Foundation first, then the API, then the acceptance tests.", TakeOffMain: []string{"old-api", "old-ui"},
		Cards: []factoryd.PlanCard{{Key: "foundation", Role: "foundation", Title: "Foundation", Size: "medium"}}})
	r.tasks.plans["t-plan"][1].Plan = plan
	ck, _ := r.loggedIn(t)
	want := "<p>Takes off main: old-api, old-ui</p>"
	if card := col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String(), "Needs you"); !strings.Contains(card, want) {
		t.Errorf("the plan card lacks %q:\n%s", want, card)
	}
	if d := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-plan/drawer?tab=overview&project=linkshort", nil), ck).Body.String(); !strings.Contains(d, want) {
		t.Errorf("the drawer lacks %q:\n%s", want, d)
	}
}
