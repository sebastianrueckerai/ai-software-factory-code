package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// fakeMessengerStore is the rig's messenger store: the codes it handed
// out, the member's links and levels, and the venues unlinked.
type fakeMessengerStore struct {
	codes    int
	links    []factoryd.MessengerLink
	levels   map[string]string
	unlinked []string
}

func (f *fakeMessengerStore) IssueMessengerCode(context.Context, string) (string, error) {
	f.codes++
	return strings.Repeat("c", 22), nil
}

func (f *fakeMessengerStore) MessengerLinks(context.Context, string) ([]factoryd.MessengerLink, error) {
	return f.links, nil
}

func (f *fakeMessengerStore) UnlinkMessenger(_ context.Context, _, venue string) error {
	f.unlinked = append(f.unlinked, venue)
	kept := f.links[:0]
	for _, l := range f.links {
		if l.Venue != venue {
			kept = append(kept, l)
		}
	}
	f.links = kept
	return nil
}

func (f *fakeMessengerStore) NotifyLevelsOf(context.Context, string) (map[string]string, error) {
	return f.levels, nil
}

func (f *fakeMessengerStore) SetNotifyLevel(_ context.Context, _, repository, level string) error {
	f.levels[repository] = level
	return nil
}

// The notifications panel (roadmap 145): the member's connections and
// the level per project, only the projects they read, with the derived
// default where they set none. Without the messengers the panel and its
// posts are not there.
func TestNotificationsPanel(t *testing.T) {
	r := newRig(t)
	members(r)
	fm := &fakeMessengerStore{levels: map[string]string{}}
	seat := func(m *Messengers) *Console {
		cfg := r.c.cfg
		cfg.Messengers = m
		c, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	get := func(c *Console, path string, ck *http.Cookie) string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if ck != nil {
			req.AddCookie(ck)
		}
		c.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %q", path, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	// without the messengers neither the panel nor its posts are there
	bk, bs := memberLogin(t, r, "bob")
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/notifications", nil), bk); rec.Code != http.StatusNotFound {
		t.Fatalf("the plain panel = %d", rec.Code)
	}
	for _, path := range []string{"/notifications/telegram/code", "/notifications/disconnect", "/notifications/level"} {
		if rec := r.do(post(path, url.Values{"csrf": {bs.csrf}}), bk); rec.Code != http.StatusNotFound {
			t.Fatalf("the plain %s = %d", path, rec.Code)
		}
	}

	// without a session the panel only leads to the sign-in page
	c := seat(&Messengers{Store: fm, TelegramName: "factorybot"})
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/notifications", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("the anonymous panel = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// bob reads demo only: the panel lists demo with its derived default
	// (demo's approvers) and no project he cannot read
	page := get(c, "/notifications", bk)
	if !strings.Contains(page, "demo") || !strings.Contains(page, "default: needs-you") {
		t.Fatalf("the panel misses demo's default:\n%s", page)
	}
	// the select rests on the effective level, so an untouched Save keeps
	// the derived default instead of silently turning it off
	if !strings.Contains(page, `value="needs-you" selected`) {
		t.Fatalf("the select does not rest on the derived default:\n%s", page)
	}
	if strings.Contains(page, "sf/code") || strings.Contains(page, "demo-app") {
		t.Fatalf("the panel lists a project bob cannot read:\n%s", page)
	}

	// a set level shows in place of the default
	fm.levels["demo/demo"] = factoryd.LevelAll
	if page = get(c, "/notifications", bk); !strings.Contains(page, `value="all" selected`) || strings.Contains(page, "default:") {
		t.Fatalf("the set level does not show:\n%s", page)
	}
	delete(fm.levels, "demo/demo")

	// the code post answers with the panel and the bot's start link
	send := func(c *Console, path string, form url.Values) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := post(path, form)
		req.AddCookie(bk)
		c.ServeHTTP(rec, req)
		return rec
	}
	rec = send(c, "/notifications/telegram/code", url.Values{"csrf": {bs.csrf}})
	code := strings.Repeat("c", 22)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/notifications?code="+code || fm.codes != 1 {
		t.Fatalf("the code post = %d %q, codes %d", rec.Code, rec.Header().Get("Location"), fm.codes)
	}
	if page = get(c, "/notifications?code="+code, bk); !strings.Contains(page, "https://t.me/factorybot?start="+code) {
		t.Fatalf("the panel misses the start link:\n%s", page)
	}

	// a connection shows and a disconnect takes it
	fm.links = []factoryd.MessengerLink{{Venue: factoryd.VenueTelegram, Address: "123", VenueUser: "42"}}
	if page = get(c, "/notifications", bk); !strings.Contains(page, "telegram") || !strings.Contains(page, "123") {
		t.Fatalf("the panel misses the link:\n%s", page)
	}
	rec = send(c, "/notifications/disconnect", url.Values{"csrf": {bs.csrf}, "venue": {factoryd.VenueTelegram}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/notifications" || len(fm.unlinked) != 1 {
		t.Fatalf("the disconnect = %d %q, unlinked %v", rec.Code, rec.Header().Get("Location"), fm.unlinked)
	}
	if page = get(c, "/notifications", bk); strings.Contains(page, "123") {
		t.Fatalf("the unlinked connection still shows:\n%s", page)
	}

	// the level post sets the level of a project the member reads and
	// refuses one they cannot: no level, no project, no wrong value
	rec = send(c, "/notifications/level", url.Values{"csrf": {bs.csrf}, "project": {"demo"}, "level": {factoryd.LevelAll}})
	if rec.Code != http.StatusSeeOther || fm.levels["demo/demo"] != factoryd.LevelAll {
		t.Fatalf("the level post = %d, level %q", rec.Code, fm.levels["demo/demo"])
	}
	rec = send(c, "/notifications/level", url.Values{"csrf": {bs.csrf},
		"project": {"ai-software-factory-code"}, "level": {factoryd.LevelAll}})
	if rec.Code != http.StatusNotFound || fm.levels["sf/code"] != "" {
		t.Fatalf("the level post for an unread project = %d, level %q", rec.Code, fm.levels["sf/code"])
	}
	for _, form := range []url.Values{
		{"csrf": {bs.csrf}, "project": {"demo"}, "level": {"daily"}},
		{"csrf": {bs.csrf}, "project": {"ghost"}, "level": {factoryd.LevelOff}},
	} {
		if rec = send(c, "/notifications/level", form); rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Fatalf("the refused level post %v = %d", form, rec.Code)
		}
	}

	// the instrument panel (C19): one row per venue the console carries or
	// the member connected, the state said in words beside its lamp; discord
	// not connected reads as a row with its connect link, connected as the
	// address and a Disconnect, and a connected telegram keeps its own row
	c = seat(&Messengers{Store: fm, TelegramName: "factorybot", Discord: &DiscordConnect{}})
	page = get(c, "/notifications", bk)
	for _, want := range []string{"telegram", "not connected", "discord", "/messengers/discord/start"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the panel of an unconnected member misses %q:\n%s", want, page)
		}
	}
	fm.links = []factoryd.MessengerLink{
		{Venue: factoryd.VenueTelegram, Address: "@sebastian", VenueUser: "42"},
		{Venue: factoryd.VenueDiscord, Address: "dexter"},
	}
	if page = get(c, "/notifications", bk); !strings.Contains(page, "@sebastian") || !strings.Contains(page, "dexter") {
		t.Fatalf("the panel misses a connected address:\n%s", page)
	}
	if strings.Count(page, "Disconnect") != 2 {
		t.Fatalf("the panel carries %d Disconnects, want one per connected venue:\n%s", strings.Count(page, "Disconnect"), page)
	}
	if strings.Contains(page, "not connected") {
		t.Fatalf("a connected venue still says not connected:\n%s", page)
	}

	// a link outlives its venue's config: the row stays, and with it the
	// console's only disconnect for that venue
	c = seat(&Messengers{Store: fm, TelegramName: "factorybot"})
	fm.links = []factoryd.MessengerLink{{Venue: factoryd.VenueDiscord, Address: "carl"}}
	if page = get(c, "/notifications", bk); !strings.Contains(page, "carl") || strings.Count(page, "Disconnect") != 1 {
		t.Fatalf("a link past its venue's config hides from the panel:\n%s", page)
	}
}
