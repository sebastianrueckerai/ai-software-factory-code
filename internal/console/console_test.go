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
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
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
	labelled   []string // "path [label ids]"
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
			"3": {{ID: 3, Name: "backlog"}},
		}
		json.NewEncoder(w).Encode(append([]factoryd.RepoLabel{}, pages[r.URL.Query().Get("page")]...))
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/linkshort/linkshort/labels":
		pages := map[string][]factoryd.RepoLabel{
			"1": {{ID: 21, Name: "agent"}, {ID: 22, Name: "human"}},
			"2": {{ID: 23, Name: "backlog"}, {ID: 24, Name: "plan"}},
		}
		json.NewEncoder(w).Encode(append([]factoryd.RepoLabel{}, pages[r.URL.Query().Get("page")]...))
	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/issues/") && strings.HasSuffix(r.URL.Path, "/labels"):
		var body struct {
			Labels []int64 `json:"labels"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.labelled = append(f.labelled, fmt.Sprintf("%s %v", r.URL.Path, body.Labels))
		w.Write([]byte(`[]`))
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/linkshort/linkshort/issues":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.issues = append(f.issues, body)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"number": 42})
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

// fakeTasks is the board's task store. read is called, with the mutex held,
// on every Task read; a test may set it to change the rows while a request
// runs (item 141: factoryd ends an accepted plan and files its cards seconds
// after the console's first read of it).
type fakeTasks struct {
	mu    sync.Mutex
	tasks []factoryd.BoardTask
	plans map[string][]factoryd.PlanRevision // by task, oldest first
	read  func(s *fakeTasks, id string)
}

func (s *fakeTasks) PlanRevision(_ context.Context, id string, n int) (factoryd.PlanRevision, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	revs := s.plans[id]
	for i := len(revs) - 1; i >= 0; i-- {
		if n == 0 || revs[i].Revision == n {
			return revs[i], true, nil
		}
	}
	return factoryd.PlanRevision{}, false, nil
}

func (s *fakeTasks) PlanCards(_ context.Context, plan string) ([]factoryd.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []factoryd.Task
	for _, t := range s.tasks {
		if t.Parent == plan && t.Source == factoryd.SourcePlan {
			out = append(out, t.Task)
		}
	}
	return out, nil
}

func (s *fakeTasks) Task(_ context.Context, id string) (factoryd.Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.read != nil {
		s.read(s, id)
	}
	for _, t := range s.tasks {
		if t.ID == id {
			return t.Task, true, nil
		}
	}
	return factoryd.Task{}, false, nil
}

func (s *fakeTasks) BoardTasks(_ context.Context, _ time.Time, limit int) ([]factoryd.BoardTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tasks[:min(limit, len(s.tasks))], nil
}

// set replaces the tasks; the escalated ones carry reason.
func (s *fakeTasks) set(reason string, tasks ...factoryd.Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks = nil
	for _, t := range tasks {
		b := factoryd.BoardTask{Task: t}
		if t.Status == "waiting-human" {
			b.Escalation = reason
		}
		s.tasks = append(s.tasks, b)
	}
}

// setStatus changes task id's row to status; a read hook already holds the
// lock, so it takes none.
func (s *fakeTasks) setStatus(id, status string) {
	for i := range s.tasks {
		if s.tasks[i].ID == id {
			s.tasks[i].Status = status
			return
		}
	}
}

type fakeWriters struct {
	mu    sync.Mutex
	users []string
	asked int
	err   error
	delay time.Duration
}

func (w *fakeWriters) list(context.Context) ([]string, error) {
	time.Sleep(w.delay)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.asked++
	if w.err != nil {
		return nil, w.err
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
	rows     []cloudres.Resource
	use      map[string]cloudres.EnvelopeUse
	standing map[string]cloudres.StandingUse
	calls    []string
}

func (f *fakeResources) Commissions(_ context.Context, since time.Time) ([]cloudres.Resource, error) {
	f.calls = append(f.calls, "commissions")
	return f.rows, nil
}

func (f *fakeResources) Use(_ context.Context, repo string) (cloudres.EnvelopeUse, error) {
	return f.use[repo], nil
}

func (f *fakeResources) Standing(_ context.Context, repo string) (cloudres.StandingUse, error) {
	return f.standing[repo], nil
}

// Promote records the standing block it was given for sf/code and for
// demo-app/demo-app.
func (f *fakeResources) Promote(_ context.Context, id int64, by string, standing func(repo string) policy.Standing) error {
	f.calls = append(f.calls, fmt.Sprintf("promote %d by %s under %v and %v", id, by, standing("sf/code"), standing("demo-app/demo-app")))
	if id == 410 {
		return fmt.Errorf("resource %d: %w", id, cloudres.ErrLeaseEnded)
	}
	if id == 500 {
		return errors.New(`pq: deadlock detected in "cloud_resources"`)
	}
	if id == 420 {
		return fmt.Errorf("resource %d: %w: server type cx53 is not in the standing types [cx23]", id, cloudres.ErrBeyondStanding)
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

// fakeAlerts is the alerts table.
type fakeAlerts struct {
	rows     []factoryd.Alert
	verdicts []string // "id verdict by"
}

func (a *fakeAlerts) Alerts(_ context.Context, limit int) ([]factoryd.Alert, error) {
	return a.rows[:min(limit, len(a.rows))], nil
}

func (a *fakeAlerts) SetAlertVerdict(_ context.Context, id int64, verdict, by string) error {
	for i, r := range a.rows {
		if r.ID == id && r.Kind == factoryd.AlertEscalation && r.Verdict == "" {
			a.rows[i].Verdict, a.rows[i].ResolvedBy = verdict, by
			a.verdicts = append(a.verdicts, fmt.Sprintf("%d %s %s", id, verdict, by))
			return nil
		}
	}
	return factoryd.ErrNotFound
}

type fakeLedger struct {
	from, to  time.Time
	costs     policy.Costs
	processes map[string]policy.Process
}

// testProcesses are the shipped two processes; spec lists its lanes out of
// the board's order, which the board does not follow.
func testProcesses() map[string]policy.Process {
	stages := []policy.Stage{
		{Name: "screening", OnFindings: "human", When: "outsider"},
		{Name: "checks", OnFindings: "agent", Enabled: true},
		{Name: "merge", OnFindings: "human", Enabled: true},
	}
	return map[string]policy.Process{
		"maintenance": {Lanes: []policy.Lane{{Name: "backlog", WriteUp: "analyzer"}, {Name: "ready", Class: "coder", MovedBy: "writers"}},
			Stages: stages, Merge: policy.Merge{Queue: "none"}},
		"spec": {Lanes: []policy.Lane{{Name: "ready", Class: "coder", MovedBy: "writers"}, {Name: "backlog"},
			{Name: "plan", Class: "planner", MovedBy: "writers", Plan: &policy.PlanShape{MaxCards: 12, Foundation: true, Integration: true}}},
			Dependencies: &policy.Dependencies{WaitUntil: "done", OnFailed: "human"}, Stages: stages, Merge: policy.Merge{Queue: "project"}},
	}
}

func (l *fakeLedger) Costs(_ context.Context, from, to time.Time) (policy.Costs, error) {
	l.from, l.to = from, to
	return l.costs, nil
}

func (l *fakeLedger) CurrentPolicy(context.Context) (policy.Version, bool, error) {
	return policy.Version{ID: 4, Commit: "c9d98bb0aa11", LoadedAt: time.Date(2026, 9, 27, 22, 50, 7, 0, time.UTC),
		ActivatedAt: time.Date(2026, 9, 27, 22, 56, 38, 0, time.UTC),
		Tree: policy.Tree{Document: policy.Document{Version: 1, Budget: policy.Budget{TaskDefaultEUR: 2, TaskMaxEUR: 5, DayEUR: 20, MonthEUR: 50},
			Classes:         map[string]policy.Class{"coder": {Models: []string{"factory/coding-cheap"}}},
			Limits:          policy.Limits{Sessions: 3, AgentTime: 20 * time.Minute, SupervisorCalls: 3, SupervisorTimeout: 6 * time.Minute, StrongerAfter: 1},
			Approvers:       []string{"sebastian"},
			EnvelopeDefault: policy.Envelope{MonthlyEUR: 10, ServerTypes: []string{"cx23"}, MaxCount: 2, MaxLeaseHours: 4, VolumeMaxGB: 20}},
			Projects: map[string]policy.ProjectConfig{
				"demo-app": {Repository: "demo-app/demo-app", MergeRule: "auto", Objects: "factory", Agents: "coder", Namespaces: []string{"demo-staging", "demo-prod"},
					Envelope: &policy.Envelope{MonthlyEUR: 30, ServerTypes: []string{"cx23", "cx33"}, MaxCount: 3, MaxLeaseHours: 8},
					Standing: &policy.Standing{MonthlyEUR: 12, ServerTypes: []string{"cx23"}, MaxCount: 2}},
				"infra": {Repository: "sf/infra", MergeRule: "human", Objects: "seed", Agents: "none"},
			},
			Processes: l.processes}}, true, nil
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
	alerts    *fakeAlerts
	projects  *fakeProjects
	// repoWriters are the writers of each project's repository, which
	// MayMove reads for a lane moved by writers
	repoWriters map[string][]string
	mayAsked    []string // "repository user moved_by" per MayMove
	mayErr      error
	signalErr   error
	retried     []string // "task by user" per RetryPlanCard
	retryErr    error
	metrics     *sdkmetric.ManualReader
}

var monthly = 8.99

func newRig(t *testing.T) *rig {
	fg := newFakeForgejo(t)
	reader := sdkmetric.NewManualReader()
	metrics, err := telemetry.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{fg: fg, tasks: &fakeTasks{}, writers: &fakeWriters{users: []string{"sebastian"}}, objects: fakeObjects{}, ledger: &fakeLedger{processes: testProcesses()},
		approvals: &fakeApprovals{rows: []factoryd.ApprovalRequest{
			{ID: 3, Kind: "resource", TaskID: "t-res", Project: "sf/code", Status: "pending", MonthlyEUR: &monthly,
				Reason: "load test (beyond the envelope: a lease of 300 minutes exceeds the envelope's 4 h)",
				Spec: json.RawMessage(`{"server_type":"cx33","purpose":"load test","lease_minutes":300,"volume_gb":0,"location":"fsn1",` +
					`"reservation_eur":0.072,"hourly_eur":0.0144,"beyond_envelope":["a lease of 300 minutes exceeds the envelope's 4 h"]}`)},
			{ID: 4, Kind: "merge", TaskID: "t-m", Project: "sf/code", PRNumber: 9, Status: "pending",
				Spec: json.RawMessage(`{"risk_class":"high","migration":true,"migrations":["internal/factoryd/migrations/0018_x.sql"],` +
					`"verdict_summary":"adds the column safely","review":{"verdict":"approve","summary":"adds the column safely"},"head_sha":"abc123",` +
					`"recommendation":"Adds the column; merge it.","why_human":["the project's merge rule is human","risk class high is requires_approval"],` +
					`"earlier_reviews":[{"verdict":"concerns","summary":"the column lacks a default","findings":["0018_x.sql: add a default"]}],` +
					`"audit":{"verdict":"findings","model":"coding-frontier","deployment":"zai/glm-5.3","findings":[` +
					`{"item":"secret in the diff: private key","file":"internal/x/x_test.go","line":14,"why":"an added line has the shape of a private key (pre-check)"},` +
					`{"item":"the audit gave no verdict","why":"the answer is not one JSON object: EOF"}]}}`)},
			{ID: 5, Kind: "merge", TaskID: "t-old", Project: "sf/code", PRNumber: 8, Status: "denied"},
		}},
		resources: &fakeResources{}, alerts: &fakeAlerts{}, projects: &fakeProjects{rows: projectRows()}, metrics: reader,
		repoWriters: map[string][]string{"sf/code": {"sebastian"}, "demo-app/demo-app": {"sebastian"}, "linkshort/linkshort": {"sebastian"}}}
	c, err := New(Config{
		Metrics:   metrics,
		PublicURL: "https://console.example", ForgejoURL: fg.srv.URL, ForgejoPublicURL: "https://git.example",
		ClientID: "cid", ClientSecret: "csecret", SessionKey: []byte("0123456789abcdef0123456789abcdef"),
		Tasks: r.tasks, Writers: r.writers.list, Timelines: r.objects, Policies: r.ledger, Costs: r.ledger,
		ConfigRepo: "https://git.example/sf/infra", ConfigDir: "factory",
		Approvals: r.approvals, Resources: r.resources, Alerts: r.alerts,
		Projects: r.projects,
		ApproveMerge: func(_ context.Context, taskID string, pr int64, approver string) error {
			fg.mu.Lock()
			defer fg.mu.Unlock()
			fg.calls = append(fg.calls, fmt.Sprintf("SIGNAL approved %s #%d by %s", taskID, pr, approver))
			return r.signalErr
		},
		MayMove: func(_ context.Context, repository, user, movedBy string) (bool, error) {
			r.mayAsked = append(r.mayAsked, repository+" "+user+" "+movedBy)
			if r.mayErr != nil {
				return false, r.mayErr
			}
			if movedBy == policy.MovedByApprovers {
				return user == "sebastian", nil
			}
			return slices.Contains(r.repoWriters[repository], user), nil
		},
		AcceptPlan: func(ctx context.Context, taskID string, revision int, sha, by string) error {
			newest, _, _ := r.tasks.PlanRevision(ctx, taskID, 0)
			if newest.Revision != revision || newest.SHA256 != sha {
				return factoryd.ErrStalePlan{Newest: newest.Revision}
			}
			fg.mu.Lock()
			defer fg.mu.Unlock()
			fg.calls = append(fg.calls, fmt.Sprintf("SIGNAL plan_accepted %s %d %s by %s", taskID, revision, sha, by))
			return nil
		},
		RejectPlan: func(_ context.Context, taskID, by, why string) error {
			fg.mu.Lock()
			defer fg.mu.Unlock()
			fg.calls = append(fg.calls, fmt.Sprintf("SIGNAL plan_rejected %s by %s: %s", taskID, by, why))
			return nil
		},
		RetryPlanCard: func(_ context.Context, taskID, by string) (int64, error) {
			r.retried = append(r.retried, taskID+" by "+by)
			return 9, r.retryErr
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
	if !strings.Contains(cb.Body.String(), "mallory cannot write the factory's own repository") {
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
	form := url.Values{"title": {"x"}, "lane": {"ready"}, "project": {"ai-software-factory-code"}}
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
	cols := []string{"Ready", "Running", "Waiting on a human", "In review", "Done", "Failed"}
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
		`hx-get="/board"`, `hx-trigger="every 3s"`, `<script src="/static/htmx.min.js"`,
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
	withLinkshort(r)
	ck, s := r.loggedIn(t)
	form := r.do(httptest.NewRequest(http.MethodGet, "/tasks/new", nil), ck).Body.String()
	if !strings.Contains(form, `<option value="priority/p1">`) || strings.Contains(form, "priority/p0") {
		t.Fatalf("form offers the existing priority labels only: %s", form)
	}
	// ruling 179: a picker over the projects agents work on, the first the
	// default; the infra project (agents none) is none of them
	picker := `<option value="ai-software-factory-code">ai-software-factory-code (sf/code)</option><option value="demo-app">demo-app (demo-app/demo-app)</option>`
	if !strings.Contains(form, picker) || strings.Contains(form, "infra") {
		t.Fatalf("form's projects: %s", form)
	}
	// section 5: the lane select holds backlog and the starting lanes of
	// the projects' processes, ready by default, and the human lane
	for _, want := range []string{
		`<option value="backlog" data-projects="ai-software-factory-code demo-app linkshort">Backlog</option>`,
		`<option value="plan" data-projects="linkshort">Plan</option>`,
		`<option value="ready" data-projects="ai-software-factory-code demo-app linkshort" selected>Ready</option>`,
		`<option value="human">Human lane</option>`,
	} {
		if !strings.Contains(form, want) {
			t.Errorf("the lane select lacks %q", want)
		}
	}
	for _, tc := range []struct {
		project, lane, priority string
		labels                  []any
		filed                   string
	}{
		{"ai-software-factory-code", "ready", "", []any{1.0}, "41"},
		{"ai-software-factory-code", "human", "priority/p1", []any{2.0, 7.0}, "41"},
		{"ai-software-factory-code", "backlog", "", []any{3.0}, "41"},
		{"linkshort", "plan", "", []any{24.0}, "42"},
	} {
		r.fg.issues, r.fg.calls = nil, nil
		rec := r.do(post("/tasks", url.Values{"csrf": {s.csrf}, "project": {tc.project}, "title": {" Fix the cache "}, "body": {"details"},
			"lane": {tc.lane}, "priority": {tc.priority}}), ck)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?filed="+tc.filed {
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
		"no title":     {"title": {"  "}, "lane": {"ready"}},
		"bad lane":     {"title": {"x"}, "lane": {"robot"}},
		"old lane":     {"title": {"x"}, "lane": {"agent"}, "project": {"ai-software-factory-code"}},
		"no plan lane": {"title": {"x"}, "lane": {"plan"}, "project": {"ai-software-factory-code"}},
		"bad priority": {"title": {"x"}, "lane": {"ready"}, "priority": {"urgent"}, "project": {"ai-software-factory-code"}},
		"no project":   {"title": {"x"}, "lane": {"ready"}},
		"agents none":  {"title": {"x"}, "lane": {"human"}, "project": {"ai-software-factory-infra"}},
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
		Tasks: []policy.TaskCost{
			{TaskID: "t-big", EUR: 3.25, Lines: []policy.CostLine{{Kind: "model", Ref: "coding-cheap", EUR: 3.125}, {Kind: "model", Ref: "review/review-frontier", EUR: 0.125}}},
			{TaskID: "t-small", EUR: 1}, {TaskID: "t-tiny", EUR: 0.000543}}}
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
		`<tr class="line"><td>model coding-cheap</td><td>3.1250</td></tr>`,
		`<tr class="line"><td>model review/review-frontier</td><td>0.1250</td></tr>`,
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

// The configuration page shows the version in force read-only: its
// commit, the policy, the projects and the processes, each linked to its
// file at that commit.
func TestPolicyShowsTheCurrentVersionAndLinksToGit(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/policy", nil), ck).Body.String()
	at := "https://git.example/sf/infra/src/commit/c9d98bb0aa11/factory"
	for _, want := range []string{"<dd>4</dd>", "2026-09-27T22:50:07Z", "2026-09-27T22:56:38Z", `&#34;day_eur&#34;: 20`,
		`&#34;agent_time&#34;: &#34;20m0s&#34;`,
		`<a href="` + at + `"><code>c9d98bb0aa11</code></a>`,
		`<a href="` + at + `/policy.yaml">policy.yaml</a>`,
		`<h3>Process maintenance (<a href="` + at + `/processes/maintenance.yaml">processes/maintenance.yaml</a>)</h3>`,
		`<a href="` + at + `/projects/demo-app.yaml">demo-app</a>`,
		`<a href="` + at + `/projects/infra.yaml">infra</a>`,
		"<td>sf/infra</td><td>human</td><td>seed</td><td>none</td><td></td>",
		"<td>demo-app/demo-app</td><td>auto</td><td>factory</td><td>coder</td><td>demo-staging, demo-prod</td>",
		"<td>screening</td><td>human</td><td>no</td><td>outsider</td>",
		"<td>checks</td><td>agent</td><td>yes</td><td></td>",
		`<a href="https://git.example/sf/infra/src/branch/main/factory">`} {
		if !strings.Contains(body, want) {
			t.Errorf("/policy lacks %q", want)
		}
	}
	if strings.Contains(body, "<form") && strings.Contains(body[strings.Index(body, "<h2>"):], "<form") {
		t.Error("/policy has a form: the configuration is read-only")
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
		`href="/tasks/t-res/timeline"`, `action="/approvals/3/approve"`, "Approves this task's request once; a wider envelope for the project is a change to its file in Git.",
		// the merge request: the PR, the risk class, the migration, the review
		`Merge <a href="https://git.example/sf/code/pulls/9">PR #9</a>`, "<dd>high</dd>",
		"yes, human review required: <code>internal/factoryd/migrations/0018_x.sql</code>", "<dd>approve: adds the column safely</dd>",
		// ruling 149: the agent's recommendation and why no auto merge
		`<dt>Agent's recommendation</dt><dd class="reason">Adds the column; merge it.</dd>`,
		"<dt>Why a human</dt><dd>the project&#39;s merge rule is human; risk class high is requires_approval</dd>",
		// ruling 177: the reviews of earlier heads, oldest first, with their findings
		"<dt>Earlier review 1</dt><dd>concerns: the column lacks a default<br>- 0018_x.sql: add a default</dd>",
		// ruling 188: the security audit's own section, beside the review
		"<dt>Security audit</dt><dd>findings (coding-frontier, zai/glm-5.3)<br>- secret in the diff: private key: <code>internal/x/x_test.go:14</code>: an added line has the shape of a private key (pre-check)" +
			"<br>- the audit gave no verdict: the answer is not one JSON object: EOF</dd>",
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

// A resource request is approved for its task, once; the project's
// envelope is its file's (ruling 183), no click widens it.
func TestResourceApproveForTheTaskAndDeny(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	rec := r.do(post("/approvals/3/approve", url.Values{"csrf": {s.csrf}, "scope": {"project"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve = %d: %s", rec.Code, rec.Body)
	}
	if a := r.approvals.get(3); a.Status != "approved" || a.DecidedBy != "sebastian" || a.DecisionNote != "approved for the task" {
		t.Fatalf("request = %+v", a)
	}
	if len(r.resources.calls) != 0 || len(r.fg.calls) != 1 { // the login's GET /user only
		t.Fatalf("a task approve touched %q / %q", r.resources.calls, r.fg.calls)
	}
	if rec := r.do(post("/approvals/3/approve", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusConflict {
		t.Fatalf("second approve = %d, want 409", rec.Code)
	}
	r.approvals.rows[0].Status = "pending"
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
	lease := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Minute)
	at := lease.Format("2006-01-02 15:04 UTC")
	r.resources.rows = []cloudres.Resource{
		{ID: 10, Kind: "firewall", Repo: "sf/code", OwnerRef: "t-res", HoldID: 7, State: "live", LeaseExpires: lease},
		{ID: 11, Kind: "server", Repo: "sf/code", OwnerRef: "t-res", HoldID: 7, State: "live", Size: "cx33", Address: "203.0.113.9",
			LeaseExpires: lease, PricePerHourEUR: 0.0144, AccruedEUR: 0.0288, Purpose: "load test", MonthlyEUR: 8.99},
		{ID: 14, Kind: "volume", Repo: "sf/code", OwnerRef: "t-res", HoldID: 7, State: "live", Size: "20GB",
			LeaseExpires: lease, PricePerHourEUR: 1.144 / 730, MonthlyEUR: 1.144},
		{ID: 12, Kind: "server", Repo: "sf/code", OwnerRef: "t-res", HoldID: 8, State: "live", Size: "cx23", Lifetime: cloudres.LifetimeStanding, PromotedBy: "alice"},
		{ID: 13, Kind: "server", Repo: "sf/code", OwnerRef: "t-old", HoldID: 6, State: "deleted", Size: "cx23", ReleasedBy: "sebastian"},
	}
	r.resources.use = map[string]cloudres.EnvelopeUse{"demo-app/demo-app": {OpenServers: 1, MonthEUR: 1.5}}
	r.resources.standing = map[string]cloudres.StandingUse{"demo-app/demo-app": {Count: 1, MonthlyEUR: 5.99}}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/resources", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /resources = %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<h3 id="project-sf/code">sf/code</h3>`, `<h3 id="project-demo-app/demo-app">demo-app/demo-app</h3>`, `<h3 id="project-sf/infra">sf/infra</h3>`,
		"<td>cx33</td>", "<td>203.0.113.9</td>", "<td>" + at + "</td>", "<td>0.0144</td>", "<td>0.0288</td>", "<td>live</td>",
		"none: standing (made standing by alice)", "released by sebastian", "<td>deleted</td>", `href="/tasks/t-res/timeline"`,
		// the commission's catalogue month: cx33 8.49 + IPv4 0.50 + 20 GB x 0.0572 = 10.134, the firewall free
		`<span id="promote-11" class="meta">standing, about 10.1340 EUR a month against the standing block&#39;s 0.00 EUR (0.0000 standing now)</span>`,
		// sf/code is in no file of this configuration: the policy's default
		"No project file names this repository: the policy's envelope_default applies.",
		"Task block: 10.00 EUR a month, types cx23, 2 servers at once, leases up to 4 h, volumes up to 20 GB",
		// demo-app's file, read-only, linked at the version's commit
		`The project's file sets it: <a href="https://git.example/sf/infra/src/commit/c9d98bb0aa11/factory/projects/demo-app.yaml">change it in Git</a>.`,
		"Task block: 30.00 EUR a month, types cx23, cx33, 3 servers at once, leases up to 8 h, volumes up to 0 GB",
		"This month: 1.5000 EUR used or reserved, 1 servers open",
		// the standing monthly line (ruling 147)
		"Standing block: 12.00 EUR a month, types cx23, 2 servers", "Standing now: 1 servers, 5.9900 EUR a month",
		`name="csrf" value="` + s.csrf + `"`, `<a href="/resources">Resources</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/resources lacks %q", want)
		}
	}
	if strings.Contains(body, "<form method=\"post\" action=\"/envelopes\"") || strings.Contains(body, "monthly_eur") {
		t.Error("/resources offers an envelope edit")
	}
	// one promote (the unpromoted open commission's server) and a release
	// per open commission; none for the closed one
	if strings.Count(body, `/promote"`) != 1 || !strings.Contains(body, `action="/resources/11/promote"`) ||
		strings.Count(body, `/release"`) != 2 || !strings.Contains(body, `action="/resources/12/release"`) || strings.Contains(body, "/resources/13/") {
		t.Errorf("buttons: %d promote, %d release", strings.Count(body, `/promote"`), strings.Count(body, `/release"`))
	}
}

// Promote and release record who; the promote is judged against the
// standing block of the project's file in force, the default's for a
// repository no file names; no envelope edit exists.
func TestPromoteAndReleaseRecordWho(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
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
	if want := "[promote 11 by sebastian under {0 [] 0} and {12 [cx23] 2} release 12 by sebastian release 404 by sebastian]"; fmt.Sprint(r.resources.calls) != want {
		t.Fatalf("calls = %q", r.resources.calls)
	}
	if rec := r.do(post("/envelopes", url.Values{"csrf": {s.csrf}, "project": {"sf/code"}}), ck); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /envelopes = %d, want no such route", rec.Code)
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
	if strings.Contains(page, "/promote\"") || !strings.Contains(page, "/resources/11/release\"") {
		t.Fatal("resources for a non-approver offer promote, or lack release")
	}
	r.fg.calls = nil
	for _, req := range []*http.Request{
		post("/approvals/3/approve", url.Values{"csrf": {s.csrf}}),
		post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}),
		post("/resources/11/promote", url.Values{"csrf": {s.csrf}}),
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
	// beyond the standing block: the human sees which bound and raises it first
	rec = r.do(post("/resources/420/promote", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "server type cx53 is not in the standing types [cx23]; raise the standing block in the project's file first") {
		t.Fatalf("promote beyond the standing block = %d: %s", rec.Code, rec.Body)
	}
	rec = r.do(post("/resources/500/promote", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "pq:") {
		t.Fatalf("a store error = %d: %s", rec.Code, rec.Body)
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

// The board card shows how long the task has taken in all and the gate
// it is in with the gate's age; a finished task its total only.
func TestBoardCardShowsTotalAndCurrentGate(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	now := time.Now()
	asked, since := now.Add(-(2*time.Hour + 5*time.Minute + 30*time.Second)), now.Add(-(25*time.Minute + 30*time.Second))
	finished := asked.Add(47*time.Minute + 10*time.Second)
	r.tasks.set("", factoryd.Task{ID: "t-c", Kind: "agent", Status: "in-review", Objective: "checking work", Repository: "sf/code",
		CreatedAt: asked, Gate: factoryd.GateChecks, GateSince: &since},
		factoryd.Task{ID: "t-d", Kind: "agent", Status: "done", Objective: "done work", Repository: "sf/code",
			CreatedAt: asked, FinishedAt: &finished})
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{"2 h 5 min in all", `<span class="gate">checks</span> for 25 min`, "47 min in all"} {
		if !strings.Contains(body, want) {
			t.Errorf("board lacks %q", want)
		}
	}
	if strings.Count(body, `class="gate"`) != 1 {
		t.Error("a finished task shows a gate")
	}
}

// A card in waiting-deps sits in its lane's column and reads the issues it
// waits on.
func TestBoardCardShowsWaitingOn(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-w", Kind: "agent", Status: "waiting-deps", Lane: "ready", Objective: "links api",
		Repository: "sf/code", IssueNumber: 7}, WaitingOn: "#3, #5"}}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if c := col(t, body, "Ready"); !strings.Contains(c, "links api") || !strings.Contains(c, `<p class="waiting">waiting on #3, #5</p>`) {
		t.Fatalf("Ready column = %s, want the card waiting on #3, #5", c)
	}
}

// A card waiting in its project's merge queue reads how many entries are
// ahead of it and since when it waits.
func TestBoardCardShowsMergeQueuePlace(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	since := time.Now().Add(-(6*time.Minute + 30*time.Second))
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-q", Kind: "agent", Status: "in-review", Objective: "queued work",
		Repository: "linkshort/linkshort", CreatedAt: since, Gate: factoryd.GateMergeQueue, GateSince: &since}, QueueAhead: 2}}
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if want := `<span class="gate">merge queue, 2 ahead,</span> for 6 min`; !strings.Contains(body, want) {
		t.Fatalf("board lacks %q", want)
	}
}

// The timeline page opens with a table of the gates: each step that ended
// a gate is a row from its start, the task's open gate a row "open for N
// min", CI jobs rows under their checks, and every bar is scaled to the
// longest gate.
func TestTimelineDurationsTable(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	since := time.Now().Add(-(12*time.Minute + 20*time.Second))
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "work",
		Gate: factoryd.GateApproval, GateSince: &since})
	r.objects["sessions/t-1/timeline.jsonl"] = []byte(strings.Join([]string{
		`{"step":"intake","time":"2026-09-28T10:00:30Z","started":"2026-09-28T10:00:00Z","source":"webhook"}`,
		`{"step":"session_start","time":"2026-09-28T10:01:00Z","attempt":1}`,
		`{"step":"session_end","time":"2026-09-28T10:21:00Z","started":"2026-09-28T10:01:00Z","attempt":1,"outcome":"no_push"}`,
		`{"step":"session_end","time":"2026-09-28T10:30:00Z","started":"2026-09-28T10:22:00Z","attempt":2,"outcome":"pushed"}`,
		`{"step":"pushed","time":"2026-09-28T10:30:01Z","commit":"c1"}`,
		`{"step":"checks","time":"2026-09-28T10:37:00Z","started":"2026-09-28T10:30:05Z","state":"success",` +
			`"jobs":[{"name":"test","status":"success","queue_s":30,"run_s":258},{"name":"vet","status":"success","run_s":146}]}`,
		`{"step":"gate_reproduction","time":"2026-09-28T10:39:00Z","started":"2026-09-28T10:37:10Z","exit":1,"command":"make x"}`,
		`{"step":"signal","time":"2026-09-28T10:40:00Z","signal":"pr_merged","author":"x"}`,
		`{"step":"approval_requested","time":"2026-09-28T10:41:00Z","approval":3}`,
	}, "\n") + "\n")
	body := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil), ck).Body.String()
	table := body[strings.Index(body, "<caption>Gates"):]
	table = table[:strings.Index(table, "</table>")]
	var rows []string
	for _, tr := range strings.Split(table, "<tr>")[2:] {
		cells := regexp.MustCompile(`<t[dh][^>]*>(.*?)</t[dh]>`).FindAllStringSubmatch(tr, -1)
		var row []string
		for _, c := range cells {
			row = append(row, c[1])
		}
		rows = append(rows, strings.Join(row, " | "))
	}
	// the longest is the first session: 20 min, 1200 s
	want := []string{
		`intake |  | 2026-09-28 10:00:00 | 30 s | webhook | <meter min="0" max="1200" value="30">30 s</meter>`,
		`session | 1 | 2026-09-28 10:01:00 | 20 min | no_push | <meter min="0" max="1200" value="1200">20 min</meter>`,
		`session | 2 | 2026-09-28 10:22:00 | 8 min | pushed | <meter min="0" max="1200" value="480">8 min</meter>`,
		`checks | 2 | 2026-09-28 10:30:05 | 6 min 55 s | success | <meter min="0" max="1200" value="415">6 min 55 s</meter>`,
		`job test | 2 |  | 4 min 18 s | success, queued 30 s | <meter min="0" max="1200" value="258">4 min 18 s</meter>`,
		`job vet | 2 |  | 2 min 26 s | success | <meter min="0" max="1200" value="146">2 min 26 s</meter>`,
		`reproduction | 2 | 2026-09-28 10:37:10 | 1 min 50 s | failed (exit 1) | <meter min="0" max="1200" value="110">1 min 50 s</meter>`,
		`approval | 2 | ` + since.UTC().Format("2006-01-02 15:04:05") + ` | open for 12 min | open | <meter min="0" max="1200" value="740">open for 12 min</meter>`,
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("gate rows:\n%s\nwant:\n%s", strings.Join(rows, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(body, "approval_requested") || strings.Contains(body, "style=") {
		t.Fatal("the raw steps are gone, or an inline style crept in")
	}
}

// A gate that closed after the task's row entered it is not open.
func TestTimelineClosedGateIsNotOpen(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	since := time.Date(2026, 9, 28, 10, 30, 5, 0, time.UTC)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "work",
		Gate: factoryd.GateChecks, GateSince: &since})
	r.objects["sessions/t-1/timeline.jsonl"] = []byte(`{"step":"checks","time":"2026-09-28T10:37:00Z","started":"2026-09-28T10:30:05Z","state":"failure"}` + "\n")
	body := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil), ck).Body.String()
	if strings.Contains(body, "open for") || !strings.Contains(body, "<td>failure</td>") {
		t.Fatalf("durations: %s", body[strings.Index(body, "<caption>Gates"):])
	}
}

// /alerts lists the alerts newest first; an open human alert takes a
// verdict from an approver, on the page and on the escalation's card.
func TestAlertsPageAndVerdicts(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	raised := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	resolved := raised.Add(time.Minute)
	r.alerts.rows = []factoryd.Alert{
		{ID: 3, Kind: factoryd.AlertEscalation, Source: "agent", TaskID: "t-e", RaisedAt: raised.Add(2 * time.Hour)},
		{ID: 2, Kind: factoryd.AlertSupervisorTrip, Source: "same_failure", TaskID: "t-e", RaisedAt: raised.Add(time.Hour),
			ResolvedAt: &resolved, Verdict: factoryd.VerdictNoise, ResolvedBy: "supervisor"},
		{ID: 1, Kind: factoryd.AlertGateBound, Source: "WaitChecks", TaskID: "t-c", RaisedAt: raised, BoundS: 600},
	}
	r.tasks.set("stuck", factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Objective: "stuck work", Repository: "sf/code", IssueNumber: 13})
	r.tasks.tasks[0].EscalationAlert = 3
	page := r.do(httptest.NewRequest(http.MethodGet, "/alerts", nil), ck).Body.String()
	i3, i2, i1 := strings.Index(page, "<td>escalation</td>"), strings.Index(page, "<td>supervisor_trip</td>"), strings.Index(page, "<td>gate_bound</td>")
	if i3 < 0 || i2 < i3 || i1 < i2 {
		t.Fatalf("alerts not newest first: %d %d %d", i3, i2, i1)
	}
	for _, want := range []string{`href="/tasks/t-c/timeline"`, "WaitChecks past 600 s", "noise (supervisor)", `<a href="/alerts">Alerts</a>`,
		`action="/alerts/3/verdict"`, `value="real"`, "real problem", `value="noise"`} {
		if !strings.Contains(page, want) {
			t.Errorf("/alerts lacks %q", want)
		}
	}
	if strings.Count(page, `/verdict"`) != 2 {
		t.Errorf("verdict forms on /alerts = %d, want the open human alert's two", strings.Count(page, `/verdict"`))
	}
	board := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if strings.Count(board, `action="/alerts/3/verdict"`) != 2 {
		t.Error("the escalation card lacks the two verdict buttons")
	}
	if rec := r.do(post("/alerts/3/verdict", url.Values{"csrf": {s.csrf}, "verdict": {"maybe"}}), ck); rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown verdict = %d, want 400", rec.Code)
	}
	if rec := r.do(post("/alerts/3/verdict", url.Values{"csrf": {s.csrf}, "verdict": {"real"}, "back": {"/"}}), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("verdict = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := r.do(post("/alerts/3/verdict", url.Values{"csrf": {s.csrf}, "verdict": {"noise"}}), ck); rec.Code != http.StatusConflict {
		t.Fatalf("a second verdict = %d, want 409", rec.Code)
	}
	if rec := r.do(post("/alerts/1/verdict", url.Values{"csrf": {s.csrf}, "verdict": {"noise"}}), ck); rec.Code != http.StatusConflict {
		t.Fatalf("a verdict on a machine alert = %d, want 409", rec.Code)
	}
	if !slices.Equal(r.alerts.verdicts, []string{"3 real sebastian"}) {
		t.Fatalf("verdicts = %q", r.alerts.verdicts)
	}
}

// Only the policy's approvers judge alerts: no buttons, and a 403.
func TestAlertVerdictNeedsAnApprover(t *testing.T) {
	r := newRig(t)
	r.fg.user, r.writers.users = "alice", []string{"sebastian", "alice"}
	ck, s := r.loggedIn(t)
	r.alerts.rows = []factoryd.Alert{{ID: 3, Kind: factoryd.AlertEscalation, Source: "agent", TaskID: "t-e", RaisedAt: time.Now()}}
	r.tasks.set("stuck", factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Objective: "stuck work", Repository: "sf/code", IssueNumber: 13})
	r.tasks.tasks[0].EscalationAlert = 3
	page := r.do(httptest.NewRequest(http.MethodGet, "/alerts", nil), ck).Body.String()
	board := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if strings.Contains(page, "/verdict") || strings.Contains(board, "/verdict") || !strings.Contains(page, "Only the policy's approvers may judge an alert.") {
		t.Fatal("a non-approver is offered the verdict buttons")
	}
	for _, v := range []string{"real", "maybe"} {
		if rec := r.do(post("/alerts/3/verdict", url.Values{"csrf": {s.csrf}, "verdict": {v}}), ck); rec.Code != http.StatusForbidden {
			t.Fatalf("verdict %q by a non-approver = %d, want 403", v, rec.Code)
		}
	}
	if len(r.alerts.verdicts) != 0 {
		t.Fatalf("verdicts = %q", r.alerts.verdicts)
	}
}

// A session_end from before attempts were recorded leaves the attempt
// empty, for its row and the rows after it.
func TestTimelineWithoutAttemptsShowsNone(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "work"})
	r.objects["sessions/t-1/timeline.jsonl"] = []byte(`{"step":"session_end","time":"2026-09-28T10:21:00Z","outcome":"pushed"}
{"step":"checks","time":"2026-09-28T10:37:00Z","started":"2026-09-28T10:30:05Z","state":"success"}
`)
	body := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil), ck).Body.String()
	if strings.Contains(body, "nil") || !strings.Contains(body, "<td>checks</td><td></td>") {
		t.Fatalf("durations: %s", body[strings.Index(body, "<caption>Gates"):])
	}
}

// Commissioning ends when the server went live, not when the lease loop
// put the step on the timeline.
func TestTimelineCommissioningEndsAtLive(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "work"})
	r.objects["sessions/t-1/timeline.jsonl"] = []byte(`{"step":"commissioned","time":"2026-09-28T10:01:44Z","started":"2026-09-28T10:00:00Z","live":"2026-09-28T10:01:35Z","server_type":"cx23"}
`)
	body := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil), ck).Body.String()
	if !strings.Contains(body, "<td>commissioning</td><td></td><td>2026-09-28 10:00:00</td><td>1 min 35 s</td><td>cx23</td>") {
		t.Fatalf("durations: %s", body[strings.Index(body, "<caption>Gates"):])
	}
}

// A running session's open row names its attempt from session_start.
func TestTimelineOpenSessionShowsItsAttempt(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	since := time.Now().Add(-3 * time.Minute)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "running", Repository: "sf/code", Objective: "work",
		Gate: factoryd.GateSession, GateSince: &since})
	r.objects["sessions/t-1/timeline.jsonl"] = []byte(`{"step":"session_start","time":"` + since.UTC().Format(time.RFC3339) + `","attempt":3}
`)
	body := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil), ck).Body.String()
	if !strings.Contains(body, "<td>session</td><td>3</td>") {
		t.Fatalf("durations: %s", body[strings.Index(body, "<caption>Gates"):])
	}
}

// A close, a merge in Forgejo or a change request ends the approval wait
// as well as an approval, each with its own outcome.
func TestTimelineApprovalWaitOutcomes(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "failed", Repository: "sf/code", Objective: "work"})
	var lines []string
	for _, s := range []string{`"signal":"approved","author":"sebastian"`, `"signal":"changes_requested","author":"alice"`,
		`"signal":"pr_merged","author":"bob"`, `"signal":"pr_closed","author":"bob"`, `"signal":"issue_closed","author":"sebastian"`} {
		lines = append(lines, `{"step":"signal","time":"2026-09-28T12:00:00Z","started":"2026-09-28T10:00:00Z",`+s+`}`)
	}
	r.objects["sessions/t-1/timeline.jsonl"] = []byte(strings.Join(lines, "\n") + "\n")
	body := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil), ck).Body.String()
	for _, want := range []string{"approved by sebastian", "changes requested by alice", "merged by bob", "closed"} {
		if !strings.Contains(body, "<td>2 h 0 min</td><td>"+want+"</td>") {
			t.Errorf("durations lack a 2 h approval wait ending %q", want)
		}
	}
	if n := strings.Count(body, "<td>2 h 0 min</td><td>closed</td>"); n != 2 {
		t.Errorf("%d closed approval waits, want 2 (the pull request's and the issue's)", n)
	}
}

// A machine alert shows what the supervisor did and why; a human
// escalation's detail stays off the page.
func TestAlertsShowTheSupervisorsActionAndDiagnosis(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	raised := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	r.alerts.rows = []factoryd.Alert{
		{ID: 3, Kind: factoryd.AlertEscalation, Source: "supervisor", TaskID: "t-e", RaisedAt: raised, Detail: map[string]any{"reason": "a private reason"}},
		{ID: 2, Kind: factoryd.AlertSupervisorTrip, Source: "no_write", TaskID: "t-e", RaisedAt: raised, Verdict: factoryd.VerdictReal, ResolvedBy: "supervisor",
			Detail: map[string]any{"action": "redirect", "diagnosis": "stop rereading the spec; write the handler"}},
		{ID: 1, Kind: factoryd.AlertGateBound, Source: "WaitChecks", TaskID: "t-c", RaisedAt: raised, BoundS: 30, Verdict: factoryd.VerdictReal, ResolvedBy: "supervisor",
			Detail: map[string]any{"action": "escalate", "diagnosis": "no runner serves the label", "recommendation": "register a runner"}},
	}
	page := r.do(httptest.NewRequest(http.MethodGet, "/alerts", nil), ck).Body.String()
	for _, want := range []string{`<th scope="col">Supervisor</th>`,
		"<td><strong>redirect</strong>: stop rereading the spec; write the handler</td>",
		"<td><strong>escalate</strong>: no runner serves the label Recommendation: register a runner</td>"} {
		if !strings.Contains(page, want) {
			t.Errorf("/alerts lacks %q", want)
		}
	}
	if strings.Contains(page, "a private reason") {
		t.Error("/alerts shows an escalation's detail")
	}
}

// The timeline names the supervisor's action and diagnosis on its steps:
// the session supervisor's decision and the gate supervisor's.
func TestTimelineShowsTheSupervisorsAction(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-1", Kind: "agent", Status: "in-review", Repository: "sf/code", Objective: "work"})
	r.objects["sessions/t-1/timeline.jsonl"] = []byte(`{"step":"supervisor","time":"2026-09-28T10:10:00Z","detector":"no_write","decision":"continue","reason":"it is compiling"}
{"step":"gate_supervisor","time":"2026-09-28T10:40:00Z","activity":"WaitChecks","action":"wait","diagnosis":"two checks run","recommendation":""}
{"step":"checks","time":"2026-09-28T10:45:00Z","state":"success"}
`)
	body := r.do(httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil), ck).Body.String()
	for _, want := range []string{`<th scope="col">Supervisor</th>`,
		"<td>supervisor</td><td><strong>continue</strong>: it is compiling</td>",
		"<td>gate_supervisor</td><td><strong>wait</strong>: two checks run</td>",
		"<td>checks</td><td></td>"} {
		if !strings.Contains(body, want) {
			t.Errorf("timeline lacks %q", want)
		}
	}
}

// A roll task (ruling 178) waits in the human lane until its card opens,
// then stands in its agent-lane column like any task.
func TestBoardShowsARollInTheHumanLaneUntilItsCard(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("", []factoryd.Task{
		{ID: "t-roll", Kind: "agent", Source: factoryd.SourceRoll, Status: "running", Objective: "roll c1c1c1c: factoryd, gateway", Repository: "sf/code"},
		{ID: "t-m", Kind: "agent", Source: factoryd.SourceRoll, Status: "in-review", Objective: "roll c2c2c2c: worker", Repository: "sf/code", PRNumber: 9},
	}...)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	column := func(id string) string {
		start := strings.Index(body, `id="`+id+`"`)
		if start < 0 {
			t.Fatalf("column %s missing", id)
		}
		return body[start : start+strings.Index(body[start:], "</section>")]
	}
	human, running, review := column("col-human"), column("col-2"), column("col-5")
	if !strings.Contains(human, "roll c1c1c1c") || strings.Contains(running, "roll c1c1c1c") {
		t.Errorf("the roll without a card is not in the human lane only:\nhuman %s\nrunning %s", human, running)
	}
	if strings.Contains(human, "roll c2c2c2c") || !strings.Contains(review, "roll c2c2c2c") {
		t.Errorf("the roll with its card is not in review:\nhuman %s\nreview %s", human, review)
	}
}

// interventions reads factory.human.intervention by kind, process and
// lane.
func (r *rig) interventions(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.metrics.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "factory.human.intervention" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				kind, _ := dp.Attributes.Value(attribute.Key("kind"))
				process, _ := dp.Attributes.Value(attribute.Key("process"))
				lane, _ := dp.Attributes.Value(attribute.Key("lane"))
				out[kind.AsString()+" "+process.AsString()+" "+lane.AsString()] = dp.Value
			}
		}
	}
	return out
}

// The console counts only the verdict click, once, when it took effect:
// its merge-card decisions act as the human in Forgejo and are counted by
// the intake, like the same decisions made in Forgejo.
func TestConsoleCountsInterventions(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.fg.failReview = true
	r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}), ck)
	r.fg.failReview = false
	if rec := r.do(post("/approvals/4/approve", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve = %d", rec.Code)
	}
	if rec := r.do(post("/approvals/4/deny", url.Values{"csrf": {s.csrf}, "action": {"request_changes"}, "note": {"split it"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("request changes = %d", rec.Code)
	}
	r.approvals.rows[1].Status = "pending"
	if rec := r.do(post("/approvals/4/deny", url.Values{"csrf": {s.csrf}, "action": {"reject"}, "note": {"not wanted"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("reject = %d", rec.Code)
	}
	if rec := r.do(post("/approvals/3/approve", url.Values{"csrf": {s.csrf}, "scope": {"task"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("resource approve = %d", rec.Code)
	}
	r.alerts.rows = []factoryd.Alert{{ID: 3, Kind: factoryd.AlertEscalation, Source: "agent", TaskID: "t-e"}}
	for range 2 {
		r.do(post("/alerts/3/verdict", url.Values{"csrf": {s.csrf}, "verdict": {"real"}}), ck)
	}
	want := map[string]int64{"alert_verdict_click none none": 1}
	if got := r.interventions(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("interventions = %v, want %v", got, want)
	}
}
