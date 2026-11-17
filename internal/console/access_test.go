package console

import (
	"context"
	"errors"
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

// The access table guards every write (roadmap 185): each POST route and
// each write tool refuses a member who lacks the table's role in the
// role's words — the page by its status and body, the tool by the same
// refusal — and nothing underneath runs: no event row, no Forgejo call,
// no recorded action. bob writes demo only, so the factory's pages, its
// code's cards and its approval requests refuse him; alice writes the
// factory's code only, so demo's chat and its exports refuse her.
func TestAccessTableRefusesEveryWrite(t *testing.T) {
	r, _, _ := newChatRig(t)
	members(r)
	r.routines.views = []factoryd.RoutineView{weeklyBump()}
	bk, bs := memberLogin(t, r, "bob")
	ak, as := memberLogin(t, r, "alice")

	// words is the refusal the rule's roles owe: the factory's writers'
	// page words, the writer's subject, or the approver's action.
	words := func(rule accessRule, subject, project string) (int, string) {
		switch {
		case rule.need&factoryWriter != 0:
			return http.StatusForbidden, "Only the writers of the factory's own code see the factory's pages."
		case rule.need&writer != 0:
			return http.StatusForbidden, "you write no project of " + subject
		case rule.need&approver != 0:
			return http.StatusForbidden, "Only the approvers of project " + project + " may " + rule.what + "."
		}
		return 0, ""
	}
	// path fills pattern's {id} (a card of sf/code (t-c), its pending request,
	// a resource, a proposal), and {project} and {name} with the
	// leg's project.
	path := func(pattern, name string) string {
		p := strings.NewReplacer("{project}", name, "{name}", name).Replace(strings.TrimPrefix(pattern, "POST "))
		id := "1"
		for _, fill := range []struct{ prefix, v string }{{"/tasks/", "t-c"}, {"/cards/", "t-c"},
			{"/approvals/", "3"}, {"/resources/", "11"}} {
			if strings.HasPrefix(p, fill.prefix) {
				id = fill.v
			}
		}
		return strings.ReplaceAll(strings.ReplaceAll(p, "{id}", id), "{token}", "x")
	}
	changes := func() []int {
		return []int{r.events.count(), len(r.fg.calls), len(r.retried), len(r.closed), len(r.invited),
			len(r.synced), len(r.issueDeps), len(r.resources.calls),
			len(r.routines.skips), len(r.routines.ran), len(r.routines.needsMe)}
	}
	before := changes()

	// The pages: every POST route whose rule goes beyond the login, by the
	// member its scope refuses — alice for demo's project routes, bob for
	// the factory's and the code's cards and requests. The login-only
	// routes are accounted for by name, so a route lost or re-ruled fails.
	pages := 0
	skipped := []string{}
	for _, p := range r.c.postRoutes() {
		rule := accessTable[p.pattern]
		if rule.need == 0 {
			skipped = append(skipped, p.pattern) // the webhook checks its own header secret, /mcp its bearer
			continue
		}
		if rule.need == member {
			skipped = append(skipped, p.pattern)
			continue
		}
		cookie, csrf, name := bk, bs.csrf, "ai-software-factory-code"
		subject, user := "task t-c", "bob"
		if rule.scope == scApproval {
			subject = "approval request 3"
		}
		if rule.scope == scProject || rule.scope == scChat {
			cookie, csrf, name, subject, user = ak, as.csrf, "demo", "demo", "alice"
		}
		wantCode, wantWords := words(rule, subject, name)
		rec := r.do(post(path(p.pattern, name), url.Values{"csrf": {csrf}}), cookie)
		if code, body := rec.Code, strings.TrimSpace(rec.Body.String()); code != wantCode || body != wantWords {
			t.Errorf("POST %s as %s = %d %q, want %d %q", p.pattern, user, code, body, wantCode, wantWords)
		}
		pages++
	}
	// the documents' save is login-only too: its handler judges the
	// project's sight, as its read does (TestDocsReadAccess).
	if pages != 38 || !slices.Equal(skipped, []string{"POST /messengers/telegram", "POST /mcp", "POST /notifications/telegram/code",
		"POST /notifications/disconnect", "POST /notifications/level", "POST /notifications/tokens", "POST /notifications/tokens/revoke", "POST /logout", "POST /projects/{name}/docs/{path...}"}) {
		t.Fatalf("%d POST routes driven, skipped %v: want 38 driven and only the telegram webhook, /mcp, the notifications' posts, the chat tokens' posts, logout and the documents' save skipped", pages, skipped)
	}

	// The chat: every write tool by the member its scope refuses — bob on
	// the factory's turn for the factory's actions and the code's cards,
	// bob on demo's for the factory's actions, alice on demo's for its
	// chat's own actions.
	demo, _, err := r.c.projectOf(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := r.c.projectOf(context.Background(), "ai-software-factory-code")
	if err != nil {
		t.Fatal(err)
	}
	turnOf := func(s *session, p factoryd.Project) *turn {
		req := httptest.NewRequest(http.MethodPost, "/projects/"+p.Name+"/chat", nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
		return &turn{c: r.c, r: req, a: actor{s: s, repository: p.Repository, via: factoryd.ViaChat}, p: p}
	}
	writes := 0
	for _, tool := range tools {
		if !tool.write {
			continue
		}
		rule := accessTable[tool.name]
		trn, subject, project, who := turnOf(bs, code), "task t-c", "ai-software-factory-code", "bob"
		if rule.scope == scFactory {
			trn = turnOf(bs, demo)
		}
		if rule.scope == scChat {
			trn, subject, project, who = turnOf(as, demo), "demo", "demo", "alice"
		}
		wantCode, wantWords := words(rule, subject, project)
		if wantCode == 0 {
			t.Errorf("tool %s's rule holds for %s: no leg refuses it", tool.name, who)
			continue
		}
		e := trn.admitTool(tool.name, toolInput{TaskID: "t-c", ID: 11})
		var no *refusal
		if !errors.As(e, &no) || no.status != wantCode || no.msg != wantWords {
			t.Errorf("tool %s as %s = %v, want %d %q", tool.name, who, e, wantCode, wantWords)
		}
		writes++
	}
	if writes != 35 || len(tools) != 79 {
		t.Fatalf("%d of %d tools checked as writes: want 35 of 79", writes, len(tools))
	}

	// #485's case in the chat's own words: bob, who writes demo, asks for
	// the factory resource's promotion and nothing reaches the registry.
	if e := turnOf(bs, demo).admitTool("promote_resource", toolInput{ID: 11}); e == nil ||
		e.Error() != "Only the writers of the factory's own code see the factory's pages." {
		t.Errorf("promote_resource 11 as bob = %v, want the factory's page words", e)
	}

	if after := changes(); !slices.Equal(before, after) {
		t.Errorf("a refused write changed the factory: %v -> %v", before, after)
	}
}

// The completeness check fails a route or tool the access table misses,
// and an entry no route or tool has (roadmap 185): New refuses to start
// a console whose tables and the table disagree, so a route or tool
// added without its rule, or a rule left over after its route or tool
// went, cannot ship.
func TestAccessTableCatchesAMissingEntry(t *testing.T) {
	c := &Console{}
	if err := c.checkAccess(); err != nil {
		t.Fatalf("the table as committed is whole: %v", err)
	}
	forget := func(key string) func() {
		rule := accessTable[key]
		delete(accessTable, key)
		return func() { accessTable[key] = rule }
	}

	t.Cleanup(forget("POST /tasks"))
	err := c.checkAccess()
	if want := "console: route POST /tasks is not in the access table"; err == nil || err.Error() != want {
		t.Fatalf("checkAccess without POST /tasks = %v, want %q", err, want)
	}

	t.Cleanup(forget("new_card"))
	if err := c.checkAccess(); err == nil ||
		!strings.Contains(err.Error(), "console: tool new_card is not in the access table") {
		t.Fatalf("checkAccess without new_card = %v, want the tool's error", err)
	}

	accessTable["POST /ghost"] = rMember
	t.Cleanup(func() { delete(accessTable, "POST /ghost") })
	if err := c.checkAccess(); err == nil ||
		!strings.Contains(err.Error(), "console: the access table names POST /ghost, which is no route or tool") {
		t.Fatalf("checkAccess with POST /ghost = %v, want the stale entry's error", err)
	}
}

// A member holding the rule's roles passes the same dispatcher (roadmap
// 185): every POST route reaches its handler in the holder's session —
// anything but the rule's refusal — and every write tool admits the
// holder's turn. alice writes and approves the factory's code, so she
// holds the factory's, the code's cards' and its requests' rules; bob
// writes and approves demo, so he holds its project's rules.
func TestAccessTableAdmitsRoleHolders(t *testing.T) {
	r, _, _ := newChatRig(t)
	members(r)
	bk, bs := memberLogin(t, r, "bob")
	ak, as := memberLogin(t, r, "alice")

	// words is the refusal the rule's roles would name, the one answer a
	// holder must never read.
	words := func(rule accessRule, subject, project string) string {
		switch {
		case rule.need&factoryWriter != 0:
			return "Only the writers of the factory's own code see the factory's pages."
		case rule.need&writer != 0:
			return "you write no project of " + subject
		case rule.need&approver != 0:
			return "Only the approvers of project " + project + " may " + rule.what + "."
		}
		return ""
	}
	path := func(pattern, name string) string {
		p := strings.NewReplacer("{project}", name, "{name}", name).Replace(strings.TrimPrefix(pattern, "POST "))
		id := "1"
		for _, fill := range []struct{ prefix, v string }{{"/tasks/", "t-c"}, {"/cards/", "t-c"},
			{"/approvals/", "3"}, {"/resources/", "11"}} {
			if strings.HasPrefix(p, fill.prefix) {
				id = fill.v
			}
		}
		return strings.ReplaceAll(strings.ReplaceAll(p, "{id}", id), "{token}", "x")
	}

	// The pages: the member the rule's scope holds drives every POST
	// route, alice for the factory's and the code's, bob for demo's.
	for _, p := range r.c.postRoutes() {
		rule := accessTable[p.pattern]
		cookie, s, name := ak, as, "ai-software-factory-code"
		subject := "task t-c"
		if rule.scope == scApproval {
			subject = "approval request 3"
		}
		if rule.scope == scProject {
			cookie, s, name, subject = bk, bs, "demo", "demo"
		}
		rec := r.do(post(path(p.pattern, name), url.Values{"csrf": {s.csrf}}), cookie)
		if refused := words(rule, subject, name); rec.Code == http.StatusForbidden && strings.TrimSpace(rec.Body.String()) == refused {
			t.Errorf("POST %s as %s = the rule's refusal %q, want the handler", p.pattern, s.user, refused)
		}
	}

	// The chat: every write tool admits the holder's turn, alice's turn
	// over the factory's code, bob's over demo.
	demo, _, err := r.c.projectOf(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := r.c.projectOf(context.Background(), "ai-software-factory-code")
	if err != nil {
		t.Fatal(err)
	}
	turnOf := func(s *session, p factoryd.Project) *turn {
		req := httptest.NewRequest(http.MethodPost, "/projects/"+p.Name+"/chat", nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
		return &turn{c: r.c, r: req, a: actor{s: s, repository: p.Repository, via: factoryd.ViaChat}, p: p}
	}
	writes := 0
	for _, tool := range tools {
		if !tool.write {
			continue
		}
		trn, who := turnOf(as, code), "alice"
		if accessTable[tool.name].scope == scChat {
			trn, who = turnOf(bs, demo), "bob"
		}
		if e := trn.admitTool(tool.name, toolInput{TaskID: "t-c", ID: 11}); e != nil {
			t.Errorf("tool %s as %s = %v, want admitted", tool.name, who, e)
		}
		writes++
	}
	if writes != 35 {
		t.Fatalf("%d write tools driven: want 35", writes)
	}
}

// A chat task's timeline is a card of its project (the fix round of
// roadmap 185): the writer main served it to still reads it, the costs
// page's row links it, and a member who writes the project gets the
// table's writer refusal.
func TestChatTimelineIsACard(t *testing.T) {
	r, chat, _ := newChatRig(t)
	members(r)
	task, err := chat.EnsureChatTask(context.Background(), "demo", "demo/demo", 5, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	bk, _ := memberLogin(t, r, "bob")
	page := r.do(httptest.NewRequest(http.MethodGet, "/tasks/"+task.ID+"/timeline", nil), bk)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Timeline of "+task.ID) {
		t.Fatalf("demo's chat task timeline as bob, demo's writer = %d, want the page:\n%s", page.Code, page.Body)
	}
	ak, _ := memberLogin(t, r, "alice")
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/tasks/"+task.ID+"/timeline", nil), ak); rec.Code != http.StatusForbidden ||
		strings.TrimSpace(rec.Body.String()) != "you write no project of task "+task.ID {
		t.Fatalf("demo's chat task timeline as alice = %d %q, want the writer refusal", rec.Code, rec.Body)
	}
	r.ledger.costs = policy.Costs{Tasks: []policy.TaskCost{{TaskID: task.ID, EUR: 1.25}}}
	costs := r.do(httptest.NewRequest(http.MethodGet, "/costs", nil), ak)
	if costs.Code != http.StatusOK || !strings.Contains(costs.Body.String(), `href="/tasks/`+task.ID+`/timeline"`) {
		t.Fatalf("the costs page as alice = %d, want demo's chat task linked:\n%s", costs.Code, costs.Body)
	}
}

// The opened card's answers keep their doors' rules: a writer who is no
// approver sees the merge disabled beside the line that says so, and gets
// 403 for an approver's answer, on a merge case (approve_merge), on a
// card's own door (retry, replan, extend, fund) and on any case that offers
// one (replan, extend, fund on a gate stop), and nothing changes. A case's
// other actions leave out what the writer may not press.
func TestAWriterPressesNoApproversAnswer(t *testing.T) {
	r := newRig(t)
	withRequestCards(r)
	k, err := factoryd.RequestCase(r.approvals.get(4))
	if err != nil {
		t.Fatal(err)
	}
	k.ID = 90
	r.openCases = map[string][]factoryd.Case{"t-m": {k}, "t-g": {{ID: 91, TaskID: "t-g", Kind: factoryd.CaseGateStop, Status: factoryd.CaseAtHuman,
		Facts: roomyFacts(func(f *factoryd.CaseFacts) { f.Kind, f.Gate, f.Attempts = factoryd.CaseGateStop, "review", 1 })}}}
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 22, BudgetEUR: 2}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-f", Kind: "agent", Status: "failed", Repository: "sf/code", IssueNumber: 23}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 24, BudgetEUR: 2}})
	r.fg.user, r.repoWriters["sf/code"] = "alice", []string{"sebastian", "alice"}
	r.forgetWriters()
	ck, s := r.loggedIn(t)
	ask := askBlockOf(t, drawer(t, r, ck, "t-m", "overview"))
	if !strings.Contains(ask, `<p class="o-who">Only the project&#39;s approvers may choose Merge it.</p>`) ||
		!strings.Contains(ask, `<button type="button" class="pick" disabled>Merge it</button>`) || !strings.Contains(ask, `data-answer="deny_merge"`) {
		t.Errorf("a writer's merge ask lacks the line, the disabled merge or the return:\n%s", ask)
	}
	before := r.events.count()
	for _, tc := range []struct {
		id   string
		form url.Values
	}{
		{"t-m", url.Values{"case_id": {"90"}, "action": {factoryd.ActApproveMerge}}},
		{"t-f", url.Values{"action": {factoryd.ActRetry}}},
		{"t-run", url.Values{"action": {factoryd.ActReplan}, "replan.note": {"cut it smaller"}}},
		{"t-run", url.Values{"action": {factoryd.ActExtend}, "extend.sessions": {"1"}}},
		{"t-run", url.Values{"action": {factoryd.ActFund}, "fund.eur": {"1.00"}}},
		{"t-g", url.Values{"case_id": {"91"}, "action": {factoryd.ActReplan}, "replan.note": {"cut it smaller"}}},
		{"t-g", url.Values{"case_id": {"91"}, "action": {factoryd.ActExtend}, "extend.sessions": {"1"}}},
		{"t-g", url.Values{"case_id": {"91"}, "action": {factoryd.ActFund}, "fund.eur": {"1.00"}}},
	} {
		tc.form.Set("csrf", s.csrf)
		if rec := r.do(post("/tasks/"+tc.id+"/resolve", tc.form), ck); rec.Code != http.StatusForbidden {
			t.Errorf("%s by a writer on %s = %d, want 403: %s", tc.form.Get("action"), tc.id, rec.Code, rec.Body)
		}
	}
	if r.approvals.get(4).Status != "pending" || r.events.count() != before || len(r.acted) != 0 {
		t.Errorf("a refused press acted: request %s, %d rows, Act %+v", r.approvals.get(4).Status, r.events.count()-before, r.acted)
	}
	if more := otherActionsOf(t, drawer(t, r, ck, "t-g", "overview")); strings.Contains(more, `data-answer="replan"`) ||
		strings.Contains(more, `data-answer="extend"`) || strings.Contains(more, `data-answer="fund"`) {
		t.Errorf("the case's other actions offer a writer an approver's action:\n%s", more)
	}
	if k, _, _ := r.tasks.Task(t.Context(), "t-run"); k.BudgetEUR != 2 || k.Status != "running" {
		t.Errorf("t-run = %s with %.2f EUR, want running with 2.00", k.Status, k.BudgetEUR)
	}
}

// More sessions for a case's cap is an approver's on every door: the
// opened card's Allow more sessions and the chat's grant_session give one
// verdict for the same human. A writer is refused on both and Act takes
// nothing; an approver reaches Act on both.
func TestBothDoorsGiveOneVerdictOnMoreSessions(t *testing.T) {
	for _, tc := range []struct {
		who  string
		acts int
	}{{"alice", 0}, {"sebastian", 2}} {
		r := newRig(t)
		r.openCases = map[string][]factoryd.Case{"t-g": {{ID: 91, TaskID: "t-g", Kind: factoryd.CaseGateStop, Status: factoryd.CaseAtHuman,
			Facts: roomyFacts(func(f *factoryd.CaseFacts) { f.Kind, f.Gate, f.Attempts = factoryd.CaseGateStop, "review", 1 })}}}
		r.tasks.tasks = append(r.tasks.tasks,
			factoryd.BoardTask{Task: factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 24, BudgetEUR: 2}})
		r.fg.user, r.repoWriters["sf/code"] = tc.who, []string{"sebastian", "alice"}
		r.forgetWriters()
		ck, s := r.loggedIn(t)
		press := r.do(post("/tasks/t-g/resolve", url.Values{"csrf": {s.csrf}, "case_id": {"91"}, "action": {factoryd.ActExtend}, "extend.sessions": {"1"}}), ck)
		_, err := runTool(t, r, "grant_session", "member", toolInput{TaskID: "t-g", Text: "Go on"})
		var no *refusal
		refused := errors.As(err, &no) && no.status == http.StatusForbidden
		switch {
		case tc.acts == 0 && (press.Code != http.StatusForbidden || !refused):
			t.Errorf("%s, a writer: the press = %d, the chat's grant = %v; want both refused with 403", tc.who, press.Code, err)
		case tc.acts > 0 && (press.Code != http.StatusSeeOther || err != nil):
			t.Errorf("%s, an approver: the press = %d: %s, the chat's grant = %v; want both taken", tc.who, press.Code, press.Body, err)
		}
		if len(r.acted) != tc.acts {
			t.Errorf("%s: Act took %+v, want %d answers", tc.who, r.acted, tc.acts)
		}
	}
}

// A viewer who may give none of an ask's answers sees its question and one
// line that names who can answer, and no button: a writer on a plan's
// criteria request, whose every answer is an approver's.
func TestAViewerWhoMayAnswerNothingSeesWhoCan(t *testing.T) {
	r := newRig(t)
	verdictRig(r)
	r.fg.user, r.repoWriters["sf/code"] = "alice", []string{"sebastian", "alice"}
	r.forgetWriters()
	ck, _ := r.loggedIn(t)
	ask := askBlockOf(t, drawer(t, r, ck, "t-plan", "overview"))
	if !strings.Contains(ask, `<p class="o-question">`) || !strings.Contains(ask, `<p class="o-who">Only the project&#39;s approvers may answer this.</p>`) ||
		strings.Contains(ask, "<button") || strings.Contains(ask, `class="answer`) {
		t.Errorf("the ask of a viewer who may answer nothing:\n%s", ask)
	}
}
