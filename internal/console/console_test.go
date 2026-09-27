package console

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// htmxSHA256 pins the vendored htmx 2.0.11 (npm htmx.org-2.0.11.tgz,
// dist/htmx.min.js, integrity sha512-Thx/WtpeOQqSrqBCw/A1cwGJGg4U...).
const htmxSHA256 = "d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717"

func TestVendoredHTMXIsPinned(t *testing.T) {
	b, err := staticFS.ReadFile("static/htmx.min.js")
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != htmxSHA256 {
		t.Fatalf("static/htmx.min.js sha256 = %x, want %s", sum, htmxSHA256)
	}
}

// fakeForgejo is the OAuth2 provider and the API, recording what it was asked.
type fakeForgejo struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	user      string
	challenge string // from the authorize URL the test followed
	grants    []url.Values
	calls     []string // "METHOD path auth"
	issues    []map[string]any
	comments  map[string]string // path -> body
	expiresIn int
	refuse    bool // token endpoint answers 400
	tokenN    int
	failAPI   bool
}

func newFakeForgejo(t *testing.T) *fakeForgejo {
	f := &fakeForgejo{t: t, user: "sebastian", comments: map[string]string{}, expiresIn: 3600}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeForgejo) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/login/oauth/access_token" {
		if err := r.ParseForm(); err != nil {
			f.t.Errorf("token form: %v", err)
		}
		f.grants = append(f.grants, r.PostForm)
		if f.refuse {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		if r.PostForm.Get("grant_type") == "authorization_code" {
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
				http.Error(w, `{"error":"unauthorized_client","error_description":"failed PKCE code challenge"}`, http.StatusBadRequest)
				return
			}
		}
		f.tokenN++
		json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("access-%d", f.tokenN),
			"refresh_token": fmt.Sprintf("refresh-%d", f.tokenN), "token_type": "bearer", "expires_in": f.expiresIn})
		return
	}
	f.calls = append(f.calls, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	if f.failAPI {
		http.Error(w, "boom", http.StatusForbidden)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user":
		json.NewEncoder(w).Encode(map[string]any{"login": f.user})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/sf/code/labels":
		if r.URL.Query().Get("page") != "1" {
			json.NewEncoder(w).Encode([]label{})
			return
		}
		json.NewEncoder(w).Encode([]label{{ID: 1, Name: "agent"}, {ID: 2, Name: "human"}, {ID: 7, Name: "priority/p1"}, {ID: 9, Name: "state/done"}})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/sf/code/issues":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.issues = append(f.issues, body)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"number": 41})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.comments[r.URL.Path] = body["body"]
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":5}`))
	default:
		http.NotFound(w, r)
	}
}

type fakeTasks struct{ tasks []factoryd.Task }

func (s *fakeTasks) Task(_ context.Context, id string) (factoryd.Task, bool, error) {
	for _, t := range s.tasks {
		if t.ID == id {
			return t, true, nil
		}
	}
	return factoryd.Task{}, false, nil
}

func (s *fakeTasks) BoardTasks(context.Context, time.Time) ([]factoryd.Task, error) {
	return s.tasks, nil
}

type fakeWriters struct {
	mu    sync.Mutex
	users []string
	asked int
}

func (w *fakeWriters) RepoWriters(_ context.Context, owner, repo string) ([]string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.asked++
	if owner+"/"+repo != "sf/code" {
		return nil, fmt.Errorf("unexpected repo %s/%s", owner, repo)
	}
	return w.users, nil
}

type fakeObjects map[string][]byte

func (o fakeObjects) Put(_ context.Context, k string, b []byte) error { o[k] = b; return nil }
func (o fakeObjects) Get(_ context.Context, k string) ([]byte, error) { return o[k], nil }

type fakeApprovals []Approval

func (a fakeApprovals) PendingApprovals(context.Context) ([]Approval, error) { return a, nil }

type rig struct {
	c       *Console
	fg      *fakeForgejo
	tasks   *fakeTasks
	writers *fakeWriters
	objects fakeObjects
}

func newRig(t *testing.T) *rig {
	fg := newFakeForgejo(t)
	r := &rig{fg: fg, tasks: &fakeTasks{}, writers: &fakeWriters{users: []string{"sebastian"}}, objects: fakeObjects{}}
	c, err := New(Config{
		PublicURL: "https://console.example", ForgejoURL: fg.srv.URL, ForgejoPublicURL: "https://git.example",
		ClientID: "cid", ClientSecret: "csecret", Repo: factoryd.Repo{Owner: "sf", Name: "code"},
		Tasks: r.tasks, Writers: r.writers, Timelines: r.objects,
		Approvals: fakeApprovals{{ID: 3, Kind: "resource", TaskID: "t-res", Summary: "cx33 for load test", MonthlyEUR: 12.5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.c = c
	return r
}

func (r *rig) do(req *http.Request, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	r.c.ServeHTTP(rec, req)
	return rec
}

func cookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == name {
			return ck
		}
	}
	return nil
}

// login runs /login and the callback as Forgejo would answer them and
// returns the session cookie (nil when none was set) and the callback's
// response.
func (r *rig) login(t *testing.T) (*http.Cookie, *httptest.ResponseRecorder) {
	t.Helper()
	rec := r.do(httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("/login = %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	r.fg.mu.Lock()
	r.fg.challenge = loc.Query().Get("code_challenge")
	r.fg.mu.Unlock()
	state := cookie(t, rec, loginCookie)
	cb := r.do(httptest.NewRequest(http.MethodGet, "/auth/callback?code=the-code&state="+url.QueryEscape(loc.Query().Get("state")), nil), state)
	return cookie(t, cb, sessionCookie), cb
}

func (r *rig) loggedIn(t *testing.T) (*http.Cookie, *session) {
	t.Helper()
	ck, cb := r.login(t)
	if ck == nil || cb.Code != http.StatusSeeOther {
		t.Fatalf("login: callback = %d, session cookie %v", cb.Code, ck)
	}
	return ck, r.c.sessions[ck.Value]
}

func post(path string, form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func TestLoginRedirectCarriesPKCEAndExplicitScope(t *testing.T) {
	r := newRig(t)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/login", nil))
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != "https://git.example/login/oauth/authorize" {
		t.Fatalf("redirect to %s, want the public Forgejo authorize URL", got)
	}
	q := loc.Query()
	want := map[string]string{"client_id": "cid", "redirect_uri": "https://console.example/auth/callback", "response_type": "code",
		"scope": "read:user write:repository write:issue", "code_challenge_method": "S256"}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if len(q.Get("code_challenge")) != 43 || q.Get("state") == "" {
		t.Errorf("code_challenge %q / state %q: want a S256 challenge and a state", q.Get("code_challenge"), q.Get("state"))
	}
	ck := cookie(t, rec, loginCookie)
	if ck == nil || ck.Value != q.Get("state") || !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteLaxMode {
		t.Fatalf("login cookie %+v: want the state, HttpOnly, Secure, Lax", ck)
	}
}

func TestLoginExchangesCodeInClusterWithVerifierAndSecret(t *testing.T) {
	r := newRig(t)
	ck, cb := r.login(t)
	if cb.Code != http.StatusSeeOther || cb.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d to %q, want 303 to /", cb.Code, cb.Header().Get("Location"))
	}
	if ck == nil || !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/" {
		t.Fatalf("session cookie %+v: want HttpOnly, Secure, Lax, path /", ck)
	}
	g := r.fg.grants[0]
	if g.Get("grant_type") != "authorization_code" || g.Get("code") != "the-code" || g.Get("client_secret") != "csecret" ||
		g.Get("redirect_uri") != "https://console.example/auth/callback" || g.Get("code_verifier") == "" {
		t.Fatalf("code exchange form %v", g)
	}
	if !strings.Contains(r.fg.calls[0], "GET /api/v1/user Bearer access-1") {
		t.Fatalf("calls %v: want the user read with the new token", r.fg.calls)
	}
	s := r.c.sessions[ck.Value]
	if s.user != "sebastian" || s.access != "access-1" || s.refresh != "refresh-1" || strings.Contains(ck.Value, "access") {
		t.Fatalf("session %+v: tokens stay server-side", s)
	}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if strings.Contains(body, "access-1") || strings.Contains(body, "refresh-1") {
		t.Fatal("a token reached the page")
	}
}

func TestCallbackRefusesAForeignState(t *testing.T) {
	r := newRig(t)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/login", nil))
	state := cookie(t, rec, loginCookie)
	cb := r.do(httptest.NewRequest(http.MethodGet, "/auth/callback?code=c&state=other", nil), state)
	if cb.Code != http.StatusBadRequest || cookie(t, cb, sessionCookie) != nil || len(r.fg.grants) != 0 {
		t.Fatalf("foreign state: %d, grants %d: want 400, no exchange, no session", cb.Code, len(r.fg.grants))
	}
	cb = r.do(httptest.NewRequest(http.MethodGet, "/auth/callback?code=c&state="+state.Value, nil))
	if cb.Code != http.StatusBadRequest {
		t.Fatalf("no state cookie: %d, want 400", cb.Code)
	}
}

func TestNonWriterGets403AndNoSession(t *testing.T) {
	r := newRig(t)
	r.fg.user = "mallory"
	ck, cb := r.login(t)
	if cb.Code != http.StatusForbidden || ck == nil || ck.MaxAge >= 0 || len(r.c.sessions) != 0 {
		t.Fatalf("non-writer: %d, cookie %+v, %d sessions: want 403, cookie deleted, none", cb.Code, ck, len(r.c.sessions))
	}
	if !strings.Contains(cb.Body.String(), "mallory cannot write sf/code") {
		t.Fatalf("403 body: %s", cb.Body)
	}
}

func TestWriterRemovedLosesTheSession(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.writers.users = []string{"someone-else"}
	r.c.writersT = time.Time{}
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck); rec.Code != http.StatusForbidden {
		t.Fatalf("GET / after removal = %d, want 403", rec.Code)
	}
	if len(r.c.sessions) != 0 {
		t.Fatal("session kept after the writer was removed")
	}
}

func TestPagesRequireASession(t *testing.T) {
	r := newRig(t)
	for _, path := range []string{"/", "/board", "/tasks/new", "/tasks/t-1/timeline"} {
		rec := r.do(httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Errorf("GET %s = %d to %q, want 303 to /login", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/board", nil)
	req.Header.Set("HX-Request", "true")
	if rec := r.do(req); rec.Code != http.StatusUnauthorized || rec.Header().Get("HX-Redirect") != "/login" {
		t.Errorf("htmx poll without session = %d, HX-Redirect %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	for _, path := range []string{"/tasks", "/tasks/t-1/answer", "/logout"} {
		if rec := r.do(post(path, url.Values{"csrf": {"x"}})); rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s = %d, want 401", path, rec.Code)
		}
	}
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/", nil), &http.Cookie{Name: sessionCookie, Value: "forged"}); rec.Code != http.StatusSeeOther {
		t.Errorf("forged session = %d, want 303", rec.Code)
	}
}

func TestPostWithoutCSRFTokenIsRefused(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	form := url.Values{"title": {"x"}, "lane": {"agent"}}
	for name, tok := range map[string]string{"missing": "", "wrong": "nope"} {
		f := url.Values{}
		for k, v := range form {
			f[k] = v
		}
		if tok != "" {
			f.Set("csrf", tok)
		}
		if rec := r.do(post("/tasks", f), ck); rec.Code != http.StatusForbidden {
			t.Errorf("%s token: %d, want 403", name, rec.Code)
		}
	}
	if len(r.fg.issues) != 0 {
		t.Fatal("an issue was filed without a valid token")
	}
	req := post("/tasks", form)
	req.Header.Set("X-CSRF-Token", s.csrf)
	if rec := r.do(req, ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("htmx header token: %d, want 303", rec.Code)
	}
}

func TestBoardRendersLanesFromTheStore(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.tasks = []factoryd.Task{
		{ID: "t-q", Kind: "agent", Status: "queued", Objective: "queued work", Repository: "sf/code", BudgetEUR: 5, IssueNumber: 11},
		{ID: "t-r", Kind: "agent", Status: "closing", Objective: "closing work", Repository: "sf/code", BudgetEUR: 5, SpentEUR: 1.234, PRNumber: 12},
		{ID: "t-e", Kind: "agent", Status: "waiting-human", Objective: "stuck work", Repository: "sf/code", BudgetEUR: 5, IssueNumber: 13},
		{ID: "t-v", Kind: "agent", Status: "in-review", Objective: "review work", Repository: "sf/code", BudgetEUR: 5},
		{ID: "t-d", Kind: "agent", Status: "done", Objective: "done work", Repository: "sf/code", BudgetEUR: 5},
		{ID: "t-f", Kind: "agent", Status: "failed", Objective: "failed work", Repository: "sf/code", BudgetEUR: 5},
		{ID: "t-h", Kind: "human", Status: "waiting-human", Objective: "human work", Repository: "sf/code", IssueNumber: 14},
		{ID: "t-hd", Kind: "human", Status: "done", Objective: "finished human work", Repository: "sf/code"},
	}
	r.objects["sessions/t-e/timeline.jsonl"] = []byte(`{"step":"intake","time":"2026-09-28T10:00:00Z"}
{"step":"escalated","reason":"old reason","comment":4}
{"step":"escalated","reason":"cannot reach the database","comment":5}
`)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	cols := []string{"Queued", "Running", "Waiting on a human", "In review", "Done", "Failed"}
	objs := []string{"queued work", "closing work", "stuck work", "review work", "done work", "failed work"}
	for i, col := range cols {
		start := strings.Index(body, ">"+col+" <span")
		if start < 0 {
			t.Fatalf("column %q missing", col)
		}
		end := strings.Index(body[start:], "</section>")
		if !strings.Contains(body[start:start+end], objs[i]) {
			t.Errorf("column %q lacks %q", col, objs[i])
		}
	}
	for _, want := range []string{
		`href="https://git.example/sf/code/issues/11"`, `href="https://git.example/sf/code/pulls/12"`,
		`href="/tasks/t-q/timeline"`, "spent 1.23 / 5.00 EUR",
		"cannot reach the database", `action="/tasks/t-e/answer"`, `name="csrf" value="` + s.csrf + `"`,
		"human work", "cx33 for load test", "12.50 EUR per month",
		`hx-get="/board"`, `hx-trigger="every 10s"`, `<script src="/static/htmx.min.js"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("board lacks %q", want)
		}
	}
	if strings.Contains(body, "old reason") || strings.Contains(body, "finished human work") {
		t.Error("board shows a superseded escalation or a finished human task")
	}
	if strings.Count(body, `action="/tasks/`) != 1 {
		t.Error("want exactly one answer box: the escalated agent task")
	}
	frag := r.do(httptest.NewRequest(http.MethodGet, "/board", nil), ck).Body.String()
	if !strings.HasPrefix(frag, `<div id="board"`) || strings.Contains(frag, "<html") || !strings.Contains(frag, "stuck work") {
		t.Fatalf("/board is not the bare board fragment: %.200s", frag)
	}
}

func TestNewTaskFilesTheIssueAsTheHumanWithTheLaneLabel(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	form := r.do(httptest.NewRequest(http.MethodGet, "/tasks/new", nil), ck).Body.String()
	if !strings.Contains(form, `<option value="priority/p1">`) || strings.Contains(form, "priority/p0") {
		t.Fatalf("form offers the existing priority labels only: %s", form)
	}
	for _, tc := range []struct {
		lane, priority string
		labels         []any
	}{
		{"agent", "", []any{1.0}},
		{"human", "priority/p1", []any{2.0, 7.0}},
	} {
		r.fg.issues, r.fg.calls = nil, nil
		rec := r.do(post("/tasks", url.Values{"csrf": {s.csrf}, "title": {" Fix the cache "}, "body": {"details"},
			"lane": {tc.lane}, "priority": {tc.priority}}), ck)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?filed=41" {
			t.Fatalf("%s: POST /tasks = %d to %q: %s", tc.lane, rec.Code, rec.Header().Get("Location"), rec.Body)
		}
		got := r.fg.issues[0]
		if got["title"] != "Fix the cache" || got["body"] != "details" || fmt.Sprint(got["labels"]) != fmt.Sprint(tc.labels) {
			t.Errorf("%s: issue %v, want labels %v", tc.lane, got, tc.labels)
		}
		for _, call := range r.fg.calls {
			if !strings.HasSuffix(call, " Bearer access-1") {
				t.Errorf("%s: call %q not made with the human's token", tc.lane, call)
			}
		}
	}
	for name, f := range map[string]url.Values{
		"no title":     {"title": {"  "}, "lane": {"agent"}},
		"bad lane":     {"title": {"x"}, "lane": {"robot"}},
		"bad priority": {"title": {"x"}, "lane": {"agent"}, "priority": {"urgent"}},
	} {
		r.fg.issues = nil
		f.Set("csrf", s.csrf)
		if rec := r.do(post("/tasks", f), ck); rec.Code != http.StatusBadRequest || len(r.fg.issues) != 0 {
			t.Errorf("%s: %d, want 400 and no issue", name, rec.Code)
		}
	}
}

func TestAnswerPostsTheCommentAsTheHuman(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.tasks = []factoryd.Task{
		{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13},
		{ID: "t-r", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 14},
	}
	rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "answer": {"use the replica"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("answer = %d: %s", rec.Code, rec.Body)
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; got != "use the replica" {
		t.Fatalf("comment = %q", got)
	}
	if last := r.fg.calls[len(r.fg.calls)-1]; last != "POST /api/v1/repos/sf/code/issues/13/comments Bearer access-1" {
		t.Fatalf("last call %q: want the comment as the human", last)
	}
	for _, id := range []string{"t-r", "t-missing"} {
		rec := r.do(post("/tasks/"+id+"/answer", url.Values{"csrf": {s.csrf}, "answer": {"x"}}), ck)
		if rec.Code == http.StatusSeeOther {
			t.Errorf("answer to %s accepted", id)
		}
	}
	if len(r.fg.comments) != 1 {
		t.Fatal("a comment went to a task not waiting for a human")
	}
}

func TestExpiredTokenIsRefreshedAndARefusedRefreshLogsOut(t *testing.T) {
	r := newRig(t)
	r.fg.expiresIn = 30 // under the one-minute margin
	ck, s := r.loggedIn(t)
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/tasks/new", nil), ck); rec.Code != http.StatusOK {
		t.Fatalf("GET /tasks/new = %d", rec.Code)
	}
	g := r.fg.grants[len(r.fg.grants)-1]
	if g.Get("grant_type") != "refresh_token" || g.Get("refresh_token") != "refresh-1" || g.Get("client_secret") != "csecret" {
		t.Fatalf("refresh grant %v", g)
	}
	if last := r.fg.calls[len(r.fg.calls)-1]; !strings.HasSuffix(last, "Bearer access-2") || s.refresh != "refresh-2" {
		t.Fatalf("after refresh: call %q, refresh %q", last, s.refresh)
	}
	r.fg.refuse = true
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/tasks/new", nil), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("refused refresh: %d to %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
	if len(r.c.sessions) != 0 {
		t.Fatal("session kept after the refresh was refused")
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	rec := r.do(post("/logout", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusSeeOther || len(r.c.sessions) != 0 {
		t.Fatalf("logout = %d, %d sessions", rec.Code, len(r.c.sessions))
	}
	if gone := cookie(t, rec, sessionCookie); gone == nil || gone.MaxAge >= 0 {
		t.Fatalf("session cookie not deleted: %+v", gone)
	}
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("old cookie after logout = %d, want 303", rec.Code)
	}
}

func TestTimelinePageAndStaticAssets(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = []factoryd.Task{{ID: "t-1", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "work"}}
	r.objects["sessions/t-1/timeline.jsonl"] = []byte(`{"step":"session_started","time":"2026-09-28T10:00:00Z","session":"s-<b>"}` + "\n")
	body := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil), ck).Body.String()
	if !strings.Contains(body, "session_started") || !strings.Contains(body, "s-\\u003cb\\u003e") && !strings.Contains(body, "s-&lt;b&gt;") {
		t.Fatalf("timeline page: %s", body)
	}
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-none/timeline", nil), ck); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task timeline = %d", rec.Code)
	}
	for _, name := range []string{"htmx.min.js", "console.js", "console.css"} {
		rec := r.do(httptest.NewRequest(http.MethodGet, "/static/"+name, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("/static/%s = %d", name, rec.Code)
		}
	}
	if csp := r.do(httptest.NewRequest(http.MethodGet, "/login", nil)).Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP %q", csp)
	}
}
