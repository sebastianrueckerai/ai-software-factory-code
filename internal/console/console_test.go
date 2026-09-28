package console

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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

	reviews    []map[string]string
	failReview bool
	closed     []string // "path state"
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
		// two a page, as a Forgejo whose MAX_RESPONSE_ITEMS is below the
		// limit asked for; an empty page ends the list
		pages := map[string][]factoryd.RepoLabel{
			"1": {{ID: 1, Name: "agent"}, {ID: 2, Name: "human"}},
			"2": {{ID: 7, Name: "priority/p1"}, {ID: 9, Name: "state/done"}},
		}
		json.NewEncoder(w).Encode(append([]factoryd.RepoLabel{}, pages[r.URL.Query().Get("page")]...))
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/sf/code/issues":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.issues = append(f.issues, body)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"number": 41})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews"):
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.reviews = append(f.reviews, body)
		if f.failReview {
			http.Error(w, `{"message":"review refused"}`, http.StatusUnprocessableEntity)
			return
		}
		w.Write([]byte(`{"id":12}`))
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/v1/repos/sf/code/pulls/"):
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.closed = append(f.closed, r.URL.Path+" "+body["state"])
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{}`))
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

type fakeTasks struct{ tasks []factoryd.BoardTask }

func (s *fakeTasks) Task(_ context.Context, id string) (factoryd.Task, bool, error) {
	for _, t := range s.tasks {
		if t.ID == id {
			return t.Task, true, nil
		}
	}
	return factoryd.Task{}, false, nil
}

func (s *fakeTasks) BoardTasks(_ context.Context, _ time.Time, limit int) ([]factoryd.BoardTask, error) {
	return s.tasks[:min(limit, len(s.tasks))], nil
}

// set replaces the tasks; the escalated ones carry reason.
func (s *fakeTasks) set(reason string, tasks ...factoryd.Task) {
	s.tasks = nil
	for _, t := range tasks {
		b := factoryd.BoardTask{Task: t}
		if t.Status == "waiting-human" {
			b.Escalation = reason
		}
		s.tasks = append(s.tasks, b)
	}
}

type fakeWriters struct {
	mu    sync.Mutex
	users []string
	asked int
	err   error
	delay time.Duration
}

func (w *fakeWriters) RepoWriters(_ context.Context, owner, repo string) ([]string, error) {
	time.Sleep(w.delay)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.asked++
	if w.err != nil {
		return nil, w.err
	}
	if owner+"/"+repo != "sf/code" {
		return nil, fmt.Errorf("unexpected repo %s/%s", owner, repo)
	}
	return w.users, nil
}

type fakeObjects map[string][]byte

func (o fakeObjects) Put(_ context.Context, k string, b []byte) error { o[k] = b; return nil }
func (o fakeObjects) Get(_ context.Context, k string) ([]byte, error) { return o[k], nil }

// fakeApprovals is the approval_requests table.
type fakeApprovals struct {
	mu   sync.Mutex
	rows []factoryd.ApprovalRequest
}

func (a *fakeApprovals) ApprovalRequests(_ context.Context, status string) ([]factoryd.ApprovalRequest, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []factoryd.ApprovalRequest
	for _, r := range a.rows {
		if r.Status == status {
			out = append(out, r)
		}
	}
	return out, nil
}

func (a *fakeApprovals) DecideApproval(_ context.Context, id int64, kind, status, by, note string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, r := range a.rows {
		if r.ID == id && r.Kind == kind && r.Status == "pending" {
			a.rows[i].Status, a.rows[i].DecidedBy, a.rows[i].DecisionNote = status, by, note
			return nil
		}
	}
	return factoryd.ErrNotFound
}

func (a *fakeApprovals) get(id int64) factoryd.ApprovalRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.rows {
		if r.ID == id {
			return r
		}
	}
	return factoryd.ApprovalRequest{}
}

// fakeResources is cloudres.AgentStore: it records the calls.
type fakeResources struct {
	rows  []cloudres.Resource
	envs  []cloudres.EnvelopeRecord
	calls []string
	set   map[string]policy.Envelope

	approveErr error
}

func (f *fakeResources) Commissions(_ context.Context, since time.Time) ([]cloudres.Resource, error) {
	f.calls = append(f.calls, "commissions")
	return f.rows, nil
}

func (f *fakeResources) Envelopes(context.Context) ([]cloudres.EnvelopeRecord, error) {
	return f.envs, nil
}

func (f *fakeResources) SetEnvelope(_ context.Context, project string, env policy.Envelope, by string) error {
	f.calls = append(f.calls, "set "+project+" by "+by)
	f.set[project] = env
	return nil
}

func (f *fakeResources) ApproveForProject(_ context.Context, id int64, by string) (policy.Envelope, error) {
	f.calls = append(f.calls, fmt.Sprintf("project-approve %d by %s", id, by))
	return policy.Envelope{}, f.approveErr
}

func (f *fakeResources) Promote(_ context.Context, id int64, by string) error {
	f.calls = append(f.calls, fmt.Sprintf("promote %d by %s", id, by))
	if id == 410 {
		return fmt.Errorf("resource %d: %w", id, cloudres.ErrLeaseEnded)
	}
	if id == 500 {
		return errors.New(`pq: deadlock detected in "cloud_resources"`)
	}
	return nil
}

func (f *fakeResources) EndLease(_ context.Context, id int64, by string) error {
	f.calls = append(f.calls, fmt.Sprintf("release %d by %s", id, by))
	if id == 404 {
		return fmt.Errorf("resource %d: %w", id, cloudres.ErrNotOpen)
	}
	return nil
}

type fakeLedger struct {
	from, to time.Time
	costs    policy.Costs
}

func (l *fakeLedger) Costs(_ context.Context, from, to time.Time) (policy.Costs, error) {
	l.from, l.to = from, to
	return l.costs, nil
}

func (l *fakeLedger) CurrentPolicy(context.Context) (policy.Version, bool, error) {
	return policy.Version{ID: 4, SHA256: "c9d98bb0aa", LoadedAt: time.Date(2026, 9, 27, 22, 50, 7, 0, time.UTC),
		ActivatedAt: time.Date(2026, 9, 27, 22, 56, 38, 0, time.UTC),
		Document: policy.Document{Version: 1, Budget: policy.Budget{TaskDefaultEUR: 2, TaskMaxEUR: 5, DayEUR: 20},
			Classes:         map[string]policy.Class{"coder": {Models: []string{"factory/coding-cheap"}}},
			Approvers:       []string{"sebastian"},
			EnvelopeDefault: policy.Envelope{MonthlyEUR: 10, ServerTypes: []string{"cx23"}, MaxCount: 2, MaxLeaseHours: 4, VolumeMaxGB: 20}}}, true, nil
}

type rig struct {
	c         *Console
	fg        *fakeForgejo
	tasks     *fakeTasks
	writers   *fakeWriters
	objects   fakeObjects
	ledger    *fakeLedger
	approvals *fakeApprovals
	resources *fakeResources
	signalErr error
}

var monthly = 8.99

func newRig(t *testing.T) *rig {
	fg := newFakeForgejo(t)
	r := &rig{fg: fg, tasks: &fakeTasks{}, writers: &fakeWriters{users: []string{"sebastian"}}, objects: fakeObjects{}, ledger: &fakeLedger{},
		approvals: &fakeApprovals{rows: []factoryd.ApprovalRequest{
			{ID: 3, Kind: "resource", TaskID: "t-res", Project: "sf/code", Status: "pending", MonthlyEUR: &monthly,
				Reason: "load test (beyond the envelope: a lease of 300 minutes exceeds the envelope's 4 h)",
				Spec: json.RawMessage(`{"server_type":"cx33","purpose":"load test","lease_minutes":300,"volume_gb":0,"location":"fsn1",` +
					`"reservation_eur":0.072,"hourly_eur":0.0144,"beyond_envelope":["a lease of 300 minutes exceeds the envelope's 4 h"]}`)},
			{ID: 4, Kind: "merge", TaskID: "t-m", Project: "sf/code", PRNumber: 9, Status: "pending",
				Spec: json.RawMessage(`{"risk_class":"high","migration":true,"migrations":["internal/factoryd/migrations/0018_x.sql"],` +
					`"verdict_summary":"adds the column safely","review":{"verdict":"approve","summary":"adds the column safely"},"head_sha":"abc123"}`)},
			{ID: 5, Kind: "merge", TaskID: "t-old", Project: "sf/code", PRNumber: 8, Status: "denied"},
		}},
		resources: &fakeResources{set: map[string]policy.Envelope{}}}
	c, err := New(Config{
		PublicURL: "https://console.example", ForgejoURL: fg.srv.URL, ForgejoPublicURL: "https://git.example",
		ClientID: "cid", ClientSecret: "csecret", SessionKey: []byte("0123456789abcdef0123456789abcdef"), Repo: factoryd.Repo{Owner: "sf", Name: "code"},
		Tasks: r.tasks, Writers: r.writers, Timelines: r.objects, Policies: r.ledger, Costs: r.ledger,
		PolicyURL: "https://git.example/sf/infra/src/branch/main/k8s/platform/policy/policy.yaml",
		Approvals: r.approvals, Resources: r.resources,
		ApproveMerge: func(_ context.Context, taskID string, pr int64, approver string) error {
			fg.mu.Lock()
			defer fg.mu.Unlock()
			fg.calls = append(fg.calls, fmt.Sprintf("SIGNAL approved %s #%d by %s", taskID, pr, approver))
			return r.signalErr
		},
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
	if ck == nil || !strings.HasPrefix(ck.Value, q.Get("state")+".") || !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteLaxMode {
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
	st := strings.SplitN(state.Value, ".", 2)[0]
	cb = r.do(httptest.NewRequest(http.MethodGet, "/auth/callback?code=c&state="+st, nil))
	if cb.Code != http.StatusBadRequest {
		t.Fatalf("no state cookie: %d, want 400", cb.Code)
	}
}

// An anonymous /login keeps nothing on the server: the state and the PKCE
// verifier travel in the signed login cookie, and a cookie whose parts or
// signature were changed, or that expired, is refused before any exchange.
func TestLoginKeepsNoServerStateAndRefusesATamperedCookie(t *testing.T) {
	r := newRig(t)
	for range 1000 {
		r.do(httptest.NewRequest(http.MethodGet, "/login", nil))
	}
	if n := serverState(r.c); n != 0 {
		t.Fatalf("1000 anonymous logins left %d entries of server state", n)
	}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/login", nil))
	ck := cookie(t, rec, loginCookie)
	parts := strings.Split(ck.Value, ".")
	if len(parts) != 4 {
		t.Fatalf("login cookie %q: want state.verifier.expiry.mac", ck.Value)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	sum := sha256.Sum256([]byte(parts[1]))
	if loc.Query().Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) || loc.Query().Get("state") != parts[0] {
		t.Fatal("the cookie's verifier and state are not the ones the authorize URL carries")
	}
	flip := func(s string) string {
		if s[len(s)-1] == 'A' {
			return s[:len(s)-1] + "B"
		}
		return s[:len(s)-1] + "A"
	}
	for name, v := range map[string]string{
		"verifier swapped": strings.Join([]string{parts[0], randomToken(), parts[2], parts[3]}, "."),
		"expiry extended":  strings.Join([]string{parts[0], parts[1], "99999999999", parts[3]}, "."),
		"mac changed":      strings.Join([]string{parts[0], parts[1], parts[2], flip(parts[3])}, "."),
		"expired":          r.c.loginCookieValue(parts[0], parts[1], time.Now().Add(-time.Second)),
		"no mac":           strings.Join(parts[:3], "."),
	} {
		cb := r.do(httptest.NewRequest(http.MethodGet, "/auth/callback?code=c&state="+parts[0], nil),
			&http.Cookie{Name: loginCookie, Value: v})
		if cb.Code != http.StatusBadRequest || cookie(t, cb, sessionCookie) != nil {
			t.Errorf("%s: %d, want 400 and no session", name, cb.Code)
		}
	}
	if len(r.fg.grants) != 0 {
		t.Fatalf("a refused cookie reached the code exchange %d times", len(r.fg.grants))
	}
	if _, err := New(Config{SessionKey: []byte("short")}); err == nil {
		t.Fatal("a session key under 32 bytes was accepted")
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
	for _, path := range []string{"/", "/board", "/tasks/new", "/tasks/t-1/timeline", "/costs", "/policy"} {
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
	r.tasks.set("cannot reach the database", []factoryd.Task{
		{ID: "t-q", Kind: "agent", Status: "queued", Objective: "queued work", Repository: "sf/code", BudgetEUR: 5, IssueNumber: 11},
		{ID: "t-r", Kind: "agent", Status: "closing", Objective: "closing work", Repository: "sf/code", BudgetEUR: 5, SpentEUR: 0.000543, PRNumber: 12},
		{ID: "t-e", Kind: "agent", Status: "waiting-human", Objective: "stuck work", Repository: "sf/code", BudgetEUR: 5, IssueNumber: 13},
		{ID: "t-v", Kind: "agent", Status: "in-review", Objective: "review work", Repository: "sf/code", BudgetEUR: 5},
		{ID: "t-d", Kind: "agent", Status: "done", Objective: "done work", Repository: "sf/code", BudgetEUR: 5},
		{ID: "t-f", Kind: "agent", Status: "failed", Objective: "failed work", Repository: "sf/code", BudgetEUR: 5},
		{ID: "t-h", Kind: "human", Status: "waiting-human", Objective: "human work", Repository: "sf/code", IssueNumber: 14},
		{ID: "t-hd", Kind: "human", Status: "done", Objective: "finished human work", Repository: "sf/code"},
	}...)
	// the reason comes from the task's row; the board reads no timeline
	r.objects["sessions/t-e/timeline.jsonl"] = []byte(`{"step":"escalated","reason":"old reason","comment":4}
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
		`href="/tasks/t-q/timeline"`, "spent 0.0005 / 5.0000 EUR",
		"cannot reach the database", `action="/tasks/t-e/answer"`, `name="csrf" value="` + s.csrf + `"`,
		"human work", "load test", "8.9900 EUR per month",
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
	r.tasks.set("cannot reach the database", []factoryd.Task{
		{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13},
		{ID: "t-r", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 14},
	}...)
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
	r.fg.expiresIn = 3600 // the refreshed token lasts: one refresh serves every page read
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
	s.mu.Lock()
	s.expiry = time.Now() // the refreshed token runs out too
	s.mu.Unlock()
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
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "work"})
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

func TestCostsShowTheLastFourteenDaysAndTodayAgainstTheCap(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	today := time.Now().UTC().Format(policy.DateLayout)
	r.ledger.costs = policy.Costs{From: "2026-09-15", To: today,
		Days: []policy.DayCost{
			{Day: "2026-09-20", ByKind: map[string]float64{"model": 0.5}, Total: 0.5},
			{Day: today, ByKind: map[string]float64{"model": 1.25, "settlement": 0.5, "resource": 2}, Total: 3.75},
		},
		Tasks: []policy.TaskCost{{TaskID: "t-big", EUR: 3.25}, {TaskID: "t-small", EUR: 1}, {TaskID: "t-tiny", EUR: 0.000543}}}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/costs", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("/costs = %d: %s", rec.Code, rec.Body)
	}
	if got := r.ledger.to.Sub(r.ledger.from); got != 13*24*time.Hour {
		t.Errorf("range %v to %v: want 14 days, today included", r.ledger.from, r.ledger.to)
	}
	if r.ledger.to.UTC().Format(policy.DateLayout) != today {
		t.Errorf("range ends %v, want today (UTC)", r.ledger.to)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<strong>3.7500 EUR</strong> of the 20.0000 EUR day cap",
		"<th scope=\"row\">" + today + "</th><td>1.2500</td><td>0.0000</td><td>2.0000</td><td>0.5000</td><td>3.7500</td>",
		"<th scope=\"row\">2026-09-20</th><td>0.5000</td>",
		`<a href="/tasks/t-big/timeline">t-big</a></th><td>3.2500</td>`,
		`<a href="/tasks/t-tiny/timeline">t-tiny</a></th><td>0.0005</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/costs lacks %q", want)
		}
	}
	if strings.Index(body, today) > strings.Index(body, "2026-09-20") {
		t.Error("days are not newest first")
	}
}

func TestPolicyShowsTheCurrentVersionAndLinksToGit(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/policy", nil), ck).Body.String()
	for _, want := range []string{"<dd>4</dd>", "c9d98bb0aa", "2026-09-27T22:50:07Z", "2026-09-27T22:56:38Z", `&#34;day_eur&#34;: 20`,
		`href="https://git.example/sf/infra/src/branch/main/k8s/platform/policy/policy.yaml"`} {
		if !strings.Contains(body, want) {
			t.Errorf("/policy lacks %q", want)
		}
	}
}

func TestBoardShowsApprovalCardsOfBothKinds(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-res", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 21})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	start := strings.Index(body, `id="col-approvals"`)
	end := strings.Index(body[start:], "</section>")
	lane := body[start : start+end]
	for _, want := range []string{
		// the resource request: type, purpose, lease, monthly cost, project, the task
		"Resource request", "<dd>cx33</dd>", "<dd>load test</dd>", "<dd>5 h</dd>", "8.9900 EUR per month; this lease 0.0720 EUR",
		"<dd>sf/code</dd>", "a lease of 300 minutes exceeds the envelope&#39;s 4 h", `href="https://git.example/sf/code/issues/21"`,
		`href="/tasks/t-res/timeline"`, `action="/approvals/3/approve"`, `name="scope" value="task" checked`, `name="scope" value="project"`,
		// the merge request: the PR, the risk class, the migration, the review
		`Merge <a href="https://git.example/sf/code/pulls/9">PR #9</a>`, "<dd>high</dd>",
		"yes, human review required: <code>internal/factoryd/migrations/0018_x.sql</code>", "<dd>approve: adds the column safely</dd>",
		`action="/approvals/4/approve"`, `action="/approvals/4/deny"`, `name="action" value="request_changes" checked`, `name="action" value="reject"`,
		`name="csrf" value="` + s.csrf + `"`, `<label for="note-4">`, `<textarea id="note-4" name="note"`,
	} {
		if !strings.Contains(lane, want) {
			t.Errorf("approval lane lacks %q", want)
		}
	}
	if strings.Contains(lane, "/approvals/5/") || !strings.Contains(lane, `Approvals <span class="count">2</span>`) {
		t.Error("the lane shows a decided request")
	}
}

// Approve submits the human's APPROVED review at the gated head through
// their token, and only then signals the workflow.
func TestMergeApproveReviewsThenSignals(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.fg.calls = nil
	rec := r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("approve = %d: %s", rec.Code, rec.Body)
	}
	want := []string{"POST /api/v1/repos/sf/code/pulls/9/reviews Bearer access-1", "SIGNAL approved t-m #9 by sebastian"}
	if fmt.Sprint(r.fg.calls) != fmt.Sprint(want) {
		t.Fatalf("calls = %q, want %q", r.fg.calls, want)
	}
	if rv := r.fg.reviews[0]; rv["event"] != "APPROVED" || rv["commit_id"] != "abc123" {
		t.Fatalf("review = %v", rv)
	}
	if r.approvals.get(4).Status != "pending" {
		t.Fatal("the console decided the merge request; factoryd's Merge records the approval once merged")
	}
}

func TestMergeApproveSignalsNothingWhenTheReviewFails(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.fg.calls, r.fg.failReview = nil, true
	rec := r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("approve with a refused review = %d", rec.Code)
	}
	for _, c := range r.fg.calls {
		if strings.HasPrefix(c, "SIGNAL") {
			t.Fatalf("signalled after a failed review: %q", r.fg.calls)
		}
	}
	// the review went through but the signal did not: say so
	r.fg.failReview, r.signalErr = false, errors.New("temporal unavailable")
	rec = r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "review is on the pull request") {
		t.Fatalf("approve with a failed signal = %d: %s", rec.Code, rec.Body)
	}
}

func TestMergeDenyRequestsChangesOrRejectsAsTheHuman(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.fg.calls = nil
	for name, f := range map[string]url.Values{
		"no reason":  {"action": {"request_changes"}, "note": {"  "}},
		"bad action": {"action": {"merge-anyway"}, "note": {"x"}},
	} {
		f.Set("csrf", s.csrf)
		if rec := r.do(post("/approvals/4/deny", f), ck); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, rec.Code)
		}
	}
	if len(r.fg.calls) != 0 || r.approvals.get(4).Status != "pending" {
		t.Fatalf("a refused deny acted: %q", r.fg.calls)
	}
	rec := r.do(post("/approvals/4/deny", url.Values{"csrf": {s.csrf}, "action": {"request_changes"}, "note": {"split the migration"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("request changes = %d: %s", rec.Code, rec.Body)
	}
	if rv := r.fg.reviews[0]; rv["event"] != "REQUEST_CHANGES" || rv["body"] != "split the migration" || rv["commit_id"] != "abc123" ||
		r.fg.calls[0] != "POST /api/v1/repos/sf/code/pulls/9/reviews Bearer access-1" {
		t.Fatalf("review = %v, calls %q", rv, r.fg.calls)
	}
	if a := r.approvals.get(4); a.Status != "denied" || a.DecidedBy != "sebastian" || a.DecisionNote != "changes requested: split the migration" {
		t.Fatalf("request = %+v", a)
	}
	if rec := r.do(post("/approvals/4/deny", url.Values{"csrf": {s.csrf}, "action": {"reject"}, "note": {"x"}}), ck); rec.Code != http.StatusConflict {
		t.Fatalf("deny of a decided request = %d, want 409", rec.Code)
	}

	r.approvals.rows[1].Status = "pending"
	r.fg.calls = nil
	rec = r.do(post("/approvals/4/deny", url.Values{"csrf": {s.csrf}, "action": {"reject"}, "note": {"not wanted"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reject = %d: %s", rec.Code, rec.Body)
	}
	want := []string{"POST /api/v1/repos/sf/code/issues/9/comments Bearer access-1", "PATCH /api/v1/repos/sf/code/pulls/9 Bearer access-1"}
	if fmt.Sprint(r.fg.calls) != fmt.Sprint(want) || r.fg.closed[0] != "/api/v1/repos/sf/code/pulls/9 closed" ||
		r.fg.comments["/api/v1/repos/sf/code/issues/9/comments"] != "Rejected in the factory console: not wanted" {
		t.Fatalf("calls = %q, closed %q, comments %v", r.fg.calls, r.fg.closed, r.fg.comments)
	}
	if a := r.approvals.get(4); a.Status != "denied" || a.DecisionNote != "rejected: not wanted" {
		t.Fatalf("request = %+v", a)
	}
}

func TestResourceApproveForTaskOrProjectAndDeny(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	if rec := r.do(post("/approvals/3/approve", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusBadRequest {
		t.Fatalf("approve without a scope = %d", rec.Code)
	}
	rec := r.do(post("/approvals/3/approve", url.Values{"csrf": {s.csrf}, "scope": {"task"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve for the task = %d: %s", rec.Code, rec.Body)
	}
	if a := r.approvals.get(3); a.Status != "approved" || a.DecidedBy != "sebastian" || a.DecisionNote != "approved for the task" {
		t.Fatalf("request = %+v", a)
	}
	if len(r.resources.calls) != 0 || len(r.fg.calls) != 1 { // the login's GET /user only
		t.Fatalf("a task approve touched %q / %q", r.resources.calls, r.fg.calls)
	}
	if rec := r.do(post("/approvals/3/approve", url.Values{"csrf": {s.csrf}, "scope": {"task"}}), ck); rec.Code != http.StatusConflict {
		t.Fatalf("second approve = %d, want 409", rec.Code)
	}

	r.approvals.rows[0].Status = "pending"
	rec = r.do(post("/approvals/3/approve", url.Values{"csrf": {s.csrf}, "scope": {"project"}}), ck)
	if rec.Code != http.StatusSeeOther || fmt.Sprint(r.resources.calls) != "[project-approve 3 by sebastian]" {
		t.Fatalf("approve for the project = %d, calls %q", rec.Code, r.resources.calls)
	}

	rec = r.do(post("/approvals/3/deny", url.Values{"csrf": {s.csrf}, "note": {"use a cx23"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("deny = %d: %s", rec.Code, rec.Body)
	}
	if a := r.approvals.get(3); a.Status != "denied" || a.DecidedBy != "sebastian" || a.DecisionNote != "use a cx23" {
		t.Fatalf("request = %+v", a)
	}
	if rec := r.do(post("/approvals/5/approve", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusConflict {
		t.Fatalf("approve of a denied request = %d", rec.Code)
	}
}

func TestResourcesPageListsCommissionsAndEnvelopes(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	lease := time.Date(2026, 9, 28, 14, 30, 0, 0, time.UTC)
	r.resources.rows = []cloudres.Resource{
		{ID: 10, Kind: "firewall", Repo: "sf/code", OwnerRef: "t-res", HoldID: 7, State: "live", LeaseExpires: lease},
		{ID: 11, Kind: "server", Repo: "sf/code", OwnerRef: "t-res", HoldID: 7, State: "live", Size: "cx33", Address: "203.0.113.9",
			LeaseExpires: lease, PricePerHourEUR: 0.0144, AccruedEUR: 0.0288, Purpose: "load test", MonthlyEUR: 8.99},
		{ID: 14, Kind: "volume", Repo: "sf/code", OwnerRef: "t-res", HoldID: 7, State: "live", Size: "20GB",
			LeaseExpires: lease, PricePerHourEUR: 1.144 / 730, MonthlyEUR: 1.144},
		{ID: 12, Kind: "server", Repo: "sf/code", OwnerRef: "t-res", HoldID: 8, State: "live", Size: "cx23", Promoted: true, PromotedBy: "alice"},
		{ID: 13, Kind: "server", Repo: "sf/code", OwnerRef: "t-old", HoldID: 6, State: "deleted", Size: "cx23", ReleasedBy: "sebastian"},
	}
	r.resources.envs = []cloudres.EnvelopeRecord{{Project: "sf/other", SetBy: "alice", SetAt: lease,
		Envelope: policy.Envelope{MonthlyEUR: 30, ServerTypes: []string{"cx23", "cx33"}, MaxCount: 3, MaxLeaseHours: 8},
		Use:      cloudres.EnvelopeUse{OpenServers: 1, MonthEUR: 1.5}}}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/resources", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /resources = %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<h3 id="project-sf/code">sf/code</h3>`, `<h3 id="project-sf/other">sf/other</h3>`,
		"<td>cx33</td>", "<td>203.0.113.9</td>", "<td>2026-09-28 14:30 UTC</td>", "<td>0.0144</td>", "<td>0.0288</td>", "<td>live</td>",
		"none (promoted by alice)", "released by sebastian", "<td>deleted</td>", `href="/tasks/t-res/timeline"`,
		// the commission's catalogue month: cx33 8.49 + IPv4 0.50 + 20 GB x 0.0572 = 10.134, the firewall free
		`<span id="promote-11" class="meta">promoted, about 10.1340 EUR a month against the envelope&#39;s 10.0000 EUR (0.0000 used or reserved this month)</span>`,
		// the code repository has no envelope yet: the policy default
		"Not set yet", `name="project" value="sf/code"`, `id="monthly-sf/code" name="monthly_eur" type="number" min="0" step="0.01" value="10"`,
		`name="server_types" type="text" value="cx23"`,
		"Set by alice at 2026-09-28 14:30 UTC. This month: 1.5000 EUR used or reserved, 1 servers open.",
		`name="server_types" type="text" value="cx23, cx33"`, `<label for="count-sf/other">`,
		`name="csrf" value="` + s.csrf + `"`, `<a href="/resources">Resources</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/resources lacks %q", want)
		}
	}
	// one promote (the unpromoted open commission's server) and a release
	// per open commission; none for the closed one
	if strings.Count(body, `/promote"`) != 1 || !strings.Contains(body, `action="/resources/11/promote"`) ||
		strings.Count(body, `/release"`) != 2 || !strings.Contains(body, `action="/resources/12/release"`) || strings.Contains(body, "/resources/13/") {
		t.Errorf("buttons: %d promote, %d release", strings.Count(body, `/promote"`), strings.Count(body, `/release"`))
	}
}

func TestPromoteReleaseAndEnvelopeEditRecordWho(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.resources.envs = []cloudres.EnvelopeRecord{{Project: "sf/other"}}
	if rec := r.do(post("/resources/11/promote", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/resources" {
		t.Fatalf("promote = %d", rec.Code)
	}
	if rec := r.do(post("/resources/12/release", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("release = %d", rec.Code)
	}
	if rec := r.do(post("/resources/404/release", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusConflict {
		t.Fatalf("release of a closed resource = %d", rec.Code)
	}
	if rec := r.do(post("/resources/11/promote", url.Values{}), ck); rec.Code != http.StatusForbidden {
		t.Fatalf("promote without the form token = %d", rec.Code)
	}
	if want := "[promote 11 by sebastian release 12 by sebastian release 404 by sebastian]"; fmt.Sprint(r.resources.calls) != want {
		t.Fatalf("calls = %q", r.resources.calls)
	}
	form := url.Values{"csrf": {s.csrf}, "project": {"sf/code"}, "monthly_eur": {"25.5"}, "server_types": {"cx23, cx33 cx23"},
		"max_count": {"3"}, "max_lease_hours": {"12"}, "volume_max_gb": {"0"}}
	rec := r.do(post("/envelopes", form), ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/resources?saved=sf%2Fcode" {
		t.Fatalf("save = %d to %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	want := policy.Envelope{MonthlyEUR: 25.5, ServerTypes: []string{"cx23", "cx33"}, MaxCount: 3, MaxLeaseHours: 12, VolumeMaxGB: 0}
	if got := r.resources.set["sf/code"]; fmt.Sprint(got) != fmt.Sprint(want) || r.resources.calls[3] != "set sf/code by sebastian" {
		t.Fatalf("saved %+v, calls %q", got, r.resources.calls)
	}
	form.Set("project", "sf/other")
	form.Set("server_types", "")
	if rec := r.do(post("/envelopes", form), ck); rec.Code != http.StatusSeeOther || r.resources.set["sf/other"].ServerTypes == nil {
		t.Fatalf("an envelope with no types (closed to agents) = %d, %+v", rec.Code, r.resources.set["sf/other"])
	}
	for name, change := range map[string][2]string{
		"unknown project":  {"project", "evil/repo"},
		"negative monthly": {"monthly_eur", "-1"},
		"not a number":     {"monthly_eur", "NaN"},
		"bad type":         {"server_types", "cx23; rm"},
		"fraction count":   {"max_count", "1.5"},
		"small volume":     {"volume_max_gb", "5"},
		"huge lease":       {"max_lease_hours", "100000"},
	} {
		f := url.Values{}
		for k, v := range form {
			f[k] = v
		}
		f.Set("project", "sf/code")
		f.Set(change[0], change[1])
		n := len(r.resources.calls)
		if rec := r.do(post("/envelopes", f), ck); rec.Code != http.StatusBadRequest || len(r.resources.calls) != n {
			t.Errorf("%s: %d, want 400 and nothing saved", name, rec.Code)
		}
	}
}

// serverState counts the entries of every map the console holds.
func serverState(c *Console) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	v := reflect.ValueOf(c).Elem()
	for i := range v.NumField() {
		if f := v.Field(i); f.Kind() == reflect.Map {
			n += f.Len()
		}
	}
	return n
}

// Ruling 128: only the policy's approvers may spend (approve a resource or
// a merge, edit an envelope, promote); other writers see the cards without
// those buttons, get 403, and may still deny and release.
func TestSpendActionsNeedAPolicyApprover(t *testing.T) {
	r := newRig(t)
	r.fg.user, r.writers.users = "alice", []string{"sebastian", "alice"}
	ck, s := r.loggedIn(t)
	r.resources.rows = []cloudres.Resource{{ID: 11, Kind: "server", Repo: "sf/code", OwnerRef: "t-res", HoldID: 7, State: "live",
		LeaseExpires: time.Now().Add(time.Hour)}}
	board := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if strings.Contains(board, "/approve\"") || !strings.Contains(board, `action="/approvals/3/deny"`) ||
		!strings.Contains(board, "Only the policy's approvers may approve.") {
		t.Fatalf("board for a non-approver: approve buttons %v, deny %v", strings.Contains(board, "/approve\""),
			strings.Contains(board, "/deny\""))
	}
	page := r.do(httptest.NewRequest(http.MethodGet, "/resources", nil), ck).Body.String()
	if strings.Contains(page, "/promote\"") || strings.Contains(page, "Save the envelope") || !strings.Contains(page, "/resources/11/release\"") {
		t.Fatal("resources for a non-approver offer promote or the envelope save, or lack release")
	}
	r.fg.calls = nil
	for _, req := range []*http.Request{
		post("/approvals/3/approve", url.Values{"csrf": {s.csrf}, "scope": {"task"}}),
		post("/approvals/3/approve", url.Values{"csrf": {s.csrf}, "scope": {"project"}}),
		post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}),
		post("/resources/11/promote", url.Values{"csrf": {s.csrf}}),
		post("/envelopes", url.Values{"csrf": {s.csrf}, "project": {"sf/code"}, "monthly_eur": {"99"}, "server_types": {"cx23"},
			"max_count": {"9"}, "max_lease_hours": {"9"}, "volume_max_gb": {"0"}}),
	} {
		if rec := r.do(req, ck); rec.Code != http.StatusForbidden {
			t.Errorf("%s by a non-approver = %d, want 403", req.URL.Path, rec.Code)
		}
	}
	if len(r.fg.calls) != 0 || len(r.resources.calls) != 1 || r.approvals.get(3).Status != "pending" || r.approvals.get(4).Status != "pending" {
		t.Fatalf("a refused spend acted: forgejo %q, resources %q", r.fg.calls, r.resources.calls)
	}
	if rec := r.do(post("/resources/11/release", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("release by a writer = %d", rec.Code)
	}
	if rec := r.do(post("/approvals/3/deny", url.Values{"csrf": {s.csrf}, "note": {"no"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("deny by a writer = %d", rec.Code)
	}
}

// A merge Forgejo refused after the approve says so on its card.
func TestMergeCardShowsWhyItHasNotMerged(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.approvals.rows[1].DecisionNote = "waiting: Forgejo refused the merge after sebastian's approval: status 405"
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if !strings.Contains(body, `<p class="problem" role="status">waiting: Forgejo refused the merge after sebastian&#39;s approval: status 405</p>`) {
		t.Fatal("the card does not show the refusal")
	}
}

// Pages say what failed in a fixed sentence; the details go to the log.
func TestErrorsShowNoInternals(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	rec := r.do(post("/resources/410/promote", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "lease has ended or it was released") {
		t.Fatalf("promote after release = %d: %s", rec.Code, rec.Body)
	}
	rec = r.do(post("/resources/500/promote", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "pq:") {
		t.Fatalf("a store error = %d: %s", rec.Code, rec.Body)
	}
	r.resources.approveErr = errors.New(`pq: could not serialize access due to concurrent update`)
	rec = r.do(post("/approvals/3/approve", url.Values{"csrf": {s.csrf}, "scope": {"project"}}), ck)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "pq:") {
		t.Fatalf("a project approve error = %d: %s", rec.Code, rec.Body)
	}
}

// Each repeated button names its row for screen readers.
func TestRepeatedButtonsNameTheirRow(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.resources.rows = []cloudres.Resource{{ID: 11, Kind: "server", Repo: "sf/code", OwnerRef: "t-res", HoldID: 7, State: "live",
		Size: "cx33", LeaseExpires: time.Now().Add(time.Hour)}}
	board := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	page := r.do(httptest.NewRequest(http.MethodGet, "/resources", nil), ck).Body.String()
	for _, want := range []string{
		`aria-label="Approve resource request 3 of t-res"`, `aria-label="Deny resource request 3 of t-res"`,
		`aria-label="Approve as you: merge PR #9 of t-m"`, `aria-label="Deny merge PR #9 of t-m"`,
	} {
		if !strings.Contains(board, want) {
			t.Errorf("board lacks %s", want)
		}
	}
	for _, want := range []string{`aria-label="Promote cx33 server 11 of t-res"`, `aria-label="Release cx33 server 11 of t-res"`} {
		if !strings.Contains(page, want) {
			t.Errorf("/resources lacks %s", want)
		}
	}
}

// The writer list is fetched once for concurrent requests when it expires,
// and a Forgejo error serves the last good list for up to five minutes.
func TestWriterListRefreshIsSharedAndSurvivesABlip(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.writers.mu.Lock()
	r.writers.asked, r.writers.delay = 0, 50*time.Millisecond
	r.writers.mu.Unlock()
	r.c.mu.Lock()
	r.c.writersT = time.Time{}.Add(time.Hour) // expired, yet a list is known
	r.c.mu.Unlock()
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.do(httptest.NewRequest(http.MethodGet, "/policy", nil), ck)
		}()
	}
	wg.Wait()
	if r.writers.asked != 1 {
		t.Fatalf("20 concurrent requests fetched the writers %d times", r.writers.asked)
	}
	r.writers.delay, r.writers.err = 0, errors.New("forgejo: 502")
	r.c.mu.Lock()
	r.c.writersT = time.Now().Add(-4 * time.Minute)
	r.c.mu.Unlock()
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/policy", nil), ck); rec.Code != http.StatusOK {
		t.Fatalf("a blip with a 4-minute-old list = %d, want the stale list served", rec.Code)
	}
	r.c.mu.Lock()
	r.c.writersT = time.Now().Add(-6 * time.Minute)
	r.c.mu.Unlock()
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/policy", nil), ck); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a blip with a 6-minute-old list = %d, want 503", rec.Code)
	}
}

func TestResponsesCarryHSTS(t *testing.T) {
	r := newRig(t)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/login", nil))
	if got := rec.Header().Get("Strict-Transport-Security"); got != "max-age=31536000; includeSubDomains" {
		t.Fatalf("Strict-Transport-Security = %q", got)
	}
}
