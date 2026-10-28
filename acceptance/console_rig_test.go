package acceptance

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/console"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	factorygateway "github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The acceptance harness: a console over stand-in stores — Forgejo, the
// task state, approvals, events, the chat's store and the operator
// gateway — so a test exercises a chat action end-to-end through the
// console's HTTP surface, as a member does. It is the console test rig's
// shape kept to what one tell of a card reads and writes.

const accProjectName = "ai-software-factory-" + "code"

// AcceptanceRig is the harness: the console's handler plus the stores it
// stands on, each recording what the actions did.
type AcceptanceRig struct {
	t *testing.T
	c *console.Console

	mu        sync.Mutex
	tasks     []factoryd.BoardTask
	approvals []factoryd.ApprovalRequest
	events    []factoryd.Event
	msgs      []accMessage
	atts      []factoryd.ChatAttachment
	proposals []*accProposal
	sessions  map[string]factoryd.ConsoleSession
	reviews   []map[string]string
	fgCalls   []string // "METHOD path Bearer token" per Forgejo API call
	commentN  int64
	tokenN    int
	challenge string // the PKCE challenge /login sent Forgejo
	replies   [][]string
	steered   []string // "task user: text" per steer call
	bus       *factoryd.EventBus
	chat      *policy.ChatConfig
	// chat is the chat block the policy names when the rig seats the
	// documents: the project's chat.doc_repo.
	intake map[string]bool // the repositories' closed intake switches
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
// request #9 and #13 t-e waiting on an answer, and no event recorded.
// steer wires the steer stub: every text handed to a task's session is
// recorded as "task user: text". docs, when not nil, seats the document
// repository fake (roadmap 277): sf/docs on a Forgejo as its contents API
// answers, seeded with docs, every landed commit recorded; the policy
// names it the projects' chat.doc_repo, so the console's document surface
// reads and writes it. It answers the rig, the fake (nil without docs)
// and the project's chat path.
func NewAcceptanceRig(t *testing.T, steer bool, docs map[string]string) (*AcceptanceRig, *AccDocs, string) {
	t.Helper()
	a := &AcceptanceRig{t: t, sessions: map[string]factoryd.ConsoleSession{}, bus: factoryd.NewEventBus()}
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

	cfg := console.Config{
		PublicURL: "https://console.example", ForgejoURL: fg.URL, ForgejoPublicURL: "https://git.example",
		ClientID: "cid", ClientSecret: "csecret", SessionKey: []byte("0123456789abcdef0123456789abcdef"),
		Sessions: a, Tasks: a, Timelines: a, Policies: a, Costs: a, Approvals: a, Events: a, Chat: a, Projects: a,
		Bus: a.bus,
		Writers: func(context.Context) (string, map[string][]string, error) {
			return "sf/code", map[string][]string{"sf/code": {"sebastian"}}, nil
		},
		MayMove: func(_ context.Context, repository, user, _ string) (bool, error) {
			return repository == "sf/code" && user == "sebastian", nil
		},
		Operator: &factoryd.OperatorModel{GatewayURL: gw.URL, Token: "gw-token"},
	}
	if steer {
		cfg.Steer = func(_ context.Context, taskID, user, text string) error {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.steered = append(a.steered, taskID+" "+user+": "+text)
			return nil
		}
	}
	var d *AccDocs
	if docs != nil {
		d = &AccDocs{t: t, files: docs,
			versions: []accVersion{{files: maps.Clone(docs), author: "Sebastian", at: "2026-10-15T14:00:00Z"}}}
		next, err := url.Parse(fg.URL)
		if err != nil {
			t.Fatal(err)
		}
		proxy := httputil.NewSingleHostReverseProxy(next)
		const base = "/api/v1/repos/sf/docs/contents"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			d.serve(w, req, proxy, base)
		}))
		t.Cleanup(srv.Close)
		cfg.ForgejoURL, cfg.DocsToken = srv.URL, "docs-token"
		a.chat = &policy.ChatConfig{DocRepo: "sf/docs"}
	}
	c, err := console.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.c = c
	return a, d, "/projects/" + accProjectName + "/chat"
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
	state := cookieValue(rec, "__Host-console-login")
	if state == "" {
		t.Fatal("/login set no login cookie")
	}
	cb := a.request(http.MethodGet, "/auth/callback?code=the-code&state="+url.QueryEscape(loc.Query().Get("state")), nil,
		&http.Cookie{Name: "__Host-console-login", Value: state, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}) // #nosec G124 -- test harness cookie stub
	ck := cookieValue(cb, "__Host-console")
	if cb.Code != http.StatusSeeOther || ck == "" {
		t.Fatalf("the callback = %d, session cookie set: %v", cb.Code, ck != "")
	}
	session := &http.Cookie{Name: "__Host-console", Value: ck, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode} // #nosec G124 -- test harness cookie stub
	page := a.request(http.MethodGet, "/", nil, session)
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindSubmatch(page.Body.Bytes())
	if page.Code != http.StatusOK || m == nil {
		t.Fatalf("the board after the login = %d, a form token on it: %v", page.Code, m != nil)
	}
	return session, string(m[1])
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

// Steered answers the steer calls recorded so far.
func (a *AcceptanceRig) Steered() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.steered)
}

// AccDocs is the document repository sf/docs on a Forgejo as its contents
// API answers: master's files by path with a blob sha per version, the
// head commit of a path with its author and time, a change of the files
// as one commit answered 201. The versions keep the head as of each
// commit — the base a stale save's three-way merge reads (the 2026-10-15
// plan, item 5) — and every landed commit is recorded with the
// Authorization that made it.
type AccDocs struct {
	t        *testing.T
	mu       sync.Mutex
	files    map[string]string
	version  int
	versions []accVersion
	commits  []AccDocCommit
}

// accVersion is the fake's head as of one version: the files and the
// head commit's author and its time.
type accVersion struct {
	files  map[string]string
	author string
	at     string
}

// AccDocCommit is one commit the fake landed: its message, the changed
// path and the Authorization header that made it.
type AccDocCommit struct {
	Message, Path, Bearer string
}

// accDocSHA is version v's commit sha.
func accDocSHA(v int) string { return "c0ffee" + strings.Repeat("0", v) }

// Files answers a copy of master's files by path.
func (d *AccDocs) Files() map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return maps.Clone(d.files)
}

// Commits answers the commits the fake landed, oldest first.
func (d *AccDocs) Commits() []AccDocCommit {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.commits)
}

// serve answers one call of sf/docs's API: a file's contents — master's,
// or the version a commit names — a path's head commit, and a change of
// the files as one commit; every other call goes through proxy to the
// rig's Forgejo.
func (d *AccDocs) serve(w http.ResponseWriter, req *http.Request, proxy *httputil.ReverseProxy, base string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	head := d.versions[min(d.version, len(d.versions)-1)]
	switch {
	case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, base+"/"):
		at := d.files
		if ref := req.URL.Query().Get("ref"); ref != "master" {
			at = nil
			for i := len(d.versions) - 1; i >= 0; i-- {
				if ref == accDocSHA(i) {
					at = d.versions[i].files
					break
				}
			}
			if at == nil {
				http.Error(w, `{"message":"object does not exist"}`, http.StatusNotFound)
				return
			}
		}
		text, ok := at[strings.TrimPrefix(req.URL.Path, base+"/")]
		if !ok {
			http.Error(w, `{"message":"object does not exist"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(text)), "sha": factoryd.ContentHash(text)[:12]})
	case req.Method == http.MethodGet && req.URL.Path == "/api/v1/repos/sf/docs/commits" && req.URL.Query().Get("path") != "" &&
		req.URL.Query().Get("sha") == "master":
		// the path's head commit: the fake's version as its sha
		_ = json.NewEncoder(w).Encode([]map[string]any{{"sha": accDocSHA(d.version),
			"commit": map[string]any{"author": map[string]any{"name": head.author},
				"committer": map[string]any{"date": head.at}}}})
	case req.Method == http.MethodPost && req.URL.Path == base:
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body) // #nosec G104 -- test harness stub request
		change := body["files"].([]any)[0].(map[string]any)
		path, _ := change["path"].(string)
		current, present := d.files[path]
		switch {
		case change["operation"] == "update" && (!present || change["sha"] != factoryd.ContentHash(current)[:12]):
			http.Error(w, `{"message":"sha does not match"}`, http.StatusConflict)
			return
		case change["operation"] == "create" && present:
			http.Error(w, `{"message":"file exists"}`, http.StatusUnprocessableEntity)
			return
		}
		raw, _ := base64.StdEncoding.DecodeString(change["content"].(string))
		d.files[path] = string(raw)
		d.version++
		d.versions = append(d.versions, accVersion{files: maps.Clone(d.files), author: "sebastian", at: "2026-10-15T14:00:00Z"})
		d.commits = append(d.commits, AccDocCommit{Message: body["message"].(string), Path: path, Bearer: req.Header.Get("Authorization")})
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{ // #nosec G104 -- test harness stub response
			"sha": accDocSHA(d.version), "created": "2026-10-15T15:00:00Z"}})
	default:
		proxy.ServeHTTP(w, req)
	}
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
	// a naming call (roadmap 304) answers nothing and consumes no reply:
	// today's head stands
	if r.Header.Get(factorygateway.HeaderFactoryPurpose) == policy.PurposeTitle {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", operatorChunk(map[string]any{"content": ""}))
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

func (a *AcceptanceRig) TaskStatuses(_ context.Context, ids []string) (map[string]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]string{}
	for _, id := range ids {
		for _, t := range a.tasks {
			if t.ID == id {
				out[id] = t.Status
			}
		}
	}
	return out, nil
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

func (a *AcceptanceRig) BoardTasks(_ context.Context, repository string) ([]factoryd.BoardTask, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cards := slices.DeleteFunc(slices.Clone(a.tasks), func(t factoryd.BoardTask) bool {
		return t.Repository != repository || t.Kind == factoryd.KindChat || factoryd.Final(t.Status)
	})
	factoryd.SortBoardTasks(cards)
	return cards, nil
}

// ProjectImageParks: the rig's cards wait for no image.
func (a *AcceptanceRig) ProjectImageParks(context.Context, string) ([]factoryd.ImagePark, error) {
	return nil, nil
}

func (a *AcceptanceRig) FinishedTasks(_ context.Context, q factoryd.FinishedQuery) ([]factoryd.BoardTask, int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	page, rest := factoryd.FinishedPage(a.tasks, q)
	return page, rest, nil
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

func (a *AcceptanceRig) PlanHoldouts(context.Context, string) ([]factoryd.Holdout, error) {
	return nil, nil
}

func (a *AcceptanceRig) PlanCards(context.Context, string) ([]factoryd.Task, error) { return nil, nil }
func (a *AcceptanceRig) CriteriaVerdicts(context.Context, string) ([]factoryd.CriterionVerdict, error) {
	return nil, nil
}

func (a *AcceptanceRig) Dependencies(context.Context, string) ([]factoryd.Dependency, error) {
	return nil, nil
}

func (a *AcceptanceRig) Dependents(context.Context, string) ([]factoryd.Dependency, error) {
	return nil, nil
}

func (a *AcceptanceRig) RollCarries(context.Context, string) ([]factoryd.Task, error) {
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

func (a *AcceptanceRig) FundCard(ctx context.Context, id string, eur float64, _ *factoryd.FundBounds, _ factoryd.Event) (float64, float64, error) {
	return a.RaiseTaskBudget(ctx, id, eur)
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
	a.mu.Unlock()
	a.bus.Publish(e)
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

func (a *AcceptanceRig) TaskEventsFor(ctx context.Context, ids []string, limit int) (map[string][]factoryd.Event, error) {
	out := map[string][]factoryd.Event{}
	for _, id := range ids {
		evs, err := a.TaskEvents(ctx, id, limit)
		if err != nil {
			return nil, err
		}
		out[id] = evs
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

func (a *AcceptanceRig) EnsureChatTask(_ context.Context, name, repository string, budget float64, at time.Time) (factoryd.Task, error) {
	id := factoryd.ChatTaskID(name, at)
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.tasks {
		if a.tasks[i].ID == id {
			a.tasks[i].BudgetEUR = max(a.tasks[i].BudgetEUR, budget)
			return a.tasks[i].Task, nil
		}
	}
	t := factoryd.Task{ID: id, Kind: factoryd.KindChat, Status: "running", BudgetEUR: budget, Repository: repository}
	a.tasks = append(a.tasks, factoryd.BoardTask{Task: t})
	return t, nil
}

func (a *AcceptanceRig) AddChatMessage(_ context.Context, repository, login string, m factoryd.ChatMessage) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m.ID = a.nextID()
	m.At = time.Now()
	m.Conversation = 1
	a.msgs = append(a.msgs, accMessage{ChatMessage: m, Repository: repository, Login: login})
	return m.ID, nil
}

// nextID is one past the highest message id; a.mu held.
func (a *AcceptanceRig) nextID() int64 {
	var n int64
	for _, m := range a.msgs {
		n = max(n, m.ID)
	}
	return n + 1
}

func (a *AcceptanceRig) AddChatAttachment(_ context.Context, _ string, _ string, at factoryd.ChatAttachment) (factoryd.ChatAttachment, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	at.ID, at.At, at.Conversation = a.nextID(), time.Now(), 1
	a.atts = append(a.atts, at)
	return at, nil
}

func (a *AcceptanceRig) ChatAttachments(_ context.Context, _ string, _ string, _ int64) ([]factoryd.ChatAttachment, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.atts), nil
}

func (a *AcceptanceRig) ChatAttachmentByID(_ context.Context, _ string, id int64) (factoryd.ChatAttachment, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, at := range a.atts {
		if at.ID == id {
			return at, true, nil
		}
	}
	return factoryd.ChatAttachment{}, false, nil
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

func (a *AcceptanceRig) ChatAsks(_ context.Context, repository, login string) ([]factoryd.ChatMessage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []factoryd.ChatMessage
	for _, m := range a.msgs {
		if m.Repository == repository && m.Login == login &&
			(m.Kind == factoryd.MessageEscalation || m.Kind == factoryd.MessagePoke) && m.TaskID != "" {
			out = append(out, m.ChatMessage)
		}
	}
	return out, nil
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

func (a *AcceptanceRig) ProjectConversations(context.Context, string) ([]factoryd.ChatConversation, error) {
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

func (a *AcceptanceRig) TaskSpendSince(context.Context, string, time.Time) (float64, error) {
	return 0, nil
}

// BudgetStoppedThreads are the members of repository whose one thread
// ends on the chat budget's stop, as the store's query reads them.
func (a *AcceptanceRig) BudgetStoppedThreads(_ context.Context, repository string) ([]factoryd.ChatConversation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	newest := map[string]factoryd.ChatMessage{}
	for _, m := range a.msgs {
		if m.Repository == repository {
			newest[m.Login] = m.ChatMessage
		}
	}
	var out []factoryd.ChatConversation
	for _, login := range slices.Sorted(maps.Keys(newest)) {
		if m := newest[login]; m.Kind == factoryd.MessageBudget && m.BirthKind == factoryd.EventChatBudgetSpent {
			out = append(out, factoryd.ChatConversation{Login: login, Number: 1})
		}
	}
	return out, nil
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
	a.mu.Lock()
	chat := a.chat
	a.mu.Unlock()
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
				Document: policy.Document{Version: 1, Chat: chat,
					Budget:          policy.Budget{TaskDefaultEUR: 2, TaskMaxEUR: 5, DayEUR: 20, MonthEUR: 50},
					Classes:         map[string]policy.Class{"coder": {Models: []string{"factory/small"}}},
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

func (a *AcceptanceRig) Delete(context.Context, string) error { return nil }

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

func (a *AcceptanceRig) ModelTitle(context.Context, string, string, int64, string, string) (bool, error) {
	return true, nil
}

func (a *AcceptanceRig) MarkSeen(context.Context, string, string, int64) error { return nil }

func (a *AcceptanceRig) MarkCleared(context.Context, string, string, int64) error { return nil }

func (a *AcceptanceRig) EnsureInbox(context.Context, string, string) (int64, error) { return 0, nil }

func (a *AcceptanceRig) EnsureCardThread(context.Context, string, string, string) (int64, error) {
	return 0, nil
}

func (a *AcceptanceRig) CardThreadFor(context.Context, string, string) (string, int64, bool, error) {
	return "", 0, false, nil
}

func (a *AcceptanceRig) CardThreads(context.Context, string, []string) (map[string]factoryd.CardThread, error) {
	return map[string]factoryd.CardThread{}, nil
}

func (a *AcceptanceRig) AddChatWatch(context.Context, string, string, int64, string) error {
	return nil
}

func (a *AcceptanceRig) RemoveChatWatch(context.Context, string, string, string) (bool, error) {
	return true, nil
}

func (a *AcceptanceRig) AddChatTimer(context.Context, string, string, int64, time.Time, string, string) error {
	return nil
}

func (a *AcceptanceRig) ArchiveConversation(context.Context, string, string, int64, bool) (bool, error) {
	return true, nil
}

func (a *AcceptanceRig) SetChatIntake(_ context.Context, repository string, closed bool, _ string, _ time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.intake == nil {
		a.intake = map[string]bool{}
	}
	a.intake[repository] = closed
	return nil
}

func (a *AcceptanceRig) ChatIntakeClosed(_ context.Context, repository string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.intake[repository], nil
}

func (a *AcceptanceRig) DeleteConversation(context.Context, string, string, int64) (int64, []int64, error) {
	return 0, nil, nil
}

func (a *AcceptanceRig) AbandonChatMessage(context.Context, string, string, int64, int64) ([]int64, error) {
	return nil, nil
}

func (a *AcceptanceRig) StartQueued(_ context.Context, _, _ string, id int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	i := slices.IndexFunc(a.msgs, func(m accMessage) bool { return m.ID == id && m.Queued })
	if i < 0 {
		return nil
	}
	m := a.msgs[i]
	a.msgs = slices.Delete(a.msgs, i, i+1)
	m.ID, m.Queued, m.At = a.nextID(), false, time.Now()
	a.msgs = append(a.msgs, m)
	return nil
}

func (a *AcceptanceRig) SaveSummary(context.Context, string, string, int64, factoryd.ThreadSummary) error {
	return nil
}

func (a *AcceptanceRig) ThreadTokens(context.Context, string, time.Time) (int, error) { return 0, nil }

func (a *AcceptanceRig) ThreadTotals(context.Context, string) (int, float64, error) { return 0, 0, nil }
