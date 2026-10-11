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
	"sync/atomic"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// withLinkshort adds the project linkshort, which follows process spec, to
// the rig's projects.
func withLinkshort(r *rig) {
	r.projects.rows = append(r.projects.rows, factoryd.Project{Name: "linkshort", Repository: "linkshort/linkshort", MergeRule: "human",
		Objects: "factory", Agents: "coder", File: true, GitToken: "t", Process: "spec", Lanes: testProcesses()["spec"].Lanes})
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
	if !slices.Contains(r.writers.users, user) {
		r.writers.users = append(r.writers.users, user)
		r.c.mu.Lock()
		r.c.writersT = time.Time{}
		r.c.mu.Unlock()
	}
	return r.loggedIn(t)
}

// planTasks are a spec's plan on linkshort, accepted and done (#1), with
// its cards: #2 and #3 in the backlog, #4 waiting on #3 and #5 in ready,
// #5 done; a second spec (#6) queued in lane plan; and a card of the code
// project running in ready (#11) and one in its backlog (#12).
func planTasks() []factoryd.BoardTask {
	child := func(id string, n int64, status, lane string) factoryd.Task {
		return factoryd.Task{ID: id, Kind: "agent", Status: status, Lane: lane, Process: "spec", Objective: fmt.Sprintf("card %d", n),
			Repository: "linkshort/linkshort", IssueNumber: n, Source: factoryd.SourcePlan, Parent: "t-plan", PlanKey: fmt.Sprintf("k%d", n)}
	}
	return []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "done", Lane: "plan", Process: "spec", Objective: "Link shortener",
			Repository: "linkshort/linkshort", IssueNumber: 1}},
		{Task: child("t-c2", 2, "backlog", "backlog")},
		{Task: child("t-c3", 3, "backlog", "backlog")},
		{Task: child("t-c4", 4, "waiting-deps", "ready"), WaitingOn: "#3, #5"},
		{Task: child("t-c5", 5, "done", "ready")},
		{Task: factoryd.Task{ID: "t-spec2", Kind: "agent", Status: "queued", Lane: "plan", Process: "spec", Objective: "Another spec",
			Repository: "linkshort/linkshort", IssueNumber: 6}},
		{Task: factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Lane: "ready", Process: "maintenance", Objective: "fix the cache",
			Repository: "sf/code", IssueNumber: 11}},
		{Task: factoryd.Task{ID: "t-b", Kind: "agent", Status: "backlog", Lane: "backlog", Objective: "an improvement",
			Repository: "sf/code", IssueNumber: 12}},
	}
}

// Section 1.1: the lanes of the shown projects' processes in the fixed
// order backlog, plan, ready, then the state columns; a starting lane's
// column holds its queued and waiting-deps cards, every later column the
// card's lane badge; a plan's card reads its plan; ?project= shows one
// project and only its processes' lanes.
func TestBoardColumnsFromLanes(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = planTasks()
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	titles := []string{"Backlog", "Plan", "Ready", "Running", "Waiting on a human", "Waiting on the author", "In review", "Done", "Failed", "Closed"}
	last := -1
	for _, title := range titles {
		i := strings.Index(body, ">"+title+" <span")
		if i < last {
			t.Fatalf("column %q at %d, before the column ahead of it (%d)", title, i, last)
		}
		last = i
	}
	for _, gone := range []string{">Queued <span", ">Suggested <span"} {
		if strings.Contains(body, gone) {
			t.Errorf("board still has %s", gone)
		}
	}
	for title, want := range map[string][]string{
		"Backlog": {"card 2", "card 3", "an improvement"},
		"Plan":    {"Another spec"},
		"Ready":   {"card 4", `<span class="chip waiting">waiting on #3, #5</span>`},
		"Running": {"fix the cache", `<span class="badge">ready</span>`},
		"Done":    {"Link shortener", `<span class="badge">plan</span>`, "card 5", `<span class="badge">ready</span>`},
	} {
		c := col(t, body, title)
		for _, w := range want {
			if !strings.Contains(c, w) {
				t.Errorf("column %s lacks %q:\n%s", title, w, c)
			}
		}
	}
	for _, title := range []string{"Backlog", "Plan", "Ready"} {
		if c := col(t, body, title); strings.Contains(c, `class="badge"`) {
			t.Errorf("lane column %s shows a lane badge:\n%s", title, c)
		}
	}
	for _, n := range []string{"2", "3", "4", "5"} {
		if !strings.Contains(body, `card `+n+`</h4>`) {
			t.Fatalf("card %s missing", n)
		}
	}
	if got := strings.Count(body, `<span class="chip plan">part of plan #1</span>`); got != 4 {
		t.Errorf("%d cards read part of plan #1, want its 4 cards", got)
	}
	if !strings.Contains(body, `<option value="linkshort">linkshort</option>`) || !strings.Contains(body, `<option value="" selected>every project</option>`) {
		t.Errorf("board lacks the project filter")
	}

	code := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()
	if strings.Contains(code, ">Plan <span") || !strings.Contains(code, ">Backlog <span") || !strings.Contains(code, ">Ready <span") {
		t.Errorf("maintenance has no lane plan: the code project's board shows exactly backlog and ready")
	}
	if strings.Contains(code, "card 2") || strings.Contains(code, "Link shortener") || !strings.Contains(code, "fix the cache") {
		t.Errorf("the code project's board shows another project's cards, or not its own")
	}
	for _, want := range []string{`<option value="ai-software-factory-code" selected>`, `data-project="ai-software-factory-code"`} {
		if !strings.Contains(code, want) {
			t.Errorf("filtered board lacks %q", want)
		}
	}
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/board?project=nope", nil), ck); rec.Code != http.StatusNotFound {
		t.Errorf("unknown project: %d, want 404", rec.Code)
	}
}

// moveForms are the lanes the move buttons of card id offer, in order.
func moveForms(body, id string) []string {
	var lanes []string
	for _, lane := range []string{"plan", "ready"} {
		if strings.Contains(body, `action="/cards/`+id+`/lane" hx-post="/cards/`+id+`/lane" hx-target="#board" hx-swap="outerHTML" class="inline move" data-move="`+lane+`"`) {
			lanes = append(lanes, lane)
		}
	}
	return lanes
}

// Section 5: a backlog card shows one button per starting lane of its
// process the viewer may use; a plan's card never offers lane plan.
func TestMoveButtonsByMovedBy(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	r.tasks.tasks = planTasks()
	ck, s := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for id, want := range map[string][]string{"t-spec2": nil, "t-c2": {"ready"}, "t-b": {"ready"}, "t-run": nil} {
		if got := moveForms(body, id); !slices.Equal(got, want) {
			t.Errorf("sebastian, %s: buttons %v, want %v", id, got, want)
		}
	}
	for _, want := range []string{
		`<form method="post" action="/cards/t-c2/lane" hx-post="/cards/t-c2/lane" hx-target="#board" hx-swap="outerHTML" class="inline move" data-move="ready">` +
			`<input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value=""><input type="hidden" name="accent" value=""><input type="hidden" name="lane" value="ready">`,
		`<button type="submit" aria-label="Move #2 to lane ready">→ Ready</button>`,
		`data-lane="backlog"`, `data-lane="plan"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("board lacks %q", want)
		}
	}
	if face := faceOf(t, body, "t-c2"); !strings.Contains(face, `draggable="true"`) {
		t.Errorf("the card with a move is not draggable:\n%s", face)
	}

	// a spec that is no plan's card may go to plan and ready
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-idea", Kind: "agent", Status: "backlog", Lane: "backlog",
		Process: "spec", Objective: "an idea", Repository: "linkshort/linkshort", IssueNumber: 8}})
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if got := moveForms(body, "t-idea"); !slices.Equal(got, []string{"plan", "ready"}) {
		t.Errorf("sebastian, t-idea: buttons %v, want [plan ready]", got)
	}

	// alice writes the code project only; ready of spec is moved by the
	// approvers, of whom she is none
	r.repoWriters["sf/code"] = []string{"sebastian", "alice"}
	spec := r.ledger.processes["spec"]
	spec.Lanes[0].MovedBy = policy.MovedByApprovers
	withLinkshortLanes(r, spec.Lanes)
	ak, _ := r.as(t, "alice")
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), ak).Body.String()
	for id, want := range map[string][]string{"t-idea": nil, "t-c2": nil, "t-b": {"ready"}} {
		if got := moveForms(body, id); !slices.Equal(got, want) {
			t.Errorf("alice, %s: buttons %v, want %v", id, got, want)
		}
	}
	if face := faceOf(t, body, "t-c2"); strings.Contains(face, "draggable") {
		t.Errorf("a card with no button is draggable:\n%s", face)
	}
	r.repoWriters["linkshort/linkshort"] = []string{"sebastian", "alice"}
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), ak).Body.String()
	if got := moveForms(body, "t-idea"); got != nil {
		t.Errorf("alice's answer is kept for a minute: buttons %v, want none", got)
	}
	r.c.moves = nil
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), ak).Body.String()
	if got := moveForms(body, "t-idea"); !slices.Equal(got, []string{"plan"}) {
		t.Errorf("alice, a writer of linkshort: buttons %v, want [plan] (ready is the approvers')", got)
	}
}

// withLinkshortLanes sets process spec's lanes in the version in force and
// on linkshort's row.
func withLinkshortLanes(r *rig, lanes []policy.Lane) {
	p := r.ledger.processes["spec"]
	p.Lanes = lanes
	r.ledger.processes["spec"] = p
	for i := range r.projects.rows {
		if r.projects.rows[i].Process == "spec" {
			r.projects.rows[i].Lanes = lanes
		}
	}
}

// Section 5: POST /cards/{id}/lane adds the lane's label to the card's
// issue with the human's own token and answers the board; every refusal
// is a 403 with one sentence and no label.
func TestMovePostAddsLabelAsHuman(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	r.tasks.tasks = append(planTasks(), factoryd.BoardTask{Task: factoryd.Task{ID: "t-idea", Kind: "agent", Status: "backlog", Lane: "backlog",
		Process: "spec", Objective: "an idea", Repository: "linkshort/linkshort", IssueNumber: 8}})
	ck, s := r.loggedIn(t)
	move := func(ck *http.Cookie, csrf, id, lane string) *httptest.ResponseRecorder {
		req := post("/cards/"+id+"/lane", url.Values{"csrf": {csrf}, "lane": {lane}, "project": {""}})
		req.Header.Set("HX-Request", "true")
		return r.do(req, ck)
	}
	for _, tc := range []struct{ id, lane, want string }{
		{"t-idea", "plan", "/api/v1/repos/linkshort/linkshort/issues/8/labels [24]"},
		{"t-c2", "ready", "/api/v1/repos/linkshort/linkshort/issues/2/labels [21]"},
		{"t-b", "ready", "/api/v1/repos/sf/code/issues/12/labels [1]"},
	} {
		r.fg.labelled, r.fg.calls = nil, nil
		rec := move(ck, s.csrf, tc.id, tc.lane)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s to %s: %d %s", tc.id, tc.lane, rec.Code, rec.Body)
		}
		if !slices.Equal(r.fg.labelled, []string{tc.want}) {
			t.Errorf("%s to %s: labelled %v, want %s", tc.id, tc.lane, r.fg.labelled, tc.want)
		}
		for _, call := range r.fg.calls {
			if !strings.HasSuffix(call, " Bearer access-1") {
				t.Errorf("%s: call %q not made with the human's token", tc.id, call)
			}
		}
		body := rec.Body.String()
		if !strings.HasPrefix(body, `<div id="board"`) || !strings.Contains(body, "leaves the backlog once factoryd picks it up") {
			t.Errorf("%s: the answer is not the board with the move's note: %.300s", tc.id, body)
		}
	}

	spec := r.ledger.processes["spec"]
	spec.Lanes[0].MovedBy = policy.MovedByApprovers
	withLinkshortLanes(r, spec.Lanes)
	r.repoWriters["sf/code"] = []string{"sebastian"}
	ak, as := r.as(t, "alice")
	for _, tc := range []struct {
		ck        *http.Cookie
		csrf      string
		id, lane  string
		want      string
		wantState int
	}{
		{ak, as.csrf, "t-b", "ready", "Lane ready takes cards from writers of sf/code.", http.StatusForbidden},
		{ak, as.csrf, "t-c2", "ready", "Lane ready takes cards from the approvers.", http.StatusForbidden},
		{ck, s.csrf, "t-c2", "plan", "Lane plan takes no card of a plan: plans do not nest.", http.StatusForbidden},
		{ck, s.csrf, "t-run", "ready", "Card #11 is not in the backlog.", http.StatusForbidden},
		{ck, s.csrf, "t-idea", "backlog", "Lane backlog is no starting lane of process spec.", http.StatusForbidden},
		{ck, s.csrf, "t-b", "plan", "Lane plan is no starting lane of process maintenance.", http.StatusForbidden},
		{ck, s.csrf, "t-none", "ready", "404 page not found", http.StatusNotFound},
	} {
		r.fg.labelled = nil
		rec := move(tc.ck, tc.csrf, tc.id, tc.lane)
		if rec.Code != tc.wantState || strings.TrimSpace(rec.Body.String()) != tc.want || len(r.fg.labelled) != 0 {
			t.Errorf("%s to %s: %d %q, labelled %v; want %d %q and no label", tc.id, tc.lane, rec.Code, rec.Body, r.fg.labelled, tc.wantState, tc.want)
		}
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

// Section 5, the plan card: while a plan waits, the Approvals list shows
// the spec's first lines, the summary and the table; Accept plan is the
// approvers' (the spend wrapper), signals the acceptance and comments it
// as the human; a stale revision shows factoryd's line on the card;
// Request changes and Reject are every writer's.
func TestAcceptPlanApproversOnly(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "waiting-human", Lane: "plan", Process: "spec",
		Objective:  "Link shortener\n\nBuild a link shortener.\nAccounts: sign up.\nLinks: shorten.\nRedirect: 302.\nStats: per day.\nQR code: a PNG.",
		Repository: "linkshort/linkshort", IssueNumber: 1}}}
	r.tasks.plans = map[string][]factoryd.PlanRevision{"t-plan": {planRevision("t-plan", 1, "sha-1"), planRevision("t-plan", 2, "sha-2")}}
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 20, Kind: factoryd.ApprovalPlan, TaskID: "t-plan",
		Project: "linkshort/linkshort", Status: "pending", Spec: json.RawMessage(`{"revision":2,"sha256":"sha-2"}`)})
	ck, s := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	card := col(t, body, "Approvals")
	for _, want := range []string{
		`Plan <a href="https://git.example/linkshort/linkshort/issues/1">#1</a>, revision 2`,
		"Link shortener\n\nBuild a link shortener.\nAccounts: sign up.\nLinks: shorten.\n…",
		"Foundation first, then the API, then the acceptance tests.",
		"<tr><td>foundation</td><td>foundation</td><td>Foundation</td><td>medium</td><td></td></tr>",
		"<tr><td>links-api</td><td>feature</td><td>Links API</td><td>medium</td><td>foundation</td></tr>",
		"<tr><td>integration</td><td>integration</td><td>Acceptance tests</td><td>small</td><td>foundation, links-api</td></tr>",
		`action="/tasks/t-plan/plan/accept"`, `<input type="hidden" name="revision" value="2">`, `<input type="hidden" name="sha256" value="sha-2">`,
		`action="/tasks/t-plan/plan/changes"`, `action="/tasks/t-plan/plan/reject"`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("plan card lacks %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, "Stats: per day.") {
		t.Error("the plan card shows the whole spec, not its first lines")
	}
	// a plan at the session cap reads the cap line on its card, which
	// Request changes answers: no second card in Escalations
	requested := time.Now().Add(-time.Hour)
	r.approvals.rows[len(r.approvals.rows)-1].CreatedAt = requested
	r.tasks.tasks[0].Escalation, r.tasks.tasks[0].EscalatedAt = "limit reached: 3 sessions", requested.Add(time.Minute)
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if !strings.Contains(col(t, body, "Approvals"), `<p class="reason">limit reached: 3 sessions</p>`) ||
		!strings.Contains(body, `Escalations <span class="count">0</span>`) {
		t.Errorf("the plan's escalation is not on its plan card alone:\n%s", body)
	}
	// a cap from before this revision's request, which a writer granted, is
	// over
	r.tasks.tasks[0].EscalatedAt = requested.Add(-time.Minute)
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if strings.Contains(body, "limit reached") || !strings.Contains(body, `Escalations <span class="count">0</span>`) {
		t.Errorf("an escalation older than the request shows:\n%s", col(t, body, "Approvals"))
	}
	r.tasks.tasks[0].Escalation, r.tasks.tasks[0].EscalatedAt = "", time.Time{}
	for _, want := range []string{`hx-post="/tasks/t-plan/plan/accept" hx-target="#board"`, `hx-post="/tasks/t-plan/plan/changes" hx-target="#board"`,
		`hx-post="/tasks/t-plan/plan/reject" hx-target="#board"`} {
		if !strings.Contains(card, want) {
			t.Errorf("plan card lacks %q", want)
		}
	}
	if filtered := col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String(), "Approvals"); strings.Count(filtered,
		`<input type="hidden" name="project" value="linkshort">`) != 3 {
		t.Errorf("the plan card's three forms do not keep the filter:\n%s", filtered)
	}

	ak, as := r.as(t, "alice")
	alice := col(t, r.do(httptest.NewRequest(http.MethodGet, "/", nil), ak).Body.String(), "Approvals")
	if strings.Contains(alice, "/plan/accept") || !strings.Contains(alice, "Only the policy's approvers may accept a plan.") ||
		!strings.Contains(alice, "/plan/changes") || !strings.Contains(alice, "/plan/reject") {
		t.Errorf("alice's plan card: accept hidden, changes and reject shown:\n%s", alice)
	}
	accept := func(ck *http.Cookie, csrf, rev, sha string) *httptest.ResponseRecorder {
		return r.do(post("/tasks/t-plan/plan/accept", url.Values{"csrf": {csrf}, "revision": {rev}, "sha256": {sha}, "project": {"linkshort"}}), ck)
	}
	r.fg.calls = nil
	if rec := accept(ak, as.csrf, "2", "sha-2"); rec.Code != http.StatusForbidden || len(r.fg.calls) != 0 || len(r.fg.comments) != 0 {
		t.Fatalf("alice accepts: %d %s, calls %v; want 403 and nothing done", rec.Code, rec.Body, r.fg.calls)
	}

	rec := accept(ck, s.csrf, "1", "sha-1")
	if rec.Code != http.StatusConflict || len(r.fg.calls) != 0 || len(r.fg.comments) != 0 {
		t.Fatalf("a stale revision: %d, calls %v, comments %v; want 409 and nothing done", rec.Code, r.fg.calls, r.fg.comments)
	}
	if c := col(t, rec.Body.String(), "Approvals"); !strings.Contains(c, `<p class="problem" role="alert">the plan changed to revision 2; reload</p>`) {
		t.Errorf("the stale line is not on the plan card:\n%s", c)
	}
	req := post("/tasks/t-plan/plan/accept", url.Values{"csrf": {s.csrf}, "revision": {"1"}, "sha256": {"sha-1"}})
	req.Header.Set("HX-Request", "true")
	if rec := r.do(req, ck); rec.Code != http.StatusConflict || !strings.HasPrefix(rec.Body.String(), `<div id="board"`) ||
		!strings.Contains(rec.Body.String(), "the plan changed to revision 2; reload") {
		t.Errorf("a stale revision by htmx: %d, want 409 and the board with the line: %.200s", rec.Code, rec.Body)
	}

	// a browser's form: a redirect to the filtered board, so a reload
	// resends nothing
	rec = accept(ck, s.csrf, "2", "sha-2")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?project=linkshort" {
		t.Fatalf("accept: %d to %q %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if !slices.Contains(r.fg.calls, "SIGNAL plan_accepted t-plan 2 sha-2 by sebastian") ||
		!slices.Contains(r.fg.calls, "POST /api/v1/repos/linkshort/linkshort/issues/1/comments Bearer access-1") ||
		r.fg.comments["/api/v1/repos/linkshort/linkshort/issues/1/comments"] != "Plan revision 2 accepted." {
		t.Errorf("accept: calls %v, comments %v", r.fg.calls, r.fg.comments)
	}

	r.fg.calls, r.fg.comments = nil, map[string]string{}
	rec = r.do(post("/tasks/t-plan/plan/changes", url.Values{"csrf": {as.csrf}, "text": {"Split the web pages into two cards."}}), ak)
	if rec.Code != http.StatusSeeOther || r.fg.comments["/api/v1/repos/linkshort/linkshort/issues/1/comments"] != "Split the web pages into two cards." ||
		!slices.Equal(r.fg.calls, []string{"POST /api/v1/repos/linkshort/linkshort/issues/1/comments Bearer access-2"}) {
		t.Errorf("request changes: %d, calls %v, comments %v", rec.Code, r.fg.calls, r.fg.comments)
	}
	r.fg.calls = nil
	rec = r.do(post("/tasks/t-plan/plan/reject", url.Values{"csrf": {as.csrf}, "note": {"not this quarter"}}), ak)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" ||
		!slices.Equal(r.fg.calls, []string{"SIGNAL plan_rejected t-plan by alice: not this quarter"}) {
		t.Errorf("reject: %d, calls %v", rec.Code, r.fg.calls)
	}
	for name, f := range map[string]url.Values{"changes": {"text": {" "}}, "reject": {"note": {""}}} {
		f.Set("csrf", as.csrf)
		if rec := r.do(post("/tasks/t-plan/plan/"+name, f), ak); rec.Code != http.StatusBadRequest {
			t.Errorf("%s without text: %d, want 400", name, rec.Code)
		}
	}
}

// Item 141: a Start plan posted seconds after Accept, while factoryd is
// still filing the plan's cards, waits for the plan task to turn done
// instead of answering 409; a plan that never turns done gets 409 with
// why, and a non-plan still answers at once.
func TestStartPlanWaitsForTheCardsToBeFiled(t *testing.T) {
	wait, poll := startPlanWait, startPlanPoll
	startPlanWait, startPlanPoll = 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { startPlanWait, startPlanPoll = wait, poll })

	r := newRig(t)
	withLinkshort(r)
	ck, s := r.loggedIn(t)
	start := func(id string) *httptest.ResponseRecorder {
		req := post("/tasks/"+id+"/start-plan", url.Values{"csrf": {s.csrf}})
		req.Header.Set("HX-Request", "true")
		return r.do(req, ck)
	}

	// the plan is accepted but not yet final when start-plan first reads it
	plan := planTasks()
	for i := range plan {
		if plan[i].ID == "t-plan" {
			plan[i].Status = "running"
		}
	}
	r.tasks.tasks = plan
	var reads int32
	// factoryd files the cards and ends the plan on the third read of it:
	// after Accept nothing had happened yet, so start-plan's first read
	// finds the plan running.
	r.tasks.read = func(s *fakeTasks, id string) {
		if id == "t-plan" && atomic.AddInt32(&reads, 1) == 3 {
			s.setStatus("t-plan", "done")
		}
	}
	r.fg.calls = nil
	rec := start("t-plan")
	if rec.Code != http.StatusOK {
		t.Fatalf("start plan while the cards are being filed: %d %.300s", rec.Code, rec.Body)
	}
	if got := atomic.LoadInt32(&reads); got != 3 {
		t.Errorf("%d reads of the plan, want the first, two polls and no more", got)
	}
	if want := []string{"/api/v1/repos/linkshort/linkshort/issues/2/labels [21]", "/api/v1/repos/linkshort/linkshort/issues/3/labels [21]"}; !slices.Equal(r.fg.labelled, want) {
		t.Errorf("labelled %v, want %v", r.fg.labelled, want)
	}

	// a plan whose cards are never filed: 409 with the reason, nothing labelled
	r.tasks.read, r.fg.labelled = nil, nil
	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].ID == "t-plan" {
			r.tasks.tasks[i].Status = "running"
		}
	}
	before := time.Now()
	if rec := start("t-plan"); rec.Code != http.StatusConflict ||
		strings.TrimSpace(rec.Body.String()) != `The plan's cards are still being filed; try again in a moment.` ||
		len(r.fg.labelled) != 0 {
		t.Errorf("a plan that never turns done: %d %q, labelled %v", rec.Code, rec.Body, r.fg.labelled)
	}
	if d := time.Since(before); d < startPlanWait {
		t.Errorf("the wait gave up after %v, want the whole %v", d, startPlanWait)
	}

	// a card of another lane is refused without waiting
	before = time.Now()
	if rec := start("t-run"); rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != "Task t-run is no accepted plan." {
		t.Errorf("start plan on a card: %d %q", rec.Code, rec.Body)
	}
	if d := time.Since(before); d >= startPlanPoll {
		t.Errorf("a card waited %v, want no wait at all", d)
	}

	// a failed plan is not an accepted one: 409 at once
	r.tasks.read = nil
	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].ID == "t-plan" {
			r.tasks.tasks[i].Status = "failed"
		}
	}
	before = time.Now()
	if rec := start("t-plan"); rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != "Task t-plan is no accepted plan." {
		t.Errorf("start plan on a failed plan: %d %q", rec.Code, rec.Body)
	}
	if d := time.Since(before); d >= startPlanPoll {
		t.Errorf("a failed plan waited %v, want no wait at all", d)
	}
}

// Section 5: an accepted plan stays in Done with its cards by issue and
// state, and Start plan (n in backlog) for approvers while n > 0, which
// labels each backlog card agent as the human and answers the count.
func TestStartPlanLabelsBacklogChildren(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	r.tasks.tasks = planTasks()
	ck, s := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	done := col(t, body, "Done")
	for _, want := range []string{
		`<li><a href="https://git.example/linkshort/linkshort/issues/2">#2</a> backlog</li>`,
		`<li><a href="https://git.example/linkshort/linkshort/issues/4">#4</a> waiting-deps</li>`,
		`<li><a href="https://git.example/linkshort/linkshort/issues/5">#5</a> done</li>`,
		`action="/tasks/t-plan/start-plan"`, `>Start plan (2 in backlog)</button>`,
	} {
		if !strings.Contains(done, want) {
			t.Errorf("the plan in Done lacks %q:\n%s", want, done)
		}
	}
	ak, as := r.as(t, "alice")
	if strings.Contains(r.do(httptest.NewRequest(http.MethodGet, "/", nil), ak).Body.String(), "Start plan") {
		t.Error("alice, no approver, sees Start plan")
	}
	start := func(ck *http.Cookie, csrf, id string) *httptest.ResponseRecorder {
		req := post("/tasks/"+id+"/start-plan", url.Values{"csrf": {csrf}})
		req.Header.Set("HX-Request", "true")
		return r.do(req, ck)
	}
	r.fg.calls = nil
	if rec := start(ak, as.csrf, "t-plan"); rec.Code != http.StatusForbidden || len(r.fg.labelled) != 0 {
		t.Fatalf("alice starts the plan: %d, labelled %v", rec.Code, r.fg.labelled)
	}
	rec := start(ck, s.csrf, "t-plan")
	if rec.Code != http.StatusOK {
		t.Fatalf("start plan: %d %s", rec.Code, rec.Body)
	}
	want := []string{"/api/v1/repos/linkshort/linkshort/issues/2/labels [21]", "/api/v1/repos/linkshort/linkshort/issues/3/labels [21]"}
	if !slices.Equal(r.fg.labelled, want) {
		t.Errorf("labelled %v, want %v", r.fg.labelled, want)
	}
	for _, call := range r.fg.calls {
		if !strings.HasSuffix(call, " Bearer access-1") {
			t.Errorf("call %q not made with the human's token", call)
		}
	}
	if !strings.Contains(rec.Body.String(), "Start plan: 2 cards of plan #1 labelled agent.") {
		t.Errorf("the answer lacks the count: %.400s", rec.Body)
	}

	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].Status == "backlog" && r.tasks.tasks[i].Parent == "t-plan" {
			r.tasks.tasks[i].Status, r.tasks.tasks[i].Lane = "queued", "ready"
		}
	}
	if done := col(t, r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(), "Done"); strings.Contains(done, "Start plan") {
		t.Errorf("Start plan shown with no card in the backlog:\n%s", done)
	}
	r.fg.labelled = nil
	if rec := start(ck, s.csrf, "t-plan"); rec.Code != http.StatusOK || len(r.fg.labelled) != 0 ||
		!strings.Contains(rec.Body.String(), "Start plan: 0 cards of plan #1 labelled agent.") {
		t.Errorf("start with none in the backlog: %d, labelled %v", rec.Code, r.fg.labelled)
	}
	if rec := start(ck, s.csrf, "t-run"); rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != "Task t-run is no accepted plan." {
		t.Errorf("start plan on a card: %d %q", rec.Code, rec.Body)
	}
}

// Review I1: the board asks MayMove once per repository, user and
// moved_by, a failure included, and keeps the answer a minute: a render
// with several backlog cards and the polls after it do not stall on
// Forgejo, and the buttons stay hidden while the check fails.
func TestBoardAsksWhoMayMoveOncePerRepository(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	r.tasks.tasks = append(planTasks(), factoryd.BoardTask{Task: factoryd.Task{ID: "t-idea", Kind: "agent", Status: "backlog", Lane: "backlog",
		Process: "spec", Objective: "an idea", Repository: "linkshort/linkshort", IssueNumber: 8}})
	ck, _ := r.loggedIn(t)
	r.mayErr = errors.New("forgejo: 502")
	var body string
	for range 3 {
		body = r.do(httptest.NewRequest(http.MethodGet, "/board", nil), ck).Body.String()
	}
	want := []string{"linkshort/linkshort sebastian writers", "sf/code sebastian writers"}
	if got := slices.Sorted(slices.Values(r.mayAsked)); !slices.Equal(got, want) {
		t.Errorf("MayMove asked %v, want once each: %v", got, want)
	}
	if strings.Contains(body, `class="inline move"`) || strings.Contains(body, "Start plan") ||
		!strings.Contains(body, "who may move a card could not be checked: the move buttons are hidden") {
		t.Errorf("a failed check shows buttons or no problem line:\n%s", body)
	}
	r.mayErr, r.mayAsked = nil, nil
	r.c.mu.Lock()
	for k, a := range r.c.moves {
		a.at = a.at.Add(-writersTTL)
		r.c.moves[k] = a
	}
	r.c.mu.Unlock()
	body = r.do(httptest.NewRequest(http.MethodGet, "/board", nil), ck).Body.String()
	if len(r.mayAsked) != 2 || moveForms(body, "t-c2") == nil {
		t.Errorf("after a minute: asked %v, buttons %v; want asked again and the buttons back", r.mayAsked, moveForms(body, "t-c2"))
	}
}

// Review M1, M2: Start plan shows only to an approver who may move the
// plan's cards into ready; a card's lanes are its project's current
// process, as intake reads them, whatever the row names.
func TestStartPlanAndLanesFollowTheProject(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	r.tasks.tasks = append(planTasks(), factoryd.BoardTask{Task: factoryd.Task{ID: "t-old", Kind: "agent", Status: "backlog", Lane: "backlog",
		Process: "maintenance", Objective: "filed under maintenance", Repository: "linkshort/linkshort", IssueNumber: 13}})
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if got := moveForms(body, "t-old"); !slices.Equal(got, []string{"plan", "ready"}) {
		t.Errorf("a card whose row names maintenance on a spec project: buttons %v, want [plan ready]", got)
	}
	if !strings.Contains(body, "Start plan (2 in backlog)") {
		t.Fatal("sebastian, an approver and a writer, lacks Start plan")
	}
	r.repoWriters["linkshort/linkshort"] = nil
	r.c.moves = nil
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if strings.Contains(body, "Start plan") {
		t.Error("an approver who does not write linkshort sees Start plan, which ready's moved_by refuses him")
	}
}

// failedCard is card #7 of plan t-plan, failed.
func failedCard() factoryd.BoardTask {
	return factoryd.BoardTask{Task: factoryd.Task{ID: "t-c7", Kind: "agent", Status: "failed", Lane: "ready", Process: "spec", Objective: "card 7",
		Repository: "linkshort/linkshort", IssueNumber: 7, Source: factoryd.SourcePlan, Parent: "t-plan", PlanKey: "k7"}}
}

// Item 119: a failed card of a plan that holds its key shows Retry to an
// approver who may move cards into ready, and so does one whose retry
// stopped before its replacement got an issue; a done card, a failed card
// retried (its replacement on an issue) and a failed task of no plan show
// none, nor does any card to a writer who is no approver or to an
// approver who does not write the project.
func TestFailedPlanCardShowsRetry(t *testing.T) {
	r := newRig(t)
	withLinkshort(r)
	retried, stopped := failedCard(), failedCard()
	retried.ID, retried.IssueNumber, retried.PlanKey = "t-c8", 8, ""
	stopped.ID, stopped.IssueNumber, stopped.PlanKey = "t-c9", 9, ""
	replacement := func(id string, n int64) factoryd.BoardTask {
		return factoryd.BoardTask{Task: factoryd.Task{ID: id, Kind: "agent", Status: "backlog", Lane: "backlog", Process: "spec",
			Objective: "again", Repository: "linkshort/linkshort", IssueNumber: n, Source: factoryd.SourcePlan, Parent: "t-plan", PlanKey: "k"}}
	}
	r.tasks.tasks = append(planTasks(), failedCard(), retried, stopped, replacement("t-fcbf45070fe4", 14), replacement("t-806433097bea", 0),
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-fix", Kind: "agent",
			Status: "failed", Lane: "ready", Process: "maintenance", Objective: "a fix", Repository: "sf/code", IssueNumber: 13}})
	ck, s := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	failed := col(t, body, "Failed")
	form := `<form method="post" action="/tasks/t-c7/retry" hx-post="/tasks/t-c7/retry" hx-target="#board" hx-swap="outerHTML" class="inline">` +
		`<input type="hidden" name="csrf" value="` + s.csrf + `"><input type="hidden" name="project" value="">` +
		`<input type="hidden" name="accent" value=""><button type="submit" aria-label="Retry #7">Retry</button></form>`
	if !strings.Contains(failed, form) || !strings.Contains(failed, `action="/tasks/t-c9/retry"`) {
		t.Errorf("the failed card, or the one whose retry stopped, lacks Retry:\n%s", failed)
	}
	for _, id := range []string{"t-c5", "t-c8", "t-fix"} {
		if strings.Contains(body, "/tasks/"+id+"/retry") {
			t.Errorf("%s shows Retry", id)
		}
	}
	ak, _ := r.as(t, "alice")
	if strings.Contains(r.do(httptest.NewRequest(http.MethodGet, "/", nil), ak).Body.String(), "/retry") {
		t.Error("alice, no approver, sees Retry")
	}
	r.repoWriters["linkshort/linkshort"] = nil
	r.c.moves = nil
	ck, _ = r.as(t, "sebastian")
	if strings.Contains(r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(), "/retry") {
		t.Error("an approver who does not write linkshort sees Retry, which ready's moved_by refuses him")
	}
}

// Item 119: Retry, by an approver, files the replacement (factoryd's
// RetryPlanCard) and labels its issue agent as the human, so intake starts
// it in ready; a refusal answers 409 with its sentence and labels nothing;
// a writer who is no approver gets 403 and nothing happens.
func TestRetryLabelsTheReplacement(t *testing.T) {
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
	if rec := retry(ak, as.csrf); rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != "Only the policy's approvers may authorize spend." ||
		len(r.retried) != 0 || len(r.fg.calls) != 0 {
		t.Fatalf("alice retries: %d %q, retried %v, forgejo %v", rec.Code, rec.Body, r.retried, r.fg.calls)
	}
	r.repoWriters["linkshort/linkshort"] = nil
	if rec := retry(ck, s.csrf); rec.Code != http.StatusForbidden ||
		strings.TrimSpace(rec.Body.String()) != "Lane ready takes cards from writers of linkshort/linkshort." || len(r.retried) != 0 {
		t.Fatalf("an approver who does not write linkshort retries: %d %q, retried %v", rec.Code, rec.Body, r.retried)
	}
	r.repoWriters["linkshort/linkshort"] = []string{"sebastian"}
	rec := retry(ck, s.csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Retried #7 as #9; it starts in lane ready once factoryd picks it up.") {
		t.Fatalf("retry: %d %.400s", rec.Code, rec.Body)
	}
	if !slices.Equal(r.retried, []string{"t-c7 by sebastian"}) {
		t.Errorf("RetryPlanCard calls %v", r.retried)
	}
	if want := []string{"/api/v1/repos/linkshort/linkshort/issues/9/labels [21]"}; !slices.Equal(r.fg.labelled, want) {
		t.Errorf("labelled %v, want %v", r.fg.labelled, want)
	}
	for _, call := range r.fg.calls {
		if !strings.HasSuffix(call, " Bearer access-1") {
			t.Errorf("call %q not made with sebastian's token", call)
		}
	}
	r.fg.labelled = nil
	r.retryErr = factoryd.ErrNoRetry("Card #7 was already retried.")
	if rec := retry(ck, s.csrf); rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != "Card #7 was already retried." ||
		len(r.fg.labelled) != 0 {
		t.Errorf("a refused retry: %d %q, labelled %v", rec.Code, rec.Body, r.fg.labelled)
	}
	r.retryErr = errors.New("the retry of t-c7 as t-x stopped at filing its issue: forgejo 502")
	if rec := retry(ck, s.csrf); rec.Code != http.StatusInternalServerError ||
		strings.TrimSpace(rec.Body.String()) != "The card could not be retried; see the factoryd log." || len(r.fg.labelled) != 0 {
		t.Errorf("a failed retry: %d %q, labelled %v", rec.Code, rec.Body, r.fg.labelled)
	}
}
