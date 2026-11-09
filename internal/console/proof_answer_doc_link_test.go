package console

import (
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// TestAnswerDocPathRendersLink proves answer_path_renders_doc_link of the
// show-doc card (roadmap 280): the chat's markdown renders a document
// path as an anchor of class docref whose data-doc names the path — a
// bare path in prose, a linked label, and a .md target without the
// docs/ prefix resolved against the document root — while a code span
// and an outside link stay plain; and the console opens the member's
// document view on a view event that carries a document and on the
// press of a rendered docref, fetching the document under the project.
func TestAnswerDocPathRendersLink(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var vm = require("vm"), fs = require("fs"), out = {};
function el(props) {
  var e = {dataset: {}, hidden: false, textContent: "", innerHTML: "", children: [], attrs: {}, className: "", value: "", disabled: false,
    title: "", scrollTop: 0, matchers: [], elListeners: [], parentNode: null, firstChild: null,
    querySelector: function () { return null; }, querySelectorAll: function () { return []; }, contains: function () { return false; },
    getAttribute: function (k) { return k in this.attrs ? this.attrs[k] : null; }, setAttribute: function (k, v) { this.attrs[k] = String(v); },
    addEventListener: function (t, f) { this.elListeners.push([t, f]); }, focus: function () {},
    appendChild: function (c) { this.children.push(c); c.parentNode = this; this.firstChild = this.children[0]; return c; },
    matches: function (sel) { return this.matchers.some(function (m) { return m.test(sel); }); },
    closest: function (sel) { var n = this; while (n) { if (n.matches && n.matches(sel)) return n; n = n.parentNode; } return null; }};
  e.classList = {contains: function (c) { return (" " + e.className + " ").indexOf(" " + c + " ") >= 0; },
    add: function (c) { if (!e.classList.contains(c)) e.className = e.className ? e.className + " " + c : c; },
    remove: function (c) { e.className = (" " + e.className + " ").split(" " + c + " ").join(" ").trim(); },
    toggle: function (c, on) { var has = e.classList.contains(c); if (on === undefined) on = !has;
      if (on && !has) e.classList.add(c); if (!on && has) e.classList.remove(c); return on; }};
  return Object.assign(e, props);
}
function store() { var m = {}; return {getItem: function (k) { return k in m ? m[k] : null; },
  setItem: function (k, v) { m[k] = String(v); }, removeItem: function (k) { delete m[k]; }}; }
var listeners = {}, winListeners = {}, requests = [];
var body = el(); body.matchers = [/^body$/];
var chatLog = el({id: "chat-log"}); chatLog.matchers = [/chat-log/];
body.appendChild(chatLog);
var anchor = el({className: "docref", textContent: "docs/roadmap.md"});
anchor.dataset = {doc: "docs/roadmap.md"};
anchor.matchers = [/docref/, /data-doc/, /^a$/, /\[href\]/];
anchor.getAttribute = function (k) { return k === "href" ? "?doc=docs/roadmap.md" : null; };
chatLog.appendChild(anchor);
var byID = {board: el({dataset: {project: "code", query: "?project=code"}}), "status-line": el(), live: el(), drawer: el({hidden: true}),
  "session-log": el(), "steer-input": el({value: ""}), "doc-view": el({hidden: true}), "doc-article": el(), "doc-path": el(), "doc-facts": el(),
  "doc-edit": el({hidden: true}), "doc-editor": el({value: ""}), "doc-back": el(), "doc-fwd": el(), "doc-wide": el(), "doc-close": el(),
  "doc-forgejo": el(), "doc-pins": el(), "doc-dot": el({hidden: true}), "doc-mode-read": el(), "doc-mode-edit": el(), documents: el(),
  "doc-new-form": el({hidden: true}), "doc-pathline": el(), "doc-name": el({value: ""}), "doc-edit-row": el(), "chat-log": chatLog};
var doc = {getElementById: function (id) { return byID[id] || null; }, createElement: el,
  createTextNode: function (s) { return el({textContent: String(s), nodeType: 3}); },
  addEventListener: function (type, fn) { (listeners[type] = listeners[type] || []).push(fn); }, removeEventListener: function () {},
  dispatchEvent: function (e) { (listeners[e.type] || []).forEach(function (f) { f(e); }); },
  querySelector: function () { return null; }, querySelectorAll: function () { return []; }, body: body,
  documentElement: {getAttribute: function () { return "dark"; }}};
var loc = {href: "http://lab/?project=code", search: "?project=code", hash: "", pathname: "/", origin: "http://lab"};
function EventSource(url) { this.url = url; this.on = {}; }
EventSource.prototype.addEventListener = function (type, fn) { this.on[type] = fn; };
EventSource.prototype.close = function () {};
EventSource.CLOSED = 2;
var sandbox = {document: doc, window: {addEventListener: function (t, f) { (winListeners[t] = winListeners[t] || []).push(f); }, location: loc},
  location: loc, history: {state: null, pushState: function () {}, replaceState: function () {}},
  localStorage: store(), sessionStorage: store(),
  fetch: function (url) { requests.push(String(url));
    return Promise.resolve({ok: true, status: 200, json: function () { return Promise.resolve(
      {path: "docs/roadmap.md", title: "The roadmap", html: "<h1>The roadmap</h1>", sha256: "5e1f", commit: "c0ffee0", at: "2026-11-09T10:00:00Z"}); },
      text: function () { return Promise.resolve(""); }}); },
  setTimeout: setTimeout, clearTimeout: clearTimeout, EventSource: EventSource,
  Event: function (type, o) { this.type = type; Object.assign(this, o || {}); },
  CustomEvent: function (type, o) { this.type = type; this.detail = o && o.detail; },
  URL: URL, URLSearchParams: URLSearchParams, CSS: {escape: function (s) { return s; }}, console: console,
  navigator: {userAgent: "node"}, matchMedia: function () { return {matches: false, addEventListener: function () {}}; },
  htmx: {ajax: function () { return Promise.resolve(); }, process: function () {}, swap: function () {}}};
vm.createContext(sandbox);
vm.runInNewContext(fs.readFileSync(process.argv[1], "utf8"), sandbox);
vm.runInNewContext(fs.readFileSync(process.argv[2], "utf8"), sandbox);
(listeners.DOMContentLoaded || []).forEach(function (f) { f({}); });
function fireClick(target) {
  var e = {target: target, defaultPrevented: false, preventDefault: function () { this.defaultPrevented = true; }, stopPropagation: function () {}};
  for (var n = target; n; n = n.parentNode) (n.elListeners || []).forEach(function (p) { p[1](e); });
  (listeners.click || []).forEach(function (f) { f(e); });
}
function settle() { return new Promise(function (r) { setTimeout(r, 0); }); }
(async function () {
  (listeners.view || []).forEach(function (f) { f({detail: {doc: "docs/roadmap.md"}}); });
  await settle();
  out.view = {hidden: byID["doc-view"].hidden, path: byID["doc-path"].textContent, article: byID["doc-article"].innerHTML, fetched: requests.slice()};
  byID["doc-view"].hidden = true;
  requests.length = 0;
  fireClick(anchor);
  await settle();
  out.press = {hidden: byID["doc-view"].hidden, path: byID["doc-path"].textContent, fetched: requests.slice()};
  var md = sandbox.chatView && sandbox.chatView.md ? sandbox.chatView.md : function () { return ""; };
  out.bare = md("See docs/roadmap.md.");
  out.linked = md("The [build plan](docs/plans/2026-09-24-build-plan.md) holds the cards.");
  out.prefixed = md("See [the plan](plans/2026-09-24-build-plan.md).");
  out.external = md("The [wiki](https://example.com/wiki) holds the rest.");
  var tick = String.fromCharCode(96);
  out.code = md("The " + tick + "docs/roadmap.md" + tick + " file.");
  console.log(JSON.stringify(out));
})();`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver, "static/chat.js", "static/console.js")
	cmd.Env = append(os.Environ(), "TZ=UTC")
	raw, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node: %v: %s", err, ee.Stderr)
		}
		t.Fatalf("node: %v", err)
	}
	var got struct {
		View struct {
			Hidden  bool     `json:"hidden"`
			Path    string   `json:"path"`
			Article string   `json:"article"`
			Fetched []string `json:"fetched"`
		} `json:"view"`
		Press struct {
			Hidden  bool     `json:"hidden"`
			Path    string   `json:"path"`
			Fetched []string `json:"fetched"`
		} `json:"press"`
		Bare, Linked, Prefixed, External, Code string
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("node said %s: %v", raw, err)
	}
	const docURL = "/projects/code/docs/docs/roadmap.md"

	// the view event opens the member's document view on the document
	if got.View.Hidden || got.View.Path != "docs/roadmap.md" {
		t.Errorf("the view event opens %q, hidden %v", got.View.Path, got.View.Hidden)
	}
	if !strings.Contains(got.View.Article, "<h1>The roadmap</h1>") {
		t.Errorf("the opened view renders no document: %s", got.View.Article)
	}
	if !slices.Contains(got.View.Fetched, docURL) {
		t.Errorf("the open fetches %v, want %s", got.View.Fetched, docURL)
	}
	// the press of a rendered docref opens the view the same way
	if got.Press.Hidden || got.Press.Path != "docs/roadmap.md" {
		t.Errorf("the docref press opens %q, hidden %v", got.Press.Path, got.Press.Hidden)
	}
	if !slices.Contains(got.Press.Fetched, docURL) {
		t.Errorf("the press fetches %v, want %s", got.Press.Fetched, docURL)
	}

	// a bare path in prose renders the docref anchor
	if want := `<a class="docref" data-doc="docs/roadmap.md" href="?doc=docs/roadmap.md">docs/roadmap.md</a>`; !strings.Contains(got.Bare, want) {
		t.Errorf("the bare path renders:\n%s\nwant\n%s", got.Bare, want)
	}
	// a linked label renders the same anchor for the linked document
	for _, lack := range []string{`class="docref"`, `data-doc="docs/plans/2026-09-24-build-plan.md"`, ">build plan</a>"} {
		if !strings.Contains(got.Linked, lack) {
			t.Errorf("the linked label renders no docref anchor for the document:\n%s", got.Linked)
			break
		}
	}
	// a .md target without the docs/ prefix resolves against the root
	for _, lack := range []string{`class="docref"`, `data-doc="docs/plans/2026-09-24-build-plan.md"`, ">the plan</a>"} {
		if !strings.Contains(got.Prefixed, lack) {
			t.Errorf("the prefixed target renders no docref anchor for the document:\n%s", got.Prefixed)
			break
		}
	}
	// an outside link and a code span stay plain
	if strings.Contains(got.External, "docref") || !strings.Contains(got.External, "https://example.com/wiki") {
		t.Errorf("the outside link is no plain link:\n%s", got.External)
	}
	if strings.Contains(got.Code, "docref") || !strings.Contains(got.Code, "<code>docs/roadmap.md</code>") {
		t.Errorf("the code span is no plain code span:\n%s", got.Code)
	}
}
