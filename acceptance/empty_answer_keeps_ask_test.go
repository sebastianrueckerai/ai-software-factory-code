package acceptance

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/console"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	factorygateway "github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The park-answer rig: everything the tests of a parked card's answers
// drive, over one real PostgreSQL store. It stands up the whole factory
// around a card the agent parked at the human with a live agent-time cap
// ask: the signed webhook intake that carries a member's issue comment,
// the console logged in as that member with the runner's own answer
// paths, and the heal pass with the live runner. The rig's database is
// its own per test; the policy in force names demo-app a factory project
// whose registration ran (its projects row holds the tokens), so the
// trust gate reads the project's assignees and collaborators off the
// fake Forgejo and the member writes the code.

const (
	parkProject    = "demo-app"
	parkRepo       = "demo-app/demo-app"
	parkMember     = "sebastian"
	parkBot        = "factory-agent"
	parkHookSecret = "park-hook-secret"
	parkTask       = "t-park"
	parkIssue      = int64(21)
	parkAskComment = int64(4101)
	// parkTimeCapReason is the ask sentence the agent-time cap's own
	// source writes, the question the card's face shows.
	parkTimeCapReason = "the agent time is used up: every finding sent to the agent so far goes to a human"
)

// parkForgejo is the Forgejo the rig stands on: the member's OAuth login,
// the project's assignees and collaborators (the trust gate's read), the
// card's issue open with no comments, every comment the factory posts on
// it recorded, and no branch of the card on the bot's fork. The writers of
// the repository are the member.
type parkForgejo struct {
	mu       sync.Mutex
	login    string // who answers the OAuth flow's /api/v1/user
	posted   []int64
	next     int64
	server   *httptest.Server
	unwanted []string
}

// parkForgejoServe answers one call of the fake Forgejo's API.
func (f *parkForgejo) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && p == "/login/oauth/access_token":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"access-1","refresh_token":"refresh-1","token_type":"bearer","expires_in":3600}`)
	case r.Method == http.MethodGet && p == "/api/v1/user":
		f.mu.Lock()
		login := f.login
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"login":%q}`, login)
	case r.Method == http.MethodGet && p == "/api/v1/repos/"+parkRepo+"/assignees":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"login":%q}]`, parkMember)
	case r.Method == http.MethodGet && p == "/api/v1/repos/"+parkRepo+"/collaborators":
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprintf(w, `[{"login":%q}]`, parkMember)
			return
		}
		fmt.Fprint(w, `[]`)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/api/v1/repos/"+parkRepo+"/issues/") && strings.HasSuffix(p, "/timeline"):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[]`)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/api/v1/repos/"+parkRepo+"/issues/") && strings.HasSuffix(p, "/comments"):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[]`)
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/api/v1/repos/"+parkRepo+"/issues/") && strings.HasSuffix(p, "/comments"):
		f.mu.Lock()
		f.next++
		id := 5100 + f.next
		f.posted = append(f.posted, id)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":%d}`, id)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/api/v1/repos/"+parkRepo+"/issues/"):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"state":"open"}`)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/api/v1/repos/"+parkBot+"/"+parkProject+"/branches/"):
		http.NotFound(w, r)
	default:
		f.mu.Lock()
		f.unwanted = append(f.unwanted, r.Method+" "+p)
		f.mu.Unlock()
		http.NotFound(w, r)
	}
}

// Posted answers the ids of the comments the factory posted on the card's issue.
func (f *parkForgejo) Posted() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.posted...)
}

// Unwanted answers the calls the rig's Forgejo never meant to answer.
func (f *parkForgejo) Unwanted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.unwanted...)
}

// parkRigs counts the rigs this process seeded, one database each.
var parkRigs = struct {
	mu sync.Mutex
	n  int
}{}

// parkRig is the factory around one parked card: the store, the runner,
// the signed webhook intake and the console over the same store, with
// the fake Forgejo and the object store behind them all; dsn is the
// store's database, for a gateway's store beside it.
type parkRig struct {
	mu        sync.Mutex
	dsn       string
	replies   [][]string // each the stream chunks of the next operator call
	store     *factoryd.PGStore
	runner    *factoryd.Runner
	forgejo   *factoryd.Forgejo
	fj        *parkForgejo
	intake    *factoryd.Intake
	console   *console.Console
	workflows *acceptanceWorkflows
	objects   *stubObjects
	version   policy.Version
	delivery  int
}

// newParkRig stands the rig up: a fresh database seeded with the policy
// in force and the demo-app project registered (its row holds the
// factory tokens the trust gate and the merge read), the runner over the
// store, the intake signing with the hook's secret, the console logged
// in through the fake Forgejo's OAuth flow.
func newParkRig(t *testing.T) *parkRig {
	t.Helper()
	ctx := context.Background()
	parkRigs.mu.Lock()
	n := parkRigs.n
	parkRigs.n++
	parkRigs.mu.Unlock()
	dsn := schematest.DSN(t, fmt.Sprintf("acceptance_park_%d", n))
	store, err := factoryd.OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// the policy in force: demo-app a factory project whose registration
	// ran, so its projects row holds the tokens and the trust gate reads
	// its assignees and collaborators, not the org teams of a seed
	v := acceptancePolicyVersion()
	// the member approves the project: an extend is an approver's answer on
	// every door, the chat's among them
	v.Tree.Projects[parkProject] = policy.ProjectConfig{Repository: parkRepo, Process: "maintenance",
		Objects: policy.ObjectsFactory, Agents: policy.AgentsCoder, Approvers: []string{parkMember}}
	if _, _, err := store.RecordPolicy(ctx, v.Commit, v.Tree); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureProjectRows(ctx, []string{parkProject}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProjectTokens(ctx, parkProject, factoryd.ProjectTokens{GitToken: "git-token", MergeToken: "merge-token"}); err != nil {
		t.Fatal(err)
	}

	fj := &parkForgejo{login: parkMember, next: 0}
	fj.server = httptest.NewServer(http.HandlerFunc(fj.serve))
	t.Cleanup(fj.server.Close)
	quiet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected gateway call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}))
	t.Cleanup(quiet.Close)
	litellm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/key/list"):
			fmt.Fprint(w, `{"keys":[],"total_pages":1}`)
		case r.Method == http.MethodPost && r.URL.Path == "/key/block":
		default:
			t.Errorf("unexpected litellm call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(litellm.Close)

	var rig *parkRig
	// the operator behind the console's chat: its streamed completions
	// come from the rig's scripted replies
	op := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer gw-token" {
			http.Error(w, "not the operator's call", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if r.Header.Get(factorygateway.HeaderFactoryPurpose) == policy.PurposeTitle {
			// a naming call answers nothing and consumes no reply
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", operatorChunk(map[string]any{"content": ""}))
			return
		}
		rig.mu.Lock()
		defer rig.mu.Unlock()
		if len(rig.replies) == 0 {
			http.Error(w, "unscripted", http.StatusInternalServerError)
			return
		}
		for _, line := range append(rig.replies[0], "[DONE]") {
			fmt.Fprintf(w, "data: %s\n\n", line)
		}
		rig.replies = rig.replies[1:]
	}))
	t.Cleanup(op.Close)

	forgejo := &factoryd.Forgejo{BaseURL: fj.server.URL, BotUser: parkBot, BotPassword: "bot-pw"}
	objects := &stubObjects{data: map[string][]byte{}}
	workflows := &acceptanceWorkflows{}
	cfg := factoryd.RunnerConfig{
		AgentNamespacePrefix: "factory-agents-", WorkerImage: "reg/worker@sha256:abc", SandboxBaseImage: "reg/sandbox-base@sha256:bcd", RegistryHost: "store.test",
		PullSecret: "forgejo-registry", FactoryNamespace: "factory",
		GatewayURL: "http://factory-gateway.factory.svc.cluster.local:8081", GitBaseURL: "http://forgejo-http.forgejo.svc.cluster.local:3000",
		Models:       factoryd.ModelLadder{Start: "factory/small", Stronger: "factory/big"},
		AnswerTokens: 8192, HeadroomTokens: 16384, ChecksPoll: time.Millisecond,
		EgressCanary: "example.com:443", EgressDirectIP: "192.0.2.7:443",
		Supervisor: factoryd.SupervisorConfig{RepetitionEvents: 24},
		Policy:     func() policy.Version { return v },
		PolicyVersion: func(_ context.Context, id int64) (policy.Version, bool, error) {
			return v, id == v.ID, nil
		},
		Leases: &acceptanceLeases{}, Gates: &factoryd.GateWatch{},
		LastFailure: func(context.Context, string, string) (string, error) { return "", nil },
		Workflows:   workflows,
	}
	completion := &factoryd.Completion{Store: store, Forgejo: forgejo, LiteLLM: &factoryd.LiteLLMAdmin{BaseURL: litellm.URL, MasterKey: "mk"}}
	runner, err := factoryd.NewRunner(cfg, store, &acceptanceKube{}, forgejo, completion, objects,
		&factoryd.SupervisorModel{GatewayURL: quiet.URL, Token: "gw-token", Budget: factoryd.TokenBudget{AnswerTokens: 1000, HeadroomTokens: 8000}})
	if err != nil {
		t.Fatal(err)
	}

	// the signed webhook intake over the same store, its events recorded
	// by the runner's own timeline so the answers land in the cases
	in := &factoryd.Intake{Store: store, Forgejo: forgejo, Workflows: workflows, BotUser: parkBot,
		Secret: parkHookSecret, Timeline: runner.Timeline(), Policy: func() policy.Tree { return v.Tree }}

	// the console over the same store: the runner's own digests, case
	// reads and Act, the chat and its operator, the intake's trust gate
	ccfg := console.Config{
		PublicURL:        "http://console.test",
		ForgejoURL:       fj.server.URL,
		ForgejoPublicURL: fj.server.URL,
		ClientID:         "console",
		ClientSecret:     "console-secret",
		SessionKey:       []byte("0123456789abcdef0123456789abcdef"),
		Sessions:         store,
		Tasks:            store,
		Timelines:        objects,
		Policies:         store,
		Costs:            store,
		Receipts:         store,
		Dependencies:     store,
		Approvals:        store,
		Projects:         store,
		Events:           store,
		Figures:          store,
		Bus:              factoryd.NewEventBus(),
		Writes:           in.Writes,
		Chat:             store,
		ChatTokens:       store,
		Notes:            store,
		Operator:         &factoryd.OperatorModel{GatewayURL: op.URL, Token: "gw-token"},
		ChatToken:        func(p factoryd.Project) string { return forgejo.Access(p).Chat },
		Writers: func(ctx context.Context) (string, map[string][]string, error) {
			ps, err := store.Projects(ctx)
			if err != nil {
				return "", nil, err
			}
			writers := map[string][]string{}
			for _, p := range ps {
				if !p.Coder() || p.MergeToken == "" {
					continue // the seeded rows hold no agent and no read token the trust gate could read
				}
				ws, err := forgejo.Writers(ctx, p)
				if err != nil {
					return "", nil, err
				}
				writers[p.Repository] = ws
			}
			return "", writers, nil
		},
	}
	cardReads(&ccfg, nil, store)
	// the store's own case reads and the runner's own answers and digests
	ccfg.OpenCasesOf = store.OpenCasesOf
	ccfg.CaseDesk = store.CaseDesk
	ccfg.CaseSteps = store.CaseSteps
	ccfg.CaseByID = store.CaseByID
	ccfg.NewestAskCase = store.NewestAskCase
	ccfg.PoolDrawn = store.PoolDrawn
	ccfg.Act = runner.Act
	ccfg.CardDigest = runner.CardDigest
	ccfg.CardStep = runner.CardStep
	ccfg.BoardDigest = runner.BoardDigest
	board, err := console.New(ccfg)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		for _, call := range fj.Unwanted() {
			t.Errorf("the rig's Forgejo answered no %s", call)
		}
	})
	rig = &parkRig{dsn: dsn, store: store, runner: runner, forgejo: forgejo, fj: fj, intake: in, console: board,
		workflows: workflows, objects: objects, version: v}
	return rig
}

// seedWaitingCard parks the running-kind card at the human: the agent's
// card on issue n of the project's repository, its work stopped for an
// answer.
func (r *parkRig) seedWaitingCard(t *testing.T, id string, issue int64) {
	t.Helper()
	if err := r.store.CreateTask(context.Background(), factoryd.Task{ID: id, Kind: "agent", Track: policy.TrackDirect,
		Source: "webhook", Repository: parkRepo, IssueNumber: issue, Status: "waiting-human", BudgetEUR: 5}); err != nil {
		t.Fatal(err)
	}
}

// seedAgentTimeCapAsk opens the card's live agent-time cap ask, recorded
// as the cap's own source writes it, and answers the case it opened.
func (r *parkRig) seedAgentTimeCapAsk(t *testing.T, taskID string, issue, comment int64) factoryd.Case {
	t.Helper()
	r.seedWaitingCard(t, taskID, issue)
	ctx := context.Background()
	if _, err := r.store.Record(ctx, factoryd.Event{Repository: parkRepo, TaskID: taskID, IssueNumber: issue,
		Kind: factoryd.EventQuestionAsked, Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd,
		Detail: map[string]any{"question": parkTimeCapReason, "ask": parkTimeCapReason,
			"comment": comment, "source": "cap", "reason": parkTimeCapReason}}); err != nil {
		t.Fatalf("record %s's agent-time cap ask: %v", taskID, err)
	}
	open, err := r.store.OpenCasesOf(ctx, taskID)
	if err != nil || len(open) != 1 || open[0].Kind != factoryd.CaseAgentTimeCap {
		t.Fatalf("%s's cases = %+v, %v; want one live agent-time cap ask", taskID, open, err)
	}
	return open[0]
}

// postCommentWebhook delivers the webhook's copy of a new comment on the
// card's issue, signed with the hook's secret, and answers what the
// intake said.
func (r *parkRig) postCommentWebhook(t *testing.T, issue, comment int64, author, body string) *httptest.ResponseRecorder {
	t.Helper()
	r.delivery++
	payload, err := json.Marshal(map[string]any{
		"action":     "created",
		"issue":      map[string]any{"number": issue},
		"comment":    map[string]any{"id": comment, "user": map[string]any{"login": author}, "body": body},
		"repository": map[string]any{"full_name": parkRepo},
		"is_pull":    false,
	})
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(parkHookSecret))
	mac.Write(payload)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/forgejo", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forgejo-Event-Type", "issue_comment")
	req.Header.Set("X-Forgejo-Signature", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Forgejo-Delivery", fmt.Sprintf("park-delivery-%d", r.delivery))
	rec := httptest.NewRecorder()
	r.intake.ServeHTTP(rec, req)
	return rec
}

// login walks the member through the console's OAuth flow against the
// fake Forgejo and answers their session cookie and the board's form
// token.
func (r *parkRig) login(t *testing.T) (*http.Cookie, string) {
	t.Helper()
	session := r.session(t)
	rec := r.get(t, "/", session)
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindSubmatch(rec.Body.Bytes())
	if rec.Code != http.StatusOK || m == nil {
		t.Fatalf("the board after the login = %d, a form token on it: %v", rec.Code, m != nil)
	}
	return session, string(m[1])
}

// session walks the member through the console's OAuth flow against the
// fake Forgejo and answers their session cookie.
func (r *parkRig) session(t *testing.T) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	r.console.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("/login = %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	var state *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "__Host-console-login" {
			state = c
		}
	}
	if state == nil || state.Value == "" {
		t.Fatal("/login set no login cookie")
	}
	req = httptest.NewRequest(http.MethodGet, "/auth/callback?code=the-code&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	req.AddCookie(state)
	rec = httptest.NewRecorder()
	r.console.ServeHTTP(rec, req)
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "__Host-console" {
			session = c
		}
	}
	if rec.Code != http.StatusSeeOther || session == nil {
		t.Fatalf("the OAuth callback = %d, session cookie set: %v", rec.Code, session != nil)
	}
	return session
}

// Reply scripts the next operator calls: each reply is one call's stream
// chunks, made with Say and Call.
func (r *parkRig) Reply(replies ...[]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replies = append(r.replies, replies...)
}

// get runs one GET on path as the member of ck and answers the recorder.
func (r *parkRig) get(t *testing.T, path string, ck *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	r.console.ServeHTTP(rec, req)
	return rec
}

// healPass runs one heal pass of the live runner over the rig's factory.
func (r *parkRig) healPass(t *testing.T) {
	t.Helper()
	factoryd.HealPass(context.Background(), r.store, r.workflows, r.intake, nil,
		func(context.Context, string) (bool, error) { return false, nil }, r.runner)
}

// askStillLive asserts the card's ask case is still its live ask at the
// human, pressable: unresolved, the card's newest live ask, the card
// itself still waiting on the human.
func (r *parkRig) askStillLive(t *testing.T, taskID string, ask factoryd.Case) {
	t.Helper()
	ctx := context.Background()
	got, err := r.store.CaseByID(ctx, ask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != factoryd.CaseAtHuman || got.Resolution != nil || got.ResolvedAt != nil {
		t.Fatalf("%s's %s case = %+v; want it still at the human, unresolved", taskID, ask.Kind, got)
	}
	newest, err := r.store.NewestAskCase(ctx, taskID)
	if err != nil || newest.ID != ask.ID || newest.Kind != ask.Kind {
		t.Fatalf("%s's newest live ask = %+v, %v; want case %d, the ask that stood", taskID, newest, err, ask.ID)
	}
	task, ok, err := r.store.Task(ctx, taskID)
	if err != nil || !ok || task.Status != "waiting-human" {
		t.Fatalf("%s = %+v (found %v), %v; want the card still waiting on the human", taskID, task, ok, err)
	}
}

// TestTheWebhooksEmptyAnswerKeepsTheCapAskLive: end to end over the real
// store, the webhook's copy of a comment on the parked card's issue — a
// row naming no action, signed with the hook's secret — leaves the
// card's live agent-time cap ask live and pressable: the case stays at
// the human unresolved, it stays the card's newest live ask through a
// heal pass, and the console's case fragment still offers it to the
// member.
func TestTheWebhooksEmptyAnswerKeepsTheCapAskLive(t *testing.T) {
	rig := newParkRig(t)
	ask := rig.seedAgentTimeCapAsk(t, parkTask, parkIssue, parkAskComment)

	// the member's comment on the card's issue, a sentence of words that
	// names no action: the webhook delivers only the comment
	rec := rig.postCommentWebhook(t, parkIssue, 4102, parkMember, "Extend 1800 s of agent time on the card")
	if rec.Code != http.StatusOK {
		t.Fatalf("the webhook's delivery = %d: %s", rec.Code, rec.Body.String())
	}
	events, err := rig.store.IntakeEvents(context.Background(), parkTask)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "issue_comment" || events[0].Author != parkMember || events[0].CommentID != 4102 || !events[0].Trusted {
		t.Fatalf("the card's intake events = %+v; want the member's trusted issue comment 4102", events)
	}

	// the empty answer leaves the ask live and pressable
	rig.askStillLive(t, parkTask, ask)

	// a heal pass with the live runner leaves the parked card and its
	// live ask standing: the pass sees the newest ask and parks nothing
	rig.healPass(t)
	rig.askStillLive(t, parkTask, ask)

	// the console still offers the ask to the member: the card's case
	// fragment carries the ask's sentence and its case's number
	ck, _ := rig.login(t)
	page := rig.get(t, "/projects/"+parkProject+"/cards/"+parkTask+"/cases/"+strconv.FormatInt(ask.ID, 10), ck)
	if page.Code != http.StatusOK {
		t.Fatalf("the case fragment = %d: %s", page.Code, page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "The card used its agent time") {
		t.Fatalf("the case fragment lacks the ask's sentence:\n%s", page.Body.String())
	}
}

// postChat posts the member's message to the project's chat and answers
// the turn's event stream.
func (r *parkRig) postChat(t *testing.T, ck *http.Cookie, form url.Values, path ...string) (int, string) {
	t.Helper()
	what := "/projects/" + parkProject + "/chat"
	if len(path) > 0 {
		what += path[0]
	}
	srv := httptest.NewServer(r.console)
	defer srv.Close()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, vs := range form {
		for _, v := range vs {
			if err := w.WriteField(k, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+what, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(ck)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// parkEvents answers the stream's events as "event data" lines, in order.
func parkEvents(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, msg := range strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n") {
		line := strings.Split(msg, "\n")
		if len(line) < 2 || !strings.HasPrefix(line[0], "event: ") {
			t.Errorf("a stream block of no event: %q", msg)
			continue
		}
		out = append(out, strings.TrimPrefix(line[0], "event: ")+" "+strings.TrimPrefix(line[1], "data: "))
	}
	return out
}

// TestTheChatsGrantResolvesTheCapAsk: end to end over the same real store
// the empty answer refused, the project chat's grant answer tool — its
// proposal confirmed by the member — lands on doGrantSession, whose
// grantOf buys agent time at the card's live agent-time cap ask: the case
// resolves with action extend and its draw of agent time, the answer row
// names the comment the console posted on the card's issue, and no live
// ask stands between the card and its work any more.
func TestTheChatsGrantResolvesTheCapAsk(t *testing.T) {
	rig := newParkRig(t)
	ask := rig.seedAgentTimeCapAsk(t, parkTask, parkIssue, parkAskComment)

	rig.Reply([]string{Call(1, "grant_session", `{"task_id":"`+parkTask+`","text":"Extend 1800 s of agent time on the card"}`)},
		[]string{Say("Granted the extend.")})
	ck, csrf := rig.login(t)
	code, body := rig.postChat(t, ck, url.Values{"message": {"Grant what the card asks for."}, "csrf": {csrf}})
	if code != http.StatusOK {
		t.Fatalf("POST the chat = %d:\n%s", code, body)
	}
	stream := parkEvents(t, body)
	if len(stream) != 2 || !strings.HasPrefix(stream[0], "proposal ") || !strings.HasPrefix(stream[1], "done ") {
		t.Fatalf("the turn's events = %v, want the grant's proposal and done", stream)
	}
	var offer struct {
		ID   string `json:"id"`
		Tool string `json:"tool"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(stream[0], "proposal ")), &offer); err != nil {
		t.Fatal(err)
	}
	if offer.Tool != "grant_session" || offer.ID == "" {
		t.Fatalf("the offer = %+v, want the grant_session tool's", offer)
	}

	code, body = rig.postChat(t, ck, url.Values{"csrf": {csrf}}, "/proposals/"+offer.ID)
	if code != http.StatusOK {
		t.Fatalf("Do it = %d:\n%s", code, body)
	}
	did := parkEvents(t, body)
	if len(did) != 3 || !strings.HasPrefix(did[0], "tool ") || !strings.HasPrefix(did[1], "applied ") || !strings.HasPrefix(did[2], "done ") {
		t.Fatalf("the Do it's events = %v, want the tool block, its applied event and done", did)
	}

	// the case resolved with the structured extend: action extend and its
	// draw of agent time, no longer at the human
	ctx := context.Background()
	got, err := rig.store.CaseByID(ctx, ask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != factoryd.CaseResolved || got.Resolution == nil || got.Resolution.Action != factoryd.ActExtend {
		t.Fatalf("the agent-time cap = %s, %+v; want it resolved by an extend", got.Status, got.Resolution)
	}
	secs, _ := got.Resolution.Args["agent_time_s"].(float64)
	if secs <= 0 {
		t.Fatalf("the extend's args = %v; want agent_time_s > 0", got.Resolution.Args)
	}
	if newest, err := rig.store.NewestAskCase(ctx, parkTask); err != nil || newest.ID != 0 {
		t.Fatalf("the card's newest live ask = %+v, %v; want none, the card goes on past its stop", newest, err)
	}

	// the answer row names the comment the console posted on the card's issue
	posted := rig.fj.Posted()
	if len(posted) != 1 {
		t.Fatalf("the console posted %+v on the card's issue; want the grant's one comment", posted)
	}
	rows, err := rig.store.TaskEvents(ctx, parkTask, 10)
	if err != nil {
		t.Fatal(err)
	}
	var answered *factoryd.Event
	for i := range rows {
		if rows[i].Kind == factoryd.EventQuestionAnswered {
			answered = &rows[i]
		}
	}
	if answered == nil || answered.Detail["action"] != factoryd.ActExtend || answered.Detail["comment"] != float64(posted[0]) {
		t.Fatalf("the card's rows hold %+v; want a question_answered row naming action extend and comment %d", rows, posted[0])
	}
}
