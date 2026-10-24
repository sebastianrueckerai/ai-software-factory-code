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
		case rule.need&writer != 0, rule.either&(writer|approver) != 0:
			return http.StatusForbidden, "you write no project of " + subject
		case rule.need&approver != 0:
			return http.StatusForbidden, "Only the approvers of project " + project + " may " + rule.what + "."
		}
		return 0, ""
	}
	// path fills pattern's {id} (a card of sf/code (t-c), its pending request,
	// a resource, an alert, a proposal), and {project} and {name} with the
	// leg's project.
	path := func(pattern, name string) string {
		p := strings.NewReplacer("{project}", name, "{name}", name).Replace(strings.TrimPrefix(pattern, "POST "))
		id := "1"
		for _, fill := range []struct{ prefix, v string }{{"/tasks/", "t-c"}, {"/cards/", "t-c"},
			{"/approvals/", "3"}, {"/resources/", "11"}, {"/alerts/", "7"}} {
			if strings.HasPrefix(p, fill.prefix) {
				id = fill.v
			}
		}
		return strings.ReplaceAll(strings.ReplaceAll(p, "{id}", id), "{token}", "x")
	}
	changes := func() []int {
		return []int{r.events.count(), len(r.fg.calls), len(r.retried), len(r.closed), len(r.invited),
			len(r.synced), len(r.issueDeps), len(r.alerts.verdicts), len(r.resources.calls),
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
		if rule.need == 0 && rule.either == 0 {
			skipped = append(skipped, p.pattern) // the webhook checks its own header secret
			continue
		}
		if rule.need == member && rule.either == 0 {
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
	if pages != 46 || !slices.Equal(skipped, []string{"POST /messengers/telegram", "POST /notifications/telegram/code",
		"POST /notifications/disconnect", "POST /notifications/level", "POST /logout", "POST /projects/{name}/docs/{path...}"}) {
		t.Fatalf("%d POST routes driven, skipped %v: want 46 driven and only the telegram webhook, the notifications' posts, logout and the documents' save skipped", pages, skipped)
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
	if writes != 40 || len(tools) != 75 {
		t.Fatalf("%d of %d tools checked as writes: want 40 of 75", writes, len(tools))
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
		case rule.need&writer != 0, rule.either&(writer|approver) != 0:
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
			{"/approvals/", "3"}, {"/resources/", "11"}, {"/alerts/", "7"}} {
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
	if writes != 40 {
		t.Fatalf("%d write tools driven: want 40", writes)
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
