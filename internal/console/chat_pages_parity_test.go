package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// Ruling 261, roadmap 158: every console page is a chat tool. These tests
// hold that three ways: no GET route New registers escapes the table, each
// factory-level page and its tool answer the same member alike down to the
// refusal's words, and every tool the table names exists, is offered and
// reads without an OK where it reads.

// pageCase is one GET route of the table with the handler New registers
// for it and the read its tool makes; a nil tool is a named exception.
type pageCase struct {
	pattern string
	handler http.HandlerFunc
	tool    func(t *turn) (string, error)
	none    string
	facts   []string // what its page shows and its tool must say too
}

func TestEveryConsolePageIsAChatTool(t *testing.T) {
	r := newRig(t)
	r.alerts.rows = []factoryd.Alert{
		{ID: 7, Kind: factoryd.AlertEscalation, Source: "cap", TaskID: "t-r", RaisedAt: time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)},
		{ID: 6, Kind: factoryd.AlertSupervisorTrip, Source: "detector", TaskID: "t-m", RaisedAt: time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC),
			Verdict: factoryd.VerdictNoise, ResolvedBy: "sebastian"},
	}
	r.resources.rows = []cloudres.Resource{{ID: 11, Kind: cloudres.KindServer, Size: "cx23", Repo: "sf/code", OwnerRef: "t-res",
		State: cloudres.StateLive, Project: "agent", HoldID: 7, Lifetime: cloudres.LifetimeLease, MonthlyEUR: 4.1616,
		PricePerHourEUR: 0.0072, LeaseExpires: time.Now().Add(time.Hour)}}
	today := time.Now().UTC().Format(policy.DateLayout)
	r.ledger.costs = policy.Costs{From: today, To: today,
		Days: []policy.DayCost{{Day: today, ByKind: map[string]float64{policy.KindModel: 1.5}, Total: 1.5}},
		Tasks: []policy.TaskCost{{TaskID: "t-r", EUR: 1.5}}}
	r.projects.rows = append(slices.Clone(r.projects.rows), factoryd.Project{Name: "ready-app", Repository: "ready/app",
		MergeRule: "auto", Objects: policy.ObjectsFactory, Agents: "coder", File: true, GitToken: "g",
		ReadyAt: time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)})
	r.mirrors.rows["ready-app "+factoryd.MirrorExport] = factoryd.Mirror{Project: "ready-app", Direction: factoryd.MirrorExport,
		Remote: "acme/ready", Mode: factoryd.ModeMirror, State: "mirrored"}
	c := r.c
	h := func(f func(http.ResponseWriter, *http.Request)) http.HandlerFunc { return f }
	cases := []pageCase{
		{"GET /login", c.login, nil, "the sign-in page: no chat before a session", nil},
		{"GET /auth/callback", c.callback, nil, "the OAuth provider's callback: no page", nil},
		{"GET /{$}", h(c.board), nil, "", nil},
		{"GET /board", h(c.board), nil, "", nil},
		{"GET /tasks/new", h(c.factory(c.newTaskForm)), nil, "the new task's form: the chat files cards (new_card), not tasks", nil},
		{"GET /tasks/{id}/timeline", h(c.timeline), nil, "a card's timeline fragment: the card read names where its reviews, checks and plan are", nil},
		{"GET /costs", h(c.factory(c.costs)), func(t *turn) (string, error) { return t.c.costsByText(t.r.Context(), "day") }, "",
			[]string{today, "1.5000", policy.KindModel}},
		{"GET /policy", h(c.factory(c.policy)), func(t *turn) (string, error) { return t.c.policyText(t.r.Context(), t.a, t.p.Repository) }, "",
			[]string{"Policy version 4", "c9d98bb0aa11", "€2.00 default", "€5.00 maximum", "sebastian"}},
		{"GET /resources", h(c.factory(c.resources)), func(t *turn) (string, error) { return t.c.resourcesText(t.r.Context(), t.a) }, "",
			[]string{"11", "server", "live", "4.1616", "promotable", "releasable", "sf/code"}},
		{"GET /alerts", h(c.factory(c.alerts)), func(t *turn) (string, error) { return t.c.alertsText(t.r.Context(), t.a, "open") }, "",
			[]string{"7", "escalation cap", "t-r", "2026-10-11 09:30", "open"}},
		{"GET /projects", h(c.factory(c.projects)), func(t *turn) (string, error) { return t.c.projectsText(t.r.Context(), t.a, t.p) }, "",
			[]string{"ai-software-factory-code", "sf/code", "seed", "human", "demo-app", "auto", "factory",
				"ready-app", "ready since 2026-10-11 09:30 UTC", "acme/ready (mirror, mirrored)", "no file: kept for its tokens"}},
		{"GET /projects/new", h(c.factory(c.newProjectForm)), nil, "the new project's form: the chat creates no project", nil},
		{"GET /projects/{name}/status", h(c.factory(c.projectStatus)), nil, "a project's status panel: a fragment of the projects page whose tool exists", nil},
		{"GET /projects/{name}/events", h(c.projectEvents), func(t *turn) (string, error) {
			events, err := t.c.cfg.Events.ProjectEvents(t.r.Context(), t.p.Repository, 0, eventLimitDefault)
			return eventLines(events), err
		}, "", nil},
		{"GET /projects/{name}/events/stream", h(c.projectEventsStream), nil, "the event stream: the live half of the events fragment", nil},
		{"GET /cards/{id}", h(c.cardFragment), nil, "a card's face: a fragment of the board page whose tool exists", nil},
		{"GET /cards/{id}/drawer", h(c.cardDrawer), nil, "a card's drawer: a fragment of the board page whose tool exists", nil},
		{"GET /projects/{name}/chat", h(c.chatGet), nil, "the chat's own page", nil},
		{"GET /projects/{name}/chat/conversations", h(c.chatConversations), nil, "the chat's conversation list: the chat's own route", nil},
		{"GET /projects/{name}/pulse", h(c.pulseGet), func(t *turn) (string, error) { return t.c.pulseText(t.r.Context(), t.a, t.p) }, "", nil},
		{"GET /static/", http.NotFound, nil, "the static files", nil},
	}

	// The table and the parity cases name the routes alike: a new GET
	// route in New must join both, and none may go without a tool or a
	// named exception.
	if len(cases) != len(chatRoutes) {
		t.Errorf("the parity table holds %d routes, chatRoutes %d: a route joined one and not the other", len(cases), len(chatRoutes))
	}
	table := slices.Clone(chatRoutes)
	for _, tc := range cases {
		i := slices.IndexFunc(table, func(row chatRouteTool) bool { return row.pattern == tc.pattern })
		if i < 0 {
			t.Errorf("%s carries no chatRoutes row: every GET route names a tool or an exception", tc.pattern)
			continue
		}
		row := table[i]
		table = slices.Delete(table, i, i+1)
		switch {
		case row.tool == "" && row.none == "":
			t.Errorf("%s names neither a tool nor an exception", tc.pattern)
		case (row.tool == "") != (tc.tool == nil):
			t.Errorf("%s: chatRoutes gives it %q, the parity table %v: one of them lies about its tool", tc.pattern, row.none, tc.tool != nil)
		case row.none != tc.none:
			t.Errorf("%s's exception reads %q in chatRoutes and %q here: name it once", tc.pattern, row.none, tc.none)
		}
	}
	for _, row := range table {
		t.Errorf("chatRoutes names %s, which the parity table does not serve", row.pattern)
	}

	// Each page and its tool answer the same member alike: sebastian, who
	// writes the factory's code, sees the page's facts in the tool, and
	// bob, who writes none, reads the page's 403 in the tool's refusal,
	// word for word.
	ck, s := r.loggedIn(t)
	bob, sb := memberLogin(t, r, "bob")
	for _, tc := range cases {
		if tc.tool == nil {
			continue
		}
		for _, who := range []struct {
			name string
			ck   *http.Cookie
			s    *session
		}{{"sebastian", ck, s}, {"bob", bob, sb}} {
			req := pageRequest(tc.pattern, who.ck, who.s)
			rec := httptest.NewRecorder()
			c.ServeHTTP(rec, req)
			page := strings.TrimSpace(rec.Body.String())
			got, err := tc.tool(pageTurn(c, req, who.s, "sf/code"))
			switch rec.Code {
			case http.StatusOK:
				if err != nil {
					t.Errorf("%s's tool for %s refused what its page showed (%d): %v", tc.pattern, who.name, rec.Code, err)
					continue
				}
				for _, want := range tc.facts {
					if !strings.Contains(got, want) {
						t.Errorf("%s's tool for %s lacks %q:\ntool: %s\npage: %s", tc.pattern, who.name, want, got, trimBody(page))
					}
				}
			default:
				var no *refusal
				if !errors.As(err, &no) || no.status != rec.Code || no.msg != page {
					t.Errorf("%s for %s = %d %q, the tool says %v: the tool must refuse as the page does",
						tc.pattern, who.name, rec.Code, page, err)
				}
			}
		}
	}
}

// pageTurn is the turn a page's tool read runs on: the member of s as the
// actor of the factory's pages, on the project whose repository is own.
func pageTurn(c *Console, r *http.Request, s *session, own string) *turn {
	return &turn{c: c, r: r, a: actor{s: s, repository: own, via: factoryd.ViaChat},
		p: factoryd.Project{Name: "ai-software-factory-code", Repository: own}}
}

// pageURL is a GET pattern as a request URL: {name} takes the project the
// rig files, {id} the card its tasks hold, {$} the board's own path.
func pageURL(pattern, name string) string {
	u := strings.TrimPrefix(pattern, "GET ")
	u = strings.ReplaceAll(u, "{$}", "/")
	u = strings.ReplaceAll(u, "{name}", name)
	u = strings.ReplaceAll(u, "{id}", "t-r")
	if u == "/static/" {
		u = "/static/layout.css"
	}
	return u
}

// pageRequest is a GET of the pattern's URL as the mux would hand it to
// the handler: the login in its context and its path values set.
func pageRequest(pattern string, ck *http.Cookie, s *session) *http.Request {
	req := httptest.NewRequest(http.MethodGet, pageURL(pattern, "ai-software-factory-code"), nil)
	req.AddCookie(ck)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
	if strings.Contains(pattern, "{name}") {
		req.SetPathValue("name", "ai-software-factory-code")
	}
	if strings.Contains(pattern, "{id}") {
		req.SetPathValue("id", "t-r")
	}
	return req
}

// trimBody is a page's body run together for a failure's message.
func trimBody(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}

// TestPageToolsAreOffered holds the tools ruling 261 names: each exists,
// reads without an OK, and each write of the pages is confirmed by default.
func TestPageToolsAreOffered(t *testing.T) {
	c := &Console{}
	for _, tc := range []struct{ tool, title string }{{"alerts", "Read the alerts"}, {"resources", "Read the resources"},
		{"projects", "Read the projects"}, {"policy", "Read the policy"}, {"pulse", "Read the pulse"},
		{"costs", "Read the costs"}, {"board", "Read the board"}, {"card", "Read a card"}, {"history", "Read the history"}} {
		tool, ok := c.chatTool(tc.tool)
		if !ok || tool.title != tc.title {
			t.Errorf("the agent is offered no tool %q titled %q", tc.tool, tc.title)
			continue
		}
		if tool.write {
			t.Errorf("%s is a read behind an OK", tc.tool)
		}
		if !slices.Contains(policy.ChatTools, tc.tool) {
			t.Errorf("%s is no tool of the policy", tc.tool)
		}
		if slices.Contains(policy.DefaultChat.Confirm, tc.tool) {
			t.Errorf("%s needs an OK: a read never does", tc.tool)
		}
	}
	for _, w := range []string{"alert_verdict", "promote_resource", "release_resource"} {
		tool, ok := c.chatTool(w)
		if !ok || !tool.write {
			t.Errorf("%s is no write tool", w)
			continue
		}
		if !slices.Contains(policy.DefaultChat.Confirm, w) {
			t.Errorf("%s runs unconfirmed: a write needs the member's OK by default", w)
		}
	}
	for _, row := range chatRoutes {
		if row.tool != "" && !slices.Contains(policy.ChatTools, row.tool) {
			t.Errorf("%s names %q, which the policy does not know", row.pattern, row.tool)
		}
	}
}

// jsonAnswer is a page's JSON body decoded, for a fact's comparison.
func jsonAnswer(body string) any {
	var v any
	if json.Unmarshal([]byte(body), &v) != nil {
		return nil
	}
	return v
}
