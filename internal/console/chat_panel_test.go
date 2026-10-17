package console

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The project agent's panel beside the board: a writer's project page
// holds it with the input and the queue under it, the tabs of a narrow
// screen under the status line, which spans both, the chat's keys and its
// script; a reader's (a project they see but do not write) holds
// it without the input, the history it may not read or the keys; a member
// who writes no project, the wide view and a console without the chat's
// model get no panel.
func TestChatPanel(t *testing.T) {
	r, _, _ := newChatRig(t)
	r.repoWriters["linkshort/linkshort"] = []string{"sebastian", "alice"}
	ck, s := r.loggedIn(t)
	page := func(ck *http.Cookie, path string) string {
		t.Helper()
		rec := r.do(httptest.NewRequest(http.MethodGet, path, nil), ck)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	const code = "/?project=ai-software-factory-code"
	body := page(ck, code)
	for _, want := range []string{
		`<div id="screen" class="main with-chat">
<section id="chat" class="chat with-threads sw-collapsed" aria-labelledby="chat-title"`,
		`<section id="chat" class="chat with-threads sw-collapsed" aria-labelledby="chat-title" data-project="ai-software-factory-code" data-login="sebastian">
<div class="chat-head"><h2 id="chat-title">Chat</h2> <span class="chat-who">project agent · ai-software-factory-code</span> <button type="button" id="chat-new" class="chat-new" title="Start a new thread">New</button></div>
<nav id="switcher" class="ttable" aria-label="Your threads"><div class="thr"><span aria-hidden="true">mark · thread · last</span><button type="button" class="swtg" data-swtoggle aria-expanded="false" aria-label="Unfold the threads" title="Unfold the threads">›</button></div>
<div id="thread-rows" class="trows"></div></nav>
<button type="button" class="sw-sum" data-swtoggle aria-expanded="false" aria-controls="switcher" hidden></button>
<div class="chat-main">
<div id="thread-head" class="thead"></div>
<p class="chat-start" title="Ask or tell the project agent. It reads, explains, moves, answers, steers and approves as you, through the console's own actions; it runs no code.">Ask or tell the project agent. It reads, explains, moves, answers, steers and approves as you, through the console's own actions; it runs no code.</p>
<div id="chat-log" class="chat-log" role="log" aria-label="The conversation with the project agent">
</div>
<div id="chat-answer" class="answer" role="group" aria-label="Answer" hidden></div>
<form id="chat-form" class="chat-form composer" method="post" action="/projects/ai-software-factory-code/chat">
<input type="hidden" name="csrf" value="` + s.csrf + `">
<p class="arch-note" hidden>archived, read-only <button type="button" class="linkbtn" data-restore-open>restore</button></p>
<div id="chat-palette" class="palette" role="listbox" aria-label="Commands" hidden></div>
<script type="application/json" id="chat-commands">[{"name":"compact","words":"the older turns of this thread become a summary"},{"name":"new","words":"a new thread"}]</script>
<div class="prompt"><span class="ps" aria-hidden="true">›</span><label for="chat-input" class="visually-hidden">Your message to the project agent</label>
<textarea id="chat-input" name="message" rows="3" maxlength="8192" placeholder="Ask or tell the project… (Enter sends, Shift+Enter breaks the line, / lists the commands)" aria-keyshortcuts="/" aria-controls="chat-palette" aria-expanded="false"></textarea>
<button type="submit">Send</button></div>
<div class="own"><div class="own-l"><span class="cmds"><button type="button" class="cmd" data-cmd="compact" title="the older turns of this thread become a summary">/compact</button><button type="button" class="cmd" data-cmd="new" title="a new thread">/new</button></span></div></div>
</form>
<ol id="chat-queue" class="chat-queue" aria-label="Your messages waiting for the turn to end" title="Each goes out as its own turn once the one before it ends; click one to edit it. They live in this page: a reload drops them." hidden></ol>
</div><script src="/static/chat.js" defer></script>
</section>
<div id="board" data-project="ai-software-factory-code"`,
		`<dt><kbd>/</kbd></dt><dd>write to the project agent; <kbd>/</kbd> at the start of the box lists the commands</dd>
<dt><kbd>Ctrl</kbd>+<kbd>k</kbd></dt><dd>the threads: open one, or the inbox</dd>
<dt><kbd>Enter</kbd></dt><dd>send the message; <kbd>Shift</kbd>+<kbd>Enter</kbd> breaks the line</dd>
<dt><kbd>Escape</kbd></dt><dd>in the chat's input: leave it</dd>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the writer's page lacks\n%s", want)
		}
	}
	band, screen := strings.Index(body, `<section id="pulse" class="status`), strings.Index(body, `<div id="screen" class="main with-chat">`)
	if band < 0 || screen < 0 || band > screen || !strings.HasSuffix(body[band:screen], "</section>\n") {
		t.Errorf("the pulse band does not stand right above the screen")
	}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/static/chat.js", nil))
	if js, _ := io.ReadAll(rec.Body); rec.Code != http.StatusOK || !strings.Contains(string(js), "var chatView = ") {
		t.Fatalf("GET /static/chat.js = %d, %d bytes", rec.Code, len(js))
	}

	ak, _ := r.as(t, "alice")
	body = page(ak, code)
	if want := `<section id="chat" class="chat" aria-labelledby="chat-title">
<div class="chat-head"><h2 id="chat-title">Chat</h2> <span class="chat-who">project agent · ai-software-factory-code</span></div>
<div class="chat-main">
<p class="chat-start" title="You read this project: its writers chat with its agent.">You read this project: its writers chat with its agent.</p>
<div id="chat-log" class="chat-log" role="log" aria-label="The conversation with the project agent">
</div>
</div><script src="/static/chat.js" defer></script>
</section>`; !strings.Contains(body, want) {
		t.Errorf("the reader's page lacks\n%s", want)
	}
	for _, unwanted := range []string{"chat-form", "chat-input", "chat-queue", "data-login", "<kbd>/</kbd>", "switcher", "thread-head"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("the reader's page holds %s", unwanted)
		}
	}

	bk, _ := r.as(t, "bob")
	none := func(who, body string) {
		t.Helper()
		for _, unwanted := range []string{`id="chat"`, "with-chat", "chat.js"} {
			if strings.Contains(body, unwanted) {
				t.Errorf("%s: the page holds %s", who, unwanted)
			}
		}
	}
	none("bob, who writes no project", page(bk, "/"))
	none("the wide view", page(ck, code+"&view=wide"))
	r.c.cfg.Operator = nil
	none("a console without the model", page(ck, code))
}

// chat.js renders a stored proposal by its state: a pending one offers Do
// it and Skip, as the stream's proposal does in the same markup, and a
// used one is its folded call with the word used. The select of older
// conversations offers the current one and each other, and is empty when
// there is no other. The queue: a message sent while no turn streams goes
// out at once; three sent during the turn wait, one edited, one removed,
// and each turn's end sends the next of the rest, in order; its markup
// labels each edit and remove control. Run under node, which the test
// needs.
func TestChatPanelScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var chatView = require("vm").runInNewContext(require("fs").readFileSync(process.argv[1], "utf8") + "\nchatView",
  {document: {getElementById: function () { return null; }}});
var args = '{"title":"Cache metrics","lane":"ready"}';
var msgs = [{role: "user", content: "file a card", at: "2026-10-11T09:30:01Z"},
  {role: "assistant", content: "", at: "2026-10-11T09:30:02Z", tool_calls: [{id: "call_1", name: "new_card", arguments: args,
    result: "Proposed to sebastian: File a card, title Cache metrics, lane ready. Nothing runs until they press Do it.", proposal: "tok1"}]}];
var now = new Date("2026-10-11T12:00:00Z");
var cmds = [{name: "compact", words: "the older turns of this thread become a summary"}, {name: "new", words: "a new thread"},
  {name: "standup", words: "what moved", message: "What moved since yesterday?"}];
var threads = [{number: 3, title: "Inbox", state: "active", inbox: true, last_at: "2026-10-11T09:31:00Z", unread: 2, needs_you: 1, new_rows: 1},
  {number: 2, title: "world", state: "active", current: true, last_at: "2026-10-11T09:30:04Z", unread: 0, working: true},
  {number: 4, title: "", state: "active", last_at: "2026-10-10T18:00:00Z", unread: 0},
  {number: 1, title: "a <b>", state: "archived", last_at: "2026-10-08T09:30:02Z", unread: 0}];
console.log(JSON.stringify({
  pending: chatView.historyHTML(msgs, "sebastian", [{token: "tok1", state: "pending"}]),
  used: chatView.historyHTML(msgs, "sebastian", [{token: "tok1", state: "used"}]),
  stream: chatView.proposalHTML({id: "tok1", tool: "new_card", arguments: JSON.parse(args), description: "File a card, title Cache metrics, lane ready"}),
  rows: chatView.rowsHTML(threads, 2, now),
  sum: chatView.sumHTML(threads, false),
  unread: String(chatView.unreadOf(threads)),
  inboxHead: chatView.headHTML(threads[0], now),
  head: chatView.headHTML(threads[1], now),
  headActs: chatView.headHTML({number: 2, title: "world", state: "active", last_at: "2026-10-11T09:30:00Z", messages: 4}, now),
  pendingRow: JSON.stringify([chatView.pendingOf([{id: 1, kind: "escalation", choices: ["Yes", "No"]}]) && 1,
    chatView.pendingOf([{id: 1, kind: "escalation", choices: ["Yes"]}, {id: 2, role: "user", content: "Yes"}]) && 1,
    chatView.pendingOf([{id: 1, kind: "escalation"}]), chatView.pendingOf([]),
    chatView.pendingOf([{id: 1, kind: "escalation", choices: ["Yes"], settled: true, reason: "answered by sebastian · 09:14"}])]),
  answerRow: chatView.answerHTML({choices: ["Postgres", "a <b>"]}),
  levelMenu: chatView.levelMenuHTML([{name: "manual", words: "every action confirms"}, {name: "assisted", words: "a <b>"}], "assisted"),
  levelConfirm: chatView.levelConfirmHTML({name: "full auto", words: "nothing waits"}),
  deleteOne: chatView.deleteHTML({number: 2, title: "a <b>", messages: 1}),
  deleteMany: chatView.deleteHTML({number: 2, title: "world", messages: 4}),
  poke: chatView.factoryHTML({id: 7, role: "assistant", kind: "poke", content: "Started the login card.", at: "2026-10-11T09:12:00Z",
    decider: "assisted level · started by the agent", choices: ["Open the card"]}),
  compacted: chatView.threadHTML([{id: 1, role: "user", content: "a1", at: "2026-10-11T09:30:01Z"}, {id: 2, role: "assistant", content: "a2", at: "2026-10-11T09:30:02Z"},
    {id: 3, role: "user", content: "b1", at: "2026-10-11T09:31:00Z"}], "sebastian", [], {summary: "the cache was fixed", summary_upto: 2}),
  cleared: chatView.threadHTML([{id: 3, role: "user", content: "b1", at: "2026-10-11T09:31:00Z"}], "sebastian", [], {cleared: 2}),
  clearedOne: chatView.threadHTML([{id: 3, role: "user", content: "b1", at: "2026-10-11T09:31:00Z"}], "sebastian", [], {cleared: 1}),
  escalation: chatView.factoryHTML({id: 8, role: "assistant", kind: "escalation", content: "Which store?", at: "2026-10-11T09:13:00Z",
    choices: ["Postgres", "SQLite"]}),
  palette: JSON.stringify([chatView.paletteOf(cmds, "/"), chatView.paletteOf(cmds, "/c"), chatView.paletteOf(cmds, "/x"), chatView.paletteOf(cmds, "/c x"),
    chatView.paletteOf(cmds, "hi")].map(function (l) { return l && l.map(function (c) { return c.name; }); })),
  paletteHTML: chatView.paletteHTML(cmds.slice(0, 2), 1),
  expand: [chatView.expand(cmds, "/standup"), chatView.expand(cmds, "/compact"), chatView.expand(cmds, "/standup now")].join("|"),
  repeated: chatView.factoryHTML({id: 9, role: "assistant", kind: "poke", content: "#7 failed.", at: "2026-10-11T09:14:00Z", repeats: 2}),
  settledEscalation: chatView.factoryHTML({id: 10, role: "assistant", kind: "escalation", content: "Which store?", at: "2026-10-11T09:15:00Z",
    choices: ["Postgres", "SQLite"], settled: true, reason: "answered by sebastian · 09:14"}),
  settledPoke: chatView.factoryHTML({id: 11, role: "assistant", kind: "poke", content: "#7 failed.", at: "2026-10-11T09:16:00Z",
    choices: ["Retry", "Leave it"], settled: true, reason: "moved on · 09:16"}),
  queue: (function () {
    var started = [], drawn = [], busy = false;
    var q = chatView.queue(function (text) { started.push(text); busy = true; }, function () { return busy; },
      function (items) { drawn.push(items.join("|")); });
    q.add("first");
    q.add("second");
    q.add("third");
    q.add("fourth");
    q.edit(0, " second, edited ");
    q.remove(1);
    for (var i = 0; i < 3; i++) {
      busy = false;
      q.flush();
    }
    return JSON.stringify({started: started, drawn: drawn});
  })(),
  queueHTML: chatView.queueHTML(["a <b>", "c"])
}));`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver, "static/chat.js")
	cmd.Env = append(os.Environ(), "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node said %s: %v", out, err)
	}
	const (
		you      = `<div class="msg you"><span class="av" aria-hidden="true">S</span><p class="who"><span class="name">sebastian</span> <time>10-11 09:30Z</time></p><p class="said">file a card</p></div>`
		agent    = `<div class="msg agent"><span class="av" aria-hidden="true"></span><p class="who"><span class="name">project agent</span> <time>10-11 09:30Z</time></p>`
		args     = `{&quot;title&quot;:&quot;Cache metrics&quot;,&quot;lane&quot;:&quot;ready&quot;}`
		result   = `Proposed to sebastian: File a card, title Cache metrics, lane ready. Nothing runs until they press Do it.`
		proposal = `<div class="proposal" data-proposal="tok1"><p class="ptitle">I will:</p><ul><li>File a card, title Cache metrics, lane ready</li></ul>` +
			`<details class="args"><summary>new_card arguments</summary><pre>` + args + `</pre></details>` +
			`<p class="pfoot"><button type="button" class="doit" data-doit>Do it</button> <button type="button" class="skip" data-skip>Skip</button></p></div>`
		dot = `<span class="tmark working"><i class="dot" aria-hidden="true"></i><span class="visually-hidden">a turn in flight</span></span>`
	)
	for _, c := range []struct{ name, want string }{
		{"pending", you + agent + proposal + `</div>`},
		{"used", you + agent + `<details class="tool"><summary>🔧 new_card · ` + result + `</summary><pre>` + args + "\n\n" + result + `</pre></details>` +
			`<p class="pfoot"><span class="expired">used</span></p></div>`},
		{"stream", proposal},
		{"rows", `<button type="button" class="trow unread inbox" data-thread="3" aria-current="false" title="Inbox, 1 needs you · 1 new">` +
			`<span class="tmark inbox" aria-hidden="true">⌂</span><span class="ttitle">Inbox</span><span class="bsplit" aria-label="1 needs you · 1 new"><span class="badge">1</span><span class="badge new">1</span></span></button>` +
			`<button type="button" class="trow on" data-thread="2" aria-current="true" title="world, a turn in flight">` + dot +
			`<span class="ttitle">world</span><span class="tat">09:30</span></button>` +
			`<button type="button" class="trow" data-thread="4" aria-current="false" title="New thread"><span class="tmark" aria-hidden="true"></span>` +
			`<span class="ttitle">New thread</span><span class="tat">yesterday</span></button>` +
			`<div class="tarch"><span class="tarch-l">archived</span><span class="trow arch"><span class="tmark" aria-hidden="true">arc</span>` +
			`<button type="button" class="ttitle open-arch" data-thread="1" aria-current="false">a &lt;b&gt;</button>` +
			`<button type="button" class="linkbtn restore" data-restore="1">restore</button></span></div>`},
		{"sum", `Threads <span class="n">3</span> · <span class="bsplit" aria-label="1 needs you · 1 new"><span class="badge">1</span><span class="badge new">1</span></span> · <span class="tmark working"><i class="dot" aria-hidden="true"></i></span> 1 working` +
			`<span class="chev" aria-hidden="true">⌃</span>`},
		{"unread", "2"},
		{"inboxHead", `<h3 class="ttl"><span class="tmark inbox" aria-hidden="true">⌂</span>Inbox</h3><span class="thint">the factory's thread: the agent speaks here uninvited</span>`},
		{"head", `<h3 class="ttl"><button type="button" class="rename" data-rename title="Rename the thread">world</button></h3><span class="thint when">09:30</span>`},
		{"headActs", `<h3 class="ttl"><button type="button" class="rename" data-rename title="Rename the thread">world</button></h3><span class="thint when">09:30</span>` +
			`<span class="tacts"><button type="button" data-clear title="Hide the old rows from view; the record keeps them">Clear old</button>` +
			`<button type="button" data-archive>Archive</button><button type="button" class="danger" data-delete>Delete…</button></span>`},
		{"pendingRow", `[1,1,null,null,null]`},
		{"answerRow", `<span class="answer-l">Answer:</span><button type="button" data-answer="Postgres">Postgres</button>` +
			`<button type="button" data-answer="a &lt;b&gt;">a &lt;b&gt;</button><span class="answer-or">or in your words below</span>`},
		{"levelMenu", `<p class="lp-head"><span>autonomy</span><span>a change confirms</span></p>` +
			`<button type="button" role="menuitemradio" aria-checked="false" data-lvl="manual"><i aria-hidden="true"></i><b>manual</b><span class="ck" aria-hidden="true"></span><span class="lw">every action confirms</span></button>` +
			`<button type="button" role="menuitemradio" aria-checked="true" data-lvl="assisted"><i aria-hidden="true"></i><b>assisted</b><span class="ck" aria-hidden="true">✓</span><span class="lw">a &lt;b&gt;</span></button>`},
		{"levelConfirm", `<div class="lvl-confirm" role="dialog" aria-labelledby="lvl-title"><h2 id="lvl-title">Set the project's autonomy to full auto?</h2>` +
			`<p>This writes a policy change and lands in the audit like every decision. From then on: nothing waits.</p>` +
			`<div class="acts"><button type="button" class="need-btn" data-set="full auto">Set</button><button type="button" data-keep>Keep</button></div></div>`},
		{"deleteOne", `<div id="thread-dialog" class="overlay" role="dialog" aria-modal="true" aria-labelledby="dlg-title"><div class="overlay-box">` +
			`<h2 id="dlg-title">Delete “a &lt;b&gt;”?</h2><p>Its 1 message goes for good: a hard delete, no archive. One event will name you, the thread and the count. ` +
			`What the chat did stays in the events, the audit and git, so no decision and no merged card goes with it.</p>` +
			`<div class="acts"><button type="button" class="danger" data-dlg="delete">Delete the thread</button><button type="button" data-dlg="keep">Keep it</button></div></div></div>`},
		{"deleteMany", `<div id="thread-dialog" class="overlay" role="dialog" aria-modal="true" aria-labelledby="dlg-title"><div class="overlay-box">` +
			`<h2 id="dlg-title">Delete “world”?</h2><p>Its 4 messages go for good: a hard delete, no archive. One event will name you, the thread and the count. ` +
			`What the chat did stays in the events, the audit and git, so no decision and no merged card goes with it.</p>` +
			`<div class="acts"><button type="button" class="danger" data-dlg="delete">Delete the thread</button><button type="button" data-dlg="keep">Keep it</button></div></div></div>`},
		{"poke", `<div class="msg agent poke" data-message="7"><span class="av" aria-hidden="true"></span><p class="who"><span class="name">the factory</span> <time>10-11 09:12Z</time></p>` +
			`<div class="said"><p>Started the login card.</p></div><p class="decider"><span class="visually-hidden">Decided by: </span>assisted level · started by the agent</p>` +
			`<p class="choices"><button type="button" data-choice="Open the card">Open the card</button></p></div>`},
		{"compacted", `<details class="compacted"><summary>2 earlier messages, compacted</summary>` +
			`<div class="msg you"><span class="av" aria-hidden="true">S</span><p class="who"><span class="name">sebastian</span> <time>10-11 09:30Z</time></p><p class="said">a1</p></div>` +
			`<div class="msg agent"><span class="av" aria-hidden="true"></span><p class="who"><span class="name">project agent</span> <time>10-11 09:30Z</time></p><div class="said"><p>a2</p></div></div></details>` +
			`<div class="msg sum"><span class="av" aria-hidden="true">≡</span><p class="who"><span class="name">summary</span> <time>10-11 09:30Z</time></p><div class="said"><p>the cache was fixed</p></div></div>` +
			`<div class="msg you"><span class="av" aria-hidden="true">S</span><p class="who"><span class="name">sebastian</span> <time>10-11 09:31Z</time></p><p class="said">b1</p></div>`},
		{"cleared", `<p class="meta chat-cleared">cleared: 2 older messages out of view</p>` +
			`<div class="msg you"><span class="av" aria-hidden="true">S</span><p class="who"><span class="name">sebastian</span> <time>10-11 09:31Z</time></p><p class="said">b1</p></div>`},
		{"clearedOne", `<p class="meta chat-cleared">cleared: 1 older message out of view</p>` +
			`<div class="msg you"><span class="av" aria-hidden="true">S</span><p class="who"><span class="name">sebastian</span> <time>10-11 09:31Z</time></p><p class="said">b1</p></div>`},
		{"escalation", `<div class="msg agent poke esc" data-message="8"><span class="av" aria-hidden="true"></span><p class="who"><span class="name">the factory</span> <time>10-11 09:13Z</time> <span class="esc-l">needs you</span></p>` +
			`<div class="esc-b"><div class="said"><p>Which store?</p></div></div></div>`},
		{"palette", `[["compact","new","standup"],["compact"],[],null,null]`},
		{"paletteHTML", `<button type="button" role="option" id="cmd-compact" aria-selected="false" data-cmd="compact"><b>/compact</b><span>the older turns of this thread become a summary</span></button>` +
			`<button type="button" role="option" id="cmd-new" aria-selected="true" data-cmd="new"><b>/new</b><span>a new thread</span></button>`},
		{"expand", `What moved since yesterday?|/compact|/standup now`},
		{"repeated", `<div class="msg agent poke" data-message="9"><span class="av" aria-hidden="true"></span><p class="who"><span class="name">the factory</span> <time>10-11 09:14Z</time> <span class="rep">3 times</span></p>` +
			`<div class="said"><p><a href="#issue-7" class="cardref" data-issue="7">#7</a> failed.</p></div></div>`},
		{"settledEscalation", `<div class="msg agent poke esc settled" data-message="10"><span class="av" aria-hidden="true"></span><p class="who"><span class="name">the factory</span> <time>10-11 09:15Z</time></p>` +
			`<div class="esc-b"><div class="said"><p>Which store?</p></div><p class="settled-why"><span class="visually-hidden">Settled: </span>answered by sebastian · 09:14</p></div><p class="fold" role="button" tabindex="0" aria-expanded="false" data-fold><span class="visually-hidden">Settled: </span><span class="fw">answered by sebastian · 09:14</span><span class="ft">Which store?</span></p></div>`},
		{"settledPoke", `<div class="msg agent poke settled" data-message="11"><span class="av" aria-hidden="true"></span><p class="who"><span class="name">the factory</span> <time>10-11 09:16Z</time></p>` +
			`<div class="said"><p><a href="#issue-7" class="cardref" data-issue="7">#7</a> failed.</p></div><p class="settled-why"><span class="visually-hidden">Settled: </span>moved on · 09:16</p><p class="fold" role="button" tabindex="0" aria-expanded="false" data-fold><span class="visually-hidden">Settled: </span><span class="fw">moved on · 09:16</span><span class="ft">#7 failed.</span></p></div>`},
		{"queue", `{"started":["first","second, edited","fourth"],"drawn":["","second","second|third","second|third|fourth","second, edited|third|fourth","second, edited|fourth","fourth","",""]}`},
		{"queueHTML", `<li><button type="button" class="queued-text" data-edit="0" aria-label="Edit queued message 1: a &lt;b&gt;">a &lt;b&gt;</button>` +
			`<button type="button" class="queued-remove" data-remove="0" aria-label="Remove queued message 1">×</button></li>` +
			`<li><button type="button" class="queued-text" data-edit="1" aria-label="Edit queued message 2: c">c</button>` +
			`<button type="button" class="queued-remove" data-remove="1" aria-label="Remove queued message 2">×</button></li>`},
	} {
		if got[c.name] != c.want {
			t.Errorf("%s =\n%s\nwant\n%s", c.name, got[c.name], c.want)
		}
	}
}
