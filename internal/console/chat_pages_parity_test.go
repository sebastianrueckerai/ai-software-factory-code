package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// routeParity is ruling 263 over routes: each GET route names a tool the
// agent is offered or an exception's reason, never both or neither.
func routeParity(c *Console, routes []getRoute) error {
	var errs []error
	for _, rt := range routes {
		_, offered := c.chatTool(rt.tool)
		switch {
		case rt.tool == "" && rt.none == "":
			errs = append(errs, fmt.Errorf("%s names neither a tool nor an exception", rt.pattern))
		case rt.tool != "" && rt.none != "":
			errs = append(errs, fmt.Errorf("%s names tool %s and an exception", rt.pattern, rt.tool))
		case rt.tool != "" && !offered:
			errs = append(errs, fmt.Errorf("%s names tool %s, which the agent is not offered", rt.pattern, rt.tool))
		}
	}
	return errors.Join(errs...)
}

// handBuilt is a route registered outside getRoutes and postRoutes.
var handBuilt = regexp.MustCompile(`mux\.Handle(Func)?\("(GET |POST )?/`)

// Every GET route comes from getRoutes and names a tool or an exception
// (ruling 263). Each tool answers alice, who writes the factory's code,
// and bob, who writes only demo, as its page does: the page shows and
// the tool reads, or the tool refuses in the page's words.
func TestEveryConsolePageIsAChatTool(t *testing.T) {
	r := newRig(t)
	members(r)
	routes, err := r.c.getRoutes()
	if err != nil {
		t.Fatal(err)
	}
	if err := routeParity(r.c, routes); err != nil {
		t.Fatal(err)
	}
	if err := r.c.checkAccess(); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := handBuilt.Find(b); m != nil && !strings.HasSuffix(f, "_test.go") {
			t.Errorf("%s registers %s by hand: a GET route belongs in getRoutes", f, m)
		}
	}
	args := map[string]toolInput{"costs": {By: "day"}}
	for _, m := range []struct{ user, project string }{{"alice", "ai-software-factory-code"}, {"bob", "demo"}} {
		ck, s := memberLogin(t, r, m.user)
		p, _, err := r.c.projectOf(context.Background(), m.project)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
		trn := &turn{c: r.c, r: req, a: actor{s: s, repository: p.Repository, via: factoryd.ViaChat}, p: p}
		for _, rt := range routes {
			if rt.tool == "" {
				continue
			}
			rec := r.do(httptest.NewRequest(http.MethodGet, strings.ReplaceAll(strings.TrimPrefix(rt.pattern, "GET "), "{$}", ""), nil), ck)
			tool, _ := r.c.chatTool(rt.tool)
			err := trn.admitTool(tool.name, args[rt.tool])
			if err == nil {
				_, _, err = tool.run(trn, args[rt.tool])
			}
			var no *refusal
			switch page := strings.TrimSpace(rec.Body.String()); {
			case rec.Code == http.StatusOK && err != nil:
				t.Errorf("%s shows %s the page, and %s refuses: %v", rt.pattern, m.user, rt.tool, err)
			case rec.Code != http.StatusOK && (!errors.As(err, &no) || no.status != rec.Code || no.msg != page):
				t.Errorf("%s answers %s %d %q, and %s %v", rt.pattern, m.user, rec.Code, page, rt.tool, err)
			}
		}
	}
}

// The parity rule fails a route added without a tool or an exception,
// and one whose tool the agent is not offered; the hand-built check
// catches a GET route registered beside the table.
func TestParityCatchesMissingRoute(t *testing.T) {
	r := newRig(t)
	routes, err := r.c.getRoutes()
	if err != nil {
		t.Fatal(err)
	}
	err = routeParity(r.c, append(routes, getRoute{pattern: "GET /escapee", handler: r.c.board},
		getRoute{pattern: "GET /ghost", handler: r.c.board, tool: "ghost"}))
	want := "GET /escapee names neither a tool nor an exception\nGET /ghost names tool ghost, which the agent is not offered"
	if err == nil || err.Error() != want {
		t.Fatalf("parity over the table and two bad routes = %v, want %q", err, want)
	}
	if !handBuilt.MatchString(`c.mux.Handle("GET /escapee", c.auth(c.board))`) ||
		!handBuilt.MatchString(`c.mux.Handle("POST /escapee", c.auth(c.board))`) {
		t.Fatal("a route registered by hand escapes the check")
	}
}

// pageFixture gives r two alerts (one judged), two commissions (a server
// with its firewall, a standing volume), two projects (one exported), a
// policy with two projects' caps and five ledger rows: t-m's small
// 0.5 and search 0.1 on 2026-10-10, t-r's big 1.2 and
// review/big 0.3 and t-m's search 0.2 on 2026-10-11.
func pageFixture(r *rig) {
	r.alerts.rows = []factoryd.Alert{
		{ID: 7, Kind: factoryd.AlertEscalation, Source: "cap", TaskID: "t-r", RaisedAt: time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)},
		{ID: 6, Kind: factoryd.AlertSupervisorTrip, Source: "detector", TaskID: "t-m", RaisedAt: time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC),
			Verdict: factoryd.VerdictNoise, ResolvedBy: "sebastian"},
	}
	r.resources.rows = []cloudres.Resource{
		{ID: 14, Kind: cloudres.KindFirewall, Repo: "sf/code", OwnerRef: "t-res", State: cloudres.StateLive, HoldID: 7,
			Lifetime: cloudres.LifetimeLease, LeaseExpires: time.Now().Add(time.Hour)},
		{ID: 11, Kind: cloudres.KindServer, Size: "cx23", Repo: "sf/code", OwnerRef: "t-res", State: cloudres.StateLive, HoldID: 7,
			Lifetime: cloudres.LifetimeLease, MonthlyEUR: 4.1616, LeaseExpires: time.Now().Add(time.Hour)},
		{ID: 12, Kind: cloudres.KindVolume, Repo: "demo-app/demo-app", OwnerRef: "t-vol", State: cloudres.StateLive, HoldID: 8,
			Lifetime: cloudres.LifetimeStanding, MonthlyEUR: 1},
	}
	r.projects.rows = []factoryd.Project{projectRows()[0], {Name: "ready-app", Repository: "ready/app", MergeRule: "auto",
		Objects: policy.ObjectsFactory, Agents: "coder", File: true, GitToken: "g", ReadyAt: time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)}}
	r.mirrors.rows["ready-app "+factoryd.MirrorExport] = factoryd.Mirror{Project: "ready-app", Direction: factoryd.MirrorExport,
		Remote: "acme/ready", Mode: factoryd.ModeMirror, State: "mirrored"}
	r.ledger.monthCaps = map[string]float64{"demo-app": 25, "infra": 10}
	r.ledger.files = map[string]policy.ProjectConfig{"linkshort": {Repository: "linkshort/linkshort", Approvers: []string{"alice"}}}
	r.ledger.risk = policy.Risk{Default: "normal",
		Classes: []policy.RiskClass{{Name: "high", Outcome: policy.OutcomeRequiresApproval, Migration: true}, {Name: "normal", Outcome: policy.OutcomeAuto}},
		Rules:   []policy.RiskRule{{Paths: []string{"internal/factoryd/migrations/**", "deploy/**"}, Class: "high"}}}
	r.ledger.costs = policy.Costs{From: "2026-10-01", To: "2026-10-11",
		Days: []policy.DayCost{
			{Day: "2026-10-10", ByKind: map[string]float64{policy.KindModel: 0.5, policy.KindTool: 0.1}, Total: 0.6},
			{Day: "2026-10-11", ByKind: map[string]float64{policy.KindModel: 1.5, policy.KindTool: 0.2}, Total: 1.7},
		},
		Tasks: []policy.TaskCost{
			{TaskID: "t-r", EUR: 1.5, Lines: []policy.CostLine{{Kind: policy.KindModel, Ref: "big", EUR: 1.2},
				{Kind: policy.KindModel, Ref: "review/big", EUR: 0.3}}},
			{TaskID: "t-m", EUR: 0.8, Lines: []policy.CostLine{{Kind: policy.KindModel, Ref: "small", EUR: 0.5},
				{Kind: policy.KindTool, Ref: "search", EUR: 0.3}}},
		}}
}

// Each page read over the fixture, as the agent reads it in a turn with
// no OK and no event row: the open and all alerts, the resources with
// the commission's actions on its server's row (promote only for an
// approver), every project, the policy's limits, caps, risk classes and
// approvers, and the month's ledger by day, card and tier.
func TestPageReadToolsOverFixtures(t *testing.T) {
	r, _, g := newChatRig(t)
	pageFixture(r)
	resources := "demo-app/demo-app:\n  12 volume live 1.0000 EUR/month, releasable\n" +
		"sf/code:\n  14 firewall live 0.0000 EUR/month\n  11 server live 4.1616 EUR/month, promotable, releasable"
	equal(t, "the reads", reads(t, r, g, "alerts", `{}`, "alerts", `{"state":"all"}`, "resources", `{}`, "settings", `{}`, "policy", `{}`,
		"costs", `{"by":"day"}`, "costs", `{"by":"card"}`, "costs", `{"by":"tier"}`), []string{
		"Open alerts:\n7 escalation cap \"t-r\" since 2026-10-11 09:30 UTC, open",
		"Alerts:\n7 escalation cap \"t-r\" since 2026-10-11 09:30 UTC, open\n6 supervisor trip detector \"t-m\" since 2026-10-10 08:00 UTC, verdict noise by sebastian",
		resources,
		"Projects (2):\n" +
			"ai-software-factory-code (sf/code): objects seed; process maintenance; merge rule human; made by the infra repository and the seed; GitHub export none\n" +
			"ready-app (ready/app): objects factory; process none; merge rule auto; ready since 2026-10-11 09:30 UTC; GitHub export acme/ready (mirror, mirrored)",
		"Policy version 4 (commit c9d98bb0aa11).\nLimits and budgets:\n" +
			"  task budget: €2.00 default, €5.00 maximum\n  sessions per card: 3\n  agent time: 20m0s\n  check-in: none\n" +
			"  supervisor calls per session: 3\n  supervisor timeout: 6m0s\n  returns before the stronger model: 1\n  steps per session: 200\n" +
			"  identical calls before a nudge: 3, nudges: 1\n  identical calls that trip the supervisor: 4\n" +
			"  day cap: €20.00\n  month cap: €50.00\n  project demo-app month cap: €25.00\n  project infra month cap: €10.00\n" +
			"Risk classes (most severe first, default normal):\n  high: requires_approval, migration\n  normal: auto\n" +
			"  internal/factoryd/migrations/**, deploy/** -> high\n" +
			"Approvers:\n  factory: sebastian\n  linkshort: alice",
		"By day 2026-10-01 to 2026-10-11, EUR:\n  2026-10-11 1.7000 (model 1.5000, tool 0.2000)\n  2026-10-10 0.6000 (model 0.5000, tool 0.1000)\ntotal: 2.3000 EUR",
		"By card 2026-10-01 to 2026-10-11, EUR:\n  t-r 1.5000\n  t-m 0.8000\ntotal: 2.3000 EUR",
		"By tier 2026-10-01 to 2026-10-11, EUR:\n  big 1.5000\n  small 0.5000\n  tool 0.3000\ntotal: 2.3000 EUR",
	})
	equal(t, "the ledger's window", []string{r.ledger.from.Format(time.DateOnly), r.ledger.to.Format(time.DateOnly)},
		[]string{time.Now().UTC().Format("2006-01") + "-01", time.Now().UTC().Format(time.DateOnly)})

	r.ledger.approvers = []string{"alice"}
	r.alerts.rows, r.resources.rows = nil, nil
	equal(t, "a writer's reads", reads(t, r, g, "alerts", `{"state":"all"}`, "resources", `{}`), []string{"No alerts.", "No commissioned resources."})
	pageFixture(r)
	equal(t, "a writer's resources", reads(t, r, g, "resources", `{}`)[0], strings.Replace(resources, ", promotable", "", 1))
}

// The open alerts as the agent reads them: a row that cleared on its own
// and waits for its verdict says when it cleared, a row whose resolved_at
// does not parse reads as open.
func TestAlertsTextNamesAClearedAlert(t *testing.T) {
	r, _, g := newChatRig(t)
	raised := time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)
	r.alerts.rows = []factoryd.Alert{
		{ID: 9, Kind: factoryd.AlertEscalation, Source: "cap", TaskID: "t-c", RaisedAt: raised, Detail: map[string]any{"resolved_at": "2026-10-20T15:12:30Z"}},
		{ID: 8, Kind: factoryd.AlertEscalation, Source: "cap", TaskID: "t-o", RaisedAt: raised},
		{ID: 7, Kind: factoryd.AlertEscalation, Source: "cap", TaskID: "t-b", RaisedAt: raised, Detail: map[string]any{"resolved_at": "not a time"}},
	}
	equal(t, "the open alerts", reads(t, r, g, "alerts", `{}`), []string{
		"Open alerts:\n" +
			"9 escalation cap \"t-c\" since 2026-10-20 14:00 UTC, cleared at 2026-10-20 15:12 UTC, verdict pending\n" +
			"8 escalation cap \"t-o\" since 2026-10-20 14:00 UTC, open\n" +
			"7 escalation cap \"t-b\" since 2026-10-20 14:00 UTC, open",
	})
}

// Each page write is a proposal naming what it does; its Do it calls the
// function the page's button calls, which the fake records, and leaves
// the button's event row, via chat.
func TestPageWriteToolsProduceProposals(t *testing.T) {
	r, _, g := newChatRig(t)
	pageFixture(r)
	for _, c := range []struct {
		tool, args, desc, result string
		row                      factoryd.Event
	}{
		{"alert_verdict", `{"id":7,"verdict":"real"}`, `Judge an alert, verdict real, alert 7 \"t-r\"`, "Alert 7 is judged real.",
			factoryd.Event{Repository: "sf/code", TaskID: "t-r", Kind: factoryd.EventAlertVerdict, Detail: map[string]any{"alert": 7, "verdict": "real"}}},
		{"promote_resource", `{"id":11}`, "Make a resource standing, server 11 of sf/code", "Resource 11 is standing now.",
			factoryd.Event{Repository: "sf/code", TaskID: "t-res", Kind: factoryd.EventResourcePromoted, Detail: map[string]any{"resource": 11, "kind": "server"}}},
		{"release_resource", `{"id":12}`, "Release a resource, volume 12 of demo-app/demo-app", "Resource 12 is released; the collector deletes it.",
			factoryd.Event{Repository: "demo-app/demo-app", TaskID: "t-vol", Kind: factoryd.EventResourceReleased, Detail: map[string]any{"resource": 12, "kind": "volume"}}},
	} {
		before := r.events.count()
		proposed, done, _ := proposeAndDo(t, r, g, c.tool, c.args)
		equal(t, c.tool+"'s proposal", proposed, []string{
			`proposal {"arguments":` + c.args + `,"description":"` + c.desc + `","id":"TOKEN","tool":"` + c.tool + `"}`, `done {}`})
		equal(t, c.tool+"'s Do it", done, ran(c.tool, c.args, c.result))
		chatRows(t, r.events, before, "sebastian", c.row)
	}
	equal(t, "the verdicts", r.alerts.verdicts, []string{"7 real sebastian"})
	equal(t, "the resource calls", r.resources.calls, []string{"promote 11 by sebastian under {0 [] 0} and {12 [cx23] 2}", "release 12 by sebastian"})
}

// noPolicy is the rig's ledger with the policy unreadable.
type noPolicy struct{ *fakeLedger }

func (noPolicy) CurrentPolicy(context.Context) (policy.Version, bool, error) {
	return policy.Version{}, false, errors.New("pq: connection refused")
}

// Bob writes demo only: each factory button and, from demo's chat, each
// page write is refused in the words the factory's pages answer him
// with, the buttons before any policy read, the chat at the proposal and,
// with no OK asked, at the run; nothing is judged, promoted or released.
func TestPageWritesRefuseNonWriters(t *testing.T) {
	r, _, g := newChatRig(t)
	pageFixture(r)
	members(r)
	g.project = "demo"
	ck, s := memberLogin(t, r, "bob")
	words := "Only the writers of the factory's own code see the factory's pages."
	for _, policies := range []Policies{r.ledger, noPolicy{r.ledger}} {
		r.c.cfg.Policies = policies
		for _, path := range []string{"/alerts/7/verdict", "/resources/11/promote", "/resources/11/release"} {
			page := r.do(post(path, url.Values{"csrf": {s.csrf}, "verdict": {"noise"}}), ck)
			equal(t, path, []any{page.Code, strings.TrimSpace(page.Body.String())}, []any{http.StatusForbidden, words})
		}
	}
	r.c.cfg.Policies = r.ledger
	for _, confirm := range [][]string{nil, {}} {
		if confirm != nil {
			r.ledger.chat = &policy.ChatConfig{Confirm: confirm}
		}
		for _, c := range [][2]string{{"alert_verdict", `{"id":7,"verdict":"noise"}`}, {"promote_resource", `{"id":11}`}, {"release_resource", `{"id":11}`}} {
			g.replies = [][]string{{call(1, c[0], c[1])}, {say("Refused.")}}
			_, events, _ := chatPost(t, r, ck, s, "/projects/demo/chat", url.Values{"message": {"do it"}})
			equal(t, c[0]+" by bob", events, []string{
				`tool {"arguments":` + c[1] + `,"duration_ms":0,"name":"` + c[0] + `","result":"Refused: ` + words + `"}`, `text {"text":"Refused."}`, `done {}`})
		}
	}
	equal(t, "the verdicts", r.alerts.verdicts, []string(nil))
	equal(t, "the resource calls", r.resources.calls, []string(nil))
}

// The costs read with a custom window: from and to as YYYY-MM-DD dates
// bound the ledger read it answers; an empty one keeps the current
// month's default; a malformed or reversed one is refused with 400.
func TestCostsToolWithCustomDateWindow(t *testing.T) {
	r, _, g := newChatRig(t)
	pageFixture(r)
	// fixture: t-r 1.5 (big 1.2, review/big 0.3)
	// and t-m 0.8 (small 0.5, tool search 0.3) on 2026-10-11,
	// t-m's small 0.5 and search 0.1 on 2026-10-10: that day's
	// card sum by hand is 1.5 + 0.8 = 2.3 EUR.
	equal(t, "the day's cards", reads(t, r, g, "costs", `{"by":"card","from_date":"2026-10-11","to_date":"2026-10-11"}`),
		[]string{"By card 2026-10-01 to 2026-10-11, EUR:\n  t-r 1.5000\n  t-m 0.8000\ntotal: 2.3000 EUR"})
	equal(t, "the store's window", []string{r.ledger.from.Format(time.DateOnly), r.ledger.to.Format(time.DateOnly)},
		[]string{"2026-10-11", "2026-10-11"})
}

// The costs read without from and to asks the current UTC month as
// before: the first day of this month to today.
func TestCostsToolDefaultsKept(t *testing.T) {
	r, _, g := newChatRig(t)
	pageFixture(r)
	equal(t, "the month's days", reads(t, r, g, "costs", `{"by":"day"}`),
		[]string{"By day 2026-10-01 to 2026-10-11, EUR:\n  2026-10-11 1.7000 (model 1.5000, tool 0.2000)\n  2026-10-10 0.6000 (model 0.5000, tool 0.1000)\ntotal: 2.3000 EUR"})
	equal(t, "the store's window", []string{r.ledger.from.Format(time.DateOnly), r.ledger.to.Format(time.DateOnly)},
		[]string{time.Now().UTC().Format("2006-01") + "-01", time.Now().UTC().Format(time.DateOnly)})
}

// A malformed or reversed costs date window is refused with 400 in one
// sentence; the store is not asked.
func TestCostsToolInvalidDateRefused(t *testing.T) {
	r, _, _ := newChatRig(t)
	pageFixture(r)
	for _, in := range []toolInput{
		{By: "day", FromDate: "2026-10-11", ToDate: "last tuesday"},
		{By: "card", FromDate: "20261011"},
		{By: "tier", FromDate: "2026-02-30"},
		{By: "day", FromDate: "2026-10-11", ToDate: "2026-10-10"},
	} {
		if status, msg := refused(t, r, "costs", in); status != http.StatusBadRequest ||
			msg != "from and to must be dates YYYY-MM-DD, from not after to" {
			t.Errorf("costs %+v = %d %q", in, status, msg)
		}
	}
	if !r.ledger.from.IsZero() {
		t.Errorf("the store was asked for %+v to %+v", r.ledger.from, r.ledger.to)
	}
}

// The /costs page reads its window from query params from and to, both
// included; a malformed one answers 400; without them the last 14 days
// with today stand.
func TestCostsPageDateParams(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	pageFixture(r)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/costs?from=2026-10-10&to=2026-10-11", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("/costs?from&to = %d: %s", rec.Code, rec.Body)
	}
	equal(t, "the store's window", []string{r.ledger.from.Format(time.DateOnly), r.ledger.to.Format(time.DateOnly)},
		[]string{"2026-10-10", "2026-10-11"})
	body := rec.Body.String()
	for _, want := range []string{"Per day, 2026-10-01 to 2026-10-11", "<th scope=\"row\">2026-10-10</th>"} {
		if !strings.Contains(body, want) {
			t.Errorf("/costs lacks %q", want)
		}
	}
	rec = r.do(httptest.NewRequest(http.MethodGet, "/costs?from=10-10-2026", nil), ck)
	if rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != "from: want YYYY-MM-DD" {
		t.Errorf("/costs?from=10-10-2026 = %d %q", rec.Code, rec.Body)
	}
	rec = r.do(httptest.NewRequest(http.MethodGet, "/costs?from=2026-10-11&to=2026-10-10", nil), ck)
	if rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != "from is after to" {
		t.Errorf("/costs reversed = %d %q", rec.Code, rec.Body)
	}
	rec = r.do(httptest.NewRequest(http.MethodGet, "/costs", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("/costs = %d: %s", rec.Code, rec.Body)
	}
	if got := r.ledger.to.Sub(r.ledger.from); got != 13*24*time.Hour {
		t.Errorf("default range %v to %v: want 14 days, today included", r.ledger.from, r.ledger.to)
	}
}
