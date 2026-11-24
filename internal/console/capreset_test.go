package console

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

type capCall struct {
	cap, repository, login string
	seen                   int64
}

// fakeCaps records the resets pressed and answers resets as the store's
// read does: those in the window, newest first as given, or err. A press
// that did not see the newest reset of its cap and repository is refused
// as the store refuses it; one that did joins the resets, stamped at.
type fakeCaps struct {
	mu     sync.Mutex
	calls  []capCall
	resets []policy.CapReset
	at     time.Time
	err    error
}

func (f *fakeCaps) ResetCap(_ context.Context, cap, repository, login string, seen int64) (policy.CapReset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, capCall{cap, repository, login, seen})
	var newest *policy.CapReset
	var top int64
	for i, r := range f.resets {
		top = max(top, r.ID)
		if r.Cap == cap && r.Repository == repository && (newest == nil || r.ID > newest.ID) {
			newest = &f.resets[i]
		}
	}
	if newest != nil && newest.ID != seen {
		return policy.CapReset{}, &policy.StaleResetError{Newest: *newest}
	}
	r := policy.CapReset{ID: top + 1, Cap: cap, Repository: repository, Login: login, At: f.at}
	f.resets = append([]policy.CapReset{r}, f.resets...)
	return r, nil
}

func (f *fakeCaps) CapResets(_ context.Context, from, to time.Time) ([]policy.CapReset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []policy.CapReset
	for _, r := range f.resets {
		if !r.At.Before(from) && r.At.Before(to) {
			out = append(out, r)
		}
	}
	return out, nil
}

// capRig is the members rig (alice writes and approves the factory's code,
// bob approves demo) with a fake reset store and a wake that records what
// it ran for.
func capRig(t *testing.T) (*rig, *fakeCaps, *[]string) {
	t.Helper()
	r := newRig(t)
	members(r)
	caps, wakes := &fakeCaps{}, new([]string)
	r.c.cfg.Caps = caps
	r.c.cfg.WakeCapWaits = func(_ context.Context, why string) error { *wakes = append(*wakes, why); return nil }
	return r, caps, wakes
}

func TestCapResetDay(t *testing.T) {
	r, caps, wakes := capRig(t)
	ck, s := memberLogin(t, r, "alice")
	req := post("/caps/day/reset", url.Values{"csrf": {s.csrf}, "seen": {"0"}})
	req.Header.Set("HX-Request", "true")
	rec := r.do(req, ck)
	if !pressRefreshes(rec) {
		t.Fatalf("reset = %d %q, want 204 with board-refresh", rec.Code, rec.Body)
	}
	equal(t, "the resets", caps.calls, []capCall{{"day", "", "alice", 0}})
	equal(t, "the wakes", *wakes, []string{"console: alice reset the day cap's count (reset 1)"})
}

func TestCapResetMonth(t *testing.T) {
	r, caps, _ := capRig(t)
	ck, s := memberLogin(t, r, "alice")
	rec := r.do(post("/caps/month/reset", url.Values{"csrf": {s.csrf}, "seen": {"0"}}), ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a form post = %d, want the redirect to the board", rec.Code)
	}
	equal(t, "the resets", caps.calls, []capCall{{"month", "", "alice", 0}})
}

// The premium cap's Reset restarts this month's premium count, and a
// second press on the same view names the first.
func TestCapResetPremium(t *testing.T) {
	r, caps, wakes := capRig(t)
	r.c.clock = func() time.Time { return time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC) }
	caps.at = time.Date(2026, 11, 20, 14, 2, 0, 0, time.UTC)
	ck, s := memberLogin(t, r, "alice")
	press := func() *httptest.ResponseRecorder {
		req := post("/caps/premium/reset", url.Values{"csrf": {s.csrf}, "seen": {"0"}})
		req.Header.Set("HX-Request", "true")
		return r.do(req, ck)
	}
	if rec := press(); !pressRefreshes(rec) {
		t.Fatalf("reset = %d %q, want 204 with board-refresh", rec.Code, rec.Body)
	}
	equal(t, "the resets", caps.calls, []capCall{{"premium", "", "alice", 0}})
	equal(t, "the wakes", *wakes, []string{"console: alice reset the premium cap's count (reset 1)"})
	const line = "alice reset this month's premium count; the panel now shows it. Press Reset again to reset it once more.\n"
	if rec := press(); rec.Code != http.StatusConflict || rec.Body.String() != line {
		t.Fatalf("a second press on the same view = %d %q, want 409 %q", rec.Code, rec.Body, line)
	}
}

// A project's Reset restarts its own month cap's count; a project with no
// month cap of its own has none to reset: a 409 says so, refreshes the
// board, and nothing is written or woken.
func TestCapResetProject(t *testing.T) {
	r, caps, wakes := capRig(t)
	ck, s := memberLogin(t, r, "bob")
	press := func() *httptest.ResponseRecorder {
		req := post("/projects/demo/cap/reset", url.Values{"csrf": {s.csrf}, "seen": {"0"}})
		req.Header.Set("HX-Request", "true")
		return r.do(req, ck)
	}
	if rec := press(); rec.Code != http.StatusConflict || rec.Body.String() != "Project demo has no month cap of its own.\n" || rec.Header().Get("HX-Trigger") != "board-refresh" {
		t.Fatalf("a reset without demo's cap = %d %q, trigger %q; want 409 with board-refresh", rec.Code, rec.Body, rec.Header().Get("HX-Trigger"))
	}
	if len(caps.calls) != 0 || len(*wakes) != 0 {
		t.Fatalf("a reset without demo's cap wrote %v and woke for %q", caps.calls, *wakes)
	}
	r.ledger.monthCaps = map[string]float64{"demo": 30}
	if rec := press(); !pressRefreshes(rec) {
		t.Fatalf("reset = %d %q, want 204 with board-refresh", rec.Code, rec.Body)
	}
	equal(t, "the resets", caps.calls, []capCall{{"project", "demo/demo", "bob", 0}})
	equal(t, "the wakes", *wakes, []string{"console: bob reset the project cap's count of demo/demo (reset 1)"})
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
		{"no factory writer on the premium cap", post("/caps/premium/reset", url.Values{"csrf": {bs.csrf}}), bk, http.StatusForbidden},
		{"another cap", post("/caps/chat/reset", url.Values{"csrf": {as.csrf}}), ak, http.StatusNotFound},
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
	if len(caps.calls) != 0 || len(*wakes) != 0 {
		t.Errorf("a refused reset wrote %v and woke for %q", caps.calls, *wakes)
	}
}

func TestCapResetWithoutAStoreIsRefused(t *testing.T) {
	r, _, _ := capRig(t)
	r.c.cfg.Caps = nil
	ck, s := memberLogin(t, r, "alice")
	if rec := r.do(post("/caps/day/reset", url.Values{"csrf": {s.csrf}, "seen": {"0"}}), ck); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store = %d %q, want 503", rec.Code, rec.Body)
	}
}

func TestCapResetWakeFailureStillResets(t *testing.T) {
	r, caps, _ := capRig(t)
	r.c.cfg.WakeCapWaits = func(context.Context, string) error { return errors.New("signal down") }
	ck, s := memberLogin(t, r, "alice")
	req := post("/caps/day/reset", url.Values{"csrf": {s.csrf}, "seen": {"0"}})
	req.Header.Set("HX-Request", "true")
	if rec := r.do(req, ck); !pressRefreshes(rec) {
		t.Fatalf("reset = %d %q, want 204 with board-refresh", rec.Code, rec.Body)
	}
	equal(t, "the resets", caps.calls, []capCall{{"day", "", "alice", 0}})
}

// A press names the newest reset of its cap its panel showed. A second
// press on the same view, from another tab, finds the first one: a 409
// says who reset the count and when, its press refreshes the board, and
// nothing is written or woken; a press that saw the first passes. Without
// htmx the 409 stands alone. A project's line names its count, dated when
// the reset was on another day; a form that names no reset is a 400.
func TestCapResetOnANewerReset(t *testing.T) {
	r, caps, wakes := capRig(t)
	r.c.clock = func() time.Time { return time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC) }
	caps.at = time.Date(2026, 11, 20, 14, 2, 0, 0, time.UTC)
	ck, s := memberLogin(t, r, "alice")
	press := func(path, seen string, htmx bool, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		t.Helper()
		req := post(path, url.Values{"csrf": {csrf}, "seen": {seen}})
		if htmx {
			req.Header.Set("HX-Request", "true")
		}
		return r.do(req, cookie)
	}
	if rec := press("/caps/day/reset", "0", true, ck, s.csrf); !pressRefreshes(rec) {
		t.Fatalf("the first press = %d %q, want 204 with board-refresh", rec.Code, rec.Body)
	}
	const line = "alice reset today's count; the panel now shows it. Press Reset again to reset it once more.\n"
	rec := press("/caps/day/reset", "0", true, ck, s.csrf)
	if rec.Code != http.StatusConflict || rec.Body.String() != line || rec.Header().Get("HX-Trigger") != "board-refresh" {
		t.Fatalf("the second press on the same view = %d %q, trigger %q; want 409 %q with board-refresh", rec.Code, rec.Body, rec.Header().Get("HX-Trigger"), line)
	}
	equal(t, "the resets after the refused press", len(caps.resets), 1)
	equal(t, "the wakes after the refused press", len(*wakes), 1)
	if rec := press("/caps/day/reset", "0", false, ck, s.csrf); rec.Code != http.StatusConflict || rec.Body.String() != line || rec.Header().Get("HX-Trigger") != "" {
		t.Fatalf("a form post on the same view = %d %q, trigger %q; want 409 %q alone", rec.Code, rec.Body, rec.Header().Get("HX-Trigger"), line)
	}
	if rec := press("/caps/day/reset", "1", true, ck, s.csrf); !pressRefreshes(rec) {
		t.Fatalf("a press that saw reset 1 = %d %q, want 204 with board-refresh", rec.Code, rec.Body)
	}
	equal(t, "the resets after a press that saw the first", len(caps.resets), 2)
	equal(t, "the wakes after a press that saw the first", len(*wakes), 2)

	r.ledger.monthCaps = map[string]float64{"demo": 30}
	caps.resets = append(caps.resets, policy.CapReset{ID: 9, Cap: policy.CapProject, Repository: "demo/demo", At: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC), Login: "alice"})
	bk, bs := memberLogin(t, r, "bob")
	if rec := press("/projects/demo/cap/reset", "0", true, bk, bs.csrf); rec.Code != http.StatusConflict ||
		rec.Body.String() != "alice reset the project's month count; the panel now shows it. Press Reset again to reset it once more.\n" {
		t.Fatalf("bob's press on demo's older count = %d %q, want 409 naming alice's reset of 3 Nov", rec.Code, rec.Body)
	}
	for _, seen := range []string{"", "x", "-"} {
		if rec := press("/caps/month/reset", seen, true, ck, s.csrf); rec.Code != http.StatusBadRequest {
			t.Errorf("a press that saw %q = %d %q, want 400", seen, rec.Code, rec.Body)
		}
	}
	equal(t, "the presses that reached the store", len(caps.calls), 5)
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
