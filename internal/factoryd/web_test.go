package factoryd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// webGateway records the tool door call it answers with plain text, or
// with the status and body a failure carries.
type webGateway struct {
	status  int
	header  map[string]string
	body    string
	gotPath string
	gotAuth string
	gotTask string
	sent    string
}

func (g *webGateway) start(t *testing.T) *WebTools {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.gotPath = r.URL.Path
		g.gotAuth = r.Header.Get("Authorization")
		g.gotTask = r.Header.Get(gateway.HeaderFactoryTask)
		b, _ := io.ReadAll(r.Body)
		g.sent = string(b)
		for k, v := range g.header {
			w.Header().Set(k, v)
		}
		if g.status != 0 {
			w.WriteHeader(g.status)
			io.WriteString(w, g.body)
			return
		}
		io.WriteString(w, g.body)
	}))
	t.Cleanup(srv.Close)
	return &WebTools{GatewayURL: srv.URL, Token: "gw-token"}
}

// One search and one scrape go to the chat tool door on the chat task's
// account, the call's arguments as the tool's JSON.
func TestWebToolsCallsTheDoor(t *testing.T) {
	g := &webGateway{body: "the answer"}
	w := g.start(t)
	text, err := w.Search(context.Background(), "t-chat-code-2026-11", "example domains", 5)
	if err != nil || text != "the answer" {
		t.Fatalf("Search = %q, %v", text, err)
	}
	if g.gotPath != "/v1/tools/web_search" || g.gotAuth != "Bearer gw-token" || g.gotTask != "t-chat-code-2026-11" {
		t.Fatalf("the call went to %s as %s for %s", g.gotPath, g.gotAuth, g.gotTask)
	}
	if g.sent != `{"query":"example domains","limit":5}` {
		t.Fatalf("search body %s", g.sent)
	}
	if _, err := w.Scrape(context.Background(), "t-chat-code-2026-11", "https://example.com/"); err != nil {
		t.Fatal(err)
	}
	if g.gotPath != "/v1/tools/web_scrape" || g.sent != `{"url":"https://example.com/"}` {
		t.Fatalf("scrape went to %s with %s", g.gotPath, g.sent)
	}
}

// A refusal comes back as the policy's, named by the gateway's decision
// header; any other status as a StatusError.
func TestWebToolsRefusalAndStatus(t *testing.T) {
	g := &webGateway{status: http.StatusPaymentRequired,
		header: map[string]string{gateway.HeaderDecision: "refused_tool_requests"},
		body:   `{"error":{"message":"web_search's 500 requests are used"}}`}
	_, err := g.start(t).Search(context.Background(), "t-chat-code-2026-11", "x", 5)
	var ref *policy.Refusal
	if !errors.As(err, &ref) || ref.Decision != "refused_tool_requests" || !strings.Contains(ref.Reason, "500 requests are used") {
		t.Fatalf("Search error = %v, want the policy's refusal", err)
	}
	failing := &webGateway{status: http.StatusBadGateway, body: "upstream down"}
	_, err = failing.start(t).Scrape(context.Background(), "t-chat-code-2026-11", "https://example.com/")
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusBadGateway {
		t.Fatalf("Scrape error = %v, want a 502 StatusError", err)
	}
}
