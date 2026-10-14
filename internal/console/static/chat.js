// The project agent's panel beside the board (proposal-v2, "The chat as a
// surface"): the member's conversation from GET /projects/{name}/chat, a
// turn streamed from POST .../chat, a proposal's Do it and Skip, an applied
// line's Undo. The board is the events stream's alone: what a tool did
// reaches it as a button's act would.

// chatView renders the panel's parts as markup. Text from the model is
// escaped first; only then do paragraphs, **bold**, `code`, lists and #n
// (a link to card n's drawer) render, so no markup of the model's ever
// reaches the page.
var chatView = (function () {
  function esc(s) {
    return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
  }
  // inline renders one escaped line
  function inline(s) {
    return s.split(/(`[^`]+`)/).map(function (part, i) {
      if (i % 2) return "<code>" + part.slice(1, -1) + "</code>";
      return part.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
        .replace(/(^|[^&\w])#(\d+)\b/g, "$1<a href=\"#issue-$2\" class=\"cardref\" data-issue=\"$2\">#$2</a>");
    }).join("");
  }
  function md(text) {
    var out = [], para = [], list = null;
    function flush() {
      if (para.length) out.push("<p>" + para.map(inline).join("<br>") + "</p>");
      if (list) out.push("<" + list.tag + ">" + list.items.map(function (item) { return "<li>" + inline(item) + "</li>"; }).join("") + "</" + list.tag + ">");
      para = [];
      list = null;
    }
    esc(text).split(/\r?\n/).forEach(function (line) {
      var item = /^\s*(?:([-*])|\d+[.)])\s+(.*)$/.exec(line);
      if (!line.trim()) {
        flush();
      } else if (item) {
        var tag = item[1] ? "ul" : "ol";
        if (para.length || (list && list.tag !== tag)) flush();
        list = list || {tag: tag, items: []};
        list.items.push(item[2]);
      } else {
        if (list) flush();
        para.push(line);
      }
    });
    flush();
    return out.join("");
  }
  function stamp(at) {
    var d = new Date(at);
    return isNaN(d) ? "" : d.toISOString().slice(5, 16).replace("T", " ") + "Z";
  }
  // a turn: a 22 px avatar at the left (the agent a cyan ring with a dot,
  // the member a filled circle with their initial), the name and the time
  // on one line above the text, the text at the full width
  function whoHTML(name, at) {
    return "<p class=\"who\"><span class=\"name\">" + esc(name) + "</span> <time>" + stamp(at) + "</time></p>";
  }
  function userHTML(login, text, at, queued) {
    return "<div class=\"msg you\"><span class=\"av\" aria-hidden=\"true\">" + esc(String(login).charAt(0).toUpperCase()) + "</span>" + whoHTML(login, at) +
      "<p class=\"said\">" + esc(text) + "</p>" + (queued ? "<p class=\"meta chat-queued\">queued: it goes out when the turn before it ends</p>" : "") + "</div>";
  }
  // factoryHTML is a row factoryd wrote itself (C14): a poke or an
  // escalation by "the factory", its choices as buttons, its decider line
  // when the agent acted alone; a summary turn after /compact
  function factoryHTML(m) {
    if (m.kind === "summary") {
      return "<div class=\"msg sum\"><span class=\"av\" aria-hidden=\"true\">≡</span>" + whoHTML("summary", m.at) + saidHTML(m.content) + "</div>";
    }
    var esc1 = m.kind === "escalation";
    var meta = "<p class=\"who\"><span class=\"name\">the factory</span> <time>" + stamp(m.at) + "</time>" +
      (m.repeats ? " <span class=\"rep\">" + (m.repeats + 1) + " times</span>" : "") + (esc1 ? " <span class=\"esc-l\">needs you</span>" : "") + "</p>";
    var body = saidHTML(m.content);
    if (m.decider) body += "<p class=\"decider\"><span class=\"visually-hidden\">Decided by: </span>" + esc(m.decider) + "</p>";
    if (m.choices && m.choices.length && !esc1) {
      body += "<p class=\"choices\">" + m.choices.map(function (c) {
        return "<button type=\"button\" data-choice=\"" + esc(c) + "\">" + esc(c) + "</button>"; }).join(" ") + "</p>";
    }
    return "<div class=\"msg agent poke" + (esc1 ? " esc" : "") + "\" data-message=\"" + esc(m.id) + "\"><span class=\"av\" aria-hidden=\"true\"></span>" + meta +
      (esc1 ? "<div class=\"esc-b\">" + body + "</div>" : body) + "</div>";
  }
  function agentHTML(at, parts) {
    return "<div class=\"msg agent\"><span class=\"av\" aria-hidden=\"true\"></span>" + whoHTML("project agent", at) + parts + "</div>";
  }
  function saidHTML(text) {
    return "<div class=\"said\">" + md(text) + "</div>";
  }
  // toolHTML is a tool call folded to its name, its duration when known
  // and its result's first line; a refusal (the policy's or the role's) is
  // the agent's line instead
  function toolHTML(name, args, ms, result) {
    result = String(result || "");
    if (result.indexOf("Refused: ") === 0) return "<p class=\"refused\"><code>" + esc(name) + "</code> " + esc(result) + "</p>";
    var first = result.split("\n")[0];
    return "<details class=\"tool\"><summary>🔧 " + esc(name) + (ms == null ? "" : " · " + Number(ms) + " ms") + (first ? " · " + esc(first) : "") +
      "</summary><pre>" + esc(typeof args === "string" ? args : JSON.stringify(args)) + (result ? "\n\n" + esc(result) : "") + "</pre></details>";
  }
  function proposalHTML(p) {
    return "<div class=\"proposal\" data-proposal=\"" + esc(p.id) + "\"><p class=\"ptitle\">I will:</p><ul><li>" + inline(esc(p.description)) + "</li></ul>" +
      "<details class=\"args\"><summary>" + esc(p.tool) + " arguments</summary><pre>" + esc(typeof p.arguments === "string" ? p.arguments : JSON.stringify(p.arguments)) + "</pre></details>" +
      "<p class=\"pfoot\"><button type=\"button\" class=\"doit\" data-doit>Do it</button> <button type=\"button\" class=\"skip\" data-skip>Skip</button></p></div>";
  }
  // appliedHTML is a write that took effect, or an undo's when undone
  function appliedHTML(a, undone) {
    return "<p class=\"applied\">" + (undone ? "<span class=\"undone\">↶ undone</span>" : "<span class=\"ok\">✓ applied</span>") + " <code>" + esc(a.tool) + "</code> · " + inline(esc(a.result)) +
      (a.undo ? " <button type=\"button\" class=\"undo\" data-undo=\"" + esc(a.undo) + "\">Undo</button>" : "") + "</p>";
  }
  // historyHTML is the stored conversation, oldest first: each member's
  // line, and the agent's rows between two of them as one message. A
  // proposed call whose state in proposals is pending offers its Do it as
  // the stream's does; a used or expired one says so under its block.
  function historyHTML(msgs, login, proposals) {
    var states = {};
    (proposals || []).forEach(function (p) { states[p.token] = p.state; });
    var out = [], parts = null, at;
    function close() {
      if (parts) out.push(agentHTML(at, parts.join("")));
      parts = null;
    }
    msgs.forEach(function (m) {
      if (m.role === "user") {
        close();
        out.push(userHTML(login, m.content, m.at, m.kind === "queued"));
        return;
      }
      if (m.kind === "poke" || m.kind === "escalation" || m.kind === "summary") {
        close();
        out.push(factoryHTML(m));
        return;
      }
      if (!parts) {
        parts = [];
        at = m.at;
      }
      if (m.content) parts.push(saidHTML(m.content));
      (m.tool_calls || []).forEach(function (c) {
        var state = c.proposal && states[c.proposal];
        if (state === "pending") {
          parts.push(proposalHTML({id: c.proposal, tool: c.name, arguments: c.arguments,
            description: c.result.replace(/^Proposed to [^:]*: /, "").replace(/\. Nothing runs until they press Do it\.$/, "")}));
          return;
        }
        parts.push(toolHTML(c.name, c.arguments, null, c.result));
        if (state) parts.push("<p class=\"pfoot\"><span class=\"expired\">" + esc(state) + "</span></p>");
      });
    });
    close();
    return out.join("");
  }
  // threadHTML is a thread's log: after a compaction its rows up to the
  // summary fold behind one line, the summary turn follows, then the rows
  // after it (C14)
  function threadHTML(msgs, login, proposals, thread) {
    var upto = thread && thread.summary ? thread.summary_upto || 0 : 0;
    if (!upto) return historyHTML(msgs, login, proposals);
    var old = msgs.filter(function (m) { return m.id <= upto; }), rest = msgs.filter(function (m) { return m.id > upto; });
    var at = old.length ? old[old.length - 1].at : (rest[0] || {}).at;
    return (old.length ? "<details class=\"compacted\"><summary>" + old.length + " earlier " + (old.length === 1 ? "message" : "messages") +
      ", compacted</summary>" + historyHTML(old, login, proposals) + "</details>" : "") +
      factoryHTML({kind: "summary", content: thread.summary, at: at}) + historyHTML(rest, login, proposals);
  }
  // paletteOf is the commands the box's value names: those whose /name
  // starts with it while it is a slash and one word; null otherwise
  function paletteOf(commands, value) {
    if (value.charAt(0) !== "/" || /\s/.test(value)) return null;
    return commands.filter(function (c) { return ("/" + c.name).indexOf(value) === 0; });
  }
  function paletteHTML(list, sel) {
    return list.map(function (c, i) {
      return "<button type=\"button\" role=\"option\" id=\"cmd-" + esc(c.name) + "\" aria-selected=\"" + (i === sel) + "\" data-cmd=\"" + esc(c.name) +
        "\"><b>/" + esc(c.name) + "</b><span>" + esc(c.words) + "</span></button>";
    }).join("");
  }
  // expand is text as the turn sends it: a command's /name its message
  function expand(commands, text) {
    var c = commands.filter(function (c) { return c.message && "/" + c.name === text; })[0];
    return c ? c.message : text;
  }
  // when is a thread's last turn as the switcher shows it: the time today,
  // yesterday, else n days ago; now is the reader's clock
  function when(at, now) {
    var d = new Date(at);
    if (isNaN(d)) return "";
    var day = function (x) { return Math.floor((x.getTime() - x.getTimezoneOffset() * 60000) / 864e5); };
    var n = day(now) - day(d);
    if (n <= 0) return String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
    return n === 1 ? "yesterday" : n + " days ago";
  }
  function titleOf(t) { return t.title || (t.inbox ? "Inbox" : "New thread"); }
  // markHTML is a thread's mark (C14): the inbox's house, a turn in flight's
  // breathing dot, else nothing
  function markHTML(t) {
    if (t.inbox) return "<span class=\"tmark inbox\" aria-hidden=\"true\">⌂</span>";
    if (t.working) return "<span class=\"tmark working\"><i class=\"dot\" aria-hidden=\"true\"></i><span class=\"visually-hidden\">a turn in flight</span></span>";
    return "<span class=\"tmark\" aria-hidden=\"true\"></span>";
  }
  function badgeHTML(t) {
    return t.unread ? "<span class=\"badge\" aria-label=\"" + esc(t.unread) + " unread\">" + esc(t.unread) + "</span>" : "";
  }
  // rowsHTML is the switcher's rows: the inbox first, the active threads,
  // then the archived under their rule; open is the open thread's number
  function rowsHTML(list, open, now) {
    var active = (list || []).filter(function (t) { return t.state !== "archived"; });
    var archived = (list || []).filter(function (t) { return t.state === "archived"; });
    var rows = active.map(function (t) {
      var on = t.number === open, tip = titleOf(t) + (t.unread ? ", " + t.unread + " unread" : "") + (t.working ? ", a turn in flight" : "");
      return "<button type=\"button\" class=\"trow" + (on ? " on" : "") + (t.unread ? " unread" : "") + (t.inbox ? " inbox" : "") +
        "\" data-thread=\"" + esc(t.number) + "\" aria-current=\"" + on + "\" title=\"" + esc(tip) + "\">" + markHTML(t) +
        "<span class=\"ttitle\">" + esc(titleOf(t)) + "</span>" + (t.unread ? badgeHTML(t) : "<span class=\"tat\">" + esc(when(t.last_at, now)) + "</span>") + "</button>";
    }).join("");
    if (archived.length) {
      rows += "<div class=\"tarch\"><span class=\"tarch-l\">archived</span>" + archived.map(function (t) {
        return "<span class=\"trow arch" + (t.number === open ? " on" : "") + "\"><span class=\"tmark\" aria-hidden=\"true\">arc</span>" +
          "<button type=\"button\" class=\"ttitle open-arch\" data-thread=\"" + esc(t.number) + "\" aria-current=\"" + (t.number === open) + "\">" + esc(titleOf(t)) + "</button>" +
          "<button type=\"button\" class=\"linkbtn restore\" data-restore=\"" + esc(t.number) + "\">restore</button></span>";
      }).join("") + "</div>";
    }
    return rows;
  }
  // sumHTML is the folded switcher's one row on the phone
  function sumHTML(list, folded) {
    var active = (list || []).filter(function (t) { return t.state !== "archived"; });
    var unread = active.reduce(function (n, t) { return n + (t.unread || 0); }, 0);
    var working = active.filter(function (t) { return t.working; }).length;
    return "Threads <span class=\"n\">" + active.length + "</span>" + (unread ? " · <span class=\"badge\">" + unread + "</span> unread" : "") +
      (working ? " · <span class=\"tmark working\"><i class=\"dot\" aria-hidden=\"true\"></i></span> " + working + " working" : "") +
      "<span class=\"chev\" aria-hidden=\"true\">" + (folded ? "⌄" : "⌃") + "</span>";
  }
  // unreadOf is the sum of the threads' unread turns, the tab title's count
  function unreadOf(list) {
    return (list || []).reduce(function (n, t) { return n + (t.unread || 0); }, 0);
  }
  // headHTML is the open thread's head: its title (a click renames it),
  // its last turn or the inbox's line or the archived note, and Archive and
  // Delete… on an active thread with rows, never on the inbox (C14)
  function headHTML(t, now) {
    var hint = t.inbox ? "the factory's thread: the agent speaks here uninvited" : t.state === "archived" ? "archived, read-only" : when(t.last_at, now);
    return "<h3 class=\"ttl\">" + (t.inbox ? "<span class=\"tmark inbox\" aria-hidden=\"true\">⌂</span>" : "") +
      (t.inbox || t.state === "archived" ? esc(titleOf(t)) : "<button type=\"button\" class=\"rename\" data-rename title=\"Rename the thread\">" + esc(titleOf(t)) + "</button>") +
      "</h3><span class=\"thint\">" + esc(hint) + "</span>" +
      (t.inbox || t.state === "archived" || !t.messages ? "" : "<span class=\"tacts\"><button type=\"button\" data-archive>Archive</button>" +
        "<button type=\"button\" class=\"danger\" data-delete>Delete…</button></span>");
  }
  // pendingOf is the escalation the thread's answer row offers: its newest
  // row, when that is an escalation with choices (C14)
  function pendingOf(msgs) {
    var m = (msgs || [])[(msgs || []).length - 1];
    return m && m.kind === "escalation" && m.choices && m.choices.length ? m : null;
  }
  function answerHTML(m) {
    return "<span class=\"answer-l\">Answer:</span>" + m.choices.map(function (c) {
      return "<button type=\"button\" data-answer=\"" + esc(c) + "\">" + esc(c) + "</button>";
    }).join("") + "<span class=\"answer-or\">or in your words below</span>";
  }
  // deleteHTML is the hard delete's warning for thread t (C14)
  function deleteHTML(t) {
    var n = t.messages || 0;
    return "<div id=\"thread-dialog\" class=\"overlay\" role=\"dialog\" aria-modal=\"true\" aria-labelledby=\"dlg-title\"><div class=\"overlay-box\">" +
      "<h2 id=\"dlg-title\">Delete “" + esc(titleOf(t)) + "”?</h2><p>Its " + n + (n === 1 ? " message goes" : " messages go") +
      " for good: a hard delete, no archive. One event will name you, the thread and the count. What the chat did stays in the events, the audit and git, " +
      "so no decision and no merged card goes with it.</p><div class=\"acts\"><button type=\"button\" class=\"danger\" data-dlg=\"delete\">Delete the thread</button>" +
      "<button type=\"button\" data-dlg=\"keep\">Keep it</button></div></div></div>";
  }
  function eventStream(contentType) {
    return /^text\/event-stream/.test(contentType || "");
  }
  // sse splits an event stream's text into its whole events and the rest
  function sse(text) {
    var blocks = text.split("\n\n"), rest = blocks.pop(), events = [];
    blocks.forEach(function (block) {
      var event = "message", data = [];
      block.split("\n").forEach(function (line) {
        var i = line.indexOf(":"), field = i < 0 ? line : line.slice(0, i), value = i < 0 ? "" : line.slice(i + 1).replace(/^ /, "");
        if (field === "event") event = value;
        else if (field === "data") data.push(value);
      });
      if (data.length) events.push({event: event, data: data.join("\n")});
    });
    return {events: events, rest: rest};
  }
  // queue keeps the messages the member sends while a turn streams, in
  // order, each editable and removable until it goes out: whenever busy()
  // is false (the turn before it ended, no edit open) flush starts the
  // first as its own turn, and shown(items) draws the rest after every
  // change.
  function queue(start, busy, shown) {
    var items = [];
    function flush() {
      if (items.length && !busy()) start(items.shift());
      shown(items.slice());
    }
    return {
      add: function (text) {
        items.push(text);
        flush();
      },
      edit: function (i, text) {
        text = text.trim();
        if (text) items[i] = text;
        else items.splice(i, 1);
        flush();
      },
      remove: function (i) {
        items.splice(i, 1);
        flush();
      },
      flush: flush
    };
  }
  function queueHTML(items) {
    return items.map(function (text, i) {
      return "<li><button type=\"button\" class=\"queued-text\" data-edit=\"" + i + "\" aria-label=\"Edit queued message " + (i + 1) + ": " + esc(text) + "\">" + esc(text) +
        "</button><button type=\"button\" class=\"queued-remove\" data-remove=\"" + i + "\" aria-label=\"Remove queued message " + (i + 1) + "\">×</button></li>";
    }).join("");
  }
  return {esc: esc, md: md, stamp: stamp, userHTML: userHTML, agentHTML: agentHTML, saidHTML: saidHTML, toolHTML: toolHTML,
    proposalHTML: proposalHTML, appliedHTML: appliedHTML, historyHTML: historyHTML, factoryHTML: factoryHTML, threadHTML: threadHTML, deleteHTML: deleteHTML, pendingOf: pendingOf, answerHTML: answerHTML, paletteOf: paletteOf, paletteHTML: paletteHTML, expand: expand, when: when, rowsHTML: rowsHTML,
    sumHTML: sumHTML, unreadOf: unreadOf, headHTML: headHTML, eventStream: eventStream, sse: sse, queue: queue, queueHTML: queueHTML};
})();

(function () {
  var panel = document.getElementById("chat");
  if (!panel) return;
  var log = document.getElementById("chat-log");
  var form = document.getElementById("chat-form"), input = document.getElementById("chat-input"), queued = document.getElementById("chat-queue");
  var fresh = document.getElementById("chat-new"), rowsEl = document.getElementById("thread-rows"), head = document.getElementById("thread-head");
  var answerRow = document.getElementById("chat-answer"), pending = null;
  var commandsEl = document.getElementById("chat-commands"), palette = document.getElementById("chat-palette"), pal = {list: null, sel: 0};
  var commands = commandsEl ? JSON.parse(commandsEl.textContent || "null") || [] : [];
  var sum = panel.querySelector(".sw-sum");
  var login = panel.dataset.login, base = "/projects/" + encodeURIComponent(panel.dataset.project || "") + "/chat";
  var baseTitle = document.title;
  // threads is the switcher's list; open the shown thread and its row;
  // running counts each thread's streams in this page
  var threads = [], open = 0, openThread = {}, running = {}, readOnly = false;
  var foldKey = "chat-fold:" + login;

  function scrollEnd(force) {
    if (force || log.scrollHeight - log.scrollTop - log.clientHeight < 160) log.scrollTop = log.scrollHeight;
  }
  // add puts html at the end of into, before the streaming cursor
  function add(into, html) {
    var t = document.createElement("template");
    t.innerHTML = html;
    into.insertBefore(t.content, into.querySelector(":scope > .cursor"));
    scrollEnd();
  }
  function busy(n) { return !!running[n]; }
  // lock disables the composer while the open thread is read-only and the
  // log's buttons while a turn of the open thread streams
  function lock() {
    if (!form) return;
    form.querySelectorAll("textarea, button").forEach(function (el) { el.disabled = readOnly; });
    log.setAttribute("aria-busy", String(busy(open)));
    log.querySelectorAll("button").forEach(function (b) { b.disabled = busy(open); });
  }
  function read(reader, onEvent) {
    var decoder = new TextDecoder(), buf = "";
    return reader.read().then(function next(chunk) {
      buf += decoder.decode(chunk.value || new Uint8Array(0), {stream: !chunk.done});
      var parsed = chatView.sse(buf);
      buf = parsed.rest;
      parsed.events.forEach(onEvent);
      if (!chunk.done) return reader.read().then(next);
    });
  }
  // run POSTs body to url as a turn of thread n and renders its stream into
  // into: queued while the turn before it runs, then text pieces, tool
  // blocks, proposals and applied lines, until done or error. A 404 is
  // onMissing's when given; a stream that ends without either says the
  // connection was lost. A thread switched away from keeps its stream: the
  // turn ends and marks the thread.
  function run(n, url, body, into, onMissing) {
    var ended = false, said = null, raw = "", reader = null, cursor = document.createElement("span"), wait = null;
    cursor.className = "cursor";
    cursor.setAttribute("aria-hidden", "true");
    cursor.textContent = "▍";
    into.appendChild(cursor);
    running[n] = (running[n] || 0) + 1;
    lock();
    listThreads();
    function event(ev) {
      var d = JSON.parse(ev.data);
      if (wait && ev.event !== "queued") {
        wait.remove();
        wait = null;
      }
      if (ev.event === "compacted") {
        add(into, "<p class=\"meta chat-queued\">compacted: " + chatView.esc(d.messages) + " earlier messages summarised</p>");
        return;
      }
      if (ev.event === "queued") {
        add(into, "<p class=\"meta chat-queued\">queued: place " + chatView.esc(d.position) + ", it starts when the turn before it ends</p>");
        wait = cursor.previousElementSibling;
        return;
      }
      if (ev.event === "text") {
        if (!said) {
          add(into, "<div class=\"said\"></div>");
          said = cursor.previousElementSibling;
          raw = "";
        }
        raw += d.text;
        said.innerHTML = chatView.md(raw);
        scrollEnd();
        return;
      }
      said = null;
      if (ev.event === "tool") add(into, chatView.toolHTML(d.name, d.arguments, d.duration_ms, d.result));
      else if (ev.event === "proposal") add(into, chatView.proposalHTML(d));
      else if (ev.event === "applied") add(into, chatView.appliedHTML(d, into.classList.contains("undoing")));
      else if (ev.event === "error") add(into, "<p class=\"chat-error\">" + chatView.esc(d.message) + "</p>");
      ended = ended || ev.event === "done" || ev.event === "error";
    }
    return fetch(url, {method: "POST", body: body,
      headers: {"Content-Type": "application/x-www-form-urlencoded", "X-CSRF-Token": form.elements.csrf.value}}).then(function (resp) {
      if (resp.status === 404 && onMissing) {
        ended = true;
        onMissing();
        return;
      }
      if (!resp.ok || !chatView.eventStream(resp.headers.get("Content-Type"))) {
        ended = true;
        return resp.text().then(function (text) {
          add(into, "<p class=\"chat-error\">" + chatView.esc(text.trim() || "The console refused: " + resp.status) + "</p>");
        });
      }
      reader = resp.body.getReader();
      return read(reader, event);
    }).catch(function () {
      if (reader) reader.cancel().catch(function () {});
    }).then(function () {
      if (!ended) add(into, "<p class=\"chat-error\">the chat lost the connection, send again</p>");
      cursor.remove();
      running[n]--;
      lock();
      queueOf(n).flush();
      return (n === open ? seen(n) : Promise.resolve()).then(listThreads);
    });
  }
  // seen moves thread n's seen marker to its newest row: the member read it
  function seen(n) {
    return fetch(base + "/seen", {method: "POST", body: new URLSearchParams({conversation: n}).toString(),
      headers: {"Content-Type": "application/x-www-form-urlencoded", "X-CSRF-Token": form.elements.csrf.value}}).catch(function () {});
  }
  // each thread's own queue of messages sent while its turn streams
  var queues = {};
  function queueOf(n) {
    if (!queues[n]) {
      queues[n] = chatView.queue(function (text) { turn(n, text); }, function () {
        return busy(n) || readOnly && n === open || n === open && !!queued.querySelector("textarea");
      }, function (items) {
        if (n !== open || queued.querySelector("textarea")) return;
        queued.innerHTML = chatView.queueHTML(items);
        queued.hidden = !items.length;
      });
    }
    return queues[n];
  }
  function send() {
    var text = input.value.trim();
    if (!text || readOnly) return;
    input.value = "";
    queueOf(open).add(text);
  }
  function turn(n, text) {
    var shown = n === open, command = text === "/compact";
    if (shown) {
      pending = null;
      drawAnswer();
    }
    var into = document.createElement("div");
    if (shown) {
      if (!command) add(log, chatView.userHTML(login, chatView.expand(commands, text), new Date()));
      add(log, chatView.agentHTML(new Date(), ""));
      scrollEnd(true);
      into = log.lastElementChild;
    }
    run(n, base, new URLSearchParams({message: text, conversation: n}).toString(), into).then(function () {
      if (command && n === open) return load(n);
    }).then(function () {
      if (document.activeElement === document.body) input.focus();
    });
  }
  function expired(el) {
    return function () { el.innerHTML = "<span class=\"expired\">expired</span>"; };
  }

  // a drawer's "From your chat thread" opens that thread here
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-open-thread]");
    if (b && Number(b.dataset.openThread) !== open) load(Number(b.dataset.openThread));
  });
  // #n in the model's text opens card n's drawer over the board
  document.addEventListener("click", function (e) {
    var ref = e.target.closest("a.cardref");
    if (!ref) return;
    e.preventDefault();
    var refs = document.querySelectorAll("#board [data-card] .ref");
    for (var i = 0; i < refs.length; i++) {
      if (refs[i].textContent.trim() === "#" + ref.dataset.issue) {
        location.hash = "#card=" + refs[i].closest("[data-card]").dataset.card;
        return;
      }
    }
    var flash = document.getElementById("flash");
    flash.textContent = "#" + ref.dataset.issue + " is not on the board.";
    flash.hidden = false;
  });
  if (!form) return;
  // the queue under the input: a click on a message edits it in place
  // (Enter or leaving it keeps the edit, Escape the message as it was),
  // its × removes it; an open edit is drawn again once it closes
  queued.addEventListener("click", function (e) {
    var b = e.target.closest("button");
    if (!b) return;
    if (b.hasAttribute("data-remove")) {
      queueOf(open).remove(Number(b.dataset.remove));
      input.focus();
      return;
    }
    var area = document.createElement("textarea");
    area.rows = 2;
    area.value = b.textContent;
    area.dataset.edit = b.dataset.edit;
    area.setAttribute("aria-label", "Queued message " + (Number(b.dataset.edit) + 1));
    b.replaceWith(area);
    area.focus();
  });
  queued.addEventListener("keydown", function (e) {
    if (e.target.localName !== "textarea") return;
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      input.focus();
    } else if (e.key === "Escape") {
      e.stopPropagation();
      e.target.dataset.drop = "";
      input.focus();
    }
  });
  queued.addEventListener("focusout", function (e) {
    var area = e.target;
    if (area.localName !== "textarea") return;
    area.remove();
    if (area.dataset.drop === undefined) queueOf(open).edit(Number(area.dataset.edit), area.value);
    else queueOf(open).flush();
  });

  form.addEventListener("submit", function (e) {
    e.preventDefault();
    if (pal.list) command(pal.list[pal.sel].name);
    else send();
  });
  // the commands: "/" at the start of the box opens the palette, the
  // arrows choose, Enter runs, Escape closes; the chips run theirs
  function drawPalette() {
    pal.list = chatView.paletteOf(commands, input.value);
    if (pal.list && !pal.list.length) pal.list = null;
    palette.hidden = !pal.list;
    input.setAttribute("aria-expanded", String(!!pal.list));
    if (!pal.list) {
      input.removeAttribute("aria-activedescendant");
      return;
    }
    pal.sel = Math.min(pal.sel, pal.list.length - 1);
    palette.innerHTML = chatView.paletteHTML(pal.list, pal.sel);
    input.setAttribute("aria-activedescendant", "cmd-" + pal.list[pal.sel].name);
  }
  function command(name) {
    input.value = "";
    drawPalette();
    if (name === "new") {
      fresh.click();
      return;
    }
    input.value = "/" + name;
    send();
  }
  input.addEventListener("input", function () {
    pal.sel = 0;
    drawPalette();
  });
  form.addEventListener("click", function (e) {
    var b = e.target.closest("[data-cmd]");
    if (b) command(b.dataset.cmd);
  });
  input.addEventListener("keydown", function (e) {
    if (pal.list && (e.key === "ArrowDown" || e.key === "ArrowUp")) {
      e.preventDefault();
      pal.sel = (pal.sel + (e.key === "ArrowDown" ? 1 : pal.list.length - 1)) % pal.list.length;
      drawPalette();
    } else if (pal.list && e.key === "Escape") {
      e.stopPropagation();
      pal.list = null;
      palette.hidden = true;
      input.setAttribute("aria-expanded", "false");
    } else if (e.key === "Enter" && !e.shiftKey && !e.isComposing && pal.list) {
      e.preventDefault();
      command(pal.list[pal.sel].name);
    } else if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      send();
    } else if (e.key === "Escape") {
      e.stopPropagation();
      input.blur();
    }
  });
  document.addEventListener("keydown", function (e) {
    var keys = document.getElementById("keys");
    if (e.key !== "/" || e.ctrlKey || e.metaKey || e.altKey || (keys && !keys.hidden) ||
      e.target.closest("input, textarea, select, [contenteditable]")) return;
    e.preventDefault();
    // the wide view hides the chat: "/" brings it back
    var screen = document.getElementById("screen");
    if (screen && screen.classList.contains("wide")) {
      document.getElementById("view-toggle").click();
      return;
    }
    input.focus();
  });
  log.addEventListener("click", function (e) {
    var b = e.target.closest("button");
    if (!b || busy(open)) return;
    var card = b.closest(".proposal");
    if (b.hasAttribute("data-choice")) {
      queueOf(open).add(b.dataset.choice);
    } else if (b.hasAttribute("data-skip")) {
      card.remove();
    } else if (b.hasAttribute("data-doit")) {
      var foot = card.querySelector(".pfoot");
      foot.textContent = "";
      run(open, base + "/proposals/" + encodeURIComponent(card.dataset.proposal), "", foot, expired(foot));
    } else if (b.hasAttribute("data-undo")) {
      var out = document.createElement("div");
      out.className = "undoing";
      b.parentNode.after(out);
      b.remove();
      run(open, base + "/undo/" + encodeURIComponent(b.dataset.undo), "", out, expired(out));
    }
  });

  // load shows thread n, the current one for "": its head and its log,
  // read marks it seen; an archived thread is read-only
  var intro = log.innerHTML;
  function load(n) {
    var url = base + (n === "" ? "" : "?conversation=" + encodeURIComponent(n));
    return fetch(url, {headers: {Accept: "application/json"}}).then(function (resp) {
      if (!resp.ok) throw new Error("GET " + url + ": " + resp.status);
      return resp.json();
    }).then(function (data) {
      open = data.conversation;
      openThread = data.thread || {number: open};
      readOnly = openThread.state === "archived";
      log.innerHTML = data.messages.length ? chatView.threadHTML(data.messages, login, data.proposals, data.thread) : intro;
      if (readOnly) add(log, "<p class=\"meta chat-old\">archived, read-only: restore it to write</p>");
      if (busy(open)) add(log, chatView.agentHTML(new Date(), "<span class=\"cursor\" aria-hidden=\"true\">▍</span>"));
      pending = readOnly ? null : chatView.pendingOf(data.messages);
      drawAnswer();
      drawHead();
      scrollEnd(true);
      queueOf(open).flush();
      lock();
      return listThreads();
    }).catch(function () {
      add(log, "<p class=\"chat-error\">The conversation could not be read; reload the page.</p>");
    });
  }
  // the answer row: the pending escalation's choices, once, between the
  // log and the box; a choice answers its card with no model turn, a
  // message below goes to the agent
  function drawAnswer() {
    if (!answerRow) return;
    answerRow.hidden = !pending;
    answerRow.innerHTML = pending ? chatView.answerHTML(pending) : "";
  }
  function drawHead() {
    if (!head) return;
    head.innerHTML = chatView.headHTML(openThread, new Date());
    head.className = "thead" + (openThread.inbox ? " inbox" : "") + (openThread.state === "archived" ? " arch" : "");
  }
  // listThreads reads the switcher's list and draws it, its fold's summary
  // and the tab title's count; the open thread, unread by a turn it did not
  // stream here, is read again
  function listThreads() {
    return fetch(base + "/conversations", {headers: {Accept: "application/json"}}).then(function (resp) {
      if (!resp.ok) throw new Error("GET " + base + "/conversations: " + resp.status);
      return resp.json();
    }).then(function (list) {
      threads = list;
      var now = new Date();
      rowsEl.innerHTML = chatView.rowsHTML(list, open, now);
      sum.innerHTML = chatView.sumHTML(list, folded());
      sum.hidden = false;
      var n = chatView.unreadOf(list);
      document.title = (n ? "(" + n + ") " : "") + baseTitle;
      var mine = list.filter(function (t) { return t.number === open; })[0];
      if (mine && mine.unread && !busy(open)) load(open);
      else if (mine) {
        openThread = mine;
        drawHead();
      }
    }).catch(function () {});
  }
  // the fold: the table folds to its marks, on the phone to one summary row;
  // kept per member in this browser
  function folded() { return panel.classList.contains("sw-collapsed"); }
  function fold(on) {
    panel.classList.toggle("sw-collapsed", on);
    try { localStorage.setItem(foldKey, on ? "1" : ""); } catch (e) {}
    panel.querySelectorAll("[data-swtoggle]").forEach(function (b) {
      b.setAttribute("aria-expanded", String(!on));
      if (b.classList.contains("swtg")) {
        b.textContent = on ? "›" : "‹";
        b.setAttribute("aria-label", on ? "Unfold the threads" : "Fold the threads");
        b.title = on ? "Unfold the threads" : "Fold the threads";
      }
    });
    sum.innerHTML = chatView.sumHTML(threads, on);
  }
  try { fold(localStorage.getItem(foldKey) === "1"); } catch (e) {}
  // postThread posts form to the chat's path, the open thread's number in it
  function postThread(path, form0) {
    return fetch(base + path, {method: "POST", body: new URLSearchParams(form0).toString(),
      headers: {"Content-Type": "application/x-www-form-urlencoded", "X-CSRF-Token": form.elements.csrf.value}}).then(function (resp) {
      if (!resp.ok) return resp.text().then(function (why) { throw new Error(why); });
    });
  }
  function failed(err) {
    add(log, "<p class=\"chat-error\">" + chatView.esc(String(err.message || err).trim()) + "</p>");
  }
  // the hard delete's warning: Delete the thread, or Keep it, Escape or a
  // click outside it; focus goes back to Delete…
  function closeDialog() {
    var d = document.getElementById("thread-dialog");
    if (d) d.remove();
    var b = head.querySelector("[data-delete]");
    if (b) b.focus();
  }
  document.addEventListener("click", function (e) {
    var d = document.getElementById("thread-dialog");
    if (!d) return;
    var b = e.target.closest("[data-dlg]");
    if (b && b.dataset.dlg === "delete") {
      var n = open;
      d.remove();
      postThread("/delete", {conversation: n}).then(function () { return load(""); }).catch(failed);
    } else if (b || e.target === d) closeDialog();
  });
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape" && document.getElementById("thread-dialog")) {
      e.stopPropagation();
      closeDialog();
    }
  }, true);
  panel.addEventListener("click", function (e) {
    var r0 = e.target.closest("[data-restore]");
    if (r0) {
      postThread("/archive", {conversation: r0.dataset.restore, restore: "1"}).then(function () {
        return load(Number(r0.dataset.restore));
      }).catch(failed);
      return;
    }
    var t = e.target.closest("[data-swtoggle]");
    if (t) {
      fold(!folded());
      t.focus();
      return;
    }
    var row = e.target.closest("[data-thread]");
    if (row && Number(row.dataset.thread) !== open) {
      load(Number(row.dataset.thread)).then(function () {
        if (!readOnly) input.focus({preventScroll: true});
      });
    }
  });
  if (answerRow) answerRow.addEventListener("click", function (e) {
    var b = e.target.closest("[data-answer]");
    if (!b || !pending) return;
    var n = open, m = pending;
    pending = null;
    drawAnswer();
    postThread("/answer", {conversation: n, message: m.id, choice: b.dataset.answer}).then(function () { return load(n); }).catch(failed);
  });
  head.addEventListener("click", function (e) {
    if (e.target.closest("[data-archive]")) {
      postThread("/archive", {conversation: open}).then(function () { return load(""); }).catch(failed);
      return;
    }
    if (e.target.closest("[data-delete]")) {
      document.body.insertAdjacentHTML("beforeend", chatView.deleteHTML(openThread));
      document.querySelector("#thread-dialog [data-dlg=keep]").focus();
      return;
    }
    var b = e.target.closest("[data-rename]");
    if (!b) return;
    var field = document.createElement("input");
    field.type = "text";
    field.maxLength = 80;
    field.value = b.textContent;
    field.className = "rename-in";
    field.setAttribute("aria-label", "The thread's title");
    b.replaceWith(field);
    field.focus();
    field.select();
    var done = false;
    function finish(save) {
      if (done) return;
      done = true;
      var title = field.value.trim();
      if (!save || !title || title === openThread.title) {
        drawHead();
        return;
      }
      fetch(base + "/rename", {method: "POST", body: new URLSearchParams({conversation: open, title: title}).toString(),
        headers: {"Content-Type": "application/x-www-form-urlencoded", "X-CSRF-Token": form.elements.csrf.value}}).then(function () {
        return listThreads();
      });
    }
    field.addEventListener("keydown", function (k) {
      if (k.key === "Enter") finish(true);
      else if (k.key === "Escape") {
        k.stopPropagation();
        finish(false);
      }
    });
    field.addEventListener("blur", function () { finish(true); });
  });
  fresh.addEventListener("click", function () {
    fetch(base + "/new", {method: "POST", headers: {"X-CSRF-Token": form.elements.csrf.value}}).then(function (resp) {
      if (!resp.ok) throw new Error("POST " + base + "/new: " + resp.status);
      return load("");
    }).then(function () {
      input.focus();
    }).catch(function () {
      add(log, "<p class=\"chat-error\">A new thread could not be started; try again.</p>");
    });
  });
  // the list follows the factory: a turn ending elsewhere, a poke arriving
  setInterval(function () {
    if (!document.hidden) listThreads();
  }, 10000);

  load("");
})();
