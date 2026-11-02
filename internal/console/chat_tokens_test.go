package console

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sealed"
)

// fakeChatTokens is the rig's chat token store.
type fakeChatTokens struct {
	mu   sync.Mutex
	rows []factoryd.ChatToken
}

func (f *fakeChatTokens) AddChatToken(_ context.Context, t factoryd.ChatToken) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t.ID = int64(len(f.rows) + 1)
	t.Created = time.Now()
	f.rows = append(f.rows, t)
	return t.ID, nil
}

func (f *fakeChatTokens) ChatTokenByHash(_ context.Context, hash string) (factoryd.ChatToken, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.rows {
		if t.Hash == hash {
			return t, true, nil
		}
	}
	return factoryd.ChatToken{}, false, nil
}

func (f *fakeChatTokens) ChatTokensOf(_ context.Context, login string) ([]factoryd.ChatToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []factoryd.ChatToken
	for _, t := range f.rows {
		if t.Login == login && t.Revoked.IsZero() {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeChatTokens) RevokeChatToken(_ context.Context, id int64, login string, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, t := range f.rows {
		if t.ID == id && t.Login == login && t.Revoked.IsZero() {
			f.rows[i].Revoked = now
			return true, nil
		}
	}
	return false, nil
}

// TouchChatToken moves id's last use to now, at most once a minute, as
// the store does.
func (f *fakeChatTokens) TouchChatToken(_ context.Context, id int64, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, t := range f.rows {
		if t.ID == id && (t.LastUsed.IsZero() || t.LastUsed.Before(now.Add(-time.Minute))) {
			f.rows[i].LastUsed = now
		}
	}
	return nil
}

// ClaimChatTokenConversation gives token id thread n while it has none
// and answers the thread in force, as the store does.
func (f *fakeChatTokens) ClaimChatTokenConversation(_ context.Context, id, n int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, t := range f.rows {
		if t.ID == id {
			if t.Conversation == 0 {
				f.rows[i].Conversation = n
			}
			return f.rows[i].Conversation, nil
		}
	}
	return 0, sql.ErrNoRows
}

// tokenRig is the rig with the notifications page and the chat tokens, and
// sebastian logged in.
func tokenRig(t *testing.T) (*rig, *http.Cookie, *session) {
	t.Helper()
	r := newRig(t)
	cfg := r.c.cfg
	cfg.Messengers = &Messengers{Store: &fakeMessengerStore{levels: map[string]string{}}}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.c = c
	ck, s := r.loggedIn(t)
	return r, ck, s
}

func makeForm(s *session, forgejo string) url.Values {
	return url.Values{"csrf": {s.csrf}, "project": {"sf/code"}, "name": {"laptop"}, "life": {"30"}, "forgejo": {forgejo}}
}

func TestMakeChatTokenShowsItOnce(t *testing.T) {
	r, ck, s := tokenRig(t)
	r.fg.tokens = map[string]string{"fj-secret": "sebastian"}
	rec := r.do(post("/notifications/tokens", makeForm(s, "fj-secret")), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d %q", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	body := rec.Body.String()
	i := strings.Index(body, "sfc_")
	if i < 0 || len(body) < i+47 {
		t.Fatalf("no bearer in the answer:\n%s", body)
	}
	bearer := body[i : i+47]
	if redact.ChatToken.FindString(bearer) != bearer {
		t.Fatalf("bearer %q is not sfc_ and 43 characters", bearer)
	}
	line := `claude mcp add --transport http factory-ai-software-factory-code https://console.example/mcp --header &#34;Authorization: Bearer ` + bearer + `&#34;`
	if !strings.Contains(body, line) {
		t.Fatalf("the answer misses the line %q:\n%s", line, body)
	}
	row, ok, _ := r.chatTokens.ChatTokenByHash(context.Background(), chatTokenHash(bearer))
	if !ok || len(r.chatTokens.rows) != 1 {
		t.Fatalf("the store holds %d rows, by hash %v", len(r.chatTokens.rows), ok)
	}
	if row.Login != "sebastian" || row.Repository != "sf/code" || row.Name != "laptop" {
		t.Fatalf("row = %+v", row)
	}
	if d := row.Expires.Sub(time.Now()); d < 29*24*time.Hour || d > 30*24*time.Hour {
		t.Fatalf("expires in %v, want 30 days", d)
	}
	if strings.Contains(row.Sealed, "fj-secret") || strings.Contains(row.Hash, bearer) {
		t.Fatalf("the row holds a plain secret: %+v", row)
	}
	opened, err := sealed.Open(r.c.cfg.SessionKey, chatTokenKeyLabel, row.Sealed, row.Hash)
	if err != nil || opened != "fj-secret" {
		t.Fatalf("Open = %q, %v", opened, err)
	}
	evs := eventsOf(r, factoryd.EventChatTokenMade)
	if len(evs) != 1 || evs[0].Actor != "sebastian" || evs[0].Via != factoryd.ViaButton || evs[0].Detail["name"] != "laptop" || evs[0].Detail["project"] != "sf/code" || evs[0].Detail["token"] != int64(1) {
		t.Fatalf("events = %+v", evs)
	}

	if !strings.Contains(body, time.Now().Format("2006-01-02")) {
		t.Fatalf("the answer's table misses the created date")
	}
	page := r.do(httptest.NewRequest(http.MethodGet, "/notifications", nil), ck).Body.String()
	if !strings.Contains(page, "laptop") || strings.Contains(page, "sfc_") {
		t.Fatalf("the page after shows %q and sfc_ %v", "laptop", strings.Contains(page, "sfc_"))
	}
}

func TestMakeChatTokenRefuses(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *fakeForgejo, form url.Values)
		code   int
		want   string
	}{
		{"another login", func(f *fakeForgejo, _ url.Values) { f.tokens["fj"] = "mallory" }, http.StatusBadRequest, "this Forgejo token belongs to mallory, not to you"},
		{"admin reach", func(f *fakeForgejo, _ url.Values) { f.adminReach = true }, http.StatusBadRequest, "this Forgejo token reaches more than read:user, write:repository and write:issue; make one with only those three scopes"},
		{"notification reach", func(f *fakeForgejo, _ url.Values) { f.notificationReach = true }, http.StatusBadRequest, "this Forgejo token reaches more than read:user, write:repository and write:issue; make one with only those three scopes"},
		{"unknown token", func(f *fakeForgejo, form url.Values) { form.Set("forgejo", "nope") }, http.StatusBadRequest, "Forgejo refused this token"},
		{"project not written", func(_ *fakeForgejo, form url.Values) { form.Set("project", "other/repo") }, http.StatusForbidden, "you do not write other/repo"},
		{"life 365", func(_ *fakeForgejo, form url.Values) { form.Set("life", "365") }, http.StatusBadRequest, "choose 7, 30 or 90 days"},
		{"no life", func(_ *fakeForgejo, form url.Values) { form.Del("life") }, http.StatusBadRequest, "choose 7, 30 or 90 days"},
		{"probe 500", func(f *fakeForgejo, _ url.Values) { f.probeStatus = http.StatusInternalServerError }, http.StatusBadRequest, "Forgejo refused this token"},
		{"empty name", func(_ *fakeForgejo, form url.Values) { form.Set("name", "  ") }, http.StatusBadRequest, "the name needs 1 to 64 characters"},
		{"long name", func(_ *fakeForgejo, form url.Values) { form.Set("name", strings.Repeat("ä", 65)) }, http.StatusBadRequest, "the name needs 1 to 64 characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, ck, s := tokenRig(t)
			r.fg.tokens = map[string]string{"fj": "sebastian"}
			form := makeForm(s, "fj")
			tc.mutate(r.fg, form)
			rec := r.do(post("/notifications/tokens", form), ck)
			if rec.Code != tc.code || strings.TrimSpace(rec.Body.String()) != tc.want {
				t.Fatalf("POST = %d %q, want %d %q", rec.Code, rec.Body.String(), tc.code, tc.want)
			}
			if len(r.chatTokens.rows) != 0 {
				t.Fatalf("the store holds %+v", r.chatTokens.rows)
			}
		})
	}
}

func TestMakeChatTokenNeedsCSRF(t *testing.T) {
	r, ck, s := tokenRig(t)
	r.fg.tokens = map[string]string{"fj": "sebastian"}
	form := makeForm(s, "fj")
	form.Del("csrf")
	if rec := r.do(post("/notifications/tokens", form), ck); rec.Code != http.StatusForbidden || len(r.chatTokens.rows) != 0 {
		t.Fatalf("POST = %d, rows %d", rec.Code, len(r.chatTokens.rows))
	}
}

func TestRevokeChatTokenOnlyOwn(t *testing.T) {
	r, ck, s := tokenRig(t)
	r.chatTokens.rows = []factoryd.ChatToken{
		{ID: 1, Login: "sebastian", Repository: "sf/code", Name: "laptop"},
		{ID: 2, Login: "alice", Repository: "sf/code", Name: "hers"},
	}
	rec := r.do(post("/notifications/tokens/revoke", url.Values{"csrf": {s.csrf}, "id": {"2"}}), ck)
	if rec.Code != http.StatusBadRequest || !r.chatTokens.rows[1].Revoked.IsZero() {
		t.Fatalf("another human's id = %d %q, revoked %v", rec.Code, rec.Body.String(), r.chatTokens.rows[1].Revoked)
	}
	rec = r.do(post("/notifications/tokens/revoke", url.Values{"csrf": {s.csrf}, "id": {"1"}}), ck)
	if rec.Code != http.StatusSeeOther || r.chatTokens.rows[0].Revoked.IsZero() {
		t.Fatalf("own id = %d, revoked %v", rec.Code, r.chatTokens.rows[0].Revoked)
	}
	if rec = r.do(post("/notifications/tokens/revoke", url.Values{"csrf": {s.csrf}, "id": {"1"}}), ck); rec.Code != http.StatusBadRequest {
		t.Fatalf("a second press = %d", rec.Code)
	}
	evs := eventsOf(r, factoryd.EventChatTokenRevoked)
	if len(evs) != 1 || evs[0].Actor != "sebastian" || evs[0].Detail["name"] != "laptop" || evs[0].Detail["token"] != int64(1) {
		t.Fatalf("events = %+v", evs)
	}
}

func TestScrubTokensRemovesChatToken(t *testing.T) {
	bearer := "sfc_" + strings.Repeat("aBcD-_", 7) + "a"
	got, scrubbed := scrubTokens("my key is " + bearer + " ok")
	if !scrubbed || got != "my key is "+tokenMask+" ok" {
		t.Fatalf("scrubTokens = %q, %v", got, scrubbed)
	}
}

func TestChatTokenBearerShape(t *testing.T) {
	b, h, err := newChatToken()
	if err != nil || len(b) != 47 || redact.ChatToken.FindString(b) != b || h != chatTokenHash(b) || len(h) != 64 {
		t.Fatalf("newChatToken = %q, %q, %v", b, h, err)
	}
}

func TestChatTokenFormOffersWrittenProjectsOnly(t *testing.T) {
	r, _, _ := tokenRig(t)
	ck, _ := r.readerOfCode(t, "bob")
	page := r.do(httptest.NewRequest(http.MethodGet, "/notifications", nil), ck).Body.String()
	i := strings.Index(page, `id="token-project"`)
	if i < 0 {
		t.Fatalf("no project select:\n%s", page)
	}
	sel := page[i : i+strings.Index(page[i:], "</select>")]
	if !strings.Contains(sel, `value="demo-app/demo-app"`) || strings.Contains(sel, `value="sf/code"`) {
		t.Fatalf("select = %s", sel)
	}
}
