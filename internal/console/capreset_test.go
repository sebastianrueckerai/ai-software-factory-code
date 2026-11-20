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

// A reset is a human's press: no chat tool and no MCP tool names it.
func TestNoAgentToolResets(t *testing.T) {
	for key := range accessTable {
		if !strings.HasPrefix(key, "POST ") && strings.Contains(key, "cap") {
			t.Errorf("the access table's tool %q names a cap", key)
		}
	}
	for _, tool := range tools {
		if strings.Contains(tool.name, "cap") {
			t.Errorf("chat tool %q names a cap", tool.name)
		}
	}
	for _, name := range policy.ChatTools {
		if strings.Contains(name, "cap") {
			t.Errorf("policy chat tool %q names a cap", name)
		}
	}
	_, srv, bearer := mcpRig(t)
	res, err := opened(t, srv, &door{auth: "Bearer " + bearer}).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if strings.Contains(tool.Name, "cap") || strings.Contains(tool.Name, "reset") {
			t.Errorf("MCP tool %q names a reset", tool.Name)
		}
	}
}
