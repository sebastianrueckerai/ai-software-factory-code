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
  function userHTML(login, text, at) {
    return "<div class=\"msg you\"><p class=\"who\">" + esc(login) + " · " + stamp(at) + "</p><p class=\"said\">" + esc(text) + "</p></div>";
  }
  function agentHTML(at, parts) {
    return "<div class=\"msg agent\"><p class=\"who\">project agent · " + stamp(at) + "</p>" + parts + "</div>";
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
    return "<details class=\"tool\"><summary>🔧 " + esc(name) + (ms == null ? "" : " · " + ms + " ms") + (first ? " · " + esc(first) : "") +
      "</summary><pre>" + esc(typeof args === "string" ? args : JSON.stringify(args)) + (result ? "\n\n" + esc(result) : "") + "</pre></details>";
  }
  function proposalHTML(p) {
    return "<div class=\"proposal\" data-proposal=\"" + esc(p.id) + "\"><p class=\"ptitle\">I will:</p><ul><li>" + inline(esc(p.description)) + "</li></ul>" +
      "<details class=\"args\"><summary>" + esc(p.tool) + " arguments</summary><pre>" + esc(JSON.stringify(p.arguments)) + "</pre></details>" +
      "<p class=\"pfoot\"><button type=\"button\" class=\"doit\" data-doit>Do it</button> <button type=\"button\" class=\"skip\" data-skip>Skip</button></p></div>";
  }
  // appliedHTML is a write that took effect, or an undo's when undone
  function appliedHTML(a, undone) {
    return "<p class=\"applied\">" + (undone ? "<span class=\"undone\">↶ undone</span>" : "<span class=\"ok\">✓ applied</span>") + " <code>" + esc(a.tool) + "</code> · " + inline(esc(a.result)) +
      (a.undo ? " <button type=\"button\" class=\"undo\" data-undo=\"" + esc(a.undo) + "\">Undo</button>" : "") + "</p>";
  }
  // historyHTML is the stored conversation, oldest first: each member's
  // line, and the agent's rows between two of them as one message
  function historyHTML(msgs, login) {
    var out = [], parts = null, at;
    function close() {
      if (parts) out.push(agentHTML(at, parts.join("")));
      parts = null;
    }
    msgs.forEach(function (m) {
      if (m.role === "user") {
        close();
        out.push(userHTML(login, m.content, m.at));
        return;
      }
      if (!parts) {
        parts = [];
        at = m.at;
      }
      if (m.content) parts.push(saidHTML(m.content));
      (m.tool_calls || []).forEach(function (c) { parts.push(toolHTML(c.name, c.arguments, null, c.result)); });
    });
    close();
    return out.join("");
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
  return {esc: esc, md: md, stamp: stamp, userHTML: userHTML, agentHTML: agentHTML, saidHTML: saidHTML, toolHTML: toolHTML,
    proposalHTML: proposalHTML, appliedHTML: appliedHTML, historyHTML: historyHTML, sse: sse};
})();

(function () {
  var panel = document.getElementById("chat");
  if (!panel) return;
  var log = document.getElementById("chat-log"), screen = document.getElementById("screen");
  var form = document.getElementById("chat-form"), input = document.getElementById("chat-input");
  var login = panel.dataset.login, base = "/projects/" + encodeURIComponent(panel.dataset.project || "") + "/chat";
  var streaming = false;

  function showPane(name) {
    screen.dataset.pane = name;
    document.querySelectorAll("[data-pane-tab]").forEach(function (b) {
      b.setAttribute("aria-selected", String(b.dataset.paneTab === name));
    });
    if (name === "chat") scrollEnd(true);
  }
  function scrollEnd(force) {
    if (force || log.scrollHeight - log.scrollTop - log.clientHeight < 160) log.scrollTop = log.scrollHeight;
  }
  // the Chat tab counts the proposals not yet answered
  function countOpen() {
    var n = log.querySelectorAll("[data-doit]").length, count = document.getElementById("chat-open");
    count.textContent = n ? String(n) : "";
    count.hidden = !n;
  }
  // add puts html at the end of into, before the streaming cursor
  function add(into, html) {
    var t = document.createElement("template");
    t.innerHTML = html;
    into.insertBefore(t.content, into.querySelector(":scope > .cursor"));
    scrollEnd();
  }
  function setStreaming(on) {
    streaming = on;
    log.setAttribute("aria-busy", String(on));
    form.querySelectorAll("textarea, button").forEach(function (el) { el.disabled = on; });
    log.querySelectorAll("button").forEach(function (b) { b.disabled = on; });
  }
  function read(resp, onEvent) {
    var reader = resp.body.getReader(), decoder = new TextDecoder(), buf = "";
    return reader.read().then(function next(chunk) {
      buf += decoder.decode(chunk.value || new Uint8Array(0), {stream: !chunk.done});
      var parsed = chatView.sse(buf);
      buf = parsed.rest;
      parsed.events.forEach(onEvent);
      if (!chunk.done) return reader.read().then(next);
    });
  }
  // run POSTs body to url and renders the turn's stream into into: text
  // pieces as they come, tool blocks, proposals, applied lines, until done
  // or error. A 404 is onMissing's when given; a stream that ends without
  // either says the connection was lost, and nothing is sent again.
  function run(url, body, into, onMissing) {
    var ended = false, said = null, raw = "", cursor = document.createElement("span");
    cursor.className = "cursor";
    cursor.setAttribute("aria-hidden", "true");
    cursor.textContent = "▍";
    into.appendChild(cursor);
    setStreaming(true);
    function event(ev) {
      var d = JSON.parse(ev.data);
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
      countOpen();
    }
    return fetch(url, {method: "POST", body: body,
      headers: {"Content-Type": "application/x-www-form-urlencoded", "X-CSRF-Token": form.elements.csrf.value}}).then(function (resp) {
      if (resp.status === 404 && onMissing) {
        ended = true;
        onMissing();
        return;
      }
      if (!resp.ok || resp.headers.get("Content-Type") !== "text/event-stream") {
        ended = true;
        return resp.text().then(function (text) {
          add(into, "<p class=\"chat-error\">" + chatView.esc(text.trim() || "The console refused: " + resp.status) + "</p>");
        });
      }
      return read(resp, event);
    }).catch(function () {}).then(function () {
      if (!ended) add(into, "<p class=\"chat-error\">the chat lost the connection, send again</p>");
      cursor.remove();
      setStreaming(false);
      countOpen();
    });
  }
  function send() {
    var text = input.value.trim();
    if (!text || streaming) return;
    input.value = "";
    var start = log.querySelector(".chat-start");
    if (start) start.remove();
    add(log, chatView.userHTML(login, text, new Date()));
    add(log, chatView.agentHTML(new Date(), ""));
    scrollEnd(true);
    run(base, new URLSearchParams({message: text}).toString(), log.lastElementChild).then(function () {
      if (document.activeElement === document.body) input.focus();
    });
  }
  function expired(el) {
    return function () { el.innerHTML = "<span class=\"expired\">expired</span>"; };
  }

  document.addEventListener("click", function (e) {
    var tab = e.target.closest("[data-pane-tab]");
    if (tab) showPane(tab.dataset.paneTab);
  });
  // #n in the model's text opens card n's drawer over the board
  document.addEventListener("click", function (e) {
    var ref = e.target.closest("a.cardref");
    if (!ref) return;
    e.preventDefault();
    var refs = document.querySelectorAll("#board [data-card] .ref");
    for (var i = 0; i < refs.length; i++) {
      if (refs[i].textContent.trim() === "#" + ref.dataset.issue) {
        showPane("board");
        location.hash = "#card=" + refs[i].closest("[data-card]").dataset.card;
        return;
      }
    }
    var flash = document.getElementById("flash");
    flash.textContent = "#" + ref.dataset.issue + " is not on the board.";
    flash.hidden = false;
  });
  if (!form) return;

  form.addEventListener("submit", function (e) {
    e.preventDefault();
    send();
  });
  input.addEventListener("keydown", function (e) {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
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
    showPane("chat");
    input.focus();
  });
  log.addEventListener("click", function (e) {
    var b = e.target.closest("button");
    if (!b || streaming) return;
    var card = b.closest(".proposal");
    if (b.hasAttribute("data-skip")) {
      card.remove();
      countOpen();
    } else if (b.hasAttribute("data-doit")) {
      var foot = card.querySelector(".pfoot");
      foot.textContent = "";
      run(base + "/proposals/" + encodeURIComponent(card.dataset.proposal), "", foot, expired(foot));
    } else if (b.hasAttribute("data-undo")) {
      var out = document.createElement("div");
      out.className = "undoing";
      b.parentNode.after(out);
      b.remove();
      run(base + "/undo/" + encodeURIComponent(b.dataset.undo), "", out, expired(out));
    }
  });

  fetch(base, {headers: {Accept: "application/json"}}).then(function (resp) {
    if (!resp.ok) throw new Error("GET " + base + ": " + resp.status);
    return resp.json();
  }).then(function (msgs) {
    if (msgs.length) log.innerHTML = chatView.historyHTML(msgs, login);
    scrollEnd(true);
  }).catch(function () {
    add(log, "<p class=\"chat-error\">The conversation could not be read; reload the page.</p>");
  });
})();
