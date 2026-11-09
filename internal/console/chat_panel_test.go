package console

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The project agent's panel beside the board: a writer's project page
// holds it with the input and the note an inbox shows in its place, the
// tabs of a narrow screen under the status line, which spans both, the
// chat's keys and its script; a reader's (a project they see but do not
// write) holds it without the input, the history it may not read or the
// keys; a human who writes no project, the wide view and a console
// without the chat's model get no panel.
func TestChatPanel(t *testing.T) {
	r, _, _ := newChatRig(t)
	r.repoWriters["linkshort/linkshort"] = []string{"sebastian", "alice"}
	r.forgetWriters()
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
<button type="button" class="edge-tab" data-tip="The board back; Esc also works">board</button>
<section id="chat" class="chat with-threads sw-collapsed" aria-labelledby="chat-title"`,
		`<section id="chat" class="chat with-threads sw-collapsed" aria-labelledby="chat-title" data-project="ai-software-factory-code" data-login="sebastian">
<div class="chat-head"><h2 id="chat-title">Chat</h2> <span class="chat-who">project agent · ai-software-factory-code</span> <button type="button" id="chat-full" class="wctl" aria-pressed="false" aria-label="Chat and board" data-tip="Grow the chat to the whole screen; the glyph flips, click again or press Esc for the board back"><svg viewBox="0 0 16 16" aria-hidden="true"><path class="exp" d="M9.5 2.5h4v4M6.5 13.5h-4v-4M3 13l10-10" fill="none" stroke="currentColor" stroke-width="1.5"/><path class="col" d="M13.5 6.5v-4h-4M2.5 9.5v4h4M13 3L3 13" fill="none" stroke="currentColor" stroke-width="1.5"/></svg></button> <button type="button" id="intake-btn" class="intake" data-intake="open" aria-pressed="false" data-tip="The project's intake: closed, the roadmap is not pulled and the agent takes no work off it on its own; every ask and need still files.">intake <span class="intake-name">open</span></button> <button type="button" id="chat-new" class="chat-new" data-tip="Start a new thread">New</button></div>
<nav id="switcher" class="ttable" aria-label="Your threads"><div class="thr"><span aria-hidden="true">mark · thread · last</span><button type="button" class="swtg" data-swtoggle aria-expanded="false" aria-label="Unfold the threads" data-tip="Unfold the threads">›</button></div>
<div id="thread-rows" class="trows"></div></nav>
<button type="button" class="sw-sum" data-swtoggle aria-expanded="false" aria-controls="switcher" hidden></button>
<div class="chat-main">
<div id="thread-head" class="thead"></div>
<p class="chat-start" title="Ask or tell the project agent. It reads, explains, files, answers, steers and approves as you, through the console's own actions; it runs no code.">Ask or tell the project agent. It reads, explains, files, answers, steers and approves as you, through the console's own actions; it runs no code.</p>
<div id="chat-log" class="chat-log" role="log" aria-label="The conversation with the project agent">
</div>
<div id="chat-answer" class="answer" role="group" aria-label="Answer" hidden></div>
<form id="chat-form" class="chat-form composer" method="post" action="/projects/ai-software-factory-code/chat">
<input type="hidden" name="csrf" value="` + s.csrf + `">
<p class="arch-note" hidden>archived, read-only <button type="button" class="linkbtn" data-restore-open>restore</button></p>
<p class="inbox-note" hidden>The factory's thread. Answer with its buttons, and write in a thread of your own. <button type="button" class="linkbtn" data-new-thread>New thread</button></p>
<div id="chat-palette" class="palette" role="listbox" aria-label="Commands" hidden></div>
<script type="application/json" id="chat-commands">[{"name":"compact","words":"the older turns of this thread become a summary"},{"name":"new","words":"a new thread"}]</script>
<ol id="chat-images" class="chat-images" aria-label="Images riding your next message" title="Each rides the message you send next, named for your text to point at; × drops one. A reload drops them all." hidden></ol>
<ol id="chat-docs" class="chat-docs" aria-label="Documents riding your next message" title="Each rides the message you send next, named for your text to point at; × drops one. A reload drops them all." hidden></ol>
<div class="prompt"><span class="ps" aria-hidden="true">›</span><label for="chat-input" class="visually-hidden">Your message to the project agent</label>
<textarea id="chat-input" name="message" rows="3" maxlength="8192" placeholder="Ask or tell the project… (Enter sends, Shift+Enter breaks the line, / lists the commands)" aria-keyshortcuts="/" aria-controls="chat-palette" aria-expanded="false"></textarea>
<button type="submit">Send</button></div>
<div class="own"><div class="own-l"><span class="cmds"><button type="button" class="cmd" data-cmd="compact" title="the older turns of this thread become a summary">/compact</button><button type="button" class="cmd" data-cmd="new" title="a new thread">/new</button></span></div></div>
</form>
</div><script src="/static/chat.js" defer></script>
</section>
<div id="chat-divider" role="separator" aria-orientation="vertical" aria-label="Drag it, or press the arrow keys, to resize the chat and the board" tabindex="0" data-tip="Drag to resize; the arrow keys step it"><span class="grip" aria-hidden="true">⤥</span></div>
<div id="board" data-project="ai-software-factory-code"`,
		`<dt><kbd>/</kbd></dt><dd>write to the project agent; <kbd>/</kbd> at the start of the box lists the commands</dd>
<dt><kbd>Ctrl</kbd>+<kbd>k</kbd></dt><dd>the threads: open one, or the inbox</dd>
<dt><kbd>Enter</kbd></dt><dd>send the message; <kbd>Shift</kbd>+<kbd>Enter</kbd> breaks the line</dd>
<dt><kbd>Escape</kbd></dt><dd>in the chat: stop the turn in flight; in the input when none runs: leave it</dd>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the writer's page lacks\n%s", want)
		}
	}
	if band, screen := strings.Index(body, `<details id="status-line" class="status"`), strings.Index(body, `<div id="screen" class="main with-chat">`); band < 0 || screen < 0 || band > screen {
		t.Errorf("the status band does not stand right above the screen")
	}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/static/chat.js", nil))
	if js, _ := io.ReadAll(rec.Body); rec.Code != http.StatusOK || !strings.Contains(string(js), "var chatView = ") {
		t.Fatalf("GET /static/chat.js = %d, %d bytes", rec.Code, len(js))
	}

	// alice writes linkshort and reads the code project's repository
	r.c.cfg.HTTP = &http.Client{Transport: readerForgejo{http.DefaultTransport}}
	ak, _ := memberLogin(t, r, "alice")
	body = page(ak, code)
	if want := `<section id="chat" class="chat" aria-labelledby="chat-title">
<div class="chat-head"><h2 id="chat-title">Chat</h2> <span class="chat-who">project agent · ai-software-factory-code</span> <button type="button" id="chat-full" class="wctl" aria-pressed="false" aria-label="Chat and board" data-tip="Grow the chat to the whole screen; the glyph flips, click again or press Esc for the board back"><svg viewBox="0 0 16 16" aria-hidden="true"><path class="exp" d="M9.5 2.5h4v4M6.5 13.5h-4v-4M3 13l10-10" fill="none" stroke="currentColor" stroke-width="1.5"/><path class="col" d="M13.5 6.5v-4h-4M2.5 9.5v4h4M13 3L3 13" fill="none" stroke="currentColor" stroke-width="1.5"/></svg></button></div>
<div class="chat-main">
<p class="chat-start" title="You read this project: its writers chat with its agent.">You read this project: its writers chat with its agent.</p>
<div id="chat-log" class="chat-log" role="log" aria-label="The conversation with the project agent">
</div>
</div><script src="/static/chat.js" defer></script>
</section>
<div id="chat-divider" role="separator" aria-orientation="vertical" aria-label="Drag it, or press the arrow keys, to resize the chat and the board" tabindex="0" data-tip="Drag to resize; the arrow keys step it"><span class="grip" aria-hidden="true">⤥</span></div>`; !strings.Contains(body, want) {
		t.Errorf("the reader's page lacks\n%s", want)
	}
	for _, unwanted := range []string{"chat-form", "chat-input", "data-login", "<kbd>/</kbd>", "switcher", "thread-head"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("the reader's page holds %s", unwanted)
		}
	}

	r.repoWriters["sf/infra"] = []string{"bob"}
	bk, _ := memberLogin(t, r, "bob")
	none := func(who, body string) {
		t.Helper()
		for _, unwanted := range []string{`id="chat"`, "with-chat", "chat.js"} {
			if strings.Contains(body, unwanted) {
				t.Errorf("%s: the page holds %s", who, unwanted)
			}
		}
	}
	none("bob, who writes no project agents work on", page(bk, "/"))
	none("the wide view", page(ck, code+"&view=wide"))
	r.c.cfg.Operator = nil
	none("a console without the model", page(ck, code))
}

// chat.js renders a stored proposal by its state: a pending one offers Do
// it and Skip, as the stream's proposal does in the same markup, and a
// used one is its folded call with the word used. The select of older
// conversations offers the current one and each other, and is empty when
// there is no other. A run of tool calls shows its newest, the earlier
// ones folded behind one line, a single call stays as it is; a member's
// row the agent has not read yet says so, and says read once it has. A
// budget row renders as the factory's, and while the chat stands stopped
// its newest open fund row alone offers the fund, prefilled with the
// suggestion, or to a member who may not fund the line of who may. The
// thinking mark is the stop's button with the fold's chevron beside it,
// and a stopped turn leaves its grey mark on the cut answer and the
// factory's stop row. Every choice, a poke's or an ask's, is a press
// through /answer; an inbox's answer row offers no words of one's own,
// and its head names it the factory's thread, whoever's inbox it is. Run
// under node, which the test needs.
func TestChatPanelScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var chatView = require("vm").runInNewContext(require("fs").readFileSync(process.argv[1], "utf8") + "\nchatView",
  {document: {getElementById: function () { return null; }}});
var args = '{"title":"Cache metrics","track":"direct"}';
var msgs = [{role: "user", content: "file a card", at: "2026-10-11T09:30:01Z"},
  {role: "assistant", content: "", at: "2026-10-11T09:30:02Z", tool_calls: [{id: "call_1", name: "new_card", arguments: args,
    result: "Proposed to sebastian: File a card, title Cache metrics, track direct. Nothing runs until they press Do it.", proposal: "tok1"}]}];
var now = new Date("2026-10-11T12:00:00Z");
var cmds = [{name: "compact", words: "the older turns of this thread become a summary"}, {name: "new", words: "a new thread"},
  {name: "standup", words: "what moved", message: "What moved since yesterday?"}];
var threads = [{number: 3, title: "Inbox", state: "active", inbox: true, last_at: "2026-10-11T09:31:00Z", unread: 2, needs_you: 1, new_rows: 1, waits_you: 2},
  {number: 2, title: "world", state: "active", current: true, last_at: "2026-10-11T09:30:04Z", unread: 0, working: true},
  {number: 4, title: "", state: "active", last_at: "2026-10-10T18:00:00Z", unread: 0, waits_you: 1},
  {number: 1, title: "a <b>", state: "archived", last_at: "2026-10-08T09:30:02Z", unread: 0}];
console.log(JSON.stringify({
  pending: chatView.historyHTML(msgs, "sebastian", [{token: "tok1", state: "pending"}]),
  used: chatView.historyHTML(msgs, "sebastian", [{token: "tok1", state: "used"}]),
  stream: chatView.proposalHTML({id: "tok1", tool: "new_card", arguments: JSON.parse(args), description: "File a card, title Cache metrics, track direct"}),
  rows: chatView.rowsHTML(threads, 2, "", "sebastian", now),
  sum: chatView.sumHTML(threads, "sebastian", false),
  unread: String(chatView.unreadOf(threads, "sebastian")),
  inboxHead: chatView.headHTML(threads[0], "sebastian", now),
  theirInboxHead: chatView.headHTML({number: 7, title: "Inbox", state: "active", inbox: true, login: "alice"}, "sebastian", now),
  head: chatView.headHTML(threads[1], "sebastian", now),
  headActs: chatView.headHTML({number: 2, title: "world", state: "active", last_at: "2026-10-11T09:30:00Z", messages: 4, unswept: 4}, "sebastian", now),
  headCleared: chatView.headHTML({number: 2, title: "world", state: "active", last_at: "2026-10-11T09:30:00Z", messages: 4}, "sebastian", now),
  pendingRow: JSON.stringify([chatView.pendingOf([{id: 1, kind: "escalation", choices: ["Yes", "No"]}]) && 1,
    chatView.pendingOf([{id: 1, kind: "escalation", choices: ["Yes"]}, {id: 2, role: "user", content: "Yes"}]) && 1,
    chatView.pendingOf([{id: 1, kind: "escalation"}]), chatView.pendingOf([]),
    chatView.pendingOf([{id: 1, kind: "escalation", choices: ["Yes"], settled: true, reason: "answered by sebastian · 09:14"}])]),
  answerRow: chatView.answerHTML({choices: ["Postgres", "a <b>"]}),
  askAnswerRow: chatView.answerHTML({id: 2, kind: "ask", choices: ["One card"]}),
  inboxAnswerRow: chatView.answerHTML({id: 2, kind: "ask", choices: ["One a night", "One an hour"]}, true),
  levelMenu: chatView.levelMenuHTML([{name: "manual", words: "every action confirms"}, {name: "assisted", words: "a <b>"}], "assisted"),
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
  calls: chatView.historyHTML([{role: "user", content: "look", at: "2026-10-11T09:30:01Z"},
    {role: "assistant", content: "", at: "2026-10-11T09:30:02Z", tool_calls: [{id: "c1", name: "board", arguments: "{}", result: "3 cards"},
      {id: "c2", name: "card", arguments: "{}", result: "Refused: no card 9"}]},
    {role: "assistant", content: "", at: "2026-10-11T09:30:03Z", tool_calls: [{id: "c3", name: "ci", arguments: "{}", result: "green"}]},
    {role: "assistant", content: "All green.", at: "2026-10-11T09:30:04Z"}], "sebastian", []),
  trail: chatView.historyHTML([{role: "user", content: "which files?", at: "2026-10-11T09:30:01Z"},
    {role: "assistant", content: "", at: "2026-10-11T09:30:02Z", tool_calls: [{id: "c1", name: "research", arguments: "{\"ask\":\"which files?\"}",
      result: "Findings:\nmain holds README.md.", trail: "\ntree {\"ref\":\"main\"}\nREADME.md 1200\n"}]},
    {role: "assistant", content: "Done.", at: "2026-10-11T09:30:03Z"}], "sebastian", []),
  queuedRow: chatView.userHTML("sebastian", "also this", "2026-10-11T09:30:05Z", true),
  tokenThread: chatView.historyHTML([{id: 1, role: "user", content: "what waits?", token: "claude-<b>", at: "2026-10-11T09:30:01Z"},
    {id: 2, role: "user", content: "and now", at: "2026-10-11T09:31:01Z"}], "sebastian", []),
  readRow: chatView.readHTML,
  openQ: [chatView.openQuestionHTML("Can you do that? <b>"), chatView.openQuestionHTML("")].join("|"),
  imgRow: chatView.userHTML("sebastian", "look at the run", "2026-10-11T09:30:01Z", false,
    [{id: 901, content_type: "image/png", name: "img1", url: "/projects/ai-software-factory-code/chat/attachments/901"}], []),
  zoom: chatView.zoomHTML("/projects/ai-software-factory-code/chat/attachments/901", "img1"),
  mark: chatView.thinkingHTML,
  stoppedMark: chatView.stoppedMarkHTML,
  stopRow: chatView.factoryHTML({id: 13, role: "assistant", kind: "stop", content: "Stopped by sebastian.", at: "2026-10-11T09:20:00Z"}),
  stoppedThread: chatView.historyHTML([{id: 1, role: "user", content: "what is open?", at: "2026-10-11T09:30:01Z"},
    {id: 2, role: "assistant", content: "Partial", cut: true, at: "2026-10-11T09:30:02Z"},
    {id: 3, role: "assistant", kind: "stop", content: "Stopped by sebastian.", at: "2026-10-11T09:30:03Z"}], "sebastian", []),
  noFoldInHistory: chatView.historyHTML([{id: 1, role: "user", content: "what is open?", at: "2026-10-11T09:30:01Z"},
    {id: 2, role: "assistant", content: "Partial", cut: true, at: "2026-10-11T09:30:02Z"}], "sebastian", []).indexOf("data-think") >= 0 ? "true" : "false",
  share: JSON.stringify([chatView.clampShare(300, 1200, 16), chatView.clampShare(100, 1200, 16),
    chatView.clampShare(1200, 1200, 16), chatView.clampShare(500.25, 1200, 16)]),
  budgetRow: chatView.factoryHTML({id: 12, role: "assistant", kind: "budget", birth_kind: "chat_budget_spent", at: "2026-10-28T22:20:00Z",
    content: "The chat's budget for October is used up: €14.96 of €15.00 spent, and the next call does not fit what is left.\nfree headroom 0.038793 EUR"},
    {budget_eur: 15, spent_eur: 14.96, suggest_eur: 3, can_fund: true}),
  budgetRowWriter: chatView.factoryHTML({id: 12, role: "assistant", kind: "budget", birth_kind: "chat_budget_spent", at: "2026-10-28T22:20:00Z",
    content: "The chat's budget is used up."}, {budget_eur: 15, spent_eur: 14.96, suggest_eur: 3, can_fund: false}),
  fundOnNewest: (function () {
    var rows = [{id: 1, role: "user", content: "what is open?", at: "2026-10-28T22:19:00Z"},
      {id: 2, role: "assistant", kind: "budget", birth_kind: "chat_budget_spent", content: "first stop", at: "2026-10-28T22:20:00Z"},
      {id: 3, role: "user", content: "and now?", at: "2026-10-28T22:21:00Z"},
      {id: 4, role: "assistant", kind: "poke", birth_kind: "chat_budget_spent", content: "second stop", at: "2026-10-28T22:22:00Z"},
      {id: 5, role: "assistant", kind: "budget", content: "the month cap", at: "2026-10-28T22:23:00Z"}];
    var stopped = chatView.historyHTML(rows, "sebastian", [], {budget_eur: 15, spent_eur: 14.96, suggest_eur: 3, can_fund: true});
    var funded = chatView.historyHTML(rows, "sebastian", []);
    return JSON.stringify([stopped.split("data-fund").length - 1, stopped.indexOf("data-fund") > stopped.indexOf("second stop"), funded.indexOf("data-fund")]);
  })()
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
		args     = `{&quot;title&quot;:&quot;Cache metrics&quot;,&quot;track&quot;:&quot;direct&quot;}`
		result   = `Proposed to sebastian: File a card, title Cache metrics, track direct. Nothing runs until they press Do it.`
		proposal = `<div class="proposal" data-proposal="tok1"><p class="ptitle">I will:</p><ul><li>File a card, title Cache metrics, track direct</li></ul>` +
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
		{"unread", "3"},
		{"inboxHead", `<h3 class="ttl"><span class="tmark inbox" aria-hidden="true">⌂</span>Inbox</h3><span class="thint">the factory's thread: it speaks here uninvited, you answer with its buttons</span>`},
		{"theirInboxHead", `<h3 class="ttl"><span class="tmark inbox" aria-hidden="true">⌂</span>Inbox · alice's</h3><span class="thint">the factory's thread: it speaks here uninvited, you answer with its buttons</span>`},
		{"head", `<h3 class="ttl"><button type="button" class="rename" data-rename title="Rename the thread">world</button></h3><span class="thint when">09:30</span>`},
		{"headActs", `<h3 class="ttl"><button type="button" class="rename" data-rename title="Rename the thread">world</button></h3><span class="thint when">09:30</span>` +
			`<span class="tacts"><button type="button" data-clear title="Hide the old rows from view; the record keeps them">Clear old</button>` +
			`<button type="button" data-archive>Archive</button><button type="button" class="danger" data-delete>Delete…</button></span>`},
		{"headCleared", `<h3 class="ttl"><button type="button" class="rename" data-rename title="Rename the thread">world</button></h3><span class="thint when">09:30</span>` +
			`<span class="tacts"><button type="button" data-clear disabled title="Hide the old rows from view; the record keeps them">Clear old</button>` +
			`<button type="button" data-archive>Archive</button><button type="button" class="danger" data-delete>Delete…</button></span>`},
		{"pendingRow", `[1,1,null,null,null]`},
		{"answerRow", `<span class="answer-l">Answer:</span><button type="button" data-answer="Postgres">Postgres</button>` +
			`<button type="button" data-answer="a &lt;b&gt;">a &lt;b&gt;</button><span class="answer-or">or in your words below</span>`},
		{"askAnswerRow", `<span class="answer-l">Answer:</span><button type="button" data-answer="One card">One card</button><span class="answer-or">or in your words below</span>`},
		{"inboxAnswerRow", `<span class="answer-l">Answer:</span><button type="button" data-answer="One a night">One a night</button><button type="button" data-answer="One an hour">One an hour</button>`},
		{"levelMenu", `<p class="lp-head"><span>autonomy</span><span>one click sets it</span></p>` +
			`<button type="button" role="menuitemradio" aria-checked="false" data-lvl="manual"><i aria-hidden="true"></i><b>manual</b><span class="ck" aria-hidden="true"></span><span class="lw">every action confirms</span></button>` +
			`<button type="button" role="menuitemradio" aria-checked="true" data-lvl="assisted"><i aria-hidden="true"></i><b>assisted</b><span class="ck" aria-hidden="true">✓</span><span class="lw">a &lt;b&gt;</span></button>`},
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
			`<p class="choices"><button type="button" data-answer="Open the card">Open the card</button></p></div>`},
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
		{"calls", `<div class="msg you"><span class="av" aria-hidden="true">S</span><p class="who"><span class="name">sebastian</span> <time>10-11 09:30Z</time></p><p class="said">look</p></div>` +
			agent + `<details class="calls"><summary>2 earlier calls</summary>` +
			`<details class="tool"><summary>🔧 board · 3 cards</summary><pre>{}` + "\n\n" + `3 cards</pre></details>` +
			`<p class="refused"><code>card</code> Refused: no card 9</p></details>` +
			`<details class="tool"><summary>🔧 ci · green</summary><pre>{}` + "\n\n" + `green</pre></details>` +
			`<div class="said"><p>All green.</p></div></div>`},
		{"trail", `<div class="msg you"><span class="av" aria-hidden="true">S</span><p class="who"><span class="name">sebastian</span> <time>10-11 09:30Z</time></p><p class="said">which files?</p></div>` +
			agent + `<details class="tool"><summary>🔧 research · Findings:</summary><pre>{&quot;ask&quot;:&quot;which files?&quot;}` + "\n\n" +
			`Findings:` + "\n" + `main holds README.md.</pre><details class="trail"><summary>the researcher's steps</summary><pre>` +
			"\n" + `tree {&quot;ref&quot;:&quot;main&quot;}` + "\n" + `README.md 1200` + "\n" + `</pre></details></details>` +
			`<div class="said"><p>Done.</p></div></div>`},
		{"queuedRow", `<div class="msg you"><span class="av" aria-hidden="true">S</span><p class="who"><span class="name">sebastian</span> <time>10-11 09:30Z</time></p>` +
			`<p class="said">also this</p><p class="meta chat-queued">not read yet: the agent reads it after its current step</p></div>`},
		{"tokenThread", `<div class="msg you tok"><span class="av" aria-hidden="true">S</span><p class="who"><span class="name">sebastian</span>` +
			` <span class="via" title="sent by the chat token claude-&lt;b&gt;">via claude-&lt;b&gt;</span> <time>10-11 09:30Z</time></p>` +
			`<p class="said">what waits?</p></div>` +
			`<div class="msg you"><span class="av" aria-hidden="true">S</span><p class="who"><span class="name">sebastian</span> <time>10-11 09:31Z</time></p>` +
			`<p class="said">and now</p></div>`},
		{"imgRow", `<div class="msg you"><span class="av" aria-hidden="true">S</span><p class="who"><span class="name">sebastian</span> <time>10-11 09:30Z</time></p>` +
			`<p class="said">look at the run</p><div class="chat-thumbs">` +
			`<a class="thumb" href="/projects/ai-software-factory-code/chat/attachments/901" target="_blank" rel="noopener">` +
			`<img loading="lazy" src="/projects/ai-software-factory-code/chat/attachments/901" alt="img1"></a></div></div>`},
		{"zoom", `<div id="img-zoom" class="overlay imgbox" role="dialog" aria-modal="true" aria-label="the image full size; click or Escape closes">` +
			`<img src="/projects/ai-software-factory-code/chat/attachments/901" alt="img1"></div>`},
		{"mark", `<span class="turn-tail"><button type="button" class="tthink" data-think title="Show the thinking" aria-expanded="false" hidden></button>` +
			`<button type="button" class="thinking" data-stop aria-label="Stop" title="Stop">thinking</button><div class="tfold" hidden></div></span>`},
		{"stoppedMark", `<span class="thinking stopped" aria-hidden="true">stopped</span>`},
		{"stopRow", `<div class="msg agent poke" data-message="13"><span class="av" aria-hidden="true"></span>` +
			`<p class="who"><span class="name">the factory</span> <time>10-11 09:20Z</time></p>` +
			`<div class="said"><p>Stopped by sebastian.</p></div></div>`},
		{"stoppedThread", `<div class="msg you"><span class="av" aria-hidden="true">S</span><p class="who"><span class="name">sebastian</span> <time>10-11 09:30Z</time></p><p class="said">what is open?</p></div>` +
			`<div class="msg agent"><span class="av" aria-hidden="true"></span><p class="who"><span class="name">project agent</span> <time>10-11 09:30Z</time></p>` +
			`<div class="said"><p>Partial</p></div><span class="thinking stopped" aria-hidden="true">stopped</span></div>` +
			`<div class="msg agent poke" data-message="3"><span class="av" aria-hidden="true"></span><p class="who"><span class="name">the factory</span> <time>10-11 09:30Z</time></p>` +
			`<div class="said"><p>Stopped by sebastian.</p></div></div>`},
		{"noFoldInHistory", `false`},
		{"readRow", `<p class="meta chat-read">✓ read by the agent</p>`},
		{"openQ", `<p class="meta chat-open">question that may be open: Can you do that? &lt;b&gt;</p>|`},
		{"share", `[384,384,720,500.25]`},
		{"budgetRow", `<div class="msg agent poke" data-message="12"><span class="av" aria-hidden="true"></span><p class="who"><span class="name">the factory</span> <time>10-28 22:20Z</time></p>` +
			`<div class="said"><p>The chat's budget for October is used up: €14.96 of €15.00 spent, and the next call does not fit what is left.<br>free headroom 0.038793 EUR</p></div>` +
			`<form class="choices fund" data-fund><label>Raise the chat's budget by, EUR <input type="number" name="eur" step="0.01" min="0.01" required value="3"></label> ` +
			`<button type="submit">Fund and go on</button></form></div>`},
		{"budgetRowWriter", `<div class="msg agent poke" data-message="12"><span class="av" aria-hidden="true"></span><p class="who"><span class="name">the factory</span> <time>10-28 22:20Z</time></p>` +
			`<div class="said"><p>The chat's budget is used up.</p></div><p class="settled-why">Only an approver of the project funds the chat.</p></div>`},
		{"fundOnNewest", `[1,true,-1]`},
	} {
		if got[c.name] != c.want {
			t.Errorf("%s =\n%s\nwant\n%s", c.name, got[c.name], c.want)
		}
	}
}

// The panel refreshes itself wherever the person reads it: an ask
// arriving over the page's stream settles the list (the tab title's
// count with it), the tab coming back to the front reads the list at
// once, and the ten-second tick keeps reading it in a hidden tab, so
// the count cannot sit stale until a reload. A settling event in a
// hidden tab reads the list but not the open thread, which the tab
// coming back reads once; a reopened stream reads both.
func TestChatPanelRefreshesItself(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var fs = require("fs"), vm = require("vm");
var fetched = [], listeners = {}, intervals = [];
function el() {
  return {textContent: "", innerHTML: "", hidden: false, className: "", value: "",
    dataset: {login: "sebastian", project: "p"}, style: {setProperty: function () {}},
    classList: {add: function () {}, remove: function () {}, toggle: function () {}, contains: function () { return false; }},
    addEventListener: function () {}, setAttribute: function () {}, focus: function () {},
    insertBefore: function () {}, firstChild: null,
    querySelector: function () { return el(); }, querySelectorAll: function () { return []; },
    getBoundingClientRect: function () { return {left: 0, width: 0}; }, clientWidth: 0};
}
var document = {title: "Factory console", hidden: false,
  createElement: function () { return el(); },
  getElementById: function (id) { return (id === "screen" || id === "chat-divider") ? null : el(); },
  addEventListener: function (kind, fn) { (listeners[kind] = listeners[kind] || []).push(fn); },
  querySelector: function () { return el(); }, querySelectorAll: function () { return []; },
  documentElement: el()};
var sandbox = {document: document, console: {log: function () {}, error: function () {}},
  fetch: function (url) { fetched.push(String(url)); return Promise.resolve({ok: true, json: function () {
    return Promise.resolve(String(url).endsWith("/conversations") ? [] : {conversation: 3, messages: [], thread: {number: 3, title: "", state: "active"}}); }}); },
  setInterval: function (fn) { intervals.push(fn); return intervals.length; },
  setTimeout: function () { return 0; }, clearTimeout: function () {}, clearInterval: function () {},
  localStorage: {getItem: function () { return null; }, setItem: function () {}},
  URL: {createObjectURL: function () { return ""; }},
  getComputedStyle: function () { return {fontSize: "16px"}; }};
sandbox.window = sandbox;
vm.createContext(sandbox);
vm.runInNewContext(fs.readFileSync(process.argv[1], "utf8"), sandbox);
var drain = function () { return new Promise(function (res) { setImmediate(res); }).then(function () {
  return new Promise(function (res) { setImmediate(res); }); }); };
var sawList = function () { return fetched.some(function (u) { return u.indexOf("/conversations", u.length - 14) !== -1; }); };
var sawThread = function () { return fetched.some(function (u) { return u.indexOf("?conversation=3") !== -1; }); };
var fire = function (kind, e) { (listeners[kind] || []).forEach(function (fn) { fn(e); }); };
var out = {};
Promise.resolve().then(drain).then(function () {
  fetched.length = 0;
  (listeners["factoryevent"] || []).forEach(function (fn) { fn({detail: {kind: "question_asked"}}); });
}).then(drain).then(function () {
  out.asked = sawList();
  fetched.length = 0;
  document.hidden = false;
  (listeners["visibilitychange"] || []).forEach(function (fn) { fn({}); });
}).then(drain).then(function () {
  out.visible = sawList();
  fetched.length = 0;
  document.hidden = true;
  intervals.forEach(function (fn) { fn(); });
}).then(drain).then(function () {
  out.hiddenTick = sawList();
  fetched.length = 0;
  fire("factoryevent", {detail: {kind: "question_answered"}});
}).then(drain).then(function () {
  out.hiddenEvent = [sawList(), sawThread()];
  fetched.length = 0;
  document.hidden = false;
  fire("visibilitychange", {});
}).then(drain).then(function () {
  out.back = sawThread();
  fetched.length = 0;
  document.hidden = true;
  fire("visibilitychange", {});
  document.hidden = false;
  fire("visibilitychange", {});
}).then(drain).then(function () {
  out.backAgain = sawThread();
  fetched.length = 0;
  fire("streamopen", {});
}).then(drain).then(function () {
  out.reopened = [sawList(), sawThread()];
  console.log(JSON.stringify(out));
});`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver, "static/chat.js")
	cmd.Env = append(os.Environ(), "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got struct {
		Asked, Visible, HiddenTick, Back, BackAgain bool
		HiddenEvent, Reopened                       []bool
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node said %s: %v", out, err)
	}
	if !slices.Equal(got.HiddenEvent, []bool{true, false}) {
		t.Errorf("a settling event in a hidden tab read the list and the thread: %v, want [true false]", got.HiddenEvent)
	}
	if !got.Back {
		t.Error("the tab coming back does not read the thread a hidden event passed")
	}
	if got.BackAgain {
		t.Error("the tab coming back again reads the thread with no event passed")
	}
	if !slices.Equal(got.Reopened, []bool{true, true}) {
		t.Errorf("a reopened stream read the list and the thread: %v, want [true true]", got.Reopened)
	}
	if !got.Asked {
		t.Error("an ask arriving over the stream does not settle the thread list")
	}
	if !got.Visible {
		t.Error("the tab coming back to the front does not read the thread list")
	}
	if !got.HiddenTick {
		t.Error("the ten-second tick skips a hidden tab")
	}
}

// A paste grows the composer's input with its content; sending or
// clearing it takes the height back to the base the rows give, so the
// expanded state never outlives the message that caused it. Run under
// node, which the test needs.
func TestChatPanelComposerHeight(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var fs = require("fs"), vm = require("vm");
var listeners = {};
function el(id) {
  return {id: id, textContent: "", innerHTML: "", hidden: false, className: "", value: "",
    dataset: {login: "sebastian", project: "p"}, style: {},
    classList: {add: function () {}, remove: function () {}, toggle: function () {}, contains: function () { return false; }},
    _handlers: {},
    addEventListener: function (kind, fn) { this._handlers[kind] = (this._handlers[kind] || []).concat(fn); },
    setAttribute: function () {}, removeAttribute: function () {}, focus: function () {}, click: function () {},
    appendChild: function () {}, insertBefore: function () {}, before: function () {},
    firstChild: null,
    querySelector: function () { return null; }, querySelectorAll: function () { return []; },
    getBoundingClientRect: function () { return {left: 0, width: 0, height: 0}; },
    clientWidth: 0, clientHeight: 60, scrollHeight: 60, scrollTop: 0,
    contains: function () { return false; }, disabled: false};
}
var input = el("chat-input"), form = el("chat-form"), log = el("chat-log");
form.elements = {csrf: {value: "t"}};
form.querySelectorAll = function () { return []; };
var byID = {chat: el("chat"), "chat-log": log, "chat-form": form, "chat-input": input,
  "chat-new": el("chat-new"), "thread-rows": el("thread-rows"), "thread-head": el("thread-head"),
  "chat-answer": el("chat-answer"), "chat-commands": {textContent: "[]"},
  "chat-palette": el("chat-palette"), "chat-images": el("chat-images"), "chat-docs": el("chat-docs"),
  "chat-full": el("chat-full"), screen: null, "chat-divider": null, "thread-dialog": null,
  "img-zoom": null};
var document = {title: "Factory console", hidden: false,
  createElement: function (tag) {
    if (tag === "template") {
      var c = el(), kid = el(), chev = el(), fold = el();
      kid.querySelector = function (sel) { return sel === "[data-think]" ? chev : sel === ".tfold" ? fold : null; };
      c.firstElementChild = kid;
      return {content: c};
    }
    return el();
  },
  getElementById: function (id) { return byID[id] || null; },
  addEventListener: function (kind, fn) { (listeners[kind] = listeners[kind] || []).push(fn); },
  querySelector: function () { return el(); }, querySelectorAll: function () { return []; },
  documentElement: el()};
var sandbox = {document: document, console: {log: function () {}, error: function () {}},
  fetch: function () { return new Promise(function () {}); },
  setInterval: function () { return 0; }, setTimeout: function () { return 0; }, clearTimeout: function () {}, clearInterval: function () {},
  localStorage: {getItem: function () { return null; }, setItem: function () {}},
  URL: {createObjectURL: function () { return ""; }},
  FormData: function () { this.append = function () {}; },
  TextDecoder: function () { this.decode = function () { return ""; }; },
  getComputedStyle: function () { return {fontSize: "16px"}; }};
sandbox.window = sandbox;
sandbox.window.innerHeight = 800;
sandbox.window.matchMedia = function () { return {matches: false}; };
vm.createContext(sandbox);
vm.runInNewContext(fs.readFileSync(process.argv[1], "utf8"), sandbox);
var grow = function () {
  input.value = "line\n".repeat(40) + "send me";
  input.scrollHeight = 920;
  (input._handlers.input || []).forEach(function (fn) { fn({}); });
};
var out = {};
grow();
out.pasteGrew = input.style.height;
(form._handlers.submit || []).forEach(function (fn) { fn({preventDefault: function () {}}); });
out.sendEmptied = input.value === "";
out.sendBaseHeight = input.style.height === "";
grow();
(form._handlers.click || []).forEach(function (fn) {
  fn({target: {closest: function (sel) { return sel === "[data-cmd]" ? {dataset: {cmd: "new"}} : null; }}});
});
out.commandEmptied = input.value === "";
out.commandBaseHeight = input.style.height === "";
console.log(JSON.stringify(out));`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver, "static/chat.js")
	cmd.Env = append(os.Environ(), "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got struct {
		PasteGrew         string
		SendEmptied       bool
		SendBaseHeight    bool
		CommandEmptied    bool
		CommandBaseHeight bool
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node said %s: %v", out, err)
	}
	if got.PasteGrew == "" {
		t.Error("a large paste does not grow the input")
	}
	if !got.SendEmptied {
		t.Error("send does not empty the input")
	}
	if !got.SendBaseHeight {
		t.Errorf("an input emptied by send keeps the grown height %s", got.PasteGrew)
	}
	if !got.CommandEmptied {
		t.Error("a command does not empty the input")
	}
	if !got.CommandBaseHeight {
		t.Errorf("an input emptied by a command keeps the grown height %s", got.PasteGrew)
	}
}

// A thumbnail's click opens its image full size over the page instead
// of leaving the screen for the serving route — which answers as a
// download — and a click on the dimmed page, the image or Escape closes
// it again. The link itself stays, so opening it in a new tab works.
func TestChatPanelZoomsThumbs(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var fs = require("fs"), vm = require("vm");
var listeners = {}, zoom = null, zoomHTML = "";
function el() {
  return {textContent: "", innerHTML: "", hidden: false, className: "", value: "",
    dataset: {login: "sebastian", project: "p"}, style: {setProperty: function () {}},
    classList: {add: function () {}, remove: function () {}, toggle: function () {}, contains: function () { return false; }},
    addEventListener: function () {}, setAttribute: function () {}, focus: function () {},
    insertBefore: function () {}, firstChild: null,
    querySelector: function () { return el(); }, querySelectorAll: function () { return []; },
    getBoundingClientRect: function () { return {left: 0, width: 0}; }, clientWidth: 0};
}
var log = el();
log.contains = function () { return true; };
var thumb = {href: "/projects/p/chat/attachments/901",
  querySelector: function () { return {alt: "img1"}; },
  closest: function (sel) { return sel === "a.thumb" ? thumb : null; }};
var document = {title: "Factory console", hidden: false,
  createElement: function () { return el(); },
  getElementById: function (id) {
    if (id === "img-zoom") return zoom;
    if (id === "chat-log") return log;
    if (id === "thread-dialog") return null;
    return (id === "screen" || id === "chat-divider") ? null : el();
  },
  addEventListener: function (kind, fn) { (listeners[kind] = listeners[kind] || []).push(fn); },
  querySelector: function () { return el(); }, querySelectorAll: function () { return []; },
  documentElement: el(),
  body: {insertAdjacentHTML: function (where, html) {
    zoom = el();
    zoom.contains = function (t) { return t === zoom; };
    zoom.closest = function () { return null; };
    zoom.remove = function () { zoom = null; };
    zoomHTML = html;
  }}};
var sandbox = {document: document, console: {log: function () {}, error: function () {}},
  fetch: function (url) { return Promise.resolve({ok: true, json: function () {
    return Promise.resolve(String(url).endsWith("/conversations") ? [] : {conversation: 3, messages: [], thread: {number: 3, title: "", state: "active"}}); }}); },
  setInterval: function () { return 0; }, setTimeout: function () { return 0; }, clearTimeout: function () {}, clearInterval: function () {},
  localStorage: {getItem: function () { return null; }, setItem: function () {}},
  URL: {createObjectURL: function () { return ""; }},
  getComputedStyle: function () { return {fontSize: "16px"}; }};
sandbox.window = sandbox;
vm.createContext(sandbox);
vm.runInNewContext(fs.readFileSync(process.argv[1], "utf8"), sandbox);
var fire = function (kind, e) { (listeners[kind] || []).forEach(function (fn) { fn(e); }); };
var click = function (target) {
  var prevented = false;
  fire("click", {target: target, preventDefault: function () { prevented = true; }, stopPropagation: function () {}});
  return prevented;
};
var out = {};
Promise.resolve().then(function () {
  out.keptHref = !click(thumb);
  out.opened = !!zoom && zoomHTML.indexOf('src="/projects/p/chat/attachments/901"') !== -1 && zoomHTML.indexOf('alt="img1"') !== -1;
}).then(function () {
  out.closedClick = !click(zoom) && !zoom;
}).then(function () {
  click(thumb);
  fire("keydown", {key: "Escape", stopPropagation: function () {}});
  out.closedEscape = !zoom;
}).then(function () {
  console.log(JSON.stringify(out));
});`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver, "static/chat.js")
	cmd.Env = append(os.Environ(), "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got struct {
		Opened, ClosedClick, ClosedEscape, KeptHref bool
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node said %s: %v", out, err)
	}
	if !got.Opened {
		t.Error("a thumbnail's click does not open its image full size over the page")
	}
	if !got.ClosedClick {
		t.Error("a click on the zoomed image does not close it")
	}
	if !got.ClosedEscape {
		t.Error("Escape does not close the zoomed image")
	}
	if got.KeptHref {
		t.Error("the thumbnail's click follows its link instead of staying in the page")
	}
}

// The panel stops a turn in flight (the chat-stop plan): a press on the
// thinking mark's button and Escape in the chat's input and panel each
// post the open thread's stop with its number and owner, Escape with no
// turn in flight only leaves the input, and a press on a page that drew
// the button from the thread's working flag — no stream open — reloads
// the thread to show the rows the stop left. Run under node, which the
// test needs.
func TestChatPanelStopsATurn(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var fs = require("fs"), vm = require("vm");
var listeners = [], posts = [], gets = 0, made = [], stopFails = false, byId = {};
function el() {
  var e = {textContent: "", innerHTML: "", hidden: false, className: "", value: "", disabled: false, blurred: false,
    dataset: {login: "sebastian", project: "p"}, style: {setProperty: function () {}},
    classList: {add: function () {}, remove: function () {}, toggle: function () {}, contains: function () { return false; }},
    setAttribute: function () {}, focus: function () {}, blur: function () { e.blurred = true; }, remove: function () {},
    appendChild: function (c) { return c; }, insertBefore: function (c) { return c; }, contains: function () { return true; },
    closest: function () { return null; }, firstChild: null, previousElementSibling: null,
    querySelector: function () { return el(); }, querySelectorAll: function () { return []; },
    getBoundingClientRect: function () { return {left: 0, width: 0}; }, clientWidth: 0};
  e.addEventListener = function (kind, fn) { listeners.push({el: e, kind: kind, fn: fn}); };
  return e;
}
var document = {title: "Factory console", hidden: false,
  createElement: function (tag) { var t = el();
    if (tag === "template") { var html = ""; t.content = {firstElementChild: el()};
      Object.defineProperty(t, "innerHTML", {get: function () { return html; }, set: function (v) { html = v; made.push(v); }}); }
    return t; },
  getElementById: function (id) {
    if (id === "img-zoom" || id === "thread-dialog" || id === "screen" || id === "chat-divider") return null;
    return byId[id] || (byId[id] = el());
  },
  addEventListener: function (kind, fn) { listeners.push({el: document, kind: kind, fn: fn}); },
  querySelector: function () { return el(); }, querySelectorAll: function () { return []; },
  documentElement: el(), body: el(), activeElement: null};
byId["chat-form"] = el();
byId["chat-form"].elements = {csrf: {value: "tok"}};
var sandbox = {document: document, console: {log: function () {}, error: function () {}},
  fetch: function (url, opts) {
    var u = String(url);
    if (opts && opts.method === "POST") {
      if (/\/chat\/stop$/.test(u)) {
        posts.push({url: u, body: String(opts.body), csrf: opts.headers["X-CSRF-Token"]});
        return stopFails ? Promise.reject(new Error("the stop did not reach the console")) : Promise.resolve({ok: true});
      }
      return new Promise(function () {}); // the turn that never ends
    }
    if (/\/chat(\?|$)/.test(u)) gets++;
    return Promise.resolve({ok: true, json: function () {
      return Promise.resolve(/\/conversations$/.test(u) ? [] :
        {conversation: 3, messages: [], thread: {number: 3, title: "", state: "active", working: true}});
    }});
  },
  setInterval: function () { return 0; }, setTimeout: function () { return 0; }, clearTimeout: function () {}, clearInterval: function () {},
  localStorage: {getItem: function () { return null; }, setItem: function () {}},
  URL: {createObjectURL: function () { return ""; }}, FormData: function () { return {append: function () {}}; },
  URLSearchParams: function (p) { this.toString = function () { return Object.keys(p).map(function (k) { return k + "=" + p[k]; }).join("&"); }; },
  getComputedStyle: function () { return {fontSize: "16px"}; }};
sandbox.window = sandbox;
vm.createContext(sandbox);
vm.runInNewContext(fs.readFileSync(process.argv[1], "utf8"), sandbox);
var fire = function (on, kind, e) { listeners.forEach(function (l) { if (l.el === on && l.kind === kind) l.fn(e); }); };
var stopButton = function () { return {closest: function (sel) { return sel === "[data-stop]" ? this : null; }}; };
var tick = function () { return new Promise(function (done) { setTimeout(done, 0); }); };
var out = {};
Promise.resolve().then(function () {
  fire(byId["chat-input"], "keydown", {key: "Escape", stopPropagation: function () {}});
  out.idleEscapeLeaves = byId["chat-input"].blurred && posts.length === 0;
}).then(function () {
  return tick(); // the opening read settles: the thread takes its number
}).then(function () {
  fire(byId["chat-log"], "click", {target: stopButton()});
  out.stopWithoutStream = posts.length === 1 && posts[0].url === "/projects/p/chat/stop" &&
    posts[0].body === "conversation=3&owner=" && posts[0].csrf === "tok";
  return tick(); // the stop with no stream open reloads the thread
}).then(function () {
  out.reloadedAfterStop = gets === 2;
  byId["chat-input"].value = "what is open?";
  fire(byId["chat-form"], "submit", {preventDefault: function () {}});
  return tick(); // the turn starts and streams
}).then(function () {
  fire(byId["chat-log"], "click", {target: stopButton()});
  out.stopByClick = posts.length === 2;
}).then(function () {
  fire(byId["chat-input"], "keydown", {key: "Escape", stopPropagation: function () {}});
  out.stopByEscapeInInput = posts.length === 3;
}).then(function () {
  fire(byId["chat"], "keydown", {key: "Escape", stopPropagation: function () {}});
  out.stopByEscapeInPanel = posts.length === 4;
}).then(function () {
  stopFails = true;
  fire(byId["chat-log"], "click", {target: stopButton()});
  return tick().then(tick); // the failed stop's error row lands in the log
}).then(function () {
  out.failedStopShowsError = made.some(function (h) { return /chat-error/.test(h) && /stop did not reach/.test(h); });
}).then(function () {
  console.log(JSON.stringify(out));
});`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver, "static/chat.js")
	cmd.Env = append(os.Environ(), "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got struct {
		IdleEscapeLeaves, StopWithoutStream, ReloadedAfterStop, StopByClick, StopByEscapeInInput, StopByEscapeInPanel, FailedStopShowsError bool
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node said %s: %v", out, err)
	}
	if !got.IdleEscapeLeaves {
		t.Error("Escape with no turn in flight does not only leave the input")
	}
	if !got.StopWithoutStream {
		t.Error("a press on the mark's button posts no stop of the working thread")
	}
	if !got.ReloadedAfterStop {
		t.Error("a stop with no stream open does not reload the thread")
	}
	if !got.StopByClick {
		t.Error("a press on the mark's button during a turn posts no stop")
	}
	if !got.StopByEscapeInInput {
		t.Error("Escape in the chat's input during a turn posts no stop")
	}
	if !got.StopByEscapeInPanel {
		t.Error("Escape elsewhere in the chat panel during a turn posts no stop")
	}
	if !got.FailedStopShowsError {
		t.Error("a stop POST that fails shows no error row in the open thread")
	}
}

// The fold shows the live thinking of the turn in flight (the
// thinking-fold plan): the chevron beside the stop mark stays hidden
// until reasoning arrives, a press opens the fold with the thinking so
// far, a delta past 16 KiB trims the front at a line boundary and keeps
// the fold scrolled to the newest line, and the turn's end takes the
// chevron, the fold and the mark with it, for nothing is stored. Run
// under node, which the test needs.
func TestChatPanelFoldsThinking(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var fs = require("fs"), vm = require("vm");
var listeners = [], queries = [], templates = [], byId = {};
var chunks = [], waiting = [], ended = false;
var reader = {read: function () {
  if (chunks.length) return Promise.resolve({done: false, value: chunks.shift()});
  if (ended) return Promise.resolve({done: true});
  return new Promise(function (res) { waiting.push(res); });
}};
function el() {
  var e = {textContent: "", innerHTML: "", hidden: false, className: "", value: "", disabled: false, blurred: false, gone: false,
    scrollTop: 0, scrollHeight: 0,
    dataset: {login: "sebastian", project: "p"}, style: {setProperty: function () {}}, attrs: {},
    classList: {add: function () {}, remove: function () {}, toggle: function () {}, contains: function () { return false; }},
    setAttribute: function (k, v) { e.attrs[k] = v; }, focus: function () {}, blur: function () { e.blurred = true; }, remove: function () { e.gone = true; },
    appendChild: function (c) { return c; }, insertBefore: function (c) { return c; }, contains: function () { return true; },
    closest: function () { return null; }, firstChild: null, previousElementSibling: null,
    querySelector: function (sel) { var r = el(); r.sel = sel; queries.push(r); return r; },
    querySelectorAll: function () { return []; },
    getBoundingClientRect: function () { return {left: 0, width: 0}; }, clientWidth: 0};
  e.addEventListener = function (kind, fn) { listeners.push({el: e, kind: kind, fn: fn}); };
  return e;
}
var document = {title: "Factory console", hidden: false,
  createElement: function (tag) { var t = el(); if (tag === "template") { t.content = {firstElementChild: el()}; templates.push(t); } return t; },
  getElementById: function (id) {
    if (id === "img-zoom" || id === "thread-dialog" || id === "screen" || id === "chat-divider") return null;
    return byId[id] || (byId[id] = el());
  },
  addEventListener: function (kind, fn) { listeners.push({el: document, kind: kind, fn: fn}); },
  querySelector: function () { return el(); }, querySelectorAll: function () { return []; },
  documentElement: el(), body: el(), activeElement: null};
byId["chat-form"] = el();
byId["chat-form"].elements = {csrf: {value: "tok"}};
var sandbox = {document: document, console: {log: function () {}, error: function () {}},
  fetch: function (url, opts) {
    var u = String(url);
    if (opts && opts.method === "POST") {
      if (/\/chat\/stop$/.test(u) || /\/chat\/seen$/.test(u)) return Promise.resolve({ok: true});
      return Promise.resolve({ok: true, headers: {get: function () { return "text/event-stream"; }}, body: {getReader: function () { return reader; }}});
    }
    return Promise.resolve({ok: true, json: function () {
      return Promise.resolve(/\/conversations$/.test(u) ? [] :
        {conversation: 3, messages: [], thread: {number: 3, title: "", state: "active", working: false}});
    }});
  },
  setInterval: function () { return 0; }, setTimeout: function () { return 0; }, clearTimeout: function () {}, clearInterval: function () {},
  localStorage: {getItem: function () { return null; }, setItem: function () {}},
  URL: {createObjectURL: function () { return ""; }}, FormData: function () { return {append: function () {}}; },
  URLSearchParams: function (p) { this.toString = function () { return Object.keys(p).map(function (k) { return k + "=" + p[k]; }).join("&"); }; },
  TextDecoder: function () { this.decode = function (v) { return v == null ? "" : String(v); }; },
  getComputedStyle: function () { return {fontSize: "16px"}; }};
sandbox.window = sandbox;
vm.createContext(sandbox);
vm.runInNewContext(fs.readFileSync(process.argv[1], "utf8"), sandbox);
var fire = function (on, kind, e) { listeners.forEach(function (l) { if (l.el === on && l.kind === kind) l.fn(e); }); };
var give = function (text) { chunks.push(text); waiting.splice(0).forEach(function (res) { res({done: false, value: chunks.shift()}); }); };
var finish = function () { ended = true; waiting.splice(0).forEach(function (res) { res({done: true}); }); };
var last = function (sel) { var q = queries.filter(function (r) { return r.sel === sel; }); return q.length ? q[q.length - 1] : null; };
var tail = function () { var t = templates.filter(function (x) { return x.innerHTML.indexOf("turn-tail") >= 0; }); return t.length ? t[t.length - 1].content.firstElementChild : null; };
var tick = function () { return new Promise(function (done) { setTimeout(done, 0); }); };
var big = "";
for (var i = 0; i < 600; i++) big += "line " + i + " of the reasoning trail\n";
big += "the tail line";
var out = {}, chev = null, fold = null, cursor = null;
Promise.resolve().then(function () {
  return tick(); // the opening read settles: the thread takes its number
}).then(function () {
  byId["chat-input"].value = "what is open?";
  fire(byId["chat-form"], "submit", {preventDefault: function () {}});
  return tick(); // the turn starts: its tail is drawn, both controls closed
}).then(function () {
  chev = last("[data-think]"); fold = last(".tfold"); cursor = tail();
  out.turnStartsClosed = !!chev && !!fold && !!cursor && chev.hidden === true && fold.hidden === true;
  give("event: think\ndata: {\"text\":\"Figuring \"}\n\n" + "event: think\ndata: {\"text\":\"what is open.\"}\n\n");
  return tick(); // the reasoning arrives
}).then(function () {
  out.chevronShowsOnThinking = !!chev && chev.hidden === false && !!fold && fold.hidden === true;
  if (chev) fire(chev, "click", {});
  out.pressOpensFold = !!fold && !!chev && fold.hidden === false && fold.textContent === "Figuring what is open." && chev.attrs["aria-expanded"] === "true";
  if (fold) fold.scrollHeight = 5000;
  give("event: think\ndata: " + JSON.stringify({text: big}) + "\n\n");
  return tick(); // the window passes 16 KiB: the front falls off at a line
}).then(function () {
  out.foldTrimsAtLine = !!fold && fold.textContent.length <= 16384 && /^line \d+ /.test(fold.textContent) &&
    fold.textContent.slice(-13) === "the tail line";
  out.foldScrolls = !!fold && fold.scrollTop === 5000;
  fire(chev, "click", {});
  out.pressClosesFold = !!fold && !!chev && fold.hidden === true && chev.attrs["aria-expanded"] === "false";
  fire(chev, "click", {});
  give("event: done\ndata: {}\n\n");
  finish();
  return tick(); // the stream ends: the tail goes, nothing is stored
}).then(function () {
  out.tailGoneOnDone = !!cursor && cursor.gone === true;
  console.log(JSON.stringify(out));
});`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver, "static/chat.js")
	cmd.Env = append(os.Environ(), "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got struct {
		TurnStartsClosed, ChevronShowsOnThinking, PressOpensFold, FoldTrimsAtLine, FoldScrolls, PressClosesFold, TailGoneOnDone bool
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node said %s: %v", out, err)
	}
	if !got.TurnStartsClosed {
		t.Error("a turn's tail starts with the chevron and the fold closed")
	}
	if !got.ChevronShowsOnThinking {
		t.Error("reasoning arrives and the chevron does not show with the fold closed")
	}
	if !got.PressOpensFold {
		t.Error("a press on the chevron does not open the fold with the thinking so far")
	}
	if !got.FoldTrimsAtLine {
		t.Error("thinking past 16 KiB is not trimmed at a line boundary with the newest kept")
	}
	if !got.FoldScrolls {
		t.Error("the open fold does not follow the newest line")
	}
	if !got.PressClosesFold {
		t.Error("a second press does not close the fold")
	}
	if !got.TailGoneOnDone {
		t.Error("the turn's end leaves the chevron, the fold or the mark behind")
	}
}

// The composer gives its height back when its words go (the lab check's
// busy board): the input's handler grows the box while its words wrap,
// and words that leave shrink it back to its base, so a box emptied by
// hand never stands taller than its pane and over the board. A height
// the member set by the drag bar or its arrow keys is theirs: typing
// that fits inside it leaves it, and the bar's double-click still
// resets it. Run under node, which the test needs.
func TestTheComposerShrinksWhenItsWordsLeave(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var fs = require("fs"), vm = require("vm");
var byId = {}, madeBar = null;
function el(id) {
  var listeners = {};
  var e = {textContent: "", innerHTML: "", hidden: false, className: "", value: "", id: id || "",
    scrollHeight: 66, clientHeight: 66,
    dataset: {login: "sebastian", project: "p"}, style: {setProperty: function () {}},
    classList: {add: function () {}, remove: function () {}, toggle: function () {}, contains: function () { return false; }},
    addEventListener: function (kind, fn) { (listeners[kind] = listeners[kind] || []).push(fn); },
    fire: function (kind, ev) { (listeners[kind] || []).slice().forEach(function (fn) {
      fn(Object.assign({preventDefault: function () {}, stopPropagation: function () {}}, ev || {})); }); },
    setAttribute: function () {}, removeAttribute: function () {}, hasAttribute: function () { return false; },
    getAttribute: function () { return null; }, focus: function () {}, blur: function () {}, remove: function () {},
    insertBefore: function (n) { if (!e.firstChild) madeBar = n; }, firstChild: null,
    querySelector: function () { return el(); }, querySelectorAll: function () { return []; },
    getBoundingClientRect: function () { return {left: 0, width: 0, height: 66}; }};
  return e;
}
var input = el("chat-input"), form = el("chat-form");
var document = {title: "Factory console", hidden: false,
  createElement: function () { return el(); },
  getElementById: function (id) {
    if (id === "chat-input") return input;
    if (id === "chat-form") return form;
    return (id === "screen" || id === "chat-divider") ? null : byId[id] || (byId[id] = el(id));
  },
  addEventListener: function () {}, querySelector: function () { return el(); }, querySelectorAll: function () { return []; },
  documentElement: el()};
var sandbox = {document: document, console: {log: function () {}, error: function () {}},
  innerHeight: 900,
  fetch: function (url) { return Promise.resolve({ok: true, json: function () {
    return Promise.resolve(String(url).endsWith("/conversations") ? [] : {conversation: 3, messages: [], thread: {number: 3, title: "", state: "active"}}); }}); },
  setInterval: function () { return 0; }, setTimeout: function () { return 0; }, clearTimeout: function () {}, clearInterval: function () {},
  localStorage: {getItem: function () { return null; }, setItem: function () {}},
  URL: {createObjectURL: function () { return ""; }},
  getComputedStyle: function () { return {fontSize: "16px"}; }};
sandbox.window = sandbox;
vm.createContext(sandbox);
vm.runInNewContext(fs.readFileSync(process.argv[1], "utf8"), sandbox);
var out = {};
Promise.resolve().then(function () {
  input.scrollHeight = 440; input.clientHeight = 66;
  input.fire("input");
  out.grew = input.style.height === "440px";
  input.scrollHeight = 44; input.clientHeight = parseInt(input.style.height, 10) || 66;
  input.fire("input");
  out.shrank = input.style.height === "";
  var bar = madeBar;
  bar.fire("pointerdown", {clientY: 500, pointerId: 1});
  bar.fire("pointermove", {clientY: 200, pointerId: 1});
  bar.fire("pointerup", {});
  out.dragged = input.style.height === "366px";
  input.scrollHeight = 44; input.clientHeight = 366;
  input.fire("input");
  out.held = input.style.height === "366px";
  bar.fire("dblclick", {});
  out.reset = input.style.height === "";
}).then(function () {
  console.log(JSON.stringify(out));
});`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver, "static/chat.js")
	cmd.Env = append(os.Environ(), "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got struct {
		Grew, Shrank, Dragged, Held, Reset bool
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node said %s: %v", out, err)
	}
	if !got.Grew {
		t.Error("the composer does not grow while its words wrap")
	}
	if !got.Shrank {
		t.Error("the composer keeps its grown height after its words leave")
	}
	if !got.Dragged {
		t.Error("the drag bar does not size the composer")
	}
	if !got.Held {
		t.Error("typing that fits inside a dragged size snaps the composer back to its base")
	}
	if !got.Reset {
		t.Error("the bar's double-click no longer resets the composer")
	}
}

// The pool proposal's panel half (the pool-chat-controls plan): a
// set_pool offer renders the operation, the cent-precision EUR input
// holding the suggestion, the reason fixed at the capture and the
// ceiling the change would stand at. Rendering and Skip ask nothing; the
// panel's contextual Set the pool control — an approver's alone, a
// reader of the line for anyone else — starts a normal chat turn with
// the amount and the reason; a Do it on the edited amount posts exactly
// the correction object, the one number the row may change. A blank or
// malformed edit posts nothing and keeps the pending card; Enter in the
// edit form stays a keystroke, and a control press while a turn runs
// stays on the page. Run under node, which the test needs.
func TestPoolProposalPanel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var fs = require("fs"), vm = require("vm");
var listeners = [], posts = [], made = [], sends = [], byId = {}, poolHostEl = null;
var poolLine = {present: false, can_ask: true, words: "0 of 30 sessions, 0 h of 30 h agent time and €0.00 of €60.00 drawn this UTC month"};
function el() {
  var e = {textContent: "", innerHTML: "", hidden: false, className: "", value: "", disabled: false,
    dataset: {login: "sebastian", project: "p"}, style: {setProperty: function () {}},
    classList: {add: function () {}, remove: function () {}, toggle: function () {}, contains: function () { return false; }},
    setAttribute: function () {}, focus: function () {}, blur: function () {}, remove: function () { e.gone = true; },
    appendChild: function (c) { return c; }, insertBefore: function (c) { return c; }, after: function (c) { e.aftered = c; },
    contains: function () { return true; }, closest: function () { return null; }, firstChild: null, previousElementSibling: null,
    querySelector: function () { return el(); }, querySelectorAll: function () { return []; },
    getBoundingClientRect: function () { return {left: 0, width: 0}; }, clientWidth: 0};
  e.addEventListener = function (kind, fn) { listeners.push({el: e, kind: kind, fn: fn}); };
  return e;
}
var document = {title: "Factory console", hidden: false,
  createElement: function (tag) { var t = el();
    if (tag === "template") { var html = ""; t.content = {firstElementChild: el()};
      Object.defineProperty(t, "innerHTML", {get: function () { return html; }, set: function (v) { html = v; made.push(v); }}); }
    return t; },
  getElementById: function (id) {
    if (id === "img-zoom" || id === "thread-dialog" || id === "screen" || id === "chat-divider") return null;
    return byId[id] || (byId[id] = el());
  },
  addEventListener: function (kind, fn) { listeners.push({el: document, kind: kind, fn: fn}); },
  querySelector: function () { return el(); }, querySelectorAll: function () { return []; },
  documentElement: el(), body: el(), activeElement: null};
byId["chat-form"] = el();
byId["chat-form"].elements = {csrf: {value: "tok"}};
byId["chat-form"].parentNode = el();
byId["chat-form"].parentNode.insertBefore = function (n) { poolHostEl = n; return n; };
function sse(text) { var sent = false; return {read: function () {
  if (!sent) { sent = true; return Promise.resolve({done: false, value: text}); }
  return Promise.resolve({done: true}); }}; }
var sandbox = {document: document, console: {log: function () {}, error: function () {}},
  fetch: function (url, opts) {
    var u = String(url);
    if (opts && opts.method === "POST") {
      posts.push({url: u, body: opts.body === undefined ? "" : String(opts.body),
        type: (opts.headers || {})["Content-Type"] || "", csrf: (opts.headers || {})["X-CSRF-Token"]});
      if (/\/chat\/(seen|stop)$/.test(u)) return Promise.resolve({ok: true});
      return Promise.resolve({ok: true, headers: {get: function (h) { return String(h).toLowerCase() === "content-type" ? "text/event-stream" : null; }},
        body: {getReader: function () { return sse("event: done\ndata: {}\n\n"); }}});
    }
    return Promise.resolve({ok: true, json: function () {
      return Promise.resolve(/\/conversations$/.test(u) ? [] :
        {conversation: 3, messages: [], thread: {number: 3, title: "", state: "active"}, pool: poolLine}); }});
  },
  setInterval: function () { return 0; }, setTimeout: function (done) { done(); return 0; }, clearTimeout: function () {}, clearInterval: function () {},
  localStorage: {getItem: function () { return null; }, setItem: function () {}},
  URL: {createObjectURL: function () { return ""; }},
  FormData: function () { var f = {parts: []}; f.append = function (k, v) { f.parts.push([k, String(v)]); }; sends.push(f); return f; },
  URLSearchParams: function (p) { this.toString = function () { return Object.keys(p).map(function (k) { return k + "=" + p[k]; }).join("&"); }; },
  TextDecoder: function () { this.decode = function (v) { return v == null ? "" : String(v); }; },
  getComputedStyle: function () { return {fontSize: "16px"}; }};
sandbox.window = sandbox;
vm.createContext(sandbox);
vm.runInNewContext(fs.readFileSync(process.argv[1], "utf8"), sandbox);
var tick = function () { return new Promise(function (done) { setTimeout(done, 0); }); };
var chatView = sandbox.chatView;
var fire = function (on, kind, e) { listeners.forEach(function (l) { if (l.el === on && l.kind === kind) l.fn(e); }); };
function cardFake(tok, pool) {
  var preview = el(), input = {value: String(pool.eur)};
  var edit = {dataset: {action: pool.action, from: String(pool.from_eur), suggested: String(pool.eur)},
    elements: {eur: input},
    querySelector: function (sel) { return sel === "[data-pool-preview]" ? preview : null; }};
  var foot = el(), card = {dataset: {proposal: tok}, gone: false,
    querySelector: function (sel) { return sel === ".pfoot" ? foot : sel === "form[data-pool]" ? (pool && edit) : null; },
    remove: function () { card.gone = true; }};
  var btn = function (attr) { return {hasAttribute: function (a) { return a === attr; },
    closest: function (sel) { return sel === "button" ? this : sel === ".proposal" ? card : null; }}; };
  return {card: card, foot: foot, edit: edit, input: input, preview: preview, doit: btn("data-doit"), skip: btn("data-skip")};
}
var poolSet = {action: "set", eur: 10, from_eur: 0, to_eur: 10, reason: "the month's two verified findings"};
var cleared = null, brokenEnter = null, enterPrevented = false, busyPrevented = false, chatPosts = 0;
var busyCtl = {dataset: {present: "false"}, elements: {eur: {value: "20"}, reason: {value: "a third finding"}}, resets: 0,
  reset: function () { this.resets++; }};
var out = {setHTML: chatView.proposalHTML({id: "tok1", tool: "set_pool", arguments: '{"action":"set","eur":10,"reason":"the month\'s two verified findings"}',
  description: "Set the pool", pool: poolSet}),
  readerCtl: chatView.poolCtlHTML({present: false, can_ask: false, words: poolLine.words})};
Promise.resolve().then(function () {
  return tick(); // the opening read settles: the pool control stands under the log
}).then(function () {
  out.ctlRendered = !!poolHostEl && poolHostEl.innerHTML.indexOf("data-pool-ctl") >= 0 &&
    poolHostEl.innerHTML.indexOf("Set the pool to, EUR") >= 0 && poolHostEl.innerHTML.indexOf("0 of 30 sessions") >= 0;
  var card = cardFake("tok9", poolSet);
  fire(byId["chat-log"], "click", {target: card.skip});
  out.skipRemoved = card.card.gone === true;
  out.renderAndSkipAskNothing = posts.length === 0;
  var ctl = {dataset: {present: "false"}, elements: {eur: {value: "10"}, reason: {value: "two verified findings"}}, reset: function () {}};
  fire(poolHostEl, "submit", {target: {closest: function (sel) { return sel === "[data-pool-ctl]" ? ctl : null; }}, preventDefault: function () {}});
  return tick(); // the ask starts a normal turn
}).then(function () {
  var turn = posts.filter(function (p) { return /\/chat$/.test(p.url); })[0];
  var sent = sends[sends.length - 1];
  out.controlStartsATurn = !!turn && !!sent && sent.parts.some(function (p) {
    return p[0] === "message" && p[1] === "Set the escalation pool to EUR 10: two verified findings"; });
  var card = cardFake("tok1", poolSet);
  card.input.value = "12";
  fire(byId["chat-log"], "click", {target: card.doit});
  return tick(); // the corrected press runs
}).then(function () {
  var press = posts.filter(function (p) { return /\/proposals\/tok1$/.test(p.url); })[0];
  out.correctionBody = !!press && press.body === '{"correction":{"eur":12}}' && press.type === "application/json" && press.csrf === "tok";
  cleared = cardFake("tok1", poolSet);
  cleared.input.value = "";
  fire(byId["chat-log"], "click", {target: cleared.doit});
  brokenEnter = cardFake("tok1", poolSet);
  brokenEnter.input.value = "12";
  fire(byId["chat-log"], "submit", {target: {closest: function (sel) { return sel === "form[data-pool]" ? brokenEnter.edit : null; }},
    preventDefault: function () { enterPrevented = true; }});
  return tick().then(tick); // the refused press and the Enter ask nothing
}).then(function () {
  out.clearedPostsNothing = posts.filter(function (p) { return /\/proposals\//.test(p.url); }).length === 1;
  out.clearedKeepsCard = !!cleared.foot.aftered && made.join("\n").indexOf("Enter a valid EUR amount") >= 0 && cleared.card.gone !== true;
  out.enterKeepsEdit = enterPrevented === true && brokenEnter.input.value === "12";
  chatPosts = posts.filter(function (p) { return /\/chat$/.test(p.url); }).length;
  var ctlTarget = {closest: function (sel) { return sel === "[data-pool-ctl]" ? busyCtl : null; }};
  fire(poolHostEl, "submit", {target: ctlTarget, preventDefault: function () {}});
  fire(poolHostEl, "submit", {target: ctlTarget, preventDefault: function () { busyPrevented = true; }});
  return tick().then(tick); // the second press meets the turn the first started
}).then(function () {
  out.busyPressStays = busyPrevented === true && busyCtl.resets === 1 &&
    posts.filter(function (p) { return /\/chat$/.test(p.url); }).length === chatPosts + 1;
  console.log(JSON.stringify(out));
});`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver, "static/chat.js")
	cmd.Env = append(os.Environ(), "TZ=UTC")
	outBytes, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(outBytes, &got); err != nil {
		t.Fatalf("node said %s: %v", outBytes, err)
	}
	if want := `<div class="proposal pool" data-proposal="tok1"><p class="ptitle">I will:</p><ul><li>Set the pool</li></ul>` +
		`<form class="pool-edit" data-pool data-action="set" data-from="0" data-suggested="10">` +
		`<label>Set the pool at, EUR <input type="number" name="eur" step="0.01" min="0.01" value="10" aria-label="Set the pool at, EUR"></label>` +
		`<p class="pool-reason">reason (fixed): the month's two verified findings</p>` +
		`<p class="pool-preview" data-pool-preview>the pool would stand at €10.00</p></form>` +
		`<p class="pfoot"><button type="button" class="doit" data-doit>Do it</button> <button type="button" class="skip" data-skip>Skip</button></p></div>`; got["setHTML"] != want {
		t.Errorf("the set_pool proposal =\n%s\nwant\n%s", got["setHTML"], want)
	}
	const poolWords = "0 of 30 sessions, 0 h of 30 h agent time and €0.00 of €60.00 drawn this UTC month"
	if want := `<div class="poolctl" data-pool-ctl><p class="pool-note">` + poolWords + `</p>` +
		`<p class="pool-why">Only an approver of the project asks for a pool change.</p></div>`; got["readerCtl"] != want {
		t.Errorf("the non-approver's pool line =\n%s\nwant\n%s", got["readerCtl"], want)
	}
	for _, c := range []struct{ name, why string }{
		{"ctlRendered", "the panel's Set the pool control does not stand with the pool line on it"},
		{"skipRemoved", "Skip does not take the proposal away"},
		{"renderAndSkipAskNothing", "rendering or Skip asked the console for something"},
		{"controlStartsATurn", "the Set the pool control does not start a normal chat turn for the amount and the reason"},
		{"correctionBody", "a Do it on the corrected EUR 12 does not post exactly the correction object"},
		{"clearedPostsNothing", "a cleared amount posts to the proposal anyway"},
		{"clearedKeepsCard", "a cleared amount does not leave the refusal line on a pending card"},
		{"enterKeepsEdit", "Enter on the pool-edit form navigates away instead of keeping the edit"},
		{"busyPressStays", "a pool control press while a turn runs navigates away instead of staying"},
	} {
		if got[c.name] != true {
			t.Error(c.why)
		}
	}
}

// The top-up proposal's panel half: the increment input holds the
// suggestion against the captured allowance, the preview follows the
// edited number before any confirmation asks the console for anything,
// and the corrected press posts the increment alone. A malformed
// increment posts nothing. A proposal of another tool keeps its normal
// renderer and its empty-body confirmation. Run under node, which the
// test needs.
func TestPoolTopUpPanel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var fs = require("fs"), vm = require("vm");
var listeners = [], posts = [], made = [], byId = {};
function el() {
  var e = {textContent: "", innerHTML: "", hidden: false, className: "", value: "", disabled: false,
    dataset: {login: "sebastian", project: "p"}, style: {setProperty: function () {}},
    classList: {add: function () {}, remove: function () {}, toggle: function () {}, contains: function () { return false; }},
    setAttribute: function () {}, focus: function () {}, blur: function () {}, remove: function () { e.gone = true; },
    appendChild: function (c) { return c; }, insertBefore: function (c) { return c; }, after: function (c) { e.aftered = c; },
    contains: function () { return true; }, closest: function () { return null; }, firstChild: null, previousElementSibling: null,
    querySelector: function () { return el(); }, querySelectorAll: function () { return []; },
    getBoundingClientRect: function () { return {left: 0, width: 0}; }, clientWidth: 0};
  e.addEventListener = function (kind, fn) { listeners.push({el: e, kind: kind, fn: fn}); };
  return e;
}
var document = {title: "Factory console", hidden: false,
  createElement: function (tag) { var t = el();
    if (tag === "template") { var html = ""; t.content = {firstElementChild: el()};
      Object.defineProperty(t, "innerHTML", {get: function () { return html; }, set: function (v) { html = v; made.push(v); }}); }
    return t; },
  getElementById: function (id) {
    if (id === "img-zoom" || id === "thread-dialog" || id === "screen" || id === "chat-divider") return null;
    return byId[id] || (byId[id] = el());
  },
  addEventListener: function (kind, fn) { listeners.push({el: document, kind: kind, fn: fn}); },
  querySelector: function () { return el(); }, querySelectorAll: function () { return []; },
  documentElement: el(), body: el(), activeElement: null};
byId["chat-form"] = el();
byId["chat-form"].elements = {csrf: {value: "tok"}};
function sse(text) { var sent = false; return {read: function () {
  if (!sent) { sent = true; return Promise.resolve({done: false, value: text}); }
  return Promise.resolve({done: true}); }}; }
var sandbox = {document: document, console: {log: function () {}, error: function () {}},
  fetch: function (url, opts) {
    var u = String(url);
    if (opts && opts.method === "POST") {
      posts.push({url: u, body: opts.body === undefined ? "" : String(opts.body), type: (opts.headers || {})["Content-Type"] || ""});
      if (/\/chat\/(seen|stop)$/.test(u)) return Promise.resolve({ok: true});
      return Promise.resolve({ok: true, headers: {get: function (h) { return String(h).toLowerCase() === "content-type" ? "text/event-stream" : null; }},
        body: {getReader: function () { return sse("event: done\ndata: {}\n\n"); }}});
    }
    return Promise.resolve({ok: true, json: function () {
      return Promise.resolve(/\/conversations$/.test(u) ? [] :
        {conversation: 3, messages: [], thread: {number: 3, title: "", state: "active"}}); }});
  },
  setInterval: function () { return 0; }, setTimeout: function (done) { done(); return 0; }, clearTimeout: function () {}, clearInterval: function () {},
  localStorage: {getItem: function () { return null; }, setItem: function () {}},
  URL: {createObjectURL: function () { return ""; }},
  FormData: function () { return {append: function () {}}; },
  URLSearchParams: function (p) { this.toString = function () { return Object.keys(p).map(function (k) { return k + "=" + p[k]; }).join("&"); }; },
  TextDecoder: function () { this.decode = function (v) { return v == null ? "" : String(v); }; },
  getComputedStyle: function () { return {fontSize: "16px"}; }};
sandbox.window = sandbox;
vm.createContext(sandbox);
vm.runInNewContext(fs.readFileSync(process.argv[1], "utf8"), sandbox);
var tick = function () { return new Promise(function (done) { setTimeout(done, 0); }); };
var chatView = sandbox.chatView;
var fire = function (on, kind, e) { listeners.forEach(function (l) { if (l.el === on && l.kind === kind) l.fn(e); }); };
function cardFake(tok, pool) {
  var preview = el(), input = {value: String(pool ? pool.eur : "")};
  var edit = pool ? {dataset: {action: pool.action, from: String(pool.from_eur), suggested: String(pool.eur)},
    elements: {eur: input},
    querySelector: function (sel) { return sel === "[data-pool-preview]" ? preview : null; }} : null;
  var foot = el(), card = {dataset: {proposal: tok}, gone: false,
    querySelector: function (sel) { return sel === ".pfoot" ? foot : sel === "form[data-pool]" ? edit : null; },
    remove: function () { card.gone = true; }};
  var btn = function (attr) { return {hasAttribute: function (a) { return a === attr; },
    closest: function (sel) { return sel === "button" ? this : sel === ".proposal" ? card : null; }}; };
  return {card: card, foot: foot, edit: edit, input: input, preview: preview, doit: btn("data-doit"), skip: btn("data-skip")};
}
var topUp = {action: "top_up", eur: 5, from_eur: 10, to_eur: 15, reason: "a second verified finding"};
var malformed = null;
var out = {topUpHTML: chatView.proposalHTML({id: "tok7", tool: "set_pool", arguments: "{}", description: "Top up the pool", pool: topUp}),
  plainHTML: chatView.proposalHTML({id: "tok2", tool: "new_card", arguments: '{"title":"Cache metrics","track":"direct"}',
    description: "File a card, title Cache metrics, track direct"})};
Promise.resolve().then(function () {
  return tick();
}).then(function () {
  var card = cardFake("tok7", topUp);
  card.input.value = "4";
  fire(byId["chat-log"], "input", {target: {closest: function (sel) { return sel === "form[data-pool]" ? card.edit : null; }}});
  out.previewFollows = card.preview.textContent === "the pool would stand at €14.00";
  out.previewAsksNothing = posts.length === 0;
  fire(byId["chat-log"], "click", {target: card.doit});
  return tick(); // the corrected press runs
}).then(function () {
  var press = posts.filter(function (p) { return /\/proposals\/tok7$/.test(p.url); })[0];
  out.correctedIncrement = !!press && press.body === '{"correction":{"eur":4}}' && press.type === "application/json";
  var plain = cardFake("tok2", null);
  fire(byId["chat-log"], "click", {target: plain.doit});
  return tick(); // the unrelated proposal's press runs
}).then(function () {
  var press = posts.filter(function (p) { return /\/proposals\/tok2$/.test(p.url); })[0];
  out.plainEmptyBody = !!press && press.body === "" && press.type === "application/x-www-form-urlencoded";
  malformed = cardFake("tok7", topUp);
  malformed.input.value = "abc";
  fire(byId["chat-log"], "click", {target: malformed.doit});
  return tick().then(tick); // the malformed press refuses before any request
}).then(function () {
  out.nonFinitePostsNothing = posts.filter(function (p) { return /\/proposals\/tok7$/.test(p.url); }).length === 1 &&
    !!malformed.foot.aftered && made.join("\n").indexOf("Enter a valid EUR amount") >= 0;
  console.log(JSON.stringify(out));
});`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver, "static/chat.js")
	cmd.Env = append(os.Environ(), "TZ=UTC")
	outBytes, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(outBytes, &got); err != nil {
		t.Fatalf("node said %s: %v", outBytes, err)
	}
	if want := `<div class="proposal pool" data-proposal="tok7"><p class="ptitle">I will:</p><ul><li>Top up the pool</li></ul>` +
		`<form class="pool-edit" data-pool data-action="top_up" data-from="10" data-suggested="5">` +
		`<label>Top up the pool by, EUR <input type="number" name="eur" step="0.01" min="0.01" value="5" aria-label="Top up the pool by, EUR"></label>` +
		`<p class="pool-reason">reason (fixed): a second verified finding</p>` +
		`<p class="pool-preview" data-pool-preview>the pool would stand at €15.00</p></form>` +
		`<p class="pfoot"><button type="button" class="doit" data-doit>Do it</button> <button type="button" class="skip" data-skip>Skip</button></p></div>`; got["topUpHTML"] != want {
		t.Errorf("the top_up proposal =\n%s\nwant\n%s", got["topUpHTML"], want)
	}
	if want := `<div class="proposal" data-proposal="tok2"><p class="ptitle">I will:</p><ul><li>File a card, title Cache metrics, track direct</li></ul>` +
		`<details class="args"><summary>new_card arguments</summary><pre>{&quot;title&quot;:&quot;Cache metrics&quot;,&quot;track&quot;:&quot;direct&quot;}</pre></details>` +
		`<p class="pfoot"><button type="button" class="doit" data-doit>Do it</button> <button type="button" class="skip" data-skip>Skip</button></p></div>`; got["plainHTML"] != want {
		t.Errorf("the unrelated proposal =\n%s\nwant\n%s", got["plainHTML"], want)
	}
	for _, c := range []struct{ name, why string }{
		{"previewFollows", "an edited increment of 4 against EUR 10 does not preview EUR 14"},
		{"previewAsksNothing", "the preview asked the console for something before the confirmation"},
		{"correctedIncrement", "a Do it on the corrected increment does not post exactly the correction object"},
		{"plainEmptyBody", "an unrelated proposal's Do it does not post an empty body as before"},
		{"nonFinitePostsNothing", "a malformed increment posts to the proposal instead of the refusal line"},
	} {
		if got[c.name] != true {
			t.Error(c.why)
		}
	}
}

// A pool press's partial failures (the pool-chat-controls plan): a
// merged change the policy in force does not hold yet answers its
// literal activation-pending words with the pull request's identity, an
// audit row that could not be written its literal repair words, and
// neither completed HTTP answer turns the proposal into an applied one —
// the card keeps its Do it, nothing claims the new pool counts, and
// only the stream's applied line, which names the pull request and the
// policy holding it, ever says the change loaded. Run under node, which
// the test needs.
func TestPoolPanelPending(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const (
		pending = "The pool change is merged as sf/infra#1482, but the policy in force still holds the pool of demo-app at EUR 10.00: " +
			"the loader picks the merge up on its next poll. The pool_changed row is written, and the pool stays EUR 10.00 until the policy holds EUR 14.00."
		repair = "The pool change is merged as sf/infra#1482, but its pool_changed row could not be written: the events store is down. " +
			"The change stands: write the missing row (action set, from_eur 10.00, confirmed_eur 4.00, to_eur 14.00, " +
			"reason \"the month's two verified findings\") and do not repeat the change."
		// the rendered row escapes the reason's quotes
		repairShown = "The pool change is merged as sf/infra#1482, but its pool_changed row could not be written: the events store is down. " +
			"The change stands: write the missing row (action set, from_eur 10.00, confirmed_eur 4.00, to_eur 14.00, " +
			"reason &quot;the month's two verified findings&quot;) and do not repeat the change."
		landed = "The pool of demo-app is €14.00 now, was €10.00: sf/infra#1482 carries the change and the policy in force holds it."
	)
	const poolDriver = `var fs = require("fs"), vm = require("vm");
var listeners = [], posts = [], made = [], byId = {}, answers = ANSWERS;
function el() {
  var e = {textContent: "", innerHTML: "", hidden: false, className: "", value: "", disabled: false,
    dataset: {login: "sebastian", project: "p"}, style: {setProperty: function () {}},
    classList: {add: function () {}, remove: function () {}, toggle: function () {}, contains: function () { return false; }},
    setAttribute: function () {}, focus: function () {}, blur: function () {}, remove: function () { e.gone = true; },
    appendChild: function (c) { return c; }, insertBefore: function (c) { return c; }, after: function (c) { e.aftered = c; },
    contains: function () { return true; }, closest: function () { return null; }, firstChild: null, previousElementSibling: null,
    querySelector: function () { return el(); }, querySelectorAll: function () { return []; },
    getBoundingClientRect: function () { return {left: 0, width: 0}; }, clientWidth: 0};
  e.addEventListener = function (kind, fn) { listeners.push({el: e, kind: kind, fn: fn}); };
  return e;
}
var document = {title: "Factory console", hidden: false,
  createElement: function (tag) { var t = el();
    if (tag === "template") { var html = ""; t.content = {firstElementChild: el()};
      Object.defineProperty(t, "innerHTML", {get: function () { return html; }, set: function (v) { html = v; made.push(v); }}); }
    return t; },
  getElementById: function (id) {
    if (id === "img-zoom" || id === "thread-dialog" || id === "screen" || id === "chat-divider") return null;
    return byId[id] || (byId[id] = el());
  },
  addEventListener: function (kind, fn) { listeners.push({el: document, kind: kind, fn: fn}); },
  querySelector: function () { return el(); }, querySelectorAll: function () { return []; },
  documentElement: el(), body: el(), activeElement: null};
byId["chat-form"] = el();
byId["chat-form"].elements = {csrf: {value: "tok"}};
function sse(text) { var sent = false; return {read: function () {
  if (!sent) { sent = true; return Promise.resolve({done: false, value: text}); }
  return Promise.resolve({done: true}); }}; }
var sandbox = {document: document, console: {log: function () {}, error: function () {}},
  fetch: function (url, opts) {
    var u = String(url);
    if (opts && opts.method === "POST") {
      posts.push({url: u, body: opts.body === undefined ? "" : String(opts.body)});
      if (/\/chat\/(seen|stop)$/.test(u)) return Promise.resolve({ok: true});
      var a = answers.shift();
      if (!a.ok) return Promise.resolve({ok: false, status: a.status, text: function () { return Promise.resolve(a.text); }});
      return Promise.resolve({ok: true, headers: {get: function (h) { return String(h).toLowerCase() === "content-type" ? "text/event-stream" : null; }},
        body: {getReader: function () { return sse(a.sse); }}});
    }
    return Promise.resolve({ok: true, json: function () {
      return Promise.resolve(/\/conversations$/.test(u) ? [] :
        {conversation: 3, messages: [], thread: {number: 3, title: "", state: "active"}}); }});
  },
  setInterval: function () { return 0; }, setTimeout: function (done) { done(); return 0; }, clearTimeout: function () {}, clearInterval: function () {},
  localStorage: {getItem: function () { return null; }, setItem: function () {}},
  URL: {createObjectURL: function () { return ""; }},
  FormData: function () { return {append: function () {}}; },
  URLSearchParams: function (p) { this.toString = function () { return Object.keys(p).map(function (k) { return k + "=" + p[k]; }).join("&"); }; },
  TextDecoder: function () { this.decode = function (v) { return v == null ? "" : String(v); }; },
  getComputedStyle: function () { return {fontSize: "16px"}; }};
sandbox.window = sandbox;
vm.createContext(sandbox);
vm.runInNewContext(fs.readFileSync(process.argv[1], "utf8"), sandbox);
var tick = function () { return new Promise(function (done) { setTimeout(done, 0); }); };
var chatView = sandbox.chatView;
var fire = function (on, kind, e) { listeners.forEach(function (l) { if (l.el === on && l.kind === kind) l.fn(e); }); };
function cardFake(tok, pool) {
  var preview = el(), input = {value: String(pool.eur)};
  var edit = {dataset: {action: pool.action, from: String(pool.from_eur), suggested: String(pool.eur)},
    elements: {eur: input},
    querySelector: function (sel) { return sel === "[data-pool-preview]" ? preview : null; }};
  var foot = el(), card = {dataset: {proposal: tok},
    querySelector: function (sel) { return sel === ".pfoot" ? foot : sel === "form[data-pool]" ? edit : null; }};
  var btn = function (attr) { return {hasAttribute: function (a) { return a === attr; },
    closest: function (sel) { return sel === "button" ? this : sel === ".proposal" ? card : null; }}; };
  return {card: card, foot: foot, edit: edit, input: input, doit: btn("data-doit")};
}
var poolSet = {action: "set", eur: 10, from_eur: 10, to_eur: 10, reason: "the month's two verified findings"};
var saw = function (needle) { return made.join("\n").indexOf(needle) >= 0; };
var applied = function () { return saw("✓ applied"); };
var out = {};
var press = function (card, eur) {
  if (eur != null) card.input.value = eur;
  fire(byId["chat-log"], "click", {target: card.doit});
};
Promise.resolve().then(function () {
  return tick();
}).then(function () {
  press(cardFake("tok1", poolSet));
  return tick().then(tick); // the press completes: merged, the policy still holds EUR 10.00
}).then(function () {
  out.pendingWords = saw(PENDING) && !saw("The pool of") && !applied();
  out.pendingKeepsCard = posts.filter(function (p) { return /\/proposals\/tok1$/.test(p.url); }).length === 1;
  var card = cardFake("tok1", poolSet);
  press(card, "4");
  return tick().then(tick); // the corrected press completes: the audit row could not be written
}).then(function () {
  out.repairWords = saw(REPAIR_SHOWN) && !applied();
  out.repairKeepsCard = !applied();
  press(cardFake("tok1", poolSet));
  return tick().then(tick); // the press runs and lands: the applied line names the pull request
}).then(function () {
  out.loadedWords = applied() && saw(LANDED);
  console.log(JSON.stringify(out));
});`
	driver := strings.ReplaceAll(poolDriver, "ANSWERS", `[{"ok":false,"status":503,"text":`+strconv.Quote(pending)+`},
      {"ok":false,"status":500,"text":`+strconv.Quote(repair)+`},
      {"ok":true,"sse":"event: applied\ndata: {\"tool\":\"set_pool\",\"result\":"+JSON.stringify(LANDED)+"}\n\nevent: done\ndata: {}\n\n"}]`)
	driver = strings.ReplaceAll(driver, "PENDING", strconv.Quote(pending))
	driver = strings.ReplaceAll(driver, "REPAIR_SHOWN", strconv.Quote(repairShown))
	driver = strings.ReplaceAll(driver, "LANDED", strconv.Quote(landed))
	cmd := exec.CommandContext(t.Context(), node, "-e", driver, "static/chat.js")
	cmd.Env = append(os.Environ(), "TZ=UTC")
	outBytes, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got map[string]bool
	if err := json.Unmarshal(outBytes, &got); err != nil {
		t.Fatalf("node said %s: %v", outBytes, err)
	}
	for _, c := range []struct{ name, why string }{
		{"pendingWords", "a merged-but-unloaded change does not render its literal activation-pending words alone, with no claim the pool changed"},
		{"pendingKeepsCard", "a completed HTTP answer alone discards or spends the proposal"},
		{"repairWords", "a change whose audit row could not be written does not render its literal repair words with no applied line"},
		{"repairKeepsCard", "the repair answer optimistically activates the change"},
		{"loadedWords", "the landed change's applied line does not name the pull request and the policy holding it"},
	} {
		if !got[c.name] {
			t.Error(c.why)
		}
	}
}

// TestChatPanelDocrefs: an answer's document path and its Markdown links
// render as docref anchors — the bare docs/….md path, a link whose target
// carries the docs/ prefix and one without it, which resolves against the
// document root docs/ — while a code path, an external link and a code
// span stay text (roadmap 280).
func TestChatPanelDocrefs(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var chatView = require("vm").runInNewContext(require("fs").readFileSync(process.argv[1], "utf8") + "\nchatView",
  {document: {getElementById: function () { return null; }}});
console.log(JSON.stringify([
  chatView.md("The backlog plan is docs/plans/2026-09-24-build-plan.md."),
  chatView.md("See [the build plan](docs/plans/2026-09-24-build-plan.md)."),
  chatView.md("See [the plan](plans/2026-09-24-build-plan.md)."),
  chatView.md("The change is internal/console/chat.go, the [wiki](https://example.com/wiki) aside."),
  chatView.md("The roadmap is ` + "`docs/roadmap.md`" + ` in code.")
]));`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver, "static/chat.js")
	cmd.Env = append(os.Environ(), "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got []string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node said %s: %v", out, err)
	}
	const anchor = `<a class="docref" data-doc="docs/plans/2026-09-24-build-plan.md" href="?doc=docs/plans/2026-09-24-build-plan.md">`
	want := []string{
		`<p>The backlog plan is ` + anchor + `docs/plans/2026-09-24-build-plan.md</a>.</p>`,
		`<p>See ` + anchor + `the build plan</a>.</p>`,
		`<p>See ` + anchor + `the plan</a>.</p>`,
		`<p>The change is internal/console/chat.go, the [wiki](https://example.com/wiki) aside.</p>`,
		`<p>The roadmap is <code>docs/roadmap.md</code> in code.</p>`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("chatView.md under node:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestChatPanelDocrefOpensView: the console's document view opens on the
// chat's view event — show_doc's open riding the turn's stream — and on a
// pressed docref, whose press dispatches the same event; both fetch the
// document under the project (roadmap 280).
func TestChatPanelDocrefOpensView(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var listeners = {}, winListeners = {}, pushes = [], fetches = [], pending = [], sources = [];
function store() { var m = {}; return {getItem: function (k) { return k in m ? m[k] : null; }, setItem: function (k, v) { m[k] = String(v); }, removeItem: function (k) { delete m[k]; }}; }
function el(props) {
  var e = {dataset: {}, hidden: false, textContent: "", innerHTML: "", children: [], style: {}, value: "", title: "", open: false, scrollTop: 0,
    querySelector: function () { return null; }, querySelectorAll: function () { return []; }, contains: function () { return false; },
    getAttribute: function () { return null; }, setAttribute: function () {}, addEventListener: function () {}, removeEventListener: function () {},
    appendChild: function (c) { this.children.push(c); return c; }, focus: function () {}, click: function () {}, scrollIntoView: function () {},
    reset: function () {}, remove: function () {}, closest: function () { return null; },
    getBoundingClientRect: function () { return {top: 0, bottom: 0, left: 0, right: 0}; },
    classList: {contains: function () { return false; }, toggle: function () {}, add: function () {}, remove: function () {}}};
  return Object.assign(e, props);
}
var byID = {board: el({dataset: {project: "code", query: "?project=code"}}), "status-line": el(), live: el(),
  drawer: el({hidden: true}), "session-log": el(), "steer-input": el(), q: el({value: ""}),
  chat: el({dataset: {login: "sebastian", project: "code"}}), "chat-log": el(), "chat-input": el({value: ""}),
  "chat-new": el(), "thread-rows": el(), "thread-head": el(), "chat-answer": el(), "chat-commands": el(), "chat-palette": el(),
  documents: el(), "doc-view": el(), "doc-article": el(), "doc-path": el(), "doc-facts": el(), "doc-edit": el(),
  "doc-new-form": el(), "doc-pathline": el(), "doc-pins": el(), "doc-back": el(), "doc-fwd": el(), "doc-wide": el(),
  "doc-forgejo": el(), "doc-recent": el(), "doc-recent-menu": el(), "doc-editor": el(), keys: el({hidden: true}), flash: el({hidden: true})};
function EventSource(url) { this.url = url; this.on = {}; sources.push(this); }
EventSource.prototype.addEventListener = function (type, fn) { this.on[type] = fn; };
EventSource.prototype.close = function () { this.closed = true; };
var sandbox = {EventSource: EventSource, URL: URL, URLSearchParams: URLSearchParams, CSS: {escape: function (s) { return s; }},
  location: {href: "http://lab/?project=code", search: "?project=code", hash: ""},
  history: {state: null, replaceState: function () {}, pushState: function (state, title, path) { pushes.push(path); }},
  localStorage: store(), sessionStorage: store(),
  setTimeout: function (fn) { if (fn) fn(); return 0; }, setInterval: function () {}, clearTimeout: function () {},
  Event: function (type) { this.type = type; }, CustomEvent: function (type, init) { this.type = type; this.detail = init && init.detail; },
  fetch: function (url) {
    fetches.push(String(url));
    return new Promise(function (ok) { pending.push(function () {
      ok({ok: true, json: function () {
        var path = String(url).indexOf("roadmap") > 0 ? "docs/roadmap.md" : "docs/plans/2026-09-24-build-plan.md";
        return Promise.resolve({path: path, html: "<h1 id=\"the-roadmap\">The roadmap</h1><p>Hello.</p>",
          sha256: "aa", commit: "c0ffee", at: "2026-11-09T09:00:00Z"});
      }});
    }); });
  },
  htmx: {ajax: function () { return new Promise(function () {}); }, swap: function () {}, process: function () {}},
  window: {addEventListener: function (type, fn) { (winListeners[type] = winListeners[type] || []).push(fn); }, innerHeight: 800},
  document: {body: el(), documentElement: {getAttribute: function () { return "dark"; }}, title: "", activeElement: null,
    getElementById: function (id) { return byID[id] || null; },
    querySelectorAll: function () { return []; }, querySelector: function () { return null; },
    createElement: function () { return el(); }, createTextNode: function (t) { return {textContent: t}; },
    addEventListener: function (type, fn) { (listeners[type] = listeners[type] || []).push(fn); },
    removeEventListener: function (type, fn) { listeners[type] = (listeners[type] || []).filter(function (f) { return f !== fn; }); },
    dispatchEvent: function (e) { (listeners[e.type] || []).forEach(function (fn) { fn(e); }); }}};
require("vm").runInNewContext(require("fs").readFileSync("static/console.js", "utf8"), sandbox);
require("vm").runInNewContext(require("fs").readFileSync("static/chat.js", "utf8"), sandbox);
(listeners.DOMContentLoaded || []).forEach(function (fn) { fn(); });
function settle() {
  pending.splice(0).forEach(function (ok) { ok(); });
  return new Promise(function (done) { setImmediate(done); });
}
(async function () {
  var out = {};
  sandbox.document.dispatchEvent({type: "view", detail: {doc: "docs/roadmap.md"}});
  await settle();
  await settle();
  out.event = [!byID["doc-view"].hidden, byID["doc-path"].textContent, pushes.slice()];
  var prevented = 0;
  var anchor = el({});
  anchor.dataset.doc = "docs/plans/2026-09-24-build-plan.md";
  anchor.closest = function (sel) { return sel === "a.docref" ? anchor : null; };
  (listeners.click || []).forEach(function (fn) { fn({target: anchor, preventDefault: function () { prevented++; }}); });
  await settle();
  await settle();
  out.press = [prevented, !byID["doc-view"].hidden, byID["doc-path"].textContent];
  out.fetches = fetches;
  console.log(JSON.stringify(out));
})();`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	want := `{"event":[true,"docs/roadmap.md",["/?project=code&doc=docs%2Froadmap.md"]],` +
		`"press":[1,true,"docs/plans/2026-09-24-build-plan.md"],` +
		`"fetches":["/projects/code/docs/docs/roadmap.md","/projects/code/docs/docs/plans/2026-09-24-build-plan.md"]}`
	if strings.TrimSpace(string(out)) != want {
		t.Errorf("console.js and chat.js under node:\n%s\nwant\n%s", out, want)
	}
}
