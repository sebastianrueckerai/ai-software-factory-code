package factoryd

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// pullsGitHub answers GitHub's REST API for acme/app and records every call
// as "METHOD path auth body". A pull request titled Deny is refused as a
// token without the permission is, unless allow. The repository reads
// with repoStatus, when set, or with its default branch and repoKiB.
type pullsGitHub struct {
	mu         sync.Mutex
	calls      []string
	state      string // GET pulls/12's answer; pulls/13 is open
	allow      bool
	repoStatus int
	repoKiB    int64
}

func (g *pullsGitHub) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		g.calls = append(g.calls, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(body)))
		deny := strings.Contains(string(body), `"title":"Deny"`)
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/acme/") && strings.Count(r.URL.Path, "/") == 3 && g.repoStatus != 0:
			w.WriteHeader(g.repoStatus)
			io.WriteString(w, `{"message":"Bad credentials","status":"401"}`)
		case strings.HasPrefix(r.URL.Path, "/repos/acme/") && strings.Count(r.URL.Path, "/") == 3:
			fmt.Fprintf(w, `{"default_branch":"main","size":%d}`, g.repoKiB)
		case r.Method == http.MethodPost && deny && !g.allow:
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"message":"Resource not accessible by personal access token","status":"403"}`)
		case r.Method == http.MethodPost && deny:
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"number":13,"html_url":"https://github.com/acme/app/pull/13"}`)
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"number":12,"html_url":"https://github.com/acme/app/pull/12"}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/13"):
			io.WriteString(w, `{"state":"open","merged":false}`)
		default:
			io.WriteString(w, g.state)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (g *pullsGitHub) took() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	calls := g.calls
	g.calls = nil
	return calls
}

// fakeForgejo answers the Forgejo API a GitHub export reads: the project
// repository's size in KiB (counting the reads), the bot's fork and its
// branches, the open pull requests, a branch, and a new pull request #3;
// it records every other call as "METHOD path auth body".
type fakeForgejo struct {
	mu             sync.Mutex
	size           int64
	sizes          int
	branches, open string // the lists' first pages
	calls          []string
}

func (f *fakeForgejo) server(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		page := func(first string) {
			if r.URL.Query().Get("page") == "1" {
				io.WriteString(w, cmp.Or(first, "[]"))
				return
			}
			io.WriteString(w, "[]")
		}
		body, _ := io.ReadAll(r.Body)
		switch path := r.URL.EscapedPath(); {
		case path == "/api/v1/repos/demo-app/demo-app":
			f.sizes++
			fmt.Fprintf(w, `{"size":%d}`, f.size)
		case path == "/api/v1/repos/factory-agent/demo-app":
			io.WriteString(w, `{"fork":true,"private":true,"has_actions":false,"parent":{"full_name":"demo-app/demo-app"}}`)
		case path == "/api/v1/repos/factory-agent/demo-app/branches":
			page(f.branches)
		case path == "/api/v1/repos/demo-app/demo-app/pulls" && r.Method == http.MethodGet:
			page(f.open)
		case strings.HasPrefix(path, "/api/v1/repos/factory-agent/demo-app/branches/") && r.Method == http.MethodGet:
			io.WriteString(w, "{}")
		case r.Method == http.MethodDelete:
			f.calls = append(f.calls, r.Method+" "+path)
			w.WriteHeader(http.StatusNoContent)
		default:
			f.calls = append(f.calls, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(body))
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"number":3,"html_url":"https://git.example/demo-app/demo-app/pulls/3"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// gitRig is a Forgejo and a GitHub of bare repositories under root,
// demo-app/demo-app with the bot's fork factory-agent/demo-app and
// acme/app, all with main at base, and a work tree that commits.
type gitRig struct {
	t                                 *testing.T
	root, work, forgejo, fork, github string
	base                              string
}

func newGitRig(t *testing.T) *gitRig {
	g := &gitRig{t: t, root: t.TempDir()}
	g.work, g.forgejo, g.fork, g.github = g.root+"/work", g.root+"/forgejo/demo-app/demo-app.git",
		g.root+"/forgejo/factory-agent/demo-app.git", g.root+"/github/acme/app.git"
	for _, dir := range []string{g.forgejo, g.fork, g.github} {
		g.git(g.root, "init", "-q", "--bare", "-b", "main", dir)
	}
	g.git(g.root, "init", "-q", "-b", "main", g.work)
	g.base = g.commit("base", "README", "hello")
	for _, dir := range []string{g.forgejo, g.fork, g.github} {
		g.git(g.work, "push", "-q", dir, "main")
	}
	return g
}

func (g *gitRig) git(dir string, args ...string) string {
	g.t.Helper()
	out, err := git(context.Background(), dir, "", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	if err != nil {
		g.t.Fatal(err)
	}
	return out
}

// commit commits file with text in the work tree and answers its SHA.
func (g *gitRig) commit(msg, file, text string) string {
	g.t.Helper()
	path := filepath.Join(g.work, file)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		g.t.Fatal(err)
	}
	g.git(g.work, "add", file)
	g.git(g.work, "commit", "-q", "-m", msg)
	return g.git(g.work, "rev-parse", "HEAD")
}

func (g *gitRig) registrar(store *PGStore, api *httptest.Server, forgejo string) *Registrar {
	return &Registrar{Forgejo: &Forgejo{BaseURL: forgejo, PublicURL: "https://git.example", ProjectsToken: "projects-tok",
		BotUser: "factory-agent", BotPassword: "bot-pw"}, ProjectsUser: "factory-projects",
		Store: &memProjects{m: map[string]Project{"demo-app": demoProject()}}, Mirrors: store,
		GitHub: &GitHub{API: api.URL, Git: "file://" + g.root + "/github", Forgejo: "file://" + g.root + "/forgejo", HTTP: api.Client()}}
}

// git hands a call's login to git as an Authorization header from the
// environment, with the low-memory and low-speed settings, and git reads
// them.
func TestGitConfig(t *testing.T) {
	for key, want := range map[string]string{"http.extraHeader": "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("u:ghp-tok")),
		"pack.threads": "1", "pack.windowMemory": "32m", "core.packedGitLimit": "64m", "core.deltaBaseCacheLimit": "32m",
		"http.lowSpeedLimit": "1000", "http.lowSpeedTime": "60"} {
		if got, err := git(context.Background(), t.TempDir(), basicLogin("u", "ghp-tok"), "config", "--get", key); err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", key, got, err, want)
		}
	}
}

// A fetch from a server that accepts and never answers ends at its
// deadline, git's remote helper with it: the server sees the connection
// close.
func TestGitTimeoutKillsTheHelper(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	closed := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		io.Copy(io.Discard, conn) // until the helper's end closes it
		close(closed)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if _, err := git(ctx, t.TempDir(), "", "ls-remote", "http://"+ln.Addr().String()+"/acme/app.git"); err == nil {
		t.Fatal("a hung fetch succeeded")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("git returned after %v, want about its 1 s deadline", took)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("the remote helper still holds the connection")
	}
}

// GitHub's pull request states read as a card shows them.
func TestGitHubPullState(t *testing.T) {
	g := &pullsGitHub{}
	srv := g.server(t)
	gh := &GitHub{API: srv.URL, HTTP: srv.Client()}
	for answer, want := range map[string]string{`{"state":"open","merged":false}`: "open", `{"state":"closed","merged":true}`: "merged",
		`{"state":"closed","merged":false}`: "closed"} {
		g.state = answer
		if got, err := gh.pullState(context.Background(), "ghp-tok", "acme/app", 12); err != nil || got != want {
			t.Errorf("%s = %q, %v; want %s", answer, got, err, want)
		}
	}
}

// pullsRig is a store, a git rig, a fake GitHub and Forgejo and the
// registrar on them, with the commit head pushed as Forgejo's
// refs/pull/6, 7 and 8 heads.
type pullsRig struct {
	*gitRig
	store *PGStore
	gh    *pullsGitHub
	fj    *fakeForgejo
	r     *Registrar
	head  string
}

func newPullsRig(t *testing.T) *pullsRig {
	p := &pullsRig{gitRig: newGitRig(t), store: chatStore(t), gh: &pullsGitHub{state: `{"state":"closed","merged":true}`}, fj: &fakeForgejo{size: 23}}
	p.r = p.registrar(p.store, p.gh.server(t), p.fj.server(t))
	p.head = p.commit("the fix", "fix.go", "package fix")
	p.git(p.work, "push", "-q", p.forgejo, "HEAD:refs/pull/7/head", "HEAD:refs/pull/6/head", "HEAD:refs/pull/8/head")
	t.Cleanup(func() { p.store.DeleteMirror(context.Background(), "demo-app", MirrorExport) })
	return p
}

// done files card id of the project as done with pull request pr.
func (p *pullsRig) done(id, objective string, pr int64) {
	p.t.Helper()
	ctx := context.Background()
	if err := p.store.CreateTask(ctx, Task{ID: id, Kind: "agent", Status: "in-review", Repository: "demo-app/demo-app", Objective: objective}); err != nil {
		p.t.Fatal(err)
	}
	if err := errors.Join(p.store.SetTaskPR(ctx, id, pr), p.store.TransitionTask(ctx, id, []string{"in-review"}, "done")); err != nil {
		p.t.Fatal(err)
	}
}

func (p *pullsRig) export(remote string) {
	p.t.Helper()
	if err := p.store.PutMirror(context.Background(), Mirror{Project: "demo-app", Direction: MirrorExport, Remote: remote, Username: "u",
		Token: "ghp-tok", State: "created", Mode: ModePullRequests}); err != nil {
		p.t.Fatal(err)
	}
}

func (p *pullsRig) card(id string) Task {
	p.t.Helper()
	k, _, err := p.store.Task(context.Background(), id)
	if err != nil {
		p.t.Fatal(err)
	}
	return k
}

func (p *pullsRig) exportState() string {
	m, _, _ := p.store.Mirror(context.Background(), "demo-app", MirrorExport)
	return m.State
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL): in mode
// pull_requests each card done since the export was put has its pull
// request's head pushed to GitHub as its branch and one pull request opened
// there, its number, URL and state on the card; a card done before is left
// alone; a token without the pull request permission leaves the card's
// failed line, which no pass retries, and a put of the same remote and mode
// keeps since and gives it one more try; the states are read at most every
// 5 minutes, one call per open pull request of a card done since; the
// export counts the open ones.
func TestGitHubPullRequests(t *testing.T) {
	p, ctx := newPullsRig(t), context.Background()
	p.done("t-old", "Before", 6)
	p.export("acme/app")
	p.done("t-new", "Fix the link\n\nThe link 404s.", 7)
	p.done("t-deny", "Deny", 8)

	p.r.githubPass(ctx)
	const auth = "Bearer ghp-tok"
	deny := "POST /repos/acme/app/pulls " + auth + ` {"base":"main","body":"Deny\n\nMerged in the AI software factory as https://git.example/demo-app/demo-app/pulls/8 (task t-deny).","head":"factory/t-deny","title":"Deny"}`
	equalStrings(t, "first pass", p.gh.took(), []string{"GET /repos/acme/app " + auth,
		"POST /repos/acme/app/pulls " + auth + ` {"base":"main","body":"Fix the link\n\nThe link 404s.\n\nMerged in the AI software factory as https://git.example/demo-app/demo-app/pulls/7 (task t-new).","head":"factory/t-new","title":"Fix the link"}`,
		"GET /repos/acme/app " + auth, deny})
	for _, branch := range []string{"factory/t-new", "factory/t-deny"} {
		if got := p.git(p.github, "rev-parse", "refs/heads/"+branch); got != p.head {
			t.Errorf("GitHub's %s = %s, want the pull request's head %s", branch, got, p.head)
		}
	}
	if k := p.card("t-new"); k.GitHubPR != 12 || k.GitHubPRURL != "https://github.com/acme/app/pull/12" || k.GitHubPRState != "open" {
		t.Errorf("t-new's GitHub pull request = %d %q %q, want 12, its URL, open", k.GitHubPR, k.GitHubPRURL, k.GitHubPRState)
	}
	const denied = `failed: factory/t-deny is on GitHub, but the export's token may not open pull requests: it needs the permission "Pull requests: read and write"`
	if k := p.card("t-deny"); k.GitHubPR != 0 || k.GitHubPRState != denied {
		t.Errorf("t-deny = %d %q, want 0 and %q", k.GitHubPR, k.GitHubPRState, denied)
	}
	if k := p.card("t-old"); k.GitHubPRState != "" {
		t.Errorf("t-old, done before the export, = %q; want left alone", k.GitHubPRState)
	}
	if got := p.exportState(); got != "1 open pull request" {
		t.Errorf("export state = %q, want 1 open pull request", got)
	}

	p.r.githubPass(ctx)
	equalStrings(t, "a pass within 5 minutes", p.gh.took(), nil)
	p.gh.allow = true
	p.export("acme/app")
	if err := p.store.ClearGitHubFailures(ctx, "demo-app/demo-app"); err != nil {
		t.Fatal(err)
	}
	p.r.githubPass(ctx)
	equalStrings(t, "the pass after a put of the same remote", p.gh.took(), []string{"GET /repos/acme/app " + auth, deny})
	if k := p.card("t-deny"); k.GitHubPR != 13 || k.GitHubPRState != "open" || p.exportState() != "2 open pull requests" {
		t.Errorf("t-deny after its one more try = %d %q, export %q; want 13, open, 2 open pull requests", k.GitHubPR, k.GitHubPRState, p.exportState())
	}

	p.r.githubAt = time.Now().Add(-githubStates)
	p.r.githubPass(ctx)
	equalStrings(t, "a pass after 5 minutes", p.gh.took(), []string{"GET /repos/acme/app/pulls/12 " + auth, "GET /repos/acme/app/pulls/13 " + auth})
	if k := p.card("t-new"); k.GitHubPRState != "merged" || p.exportState() != "1 open pull request" {
		t.Errorf("after the state pass: t-new %q, export %q; want merged, 1 open pull request", k.GitHubPRState, p.exportState())
	}

	p.git(p.root, "init", "-q", "--bare", "-b", "main", p.root+"/github/acme/other.git")
	p.export("acme/other")
	p.done("t-later", "Later", 7)
	p.r.githubAt = time.Now().Add(-githubStates)
	p.r.githubPass(ctx)
	equalStrings(t, "the pass after a put of another remote", p.gh.took(), []string{"GET /repos/acme/other " + auth,
		"POST /repos/acme/other/pulls " + auth + ` {"base":"main","body":"Later\n\nMerged in the AI software factory as https://git.example/demo-app/demo-app/pulls/7 (task t-later).","head":"factory/t-later","title":"Later"}`})
	if got := p.exportState(); got != "1 open pull request" {
		t.Errorf("export state after the move = %q, want t-later's 1 open pull request", got)
	}
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL): a GitHub
// repository the token cannot read fails the card with a plain line and
// fetches nothing; so does a project repository over the cap; a push
// GitHub rejects fails it; a push that cannot reach GitHub leaves it for
// the next pass.
func TestGitHubCardFailures(t *testing.T) {
	p, ctx := newPullsRig(t), context.Background()
	p.export("acme/app")
	p.gh.repoStatus = http.StatusUnauthorized
	p.done("t-401", "Unread", 7)
	p.r.githubPass(ctx)
	if got, want := p.card("t-401").GitHubPRState, "failed: GitHub refused the export's repository acme/app: GitHub answered 401 Bad credentials"; got != want ||
		p.fj.sizes != 0 {
		t.Errorf("a 401: %q after %d size reads, want %q after none", got, p.fj.sizes, want)
	}
	if _, err := git(ctx, p.github, "", "rev-parse", "--verify", "-q", "refs/heads/factory/t-401"); err == nil {
		t.Error("a 401 pushed the card's branch")
	}
	p.gh.repoStatus = 0
	p.fj.size = 300 << 10
	p.done("t-big", "Big", 7)
	p.r.githubPass(ctx)
	if got, want := p.card("t-big").GitHubPRState, "failed: demo-app/demo-app is 300 MiB, over the 200 MiB the factory fetches to take a card to GitHub"; got != want {
		t.Errorf("over the cap: %q, want %q", got, want)
	}
	p.fj.size = 23
	p.git(p.work, "commit", "-q", "--allow-empty", "-m", "elsewhere")
	p.git(p.work, "push", "-q", p.github, "HEAD:refs/heads/factory/t-rej")
	p.done("t-rej", "Rejected", 7)
	p.r.githubPass(ctx)
	if got := p.card("t-rej").GitHubPRState; !strings.HasPrefix(got, "failed: the push of factory/t-rej to GitHub was refused: git push: ") ||
		!strings.Contains(got, "[rejected]") {
		t.Errorf("a rejected push: %q", got)
	}
	p.r.GitHub.Git = "file://" + p.root + "/nowhere"
	p.done("t-gone", "Unreachable", 7)
	p.r.githubPass(ctx)
	if got := p.card("t-gone").GitHubPRState; got != "" {
		t.Errorf("an unreachable GitHub: %q, want the card left for the next pass", got)
	}
	read := "GET /repos/acme/app Bearer ghp-tok"
	equalStrings(t, "GitHub API calls", p.gh.took(), []string{read, read, read, read})
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL): Sync latest
// opens nothing while GitHub's default branch is main's or the last sync's;
// a commit on GitHub becomes the bot's fork's branch github/sync-<short
// sha> with one pull request to main as the bot; a commit that changes
// .forgejo/ is refused and pushes nothing, and so is a history main does
// not share. Each answer is the export row's last sync.
func TestGitHubSync(t *testing.T) {
	store, ctx := chatStore(t), context.Background()
	g := newGitRig(t)
	fj, gh := &fakeForgejo{size: 23}, &pullsGitHub{}
	r := g.registrar(store, gh.server(t), fj.server(t))
	if _, err := r.GitHubSync(ctx, "demo-app", "sebastian"); err != ErrNoExport {
		t.Fatalf("without an export: %v, want ErrNoExport", err)
	}
	if err := store.PutMirror(ctx, Mirror{Project: "demo-app", Direction: MirrorExport, Remote: "acme/app", Username: "u", Token: "ghp-tok"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.DeleteMirror(ctx, "demo-app", MirrorExport) })
	syncs := func(want string, failed bool) {
		t.Helper()
		got, err := r.GitHubSync(ctx, "demo-app", "sebastian")
		row := got
		if failed {
			got, row = err.Error(), "failed: "+err.Error()
		} else if err != nil {
			t.Fatal(err)
		}
		m, _, _ := store.Mirror(ctx, "demo-app", MirrorExport)
		if got != want || m.SyncResult != row {
			t.Fatalf("GitHubSync = %q, row %q\nwant %q", got, m.SyncResult, want)
		}
	}
	syncs("main holds GitHub's default branch of acme/app at "+g.base[:7]+": nothing to sync.", false)
	syncs("GitHub's default branch of acme/app is still at "+g.base[:7]+", which the last sync brought back: nothing to sync.", false)
	if fj.calls != nil {
		t.Fatalf("Forgejo calls without a change: %q", fj.calls)
	}

	moved := g.commit("on GitHub", "NOTES", "from GitHub")
	g.git(g.work, "push", "-q", g.github, "main")
	short := moved[:7]
	gh.repoKiB = 300 << 10
	syncs("acme/app is 300 MiB on GitHub, over the 200 MiB the factory fetches to sync from GitHub", true)
	gh.repoKiB = 23
	var twice sync.WaitGroup
	answers := make([]string, 2)
	for i := range answers {
		twice.Go(func() {
			got, err := r.GitHubSync(ctx, "demo-app", "sebastian")
			answers[i] = got + fmt.Sprint(err)
		})
	}
	twice.Wait()
	slices.Sort(answers)
	equalStrings(t, "a double click", answers, []string{
		"GitHub's default branch of acme/app is still at " + short + ", which the last sync brought back: nothing to sync.<nil>",
		"Pull request #3 brings GitHub's default branch of acme/app at " + short + " to main: https://git.example/demo-app/demo-app/pulls/3<nil>"})
	equalStrings(t, "Forgejo calls", fj.calls, []string{"POST /api/v1/repos/demo-app/demo-app/pulls Basic " + base64.StdEncoding.EncodeToString([]byte("factory-agent:bot-pw")) + " " +
		`{"base":"main","body":"GitHub's default branch of acme/app at ` + moved + `, brought back for sebastian.","head":"factory-agent:github/sync-` + short + `","title":"Sync from GitHub ` + short + `"}`})
	if got := g.git(g.fork, "rev-parse", "refs/heads/github/sync-"+short); got != moved {
		t.Fatalf("the fork's github/sync-%s = %s, want %s", short, got, moved)
	}
	if branches := g.git(g.forgejo, "branch", "--list", "github/*"); branches != "" {
		t.Fatalf("the project repository's sync branches = %q, want none", branches)
	}
	syncs("GitHub's default branch of acme/app is still at "+short+", which the last sync brought back: nothing to sync.", false)

	workflow := g.commit("CI on GitHub", ".forgejo/workflows/ci.yml", "on: push")
	g.git(g.work, "push", "-q", g.github, "main")
	syncs("GitHub's default branch of acme/app at "+workflow[:7]+" changes .forgejo/, .gitea/ or .github/, which no pull request of the factory's may change (rulings 90, 100): bring it back by hand", true)
	g.git(g.work, "checkout", "-q", "--orphan", "other")
	unrelated := g.commit("another history", "OTHER", "x")
	g.git(g.work, "push", "-q", "-f", g.github, "other:main")
	syncs("GitHub's default branch of acme/app at "+unrelated[:7]+" shares no history with main: nothing synced", true)
	if branches := g.git(g.fork, "branch", "--list", "github/*"); branches != "github/sync-"+short {
		t.Fatalf("the fork's sync branches = %q, want github/sync-%s alone", branches, short)
	}
	if len(fj.calls) != 1 {
		t.Fatalf("the refused syncs called Forgejo: %q", fj.calls[1:])
	}
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL): at most
// every 5 minutes, and never while a git operation runs, the pass deletes
// the bot's fork's github/sync-* branches with no open pull request, and
// no other branch.
func TestGitHubSyncBranches(t *testing.T) {
	p, ctx := newPullsRig(t), context.Background()
	if err := p.store.PutMirror(ctx, Mirror{Project: "demo-app", Direction: MirrorExport, Remote: "acme/app", Username: "u", Token: "ghp-tok"}); err != nil {
		t.Fatal(err)
	}
	p.fj.branches = `[{"name":"main"},{"name":"factory/t-x"},{"name":"github/sync-aaaaaaa"},{"name":"github/sync-bbbbbbb"}]`
	p.fj.open = `[{"number":5,"head":{"ref":"github/sync-bbbbbbb","repo":{"full_name":"factory-agent/demo-app"}}},` +
		`{"number":6,"head":{"ref":"github/sync-aaaaaaa","repo":{"full_name":"someone/demo-app"}}}]`
	p.r.gitMu.Lock()
	p.r.githubPass(ctx)
	p.r.gitMu.Unlock()
	equalStrings(t, "while a git operation runs", p.fj.calls, nil)
	p.r.githubAt = time.Time{}
	p.r.githubPass(ctx)
	p.r.githubPass(ctx)
	equalStrings(t, "two passes", p.fj.calls, []string{"DELETE /api/v1/repos/factory-agent/demo-app/branches/github%2Fsync-aaaaaaa"})
}

func equalStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s:\n%s\nwant\n%s", what, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func syncTaskID(repository string, pr int64) string {
	sum := sha256.Sum256([]byte("sync " + repository + " " + strconv.FormatInt(pr, 10)))
	return "t-" + hex.EncodeToString(sum[:6])
}

// githubSync files a card of SourceGitHubSync with the pull request linked
// when the sync opens one (roadmap 187).
func TestGitHubSyncFilesCard(t *testing.T) {
	ctx := context.Background()
	f, _ := fileStub(t)
	store, wf := newFakeStore(), &fakeWorkflows{}
	p := seedProject("sf/r")
	r := &Registrar{Forgejo: f, Tasks: store, Workflows: wf, Store: &memProjects{m: map[string]Project{"sf-r": p}}}
	r.fileSyncCard(ctx, p, Repo{Owner: "factory-agent", Name: "r"}, 9, "abc1234", "abc1234567890abcdef", "sebastian")
	wantID := syncTaskID("sf/r", 9)
	row, ok := store.tasks[wantID]
	if !ok {
		t.Fatalf("no task filed for pull request #9; tasks = %+v", store.tasks)
	}
	if row.Source != SourceGitHubSync || row.PRNumber != 9 || row.Author != "sebastian" {
		t.Fatalf("task = %+v, want SourceGitHubSync, PRNumber 9 and author sebastian", row)
	}
	if row.IssueNumber == 0 {
		t.Fatal("the card's issue was never written")
	}
	if !slices.Equal(wf.started, []string{wantID}) {
		t.Fatalf("started %v, want the workflow started once", wf.started)
	}
}

// githubSync is idempotent by SHA in the branch name: syncing the same
// pull request twice files one card only (roadmap 187).
func TestGitHubSyncIdempotent(t *testing.T) {
	ctx := context.Background()
	f, _ := fileStub(t)
	store, wf := newFakeStore(), &fakeWorkflows{}
	p := seedProject("sf/r")
	r := &Registrar{Forgejo: f, Tasks: store, Workflows: wf, Store: &memProjects{m: map[string]Project{"sf-r": p}}}
	fork := Repo{Owner: "factory-agent", Name: "r"}
	r.fileSyncCard(ctx, p, fork, 9, "abc1234", "abc1234567890abcdef", "sebastian")
	first := len(store.tasks)
	r.fileSyncCard(ctx, p, fork, 9, "abc1234", "abc1234567890abcdef", "sebastian")
	if len(store.tasks) != first {
		t.Fatalf("tasks after second call = %d, want %d: the same SHA must not file another card", len(store.tasks), first)
	}
	if len(wf.started) != 1 {
		t.Fatalf("workflow started %d times, want 1", len(wf.started))
	}
}
