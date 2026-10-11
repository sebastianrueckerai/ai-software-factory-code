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
}

func (f *fakeMirrors) Mirror(_ context.Context, project, direction string) (factoryd.Mirror, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.rows[project+" "+direction]
	m.Token = ""
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

func (f *fakeMirrors) DeleteMirror(_ context.Context, project, direction string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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

// The chat's github_export proposes with the remote alone in its
// description; no stream line, conversation row, proposal row, event or
// log line holds the token, not even the member's own message once the
// call took it, nor a token under a key in another case. Do it replaces
// an earlier push mirror to the remote, adds the new one with the token,
// syncs it, stores the row and leaves one github_export_set row with the
// remote. A member who is no approver is refused, and so is a seed
// project, with nothing reaching Forgejo; after a restart, which forgets
// the held token, a Do it asks again.
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
	g.replies = [][]string{{call(1, "github_export", `{"remote":"acme/app","username":"u","token":"`+ghToken+`"}`)}}
	_, events, tokens := chatPost(t, r, ck, s, demo, url.Values{"message": {"export to acme/app as u, token " + ghToken}})
	const masked = `{"remote":"acme/app","token":"[token]","username":"u"}`
	equal(t, "stream", events, []string{
		`proposal {"arguments":` + masked + `,"description":"Export to GitHub, remote acme/app","id":"TOKEN","tool":"github_export"}`, `done {}`})
	stored, _ := chat.ChatMessages(t.Context(), "demo-app/demo-app", "sebastian", 0, 2)
	equal(t, "the member's message", stored[0].Content, "export to acme/app as u, token [token]")
	equal(t, "the stored call's arguments", stored[1].ToolCalls[0].Arguments, masked)
	row, _ := chat.proposal(tokens[0])
	equal(t, "the proposal's args", string(row.Args), masked)
	equal(t, "Forgejo calls before Do it", r.mirrors.took(), []string(nil))

	_, done, _ := chatPost(t, r, ck, s, demo+"/proposals/"+tokens[0], nil)
	set := "GitHub export of demo-app to acme/app is set: Forgejo pushes every branch and tag on each commit and every 8 h."
	equal(t, "Do it", done, []string{`tool {"arguments":` + masked + `,"duration_ms":0,"name":"github_export","result":"` + set + `"}`,
		`applied {"result":"` + set + `","tool":"github_export"}`, `done {}`})
	equal(t, "Forgejo calls", r.mirrors.took(), []string{"PushMirrors demo-app/demo-app", "DeletePushMirror demo-app/demo-app remote_mirror_old",
		"AddPushMirror demo-app/demo-app https://github.com/acme/app.git u " + ghToken, "SyncPushMirrors demo-app/demo-app"})
	equal(t, "the row", r.mirrors.rows["demo-app export"], factoryd.Mirror{Project: "demo-app",
		Direction: factoryd.MirrorExport, Remote: "acme/app", Username: "u", Token: ghToken, State: "created"})
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "demo-app/demo-app", Kind: factoryd.EventGithubExportSet, Detail: map[string]any{"remote": "acme/app"}})

	r.repoWriters["demo-app/demo-app"] = append(r.repoWriters["demo-app/demo-app"], "anna")
	ak, as := r.as(t, "anna")
	g.replies = [][]string{{call(1, "github_export", `{"remote":"acme/app","username":"u","Token":"`+ghToken+`"}`)}}
	_, asked, tokens := chatPost(t, r, ak, as, demo, url.Values{"message": {"export it"}})
	const cased = `{"Token":"[token]","remote":"acme/app","username":"u"}`
	row, _ = chat.proposal(tokens[0])
	equal(t, "a Token key's proposal", []any{asked[0][:len("proposal ")+len(`{"arguments":`)+len(cased)], string(row.Args)},
		[]any{`proposal {"arguments":` + cased, cased})
	_, refused, _ := chatPost(t, r, ak, as, demo+"/proposals/"+tokens[0], nil)
	no := "Refused: Only the approvers of project demo-app may export it to GitHub."
	equal(t, "anna's Do it", refused, []string{`tool {"arguments":` + cased + `,"duration_ms":0,"name":"github_export","result":"` + no + `"}`, `done {}`})
	equal(t, "Forgejo calls of anna's", r.mirrors.took(), []string(nil))

	g.project = ""
	g.replies = [][]string{{call(1, "github_export", `{"remote":"acme/code","username":"u","token":"`+ghToken+`"}`)}}
	_, _, tokens = chatPost(t, r, ck, s, chatPath, url.Values{"message": {"export the factory"}})
	_, seed, _ := chatPost(t, r, ck, s, chatPath+"/proposals/"+tokens[0], nil)
	own := "Refused: ai-software-factory-code is the factory's own: infra's push-mirrors CronJob mirrors it (ruling 126)"
	equal(t, "the seed project's Do it", seed, []string{`tool {"arguments":{"remote":"acme/code","token":"[token]","username":"u"},"duration_ms":0,` +
		`"name":"github_export","result":"` + own + `"}`, `done {}`})
	equal(t, "Forgejo calls of the seed's", r.mirrors.took(), []string(nil))
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "demo-app/demo-app", Kind: factoryd.EventGithubExportSet, Detail: map[string]any{"remote": "acme/app"}})

	said, _ := json.Marshal([]any{chat.msgs, chat.proposals, r.events.all(), events, done, asked, refused, seed})
	if strings.Contains(string(said)+logs.String(), ghToken) {
		t.Fatalf("the token was kept or said:\n%s\n%s", said, logs.String())
	}

	g.project = "demo-app"
	g.replies = [][]string{{call(1, "github_export", `{"remote":"acme/app","username":"u","token":"`+ghToken+`"}`)}}
	_, _, tokens = chatPost(t, r, ck, s, demo, url.Values{"message": {"export it again"}})
	clear(r.c.held)
	_, gone, _ := chatPost(t, r, ck, s, demo+"/proposals/"+tokens[0], nil)
	stored, _ = chat.ChatMessages(t.Context(), "demo-app/demo-app", "sebastian", 0, 1)
	equal(t, "Do it after a restart", []any{gone, stored[0].ToolCalls[0].Result, r.mirrors.took()},
		[]any{[]string{`done {}`}, "The token is gone with a restart of factoryd; ask again with it.", []string(nil)})
}

// A held token lives as long as its proposal: each hold forgets the
// expired ones.
func TestHeldTokensExpire(t *testing.T) {
	r := newRig(t)
	r.c.held["old"] = heldToken{"ghp_0ld0ld0ld", time.Now().Add(-time.Second)}
	r.c.held["live"] = heldToken{"ghp_l1vel1ve", time.Now().Add(time.Minute)}
	start := time.Now()
	r.c.hold("new", ghToken)
	if _, ok := r.c.held["old"]; ok || r.c.held["live"].token != "ghp_l1vel1ve" || r.c.held["new"].token != ghToken ||
		r.c.held["new"].until.Before(start.Add(chatTTL)) || r.c.held["new"].until.After(time.Now().Add(chatTTL)) {
		t.Fatalf("held = %+v, want live and new, new until 30 minutes on", r.c.held)
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
// snapshot carries too; a project without an export shows none.
func TestStatusLineGithubExport(t *testing.T) {
	r, _, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	line := func() string {
		body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
		start := strings.Index(body, `<p class="status-line"`)
		return body[start : start+strings.Index(body[start:], "</p>")]
	}
	if got := line(); strings.Contains(got, "GitHub") {
		t.Fatalf("status line without an export: %s", got)
	}
	r.mirrors.rows["ai-software-factory-code export"] = factoryd.Mirror{Project: "ai-software-factory-code", Direction: factoryd.MirrorExport,
		Remote: "acme/app", State: "synced"}
	if got := line(); !strings.HasSuffix(got, "\n<span class=\"fig\">GitHub: synced</span>\n") {
		t.Fatalf("status line = %q, want it to end with GitHub: synced", got)
	}
	g.replies = [][]string{{say("ok")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"how is it?"}})
	system := messages(t, g.calls()[0])[0]["content"].(string)
	if !regexp.MustCompile(`\nStatus: [^\n]*; GitHub: synced\n`).MatchString(system) {
		t.Fatalf("the snapshot's status line lacks GitHub: synced:\n%s", system)
	}
	r.mirrors.rows["ai-software-factory-code export"] = factoryd.Mirror{Project: "ai-software-factory-code", Direction: factoryd.MirrorExport,
		Remote: "acme/app", State: "failed: auth"}
	if got := line(); !strings.HasSuffix(got, "\n<span class=\"fig\">GitHub: failed</span>\n") {
		t.Fatalf("status line = %q, want it to end with GitHub: failed", got)
	}
}

func (f *fakeChat) ScrubChatMessages(_ context.Context, repository, login, secret string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, m := range f.msgs[repository+" "+login] {
		b, _ := json.Marshal(m)
		json.Unmarshal([]byte(strings.ReplaceAll(string(b), secret, "[token]")), &f.msgs[repository+" "+login][i])
	}
	return nil
}

// The form asks for a GitHub repository and a token. With them it stores
// the import row first, the token for registration alone, then files the
// same project file and pull request as without, and the project_filed
// row names the source; no file, pull request, event or log line holds the
// token. A row it cannot store files nothing; a bad source or token is
// refused without the token on the page; a form without a source drops a
// stale import row of the name.
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
	want := factoryd.Mirror{Project: "linkshort", Direction: factoryd.MirrorImport, Remote: "acme/app", Token: ghToken, State: "pending"}
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
	if rec.Code != http.StatusInternalServerError || len(infra.contents) != 1 {
		t.Errorf("an unstored row: %d, %d contents calls; want 500 and nothing filed", rec.Code, len(infra.contents))
	}
	infra.mu.Unlock()
	r.mirrors.put = nil
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

// An import row adds the step "Imported from GitHub" after the
// repository's: waiting before the load, running until registration's
// migrate, then done with its time or failed with the row's state.
func TestStatusShowsGitHubImport(t *testing.T) {
	for _, c := range []struct {
		load                bool
		state, want, detail string
	}{
		{false, "pending", stepWaiting, "After the merge."},
		{true, "pending", stepRunning, "Registration copies github.com/acme/app into the repository within a minute of the load."},
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
