package console

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// members makes r the rig of ruling 254: the factory's own code project
// (sf/code) written by alice and controller, project demo (demo/demo)
// written by bob, the policy's approvers [alice] and demo's file's [bob];
// demo-app's file names none, so it keeps the policy's.

// readerForgejo shows the member repository sf/code, as Forgejo does a
// repository they read.
type readerForgejo struct{ http.RoundTripper }

func (f readerForgejo) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/sf/code" {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"full_name":"sf/code"}`)), Request: r}, nil
	}
	return f.RoundTripper.RoundTrip(r)
}

func members(r *rig) {
	r.writers.users = []string{"alice", "controller"}
	r.writers.others = map[string][]string{"demo/demo": {"bob"}}
	r.repoWriters["sf/code"] = []string{"alice", "controller"}
	r.repoWriters["demo/demo"] = []string{"bob"}
	r.ledger.approvers = []string{"alice"}
	r.ledger.files = map[string]policy.ProjectConfig{"demo": {Repository: "demo/demo", MergeRule: "human", Objects: "factory",
		Agents: "coder", Process: "maintenance", Approvers: []string{"bob"}}}
	r.projects.rows = append(r.projects.rows, factoryd.Project{Name: "demo", Repository: "demo/demo", MergeRule: "human",
		Objects: "factory", Agents: "coder", File: true, GitToken: "t", Process: "maintenance", Lanes: testProcesses()["maintenance"].Lanes})
	r.tasks.set("", factoryd.Task{ID: "t-c", Kind: "agent", Status: "backlog", Lane: policy.LaneBacklog, Process: "maintenance",
		Repository: "sf/code", IssueNumber: 5, Objective: "Fix the cache."})
}

// memberLogin logs user in as Forgejo answers the rig's login.
func memberLogin(t *testing.T, r *rig, user string) (*http.Cookie, *session) {
	t.Helper()
	r.fg.mu.Lock()
	r.fg.user = user
	r.fg.mu.Unlock()
	r.c.mu.Lock()
	r.c.writersT = time.Time{}
	r.c.mu.Unlock()
	return r.loggedIn(t)
}

const badLogin = "a Forgejo user name is required: 1 to 40 letters, digits, dots, dashes and underscores, starting and ending with a letter or digit"

var factoryLinks = []string{`href="/tasks/new"`, `href="/projects"`, `href="/resources"`, `href="/costs"`, `href="/policy"`, `href="/alerts"`}

// A writer of any project logs in and sees the projects they write: another
// project's board is 404 unless Forgejo shows them its repository, then
// read-only; the factory-level pages stay with the writers of the
// factory's own code, and the layout links them only for those. A login
// that writes nothing is refused as before.
func TestMemberSeesTheirProjectsOnly(t *testing.T) {
	r, _, _ := newChatRig(t)
	members(r)
	ck, s := memberLogin(t, r, "bob")
	board := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck)
	if body := board.Body.String(); board.Code != http.StatusOK || !strings.Contains(body, `data-project="demo"`) {
		t.Fatalf("bob's default board = %d, want demo's:\n%s", board.Code, body)
	}
	for _, link := range factoryLinks {
		if strings.Contains(board.Body.String(), link) {
			t.Errorf("bob's board links %s", link)
		}
	}
	if other := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck); other.Code != http.StatusNotFound {
		t.Fatalf("the factory's board for bob, who neither writes nor reads it = %d, want 404:\n%s", other.Code, other.Body)
	}
	plain := r.c.cfg.HTTP
	r.c.cfg.HTTP = &http.Client{Transport: readerForgejo{http.DefaultTransport}}
	other := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck)
	if other.Code != http.StatusOK || !strings.Contains(other.Body.String(), `data-project="ai-software-factory-code"`) || strings.Contains(other.Body.String(), `class="open"`) {
		t.Fatalf("the factory's board for bob, who reads it = %d, want it read-only:\n%s", other.Code, other.Body)
	}
	r.c.cfg.HTTP = plain
	for _, path := range []string{"/cards/t-c/drawer", "/projects/ai-software-factory-code/chat", "/tasks/t-c/timeline",
		"/costs", "/policy", "/alerts", "/resources", "/projects", "/projects/new", "/tasks/new", "/projects/demo/status"} {
		if rec := r.do(httptest.NewRequest(http.MethodGet, path, nil), ck); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s as bob = %d, want 403", path, rec.Code)
		}
	}
	for _, path := range []string{"/tasks", "/alerts/1/verdict", "/resources/1/promote", "/resources/1/release", "/projects", "/projects/demo/merge"} {
		if rec := r.do(post(path, url.Values{"csrf": {s.csrf}, "verdict": {"real"}, "project": {"ai-software-factory-code"}, "title": {"x"},
			"lane": {"ready"}}), ck); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s as bob = %d, want 403", path, rec.Code)
		}
	}
	if len(r.fg.issues) != 0 {
		t.Errorf("bob filed %v", r.fg.issues)
	}

	ak, _ := memberLogin(t, r, "alice")
	page := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ak).Body.String()
	for _, link := range factoryLinks {
		if !strings.Contains(page, link) {
			t.Errorf("alice's board lacks %s", link)
		}
	}
	for _, path := range []string{"/costs", "/policy", "/alerts", "/resources", "/projects", "/projects/new", "/tasks/new"} {
		if rec := r.do(httptest.NewRequest(http.MethodGet, path, nil), ak); rec.Code != http.StatusOK {
			t.Errorf("GET %s as alice = %d, want 200", path, rec.Code)
		}
	}

	r.fg.user = "carol"
	if ck, cb := r.login(t); cb.Code != http.StatusForbidden || ck == nil || ck.MaxAge >= 0 {
		t.Fatalf("carol, who writes nothing: %d, cookie %+v, want 403 and no session", cb.Code, ck)
	}
}

// A project's approvers are its file's, else the policy's: bob approves
// demo's request and not the factory's, alice the factory's and demo-app's
// and not demo's; each refusal names the project. The projects page
// offers alice demo-app's export and not demo's.
func TestApproversArePerProject(t *testing.T) {
	r := newRig(t)
	members(r)
	r.approvals.rows = append(r.approvals.rows,
		factoryd.ApprovalRequest{ID: 6, Kind: "resource", TaskID: "t-d", Project: "demo/demo", Status: "pending"},
		factoryd.ApprovalRequest{ID: 7, Kind: "resource", TaskID: "t-a", Project: "demo-app/demo-app", Status: "pending"})
	as := func(user string) actor { return actor{s: &session{user: user}, via: factoryd.ViaButton} }
	repo := map[int64]string{3: "sf/code", 6: "demo/demo", 7: "demo-app/demo-app"}
	for _, c := range []struct {
		user string
		id   int64
		want string
	}{
		{"bob", 3, "Only the approvers of project ai-software-factory-code may authorize spend."},
		{"alice", 6, "Only the approvers of project demo may authorize spend."},
		{"bob", 7, "Only the approvers of project demo-app may authorize spend."},
		{"bob", 6, ""}, {"alice", 3, ""}, {"alice", 7, ""},
	} {
		err := r.c.admit(context.Background(), accessTable["POST /approvals/{id}/approve"], as(c.user), repo[c.id],
			fmt.Sprintf("approval request %d", c.id))
		var no *refusal
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s approves %d: %v, want it approved", c.user, c.id, err)
		case c.want != "" && (!errors.As(err, &no) || no.status != http.StatusForbidden || no.msg != c.want):
			t.Errorf("%s approves %d: %v, want 403 %q", c.user, c.id, err, c.want)
		}
	}
	ak, _ := memberLogin(t, r, "alice")
	if page := r.do(httptest.NewRequest(http.MethodGet, "/projects", nil), ak).Body.String(); !strings.Contains(page, `action="/projects/demo-app/export"`) ||
		strings.Contains(page, `action="/projects/demo/export"`) {
		t.Errorf("alice's /projects: want demo-app's export form and not demo's:\n%s", page)
	}
}

// invite makes a login a member of the chat's project: an approver's
// proposal, run on their Do it with the projects token's write
// collaboration and one member_invited row. A writer who is no approver
// is refused, a login Forgejo cannot have answers 400, and a seed
// project's collaborators are Forgejo's to keep.
func TestInvite(t *testing.T) {
	r, _, g := newChatRig(t)
	members(r)
	g.project = "demo"
	ck, s := memberLogin(t, r, "bob")
	before := r.events.count()
	g.replies = [][]string{{call(1, "invite", `{"login":"dave"}`)}}
	_, events, tokens := chatPost(t, r, ck, s, "/projects/demo/chat", url.Values{"message": {"invite dave"}})
	equal(t, "stream", events, []string{
		`proposal {"arguments":{"login":"dave"},"description":"Invite a member, login dave","id":"TOKEN","tool":"invite"}`, `done {}`})
	if len(r.invited) != 0 {
		t.Fatalf("a proposal invited %v", r.invited)
	}
	_, events, _ = chatPost(t, r, ck, s, "/projects/demo/chat/proposals/"+tokens[0], nil)
	said := "dave is a member; the console admits them within a minute."
	equal(t, "Do it", events, []string{
		`tool {"arguments":{"login":"dave"},"duration_ms":0,"name":"invite","result":"` + said + `"}`,
		`applied {"result":"` + said + `","tool":"invite"}`, `done {}`})
	equal(t, "invited", r.invited, []string{"demo/demo dave write"})
	chatRows(t, r.events, before, "bob", factoryd.Event{Repository: "demo/demo", Kind: factoryd.EventMemberInvited, Detail: map[string]any{"login": "dave", "autonomy": "manual", "merge_rule": "human", "decider": "member", "chat_task": factoryd.ChatTaskID("demo", time.Now())}})

	r.repoWriters["demo/demo"] = append(r.repoWriters["demo/demo"], "alice")
	r.ledger.chat = &policy.ChatConfig{Confirm: []string{}}
	ak, as := memberLogin(t, r, "alice")
	g.replies = [][]string{{call(1, "invite", `{"login":"erin"}`)}, {say("Only an approver may.")}}
	_, events, _ = chatPost(t, r, ak, as, "/projects/demo/chat", url.Values{"message": {"invite erin"}})
	equal(t, "alice's stream", events, []string{
		`tool {"arguments":{"login":"erin"},"duration_ms":0,"name":"invite","result":"Refused: Only the approvers of project demo may invite a member."}`,
		`text {"text":"Only an approver may."}`, `done {}`})
	equal(t, "invited", r.invited, []string{"demo/demo dave write"})

	for _, c := range []struct {
		user, repository, login string
		status                  int
		msg                     string
	}{
		{"bob", "demo/demo", "no spaces here", http.StatusBadRequest, badLogin},
		{"bob", "demo/demo", strings.Repeat("a", 41), http.StatusBadRequest, badLogin},
		{"bob", "demo/demo", "..", http.StatusBadRequest, badLogin},
		{"bob", "demo/demo", "-x", http.StatusBadRequest, badLogin},
		{"alice", "sf/code", "dave", http.StatusConflict, "Project ai-software-factory-code is a seed project, whose collaborators Forgejo keeps: invite dave there."},
	} {
		_, err := r.c.doInvite(context.Background(), actor{s: &session{user: c.user}, repository: c.repository, via: factoryd.ViaChat}, c.login)
		var no *refusal
		if !errors.As(err, &no) || no.status != c.status || no.msg != c.msg {
			t.Errorf("%s invites %q on %s: %v, want %d %q", c.user, c.login, c.repository, err, c.status, c.msg)
		}
	}
	equal(t, "invited", r.invited, []string{"demo/demo dave write"})
}
