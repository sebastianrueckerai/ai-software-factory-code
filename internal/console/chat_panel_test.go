package console

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
<section id="chat" class="chat" aria-labelledby="chat-title"`,
		`<section id="chat" class="chat" aria-labelledby="chat-title" data-project="ai-software-factory-code" data-login="sebastian">
<div class="chat-head"><h2 id="chat-title">Chat</h2> <span class="chat-who">project agent · ai-software-factory-code</span> <button type="button" id="chat-new" class="chat-new">New conversation</button> <select id="chat-conversations" hidden aria-label="Old conversations"></select></div>
<div id="chat-log" class="chat-log" role="log" aria-label="The conversation with the project agent">
<p class="meta chat-start">Ask or tell the project agent. It reads, explains, moves, answers, steers and approves as you, through the console's own actions; it runs no code.</p>
</div>
<p class="meta chat-own">This is your own conversation with the project agent; other members have theirs.</p>
<form id="chat-form" class="chat-form" method="post" action="/projects/ai-software-factory-code/chat">
<input type="hidden" name="csrf" value="` + s.csrf + `">
<label for="chat-input" class="visually-hidden">Your message to the project agent</label>
<textarea id="chat-input" name="message" rows="2" maxlength="8192" placeholder="Ask or tell the project… (Enter sends, Shift+Enter breaks the line)" aria-keyshortcuts="/"></textarea>
<button type="submit">Send</button>
</form>
<ol id="chat-queue" class="chat-queue" aria-label="Your messages waiting for the turn to end" title="Each goes out as its own turn once the one before it ends; click one to edit it. They live in this page: a reload drops them." hidden></ol>
<script src="/static/chat.js" defer></script>
</section>
<div id="board" data-project="ai-software-factory-code"`,
		`<dt><kbd>/</kbd></dt><dd>write to the project agent</dd>
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
<div id="chat-log" class="chat-log" role="log" aria-label="The conversation with the project agent">
<p class="meta chat-start">You read this project: its writers chat with its agent.</p>
</div>
<script src="/static/chat.js" defer></script>
</section>`; !strings.Contains(body, want) {
		t.Errorf("the reader's page lacks\n%s", want)
	}
	for _, unwanted := range []string{"chat-form", "chat-input", "chat-queue", "data-login", "<kbd>/</kbd>"} {
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
var one = {number: 1, first_at: "2026-10-11T09:30:01Z", last_at: "2026-10-11T09:30:02Z", messages: 2};
console.log(JSON.stringify({
  pending: chatView.historyHTML(msgs, "sebastian", [{token: "tok1", state: "pending"}]),
  used: chatView.historyHTML(msgs, "sebastian", [{token: "tok1", state: "used"}]),
  stream: chatView.proposalHTML({id: "tok1", tool: "new_card", arguments: JSON.parse(args), description: "File a card, title Cache metrics, lane ready"}),
  two: chatView.conversationsHTML([{number: 2, first_at: "2026-10-11T09:30:03Z", last_at: "2026-10-11T09:30:04Z", messages: 2}, one], 2),
  afterNew: chatView.conversationsHTML([one], 2),
  one: chatView.conversationsHTML([one], 1),
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
	out, err := exec.CommandContext(t.Context(), node, "-e", driver, "static/chat.js").Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node said %s: %v", out, err)
	}
	const (
		you      = `<div class="msg you"><p class="who">sebastian · 10-11 09:30Z</p><p class="said">file a card</p></div>`
		agent    = `<div class="msg agent"><p class="who">project agent · 10-11 09:30Z</p>`
		args     = `{&quot;title&quot;:&quot;Cache metrics&quot;,&quot;lane&quot;:&quot;ready&quot;}`
		result   = `Proposed to sebastian: File a card, title Cache metrics, lane ready. Nothing runs until they press Do it.`
		proposal = `<div class="proposal" data-proposal="tok1"><p class="ptitle">I will:</p><ul><li>File a card, title Cache metrics, lane ready</li></ul>` +
			`<details class="args"><summary>new_card arguments</summary><pre>` + args + `</pre></details>` +
			`<p class="pfoot"><button type="button" class="doit" data-doit>Do it</button> <button type="button" class="skip" data-skip>Skip</button></p></div>`
		older = `<option value="">Current conversation</option><option value="1">Conversation 1 · 10-11 09:30Z · 2 messages</option>`
	)
	for _, c := range []struct{ name, want string }{
		{"pending", you + agent + proposal + `</div>`},
		{"used", you + agent + `<details class="tool"><summary>🔧 new_card · ` + result + `</summary><pre>` + args + "\n\n" + result + `</pre></details>` +
			`<p class="pfoot"><span class="expired">used</span></p></div>`},
		{"stream", proposal},
		{"two", older},
		{"afterNew", older},
		{"one", ""},
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
