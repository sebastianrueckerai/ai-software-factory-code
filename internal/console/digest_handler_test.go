package console

import (
	"context"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The fold of a card's life answers a writer of the card's project with
// the card's digest as a human reads it, a line per step with a count for
// the finding ids and a writer's words apart, then each open case under
// its ask, with who weighed it and what came of it in plain words, oldest
// first, and refuses anyone else with 404.
func TestCardLifeFoldReadsTheDigestAndTheOpenCases(t *testing.T) {
	r := newRig(t)
	members(r)
	at := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	r.c.cfg.CardDigest = func(context.Context, string) (factoryd.CardDigest, error) {
		return factoryd.CardDigest{Steps: []factoryd.DigestStep{
			{N: 1, At: at, Kind: "filed", Actor: factoryd.SourceWebhook, Outcome: "direct card", Count: 1},
			{N: 2, At: at.Add(time.Hour), Kind: "review", Outcome: "concerns, findings 4285c299662c d095c64ba7a3", Findings: []string{"4285c299662c", "d095c64ba7a3"}, Count: 1},
			{N: 3, At: at.Add(2 * time.Hour), Kind: "handover", Outcome: "Fix the vet finding first.", Count: 1},
		}}, nil
	}
	r.openCases = map[string][]factoryd.Case{"t-c": {
		{ID: 4, TaskID: "t-c", Kind: factoryd.CaseGateStop},
		{ID: 9, TaskID: "t-c", Kind: factoryd.CaseSessionCap},
		{ID: 11, TaskID: "t-c", Kind: factoryd.CaseBudgetFloor},
	}}
	r.caseSteps = map[int64][]factoryd.CaseStep{
		4: {{Rung: 1, Actor: factoryd.CheckinDecider, Outcome: factoryd.StepHandedUp, Found: "the review & the tests disagree"}},
		9: {{Rung: 2, Actor: factoryd.ChatUser, Outcome: factoryd.StepHandedUp}, {Rung: 3, Actor: "sebastian", Outcome: factoryd.StepInProgress}},
	}
	ak, _ := memberLogin(t, r, "alice")
	rec := getPage(r, ak, "/tasks/t-c/digest")
	want := `<ol class="life-log"><li><time>18 Nov 10:00</time> Filed by webhook: direct card.</li><li><time>18 Nov 11:00</time> The model review: concerns, 2 findings.</li><li><time>18 Nov 12:00</time> Handed over: <span class="quote">Fix the vet finding first.</span></li></ol>
<h4>The open ask: A step of the work ran past its time limit. Check the work again, or tell the card how to go on?</h4><ul><li>The check-in passed it to the factory agent, and found: <span class="quote">the review &amp; the tests disagree</span></li></ul>
<h4>The open ask: The card used all 0 sessions. Allow more, or end the card?</h4><ul><li>The factory agent passed it to a human.</li><li>sebastian is weighing it.</li></ul>
<h4>The open ask: The card is out of money, and adding 0.00 EUR lets it run again. How should it go on?</h4><p class="meta">Nobody has weighed this ask yet.</p>
`
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("the fold for alice = %d\n%s\nwant 200\n%s", rec.Code, rec.Body, want)
	}
	bk, _ := memberLogin(t, r, "bob")
	// the fold follows the board's sight: a member who cannot read the project has no card there
	if rec := getPage(r, bk, "/tasks/t-c/digest"); rec.Code != http.StatusNotFound {
		t.Fatalf("the fold for bob, who writes only demo = %d, want 404:\n%s", rec.Code, rec.Body)
	}
	// a reader of the project, who sees the board and its fold, loads it
	rk, _ := r.readerOfCode(t, "anna")
	if rec := getPage(r, rk, "/tasks/t-c/digest"); rec.Code != http.StatusOK {
		t.Fatalf("the fold for anna, who reads the project = %d, want 200:\n%s", rec.Code, rec.Body)
	}
	if rec := getPage(r, ak, "/tasks/t-none/digest"); rec.Code != http.StatusNotFound {
		t.Fatalf("the fold of no card = %d, want 404", rec.Code)
	}
}

// The drawer folds the card's life and asks for it the first time the
// human opens the fold: neither the board nor the drawer reads the digest.
func TestDrawerFoldsTheCardsLifeLazily(t *testing.T) {
	r := newRig(t)
	members(r)
	reads := 0
	read := r.c.cfg.CardDigest
	r.c.cfg.CardDigest = func(ctx context.Context, taskID string) (factoryd.CardDigest, error) {
		reads++
		return read(ctx, taskID)
	}
	ak, _ := memberLogin(t, r, "alice")
	if rec := getPage(r, ak, "/"); rec.Code != http.StatusOK {
		t.Fatalf("the board = %d", rec.Code)
	}
	drawer := getPage(r, ak, "/cards/t-c/drawer?tab=overview").Body.String()
	for _, want := range []string{`<details class="fold" data-sec="life" hx-get="/tasks/t-c/digest" hx-trigger="toggle once" hx-target="find .sec-body"><summary>The card's life`,
		`<div class="sec-body"><p class="meta">Loading…</p></div></details>`} {
		if !strings.Contains(drawer, want) {
			t.Fatalf("the drawer lacks the fold\n%s\nin\n%s", want, drawer)
		}
	}
	if strings.Contains(drawer, "revealed") {
		t.Fatal("the drawer loads something once it is in view")
	}
	if reads != 0 {
		t.Fatalf("the board and the drawer read the digest %d times, want none", reads)
	}
}

// A fold of the card's life whose read failed, refused or never reached
// the factory, says so in place of its loading line; another fold's
// failure leaves it alone. console.js under node, which the test needs.
func TestCardLifeFoldSaysWhenItFails(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var listeners = {};
function el(props) {
  return Object.assign({dataset: {}, textContent: "", querySelector: function () { return null; }, querySelectorAll: function () { return []; },
    addEventListener: function () {}, getAttribute: function () { return null; }}, props);
}
var sandbox = {URL: URL, URLSearchParams: URLSearchParams, CSS: {escape: function (s) { return s; }},
  location: {href: "http://lab/", pathname: "/", search: "", hash: ""}, history: {state: null, replaceState: function () {}, pushState: function () {}},
  localStorage: {getItem: function () { return null; }, setItem: function () {}},
  setTimeout: function () {}, setInterval: function () {}, clearTimeout: function () {},
  Event: function (type) { this.type = type; }, CustomEvent: function (type, init) { this.type = type; this.detail = init && init.detail; },
  htmx: {ajax: function () { return new Promise(function () {}); }, swap: function () {}, process: function () {}},
  window: {addEventListener: function () {}},
  document: {body: {classList: {toggle: function () {}}}, documentElement: {getAttribute: function () { return "dark"; }},
    getElementById: function () { return null; }, querySelectorAll: function () { return []; }, querySelector: function () { return null; },
    removeEventListener: function () {}, createElement: function () { return el(); },
    addEventListener: function (type, fn) { (listeners[type] = listeners[type] || []).push(fn); }}};
require("vm").runInNewContext(require("fs").readFileSync(process.argv[1], "utf8"), sandbox);
(listeners.DOMContentLoaded || []).forEach(function (fn) { fn(); });
function fold(sec) {
  var line = el({textContent: "Loading…"});
  return {line: line, elt: el({dataset: {sec: sec}}), target: el({querySelector: function (sel) { return sel === "p" ? line : null; }})};
}
function fire(type, f, status) {
  var detail = {elt: f.elt, target: f.target, requestConfig: {verb: "get"},
    xhr: {status: status, responseText: "cannot read the card's life\n", getResponseHeader: function () { return "default-src 'self'"; }}};
  (listeners[type] || []).forEach(function (fn) { fn({type: type, target: f.elt, detail: detail}); });
}
var refused = fold("life"), unsent = fold("life"), other = fold("time");
fire("htmx:responseError", refused, 500);
fire("htmx:sendError", unsent, 0);
fire("htmx:responseError", other, 500);
console.log(JSON.stringify([refused.line.textContent, unsent.line.textContent, other.line.textContent]));`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver, "static/console.js")
	got, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	want := `["The card's life could not be read: the read failed. Open the card again, or read the card on its issue.","The card's life could not be read: the factory did not answer. Open the card again, or read the card on its issue.","Loading…"]`
	if strings.TrimSpace(string(got)) != want {
		t.Errorf("console.js under node:\n%s\nwant\n%s", got, want)
	}
}

// A link to #card=<id>:life opens the fold once the drawer has settled:
// htmx wires the swapped nodes there, after the swap's promise, and a fold
// opened before it fires its toggle to no listener and stays on its
// loading line. Only the settle of the link's own request on the drawer
// opens it, once: a settle of another path, from another element or on
// another target is passed over, and the listener leaves after its
// settle. console.js under node, which the test needs.
func TestCardLifeLinkOpensTheFoldOnceTheDrawerSettles(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var listeners = {}, focused = [], read = false, opens = 0, asked = [];
function el(props) {
  return Object.assign({dataset: {}, textContent: "", querySelector: function () { return null; }, querySelectorAll: function () { return []; },
    addEventListener: function () {}, setAttribute: function () {}, getAttribute: function () { return null; }}, props);
}
var summary = el({focus: function () { focused.push("summary"); }}), h2 = el({focus: function () { focused.push("h2"); }}),
  life = el({wired: false, querySelector: function () { return summary; }}),
  drawer = el({id: "drawer", hidden: true, querySelector: function (sel) { return sel === 'details[data-sec="life"]' ? life : sel === "h2" ? h2 : null; }});
Object.defineProperty(life, "open", {set: function (v) { opens++; if (v && life.wired) read = true; }});
var sandbox = {URL: URL, URLSearchParams: URLSearchParams, CSS: {escape: function (s) { return s; }},
  location: {href: "http://lab/#card=t-9:life", pathname: "/", search: "", hash: "#card=t-9:life"}, history: {state: null, replaceState: function () {}, pushState: function () {}},
  localStorage: {getItem: function () { return null; }, setItem: function () {}},
  setTimeout: function () {}, setInterval: function () {}, clearTimeout: function () {},
  Event: function (type) { this.type = type; }, CustomEvent: function (type, init) { this.type = type; this.detail = init && init.detail; },
  htmx: {ajax: function (verb, url) { asked.push(url); return Promise.resolve(); }, swap: function () {}, process: function () {}},
  window: {addEventListener: function () {}},
  document: {body: {classList: {toggle: function () {}}}, documentElement: {getAttribute: function () { return "dark"; }},
    getElementById: function (id) { return id === "drawer" ? drawer : null; }, querySelectorAll: function () { return []; }, querySelector: function () { return null; },
    removeEventListener: function (type, fn) { listeners[type] = (listeners[type] || []).filter(function (f) { return f !== fn; }); },
    createElement: function () { return el(); },
    addEventListener: function (type, fn) { (listeners[type] = listeners[type] || []).push(fn); }}};
require("vm").runInNewContext(require("fs").readFileSync(process.argv[1], "utf8"), sandbox);
(listeners.DOMContentLoaded || []).forEach(function (fn) { fn(); });
function settle(path, elt, target) {
  target = target || drawer;
  (listeners["htmx:afterSettle"] || []).slice().forEach(function (fn) {
    fn({type: "htmx:afterSettle", target: target, detail: {target: target, requestConfig: {path: path, elt: elt}}});
  });
}
setTimeout(function () {
  life.wired = true;
  settle("/cards/t-8/drawer?tab=overview", sandbox.document.body);
  settle(asked[0], el());
  settle(asked[0], sandbox.document.body, el());
  var passed = [opens, focused.slice()];
  settle(asked[0], sandbox.document.body);
  settle(asked[0], sandbox.document.body);
  console.log(JSON.stringify([asked, passed, read, opens, focused]));
}, 20);`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver, "static/console.js")
	got, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	if want := `[["/cards/t-9/drawer?tab=overview"],[0,[]],true,1,["summary"]]`; strings.TrimSpace(string(got)) != want {
		t.Errorf("console.js under node:\n%s\nwant\n%s", got, want)
	}
}
