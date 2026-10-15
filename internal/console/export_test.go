package console

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// fakeMirrors are the mirror rows by project and direction and Forgejo's
// push mirrors by repository, recording every Forgejo call.
type fakeMirrors struct {
	mu    sync.Mutex
	rows  map[string]factoryd.Mirror
	push  map[string][]factoryd.PushMirror
	calls []string
	fail  error // AddPushMirror's answer
	put   error // PutMirror's answer
	setPR error // SetImportPR's answer
	// cleared are the repositories whose GitHub failures were cleared
	cleared []string
	// deletes are the DeleteMirror calls as "project direction"
	deletes []string
}

func (f *fakeMirrors) ClearGitHubFailures(_ context.Context, repository string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, repository)
	return nil
}

func (f *fakeMirrors) Mirror(_ context.Context, project, direction string) (factoryd.Mirror, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.rows[project+" "+direction]
	m.Token = ""
	return m, ok, nil
}

func (f *fakeMirrors) MirrorToken(_ context.Context, project, direction string) (factoryd.Mirror, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.rows[project+" "+direction]
	return m, ok, nil
}

func (f *fakeMirrors) PutMirror(_ context.Context, m factoryd.Mirror) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.put != nil {
		return f.put
	}
	f.rows[m.Project+" "+m.Direction] = m
	return nil
}

func (f *fakeMirrors) SetImportPR(_ context.Context, project string, pr int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setPR != nil {
		return f.setPR
	}
	m, ok := f.rows[project+" import"]
	if !ok {
		return factoryd.ErrNotFound
	}
	m.PR = pr
	f.rows[project+" import"] = m
	return nil
}

func (f *fakeMirrors) DeleteMirror(_ context.Context, project, direction string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, project+" "+direction)
	delete(f.rows, project+" "+direction)
	return nil
}

// call records one Forgejo call and changes repo's push mirrors by edit.
func (f *fakeMirrors) call(repo factoryd.Repo, call string, edit func([]factoryd.PushMirror) []factoryd.PushMirror) []factoryd.PushMirror {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	f.push[repo.String()] = edit(f.push[repo.String()])
	return slices.Clone(f.push[repo.String()])
}

func same(ms []factoryd.PushMirror) []factoryd.PushMirror { return ms }

func (f *fakeMirrors) PushMirrors(_ context.Context, repo factoryd.Repo) ([]factoryd.PushMirror, error) {
	return f.call(repo, "PushMirrors "+repo.String(), same), nil
}

func (f *fakeMirrors) AddPushMirror(_ context.Context, repo factoryd.Repo, address, username, token string) error {
	f.call(repo, "AddPushMirror "+repo.String()+" "+address+" "+username+" "+token, func(ms []factoryd.PushMirror) []factoryd.PushMirror {
		if f.fail != nil {
			return ms
		}
		return append(ms, factoryd.PushMirror{RemoteName: fmt.Sprintf("remote_mirror_%d", len(f.calls)), RemoteAddress: address})
	})
	return f.fail
}

func (f *fakeMirrors) SyncPushMirrors(_ context.Context, repo factoryd.Repo) error {
	f.call(repo, "SyncPushMirrors "+repo.String(), same)
	return nil
}

func (f *fakeMirrors) DeletePushMirror(_ context.Context, repo factoryd.Repo, name string) error {
	f.call(repo, "DeletePushMirror "+repo.String()+" "+name, func(ms []factoryd.PushMirror) []factoryd.PushMirror {
		return slices.DeleteFunc(ms, func(m factoryd.PushMirror) bool { return m.RemoteName == name })
	})
	return nil
}

func (f *fakeMirrors) took() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

const ghToken = "ghp_5ecretV4lue"

// The chat's github_export takes the remote alone and pushes with the
// login and token the projects page's form stored. Without a token its
// proposal is refused with the form's link and nothing reaches Forgejo;
// with one, Do it replaces an earlier push mirror to the remote, adds the
// new one with the stored token, syncs it, stores the row and leaves one
// github_export_set row with the remote. A token gone before Do it, a
// member who is no approver and a seed project are refused with nothing
// reaching Forgejo. No stream line, conversation row, proposal row, event
// or log line holds the token.
func TestChatGithubExport(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	r, chat, g := newChatRig(t)
	g.project = "demo-app"
	const demo = "/projects/demo-app/chat"
	r.mirrors.push["demo-app/demo-app"] = []factoryd.PushMirror{{RemoteName: "remote_mirror_old", RemoteAddress: "https://github.com/Acme/App.git"},
		{RemoteName: "remote_mirror_other", RemoteAddress: "https://github.com/acme/other.git"}}
	ck, s := r.loggedIn(t)
	before := r.events.count()
	// args is the agent's call, ran the call with the mode's default
	const args, ran = `{"remote":"acme/app"}`, `{"mode":"mirror","remote":"acme/app"}`
	tool := func(result string) string {
		return `tool {"arguments":` + ran + `,"duration_ms":0,"name":"github_export","result":"` + result + `"}`
	}
	none := "Refused: demo-app has no GitHub token for an export. Set the export's token on the project page: https://console.example/projects#export-demo-app"
	g.replies = [][]string{{call(1, "github_export", args)}, {say("Set it first.")}}
	_, events, _ := chatPost(t, r, ck, s, demo, url.Values{"message": {"export to acme/app"}})
	equal(t, "without a token", events, []string{tool(none), `text {"text":"Set it first."}`, `done {}`})
	var params any
	for _, d := range g.calls()[0]["tools"].([]any) {
		if f := d.(map[string]any)["function"].(map[string]any); f["name"] == "github_export" {
			params = f["parameters"]
		}
	}
	equal(t, "the tool's parameters", params, map[string]any{"type": "object", "required": []any{"remote"},
		"properties": map[string]any{"remote": map[string]any{"type": "string", "description": "the GitHub repository, owner/name"},
			"mode": map[string]any{"type": "string", "enum": []any{"mirror", "pull_requests"}, "default": "mirror", "description": "the export's mode"}}})
	equal(t, "Forgejo calls without a token", r.mirrors.took(), []string(nil))

	r.mirrors.rows["demo-app export"] = factoryd.Mirror{Project: "demo-app", Direction: factoryd.MirrorExport, Remote: "acme/old",
		Username: "u", Token: ghToken, State: "synced"}
	g.replies = [][]string{{call(1, "github_export", args)}}
	_, events, tokens := chatPost(t, r, ck, s, demo, url.Values{"message": {"export to acme/app"}})
	equal(t, "stream", events, []string{
		`proposal {"arguments":` + ran + `,"description":"Export to GitHub, remote acme/app, mode mirror","id":"TOKEN","tool":"github_export"}`, `done {}`})
	equal(t, "Forgejo calls before Do it", r.mirrors.took(), []string(nil))
	_, done, _ := chatPost(t, r, ck, s, demo+"/proposals/"+tokens[0], nil)
	set := "GitHub export of demo-app to acme/app is set: Forgejo pushes every branch and tag on each commit and every 8 h."
	equal(t, "Do it", done, []string{tool(set), `applied {"result":"` + set + `","tool":"github_export"}`, `done {}`})
	equal(t, "Forgejo calls", r.mirrors.took(), []string{"PushMirrors demo-app/demo-app", "DeletePushMirror demo-app/demo-app remote_mirror_old",
		"AddPushMirror demo-app/demo-app https://github.com/acme/app.git u " + ghToken, "SyncPushMirrors demo-app/demo-app"})
	equal(t, "the row", r.mirrors.rows["demo-app export"], factoryd.Mirror{Project: "demo-app",
		Direction: factoryd.MirrorExport, Remote: "acme/app", Username: "u", Token: ghToken, State: "created", Mode: factoryd.ModeMirror})
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "demo-app/demo-app", Kind: factoryd.EventGithubExportSet, Detail: map[string]any{"remote": "acme/app", "autonomy": "manual", "merge_rule": "auto", "decider": "member", "chat_task": factoryd.ChatTaskID("demo-app", time.Now())}})

	g.replies = [][]string{{call(1, "github_export", args)}}
	_, _, tokens = chatPost(t, r, ck, s, demo, url.Values{"message": {"export it again"}})
	delete(r.mirrors.rows, "demo-app export")
	_, gone, _ := chatPost(t, r, ck, s, demo+"/proposals/"+tokens[0], nil)
	equal(t, "Do it once the token is gone", gone, []string{tool(none), `done {}`})

	r.repoWriters["demo-app/demo-app"] = append(r.repoWriters["demo-app/demo-app"], "anna")
	ak, as := r.as(t, "anna")
	g.replies = [][]string{{call(1, "github_export", args)}, {say("Ask an approver.")}}
	_, refused, _ := chatPost(t, r, ak, as, demo, url.Values{"message": {"export it"}})
	equal(t, "anna's", refused, []string{tool("Refused: Only the approvers of project demo-app may export it to GitHub."),
		`text {"text":"Ask an approver."}`, `done {}`})

	g.project = ""
	g.replies = [][]string{{call(1, "github_export", args)}, {say("It is the factory's own.")}}
	_, seed, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"export the factory"}})
	equal(t, "the seed project's", seed, []string{
		tool("Refused: ai-software-factory-code is the factory's own: infra's push-mirrors CronJob mirrors it (ruling 126)"),
		`text {"text":"It is the factory's own."}`, `done {}`})
	equal(t, "Forgejo calls of the refused", r.mirrors.took(), []string(nil))
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "demo-app/demo-app", Kind: factoryd.EventGithubExportSet, Detail: map[string]any{"remote": "acme/app", "autonomy": "manual", "merge_rule": "auto", "decider": "member", "chat_task": factoryd.ChatTaskID("demo-app", time.Now())}})

	said, _ := json.Marshal([]any{chat.msgs, chat.proposals, r.events.all(), g.calls(), events, done, gone, refused, seed})
	if strings.Contains(string(said)+logs.String(), ghToken) {
		t.Fatalf("the token was kept or said:\n%s\n%s", said, logs.String())
	}
}

// The projects page shows a factory project's export and, to an approver
// alone, the form and the Remove button; the form's remote must be
// owner/name; a refused add logs Forgejo's answer without the token;
// Remove deletes the push mirror and the row with one
// github_export_removed row.
func TestProjectsPageGithubExport(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	page := func(ck *http.Cookie) string {
		return r.do(httptest.NewRequest(http.MethodGet, "/projects", nil), ck).Body.String()
	}
	form := `<form method="post" action="/projects/demo-app/export"`
	if body := page(ck); !strings.Contains(body, form) || !strings.Contains(body, "<li>GitHub export: none</li>") ||
		strings.Contains(body, `action="/projects/ai-software-factory-code/export"`) {
		t.Fatalf("an approver's /projects: want demo-app's export form and none for a seed project:\n%s", body)
	}
	if rec := r.do(post("/projects/demo-app/export", url.Values{"csrf": {s.csrf}, "remote": {"acme"}, "username": {"u"}, "token": {ghToken}}), ck); rec.Code != http.StatusBadRequest ||
		strings.TrimSpace(rec.Body.String()) != "remote must be a GitHub repository, owner/name" {
		t.Fatalf("remote acme = %d %q, want 400", rec.Code, rec.Body)
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	r.mirrors.fail = errors.New("status 500: git remote add https://u:" + ghToken + "@github.com/acme/app.git: exit 1")
	if rec := r.do(post("/projects/demo-app/export", url.Values{"csrf": {s.csrf}, "remote": {"acme/app"}, "username": {"u"}, "token": {ghToken}}), ck); rec.Code != http.StatusBadGateway ||
		strings.Contains(logs.String(), ghToken) || !strings.Contains(logs.String(), "https://u:[token]@github.com") {
		t.Fatalf("a refused add = %d, log %q: want 502 and the token out of the log", rec.Code, logs.String())
	}
	r.mirrors.fail = nil
	r.mirrors.took()
	before := r.events.count()
	if rec := r.do(post("/projects/demo-app/export", url.Values{"csrf": {s.csrf}, "remote": {"acme/app"}, "username": {"u"}, "token": {ghToken}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("export = %d %q, want 303", rec.Code, rec.Body)
	}
	equal(t, "Forgejo calls", r.mirrors.took(), []string{"PushMirrors demo-app/demo-app",
		"AddPushMirror demo-app/demo-app https://github.com/acme/app.git u " + ghToken, "SyncPushMirrors demo-app/demo-app"})
	r.mirrors.rows["demo-app export"] = factoryd.Mirror{Project: "demo-app", Direction: factoryd.MirrorExport, Remote: "acme/app", Username: "u",
		Token: ghToken, State: "failed: Authentication failed"}
	remove := `<form method="post" action="/projects/demo-app/export/remove"`
	if body := page(ck); !strings.Contains(body, "<li>GitHub export: acme/app, failed: Authentication failed "+remove) || strings.Contains(body, ghToken) {
		t.Fatalf("/projects after the export: want its state and Remove, no token:\n%s", body)
	}

	r.repoWriters["demo-app/demo-app"] = append(r.repoWriters["demo-app/demo-app"], "anna")
	ak, as := r.as(t, "anna")
	if body := page(ak); strings.Contains(body, form) || strings.Contains(body, remove) || !strings.Contains(body, "<li>GitHub export: acme/app, failed: Authentication failed</li>") {
		t.Fatalf("a writer's /projects: want the state without the form or Remove:\n%s", body)
	}
	if rec := r.do(post("/projects/demo-app/export/remove", url.Values{"csrf": {as.csrf}}), ak); rec.Code != http.StatusForbidden {
		t.Fatalf("anna's Remove = %d, want 403", rec.Code)
	}
	if rec := r.do(post("/projects/demo-app/export/remove", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("Remove = %d %q, want 303", rec.Code, rec.Body)
	}
	equal(t, "Forgejo calls of Remove", r.mirrors.took(), []string{"PushMirrors demo-app/demo-app", "DeletePushMirror demo-app/demo-app remote_mirror_2"})
	if _, ok := r.mirrors.rows["demo-app export"]; ok {
		t.Fatal("Remove kept the row")
	}
	rows := r.events.all()[before:]
	equal(t, "event kinds", []string{rows[0].Kind, rows[1].Kind}, []string{factoryd.EventGithubExportSet, factoryd.EventGithubExportRemoved})
	equal(t, "event details", []any{rows[0].Detail, rows[1].Detail, len(rows)}, []any{map[string]any{"remote": "acme/app"}, map[string]any{"remote": "acme/app"}, 2})
}

// The status line ends with the export's state word, which the chat's
// status line carries too; a project without an export shows none.
func TestStatusLineGithubExport(t *testing.T) {
	r, _, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	line := func() string {
		body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
		start := strings.Index(body, `<div id="status-line">`)
		return body[start : start+strings.Index(body[start:], `<button type="button" class="cell xp"`)]
	}
	if got := line(); strings.Contains(got, "GitHub") {
		t.Fatalf("status line without an export: %s", got)
	}
	r.mirrors.rows["ai-software-factory-code export"] = factoryd.Mirror{Project: "ai-software-factory-code", Direction: factoryd.MirrorExport,
		Remote: "acme/app", State: "synced"}
	if got := line(); !strings.HasSuffix(got, "\n<span class=\"cell gh\">GitHub: synced</span>\n") {
		t.Fatalf("status line = %q, want it to end with GitHub: synced", got)
	}
	g.replies = [][]string{{say("ok")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"how is it?"}})
	system := messages(t, g.calls()[0])[0]["content"].(string)
	if !regexp.MustCompile(`\nStatus: [^\n]*; GitHub: synced$`).MatchString(system) {
		t.Fatalf("the chat's status line lacks GitHub: synced:\n%s", system)
	}
	r.mirrors.rows["ai-software-factory-code export"] = factoryd.Mirror{Project: "ai-software-factory-code", Direction: factoryd.MirrorExport,
		Remote: "acme/app", State: "failed: auth"}
	if got := line(); !strings.HasSuffix(got, "\n<span class=\"cell gh\">GitHub: failed</span>\n") {
		t.Fatalf("status line = %q, want it to end with GitHub: failed", got)
	}
}

// The form asks for a GitHub repository and a token. With them it files
// the same project file and pull request as without, the import row put
// between them, the token for registration alone; the project_filed row
// names the source; no file, pull request, event or log line holds the
// token. A row it cannot store opens no pull request; a filing Forgejo
// refuses leaves the row; a bad source or token is refused without the
// token on the page; a form without a source drops a stale import row.
func TestNewProjectImportsFromGitHub(t *testing.T) {
	r := newRig(t)
	infra := withInfra(t, r)
	r.c.cfg.CheckProject = func(context.Context, string, []byte) ([]string, error) { return nil, nil }
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	ck, s := r.loggedIn(t)
	page := r.do(httptest.NewRequest(http.MethodGet, "/projects/new", nil), ck).Body.String()
	for _, want := range []string{`<label for="source">GitHub repository (owner/name)</label>`, `<input id="source" name="source" type="text"`,
		`<label for="token">GitHub token</label>`, `<input id="token" name="token" type="password" autocomplete="off"`} {
		if !strings.Contains(page, want) {
			t.Errorf("GET /projects/new lacks %q", want)
		}
	}
	form := func(source, token string) url.Values {
		f := linkshortForm(s.csrf)
		f.Set("source", source)
		f.Set("token", token)
		return f
	}
	today := time.Now().UTC().Format(time.DateOnly)
	if rec := r.do(post("/projects", form("acme/app", ghToken)), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /projects with a source = %d: %s", rec.Code, rec.Body.String())
	}
	want := factoryd.Mirror{Project: "linkshort", Direction: factoryd.MirrorImport, Remote: "acme/app", Token: ghToken, State: "pending", PR: 12}
	if got := r.mirrors.rows["linkshort import"]; got != want {
		t.Fatalf("import row %+v, want %+v", got, want)
	}
	if m, ok, _ := r.mirrors.Mirror(context.Background(), "linkshort", factoryd.MirrorImport); !ok || m.Token != "" {
		t.Fatalf("the handler's read answers %v, token %q", ok, m.Token)
	}
	infra.mu.Lock()
	contents, pulls := infra.contents, infra.pulls
	infra.mu.Unlock()
	if len(contents) != 1 || len(pulls) != 1 {
		t.Fatalf("%d contents and %d pull calls, want one each", len(contents), len(pulls))
	}
	files := contents[0].body["files"].([]any)
	if file, _ := base64.StdEncoding.DecodeString(files[0].(map[string]any)["content"].(string)); string(file) != linkshortFile(today) {
		t.Errorf("project file\n%s\nwant\n%s", file, linkshortFile(today))
	}
	if want := map[string]any{"head": "project-linkshort", "base": "main", "title": "Project linkshort"}; !reflect.DeepEqual(pulls[0].body, want) {
		t.Errorf("pull request %v, want %v", pulls[0].body, want)
	}
	var filed []map[string]any
	r.events.mu.Lock()
	for _, e := range r.events.rows {
		if e.Kind == factoryd.EventProjectFiled {
			filed = append(filed, e.Detail)
		}
	}
	r.events.mu.Unlock()
	if want := []map[string]any{{"name": "linkshort", "pr": int64(12), "source": "acme/app"}}; !reflect.DeepEqual(filed, want) {
		t.Errorf("project_filed rows %v, want %v", filed, want)
	}

	r.mirrors.put = errors.New("db down")
	rec := r.do(post("/projects", form("acme/other", ghToken)), ck)
	infra.mu.Lock()
	if rec.Code != http.StatusInternalServerError || len(infra.contents) != 2 || len(infra.pulls) != 1 {
		t.Errorf("an unstored row: %d, %d contents and %d pull calls; want 500, two and one", rec.Code, len(infra.contents), len(infra.pulls))
	}
	infra.contentsStatus = http.StatusUnprocessableEntity
	infra.mu.Unlock()
	r.mirrors.put = nil
	for _, f := range []url.Values{form("acme/other", ghToken), linkshortForm(s.csrf)} {
		if rec := r.do(post("/projects", f), ck); rec.Code != http.StatusBadGateway || r.mirrors.rows["linkshort import"] != want {
			t.Errorf("a refused filing with source %q: %d, import row %+v; want 502 and %+v", f.Get("source"), rec.Code, r.mirrors.rows["linkshort import"], want)
		}
	}
	infra.mu.Lock()
	infra.contentsStatus = 0
	infra.mu.Unlock()
	for _, c := range []struct{ source, token, line string }{
		{"acme", ghToken, "GitHub repository: owner/name of a repository on GitHub, or none"},
		{"acme/app", "short", "GitHub token: 8 to 255 letters, digits, _ or -"},
		{"", ghToken, "GitHub token: only with a GitHub repository"},
	} {
		rec := r.do(post("/projects", form(c.source, c.token)), ck)
		if body := rec.Body.String(); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, c.line) || strings.Contains(body, ghToken) {
			t.Errorf("source %q: %d, want 422 with %q and no token:\n%s", c.source, rec.Code, c.line, body)
		}
	}
	if r.mirrors.rows["linkshort import"] != want {
		t.Fatalf("a refused form changed the import row: %+v", r.mirrors.rows["linkshort import"])
	}
	if rec := r.do(post("/projects", linkshortForm(s.csrf)), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /projects without a source = %d", rec.Code)
	}
	if _, ok := r.mirrors.rows["linkshort import"]; ok {
		t.Error("a form without a source kept the stale import row")
	}
	infra.mu.Lock()
	sent, _ := json.Marshal([]any{infra.contents, infra.pulls})
	infra.mu.Unlock()
	r.events.mu.Lock()
	events, _ := json.Marshal(r.events.rows)
	r.events.mu.Unlock()
	if strings.Contains(logs.String()+string(sent)+string(events), ghToken) {
		t.Fatalf("the token left the row:\n%s", logs.String())
	}
}

// importForm is linkshort's form with the GitHub repository acme/app and
// its token.
func importForm(csrf string) url.Values {
	form := linkshortForm(csrf)
	form.Set("source", "acme/app")
	form.Set("token", ghToken)
	return form
}

// A failed filing takes its import row and token with it once no pull
// request of the filing can merge, and says the filing was undone: a pull
// request that does not open, or one the row cannot record, closed as the
// human, or whose branch is deleted. If neither the close nor the delete
// succeeds, the row stays, so a merge still imports, and the console names
// the pull request and the branch left. None leaves an event.
func TestFailedFilingDropsItsImport(t *testing.T) {
	r := newRig(t)
	infra := withInfra(t, r)
	r.c.cfg.CheckProject = func(context.Context, string, []byte) ([]string, error) { return nil, nil }
	ck, s := r.loggedIn(t)
	infra.branches = map[string]bool{}
	file := func(want int, says string) {
		t.Helper()
		if rec := r.do(post("/projects", importForm(s.csrf)), ck); rec.Code != want || !strings.Contains(rec.Body.String(), says) {
			t.Errorf("POST /projects = %d %q, want %d saying %q", rec.Code, rec.Body.String(), want, says)
		}
	}
	set := func(pulls, closes, branches int) {
		infra.mu.Lock()
		infra.pullsStatus, infra.closeStatus, infra.branchStatus = pulls, closes, branches
		infra.mu.Unlock()
	}
	calls := func() (closes, deletes []recorded) {
		infra.mu.Lock()
		defer infra.mu.Unlock()
		return slices.Clone(infra.closes), slices.Clone(infra.branchDeletes)
	}

	set(http.StatusUnprocessableEntity, 0, 0)
	file(http.StatusBadGateway, "Forgejo refused to open the pull request; the filing was undone; see the factoryd log")
	if m, ok := r.mirrors.rows["linkshort import"]; ok || !slices.Equal(r.mirrors.deletes, []string{"linkshort import"}) {
		t.Errorf("an unopened pull request: import row %+v (%v), deletes %q; want none, one delete", m, ok, r.mirrors.deletes)
	}

	set(0, 0, 0)
	r.mirrors.setPR = errors.New("db down")
	file(http.StatusInternalServerError, "cannot record sf/infra#12 on the GitHub import; the filing was undone")
	if closes, _ := calls(); len(closes) != 1 || closes[0].path != "/api/v1/repos/sf/infra/pulls/12" || closes[0].auth != "Bearer access-1" ||
		!reflect.DeepEqual(closes[0].body, map[string]any{"state": "closed"}) {
		t.Errorf("closes %+v, want #12 closed as the human", closes)
	}
	if m, ok := r.mirrors.rows["linkshort import"]; ok || len(r.mirrors.deletes) != 2 {
		t.Errorf("an unrecorded pull request: import row %+v (%v), deletes %q; want none, two deletes", m, ok, r.mirrors.deletes)
	}

	set(0, http.StatusForbidden, 0)
	file(http.StatusInternalServerError, "cannot record sf/infra#12 on the GitHub import; the filing was undone")
	if m, ok := r.mirrors.rows["linkshort import"]; ok || len(r.mirrors.deletes) != 3 {
		t.Errorf("an unclosed pull request on a deleted branch: import row %+v (%v), deletes %q; want none, three deletes", m, ok, r.mirrors.deletes)
	}

	set(0, http.StatusForbidden, http.StatusForbidden)
	file(http.StatusInternalServerError, "cannot record sf/infra#12 on the GitHub import; sf/infra#12 may stay open: close it; "+
		"delete branch project-linkshort to file the name again; see the factoryd log")
	want := factoryd.Mirror{Project: "linkshort", Direction: factoryd.MirrorImport, Remote: "acme/app", Token: ghToken, State: "pending"}
	if got := r.mirrors.rows["linkshort import"]; got != want || len(r.mirrors.deletes) != 3 {
		t.Errorf("an open pull request on its branch: import row %+v, deletes %q; want %+v kept", got, r.mirrors.deletes, want)
	}
	if closes, deletes := calls(); len(closes) != 3 || len(deletes) != 4 {
		t.Errorf("%d closes and %d branch deletes, want 3 and 4", len(closes), len(deletes))
	}
	if pr, ok, err := r.events.FiledPR(context.Background(), "linkshort"); ok || err != nil {
		t.Errorf("FiledPR = %d, %v, %v; want no filing recorded", pr, ok, err)
	}
}

// A pull request whose opening answers an error may have opened all the
// same: the filing reads infra's open pull requests with the config token
// and keeps the one from its branch into main, not a fork's of that name,
// recording it on the import row and in the project_filed event instead
// of deleting the row.
func TestFilingKeepsAPullRequestOpenedDespiteAnError(t *testing.T) {
	r := newRig(t)
	infra := withInfra(t, r)
	r.c.cfg.CheckProject = func(context.Context, string, []byte) ([]string, error) { return nil, nil }
	ck, s := r.loggedIn(t)
	pull := func(number int, branch, repo string) map[string]any {
		return map[string]any{"number": number, "state": "open", "base": map[string]any{"ref": "main"},
			"head": map[string]any{"ref": branch, "repo": map[string]any{"full_name": repo}}}
	}
	infra.mu.Lock()
	infra.pullsStatus = http.StatusInternalServerError
	infra.open = []map[string]any{pull(13, "project-linkshort", "mallory/infra"), pull(12, "project-linkshort", "sf/infra"), pull(9, "project-other", "sf/infra")}
	infra.mu.Unlock()
	if rec := r.do(post("/projects", importForm(s.csrf)), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/projects/linkshort/status" {
		t.Fatalf("POST /projects = %d %q, want 303 to the panel", rec.Code, rec.Body.String())
	}
	want := factoryd.Mirror{Project: "linkshort", Direction: factoryd.MirrorImport, Remote: "acme/app", Token: ghToken, State: "pending", PR: 12}
	if got := r.mirrors.rows["linkshort import"]; got != want {
		t.Errorf("import row %+v, want %+v", got, want)
	}
	if pr, ok, err := r.events.FiledPR(context.Background(), "linkshort"); pr != 12 || !ok || err != nil {
		t.Errorf("FiledPR = %d, %v, %v; want #12", pr, ok, err)
	}
	infra.mu.Lock()
	defer infra.mu.Unlock()
	if len(infra.closes) != 0 || len(infra.branchDeletes) != 0 || !slices.Equal(infra.pullAuth, []string{"Bearer config-token"}) {
		t.Errorf("closes %+v, branch deletes %+v, reads %q; want none, none and one with the config token", infra.closes, infra.branchDeletes, infra.pullAuth)
	}
}

// The project_filed event is part of the filing: when it cannot be
// written, the filing fails as a whole. Its pull request is closed and its
// branch deleted as the human, and its import row goes, so the panel never
// lacks a filing's pull request.
func TestFilingFailsWithoutItsEvent(t *testing.T) {
	r := newRig(t)
	infra := withInfra(t, r)
	r.c.cfg.CheckProject = func(context.Context, string, []byte) ([]string, error) { return nil, nil }
	ck, s := r.loggedIn(t)
	infra.branches = map[string]bool{}
	r.events.mu.Lock()
	r.events.fail = errors.New("db down")
	r.events.mu.Unlock()
	rec := r.do(post("/projects", importForm(s.csrf)), ck)
	if body := rec.Body.String(); rec.Code != http.StatusInternalServerError || !strings.Contains(body, "cannot record the filing of sf/infra#12; the filing was undone") {
		t.Fatalf("POST /projects = %d %q, want 500 saying the filing was undone", rec.Code, body)
	}
	infra.mu.Lock()
	closes, deletes := infra.closes, infra.branchDeletes
	infra.mu.Unlock()
	if len(closes) != 1 || closes[0].path != "/api/v1/repos/sf/infra/pulls/12" || closes[0].auth != "Bearer access-1" {
		t.Errorf("closes %+v, want #12 closed as the human", closes)
	}
	if len(deletes) != 1 || deletes[0].path != "/api/v1/repos/sf/infra/branches/project-linkshort" || deletes[0].auth != "Bearer access-1" {
		t.Errorf("branch deletes %+v, want project-linkshort deleted as the human", deletes)
	}
	if m, ok := r.mirrors.rows["linkshort import"]; ok {
		t.Errorf("import row %+v kept, want none", m)
	}
	r.events.mu.Lock()
	r.events.fail = nil
	r.events.mu.Unlock()
	if pr, ok, err := r.events.FiledPR(context.Background(), "linkshort"); ok || err != nil {
		t.Errorf("FiledPR = %d, %v, %v; want no filing", pr, ok, err)
	}
}

// An import row adds the step "Imported from GitHub" after the
// repository's: waiting before the load, running until registration's
// migrate and during it with its start, then done with its time or failed
// with the row's state.
func TestStatusShowsGitHubImport(t *testing.T) {
	for _, c := range []struct {
		load                bool
		state, want, detail string
	}{
		{false, "pending", stepWaiting, "After the merge."},
		{true, "pending", stepRunning, "Registration copies github.com/acme/app into the repository within a minute of the load."},
		{true, "copying since 2026-10-11 14:00 UTC", stepRunning, "github.com/acme/app, copying since 2026-10-11 14:00 UTC."},
		{true, "failed: forgejo POST /api/v1/repos/migrate: status 500: boom", stepFailed,
			"The copy of github.com/acme/app failed: forgejo POST /api/v1/repos/migrate: status 500: boom. The reconcile tries again at least hourly."},
		{true, "imported 2026-10-11 14:00 UTC", stepDone, "github.com/acme/app, imported 2026-10-11 14:00 UTC."},
	} {
		s := newStatusRig(t)
		s.mirrors.rows["linkshort import"] = factoryd.Mirror{Project: "linkshort", Direction: factoryd.MirrorImport, Remote: "acme/app", State: c.state}
		if c.load {
			s.force.load("linkshort")
		}
		body := s.status(t)
		names, states, _ := steps(body)
		wantNames := []string{"Pull request opened", "Approved and merged", "Repository created", "Imported from GitHub", "Board ready"}
		if !slices.Equal(names, wantNames) || states[3] != c.want || !strings.Contains(body, `<span class="detail">`+c.detail+`</span>`) {
			t.Errorf("%s: steps %q, states %q, want %s with %q:\n%s", c.state, names, states, c.want, c.detail, body)
		}
	}
}
