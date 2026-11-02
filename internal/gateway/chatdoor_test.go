package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// chatTask is the chat task EnsureChatTask files: running, the chat
// class, on the project's repository.
func chatTask(status string) Task {
	return Task{ID: "t-chat-code-2026-11", Status: status, Classes: []string{policy.ClassChat}, Repository: "sf/code", BudgetEUR: 5}
}

// chatDoorPolicy is toolPolicy with the chat block listing both web
// tools; a caller may drop a tool to test the listing check.
func chatDoorPolicy(tools map[string]policy.ToolCap) policy.Version {
	v := toolPolicy(policy.ToolCap{MaxRequests: 30, MaxEUR: 0.2})
	c := policy.DefaultChat
	c.Tools = tools
	v.Document.Chat = &c
	return v
}

var bothWebTools = map[string]policy.ToolCap{
	policy.ToolWebSearch: {MaxRequests: 30, MaxEUR: 0.2},
	policy.ToolWebScrape: {MaxRequests: 30, MaxEUR: 0.2},
}

type chatDoorEnv struct {
	srv       *httptest.Server
	store     *memStore
	providers *fakeProviders
}

func newChatDoorEnv(t *testing.T, pv policy.Version, tasks ...Task) *chatDoorEnv {
	t.Helper()
	store := newMemStore(tasks...)
	store.setPolicy(pv)
	providers := newFakeProviders(t)
	s := NewServer(ServerOptions{
		Auth: fakeAuth{id: Identity{TaskID: "42", SAUID: "u", Session: "s-1"}}, Store: store, Minter: &fakeMinter{}, Prices: testPrices,
		Firecrawl:     &Firecrawl{BaseURL: providers.URL, Key: "fc-key", HTTP: providers.Client()},
		FactorydToken: "fd-token",
	})
	srv := httptest.NewServer(s.Routes("test"))
	t.Cleanup(srv.Close)
	return &chatDoorEnv{srv: srv, store: store, providers: providers}
}

// call posts one tool call to the door.
func (e *chatDoorEnv) call(t *testing.T, auth, taskID, tool, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/v1/tools/"+tool, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+auth)
	req.Header.Set(HeaderFactoryTask, taskID)
	res, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, string(text)
}

// toolRows counts the chat task's booked tool calls and their euros.
func (e *chatDoorEnv) toolRows() (int, float64) {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	n, eur := 0, 0.0
	for _, l := range e.store.ledger {
		if l.taskID == chatTask("running").ID && l.kind == policy.KindTool {
			n++
			eur += l.eur
		}
	}
	return n, eur
}

// The happy path: both web tools answer with the provider's text and the
// cost lands on the chat task's account, audited under the policy version.
func TestChatDoorRunsTheWebTools(t *testing.T) {
	e := newChatDoorEnv(t, chatDoorPolicy(bothWebTools), chatTask("running"))
	res, text := e.call(t, "fd-token", "t-chat-code-2026-11", policy.ToolWebSearch, `{"query":"example domains","limit":3}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("web_search status %d: %s", res.StatusCode, text)
	}
	if !strings.Contains(text, "Example Domains") || !strings.Contains(text, "iana.org") {
		t.Fatalf("web_search answer %q, want the found pages' titles and urls", text)
	}
	res, text = e.call(t, "fd-token", "t-chat-code-2026-11", policy.ToolWebScrape, `{"url":"https://example.com/"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("web_scrape status %d: %s", res.StatusCode, text)
	}
	if !strings.Contains(text, "Example Domain") {
		t.Fatalf("web_scrape answer %q, want the page", text)
	}
	if n, eur := e.toolRows(); n != 2 || !near6(eur, 3*0.005) {
		t.Fatalf("booked %d tool calls at %.6f EUR, want 2 at 0.015", n, eur)
	}
	rows := e.store.auditRows()
	last := rows[len(rows)-1]
	if last.Tool != policy.ToolWebScrape || last.Decision != "allowed" || last.PolicyVersion == nil || *last.PolicyVersion != 7 {
		t.Fatalf("last audit row = %+v, want an allowed web_scrape under policy 7", last)
	}
}

// Every refusal names itself in X-Gateway-Decision and answers an error
// body: no sandbox JWT opens the door, the task must exist and be the
// chat's, and the arguments must hold.
func TestChatDoorRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		auth, taskID, tool, body string
		tasks                    []Task
		status                   int
		decision, typ, reason    string
	}{
		"a sandbox JWT": {"sandbox-jwt", "42", policy.ToolWebSearch, `{"query":"x"}`, []Task{chatTask("running"), running("coder")},
			http.StatusUnauthorized, "refused_auth", "authentication_error", "factoryd shared token"},
		"no task header": {"fd-token", "", policy.ToolWebSearch, `{"query":"x"}`, []Task{chatTask("running")}, http.StatusBadRequest, "refused_auth", "invalid_request_error", HeaderFactoryTask},
		"an unknown task": {"fd-token", "t-chat-none", policy.ToolWebSearch, `{"query":"x"}`, []Task{chatTask("running")},
			http.StatusNotFound, "refused_task_unknown", "invalid_request_error", "no task row"},
		"an agent task": {"fd-token", "42", policy.ToolWebSearch, `{"query":"x"}`, []Task{chatTask("running"), running("coder")},
			http.StatusForbidden, "refused_capability", "permission_error", "is not one"},
		"a closed chat task": {"fd-token", "t-chat-code-2026-11", policy.ToolWebSearch, `{"query":"x"}`, []Task{chatTask("closed")},
			http.StatusConflict, "refused_task_status", "invalid_request_error", "the task is closed"},
		"an empty query": {"fd-token", "t-chat-code-2026-11", policy.ToolWebSearch, `{"query":" "}`, []Task{chatTask("running")},
			http.StatusBadRequest, "refused_arguments", "invalid_request_error", "query"},
		"a private URL": {"fd-token", "t-chat-code-2026-11", policy.ToolWebScrape, `{"url":"http://10.0.0.5/"}`, []Task{chatTask("running")},
			http.StatusBadRequest, "refused_arguments", "invalid_request_error", "not a public DNS name"},
		"an unknown tool": {"fd-token", "t-chat-code-2026-11", "replicate_predict", `{}`, []Task{chatTask("running")},
			http.StatusForbidden, "refused_capability", "permission_error", "chat.tools"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newChatDoorEnv(t, chatDoorPolicy(bothWebTools), tc.tasks...)
			res, text := e.call(t, tc.auth, tc.taskID, tc.tool, tc.body)
			if res.StatusCode != tc.status {
				t.Fatalf("status %d: %s", res.StatusCode, text)
			}
			if got := res.Header.Get(HeaderDecision); got != tc.decision {
				t.Fatalf("%s = %q, want %q", HeaderDecision, got, tc.decision)
			}
			if !strings.Contains(text, `"type":`+`"`+tc.typ) || !strings.Contains(text, tc.reason) {
				t.Fatalf("body %q, want type %q and %q", text, tc.typ, tc.reason)
			}
			if n, _ := e.toolRows(); n != 0 {
				t.Fatalf("booked %d tool calls, want none on a refusal", n)
			}
		})
	}
}

// A tool the chat block does not list is refused even though the agent
// classes carry it: the door reads only chat.tools.
func TestChatDoorOnlyTheListedTools(t *testing.T) {
	e := newChatDoorEnv(t, chatDoorPolicy(map[string]policy.ToolCap{policy.ToolWebSearch: {MaxRequests: 30, MaxEUR: 0.2}}), chatTask("running"))
	res, text := e.call(t, "fd-token", "t-chat-code-2026-11", policy.ToolWebScrape, `{"url":"https://example.com/"}`)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d: %s", res.StatusCode, text)
	}
	if got := res.Header.Get(HeaderDecision); got != "refused_capability" {
		t.Fatalf("%s = %q, want refused_capability", HeaderDecision, got)
	}
	if !strings.Contains(text, "chat.tools") {
		t.Fatalf("body %q, want chat.tools named", text)
	}
}

// The chat's tool caps count the task's booked calls: the second search
// past max_requests is refused at 402, nothing booked.
func TestChatDoorCountsTheCap(t *testing.T) {
	tools := map[string]policy.ToolCap{policy.ToolWebSearch: {MaxRequests: 1, MaxEUR: 0.2},
		policy.ToolWebScrape: {MaxRequests: 30, MaxEUR: 0.2}}
	e := newChatDoorEnv(t, chatDoorPolicy(tools), chatTask("running"))
	if res, text := e.call(t, "fd-token", "t-chat-code-2026-11", policy.ToolWebSearch, `{"query":"example domains"}`); res.StatusCode != http.StatusOK {
		t.Fatalf("first call status %d: %s", res.StatusCode, text)
	}
	res, text := e.call(t, "fd-token", "t-chat-code-2026-11", policy.ToolWebSearch, `{"query":"example domains"}`)
	if res.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("second call status %d: %s", res.StatusCode, text)
	}
	if got := res.Header.Get(HeaderDecision); got != "refused_tool_requests" {
		t.Fatalf("%s = %q, want refused_tool_requests", HeaderDecision, got)
	}
	if n, _ := e.toolRows(); n != 1 {
		t.Fatalf("booked %d tool calls, want one", n)
	}
}

// A provider failure answers 502 with the failure named, the call booked
// at its hold.
func TestChatDoorProviderFailure(t *testing.T) {
	e := newChatDoorEnv(t, chatDoorPolicy(bothWebTools), chatTask("running"))
	e.providers.mu.Lock()
	e.providers.fails["POST /v2/search"] = 1
	e.providers.mu.Unlock()
	res, text := e.call(t, "fd-token", "t-chat-code-2026-11", policy.ToolWebSearch, `{"query":"example domains"}`)
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d: %s", res.StatusCode, text)
	}
	if !strings.Contains(text, "upstream_error") {
		t.Fatalf("body %q, want an upstream_error", text)
	}
	if n, _ := e.toolRows(); n != 1 {
		t.Fatalf("booked %d tool calls, want one at the hold", n)
	}
}
