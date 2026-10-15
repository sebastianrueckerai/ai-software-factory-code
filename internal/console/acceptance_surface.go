package console

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The acceptance harness: a console over stand-in stores — Forgejo, the
// task state, approvals, events, the chat's store and the operator
// gateway — so a test outside this package exercises a chat action
// end-to-end through the HTTP surface. It is the console test rig's shape
// kept to what one tell of a card reads and writes.

const accProjectName = "ai-software-factory-" + "code"

// AcceptanceRig is the harness: the console's handler plus the stores it
// stands on, each recording what the actions did.
type AcceptanceRig struct {
	t *testing.T
	c *Console

	mu        sync.Mutex
	tasks     []factoryd.BoardTask
	approvals []factoryd.ApprovalRequest
	events    []factoryd.Event
	msgs      []accMessage
	proposals []*accProposal
	sessions  map[string]factoryd.ConsoleSession
	reviews   []map[string]string
	fgCalls   []string // "METHOD path Bearer token" per Forgejo API call
	commentN  int64
	tokenN    int
	challenge string // the PKCE challenge /login sent Forgejo
	replies   [][]string
	steered   []string // "task user: text" per SetSteer call
}

// accMessage is one conversation row with the member it belongs to.
type accMessage struct {
	factoryd.ChatMessage
	Repository, Login string
}

// accProposal is one stored proposal or undo, used once at most.
type accProposal struct {
	factoryd.ChatProposal
	used time.Time
}

// NewAcceptanceRig builds the rig: logged out, with the code project's
// cards #5 t-m in the backlog, #11 t-r running its session with pull
// request #9 and #13 t-e waiting on an answer, no steer wired and no
// event recorded. It answers the rig and the project's chat path.
func NewAcceptanceRig(t *testing.T) (*AcceptanceRig, string) {
	t.Helper()
	a := &AcceptanceRig{t: t, sessions: map[string]factoryd.ConsoleSession{}}
	a.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-m", Kind: "agent", Status: "backlog", Lane: policy.LaneBacklog, Process: "maintenance",
			Repository: "sf/code", IssueNumber: 5, Size: "small", BudgetEUR: 3, SpentEUR: 0.4,
			Objective: "Fix the cache. It drops entries.\n\nAcceptance:\n- The cache keeps entries across a restart\n"}},
		{Task: factoryd.Task{ID: "t-r", Kind: "agent", Status: "running", Gate: factoryd.GateSession, Lane: policy.LaneReady,
			Process: "maintenance", Repository: "sf/code", IssueNumber: 11, PRNumber: 9, Objective: "Speed up the board."}},
		{Task: factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13,
			Objective: "Use a replica?"}},
	}
	fg := httptest.NewServer(http.HandlerFunc(a.serveForgejo))
	t.Cleanup(fg.Close)
	gw := httptest.NewServer(http.HandlerFunc(a.serveGateway))
	t.Cleanup(gw.Close)

	c, err := New(Config{
		PublicURL: "https://console.example", ForgejoURL: fg.URL, ForgejoPublicURL: "https://git.example",
		ClientID: "cid", ClientSecret: "csecret", SessionKey: []byte("0123456789abcdef0123456789abcdef"),
		Sessions: a, Tasks: a, Timelines: a, Policies: a, Costs: a, Approvals: a, Events: a, Chat: a, Projects: a,
		Bus: factoryd.NewEventBus(),
		Writers: func(context.Context) (string, map[string][]string, error) {
			return "sf/code", map[string][]string{"sf/code": {"sebastian"}}, nil
		},
		MayMove: func(_ context.Context, repository, user, _ string) (bool, error) {
			return repository == "sf/code" && user == "sebastian", nil
		},
		Operator: &factoryd.OperatorModel{GatewayURL: gw.URL, Token: "gw-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	a.c = c
	return a, "/projects/" + accProjectName + "/chat"
}

// Login logs the member in as Forgejo answers the OAuth flow and answers
// their session cookie and form token.
func (a *AcceptanceRig) Login(t *testing.T) (*http.Cookie, string) {
	t.Helper()
	rec := a.request(http.MethodGet, "/login", nil, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("/login = %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.challenge = loc.Query().Get("code_challenge")
	a.mu.Unlock()
	state := cookieValue(rec, loginCookie)
	if state == "" {
		t.Fatal("/login set no login cookie")
	}
	cb := a.request(http.MethodGet, "/auth/callback?code=the-code&state="+url.QueryEscape(loc.Query().Get("state")), nil,
		&http.Cookie{Name: loginCookie, Value: state, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}) // #nosec G124 -- test harness cookie stub
	ck := cookieValue(cb, sessionCookie)
	if cb.Code != http.StatusSeeOther || ck == "" {
		t.Fatalf("the callback = %d, session cookie set: %v", cb.Code, ck != "")
	}
	s, err := a.c.sessionByCookie(t.Context(), ck)
	if err != nil || s == nil {
		t.Fatalf("the fresh login's row does not read back: found %v, %v", s != nil, err)
	}
	return &http.Cookie{Name: sessionCookie, Value: ck, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}, s.csrf // #nosec G124 -- test harness cookie stub
}

// Handler is the console's HTTP handler, for tests that need a real server
// (streaming SSE through httptest.ResponseRecorder drops writes because it
// does not implement SetWriteDeadline).
func (a *AcceptanceRig) Handler() http.Handler { return a.c }

// Post runs one POST on path as the member of ck and answers the
// recorder: the status and the streamed body.
func (a *AcceptanceRig) Post(path string, ck *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return a.requestRaw(req, ck)
}

// SetSteer installs the steer stub: every text handed to a task's session
// is recorded as "task user: text".
func (a *AcceptanceRig) SetSteer() {
	a.c.cfg.Steer = func(_ context.Context, taskID, user, text string) error {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.steered = append(a.steered, taskID+" "+user+": "+text)
		return nil
	}
}

// Steered answers the steer calls recorded so far.
func (a *AcceptanceRig) Steered() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.steered)
}

// TellCard puts card taskID of issue of the code project in review at its
// approval gate — gate approval, pull request pr — with pending merge
// request id at head sha, so a tell of it takes the deny branch.
func (a *AcceptanceRig) TellCard(taskID string, issue, pr, id int64, objective, headSHA string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tasks = append(a.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: taskID, Kind: "agent", Status: "in-review",
		Gate: factoryd.GateApproval, Lane: policy.LaneReady, Process: "maintenance", Repository: "sf/code",
		IssueNumber: issue, PRNumber: pr, Objective: objective}})
	a.approvals = append(a.approvals, factoryd.ApprovalRequest{ID: id, Kind: factoryd.ApprovalMerge, TaskID: taskID,
		Project: "sf/code", PRNumber: pr, Status: "pending",
		Spec: json.RawMessage(`{"head_sha":"` + headSHA + `","risk_class":"low"}`)})
}

// Approval answers approval request id's row.
func (a *AcceptanceRig) Approval(id int64) factoryd.ApprovalRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, q := range a.approvals {
		if q.ID == id {
			return q
		}
	}
	return factoryd.ApprovalRequest{}
}

// Reviews answers every Forgejo pull-request review posted, each with its
// event, body and commit and the call line Forgejo saw.
func (a *AcceptanceRig) Reviews() []map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]map[string]string, len(a.reviews))
	for i, rv := range a.reviews {
		m := map[string]string{}
		for k, v := range rv {
			m[k] = v
		}
		m["call"] = a.fgCalls[i+1]
		out[i] = m
	}
	return out
}

// Events answers the event spine's rows, oldest first.
func (a *AcceptanceRig) Events() []factoryd.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.events)
}

// ToolCalls answers the conversation's tool-call rows of repository for
// login, oldest first.
func (a *AcceptanceRig) ToolCalls(repository, login string) []factoryd.ChatToolCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []factoryd.ChatToolCall
	for _, m := range a.msgs {
		if m.Repository == repository && m.Login == login {
			out = append(out, m.ToolCalls...)
		}
	}
	return out
}

// Reply scripts the next model calls: each reply is one call's stream
// chunks, made with Say and Call.
func (a *AcceptanceRig) Reply(replies ...[]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.replies = append(a.replies, replies...)
}

// Say is a chunk of the answer's text.
func Say(s string) string { return operatorChunk(map[string]any{"content": s}) }

// Call is a chunk of tool calls, each name and arguments JSON at indexes
// 0, 1, …, with ids call_<n>, n counting from first.
func Call(first int, nameArgs ...string) string {
	var calls []any
	for i := 0; i < len(nameArgs); i += 2 {
		calls = append(calls, map[string]any{"index": i / 2, "id": fmt.Sprintf("call_%d", first+i/2), "type": "function",
			"function": map[string]any{"name": nameArgs[i], "arguments": nameArgs[i+1]}})
	}
	return operatorChunk(map[string]any{"tool_calls": calls})
}

// operatorChunk is one streamed answer chunk carrying delta.
func operatorChunk(delta map[string]any) string {
	b, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// ChatTaskID is project name's chat task for the month of at.
func ChatTaskID(name string, at time.Time) string { return factoryd.ChatTaskID(name, at) }

// requestRaw runs req on the console with ck added.
func (a *AcceptanceRig) requestRaw(req *http.Request, ck *http.Cookie) *httptest.ResponseRecorder {
	if ck != nil {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	a.c.ServeHTTP(rec, req)
	return rec
}

func (a *AcceptanceRig) request(method, path string, form url.Values, ck *http.Cookie) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return a.requestRaw(req, ck)
}

// cookieValue is the response's Set-Cookie value of name, "" when none.
func cookieValue(rec *httptest.ResponseRecorder, name string) string {
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

// serveGateway answers the operator's streamed completion with the next
// scripted reply.
func (a *AcceptanceRig) serveGateway(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer gw-token" {
		http.Error(w, "not the operator's call", http.StatusInternalServerError)
		return
	}
	if len(a.replies) == 0 {
		a.t.Errorf("an unscripted model call")
		http.Error(w, "unscripted", http.StatusInternalServerError)
		return
	}
	reply := a.replies[0]
	a.replies = a.replies[1:]
	w.Header().Set("Content-Type", "text/event-stream")
	for _, line := range append(reply, "[DONE]") {
		fmt.Fprintf(w, "data: %s\n\n", line)
	}
}

// serveForgejo is the fake Forgejo: the OAuth endpoints, the reviews and
// comments a tell writes, and the reads the board needs.
func (a *AcceptanceRig) serveForgejo(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.URL.Path == "/login/oauth/access_token" {
		if err := r.ParseForm(); err != nil {
			a.t.Errorf("token form: %v", err)
		}
		if r.PostForm.Get("grant_type") == "authorization_code" {
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != a.challenge {
				http.Error(w, `{"error":"unauthorized_client","error_description":"failed PKCE code challenge"}`, http.StatusBadRequest)
				return
			}
		}
		a.tokenN++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("access-%d", a.tokenN), // #nosec G104 -- test harness stub response
			"refresh_token": fmt.Sprintf("refresh-%d", a.tokenN), "token_type": "bearer", "expires_in": 3600})
		return
	}
	a.fgCalls = append(a.fgCalls, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user":
		_ = json.NewEncoder(w).Encode(map[string]any{"login": "sebastian"}) // #nosec G104 -- test harness stub response
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews"):
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body) // #nosec G104 -- test harness stub request
		a.reviews = append(a.reviews, body)
		_, _ = w.Write([]byte(`{"id":12}`)) // #nosec G104 -- test harness stub response
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body) // #nosec G104 -- test harness stub request
		a.commentN++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": a.commentN}) // #nosec G104 -- test harness stub response
	default:
		http.NotFound(w, r)
	}
}

// Sessions: the logins in memory, sealed as the store keeps them.

func (a *AcceptanceRig) SaveConsoleSession(_ context.Context, sess factoryd.ConsoleSession) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[sess.IDHash] = sess
	return nil
}

func (a *AcceptanceRig) ConsoleSessionByHash(_ context.Context, hash string) (factoryd.ConsoleSession, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	row, ok := a.sessions[hash]
	if !ok || time.Now().After(row.ExpiresAt) {
		delete(a.sessions, hash)
		return factoryd.ConsoleSession{}, false, nil
	}
	return row, true, nil
}

func (a *AcceptanceRig) DeleteConsoleSession(_ context.Context, hash string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, hash)
	return nil
}

func (a *AcceptanceRig) TouchConsoleSession(_ context.Context, hash string, seenAt time.Time) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	row, ok := a.sessions[hash]
	if !ok || !row.LastSeen.Equal(seenAt) {
		return false, nil
	}
	row.LastSeen = time.Now()
	a.sessions[hash] = row
	return true, nil
}

func (a *AcceptanceRig) UpdateConsoleSessionTokens(_ context.Context, hash, access, refresh string, expiry time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	row, ok := a.sessions[hash]
	if !ok {
		return nil
	}
	row.Access, row.Refresh, row.TokenExpiry = access, refresh, expiry
	a.sessions[hash] = row
	return nil
}

func (a *AcceptanceRig) PurgeConsoleSessions(_ context.Context, at time.Time) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var n int64
	for hash, row := range a.sessions {
		if row.ExpiresAt.Before(at) {
			delete(a.sessions, hash)
			n++
		}
	}
	return n, nil
}

// Tasks: the board's cards in memory.

func (a *AcceptanceRig) Task(_ context.Context, id string) (factoryd.Task, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range a.tasks {
		if t.ID == id {
			return t.Task, true, nil
		}
	}
	return factoryd.Task{}, false, nil
}

func (a *AcceptanceRig) TaskByIssue(_ context.Context, repository string, issue int64) (factoryd.Task, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range a.tasks {
		if t.Repository == repository && t.IssueNumber == issue {
			return t.Task, true, nil
		}
	}
	return factoryd.Task{}, false, nil
}

func (a *AcceptanceRig) BoardTasks(_ context.Context, _ time.Time, limit int) ([]factoryd.BoardTask, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cards := slices.DeleteFunc(slices.Clone(a.tasks), func(t factoryd.BoardTask) bool { return t.Kind == factoryd.KindChat })
	factoryd.SortBoardTasks(cards)
	return cards[:min(limit, len(cards))], nil
}

func (a *AcceptanceRig) BoardTask(_ context.Context, id string) (factoryd.BoardTask, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	i := slices.IndexFunc(a.tasks, func(t factoryd.BoardTask) bool { return t.ID == id })
	if i < 0 {
		return factoryd.BoardTask{}, false, nil
	}
	return a.tasks[i], true, nil
}

func (a *AcceptanceRig) PlanRevision(context.Context, string, int) (factoryd.PlanRevision, bool, error) {
	return factoryd.PlanRevision{}, false, nil
}

func (a *AcceptanceRig) PlanQuestions(context.Context, string, int) ([]factoryd.PlanQuestion, error) {
	return nil, nil
}

func (a *AcceptanceRig) PlanCards(context.Context, string) ([]factoryd.Task, error) { return nil, nil }

func (a *AcceptanceRig) Dependencies(context.Context, string) ([]factoryd.Dependency, error) {
	return nil, nil
}

func (a *AcceptanceRig) Dependents(context.Context, string) ([]factoryd.Dependency, error) {
	return nil, nil
}

func (a *AcceptanceRig) SetHumanSummary(context.Context, string, string) error { return nil }

func (a *AcceptanceRig) SetTaskAccent(context.Context, string, string) error { return nil }

func (a *AcceptanceRig) SetObjective(context.Context, string, string) error { return nil }

func (a *AcceptanceRig) RaiseTaskBudget(_ context.Context, id string, eur float64) (float64, float64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.tasks {
		if a.tasks[i].ID == id {
			from := a.tasks[i].BudgetEUR
			a.tasks[i].BudgetEUR += eur
			return from, a.tasks[i].BudgetEUR, nil
		}
	}
	return 0, 0, factoryd.ErrNotFound
}

func (a *AcceptanceRig) AddTaskDeps(context.Context, string, []string) error { return nil }

func (a *AcceptanceRig) RemoveTaskDeps(context.Context, string, []string) error { return nil }

// Approvals: the approval_requests table in memory.

func (a *AcceptanceRig) ApprovalRequests(_ context.Context, status string) ([]factoryd.ApprovalRequest, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []factoryd.ApprovalRequest
	for _, q := range a.approvals {
		if q.Status == status {
			out = append(out, q)
		}
	}
	return out, nil
}

func (a *AcceptanceRig) DecideApproval(_ context.Context, id int64, kind, status, by, note string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, q := range a.approvals {
		if q.ID == id && q.Kind == kind && q.Status == "pending" {
			a.approvals[i].Status, a.approvals[i].DecidedBy, a.approvals[i].DecisionNote = status, by, note
			now := time.Now()
			a.approvals[i].DecidedAt = &now
			return nil
		}
	}
	return factoryd.ErrNotFound
}

// Events: the event spine in memory.

func (a *AcceptanceRig) Record(_ context.Context, e factoryd.Event) (int64, error) {
	if e.Repository == "" || e.Kind == "" {
		return 0, fmt.Errorf("event: repository and kind are required")
	}
	a.mu.Lock()
	e.ID = int64(len(a.events) + 1)
	if e.At.IsZero() {
		e.At = time.Now()
	}
	a.events = append(a.events, e)
	bus := a.c.cfg.Bus
	a.mu.Unlock()
	bus.Publish(e)
	return e.ID, nil
}

func (a *AcceptanceRig) ProjectEvents(_ context.Context, repository string, after int64, limit int) ([]factoryd.Event, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []factoryd.Event
	for i := len(a.events) - 1; i >= 0; i-- {
		e := a.events[i]
		if e.Repository == repository && e.ID > after {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (a *AcceptanceRig) FiledPR(_ context.Context, name string) (int64, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.events) - 1; i >= 0; i-- {
		if e := a.events[i]; e.Repository == name+"/"+name && e.Kind == factoryd.EventProjectFiled {
			pr, _ := e.Detail["pr"].(int64)
			return pr, pr > 0, nil
		}
	}
	return 0, false, nil
}

func (a *AcceptanceRig) TaskEvents(_ context.Context, id string, limit int) ([]factoryd.Event, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []factoryd.Event
	for i := len(a.events) - 1; i >= 0 && len(out) < limit; i-- {
		if a.events[i].TaskID == id {
			out = append(out, a.events[i])
		}
	}
	return out, nil
}

func (a *AcceptanceRig) NewestTaskEvents(_ context.Context, tasks []string) (map[string]factoryd.Event, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]factoryd.Event{}
	for i := len(a.events) - 1; i >= 0; i-- {
		e := a.events[i]
		if slices.Contains(tasks, e.TaskID) {
			if _, ok := out[e.TaskID]; !ok {
				out[e.TaskID] = e
			}
		}
	}
	return out, nil
}

// Chat: the conversations, proposals and spend in memory.

func (a *AcceptanceRig) EnsureChatTask(ctx context.Context, name, repository string, budget float64, at time.Time) (factoryd.Task, error) {
	id := factoryd.ChatTaskID(name, at)
	if t, ok, _ := a.Task(ctx, id); ok {
		return t, nil
	}
	t := factoryd.Task{ID: id, Kind: factoryd.KindChat, Status: "running", BudgetEUR: budget, Repository: repository}
	a.mu.Lock()
	a.tasks = append(a.tasks, factoryd.BoardTask{Task: t})
	a.mu.Unlock()
	return t, nil
}

func (a *AcceptanceRig) AddChatMessage(_ context.Context, repository, login string, m factoryd.ChatMessage) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	m.ID = a.nextID()
	m.At = time.Now()
	m.Conversation = 1
	a.msgs = append(a.msgs, accMessage{ChatMessage: m, Repository: repository, Login: login})
	return nil
}

// nextID is one past the highest message id; a.mu held.
func (a *AcceptanceRig) nextID() int64 {
	var n int64
	for _, m := range a.msgs {
		n = max(n, m.ID)
	}
	return n + 1
}

func (a *AcceptanceRig) ChatMessages(_ context.Context, repository, login string, _ int64, limit int) ([]factoryd.ChatMessage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var all []factoryd.ChatMessage
	for _, m := range a.msgs {
		if m.Repository == repository && m.Login == login {
			all = append(all, m.ChatMessage)
		}
	}
	return all[max(0, len(all)-limit):], nil
}

func (a *AcceptanceRig) CurrentConversation(context.Context, string, string) (int64, error) {
	return 1, nil
}

func (a *AcceptanceRig) NewConversation(context.Context, string, string) (int64, error) {
	return 1, nil
}

func (a *AcceptanceRig) Conversations(context.Context, string, string) ([]factoryd.ChatConversation, error) {
	return nil, nil
}

func (a *AcceptanceRig) ProposalStates(_ context.Context, _, _ string, tokens []string, now time.Time) ([]factoryd.ProposalState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]factoryd.ProposalState, len(tokens))
	for i, token := range tokens {
		out[i] = factoryd.ProposalState{Token: token, State: "expired"}
		for _, p := range a.proposals {
			if p.Token != token || now.After(p.ExpiresAt) {
				continue
			}
			state := "pending"
			if !p.used.IsZero() {
				state = "used"
			}
			out[i] = factoryd.ProposalState{Token: token, State: state}
		}
	}
	return out, nil
}

func (a *AcceptanceRig) ProjectMonthSpend(context.Context, string, time.Time) (factoryd.ProjectSpend, error) {
	return factoryd.ProjectSpend{}, nil
}

func (a *AcceptanceRig) PutProposal(_ context.Context, p factoryd.ChatProposal) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.proposals = append(a.proposals, &accProposal{ChatProposal: p})
	return nil
}

func (a *AcceptanceRig) TakeProposal(_ context.Context, kind, token, repository, login string, now time.Time) (factoryd.ChatProposal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.proposals {
		if p.Kind != kind || p.Token != token || p.Repository != repository || !p.used.IsZero() || now.After(p.ExpiresAt) {
			continue
		}
		if p.Login != login {
			return factoryd.ChatProposal{}, factoryd.ErrOthersProposal
		}
		p.used = now
		return p.ChatProposal, nil
	}
	return factoryd.ChatProposal{}, factoryd.ErrNoProposal
}

func (a *AcceptanceRig) PurgeProposals(_ context.Context, before time.Time) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(a.proposals)
	a.proposals = slices.DeleteFunc(a.proposals, func(p *accProposal) bool {
		return p.ExpiresAt.Before(before) && (p.used.IsZero() || p.Kind == "undo")
	})
	return int64(n - len(a.proposals)), nil
}

// Projects: the code project, a coder of maintenance, and no others.

func (a *AcceptanceRig) Projects(ctx context.Context) ([]factoryd.Project, error) {
	v, _, _ := a.CurrentPolicy(ctx)
	return []factoryd.Project{{Name: accProjectName, Repository: "sf/code", MergeRule: "human", Objects: "seed",
		Agents: "coder", File: true, Process: "maintenance", Lanes: v.Tree.Processes["maintenance"].Lanes,
		Namespaces: []string{"factory"},
		Tokens:     policy.SeedTokens{Read: policy.SecretCIRegistrar, Actions: policy.SecretCIRegistrar, Merge: policy.SecretMerge}}}, nil
}

// Policies and Costs: the shipped default policy with the maintenance
// process, no ledger rows.

func (a *AcceptanceRig) CurrentPolicy(context.Context) (policy.Version, bool, error) {
	stages := []policy.Stage{
		{Name: "screening", OnFindings: "human", When: "outsider"},
		{Name: "checks", OnFindings: "agent", Enabled: true},
		{Name: "merge", OnFindings: "human", Enabled: true},
	}
	processes := map[string]policy.Process{
		"maintenance": {Lanes: []policy.Lane{{Name: "backlog", WriteUp: "analyzer"}, {Name: "ready", Class: "coder", MovedBy: "writers"}},
			Stages: stages, Merge: policy.Merge{Queue: "none"}},
	}
	return policy.Version{ID: 4, Commit: "c9d98bb0aa11", LoadedAt: time.Now(), ActivatedAt: time.Now(),
			Tree: policy.Tree{
				Document: policy.Document{Version: 1,
					Budget:          policy.Budget{TaskDefaultEUR: 2, TaskMaxEUR: 5, DayEUR: 20, MonthEUR: 50},
					Classes:         map[string]policy.Class{"coder": {Models: []string{"factory/coding-cheap"}}},
					Limits:          policy.Limits{Sessions: 3, AgentTime: 20 * time.Minute, SupervisorCalls: 3, SupervisorTimeout: 6 * time.Minute, StrongerAfter: 1},
					Approvers:       []string{"sebastian"},
					EnvelopeDefault: policy.Envelope{MonthlyEUR: 10, ServerTypes: []string{"cx23"}, MaxCount: 2, MaxLeaseHours: 4, VolumeMaxGB: 20}},
				Projects:  map[string]policy.ProjectConfig{accProjectName: {Repository: "sf/code", MergeRule: "human", Objects: "seed", Agents: "coder", Process: "maintenance"}},
				Processes: processes}},
		true, nil
}

func (a *AcceptanceRig) Costs(context.Context, time.Time, time.Time) (policy.Costs, error) {
	return policy.Costs{}, nil
}

func (a *AcceptanceRig) DaySpend(context.Context, time.Time) (float64, error) { return 0, nil }

func (a *AcceptanceRig) ModelCalls(context.Context, string) ([]policy.ModelCall, error) {
	return nil, nil
}

func (a *AcceptanceRig) CallTotals(context.Context, []string) (map[string]policy.CallTotals, error) {
	return nil, nil
}

// Timelines: no timeline, so a card's face reads no checks.

func (a *AcceptanceRig) Get(context.Context, string) ([]byte, error) {
	return nil, errors.New("acceptance: no timeline")
}

func (a *AcceptanceRig) Put(context.Context, string, []byte) error { return nil }

func (a *AcceptanceRig) CountAdmitted(_ context.Context, repository string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, bt := range a.tasks {
		t := bt.Task
		if t.Repository != repository || t.Kind != "agent" || t.Lane == "" {
			continue
		}
		if slices.Contains([]string{"waiting-deps", "running", "in-review", "closing"}, t.Status) ||
			(t.Status == "queued" && t.AdmittedAt != nil) {
			n++
		}
	}
	return n, nil
}

func (a *AcceptanceRig) CountBacklog(_ context.Context, repository string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, bt := range a.tasks {
		t := bt.Task
		if t.Repository != repository || t.Kind != "agent" || t.Lane == "" || t.AdmittedAt != nil || factoryd.Final(t.Status) {
			continue
		}
		n++
	}
	return n, nil
}

func (a *AcceptanceRig) Conversation(_ context.Context, _, _ string, n int64) (factoryd.ChatConversation, bool, error) {
	return factoryd.ChatConversation{Number: n, State: factoryd.ThreadActive, Current: true}, true, nil
}

func (a *AcceptanceRig) OpenConversation(context.Context, string, string, int64) error { return nil }

func (a *AcceptanceRig) RenameConversation(context.Context, string, string, int64, string) (bool, error) {
	return true, nil
}

func (a *AcceptanceRig) MarkSeen(context.Context, string, string, int64) error { return nil }

func (a *AcceptanceRig) EnsureInbox(context.Context, string, string) (int64, error) { return 0, nil }

func (a *AcceptanceRig) ArchiveConversation(context.Context, string, string, int64, bool) (bool, error) {
	return true, nil
}

func (a *AcceptanceRig) DeleteConversation(context.Context, string, string, int64) (int64, error) {
	return 0, nil
}

func (a *AcceptanceRig) QueueChatMessage(_ context.Context, repository, login string, _ int64, text string) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := factoryd.ChatMessage{ID: a.nextID(), Role: "user", Content: text, Kind: factoryd.MessageQueued, At: time.Now(), Conversation: 1}
	a.msgs = append(a.msgs, accMessage{ChatMessage: m, Repository: repository, Login: login})
	return m.ID, nil
}

func (a *AcceptanceRig) StartQueued(_ context.Context, _, _ string, id int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	i := slices.IndexFunc(a.msgs, func(m accMessage) bool { return m.ID == id && m.Kind == factoryd.MessageQueued })
	if i < 0 {
		return nil
	}
	m := a.msgs[i]
	a.msgs = slices.Delete(a.msgs, i, i+1)
	m.ID, m.Kind, m.At = a.nextID(), "", time.Now()
	a.msgs = append(a.msgs, m)
	return nil
}

func (a *AcceptanceRig) SaveSummary(context.Context, string, string, int64, factoryd.ThreadSummary) error {
	return nil
}

func (a *AcceptanceRig) ThreadTokens(context.Context, string, time.Time) (int, error) { return 0, nil }

func (a *AcceptanceRig) ThreadTotals(context.Context, string) (int, float64, error) { return 0, 0, nil }
