package console

import (
	"context"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The fold of a card's life answers a writer of the card's project with
// the card's digest and then each open case under its kind, with its
// steps in plain words, oldest first, and refuses anyone else with 403.
func TestCardLifeFoldReadsTheDigestAndTheOpenCases(t *testing.T) {
	r := newRig(t)
	members(r)
	r.openCases = map[string][]factoryd.Case{"t-c": {
		{ID: 4, TaskID: "t-c", Kind: factoryd.CaseGateStop},
		{ID: 9, TaskID: "t-c", Kind: factoryd.CaseSessionCap},
		{ID: 11, TaskID: "t-c", Kind: factoryd.CaseBudgetFloor},
	}}
	r.caseSteps = map[int64][]factoryd.CaseStep{
		4: {{Rung: 1, Actor: "checkin", Outcome: factoryd.StepHandedUp, Found: "the review & the tests disagree"}},
		9: {{Rung: 2, Actor: "factory agent", Outcome: factoryd.StepHandedUp}, {Rung: 3, Actor: "sebastian", Outcome: factoryd.StepInProgress}},
	}
	ak, _ := memberLogin(t, r, "alice")
	rec := getPage(r, ak, "/tasks/t-c/digest")
	want := `<pre class="digest">#5 · budget 0.00 spent 0.00 holds 0.00 EUR · queued
flags: none
</pre>
<h4>Open case: gate stop</h4><ul><li>rung 1 by checkin → handed up: the review &amp; the tests disagree</li></ul>
<h4>Open case: session cap</h4><ul><li>rung 2 by factory agent → handed up</li><li>rung 3 by sebastian → in progress</li></ul>
<h4>Open case: budget floor</h4><p class="meta">No step yet.</p>
`
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("the fold for alice = %d\n%s\nwant 200\n%s", rec.Code, rec.Body, want)
	}
	bk, _ := memberLogin(t, r, "bob")
	if rec := getPage(r, bk, "/tasks/t-c/digest"); rec.Code != http.StatusForbidden {
		t.Fatalf("the fold for bob, who writes only demo = %d, want 403:\n%s", rec.Code, rec.Body)
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
	if want := `<details class="sec" data-sec="life" hx-get="/tasks/t-c/digest" hx-trigger="toggle once" hx-target="find .sec-body">` +
		`<summary>The card's life</summary><div class="sec-body"><p class="meta">Loading…</p></div></details>`; !strings.Contains(drawer, want) {
		t.Fatalf("the drawer lacks the fold\n%s\nin\n%s", want, drawer)
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
	failed := `"The card's life could not be read; see the factoryd log."`
	if want := "[" + failed + "," + failed + `,"Loading…"]`; strings.TrimSpace(string(got)) != want {
		t.Errorf("console.js under node:\n%s\nwant\n%s", got, want)
	}
}
