package console

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// An export in mode pull_requests adds no push mirror, stores its mode,
// which the projects page shows with the last sync and Sync latest beside
// Remove, and gives its project's failed cards one more try; an unknown
// mode is refused. Sync latest answers at once and runs factoryd's sync as
// the approver, its result one github_synced row, a failure's too; a
// writer is refused, and a project without an export answers 404.
func TestGithubPullRequestMode(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	export := func(mode string) *httptest.ResponseRecorder {
		return r.do(post("/projects/demo-app/export", url.Values{"csrf": {s.csrf}, "remote": {"acme/app"}, "username": {"u"}, "token": {ghToken}, "mode": {mode}}), ck)
	}
	if rec := export("pulls"); rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != "mode must be mirror or pull_requests" {
		t.Fatalf("mode pulls = %d %q, want 400", rec.Code, rec.Body)
	}
	if rec := export(factoryd.ModePullRequests); rec.Code != http.StatusSeeOther {
		t.Fatalf("export = %d %q, want 303", rec.Code, rec.Body)
	}
	equal(t, "Forgejo calls", r.mirrors.took(), []string{"PushMirrors demo-app/demo-app"})
	equal(t, "cleared", r.mirrors.cleared, []string{"demo-app/demo-app"})
	equal(t, "the row", r.mirrors.rows["demo-app export"], factoryd.Mirror{Project: "demo-app", Direction: factoryd.MirrorExport, Remote: "acme/app",
		Username: "u", Token: ghToken, State: "created", Mode: factoryd.ModePullRequests})
	m := r.mirrors.rows["demo-app export"]
	m.SyncResult = "nothing to sync."
	r.mirrors.rows["demo-app export"] = m
	body := r.do(httptest.NewRequest(http.MethodGet, "/settings?project=demo-app", nil), ck).Body.String()
	if want := `GitHub export</dt><dd>acme/app, pull requests, created; last sync: nothing to sync. <form method="post" action="/projects/demo-app/export/remove" class="inline">`; !strings.Contains(body, want) ||
		!strings.Contains(body, `<form method="post" action="/projects/demo-app/export/sync" class="inline">`) ||
		!strings.Contains(body, `<option value="pull_requests">pull requests: each merged card</option>`) {
		t.Fatalf("/settings: want the mode, the last sync, Sync latest and the form's mode:\n%s", body)
	}

	sync := func(ck *http.Cookie, csrf string, code int, result string) {
		t.Helper()
		before := r.events.count()
		if rec := r.do(post("/projects/demo-app/export/sync", url.Values{"csrf": {csrf}}), ck); rec.Code != code {
			t.Fatalf("Sync latest = %d %q, want %d", rec.Code, rec.Body, code)
		}
		if result == "" {
			return
		}
		if r.syncGate != nil {
			equal(t, "events while the sync runs", r.events.count(), before)
			close(r.syncGate)
		}
		for deadline := time.Now().Add(5 * time.Second); r.events.count() == before && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
		rows := r.events.all()[before:]
		equal(t, "the event", []any{len(rows), rows[0].Kind, rows[0].Actor, rows[0].Detail}, []any{1, factoryd.EventGithubSynced, "sebastian",
			map[string]any{"result": result}})
		r.syncGate = nil
	}
	r.syncGate = make(chan struct{})
	sync(ck, s.csrf, http.StatusSeeOther, "Pull request #3 brings GitHub's default branch of acme/app at abc1234 to main.")
	r.syncErr = errors.New("git fetch: exit status 128: fatal: unable to access 'https://github.com/acme/app.git/': Could not resolve host: github.com")
	sync(ck, s.csrf, http.StatusSeeOther, "failed: git fetch: exit status 128: fatal: unable to access 'https://github.com/acme/app.git/': Could not resolve host: github.com")
	r.repoWriters["demo-app/demo-app"] = append(r.repoWriters["demo-app/demo-app"], "anna")
	r.forgetWriters()
	ak, as := r.as(t, "anna")
	sync(ak, as.csrf, http.StatusForbidden, "")
	delete(r.mirrors.rows, "demo-app export")
	sync(ck, s.csrf, http.StatusNotFound, "")
	equal(t, "syncs", r.synced, []string{"demo-app by sebastian", "demo-app by sebastian"})
}

// The chat's github_sync proposes and its Do it starts the sync, whose
// result arrives as an event; github_export names its mode in the
// proposal.
func TestChatGithubSync(t *testing.T) {
	r, _, g := newChatRig(t)
	g.project = "demo-app"
	const demo = "/projects/demo-app/chat"
	ck, s := r.loggedIn(t)
	r.mirrors.rows["demo-app export"] = factoryd.Mirror{Project: "demo-app", Direction: factoryd.MirrorExport, Remote: "acme/app",
		Username: "u", Token: ghToken, State: "created"}
	g.replies = [][]string{{call(1, "github_export", `{"remote":"acme/app","mode":"pull_requests"}`)}}
	_, events, _ := chatPost(t, r, ck, s, demo, url.Values{"message": {"export"}})
	equal(t, "export's proposal", events[0][:strings.Index(events[0], `,"id"`)],
		`proposal {"arguments":{"remote":"acme/app","mode":"pull_requests"},"description":"Export to GitHub, remote acme/app, mode pull_requests"`)
	g.replies = [][]string{{call(1, "github_sync", `{}`)}}
	_, events, tokens := chatPost(t, r, ck, s, demo, url.Values{"message": {"sync from GitHub"}})
	equal(t, "stream", events, []string{`proposal {"arguments":{},"description":"Sync from GitHub","id":"TOKEN","tool":"github_sync"}`, `done {}`})
	before := r.events.count()
	_, done, _ := chatPost(t, r, ck, s, demo+"/proposals/"+tokens[0], nil)
	started := "Sync from GitHub of demo-app started: its result arrives as an event and on the projects page's export row."
	equal(t, "Do it", done[1:], []string{`applied {"result":"` + started + `","tool":"github_sync"}`, `done {}`})
	for deadline := time.Now().Add(5 * time.Second); !slices.ContainsFunc(r.events.all()[before:], func(e factoryd.Event) bool {
		return e.Kind == factoryd.EventGithubSynced
	}) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	equal(t, "syncs", r.synced, []string{"demo-app by sebastian"})
}

// A card shows its GitHub pull request's state on its face and in the
// chat's card read, or why it has none.
func TestCardGitHubPullRequest(t *testing.T) {
	r, _, g := newChatRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-x", Kind: "agent", Status: "done", Repository: "sf/code", IssueNumber: 99, PRNumber: 7,
		GitHubPR: 12, GitHubPRURL: "https://github.com/acme/app/pull/12", GitHubPRState: "merged"},
		factoryd.Task{ID: "t-y", Kind: "agent", Status: "done", Repository: "sf/code", IssueNumber: 98, PRNumber: 6,
			GitHubPRState: "failed: GitHub refused the pull request: GitHub answered 422 Validation Failed"})
	ck, _ := r.loggedIn(t)
	for id, want := range map[string]string{
		"t-x": `<span class="github">GitHub: <a href="https://github.com/acme/app/pull/12" target="_blank" rel="noopener">PR #12</a> merged</span>`,
		"t-y": `<span class="github">GitHub: failed: GitHub refused the pull request: GitHub answered 422 Validation Failed</span>`,
	} {
		if face := r.do(httptest.NewRequest(http.MethodGet, "/cards/"+id, nil), ck).Body.String(); !strings.Contains(face, want) {
			t.Errorf("%s's face lacks %s:\n%s", id, want, face)
		}
	}
	text := reads(t, r, g, "card", `{"task_id":"t-x"}`)[0]
	if want := "\nGitHub: pull request #12 https://github.com/acme/app/pull/12, merged"; !strings.Contains(text, want) {
		t.Errorf("the card read lacks %q:\n%s", want, text)
	}
}
