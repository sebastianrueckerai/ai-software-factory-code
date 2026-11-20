package console

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

type capCall struct{ cap, repository, login string }

type fakeCaps struct {
	mu    sync.Mutex
	calls []capCall
}

func (f *fakeCaps) ResetCap(_ context.Context, cap, repository, login string) (policy.CapReset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, capCall{cap, repository, login})
	return policy.CapReset{ID: int64(len(f.calls)), Cap: cap, Repository: repository, Login: login, At: time.Now()}, nil
}

func (f *fakeCaps) CapResets(context.Context, time.Time, time.Time) ([]policy.CapReset, error) {
	return nil, nil
}

// capRig is the members rig (alice writes and approves the factory's code,
// bob approves demo) with a fake reset store and a counted wake.
func capRig(t *testing.T) (*rig, *fakeCaps, *int) {
	t.Helper()
	r := newRig(t)
	members(r)
	caps, wakes := &fakeCaps{}, new(int)
	r.c.cfg.Caps = caps
	r.c.cfg.WakeCapWaits = func(context.Context) error { *wakes++; return nil }
	return r, caps, wakes
}

func TestCapResetDay(t *testing.T) {
	r, caps, wakes := capRig(t)
	ck, s := memberLogin(t, r, "alice")
	req := post("/caps/day/reset", url.Values{"csrf": {s.csrf}})
	req.Header.Set("HX-Request", "true")
	rec := r.do(req, ck)
	if !pressRefreshes(rec) {
		t.Fatalf("reset = %d %q, want 204 with board-refresh", rec.Code, rec.Body)
	}
	equal(t, "the resets", caps.calls, []capCall{{"day", "", "alice"}})
	equal(t, "the wakes", *wakes, 1)
}

func TestCapResetMonth(t *testing.T) {
	r, caps, _ := capRig(t)
	ck, s := memberLogin(t, r, "alice")
	rec := r.do(post("/caps/month/reset", url.Values{"csrf": {s.csrf}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a form post = %d, want the redirect to the board", rec.Code)
	}
	equal(t, "the resets", caps.calls, []capCall{{"month", "", "alice"}})
}

func TestCapResetProject(t *testing.T) {
	r, caps, wakes := capRig(t)
	ck, s := memberLogin(t, r, "bob")
	req := post("/projects/demo/cap/reset", url.Values{"csrf": {s.csrf}})
	req.Header.Set("HX-Request", "true")
	if rec := r.do(req, ck); !pressRefreshes(rec) {
		t.Fatalf("reset = %d %q, want 204 with board-refresh", rec.Code, rec.Body)
	}
	equal(t, "the resets", caps.calls, []capCall{{"project", "demo/demo", "bob"}})
	equal(t, "the wakes", *wakes, 1)
}

func TestCapResetAccess(t *testing.T) {
	r, caps, wakes := capRig(t)
	ak, as := memberLogin(t, r, "alice")
	bk, bs := memberLogin(t, r, "bob")

	for _, tc := range []struct {
		name   string
		req    *http.Request
		cookie *http.Cookie
		want   int
	}{
		{"no csrf", post("/caps/day/reset", url.Values{}), ak, http.StatusForbidden},
		{"no csrf on a project", post("/projects/demo/cap/reset", url.Values{}), bk, http.StatusForbidden},
		{"no factory writer", post("/caps/month/reset", url.Values{"csrf": {bs.csrf}}), bk, http.StatusForbidden},
		{"another project's approver", post("/projects/demo/cap/reset", url.Values{"csrf": {as.csrf}}), ak, http.StatusForbidden},
		{"another cap", post("/caps/premium/reset", url.Values{"csrf": {as.csrf}}), ak, http.StatusNotFound},
		{"the project's cap as a factory cap", post("/caps/project/reset", url.Values{"csrf": {as.csrf}}), ak, http.StatusNotFound},
	} {
		if rec := r.do(tc.req, tc.cookie); rec.Code != tc.want {
			t.Errorf("%s = %d %q, want %d", tc.name, rec.Code, rec.Body, tc.want)
		}
	}

	req := post("/caps/day/reset", url.Values{"csrf": {as.csrf}})
	req.Header.Set("Authorization", "Bearer chat-token")
	if rec := r.do(req); rec.Code != http.StatusUnauthorized {
		t.Errorf("a bearer token alone = %d %q, want 401", rec.Code, rec.Body)
	}
	if len(caps.calls) != 0 || *wakes != 0 {
		t.Errorf("a refused reset wrote %v and woke %d times", caps.calls, *wakes)
	}
}

func TestCapResetWithoutAStoreIsRefused(t *testing.T) {
	r, _, _ := capRig(t)
	r.c.cfg.Caps = nil
	ck, s := memberLogin(t, r, "alice")
	if rec := r.do(post("/caps/day/reset", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store = %d %q, want 503", rec.Code, rec.Body)
	}
}

func TestCapResetWakeFailureStillResets(t *testing.T) {
	r, caps, _ := capRig(t)
	r.c.cfg.WakeCapWaits = func(context.Context) error { return errors.New("signal down") }
	ck, s := memberLogin(t, r, "alice")
	req := post("/caps/day/reset", url.Values{"csrf": {s.csrf}})
	req.Header.Set("HX-Request", "true")
	if rec := r.do(req, ck); !pressRefreshes(rec) {
		t.Fatalf("reset = %d %q, want 204 with board-refresh", rec.Code, rec.Body)
	}
	equal(t, "the resets", caps.calls, []capCall{{"day", "", "alice"}})
}

// namesReset reports whether a tool name says reset, the one verb of a
// cap's restart; the only tool that does is the board filter's own.
func namesReset(name string) bool {
	return name != "board_view_reset" && strings.Contains(strings.ToLower(name), "reset")
}

// A reset is a human's press: no chat tool and no MCP tool names it, and
// its two routes need a human's roles.
func TestNoAgentToolResets(t *testing.T) {
	// board_view_reset clears a board filter, not a cap's count.
	check := func(kind, name string) {
		if namesReset(name) {
			t.Errorf("%s %q names a reset", kind, name)
		}
	}
	for name, want := range map[string]bool{"reset_cap": true, "cap-reset": true, "ResetCaps": true, "caps": false, "board_view_reset": false} {
		if namesReset(name) != want {
			t.Errorf("namesReset(%q) = %v, want %v", name, !want, want)
		}
	}
	for key := range accessTable {
		if !strings.HasPrefix(key, "POST ") && !strings.HasPrefix(key, "GET ") {
			check("access-table tool", key)
		}
	}
	for _, tool := range tools {
		check("chat tool", tool.name)
	}
	for _, name := range policy.ChatTools {
		check("policy chat tool", name)
	}
	_, srv, bearer := mcpRig(t)
	res, err := opened(t, srv, &door{auth: "Bearer " + bearer}).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		check("MCP tool", tool.Name)
	}

	posts := map[string]bool{}
	for _, p := range (&Console{}).postRoutes() {
		posts[p.pattern] = true
	}
	for pattern, want := range map[string]accessRule{
		"POST /caps/{cap}/reset":          {need: factoryWriter, scope: scFactory, what: "reset a factory cap's count"},
		"POST /projects/{name}/cap/reset": {need: approver, scope: scProject, what: "reset the project's month count"},
	} {
		if !posts[pattern] || accessTable[pattern] != want {
			t.Errorf("%s: a POST route %v with rule %+v, want %+v", pattern, posts[pattern], accessTable[pattern], want)
		}
	}
}
